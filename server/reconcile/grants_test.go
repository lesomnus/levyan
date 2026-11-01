package reconcile_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/payers"
	"github.com/lesomnus/levyan/server/reconcile"
)

// A restriction is there exactly where a rule says it should be: a
// workspace nobody has paid for since its grace ran out is restricted by the
// loop that keeps grants, and found missing it once the rule has wanted it
// for longer than the loop takes to give it -- ended by somebody, written
// past the servers. The operator follows the rule again, and it is there.
func TestAGrantARuleWantsIsThere(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	account, _ := w.payer(ws)
	w.to(on(1, 5))
	_, err := w.S.Payers.Leave(w.ctx, w.product, ws, account)
	x.NoError(err)

	// Wanted from February 4th, and given by the loop.
	w.to(on(2, 4).Add(30 * time.Minute))
	x.NoError(w.S.Grants.Pass(w.ctx))
	w.reconcile()
	w.nothing()
	g, err := w.S.Ent.Grant.Query().Where(entgrant.Rule(payers.RuleName)).Only(w.ctx)
	x.NoError(err)
	x.Equal("loft.restricted", g.Key)

	// Ended by hand, past the servers.
	x.NoError(w.S.Ent.Grant.UpdateOneId(g.Id).SetValidUntil(w.srv.Now()).SetRenews(false).Exec(w.ctx))
	w.reconcile()
	w.nothing()

	w.to(w.srv.Now().Add(reconcile.Lag(time.Hour)))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "loft.restricted")
	x.Equal(int32(levyan.FindingCorrection_FINDING_CORRECTION_RULE), fs[0].Correction)

	x.Error(w.S.Reconciler.Rule(w.ctx, w.product, ""), "an operator's")
	x.NoError(w.S.Reconciler.Rule(w.op, w.product, ""))
	w.reconcile()
	w.nothing()
}

// A grant of a rule the deployment has no longer -- renewed by the loop all
// the same, since nothing wants it ended -- is left over, and found. The
// operator follows that rule again, which wants nothing, and it ends.
func TestAGrantOfARuleThereIsNoLongerIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	w.payer(ws)
	g, err := w.S.Grants.Give(w.ctx, grants.Give{
		Product: w.product, Workspace: ws, Key: "loft.overdue", Value: entitle.On,
		Source: levyan.GrantSource_GRANT_SOURCE_RULE, Rule: "dunning.v0", Desc: "a rule of a build before",
	})
	x.NoError(err)

	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT)
	x.Len(fs, 1)
	id, err := pdid.From(g.GetId())
	x.NoError(err)
	x.Equal(reconcile.Subject("Grant", id.Uuid()), fs[0].Subject)
	x.Contains(fs[0].Summary, "dunning.v0, which this deployment has no longer")

	x.NoError(w.S.Reconciler.Rule(w.op, w.product, "dunning.v0"))
	w.reconcile()
	w.nothing()
}

// A grant that renews of a workspace that was destroyed is ended as it is
// next renewed (#14): one still in force once that renewal is long past is
// left over, and found. The operator follows the rules again, which ends it.
func TestAGrantOfADestroyedWorkspaceIsFoundOnceItsRenewalIsPast(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	w.payer(ws)
	_, err := w.S.Grants.Give(w.ctx, grants.Give{Product: w.product, Workspace: ws, Key: "loft.export", Value: entitle.On, Source: levyan.GrantSource_GRANT_SOURCE_TRIAL})
	x.NoError(err)
	w.to(on(1, 2))
	_, err = w.S.Payers.Destroy(w.ctx, w.product, ws, on(1, 2))
	x.NoError(err)

	w.to(on(1, 2).Add(grants.DefaultHorizon / 2))
	w.reconcile()
	w.nothing()

	w.to(on(1, 2).Add(grants.DefaultHorizon/2 + reconcile.Lag(time.Hour) + time.Minute))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "destroyed at 2027-01-02T00:00:00Z")

	x.NoError(w.S.Reconciler.Rule(w.op, w.product, ""))
	w.reconcile()
	w.nothing()
}

// The rules are each held to, `dunning` beside `payers.grace` (#11): a
// workspace whose invoice is unpaid past its grace is restricted by the
// loop of dunning, as the policy table says (`dunning.Policy.Dun`, which the
// rule asks); ended by somebody, the restriction is found missing once the
// rule has wanted it for longer than the loops take to give it, and the
// operator follows that rule again.
func TestTheDunningRuleIsHeldToToo(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.month(on(1, 1), ws)
	w.run()
	inv := w.only(ba)
	a := w.charges(inv)[0]
	x.Equal(failed, a.State, "no subscription to charge: a method to give anew")

	// The grace of a method to replace is a fortnight, and no retry.
	w.to(w.srv.Now().Add(14*24*time.Hour + time.Hour))
	x.NoError(w.S.Invoices.Dun(w.ctx, &w.S.Grants))
	g, err := w.S.Ent.Grant.Query().Where(entgrant.Rule("dunning")).Only(w.ctx)
	x.NoError(err)
	x.Equal("loft.restricted", g.Key)
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING))
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT))

	x.NoError(w.S.Ent.Grant.UpdateOneId(g.Id).SetValidUntil(w.srv.Now()).SetRenews(false).Exec(w.ctx))
	w.to(w.srv.Now().Add(reconcile.Lag(time.Hour)))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING)
	x.Len(fs, 1)
	x.Contains(fs[0].Subject, "/dunning/")

	x.NoError(w.S.Reconciler.Rule(w.op, w.product, "dunning"))
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING))
}

// A grant of a rule the rule does not want -- given past it, of a workspace
// that has a payer -- is left over, and found; followed again, the rule ends
// it.
func TestAGrantARuleDoesNotWantIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	ws := levyantest.Tenant()
	w.payer(ws)
	_, err := w.S.Grants.Give(w.ctx, grants.Give{
		Product: w.product, Workspace: ws, Key: "loft.restricted", Value: entitle.On,
		Source: levyan.GrantSource_GRANT_SOURCE_RULE, Rule: payers.RuleName, Desc: "given past the rule",
	})
	x.NoError(err)

	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "is of rule payers.grace, which has wanted it neither now nor a while ago")

	x.NoError(w.S.Reconciler.Rule(w.op, w.product, payers.RuleName))
	w.reconcile()
	w.nothing()
}

// The second stage of each restriction is held to as the first is (#11):
// `dunning.readonly` makes what an unpaid invoice bills read-only sixty days
// after its first failure, and `payers.readonly` a workspace nobody has paid
// for sixty days after its last payer. A grant of either ended by somebody is
// found missing once the rule has wanted it longer than the loops take to
// give it, and one given past the rule is found left over; the operator
// follows the rule again, by its name, and nothing is found.
func TestTheReadonlyRulesAreHeldToToo(t *testing.T) {
	w := setup(t)
	x := w.X
	day := 24 * time.Hour

	owed, left := levyantest.Tenant(), levyantest.Tenant()
	_, ba := w.payer(owed)
	account, _ := w.payer(left)
	w.month(on(1, 1), owed)
	w.run()
	inv := w.only(ba)
	t0 := w.charges(inv)[0].DateResolved.UTC()
	_, err := w.S.Payers.Leave(w.ctx, w.product, left, account)
	x.NoError(err)
	since := w.srv.Now()

	// Sixty days on: each read-only, by its own rule, as the loops keep it.
	w.to(latest(t0, since).Add(60*day + time.Hour))
	x.NoError(w.S.Grants.Pass(w.ctx))
	x.NoError(w.S.Invoices.Dun(w.ctx, &w.S.Grants))
	by := map[string]*ent.Grant{}
	vs, err := w.S.Ent.Grant.Query().Where(entgrant.Key("loft.readonly")).All(w.ctx)
	x.NoError(err)
	for _, v := range vs {
		by[v.Rule] = v
	}
	x.Len(by, 2)
	x.Contains(by, invoices.ReadonlyRule)
	x.Contains(by, payers.ReadonlyRule)
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING))
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT))

	// Ended by hand, past the servers: found missing, each of its rule.
	for _, g := range by {
		x.NoError(w.S.Ent.Grant.UpdateOneId(g.Id).SetValidUntil(w.srv.Now()).SetRenews(false).Exec(w.ctx))
	}
	w.to(w.srv.Now().Add(reconcile.Lag(time.Hour)))
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING)
	x.Len(fs, 2)
	rules := []string{}
	for _, f := range fs {
		x.Contains(f.Summary, "loft.readonly")
		x.Equal(int32(levyan.FindingCorrection_FINDING_CORRECTION_RULE), f.Correction)
		for _, r := range []string{invoices.ReadonlyRule, payers.ReadonlyRule} {
			if strings.Contains(f.Subject, "/"+r+"/") {
				rules = append(rules, r)
			}
		}
	}
	x.ElementsMatch([]string{invoices.ReadonlyRule, payers.ReadonlyRule}, rules)

	for _, r := range []string{invoices.ReadonlyRule, payers.ReadonlyRule} {
		x.NoError(w.S.Reconciler.Rule(w.op, w.product, r))
	}
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_MISSING))
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT))

	// Given past the rule, of a workspace somebody pays for: left over.
	paid := levyantest.Tenant()
	w.payer(paid)
	_, err = w.S.Grants.Give(w.ctx, grants.Give{
		Product: w.product, Workspace: paid, Key: "loft.readonly", Value: entitle.On,
		Source: levyan.GrantSource_GRANT_SOURCE_RULE, Rule: payers.ReadonlyRule, Desc: "given past the rule",
	})
	x.NoError(err)
	w.reconcile()
	fs = w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "is of rule payers.readonly, which has wanted it neither now nor a while ago")
	x.NoError(w.S.Reconciler.Rule(w.op, w.product, payers.ReadonlyRule))
	w.reconcile()
	x.Empty(w.of(levyan.FindingKind_FINDING_KIND_GRANT_LEFT))
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
