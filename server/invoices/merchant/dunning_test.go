package merchant_test

import (
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/payers"
)

const day = 24 * time.Hour

var uncollectible = int32(levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE)

// dun is a pass of the loop `levyan.invoices.dunning`.
func (w *world) dun() {
	w.t.Helper()
	w.X.NoError(w.S.Invoices.Dun(w.ctx, &w.S.Grants))
}

// restricted says whether workspace `ws` is restricted now as loft's copy
// decides it: by a grant of `loft.restricted` in force, whichever rule's.
func (w *world) restricted(ws pdid.Id) bool {
	w.t.Helper()
	row, ok, err := payers.Workspace(w.ctx, w.S.Ent, w.product.Uuid(), ws)
	w.X.NoError(err)
	if !ok {
		return false
	}
	vs, err := w.S.Ent.Grant.Query().Where(entgrant.WorkspaceId(row), entgrant.Key("loft.restricted"), entgrant.DateErasedIsNil()).All(w.ctx)
	w.X.NoError(err)
	var rows []entitle.Row
	for _, v := range vs {
		rows = append(rows, entitle.Row{Key: v.Key, Value: grants.Value(v.Proto()), From: v.ValidFrom, Until: v.ValidUntil, Grant: v.Id[:]})
	}

	return entitle.Decide("loft.restricted", rows, w.S.Clock()).Value.Has()
}

// told is what the payer is told of billing account `ba` and of invoice
// `inv` through loft, and what loft is told of workspace `ws`: the dunning
// each says, which are one.
func (w *world) told(account, ba, ws pdid.Id, inv pdid.Id) (*levyanv1.GetBillingAccountResponse, *levyanv1.GetInvoiceResponse, *levyanv1.Payer) {
	w.t.Helper()
	as := levyantest.AsAccount(w.ctx, w.key, account)
	payer := levyanv1.NewPayerServiceClient(w.conn)
	b, err := payer.GetBillingAccount(as, levyanv1.GetBillingAccountRequest_builder{Id: ba.String()}.Build())
	w.X.NoError(err)
	i, err := payer.GetInvoice(as, levyanv1.GetInvoiceRequest_builder{Id: inv.String()}.Build())
	w.X.NoError(err)
	p, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.as, levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	w.X.NoError(err)

	return b, i, p.GetPayer()
}

// Scenario 4 (levyan#1), end to end on the merchant of record: an invoice's
// charge declined, tried again a day, three and seven days after, its
// fortnight's grace over and the workspace it bills restricted, the invoice
// given up on; then
// its payer begins a subscription anew, the billing account is collectable
// again, the invoice is charged at once and paid, and the workspace is let
// go -- and what the payer and loft are told at each step is what the table
// says.
func TestScenario4AFailedPaymentIsRetriedRestrictedAndRecovered(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	for range 4 {
		w.srv.Decline("not_enough_balance")
	}
	w.month(on(1, 1), ws)
	w.run()

	// The failure.
	inv := w.only(ba)
	id := pdid.Id(inv.Id)
	t0 := w.charges(inv)[0].DateResolved.UTC()
	x.Equal(open, inv.State)
	b, i, p := w.told(account, ba, ws, id)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_COLLECTABLE, b.GetCollection().GetState(), "a decline of a charge made now leaves the subscription as it was")
	x.Len(b.GetDunning(), 1)
	d := b.GetDunning()[0]
	x.Equal(id.String(), d.GetInvoice())
	x.Equal(ba.String(), d.GetBillingAccount())
	x.Equal(levyanv1.DeclineKind_DECLINE_KIND_UNSPECIFIED, d.GetKind(), "a charge made now that is declined says no more of why")
	x.True(t0.Equal(d.GetFailed().AsTime()))
	x.True(t0.Add(day).Equal(d.GetNextRetry().AsTime()))
	x.True(t0.Add(14 * day).Equal(d.GetGraceEnds().AsTime()))
	x.True(t0.Add(14 * day).Equal(d.GetRestrictedFrom().AsTime()))
	x.True(t0.Add(60*day).Equal(d.GetReadonlyFrom().AsTime()), "read-only sixty days after it first failed")
	x.Equal(d.String(), i.GetDunning().String(), "the payer's invoice says the same")
	x.Len(p.GetDunning(), 1)
	x.Equal(d.String(), p.GetDunning()[0].String(), "and loft's workspace")

	// The retries.
	for n, at := range []time.Duration{day, 3 * day, 7 * day} {
		w.to(t0.Add(at))
		w.dun()
		w.dun()
		cs := w.charges(inv)
		x.Len(cs, n+2, "%s after", at)
		x.Equal(failedAt, cs[n+1].State)
		x.NotEqual(cs[n].AttemptKey, cs[n+1].AttemptKey, "a key of its own")
		_, i, _ := w.told(account, ba, ws, id)
		if n < 2 {
			x.True(t0.Add([]time.Duration{3 * day, 7 * day}[n]).Equal(i.GetDunning().GetNextRetry().AsTime()), "%s after", at)
		} else {
			x.False(i.GetDunning().HasNextRetry(), "no retry left")
		}
	}
	x.Equal(4, w.srv.Requests(paddletest.Charge))

	// The grace.
	w.to(t0.Add(13 * day))
	w.dun()
	x.Equal(open, w.again(inv).State)
	x.False(w.restricted(ws), "within its grace")

	// The restriction, and the invoice given up on.
	w.to(t0.Add(14 * day))
	w.dun()
	inv = w.again(inv)
	x.Equal(uncollectible, inv.State)
	x.True(w.restricted(ws))
	b, i, p = w.told(account, ba, ws, id)
	x.Equal(levyanv1.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE, i.GetInvoice().GetState())
	x.Equal(inv.Due, i.GetInvoice().GetRemaining(), "still owed")
	x.Len(b.GetDunning(), 1)
	x.Len(p.GetDunning(), 1)
	x.False(p.GetDunning()[0].HasNextRetry())

	// The payment: the payer cancels the subscription that would not pay
	// and begins another, and the invoice is charged as the billing account
	// is collectable again.
	w.to(t0.Add(16 * day))
	w.srv.CancelNow(sub.MorSubscription)
	w.heard("subscription.canceled", sub.MorSubscription)
	w.take()
	w.dun()
	x.Len(w.charges(inv), 4, "nothing is charged of a canceled subscription")
	again := w.subscribe(account, ba)
	x.NotEqual(sub.MorSubscription, again.MorSubscription)
	w.dun()
	w.dun()

	// The recovery.
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(inv.Due, inv.Paid)
	cs := w.charges(inv)
	x.Len(cs, 5, "charged once as it was collectable again")
	x.Equal(succeeded, cs[4].State)
	x.Equal(5, w.srv.Requests(paddletest.Charge))
	x.False(w.restricted(ws), "let go")
	b, i, p = w.told(account, ba, ws, id)
	x.Empty(b.GetDunning())
	x.False(i.HasDunning())
	x.Equal(levyanv1.InvoiceState_INVOICE_STATE_PAID, i.GetInvoice().GetState())
	x.Empty(p.GetDunning())
	vs, err := w.S.Ent.Grant.Query().Where(entgrant.Rule(invoices.RestrictionRule)).All(w.ctx)
	x.NoError(err)
	x.Len(vs, 1, "the restriction was one grant, given once")
	x.False(vs[0].Renews)
	x.False(vs[0].ValidUntil.After(w.S.Clock()), "and ended")
}

// A billing account that cannot be collected from -- here its payer never
// finished a checkout -- fails its invoice's charge as a method to give
// anew, which is never tried again as it is: the payer is told at once, and
// the invoice is charged as soon as the billing account is collectable, once.
func TestABillingAccountCollectableAgainIsChargedAtOnce(t *testing.T) {
	w := setup(t, on(2, 1))
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.month(on(1, 1), ws)
	w.run()
	inv := w.only(ba)
	cs := w.charges(inv)
	x.Len(cs, 1)
	x.Equal("no_subscription", cs[0].Failure.GetCode())

	b, _, _ := w.told(account, ba, ws, pdid.Id(inv.Id))
	x.Len(b.GetDunning(), 1)
	x.Equal(levyanv1.DeclineKind_DECLINE_KIND_REPLACE, b.GetDunning()[0].GetKind(), "a method to give anew: the payer is told at once")
	x.False(b.GetDunning()[0].HasNextRetry(), "and it is never tried again as it is")
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION, b.GetCollection().GetState())

	w.to(on(2, 13))
	w.dun()
	x.Len(w.charges(inv), 1, "nothing scheduled")

	w.subscribe(account, ba)
	b, _, _ = w.told(account, ba, ws, pdid.Id(inv.Id))
	x.True(b.GetDunning()[0].GetNextRetry().AsTime().Equal(b.GetCollection().GetSince().AsTime()), "due as it became collectable")
	w.dun()
	w.dun()
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Len(w.charges(inv), 2, "charged once")
	x.Equal(1, w.srv.Requests(paddletest.Charge))
}
