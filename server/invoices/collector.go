package invoices

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/pg"
)

// Collector is how an invoice's money is moved on one channel: what the
// ledger asks of a channel once an invoice on it is finalized, in the
// ledger's words. There is one for each channel levyan collects on
// ([Invoices.Collectors]); a channel with none has its drafts kept, and
// never finalized, so that nothing is asked of a channel nothing can ask.
//
// It is the seam between the ledger, which is levyan's and the same on every
// channel, and a channel's adapter, which is the channel's. A merchant of
// record (`mor.Channel`) is one behind it -- a charge is `Charge` of the
// payer's subscription under the attempt's key with the send before it as
// `Sent`, an ask is `Charged`, a refund `Refund` and `Refunded` -- and a
// gateway (`pg.Channel`) is another: a charge is `Charge` of the billing
// key with the attempt's key as the order and the send's as the
// idempotency key, an ask is `Payment` of the order, a refund `Cancel`. A
// transfer is a third, which moves nothing itself ([Collector.Pushes]).
//
// What fails says so as an error of a [Kind]: the adapters' own `pg.Error`
// and `mor.Error` are read as they are ([KindOf]).
type Collector interface {
	// Minimum is the least the channel collects of one invoice in
	// `currency`, in its minor unit: 0 for no least. An invoice that would
	// ask for more than nothing and less than this waits for the next
	// period (the owner's, 2026-10-31).
	Minimum(currency string) int64

	// Pushes says whether levyan moves an invoice's money itself, by
	// sending attempts -- a card's, a subscription's -- or waits for the
	// payer to, as a transfer is waited for and recorded by an operator.
	Pushes() bool

	// Due is when an invoice finalized at `at` is due: zero for one
	// collected as it is finalized.
	Due(at time.Time) time.Time

	// Issue issues the legal document of an invoice, or of a credit note,
	// on a channel levyan sells on itself (`Seller`): a tax invoice, or the
	// amended one behind a credit note. Its number keeps it once -- one
	// issued again is answered with the document it already is -- and it
	// answers the document's reference. A merchant of record, the seller on
	// its own channel, is never asked.
	Issue(ctx context.Context, d Document) (Issued, error)

	// Send sends an attempt once: a charge of what an invoice has due, or a
	// refund of what a credit note gives back. The send has been written
	// down before it is made, and is told when the one before it was
	// ([Send.Previous]).
	Send(ctx context.Context, s Send) (Outcome, error)

	// Ask is what the channel has of an attempt now, by its key: an error of
	// [NotFound] when it has nothing of it -- the channel saying it was
	// never made, which it says only once it can (a merchant of record not
	// before `mor.Settle` has passed since the last send).
	Ask(ctx context.Context, a Attempt) (Outcome, error)
}

// Seller says whether levyan sells on `c` itself, so that its invoice is
// the legal one, numbered, with a tax invoice issued for it: every channel
// but a merchant of record, which is the seller on its own.
func Seller(c levyan.Channel) bool {
	switch c {
	case levyan.Channel_CHANNEL_MOR, levyan.Channel_CHANNEL_UNSPECIFIED:
		return false
	}

	return true
}

// Invoice is an invoice as a collector is told of it.
type Invoice struct {
	Id      pdid.Id
	Product pdid.Id

	// Account is its billing account, which a channel finds what to charge
	// by: a subscription, a billing key.
	Account pdid.Id

	Channel levyan.Channel

	// Start and End are the first moment it covers and the first after.
	Start time.Time
	End   time.Time

	// Description is what the payer sees it as: the product and its
	// periods, "loft 2027-01".
	Description string
}

// Attempt is an attempt as the ledger keeps it.
type Attempt struct {
	Id pdid.Id

	// Key is what keeps it once at the channel: a gateway's order, a
	// merchant of record's charge key.
	Key string

	Kind    levyan.AttemptKind
	Invoice Invoice
	Amount  money.Amount

	// Sends is how many sends of it are written down, and Sent when the
	// last was: zero for none.
	Sends int
	Sent  time.Time

	// For a refund: the channel's reference of the payment it gives back of,
	// and why, in words the payer may be shown.
	Refunds string
	Reason  string
}

// Send is one send of an attempt, written down before it is made.
type Send struct {
	Attempt

	// N is its number, from 1, and Key its own: `<attempt key>-<n>`, what a
	// gateway keeps one request by (an idempotency key).
	N   int
	Key string

	// Previous is when the send before it was written down: zero for the
	// first. A merchant of record holds a charge to it (`mor.Charge.Sent`).
	Previous time.Time
}

// SendKey is the key of the `n`th send of the attempt keyed `key`.
func SendKey(key string, n int) string { return key + "-" + strconv.Itoa(n) }

// Outcome is what a channel says came of an attempt.
type Outcome struct {
	// State is SUCCEEDED, FAILED, PENDING -- the channel has it and is not
	// done -- or REQUESTED: nothing came of it, and it is sent again.
	State levyan.AttemptState

	// Ref is the channel's id of the payment, the transaction or the refund,
	// and Number its document's: a merchant of record's invoice number.
	Ref    string
	Number string

	// At is when the money moved, by the channel's clock.
	At time.Time

	// Failure is why, for FAILED.
	Failure *levyan.AttemptFailure

	// Twice is the channel holding the attempt's key in more than one
	// payment -- a merchant of record's key charged twice (`mor.ErrTwice`) --
	// of which the outcome is of the one the attempt settles by: the attempt
	// is marked for reconciliation (#13), which gives the rest back.
	Twice bool
}

// Document is what a seller's legal document is made of: an invoice's tax
// invoice, or the amended one behind a credit note.
type Document struct {
	// Number is the invoice's or the credit note's, which keeps it once.
	Number int64

	// CreditNote is set for an amended one, with the reason and the
	// original's number and reference.
	CreditNote bool
	Reason     levyan.CreditNoteReason
	Amends     int64
	Document   string

	Invoice Invoice

	// Supply and Tax are the amounts apart, in the currency's minor unit,
	// and Rate what the tax is of the supply: less than nothing on a credit
	// note's.
	Currency string
	Supply   int64
	Tax      int64
	Rate     string

	// Written is the day it is written for: the last day an invoice
	// covers, in the product's zone, or the day a credit note is written --
	// and for a credit note, Original the day its invoice's was.
	Written  time.Time
	Original time.Time

	// BillTo is who it is to.
	BillTo *levyan.BillTo
}

// Issued is a legal document issued: when, and the channel's reference of
// it -- a tax invoice's approval number.
type Issued struct {
	At       time.Time
	Document string
}

// Kind is what a request to a channel that failed is known to have done: the
// kinds `pg.Kind` and `mor.Kind` are, by the same names and values.
type Kind int

const (
	// Unknown may have moved money: find out before anything is sent again.
	Unknown Kind = iota

	// Declined is the payer's method refusing it.
	Declined

	// Invalid is a request the channel will not do as asked; nothing
	// happened, and sent again as it is it fails again.
	Invalid

	// Auth is the channel refusing levyan; nothing happened.
	Auth

	// RateLimited is too many requests, or not now; nothing happened.
	RateLimited

	// NotFound is the channel having nothing of it.
	NotFound
)

func (k Kind) String() string {
	switch k {
	case Unknown:
		return "unknown"
	case Declined:
		return "declined"
	case Invalid:
		return "invalid"
	case Auth:
		return "auth"
	case RateLimited:
		return "rate limited"
	case NotFound:
		return "not found"
	default:
		return "kind " + strconv.Itoa(int(k))
	}
}

// Error is a request to a channel that failed, said by a collector that
// wraps no adapter's error of its own.
type Error struct {
	Op   string
	Kind Kind

	// Code and Message are the channel's.
	Code    string
	Message string

	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("invoices: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Kind.String())
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf is what is known of the request `err` says failed: its kind as a
// collector's [Error], a gateway's `pg.Error` or a merchant of record's
// `mor.Error` says it, and [Unknown] for any other -- nothing is known of
// what that one did.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	var p *pg.Error
	if errors.As(err, &p) {
		return Kind(p.Kind)
	}
	var m *mor.Error
	if errors.As(err, &m) {
		return Kind(m.Kind)
	}

	return Unknown
}

// codeOf is the channel's code and words of `err`, when it has them.
func codeOf(err error) (string, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Message
	}
	var p *pg.Error
	if errors.As(err, &p) {
		return p.Code, p.Message
	}
	var m *mor.Error
	if errors.As(err, &m) {
		return m.Code, m.Message
	}

	return "", ""
}

// FailureOf is a failure of `kind`, and of `decline` for one Declined, as a
// channel's adapter says them, value for value.
func FailureOf(kind Kind, decline pg.Decline, code, message string) *levyan.AttemptFailure {
	out := levyan.AttemptFailure_builder{
		Kind:    levyan.FailureKind(kind),
		Code:    code,
		Message: message,
	}.Build()
	if kind == Declined {
		out.SetDecline(levyan.DeclineKind(decline))
	}

	return out
}
