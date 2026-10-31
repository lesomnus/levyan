package toss

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pg"
)

// maxAnswer is the most of an answer read: a payment is a few kilobytes.
const maxAnswer = 1 << 20

// call sends a request to Toss and reads its answer: a 2xx into out, and
// anything else as the [pg.Error] it is. key is the Idempotency-Key, for a
// POST that changes money; "" sends none.
//
// Nothing it returns names the URL, which for a charge names the billing key:
// a transport's error is unwrapped from the *url.Error that would.
func (c *Channel) call(ctx context.Context, op, method, path, key string, in, out any) error {
	secret, err := c.secret()
	if err != nil {
		return &pg.Error{Op: op, Kind: pg.Auth, Err: err}
	}

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &pg.Error{Op: op, Kind: pg.Invalid, Err: err}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return &pg.Error{Op: op, Kind: pg.Invalid, Err: errors.New("the request could not be made")}
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(secret+":")))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	res, err := c.client.Do(req)
	if err != nil {
		return &pg.Error{Op: op, Kind: pg.Unknown, Err: bare(err)}
	}
	defer res.Body.Close()

	b, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return &pg.Error{Op: op, Kind: pg.Unknown, Status: res.StatusCode, Err: fmt.Errorf("the answer could not be read: %w", bare(err))}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return refusal(op, res.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		// Answered, and for all that is known done: but what was done cannot
		// be read, so it is as good as no answer.
		return &pg.Error{Op: op, Kind: pg.Unknown, Status: res.StatusCode, Err: fmt.Errorf("the answer could not be read: %w", err)}
	}
	return nil
}

// bare is err without the *url.Error around it, which names the URL.
func bare(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		if u.Err != nil {
			return u.Err
		}
		return errors.New("the request failed")
	}
	return err
}

// refusal is an answer that is not a 2xx: Toss's error object, `{"code",
// "message"}` (and v2's `{"error": {...}}`), read for its kind
// (https://docs.tosspayments.com/reference/using-api/req-res#에러-객체).
func refusal(op string, status int, b []byte) *pg.Error {
	var v struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &v)
	code, message := v.Code, v.Message
	if code == "" && v.Error != nil {
		code, message = v.Error.Code, v.Error.Message
	}
	return &pg.Error{Op: op, Kind: kind(status, code), Code: code, Message: message, Status: status}
}

// payment is Toss's Payment object, of the fields read
// (https://docs.tosspayments.com/reference#payment-객체). Fields Toss adds
// are left alone, as it asks (https://docs.tosspayments.com/reference/using-api/req-res#유의사항).
type payment struct {
	PaymentKey    string      `json:"paymentKey"`
	OrderID       string      `json:"orderId"`
	Status        string      `json:"status"`
	Type          string      `json:"type"`
	Currency      string      `json:"currency"`
	TotalAmount   json.Number `json:"totalAmount"`
	BalanceAmount json.Number `json:"balanceAmount"`
	ApprovedAt    string      `json:"approvedAt"`
	Cancels       []cancel    `json:"cancels"`
	Failure       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"failure"`
}

type cancel struct {
	CancelAmount   json.Number `json:"cancelAmount"`
	CancelReason   string      `json:"cancelReason"`
	CanceledAt     string      `json:"canceledAt"`
	TransactionKey string      `json:"transactionKey"`
	CancelStatus   string      `json:"cancelStatus"`
}

// statuses is what each of Toss's payment statuses is
// (https://docs.tosspayments.com/reference#payment-객체, status).
var statuses = map[string]pg.Status{
	"READY":               pg.Pending,
	"IN_PROGRESS":         pg.Pending,
	"WAITING_FOR_DEPOSIT": pg.Pending,
	"DONE":                pg.Paid,
	"PARTIAL_CANCELED":    pg.PartlyCanceled,
	"CANCELED":            pg.Canceled,
	"ABORTED":             pg.Failed,
	"EXPIRED":             pg.Failed,
}

// read is p as levyan has a payment.
func (p payment) read() (pg.Payment, error) {
	s, ok := statuses[p.Status]
	if !ok {
		return pg.Payment{}, fmt.Errorf("a payment of status %q", p.Status)
	}
	currency := p.Currency
	if currency == "" {
		currency = "KRW"
	}
	total, err := minor(p.TotalAmount, currency)
	if err != nil {
		return pg.Payment{}, fmt.Errorf("totalAmount: %w", err)
	}
	balance, err := minor(p.BalanceAmount, currency)
	if err != nil {
		return pg.Payment{}, fmt.Errorf("balanceAmount: %w", err)
	}
	at, err := when(p.ApprovedAt)
	if err != nil {
		return pg.Payment{}, fmt.Errorf("approvedAt: %w", err)
	}

	v := pg.Payment{
		ID:      p.PaymentKey,
		Order:   p.OrderID,
		Status:  s,
		Amount:  money.Amount{Minor: total, Currency: currency},
		Balance: money.Amount{Minor: balance, Currency: currency},
		At:      at,
	}
	for _, c := range p.Cancels {
		// DONE is a cancellation that happened; the others are an
		// asynchronous payment's, which billing never is, and a cancellation
		// in progress or failed has given nothing back.
		if c.CancelStatus != "" && c.CancelStatus != "DONE" {
			continue
		}
		n, err := minor(c.CancelAmount, currency)
		if err != nil {
			return pg.Payment{}, fmt.Errorf("cancelAmount: %w", err)
		}
		at, err := when(c.CanceledAt)
		if err != nil {
			return pg.Payment{}, fmt.Errorf("canceledAt: %w", err)
		}
		v.Cancels = append(v.Cancels, pg.Cancellation{
			ID:     c.TransactionKey,
			Amount: money.Amount{Minor: n, Currency: currency},
			Reason: c.CancelReason,
			At:     at,
		})
	}
	// Oldest first, which Toss does not promise.
	slices.SortStableFunc(v.Cancels, func(a, b pg.Cancellation) int { return a.At.Compare(b.At) })

	if s == pg.Failed {
		// A failure is the error the payment met, read as that error was; one
		// of a code levyan does not read -- COMMON_ERROR, a 5xx's -- is the
		// PG's own, and says nothing of the card.
		v.Failure = &pg.Failure{Kind: pg.Unknown}
		if p.Failure != nil {
			v.Failure.Code, v.Failure.Message = p.Failure.Code, p.Failure.Message
			if k, ok := codes[p.Failure.Code]; ok {
				v.Failure.Kind = k
			}
		}
	}
	return v, nil
}

// billing is Toss's Billing object, of the fields read
// (https://docs.tosspayments.com/reference#billing-객체).
type billing struct {
	CustomerKey     string `json:"customerKey"`
	AuthenticatedAt string `json:"authenticatedAt"`
	BillingKey      string `json:"billingKey"`
	Card            struct {
		IssuerCode string `json:"issuerCode"`
		Number     string `json:"number"`
		CardType   string `json:"cardType"`
		OwnerType  string `json:"ownerType"`
	} `json:"card"`
}

var cardKinds = map[string]pg.CardKind{"신용": pg.Credit, "체크": pg.Debit, "기프트": pg.Gift}
var cardOwners = map[string]pg.CardOwner{"개인": pg.Personal, "법인": pg.Corporate}

func (b billing) read() (pg.Method, error) {
	at, err := when(b.AuthenticatedAt)
	if err != nil {
		return pg.Method{}, fmt.Errorf("authenticatedAt: %w", err)
	}
	return pg.Method{
		Customer: b.CustomerKey,
		Key:      pg.NewBillingKey(b.BillingKey),
		Card: pg.Card{
			Issuer: b.Card.IssuerCode,
			Number: b.Card.Number,
			Kind:   cardKinds[b.Card.CardType],
			Owner:  cardOwners[b.Card.OwnerType],
		},
		At: at,
	}, nil
}

// minor is an amount Toss writes as a JSON number, as a whole number of the
// currency's minor unit: "4900" and "4900.0" are 4900 won, and "4900.5" of
// a currency of no decimal digits is refused, as is an amount of another
// currency than levyan reads -- won is all a Korean PG is paid in.
func minor(n json.Number, currency string) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if d := money.Digits(currency); d != 0 {
		return 0, fmt.Errorf("an amount in %s, which has %d decimal digits", currency, d)
	}
	if v, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("%q is no whole amount", n)
	}
	return int64(f), nil
}

// kst is the zone Toss means when it writes a time without one
// (https://docs.tosspayments.com/reference/using-api/webhook-events, createdAt).
var kst = time.FixedZone("KST", 9*60*60)

// when is a time as Toss writes one: ISO 8601 with its offset, or a
// webhook's without one, which is in KST. Empty is the zero time.
func when(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, kst)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is no time Toss writes", s)
	}
	return t.UTC(), nil
}

// segment is s as one segment of a URL's path.
func segment(s string) string { return url.PathEscape(s) }

// chars is how many characters s is, as Toss counts a length.
func chars(s string) int { return len([]rune(s)) }

// orderOK is whether s is an orderId Toss takes: 6 to 64 of letters, digits,
// '-', '_' and '=' (https://docs.tosspayments.com/reference#자동결제-승인).
func orderOK(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_' || r == '=')
	})
}

// customerOK is whether s is a customerKey Toss takes: 22 to 50 of letters,
// digits, '-', '_', '=', '.' and '@', at least one of them not a letter or a
// digit (https://docs.tosspayments.com/reference#billing-객체, customerKey).
func customerOK(s string) bool {
	if len(s) < 22 || len(s) > 50 {
		return false
	}
	special := false
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '-' || r == '_' || r == '=' || r == '.' || r == '@':
			special = true
		default:
			return false
		}
	}
	return special
}
