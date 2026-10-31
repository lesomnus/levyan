package invoices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// Loop is the name of the loop that keeps invoices.
const Loop = "levyan.invoices.collect"

// DefaultEvery is how often the loop runs when `invoices.every` says nothing:
// as often as a merchant of record lets a charge whose answer never came be
// asked about (`mor.Settle`).
const DefaultEvery = 10 * time.Minute

// Run is one pass of the loop: every statement on no invoice put on its
// draft and every draft whose period is closed decided ([Invoices.Account]),
// the legal documents of what was finalized issued ([Invoices.Issue]), and
// what is due moved ([Invoices.Collect]). What fails is said in the log and
// tried again by the next pass; one billing account's does not stop
// another's.
func (v Invoices) Run(ctx context.Context) error {
	var errs []error

	ps, err := v.pairs(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if err := v.Account(ctx, p.product, p.account); err != nil {
			logErr(ctx, "invoices: a billing account's statements were not invoiced", err,
				slog.String("product", pdid.Id(p.product).String()), slog.String("billing_account", pdid.Id(p.account).String()))
			errs = append(errs, err)
		}
	}
	if err := v.Issue(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := v.Collect(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Collects is the loop [Loop]: [Invoices.Run] every `every`, in one process.
func Collects(v *Invoices, every time.Duration) spin.Spinner {
	return spin.Called(Loop, spin.Every(every, func(ctx context.Context) error {
		if err := v.Run(ctx); err != nil {
			log.From(ctx).WarnContext(ctx, "invoices: a pass", slog.Any("err", err))
		}
		return nil
	}))
}

// Issue issues the legal document of every invoice and credit note numbered
// on a channel levyan sells on and not issued yet. Each is numbered, and the
// number committed, before its document is asked for -- the number is what
// keeps the document once at the issuer -- so one whose answer never came is
// asked for again under it. A credit note waits for its invoice's.
func (v Invoices) Issue(ctx context.Context) error {
	var errs []error

	invs, err := v.db().Invoice.Query().
		Where(entinvoice.NumberNotNil(), entinvoice.DateIssuedIsNil()).
		Order(ent.Asc(entinvoice.FieldNumber)).
		All(ctx)
	if err != nil {
		return err
	}
	for _, inv := range invs {
		if err := v.issue(ctx, inv); err != nil {
			logErr(ctx, "invoices: an invoice's legal document was not issued", err, slog.String("invoice", pdid.Id(inv.Id).String()))
			errs = append(errs, err)
		}
	}

	cns, err := v.db().CreditNote.Query().
		Where(entcreditnote.NumberNotNil(), entcreditnote.DateIssuedIsNil()).
		WithInvoice().
		Order(ent.Asc(entcreditnote.FieldNumber)).
		All(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, cn := range cns {
		if inv := cn.Edges.Invoice; inv == nil || inv.DateIssued == nil {
			continue
		}
		if err := v.amend(ctx, cn); err != nil {
			logErr(ctx, "invoices: a credit note's amended document was not issued", err, slog.String("credit_note", pdid.Id(cn.Id).String()))
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// issue issues invoice `inv`'s legal document, and writes down that it was.
func (v Invoices) issue(ctx context.Context, inv *ent.Invoice) error {
	_, c := v.collector(inv.Channel)
	if c == nil {
		return fmt.Errorf("nothing collects on %s", inv.Channel)
	}
	product, err := v.db().Tenant.Get(ctx, inv.TenantId)
	if err != nil {
		return err
	}

	got, err := c.Issue(ctx, Document{
		Number:   *inv.Number,
		Invoice:  invoiceOf(inv, product),
		Currency: inv.Currency,
		Supply:   inv.Subtotal,
		Tax:      inv.Tax,
		Rate:     inv.TaxRate,
		Written:  LastDay(product, inv.DateEnd),
		BillTo:   inv.BillTo,
	})
	if err != nil {
		return err
	}

	return v.issued(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, at time.Time) error {
		was, err := db.Invoice.Get(ctx, inv.Id)
		if err != nil || was.DateIssued != nil {
			return errors.Join(err, errSkip)
		}
		_, err = s.Invoice().Patch(ctx, levyan.InvoicePatchRequest_builder{
			Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			DateIssued:       timestamppb.New(issuedAt(got, at)),
			Document:         proto.String(got.Document),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		return err
	})
}

// amend issues the amended legal document behind credit note `cn`, and
// writes down that it was.
func (v Invoices) amend(ctx context.Context, cn *ent.CreditNote) error {
	inv := cn.Edges.Invoice
	_, c := v.collector(cn.Channel)
	if c == nil {
		return fmt.Errorf("nothing collects on %s", cn.Channel)
	}
	product, err := v.db().Tenant.Get(ctx, cn.TenantId)
	if err != nil {
		return err
	}
	original := int64(0)
	if inv.Number != nil {
		original = *inv.Number
	}

	got, err := c.Issue(ctx, Document{
		Number:     *cn.Number,
		CreditNote: true,
		Reason:     levyan.CreditNoteReason(cn.Reason),
		Original:   original,
		Document:   inv.Document,
		Invoice:    invoiceOf(inv, product),
		Currency:   cn.Currency,
		Supply:     -cn.Amount,
		Tax:        -cn.Tax,
		Rate:       inv.TaxRate,
		Written:    Day(product, cn.DateCreated),
		BillTo:     inv.BillTo,
	})
	if err != nil {
		return err
	}

	return v.issued(ctx, cn.BillingAccountId, func(s levyan.Server, db *ent.Client, at time.Time) error {
		was, err := db.CreditNote.Get(ctx, cn.Id)
		if err != nil || was.DateIssued != nil {
			return errors.Join(err, errSkip)
		}
		_, err = s.CreditNote().Patch(ctx, levyan.CreditNotePatchRequest_builder{
			Ref:              levyan.CreditNoteRef_builder{Id: cn.Id[:]}.Build(),
			DateIssued:       timestamppb.New(issuedAt(got, at)),
			Document:         proto.String(got.Document),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		return err
	})
}

func (v Invoices) issued(ctx context.Context, ba [16]byte, f func(s levyan.Server, db *ent.Client, at time.Time) error) error {
	return v.held(ctx, ba, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		return f(s, db, v.now())
	})
}

func issuedAt(got Issued, now time.Time) time.Time {
	if got.At.IsZero() {
		return now
	}

	return ledger.Moment(got.At)
}

// LastDay is the last day before `end` in `product`'s zone, at its first
// moment: what a month's tax invoice is written for.
func LastDay(product *ent.Tenant, end time.Time) time.Time {
	return Day(product, end.Add(-time.Microsecond))
}

// Day is the day `t` is in, in `product`'s zone, at its first moment.
func Day(product *ent.Tenant, t time.Time) time.Time {
	zone, err := billing.Zone(product.Zone)
	if err != nil {
		zone = time.UTC
	}
	u := t.In(zone)

	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, zone)
}
