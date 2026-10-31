package merchant_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/merchant"
)

var (
	morCh = levyan.Channel_CHANNEL_MOR

	draft  = int32(levyan.InvoiceState_INVOICE_STATE_DRAFT)
	open   = int32(levyan.InvoiceState_INVOICE_STATE_OPEN)
	paid   = int32(levyan.InvoiceState_INVOICE_STATE_PAID)
	shadow = int32(levyan.InvoiceState_INVOICE_STATE_SHADOW)

	requested = int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED)
	unknown   = int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN)
	pending   = int32(levyan.AttemptState_ATTEMPT_STATE_PENDING)
	succeeded = int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED)
	failedAt  = int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)
)

const back = "https://loft.example.com/billing/subscribed"

// on is a day of 2027 at midnight UTC.
func on(month, d int) time.Time { return time.Date(2027, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

func krw(n int64) money.Amount { return money.Amount{Minor: n, Currency: "KRW"} }

type world struct {
	*levyantest.World
	t       *testing.T
	ctx     context.Context
	srv     *paddletest.Server
	product pdid.Id
	conn    *grpc.ClientConn
	key     string
	as      context.Context
}

// setup is loft, sold through the fake Paddle in won -- active assets at 50
// won each past 100, from 2027 -- with collection on the merchant's channel
// beginning at `from`: built as `levyan serve` builds one, its collector and
// its inbox wired, and the deployment on the fake's clock, which [world.to]
// moves.
func setup(t *testing.T, from time.Time, with ...func(*cmd.Config)) *world {
	t.Helper()
	srv := paddletest.New(t)
	w := levyantest.New(t, append([]func(*cmd.Config){levyantest.Merchant(srv, true), levyantest.LiveFrom(from)}, with...)...)
	return arrange(t, w, srv)
}

// arrange is loft put up on `w`, whose clock is the fake's.
func arrange(t *testing.T, w *levyantest.World, srv *paddletest.Server) *world {
	t.Helper()
	w.S.Clock = srv.Now
	x := &world{World: w, t: t, ctx: context.Background(), srv: srv}
	x.to(on(1, 1).Add(-24 * time.Hour))

	loft := w.Terms(w.Product("loft"), "UTC", morCh, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE)
	w.Price(loft, assets, morCh, "KRW", 100, 50, on(1, 1))
	p, err := pdid.From(loft.GetId())
	w.X.NoError(err)
	x.product, x.key = p, key
	x.conn = w.Serve()
	x.as = levyantest.As(x.ctx, key)

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
	w.X.Equal(morCh.String(), v.GetChannel())
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
// nine days past its end, `ws` holding 200 assets each day of it, and the
// month closed.
func (w *world) month(from time.Time, ws ...pdid.Id) {
	w.t.Helper()
	to := from.AddDate(0, 1, 0)
	w.to(to.AddDate(0, 0, 9))
	for _, v := range ws {
		w.daily(v, from, to, 200)
	}
	w.close(to.AddDate(0, 0, 9))
}

func (w *world) invoices(ba pdid.Id) []*ent.Invoice {
	w.t.Helper()
	vs, err := w.S.Ent.Invoice.Query().
		Where(entinvoice.BillingAccountId(ba.Uuid())).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	return vs
}

func (w *world) only(ba pdid.Id) *ent.Invoice {
	w.t.Helper()
	vs := w.invoices(ba)
	w.X.Len(vs, 1)
	return vs[0]
}

func (w *world) again(inv *ent.Invoice) *ent.Invoice {
	w.t.Helper()
	v, err := w.S.Ent.Invoice.Get(w.ctx, inv.Id)
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

// charged is what the merchant has of attempt `a`'s charge in subscription
// `sub`: the transaction, and ErrTwice of a key it has more than once.
func (w *world) charged(sub *ent.Subscription, a *ent.PaymentAttempt) (mor.Transaction, error) {
	return w.S.Subscriptions.Channel.Charged(w.ctx, mor.Charge{
		Subscription: sub.MorSubscription, Key: a.AttemptKey, When: mor.Now, Amount: krw(a.Amount),
	})
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

// A period that ended after collection began is invoiced and charged to the
// payer's subscription with the merchant at once: under the attempt's key,
// for what is due, as the invoice's description -- and the invoice is paid,
// with the merchant's references of it: its transaction, and the number of
// the invoice the merchant, the seller, issued. Nothing is charged twice,
// however often the loop runs.
func TestAPeriodsAmountIsChargedToTheSubscription(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.run()
	w.run()

	inv := w.only(ba)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Due, "100 past what is included, at 50")
	x.Zero(inv.Tax, "the merchant of record adds its own")
	x.Nil(inv.Number, "and numbers what it sells")
	x.Nil(inv.DateDue, "collected as it is finalized")
	x.Contains(inv.ChannelRef, "txn_")
	x.Regexp(`^325-\d{5}$`, inv.ChannelNumber, "the merchant's invoice number")

	as := w.charges(inv)
	x.Len(as, 1)
	a := as[0]
	x.Equal(succeeded, a.State)
	x.Equal(uint32(1), a.Sends)
	x.Equal(inv.ChannelRef, a.ChannelRef)
	x.Equal(inv.ChannelNumber, a.ChannelNumber)
	x.Nil(a.DateTwice)

	txn, err := w.charged(sub, a)
	x.NoError(err, "the key, once")
	x.Equal(mor.Paid, txn.Status)
	x.Equal(a.ChannelRef, txn.ID)
	l, ok := txn.Line(a.AttemptKey)
	x.True(ok)
	x.Equal(krw(5000), l.Amount, "what levyan computed: the merchant's tax goes on top")
	x.Equal("loft 2027-01", l.Name)
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}

// Scenario 5, the charge made and its answer lost: the attempt is out, and
// the loop's next pass asks the merchant, finds the charge by its key and is
// paid by it -- and sends nothing again.
func TestAChargeWhoseAnswerWasLostIsPaidByAsking(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)

	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(ba)
	x.Equal(open, inv.State)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State, "out, as far as anybody can tell")
	x.Equal(uint32(1), a.Sends)

	w.to(on(2, 10).Add(time.Minute))
	w.run()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	a = w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	x.Equal(uint32(1), a.Sends, "asked, never sent again")
	x.NotEmpty(inv.ChannelNumber)
	_, err := w.charged(sub, a)
	x.NoError(err, "charged once")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}

// What the merchant notices of a charge lands as soon as the inbox takes it,
// before the loop's next pass: the transaction its webhook names, asked of
// the merchant again, settles the attempt whose key it carries and pays the
// invoice. Taken twice, it is paid once; an answer from before, come back
// late, changes nothing.
func TestAChargesNoticeSettlesItAtOnce(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X
	x.NotNil(w.S.Taker.Transaction, "the inbox hands each transaction to the collector")

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.take()

	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State)
	txn, err := w.charged(sub, a)
	x.NoError(err)

	w.heard("transaction.completed", txn.ID)
	w.take()
	inv = w.again(inv)
	x.Equal(paid, inv.State, "paid as the notice was taken")
	x.Equal(int64(5000), inv.Paid)
	x.Equal(txn.ID, inv.ChannelRef)
	x.Equal(txn.InvoiceNumber, inv.ChannelNumber)
	a = w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	x.Equal(txn.ID, a.ChannelRef)

	lists := w.srv.Requests(paddletest.List)
	w.run()
	x.Equal(lists, w.srv.Requests(paddletest.List), "nothing left to ask about")

	// Taken again, by another notice of the same transaction: paid once.
	w.heard("transaction.updated", txn.ID)
	w.take()
	again := w.again(inv)
	x.Equal(int64(5000), again.Paid)
	x.Equal(inv.DateUpdated, again.DateUpdated, "nothing written")
	x.Zero(w.balance(ba), "nothing paid past what was due")

	// An answer from before it was paid, come back late: nothing.
	old := mor.Transaction{ID: txn.ID, Subscription: txn.Subscription, Status: mor.Pending, Lines: txn.Lines}
	x.NoError(w.S.Taker.Transaction(w.ctx, nil, old))
	x.Equal(succeeded, w.charges(inv)[0].State)
	x.Equal(inv.DateUpdated, w.again(inv).DateUpdated)
}

func (w *world) balance(ba pdid.Id) int64 {
	w.t.Helper()
	v, err := invoices.Balance(w.ctx, w.S.Ent, w.product.Uuid(), ba.Uuid())
	w.X.NoError(err)
	return v
}

// Scenario 5 at its worst: the charge made, its answer lost, and the
// merchant not showing it for a while. What the merchant does not show yet is
// not believed absent until `mor.Settle` has passed since it was sent -- the
// attempt stays out, and nothing is sent again -- and it is then found, and
// paid by, once.
func TestALostAnswerIsAskedAfterItSettlesAndPaidOnce(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	sent := w.srv.Now()

	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true, Unseen: 3 * time.Minute})
	w.run()
	inv := w.only(ba)
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute} {
		w.to(sent.Add(d))
		w.run()
		a := w.charges(inv)[0]
		x.Equal(unknown, a.State, "%s after: not seen is not not made", d)
		x.Equal(uint32(1), a.Sends)
	}
	x.Equal(1, w.srv.Requests(paddletest.Charge), "nothing sent while it may be on its way")

	w.to(sent.Add(mor.Settle + time.Minute))
	w.run()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	a := w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	_, err := w.charged(sub, a)
	x.NoError(err, "paid exactly once")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}

// A send that never reached the merchant is out all the same until it has
// settled, and only once the merchant can say it never had it is it sent
// again: the same attempt and key, and a send of its own told when the one
// before was -- and charged once.
func TestASendThatNeverArrivedIsSentAgainOnceItHasSettled(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	sent := w.srv.Now()

	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 502, Code: "bad_gateway"})
	w.run()
	inv := w.only(ba)
	w.to(sent.Add(time.Minute))
	w.run()
	x.Equal(unknown, w.charges(inv)[0].State, "too soon to say it was never made")

	w.to(sent.Add(mor.Settle + time.Minute))
	w.run()
	a := w.charges(inv)[0]
	x.Equal(requested, a.State, "the merchant never had it")
	x.Equal(1, w.srv.Requests(paddletest.Charge))

	w.to(sent.Add(mor.Settle + 2*time.Minute))
	w.run()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	as := w.charges(inv)
	x.Len(as, 1, "the same attempt")
	x.Equal(uint32(2), as[0].Sends)
	x.Equal(2, w.srv.Requests(paddletest.Charge))
	_, err := w.charged(sub, as[0])
	x.NoError(err, "charged once")
}

// A charge sent to a subscription the payer canceled at the merchant since,
// and subscribed again -- its answer lost on the way -- is asked of every
// subscription the billing account has had: found in the one it charged,
// and never charged again to the new one.
func TestAChargeIsAskedOfEverySubscriptionTheBillingAccountHasHad(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	first := w.subscribe(account, ba)
	w.month(on(1, 1), ws)

	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State)

	w.srv.CancelNow(first.MorSubscription)
	w.heard("subscription.canceled", first.MorSubscription)
	w.take()
	second := w.subscribe(account, ba)
	x.NotEqual(first.MorSubscription, second.MorSubscription)

	w.to(on(2, 10).Add(mor.Settle + time.Minute))
	w.run()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	a = w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	_, err := w.charged(first, a)
	x.NoError(err, "in the subscription it was sent to")
	_, err = w.charged(second, a)
	x.Equal(mor.NotFound, mor.KindOf(err), "and in no other")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}

// The payer's method refusing the charge fails the attempt as a decline of
// its kind, with the merchant's code: what dunning (#11) sets its policy by.
// The invoice stays open, and nothing more is sent of it here.
func TestADeclineFailsTheAttemptWithItsKind(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	refused, gone := levyantest.Tenant(), levyantest.Tenant()
	a1, b1 := w.payer(refused)
	a2, b2 := w.payer(gone)
	w.to(on(2, 9))
	w.subscribe(a1, b1)
	w.subscribe(a2, b2)
	w.month(on(1, 1), refused, gone)

	// Each billing account's charge in turn: the one refused, and the one
	// whose subscription has no method to charge.
	w.srv.Decline("not_enough_balance")
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 400, Code: "subscription_missing_payment_method_cannot_update"})
	w.run()

	got := map[string]*levyan.AttemptFailure{}
	for _, ba := range []pdid.Id{b1, b2} {
		inv := w.only(ba)
		x.Equal(open, inv.State)
		as := w.charges(inv)
		x.Len(as, 1)
		x.Equal(failedAt, as[0].State)
		x.NotNil(as[0].DateResolved)
		got[as[0].Failure.GetCode()] = as[0].Failure
	}
	declined := got["subscription_payment_declined"]
	x.NotNil(declined)
	x.Equal(levyan.FailureKind_FAILURE_KIND_DECLINED, declined.GetKind())
	x.Equal(levyan.DeclineKind_DECLINE_KIND_UNSPECIFIED, declined.GetDecline(), "a charge made now that is declined says no more of why")
	method := got["subscription_missing_payment_method_cannot_update"]
	x.NotNil(method)
	x.Equal(levyan.FailureKind_FAILURE_KIND_DECLINED, method.GetKind())
	x.Equal(levyan.DeclineKind_DECLINE_KIND_REPLACE, method.GetDecline(), "a method to give anew")

	w.to(on(2, 11))
	w.run()
	x.Len(w.charges(w.only(b1)), 1, "trying again is dunning's")
	x.Equal(2, w.srv.Requests(paddletest.Charge))
}

// A billing account that cannot be collected from is sent nothing: its
// attempt fails at once, as a decline of the kind closest to what its payer
// has to do, by a code that names its collection -- no subscription, or one
// canceled, as a method to give anew; a payment that waits for its payer as
// the payer's to act on; past due as one that may go through later. So
// dunning treats a billing account with no method as it treats a card to
// replace.
func TestABillingAccountThatCannotBeCollectedFromIsSentNothing(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	none, canceled, waits, due, stale := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()
	_, bNone := w.payer(none)
	aCanceled, bCanceled := w.payer(canceled)
	aWaits, bWaits := w.payer(waits)
	aDue, bDue := w.payer(due)
	aStale, bStale := w.payer(stale)

	w.to(on(1, 20))
	sCanceled := w.subscribe(aCanceled, bCanceled)
	sWaits := w.subscribe(aWaits, bWaits)
	sDue := w.subscribe(aDue, bDue)
	w.to(on(2, 9))
	sStale := w.subscribe(aStale, bStale)

	// Canceled at the merchant, and heard of.
	w.srv.CancelNow(sCanceled.MorSubscription)
	w.heard("subscription.canceled", sCanceled.MorSubscription)
	// A renewal that waits for its payer to approve it, and one declined.
	w.srv.AwaitAction()
	txn := w.srv.Renew(sWaits.MorSubscription)
	w.heard("transaction.past_due", txn)
	w.heard("subscription.past_due", sWaits.MorSubscription)
	w.srv.Decline("not_enough_balance")
	txn = w.srv.Renew(sDue.MorSubscription)
	w.heard("transaction.past_due", txn)
	w.heard("subscription.past_due", sDue.MorSubscription)
	w.take()
	// Canceled at the merchant, and not heard of yet: the merchant says so.
	w.srv.CancelNow(sStale.MorSubscription)

	w.month(on(1, 1), none, canceled, waits, due, stale)
	w.run()

	for ba, want := range map[pdid.Id]struct {
		code    string
		decline levyan.DeclineKind
	}{
		bNone:     {"no_subscription", levyan.DeclineKind_DECLINE_KIND_REPLACE},
		bCanceled: {"canceled", levyan.DeclineKind_DECLINE_KIND_REPLACE},
		bWaits:    {"needs_action", levyan.DeclineKind_DECLINE_KIND_REPLACE},
		bDue:      {"past_due", levyan.DeclineKind_DECLINE_KIND_TEMPORARY},
		bStale:    {"canceled", levyan.DeclineKind_DECLINE_KIND_REPLACE},
	} {
		inv := w.only(ba)
		x.Equal(open, inv.State, want.code)
		as := w.charges(inv)
		x.Len(as, 1, want.code)
		f := as[0].Failure
		x.Equal(failedAt, as[0].State, want.code)
		x.Equal(levyan.FailureKind_FAILURE_KIND_DECLINED, f.GetKind(), want.code)
		x.Equal(want.decline, f.GetDecline(), want.code)
		x.Equal(want.code, f.GetCode())
		x.NotEmpty(f.GetMessage())
	}
	x.Zero(w.srv.Requests(paddletest.Charge), "nothing charged: the one levyan believed collectable, the merchant said had ended")
}

// What a billing account whose collection is not collectable fails with, by
// its collection: a paused subscription is one to try again on later, the
// method being sound.
func TestWhatAnUncollectableBillingAccountFailsWith(t *testing.T) {
	x := require.New(t)
	x.Nil(merchant.Failure(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE))
	for state, want := range map[levyan.CollectionState]levyan.DeclineKind{
		levyan.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION: levyan.DeclineKind_DECLINE_KIND_REPLACE,
		levyan.CollectionState_COLLECTION_STATE_CANCELED:        levyan.DeclineKind_DECLINE_KIND_REPLACE,
		levyan.CollectionState_COLLECTION_STATE_NEEDS_ACTION:    levyan.DeclineKind_DECLINE_KIND_REPLACE,
		levyan.CollectionState_COLLECTION_STATE_PAUSED:          levyan.DeclineKind_DECLINE_KIND_TEMPORARY,
		levyan.CollectionState_COLLECTION_STATE_PAST_DUE:        levyan.DeclineKind_DECLINE_KIND_TEMPORARY,
	} {
		f := merchant.Failure(state)
		x.Equal(levyan.FailureKind_FAILURE_KIND_DECLINED, f.GetKind(), "%s", state)
		x.Equal(want, f.GetDecline(), "%s", state)
		x.NotEmpty(f.GetCode())
	}
	x.Equal("paused", merchant.Failure(levyan.CollectionState_COLLECTION_STATE_PAUSED).GetCode())
}

// A key the merchant charged twice -- here, a charge made again by a caller
// that did not keep to `mor.Settle` -- settles the attempt by the first that
// was paid, once: the invoice is paid what was due and no more, and the
// attempt is marked for reconciliation (#13), which gives the rest back. A
// second charge of a key whose attempt has ended, heard of from a notice, is
// marked as well, and moves nothing.
func TestAKeyChargedTwiceSettlesOnceAndIsMarked(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	a1, b1 := w.payer(ws)
	w.to(on(2, 9))
	s1 := w.subscribe(a1, b1)
	w.month(on(1, 1), ws)

	// Asked: the answer lost, and the key charged twice meanwhile.
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(b1)
	a := w.charges(inv)[0]
	txn, err := w.charged(s1, a)
	x.NoError(err)
	w.srv.Duplicate(txn.ID)
	w.to(on(2, 10).Add(time.Minute))
	w.run()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Paid, "paid what was due, once")
	x.Zero(w.balance(b1), "the second charge is reconciliation's, and no credit of levyan's")
	a = w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	x.Equal(txn.ID, a.ChannelRef, "settled by the first")
	x.NotNil(a.DateTwice, "marked for reconciliation")
	_, err = w.charged(s1, a)
	x.ErrorIs(err, mor.ErrTwice)

	// Heard: February's attempt settled, and a second charge of its key
	// noticed.
	w.srv.Renew(s1.MorSubscription)
	w.month(on(2, 1), ws)
	w.run()
	inv = w.invoices(b1)[1]
	x.Equal(paid, inv.State)
	a = w.charges(inv)[0]
	x.Nil(a.DateTwice)
	txn, err = w.charged(s1, a)
	x.NoError(err)
	dup := w.srv.Duplicate(txn.ID)
	w.heard("transaction.completed", dup)
	w.take()
	a = w.charges(inv)[0]
	x.Equal(succeeded, a.State)
	x.Equal(txn.ID, a.ChannelRef, "what it settled by")
	x.NotNil(a.DateTwice, "marked for reconciliation")
	x.Equal(int64(5000), w.again(inv).Paid)
}

// Scenario 13 through the merchant: a credit note against a paid invoice is
// a refund of the transaction the invoice was charged in, which the merchant
// may take days to approve. Requested, it is pending, and asked about until
// the merchant decides; approved, it succeeded; rejected, it failed, and an
// operator is to see to it. One refund waiting holds the next back, which is
// sent once it is decided; one of all that was charged gives back all of the
// line, the merchant's tax as it took it.
func TestACreditNoteIsRefundedThroughTheMerchant(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws, whole := levyantest.Tenant(), levyantest.Tenant()
	account, ba := w.payer(ws)
	a2, b2 := w.payer(whole)
	w.to(on(2, 9))
	w.subscribe(account, ba)
	w.subscribe(a2, b2)
	w.month(on(1, 1), ws, whole)
	w.run()
	inv := w.only(ba)
	x.Equal(paid, inv.State)
	charge := w.charges(inv)[0]

	refund := func(inv *ent.Invoice, amount int64) {
		w.t.Helper()
		cn, err := w.S.Invoices.Credit(w.ctx, pdid.Id(inv.Id), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: amount, Words: "부분 환불"})
		x.NoError(err)
		x.Equal(amount, cn.Refunded)
	}

	refund(inv, 2000)
	w.run()
	rs := w.refunds(inv)
	x.Len(rs, 1)
	x.Equal(pending, rs[0].State, "waiting for the merchant's approval")
	x.Contains(rs[0].ChannelRef, "adj_")
	first := rs[0].ChannelRef
	got, err := w.S.Subscriptions.Channel.Transaction(w.ctx, charge.ChannelRef)
	x.NoError(err)
	adj, ok := got.Adjustment(rs[0].AttemptKey)
	x.True(ok, "kept by its key at the merchant")
	x.Equal(krw(2200), adj.Amount, "levyan's amount, and the merchant's tax on it")

	// The rest, while the first waits: held back, and not lost.
	refund(inv, 3000)
	w.run()
	rs = w.refunds(inv)
	x.Len(rs, 2)
	x.Equal(requested, rs[1].State, "one refund waiting holds the next back")

	w.srv.Approve(first)
	w.to(on(2, 10).Add(mor.Settle + time.Minute))
	w.run()
	rs = w.refunds(inv)
	x.Equal(succeeded, rs[0].State, "approved")
	x.Equal(pending, rs[1].State, "sent once the first was decided")

	// The merchant rejects the second, and its notice says so.
	w.srv.Reject(rs[1].ChannelRef)
	w.heard("adjustment.updated", rs[1].ChannelRef)
	w.take()
	rs = w.refunds(inv)
	x.Equal(failedAt, rs[1].State, "rejected")
	x.Equal(levyan.FailureKind_FAILURE_KIND_INVALID, rs[1].Failure.GetKind())
	x.Equal("rejected", rs[1].Failure.GetCode())
	x.Equal(int64(5000), w.again(inv).Paid, "a refund takes nothing back from what was paid of the invoice")

	// All that was charged, given back as all of the line.
	whole2 := w.only(b2)
	refund(whole2, 5000)
	w.run()
	r := w.refunds(whole2)[0]
	x.Equal(pending, r.State)
	got, err = w.S.Subscriptions.Channel.Transaction(w.ctx, w.charges(whole2)[0].ChannelRef)
	x.NoError(err)
	adj, ok = got.Adjustment(r.AttemptKey)
	x.True(ok)
	x.Equal(krw(5500), adj.Amount, "all of the line, its tax with it")
	w.srv.Approve(r.ChannelRef)
	w.heard("adjustment.updated", r.ChannelRef)
	w.take()
	x.Equal(succeeded, w.refunds(whole2)[0].State, "approved, as the notice was taken")
}

// What is due of a billing account that is more than nothing and less than
// the merchant charges -- 980 won, as its adapter says -- is not charged: it
// waits for the next period, and the invoice of both is charged once
// (the owner's, 2026-10-31).
func TestLessThanTheMerchantChargesWaitsForTheNextPeriod(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X
	x.Equal(int64(980), w.S.Invoices.Collectors[morCh].Minimum("KRW"))

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	w.daily(ws, on(1, 1), on(2, 1), 110)
	w.to(on(2, 10))
	w.close(on(2, 10))
	w.run()

	inv := w.only(ba)
	x.Equal(draft, inv.State, "500 is less than 980")
	x.Equal(uint32(1), inv.Carried)
	x.Zero(w.srv.Requests(paddletest.Charge))

	w.to(on(3, 10))
	w.daily(ws, on(2, 1), on(3, 1), 110)
	w.srv.Renew(sub.MorSubscription)
	w.close(on(3, 10))
	w.run()
	inv = w.only(ba)
	x.Equal(paid, inv.State)
	x.Equal(int64(1000), inv.Due, "January's and February's")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
	_, err := w.charged(sub, w.charges(inv)[0])
	x.NoError(err)
}

// Shadow billing's drafts are never charged (open question 63, Claude's): a
// draft of a period that ended before collection on the merchant's channel
// began is closed as a shadow -- no attempt, nothing asked of the merchant,
// no credit taken -- which its payer is not shown, and the first period that
// ends from then on is invoiced and charged as any other.
func TestAShadowDraftIsNeverCharged(t *testing.T) {
	w := setup(t, on(3, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(1, 20))
	sub := w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.run()
	w.run()

	jan := w.only(ba)
	x.Equal(shadow, jan.State, "January ended before collection began")
	x.Equal(int64(5000), jan.Subtotal, "what it would have come to, kept for an operator")
	x.Zero(jan.Due)
	x.Nil(jan.DateFinalized, "never finalized")
	x.Nil(jan.BillTo)
	x.NotNil(jan.DateSettled)
	x.Contains(jan.Reason, "shadow billing")
	x.Empty(w.charges(jan))
	x.Zero(w.srv.Requests(paddletest.Charge))

	// Its payer is shown nothing of it.
	payers := levyanv1.NewPayerServiceClient(w.conn)
	as := levyantest.AsAccount(w.ctx, w.key, account)
	list, err := payers.ListInvoices(as, &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Empty(list.GetInvoices())
	_, err = payers.GetInvoice(as, levyanv1.GetInvoiceRequest_builder{Id: pdid.Id(jan.Id).String()}.Build())
	x.Equal(codes.NotFound, status.Code(err))

	// February ended as collection began: charged.
	w.srv.Renew(sub.MorSubscription)
	w.month(on(2, 1), ws)
	w.run()
	vs := w.invoices(ba)
	x.Len(vs, 2)
	feb := vs[1]
	x.Equal(paid, feb.State)
	x.True(feb.DateStart.Equal(on(2, 1)))
	x.Equal(int64(5000), feb.Due, "February's alone")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
	x.Equal(shadow, w.again(jan).State, "and the shadow stays one")

	list, err = payers.ListInvoices(as, &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Len(list.GetInvoices(), 1)
	x.Equal(pdid.Id(feb.Id).String(), list.GetInvoices()[0].GetId())
}

// A draft a deployment gathered while nothing collected on the merchant's
// channel -- a build before this one, or one that named no merchant -- holds
// every period since its first. Once a merchant is named, it is decided by
// its first period: begun before collection began, it is a shadow whatever it
// holds, and what it holds of periods after is not collected; begun from
// then on, it is charged, all of it.
func TestADraftGatheredBeforeCollectionIsDecidedByItsFirstPeriod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  time.Time
		state int32
		due   int64
	}{
		{"before it all", on(4, 1), shadow, 0},
		{"between its periods", on(2, 15), shadow, 0},
		{"from its first", on(2, 1), paid, 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := paddletest.New(t)
			w := arrange(t, levyantest.New(t), srv)
			x := w.X
			x.Nil(w.S.Invoices.Collectors[morCh], "no merchant, nothing collects on its channel")

			ws := levyantest.Tenant()
			account, ba := w.payer(ws)
			w.month(on(1, 1), ws)
			w.run()
			w.month(on(2, 1), ws)
			w.run()
			gathered := w.only(ba)
			x.Equal(draft, gathered.State)
			x.Len(w.lines(gathered), 2, "January's and February's, gathered")

			// The same deployment come up again, a merchant named: on the
			// fake's clock, as the one before it was.
			s := w.Again(levyantest.Merchant(srv, true), levyantest.LiveFrom(tc.from))
			s.Clock = srv.Now
			w.S = s
			w.subscribe(account, ba)
			w.run()
			inv := w.only(ba)
			x.Equal(tc.state, inv.State)
			x.Equal(tc.due, inv.Due)
			if tc.state == shadow {
				x.Empty(w.charges(inv))
				x.Zero(srv.Requests(paddletest.Charge))
			} else {
				x.Equal(1, srv.Requests(paddletest.Charge))
			}
		})
	}
}

func (w *world) lines(inv *ent.Invoice) []*ent.InvoiceLine {
	w.t.Helper()
	vs, err := w.S.Ent.InvoiceLine.Query().Where(entinvoiceline.InvoiceId(inv.Id)).All(w.ctx)
	w.X.NoError(err)
	return vs
}

// A draft of the shadow billing's periods alone gathers nothing that is
// collected, even while nothing decides it: a statement of a period that
// ended after collection began waits for the draft to be closed as a shadow,
// and is then an invoice of its own.
func TestAShadowsDraftGathersNothingCollected(t *testing.T) {
	w := setup(t, on(2, 15))
	x := w.X
	collector := w.S.Invoices.Collectors[morCh]
	delete(w.S.Invoices.Collectors, morCh)

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.month(on(1, 1), ws)
	w.run()
	w.month(on(2, 1), ws)
	w.run()
	jan := w.only(ba)
	x.Equal(draft, jan.State)
	x.Len(w.lines(jan), 1, "January's alone: February's waits")

	w.S.Invoices.Collectors[morCh] = collector
	w.subscribe(account, ba)
	w.run()
	vs := w.invoices(ba)
	x.Len(vs, 2)
	x.Equal(shadow, vs[0].State)
	x.Len(w.lines(vs[0]), 1)
	x.Equal(paid, vs[1].State)
	x.True(vs[1].DateStart.Equal(on(2, 1)))
	x.Len(w.lines(vs[1]), 1)
	x.Equal(int64(5000), vs[1].Due)
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}

// What the merchant's notice names of no attempt of levyan's -- a key of
// nobody's here, a line of no key -- is nothing to the ledger; and one that
// names a refund's key as a charge's is not taken for it.
func TestANoticeOfNoAttemptIsNothing(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	x.NoError(w.S.Taker.Transaction(w.ctx, nil, mor.Transaction{ID: "txn_00000000000000000000000001", Status: mor.Paid, Lines: []mor.Line{{Key: "lv-nobodys"}, {}}}))

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	a := w.charges(w.only(ba))[0]
	err := w.S.Taker.Transaction(w.ctx, nil, mor.Transaction{ID: "txn_00000000000000000000000002", Status: mor.Paid,
		Adjustments: []mor.Adjustment{{ID: "adj_00000000000000000000000001", Kind: mor.RefundAdjustment, Status: mor.Approved, Key: a.AttemptKey}}})
	x.NoError(err)
	x.Equal(unknown, w.charges(w.only(ba))[0].State, "a charge's key named as a refund's")
}
