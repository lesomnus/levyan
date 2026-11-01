package billing

import (
	"context"
	"time"
	"uuid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entworkspaceevent "github.com/lesomnus/levyan/internal/ent/workspaceevent"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/server/ledger"
)

// Lives is what became of each of workspaces `ws` of `product`, as the
// product said (`WorkspaceEvent`, #14): when each was away -- closed and not
// restored, and from its destruction on -- and so no customer. One nothing
// was said of was never away, and is not in it.
func Lives(ctx context.Context, db *ent.Client, product uuid.UUID, ws ...uuid.UUID) (map[uuid.UUID]rating.Life, error) {
	out := map[uuid.UUID]rating.Life{}
	if len(ws) == 0 {
		return out, nil
	}

	vs, err := db.WorkspaceEvent.Query().
		Where(entworkspaceevent.TenantId(product), entworkspaceevent.WorkspaceIdIn(ws...)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	by := map[uuid.UUID][]rating.Event{}
	for _, v := range vs {
		by[v.WorkspaceId] = append(by[v.WorkspaceId], eventOf(v))
	}
	for w, es := range by {
		out[w] = rating.Lived(es)
	}

	return out, nil
}

func eventOf(v *ent.WorkspaceEvent) rating.Event {
	return rating.Event{Kind: levyan.WorkspaceEventKind(v.Kind), At: ledger.Moment(v.HappenedAt)}
}

// Events is everything `product` said became of workspace `ws`.
func Events(ctx context.Context, db *ent.Client, product, ws uuid.UUID) ([]rating.Event, error) {
	vs, err := db.WorkspaceEvent.Query().
		Where(entworkspaceevent.TenantId(product), entworkspaceevent.WorkspaceId(ws)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]rating.Event, len(vs))
	for i, v := range vs {
		out[i] = eventOf(v)
	}

	return out, nil
}

// Life is what became of workspace `ws` of `product` ([Lives]).
func Life(ctx context.Context, db *ent.Client, product, ws uuid.UUID) (rating.Life, error) {
	ls, err := Lives(ctx, db, product, ws)
	if err != nil {
		return rating.Life{}, err
	}

	return ls[ws], nil
}

// lateFor is which of `periods` an event of workspace `ws` that came late
// speaks for: each period from the one its reach is in -- what it changes is
// from then on ([rating.Reach]: its moment, or for a restore or a
// destruction the close of the stretch it ends) -- through the last whose
// close had begun when it came, which is all that was billed without it.
// The reach is the one every event said of the workspace says now: an event
// said since of a moment before can move what it changes, and whatever moves
// it was said late too.
func (r Rater) lateFor(ctx context.Context, product, ws uuid.UUID, periods []*ent.Period, out map[uuid.UUID]bool) error {
	vs, err := r.Db.WorkspaceEvent.Query().
		Where(entworkspaceevent.TenantId(product), entworkspaceevent.WorkspaceId(ws)).
		All(ctx)
	if err != nil {
		return err
	}
	es := make([]rating.Event, len(vs))
	for i, v := range vs {
		es[i] = eventOf(v)
	}
	for _, v := range vs {
		if v.LateThrough == nil {
			continue
		}
		from, through := rating.Reach(es, eventOf(v)), ledger.Moment(*v.LateThrough)
		for _, p := range periods {
			if end := ledger.Moment(p.DateEnd); end.After(from) && !end.After(through) {
				out[p.Id] = true
			}
		}
	}

	return nil
}

// LateThrough is when what an event of `product`'s whose reach is `at`
// ([rating.Reach]) says is late through: the end of the last of its periods
// whose close has begun, when that period ends after `at`, and false when no
// begun close reaches it -- read on the transaction the event is written in,
// under its workspace's lock.
func LateThrough(ctx context.Context, db *ent.Client, product uuid.UUID, at time.Time) (time.Time, bool, error) {
	p, err := db.Period.Query().
		Where(entperiod.TenantId(product), entperiod.DateEndGT(at)).
		Order(ent.Desc(entperiod.FieldDateEnd)).
		First(ctx)
	if ent.IsNotFound(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}

	return ledger.Moment(p.DateEnd), true, nil
}
