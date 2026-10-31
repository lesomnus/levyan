package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/pdtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cli"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/keys"
)

var (
	stock = levyan.MeterKind_METER_KIND_STOCK
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	krw   = levyan.Channel_CHANNEL_PG_KR
)

// A product calls with its key, and is answered about itself: its own meters
// and its own prices, and of another product's nothing -- not even that it is
// there. The wall is what keeps them apart, so asking by another product's
// meter's key is NotFound rather than a refusal.
func TestAProductIsAnsweredAboutItselfAndNoOther(t *testing.T) {
	w := levyantest.New(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	loft, other := w.Product("loft"), w.Product("other")
	loftKey, _ := w.Key(loft, "loft-prod", nil)
	otherKey, _ := w.Key(other, "other-prod", nil)

	at := time.Now().Add(time.Hour)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)
	w.Price(loft, assets, krw, "KRW", 100, 50, at)
	things := w.Meter(other, "other.things", "{thing}", stock, twa)
	w.Price(other, things, krw, "KRW", 0, 10, at)

	books := levyanv1.NewPriceBookServiceClient(w.Serve())

	p, err := books.GetProduct(levyantest.As(ctx, loftKey), &levyanv1.GetProductRequest{})
	x.NoError(err)
	x.Equal("loft", p.GetProduct().GetAlias())

	ms, err := books.ListMeters(levyantest.As(ctx, loftKey), &levyanv1.ListMetersRequest{})
	x.NoError(err)
	x.Len(ms.GetMeters(), 1)
	x.Equal("loft.assets.active", ms.GetMeters()[0].GetKey())
	x.Equal("{asset}", ms.GetMeters()[0].GetUnit())
	x.Equal(levyanv1.MeterKind_METER_KIND_STOCK, ms.GetMeters()[0].GetKind())
	x.Equal(levyanv1.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE, ms.GetMeters()[0].GetAggregation())

	ps, err := books.ListPrices(levyantest.As(ctx, loftKey), levyanv1.ListPricesRequest_builder{Meter: "loft.assets.active"}.Build())
	x.NoError(err)
	x.Len(ps.GetPrices(), 1)
	x.Equal(int64(100), ps.GetPrices()[0].GetPerUnit().GetIncluded())
	x.Equal(int64(50), ps.GetPrices()[0].GetPerUnit().GetUnitPrice())
	x.Equal("KRW", ps.GetPrices()[0].GetCurrency())
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, ps.GetPrices()[0].GetChannel())

	_, err = books.ListPrices(levyantest.As(ctx, loftKey), levyanv1.ListPricesRequest_builder{Meter: "other.things"}.Build())
	x.Equal(codes.NotFound, status.Code(err), "another product's meter is no meter here")

	// And the other product is told about itself, by its own key.
	p, err = books.GetProduct(levyantest.As(ctx, otherKey), &levyanv1.GetProductRequest{})
	x.NoError(err)
	x.Equal("other", p.GetProduct().GetAlias())
	ms, err = books.ListMeters(levyantest.As(ctx, otherKey), &levyanv1.ListMetersRequest{})
	x.NoError(err)
	x.Len(ms.GetMeters(), 1)
	x.Equal("other.things", ms.GetMeters()[0].GetKey())
	_, err = books.ListPrices(levyantest.As(ctx, otherKey), levyanv1.ListPricesRequest_builder{Meter: "loft.assets.active"}.Build())
	x.ErrCode(codes.NotFound, err)
}

// What is not a key opens nothing: no key, a string that is none, a key
// revoked, a key that ran out. Each is Unauthenticated, and alike.
func TestOnlyALiveKeyIsAnybody(t *testing.T) {
	w := levyantest.New(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	loft := w.Product("loft")
	live, _ := w.Key(loft, "live", nil)
	revoked, row := w.Key(loft, "revoked", nil)
	ran := time.Now().Add(-time.Minute)
	expired, _ := w.Key(loft, "expired", &ran)
	later := time.Now().Add(time.Hour)
	until, _ := w.Key(loft, "until", &later)

	_, err := w.S.Ungated.ServiceKey().Erase(ctx, levyan.ServiceKeyRef_builder{Id: row.GetId()}.Build())
	x.NoError(err)

	books := levyanv1.NewPriceBookServiceClient(w.Serve())
	call := func(token string) error {
		c := ctx
		if token != "" {
			c = levyantest.As(ctx, token)
		}
		_, err := books.GetProduct(c, &levyanv1.GetProductRequest{})
		return err
	}

	x.NoError(call(live))
	x.NoError(call(until), "a key that has not run out yet")
	x.Equal(codes.Unauthenticated, status.Code(call("")), "no key")
	x.Equal(codes.Unauthenticated, status.Code(call("lvk_"+"nothing-anybody-was-issued")), "a key nobody was issued")
	x.ErrCode(codes.Unauthenticated, call("not even the shape of one"))
	x.Equal(codes.Unauthenticated, status.Code(call(revoked)), "a key revoked")
	x.Equal(codes.Unauthenticated, status.Code(call(expired)), "a key that ran out")

	// What is stored is the key's hash, and nothing answers with that.
	got, err := w.S.Ungated.ServiceKey().Get(ctx, levyan.ServiceKeyGetRequest_builder{
		Ref:    levyan.ServiceKeyRef_builder{Secret: keys.Sum(live)}.Build(),
		Select: levyan.ServiceKeySelect_builder{All: proto.Bool(true)}.Build(),
	}.Build())
	x.NoError(err)
	x.Empty(got.GetSecret())
}

// A product may call what the policy table gives a product, and nothing else
// -- the generated methods of the schema least of all, which would have it
// writing its own prices.
func TestAProductCallsLevyanV1AndNothingElse(t *testing.T) {
	w := levyantest.New(t)
	x := pdtest.NewX(t)

	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)
	ctx := levyantest.As(context.Background(), key)
	conn := w.Serve()

	_, err := levyan.NewMeterServiceClient(conn).List(ctx, levyan.MeterListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "the schema's own reads are not an API")

	_, err = levyan.NewMeterServiceClient(conn).Add(ctx, levyan.MeterAddRequest_builder{
		Tenant: levyantest.Ref(loft), Key: "loft.free.lunch", Unit: "{lunch}",
		Kind: stock, Aggregation: twa,
	}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "a product writes no meter of its own")

	_, err = levyan.NewPriceServiceClient(conn).Add(ctx, levyan.PriceAddRequest_builder{Tenant: levyantest.Ref(loft)}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor a price")

	_, err = levyan.NewServiceKeyServiceClient(conn).List(ctx, levyan.ServiceKeyListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor reads its keys")

	_, err = levyan.NewReadingServiceClient(conn).Add(ctx, levyan.ReadingAddRequest_builder{Tenant: levyantest.Ref(loft)}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "a reading is recorded through levyan.v1, which says what came of it, and never written as a row")

	_, err = levyan.NewPointServiceClient(conn).List(ctx, levyan.PointListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor the ledger read as rows")

	_, err = levyan.NewWorkspaceServiceClient(conn).Patch(ctx, levyan.WorkspacePatchRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor a workspace patched, which no caller may")

	_, err = levyan.NewPeriodServiceClient(conn).Add(ctx, levyan.PeriodAddRequest_builder{Tenant: levyantest.Ref(loft)}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor a period of its own closed")

	_, err = levyan.NewTenantServiceClient(conn).Patch(ctx, levyan.TenantPatchRequest_builder{
		Ref: levyantest.Ref(loft), Name: proto.String("free"),
	}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nor renames itself")

	// Nor around the table by a batch, which is asked of each operation.
	op, err := anypb.New(levyan.MeterListRequest_builder{}.Build())
	x.NoError(err)
	_, err = pdpb.NewBatchServiceClient(conn).Do(ctx, pdpb.BatchRequest_builder{
		Ops: []*pdpb.Op{pdpb.Op_builder{Method: "/levyan.MeterService/List", Request: op}.Build()},
	}.Build())
	x.ErrCode(codes.PermissionDenied, err)
}

// A server is not served without a door: nothing that says who is calling,
// or nothing that says what they may do, is a server that does not start --
// rather than one that believes what a caller writes about itself, or lets
// every caller do everything in its own tenant.
func TestNothingIsServedOpen(t *testing.T) {
	w := levyantest.New(t)
	x := pdtest.NewX(t)

	_, err := w.S.Grpc(t.Context(), w.C)
	x.Error(err, "no door at all")

	cli.Front(w.S)
	s := *w.S
	s.Auth = nil
	_, err = s.Grpc(t.Context(), w.C)
	x.Error(err, "who is calling is not said")

	s = *w.S
	s.Policy = nil
	_, err = s.Grpc(t.Context(), w.C)
	x.Error(err, "what they may do is not said")

	_, err = w.S.Grpc(t.Context(), w.C)
	x.NoError(err, "and with both, it is served")
}
