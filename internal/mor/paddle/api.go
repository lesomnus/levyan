package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
)

// maxAnswer is the most of an answer read: a page of thirty transactions with
// their adjustments is some hundreds of kilobytes.
const maxAnswer = 8 << 20

// meta is what an answer says of itself
// (https://developer.paddle.com/api-reference/about/success-responses).
type meta struct {
	RequestID  string `json:"request_id"`
	Pagination *struct {
		PerPage int    `json:"per_page"`
		Next    string `json:"next"`
		HasMore bool   `json:"has_more"`
	} `json:"pagination"`
}

// call sends a request to Paddle and reads its answer: a 2xx's data into out,
// and anything else as the [mor.Error] it is.
//
// Nothing it returns carries the API key: it is in a header, never in a URL,
// and an answer that repeats it has it taken out.
func (c *Channel) call(ctx context.Context, op, method, path string, query url.Values, in, out any) (meta, error) {
	key, err := c.c.APIKey.Value()
	if err != nil || key == "" {
		return meta{}, &mor.Error{Op: op, Kind: mor.Auth, Err: errors.New("the API key could not be read")}
	}

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return meta{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: err}
		}
		body = bytes.NewReader(b)
	}
	u := c.url + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return meta{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New("the request could not be made")}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Paddle-Version", Version)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.client.Do(req)
	if err != nil {
		return meta{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: bare(err)}
	}
	defer res.Body.Close()

	b, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return meta{}, &mor.Error{Op: op, Kind: mor.Unknown, Status: res.StatusCode, Err: fmt.Errorf("the answer could not be read: %w", bare(err))}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return meta{}, refusal(op, res.StatusCode, b, key)
	}
	var v struct {
		Data json.RawMessage `json:"data"`
		Meta meta            `json:"meta"`
	}
	if err := json.Unmarshal(b, &v); err != nil || (out != nil && len(v.Data) == 0) {
		// Answered, and for all that is known done: but what was done cannot
		// be read, so it is as good as no answer.
		return meta{}, &mor.Error{Op: op, Kind: mor.Unknown, Status: res.StatusCode, Err: errors.New("the answer could not be read")}
	}
	if out != nil {
		if err := json.Unmarshal(v.Data, out); err != nil {
			return meta{}, &mor.Error{Op: op, Kind: mor.Unknown, Status: res.StatusCode, Request: v.Meta.RequestID, Err: fmt.Errorf("the answer could not be read: %w", err)}
		}
	}
	return v.Meta, nil
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

// unread is an answer that came, and could not be read for what it says.
func unread(op string, err error) *mor.Error {
	return &mor.Error{Op: op, Kind: mor.Unknown, Err: fmt.Errorf("the answer could not be read: %w", err)}
}

// refusal is an answer that is not a 2xx: Paddle's error object, read for its
// kind (https://developer.paddle.com/api-reference/about/errors). An answer
// that names the API key -- none is known to -- has it taken out.
func refusal(op string, status int, b []byte, key string) *mor.Error {
	var v struct {
		Error *struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
			Errors []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
		Meta meta `json:"meta"`
	}
	_ = json.Unmarshal(b, &v)
	e := &mor.Error{Op: op, Status: status, Request: v.Meta.RequestID}
	if v.Error != nil {
		e.Code = v.Error.Code
		var m strings.Builder
		m.WriteString(v.Error.Detail)
		for _, f := range v.Error.Errors {
			fmt.Fprintf(&m, "; %s: %s", f.Field, f.Message)
		}
		e.Message = m.String()
		if key != "" {
			e.Message = strings.ReplaceAll(e.Message, key, "[api key]")
		}
	}
	e.Kind = kind(status, e.Code)
	e.Err = sentinels[e.Code]
	return e
}

// custom is Paddle's custom_data, of the fields read: levyan writes strings.
type custom map[string]json.RawMessage

func (c custom) get(field string) string {
	var s string
	if err := json.Unmarshal(c[field], &s); err != nil {
		return ""
	}
	return s
}

// subscription is Paddle's subscription entity, of the fields read
// (https://developer.paddle.com/api-reference/subscriptions/get-subscription).
// Fields Paddle adds are left alone.
type subscription struct {
	ID                   string  `json:"id"`
	Status               string  `json:"status"`
	CustomerID           string  `json:"customer_id"`
	CurrencyCode         string  `json:"currency_code"`
	NextBilledAt         *string `json:"next_billed_at"`
	CanceledAt           *string `json:"canceled_at"`
	UpdatedAt            string  `json:"updated_at"`
	CurrentBillingPeriod *period `json:"current_billing_period"`
	ScheduledChange      *struct {
		Action      string  `json:"action"`
		EffectiveAt string  `json:"effective_at"`
		ResumeAt    *string `json:"resume_at"`
	} `json:"scheduled_change"`
	CustomData      custom   `json:"custom_data"`
	NextTransaction *preview `json:"next_transaction"`
}

type period struct {
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
}

var subscriptionStatuses = map[string]mor.SubscriptionStatus{
	"active":   mor.Active,
	"trialing": mor.Trialing,
	"past_due": mor.PastDue,
	"paused":   mor.Paused,
	"canceled": mor.Canceled,
}

var changes = map[string]mor.ChangeKind{
	"cancel": mor.CancelAtEnd,
	"pause":  mor.PauseAtEnd,
	"resume": mor.Resume,
}

// read is s as levyan keeps a mirror of one.
func (s subscription) read() (mor.Subscription, error) {
	st, ok := subscriptionStatuses[s.Status]
	if !ok {
		return mor.Subscription{}, fmt.Errorf("a subscription of status %q", s.Status)
	}
	if !idOK("sub", s.ID) {
		return mor.Subscription{}, errors.New("a subscription with no id")
	}
	v := mor.Subscription{
		ID:       s.ID,
		Customer: s.CustomerID,
		Account:  s.CustomData.get(accountField),
		Status:   st,
		Currency: s.CurrencyCode,
	}
	var err error
	if v.Updated, err = when(s.UpdatedAt); err != nil {
		return mor.Subscription{}, fmt.Errorf("updated_at: %w", err)
	}
	if v.Next, err = whenOf(s.NextBilledAt); err != nil {
		return mor.Subscription{}, fmt.Errorf("next_billed_at: %w", err)
	}
	if v.Canceled, err = whenOf(s.CanceledAt); err != nil {
		return mor.Subscription{}, fmt.Errorf("canceled_at: %w", err)
	}
	if p := s.CurrentBillingPeriod; p != nil {
		if v.Period, err = p.read(); err != nil {
			return mor.Subscription{}, fmt.Errorf("current_billing_period: %w", err)
		}
	}
	if c := s.ScheduledChange; c != nil {
		k, ok := changes[c.Action]
		if !ok {
			// A change levyan cannot place may end the subscription: it is
			// read as one that does, and the subscription as one that will
			// not renew.
			k = mor.CancelAtEnd
		}
		at, err := when(c.EffectiveAt)
		if err != nil {
			return mor.Subscription{}, fmt.Errorf("scheduled_change: %w", err)
		}
		v.Change = &mor.Change{Kind: k, At: at}
	}
	return v, nil
}

func (p period) read() (mor.Period, error) {
	s, err := when(p.StartsAt)
	if err != nil {
		return mor.Period{}, err
	}
	e, err := when(p.EndsAt)
	if err != nil {
		return mor.Period{}, err
	}
	return mor.Period{Start: s, End: e}, nil
}

// totals is a transaction's or a line's totals, of the fields read.
type totals struct {
	Subtotal   string `json:"subtotal"`
	Tax        string `json:"tax"`
	Total      string `json:"total"`
	GrandTotal string `json:"grand_total"`
}

// lineItem is one of a transaction's details.line_items, which say what
// Paddle billed for each item -- and, with its product's custom_data, which
// of levyan's charges it is.
type lineItem struct {
	ID      string  `json:"id"`
	PriceID *string `json:"price_id"`
	Totals  totals  `json:"totals"`
	Product struct {
		Name       string `json:"name"`
		CustomData custom `json:"custom_data"`
	} `json:"product"`
}

// transaction is Paddle's transaction entity, of the fields read
// (https://developer.paddle.com/api-reference/transactions/get-transaction).
type transaction struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	CustomerID     *string `json:"customer_id"`
	SubscriptionID *string `json:"subscription_id"`
	CurrencyCode   string  `json:"currency_code"`
	CustomData     custom  `json:"custom_data"`
	CreatedAt      string  `json:"created_at"`
	BilledAt       *string `json:"billed_at"`
	Items          []struct {
		Price struct {
			ID         string `json:"id"`
			CustomData custom `json:"custom_data"`
		} `json:"price"`
	} `json:"items"`
	Details struct {
		Totals    totals     `json:"totals"`
		LineItems []lineItem `json:"line_items"`
	} `json:"details"`
	Payments []payment `json:"payments"`
	Checkout *struct {
		URL *string `json:"url"`
	} `json:"checkout"`
	Adjustments []adjustment `json:"adjustments"`
}

func (t transaction) custom(field string) string { return t.CustomData.get(field) }

// payment is one of a transaction's attempts to collect it
// (https://developer.paddle.com/api-reference/transactions/get-transaction,
// payments), of the fields read: of its method, the kind, and of a card no
// more than its brand -- or a Korean card's company -- and its last four
// digits.
type payment struct {
	Status        string  `json:"status"`
	ErrorCode     *string `json:"error_code"`
	CapturedAt    *string `json:"captured_at"`
	CreatedAt     string  `json:"created_at"`
	MethodDetails *struct {
		Type string `json:"type"`
		Card *struct {
			Type  string `json:"type"`
			Last4 string `json:"last4"`
		} `json:"card"`
		SouthKoreaLocalCard *struct {
			Type  string `json:"type"`
			Last4 string `json:"last4"`
		} `json:"south_korea_local_card"`
	} `json:"method_details"`
}

// latest is the newest of t's attempts to collect it, which Paddle lists
// first: nil for none.
func (t transaction) latest() *payment {
	if len(t.Payments) == 0 {
		return nil
	}
	return &t.Payments[0]
}

// method is what p was attempted with, and nil when Paddle says nothing of
// it.
func (p payment) method() *mor.Method {
	d := p.MethodDetails
	if d == nil || d.Type == "" {
		return nil
	}
	m := &mor.Method{Type: d.Type}
	switch {
	case d.Card != nil:
		m.Brand, m.Last4 = d.Card.Type, d.Card.Last4
	case d.SouthKoreaLocalCard != nil:
		m.Brand, m.Last4 = d.SouthKoreaLocalCard.Type, d.SouthKoreaLocalCard.Last4
	}
	return m
}

// statuses is what each of Paddle's transaction statuses is
// (https://developer.paddle.com/api-reference/transactions/get-transaction,
// status). past_due is a transaction Paddle is still collecting, in its
// dunning; canceled one it never will.
var statuses = map[string]mor.Status{
	"draft":     mor.Pending,
	"ready":     mor.Pending,
	"billed":    mor.Pending,
	"past_due":  mor.Pending,
	"paid":      mor.Paid,
	"completed": mor.Paid,
	"canceled":  mor.Failed,
}

// key is the key of levyan's charge a line is: its product's, or its price's.
func (t transaction) key(l lineItem) string {
	if k := l.Product.CustomData.get(keyField); k != "" {
		return k
	}
	for _, it := range t.Items {
		if l.PriceID != nil && it.Price.ID == *l.PriceID {
			return it.Price.CustomData.get(keyField)
		}
	}
	return ""
}

// has is whether t has a line of key.
func (t transaction) has(key string) bool {
	for _, l := range t.Details.LineItems {
		if t.key(l) == key {
			return true
		}
	}
	return false
}

// read is t as levyan has a transaction, its lines' amounts in the terms of a
// charge under taxMode.
func (t transaction) read(taxMode string) (mor.Transaction, error) {
	s, ok := statuses[t.Status]
	if !ok {
		return mor.Transaction{}, fmt.Errorf("a transaction of status %q", t.Status)
	}
	cur := t.CurrencyCode
	v := mor.Transaction{ID: t.ID, Status: s}
	if t.SubscriptionID != nil {
		v.Subscription = *t.SubscriptionID
	}
	if t.CustomerID != nil {
		v.Customer = *t.CustomerID
	}
	var err error
	if v.Total, err = amount(t.Details.Totals.Total, cur); err != nil {
		return mor.Transaction{}, fmt.Errorf("total: %w", err)
	}
	for _, l := range t.Details.LineItems {
		line, err := readLine(l, t.key(l), cur, taxMode)
		if err != nil {
			return mor.Transaction{}, err
		}
		v.Lines = append(v.Lines, line)
	}

	// Payments come newest first: the latest attempt is what is said of it.
	var latest *string
	for _, p := range t.Payments {
		if p.Status == "captured" && v.At.IsZero() && p.CapturedAt != nil {
			if v.At, err = when(*p.CapturedAt); err != nil {
				return mor.Transaction{}, fmt.Errorf("captured_at: %w", err)
			}
		}
		if latest == nil && p.ErrorCode != nil && *p.ErrorCode != "" {
			latest = p.ErrorCode
		}
	}
	if s == mor.Paid && v.At.IsZero() {
		if v.At, err = whenOf(t.BilledAt); err != nil {
			return mor.Transaction{}, fmt.Errorf("billed_at: %w", err)
		}
	}
	switch {
	case s == mor.Failed:
		v.Failure = &mor.Failure{Kind: mor.Unknown, Code: "canceled", Message: "the transaction was canceled"}
		if latest != nil {
			v.Failure = &mor.Failure{Kind: paymentKind(*latest), Code: *latest}
		}
	case t.Status == "past_due" && latest != nil:
		v.Failure = &mor.Failure{Kind: paymentKind(*latest), Code: *latest}
	}
	if p := t.latest(); p != nil {
		v.Method = p.method()
		// A payment the payer has yet to approve -- in their card company's
		// app, by their bank's check -- is one Paddle cannot collect
		// without them.
		v.Awaiting = s == mor.Pending && p.Status == "action_required"
	}

	for _, a := range t.Adjustments {
		av, err := a.read()
		if err != nil {
			return mor.Transaction{}, err
		}
		v.Adjustments = append(v.Adjustments, av)
	}
	// Oldest first, which Paddle does not promise.
	slices.SortStableFunc(v.Adjustments, func(a, b mor.Adjustment) int { return a.At.Compare(b.At) })
	return v, nil
}

// readLine is l as levyan has a line: Amount is what was charged -- before
// tax under the external tax mode, with it under the internal one.
func readLine(l lineItem, key, cur, taxMode string) (mor.Line, error) {
	sub, err := amount(l.Totals.Subtotal, cur)
	if err != nil {
		return mor.Line{}, fmt.Errorf("line subtotal: %w", err)
	}
	tax, err := amount(l.Totals.Tax, cur)
	if err != nil {
		return mor.Line{}, fmt.Errorf("line tax: %w", err)
	}
	total, err := amount(l.Totals.Total, cur)
	if err != nil {
		return mor.Line{}, fmt.Errorf("line total: %w", err)
	}
	v := mor.Line{ID: l.ID, Key: key, Name: l.Product.Name, Amount: sub, Tax: tax, Total: total}
	if taxMode == "internal" {
		v.Amount = total
	}
	return v, nil
}

// preview is a subscription's next_transaction: what Paddle will bill it
// with next, one-time charges for the next period among it. It is no
// transaction yet, and its lines have no ids.
type preview struct {
	BillingPeriod *period `json:"billing_period"`
	Details       struct {
		Totals    totals     `json:"totals"`
		LineItems []lineItem `json:"line_items"`
	} `json:"details"`
}

func (p *preview) has(key string) bool {
	for _, l := range p.Details.LineItems {
		if l.Product.CustomData.get(keyField) == key {
			return true
		}
	}
	return false
}

// read is p as a transaction of s still to be billed: [mor.Scheduled].
func (p *preview) read(s mor.Subscription, taxMode string) (mor.Transaction, error) {
	v := mor.Transaction{Subscription: s.ID, Customer: s.Customer, Status: mor.Scheduled}
	var err error
	if v.Total, err = amount(p.Details.Totals.Total, s.Currency); err != nil {
		return mor.Transaction{}, fmt.Errorf("next transaction total: %w", err)
	}
	for _, l := range p.Details.LineItems {
		line, err := readLine(l, l.Product.CustomData.get(keyField), s.Currency, taxMode)
		if err != nil {
			return mor.Transaction{}, err
		}
		line.ID = ""
		v.Lines = append(v.Lines, line)
	}
	return v, nil
}

// adjustment is Paddle's adjustment entity, of the fields read
// (https://developer.paddle.com/api-reference/adjustments/create-adjustment).
type adjustment struct {
	ID            string `json:"id"`
	Action        string `json:"action"`
	TransactionID string `json:"transaction_id"`
	Reason        string `json:"reason"`
	Status        string `json:"status"`
	CurrencyCode  string `json:"currency_code"`
	CreatedAt     string `json:"created_at"`
	Totals        totals `json:"totals"`
}

var adjustmentKinds = map[string]mor.AdjustmentKind{
	"refund":                     mor.RefundAdjustment,
	"chargeback":                 mor.Chargeback,
	"chargeback_warning":         mor.Chargeback,
	"chargeback_reverse":         mor.ChargebackReversed,
	"chargeback_warning_reverse": mor.ChargebackReversed,
	"credit":                     mor.OtherAdjustment,
	"credit_reverse":             mor.OtherAdjustment,
}

var adjustmentStatuses = map[string]mor.AdjustmentStatus{
	"pending_approval": mor.Requested,
	"approved":         mor.Approved,
	"rejected":         mor.Rejected,
	"reversed":         mor.Reversed,
}

func (a adjustment) read() (mor.Adjustment, error) {
	st, ok := adjustmentStatuses[a.Status]
	if !ok {
		return mor.Adjustment{}, fmt.Errorf("an adjustment of status %q", a.Status)
	}
	n, err := amount(a.Totals.Total, a.CurrencyCode)
	if err != nil {
		return mor.Adjustment{}, fmt.Errorf("adjustment total: %w", err)
	}
	at, err := when(a.CreatedAt)
	if err != nil {
		return mor.Adjustment{}, fmt.Errorf("adjustment created_at: %w", err)
	}
	v := mor.Adjustment{
		ID:          a.ID,
		Transaction: a.TransactionID,
		Kind:        adjustmentKinds[a.Action],
		Status:      st,
		Amount:      n,
		At:          at,
	}
	v.Reason, v.Key = unmark(a.Reason)
	if v.Kind != mor.RefundAdjustment {
		// Only a refund is levyan's to ask for: a chargeback that happens
		// to be worded like one of levyan's reasons is still none of them.
		v.Reason, v.Key = a.Reason, ""
	}
	return v, nil
}

// amount is an amount Paddle writes, a string of a whole number of the
// currency's lowest denomination ("1000" is 10.00 USD), with its currency.
// Empty is nothing.
func amount(s, currency string) (money.Amount, error) {
	if s == "" {
		return money.Zero(currency), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%q is no whole amount", s)
	}
	return money.Amount{Minor: n, Currency: currency}, nil
}

// when is a time as Paddle writes one, RFC 3339 -- with microseconds, in UTC.
// Empty is the zero time.
func when(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is no time Paddle writes", s)
	}
	return t.UTC(), nil
}

func whenOf(s *string) (time.Time, error) {
	if s == nil {
		return time.Time{}, nil
	}
	return when(*s)
}
