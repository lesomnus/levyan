package toss_test

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/internal/pg/toss"
	"github.com/lesomnus/levyan/internal/pg/toss/tosstest"
)

// TestLive asks Toss's test environment what the fake answers in its place.
// It runs by hand, never in CI, with the keys of a test MID in the
// environment -- never in a file of this repository:
//
//	TOSS_TEST_SECRET_KEY    test_sk_…, the API 개별 연동 secret key. Without it
//	                        the test is skipped; with a live key it fails
//	                        before it sends anything.
//	TOSS_TEST_CUSTOMER_KEY  the customerKey a billing key was issued to, and
//	TOSS_TEST_BILLING_KEY   that billing key, to charge and give back; or
//	TOSS_TEST_AUTH_KEY      instead, the authKey Toss's window sent back for
//	                        that customer, which the test exchanges first.
//
// A billing key of a test MID comes from Toss's own pages, so that no card
// data passes through anything of levyan's even here: the API tester at
// https://docs.tosspayments.com/reference/test with the test secret key
// ("카드 정보로 빌링키 발급", with a test card), or the window opened from a
// page of one's own with the test client key. A test key moves no money
// (https://docs.tosspayments.com/guides/v2/get-started/environment).
//
//	TOSS_TEST_SECRET_KEY=test_sk_… TOSS_TEST_CUSTOMER_KEY=… TOSS_TEST_BILLING_KEY=… \
//		go test ./internal/pg/toss -run TestLive -v
//
// Where Toss's documentation does not say what Toss does -- what a query of a
// declined order answers -- the test says what Toss answered in its log.
func TestLive(t *testing.T) {
	secret := os.Getenv("TOSS_TEST_SECRET_KEY")
	if secret == "" {
		t.Skip("TOSS_TEST_SECRET_KEY is not set: the live test runs by hand, against Toss's test environment")
	}
	require.True(t, strings.HasPrefix(secret, "test_"), "TOSS_TEST_SECRET_KEY is no test key: the live test charges nothing real, and is never given a live key")

	channel := func(t *testing.T, test string) *toss.Channel {
		t.Helper()
		c := toss.Config{SecretKey: tosstest.Secret("${env:TOSS_TEST_SECRET_KEY}")}
		if test != "" {
			c.Client = &http.Client{Timeout: toss.Timeout, Transport: testCode{test, http.DefaultTransport}}
		}
		ch, err := toss.New(c)
		require.NoError(t, err)
		return ch
	}
	ch := channel(t, "")
	order := func() string { return "lv-live-" + rand.Text() }

	t.Run("an order Toss has nothing of is not found", func(t *testing.T) {
		_, err := ch.Payment(t.Context(), order())
		require.Equal(t, pg.NotFound, pg.KindOf(err), "%v", err)
	})

	t.Run("a key Toss does not know is refused as auth", func(t *testing.T) {
		wrong, err := toss.New(toss.Config{SecretKey: tosstest.Secret("test_sk_" + rand.Text())})
		require.NoError(t, err)
		_, err = wrong.Payment(t.Context(), order())
		require.Equal(t, pg.Auth, pg.KindOf(err), "%v", err)
	})

	m := pg.Method{Customer: os.Getenv("TOSS_TEST_CUSTOMER_KEY"), Key: pg.NewBillingKey(os.Getenv("TOSS_TEST_BILLING_KEY"))}
	if auth := os.Getenv("TOSS_TEST_AUTH_KEY"); auth != "" && m.Customer != "" {
		var err error
		m, err = ch.CompleteRegistration(t.Context(), pg.Registration{Customer: m.Customer}, url.Values{"authKey": {auth}, "customerKey": {m.Customer}})
		require.NoError(t, err)
		t.Logf("registered: issuer %s, %s, %d, %d", m.Card.Issuer, m.Card.Number, m.Card.Kind, m.Card.Owner)
	}
	if m.Customer == "" || m.Key.IsZero() {
		t.Log("TOSS_TEST_CUSTOMER_KEY with TOSS_TEST_BILLING_KEY or TOSS_TEST_AUTH_KEY is not set: nothing is charged")
		return
	}

	t.Run("a charge, sent again under its key, asked, and given back in parts", func(t *testing.T) {
		ctx := t.Context()
		o := order()
		c := pg.Charge{Method: m, Order: o, Amount: won(1_000), Name: "levyan live test", Key: o + "-1"}
		p, err := ch.Charge(ctx, c)
		require.NoError(t, err)
		require.Equal(t, pg.Paid, p.Status, "%+v", p)
		require.Equal(t, won(1_000), p.Amount)

		again, err := ch.Charge(ctx, c)
		require.NoError(t, err)
		require.Equal(t, p.ID, again.ID, "the first answer, to its key")

		c.Key = o + "-2"
		again, err = ch.Charge(ctx, c)
		require.NoError(t, err, "an order approved before is the payment it is")
		require.Equal(t, p.ID, again.ID)

		q, err := ch.Payment(ctx, o)
		require.NoError(t, err)
		require.Equal(t, pg.Paid, q.Status)
		require.Equal(t, p.ID, q.ID)

		part, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, Amount: won(300), Reason: "levyan live test", Key: o + "-cancel-1"})
		require.NoError(t, err)
		require.Equal(t, pg.PartlyCanceled, part.Status)
		require.Equal(t, won(700), part.Balance)

		all, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, All: true, Reason: "levyan live test", Key: o + "-cancel-2"})
		require.NoError(t, err)
		require.Equal(t, pg.Canceled, all.Status)
		require.Equal(t, won(0), all.Balance)
		require.Len(t, all.Cancels, 2)

		done, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, All: true, Reason: "levyan live test", Key: o + "-cancel-3"})
		require.NoError(t, err)
		require.Equal(t, pg.Canceled, done.Status)
	})

	t.Run("a declined charge is a failed payment", func(t *testing.T) {
		o := order()
		p, err := channel(t, "REJECT_CARD_PAYMENT").Charge(t.Context(), pg.Charge{Method: m, Order: o, Amount: won(1_000), Name: "levyan live test", Key: o + "-1"})
		require.NoError(t, err)
		require.Equal(t, pg.Failed, p.Status)
		require.Equal(t, pg.Declined, p.Failure.Kind)
		require.Equal(t, pg.DeclineTemporary, p.Failure.Decline)
		require.Equal(t, "REJECT_CARD_PAYMENT", p.Failure.Code)

		q, err := ch.Payment(t.Context(), o)
		t.Logf("a declined order, asked: %+v, %v", q, err)
	})

	t.Run("a 5xx is unknown", func(t *testing.T) {
		o := order()
		_, err := channel(t, "FAILED_INTERNAL_SYSTEM_PROCESSING").Charge(t.Context(), pg.Charge{Method: m, Order: o, Amount: won(1_000), Name: "levyan live test", Key: o + "-1"})
		require.Equal(t, pg.Unknown, pg.KindOf(err), "%v", err)

		q, err := ch.Payment(t.Context(), o)
		t.Logf("an order answered 5xx, asked: %+v, %v", q, err)
		if err == nil && q.Status == pg.Paid {
			_, err := ch.Cancel(t.Context(), pg.Cancel{Payment: q.ID, All: true, Reason: "levyan live test", Key: o + "-cancel-1"})
			require.NoError(t, err)
		}
	})
}

// testCode asks Toss's test environment to answer every charge with an
// error, by the header it reproduces one with
// (https://docs.tosspayments.com/guides/v2/get-started/environment).
type testCode struct {
	code string
	next http.RoundTripper
}

func (t testCode) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/billing/") {
		r = r.Clone(r.Context())
		r.Header.Set("TossPayments-Test-Code", t.code)
	}
	return t.next.RoundTrip(r)
}
