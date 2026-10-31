// Package api is `levyan.v1` served: what a product calls -- its price book,
// the ledger its readings are kept in, and what its workspaces come to
// (docs/design.md, "앱이 levyan을 부르는 길") -- and payday's `GrantService`,
// what its workspaces are granted ([Grants]).
//
// Every method reads through the stack a caller reaches, with the caller's
// frame, so the wall keeps a product to its own rows here as it does
// everywhere, and asks it nothing it could get round by naming another
// product. A write to the ledger goes through the same stack as the product
// (`server/ledger`), which decides what to write from rows of the product the
// call is framed in and of no other. What it answers is `levyan.v1`'s
// messages and never the schema's: the schema is levyan's to change, and the
// API is what was promised to the apps that call it.
package api

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/watch"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/payers"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// PriceBook is `levyan.v1.PriceBookService`.
type PriceBook struct {
	levyanv1.UnimplementedPriceBookServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server
}

// Deps is what `levyan.v1` is served from.
type Deps struct {
	// S is the walled stack, and never the one the deployment works through.
	S levyan.Server

	// L writes the ledger through S.
	L ledger.Ledger

	// Db is what an estimate reads the ledger from, narrowed to the caller's
	// product (see [Billing]), and Now the deployment's clock.
	Db  *ent.Client
	Now func() time.Time

	// W is what a change of a grant is published to ([Grants]).
	W *watch.Watch

	// P is who pays for workspaces ([Workspaces], [Payers]), and Free how
	// many of a payer's have their allowances, which an estimate rates by.
	P    payers.Payers
	Free int

	// M is billing accounts' subscriptions with the merchant of record,
	// written through S ([Payers]), and what a workspace's payer's billing
	// account's collection is ([Workspaces]).
	M subscriptions.Subscriptions

	// Dunning is the policy table of what is done after a charge fails
	// (#11): what a payer and a product are told of an invoice whose charge
	// failed is what it says ([Payers], [Workspaces]).
	Dunning dunning.Policy
}

// Register serves `levyan.v1` on g, from d.
func Register(g grpc.ServiceRegistrar, d Deps) {
	levyanv1.RegisterPriceBookServiceServer(g, PriceBook{S: d.S})
	levyanv1.RegisterMeteringServiceServer(g, Metering{L: d.L})
	levyanv1.RegisterBillingServiceServer(g, Billing{S: d.S, Db: d.Db, Now: d.Now, Free: d.Free})
	pdpb.RegisterGrantServiceServer(g, Grants{S: d.S, W: d.W})
	levyanv1.RegisterWorkspaceServiceServer(g, Workspaces{S: d.S, P: d.P, Db: d.Db, Dunning: d.Dunning, Now: d.Now})
	levyanv1.RegisterPayerServiceServer(g, Payers{S: d.S, P: d.P, M: d.M, Db: d.Db, Dunning: d.Dunning, Now: d.Now})
}

// page is how many rows one read through the stack asks for at a time.
const page = 200

func (b PriceBook) GetProduct(ctx context.Context, _ *levyanv1.GetProductRequest) (*levyanv1.GetProductResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}

	t, err := b.S.Tenant().Get(ctx, levyan.TenantGetRequest_builder{
		Ref:    levyan.TenantRef_builder{Id: f.Tenant.Bytes()}.Build(),
		Select: levyan.TenantSelect_builder{All: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	return levyanv1.GetProductResponse_builder{Product: product(t)}.Build(), nil
}

func (b PriceBook) ListMeters(ctx context.Context, _ *levyanv1.ListMetersRequest) (*levyanv1.ListMetersResponse, error) {
	var out []*levyanv1.Meter
	after := ""
	for {
		vs, err := b.S.Meter().List(ctx, levyan.MeterListRequest_builder{Size: page, After: after}.Build())
		if err != nil {
			return nil, err
		}
		for _, v := range vs.GetItems() {
			out = append(out, meter(v))
		}
		if after = vs.GetNext(); after == "" {
			break
		}
	}

	return levyanv1.ListMetersResponse_builder{Meters: out}.Build(), nil
}

func (b PriceBook) ListPrices(ctx context.Context, req *levyanv1.ListPricesRequest) (*levyanv1.ListPricesResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}

	m, err := b.S.Meter().Get(ctx, levyan.MeterGetRequest_builder{
		Ref: levyan.MeterRef_builder{Keyed: levyan.MeterRefByKeyed_builder{
			Key:    proto.String(req.GetMeter()),
			Tenant: levyan.TenantRef_builder{Id: f.Tenant.Bytes()}.Build(),
		}.Build()}.Build(),
		Select: levyan.MeterSelect_builder{Key: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "no meter is %q", req.GetMeter())
		}
		return nil, err
	}

	var out []*levyanv1.Price
	after := ""
	for {
		vs, err := b.S.Price().List(ctx, levyan.PriceListRequest_builder{
			Filters: []*levyan.PriceFilter{levyan.PriceFilter_builder{
				Meter: levyan.MeterRef_builder{Id: m.GetId()}.Build(),
			}.Build()},
			Size:  page,
			After: after,
		}.Build())
		if err != nil {
			return nil, err
		}
		for _, v := range vs.GetItems() {
			out = append(out, price(v, m.GetKey()))
		}
		if after = vs.GetNext(); after == "" {
			break
		}
	}

	return levyanv1.ListPricesResponse_builder{Prices: out}.Build(), nil
}

func product(t *levyan.Tenant) *levyanv1.Product {
	return levyanv1.Product_builder{
		Id:    text(t.GetId()),
		Alias: t.GetAlias(),
		Name:  t.GetName(),
	}.Build()
}

func meter(v *levyan.Meter) *levyanv1.Meter {
	return levyanv1.Meter_builder{
		Key:  v.GetKey(),
		Name: v.GetName(),
		Unit: v.GetUnit(),
		// The API's enums are the schema's, value for value; a test holds
		// them to it (api_test.go).
		Kind:          levyanv1.MeterKind(v.GetKind()),
		Aggregation:   levyanv1.Aggregation(v.GetAggregation()),
		UnboundedZero: v.GetUnboundedZero(),
	}.Build()
}

func price(v *levyan.Price, key string) *levyanv1.Price {
	p := levyanv1.Price_builder{
		Id:        text(v.GetId()),
		Meter:     key,
		Channel:   levyanv1.Channel(v.GetChannel()),
		Currency:  v.GetCurrency(),
		Effective: stamp(v.GetDateEffective()),
		Existing:  stamp(v.GetDateExisting()),
		Published: stamp(v.GetDateCreated()),
		Desc:      v.GetDesc(),
	}.Build()

	if bs := v.GetBands().GetBands(); len(bs) > 0 {
		out := make([]*levyanv1.Band, 0, len(bs))
		for _, b := range bs {
			band := levyanv1.Band_builder{Fee: b.GetFee()}.Build()
			if b.HasUpTo() {
				band.SetUpTo(b.GetUpTo())
			}
			out = append(out, band)
		}
		p.SetBanded(levyanv1.Banded_builder{Bands: out}.Build())
	} else {
		p.SetPerUnit(levyanv1.PerUnit_builder{
			Included:  v.GetIncluded(),
			UnitPrice: v.GetUnitPrice(),
			Per:       v.GetPer(),
		}.Build())
	}

	return p
}

// text is an identifier as the API writes one: its canonical text, and empty
// for bytes that are none.
func text(b []byte) string {
	id, err := pdid.From(b)
	if err != nil {
		return ""
	}

	return id.String()
}

func stamp(t *timestamppb.Timestamp) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}

	return timestamppb.New(t.AsTime())
}
