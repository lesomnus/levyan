// Package toss is a [pg.Channel] for Toss Payments (토스페이먼츠): automatic
// billing (자동결제) of a card the payer registered in Toss's window.
//
// **It is a candidate.** Which PG levyan sells through in Korea is the
// owner's decision and has not been made (levyan#10). Toss is the leading
// candidate, and this adapter is here so that it can be tried against Toss's
// test environment and so that what the rest of levyan asks of a PG is
// written down -- not because the choice is made. Nothing outside the wiring
// names this package.
//
// # What it relies on
//
// As Toss's documentation has it on 2026-10-31:
//
//   - Automatic billing is a contract of its own, made after a risk review,
//     and is for subscriptions: "정기 구독형 서비스가 아니라면 정책적으로
//     자동결제 사용이 제한" (https://docs.tosspayments.com/guides/v2/billing).
//     It is called with the API 개별 연동 keys of the MID it is contracted on,
//     test_ck_/test_sk_ or live_ck_/live_sk_; the 결제위젯 keys (_gck_,
//     _gsk_) cannot ask a payment by its orderId
//     (https://docs.tosspayments.com/reference/using-api/api-keys).
//   - A card is registered in Toss's window: the v2 SDK
//     (https://js.tosspayments.com/v2/standard), TossPayments(clientKey)
//     .payment({customerKey}).requestBillingAuth({method: "CARD", successUrl,
//     failUrl}), which sends the payer to successUrl with customerKey and a
//     one-time authKey, or to failUrl with code and message
//     (https://docs.tosspayments.com/guides/v2/billing/integration,
//     https://docs.tosspayments.com/sdk/v2/js/payment#paymentrequestbillingauth).
//     The window asks for the card's number, its expiry and a birth date, and
//     never its password: a contract that wants the password cannot use it.
//     It takes cards issued in Korea only.
//   - POST /v1/billing/authorizations/issue {authKey, customerKey} answers the
//     billing key (https://docs.tosspayments.com/reference#인증-정보authkey로-빌링키-발급),
//     and nothing answers it again: "발급된 빌링키를 조회하는 API는 제공되지
//     않습니다" (https://docs.tosspayments.com/guides/v2/billing). A key is
//     good for as long as its card is.
//   - POST /v1/billing/{billingKey} {customerKey, amount, orderId, orderName}
//     charges it, and may take a minute: "자동결제 승인은 최대 60초가
//     소요됩니다. 타임아웃 값을 최소 60초로 설정하세요"
//     (https://docs.tosspayments.com/reference#자동결제-승인). A card takes no
//     charge under 100 won (BELOW_MINIMUM_AMOUNT).
//   - GET /v1/payments/orders/{orderId} and GET /v1/payments/{paymentKey}
//     answer a payment (https://docs.tosspayments.com/reference#orderid로-결제-조회).
//   - POST /v1/payments/{paymentKey}/cancel {cancelReason, cancelAmount}
//     gives back cancelAmount, or what is left without one
//     (https://docs.tosspayments.com/reference#결제-취소); a card payment can
//     be canceled for about a year (https://docs.tosspayments.com/guides/v2/cancel-payment).
//   - DELETE /v1/billing/{billingKey} deletes a key
//     (https://docs.tosspayments.com/reference#빌링키-삭제).
//   - Every call is HTTP Basic with the secret key as the user and no
//     password, "Basic base64(secretKey + ':')"
//     (https://docs.tosspayments.com/reference/using-api/authorization#인증-헤더).
//   - Every POST takes an Idempotency-Key of at most 300 characters. Toss
//     keeps it for 15 days, for one API key, URL and method, and answers a
//     request sent again under it with the first answer; while the first is
//     still being worked on it answers 409 IDEMPOTENT_REQUEST_PROCESSING. It
//     warns against sending a request that was answered with an error again
//     under a new key before the cause is known
//     (https://docs.tosspayments.com/reference/using-api/authorization#멱등키-헤더).
//   - An orderId approved or canceled before is refused with
//     DUPLICATED_ORDER_ID (https://docs.tosspayments.com/reference/error-codes#카드-자동결제-승인).
//   - A refusal is an HTTP status and {code, message}, and codes are added
//     without a new version (https://docs.tosspayments.com/reference/using-api/req-res#에러-객체,
//     https://docs.tosspayments.com/reference/error-codes,
//     https://docs.tosspayments.com/reference/versioning). A 5xx is an
//     unknown outcome here whatever its code says.
//   - Webhooks are a POST of JSON, answered 200 within 10 seconds or sent
//     again, 7 times over 3 days and 19 hours
//     (https://docs.tosspayments.com/guides/v2/webhook). A charge of a
//     billing key is approved as it is asked, and sends no webhook for it:
//     "자동결제와 키인 결제는 결제 요청에 이어서 승인이 되기 때문에 결제가
//     완료됐을 때 웹훅을 전송하지 않습니다"; PAYMENT_STATUS_CHANGED, for what
//     changes after, carries the Payment and no eventId
//     (https://docs.tosspayments.com/reference/using-api/webhook-events).
//   - A webhook may carry tosspayments-webhook-signature: "v1:" and the
//     base64 of an HMAC-SHA256 of "{body}:{tosspayments-webhook-transmission-time}"
//     under the 보안 키, and maybe more than one, separated by commas. The
//     documentation does not say which webhooks carry one, nor whether the
//     key's 64 hex digits are the HMAC's key as they are or decoded; so a
//     signature holds under either, and one that is not there is refused only
//     when [Config.Signed] says so. Whatever it carries, a payment webhook is
//     checked against the payment query API, which Toss's own security
//     checklist calls the defence that matters
//     (https://docs.tosspayments.com/reference/using-api/security-checklist).
//   - The test environment approves without moving money, and reproduces an
//     error asked for in the header TossPayments-Test-Code
//     (https://docs.tosspayments.com/guides/v2/get-started/environment).
package toss

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pg"
)

// API is Toss's API, which a [Config] with no URL calls.
const API = "https://api.tosspayments.com"

// SDK is the script a page opens Toss's window with.
const SDK = "https://js.tosspayments.com/v2/standard"

// Timeout is how long a request waits by default: longer than the minute a
// charge may take (https://docs.tosspayments.com/reference#자동결제-승인).
const Timeout = 70 * time.Second

// Config is how the adapter reaches Toss.
type Config struct {
	// SecretKey is the API 개별 연동 secret key of the MID automatic billing is
	// contracted on, test_sk_… or live_sk_…. It is read every time it is used,
	// so a key reissued into its file is the next request's.
	SecretKey cfg.Secret `yaml:"secret_key"`

	// ClientKey is the client key that goes with it, test_ck_… or live_ck_…:
	// public, the window's. It is needed only to start a registration.
	ClientKey string `yaml:"client_key"`

	// WebhookKey is the 보안 키, which signs Toss's webhooks. Without it no
	// signature is checked, and a webhook is still believed only as its
	// recheck answers it.
	WebhookKey cfg.Secret `yaml:"webhook_key"`

	// Signed refuses a webhook that carries no signature. Set it once Toss
	// signs every webhook levyan is registered for, which its documentation
	// does not say it does.
	Signed bool `yaml:"signed"`

	// URL is the API it calls: [API] when empty, a fake's in a test.
	URL string `yaml:"url"`

	// Client is what sends the requests: one with [Timeout] when nil. It must
	// not record URLs -- a charge's path names the billing key.
	Client *http.Client `yaml:"-"`
}

// Channel is Toss Payments as a [pg.Channel]. It holds nothing that changes,
// and is used by many goroutines at once.
type Channel struct {
	c      Config
	url    string
	client *http.Client
}

var _ pg.Channel = (*Channel)(nil)

// New is a channel to Toss as c says.
func New(c Config) (*Channel, error) {
	if c.SecretKey.IsZero() {
		return nil, errors.New("toss: no secret key")
	}
	if c.Signed && c.WebhookKey.IsZero() {
		return nil, errors.New("toss: webhooks are to be signed, and there is no key to check them with")
	}
	if sk, err := c.SecretKey.Value(); err == nil {
		// A key is read again on every call, and may not be there yet; one
		// that is can be told wrong now rather than at the first charge.
		if strings.Contains(sk, "_gsk_") {
			return nil, errors.New("toss: the secret key is a 결제위젯 key; automatic billing is called with the API 개별 연동 keys")
		}
		if env(sk) != "" && env(c.ClientKey) != "" && env(sk) != env(c.ClientKey) {
			return nil, errors.New("toss: the secret key and the client key are of different environments")
		}
	}

	u := c.URL
	if u == "" {
		u = API
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return nil, fmt.Errorf("toss: %q is no URL of an API", u)
	}

	client := c.Client
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// Toss asks that a connection is not kept idle for longer than its
		// addresses are good for (https://docs.tosspayments.com/reference/using-api/network-requirements, D3).
		t.IdleConnTimeout = time.Minute
		client = &http.Client{
			Timeout:   Timeout,
			Transport: t,
			// Toss's API answers where it is asked. An answer that sends the
			// request elsewhere is no answer to it -- an unknown one, for a
			// charge -- and is not followed with the secret key.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Channel{c: c, url: strings.TrimSuffix(u, "/"), client: client}, nil
}

// env is which environment a key is of, by the prefix Toss promises to keep
// (https://docs.tosspayments.com/reference/using-api/req-res#식별자): "test",
// "live", or "" for a key of neither.
func env(key string) string {
	for _, e := range []string{"test", "live"} {
		if strings.HasPrefix(key, e+"_") {
			return e
		}
	}
	return ""
}

func (c *Channel) secret() (string, error) {
	sk, err := c.c.SecretKey.Value()
	if err != nil {
		return "", fmt.Errorf("the secret key could not be read: %w", err)
	}
	if sk == "" {
		return "", errors.New("no secret key")
	}
	return sk, nil
}

func (c *Channel) Name() string { return "toss" }

func invalid(op, why string) *pg.Error {
	return &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New(why)}
}

// StartRegistration is the window of Toss's SDK v2, for a card: what a page
// hands TossPayments(clientKey).payment({customerKey}).requestBillingAuth({
// method, successUrl, failUrl }). It asks nothing of Toss.
func (c *Channel) StartRegistration(_ context.Context, s pg.Start) (pg.Registration, error) {
	const op = "registration"
	if c.c.ClientKey == "" {
		return pg.Registration{}, invalid(op, "no client key, which the window is opened with")
	}
	for _, v := range []string{s.Success, s.Fail} {
		if u, err := url.Parse(v); err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
			return pg.Registration{}, invalid(op, "the window sends the payer back to an absolute URL")
		}
	}

	customer := s.Customer
	if customer == "" {
		// 29 characters, one of them a '-', none to guess: what Toss asks a
		// customerKey to be.
		customer = "lv-" + rand.Text()
	}
	if !customerOK(customer) {
		return pg.Registration{}, invalid(op, "the customer is no customerKey Toss takes")
	}

	return pg.Registration{
		Customer: customer,
		Window: pg.Window{
			Script: SDK,
			Values: map[string]string{
				"clientKey":   c.c.ClientKey,
				"customerKey": customer,
				"method":      "CARD",
				"successUrl":  s.Success,
				"failUrl":     s.Fail,
			},
		},
	}, nil
}

// CompleteRegistration exchanges the authKey the window sent the payer back
// with for the billing key. The exchange is sent under an idempotency key of
// the authKey's, so that a page reloaded is the same method -- and a billing
// key, which nothing answers again, is not lost to an answer that never came:
// sent again, the exchange is answered as it was the first time.
func (c *Channel) CompleteRegistration(ctx context.Context, r pg.Registration, back url.Values) (pg.Method, error) {
	const op = "registration"
	auth := back.Get("authKey")
	if auth == "" {
		if code := back.Get("code"); code != "" {
			// failUrl: the payer left the window, or the card was refused.
			return pg.Method{}, &pg.Error{Op: op, Kind: kind(http.StatusBadRequest, code), Code: code, Message: back.Get("message")}
		}
		return pg.Method{}, invalid(op, "the window sent back no authKey")
	}
	if ck := back.Get("customerKey"); ck != "" && ck != r.Customer {
		return pg.Method{}, invalid(op, "the window sent back another registration's customer")
	}
	if !customerOK(r.Customer) {
		return pg.Method{}, invalid(op, "the registration's customer is no customerKey Toss takes")
	}

	var b billing
	in := map[string]string{"authKey": auth, "customerKey": r.Customer}
	if err := c.call(ctx, op, http.MethodPost, "/v1/billing/authorizations/issue", "issue-"+pg.Digest(auth), in, &b); err != nil {
		return pg.Method{}, err
	}
	m, err := b.read()
	if err != nil {
		return pg.Method{}, &pg.Error{Op: op, Kind: pg.Unknown, Err: fmt.Errorf("the answer could not be read: %w", err)}
	}
	if m.Key.IsZero() {
		return pg.Method{}, &pg.Error{Op: op, Kind: pg.Unknown, Err: errors.New("the answer holds no billing key")}
	}
	if m.Customer == "" {
		m.Customer = r.Customer
	}
	if m.Customer != r.Customer {
		return pg.Method{}, &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New("the billing key was issued to another customer")}
	}
	return m, nil
}

// Charge is POST /v1/billing/{billingKey}.
func (c *Channel) Charge(ctx context.Context, ch pg.Charge) (pg.Payment, error) {
	const op = "charge"
	switch {
	case ch.Method.Key.IsZero():
		return pg.Payment{}, invalid(op, "no billing key")
	case !customerOK(ch.Method.Customer):
		return pg.Payment{}, invalid(op, "the method's customer is no customerKey Toss takes")
	case !orderOK(ch.Order):
		return pg.Payment{}, invalid(op, "the order is no orderId Toss takes: 6 to 64 letters, digits, '-', '_' or '='")
	case ch.Amount.Currency != "KRW":
		return pg.Payment{}, invalid(op, "Toss charges won, and the amount is in "+ch.Amount.Currency)
	case ch.Amount.Minor <= 0:
		return pg.Payment{}, invalid(op, "nothing to charge")
	case ch.Name == "" || chars(ch.Name) > 100:
		return pg.Payment{}, invalid(op, "the name is 1 to 100 characters")
	case ch.Key == "" || len(ch.Key) > 300:
		return pg.Payment{}, invalid(op, "the idempotency key is 1 to 300 characters")
	}

	in := struct {
		CustomerKey string `json:"customerKey"`
		Amount      int64  `json:"amount"`
		OrderID     string `json:"orderId"`
		OrderName   string `json:"orderName"`
	}{ch.Method.Customer, ch.Amount.Minor, ch.Order, ch.Name}

	var p payment
	err := c.call(ctx, op, http.MethodPost, "/v1/billing/"+segment(ch.Method.Key.Reveal()), ch.Key, in, &p)
	var e *pg.Error
	switch {
	case err == nil:
	case errors.As(err, &e) && e.Kind == pg.Declined:
		// The payment failed; the request did not.
		return pg.Payment{
			Order:   ch.Order,
			Status:  pg.Failed,
			Amount:  ch.Amount,
			Balance: money.Zero(ch.Amount.Currency),
			Failure: &pg.Failure{Kind: pg.Declined, Decline: decline(e.Code), Code: e.Code, Message: e.Message},
		}, nil
	case errors.As(err, &e) && e.Code == "DUPLICATED_ORDER_ID":
		// Approved before, under another key or none: the charge is the
		// payment it already is.
		return c.charged(ctx, ch)
	default:
		return pg.Payment{}, err
	}

	v, err := p.read()
	if err != nil {
		return pg.Payment{}, &pg.Error{Op: op, Kind: pg.Unknown, Status: http.StatusOK, Err: fmt.Errorf("the answer could not be read: %w", err)}
	}
	if v.Order != ch.Order || v.Amount != ch.Amount {
		// A first answer, to a key sent for another charge before.
		return pg.Payment{}, invalid(op, "answered for another charge: the idempotency key was used for one before")
	}
	return v, nil
}

// charged is the payment of an order Toss says it has approved before:
// asked, and held to the charge it is asked for again.
func (c *Channel) charged(ctx context.Context, ch pg.Charge) (pg.Payment, error) {
	v, err := c.Payment(ctx, ch.Order)
	if pg.KindOf(err) == pg.NotFound {
		// Toss has the order and does not find it: whatever that is, it is
		// not an order that was never charged.
		return pg.Payment{}, &pg.Error{Op: "charge", Kind: pg.Unknown, Code: "DUPLICATED_ORDER_ID", Err: errors.New("an order Toss calls a duplicate, and has no payment of")}
	}
	if err != nil {
		return pg.Payment{}, err
	}
	if v.Amount != ch.Amount {
		return pg.Payment{}, invalid("charge", fmt.Sprintf("the order was charged %s, not %s", v.Amount, ch.Amount))
	}
	return v, nil
}

// Payment is GET /v1/payments/orders/{orderId}.
func (c *Channel) Payment(ctx context.Context, order string) (pg.Payment, error) {
	const op = "payment"
	if !orderOK(order) {
		return pg.Payment{}, invalid(op, "the order is no orderId Toss takes")
	}
	return c.payment(ctx, op, "/v1/payments/orders/"+segment(order))
}

func (c *Channel) payment(ctx context.Context, op, path string) (pg.Payment, error) {
	var p payment
	if err := c.call(ctx, op, http.MethodGet, path, "", nil, &p); err != nil {
		return pg.Payment{}, err
	}
	v, err := p.read()
	if err != nil {
		return pg.Payment{}, &pg.Error{Op: op, Kind: pg.Unknown, Status: http.StatusOK, Err: fmt.Errorf("the answer could not be read: %w", err)}
	}
	return v, nil
}

// Cancel is POST /v1/payments/{paymentKey}/cancel.
func (c *Channel) Cancel(ctx context.Context, cn pg.Cancel) (pg.Payment, error) {
	const op = "cancel"
	switch {
	case !idOK(cn.Payment, 200):
		return pg.Payment{}, invalid(op, "the payment is no paymentKey")
	case cn.All && cn.Amount.Minor != 0:
		return pg.Payment{}, invalid(op, "all of what is left, or an amount: not both")
	case !cn.All && cn.Amount.Currency != "KRW":
		return pg.Payment{}, invalid(op, "Toss gives back won, and the amount is in "+cn.Amount.Currency)
	case !cn.All && cn.Amount.Minor <= 0:
		return pg.Payment{}, invalid(op, "nothing to give back")
	case cn.Reason == "" || chars(cn.Reason) > 200:
		return pg.Payment{}, invalid(op, "the reason is 1 to 200 characters")
	case cn.Key == "" || len(cn.Key) > 300:
		return pg.Payment{}, invalid(op, "the idempotency key is 1 to 300 characters")
	}

	in := struct {
		CancelReason string `json:"cancelReason"`
		CancelAmount int64  `json:"cancelAmount,omitempty"`
	}{cn.Reason, cn.Amount.Minor}

	var p payment
	err := c.call(ctx, op, http.MethodPost, "/v1/payments/"+segment(cn.Payment)+"/cancel", cn.Key, in, &p)
	var e *pg.Error
	switch {
	case err == nil:
	case cn.All && errors.As(err, &e) && e.Code == "ALREADY_CANCELED_PAYMENT":
		// Nothing is left of it, which is what was asked.
		v, qerr := c.payment(ctx, op, "/v1/payments/"+segment(cn.Payment))
		switch {
		case qerr != nil:
			return pg.Payment{}, qerr
		case v.Status != pg.Canceled:
			return pg.Payment{}, err
		}
		return v, nil
	default:
		return pg.Payment{}, err
	}

	v, err := p.read()
	if err != nil {
		return pg.Payment{}, &pg.Error{Op: op, Kind: pg.Unknown, Status: http.StatusOK, Err: fmt.Errorf("the answer could not be read: %w", err)}
	}
	if v.ID != "" && v.ID != cn.Payment {
		return pg.Payment{}, invalid(op, "answered for another payment: the idempotency key was used for one before")
	}
	return v, nil
}

// RemoveMethod is DELETE /v1/billing/{billingKey}, which Toss keeps once by
// itself (https://docs.tosspayments.com/reference/using-api/authorization#멱등키-헤더).
func (c *Channel) RemoveMethod(ctx context.Context, m pg.Method) error {
	const op = "removal"
	if m.Key.IsZero() {
		return invalid(op, "no billing key")
	}
	err := c.call(ctx, op, http.MethodDelete, "/v1/billing/"+segment(m.Key.Reveal()), "", nil, nil)
	var e *pg.Error
	if errors.As(err, &e) && e.Code == "NOT_FOUND_BILLING" {
		return nil
	}
	return err
}

// idOK is whether s may be one of Toss's identifiers, which are opaque: of
// printable ASCII and no space, and no longer than they are said to be
// (https://docs.tosspayments.com/reference/using-api/req-res#식별자).
func idOK(s string, most int) bool {
	if s == "" || len(s) > most || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
}
