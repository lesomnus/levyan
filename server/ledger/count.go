package ledger

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entpoint "github.com/lesomnus/levyan/internal/ent/point"
	entseries "github.com/lesomnus/levyan/internal/ent/series"
)

// Point is one point of a flow's series, as a product exports it: how much
// the series had counted from StartedAt to CountedAt.
type Point struct {
	// Workspace is the canonical text of its payday tenant's identifier: the
	// point's `payday.tenant`.
	Workspace string

	// Meter is the meter's key, which is the Sum's name, and Unit the Sum's
	// unit.
	Meter string
	Unit  string

	// Digest is which stream it is of, and Source where the stream came from
	// (see `Series`).
	Digest []byte
	Source string

	StartedAt time.Time
	CountedAt time.Time
	Value     int64
}

// counted is one point to be kept, as [Ledger.Count] works it out.
type counted struct {
	i     int
	ws    pdid.Id
	meter *ent.Meter
	p     Point
	start time.Time
	at    time.Time
}

// run is one start of one series: a count from nothing.
type run struct {
	series uuid.UUID
	start  int64 // unix microseconds
}

// held is what a run has kept: its latest moment and the most it counted.
type held struct {
	latest time.Time
	most   int64
}

// Count keeps points of flows a product exported, and answers what came of
// each, in the order they came.
//
// What is refused is refused alone, as [Ledger.Record] refuses. An error is
// the ledger not having written any of what it would have, and the call is
// safe to make again as it was: an exporter that was not answered sends the
// same points again, and they are kept once.
func (l Ledger) Count(ctx context.Context, product pdid.Id, ps []Point) ([]Result, error) {
	if len(ps) > Most {
		return nil, fmt.Errorf("ledger: %d points in one call, and %d is the most", len(ps), Most)
	}

	out := make([]Result, len(ps))
	now := l.now()
	db := ent.NewClient(ent.Driver(l.Drv))
	ms, err := meters(ctx, db, product)
	if err != nil {
		return nil, err
	}

	items := []counted{}
	keys := []pdid.Id{}
	seen := map[pdid.Id]bool{}
	for i, p := range ps {
		ws, err := Workspace(p.Workspace)
		if err != nil {
			out[i] = refused(Invalid, "%s", err)
			continue
		}
		m, ok := ms[p.Meter]
		switch {
		case !ok:
			out[i] = refused(NoMeter, "no meter of this product is %q", p.Meter)
			continue
		case m.Kind != int32(levyan.MeterKind_METER_KIND_FLOW):
			out[i] = refused(OtherKind, "%s is a stock, which is recorded as readings and not counted", p.Meter)
			continue
		case p.Unit != m.Unit:
			out[i] = refused(Unit, "%s counts in %q, and this point is in %q", p.Meter, m.Unit, p.Unit)
			continue
		case p.Value < 0:
			out[i] = refused(Invalid, "%s: %d is less than nothing, which nothing counts", p.Meter, p.Value)
			continue
		case len(p.Digest) == 0:
			out[i] = refused(Invalid, "%s: a point of no series", p.Meter)
			continue
		}
		start, bad := l.moment(p.StartedAt, now, "start_time")
		if bad != nil {
			out[i] = *bad
			continue
		}
		at, bad := l.moment(p.CountedAt, now, "time")
		if bad != nil {
			out[i] = *bad
			continue
		}
		if at.Before(start) {
			out[i] = refused(Invalid, "%s: counted to %s, before it began counting at %s", p.Meter, at.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
			continue
		}

		items = append(items, counted{i: i, ws: ws, meter: m, p: p, start: start, at: at})
		if !seen[ws] {
			seen[ws] = true
			keys = append(keys, ws)
		}
	}
	if len(items) == 0 {
		return out, nil
	}

	wss, err := l.workspaces(ctx, db, product, keys)
	if err != nil {
		return nil, err
	}

	// In the order of their moments within each series, so that what a point
	// is measured against -- the most counted before it -- is in hand.
	slices.SortStableFunc(items, func(a, b counted) int {
		if c := bytes.Compare(a.p.Digest, b.p.Digest); c != 0 {
			return c
		}
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		return a.at.Compare(b.at)
	})

	err = l.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		ids := make([]uuid.UUID, 0, len(wss))
		for _, id := range wss {
			ids = append(ids, id)
		}
		if err := Lock(ctx, db, l.postgres(), ids...); err != nil {
			return err
		}

		series, err := l.series(ctx, s, db, product, wss, items)
		if err != nil {
			return err
		}
		runs, err := runsOf(ctx, db, series)
		if err != nil {
			return err
		}

		from, to := items[0].at, items[0].at
		for _, v := range items {
			from, to = earliest(from, v.at), latest(to, v.at)
		}
		closed, err := Closed(ctx, db, product, from, to.Add(time.Microsecond))
		if err != nil {
			return err
		}

		type moment struct {
			run run
			at  int64
		}
		sent := map[moment]int64{}
		adds := []*levyan.PointAddRequest{}
		for _, v := range items {
			sid := series[string(v.p.Digest)]
			k := run{series: sid, start: v.start.UnixMicro()}
			m := moment{run: k, at: v.at.UnixMicro()}

			// Twice in one call is once, or a conflict, as twice in two is.
			if was, ok := sent[m]; ok {
				if was == v.p.Value {
					out[v.i] = Result{Outcome: Already}
				} else {
					out[v.i] = conflict(ctx, "point", product, v.ws, v.meter.Key, v.at, was, v.p.Value)
				}
				continue
			}

			r := runs[k]
			late := false
			if r == nil || v.at.After(r.latest) {
				// After every point of its start that is kept, which is what
				// comes in order -- nearly all of it -- and is measured against
				// what the run holds and nothing else.
				if r != nil && r.most >= v.p.Value {
					out[v.i] = Result{Outcome: Nothing}
					continue
				}
				late = in(closed, v.at)
			} else {
				// Among them, or one of them, which is asked of the points
				// themselves.
				res, keep, err := l.among(ctx, db, product, sid, v)
				if err != nil {
					return err
				}
				if !keep {
					out[v.i] = res
					continue
				}
				late = res.Late
			}

			sent[m] = v.p.Value
			if r == nil {
				r = &held{}
				runs[k] = r
			}
			if v.at.After(r.latest) {
				r.latest = v.at
			}
			r.most = max(r.most, v.p.Value)

			adds = append(adds, levyan.PointAddRequest_builder{
				Tenant:    levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
				Workspace: levyan.WorkspaceRef_builder{Id: bytesOf(wss[v.ws])}.Build(),
				Series:    levyan.SeriesRef_builder{Id: sid[:]}.Build(),
				StartedAt: stamp(v.start),
				CountedAt: stamp(v.at),
				Value:     v.p.Value,
				Late:      late,
			}.Build())
			out[v.i] = Result{Outcome: Kept, Late: late}
		}

		for len(adds) > 0 {
			n := min(len(adds), 1000)
			if _, err := s.Point().AddMany(ctx, levyan.PointAddManyRequest_builder{Items: adds[:n]}.Build()); err != nil {
				return err
			}
			adds = adds[n:]
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// among is what comes of a point that is not after every point its run has
// kept: the same as one kept, another value for one, nothing over what came
// before it, or one to keep -- which is late when its own moment is in a
// closed period, or the moment of the point after it is: it takes what it
// counted from that point, so both periods are what it changes.
func (l Ledger) among(ctx context.Context, db *ent.Client, product pdid.Id, sid uuid.UUID, v counted) (Result, bool, error) {
	u, err := db.Point.Query().
		Where(entpoint.SeriesId(sid), entpoint.StartedAt(v.start), entpoint.CountedAt(v.at)).
		Only(ctx)
	if err == nil {
		if u.Value == v.p.Value {
			return Result{Outcome: Already, Late: u.Late}, false, nil
		}
		return conflict(ctx, "point", product, v.ws, v.meter.Key, v.at, u.Value, v.p.Value), false, nil
	}
	if !ent.IsNotFound(err) {
		return Result{}, false, err
	}

	before, err := db.Point.Query().
		Where(entpoint.SeriesId(sid), entpoint.StartedAt(v.start), entpoint.CountedAtLT(v.at), entpoint.ValueGTE(v.p.Value)).
		Exist(ctx)
	if err != nil {
		return Result{}, false, err
	}
	if before {
		return Result{Outcome: Nothing}, false, nil
	}

	moments := []time.Time{v.at}
	next, err := db.Point.Query().
		Where(entpoint.SeriesId(sid), entpoint.StartedAt(v.start), entpoint.CountedAtGT(v.at)).
		Order(ent.Asc(entpoint.FieldCountedAt)).
		First(ctx)
	switch {
	case err == nil:
		moments = append(moments, Moment(next.CountedAt))
	case !ent.IsNotFound(err):
		return Result{}, false, err
	}

	for _, t := range moments {
		ps, err := Closed(ctx, db, product, t, t.Add(time.Microsecond))
		if err != nil {
			return Result{}, false, err
		}
		if len(ps) > 0 {
			return Result{Late: true}, true, nil
		}
	}

	return Result{}, true, nil
}

// runsOf is what each start of each of `series` has kept.
func runsOf(ctx context.Context, db *ent.Client, series map[string]uuid.UUID) (map[run]*held, error) {
	ids := make([]uuid.UUID, 0, len(series))
	for _, id := range series {
		ids = append(ids, id)
	}

	var vs []struct {
		SeriesId  key     `json:"series_id"`
		StartedAt instant `json:"started_at"`
		Latest    instant `json:"latest"`
		Most      int64   `json:"most"`
	}
	err := db.Point.Query().
		Where(entpoint.SeriesIdIn(ids...)).
		GroupBy(entpoint.FieldSeriesId, entpoint.FieldStartedAt).
		Aggregate(
			ent.As(ent.Max(entpoint.FieldCountedAt), "latest"),
			ent.As(ent.Max(entpoint.FieldValue), "most"),
		).
		Scan(ctx, &vs)
	if err != nil {
		return nil, err
	}

	out := make(map[run]*held, len(vs))
	for _, v := range vs {
		k := run{series: uuid.UUID(v.SeriesId), start: Moment(time.Time(v.StartedAt)).UnixMicro()}
		out[k] = &held{latest: Moment(time.Time(v.Latest)), most: v.Most}
	}

	return out, nil
}

// series is the row of every stream `items` names, by its digest, written
// through `s` for one there is none of yet -- in the transaction that holds
// its workspace, so that two calls that both find it new are one after the
// other, and the second finds it.
func (l Ledger) series(ctx context.Context, s levyan.Server, db *ent.Client, product pdid.Id, wss map[pdid.Id]uuid.UUID, items []counted) (map[string]uuid.UUID, error) {
	digests := [][]byte{}
	want := map[string]bool{}
	for _, v := range items {
		if !want[string(v.p.Digest)] {
			want[string(v.p.Digest)] = true
			digests = append(digests, v.p.Digest)
		}
	}

	vs, err := db.Series.Query().
		Where(entseries.TenantId(product.Uuid()), entseries.DigestIn(digests...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]uuid.UUID{}
	for _, v := range vs {
		out[string(v.Digest)] = v.Id
	}

	for _, v := range items {
		if _, ok := out[string(v.p.Digest)]; ok {
			continue
		}
		w := wss[v.ws]
		u, err := s.Series().Add(ctx, levyan.SeriesAddRequest_builder{
			Tenant:    levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
			Workspace: levyan.WorkspaceRef_builder{Id: w[:]}.Build(),
			Meter:     levyan.MeterRef_builder{Id: v.meter.Id[:]}.Build(),
			Digest:    v.p.Digest,
			Source:    v.p.Source,
		}.Build())
		if err != nil {
			return nil, err
		}
		id, err := pdid.From(u.GetId())
		if err != nil {
			return nil, err
		}
		out[string(v.p.Digest)] = id.Uuid()
	}

	return out, nil
}
