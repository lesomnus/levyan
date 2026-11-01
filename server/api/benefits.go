package api

import (
	"context"
	"slices"
	"uuid"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/invoices"
)

// What a workspace and a billing account are given beyond their prices (#43),
// as the API says it: the allowances in force over a period, the credit an
// operator granted that is left to spend, and what paid an invoice's
// `credited`. None of it names why it was given or who gave it: those are an
// operator's words, and the operator's.

// allowanceOf is an allowance as the API says it.
func allowanceOf(v *ent.Allowance) *levyanv1.Allowance {
	out := levyanv1.Allowance_builder{
		Id:       pdid.Id(v.Id).String(),
		Quantity: v.Quantity,
		From:     timestamppb.New(v.ValidFrom),
	}.Build()
	if m := v.Edges.Meter; m != nil {
		out.SetMeter(m.Key)
	}
	if v.ValidUntil != nil {
		out.SetUntil(timestamppb.New(*v.ValidUntil))
	}

	return out
}

// creditOf is a lot an operator granted as the API says it, its workspace by
// the product's name of it in `keys`.
func creditOf(l invoices.Lot, keys map[uuid.UUID]string) *levyanv1.Credit {
	out := levyanv1.Credit_builder{
		Id:       pdid.Id(l.Row.Id).String(),
		Granted:  l.Row.Amount,
		Left:     max(l.Left, 0),
		Currency: l.Row.Currency,
		Expires:  timestamppb.New(l.Expires()),
		Created:  timestamppb.New(l.Row.DateCreated),
	}.Build()
	if ws := l.Workspace(); ws != (uuid.UUID{}) {
		out.SetWorkspace(keys[ws])
	}
	if ba := l.Row.BillingAccountId; ba != (uuid.UUID{}) {
		out.SetBillingAccount(pdid.Id(ba).String())
	}

	return out
}

// workspaceKeys is the product's name of each workspace `lots` are of.
func workspaceKeys(ctx context.Context, db *ent.Client, product uuid.UUID, lots []invoices.Lot) (map[uuid.UUID]string, error) {
	ids := []uuid.UUID{}
	for _, l := range lots {
		if ws := l.Workspace(); ws != (uuid.UUID{}) {
			ids = append(ids, ws)
		}
	}
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := db.Workspace.Query().Where(entworkspace.TenantId(product), entworkspace.IdIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		out[w.Id] = text(w.Key)
	}

	return out, nil
}

// paidWith is what paid invoice `inv`'s `credited`, of `product`: what each
// lot an operator granted paid of it, the soonest to expire first, and what
// the balance paid, last.
func paidWith(ctx context.Context, db *ent.Client, product uuid.UUID, inv uuid.UUID) ([]*levyanv1.InvoiceCredit, error) {
	vs, err := db.Credit.Query().
		Where(entcredit.TenantId(product), entcredit.InvoiceId(inv), entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_APPLIED))).
		All(ctx)
	if err != nil || len(vs) == 0 {
		return nil, err
	}

	paid := map[uuid.UUID]int64{}
	balance := int64(0)
	ids := []uuid.UUID{}
	for _, v := range vs {
		if v.Lot == nil {
			balance -= v.Amount
			continue
		}
		if _, ok := paid[*v.Lot]; !ok {
			ids = append(ids, *v.Lot)
		}
		paid[*v.Lot] -= v.Amount
	}
	lots, err := invoices.Lots(ctx, db, product, entcredit.IdIn(ids...))
	if err != nil {
		return nil, err
	}
	keys, err := workspaceKeys(ctx, db, product, lots)
	if err != nil {
		return nil, err
	}

	out := []*levyanv1.InvoiceCredit{}
	for _, l := range lots {
		c := levyanv1.InvoiceCredit_builder{
			Credit:  pdid.Id(l.Row.Id).String(),
			Amount:  paid[l.Row.Id],
			Expires: timestamppb.New(l.Expires()),
		}.Build()
		if ws := l.Workspace(); ws != (uuid.UUID{}) {
			c.SetWorkspace(keys[ws])
		}
		out = append(out, c)
	}
	if balance != 0 {
		out = append(out, levyanv1.InvoiceCredit_builder{Amount: balance}.Build())
	}
	slices.SortStableFunc(out, func(a, b *levyanv1.InvoiceCredit) int {
		switch {
		case a.GetCredit() == "" && b.GetCredit() != "":
			return 1
		case a.GetCredit() != "" && b.GetCredit() == "":
			return -1
		}
		return 0
	})

	return out, nil
}
