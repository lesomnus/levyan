package toss_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/internal/pg/toss"
	"github.com/lesomnus/levyan/internal/pg/toss/tosstest"
)

func won(n int64) money.Amount { return money.Amount{Minor: n, Currency: "KRW"} }

var back = pg.Start{Success: "https://pay.example.com/methods/back", Fail: "https://pay.example.com/methods/fail"}

func setup(t *testing.T) (*tosstest.Server, *toss.Channel) {
	t.Helper()
	srv := tosstest.New(t)
	ch, err := toss.New(srv.Config())
	require.NoError(t, err)
	return srv, ch
}

// register is a payer registering the fake's test card through the window.
func register(t *testing.T, srv *tosstest.Server, ch *toss.Channel) pg.Method {
	t.Helper()
	reg, err := ch.StartRegistration(t.Context(), back)
	require.NoError(t, err)
	m, err := ch.CompleteRegistration(t.Context(), reg, srv.Register(reg))
	require.NoError(t, err)
	return m
}

func charge(m pg.Method, order string, n int64) pg.Charge {
	return pg.Charge{Method: m, Order: order, Amount: won(n), Name: "loft 2026년 10월", Key: order + "-1"}
}

func TestACardRegisteredInTheWindowIsCharged(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()

	reg, err := ch.StartRegistration(ctx, back)
	require.NoError(t, err)
	require.Equal(t, toss.SDK, reg.Window.Script)
	require.Equal(t, map[string]string{
		"clientKey":   srv.ClientKey,
		"customerKey": reg.Customer,
		"method":      "CARD",
		"successUrl":  back.Success,
		"failUrl":     back.Fail,
	}, reg.Window.Values)
	require.GreaterOrEqual(t, len(reg.Customer), 22, "a customerKey Toss takes")
	require.LessOrEqual(t, len(reg.Customer), 50)

	again, err := ch.StartRegistration(ctx, back)
	require.NoError(t, err)
	require.NotEqual(t, reg.Customer, again.Customer, "made up for each, and nobody's to guess")

	m, err := ch.CompleteRegistration(ctx, reg, srv.Register(reg))
	require.NoError(t, err)
	require.Equal(t, reg.Customer, m.Customer)
	require.False(t, m.Key.IsZero())
	require.Equal(t, pg.Card{Issuer: "61", Number: "43301234****123*", Kind: pg.Credit, Owner: pg.Personal}, m.Card)
	require.False(t, m.At.IsZero())

	p, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)
	require.Equal(t, "order-0001", p.Order)
	require.NotEmpty(t, p.ID)
	require.Equal(t, won(15_000), p.Amount)
	require.Equal(t, won(15_000), p.Balance)
	require.False(t, p.At.IsZero())
	require.Nil(t, p.Failure)
	require.Equal(t, 1, srv.Approved())

	q, err := ch.Payment(ctx, "order-0001")
	require.NoError(t, err)
	require.Equal(t, p, q)
}

// A payer who reloads the page Toss's window sent them back to completes the
// same registration again: the authKey is spent, and the exchange is answered
// as it was the first time.
func TestCompletingARegistrationAgainIsTheSameMethod(t *testing.T) {
	srv, ch := setup(t)
	reg, err := ch.StartRegistration(t.Context(), back)
	require.NoError(t, err)
	returned := srv.Register(reg)

	m, err := ch.CompleteRegistration(t.Context(), reg, returned)
	require.NoError(t, err)
	again, err := ch.CompleteRegistration(t.Context(), reg, returned)
	require.NoError(t, err)
	require.Equal(t, m.Key.Reveal(), again.Key.Reveal())
	require.Equal(t, 2, srv.Requests(tosstest.Issue))
}

func TestARegistrationIsCompletedOnlyByWhatItsWindowSentBack(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	reg, err := ch.StartRegistration(ctx, back)
	require.NoError(t, err)
	other, err := ch.StartRegistration(ctx, back)
	require.NoError(t, err)

	_, err = ch.CompleteRegistration(ctx, reg, srv.Register(other))
	require.Equal(t, pg.Invalid, pg.KindOf(err), "another registration's")

	_, err = ch.CompleteRegistration(ctx, reg, url.Values{"code": {"PAY_PROCESS_CANCELED"}, "message": {"결제가 사용자에 의해 취소되었습니다."}})
	require.Equal(t, pg.Declined, pg.KindOf(err), "a window the payer left")
	var e *pg.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "PAY_PROCESS_CANCELED", e.Code)

	_, err = ch.CompleteRegistration(ctx, reg, url.Values{"customerKey": {reg.Customer}, "authKey": {"made-up"}})
	require.Equal(t, pg.Declined, pg.KindOf(err), "an authKey Toss did not give")

	_, err = ch.CompleteRegistration(ctx, reg, url.Values{})
	require.Equal(t, pg.Invalid, pg.KindOf(err))
	require.Equal(t, 1, srv.Requests(tosstest.Issue), "nothing is asked of Toss that is not Toss's to answer")
}

// The payment failing is not the request failing: a charge the card declined
// is a payment, failed, and no error.
func TestADeclinedChargeIsAFailedPaymentAndNoError(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	srv.Decline("REJECT_CARD_PAYMENT")
	p, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, pg.Failed, p.Status)
	require.Equal(t, "order-0001", p.Order)
	require.Equal(t, won(15_000), p.Amount)
	require.Equal(t, &pg.Failure{Kind: pg.Declined, Code: "REJECT_CARD_PAYMENT", Message: "카드 사용이 거절되었습니다."}, p.Failure)
	require.Equal(t, 0, srv.Approved())

	q, err := ch.Payment(ctx, "order-0001")
	require.NoError(t, err)
	require.Equal(t, pg.Failed, q.Status)
	require.Equal(t, pg.Declined, q.Failure.Kind)

	// Tried again, it is another charge: another order.
	p, err = ch.Charge(ctx, charge(m, "order-0001-2", 15_000))
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)
	require.Equal(t, 1, srv.Approved())
}

// Toss charged the card and its answer was lost (levyan#1, scenario 5): the
// charge is unknown, the order says it was paid, and the same charge sent
// again -- under its key, or under a new one -- is that payment, not another.
func TestAnAnswerLostAfterTheChargeIsUnknownUntilTheOrderIsAsked(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	srv.Fail(tosstest.Charge, tosstest.Fault{Status: 500, Code: "FAILED_INTERNAL_SYSTEM_PROCESSING", After: true})
	_, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err))
	require.Equal(t, 1, srv.Approved(), "charged all the same")

	p, err := ch.Payment(ctx, "order-0001")
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)

	again, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, p, again, "under its key, the first answer")

	c := charge(m, "order-0001", 15_000)
	c.Key = "order-0001-2"
	again, err = ch.Charge(ctx, c)
	require.NoError(t, err)
	require.Equal(t, p, again, "under a new key, the order Toss approved")
	require.Equal(t, 1, srv.Approved())
}

func TestATimeoutIsUnknownUntilTheOrderIsAsked(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)

	srv.Fail(tosstest.Charge, tosstest.Fault{Hang: true})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err))
	require.ErrorIs(t, err, context.DeadlineExceeded)

	p, err := ch.Payment(t.Context(), "order-0001")
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)
	require.Equal(t, 1, srv.Approved())
}

// A 5xx that came before Toss did anything is as unknown as one after: the
// order is what says nothing was charged, and only then is it charged again.
func TestA5xxBeforeTheChargeIsUnknownUntilTheOrderIsAsked(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	srv.Fail(tosstest.Charge, tosstest.Fault{Status: http.StatusBadGateway})
	_, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err))

	_, err = ch.Payment(ctx, "order-0001")
	require.Equal(t, pg.NotFound, pg.KindOf(err), "never charged")

	c := charge(m, "order-0001", 15_000)
	c.Key = "order-0001-2"
	p, err := ch.Charge(ctx, c)
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)
	require.Equal(t, 1, srv.Approved())
}

// Toss keeps its own failure under the key it answered, and answers it again
// to that key: which is why a charge is sent again under a new key once the
// order is asked, and never under the old one in a loop.
func TestAFailureTossKeepsIsAnsweredToItsKeyAgain(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	srv.Fail(tosstest.Charge, tosstest.Fault{Status: 500, Code: "FAILED_INTERNAL_SYSTEM_PROCESSING", Kept: true})
	_, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err))
	_, err = ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err), "the same answer to the same key")

	_, err = ch.Payment(ctx, "order-0001")
	require.Equal(t, pg.NotFound, pg.KindOf(err))
	c := charge(m, "order-0001", 15_000)
	c.Key = "order-0001-2"
	p, err := ch.Charge(ctx, c)
	require.NoError(t, err)
	require.Equal(t, pg.Paid, p.Status)
	require.Equal(t, 1, srv.Approved())
}

func TestTheSameIdempotencyKeyIsTheSameCharge(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)

	p, err := ch.Charge(t.Context(), charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	again, err := ch.Charge(t.Context(), charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, p, again)
	require.Equal(t, 2, srv.Requests(tosstest.Charge))
	require.Equal(t, 1, srv.Approved())
}

// A key is one charge's. Sent with another, Toss answers what it answered the
// first: which is no answer to the second.
func TestAKeyUsedForAnotherChargeIsRefused(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)

	_, err := ch.Charge(t.Context(), pg.Charge{Method: m, Order: "order-0001", Amount: won(15_000), Name: "loft", Key: "the-key"})
	require.NoError(t, err)
	_, err = ch.Charge(t.Context(), pg.Charge{Method: m, Order: "order-0002", Amount: won(15_000), Name: "loft", Key: "the-key"})
	require.Equal(t, pg.Invalid, pg.KindOf(err))
	require.Equal(t, 1, srv.Approved())
}

func TestAnOrderChargedForAnotherAmountIsRefused(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)

	_, err := ch.Charge(t.Context(), charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	c := charge(m, "order-0001", 20_000)
	c.Key = "order-0001-2"
	_, err = ch.Charge(t.Context(), c)
	require.Equal(t, pg.Invalid, pg.KindOf(err))
	require.Equal(t, 1, srv.Approved())
}

func TestACancellationGivesBackPartOrAllOfWhatIsLeft(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	p, err := ch.Charge(ctx, charge(m, "order-0001", 10_000))
	require.NoError(t, err)

	part, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, Amount: won(3_000), Reason: "정정 청구", Key: "refund-1"})
	require.NoError(t, err)
	require.Equal(t, pg.PartlyCanceled, part.Status)
	require.Equal(t, won(10_000), part.Amount)
	require.Equal(t, won(7_000), part.Balance)
	require.Len(t, part.Cancels, 1)
	require.Equal(t, won(3_000), part.Cancels[0].Amount)
	require.Equal(t, "정정 청구", part.Cancels[0].Reason)
	require.NotEmpty(t, part.Cancels[0].ID)

	again, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, Amount: won(3_000), Reason: "정정 청구", Key: "refund-1"})
	require.NoError(t, err)
	require.Equal(t, part, again, "the same cancellation sent again gives back nothing more")

	_, err = ch.Cancel(ctx, pg.Cancel{Payment: p.ID, Amount: won(8_000), Reason: "정정 청구", Key: "refund-2"})
	require.Equal(t, pg.Invalid, pg.KindOf(err), "more than is left")

	all, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, All: true, Reason: "해지", Key: "refund-3"})
	require.NoError(t, err)
	require.Equal(t, pg.Canceled, all.Status)
	require.Equal(t, won(0), all.Balance)
	require.Len(t, all.Cancels, 2)
	require.Equal(t, won(3_000), all.Cancels[0].Amount)
	require.Equal(t, won(7_000), all.Cancels[1].Amount)

	done, err := ch.Cancel(ctx, pg.Cancel{Payment: p.ID, All: true, Reason: "해지", Key: "refund-4"})
	require.NoError(t, err)
	require.Equal(t, pg.Canceled, done.Status, "all of what is left of nothing is done")

	_, err = ch.Cancel(ctx, pg.Cancel{Payment: p.ID, Amount: won(1_000), Reason: "정정 청구", Key: "refund-5"})
	require.Equal(t, pg.Invalid, pg.KindOf(err), "a part of nothing is not")

	require.Less(t, p.Progress(), part.Progress())
	require.Less(t, part.Progress(), all.Progress())

	for _, c := range []pg.Cancel{
		{Payment: p.ID, Reason: "정정 청구", Key: "k"},                                // no amount, and not all
		{Payment: p.ID, Amount: won(1_000), All: true, Reason: "정정 청구", Key: "k"}, // both
		{Payment: p.ID, Amount: won(1_000), Reason: "정정 청구"},                      // no key
		{Payment: p.ID, Amount: won(1_000), Key: "k"},                             // no reason
		{Payment: p.ID, Amount: money.Amount{Minor: 1_000, Currency: "USD"}, Reason: "정정 청구", Key: "k"},
	} {
		_, err := ch.Cancel(ctx, c)
		require.Equal(t, pg.Invalid, pg.KindOf(err), "%+v", c)
	}
	require.Equal(t, 6, srv.Requests(tosstest.Cancel), "a cancellation that cannot be is not sent")
}

// What a webhook says is not believed: the payment it names is asked of the
// API, and the answer is the payment, whatever the webhook said of it.
func TestAWebhookIsBelievedOnlyAsTheAPIAnswersIt(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)
	p, err := ch.Charge(ctx, charge(m, "order-0001", 10_000))
	require.NoError(t, err)

	forged, err := json.Marshal(map[string]any{
		"eventType": "PAYMENT_STATUS_CHANGED",
		"createdAt": "2026-10-31T12:00:00.000000",
		"data":      map[string]any{"paymentKey": p.ID, "orderId": p.Order, "status": "CANCELED", "balanceAmount": 0, "totalAmount": 10_000},
	})
	require.NoError(t, err)
	n, err := ch.Webhook(http.Header{}, forged)
	require.NoError(t, err, "unsigned, and still a notice: it is only as good as its recheck")
	require.Equal(t, pg.NoticePayment, n.Type)
	require.Equal(t, "PAYMENT_STATUS_CHANGED", n.Event)
	require.False(t, n.Signed)
	require.Equal(t, p.ID, n.Payment)
	require.Equal(t, p.Order, n.Order)
	require.Equal(t, time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC), n.At, "a time with no zone is Toss's, in KST")

	asked, err := ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.Equal(t, p, asked, "paid, as the API says, and not canceled, as the webhook did")

	_, err = ch.Cancel(ctx, pg.Cancel{Payment: p.ID, All: true, Reason: "해지", Key: "refund-1"})
	require.NoError(t, err)
	h, body := srv.Webhook(p.ID)
	n, err = ch.Webhook(h, body)
	require.NoError(t, err)
	require.True(t, n.Signed)
	asked, err = ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.Equal(t, pg.Canceled, asked.Status)

	// And a webhook rechecked after a later one never walks it back: the
	// forged one, asked again now, is the payment as it is now.
	n, err = ch.Webhook(http.Header{}, forged)
	require.NoError(t, err)
	late, err := ch.Recheck(ctx, n)
	require.NoError(t, err)
	require.Equal(t, asked.Progress(), late.Progress())
}

func TestAWebhookWhoseSignatureDoesNotHoldIsRefused(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)
	p, err := ch.Charge(t.Context(), charge(m, "order-0001", 10_000))
	require.NoError(t, err)

	h, body := srv.Webhook(p.ID)
	_, err = ch.Webhook(h, body)
	require.NoError(t, err)

	tampered := []byte(strings.Replace(string(body), `"DONE"`, `"CANCELED"`, 1))
	require.NotEqual(t, body, tampered)
	_, err = ch.Webhook(h, tampered)
	require.Equal(t, pg.Invalid, pg.KindOf(err))

	late := h.Clone()
	late.Set(toss.TimeHeader, time.Now().Add(time.Hour).Format(time.RFC3339))
	_, err = ch.Webhook(late, body)
	require.Equal(t, pg.Invalid, pg.KindOf(err), "the time is signed with the body")

	// A webhook may carry more than one signature, and one that holds is
	// enough.
	both := h.Clone()
	both.Set(toss.SignatureHeader, "v1:"+base64.StdEncoding.EncodeToString(make([]byte, 32))+","+h.Get(toss.SignatureHeader))
	n, err := ch.Webhook(both, body)
	require.NoError(t, err)
	require.True(t, n.Signed)

	// Unsigned is a notice to recheck, unless webhooks are to be signed.
	unsigned := h.Clone()
	unsigned.Del(toss.SignatureHeader)
	n, err = ch.Webhook(unsigned, body)
	require.NoError(t, err)
	require.False(t, n.Signed)

	c := srv.Config()
	c.Signed = true
	strict, err := toss.New(c)
	require.NoError(t, err)
	_, err = strict.Webhook(unsigned, body)
	require.Equal(t, pg.Invalid, pg.KindOf(err))
	_, err = strict.Webhook(h, body)
	require.NoError(t, err)
}

// The 보안 키 is 64 hex digits, and the documentation does not say whether
// they are the HMAC's key as written or decoded: a signature under either
// holds, and under any other key does not.
func TestASignatureHoldsUnderTheKeyWrittenOrDecoded(t *testing.T) {
	srv := tosstest.New(t)
	key := strings.Repeat("0123456789abcdef", 4)
	c := srv.Config()
	c.WebhookKey = tosstest.Secret(key)
	ch, err := toss.New(c)
	require.NoError(t, err)

	body := []byte(`{"eventType":"PAYMENT_STATUS_CHANGED","createdAt":"2026-10-31T12:00:00.000000","data":{"paymentKey":"a-payment","orderId":"order-0001"}}`)
	at := "2026-10-31T12:00:01+09:00"
	sign := func(k []byte) http.Header {
		m := hmac.New(sha256.New, k)
		m.Write(body)
		m.Write([]byte(":" + at))
		h := http.Header{}
		h.Set(toss.TimeHeader, at)
		h.Set(toss.SignatureHeader, "v1:"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
		return h
	}
	decoded, err := hex.DecodeString(key)
	require.NoError(t, err)

	for _, k := range [][]byte{[]byte(key), decoded} {
		n, err := ch.Webhook(sign(k), body)
		require.NoError(t, err)
		require.True(t, n.Signed)
	}
	_, err = ch.Webhook(sign([]byte("another key")), body)
	require.Equal(t, pg.Invalid, pg.KindOf(err))
}

// A webhook sent again is sent with a new time, a new delivery id and a new
// signature, and the same body: it is the same notice.
func TestAWebhookSentAgainIsTheSameNotice(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)
	p, err := ch.Charge(t.Context(), charge(m, "order-0001", 10_000))
	require.NoError(t, err)

	h, body := srv.Webhook(p.ID)
	first, err := ch.Webhook(h, body)
	require.NoError(t, err)
	retried := srv.Sign(body)
	retried.Set("tosspayments-webhook-transmission-retried-count", "1")
	second, err := ch.Webhook(retried, body)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	_, err = ch.Cancel(t.Context(), pg.Cancel{Payment: p.ID, All: true, Reason: "해지", Key: "refund-1"})
	require.NoError(t, err)
	h, body = srv.Webhook(p.ID)
	third, err := ch.Webhook(h, body)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, third.ID, "another event is another notice")

	// A webhook that has an eventId is kept once by it.
	withID := []byte(`{"eventType":"PAYMENT_STATUS_CHANGED","eventId":"evt-1","data":{"paymentKey":"` + p.ID + `"}}`)
	n, err := ch.Webhook(srv.Sign(withID), withID)
	require.NoError(t, err)
	require.Equal(t, "evt-1", n.ID)
}

// Toss's word that it deleted a billing key names the key: the notice keeps
// only its digest, and there is no recheck of it.
func TestAMethodWebhookKeepsOnlyTheKeysDigest(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)

	body, err := json.Marshal(map[string]any{
		"eventType": "BILLING_DELETED",
		"createdAt": "2026-10-31T12:00:00.000000",
		"data":      map[string]any{"billingKey": m.Key.Reveal(), "reason": "삭제 API 요청"},
	})
	require.NoError(t, err)
	n, err := ch.Webhook(srv.Sign(body), body)
	require.NoError(t, err)
	require.Equal(t, pg.NoticeMethod, n.Type)
	require.Equal(t, m.Key.Digest(), n.Method)
	kept, err := json.Marshal(n)
	require.NoError(t, err)
	require.NotContains(t, string(kept), m.Key.Reveal())
	require.NotContains(t, fmt.Sprintf("%+v", n), m.Key.Reveal())

	_, err = ch.Recheck(t.Context(), n)
	require.Equal(t, pg.Invalid, pg.KindOf(err))
}

func TestAWebhookAboutNothingLevyanDoesIsAnsweredAndDropped(t *testing.T) {
	srv, ch := setup(t)
	for _, body := range []string{
		`{"createdAt":"2022-01-01T00:00:00.000000","secret":"s","status":"DONE","transactionKey":"t","orderId":"order-0001"}`,
		`{"eventType":"payout.changed","eventId":"evt-2","entityType":"payout","entityBody":{}}`,
	} {
		n, err := ch.Webhook(srv.Sign([]byte(body)), []byte(body))
		require.NoError(t, err)
		require.Equal(t, pg.NoticeOther, n.Type)
	}
	for _, body := range []string{`not json`, `{"eventType":"PAYMENT_STATUS_CHANGED","data":{}}`, `{"eventType":"BILLING_DELETED","data":{}}`} {
		_, err := ch.Webhook(srv.Sign([]byte(body)), []byte(body))
		require.Equal(t, pg.Invalid, pg.KindOf(err), body)
	}
}

func TestTossRefusingLevyanIsAuthAndToldToWaitIsRateLimited(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()

	c := srv.Config()
	c.SecretKey = tosstest.Secret("test_sk_not-the-key")
	wrong, err := toss.New(c)
	require.NoError(t, err)
	_, err = wrong.Payment(ctx, "order-0001")
	require.Equal(t, pg.Auth, pg.KindOf(err))

	srv.Fail(tosstest.Query, tosstest.Fault{Status: http.StatusTooManyRequests})
	_, err = ch.Payment(ctx, "order-0001")
	require.Equal(t, pg.RateLimited, pg.KindOf(err))

	srv.Fail(tosstest.Query, tosstest.Fault{Status: http.StatusForbidden, Code: "FORBIDDEN_CONSECUTIVE_REQUEST"})
	_, err = ch.Payment(ctx, "order-0001")
	require.Equal(t, pg.RateLimited, pg.KindOf(err))

	srv.Fail(tosstest.Query, tosstest.Fault{Status: http.StatusForbidden, Code: "API_KEY_ACCESS_DENIED"})
	_, err = ch.Payment(ctx, "order-0001")
	require.Equal(t, pg.Auth, pg.KindOf(err))

	m := register(t, srv, ch)
	_, err = ch.Charge(ctx, charge(m, "order-0001", 50))
	require.Equal(t, pg.Invalid, pg.KindOf(err), "a card takes no charge under 100 won")
	var e *pg.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "BELOW_MINIMUM_AMOUNT", e.Code)
}

// The charge's URL names the billing key, and an HTTP client's error names
// the URL: none of what the channel says carries either.
func TestNothingTheChannelSaysCarriesTheBillingKey(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)
	key := m.Key.Reveal()

	srv.Fail(tosstest.Charge, tosstest.Fault{Hang: true})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)
	require.NotContains(t, err.Error(), url.PathEscape(key))
	require.NotContains(t, fmt.Sprintf("%+v %#v", err, err), key)

	gone, err := toss.New(toss.Config{SecretKey: tosstest.Secret(srv.SecretKey), URL: "http://127.0.0.1:1"})
	require.NoError(t, err)
	_, err = gone.Charge(t.Context(), charge(m, "order-0002", 15_000))
	require.Equal(t, pg.Unknown, pg.KindOf(err))
	require.NotContains(t, err.Error(), key)
	require.NotContains(t, err.Error(), url.PathEscape(key))
	err = gone.RemoveMethod(t.Context(), m)
	require.Error(t, err)
	require.NotContains(t, err.Error(), url.PathEscape(key))

	require.NotContains(t, fmt.Sprintf("%v %+v", m, m), key)
}

func TestARemovedMethodChargesNothing(t *testing.T) {
	srv, ch := setup(t)
	ctx := t.Context()
	m := register(t, srv, ch)

	require.NoError(t, ch.RemoveMethod(ctx, m))
	require.NoError(t, ch.RemoveMethod(ctx, m), "a key Toss no longer has is removed")

	p, err := ch.Charge(ctx, charge(m, "order-0001", 15_000))
	require.NoError(t, err)
	require.Equal(t, pg.Failed, p.Status)
	require.Equal(t, pg.Declined, p.Failure.Kind, "the payer registers another")
	require.Equal(t, "NOT_FOUND_BILLING", p.Failure.Code)
	require.Equal(t, 0, srv.Approved())
}

func TestAChargeThatCannotBeIsNotSent(t *testing.T) {
	srv, ch := setup(t)
	m := register(t, srv, ch)
	ok := charge(m, "order-0001", 15_000)

	for name, c := range map[string]func(c *pg.Charge){
		"no key":           func(c *pg.Charge) { c.Method.Key = pg.BillingKey{} },
		"no customer":      func(c *pg.Charge) { c.Method.Customer = "" },
		"a short order":    func(c *pg.Charge) { c.Order = "o-1" },
		"an order of more": func(c *pg.Charge) { c.Order = "order/0001" },
		"dollars":          func(c *pg.Charge) { c.Amount = money.Amount{Minor: 1_500, Currency: "USD"} },
		"nothing":          func(c *pg.Charge) { c.Amount = won(0) },
		"no name":          func(c *pg.Charge) { c.Name = "" },
		"a long name":      func(c *pg.Charge) { c.Name = strings.Repeat("가", 101) },
		"no idempotency":   func(c *pg.Charge) { c.Key = "" },
		"a long key":       func(c *pg.Charge) { c.Key = strings.Repeat("k", 301) },
	} {
		t.Run(name, func(t *testing.T) {
			c2 := ok
			c(&c2)
			_, err := ch.Charge(t.Context(), c2)
			require.Equal(t, pg.Invalid, pg.KindOf(err))
		})
	}
	require.Zero(t, srv.Requests(tosstest.Charge))

	_, err := ch.Payment(t.Context(), "o/1")
	require.Equal(t, pg.Invalid, pg.KindOf(err))
	require.Zero(t, srv.Requests(tosstest.Query))
}

func TestNewRefusesAChannelThatCannotWork(t *testing.T) {
	srv := tosstest.New(t)
	for name, f := range map[string]func(c *toss.Config){
		"no secret key":          func(c *toss.Config) { c.SecretKey = tosstest.Secret("") },
		"a widget key":           func(c *toss.Config) { c.SecretKey = tosstest.Secret("test_gsk_x") },
		"keys of two worlds":     func(c *toss.Config) { c.ClientKey = "live_ck_x" },
		"signed with no key":     func(c *toss.Config) { c.Signed, c.WebhookKey = true, tosstest.Secret("") },
		"an API that is no URL":  func(c *toss.Config) { c.URL = "api.tosspayments.com" },
		"an API of another sort": func(c *toss.Config) { c.URL = "ftp://api.tosspayments.com" },
	} {
		t.Run(name, func(t *testing.T) {
			c := srv.Config()
			f(&c)
			_, err := toss.New(c)
			require.Error(t, err)
		})
	}

	c := srv.Config()
	c.ClientKey = ""
	ch, err := toss.New(c)
	require.NoError(t, err, "a channel that only charges needs no client key")
	_, err = ch.StartRegistration(t.Context(), back)
	require.Equal(t, pg.Invalid, pg.KindOf(err), "but one that opens the window does")
	require.Equal(t, "toss", ch.Name())
}

func TestARegistrationStartsFromWhereTheWindowSendsThePayerBack(t *testing.T) {
	_, ch := setup(t)
	for _, s := range []pg.Start{
		{Success: "/methods/back", Fail: back.Fail},
		{Success: back.Success, Fail: ""},
		{Success: "javascript:alert(1)", Fail: back.Fail},
		{Success: back.Success, Fail: back.Fail, Customer: "short"},
		{Success: back.Success, Fail: back.Fail, Customer: "abcdefghijklmnopqrstuvwxyz"}, // nothing but letters
	} {
		_, err := ch.StartRegistration(t.Context(), s)
		require.Equal(t, pg.Invalid, pg.KindOf(err), "%+v", s)
	}

	reg, err := ch.StartRegistration(t.Context(), pg.Start{Success: back.Success, Fail: back.Fail, Customer: "lv-an-account-of-its-own"})
	require.NoError(t, err)
	require.Equal(t, "lv-an-account-of-its-own", reg.Customer, "a billing account's customer, kept from before")
}
