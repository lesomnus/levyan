package billing

import (
	"math/big"

	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/rating"
)

// Line is a line of a statement: what a meter came to over the statement's
// period.
func Line(l rating.Line) *levyan.StatementLine {
	out := levyan.StatementLine_builder{
		Meter:    l.Meter.Id.Bytes(),
		Key:      l.Meter.Key,
		Unit:     l.Meter.Unit,
		Kind:     levyan.LineKind_LINE_KIND_USAGE,
		Usage:    fraction(l.Quantity),
		Included: fraction(l.Included),
		Billable: fraction(l.Billable),
		Amount:   fraction(l.Amount),
	}.Build()
	if l.Allowance != nil && l.Allowance.Sign() != 0 {
		out.SetAllowance(fraction(l.Allowance))
	}
	if l.Pooled != nil && l.Pooled.Sign() != 0 {
		out.SetPooled(fraction(l.Pooled))
	}
	for _, t := range l.Takes {
		out.SetPools(append(out.GetPools(), levyan.StatementPool_builder{Pool: t.Pool.Bytes(), Taken: fraction(t.Quantity)}.Build()))
	}

	for _, p := range l.Parts {
		part := levyan.StatementPart_builder{
			Price:     p.Version.Bytes(),
			From:      timestamppb.New(p.From),
			To:        timestamppb.New(p.To),
			Share:     fraction(p.Share),
			Included:  p.Included,
			Allowance: p.Allowance,
			Billable:  fraction(p.Billable),
			Amount:    fraction(p.Amount),
		}.Build()
		for _, a := range p.Allowances {
			part.SetAllowances(append(part.GetAllowances(), a.Bytes()))
		}
		if p.Pooled != nil && p.Pooled.Sign() != 0 {
			part.SetPooled(fraction(p.Pooled))
		}
		for _, b := range p.Bands {
			band := levyan.StatementBand_builder{Fee: b.Fee, Share: fraction(b.Share), Amount: fraction(b.Amount)}.Build()
			if b.Bounded {
				band.SetUpTo(b.UpTo)
			}
			part.SetBands(append(part.GetBands(), band))
		}
		out.SetParts(append(out.GetParts(), part))
	}
	for _, s := range l.Unpriced {
		out.SetUnpriced(append(out.GetUnpriced(), levyan.StatementUnpriced_builder{
			From: timestamppb.New(s.From),
			To:   timestamppb.New(s.To),
		}.Build()))
	}

	return out
}

// Corrects is a line of a statement correcting an earlier period's: what the
// meter comes to over it now, what was billed for it, and the difference as
// the line's amount.
func Corrects(c Correction) *levyan.StatementLine {
	out := Line(c.Now)
	out.SetKind(levyan.LineKind_LINE_KIND_CORRECTION)
	out.SetCorrectsStart(timestamppb.New(c.Period.DateStart))
	out.SetCorrectsEnd(timestamppb.New(c.Period.DateEnd))
	if c.Statement != nil {
		out.SetCorrects(c.Statement.Id[:])
	}
	out.SetBilled(fraction(c.Billed))
	out.SetAmount(fraction(c.Moves))

	return out
}

// fraction is a fraction as a statement writes one: `110/3`, `100`.
func fraction(r *big.Rat) string {
	if r == nil {
		return "0"
	}

	return r.RatString()
}
