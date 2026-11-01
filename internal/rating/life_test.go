package rating_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/internal/usage"
)

var (
	closed    = levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_CLOSED
	restored  = levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED
	destroyed = levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED
)

func ev(kind levyan.WorkspaceEventKind, d int) rating.Event {
	return rating.Event{Kind: kind, At: day(d)}
}

func away(from, to int) rating.Stretch { return rating.Stretch{From: day(from), To: day(to)} }

func forever(from int) rating.Stretch { return rating.Stretch{From: day(from), To: rating.Forever} }

// What became of a workspace is read in the order of its moments, never of
// its coming: the same said again changes nothing, one said late takes its
// place, and an older one never undoes what a newer one says. A stretch
// closed and then restored is no stretch away, so only what is closed still,
// or ended in a destruction, is -- to [rating.Forever]: the owner's
// (2026-11-01). A destruction is for good.
func TestALifeIsReadInTheOrderOfItsMoments(t *testing.T) {
	for _, tc := range []struct {
		name string
		es   []rating.Event
		want rating.Life
	}{
		{"nothing said, never away", nil, rating.Life{}},
		{"closed and restored, billed as if open", []rating.Event{ev(closed, 11), ev(restored, 21)}, rating.Life{}},
		{"the restore came first", []rating.Event{ev(restored, 21), ev(closed, 11)}, rating.Life{}},
		{"each said twice", []rating.Event{ev(closed, 11), ev(restored, 21), ev(closed, 11), ev(restored, 21)}, rating.Life{}},
		{"closed, and not restored yet", []rating.Event{ev(closed, 11)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11)}},
		{"a close of what is closed changes nothing", []rating.Event{ev(closed, 11), ev(closed, 15)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11)}},
		{"a restore of what is open changes nothing", []rating.Event{ev(restored, 5), ev(closed, 11)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11)}},
		{"an older close takes its place, and the newer restore stands", []rating.Event{ev(closed, 11), ev(restored, 21), ev(closed, 5)}, rating.Life{}},
		{"closed twice, the second time still", []rating.Event{ev(closed, 3), ev(restored, 6), ev(closed, 11)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11)}},
		{"closed and restored at once was never closed", []rating.Event{ev(restored, 11), ev(closed, 11)}, rating.Life{}},
		{"destroyed, for good", []rating.Event{ev(destroyed, 15), ev(closed, 20), ev(restored, 25)}, rating.Life{Away: []rating.Stretch{forever(15)}, Destroyed: day(15)}},
		{"destroyed closed, from its close", []rating.Event{ev(closed, 11), ev(destroyed, 15)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11), Destroyed: day(15)}},
		{"restored, and destroyed after", []rating.Event{ev(closed, 5), ev(restored, 10), ev(destroyed, 15)}, rating.Life{Away: []rating.Stretch{forever(15)}, Destroyed: day(15)}},
		{"destroyed before the restore said first", []rating.Event{ev(closed, 11), ev(restored, 21), ev(destroyed, 15)}, rating.Life{Away: []rating.Stretch{forever(11)}, Closed: day(11), Destroyed: day(15)}},
		{"destroyed twice is the first", []rating.Event{ev(destroyed, 20), ev(destroyed, 15)}, rating.Life{Away: []rating.Stretch{forever(15)}, Destroyed: day(15)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, rating.Lived(tc.es))
		})
	}

	l := rating.Lived([]rating.Event{ev(closed, 11), ev(restored, 21)})
	require.False(t, l.Through(jan), "a customer of all of it")
	require.False(t, l.Through(rating.Period{Start: day(12), End: day(20)}), "closed then, and restored since: billed as if open")
	still := rating.Lived([]rating.Event{ev(closed, 11)})
	require.False(t, still.Through(jan), "a customer of some of it")
	require.True(t, still.Through(rating.Period{Start: day(12), End: day(20)}))
	require.True(t, rating.Lived([]rating.Event{{Kind: closed, At: day(1).Add(-time.Hour)}}).Through(jan))
	require.False(t, rating.Lived(nil).Through(jan))
}

// What an event can move of what a workspace is billed for reaches back to
// the close of the stretch it ends, when it ends one: a restore makes the
// stretch billed, and a destruction makes one a restore would have billed
// not billed; anything else moves what is from its own moment on.
func TestAnEventReachesBackToTheCloseOfTheStretchItEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		es   []rating.Event
		e    rating.Event
		want time.Time
	}{
		{"a close, from its moment", []rating.Event{ev(closed, 11)}, ev(closed, 11), day(11)},
		{"a close of what is closed, from its moment", []rating.Event{ev(closed, 5)}, ev(closed, 11), day(11)},
		{"a restore, from the close it ends", []rating.Event{ev(closed, 11)}, ev(restored, 21), day(11)},
		{"the same, among the events", []rating.Event{ev(closed, 11), ev(restored, 21)}, ev(restored, 21), day(11)},
		{"from the first close of the stretch", []rating.Event{ev(closed, 5), ev(closed, 11)}, ev(restored, 21), day(5)},
		{"from the close after the restore before it", []rating.Event{ev(closed, 3), ev(restored, 6), ev(closed, 11)}, ev(restored, 21), day(11)},
		{"a restore of what is open, from its moment", []rating.Event{ev(closed, 3), ev(restored, 6)}, ev(restored, 21), day(21)},
		{"a destruction of what is open, from its moment", nil, ev(destroyed, 15), day(15)},
		{"a destruction of what was closed still, from its moment", []rating.Event{ev(closed, 11)}, ev(destroyed, 15), day(15)},
		{"a destruction before a restore, from the close it takes back", []rating.Event{ev(closed, 11), ev(restored, 21)}, ev(destroyed, 15), day(11)},
		{"after a destruction, from its moment", []rating.Event{ev(closed, 5), ev(destroyed, 10)}, ev(restored, 21), day(21)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, rating.Reach(tc.es, tc.e))
		})
	}
}

// cut is what `rs` held and were while a customer, as the ledger hands it to
// the rating: the time away taken out.
func cut(rs []usage.Reading, ss ...rating.Stretch) rating.Usage {
	in := []usage.Reading{}
	for _, r := range rs {
		if !usage.In(r.At, ss) {
			in = append(in, r)
		}
	}
	return rating.Usage{Held: usage.Cut(usage.Profile(rs, jan.Start, jan.End, time.Time{}), ss), Readings: in}
}

func rateAway(t *testing.T, m rating.Meter, u rating.Usage, book pricing.Book, since time.Time, ss ...rating.Stretch) rating.Line {
	t.Helper()
	l, err := rating.Rate(m, u, jan, book, line, rating.Customer{Since: since, Away: ss})
	require.NoError(t, err)
	return l
}

// A workspace away is no customer (#14): what it held then is not what it
// held, so its average counts that time as nothing and its most is of what
// was measured while it was not; what its prices include is all of it, as
// for one that began during the period; a price by bands charges the time
// outside; and a workspace away for all of the period is billed nothing.
func TestAWorkspaceAwayIsNoCustomer(t *testing.T) {
	x := require.New(t)
	rs := daily(1, 32, 200)

	// Away from the 11th to the 21st: 21 days of 200.
	l := rateAway(t, meter(stock, twa), cut(rs, away(11, 21)), plain, day(1), away(11, 21))
	x.Equal(rat("4200/31"), l.Quantity)
	x.Equal(rat("100"), l.Included, "all of it")
	x.Equal(rat("55000/31"), l.Amount, "(4200/31 - 100) * 50")
	x.Equal(int64(1774), total(t, l))

	// The most is of what was measured while it was a customer.
	high := append(daily(1, 11, 150), append(daily(11, 21, 900), daily(21, 32, 150)...)...)
	l = rateAway(t, meter(stock, most), cut(high, away(11, 21)), plain, day(1), away(11, 21))
	x.Equal(rat("150"), l.Quantity)

	// A price by bands, the time outside.
	bands := version(pricing.Banded{Bands: []pricing.Band{
		{UpTo: 365, Bounded: true, Fee: 0},
		{UpTo: 1095, Bounded: true, Fee: 10000},
		{Fee: 30000},
	}}, day(1), day(1))
	l = rateAway(t, meter(stock, last), cut(daily(1, 32, 730), away(11, 21)), pricing.Book{bands}, day(1), away(11, 21))
	x.Equal(rat("210000/31"), l.Amount, "21 of 31 days in the 10,000 band")

	// Two versions, and away all the time the first was in force: the second
	// is charged all of it.
	dear := version(perUnit(100, 100, 1), day(16), day(16))
	two := pricing.Book{plain[0], dear}
	l = rateAway(t, meter(stock, most), cut(rs, away(1, 16)), two, day(1), away(1, 16))
	x.Len(l.Parts, 1)
	x.Equal(big.NewRat(1, 1), l.Parts[0].Share)
	x.Equal(rat("10000"), l.Amount, "100 past what is included, at 100")

	// Away all of it: nothing.
	l = rateAway(t, meter(stock, twa), cut(rs, forever(1)), plain, day(1), forever(1))
	x.Zero(l.Amount.Sign())
	x.Empty(l.Parts)

	// And what was priced elsewhere is said only of the time it was a
	// customer.
	usd := version(perUnit(0, 5, 1), day(1), day(1))
	usd.Currency = "USD"
	l = rateAway(t, meter(stock, twa), cut(rs, away(11, 21)), pricing.Book{usd}, day(1), away(11, 21))
	x.Equal([]rating.Stretch{away(1, 11), {From: day(21), To: jan.End}}, l.Unpriced)
}
