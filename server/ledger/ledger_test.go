package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/ent/audit"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

var (
	stock = levyan.MeterKind_METER_KIND_STOCK
	flow  = levyan.MeterKind_METER_KIND_FLOW
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE
	sum   = levyan.Aggregation_AGGREGATION_SUM

	kept     = levyanv1.Outcome_OUTCOME_KEPT
	already  = levyanv1.Outcome_OUTCOME_ALREADY
	refused  = levyanv1.Outcome_OUTCOME_REFUSED
	conflict = levyanv1.Refusal_REFUSAL_CONFLICT
)

// at is a moment, a while ago: readings are of what was held, and none is of
// the future.
func at(days, hours int) time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days).Add(time.Duration(hours) * time.Hour)
}

func reading(ws pdid.Id, meter, unit string, t time.Time, v int64) *levyanv1.Reading {
	return levyanv1.Reading_builder{
		Workspace:  ws.String(),
		Meter:      meter,
		MeasuredAt: timestamppb.New(t),
		Value:      v,
		Unit:       unit,
	}.Build()
}

type product struct {
	w   *levyantest.World
	t   *levyan.Tenant
	key string
	m   levyanv1.MeteringServiceClient
	ctx context.Context
}

func setup(t *testing.T) (*levyantest.World, product) {
	w := levyantest.New(t)
	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)
	w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)
	w.Meter(loft, "loft.transfer.bytes", "By", flow, sum)

	m := levyanv1.NewMeteringServiceClient(w.Serve())
	return w, product{w: w, t: loft, key: key, m: m, ctx: levyantest.As(context.Background(), key)}
}

func (p product) record(rs ...*levyanv1.Reading) []*levyanv1.Recorded {
	res, err := p.m.Record(p.ctx, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	p.w.X.NoError(err)
	p.w.X.Len(res.GetResults(), len(rs))
	return res.GetResults()
}

func outcomes(rs []*levyanv1.Recorded) []levyanv1.Outcome {
	out := []levyanv1.Outcome{}
	for _, r := range rs {
		out = append(out, r.GetOutcome())
	}
	return out
}

func rows(t *testing.T, w *levyantest.World, product *levyan.Tenant) []*ent.Reading {
	t.Helper()
	pid, err := pdid.From(product.GetId())
	w.X.NoError(err)
	vs, err := w.S.Ent.Reading.Query().Where(entreading.TenantId(pid.Uuid())).Order(ent.Asc(entreading.FieldMeasuredAt)).All(context.Background())
	w.X.NoError(err)
	return vs
}

// A reading is its workspace, its meter and its moment: sent again it is the
// one already kept, and sent with another value it is refused and the kept one
// stays -- in one call or in two.
func TestAReadingIsKeptOnce(t *testing.T) {
	w, p := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)

	rs := p.record(
		reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10),
		reading(ws, "loft.assets.active", "{asset}", at(1, 0), 12),
	)
	x.Equal([]levyanv1.Outcome{kept, kept}, outcomes(rs))

	// Again, as a reader whose call timed out after it was kept sends it.
	rs = p.record(
		reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10),
		reading(ws, "loft.assets.active", "{asset}", at(1, 0), 12),
		reading(ws, "loft.assets.active", "{asset}", at(2, 0), 15),
	)
	x.Equal([]levyanv1.Outcome{already, already, kept}, outcomes(rs))

	// The same moment with another value: refused, and said why.
	rs = p.record(reading(ws, "loft.assets.active", "{asset}", at(1, 0), 13))
	x.Equal(refused, rs[0].GetOutcome())
	x.Equal(conflict, rs[0].GetRefusal())
	x.Contains(rs[0].GetWhy(), "12")

	// And the same within one call.
	rs = p.record(
		reading(ws, "loft.assets.active", "{asset}", at(3, 0), 20),
		reading(ws, "loft.assets.active", "{asset}", at(3, 0), 20),
		reading(ws, "loft.assets.active", "{asset}", at(3, 0), 21),
	)
	x.Equal([]levyanv1.Outcome{kept, already, refused}, outcomes(rs))

	vs := rows(t, w, p.t)
	x.Len(vs, 4)
	got := []int64{}
	for _, v := range vs {
		got = append(got, v.Value)
	}
	x.Equal([]int64{10, 12, 15, 20}, got, "what was kept first stays")

	// One workspace, written once, and every reading through the servers: the
	// trail names the key that sent it.
	n, err := w.S.Ent.Workspace.Query().Count(context.Background())
	x.NoError(err)
	x.Equal(1, n)

	key, err := w.S.Ent.ServiceKey.Query().Only(context.Background())
	x.NoError(err)
	for _, v := range vs {
		a, err := w.S.Ent.Audit.Query().Where(audit.ObjectId(v.Id)).Only(context.Background())
		x.NoError(err)
		x.Equal(key.Id, a.ActorId)
	}
}

// What is not a reading of one of the product's stocks is refused, each alone,
// and the rest of the call is kept.
func TestWhatIsNotAReadingOfTheProductIsRefused(t *testing.T) {
	w, p := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)

	ahead := time.Now().Add(time.Hour)
	noMoment := reading(ws, "loft.assets.active", "{asset}", at(0, 0), 1)
	noMoment.ClearMeasuredAt()

	rs := p.record(
		reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10),
		reading(ws, "loft.assets.all", "{asset}", at(0, 0), 10),
		reading(ws, "loft.transfer.bytes", "By", at(0, 0), 10),
		reading(ws, "loft.assets.active", "{assets}", at(0, 0), 10),
		reading(ws, "loft.assets.active", "1", at(0, 0), 10),
		reading(pdid.New(pd.HolderDomain), "loft.assets.active", "{asset}", at(0, 0), 10),
		levyanv1.Reading_builder{Workspace: "loft", Meter: "loft.assets.active", MeasuredAt: timestamppb.New(at(0, 0)), Value: 1, Unit: "{asset}"}.Build(),
		reading(ws, "loft.assets.active", "{asset}", ahead, 10),
		noMoment,
		reading(ws, "loft.assets.active", "{asset}", at(1, 0), -1),
	)
	refusal := []levyanv1.Refusal{}
	for _, r := range rs {
		refusal = append(refusal, r.GetRefusal())
	}
	x.Equal(kept, rs[0].GetOutcome())
	x.Equal([]levyanv1.Refusal{
		levyanv1.Refusal_REFUSAL_UNSPECIFIED,
		levyanv1.Refusal_REFUSAL_NO_METER,
		levyanv1.Refusal_REFUSAL_FLOW,
		levyanv1.Refusal_REFUSAL_UNIT,
		levyanv1.Refusal_REFUSAL_UNIT,
		levyanv1.Refusal_REFUSAL_INVALID,
		levyanv1.Refusal_REFUSAL_INVALID,
		levyanv1.Refusal_REFUSAL_INVALID,
		levyanv1.Refusal_REFUSAL_INVALID,
		levyanv1.Refusal_REFUSAL_INVALID,
	}, refusal)
	for _, r := range rs[1:] {
		x.Equal(refused, r.GetOutcome())
		x.NotEmpty(r.GetWhy())
	}
	x.Len(rows(t, w, p.t), 1)

	// A call of more than a call may hold is no call.
	many := make([]*levyanv1.Reading, ledger.Most+1)
	for i := range many {
		many[i] = reading(ws, "loft.assets.active", "{asset}", at(0, i), 1)
	}
	_, err := p.m.Record(p.ctx, levyanv1.RecordRequest_builder{Readings: many}.Build())
	pdtest.NewX(t).ErrCode(codes.InvalidArgument, err)
}

// A product records into its own ledger and no other's: another product's
// meter is no meter of its, and the same workspace sent by two products is two
// workspaces, one of each.
func TestAProductRecordsIntoItsOwnLedgerAlone(t *testing.T) {
	w, p := setup(t)
	x := w.X
	other := w.Product("other")
	otherKey, _ := w.Key(other, "other-prod", nil)
	w.Meter(other, "other.things", "{thing}", stock, twa)
	o := product{w: w, t: other, key: otherKey, m: p.m, ctx: levyantest.As(context.Background(), otherKey)}

	ws := pdid.New(pd.TenantDomain)
	rs := o.record(
		reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10),
		reading(ws, "other.things", "{thing}", at(0, 0), 3),
	)
	x.Equal(levyanv1.Refusal_REFUSAL_NO_METER, rs[0].GetRefusal(), "loft's meter is nothing to another product")
	x.Equal(kept, rs[1].GetOutcome())

	rs = p.record(reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10))
	x.Equal(kept, rs[0].GetOutcome(), "and the same moment of loft's is loft's own")

	x.Len(rows(t, w, p.t), 1)
	x.Len(rows(t, w, other), 1)

	vs, err := w.S.Ent.Workspace.Query().Where(entworkspace.Key(ws.Bytes())).All(context.Background())
	x.NoError(err)
	x.Len(vs, 2, "a workspace is a product's")
	x.NotEqual(vs[0].TenantId, vs[1].TenantId)
}

// A reading that speaks for a period whose close has begun is kept, and marked
// late: measured in it, or measured before it near enough that it holds into
// it. One measured in a period still open is not.
func TestAReadingForAClosedPeriodIsLate(t *testing.T) {
	w, p := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)

	// September, closed -- or begun on, which is the same to a reading.
	_, err := w.S.Ungated.Period().Add(context.Background(), levyan.PeriodAddRequest_builder{
		Tenant:    levyantest.Ref(p.t),
		DateStart: timestamppb.New(at(0, 0)),
		DateEnd:   timestamppb.New(at(30, 0)),
		Zone:      "UTC",
	}.Build())
	x.NoError(err)

	rs := p.record(
		reading(ws, "loft.assets.active", "{asset}", at(-3, 0), 1),  // August, and too long before
		reading(ws, "loft.assets.active", "{asset}", at(-1, 2), 2),  // August, holding into September
		reading(ws, "loft.assets.active", "{asset}", at(10, 0), 3),  // September
		reading(ws, "loft.assets.active", "{asset}", at(29, 23), 4), // its last hour
		reading(ws, "loft.assets.active", "{asset}", at(30, 0), 5),  // October
	)
	late := []bool{}
	for _, r := range rs {
		x.Equal(kept, r.GetOutcome())
		late = append(late, r.GetLate())
	}
	x.Equal([]bool{false, true, true, true, false}, late)

	vs := rows(t, w, p.t)
	got := []bool{}
	for _, v := range vs {
		got = append(got, v.Late)
	}
	x.Equal(late, got, "as kept")

	// And a late one sent again is still late.
	rs = p.record(reading(ws, "loft.assets.active", "{asset}", at(10, 0), 3))
	x.Equal(already, rs[0].GetOutcome())
	x.True(rs[0].GetLate())
}

// Two calls at once about a workspace nobody has heard of are one workspace,
// and each reading is kept once, whichever comes first.
func TestReadingsSentAtOnceAreKeptOnce(t *testing.T) {
	w, p := setup(t)
	x := w.X
	ws := pdid.New(pd.TenantDomain)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []levyanv1.Outcome
	)
	for range 4 {
		wg.Go(func() {
			res, err := p.m.Record(p.ctx, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
				reading(ws, "loft.assets.active", "{asset}", at(0, 0), 10),
				reading(ws, "loft.assets.active", "{asset}", at(1, 0), 11),
			}}.Build())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("record: %v", err)
				return
			}
			got = append(got, outcomes(res.GetResults())...)
		})
	}
	wg.Wait()

	n := 0
	for _, o := range got {
		x.Contains([]levyanv1.Outcome{kept, already}, o)
		if o == kept {
			n++
		}
	}
	x.Equal(2, n, "each reading kept by one call")
	x.Len(rows(t, w, p.t), 2)

	c, err := w.S.Ent.Workspace.Query().Count(context.Background())
	x.NoError(err)
	x.Equal(1, c)
}
