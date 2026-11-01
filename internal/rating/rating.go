// Package rating is what a period's usage of a meter comes to under the price
// book: which versions were in force through it, what each part of it is
// charged, and what a statement of several meters comes to in all.
//
// It is pure, as `internal/pricing` and `internal/usage` are: handed the usage,
// the versions and the period, it answers the same however often it is asked,
// which is what a closed period rated again for its correction relies on.
//
// # The rules (levyan#6: Claude's, the lead's, which the owner may overturn)
//
//   - A period is what the product's zone makes a calendar month. Its usage is
//     the meter's aggregation over the **whole** period ([usage.Average],
//     [usage.Most], [usage.Last], or what a flow counted), and what is
//     included for nothing is not prorated for a workspace that began during
//     it: its average is over the whole period, the time before it began
//     counting as nothing, and what is included is all of what the version
//     includes.
//   - A price that changes during a period splits it ([pricing.Book.Timeline]),
//     and each part is charged by its share of the time: by the unit, the
//     whole period's usage at that part's version, weighed by the part's share
//     of the time the workspace was a customer in the period; by bands, the
//     time spent at each value in that part, at that part's band for it.
//   - A band's fee is for a whole period, so the time a value spent in a band
//     is what is charged of it: the retention window kept for a third of a
//     period is a third of that window's fee.
//   - Everything is exact until a statement's total, which is rounded once,
//     down ([Total]): when in doubt levyan charges less, and a total rounded at
//     every step would be a different bill for the same usage depending on how
//     the steps were cut.
//
// And levyan#14's -- when a workspace is no customer, which is the owner's
// (2026-11-01, levyan#1 open question 102); how it is taken out of what is
// rated is Claude's, the lead's:
//
//   - A workspace is no customer while it is closed and nothing has restored
//     it -- from a close it is still in when it is rated, or one it was
//     destroyed in -- nor from its destruction on, for good ([Lived]). A
//     stretch it was closed for and then restored from is billed as any
//     open time is. What it held and did while it was no customer is taken
//     out of its usage, as the time before it began is: its average counts
//     that time as nothing, its most and its last are of the readings
//     measured outside it, a flow counts nothing in it, and the time a price
//     by bands charges is the time outside it. What its prices include is
//     all of what they include, as for a workspace that began during the
//     period. A workspace away for the whole of a period is billed nothing
//     for it.
//
// And levyan#43's -- that the owner wants a workspace given more of a meter
// for nothing is the owner's (2026-11-01); how it is rated is Claude's
// proposal, which the owner had carried out, and how it is prorated is the
// implementation's:
//
//   - An allowance a workspace is given ([Allowance]) is added to what the
//     version in force includes, for the stretch of the period it is in
//     force: a part of the period priced by the unit is cut again where an
//     allowance begins or ends, as the period is cut where a version does,
//     and each piece is charged by its share of the time the workspace was a
//     customer -- so one in force for a third of the period gives a third of
//     what it gives, and one in force for all of the time a workspace that
//     began during the period was a customer gives all of it, as what a
//     price includes is not prorated for that workspace either. What it
//     gives is the workspace's whatever it is rated by: a workspace rated
//     from the first unit is given it all the same, and a version published
//     during it changes nothing of it. It never makes what is billable less
//     than nothing, and a price by bands, which includes nothing, is given
//     nothing by it.
//   - What is billable past what is included -- allowances' included -- is
//     taken first out of the workspace's pools ([Pool]), bundles of the
//     meter's unit for a period: of each pool in force at any moment of the
//     period, the soonest to expire first, as far as what is left of it goes,
//     and with no share of the period -- a bundle is not prorated. What is
//     priced is what is left, each part priced by the unit charged for its
//     share of it: a pool takes of every part in proportion to what the part
//     is billable for, so that which version priced what it took is no
//     question. What is left of a pool for a period is the caller's to say
//     -- its quantity less what the other periods took -- and what the
//     period takes is said back ([Line.Takes]).
package rating

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/usage"
)

// Version is the version of these rules -- and of the hold a reading speaks
// for ([usage.Hold]) -- that a statement says it was rated by. A change that
// moves what any usage comes to is a new version.
//
// 2 is a workspace away -- closed, or destroyed -- being no customer, and its
// last reading holding no further than its destruction (#14). A workspace
// nothing was said of comes to what it came to by 1.
//
// 3 is a stretch closed and then restored being billed as any open time is
// (the owner's, 2026-11-01): away is only what is closed still, or ended in a
// destruction, and what follows a destruction. A workspace never restored
// comes to what it came to by 2.
//
// 4 is a workspace's allowances added to what its prices include (#43). A
// workspace given none comes to what it came to by 3.
//
// 5 is what is billable taken out of a workspace's pools before it is priced
// (#43). A workspace given none comes to what it came to by 4.
const Version = 5

// Meter is a meter as it is rated.
type Meter struct {
	Id          pdid.Id
	Key         string
	Unit        string
	Kind        levyan.MeterKind
	Aggregation levyan.Aggregation
}

// Period is a stretch of a product's billing, [Start, End).
type Period struct {
	Start time.Time
	End   time.Time
}

// Usage is what the ledger says a period held or did of one meter.
type Usage struct {
	// Held is what a stock held through the period ([usage.Profile]).
	Held []usage.Held

	// Readings are a stock's readings measured in the period, which its most
	// and its last are of.
	Readings []usage.Reading

	// Counted is what a flow counted over the period ([usage.Increase]).
	Counted int64
}

// Any says whether there is anything of the meter in the period to rate.
func (u Usage) Any() bool { return len(u.Held) > 0 || len(u.Readings) > 0 || u.Counted > 0 }

// Line is what one meter came to over a period.
type Line struct {
	Meter Meter

	// Quantity is the usage as the meter aggregates it, in its unit.
	Quantity *big.Rat

	// Included and Billable are what the parts priced by the unit included
	// and found billable, each weighed by its share; a price by bands has
	// neither, and its bands say what it came to. What was included is what
	// the versions included and what the workspace's allowances gave
	// together, and Allowance is the second of the two.
	Included  *big.Rat
	Billable  *big.Rat
	Allowance *big.Rat

	// Pooled is what of Billable the workspace's pools took, and Takes what
	// each took: what is priced is what is billable past it.
	Pooled *big.Rat
	Takes  []Take

	// Amount is what it comes to in the currency's minor unit, exactly.
	Amount *big.Rat

	// Parts are the stretches of the period one version was in force for the
	// customer, and what each came to; a stretch nothing was in force is in
	// none.
	Parts []Part

	// Unpriced are the stretches of the period, while it was a customer's,
	// that nothing was in force on the line it is priced by while a version
	// of the meter's price was on another line -- another channel, another
	// currency: what came to nothing for want of a price, and not because
	// the meter is sold for nothing (#9). A meter priced on no line at the
	// time is not sold then, and is in none.
	Unpriced []Stretch
}

// Stretch is a stretch of time, [From, To).
type Stretch = usage.Stretch

// Part is one stretch of a period priced by one version, with the same
// allowances in force through it.
type Part struct {
	Version pdid.Id
	From    time.Time
	To      time.Time

	// Share is what of the period the part is charged by.
	Share *big.Rat

	// By the unit: what the version includes, what the workspace's
	// allowances in force through the part gave on top of it, and which,
	// and what was billable past the two.
	Included   int64
	Allowance  int64
	Allowances []pdid.Id
	Billable   *big.Rat

	// Pooled is what of the part's billable, weighed by its share, the
	// workspace's pools took: the line's [Line.Pooled] shared among its
	// parts in proportion to what each is billable for.
	Pooled *big.Rat

	// By bands: the time spent in each.
	Bands []Band

	Amount *big.Rat
}

// Band is the time a part spent in one band of a price by bands.
type Band struct {
	UpTo    int64
	Bounded bool
	Fee     int64

	// Share is the share of the period spent in it.
	Share  *big.Rat
	Amount *big.Rat
}

// Quantity is the usage of `m` over `p` as its aggregation says.
func Quantity(m Meter, u Usage, p Period) *big.Rat {
	switch m.Aggregation {
	case levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE:
		return usage.Average(u.Held, p.Start, p.End)
	case levyan.Aggregation_AGGREGATION_MAX:
		v, _ := usage.Most(u.Readings, p.Start, p.End)
		return new(big.Rat).SetInt64(v)
	case levyan.Aggregation_AGGREGATION_LAST:
		v, _ := usage.Last(u.Readings, p.Start, p.End)
		return new(big.Rat).SetInt64(v)
	case levyan.Aggregation_AGGREGATION_SUM:
		return new(big.Rat).SetInt64(u.Counted)
	}

	return new(big.Rat)
}

// whole is the usage as a price by the unit reads it: the time held at each
// value, for an average, and the one value for the rest.
func whole(m Meter, u Usage, p Period) pricing.Usage {
	if m.Aggregation == levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE {
		return shares(u.Held, p.Start, p.End, p)
	}

	q := Quantity(m, u, p)
	return pricing.Usage{{Value: q.Num().Int64(), Of: big.NewRat(1, 1)}}
}

// shares is the time held at each value within [from, to), each as its share
// of the period.
func shares(hs []usage.Held, from, to time.Time, p Period) pricing.Usage {
	out := pricing.Usage{}
	for _, h := range hs {
		a, b := latest(h.From, from), earliest(h.To, to)
		if !a.Before(b) {
			continue
		}
		out = append(out, pricing.Share{Value: h.Value, Of: usage.Share(a, b, p.Start, p.End)})
	}

	return out
}

// Customer is when a workspace was a customer: from Since -- when its first
// payer began to pay for it, or the ledger first said anything of it -- but
// for the stretches it was Away: closed and not restored, and from its
// destruction on (#14, [Lived]). Since says which of a price's dates it is priced by, a customer
// new or existing; what it held and did while away is taken out of its usage
// before it is rated ([usage.Cut]), and a version in force is charged by its
// share of the time the workspace was a customer.
type Customer struct {
	Since time.Time
	Away  []Stretch

	// Allowances are what the workspace was given of the meter being rated
	// on top of what its prices include (#43), in any order.
	Allowances []Allowance

	// Pools are its bundles of the meter (#43), each with what is left of it
	// for the period being rated, in any order.
	Pools []Pool
}

// Pool is a bundle of a meter a workspace was given (#43), in the meter's
// unit for a period: Left of it is left for the period being rated -- its
// quantity less what the other periods took -- and it is taken from by a
// period it is in force at any moment of, from From until Expires.
type Pool struct {
	Id      pdid.Id
	Left    *big.Rat
	From    time.Time
	Expires time.Time
}

// Take is what of one pool a line took.
type Take struct {
	Pool     pdid.Id
	Quantity *big.Rat
}

// Allowance is more of a meter a workspace was given for nothing each
// period, on top of what its prices include (#43): Quantity, in the meter's
// unit, from From until Until -- which is never, when it is zero.
type Allowance struct {
	Id       pdid.Id
	Quantity int64
	From     time.Time
	Until    time.Time
}

// in says whether `a` is in force at `t`.
func (a Allowance) in(t time.Time) bool {
	return !t.Before(a.From) && (a.Until.IsZero() || t.Before(a.Until))
}

// Rate is what `u` of meter `m` comes to over `p`, under the line of `book` a
// customer `c` is priced by. `u` is what the workspace held and did while it
// was a customer, the time it was away taken out of it.
//
// What is not priced -- a stretch before the line's first version applied to
// the customer -- is charged nothing, and is in no part; nor is anything of
// a workspace that was away for the whole of what of `p` it might have been a
// customer in.
func Rate(m Meter, u Usage, p Period, book pricing.Book, line pricing.Line, c Customer) (Line, error) {
	out := Line{
		Meter:     m,
		Quantity:  Quantity(m, u, p),
		Included:  new(big.Rat),
		Billable:  new(big.Rat),
		Allowance: new(big.Rat),
		Pooled:    new(big.Rat),
		Amount:    new(big.Rat),
	}
	if !p.Start.Before(p.End) {
		return out, nil
	}
	since := c.Since
	if since.IsZero() || since.Before(p.Start) {
		since = p.Start
	}
	if !since.Before(p.End) {
		return out, nil
	}
	customer := usage.Length(usage.Open(since, p.End, c.Away))
	if customer.Sign() == 0 {
		return out, nil
	}

	unit := whole(m, u, p)
	for _, s := range book.Timeline(line, since, p.Start, p.End) {
		if !s.Priced {
			out.Unpriced = append(out.Unpriced, outside(elsewhere(book, line, since, latest(s.From, since), s.To), c.Away)...)
			continue
		}
		v := s.Version

		if b, ok := v.Rate.(pricing.Banded); ok && m.Kind == levyan.MeterKind_METER_KIND_STOCK {
			part, err := banded(v, b, u, s, p)
			if err != nil {
				return Line{}, err
			}
			if part.Share.Sign() > 0 {
				out.Parts = append(out.Parts, part)
				out.Amount.Add(out.Amount, part.Amount)
			}
			continue
		}

		pu, ok := v.Rate.(pricing.PerUnit)
		if !ok {
			// By bands, of a flow: the whole of what it counted, at the band
			// of it, for the part's share. Nothing included, and nothing
			// given on top.
			w := new(big.Rat).Quo(usage.Length(usage.Open(latest(s.From, since), s.To, c.Away)), customer)
			if w.Sign() == 0 {
				continue
			}
			exact, err := v.Exact(unit)
			if err != nil {
				return Line{}, fmt.Errorf("rating: %s: %w", m.Key, err)
			}
			part := Part{Version: v.Id, From: s.From, To: s.To, Share: w, Billable: new(big.Rat), Amount: new(big.Rat).Mul(exact, w)}
			out.Parts = append(out.Parts, part)
			out.Amount.Add(out.Amount, part.Amount)
			continue
		}

		// By the unit, cut again where an allowance begins or ends: each
		// piece's share of the time the workspace was a customer in the
		// period, its whole usage at this part's version with what the
		// allowances in force through the piece give added to what the
		// version includes, for that share.
		for _, piece := range pieces(s.From, s.To, c.Allowances) {
			w := new(big.Rat).Quo(usage.Length(usage.Open(latest(piece.From, since), piece.To, c.Away)), customer)
			if w.Sign() == 0 {
				continue
			}
			given, ids, err := allowed(c.Allowances, piece.From)
			if err != nil {
				return Line{}, fmt.Errorf("rating: %s: %w", m.Key, err)
			}
			with := v
			if given > 0 {
				if pu.Included > math.MaxInt64-given {
					return Line{}, fmt.Errorf("rating: %s: %w", m.Key, ErrTooMuch)
				}
				r := pu
				r.Included += given
				with.Rate = r
			}
			exact, err := with.Exact(unit)
			if err != nil {
				return Line{}, fmt.Errorf("rating: %s: %w", m.Key, err)
			}

			part := Part{
				Version:    v.Id,
				From:       piece.From,
				To:         piece.To,
				Share:      w,
				Included:   pu.Included,
				Allowance:  given,
				Allowances: ids,
				Billable:   new(big.Rat).Sub(out.Quantity, new(big.Rat).SetInt64(pu.Included+given)),
				Amount:     new(big.Rat).Mul(exact, w),
			}
			if part.Billable.Sign() < 0 {
				part.Billable.SetInt64(0)
			}
			out.Included.Add(out.Included, new(big.Rat).Mul(new(big.Rat).SetInt64(pu.Included+given), w))
			out.Allowance.Add(out.Allowance, new(big.Rat).Mul(new(big.Rat).SetInt64(given), w))
			out.Billable.Add(out.Billable, new(big.Rat).Mul(part.Billable, w))
			out.Parts = append(out.Parts, part)
			out.Amount.Add(out.Amount, part.Amount)
		}
	}

	take(&out, p, c.Pools)

	return out, nil
}

// take takes what `l` is billable for out of `pools` in force over `p`, the
// soonest to expire first, and charges each part priced by the unit for what
// is left of its share: in proportion to what each is billable for.
func take(l *Line, p Period, pools []Pool) {
	if l.Billable.Sign() <= 0 || len(pools) == 0 {
		return
	}

	in := []Pool{}
	for _, v := range pools {
		if v.Left != nil && v.Left.Sign() > 0 && v.From.Before(p.End) && v.Expires.After(p.Start) {
			in = append(in, v)
		}
	}
	slices.SortFunc(in, func(a, b Pool) int {
		if c := a.Expires.Compare(b.Expires); c != 0 {
			return c
		}
		if c := a.From.Compare(b.From); c != 0 {
			return c
		}
		return slices.Compare(a.Id[:], b.Id[:])
	})

	need := new(big.Rat).Set(l.Billable)
	for _, v := range in {
		if need.Sign() == 0 {
			break
		}
		n := new(big.Rat).Set(v.Left)
		if n.Cmp(need) > 0 {
			n.Set(need)
		}
		l.Takes = append(l.Takes, Take{Pool: v.Id, Quantity: n})
		l.Pooled.Add(l.Pooled, n)
		need.Sub(need, n)
	}
	if l.Pooled.Sign() == 0 {
		return
	}

	// What of each part is left to price: its share of what the pools left.
	left := new(big.Rat).Quo(need, l.Billable)
	of := new(big.Rat).Quo(l.Pooled, l.Billable)
	l.Amount = new(big.Rat)
	for i := range l.Parts {
		part := &l.Parts[i]
		if part.Billable != nil && part.Billable.Sign() > 0 {
			part.Pooled = new(big.Rat).Mul(new(big.Rat).Mul(part.Billable, part.Share), of)
			part.Amount = new(big.Rat).Mul(part.Amount, left)
		}
		l.Amount.Add(l.Amount, part.Amount)
	}
}

// pieces is [from, to) cut where one of `as` begins or ends inside it: the
// stretches through which the same allowances are in force, one after
// another. With none that begins or ends inside it, it is the one stretch.
func pieces(from, to time.Time, as []Allowance) []Stretch {
	cuts := []time.Time{from}
	cut := func(t time.Time) {
		if t.After(from) && t.Before(to) {
			cuts = append(cuts, t)
		}
	}
	for _, a := range as {
		cut(a.From)
		if !a.Until.IsZero() {
			cut(a.Until)
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)

	out := make([]Stretch, 0, len(cuts))
	for i, at := range cuts {
		end := to
		if i+1 < len(cuts) {
			end = cuts[i+1]
		}
		out = append(out, Stretch{From: at, To: end})
	}

	return out
}

// allowed is what the allowances of `as` in force at `at` give together, and
// which they are, by when they begin and then by their identifiers.
func allowed(as []Allowance, at time.Time) (int64, []pdid.Id, error) {
	in := []Allowance{}
	for _, a := range as {
		if a.Quantity > 0 && a.in(at) {
			in = append(in, a)
		}
	}
	slices.SortFunc(in, func(a, b Allowance) int {
		if c := a.From.Compare(b.From); c != 0 {
			return c
		}
		return slices.Compare(a.Id[:], b.Id[:])
	})

	var (
		given int64
		ids   []pdid.Id
	)
	for _, a := range in {
		if given > math.MaxInt64-a.Quantity {
			return 0, nil, ErrTooMuch
		}
		given += a.Quantity
		ids = append(ids, a.Id)
	}

	return given, ids, nil
}

// elsewhere is what of [from, to) a version of `line`'s meter was in force
// on another line of `book` -- another channel or currency -- for a
// customer since `since`, as stretches one after another.
func elsewhere(book pricing.Book, line pricing.Line, since, from, to time.Time) []Stretch {
	if !from.Before(to) {
		return nil
	}

	others := map[pricing.Line]bool{}
	for _, v := range book {
		if l := v.Line(); l.Meter == line.Meter && l != line {
			others[l] = true
		}
	}

	var in []Stretch
	for l := range others {
		for _, s := range book.Timeline(l, since, from, to) {
			if s.Priced {
				in = append(in, Stretch{From: s.From, To: s.To})
			}
		}
	}
	if len(in) == 0 {
		return nil
	}

	slices.SortFunc(in, func(a, b Stretch) int { return a.From.Compare(b.From) })
	out := []Stretch{in[0]}
	for _, s := range in[1:] {
		last := &out[len(out)-1]
		if !s.From.After(last.To) {
			last.To = latest(last.To, s.To)
			continue
		}
		out = append(out, s)
	}

	return out
}

// outside is what of `ss` is in none of `away`.
func outside(ss []Stretch, away []Stretch) []Stretch {
	if len(away) == 0 {
		return ss
	}

	var out []Stretch
	for _, s := range ss {
		out = append(out, usage.Open(s.From, s.To, away)...)
	}

	return out
}

// banded is a part of a stock's period priced by bands: the time each value
// was held in it, at the band of this part's version that value is in.
func banded(v pricing.Version, b pricing.Banded, u Usage, s pricing.Segment, p Period) (Part, error) {
	part := Part{Version: v.Id, From: s.From, To: s.To, Share: new(big.Rat), Billable: new(big.Rat), Amount: new(big.Rat)}

	in := shares(u.Held, s.From, s.To, p)
	if len(in) == 0 {
		return part, nil
	}
	exact, err := v.Exact(in)
	if err != nil {
		return Part{}, err
	}
	part.Amount = exact

	at := map[int]int{}
	for _, sh := range in {
		band := b.Band(sh.Value)
		i, ok := at[bandKey(band)]
		if !ok {
			i = len(part.Bands)
			at[bandKey(band)] = i
			part.Bands = append(part.Bands, Band{UpTo: band.UpTo, Bounded: band.Bounded, Fee: band.Fee, Share: new(big.Rat), Amount: new(big.Rat)})
		}
		part.Bands[i].Share.Add(part.Bands[i].Share, sh.Of)
		fee := new(big.Rat).SetInt64(band.Fee)
		part.Bands[i].Amount.Add(part.Bands[i].Amount, fee.Mul(fee, sh.Of))
		part.Share.Add(part.Share, sh.Of)
	}

	return part, nil
}

// bandKey tells a price's bands apart: by their bound, the last one by none.
func bandKey(b pricing.Band) int {
	if !b.Bounded {
		return -1
	}
	return int(b.UpTo)
}

// ErrTooMuch is a total past what an int64 of minor units holds -- or what a
// version includes and allowances give past what an int64 of a meter holds.
var ErrTooMuch = errors.New("rating: the total is more than an amount holds")

// Total is amounts added exactly, and that rounded once, down, to a whole
// minor unit: what a statement comes to.
func Total(amounts ...*big.Rat) (*big.Rat, int64, error) {
	exact := new(big.Rat)
	for _, a := range amounts {
		if a != nil {
			exact.Add(exact, a)
		}
	}

	n := Floor(exact)
	if !n.IsInt64() {
		return nil, 0, ErrTooMuch
	}

	return exact, n.Int64(), nil
}

// Floor is the greatest whole number not more than r: down, for what is owed
// and for what is owed back alike.
func Floor(r *big.Rat) *big.Int {
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() < 0 {
		q.Sub(q, big.NewInt(1))
	}

	return q
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
