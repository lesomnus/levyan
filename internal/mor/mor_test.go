package mor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/mor"
)

// Of two observations of one charge, the later is the one further along:
// scheduled, then pending, then paid, then each adjustment requested and
// decided.
func TestAChargeNeverWalksBack(t *testing.T) {
	refund := func(s mor.AdjustmentStatus) mor.Adjustment {
		return mor.Adjustment{Kind: mor.RefundAdjustment, Status: s}
	}
	life := []mor.Transaction{
		{Status: mor.Scheduled},
		{Status: mor.Pending},
		{Status: mor.Paid},
		{Status: mor.Paid, Adjustments: []mor.Adjustment{refund(mor.Requested)}},
		{Status: mor.Paid, Adjustments: []mor.Adjustment{refund(mor.Approved)}},
		{Status: mor.Paid, Adjustments: []mor.Adjustment{refund(mor.Approved), refund(mor.Requested)}},
		{Status: mor.Paid, Adjustments: []mor.Adjustment{refund(mor.Approved), refund(mor.Rejected)}},
		{Status: mor.Paid, Adjustments: []mor.Adjustment{refund(mor.Reversed), refund(mor.Rejected)}},
	}
	for i := range life {
		for j := i + 1; j < len(life); j++ {
			require.Less(t, life[i].Progress(), life[j].Progress(), "%d, then %d", i, j)
		}
	}
	require.Equal(t, mor.Transaction{Status: mor.Paid}.Progress(), mor.Transaction{Status: mor.Failed}.Progress(),
		"paid and failed are each where a transaction ends up, and neither follows the other")
}

// A subscription renews only while it is billed and nothing is scheduled to
// end it: what a charge for the next period is refused by.
func TestASubscriptionRenewsOnlyWhileNothingEndsIt(t *testing.T) {
	next := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	require.True(t, mor.Subscription{Status: mor.Active, Next: next}.Renews())
	require.True(t, mor.Subscription{Status: mor.Trialing, Next: next}.Renews())
	for _, s := range []mor.Subscription{
		{Status: mor.Active},
		{Status: mor.Active, Next: next, Change: &mor.Change{Kind: mor.CancelAtEnd, At: next}},
		{Status: mor.Active, Next: next, Change: &mor.Change{Kind: mor.PauseAtEnd, At: next}},
		{Status: mor.PastDue, Next: next},
		{Status: mor.Paused, Next: next},
		{Status: mor.Canceled},
	} {
		require.False(t, s.Renews(), "%+v", s)
	}
}

func TestATransactionsLineAndAdjustmentAreFoundByKey(t *testing.T) {
	tx := mor.Transaction{
		Lines:       []mor.Line{{ID: "a"}, {ID: "b", Key: "stmt-1"}},
		Adjustments: []mor.Adjustment{{ID: "c"}, {ID: "d", Key: "refund-1"}},
	}
	l, ok := tx.Line("stmt-1")
	require.True(t, ok)
	require.Equal(t, "b", l.ID)
	_, ok = tx.Line("")
	require.False(t, ok, "a line of no key is nobody's charge")
	a, ok := tx.Adjustment("refund-1")
	require.True(t, ok)
	require.Equal(t, "d", a.ID)
	_, ok = tx.Adjustment("")
	require.False(t, ok)
}

// An error says what is known of what the request did, wrapped or not, and
// why by an Err of the package's; an error that is no mor.Error is not known
// to have done nothing.
func TestAnErrorSaysWhatIsKnown(t *testing.T) {
	e := &mor.Error{Op: "charge", Kind: mor.Invalid, Code: "subscription_update_when_past_due", Status: 400, Message: "Cannot update subscription.", Request: "r-1", Err: mor.ErrPastDue}
	require.Equal(t, mor.Invalid, mor.KindOf(e))
	require.Equal(t, mor.Invalid, mor.KindOf(fmt.Errorf("charging: %w", e)))
	require.ErrorIs(t, fmt.Errorf("charging: %w", e), mor.ErrPastDue)
	require.Equal(t, "mor: charge: invalid: subscription_update_when_past_due (400): Cannot update subscription.: the subscription is past due [request r-1]", e.Error())

	require.Equal(t, mor.Unknown, mor.KindOf(errors.New("a reset connection")))
	require.Equal(t, mor.Unknown, mor.Kind(0), "the zero Kind is the safe one")

	u := &mor.Error{Op: "charge", Kind: mor.Unknown, Err: context.DeadlineExceeded}
	require.ErrorIs(t, u, context.DeadlineExceeded)
	require.Equal(t, "mor: charge: unknown: context deadline exceeded", u.Error())
}

// What is said of a status is its name, and a status of none is said so.
func TestEveryStatusIsSaid(t *testing.T) {
	require.Equal(t, "scheduled", mor.Scheduled.String())
	require.Equal(t, "no status", mor.Status(0).String())
	require.Equal(t, "past due", mor.PastDue.String())
	require.Equal(t, "no status", mor.SubscriptionStatus(0).String())
	require.Equal(t, "requested", mor.Requested.String())
	require.Equal(t, "chargeback", mor.Chargeback.String())
	require.Equal(t, "next period", mor.NextPeriod.String())
	require.Equal(t, "no time", mor.When(0).String())
	require.Equal(t, "cancel", mor.CancelAtEnd.String())
	require.Equal(t, "subscription", mor.NoticeSubscription.String())
	require.Equal(t, "rate limited", mor.RateLimited.String())
}
