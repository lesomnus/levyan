package paddle

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
)

// What each answer says happened: Paddle's code where it is one levyan
// reads, its status where it is not, and never a 5xx's code.
func TestAnAnswerIsReadForWhatItSaysHappened(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   mor.Kind
	}{
		{400, "subscription_payment_declined", mor.Declined},
		{400, "subscription_missing_payment_method_cannot_update", mor.Declined},
		{400, "invalid_field", mor.Invalid},
		{400, "subscription_update_when_past_due", mor.Invalid},
		{400, "subscription_update_transaction_balance_less_than_charge_limit", mor.Invalid},
		{400, "adjustment_amount_above_remaining_allowed", mor.Invalid},
		{403, "invalid_token", mor.Auth},
		{403, "forbidden", mor.Auth},
		{400, "subscription_product_tax_category_not_approved", mor.Auth},
		{429, "too_many_requests", mor.RateLimited},
		{429, "subscription_immediate_charge_hour_limit_exceeded", mor.RateLimited},
		{409, "subscription_locked_renewal", mor.RateLimited},
		{400, "adjustment_pending_refund_request", mor.RateLimited},
		{404, "not_found", mor.NotFound},
		{409, "concurrent_modification", mor.Unknown},

		// A 5xx says it does not know, whatever its code says it knows.
		{500, "internal_error", mor.Unknown},
		{500, "subscription_payment_provider_unavailable", mor.Unknown},
		{500, "subscription_payment_declined", mor.Unknown},
		{540, "temporarily_unavailable", mor.Unknown},
		{502, "", mor.Unknown},

		// A code levyan does not read yet is read by its status, and kept.
		{400, "a_code_paddle_adds_tomorrow", mor.Invalid},
		{401, "", mor.Auth},
		{403, "", mor.Auth},
		{404, "", mor.NotFound},
		{409, "", mor.Unknown},
		{408, "", mor.Unknown},
	} {
		require.Equal(t, tc.want, kind(tc.status, tc.code), "%d %s", tc.status, tc.code)
	}
}

func TestAnErrorObjectIsReadForItsCode(t *testing.T) {
	e := refusal("charge", 400, []byte(`{
		"error": {"type": "request_error", "code": "subscription_update_when_past_due",
			"detail": "Cannot update subscription, as the subscription status is 'past_due'.",
			"documentation_url": "https://developer.paddle.com/errors/subscriptions/subscription_update_when_past_due"},
		"meta": {"request_id": "9346b365-4cad-43a6-b7c1-48ff6a1c7836"}}`), "")
	require.Equal(t, mor.Invalid, e.Kind)
	require.Equal(t, "subscription_update_when_past_due", e.Code)
	require.Equal(t, "9346b365-4cad-43a6-b7c1-48ff6a1c7836", e.Request)
	require.ErrorIs(t, e, mor.ErrPastDue, "said in words that name no MoR")

	e = refusal("charge", 400, []byte(`{"error": {"code": "invalid_field", "detail": "Request does not pass validation.",
		"errors": [{"field": "items[0].price.description", "message": "minimum length of 2 not met"}]}}`), "")
	require.Equal(t, "Request does not pass validation.; items[0].price.description: minimum length of 2 not met", e.Message)

	e = refusal("charge", 502, []byte(`<html>Bad Gateway</html>`), "")
	require.Equal(t, &mor.Error{Op: "charge", Kind: mor.Unknown, Status: 502}, e)

	e = refusal("charge", 403, []byte(`{"error": {"code": "forbidden", "detail": "the key pdl_x may not"}}`), "pdl_x")
	require.Equal(t, "the key [api key] may not", e.Message)
}

func TestAnAmountIsAWholeNumberOfTheLowestDenomination(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "1000": 1000, "-300": -300} {
		got, err := amount(in, "USD")
		require.NoError(t, err, in)
		require.Equal(t, money.Amount{Minor: want, Currency: "USD"}, got, in)
	}
	for _, in := range []string{"10.00", "1e3", "dollars"} {
		_, err := amount(in, "USD")
		require.Error(t, err, in)
	}
}

func TestATimeIsPaddlesInUTC(t *testing.T) {
	got, err := when("2024-04-12T10:18:50.155553Z")
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, 4, 12, 10, 18, 50, 155553000, time.UTC), got)
	got, err = when("")
	require.NoError(t, err)
	require.True(t, got.IsZero())
	_, err = when("yesterday")
	require.Error(t, err)
}

func TestIdentifiersAreTheShapesPaddleWritesAndLevyanWrites(t *testing.T) {
	require.True(t, idOK("txn", "txn_01hv8wptq8987qeep44cyrewp9"))
	require.False(t, idOK("txn", "sub_01hv8wptq8987qeep44cyrewp9"))
	require.False(t, idOK("txn", "txn_01HV8WPTQ8987QEEP44CYREWP9"))
	require.False(t, idOK("txn", "txn_01hv8wptq8987qeep44cyrew"))
	require.False(t, idOK("sub", "sub_../../transactions/abcd"))

	require.True(t, keyOK("stmt-0193e1f2-7a3c-7d4e-9f10-2b3c4d5e6f70"), "a payday id is a key")
	require.True(t, keyOK("refund:2026-10.1=x_y"))
	require.False(t, keyOK(""))
	require.False(t, keyOK("a key"))
	require.False(t, keyOK("키"))
	require.False(t, keyOK("a]b"))

	for _, k := range []string{"refund-0001", "a:b.c=d_e"} {
		reason, key := unmark(mark("정정 청구 [2026-10]", k))
		require.Equal(t, "정정 청구 [2026-10]", reason)
		require.Equal(t, k, key)
	}
	for _, s := range []string{"error", "dispute [lv:]", "made in the dashboard [lv:a key]"} {
		reason, key := unmark(s)
		require.Equal(t, s, reason, "a reason levyan did not write is the reason")
		require.Empty(t, key)
	}
}

func TestATransactionIsReadAsLevyanHasOne(t *testing.T) {
	var tx transaction
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "txn_00000000000000000000000001", "status": "completed",
		"customer_id": "ctm_00000000000000000000000001", "subscription_id": "sub_00000000000000000000000001",
		"currency_code": "USD", "origin": "subscription_recurring",
		"items": [
			{"price": {"id": "pri_00000000000000000000000001", "custom_data": null}},
			{"price": {"id": "pri_00000000000000000000000002", "custom_data": {"levyan_key": "by-price"}}}
		],
		"details": {
			"totals": {"subtotal": "15000", "tax": "1500", "total": "16500", "grand_total": "16500"},
			"line_items": [
				{"id": "txnitm_00000000000000000000000001", "price_id": "pri_00000000000000000000000001",
				 "totals": {"subtotal": "0", "tax": "0", "total": "0"}, "product": {"name": "plan", "custom_data": null}},
				{"id": "txnitm_00000000000000000000000002", "price_id": "pri_00000000000000000000000002",
				 "totals": {"subtotal": "10000", "tax": "1000", "total": "11000"}, "product": {"name": "loft", "custom_data": null}},
				{"id": "txnitm_00000000000000000000000003", "price_id": "pri_00000000000000000000000003",
				 "totals": {"subtotal": "5000", "tax": "500", "total": "5500"}, "product": {"name": "loft", "custom_data": {"levyan_key": "by-product"}}}
			]},
		"payments": [
			{"status": "captured", "error_code": null, "captured_at": "2026-10-31T10:00:00.000000Z", "created_at": "2026-10-31T10:00:00.000000Z"},
			{"status": "error", "error_code": "declined", "captured_at": null, "created_at": "2026-10-30T10:00:00.000000Z"}
		],
		"adjustments": [
			{"id": "adj_00000000000000000000000002", "action": "chargeback", "transaction_id": "txn_00000000000000000000000001",
			 "reason": "fraudulent [lv:not-ours]", "status": "approved", "currency_code": "USD", "created_at": "2026-11-02T00:00:00Z",
			 "totals": {"total": "5500"}},
			{"id": "adj_00000000000000000000000001", "action": "refund", "transaction_id": "txn_00000000000000000000000001",
			 "reason": "정정 [lv:refund-1]", "status": "pending_approval", "currency_code": "USD", "created_at": "2026-11-01T00:00:00Z",
			 "totals": {"total": "1100"}}
		],
		"aFieldPaddleAddsTomorrow": {"is": "ignored"}
	}`), &tx))
	v, err := tx.read("external")
	require.NoError(t, err)
	require.Equal(t, mor.Paid, v.Status)
	require.Equal(t, time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC), v.At)
	require.Nil(t, v.Failure, "an attempt that failed before one that was captured is no failure")
	require.Equal(t, money.Amount{Minor: 16500, Currency: "USD"}, v.Total)
	require.Len(t, v.Lines, 3)
	require.Equal(t, "", v.Lines[0].Key, "the plan is no charge of levyan's")
	l, ok := v.Line("by-price")
	require.True(t, ok)
	require.Equal(t, money.Amount{Minor: 10000, Currency: "USD"}, l.Amount)
	l, ok = v.Line("by-product")
	require.True(t, ok)
	require.Equal(t, "txnitm_00000000000000000000000003", l.ID)

	require.Len(t, v.Adjustments, 2)
	require.Equal(t, "adj_00000000000000000000000001", v.Adjustments[0].ID, "oldest first")
	require.Equal(t, mor.RefundAdjustment, v.Adjustments[0].Kind)
	require.Equal(t, mor.Requested, v.Adjustments[0].Status)
	require.Equal(t, "refund-1", v.Adjustments[0].Key)
	require.Equal(t, "정정", v.Adjustments[0].Reason)
	require.Equal(t, mor.Chargeback, v.Adjustments[1].Kind)
	require.Equal(t, "", v.Adjustments[1].Key, "a chargeback is never one of levyan's refunds")
	require.Equal(t, 2+(1+0)+(1+1), v.Progress())

	internal, err := tx.read("internal")
	require.NoError(t, err)
	l, _ = internal.Line("by-product")
	require.Equal(t, money.Amount{Minor: 5500, Currency: "USD"}, l.Amount, "with tax in it, under the internal tax mode")

	for status, want := range map[string]mor.Status{"draft": mor.Pending, "ready": mor.Pending, "billed": mor.Pending, "paid": mor.Paid, "canceled": mor.Failed} {
		v, err := transaction{ID: "txn_00000000000000000000000001", Status: status, CurrencyCode: "USD"}.read("external")
		require.NoError(t, err)
		require.Equal(t, want, v.Status, status)
	}

	due := transaction{Status: "past_due", CurrencyCode: "USD"}
	code := "not_enough_balance"
	due.Payments = append(due.Payments, payment{Status: "error", ErrorCode: &code})
	v, err = due.read("external")
	require.NoError(t, err)
	require.Equal(t, mor.Pending, v.Status, "Paddle is still collecting it")
	require.Equal(t, &mor.Failure{Kind: mor.Declined, Code: "not_enough_balance"}, v.Failure)

	gone, err := transaction{Status: "canceled", CurrencyCode: "USD"}.read("external")
	require.NoError(t, err)
	require.Equal(t, mor.Unknown, gone.Failure.Kind, "canceled, and nothing said of the card")

	_, err = transaction{Status: "a_status_paddle_adds_tomorrow"}.read("external")
	require.Error(t, err, "a status levyan cannot place is no transaction to act on")
}

func TestASubscriptionIsReadAsLevyanMirrorsOne(t *testing.T) {
	var s subscription
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "sub_00000000000000000000000001", "status": "active", "customer_id": "ctm_00000000000000000000000001",
		"currency_code": "EUR", "updated_at": "2026-10-31T10:00:00.000000Z", "next_billed_at": null, "canceled_at": null,
		"current_billing_period": {"starts_at": "2026-10-15T00:00:00Z", "ends_at": "2026-11-15T00:00:00Z"},
		"scheduled_change": {"action": "cancel", "effective_at": "2026-11-15T00:00:00Z", "resume_at": null},
		"custom_data": {"levyan_account": "acct-0001", "other": 3}
	}`), &s))
	v, err := s.read()
	require.NoError(t, err)
	require.Equal(t, mor.Subscription{
		ID:       "sub_00000000000000000000000001",
		Customer: "ctm_00000000000000000000000001",
		Account:  "acct-0001",
		Status:   mor.Active,
		Currency: "EUR",
		Period:   mor.Period{Start: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		Change:   &mor.Change{Kind: mor.CancelAtEnd, At: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		Updated:  time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC),
	}, v)
	require.False(t, v.Renews())

	s.ScheduledChange.Action = "a_change_paddle_adds_tomorrow"
	v, err = s.read()
	require.NoError(t, err)
	require.Equal(t, mor.CancelAtEnd, v.Change.Kind, "a change levyan cannot place is read as one that ends it")

	_, err = subscription{ID: "sub_00000000000000000000000001", Status: "a_status_paddle_adds_tomorrow"}.read()
	require.Error(t, err)
}

func TestAnAdjustmentLevyanCannotPlaceIsNoAdjustmentToActOn(t *testing.T) {
	_, err := adjustment{Status: "a_status_paddle_adds_tomorrow"}.read()
	require.Error(t, err)
	a, err := adjustment{Action: "credit", Status: "approved", CurrencyCode: "USD", Totals: totals{Total: "100"}}.read()
	require.NoError(t, err)
	require.Equal(t, mor.OtherAdjustment, a.Kind)
	require.True(t, errors.Is(sentinels["subscription_update_when_canceled"], mor.ErrEnded))
}
