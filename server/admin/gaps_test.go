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

// An operator asks which workspaces have stopped sending readings, of every
// product or of one, and is told what the loop that looks for them finds.
func TestAnOperatorAsksWhichWorkspacesStoppedSendingReadings(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X

	loft, other := d.w.Product("loft"), d.w.Product("other")
	d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Meter(other, "other.things", "{thing}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	loftKey, _ := d.w.Key(loft, "loft-prod", nil)
	otherKey, _ := d.w.Key(other, "other-prod", nil)
	m := levyanv1.NewMeteringServiceClient(d.w.Serve())

	quiet := pdid.New(pd.TenantDomain)
	last := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	for _, v := range []struct {
		key, meter, unit string
	}{{loftKey, "loft.assets.active", "{asset}"}, {otherKey, "other.things", "{thing}"}} {
		res, err := m.Record(levyantest.As(context.Background(), v.key), levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
			levyanv1.Reading_builder{Workspace: quiet.String(), Meter: v.meter, MeasuredAt: timestamppb.New(last), Value: 1, Unit: v.unit}.Build(),
		}}.Build())
		x.NoError(err)
		x.Equal(levyanv1.Outcome_OUTCOME_KEPT, res.GetResults()[0].GetOutcome())
	}

	var vs []admin.Gap
	d.as(aliceKey, http.MethodGet, "/admin/gaps", nil, &vs)
	x.Len(vs, 2)

	d.as(aliceKey, http.MethodGet, "/admin/gaps?product=loft", nil, &vs)
	x.Equal([]admin.Gap{{Product: "loft", Workspace: quiet.String(), Meter: "loft.assets.active", Last: last}}, vs)

	code, _ := d.ask("", http.MethodGet, "/admin/gaps", nil)
	x.Equal(http.StatusUnauthorized, code)
}
