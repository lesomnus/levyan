package pg

import (
	"errors"
	"strconv"
	"strings"
)

// Kind is what a request to a PG that failed is known to have done -- and, in
// a [Failure], why a payment failed. A PG's own codes are many and its own; a
// channel maps each to one of these, and keeps the code beside it.
type Kind int

const (
	// Unknown is a request whether anything came of which is not known: a
	// timeout, a 5xx whatever it says, an answer that could not be read, a
	// request the PG says it is still working on. Find out before sending
	// anything again (package doc). It is the zero Kind, as it is the one a
	// caller that reads no further is safe with.
	Unknown Kind = iota

	// Declined is the card's fault: refused by its company, over its limit,
	// stopped, lost, expired, or a billing key the PG no longer has. Nothing
	// was charged. A charge is answered with a [Failed] payment rather than
	// this error, whose [Failure] says which kind of decline it is
	// ([Decline]); a registration, with this error.
	Declined

	// Invalid is a request that was wrong, or that the PG will not do as
	// asked: more than is left to give back, a payment past its refund
	// window. Nothing happened, and sent again as it is it fails again.
	Invalid

	// Auth is the PG refusing levyan: its keys, the address it calls from, or
	// a contract it does not have. Nothing happened, and an operator has
	// something to fix.
	Auth

	// RateLimited is too many requests, or a PG that cannot take any for a
	// while. Nothing happened; later, it may.
	RateLimited

	// NotFound is the PG having no such payment: an answer more than a
	// failure, since an order the PG has nothing of was never charged.
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

// Error is a request to a PG that failed.
type Error struct {
	// Op is what was asked: "registration", "charge", "payment", "cancel",
	// "removal", "webhook", "recheck".
	Op   string
	Kind Kind

	// Code and Message are the PG's, when it answered with them, and Status
	// the HTTP status it answered with: 0 for no answer.
	Code    string
	Message string
	Status  int

	// Err is what failed beneath: a timeout, a reset connection, an answer
	// that could not be read. Never one that names a URL, since a charge's
	// names the billing key.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("pg: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Kind.String())
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
	}
	if e.Status != 0 {
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(e.Status))
		b.WriteString(")")
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

// KindOf is what is known of the request err says failed: its [Error]'s
// kind, and [Unknown] for an error that is no [Error] -- nothing is known of
// what that one did. It is asked of an error, never of nil.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Unknown
}
