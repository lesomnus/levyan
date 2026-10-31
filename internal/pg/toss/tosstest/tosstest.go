// Package tosstest is a fake Toss Payments: an httptest server that speaks the
// part of Toss's API package toss calls, as Toss's documentation says Toss
// speaks it (package toss cites the pages), so that the adapter, and whatever
// is built on it, is tested offline.
//
// It is not Toss. Where the documentation does not say what Toss does, it
// does something said here, and the live test (toss's live_test.go) is what
// asks Toss itself:
//
//   - A declined charge is kept as an ABORTED payment of its order, with the
//     failure, which a query of the order answers.
//   - An idempotency key is kept for the life of the server, and a request
//     sent again under it is answered as the first was, whatever its body.
//   - A billing key is issued once for an authKey, which is then spent.
//
// A failure is asked for before the request it is for ([Server.Fail],
// [Server.Decline]); a test's own card is registered as the window would
// register it ([Server.Register]).
package tosstest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/internal/pg/toss"
)

// Op is a kind of request the fake answers.
type Op string

const (
	Issue   Op = "issue"   // POST /v1/billing/authorizations/issue
	Charge  Op = "charge"  // POST /v1/billing/{billingKey}
	Query   Op = "query"   // GET /v1/payments/orders/{orderId}, GET /v1/payments/{paymentKey}
	Cancel  Op = "cancel"  // POST /v1/payments/{paymentKey}/cancel
	Removal Op = "removal" // DELETE /v1/billing/{billingKey}
)

// Fault is what the next request of an [Op] meets instead of its answer.
type Fault struct {
	// Status and Code are the answer it gets: Toss's error object of them, or
	// no body for a Code of "".
	Status int
	Code   string

	// After does the work first, and keeps its own answer under the request's
	// idempotency key: an answer lost on its way back, which the same request
	// sent again is answered with. Without it nothing is done: the request
	// failed on its way to Toss.
	After bool

	// Kept is what Toss keeps under the key instead: its own failure, which
	// the same request sent again is answered with too.
	Kept bool

	// Hang does the work, keeps its answer, and answers nothing at all until
	// the client gives up.
	Hang bool
}

// Card is a card as the window registers it, and as Toss shows it
// (https://docs.tosspayments.com/reference#billing-객체).
type Card struct {
	Issuer string // issuerCode
	Number string // number, masked
	Type   string // cardType: 신용, 체크, 기프트
	Owner  string // ownerType: 개인, 법인
}

// TestCard is the card [Server.Register] registers.
var TestCard = Card{Issuer: "61", Number: "43301234****123*", Type: "신용", Owner: "개인"}

// Server is the fake.
type Server struct {
	// URL is its API, which a [toss.Config] names.
	URL string

	// SecretKey and ClientKey are the keys it takes, and WebhookKey the 보안
	// 키 it signs its webhooks with.
	SecretKey  string
	ClientKey  string
	WebhookKey string

	srv  *httptest.Server
	done chan struct{}

	mu       sync.Mutex
	auths    map[string]auth
	keys     map[string]billing
	payments map[string]*payment
	orders   map[string]string // orderId: the paymentKey of its latest payment
	kept     map[string]*answer
	faults   map[Op][]Fault
	declines []string
	requests map[Op]int
	approved int
}

type auth struct {
	customer string
	card     Card
	spent    bool
}

type billing struct {
	customer string
	card     Card
	at       time.Time
}

type payment struct {
	key      string
	order    string
	name     string
	status   string
	total    int64
	balance  int64
	at       time.Time
	last     string
	card     Card
	cancels  []cancel
	failCode string
	failMsg  string
}

type cancel struct {
	amount int64
	reason string
	at     time.Time
	tx     string
	left   int64
}

// answer is an answer as it is kept under an idempotency key.
type answer struct {
	status int
	body   []byte
}

// New starts a fake for t, which closes it when t ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		SecretKey:  "test_sk_" + rand.Text(),
		ClientKey:  "test_ck_" + rand.Text(),
		WebhookKey: "fake-webhook-key-" + rand.Text(),
		done:       make(chan struct{}),
		auths:      map[string]auth{},
		keys:       map[string]billing{},
		payments:   map[string]*payment{},
		orders:     map[string]string{},
		kept:       map[string]*answer{},
		faults:     map[Op][]Fault{},
		requests:   map[Op]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/billing/authorizations/issue", s.handle(Issue, s.issue))
	mux.HandleFunc("POST /v1/billing/{billingKey}", s.handle(Charge, s.charge))
	mux.HandleFunc("DELETE /v1/billing/{billingKey}", s.handle(Removal, s.remove))
	mux.HandleFunc("GET /v1/payments/orders/{orderId}", s.handle(Query, s.byOrder))
	mux.HandleFunc("GET /v1/payments/{paymentKey}", s.handle(Query, s.byKey))
	mux.HandleFunc("POST /v1/payments/{paymentKey}/cancel", s.handle(Cancel, s.cancel))

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

// Config is a [toss.Config] that calls the fake with its keys.
func (s *Server) Config() toss.Config {
	return toss.Config{
		SecretKey:  Secret(s.SecretKey),
		ClientKey:  s.ClientKey,
		WebhookKey: Secret(s.WebhookKey),
		URL:        s.URL,
	}
}

// Fail makes the next request of op meet f.
func (s *Server) Fail(op Op, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[op] = append(s.faults[op], f)
}

// Decline makes the next charge declined with code, as Toss answers it: the
// error object, and an ABORTED payment of the order that says why.
func (s *Server) Decline(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declines = append(s.declines, code)
}

// Approved is how many charges the fake has approved: how many times a card
// was charged.
func (s *Server) Approved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approved
}

// Requests is how many requests of op came, faults and all.
func (s *Server) Requests(op Op) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[op]
}

// Register is the payer in Toss's window, registering [TestCard] for r: what
// the window sends the payer back to successUrl with.
func (s *Server) Register(r pg.Registration) url.Values { return s.RegisterCard(r, TestCard) }

// RegisterCard is [Server.Register] of the card c. A window opened with
// another client key fails, as Toss's does, back to failUrl.
func (s *Server) RegisterCard(r pg.Registration, c Card) url.Values {
	if r.Window.Values["clientKey"] != s.ClientKey || r.Window.Values["customerKey"] != r.Customer {
		return url.Values{"code": {"INVALID_CLIENT_KEY"}, "message": {"잘못된 클라이언트 연동 정보 입니다."}}
	}
	key := "fake-auth-" + rand.Text()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths[key] = auth{customer: r.Customer, card: c}
	return url.Values{"customerKey": {r.Customer}, "authKey": {key}}
}

// Webhook is what Toss sends when the payment of key changes:
// PAYMENT_STATUS_CHANGED, with the payment as it is now, signed with
// WebhookKey.
func (s *Server) Webhook(key string) (http.Header, []byte) {
	s.mu.Lock()
	p, ok := s.payments[key]
	var data map[string]any
	if ok {
		data = p.object()
	}
	s.mu.Unlock()
	if !ok {
		panic(fmt.Sprintf("tosstest: no payment %q", key))
	}
	body, err := json.Marshal(map[string]any{
		"eventType": "PAYMENT_STATUS_CHANGED",
		"createdAt": time.Now().In(kst).Format("2006-01-02T15:04:05.000000"),
		"data":      data,
	})
	if err != nil {
		panic(err)
	}
	return s.Sign(body), body
}

// Sign is the headers Toss sends body with: when, how many times it was sent
// again, the delivery's id, and the signature of it under WebhookKey
// (https://docs.tosspayments.com/reference/using-api/webhook-events#웹훅-헤더).
func (s *Server) Sign(body []byte) http.Header {
	at := time.Now().In(kst).Format(time.RFC3339)
	m := hmac.New(sha256.New, []byte(s.WebhookKey))
	m.Write(body)
	m.Write([]byte(":" + at))
	h := http.Header{}
	h.Set(toss.TimeHeader, at)
	h.Set("tosspayments-webhook-transmission-retried-count", "0")
	h.Set("tosspayments-webhook-transmission-id", "whtrans_"+strings.ToLower(rand.Text()))
	h.Set(toss.SignatureHeader, "v1:"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	return h
}

var kst = time.FixedZone("KST", 9*60*60)

// handle is a request of op: its secret key checked, then a fault on its way
// to Toss met, then an answer kept under its idempotency key given again --
// and only then f, and whatever fault is left.
func (s *Server) handle(op Op, f func(r *http.Request, body []byte) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		s.mu.Lock()
		s.requests[op]++
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(s.SecretKey+":")) {
			s.mu.Unlock()
			refuse(w, http.StatusUnauthorized, "UNAUTHORIZED_KEY", "인증되지 않은 시크릿 키 혹은 클라이언트 키 입니다.")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 300 {
			s.mu.Unlock()
			refuse(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "멱등키는 300자 이하여야 합니다.")
			return
		}
		if fs := s.faults[op]; len(fs) > 0 && fs[0].plain() {
			fault := fs[0]
			s.faults[op] = fs[1:]
			s.mu.Unlock()
			refuse(w, fault.Status, fault.Code, "")
			return
		}

		// A GET is idempotent by itself, and its key is ignored.
		kept := ""
		if key != "" && r.Method == http.MethodPost {
			kept = r.Method + " " + r.URL.EscapedPath() + " " + key
			if a, ok := s.kept[kept]; ok {
				s.mu.Unlock()
				write(w, a.status, a.body)
				return
			}
		}

		var fault *Fault
		if fs := s.faults[op]; len(fs) > 0 {
			fault, s.faults[op] = &fs[0], fs[1:]
		}
		var a answer
		if fault == nil || fault.After || fault.Hang {
			status, v := f(r, body)
			a = answer{status: status}
			if v != nil {
				b, err := json.Marshal(v)
				if err != nil {
					panic(err)
				}
				a.body = b
			}
		}
		if fault != nil && fault.Kept {
			a = answer{fault.Status, errorBody(fault.Code, "")}
		}
		if kept != "" {
			s.kept[kept] = &a
		}
		s.mu.Unlock()

		switch {
		case fault == nil:
			write(w, a.status, a.body)
		case fault.Hang:
			select {
			case <-r.Context().Done():
			case <-s.done:
			}
		default:
			refuse(w, fault.Status, fault.Code, "")
		}
	}
}

// plain is a fault met on the way to Toss: nothing done, nothing kept.
func (f Fault) plain() bool { return !f.After && !f.Kept && !f.Hang }

func write(w http.ResponseWriter, status int, body []byte) {
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	write(w, status, errorBody(code, message))
}

func errorBody(code, message string) []byte {
	if code == "" {
		return nil
	}
	if message == "" {
		message = code
	}
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return b
}

type refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func no(code, message string) any { return refusal{code, message} }

// The status Toss answers each code it declines with
// (https://docs.tosspayments.com/reference/error-codes#카드-자동결제-승인).
var declineStatus = map[string]int{
	"REJECT_CARD_PAYMENT":    http.StatusForbidden,
	"REJECT_CARD_COMPANY":    http.StatusForbidden,
	"REJECT_ACCOUNT_PAYMENT": http.StatusForbidden,
	"EXCEED_MAX_AUTH_COUNT":  http.StatusForbidden,
	"KYC_BLOCKED":            http.StatusForbidden,
}

// The functions below are called with s.mu held.

func (s *Server) issue(_ *http.Request, body []byte) (int, any) {
	var in struct {
		AuthKey     string `json:"authKey"`
		CustomerKey string `json:"customerKey"`
	}
	if json.Unmarshal(body, &in) != nil || in.AuthKey == "" || in.CustomerKey == "" {
		return http.StatusBadRequest, no("INVALID_REQUEST", "잘못된 요청입니다.")
	}
	a, ok := s.auths[in.AuthKey]
	if !ok || a.spent || a.customer != in.CustomerKey {
		return http.StatusBadRequest, no("INVALID_BILLING_AUTH", "유효하지 않은 빌링 인증입니다.")
	}
	a.spent = true
	s.auths[in.AuthKey] = a

	key := "fake-billing-" + rand.Text() + "="
	b := billing{customer: a.customer, card: a.card, at: now()}
	s.keys[key] = b
	return http.StatusOK, map[string]any{
		"mId":             "tosspayments",
		"customerKey":     b.customer,
		"authenticatedAt": b.at.Format(time.RFC3339),
		"method":          "카드",
		"billingKey":      key,
		"card": map[string]any{
			"issuerCode":   b.card.Issuer,
			"acquirerCode": b.card.Issuer,
			"number":       b.card.Number,
			"cardType":     b.card.Type,
			"ownerType":    b.card.Owner,
		},
	}
}

func (s *Server) charge(r *http.Request, body []byte) (int, any) {
	b, ok := s.keys[r.PathValue("billingKey")]
	if !ok {
		return http.StatusNotFound, no("NOT_FOUND_BILLING", "존재하지 않는 빌링 결제 인증 정보 입니다.")
	}
	var in struct {
		CustomerKey string `json:"customerKey"`
		Amount      *int64 `json:"amount"`
		OrderID     string `json:"orderId"`
		OrderName   string `json:"orderName"`
	}
	if json.Unmarshal(body, &in) != nil || in.Amount == nil || in.OrderID == "" || in.OrderName == "" || in.CustomerKey == "" {
		return http.StatusBadRequest, no("INVALID_REQUIRED_PARAM", "필수 파라미터가 누락되었습니다.")
	}
	switch {
	case in.CustomerKey != b.customer:
		return http.StatusBadRequest, no("NOT_MATCHES_CUSTOMER_KEY", "빌링 인증 고객키와 결제 요청 고객키가 일치하지 않습니다.")
	case !orderOK(in.OrderID):
		return http.StatusBadRequest, no("INVALID_ORDER_ID", "orderId는 영문 대소문자, 숫자, 특수문자(-, _, =)만 허용합니다. 6자 이상 64자 이하여야 합니다.")
	case len([]rune(in.OrderName)) > 100:
		return http.StatusBadRequest, no("INVALID_ORDER_NAME", "주문 이름은 100자 이하여야 합니다.")
	case *in.Amount <= 0:
		return http.StatusBadRequest, no("BELOW_ZERO_AMOUNT", "금액은 0보다 커야 합니다.")
	case *in.Amount < 100:
		return http.StatusBadRequest, no("BELOW_MINIMUM_AMOUNT", "신용카드는 결제금액이 100원 이상, 계좌는 200원이상부터 결제가 가능합니다.")
	}
	if k, ok := s.orders[in.OrderID]; ok && s.payments[k].status != "ABORTED" {
		return http.StatusBadRequest, no("DUPLICATED_ORDER_ID", "이미 승인 및 취소가 진행된 중복된 주문번호 입니다. 다른 주문번호로 진행해주세요.")
	}

	p := &payment{
		key:   "fake-payment-" + rand.Text(),
		order: in.OrderID,
		name:  in.OrderName,
		total: *in.Amount,
		last:  tx(),
		card:  b.card,
	}
	s.payments[p.key] = p
	s.orders[p.order] = p.key

	if len(s.declines) > 0 {
		code := s.declines[0]
		s.declines = s.declines[1:]
		p.status, p.balance, p.failCode, p.failMsg = "ABORTED", p.total, code, "카드 사용이 거절되었습니다."
		status, ok := declineStatus[code]
		if !ok {
			status = http.StatusBadRequest
		}
		return status, no(code, p.failMsg)
	}

	p.status, p.balance, p.at = "DONE", p.total, now()
	s.approved++
	return http.StatusOK, p.object()
}

func (s *Server) remove(r *http.Request, _ []byte) (int, any) {
	key := r.PathValue("billingKey")
	if _, ok := s.keys[key]; !ok {
		return http.StatusNotFound, no("NOT_FOUND_BILLING", "존재하지 않는 빌링 결제 인증 정보 입니다.")
	}
	delete(s.keys, key)
	return http.StatusOK, nil
}

func (s *Server) byOrder(r *http.Request, _ []byte) (int, any) {
	k, ok := s.orders[r.PathValue("orderId")]
	if !ok {
		return http.StatusNotFound, no("NOT_FOUND_PAYMENT", "존재하지 않는 결제 정보 입니다.")
	}
	return http.StatusOK, s.payments[k].object()
}

func (s *Server) byKey(r *http.Request, _ []byte) (int, any) {
	p, ok := s.payments[r.PathValue("paymentKey")]
	if !ok {
		return http.StatusNotFound, no("NOT_FOUND_PAYMENT", "존재하지 않는 결제 정보 입니다.")
	}
	return http.StatusOK, p.object()
}

func (s *Server) cancel(r *http.Request, body []byte) (int, any) {
	p, ok := s.payments[r.PathValue("paymentKey")]
	if !ok {
		return http.StatusNotFound, no("NOT_FOUND_PAYMENT", "존재하지 않는 결제 정보 입니다.")
	}
	var in struct {
		CancelReason string `json:"cancelReason"`
		CancelAmount *int64 `json:"cancelAmount"`
	}
	if json.Unmarshal(body, &in) != nil || in.CancelReason == "" {
		return http.StatusBadRequest, no("INVALID_REQUEST", "잘못된 요청입니다.")
	}
	switch p.status {
	case "CANCELED":
		return http.StatusBadRequest, no("ALREADY_CANCELED_PAYMENT", "이미 취소된 결제 입니다.")
	case "DONE", "PARTIAL_CANCELED":
	default:
		return http.StatusForbidden, no("NOT_CANCELABLE_PAYMENT", "취소 할 수 없는 결제 입니다.")
	}
	amount := p.balance
	if in.CancelAmount != nil {
		amount = *in.CancelAmount
	}
	if amount <= 0 || amount > p.balance {
		return http.StatusForbidden, no("NOT_CANCELABLE_AMOUNT", "취소 할 수 없는 금액 입니다.")
	}

	p.balance -= amount
	p.last = tx()
	p.cancels = append(p.cancels, cancel{amount: amount, reason: in.CancelReason, at: now(), tx: p.last, left: p.balance})
	p.status = "PARTIAL_CANCELED"
	if p.balance == 0 {
		p.status = "CANCELED"
	}
	return http.StatusOK, p.object()
}

// object is p as Toss's Payment object, of version 2024-06-01
// (https://docs.tosspayments.com/reference#payment-객체).
func (p *payment) object() map[string]any {
	o := map[string]any{
		"mId":                 "tosspayments",
		"version":             "2024-06-01",
		"paymentKey":          p.key,
		"type":                "BILLING",
		"orderId":             p.order,
		"orderName":           p.name,
		"currency":            "KRW",
		"method":              "카드",
		"status":              p.status,
		"totalAmount":         p.total,
		"balanceAmount":       p.balance,
		"lastTransactionKey":  p.last,
		"requestedAt":         now().Format(time.RFC3339),
		"approvedAt":          nil,
		"isPartialCancelable": true,
		"card": map[string]any{
			"issuerCode": p.card.Issuer,
			"number":     p.card.Number,
			"cardType":   p.card.Type,
			"ownerType":  p.card.Owner,
			"amount":     p.total,
		},
		"cancels": nil,
		"failure": nil,
		"secret":  nil,
	}
	if !p.at.IsZero() {
		o["approvedAt"] = p.at.Format(time.RFC3339)
	}
	if len(p.cancels) > 0 {
		cs := make([]map[string]any, len(p.cancels))
		for i, c := range p.cancels {
			cs[i] = map[string]any{
				"cancelAmount":     c.amount,
				"cancelReason":     c.reason,
				"taxFreeAmount":    0,
				"refundableAmount": c.left,
				"canceledAt":       c.at.Format(time.RFC3339),
				"transactionKey":   c.tx,
				"cancelStatus":     "DONE",
				"cancelRequestId":  nil,
			}
		}
		o["cancels"] = cs
	}
	if p.failCode != "" {
		o["failure"] = map[string]any{"code": p.failCode, "message": p.failMsg}
	}
	return o
}

func now() time.Time { return time.Now().In(kst).Truncate(time.Second) }

func tx() string { return rand.Text() }

func orderOK(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_' || r == '=')
	})
}
