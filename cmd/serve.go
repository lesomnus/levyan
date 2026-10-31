package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"google.golang.org/grpc"

	"github.com/lesomnus/payday/access"
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/grpcx"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/resource"
	"github.com/lesomnus/payday/spin"
	"github.com/lesomnus/payday/trail"
	"github.com/lesomnus/payday/watch"
	"github.com/lesomnus/payday/web"
	"github.com/lesomnus/roster/standing"
	"google.golang.org/protobuf/reflect/protoreflect"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/keyring"
	"github.com/lesomnus/levyan/server/bare"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/book"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/payers"
	"github.com/lesomnus/levyan/server/pd"
)

// Server is a built app: the database it runs on and the two stacks it answers
// through.
type Server struct {
	Db  *sql.DB
	Ent *ent.Client

	// Drv is what the client was built on, kept because a transaction is begun
	// on a driver and a `*ent.Client` does not hand out the one it holds. It is
	// what `pd.Batch` puts a whole stack onto.
	Drv dialect.Driver

	// Dialect is what that driver speaks, which `Open` worked out and nothing
	// else can.
	Dialect string

	// Clock is the deployment's time: every stamp the servers write, what the
	// price book holds a version's dates to (`server/book`), what a reading is
	// too far ahead of, and when a period has waited out its grace. One clock,
	// so that a version published "now" is effective at the moment it says it
	// was published.
	//
	// Everything [Build] makes reads the time through this field, so a test
	// may set it -- before it serves anything -- and have the whole
	// deployment be at another moment.
	Clock func() time.Time

	// Watch is what a change is published to once the call that made it has
	// answered.
	Watch *watch.Watch

	// Walled is what a caller reaches, and Ungated is what the deployment does
	// its own work through -- working out who is calling, and what an operator
	// asks of it through `levyan-admin serve`. Neither is a privilege anybody
	// holds: the second is a server instance somebody was handed, so going
	// around the wall is a line of wiring a reader can find rather than a rule
	// that opens up whenever nobody is asking.
	Walled  levyan.Server
	Ungated levyan.Server

	// Auth is how a credential is read, and Policy is what its caller may do
	// and which tenants they may see. Both are the door a binary puts on the
	// server, and so are its own to set after [Build]: `levyan serve` takes
	// products by their keys (`cli.Front`), and the operator's binary serves
	// no gRPC at all. [Server.Grpc] refuses to serve without both, rather
	// than believing what a caller writes about itself (`auth.Plain`) or
	// letting every caller do everything in its own tenant (a nil policy).
	Auth   auth.Handler
	Policy gate.Policy

	// Resolver is the frame a credential [Server.Auth] read is served in:
	// the door's too, and a product's key's (`keys.Resolver`) when it says
	// nothing.
	Resolver auth.Resolver

	// Register is what else the binary serves beside the generated services,
	// on the same server and behind the same chain: `levyan.v1`, for one.
	Register []func(grpc.ServiceRegistrar)

	// Access is the access record's writer (payday#111): what a read's row
	// waits in to be written with the others of its second, and what writes a
	// download's at once. Its loop is in [Server.Spin], and each process runs
	// its own, since what it holds is in this one's memory.
	Access *access.Writer

	// Downloads are the methods that hand personal data out to be kept
	// somewhere else, which say what for or are refused. levyan has none yet.
	Downloads []string

	// Closer closes products' periods, as the deployment: by the loop
	// `levyan.periods.close`, and by an operator's hand through the
	// operator's entry point.
	Closer billing.Closer

	// Invoices is what billing accounts are asked to pay, and the money it
	// moves (#9), through the server with no wall: by the loop
	// `levyan.invoices.collect`, and by an operator's hand. Its Collectors
	// are how each channel is collected, which a deployment wires, or a test.
	Invoices invoices.Invoices

	// Grants gives and ends what products' workspaces are granted, through
	// the server with no wall: by the loop `levyan.grants.renew`, and by an
	// operator's hand through the operator's entry point. Its Rules are the
	// hook billing state gives grants by (`grants.Rule`).
	Grants grants.Grants

	// BillingKeys is what a billing key is sealed under before it is
	// written, and opened with for a charge (`pg.BillingKey.Seal`,
	// `pg.OpenBillingKey`): the keyring `billing_keys` says. Nil when the
	// deployment names none, which seals and opens nothing.
	BillingKeys *keyring.Keyring

	// Payers is who pays for products' workspaces, through the server with no
	// wall: what roster says of an account is written down through it. A
	// product's call and an account's are served through the walled stack
	// ([payers.Payers.On]).
	Payers payers.Payers

	// Standing is what roster says of an account a product relays a call for
	// (`auth.roster`): roster's `standing.Accounts`, and nil when no roster is
	// named, which serves no account's call. A test sets one of its own.
	Standing payers.Standing

	// Roster is that roster, asked again and of everybody now and then by the
	// loops; nil when none is named.
	Roster *standing.Accounts

	// Free is how many of each payer's workspaces of a product have their
	// allowances (`payers.free`).
	Free int

	closeRoster func() error

	// Spin is whatever this deployment has to run besides answering requests,
	// each a loop with a name of its own (`spin.Called`), run under
	// `spin.lease` by the serve command: in one process of the deployment, but
	// for what each process runs for itself (`spin.InEveryProcess`).
	Spin []any
}

// Build opens the database and stacks the servers.
//
// The two hooks are the whole of what payday puts in the write and read paths,
// and both come out of what the schema declared: [pd.Minter] stamps a new row
// with the domain of its entity and refuses one of another, [pd.Wall] narrows
// every read to the tenants the caller may see.
func Build(ctx context.Context, c Config) (*Server, error) {
	// The keys billing keys are sealed under, before anything else: a
	// deployment that names keys this process cannot read, or a kind it does
	// not have, is refused before it touches the database.
	var bk *keyring.Keyring
	if !c.BillingKeys.IsZero() {
		var err error
		if bk, err = c.BillingKeys.Open(); err != nil {
			return nil, fmt.Errorf("billing_keys.%w", err)
		}
	}

	// What a billing account is billed through, by its country, is read
	// before anything is opened: a map that bills through nothing stops the
	// deployment rather than its first payer.
	channels, err := c.Payers.ChannelsOf()
	if err != nil {
		return nil, fmt.Errorf("payers.channels: %w", err)
	}

	db, dialect, err := c.Db.Open(ctx)
	if err != nil {
		return nil, err
	}

	// The ent client is the app's to build, which is why config hands back a
	// *sql.DB and the dialect rather than a client: the client is generated
	// into this app from this app's schema, and payday has no name for it.
	drv := entsql.OpenDB(dialect, db)
	client := ent.NewClient(ent.Driver(drv))

	b, err := c.Watch.Build(ctx, c.Db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if b == nil {
		// What a product is granted is streamed to it (`payday.GrantService`
		// Watch), from the outbox through the broker; a deployment that
		// publishes nowhere would answer every product's stream with nothing.
		db.Close()
		return nil, errors.New("watch.broker: levyan streams what it grants to products, so it publishes: `memory` for one process, `postgres` for more")
	}

	w := watch.New(b)

	// The recorders, in the order they are told, inside the transaction that
	// makes each write. The trail is required -- a write it could not account
	// for is undone -- and what it keeps of each kind's writes is the
	// deployment's to say, `audit.content`, read here where the trail is
	// wired. The access record's is last: what it writes at the commit is of
	// the data the call's writes wrote, so it is told of each and writes once.
	content, err := c.Audit.Contents()
	if err != nil {
		db.Close()
		return nil, err
	}

	// A grant's change is in the outbox always, whatever `watch.outbox` says
	// of the rest: written in the transaction that makes it and published by
	// the drain once it is committed, which is what a product's stream of its
	// grants is fed by (`server/api`). So a grant an operator gives through
	// `levyan-admin`, another process, reaches the stream as one the loop
	// gives does, and none is lost between the commit and the publish.
	rec := bare.Recorders{
		pd.Recorder(content),
		pd.ResourceRecorder(w, c.Watch.Outbox, not(isGrant), resource.RecorderOptions{}),
		pd.ResourceRecorder(nil, true, isGrant, resource.RecorderOptions{}),
		pd.AccessRecorder(),
	}

	s := &Server{Clock: time.Now, BillingKeys: bk}
	clock := func() time.Time { return s.Clock() }
	opts := []bare.Option{bare.WithMinter(pd.Minter()), bare.WithRecorder(rec), bare.WithClock(clock)}

	sink, err := pd.NewSink(client, opts...)
	if err != nil {
		db.Close()
		return nil, err
	}

	walled, err := pd.NewSink(client, append(opts, bare.WithScope(pd.Wall()))...)
	if err != nil {
		db.Close()
		return nil, err
	}

	// The stack a caller reaches. The price book's rules (`server/book`) are
	// behind the gate, so that what they read of a row is what the gate has
	// already let the caller name; `pd.Gate` is outermost, so nothing behind
	// it asks again. A key's verifier is cleared on the way out of both
	// (`pd.Secret`), so that nothing answers with it.
	stacked, err := levyan.Build(walled.WithWatch(w), pd.AuditBuild(), pd.SecretBuild(), book.Build(clock), grants.Build(clock), payers.Build(channels), invoices.Build(), pd.GateBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	// And the same servers with no wall and no gate, which is what the
	// deployment does its own work through -- the same rules of the price
	// book and of grants on it, since an operator's price is held to them as
	// a product's would be. It still keeps the trail: going around the wall
	// is not going around the record of having done so.
	ungated, err := levyan.Build(sink.WithWatch(w), pd.AuditBuild(), pd.SecretBuild(), book.Build(clock), grants.Build(clock), payers.Build(channels), invoices.Build())
	if err != nil {
		db.Close()
		return nil, err
	}

	s.Db, s.Ent, s.Drv, s.Dialect, s.Watch, s.Walled, s.Ungated = db, client, drv, dialect, w, stacked, ungated

	// The access record's reads are written a second at a time, by a loop of
	// each process's own (`payday.access`) that writes what it holds once
	// more as it stops.
	s.Access = &access.Writer{Sink: pd.AccessSink(client)}
	s.Spin = append(s.Spin, s.Access)

	// The loop that publishes what the outbox holds -- every grant's change,
	// and the rest when `watch.outbox` says so -- once it is committed.
	s.Spin = append(s.Spin, pd.Drain(client, b, c.Watch.Every()))

	// The loop that closes a product's period once it has waited out its
	// grace for what comes late (`levyan.periods.close`): a statement of each
	// workspace, written through the server with no wall, since a period is
	// closed for every workspace of a product and by no caller of it.
	s.Free = c.Payers.FreeOf()
	s.Closer = billing.Closer{S: ungated, Drv: drv, Dialect: dialect, Now: clock, Grace: c.Periods.GraceAfter(), Free: s.Free}
	s.Spin = append(s.Spin, billing.ClosePeriods(s.Closer, c.Periods.CloseEvery()))

	// The loop that invoices what the close wrote, and moves what is due
	// (`levyan.invoices.collect`): each statement with a billing account on
	// one invoice of it, each draft decided once its period is closed, the
	// legal documents of a seller issued, and attempts sent and asked about
	// -- through the server with no wall, as the deployment. What collects on
	// each channel is wired beside it; a channel nothing collects on keeps
	// its drafts, and asks nothing of anybody.
	tax, err := c.Invoices.TaxOf()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("invoices.tax: %w", err)
	}
	s.Invoices = invoices.Invoices{S: ungated, Drv: drv, Dialect: dialect, Now: clock, Collectors: map[levyan.Channel]invoices.Collector{}, Tax: tax}
	s.Spin = append(s.Spin, invoices.Collects(&s.Invoices, c.Invoices.CollectEvery()))

	// The loop that keeps what products' workspaces are granted
	// (`levyan.grants.renew`): what the rules want given and what they no
	// longer want ended, the grants that renew renewed before they end, and
	// those over for long enough erased out of every list -- through the
	// server with no wall, as the deployment.
	s.Grants = grants.Grants{S: ungated, Drv: drv, Dialect: dialect, Now: clock, Horizon: c.Grants.HorizonOf()}
	s.Spin = append(s.Spin, grants.Renewals(&s.Grants, c.Grants.RenewEvery()))

	// Who pays (#4): through the server with no wall for the deployment's
	// own work, and a rule of the grants' -- a workspace nobody has paid for
	// since `payers.grace` ago is restricted.
	s.Payers = payers.Payers{S: ungated, Drv: drv, Dialect: dialect, Now: clock, Grace: c.Payers.GraceOf()}
	s.Grants.Rules = append(s.Grants.Rules, payers.Payerless{Grace: c.Payers.GraceOf()})

	// roster, whose accounts pay: asked whether each account a product relays
	// a call for is in good standing, and heard saying when one is suspended
	// or erased. Its stream runs in every process, since what it hears drops
	// what each process knows of an account (roster's `docs/relying-app.md`);
	// asking again before acting on what it said, and asking about everybody
	// now and then, in one.
	accounts, closeRoster, err := Roster(ctx, c.Auth.Roster)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.closeRoster = closeRoster
	if accounts != nil {
		s.Roster, s.Standing = accounts, accounts
		s.Spin = append(s.Spin,
			spin.InEveryProcess(accounts.Follower(payers.StreamLoop, s.Payers.Heard)),
			payers.Settles(&s.Payers, accounts, c.Payers.SettleEvery()),
			payers.Reconciles(&s.Payers, accounts, c.Payers.ReconcileEvery()),
		)
	} else {
		log.From(ctx).WarnContext(ctx, "roster: auth.roster names none, so no account's standing can be asked, and no call an account makes is served")
	}

	// The loop that looks for readings that should have come and have not
	// (`levyan.readings.gaps`): a workspace that has been sending a stock
	// meter's readings and has stopped is one whose bill would be short, and
	// nothing else would say so.
	s.Spin = append(s.Spin, ledger.LookForGaps(client, clock, c.Readings.LookEvery(), ledger.Expect{
		Gap:    c.Readings.GapAfter(),
		Recent: c.Readings.RecentWithin(),
	}))

	// And the trail's retention, which is the one loop here that is a
	// **mechanism** rather than a tidy-up: nothing else applies a window, so an
	// outage of it is a deployment keeping records it said it would not.
	// Resolved and checked here, where a refusal stops the process. Configured
	// nowhere, it does nothing and the trail is kept forever -- which a billing
	// service's trail, the evidence of what it charged, is until somebody who
	// knows what it is regulated as says otherwise.
	p, err := c.Audit.Policy()
	if err != nil {
		db.Close()
		return nil, err
	}

	// The trail's turns with whatever erases from it, on the deployment's
	// lease, so that neither undoes the other. A process that runs no loops
	// (`none`) has nothing to take turns on.
	if c.Spin.Lease != "" && c.Spin.Lease != config.LeaseNone {
		if p.Lease, err = c.Spin.Build(ctx, c.Db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if p.On() {
		log.From(ctx).InfoContext(ctx, "trail: retention", "policy", p.String())

		s.Spin = append(s.Spin, trail.Sweep(pd.TrailStore(client), p))
	}

	return s, nil
}

func (s *Server) Close() error {
	err := s.Db.Close()
	if s.closeRoster != nil {
		err = errors.Join(err, s.closeRoster())
	}

	return err
}

// Grpc builds the server every call arrives at, with the door the binary put
// on it ([Server.Auth], [Server.Policy]).
//
// The chain is payday's and the order is this app's to read: what records a
// call is outside everything, then the recovery, then the deadline a call that
// named none is given, then who is calling, how often one caller may ask, the
// access record, what this caller may do, and what is closed to callers
// entirely.
func (s *Server) Grpc(ctx context.Context, c Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	if s.Auth == nil || s.Policy == nil {
		return nil, errors.New("cmd: nothing says who is calling and what they may do here (Server.Auth, Server.Policy), and a server is not served open")
	}

	// Who the caller is, from the key they sent and the row it is: the key
	// itself, in its product's tenant (`server/keys`). Read through the server
	// with no wall, since working out who is calling happens before there is
	// anybody to be walled by.
	resolver := s.Resolver
	if resolver == nil {
		resolver = keys.Resolver(s.Ungated, s.Clock)
	}

	chain := grpcx.Serving(ctx, grpcx.WithDeadline(c.Server.CallTimeout())).
		WithUnary(auth.InterceptorUnary(s.Auth, resolver, auth.PublicDefault)).
		WithStream(auth.InterceptorStream(s.Auth, resolver, auth.PublicDefault)).
		WithUnary(grpcx.LimitUnary(c.Server.Limiter(), gate.ByTenant(), pd.Cost)).
		With(s.accessed().Interceptor()).
		With(gate.Interceptor(s.Policy)).
		With(s.Watch.Interceptor()).
		WithUnary(grpcx.ClosedUnary(c.Server.Closed()))

	os := append(opts, chain.ServerOptions()...)
	vs, err := c.Server.GrpcOptions()
	if err != nil {
		return nil, err
	}

	os = append(os, vs...)

	g := grpc.NewServer(os...)
	levyan.RegisterServer(g, s.Walled)
	for _, r := range s.Register {
		r(g)
	}

	// The batch, with the same rules the chain above enforces, per operation.
	guard := c.Server.Guard(s.Policy)
	if b, err := pd.Batch(s.Walled, s.Drv, guard); err == nil {
		pdpb.RegisterBatchServiceServer(g, b)

		resources, err := pd.Resources(s.Walled, s.Drv, guard, s.Watch,
			pd.ResourceProjector(s.Ent, pd.Wall(), guard))
		if err != nil {
			return nil, err
		}
		pdpb.RegisterResourceServiceServer(g, resources)
	} else {
		log.From(ctx).WarnContext(ctx, "no batch", slog.Any("err", err))
	}

	return g, nil
}

// accessed is what the access record's interceptor is told of this app: whose
// data a message carries, read off what its schema declared `personal`; where a
// call came from; and which of its calls are downloads.
//
// Where a call came from is the connection's here. Behind a proxy that is the
// proxy, and the caller is in a header only the deployment knows whether to
// believe: answer `Address` with that, past the proxies it trusts.
func (s *Server) accessed() access.Config {
	return access.Config{
		Subjects:  pd.Subjects,
		Refs:      pd.SubjectRefs,
		Address:   access.Peer,
		Downloads: s.Downloads,
		Writer:    s.Access,
	}
}

// Serve answers on `l` until the context is done, and lets what it is
// answering end before it returns.
func (s *Server) Serve(ctx context.Context, c Config, l net.Listener) error {
	g, err := s.Grpc(ctx, c)
	if err != nil {
		return err
	}

	stop, err := s.serveHttp(ctx, c, g)
	if err != nil {
		return err
	}
	defer stop()

	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		g.GracefulStop()
		close(stopped)
	}()

	err = g.Serve(l)
	if ctx.Err() == nil {
		return err
	}

	// Asked to stop: what was being answered ends first, and what it read is
	// written after it.
	<-stopped
	last, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Access.Flush(last); err != nil {
		log.From(ctx).ErrorContext(ctx, "access: the record waiting to be written was lost as the server stopped", "rows", s.Access.Pending(), "err", err)
	}

	return err
}

// serveHttp is the second listener, for a caller that speaks Connect or
// gRPC-Web rather than gRPC -- a shell with curl, an app in a language with no
// gRPC of its own. Nothing is opened unless the configuration named an address,
// and levyan's names none: what calls it is a product's backend.
//
// It is the **same** `g`: through the interceptors a gRPC client goes through,
// behind the same wall.
func (s *Server) serveHttp(ctx context.Context, c Config, g *grpc.Server) (func(), error) {
	if !c.Server.Http.Serves() {
		return func() {}, nil
	}

	h, err := web.New(c.Server.Http, g)
	if err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", c.Server.Http.Addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.From(ctx).ErrorContext(ctx, "http", slog.Any("err", err))
		}
	}()

	log.From(ctx).InfoContext(ctx, "http", slog.String("addr", l.Addr().String()))

	return func() { srv.Close() }, nil
}

// grantName is a grant's message, as an event of a change names its type.
var grantName = (&levyan.Grant{}).ProtoReflect().Descriptor().FullName()

// isGrant selects the events of a change of a grant.
func isGrant(_ context.Context, e *pdpb.WatchEvent) (bool, error) {
	var name protoreflect.FullName
	if v := e.GetValue(); v != nil {
		name = v.MessageName()
	} else {
		name = e.GetPrevious().MessageName()
	}

	return name == grantName, nil
}

// not is the events `f` does not select.
func not(f resource.Select) resource.Select {
	return func(ctx context.Context, e *pdpb.WatchEvent) (bool, error) {
		ok, err := f(ctx, e)
		return !ok, err
	}
}
