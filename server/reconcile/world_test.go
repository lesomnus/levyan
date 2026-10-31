package reconcile_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entfinding "github.com/lesomnus/levyan/internal/ent/finding"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/reconcile"
)

var (
	morCh = levyan.Channel_CHANNEL_MOR

	paid = int32(levyan.InvoiceState_INVOICE_STATE_PAID)

	requested = int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED)
	unknown   = int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN)
	pending   = int32(levyan.AttemptState_ATTEMPT_STATE_PENDING)
	succeeded = int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED)
	failed    = int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)

	open     = int32(levyan.FindingState_FINDING_STATE_OPEN)
	accepted = int32(levyan.FindingState_FINDING_STATE_ACCEPTED)
	resolved = int32(levyan.FindingState_FINDING_STATE_RESOLVED)
)

const back = "https://loft.example.com/billing/subscribed"

// on is a day of 2027 at midnight UTC.
func on(month, d int) time.Time { return time.Date(2027, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

func krw(n int64) money.Amount { return money.Amount{Minor: n, Currency: "KRW"} }

// world is loft, sold through the fake Paddle in won -- active assets at 50
// won each past 100, from 2027 -- with collection on the merchant's channel
// beginning on February 1st: built as `levyan serve` builds one, its
// collector, its inbox and its reconciler wired, and the deployment on the
// fake's clock, which [world.to] moves. A pass asks the fake as fast as it
// answers.
type world struct {
	*levyantest.World
	t       *testing.T
	ctx     context.Context
	srv     *paddletest.Server
	product pdid.Id
	conn    *grpc.ClientConn
	as      context.Context

	// op is a context framed as an operator, alice, by her row: what the
	// operator's entry point frames her requests as.
	op context.Context
}

func setup(t *testing.T, with ...func(*cmd.Config)) *world {
	t.Helper()
	srv := paddletest.New(t)
	w := levyantest.New(t, append([]func(*cmd.Config){levyantest.Merchant(srv, true), levyantest.LiveFrom(on(2, 1))}, with...)...)
	return arrange(t, w, srv)
}

// bare is loft on a levyan that names no merchant of record: its billing
// accounts are on the merchant's channel all the same, which nothing
// collects on.
func bare(t *testing.T) *world {
	t.Helper()
	srv := paddletest.New(t)
	return arrange(t, levyantest.New(t), srv)
}

func arrange(t *testing.T, w *levyantest.World, srv *paddletest.Server) *world {
	t.Helper()
	w.S.Clock = srv.Now
	w.S.Reconciler.Pace = 0
	x := &world{World: w, t: t, ctx: context.Background(), srv: srv}
	x.to(on(1, 1).Add(-24 * time.Hour))

	loft := w.Terms(w.Product("loft"), "UTC", morCh, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE)
	w.Price(loft, assets, morCh, "KRW", 100, 50, on(1, 1))
	p, err := pdid.From(loft.GetId())
	w.X.NoError(err)
	x.product = p
	x.conn = w.Serve()
	x.as = levyantest.As(x.ctx, key)

	alice, err := w.S.Ungated.Operator().Add(x.ctx, levyan.OperatorAddRequest_builder{Alias: "alice"}.Build())
	w.X.NoError(err)
	id, err := pdid.From(alice.GetId())
	w.X.NoError(err)
	x.op = frame.Into(x.ctx, frame.New(id, pdid.Nil, frame.Whole()).WithRow(alice))

	return x
}

// to is the deployment, and the merchant, at `at`.
func (w *world) to(at time.Time) { w.srv.Advance(at.Sub(w.srv.Now())) }

// payer is an account of roster's with a billing account in Korea -- on the
// merchant's channel, in won -- paying for the workspaces it made on New
// Year's day.
func (w *world) payer(ws ...pdid.Id) (pdid.Id, pdid.Id) {
	w.t.Helper()
	account := levyantest.Account()
	v, err := w.S.Payers.Create(w.ctx, account, levyan.BillingAccount_builder{
		Name: "회사용", Country: "KR", Recipient: "수민", Email: "billing@example.com",
	}.Build())
	w.X.NoError(err)
	ba, err := pdid.From(v.GetId())
	w.X.NoError(err)
	if w.srv.Now().Before(on(1, 1)) {
		w.to(on(1, 1))
	}
	for _, v := range ws {
		_, err := w.S.Payers.Arrive(w.ctx, w.product, v, account, on(1, 1))
		w.X.NoError(err)
	}

	return account, ba
}

// subscribe is billing account `ba` of `account`'s subscribed in the
// merchant's checkout, which its payer finishes: the mirror.
func (w *world) subscribe(account, ba pdid.Id) *ent.Subscription {
	w.t.Helper()
	co, win, err := w.S.Subscriptions.Start(w.ctx, account, ba, back)
	w.X.NoError(err)
	w.X.NoError(w.srv.Pay(mor.Checkout{ID: co.MorTransaction, Window: win}))
	_, sub, err := w.S.Subscriptions.Complete(w.ctx, account, pdid.Id(co.Id))
	w.X.NoError(err)
	w.X.NotNil(sub)
	return sub
}

// daily records a reading of active assets for `ws` at each midnight of
// [from, to), of `v`.
func (w *world) daily(ws pdid.Id, from, to time.Time, v int64) {
	w.t.Helper()
	rs := []*levyanv1.Reading{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(d), Value: v, Unit: "{asset}"}.Build())
	}
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.as, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	w.X.NoError(err)
	for _, r := range res.GetResults() {
		w.X.Equal(levyanv1.Outcome_OUTCOME_KEPT, r.GetOutcome(), r.GetWhy())
	}
}

// close closes what has ended by `through`, as an operator's hand does.
func (w *world) close(through time.Time) {
	w.t.Helper()
	p, err := w.S.Ent.Tenant.Get(w.ctx, w.product.Uuid())
	w.X.NoError(err)
	_, err = w.S.Closer.Close(w.ctx, p, through)
	w.X.NoError(err)
}

// run is a pass of the loop `levyan.invoices.collect`.
func (w *world) run() {
	w.t.Helper()
	w.X.NoError(w.S.Invoices.Run(w.ctx))
}

// month is the month that began at `from` used and closed: the deployment
// nine days past its end, `ws` holding 200 assets each day of it -- 5,000
// won of each -- and the month closed.
func (w *world) month(from time.Time, ws ...pdid.Id) {
	w.t.Helper()
	to := from.AddDate(0, 1, 0)
	w.to(to.AddDate(0, 0, 9))
	for _, v := range ws {
		w.daily(v, from, to, 200)
	}
	w.close(to.AddDate(0, 0, 9))
}

// charged is January of a billing account subscribed with the merchant,
// closed, invoiced and charged -- and paid, as the send answered: the
// invoice, its attempt and the mirror.
func (w *world) charged() (*ent.Invoice, *ent.PaymentAttempt, *ent.Subscription) {
	w.t.Helper()
	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.run()
	inv := w.only(ba)
	w.X.Equal(paid, inv.State)
	a := w.charges(inv)[0]
	w.X.Equal(succeeded, a.State)
	return inv, a, sub
}

func (w *world) only(ba pdid.Id) *ent.Invoice {
	w.t.Helper()
	vs, err := w.S.Ent.Invoice.Query().
		Where(entinvoice.BillingAccountId(ba.Uuid())).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	w.X.Len(vs, 1)
	return vs[0]
}

func (w *world) again(inv *ent.Invoice) *ent.Invoice {
	w.t.Helper()
	v, err := w.S.Ent.Invoice.Get(w.ctx, inv.Id)
	w.X.NoError(err)
	return v
}

func (w *world) attempt(a *ent.PaymentAttempt) *ent.PaymentAttempt {
	w.t.Helper()
	v, err := w.S.Ent.PaymentAttempt.Get(w.ctx, a.Id)
	w.X.NoError(err)
	return v
}

func (w *world) attempts(inv *ent.Invoice, kind levyan.AttemptKind) []*ent.PaymentAttempt {
	w.t.Helper()
	vs, err := w.S.Ent.PaymentAttempt.Query().
		Where(entattempt.InvoiceId(inv.Id), entattempt.Kind(int32(kind))).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	return vs
}

func (w *world) charges(inv *ent.Invoice) []*ent.PaymentAttempt {
	return w.attempts(inv, levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
}

func (w *world) refunds(inv *ent.Invoice) []*ent.PaymentAttempt {
	return w.attempts(inv, levyan.AttemptKind_ATTEMPT_KIND_REFUND)
}

// heard keeps the webhook the fake sends of `event`, of entity `id`, as the
// endpoint does.
func (w *world) heard(event, id string) {
	w.t.Helper()
	h, body := w.srv.Webhook(event, id)
	n, err := w.S.Inbox.Channel.Webhook(h, body)
	w.X.NoError(err)
	_, err = w.S.Inbox.Keep(w.ctx, n, levyan.NoticeVia_NOTICE_VIA_WEBHOOK)
	w.X.NoError(err)
}

// take takes the inbox as `levyan serve`'s loop does, until nothing waits in
// it that is due.
func (w *world) take() {
	w.t.Helper()
	for {
		n, err := w.S.Taker.Take(w.ctx)
		w.X.NoError(err)
		if n == 0 {
			return
		}
	}
}

// reconcile is a pass of the loop `levyan.reconcile`, as the deployment.
func (w *world) reconcile() reconcile.Report {
	w.t.Helper()
	rep, err := w.S.Reconciler.Run(w.ctx)
	w.X.NoError(err)
	for _, c := range rep.Checks {
		w.X.Empty(c.Err, "check %s", c.Name)
	}
	return rep
}

// findings is every finding that is so -- open or accepted -- by its kind's
// short name and its subject.
func (w *world) findings() map[string]*ent.Finding {
	w.t.Helper()
	vs, err := w.S.Ent.Finding.Query().
		Where(entfinding.StateIn(open, accepted)).
		All(w.ctx)
	w.X.NoError(err)
	out := map[string]*ent.Finding{}
	for _, v := range vs {
		out[short(v.Kind)+" "+v.Subject] = v
	}
	return out
}

// of is the findings of kind `k` that are so.
func (w *world) of(k levyan.FindingKind) []*ent.Finding {
	w.t.Helper()
	vs, err := w.S.Ent.Finding.Query().
		Where(entfinding.Kind(k.String()), entfinding.StateIn(open, accepted)).
		Order(ent.Asc(entfinding.FieldDateCreated), ent.Asc(entfinding.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	return vs
}

// finding is finding `f` as it is now.
func (w *world) finding(f *ent.Finding) *ent.Finding {
	w.t.Helper()
	v, err := w.S.Ent.Finding.Get(w.ctx, f.Id)
	w.X.NoError(err)
	return v
}

// nothing says that nothing is so: no finding is open or accepted.
func (w *world) nothing() {
	w.t.Helper()
	got := w.findings()
	for k, v := range got {
		w.t.Logf("%s: %s", k, v.Summary)
	}
	w.X.Empty(got)
}

func short(name string) string {
	const p = "FINDING_KIND_"
	if len(name) > len(p) {
		name = name[len(p):]
	}
	b := []byte(name)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
