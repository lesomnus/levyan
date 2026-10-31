package paddle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
)

func usd(n int64) money.Amount { return money.Amount{Minor: n, Currency: "USD"} }

const account = "acct-0001"

var start = mor.Start{Account: account, Success: "https://pay.example.com/subscribed"}

func setup(t *testing.T) (*paddletest.Server, *paddle.Channel) {
	t.Helper()
	srv := paddletest.New(t)
	ch, err := paddle.New(srv.Config())
	require.NoError(t, err)
	return srv, ch
}

// subscribe is a payer starting a subscription through Paddle's checkout.
func subscribe(t *testing.T, srv *paddletest.Server, ch *paddle.Channel) mor.Subscription {
	t.Helper()
	co, err := ch.StartCheckout(t.Context(), start)
	require.NoError(t, err)
	require.NoError(t, srv.Pay(co))
	s, err := ch.CompleteCheckout(t.Context(), co)
	require.NoError(t, err)
	return s
}

func now(s mor.Subscription, key string, n int64) mor.Charge {
	return mor.Charge{Subscription: s.ID, Key: key, When: mor.Now, Amount: usd(n), Description: "loft 2026-10"}
}

func next(s mor.Subscription, key string, n int64) mor.Charge {
	c := now(s, key, n)
	c.When = mor.NextPeriod
	return c
}

func TestACheckoutStartsASubscriptionForItsAccount(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()

	co, err := ch.StartCheckout(ctx, start)
	require.NoError(t, err)
	require.Equal(t, account, co.Account)
	require.Equal(t, paddle.SDK, co.Window.Script)
	require.Equal(t, map[string]string{
		"token":         srv.ClientToken,
		"environment":   "sandbox",
		"transactionId": co.ID,
		"successUrl":    start.Success,
	}, co.Window.Values)
	require.Equal(t, srv.URL+"/pay?_ptxn="+co.ID, co.Window.URL, "the payment link, for a page that sends the payer to Paddle")

	_, err = ch.CompleteCheckout(ctx, co)
	require.Equal(t, mor.NotFound, mor.KindOf(err), "not paid yet")
	require.ErrorIs(t, err, mor.ErrOpen)

	require.NoError(t, srv.Pay(co))
	s, err := ch.CompleteCheckout(ctx, co)
	require.NoError(t, err)
	require.NotEmpty(t, s.ID)
	require.NotEmpty(t, s.Customer)
	require.Equal(t, account, s.Account, "Paddle keeps the account on the subscription")
	require.Equal(t, mor.Active, s.Status)
	require.Equal(t, "USD", s.Currency)
	require.True(t, s.Renews())
	require.False(t, s.Next.IsZero())
	require.Equal(t, s.Period.End, s.Next)

	again, err := ch.CompleteCheckout(ctx, co)
	require.NoError(t, err)
	require.Equal(t, s, again, "the payer reloading the page is the same subscription")

	got, err := ch.Subscription(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, s, got)
}

// A checkout is completed by levyan's own record of it: one started for an
// account completes for no other, and a page cannot hand over another's.
func TestACheckoutIsCompletedOnlyForItsOwnAccount(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	co, err := ch.StartCheckout(ctx, start)
	require.NoError(t, err)
	require.NoError(t, srv.Pay(co))

	other := co
	other.Account = "acct-0002"
	_, err = ch.CompleteCheckout(ctx, other)
	require.Equal(t, mor.Invalid, mor.KindOf(err))

	theirs, err := ch.StartCheckout(ctx, mor.Start{Account: "acct-0002", Success: start.Success})
	require.NoError(t, err)
	mixed := co
	mixed.ID = theirs.ID
	_, err = ch.CompleteCheckout(ctx, mixed)
	require.Equal(t, mor.Invalid, mor.KindOf(err))

	wrong := co
	wrong.Window.Values = map[string]string{"token": "test_another", "environment": "sandbox", "transactionId": co.ID}
	require.Error(t, srv.Pay(wrong), "a checkout opened with another token is no checkout of Paddle's")
}

func TestAChargeMadeNowIsCollectedAndFoundByItsKey(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Paid, tx.Status)
	require.NotEmpty(t, tx.ID)
	require.Equal(t, s.ID, tx.Subscription)
	require.Equal(t, s.Customer, tx.Customer)
	require.False(t, tx.At.IsZero())
	require.Nil(t, tx.Failure)
	l, ok := tx.Line("stmt-0001")
	require.True(t, ok)
	require.NotEmpty(t, l.ID)
	require.Equal(t, "loft 2026-10", l.Name)
	require.Equal(t, usd(15_000), l.Amount, "what levyan computed: Paddle's tax goes on top")
	require.Equal(t, usd(1_500), l.Tax)
	require.Equal(t, usd(16_500), l.Total)
	require.Equal(t, usd(16_500), tx.Total)
	require.Equal(t, 1, srv.Charged())

	got, err := ch.Charged(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, tx, got)
	got, err = ch.Transaction(ctx, tx.ID)
	require.NoError(t, err)
	require.Equal(t, tx, got)

	// Sent again, the key is found and nothing is sent: Paddle keeps no
	// key, so the adapter looks first.
	again, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, tx, again)
	require.Equal(t, 1, srv.Requests(paddletest.Charge))
	require.Equal(t, 1, srv.Charged())

	_, err = ch.Charge(ctx, now(s, "stmt-0001", 20_000))
	require.Equal(t, mor.Invalid, mor.KindOf(err), "a key charged for another amount")
	require.Equal(t, 1, srv.Charged())

	_, err = ch.Charged(ctx, now(s, "stmt-0002", 15_000))
	require.Equal(t, mor.NotFound, mor.KindOf(err), "a key never sent was never charged")
}

func TestAChargeForTheNextPeriodIsBilledWithTheRenewal(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	tx, err := ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Scheduled, tx.Status)
	require.Empty(t, tx.ID, "in no transaction yet")
	l, ok := tx.Line("stmt-0001")
	require.True(t, ok)
	require.Empty(t, l.ID)
	require.Equal(t, usd(15_000), l.Amount)
	require.Equal(t, 0, srv.Charged(), "nothing collected yet")

	got, err := ch.Charged(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Scheduled, got.Status)

	again, err := ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Scheduled, again.Status)
	require.Equal(t, 1, srv.Requests(paddletest.Charge), "found where it waits, and not sent again")

	renewal := srv.Renew(s.ID)
	require.NotEmpty(t, renewal)
	got, err = ch.Charged(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Paid, got.Status)
	require.Equal(t, renewal, got.ID, "the renewal's transaction")
	require.Len(t, got.Lines, 2, "the plan, and the charge")
	l, ok = got.Line("stmt-0001")
	require.True(t, ok)
	require.NotEmpty(t, l.ID)
	require.Equal(t, usd(15_000), l.Amount)
	require.Less(t, tx.Progress(), got.Progress())

	again, err = ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, got, again, "billed, and found where it was billed")
	require.Equal(t, 1, srv.Requests(paddletest.Charge))
	require.Equal(t, 1, srv.Charged())
}

// levyan#1's gotcha: a subscription scheduled to cancel at the end of its
// period has no next period, and Paddle forgives what was to be billed in it.
// A charge for it is refused before it is sent, and one already scheduled is
// found nowhere once the subscription will not renew -- so it is charged now,
// under its key.
func TestAChargeForTheNextPeriodOfASubscriptionThatEndsIsNeverLeftToBeDropped(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	_, err := ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)

	// The payer cancels in Paddle's own pages, which levyan learns of from
	// the subscription's webhook.
	srv.ScheduleCancel(s.ID)
	h, body := srv.Webhook("subscription.updated", s.ID)
	n, err := ch.Webhook(h, body)
	require.NoError(t, err)
	r, err := ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.False(t, r.Subscription.Renews())
	require.Equal(t, mor.CancelAtEnd, r.Subscription.Change.Kind)
	require.True(t, r.Subscription.Next.IsZero())

	_, err = ch.Charged(ctx, next(s, "stmt-0001", 15_000))
	require.Equal(t, mor.NotFound, mor.KindOf(err), "dropped with the period that will not come")

	_, err = ch.Charge(ctx, next(s, "stmt-0002", 15_000))
	require.Equal(t, mor.Invalid, mor.KindOf(err))
	require.ErrorIs(t, err, mor.ErrNoNextPeriod)
	require.Equal(t, 1, srv.Requests(paddletest.Charge), "refused before it is sent")

	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err, "the same key, now")
	require.Equal(t, mor.Paid, tx.Status)

	require.Empty(t, srv.Renew(s.ID), "the cancellation takes effect: no renewal")
	ended, err := ch.Subscription(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, mor.Canceled, ended.Status)
	require.False(t, ended.Canceled.IsZero())
	require.Equal(t, 1, srv.Charged(), "the period was billed once, and now")

	_, err = ch.Charge(ctx, now(s, "stmt-0003", 15_000))
	require.ErrorIs(t, err, mor.ErrEnded)
}

// levyan asking for the cancellation at the end of the period is the same: a
// charge for the next period is refused from then on.
func TestACancellationAtTheEndOfThePeriodClosesTheNextPeriodToCharges(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	ended, err := ch.Cancel(ctx, mor.Cancel{Subscription: s.ID})
	require.NoError(t, err)
	require.Equal(t, mor.Active, ended.Status, "until the end of its period")
	require.Equal(t, mor.CancelAtEnd, ended.Change.Kind)
	require.Equal(t, s.Next, ended.Change.At)
	require.False(t, ended.Renews())

	again, err := ch.Cancel(ctx, mor.Cancel{Subscription: s.ID})
	require.NoError(t, err, "scheduled already: what was asked")
	require.Equal(t, ended.Change, again.Change)

	_, err = ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.ErrorIs(t, err, mor.ErrNoNextPeriod)
	require.Zero(t, srv.Requests(paddletest.Charge))

	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Paid, tx.Status)

	gone, err := ch.Cancel(ctx, mor.Cancel{Subscription: s.ID, Now: true})
	require.NoError(t, err)
	require.Equal(t, mor.Canceled, gone.Status)
	gone, err = ch.Cancel(ctx, mor.Cancel{Subscription: s.ID, Now: true})
	require.NoError(t, err, "canceled already")
	require.Equal(t, mor.Canceled, gone.Status)
}

// The payment failing is not the request failing: a charge the card declined
// is a transaction, failed, and no error.
func TestADeclinedChargeIsAFailedTransactionAndNoError(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	srv.Decline("not_enough_balance")
	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Failed, tx.Status)
	require.Equal(t, mor.Declined, tx.Failure.Kind)
	require.Equal(t, "subscription_payment_declined", tx.Failure.Code)
	l, ok := tx.Line("stmt-0001")
	require.True(t, ok)
	require.Equal(t, usd(15_000), l.Amount)
	require.Equal(t, 0, srv.Charged())

	// Tried again, it is another charge: another key.
	tx, err = ch.Charge(ctx, now(s, "stmt-0001-2", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Paid, tx.Status)
	require.Equal(t, 1, srv.Charged())
}

// A renewal the card declines is Paddle's to retry: the transaction is
// pending with why, and the subscription past due takes no more charges.
func TestARenewalTheCardDeclinesIsPastDue(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	_, err := ch.Charge(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)

	srv.Decline("expired_card")
	srv.Renew(s.ID)
	tx, err := ch.Charged(ctx, next(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, mor.Pending, tx.Status, "Paddle is still collecting it")
	require.Equal(t, &mor.Failure{Kind: mor.Declined, Code: "expired_card"}, tx.Failure)

	_, err = ch.Charge(ctx, now(s, "stmt-0002", 15_000))
	require.Equal(t, mor.Invalid, mor.KindOf(err))
	require.ErrorIs(t, err, mor.ErrPastDue)
}

// Paddle charged the card and its answer was lost (levyan#1, scenario 5): the
// charge is unknown, a look too soon is unknown too, and once the look may
// be believed the charge is found -- and sent again it is that charge, not
// another.
func TestAnAnswerLostAfterTheChargeIsUnknownUntilTheKeyIsFound(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	c := now(s, "stmt-0001", 15_000)
	srv.Fail(paddletest.Charge, paddletest.Fault{Status: 500, Code: "internal_error", After: true})
	_, err := ch.Charge(ctx, c)
	require.Equal(t, mor.Unknown, mor.KindOf(err))
	require.Equal(t, 1, srv.Charged(), "charged all the same")
	c.Sent = srv.Now()

	tx, err := ch.Charged(ctx, c)
	require.NoError(t, err, "found, so believed, however soon")
	require.Equal(t, mor.Paid, tx.Status)

	again, err := ch.Charge(ctx, c)
	require.NoError(t, err)
	require.Equal(t, tx, again)
	require.Equal(t, 1, srv.Requests(paddletest.Charge))
	require.Equal(t, 1, srv.Charged())
}

// A timeout, and the charge still on its way into Paddle's records when the
// client gave up: a look sees nothing, and nothing is believed of that until
// Settle has passed -- the one window a look-first adapter cannot see into.
func TestATimeoutThenAQueryWaitsForTheChargeToSettle(t *testing.T) {
	srv, ch := setup(t)
	s := subscribe(t, srv, ch)

	c := now(s, "stmt-0001", 15_000)
	srv.Fail(paddletest.Charge, paddletest.Fault{Hang: true, Unseen: 3 * time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := ch.Charge(ctx, c)
	require.Equal(t, mor.Unknown, mor.KindOf(err))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	c.Sent = srv.Now()

	_, err = ch.Charged(t.Context(), c)
	require.Equal(t, mor.Unknown, mor.KindOf(err), "not seen is not not made")
	require.ErrorIs(t, err, mor.ErrTooSoon)

	_, err = ch.Charge(t.Context(), c)
	require.ErrorIs(t, err, mor.ErrTooSoon)
	require.Equal(t, 1, srv.Requests(paddletest.Charge), "not sent again while it may be on its way")

	srv.Advance(mor.Settle)
	tx, err := ch.Charged(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, mor.Paid, tx.Status)

	tx2, err := ch.Charge(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, tx, tx2)
	require.Equal(t, 1, srv.Requests(paddletest.Charge))
	require.Equal(t, 1, srv.Charged())
}

// A 5xx that came before Paddle did anything is as unknown as one after:
// only once the key may be believed absent is it charged again, under the
// same key.
func TestA5xxBeforeTheChargeIsChargedAgainOnceItHasSettled(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	c := now(s, "stmt-0001", 15_000)
	srv.Fail(paddletest.Charge, paddletest.Fault{Status: http.StatusBadGateway, Code: "bad_gateway"})
	_, err := ch.Charge(ctx, c)
	require.Equal(t, mor.Unknown, mor.KindOf(err))
	c.Sent = srv.Now()

	_, err = ch.Charged(ctx, c)
	require.ErrorIs(t, err, mor.ErrTooSoon)

	srv.Advance(mor.Settle)
	_, err = ch.Charged(ctx, c)
	require.Equal(t, mor.NotFound, mor.KindOf(err), "never charged")

	tx, err := ch.Charge(ctx, c)
	require.NoError(t, err, "the same key, sent once it is known not to be there")
	require.Equal(t, mor.Paid, tx.Status)
	require.Equal(t, 1, srv.Charged())
}

// A key charged twice -- by a caller that sent it again before Settle -- is
// found twice, and said: the first is the charge, and the rest are to give
// back.
func TestAKeyChargedTwiceIsSaid(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.NoError(t, err)
	srv.Duplicate(tx.ID)

	got, err := ch.Charged(ctx, now(s, "stmt-0001", 15_000))
	require.ErrorIs(t, err, mor.ErrTwice)
	require.Equal(t, tx.ID, got.ID, "the first of them")

	_, err = ch.Charge(ctx, now(s, "stmt-0001", 15_000))
	require.ErrorIs(t, err, mor.ErrTwice)
	require.Equal(t, 1, srv.Requests(paddletest.Charge))
}

// A key is looked for through every page of a subscription's transactions,
// and its absence said only once every page is looked through.
func TestAKeyIsLookedForOnEveryPage(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	srv.PageSize = 2
	s := subscribe(t, srv, ch)

	for i := range 5 {
		_, err := ch.Charge(ctx, now(s, fmt.Sprintf("stmt-%04d", i), 1_000+int64(i)))
		require.NoError(t, err)
	}
	for i := range 5 {
		tx, err := ch.Charged(ctx, now(s, fmt.Sprintf("stmt-%04d", i), 1_000+int64(i)))
		require.NoError(t, err)
		l, _ := tx.Line(fmt.Sprintf("stmt-%04d", i))
		require.Equal(t, usd(1_000+int64(i)), l.Amount)
	}
	_, err := ch.Charged(ctx, now(s, "stmt-9999", 1_000))
	require.Equal(t, mor.NotFound, mor.KindOf(err))
}

func TestARefundIsAskedOnceAndApprovedByPaddle(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 10_000))
	require.NoError(t, err)
	l, _ := tx.Line("stmt-0001")

	r := mor.Refund{Transaction: tx.ID, Line: l.ID, Amount: usd(3_000), Reason: "정정 청구", Key: "refund-0001"}
	a, err := ch.Refund(ctx, r)
	require.NoError(t, err)
	require.Equal(t, mor.RefundAdjustment, a.Kind)
	require.Equal(t, mor.Requested, a.Status, "Paddle reviews most refunds")
	require.Equal(t, usd(3_300), a.Amount, "levyan's amount, and Paddle's tax on it")
	require.Equal(t, "정정 청구", a.Reason)
	require.Equal(t, "refund-0001", a.Key)
	require.Equal(t, tx.ID, a.Transaction)

	again, err := ch.Refund(ctx, r)
	require.NoError(t, err)
	require.Equal(t, a, again, "found, and not sent again")
	require.Equal(t, 1, srv.Requests(paddletest.Refund))

	_, err = ch.Refund(ctx, mor.Refund{Transaction: tx.ID, Line: l.ID, Amount: usd(1_000), Reason: "정정 청구", Key: "refund-0002"})
	require.Equal(t, mor.RateLimited, mor.KindOf(err), "one refund waits for approval: later")

	srv.Approve(a.ID)
	h, body := srv.Webhook("adjustment.updated", a.ID)
	n, err := ch.Webhook(h, body)
	require.NoError(t, err)
	require.Equal(t, mor.NoticeTransaction, n.Type)
	require.Equal(t, tx.ID, n.Transaction)
	require.Equal(t, a.ID, n.Adjustment)
	rc, err := ch.Recheck(ctx, n)
	require.NoError(t, err)
	approved, ok := rc.Transaction.Adjustment("refund-0001")
	require.True(t, ok)
	require.Equal(t, mor.Approved, approved.Status)
	require.Less(t, tx.Progress(), rc.Transaction.Progress())

	got, err := ch.Refunded(ctx, r)
	require.NoError(t, err)
	require.Equal(t, approved, got)

	_, err = ch.Refund(ctx, mor.Refund{Transaction: tx.ID, Line: l.ID, Amount: usd(8_000), Reason: "정정 청구", Key: "refund-0002"})
	require.Equal(t, mor.Invalid, mor.KindOf(err), "more than is left")

	rest, err := ch.Refund(ctx, mor.Refund{Transaction: tx.ID, Line: l.ID, All: true, Reason: "해지", Key: "refund-0003"})
	require.NoError(t, err)
	require.Equal(t, usd(11_000-3_300), rest.Amount, "all of what is left of the line")

	_, err = ch.Refunded(ctx, mor.Refund{Transaction: tx.ID, Key: "refund-9999"})
	require.Equal(t, mor.NotFound, mor.KindOf(err))

	for _, bad := range []mor.Refund{
		{Transaction: tx.ID, Line: l.ID, Reason: "정정 청구", Key: "k"},                            // no amount, and not all
		{Transaction: tx.ID, Line: l.ID, Amount: usd(1), All: true, Reason: "정정 청구", Key: "k"}, // both
		{Transaction: tx.ID, Amount: usd(1_000), Reason: "정정 청구", Key: "k"},                    // an amount of no line
		{Transaction: tx.ID, Line: l.ID, Amount: usd(1_000), Reason: "정정 청구"},                  // no key
		{Transaction: tx.ID, Line: l.ID, Amount: usd(1_000), Reason: " ", Key: "k"},            // no reason
		{Transaction: "txn_nope", Line: l.ID, Amount: usd(1_000), Reason: "정정 청구", Key: "k"},
	} {
		_, err := ch.Refund(ctx, bad)
		require.Equal(t, mor.Invalid, mor.KindOf(err), "%+v", bad)
	}
}

// A refund whose answer was lost is found by its key once it may be, and
// never asked twice.
func TestARefundWhoseAnswerWasLostIsFoundByItsKey(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 10_000))
	require.NoError(t, err)

	r := mor.Refund{Transaction: tx.ID, All: true, Reason: "해지", Key: "refund-0001"}
	srv.Fail(paddletest.Refund, paddletest.Fault{Hang: true, Unseen: time.Minute})
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = ch.Refund(short, r)
	require.Equal(t, mor.Unknown, mor.KindOf(err))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	r.Sent = srv.Now()

	_, err = ch.Refunded(ctx, r)
	require.ErrorIs(t, err, mor.ErrTooSoon)
	_, err = ch.Refund(ctx, r)
	require.ErrorIs(t, err, mor.ErrTooSoon)

	srv.Advance(mor.Settle)
	a, err := ch.Refunded(ctx, r)
	require.NoError(t, err)
	require.Equal(t, usd(11_000), a.Amount, "all of the transaction")
	again, err := ch.Refund(ctx, r)
	require.NoError(t, err)
	require.Equal(t, a, again)
	require.Equal(t, 1, srv.Requests(paddletest.Refund))
}

func TestAWebhookIsBelievedOnlyAsTheAPIAnswersIt(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	tx, err := ch.Charge(ctx, now(s, "stmt-0001", 10_000))
	require.NoError(t, err)

	// A webhook's data is Paddle's snapshot when the event happened: signed,
	// and still not what the transaction is now.
	stale, err := json.Marshal(map[string]any{
		"event_id":        "evt_" + strings.Repeat("0", 26),
		"event_type":      "transaction.created",
		"occurred_at":     "2026-10-31T12:00:00.000000Z",
		"notification_id": "ntf_" + strings.Repeat("0", 26),
		"data":            map[string]any{"id": tx.ID, "status": "draft", "subscription_id": s.ID},
	})
	require.NoError(t, err)
	n, err := ch.Webhook(srv.Sign(stale, srv.Now()), stale)
	require.NoError(t, err)
	require.Equal(t, mor.NoticeTransaction, n.Type)
	require.Equal(t, "transaction.created", n.Event)
	require.Equal(t, tx.ID, n.Transaction)
	require.Equal(t, s.ID, n.Subscription)
	require.Equal(t, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), n.At)

	r, err := ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.Equal(t, tx, *r.Transaction, "paid, as the API says, and not a draft, as the webhook did")
	require.Nil(t, r.Subscription)

	h, body := srv.Webhook("subscription.updated", s.ID)
	n, err = ch.Webhook(h, body)
	require.NoError(t, err)
	require.Equal(t, mor.NoticeSubscription, n.Type)
	r, err = ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.Equal(t, s.ID, r.Subscription.ID)

	for _, body := range []string{
		`{"event_id":"evt_` + strings.Repeat("1", 26) + `","event_type":"customer.updated","data":{"id":"ctm_` + strings.Repeat("1", 26) + `"}}`,
		`{"event_id":"evt_` + strings.Repeat("2", 26) + `","event_type":"payment_method.saved","data":{}}`,
	} {
		n, err := ch.Webhook(srv.Sign([]byte(body), srv.Now()), []byte(body))
		require.NoError(t, err)
		require.Equal(t, mor.NoticeOther, n.Type, "answered, and dropped")
		_, err = ch.Recheck(ctx, n)
		require.Equal(t, mor.Invalid, mor.KindOf(err))
	}
	for _, body := range []string{
		`not json`,
		`{"event_type":"transaction.completed","data":{"id":"txn_` + strings.Repeat("3", 26) + `"}}`, // no event
		`{"event_id":"evt_` + strings.Repeat("4", 26) + `","event_type":"transaction.completed","data":{}}`,
		`{"event_id":"evt_` + strings.Repeat("5", 26) + `","event_type":"adjustment.updated","data":{"id":"adj_` + strings.Repeat("5", 26) + `"}}`,
		`{"event_id":"evt_` + strings.Repeat("6", 26) + `","event_type":"subscription.updated","data":{"id":"sub_x/../.."}}`,
	} {
		_, err := ch.Webhook(srv.Sign([]byte(body), srv.Now()), []byte(body))
		require.Equal(t, mor.Invalid, mor.KindOf(err), body)
	}
}

func TestAWebhookWhoseSignatureDoesNotHoldIsRefused(t *testing.T) {
	srv, ch := setup(t)
	s := subscribe(t, srv, ch)
	h, body := srv.Webhook("subscription.updated", s.ID)
	_, err := ch.Webhook(h, body)
	require.NoError(t, err)

	tampered := bytes.Replace(body, []byte(`"active"`), []byte(`"canceled"`), 1)
	require.NotEqual(t, body, tampered)
	_, err = ch.Webhook(h, tampered)
	require.Equal(t, mor.Invalid, mor.KindOf(err))

	moved := h.Clone()
	sig := moved.Get(paddle.SignatureHeader)
	ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "ts="), ";")
	moved.Set(paddle.SignatureHeader, strings.Replace(sig, "ts="+ts, fmt.Sprintf("ts=%d", srv.Now().Unix()+1), 1))
	_, err = ch.Webhook(moved, body)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "the time is signed with the body")

	for name, v := range map[string]string{
		"none":         "",
		"no time":      "h1=" + strings.Repeat("0", 64),
		"no signature": "ts=1",
		"not a header": "v1,whatever",
		"two times":    "ts=1;ts=2;" + strings.TrimPrefix(sig, "ts="+ts+";"),
	} {
		bad := h.Clone()
		bad.Set(paddle.SignatureHeader, v)
		if v == "" {
			bad.Del(paddle.SignatureHeader)
		}
		_, err := ch.Webhook(bad, body)
		require.Equal(t, mor.Invalid, mor.KindOf(err), name)
	}

	c := srv.Config()
	c.WebhookSecret = paddletest.Secret("pdl_ntfset_another")
	other, err := paddle.New(c)
	require.NoError(t, err)
	_, err = other.Webhook(h, body)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "under another secret")

	c.WebhookSecret = paddletest.Secret("")
	none, err := paddle.New(c)
	require.NoError(t, err)
	_, err = none.Webhook(h, body)
	require.Equal(t, mor.Unknown, mor.KindOf(err), "levyan's own failure, which Paddle sends again")
}

// While Paddle rotates a secret, a webhook carries a signature under each:
// one that holds is enough.
func TestAWebhookSignedUnderTwoSecretsHoldsUnderEither(t *testing.T) {
	srv, ch := setup(t)
	s := subscribe(t, srv, ch)
	h, body := srv.Webhook("subscription.updated", s.ID)
	sig := h.Get(paddle.SignatureHeader)
	ts, h1, _ := strings.Cut(sig, ";")

	rotating := h.Clone()
	rotating.Set(paddle.SignatureHeader, ts+";h1="+strings.Repeat("ab", 32)+";"+h1)
	n, err := ch.Webhook(rotating, body)
	require.NoError(t, err)
	require.Equal(t, mor.NoticeSubscription, n.Type)
}

// A delivery replayed later is refused, for the time it was signed at is
// signed too; a delivery Paddle sends again is signed again, and is the same
// notice -- as is a replay inside the tolerance, which the inbox keeps once.
func TestAReplayedOrOldWebhookIsRefused(t *testing.T) {
	srv, ch := setup(t)
	s := subscribe(t, srv, ch)
	h, body := srv.Webhook("subscription.updated", s.ID)
	first, err := ch.Webhook(h, body)
	require.NoError(t, err)

	replay, err := ch.Webhook(h, body)
	require.NoError(t, err)
	require.Equal(t, first.ID, replay.ID, "inside the tolerance: the same event, which the inbox has")

	srv.Advance(paddle.Tolerance + time.Second)
	_, err = ch.Webhook(h, body)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "replayed after the tolerance")

	again, err := ch.Webhook(srv.Sign(body, srv.Now()), body)
	require.NoError(t, err, "Paddle sending it again signs it again")
	require.Equal(t, first.ID, again.ID)

	_, err = ch.Webhook(srv.Sign(body, srv.Now().Add(time.Minute)), body)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "signed in the future is no fresher")

	c := srv.Config()
	c.Tolerance = time.Hour
	lenient, err := paddle.New(c)
	require.NoError(t, err)
	_, err = lenient.Webhook(h, body)
	require.NoError(t, err, "a tolerance of the deployment's own")
}

func TestPaddleRefusingLevyanIsAuthAndToldToWaitIsRateLimited(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)

	c := srv.Config()
	c.APIKey = paddletest.Secret("pdl_sdbx_apikey_not-the-key")
	wrong, err := paddle.New(c)
	require.NoError(t, err)
	_, err = wrong.Subscription(ctx, s.ID)
	require.Equal(t, mor.Auth, mor.KindOf(err))

	srv.Fail(paddletest.Get, paddletest.Fault{Status: http.StatusTooManyRequests, Code: "too_many_requests"})
	_, err = ch.Subscription(ctx, s.ID)
	require.Equal(t, mor.RateLimited, mor.KindOf(err))

	srv.Fail(paddletest.Get, paddletest.Fault{Status: http.StatusForbidden, Code: "forbidden"})
	_, err = ch.Subscription(ctx, s.ID)
	require.Equal(t, mor.Auth, mor.KindOf(err), "a key without the permission, or of the other environment")

	_, err = ch.Subscription(ctx, "sub_"+strings.Repeat("9", 26))
	require.Equal(t, mor.NotFound, mor.KindOf(err))

	_, err = ch.Charge(ctx, now(s, "stmt-0001", 50))
	require.Equal(t, mor.Invalid, mor.KindOf(err), "Paddle charges no less than 0.70 USD")
	require.ErrorIs(t, err, mor.ErrBelowMinimum)
	var e *mor.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "subscription_update_transaction_balance_less_than_charge_limit", e.Code)
	require.NotEmpty(t, e.Request, "Paddle's request id, which its support asks for")

	srv.TaxCategories = nil
	_, err = ch.Charge(ctx, now(s, "stmt-0002", 1_000))
	require.Equal(t, mor.Auth, mor.KindOf(err), "a tax category the account has not approved is an operator's to fix")

	srv.Advance(s.Next.Sub(srv.Now()) - 10*time.Minute)
	_, err = ch.Charge(ctx, now(s, "stmt-0003", 1_000))
	require.Equal(t, mor.RateLimited, mor.KindOf(err), "renewing within 30 minutes: after it")
}

// Neither the API key nor the webhook secret is printed, logged, marshalled
// or put in an error -- not even when Paddle's answer repeats the key.
func TestNoSecretIsEverSaid(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	s := subscribe(t, srv, ch)
	c := srv.Config()

	var said []string
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("paddle", "config", c, "channel", ch)
	slog.New(slog.NewJSONHandler(&b, nil)).Info("paddle", "config", c)
	said = append(said, b.String(), fmt.Sprintf("%v %+v %#v", c, c, c), fmt.Sprintf("%v %+v", ch, ch))
	if j, err := json.Marshal(c); err == nil {
		said = append(said, string(j))
	}

	srv.Fail(paddletest.Get, paddletest.Fault{Status: http.StatusForbidden, Code: "forbidden", Detail: "the key " + srv.APIKey + " may not"})
	_, err := ch.Subscription(ctx, s.ID)
	require.Error(t, err)
	said = append(said, err.Error(), fmt.Sprintf("%+v %#v", err, err))

	gone, err := paddle.New(paddle.Config{APIKey: paddletest.Secret(srv.APIKey), Sandbox: true, URL: "http://127.0.0.1:1"})
	require.NoError(t, err)
	_, err = gone.Charge(ctx, now(s, "stmt-0001", 1_000))
	require.Equal(t, mor.Unknown, mor.KindOf(err))
	said = append(said, err.Error())

	h, body := srv.Webhook("subscription.updated", s.ID)
	h.Set(paddle.SignatureHeader, "ts=1;h1="+strings.Repeat("0", 64))
	_, err = ch.Webhook(h, body)
	require.Error(t, err)
	said = append(said, err.Error())

	for _, s := range said {
		require.NotContains(t, s, srv.APIKey)
		require.NotContains(t, s, srv.WebhookSecret)
	}
}

func TestAChargeThatCannotBeIsNotSent(t *testing.T) {
	srv, ch := setup(t)
	s := subscribe(t, srv, ch)
	ok := now(s, "stmt-0001", 15_000)

	for name, c := range map[string]func(c *mor.Charge){
		"no subscription":           func(c *mor.Charge) { c.Subscription = "" },
		"a subscription of a path":  func(c *mor.Charge) { c.Subscription = "sub_../../transactions" },
		"no key":                    func(c *mor.Charge) { c.Key = "" },
		"a key of more":             func(c *mor.Charge) { c.Key = "stmt 0001" },
		"a long key":                func(c *mor.Charge) { c.Key = strings.Repeat("k", 65) },
		"no time":                   func(c *mor.Charge) { c.When = 0 },
		"nothing":                   func(c *mor.Charge) { c.Amount = usd(0) },
		"won":                       func(c *mor.Charge) { c.Amount = money.Amount{Minor: 15_000, Currency: "KRW"} },
		"a currency levyan has not": func(c *mor.Charge) { c.Amount = money.Amount{Minor: 15_000, Currency: "GBP"} },
		"a short description":       func(c *mor.Charge) { c.Description = "x" },
		"a long description":        func(c *mor.Charge) { c.Description = strings.Repeat("가", 201) },
	} {
		t.Run(name, func(t *testing.T) {
			c2 := ok
			c(&c2)
			_, err := ch.Charge(t.Context(), c2)
			require.Equal(t, mor.Invalid, mor.KindOf(err))
		})
	}
	require.Zero(t, srv.Requests(paddletest.Charge))
}

func TestNewRefusesAChannelThatCannotWork(t *testing.T) {
	srv := paddletest.New(t)
	// A key's shape, made up here: no key of anybody's is written in this
	// repository.
	live := "pdl_live_" + "apikey_" + strings.Repeat("0", 26) + "_" + strings.Repeat("A", 22) + "_AAA"
	for name, f := range map[string]func(c *paddle.Config){
		"no API key":                    func(c *paddle.Config) { c.APIKey = paddletest.Secret("") },
		"a client token for a key":      func(c *paddle.Config) { c.APIKey = paddletest.Secret(srv.ClientToken) },
		"a webhook secret for a key":    func(c *paddle.Config) { c.APIKey = paddletest.Secret(srv.WebhookSecret) },
		"a live key in the sandbox":     func(c *paddle.Config) { c.APIKey = paddletest.Secret(live) },
		"a sandbox key live":            func(c *paddle.Config) { c.Sandbox = false },
		"a key in the page":             func(c *paddle.Config) { c.ClientToken = srv.APIKey },
		"a live token in the sandbox":   func(c *paddle.Config) { c.ClientToken = "live_" + strings.Repeat("0", 27) },
		"a plan that is no price":       func(c *paddle.Config) { c.Plan = "pro_" + strings.Repeat("0", 26) },
		"a tax mode of Paddle's own":    func(c *paddle.Config) { c.TaxMode = "account_setting" },
		"a tax category Paddle has not": func(c *paddle.Config) { c.TaxCategory = "groceries" },
		"a negative tolerance":          func(c *paddle.Config) { c.Tolerance = -time.Second },
		"an API that is no URL":         func(c *paddle.Config) { c.URL = "api.paddle.com" },
		"an API of another sort":        func(c *paddle.Config) { c.URL = "ftp://api.paddle.com" },
	} {
		t.Run(name, func(t *testing.T) {
			c := srv.Config()
			f(&c)
			_, err := paddle.New(c)
			require.Error(t, err)
		})
	}

	c := srv.Config()
	c.ClientToken, c.Plan = "", ""
	ch, err := paddle.New(c)
	require.NoError(t, err, "a channel that only charges needs no client token and no plan")
	_, err = ch.StartCheckout(t.Context(), start)
	require.Equal(t, mor.Invalid, mor.KindOf(err), "but one that opens a checkout does")
	require.Equal(t, "paddle", ch.Name())
}

func TestACheckoutStartsFromWhatItCanBe(t *testing.T) {
	srv, ch := setup(t)
	for _, s := range []mor.Start{
		{Success: start.Success},                                  // no account
		{Account: "acct 0001", Success: start.Success},            // not an account levyan writes
		{Account: account, Success: "/subscribed"},                // not absolute
		{Account: account, Success: "javascript:alert(1)"},        // not a page
		{Account: account, Success: start.Success, Customer: "c"}, // not Paddle's
		{Account: account, Success: start.Success, Currency: "XAU"},
	} {
		_, err := ch.StartCheckout(t.Context(), s)
		require.Equal(t, mor.Invalid, mor.KindOf(err), "%+v", s)
	}
	require.Zero(t, srv.Requests(paddletest.Checkout))

	co, err := ch.StartCheckout(t.Context(), mor.Start{Account: account, Success: start.Success, Currency: "EUR"})
	require.NoError(t, err)
	require.NoError(t, srv.Pay(co))
	s, err := ch.CompleteCheckout(t.Context(), co)
	require.NoError(t, err)
	require.Equal(t, "EUR", s.Currency, "billed in what the checkout asked")

	_, err = ch.Charge(t.Context(), now(s, "stmt-0001", 15_000))
	require.Equal(t, mor.Invalid, mor.KindOf(err), "a charge in dollars of a subscription in euros")
}

// An error from beneath the channel -- one that is no mor.Error -- says
// nothing of what it did.
func TestAnErrorFromBeneathIsUnknown(t *testing.T) {
	require.Equal(t, mor.Unknown, mor.KindOf(errors.New("a reset connection")))
}
