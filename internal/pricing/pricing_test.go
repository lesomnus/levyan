package pricing_test

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/server/pd"
)

var (
	assets = pdid.New(pd.MeterDomain)
	kept   = pdid.New(pd.MeterDomain)
	stored = pdid.New(pd.MeterDomain)
)

func version(meter pdid.Id, currency string, r pricing.Rate) pricing.Version {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return pricing.Version{
		Id: pdid.New(pd.PriceDomain), Meter: meter, Channel: levyan.Channel_CHANNEL_PG_KR, Currency: currency,
		Rate: r, Effective: at, Existing: at, Published: at,
	}
}

func of(n, d int64) *big.Rat { return big.NewRat(n, d) }

func charge(t *testing.T, v pricing.Version, u pricing.Usage) int64 {
	t.Helper()
	a, err := v.Charge(u)
	require.NoError(t, err)
	require.Equal(t, v.Currency, a.Currency, "an amount is in the currency of the version it came to")
	return a.Minor
}

// What a period includes is free up to and including the last unit of it, and
// the unit after it is the first one paid for -- and a fraction of a unit past
// it, from a time-weighted average, is that fraction of a unit's price.
func TestTheFreeUnitsEndAtTheirBoundary(t *testing.T) {
	x := require.New(t)
	v := version(assets, "KRW", pricing.PerUnit{Included: 100, UnitPrice: 50, Per: 1})

	x.Equal(int64(0), charge(t, v, pricing.Whole(0)), "nothing used")
	x.Equal(int64(0), charge(t, v, pricing.Whole(99)), "less than is included")
	x.Equal(int64(0), charge(t, v, pricing.Whole(100)), "exactly what is included")
	x.Equal(int64(50), charge(t, v, pricing.Whole(101)), "the first unit past it")
	x.Equal(int64(5000), charge(t, v, pricing.Whole(200)))

	// 100 assets for half a period and 101 for the other half average 100.5.
	half := pricing.Usage{{Value: 100, Of: of(1, 2)}, {Value: 101, Of: of(1, 2)}}
	x.Equal(0, half.Quantity().Cmp(of(201, 2)))
	x.Equal(int64(25), charge(t, v, half), "half a unit past it is half a unit's price")

	// And what is not used of the free units does not carry over to the rest:
	// 90 for half the period and 110 for the other is 100 on average, and free.
	x.Equal(int64(0), charge(t, v, pricing.Usage{{Value: 90, Of: of(1, 2)}, {Value: 110, Of: of(1, 2)}}))

	// Nothing included is the first unit paid for.
	none := version(assets, "KRW", pricing.PerUnit{UnitPrice: 50, Per: 1})
	x.Equal(int64(50), charge(t, none, pricing.Whole(1)))
	x.Equal(int64(0), charge(t, none, pricing.Whole(0)))
}

// An amount is rounded once, at the end, and down: whole minor units of what
// the exact usage comes to.
func TestAnAmountIsRoundedDownOnceAtTheEnd(t *testing.T) {
	x := require.New(t)
	v := version(assets, "KRW", pricing.PerUnit{Included: 100, UnitPrice: 50, Per: 1})

	// 100 and a third past the free ones is 16.66… won, which is 16.
	x.Equal(int64(16), charge(t, v, pricing.Usage{{Value: 100, Of: of(2, 3)}, {Value: 101, Of: of(1, 3)}}))
	// And .99… is not rounded up into a whole won.
	x.Equal(int64(49), charge(t, v, pricing.Usage{{Value: 100, Of: of(1, 50)}, {Value: 101, Of: of(49, 50)}}))

	// Rounded per share, three thirds of one unit would each have been
	// nothing; rounded at the end they are the one unit they are.
	once := version(assets, "USD", pricing.PerUnit{UnitPrice: 1, Per: 1})
	thirds := pricing.Usage{{Value: 1, Of: of(1, 3)}, {Value: 1, Of: of(1, 3)}, {Value: 1, Of: of(1, 3)}}
	x.Equal(int64(1), charge(t, once, thirds))

	// A price for a block of units: two cents a GiB is nothing for one byte,
	// a cent for all but a byte of a GiB, and three cents for one and a half.
	gib := int64(1 << 30)
	storage := version(stored, "USD", pricing.PerUnit{UnitPrice: 2, Per: gib})
	x.Equal(int64(0), charge(t, storage, pricing.Whole(1)))
	x.Equal(int64(1), charge(t, storage, pricing.Whole(gib-1)))
	x.Equal(int64(2), charge(t, storage, pricing.Whole(gib)))
	x.Equal(int64(3), charge(t, storage, pricing.Whole(gib+gib/2)))

	// A band held for a third of a period is a third of its fee, down.
	bands := version(kept, "KRW", pricing.Banded{Bands: []pricing.Band{{Fee: 1000}}})
	x.Equal(int64(333), charge(t, bands, pricing.Usage{{Value: 1, Of: of(1, 3)}}))

	// And an amount past what an int64 holds is refused rather than wrapped.
	big := version(stored, "KRW", pricing.PerUnit{UnitPrice: math.MaxInt64, Per: 1})
	_, err := big.Charge(pricing.Whole(2))
	x.ErrorIs(err, pricing.ErrTooMuch)
}

// An amount is in the currency of the version it came to, in that currency's
// minor unit; a version in a currency levyan does not price in is no version,
// and two amounts of two currencies are never one.
func TestAnAmountIsInItsVersionsCurrency(t *testing.T) {
	x := require.New(t)

	won := version(assets, "KRW", pricing.PerUnit{UnitPrice: 1250, Per: 1})
	dollars := version(assets, "USD", pricing.PerUnit{UnitPrice: 1250, Per: 1})

	a, err := won.Charge(pricing.Whole(2))
	x.NoError(err)
	x.Equal("2,500 KRW", a.String(), "won have no minor unit")
	b, err := dollars.Charge(pricing.Whole(2))
	x.NoError(err)
	x.Equal("25.00 USD", b.String(), "and the same number of cents is twenty-five dollars")

	_, err = a.Add(b)
	x.ErrorIs(err, money.ErrCurrency)

	typo := version(assets, "KWR", pricing.PerUnit{UnitPrice: 1250, Per: 1})
	_, err = typo.Charge(pricing.Whole(2))
	x.Error(err, "KWR is three letters, and no currency")
	x.Error(typo.Check())
}

// The history kept: a band a year, three years, forever. 0 is forever on a
// meter that says so, and a period spent in two bands costs each for its share.
func TestARetentionWindowIsPricedByItsBand(t *testing.T) {
	x := require.New(t)

	b := pricing.Banded{
		UnboundedZero: true,
		Bands: []pricing.Band{
			{UpTo: 365, Bounded: true, Fee: 0},
			{UpTo: 1095, Bounded: true, Fee: 10000},
			{Fee: 30000},
		},
	}
	x.Equal(int64(0), b.Band(365).Fee, "a year is the first band, and its last day too")
	x.Equal(int64(10000), b.Band(366).Fee)
	x.Equal(int64(10000), b.Band(1095).Fee)
	x.Equal(int64(30000), b.Band(1096).Fee)
	x.Equal(int64(30000), b.Band(0).Fee, "forever")
	x.Equal(int64(0), pricing.Banded{Bands: b.Bands}.Band(0).Fee, "and 0 days on a meter whose 0 is only 0")

	v := version(kept, "KRW", b)
	x.Equal(int64(30000), charge(t, v, pricing.Whole(0)))
	x.Equal(int64(10000), charge(t, v, pricing.Whole(1000)))

	// Ten days of a thirty-day period at a year and twenty forever.
	x.Equal(int64(20000), charge(t, v, pricing.Usage{{Value: 365, Of: of(10, 30)}, {Value: 0, Of: of(20, 30)}}))
}

func TestWhatIsNotAPriceIsRefused(t *testing.T) {
	x := require.New(t)

	for name, r := range map[string]pricing.Rate{
		"a negative unit price":    pricing.PerUnit{UnitPrice: -1, Per: 1},
		"negative free units":      pricing.PerUnit{Included: -1, UnitPrice: 1, Per: 1},
		"a price for no units":     pricing.PerUnit{UnitPrice: 1},
		"no bands":                 pricing.Banded{},
		"a last band with a bound": pricing.Banded{Bands: []pricing.Band{{UpTo: 10, Bounded: true}}},
		"a band before the last without one": pricing.Banded{Bands: []pricing.Band{
			{Fee: 1}, {Fee: 2},
		}},
		"bands that do not go up": pricing.Banded{Bands: []pricing.Band{
			{UpTo: 10, Bounded: true}, {UpTo: 10, Bounded: true}, {},
		}},
		"a negative fee": pricing.Banded{Bands: []pricing.Band{{Fee: -1}}},
	} {
		x.Error(version(assets, "KRW", r).Check(), name)
	}

	v := version(assets, "KRW", pricing.PerUnit{UnitPrice: 1, Per: 1})
	v.Channel = levyan.Channel_CHANNEL_UNSPECIFIED
	x.Error(v.Check(), "no channel")

	v = version(assets, "KRW", pricing.PerUnit{UnitPrice: 1, Per: 1})
	v.Existing = v.Effective.Add(-time.Hour)
	x.Error(v.Check(), "for the customers already there before it is for anybody")

	for name, u := range map[string]pricing.Usage{
		"a negative value":       {{Value: -1, Of: of(1, 1)}},
		"no share":               {{Value: 1, Of: of(0, 1)}},
		"more than the period":   {{Value: 1, Of: of(2, 3)}, {Value: 2, Of: of(2, 3)}},
		"a share of nothing set": {{Value: 1}},
	} {
		_, err := v.Charge(u)
		x.Error(err, name)
	}
}

func day(m time.Month, d int) time.Time { return time.Date(2027, m, d, 0, 0, 0, 0, time.UTC) }

// A price changes by a version published beside the ones before, which stay:
// so which version a customer was priced by over any stretch of time can be
// asked, and is answered from what was published.
func TestWhichVersionIsInForceWhen(t *testing.T) {
	x := require.New(t)

	line := pricing.Line{Meter: assets, Channel: levyan.Channel_CHANNEL_PG_KR, Currency: "KRW"}
	at := func(effective, existing time.Time, price int64) pricing.Version {
		v := version(assets, "KRW", pricing.PerUnit{UnitPrice: price, Per: 1})
		v.Effective, v.Existing, v.Published = effective, existing, effective.Add(-30*24*time.Hour)
		return v
	}
	v1 := at(day(1, 1), day(1, 1), 100)
	// A rise: for new customers from March, and for the ones already there
	// from June, which is the notice they are given.
	v2 := at(day(3, 1), day(6, 1), 150)
	// And a cut, for everybody from May.
	v3 := at(day(5, 1), day(5, 1), 80)

	price := func(s pricing.Segment) int64 { return s.Version.Rate.(pricing.PerUnit).UnitPrice }
	segments := func(b pricing.Book, since, from, to time.Time) [][3]any {
		var out [][3]any
		for _, s := range b.Timeline(line, since, from, to) {
			p := int64(-1) // nothing in force
			if s.Priced {
				p = price(s)
			}
			out = append(out, [3]any{s.From, s.To, p})
		}
		return out
	}

	// With the rise published and the cut not yet: a customer from January
	// keeps the old price until the notice runs out, and one who starts in
	// April pays the new one from the start.
	b := pricing.Book{v1, v2}
	x.Equal([][3]any{{day(1, 15), day(6, 1), int64(100)}, {day(6, 1), day(8, 1), int64(150)}},
		segments(b, day(1, 15), day(1, 15), day(8, 1)))
	x.Equal([][3]any{{day(4, 1), day(8, 1), int64(150)}},
		segments(b, day(4, 1), day(4, 1), day(8, 1)))

	// Publishing the cut leaves both as they were -- what January to May was
	// priced at is answered as before -- and supersedes the rise for everybody
	// from May, the customers the rise had not reached yet included.
	b = pricing.Book{v1, v2, v3}
	x.Equal([][3]any{{day(1, 15), day(5, 1), int64(100)}, {day(5, 1), day(8, 1), int64(80)}},
		segments(b, day(1, 15), day(1, 15), day(8, 1)))
	x.Equal([][3]any{{day(3, 15), day(5, 1), int64(150)}, {day(5, 1), day(8, 1), int64(80)}},
		segments(b, day(3, 15), day(3, 15), day(8, 1)))
	v, ok := b.InForce(line, day(1, 15), day(6, 15))
	x.True(ok)
	x.Equal(v3.Id, v.Id, "the rise never reaches a customer the cut reached first")
	v, ok = b.InForce(line, day(1, 15), day(2, 1))
	x.True(ok)
	x.Equal(v1.Id, v.Id, "and February is what it was")

	// Before the first version, and before the customer, nothing is in force.
	x.Equal([][3]any{{day(1, 1).AddDate(0, -1, 0), day(1, 1), int64(-1)}, {day(1, 1), day(2, 1), int64(100)}},
		segments(b, day(1, 1).AddDate(0, -1, 0), day(1, 1).AddDate(0, -1, 0), day(2, 1)))
	x.Equal([][3]any{{day(1, 10), day(1, 20), int64(-1)}, {day(1, 20), day(2, 1), int64(100)}},
		segments(b, day(1, 20), day(1, 10), day(2, 1)))
	_, ok = b.InForce(line, day(1, 20), day(1, 19))
	x.False(ok)

	// A version published by mistake is answered by another of its dates,
	// published after it.
	typo := at(day(9, 1), day(9, 1), 999)
	typo.Published = day(8, 1)
	fixed := at(day(9, 1), day(9, 1), 99)
	fixed.Published = day(8, 1).Add(time.Hour)
	b = pricing.Book{v1, v2, v3, fixed, typo}
	v, ok = b.InForce(line, day(1, 15), day(9, 2))
	x.True(ok)
	x.Equal(fixed.Id, v.Id)

	// The order the versions come in decides nothing.
	want := b.Timeline(line, day(1, 15), day(1, 15), day(12, 1))
	for range 20 {
		rand.Shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
		x.Equal(want, b.Timeline(line, day(1, 15), day(1, 15), day(12, 1)))
	}

	// And a line is its own: a version in dollars, or on another channel,
	// prices nobody on this one.
	dollars := at(day(2, 1), day(2, 1), 1)
	dollars.Currency = "USD"
	mor := at(day(2, 1), day(2, 1), 2)
	mor.Channel = levyan.Channel_CHANNEL_MOR
	b = pricing.Book{v1, dollars, mor}
	x.Equal([][3]any{{day(1, 15), day(8, 1), int64(100)}}, segments(b, day(1, 15), day(1, 15), day(8, 1)))

	x.Empty(b.Timeline(line, day(1, 1), day(2, 1), day(2, 1)), "an empty stretch has nothing in it")
}

// What is stored reads back as the version it was published as.
func TestAStoredPriceIsTheVersionItWasPublishedAs(t *testing.T) {
	x := require.New(t)

	id := pdid.New(pd.PriceDomain)
	p := levyan.Price_builder{
		Id:       id.Bytes(),
		Meter:    levyan.Meter_builder{Id: kept.Bytes()}.Build(),
		Channel:  levyan.Channel_CHANNEL_MOR,
		Currency: "USD",
		Bands: levyan.PriceBands_builder{Bands: []*levyan.PriceBand{
			levyan.PriceBand_builder{UpTo: z(int64(365)), Fee: 0}.Build(),
			levyan.PriceBand_builder{Fee: 900}.Build(),
		}}.Build(),
	}.Build()
	v, err := pricing.VersionOf(p, true)
	x.NoError(err)
	x.Equal(id, v.Id)
	x.Equal(kept, v.Meter)
	x.Equal(pricing.Banded{UnboundedZero: true, Bands: []pricing.Band{{UpTo: 365, Bounded: true}, {Fee: 900}}}, v.Rate)

	p = levyan.Price_builder{
		Id: id.Bytes(), Meter: levyan.Meter_builder{Id: assets.Bytes()}.Build(),
		Channel: levyan.Channel_CHANNEL_PG_KR, Currency: "KRW", Included: 10, UnitPrice: 500,
	}.Build()
	v, err = pricing.VersionOf(p, false)
	x.NoError(err)
	x.Equal(pricing.PerUnit{Included: 10, UnitPrice: 500, Per: 1}, v.Rate, "a per of 0 is one unit")
}

func z[T any](v T) *T { return &v }
