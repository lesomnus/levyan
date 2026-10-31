package toss

import (
	"net/http"

	"github.com/lesomnus/levyan/internal/pg"
)

// codes is what each of Toss's error codes says happened, of the ones the
// calls this adapter makes can answer: the per-API tables and the full list of
// https://docs.tosspayments.com/reference/error-codes (2026-10-31), and the
// window's failUrl codes of
// https://docs.tosspayments.com/guides/v2/billing/integration.
//
// A code that is not here is read by its status ([kind]): Toss adds codes
// without a new API version (https://docs.tosspayments.com/reference/versioning),
// so the table is not all there is, and the code is always kept beside the
// kind for an operator to read.
var codes = map[string]pg.Kind{
	// The card's fault: nothing was charged, and the payer has something to
	// do -- another card, a word with the card company, or waiting for a limit.
	"REJECT_CARD_PAYMENT":                 pg.Declined, // 한도초과 혹은 잔액부족
	"REJECT_CARD_COMPANY":                 pg.Declined, // 결제 승인이 거절
	"REJECT_ACCOUNT_PAYMENT":              pg.Declined, // 잔액부족
	"REJECT_TOSSPAY_INVALID_ACCOUNT":      pg.Declined,
	"INVALID_REJECT_CARD":                 pg.Declined, // 카드 사용이 거절
	"INVALID_STOPPED_CARD":                pg.Declined, // 정지된 카드
	"INVALID_CARD_EXPIRATION":             pg.Declined,
	"INVALID_CARD_NUMBER":                 pg.Declined,
	"INVALID_CARD_LOST_OR_STOLEN":         pg.Declined,
	"INVALID_CARD_PASSWORD":               pg.Declined,
	"INVALID_CARD_IDENTITY":               pg.Declined,
	"INVALID_CARD":                        pg.Declined,
	"NOT_SUPPORTED_CARD_TYPE":             pg.Declined,
	"NOT_REGISTERED_CARD_COMPANY":         pg.Declined, // 카드를 사용 등록 후 이용
	"EXCEED_MAX_AUTH_COUNT":               pg.Declined,
	"EXCEED_MAX_DAILY_PAYMENT_COUNT":      pg.Declined,
	"EXCEED_MAX_PAYMENT_AMOUNT":           pg.Declined,
	"EXCEED_MAX_ONE_DAY_AMOUNT":           pg.Declined,
	"EXCEED_MAX_MONTHLY_PAYMENT_AMOUNT":   pg.Declined,
	"EXCEED_MAX_AMOUNT":                   pg.Declined,
	"EXCEED_MAX_ONE_DAY_WITHDRAW_AMOUNT":  pg.Declined,
	"EXCEED_MAX_ONE_TIME_WITHDRAW_AMOUNT": pg.Declined,
	"EXCEED_UNKNOWN_CASE":                 pg.Declined, // 한도 초과
	"FDS_ERROR":                           pg.Declined, // 위험거래가 감지되어 결제가 제한
	"KYC_BLOCKED":                         pg.Declined,
	"RESTRICTED_TRANSFER_ACCOUNT":         pg.Declined,
	"INVALID_ACCOUNT_INFO_RE_REGISTER":    pg.Declined,
	"NOT_ALLOWED_POINT_USE":               pg.Declined,
	"INVALID_PASSWORD":                    pg.Declined,
	// A billing key Toss does not have, or will not charge: the method is
	// gone, and the payer registers another.
	"NOT_FOUND_BILLING":        pg.Declined,
	"INVALID_BILL_KEY_REQUEST": pg.Declined,
	"INVALID_BILLING_AUTH":     pg.Declined,
	// The window, sent back to failUrl: the payer left it, or it failed.
	"PAY_PROCESS_CANCELED": pg.Declined,
	"PAY_PROCESS_ABORTED":  pg.Declined,

	// Wrong, or not done as asked: nothing happened, and the same request
	// fails the same way again.
	"INVALID_REQUEST":                         pg.Invalid,
	"INVALID_REQUIRED_PARAM":                  pg.Invalid,
	"INVALID_ORDER_ID":                        pg.Invalid,
	"INVALID_ORDER_NAME":                      pg.Invalid,
	"INVALID_CUSTOMER_KEY":                    pg.Invalid,
	"INVALID_IDEMPOTENCY_KEY":                 pg.Invalid,
	"INVALID_CURRENCY":                        pg.Invalid,
	"INVALID_PAYMENT_KEY":                     pg.Invalid,
	"INVALID_TEST_CODE":                       pg.Invalid,
	"BELOW_MINIMUM_AMOUNT":                    pg.Invalid, // 신용카드는 100원 이상
	"BELOW_ZERO_AMOUNT":                       pg.Invalid,
	"REQUIRED_AMOUNT":                         pg.Invalid,
	"NOT_MATCHES_CUSTOMER_KEY":                pg.Invalid, // the key is another customer's
	"DUPLICATED_ORDER_ID":                     pg.Invalid, // Charge asks the order first
	"API_VERSION_UPDATE_NEEDED":               pg.Invalid,
	"NOT_SUPPORTED_PROCESS":                   pg.Invalid,
	"ALREADY_CANCELED_PAYMENT":                pg.Invalid, // Cancel of all asks the payment first
	"ALREADY_REFUND_PAYMENT":                  pg.Invalid,
	"NOT_CANCELABLE_AMOUNT":                   pg.Invalid, // more than is left
	"NOT_CANCELABLE_PAYMENT":                  pg.Invalid,
	"NOT_CANCELABLE_PAYMENT_FOR_DORMANT_USER": pg.Invalid,
	"NOT_MATCHES_REFUNDABLE_AMOUNT":           pg.Invalid,
	"NOT_ALLOWED_PARTIAL_REFUND":              pg.Invalid,
	"EXCEED_CANCEL_AMOUNT_DISCOUNT_AMOUNT":    pg.Invalid,
	"EXCEED_MAX_REFUND_DUE":                   pg.Invalid, // past the refund window
	"EXCEED_MAX_REFUND_AMOUNT":                pg.Invalid,
	"EXCEED_CANCEL_LIMIT":                     pg.Invalid,
	"REFUND_REJECTED":                         pg.Invalid,
	"NOT_SUPPORTED_REFUND":                    pg.Invalid,

	// Levyan refused: its keys, the address it calls from, its contract.
	"UNAUTHORIZED_KEY":               pg.Auth,
	"INVALID_API_KEY":                pg.Auth,
	"INVALID_CLIENT_KEY":             pg.Auth,
	"INCORRECT_BASIC_AUTH_FORMAT":    pg.Auth,
	"API_KEY_ACCESS_DENIED":          pg.Auth, // not an address the key's access policy allows
	"FORBIDDEN_REQUEST":              pg.Auth,
	"NOT_SUPPORTED_METHOD":           pg.Auth, // a key with no automatic billing contract
	"NOT_FOUND_MERCHANT":             pg.Auth,
	"NOT_FOUND_MERCHANT_INTEGRATION": pg.Auth,
	"USER_ACCOUNT_ON_HOLD":           pg.Auth,
	"NOT_FOUND_TERMINAL_ID":          pg.Auth,

	// Not now: nothing happened, and later it may.
	"FORBIDDEN_CONSECUTIVE_REQUEST": pg.RateLimited, // 반복적인 요청은 허용되지 않습니다
	"MAINTAINED_METHOD":             pg.RateLimited, // 현재 점검 중
	"NOT_AVAILABLE_PAYMENT":         pg.RateLimited, // 결제가 불가능한 시간대
	"NOT_AVAILABLE_BANK":            pg.RateLimited, // 은행 서비스 시간이 아닙니다

	// Asked of nothing there is.
	"NOT_FOUND_PAYMENT":         pg.NotFound,
	"NOT_FOUND":                 pg.NotFound,
	"NOT_FOUND_PAYMENT_SESSION": pg.NotFound,

	// Whether anything happened is not known: the request is still being
	// worked on, or a failure Toss calls temporary. Every 5xx is too,
	// whatever its code says ([kind]).
	"IDEMPOTENT_REQUEST_PROCESSING": pg.Unknown, // 이전 멱등 요청이 처리중
	"ALREADY_PROCESSING_REQUEST":    pg.Unknown, // 이미 처리중인 요청
	"DUPLICATED_REQUEST":            pg.Unknown,
	"PROVIDER_ERROR":                pg.Unknown, // 일시적인 오류
	"CARD_PROCESSING_ERROR":         pg.Unknown, // 카드사에서 오류
}

// kind is what an answer of status with code says happened.
//
// A 5xx is [pg.Unknown] whatever its code: a PG that answered 500 may have
// charged the card all the same (levyan#10, Lago #6903), and only asking says.
// A code in [codes] is what the table says; one that is not is read by its
// status, and a 4xx that says nothing more is [pg.Invalid]: an operator reads
// the code, which is kept.
func kind(status int, code string) pg.Kind {
	if status >= 500 {
		return pg.Unknown
	}
	if k, ok := codes[code]; ok {
		return k
	}
	switch status {
	case http.StatusUnauthorized:
		return pg.Auth
	case http.StatusTooManyRequests:
		return pg.RateLimited
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly:
		return pg.Unknown
	}
	if status >= 400 {
		return pg.Invalid
	}
	return pg.Unknown
}
