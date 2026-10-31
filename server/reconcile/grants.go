package reconcile

import (
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/ledger"
)

// Lag is how long a rule may want something before the loop that keeps
// grants (`levyan.grants.renew`) is held to have given it, and how long it
// may have stopped wanting it before the grant is held to have ended: two of
// the loop's passes, and the minute a grant's now may lag by (`grants.Lag`).
func Lag(renew time.Duration) time.Duration { return 2*renew + time.Minute }

// grants holds what is granted to what the rules want ([grants.Rule]): a
// restriction, say, is there exactly where a rule says it should be. What a
// rule has wanted since a lag ago ([Lag]) and has no grant of is missing;
// a grant of a rule that has wanted it neither now nor a lag ago is left
// over -- as is one of a rule there is no longer, and one that renews of a
// workspace destroyed longer ago than its renewal would have ended it in
// (#14). What is in between is the loop's to give or to end at its next
// pass, and is not found.
func (r *Reconciler) grants(ctx context.Context, p *pass) ([]Found, error) {
	g := r.Grants
	if g == nil {
		return nil, nil
	}
	lag := r.lag()
	ps, err := p.db.Tenant.Query().Order(ent.Asc(enttenant.FieldId)).All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, t := range ps {
		live, err := p.db.Grant.Query().
			Where(
				entgrant.TenantId(t.Id),
				entgrant.DateErasedIsNil(),
				entgrant.ValidFromLTE(p.now),
				entgrant.Or(entgrant.Renews(true), entgrant.ValidUntilGT(p.now)),
			).
			Order(ent.Asc(entgrant.FieldDateCreated), ent.Asc(entgrant.FieldId)).
			All(ctx)
		if err != nil {
			return nil, err
		}

		there := map[string]bool{}
		for _, rule := range g.Rules {
			there[rule.Name()] = true
			now, err := rule.Want(ctx, p.db, t.Id, p.now)
			if err != nil {
				return nil, fmt.Errorf("what %s wants of %s: %w", rule.Name(), t.Alias, err)
			}
			before, err := rule.Want(ctx, p.db, t.Id, p.now.Add(-lag))
			if err != nil {
				return nil, fmt.Errorf("what %s wanted of %s: %w", rule.Name(), t.Alias, err)
			}
			out = append(out, r.wanted(t, rule.Name(), now, before, live)...)
		}

		for _, v := range live {
			if v.Source != int32(levyan.GrantSource_GRANT_SOURCE_RULE) || there[v.Rule] {
				continue
			}
			out = append(out, left(t, v, fmt.Sprintf("is of rule %s, which this deployment has no longer: no rule wants it, and the loop renews it all the same", v.Rule)))
		}

		ws := []uuid.UUID{}
		for _, v := range live {
			if v.Renews {
				ws = append(ws, v.WorkspaceId)
			}
		}
		d, err := ledger.Destroyed(ctx, p.db, t.Id, ws...)
		if err != nil {
			return nil, err
		}
		gone := p.now.Add(-(g.Horizon/2 + lag))
		if g.Horizon <= 0 {
			gone = p.now.Add(-(grants.DefaultHorizon/2 + lag))
		}
		for _, v := range live {
			at, ok := d[v.WorkspaceId]
			if !v.Renews || !ok || !at.Before(gone) {
				continue
			}
			out = append(out, left(t, v, fmt.Sprintf("renews for workspace %s, destroyed at %s: its renewal ends it, and has not", pdid.Id(v.WorkspaceId), stamp(at))))
		}
	}

	// One left over for more than one reason is found once, for the first.
	return out, nil
}

func (r *Reconciler) lag() time.Duration { return Lag(or(r.Renew, time.Hour)) }

// wanted is what rule `name` of product `t` wants -- `now`, and a lag ago,
// `before` -- held to the grants of it in force, `live`.
func (r *Reconciler) wanted(t *ent.Tenant, name string, now, before []grants.Want, live []*ent.Grant) []Found {
	type of struct {
		ws    uuid.UUID
		key   string
		value entitle.Value
	}
	both, either := map[of]grants.Want{}, map[of]bool{}
	for _, w := range before {
		either[of{w.Workspace, w.Key, w.Value}] = true
	}
	for _, w := range now {
		k := of{w.Workspace, w.Key, w.Value}
		if either[k] {
			both[k] = w
		}
		either[k] = true
	}

	have := map[of]bool{}
	out := []Found{}
	for _, v := range live {
		if v.Source != int32(levyan.GrantSource_GRANT_SOURCE_RULE) || v.Rule != name {
			continue
		}
		k := of{v.WorkspaceId, v.Key, grants.Value(v.Proto())}
		have[k] = true
		if !either[k] {
			out = append(out, left(t, v, fmt.Sprintf("is of rule %s, which has wanted it neither now nor a while ago", name)))
		}
	}

	keys := []of{}
	for k := range both {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b of) int {
		if c := slices.Compare(a.ws[:], b.ws[:]); c != 0 {
			return c
		}
		if a.key != b.key {
			if a.key < b.key {
				return -1
			}
			return 1
		}
		return 0
	})
	for _, k := range keys {
		if have[k] {
			continue
		}
		w := both[k]
		out = append(out, Found{
			Kind:    levyan.FindingKind_FINDING_KIND_GRANT_MISSING,
			Subject: fmt.Sprintf("Grant/%s/%s/%s/%s", pdid.Id(t.Id), name, pdid.Id(w.Workspace), w.Key),
			Product: t.Id,
			Summary: fmt.Sprintf("rule %s wants workspace %s of %s to have %s %s, and it has no grant of it: %s",
				name, pdid.Id(w.Workspace), t.Alias, w.Key, w.Value, w.Desc),
			Facts: fmt.Sprintf("rule=%s key=%s value=%s", name, w.Key, w.Value),
			Rows:  []Row{RowOf("Workspace", w.Workspace)},
		})
	}

	return out
}

// left is grant `v` of product `t` found left over, `why`.
func left(t *ent.Tenant, v *ent.Grant, why string) Found {
	return Found{
		Kind:    levyan.FindingKind_FINDING_KIND_GRANT_LEFT,
		Subject: Subject("Grant", v.Id),
		Product: t.Id,
		Summary: fmt.Sprintf("grant %s of %s %s to workspace %s of %s %s", pdid.Id(v.Id), v.Key, grants.Value(v.Proto()), pdid.Id(v.WorkspaceId), t.Alias, why),
		Facts:   why,
		Rows:    []Row{RowOf("Grant", v.Id), RowOf("Workspace", v.WorkspaceId)},
	}
}
