package reconcile

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/mor"
)

// What an operator does about a finding, through the operator's entry point
// (`server/admin`): each through the stack, as the operator `ctx` is framed
// as, so that the trail says who did it. None of them is done by a pass.

// operator is the operator `ctx` is framed as, by their row: what the
// operator's entry point frames each request as.
func operator(ctx context.Context) (pdid.Id, error) {
	f, ok := frame.From(ctx)
	if !ok {
		return pdid.Nil, status.Error(codes.PermissionDenied, "an operator's correction is an operator's")
	}
	if _, ok := f.Row.(*levyan.Operator); !ok || f.Actor.IsZero() {
		return pdid.Nil, status.Error(codes.PermissionDenied, "an operator's correction is an operator's")
	}

	return f.Actor, nil
}

// Accept accepts finding `id` as it is, with `reason`, in the words of the
// operator `ctx` is framed as: it stays accepted while it says what it said
// as it was accepted, and is open again once a pass finds it says something
// else. One resolved is resolved, and is not accepted.
func (r *Reconciler) Accept(ctx context.Context, id pdid.Id, reason string) (*ent.Finding, error) {
	who, err := operator(ctx)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, status.Error(codes.InvalidArgument, "reason: why it is accepted as it is, in your words")
	}
	f, err := r.db().Finding.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no finding is %s", id)
	}
	if err != nil {
		return nil, err
	}
	if f.State == int32(levyan.FindingState_FINDING_STATE_RESOLVED) {
		return nil, status.Errorf(codes.FailedPrecondition, "finding %s is resolved: %s", id, f.Resolution)
	}

	if _, err := r.S.Finding().Patch(ctx, levyan.FindingPatchRequest_builder{
		Ref:              levyan.FindingRef_builder{Id: id.Bytes()}.Build(),
		State:            levyan.FindingState_FINDING_STATE_ACCEPTED.Enum(),
		Reason:           proto.String(reason),
		DateAccepted:     timestamppb.New(r.now()),
		AcceptedBy:       levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
		DateUpdatedForce: proto.Bool(true),
	}.Build()); err != nil {
		return nil, err
	}

	return r.db().Finding.Get(ctx, id.Uuid())
}

// Refund gives back what the key of charge `ref` -- an attempt, by its
// identifier or its key -- was charged past what the ledger accounts for:
// of every subscription its billing account has had with the merchant of
// record, each payment of the key but the one the attempt settled by, as
// much of it as the merchant has not given back. Each is a refund through
// the ledger ([invoices.Invoices.RefundExtra]), sent at once as the loop
// sends one and asked about by the loop after, as the operator `ctx` is
// framed as; one asked for already is answered again, and nothing is sent.
func (r *Reconciler) Refund(ctx context.Context, ref string) ([]*ent.PaymentAttempt, error) {
	if _, err := operator(ctx); err != nil {
		return nil, err
	}
	if r.channel() == nil {
		return nil, status.Error(codes.FailedPrecondition, "no merchant of record is configured here (`mor`), and nothing is given back through one")
	}
	a, err := r.Invoices.AttemptOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	if a.Kind != charge || a.Channel != onMerchant {
		return nil, status.Errorf(codes.FailedPrecondition, "attempt %s is no charge of the merchant of record's", a.AttemptKey)
	}

	h := &held{r: r, p: &pass{now: r.now(), db: r.db()}, txns: map[string]mor.Transaction{}, missing: map[string]bool{}, attempts: map[string]*ent.PaymentAttempt{}, extras: map[string]bool{}}
	extra, err := h.extra(ctx, a)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "the merchant of record could not be asked what it holds of key %s: %v", a.AttemptKey, err)
	}

	out := []*ent.PaymentAttempt{}
	for _, e := range extra {
		left := e.line.Amount.Minor - e.given.Minor
		if left <= 0 {
			continue
		}
		v, err := r.Invoices.RefundExtra(ctx, pdid.Id(a.Id), e.txn.ID, left)
		if err != nil {
			return nil, err
		}
		if v.State == requested {
			if v, err = r.Invoices.Drive(ctx, pdid.Id(v.Id)); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "the merchant of record holds no payment of key %s that the ledger does not account for and has not given back", a.AttemptKey)
	}

	return out, nil
}

// Settle settles attempt `ref` -- by its identifier or its key -- from what
// the channel answers of it, as the operator `ctx` is framed as: with no
// `transaction`, the channel is asked of it by its key, as the loop asks
// ([invoices.Invoices.Ask]); with one, of the merchant of record's naming it
// -- found in the merchant's own pages, of a subscription no mirror is of --
// what the merchant answers of that transaction is written as a notice of
// it is ([merchant.Collector.Heard]). Either way only what is further along
// than the attempt is written, and one that ended is what it is: what the
// channel holds more of its key is a key charged twice, to give back.
func (r *Reconciler) Settle(ctx context.Context, ref, transaction string) (*ent.PaymentAttempt, error) {
	if _, err := operator(ctx); err != nil {
		return nil, err
	}
	a, err := r.Invoices.AttemptOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	transaction = strings.TrimSpace(transaction)
	if transaction == "" {
		v, err := r.Invoices.Ask(ctx, pdid.Id(a.Id))
		if err != nil && v != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return v, err
	}

	if r.channel() == nil || r.Merchant == nil || a.Channel != onMerchant {
		return nil, status.Error(codes.FailedPrecondition, "a transaction is the merchant of record's, and the attempt is no attempt of its")
	}
	t, err := r.channel().Transaction(ctx, transaction)
	if err != nil {
		if mor.KindOf(err) == mor.NotFound || mor.KindOf(err) == mor.Invalid {
			return nil, status.Errorf(codes.NotFound, "the merchant of record has no transaction %s", transaction)
		}
		return nil, status.Errorf(codes.Unavailable, "the merchant of record could not be asked of transaction %s: %v", transaction, err)
	}
	_, line := t.Line(a.AttemptKey)
	_, adj := t.Adjustment(a.AttemptKey)
	if (a.Kind == charge && !line) || (a.Kind == refund && !adj) {
		return nil, status.Errorf(codes.FailedPrecondition, "transaction %s holds nothing of key %s", t.ID, a.AttemptKey)
	}
	if err := r.Merchant.Heard(ctx, nil, t); err != nil {
		return nil, err
	}

	return r.db().PaymentAttempt.Query().Where(entattempt.Id(a.Id)).Only(ctx)
}

// Rule follows rule `rule` of `product` again -- every rule, with none
// named -- as the operator `ctx` is framed as ([grants.Grants.Again]): what
// the rules want given, what they no longer want ended, and what was given
// to the product's workspaces that were destroyed ended.
func (r *Reconciler) Rule(ctx context.Context, product pdid.Id, rule string) error {
	if _, err := operator(ctx); err != nil {
		return err
	}
	if r.Grants == nil {
		return status.Error(codes.FailedPrecondition, "nothing keeps grants here")
	}

	return r.Grants.Again(ctx, product, strings.TrimSpace(rule))
}
