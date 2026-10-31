package toss

import (
	"net/http"

	"github.com/lesomnus/levyan/internal/pg"
)

// declines is each of Toss's codes for a charge the card refused -- what
// [kind] reads as pg.Declined, and only these are -- and which kind of
// decline it is (pg.Decline). The codes are those of
// https://docs.tosspayments.com/reference/error-codes (2026-10-31): the
// tables of the calls this adapter makes, 카드 자동결제 승인
// (#카드-자동결제-승인) and 카드 자동결제 빌링키 발급 요청
// (#카드-자동결제-빌링키-발급-요청), then 결제 승인 (#결제-승인) and the page's
// 전체 에러 목록; and the window's failUrl codes of
// https://docs.tosspayments.com/guides/v2/billing/integration. The words
// beside each are Toss's, cut short.
//
// A code is the kind its words say. One whose words say only that the card
// was refused is pg.DeclineOther, the zero kind, which is also what [decline]
// answers of a code that is not here. That the four kinds are these is
// Claude's, the lead's (levyan#10, 26); which code is which is the
// implementation's, and so is where it draws the line between a card to
// replace and one blocked: Toss names no block of a card's company as such,
// and a card its company stopped (INVALID_STOPPED_CARD) or locked
// (EXCEED_MAX_AUTH_COUNT) says nothing of fraud, so it is one the payer
// replaces, or takes up with the company. Fraud is what a risk or identity
// check refused.
var declines = map[string]pg.Decline{
	// Too little left, or a limit reached: later, on the same card, it may
	// go through. A card company that cannot answer is temporary too, and
	// Toss says so with no decline -- CARD_PROCESSING_ERROR and a 5xx, an
	// unknown outcome, below.
	"REJECT_CARD_PAYMENT":                 pg.DeclineTemporary, // 한도초과 혹은 잔액부족
	"REJECT_ACCOUNT_PAYMENT":              pg.DeclineTemporary, // 잔액부족
	"EXCEED_MAX_DAILY_PAYMENT_COUNT":      pg.DeclineTemporary, // 하루 결제 가능 횟수를 초과
	"EXCEED_MAX_PAYMENT_AMOUNT":           pg.DeclineTemporary, // 하루 결제 가능 금액을 초과
	"EXCEED_MAX_ONE_DAY_AMOUNT":           pg.DeclineTemporary, // 일일 한도를 초과
	"EXCEED_MAX_MONTHLY_PAYMENT_AMOUNT":   pg.DeclineTemporary, // 당월 결제 가능금액을 초과
	"EXCEED_MAX_AMOUNT":                   pg.DeclineTemporary, // 거래금액 한도를 초과
	"EXCEED_UNKNOWN_CASE":                 pg.DeclineTemporary, // 한도 초과
	"EXCEED_MAX_ONE_DAY_WITHDRAW_AMOUNT":  pg.DeclineTemporary, // 1일 출금 한도를 초과 (계좌)
	"EXCEED_MAX_ONE_TIME_WITHDRAW_AMOUNT": pg.DeclineTemporary, // 1회 출금 한도를 초과 (계좌)
	"RESTRICTED_TRANSFER_ACCOUNT":         pg.DeclineTemporary, // 계좌는 등록 후 12시간 뒤부터

	// The method will not be charged as it is: expired, lost or stolen,
	// stopped or locked by its company, not one Toss takes, not what was
	// registered, or a billing key Toss no longer has. The payer registers
	// another. Toss has no code of its own for a card that was closed (해지).
	"INVALID_CARD_EXPIRATION":          pg.DeclineReplace, // 카드 정보를 다시 확인해주세요 (유효기간)
	"INVALID_CARD_LOST_OR_STOLEN":      pg.DeclineReplace, // 분실 혹은 도난 카드
	"INVALID_STOPPED_CARD":             pg.DeclineReplace, // 정지된 카드
	"INVALID_CARD_NUMBER":              pg.DeclineReplace, // 카드번호를 다시 확인해주세요
	"INVALID_CARD":                     pg.DeclineReplace, // 카드 입력정보가 올바르지 않습니다
	"INVALID_CARD_PASSWORD":            pg.DeclineReplace, // 카드 정보를 다시 확인해주세요 (비밀번호)
	"INVALID_CARD_IDENTITY":            pg.DeclineReplace, // 주민번호/사업자번호가 카드 소유주 정보와 일치하지 않습니다
	"NOT_SUPPORTED_CARD_TYPE":          pg.DeclineReplace, // 지원되지 않는 카드 종류
	"NOT_REGISTERED_CARD_COMPANY":      pg.DeclineReplace, // 카드를 사용 등록 후 이용해주세요
	"EXCEED_MAX_AUTH_COUNT":            pg.DeclineReplace, // 최대 인증 횟수를 초과했습니다. 카드사로 문의해주세요
	"NOT_FOUND_BILLING":                pg.DeclineReplace, // 존재하지 않는 빌링 결제 인증 정보
	"INVALID_BILL_KEY_REQUEST":         pg.DeclineReplace, // 빌링키 인증이 완료되지 않았거나 유효하지 않은 빌링 거래 건
	"INVALID_BILLING_AUTH":             pg.DeclineReplace, // 유효하지 않은 빌링 인증
	"REJECT_TOSSPAY_INVALID_ACCOUNT":   pg.DeclineReplace, // 출금 계좌가 출금이체 등록이 되어 있지 않아요 (계좌)
	"INVALID_ACCOUNT_INFO_RE_REGISTER": pg.DeclineReplace, // 유효하지 않은 계좌입니다. 계좌 재등록 후 (계좌)

	// Refused as suspected fraud, or blocked by a check of who is paying.
	"FDS_ERROR":   pg.DeclineFraud, // 위험거래가 감지되어 결제가 제한됩니다
	"KYC_BLOCKED": pg.DeclineFraud, // KYC 결제 차단

	// Refused, and no more said; or a way of paying levyan never asks for.
	"REJECT_CARD_COMPANY":   pg.DeclineOther, // 결제 승인이 거절되었습니다
	"INVALID_REJECT_CARD":   pg.DeclineOther, // 카드 사용이 거절되었습니다. 카드사 문의가 필요합니다
	"NOT_ALLOWED_POINT_USE": pg.DeclineOther, // 포인트 사용이 불가한 카드로 카드 포인트 결제에 실패
	"INVALID_PASSWORD":      pg.DeclineOther, // 결제 비밀번호가 일치하지 않습니다
	// The window, sent back to failUrl: the payer left it, or it failed.
	"PAY_PROCESS_CANCELED": pg.DeclineOther, // 결제가 사용자에 의해 취소되었습니다
	"PAY_PROCESS_ABORTED":  pg.DeclineOther, // 결제가 취소되었거나 진행되지 않았습니다
}

// decline is which kind of decline a refusal with code is: what [declines]
// says, and pg.DeclineOther for a code it does not have.
func decline(code string) pg.Decline { return declines[code] }

// codes is what each of Toss's other error codes says happened, of the ones
// the calls this adapter makes can answer: the per-API tables and the full
// list of https://docs.tosspayments.com/reference/error-codes (2026-10-31).
// The card's own refusals are [declines].
//
// A code that is in neither is read by its status ([kind]): Toss adds codes
// without a new API version (https://docs.tosspayments.com/reference/versioning),
// so the table is not all there is, and the code is always kept beside the
// kind for an operator to read.
var codes = map[string]pg.Kind{
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
// A code in [declines] is [pg.Declined] and one in [codes] is what the table
// says; one in neither is read by its status, and a 4xx that says nothing more
// is [pg.Invalid]: an operator reads the code, which is kept.
func kind(status int, code string) pg.Kind {
	if status >= 500 {
		return pg.Unknown
	}
	if k, ok := table(code); ok {
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

// table is what the tables say a code is, if either has it: [pg.Declined] for
// one of [declines], and what [codes] says for one of its.
func table(code string) (pg.Kind, bool) {
	if _, ok := declines[code]; ok {
		return pg.Declined, true
	}
	k, ok := codes[code]
	return k, ok
}

// failure is why a payment Toss failed with code failed, as Toss's Payment
// keeps it (`failure.code`): a refusal of the card's, and which kind; another
// of Toss's codes, as the table reads it; and a code levyan does not read --
// COMMON_ERROR, a 5xx's -- the PG's own, which says nothing of the card.
func failure(code, message string) *pg.Failure {
	k, ok := table(code)
	if !ok {
		k = pg.Unknown
	}
	f := &pg.Failure{Kind: k, Code: code, Message: message}
	if k == pg.Declined {
		f.Decline = decline(code)
	}
	return f
}
