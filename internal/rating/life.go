package rating

import (
	"cmp"
	"slices"
	"time"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/usage"
)

// Forever is the end of a stretch that does not end: a workspace's from its
// destruction on, and from a close nothing has restored yet. Far past
// anything billed, and near enough that the time to it from now is a
// time.Duration still.
var Forever = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)

// Event is what a product said became of a workspace at a moment
// (`WorkspaceEvent`, #14).
type Event struct {
	Kind levyan.WorkspaceEventKind
	At   time.Time
}

// Life is what a workspace's events say of it.
type Life struct {
	// Away is the stretches it was no customer: from each close to the
	// restore after it, and from its destruction on, each in the order of
	// its moments. A close nothing has restored is away until [Forever].
	Away []Stretch

	// Closed is since when it has been closed -- now, or when it was
	// destroyed -- and zero for one that is open.
	Closed time.Time

	// Destroyed is when it was destroyed, for good, and zero for one that was
	// not.
	Destroyed time.Time
}

// Lived is the life `es` say, in any order and each once (levyan keeps one
// event of a workspace's moment). They are read in the order of their
// moments, never of their coming, so that what was said late takes its
// place, and an older event never undoes a newer one:
//
//   - a close is away from its moment, and a close of what is closed
//     changes nothing;
//   - a restore is back from its moment, and a restore of what is open
//     changes nothing;
//   - a destruction is away from its moment for good -- from the close it
//     came in, for one destroyed closed -- and nothing said after it changes
//     anything.
//
// Two of one moment, which levyan does not keep, are read a close before a
// restore, so that a workspace closed and restored at once was never away:
// when in doubt, less.
func Lived(es []Event) Life {
	es = slices.Clone(es)
	slices.SortFunc(es, func(a, b Event) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind, b.Kind)
	})

	var out Life
	away := func(from, to time.Time) {
		if from.Before(to) {
			out.Away = append(out.Away, Stretch{From: from, To: to})
		}
	}
	for _, e := range es {
		switch e.Kind {
		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_CLOSED:
			if out.Closed.IsZero() {
				out.Closed = e.At
			}

		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED:
			if !out.Closed.IsZero() {
				away(out.Closed, e.At)
				out.Closed = time.Time{}
			}

		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED:
			from := e.At
			if !out.Closed.IsZero() {
				from = out.Closed
			}
			away(from, Forever)
			out.Destroyed = e.At
			return out
		}
	}
	if !out.Closed.IsZero() {
		away(out.Closed, Forever)
	}

	return out
}

// Through says whether the workspace was away for the whole of `p`: a
// customer of none of it.
func (l Life) Through(p Period) bool {
	return p.Start.Before(p.End) && len(usage.Open(p.Start, p.End, l.Away)) == 0
}
