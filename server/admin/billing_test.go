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

// An operator says a product's terms, closes its periods by hand -- on the
// trail as theirs -- and reads the statements that came of it.
func TestAnOperatorClosesAProductsPeriodsAndReadsItsStatements(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Product("loft")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_PG_KR, "KRW", 100, 50, jan)
	key, _ := d.w.Key(loft, "loft-prod", nil)
	m := levyanv1.NewMeteringServiceClient(d.w.Serve())

	d.w.At(jan.AddDate(0, 1, 0))
	ws := pdid.New(pd.TenantDomain)
	res, err := m.Record(levyantest.As(context.Background(), key), levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 0, 20)), Value: 130, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	x.Equal(levyanv1.Outcome_OUTCOME_KEPT, res.GetResults()[0].GetOutcome())

	// Not priced yet: nothing to close.
	code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{})
	x.Equal(http.StatusConflict, code, "%s", b)

	var p admin.Detail
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/products/loft/terms", map[string]string{"channel": "mor"})
	x.Less(code, 300)
	code, b = d.ask(aliceKey, http.MethodPost, "/admin/products/loft/terms", map[string]string{"zone": "Atlantis/Lost"})
	x.Equal(http.StatusBadRequest, code, "%s", b)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/terms", map[string]string{"zone": "UTC", "channel": "pg_kr", "currency": "KRW"}, &p)
	x.Equal("pg_kr", p.Channel)
	x.Equal("KRW", p.Currency)
	x.Equal("UTC", p.Zone)

	// A period that has not ended does not close, however it is asked.
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{"through": jan.AddDate(1, 0, 0)})
	x.Equal(http.StatusBadRequest, code)

	var ps []admin.Period
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, &ps)
	x.Len(ps, 1, "January, before its grace: by hand")
	x.True(ps[0].Start.Equal(jan))
	x.NotNil(ps[0].Closed)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, &ps)
	x.Empty(ps, "and once")

	d.as(aliceKey, http.MethodGet, "/admin/products/loft/periods", nil, &ps)
	x.Len(ps, 1)

	var sts []admin.Statement
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/statements?workspace="+ws.String(), nil, &sts)
	x.Len(sts, 1)
	x.Equal(int64(1500), sts[0].Total, "130 last, 30 past what is included, at 50")
	x.Equal(ws.String(), sts[0].Workspace)

	var st admin.Statement
	d.as(aliceKey, http.MethodGet, "/admin/statements/"+sts[0].Id, nil, &st)
	x.Len(st.Lines, 1)
	x.Equal("loft.assets.active", st.Lines[0].Meter)
	x.Equal("usage", st.Lines[0].Kind)
	x.Equal("130", st.Lines[0].Usage)
	x.Equal("1500", st.Lines[0].Amount)
	x.Equal("1500", st.Exact)

	// The trail says who closed it.
	x.Equal(d.operator("alice"), d.actorOf(st.Id))
	x.Equal(d.operator("alice"), d.actorOf(ps[0].Id))
}
