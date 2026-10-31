package invoices

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// Unlined is a statement on no invoice yet: no line names it.
func Unlined() predicate.Statement {
	return predicate.Statement(func(s *entsql.Selector) {
		t := entsql.Table(entinvoiceline.Table)
		s.Where(entsql.Not(entsql.Exists(
			entsql.Select(t.C(entinvoiceline.FieldId)).From(t).
				Where(entsql.ColumnsEQ(t.C(entinvoiceline.FieldStatementId), s.C(entstatement.FieldId))),
		)))
	})
}

// pair is a billing account of a product.
type pair struct{ product, account uuid.UUID }

// Gather is what the loop goes through first: every billing account of a
// product that has a statement on no invoice, or a draft.
func (v Invoices) pairs(ctx context.Context) ([]pair, error) {
	seen := map[pair]bool{}
	out := []pair{}
	keep := func(p pair) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	sts, err := v.db().Statement.Query().
		Where(entstatement.BillingAccountIdNotNil(), Unlined()).
		Select(entstatement.FieldTenantId, entstatement.FieldBillingAccountId).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range sts {
		keep(pair{s.TenantId, s.BillingAccountId})
	}

	ds, err := v.db().Invoice.Query().
		Where(entinvoice.State(int32(levyan.InvoiceState_INVOICE_STATE_DRAFT))).
		Select(entinvoice.FieldTenantId, entinvoice.FieldBillingAccountId).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		keep(pair{d.TenantId, d.BillingAccountId})
	}

	slices.SortFunc(out, func(a, b pair) int {
		if c := slices.Compare(a.product[:], b.product[:]); c != 0 {
			return c
		}
		return slices.Compare(a.account[:], b.account[:])
	})

	return out, nil
}

// Account puts every statement of billing account `ba` in `product` that is
// on no invoice yet on its draft -- one begun, if it has none -- and decides
// each draft whose periods are all closed: finalized, kept for the next
// period, or written off. A statement of a later period than its draft
// covers waits until the draft is decided, so that a product closing several
// periods at once still has an invoice of each.
func (v Invoices) Account(ctx context.Context, product, ba uuid.UUID) error {
	return v.held(ctx, ba, func(s levyan.Server, db *ent.Client, drv dialect.Driver) error {
		acct, err := db.BillingAccount.Get(ctx, ba)
		if err != nil {
			return err
		}
		sts, err := db.Statement.Query().
			Where(entstatement.TenantId(product), entstatement.BillingAccountId(ba), Unlined()).
			WithWorkspace().
			Order(ent.Asc(entstatement.FieldDateStart), ent.Asc(entstatement.FieldId)).
			All(ctx)
		if err != nil {
			return err
		}
		draft, err := draftOf(ctx, db, product, ba)
		if err != nil {
			return err
		}

		for _, st := range sts {
			if st.Channel != acct.Channel || st.Currency != acct.Currency {
				// Never: a statement is priced on its billing account's
				// channel and currency, which never change. Said, and left
				// on no invoice, for reconciliation to find.
				log.From(ctx).ErrorContext(ctx, "invoices: a statement is priced on other terms than its billing account's, and is put on no invoice",
					slog.String("statement", pdid.Id(st.Id).String()), slog.String("billing_account", pdid.Id(ba).String()))
				continue
			}
			if draft != nil && !st.DateStart.Before(draft.DateEnd) {
				done, err := v.decide(ctx, s, db, drv, draft, acct)
				if err != nil {
					return err
				}
				if done {
					draft = nil
				}
			}
			if draft == nil {
				if draft, err = begin(ctx, s, db, st, acct); err != nil {
					return err
				}
			}
			if err := add(ctx, s, draft, st); err != nil {
				return err
			}
		}
		if draft == nil {
			return nil
		}

		_, err = v.decide(ctx, s, db, drv, draft, acct)
		return err
	})
}

// draftOf is the draft of billing account `ba` in `product`, or nil.
func draftOf(ctx context.Context, db *ent.Client, product, ba uuid.UUID) (*ent.Invoice, error) {
	vs, err := db.Invoice.Query().
		Where(
			entinvoice.TenantId(product),
			entinvoice.BillingAccountId(ba),
			entinvoice.State(int32(levyan.InvoiceState_INVOICE_STATE_DRAFT)),
		).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(ctx)
	if err != nil || len(vs) == 0 {
		return nil, err
	}
	if len(vs) > 1 {
		// Never, since every write of one holds the billing account: the
		// earliest gathers, and the rest are said.
		log.From(ctx).ErrorContext(ctx, "invoices: a billing account has more than one draft in a product", slog.String("billing_account", pdid.Id(ba).String()), slog.Int("drafts", len(vs)))
	}

	return vs[0], nil
}

// begin is a draft of statement `st`'s billing account, begun for its
// period.
func begin(ctx context.Context, s levyan.Server, db *ent.Client, st *ent.Statement, acct *ent.BillingAccount) (*ent.Invoice, error) {
	v, err := s.Invoice().Add(ctx, levyan.InvoiceAddRequest_builder{
		Tenant:         levyan.TenantRef_builder{Id: st.TenantId[:]}.Build(),
		BillingAccount: levyan.BillingAccountRef_builder{Id: acct.Id[:]}.Build(),
		Period:         levyan.PeriodRef_builder{Id: st.PeriodId[:]}.Build(),
		DateStart:      timestamppb.New(st.DateStart),
		DateEnd:        timestamppb.New(st.DateEnd),
		Channel:        acct.Channel,
		Currency:       acct.Currency,
		State:          levyan.InvoiceState_INVOICE_STATE_DRAFT,
	}.Build())
	if err != nil {
		return nil, fmt.Errorf("invoice: %w", err)
	}
	id, err := pdid.From(v.GetId())
	if err != nil {
		return nil, err
	}

	return db.Invoice.Get(ctx, id.Uuid())
}

// add puts statement `st` on draft `inv` as a line, and the draft's
// subtotal and end with it.
func add(ctx context.Context, s levyan.Server, inv *ent.Invoice, st *ent.Statement) error {
	label := ""
	if w := st.Edges.Workspace; w != nil {
		label = w.Label
	}
	_, err := s.InvoiceLine().Add(ctx, levyan.InvoiceLineAddRequest_builder{
		Tenant:    levyan.TenantRef_builder{Id: st.TenantId[:]}.Build(),
		Workspace: levyan.WorkspaceRef_builder{Id: st.WorkspaceId[:]}.Build(),
		Invoice:   levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Statement: levyan.StatementRef_builder{Id: st.Id[:]}.Build(),
		Label:     label,
		DateStart: timestamppb.New(st.DateStart),
		DateEnd:   timestamppb.New(st.DateEnd),
		Total:     st.Total,
	}.Build())
	if err != nil {
		return fmt.Errorf("statement %s: %w", pdid.Id(st.Id), err)
	}

	end := inv.DateEnd
	if st.DateEnd.After(end) {
		end = st.DateEnd
	}
	u, err := s.Invoice().Patch(ctx, levyan.InvoicePatchRequest_builder{
		Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Subtotal:         proto.Int64(inv.Subtotal + st.Total),
		DateEnd:          timestamppb.New(end),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	if err != nil {
		return fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
	}
	inv.Subtotal = u.GetSubtotal()
	inv.DateEnd = ledger.Moment(u.GetDateEnd().AsTime())

	return nil
}

// decide decides draft `inv` once every period it covers is closed, and
// answers whether it is a draft no longer:
//
//   - on a channel nothing collects on, it is kept as it is, and said;
//   - coming to nothing or less, it is finalized as settled, and what is
//     less than nothing is the billing account's credit;
//   - its credit balance taken off, what is left to ask for is more than
//     nothing and less than its channel collects: it waits for the next
//     period (the owner's, 2026-10-31) -- or, when nothing more will come to
//     it, every link of its billing account having ended, it is written off
//     (Claude's proposal, which the owner has not confirmed);
//   - otherwise it is finalized: what credit paid of it taken from the
//     balance, its bill-to kept, numbered on a channel levyan sells on, and
//     open for what is left, or settled when nothing is.
func (v Invoices) decide(ctx context.Context, s levyan.Server, db *ent.Client, drv dialect.Driver, inv *ent.Invoice, acct *ent.BillingAccount) (bool, error) {
	closed, err := db.Period.Query().
		Where(entperiod.TenantId(inv.TenantId), entperiod.DateEnd(inv.DateEnd), entperiod.DateClosedNotNil()).
		Exist(ctx)
	if err != nil || !closed {
		return false, err
	}
	channel, c := v.collector(inv.Channel)
	if c == nil {
		log.From(ctx).WarnContext(ctx, "invoices: nothing collects on the channel, so its drafts wait",
			slog.String("channel", inv.Channel), slog.String("invoice", pdid.Id(inv.Id).String()))
		return false, nil
	}

	now := v.now()
	seller := Seller(channel)
	rate := new(big.Rat)
	if seller {
		rate = v.tax()
	}
	tax := TaxOf(rate, inv.Subtotal)
	total := inv.Subtotal + tax

	req := levyan.InvoicePatchRequest_builder{
		Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		DateUpdatedForce: proto.Bool(true),
	}.Build()

	credited, due := int64(0), int64(0)
	if total > 0 {
		bal, err := Balance(ctx, db, inv.TenantId, inv.BillingAccountId)
		if err != nil {
			return false, err
		}
		credited = min(max(bal, 0), total)
		due = total - credited

		if least := c.Minimum(inv.Currency); due > 0 && due < least {
			more, err := more(ctx, db, inv)
			if err != nil {
				return false, err
			}
			if !more {
				req.SetState(levyan.InvoiceState_INVOICE_STATE_WRITTEN_OFF)
				req.SetDateSettled(timestamppb.New(now))
				req.SetReason(fmt.Sprintf("less than the channel collects (%d), and nothing more will come", least))
				if _, err := s.Invoice().Patch(ctx, req); err != nil {
					return false, fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
				}
				return true, nil
			}
			if inv.DateCarried != nil && inv.DateCarried.Equal(inv.DateEnd) {
				// It waited at this end already.
				return false, nil
			}
			req.SetCarried(inv.Carried + 1)
			req.SetDateCarried(timestamppb.New(inv.DateEnd))
			if _, err := s.Invoice().Patch(ctx, req); err != nil {
				return false, fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
			}
			return false, nil
		}
	}

	if total < 0 {
		if err := credit(ctx, s, inv.TenantId, inv.BillingAccountId, inv.Currency, -total, levyan.CreditKind_CREDIT_KIND_NEGATIVE, inv.Id, uuid.UUID{}); err != nil {
			return false, err
		}
	}
	if credited > 0 {
		if err := credit(ctx, s, inv.TenantId, inv.BillingAccountId, inv.Currency, -credited, levyan.CreditKind_CREDIT_KIND_APPLIED, inv.Id, uuid.UUID{}); err != nil {
			return false, err
		}
	}

	req.SetTax(tax)
	req.SetTaxRate(rate.RatString())
	req.SetTotal(total)
	req.SetCredited(credited)
	req.SetDue(due)
	req.SetDateFinalized(timestamppb.New(now))
	req.SetBillTo(BillToOf(acct))
	if due > 0 {
		req.SetState(levyan.InvoiceState_INVOICE_STATE_OPEN)
		if d := c.Due(now); !d.IsZero() {
			req.SetDateDue(timestamppb.New(ledger.Moment(d)))
		}
	} else {
		req.SetState(levyan.InvoiceState_INVOICE_STATE_PAID)
		req.SetDateSettled(timestamppb.New(now))
	}
	if seller && total > 0 {
		n, err := Next(ctx, db, drv, v.postgres(), entinvoice.Table)
		if err != nil {
			return false, err
		}
		req.SetNumber(n)
	}
	if _, err := s.Invoice().Patch(ctx, req); err != nil {
		return false, fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
	}

	return true, nil
}

// more says whether anything more will come to draft `inv`: whether a link
// of its billing account in its product is in force after the end of what
// it covers.
func more(ctx context.Context, db *ent.Client, inv *ent.Invoice) (bool, error) {
	ls, err := db.Link.Query().
		Where(
			entlink.TenantId(inv.TenantId),
			entlink.BillingAccountId(inv.BillingAccountId),
			entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(inv.DateEnd)),
		).
		All(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range ls {
		if l.DateEnd == nil || l.DateEnd.After(l.DateStart) {
			return true, nil
		}
	}

	return false, nil
}

// BillToOf is who billing account `a` bills, as it says now.
func BillToOf(a *ent.BillingAccount) *levyan.BillTo {
	out := levyan.BillTo_builder{
		Name:      a.Name,
		Country:   a.Country,
		Recipient: a.Recipient,
		Email:     a.Email,
		Address:   a.Address,
	}.Build()
	if a.Business != nil {
		out.SetBusiness(proto.CloneOf(a.Business))
	}

	return out
}

// numbering is the key PostgreSQL holds a seller's numbers by while one is
// given, so that two finalized at once are given one after another.
const numbering = 0x6c76696e // "lvin"

// Next is the next of a seller's numbers in `table` -- invoices' or credit
// notes', each a series of its own across the deployment -- given in the
// transaction `db` is on: one more than the greatest given, so that numbers
// follow one another, and none is given again. On PostgreSQL the series is
// held until the transaction ends.
func Next(ctx context.Context, db *ent.Client, drv dialect.Driver, postgres bool, table string) (int64, error) {
	if postgres {
		key := numbering
		if table != entinvoice.Table {
			key++
		}
		var rows entsql.Rows
		if err := drv.Query(ctx, "SELECT pg_advisory_xact_lock($1)", []any{key}, &rows); err != nil {
			return 0, err
		}
		rows.Close()
	}

	var vs []struct {
		Most sql.NullInt64 `json:"most"`
	}
	var err error
	switch table {
	case entinvoice.Table:
		err = db.Invoice.Query().Aggregate(ent.As(ent.Max(entinvoice.FieldNumber), "most")).Scan(ctx, &vs)
	default:
		err = db.CreditNote.Query().Aggregate(ent.As(ent.Max("number"), "most")).Scan(ctx, &vs)
	}
	if err != nil {
		return 0, err
	}
	if len(vs) == 0 || !vs[0].Most.Valid {
		return 1, nil
	}

	return vs[0].Most.Int64 + 1, nil
}

// Description is what the payer sees an invoice of `product` as: the
// product's name and the months it covers in its zone, "loft 2027-01".
func Description(product *ent.Tenant, start, end time.Time) string {
	name := strings.TrimSpace(product.Name)
	if name == "" {
		name = product.Alias
	}
	zone, err := billing.Zone(product.Zone)
	if err != nil {
		zone = time.UTC
	}
	from := start.In(zone).Format("2006-01")
	to := end.Add(-time.Microsecond).In(zone).Format("2006-01")
	if from == to {
		return name + " " + from
	}

	return name + " " + from + " – " + to
}
