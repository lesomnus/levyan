package invoices

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
	"uuid"

	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// Credit is what a credit note is asked for.
type Credit struct {
	// Reason is CORRECTION or REFUND; a CANCELLATION is a void's.
	Reason levyan.CreditNoteReason

	// Amount is how much of the invoice's subtotal it takes back, in the
	// currency's minor unit: its tax is worked out from it.
	Amount int64

	// Refund gives back through the channel what it takes back past what
	// is still due, as far as the invoice was paid; the rest is the billing
	// account's credit. A REFUND always does.
	Refund bool

	// Words is why, in its writer's words.
	Words string

	// Reference and At are a transfer an operator made back by hand, on a
	// channel that moves no money itself: the bank's reference, and the day.
	Reference string
	At        time.Time
}

// finalized is an invoice's states a credit note may be written against.
var finalized = []levyan.InvoiceState{
	levyan.InvoiceState_INVOICE_STATE_OPEN,
	levyan.InvoiceState_INVOICE_STATE_PAID,
	levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE,
}

// Credit writes a credit note against finalized invoice `id`, as whoever
// `ctx` is framed as: it takes `r.Amount` of the subtotal back with its tax,
// off what is still due first, and gives back or credits the rest.
func (v Invoices) Credit(ctx context.Context, id pdid.Id, r Credit) (*ent.CreditNote, error) {
	switch r.Reason {
	case levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION:
	case levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND:
		r.Refund = true
	default:
		return nil, status.Error(codes.InvalidArgument, "reason: a correction or a refund; a cancellation is the invoice voided")
	}
	inv, err := v.db().Invoice.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no invoice is %s", id)
	}
	if err != nil {
		return nil, err
	}

	var out *ent.CreditNote
	err = v.held(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, drv dialect.Driver) error {
		inv, err := db.Invoice.Get(ctx, id.Uuid())
		if err != nil {
			return err
		}
		if !is(inv, finalized...) {
			return status.Errorf(codes.FailedPrecondition, "a credit note is written against an invoice once it is finalized, and this one is %s", stateName(inv.State))
		}
		if err := quiet(ctx, db, inv); err != nil {
			return err
		}

		out, err = v.note(ctx, s, db, drv, inv, r)
		return err
	})

	return out, err
}

// Void voids invoice `id`, as whoever `ctx` is framed as: nothing of it is
// owed from now on. What is left of it is taken back by a credit note of its
// cancellation -- on a channel levyan sells on, with the amended tax invoice
// behind it -- off what is due, and what credit paid of it is the billing
// account's again. An invoice paid in any part is not voided: a credit note
// gives that back.
func (v Invoices) Void(ctx context.Context, id pdid.Id, words string) (*ent.Invoice, error) {
	inv, err := v.db().Invoice.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no invoice is %s", id)
	}
	if err != nil {
		return nil, err
	}

	err = v.held(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, drv dialect.Driver) error {
		inv, err := db.Invoice.Get(ctx, id.Uuid())
		if err != nil {
			return err
		}
		if !is(inv, levyan.InvoiceState_INVOICE_STATE_OPEN, levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE) {
			return status.Errorf(codes.FailedPrecondition, "an invoice is voided while it is open, and this one is %s", stateName(inv.State))
		}
		if inv.Paid > 0 {
			return status.Errorf(codes.FailedPrecondition, "%d of it was paid: a credit note gives that back, and voids nothing", inv.Paid)
		}
		if err := quiet(ctx, db, inv); err != nil {
			return err
		}
		if err := v.unsend(ctx, s, db, inv); err != nil {
			return err
		}

		left, _, _, err := taken(ctx, db, inv)
		if err != nil {
			return err
		}
		if left > 0 {
			if _, err := v.note(ctx, s, db, drv, inv, Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION, Amount: left, Words: words}); err != nil {
				return err
			}
		}

		_, err = s.Invoice().Patch(ctx, levyan.InvoicePatchRequest_builder{
			Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			State:            levyan.InvoiceState_INVOICE_STATE_VOID.Enum(),
			DateSettled:      timestamppb.New(v.now()),
			Reason:           proto.String(strings.TrimSpace(words)),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		return err
	})
	if err != nil {
		return nil, err
	}

	return v.db().Invoice.Get(ctx, id.Uuid())
}

// Record writes down a transfer an operator saw come for invoice `id` --
// `amount` of its currency's minor unit, on day `at`, by the bank's
// `reference` -- as an attempt that succeeded and was never sent: on a
// channel that moves no money itself, and on no other.
func (v Invoices) Record(ctx context.Context, id pdid.Id, amount int64, at time.Time, reference string) (*ent.PaymentAttempt, error) {
	if amount <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount: what came, more than nothing")
	}
	inv, err := v.db().Invoice.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no invoice is %s", id)
	}
	if err != nil {
		return nil, err
	}
	switch _, c := v.collector(inv.Channel); {
	case c == nil:
		return nil, status.Errorf(codes.FailedPrecondition, "nothing collects on %s", inv.Channel)
	case c.Pushes():
		return nil, status.Errorf(codes.FailedPrecondition, "%s collects what is due itself: a payment is recorded only on a channel that moves no money", inv.Channel)
	}
	now := v.now()
	if at.IsZero() {
		at = now
	}
	if ledger.Moment(at).After(now) {
		return nil, status.Errorf(codes.InvalidArgument, "date: %s is ahead of now", at.Format(time.DateOnly))
	}

	var out *ent.PaymentAttempt
	err = v.held(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		inv, err := db.Invoice.Get(ctx, id.Uuid())
		if err != nil {
			return err
		}
		if !is(inv, levyan.InvoiceState_INVOICE_STATE_OPEN, levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE) {
			return status.Errorf(codes.FailedPrecondition, "a payment is recorded for an open invoice, and this one is %s", stateName(inv.State))
		}

		a, err := recorded(ctx, s, inv, levyan.AttemptKind_ATTEMPT_KIND_CHARGE, amount, uuid.UUID{}, at, reference, now)
		if err != nil {
			return err
		}
		if err := v.paid(ctx, s, db, inv.Id, amount, Outcome{At: at}, now); err != nil {
			return err
		}
		out, err = db.PaymentAttempt.Get(ctx, a)
		return err
	})

	return out, err
}

// recorded writes an attempt an operator saw succeed, never sent: of `kind`,
// for `amount` of invoice `inv`, on day `at` by `reference`.
func recorded(ctx context.Context, s levyan.Server, inv *ent.Invoice, kind levyan.AttemptKind, amount int64, cn uuid.UUID, at time.Time, reference string, now time.Time) (uuid.UUID, error) {
	id := pdid.New(pd.PaymentAttemptDomain)
	req := levyan.PaymentAttemptAddRequest_builder{
		Id:           id.Bytes(),
		Tenant:       levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
		Invoice:      levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Kind:         kind,
		AttemptKey:   KeyOf(id),
		Channel:      inv.Channel,
		Currency:     inv.Currency,
		Amount:       amount,
		State:        levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED,
		Recorded:     true,
		Reference:    strings.TrimSpace(reference),
		DatePaid:     timestamppb.New(ledger.Moment(at)),
		DateResolved: timestamppb.New(now),
	}.Build()
	if cn != (uuid.UUID{}) {
		req.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn[:]}.Build())
	}
	if _, err := s.PaymentAttempt().Add(ctx, req); err != nil {
		return uuid.UUID{}, fmt.Errorf("attempt: %w", err)
	}

	return id.Uuid(), nil
}

// note writes a credit note of `r` against invoice `inv`, in the
// transaction `db` is on, with what it does: what it takes off what is due,
// gives back, and credits.
func (v Invoices) note(ctx context.Context, s levyan.Server, db *ent.Client, drv dialect.Driver, inv *ent.Invoice, r Credit) (*ent.CreditNote, error) {
	channel, c := v.collector(inv.Channel)
	left, leftTax, refunded, err := taken(ctx, db, inv)
	if err != nil {
		return nil, err
	}
	if r.Amount <= 0 || r.Amount > left {
		return nil, status.Errorf(codes.InvalidArgument, "amount: more than nothing, and no more than the %d of the invoice's subtotal not taken back yet", left)
	}

	tax := int64(0)
	if Seller(channel) {
		if r.Amount == left {
			tax = leftTax
		} else {
			rate, ok := new(big.Rat).SetString(inv.TaxRate)
			if !ok {
				rate = new(big.Rat)
			}
			tax = TaxOf(rate, r.Amount)
		}
	}
	total := r.Amount + tax

	reduced := int64(0)
	if is(inv, levyan.InvoiceState_INVOICE_STATE_OPEN, levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE) {
		reduced = min(total, remaining(inv))
	}
	rest := total - reduced
	back := int64(0)
	if r.Refund {
		back = min(rest, max(inv.Paid-refunded, 0))
	}
	credited := rest - back

	if back > 0 && (c == nil || !c.Pushes()) && strings.TrimSpace(r.Reference) == "" {
		return nil, status.Error(codes.InvalidArgument, "reference: the channel moves no money itself, so what is given back is a transfer made by hand, and is recorded by its reference and day")
	}

	id := pdid.New(pd.CreditNoteDomain)
	req := levyan.CreditNoteAddRequest_builder{
		Id:             id.Bytes(),
		Tenant:         levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
		Invoice:        levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		BillingAccount: levyan.BillingAccountRef_builder{Id: inv.BillingAccountId[:]}.Build(),
		Reason:         r.Reason,
		Amount:         r.Amount,
		Tax:            tax,
		Total:          total,
		Reduced:        reduced,
		Refunded:       back,
		Credited:       credited,
		Channel:        inv.Channel,
		Currency:       inv.Currency,
		Words:          strings.TrimSpace(r.Words),
	}.Build()
	if Seller(channel) {
		n, err := Next(ctx, db, drv, v.postgres(), entcreditnote.Table)
		if err != nil {
			return nil, err
		}
		req.SetNumber(n)
	}
	if _, err := s.CreditNote().Add(ctx, req); err != nil {
		return nil, fmt.Errorf("credit note: %w", err)
	}

	now := v.now()
	if credited > 0 {
		if err := credit(ctx, s, inv.TenantId, inv.BillingAccountId, inv.Currency, credited, levyan.CreditKind_CREDIT_KIND_CREDIT_NOTE, inv.Id, id.Uuid()); err != nil {
			return nil, err
		}
	}
	if reduced > 0 {
		req := levyan.InvoicePatchRequest_builder{
			Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			Reduced:          proto.Int64(inv.Reduced + reduced),
			DateUpdatedForce: proto.Bool(true),
		}.Build()
		if reduced == remaining(inv) && r.Reason != levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION {
			req.SetState(levyan.InvoiceState_INVOICE_STATE_PAID)
			req.SetDateSettled(timestamppb.New(now))
		}
		if _, err := s.Invoice().Patch(ctx, req); err != nil {
			return nil, fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
		}
	}
	if back > 0 {
		if c != nil && c.Pushes() {
			if _, err := addAttempt(ctx, s, inv, levyan.AttemptKind_ATTEMPT_KIND_REFUND, back, id.Uuid()); err != nil {
				return nil, err
			}
		} else {
			at := r.At
			if at.IsZero() {
				at = now
			}
			if _, err := recorded(ctx, s, inv, levyan.AttemptKind_ATTEMPT_KIND_REFUND, back, id.Uuid(), at, r.Reference, now); err != nil {
				return nil, err
			}
		}
	}

	return db.CreditNote.Get(ctx, id.Uuid())
}

// taken is what of invoice `inv` credit notes have not taken back yet -- of
// its subtotal and of its tax -- and what they gave back through the
// channel.
func taken(ctx context.Context, db *ent.Client, inv *ent.Invoice) (int64, int64, int64, error) {
	cns, err := db.CreditNote.Query().Where(entcreditnote.InvoiceId(inv.Id)).All(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	amount, tax, back := inv.Subtotal, inv.Tax, int64(0)
	for _, cn := range cns {
		amount -= cn.Amount
		tax -= cn.Tax
		back += cn.Refunded
	}

	return max(amount, 0), max(tax, 0), back, nil
}

// quiet refuses a change of an invoice while an attempt to charge it is out:
// what came of it is found out first.
func quiet(ctx context.Context, db *ent.Client, inv *ent.Invoice) error {
	out, err := db.PaymentAttempt.Query().
		Where(
			entattempt.InvoiceId(inv.Id),
			entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)),
			entattempt.Or(
				entattempt.StateIn(int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN), int32(levyan.AttemptState_ATTEMPT_STATE_PENDING)),
				entattempt.And(entattempt.State(int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED)), entattempt.SendsGT(0)),
			),
		).
		Exist(ctx)
	if err != nil {
		return err
	}
	if out {
		return status.Error(codes.FailedPrecondition, "an attempt to charge the invoice is out, and what came of it is found out first")
	}

	return nil
}

// unsend ends every attempt to charge invoice `inv` that was never sent: it
// is voided, and they are to be sent no more.
func (v Invoices) unsend(ctx context.Context, s levyan.Server, db *ent.Client, inv *ent.Invoice) error {
	as, err := db.PaymentAttempt.Query().
		Where(
			entattempt.InvoiceId(inv.Id),
			entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)),
			entattempt.State(int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED)),
			entattempt.Sends(0),
		).
		All(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		if _, err := s.PaymentAttempt().Patch(ctx, levyan.PaymentAttemptPatchRequest_builder{
			Ref:              levyan.PaymentAttemptRef_builder{Id: a.Id[:]}.Build(),
			State:            levyan.AttemptState_ATTEMPT_STATE_FAILED.Enum(),
			Failure:          FailureOf(Invalid, 0, "voided", "the invoice was voided before it was sent"),
			DateResolved:     timestamppb.New(v.now()),
			DateUpdatedForce: proto.Bool(true),
		}.Build()); err != nil {
			return fmt.Errorf("attempt %s: %w", pdid.Id(a.Id), err)
		}
	}

	return nil
}

// is says whether invoice `inv` is in one of `states`.
func is(inv *ent.Invoice, states ...levyan.InvoiceState) bool {
	for _, s := range states {
		if inv.State == int32(s) {
			return true
		}
	}

	return false
}

// stateName is a state as a sentence says it: "open".
func stateName(v int32) string {
	return strings.ToLower(strings.TrimPrefix(levyan.InvoiceState(v).String(), "INVOICE_STATE_"))
}
