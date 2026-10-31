package rating_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/internal/usage"
)

var (
	jan    = rating.Period{Start: day(1), End: day(32)}
	pgkr   = levyan.Channel_CHANNEL_PG_KR
	stock  = levyan.MeterKind_METER_KIND_STOCK
	flow   = levyan.MeterKind_METER_KIND_FLOW
	twa    = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	most   = levyan.Aggregation_AGGREGATION_MAX
	last   = levyan.Aggregation_AGGREGATION_LAST
	sum    = levyan.Aggregation_AGGREGATION_SUM
	meters = pdid.New(10)
)

// day is a day of January 2027, at midnight UTC; the 32nd is February's first.
func day(d int) time.Time { return time.Date(2027, 1, d, 0, 0, 0, 0, time.UTC) }

func meter(kind levyan.MeterKind, agg levyan.Aggregation) rating.Meter {
	return rating.Meter{Id: meters, Key: "loft.things", Unit: "{thing}", Kind: kind, Aggregation: agg}
}

var line = pricing.Line{Meter: meters, Channel: pgkr, Currency: "KRW"}

func version(rate pricing.Rate, effective, existing time.Time) pricing.Version {
	return pricing.Version{
		Id: pdid.New(11), Meter: meters, Channel: pgkr, Currency: "KRW",
		Rate: rate, Effective: effective, Existing: existing, Published: effective.Add(-time.Hour),
	}
}

func perUnit(included, price, per int64) pricing.Rate {
	return pricing.PerUnit{Included: included, UnitPrice: price, Per: per}
}

// daily is a reading at each midnight of [from, to), of v.
func daily(from, to int, v int64) []usage.Reading {
	out := []usage.Reading{}
	for d := from; d < to; d++ {
		out = append(out, usage.Reading{At: day(d), Value: v})
	}
	return out
}

func stockUsage(rs ...[]usage.Reading) rating.Usage {
	all := []usage.Reading{}
	for _, r := range rs {
		all = append(all, r...)
	}
	return rating.Usage{Held: usage.Profile(all, jan.Start, jan.End, time.Time{}), Readings: all}
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func rate(t *testing.T, m rating.Meter, u rating.Usage, book pricing.Book, since time.Time) rating.Line {
	t.Helper()
	l, err := rating.Rate(m, u, jan, book, line, since)
	require.NoError(t, err)
	return l
}

func total(t *testing.T, ls ...rating.Line) int64 {
	t.Helper()
	as := []*big.Rat{}
	for _, l := range ls {
		as = append(as, l.Amount)
	}
	_, n, err := rating.Total(as...)
	require.NoError(t, err)
	return n
}

// 100 included, and 50 won for each one past it, all January.
var plain = pricing.Book{version(perUnit(100, 50, 1), day(1), day(1))}

// What is held is averaged over the whole period, each value for as long as
// it was held: ten days of 100 and twenty-one of 200.
func TestTheAverageIsOverTheWholePeriod(t *testing.T) {
	l := rate(t, meter(stock, twa), stockUsage(daily(1, 11, 100), daily(11, 32, 200)), plain, day(1))

	require.Equal(t, rat("5200/31"), l.Quantity)
	require.Equal(t, rat("100"), l.Included)
	require.Equal(t, rat("2100/31"), l.Billable)
	require.Equal(t, rat("105000/31"), l.Amount, "(5200/31 - 100) * 50")
	require.Equal(t, int64(3387), total(t, l), "rounded once, down")
}

// A reading speaks for no longer than its hold: a reader that stopped for a
// week is not billed that week at the value it stopped at.
func TestAReadingHoldsUntilTheNextAndNoLonger(t *testing.T) {
	l := rate(t, meter(stock, twa), stockUsage(daily(1, 11, 310), daily(18, 32, 310)), plain, day(1))

	// Nine days from the first reading to the tenth, the tenth's thirty-six
	// hours, and fourteen days from the 18th: 24.5 days of 310, over 31.
	require.Equal(t, rat("245"), l.Quantity)
}

// The most and the last are of the period's own readings: one before it that
// holds into it is not one of them.
func TestTheMostAndTheLastAreOfThePeriodsReadings(t *testing.T) {
	before := []usage.Reading{{At: day(1).Add(-time.Hour), Value: 1000}}
	rs := append(daily(1, 32, 100), usage.Reading{At: day(5).Add(time.Hour), Value: 300})

	l := rate(t, meter(stock, most), stockUsage(before, rs), plain, day(1))
	require.Equal(t, rat("300"), l.Quantity)
	require.Equal(t, rat("10000"), l.Amount)

	rs = append(daily(1, 31, 100), usage.Reading{At: day(31), Value: 150})
	l = rate(t, meter(stock, last), stockUsage(before, rs), plain, day(1))
	require.Equal(t, rat("150"), l.Quantity)
	require.Equal(t, rat("2500"), l.Amount)
}

// A flow is what it counted, priced per a number of units: 2 won for each
// hundred past a thousand.
func TestAFlowIsWhatItCounted(t *testing.T) {
	book := pricing.Book{version(perUnit(1000, 2, 100), day(1), day(1))}
	l := rate(t, meter(flow, sum), rating.Usage{Counted: 1234}, book, day(1))

	require.Equal(t, rat("1234"), l.Quantity)
	require.Equal(t, rat("234"), l.Billable)
	require.Equal(t, rat("117/25"), l.Amount)
	require.Equal(t, int64(4), total(t, l))
}

// What is included is free up to it and the next unit is not; and a period
// that used less does not carry what it did not use.
func TestWhatIsIncludedIsFreeUpToItsBound(t *testing.T) {
	for _, tc := range []struct {
		v    int64
		want string
	}{{0, "0"}, {99, "0"}, {100, "0"}, {101, "50"}, {150, "2500"}} {
		l := rate(t, meter(stock, twa), stockUsage(daily(1, 32, tc.v)), plain, day(1))
		require.Equal(t, rat(tc.want), l.Amount, "%d held all month", tc.v)

		l = rate(t, meter(stock, most), stockUsage(daily(1, 32, tc.v)), plain, day(1))
		require.Equal(t, rat(tc.want), l.Amount, "%d at most", tc.v)
	}
}

// A workspace that began during the period is averaged over the whole of it,
// and is included the whole of what the version includes: neither is
// prorated, so its average is a third of what it held, a third of the time.
func TestAWorkspaceThatBeganLateIsIncludedAllOfIt(t *testing.T) {
	since := day(21)
	l := rate(t, meter(stock, twa), stockUsage(daily(21, 32, 310)), plain, since)

	require.Equal(t, rat("110"), l.Quantity, "310 for 11 of 31 days")
	require.Equal(t, rat("100"), l.Included, "all of it")
	require.Equal(t, rat("500"), l.Amount, "(110 - 100) * 50, and not that times 11/31")
	require.Len(t, l.Parts, 1)
	require.Equal(t, rat("1"), l.Parts[0].Share, "the whole of its time as a customer")

	// And what it held at most, it is billed at most for, from whenever.
	l = rate(t, meter(stock, most), stockUsage(daily(21, 32, 310)), plain, since)
	require.Equal(t, rat("10500"), l.Amount)
}

// A price that changes in the middle of a period splits it, and each part is
// charged by its share of the time.
func TestAPriceChangedDuringThePeriodChargesEachPartForItsTime(t *testing.T) {
	a := version(perUnit(100, 50, 1), day(1), day(1))
	b := version(perUnit(100, 80, 1), day(16), day(16))
	book := pricing.Book{a, b}
	u := stockUsage(daily(1, 32, 200))

	l := rate(t, meter(stock, twa), u, book, day(1).AddDate(0, -1, 0))
	require.Len(t, l.Parts, 2)
	require.Equal(t, a.Id, l.Parts[0].Version)
	require.Equal(t, rat("15/31"), l.Parts[0].Share)
	require.Equal(t, b.Id, l.Parts[1].Version)
	require.Equal(t, rat("16/31"), l.Parts[1].Share)
	require.Equal(t, rat("203000/31"), l.Amount, "15/31 of 100 * 50, and 16/31 of 100 * 80")
	require.Equal(t, int64(6548), total(t, l))

	// A rise that gives the customers already there notice reaches them only
	// when the notice is out: January is the old price whole.
	b = version(perUnit(100, 80, 1), day(16), day(32).AddDate(0, 0, 14))
	book = pricing.Book{a, b}
	l = rate(t, meter(stock, twa), u, book, day(1).AddDate(0, -1, 0))
	require.Len(t, l.Parts, 1)
	require.Equal(t, rat("5000"), l.Amount)

	// And a customer who starts after it is effective is on it from their
	// start, whatever the notice.
	l = rate(t, meter(stock, twa), stockUsage(daily(20, 32, 200)), book, day(20))
	require.Len(t, l.Parts, 1)
	require.Equal(t, b.Id, l.Parts[0].Version)

	// The same terms published again change nothing: the bill is the usage
	// at the price, however many versions say it.
	again := version(perUnit(100, 50, 1), day(16), day(16))
	l = rate(t, meter(stock, twa), u, pricing.Book{a, again}, day(1).AddDate(0, -1, 0))
	require.Equal(t, rat("5000"), l.Amount)
}

// A window kept in bands is charged the time it spent in each, at that band's
// fee: ten days of a year, for nothing, and twenty-one days of forever.
func TestBandsAreChargedForTheTimeSpentInThem(t *testing.T) {
	bands := pricing.Banded{
		Bands: []pricing.Band{
			{UpTo: 365, Bounded: true, Fee: 0},
			{UpTo: 1095, Bounded: true, Fee: 10000},
			{Fee: 30000},
		},
		UnboundedZero: true,
	}
	book := pricing.Book{version(bands, day(1), day(1))}
	m := meter(stock, last)
	m.Key, m.Unit = "loft.history.keep", "d"

	l := rate(t, m, stockUsage(daily(1, 11, 365), daily(11, 32, 0)), book, day(1))
	require.Equal(t, rat("630000/31"), l.Amount)
	require.Equal(t, int64(20322), total(t, l))
	require.Len(t, l.Parts, 1)
	require.Len(t, l.Parts[0].Bands, 2)
	require.Equal(t, rat("10/31"), l.Parts[0].Bands[0].Share)
	require.Equal(t, int64(0), l.Parts[0].Bands[0].Fee)
	require.Equal(t, rat("21/31"), l.Parts[0].Bands[1].Share)
	require.False(t, l.Parts[0].Bands[1].Bounded, "0 is forever, which is the last band")

	// A third of the period in the middle band.
	l = rate(t, m, stockUsage(daily(1, 32, 730)), book, day(1))
	require.Equal(t, rat("10000"), l.Amount, "all of it in the middle band")

	// And a price of the bands changed in the middle: each part at its own.
	doubled := pricing.Banded{Bands: []pricing.Band{
		{UpTo: 365, Bounded: true, Fee: 0},
		{UpTo: 1095, Bounded: true, Fee: 20000},
		{Fee: 60000},
	}, UnboundedZero: true}
	book = pricing.Book{version(bands, day(1), day(1)), version(doubled, day(21), day(21))}
	l = rate(t, m, stockUsage(daily(1, 32, 0)), book, day(1))
	require.Len(t, l.Parts, 2)
	require.Equal(t, rat("1260000/31"), l.Amount, "20 days of 30000 and 11 of 60000, over 31")

	// A workspace that kept its history only for the last ten days of the
	// period is charged ten days of it.
	l = rate(t, m, stockUsage(daily(22, 32, 0)), pricing.Book{version(bands, day(1), day(1))}, day(22))
	require.Equal(t, rat("300000/31"), l.Amount)
}

// Nothing in force is nothing charged: a price that begins during the period
// does not reach back before it.
func TestAPriceDoesNotReachBack(t *testing.T) {
	book := pricing.Book{version(perUnit(0, 10, 1), day(11), day(11))}
	l := rate(t, meter(stock, twa), stockUsage(daily(1, 32, 31)), book, day(1).AddDate(0, -1, 0))

	require.Len(t, l.Parts, 1)
	require.Equal(t, rat("21/31"), l.Parts[0].Share)
	require.Equal(t, rat("6510/31"), l.Amount, "31 held, at 10, for 21 of the 31 days")
}

// A statement's total is its lines added exactly and rounded once, down --
// what is owed back as well as what is owed.
func TestTheTotalIsRoundedOnceAndDown(t *testing.T) {
	exact, n, err := rating.Total(rat("1/2"), rat("1/2"), rat("2/3"))
	require.NoError(t, err)
	require.Equal(t, rat("5/3"), exact)
	require.Equal(t, int64(1), n, "and not 0, which rounding each would be")

	_, n, err = rating.Total(rat("-1/2"))
	require.NoError(t, err)
	require.Equal(t, int64(-1), n, "less owed is down too")

	_, n, err = rating.Total()
	require.NoError(t, err)
	require.Zero(t, n)
}

// A stretch nothing was in force on the line a customer is priced by, while
// a version of the meter's price was on another line, is said: it came to
// nothing for want of a price, and not because the meter is free (#9). A
// meter priced on no line at the time is not sold then, and nothing is said
// of it.
func TestAStretchPricedElsewhereIsSaid(t *testing.T) {
	x := require.New(t)
	m := meter(stock, twa)
	u := stockUsage(daily(1, 32, 10))

	// Priced on the merchant of record from the 1st, and on the gateway --
	// the customer's line -- only from the 11th.
	mor := version(perUnit(0, 5, 1), day(1), day(1))
	mor.Channel = levyan.Channel_CHANNEL_MOR
	gateway := version(perUnit(0, 5, 1), day(11), day(11))
	l := rate(t, m, u, pricing.Book{mor, gateway}, day(1))
	x.Equal([]rating.Stretch{{From: day(1), To: day(11)}}, l.Unpriced)
	x.Len(l.Parts, 1, "the gateway's part, from the 11th")

	// In another currency on the customer's channel, the same.
	usd := version(perUnit(0, 5, 1), day(1), day(1))
	usd.Currency = "USD"
	l = rate(t, m, u, pricing.Book{usd}, day(1))
	x.Equal([]rating.Stretch{{From: day(1), To: jan.End}}, l.Unpriced)
	x.Empty(l.Parts)
	x.Zero(l.Amount.Sign())

	// Priced nowhere until the 21st, and then only elsewhere: the stretch
	// before is not sold anywhere, and only the rest is said.
	late := version(perUnit(0, 5, 1), day(21), day(21))
	late.Channel = levyan.Channel_CHANNEL_MOR
	l = rate(t, m, u, pricing.Book{late}, day(1))
	x.Equal([]rating.Stretch{{From: day(21), To: jan.End}}, l.Unpriced)

	// Priced on its own line, and nowhere else: nothing to say.
	own := version(perUnit(0, 5, 1), day(1), day(1))
	l = rate(t, m, u, pricing.Book{own}, day(1))
	x.Empty(l.Unpriced)

	// And a meter priced on no line at all is not sold: nothing is said.
	l = rate(t, m, u, pricing.Book{}, day(1))
	x.Empty(l.Unpriced)

	// Only while it was a customer's: one that began on the 16th is said
	// from then.
	l = rate(t, m, u, pricing.Book{usd}, day(16))
	x.Equal([]rating.Stretch{{From: day(16), To: jan.End}}, l.Unpriced)
}
