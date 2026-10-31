// Package invoicestest is a channel to collect invoices on in a test: an
// [invoices.Collector] that keeps what it was sent by each attempt's key, as
// a gateway keeps an order and a merchant of record a charge, and answers as
// the test tells it to -- paid, declined, an answer lost after the money
// moved, a send lost before it arrived, still pending, refused -- and
// declined as each kind of decline there is, which dunning (#11) follows.
//
// It is what the ledger's tests stand on in place of a channel's adapter, as
// `tosstest` and `paddletest` stand in for a PG's and an MoR's own APIs.
package invoicestest

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/server/invoices"
)

// Answer is how the channel answers a send.
type Answer int

const (
	// Pay moves the money, and says so.
	Pay Answer = iota

	// Decline is the payer's method refusing it, temporarily -- too little
	// in the account: a result.
	Decline

	// Lost moves the money, and the answer never comes: an error of
	// Unknown. Asked, the channel says it was paid.
	Lost

	// Dropped never arrives, and the answer never comes either: an error of
	// Unknown. Asked, the channel has nothing of it.
	Dropped

	// Hold takes it and does not finish: PENDING, until [Channel.Finish].
	Hold

	// Refuse will not do it as asked: an error of Invalid.
	Refuse

	// Busy does nothing for now: an error of RateLimited.
	Busy

	// DeclineReplace is the payer's method refusing it as one that will not
	// be charged as it is: expired.
	DeclineReplace

	// DeclineFraud is a charge refused as suspected fraud.
	DeclineFraud

	// DeclineOther is a refusal that says no more than that it is one.
	DeclineOther
)

// Channel is a channel in a test.
type Channel struct {
	// Push is whether levyan moves its money: false is a transfer's channel,
	// on which an operator records what came.
	Push bool

	// Least is the least it collects of an invoice, by currency.
	Least map[string]int64

	// After is how long after it is finalized an invoice is due.
	After time.Duration

	// Before is called with each send as it arrives, before it is answered:
	// what a test asks the ledger then, as a crash would find it.
	Before func(invoices.Send)

	mu     sync.Mutex
	script []Answer
	made   map[string]*made
	sends  []invoices.Send
	asks   []invoices.Attempt
	docs   map[string]invoices.Document
	issues int
	next   int
}

type made struct {
	kind    levyan.AttemptKind
	state   levyan.AttemptState
	decline pg.Decline
	ref     string
	amount  int64
	at      time.Time
}

// New is a channel that pushes: each send is paid unless [Channel.Then]
// says otherwise.
func New() *Channel { return &Channel{Push: true} }

var _ invoices.Collector = (*Channel)(nil)

// Then has the next sends answered so, in order; once they are used up,
// sends are paid.
func (c *Channel) Then(as ...Answer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.script = append(c.script, as...)
}

// Finish ends what was held of the attempt keyed `key`: paid, or not --
// declined, temporarily.
func (c *Channel) Finish(key string, paid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.made[key]
	if !ok {
		return
	}
	if paid {
		m.state = levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED
	} else {
		m.state, m.decline = levyan.AttemptState_ATTEMPT_STATE_FAILED, pg.DeclineTemporary
	}
}

// Sends is every send the channel was given, in order.
func (c *Channel) Sends() []invoices.Send {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]invoices.Send(nil), c.sends...)
}

// Asks is every attempt it was asked about, in order.
func (c *Channel) Asks() []invoices.Attempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]invoices.Attempt(nil), c.asks...)
}

// Made is how many attempts of `kind` the channel has made -- moved money
// for -- and how much in all.
func (c *Channel) Made(kind levyan.AttemptKind) (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, sum := 0, int64(0)
	for _, m := range c.made {
		if m.kind == kind && m.state == levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED {
			n++
			sum += m.amount
		}
	}
	return n, sum
}

// Documents is every legal document the channel issued, by its key: an
// invoice's number, or a credit note's after "cn-".
func (c *Channel) Documents() map[string]invoices.Document {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]invoices.Document{}
	for k, v := range c.docs {
		out[k] = v
	}
	return out
}

// Issues is how many times a document was asked for, the same one again
// included.
func (c *Channel) Issues() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues
}

func (c *Channel) Minimum(currency string) int64 { return c.Least[currency] }

func (c *Channel) Pushes() bool { return c.Push }

func (c *Channel) Due(at time.Time) time.Time {
	if c.After <= 0 {
		return time.Time{}
	}
	return at.Add(c.After)
}

// Issue issues a document once by its key: asked again, it answers the one
// it issued.
func (c *Channel) Issue(_ context.Context, d invoices.Document) (invoices.Issued, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues++
	key := strconv.FormatInt(d.Number, 10)
	if d.CreditNote {
		key = "cn-" + key
	}
	if c.docs == nil {
		c.docs = map[string]invoices.Document{}
	}
	if _, ok := c.docs[key]; !ok {
		c.docs[key] = d
	}

	return invoices.Issued{Document: "doc-" + key}, nil
}

func (c *Channel) Send(_ context.Context, s invoices.Send) (invoices.Outcome, error) {
	if c.Before != nil {
		c.Before(s)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, s)
	if c.made == nil {
		c.made = map[string]*made{}
	}
	if m, ok := c.made[s.Attempt.Key]; ok {
		// What it has of the key, whatever the send says: the channel keeps
		// an attempt once by it, as a gateway's order is.
		return outcome(m), nil
	}

	a := Pay
	if len(c.script) > 0 {
		a, c.script = c.script[0], c.script[1:]
	}
	c.next++
	ref := fmt.Sprintf("ref-%d", c.next)
	now := time.Now()
	switch a {
	case Pay:
		c.made[s.Attempt.Key] = &made{kind: s.Attempt.Kind, state: levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, ref: ref, amount: s.Attempt.Amount.Minor, at: now}
		return outcome(c.made[s.Attempt.Key]), nil
	case Decline, DeclineReplace, DeclineFraud, DeclineOther:
		m := &made{kind: s.Attempt.Kind, state: levyan.AttemptState_ATTEMPT_STATE_FAILED, decline: declines[a], ref: ref, amount: s.Attempt.Amount.Minor}
		c.made[s.Attempt.Key] = m
		return outcome(m), nil
	case Lost:
		c.made[s.Attempt.Key] = &made{kind: s.Attempt.Kind, state: levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, ref: ref, amount: s.Attempt.Amount.Minor, at: now}
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Unknown, Message: "the answer never came"}
	case Dropped:
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Unknown, Message: "the send never came"}
	case Hold:
		m := &made{kind: s.Attempt.Kind, state: levyan.AttemptState_ATTEMPT_STATE_PENDING, ref: ref, amount: s.Attempt.Amount.Minor, at: now}
		c.made[s.Attempt.Key] = m
		return outcome(m), nil
	case Refuse:
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Code: "REFUSED", Message: "not as asked"}
	case Busy:
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.RateLimited, Code: "BUSY", Message: "later"}
	}

	return invoices.Outcome{}, fmt.Errorf("invoicestest: answer %d", a)
}

func (c *Channel) Ask(_ context.Context, a invoices.Attempt) (invoices.Outcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks = append(c.asks, a)
	m, ok := c.made[a.Key]
	if !ok {
		return invoices.Outcome{}, &invoices.Error{Op: "ask", Kind: invoices.NotFound, Message: "no such attempt"}
	}

	return outcome(m), nil
}

// declines are the kinds each answer that declines declines as.
var declines = map[Answer]pg.Decline{
	Decline:        pg.DeclineTemporary,
	DeclineReplace: pg.DeclineReplace,
	DeclineFraud:   pg.DeclineFraud,
	DeclineOther:   pg.DeclineOther,
}

// codes are a gateway's code and words of each kind, as Toss says them.
var codes = map[pg.Decline][2]string{
	pg.DeclineTemporary: {"REJECT_CARD_PAYMENT", "잔액이 부족합니다"},
	pg.DeclineReplace:   {"INVALID_CARD_EXPIRATION", "카드 정보를 다시 확인해주세요 (유효기간)"},
	pg.DeclineFraud:     {"FDS_ERROR", "위험거래가 감지되어 결제가 제한됩니다"},
	pg.DeclineOther:     {"REJECT_CARD_COMPANY", "결제 승인이 거절되었습니다"},
}

func outcome(m *made) invoices.Outcome {
	o := invoices.Outcome{State: m.state, Ref: m.ref, At: m.at}
	switch m.state {
	case levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED:
		o.Number = "MOR-" + m.ref
	case levyan.AttemptState_ATTEMPT_STATE_FAILED:
		said := codes[m.decline]
		o.Failure = invoices.FailureOf(invoices.Declined, m.decline, said[0], said[1])
	}
	return o
}
