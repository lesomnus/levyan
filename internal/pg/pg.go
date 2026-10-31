// Package pg is what levyan asks of a payment gateway it sells through: a
// channel where levyan is the seller -- in Korea a PG (전자지급결제대행), and
// later a region's own -- unlike a merchant of record, which sells for levyan
// and is another kind of channel (#12).
//
// It is the boundary, and nothing behind it. Which PG levyan sells through is
// the owner's decision and has not been made (levyan#10); package toss is one
// candidate's adapter, another PG is another package implementing [Channel],
// and nothing that uses a channel names one. It knows nothing of billing
// accounts (#4), invoices (#9) or dunning (#11): they are what calls it.
//
// # What it promises
//
//   - Card data never passes through levyan. A payer registers a card in the
//     PG's own window, which [Channel.StartRegistration] says how to open;
//     levyan gets a one-time authorization back from it, and exchanges that
//     for a billing key ([Channel.CompleteRegistration]).
//   - A billing key is a secret ([BillingKey]). It prints, logs and marshals
//     as nothing, and [BillingKey.Reveal] is the one way to its value -- for
//     the request that sends it and the store that encrypts it.
//   - The request failing is not the payment failing. A charge the card
//     declined is a [Payment] whose status is [Failed], and no error. An error
//     is a request that failed, and its [Kind] says what is known: [Invalid],
//     [Auth] and [RateLimited] that nothing happened, [Unknown] that something
//     may have.
//   - An unknown outcome is asked about before anything is sent again.
//   - A webhook is a hint. What it says is not believed: the payment it names
//     is asked of the PG ([Channel.Recheck]), and the answer is.
//
// # An unknown outcome
//
// A timeout, a 5xx, an answer that could not be read, a request the PG says
// it is still working on: whether the money moved is not known, so whatever
// is done next starts by finding out.
//
//   - A charge. Ask the order's payment ([Channel.Payment]). [NotFound] is the
//     PG saying the order was never charged, and only then is it charged
//     again: the same order, under a new idempotency key -- the PG answers a
//     key with its first answer, an error included, for as long as it keeps
//     the key. The order is what makes that safe: the PG charges an order
//     once, and a channel answers a charge of an order the PG has already
//     approved with the payment it is.
//   - A cancellation. The same cancellation again under the same key is
//     answered with what the PG did with it the first time. If that is no
//     answer either, ask the payment: what is left of it ([Payment.Balance])
//     says whether the cancellation happened, and only once it did not is it
//     sent under a new key. A cancellation has no order to keep it once, so
//     its key is all that does.
//
// # How a billing key is stored
//
// The store is #4's and #10's wiring; this is what it has to be.
//
//   - Encrypted at rest, by levyan, before it reaches the database: sealed
//     with an AEAD under a key the deployment holds as a secret apart from the
//     database (a cfg.Secret, `${file:/run/…}`), with the row it belongs to as
//     the associated data, so that a copy of the database charges nobody and a
//     sealed key moved to another row opens for nobody. The sealing key is
//     named in what it sealed, so that it can be rotated.
//   - Beside it, its [BillingKey.Digest], which finds the method a webhook
//     names without opening every row.
//   - Never in a log line, a span, an error, the access record, or the trail
//     as a value. A charge names the key in its URL, so the http.Client a
//     channel is handed must not record URLs (no HTTP instrumentation that
//     keeps url.full or url.path), and a channel's errors never carry one.
//   - Removed at the PG ([Channel.RemoveMethod]) when the payer removes the
//     method, and then from the store.
package pg

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/lesomnus/levyan/internal/money"
)

// Channel is a PG levyan sells through.
//
// Every method that asks the PG takes a context, which is the most it waits:
// a charge may take a minute at the PG, so a context shorter than that makes
// outcomes unknown that would not have been. Charge and Cancel take an
// idempotency key, under which the PG answers a request sent again as it
// answered it the first time.
type Channel interface {
	// Name is the PG's, as a stored method says which channel charges it:
	// "toss".
	Name() string

	// StartRegistration begins registering a payment method: what a page
	// opens the PG's window with, in which the payer gives the card to the PG
	// and not to levyan. The caller keeps the [Registration] until the payer
	// is sent back.
	StartRegistration(ctx context.Context, s Start) (Registration, error)

	// CompleteRegistration exchanges what the window sent the payer back with
	// -- the query of the URL it came back to, [Start.Success] or [Start.Fail]
	// -- for the method it registered. A card the PG refused is [Declined]; a
	// window the payer left is too.
	//
	// Completing one registration again, as a payer reloading the page does,
	// is the same method: the one-time authorization is exchanged under a key
	// of its own, so the PG answers the second exchange with the first.
	CompleteRegistration(ctx context.Context, r Registration, back url.Values) (Method, error)

	// Charge charges a registered method. A charge the PG answered is a
	// [Payment]: [Paid], or [Failed] with why. An error is a request that
	// failed, and an [Unknown] one may have charged the card (package doc).
	//
	// An order the PG has already approved is answered with the payment it
	// is, whatever key it is sent under, and an order charged for another
	// amount is [Invalid].
	Charge(ctx context.Context, c Charge) (Payment, error)

	// Payment is the payment of an order, as the PG has it now: [NotFound]
	// when it has none, which is the PG saying the order was never charged.
	Payment(ctx context.Context, order string) (Payment, error)

	// Cancel gives back some or all of a paid payment, and answers the
	// payment as it is after it. Asked to cancel all of what is left of a
	// payment that has nothing left, it answers the payment canceled.
	Cancel(ctx context.Context, c Cancel) (Payment, error)

	// Webhook reads a webhook the PG sent, as the HTTP request carried it,
	// into what an inbox keeps of it: checked, and not yet believed. It asks
	// nothing of the PG, so that it answers inside the time the PG waits for
	// an answer -- the inbox keeps the [Notice], the request is answered, and
	// [Channel.Recheck] comes after. An error is a webhook to refuse: one
	// whose signature does not hold, or that cannot be read.
	Webhook(header http.Header, body []byte) (Notice, error)

	// Recheck asks the PG about the payment a [NoticePayment] names, and
	// answers what the PG's API says of it now: the only thing of a webhook
	// that is believed. Two rechecks of one payment are told apart by
	// [Payment.Progress], never by when they were made.
	Recheck(ctx context.Context, n Notice) (Payment, error)

	// RemoveMethod deletes a billing key at the PG, so that it charges
	// nothing even if it is found. A key the PG no longer has is removed.
	RemoveMethod(ctx context.Context, m Method) error
}

// Start is what registering a method starts from.
type Start struct {
	// Customer is the PG's id for the payer the method is for, when the
	// billing account has one from a method before: the PG then shows the
	// account's methods together. Left empty, the channel makes one up. It is
	// not an id of levyan's -- the PG asks for one nobody could guess.
	Customer string

	// Success is where the window sends the payer back once the method is
	// registered, with what [Channel.CompleteRegistration] takes in its
	// query; Fail is where it sends the payer when it is not, with why. Both
	// are absolute URLs.
	Success string
	Fail    string
}

// Registration is a registration begun.
type Registration struct {
	// Customer is the PG's id for the payer the method will be of. The page is
	// handed it too, so it is no secret; but it is nobody else's business
	// either, and is not written in a log.
	Customer string

	// Window is what the page opens the PG's window with.
	Window Window
}

// Window is what a page opens a PG's window with: the PG's script, and what
// its SDK is called with, by the SDK's own names. A page that draws a window
// is written for the PG whose window it draws, so the names are that PG's.
type Window struct {
	Script string
	Values map[string]string
}

// Method is a payment method registered with a PG: what charging it takes,
// and what a person may be shown of it.
type Method struct {
	// Customer is whom the PG has it under ([Registration.Customer]).
	Customer string

	// Key is the billing key, which charges it.
	Key BillingKey

	// Card is what a person may be shown of the card.
	Card Card

	// At is when the PG authenticated it.
	At time.Time
}

// Card is what a person may be shown of a card, and never enough to charge
// it.
type Card struct {
	// Issuer is the company that issued it, by the PG's code for it (Toss's
	// two-digit 카드사 코드).
	Issuer string

	// Number is the card's number as the PG masks it, "43301234****123*".
	// Masked, it is still the payer's, and is kept as personal data.
	Number string

	Kind  CardKind
	Owner CardOwner
}

// CardKind is what kind of card it is.
type CardKind int

const (
	// CardKindUnknown is a card the PG does not say the kind of.
	CardKindUnknown CardKind = iota
	Credit
	Debit // 체크카드
	Gift
)

// CardOwner is whose card it is.
type CardOwner int

const (
	// CardOwnerUnknown is a card the PG does not say the owner of.
	CardOwnerUnknown CardOwner = iota
	Personal
	Corporate
)

// Charge is a charge of a registered method.
type Charge struct {
	// Method is what is charged.
	Method Method

	// Order is levyan's id for the charge: 6 to 64 letters, digits, '-', '_'
	// or '='. A charge sent again after an unknown outcome is the same order;
	// one tried again after a decline is a new one, and an order is never
	// used for anything else -- the PG answers an order it has approved with
	// the payment it already is.
	Order string

	// Amount is how much, in a currency the channel charges in: KRW for a
	// Korean PG, whose minor unit is the won.
	Amount money.Amount

	// Name is what the payer sees the charge as, at most 100 characters.
	Name string

	// Key is the idempotency key, at most 300 characters and never used for
	// another request. The PG answers a request sent again under its key with
	// its first answer: so it is the same when a request whose answer never
	// came is sent again, and new when the order is charged again after the
	// PG said it has no payment of it ("<order>-2"). Required.
	Key string
}

// Cancel gives back some or all of a payment.
type Cancel struct {
	// Payment is the PG's id of what is canceled ([Payment.ID]).
	Payment string

	// Amount is how much of it is given back. All, instead, is whatever is
	// left of it. It is one or the other: a zero amount is never read as all
	// of it.
	Amount money.Amount
	All    bool

	// Reason is why, in words the payer may be shown: at most 200
	// characters.
	Reason string

	// Key is the idempotency key, as [Charge.Key]: the same each time this
	// cancellation is sent, and another for every other -- two cancellations
	// of the same amount are two keys. Required, since nothing else keeps a
	// cancellation once.
	Key string
}

// Payment is a payment as the PG says it is.
type Payment struct {
	// ID is the PG's id for it (Toss's paymentKey), which [Cancel] names. A
	// charge the card declined may have none.
	ID string

	// Order is the order it is the payment of.
	Order string

	Status Status

	// Amount is what it was for, and Balance what of it is left to give back.
	Amount  money.Amount
	Balance money.Amount

	// At is when it was approved: zero for one that was not.
	At time.Time

	// Cancels is every cancellation of it that is done, oldest first.
	Cancels []Cancellation

	// Failure is why a [Failed] payment failed.
	Failure *Failure
}

// Progress is how far along its life a payment is, by one observation of it.
// Of two observations of one payment, the one with more progress is the
// later, and one with less never takes the place of one with more: which is
// what keeps a webhook rechecked late, or two rechecks that finish out of
// order, from walking a payment back.
//
// A payment is pending, then paid or failed, then canceled a part at a time:
// so its progress is 0 while it is pending, and one more than how many
// cancellations it has once it is not.
func (p Payment) Progress() int {
	if p.Status == Pending {
		return 0
	}
	return 1 + len(p.Cancels)
}

// Status is where a payment is.
type Status int

const (
	// Pending is a payment the PG has and has not finished: one to ask about
	// again.
	Pending Status = iota + 1

	// Paid is a payment approved, none of which has been given back.
	Paid

	// PartlyCanceled is a paid payment some of which has been given back.
	PartlyCanceled

	// Canceled is a paid payment given back in whole.
	Canceled

	// Failed is a payment that did not happen: declined, or failed at the PG.
	Failed
)

func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Paid:
		return "paid"
	case PartlyCanceled:
		return "partly canceled"
	case Canceled:
		return "canceled"
	case Failed:
		return "failed"
	default:
		return "no status"
	}
}

// Cancellation is a part of a payment given back.
type Cancellation struct {
	// ID is the PG's id for it (Toss's transactionKey).
	ID     string
	Amount money.Amount
	Reason string
	At     time.Time
}

// Failure is why a payment failed.
type Failure struct {
	// Kind is [Declined] for the card's fault -- refused, over its limit,
	// stopped, lost, expired -- and anything else for a payment that failed
	// at the PG for a reason of its own, which says nothing of the card.
	Kind Kind

	// Code and Message are the PG's. The message is the PG's words, which may
	// be shown to the payer.
	Code    string
	Message string
}

// Notice is a webhook as it came, before anything it says is believed, and
// what an inbox keeps of it.
type Notice struct {
	// ID is what keeps it once: the PG's id of the event when the webhook has
	// one, and its body's digest when it has not -- which every retry of a
	// delivery repeats.
	ID string

	// Type is what it is about, and Event the PG's own name for that.
	Type  NoticeType
	Event string

	// At is when the PG says it happened. It is not believed, and is not an
	// order: of two states of a payment the later is the one with more
	// [Payment.Progress].
	At time.Time

	// Signed is whether it carried a signature that held against the PG's
	// key. One that carries none is not refused unless the channel is told to
	// (a PG signs some of its webhooks and not others): it is worth what its
	// recheck answers, as a signed one is.
	Signed bool

	// Payment and Order are the payment a [NoticePayment] names, as it names
	// it. Nothing else of what it says is kept, since nothing else of it is
	// believed.
	Payment string
	Order   string

	// Method is the [BillingKey.Digest] of the key a [NoticeMethod] names.
	Method string
}

// NoticeType is what a webhook is about.
type NoticeType int

const (
	// NoticeOther is a webhook about nothing levyan acts on: answered, and
	// dropped.
	NoticeOther NoticeType = iota

	// NoticePayment is a payment whose state changed: recheck it.
	NoticePayment

	// NoticeMethod is a billing key the PG says it deleted. There is nothing
	// to recheck it by -- a PG does not answer what keys it has -- so it is a
	// hint: the next charge of the method says whether it is gone.
	NoticeMethod
)

func (t NoticeType) String() string {
	switch t {
	case NoticePayment:
		return "payment"
	case NoticeMethod:
		return "method"
	default:
		return "other"
	}
}
