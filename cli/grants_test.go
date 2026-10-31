package cli_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/pdtest"
	"github.com/lesomnus/payday/watch"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
)

// What a product's workspaces are granted, as the product hears of it:
// payday's `GrantService`, over a connection, through the whole chain a
// product's call goes through (#7).

type granting struct {
	w    *levyantest.World
	conn *grpc.ClientConn
}

func granted(t *testing.T) *granting {
	t.Helper()
	w := levyantest.New(t)
	w.Drain()

	return &granting{w: w, conn: w.Serve()}
}

func (g *granting) give(p *levyan.Tenant, ws pdid.Id, key string, v entitle.Value, with ...func(*grants.Give)) *levyan.Grant {
	give := grants.Give{
		Product: pdid.Id(p.GetId()), Workspace: ws, Key: key, Value: v,
		Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}
	for _, f := range with {
		f(&give)
	}
	out, err := g.w.S.Grants.Give(context.Background(), give)
	g.w.X.NoError(err)
	return out
}

func (g *granting) end(v *levyan.Grant) *levyan.Grant {
	out, err := g.w.S.Grants.End(context.Background(), pdid.Id(v.GetId()))
	g.w.X.NoError(err)
	return out
}

// watching is a product's stream of its grants, open and past its `ready`.
func (g *granting) watching(t *testing.T, key string) (grpc.ServerStreamingClient[pdpb.GrantWatchResponse], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(levyantest.As(context.Background(), key))
	t.Cleanup(cancel)

	s, err := pdpb.NewGrantServiceClient(g.conn).Watch(ctx, &pdpb.GrantWatchRequest{})
	g.w.X.NoError(err)
	m, err := s.Recv()
	g.w.X.NoError(err)
	g.w.X.True(m.HasReady(), "the first message of a stream is ready")

	return s, cancel
}

// next is the next grant a stream carries, within a few seconds.
func next(t *testing.T, s grpc.ServerStreamingClient[pdpb.GrantWatchResponse]) *pdpb.EntitlementGrant {
	t.Helper()
	got := make(chan *pdpb.GrantWatchResponse, 1)
	failed := make(chan error, 1)
	go func() {
		m, err := s.Recv()
		if err != nil {
			failed <- err
			return
		}
		got <- m
	}()

	select {
	case m := <-got:
		require.True(t, m.HasGrant(), "after ready, a stream carries grants")
		return m.GetGrant()
	case err := <-failed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "no grant came")
	}
	return nil
}

// list is the whole of what List answers a product with, a page of `size`
// at a time.
func (g *granting) list(t *testing.T, key string, tenant []byte, size int32) ([]*pdpb.EntitlementGrant, int) {
	t.Helper()
	ctx := levyantest.As(context.Background(), key)

	var out []*pdpb.EntitlementGrant
	pages, cursor := 0, ""
	for {
		res, err := pdpb.NewGrantServiceClient(g.conn).List(ctx, pdpb.GrantListRequest_builder{
			TenantId: tenant, Size: size, Cursor: cursor,
		}.Build())
		require.NoError(t, err)
		out = append(out, res.GetItems()...)
		pages++
		if cursor = res.GetNext(); cursor == "" {
			return out, pages
		}
	}
}

func ids(vs []*pdpb.EntitlementGrant) []string {
	out := []string{}
	for _, v := range vs {
		out = append(out, pdid.Id(v.GetId()).String())
	}
	slices.Sort(out)
	return out
}

func idsOf(vs ...*levyan.Grant) []string {
	out := []string{}
	for _, v := range vs {
		out = append(out, pdid.Id(v.GetId()).String())
	}
	slices.Sort(out)
	return out
}

// A stream begins with ready and then carries each grant as it is after it
// changed -- given, ended -- the whole of it every time, at a greater
// version, by the product's own identifier of the workspace.
func TestAStreamIsReadyAndThenEveryChange(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft := g.w.Product("loft")
	key, _ := g.w.Key(loft, "loft-prod", nil)
	ws := levyantest.Tenant()
	s, _ := g.watching(t, key)

	v := g.give(loft, ws, "loft.seats", entitle.Limited(5))
	m := next(t, s)
	x.Equal(v.GetId(), m.GetId())
	x.Equal(ws.Bytes(), m.GetTenantId(), "the workspace by the product's identifier of it")
	x.Equal("loft.seats", m.GetKey())
	x.Equal(uint64(1), m.GetVersion())
	x.Equal(int64(5), m.GetLimit())
	x.True(m.GetValidUntil().AsTime().After(time.Now()))

	g.end(v)
	m = next(t, s)
	x.Equal(v.GetId(), m.GetId())
	x.Equal(uint64(2), m.GetVersion(), "ended, at the version after")
	x.False(m.GetValidUntil().AsTime().After(time.Now()))

	u := g.give(loft, ws, "loft.export", entitle.On)
	m = next(t, s)
	x.Equal(u.GetId(), m.GetId())
	x.True(m.GetOn())
	o := g.give(loft, ws, "loft.robots", entitle.Unlimited)
	m = next(t, s)
	x.Equal(o.GetId(), m.GetId())
	x.True(m.HasUnlimited())
}

// A product hears of its own grants and of no other product's: not on its
// stream, not in its list, not by naming the other's workspace -- not even
// when two products' workspaces are one payday tenant's identifier.
func TestAProductHearsOfItsOwnGrantsAlone(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft, other := g.w.Product("loft"), g.w.Product("other")
	loftKey, _ := g.w.Key(loft, "loft-prod", nil)
	otherKey, _ := g.w.Key(other, "other-prod", nil)
	shared, mine, theirs := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()

	ls, _ := g.watching(t, loftKey)
	os, _ := g.watching(t, otherKey)

	a := g.give(other, shared, "other.export", entitle.On)
	b := g.give(other, theirs, "other.seats", entitle.Limited(9))
	c := g.give(loft, shared, "loft.export", entitle.On)
	d := g.give(loft, mine, "loft.seats", entitle.Limited(2))

	x.Equal(idsOf(c, d), ids([]*pdpb.EntitlementGrant{next(t, ls), next(t, ls)}), "the other's grants are passed over")
	x.Equal(idsOf(a, b), ids([]*pdpb.EntitlementGrant{next(t, os), next(t, os)}))

	vs, _ := g.list(t, loftKey, nil, 0)
	x.Equal(idsOf(c, d), ids(vs))
	vs, _ = g.list(t, loftKey, shared.Bytes(), 0)
	x.Equal(idsOf(c), ids(vs), "a workspace of both is each one's own")
	vs, _ = g.list(t, loftKey, theirs.Bytes(), 0)
	x.Empty(vs, "a workspace of the other's is nothing here")
	vs, _ = g.list(t, otherKey, nil, 0)
	x.Equal(idsOf(a, b), ids(vs))

	_, err := pdpb.NewGrantServiceClient(g.conn).List(levyantest.As(context.Background(), loftKey),
		pdpb.GrantListRequest_builder{TenantId: []byte("not a tenant")}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err))

	_, err = pdpb.NewGrantServiceClient(g.conn).List(context.Background(), &pdpb.GrantListRequest{})
	x.Equal(codes.Unauthenticated, status.Code(err), "and nobody hears of anything")
}

// A list is every grant in force or to come, and those ended for two days
// after, a page at a time; a grant over for longer is in it no more.
func TestAListIsWhatIsInForceAndWhatEndedLately(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft := g.w.Product("loft")
	key, _ := g.w.Key(loft, "loft-prod", nil)
	ws := levyantest.Tenant()

	now := time.Now().UTC().Truncate(time.Second)
	g.w.At(now)
	vs := []*levyan.Grant{}
	for _, k := range []string{"loft.a", "loft.b", "loft.c", "loft.d", "loft.e", "loft.f"} {
		vs = append(vs, g.give(loft, ws, k, entitle.On))
	}
	soon := g.give(loft, levyantest.Tenant(), "loft.g", entitle.On, func(v *grants.Give) { v.Until = now.Add(time.Hour) })
	vs = append(vs, soon)

	got, pages := g.list(t, key, nil, 3)
	x.Equal(idsOf(vs...), ids(got))
	x.Equal(3, pages)
	got, _ = g.list(t, key, ws.Bytes(), 0)
	x.Len(got, 6)

	g.w.At(now.Add(2 * time.Hour))
	got, _ = g.list(t, key, nil, 0)
	x.Equal(idsOf(vs...), ids(got), "ended an hour ago, and listed")

	g.w.At(now.Add(time.Hour + grants.Listed + time.Minute))
	_, err := g.w.S.Grants.Retire(context.Background())
	x.NoError(err)
	got, _ = g.list(t, key, nil, 0)
	x.Equal(idsOf(vs[:6]...), ids(got), "over for longer, and erased out of the list")
}

// A stream that ended missed what changed while it was closed, and is put
// right by the list read after the next one is ready: nothing is replayed,
// and nothing needs to be.
func TestAStreamOpenedAgainAndAListConverge(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft := g.w.Product("loft")
	key, _ := g.w.Key(loft, "loft-prod", nil)
	ws := levyantest.Tenant()

	held := map[string]*pdpb.EntitlementGrant{}
	hold := func(m *pdpb.EntitlementGrant) {
		k := pdid.Id(m.GetId()).String()
		if was, ok := held[k]; !ok || was.GetVersion() < m.GetVersion() {
			held[k] = m
		}
	}

	s, cancel := g.watching(t, key)
	kept := g.give(loft, ws, "loft.export", entitle.On)
	ended := g.give(loft, ws, "loft.seats", entitle.Limited(3))
	hold(next(t, s))
	hold(next(t, s))
	cancel()

	// While nothing is open.
	g.end(ended)
	given := g.give(loft, ws, "loft.robots", entitle.Unlimited)
	g.w.At(time.Now().Add(4 * 24 * time.Hour))
	_, err := g.w.S.Grants.Renew(context.Background())
	x.NoError(err)

	s, _ = g.watching(t, key)
	vs, _ := g.list(t, key, nil, 0)
	for _, v := range vs {
		hold(v)
	}

	x.Len(held, 3)
	for _, v := range []*levyan.Grant{kept, ended, given} {
		r, err := g.w.S.Ent.Grant.Get(context.Background(), pdid.Id(v.GetId()).Uuid())
		x.NoError(err)
		m := held[pdid.Id(v.GetId()).String()]
		x.Equal(r.Version, m.GetVersion(), "%s as levyan holds it", r.Key)
		x.Equal(r.ValidUntil.UTC(), m.GetValidUntil().AsTime())
	}

	// And the stream opened again carries what changes from then on -- after
	// what changed before it opened and was published after, which is state
	// too and changes nothing it holds.
	g.end(kept)
	for {
		m := next(t, s)
		if string(m.GetId()) != string(kept.GetId()) {
			was := held[pdid.Id(m.GetId()).String()]
			x.LessOrEqual(m.GetVersion(), was.GetVersion(), "nothing it has not heard of")
			continue
		}
		if m.GetVersion() > held[pdid.Id(kept.GetId()).String()].GetVersion() {
			break
		}
	}
}

// A consumer that cannot keep up is cut off rather than buffered for: its
// stream ends, ResourceExhausted, and every write goes on unhindered. It
// opens another and compares, as a follower does after any stream that
// ended.
func TestASlowConsumerIsCutOff(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft := g.w.Product("loft")
	key, _ := g.w.Key(loft, "loft-prod", nil)
	v := g.give(loft, levyantest.Tenant(), "loft.export", entitle.On)
	k, err := pdid.From(v.GetId())
	x.NoError(err)

	s, _ := g.watching(t, key)

	// Changes come faster than one stream reads and sends them: it falls
	// further behind than the broker keeps for it, and is cut off.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e := watch.Event{Changes: []watch.Change{{Method: levyan.GrantService_Patch_FullMethodName, By: levyan.GrantService_Patch_FullMethodName, Key: k}}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			for range 1000 {
				g.w.S.Watch.Broker().Publish(context.Background(), e)
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := s.Recv(); err != nil {
				ended <- err
				return
			}
		}
	}()
	select {
	case err := <-ended:
		x.Equal(codes.ResourceExhausted, status.Code(err))
	case <-time.After(10 * time.Second):
		x.Fail("a stream that fell behind went on")
	}

	// And another is opened, and is ready.
	g.watching(t, key)
}

// payday's own follower, against levyan, keeps a copy that is what levyan
// grants: what was given before it began, what is given and ended and
// renewed while it follows, and after its stream is cut, what changed in the
// meantime. Of another product's grants it is given nothing.
func TestAFollowerKeepsACopyOfWhatLevyanGrants(t *testing.T) {
	g := granted(t)
	x := pdtest.NewX(t)

	loft, other := g.w.Product("loft"), g.w.Product("other")
	key, _ := g.w.Key(loft, "loft-prod", nil)
	a, b := levyantest.Tenant(), levyantest.Tenant()
	cp := newCopy(a, b)

	before := g.give(loft, a, "loft.export", entitle.On)
	theirs := g.give(other, a, "other.export", entitle.On)

	f := entitle.Follower{Source: pdpb.NewGrantServiceClient(g.conn), Copy: cp, Every: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(levyantest.As(context.Background(), key))
	done := make(chan error, 1)
	go func() { done <- f.Spin(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	holds := func(v *levyan.Grant) func() bool {
		return func() bool {
			r, err := g.w.S.Ent.Grant.Get(context.Background(), pdid.Id(v.GetId()).Uuid())
			if err != nil {
				return false
			}
			ws, err := pdid.From(r.QueryWorkspace().OnlyX(context.Background()).Key)
			if err != nil {
				return false
			}
			c, ok := cp.of(ws)[string(v.GetId())]
			return ok && c.Version == r.Version && c.Until.Equal(r.ValidUntil.UTC()) && c.Key == r.Key
		}
	}
	x.Eventually(holds(before), 5*time.Second, 10*time.Millisecond, "what was given before it followed")

	seats := g.give(loft, b, "loft.seats", entitle.Limited(3))
	x.Eventually(holds(seats), 5*time.Second, 10*time.Millisecond, "what is given while it follows")
	c := cp.of(b)[string(seats.GetId())]
	x.Equal(entitle.Limited(3), c.Value)
	x.Equal(entitle.Limited(3), entitle.Decide("loft.seats", []entitle.Row{c}, time.Now()).Value)

	ended := g.end(before)
	x.Eventually(holds(ended), 5*time.Second, 10*time.Millisecond, "what is ended")
	x.False(entitle.Decide("loft.export", rows(cp.of(a)), time.Now()).Value.Has())

	// The stream cut, and something given at once: the copy is put right
	// whether the next stream carries it or the comparison after it does.
	k, err := pdid.From(seats.GetId())
	x.NoError(err)
	e := watch.Event{Changes: []watch.Change{{Method: levyan.GrantService_Patch_FullMethodName, By: levyan.GrantService_Patch_FullMethodName, Key: k}}}
	for range 100_000 {
		g.w.S.Watch.Broker().Publish(context.Background(), e)
	}
	robots := g.give(loft, a, "loft.robots", entitle.Unlimited)
	x.Eventually(holds(robots), 10*time.Second, 10*time.Millisecond, "what changed as the stream was cut")

	_, mine := cp.of(a)[string(theirs.GetId())]
	x.False(mine, "another product's grant to a workspace of the same identifier is nothing here")
}

func rows(m map[string]entitle.Row) []entitle.Row {
	out := []entitle.Row{}
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// memCopy is an `entitle.Copy` in memory, of the tenants it is made with: an
// app's copy without the app, one transaction at a time.
type memCopy struct {
	mu      sync.Mutex
	rows    map[pdid.Id]entitle.Row
	tenants map[pdid.Id]bool
}

func newCopy(tenants ...pdid.Id) *memCopy {
	c := &memCopy{rows: map[pdid.Id]entitle.Row{}, tenants: map[pdid.Id]bool{}}
	for _, t := range tenants {
		c.tenants[t] = true
	}
	return c
}

func (c *memCopy) In(ctx context.Context, tenant pdid.Id, f func(ctx context.Context, s entitle.Store) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := &memStore{c: c, rows: map[pdid.Id]entitle.Row{}}
	for k, v := range c.rows {
		s.rows[k] = v
	}
	ctx = frame.Into(ctx, frame.New(pdid.Nil, tenant, frame.Grant{}).WithScope(frame.Only(tenant)))
	if err := f(ctx, s); err != nil {
		return err
	}
	c.rows = s.rows

	return nil
}

func (c *memCopy) Tenants(_ context.Context, after pdid.Id, limit int) ([]pdid.Id, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[pdid.Id]bool{}
	for _, v := range c.rows {
		seen[v.Tenant] = true
	}
	vs := []pdid.Id{}
	for t := range seen {
		if t.Uuid().Compare(after.Uuid()) > 0 {
			vs = append(vs, t)
		}
	}
	slices.SortFunc(vs, func(a, b pdid.Id) int { return a.Uuid().Compare(b.Uuid()) })

	return vs[:min(len(vs), limit)], nil
}

// of is the rows `tenant` holds, by the grant each copies.
func (c *memCopy) of(tenant pdid.Id) map[string]entitle.Row {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := map[string]entitle.Row{}
	for _, v := range c.rows {
		if v.Tenant == tenant {
			out[string(v.Grant)] = v
		}
	}
	return out
}

type memStore struct {
	c    *memCopy
	rows map[pdid.Id]entitle.Row
}

// Driver is no database: a follower reads and writes through the rest.
func (s *memStore) Driver() dialect.Driver { return nil }

func (s *memStore) Rows(_ context.Context, tenant pdid.Id, key string, _ bool) ([]entitle.Row, error) {
	vs := []entitle.Row{}
	for _, v := range s.rows {
		if v.Tenant == tenant && (key == "" || v.Key == key) {
			vs = append(vs, v)
		}
	}
	slices.SortFunc(vs, func(a, b entitle.Row) int { return a.Id.Uuid().Compare(b.Id.Uuid()) })
	return vs, nil
}

func (s *memStore) Grant(_ context.Context, grant []byte) (entitle.Row, bool, error) {
	for _, v := range s.rows {
		if string(v.Grant) == string(grant) {
			return v, true, nil
		}
	}
	return entitle.Row{}, false, nil
}

func (s *memStore) Tenant(_ context.Context, tenant pdid.Id) (bool, error) {
	return s.c.tenants[tenant], nil
}

func (s *memStore) Put(_ context.Context, r entitle.Row) (pdid.Id, error) {
	if r.Id.IsZero() {
		for _, v := range s.rows {
			if string(v.Grant) == string(r.Grant) {
				return pdid.Nil, errors.New("unique: grant")
			}
		}
		r.Id = pdid.New(43)
	} else {
		was, ok := s.rows[r.Id]
		if !ok {
			return pdid.Nil, errors.New("no such row")
		}
		r.Tenant, r.Key, r.Grant = was.Tenant, was.Key, was.Grant
	}
	s.rows[r.Id] = r
	return r.Id, nil
}

func (s *memStore) Forget(_ context.Context, ids []pdid.Id) error {
	for _, id := range ids {
		delete(s.rows, id)
	}
	return nil
}
