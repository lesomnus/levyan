// Package ledger is the meter ledger: what a product sends of its workspaces
// -- a stock's readings ([Ledger.Record]), a flow's points ([Ledger.Count]) --
// kept as rows that are never changed, so that a period billed from them is
// billed from them again, the same, when somebody asks why.
//
// # What it keeps
//
// One reading of each workspace, meter and moment, and one point of each
// series, start and moment. What is sent again is what was kept: a retry of a
// call that did arrive, or two processes around a failover, counts once. The
// same moment with another value is refused and said in the log, rather than
// either value chosen -- a reading is stamped once, so two values for it are a
// fault somewhere, and which of them was true is not this package's to guess.
// A point that adds nothing -- it counts no more than one before it under the
// same start -- is not written at all ([Nothing]), so a series idle for a day
// is one row.
//
// # How it writes
//
// Through the servers, never past them: the stack it is handed, as whoever is
// calling -- the walled one, as the product, for what a product sends -- so
// the trail says which key sent what, and the price book's rules and the wall
// are asked of every row. What it reads to decide is only ever of the product
// the call is framed in, never one a request names; and whether a row is
// there already, and which periods are closed, it reads on the transaction it
// writes in.
//
// Every write for a workspace holds the workspace's row until it commits, and
// so does the close of a period for it (#6). That is what makes "measured in
// a period that was closed already" a question with one answer: a close writes
// its period's row before it reads anything, so a reading is either in the
// statement it reads, or written after the row and marked [Result.Late] -- or
// both, which a correction finds to be nothing.
//
// # A workspace destroyed
//
// A workspace its product said was destroyed (`WorkspaceEvent`, #14) is
// measured no further: a reading or a point of a moment at its destruction
// or after is refused ([Destroyed]), under the same lock as the event is
// written, so that one of the two comes first. One kept before levyan was
// told is kept, and billed for nothing (`internal/rating`).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/ent/dialect/sql"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	entworkspaceevent "github.com/lesomnus/levyan/internal/ent/workspaceevent"
	"github.com/lesomnus/levyan/server/pd"
)

// Skew is how far ahead of this deployment's clock a moment may be said to
// have been: a reading measured on a clock a little fast is a reading, and one
// measured next week is not.
const Skew = 5 * time.Minute

// Most is how many readings or points one call may hand over.
const Most = 10_000

// Ledger keeps what products send.
type Ledger struct {
	// S is the stack a row is written through, as whoever is calling: the
	// walled one for a product's own calls.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time
}

// Outcome is what came of one reading or point.
type Outcome int

const (
	// Kept is a row written.
	Kept Outcome = iota + 1

	// Already is the same kept before: nothing written, and nothing wrong.
	Already

	// Nothing is a point that adds nothing to any period -- it counts no more
	// than a point before it under the same start -- so it is not written.
	Nothing

	// Refused is not kept, and [Result.Refusal] says why.
	Refused
)

func (o Outcome) String() string {
	switch o {
	case Kept:
		return "kept"
	case Already:
		return "already"
	case Nothing:
		return "nothing"
	case Refused:
		return "refused"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Refusal is why something was not kept.
type Refusal int

const (
	// Invalid is not one: no workspace, a workspace that is no payday
	// tenant's identifier, no moment or one in the future, a count below
	// nothing.
	Invalid Refusal = iota + 1

	// NoMeter is a key no meter of the product has.
	NoMeter

	// OtherKind is a meter of the other kind: a stock's readings are recorded,
	// and a flow is counted by its Sum.
	OtherKind

	// Unit is a unit that is not the meter's.
	Unit

	// Conflict is the same moment kept with another value.
	Conflict

	// Gone is of a moment at which its workspace had been destroyed, as its
	// product said (#14): nothing of it after that is billed, or kept.
	Gone
)

func (r Refusal) String() string {
	switch r {
	case Invalid:
		return "invalid"
	case NoMeter:
		return "no meter"
	case OtherKind:
		return "other kind"
	case Unit:
		return "unit"
	case Conflict:
		return "conflict"
	case Gone:
		return "destroyed"
	}
	return fmt.Sprintf("Refusal(%d)", int(r))
}

// Result is what came of one reading or point.
type Result struct {
	Outcome Outcome
	Refusal Refusal

	// Why is a refusal in a sentence, for whoever sent it.
	Why string

	// Late is a row kept that speaks for a period of the product's already
	// closed: what that period was billed is corrected on a later statement.
	Late bool
}

func refused(r Refusal, format string, args ...any) Result {
	return Result{Outcome: Refused, Refusal: r, Why: fmt.Sprintf(format, args...)}
}

// Workspace reads a workspace as a product names one: the canonical text of
// the identifier of its payday tenant.
func Workspace(text string) (pdid.Id, error) {
	if text == "" {
		return pdid.Nil, errors.New("no workspace: the canonical text of its payday tenant's identifier")
	}
	id, err := pdid.Parse(text)
	if err != nil {
		return pdid.Nil, fmt.Errorf("workspace %q is not a payday identifier", text)
	}
	if id.Domain() != pd.TenantDomain {
		return pdid.Nil, fmt.Errorf("workspace %q names something other than a payday tenant", text)
	}

	return id, nil
}

// Moment is a time as the ledger keeps one: in UTC, to the microsecond, which
// is what PostgreSQL keeps of a time.
func Moment(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func (l Ledger) now() time.Time {
	if l.Now == nil {
		return Moment(time.Now())
	}

	return Moment(l.Now())
}

func (l Ledger) postgres() bool { return l.Dialect == config.DialectPostgres }

// moment refuses what is no moment a row may say: none at all, and one ahead
// of the clock by more than [Skew].
func (l Ledger) moment(t time.Time, now time.Time, what string) (time.Time, *Result) {
	if t.IsZero() || t.Unix() <= 0 {
		r := refused(Invalid, "%s says no moment", what)
		return time.Time{}, &r
	}
	t = Moment(t)
	if t.After(now.Add(Skew)) {
		r := refused(Invalid, "%s is %s, which is ahead of now", what, t.Format(time.RFC3339Nano))
		return time.Time{}, &r
	}

	return t, nil
}

// meters is the product's meters by their keys.
func meters(ctx context.Context, db *ent.Client, product pdid.Id) (map[string]*ent.Meter, error) {
	vs, err := db.Meter.Query().Where(entmeter.TenantId(product.Uuid())).All(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]*ent.Meter, len(vs))
	for _, v := range vs {
		out[v.Key] = v
	}

	return out, nil
}

// workspaces is the row of each of the product's workspaces named by `keys`,
// written through the stack for those it has none of yet. Each is written on
// its own, before the transaction that writes the ledger: two calls that
// both find a workspace new are both answered with the one row.
func (l Ledger) workspaces(ctx context.Context, db *ent.Client, product pdid.Id, keys []pdid.Id) (map[pdid.Id]uuid.UUID, error) {
	bs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		bs = append(bs, k.Bytes())
	}

	read := func() (map[pdid.Id]uuid.UUID, error) {
		vs, err := db.Workspace.Query().
			Where(entworkspace.TenantId(product.Uuid()), entworkspace.KeyIn(bs...)).
			All(ctx)
		if err != nil {
			return nil, err
		}

		out := map[pdid.Id]uuid.UUID{}
		for _, v := range vs {
			k, err := pdid.From(v.Key)
			if err != nil {
				return nil, err
			}
			out[k] = v.Id
		}

		return out, nil
	}

	have, err := read()
	if err != nil {
		return nil, err
	}

	made := false
	for _, k := range keys {
		if _, ok := have[k]; ok {
			continue
		}
		_, err := l.S.Workspace().Add(ctx, levyan.WorkspaceAddRequest_builder{
			Tenant: levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
			Key:    k.Bytes(),
		}.Build())
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return nil, err
		}
		made = true
	}
	if !made {
		return have, nil
	}

	return read()
}

// Known is the row of `product`'s workspace `key`, written through `s` when
// levyan has none of it yet -- what an operator names a workspace by before
// the product has said anything of it, as a grant does (`server/grants`).
// Written on its own, before the transaction that uses it: two that both
// find it new are both answered with the one row. A product levyan does not
// have is NotFound.
func Known(ctx context.Context, s levyan.Server, db *ent.Client, product, key pdid.Id) (uuid.UUID, error) {
	if key.IsZero() || key.Domain() != pd.TenantDomain {
		return uuid.UUID{}, status.Errorf(codes.InvalidArgument, "workspace: %s is not the identifier of a payday tenant", key)
	}
	get := func() (uuid.UUID, error) {
		return db.Workspace.Query().Where(entworkspace.TenantId(product.Uuid()), entworkspace.Key(key.Bytes())).OnlyId(ctx)
	}

	id, err := get()
	if err == nil || !ent.IsNotFound(err) {
		return id, err
	}
	if ok, err := db.Tenant.Query().Where(enttenant.Id(product.Uuid())).Exist(ctx); err != nil || !ok {
		if err == nil {
			err = status.Errorf(codes.NotFound, "no product is %s", product)
		}
		return uuid.UUID{}, err
	}
	_, err = s.Workspace().Add(ctx, levyan.WorkspaceAddRequest_builder{
		Tenant: levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
		Key:    key.Bytes(),
	}.Build())
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return uuid.UUID{}, err
	}

	return get()
}

// tx runs `f` in one transaction: the stack and an ent client on it.
func (l Ledger) tx(ctx context.Context, f func(s levyan.Server, db *ent.Client) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, l.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(l.S, tdrv)
	if err != nil {
		return err
	}
	if err := f(s, ent.NewClient(ent.Driver(tdrv))); err != nil {
		return err
	}

	return tx.Commit()
}

// Lock holds the rows of `ids` -- workspaces -- until the transaction `db` is
// on ends, in the order of their identifiers, so that two transactions that
// both hold several never wait on each other. SQLite runs one writer at a
// time, which holds the whole database from the first statement on
// (`_txlock=immediate`), so there it asks nothing.
func Lock(ctx context.Context, db *ent.Client, postgres bool, ids ...uuid.UUID) error {
	if !postgres || len(ids) == 0 {
		return nil
	}

	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)

	q := db.Workspace.Query().Where(entworkspace.IdIn(ids...)).Order(ent.Asc(entworkspace.FieldId))
	q.Modify(func(s *sql.Selector) { s.ForUpdate() })
	_, err := q.Ids(ctx)

	return err
}

// Closed is the product's periods that a close has begun on and that
// [from, to) reaches into: what is measured there, or speaks for some of
// there, is late.
func Closed(ctx context.Context, db *ent.Client, product pdid.Id, from, to time.Time) ([]*ent.Period, error) {
	return db.Period.Query().
		Where(
			entperiod.TenantId(product.Uuid()),
			entperiod.DateStartLT(to),
			entperiod.DateEndGT(from),
		).
		Order(ent.Asc(entperiod.FieldDateStart)).
		All(ctx)
}

// Destroyed is when each of `ws`, workspaces of `product`, was destroyed, as
// the product said (#14): the first destruction said of each, which is for
// good. A workspace not destroyed is not in it.
func Destroyed(ctx context.Context, db *ent.Client, product uuid.UUID, ws ...uuid.UUID) (map[uuid.UUID]time.Time, error) {
	out := map[uuid.UUID]time.Time{}
	if len(ws) == 0 {
		return out, nil
	}

	vs, err := db.WorkspaceEvent.Query().
		Where(
			entworkspaceevent.TenantId(product),
			entworkspaceevent.WorkspaceIdIn(ws...),
			entworkspaceevent.Kind(int32(levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		at := Moment(v.HappenedAt)
		if was, ok := out[v.WorkspaceId]; !ok || at.Before(was) {
			out[v.WorkspaceId] = at
		}
	}

	return out, nil
}

// gone is the refusal of what `what` says of workspace `ws` at a moment it
// had been destroyed by, at `at`.
func gone(what string, ws pdid.Id, at time.Time) Result {
	return refused(Gone, "%s: workspace %s was destroyed at %s, and nothing of it from then is kept", what, ws, at.Format(time.RFC3339Nano))
}

// reaches says whether [from, to) reaches into one of `ps`.
func reaches(ps []*ent.Period, from, to time.Time) bool {
	for _, p := range ps {
		if p.DateStart.Before(to) && p.DateEnd.After(from) {
			return true
		}
	}

	return false
}

// in says whether `at` is in one of `ps`.
func in(ps []*ent.Period, at time.Time) bool {
	return reaches(ps, at, at.Add(time.Microsecond))
}

// conflict says a moment sent with a value other than the one kept, in the
// log -- which an operator has to look into, since one of the two is wrong --
// and to whoever sent it.
func conflict(ctx context.Context, what string, product pdid.Id, workspace pdid.Id, meter string, at time.Time, kept, sent int64) Result {
	log.From(ctx).WarnContext(ctx, "ledger: refused a "+what+" that says another value for a moment already kept",
		slog.String("product", product.String()),
		slog.String("workspace", workspace.String()),
		slog.String("meter", meter),
		slog.Time("at", at),
		slog.Int64("kept", kept),
		slog.Int64("sent", sent),
	)

	return refused(Conflict, "%s at %s was kept as %d, and is not %d", meter, at.Format(time.RFC3339Nano), kept, sent)
}

func stamp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// instant is a time as an aggregate of a time column answers with: a time on
// PostgreSQL, and on SQLite the number it is kept as, nanoseconds since the
// epoch (`_timefmt=unixepoch_nano`), since a column of an aggregate has no
// declared type for the driver to read it by.
type instant time.Time

func (v *instant) Scan(src any) error {
	switch u := src.(type) {
	case nil:
		*v = instant{}
	case time.Time:
		*v = instant(u)
	case int64:
		*v = instant(time.Unix(0, u))
	case float64:
		*v = instant(time.Unix(0, int64(u)))
	case []byte:
		return v.Scan(string(u))
	case string:
		t, err := time.Parse(time.RFC3339Nano, u)
		if err != nil {
			return fmt.Errorf("ledger: %q is not a moment: %w", u, err)
		}
		*v = instant(t)
	default:
		return fmt.Errorf("ledger: %T is not a moment", src)
	}

	return nil
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func bytesOf(id uuid.UUID) []byte { return id[:] }

// key is an identifier as a column of an aggregate answers with it, which the
// generated scans read for themselves and a struct of an aggregate's does not:
// the standard library's uuid is no `sql.Scanner`.
type key uuid.UUID

func (v *key) Scan(src any) error {
	switch u := src.(type) {
	case [16]byte:
		*v = key(u)
	case []byte:
		if len(u) == 16 {
			*v = key(uuid.UUID(u))
			return nil
		}
		return v.Scan(string(u))
	case string:
		id, err := uuid.Parse(u)
		if err != nil {
			return fmt.Errorf("ledger: %q is not an identifier: %w", u, err)
		}
		*v = key(id)
	default:
		return fmt.Errorf("ledger: %T is not an identifier", src)
	}

	return nil
}
