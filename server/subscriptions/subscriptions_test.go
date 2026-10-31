package subscriptions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entnotice "github.com/lesomnus/levyan/internal/ent/notice"
	entnoticecursor "github.com/lesomnus/levyan/internal/ent/noticecursor"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/subscriptions"
)

const back = "https://loft.example.com/billing/subscribed"

type world struct {
	*levyantest.World
	t   *testing.T
	ctx context.Context
	srv *paddletest.Server
}

// setup is a levyan selling through the fake Paddle: with its webhooks
// received, or, without `webhooks`, its events pulled alone.
func setup(t *testing.T, webhooks bool) world {
	srv := paddletest.New(t)
	w := levyantest.New(t, levyantest.Merchant(srv, webhooks))
	return world{World: w, t: t, ctx: context.Background(), srv: srv}
}

func (w world) subs() subscriptions.Subscriptions { return w.S.Subscriptions }

func (w world) taker() subscriptions.Taker {
	return subscriptions.Taker{In: w.S.Inbox, Subs: w.S.Subscriptions}
}

func (w world) puller() subscriptions.Puller {
	return subscriptions.Puller{In: w.S.Inbox, Now: w.srv.Now}
}

// billing is a billing account of `owner`'s in `country`, as the account
// makes one.
func (w world) billing(owner pdid.Id, country string) pdid.Id {
	w.t.Helper()
	v, err := w.S.Payers.Create(w.ctx, owner, levyan.BillingAccount_builder{Name: "회사용", Country: country}.Build())
	w.X.NoError(err)
	id, err := pdid.From(v.GetId())
	w.X.NoError(err)
	return id
}

// begun is a checkout of billing account `ba` of `owner`'s, begun, and the
// checkout the fake's payer finishes.
func (w world) begun(owner, ba pdid.Id) (*ent.Checkout, mor.Checkout) {
	w.t.Helper()
	co, win, err := w.subs().Start(w.ctx, owner, ba, back)
	w.X.NoError(err)
	return co, mor.Checkout{ID: co.MorTransaction, Window: win}
}

// subscribed is billing account `ba` of `owner`'s subscribed, paid with `m`:
// the mirror.
func (w world) subscribed(owner, ba pdid.Id, m paddletest.Method) *ent.Subscription {
	w.t.Helper()
	co, opened := w.begun(owner, ba)
	w.X.NoError(w.srv.PayWith(opened, m))
	_, sub, err := w.subs().Complete(w.ctx, owner, pdid.Id(co.Id))
	w.X.NoError(err)
	w.X.NotNil(sub)
	return sub
}

func (w world) collection(ba pdid.Id) subscriptions.Collection {
	w.t.Helper()
	b, err := w.S.Ent.BillingAccount.Get(w.ctx, ba.Uuid())
	w.X.NoError(err)
	c, err := subscriptions.CollectionOf(w.ctx, w.S.Ent, b)
	w.X.NoError(err)
	return c
}

// take takes the inbox until nothing waits in it that is due.
func (w world) take() {
	w.t.Helper()
	for {
		n, err := w.taker().Take(w.ctx)
		w.X.NoError(err)
		if n == 0 {
			return
		}
	}
}

// heard keeps the webhook of the event of `id` the fake sends, as the
// endpoint does.
func (w world) heard(event, id string) mor.Notice {
	w.t.Helper()
	h, body := w.srv.Webhook(event, id)
	n, err := w.S.Inbox.Channel.Webhook(h, body)
	w.X.NoError(err)
	_, err = w.S.Inbox.Keep(w.ctx, n, levyan.NoticeVia_NOTICE_VIA_WEBHOOK)
	w.X.NoError(err)
	return n
}

// A webhook's body is what the subscription was; what it is now is what the
// merchant's API answers, and an answer older than the one the mirror holds
// -- by the merchant's own clock -- walks nothing back.
func TestTheMirrorIsWhatTheMerchantAnswersAndNeverWalksBack(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")
	m := w.subscribed(a, ba, paddletest.Card)

	// A webhook sent while it was active, delivered once it is canceled.
	stale := w.heard("subscription.updated", m.MorSubscription)
	w.srv.Advance(time.Second)
	w.srv.CancelNow(m.MorSubscription)
	w.take()
	got, err := w.S.Ent.Subscription.Get(w.ctx, m.Id)
	x.NoError(err)
	x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED), got.Status, "the body said active; Paddle says canceled")
	x.NotNil(got.DateCanceled)
	n, err := w.S.Ent.Notice.Query().Where(entnotice.MorEvent(stale.ID)).Only(w.ctx)
	x.NoError(err)
	x.Equal(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_APPLIED), n.Outcome)
	x.Equal(m.Id, n.SubscriptionId)
	x.Equal(ba.Uuid(), n.BillingAccountId)
	x.Equal(levyan.CollectionState_COLLECTION_STATE_CANCELED, w.collection(ba).State)

	// An answer from before, come back late.
	old := mor.Subscription{ID: m.MorSubscription, Customer: m.MorCustomer, Account: ba.String(), Status: mor.Active, Currency: "USD", Updated: m.DateMorUpdated}
	again, err := w.subs().Observe(w.ctx, ba.Uuid(), old, nil)
	x.NoError(err)
	x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED), again.Status, "older than what is held: nothing")
	x.Equal(got.DateUpdated, again.DateUpdated, "not even when it was checked")
	x.Equal(levyan.CollectionState_COLLECTION_STATE_CANCELED, w.collection(ba).State)
}

// Without webhooks, a notice reaches levyan by the pull of the merchant's
// list of its events, into the same inbox: a checkout the payer finished is
// completed from it, and what changed after is mirrored. The pull reads on
// from where it stopped, and past nothing newer than the overlap.
func TestTheMerchantsEventsArePulledIntoTheInbox(t *testing.T) {
	w := setup(t, false)
	x := pdtest.NewX(t)
	x.False(w.S.Webhooks)
	a := levyantest.Account()
	ba := w.billing(a, "KR")

	kept, err := w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Zero(kept, "no checkout begun: nothing of levyan's at the merchant")

	co, opened := w.begun(a, ba)
	x.NoError(w.srv.Pay(opened))
	kept, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Equal(4, kept, "the checkout made, paid, completed, and its subscription")
	ns, err := w.S.Ent.Notice.Query().All(w.ctx)
	x.NoError(err)
	for _, n := range ns {
		x.Equal(int32(levyan.NoticeVia_NOTICE_VIA_EVENTS), n.Via)
	}
	cur, err := w.S.Ent.NoticeCursor.Query().Where(entnoticecursor.Mor("paddle")).Only(w.ctx)
	x.NoError(err)
	x.Empty(cur.MorEvent, "all of it newer than the overlap: read again next time")

	w.take()
	done, err := w.S.Ent.Checkout.Get(w.ctx, co.Id)
	x.NoError(err)
	x.Equal(int32(levyan.CheckoutState_CHECKOUT_STATE_COMPLETED), done.State, "completed by the merchant's notice, the payer never back")
	x.Equal(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE, w.collection(ba).State)

	kept, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Zero(kept, "read again, and kept once")

	w.srv.Advance(subscriptions.Overlap + time.Minute)
	_, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	cur, err = w.S.Ent.NoticeCursor.Get(w.ctx, cur.Id)
	x.NoError(err)
	events := w.srv.Events()
	x.Equal(events[len(events)-1][0], cur.MorEvent, "past what is old enough")

	mirror, err := w.S.Ent.Subscription.Get(w.ctx, done.SubscriptionId)
	x.NoError(err)
	w.srv.ScheduleCancel(mirror.MorSubscription)
	kept, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Equal(1, kept, "what came since, and only that")
	w.take()
	c := w.collection(ba)
	x.Equal(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE, c.State)
	x.False(c.Ends.IsZero(), "canceled at the end of the period")
}

// A notice that came both ways -- a webhook, and the merchant's list -- is
// one row, whichever came first.
func TestAWebhookAndItsEventAreOneNotice(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")
	m := w.subscribed(a, ba, paddletest.Card)

	n := w.heard("subscription.updated", m.MorSubscription)
	kept, err := w.puller().Pull(w.ctx)
	x.NoError(err)
	rows, err := w.S.Ent.Notice.Query().Where(entnotice.MorEvent(n.ID)).All(w.ctx)
	x.NoError(err)
	x.Len(rows, 1)
	x.Equal(int32(levyan.NoticeVia_NOTICE_VIA_WEBHOOK), rows[0].Via, "it came first")
	total, err := w.S.Ent.Notice.Query().Count(w.ctx)
	x.NoError(err)
	x.Equal(len(w.srv.Events()), total, "every event once")
	x.Equal(total-1, kept)

	// And the other way round: listed first, delivered after.
	w.srv.ScheduleCancel(m.MorSubscription)
	kept, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Equal(1, kept)
	events := w.srv.Events()
	h, body := w.srv.Deliver(events[len(events)-1][0])
	got, err := w.S.Inbox.Channel.Webhook(h, body)
	x.NoError(err)
	again, err := w.S.Inbox.Keep(w.ctx, got, levyan.NoticeVia_NOTICE_VIA_WEBHOOK)
	x.NoError(err)
	x.False(again, "the inbox has it")
}

// A notice the merchant lists a little after one it made later is not
// passed over: the pull reads past nothing newer than the overlap.
func TestANoticeListedLateIsPulledAllTheSame(t *testing.T) {
	w := setup(t, false)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")

	w.srv.Behind(5 * time.Minute)
	_, opened := w.begun(a, ba) // transaction.created, listed late
	w.srv.Behind(0)
	w.srv.Advance(time.Second)
	x.NoError(w.srv.Pay(opened))

	kept, err := w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Equal(3, kept, "all but the one not listed yet")
	created := w.srv.Events()[0]
	x.Equal("transaction.created", created[1])
	has, err := w.S.Ent.Notice.Query().Where(entnotice.MorEvent(created[0])).Exist(w.ctx)
	x.NoError(err)
	x.False(has)

	w.srv.Advance(6 * time.Minute)
	kept, err = w.puller().Pull(w.ctx)
	x.NoError(err)
	x.Equal(1, kept, "listed now, and read: the pull had not read past it")
	has, err = w.S.Ent.Notice.Query().Where(entnotice.MorEvent(created[0])).Exist(w.ctx)
	x.NoError(err)
	x.True(has)
}

// A payment that waits for the payer -- a Korean card's renewal they are to
// approve in its app -- leaves the billing account needing their action,
// until they act; and how they pay is the kind of card, its company and its
// last four digits, and no more.
func TestAPaymentThatWaitsForThePayerNeedsTheirAction(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "KR")
	m := w.subscribed(a, ba, paddletest.Method{Type: "south_korea_local_card", Brand: "hyundai", Last4: "1234"})
	x.Equal("south_korea_local_card", m.MethodType)
	x.Equal("hyundai", m.MethodBrand)
	x.Equal("1234", m.MethodLast4)
	x.Equal("KRW", m.Currency)

	w.srv.AwaitAction()
	txn := w.srv.Renew(m.MorSubscription)
	w.heard("transaction.past_due", txn)
	w.heard("subscription.past_due", m.MorSubscription)
	w.take()
	x.Equal(levyan.CollectionState_COLLECTION_STATE_NEEDS_ACTION, w.collection(ba).State)
	got, err := w.S.Ent.Subscription.Get(w.ctx, m.Id)
	x.NoError(err)
	x.Equal(txn, got.LastTransaction)
	x.True(got.AwaitingPayer)

	waited, err := w.S.Inbox.Channel.Transaction(w.ctx, txn)
	x.NoError(err)
	x.True(waited.Awaiting)

	w.srv.Act(txn)
	w.heard("transaction.completed", txn)
	w.heard("subscription.updated", m.MorSubscription)
	w.take()
	x.Equal(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE, w.collection(ba).State)

	// The next renewal paid; and then the answer of the one before, from
	// while it waited, come back late: it walks nothing back.
	next := w.srv.Renew(m.MorSubscription)
	w.heard("transaction.completed", next)
	w.take()
	_, moved, err := w.subs().ObserveTransaction(w.ctx, waited)
	x.NoError(err)
	x.False(moved, "an older transaction's")
	got, err = w.S.Ent.Subscription.Get(w.ctx, m.Id)
	x.NoError(err)
	x.Equal(next, got.LastTransaction)
	x.False(got.AwaitingPayer)
	x.Equal(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE, w.collection(ba).State)

	// A card declined at renewal is past due, and the payer's action is not
	// what it waits for.
	w.srv.Decline("not_enough_balance")
	txn = w.srv.Renew(m.MorSubscription)
	w.heard("transaction.past_due", txn)
	w.heard("subscription.past_due", m.MorSubscription)
	w.take()
	x.Equal(levyan.CollectionState_COLLECTION_STATE_PAST_DUE, w.collection(ba).State)
}

// A transaction a notice names is kept as the merchant answered it, beside
// the notice -- what the collection of a period's amount will bring a
// payment to (#9) -- and handed to what else is to be done with it; one that
// cannot be is taken again.
func TestATransactionsNoticeKeepsWhatTheMerchantAnswered(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")
	m := w.subscribed(a, ba, paddletest.Card)
	w.take()

	txn := w.srv.Renew(m.MorSubscription)
	n := w.heard("transaction.completed", txn)
	var handed []mor.Transaction
	fail := true
	tk := w.taker()
	tk.Transaction = func(ctx context.Context, _ *ent.Notice, t mor.Transaction) error {
		if fail {
			fail = false
			return errors.New("not now")
		}
		handed = append(handed, t)
		return nil
	}
	_, err := tk.Take(w.ctx)
	x.NoError(err)
	row, err := w.S.Ent.Notice.Query().Where(entnotice.MorEvent(n.ID)).Only(w.ctx)
	x.NoError(err)
	x.Nil(row.DateDone, "what else was to be done failed: taken again")
	x.EqualValues(1, row.Tries)
	x.Contains(row.Error, "not now")
	x.NotNil(row.DateRetry)

	w.At(row.DateRetry.Add(time.Second))
	_, err = tk.Take(w.ctx)
	x.NoError(err)
	row, err = w.S.Ent.Notice.Query().Where(entnotice.MorEvent(n.ID)).Only(w.ctx)
	x.NoError(err)
	x.NotNil(row.DateDone)
	x.Len(handed, 1)
	x.Equal(txn, handed[0].ID)
	snap := row.Transaction
	x.NotNil(snap)
	x.Equal("paid", snap.GetStatus())
	x.EqualValues(2, snap.GetProgress())
	x.Equal(m.MorSubscription, snap.GetMorSubscription())
	x.Equal("card", snap.GetMethodType())
	x.NotEmpty(snap.GetLines(), "the plan's line")
	x.Equal(m.Id, row.SubscriptionId)
}

// A notice the merchant cannot be asked about now is taken again later,
// longer after each try; one about what the merchant has no such thing of
// is taken for good; and one about nothing of levyan's is left alone.
func TestANoticeIsTakenAgainUntilTheMerchantAnswers(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")
	m := w.subscribed(a, ba, paddletest.Card)
	w.take()

	n := w.heard("subscription.updated", m.MorSubscription)
	w.srv.Fail(paddletest.Get, paddletest.Fault{Status: 500})
	_, err := w.taker().Take(w.ctx)
	x.NoError(err)
	row, err := w.S.Ent.Notice.Query().Where(entnotice.MorEvent(n.ID)).Only(w.ctx)
	x.NoError(err)
	x.Nil(row.DateDone)
	x.EqualValues(1, row.Tries)
	took, err := w.taker().Take(w.ctx)
	x.NoError(err)
	x.Zero(took, "not due yet")
	w.At(row.DateRetry.Add(time.Second))
	w.take()
	row, err = w.S.Ent.Notice.Query().Where(entnotice.MorEvent(n.ID)).Only(w.ctx)
	x.NoError(err)
	x.NotNil(row.DateDone)

	// A checkout nobody here began, finished at the merchant: foreign.
	co, err := w.S.Inbox.Channel.StartCheckout(w.ctx, mor.Start{Account: "someone-else", Currency: "USD", Success: back})
	x.NoError(err)
	x.NoError(w.srv.Pay(co))
	got, err := w.S.Inbox.Channel.CompleteCheckout(w.ctx, co)
	x.NoError(err)
	f := w.heard("subscription.created", got.ID)
	w.take()
	row, err = w.S.Ent.Notice.Query().Where(entnotice.MorEvent(f.ID)).Only(w.ctx)
	x.NoError(err)
	x.Equal(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_FOREIGN), row.Outcome)
	has, err := w.S.Ent.Subscription.Query().Count(w.ctx)
	x.NoError(err)
	x.Equal(1, has, "no mirror of it")
}

// Two checkouts begun at the same moment, both finished, are one
// subscription too many: the one levyan learns of second is canceled at the
// merchant, and the billing account is collected through the first.
func TestASecondSubscriptionOfABillingAccountIsCanceled(t *testing.T) {
	w := setup(t, true)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := w.billing(a, "US")
	first, opened := w.begun(a, ba)

	// The other of the two, begun beside it.
	co, err := w.S.Inbox.Channel.StartCheckout(w.ctx, mor.Start{Account: ba.String(), Currency: "USD", Success: back})
	x.NoError(err)
	_, err = w.S.Ungated.Checkout().Add(w.ctx, levyan.CheckoutAddRequest_builder{
		Account:        a.Bytes(),
		BillingAccount: levyan.BillingAccountRef_builder{Id: ba.Bytes()}.Build(),
		Mor:            "paddle",
		MorTransaction: co.ID,
		State:          levyan.CheckoutState_CHECKOUT_STATE_OPEN,
	}.Build())
	x.NoError(err)

	x.NoError(w.srv.Pay(opened))
	x.NoError(w.srv.Pay(co))
	_, kept, err := w.subs().Complete(w.ctx, a, pdid.Id(first.Id))
	x.NoError(err)
	w.heard("transaction.completed", co.ID)
	w.take()

	ms, err := w.S.Ent.Subscription.Query().All(w.ctx)
	x.NoError(err)
	x.Len(ms, 2)
	for _, m := range ms {
		if m.Id == kept.Id {
			x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE), m.Status)
		} else {
			x.Equal(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED), m.Status, "canceled at the merchant")
		}
	}
	c := w.collection(ba)
	x.Equal(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE, c.State)
	x.Equal(kept.Id, c.Mirror.Id)
}

// A status and a scheduled change of package mor's are the schema's of the
// same number, as the schema says they are: the API hands the schema's on by
// its number (`server/api`), and a reader of either reads the same.
func TestTheMirrorsEnumsAreTheMerchants(t *testing.T) {
	x := pdtest.NewX(t)
	for v := mor.Active; v <= mor.Canceled; v++ {
		x.Equal(int32(v), int32(levyan.SubscriptionStatus(v)), "%s", v)
		x.NotEmpty(levyan.SubscriptionStatus(v).String())
	}
	x.Len(levyan.SubscriptionStatus_name, int(mor.Canceled)+1)
	for v := mor.CancelAtEnd; v <= mor.Resume; v++ {
		x.Equal(int32(v), int32(levyan.SubscriptionChange(v)), "%s", v)
	}
	x.Len(levyan.SubscriptionChange_name, int(mor.Resume)+1)
}
