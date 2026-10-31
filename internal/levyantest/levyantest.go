// Package levyantest is a levyan to test against: built as `levyan serve`
// builds one, on a database of the test's own -- an in-memory SQLite, or a
// schema of its own on the PostgreSQL `PDTEST_POSTGRES` names -- and migrated.
//
// What a test arranges it arranges through the server with no wall
// (`cmd.Server.Ungated`), as an operator's entry point does, so that the rows
// it stands on went through the servers and the price book's rules like any
// other. What it asks is then asked as a caller asks it.
package levyantest

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/cli"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/pd"
)

// World is a levyan, built and migrated.
type World struct {
	X *require.Assertions
	C cmd.Config
	S *cmd.Server

	t *testing.T
}

// New is a levyan on a database of the test's own. `with` has a say over its
// configuration first.
func New(t *testing.T, with ...func(*cmd.Config)) *World {
	t.Helper()
	x := require.New(t)

	c := cmd.Config{}
	c.Db.Driver, c.Db.Dsn = pdtest.DB(t)
	c.Watch.Broker = "memory"
	for _, f := range with {
		f(&c)
	}

	s, err := cmd.Build(t.Context(), c)
	x.NoError(err)
	t.Cleanup(func() { s.Close() })
	x.NoError(cli.Migrate(t.Context(), s))

	return &World{X: x, C: c, S: s, t: t}
}

// Serve is the public binary's server over a connection that never leaves
// the process: the door `levyan serve` puts on it (`cli.Front`), and the
// whole chain a product's call goes through.
func (w *World) Serve() *grpc.ClientConn {
	w.t.Helper()

	cli.Front(w.S)
	g, err := w.S.Grpc(w.t.Context(), w.C, pdtest.Logging(w.t))
	w.X.NoError(err)

	return pdtest.Serve(w.t, g)
}

// Product is a product put up, as an operator puts one up.
func (w *World) Product(alias string) *levyan.Tenant {
	w.t.Helper()

	v, err := w.S.Ungated.Tenant().Add(w.t.Context(), levyan.TenantAddRequest_builder{Alias: alias, Name: alias}.Build())
	w.X.NoError(err)
	return v
}

// Key is a key issued to a product: the key itself, and its row.
func (w *World) Key(p *levyan.Tenant, alias string, expires *time.Time) (string, *levyan.ServiceKey) {
	w.t.Helper()

	token, v, err := keys.Issue(w.t.Context(), w.S.Ungated, Ref(p), alias, "", expires)
	w.X.NoError(err)
	return token, v
}

// Meter is a meter written for a product.
func (w *World) Meter(p *levyan.Tenant, key, unit string, kind levyan.MeterKind, agg levyan.Aggregation) *levyan.Meter {
	w.t.Helper()

	v, err := w.S.Ungated.Meter().Add(w.t.Context(), levyan.MeterAddRequest_builder{
		Tenant: Ref(p), Key: key, Name: key, Unit: unit, Kind: kind, Aggregation: agg,
	}.Build())
	w.X.NoError(err)
	return v
}

// Price is a version of a meter's price published by the unit, effective
// from `at`.
func (w *World) Price(p *levyan.Tenant, m *levyan.Meter, channel levyan.Channel, currency string, included, unitPrice int64, at time.Time) *levyan.Price {
	w.t.Helper()

	v, err := w.S.Ungated.Price().Add(w.t.Context(), levyan.PriceAddRequest_builder{
		Tenant:        Ref(p),
		Meter:         levyan.MeterRef_builder{Id: m.GetId()}.Build(),
		Channel:       channel,
		Currency:      currency,
		Included:      included,
		UnitPrice:     unitPrice,
		DateEffective: timestamppb.New(at),
	}.Build())
	w.X.NoError(err)
	return v
}

// At is the deployment at `t`: every stamp the servers write from now on,
// what a reading may not be ahead of, what a price may not reach back before,
// and when a period has waited out its grace. Set it before serving anything,
// or between calls.
func (w *World) At(t time.Time) {
	w.S.Clock = func() time.Time { return t }
}

// Terms are a product's terms set, as an operator sets them: the zone its
// periods are months in, and the channel and currency they are priced on.
func (w *World) Terms(p *levyan.Tenant, zone string, channel levyan.Channel, currency string) *levyan.Tenant {
	w.t.Helper()

	name := ""
	if channel != levyan.Channel_CHANNEL_UNSPECIFIED {
		name = channel.String()
	}
	v, err := w.S.Ungated.Tenant().Patch(w.t.Context(), levyan.TenantPatchRequest_builder{
		Ref:              Ref(p),
		Zone:             proto.String(zone),
		Channel:          proto.String(name),
		Currency:         proto.String(currency),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	w.X.NoError(err)
	return v
}

// Ref is a product by its identifier.
func Ref(p *levyan.Tenant) *levyan.TenantRef {
	return levyan.TenantRef_builder{Id: p.GetId()}.Build()
}

// As is a context whose calls carry a key, as a product's do.
func As(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// Drain publishes what the outbox holds as `levyan serve`'s loop does
// (`payday.outbox`), every few milliseconds for as long as the test runs: what
// a product's stream of its grants is fed by.
func (w *World) Drain() {
	w.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d := pd.Drain(w.S.Ent, w.S.Watch.Broker(), 5*time.Millisecond)
	go func() {
		defer close(done)
		_ = d.Spin(ctx)
	}()
	w.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// Tenant is a workspace as a product names one, new: the identifier of a
// payday tenant of its own.
func Tenant() pdid.Id { return pdid.New(pd.TenantDomain) }
