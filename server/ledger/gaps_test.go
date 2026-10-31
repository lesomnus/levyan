package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/otx/otxtest"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// A workspace that has been sending a stock's readings and has gone quiet for
// longer than it should have is a gap, warned of in the log and in the gauge;
// one that is still sending is not, nor one gone quiet so long ago that it is
// no longer looked for, nor a flow, which has no schedule.
func TestTheLoopFindsWorkspacesThatStoppedSendingReadings(t *testing.T) {
	w, p := setup(t)
	x := w.X
	w.Meter(p.t, "loft.storage.bytes", "By", stock, twa)
	now := ledger.Moment(time.Now())

	quiet, sending, gone, other := pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain), pdid.New(pd.TenantDomain)
	rs := p.record(
		reading(quiet, "loft.assets.active", "{asset}", now.Add(-72*time.Hour), 1),
		reading(quiet, "loft.assets.active", "{asset}", now.Add(-48*time.Hour), 1),
		reading(sending, "loft.assets.active", "{asset}", now.Add(-48*time.Hour), 1),
		reading(sending, "loft.assets.active", "{asset}", now.Add(-time.Hour), 1),
		reading(gone, "loft.assets.active", "{asset}", now.Add(-10*24*time.Hour), 1),
		// Quiet of one meter, and sending another.
		reading(other, "loft.assets.active", "{asset}", now.Add(-40*time.Hour), 1),
		reading(other, "loft.storage.bytes", "By", now.Add(-time.Hour), 1),
	)
	for _, r := range rs {
		x.Equal(kept, r.GetOutcome())
	}

	e := ledger.Expect{Gap: 36 * time.Hour, Recent: 7 * 24 * time.Hour}
	gaps, counts, err := ledger.Gaps(context.Background(), w.S.Ent, now, e)
	x.NoError(err)
	x.Len(gaps, 2)
	x.Equal(ledger.Gap{Product: "loft", Workspace: quiet, Meter: "loft.assets.active", Last: now.Add(-48 * time.Hour)}, gaps[0])
	x.Equal(ledger.Gap{Product: "loft", Workspace: other, Meter: "loft.assets.active", Last: now.Add(-40 * time.Hour)}, gaps[1])
	x.Equal(map[[2]string]int{{"loft", "loft.assets.active"}: 2, {"loft", "loft.storage.bytes"}: 0}, counts)

	// Said in the gauge, by product and meter, none included, and in one line
	// of the log for the meter that has any.
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	x.NoError(ledger.ReportGaps(ctx, w.S.Ent, now, e))

	got := map[string]int64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != ledger.GapsGauge {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("levyan.meter"))
				got[v.AsString()] = dp.Value
			}
		}
	}
	x.Equal(map[string]int64{"loft.assets.active": 2, "loft.storage.bytes": 0}, got)

	warned := 0
	for _, r := range h.Records() {
		if r.Body().AsString() == "ledger: workspaces without a reading for longer than they should have" {
			warned++
		}
	}
	x.Equal(1, warned)

	// And it is a loop of its own name, which a lease holds it by.
	s := ledger.LookForGaps(w.S.Ent, nil, time.Hour, e)
	n, ok := s.(spin.Named)
	x.True(ok)
	x.Equal("levyan.readings.gaps", n.SpinName())
}
