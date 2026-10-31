package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/admin"
	"github.com/lesomnus/levyan/server/pd"
)

// An operator makes a pass of reconciliation now -- what it writes is on the
// trail as theirs -- lists and reads what it found, with the command that
// does what each asks for, and accepts one with a reason, which says who
// accepted it. The merchant of record's checks are passed over where none is
// named, and its corrections refused.
func TestAnOperatorReconcilesAndAcceptsWhatIsFound(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey, "bob": bobKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	key, _ := d.w.Key(loft, "loft-prod", nil)
	m := levyanv1.NewMeteringServiceClient(d.w.Serve())
	d.w.At(jan.AddDate(0, 0, 20))
	res, err := m.Record(levyantest.As(context.Background(), key), levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: pdid.New(pd.TenantDomain).String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 0, 10)), Value: 130, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	x.Equal(levyanv1.Outcome_OUTCOME_KEPT, res.GetResults()[0].GetOutcome())

	// January, not closed long after its end and its grace.
	d.w.At(jan.AddDate(0, 1, 5))
	var p admin.Pass
	d.as(aliceKey, http.MethodPost, "/admin/reconcile", map[string]string{}, &p)
	skipped := map[string]bool{}
	for _, c := range p.Checks {
		x.Empty(c.Err, c.Name)
		skipped[c.Name] = c.Skipped
	}
	x.True(skipped["merchant"], "no merchant of record is named")
	x.False(skipped["periods"])
	x.Equal(1, p.New)
	x.Equal(1, p.Open)
	x.Len(p.Findings, 1)
	f := p.Findings[0]
	x.Equal("period_unclosed", f.Kind)
	x.Equal("warning", f.Severity)
	x.Equal("open", f.State)
	x.Equal("loft", f.Product)
	x.Equal("close", f.Correction)
	x.Equal("levyan-admin period close --yes loft", f.Command)
	x.Equal(d.operator("alice"), d.actorOf(f.Id), "found by alice's pass, on the trail as hers")

	var vs []admin.Finding
	d.as(aliceKey, http.MethodGet, "/admin/findings", nil, &vs)
	x.Len(vs, 1)
	d.as(aliceKey, http.MethodGet, "/admin/findings?kind=period_unclosed&product=loft", nil, &vs)
	x.Len(vs, 1)
	d.as(aliceKey, http.MethodGet, "/admin/findings?state=resolved", nil, &vs)
	x.Empty(vs)
	code, _ := d.ask(aliceKey, http.MethodGet, "/admin/findings?kind=nonsense", nil)
	x.Equal(http.StatusBadRequest, code)
	code, _ = d.ask(aliceKey, http.MethodGet, "/admin/findings?product=nobody", nil)
	x.Equal(http.StatusNotFound, code)

	var v admin.Finding
	d.as(aliceKey, http.MethodGet, "/admin/findings/"+f.Id, nil, &v)
	x.Equal(f.Subject, v.Subject)

	code, _ = d.ask(bobKey, http.MethodPost, "/admin/findings/"+f.Id+"/accept", map[string]string{"reason": " "})
	x.Equal(http.StatusBadRequest, code, "with a reason")
	var ok admin.Finding
	d.as(bobKey, http.MethodPost, "/admin/findings/"+f.Id+"/accept", map[string]string{"reason": "January is closed by hand on the 10th"}, &ok)
	x.Equal("accepted", ok.State)
	x.Equal("bob", ok.AcceptedBy)
	x.Equal("January is closed by hand on the 10th", ok.Reason)
	x.Empty(ok.Command, "nothing more to do")

	d.as(aliceKey, http.MethodPost, "/admin/reconcile", map[string]string{}, &p)
	x.Zero(p.Open)
	x.Equal(1, p.Found)

	// The merchant of record's corrections, with none named.
	code, b := d.ask(aliceKey, http.MethodPost, "/admin/attempts/lv-0001/refund", map[string]string{})
	x.Equal(http.StatusConflict, code, "%s", b)
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/attempts/lv-0001/settle", map[string]string{})
	x.Equal(http.StatusNotFound, code, "no attempt is lv-0001")

	// The rules, followed again: what the product is granted, after.
	var gs []admin.Grant
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/rules", map[string]string{}, &gs)
	x.Empty(gs)

	code, _ = d.ask("", http.MethodPost, "/admin/reconcile", map[string]string{})
	x.Equal(http.StatusUnauthorized, code)
}
