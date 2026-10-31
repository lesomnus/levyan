package paddle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/levyan/internal/mor"
)

// SignatureHeader is the header a webhook's signature comes in
// (https://developer.paddle.com/webhooks/about/signature-verification).
const SignatureHeader = "Paddle-Signature"

var (
	errUnsigned  = errors.New("it carries no signature, and every webhook of Paddle's does")
	errHeader    = errors.New("its signature is no ts=…;h1=… Paddle writes")
	errSignature = errors.New("its signature does not hold under the webhook secret")
	errStale     = errors.New("it was signed too far from now: a webhook replayed, or a clock that is off")
)

// Webhook reads one of Paddle's webhooks into a [mor.Notice]. It checks the
// signature, and that it was signed within the tolerance of now, and reads of
// the body only what it names: everything else of it is Paddle's word, which
// a recheck takes instead.
//
// A webhook is kept once by its event_id, which every delivery of the event
// repeats -- a delivery sent again is signed again, and is the same notice. A
// delivery replayed by somebody else carries the time it was signed at, and
// is refused once that is past the tolerance; within it, it is the same
// notice too, which the inbox has.
func (c *Channel) Webhook(h http.Header, body []byte) (mor.Notice, error) {
	const op = "webhook"
	if err := c.signed(h, body); err != nil {
		return mor.Notice{}, err
	}

	var v event
	if err := json.Unmarshal(body, &v); err != nil {
		return mor.Notice{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("it is no JSON object")}
	}
	return v.notice(op)
}

// event is one of Paddle's events, as a webhook delivers it and as the event
// stream lists it (https://developer.paddle.com/webhooks/about/how-webhooks-work,
// https://developer.paddle.com/api-reference/events/list-events): of its
// data, only what it names is read.
type event struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt string          `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// notice is what e names, as levyan keeps a notice: its id, what it is
// about, and the ids of that.
func (e event) notice(op string) (mor.Notice, error) {
	if !idOK("evt", e.EventID) {
		return mor.Notice{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("it names no event")}
	}
	n := mor.Notice{ID: e.EventID, Event: e.EventType}
	// A time that cannot be read is no time: it is believed of nothing
	// either way.
	n.At, _ = when(e.OccurredAt)

	var d struct {
		ID             string  `json:"id"`
		TransactionID  string  `json:"transaction_id"`
		SubscriptionID *string `json:"subscription_id"`
	}
	_ = json.Unmarshal(e.Data, &d)
	sub := ""
	if d.SubscriptionID != nil && idOK("sub", *d.SubscriptionID) {
		sub = *d.SubscriptionID
	}
	entity, _, _ := strings.Cut(e.EventType, ".")
	switch entity {
	case "transaction":
		if !idOK("txn", d.ID) {
			return mor.Notice{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("it names no transaction")}
		}
		n.Type, n.Transaction, n.Subscription = mor.NoticeTransaction, d.ID, sub
	case "adjustment":
		// What an adjustment did is what its transaction is now: a refund
		// approved, a chargeback, are rechecked as the transaction.
		if !idOK("adj", d.ID) || !idOK("txn", d.TransactionID) {
			return mor.Notice{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("it names no adjustment of a transaction")}
		}
		n.Type, n.Transaction, n.Adjustment, n.Subscription = mor.NoticeTransaction, d.TransactionID, d.ID, sub
	case "subscription":
		if !idOK("sub", d.ID) {
			return mor.Notice{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("it names no subscription")}
		}
		n.Type, n.Subscription = mor.NoticeSubscription, d.ID
	default:
		// A customer, an address, a payment method saved, and whatever else
		// a destination is subscribed to that levyan does not act on.
		n.Type = mor.NoticeOther
	}
	return n, nil
}

// eventsPage is the most of the event stream one read asks for: Paddle's
// most (https://developer.paddle.com/api-reference/events/list-events).
const eventsPage = 200

// Notices is GET /events, oldest first: after the event r.After names, or
// from r.Since for a first read. An event that names nothing it should --
// a transaction's with no transaction -- is no notice, and is passed over:
// it would be refused as a webhook.
func (c *Channel) Notices(ctx context.Context, r mor.Read) (mor.Notices, error) {
	const op = "notices"
	q := url.Values{
		"order_by": {"id[ASC]"},
		"per_page": {strconv.Itoa(eventsPage)},
	}
	switch {
	case r.After != "":
		if !idOK("evt", r.After) {
			return mor.Notices{}, invalid(op, "a read begins after an event of Paddle's")
		}
		q.Set("after", r.After)
	case !r.Since.IsZero():
		q.Set("from", r.Since.UTC().Format(time.RFC3339))
	}
	var es []event
	m, err := c.call(ctx, op, http.MethodGet, "/events", q, nil, &es)
	if err != nil {
		return mor.Notices{}, err
	}
	out := mor.Notices{More: m.Pagination != nil && m.Pagination.HasMore && len(es) > 0}
	for _, e := range es {
		if idOK("evt", e.EventID) {
			out.Last = e.EventID
		}
		n, err := e.notice(op)
		if err != nil {
			continue
		}
		out.Notices = append(out.Notices, n)
	}
	return out, nil
}

// signed is nil for a webhook whose signature holds and is fresh, and the
// error to refuse it with otherwise.
func (c *Channel) signed(h http.Header, body []byte) error {
	const op = "webhook"
	if c.c.WebhookSecret.IsZero() {
		// Not the webhook's fault: refused as a failure of levyan's, which
		// Paddle sends again.
		return &mor.Error{Op: op, Kind: mor.Unknown, Err: errors.New("there is no webhook secret to check it with")}
	}
	secret, err := c.c.WebhookSecret.Value()
	if err != nil || secret == "" {
		return &mor.Error{Op: op, Kind: mor.Unknown, Err: errors.New("the webhook secret could not be read")}
	}
	sig := h.Get(SignatureHeader)
	if sig == "" {
		return &mor.Error{Op: op, Kind: mor.Invalid, Err: errUnsigned}
	}

	ts := ""
	var sums [][]byte
	for _, part := range strings.Split(sig, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return &mor.Error{Op: op, Kind: mor.Invalid, Err: errHeader}
		}
		switch k {
		case "ts":
			if ts != "" {
				return &mor.Error{Op: op, Kind: mor.Invalid, Err: errHeader}
			}
			ts = v
		case "h1":
			// More than one while Paddle rotates a secret: one that holds
			// is enough.
			if b, err := hex.DecodeString(v); err == nil && len(b) == sha256.Size {
				sums = append(sums, b)
			}
		}
	}
	at, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sums) == 0 {
		return &mor.Error{Op: op, Kind: mor.Invalid, Err: errHeader}
	}

	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte(":"))
	m.Write(body)
	want := m.Sum(nil)
	held := false
	for _, s := range sums {
		if hmac.Equal(s, want) {
			held = true
		}
	}
	if !held {
		return &mor.Error{Op: op, Kind: mor.Invalid, Err: errSignature}
	}

	// The time is signed with the body, so a webhook replayed later cannot
	// say it was signed now.
	if d := c.now().Sub(time.Unix(at, 0)); d > c.c.Tolerance || d < -c.c.Tolerance {
		return &mor.Error{Op: op, Kind: mor.Invalid, Err: errStale}
	}
	return nil
}

// Recheck is what a notice names as Paddle's API answers it now: the
// transaction, with its adjustments, for a transaction's or an adjustment's
// notice; the subscription for a subscription's. What the notice said of it
// is not looked at.
func (c *Channel) Recheck(ctx context.Context, n mor.Notice) (mor.Rechecked, error) {
	const op = "recheck"
	switch n.Type {
	case mor.NoticeTransaction:
		if !idOK("txn", n.Transaction) {
			return mor.Rechecked{}, invalid(op, "the notice names no transaction")
		}
		t, err := c.transaction(ctx, op, n.Transaction)
		if err != nil {
			return mor.Rechecked{}, err
		}
		return mor.Rechecked{Transaction: &t}, nil
	case mor.NoticeSubscription:
		if !idOK("sub", n.Subscription) {
			return mor.Rechecked{}, invalid(op, "the notice names no subscription")
		}
		s, _, err := c.subscription(ctx, op, n.Subscription, false)
		if err != nil {
			return mor.Rechecked{}, err
		}
		return mor.Rechecked{Subscription: &s}, nil
	default:
		return mor.Rechecked{}, invalid(op, "the notice names nothing to recheck")
	}
}
