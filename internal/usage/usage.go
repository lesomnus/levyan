// Package usage is what a stretch of time held or did of a meter, from the
// rows of the ledger: the readings a stock was measured by, the points a
// flow's series were counted by.
//
// # Stocks
//
// A reading says what a workspace held at a moment, and it goes on saying it
// until the next reading -- for at most [Hold], so that a workspace whose
// readings stopped is not held to its last one for ever ([Profile]). What a
// period held is then, as its meter aggregates it: the average over the whole
// period ([Average]), the most of its readings ([Most]), or its last ([Last]).
// A price by bands reads the time spent at each value instead.
//
// It is pure. No database and no clock: it is handed the rows and the stretch,
// and answers the same however often it is asked -- which is what a closed
// period asked again later relies on. What a period's usage costs is
// `internal/pricing`'s.
//
// # Flows
//
// A flow is counted where it happens, as an OpenTelemetry Sum, monotonic and
// cumulative: each point of a series is the whole count from the series'
// start to the point's moment. So what a series did over a stretch is the
// increase its points show there, each increase counted at the point it was
// seen at ([Increase]):
//
//   - A start that moves is the count beginning again from nothing -- the
//     process restarted -- and the first point under it is all increase.
//   - Under one start, a point counts the increase over the most any point
//     before it counted, and never less than nothing: a count that went down
//     without its start moving is no count anybody believes, and when in
//     doubt levyan charges less.
//   - Points are taken in the order of their moments, whatever order they
//     came in, and one sent twice is one point.
//
// So every increase is counted once and in one stretch: what a series counted
// over two stretches is what it counted over each, added.
//
// # Away
//
// A workspace is no customer while it is closed, nor from its destruction on
// (#14): what it held then is held by nobody who is billed, and what its
// series counted then is counted for nobody. Those stretches are taken out of
// what a stretch held ([Cut]) and of where a flow is counted ([Open]); a
// reading measured in one is none of the period's ([In]). The time it was
// away counts as nothing, as the time before it began does.
package usage

import (
	"errors"
	"math/big"
	"slices"
	"time"
)

// Hold is how long a reading of a stock speaks for what it read when no later
// one comes: a reading holds until the next one, and at most this long. A
// workspace whose readings stopped is not billed for what it held after them
// for longer than that -- the gap the loop that looks for missing readings
// warns of (`levyan.readings.gaps`).
//
// It is part of what a period's usage is, and so of the rating's version: a
// period rated again is rated with the hold it was rated with first.
const Hold = 36 * time.Hour

// Point is one point of a flow's series: how much it had counted from Start
// to At.
type Point struct {
	Start time.Time
	At    time.Time
	Value int64
}

// ErrTooMuch is an increase past what an int64 holds.
var ErrTooMuch = errors.New("usage: the increase is more than a count holds")

// Increase is what a series counted over [from, to): the increases its points
// show there, each at the point it was seen at (see the package comment).
//
// `ps` is every point of the series that matters to the stretch, in any order:
// those in it, and before it whatever says how much each start had counted by
// then -- the most any point of a start counted before `from` is enough, which
// is what the ledger hands over (`server/ledger`).
func Increase(ps []Point, from, to time.Time) (int64, error) {
	ps = slices.Clone(ps)
	slices.SortFunc(ps, func(a, b Point) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return a.Start.Compare(b.Start)
	})

	most := map[time.Time]int64{}
	var total int64
	for _, p := range ps {
		if p.At.Compare(to) >= 0 {
			break
		}

		k := p.Start.UTC()
		was := most[k]
		if p.Value <= was {
			continue
		}
		most[k] = p.Value
		if p.At.Before(from) {
			continue
		}

		inc := p.Value - was
		if total > total+inc {
			return 0, ErrTooMuch
		}
		total += inc
	}

	return total, nil
}

// Reading is one reading of a stock: what was held at At.
type Reading struct {
	At    time.Time
	Value int64
}

// Held is one value a stock held, through [From, To).
type Held struct {
	From  time.Time
	To    time.Time
	Value int64
}

// Profile is what a stock held through [from, to), as its readings say: each
// reading's value from its moment until the next reading's, and for no longer
// than [Hold] after it. Time no reading speaks for is in no piece of it --
// before the first, and past a hold that ran out -- and so is nothing held.
//
// `until`, when it is not the zero time, carries the last of the readings to
// it whatever its hold: what an estimate takes, the latest a workspace was
// known to hold as what it will go on holding.
//
// The readings are in any order, and of one moment there is one (the ledger
// keeps one).
func Profile(rs []Reading, from, to, until time.Time) []Held {
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b Reading) int { return a.At.Compare(b.At) })

	out := []Held{}
	for i, r := range rs {
		end := r.At.Add(Hold)
		switch {
		case i+1 < len(rs):
			end = earliest(end, rs[i+1].At)
		case !until.IsZero():
			end = latest(end, until)
		}

		a, b := latest(r.At, from), earliest(end, to)
		if !a.Before(b) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(a) && out[n-1].Value == r.Value {
			out[n-1].To = b
			continue
		}
		out = append(out, Held{From: a, To: b, Value: r.Value})
	}

	return out
}

// Average is what was held on average through [from, to): each value weighed
// by how long it was held, over the whole stretch, the time nothing held
// counting as nothing -- so that a workspace that began a third of the way in
// held two thirds of what it held each day.
func Average(hs []Held, from, to time.Time) *big.Rat {
	whole := span(from, to)
	if whole.Sign() == 0 {
		return new(big.Rat)
	}

	sum := new(big.Rat)
	for _, h := range hs {
		v := new(big.Rat).SetInt64(h.Value)
		sum.Add(sum, v.Mul(v, span(h.From, h.To)))
	}

	return sum.Quo(sum, whole)
}

// Most is the most of the readings measured in [from, to), and false when
// none was: what a period held at its most, as it was read then.
func Most(rs []Reading, from, to time.Time) (int64, bool) {
	var (
		most int64
		ok   bool
	)
	for _, r := range rs {
		if r.At.Before(from) || !r.At.Before(to) {
			continue
		}
		if !ok || r.Value > most {
			most, ok = r.Value, true
		}
	}

	return most, ok
}

// Last is the latest of the readings measured in [from, to), and false when
// none was: what a period held when it was last read.
func Last(rs []Reading, from, to time.Time) (int64, bool) {
	var (
		last Reading
		ok   bool
	)
	for _, r := range rs {
		if r.At.Before(from) || !r.At.Before(to) {
			continue
		}
		if !ok || r.At.After(last.At) {
			last, ok = r, true
		}
	}

	return last.Value, ok
}

// Share is the share of [from, to) that [a, b) is, exactly, in the
// microseconds the ledger keeps.
func Share(a, b, from, to time.Time) *big.Rat {
	whole := span(from, to)
	if whole.Sign() == 0 {
		return new(big.Rat)
	}

	return new(big.Rat).Quo(span(a, b), whole)
}

// Stretch is a stretch of time, [From, To).
type Stretch struct {
	From time.Time
	To   time.Time
}

// Open is what of [from, to) is in none of `away`, as stretches one after
// another: the time a workspace was a customer, when `away` is when it was
// not. `away` is in any order, and its stretches may overlap.
func Open(from, to time.Time, away []Stretch) []Stretch {
	if !from.Before(to) {
		return nil
	}

	away = slices.Clone(away)
	slices.SortFunc(away, func(a, b Stretch) int { return a.From.Compare(b.From) })

	out := []Stretch{}
	at := from
	for _, s := range away {
		if !s.From.Before(to) {
			break
		}
		if !s.From.Before(s.To) || !s.To.After(at) {
			continue
		}
		if s.From.After(at) {
			out = append(out, Stretch{From: at, To: s.From})
		}
		if at = latest(at, s.To); !at.Before(to) {
			return out
		}
	}
	if at.Before(to) {
		out = append(out, Stretch{From: at, To: to})
	}

	return out
}

// Cut is what `hs` held outside `away`: each piece with the time it was away
// taken out of it.
func Cut(hs []Held, away []Stretch) []Held {
	if len(away) == 0 {
		return hs
	}

	out := []Held{}
	for _, h := range hs {
		for _, s := range Open(h.From, h.To, away) {
			out = append(out, Held{From: s.From, To: s.To, Value: h.Value})
		}
	}

	return out
}

// In says whether `t` is in one of `ss`.
func In(t time.Time, ss []Stretch) bool {
	for _, s := range ss {
		if !t.Before(s.From) && t.Before(s.To) {
			return true
		}
	}

	return false
}

// Length is how long `ss` are in all, in microseconds: what a share of them
// is weighed by.
func Length(ss []Stretch) *big.Rat {
	out := new(big.Rat)
	for _, s := range ss {
		out.Add(out, span(s.From, s.To))
	}

	return out
}

// span is how long [a, b) is, in microseconds, and nothing when it is not a
// stretch.
func span(a, b time.Time) *big.Rat {
	if !a.Before(b) {
		return new(big.Rat)
	}

	return new(big.Rat).SetInt64(b.Sub(a).Microseconds())
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
