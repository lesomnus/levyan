package billing

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entpoint "github.com/lesomnus/levyan/internal/ent/point"
	entprice "github.com/lesomnus/levyan/internal/ent/price"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	entseries "github.com/lesomnus/levyan/internal/ent/series"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/internal/usage"
	"github.com/lesomnus/levyan/server/ledger"
)

// Rater rates a product's workspaces from the ledger on Db: the deployment's
// client, or a transaction's.
//
// It reads with ent and narrows every read to the product and workspace it is
// asked about, which the caller has from a frame or a row and never from a
// request: a product's own call is about that product and no other.
type Rater struct {
	Db *ent.Client
}

// Book is a product's price book: its meters, by their keys, and every
// version of every price.
type Book struct {
	Meters   []*ent.Meter
	Versions pricing.Book
}

// Book is what `product` is priced by.
func (r Rater) Book(ctx context.Context, product uuid.UUID) (Book, error) {
	ms, err := r.Db.Meter.Query().Where(entmeter.TenantId(product)).Order(ent.Asc(entmeter.FieldKey)).All(ctx)
	if err != nil {
		return Book{}, err
	}
	zero := map[uuid.UUID]bool{}
	for _, m := range ms {
		zero[m.Id] = m.UnboundedZero
	}

	ps, err := r.Db.Price.Query().Where(entprice.TenantId(product)).All(ctx)
	if err != nil {
		return Book{}, err
	}
	out := Book{Meters: ms}
	for _, p := range ps {
		out.Versions = append(out.Versions, pricing.Version{
			Id:        pdid.Id(p.Id),
			Meter:     pdid.Id(p.MeterId),
			Channel:   levyan.Channel(p.Channel),
			Currency:  p.Currency,
			Rate:      pricing.RateOf(p.Included, p.UnitPrice, p.Per, p.Bands, zero[p.MeterId]),
			Effective: p.DateEffective,
			Existing:  p.DateExisting,
			Published: p.DateCreated,
		})
	}

	return out, nil
}

// Since is when the ledger first says anything of workspace `ws`: its
// earliest reading or point, which is when it began to be a customer as far as
// levyan can tell -- until billing accounts and the product's lifecycle say
// it (#4, #14). False when it says nothing.
func (r Rater) Since(ctx context.Context, product, ws uuid.UUID) (time.Time, bool, error) {
	var (
		at time.Time
		ok bool
	)
	keep := func(v time.Time) {
		if v.IsZero() {
			return
		}
		v = ledger.Moment(v)
		if !ok || v.Before(at) {
			at, ok = v, true
		}
	}

	rs, err := r.Db.Reading.Query().
		Where(entreading.TenantId(product), entreading.WorkspaceId(ws)).
		Order(ent.Asc(entreading.FieldMeasuredAt)).
		Limit(1).
		All(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, v := range rs {
		keep(v.MeasuredAt)
	}

	ps, err := r.Db.Point.Query().
		Where(entpoint.WorkspaceId(ws)).
		Order(ent.Asc(entpoint.FieldCountedAt)).
		Limit(1).
		All(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, v := range ps {
		keep(v.CountedAt)
	}

	return at, ok, nil
}

// Usage is what workspace `ws` held or did of meter `m` over `p`. `until`,
// when it is set, carries a stock's latest reading to it -- what an estimate
// of a period not yet over takes the rest of it to hold.
func (r Rater) Usage(ctx context.Context, product, ws uuid.UUID, m *ent.Meter, p rating.Period, until time.Time) (rating.Usage, error) {
	if m.Kind == int32(levyan.MeterKind_METER_KIND_FLOW) {
		n, err := r.counted(ctx, ws, m, p)
		if err != nil {
			return rating.Usage{}, err
		}
		return rating.Usage{Counted: n}, nil
	}

	// Those that speak for some of it: in it, and as long before it as a
	// reading holds.
	vs, err := r.Db.Reading.Query().
		Where(
			entreading.TenantId(product),
			entreading.WorkspaceId(ws),
			entreading.MeterId(m.Id),
			entreading.MeasuredAtGTE(p.Start.Add(-usage.Hold)),
			entreading.MeasuredAtLT(p.End),
		).
		Order(ent.Asc(entreading.FieldMeasuredAt)).
		All(ctx)
	if err != nil {
		return rating.Usage{}, err
	}

	all := make([]usage.Reading, 0, len(vs))
	in := []usage.Reading{}
	for _, v := range vs {
		u := usage.Reading{At: ledger.Moment(v.MeasuredAt), Value: v.Value}
		all = append(all, u)
		if !u.At.Before(p.Start) {
			in = append(in, u)
		}
	}

	return rating.Usage{Held: usage.Profile(all, p.Start, p.End, until), Readings: in}, nil
}

// counted is what workspace `ws`'s series of flow `m` counted over `p`: the
// points counted in it, and of each start among them the most it had counted
// before it, which is what its first point in the period counts from.
func (r Rater) counted(ctx context.Context, ws uuid.UUID, m *ent.Meter, p rating.Period) (int64, error) {
	ss, err := r.Db.Series.Query().Where(entseries.WorkspaceId(ws), entseries.MeterId(m.Id)).Ids(ctx)
	if err != nil || len(ss) == 0 {
		return 0, err
	}

	pts, err := r.Db.Point.Query().
		Where(
			entpoint.WorkspaceId(ws),
			entpoint.SeriesIdIn(ss...),
			entpoint.CountedAtGTE(p.Start),
			entpoint.CountedAtLT(p.End),
		).
		All(ctx)
	if err != nil {
		return 0, err
	}

	by := map[uuid.UUID][]usage.Point{}
	type run struct {
		series uuid.UUID
		start  int64
	}
	runs := map[run]time.Time{}
	for _, v := range pts {
		start := ledger.Moment(v.StartedAt)
		by[v.SeriesId] = append(by[v.SeriesId], usage.Point{Start: start, At: ledger.Moment(v.CountedAt), Value: v.Value})
		runs[run{v.SeriesId, start.UnixMicro()}] = start
	}

	for k, start := range runs {
		var vs []struct {
			Most sql.NullInt64 `json:"most"`
		}
		err := r.Db.Point.Query().
			Where(entpoint.SeriesId(k.series), entpoint.StartedAt(start), entpoint.CountedAtLT(p.Start)).
			Aggregate(ent.As(ent.Max(entpoint.FieldValue), "most")).
			Scan(ctx, &vs)
		if err != nil {
			return 0, err
		}
		if len(vs) == 1 && vs[0].Most.Valid {
			by[k.series] = append(by[k.series], usage.Point{Start: start, At: p.Start.Add(-time.Microsecond), Value: vs[0].Most.Int64})
		}
	}

	var total int64
	for _, ps := range by {
		n, err := usage.Increase(ps, p.Start, p.End)
		if err != nil {
			return 0, err
		}
		if total > total+n {
			return 0, usage.ErrTooMuch
		}
		total += n
	}

	return total, nil
}

// Lines is what each of the product's meters came to for workspace `ws` over
// `p`, a line for each the ledger says anything of in it, in the order of
// their keys. A product its terms price nothing for has its lines of usage
// and no parts.
func (r Rater) Lines(ctx context.Context, b Book, product, ws uuid.UUID, p rating.Period, t Terms, until time.Time) ([]rating.Line, error) {
	since, ok, err := r.Since(ctx, product, ws)
	if err != nil || !ok {
		return nil, err
	}

	out := []rating.Line{}
	for _, m := range b.Meters {
		u, err := r.Usage(ctx, product, ws, m, p, until)
		if err != nil {
			return nil, err
		}
		if !u.Any() {
			continue
		}

		book := b.Versions
		if !t.Priced() {
			book = nil
		}
		l, err := rating.Rate(meterOf(m), u, p, book, pricing.Line{Meter: pdid.Id(m.Id), Channel: t.Channel, Currency: t.Currency}, since)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Key, err)
		}
		out = append(out, l)
	}

	return out, nil
}

func meterOf(m *ent.Meter) rating.Meter {
	return rating.Meter{
		Id:          pdid.Id(m.Id),
		Key:         m.Key,
		Unit:        m.Unit,
		Kind:        levyan.MeterKind(m.Kind),
		Aggregation: levyan.Aggregation(m.Aggregation),
	}
}

// Correction is how much what a closed period was billed for one meter moves:
// what it comes to now, rated again from the ledger as it is, less what was
// billed for it -- its statement's line and the corrections made since.
type Correction struct {
	Period *ent.Period

	// Statement is the period's statement of the workspace, when it had one.
	Statement *ent.Statement

	// Now is what the meter comes to now over the period; a line of nothing
	// for a meter the period was billed for and is not now.
	Now rating.Line

	Billed *big.Rat
	Moves  *big.Rat
}

// Corrections is how much what workspace `ws` was billed for the product's
// periods closed before `before` moves, for each period a late row of its
// speaks for: a correction of each meter whose amount moved, in the order of
// the periods and then of the meters' keys. One already made comes to
// nothing, and is not here.
//
// A period whose statement is in another channel or currency than `t` cannot
// be corrected on a statement in `t`, and is said in `skipped`.
func (r Rater) Corrections(ctx context.Context, b Book, product, ws uuid.UUID, before time.Time, t Terms) (out []Correction, skipped []*ent.Period, err error) {
	periods, err := r.Db.Period.Query().
		Where(entperiod.TenantId(product), entperiod.DateStartLT(before)).
		Order(ent.Asc(entperiod.FieldDateStart)).
		All(ctx)
	if err != nil || len(periods) == 0 {
		return nil, nil, err
	}

	marked, err := r.marked(ctx, product, ws, periods)
	if err != nil || len(marked) == 0 {
		return nil, nil, err
	}

	sts, err := r.Db.Statement.Query().Where(entstatement.TenantId(product), entstatement.WorkspaceId(ws)).All(ctx)
	if err != nil {
		return nil, nil, err
	}

	meters := map[uuid.UUID]*ent.Meter{}
	for _, m := range b.Meters {
		meters[m.Id] = m
	}

	for _, pr := range periods {
		if !marked[pr.Id] {
			continue
		}

		var of *ent.Statement
		for _, s := range sts {
			if s.PeriodId == pr.Id {
				of = s
			}
		}
		terms := t
		if of != nil {
			c, err := Channel(of.Channel)
			if err != nil {
				return nil, nil, err
			}
			if c != t.Channel || of.Currency != t.Currency {
				skipped = append(skipped, pr)
				continue
			}
		}

		billed, err := billedFor(sts, pr)
		if err != nil {
			return nil, nil, err
		}
		p := rating.Period{Start: ledger.Moment(pr.DateStart), End: ledger.Moment(pr.DateEnd)}
		lines, err := r.Lines(ctx, b, product, ws, p, terms, time.Time{})
		if err != nil {
			return nil, nil, err
		}

		now := map[uuid.UUID]rating.Line{}
		for _, l := range lines {
			now[l.Meter.Id.Uuid()] = l
		}
		ids := []uuid.UUID{}
		for id := range now {
			ids = append(ids, id)
		}
		for id := range billed {
			if _, ok := now[id]; !ok {
				ids = append(ids, id)
			}
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int {
			return compareKeys(meters[a], meters[b], a, b)
		})

		for _, id := range ids {
			l, ok := now[id]
			if !ok {
				m := meters[id]
				if m == nil {
					return nil, nil, fmt.Errorf("billing: a statement names meter %s, which the product has not got", pdid.Id(id))
				}
				l = rating.Line{Meter: meterOf(m), Quantity: new(big.Rat), Included: new(big.Rat), Billable: new(big.Rat), Amount: new(big.Rat)}
			}
			was := billed[id]
			if was == nil {
				was = new(big.Rat)
			}
			moves := new(big.Rat).Sub(l.Amount, was)
			if moves.Sign() == 0 {
				continue
			}
			out = append(out, Correction{Period: pr, Statement: of, Now: l, Billed: was, Moves: moves})
		}
	}

	return out, skipped, nil
}

// marked is which of `periods` a late row of workspace `ws` speaks for: a
// reading measured in one or within a reading's hold before it, a point
// counted in one or whose next point is.
func (r Rater) marked(ctx context.Context, product, ws uuid.UUID, periods []*ent.Period) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	reach := func(from, to time.Time) {
		for _, p := range periods {
			if p.DateStart.Before(to) && p.DateEnd.After(from) {
				out[p.Id] = true
			}
		}
	}

	rs, err := r.Db.Reading.Query().Where(entreading.WorkspaceId(ws), entreading.Late(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range rs {
		at := ledger.Moment(v.MeasuredAt)
		reach(at, at.Add(usage.Hold))
	}

	ps, err := r.Db.Point.Query().Where(entpoint.WorkspaceId(ws), entpoint.Late(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range ps {
		at := ledger.Moment(v.CountedAt)
		reach(at, at.Add(time.Microsecond))

		next, err := r.Db.Point.Query().
			Where(entpoint.SeriesId(v.SeriesId), entpoint.StartedAt(v.StartedAt), entpoint.CountedAtGT(v.CountedAt)).
			Order(ent.Asc(entpoint.FieldCountedAt)).
			First(ctx)
		switch {
		case err == nil:
			t := ledger.Moment(next.CountedAt)
			reach(t, t.Add(time.Microsecond))
		case !ent.IsNotFound(err):
			return nil, err
		}
	}

	return out, nil
}

// billedFor is what each meter was billed for period `p` by the statements
// `sts`: its own line on the period's statement, and every correction of the
// period on the others.
func billedFor(sts []*ent.Statement, p *ent.Period) (map[uuid.UUID]*big.Rat, error) {
	out := map[uuid.UUID]*big.Rat{}
	add := func(l *levyan.StatementLine) error {
		id, err := pdid.From(l.GetMeter())
		if err != nil {
			return err
		}
		a, err := frac(l.GetAmount())
		if err != nil {
			return err
		}
		v, ok := out[id.Uuid()]
		if !ok {
			v = new(big.Rat)
			out[id.Uuid()] = v
		}
		v.Add(v, a)
		return nil
	}

	for _, s := range sts {
		for _, l := range s.Lines.GetLines() {
			switch {
			case s.PeriodId == p.Id && l.GetKind() == levyan.LineKind_LINE_KIND_USAGE:
			case l.GetKind() == levyan.LineKind_LINE_KIND_CORRECTION && l.GetCorrectsStart().AsTime().Equal(p.DateStart):
			default:
				continue
			}
			if err := add(l); err != nil {
				return nil, err
			}
		}
	}

	return out, nil
}

func compareKeys(a, b *ent.Meter, x, y uuid.UUID) int {
	switch {
	case a != nil && b != nil && a.Key != b.Key:
		if a.Key < b.Key {
			return -1
		}
		return 1
	}
	return slices.Compare(x[:], y[:])
}

// frac reads a fraction as a statement writes one.
func frac(s string) (*big.Rat, error) {
	if s == "" {
		return new(big.Rat), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("billing: %q is not a fraction", s)
	}

	return r, nil
}
