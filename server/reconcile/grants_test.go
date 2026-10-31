package reconcile_test

import (
	"testing"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
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
