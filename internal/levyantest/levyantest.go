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
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/payday/frame"
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
	"github.com/lesomnus/levyan/internal/ent"
	entoperator "github.com/lesomnus/levyan/internal/ent/operator"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/pd"
	"github.com/lesomnus/levyan/server/relay"
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

	// roster, as a test has it: every account in good standing but those
	// it is told otherwise of. Serve with it nil to have none configured.
	s.Standing = &Standing{}

	return &World{X: x, C: c, S: s, t: t}
}

// Again is this levyan come up again on the same database, as a deployment
// restarted with other configuration is: `with` has a say over a copy of its
// configuration -- replacing what it changes rather than writing into what
// the copy shares -- and the server keeps this one's clock and roster.
func (w *World) Again(with ...func(*cmd.Config)) *cmd.Server {
	w.t.Helper()

	c := w.C
	for _, f := range with {
		f(&c)
	}

	s, err := cmd.Build(w.t.Context(), c)
	w.X.NoError(err)
	w.t.Cleanup(func() { s.Close() })
	s.Clock = func() time.Time { return w.S.Clock() }
	s.Standing = w.S.Standing

	return s
}

// Standing is roster's word on accounts, as a test says it: good for every
// account but those Refuse has been told of.
type Standing struct {
	mu      sync.Mutex
	refused map[pdid.Id]error
}

// Refuse has `account` answered with `err` -- `standing.ErrRefused`,
// `standing.ErrUnreachable` -- or good again with nil.
func (s *Standing) Refuse(account pdid.Id, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		s.refused = map[pdid.Id]error{}
	}
	if err == nil {
		delete(s.refused, account)
		return
	}
	s.refused[account] = err
}

func (s *Standing) Held(_ context.Context, account pdid.Id, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused[account]
}

// Account is an account of roster's, new.
func Account() pdid.Id { return pdid.New(pd.HolderDomain) }

// AsAccount is a context whose calls carry a product's key and the account
// it relays them for, as a product's do for one of its users.
func AsAccount(ctx context.Context, token string, account pdid.Id) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token, relay.Header, account.String())
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

// Operator is `ctx` framed as an operator -- one written as the operator's
// entry point writes one of the names `admin.keys` gives (`server/admin`)
// -- as that entry point frames their requests: what is given beyond a
// price (#43) is an operator's alone.
func (w *World) Operator(ctx context.Context, alias string) context.Context {
	w.t.Helper()

	var v *levyan.Operator
	u, err := w.S.Ent.Operator.Query().Where(entoperator.Alias(alias), entoperator.DateErasedIsNil()).Only(w.t.Context())
	switch {
	case err == nil:
		v = levyan.Operator_builder{Id: u.Id[:], Alias: u.Alias}.Build()
	case ent.IsNotFound(err):
		v, err = w.S.Ungated.Operator().Add(w.t.Context(), levyan.OperatorAddRequest_builder{Alias: alias}.Build())
		w.X.NoError(err)
	default:
		w.X.NoError(err)
	}
	id, err := pdid.From(v.GetId())
	w.X.NoError(err)

	return frame.Into(ctx, frame.New(id, pdid.Nil, frame.Whole()).WithRow(v))
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

// Merchant is a levyan's configuration naming the fake Paddle `srv` as its
// merchant of record (`mor`): its keys -- made up for the fake as it starts,
// never written in a file -- its plans, its API and its clock; and, unless
// the configuration says one, collection on its channel beginning as the
// fake's clock is now (`invoices.mor.live_from`, which [LiveFrom] says
// otherwise). Without `webhooks`, no webhook secret is named, and the
// merchant's events are pulled alone.
func Merchant(srv *paddletest.Server, webhooks bool) func(*cmd.Config) {
	return func(c *cmd.Config) {
		m := &cmd.MorConfig{
			Kind:        "paddle",
			Environment: "sandbox",
			APIKey:      paddletest.Secret(srv.APIKey),
			ClientToken: srv.ClientToken,
			Plans:       srv.Plans,
			URL:         srv.URL,
			Clock:       srv.Now,
		}
		if webhooks {
			m.WebhookSecret = paddletest.Secret(srv.WebhookSecret)
		}
		c.Mor = m
		if c.Invoices.Mor.LiveFrom == nil {
			at := srv.Now()
			c.Invoices.Mor.LiveFrom = &at
		}
	}
}

// LiveFrom is a levyan's configuration in which collection on the merchant of
// record's channel begins at `at` (`invoices.mor.live_from`).
func LiveFrom(at time.Time) func(*cmd.Config) {
	return func(c *cmd.Config) { c.Invoices.Mor.LiveFrom = &at }
}
