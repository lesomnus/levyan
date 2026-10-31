package ledger

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
)

// GapsLoop is the name of the loop that looks for missing readings.
const GapsLoop = "levyan.readings.gaps"

// GapsGauge is how many of a product's workspaces have gone without a reading
// of one of its stock meters for longer than they should have, by product and
// meter: what the monitoring alerts on.
const GapsGauge = "levyan.readings.gaps"

// Gap is a workspace that has been sending readings of a stock meter and has
// sent none for longer than it should have.
type Gap struct {
	// Product is the product's alias, and Workspace the workspace as the
	// product knows it.
	Product   string
	Workspace pdid.Id
	Meter     string

	// Last is when the latest reading of it was measured.
	Last time.Time
}

// Expect is what is expected of readings: a workspace whose latest reading
// of a meter was measured within Recent and not within Gap is a gap.
type Expect struct {
	Gap    time.Duration
	Recent time.Duration
}

// Gaps is every workspace of every product that has sent a reading of a stock
// meter within `e.Recent` of `now` and none within `e.Gap`, by product, meter
// and how long it has been -- and, for every product's stock meters, how many
// of them there are, none included, for the gauge.
func Gaps(ctx context.Context, db *ent.Client, now time.Time, e Expect) ([]Gap, map[[2]string]int, error) {
	ms, err := db.Meter.Query().
		Where(entmeter.Kind(int32(levyan.MeterKind_METER_KIND_STOCK))).
		WithTenant().
		All(ctx)
	if err != nil {
		return nil, nil, err
	}

	out := []Gap{}
	counts := map[[2]string]int{}
	for _, m := range ms {
		product := m.Edges.Tenant.Alias
		counts[[2]string{product, m.Key}] = 0

		var vs []struct {
			WorkspaceId key     `json:"workspace_id"`
			Last        instant `json:"last"`
		}
		err := db.Reading.Query().
			Where(
				entreading.TenantId(m.TenantId),
				entreading.MeterId(m.Id),
				entreading.MeasuredAtGTE(now.Add(-e.Recent)),
			).
			GroupBy(entreading.FieldWorkspaceId).
			Aggregate(ent.As(ent.Max(entreading.FieldMeasuredAt), "last")).
			Scan(ctx, &vs)
		if err != nil {
			return nil, nil, err
		}

		late := map[uuid.UUID]time.Time{}
		ids := []uuid.UUID{}
		for _, v := range vs {
			last := Moment(time.Time(v.Last))
			if now.Sub(last) <= e.Gap {
				continue
			}
			late[uuid.UUID(v.WorkspaceId)] = last
			ids = append(ids, uuid.UUID(v.WorkspaceId))
		}
		if len(ids) == 0 {
			continue
		}

		wss, err := db.Workspace.Query().Where(entworkspace.IdIn(ids...)).All(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, w := range wss {
			k, err := pdid.From(w.Key)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, Gap{Product: product, Workspace: k, Meter: m.Key, Last: late[w.Id]})
		}
		counts[[2]string{product, m.Key}] = len(wss)
	}

	slices.SortFunc(out, func(a, b Gap) int {
		return cmp.Or(
			cmp.Compare(a.Product, b.Product),
			cmp.Compare(a.Meter, b.Meter),
			a.Last.Compare(b.Last),
			slices.Compare(a.Workspace[:], b.Workspace[:]),
		)
	})

	return out, counts, nil
}

// LookForGaps is the loop that looks for missing readings every `every`, and
// says what it finds: a line of the log for each product and meter that has
// any, and the gauge [GapsGauge] for every one, none included, so that it goes
// back down. A pass that fails is said in the log and the next one tries
// again: a loop that warns of what is missing is not a reason to stop
// serving.
//
// It is the deployment's work, so under a lease one process runs it; its name
// is what the processes agree on.
func LookForGaps(db *ent.Client, now func() time.Time, every time.Duration, e Expect) spin.Spinner {
	if now == nil {
		now = time.Now
	}

	return spin.Called(GapsLoop, spin.Every(every, func(ctx context.Context) error {
		if err := ReportGaps(ctx, db, Moment(now()), e); err != nil {
			log.From(ctx).WarnContext(ctx, "ledger: looking for missing readings", slog.Any("err", err))
		}
		return nil
	}))
}

// ReportGaps is one pass of [LookForGaps].
func ReportGaps(ctx context.Context, db *ent.Client, now time.Time, e Expect) error {
	gaps, counts, err := Gaps(ctx, db, now, e)
	if err != nil {
		return err
	}

	g := otx.Int64Gauge(ctx, GapsGauge,
		metric.WithDescription("workspaces that have sent readings of a stock meter lately, and none for longer than they should have"),
		metric.WithUnit("{workspace}"),
	)
	for k, n := range counts {
		g.Record(ctx, int64(n), metric.WithAttributes(
			attribute.String("levyan.product", k[0]),
			attribute.String("levyan.meter", k[1]),
		))
	}

	// A line for each product and meter, and not one for each workspace: a
	// product whose reader stopped is every workspace it has at once.
	for i := 0; i < len(gaps); {
		j := i
		for j < len(gaps) && gaps[j].Product == gaps[i].Product && gaps[j].Meter == gaps[i].Meter {
			j++
		}
		some := []string{}
		for _, v := range gaps[i:min(j, i+5)] {
			some = append(some, v.Workspace.String())
		}
		log.From(ctx).WarnContext(ctx, "ledger: workspaces without a reading for longer than they should have",
			slog.String("product", gaps[i].Product),
			slog.String("meter", gaps[i].Meter),
			slog.Int("workspaces", j-i),
			slog.Time("oldest", gaps[i].Last),
			slog.Duration("gap", e.Gap),
			slog.Any("some", some),
		)
		i = j
	}

	return nil
}
