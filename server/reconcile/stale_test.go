package reconcile_test

import (
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/usage"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
	"github.com/lesomnus/levyan/server/reconcile"
)

// A period not closed long after its end and the grace it waits for what
// comes late is found -- nothing of it is billed until it is -- and closed,
// it is not.
func TestAPeriodNotClosedIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	w.to(on(2, 4).Add(time.Hour))
	w.daily(ws, on(1, 1), on(2, 5), 200)

	w.to(on(2, 3).Add(23 * time.Hour))
	w.reconcile()
	w.nothing()

	w.to(on(2, 4).Add(time.Hour))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_PERIOD_UNCLOSED)
	x.Len(fs, 1)
	x.Equal("Period/"+w.product.String()+"/2027-01-01T00:00:00Z", fs[0].Subject)
	x.Contains(fs[0].Summary, "no close of it began")
	x.Equal(int32(levyan.FindingCorrection_FINDING_CORRECTION_CLOSE), fs[0].Correction)

	w.close(on(2, 4))
	w.reconcile()
	w.nothing()
}

// A draft that should have been decided -- its last period closed a day ago
// and more -- and was not is found: here, since nothing collects on its
// channel, which a deployment that names no merchant of record collects on
// nothing. Once something does, it is decided, and is not.
func TestADraftNothingDecidesIsFound(t *testing.T) {
	w := bare(t)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.month(on(1, 1), ws)
	w.run()
	inv := w.only(ba)
	x.Equal(int32(levyan.InvoiceState_INVOICE_STATE_DRAFT), inv.State)
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_DRAFT_STALE), "its period closed just now")

	w.to(w.srv.Now().Add(reconcile.DefaultDraft + time.Hour))
	w.daily(ws, on(2, 1), on(2, 12), 200)
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_DRAFT_STALE)
	x.Len(fs, 1)
	x.Equal(reconcile.Subject("Invoice", inv.Id), fs[0].Subject)
	x.Contains(fs[0].Summary, "nothing collects on its channel, CHANNEL_MOR")
	x.Equal(int64(5000), fs[0].Amount)

	w.S.Invoices.Collectors[morCh] = &invoicestest.Channel{Push: true}
	w.run()
	x.Equal(paid, w.again(inv).State)
	w.reconcile()
	w.nothing()
}

// read records a reading of `meter` for `ws` at each midnight of [from, to).
func (w *world) read(ws pdid.Id, meter, unit string, from, to time.Time) {
	w.t.Helper()
	rs := []*levyanv1.Reading{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: ws.String(), Meter: meter, MeasuredAt: timestamppb.New(d), Value: 1, Unit: unit}.Build())
	}
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.as, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	w.X.NoError(err)
	for _, r := range res.GetResults() {
		w.X.Equal(levyanv1.Outcome_OUTCOME_KEPT, r.GetOutcome(), r.GetWhy())
	}
}

// Of each stock meter a product's workspaces send, the workspaces that have
// gone without a reading of it for longer than a reading holds are billed
// short, and found -- one finding of the meter, however many there are: one
// that stopped, and one that never sent it while it sent another. One
// closed is billed nothing while it is, and is not looked for. Read again,
// they are not.
func TestWorkspacesWithoutReadingsAreFound(t *testing.T) {
	w := setup(t)
	x := w.X
	loft, err := w.S.Ungated.Tenant().Get(w.ctx, levyan.TenantGetRequest_builder{Ref: levyan.TenantRef_builder{Id: w.product.Bytes()}.Build()}.Build())
	x.NoError(err)
	w.Meter(loft, "loft.storage.bytes", "By", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_MAX)

	sending, never, stopped, closed := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()
	w.to(on(1, 25))
	for _, ws := range []pdid.Id{sending, stopped, closed} {
		to := on(1, 26)
		if ws != sending {
			to = on(1, 21)
		}
		w.read(ws, "loft.assets.active", "{asset}", on(1, 1), to)
		w.read(ws, "loft.storage.bytes", "By", on(1, 1), to)
	}
	w.read(never, "loft.assets.active", "{asset}", on(1, 1), on(1, 26))
	_, err = w.S.Payers.Close(w.ctx, w.product, closed, on(1, 20))
	x.NoError(err)

	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_READINGS_MISSING)
	x.Len(fs, 2)
	by := map[string]string{}
	for _, f := range fs {
		by[f.Subject] = f.Summary
		x.Equal(int32(levyan.FindingSeverity_FINDING_SEVERITY_WARNING), f.Severity)
	}
	got := []string{}
	for _, s := range by {
		got = append(got, s)
	}
	x.Contains(got, "1 workspaces of loft have sent no reading of loft.assets.active for longer than a reading holds ("+usage.Hold.String()+"), and are billed short, and the longest since 2027-01-20T00:00:00Z")
	x.Contains(got, "2 workspaces of loft have sent no reading of loft.storage.bytes for longer than a reading holds ("+usage.Hold.String()+"), and are billed short: 1 of them never sent one, and the longest since 2027-01-20T00:00:00Z")

	w.read(stopped, "loft.assets.active", "{asset}", on(1, 25), on(1, 26))
	w.read(stopped, "loft.storage.bytes", "By", on(1, 25), on(1, 26))
	w.read(never, "loft.storage.bytes", "By", on(1, 25), on(1, 26))
	w.reconcile()
	w.nothing()
}
