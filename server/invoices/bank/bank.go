// Package bank is the collector of the bank-transfer channel
// (`CHANNEL_BANK_KR`, #9): an institution in Korea pays what its invoice says
// by transfer, with a tax invoice, and levyan is the seller.
//
// Nothing here moves money. An invoice is finalized with the day it is due
// ([Channel.After] after) and its tax invoice is issued through an issuer
// (`internal/taxinvoice`) as it is; the institution transfers what it says,
// and an operator records the transfer as they see it come (`levyan-admin
// invoice paid`), which is on the trail as theirs. A refund is a transfer an
// operator makes back, and records the same way. So the channel pushes
// nothing ([Channel.Pushes]), and is never sent nor asked anything.
//
// That an institution pays by transfer with a tax invoice is the owner's
// (2026-10-31). That its billing account is opened by an operator alone,
// after a contract -- paying after the fact is credit -- is Claude's. Which
// service issues the tax invoices is the owner's open question, and none is
// named here.
package bank

import (
	"context"
	"strconv"
	"strings"
	"time"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/taxinvoice"
	"github.com/lesomnus/levyan/server/invoices"
)

// DefaultDue is how long after it is finalized an invoice on the channel is
// due, when `invoices.bank_kr.due` says nothing: thirty days, a month's
// usual term (implementation decision).
const DefaultDue = 30 * 24 * time.Hour

// Channel is the bank-transfer channel's collector.
type Channel struct {
	// Issuer issues its tax invoices.
	Issuer taxinvoice.Issuer

	// Seller is the deployment's own business: the supplier of every tax
	// invoice.
	Seller taxinvoice.Party

	// Least is the least it collects of an invoice, in won: 0, none, by
	// default -- carrying a small amount over is risky where levyan issues
	// the tax invoice (docs/design.md §17, 64).
	Least int64

	// After is how long after it is finalized an invoice is due:
	// [DefaultDue] when zero.
	After time.Duration
}

var _ invoices.Collector = (*Channel)(nil)

func (c *Channel) Minimum(currency string) int64 { return c.Least }

func (c *Channel) Pushes() bool { return false }

func (c *Channel) Due(at time.Time) time.Time {
	if c.After <= 0 {
		return at.Add(DefaultDue)
	}
	return at.Add(c.After)
}

// Key is the key a tax invoice of `d` is issued under, which keeps it once:
// the invoice's number, or the credit note's.
func Key(d invoices.Document) string {
	if d.CreditNote {
		return "LV-CN-" + strconv.FormatInt(d.Number, 10)
	}
	return "LV-INV-" + strconv.FormatInt(d.Number, 10)
}

// Issue issues `d`'s tax invoice -- or the amended one behind a credit note,
// less than nothing, written for the day the credit note was and naming the
// one it amends -- under its number. An answer that never came is asked
// after under the key before it is said to be unknown.
func (c *Channel) Issue(ctx context.Context, d invoices.Document) (invoices.Issued, error) {
	if d.Currency != "KRW" {
		return invoices.Issued{}, &invoices.Error{Op: "issue", Kind: invoices.Invalid, Message: "a Korean tax invoice is in won, and this is " + d.Currency}
	}
	v := taxinvoice.Invoice{
		Key:      Key(d),
		Written:  d.Written,
		Supplier: c.Seller,
		Buyer:    Buyer(d.BillTo),
		Supply:   d.Supply,
		Tax:      d.Tax,
		Items:    []taxinvoice.Item{{Name: item(d), Supply: d.Supply, Tax: d.Tax}},
	}
	if d.CreditNote {
		v.Amends = &taxinvoice.Amends{
			Key:      Key(invoices.Document{Number: d.Amends}),
			Approval: d.Document,
			Written:  d.Original,
			Cause:    Cause(d.Reason),
		}
		v.Note = "당초 작성일 " + d.Original.Format(time.DateOnly)
	}

	got, err := c.Issuer.Issue(ctx, v)
	if err != nil && taxinvoice.KindOf(err) == taxinvoice.Unknown {
		if had, gerr := c.Issuer.Get(ctx, v.Key); gerr == nil {
			got, err = had, nil
		}
	}
	if err != nil {
		return invoices.Issued{}, &invoices.Error{Op: "issue", Kind: kindOf(taxinvoice.KindOf(err)), Message: err.Error(), Err: err}
	}

	return invoices.Issued{At: got.At, Document: got.Approval}, nil
}

// Send is never asked of this channel: a transfer is the payer's to make.
func (c *Channel) Send(ctx context.Context, s invoices.Send) (invoices.Outcome, error) {
	return invoices.Outcome{}, &invoices.Error{Op: "send", Kind: invoices.Invalid, Message: "a transfer is the payer's to make, and an operator's to record"}
}

// Ask is never asked of this channel either: it has nothing levyan sent.
func (c *Channel) Ask(ctx context.Context, a invoices.Attempt) (invoices.Outcome, error) {
	return invoices.Outcome{}, &invoices.Error{Op: "ask", Kind: invoices.NotFound, Message: "a transfer is recorded, never sent"}
}

// Buyer is who a tax invoice is to: the business a billing account bills,
// as the invoice kept it.
func Buyer(b *levyan.BillTo) taxinvoice.Party {
	v := b.GetBusiness()
	email := strings.TrimSpace(v.GetEmail())
	if email == "" {
		email = b.GetEmail()
	}

	return taxinvoice.Party{
		Registration:   v.GetRegistration(),
		Name:           v.GetName(),
		Representative: v.GetRepresentative(),
		Address:        v.GetAddress(),
		Kind:           v.GetKind(),
		Item:           v.GetItem(),
		Email:          email,
	}
}

// Cause is why a credit note amends its invoice's tax invoice: a
// cancellation is the contract cancelled, and the rest an amount taken from
// the supply value (implementation decision; docs/design.md §17, 67).
func Cause(r levyan.CreditNoteReason) taxinvoice.Cause {
	if r == levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION {
		return taxinvoice.Cancelled
	}
	return taxinvoice.Changed
}

func item(d invoices.Document) string {
	if d.CreditNote {
		return d.Invoice.Description + " 이용료 정정"
	}
	return d.Invoice.Description + " 이용료"
}

func kindOf(k taxinvoice.Kind) invoices.Kind {
	switch k {
	case taxinvoice.Invalid:
		return invoices.Invalid
	case taxinvoice.Auth:
		return invoices.Auth
	case taxinvoice.RateLimited:
		return invoices.RateLimited
	case taxinvoice.NotFound:
		return invoices.NotFound
	}
	return invoices.Unknown
}
