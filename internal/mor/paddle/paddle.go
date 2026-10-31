// Package paddle is a [mor.Channel] for Paddle Billing: Paddle as the
// merchant of record, a subscription started in Paddle's checkout, and each
// closed period's amount handed to Paddle as a one-time charge of it.
//
// **It is a candidate.** Which MoR levyan sells through is the owner's
// decision and has not been made (levyan#12); Paddle is the candidate this
// adapter was built for, and Polar the alternative levyan#1 names. The adapter
// is here so that it can be tried against Paddle's sandbox and so that what
// the rest of levyan asks of an MoR is written down -- not because the choice
// is made. Nothing outside the wiring names this package.
//
// # What it relies on
//
// As Paddle's documentation has it on 2026-10-31:
//
//   - The API is https://api.paddle.com, and the sandbox's
//     https://sandbox-api.paddle.com; each takes only its own keys, and a key
//     of the other answers forbidden. Every call is Bearer authentication
//     with an API key, pdl_live_apikey_… or pdl_sdbx_apikey_…, which Paddle
//     shows once (https://developer.paddle.com/api-reference/about,
//     https://developer.paddle.com/api-reference/about/authentication,
//     https://developer.paddle.com/sdks/sandbox). The version is asked for in
//     Paddle-Version, 1 (https://developer.paddle.com/api-reference/about/versioning).
//   - A refusal is an HTTP status and {error: {type, code, detail,
//     documentation_url, errors}, meta: {request_id}}; a 5xx is Paddle's own
//     trouble, to be tried again (https://developer.paddle.com/api-reference/about/errors).
//     Here a 5xx is an unknown outcome whatever its code says. Codes are
//     listed at https://developer.paddle.com/errors/overview.
//   - 240 requests a minute from an address, then 429 too_many_requests and
//     a minute of nothing; a subscription takes 20 charges made now an hour
//     and 100 a day (https://developer.paddle.com/api-reference/about/rate-limiting).
//   - POST /subscriptions/{subscription_id}/charge adds one-time charges, of
//     a price with no billing cycle, to a subscription: effective_from
//     immediately, in a transaction made and collected while Paddle answers,
//     or next_billing_period, added to the transaction of the next renewal.
//     An item may be a price and a product made for it alone ("non-catalog"),
//     each with custom_data. It answers the subscription, which holds no
//     one-time charge: a charge made now is found by listing the
//     subscription's transactions, and one for the next period in the
//     subscription's next_transaction. A subscription past_due takes no
//     charge, nor one renewing within 30 minutes. on_payment_failure,
//     prevent_change by default, answers a declined payment with an error
//     and makes no change
//     (https://developer.paddle.com/api-reference/subscriptions/create-subscription-charge,
//     https://developer.paddle.com/build/subscriptions/bill-add-one-time-charge,
//     https://developer.paddle.com/changelog/2023/payment-failure-behavior-update-subscription).
//   - **Paddle keeps no idempotency key.** Nothing in the API takes one and no
//     error speaks of one (https://developer.paddle.com/errors/overview). So
//     the adapter writes levyan's key into the custom_data of the product it
//     makes for each charge -- a line's product, and its custom_data, are in
//     a transaction's details.line_items and in a next_transaction alike
//     (https://developer.paddle.com/api-reference/subscriptions/get-subscription)
//     -- and looks for the key in both before it sends a charge (package mor,
//     "Once for its key"). A refund is an adjustment, which has no
//     custom_data, so the key goes at the end of its reason, which Paddle
//     shows in its dashboard.
//   - A subscription scheduled to cancel has next_billed_at null and no
//     next_transaction (as one scheduled to pause), and "If you've made
//     changes to a subscription or billed for one-time charges and set them
//     to be charged on the next billing period, these are automatically
//     forgiven" when it cancels; a canceled subscription is never reinstated
//     (https://developer.paddle.com/build/subscriptions/cancel-subscriptions,
//     https://developer.paddle.com/api-reference/subscriptions/preview-subscription-charge).
//     So a charge for the next period of a subscription that will not renew
//     is refused here before it is sent ([mor.ErrNoNextPeriod]).
//   - POST /subscriptions/{subscription_id}/cancel cancels at the end of the
//     period, or immediately; a paused subscription only immediately
//     (https://developer.paddle.com/api-reference/subscriptions/cancel-subscription).
//   - A plan of nothing is Paddle's own way to take a card for usage billed
//     as one-time charges -- "signing customers up for a zero dollar base
//     plan to capture their payment details" -- and Paddle cancels such a
//     subscription immediately when the payer follows the link in its email
//     (https://developer.paddle.com/changelog/2023/zero-dollar-subscription-emails).
//   - A checkout is a transaction made with POST /transactions, whose
//     custom_data Paddle copies to the subscription it makes; the page opens
//     it with Paddle.js (https://cdn.paddle.com/paddle/v2/paddle.js,
//     Paddle.Environment.set("sandbox") first in the sandbox,
//     Paddle.Initialize({token}), Paddle.Checkout.open({transactionId,
//     settings: {successUrl}})), or sends the payer to its checkout.url, the
//     default payment link with ?_ptxn= and its id. Paddle makes the
//     subscription when the transaction is completed
//     (https://developer.paddle.com/api-reference/transactions/create-transaction,
//     https://developer.paddle.com/build/transactions/custom-data,
//     https://developer.paddle.com/build/transactions/pass-transaction-checkout,
//     https://developer.paddle.com/paddle-js/about/include-paddlejs,
//     https://developer.paddle.com/paddle-js/methods/paddle-environment-set,
//     https://developer.paddle.com/paddle-js/methods/paddle-checkout-open).
//   - GET /transactions/{transaction_id}?include=adjustments, and GET
//     /transactions?subscription_id=… thirty at a time, after a cursor
//     (https://developer.paddle.com/api-reference/transactions/list-transactions,
//     https://developer.paddle.com/api-reference/about/pagination).
//   - A refund is POST /adjustments with action refund, of a completed
//     transaction, all of it or items of it; a partial amount is with tax
//     unless tax_mode says external. Most live refunds wait as
//     pending_approval for Paddle's review, the sandbox approves them every
//     ten minutes, and a transaction takes no adjustment while one is pending
//     (https://developer.paddle.com/api-reference/adjustments/create-adjustment,
//     https://developer.paddle.com/build/transactions/create-transaction-adjustments).
//   - Paddle charges no less than its minimum for a currency: 0.70 USD, 0.65
//     EUR, 100 JPY, 980 KRW, and answers less with
//     subscription_update_transaction_balance_less_than_charge_limit
//     (https://developer.paddle.com/concepts/sell/supported-currencies,
//     https://developer.paddle.com/errors/subscriptions/subscription_update_transaction_balance_less_than_charge_limit).
//   - A webhook is a POST of {event_id, event_type, occurred_at,
//     notification_id, data}, delivered at least once, out of order, and
//     answered 200 within five seconds or sent again -- three times in two
//     minutes in the sandbox, up to sixty times over two days live
//     (https://developer.paddle.com/webhooks/about/how-webhooks-work,
//     https://developer.paddle.com/webhooks/about/respond-to-webhooks).
//   - Every webhook carries Paddle-Signature: "ts=" a Unix time ";h1=" the hex
//     HMAC-SHA256 of ts + ":" + the raw body under the notification
//     destination's endpoint secret key, and more than one h1 while a secret
//     is rotated. Paddle's SDKs refuse one signed more than five seconds from
//     now, against a replay (https://developer.paddle.com/webhooks/about/signature-verification).
//   - The sandbox moves no money and takes test cards only: 4242 4242 4242
//     4242, 4000 0000 0000 0002 declined, 4000 0027 6000 3184 paid once and
//     declined after (https://developer.paddle.com/sdks/sandbox).
package paddle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
)

// The APIs a [Config] with no URL calls.
const (
	API        = "https://api.paddle.com"
	SandboxAPI = "https://sandbox-api.paddle.com"
)

// SDK is the script a page opens Paddle's checkout with.
const SDK = "https://cdn.paddle.com/paddle/v2/paddle.js"

// Version is the Paddle API version every request asks for.
const Version = "1"

// Timeout is how long a request waits by default. Paddle collects a charge
// made now while it answers, and says it "may take longer than usual", not
// how long; this is longer than a card network takes to answer.
const Timeout = 70 * time.Second

// Tolerance is how far from now a webhook's signature time may be by default:
// what Paddle's SDKs allow.
const Tolerance = 5 * time.Second

// The custom_data fields the adapter writes and reads.
const (
	// keyField is a charge's key, on the product and the price made for it.
	keyField = "levyan_key"

	// accountField is the billing account a checkout is for, on its
	// transaction and so on the subscription Paddle makes of it.
	accountField = "levyan_account"
)

// Config is how the adapter reaches Paddle.
type Config struct {
	// APIKey is a Paddle Billing API key, pdl_sdbx_apikey_… for the sandbox
	// and pdl_live_apikey_… live, with the permissions to read and write
	// transactions, subscriptions and adjustments. It is read every time it
	// is used, so a key rotated into its file is the next request's.
	APIKey cfg.Secret `yaml:"api_key"`

	// ClientToken is the client-side token Paddle.js is initialized with,
	// test_… or live_…: public, the checkout's. It is needed only to start a
	// checkout.
	ClientToken string `yaml:"client_token"`

	// WebhookSecret is the endpoint secret key of the notification
	// destination levyan's webhooks come to (pdl_ntfset_…), which signs
	// them. Without it every webhook is refused.
	WebhookSecret cfg.Secret `yaml:"webhook_secret"`

	// Sandbox is Paddle's sandbox, where test cards move no money: its API,
	// its keys, and its checkout.
	Sandbox bool `yaml:"sandbox"`

	// Plan is the catalog price a checkout subscribes the payer to (pri_…):
	// a recurring price, of nothing when every amount is levyan's one-time
	// charges. It is needed only to start a checkout.
	Plan string `yaml:"plan"`

	// TaxMode is whether Paddle's tax is added to an amount levyan charges,
	// "external" (the default), or is in it, "internal": the tax_mode of the
	// price made for each charge, and of each refund of part of one. It is
	// one for the life of a deployment, since it is what an amount means.
	TaxMode string `yaml:"tax_mode"`

	// TaxCategory is the tax category of the product made for each charge,
	// which Paddle wants approved on the account: "standard" when empty.
	TaxCategory string `yaml:"tax_category"`

	// Tolerance is how far from now a webhook's signature time may be:
	// [Tolerance] when zero.
	Tolerance time.Duration `yaml:"tolerance"`

	// URL is the API it calls: [API], or [SandboxAPI] when Sandbox, when
	// empty; a fake's in a test.
	URL string `yaml:"url"`

	// Client is what sends the requests: one with [Timeout] when nil.
	Client *http.Client `yaml:"-"`

	// Clock is what the time is: time.Now when nil. A test's fake moves it.
	Clock func() time.Time `yaml:"-"`
}

// Channel is Paddle Billing as a [mor.Channel]. It holds nothing that
// changes, and is used by many goroutines at once.
type Channel struct {
	c      Config
	url    string
	client *http.Client
	now    func() time.Time
}

var _ mor.Channel = (*Channel)(nil)

// taxCategories is what Paddle takes as a product's tax_category
// (https://developer.paddle.com/api-reference/subscriptions/create-subscription-charge).
var taxCategories = map[string]bool{
	"digital-goods": true, "ebooks": true, "implementation-services": true, "professional-services": true,
	"saas": true, "software-programming-services": true, "standard": true, "training-services": true,
	"website-hosting": true,
}

// New is a channel to Paddle as c says.
func New(c Config) (*Channel, error) {
	if c.APIKey.IsZero() {
		return nil, errors.New("paddle: no API key")
	}
	if k, err := c.APIKey.Value(); err == nil {
		// A key is read again on every call, and may not be there yet; one
		// that is can be told wrong now rather than at the first charge.
		switch {
		case strings.HasPrefix(k, "test_") || strings.HasPrefix(k, "live_"):
			return nil, errors.New("paddle: the API key is a client-side token, which is the checkout's and calls nothing")
		case strings.HasPrefix(k, "pdl_") && !strings.Contains(k, "_apikey_"):
			return nil, errors.New("paddle: the API key is no API key, but another of Paddle's secrets")
		case keyEnv(k) == "live" && c.Sandbox:
			return nil, errors.New("paddle: the API key is live, and the channel is the sandbox's")
		case keyEnv(k) == "sdbx" && !c.Sandbox:
			return nil, errors.New("paddle: the API key is the sandbox's, and the channel is live")
		}
	}
	switch t := c.ClientToken; {
	case t == "":
	case strings.HasPrefix(t, "pdl_"):
		// It goes to a page: an API key there is a key published.
		return nil, errors.New("paddle: the client token is an API key, which never goes to a page")
	case strings.HasPrefix(t, "test_") && !c.Sandbox, strings.HasPrefix(t, "live_") && c.Sandbox:
		return nil, errors.New("paddle: the client token and the channel are of different environments")
	}
	if c.Plan != "" && !idOK("pri", c.Plan) {
		return nil, fmt.Errorf("paddle: %q is no price", c.Plan)
	}
	switch c.TaxMode {
	case "":
		c.TaxMode = "external"
	case "external", "internal":
	default:
		return nil, fmt.Errorf("paddle: %q is no tax mode: external or internal", c.TaxMode)
	}
	if c.TaxCategory == "" {
		c.TaxCategory = "standard"
	}
	if !taxCategories[c.TaxCategory] {
		return nil, fmt.Errorf("paddle: %q is no tax category of Paddle's", c.TaxCategory)
	}
	switch {
	case c.Tolerance < 0:
		return nil, errors.New("paddle: a negative tolerance")
	case c.Tolerance == 0:
		c.Tolerance = Tolerance
	}

	u := c.URL
	if u == "" {
		u = API
		if c.Sandbox {
			u = SandboxAPI
		}
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return nil, fmt.Errorf("paddle: %q is no URL of an API", u)
	}

	client := c.Client
	if client == nil {
		client = &http.Client{
			Timeout: Timeout,
			// Paddle's API answers where it is asked. An answer that sends
			// the request elsewhere is no answer to it -- an unknown one, for
			// a charge -- and is not followed with the API key.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	now := c.Clock
	if now == nil {
		now = time.Now
	}
	return &Channel{c: c, url: strings.TrimSuffix(u, "/"), client: client, now: now}, nil
}

// keyEnv is which environment an API key is of, by its prefix
// (https://developer.paddle.com/api-reference/about/authentication): "live",
// "sdbx", or "" for a key of neither -- a legacy key says nothing of it.
func keyEnv(k string) string {
	for _, e := range []string{"live", "sdbx"} {
		if strings.HasPrefix(k, "pdl_"+e+"_") {
			return e
		}
	}
	return ""
}

func (c *Channel) Name() string { return "paddle" }

func invalid(op, why string) *mor.Error {
	return &mor.Error{Op: op, Kind: mor.Invalid, Err: errors.New(why)}
}

// StartCheckout makes the transaction a checkout is: the plan, for the
// billing account, in its custom_data -- which Paddle copies to the
// subscription -- so that the page opens a checkout levyan made, and not one
// it made up. The page opens it with Paddle.js, or sends the payer to its
// URL.
//
// A checkout started again is another transaction, of which the payer
// finishes one or none: starting one charges nothing.
func (c *Channel) StartCheckout(ctx context.Context, s mor.Start) (mor.Checkout, error) {
	const op = "checkout"
	switch {
	case c.c.ClientToken == "":
		return mor.Checkout{}, invalid(op, "no client token, which the checkout is opened with")
	case c.c.Plan == "":
		return mor.Checkout{}, invalid(op, "no plan, which the checkout subscribes the payer to")
	case !keyOK(s.Account):
		return mor.Checkout{}, invalid(op, "the account is 1 to 64 of letters, digits, '-', '_', '.', ':' or '='")
	case s.Customer != "" && !idOK("ctm", s.Customer):
		return mor.Checkout{}, invalid(op, "the customer is no customer of Paddle's")
	case s.Currency != "" && money.Check(s.Currency) != nil:
		return mor.Checkout{}, invalid(op, "a currency levyan does not price in: "+s.Currency)
	}
	if u, err := url.Parse(s.Success); err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
		return mor.Checkout{}, invalid(op, "the checkout sends the payer back to an absolute URL")
	}

	in := map[string]any{
		"items":           []map[string]any{{"price_id": c.c.Plan, "quantity": 1}},
		"collection_mode": "automatic",
		"custom_data":     map[string]string{accountField: s.Account},
	}
	if s.Customer != "" {
		in["customer_id"] = s.Customer
	}
	if s.Currency != "" {
		in["currency_code"] = s.Currency
	}
	var t transaction
	if _, err := c.call(ctx, op, http.MethodPost, "/transactions", nil, in, &t); err != nil {
		return mor.Checkout{}, err
	}
	if !idOK("txn", t.ID) {
		return mor.Checkout{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: errors.New("the answer names no transaction")}
	}

	env := "production"
	if c.c.Sandbox {
		env = "sandbox"
	}
	w := mor.Window{
		Script: SDK,
		Values: map[string]string{
			"token":         c.c.ClientToken,
			"environment":   env,
			"transactionId": t.ID,
			"successUrl":    s.Success,
		},
	}
	if t.Checkout != nil && t.Checkout.URL != nil {
		w.URL = *t.Checkout.URL
	}
	return mor.Checkout{ID: t.ID, Account: s.Account, Window: w}, nil
}

// CompleteCheckout asks the checkout's transaction, and the subscription it
// made once it is paid.
func (c *Channel) CompleteCheckout(ctx context.Context, co mor.Checkout) (mor.Subscription, error) {
	const op = "checkout"
	if !idOK("txn", co.ID) {
		return mor.Subscription{}, invalid(op, "the checkout is no transaction of Paddle's")
	}
	if !keyOK(co.Account) {
		return mor.Subscription{}, invalid(op, "the checkout is for no account")
	}
	var t transaction
	if _, err := c.call(ctx, op, http.MethodGet, "/transactions/"+co.ID, nil, nil, &t); err != nil {
		return mor.Subscription{}, err
	}
	if t.custom(accountField) != co.Account {
		return mor.Subscription{}, invalid(op, "the transaction is no checkout of the account's")
	}
	switch t.Status {
	case "canceled":
		return mor.Subscription{}, &mor.Error{Op: op, Kind: mor.Declined, Err: errors.New("the checkout was canceled: start another")}
	case "paid", "completed":
		if t.SubscriptionID == nil || !idOK("sub", *t.SubscriptionID) {
			return mor.Subscription{}, &mor.Error{Op: op, Kind: mor.NotFound, Err: mor.ErrOpen}
		}
	default:
		return mor.Subscription{}, &mor.Error{Op: op, Kind: mor.NotFound, Err: mor.ErrOpen}
	}

	s, _, err := c.subscription(ctx, op, *t.SubscriptionID, false)
	if err != nil {
		return mor.Subscription{}, err
	}
	switch s.Account {
	case "":
		s.Account = co.Account
	case co.Account:
	default:
		return mor.Subscription{}, invalid(op, "the subscription is another account's")
	}
	return s, nil
}

// Subscription is GET /subscriptions/{subscription_id}.
func (c *Channel) Subscription(ctx context.Context, id string) (mor.Subscription, error) {
	const op = "subscription"
	if !idOK("sub", id) {
		return mor.Subscription{}, invalid(op, "no subscription of Paddle's")
	}
	s, _, err := c.subscription(ctx, op, id, false)
	return s, err
}

// subscription is a subscription, and with next its next transaction: nil
// for one that will not renew.
func (c *Channel) subscription(ctx context.Context, op, id string, next bool) (mor.Subscription, *preview, error) {
	var q url.Values
	if next {
		q = url.Values{"include": {"next_transaction"}}
	}
	var s subscription
	if _, err := c.call(ctx, op, http.MethodGet, "/subscriptions/"+id, q, nil, &s); err != nil {
		return mor.Subscription{}, nil, err
	}
	v, err := s.read()
	if err != nil {
		return mor.Subscription{}, nil, unread(op, err)
	}
	return v, s.NextTransaction, nil
}

// Cancel is POST /subscriptions/{subscription_id}/cancel.
func (c *Channel) Cancel(ctx context.Context, cn mor.Cancel) (mor.Subscription, error) {
	const op = "cancel"
	if !idOK("sub", cn.Subscription) {
		return mor.Subscription{}, invalid(op, "no subscription of Paddle's")
	}
	when := "next_billing_period"
	if cn.Now {
		when = "immediately"
	}
	var s subscription
	_, err := c.call(ctx, op, http.MethodPost, "/subscriptions/"+cn.Subscription+"/cancel", nil, map[string]string{"effective_from": when}, &s)
	var e *mor.Error
	switch {
	case err == nil:
	case errors.As(err, &e) && (errors.Is(err, mor.ErrEnded) || e.Code == "subscription_locked_pending_changes"):
		// Canceled already, or scheduled to be: what was asked, if it is.
		v, _, qerr := c.subscription(ctx, op, cn.Subscription, false)
		switch {
		case qerr != nil:
			return mor.Subscription{}, qerr
		case v.Status == mor.Canceled:
			return v, nil
		case !cn.Now && v.Change != nil && v.Change.Kind == mor.CancelAtEnd:
			return v, nil
		}
		return mor.Subscription{}, err
	default:
		return mor.Subscription{}, err
	}
	v, err := s.read()
	if err != nil {
		return mor.Subscription{}, unread(op, err)
	}
	return v, nil
}

// Charge is POST /subscriptions/{subscription_id}/charge, of a price and a
// product made for the charge alone and carrying its key, sent only once the
// key is found nowhere: neither in the subscription's next transaction nor in
// any of its transactions.
func (c *Channel) Charge(ctx context.Context, ch mor.Charge) (mor.Transaction, error) {
	const op = "charge"
	if err := c.chargeOK(op, ch); err != nil {
		return mor.Transaction{}, err
	}
	switch {
	case ch.Amount.Minor <= 0:
		return mor.Transaction{}, invalid(op, "nothing to charge")
	case money.Check(ch.Amount.Currency) != nil:
		return mor.Transaction{}, invalid(op, "a currency levyan does not price in: "+ch.Amount.Currency)
	case utf8.RuneCountInString(ch.Description) < 2 || utf8.RuneCountInString(ch.Description) > 200:
		return mor.Transaction{}, invalid(op, "the description is 2 to 200 characters")
	}

	// What the charge is, first: one made is what it is whatever has become
	// of its subscription since, and one that may still be on its way is
	// not known to have been refused.
	s, next, err := c.subscription(ctx, op, ch.Subscription, true)
	if err != nil {
		return mor.Transaction{}, err
	}
	found, err := c.find(ctx, op, s, next, ch.Key)
	if err != nil {
		return mor.Transaction{}, err
	}
	if len(found) > 0 {
		t := found[0]
		if l, _ := t.Line(ch.Key); l.Amount != ch.Amount {
			return mor.Transaction{}, invalid(op, fmt.Sprintf("the key was charged %s, not %s", l.Amount, ch.Amount))
		}
		if len(found) > 1 {
			return t, &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrTwice}
		}
		return t, nil
	}
	if c.tooSoon(ch.Sent) {
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: mor.ErrTooSoon}
	}

	switch {
	case s.Status == mor.Canceled:
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrEnded}
	case s.Status == mor.PastDue:
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrPastDue}
	case s.Currency != ch.Amount.Currency:
		return mor.Transaction{}, invalid(op, fmt.Sprintf("the subscription is billed in %s, and the charge is in %s", s.Currency, ch.Amount.Currency))
	case ch.When == mor.NextPeriod && !s.Renews():
		// Paddle would take it, and forgive it when the subscription ends.
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrNoNextPeriod}
	}

	when := "immediately"
	if ch.When == mor.NextPeriod {
		when = "next_billing_period"
	}
	marked := map[string]string{keyField: ch.Key}
	in := map[string]any{
		"effective_from":     when,
		"on_payment_failure": "prevent_change",
		"items": []map[string]any{{
			"quantity": 1,
			"price": map[string]any{
				"description": ch.Description,
				"tax_mode":    c.c.TaxMode,
				"unit_price":  map[string]string{"amount": fmt.Sprint(ch.Amount.Minor), "currency_code": ch.Amount.Currency},
				"quantity":    map[string]int{"minimum": 1, "maximum": 1},
				"custom_data": marked,
				"product": map[string]any{
					"name":         ch.Description,
					"tax_category": c.c.TaxCategory,
					"custom_data":  marked,
				},
			},
		}},
	}
	var after subscription
	_, err = c.call(ctx, op, http.MethodPost, "/subscriptions/"+ch.Subscription+"/charge", nil, in, &after)
	var e *mor.Error
	switch {
	case err == nil:
	case errors.As(err, &e) && e.Kind == mor.Declined:
		// The charge failed; the request did not. prevent_change made no
		// change, so there is nothing at Paddle to say so.
		return mor.Transaction{
			Subscription: s.ID,
			Customer:     s.Customer,
			Status:       mor.Failed,
			Lines:        []mor.Line{{Key: ch.Key, Name: ch.Description, Amount: ch.Amount}},
			Failure:      &mor.Failure{Kind: mor.Declined, Code: e.Code, Message: e.Message},
		}, nil
	default:
		return mor.Transaction{}, err
	}

	// Made: the charge is what the subscription now holds of the key. A look
	// that does not see it yet is no answer that it is not there.
	made := mor.Transaction{
		Subscription: s.ID,
		Customer:     s.Customer,
		Status:       mor.Pending,
		Lines:        []mor.Line{{Key: ch.Key, Name: ch.Description, Amount: ch.Amount}},
	}
	if ch.When == mor.NextPeriod {
		made.Status = mor.Scheduled
	}
	s, next, err = c.subscription(ctx, op, ch.Subscription, ch.When == mor.NextPeriod)
	if err != nil {
		return made, nil
	}
	found, err = c.find(ctx, op, s, next, ch.Key)
	if err != nil || len(found) == 0 {
		return made, nil
	}
	if len(found) > 1 {
		return found[0], &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrTwice}
	}
	return found[0], nil
}

func (c *Channel) chargeOK(op string, ch mor.Charge) error {
	switch {
	case !idOK("sub", ch.Subscription):
		return invalid(op, "no subscription of Paddle's")
	case !keyOK(ch.Key):
		return invalid(op, "the key is 1 to 64 of letters, digits, '-', '_', '.', ':' or '='")
	case ch.When != mor.Now && ch.When != mor.NextPeriod:
		return invalid(op, "a charge is billed now or with the next period")
	}
	return nil
}

// tooSoon is whether a request last sent at sent may still be on its way into
// Paddle's records.
func (c *Channel) tooSoon(sent time.Time) bool {
	return !sent.IsZero() && c.now().Sub(sent) < mor.Settle
}

// Charged looks for a charge's key where Paddle keeps one-time charges: the
// subscription's next transaction, and its transactions.
func (c *Channel) Charged(ctx context.Context, ch mor.Charge) (mor.Transaction, error) {
	const op = "charge"
	if err := c.chargeOK(op, ch); err != nil {
		return mor.Transaction{}, err
	}
	s, next, err := c.subscription(ctx, op, ch.Subscription, true)
	if err != nil {
		return mor.Transaction{}, err
	}
	found, err := c.find(ctx, op, s, next, ch.Key)
	switch {
	case err != nil:
		return mor.Transaction{}, err
	case len(found) > 1:
		return found[0], &mor.Error{Op: op, Kind: mor.Invalid, Err: mor.ErrTwice}
	case len(found) == 1:
		return found[0], nil
	case c.tooSoon(ch.Sent):
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: mor.ErrTooSoon}
	default:
		return mor.Transaction{}, &mor.Error{Op: op, Kind: mor.NotFound, Err: errors.New("Paddle has no charge of the key")}
	}
}

// maxPages is the most pages of a subscription's transactions looked through
// for a key: 3,000 transactions, more than a subscription charged monthly has
// in a lifetime. One that has more is looked through no further, and the
// look is [mor.Unknown] -- never [mor.NotFound], which only a look through
// all of them may answer.
const maxPages = 100

// find is every transaction of s with a line of key, oldest first, and a
// charge of it scheduled for the next period last.
func (c *Channel) find(ctx context.Context, op string, s mor.Subscription, next *preview, key string) ([]mor.Transaction, error) {
	var found []mor.Transaction
	q := url.Values{
		"subscription_id": {s.ID},
		"include":         {"adjustments"},
		"order_by":        {"id[ASC]"},
		"per_page":        {"30"},
	}
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, &mor.Error{Op: op, Kind: mor.Unknown, Err: errors.New("the subscription has more transactions than are looked through")}
		}
		var ts []transaction
		m, err := c.call(ctx, op, http.MethodGet, "/transactions", q, nil, &ts)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if !t.has(key) {
				continue
			}
			v, err := t.read(c.c.TaxMode)
			if err != nil {
				return nil, unread(op, err)
			}
			found = append(found, v)
		}
		if m.Pagination == nil || !m.Pagination.HasMore || len(ts) == 0 {
			break
		}
		q.Set("after", ts[len(ts)-1].ID)
	}
	if next != nil && next.has(key) {
		v, err := next.read(s, c.c.TaxMode)
		if err != nil {
			return nil, unread(op, err)
		}
		found = append(found, v)
	}
	return found, nil
}

// Transaction is GET /transactions/{transaction_id}, with its adjustments.
func (c *Channel) Transaction(ctx context.Context, id string) (mor.Transaction, error) {
	const op = "transaction"
	if !idOK("txn", id) {
		return mor.Transaction{}, invalid(op, "no transaction of Paddle's")
	}
	return c.transaction(ctx, op, id)
}

func (c *Channel) transaction(ctx context.Context, op, id string) (mor.Transaction, error) {
	var t transaction
	if _, err := c.call(ctx, op, http.MethodGet, "/transactions/"+id, url.Values{"include": {"adjustments"}}, nil, &t); err != nil {
		return mor.Transaction{}, err
	}
	v, err := t.read(c.c.TaxMode)
	if err != nil {
		return mor.Transaction{}, unread(op, err)
	}
	return v, nil
}

// Refund is POST /adjustments, an action of refund, sent only once the
// transaction has no adjustment of the key.
func (c *Channel) Refund(ctx context.Context, r mor.Refund) (mor.Adjustment, error) {
	const op = "refund"
	if err := refundOK(op, r); err != nil {
		return mor.Adjustment{}, err
	}
	switch {
	case r.All && r.Amount.Minor != 0:
		return mor.Adjustment{}, invalid(op, "all of it, or an amount: not both")
	case !r.All && r.Amount.Minor <= 0:
		return mor.Adjustment{}, invalid(op, "nothing to give back")
	case !r.All && r.Line == "":
		return mor.Adjustment{}, invalid(op, "an amount is given back from a line")
	case utf8.RuneCountInString(strings.TrimSpace(r.Reason)) == 0 || utf8.RuneCountInString(r.Reason) > 200:
		return mor.Adjustment{}, invalid(op, "the reason is 1 to 200 characters")
	}

	t, err := c.transaction(ctx, op, r.Transaction)
	if err != nil {
		return mor.Adjustment{}, err
	}
	if a, ok := t.Adjustment(r.Key); ok {
		return a, nil
	}
	if c.tooSoon(r.Sent) {
		return mor.Adjustment{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: mor.ErrTooSoon}
	}
	if !r.All && r.Amount.Currency != t.Total.Currency {
		return mor.Adjustment{}, invalid(op, fmt.Sprintf("the transaction is in %s, and the refund in %s", t.Total.Currency, r.Amount.Currency))
	}
	if r.Line != "" && !hasLine(t, r.Line) {
		return mor.Adjustment{}, invalid(op, "the transaction has no such line")
	}

	in := map[string]any{
		"action":         "refund",
		"transaction_id": r.Transaction,
		"reason":         mark(r.Reason, r.Key),
	}
	switch {
	case r.Line == "":
		in["type"] = "full"
	case r.All:
		in["type"] = "partial"
		in["items"] = []map[string]any{{"item_id": r.Line, "type": "full"}}
	default:
		in["type"] = "partial"
		in["tax_mode"] = c.c.TaxMode
		in["items"] = []map[string]any{{"item_id": r.Line, "type": "partial", "amount": fmt.Sprint(r.Amount.Minor)}}
	}
	var a adjustment
	if _, err := c.call(ctx, op, http.MethodPost, "/adjustments", nil, in, &a); err != nil {
		return mor.Adjustment{}, err
	}
	v, err := a.read()
	if err != nil {
		return mor.Adjustment{}, unread(op, err)
	}
	return v, nil
}

func refundOK(op string, r mor.Refund) error {
	switch {
	case !idOK("txn", r.Transaction):
		return invalid(op, "no transaction of Paddle's")
	case r.Line != "" && !idOK("txnitm", r.Line):
		return invalid(op, "no line of a transaction of Paddle's")
	case !keyOK(r.Key):
		return invalid(op, "the key is 1 to 64 of letters, digits, '-', '_', '.', ':' or '='")
	}
	return nil
}

func hasLine(t mor.Transaction, id string) bool {
	for _, l := range t.Lines {
		if l.ID == id {
			return true
		}
	}
	return false
}

// Refunded looks for a refund's key among the adjustments of its
// transaction.
func (c *Channel) Refunded(ctx context.Context, r mor.Refund) (mor.Adjustment, error) {
	const op = "refund"
	if err := refundOK(op, r); err != nil {
		return mor.Adjustment{}, err
	}
	t, err := c.transaction(ctx, op, r.Transaction)
	if err != nil {
		return mor.Adjustment{}, err
	}
	if a, ok := t.Adjustment(r.Key); ok {
		return a, nil
	}
	if c.tooSoon(r.Sent) {
		return mor.Adjustment{}, &mor.Error{Op: op, Kind: mor.Unknown, Err: mor.ErrTooSoon}
	}
	return mor.Adjustment{}, &mor.Error{Op: op, Kind: mor.NotFound, Err: errors.New("the transaction has no refund of the key")}
}

// idOK is whether s is one of Paddle's ids of the entity prefix names:
// "<prefix>_" and 26 lowercase letters and digits
// (https://developer.paddle.com/api-reference/about/paddle-ids). An id goes
// into a URL's path, so nothing else is taken.
func idOK(prefix, s string) bool {
	v, ok := strings.CutPrefix(s, prefix+"_")
	if !ok || len(v) != 26 {
		return false
	}
	return !strings.ContainsFunc(v, func(r rune) bool { return !('a' <= r && r <= 'z' || '0' <= r && r <= '9') })
}

// keyOK is whether s is a key levyan writes into Paddle: 1 to 64 of letters,
// digits, '-', '_', '.', ':' and '='.
func keyOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-_.:=", r))
	})
}

// mark is a refund's reason with its key at the end, where Paddle keeps it --
// an adjustment has no custom_data -- and [unmark] finds it again.
func mark(reason, key string) string { return reason + " [lv:" + key + "]" }

// unmark is the reason and the key of a reason [mark] wrote, and the reason
// as it is, with no key, of one it did not.
func unmark(s string) (reason, key string) {
	i := strings.LastIndex(s, " [lv:")
	if i < 0 || !strings.HasSuffix(s, "]") {
		return s, ""
	}
	k := s[i+len(" [lv:") : len(s)-1]
	if !keyOK(k) {
		return s, ""
	}
	return s[:i], k
}
