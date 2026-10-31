package grants_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entaudit "github.com/lesomnus/levyan/internal/ent/audit"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/pd"
)

const (
	week = 7 * 24 * time.Hour
	day  = 24 * time.Hour
)

var t0 = time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)

// world is a levyan at t0, with a product and a workspace of it.
func world(t *testing.T) (*levyantest.World, *levyan.Tenant, pdid.Id) {
	t.Helper()
	w := levyantest.New(t)
	w.At(t0)

	return w, w.Product("loft"), levyantest.Tenant()
}

func give(w *levyantest.World, p *levyan.Tenant, ws pdid.Id, key string, v entitle.Value, with ...func(*grants.Give)) (*levyan.Grant, error) {
	g := grants.Give{
		Product:   pdid.Id(p.GetId()),
		Workspace: ws,
		Key:       key,
		Value:     v,
		Source:    levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}
	for _, f := range with {
		f(&g)
	}

	return w.S.Grants.Give(context.Background(), g)
}

func row(t *testing.T, w *levyantest.World, id []byte) *ent.Grant {
	t.Helper()
	k, err := pdid.From(id)
	w.X.NoError(err)
	v, err := w.S.Ent.Grant.Get(context.Background(), k.Uuid())
	w.X.NoError(err)
	return v
}

// A grant is given at version 1, and every change of it after is a greater
// one: renewed, then ended. Ended, it stays ended -- ending it again changes
// nothing, and no renewal moves it on. And its identifier is its own: a
// second grant of the same is another.
func TestEveryChangeOfAGrantIsAGreaterVersion(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	v, err := give(w, p, ws, "loft.export", entitle.On)
	x.NoError(err)
	x.Equal(uint64(1), v.GetVersion())
	x.True(v.GetRenews(), "a grant with no end of its own renews")
	x.Equal(t0, v.GetValidFrom().AsTime())
	x.Equal(t0.Add(week), v.GetValidUntil().AsTime(), "and reaches a horizon")

	// Half a horizon on, it is renewed: its end a horizon from then.
	w.At(t0.Add(week/2 + time.Hour))
	n, err := w.S.Grants.Renew(ctx)
	x.NoError(err)
	x.Equal(1, n)
	r := row(t, w, v.GetId())
	x.Equal(uint64(2), r.Version)
	x.Equal(t0.Add(week/2+time.Hour+week), r.ValidUntil.UTC())

	ended, err := w.S.Grants.End(ctx, pdid.Id(r.Id))
	x.NoError(err)
	x.Equal(uint64(3), ended.GetVersion())
	x.False(ended.GetRenews())
	x.Equal(t0.Add(week/2+time.Hour), ended.GetValidUntil().AsTime(), "ended now")

	again, err := w.S.Grants.End(ctx, pdid.Id(r.Id))
	x.NoError(err)
	x.Equal(uint64(3), again.GetVersion(), "ending what has ended changes nothing")

	w.At(t0.Add(3 * week))
	n, err = w.S.Grants.Renew(ctx)
	x.NoError(err)
	x.Zero(n, "an ended grant is not renewed")
	x.Equal(uint64(3), row(t, w, v.GetId()).Version)

	_, err = w.S.Ungated.Grant().Patch(ctx, levyan.GrantPatchRequest_builder{
		Ref:              levyan.GrantRef_builder{Id: v.GetId()}.Build(),
		ValidUntil:       timestamppb.New(t0.Add(4 * week)),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err), "and stays ended")

	other, err := give(w, p, ws, "loft.export", entitle.On)
	x.NoError(err)
	x.NotEqual(v.GetId(), other.GetId(), "a grant given again is another grant")
	x.Equal(uint64(1), other.GetVersion())
}

// The version is the server's to say: an Add is version 1 and a Patch the
// version after the row's, whatever the request says.
func TestTheVersionIsTheServers(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	v, err := give(w, p, ws, "loft.seats", entitle.Limited(5))
	x.NoError(err)
	r := row(t, w, v.GetId())

	added, err := w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant:     levyantest.Ref(p),
		Workspace:  levyan.WorkspaceRef_builder{Id: r.WorkspaceId[:]}.Build(),
		Key:        "loft.robots",
		Value:      levyan.GrantValue_GRANT_VALUE_UNLIMITED,
		ValidUntil: timestamppb.New(t0.Add(day)),
		Source:     levyan.GrantSource_GRANT_SOURCE_TRIAL,
		Version:    41,
	}.Build())
	x.NoError(err)
	x.Equal(uint64(1), added.GetVersion())

	patched, err := w.S.Ungated.Grant().Patch(ctx, levyan.GrantPatchRequest_builder{
		Ref:              levyan.GrantRef_builder{Id: added.GetId()}.Build(),
		ValidUntil:       timestamppb.New(t0.Add(2 * day)),
		Version:          proto.Uint64(1),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	x.NoError(err)
	x.Equal(uint64(2), patched.GetVersion())
}

// What a grant may be is held of every write of one: a key that is one, a
// value, an end after its beginning and after now, a beginning no earlier
// than now, a source -- and a rule named exactly when it is a rule's. A
// document says none of that, and is refused.
func TestAGrantIsHeldToWhatAGrantMayBe(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	_, err := give(w, p, ws, "export", entitle.On)
	x.Equal(codes.InvalidArgument, status.Code(err), "a key is the product's name and more")
	_, err = give(w, p, ws, "loft.export", entitle.Off)
	x.Equal(codes.InvalidArgument, status.Code(err), "a grant gives something; what is taken away is ended")
	_, err = give(w, p, ws, "loft.export", entitle.None)
	x.ErrCode(codes.InvalidArgument, err)
	_, err = give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) { g.From = t0.Add(-time.Hour) })
	x.Equal(codes.InvalidArgument, status.Code(err), "a grant does not reach back")
	_, err = give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) { g.Until = t0.Add(-time.Hour) })
	x.Equal(codes.InvalidArgument, status.Code(err), "nor is given ended")
	_, err = give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) {
		g.From, g.Until = t0.Add(2*day), t0.Add(day)
	})
	x.Equal(codes.InvalidArgument, status.Code(err), "and ends after it begins")
	_, err = give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) { g.Source = levyan.GrantSource_GRANT_SOURCE_UNSPECIFIED })
	x.Equal(codes.InvalidArgument, status.Code(err), "a grant says where it is from")
	_, err = give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) { g.Rule = "payers.grace" })
	x.Equal(codes.InvalidArgument, status.Code(err), "only a rule's grant names a rule")
	_, err = give(w, p, pdid.New(pd.GrantDomain), "loft.export", entitle.On)
	x.Equal(codes.InvalidArgument, status.Code(err), "a workspace is a payday tenant")

	v, err := give(w, p, ws, "loft.export", entitle.On)
	x.NoError(err)
	r := row(t, w, v.GetId())
	ref := levyan.WorkspaceRef_builder{Id: r.WorkspaceId[:]}.Build()

	_, err = w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant: levyantest.Ref(p), Workspace: ref, Key: "loft.robots",
		Value: levyan.GrantValue_GRANT_VALUE_ON, Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "every grant says when it ends")

	_, err = w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant: levyantest.Ref(p), Workspace: ref, Key: "loft.robots",
		Value: levyan.GrantValue_GRANT_VALUE_ON, Limit: 3, ValidUntil: timestamppb.New(t0.Add(day)),
		Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "a switch is no amount")

	_, err = w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant: levyantest.Ref(p), Workspace: ref, Key: "loft.robots",
		Value: levyan.GrantValue_GRANT_VALUE_LIMIT, Limit: -1, ValidUntil: timestamppb.New(t0.Add(day)),
		Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "a limit is zero or more")

	_, err = w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant: levyantest.Ref(p), Workspace: ref, Key: "loft.robots",
		Value: levyan.GrantValue_GRANT_VALUE_ON, ValidUntil: timestamppb.New(t0.Add(day)),
		Source: levyan.GrantSource_GRANT_SOURCE_RULE,
	}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "a rule's grant names its rule")

	_, err = w.S.Ungated.Grant().Patch(ctx, levyan.GrantPatchRequest_builder{
		Ref:              levyan.GrantRef_builder{Id: v.GetId()}.Build(),
		ValidUntil:       timestamppb.New(t0.Add(-time.Hour)),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "a grant is ended now, never before")

	_, err = w.S.Ungated.Grant().Apply(ctx, levyan.GrantApplyRequest_builder{Ref: levyan.GrantRef_builder{Id: v.GetId()}.Build()}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err), "nor changed by a document")

	_, err = w.S.Ungated.Grant().Erase(ctx, levyan.GrantRef_builder{Id: v.GetId()}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err), "a grant in force is ended, not erased")

	// And a grant is of its product's workspace, which the sink holds it to.
	other := w.Product("other")
	_, err = w.S.Ungated.Grant().Add(ctx, levyan.GrantAddRequest_builder{
		Tenant: levyantest.Ref(other), Workspace: ref, Key: "other.robots",
		Value: levyan.GrantValue_GRANT_VALUE_ON, ValidUntil: timestamppb.New(t0.Add(day)),
		Source: levyan.GrantSource_GRANT_SOURCE_OPERATOR,
	}.Build())
	x.Error(err, "a grant of one product's to another's workspace")
}

// A grant that renews reaches a horizon from its last renewal, and is
// renewed once less than half of it is left -- not on every pass. One with an
// end of its own is never renewed, and one that lapsed while levyan was away
// is renewed when it is back.
func TestAGrantThatRenewsIsRenewedBeforeItEnds(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	open, err := give(w, p, ws, "loft.export", entitle.On)
	x.NoError(err)
	fixed, err := give(w, p, ws, "loft.seats", entitle.Limited(3), func(g *grants.Give) { g.Until = t0.Add(30 * day) })
	x.NoError(err)
	x.False(fixed.GetRenews())
	x.Equal(t0.Add(30*day), fixed.GetValidUntil().AsTime(), "an end of its own, past the horizon")
	later, err := give(w, p, ws, "loft.robots", entitle.Unlimited, func(g *grants.Give) { g.From = t0.Add(20 * day) })
	x.NoError(err)
	x.Equal(t0.Add(20*day+week), later.GetValidUntil().AsTime(), "one to come reaches a horizon from when it begins")

	w.At(t0.Add(week/2 - time.Hour))
	n, err := w.S.Grants.Renew(ctx)
	x.NoError(err)
	x.Zero(n, "more than half a horizon left")
	x.Equal(uint64(1), row(t, w, open.GetId()).Version)

	w.At(t0.Add(week/2 + time.Hour))
	n, err = w.S.Grants.Renew(ctx)
	x.NoError(err)
	x.Equal(1, n)
	x.Equal(t0.Add(week/2+time.Hour+week), row(t, w, open.GetId()).ValidUntil.UTC())
	x.Equal(uint64(1), row(t, w, fixed.GetId()).Version, "an end of its own is never moved")
	x.Equal(uint64(1), row(t, w, later.GetId()).Version)

	// levyan away for a month: it lapsed, and is renewed on the way back.
	back := t0.Add(40 * day)
	w.At(back)
	n, err = w.S.Grants.Renew(ctx)
	x.NoError(err)
	x.Equal(2, n, "what lapsed, and what was to come")
	x.Equal(back.Add(week), row(t, w, open.GetId()).ValidUntil.UTC())
	x.Equal(back.Add(week), row(t, w, later.GetId()).ValidUntil.UTC())
	x.Equal(uint64(1), row(t, w, fixed.GetId()).Version, "and what ended stays ended")
}

// A grant over for long enough is erased out of every list, and its row
// stays: two days after it ends, and not before.
func TestAGrantOverForLongEnoughIsRetired(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	v, err := give(w, p, ws, "loft.export", entitle.On, func(g *grants.Give) { g.Until = t0.Add(day) })
	x.NoError(err)

	w.At(t0.Add(day + grants.Listed - time.Minute))
	n, err := w.S.Grants.Retire(ctx)
	x.NoError(err)
	x.Zero(n, "ended, and still listed")

	w.At(t0.Add(day + grants.Listed + time.Minute))
	n, err = w.S.Grants.Retire(ctx)
	x.NoError(err)
	x.Equal(1, n)
	r := row(t, w, v.GetId())
	x.NotNil(r.DateErased, "the row stays")
	_, err = w.S.Ungated.Grant().Get(ctx, levyan.GrantGetRequest_builder{Ref: levyan.GrantRef_builder{Id: v.GetId()}.Build()}.Build())
	x.Equal(codes.NotFound, status.Code(err), "and is in no list")
}

// What an operator gave a workspace of a key, they give again only once it
// is ended, or by replacing it: the old one ends where the new one begins, so
// that one grant decides the key at a time. A rule's grant of the same key is
// the rule's, beside it.
func TestAnOperatorsGrantOfAKeyIsOneAtATime(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	first, err := give(w, p, ws, "loft.seats", entitle.Limited(3))
	x.NoError(err)
	_, err = give(w, p, ws, "loft.seats", entitle.Limited(5), func(g *grants.Give) { g.Source = levyan.GrantSource_GRANT_SOURCE_CONTRACT })
	x.Equal(codes.FailedPrecondition, status.Code(err), "the operator's grant of the key lasts")

	_, err = give(w, p, ws, "loft.export", entitle.On)
	x.NoError(err, "another key is another grant")
	_, err = give(w, p, levyantest.Tenant(), "loft.seats", entitle.Limited(5))
	x.NoError(err, "and another workspace")

	at := t0.Add(3 * day)
	second, err := give(w, p, ws, "loft.seats", entitle.Limited(5), func(g *grants.Give) {
		g.From, g.Replace, g.Source = at, true, levyan.GrantSource_GRANT_SOURCE_CONTRACT
	})
	x.NoError(err)
	r := row(t, w, first.GetId())
	x.Equal(at, r.ValidUntil.UTC(), "the first ends where the second begins")
	x.False(r.Renews)
	x.Equal(uint64(2), r.Version)
	x.Equal(at, second.GetValidFrom().AsTime())

	// One to come after the last ends is no overlap, and needs no replacing.
	fixed, err := give(w, p, ws, "loft.robots", entitle.Limited(1), func(g *grants.Give) { g.Until = t0.Add(10 * day) })
	x.NoError(err)
	_, err = give(w, p, ws, "loft.robots", entitle.Limited(2), func(g *grants.Give) { g.From = t0.Add(10 * day) })
	x.NoError(err, "it begins as the other ends")
	_, err = give(w, p, ws, "loft.robots", entitle.Limited(3), func(g *grants.Give) { g.From = t0.Add(9 * day) })
	x.Equal(codes.FailedPrecondition, status.Code(err), "it begins before the other ends")
	x.Equal(uint64(1), row(t, w, fixed.GetId()).Version)

	// A rule's is beside them, and the rule's own.
	w.S.Grants.Rules = []grants.Rule{wants{name: "test.restrict", of: map[uuid.UUID][]grants.Want{
		uuid.UUID(p.GetId()): {{Workspace: r.WorkspaceId, Key: "loft.seats", Value: entitle.Limited(1)}},
	}}}
	x.NoError(w.S.Grants.Follow(ctx))
	ruled, err := w.S.Ent.Grant.Query().Where(entgrant.Source(int32(levyan.GrantSource_GRANT_SOURCE_RULE))).Only(ctx)
	x.NoError(err)
	x.Equal("test.restrict", ruled.Rule)
	x.Equal(uint64(2), row(t, w, first.GetId()).Version, "the operator's grants are not the rule's to end")
	x.Equal(uint64(1), row(t, w, second.GetId()).Version)

	_, err = w.S.Grants.End(ctx, pdid.Id(ruled.Id))
	x.Equal(codes.FailedPrecondition, status.Code(err), "nor is the rule's an operator's to end")
}

// wants is a rule that wants what it is told, of each product.
type wants struct {
	name string
	of   map[uuid.UUID][]grants.Want
	err  error
}

func (r wants) Name() string { return r.name }

func (r wants) Want(_ context.Context, _ *ent.Client, product uuid.UUID, _ time.Time) ([]grants.Want, error) {
	return r.of[product], r.err
}

// The rules hook: what a rule wants is given, kept as it is for as long as it
// is wanted, given again when the value it wants changes, and ended when it is
// wanted no more. A rule that cannot say what it wants ends nothing.
func TestARulesGrantsAreWhatItWants(t *testing.T) {
	w, p, a := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	// Two workspaces levyan has heard of.
	_, err := give(w, p, a, "loft.export", entitle.On)
	x.NoError(err)
	b := levyantest.Tenant()
	_, err = give(w, p, b, "loft.export", entitle.On)
	x.NoError(err)
	wsOf := func(key pdid.Id) uuid.UUID {
		v, err := w.S.Ent.Workspace.Query().Where(entworkspace.Key(key.Bytes())).Only(ctx)
		x.NoError(err)
		return v.Id
	}

	product := uuid.UUID(p.GetId())
	r := wants{name: "test.restrict", of: map[uuid.UUID][]grants.Want{
		product: {{Workspace: wsOf(a), Key: "loft.restricted", Value: entitle.On, Desc: "no payer"}},
	}}
	w.S.Grants.Rules = []grants.Rule{r}

	ruled := func() []*ent.Grant {
		vs, err := w.S.Ent.Grant.Query().
			Where(entgrant.Rule("test.restrict")).
			Order(ent.Asc(entgrant.FieldDateCreated, entgrant.FieldId)).
			All(ctx)
		x.NoError(err)
		return vs
	}

	x.NoError(w.S.Grants.Pass(ctx))
	vs := ruled()
	x.Len(vs, 1)
	x.Equal(wsOf(a), vs[0].WorkspaceId)
	x.Equal(int32(levyan.GrantSource_GRANT_SOURCE_RULE), vs[0].Source)
	x.True(vs[0].Renews)
	x.Equal("no payer", vs[0].Desc)

	w.At(t0.Add(time.Hour))
	x.NoError(w.S.Grants.Pass(ctx))
	vs = ruled()
	x.Len(vs, 1, "the same again is the one there")
	x.Equal(uint64(1), vs[0].Version)

	// It cannot say: nothing of its is ended.
	w.S.Grants.Rules = []grants.Rule{wants{name: "test.restrict", err: errors.New("the ledger is away")}}
	x.Error(w.S.Grants.Pass(ctx))
	x.True(ruled()[0].Renews)

	// It wants b and not a.
	w.At(t0.Add(2 * time.Hour))
	r.of[product] = []grants.Want{{Workspace: wsOf(b), Key: "loft.restricted", Value: entitle.On}}
	w.S.Grants.Rules = []grants.Rule{r}
	x.NoError(w.S.Grants.Pass(ctx))
	vs = ruled()
	x.Len(vs, 2)
	x.Equal(wsOf(a), vs[0].WorkspaceId)
	x.False(vs[0].Renews, "a's is ended")
	x.Equal(t0.Add(2*time.Hour), vs[0].ValidUntil.UTC())
	x.Equal(uint64(2), vs[0].Version)
	x.Equal(wsOf(b), vs[1].WorkspaceId)
	x.True(vs[1].Renews)

	// Another value: given again, and the one before ended.
	w.At(t0.Add(3 * time.Hour))
	r.of[product] = []grants.Want{{Workspace: wsOf(b), Key: "loft.restricted", Value: entitle.Limited(0)}}
	x.NoError(w.S.Grants.Pass(ctx))
	vs = ruled()
	x.Len(vs, 3)
	x.False(vs[1].Renews)
	x.Equal(int32(levyan.GrantValue_GRANT_VALUE_LIMIT), vs[2].Value)

	// And nothing wanted at all: everything of its ended.
	r.of[product] = nil
	x.NoError(w.S.Grants.Pass(ctx))
	for _, v := range ruled() {
		x.False(v.Renews)
	}
}

// Changes of one grant made at once are made one after the other, each a
// greater version: what the trail says was written of each grant is one
// change for each version past the first. And an operator's grants of one
// key given at once still leave one of them in force.
func TestChangesAtOnceAreOneAfterTheOther(t *testing.T) {
	w, p, ws := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	first, err := give(w, p, ws, "loft.seats", entitle.Limited(3))
	x.NoError(err)

	w.At(t0.Add(week/2 + time.Hour))
	var g errgroup.Group
	for i := range 4 {
		g.Go(func() error {
			_, err := w.S.Grants.Renew(ctx)
			return err
		})
		g.Go(func() error {
			_, err := give(w, p, ws, "loft.seats", entitle.Limited(int64(10+i)), func(g *grants.Give) { g.Replace = true })
			return err
		})
	}
	g.Go(func() error {
		_, err := w.S.Grants.End(ctx, pdid.Id(first.GetId()))
		return err
	})
	x.NoError(g.Wait())

	vs, err := w.S.Ent.Grant.Query().Where(entgrant.Key("loft.seats")).All(ctx)
	x.NoError(err)
	x.Len(vs, 5)
	live := 0
	for _, v := range vs {
		n, err := w.S.Ent.Audit.Query().
			Where(entaudit.ObjectId(v.Id), entaudit.Action(levyan.GrantService_Patch_FullMethodName)).
			Count(ctx)
		x.NoError(err)
		x.Equal(uint64(1+n), v.Version, "grant %s: a version for every change", pdid.Id(v.Id))
		if v.ValidUntil.After(w.S.Clock()) {
			live++
		}
	}
	x.Equal(1, live, "one grant decides the key")
}

// A renewal and an end of one grant at once are two changes, of two versions,
// whichever comes first -- or one, when the end comes first and leaves
// nothing to renew.
func TestARenewalAndAnEndAtOnceAreTwoVersions(t *testing.T) {
	w, p, _ := world(t)
	x := pdtest.NewX(t)
	ctx := context.Background()

	for range 20 {
		w.At(t0)
		v, err := give(w, p, levyantest.Tenant(), "loft.export", entitle.On)
		x.NoError(err)

		w.At(t0.Add(week/2 + time.Hour))
		var g errgroup.Group
		g.Go(func() error {
			_, err := w.S.Grants.Renew(ctx)
			return err
		})
		g.Go(func() error {
			_, err := w.S.Grants.End(ctx, pdid.Id(v.GetId()))
			return err
		})
		x.NoError(g.Wait())

		r := row(t, w, v.GetId())
		n, err := w.S.Ent.Audit.Query().
			Where(entaudit.ObjectId(r.Id), entaudit.Action(levyan.GrantService_Patch_FullMethodName)).
			Count(ctx)
		x.NoError(err)
		x.Equal(uint64(1+n), r.Version, "a version for every change")
		x.False(r.Renews, "and it is ended")
	}
}
