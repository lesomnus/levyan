package invoices

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/server/pd"
)

// What reconciliation (#13) asks of the ledger: an attempt asked about
// again, as the loop asks about one, and what a key was charged past what the
// ledger accounts for given back. Neither moves money on the ledger's word: an
// ask writes down what the channel says came of an attempt, and a refund of
// an extra payment is an operator's, sent as any refund is.

// AttemptOf is the attempt `ref` names: by its identifier, or by its key
// (`lv-…`), as an operator finds it in `invoice show`.
func (v Invoices) AttemptOf(ctx context.Context, ref string) (*ent.PaymentAttempt, error) {
	ref = strings.TrimSpace(ref)
	q := v.db().PaymentAttempt.Query()
	if id, err := pdid.Parse(ref); err == nil {
		q = q.Where(entattempt.Id(id.Uuid()))
	} else {
		q = q.Where(entattempt.AttemptKey(ref))
	}
	a, err := q.Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no attempt is %s", ref)
	}

	return a, err
}

// Ask asks the channel what came of attempt `id`, which is out -- unknown, or
// pending -- and writes down what it answers, as a pass of the loop does
// ([Invoices.Collect]): what an answer says is written once, and an ask the
// channel could not answer leaves it as it is, and answers why. One that is
// not out is answered as it is, and nothing is asked: one requested is the
// loop's to send, and one that ended is what it is.
func (v Invoices) Ask(ctx context.Context, id pdid.Id) (*ent.PaymentAttempt, error) {
	a, err := v.db().PaymentAttempt.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no attempt is %s", id)
	}
	if err != nil {
		return nil, err
	}
	switch levyan.AttemptState(a.State) {
	case levyan.AttemptState_ATTEMPT_STATE_UNKNOWN, levyan.AttemptState_ATTEMPT_STATE_PENDING:
	default:
		return a, nil
	}
	_, c := v.collector(a.Channel)
	if c == nil || !c.Pushes() {
		return a, status.Errorf(codes.FailedPrecondition, "nothing asks %s what came of an attempt", a.Channel)
	}
	inv, err := v.db().Invoice.Get(ctx, a.InvoiceId)
	if err != nil {
		return nil, err
	}
	at, err := v.attemptOf(ctx, v.db(), a, inv)
	if err != nil {
		return nil, err
	}

	o, asked := c.Ask(ctx, at)
	if err := v.settle(ctx, a.Id, inv.BillingAccountId, o, asked, opAsk); err != nil {
		return nil, err
	}
	a, err = v.db().PaymentAttempt.Get(ctx, a.Id)
	if err != nil {
		return nil, err
	}
	if asked != nil && KindOf(asked) != NotFound {
		return a, fmt.Errorf("the channel could not say what came of attempt %s: %w", id, asked)
	}

	return a, nil
}

// Drive takes attempt `id` one step on, as a pass of the loop does: sent, when
// nothing of it is out, and asked about, when something is. One that ended is
// what it is.
func (v Invoices) Drive(ctx context.Context, id pdid.Id) (*ent.PaymentAttempt, error) {
	a, err := v.db().PaymentAttempt.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no attempt is %s", id)
	}
	if err != nil {
		return nil, err
	}
	if !ended(a) {
		if err := v.drive(ctx, a); err != nil {
			return nil, err
		}
	}

	return v.db().PaymentAttempt.Get(ctx, a.Id)
}

// RefundExtra writes an attempt to give back `amount` of `payment`, a payment
// the channel holds of the key of charge `charge` past what the ledger
// accounts for -- a key charged twice, or charged after its attempt failed
// (`PaymentAttempt.date_twice`) -- as whoever `ctx` is framed as: an
// operator's correction of what reconciliation found (#13). It is a refund
// as a credit note's is, sent as the loop sends one, and nothing of the
// invoice moves: what the charge settled by paid it, and the extra payment
// never did. The payment the charge settled by is no extra one, and a refund
// of the same payment written before and not failed is answered again,
// writing nothing.
func (v Invoices) RefundExtra(ctx context.Context, charge pdid.Id, payment string, amount int64) (*ent.PaymentAttempt, error) {
	payment = strings.TrimSpace(payment)
	if payment == "" {
		return nil, status.Error(codes.InvalidArgument, "payment: the channel's id of the payment to give back of")
	}
	if amount <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount: what to give back, more than nothing")
	}
	a, err := v.db().PaymentAttempt.Get(ctx, charge.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no attempt is %s", charge)
	}
	if err != nil {
		return nil, err
	}
	inv, err := v.db().Invoice.Get(ctx, a.InvoiceId)
	if err != nil {
		return nil, err
	}

	var out *ent.PaymentAttempt
	err = v.held(ctx, inv.BillingAccountId, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		a, err := db.PaymentAttempt.Get(ctx, a.Id)
		if err != nil {
			return err
		}
		switch {
		case a.Kind != int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE):
			return status.Errorf(codes.FailedPrecondition, "attempt %s is a refund: what is given back of a key is of a charge's", charge)
		case a.State == int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED) && a.ChannelRef == payment:
			return status.Errorf(codes.FailedPrecondition, "%s is the payment attempt %s settled by, which paid its invoice: a credit note gives that back", payment, charge)
		case a.Recorded:
			return status.Errorf(codes.FailedPrecondition, "attempt %s was recorded by hand, and nothing of it is sent", charge)
		}
		if _, c := v.collector(a.Channel); c == nil || !c.Pushes() {
			return status.Errorf(codes.FailedPrecondition, "nothing gives back through %s", a.Channel)
		}

		was, err := db.PaymentAttempt.Query().
			Where(
				entattempt.ExtraOf(a.AttemptKey),
				entattempt.Refunds(payment),
				entattempt.StateNEQ(int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)),
			).
			First(ctx)
		switch {
		case err == nil:
			out = was
			return errSkip
		case !ent.IsNotFound(err):
			return err
		}

		inv, err := db.Invoice.Get(ctx, a.InvoiceId)
		if err != nil {
			return err
		}
		id := pdid.New(pd.PaymentAttemptDomain)
		if _, err := s.PaymentAttempt().Add(ctx, levyan.PaymentAttemptAddRequest_builder{
			Id:         id.Bytes(),
			Tenant:     levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
			Invoice:    levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			Kind:       levyan.AttemptKind_ATTEMPT_KIND_REFUND,
			AttemptKey: KeyOf(id),
			Channel:    a.Channel,
			Currency:   a.Currency,
			Amount:     amount,
			State:      levyan.AttemptState_ATTEMPT_STATE_REQUESTED,
			ExtraOf:    proto.String(a.AttemptKey),
			Refunds:    proto.String(payment),
		}.Build()); err != nil {
			return fmt.Errorf("attempt: %w", err)
		}
		out, err = db.PaymentAttempt.Get(ctx, id.Uuid())
		return err
	})
	if errors.Is(err, errSkip) {
		err = nil
	}

	return out, err
}
