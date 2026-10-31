// Package mor is what levyan asks of a merchant of record it sells through: a
// channel where the MoR is the seller. It sells to the payer in its own name,
// so the subscription's cycle, the tax, the legal invoice, refunds and
// chargebacks are the MoR's, and levyan keeps a mirror of the subscription --
// unlike a PG (package pg), where levyan is the seller and keeps the cycle
// itself.
//
// It is the boundary, and nothing behind it. Which MoR levyan sells through
// is the owner's decision: Paddle (2026-10-31, levyan#12), whose adapter is
// package paddle. Another MoR is another package implementing [Channel], and
// nothing that uses a channel names one but the wiring. It knows nothing of
// billing accounts (#4), invoices (#9) or dunning (#11): they are what calls
// it (`server/subscriptions` is what keeps a billing account's subscription).
//
// # What it promises
//
//   - levyan computes the amount, on this channel as on every other. Once a
//     period is closed, what a billing account owes for it is handed to the
//     MoR as a one-time charge of the payer's subscription ([Channel.Charge]).
//     The MoR is told an amount, never a quantity to price: no usage feature
//     of the MoR's is relied on.
//   - Card data never passes through levyan. The payer gives a card to the
//     MoR's checkout, which [Channel.StartCheckout] says how to open, and
//     levyan learns the customer and the subscription it started
//     ([Channel.CompleteCheckout]). What charges the card afterwards is the
//     subscription's id with levyan's API key, and only the key is a secret.
//   - The request failing is not the charge failing. A charge the card
//     declined is a [Transaction] whose status is [Failed], and no error. An
//     error is a request that failed, and its [Kind] says what is known --
//     the kinds of package pg, by the same names: [Invalid], [Auth] and
//     [RateLimited] that nothing happened, [Unknown] that something may have.
//   - A charge is made once for its key ([Charge.Key]). The MoR keeps no key
//     of levyan's for it, so a channel writes the key into the charge, looks
//     for it before it sends one, and answers a charge it finds with what it
//     is.
//   - An unknown outcome is asked about before anything is sent again, and
//     not before [Settle] has passed.
//   - A webhook is a hint. What it says is not believed: what it names is
//     asked of the MoR ([Channel.Recheck]), and the answer is. And it is not
//     waited for alone: the MoR's own list of what it noticed
//     ([Channel.Notices]) is read too, so that a notice reaches levyan when
//     its webhook cannot.
//
// # Once for its key
//
// A charge that is sent again is the same charge only if the MoR can tell it
// is, and an MoR with no idempotency key of the sender's cannot: so a channel
// tells it, by writing the key into what it charges, and looks for the key at
// the MoR before every charge it sends. A charge whose key is there is
// answered with what is there, and nothing is sent.
//
// A look sees what the MoR has written down, which a request still at the
// MoR -- one whose answer never came -- may not have been yet. So a key is
// believed absent only once [Settle] has passed since it was last sent: until
// then a look that finds nothing is [Unknown] ([ErrTooSoon]), and a charge of
// it sends nothing. Whoever sends a charge writes each send down before it
// makes it, and hands the channel the last one before ([Charge.Sent]), which
// the channel holds the charge to.
//
// A key the MoR has more than one charge of is a key charged twice, which the
// rule above is there to prevent and reconciliation (#13) is there to find:
// [Channel.Charged] answers the first of them, with [ErrTwice].
//
// # A charge for the next period
//
// A charge is billed now ([Now]), in a transaction of its own, or with the
// subscription's next renewal ([NextPeriod]), in the transaction that renews
// it. A subscription that will not renew -- scheduled to cancel or to pause at
// the end of its period, paused, canceled -- has no next period, and the MoR
// drops what was to be billed in it: Paddle forgives a subscription's one-time
// charges for the next period when it cancels. So:
//
//   - A charge for the next period of a subscription that will not renew is
//     refused before anything is sent, with [ErrNoNextPeriod]: it is charged
//     [Now] instead.
//   - A charge for the next period that the MoR took is found in what the
//     subscription will be billed next ([Scheduled]) until it is billed. If
//     the subscription is scheduled to end meanwhile, the charge is dropped
//     with it, and found nowhere: [NotFound], which is charged again, now,
//     under the same key -- so that a charge the MoR brings back after all
//     is a key charged twice ([ErrTwice]), and not a charge nobody can tell
//     from another.
//
// Charging [Now] has neither hazard, and is what a period's amount is handed
// over with unless there is a reason not to.
//
// # The subscription is the MoR's
//
// The MoR runs the cycle: it renews the subscription, retries a payment that
// failed, and cancels it -- when levyan asks ([Channel.Cancel]), and when the
// payer does, in the MoR's own pages, without asking levyan. What levyan keeps
// of a subscription is a mirror, made of what the MoR's API answers
// ([Channel.Subscription], [Channel.Recheck]), never of a webhook's body; two
// answers about one subscription are ordered by [Subscription.Updated], the
// MoR's own time of its last change.
package mor

import (
	"context"
	"net/http"
	"time"

	"github.com/lesomnus/levyan/internal/money"
)

// Settle is how long after a charge or a refund was last sent its absence at
// the MoR is not believed: a request whose answer never came may still be
// on its way into the MoR's records, and the MoR keeps no key of levyan's to
// answer it again with. Within it a look that finds nothing is [ErrTooSoon],
// and nothing is sent again.
const Settle = 10 * time.Minute

// Channel is an MoR levyan sells through.
//
// Every method that asks the MoR takes a context, which is the most it waits:
// a charge made now is collected while the MoR answers, so a context shorter
// than that makes outcomes unknown that would not have been.
type Channel interface {
	// Name is the MoR's, as a stored subscription says which channel it is
	// of: "paddle".
	Name() string

	// StartCheckout begins a subscription: what a page opens the MoR's
	// checkout with, in which the payer gives a card to the MoR and not to
	// levyan, and subscribes to the plan the channel is configured with. The
	// caller keeps the [Checkout] until the payer is sent back.
	StartCheckout(ctx context.Context, s Start) (Checkout, error)

	// CompleteCheckout is the subscription a checkout started, once the
	// payer has finished it: the customer the MoR has the payer as, and the
	// subscription, as the MoR answers them now. A checkout not finished yet
	// -- the payer still in it, or gone from it, or the MoR still making the
	// subscription -- is [NotFound] with [ErrOpen], and is asked again on the
	// payer's return or on the webhook that says it is done. One that can no
	// longer be finished is [Declined].
	//
	// A checkout is completed by levyan's own record of it, never by what
	// the page says: a checkout started for one billing account is completed
	// for no other.
	CompleteCheckout(ctx context.Context, c Checkout) (Subscription, error)

	// Subscription is a subscription as the MoR has it now.
	Subscription(ctx context.Context, id string) (Subscription, error)

	// Cancel ends a subscription at the end of its period, or now, and
	// answers it as it is after. A subscription already canceled is
	// answered as it is, and one already scheduled to cancel at the end of
	// its period is too when that is what is asked.
	Cancel(ctx context.Context, c Cancel) (Subscription, error)

	// Charge adds a one-time charge to a subscription, now or with its next
	// renewal, and answers the [Transaction] it is in: [Paid], [Failed] with
	// why, [Pending] while the MoR collects, or [Scheduled] for one that
	// waits for the renewal. An error is a request that failed, and an
	// [Unknown] one may have charged the card (package doc).
	//
	// A charge whose key the MoR has is answered with what it is, and
	// nothing is sent; one for another amount is [Invalid]. A charge sent
	// less than [Settle] ago is not sent again ([ErrTooSoon]).
	Charge(ctx context.Context, c Charge) (Transaction, error)

	// Charged is the transaction a charge is in, by its key, as the MoR has
	// it now: [NotFound] when the MoR has no charge of the key, which is the
	// MoR saying it was never made -- or was dropped with a subscription that
	// ended -- and is answered only once [Settle] has passed since the charge
	// was last sent. A key the MoR has more than once is answered with the
	// first, and [ErrTwice].
	Charged(ctx context.Context, c Charge) (Transaction, error)

	// Transaction is a transaction as the MoR has it now, with every
	// adjustment of it.
	Transaction(ctx context.Context, id string) (Transaction, error)

	// Refund asks the MoR to give back some or all of a paid transaction,
	// and answers the [Adjustment] it is: [Requested] until the MoR approves
	// it, which may be days, or [Approved]. A refund whose key the MoR has is
	// answered with what it is, and nothing is sent; one sent less than
	// [Settle] ago is not sent again.
	Refund(ctx context.Context, r Refund) (Adjustment, error)

	// Refunded is the refund of a key as the MoR has it now: [NotFound] when
	// the transaction has none, answered only once [Settle] has passed since
	// the refund was last sent.
	Refunded(ctx context.Context, r Refund) (Adjustment, error)

	// Webhook reads a webhook the MoR sent, as the HTTP request carried it,
	// into what an inbox keeps of it: checked, and not yet believed. It asks
	// nothing of the MoR, so that it answers inside the time the MoR waits
	// for an answer -- the inbox keeps the [Notice], the request is answered,
	// and [Channel.Recheck] comes after. An error is a webhook to refuse: one
	// whose signature does not hold, that was signed too long ago to be
	// fresh, or that cannot be read.
	Webhook(header http.Header, body []byte) (Notice, error)

	// Recheck asks the MoR about what a notice names, and answers what the
	// MoR's API says of it now: the only thing of a webhook that is believed.
	Recheck(ctx context.Context, n Notice) (Rechecked, error)

	// Notices is what the MoR has noticed, as its own list of them answers:
	// the notices its webhooks are deliveries of, oldest first, a page at a
	// time ([Read]). It is how a notice reaches levyan when no webhook can --
	// a deployment the MoR cannot reach -- and how a webhook that never came
	// is found: a notice read here is the one its webhook delivers, by the
	// same [Notice.ID], and is no more believed.
	Notices(ctx context.Context, r Read) (Notices, error)
}

// Read is where a read of an MoR's notices begins: after the notice After
// names, or, for a first read with none, with the notices since Since.
type Read struct {
	After string
	Since time.Time
}

// Notices is a page of an MoR's notices, oldest first, and whether there are
// more after it.
type Notices struct {
	Notices []Notice

	// Last is the ID of the last notice the page read, which the next read
	// begins after: the last of Notices, or one after it that was no notice
	// levyan could read -- passed over, as a webhook of it would be refused.
	Last string

	More bool
}

// Start is what a checkout starts from.
type Start struct {
	// Account is levyan's id of the billing account the subscription is
	// for. The checkout carries it to the MoR, which keeps it on the
	// subscription, and the checkout is completed for it and for no other.
	Account string

	// Customer is the MoR's id for the payer, when the billing account has
	// one from a subscription before: the MoR then knows the payer. Left
	// empty, the MoR makes one in the checkout.
	Customer string

	// Currency is what the subscription is billed in, and so every charge
	// of it: one levyan prices in (package money). Left empty, the MoR
	// chooses by where the payer is.
	Currency string

	// Success is where the checkout sends the payer once they have finished:
	// an absolute URL.
	Success string

	// Again is a checkout begun for the same account before and not
	// finished, as levyan recorded it: answered again -- the same checkout,
	// opened anew -- while the MoR still takes it, so that a payer who
	// begins twice finishes one, and one subscription is made. One the MoR
	// no longer takes is begun anew, and answered with another ID; one that
	// was finished meanwhile is [Invalid] with [ErrFinished], to complete
	// instead. Nil begins one.
	Again *Checkout
}

// Checkout is a checkout begun.
type Checkout struct {
	// ID is the MoR's id for it (Paddle's transaction), which completes it.
	ID string

	// Account is the billing account it is for ([Start.Account]).
	Account string

	// Window is what a page opens it with.
	Window Window
}

// Window is what a page opens an MoR's checkout with: the MoR's script, and
// what its SDK is called with, by the SDK's own names -- a page that draws a
// checkout is written for the MoR whose checkout it draws -- or the MoR's own
// page for it, to send the payer to instead.
type Window struct {
	Script string
	Values map[string]string
	URL    string

	// Environment is the MoR's environment the checkout is in, which its SDK
	// is told: "sandbox", where test cards move no money, or "production".
	Environment string

	// Token is the client-side token its SDK is initialized with, among
	// Values: public, the checkout's, and no key. Empty for an MoR whose
	// checkout takes none.
	Token string
}

// Subscription is a subscription as the MoR says it is: what levyan keeps a
// mirror of.
type Subscription struct {
	// ID is the MoR's id for it, which [Charge] and [Cancel] name.
	ID string

	// Customer is the MoR's id for the payer.
	Customer string

	// Account is the billing account it was started for, as the checkout
	// told the MoR.
	Account string

	Status SubscriptionStatus

	// Currency is what it is billed in: every charge of it is in this.
	Currency string

	// Period is the billing period the MoR is in: zero for a subscription
	// paused or canceled.
	Period Period

	// Next is when the MoR next bills it: zero when it will not.
	Next time.Time

	// Change is what the MoR will do to it at a time of its own: cancel it,
	// pause it, or resume it. Nil for nothing.
	Change *Change

	// Canceled is when it was canceled: zero for one that is not.
	Canceled time.Time

	// Updated is when the MoR last changed it, by the MoR's clock. Of two
	// answers about one subscription, the one updated later is the later,
	// whatever order they came back in.
	Updated time.Time

	// Method is how the payer pays, when the answer says: what the checkout
	// that started it was paid with, from [Channel.CompleteCheckout]. Nil
	// for an answer that does not say -- [Channel.Subscription] and
	// [Channel.Recheck] of one -- which is no answer that it has none; how a
	// subscription is paid afterwards is in its transactions
	// ([Transaction.Method]).
	Method *Method
}

// Method is how a payment was made, as much of it as levyan keeps: never
// more of a card than its brand and its last four digits.
type Method struct {
	// Type is the MoR's name of the kind: card, south_korea_local_card,
	// kakao_pay, naver_pay, paypal, apple_pay, ….
	Type string

	// Brand is a card's brand (visa), or the company of a Korean card
	// (hyundai); empty for a method that is no card.
	Brand string

	// Last4 is a card's last four digits, when the MoR says them.
	Last4 string
}

// Renews is whether the subscription has a next period to bill a charge with:
// active or in trial, with a next billing and nothing scheduled to end it.
func (s Subscription) Renews() bool {
	if s.Status != Active && s.Status != Trialing {
		return false
	}
	if s.Next.IsZero() {
		return false
	}
	return s.Change == nil || s.Change.Kind == Resume
}

// Period is a billing period: from Start, to End.
type Period struct {
	Start time.Time
	End   time.Time
}

// Change is a change the MoR has scheduled of a subscription.
type Change struct {
	Kind ChangeKind

	// At is when it takes effect.
	At time.Time
}

// ChangeKind is what a scheduled change does.
type ChangeKind int

const (
	// CancelAtEnd cancels it at the end of its period.
	CancelAtEnd ChangeKind = iota + 1

	// PauseAtEnd pauses it at the end of its period.
	PauseAtEnd

	// Resume resumes a paused subscription.
	Resume
)

func (k ChangeKind) String() string {
	switch k {
	case CancelAtEnd:
		return "cancel"
	case PauseAtEnd:
		return "pause"
	case Resume:
		return "resume"
	default:
		return "no change"
	}
}

// SubscriptionStatus is where a subscription is.
type SubscriptionStatus int

const (
	// Active is billed, and paid up.
	Active SubscriptionStatus = iota + 1

	// Trialing is in a trial the MoR has not billed yet.
	Trialing

	// PastDue has a payment the MoR has not collected, and is retrying.
	// Nothing more is charged of it until it is paid ([ErrPastDue]).
	PastDue

	// Paused is billed nothing until it resumes.
	Paused

	// Canceled is over: the MoR bills it nothing again, and it is never
	// resumed ([ErrEnded]).
	Canceled
)

func (s SubscriptionStatus) String() string {
	switch s {
	case Active:
		return "active"
	case Trialing:
		return "trialing"
	case PastDue:
		return "past due"
	case Paused:
		return "paused"
	case Canceled:
		return "canceled"
	default:
		return "no status"
	}
}

// Cancel ends a subscription.
type Cancel struct {
	// Subscription is the MoR's id of it.
	Subscription string

	// Now cancels it now. Otherwise it is canceled at the end of its
	// period, and a charge for its next period is refused from then on.
	Now bool
}

// When is when a charge is billed.
type When int

const (
	// Now bills it in a transaction of its own, collected while the MoR
	// answers.
	Now When = iota + 1

	// NextPeriod bills it with the subscription's next renewal, in the
	// transaction that renews it.
	NextPeriod
)

func (w When) String() string {
	switch w {
	case Now:
		return "now"
	case NextPeriod:
		return "next period"
	default:
		return "no time"
	}
}

// Charge is a one-time charge of a subscription.
type Charge struct {
	// Subscription is the MoR's id of what is charged.
	Subscription string

	// Key is levyan's id of the charge, which keeps it once (package doc):
	// 1 to 64 of letters, digits, '-', '_', '.', ':' and '='. It is the
	// same every time this charge is sent, and never another charge's -- a
	// charge the card declined is tried again as a new key. Required.
	Key string

	// When is when it is billed. Required.
	When When

	// Amount is what levyan computed, in the subscription's currency.
	// Whether the MoR's tax is added to it or is in it is the channel's
	// configuration, and the same for every charge and refund of a channel.
	Amount money.Amount

	// Description is what the payer sees the charge as, on the MoR's
	// invoice: 2 to 200 characters.
	Description string

	// Sent is when this charge was last sent to the MoR before the call it
	// is given to, by the caller's record: zero for one never sent. The
	// caller writes each send down before it makes it, so that a send whose
	// answer never came is in the record. A charge last sent less than
	// [Settle] ago is not sent again, and its absence is not believed.
	Sent time.Time
}

// Transaction is what the MoR bills a subscription with -- a charge made now,
// a renewal with the charges of its period, a checkout -- as the MoR says it
// is.
type Transaction struct {
	// ID is the MoR's id for it, which [Refund] names: "" for a charge still
	// [Scheduled], which is in no transaction yet.
	ID string

	Subscription string
	Customer     string

	Status Status

	// Lines is what it bills, levyan's charges among them.
	Lines []Line

	// Total is what it bills the payer, tax included, before anything is
	// given back.
	Total money.Amount

	// At is when it was paid: zero for one that was not.
	At time.Time

	// Adjustments is what was given back of it, or taken back, oldest
	// first: refunds and chargebacks.
	Adjustments []Adjustment

	// Failure is why the last attempt to collect it failed: of a [Failed]
	// transaction, and of a [Pending] one the MoR is retrying.
	Failure *Failure

	// Awaiting is whether the MoR waits for the payer to act on its latest
	// attempt to collect it -- approve it in their card company's app, pass
	// their bank's check -- before it can: of a [Pending] transaction.
	Awaiting bool

	// Method is what its latest attempt to collect it was made with: nil
	// for one never attempted, as a charge [Scheduled] is.
	Method *Method
}

// Line is a line of a transaction.
type Line struct {
	// ID is the MoR's id of the line, which a refund of part of a
	// transaction names: "" in a charge still [Scheduled].
	ID string

	// Key is the key of levyan's charge it is ([Charge.Key]): "" for a line
	// that is no charge of levyan's, as the subscription's plan is.
	Key string

	// Name is what the payer sees it as.
	Name string

	// Amount is what was charged, in the terms of [Charge.Amount]; Tax is
	// the MoR's tax on it, and Total what the payer is billed for it.
	Amount money.Amount
	Tax    money.Amount
	Total  money.Amount
}

// Line is the line of a charge's key, and whether there is one.
func (t Transaction) Line(key string) (Line, bool) {
	for _, l := range t.Lines {
		if key != "" && l.Key == key {
			return l, true
		}
	}
	return Line{}, false
}

// Adjustment is the adjustment of a refund's key, and whether there is one.
func (t Transaction) Adjustment(key string) (Adjustment, bool) {
	for _, a := range t.Adjustments {
		if key != "" && a.Key == key {
			return a, true
		}
	}
	return Adjustment{}, false
}

// Progress is how far along its life a transaction is, by one observation of
// it. Of two observations of one charge, the one with more progress is the
// later, and one with less never takes the place of one with more: which is
// what keeps a webhook rechecked late, or two rechecks that finish out of
// order, from walking a charge back.
//
// A charge is scheduled, then in a transaction the MoR is collecting, then
// paid or failed, and then adjusted an adjustment at a time, each of which is
// requested and then decided. So its progress is 0 while it is scheduled, 1
// while it is pending, 2 once it is paid or failed, and one more for each
// adjustment of it and for each step each has taken.
func (t Transaction) Progress() int {
	n := 2
	switch t.Status {
	case Scheduled:
		n = 0
	case Pending:
		n = 1
	}
	for _, a := range t.Adjustments {
		n += 1 + a.Progress()
	}
	return n
}

// Status is where a transaction is.
type Status int

const (
	// Scheduled is a charge for the next period, which the MoR bills with
	// the subscription's renewal: nothing is collected yet, and it is in no
	// transaction until then.
	Scheduled Status = iota + 1

	// Pending is a transaction the MoR has and has not collected: one being
	// paid, or one whose payment failed and that the MoR is retrying. One to
	// ask about again.
	Pending

	// Paid is a transaction collected.
	Paid

	// Failed is a transaction that will not be collected: declined, or
	// canceled at the MoR.
	Failed
)

func (s Status) String() string {
	switch s {
	case Scheduled:
		return "scheduled"
	case Pending:
		return "pending"
	case Paid:
		return "paid"
	case Failed:
		return "failed"
	default:
		return "no status"
	}
}

// Failure is why a transaction was not collected.
type Failure struct {
	// Kind is [Declined] for the card's fault -- refused, over its limit,
	// expired, gone -- and anything else for one that failed at the MoR for
	// a reason of its own, which says nothing of the card.
	Kind Kind

	// Code and Message are the MoR's. The message is the MoR's words.
	Code    string
	Message string
}

// Refund gives back some or all of a paid transaction.
type Refund struct {
	// Transaction is the MoR's id of what is given back from
	// ([Transaction.ID]).
	Transaction string

	// Line is the line given back from ([Line.ID]): required for an amount,
	// and with All it gives back all of the line. With All and no line, all
	// of the transaction is given back.
	Line string

	// Amount is how much, in the terms of [Charge.Amount]. All, instead, is
	// all of the line or the transaction. It is one or the other: a zero
	// amount is never read as all of it.
	Amount money.Amount
	All    bool

	// Reason is why, in words kept with the refund at the MoR: 1 to 200
	// characters.
	Reason string

	// Key is levyan's id of the refund, as [Charge.Key] is of a charge: the
	// same each time this refund is sent, and another for every other --
	// two refunds of the same amount are two keys. Required, since nothing
	// else keeps a refund once.
	Key string

	// Sent is when this refund was last sent, as [Charge.Sent].
	Sent time.Time
}

// Adjustment is what was given back of a transaction, or taken back: a
// refund, or a chargeback, as the MoR says it is.
type Adjustment struct {
	// ID is the MoR's id for it.
	ID string

	// Transaction is the MoR's id of what it adjusts.
	Transaction string

	Kind   AdjustmentKind
	Status AdjustmentStatus

	// Amount is what it gives back, tax included.
	Amount money.Amount

	// Reason is why, as it was asked, and Key the key it was asked under:
	// "" for one levyan did not ask for -- a chargeback, or a refund made in
	// the MoR's own pages.
	Reason string
	Key    string

	// At is when it was made.
	At time.Time
}

// Progress is how far along its life an adjustment is: 0 while it waits for
// the MoR, 1 once the MoR has approved or rejected it, and 2 once what was
// approved is reversed.
func (a Adjustment) Progress() int {
	switch a.Status {
	case Requested:
		return 0
	case Reversed:
		return 2
	default:
		return 1
	}
}

// AdjustmentKind is what an adjustment is.
type AdjustmentKind int

const (
	// OtherAdjustment is one levyan does not act on: a credit, of an
	// invoice levyan never asks for.
	OtherAdjustment AdjustmentKind = iota

	// RefundAdjustment is a refund: asked by levyan, or made in the MoR's
	// own pages.
	RefundAdjustment

	// Chargeback is the payer's bank taking a payment back -- a dispute, or
	// the MoR's warning of one -- which the MoR contests, not levyan.
	Chargeback

	// ChargebackReversed is a chargeback the MoR won back.
	ChargebackReversed
)

func (k AdjustmentKind) String() string {
	switch k {
	case RefundAdjustment:
		return "refund"
	case Chargeback:
		return "chargeback"
	case ChargebackReversed:
		return "chargeback reversed"
	default:
		return "other"
	}
}

// AdjustmentStatus is where an adjustment is.
type AdjustmentStatus int

const (
	// Requested waits for the MoR to approve it: nothing is given back yet.
	Requested AdjustmentStatus = iota + 1

	// Approved is given back, or on its way back to the payer's card.
	Approved

	// Rejected is a refund the MoR would not make: nothing is given back.
	Rejected

	// Reversed is one approved and then undone.
	Reversed
)

func (s AdjustmentStatus) String() string {
	switch s {
	case Requested:
		return "requested"
	case Approved:
		return "approved"
	case Rejected:
		return "rejected"
	case Reversed:
		return "reversed"
	default:
		return "no status"
	}
}

// Notice is a webhook as it came, checked and before anything it says is
// believed: what an inbox keeps of it.
type Notice struct {
	// ID is what keeps it once: the MoR's id of the event (Paddle's
	// event_id), which every delivery of it repeats.
	ID string

	// Type is what it is about, and Event the MoR's own name for that.
	Type  NoticeType
	Event string

	// At is when the MoR says it happened. It is not believed, and is not an
	// order: of two states of a transaction the later is the one with more
	// [Transaction.Progress], and of a subscription the one [Subscription.Updated]
	// later.
	At time.Time

	// Transaction, Subscription and Adjustment are what it names, as it
	// names them. Nothing else of what it says is kept, since nothing else
	// of it is believed.
	Transaction  string
	Subscription string
	Adjustment   string
}

// NoticeType is what a webhook is about.
type NoticeType int

const (
	// NoticeOther is a webhook about nothing levyan acts on: answered, and
	// dropped.
	NoticeOther NoticeType = iota

	// NoticeTransaction is a transaction that changed, or an adjustment of
	// one: recheck the transaction.
	NoticeTransaction

	// NoticeSubscription is a subscription that changed: recheck it.
	NoticeSubscription
)

func (t NoticeType) String() string {
	switch t {
	case NoticeTransaction:
		return "transaction"
	case NoticeSubscription:
		return "subscription"
	default:
		return "other"
	}
}

// Rechecked is what the MoR's API answers of what a notice names: the
// transaction, with every adjustment of it, or the subscription.
type Rechecked struct {
	Transaction  *Transaction
	Subscription *Subscription
}
