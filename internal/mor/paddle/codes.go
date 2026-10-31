package paddle

import (
	"net/http"

	"github.com/lesomnus/levyan/internal/mor"
)

// codes is what each of Paddle's error codes says happened, of the ones the
// calls this adapter makes can answer: the codes of
// https://developer.paddle.com/errors/overview and their own pages
// (2026-10-31).
//
// A code that is not here is read by its status ([kind]): Paddle adds codes,
// so the table is not all there is, and the code is always kept beside the
// kind for an operator to read.
var codes = map[string]mor.Kind{
	// The card's fault, or no card: nothing was charged, and the payer has
	// something to do. prevent_change answers a declined charge made now
	// with this, and leaves the subscription as it was.
	"subscription_payment_declined":                     mor.Declined,
	"subscription_missing_payment_method_cannot_update": mor.Declined,

	// Wrong, or not done as asked: nothing happened, and the same request
	// fails the same way again.
	"bad_request":   mor.Invalid,
	"invalid_field": mor.Invalid,
	"invalid_json":  mor.Invalid,
	"invalid_url":   mor.Invalid,

	"subscription_update_when_canceled":                                    mor.Invalid, // ErrEnded
	"subscription_is_canceled_action_invalid":                              mor.Invalid, // ErrEnded
	"subscription_update_when_past_due":                                    mor.Invalid, // ErrPastDue
	"subscription_update_transaction_balance_less_than_charge_limit":       mor.Invalid, // ErrBelowMinimum
	"subscription_locked_pending_changes":                                  mor.Invalid, // a scheduled change stands in the way
	"subscription_not_active":                                              mor.Invalid,
	"subscription_is_inactive_action_invalid":                              mor.Invalid,
	"subscription_update_when_trialing":                                    mor.Invalid,
	"subscription_scheduled_change_invalid_update":                         mor.Invalid,
	"subscription_charge_duplicate_price_ids":                              mor.Invalid,
	"subscription_duplicate_price_ids":                                     mor.Invalid,
	"subscription_maximum_number_of_line_items_reached":                    mor.Invalid,
	"subscription_currency_code_incompatible_with_payment_method_provider": mor.Invalid,
	"subscription_not_automatic_collection":                                mor.Invalid,
	"subscription_must_be_paused":                                          mor.Invalid,

	"transaction_immutable":                        mor.Invalid,
	"transaction_status_must_be_ready":             mor.Invalid,
	"transaction_invalid_status_change":            mor.Invalid,
	"transaction_creation_blocked":                 mor.Invalid,
	"transaction_not_ready_cannot_process_payment": mor.Invalid,

	"adjustment_amount_cannot_be_zero":                            mor.Invalid,
	"adjustment_amount_above_remaining_allowed":                   mor.Invalid, // more than is left of the line
	"adjustment_total_amount_above_remaining_allowed":             mor.Invalid, // more than is left of the transaction
	"adjustment_transaction_invalid_status_for_refund":            mor.Invalid, // not completed (yet)
	"adjustment_transaction_item_has_already_been_fully_adjusted": mor.Invalid,
	"adjustment_transaction_without_captured_payment":             mor.Invalid,
	"adjustment_invalid_credit_action":                            mor.Invalid,

	// Levyan refused: its key, the key's permissions, the account's settings.
	"authentication_missing":   mor.Auth,
	"authentication_malformed": mor.Auth,
	"invalid_token":            mor.Auth,
	"invalid_client_token":     mor.Auth,
	"forbidden":                mor.Auth, // a sandbox key at the live API, or one without the permission
	"subscription_product_tax_category_not_approved": mor.Auth,

	// Not now: nothing happened, and later it may.
	"too_many_requests": mor.RateLimited,
	"subscription_immediate_charge_hour_limit_exceeded":    mor.RateLimited,
	"subscription_immediate_charge_24_hour_limit_exceeded": mor.RateLimited,
	"subscription_locked_renewal":                          mor.RateLimited, // renewing within 30 minutes
	"subscription_locked_processing":                       mor.RateLimited,
	"subscription_locked_consent_review_period":            mor.RateLimited,
	"subscription_outstanding_transaction":                 mor.RateLimited,
	"subscription_outstanding_pending_refund":              mor.RateLimited,
	"adjustment_pending_refund_request":                    mor.RateLimited, // one refund waits for approval

	// Asked of nothing there is.
	"not_found": mor.NotFound,

	// Whether anything happened is not known: a payment still in progress, a
	// change made at the same time. Every 5xx is too, whatever its code says
	// ([kind]).
	"subscription_payment_attempt_pending": mor.Unknown,
	"conflict":                             mor.Unknown,
	"concurrent_modification":              mor.Unknown,
}

// sentinels is what a code says beyond its kind, in words that name no MoR.
var sentinels = map[string]error{
	"subscription_update_when_canceled":                              mor.ErrEnded,
	"subscription_is_canceled_action_invalid":                        mor.ErrEnded,
	"subscription_update_when_past_due":                              mor.ErrPastDue,
	"subscription_update_transaction_balance_less_than_charge_limit": mor.ErrBelowMinimum,
}

// kind is what an answer of status with code says happened.
//
// A 5xx is [mor.Unknown] whatever its code: an MoR that answered 500 may have
// charged the card all the same, and only asking says. A code in [codes] is
// what the table says; one that is not is read by its status, and a 4xx that
// says nothing more is [mor.Invalid]: an operator reads the code, which is
// kept.
func kind(status int, code string) mor.Kind {
	if status >= 500 {
		return mor.Unknown
	}
	if k, ok := codes[code]; ok {
		return k
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return mor.Auth
	case http.StatusNotFound:
		return mor.NotFound
	case http.StatusTooManyRequests:
		return mor.RateLimited
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly:
		return mor.Unknown
	}
	if status >= 400 {
		return mor.Invalid
	}
	return mor.Unknown
}

// payments is each of a payment attempt's error codes that is the payer's
// method refusing it -- what is read as mor.Declined, and only these are --
// and which kind of decline it is (mor.Decline). The codes, and the words
// beside each, are Paddle's
// (https://developer.paddle.com/api-reference/transactions/get-transaction,
// payments[].error_code, 2026-10-31). The rest of its codes --
// issuer_unavailable, psp_error, system_error, unknown, and those of a
// cancellation or a refund -- say nothing of the method, and are read as an
// unknown failure with the code kept.
//
// A code is the kind its words say, as package toss's are Toss's: that the
// four kinds are these is Claude's, the lead's (levyan#10, 26), and which code
// is which is the implementation's. One that says only that the method
// refused is mor.DeclineOther. An authentication the payer did not pass is a
// method to give again -- as Toss's wrong password and count of tries are --
// and not fraud, which is what a check of risk refused: the issuer's, the
// provider's, or one the seller set.
var payments = map[string]mor.Decline{
	// Later, on the same method, it may go through.
	"not_enough_balance": mor.DeclineTemporary, // insufficient funds, or fund limits being reached

	// The method will not be charged as it is: the payer gives another, or
	// takes it up with its issuer.
	"authentication_failed":           mor.DeclineReplace, // the authentication challenge … wasn't successful
	"blocked_card":                    mor.DeclineReplace, // frozen, lost, damaged, or stolen
	"canceled":                        mor.DeclineReplace, // the mandate for recurring payments canceled
	"declined_not_retryable":          mor.DeclineReplace, // the issuer has indicated that it should not be retried
	"expired_card":                    mor.DeclineReplace, // this card is expired
	"invalid_payment_details":         mor.DeclineReplace, // the payment method isn't valid
	"preferred_network_not_supported": mor.DeclineReplace, // the network scheme … isn't supported
	"prepaid_card_not_supported":      mor.DeclineReplace, // prepaid cards are blocked on this Paddle account
	"redacted_payment_method":         mor.DeclineReplace, // the payment method information … redacted
	"transaction_not_permitted":       mor.DeclineReplace, // the issuer doesn't allow this kind of payment

	// A check of risk refused it.
	"fraud":             mor.DeclineFraud, // flagged … as potentially fraudulent
	"blocked_by_policy": mor.DeclineFraud, // blocked by one of the seller's configured policy checks

	// Refused, and no more said.
	"declined":       mor.DeclineOther, // declined, with no other information returned
	"invalid_amount": mor.DeclineOther, // cannot process a payment that is this high or low
}

// paymentFailure is why a payment attempt with error code failed: a decline
// of its kind, for one of [payments], and an unknown failure, which says
// nothing of the method, for any other.
func paymentFailure(code string) *mor.Failure {
	d, ok := payments[code]
	if !ok {
		return &mor.Failure{Kind: mor.Unknown, Code: code}
	}
	return &mor.Failure{Kind: mor.Declined, Decline: d, Code: code}
}

// declines is which kind of decline each code a request is refused with as
// mor.Declined is ([codes]): a charge made now that the method declined,
// which prevent_change answers with subscription_payment_declined and no
// payment to read why from -- refused, and no more said -- and a
// subscription with no method to charge, which the payer gives anew.
var declines = map[string]mor.Decline{
	"subscription_payment_declined":                     mor.DeclineOther,
	"subscription_missing_payment_method_cannot_update": mor.DeclineReplace,
}

// decline is which kind of decline a request refused with code is.
func decline(code string) mor.Decline { return declines[code] }
