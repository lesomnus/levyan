package grants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
)

// Rule is a source of grants by what levyan knows of a workspace's billing:
// a workspace with no payer past its grace is restricted (#4), and so is one
// whose payment is overdue (#11). It is the hook those plug into; a pass of
// [RenewLoop] asks each rule what it wants of each product, gives what it
// wants and does not have, and ends what it has and no longer wants.
//
// A rule's grants are its own, by its name: two rules may give one key of one
// workspace -- each its own grant, both in force -- and what one stops wanting
// is ended without the other's being touched. So two rules that give one key
// give it the same value, or the one whose grant began last decides; a
// restriction is ON from whichever rule restricts.
//
// What a rule gives is positive, as every grant is: a restriction is a grant
// that restricts when it is there (see the package). A rule that cannot say
// what it wants -- its reads failed -- answers an error, and nothing of its
// grants is ended for it: not knowing is not wanting nothing.
type Rule interface {
	// Name is what its grants say they come from: `payers.grace`. It never
	// changes, since a grant of another name is another rule's.
	Name() string

	// Want is every grant the rule wants `product`'s workspaces to have at
	// `now`, read from `db`, the deployment's client -- of `product`'s rows
	// and of no other product's.
	Want(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) ([]Want, error)
}

// Want is a grant a rule wants a workspace to have: given if it has none of
// the rule's, given again if the one it has gives another value, and kept,
// renewed, for as long as the rule goes on wanting it.
type Want struct {
	// Workspace is the workspace's row.
	Workspace uuid.UUID

	Key   string
	Value entitle.Value

	// Desc is why, in a sentence, as the grant says it when it is given.
	Desc string
}

// Restricted is the key a product's workspace is restricted by, whichever
// rule restricts it -- a workspace nobody pays for (`payers.grace`), an
// invoice unpaid past its grace (`dunning`) -- so that the product checks one
// flag: its alias, as a key's first word is written, and `.restricted`,
// `loft.restricted`. Each rule's grant of it is its own, and the workspace is
// restricted while any is in force.
//
// It is the first of a restriction's two stages (#11, the owner's,
// 2026-11-01): the product stops what would grow what the workspace is billed
// for, and its everyday work goes on. The second is [Readonly].
func Restricted(alias string) (string, error) {
	return keyOf(alias, "restricted", "restrict its workspaces by")
}

// Readonly is the key a product's workspace is made read-only by, the second
// stage of a restriction, whichever rule makes it so -- a workspace nobody
// has paid for long enough (`payers.readonly`), an invoice unpaid long
// enough (`dunning.readonly`): its alias and `.readonly`, `loft.readonly`.
// The product refuses every write of a workspace given it but reading,
// exporting and downloading, paying, and closing or leaving it
// (docs/apps.md). Each rule's grant of it is its own, as of [Restricted],
// and a rule gives it no sooner than the workspace is restricted.
func Readonly(alias string) (string, error) {
	return keyOf(alias, "readonly", "make its workspaces read-only by")
}

// keyOf is `alias`'s key `word`, as a key's first word is written.
func keyOf(alias, word, what string) (string, error) {
	key := strings.ReplaceAll(alias, "-", "_") + "." + word
	if err := entitle.CheckKey(key); err != nil {
		return "", fmt.Errorf("grants: a product called %q has no key to %s: %w", alias, what, err)
	}

	return key, nil
}

// Follow is what `rules` want of every product, given, and what each gave
// and no longer wants, ended: of every rule of [Grants.Rules] when it names
// none. A rule followed outside the loop -- by a loop of its own, sooner than
// the grants' -- is the same rule, by its name, as the one in the loop's
// list, and following it twice is following it once.
func (g Grants) Follow(ctx context.Context, rules ...Rule) error {
	if len(rules) == 0 {
		rules = g.Rules
	}
	if len(rules) == 0 {
		return nil
	}

	ps, err := g.db().Tenant.Query().Order(ent.Asc(enttenant.FieldId)).Ids(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, p := range ps {
		for _, r := range rules {
			if err := g.follow(ctx, p, r); err != nil {
				errs = append(errs, fmt.Errorf("%s of product %s: %w", r.Name(), pdid.Id(p), err))
			}
		}
	}

	return errors.Join(errs...)
}

// follow is one rule's grants of one product made what it wants.
func (g Grants) follow(ctx context.Context, product uuid.UUID, r Rule) error {
	now := g.now()
	wants, err := r.Want(ctx, g.db(), product, now)
	if err != nil {
		return err
	}

	type of struct {
		ws  uuid.UUID
		key string
	}
	wanted := map[of]bool{}
	var errs []error
	for _, w := range wants {
		k := of{w.Workspace, w.Key}
		if wanted[k] {
			// A rule says what it wants of a key once; a second value of one
			// would end the first every pass.
			continue
		}
		wanted[k] = true

		_, err := g.give(ctx, Give{
			Product: pdid.Id(product),
			Key:     w.Key,
			Value:   w.Value,
			Source:  levyan.GrantSource_GRANT_SOURCE_RULE,
			Rule:    r.Name(),
			Desc:    w.Desc,
		}, w.Workspace)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s of workspace %s: %w", w.Key, pdid.Id(w.Workspace), err))
		}
	}

	have, err := g.db().Grant.Query().
		Where(
			entgrant.TenantId(product),
			entgrant.Source(int32(levyan.GrantSource_GRANT_SOURCE_RULE)),
			entgrant.Rule(r.Name()),
			entgrant.DateErasedIsNil(),
			entgrant.Or(entgrant.Renews(true), entgrant.ValidUntilGT(now)),
		).
		All(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, h := range have {
		if wanted[of{h.WorkspaceId, h.Key}] {
			continue
		}
		err := g.tx(ctx, func(s levyan.Server, db *ent.Client) error {
			_, err := g.end(ctx, s, db, h.Id, now)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("ending grant %s: %w", pdid.Id(h.Id), err))
		}
	}

	return errors.Join(errs...)
}
