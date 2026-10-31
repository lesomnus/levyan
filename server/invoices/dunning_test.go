package invoices_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
	"github.com/lesomnus/levyan/server/payers"
)

const day = 24 * time.Hour

var (
	uncollectible = int32(levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE)
	failedState   = int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)
)

// dun is a pass of the loop `levyan.invoices.dunning`: what the table says is
// due made and sent, what is spent given up on, and the rule's restrictions
// followed.
func (w world) dun() {
	w.t.Helper()
	w.X.NoError(w.S.Invoices.Dun(w.ctx, &w.S.Grants))
}

// january is January used by `ws` and closed on February 9th, and the loop
// run: each invoice finalized, and its first charge sent -- answered as the
// channel's script says. It answers when that was: what the table measures
// from.
func (w world) january(ws ...pdid.Id) time.Time {
	w.t.Helper()
	w.At(on(2, 9))
	for _, v := range ws {
		w.daily(v, on(1, 1), on(2, 1), 200)
	}
	w.close(on(2, 9))
	w.run()
	return on(2, 9)
}

func (w world) charges(inv *ent.Invoice) []*ent.PaymentAttempt {
	w.t.Helper()
	var out []*ent.PaymentAttempt
	for _, a := range w.attempts(inv) {
		if a.Kind == int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE) {
			out = append(out, a)
		}
	}
	return out
}

// restricted says whether workspace `ws` of the product is restricted at the
// deployment's now as the product's copy decides it (`entitle.Decide`): by
// any grant of `loft.restricted` in force, whichever rule's -- and which
// rules' grants are in force.
func (w world) restricted(ws pdid.Id) (bool, []string) {
	w.t.Helper()
	row, ok, err := payers.Workspace(w.ctx, w.S.Ent, w.product.Uuid(), ws)
	w.X.NoError(err)
	if !ok {
		return false, nil
	}
	vs, err := w.S.Ent.Grant.Query().Where(entgrant.WorkspaceId(row), entgrant.Key("loft.restricted"), entgrant.DateErasedIsNil()).All(w.ctx)
	w.X.NoError(err)
	now := w.S.Clock()
	var rows []entitle.Row
	var by []string
	for _, v := range vs {
		r := entitle.Row{Key: v.Key, Value: grants.Value(v.Proto()), From: v.ValidFrom, Until: v.ValidUntil, Grant: v.Id[:]}
		rows = append(rows, r)
		if r.In(now) {
			by = append(by, v.Rule)
		}
	}

	return entitle.Decide("loft.restricted", rows, now).Value.Has(), by
}

// Each kind of decline is dunned as its row of the table says, by default
// (Claude's, provisional): a temporary one, and one that says no more, tried
// again a day, three days and five days after it failed -- each once,
// however often the loop runs -- restricted and given up on after a week; a
// method to replace never tried again, restricted and given up on after a
// week; suspected fraud restricted, and given up on, at once.
func TestEachKindIsDunnedAsItsRowSays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decline invoicestest.Answer
		kind    levyan.DeclineKind
		retries []time.Duration
		grace   time.Duration
	}{
		{"temporary", invoicestest.Decline, levyan.DeclineKind_DECLINE_KIND_TEMPORARY, []time.Duration{day, 3 * day, 5 * day}, 7 * day},
		{"other", invoicestest.DeclineOther, levyan.DeclineKind_DECLINE_KIND_UNSPECIFIED, []time.Duration{day, 3 * day, 5 * day}, 7 * day},
		{"replace", invoicestest.DeclineReplace, levyan.DeclineKind_DECLINE_KIND_REPLACE, nil, 7 * day},
		{"fraud", invoicestest.DeclineFraud, levyan.DeclineKind_DECLINE_KIND_FRAUD, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := setup(t, mor)
			x := w.X

			ws := levyantest.Tenant()
			_, ba := w.payer(ws)
			for range 10 {
				w.ch.Then(tc.decline)
			}
			t0 := w.january(ws)

			inv := w.only(ba)
			x.Equal(open, inv.State)
			cs := w.charges(inv)
			x.Len(cs, 1)
			x.Equal(failedState, cs[0].State)
			x.Equal(tc.kind, cs[0].Failure.GetDecline())

			// Through every moment the row names, the loop running twice at
			// each: a retry is made once its time has come, and once.
			n := 1
			for _, at := range []time.Duration{0, day - time.Minute, day, 2 * day, 3 * day, 4 * day, 5 * day, 6 * day} {
				w.At(t0.Add(at))
				w.dun()
				w.dun()
				for _, r := range tc.retries {
					if r == at {
						n++
					}
				}
				cs := w.charges(w.again(inv))
				x.Len(cs, n, "%s after", at)
				for _, c := range cs {
					x.Equal(failedState, c.State, "declined, each")
				}
				restricted, _ := w.restricted(ws)
				if at < tc.grace {
					x.Equal(open, w.again(inv).State, "%s after: within its grace", at)
					x.False(restricted, "%s after: within its grace", at)
				} else {
					x.Equal(uncollectible, w.again(inv).State, "%s after: no grace, and no retry", at)
					x.True(restricted, "%s after: at once", at)
				}
			}
			x.Len(w.ch.Sends(), n, "each retry a charge of its own, sent once")
			keys := map[string]bool{}
			for _, s := range w.ch.Sends() {
				keys[s.Attempt.Key] = true
			}
			x.Len(keys, n, "each with a key of its own")

			w.At(t0.Add(max(tc.grace, 6*day+time.Hour)))
			w.dun()
			inv = w.again(inv)
			x.Equal(uncollectible, inv.State, "no retry left, and the grace over")
			x.Equal(inv.Due, invoices.Remaining(inv), "still owed")
			restricted, by := w.restricted(ws)
			x.True(restricted)
			x.Equal([]string{invoices.RestrictionRule}, by)
			x.Len(w.charges(inv), n, "nothing more is tried")
		})
	}
}

// A pass of the loop decides each invoice again on the transaction that
// holds its billing account's row: passes run at once make one retry of a
// retry's time, not one each.
func TestARetryIsMadeOnceHoweverManyPassesRunAtOnce(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.ch.Then(invoicestest.Decline, invoicestest.Hold)
	t0 := w.january(ws)

	w.At(t0.Add(day))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = w.S.Invoices.Dun(w.ctx, &w.S.Grants) })
	}
	wg.Wait()
	for _, err := range errs {
		x.NoError(err)
	}
	cs := w.charges(w.only(ba))
	x.Len(cs, 2, "the first, and one retry")
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_PENDING), cs[1].State, "sent once, and held by the channel")
	x.Len(w.ch.Sends(), 2)
}

// A workspace two rules restrict -- it has had no payer for longer than its
// grace, and an invoice that bills it is unpaid past its own -- stays
// restricted until both let it go: each rule's grant is its own, of the one
// key, and the product's copy reads it restricted while either is in force.
// Here the invoice is paid by a retry first, and then somebody pays for the
// workspace. And a row of the table is what the deployment says.
func TestAWorkspaceTwoRulesRestrictIsRestrictedUntilBothLetItGo(t *testing.T) {
	retries := cmd.Retries{cmd.Span(day), cmd.Span(12 * day)}
	w := setup(t, mor, func(c *cmd.Config) { c.Dunning.Kinds.Temporary.Retries = &retries })
	x := w.X

	ws := levyantest.Tenant()
	a, ba := w.payer(ws)
	w.At(on(1, 20))
	_, err := w.S.Payers.Leave(w.ctx, w.product, ws, a)
	x.NoError(err, "nobody pays for it from January 20th, and its January is the one who left")
	w.ch.Then(invoicestest.Decline, invoicestest.Decline)
	t0 := w.january(ws)
	inv := w.only(ba)

	// February 16th: the invoice's grace is over.
	w.At(t0.Add(7 * day))
	w.dun()
	x.NoError(w.S.Grants.Pass(w.ctx))
	restricted, by := w.restricted(ws)
	x.True(restricted)
	x.Equal([]string{invoices.RestrictionRule}, by)
	x.Equal(open, w.again(inv).State, "a retry is left, on the 21st")

	// February 20th: and thirty days without a payer.
	w.At(on(2, 20))
	w.dun()
	x.NoError(w.S.Grants.Pass(w.ctx))
	restricted, by = w.restricted(ws)
	x.True(restricted)
	x.ElementsMatch([]string{invoices.RestrictionRule, payers.RuleName}, by, "two grants of one key, each rule's own")

	// February 21st: the retry the deployment's row schedules goes through.
	w.At(t0.Add(12 * day))
	w.dun()
	x.Equal(paid, w.again(inv).State)
	restricted, by = w.restricted(ws)
	x.True(restricted, "the invoice's let go; having no payer still restricts it")
	x.Equal([]string{payers.RuleName}, by)

	// Somebody pays for it.
	b := levyantest.Account()
	o, err := w.S.Payers.Offer(w.ctx, w.product, ws, b)
	x.NoError(err)
	oId, err := pdid.From(o.GetId())
	x.NoError(err)
	_, err = w.S.Payers.Accept(w.ctx, w.product, b, oId, pdid.Nil)
	x.NoError(err)
	x.NoError(w.S.Grants.Pass(w.ctx))
	restricted, by = w.restricted(ws)
	x.False(restricted, "let go by both")
	x.Empty(by)
}

// What an invoice's charge failing does is of its product alone: a billing
// account paying for a workspace of each of two products, declined in one,
// restricts that one's workspace by that one's key, and nothing of the
// other's; a payer reading the billing account through the other product,
// and the other product reading its workspace, are told of no dunning.
func TestDunningIsOfOneProductAlone(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	kamino := w.Terms(w.Product("kamino"), "UTC", mor, "KRW")
	key, _ := w.Key(kamino, "kamino-prod", nil)
	m := w.Meter(kamino, "kamino.assets.active", "{asset}", stock, twa)
	w.Price(kamino, m, mor, "KRW", 100, 50, on(1, 1))
	k, err := pdid.From(kamino.GetId())
	x.NoError(err)
	as := levyantest.As(w.ctx, key)

	ours, theirs := levyantest.Tenant(), levyantest.Tenant()
	account, ba := w.payer(ours)
	_, err = w.S.Payers.Arrive(w.ctx, k, theirs, account, on(1, 1))
	x.NoError(err)
	w.At(on(2, 9))
	rs := []*levyanv1.Reading{}
	for d := on(1, 1); d.Before(on(2, 1)); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: theirs.String(), Meter: "kamino.assets.active", MeasuredAt: timestamppb.New(d), Value: 200, Unit: "{asset}"}.Build())
	}
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(as, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	x.NoError(err)
	for _, r := range res.GetResults() {
		x.Equal(levyanv1.Outcome_OUTCOME_KEPT, r.GetOutcome(), r.GetWhy())
	}
	p, err := w.S.Ent.Tenant.Get(w.ctx, k.Uuid())
	x.NoError(err)
	_, err = w.S.Closer.Close(w.ctx, p, on(2, 9))
	x.NoError(err)

	// Kamino's charge, paid; then loft's, declined as fraud.
	w.ch.Then(invoicestest.Pay)
	w.run()
	w.ch.Then(invoicestest.DeclineFraud)
	w.january(ours)
	w.dun()

	invs := w.invoices(ba)
	x.Len(invs, 2)
	for _, inv := range invs {
		if inv.TenantId == w.product.Uuid() {
			x.Equal(uncollectible, inv.State)
		} else {
			x.Equal(paid, inv.State)
		}
	}
	restricted, _ := w.restricted(ours)
	x.True(restricted)
	vs, err := w.S.Ent.Grant.Query().Where(entgrant.TenantId(k.Uuid())).All(w.ctx)
	x.NoError(err)
	x.Empty(vs, "nothing of kamino's is restricted")

	// What each product, and the payer through each, is told.
	payer := levyanv1.NewPayerServiceClient(w.conn)
	got, err := payer.GetBillingAccount(levyantest.AsAccount(w.ctx, w.key, account), levyanv1.GetBillingAccountRequest_builder{Id: ba.String()}.Build())
	x.NoError(err)
	x.Len(got.GetDunning(), 1, "through loft")
	x.Equal(levyanv1.DeclineKind_DECLINE_KIND_FRAUD, got.GetDunning()[0].GetKind())
	got, err = payer.GetBillingAccount(levyantest.AsAccount(w.ctx, key, account), levyanv1.GetBillingAccountRequest_builder{Id: ba.String()}.Build())
	x.NoError(err)
	x.Empty(got.GetDunning(), "through kamino, which bills it nothing unpaid")

	pw, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(as, levyanv1.GetPayerRequest_builder{Workspace: theirs.String()}.Build())
	x.NoError(err)
	x.Empty(pw.GetPayer().GetDunning())
	pw, err = levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.as, levyanv1.GetPayerRequest_builder{Workspace: ours.String()}.Build())
	x.NoError(err)
	x.Len(pw.GetPayer().GetDunning(), 1)
	pw, err = levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(as, levyanv1.GetPayerRequest_builder{Workspace: ours.String()}.Build())
	x.NoError(err, "a workspace kamino never heard of is one with no payer")
	x.Empty(pw.GetPayer().GetDunning())
}

// Dunning is a loop of its own name, which a lease holds it by, and `levyan
// serve` runs it.
func TestDunningIsALoopOfItsOwnName(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	n, ok := invoices.Duns(&w.S.Invoices, &w.S.Grants, time.Minute).(spin.Named)
	x.True(ok)
	x.Equal("levyan.invoices.dunning", n.SpinName())

	wired := false
	for _, s := range w.S.Spin {
		if n, ok := s.(spin.Named); ok && n.SpinName() == invoices.DunLoop {
			wired = true
		}
	}
	x.True(wired)

	rules := []string{}
	for _, r := range w.S.Grants.Rules {
		rules = append(rules, r.Name())
	}
	x.Equal([]string{payers.RuleName, invoices.RestrictionRule}, rules, "the grants' loop follows both rules too")
}
