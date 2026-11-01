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
package rating

import (
	"errors"
	"fmt"
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
const Version = 3

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
	// neither, and its bands say what it came to.
	Included *big.Rat
	Billable *big.Rat

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

// Part is one stretch of a period priced by one version.
type Part struct {
	Version pdid.Id
	From    time.Time
	To      time.Time

	// Share is what of the period the part is charged by.
	Share *big.Rat

	// By the unit: what the version includes, and what was billable past it.
	Included int64
	Billable *big.Rat

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
		Meter:    m,
		Quantity: Quantity(m, u, p),
		Included: new(big.Rat),
		Billable: new(big.Rat),
		Amount:   new(big.Rat),
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

		// The part's share of the time the workspace was a customer in the
		// period: its whole usage, at this part's version, for that share.
		w := new(big.Rat).Quo(usage.Length(usage.Open(latest(s.From, since), s.To, c.Away)), customer)
		if w.Sign() == 0 {
			continue
		}
		exact, err := v.Exact(unit)
		if err != nil {
			return Line{}, fmt.Errorf("rating: %s: %w", m.Key, err)
		}

		part := Part{
			Version:  v.Id,
			From:     s.From,
			To:       s.To,
			Share:    w,
			Billable: new(big.Rat),
			Amount:   new(big.Rat).Mul(exact, w),
		}
		if pu, ok := v.Rate.(pricing.PerUnit); ok {
			part.Included = pu.Included
			part.Billable.Sub(out.Quantity, new(big.Rat).SetInt64(pu.Included))
			if part.Billable.Sign() < 0 {
				part.Billable.SetInt64(0)
			}
			out.Included.Add(out.Included, new(big.Rat).Mul(new(big.Rat).SetInt64(pu.Included), w))
			out.Billable.Add(out.Billable, new(big.Rat).Mul(part.Billable, w))
		}

		out.Parts = append(out.Parts, part)
		out.Amount.Add(out.Amount, part.Amount)
	}

	return out, nil
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

// ErrTooMuch is a total past what an int64 of minor units holds.
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
