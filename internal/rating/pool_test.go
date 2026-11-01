package rating_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/internal/usage"
)

// What a workspace's pools take of what it is billable for (#43): after what
// is included and what allowances give, the soonest to expire first, as far
// as each goes, the whole of what is billable and never a share of the
// period; and what is priced is what is left.

func pool(left string, from, expires time.Time) rating.Pool {
	return rating.Pool{Id: pdid.New(47), Left: rat(left), From: from, Expires: expires}
}

var always = []time.Time{day(1).AddDate(-1, 0, 0), day(1).AddDate(1, 0, 0)}

// A pool takes what is billable past what is included, before it is priced:
// 800 held, 100 included, 700 billable, of which a pool of 10,000 takes all.
func TestAPoolTakesWhatIsBillable(t *testing.T) {
	v := pool("10000", always[0], always[1])
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})

	require.Equal(t, rat("700"), l.Billable, "what is billable is past what is included, as it was")
	require.Equal(t, rat("700"), l.Pooled)
	require.Zero(t, l.Amount.Sign())
	require.Equal(t, []rating.Take{{Pool: v.Id, Quantity: rat("700")}}, l.Takes)
	require.Equal(t, rat("700"), l.Parts[0].Pooled)
}

// And past what an allowance gives: what the pools take is what is billable
// once both are included.
func TestAPoolTakesAfterAnAllowance(t *testing.T) {
	v := pool("10000", always[0], always[1])
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{
		Since: day(1), Pools: []rating.Pool{v}, Allowances: []rating.Allowance{given(500, always[0], time.Time{})},
	})

	require.Equal(t, rat("200"), l.Billable)
	require.Equal(t, rat("200"), l.Pooled)
	require.Zero(t, l.Amount.Sign())
}

// A pool takes no more than what is left of it, and what is left to bill is
// priced: 300 left of 700 billable, 400 at 50.
func TestWhatAPoolHasNotIsPriced(t *testing.T) {
	v := pool("300", always[0], always[1])
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})

	require.Equal(t, rat("300"), l.Pooled)
	require.Equal(t, rat("20000"), l.Amount)

	// A fraction of a unit, exactly: what is left of a pool is what the
	// periods before left of it.
	v = pool("1001/3", always[0], always[1])
	l = rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})
	require.Equal(t, rat("1001/3"), l.Pooled)
	require.Equal(t, rat("54950/3"), l.Amount, "(700 - 1001/3) * 50")
}

// Of two, the one that expires sooner is taken from first.
func TestThePoolThatExpiresSoonestIsTakenFromFirst(t *testing.T) {
	late := pool("500", always[0], day(1).AddDate(0, 6, 0))
	soon := pool("500", always[0], day(1).AddDate(0, 2, 0))
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{late, soon}})

	require.Equal(t, []rating.Take{{Pool: soon.Id, Quantity: rat("500")}, {Pool: late.Id, Quantity: rat("200")}}, l.Takes)
}

// A pool is taken from by a period it is in force at any moment of, for the
// whole of what the period is billable for: begun on the 20th, or expired on
// the 10th, it takes all of January's -- and one that began after the
// period, or expired before it, nothing.
func TestAPoolIsTakenFromByEveryPeriodItIsInForceFor(t *testing.T) {
	for name, v := range map[string]rating.Pool{
		"begun during it":   pool("10000", day(20), always[1]),
		"expired during it": pool("10000", always[0], day(10)),
	} {
		l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})
		require.Equal(t, rat("700"), l.Pooled, name)
	}
	for name, v := range map[string]rating.Pool{
		"begun after it":    pool("10000", day(32), always[1]),
		"expired before":    pool("10000", always[0], day(1)),
		"with nothing left": pool("0", always[0], always[1]),
	} {
		l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})
		require.Zero(t, l.Pooled.Sign(), name)
		require.Empty(t, l.Takes, name)
		require.Equal(t, rat("35000"), l.Amount, name)
	}
}

// A price changed during the period: a pool takes of each part in
// proportion to what it is billable for, and each is priced for the rest.
func TestAPoolTakesOfEachPartInProportion(t *testing.T) {
	book := pricing.Book{
		version(perUnit(100, 50, 1), day(1), day(1)),
		version(perUnit(200, 80, 1), day(11), day(11)),
	}
	v := pool("300", always[0], always[1])
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), book, rating.Customer{Since: day(1), Pools: []rating.Pool{v}})

	// Billable: 700 for 10/31 and 600 for 21/31, 19600/31 in all.
	require.Equal(t, rat("19600/31"), l.Billable)
	require.Equal(t, rat("300"), l.Pooled)
	require.Len(t, l.Parts, 2)
	require.Equal(t, rat("10500/98"), l.Parts[0].Pooled, "7000/31 of the 19600/31, of 300")
	require.Equal(t, rat("18900/98"), l.Parts[1].Pooled)
	sum := new(big.Rat).Add(l.Parts[0].Pooled, l.Parts[1].Pooled)
	require.Equal(t, rat("300"), sum)
	// What is left of each, priced: (7000/31) * (1 - 300*31/19600) * 50, and
	// (12600/31) * the same * 80.
	left := new(big.Rat).Sub(big.NewRat(1, 1), new(big.Rat).Quo(rat("300"), rat("19600/31")))
	want := new(big.Rat).Add(
		new(big.Rat).Mul(new(big.Rat).Mul(rat("7000/31"), left), rat("50")),
		new(big.Rat).Mul(new(big.Rat).Mul(rat("12600/31"), left), rat("80")),
	)
	require.Equal(t, want, l.Amount)
}

// A price by bands is billable for nothing a pool could take.
func TestAPriceByBandsIsTakenFromByNoPool(t *testing.T) {
	keep := pricing.Book{version(pricing.Banded{Bands: []pricing.Band{
		{UpTo: 365, Bounded: true, Fee: 0},
		{Fee: 30000},
	}}, day(1), day(1))}
	u := rating.Usage{Held: usage.Profile(daily(1, 32, 1000), jan.Start, jan.End, time.Time{})}
	l := rateWith(t, meter(stock, last), u, keep, rating.Customer{Since: day(1), Pools: []rating.Pool{pool("10000", always[0], always[1])}})

	require.Equal(t, rat("30000"), l.Amount)
	require.Zero(t, l.Pooled.Sign())
}

// A flow's bundle is of what it counts.
func TestAFlowsPoolIsOfWhatItCounts(t *testing.T) {
	book := pricing.Book{version(perUnit(1000, 2, 100), day(1), day(1))}
	l := rateWith(t, meter(flow, sum), rating.Usage{Counted: 6000}, book, rating.Customer{Since: day(1), Pools: []rating.Pool{pool("4000", always[0], always[1])}})

	require.Equal(t, rat("4000"), l.Pooled)
	require.Equal(t, rat("20"), l.Amount, "1000 past 1000 and 4000, at 2 a hundred")
}
