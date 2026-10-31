package api

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/ledger"
)

// What a payer and a product are told of an invoice whose charge failed
// (#11): what the policy table says of it now (`server/invoices.Dunning`),
// worked out as it is asked from the invoice's charges, which are read with
// ent once what names the invoice -- the billing account, the invoice, the
// workspace -- was read as the caller may. Nothing of how the payer pays is
// in it: the kind of the decline, and the times.

// dunningOf is what is done of an invoice in dunning, as the API says it.
func dunningOf(d invoices.Dunned) *levyanv1.Dunning {
	out := levyanv1.Dunning_builder{
		Invoice:        pdid.Id(d.Invoice.Id).String(),
		BillingAccount: pdid.Id(d.Invoice.BillingAccountId).String(),
		Kind:           levyanv1.DeclineKind(d.Kind),
		Failed:         timestamppb.New(d.Failed),
		GraceEnds:      timestamppb.New(d.GraceEnds),
		RestrictedFrom: timestamppb.New(d.Restricted),
	}.Build()
	if !d.Retry.IsZero() {
		out.SetNextRetry(timestamppb.New(d.Retry))
	}

	return out
}

func dunningsOf(ds []invoices.Dunned) []*levyanv1.Dunning {
	out := []*levyanv1.Dunning{}
	for _, d := range ds {
		out = append(out, dunningOf(d))
	}
	return out
}

// clock is the time `now` says, as the ledger keeps one; the wall clock when
// it says nothing.
func clock(now func() time.Time) time.Time {
	if now == nil {
		return ledger.Moment(time.Now())
	}
	return ledger.Moment(now())
}
