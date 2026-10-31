package reconcile_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/reconcile"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// A deployment whose charges, refunds and notices went as they should finds
// nothing: every check is made, the merchant's included, and none is passed
// over -- and a pass made again writes nothing.
func TestWhatAddsUpFindsNothing(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, a, _ := w.charged()
	txn, err := w.S.Subscriptions.Channel.Transaction(w.ctx, a.ChannelRef)
	x.NoError(err)
	w.heard("transaction.completed", txn.ID)
	w.take()

	// And a credit note given back through the merchant, approved.
	_, err = w.S.Invoices.Credit(w.ctx, pdid.Id(inv.Id), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: 2000})
	x.NoError(err)
	w.run()
	r := w.refunds(inv)[0]
	x.Equal(pending, r.State)
	w.srv.Approve(r.ChannelRef)
	w.to(on(2, 10).Add(mor.Settle + time.Minute))
	w.run()
	x.Equal(succeeded, w.attempt(r).State)

	rep := w.reconcile()
	x.Len(rep.Checks, len(reconcile.Checks()))
	for _, c := range rep.Checks {
		x.False(c.Skipped, c.Name)
		x.Zero(c.Found, c.Name)
	}
	x.Zero(rep.New)
	w.nothing()
	x.Equal(reconcile.Statements{WithAccount: 1, OnOne: 1}, rep.Statements, "counted, though the index says so")

	rep = w.reconcile()
	x.Zero(rep.New + rep.Changed + rep.Resolved)
}

// A key the merchant charged twice whose notice never came -- its webhook
// dropped, its event never read -- is found by the pass that reads what the
// merchant billed: the attempt settled by one payment, and the other is to
// give back. A pass that finds it again writes nothing new. The operator
// gives it back -- a refund of the extra payment, through the ledger, which
// takes nothing of the invoice -- and once the merchant approves it the
// finding is resolved, and nothing else is found of it.
func TestAKeyChargedTwiceWhoseNoticeNeverCameIsFoundAndGivenBack(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, a, _ := w.charged()
	dup := w.srv.Duplicate(a.ChannelRef)
	x.Nil(w.attempt(a).DateTwice, "nothing marked it: its notice never came")

	rep := w.reconcile()
	x.Equal(1, rep.New)
	fs := w.of(levyan.FindingKind_FINDING_KIND_TWICE)
	x.Len(fs, 1)
	f := fs[0]
	x.Equal(reconcile.Subject("PaymentAttempt", a.Id), f.Subject)
	x.Equal(open, f.State)
	x.Equal(int32(levyan.FindingSeverity_FINDING_SEVERITY_CRITICAL), f.Severity)
	x.Equal(int32(levyan.FindingCorrection_FINDING_CORRECTION_REFUND), f.Correction)
	x.Equal(int64(5000), f.Amount)
	x.Equal("KRW", f.Currency)
	x.Contains(f.Summary, dup)
	x.Equal(w.product.Uuid(), f.ProductId)
	x.Len(w.findings(), 1, "and nothing else")

	rep = w.reconcile()
	x.Zero(rep.New+rep.Changed, "still so, and not written again")

	// Given back by the operator: a refund of the extra payment, sent now.
	rs, err := w.S.Reconciler.Refund(w.op, a.AttemptKey)
	x.NoError(err)
	x.Len(rs, 1)
	r := rs[0]
	x.Equal(int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND), r.Kind)
	x.Equal(pending, r.State, "waiting for the merchant's approval")
	x.Equal(a.AttemptKey, r.ExtraOf)
	x.Equal(dup, r.Refunds)
	x.Equal(int64(5000), r.Amount)
	got, err := w.S.Subscriptions.Channel.Transaction(w.ctx, dup)
	x.NoError(err)
	adj, ok := got.Adjustment(r.AttemptKey)
	x.True(ok, "asked of the extra payment, by its key")
	x.Equal(krw(5000), adj.Asked)

	again, err := w.S.Reconciler.Refund(w.op, a.AttemptKey)
	x.NoError(err)
	x.Len(again, 1)
	x.Equal(r.Id, again[0].Id, "asked for already: nothing is sent again")

	w.reconcile()
	f = w.finding(f)
	x.Equal(open, f.State, "not given back until the merchant approves it")
	x.Contains(f.Summary, "being given back by "+r.AttemptKey)

	w.srv.Approve(adj.ID)
	w.to(w.srv.Now().Add(time.Minute))
	w.run()
	x.Equal(succeeded, w.attempt(r).State)

	rep = w.reconcile()
	x.Equal(1, rep.Resolved)
	f = w.finding(f)
	x.Equal(resolved, f.State)
	x.Equal(reconcile.NoLonger, f.Resolution)
	w.nothing()

	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Paid, "the invoice was paid once, and still is")
	x.Zero(inv.Reduced)
}

// A key the ledger marked charged twice -- a notice of the second payment
// taken, which wrote nothing but the mark -- is found by its mark, even once
// the payments are older than the window the merchant is read in.
func TestAKeyMarkedChargedTwiceIsFoundByItsMark(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	dup := w.srv.Duplicate(a.ChannelRef)
	w.heard("transaction.completed", dup)
	w.heard("transaction.completed", dup)
	w.take()
	x.NotNil(w.attempt(a).DateTwice)

	w.to(w.srv.Now().Add(reconcile.DefaultWindow + 24*time.Hour))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_TWICE)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, dup)
}

// An operator may accept a finding as it is, with a reason: it stays
// accepted, and nothing open is said of it, while it says what it said --
// and once it says something else, it is open again.
func TestAnAcceptedFindingStaysAcceptedUntilItSaysSomethingElse(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	w.srv.Duplicate(a.ChannelRef)
	w.reconcile()
	f := w.of(levyan.FindingKind_FINDING_KIND_TWICE)[0]

	_, err := w.S.Reconciler.Accept(w.ctx, pdid.Id(f.Id), "the payer agreed to keep it as credit")
	x.Error(err, "an operator's")
	_, err = w.S.Reconciler.Accept(w.op, pdid.Id(f.Id), " ")
	x.Error(err, "with a reason")

	v, err := w.S.Reconciler.Accept(w.op, pdid.Id(f.Id), "the payer agreed to keep it as credit")
	x.NoError(err)
	x.Equal(accepted, v.State)
	x.Equal("the payer agreed to keep it as credit", v.Reason)
	x.NotNil(v.DateAccepted)
	x.NotEqual(uuid.UUID{}, v.AcceptedById)

	rep := w.reconcile()
	x.Zero(rep.Open)
	x.Equal(1, rep.Found)
	x.Equal(accepted, w.finding(f).State, "still what was accepted")

	// Charged a third time: not what was accepted.
	w.srv.Duplicate(a.ChannelRef)
	w.reconcile()
	v = w.finding(f)
	x.Equal(open, v.State)
	x.Empty(v.Reason)
	x.Nil(v.DateAccepted)
	x.Equal(int64(10000), v.Amount)
}

// A paid transaction that carries a key no attempt of this deployment's has
// -- another deployment charging through the same account, a ledger restored
// from before it was written -- is money the ledger has no record of: found,
// of no product, until the merchant has given it all back.
func TestAPaymentOfAKeyNoAttemptHasIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	sub := w.subscribe(account, ba)
	ch := w.S.Subscriptions.Channel
	foreign := "lv-00000000000000000000000000000001"
	txn, err := ch.Charge(w.ctx, mor.Charge{Subscription: sub.MorSubscription, Key: foreign, When: mor.Now, Amount: krw(1500), Description: "다른 배포의 청구"})
	x.NoError(err)

	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_KEY_UNKNOWN)
	x.Len(fs, 1)
	f := fs[0]
	x.Equal("paddle.transaction/"+txn.ID, f.Subject)
	x.Equal(int32(levyan.FindingSeverity_FINDING_SEVERITY_CRITICAL), f.Severity)
	x.Equal(int64(1500), f.Amount)
	x.Contains(f.Summary, foreign)
	x.Equal(uuid.UUID{}, f.ProductId, "no product's")

	// Given back at the merchant, by hand.
	adj, err := ch.Refund(w.ctx, mor.Refund{Transaction: txn.ID, All: true, Reason: "다른 배포의 것", Key: "byhand-1"})
	x.NoError(err)
	w.reconcile()
	x.Len(w.of(levyan.FindingKind_FINDING_KIND_KEY_UNKNOWN), 1, "asked, not given back yet")
	w.srv.Approve(adj.ID)
	w.reconcile()
	w.nothing()
}

// A subscription the payer cancelled in the merchant's own pages, whose
// notice never came -- its webhook dropped, its event never read -- leaves
// the mirror active and the billing account collectable. Once the mirror has
// gone unchecked for longer than it should, a pass asks the merchant of it
// again: the mirror is found behind, and put right by the asking.
func TestAMirrorTheMerchantMovedOnFromIsAskedAgain(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	w.srv.Behind(30 * 24 * time.Hour)
	sub := w.subscribe(account, ba)
	w.to(on(2, 9).Add(time.Hour))
	w.srv.CancelNow(sub.MorSubscription)
	_, err := subscriptions.Puller{In: w.S.Inbox, Now: w.srv.Now}.Pull(w.ctx)
	x.NoError(err)
	w.take()

	w.to(on(2, 12))
	w.reconcile()
	w.nothing()
	m, err := w.S.Ent.Subscription.Get(w.ctx, sub.Id)
	x.NoError(err)
	x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE), m.Status, "neither its webhook nor its event came, and it was checked lately")

	w.to(on(2, 9).Add(reconcile.DefaultMirror + time.Hour))
	rep := w.reconcile()
	x.Equal(1, rep.Resolved, "found, and put right at once")
	f, err := w.S.Ent.Finding.Query().All(w.ctx)
	x.NoError(err)
	x.Len(f, 1)
	x.Equal(levyan.FindingKind_FINDING_KIND_MIRROR_STALE.String(), f[0].Kind)
	x.Equal(resolved, f[0].State)
	x.Equal(reconcile.Reasked, f[0].Resolution)

	m, err = w.S.Ent.Subscription.Get(w.ctx, sub.Id)
	x.NoError(err)
	x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED), m.Status)
	b, err := w.S.Ent.BillingAccount.Get(w.ctx, ba.Uuid())
	x.NoError(err)
	x.Equal(int32(levyan.CollectionState_COLLECTION_STATE_CANCELED), b.Collection)

	// The notice that never came, come at last -- and an older one after it,
	// and the newer again: nothing is walked back.
	es := w.srv.Events()
	slices.Reverse(es)
	es = append(es, es...)
	for _, e := range es {
		if e[1] == "subscription.canceled" || e[1] == "subscription.created" {
			h, body := w.srv.Deliver(e[0])
			n, err := w.S.Inbox.Channel.Webhook(h, body)
			x.NoError(err)
			_, err = w.S.Inbox.Keep(w.ctx, n, levyan.NoticeVia_NOTICE_VIA_WEBHOOK)
			x.NoError(err)
		}
	}
	w.take()
	m, err = w.S.Ent.Subscription.Get(w.ctx, sub.Id)
	x.NoError(err)
	x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED), m.Status)
	rep = w.reconcile()
	x.Zero(rep.New + rep.Changed)
	w.nothing()
}

// A charge whose send never arrived, and whose absence cannot be believed
// yet, is left unknown: once it has been for longer than it should, it is
// found, and asked again -- which says nothing yet. Once the merchant can
// say it never had it, asking again puts it to be sent again, and the
// finding is resolved; sent, it is paid once.
func TestAnAttemptLeftUnknownIsAskedAgainAndFound(t *testing.T) {
	w := setup(t)
	x := w.X
	w.S.Reconciler.Stale.Unknown = 5 * time.Minute

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error"})
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State)

	w.to(w.srv.Now().Add(6 * time.Minute))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_ATTEMPT_STALE)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "unknown since")
	x.Contains(fs[0].Summary, "asking the channel again failed: unknown")
	x.Equal(int32(levyan.FindingCorrection_FINDING_CORRECTION_SETTLE), fs[0].Correction)

	w.to(w.srv.Now().Add(mor.Settle))
	rep := w.reconcile()
	x.Equal(1, rep.Resolved)
	x.Equal(requested, w.attempt(a).State, "never had: to be sent again")
	w.run()
	x.Equal(succeeded, w.attempt(a).State)
	x.Equal(1, w.srv.Charged(), "paid once")
	w.reconcile()
	w.nothing()
}

// A charge whose answer was lost after the merchant made it, left unknown
// for longer than it should be, is asked again by the pass that finds it,
// and settled by what the merchant answers: found, and put right at once.
func TestALostAnswerIsSettledByThePassThatFindsIt(t *testing.T) {
	w := setup(t)
	x := w.X
	w.S.Reconciler.Stale.Unknown = 30 * time.Minute

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State)

	w.to(w.srv.Now().Add(time.Hour))
	rep := w.reconcile()
	x.Equal(1, rep.Resolved)
	x.Equal(succeeded, w.attempt(a).State)
	x.Equal(paid, w.again(inv).State)
	fs, err := w.S.Ent.Finding.Query().All(w.ctx)
	x.NoError(err)
	x.Len(fs, 1)
	x.Equal(resolved, fs[0].State)
	x.Contains(fs[0].Summary, "settled it as succeeded")
	w.nothing()
}

// An operator settles an attempt from what the merchant answers of a
// transaction they name -- found in the merchant's own pages -- as a notice
// of it would: what is further along than the attempt is written, and the
// rest is nothing.
func TestAnOperatorSettlesAnAttemptFromATransactionTheyName(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.to(on(2, 9))
	w.subscribe(account, ba)
	w.month(on(1, 1), ws)
	w.srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(unknown, a.State)

	_, err := w.S.Reconciler.Settle(w.ctx, a.AttemptKey, "")
	x.Error(err, "an operator's")
	ts, err := w.S.Subscriptions.Channel.Transactions(w.ctx, mor.List{})
	x.NoError(err)
	txn := ""
	for _, t := range ts.Transactions {
		if _, ok := t.Line(a.AttemptKey); ok {
			txn = t.ID
		}
	}
	x.NotEmpty(txn)
	_, err = w.S.Reconciler.Settle(w.op, a.AttemptKey, ts.Transactions[0].ID)
	x.Error(err, "a transaction of nothing of its key")

	v, err := w.S.Reconciler.Settle(w.op, pdid.Id(a.Id).String(), txn)
	x.NoError(err)
	x.Equal(succeeded, v.State)
	x.Equal(txn, v.ChannelRef)
	x.Equal(paid, w.again(inv).State)

	v, err = w.S.Reconciler.Settle(w.op, a.AttemptKey, "")
	x.NoError(err)
	x.Equal(succeeded, v.State, "one that ended is what it is")
}

// A refund the merchant rejected leaves what its credit note gives back not
// given back: found, for an operator to see to (open question 70).
func TestARefundTheMerchantRejectedIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, _, _ := w.charged()
	_, err := w.S.Invoices.Credit(w.ctx, pdid.Id(inv.Id), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: 2000})
	x.NoError(err)
	w.run()
	r := w.refunds(inv)[0]
	w.reconcile()
	w.nothing()

	w.srv.Reject(r.ChannelRef)
	w.heard("adjustment.updated", r.ChannelRef)
	w.take()
	x.Equal(failed, w.attempt(r).State)

	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_REFUND_NOT_MADE)
	x.Len(fs, 1)
	x.Equal(int64(2000), fs[0].Amount, "what the credit note gives back")

	_, err = w.S.Reconciler.Accept(w.op, pdid.Id(fs[0].Id), "given back by transfer, by hand")
	x.NoError(err)
	rep := w.reconcile()
	x.Zero(rep.Open)
}

// A refund the ledger says succeeded is held to the merchant: one the
// merchant has not approved -- a ledger that wrote more than it was told --
// is found, until the merchant approves it.
func TestARefundTheLedgerSaysSucceededIsHeldToTheMerchant(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, _, _ := w.charged()
	_, err := w.S.Invoices.Credit(w.ctx, pdid.Id(inv.Id), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: 2000})
	x.NoError(err)
	w.run()
	r := w.refunds(inv)[0]
	x.Equal(pending, r.State)

	// Written past the servers, as a ledger that went wrong would.
	x.NoError(w.S.Ent.PaymentAttempt.UpdateOneId(r.Id).SetState(succeeded).SetDateResolved(w.srv.Now()).Exec(w.ctx))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_REFUND_UNMATCHED)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "holds as requested")

	w.srv.Approve(r.ChannelRef)
	w.reconcile()
	w.nothing()
}

// A charge the ledger says succeeded is held to the merchant: one whose
// transaction charged another amount of its key -- or holds none of it -- is
// found.
func TestAChargeTheLedgerSaysSucceededIsHeldToTheMerchant(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()

	// Written past the servers, as a ledger that went wrong would: settled
	// by a transaction the merchant has not.
	x.NoError(w.S.Ent.PaymentAttempt.UpdateOneId(a.Id).SetChannelRef("txn_00000000000000000000000000").Exec(w.ctx))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_CHARGE_UNMATCHED)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "which the merchant has not")
	x.Equal(int32(levyan.FindingSeverity_FINDING_SEVERITY_ERROR), fs[0].Severity)
	x.Len(w.of(levyan.FindingKind_FINDING_KIND_TWICE), 1, "and the payment the merchant does hold of its key is one the ledger does not account for")

	x.NoError(w.S.Ent.PaymentAttempt.UpdateOneId(a.Id).SetChannelRef(a.ChannelRef).Exec(w.ctx))
	w.reconcile()
	w.nothing()
}

// What the merchant gave back of a charge of levyan's that the ledger has no
// refund of -- made in the merchant's own pages -- is found.
func TestARefundMadeAtTheMerchantIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	adj, err := w.S.Subscriptions.Channel.Refund(w.ctx, mor.Refund{Transaction: a.ChannelRef, All: true, Reason: "대시보드에서", Key: "dashboard-1"})
	x.NoError(err)
	w.srv.Approve(adj.ID)
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_ADJUSTMENT_UNKNOWN)
	x.Len(fs, 1)
	x.Equal("paddle.adjustment/"+adj.ID, fs[0].Subject)
	x.Contains(fs[0].Summary, "dashboard-1")
	x.Equal(int64(5000), fs[0].Amount)
	x.Len(w.findings(), 1)

	_, err = w.S.Reconciler.Accept(w.op, pdid.Id(fs[0].Id), "given back by hand at the payer's request")
	x.NoError(err)
	rep := w.reconcile()
	x.Zero(rep.Open)
}

// A notice of the merchant's that could not be taken for longer than it
// should is found, and resolved once the inbox has taken it.
func TestANoticeNotTakenIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	w.heard("transaction.updated", a.ChannelRef)
	w.srv.Fail(paddletest.Get, paddletest.Fault{Status: 503, Code: "service_unavailable"})
	w.take()

	w.to(w.srv.Now().Add(2 * time.Hour))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_NOTICE_STUCK)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "1 times")

	w.take()
	w.reconcile()
	w.nothing()
}

// A check that cannot be made -- the merchant could not be asked -- is a
// finding of its own, and resolves nothing it found before: what was so is
// left so until the check is made again.
func TestACheckThatCannotBeMadeResolvesNothing(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	w.srv.Duplicate(a.ChannelRef)
	w.reconcile()
	x.Len(w.of(levyan.FindingKind_FINDING_KIND_TWICE), 1)

	w.srv.Fail(paddletest.List, paddletest.Fault{Status: 500, Code: "internal_error"})
	rep, err := w.S.Reconciler.Run(w.ctx)
	x.NoError(err)
	failedChecks := 0
	for _, c := range rep.Checks {
		if c.Err != "" {
			failedChecks++
			x.Equal("merchant", c.Name)
		}
	}
	x.Equal(1, failedChecks)
	x.Len(w.of(levyan.FindingKind_FINDING_KIND_TWICE), 1, "left as it was")
	fs := w.of(levyan.FindingKind_FINDING_KIND_CHECK_FAILED)
	x.Len(fs, 1)
	x.Equal("check/merchant", fs[0].Subject)

	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_CHECK_FAILED))
	x.Len(w.of(levyan.FindingKind_FINDING_KIND_TWICE), 1)
}

// With no merchant of record named, its checks are passed over, and what
// they found before is left as it was.
func TestWithNoMerchantItsChecksArePassedOver(t *testing.T) {
	w := bare(t)
	x := w.X

	rep := w.reconcile()
	skipped := []string{}
	for _, c := range rep.Checks {
		if c.Skipped {
			skipped = append(skipped, c.Name)
		}
	}
	x.Equal([]string{"mirrors", "notices", "merchant"}, skipped)
}
