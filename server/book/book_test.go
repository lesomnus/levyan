package book_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pderr"
	"github.com/lesomnus/payday/pdid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent/audit"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/pricing"
)

var (
	stock = levyan.MeterKind_METER_KIND_STOCK
	flow  = levyan.MeterKind_METER_KIND_FLOW
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	last  = levyan.Aggregation_AGGREGATION_LAST
	sum   = levyan.Aggregation_AGGREGATION_SUM
	pgkr  = levyan.Channel_CHANNEL_PG_KR
)

// field is the field a refusal names.
func field(t *testing.T, err error) string {
	t.Helper()
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	vs := pderr.Violations(err)
	require.Len(t, vs, 1, "%v", err)
	return vs[0].Field
}

func TestAMeterIsWhatPaydaysMetersCanBeFiledUnder(t *testing.T) {
	w := levyantest.New(t)
	x := w.X
	ctx := context.Background()
	loft := w.Product("loft")

	add := func(key, unit string, kind levyan.MeterKind, agg levyan.Aggregation, unbounded bool) error {
		_, err := w.S.Ungated.Meter().Add(ctx, levyan.MeterAddRequest_builder{
			Tenant: levyantest.Ref(loft), Key: key, Unit: unit, Kind: kind, Aggregation: agg, UnboundedZero: unbounded,
		}.Build())
		return err
	}

	x.NoError(add("loft.assets.active", "{asset}", stock, twa, false))
	x.NoError(add("loft.history.keep", "d", stock, last, true), "a retention window, whose 0 is forever")
	x.NoError(add("loft.transfer.bytes", "By", flow, sum, false))

	x.Equal("key", field(t, add("Loft Assets", "{asset}", stock, twa, false)))
	x.Equal("key", field(t, add("loft", "{asset}", stock, twa, false)), "the product's name and a word at least")
	x.Equal("unit", field(t, add("loft.assets.all", "", stock, twa, false)), "a number nobody can price")
	x.Equal("unit", field(t, add("loft.assets.all", "a unit", stock, twa, false)))
	x.Equal("kind", field(t, add("loft.assets.all", "{asset}", levyan.MeterKind_METER_KIND_UNSPECIFIED, twa, false)))
	x.Equal("aggregation", field(t, add("loft.assets.all", "{asset}", stock, sum, false)), "a sum of what was held depends on how often somebody looked")
	x.Equal("aggregation", field(t, add("loft.transfer.peak", "By", flow, levyan.Aggregation_AGGREGATION_MAX, false)), "and the most of what happened is a rate nobody measured")
	x.Equal("unbounded_zero", field(t, add("loft.calls", "{call}", flow, sum, true)), "what happens is never forever")

	// A key is one meter's for good: neither taken twice nor freed.
	x.Equal(codes.AlreadyExists, status.Code(add("loft.assets.active", "{asset}", stock, twa, false)))
	_, err := w.S.Ungated.Meter().Get(ctx, levyan.MeterGetRequest_builder{
		Ref: levyan.MeterRef_builder{Keyed: levyan.MeterRefByKeyed_builder{Key: proto.String("loft.assets.active"), Tenant: levyantest.Ref(loft)}.Build()}.Build(),
	}.Build())
	x.NoError(err)

	// But another product's meters are its own, under any key.
	other := w.Product("other")
	_, err = w.S.Ungated.Meter().Add(ctx, levyan.MeterAddRequest_builder{
		Tenant: levyantest.Ref(other), Key: "loft.assets.active", Unit: "{asset}", Kind: stock, Aggregation: twa,
	}.Build())
	x.NoError(err)
}

func TestWhatAVersionOfAPriceMayBe(t *testing.T) {
	w := levyantest.New(t)
	x := w.X
	ctx := context.Background()
	loft := w.Product("loft")
	assets := w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)
	soon := time.Now().Add(time.Hour).Truncate(time.Microsecond)

	add := func(f func(b *levyan.PriceAddRequest_builder)) (*levyan.Price, error) {
		b := levyan.PriceAddRequest_builder{
			Tenant:        levyantest.Ref(loft),
			Meter:         levyan.MeterRef_builder{Id: assets.GetId()}.Build(),
			Channel:       pgkr,
			Currency:      "KRW",
			Included:      100,
			UnitPrice:     50,
			DateEffective: timestamppb.New(soon),
		}
		f(&b)
		return w.S.Ungated.Price().Add(ctx, b.Build())
	}
	refused := func(f func(b *levyan.PriceAddRequest_builder)) string {
		_, err := add(f)
		return field(t, err)
	}

	v, err := add(func(b *levyan.PriceAddRequest_builder) {})
	x.NoError(err)
	x.Equal(int64(1), v.GetPer(), "a unit price unset per is per one unit")
	x.True(v.GetDateExisting().AsTime().Equal(soon), "and for the customers already there from when it is for the new ones")

	v, err = add(func(b *levyan.PriceAddRequest_builder) { b.DateEffective = nil })
	x.NoError(err)
	x.True(v.GetDateEffective().AsTime().Equal(v.GetDateCreated().AsTime()), "unset is from the moment it is published")

	x.Equal("date_effective", refused(func(b *levyan.PriceAddRequest_builder) {
		b.DateEffective = timestamppb.New(time.Now().Add(-time.Hour))
	}), "a price does not reach back")
	x.Equal("date_existing", refused(func(b *levyan.PriceAddRequest_builder) {
		b.DateExisting = timestamppb.New(soon.Add(-time.Minute))
	}))
	x.Equal("currency", refused(func(b *levyan.PriceAddRequest_builder) { b.Currency = "KWR" }))
	x.Equal("currency", refused(func(b *levyan.PriceAddRequest_builder) { b.Currency = "krw" }))
	x.Equal("channel", refused(func(b *levyan.PriceAddRequest_builder) { b.Channel = levyan.Channel_CHANNEL_UNSPECIFIED }))
	x.Equal("unit_price", refused(func(b *levyan.PriceAddRequest_builder) { b.UnitPrice = -1 }))
	x.Equal("included", refused(func(b *levyan.PriceAddRequest_builder) { b.Included = -1 }))
	x.Equal("bands", refused(func(b *levyan.PriceAddRequest_builder) {
		b.Bands = levyan.PriceBands_builder{Bands: []*levyan.PriceBand{levyan.PriceBand_builder{Fee: 1}.Build()}}.Build()
	}), "by the unit and by bands at once")
	x.Equal("bands", refused(func(b *levyan.PriceAddRequest_builder) {
		b.Included, b.UnitPrice = 0, 0
		b.Bands = levyan.PriceBands_builder{Bands: []*levyan.PriceBand{
			levyan.PriceBand_builder{UpTo: proto.Int64(365), Fee: 0}.Build(),
		}}.Build()
	}), "bands that do not hold every value")
	x.Equal("meter", refused(func(b *levyan.PriceAddRequest_builder) {
		b.Meter = levyan.MeterRef_builder{Id: pdid.New(10).Bytes()}.Build()
	}))

	// A meter of another product is no meter of this one's prices -- the
	// sink's to refuse, for the deployment's own work as much as a caller's.
	other := w.Product("other")
	things := w.Meter(other, "other.things", "{thing}", stock, twa)
	_, err = add(func(b *levyan.PriceAddRequest_builder) { b.Meter = levyan.MeterRef_builder{Id: things.GetId()}.Build() })
	x.Error(err)

	// Whatever a request says of when it was published, it was published now.
	v, err = add(func(b *levyan.PriceAddRequest_builder) { b.DateCreated = timestamppb.New(time.Unix(0, 0)) })
	x.NoError(err)
	x.WithinDuration(time.Now(), v.GetDateCreated().AsTime(), time.Minute)
}

// A retention window by bands: a year free, three years for a fee, forever
// for more -- forever being 0 on a meter that says so.
func TestARetentionWindowIsPricedByBands(t *testing.T) {
	w := levyantest.New(t)
	x := w.X
	ctx := context.Background()
	loft := w.Product("loft")

	kept, err := w.S.Ungated.Meter().Add(ctx, levyan.MeterAddRequest_builder{
		Tenant: levyantest.Ref(loft), Key: "loft.history.keep", Unit: "d", Kind: stock, Aggregation: last, UnboundedZero: true,
	}.Build())
	x.NoError(err)

	v, err := w.S.Ungated.Price().Add(ctx, levyan.PriceAddRequest_builder{
		Tenant:   levyantest.Ref(loft),
		Meter:    levyan.MeterRef_builder{Id: kept.GetId()}.Build(),
		Channel:  pgkr,
		Currency: "KRW",
		Bands: levyan.PriceBands_builder{Bands: []*levyan.PriceBand{
			levyan.PriceBand_builder{UpTo: proto.Int64(365), Fee: 0}.Build(),
			levyan.PriceBand_builder{UpTo: proto.Int64(1095), Fee: 10000}.Build(),
			levyan.PriceBand_builder{Fee: 30000}.Build(),
		}}.Build(),
	}.Build())
	x.NoError(err)
	x.Equal(int64(0), v.GetPer(), "a price by bands is per nothing")

	p, err := pricing.VersionOf(v, kept.GetUnboundedZero())
	x.NoError(err)
	for value, fee := range map[int64]int64{0: 30000, 30: 0, 365: 0, 366: 10000, 1095: 10000, 3650: 30000} {
		a, err := p.Charge(pricing.Whole(value))
		x.NoError(err)
		x.Equal(fee, a.Minor, "%d days", value)
	}
}

// A version is never changed. Publishing the next leaves the one before as
// it was, both are there to be asked which was in force when, and there is no
// method that would change or erase one.
func TestAVersionIsNeverChanged(t *testing.T) {
	w := levyantest.New(t)
	x := w.X
	ctx := context.Background()
	loft := w.Product("loft")
	assets := w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)

	first := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	v1 := w.Price(loft, assets, pgkr, "KRW", 100, 50, first)
	v2 := w.Price(loft, assets, pgkr, "KRW", 100, 70, first.Add(30*24*time.Hour))

	got, err := w.S.Ungated.Price().Get(ctx, levyan.PriceGetRequest_builder{
		Ref:    levyan.PriceRef_builder{Id: v1.GetId()}.Build(),
		Select: levyan.PriceSelect_builder{All: proto.Bool(true)}.Build(),
	}.Build())
	x.NoError(err)
	x.Equal(int64(50), got.GetUnitPrice())
	x.True(got.GetDateEffective().AsTime().Equal(first))

	vs, err := w.S.Ungated.Price().List(ctx, levyan.PriceListRequest_builder{
		Filters: []*levyan.PriceFilter{levyan.PriceFilter_builder{Meter: levyan.MeterRef_builder{Id: assets.GetId()}.Build()}.Build()},
	}.Build())
	x.NoError(err)
	x.Len(vs.GetItems(), 2)

	// The price service is Add, Get and List, and no more.
	sd := levyan.File_levyan_price_svc_g_proto.Services().ByName("PriceService")
	x.NotNil(sd)
	methods := []string{}
	for i := range sd.Methods().Len() {
		methods = append(methods, string(sd.Methods().Get(i).Name()))
	}
	x.ElementsMatch([]string{"Add", "Get", "List"}, methods)

	// Each version is on the trail, written by the server that wrote it.
	for _, v := range []*levyan.Price{v1, v2} {
		id, err := pdid.From(v.GetId())
		x.NoError(err)
		n, err := w.S.Ent.Audit.Query().Where(audit.ObjectId(id.Uuid())).Count(ctx)
		x.NoError(err)
		x.Equal(1, n)
	}
}
