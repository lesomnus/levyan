package invoices

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// live is the states of an attempt whose outcome is not settled.
var live = []int32{
	int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED),
	int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN),
	int32(levyan.AttemptState_ATTEMPT_STATE_PENDING),
}

// Collect moves what open invoices have due through the channels that push:
// an attempt of each that has none yet, and every attempt whose outcome is
// not settled taken one step on -- one not out is sent, and one out is asked
// about. An invoice whose attempt failed waits for dunning (#11), which
// decides whether to try again; nothing here does.
func (v Invoices) Collect(ctx context.Context) error {
	var errs []error

	open, err := v.db().Invoice.Query().
		Where(entinvoice.State(int32(levyan.InvoiceState_INVOICE_STATE_OPEN))).
		Order(ent.Asc(entinvoice.FieldDateFinalized), ent.Asc(entinvoice.FieldId)).
		All(ctx)
	if err != nil {
		return err
	}
	for _, inv := range open {
		if _, c := v.collector(inv.Channel); c == nil || !c.Pushes() || remaining(inv) == 0 {
			continue
		}
		tried, err := v.db().PaymentAttempt.Query().
			Where(entattempt.InvoiceId(inv.Id), entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE))).
			Exist(ctx)
		if err != nil {
			return err
		}
		if tried {
			continue
		}
		if err := v.attempt(ctx, inv.Id, inv.BillingAccountId); err != nil {
			logErr(ctx, "invoices: an invoice's first attempt was not written", err, slog.String("invoice", pdid.Id(inv.Id).String()))
			errs = append(errs, err)
		}
	}

	as, err := v.db().PaymentAttempt.Query().
		Where(entattempt.StateIn(live...)).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, a := range as {
		if err := v.drive(ctx, a); err != nil {
			logErr(ctx, "invoices: an attempt was not taken on", err, slog.String("attempt", pdid.Id(a.Id).String()))
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// attempt writes the first attempt to charge what is left of invoice `id`.
func (v Invoices) attempt(ctx context.Context, id, ba uuid.UUID) error {
	return v.held(ctx, ba, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		inv, err := db.Invoice.Get(ctx, id)
		if err != nil {
			return err
		}
		if inv.State != int32(levyan.InvoiceState_INVOICE_STATE_OPEN) || remaining(inv) == 0 {
			return errSkip
		}
		tried, err := db.PaymentAttempt.Query().
			Where(entattempt.InvoiceId(inv.Id), entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE))).
			Exist(ctx)
		if err != nil || tried {
			return errors.Join(err, errSkip)
		}

		_, err = addAttempt(ctx, s, inv, levyan.AttemptKind_ATTEMPT_KIND_CHARGE, remaining(inv), uuid.UUID{})
		return err
	})
}

// addAttempt writes an attempt of `kind` for `amount` of invoice `inv`
// through `s`, not sent yet: for a refund, of credit note `cn`.
func addAttempt(ctx context.Context, s levyan.Server, inv *ent.Invoice, kind levyan.AttemptKind, amount int64, cn uuid.UUID) (*levyan.PaymentAttempt, error) {
	id := pdid.New(pd.PaymentAttemptDomain)
	req := levyan.PaymentAttemptAddRequest_builder{
		Id:         id.Bytes(),
		Tenant:     levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
		Invoice:    levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Kind:       kind,
		AttemptKey: KeyOf(id),
		Channel:    inv.Channel,
		Currency:   inv.Currency,
		Amount:     amount,
		State:      levyan.AttemptState_ATTEMPT_STATE_REQUESTED,
	}.Build()
	if cn != (uuid.UUID{}) {
		req.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn[:]}.Build())
	}
	v, err := s.PaymentAttempt().Add(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("attempt: %w", err)
	}

	return v, nil
}

// KeyOf is the key of the attempt `id`: what keeps it once at its channel,
// as a gateway's order (6 to 64 of letters, digits, '-', '_', '=') and a
// merchant of record's charge key (1 to 64 of those, '.' and ':') both take.
func KeyOf(id pdid.Id) string { return "lv-" + hex.EncodeToString(id.Bytes()) }

// drive takes attempt `a` one step on: sent, when nothing of it is out, and
// asked about, when something is.
func (v Invoices) drive(ctx context.Context, a *ent.PaymentAttempt) error {
	inv, err := v.db().Invoice.Get(ctx, a.InvoiceId)
	if err != nil {
		return err
	}
	_, c := v.collector(a.Channel)
	if c == nil || !c.Pushes() {
		// A refund of a transfer is made, and recorded, by an operator's
		// hand; nothing of it is sent from here.
		return nil
	}

	switch levyan.AttemptState(a.State) {
	case levyan.AttemptState_ATTEMPT_STATE_REQUESTED:
		send, err := v.out(ctx, a.Id, inv)
		if errors.Is(err, errSkip) {
			return nil
		}
		if err != nil {
			return err
		}
		o, err := c.Send(ctx, send)
		return v.settle(ctx, a.Id, inv.BillingAccountId, o, err, opSend)

	default:
		at, err := v.attemptOf(ctx, v.db(), a, inv)
		if err != nil {
			return err
		}
		o, err := c.Ask(ctx, at)
		return v.settle(ctx, a.Id, inv.BillingAccountId, o, err, opAsk)
	}
}

// out writes a send of attempt `id` down and commits it -- the attempt is
// UNKNOWN from then on, its send counted and stamped -- before anything is
// sent, and answers the send to make.
func (v Invoices) out(ctx context.Context, id uuid.UUID, inv *ent.Invoice) (Send, error) {
	var out Send
	err := v.held(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		a, err := db.PaymentAttempt.Get(ctx, id)
		if err != nil {
			return err
		}
		if a.State != int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED) {
			// Taken on by another pass meanwhile.
			return errSkip
		}

		previous := time.Time{}
		if a.DateSent != nil {
			previous = ledger.Moment(*a.DateSent)
		}
		now := v.now()
		n := int(a.Sends) + 1
		if _, err := s.PaymentAttempt().Patch(ctx, levyan.PaymentAttemptPatchRequest_builder{
			Ref:              levyan.PaymentAttemptRef_builder{Id: a.Id[:]}.Build(),
			State:            levyan.AttemptState_ATTEMPT_STATE_UNKNOWN.Enum(),
			Sends:            proto.Uint32(uint32(n)),
			DateSent:         timestamppb.New(now),
			DateUpdatedForce: proto.Bool(true),
		}.Build()); err != nil {
			return fmt.Errorf("attempt %s: %w", pdid.Id(a.Id), err)
		}

		at, err := v.attemptOf(ctx, db, a, inv)
		if err != nil {
			return err
		}
		at.Sends, at.Sent = n, now
		out = Send{Attempt: at, N: n, Key: SendKey(a.AttemptKey, n), Previous: previous}
		return nil
	})
	if err == nil && out.N == 0 {
		return Send{}, errSkip
	}

	return out, err
}

// attemptOf is attempt `a` of invoice `inv` as a collector is told of it.
func (v Invoices) attemptOf(ctx context.Context, db *ent.Client, a *ent.PaymentAttempt, inv *ent.Invoice) (Attempt, error) {
	product, err := db.Tenant.Get(ctx, inv.TenantId)
	if err != nil {
		return Attempt{}, err
	}
	out := Attempt{
		Id:      pdid.Id(a.Id),
		Key:     a.AttemptKey,
		Kind:    levyan.AttemptKind(a.Kind),
		Invoice: invoiceOf(inv, product),
		Amount:  money.Amount{Minor: a.Amount, Currency: a.Currency},
		Sends:   int(a.Sends),
	}
	if a.DateSent != nil {
		out.Sent = ledger.Moment(*a.DateSent)
	}
	if a.Kind == int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND) {
		paid, err := db.PaymentAttempt.Query().
			Where(
				entattempt.InvoiceId(inv.Id),
				entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)),
				entattempt.State(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED)),
			).
			Order(ent.Asc(entattempt.FieldDateCreated)).
			First(ctx)
		switch {
		case err == nil:
			out.Refunds = paid.ChannelRef
		case !ent.IsNotFound(err):
			return Attempt{}, err
		}
		if a.CreditNoteId != (uuid.UUID{}) {
			cn, err := db.CreditNote.Get(ctx, a.CreditNoteId)
			if err != nil {
				return Attempt{}, err
			}
			out.Reason = reasonText(levyan.CreditNoteReason(cn.Reason))
		}
	}

	return out, nil
}

// invoiceOf is invoice `inv` of `product` as a collector is told of it.
func invoiceOf(inv *ent.Invoice, product *ent.Tenant) Invoice {
	return Invoice{
		Id:          pdid.Id(inv.Id),
		Product:     pdid.Id(inv.TenantId),
		Account:     pdid.Id(inv.BillingAccountId),
		Channel:     levyan.Channel(levyan.Channel_value[inv.Channel]),
		Start:       ledger.Moment(inv.DateStart),
		End:         ledger.Moment(inv.DateEnd),
		Description: Description(product, inv.DateStart, inv.DateEnd),
	}
}

// reasonText is why a refund is made, in words a payer may be shown.
func reasonText(r levyan.CreditNoteReason) string {
	switch r {
	case levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION:
		return "청구 정정"
	case levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION:
		return "청구 취소"
	default:
		return "환불"
	}
}

type op int

const (
	opSend op = iota
	opAsk
)

// settle writes down what came of a send or an ask of attempt `id`, and of
// a charge that succeeded what its invoice was paid.
//
//   - Sent, an error of [Unknown] leaves it UNKNOWN, to be asked about; one
//     the channel says did nothing for now -- [Auth], [RateLimited] -- leaves
//     it REQUESTED, to be sent again as a send of its own; one it says will
//     never be done -- [Invalid], [Declined], [NotFound] -- fails it.
//   - Asked, [NotFound] is the channel saying it never had it: REQUESTED, to
//     be sent again. Any other error leaves it as it is, to be asked again.
//   - An outcome is written as it is.
func (v Invoices) settle(ctx context.Context, id, ba uuid.UUID, o Outcome, sent error, how op) error {
	return v.held(ctx, ba, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		a, err := db.PaymentAttempt.Get(ctx, id)
		if err != nil {
			return err
		}
		if a.State == int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED) || a.State == int32(levyan.AttemptState_ATTEMPT_STATE_FAILED) {
			return errSkip
		}

		if sent != nil {
			k := KindOf(sent)
			code, message := codeOf(sent)
			attrs := []any{slog.String("attempt", pdid.Id(a.Id).String()), slog.String("kind", k.String()), slog.Any("err", sent)}
			switch {
			case how == opAsk && k == NotFound:
				log.From(ctx).InfoContext(ctx, "invoices: the channel never had the attempt, which is sent again", attrs...)
				o = Outcome{State: levyan.AttemptState_ATTEMPT_STATE_REQUESTED}
			case how == opAsk, k == Unknown:
				log.From(ctx).WarnContext(ctx, "invoices: what came of an attempt is not known yet, and it is asked about again", attrs...)
				return errSkip
			case k == Auth || k == RateLimited:
				log.From(ctx).WarnContext(ctx, "invoices: the channel did nothing of the attempt for now, which is sent again later", attrs...)
				o = Outcome{State: levyan.AttemptState_ATTEMPT_STATE_REQUESTED}
			default:
				log.From(ctx).WarnContext(ctx, "invoices: the channel will not do the attempt as it is asked", attrs...)
				o = Outcome{State: levyan.AttemptState_ATTEMPT_STATE_FAILED, Failure: FailureOf(k, 0, code, message)}
			}
		}

		now := v.now()
		req := levyan.PaymentAttemptPatchRequest_builder{
			Ref:              levyan.PaymentAttemptRef_builder{Id: a.Id[:]}.Build(),
			State:            o.State.Enum(),
			DateUpdatedForce: proto.Bool(true),
		}.Build()
		if o.Ref != "" {
			req.SetChannelRef(o.Ref)
		}
		if o.Number != "" {
			req.SetChannelNumber(o.Number)
		}
		switch o.State {
		case levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED:
			req.SetDateResolved(timestamppb.New(now))
			at := o.At
			if at.IsZero() {
				at = now
			}
			req.SetDatePaid(timestamppb.New(ledger.Moment(at)))
		case levyan.AttemptState_ATTEMPT_STATE_FAILED:
			req.SetDateResolved(timestamppb.New(now))
			if o.Failure != nil {
				req.SetFailure(o.Failure)
			}
		case levyan.AttemptState_ATTEMPT_STATE_PENDING, levyan.AttemptState_ATTEMPT_STATE_REQUESTED:
		default:
			return fmt.Errorf("invoices: %s is no outcome of an attempt", o.State)
		}
		if _, err := s.PaymentAttempt().Patch(ctx, req); err != nil {
			return fmt.Errorf("attempt %s: %w", pdid.Id(a.Id), err)
		}

		if o.State == levyan.AttemptState_ATTEMPT_STATE_FAILED && a.Kind == int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND) {
			log.From(ctx).ErrorContext(ctx, "invoices: the channel would not make a refund a credit note gives back, and an operator has to see to it",
				slog.String("attempt", pdid.Id(a.Id).String()), slog.String("credit_note", pdid.Id(a.CreditNoteId).String()))
		}
		if o.State != levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED || a.Kind != int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE) {
			return nil
		}

		return v.paid(ctx, s, db, a.InvoiceId, a.Amount, o, now)
	})
}

// paid writes down that `amount` of invoice `id` was collected: settled once
// nothing is left, with the channel's references of it. What is more than is
// left is the billing account's credit.
func (v Invoices) paid(ctx context.Context, s levyan.Server, db *ent.Client, id uuid.UUID, amount int64, o Outcome, now time.Time) error {
	inv, err := db.Invoice.Get(ctx, id)
	if err != nil {
		return err
	}
	left := remaining(inv)
	took := min(amount, left)
	over := amount - took

	req := levyan.InvoicePatchRequest_builder{
		Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Paid:             proto.Int64(inv.Paid + took),
		DateUpdatedForce: proto.Bool(true),
	}.Build()
	if o.Ref != "" && inv.ChannelRef == "" {
		req.SetChannelRef(o.Ref)
	}
	if o.Number != "" && inv.ChannelNumber == "" {
		req.SetChannelNumber(o.Number)
	}
	open := inv.State == int32(levyan.InvoiceState_INVOICE_STATE_OPEN) || inv.State == int32(levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE)
	if !open {
		// Collected for an invoice voided or settled meanwhile: all of it is
		// owed back, and reconciliation finds it beside its invoice.
		log.From(ctx).ErrorContext(ctx, "invoices: money was collected for an invoice that was not open, and is the billing account's credit",
			slog.String("invoice", pdid.Id(inv.Id).String()), slog.Int64("amount", amount))
		return credit(ctx, s, inv.TenantId, inv.BillingAccountId, inv.Currency, amount, levyan.CreditKind_CREDIT_KIND_OVERPAID, inv.Id, uuid.UUID{})
	}
	if took == left {
		req.SetState(levyan.InvoiceState_INVOICE_STATE_PAID)
		req.SetDateSettled(timestamppb.New(now))
	}
	if _, err := s.Invoice().Patch(ctx, req); err != nil {
		return fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
	}

	return credit(ctx, s, inv.TenantId, inv.BillingAccountId, inv.Currency, over, levyan.CreditKind_CREDIT_KIND_OVERPAID, inv.Id, uuid.UUID{})
}
