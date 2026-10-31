// Package merchant is the collector of the merchant of record's channel
// (`CHANNEL_MOR`, #12): what an invoice on it has due is charged as a one-time
// charge of its billing account's subscription with the MoR, and what a
// credit note gives back of one is refunded from the transaction that charge
// is in (docs/design.md §15, "MoR 수금기").
//
// The MoR is the seller on its channel. It issues the legal invoice and the
// receipt, adds its own tax and numbers what it sells, so levyan's invoice is
// the record of what levyan asked it to collect, with its references once it
// has: nothing is ever issued here ([Collector.Issue]). An invoice is
// collected as it is finalized, with no day it is due ([Collector.Due]), by
// attempts levyan sends ([Collector.Pushes]), and the least the channel
// collects of one is the MoR's, as its adapter says ([Collector.Minimum]).
// That the MoR is Paddle is the owner's (2026-10-31); nothing here names it.
//
// # What is charged, and when nothing is
//
// A charge is of the subscription the billing account is collected through
// -- its live mirror (`subscriptions.CollectionOf`) -- made now, under the
// attempt's key, for what is due, as the invoice's description, and with the
// send before it as the MoR is told it (`mor.Charge.Sent`): within
// `mor.Settle` of a send whose answer never came, the adapter sends nothing.
//
// A billing account whose collection is not collectable is sent nothing, and
// the attempt fails at once as a decline of the kind closest to what its
// payer has to do ([Failure]), with a code that names the state, so that
// dunning (#11) treats a billing account with no method to charge as it
// treats a card to replace. That it fails so, and that no subscription or a
// canceled one is a method to give anew, is Claude's (2026-10-31), who said
// of a payment waiting for its payer that the payer has to act; which kind
// that is, and a paused or past-due one's, are the implementation's. What
// follows -- another attempt, a restriction -- is dunning's.
//
// # What is asked
//
// What came of a charge is asked of every subscription the billing account
// has had with the MoR, not of its live one alone: a send whose answer never
// came may have charged one the payer has canceled since, and a charge found
// nowhere is sent again. The charge is found by its key; one found in more
// than one transaction is a key charged twice (`mor.ErrTwice`), which settles
// by the first of them that was paid, is said in the log, and marks the
// attempt for reconciliation (#13), which gives the rest back.
//
// # What a notice brings
//
// The inbox asks the MoR again of every transaction a notice names
// (`subscriptions.Taker`), and hands the answer to [Collector.Heard], which
// settles every attempt whose key it carries -- a charge's line, a refund's
// adjustment -- as an ask would (`invoices.Invoices.Heard`), at once: a
// payment lands without waiting for the next pass of the loop. The inbox's
// own rows are off the trail; what this writes goes through the servers.
package merchant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lesomnus/otx/log"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// Collector is the merchant of record's channel's collector.
type Collector struct {
	// Channel is the merchant of record: the same one the subscriptions are
	// begun and mirrored through (`cmd.Server.Subscriptions.Channel`).
	Channel mor.Channel

	// Db is what a billing account's subscriptions are read from.
	Db *ent.Client

	// Ledger is what [Collector.Heard] writes what a notice brought through:
	// the ledger this collects for (`cmd.Server.Invoices`). Nil writes
	// nothing.
	Ledger *invoices.Invoices
}

var _ invoices.Collector = (*Collector)(nil)

// Minimum is the least the MoR charges in currency, as its adapter says it.
func (c *Collector) Minimum(currency string) int64 { return c.Channel.Minimum(currency).Minor }

// Pushes: levyan charges the payer's subscription itself.
func (c *Collector) Pushes() bool { return true }

// Due is nothing: an invoice is charged as it is finalized.
func (c *Collector) Due(time.Time) time.Time { return time.Time{} }

// Issue is never asked of this channel: the MoR, the seller, issues what it
// sells, and levyan numbers nothing of it.
func (c *Collector) Issue(context.Context, invoices.Document) (invoices.Issued, error) {
	return invoices.Issued{}, &invoices.Error{Op: "issue", Kind: invoices.Invalid, Message: "the merchant of record is the seller, and issues its own"}
}

// Send sends attempt `s`: a charge of the billing account's subscription, now,
// or a refund of what a credit note gives back from the transaction the
// invoice was charged in.
func (c *Collector) Send(ctx context.Context, s invoices.Send) (invoices.Outcome, error) {
	switch s.Kind {
	case levyan.AttemptKind_ATTEMPT_KIND_CHARGE:
		return c.charge(ctx, s)
	case levyan.AttemptKind_ATTEMPT_KIND_REFUND:
		return c.refund(ctx, s)
	}

	return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Message: "an attempt is a charge or a refund"}
}

// Ask is what the MoR has of attempt `a` now, by its key.
func (c *Collector) Ask(ctx context.Context, a invoices.Attempt) (invoices.Outcome, error) {
	switch a.Kind {
	case levyan.AttemptKind_ATTEMPT_KIND_CHARGE:
		return c.charged(ctx, a)
	case levyan.AttemptKind_ATTEMPT_KIND_REFUND:
		return c.refunded(ctx, a)
	}

	return invoices.Outcome{}, &invoices.Error{Op: "ask", Kind: invoices.Invalid, Message: "an attempt is a charge or a refund"}
}

// collection is the collection of the billing account invoice `inv` bills.
func (c *Collector) collection(ctx context.Context, inv invoices.Invoice) (subscriptions.Collection, error) {
	ba, err := c.Db.BillingAccount.Get(ctx, inv.Account.Uuid())
	if err != nil {
		return subscriptions.Collection{}, err
	}
	if ba.Channel != levyan.Channel_CHANNEL_MOR.String() {
		return subscriptions.Collection{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Message: "the billing account is billed through " + ba.Channel + ", not the merchant of record"}
	}

	return subscriptions.CollectionOf(ctx, c.Db, ba)
}

// charge charges what attempt `s` is for, now, of the subscription the
// billing account is collected through -- or fails it, sending nothing, when
// the billing account is not collectable.
func (c *Collector) charge(ctx context.Context, s invoices.Send) (invoices.Outcome, error) {
	col, err := c.collection(ctx, s.Invoice)
	if err != nil {
		return invoices.Outcome{}, err
	}
	m := col.Mirror
	if col.State != levyan.CollectionState_COLLECTION_STATE_COLLECTABLE || m == nil || m.Mor != c.Channel.Name() {
		state := col.State
		if state == levyan.CollectionState_COLLECTION_STATE_COLLECTABLE {
			// Collectable, and through no subscription of this MoR's: one
			// begun with another, which nothing here charges.
			state = levyan.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION
		}
		log.From(ctx).InfoContext(ctx, "invoices: the billing account cannot be collected from, and nothing is sent",
			slog.String("attempt", s.Key), slog.String("billing_account", s.Invoice.Account.String()), slog.String("collection", state.String()))
		return failed(state), nil
	}

	t, err := c.Channel.Charge(ctx, mor.Charge{
		Subscription: m.MorSubscription,
		Key:          s.Attempt.Key,
		When:         mor.Now,
		Amount:       s.Amount,
		Description:  s.Invoice.Description,
		Sent:         s.Previous,
	})
	switch {
	case errors.Is(err, mor.ErrTwice):
		return c.twice(ctx, s.Attempt, t), nil
	case errors.Is(err, mor.ErrEnded):
		// Ended at the MoR before levyan heard: as a canceled one is.
		return failed(levyan.CollectionState_COLLECTION_STATE_CANCELED), nil
	case errors.Is(err, mor.ErrPastDue):
		return failed(levyan.CollectionState_COLLECTION_STATE_PAST_DUE), nil
	case err != nil:
		return invoices.Outcome{}, err
	}

	return outcomeOf(t), nil
}

// charged looks for attempt `a`'s charge in every subscription the billing
// account has had with this MoR: found in one, it is what that one has;
// found nowhere, once every look may be believed, the MoR never made it.
func (c *Collector) charged(ctx context.Context, a invoices.Attempt) (invoices.Outcome, error) {
	col, err := c.collection(ctx, a.Invoice)
	if err != nil {
		return invoices.Outcome{}, err
	}

	var (
		found   []mor.Transaction
		twice   bool
		unknown error
	)
	for _, m := range col.Mirrors {
		if m.Mor != c.Channel.Name() {
			continue
		}
		t, err := c.Channel.Charged(ctx, mor.Charge{
			Subscription: m.MorSubscription,
			Key:          a.Key,
			When:         mor.Now,
			Amount:       a.Amount,
			Description:  a.Invoice.Description,
			Sent:         a.Sent,
		})
		switch {
		case err == nil:
			found = append(found, t)
		case errors.Is(err, mor.ErrTwice):
			found, twice = append(found, t), true
		case mor.KindOf(err) == mor.NotFound:
		case unknown == nil:
			// Too soon to believe (mor.ErrTooSoon), or not asked: what is
			// not known is not that it was never made.
			unknown = err
		}
	}

	switch {
	case len(found) > 1 || twice:
		return c.twice(ctx, a, firstPaid(found)), nil
	case len(found) == 1:
		return outcomeOf(found[0]), nil
	case unknown != nil:
		return invoices.Outcome{}, unknown
	}

	return invoices.Outcome{}, &invoices.Error{Op: "ask", Kind: invoices.NotFound, Message: "no subscription of the billing account's has a charge of the key"}
}

// twice is what attempt `a` comes to whose key the MoR charged more than once:
// what `t`, the first of its charges that was paid, is -- and the attempt
// marked for reconciliation (#13), which gives the rest back.
func (c *Collector) twice(ctx context.Context, a invoices.Attempt, t mor.Transaction) invoices.Outcome {
	log.From(ctx).ErrorContext(ctx, "invoices: the merchant of record charged an attempt's key more than once; the attempt settles by the first that was paid, and reconciliation gives the rest back",
		slog.String("attempt", a.Key), slog.String("transaction", t.ID), slog.String("invoice", a.Invoice.Id.String()))
	o := outcomeOf(t)
	o.Twice = true
	return o
}

// firstPaid is the first of `ts` that was paid, or the first of them.
func firstPaid(ts []mor.Transaction) mor.Transaction {
	for _, t := range ts {
		if t.Status == mor.Paid {
			return t
		}
	}
	return ts[0]
}

// refund asks the MoR to give back what attempt `s` is for, from the line of
// the charge in the transaction the invoice was charged in: all of the line
// when it is all that was charged, so that the MoR gives back its tax as it
// took it, and otherwise that amount of it.
func (c *Collector) refund(ctx context.Context, s invoices.Send) (invoices.Outcome, error) {
	if s.Refunds == "" {
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Message: "nothing of the invoice was charged to give back from"}
	}
	t, err := c.Channel.Transaction(ctx, s.Refunds)
	if err != nil {
		return invoices.Outcome{}, err
	}
	var line *mor.Line
	for i, l := range t.Lines {
		if l.Key != "" {
			line = &t.Lines[i]
			break
		}
	}
	if line == nil {
		return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Message: "the transaction " + t.ID + " holds no charge of levyan's to give back from"}
	}

	r := mor.Refund{Transaction: t.ID, Line: line.ID, Reason: reason(s.Reason), Key: s.Attempt.Key, Sent: s.Previous}
	if line.Amount == s.Amount {
		r.All = true
	} else {
		r.Amount = s.Amount
	}
	adj, err := c.Channel.Refund(ctx, r)
	if err != nil {
		return invoices.Outcome{}, err
	}

	return refundOf(adj), nil
}

// refunded is what the MoR has of attempt `a`'s refund, by its key, among the
// adjustments of the transaction it gives back from.
func (c *Collector) refunded(ctx context.Context, a invoices.Attempt) (invoices.Outcome, error) {
	if a.Refunds == "" {
		return invoices.Outcome{}, &invoices.Error{Op: "ask", Kind: invoices.NotFound, Message: "nothing of the invoice was charged to give back from"}
	}
	adj, err := c.Channel.Refunded(ctx, mor.Refund{Transaction: a.Refunds, Key: a.Key, Sent: a.Sent})
	if err != nil {
		return invoices.Outcome{}, err
	}

	return refundOf(adj), nil
}

// reason is why a refund is made as the MoR keeps it: 1 to 200 characters.
func reason(why string) string {
	why = strings.TrimSpace(why)
	if why == "" {
		why = "환불"
	}
	if r := []rune(why); len(r) > 150 {
		why = string(r[:150])
	}
	return why
}

// Heard settles every attempt whose key transaction `t` carries, as the MoR
// answered it of a notice: the charge each line of levyan's is, and the
// refund each adjustment of levyan's is, written through the ledger as an
// ask's answer is (`invoices.Invoices.Heard`). It is the inbox's
// `subscriptions.Taker.Transaction`; an error leaves the notice to be taken
// again.
func (c *Collector) Heard(ctx context.Context, _ *ent.Notice, t mor.Transaction) error {
	if c.Ledger == nil {
		return nil
	}
	var errs []error
	for _, l := range t.Lines {
		if l.Key == "" {
			continue
		}
		if err := c.Ledger.Heard(ctx, levyan.Channel_CHANNEL_MOR, levyan.AttemptKind_ATTEMPT_KIND_CHARGE, l.Key, outcomeOf(t)); err != nil {
			errs = append(errs, fmt.Errorf("charge %s: %w", l.Key, err))
		}
	}
	for _, a := range t.Adjustments {
		if a.Key == "" || a.Kind != mor.RefundAdjustment {
			continue
		}
		if err := c.Ledger.Heard(ctx, levyan.Channel_CHANNEL_MOR, levyan.AttemptKind_ATTEMPT_KIND_REFUND, a.Key, refundOf(a)); err != nil {
			errs = append(errs, fmt.Errorf("refund %s: %w", a.Key, err))
		}
	}

	return errors.Join(errs...)
}

// outcomeOf is what the ledger writes of a charge in transaction `t`: paid,
// with the transaction and the MoR's invoice number of it; failed, with why;
// or pending -- collecting, or scheduled -- to be asked again.
func outcomeOf(t mor.Transaction) invoices.Outcome {
	switch t.Status {
	case mor.Paid:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, Ref: t.ID, Number: t.InvoiceNumber, At: t.At}
	case mor.Failed:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_FAILED, Ref: t.ID, Failure: failureOf(t.Failure)}
	default:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_PENDING, Ref: t.ID}
	}
}

// failureOf is why a charge failed, as the MoR said it, value for value.
func failureOf(f *mor.Failure) *levyan.AttemptFailure {
	if f == nil {
		return invoices.FailureOf(invoices.Unknown, 0, "", "")
	}
	return invoices.FailureOf(invoices.Kind(f.Kind), pg.Decline(f.Decline), f.Code, f.Message)
}

// refundOf is what the ledger writes of a refund in adjustment `a`: waiting
// for the MoR's approval, given back, or refused. One approved and reversed
// after was given back, and what took it back again is reconciliation's.
func refundOf(a mor.Adjustment) invoices.Outcome {
	switch a.Status {
	case mor.Approved, mor.Reversed:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, Ref: a.ID}
	case mor.Rejected:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_FAILED, Ref: a.ID,
			Failure: invoices.FailureOf(invoices.Invalid, 0, "rejected", "the merchant of record rejected the refund")}
	default:
		return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_PENDING, Ref: a.ID}
	}
}

// Failure is what an attempt to charge a billing account whose collection is
// `state` fails with, sent nothing -- a decline of the kind closest to what
// its payer has to do, and a code that names the state -- and nil for one
// that is collectable:
//
//   - no subscription, or one canceled: a method to give anew, as a card to
//     replace is (Claude's, 2026-10-31);
//   - a payment that waits for the payer to approve it: the payer has to act
//     (Claude's), and of the four kinds the one whose payer acts -- gives
//     another method, or takes it up with its issuer -- is a card to replace
//     (implementation decision);
//   - paused, or past due: later, on the same subscription, as a temporary
//     decline is -- the method is sound, and it is a pause someone took, or
//     a payment the MoR is collecting by its own retries, that stands in the
//     way (implementation decision).
func Failure(state levyan.CollectionState) *levyan.AttemptFailure {
	decline, say := pg.DeclineReplace, ""
	switch state {
	case levyan.CollectionState_COLLECTION_STATE_COLLECTABLE:
		return nil
	case levyan.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION:
		say = "the billing account has no subscription to charge: its payer begins one in the merchant of record's checkout"
	case levyan.CollectionState_COLLECTION_STATE_CANCELED:
		say = "the billing account's subscription is canceled: its payer begins another in the merchant of record's checkout"
	case levyan.CollectionState_COLLECTION_STATE_NEEDS_ACTION:
		say = "a payment of the billing account's subscription waits for its payer to act on it"
	case levyan.CollectionState_COLLECTION_STATE_PAUSED:
		decline, say = pg.DeclineTemporary, "the billing account's subscription is paused, and is charged nothing until it resumes"
	case levyan.CollectionState_COLLECTION_STATE_PAST_DUE:
		decline, say = pg.DeclineTemporary, "the billing account's subscription is past due, and is charged nothing more until the merchant of record has collected what it is owed"
	default:
		say = "the billing account's collection is not known"
	}

	return invoices.FailureOf(invoices.Declined, decline, code(state), say)
}

// code is the code of an attempt that failed for a billing account's
// collection: the state's name, `no_subscription`.
func code(state levyan.CollectionState) string {
	return strings.ToLower(strings.TrimPrefix(state.String(), "COLLECTION_STATE_"))
}

// failed is an attempt that failed for a billing account's collection.
func failed(state levyan.CollectionState) invoices.Outcome {
	return invoices.Outcome{State: levyan.AttemptState_ATTEMPT_STATE_FAILED, Failure: Failure(state)}
}
