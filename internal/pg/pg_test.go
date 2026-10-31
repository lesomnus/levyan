package pg_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/pg"
)

const theKey = "Z_t5vOvQxrj4499PeiJcjen28-V2RyqgYTwN44Rdzk0="

// A billing key is printed, logged and marshalled as nothing, alone or in
// the method it is a part of, and Reveal is the one way to it.
func TestABillingKeySaysNothingOfItself(t *testing.T) {
	k := pg.NewBillingKey(theKey)
	m := pg.Method{Customer: "lv-customer-0000000000000", Key: k, At: time.Now()}

	for _, s := range []string{
		fmt.Sprint(k),
		fmt.Sprintf("%v %+v %#v %s %q %x %X %d %10.3s", k, k, k, k, k, k, k, k, k),
		fmt.Sprintf("%v %+v %#v", m, m, m),
		fmt.Sprintf("%v %+v", &m, &m),
		fmt.Sprint([]pg.BillingKey{k}, map[string]pg.BillingKey{"k": k}),
		k.String(),
		k.GoString(),
	} {
		require.NotContains(t, s, theKey)
		require.Contains(t, s, "[billing key]")
	}

	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("a method", "key", k, "method", m)
	slog.New(slog.NewJSONHandler(&b, nil)).Info("a method", "key", k, "method", m, "group", slog.GroupValue(slog.Any("key", k)))
	require.NotContains(t, b.String(), theKey)

	_, err := json.Marshal(m)
	require.Error(t, err, "a key written out is refused, not written as a placeholder")
	require.NotContains(t, err.Error(), theKey)
	_, err = k.MarshalText()
	require.Error(t, err)

	require.Equal(t, theKey, k.Reveal())
	require.False(t, k.IsZero())
	require.True(t, pg.BillingKey{}.IsZero())
}

// The digest finds a key without holding it: the same for the same key, and
// nothing of the key in it.
func TestABillingKeysDigestIsItsOwnAndNotIt(t *testing.T) {
	k := pg.NewBillingKey(theKey)
	require.Equal(t, k.Digest(), pg.Digest(theKey))
	require.Equal(t, k.Digest(), pg.NewBillingKey(theKey).Digest())
	require.NotEqual(t, k.Digest(), pg.NewBillingKey(theKey+"x").Digest())
	require.NotContains(t, k.Digest(), theKey)
	require.Len(t, k.Digest(), 64)
}

// Of two observations of one payment, the later is the one further along:
// pending, then paid, then each cancellation, to all of it.
func TestAPaymentNeverWalksBack(t *testing.T) {
	cancels := func(n int) []pg.Cancellation { return make([]pg.Cancellation, n) }
	life := []pg.Payment{
		{Status: pg.Pending},
		{Status: pg.Paid},
		{Status: pg.PartlyCanceled, Cancels: cancels(1)},
		{Status: pg.PartlyCanceled, Cancels: cancels(2)},
		{Status: pg.Canceled, Cancels: cancels(3)},
	}
	for i := range life {
		for j := i + 1; j < len(life); j++ {
			require.Less(t, life[i].Progress(), life[j].Progress(), "%s, then %s", life[i].Status, life[j].Status)
		}
	}
	require.Equal(t, pg.Payment{Status: pg.Paid}.Progress(), pg.Payment{Status: pg.Failed}.Progress(),
		"paid and failed are each where a payment ends up, and neither follows the other")
}

// An error says what is known of what the request did, wrapped or not; an
// error that is no pg.Error is not known to have done nothing.
func TestAnErrorSaysWhatIsKnown(t *testing.T) {
	e := &pg.Error{Op: "charge", Kind: pg.Declined, Code: "REJECT_CARD_PAYMENT", Status: 403, Message: "한도초과 혹은 잔액부족으로 결제에 실패했습니다."}
	require.Equal(t, pg.Declined, pg.KindOf(e))
	require.Equal(t, pg.Declined, pg.KindOf(fmt.Errorf("charging: %w", e)))
	require.Equal(t, pg.Unknown, pg.KindOf(errors.New("a reset connection")))
	require.Equal(t, pg.Unknown, pg.Kind(0), "the zero Kind is the safe one")
	require.Equal(t, "pg: charge: declined: REJECT_CARD_PAYMENT (403): 한도초과 혹은 잔액부족으로 결제에 실패했습니다.", e.Error())

	cause := errors.New("context deadline exceeded")
	u := &pg.Error{Op: "charge", Kind: pg.Unknown, Err: cause}
	require.ErrorIs(t, u, cause)
	require.Equal(t, "pg: charge: unknown: context deadline exceeded", u.Error())
}
