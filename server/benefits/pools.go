package benefits

import (
	"context"
	"strings"
	"time"
	"uuid"

	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	entpool "github.com/lesomnus/levyan/internal/ent/pool"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// Pools gives and ends what products' workspaces are given of their meters
// as bundles to take what is billable out of (#43), through S.
type Pools struct {
	// S is the stack every pool is written through: the deployment's own
	// (`cmd.Server.Ungated`), as the operator the context is framed as.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time
}

func (a Pools) now() time.Time {
	if a.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(a.Now())
}

func (a Pools) postgres() bool { return a.Dialect == config.DialectPostgres }

func (a Pools) db() *ent.Client { return ent.NewClient(ent.Driver(a.Drv)) }

// Fill is a pool to give.
type Fill struct {
	Product pdid.Id

	// Workspace is the product's own tenant it is given to, by its
	// identifier: what the product names it by.
	Workspace pdid.Id

	// Meter is the meter it is a bundle of, by its key.
	Meter string

	// Quantity is how much it holds, in the meter's unit for a period.
	Quantity int64

	// From is when it is in force from, now when zero; Expires when it
	// expires, a year from now when zero (Claude's proposal, #43).
	From    time.Time
	Expires time.Time

	// Reason is why, in the operator's words.
	Reason string
}

// Grant gives a workspace a pool, as the operator `ctx` is framed as, the
// workspace's row held as it is written, as a close of it holds it. A
// workspace levyan has heard nothing of yet is written, as a grant's is; one
// destroyed is given nothing.
func (a Pools) Grant(ctx context.Context, v Fill) (*ent.Pool, error) {
	who, err := Operator(ctx)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(v.Reason)
	switch {
	case reason == "":
		return nil, status.Error(codes.InvalidArgument, "reason: why it is given, in your words")
	case v.Quantity <= 0:
		return nil, status.Error(codes.InvalidArgument, "quantity: more than nothing, in the meter's unit for a period")
	}
	expires := ledger.Moment(v.Expires)
	if v.Expires.IsZero() {
		expires = a.now().AddDate(1, 0, 0)
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

	id := pdid.New(pd.PoolDomain)
	err = held(ctx, a.S, a.Drv, a.postgres(), v.Product.Uuid(), ws, func(s levyan.Server, db *ent.Client) error {
		req := levyan.PoolAddRequest_builder{
			Id:          id.Bytes(),
			Tenant:      levyan.TenantRef_builder{Id: v.Product.Bytes()}.Build(),
			Workspace:   levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
			Meter:       levyan.MeterRef_builder{Id: m.Id[:]}.Build(),
			Quantity:    v.Quantity,
			DateExpires: timestamppb.New(expires),
			Reason:      reason,
			GrantedBy:   levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
		}.Build()
		if !v.From.IsZero() {
			req.SetValidFrom(timestamppb.New(ledger.Moment(v.From)))
		}
		_, err := s.Pool().Add(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}

	return a.db().Pool.Get(ctx, id.Uuid())
}

// Expire ends pool `id`, as the operator `ctx` is framed as, for `reason`:
// from the start of the period now open, no period takes from it -- a
// period is taken from at its close, whole -- and what the periods before
// took stays taken.
func (a Pools) Expire(ctx context.Context, id pdid.Id, reason string) (*ent.Pool, error) {
	who, err := Operator(ctx)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, status.Error(codes.InvalidArgument, "reason: why it is ended, in your words")
	}
	was, err := a.db().Pool.Query().Where(entpool.Id(id.Uuid())).WithTenant().Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no pool is %s", id)
	}
	if err != nil {
		return nil, err
	}
	terms, err := billing.TermsOf(was.Edges.Tenant)
	if err != nil {
		return nil, err
	}

	err = held(ctx, a.S, a.Drv, a.postgres(), was.TenantId, was.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
		v, err := db.Pool.Get(ctx, id.Uuid())
		if err != nil {
			return err
		}
		open, err := billing.Current(ctx, db, v.TenantId, terms, a.now())
		if err != nil {
			return err
		}
		at := latest(open.Start, ledger.Moment(v.ValidFrom))
		if !ledger.Moment(v.DateExpires).After(at) {
			return status.Errorf(codes.FailedPrecondition, "pool %s is over already: it expired at %s", id, v.DateExpires.UTC().Format(time.RFC3339))
		}
		_, err = s.Pool().Patch(ctx, levyan.PoolPatchRequest_builder{
			Ref:              levyan.PoolRef_builder{Id: id.Bytes()}.Build(),
			DateExpires:      timestamppb.New(at),
			EndedBy:          levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
			EndReason:        proto.String(reason),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		return err
	})
	if err != nil {
		return nil, err
	}

	return a.db().Pool.Get(ctx, id.Uuid())
}

// PoolsOf is `product`'s pools of workspace `ws` -- every workspace's, for
// none -- by when each expires.
func PoolsOf(ctx context.Context, db *ent.Client, product uuid.UUID, ws ...uuid.UUID) ([]*ent.Pool, error) {
	q := db.Pool.Query().Where(entpool.TenantId(product))
	if len(ws) > 0 {
		q = q.Where(entpool.WorkspaceIdIn(ws...))
	}

	return q.WithMeter().WithWorkspace().Order(ent.Asc(entpool.FieldDateExpires), ent.Asc(entpool.FieldId)).All(ctx)
}
