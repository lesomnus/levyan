// Package paddletest is a fake Paddle Billing: an httptest server that speaks
// the part of Paddle's API package paddle calls, as Paddle's documentation
// says Paddle speaks it (package paddle cites the pages), so that the adapter,
// and whatever is built on it, is tested offline.
//
// It is not Paddle. Where the documentation does not say what Paddle does, it
// does something said here, and the live test (paddle's live_test.go) is what
// asks Paddle itself:
//
//   - A charge made now that the card declines leaves nothing behind: no
//     transaction, and the subscription as it was (prevent_change).
//   - A subscription scheduled to cancel or to pause takes a charge for the
//     next period all the same, and forgives it when it ends; it takes a
//     charge made now, too. A cancellation asked again of a subscription
//     scheduled to cancel is refused with subscription_locked_pending_changes.
//   - A price's tax_mode of account_setting is external. There is one tax
//     rate, [Server.TaxRate], everywhere.
//   - A refund waits as pending_approval until [Server.Approve] or
//     [Server.Reject]. A refund of all of a line, or of the transaction,
//     gives back what is left of it once approved refunds are taken off.
//   - A paused subscription takes no charge (subscription_not_active), and
//     is canceled only immediately.
//   - An id is "<prefix>_" and 26 lowercase letters and digits, in the order
//     the fake made them.
//   - It sells one plan in each currency, [Server.Plans]: a checkout of
//     another currency's plan is refused.
//   - It records an event of every change it makes -- transaction.created
//     as a checkout is begun; transaction.paid, transaction.completed and
//     subscription.created as one is paid; subscription.updated,
//     .past_due, .canceled and .paused, transaction.completed, .past_due
//     and .canceled, adjustment.created and .updated as their entities
//     change -- each of its entity as it was then, at once in its event
//     stream (GET /events) unless [Server.Behind] keeps it out for a while.
//     A webhook ([Server.Webhook], [Server.Deliver]) is a delivery of one of
//     those events, by its id. It keeps them all, not 90 days of them.
//   - A payment says the method it was made with ([Server.PayWith]); a
//     renewal is made with the method the checkout was paid with. A renewal
//     made to wait for the payer ([Server.AwaitAction]) is past_due, its
//     payment action_required, until [Server.Act].
//   - A checkout's transaction is canceled only when a test says so
//     ([Server.Abandon]).
//
// Time is the fake's own clock: now, moved by [Server.Advance]. A
// subscription renews when [Server.Renew] says so, not when its time comes.
// A failure is asked for before the request it is for ([Server.Fail],
// [Server.Decline]); a payer finishing a checkout is [Server.Pay], and a
// payer canceling in Paddle's own pages [Server.ScheduleCancel] and
// [Server.CancelNow].
package paddletest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle"
)

// Op is a kind of request the fake answers.
type Op string

const (
	Checkout Op = "checkout" // POST /transactions
	Get      Op = "get"      // GET /transactions/{id}, GET /subscriptions/{id}
	List     Op = "list"     // GET /transactions
	Charge   Op = "charge"   // POST /subscriptions/{id}/charge
	Cancel   Op = "cancel"   // POST /subscriptions/{id}/cancel
	Refund   Op = "refund"   // POST /adjustments
	Events   Op = "events"   // GET /events
)

// Fault is what the next request of an [Op] meets instead of its answer.
type Fault struct {
	// Status and Code are the answer it gets: Paddle's error object of them,
	// or no body for a Code of "". Detail is the object's detail: the code
	// when empty.
	Status int
	Code   string
	Detail string

	// After does the work first, and answers the fault: an answer lost on
	// its way back. Without it nothing is done: the request failed on its
	// way to Paddle.
	After bool

	// Hang does the work, and answers nothing at all until the client gives
	// up.
	Hang bool

	// Unseen keeps what the work wrote out of every answer -- a get, a list,
	// a subscription's next transaction -- until the fake's clock has moved
	// this far: Paddle still writing it down. With neither After nor Hang,
	// the work is done and answered as it is.
	Unseen time.Duration
}

// Server is the fake.
type Server struct {
	// URL is its API, which a [paddle.Config] names.
	URL string

	// APIKey is the key it takes, ClientToken the client-side token its
	// checkout is opened with, and WebhookSecret the endpoint secret key it
	// signs its webhooks with. Each is made up for the server.
	APIKey        string
	ClientToken   string
	WebhookSecret string

	// Plans is the catalog price its checkouts subscribe a payer to, by the
	// currency of the checkout -- USD and KRW -- each a month's, of
	// PlanAmount in that currency. A checkout of another currency's plan,
	// or of none, is refused as Paddle refuses a price it has not.
	Plans      map[string]string
	PlanAmount int64

	// TaxRate is the tax it adds, in basis points: 1,000 is 10%.
	TaxRate int64

	// TaxCategories is what tax categories the account has approved.
	TaxCategories []string

	// PageSize is the most a list answers at once: Paddle's 30 for
	// transactions, and less to make a test page.
	PageSize int

	// EventsPageSize is the most the event stream answers at once: Paddle's
	// 200, and less to make a test page.
	EventsPageSize int

	srv  *httptest.Server
	done chan struct{}

	mu       sync.Mutex
	offset   time.Duration
	seq      int
	subs     map[string]*sub
	txns     map[string]*txn
	adjs     map[string]*adj
	faults   map[Op][]Fault
	declines []string
	awaits   int
	requests map[Op]int
	charged  int
	made     []string // what the request being handled made, by id
	events   []*event
	behind   time.Duration
}

// Method is what the fake's payer pays with: Paddle's method_details, of the
// fields package paddle reads.
type Method struct {
	// Type is Paddle's: card, south_korea_local_card, kakao_pay, ….
	Type string

	// Brand is a card's brand, or a Korean card's company, and Last4 its last
	// four digits; both empty for a method that is no card.
	Brand string
	Last4 string
}

// Card is the card the fake's payer pays with unless told otherwise.
var Card = Method{Type: "card", Brand: "visa", Last4: "4242"}

// event is one of the fake's events: what changed, as it was then.
type event struct {
	id       string
	typ      string
	occurred time.Time
	data     map[string]any
	seen     time.Time // listed from then on
}

type sub struct {
	id         string
	customer   string
	address    string
	method     Method
	status     string
	currency   string
	custom     map[string]any
	plan       item
	start      time.Time
	end        time.Time
	next       time.Time
	change     *change
	canceledAt time.Time
	created    time.Time
	updated    time.Time
	pending    []item // one-time charges for the next period
}

type change struct {
	action string
	at     time.Time
}

// item is what a transaction bills, as one of its items and the line of it.
type item struct {
	line        string // txnitm_…
	price       string // pri_…
	product     string // pro_…
	name        string // the product's
	description string // the price's
	amount      int64  // the unit price
	quantity    int64
	taxMode     string
	category    string
	recurring   bool
	priceData   map[string]any
	productData map[string]any
	hidden      time.Time
}

type txn struct {
	id       string
	status   string
	customer string
	address  string
	sub      string
	currency string
	origin   string
	custom   map[string]any
	items    []item
	period   *[2]time.Time
	created  time.Time
	updated  time.Time
	billed   time.Time
	payments []payment
	checkout string
	hidden   time.Time
}

type payment struct {
	id       string
	amount   int64
	status   string
	code     string
	method   Method
	created  time.Time
	captured time.Time
}

type adj struct {
	id       string
	txn      string
	sub      string
	customer string
	typ      string
	reason   string
	status   string
	currency string
	items    []adjItem
	created  time.Time
	updated  time.Time
	hidden   time.Time
}

type adjItem struct {
	id       string
	line     string
	typ      string
	amount   *int64
	subtotal int64
	tax      int64
	total    int64
}

// New starts a fake for t, which closes it when t ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		APIKey:         "pdl_sdbx_apikey_" + id26() + "_" + alnum(22) + "_" + alnum(3),
		ClientToken:    "test_" + strings.ToLower(alnum(27)),
		WebhookSecret:  "pdl_ntfset_" + id26() + "_" + alnum(32),
		TaxRate:        1_000,
		TaxCategories:  []string{"standard"},
		PageSize:       30,
		EventsPageSize: 200,
		done:           make(chan struct{}),
		subs:           map[string]*sub{},
		txns:           map[string]*txn{},
		adjs:           map[string]*adj{},
		faults:         map[Op][]Fault{},
		requests:       map[Op]int{},
	}
	s.Plans = map[string]string{"USD": s.id("pri"), "KRW": s.id("pri")}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /transactions", s.handle(Checkout, s.checkout))
	mux.HandleFunc("GET /transactions/{id}", s.handle(Get, s.getTransaction))
	mux.HandleFunc("GET /transactions", s.handle(List, s.listTransactions))
	mux.HandleFunc("GET /subscriptions/{id}", s.handle(Get, s.getSubscription))
	mux.HandleFunc("POST /subscriptions/{id}/charge", s.handle(Charge, s.charge))
	mux.HandleFunc("POST /subscriptions/{id}/cancel", s.handle(Cancel, s.cancel))
	mux.HandleFunc("POST /adjustments", s.handle(Refund, s.refund))
	mux.HandleFunc("GET /events", s.handle(Events, s.listEvents))

	s.srv = httptest.NewServer(mux)
	s.URL = s.srv.URL
	t.Cleanup(func() {
		close(s.done)
		s.srv.Close()
	})
	return s
}

// Secret is v as a configuration's secret, as a literal.
func Secret(v string) cfg.Secret {
	var s cfg.Secret
	if err := s.UnmarshalText([]byte(v)); err != nil {
		panic(err)
	}
	return s
}

// Config is a [paddle.Config] that calls the fake, with its keys, its plan
// and its clock.
func (s *Server) Config() paddle.Config {
	return paddle.Config{
		APIKey:        Secret(s.APIKey),
		ClientToken:   s.ClientToken,
		WebhookSecret: Secret(s.WebhookSecret),
		Sandbox:       true,
		Plans:         s.Plans,
		URL:           s.URL,
		Clock:         s.Now,
	}
}

// Now is the fake's time.
func (s *Server) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now()
}

func (s *Server) now() time.Time { return time.Now().Add(s.offset).UTC().Truncate(time.Microsecond) }

// Advance moves the fake's clock on by d.
func (s *Server) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset += d
}

// Fail makes the next request of op meet f.
func (s *Server) Fail(op Op, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[op] = append(s.faults[op], f)
}

// Decline makes the next payment the fake collects declined, with the
// payment error code given ("declined", "not_enough_balance", …): a charge
// made now is refused with subscription_payment_declined, and a renewal is
// past_due.
func (s *Server) Decline(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declines = append(s.declines, code)
}

// Charged is how many payments the fake has collected: how many times a card
// was charged.
func (s *Server) Charged() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.charged
}

// Requests is how many requests of op came, faults and all.
func (s *Server) Requests(op Op) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[op]
}

// Pay is the payer in Paddle's checkout, finishing the checkout c with a card
// that pays ([Card]): the transaction completed, and a subscription made of
// it, which has the transaction's custom_data. A checkout opened with another
// token, or in another environment, is an error, as Paddle.js refuses it.
func (s *Server) Pay(c mor.Checkout) error { return s.PayWith(c, Card) }

// PayWith is [Server.Pay] with the method m: what the checkout's payment, and
// the subscription's renewals after it, say they were made with.
func (s *Server) PayWith(c mor.Checkout, m Method) error {
	if c.Window.Values["token"] != s.ClientToken || c.Window.Values["environment"] != "sandbox" {
		return errors.New("paddletest: the checkout was opened with another token, or in another environment")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[c.Window.Values["transactionId"]]
	if !ok || t.id != c.ID {
		return errors.New("paddletest: no such checkout")
	}
	if t.status != "draft" && t.status != "ready" {
		return fmt.Errorf("paddletest: the checkout is %s", t.status)
	}
	now := s.now()
	if t.customer == "" {
		t.customer = s.id("ctm")
	}
	t.address = s.id("add")
	if t.currency == "" {
		t.currency = "USD"
	}
	_, _, total := s.sum(t.items)
	t.payments = []payment{{id: s.id("pay"), amount: total, status: "captured", method: m, created: now, captured: now}}
	if total > 0 {
		s.charged++
	}
	t.status, t.billed, t.updated = "completed", now, now

	su := &sub{
		id:       s.id("sub"),
		customer: t.customer,
		address:  t.address,
		method:   m,
		status:   "active",
		currency: t.currency,
		custom:   t.custom,
		plan:     t.items[0],
		start:    now,
		end:      now.AddDate(0, 1, 0),
		created:  now,
		updated:  now,
	}
	su.next = su.end
	s.subs[su.id] = su
	t.sub = su.id
	s.emit("transaction.paid", t.object(s, false))
	s.emit("transaction.completed", t.object(s, false))
	s.emit("subscription.created", su.object(s, false))
	return nil
}

// Renew is Paddle billing a subscription at the end of its period: a
// scheduled cancellation or pause taking effect -- and every charge for the
// next period forgiven -- or a renewal, billing the plan and those charges,
// collected, past due, or waiting for the payer ([Server.AwaitAction]). It
// answers the renewal's transaction, or "" for none.
func (s *Server) Renew(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	su, ok := s.subs[id]
	if !ok {
		panic(fmt.Sprintf("paddletest: no subscription %q", id))
	}
	now := s.now()
	su.updated = now
	if su.change != nil {
		switch su.change.action {
		case "cancel":
			s.end(su, now)
			s.emit("subscription.canceled", su.object(s, false))
		case "pause":
			su.status, su.start, su.end, su.next, su.pending = "paused", time.Time{}, time.Time{}, time.Time{}, nil
			s.emit("subscription.paused", su.object(s, false))
		}
		su.change = nil
		return ""
	}
	if su.status != "active" && su.status != "trialing" {
		return ""
	}

	t := &txn{
		id:       s.id("txn"),
		customer: su.customer,
		address:  su.address,
		sub:      su.id,
		currency: su.currency,
		origin:   "subscription_recurring",
		items:    append([]item{s.lineOf(su.plan)}, su.pending...),
		period:   &[2]time.Time{su.end, su.end.AddDate(0, 1, 0)},
		created:  now,
		updated:  now,
		billed:   now,
	}
	for i := range t.items {
		if t.items[i].line == "" {
			t.items[i].line = s.id("txnitm")
		}
		t.items[i].hidden = time.Time{}
	}
	su.pending = nil
	su.start, su.end = su.end, su.end.AddDate(0, 1, 0)
	su.next = su.end
	_, _, total := s.sum(t.items)
	if code, ok := s.decline(); ok {
		t.status = "past_due"
		t.payments = []payment{{id: s.id("pay"), amount: total, status: "error", code: code, method: su.method, created: now}}
		su.status = "past_due"
	} else if s.awaits > 0 {
		s.awaits--
		t.status = "past_due"
		t.payments = []payment{{id: s.id("pay"), amount: total, status: "action_required", method: su.method, created: now}}
		su.status = "past_due"
	} else {
		t.status = "completed"
		t.payments = []payment{{id: s.id("pay"), amount: total, status: "captured", method: su.method, created: now, captured: now}}
		s.charged++
	}
	s.txns[t.id] = t
	if t.status == "completed" {
		s.emit("transaction.completed", t.object(s, false))
		s.emit("subscription.updated", su.object(s, false))
	} else {
		s.emit("transaction.past_due", t.object(s, false))
		s.emit("subscription.past_due", su.object(s, false))
	}
	return t.id
}

// Abandon is Paddle canceling the transaction of a checkout the payer never
// finished. Its documentation does not say when it does, or whether it does
// at all; a checkout so canceled is begun anew.
func (s *Server) Abandon(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[id]
	if !ok || (t.status != "draft" && t.status != "ready") {
		panic(fmt.Sprintf("paddletest: no open checkout %q", id))
	}
	t.status, t.updated = "canceled", s.now()
	s.emit("transaction.canceled", t.object(s, false))
}

// AwaitAction makes the next renewal the fake bills wait for the payer to
// act on its payment -- approve it in their card company's app, or pass
// their bank's check -- which [Server.Act] is: the payment action_required,
// the transaction and the subscription past_due until then. Paddle's
// documentation does not say what a renewal with a Korean card that is not
// saved does (TestLive asks); this is the fake's guess, for what levyan does
// with it.
func (s *Server) AwaitAction() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaits++
}

// Act is the payer acting on the payment transaction id waits for: it is
// collected, and its subscription is active again.
func (s *Server) Act(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[id]
	if !ok || len(t.payments) == 0 || t.payments[len(t.payments)-1].status != "action_required" {
		panic(fmt.Sprintf("paddletest: no payment of %q waits for the payer", id))
	}
	now := s.now()
	p := &t.payments[len(t.payments)-1]
	p.status, p.captured = "captured", now
	t.status, t.updated = "completed", now
	s.charged++
	s.emit("transaction.completed", t.object(s, false))
	if su, ok := s.subs[t.sub]; ok && su.status == "past_due" {
		su.status, su.updated = "active", now
		s.emit("subscription.updated", su.object(s, false))
	}
}

// ScheduleCancel is the payer canceling a subscription in Paddle's own pages,
// at the end of its period: what levyan learns of from a webhook.
func (s *Server) ScheduleCancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	su := s.subs[id]
	su.change = &change{action: "cancel", at: su.next}
	su.next, su.updated = time.Time{}, s.now()
	s.emit("subscription.updated", su.object(s, false))
}

// CancelNow is the payer canceling a subscription in Paddle's own pages at
// once -- as the link in Paddle's email does to a subscription of nothing
// (https://developer.paddle.com/changelog/2023/zero-dollar-subscription-emails)
// -- without asking levyan.
func (s *Server) CancelNow(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	su := s.subs[id]
	now := s.now()
	s.end(su, now)
	su.updated = now
	s.emit("subscription.canceled", su.object(s, false))
}

// Duplicate is a charge made twice -- by a caller that sent its key again
// before it could have been seen: a transaction like t, under its own id.
func (s *Server) Duplicate(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := *s.txns[id]
	t.id = s.id("txn")
	t.items = slices.Clone(t.items)
	for i := range t.items {
		t.items[i].line = s.id("txnitm")
	}
	t.created, t.updated = s.now(), s.now()
	s.txns[t.id] = &t
	s.charged++
	s.emit("transaction.completed", t.object(s, false))
	return t.id
}

// Approve is Paddle approving a refund: given back.
func (s *Server) Approve(id string) { s.decide(id, "approved") }

// Reject is Paddle rejecting a refund: nothing given back.
func (s *Server) Reject(id string) { s.decide(id, "rejected") }

func (s *Server) decide(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.adjs[id]
	if !ok || a.status != "pending_approval" {
		panic(fmt.Sprintf("paddletest: no refund %q waiting for approval", id))
	}
	a.status, a.updated = status, s.now()
	s.emit("adjustment.updated", a.object(s))
}

// Behind keeps every event the fake records from now on out of its event
// stream until the fake's clock has moved d past it: Paddle still writing
// its stream when it is read, which its documentation does not say it does
// or does not. Zero puts the stream back in step.
func (s *Server) Behind(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behind = d
}

// Events is the id and the type of every event the fake has recorded, oldest
// first.
func (s *Server) Events() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][2]string, len(s.events))
	for i, e := range s.events {
		out[i] = [2]string{e.id, e.typ}
	}
	return out
}

// Webhook is what Paddle sends when the entity of id changes -- a
// transaction, a subscription or an adjustment, as it is now -- as the event
// named, signed with WebhookSecret at the fake's now. The event is recorded
// in the fake's event stream too, as Paddle's events are both delivered and
// listed.
func (s *Server) Webhook(event, id string) (http.Header, []byte) {
	s.mu.Lock()
	var data map[string]any
	switch {
	case strings.HasPrefix(id, "txn_"):
		data = s.txns[id].object(s, false)
	case strings.HasPrefix(id, "sub_"):
		data = s.subs[id].object(s, false)
	case strings.HasPrefix(id, "adj_"):
		data = s.adjs[id].object(s)
	default:
		s.mu.Unlock()
		panic(fmt.Sprintf("paddletest: no entity %q", id))
	}
	e := s.emit(event, data)
	s.mu.Unlock()
	return s.Deliver(e.id)
}

// Deliver is a delivery of the event recorded as id, as Paddle sends it --
// and sends it again -- signed at the fake's now.
func (s *Server) Deliver(id string) (http.Header, []byte) {
	s.mu.Lock()
	var found *event
	for _, e := range s.events {
		if e.id == id {
			found = e
		}
	}
	if found == nil {
		s.mu.Unlock()
		panic(fmt.Sprintf("paddletest: no event %q", id))
	}
	body, err := json.Marshal(map[string]any{
		"event_id":        found.id,
		"event_type":      found.typ,
		"occurred_at":     stamp(found.occurred),
		"notification_id": s.id("ntf"),
		"data":            found.data,
	})
	now := s.now()
	s.mu.Unlock()
	if err != nil {
		panic(err)
	}
	return s.Sign(body, now), body
}

// Sign is the header Paddle sends body with when it sends it at at: its
// signature under WebhookSecret
// (https://developer.paddle.com/webhooks/about/signature-verification).
// Paddle signs a delivery when it sends it, so a webhook sent again is body
// signed again.
func (s *Server) Sign(body []byte, at time.Time) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(s.WebhookSecret))
	m.Write([]byte(ts + ":"))
	m.Write(body)
	h := http.Header{}
	h.Set(paddle.SignatureHeader, "ts="+ts+";h1="+hex.EncodeToString(m.Sum(nil)))
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Paddle")
	return h
}

// handle is a request of op: its key checked, then a fault on its way to
// Paddle met, and only then f, and whatever fault is left.
func (s *Server) handle(op Op, f func(r *http.Request, body []byte) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		s.mu.Lock()
		s.requests[op]++
		switch a := r.Header.Get("Authorization"); {
		case a == "":
			s.mu.Unlock()
			refuse(w, http.StatusForbidden, "authentication_missing", "Authentication header missing.")
			return
		case a != "Bearer "+s.APIKey:
			s.mu.Unlock()
			refuse(w, http.StatusForbidden, "invalid_token", "Invalid or revoked API key.")
			return
		case r.Header.Get("Paddle-Version") != "1":
			s.mu.Unlock()
			refuse(w, http.StatusBadRequest, "invalid_field", "Paddle-Version: the fake speaks version 1.")
			return
		}
		var fault *Fault
		if fs := s.faults[op]; len(fs) > 0 {
			fault, s.faults[op] = &fs[0], fs[1:]
		}
		if fault != nil && !fault.After && !fault.Hang && fault.Unseen == 0 {
			s.mu.Unlock()
			refuse(w, fault.Status, fault.Code, fault.Detail)
			return
		}
		s.made = nil
		status, v := f(r, body)
		if fault != nil && fault.Unseen > 0 {
			s.hide(s.now().Add(fault.Unseen))
		}
		s.mu.Unlock()

		switch {
		case fault == nil || (!fault.After && !fault.Hang):
			write(w, status, v)
		case fault.Hang:
			select {
			case <-r.Context().Done():
			case <-s.done:
			}
		default:
			refuse(w, fault.Status, fault.Code, fault.Detail)
		}
	}
}

// hide keeps what the request just handled made out of sight until until: a
// transaction, an adjustment, a charge for the next period.
func (s *Server) hide(until time.Time) {
	for _, id := range s.made {
		if t, ok := s.txns[id]; ok {
			t.hidden = until
		}
		if a, ok := s.adjs[id]; ok {
			a.hidden = until
		}
		for _, su := range s.subs {
			for i := range su.pending {
				if su.pending[i].product == id {
					su.pending[i].hidden = until
				}
			}
		}
	}
}

func write(w http.ResponseWriter, status int, v any) {
	if e, ok := v.(failure); ok {
		refuse(w, status, e.code, e.detail)
		return
	}
	m := map[string]any{"request_id": requestID()}
	if p, ok := v.(page); ok {
		v = p.data
		m["pagination"] = map[string]any{"per_page": p.perPage, "next": p.next, "has_more": p.more, "estimated_total": -1}
	}
	b, err := json.Marshal(map[string]any{"data": v, "meta": m})
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func refuse(w http.ResponseWriter, status int, code, detail string) {
	if code == "" {
		w.WriteHeader(status)
		return
	}
	if detail == "" {
		detail = code
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":              "request_error",
			"code":              code,
			"detail":            detail,
			"documentation_url": "https://developer.paddle.com/errors/shared/" + code,
		},
		"meta": map[string]any{"request_id": requestID()},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// failure is an answer of Paddle's error object.
type failure struct{ code, detail string }

func no(code, detail string) any { return failure{code, detail} }

// The functions below are called with s.mu held.

// id is a new id of an entity of prefix, after every id the fake made
// before.
func (s *Server) id(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s_%010d%s", prefix, s.seq, strings.ToLower(alnum(16)))
}

// seen is whether something hidden until until is in sight now.
func (s *Server) seen(until time.Time) bool { return until.IsZero() || !s.now().Before(until) }

// emit records an event of typ, of data as it is now, in the fake's event
// stream: listed at once, or once the fake's clock has moved [Server.Behind]
// past it.
func (s *Server) emit(typ string, data map[string]any) *event {
	now := s.now()
	e := &event{id: s.id("evt"), typ: typ, occurred: now, data: data}
	if s.behind > 0 {
		e.seen = now.Add(s.behind)
	}
	s.events = append(s.events, e)
	return e
}

// listEvents is GET /events: the event stream, by id, after a cursor or from
// a time (https://developer.paddle.com/api-reference/events/list-events).
// The fake keeps every event, and none of them for 90 days only.
func (s *Server) listEvents(r *http.Request, _ []byte) (int, any) {
	q := r.URL.Query()
	asc := q.Get("order_by") == "id[ASC]"
	if o := q.Get("order_by"); o != "" && o != "id[ASC]" && o != "id[DESC]" {
		return http.StatusBadRequest, no("invalid_field", "order_by: the fake orders by id")
	}
	per := 50
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		per = min(n, 200)
	}
	per = min(per, s.EventsPageSize)
	var from time.Time
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return http.StatusBadRequest, no("invalid_field", "from: an RFC 3339 time")
		}
		from = t
	}
	var es []*event
	for _, e := range s.events {
		if s.seen(e.seen) && !e.occurred.Before(from) {
			es = append(es, e)
		}
	}
	slices.SortFunc(es, func(a, b *event) int {
		if asc {
			return strings.Compare(a.id, b.id)
		}
		return strings.Compare(b.id, a.id)
	})
	if after := q.Get("after"); after != "" {
		// After an id, whether or not it is listed: ids sort as their
		// entities were made.
		i := slices.IndexFunc(es, func(e *event) bool {
			if asc {
				return e.id > after
			}
			return e.id < after
		})
		if i < 0 {
			i = len(es)
		}
		es = es[i:]
	}
	more := len(es) > per
	if more {
		es = es[:per]
	}
	data := make([]map[string]any, len(es))
	for i, e := range es {
		data[i] = map[string]any{"event_id": e.id, "event_type": e.typ, "occurred_at": stamp(e.occurred), "data": e.data}
	}
	if len(es) > 0 {
		q.Set("after", es[len(es)-1].id)
	}
	return http.StatusOK, page{data: data, perPage: per, next: s.URL + "/events?" + q.Encode(), more: more}
}

func (s *Server) decline() (string, bool) {
	if len(s.declines) == 0 {
		return "", false
	}
	code := s.declines[0]
	s.declines = s.declines[1:]
	return code, true
}

// tax is the tax of an amount, and what of it is before tax, under a tax mode.
func (s *Server) tax(amount int64, mode string) (subtotal, tax, total int64) {
	if mode == "internal" {
		subtotal = amount * 10_000 / (10_000 + s.TaxRate)
		return subtotal, amount - subtotal, amount
	}
	tax = amount * s.TaxRate / 10_000
	return amount, tax, amount + tax
}

func (s *Server) totals(it item) (subtotal, tax, total int64) {
	return s.tax(it.amount*it.quantity, it.taxMode)
}

func (s *Server) sum(items []item) (subtotal, tax, total int64) {
	for _, it := range items {
		a, b, c := s.totals(it)
		subtotal, tax, total = subtotal+a, tax+b, total+c
	}
	return
}

// minimums is the least Paddle charges in a currency
// (https://developer.paddle.com/concepts/sell/supported-currencies).
var minimums = map[string]int64{"USD": 70, "EUR": 65, "JPY": 100, "KRW": 980}

func (s *Server) lineOf(it item) item {
	it.line = s.id("txnitm")
	return it
}

func (s *Server) checkout(_ *http.Request, body []byte) (int, any) {
	var in struct {
		Items []struct {
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
		} `json:"items"`
		CustomerID     string         `json:"customer_id"`
		CurrencyCode   string         `json:"currency_code"`
		CollectionMode string         `json:"collection_mode"`
		CustomData     map[string]any `json:"custom_data"`
	}
	if json.Unmarshal(body, &in) != nil || len(in.Items) != 1 || in.Items[0].Quantity < 1 {
		return http.StatusBadRequest, no("invalid_field", "items: the fake sells one plan")
	}
	plan := ""
	for cur, p := range s.Plans {
		if p == in.Items[0].PriceID {
			plan = cur
		}
	}
	if plan == "" {
		return http.StatusNotFound, no("not_found", "Entity "+in.Items[0].PriceID+" not found")
	}
	if in.CurrencyCode != "" && in.CurrencyCode != plan {
		return http.StatusBadRequest, no("invalid_field", "currency_code: the plan is priced in "+plan)
	}
	if in.CollectionMode != "" && in.CollectionMode != "automatic" {
		return http.StatusBadRequest, no("invalid_field", "collection_mode: the fake collects automatically")
	}
	now := s.now()
	t := &txn{
		id:       s.id("txn"),
		status:   "draft",
		customer: in.CustomerID,
		currency: in.CurrencyCode,
		origin:   "api",
		custom:   in.CustomData,
		created:  now,
		updated:  now,
	}
	cur := t.currency
	if cur == "" {
		cur = "USD"
	}
	t.items = []item{{
		line:        s.id("txnitm"),
		price:       in.Items[0].PriceID,
		product:     s.id("pro"),
		name:        "levyan",
		description: "monthly",
		amount:      s.PlanAmount,
		quantity:    in.Items[0].Quantity,
		taxMode:     "external",
		category:    "standard",
		recurring:   true,
	}}
	t.checkout = s.URL + "/pay?_ptxn=" + t.id
	s.txns[t.id] = t
	s.made = append(s.made, t.id)
	s.emit("transaction.created", t.object(s, false))
	return http.StatusCreated, t.object(s, false)
}

func (s *Server) getTransaction(r *http.Request, _ []byte) (int, any) {
	t, ok := s.txns[r.PathValue("id")]
	if !ok || !s.seen(t.hidden) {
		return http.StatusNotFound, no("not_found", "Entity "+r.PathValue("id")+" not found")
	}
	return http.StatusOK, t.object(s, includes(r, "adjustments"))
}

func (s *Server) listTransactions(r *http.Request, _ []byte) (int, any) {
	q := r.URL.Query()
	asc := q.Get("order_by") == "id[ASC]"
	if o := q.Get("order_by"); o != "" && o != "id[ASC]" && o != "id[DESC]" {
		return http.StatusBadRequest, no("invalid_field", "order_by: the fake orders by id")
	}
	per := s.PageSize
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 && n < per {
		per = n
	}
	subs := q.Get("subscription_id")
	var ts []*txn
	for _, t := range s.txns {
		if !s.seen(t.hidden) || (subs != "" && t.sub != subs) {
			continue
		}
		ts = append(ts, t)
	}
	slices.SortFunc(ts, func(a, b *txn) int {
		if asc {
			return strings.Compare(a.id, b.id)
		}
		return strings.Compare(b.id, a.id)
	})
	if after := q.Get("after"); after != "" {
		i := slices.IndexFunc(ts, func(t *txn) bool { return t.id == after })
		if i < 0 {
			return http.StatusBadRequest, no("invalid_field", "after: no such cursor")
		}
		ts = ts[i+1:]
	}
	more := len(ts) > per
	if more {
		ts = ts[:per]
	}
	data := make([]map[string]any, len(ts))
	for i, t := range ts {
		data[i] = t.object(s, includes(r, "adjustments"))
	}
	// Paddle always says what the next page would be; has_more says whether
	// there is one.
	if len(ts) > 0 {
		q.Set("after", ts[len(ts)-1].id)
	}
	return http.StatusOK, page{data: data, perPage: per, next: s.URL + "/transactions?" + q.Encode(), more: more}
}

// page is a list's answer: its data, and its meta's pagination.
type page struct {
	data    []map[string]any
	perPage int
	next    string
	more    bool
}

func (s *Server) getSubscription(r *http.Request, _ []byte) (int, any) {
	su, ok := s.subs[r.PathValue("id")]
	if !ok {
		return http.StatusNotFound, no("not_found", "Entity "+r.PathValue("id")+" not found")
	}
	return http.StatusOK, su.object(s, includes(r, "next_transaction"))
}

func (s *Server) charge(r *http.Request, body []byte) (int, any) {
	su, ok := s.subs[r.PathValue("id")]
	if !ok {
		return http.StatusNotFound, no("not_found", "Entity "+r.PathValue("id")+" not found")
	}
	now := s.now()
	switch {
	case su.status == "canceled":
		return http.StatusBadRequest, no("subscription_update_when_canceled", "Cannot update subscription, as subscription is canceled.")
	case su.status == "past_due":
		return http.StatusBadRequest, no("subscription_update_when_past_due", "Cannot update subscription, as the subscription status is 'past_due'.")
	case su.status == "paused":
		return http.StatusBadRequest, no("subscription_not_active", "Action requires the subscription to be active.")
	case !su.next.IsZero() && su.next.Sub(now) < 30*time.Minute:
		return http.StatusConflict, no("subscription_locked_renewal", "Unable to update subscription within 30 minutes of renewal.")
	}

	var in struct {
		EffectiveFrom    string `json:"effective_from"`
		OnPaymentFailure string `json:"on_payment_failure"`
		Items            []struct {
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
			Price    *struct {
				Description string `json:"description"`
				Name        string `json:"name"`
				TaxMode     string `json:"tax_mode"`
				UnitPrice   struct {
					Amount       string `json:"amount"`
					CurrencyCode string `json:"currency_code"`
				} `json:"unit_price"`
				CustomData map[string]any `json:"custom_data"`
				Product    *struct {
					Name        string         `json:"name"`
					TaxCategory string         `json:"tax_category"`
					CustomData  map[string]any `json:"custom_data"`
				} `json:"product"`
			} `json:"price"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &in) != nil {
		return http.StatusBadRequest, no("invalid_json", "JSON payload invalid.")
	}
	switch {
	case in.EffectiveFrom != "immediately" && in.EffectiveFrom != "next_billing_period":
		return http.StatusBadRequest, no("invalid_field", "effective_from: must be immediately or next_billing_period")
	case in.OnPaymentFailure != "" && in.OnPaymentFailure != "prevent_change":
		return http.StatusBadRequest, no("invalid_field", "on_payment_failure: the fake speaks prevent_change")
	case len(in.Items) < 1 || len(in.Items) > 100:
		return http.StatusBadRequest, no("invalid_field", "items: 1 to 100")
	}
	var items []item
	for _, it := range in.Items {
		p := it.Price
		switch {
		case p == nil || it.PriceID != "":
			return http.StatusBadRequest, no("invalid_field", "items: the fake bills non-catalog prices")
		case it.Quantity < 1:
			return http.StatusBadRequest, no("invalid_field", "items.quantity: at least 1")
		case utf8.RuneCountInString(p.Description) < 2 || utf8.RuneCountInString(p.Description) > 200:
			return http.StatusBadRequest, no("invalid_field", "items.price.description: 2 to 200 characters")
		case p.UnitPrice.CurrencyCode != su.currency:
			return http.StatusBadRequest, no("invalid_field", "items.price.unit_price.currency_code: not the subscription's")
		case p.Product == nil || p.Product.Name == "" || utf8.RuneCountInString(p.Product.Name) > 200:
			return http.StatusBadRequest, no("invalid_field", "items.price.product.name: 1 to 200 characters")
		case !slices.Contains(s.TaxCategories, p.Product.TaxCategory):
			return http.StatusBadRequest, no("subscription_product_tax_category_not_approved", "tax category for non-catalog item not approved")
		}
		n, err := strconv.ParseInt(p.UnitPrice.Amount, 10, 64)
		if err != nil || n < 1 {
			return http.StatusBadRequest, no("invalid_field", "items.price.unit_price.amount: a whole amount")
		}
		mode := p.TaxMode
		switch mode {
		case "", "account_setting":
			mode = "external"
		case "external", "internal":
		default:
			return http.StatusBadRequest, no("invalid_field", "items.price.tax_mode")
		}
		items = append(items, item{
			price:       s.id("pri"),
			product:     s.id("pro"),
			name:        p.Product.Name,
			description: p.Description,
			amount:      n,
			quantity:    it.Quantity,
			taxMode:     mode,
			category:    p.Product.TaxCategory,
			priceData:   p.CustomData,
			productData: p.Product.CustomData,
		})
	}

	if in.EffectiveFrom == "next_billing_period" {
		for _, it := range items {
			s.made = append(s.made, it.product)
		}
		su.pending = append(su.pending, items...)
		su.updated = now
		s.emit("subscription.updated", su.object(s, false))
		return http.StatusCreated, su.object(s, false)
	}

	_, _, total := s.sum(items)
	if min := minimums[su.currency]; total < min {
		return http.StatusBadRequest, no("subscription_update_transaction_balance_less_than_charge_limit",
			fmt.Sprintf("Unable to charge for Subscription update: Transaction balance is less than what we can charge. Transaction balance: %d, Minimum payment amount: %d, Currency code: %s", total, min, su.currency))
	}
	if _, ok := s.decline(); ok {
		return http.StatusBadRequest, no("subscription_payment_declined", "Payment declined.")
	}
	t := &txn{
		id:       s.id("txn"),
		status:   "completed",
		customer: su.customer,
		address:  su.address,
		sub:      su.id,
		currency: su.currency,
		origin:   "subscription_charge",
		items:    items,
		created:  now,
		updated:  now,
		billed:   now,
	}
	for i := range t.items {
		t.items[i].line = s.id("txnitm")
	}
	t.payments = []payment{{id: s.id("pay"), amount: total, status: "captured", method: su.method, created: now, captured: now}}
	s.txns[t.id] = t
	s.made = append(s.made, t.id)
	s.charged++
	su.updated = now
	s.emit("transaction.completed", t.object(s, false))
	s.emit("subscription.updated", su.object(s, false))
	return http.StatusCreated, su.object(s, false)
}

func (s *Server) cancel(r *http.Request, body []byte) (int, any) {
	su, ok := s.subs[r.PathValue("id")]
	if !ok {
		return http.StatusNotFound, no("not_found", "Entity "+r.PathValue("id")+" not found")
	}
	var in struct {
		EffectiveFrom *string `json:"effective_from"`
	}
	if len(body) > 0 && json.Unmarshal(body, &in) != nil {
		return http.StatusBadRequest, no("invalid_json", "JSON payload invalid.")
	}
	when := "next_billing_period"
	if in.EffectiveFrom != nil {
		when = *in.EffectiveFrom
	}
	now := s.now()
	switch {
	case su.status == "canceled":
		return http.StatusBadRequest, no("subscription_is_canceled_action_invalid", "Action can't be performed on canceled subscription.")
	case when != "immediately" && when != "next_billing_period":
		return http.StatusBadRequest, no("invalid_field", "effective_from: must be immediately or next_billing_period")
	case when == "immediately":
		s.end(su, now)
	case su.status == "paused":
		return http.StatusBadRequest, no("subscription_not_active", "A paused subscription is canceled immediately.")
	case su.change != nil:
		return http.StatusBadRequest, no("subscription_locked_pending_changes", "The subscription already has a scheduled change.")
	default:
		su.change = &change{action: "cancel", at: su.next}
		su.next = time.Time{}
	}
	su.updated = now
	if su.status == "canceled" {
		s.emit("subscription.canceled", su.object(s, false))
	} else {
		s.emit("subscription.updated", su.object(s, false))
	}
	return http.StatusOK, su.object(s, false)
}

// end cancels a subscription now: its charges for the next period forgiven,
// and its transactions not paid canceled.
func (s *Server) end(su *sub, now time.Time) {
	su.status, su.canceledAt = "canceled", now
	su.start, su.end, su.next, su.change, su.pending = time.Time{}, time.Time{}, time.Time{}, nil, nil
	for _, t := range s.txns {
		if t.sub == su.id && (t.status == "draft" || t.status == "ready" || t.status == "billed" || t.status == "past_due") {
			t.status, t.updated = "canceled", now
		}
	}
}

func (s *Server) refund(_ *http.Request, body []byte) (int, any) {
	var in struct {
		Action        string `json:"action"`
		Type          string `json:"type"`
		TaxMode       string `json:"tax_mode"`
		TransactionID string `json:"transaction_id"`
		Reason        string `json:"reason"`
		Items         []struct {
			ItemID string  `json:"item_id"`
			Type   string  `json:"type"`
			Amount *string `json:"amount"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &in) != nil {
		return http.StatusBadRequest, no("invalid_json", "JSON payload invalid.")
	}
	t, ok := s.txns[in.TransactionID]
	switch {
	case in.Action != "refund":
		return http.StatusBadRequest, no("invalid_field", "action: the fake refunds")
	case !ok || !s.seen(t.hidden):
		return http.StatusNotFound, no("not_found", "Entity "+in.TransactionID+" not found")
	case strings.TrimSpace(in.Reason) == "":
		return http.StatusBadRequest, no("invalid_field", "reason: must be provided")
	case t.status != "completed":
		return http.StatusBadRequest, no("adjustment_transaction_invalid_status_for_refund", "Transaction status not supported for refund adjustment.")
	}
	for _, a := range s.adjs {
		if a.txn == t.id && a.status == "pending_approval" {
			return http.StatusBadRequest, no("adjustment_pending_refund_request", "The transaction has a refund pending approval.")
		}
	}
	mode := in.TaxMode
	if mode == "" {
		mode = "internal"
	}

	now := s.now()
	a := &adj{id: s.id("adj"), txn: t.id, sub: t.sub, customer: t.customer, reason: in.Reason, status: "pending_approval", currency: t.currency, created: now, updated: now}
	switch in.Type {
	case "full":
		for _, it := range t.items {
			left := s.left(t, it)
			if left == 0 {
				continue
			}
			sub, tax := s.split(it, left)
			a.items = append(a.items, adjItem{id: s.id("adjitm"), line: it.line, typ: "full", subtotal: sub, tax: tax, total: left})
		}
		if len(a.items) == 0 {
			return http.StatusBadRequest, no("adjustment_total_amount_above_remaining_allowed", "Nothing is left of the transaction to adjust.")
		}
		a.typ = "full"
	case "", "partial":
		if len(in.Items) == 0 {
			return http.StatusBadRequest, no("invalid_field", "items: required for a partial adjustment")
		}
		for _, x := range in.Items {
			i := slices.IndexFunc(t.items, func(it item) bool { return it.line == x.ItemID })
			if i < 0 {
				return http.StatusBadRequest, no("invalid_field", "items.item_id: no such item of the transaction")
			}
			it := t.items[i]
			left := s.left(t, it)
			var total int64
			switch x.Type {
			case "full":
				if left == 0 {
					return http.StatusBadRequest, no("adjustment_transaction_item_has_already_been_fully_adjusted", "Transaction item already fully adjusted.")
				}
				total = left
			case "partial":
				if x.Amount == nil {
					return http.StatusBadRequest, no("invalid_field", "items.amount: required for a partial item")
				}
				n, err := strconv.ParseInt(*x.Amount, 10, 64)
				switch {
				case err != nil:
					return http.StatusBadRequest, no("invalid_field", "items.amount: a whole amount")
				case n == 0:
					return http.StatusBadRequest, no("adjustment_amount_cannot_be_zero", "Adjustment amount can't be zero.")
				}
				if mode == "external" {
					_, _, n = s.tax(n, "external")
				}
				total = n
			default:
				return http.StatusBadRequest, no("invalid_field", "items.type: full or partial")
			}
			if total > left {
				return http.StatusBadRequest, no("adjustment_amount_above_remaining_allowed", "Adjustment item amount exceeds the remaining allowed.")
			}
			sub, tax := s.split(it, total)
			amt := total
			a.items = append(a.items, adjItem{id: s.id("adjitm"), line: it.line, typ: x.Type, amount: &amt, subtotal: sub, tax: tax, total: total})
		}
		a.typ = "partial"
	default:
		return http.StatusBadRequest, no("invalid_field", "type: full or partial")
	}
	s.adjs[a.id] = a
	s.made = append(s.made, a.id)
	s.emit("adjustment.created", a.object(s))
	return http.StatusCreated, a.object(s)
}

// left is what is left of a line to give back: its total, less the refunds
// of it approved.
func (s *Server) left(t *txn, it item) int64 {
	_, _, total := s.totals(it)
	for _, a := range s.adjs {
		if a.txn != t.id || a.status != "approved" {
			continue
		}
		for _, x := range a.items {
			if x.line == it.line {
				total -= x.total
			}
		}
	}
	return total
}

// split is a total given back of a line, as what of it is tax.
func (s *Server) split(it item, total int64) (subtotal, tax int64) {
	_, t, all := s.totals(it)
	if all == 0 {
		return 0, 0
	}
	tax = t * total / all
	return total - tax, tax
}

func includes(r *http.Request, what string) bool {
	for _, v := range strings.Split(r.URL.Query().Get("include"), ",") {
		if v == what {
			return true
		}
	}
	return false
}

// object is t as Paddle's transaction entity
// (https://developer.paddle.com/api-reference/transactions/get-transaction).
func (t *txn) object(s *Server, adjustments bool) map[string]any {
	sub, tax, total := s.sum(t.items)
	cur := t.currency
	if cur == "" {
		cur = "USD"
	}
	items := make([]map[string]any, len(t.items))
	lines := make([]map[string]any, len(t.items))
	for i, it := range t.items {
		items[i] = map[string]any{"price": it.priceObject(cur), "quantity": it.quantity, "proration": nil}
		lines[i] = s.lineObject(it, cur)
		lines[i]["id"] = it.line
	}
	o := map[string]any{
		"id":              t.id,
		"status":          t.status,
		"customer_id":     orNil(t.customer),
		"address_id":      orNil(t.address),
		"business_id":     nil,
		"custom_data":     t.custom,
		"currency_code":   cur,
		"origin":          t.origin,
		"subscription_id": orNil(t.sub),
		"invoice_id":      nil,
		"invoice_number":  nil,
		"collection_mode": "automatic",
		"discount_id":     nil,
		"billing_details": nil,
		"billing_period":  nil,
		"items":           items,
		"details": map[string]any{
			"tax_rates_used": []map[string]any{{"tax_rate": s.rate(), "totals": map[string]any{"subtotal": str(sub), "discount": "0", "tax": str(tax), "total": str(total)}}},
			"totals": map[string]any{
				"subtotal": str(sub), "discount": "0", "tax": str(tax), "total": str(total),
				"credit": "0", "credit_to_balance": "0", "balance": "0", "grand_total": str(total), "grand_total_tax": str(tax),
				"fee": nil, "earnings": nil, "currency_code": cur,
			},
			"payout_totals":          nil,
			"adjusted_payout_totals": nil,
			"line_items":             lines,
		},
		"payments":   t.paymentObjects(),
		"checkout":   nil,
		"created_at": stamp(t.created),
		"updated_at": stamp(t.updated),
		"billed_at":  stampOrNil(t.billed),
		"revised_at": nil,
	}
	if t.period != nil {
		o["billing_period"] = map[string]any{"starts_at": stamp(t.period[0]), "ends_at": stamp(t.period[1])}
	}
	if t.checkout != "" {
		o["checkout"] = map[string]any{"url": t.checkout}
	}
	if adjustments {
		var as []map[string]any
		ids := make([]string, 0, len(s.adjs))
		for id, a := range s.adjs {
			if a.txn == t.id && s.seen(a.hidden) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			as = append(as, s.adjs[id].object(s))
		}
		o["adjustments"] = as
	}
	return o
}

func (t *txn) paymentObjects() []map[string]any {
	ps := make([]map[string]any, 0, len(t.payments))
	// Newest first, as Paddle sorts them.
	for i := len(t.payments) - 1; i >= 0; i-- {
		p := t.payments[i]
		ps = append(ps, map[string]any{
			"payment_attempt_id":       p.id,
			"stored_payment_method_id": p.id,
			"payment_method_id":        nil,
			"amount":                   str(p.amount),
			"status":                   p.status,
			"error_code":               orNil(p.code),
			"method_details":           p.method.object(),
			"created_at":               stamp(p.created),
			"captured_at":              stampOrNil(p.captured),
		})
	}
	return ps
}

// object is m as Paddle's method_details: of a card, its brand, its last
// four digits and its expiry -- which package paddle does not keep -- and of
// a Korean card its company and its last four digits.
func (m Method) object() map[string]any {
	if m.Type == "" {
		m = Card
	}
	o := map[string]any{"type": m.Type, "card": nil, "south_korea_local_card": nil, "paypal": nil, "underlying_details": nil}
	switch m.Type {
	case "card":
		o["card"] = map[string]any{"type": m.Brand, "last4": m.Last4, "expiry_month": 12, "expiry_year": 2030, "cardholder_name": "Hong Gildong"}
	case "south_korea_local_card":
		o["south_korea_local_card"] = map[string]any{"type": m.Brand, "last4": m.Last4}
	}
	return o
}

func (it item) priceObject(cur string) map[string]any {
	var cycle any
	if it.recurring {
		cycle = map[string]any{"interval": "month", "frequency": 1}
	}
	typ := "custom"
	if it.recurring {
		typ = "standard"
	}
	return map[string]any{
		"id":                   it.price,
		"product_id":           it.product,
		"description":          it.description,
		"type":                 typ,
		"name":                 nil,
		"billing_cycle":        cycle,
		"trial_period":         nil,
		"tax_mode":             it.taxMode,
		"unit_price":           map[string]any{"amount": str(it.amount), "currency_code": cur},
		"unit_price_overrides": []any{},
		"quantity":             map[string]any{"minimum": 1, "maximum": 1},
		"status":               "active",
		"custom_data":          it.priceData,
		"import_meta":          nil,
	}
}

func (s *Server) lineObject(it item, cur string) map[string]any {
	sub, tax, total := s.totals(it)
	usub, utax, utotal := s.tax(it.amount, it.taxMode)
	return map[string]any{
		"price_id":    it.price,
		"quantity":    it.quantity,
		"proration":   nil,
		"tax_rate":    s.rate(),
		"unit_totals": map[string]any{"subtotal": str(usub), "discount": "0", "tax": str(utax), "total": str(utotal)},
		"totals":      map[string]any{"subtotal": str(sub), "discount": "0", "tax": str(tax), "total": str(total)},
		"product": map[string]any{
			"id":           it.product,
			"name":         it.name,
			"description":  nil,
			"type":         map[bool]string{true: "standard", false: "custom"}[it.recurring],
			"tax_category": it.category,
			"image_url":    nil,
			"custom_data":  it.productData,
			"status":       "active",
			"import_meta":  nil,
		},
		"currency_code": cur,
	}
}

func (s *Server) rate() string {
	return strconv.FormatFloat(float64(s.TaxRate)/10_000, 'f', -1, 64)
}

// object is su as Paddle's subscription entity
// (https://developer.paddle.com/api-reference/subscriptions/get-subscription),
// and with next its next_transaction: null for one that will not renew.
func (su *sub) object(s *Server, next bool) map[string]any {
	o := map[string]any{
		"id":                     su.id,
		"status":                 su.status,
		"customer_id":            su.customer,
		"address_id":             su.address,
		"business_id":            nil,
		"currency_code":          su.currency,
		"created_at":             stamp(su.created),
		"updated_at":             stamp(su.updated),
		"started_at":             stamp(su.created),
		"first_billed_at":        stamp(su.created),
		"next_billed_at":         stampOrNil(su.next),
		"paused_at":              nil,
		"canceled_at":            stampOrNil(su.canceledAt),
		"discount":               nil,
		"collection_mode":        "automatic",
		"billing_details":        nil,
		"current_billing_period": nil,
		"billing_cycle":          map[string]any{"interval": "month", "frequency": 1},
		"scheduled_change":       nil,
		"management_urls":        map[string]any{"update_payment_method": s.URL + "/portal/" + su.id + "/payment", "cancel": s.URL + "/portal/" + su.id + "/cancel"},
		"items":                  []map[string]any{{"status": "active", "quantity": su.plan.quantity, "recurring": true, "price": su.plan.priceObject(su.currency)}},
		"custom_data":            su.custom,
		"import_meta":            nil,
	}
	if !su.start.IsZero() {
		o["current_billing_period"] = map[string]any{"starts_at": stamp(su.start), "ends_at": stamp(su.end)}
	}
	if su.change != nil {
		o["scheduled_change"] = map[string]any{"action": su.change.action, "effective_at": stamp(su.change.at), "resume_at": nil}
	}
	if next {
		o["next_transaction"] = nil
		if (su.status == "active" || su.status == "trialing") && su.change == nil && !su.next.IsZero() {
			items := []item{su.plan}
			for _, it := range su.pending {
				if s.seen(it.hidden) {
					items = append(items, it)
				}
			}
			sub, tax, total := s.sum(items)
			lines := make([]map[string]any, len(items))
			for i, it := range items {
				lines[i] = s.lineObject(it, su.currency)
			}
			o["next_transaction"] = map[string]any{
				"billing_period": map[string]any{"starts_at": stamp(su.end), "ends_at": stamp(su.end.AddDate(0, 1, 0))},
				"details": map[string]any{
					"tax_rates_used": []map[string]any{{"tax_rate": s.rate(), "totals": map[string]any{"subtotal": str(sub), "discount": "0", "tax": str(tax), "total": str(total)}}},
					"totals": map[string]any{
						"subtotal": str(sub), "discount": "0", "tax": str(tax), "total": str(total),
						"credit": "0", "credit_to_balance": "0", "balance": str(total), "grand_total": str(total), "grand_total_tax": str(tax),
						"fee": nil, "earnings": nil, "currency_code": su.currency,
					},
					"line_items": lines,
				},
				"adjustments": []any{},
			}
		}
	}
	return o
}

// object is a as Paddle's adjustment entity
// (https://developer.paddle.com/api-reference/adjustments/create-adjustment).
func (a *adj) object(_ *Server) map[string]any {
	var sub, tax, total int64
	items := make([]map[string]any, len(a.items))
	for i, x := range a.items {
		sub, tax, total = sub+x.subtotal, tax+x.tax, total+x.total
		var amt any
		if x.amount != nil {
			amt = str(*x.amount)
		}
		items[i] = map[string]any{
			"id": x.id, "item_id": x.line, "type": x.typ, "amount": amt, "proration": nil,
			"totals": map[string]any{"subtotal": str(x.subtotal), "tax": str(x.tax), "total": str(x.total)},
		}
	}
	return map[string]any{
		"id":                        a.id,
		"action":                    "refund",
		"type":                      a.typ,
		"transaction_id":            a.txn,
		"subscription_id":           orNil(a.sub),
		"customer_id":               a.customer,
		"reason":                    a.reason,
		"credit_applied_to_balance": nil,
		"currency_code":             a.currency,
		"status":                    a.status,
		"items":                     items,
		"totals": map[string]any{
			"subtotal": str(sub), "tax": str(tax), "total": str(total), "fee": "0", "retained_fee": "0", "earnings": str(sub), "currency_code": a.currency,
		},
		"payout_totals":  nil,
		"tax_rates_used": nil,
		"created_at":     stamp(a.created),
		"updated_at":     stamp(a.updated),
	}
}

func str(n int64) string { return strconv.FormatInt(n, 10) }

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// stamp is a time as Paddle writes one: RFC 3339, in UTC, to the microsecond.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z07:00") }

func stampOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return stamp(t)
}

// id26 is 26 lowercase letters and digits: what follows an id's prefix.
func id26() string { return strings.ToLower(rand.Text()) }

// alnum is n letters and digits, made up.
func alnum(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(rand.Text())
	}
	return b.String()[:n]
}

func requestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
