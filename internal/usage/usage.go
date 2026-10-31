// Package usage is what a stretch of time held or did of a meter, from the
// rows of the ledger: the readings a stock was measured by, the points a
// flow's series were counted by.
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
package usage

import (
	"errors"
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
