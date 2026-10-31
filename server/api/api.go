// Package api is `levyan.v1` served: what a product calls, answered from its
// price book (docs/design.md, "앱이 levyan을 부르는 길").
//
// Every method reads through the stack a caller reaches, with the caller's
// frame, so the wall keeps a product to its own rows here as it does
// everywhere, and asks it nothing it could get round by naming another
// product. What it answers is `levyan.v1`'s messages and never the schema's:
// the schema is levyan's to change, and the API is what was promised to the
// apps that call it.
package api

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
)

// PriceBook is `levyan.v1.PriceBookService`.
type PriceBook struct {
	levyanv1.UnimplementedPriceBookServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server
}

// Register serves `levyan.v1` on g, from s -- which is the walled stack, and
// never the one the deployment works through.
func Register(g grpc.ServiceRegistrar, s levyan.Server) {
	levyanv1.RegisterPriceBookServiceServer(g, PriceBook{S: s})
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
