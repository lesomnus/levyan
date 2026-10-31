package dunning_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/pg"
)

const day = 24 * time.Hour

// t0 is when an invoice's first charge failed, in every test here.
var t0 = time.Date(2027, 2, 10, 9, 0, 0, 0, time.UTC)

// failed is a charge made at `at` that failed as `d` a minute after.
func failed(at time.Time, d pg.Decline) dunning.Charge {
	return dunning.Charge{Made: at.Add(-time.Minute), Sent: at.Add(-time.Minute), Failed: at, Decline: d}
}

// out is a charge made at `at` and not settled.
func out(at time.Time) dunning.Charge { return dunning.Charge{Made: at, Sent: at, Out: true} }

// The table as Claude proposed it, provisionally: a temporary decline is
// tried again a day, three and five days after it failed, with a week of
// grace; a method to replace is never retried, with a week too; suspected
// fraud is restricted at once; the other kind is as the temporary one; and a
// workspace nobody pays for has its thirty days.
func TestTheDefaultsAreTheTableClaudeProposed(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()
	x.NoError(p.Check())

	x.Equal(dunning.Row{Retries: []time.Duration{day, 3 * day, 5 * day}, Grace: 7 * day}, p.Temporary)
	x.Equal(dunning.Row{Grace: 7 * day}, p.Replace)
	x.Equal(dunning.Row{AtOnce: true}, p.Fraud)
	x.Equal(p.Temporary, p.Other)
	p.Other.Retries[0] = time.Hour
	x.Equal(day, p.Temporary.Retries[0], "the other kind's retries are its own")
	x.Equal(dunning.Row{Grace: 30 * day}, p.Payerless)

	x.Equal(p.Temporary, dunning.Defaults().Of(pg.DeclineTemporary))
	x.Equal(p.Replace, p.Of(pg.DeclineReplace))
	x.Equal(p.Fraud, p.Of(pg.DeclineFraud))
	x.Equal(p.Other, p.Of(pg.DeclineOther))
	x.Equal(p.Other, p.Of(pg.Decline(9)), "a kind the table has no row of is the other kind")
}

// A row that says no schedule is refused: a retry not after the failure, or
// not after the one before it, a grace less than nothing -- and a workspace
// nobody pays for, which nothing is charged of, retries nothing.
func TestATableThatSaysNoScheduleIsRefused(t *testing.T) {
	x := require.New(t)

	for row, says := range map[*dunning.Row]string{
		{Retries: []time.Duration{0}}:          "retries: 0s is no time after the failure",
		{Retries: []time.Duration{-day}}:       "retries: -24h0m0s is no time after the failure",
		{Retries: []time.Duration{day, day}}:   "retries: 24h0m0s is no later than the retry before it, 24h0m0s",
		{Retries: []time.Duration{3 * day, 1}}: "retries: 1ns is no later than the retry before it, 72h0m0s",
		{Grace: -time.Second}:                  "grace: less than nothing",
	} {
		x.ErrorContains(row.Check(), says)
		p := dunning.Defaults()
		p.Fraud = *row
		x.ErrorContains(p.Check(), "fraud: "+says)
	}

	p := dunning.Defaults()
	p.Payerless.Retries = []time.Duration{day}
	x.ErrorContains(p.Check(), "payerless: retries: nothing is charged of a workspace nobody pays for")
}

// An invoice none of whose charges failed is in no dunning: its first charge
// is out, or it has none.
func TestAnInvoiceWhoseChargeDidNotFailIsInNoDunning(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()

	_, ok := p.Dun(nil, time.Time{}, t0)
	x.False(ok)
	_, ok = p.Dun([]dunning.Charge{out(t0)}, time.Time{}, t0.Add(30*day))
	x.False(ok)
}

// A temporary decline, through its row: tried again a day, three days and
// five days after the first failure, each once its time has come and once
// only -- a charge made at or after a retry's time covers it -- restricted
// once the week's grace is over, and given up on then, no retry being left.
func TestATemporaryDeclineIsRetriedAndThenGivenUpOn(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()
	cs := []dunning.Charge{failed(t0, pg.DeclineTemporary)}

	st, ok := p.Dun(cs, time.Time{}, t0)
	x.True(ok)
	x.Equal(pg.DeclineTemporary, st.Kind)
	x.Equal(t0, st.Failed)
	x.Equal(t0.Add(day), st.Retry, "the first retry to come")
	x.Equal(t0.Add(7*day), st.GraceEnds)
	x.Equal(t0.Add(7*day), st.Restricted)
	x.False(st.Due)
	x.False(st.Spent)

	st, _ = p.Dun(cs, time.Time{}, t0.Add(day-time.Second))
	x.False(st.Due, "not before its time")

	for i, at := range []time.Duration{day, 3 * day, 5 * day} {
		now := t0.Add(at)
		st, _ = p.Dun(cs, time.Time{}, now)
		x.True(st.Due, "retry %d", i+1)
		x.Equal(now, st.Retry, "due now: its time")

		// Made, and out: nothing more is due while it is.
		cs = append(cs, out(now))
		st, _ = p.Dun(cs, time.Time{}, now.Add(time.Minute))
		x.False(st.Due, "retry %d is out", i+1)
		x.False(st.Spent)

		// Failed: covered, however often it is asked.
		cs[len(cs)-1] = failed(now.Add(time.Hour), pg.DeclineTemporary)
		cs[len(cs)-1].Made, cs[len(cs)-1].Sent = now, now
		st, _ = p.Dun(cs, time.Time{}, now.Add(2*time.Hour))
		x.False(st.Due, "retry %d is made", i+1)
		x.Equal(t0, st.Failed, "measured from the first failure")
	}

	st, _ = p.Dun(cs, time.Time{}, t0.Add(6*day))
	x.True(st.Retry.IsZero(), "no retry left")
	x.False(st.Spent, "within its grace")

	st, _ = p.Dun(cs, time.Time{}, t0.Add(7*day))
	x.True(st.Spent, "no retry left, nothing out, and its grace over")
	x.False(st.Due)
	x.Equal(t0.Add(7*day), st.Restricted)
}

// Of the retries whose times passed while nothing ran, only the latest is
// made: the charges are not bunched up after an outage.
func TestOfRetriesMissedOnlyTheLatestIsMade(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()
	cs := []dunning.Charge{failed(t0, pg.DeclineOther)}

	st, _ := p.Dun(cs, time.Time{}, t0.Add(4*day))
	x.True(st.Due)
	x.Equal(t0.Add(3*day), st.Retry, "the latest whose time has come")

	cs = append(cs, failed(t0.Add(4*day+time.Minute), pg.DeclineOther))
	cs[1].Made = t0.Add(4 * day)
	st, _ = p.Dun(cs, time.Time{}, t0.Add(4*day+time.Hour))
	x.False(st.Due, "the one made covers both")
	x.Equal(t0.Add(5*day), st.Retry, "and the next is the one to come")
}

// A charge is due as soon as the billing account becomes collectable again
// after its last charge was sent -- whatever the row schedules, a method to
// replace included -- and once made, it is not due again for that.
func TestAChargeIsDueAsTheBillingAccountBecomesCollectableAgain(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()
	cs := []dunning.Charge{failed(t0, pg.DeclineReplace)}

	st, _ := p.Dun(cs, time.Time{}, t0.Add(2*day))
	x.True(st.Retry.IsZero(), "a method to replace is never retried")
	x.False(st.Due)

	st, _ = p.Dun(cs, t0.Add(-day), t0.Add(2*day))
	x.False(st.Due, "collectable since before its charge was sent: that is what failed")

	back := t0.Add(2 * day)
	st, _ = p.Dun(cs, back, back.Add(time.Minute))
	x.True(st.Due, "collectable again")
	x.Equal(back, st.Retry)

	cs = append(cs, out(back.Add(time.Minute)))
	st, _ = p.Dun(cs, back, back.Add(2*time.Minute))
	x.False(st.Due, "made, and out")
	cs[1] = failed(back.Add(3*time.Minute), pg.DeclineReplace)
	cs[1].Made, cs[1].Sent = back.Add(time.Minute), back.Add(time.Minute)
	st, _ = p.Dun(cs, back, back.Add(time.Hour))
	x.False(st.Due, "made once for it")

	// After a week, given up on -- and collectable again after, due again.
	st, _ = p.Dun(cs, back, t0.Add(7*day))
	x.True(st.Spent)
	st, _ = p.Dun(cs, t0.Add(9*day), t0.Add(9*day))
	x.True(st.Due, "an invoice given up on is charged as its billing account becomes collectable again")
	x.False(st.Spent)
}

// The row is the one of the latest failure's kind, measured from the first
// failure: a temporary decline whose retry is declined as a method to
// replace is retried no more, and keeps the week it had.
func TestTheRowIsTheLatestFailuresKind(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()
	cs := []dunning.Charge{failed(t0, pg.DeclineTemporary), failed(t0.Add(day+time.Minute), pg.DeclineReplace)}
	cs[1].Made = t0.Add(day)

	st, _ := p.Dun(cs, time.Time{}, t0.Add(4*day))
	x.Equal(pg.DeclineReplace, st.Kind)
	x.Equal(t0, st.Failed)
	x.True(st.Retry.IsZero())
	x.False(st.Due, "the third day's retry is the temporary kind's")
	x.Equal(t0.Add(7*day), st.GraceEnds)
}

// Suspected fraud is restricted at once, as it failed, and with no retry and
// no grace is given up on at once; a row that restricts at once and has a
// grace and retries keeps trying through it.
func TestFraudIsRestrictedAtOnce(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()

	st, ok := p.Dun([]dunning.Charge{failed(t0, pg.DeclineFraud)}, time.Time{}, t0)
	x.True(ok)
	x.Equal(pg.DeclineFraud, st.Kind)
	x.Equal(t0, st.Restricted)
	x.Equal(t0, st.GraceEnds)
	x.True(st.Spent)
	x.True(st.Retry.IsZero())

	p.Fraud = dunning.Row{Retries: []time.Duration{2 * day}, Grace: 3 * day, AtOnce: true}
	st, _ = p.Dun([]dunning.Charge{failed(t0, pg.DeclineFraud)}, time.Time{}, t0.Add(day))
	x.Equal(t0, st.Restricted, "at once")
	x.Equal(t0.Add(2*day), st.Retry)
	x.False(st.Spent)
}

// A workspace nobody pays for is restricted as its row says: once its grace
// is over, or at once.
func TestAWorkspaceNobodyPaysForIsRestrictedAsItsRowSays(t *testing.T) {
	x := require.New(t)
	p := dunning.Defaults()

	x.Equal(t0.Add(30*day), p.Payerless.Restricts(t0))
	p.Payerless = dunning.Row{Grace: 30 * day, AtOnce: true}
	x.Equal(t0, p.Payerless.Restricts(t0))
}

// A span in a grant's description is said in days, or in hours.
func TestASpanIsSaidInDaysOrHours(t *testing.T) {
	x := require.New(t)
	x.Equal("30일", dunning.Korean(30*day))
	x.Equal("36시간", dunning.Korean(36*time.Hour))
	x.Equal("1h30m0s", dunning.Korean(90*time.Minute))
	x.Equal("0s", dunning.Korean(0))
}
