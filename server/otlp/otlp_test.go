package otlp_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/meter"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entpoint "github.com/lesomnus/levyan/internal/ent/point"
	entseries "github.com/lesomnus/levyan/internal/ent/series"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/usage"
	"github.com/lesomnus/levyan/server/pd"
)

var (
	stock = levyan.MeterKind_METER_KIND_STOCK
	flow  = levyan.MeterKind_METER_KIND_FLOW
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	sum   = levyan.Aggregation_AGGREGATION_SUM
)

func day(d, h int) time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d).Add(time.Duration(h) * time.Hour)
}

type world struct {
	*levyantest.World
	loft *levyan.Tenant
	key  string
	conn *grpc.ClientConn
	ctx  context.Context
}

func setup(t *testing.T) world {
	w := levyantest.New(t)
	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)
	w.Meter(loft, "loft.transfer.bytes", "By", flow, sum)
	w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)

	return world{World: w, loft: loft, key: key, conn: w.Serve(), ctx: levyantest.As(context.Background(), key)}
}

// increase is what the ledger says workspace `ws` did of `key` over
// [from, to), from the points it kept.
func (w world) increase(t *testing.T, ws pdid.Id, key string, from, to time.Time) int64 {
	t.Helper()
	ctx := context.Background()

	wr, err := w.S.Ent.Workspace.Query().Where(entworkspace.Key(ws.Bytes())).Only(ctx)
	w.X.NoError(err)
	ss, err := w.S.Ent.Series.Query().Where(entseries.WorkspaceId(wr.Id)).WithMeter().All(ctx)
	w.X.NoError(err)

	var total int64
	for _, s := range ss {
		if s.Edges.Meter.Key != key {
			continue
		}
		vs, err := w.S.Ent.Point.Query().Where(entpoint.SeriesId(s.Id)).All(ctx)
		w.X.NoError(err)
		ps := []usage.Point{}
		for _, v := range vs {
			ps = append(ps, usage.Point{Start: v.StartedAt, At: v.CountedAt, Value: v.Value})
		}
		n, err := usage.Increase(ps, from, to)
		w.X.NoError(err)
		total += n
	}

	return total
}

func (w world) points(t *testing.T) []*ent.Point {
	t.Helper()
	vs, err := w.S.Ent.Point.Query().Order(ent.Asc(entpoint.FieldCountedAt)).All(context.Background())
	w.X.NoError(err)
	return vs
}

// A product's flow, exported by OpenTelemetry's own exporter as payday's
// convention says -- a counter, cumulative, with the workspace in
// `payday.tenant`, the key as `authorization` -- is kept by its series, and
// what each workspace did is what was counted for it, a process that
// restarted included.
func TestAFlowExportedByOpenTelemetryIsKept(t *testing.T) {
	w := setup(t)
	x := w.X
	ctx := context.Background()
	a, b := pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain)

	process := func(instance string) (*sdkmetric.MeterProvider, metric.Int64Counter) {
		exp, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithGRPCConn(w.conn),
			otlpmetricgrpc.WithHeaders(map[string]string{"authorization": "Bearer " + w.key}),
			otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}),
		)
		x.NoError(err)
		p := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(time.Hour))),
			sdkmetric.WithResource(resource.NewSchemaless(
				attribute.String("service.name", "loft"),
				attribute.String("service.instance.id", instance),
				attribute.String("process.command_args", "loft serve --secret hunter2"),
			)),
		)
		c, err := p.Meter("github.com/lesomnus/loft").Int64Counter("loft.transfer.bytes", metric.WithUnit("By"))
		x.NoError(err)
		return p, c
	}
	in := func(ws pdid.Id) metric.AddOption {
		return metric.WithAttributes(attribute.String(meter.TenantAttribute, ws.String()))
	}

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	one, sent := process("one")
	sent.Add(ctx, 100, in(a))
	sent.Add(ctx, 7, in(b))
	x.NoError(one.ForceFlush(ctx))
	x.Len(w.points(t), 2)

	// Exported again with nothing counted since: the same count, which adds
	// nothing and is not written again.
	x.NoError(one.ForceFlush(ctx))
	x.Len(w.points(t), 2)

	sent.Add(ctx, 50, in(a))
	x.NoError(one.ForceFlush(ctx))
	x.Len(w.points(t), 3)
	x.Equal(int64(150), w.increase(t, a, "loft.transfer.bytes", from, to))
	x.Equal(int64(7), w.increase(t, b, "loft.transfer.bytes", from, to))

	// The process restarts as the same instance: the same series, counting
	// again from nothing under a start of its own.
	x.NoError(one.Shutdown(ctx))
	again, sent := process("one")
	sent.Add(ctx, 30, in(a))
	x.NoError(again.ForceFlush(ctx))
	x.Equal(int64(180), w.increase(t, a, "loft.transfer.bytes", from, to))

	// And a process beside it is a series of its own.
	two, sent := process("two")
	sent.Add(ctx, 5, in(a))
	x.NoError(two.ForceFlush(ctx))
	x.Equal(int64(185), w.increase(t, a, "loft.transfer.bytes", from, to))

	ss, err := w.S.Ent.Series.Query().Order(ent.Asc(entseries.FieldSource)).All(ctx)
	x.NoError(err)
	x.Len(ss, 3)
	x.Equal("loft/one", ss[0].Source, "where it came from, and nothing else the resource said")
	x.Equal("loft/two", ss[2].Source)

	x.NoError(again.Shutdown(ctx))
	x.NoError(two.Shutdown(ctx))
}

func sumOf(name, unit string, ps ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Unit: unit,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			IsMonotonic:            true,
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			DataPoints:             ps,
		}},
	}
}

func point(ws pdid.Id, start, at time.Time, v int64) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		Attributes: []*commonpb.KeyValue{{
			Key:   meter.TenantAttribute,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ws.String()}},
		}},
		StartTimeUnixNano: uint64(start.UnixNano()),
		TimeUnixNano:      uint64(at.UnixNano()),
		Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
	}
}

func export(ms ...*metricspb.Metric) *collectormetrics.ExportMetricsServiceRequest {
	return &collectormetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key:   "service.name",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "loft"}},
		}}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "github.com/lesomnus/loft"},
			Metrics: ms,
		}},
	}}}
}

func (w world) export(t *testing.T, ms ...*metricspb.Metric) int64 {
	t.Helper()
	res, err := collectormetrics.NewMetricsServiceClient(w.conn).Export(w.ctx, export(ms...))
	w.X.NoError(err)
	return res.GetPartialSuccess().GetRejectedDataPoints()
}

// Points come as they come: out of order, twice, after a restart. What a
// period's points come to is the same however they came, and a point sent
// again with another count is refused rather than either count chosen.
func TestPointsAddUpHoweverTheyCome(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)
	s1, s2 := day(0, 0), day(3, 0)
	bytes := func(ps ...*metricspb.NumberDataPoint) *metricspb.Metric {
		return sumOf("loft.transfer.bytes", "By", ps...)
	}

	x.Zero(w.export(t, bytes(point(ws, s1, day(1, 0), 20))))
	x.Zero(w.export(t, bytes(point(ws, s1, day(0, 6), 10))), "out of order")
	x.Zero(w.export(t, bytes(point(ws, s1, day(1, 0), 20))), "twice")
	x.Equal(int64(1), w.export(t, bytes(point(ws, s1, day(1, 0), 25))), "twice, and not the same")
	x.Zero(w.export(t, bytes(point(ws, s1, day(2, 0), 20))), "no more than before")
	x.Zero(w.export(t, bytes(point(ws, s2, day(3, 1), 5))), "counting again from a start of its own")
	x.Zero(w.export(t, bytes(point(ws, s1, day(0, 12), 15))), "between two that came before it")
	x.Zero(w.export(t, bytes(point(ws, s1, day(0, 12), 15), point(ws, s1, day(0, 12), 15))), "twice in one export")

	x.Equal(int64(15), w.increase(t, ws, "loft.transfer.bytes", day(0, 0), day(1, 0)))
	x.Equal(int64(10), w.increase(t, ws, "loft.transfer.bytes", day(1, 0), day(5, 0)))
	x.Equal(int64(25), w.increase(t, ws, "loft.transfer.bytes", day(-1, 0), day(5, 0)), "the most of each start, added")

	got := []int64{}
	for _, v := range w.points(t) {
		got = append(got, v.Value)
	}
	x.Equal([]int64{10, 15, 20, 5}, got, "and the one that added nothing is not kept")
}

// A point that speaks for a period whose close has begun is kept, and marked
// late; one that does not, is not.
func TestAPointForAClosedPeriodIsLate(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)
	start := day(-20, 0)
	bytes := func(ps ...*metricspb.NumberDataPoint) *metricspb.Metric {
		return sumOf("loft.transfer.bytes", "By", ps...)
	}

	x.Zero(w.export(t, bytes(point(ws, start, day(1, 0), 10), point(ws, start, day(35, 0), 40))))

	// September is closed; October is not.
	_, err := w.S.Ungated.Period().Add(context.Background(), levyan.PeriodAddRequest_builder{
		Tenant:    levyantest.Ref(w.loft),
		DateStart: timestamppb.New(day(0, 0)),
		DateEnd:   timestamppb.New(day(30, 0)),
		Zone:      "UTC",
	}.Build())
	x.NoError(err)

	x.Zero(w.export(t, bytes(
		point(ws, start, day(-5, 0), 4),  // August, before any period, and taking from a point in September
		point(ws, start, day(10, 0), 20), // September
		point(ws, start, day(33, 0), 30), // October
		point(ws, start, day(40, 0), 50), // October, after every other
	)))

	late := map[int64]bool{}
	for _, v := range w.points(t) {
		late[v.Value] = v.Late
	}
	x.Equal(map[int64]bool{4: true, 10: false, 20: true, 30: false, 40: false, 50: false}, late)
}

// What is not a flow of the product's, as payday's convention says one is, is
// refused point by point, and the rest of the export is kept.
func TestWhatIsNotAFlowIsRefused(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)
	s, at := day(0, 0), day(1, 0)

	gauge := &metricspb.Metric{Name: "loft.transfer.bytes", Unit: "By", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
		DataPoints: []*metricspb.NumberDataPoint{point(ws, s, at, 1)},
	}}}
	delta := sumOf("loft.transfer.bytes", "By", point(ws, s, at, 1), point(ws, s, at, 2))
	delta.GetSum().AggregationTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	updown := sumOf("loft.transfer.bytes", "By", point(ws, s, at, 1))
	updown.GetSum().IsMonotonic = false
	double := point(ws, s, at, 0)
	double.Value = &metricspb.NumberDataPoint_AsDouble{AsDouble: 1.5}
	nobody := point(ws, s, at, 1)
	nobody.Attributes = nil
	holder := point(pdid.New(pd.HolderDomain), s, at, 1)
	unstarted := point(ws, s, at, 1)
	unstarted.StartTimeUnixNano = 0
	backwards := point(ws, at, s, 1)
	empty := point(ws, s, at, 1)
	empty.Flags = uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)
	ahead := point(ws, s, time.Now().Add(time.Hour), 1)

	res, err := collectormetrics.NewMetricsServiceClient(w.conn).Export(w.ctx, export(
		sumOf("loft.transfer.bytes", "By", point(ws, s, at, 100)),
		gauge,  // 1
		delta,  // 2
		updown, // 1
		sumOf("loft.transfer.bytes", "By", double, nobody, holder, unstarted, backwards, empty, ahead), // 7
		sumOf("loft.transfer.packets", "{packet}", point(ws, s, at, 1)),                                // 1
		sumOf("loft.assets.active", "{asset}", point(ws, s, at, 1)),                                    // 1
		sumOf("loft.transfer.bytes", "KiBy", point(ws, s, day(2, 0), 1)),                               // 1
	))
	x.NoError(err)
	x.Equal(int64(14), res.GetPartialSuccess().GetRejectedDataPoints())
	x.Contains(res.GetPartialSuccess().GetErrorMessage(), "14 points not kept")
	x.Len(w.points(t), 1, "and the one that was a flow is kept")
	x.Equal(int64(100), w.points(t)[0].Value)
}

// The receiver is a product's door like any other: a key or nothing, and a
// product's own meters and no other's.
func TestTheReceiverIsAProductsOwn(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)
	ws := pdid.New(pd.TenantDomain)
	req := export(sumOf("loft.transfer.bytes", "By", point(ws, day(0, 0), day(1, 0), 1)))
	c := collectormetrics.NewMetricsServiceClient(w.conn)

	_, err := c.Export(context.Background(), req)
	x.ErrCode(codes.Unauthenticated, err)

	other := w.Product("other")
	key, _ := w.Key(other, "other-prod", nil)
	res, err := c.Export(levyantest.As(context.Background(), key), req)
	x.NoError(err)
	x.Equal(int64(1), res.GetPartialSuccess().GetRejectedDataPoints(), "loft's meter is nothing to another product")
	x.Empty(w.points(t))
}

// A point of a workspace its product destroyed (#14), counted at its
// destruction or after, is not kept, and the export says so; one counted
// before it is, and one kept before levyan was told is what it was.
func TestAPointAfterAWorkspacesDestructionIsRefused(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)
	start := day(0, 0)
	bytes := func(ps ...*metricspb.NumberDataPoint) *metricspb.Metric {
		return sumOf("loft.transfer.bytes", "By", ps...)
	}

	x.Zero(w.export(t, bytes(point(ws, start, day(1, 0), 10), point(ws, start, day(3, 0), 30))))
	product, err := pdid.From(w.loft.GetId())
	x.NoError(err)
	_, err = w.S.Payers.Destroy(context.Background(), product, ws, day(2, 0))
	x.NoError(err)

	res, err := collectormetrics.NewMetricsServiceClient(w.conn).Export(w.ctx, export(bytes(
		point(ws, start, day(1, 0), 10), // kept before, and again
		point(ws, start, day(3, 0), 30), // kept before levyan was told, and as it was
		point(ws, start, day(2, 0), 25), // at its destruction
		point(ws, start, day(4, 0), 40), // after it
	)))
	x.NoError(err)
	x.Equal(int64(2), res.GetPartialSuccess().GetRejectedDataPoints())
	x.Contains(res.GetPartialSuccess().GetErrorMessage(), "destroyed")
	x.Len(w.points(t), 2)
}
