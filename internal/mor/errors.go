package mor

import (
	"errors"
	"strconv"
	"strings"
)

// Kind is what a request to an MoR that failed is known to have done -- and,
// in a [Failure], why a transaction was not collected. They are package pg's
// kinds, by the same names and meaning, so that whatever keeps payments keeps
// both channels' alike. An MoR's own codes are many and its own; a channel
// maps each to one of these, and keeps the code beside it.
type Kind int

const (
	// Unknown is a request whether anything came of which is not known: a
	// timeout, a 5xx whatever it says, an answer that could not be read, a
	// look too soon to believe ([ErrTooSoon]). Find out before sending
	// anything again (package doc). It is the zero Kind, as it is the one a
	// caller that reads no further is safe with.
	Unknown Kind = iota

	// Declined is the card's fault: refused by its company, over its limit,
	// expired, gone, or a subscription with no card to charge. Nothing was
	// charged. A charge is answered with a [Failed] transaction rather than
	// this error; a checkout that can no longer be finished, with this
	// error.
	Declined

	// Invalid is a request that was wrong, or that the MoR will not do as
	// asked: more than is left to give back, a subscription that has ended.
	// Nothing happened, and sent again as it is it fails again.
	Invalid

	// Auth is the MoR refusing levyan: its key, its permissions, its account.
	// Nothing happened, and an operator has something to fix.
	Auth

	// RateLimited is too many requests, or an MoR that will not take this one
	// for a while: a renewal about to be billed, a refund still waiting for
	// approval. Nothing happened; later, it may.
	RateLimited

	// NotFound is the MoR having no such thing: an answer more than a
	// failure, since a key the MoR has nothing of was never charged.
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

// What a channel's [Error] can say beyond its kind, by [errors.Is]: why the
// MoR would not, in words that name no MoR.
var (
	// ErrNoNextPeriod is a charge for the next period of a subscription that
	// will not renew, which the MoR would drop: charge it [Now] (package
	// doc). [Invalid].
	ErrNoNextPeriod = errors.New("mor: the subscription has no next period to bill the charge with: charge it now")

	// ErrEnded is a subscription that is canceled: nothing charges it again.
	// [Invalid].
	ErrEnded = errors.New("mor: the subscription has ended")

	// ErrPastDue is a subscription with a payment the MoR has not collected:
	// nothing more is charged of it until that is paid. [Invalid].
	ErrPastDue = errors.New("mor: the subscription is past due")

	// ErrBelowMinimum is less than the MoR charges at all. [Invalid].
	ErrBelowMinimum = errors.New("mor: less than the MoR charges")

	// ErrOpen is a checkout the payer has not finished, or whose
	// subscription the MoR has not made yet. [NotFound].
	ErrOpen = errors.New("mor: the checkout has started no subscription yet")

	// ErrFinished is a checkout begun again ([Start.Again]) that the payer
	// has finished meanwhile: complete it, rather than begin another.
	// [Invalid].
	ErrFinished = errors.New("mor: the checkout was finished: complete it")

	// ErrTooSoon is a charge or a refund last sent less than [Settle] ago,
	// which a look cannot yet say was not made: nothing is sent. [Unknown].
	ErrTooSoon = errors.New("mor: sent too recently to say it was not made")

	// ErrTwice is a key the MoR has more than one charge of. [Invalid].
	ErrTwice = errors.New("mor: the key was charged more than once")
)

// Error is a request to an MoR that failed.
type Error struct {
	// Op is what was asked: "checkout", "subscription", "cancel", "charge",
	// "transaction", "refund", "webhook", "recheck".
	Op   string
	Kind Kind

	// Code and Message are the MoR's, when it answered with them, and Status
	// the HTTP status it answered with: 0 for no answer. Request is the
	// MoR's id of the request, which its support asks for.
	Code    string
	Message string
	Status  int
	Request string

	// Err is what failed beneath: a timeout, a reset connection, an answer
	// that could not be read, or one of this package's Err values.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("mor: ")
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
		b.WriteString(strings.TrimPrefix(e.Err.Error(), "mor: "))
	}
	if e.Request != "" {
		b.WriteString(" [request ")
		b.WriteString(e.Request)
		b.WriteString("]")
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
