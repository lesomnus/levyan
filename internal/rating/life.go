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
	// Away is the stretches it was no customer, to [Forever]: from a close
	// nothing has restored -- still closed, or destroyed closed -- or from
	// its destruction, for one destroyed open. A stretch closed and then
	// restored is no stretch away (the owner's, 2026-11-01), so there is one
	// at most.
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
//   - a close is away from its moment, unless a restore comes after it: a
//     stretch closed and restored is billed as any open time is, and only a
//     stretch still closed, or one that ended in a destruction, is not (the
//     owner's, 2026-11-01). A close of what is closed changes nothing;
//   - a restore ends the stretch the close before it began, which is then no
//     stretch away; a restore of what is open changes nothing;
//   - a destruction is away from its moment for good -- from the close it
//     came in, for one destroyed closed -- and nothing said after it changes
//     anything.
//
// Two of one moment, which levyan does not keep, are read a close before a
// restore, so that a workspace closed and restored at once was never closed.
func Lived(es []Event) Life {
	var out Life
	for _, e := range inOrder(es) {
		switch e.Kind {
		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_CLOSED:
			if out.Closed.IsZero() {
				out.Closed = e.At
			}

		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED:
			out.Closed = time.Time{}

		case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED:
			from := e.At
			if !out.Closed.IsZero() {
				from = out.Closed
			}
			out.Away = []Stretch{{From: from, To: Forever}}
			out.Destroyed = e.At
			return out
		}
	}
	if !out.Closed.IsZero() {
		out.Away = []Stretch{{From: out.Closed, To: Forever}}
	}

	return out
}

// Reach is from when what event `e` of a workspace whose events are `es` --
// with `e` among them, or not yet -- says can move what the workspace is
// billed for, as `es` say. Whether a stretch the workspace was closed for is
// billed is its end's to say (the owner's, 2026-11-01): restored, it is;
// still closed, or destroyed, it is not. So:
//
//   - a close is from its moment;
//   - a restore of a workspace closed just before it is from the close that
//     began that stretch, which it makes billed;
//   - a destruction of a workspace closed just before it is from that close
//     too, when a restore after it would have ended the stretch -- billed,
//     until the destruction said otherwise -- and from its own moment when
//     nothing would have: the stretch was not billed either way;
//   - anything else is from its own moment, and an event after the
//     workspace's destruction moves nothing.
//
// It is how far back an event said after a period it reaches into began to
// close is late (`server/billing`): a restore said today of a workspace
// closed since a closed period corrects that period.
func Reach(es []Event, e Event) time.Time {
	var (
		closed time.Time
		after  bool
	)
	all := inOrder(append(slices.Clone(es), e))
	for i, v := range all {
		if v.Kind != e.Kind || !v.At.Equal(e.At) {
			switch v.Kind {
			case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_CLOSED:
				if closed.IsZero() {
					closed = v.At
				}
			case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED:
				closed = time.Time{}
			case levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED:
				return e.At
			}
			continue
		}
		for _, w := range all[i+1:] {
			after = after || w.Kind == levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED
		}
		break
	}

	switch {
	case closed.IsZero():
		return e.At
	case e.Kind == levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED:
		return closed
	case e.Kind == levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED && after:
		return closed
	}

	return e.At
}

// inOrder is `es` in the order of their moments, a close before a restore of
// one moment.
func inOrder(es []Event) []Event {
	es = slices.Clone(es)
	slices.SortStableFunc(es, func(a, b Event) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind, b.Kind)
	})

	return es
}

// Through says whether the workspace was away for the whole of `p`: a
// customer of none of it.
func (l Life) Through(p Period) bool {
	return p.Start.Before(p.End) && len(usage.Open(p.Start, p.End, l.Away)) == 0
}
