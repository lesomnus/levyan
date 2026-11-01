// Package benefits is what a product's workspaces are given beyond what their
// prices say (#43): allowances -- more of a meter each period for nothing, on
// top of what a price includes -- given and ended by an operator.
//
// That the owner wants a workspace given such things is the owner's
// (2026-11-01). Their shape -- the workspace's, whoever pays for it; given by
// an operator alone (`levyan-admin allowance`), with a reason, on the trail;
// added to what a price includes before anything is priced -- is Claude's
// proposal, which the owner had carried out ("진행해줘"), and may overturn.
// What a period of a workspace comes to with them is `internal/rating`'s
// ([rating.Allowance]); the money a billing account or a workspace is
// granted -- a credit, with an expiry -- is the credit ledger's
// (`server/invoices`).
//
// # Written through the servers, by an operator
//
// Every write goes through the stack it is handed -- the deployment's own
// (`cmd.Server.Ungated`), as the operator the context is framed as -- so the
// trail says who gave an allowance and who ended it, and the row says it too
// (`granted_by`, `ended_by`). A frame that is no operator's is refused: a
// product's own grants of what it sells (a referral's reward) are a later
// question (#43). The [Layer] holds every write of one to what it may be.
package benefits

import (
	"context"
	"strings"
	"time"
	"uuid"

	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entallowance "github.com/lesomnus/levyan/internal/ent/allowance"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// Allowances gives and ends what products' workspaces are given of their
// meters, through S.
type Allowances struct {
	// S is the stack every allowance is written through: the deployment's
	// own (`cmd.Server.Ungated`), as the operator the context is framed as.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time
}

func (a Allowances) now() time.Time {
	if a.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(a.Now())
}

func (a Allowances) postgres() bool { return a.Dialect == config.DialectPostgres }

func (a Allowances) db() *ent.Client { return ent.NewClient(ent.Driver(a.Drv)) }

// Operator is the operator `ctx` is framed as, by their row: who an
// operator's write is written as, and the only caller who gives what this
// package gives.
func Operator(ctx context.Context) (pdid.Id, error) {
	f, ok := frame.From(ctx)
	if !ok {
		return pdid.Nil, status.Error(codes.PermissionDenied, "what a workspace is given beyond its prices is an operator's to give")
	}
	if _, ok := f.Row.(*levyan.Operator); !ok || f.Actor.IsZero() {
		return pdid.Nil, status.Error(codes.PermissionDenied, "what a workspace is given beyond its prices is an operator's to give")
	}

	return f.Actor, nil
}

// Give is an allowance to give.
type Give struct {
	Product pdid.Id

	// Workspace is the product's own tenant it is given to, by its
	// identifier: what the product names it by.
	Workspace pdid.Id

	// Meter is the meter it gives more of, by its key.
	Meter string

	// Quantity is how much more each period includes, in the meter's unit.
	Quantity int64

	// From is when it begins, now when zero; Until when it ends, never when
	// zero -- until it is ended.
	From  time.Time
	Until time.Time

	// Reason is why, in the operator's words.
	Reason string
}

// Give gives a workspace an allowance, as the operator `ctx` is framed as:
// the workspace's row held as it is written, as a close of it holds it, so
// that whether it reaches into a period whose close has begun has one answer.
// A workspace levyan has heard nothing of yet is written, as a grant's is;
// one destroyed is given nothing.
func (a Allowances) Give(ctx context.Context, v Give) (*ent.Allowance, error) {
	who, err := Operator(ctx)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(v.Reason)
	switch {
	case reason == "":
		return nil, status.Error(codes.InvalidArgument, "reason: why it is given, in your words")
	case v.Quantity <= 0:
		return nil, status.Error(codes.InvalidArgument, "quantity: more than nothing, in the meter's unit")
	}

	m, err := a.db().Meter.Query().Where(entmeter.TenantId(v.Product.Uuid()), entmeter.Key(strings.TrimSpace(v.Meter))).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "the product has no meter %q", v.Meter)
	}
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Known(ctx, a.S, a.db(), v.Product, v.Workspace)
	if err != nil {
		return nil, err
	}

	id := pdid.New(pd.AllowanceDomain)
	err = a.held(ctx, v.Product.Uuid(), ws, func(s levyan.Server, db *ent.Client) error {
		req := levyan.AllowanceAddRequest_builder{
			Id:        id.Bytes(),
			Tenant:    levyan.TenantRef_builder{Id: v.Product.Bytes()}.Build(),
			Workspace: levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
			Meter:     levyan.MeterRef_builder{Id: m.Id[:]}.Build(),
			Quantity:  v.Quantity,
			Reason:    reason,
			GrantedBy: levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
		}.Build()
		if !v.From.IsZero() {
			req.SetValidFrom(timestamppb.New(ledger.Moment(v.From)))
		}
		if !v.Until.IsZero() {
			req.SetValidUntil(timestamppb.New(ledger.Moment(v.Until)))
		}
		_, err := s.Allowance().Add(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}

	return a.db().Allowance.Get(ctx, id.Uuid())
}

// End ends allowance `id` now, as the operator `ctx` is framed as, for
// `reason`: in force no longer from now -- or never, for one that has not
// begun. What it gave of the time before stays given.
func (a Allowances) End(ctx context.Context, id pdid.Id, reason string) (*ent.Allowance, error) {
	who, err := Operator(ctx)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, status.Error(codes.InvalidArgument, "reason: why it is ended, in your words")
	}
	was, err := a.db().Allowance.Get(ctx, id.Uuid())
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no allowance is %s", id)
	}
	if err != nil {
		return nil, err
	}

	err = a.held(ctx, was.TenantId, was.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
		v, err := db.Allowance.Get(ctx, id.Uuid())
		if err != nil {
			return err
		}
		until := latest(a.now(), ledger.Moment(v.ValidFrom))
		if v.ValidUntil != nil && !ledger.Moment(*v.ValidUntil).After(until) {
			return status.Errorf(codes.FailedPrecondition, "allowance %s is over already: it ended at %s", id, v.ValidUntil.UTC().Format(time.RFC3339))
		}
		_, err = s.Allowance().Patch(ctx, levyan.AllowancePatchRequest_builder{
			Ref:              levyan.AllowanceRef_builder{Id: id.Bytes()}.Build(),
			ValidUntil:       timestamppb.New(until),
			EndedBy:          levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
			EndReason:        proto.String(reason),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		return err
	})
	if err != nil {
		return nil, err
	}

	return a.db().Allowance.Get(ctx, id.Uuid())
}

// held is `f` in a transaction that holds workspace `ws`'s row first, and
// refuses one destroyed: a workspace destroyed is given nothing more (#14).
func (a Allowances) held(ctx context.Context, product, ws uuid.UUID, f func(s levyan.Server, db *ent.Client) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, a.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(a.S, tdrv)
	if err != nil {
		return err
	}
	db := ent.NewClient(ent.Driver(tdrv))
	if err := ledger.Lock(ctx, db, a.postgres(), ws); err != nil {
		return err
	}
	d, err := ledger.Destroyed(ctx, db, product, ws)
	if err != nil {
		return err
	}
	if at, ok := d[ws]; ok {
		return status.Errorf(codes.FailedPrecondition, "the workspace was destroyed at %s, and is given nothing", at.Format(time.RFC3339))
	}
	if err := f(s, db); err != nil {
		return err
	}

	return tx.Commit()
}

// InForce is what `product`'s workspace `ws` is given of its meters that is
// in force at any moment of [from, to), by when each begins: what a period
// of it is rated with.
func InForce(ctx context.Context, db *ent.Client, product, ws uuid.UUID, from, to time.Time) ([]*ent.Allowance, error) {
	return db.Allowance.Query().
		Where(
			entallowance.TenantId(product),
			entallowance.WorkspaceId(ws),
			entallowance.ValidFromLT(to),
			entallowance.Or(entallowance.ValidUntilIsNil(), entallowance.ValidUntilGT(from)),
		).
		WithMeter().
		Order(ent.Asc(entallowance.FieldValidFrom), ent.Asc(entallowance.FieldId)).
		All(ctx)
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
