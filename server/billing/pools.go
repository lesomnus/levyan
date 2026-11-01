package billing

import (
	"context"
	"math/big"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entpool "github.com/lesomnus/levyan/internal/ent/pool"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/server/ledger"
)

// Taps is what a workspace's pools (#43) have left for each of its periods,
// and what each period took of them.
//
// What a period took of a pool is what the latest line rating it says: its
// own statement's line of the pool's meter, and then each correction of it
// since, each saying what the period takes as it is rated then
// (`StatementLine.pools`) -- the latest by the statement's period. What is
// left of a pool for a period is its quantity less what every other period
// took: so a period rated again takes what it takes now of what the others
// left it, and never what it took before as well. Taking is in the order
// periods are rated in a statement -- the corrected ones, oldest first, and
// then its own -- and each takes of what those before it left.
type Taps struct {
	pools map[uuid.UUID][]*ent.Pool // by meter

	// said is each pool's draw of each period as statements say it, and
	// took what the statement being worked out takes of it, by the period's
	// first moment in microseconds: a draw is what took says, or else what
	// said does.
	said map[uuid.UUID]map[int64]*big.Rat
	took map[uuid.UUID]map[int64]*big.Rat
}

// at is the key of the period beginning at `t`.
func at(t time.Time) int64 { return t.UnixMicro() }

// Taps is what `product`'s workspace `ws`'s pools have left, as its
// statements say what each period took.
func (r Rater) Taps(ctx context.Context, product, ws uuid.UUID) (*Taps, error) {
	out := &Taps{pools: map[uuid.UUID][]*ent.Pool{}, said: map[uuid.UUID]map[int64]*big.Rat{}, took: map[uuid.UUID]map[int64]*big.Rat{}}

	ps, err := r.Db.Pool.Query().
		Where(entpool.TenantId(product), entpool.WorkspaceId(ws)).
		Order(ent.Asc(entpool.FieldDateExpires), ent.Asc(entpool.FieldId)).
		All(ctx)
	if err != nil || len(ps) == 0 {
		return out, err
	}
	meterOf := map[uuid.UUID]uuid.UUID{}
	for _, v := range ps {
		out.pools[v.MeterId] = append(out.pools[v.MeterId], v)
		meterOf[v.Id] = v.MeterId
	}

	sts, err := r.Db.Statement.Query().
		Where(entstatement.TenantId(product), entstatement.WorkspaceId(ws)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// The latest line rating each period of each meter: by the period of
	// the statement it is on.
	type key struct {
		period int64
		meter  uuid.UUID
	}
	type said struct {
		on    time.Time
		pools map[uuid.UUID]*big.Rat
	}
	latest := map[key]said{}
	for _, s := range sts {
		on := ledger.Moment(s.DateStart)
		for _, l := range s.Lines.GetLines() {
			m, err := pdid.From(l.GetMeter())
			if err != nil {
				return nil, err
			}
			k := key{period: at(on), meter: m.Uuid()}
			if l.GetKind() == levyan.LineKind_LINE_KIND_CORRECTION {
				k.period = at(l.GetCorrectsStart().AsTime())
			}
			if was, ok := latest[k]; ok && !was.on.Before(on) {
				continue
			}
			v := said{on: on, pools: map[uuid.UUID]*big.Rat{}}
			for _, t := range l.GetPools() {
				id, err := pdid.From(t.GetPool())
				if err != nil {
					return nil, err
				}
				n, err := frac(t.GetTaken())
				if err != nil {
					return nil, err
				}
				v.pools[id.Uuid()] = n
			}
			latest[k] = v
		}
	}
	for k, v := range latest {
		for _, pl := range out.pools[k.meter] {
			if n, ok := v.pools[pl.Id]; ok && n.Sign() != 0 {
				if out.said[pl.Id] == nil {
					out.said[pl.Id] = map[int64]*big.Rat{}
				}
				out.said[pl.Id][k.period] = n
			}
		}
	}

	return out, nil
}

// drawn is what pool `id` took of period `p`, by its key.
func (t *Taps) drawn(id uuid.UUID, p int64) *big.Rat {
	if v, ok := t.took[id][p]; ok {
		return v
	}
	if v, ok := t.said[id][p]; ok {
		return v
	}
	return new(big.Rat)
}

// left is what is left of pool `v` for period `p`, by its key: its quantity
// less what every other period took of it, never less than nothing.
func (t *Taps) left(v *ent.Pool, p int64) *big.Rat {
	out := new(big.Rat).SetInt64(v.Quantity)
	seen := map[int64]bool{p: true}
	for _, m := range []map[int64]*big.Rat{t.took[v.Id], t.said[v.Id]} {
		for p := range m {
			if seen[p] {
				continue
			}
			seen[p] = true
			out.Sub(out, t.drawn(v.Id, p))
		}
	}
	if out.Sign() < 0 {
		out.SetInt64(0)
	}

	return out
}

// For is the pools of meter `m` for period `p`, with what each has left for
// it.
func (t *Taps) For(m uuid.UUID, p rating.Period) []rating.Pool {
	if t == nil {
		return nil
	}
	out := []rating.Pool{}
	for _, v := range t.pools[m] {
		out = append(out, rating.Pool{
			Id:      pdid.Id(v.Id),
			Left:    t.left(v, at(p.Start)),
			From:    ledger.Moment(v.ValidFrom),
			Expires: ledger.Moment(v.DateExpires),
		})
	}

	return out
}

// Took says that period `p` takes `takes` of meter `m`'s pools, and nothing
// of those of them it does not name.
func (t *Taps) Took(m uuid.UUID, p rating.Period, takes []rating.Take) {
	if t == nil {
		return
	}
	by := map[uuid.UUID]*big.Rat{}
	for _, v := range takes {
		by[v.Pool.Uuid()] = v.Quantity
	}
	for _, v := range t.pools[m] {
		n, ok := by[v.Id]
		if !ok {
			n = new(big.Rat)
		}
		if t.took[v.Id] == nil {
			t.took[v.Id] = map[int64]*big.Rat{}
		}
		t.took[v.Id][at(p.Start)] = n
	}
}

// Moved says whether what period `p` takes of meter `m`'s pools now is not
// what the statements say it took: a correction of it says so, though what
// it comes to may not move -- what is left of a pool for the periods after
// it does.
func (t *Taps) Moved(m uuid.UUID, p rating.Period) bool {
	if t == nil {
		return false
	}
	for _, v := range t.pools[m] {
		now, ok := t.took[v.Id][at(p.Start)]
		if !ok {
			continue
		}
		was, ok := t.said[v.Id][at(p.Start)]
		if !ok {
			was = new(big.Rat)
		}
		if now.Cmp(was) != 0 {
			return true
		}
	}

	return false
}

// Left is what is left of each pool as the statements say: its quantity less
// what every period they rate took of it.
func (t *Taps) Left(id uuid.UUID) *big.Rat {
	for _, vs := range t.pools {
		for _, v := range vs {
			if v.Id != id {
				continue
			}
			out := new(big.Rat).SetInt64(v.Quantity)
			for _, n := range t.said[id] {
				out.Sub(out, n)
			}
			if out.Sign() < 0 {
				out.SetInt64(0)
			}
			return out
		}
	}

	return new(big.Rat)
}

// PoolLeft is a pool, and what is left of it as the statements say.
type PoolLeft struct {
	Row  *ent.Pool
	Left *big.Rat
}

// InForce is the pools in force at any moment of `p` -- what a period of it
// takes from -- the soonest to expire first, each with what is left of it as
// the statements say: before what `p` takes of it.
func (t *Taps) InForce(p rating.Period) []PoolLeft {
	out := []PoolLeft{}
	for _, vs := range t.pools {
		for _, v := range vs {
			if ledger.Moment(v.ValidFrom).Before(p.End) && ledger.Moment(v.DateExpires).After(p.Start) {
				out = append(out, PoolLeft{Row: v, Left: t.Left(v.Id)})
			}
		}
	}
	slices.SortFunc(out, func(a, b PoolLeft) int {
		if c := a.Row.DateExpires.Compare(b.Row.DateExpires); c != 0 {
			return c
		}
		return slices.Compare(a.Row.Id[:], b.Row.Id[:])
	})

	return out
}
