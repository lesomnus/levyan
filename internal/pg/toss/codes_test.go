package toss

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/pg"
)

// What each answer says happened: Toss's code where it is one levyan reads,
// its status where it is not, and never a 5xx's code.
func TestAnAnswerIsReadForWhatItSaysHappened(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   pg.Kind
	}{
		{403, "REJECT_CARD_PAYMENT", pg.Declined},
		{400, "INVALID_STOPPED_CARD", pg.Declined},
		{404, "NOT_FOUND_BILLING", pg.Declined},
		{400, "PAY_PROCESS_CANCELED", pg.Declined},
		{400, "BELOW_MINIMUM_AMOUNT", pg.Invalid},
		{403, "NOT_CANCELABLE_AMOUNT", pg.Invalid},
		{400, "NOT_MATCHES_CUSTOMER_KEY", pg.Invalid},
		{401, "UNAUTHORIZED_KEY", pg.Auth},
		{403, "API_KEY_ACCESS_DENIED", pg.Auth},
		{400, "NOT_SUPPORTED_METHOD", pg.Auth},
		{403, "FORBIDDEN_CONSECUTIVE_REQUEST", pg.RateLimited},
		{429, "", pg.RateLimited},
		{404, "NOT_FOUND_PAYMENT", pg.NotFound},
		{409, "IDEMPOTENT_REQUEST_PROCESSING", pg.Unknown},
		{400, "ALREADY_PROCESSING_REQUEST", pg.Unknown},
		{400, "PROVIDER_ERROR", pg.Unknown},

		// A 5xx says it does not know, whatever its code says it knows.
		{500, "FAILED_INTERNAL_SYSTEM_PROCESSING", pg.Unknown},
		{500, "REJECT_CARD_PAYMENT", pg.Unknown},
		{502, "", pg.Unknown},

		// A code levyan does not read yet is read by its status, and kept.
		{400, "A_CODE_TOSS_ADDS_TOMORROW", pg.Invalid},
		{403, "A_CODE_TOSS_ADDS_TOMORROW", pg.Invalid},
		{401, "", pg.Auth},
		{409, "", pg.Unknown},
		{408, "", pg.Unknown},
	} {
		require.Equal(t, tc.want, kind(tc.status, tc.code), "%d %s", tc.status, tc.code)
	}
}

func TestAnErrorObjectIsReadForItsCode(t *testing.T) {
	e := refusal("charge", 403, []byte(`{"code":"REJECT_CARD_PAYMENT","message":"한도초과 혹은 잔액부족으로 결제에 실패했습니다."}`))
	require.Equal(t, &pg.Error{Op: "charge", Kind: pg.Declined, Code: "REJECT_CARD_PAYMENT", Message: "한도초과 혹은 잔액부족으로 결제에 실패했습니다.", Status: 403}, e)

	e = refusal("payment", 404, []byte(`{"version":"2024-06-01","traceId":"t","error":{"code":"NOT_FOUND_PAYMENT","message":"존재하지 않는 결제 정보 입니다."}}`))
	require.Equal(t, pg.NotFound, e.Kind, "v2's error object")

	e = refusal("charge", 502, []byte(`<html>Bad Gateway</html>`))
	require.Equal(t, &pg.Error{Op: "charge", Kind: pg.Unknown, Status: 502}, e)
}

func TestAnAmountIsAWholeNumberOfWon(t *testing.T) {
	for in, want := range map[json.Number]int64{"": 0, "0": 0, "4900": 4900, "4900.0": 4900, "-300": -300} {
		got, err := minor(in, "KRW")
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []json.Number{"4900.5", "1e300", "won"} {
		_, err := minor(in, "KRW")
		require.Error(t, err, in)
	}
	_, err := minor("49.00", "USD")
	require.Error(t, err, "a currency of cents is not one Toss is read in here")
}

func TestATimeIsTossesWithItsZoneOrKST(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2022-01-01T00:00:00+09:00":  time.Date(2021, 12, 31, 15, 0, 0, 0, time.UTC),
		"2024-09-05T12:19:21+09:00":  time.Date(2024, 9, 5, 3, 19, 21, 0, time.UTC),
		"2022-01-01T00:00:00.000000": time.Date(2021, 12, 31, 15, 0, 0, 0, time.UTC),
		"2024-12-01T00:00:00.5":      time.Date(2024, 11, 30, 15, 0, 0, 5e8, time.UTC),
		"":                           {},
	} {
		got, err := when(in)
		require.NoError(t, err, in)
		require.True(t, want.Equal(got), "%s: %s", in, got)
	}
	_, err := when("yesterday")
	require.Error(t, err)
}

func TestIdentifiersAreTheShapesTossTakes(t *testing.T) {
	require.True(t, orderOK("order-0001"))
	require.True(t, orderOK("0193e1f2-7a3c-7d4e-9f10-2b3c4d5e6f70"), "a payday id is an order")
	require.False(t, orderOK("o-001"))
	require.False(t, orderOK("order 0001"))
	require.False(t, orderOK("주문-0001"))
	require.False(t, orderOK(string(make([]byte, 65))))

	require.True(t, customerOK("lv-ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	require.True(t, customerOK("account.id@billing-0000001"))
	require.False(t, customerOK("ABCDEFGHIJKLMNOPQRSTUVWXYZ"), "at least one that is not a letter or a digit")
	require.False(t, customerOK("lv-short"))
	require.False(t, customerOK("lv-ABCDEFGHIJKLMNOPQRSTUVWXYZ/"))

	require.True(t, idOK("5EnNZRJGvaBX7zk2yd8ydw26XvwXkLrx9POLqKQjmAw4b0e1", 200))
	require.False(t, idOK("a key", 200))
	require.False(t, idOK("..", 200))
	require.False(t, idOK("", 200))
	require.False(t, idOK("키", 200))
}

func TestAPaymentIsReadAsLevyanHasOne(t *testing.T) {
	var p payment
	require.NoError(t, json.Unmarshal([]byte(`{
		"paymentKey": "a-payment", "orderId": "order-0001", "status": "PARTIAL_CANCELED", "type": "BILLING",
		"currency": "KRW", "totalAmount": 10000, "balanceAmount": 6000.0,
		"approvedAt": "2026-10-31T10:00:00+09:00",
		"cancels": [
			{"cancelAmount": 3000, "cancelReason": "later", "canceledAt": "2026-10-31T12:00:00+09:00", "transactionKey": "t2", "cancelStatus": "DONE"},
			{"cancelAmount": 1000, "cancelReason": "first", "canceledAt": "2026-10-31T11:00:00+09:00", "transactionKey": "t1", "cancelStatus": "DONE"},
			{"cancelAmount": 5000, "cancelReason": "never", "canceledAt": "2026-10-31T13:00:00+09:00", "transactionKey": "t3", "cancelStatus": "ABORTED"}
		],
		"aFieldTossAddsTomorrow": {"is": "ignored"}
	}`), &p))
	v, err := p.read()
	require.NoError(t, err)
	require.Equal(t, pg.PartlyCanceled, v.Status)
	require.Equal(t, int64(10000), v.Amount.Minor)
	require.Equal(t, int64(6000), v.Balance.Minor)
	require.Equal(t, time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC), v.At)
	require.Len(t, v.Cancels, 2, "a cancellation that is not done gave nothing back")
	require.Equal(t, "t1", v.Cancels[0].ID, "oldest first")
	require.Equal(t, "t2", v.Cancels[1].ID)

	for code, want := range map[string]pg.Kind{"REJECT_CARD_PAYMENT": pg.Declined, "COMMON_ERROR": pg.Unknown} {
		p := payment{Status: "ABORTED", TotalAmount: "1000", BalanceAmount: "1000"}
		p.Failure = &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{code, "…"}
		v, err := p.read()
		require.NoError(t, err)
		require.Equal(t, pg.Failed, v.Status)
		require.Equal(t, want, v.Failure.Kind, code)
	}

	_, err = payment{Status: "A_STATUS_TOSS_ADDS_TOMORROW"}.read()
	require.Error(t, err, "a status levyan cannot place is no payment to act on")
}
