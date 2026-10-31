package reconcile_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/otx/otxtest"
	"github.com/lesomnus/payday/spin"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/reconcile"
)

// What a pass finds is said in the gauge a dashboard alerts on -- of every
// kind, none included, so that it goes back down -- and in the log, a line
// at its severity for each finding open; and when the pass ended, so that a
// reconciliation that stopped is seen to have.
func TestAPassIsSaidInTheGaugeAndTheLog(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	w.srv.Duplicate(a.ChannelRef)

	h := otxtest.New(t)
	ctx := h.Into(w.ctx)
	_, err := w.S.Reconciler.Run(ctx)
	x.NoError(err)

	got := map[string]int64{}
	severity := map[string]string{}
	ran := false
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case reconcile.FindingsGauge:
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					k, _ := dp.Attributes.Value(attribute.Key("levyan.finding.kind"))
					s, _ := dp.Attributes.Value(attribute.Key("levyan.finding.severity"))
					got[k.AsString()] = dp.Value
					severity[k.AsString()] = s.AsString()
				}
			case reconcile.RanGauge:
				ran = true
				x.Equal(w.srv.Now().Unix(), m.Data.(metricdata.Gauge[int64]).DataPoints[0].Value)
			}
		}
	}
	x.True(ran)
	x.Len(got, len(levyan.FindingKind_name)-1, "every kind, none included")
	x.Equal(int64(1), got["twice"])
	x.Equal("critical", severity["twice"])
	x.Equal(int64(0), got["charge_unmatched"])
	x.Equal("warning", severity["attempt_stale"])

	said := 0
	for _, r := range h.Records() {
		if r.Severity() == otellog.SeverityError && strings.HasPrefix(r.Body().AsString(), "reconcile: key ") {
			said++
		}
	}
	x.Equal(1, said, "a line for the finding, at its severity")
}

// Each finding is written once: a pass that finds what was found again
// writes nothing of it, one that finds it changed writes what changed into
// the same row, and one that no longer finds it resolves it -- which a pass
// that finds it again opens.
func TestAFindingIsWrittenOnce(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	w.srv.Duplicate(a.ChannelRef)
	rep := w.reconcile()
	x.Equal(1, rep.New)
	f := w.of(levyan.FindingKind_FINDING_KIND_TWICE)[0]
	at := f.DateUpdated

	rep = w.reconcile()
	x.Zero(rep.New + rep.Changed + rep.Resolved)
	x.Equal(at, w.finding(f).DateUpdated, "not written again")

	w.srv.Duplicate(a.ChannelRef)
	rep = w.reconcile()
	x.Equal(1, rep.Changed)
	v := w.finding(f)
	x.Equal(int64(10000), v.Amount)
	x.Equal(f.DateCreated, v.DateCreated, "the same row")

	// Given back at the merchant, both, by hand.
	ts := w.of(levyan.FindingKind_FINDING_KIND_TWICE)[0].Rows.GetRows()
	for _, r := range ts {
		if r.GetEntity() != "paddle.transaction" {
			continue
		}
		w.giveBack(r.GetRef())
	}
	rep = w.reconcile()
	x.Equal(1, rep.Resolved)
	x.Equal(resolved, w.finding(f).State)

	w.srv.Duplicate(a.ChannelRef)
	rep = w.reconcile()
	x.Equal(1, rep.Changed, "opened again")
	v = w.finding(f)
	x.Equal(open, v.State)
	x.Nil(v.DateResolved)
	x.Empty(v.Resolution)
}

// giveBack is all of transaction `id` given back at the merchant, by hand,
// and approved.
func (w *world) giveBack(id string) {
	w.t.Helper()
	ch := w.S.Subscriptions.Channel
	t, err := ch.Transaction(w.ctx, id)
	w.X.NoError(err)
	if len(t.Adjustments) > 0 {
		return
	}
	adj, err := ch.Refund(w.ctx, refundAll(id))
	w.X.NoError(err)
	w.srv.Approve(adj.ID)
}

func refundAll(id string) mor.Refund {
	return mor.Refund{Transaction: id, All: true, Reason: "by hand", Key: "byhand-" + id[len(id)-8:]}
}

// It is a loop of its own name, which a lease holds it by.
func TestTheLoopHasANameOfItsOwn(t *testing.T) {
	s := reconcile.Reconciles(&reconcile.Reconciler{}, time.Hour)
	n, ok := s.(spin.Named)
	if !ok || n.SpinName() != "levyan.reconcile" {
		t.Fatalf("the loop is %v", s)
	}
}
