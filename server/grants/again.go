package grants

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	"github.com/lesomnus/levyan/server/ledger"
)

// Again follows the rules again for `product`, now, as whoever `ctx` is framed
// as: an operator's correction of what reconciliation found (#13), which
// does what the loop [RenewLoop] does for the product at its next pass, and
// no more.
//
//   - `rule` names a rule there is ([Grants.Rules]): its grants of the
//     product are made what it wants.
//   - `rule` names a rule there is not -- one the deployment had, and has no
//     longer -- every grant of the product given by a rule of that name and
//     in force is ended: a rule there is no longer wants nothing. The loop
//     follows the rules there are, and renews a rule's grant whatever
//     became of its rule.
//   - `rule` empty is every rule there is, and every one there is not that
//     a grant of the product in force is of.
//
// And every grant of the product's workspaces that were destroyed, and that
// renews, is ended: what its next renewal would do (#14).
func (g Grants) Again(ctx context.Context, product pdid.Id, rule string) error {
	if _, err := g.db().Tenant.Get(ctx, product.Uuid()); err != nil {
		return err
	}
	now := g.now()

	there := map[string]Rule{}
	names := []string{}
	for _, r := range g.Rules {
		there[r.Name()] = r
		if rule == "" {
			names = append(names, r.Name())
		}
	}
	if rule != "" {
		names = append(names, rule)
	} else {
		gone, err := g.Gone(ctx, product.Uuid())
		if err != nil {
			return err
		}
		names = append(names, gone...)
	}

	var errs []error
	for _, n := range names {
		if r, ok := there[n]; ok {
			if err := g.follow(ctx, product.Uuid(), r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n, err))
			}
			continue
		}
		hs, err := g.db().Grant.Query().
			Where(
				entgrant.TenantId(product.Uuid()),
				entgrant.Source(int32(levyan.GrantSource_GRANT_SOURCE_RULE)),
				entgrant.Rule(n),
				entgrant.DateErasedIsNil(),
				entgrant.Or(entgrant.Renews(true), entgrant.ValidUntilGT(now)),
			).
			Ids(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range hs {
			if err := g.endNow(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("ending grant %s of %s: %w", pdid.Id(id), n, err))
			}
		}
	}

	if err := g.endDestroyed(ctx, product.Uuid()); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Gone is the names of the rules a grant of `product` in force is of that
// there are not ([Grants.Rules]): rules the deployment had, and has no
// longer.
func (g Grants) Gone(ctx context.Context, product uuid.UUID) ([]string, error) {
	vs, err := g.db().Grant.Query().
		Where(
			entgrant.TenantId(product),
			entgrant.Source(int32(levyan.GrantSource_GRANT_SOURCE_RULE)),
			entgrant.DateErasedIsNil(),
			entgrant.Or(entgrant.Renews(true), entgrant.ValidUntilGT(g.now())),
		).
		Select(entgrant.FieldRule).
		All(ctx)
	if err != nil {
		return nil, err
	}
	there := map[string]bool{}
	for _, r := range g.Rules {
		there[r.Name()] = true
	}
	out := []string{}
	for _, v := range vs {
		if !there[v.Rule] && !slices.Contains(out, v.Rule) {
			out = append(out, v.Rule)
		}
	}
	slices.Sort(out)

	return out, nil
}

// endDestroyed ends every grant of `product` that renews and is of a workspace
// that was destroyed.
func (g Grants) endDestroyed(ctx context.Context, product uuid.UUID) error {
	vs, err := g.db().Grant.Query().
		Where(entgrant.TenantId(product), entgrant.Renews(true), entgrant.DateErasedIsNil()).
		All(ctx)
	if err != nil || len(vs) == 0 {
		return err
	}
	ws := []uuid.UUID{}
	for _, v := range vs {
		ws = append(ws, v.WorkspaceId)
	}
	d, err := ledger.Destroyed(ctx, g.db(), product, ws...)
	if err != nil {
		return err
	}

	var errs []error
	for _, v := range vs {
		if _, gone := d[v.WorkspaceId]; !gone {
			continue
		}
		if err := g.endNow(ctx, v.Id); err != nil {
			errs = append(errs, fmt.Errorf("ending grant %s: %w", pdid.Id(v.Id), err))
		}
	}

	return errors.Join(errs...)
}

// endNow ends grant `id` now, in a transaction of its own.
func (g Grants) endNow(ctx context.Context, id uuid.UUID) error {
	return g.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		_, err := g.end(ctx, s, db, id, g.now())
		return err
	})
}
