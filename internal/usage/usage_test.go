package usage_test

import (
	"math"
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
