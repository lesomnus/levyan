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

// payments is what each of a payment attempt's error codes says of why it
// failed (https://developer.paddle.com/api-reference/transactions/get-transaction,
// payments[].error_code): the card's fault, or the MoR's own. A code that is
// not here says nothing of the card.
var payments = map[string]mor.Kind{
	"authentication_failed":           mor.Declined,
	"blocked_card":                    mor.Declined,
	"canceled":                        mor.Declined, // the payer canceled the mandate
	"declined":                        mor.Declined,
	"declined_not_retryable":          mor.Declined,
	"expired_card":                    mor.Declined,
	"fraud":                           mor.Declined,
	"invalid_amount":                  mor.Declined,
	"invalid_payment_details":         mor.Declined,
	"not_enough_balance":              mor.Declined,
	"preferred_network_not_supported": mor.Declined,
	"prepaid_card_not_supported":      mor.Declined,
	"transaction_not_permitted":       mor.Declined,
}

func paymentKind(code string) mor.Kind {
	if k, ok := payments[code]; ok {
		return k
	}
	return mor.Unknown
}
