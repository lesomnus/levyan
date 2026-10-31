package toss

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lesomnus/levyan/internal/pg"
)

// The headers a webhook comes with
// (https://docs.tosspayments.com/reference/using-api/webhook-events#웹훅-헤더).
const (
	SignatureHeader = "tosspayments-webhook-signature"
	TimeHeader      = "tosspayments-webhook-transmission-time"
)

var (
	errSignature = errors.New("its signature does not hold under the webhook key")
	errUnsigned  = errors.New("it carries no signature, and webhooks are to be signed")
)

// Webhook reads one of Toss's webhooks into a [pg.Notice]. It checks the
// signature when there is a key to check it with, and reads of the body only
// which payment or key it names: everything else of it is Toss's word, which
// a recheck takes instead.
//
// PAYMENT_STATUS_CHANGED has no eventId, so the notice is kept once by its
// body's digest, which a delivery sent again repeats; a webhook with one is
// kept once by it.
func (c *Channel) Webhook(h http.Header, body []byte) (pg.Notice, error) {
	const op = "webhook"
	signed, err := c.signed(h, body)
	if err != nil {
		return pg.Notice{}, err
	}

	var v struct {
		EventType string          `json:"eventType"`
		EventID   string          `json:"eventId"`
		CreatedAt string          `json:"createdAt"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return pg.Notice{}, &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New("it is no JSON object")}
	}
	n := pg.Notice{ID: v.EventID, Event: v.EventType, Signed: signed}
	if n.ID == "" {
		n.ID = "sha256:" + pg.Digest(string(body))
	}
	// A time that cannot be read is no time: it is believed of nothing
	// either way.
	n.At, _ = when(v.CreatedAt)

	switch v.EventType {
	case "PAYMENT_STATUS_CHANGED":
		var d struct {
			PaymentKey string `json:"paymentKey"`
			OrderID    string `json:"orderId"`
		}
		_ = json.Unmarshal(v.Data, &d)
		switch {
		case idOK(d.PaymentKey, 200):
		case d.PaymentKey == "" && orderOK(d.OrderID):
		default:
			return pg.Notice{}, &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New("it names no payment")}
		}
		if d.OrderID != "" && !orderOK(d.OrderID) {
			d.OrderID = ""
		}
		n.Type, n.Payment, n.Order = pg.NoticePayment, d.PaymentKey, d.OrderID

	case "BILLING_DELETED":
		var d struct {
			BillingKey string `json:"billingKey"`
		}
		_ = json.Unmarshal(v.Data, &d)
		if !idOK(d.BillingKey, 200) {
			return pg.Notice{}, &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New("it names no billing key")}
		}
		// The key is a secret even in a webhook: only its digest is kept.
		n.Type, n.Method = pg.NoticeMethod, pg.Digest(d.BillingKey)

	default:
		// A virtual account's DEPOSIT_CALLBACK, which has no eventType, and
		// whatever else levyan is not registered for.
		n.Type = pg.NoticeOther
	}
	return n, nil
}

// signed is whether the webhook's signature holds, and an error for one that
// is to be refused.
func (c *Channel) signed(h http.Header, body []byte) (bool, error) {
	const op = "webhook"
	sig := h.Get(SignatureHeader)
	if c.c.WebhookKey.IsZero() {
		return false, nil
	}
	if sig == "" {
		if c.c.Signed {
			return false, &pg.Error{Op: op, Kind: pg.Invalid, Err: errUnsigned}
		}
		return false, nil
	}
	key, err := c.c.WebhookKey.Value()
	if err != nil || key == "" {
		// Not the webhook's fault: refused as a failure of levyan's, which
		// Toss sends again.
		return false, &pg.Error{Op: op, Kind: pg.Unknown, Err: errors.New("the webhook key could not be read")}
	}

	msg := make([]byte, 0, len(body)+1+len(h.Get(TimeHeader)))
	msg = append(append(append(msg, body...), ':'), h.Get(TimeHeader)...)

	// The key as it is written, and its 64 hex digits decoded: the
	// documentation says the one and the payouts guide decodes it for JWE
	// (package doc). Either takes the key to make.
	keys := [][]byte{[]byte(key)}
	if b, err := hex.DecodeString(key); err == nil && len(b) > 0 {
		keys = append(keys, b)
	}
	for _, s := range strings.Split(sig, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(s), "v1:")
		if !ok {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		for _, k := range keys {
			m := hmac.New(sha256.New, k)
			m.Write(msg)
			if hmac.Equal(m.Sum(nil), got) {
				return true, nil
			}
		}
	}
	return false, &pg.Error{Op: op, Kind: pg.Invalid, Err: errSignature}
}

// Recheck is the payment a payment notice names, as Toss's API answers it
// now: GET /v1/payments/{paymentKey}, or by its orderId for a notice that
// names no key. What the notice said of it is not looked at, and where the
// API disagrees with what it named, the API is the payment.
func (c *Channel) Recheck(ctx context.Context, n pg.Notice) (pg.Payment, error) {
	const op = "recheck"
	switch {
	case n.Type != pg.NoticePayment:
		return pg.Payment{}, invalid(op, "the notice names no payment to recheck")
	case n.Payment != "":
		if !idOK(n.Payment, 200) {
			return pg.Payment{}, invalid(op, "the notice names no paymentKey")
		}
		return c.payment(ctx, op, "/v1/payments/"+segment(n.Payment))
	default:
		if !orderOK(n.Order) {
			return pg.Payment{}, invalid(op, "the notice names no orderId")
		}
		return c.payment(ctx, op, "/v1/payments/orders/"+segment(n.Order))
	}
}
