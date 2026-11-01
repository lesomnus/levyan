package billing

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/levyan/internal/ent"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
)

// DefaultFree is how many of each payer's workspaces of a product have the
// allowances their prices include, when `payers.free` says nothing: three,
// as many as an account may own in loft (`app.workspaces.owned`).
const DefaultFree = 3

// Payer is who a workspace's period is billed to (#4): the link in force
// through it -- of which a period has one at most, since every change of who
// pays takes effect at a period's end -- and the billing account it pays
// with.
type Payer struct {
	// Link is nil for a workspace nobody paid for over the period, or one
	// levyan has heard of no payer of.
	Link *ent.Link

	// Account is the link's billing account, and nil for one with none.
	Account *ent.BillingAccount

	// FirstUnit is a workspace rated from the first unit: none of its payer's
	// first [Rater.Free] of the product, so that what its prices include is
	// not its. False for a workspace with no payer: when in doubt, less.
	FirstUnit bool
}

// PayerOf is who workspace `ws` of `product` was paid for by over `p`.
//
// Which of a payer's workspaces have their allowances is counted per product
// and per period: of the workspaces the payer paid for over it, the first
// [Rater.Free] they began paying for -- by the first link of theirs to each,
// whenever that was, so that a payer who moves a workspace to another billing
// account, or stops and pays again, keeps its place. A workspace is counted
// once, under the one payer of its period, whoever else owns it.
//
// A workspace away for the whole of the period -- closed through it and not
// restored, or destroyed before it -- is none of what its payer paid for
// over it (#14, Claude's): its place passes to the next, and it keeps its
// allowances itself, should anything of it come to be rated for the period
// (when in doubt, less). One closed for some of the period is counted, and
// so is one closed through it and restored since, which is billed for it as
// for any open time (the owner's, 2026-11-01).
func (r Rater) PayerOf(ctx context.Context, product, ws uuid.UUID, p rating.Period) (Payer, error) {
	ls, err := r.Db.Link.Query().
		Where(
			entlink.TenantId(product),
			entlink.WorkspaceId(ws),
			entlink.DateStartLT(p.End),
			entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(p.Start)),
		).
		WithBillingAccount().
		Order(ent.Desc(entlink.FieldDateStart), ent.Desc(entlink.FieldId)).
		All(ctx)
	if err != nil {
		return Payer{}, err
	}
	ls = slices.DeleteFunc(ls, empty)
	if len(ls) == 0 {
		return Payer{}, nil
	}

	l := ls[0]
	out := Payer{Link: l, Account: l.Edges.BillingAccount}

	// What else the payer paid for over the period, and when they first
	// began paying for each.
	theirs, err := r.Db.Link.Query().
		Where(
			entlink.TenantId(product),
			entlink.Payer(l.Payer),
			entlink.DateStartLT(p.End),
			entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(p.Start)),
		).
		All(ctx)
	if err != nil {
		return Payer{}, err
	}
	paid := map[uuid.UUID]bool{ws: true}
	for _, v := range theirs {
		if !empty(v) {
			paid[v.WorkspaceId] = true
		}
	}
	lives, err := Lives(ctx, r.Db, product, keysOf(paid)...)
	if err != nil {
		return Payer{}, err
	}
	for w, l := range lives {
		if l.Through(p) {
			delete(paid, w)
		}
	}
	if !paid[ws] {
		return out, nil
	}

	all, err := r.Db.Link.Query().
		Where(entlink.TenantId(product), entlink.Payer(l.Payer), entlink.WorkspaceIdIn(keysOf(paid)...)).
		All(ctx)
	if err != nil {
		return Payer{}, err
	}
	first := map[uuid.UUID]time.Time{}
	for _, v := range all {
		if empty(v) {
			continue
		}
		if at, ok := first[v.WorkspaceId]; !ok || v.DateStart.Before(at) {
			first[v.WorkspaceId] = v.DateStart
		}
	}

	order := keysOf(paid)
	slices.SortFunc(order, func(a, b uuid.UUID) int {
		if c := first[a].Compare(first[b]); c != 0 {
			return c
		}
		return cmp.Compare(a.String(), b.String())
	})
	free := r.Free
	if free < 0 {
		free = 0
	}
	out.FirstUnit = slices.Index(order, ws) >= free

	return out, nil
}

// TermsFor is what workspace `ws`'s period `p` is priced by: the product's
// terms `t`, on the channel and in the currency of the billing account its
// payer paid with when it has one, and from the first unit when it is none
// of its payer's first [Rater.Free].
func (r Rater) TermsFor(ctx context.Context, product, ws uuid.UUID, p rating.Period, t Terms) (Terms, Payer, error) {
	who, err := r.PayerOf(ctx, product, ws, p)
	if err != nil {
		return Terms{}, Payer{}, err
	}

	out := t
	out.FirstUnit = who.FirstUnit
	if a := who.Account; a != nil && a.Channel != "" && a.Currency != "" {
		c, err := Channel(a.Channel)
		if err != nil {
			return Terms{}, Payer{}, err
		}
		out.Channel, out.Currency = c, a.Currency
	}

	return out, who, nil
}

// fromFirst is `b` with nothing included by any version priced by the unit:
// what a workspace that is none of its payer's first few is rated by. A price
// by bands is as it is: a band's fee is what the band costs, and a band of
// nothing is a price of nothing rather than an allowance.
func fromFirst(b pricing.Book) pricing.Book {
	out := make(pricing.Book, 0, len(b))
	for _, v := range b {
		if pu, ok := v.Rate.(pricing.PerUnit); ok {
			pu.Included = 0
			v.Rate = pu
		}
		out = append(out, v)
	}

	return out
}

// empty is a link taken back before it began: one nothing was ever paid by.
func empty(l *ent.Link) bool { return l.DateEnd != nil && !l.DateEnd.After(l.DateStart) }

func keysOf(m map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
