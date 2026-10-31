// Package otlp is the receiver a product exports its flows to: OpenTelemetry's
// own `MetricsService`, over gRPC, on the address `levyan.v1` is served on and
// behind the same door -- the product's service key as `authorization: Bearer
// lvk_…`, the policy table, the rate limit, the wall.
//
// It takes what payday's convention for a flow is (`docs/guide/meters.md`, §7)
// and nothing else:
//
//   - a Sum, monotonic and cumulative, named by a flow meter's key and in its
//     unit -- never a gauge, a histogram, a delta, or a Sum that may go down;
//   - each point a whole number, with a start time, and the workspace it
//     counts for in `payday.tenant` (`meter.TenantAttribute`), as the
//     canonical text of the identifier of the product's own payday tenant.
//
// Each point is kept in the ledger by its series -- the stream OpenTelemetry
// says it is of: its resource, its scope, the Sum's name and its attributes --
// its start time, its time and its count (`server/ledger`). Anything else is
// refused point by point, as OpenTelemetry's partial success says, so that an
// exporter that sends a metric levyan does not bill by still has the rest of
// what it sent kept, and is told how many were not. A point sent again is
// kept once; an export that failed is one the exporter sends again.
//
// # Why gRPC
//
// One door. The receiver is a gRPC service registered beside `levyan.v1` on
// the same server, so a product's key opens it through the interceptors every
// call goes through, and the policy table says who may call it. OTLP over
// HTTP would be a second listener with a second copy of all of that. And
// OTLP/gRPC is what OpenTelemetry's exporters speak first: retries on the
// codes that mean "try again", partial success, gzip.
package otlp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/lesomnus/otx/log"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	// An exporter may compress what it sends, and gzip is the one
	// OpenTelemetry's exporters offer.
	_ "google.golang.org/grpc/encoding/gzip"

	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/meter"

	"github.com/lesomnus/levyan/server/ledger"
)

// Service is the receiver's service, by the name gRPC and the policy table
// know it by.
const Service = "opentelemetry.proto.collector.metrics.v1.MetricsService"

// Receiver is OpenTelemetry's `MetricsService`, into the ledger.
type Receiver struct {
	collectormetrics.UnimplementedMetricsServiceServer

	// L writes through the walled stack, with the caller's frame.
	L ledger.Ledger
}

// Register serves the receiver on g, writing through l.
func Register(g grpc.ServiceRegistrar, l ledger.Ledger) {
	collectormetrics.RegisterMetricsServiceServer(g, Receiver{L: l})
}

// refusals is what one export had refused: how many, and the first few
// reasons, which is what partial success says back.
type refusals struct {
	n    int64
	whys []string
}

func (r *refusals) add(n int, format string, args ...any) {
	if n <= 0 {
		return
	}
	r.n += int64(n)
	if len(r.whys) < 5 {
		r.whys = append(r.whys, fmt.Sprintf(format, args...))
	}
}

func (r *refusals) message() string {
	if r.n == 0 {
		return ""
	}

	more := ""
	if int64(len(r.whys)) < r.n {
		more = "; and more"
	}
	return fmt.Sprintf("%d points not kept: %s%s", r.n, strings.Join(r.whys, "; "), more)
}

func (r Receiver) Export(ctx context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}

	var no refusals
	ps := []ledger.Point{}
	for _, rm := range req.GetResourceMetrics() {
		res := rm.GetResource().GetAttributes()
		source := sourceOf(res)
		for _, sm := range rm.GetScopeMetrics() {
			scope := sm.GetScope()
			for _, m := range sm.GetMetrics() {
				sum := m.GetSum()
				switch {
				case sum == nil:
					no.add(count(m), "%s is not a Sum, and a flow is counted by a monotonic, cumulative Sum", m.GetName())
					continue
				case !sum.GetIsMonotonic():
					no.add(len(sum.GetDataPoints()), "%s is a Sum that may go down, and a flow only adds up", m.GetName())
					continue
				case sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
					no.add(len(sum.GetDataPoints()), "%s is %s, and a flow is exported cumulative: a point lost is then covered by the next", m.GetName(), sum.GetAggregationTemporality())
					continue
				}

				for _, dp := range sum.GetDataPoints() {
					if dp.GetFlags()&uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK) != 0 {
						no.add(1, "%s: a point with no value", m.GetName())
						continue
					}
					v, ok := dp.GetValue().(*metricspb.NumberDataPoint_AsInt)
					if !ok {
						no.add(1, "%s: a point that is not a whole number, and what is billed is counted in whole ones", m.GetName())
						continue
					}
					ws, ok := attribute(dp.GetAttributes(), meter.TenantAttribute)
					if !ok {
						no.add(1, "%s: a point that says no %s, which is the workspace it counts for", m.GetName(), meter.TenantAttribute)
						continue
					}
					if dp.GetStartTimeUnixNano() == 0 || dp.GetTimeUnixNano() == 0 {
						no.add(1, "%s: a point with no start time or no time, which is no count anybody can add up", m.GetName())
						continue
					}

					ps = append(ps, ledger.Point{
						Workspace: ws,
						Meter:     m.GetName(),
						Unit:      m.GetUnit(),
						Digest:    digest(res, scope, m.GetName(), dp.GetAttributes()),
						Source:    source,
						StartedAt: nanos(dp.GetStartTimeUnixNano()),
						CountedAt: nanos(dp.GetTimeUnixNano()),
						Value:     v.AsInt,
					})
				}
			}
		}
	}

	// A call's worth at a time, each in a transaction of its own. One that
	// fails after another was kept fails the export, which the exporter sends
	// again whole: what was kept is kept once.
	for len(ps) > 0 {
		n := min(len(ps), ledger.Most)
		vs, err := r.L.Count(ctx, f.Tenant, ps[:n])
		if err != nil {
			log.From(ctx).ErrorContext(ctx, "otlp: the ledger did not keep an export", slog.Any("err", err))
			return nil, status.Error(codes.Unavailable, "the points were not kept; send them again")
		}
		for _, v := range vs {
			if v.Outcome == ledger.Refused {
				no.add(1, "%s", v.Why)
			}
		}
		ps = ps[n:]
	}

	out := &collectormetrics.ExportMetricsServiceResponse{}
	if no.n > 0 {
		log.From(ctx).WarnContext(ctx, "otlp: points refused", slog.Int64("points", no.n), slog.String("why", no.message()))
		out.PartialSuccess = &collectormetrics.ExportMetricsPartialSuccess{
			RejectedDataPoints: no.n,
			ErrorMessage:       no.message(),
		}
	}

	return out, nil
}

// count is how many points a metric holds, whatever it is.
func count(m *metricspb.Metric) int {
	switch {
	case m.GetGauge() != nil:
		return len(m.GetGauge().GetDataPoints())
	case m.GetSum() != nil:
		return len(m.GetSum().GetDataPoints())
	case m.GetHistogram() != nil:
		return len(m.GetHistogram().GetDataPoints())
	case m.GetExponentialHistogram() != nil:
		return len(m.GetExponentialHistogram().GetDataPoints())
	case m.GetSummary() != nil:
		return len(m.GetSummary().GetDataPoints())
	}

	return 0
}

func nanos(v uint64) time.Time {
	if v > math.MaxInt64 {
		v = math.MaxInt64
	}
	return time.Unix(0, int64(v)).UTC()
}

// attribute is the string attribute `key` says.
func attribute(kvs []*commonpb.KeyValue, key string) (string, bool) {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				return "", false
			}
			return v.StringValue, true
		}
	}

	return "", false
}

// sourceOf is where a resource says it is: its service, and which process of
// it -- and nothing else of what it says, which may be a command line.
func sourceOf(res []*commonpb.KeyValue) string {
	parts := []string{}
	for _, k := range []string{"service.name", "service.instance.id"} {
		if v, ok := attribute(res, k); ok && v != "" {
			parts = append(parts, v)
		}
	}

	return strings.Join(parts, "/")
}

// digest is which stream a point is of: its resource, its scope, its metric's
// name and its own attributes, each attribute in the order of its key and
// every part said with its length, so that no two streams write the same
// bytes.
func digest(res []*commonpb.KeyValue, scope *commonpb.InstrumentationScope, name string, attrs []*commonpb.KeyValue) []byte {
	h := sha256.New()
	writeAttributes(h, res)
	writeString(h, scope.GetName())
	writeString(h, scope.GetVersion())
	writeAttributes(h, scope.GetAttributes())
	writeString(h, name)
	writeAttributes(h, attrs)

	return h.Sum(nil)
}

func writeString(h hash.Hash, s string) {
	_ = binary.Write(h, binary.BigEndian, uint64(len(s)))
	h.Write([]byte(s))
}

func writeAttributes(h hash.Hash, kvs []*commonpb.KeyValue) {
	kvs = slices.Clone(kvs)
	slices.SortStableFunc(kvs, func(a, b *commonpb.KeyValue) int { return strings.Compare(a.GetKey(), b.GetKey()) })
	// A key said twice is said once, the last time: what a reader of the
	// attributes as a map would make of it.
	uniq := kvs[:0]
	for i, kv := range kvs {
		if i+1 < len(kvs) && kvs[i+1].GetKey() == kv.GetKey() {
			continue
		}
		uniq = append(uniq, kv)
	}

	_ = binary.Write(h, binary.BigEndian, uint64(len(uniq)))
	for _, kv := range uniq {
		writeString(h, kv.GetKey())
		writeValue(h, kv.GetValue())
	}
}

func writeValue(h hash.Hash, v *commonpb.AnyValue) {
	switch u := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		h.Write([]byte{'s'})
		writeString(h, u.StringValue)
	case *commonpb.AnyValue_BoolValue:
		h.Write([]byte{'b'})
		if u.BoolValue {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	case *commonpb.AnyValue_IntValue:
		h.Write([]byte{'i'})
		_ = binary.Write(h, binary.BigEndian, u.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		h.Write([]byte{'d'})
		_ = binary.Write(h, binary.BigEndian, math.Float64bits(u.DoubleValue))
	case *commonpb.AnyValue_BytesValue:
		h.Write([]byte{'y'})
		writeString(h, string(u.BytesValue))
	case *commonpb.AnyValue_ArrayValue:
		h.Write([]byte{'a'})
		vs := u.ArrayValue.GetValues()
		_ = binary.Write(h, binary.BigEndian, uint64(len(vs)))
		for _, w := range vs {
			writeValue(h, w)
		}
	case *commonpb.AnyValue_KvlistValue:
		h.Write([]byte{'m'})
		writeAttributes(h, u.KvlistValue.GetValues())
	default:
		h.Write([]byte{'0'})
	}
}
