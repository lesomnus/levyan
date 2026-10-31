package paddle_test

import (
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
)

// TestLive asks Paddle's sandbox what the fake answers in its place. It runs
// by hand, never in CI, with the credentials of a sandbox account in the
// environment -- never in a file of this repository:
//
//	PADDLE_SANDBOX_API_KEY       pdl_sdbx_apikey_…, with the permissions to read
//	                             and write transactions, subscriptions and
//	                             adjustments. Without it the test is skipped;
//	                             with a live key it fails before it sends
//	                             anything.
//	PADDLE_SANDBOX_CLIENT_TOKEN  test_…, and
//	PADDLE_SANDBOX_PLAN          pri_…, a recurring price of the account, in
//	PADDLE_SANDBOX_CURRENCY      (USD when unset): the test starts a checkout
//	                             and logs its URL, to finish by hand with a
//	                             test card. In KRW, with South Korea picked on
//	                             the checkout's first screen, it is where
//	                             Korean cards, KakaoPay and NaverPay are to be
//	                             seen (docs/design.md §16, 61).
//	PADDLE_SANDBOX_SUBSCRIPTION  sub_…, one such a checkout started: the test
//	                             charges it now and for the next period, asks
//	                             both, and refunds part of the first.
//	PADDLE_SANDBOX_CANCEL=1      and then cancels it -- at the end of its period,
//	                             to see what Paddle does with charges for a
//	                             period that will not come, and then now. A
//	                             canceled subscription is never reinstated.
//
// A card is given to Paddle's sandbox checkout, so that no card data passes
// through anything of levyan's even here: 4242 4242 4242 4242 with any future
// expiry, or 4000 0027 6000 3184 to have later charges declined. The sandbox
// moves no money (https://developer.paddle.com/sdks/sandbox).
//
//	PADDLE_SANDBOX_API_KEY=pdl_sdbx_apikey_… PADDLE_SANDBOX_SUBSCRIPTION=sub_… \
//		go test ./internal/mor/paddle -run TestLive -v
//
// Where Paddle's documentation does not say what Paddle does -- what a
// declined charge leaves behind, whether a charge made now is taken of a
// subscription scheduled to cancel, whether one for the next period is
// forgiven then or only once it cancels -- the test says what Paddle answered
// in its log.
func TestLive(t *testing.T) {
	key := os.Getenv("PADDLE_SANDBOX_API_KEY")
	if key == "" {
		t.Skip("PADDLE_SANDBOX_API_KEY is not set: the live test runs by hand, against Paddle's sandbox")
	}
	require.True(t, strings.HasPrefix(key, "pdl_sdbx_"), "PADDLE_SANDBOX_API_KEY is no sandbox key: the live test charges nothing real, and is never given a live key")

	currency := os.Getenv("PADDLE_SANDBOX_CURRENCY")
	if currency == "" {
		currency = "USD"
	}
	plans := map[string]string{}
	if p := os.Getenv("PADDLE_SANDBOX_PLAN"); p != "" {
		plans[currency] = p
	}
	ch, err := paddle.New(paddle.Config{
		APIKey:      paddletest.Secret("${env:PADDLE_SANDBOX_API_KEY}"),
		ClientToken: os.Getenv("PADDLE_SANDBOX_CLIENT_TOKEN"),
		Plans:       plans,
		Sandbox:     true,
	})
	require.NoError(t, err)
	ctx := t.Context()
	made := func(prefix string) string { return prefix + "_" + strings.ToLower(rand.Text()) }
	key2 := func() string { return "lv-live-" + rand.Text() }

	t.Run("a transaction Paddle has nothing of is not found", func(t *testing.T) {
		_, err := ch.Transaction(ctx, made("txn"))
		require.Equal(t, mor.NotFound, mor.KindOf(err), "%v", err)
	})

	t.Run("a key Paddle does not know is refused as auth", func(t *testing.T) {
		wrong, err := paddle.New(paddle.Config{APIKey: paddletest.Secret("pdl_sdbx_apikey_" + strings.ToLower(rand.Text())), Sandbox: true})
		require.NoError(t, err)
		_, err = wrong.Subscription(ctx, made("sub"))
		require.Equal(t, mor.Auth, mor.KindOf(err), "%v", err)
	})

	if os.Getenv("PADDLE_SANDBOX_CLIENT_TOKEN") != "" && os.Getenv("PADDLE_SANDBOX_PLAN") != "" {
		t.Run("a checkout is started, to finish by hand", func(t *testing.T) {
			co, err := ch.StartCheckout(ctx, mor.Start{Account: "lv-live-" + rand.Text(), Currency: currency, Success: "https://example.com/subscribed"})
			require.NoError(t, err)
			_, err = ch.CompleteCheckout(ctx, co)
			require.ErrorIs(t, err, mor.ErrOpen)
			t.Logf("checkout %s: %s -- pay with a test card, then run with PADDLE_SANDBOX_SUBSCRIPTION", co.ID, co.Window.URL)
		})
	}

	t.Run("the event stream is read, oldest first", func(t *testing.T) {
		got, err := ch.Notices(ctx, mor.Read{Since: time.Now().Add(-24 * time.Hour)})
		if mor.KindOf(err) == mor.Auth {
			t.Skipf("the key may not read the event stream (notification.read): %v", err)
		}
		require.NoError(t, err)
		t.Logf("%d notices in the last day, more: %v, last %s", len(got.Notices), got.More, got.Last)
		for i := 1; i < len(got.Notices); i++ {
			require.Less(t, got.Notices[i-1].ID, got.Notices[i].ID)
		}
	})

	id := os.Getenv("PADDLE_SANDBOX_SUBSCRIPTION")
	if id == "" {
		t.Log("PADDLE_SANDBOX_SUBSCRIPTION is not set: nothing is charged")
		return
	}
	s, err := ch.Subscription(ctx, id)
	require.NoError(t, err)
	t.Logf("subscription: %s, %s, next %s, change %+v", s.Status, s.Currency, s.Next, s.Change)

	// Twice what Paddle charges at least, in the subscription's currency.
	least := map[string]int64{"USD": 140, "EUR": 130, "JPY": 200, "KRW": 1_960}[s.Currency]
	require.NotZero(t, least, "a subscription in %s, which the test does not charge", s.Currency)
	amount := money.Amount{Minor: least, Currency: s.Currency}

	t.Run("a charge made now, sent again, asked, and refunded in part", func(t *testing.T) {
		c := mor.Charge{Subscription: id, Key: key2(), When: mor.Now, Amount: amount, Description: "levyan live test"}
		tx, err := ch.Charge(ctx, c)
		require.NoError(t, err)
		t.Logf("charged now: %s %s, failure %+v", tx.ID, tx.Status, tx.Failure)
		if tx.Status == mor.Failed {
			got, err := ch.Charged(ctx, c)
			t.Logf("a declined charge, asked: %+v, %v", got, err)
			return
		}

		again, err := ch.Charge(ctx, c)
		require.NoError(t, err)
		require.Equal(t, tx.ID, again.ID, "found, and not sent again")
		got, err := ch.Charged(ctx, c)
		require.NoError(t, err)
		require.Equal(t, tx.ID, got.ID)
		l, ok := got.Line(c.Key)
		require.True(t, ok)
		t.Logf("line: %s, tax %s, total %s", l.Amount, l.Tax, l.Total)

		if got.Status != mor.Paid {
			t.Log("not paid yet: no refund")
			return
		}
		r := mor.Refund{Transaction: got.ID, Line: l.ID, Amount: money.Amount{Minor: least / 2, Currency: s.Currency}, Reason: "levyan live test", Key: key2()}
		a, err := ch.Refund(ctx, r)
		require.NoError(t, err)
		t.Logf("refund: %s %s %s", a.ID, a.Status, a.Amount)
		found, err := ch.Refunded(ctx, r)
		require.NoError(t, err)
		require.Equal(t, a.ID, found.ID)
	})

	scheduled := mor.Charge{Subscription: id, Key: key2(), When: mor.NextPeriod, Amount: amount, Description: "levyan live test"}
	if s.Renews() {
		t.Run("a charge for the next period waits in the next transaction", func(t *testing.T) {
			tx, err := ch.Charge(ctx, scheduled)
			require.NoError(t, err)
			require.Equal(t, mor.Scheduled, tx.Status, "%+v", tx)
			got, err := ch.Charged(ctx, scheduled)
			require.NoError(t, err)
			require.Equal(t, mor.Scheduled, got.Status)
		})
	}

	if os.Getenv("PADDLE_SANDBOX_CANCEL") != "1" {
		return
	}
	t.Run("a subscription scheduled to cancel, and then canceled", func(t *testing.T) {
		ended, err := ch.Cancel(ctx, mor.Cancel{Subscription: id})
		require.NoError(t, err)
		require.False(t, ended.Renews())

		_, err = ch.Charge(ctx, mor.Charge{Subscription: id, Key: key2(), When: mor.NextPeriod, Amount: amount, Description: "levyan live test"})
		require.ErrorIs(t, err, mor.ErrNoNextPeriod)

		got, err := ch.Charged(ctx, scheduled)
		t.Logf("a charge for the next period, once the subscription is scheduled to cancel: %+v, %v", got, err)

		now := mor.Charge{Subscription: id, Key: key2(), When: mor.Now, Amount: amount, Description: "levyan live test"}
		tx, err := ch.Charge(ctx, now)
		t.Logf("a charge made now of a subscription scheduled to cancel: %+v, %v", tx, err)

		gone, err := ch.Cancel(ctx, mor.Cancel{Subscription: id, Now: true})
		require.NoError(t, err)
		require.Equal(t, mor.Canceled, gone.Status)
		_, err = ch.Charge(ctx, mor.Charge{Subscription: id, Key: key2(), When: mor.Now, Amount: amount, Description: "levyan live test"})
		require.ErrorIs(t, err, mor.ErrEnded)
	})
}
