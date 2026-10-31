package usage_test

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/usage"
)

func at(h int) time.Time {
	return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
}

func p(start, h int, v int64) usage.Point { return usage.Point{Start: at(start), At: at(h), Value: v} }

func TestAFlowIncreasesByWhatItsPointsShow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ps       []usage.Point
		from, to int
		want     int64
	}{
		{"nothing", nil, 0, 24, 0},
		{"one point is all it counted since its start", []usage.Point{p(0, 1, 7)}, 0, 24, 7},
		{"each point counts what it adds", []usage.Point{p(0, 1, 7), p(0, 2, 10), p(0, 3, 15)}, 0, 24, 15},
		{"in any order", []usage.Point{p(0, 3, 15), p(0, 1, 7), p(0, 2, 10)}, 0, 24, 15},
		{"twice is once", []usage.Point{p(0, 1, 7), p(0, 1, 7), p(0, 2, 10), p(0, 2, 10)}, 0, 24, 10},
		{"a new start counts from nothing", []usage.Point{p(0, 1, 7), p(0, 2, 10), p(5, 6, 4), p(5, 7, 9)}, 0, 24, 19},
		{"a start that comes back is its own", []usage.Point{p(0, 1, 7), p(5, 6, 4), p(0, 7, 12)}, 0, 24, 16},
		{"down under one start is no count, and adds nothing after", []usage.Point{p(0, 1, 10), p(0, 2, 6), p(0, 3, 12)}, 0, 24, 12},
		{"what was counted before the stretch is not in it", []usage.Point{p(0, 1, 10), p(0, 5, 14), p(0, 30, 20)}, 2, 24, 4},
		{"and what comes after is not either", []usage.Point{p(0, 1, 10), p(0, 30, 20)}, 0, 24, 10},
		{"an increase is in the stretch its point is", []usage.Point{p(0, 23, 10), p(0, 25, 30)}, 24, 48, 20},
		{"the end is not in it", []usage.Point{p(0, 24, 10)}, 0, 24, 0},
		{"the start is", []usage.Point{p(0, 24, 10)}, 24, 48, 10},
		{"a start before the stretch counts from what it had by the stretch", []usage.Point{p(-48, -1, 100), p(-48, 2, 130)}, 0, 24, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := usage.Increase(tc.ps, at(tc.from), at(tc.to))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// Two stretches one after the other come to what the two together do: every
// increase is counted once, in one of them.
func TestEveryIncreaseIsCountedOnce(t *testing.T) {
	ps := []usage.Point{p(0, 1, 5), p(0, 20, 9), p(0, 26, 11), p(30, 31, 3), p(0, 33, 8), p(30, 47, 20), p(0, 50, 30)}
	whole, err := usage.Increase(ps, at(0), at(72))
	require.NoError(t, err)

	var parts int64
	for _, cut := range [][2]int{{0, 24}, {24, 48}, {48, 72}} {
		n, err := usage.Increase(ps, at(cut[0]), at(cut[1]))
		require.NoError(t, err)
		parts += n
	}
	require.Equal(t, whole, parts)
	require.Equal(t, int64(30+20), whole, "the most of each start")
}

func TestAnIncreaseNoCountHoldsIsRefused(t *testing.T) {
	_, err := usage.Increase([]usage.Point{p(0, 1, math.MaxInt64), p(5, 6, 1)}, at(0), at(24))
	require.ErrorIs(t, err, usage.ErrTooMuch)
}

func r(h int, v int64) usage.Reading { return usage.Reading{At: at(h), Value: v} }

func held(from, to int, v int64) usage.Held { return usage.Held{From: at(from), To: at(to), Value: v} }

// A reading speaks for its value until the next, for no longer than the hold,
// from before the stretch into it, and for nothing before the first.
func TestAReadingHoldsUntilTheNextAndNoLonger(t *testing.T) {
	none := time.Time{}
	for _, tc := range []struct {
		name     string
		rs       []usage.Reading
		from, to int
		until    time.Time
		want     []usage.Held
	}{
		{"nothing read is nothing held", nil, 0, 48, none, []usage.Held{}},
		{"until the next", []usage.Reading{r(0, 1), r(24, 2)}, 0, 48, none, []usage.Held{held(0, 24, 1), held(24, 48, 2)}},
		{"in any order", []usage.Reading{r(24, 2), r(0, 1)}, 0, 48, none, []usage.Held{held(0, 24, 1), held(24, 48, 2)}},
		{"and no longer than the hold", []usage.Reading{r(0, 1), r(100, 2)}, 0, 120, none, []usage.Held{held(0, 36, 1), held(100, 120, 2)}},
		{"nothing before the first", []usage.Reading{r(10, 1)}, 0, 24, none, []usage.Held{held(10, 24, 1)}},
		{"from before the stretch, into it", []usage.Reading{r(-12, 5), r(6, 7)}, 0, 24, none, []usage.Held{held(0, 6, 5), held(6, 24, 7)}},
		{"one too long before speaks for none of it", []usage.Reading{r(-40, 5)}, 0, 24, none, []usage.Held{}},
		{"one value read twice is one stretch", []usage.Reading{r(0, 3), r(12, 3), r(24, 3)}, 0, 48, none, []usage.Held{held(0, 48, 3)}},
		{"an estimate holds the last to its end", []usage.Reading{r(0, 1), r(12, 4)}, 0, 240, at(240), []usage.Held{held(0, 12, 1), held(12, 240, 4)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, usage.Profile(tc.rs, at(tc.from), at(tc.to), tc.until))
		})
	}
}

func TestWhatAStretchHeld(t *testing.T) {
	hs := []usage.Held{held(0, 6, 10), held(12, 24, 40)}

	require.Equal(t, big.NewRat(45, 2), usage.Average(hs, at(0), at(24)), "(10*6 + 40*12) / 24")
	require.Equal(t, big.NewRat(0, 1), usage.Average(nil, at(0), at(24)))

	rs := []usage.Reading{r(-1, 99), r(1, 3), r(5, 8), r(23, 2), r(24, 50)}
	v, ok := usage.Most(rs, at(0), at(24))
	require.True(t, ok)
	require.Equal(t, int64(8), v, "of its own readings")
	v, ok = usage.Last(rs, at(0), at(24))
	require.True(t, ok)
	require.Equal(t, int64(2), v)
	_, ok = usage.Most(rs, at(30), at(40))
	require.False(t, ok)

	require.Equal(t, big.NewRat(1, 4), usage.Share(at(0), at(6), at(0), at(24)))
}

func stretch(from, to int) usage.Stretch { return usage.Stretch{From: at(from), To: at(to)} }

// What of a stretch a workspace was a customer is what is in none of the
// stretches it was away, which may come in any order and overlap; and what
// it held is cut to that, the time it was away taken out (#14).
func TestTheTimeAwayIsTakenOut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to int
		away     []usage.Stretch
		want     []usage.Stretch
	}{
		{"never away is all of it", 0, 24, nil, []usage.Stretch{stretch(0, 24)}},
		{"away within it", 0, 24, []usage.Stretch{stretch(6, 12)}, []usage.Stretch{stretch(0, 6), stretch(12, 24)}},
		{"away from before it", 0, 24, []usage.Stretch{stretch(-6, 12)}, []usage.Stretch{stretch(12, 24)}},
		{"away past it", 0, 24, []usage.Stretch{stretch(12, 48)}, []usage.Stretch{stretch(0, 12)}},
		{"away all of it", 0, 24, []usage.Stretch{stretch(-1, 25)}, []usage.Stretch{}},
		{"in any order, overlapping", 0, 24, []usage.Stretch{stretch(18, 20), stretch(4, 8), stretch(6, 10)}, []usage.Stretch{stretch(0, 4), stretch(10, 18), stretch(20, 24)}},
		{"away outside it is nothing", 0, 24, []usage.Stretch{stretch(-10, -5), stretch(30, 40)}, []usage.Stretch{stretch(0, 24)}},
		{"away for no time is nothing", 0, 24, []usage.Stretch{stretch(6, 6)}, []usage.Stretch{stretch(0, 24)}},
		{"no stretch is nothing", 24, 24, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, usage.Open(at(tc.from), at(tc.to), tc.away))
		})
	}

	away := []usage.Stretch{stretch(6, 12)}
	require.Equal(t, []usage.Held{held(0, 6, 10), held(12, 18, 10), held(20, 24, 7)},
		usage.Cut([]usage.Held{held(0, 18, 10), held(20, 24, 7)}, away))
	require.Equal(t, []usage.Held{held(0, 24, 3)}, usage.Cut([]usage.Held{held(0, 24, 3)}, nil))

	require.True(t, usage.In(at(6), away), "its first moment is in it")
	require.False(t, usage.In(at(12), away), "its end is not")
	require.Equal(t, big.NewRat(int64(18*time.Hour/time.Microsecond), 1), usage.Length(usage.Open(at(0), at(24), away)))
}
