package paddle_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
)

// A checkout begun again is the one begun before while Paddle still takes
// it -- the same transaction, opened anew, and nothing made -- so that a
// payer who begins twice subscribes once; one Paddle canceled is begun anew,
// and one the payer finished meanwhile is to be completed instead.
func TestACheckoutBegunAgainIsTheSameWhileItIsOpen(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()

	co, err := ch.StartCheckout(ctx, start)
	require.NoError(t, err)
	made := srv.Requests(paddletest.Checkout)

	again := start
	again.Again = &co
	again.Success = "https://pay.example.com/back"
	same, err := ch.StartCheckout(ctx, again)
	require.NoError(t, err)
	require.Equal(t, co.ID, same.ID)
	require.Equal(t, co.Window.URL, same.Window.URL)
	require.Equal(t, "https://pay.example.com/back", same.Window.Values["successUrl"], "sent back where this page says")
	require.Equal(t, made, srv.Requests(paddletest.Checkout), "nothing made")

	theirs := again
	theirs.Account = "acct-0002"
	_, err = ch.StartCheckout(ctx, theirs)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "another account's checkout is no checkout of this one's")

	srv.Abandon(co.ID)
	anew, err := ch.StartCheckout(ctx, again)
	require.NoError(t, err)
	require.NotEqual(t, co.ID, anew.ID, "Paddle takes it no longer")

	require.NoError(t, srv.Pay(anew))
	again.Again = &anew
	_, err = ch.StartCheckout(ctx, again)
	require.Equal(t, mor.Invalid, mor.KindOf(err))
	require.ErrorIs(t, err, mor.ErrFinished)
}

// A payment says what it was made with -- the kind, a card's brand or a
// Korean card's company, its last four digits, and nothing more of it -- and
// whether it waits for the payer to approve it before Paddle can collect it.
func TestAPaymentSaysHowAndWhetherItWaitsForThePayer(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()

	kr := start
	kr.Currency = "KRW"
	co, err := ch.StartCheckout(ctx, kr)
	require.NoError(t, err)
	local := paddletest.Method{Type: "south_korea_local_card", Brand: "hyundai", Last4: "1234"}
	require.NoError(t, srv.PayWith(co, local))
	s, err := ch.CompleteCheckout(ctx, co)
	require.NoError(t, err)
	require.Equal(t, &mor.Method{Type: "south_korea_local_card", Brand: "hyundai", Last4: "1234"}, s.Method)

	paid, err := ch.Transaction(ctx, co.ID)
	require.NoError(t, err)
	require.Equal(t, s.Method, paid.Method)
	require.False(t, paid.Awaiting)

	srv.AwaitAction()
	txn := srv.Renew(s.ID)
	waits, err := ch.Transaction(ctx, txn)
	require.NoError(t, err)
	require.Equal(t, mor.Pending, waits.Status)
	require.True(t, waits.Awaiting, "the payer's to approve")
	require.Equal(t, s.Method, waits.Method)
	sub, err := ch.Subscription(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, mor.PastDue, sub.Status)

	srv.Act(txn)
	done, err := ch.Transaction(ctx, txn)
	require.NoError(t, err)
	require.Equal(t, mor.Paid, done.Status)
	require.False(t, done.Awaiting)
	require.Greater(t, done.Progress(), waits.Progress())
}

// The event stream is read into the notices its webhooks are read into, by
// the same ids and oldest first, a page at a time: after the last read, or
// since a time for a first read.
func TestTheEventStreamIsReadAsItsWebhooksAre(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	began := srv.Now()

	co, err := ch.StartCheckout(ctx, start)
	require.NoError(t, err)
	require.NoError(t, srv.Pay(co))
	s, err := ch.CompleteCheckout(ctx, co)
	require.NoError(t, err)

	got, err := ch.Notices(ctx, mor.Read{Since: began})
	require.NoError(t, err)
	require.False(t, got.More)
	var types []string
	for _, n := range got.Notices {
		types = append(types, n.Event)
	}
	require.Equal(t, []string{"transaction.created", "transaction.paid", "transaction.completed", "subscription.created"}, types)
	require.Equal(t, mor.NoticeTransaction, got.Notices[2].Type)
	require.Equal(t, co.ID, got.Notices[2].Transaction)
	require.Equal(t, s.ID, got.Notices[2].Subscription)
	require.Equal(t, mor.NoticeSubscription, got.Notices[3].Type)
	require.Equal(t, s.ID, got.Notices[3].Subscription)
	require.Equal(t, got.Notices[3].ID, got.Last)

	// A webhook of one of them is the same notice.
	h, body := srv.Deliver(got.Notices[3].ID)
	n, err := ch.Webhook(h, body)
	require.NoError(t, err)
	require.Equal(t, got.Notices[3], n)

	none, err := ch.Notices(ctx, mor.Read{After: got.Last})
	require.NoError(t, err)
	require.Empty(t, none.Notices, "nothing since")

	srv.ScheduleCancel(s.ID)
	next, err := ch.Notices(ctx, mor.Read{After: got.Last})
	require.NoError(t, err)
	require.Len(t, next.Notices, 1)
	require.Equal(t, "subscription.updated", next.Notices[0].Event)

	later, err := ch.Notices(ctx, mor.Read{Since: srv.Now().Add(time.Minute)})
	require.NoError(t, err)
	require.Empty(t, later.Notices, "nothing happened since then")

	_, err = ch.Notices(ctx, mor.Read{After: "evt_1"})
	require.Equal(t, mor.Invalid, mor.KindOf(err), "no event of Paddle's")
}

// The stream is read a page at a time, and a page says there is more.
func TestTheEventStreamIsReadAPageAtATime(t *testing.T) {
	srv, ch := setup(t)
	srv.EventsPageSize = 3
	ctx := t.Context()
	began := srv.Now()

	co, err := ch.StartCheckout(ctx, start)
	require.NoError(t, err)
	require.NoError(t, srv.Pay(co))
	s, err := ch.CompleteCheckout(ctx, co)
	require.NoError(t, err)
	srv.ScheduleCancel(s.ID)

	first, err := ch.Notices(ctx, mor.Read{Since: began})
	require.NoError(t, err)
	require.Len(t, first.Notices, 3)
	require.True(t, first.More)
	rest, err := ch.Notices(ctx, mor.Read{After: first.Last})
	require.NoError(t, err)
	require.Len(t, rest.Notices, 2)
	require.False(t, rest.More)

	var all []string
	for _, n := range append(first.Notices, rest.Notices...) {
		all = append(all, n.ID)
	}
	var recorded []string
	for _, e := range srv.Events() {
		recorded = append(recorded, e[0])
	}
	require.Equal(t, recorded, all, "every event, once, in order")
}
