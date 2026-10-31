package invoices_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	levyan "github.com/lesomnus/levyan"
	morpkg "github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/server/invoices"
)

// What a channel's adapter says of a request that failed is read as it is:
// a gateway's kinds and a merchant of record's are the ledger's, by the same
// names and values, and so are the schema's kinds of failure and of decline.
// One of them that moved would have the ledger send blind what it should ask
// about.
func TestTheChannelsKindsAreTheLedgers(t *testing.T) {
	x := require.New(t)

	for _, k := range []struct {
		ledger invoices.Kind
		pg     pg.Kind
		mor    morpkg.Kind
		schema levyan.FailureKind
	}{
		{invoices.Unknown, pg.Unknown, morpkg.Unknown, levyan.FailureKind_FAILURE_KIND_UNSPECIFIED},
		{invoices.Declined, pg.Declined, morpkg.Declined, levyan.FailureKind_FAILURE_KIND_DECLINED},
		{invoices.Invalid, pg.Invalid, morpkg.Invalid, levyan.FailureKind_FAILURE_KIND_INVALID},
		{invoices.Auth, pg.Auth, morpkg.Auth, levyan.FailureKind_FAILURE_KIND_AUTH},
		{invoices.RateLimited, pg.RateLimited, morpkg.RateLimited, levyan.FailureKind_FAILURE_KIND_RATE_LIMITED},
		{invoices.NotFound, pg.NotFound, morpkg.NotFound, levyan.FailureKind_FAILURE_KIND_NOT_FOUND},
	} {
		x.Equal(k.ledger, invoices.KindOf(&pg.Error{Op: "charge", Kind: k.pg}), "%s", k.ledger)
		x.Equal(k.ledger, invoices.KindOf(&morpkg.Error{Op: "charge", Kind: k.mor}), "%s", k.ledger)
		x.Equal(k.ledger, invoices.KindOf(&invoices.Error{Op: "send", Kind: k.ledger}))
		x.Equal(k.schema, invoices.FailureOf(k.ledger, 0, "", "").GetKind())
		x.Equal(k.ledger.String(), k.pg.String())
	}
	x.Equal(invoices.Unknown, invoices.KindOf(errors.New("nobody knows")), "an error that is no channel's may have done anything")

	for _, d := range []struct {
		pg     pg.Decline
		mor    morpkg.Decline
		schema levyan.DeclineKind
	}{
		{pg.DeclineOther, morpkg.DeclineOther, levyan.DeclineKind_DECLINE_KIND_UNSPECIFIED},
		{pg.DeclineTemporary, morpkg.DeclineTemporary, levyan.DeclineKind_DECLINE_KIND_TEMPORARY},
		{pg.DeclineReplace, morpkg.DeclineReplace, levyan.DeclineKind_DECLINE_KIND_REPLACE},
		{pg.DeclineFraud, morpkg.DeclineFraud, levyan.DeclineKind_DECLINE_KIND_FRAUD},
	} {
		x.Equal(d.schema, invoices.FailureOf(invoices.Declined, d.pg, "", "").GetDecline())
		x.Equal(d.pg, pg.Decline(d.mor), "a merchant of record's kind of decline is a gateway's, value for value: %s", d.mor)
		x.Equal(d.pg.String(), d.mor.String())
	}
}

// The seller's tax is the rate of the amount toward nothing, the same for an
// amount taken back as for one charged.
func TestTheTaxIsTowardNothing(t *testing.T) {
	x := require.New(t)
	for in, want := range map[int64]int64{0: 0, 9: 0, 10: 1, 19: 1, 1005: 100, 5000: 500, -1005: -100, -19: -1} {
		x.Equal(want, invoices.TaxOf(invoices.DefaultTax, in), "%d", in)
	}
	x.Zero(invoices.TaxOf(nil, 1000))
	x.False(invoices.Seller(levyan.Channel_CHANNEL_MOR), "the merchant of record sells on its own")
	x.True(invoices.Seller(levyan.Channel_CHANNEL_PG_KR))
}
