// Package grants is what levyan gives a product's workspaces: grants -- a key,
// a value and a window -- which the product keeps a copy of in its own
// database and checks in the transaction of the write each decides (#7,
// payday#185). This is the source's half: giving a grant, ending one, renewing
// the ones that renew, and the rules that give and end grants by what levyan
// knows of a workspace's billing. What the product is told of them is payday's
// `GrantService`, served in `server/api`.
//
// # What a grant is
//
// State, and never a delta. A grant gives one value of one key to one
// workspace from when to when, and every change to it -- its end moved -- is
// the whole of it again at a greater version (`Grant.version`, which the
// [Layer] says and no request does). It is taken away by ending it: its end
// moved to now. Its identifier is never another's, since the row is never
// deleted: a grant over for [Listed] is erased, softly, out of every list.
//
// # Every grant ends
//
// A grant given with no end of its own renews: its end is the horizon from
// now (`grants.horizon`), and the loop [RenewLoop] moves it on while the grant
// lasts. So a product that stops hearing from levyan keeps what it was given
// for a horizon and no longer. A grant given with an end of its own -- an
// operator's `--until` -- ends then, and is never renewed.
//
// # One grant decides a key
//
// A grant is the whole of what it gives while it decides its key: a limit is a
// total and never an amount to add to another grant's. Of one giver, a key of
// a workspace has one grant at a time: an operator's second grant of a key is
// refused while the first lasts, or ends it where the new one begins when
// asked to replace it ([Give.Replace]); a rule's is the one it gave, ended and
// given again only when the value it wants changes. Where two givers' grants
// of one key are in force at once, the one that began last decides, as
// payday's copy decides it (`entitle.Decide`).
//
// # Positive grants only
//
// What a grant gives is something: a feature, a number. The copy reads a key
// no grant gives as not entitled, so a gap in levyan -- a grant not renewed in
// time, a stream that never came back -- takes away exactly what grants give,
// and nothing else. A restriction is therefore a grant that restricts when it
// is there (`loft.restricted` ON), and an app never holds its core writes to a
// grant's presence: a gap in levyan would then stop every workspace at once.
//
// # Written through the servers
//
// Every write goes through the stack it is handed (`cmd.Server.Ungated`), as
// whoever the context is framed as -- an operator, by their row; the
// deployment's own loop, as nobody -- so the trail says who gave a grant and
// who ended it. And each is in the outbox in its transaction (`cmd.Build`), so
// that what the product is streamed is exactly what was committed.
package grants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/ent/dialect/sql"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

const (
	// RenewLoop is the name of the loop that keeps grants: what the rules
	// want given and ended, the grants that renew renewed, and the ones over
	// for long enough erased.
	RenewLoop = "levyan.grants.renew"

	// Listed is how long a grant that has ended is still listed to its
	// product, after which it is erased out of every list: a day, which
	// payday asks of a source at least -- so that a comparison begun before
	// it ended never finds it gone -- and a day more for a clock that is
	// behind (implementation decision).
	Listed = 48 * time.Hour

	// DefaultHorizon is how far a grant that renews reaches, when
	// `grants.horizon` says nothing: a week (Claude's, the lead's, #7).
	DefaultHorizon = 7 * 24 * time.Hour
)

// Moment is a time as a grant keeps one: in UTC, to the microsecond, which is
// what PostgreSQL keeps of a time.
func Moment(t time.Time) time.Time { return ledger.Moment(t) }

// Grants gives, ends and keeps grants, through S.
type Grants struct {
	// S is the stack every grant is written through, as whoever the context
	// is framed as: the deployment's own (`cmd.Server.Ungated`).
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time

	// Horizon is how far a grant that renews reaches from its last renewal:
	// [DefaultHorizon] when zero.
	Horizon time.Duration

	// Rules give and end grants by what levyan knows of a workspace's
	// billing ([Rule]), each pass of the loop.
	Rules []Rule
}

func (g Grants) now() time.Time {
	if g.Now == nil {
		return Moment(time.Now())
	}

	return Moment(g.Now())
}

func (g Grants) horizon() time.Duration {
	if g.Horizon <= 0 {
		return DefaultHorizon
	}

	return g.Horizon
}

func (g Grants) postgres() bool { return g.Dialect == config.DialectPostgres }

// Give is a grant to give.
type Give struct {
	// Product is the product whose workspace it is given to, and Workspace
	// the workspace as the product knows it: the identifier of its payday
	// tenant.
	Product   pdid.Id
	Workspace pdid.Id

	// Key and Value are what it gives: `loft.export` on, `loft.seats` 10.
	// A value is on, a limit or unlimited; off and none are refused, since a
	// grant that gives nothing is a grant ended.
	Key   string
	Value entitle.Value

	// From is when it begins, and zero is now. Until is when it ends, and
	// zero is a grant that renews: its end is the horizon from now, moved on
	// while it lasts.
	From  time.Time
	Until time.Time

	// Source is where it comes from, and Rule which rule for one of a rule.
	Source levyan.GrantSource
	Rule   string

	// Desc is why it is given, as whoever gives it says.
	Desc string

	// Replace ends the grant of the key the same giver has given the
	// workspace, where this one begins, rather than refusing this one while
	// that one lasts.
	Replace bool
}

// operators are the sources of a grant given by hand, which are one giver:
// what an operator gives a workspace of a key replaces what they gave it
// before, whichever of the three each says.
var operators = []int32{
	int32(levyan.GrantSource_GRANT_SOURCE_OPERATOR),
	int32(levyan.GrantSource_GRANT_SOURCE_CONTRACT),
	int32(levyan.GrantSource_GRANT_SOURCE_TRIAL),
}

// ByHand is whether a grant of `source` is given by an operator's hand.
func ByHand(source levyan.GrantSource) bool {
	for _, v := range operators {
		if int32(source) == v {
			return true
		}
	}

	return false
}

// Give gives a grant, through the stack, as whoever ctx is framed as, and
// answers with it: the workspace's row is written first if levyan has heard
// nothing of it yet. See [Give] for what an operator's grant of a key they
// have given before does, and [Rule] for what a rule's does.
func (g Grants) Give(ctx context.Context, v Give) (*levyan.Grant, error) {
	ws, err := g.workspace(ctx, v.Product, v.Workspace)
	if err != nil {
		return nil, err
	}

	return g.give(ctx, v, ws)
}

func (g Grants) give(ctx context.Context, v Give, ws uuid.UUID) (*levyan.Grant, error) {
	value, limit, err := fieldsOf(v.Value)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "value: %s", err)
	}

	var out *levyan.Grant
	err = g.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		// The workspace's row, so that two grants of one key to it are given
		// one after the other: each finds what the other gave.
		if err := ledger.Lock(ctx, db, g.postgres(), ws); err != nil {
			return err
		}

		// Now, once the row is held: what a grant given after a wait for it
		// begins at.
		now := g.now()
		from := now
		if !v.From.IsZero() {
			from = Moment(v.From)
		}
		until, renews := Moment(v.Until), false
		if v.Until.IsZero() {
			until, renews = latest(from, now).Add(g.horizon()), true
		}

		q := db.Grant.Query().Where(
			entgrant.WorkspaceId(ws),
			entgrant.Key(v.Key),
			entgrant.DateErasedIsNil(),
			entgrant.Or(entgrant.Renews(true), entgrant.ValidUntilGT(now)),
		)
		if v.Source == levyan.GrantSource_GRANT_SOURCE_RULE {
			q.Where(entgrant.Source(int32(v.Source)), entgrant.Rule(v.Rule))
		} else {
			q.Where(entgrant.SourceIn(operators...))
		}
		live, err := q.Order(ent.Asc(entgrant.FieldDateCreated, entgrant.FieldId)).All(ctx)
		if err != nil {
			return err
		}

		end := now
		if v.Source == levyan.GrantSource_GRANT_SOURCE_RULE {
			// A rule's grant of a key is the one it gave: the same again is
			// that one, and another value ends it.
			for _, l := range live {
				if l.Value == int32(value) && l.Limit == limit {
					out = l.Proto()
					return nil
				}
			}
		} else {
			// What the operator gave of the key that would be in force with
			// this one: it lasts, or will, into this one's window.
			overlaps := live[:0]
			for _, l := range live {
				if (l.Renews || Moment(l.ValidUntil).After(from)) && Moment(l.ValidFrom).Before(until) {
					overlaps = append(overlaps, l)
				}
			}
			if len(overlaps) > 0 && !v.Replace {
				l := overlaps[0]
				return status.Errorf(codes.FailedPrecondition,
					"%s of this workspace is given by grant %s until %s: end it, or replace it with this one",
					v.Key, pdid.Id(l.Id), l.ValidUntil.UTC().Format(time.RFC3339))
			}

			// Replaced where the new one begins, so that one ends as the
			// other begins and the key is never decided by both -- and one
			// that would only have begun later, taken back before it begins.
			live, end = overlaps, latest(from, now)
		}
		for _, l := range live {
			if _, err := g.end(ctx, s, db, l.Id, end); err != nil {
				return err
			}
		}

		out, err = s.Grant().Add(ctx, levyan.GrantAddRequest_builder{
			Tenant:     levyan.TenantRef_builder{Id: v.Product.Bytes()}.Build(),
			Workspace:  levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
			Desc:       v.Desc,
			Key:        v.Key,
			Value:      value,
			Limit:      limit,
			ValidFrom:  timestamppb.New(from),
			ValidUntil: timestamppb.New(until),
			Source:     v.Source,
			Rule:       v.Rule,
			Renews:     renews,
		}.Build())
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// End ends a grant now, through the stack, as whoever ctx is framed as, and
// answers with it: in force no longer, and never again. One that has ended
// already is answered as it is. A grant of a rule is the rule's to end -- it
// would give it again on its next pass -- and is refused.
func (g Grants) End(ctx context.Context, id pdid.Id) (*levyan.Grant, error) {
	var out *levyan.Grant
	err := g.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		if err := lock(ctx, db, g.postgres(), id.Uuid()); err != nil {
			return err
		}
		v, err := s.Grant().Get(ctx, levyan.GrantGetRequest_builder{
			Ref:    levyan.GrantRef_builder{Id: id.Bytes()}.Build(),
			Select: levyan.GrantSelect_builder{Source: proto.Bool(true)}.Build(),
		}.Build())
		if err != nil {
			return err
		}
		if v.GetSource() == levyan.GrantSource_GRANT_SOURCE_RULE {
			return status.Error(codes.FailedPrecondition, "a rule's grant is the rule's: it ends when what the rule follows changes")
		}

		out, err = g.end(ctx, s, db, id.Uuid(), g.now())
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// end ends grant `id` at `at` -- now, or later -- on the transaction `s` and
// `db` are on, its row held first: not renewed from then on. One that ends by
// then already is answered as it is.
func (g Grants) end(ctx context.Context, s levyan.Server, db *ent.Client, id uuid.UUID, at time.Time) (*levyan.Grant, error) {
	if err := lock(ctx, db, g.postgres(), id); err != nil {
		return nil, err
	}
	v, err := db.Grant.Query().Where(entgrant.Id(id), entgrant.DateErasedIsNil()).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no grant is %s", pdid.Id(id))
	}
	if err != nil {
		return nil, err
	}
	if !v.Renews && !Moment(v.ValidUntil).After(at) {
		return v.Proto(), nil
	}

	until := Moment(v.ValidUntil)
	if until.After(at) || v.Renews {
		until = at
	}

	return s.Grant().Patch(ctx, levyan.GrantPatchRequest_builder{
		Ref:              levyan.GrantRef_builder{Id: id[:]}.Build(),
		ValidUntil:       timestamppb.New(until),
		Renews:           proto.Bool(false),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
}

// Renew moves on the end of every grant that renews and has less than half
// its horizon left, to the horizon from now, and answers with how many: a
// copy that stops hearing from levyan holds every such grant for at least
// half a horizon, and a grant changes twice a horizon rather than every pass
// of the loop -- a lease renewed at half its time, as DHCP renews one
// (implementation decision). One that lapsed, levyan having been away for
// longer than it reached, is renewed when it can be.
func (g Grants) Renew(ctx context.Context) (int, error) {
	db := g.db()
	now := g.now()
	due := now.Add(g.horizon() / 2)

	var (
		n     int
		errs  []error
		after uuid.UUID
	)
	for {
		ids, err := db.Grant.Query().
			Where(entgrant.Renews(true), entgrant.ValidUntilLT(due), entgrant.DateErasedIsNil(), entgrant.IdGT(after)).
			Order(ent.Asc(entgrant.FieldId)).
			Limit(page).
			Ids(ctx)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, id := range ids {
			after = id
			ok, err := g.renew(ctx, id, now, due)
			if err != nil {
				errs = append(errs, fmt.Errorf("grant %s: %w", pdid.Id(id), err))
				continue
			}
			if ok {
				n++
			}
		}
		if len(ids) < page {
			break
		}
	}

	return n, errors.Join(errs...)
}

// page is how many rows a pass reads at a time.
const page = 200

// renew is one grant renewed, in a transaction that holds its row, if it
// still renews and is still due.
func (g Grants) renew(ctx context.Context, id uuid.UUID, now, due time.Time) (bool, error) {
	ok := false
	err := g.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		if err := lock(ctx, db, g.postgres(), id); err != nil {
			return err
		}
		v, err := db.Grant.Get(ctx, id)
		if err != nil {
			return err
		}
		if !v.Renews || v.DateErased != nil || !Moment(v.ValidUntil).Before(due) {
			return nil
		}

		_, err = s.Grant().Patch(ctx, levyan.GrantPatchRequest_builder{
			Ref:              levyan.GrantRef_builder{Id: id[:]}.Build(),
			ValidUntil:       timestamppb.New(latest(now, Moment(v.ValidFrom)).Add(g.horizon())),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
		ok = err == nil
		return err
	})

	return ok, err
}

// Retire erases every grant that has been over for [Listed], and answers with
// how many: it is listed to its product no more, and its row stays.
func (g Grants) Retire(ctx context.Context) (int, error) {
	db := g.db()
	before := g.now().Add(-Listed)

	var (
		n     int
		errs  []error
		after uuid.UUID
	)
	for {
		ids, err := db.Grant.Query().
			Where(entgrant.Renews(false), entgrant.ValidUntilLT(before), entgrant.DateErasedIsNil(), entgrant.IdGT(after)).
			Order(ent.Asc(entgrant.FieldId)).
			Limit(page).
			Ids(ctx)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, id := range ids {
			after = id
			v, err := g.S.Grant().Erase(ctx, levyan.GrantRef_builder{Id: id[:]}.Build())
			if err != nil {
				errs = append(errs, fmt.Errorf("grant %s: %w", pdid.Id(id), err))
				continue
			}
			if v.GetErased() {
				n++
			}
		}
		if len(ids) < page {
			break
		}
	}

	return n, errors.Join(errs...)
}

// Pass is one pass of the loop: what the rules want given, and what they no
// longer want ended; every grant that is due renewed; every grant over for
// [Listed] erased. Each grant is written in a transaction of its own, so one
// that fails is said in the answer and the others are written.
func (g Grants) Pass(ctx context.Context) error {
	var errs []error
	if err := g.Follow(ctx); err != nil {
		errs = append(errs, fmt.Errorf("rules: %w", err))
	}
	if _, err := g.Renew(ctx); err != nil {
		errs = append(errs, fmt.Errorf("renew: %w", err))
	}
	if _, err := g.Retire(ctx); err != nil {
		errs = append(errs, fmt.Errorf("retire: %w", err))
	}

	return errors.Join(errs...)
}

// Renewals is the loop [RenewLoop]: a [Grants.Pass] every `every`, of `g` as
// it is at each -- its rules included, which the wiring may add after the
// loop is made. A pass that fails is said in the log, and the next one tries
// again.
func Renewals(g *Grants, every time.Duration) spin.Spinner {
	return spin.Called(RenewLoop, spin.Every(every, func(ctx context.Context) error {
		if err := g.Pass(ctx); err != nil {
			log.From(ctx).WarnContext(ctx, "grants: a pass of the loop", slog.Any("err", err))
		}
		return nil
	}))
}

// workspace is the row of `key`, a workspace of `product`, written through the
// stack if levyan has heard nothing of it yet.
func (g Grants) workspace(ctx context.Context, product, key pdid.Id) (uuid.UUID, error) {
	if key.IsZero() || key.Domain() != pd.TenantDomain {
		return uuid.UUID{}, status.Errorf(codes.InvalidArgument, "workspace: %s is not the identifier of a payday tenant", key)
	}

	ref := levyan.WorkspaceRef_builder{Keyed: levyan.WorkspaceRefByKeyed_builder{
		Key:    key.Bytes(),
		Tenant: levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
	}.Build()}.Build()
	get := func() (uuid.UUID, error) {
		v, err := g.S.Workspace().Get(ctx, levyan.WorkspaceGetRequest_builder{Ref: ref}.Build())
		if err != nil {
			return uuid.UUID{}, err
		}
		id, err := pdid.From(v.GetId())
		return id.Uuid(), err
	}

	id, err := get()
	if status.Code(err) != codes.NotFound {
		return id, err
	}
	if _, err := g.db().Tenant.Query().Where(enttenant.Id(product.Uuid())).Only(ctx); err != nil {
		if ent.IsNotFound(err) {
			return uuid.UUID{}, status.Errorf(codes.NotFound, "no product is %s", product)
		}
		return uuid.UUID{}, err
	}
	_, err = g.S.Workspace().Add(ctx, levyan.WorkspaceAddRequest_builder{
		Tenant: levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
		Key:    key.Bytes(),
	}.Build())
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return uuid.UUID{}, err
	}

	return get()
}

func (g Grants) db() *ent.Client { return ent.NewClient(ent.Driver(g.Drv)) }

// tx runs `f` in one transaction: the stack and an ent client on it.
func (g Grants) tx(ctx context.Context, f func(s levyan.Server, db *ent.Client) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, g.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(g.S, tdrv)
	if err != nil {
		return err
	}
	if err := f(s, ent.NewClient(ent.Driver(tdrv))); err != nil {
		return err
	}

	return tx.Commit()
}

// lock holds the row of grant `id` until the transaction `db` is on ends, so
// that two changes of one grant are one after the other and each is a greater
// version than the last. SQLite runs one writer at a time, which holds the
// whole database from the first statement on, so there it asks nothing.
func lock(ctx context.Context, db *ent.Client, postgres bool, id uuid.UUID) error {
	if !postgres {
		return nil
	}

	q := db.Grant.Query().Where(entgrant.Id(id))
	q.Modify(func(s *sql.Selector) { s.ForUpdate() })
	_, err := q.Ids(ctx)

	return err
}

// Value is what a grant gives, as payday's `entitle` reads it.
func Value(v *levyan.Grant) entitle.Value {
	out, _ := valueOf(v.GetValue(), v.GetLimit())
	return out
}

// valueOf is a value as a row holds it, read back, and refuses one that is
// none.
func valueOf(v levyan.GrantValue, limit int64) (entitle.Value, error) {
	switch v {
	case levyan.GrantValue_GRANT_VALUE_ON:
		if limit != 0 {
			return entitle.None, errors.New("a feature switched on is no amount")
		}
		return entitle.On, nil
	case levyan.GrantValue_GRANT_VALUE_LIMIT:
		if limit < 0 {
			return entitle.None, fmt.Errorf("a limit is zero or more, and not %d", limit)
		}
		return entitle.Limited(limit), nil
	case levyan.GrantValue_GRANT_VALUE_UNLIMITED:
		if limit != 0 {
			return entitle.None, errors.New("no bound is no amount")
		}
		return entitle.Unlimited, nil
	}

	return entitle.None, errors.New("a grant gives a feature on, a limit, or no bound")
}

// fieldsOf is a value as a row holds it.
func fieldsOf(v entitle.Value) (levyan.GrantValue, int64, error) {
	switch v.Kind() {
	case entitle.KindOn:
		return levyan.GrantValue_GRANT_VALUE_ON, 0, nil
	case entitle.KindLimit:
		n, _ := v.Number()
		return levyan.GrantValue_GRANT_VALUE_LIMIT, n, nil
	case entitle.KindUnlimited:
		return levyan.GrantValue_GRANT_VALUE_UNLIMITED, 0, nil
	}

	return 0, 0, fmt.Errorf("a grant gives a feature on, a limit, or no bound, and not %s: what is taken away is ended", v)
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
