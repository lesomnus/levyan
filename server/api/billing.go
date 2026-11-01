package api

import (
	"context"
	"math/big"
	"strings"
	"time"
	"uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/benefits"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// Billing is `levyan.v1.BillingService`: what a product's workspaces come to.
//
// A statement is read through the walled stack, with the caller's frame. An
// estimate is worked out of the ledger as a close works it out
// (`server/billing`), with ent, narrowed to the product the frame names and to
// its workspace the request names within it -- a product's call is about its
// own rows, and the arithmetic is the close's and no copy of it.
type Billing struct {
	levyanv1.UnimplementedBillingServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server

	// Db is what an estimate reads the ledger from, and Now the deployment's
	// clock, which says which period is open.
	Db  *ent.Client
	Now func() time.Time

	// Free is how many of a payer's workspaces have their allowances.
	Free int
}

func (b Billing) GetUsage(ctx context.Context, req *levyanv1.GetUsageRequest) (*levyanv1.GetUsageResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}
	key, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	product, err := b.Db.Tenant.Get(ctx, f.Tenant.Uuid())
	if err != nil {
		return nil, err
	}
	terms, err := billing.TermsOf(product)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the product's terms: %s", err)
	}
	w, err := b.Db.Workspace.Query().Where(entworkspace.TenantId(product.Id), entworkspace.Key(key.Bytes())).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "nothing has been heard of workspace %s", key)
	}
	if err != nil {
		return nil, err
	}

	now := ledger.Moment(b.Now())
	p, err := billing.Current(ctx, b.Db, product.Id, terms, now)
	if err != nil {
		return nil, err
	}
	r := billing.Rater{Db: b.Db, Free: b.Free}
	book, err := r.Book(ctx, product.Id)
	if err != nil {
		return nil, err
	}
	// On the terms of who pays for the workspace over the period.
	if terms, _, err = r.TermsFor(ctx, product.Id, w.Id, p, terms); err != nil {
		return nil, err
	}
	st, err := billing.Estimate(ctx, r, book, product.Id, w.Id, p, terms)
	if err != nil {
		return nil, err
	}

	u := levyanv1.Usage_builder{
		Workspace: key.String(),
		Period:    period(p.Start, p.End, terms.Zone.String()),
		AsOf:      timestamppb.New(now),
		Total:     st.Total,
	}.Build()
	if terms.Priced() {
		u.SetChannel(levyanv1.Channel(terms.Channel))
		u.SetCurrency(terms.Currency)
	}
	for _, l := range st.Lines {
		u.SetLines(append(u.GetLines(), line(l, terms.Zone.String())))
	}

	// What it is given of its meters on top of what its prices include, in
	// force at any moment of the period (#43).
	as, err := benefits.InForce(ctx, b.Db, product.Id, w.Id, p.Start, p.End)
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		u.SetAllowances(append(u.GetAllowances(), allowanceOf(a)))
	}

	return levyanv1.GetUsageResponse_builder{Usage: u}.Build(), nil
}

func (b Billing) GetStatement(ctx context.Context, req *levyanv1.GetStatementRequest) (*levyanv1.GetStatementResponse, error) {
	id, err := pdid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %q is not an identifier", req.GetId())
	}

	v, err := b.S.Statement().Get(ctx, levyan.StatementGetRequest_builder{
		Ref: levyan.StatementRef_builder{Id: id.Bytes()}.Build(),
		Select: levyan.StatementSelect_builder{
			All:       proto.Bool(true),
			Workspace: levyan.WorkspaceSelect_builder{Key: proto.Bool(true)}.Build(),
			Period:    levyan.PeriodSelect_builder{Zone: proto.Bool(true)}.Build(),
		}.Build(),
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "no statement is %s", id)
		}
		return nil, err
	}

	ws, err := pdid.From(v.GetWorkspace().GetKey())
	if err != nil {
		return nil, err
	}

	return levyanv1.GetStatementResponse_builder{Statement: statement(v, ws, v.GetPeriod().GetZone())}.Build(), nil
}

func (b Billing) ListStatements(ctx context.Context, req *levyanv1.ListStatementsRequest) (*levyanv1.ListStatementsResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}
	key, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	size := req.GetPageSize()
	switch {
	case size <= 0:
		size = 20
	case size > 100:
		size = 100
	}

	w, err := b.S.Workspace().Get(ctx, levyan.WorkspaceGetRequest_builder{
		Ref: levyan.WorkspaceRef_builder{Keyed: levyan.WorkspaceRefByKeyed_builder{
			Key:    key.Bytes(),
			Tenant: levyan.TenantRef_builder{Id: f.Tenant.Bytes()}.Build(),
		}.Build()}.Build(),
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "nothing has been heard of workspace %s", key)
		}
		return nil, err
	}

	vs, err := b.S.Statement().List(ctx, levyan.StatementListRequest_builder{
		Filters: []*levyan.StatementFilter{levyan.StatementFilter_builder{
			Workspace: levyan.WorkspaceRef_builder{Id: w.GetId()}.Build(),
		}.Build()},
		Size:    size,
		After:   req.GetPageToken(),
		Reverse: true,
	}.Build())
	if err != nil {
		return nil, err
	}

	// The zone each period was a month in, which is the period's.
	ids := []uuid.UUID{}
	for _, v := range vs.GetItems() {
		id, err := pdid.From(v.GetPeriod().GetId())
		if err != nil {
			return nil, err
		}
		ids = append(ids, id.Uuid())
	}
	ps, err := b.Db.Period.Query().Where(entperiod.TenantId(f.Tenant.Uuid()), entperiod.IdIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	zones := map[uuid.UUID]string{}
	for _, p := range ps {
		zones[p.Id] = p.Zone
	}

	out := []*levyanv1.Statement{}
	for i, v := range vs.GetItems() {
		out = append(out, statement(v, key, zones[ids[i]]))
	}

	return levyanv1.ListStatementsResponse_builder{Statements: out, NextPageToken: vs.GetNext()}.Build(), nil
}

func period(start, end time.Time, zone string) *levyanv1.Period {
	return levyanv1.Period_builder{Start: timestamppb.New(start), End: timestamppb.New(end), Zone: zone}.Build()
}

func statement(v *levyan.Statement, ws pdid.Id, zone string) *levyanv1.Statement {
	out := levyanv1.Statement_builder{
		Id:        text(v.GetId()),
		Workspace: ws.String(),
		Period:    period(v.GetDateStart().AsTime(), v.GetDateEnd().AsTime(), zone),
		Currency:  v.GetCurrency(),
		Total:     v.GetTotal(),
		Closed:    stamp(v.GetDateCreated()),
	}.Build()
	if c, err := billing.Channel(v.GetChannel()); err == nil {
		out.SetChannel(levyanv1.Channel(c))
	}
	for _, l := range v.GetLines().GetLines() {
		out.SetLines(append(out.GetLines(), line(l, zone)))
	}

	return out
}

func line(l *levyan.StatementLine, zone string) *levyanv1.Line {
	out := levyanv1.Line_builder{
		Meter:    l.GetKey(),
		Unit:     l.GetUnit(),
		Kind:     levyanv1.LineKind(l.GetKind()),
		Usage:    decimal(l.GetUsage()),
		Included: decimal(l.GetIncluded()),
		Billable: decimal(l.GetBillable()),
		Amount:   decimal(l.GetAmount()),
	}.Build()
	if v := l.GetAllowance(); v != "" {
		out.SetAllowance(decimal(v))
	} else {
		out.SetAllowance("0")
	}
	if l.GetKind() == levyan.LineKind_LINE_KIND_CORRECTION {
		out.SetCorrects(period(l.GetCorrectsStart().AsTime(), l.GetCorrectsEnd().AsTime(), zone))
		out.SetBilled(decimal(l.GetBilled()))
	}

	for _, p := range l.GetParts() {
		part := levyanv1.LinePart_builder{
			Price:     text(p.GetPrice()),
			From:      stamp(p.GetFrom()),
			To:        stamp(p.GetTo()),
			Share:     decimal(p.GetShare()),
			Included:  p.GetIncluded(),
			Allowance: p.GetAllowance(),
			Billable:  decimal(p.GetBillable()),
			Amount:    decimal(p.GetAmount()),
		}.Build()
		for _, a := range p.GetAllowances() {
			part.SetAllowances(append(part.GetAllowances(), text(a)))
		}
		for _, b := range p.GetBands() {
			band := levyanv1.BandShare_builder{Fee: b.GetFee(), Share: decimal(b.GetShare()), Amount: decimal(b.GetAmount())}.Build()
			if b.HasUpTo() {
				band.SetUpTo(b.GetUpTo())
			}
			part.SetBands(append(part.GetBands(), band))
		}
		out.SetParts(append(out.GetParts(), part))
	}
	for _, s := range l.GetUnpriced() {
		out.SetUnpriced(append(out.GetUnpriced(), levyanv1.Stretch_builder{From: stamp(s.GetFrom()), To: stamp(s.GetTo())}.Build()))
	}

	return out
}

// decimal is a fraction as the API writes one: exact when it ends within six
// digits after the point, and cut there, toward nothing, when it does not.
func decimal(s string) string {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return s
	}

	const digits = 6
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(digits), nil)
	n := new(big.Int).Mul(r.Num(), scale)
	n.Quo(n, r.Denom())

	sign := ""
	if n.Sign() < 0 {
		sign = "-"
		n.Neg(n)
	}
	v := n.String()
	if len(v) <= digits {
		v = strings.Repeat("0", digits-len(v)+1) + v
	}
	whole, frac := v[:len(v)-digits], strings.TrimRight(v[len(v)-digits:], "0")
	if whole == "0" && frac == "" {
		sign = ""
	}
	if frac == "" {
		return sign + whole
	}

	return sign + whole + "." + frac
}
