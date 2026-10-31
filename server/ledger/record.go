package ledger

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	"github.com/lesomnus/levyan/internal/usage"
)

// Reading is what one workspace held of one stock meter at one moment, as a
// product sends it: payday's `meter.Reading`.
type Reading struct {
	// Workspace is the canonical text of its payday tenant's identifier.
	Workspace string

	// Meter is the meter's key.
	Meter string

	MeasuredAt time.Time
	Value      int64

	// Unit is the meter's, which a reading in any other is refused for not
	// being.
	Unit string
}

// Record keeps readings of stock meters a product sent, and answers what came
// of each, in the order they were sent.
//
// What is refused is refused alone: a reading of a meter the product has not
// got yet does not keep the others from being kept. An error is the ledger
// not having written any of what it would have, and the call is safe to make
// again as it was.
func (l Ledger) Record(ctx context.Context, product pdid.Id, rs []Reading) ([]Result, error) {
	if len(rs) > Most {
		return nil, fmt.Errorf("ledger: %d readings in one call, and %d is the most", len(rs), Most)
	}

	out := make([]Result, len(rs))
	now := l.now()
	db := ent.NewClient(ent.Driver(l.Drv))
	ms, err := meters(ctx, db, product)
	if err != nil {
		return nil, err
	}

	type item struct {
		i     int
		ws    pdid.Id
		meter *ent.Meter
		at    time.Time
		value int64
	}
	items := []item{}
	keys := []pdid.Id{}
	seen := map[pdid.Id]bool{}
	for i, r := range rs {
		ws, err := Workspace(r.Workspace)
		if err != nil {
			out[i] = refused(Invalid, "%s", err)
			continue
		}
		m, ok := ms[r.Meter]
		switch {
		case !ok:
			out[i] = refused(NoMeter, "no meter of this product is %q", r.Meter)
			continue
		case m.Kind != int32(levyan.MeterKind_METER_KIND_STOCK):
			out[i] = refused(OtherKind, "%s is a flow, which is counted by its OpenTelemetry Sum and not recorded", r.Meter)
			continue
		case r.Unit != m.Unit:
			out[i] = refused(Unit, "%s counts in %q, and this reading is in %q", r.Meter, m.Unit, r.Unit)
			continue
		case r.Value < 0:
			out[i] = refused(Invalid, "%s: %d is less than nothing, which nothing holds", r.Meter, r.Value)
			continue
		}
		at, bad := l.moment(r.MeasuredAt, now, "measured_at")
		if bad != nil {
			out[i] = *bad
			continue
		}

		items = append(items, item{i: i, ws: ws, meter: m, at: at, value: r.Value})
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

	err = l.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		ids := make([]uuid.UUID, 0, len(wss))
		for _, id := range wss {
			ids = append(ids, id)
		}
		if err := Lock(ctx, db, l.postgres(), ids...); err != nil {
			return err
		}

		from, to := items[0].at, items[0].at
		for _, v := range items {
			from, to = earliest(from, v.at), latest(to, v.at)
		}
		ps, err := Closed(ctx, db, product, from, to.Add(usage.Hold))
		if err != nil {
			return err
		}
		destroyed, err := Destroyed(ctx, db, product.Uuid(), ids...)
		if err != nil {
			return err
		}

		type at struct {
			ws    uuid.UUID
			meter uuid.UUID
			at    time.Time
		}
		sent := map[at]int64{}
		adds := []*levyan.ReadingAddRequest{}
		kept := []int{}
		for _, v := range items {
			w := wss[v.ws]
			k := at{ws: w, meter: v.meter.Id, at: v.at}

			// Twice in one call is once, or a conflict, as twice in two is.
			if was, ok := sent[k]; ok {
				if was == v.value {
					out[v.i] = Result{Outcome: Already}
				} else {
					out[v.i] = conflict(ctx, "reading", product, v.ws, v.meter.Key, v.at, was, v.value)
				}
				continue
			}

			u, err := db.Reading.Query().
				Where(
					entreading.TenantId(product.Uuid()),
					entreading.WorkspaceId(w),
					entreading.MeterId(v.meter.Id),
					entreading.MeasuredAt(v.at),
				).
				Only(ctx)
			switch {
			case err == nil:
				if u.Value == v.value {
					out[v.i] = Result{Outcome: Already, Late: u.Late}
				} else {
					out[v.i] = conflict(ctx, "reading", product, v.ws, v.meter.Key, v.at, u.Value, v.value)
				}
				continue
			case !ent.IsNotFound(err):
				return err
			}
			if d, ok := destroyed[w]; ok && !v.at.Before(d) {
				out[v.i] = gone(v.meter.Key, v.ws, d)
				continue
			}

			late := reaches(ps, v.at, v.at.Add(usage.Hold))
			sent[k] = v.value
			adds = append(adds, levyan.ReadingAddRequest_builder{
				Tenant:     levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
				Workspace:  levyan.WorkspaceRef_builder{Id: w[:]}.Build(),
				Meter:      levyan.MeterRef_builder{Id: v.meter.Id[:]}.Build(),
				MeasuredAt: stamp(v.at),
				Value:      v.value,
				Late:       late,
			}.Build())
			kept = append(kept, v.i)
			out[v.i] = Result{Outcome: Kept, Late: late}
		}

		for len(adds) > 0 {
			n := min(len(adds), 1000)
			if _, err := s.Reading().AddMany(ctx, levyan.ReadingAddManyRequest_builder{Items: adds[:n]}.Build()); err != nil {
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
