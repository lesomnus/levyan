// Package invoices is what billing accounts are asked to pay, and how what
// they pay moves (#9): the invoices made of products' statements, the credit
// notes that take some of an invoice back, the credit balance a billing
// account is owed, and the payment ledger of every attempt to move money
// through a channel.
//
// # One invoice of a billing account a period
//
// A product's period is closed workspace by workspace (`server/billing`),
// and each statement it writes names the billing account that paid for the
// workspace over the period, when one did. Every such statement goes on one
// invoice of its billing account, as a line ([Invoices.Account]): the draft
// of that billing account in the product, begun by the first of them. Once
// the period is closed the draft is decided: finalized, or -- when it would
// ask for more than nothing and less than its channel collects -- kept to
// gather the next period's too (the owner's, 2026-10-31), or, when nothing
// more will come to it, written off (Claude's proposal, which the owner has
// not confirmed). A statement with no billing account is on no invoice:
// nobody is there to bill. The line is unique by its statement, so a
// statement is on one invoice and never on another, however often or
// wherever the loop runs -- what reconciliation (#13) relies on.
//
// # Amounts
//
// An invoice's subtotal is its statements' totals added. On a channel levyan
// sells on itself a seller's tax is added to it ([Invoices.Tax]), toward
// nothing to the minor unit; a merchant of record adds its own. What is owed
// back -- an invoice that comes to less than nothing, a credit note that
// takes back more than is due -- is never paid out by itself: it is the
// billing account's credit balance in the product (`Credit`), taken off its
// next invoices before anything is asked of the channel. A channel sees only
// what is left to collect, and money goes back only through a credit note
// against an invoice that was paid.
//
// # Moving money
//
// What an open invoice has due is collected through its channel's
// [Collector]: by attempts levyan sends, on a channel that pushes (a card, a
// subscription), or by a transfer an operator records, on one that does not.
// Each attempt has its own key, and each send of it is written down and
// committed before it is made, so a send whose answer never came is known to
// be out; it is then asked about, and sent again only once the channel says
// it never had it. A decline ends the attempt and leaves the invoice open:
// trying again is dunning's (#11).
//
// # Before collection began
//
// A channel that was billed in the shadow before levyan collected on it --
// the merchant of record's, through shadow billing (#8) -- says when
// collection on it begins ([Invoices.LiveFrom]). A draft whose first period
// ended before then is closed as it is, a shadow: never finalized, nothing
// asked of the channel, nothing taken of the credit balance, and never shown
// to its payer, while an operator sees it as any other. A shadow's draft
// gathers no statement of a period that ends from then on, so that nothing
// collected is ever on one (Claude's, 2026-10-31, open question 63).
//
// # Written through the servers
//
// Every write goes through the stack it is handed -- the deployment's own
// (`cmd.Server.Ungated`), as whoever the context is framed as: the
// deployment, or an operator by their row -- so the trail says who did what.
// What decides a write is read with ent on the transaction it is written in,
// and every write of a billing account's invoices holds the billing
// account's row first, so two never decide on one balance at once.
package invoices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/ent/dialect/sql"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"

	"github.com/lesomnus/payday/config"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	"github.com/lesomnus/levyan/server/ledger"
)

// DefaultTax is the seller's tax on a channel levyan sells on itself when
// `invoices.tax` says nothing: Korea's VAT, a tenth (부가가치세법 제30조).
var DefaultTax = big.NewRat(1, 10)

// Invoices keeps products' invoices and the money they move, through S.
type Invoices struct {
	// S is the stack every row is written through: the deployment's own
	// (`cmd.Server.Ungated`), as whoever the context is framed as.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time

	// Collectors is how each channel's invoices are collected. A channel
	// with none has its drafts kept and never finalized.
	Collectors map[levyan.Channel]Collector

	// LiveFrom is when collection on a channel begins, for a channel that
	// was billed in the shadow before (`invoices.mor.live_from`): a draft of
	// it whose first period ended before then is closed as a shadow, never
	// collected, and only a period ending at or after it is finalized and
	// collected (Claude's, 2026-10-31, open question 63). A channel with none
	// collects from its first period.
	LiveFrom map[levyan.Channel]time.Time

	// Tax is what of an invoice's subtotal a seller's tax is, on a channel
	// levyan sells on itself ([Seller]): [DefaultTax] when nil.
	Tax *big.Rat
}

func (v Invoices) now() time.Time {
	if v.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(v.Now())
}

func (v Invoices) tax() *big.Rat {
	if v.Tax == nil {
		return DefaultTax
	}

	return v.Tax
}

func (v Invoices) postgres() bool { return v.Dialect == config.DialectPostgres }

func (v Invoices) db() *ent.Client { return ent.NewClient(ent.Driver(v.Drv)) }

// collector is what collects on `channel`, by its name, and nil for none.
func (v Invoices) collector(channel string) (levyan.Channel, Collector) {
	c := levyan.Channel(levyan.Channel_value[channel])
	return c, v.Collectors[c]
}

// errSkip ends a transaction that found nothing left to do, unwritten.
var errSkip = errors.New("invoices: nothing to do")

// tx runs `f` in one transaction: the stack and an ent client on it, and
// what it wrote committed unless it fails.
func (v Invoices) tx(ctx context.Context, f func(s levyan.Server, db *ent.Client, drv dialect.Driver) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, v.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(v.S, tdrv)
	if err != nil {
		return err
	}
	if err := f(s, ent.NewClient(ent.Driver(tdrv)), tdrv); err != nil {
		if errors.Is(err, errSkip) {
			return nil
		}
		return err
	}

	return tx.Commit()
}

// held is `f` in a transaction that holds billing account `ba`'s row first:
// every write of its invoices, its credit and its attempts does, so that two
// never decide on one balance, one draft or one attempt at once.
func (v Invoices) held(ctx context.Context, ba uuid.UUID, f func(s levyan.Server, db *ent.Client, drv dialect.Driver) error) error {
	return v.tx(ctx, func(s levyan.Server, db *ent.Client, drv dialect.Driver) error {
		if err := Lock(ctx, db, v.postgres(), ba); err != nil {
			return err
		}

		return f(s, db, drv)
	})
}

// Lock holds billing account `id`'s row until the transaction `db` is on
// ends: `FOR UPDATE` on PostgreSQL. SQLite runs one writer at a time, which
// holds the whole database from the first statement on, so there it asks
// nothing.
func Lock(ctx context.Context, db *ent.Client, postgres bool, id uuid.UUID) error {
	if !postgres {
		return nil
	}

	q := db.BillingAccount.Query().Where(entbillingaccount.Id(id))
	q.Modify(func(s *sql.Selector) { s.ForUpdate() })
	_, err := q.Ids(ctx)

	return err
}

// Balance is what billing account `ba` is owed in `product`, in its
// currency's minor unit: what its credit's movements add up to.
func Balance(ctx context.Context, db *ent.Client, product, ba uuid.UUID) (int64, error) {
	var vs []struct {
		Sum sql.NullInt64 `json:"sum"`
	}
	err := db.Credit.Query().
		Where(entcredit.TenantId(product), entcredit.BillingAccountId(ba)).
		Aggregate(ent.As(ent.Sum(entcredit.FieldAmount), "sum")).
		Scan(ctx, &vs)
	if err != nil {
		return 0, err
	}
	if len(vs) == 0 || !vs[0].Sum.Valid {
		return 0, nil
	}

	return vs[0].Sum.Int64, nil
}

// credit moves billing account `ba`'s balance in `product` by `amount`, for
// `kind`, through `s`: of `invoice` and of credit note `cn` when either is
// set.
func credit(ctx context.Context, s levyan.Server, product, ba uuid.UUID, currency string, amount int64, kind levyan.CreditKind, invoice, cn uuid.UUID) error {
	if amount == 0 {
		return nil
	}
	req := levyan.CreditAddRequest_builder{
		Tenant:         levyan.TenantRef_builder{Id: product[:]}.Build(),
		BillingAccount: levyan.BillingAccountRef_builder{Id: ba[:]}.Build(),
		Amount:         amount,
		Currency:       currency,
		Kind:           kind,
	}.Build()
	if invoice != (uuid.UUID{}) {
		req.SetInvoice(levyan.InvoiceRef_builder{Id: invoice[:]}.Build())
	}
	if cn != (uuid.UUID{}) {
		req.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn[:]}.Build())
	}
	if _, err := s.Credit().Add(ctx, req); err != nil {
		return fmt.Errorf("credit: %w", err)
	}

	return nil
}

// TaxOf is the tax of `amount` at `rate`: toward nothing, to the minor unit,
// so that a credit's is the same as a charge's of the same amount, less than
// nothing (implementation decision; docs/design.md §15).
func TaxOf(rate *big.Rat, amount int64) int64 {
	if rate == nil || rate.Sign() == 0 || amount == 0 {
		return 0
	}
	neg := amount < 0
	if neg {
		amount = -amount
	}
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(amount), rate)
	n := new(big.Int).Quo(v.Num(), v.Denom())
	if neg {
		n.Neg(n)
	}

	return n.Int64()
}

// remaining is what is left of an invoice's due: due, less what was paid
// and what credit notes took off it.
func remaining(inv *ent.Invoice) int64 { return max(inv.Due-inv.Paid-inv.Reduced, 0) }

// Remaining is what is left to collect of `inv`.
func Remaining(inv *ent.Invoice) int64 { return remaining(inv) }

func logErr(ctx context.Context, msg string, err error, attrs ...any) {
	log.From(ctx).ErrorContext(ctx, msg, append(attrs, slog.Any("err", err))...)
}
