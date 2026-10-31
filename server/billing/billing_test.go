package billing_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/payday/meter"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/pd"
)

var (
	stock = levyan.MeterKind_METER_KIND_STOCK
	flow  = levyan.MeterKind_METER_KIND_FLOW
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	last  = levyan.Aggregation_AGGREGATION_LAST
	sum   = levyan.Aggregation_AGGREGATION_SUM
	pgkr  = levyan.Channel_CHANNEL_PG_KR
)

// on is a day of 2027 at midnight UTC: on(1, 32) is February's first.
func on(month, d int) time.Time { return time.Date(2027, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

type world struct {
	*levyantest.World
	t    *testing.T
	loft *levyan.Tenant
	key  string
	conn *grpc.ClientConn
	ctx  context.Context
}

// setup is loft, priced on the Korean gateway in won from 2027: active assets
// at 50 won each past 100, its history kept in bands, and the bytes it sends
// at 2 won a hundred past a thousand.
func setup(t *testing.T) world {
	w := levyantest.New(t)
	w.At(on(1, 1).Add(-24 * time.Hour))

	loft := w.Product("loft")
	loft = w.Terms(loft, "UTC", pgkr, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)

	assets := w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)
	w.Price(loft, assets, pgkr, "KRW", 100, 50, on(1, 1))

	history, err := w.S.Ungated.Meter().Add(t.Context(), levyan.MeterAddRequest_builder{
		Tenant: levyantest.Ref(loft), Key: "loft.history.keep", Name: "loft.history.keep", Unit: "d",
		Kind: stock, Aggregation: last, UnboundedZero: true,
	}.Build())
	w.X.NoError(err)
	_, err = w.S.Ungated.Price().Add(t.Context(), levyan.PriceAddRequest_builder{
		Tenant:   levyantest.Ref(loft),
		Meter:    levyan.MeterRef_builder{Id: history.GetId()}.Build(),
		Channel:  pgkr,
		Currency: "KRW",
		Bands: levyan.PriceBands_builder{Bands: []*levyan.PriceBand{
			levyan.PriceBand_builder{UpTo: proto.Int64(365), Fee: 0}.Build(),
			levyan.PriceBand_builder{UpTo: proto.Int64(1095), Fee: 10000}.Build(),
			levyan.PriceBand_builder{Fee: 30000}.Build(),
		}}.Build(),
		DateEffective: timestamppb.New(on(1, 1)),
	}.Build())
	w.X.NoError(err)

	bytes := w.Meter(loft, "loft.transfer.bytes", "By", flow, sum)
	_, err = w.S.Ungated.Price().Add(t.Context(), levyan.PriceAddRequest_builder{
		Tenant:        levyantest.Ref(loft),
		Meter:         levyan.MeterRef_builder{Id: bytes.GetId()}.Build(),
		Channel:       pgkr,
		Currency:      "KRW",
		Included:      1000,
		UnitPrice:     2,
		Per:           100,
		DateEffective: timestamppb.New(on(1, 1)),
	}.Build())
	w.X.NoError(err)

	return world{World: w, t: t, loft: loft, key: key, conn: w.Serve(), ctx: levyantest.As(context.Background(), key)}
}

// daily records a reading of `meter` for `ws` at each midnight of
// [from, to).
func (w world) daily(ws pdid.Id, meter, unit string, from, to time.Time, v int64) {
	w.t.Helper()
	rs := []*levyanv1.Reading{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{
			Workspace: ws.String(), Meter: meter, MeasuredAt: timestamppb.New(d), Value: v, Unit: unit,
		}.Build())
	}
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.ctx, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	w.X.NoError(err)
	for _, r := range res.GetResults() {
		w.X.Equal(levyanv1.Outcome_OUTCOME_KEPT, r.GetOutcome(), r.GetWhy())
	}
}

// counted exports the cumulative count of a flow for `ws`, from `start`.
func (w world) counted(ws pdid.Id, start time.Time, points map[time.Time]int64) {
	w.t.Helper()
	ps := []*metricspb.NumberDataPoint{}
	for at, v := range points {
		ps = append(ps, &metricspb.NumberDataPoint{
			Attributes: []*commonpb.KeyValue{{
				Key:   meter.TenantAttribute,
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ws.String()}},
			}},
			StartTimeUnixNano: uint64(start.UnixNano()),
			TimeUnixNano:      uint64(at.UnixNano()),
			Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
		})
	}
	res, err := collectormetrics.NewMetricsServiceClient(w.conn).Export(w.ctx, &collectormetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "loft.transfer.bytes", Unit: "By",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				IsMonotonic:            true,
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints:             ps,
			}},
		}}}}}},
	})
	w.X.NoError(err)
	w.X.Zero(res.GetPartialSuccess().GetRejectedDataPoints(), res.GetPartialSuccess().GetErrorMessage())
}

// close closes what has ended by `through`, as an operator's hand does.
func (w world) close(through time.Time) []*ent.Period {
	w.t.Helper()
	p, err := w.S.Ent.Tenant.Get(context.Background(), w.id(w.loft))
	w.X.NoError(err)
	ps, err := w.S.Closer.Close(context.Background(), p, through)
	w.X.NoError(err)
	return ps
}

func (w world) id(t *levyan.Tenant) [16]byte {
	id, err := pdid.From(t.GetId())
	w.X.NoError(err)
	return id.Uuid()
}

// statement is the statement of workspace `ws` and the period beginning at
// `start`, as a product reads it.
func (w world) statement(ws pdid.Id, start time.Time) *levyanv1.Statement {
	w.t.Helper()
	res, err := levyanv1.NewBillingServiceClient(w.conn).ListStatements(w.ctx, levyanv1.ListStatementsRequest_builder{Workspace: ws.String()}.Build())
	w.X.NoError(err)
	for _, s := range res.GetStatements() {
		if s.GetPeriod().GetStart().AsTime().Equal(start) {
			return s
		}
	}
	return nil
}

func amounts(s *levyanv1.Statement) map[string]string {
	out := map[string]string{}
	for _, l := range s.GetLines() {
		k := l.GetMeter()
		if l.GetKind() == levyanv1.LineKind_LINE_KIND_CORRECTION {
			k += " corrects " + l.GetCorrects().GetStart().AsTime().Format("2006-01")
		}
		out[k] = l.GetAmount()
	}
	return out
}

// A month closed is a statement of each workspace that held or did anything
// in it, its amounts as worked out by hand: a workspace held to its free
// allowance comes to nothing, and one that went past it comes to its usage
// at the price, its history at the bands for the time spent in each, and what
// it sent past what is included -- added exactly, and rounded once, down.
func TestAMonthClosedIsAStatementOfEachWorkspace(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(2, 10))
	a, b, c := pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain)

	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 200)
	w.counted(a, on(1, 1), map[time.Time]int64{on(1, 10): 600, on(1, 20): 1500, on(1, 31): 2234})
	w.daily(b, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 50)
	w.daily(c, "loft.history.keep", "d", on(1, 1), on(1, 11), 365)
	w.daily(c, "loft.history.keep", "d", on(1, 11), on(2, 1), 0)

	ps := w.close(on(2, 10))
	x.Len(ps, 1, "January")
	x.True(ps[0].DateStart.Equal(on(1, 1)))
	x.True(ps[0].DateEnd.Equal(on(2, 1)))
	x.NotNil(ps[0].DateClosed)

	// 200 assets all month: 100 past what is included, at 50. 2,234 bytes:
	// 1,234 past a thousand, at 2 a hundred, 24.68.
	sa := w.statement(a, on(1, 1))
	x.NotNil(sa)
	x.Equal(map[string]string{"loft.assets.active": "5000", "loft.transfer.bytes": "24.68"}, amounts(sa))
	x.Equal(int64(5024), sa.GetTotal())
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, sa.GetChannel())
	x.Equal("KRW", sa.GetCurrency())

	// 50 assets is within what is included.
	sb := w.statement(b, on(1, 1))
	x.NotNil(sb)
	x.Equal(map[string]string{"loft.assets.active": "0"}, amounts(sb))
	x.Equal(int64(0), sb.GetTotal())
	x.Equal("50", sb.GetLines()[0].GetUsage())
	x.Equal("100", sb.GetLines()[0].GetIncluded())

	// Ten days in the first band, for nothing, and twenty-one of forever:
	// 30,000 * 21/31 = 20,322.58...
	sc := w.statement(c, on(1, 1))
	x.NotNil(sc)
	x.Equal(map[string]string{"loft.history.keep": "20322.580645"}, amounts(sc))
	x.Equal(int64(20322), sc.GetTotal())
	x.Len(sc.GetLines()[0].GetParts(), 1)
	x.Len(sc.GetLines()[0].GetParts()[0].GetBands(), 2)

	// Closing again writes nothing more, and changes nothing written.
	before := statements(t, w)
	x.Len(w.close(on(2, 10)), 0, "nothing more has ended")
	x.NoError(w.S.Closer.Due(context.Background()))
	x.Equal(before, statements(t, w))
}

// statements is every statement there is, by identifier, with what it says.
func statements(t *testing.T, w world) map[[16]byte]string {
	t.Helper()
	vs, err := w.S.Ent.Statement.Query().All(context.Background())
	w.X.NoError(err)
	out := map[[16]byte]string{}
	for _, v := range vs {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(v.Lines)
		w.X.NoError(err)
		out[v.Id] = v.Exact + " " + string(b)
	}
	return out
}

// What comes late for a closed period is kept, and corrects it on the next
// statement: the closed one is not changed, the line names the period it
// corrects, and what the customer is told meanwhile is what the next
// statement will say. A correction made is made once.
func TestALateReadingIsCorrectedOnTheNextStatement(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(2, 10))
	a := pdid.New(pd.TenantDomain)

	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 200)
	w.close(on(2, 10))
	jan := w.statement(a, on(1, 1))
	x.Equal(int64(5000), jan.GetTotal())
	was := statements(t, w)

	// Half a day of 400 that should have been in January, sent in February.
	w.At(on(2, 12))
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.ctx, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: a.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(on(1, 15).Add(12 * time.Hour)), Value: 400, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	x.True(res.GetResults()[0].GetLate())

	// What February comes to so far says the correction it will carry:
	// 200 more for 12 of January's 744 hours, at 50 -- 5,000/31.
	w.At(on(2, 20))
	w.daily(a, "loft.assets.active", "{asset}", on(2, 10), on(2, 21), 200)
	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.True(u.GetUsage().GetPeriod().GetStart().AsTime().Equal(on(2, 1)))
	x.Equal(map[string]string{"loft.assets.active": "5000", "loft.assets.active corrects 2027-01": "161.290322"}, amountsOf(u.GetUsage().GetLines()))
	x.Equal(int64(5161), u.GetUsage().GetTotal())

	w.At(on(3, 5))
	w.daily(a, "loft.assets.active", "{asset}", on(2, 21), on(3, 1), 200)
	w.close(on(3, 5))
	feb := w.statement(a, on(2, 1))
	x.NotNil(feb)
	x.Equal(map[string]string{"loft.assets.active": "5000", "loft.assets.active corrects 2027-01": "161.290322"}, amounts(feb))
	x.Equal(int64(5161), feb.GetTotal(), "what the customer was told")

	var fix *levyanv1.Line
	for _, l := range feb.GetLines() {
		if l.GetKind() == levyanv1.LineKind_LINE_KIND_CORRECTION {
			fix = l
		}
	}
	x.NotNil(fix)
	x.True(fix.GetCorrects().GetStart().AsTime().Equal(on(1, 1)))
	x.True(fix.GetCorrects().GetEnd().AsTime().Equal(on(2, 1)))
	x.Equal("5000", fix.GetBilled())
	x.Equal("203.225806", fix.GetUsage(), "what January held, as it is known now")

	// January's statement is as it was.
	got := statements(t, w)
	for id, v := range was {
		x.Equal(v, got[id])
	}

	// And March corrects nothing again.
	w.At(on(4, 5))
	w.daily(a, "loft.assets.active", "{asset}", on(3, 1), on(4, 1), 200)
	w.close(on(4, 5))
	mar := w.statement(a, on(3, 1))
	x.Equal(map[string]string{"loft.assets.active": "5000"}, amounts(mar))
}

func amountsOf(ls []*levyanv1.Line) map[string]string {
	return amounts(levyanv1.Statement_builder{Lines: ls}.Build())
}

// A workspace that stops sending readings -- destroyed, its last read taken
// before -- is billed for what it last held for the hold, into the next
// period, and no longer.
func TestTheLastReadingHoldsIntoTheNextPeriodAndNoFurther(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(3, 10))
	c := pdid.New(pd.TenantDomain)

	w.daily(c, "loft.history.keep", "d", on(1, 1), on(2, 1), 0)
	ps := w.close(on(3, 10))
	x.Len(ps, 2, "January and February")

	x.Equal(int64(30000), w.statement(c, on(1, 1)).GetTotal())

	// The last, on January 31st, holds 36 hours: 12 of February's 672,
	// forever -- 30,000 * 12/672.
	feb := w.statement(c, on(2, 1))
	x.NotNil(feb)
	x.Equal(map[string]string{"loft.history.keep": "535.714285"}, amounts(feb))
	x.Equal(int64(535), feb.GetTotal())

	// And March, nothing: no statement at all.
	w.At(on(4, 5))
	w.close(on(4, 5))
	x.Nil(w.statement(c, on(3, 1)))
}

// The current period's usage is an estimate of what it will come to: the
// latest readings held to its end, and a flow as counted so far.
func TestUsageIsWhatThePeriodWouldComeToNow(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(1, 10).Add(12 * time.Hour))
	a := pdid.New(pd.TenantDomain)

	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(1, 11), 300)
	w.counted(a, on(1, 1), map[time.Time]int64{on(1, 5): 1500})

	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.True(u.GetUsage().GetAsOf().AsTime().Equal(on(1, 10).Add(12 * time.Hour)))
	x.True(u.GetUsage().GetPeriod().GetStart().AsTime().Equal(on(1, 1)))
	x.True(u.GetUsage().GetPeriod().GetEnd().AsTime().Equal(on(2, 1)))
	x.Equal("UTC", u.GetUsage().GetPeriod().GetZone())
	x.Equal(map[string]string{"loft.assets.active": "10000", "loft.transfer.bytes": "10"}, amountsOf(u.GetUsage().GetLines()))
	x.Equal("300", u.GetUsage().GetLines()[0].GetUsage(), "held at 300 to the end")
	x.Equal(int64(10010), u.GetUsage().GetTotal())

	// What it is told is what the close says, when the rest of the month
	// goes as it said it would.
	w.At(on(2, 5))
	w.daily(a, "loft.assets.active", "{asset}", on(1, 11), on(2, 1), 300)
	w.close(on(2, 5))
	x.Equal(int64(10010), w.statement(a, on(1, 1)).GetTotal())
}

// And what it is told is no promise: readings that stop make the period come
// to what was held while they came, and for the hold after.
func TestAnEstimateIsWhatWasKnownThen(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(1, 10).Add(12 * time.Hour))
	a := pdid.New(pd.TenantDomain)
	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(1, 11), 300)

	w.At(on(2, 5))
	w.close(on(2, 5))

	// Ten days and a half of 300: 300 * 10.5/31 = 101.61..., 1.61... past
	// what is included, at 50 = 2,500/31.
	x.Equal(map[string]string{"loft.assets.active": "80.645161"}, amounts(w.statement(a, on(1, 1))))
}

// A product reads its own statements and no other's, and nothing of a
// workspace it never sent anything of.
func TestAProductReadsItsOwnStatementsAlone(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)
	w.At(on(2, 10))
	a := pdid.New(pd.TenantDomain)
	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	jan := w.statement(a, on(1, 1))

	other := w.Product("other")
	key, _ := w.Key(other, "other-prod", nil)
	them := levyantest.As(context.Background(), key)
	bs := levyanv1.NewBillingServiceClient(w.conn)

	got, err := bs.GetStatement(w.ctx, levyanv1.GetStatementRequest_builder{Id: jan.GetId()}.Build())
	x.NoError(err)
	x.Equal(jan.GetTotal(), got.GetStatement().GetTotal())
	x.Equal(a.String(), got.GetStatement().GetWorkspace())

	_, err = bs.GetStatement(them, levyanv1.GetStatementRequest_builder{Id: jan.GetId()}.Build())
	x.ErrCode(codes.NotFound, err)
	_, err = bs.ListStatements(them, levyanv1.ListStatementsRequest_builder{Workspace: a.String()}.Build())
	x.ErrCode(codes.NotFound, err)
	_, err = bs.GetUsage(them, levyanv1.GetUsageRequest_builder{Workspace: a.String()}.Build())
	x.ErrCode(codes.NotFound, err)

	// A workspace nothing was heard of.
	_, err = bs.GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: pdid.New(pd.TenantDomain).String()}.Build())
	x.ErrCode(codes.NotFound, err)
}

// A period is closed once whoever closes it and however many at once: one
// statement of each workspace.
func TestAPeriodClosedAtOnceIsClosedOnce(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(2, 10))
	ws := []pdid.Id{}
	for range 5 {
		v := pdid.New(pd.TenantDomain)
		ws = append(ws, v)
		w.daily(v, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 150)
	}

	p, err := w.S.Ent.Tenant.Get(context.Background(), w.id(w.loft))
	x.NoError(err)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := w.S.Closer.Close(context.Background(), p, on(2, 10)); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
	wg.Wait()

	n, err := w.S.Ent.Statement.Query().Count(context.Background())
	x.NoError(err)
	x.Equal(len(ws), n)
	c, err := w.S.Ent.Period.Query().Where(entperiod.DateClosedNotNil()).Count(context.Background())
	x.NoError(err)
	x.Equal(1, c)
	for _, v := range ws {
		x.Equal(int64(2500), w.statement(v, on(1, 1)).GetTotal())
	}
}

// A product in Seoul is billed by Seoul's months, and a product whose zone
// changes goes on from where its last period ended.
func TestAPeriodIsAMonthInTheProductsZone(t *testing.T) {
	w := setup(t)
	x := w.X
	w.Terms(w.loft, "Asia/Seoul", pgkr, "KRW")
	w.At(on(2, 10))
	a := pdid.New(pd.TenantDomain)
	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 200)

	ps := w.close(on(2, 10))
	x.Len(ps, 1)
	seoul := time.FixedZone("KST", 9*60*60)
	x.True(ps[0].DateStart.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, seoul)), "%s", ps[0].DateStart)
	x.True(ps[0].DateEnd.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, seoul)))
	x.Equal("Asia/Seoul", ps[0].Zone)

	// From UTC on: February, from where Seoul's January ended to where
	// February ends in UTC, and nothing between them.
	w.Terms(w.loft, "UTC", pgkr, "KRW")
	w.At(on(3, 5))
	w.daily(a, "loft.assets.active", "{asset}", on(2, 10), on(3, 5), 200)
	ps = w.close(on(3, 5))
	x.Len(ps, 1)
	x.True(ps[0].DateStart.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, seoul)))
	x.True(ps[0].DateEnd.Equal(on(3, 1)))
	x.Equal("UTC", ps[0].Zone)
}

// A product that names no channel prices nothing, and closes nothing: what it
// is told is its usage, with no amounts.
func TestAProductNotPricedYetClosesNothing(t *testing.T) {
	w := setup(t)
	x := w.X
	w.Terms(w.loft, "", levyan.Channel_CHANNEL_UNSPECIFIED, "")
	w.At(on(2, 10))
	a := pdid.New(pd.TenantDomain)
	w.daily(a, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 200)

	x.NoError(w.S.Closer.Due(context.Background()))
	n, err := w.S.Ent.Period.Query().Count(context.Background())
	x.NoError(err)
	x.Zero(n)

	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.Channel_CHANNEL_UNSPECIFIED, u.GetUsage().GetChannel())
	x.Equal("200", u.GetUsage().GetLines()[0].GetUsage())
	x.Equal("0", u.GetUsage().GetLines()[0].GetAmount())
	x.Empty(u.GetUsage().GetLines()[0].GetParts())

	// And what a product's terms may be is held where they are written.
	_, err = w.S.Ungated.Tenant().Patch(context.Background(), levyan.TenantPatchRequest_builder{
		Ref: levyantest.Ref(w.loft), Zone: proto.String("Mars/Olympus"), DateUpdatedForce: proto.Bool(true),
	}.Build())
	pdtest.NewX(t).ErrCode(codes.InvalidArgument, err)
	_, err = w.S.Ungated.Tenant().Patch(context.Background(), levyan.TenantPatchRequest_builder{
		Ref: levyantest.Ref(w.loft), Channel: proto.String("CHANNEL_CARRIER_PIGEON"), DateUpdatedForce: proto.Bool(true),
	}.Build())
	pdtest.NewX(t).ErrCode(codes.InvalidArgument, err)
	_, err = w.S.Ungated.Tenant().Patch(context.Background(), levyan.TenantPatchRequest_builder{
		Ref: levyantest.Ref(w.loft), Currency: proto.String("KWR"), DateUpdatedForce: proto.Bool(true),
	}.Build())
	pdtest.NewX(t).ErrCode(codes.InvalidArgument, err)
}

// A flow counted late -- a process whose export did not come until after its
// period closed -- is corrected the same way.
func TestALateFlowIsCorrectedOnTheNextStatement(t *testing.T) {
	w := setup(t)
	x := w.X
	w.At(on(2, 10))
	a := pdid.New(pd.TenantDomain)
	w.counted(a, on(1, 1), map[time.Time]int64{on(1, 10): 1500})
	w.close(on(2, 10))
	x.Equal(map[string]string{"loft.transfer.bytes": "10"}, amounts(w.statement(a, on(1, 1))))

	// Another process's January, exported in February: a series of its own.
	res, err := collectormetrics.NewMetricsServiceClient(w.conn).Export(w.ctx, &collectormetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key: "service.instance.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "late"}},
			}}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: "loft.transfer.bytes", Unit: "By",
				Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
					IsMonotonic:            true,
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					DataPoints: []*metricspb.NumberDataPoint{{
						Attributes: []*commonpb.KeyValue{{
							Key:   meter.TenantAttribute,
							Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: a.String()}},
						}},
						StartTimeUnixNano: uint64(on(1, 1).UnixNano()),
						TimeUnixNano:      uint64(on(1, 20).UnixNano()),
						Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 500},
					}},
				}},
			}}}},
		}},
	})
	x.NoError(err)
	x.Zero(res.GetPartialSuccess().GetRejectedDataPoints())

	// 500 bytes more of January: 1,000 past a thousand at 2 a hundred, 20,
	// less the 10 billed. February's own 100 are within what is included.
	w.At(on(3, 5))
	w.counted(a, on(1, 1), map[time.Time]int64{on(2, 10): 1600})
	w.close(on(3, 5))
	x.Equal(map[string]string{"loft.transfer.bytes": "0", "loft.transfer.bytes corrects 2027-01": "10"}, amounts(w.statement(a, on(2, 1))))
}

// A price raised in the middle of a period, for everybody from that day:
// each half at its own.
func TestAPriceRaisedMidPeriodChargesEachPartForItsTime(t *testing.T) {
	w := setup(t)
	x := w.X
	assets, err := w.S.Ent.Meter.Query().All(context.Background())
	x.NoError(err)
	var id []byte
	for _, m := range assets {
		if m.Key == "loft.assets.active" {
			id = m.Id[:]
		}
	}
	_, err = w.S.Ungated.Price().Add(context.Background(), levyan.PriceAddRequest_builder{
		Tenant:        levyantest.Ref(w.loft),
		Meter:         levyan.MeterRef_builder{Id: id}.Build(),
		Channel:       pgkr,
		Currency:      "KRW",
		Included:      100,
		UnitPrice:     80,
		DateEffective: timestamppb.New(on(2, 15)),
		Desc:          "인상",
	}.Build())
	x.NoError(err)

	w.At(on(3, 5))
	a := pdid.New(pd.TenantDomain)
	w.daily(a, "loft.assets.active", "{asset}", on(1, 25), on(3, 1), 200)
	w.close(on(3, 5))

	// 14 of February's 28 days at 50, and 14 at 80, of 100 past what is
	// included: 2,500 + 4,000.
	feb := w.statement(a, on(2, 1))
	x.Equal(map[string]string{"loft.assets.active": "6500"}, amounts(feb))
	x.Len(feb.GetLines()[0].GetParts(), 2)
	x.Equal("0.5", feb.GetLines()[0].GetParts()[0].GetShare())
}
