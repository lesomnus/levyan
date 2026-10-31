package cmd

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/lesomnus/payday/trail"
	"github.com/lesomnus/payday/watch"
	"github.com/lesomnus/payday/web"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/server/bare"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/book"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/ledger"
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
	rec := bare.Recorders{
		pd.Recorder(content),
		pd.ResourceRecorder(w, c.Watch.Outbox, nil, resource.RecorderOptions{}),
		pd.AccessRecorder(),
	}

	s := &Server{Clock: time.Now}
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
	stacked, err := levyan.Build(walled.WithWatch(w), pd.AuditBuild(), pd.SecretBuild(), book.Build(clock), pd.GateBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	// And the same servers with no wall and no gate, which is what the
	// deployment does its own work through -- the same rules of the price
	// book on it, since an operator's price is held to them as a product's
	// would be. It still keeps the trail: going around the wall is not going
	// around the record of having done so.
	ungated, err := levyan.Build(sink.WithWatch(w), pd.AuditBuild(), pd.SecretBuild(), book.Build(clock))
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

	if c.Watch.Outbox && b != nil {
		// The loop that makes an event durable.
		s.Spin = append(s.Spin, pd.Drain(client, b, c.Watch.Every()))
	}

	// The loop that closes a product's period once it has waited out its
	// grace for what comes late (`levyan.periods.close`): a statement of each
	// workspace, written through the server with no wall, since a period is
	// closed for every workspace of a product and by no caller of it.
	s.Closer = billing.Closer{S: ungated, Drv: drv, Dialect: dialect, Now: clock, Grace: c.Periods.GraceAfter()}
	s.Spin = append(s.Spin, billing.ClosePeriods(s.Closer, c.Periods.CloseEvery()))

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

func (s *Server) Close() error { return s.Db.Close() }

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
	resolver := keys.Resolver(s.Ungated, s.Clock)

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
