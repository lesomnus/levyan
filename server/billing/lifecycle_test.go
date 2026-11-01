package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entoffer "github.com/lesomnus/levyan/internal/ent/offer"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
)

// What became of a workspace (#14), as what its periods come to: closed and
// then restored, it is billed for the stretch it was closed for as for any
// open time; closed still when a period is rated, or closed and then
// destroyed, it is billed for none of the time it was, and closed through a
// whole period it is billed nothing for it and its place among its payer's
// free workspaces passes on; destroyed, its last reading holds no further
// and nothing measured after is kept. What is billed of a closed stretch is
// the owner's (2026-11-01). Each is said once of a moment and read in the
// order of the moments, and one said late -- a restore of a stretch that
// began in a period closed since, however soon it is said -- is a
// correction on a later statement, as a late reading is.
//
// Every amount is worked out by hand: setup's active assets at 50 won each
// past the 100 included, on the product's gateway in won.

// said is what a product's call of WorkspaceService `method` -- close,
// restore, destroy -- of workspace `ws` at `at` answered.
func (w world) said(method string, ws pdid.Id, at time.Time) (*levyanv1.Lifecycle, bool, error) {
	w.t.Helper()
	c := levyanv1.NewWorkspaceServiceClient(w.conn)
	switch method {
	case "close":
		res, err := c.Close(w.ctx, levyanv1.CloseRequest_builder{Workspace: ws.String(), At: timestamppb.New(at)}.Build())
		return res.GetLifecycle(), res.GetLate(), err
	case "restore":
		res, err := c.Restore(w.ctx, levyanv1.RestoreRequest_builder{Workspace: ws.String(), At: timestamppb.New(at)}.Build())
		return res.GetLifecycle(), res.GetLate(), err
	case "destroy":
		res, err := c.Destroy(w.ctx, levyanv1.DestroyRequest_builder{Workspace: ws.String(), At: timestamppb.New(at)}.Build())
		return res.GetLifecycle(), res.GetLate(), err
	}
	w.t.Fatalf("no method %q", method)
	return nil, false, nil
}

// say is said, which is to be answered and on time.
func (w world) say(method string, ws pdid.Id, at time.Time) *levyanv1.Lifecycle {
	w.t.Helper()
	l, late, err := w.said(method, ws, at)
	w.X.NoError(err)
	w.X.False(late, "%s at %s", method, at)
	return l
}

// record is what came of one reading of active assets.
func (w world) record(ws pdid.Id, at time.Time, v int64) *levyanv1.Recorded {
	w.t.Helper()
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.ctx, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(at), Value: v, Unit: "{asset}"}.Build(),
	}}.Build())
	w.X.NoError(err)
	w.X.Len(res.GetResults(), 1)
	return res.GetResults()[0]
}

// A workspace closed on the 11th and restored on the 21st is billed for all
// of the month, as if it had been open: what it held while closed is kept,
// and billed once it is restored. While it is closed, what the month would
// come to stops at its close. Closed for some of the month, it is one of its
// payer's free workspaces as any open one is.
func TestAWorkspaceClosedAndRestoredInAPeriodIsBilledAsIfOpen(t *testing.T) {
	one := 1
	w := setup(t, func(c *cmd.Config) { c.Payers.Free = &one })
	x := w.X

	owner := levyantest.Account()
	ws, open := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 1).Add(time.Hour))
	w.arrive(ws, owner, on(1, 1))
	w.arrive(open, owner, on(1, 1).Add(time.Hour))

	w.At(on(1, 11))
	l := w.say("close", ws, on(1, 11))
	x.True(l.GetClosedSince().AsTime().Equal(on(1, 11)))
	x.Nil(l.GetDestroyed())

	// Four days into it: ten days of 1000 so far, and nothing after the
	// close carried to the end of the month -- (10000/31 - 100) * 50.
	w.At(on(1, 15))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(1, 16), 1000)
	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(int64(11129), u.GetUsage().GetTotal(), "closed still: what it would come to stops at the close")

	w.At(on(1, 21))
	l = w.say("restore", ws, on(1, 21))
	x.Nil(l.GetClosedSince(), "open again")
	u, err = levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(int64(45000), u.GetUsage().GetTotal(), "restored: its latest reading carried through the month, the closed stretch billed")

	// And what it sent: 1000 bytes by the 5th, 5000 more while it was
	// closed, and 3000 after.
	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 16), on(2, 1), 1000)
	w.counted(ws, on(1, 1), map[time.Time]int64{on(1, 5): 1000, on(1, 15): 6000, on(1, 25): 9000})
	w.daily(open, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 1000)
	w.close(on(2, 10))

	// All 31 days of 1000, 900 past what is included at 50; and all 9000
	// bytes sent, 8000 past what is included at 2 won a hundred.
	s := w.statement(ws, on(1, 1))
	x.Equal(map[string]string{"loft.assets.active": "45000", "loft.transfer.bytes": "160"}, amounts(s))
	x.Equal(int64(45160), s.GetTotal())
	x.False(w.row(ws, on(1, 1)).FirstUnit, "the payer's one free workspace, made first")
	x.Equal(uint32(3), w.row(ws, on(1, 1)).Rating, "rated by the rules that bill a stretch closed and restored")
	x.Equal(int64(50000), w.statement(open, on(1, 1)).GetTotal(), "the other, from the first unit: 1000 at 50")
	x.True(w.row(open, on(1, 1)).FirstUnit)

	got, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetLifecycle(w.ctx, levyanv1.GetLifecycleRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(ws.String(), got.GetLifecycle().GetWorkspace())
	x.Nil(got.GetLifecycle().GetClosedSince())
}

// A workspace closed still when a period is rated is billed for none of the
// time it was closed, and closed through a whole period it is billed nothing
// for it -- it has no statement -- and is none of its payer's free
// workspaces for it: the next one has them. Closed for some of a period, it
// counts. What it would come to now stops at its close.
func TestAWorkspaceClosedStillIsBilledNothingOfItsClose(t *testing.T) {
	w, a, b := pair(t)
	x := w.X

	w.At(on(1, 20))
	w.say("close", a, on(1, 20))
	w.At(on(2, 10))
	w.close(on(2, 10))
	w.At(on(3, 10))
	w.close(on(3, 10))

	// January: 19 days of 200 over 31, (3800/31 - 100) * 50 = 35000/31;
	// the first is open for some of it, and the payer's one free workspace,
	// so the second is rated from the first unit, 200 at 50.
	x.Equal(int64(1129), w.statement(a, on(1, 1)).GetTotal())
	x.False(w.row(a, on(1, 1)).FirstUnit)
	x.Equal(int64(10000), w.statement(b, on(1, 1)).GetTotal())
	x.True(w.row(b, on(1, 1)).FirstUnit)

	// February: the first was closed through it, and is billed nothing --
	// no statement -- and the second is the payer's free one.
	x.Nil(w.statement(a, on(2, 1)))
	x.Equal(int64(5000), w.statement(b, on(2, 1)).GetTotal())
	x.False(w.row(b, on(2, 1)).FirstUnit)

	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.Zero(u.GetUsage().GetTotal(), "closed through March so far")
}

// A workspace closed in one period, and restored in the next after the
// first was closed, is billed for the stretch it was closed for: the
// restore reaches back to the close, so it is late however soon it is said,
// and the first period is corrected on the next statement by what the
// stretch comes to. Nothing written is changed. Open for some of each
// period, it counts among its payer's free workspaces in both.
func TestAWorkspaceRestoredAfterAPeriodClosedIsCorrected(t *testing.T) {
	w, a, b := pair(t)
	x := w.X

	w.At(on(1, 20))
	w.say("close", a, on(1, 20))
	w.At(on(2, 10))
	w.close(on(2, 10))
	x.Equal(int64(1129), w.statement(a, on(1, 1)).GetTotal(), "19 days of 200, as far as anybody knew")

	w.At(on(2, 15))
	_, late, err := w.said("restore", a, on(2, 15))
	x.NoError(err)
	x.True(late, "said at once, and late: the stretch began in January, closed since")
	_, late, err = w.said("restore", a, on(2, 15))
	x.NoError(err)
	x.False(late, "said again, it changes nothing")

	w.At(on(3, 10))
	w.close(on(3, 10))

	// January is as it was billed; February's statement has all of
	// February, 200 at 50 past 100, and corrects January by what it comes to
	// now -- all of it, (200 - 100) * 50 -- less what it was billed:
	// 5000 - 35000/31 = 120000/31.
	x.Equal(int64(1129), w.statement(a, on(1, 1)).GetTotal())
	feb := w.statement(a, on(2, 1))
	x.Equal(map[string]string{
		"loft.assets.active":                  "5000",
		"loft.assets.active corrects 2027-01": "3870.967741",
	}, amounts(feb))
	x.Equal(int64(8870), feb.GetTotal())
	x.False(w.row(a, on(2, 1)).FirstUnit)
	x.Equal(int64(10000), w.statement(b, on(2, 1)).GetTotal())
	x.True(w.row(b, on(2, 1)).FirstUnit)
}

// A workspace closed through a whole period and restored after it was
// closed is billed for that period too, on a later statement, and counts in
// it as an open workspace does: rated again, its free place is its own, by
// its payer's first link to it. The workspace that had the place while it
// was closed keeps what it was billed -- a correction rates again only the
// workspace a late row is of -- so for that period its payer had one free
// workspace more than it would have had, had the restore been known: less
// is billed (docs/design.md §17, 103).
func TestAWorkspaceRestoredAfterAPeriodItWasClosedThroughCountsInIt(t *testing.T) {
	w, a, b := pair(t)
	x := w.X

	w.At(on(1, 20))
	w.say("close", a, on(1, 20))
	w.At(on(2, 10))
	w.close(on(2, 10))
	w.At(on(3, 10))
	w.close(on(3, 10))
	x.Nil(w.statement(a, on(2, 1)), "closed through February, as far as anybody knew")
	x.False(w.row(b, on(2, 1)).FirstUnit, "and the second had its place")

	w.At(on(3, 15))
	_, late, err := w.said("restore", a, on(3, 15))
	x.NoError(err)
	x.True(late, "reaching back to January 20th")

	w.At(on(4, 10))
	w.close(on(4, 10))

	// March: all of it, and the payer's free one again -- (200 - 100) * 50
	// -- and the corrections of January, 120000/31 as above, and of
	// February, rated now as an open workspace that has its place: 5000,
	// less nothing billed.
	mar := w.statement(a, on(3, 1))
	x.Equal(map[string]string{
		"loft.assets.active":                  "5000",
		"loft.assets.active corrects 2027-01": "3870.967741",
		"loft.assets.active corrects 2027-02": "5000",
	}, amounts(mar))
	x.Equal(int64(13870), mar.GetTotal())
	x.False(w.row(a, on(3, 1)).FirstUnit)
	x.Equal(int64(10000), w.statement(b, on(3, 1)).GetTotal())
	x.True(w.row(b, on(3, 1)).FirstUnit)

	// February's statement of the second is as it was written.
	x.Equal(int64(5000), w.statement(b, on(2, 1)).GetTotal())
	x.False(w.row(b, on(2, 1)).FirstUnit)
}

// A workspace closed through a whole period and restored before that period
// is rated is billed for it as any open workspace is, and counts in it among
// its payer's free workspaces: here it has the one place, by its payer's
// first link to it, and the other is rated from the first unit.
func TestAWorkspaceRestoredBeforeAPeriodItWasClosedThroughIsRatedCountsInIt(t *testing.T) {
	w, a, b := pair(t)
	x := w.X

	w.At(on(1, 20))
	w.say("close", a, on(1, 20))
	w.At(on(3, 5))
	w.say("restore", a, on(3, 5))
	w.At(on(4, 10))
	w.close(on(4, 10))

	for _, m := range []int{1, 2, 3} {
		x.Equal(int64(5000), w.statement(a, on(m, 1)).GetTotal(), "all of the month, (200 - 100) * 50")
		x.False(w.row(a, on(m, 1)).FirstUnit, "the payer's free one")
		x.Len(w.statement(a, on(m, 1)).GetLines(), 1, "and nothing to correct")
		x.Equal(int64(10000), w.statement(b, on(m, 1)).GetTotal(), "200 at 50")
		x.True(w.row(b, on(m, 1)).FirstUnit)
	}
}

// A workspace closed and then destroyed is billed for none of the time it
// was closed, as one closed still is: the destruction is no restore. Said in
// order, it moves nothing before its own moment, so it is no later than
// that is; and through the period it was away in, its place among its
// payer's free workspaces passes on.
func TestAWorkspaceClosedAndDestroyedIsBilledNothingOfItsClose(t *testing.T) {
	w, a, b := pair(t)
	x := w.X

	w.At(on(1, 20))
	w.say("close", a, on(1, 20))
	w.At(on(2, 10))
	w.close(on(2, 10))
	w.At(on(3, 5))
	l := w.say("destroy", a, on(3, 5))
	x.True(l.GetClosedSince().AsTime().Equal(on(1, 20)), "the closure it was destroyed in")
	w.At(on(3, 10))
	w.close(on(3, 10))
	w.At(on(4, 10))
	w.close(on(4, 10))

	x.Equal(int64(1129), w.statement(a, on(1, 1)).GetTotal())
	x.Nil(w.statement(a, on(2, 1)), "nothing of February, nor any correction of January")
	x.Nil(w.statement(a, on(3, 1)))
	for _, m := range []int{2, 3} {
		x.Equal(int64(5000), w.statement(b, on(m, 1)).GetTotal(), "the second has the free place")
		x.False(w.row(b, on(m, 1)).FirstUnit)
	}
}

// pair is two workspaces of one payer's, made on New Year's day -- the first
// an hour before the second -- of a deployment where a payer has one free
// workspace, each holding 200 active assets every day through April.
func pair(t *testing.T) (world, pdid.Id, pdid.Id) {
	t.Helper()
	one := 1
	w := setup(t, func(c *cmd.Config) { c.Payers.Free = &one })
	owner := levyantest.Account()
	a, b := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 1).Add(3 * time.Hour))
	w.arrive(a, owner, on(1, 1).Add(time.Hour))
	w.arrive(b, owner, on(1, 1).Add(2*time.Hour))
	w.At(on(4, 9))
	for _, ws := range []pdid.Id{a, b} {
		w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(4, 9), 200)
	}

	return w, a, b
}

// A workspace destroyed is billed for nothing after, for good: its last
// reading holds no further, nothing measured from then is kept, its payer's
// link ends there, what was offered of it is withdrawn, and it is neither
// restricted for having no payer nor given anything. Said again, it changes
// nothing; a close or a restore after it, or an offer of it, is refused.
func TestADestroyedWorkspaceIsBilledForNothingAfter(t *testing.T) {
	w := setup(t)
	x := w.X
	ctx := context.Background()

	owner, other := levyantest.Account(), levyantest.Account()
	ws, left := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 1))
	w.arrive(ws, owner, on(1, 1))
	w.arrive(left, owner, on(1, 1))
	w.At(on(1, 10))
	_, err := w.S.Payers.Offer(ctx, w.product(), ws, other)
	x.NoError(err)

	w.At(on(1, 15).Add(6 * time.Hour))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(1, 16), 1000)
	l := w.say("destroy", ws, on(1, 15).Add(6*time.Hour))
	x.True(l.GetDestroyed().AsTime().Equal(on(1, 15).Add(6 * time.Hour)))
	_, err = w.S.Payers.Leave(ctx, w.product(), left, owner)
	x.NoError(err)

	w.At(on(1, 20))
	x.Equal(levyanv1.Refusal_REFUSAL_DESTROYED, w.record(ws, on(1, 15).Add(12*time.Hour), 1000).GetRefusal())
	x.Equal(levyanv1.Refusal_REFUSAL_DESTROYED, w.record(ws, on(1, 16), 1000).GetRefusal())
	x.Equal(levyanv1.Outcome_OUTCOME_ALREADY, w.record(ws, on(1, 15), 1000).GetOutcome(), "measured before, and kept")

	// Said again, later, it is as it was: destroyed when it first was.
	l = w.say("destroy", ws, on(1, 18))
	x.True(l.GetDestroyed().AsTime().Equal(on(1, 15).Add(6 * time.Hour)))
	_, _, err = w.said("restore", ws, on(1, 19))
	x.Equal(codes.FailedPrecondition, status.Code(err), "nothing is restored after")
	_, _, err = w.said("close", ws, on(1, 19))
	x.Equal(codes.FailedPrecondition, status.Code(err))
	_, err = w.S.Payers.Offer(ctx, w.product(), ws, other)
	x.Equal(codes.FailedPrecondition, status.Code(err), "nobody is asked to pay for it")

	// One heard of by its readings alone, destroyed, arrives to nobody.
	unpaid := levyantest.Tenant()
	x.Equal(levyanv1.Outcome_OUTCOME_KEPT, w.record(unpaid, on(1, 19), 10).GetOutcome())
	w.say("destroy", unpaid, on(1, 19).Add(time.Hour))
	_, err = w.S.Payers.Arrive(ctx, w.product(), unpaid, owner, on(1, 19))
	x.Equal(codes.FailedPrecondition, status.Code(err))

	// Its payer pays no longer, and what was offered is withdrawn.
	k, err := w.S.Ent.Link.Query().Where(entlink.HasWorkspaceWith(entworkspace.Key(ws.Bytes()))).Only(ctx)
	x.NoError(err)
	x.Equal(int32(levyan.LinkEnded_LINK_ENDED_DESTROYED), k.Ended)
	x.True(k.DateEnd.Equal(on(1, 15).Add(6 * time.Hour)))
	o, err := w.S.Ent.Offer.Query().Where(entoffer.HasWorkspaceWith(entworkspace.Key(ws.Bytes()))).Only(ctx)
	x.NoError(err)
	x.Equal(int32(levyan.OfferAnswer_OFFER_ANSWER_WITHDRAWN), o.Answer)

	// Past the grace, the one its payer left is restricted, and the one
	// destroyed is not: it is nobody's to pay for.
	w.At(on(2, 20))
	p, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.ctx, levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Nil(p.GetPayer().GetCurrent())
	x.Nil(p.GetPayer().GetPayerlessSince())
	x.NoError(w.S.Grants.Follow(ctx))
	gs, err := w.S.Ent.Grant.Query().Where(entgrant.HasWorkspaceWith(entworkspace.KeyIn(ws.Bytes(), left.Bytes()))).WithWorkspace().All(ctx)
	x.NoError(err)
	x.Len(gs, 1)
	x.Equal(left.Bytes(), gs[0].Edges.Workspace.Key)
	_, err = w.S.Grants.Give(ctx, grants.Give{
		Product: w.product(), Workspace: ws, Key: "loft.export", Value: entitle.On, Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	})
	x.Equal(codes.FailedPrecondition, status.Code(err), "and given nothing")

	// January: 14 days and 6 hours of 1000, and not the 36 hours its last
	// reading would have held -- (14250/31 - 100) * 50, where 15.5 days
	// would have been 20000.
	w.At(on(2, 10))
	w.close(on(2, 10))
	x.Equal(int64(17983), w.statement(ws, on(1, 1)).GetTotal())
}

// What is said of a workspace is put in the order of its moments, never of
// its coming: the same said again changes nothing, a restore said before the
// close it follows is the same as one said after it, and a close older than
// a restore takes its place without undoing it -- so both are restored, and
// billed as if open all month, where read in the order they came each would
// be closed still. What is not a moment, a moment ahead of now or before the
// workspace arrived, a workspace levyan has heard nothing of, and another
// thing said of a moment kept, are refused.
func TestWhatIsSaidIsReadInTheOrderOfItsMoments(t *testing.T) {
	w := setup(t)
	x := w.X

	owner := levyantest.Account()
	early, older, late := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 10))
	w.arrive(early, owner, on(1, 1))
	w.arrive(older, owner, on(1, 1))
	w.arrive(late, owner, on(1, 10))

	// The restore first, then the close, each twice.
	w.At(on(1, 25))
	x.Nil(w.say("restore", early, on(1, 21)).GetClosedSince(), "a restore of what is open changes nothing yet")
	x.Nil(w.say("restore", early, on(1, 21)).GetClosedSince())
	x.Nil(w.say("close", early, on(1, 11)).GetClosedSince(), "restored since")
	x.Nil(w.say("close", early, on(1, 11)).GetClosedSince())

	// Closed and restored, and then a close older than both: closed from
	// the 5th to the 21st, and open after it still.
	w.say("close", older, on(1, 11))
	w.say("restore", older, on(1, 21))
	x.Nil(w.say("close", older, on(1, 5)).GetClosedSince(), "the restore stands")

	_, err := levyanv1.NewWorkspaceServiceClient(w.conn).Close(w.ctx, levyanv1.CloseRequest_builder{Workspace: late.String()}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "no moment")
	_, _, err = w.said("close", late, on(1, 25).Add(time.Hour))
	x.Equal(codes.InvalidArgument, status.Code(err), "ahead of now")
	_, _, err = w.said("close", late, on(1, 5))
	x.Equal(codes.InvalidArgument, status.Code(err), "before it arrived")
	_, _, err = w.said("close", levyantest.Tenant(), on(1, 20))
	x.Equal(codes.NotFound, status.Code(err), "never arrived")
	_, _, err = w.said("restore", early, on(1, 11))
	x.Equal(codes.FailedPrecondition, status.Code(err), "closed then, and not restored")
	_, err = levyanv1.NewWorkspaceServiceClient(w.conn).Close(w.ctx, levyanv1.CloseRequest_builder{Workspace: "loft", At: timestamppb.New(on(1, 20))}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "no workspace")

	w.At(on(2, 10))
	for _, ws := range []pdid.Id{early, older} {
		w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 1000)
	}
	w.close(on(2, 10))

	// Each as if said in order: closed and restored, so all the month's
	// 1000, (1000 - 100) * 50. Read as they came, the first would be closed
	// from the 11th and the second from the 5th, and billed for ten days and
	// four.
	x.Equal(int64(45000), w.statement(early, on(1, 1)).GetTotal())
	x.Equal(int64(45000), w.statement(older, on(1, 1)).GetTotal())
}

// What a product says of its workspaces is of its own: another product's
// key closes none of them, whatever it names, and what it closes of its own
// is of its own alone.
func TestAnEventOfAnotherProductsWorkspaceIsRefused(t *testing.T) {
	w := setup(t)
	x := w.X
	ctx := context.Background()

	other := w.Product("other")
	key, _ := w.Key(other, "other-prod", nil)
	as := levyantest.As(ctx, key)
	theirs, err := pdid.From(other.GetId())
	x.NoError(err)

	owner := levyantest.Account()
	ws, mine := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 1))
	w.arrive(ws, owner, on(1, 1))
	_, err = w.S.Payers.Arrive(ctx, theirs, mine, owner, on(1, 1))
	x.NoError(err)

	w.At(on(1, 11))
	c := levyanv1.NewWorkspaceServiceClient(w.conn)
	_, err = c.Close(as, levyanv1.CloseRequest_builder{Workspace: ws.String(), At: timestamppb.New(on(1, 11))}.Build())
	x.Equal(codes.NotFound, status.Code(err), "loft's, which the other has heard nothing of")
	_, err = c.Destroy(as, levyanv1.DestroyRequest_builder{Workspace: ws.String(), At: timestamppb.New(on(1, 11))}.Build())
	x.Equal(codes.NotFound, status.Code(err))

	// The same workspace, arrived at the other product too: closing it there
	// closes it there.
	_, err = w.S.Payers.Arrive(ctx, theirs, ws, owner, on(1, 1))
	x.NoError(err)
	res, err := c.Close(as, levyanv1.CloseRequest_builder{Workspace: ws.String(), At: timestamppb.New(on(1, 11))}.Build())
	x.NoError(err)
	x.NotNil(res.GetLifecycle().GetClosedSince())
	got, err := c.GetLifecycle(w.ctx, levyanv1.GetLifecycleRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Nil(got.GetLifecycle().GetClosedSince(), "and open at loft")

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 1000)
	w.close(on(2, 10))
	x.Equal(int64(45000), w.statement(ws, on(1, 1)).GetTotal(), "billed by loft as open all month")
}

// What is said after the periods it reaches into began to close is late:
// what they were billed is not changed, and each is corrected on the next
// statement -- a close said late comes back as less, and a restore said late
// as more: all of the stretch it ends, which reaches back to the close. A
// destruction said late of a moment before that restore takes the stretch
// back again: it ended in the destruction, which is never billed.
func TestAnEventSaidLateIsACorrection(t *testing.T) {
	w := setup(t)
	x := w.X

	owner := levyantest.Account()
	ws := levyantest.Tenant()
	w.At(on(1, 1))
	w.arrive(ws, owner, on(1, 1))

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 1000)
	w.close(on(2, 10))
	x.Equal(int64(45000), w.statement(ws, on(1, 1)).GetTotal(), "open all January, as far as anybody knew")

	// Closed on the 21st, said on February 12th.
	w.At(on(2, 12))
	_, late, err := w.said("close", ws, on(1, 21))
	x.NoError(err)
	x.True(late)
	_, late, err = w.said("close", ws, on(1, 21))
	x.NoError(err)
	x.False(late, "said again, it changes nothing")

	w.At(on(3, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 1), on(3, 1), 1000)
	w.close(on(3, 10))

	// January is as it was billed; February's statement has nothing of
	// February -- closed through it -- and corrects January by what it
	// comes to now, 20 days of 1000, less what it was billed:
	// (20000/31 - 100) * 50 - 45000 = -550000/31.
	x.Equal(int64(45000), w.statement(ws, on(1, 1)).GetTotal())
	feb := w.statement(ws, on(2, 1))
	x.Len(feb.GetLines(), 1)
	x.Equal(levyanv1.LineKind_LINE_KIND_CORRECTION, feb.GetLines()[0].GetKind())
	x.True(feb.GetLines()[0].GetCorrects().GetStart().AsTime().Equal(on(1, 1)))
	x.Equal(int64(-17742), feb.GetTotal())

	// Restored on February 15th, said on March 12th.
	w.At(on(3, 12))
	_, late, err = w.said("restore", ws, on(2, 15))
	x.NoError(err)
	x.True(late)

	w.At(on(4, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(3, 1), on(4, 1), 1000)
	w.close(on(4, 10))

	// March is open all month, 45000; and the stretch closed from January
	// 21st, restored, is billed: January corrected back to all of it,
	// 45000 less the 45000 - 550000/31 it was billed, and February by all
	// of it, (1000 - 100) * 50, less nothing.
	mar := w.statement(ws, on(3, 1))
	x.Equal(map[string]string{
		"loft.assets.active":                  "45000",
		"loft.assets.active corrects 2027-01": "17741.935483",
		"loft.assets.active corrects 2027-02": "45000",
	}, amounts(mar))
	x.Equal(int64(107741), mar.GetTotal())
	x.Equal(int64(-17742), w.statement(ws, on(2, 1)).GetTotal(), "and February's statement is as it was")

	// Destroyed on February 10th, said on April 12th: before the restore,
	// so the stretch closed from January 21st ended in the destruction, and
	// is billed for nothing -- January and February taken back again, and
	// March has nothing of it.
	w.At(on(4, 12))
	_, late, err = w.said("destroy", ws, on(2, 10))
	x.NoError(err)
	x.True(late, "reaching back to the close it takes back")

	w.At(on(5, 10))
	w.close(on(5, 10))
	apr := w.statement(ws, on(4, 1))
	x.Equal(map[string]string{
		"loft.assets.active corrects 2027-01": "-17741.935483",
		"loft.assets.active corrects 2027-02": "-45000",
		"loft.assets.active corrects 2027-03": "-45000",
	}, amounts(apr))
	x.Equal(int64(-107742), apr.GetTotal())
}

// A destruction said late, of a moment before a restore said already, ends
// the stretch the restore had ended: what it was closed for is billed for
// nothing again. It reaches back to the close of that stretch, though the
// close itself came on time, so each period from there that was billed with
// the restore is corrected back on the next statement.
func TestADestructionSaidBeforeARestoreTakesItsStretchBack(t *testing.T) {
	w := setup(t)
	x := w.X

	owner := levyantest.Account()
	ws := levyantest.Tenant()
	w.At(on(1, 1))
	w.arrive(ws, owner, on(1, 1))
	w.At(on(1, 21))
	w.say("close", ws, on(1, 21))

	// January: 20 days of 1000, (20000/31 - 100) * 50 = 845000/31.
	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 1000)
	w.close(on(2, 10))
	x.Equal(int64(27258), w.statement(ws, on(1, 1)).GetTotal())

	// Restored on the 15th, said at once: January corrected to all of it.
	w.At(on(2, 15))
	_, late, err := w.said("restore", ws, on(2, 15))
	x.NoError(err)
	x.True(late)
	w.At(on(3, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 1), on(3, 1), 1000)
	w.close(on(3, 10))
	x.Equal(map[string]string{
		"loft.assets.active":                  "45000",
		"loft.assets.active corrects 2027-01": "17741.935483",
	}, amounts(w.statement(ws, on(2, 1))))

	// Destroyed on February 12th, said on March 12th: closed from January
	// 21st to its destruction, and so billed for none of it -- January back
	// to its 20 days, and February to nothing.
	w.At(on(3, 12))
	_, late, err = w.said("destroy", ws, on(2, 12))
	x.NoError(err)
	x.True(late, "reaching back to the close of the stretch the restore had ended")
	w.At(on(4, 10))
	w.close(on(4, 10))
	mar := w.statement(ws, on(3, 1))
	x.Equal(map[string]string{
		"loft.assets.active corrects 2027-01": "-17741.935483",
		"loft.assets.active corrects 2027-02": "-45000",
	}, amounts(mar))
	x.Equal(int64(-62742), mar.GetTotal())
	x.Equal(int64(27258), w.statement(ws, on(1, 1)).GetTotal(), "and what was written is as it was")
}
