package rating_test

import (
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/internal/usage"
)

// What a workspace is given on top of what its prices include (#43): added
// to what the version in force includes, for the share of the period it is
// in force, and never below nothing billable.

func given(quantity int64, from, until time.Time) rating.Allowance {
	return rating.Allowance{Id: pdid.New(46), Quantity: quantity, From: from, Until: until}
}

func rateWith(t *testing.T, m rating.Meter, u rating.Usage, book pricing.Book, c rating.Customer) rating.Line {
	t.Helper()
	l, err := rating.Rate(m, u, jan, book, line, c)
	require.NoError(t, err)
	return l
}

// An allowance in force for the whole of a period adds all it gives to what
// the version includes: 800 held, 100 included and 500 given, 200 billable.
func TestAnAllowanceForAWholePeriodAddsToWhatIsIncluded(t *testing.T) {
	a := given(500, day(1).AddDate(0, -1, 0), time.Time{})
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Allowances: []rating.Allowance{a}})

	require.Equal(t, rat("800"), l.Quantity)
	require.Equal(t, rat("600"), l.Included, "what the version includes and what is given")
	require.Equal(t, rat("500"), l.Allowance)
	require.Equal(t, rat("200"), l.Billable)
	require.Equal(t, rat("10000"), l.Amount, "200 * 50")
	require.Len(t, l.Parts, 1)
	require.Equal(t, int64(100), l.Parts[0].Included)
	require.Equal(t, int64(500), l.Parts[0].Allowance)
	require.Equal(t, []pdid.Id{a.Id}, l.Parts[0].Allowances)
}

// An allowance in force for part of a period gives its share of the period:
// begun on the 11th, twenty-one days of 31; the stretch before it is billed
// past what the version alone includes.
func TestAnAllowanceForPartOfAPeriodGivesItsShare(t *testing.T) {
	a := given(500, day(11), time.Time{})
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Allowances: []rating.Allowance{a}})

	require.Equal(t, rat("10500/31"), l.Allowance, "500 * 21/31")
	require.Equal(t, rat("13600/31"), l.Included, "100 and 500 * 21/31")
	require.Equal(t, rat("11200/31"), l.Billable, "700 * 10/31 + 200 * 21/31")
	require.Equal(t, rat("560000/31"), l.Amount)
	require.Len(t, l.Parts, 2)
	require.Equal(t, day(1), l.Parts[0].From)
	require.Equal(t, day(11), l.Parts[0].To)
	require.Zero(t, l.Parts[0].Allowance)
	require.Empty(t, l.Parts[0].Allowances)
	require.Equal(t, rat("10/31"), l.Parts[0].Share)
	require.Equal(t, rat("700"), l.Parts[0].Billable)
	require.Equal(t, day(11), l.Parts[1].From)
	require.Equal(t, int64(500), l.Parts[1].Allowance)
	require.Equal(t, rat("21/31"), l.Parts[1].Share)
	require.Equal(t, rat("200"), l.Parts[1].Billable)

	// Ended on the 21st, as an operator ends one: ten days of it.
	a.Until = day(21)
	l = rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Allowances: []rating.Allowance{a}})
	require.Equal(t, rat("5000/31"), l.Allowance, "500 * 10/31")
	require.Len(t, l.Parts, 3)
}

// Two allowances add up while both are in force.
func TestAllowancesInForceTogetherAddUp(t *testing.T) {
	a, b := given(300, day(1), time.Time{}), given(200, day(11), day(21))
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Allowances: []rating.Allowance{b, a}})

	require.Equal(t, rat("11300/31"), l.Allowance, "300 all month and 200 for ten days of it")
	require.Len(t, l.Parts, 3)
	require.Equal(t, int64(500), l.Parts[1].Allowance)
	require.Equal(t, []pdid.Id{a.Id, b.Id}, l.Parts[1].Allowances, "by when they began")
}

// Never less than nothing billable: what is given past what was used is
// given for nothing, and carries nowhere.
func TestAnAllowanceNeverMakesWhatIsBillableLessThanNothing(t *testing.T) {
	a := given(5000, day(1), time.Time{})
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), plain, rating.Customer{Since: day(1), Allowances: []rating.Allowance{a}})

	require.Zero(t, l.Billable.Sign())
	require.Zero(t, l.Amount.Sign())
	require.Equal(t, rat("5000"), l.Allowance)
}

// A workspace rated from the first unit -- none of its payer's first few --
// includes nothing of its prices', and is given what it was given all the
// same.
func TestAWorkspaceRatedFromTheFirstUnitIsGivenItsAllowance(t *testing.T) {
	first := pricing.Book{version(perUnit(0, 50, 1), day(1), day(1))}
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), first, rating.Customer{Since: day(1), Allowances: []rating.Allowance{given(500, day(1), time.Time{})}})

	require.Equal(t, rat("300"), l.Billable)
	require.Equal(t, rat("15000"), l.Amount)
	require.Zero(t, l.Parts[0].Included)
	require.Equal(t, int64(500), l.Parts[0].Allowance)
}

// A version published during an allowance changes nothing of it: each part
// is given it on top of what that part's version includes.
func TestAPriceChangedDuringAnAllowanceChangesNothingOfIt(t *testing.T) {
	book := pricing.Book{
		version(perUnit(100, 50, 1), day(1), day(1)),
		version(perUnit(200, 80, 1), day(11), day(11)),
	}
	l := rateWith(t, meter(stock, twa), stockUsage(daily(1, 32, 800)), book, rating.Customer{Since: day(1), Allowances: []rating.Allowance{given(500, day(1), time.Time{})}})

	require.Len(t, l.Parts, 2)
	require.Equal(t, int64(100), l.Parts[0].Included)
	require.Equal(t, int64(500), l.Parts[0].Allowance)
	require.Equal(t, rat("200"), l.Parts[0].Billable)
	require.Equal(t, int64(200), l.Parts[1].Included)
	require.Equal(t, int64(500), l.Parts[1].Allowance)
	require.Equal(t, rat("100"), l.Parts[1].Billable)
	require.Equal(t, rat("268000/31"), l.Amount, "200 * 50 * 10/31 + 100 * 80 * 21/31")
	require.Equal(t, rat("500"), l.Allowance)
}

// A workspace that began during the period is given all of an allowance in
// force for all the time it was a customer, as what its price includes is
// not prorated for it either.
func TestAWorkspaceThatBeganDuringThePeriodIsGivenAllOfIt(t *testing.T) {
	u := stockUsage(daily(11, 32, 800))
	l := rateWith(t, meter(stock, twa), u, plain, rating.Customer{Since: day(11), Allowances: []rating.Allowance{given(500, day(1), time.Time{})}})

	require.Equal(t, rat("500"), l.Allowance)
	require.Equal(t, rat("600"), l.Included)

	// And one begun with it, the same.
	l = rateWith(t, meter(stock, twa), u, plain, rating.Customer{Since: day(11), Allowances: []rating.Allowance{given(500, day(11), time.Time{})}})
	require.Equal(t, rat("500"), l.Allowance)
}

// Away is no customer: an allowance gives its share of the time the
// workspace was one.
func TestAnAllowanceIsGivenForTheTimeTheWorkspaceWasACustomer(t *testing.T) {
	away := []rating.Stretch{{From: day(21), To: day(32)}}
	u := stockUsage(daily(1, 32, 800))
	u.Held = usage.Cut(u.Held, away)
	l := rateWith(t, meter(stock, twa), u, plain, rating.Customer{Since: day(1), Away: away, Allowances: []rating.Allowance{given(500, day(11), time.Time{})}})

	require.Equal(t, rat("250"), l.Allowance, "ten of the twenty days it was a customer")
}

// A price by bands includes nothing, and is given nothing.
func TestAPriceByBandsIsGivenNothing(t *testing.T) {
	keep := pricing.Book{version(pricing.Banded{Bands: []pricing.Band{
		{UpTo: 365, Bounded: true, Fee: 0},
		{Fee: 30000},
	}}, day(1), day(1))}
	u := rating.Usage{Held: usage.Profile(daily(1, 32, 1000), jan.Start, jan.End, time.Time{})}
	l := rateWith(t, meter(stock, last), u, keep, rating.Customer{Since: day(1), Allowances: []rating.Allowance{given(5000, day(1), time.Time{})}})

	require.Equal(t, rat("30000"), l.Amount)
	require.Zero(t, l.Allowance.Sign())
	require.Zero(t, l.Parts[0].Allowance)
}

// A flow is given what it counts, as a stock is what it holds.
func TestAFlowIsGivenOnTopOfWhatItsPriceIncludes(t *testing.T) {
	book := pricing.Book{version(perUnit(1000, 2, 100), day(1), day(1))}
	l := rateWith(t, meter(flow, sum), rating.Usage{Counted: 6000}, book, rating.Customer{Since: day(1), Allowances: []rating.Allowance{given(2000, day(1), time.Time{})}})

	require.Equal(t, rat("3000"), l.Billable)
	require.Equal(t, rat("60"), l.Amount, "3000 past 1000 and 2000, at 2 a hundred")
}

// What is given past what a meter holds is refused rather than wrapped.
func TestAllowancesPastWhatAMeterHoldsAreRefused(t *testing.T) {
	_, err := rating.Rate(meter(stock, twa), stockUsage(daily(1, 32, 800)), jan, plain, line, rating.Customer{Since: day(1), Allowances: []rating.Allowance{
		given(1<<62, day(1), time.Time{}),
		given(1<<62, day(1), time.Time{}),
	}})
	require.ErrorIs(t, err, rating.ErrTooMuch)
}
