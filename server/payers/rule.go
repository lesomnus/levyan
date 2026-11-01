package payers

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/lesomnus/payday/entitle"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/ent"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entworkspaceevent "github.com/lesomnus/levyan/internal/ent/workspaceevent"
	"github.com/lesomnus/levyan/server/grants"
)

// Payerless is the rule that restricts a workspace nobody has paid for since
// it last had a payer, as the policy table's row of it says
// (`dunning.kinds.payerless`, #11; it was `payers.grace`, #4): once its grace
// is over -- thirty days, by default -- or at once. It wants
// `<product>.restricted` ON of it, through the grants' rules hook
// ([grants.Rule]), until somebody pays for it again or will from the end of
// the period. A workspace that never had a payer is not one it restricts:
// levyan has heard nothing of who should. Nor is one destroyed (#14), which
// nobody pays for because there is nothing of it left.
//
// The restriction is a grant that restricts when it is there, as every
// restriction is (docs/design.md §13), so a gap in levyan lifts it rather than
// stopping every workspace. Its key is the one an unpaid invoice restricts by
// too ([grants.Restricted]), each rule with a grant of its own.
type Payerless struct {
	// Row is the table's row of a workspace nobody pays for: its grace, from
	// when it last had a payer, and whether it is restricted at once.
	Row dunning.Row
}

var _ grants.Rule = Payerless{}

// RuleName is what its grants say they come from: the name it had before its
// row was the table's, which it keeps, since a grant of another name is
// another rule's.
const RuleName = "payers.grace"

func (Payerless) Name() string { return RuleName }

func (r Payerless) Want(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) ([]grants.Want, error) {
	return payerless(ctx, db, product, now, grants.Restricted, r.Row.Restricts, func(at time.Time) string {
		if r.Row.AtOnce {
			return fmt.Sprintf("%s부터 지불자가 없다", at.UTC().Format(time.RFC3339))
		}
		return fmt.Sprintf("%s부터 지불자가 없고, %s이 지났다", at.UTC().Format(time.RFC3339), dunning.Korean(r.Row.Grace))
	})
}

// Readonly is the rule that makes a workspace nobody has paid for long
// enough read-only, the second stage of [Payerless]'s restriction (#11, the
// owner's, 2026-11-01): as the policy table's row of a workspace with no
// payer says, sixty days after it last had a payer by default -- from then,
// as its grace is, which is Claude's -- and never sooner than it is
// restricted. It wants `<product>.readonly` ON of it ([grants.Readonly])
// until somebody pays for it again or will from the end of the period, and
// passes over what [Payerless] does: a workspace that never had a payer, and
// one destroyed. A rule of its own, by its own name, as an unpaid invoice's
// second stage is (`dunning.readonly`).
type Readonly struct {
	// Row is the table's row of a workspace nobody pays for: when it is
	// read-only, from when it last had a payer.
	Row dunning.Row
}

var _ grants.Rule = Readonly{}

// ReadonlyRule is what its grants say they come from.
const ReadonlyRule = "payers.readonly"

func (Readonly) Name() string { return ReadonlyRule }

func (r Readonly) Want(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) ([]grants.Want, error) {
	return payerless(ctx, db, product, now, grants.Readonly, r.Row.Readonly, func(at time.Time) string {
		return fmt.Sprintf("%s부터 지불자가 없고, %s이 지났다", at.UTC().Format(time.RFC3339), dunning.Korean(r.Row.ReadonlyAfter))
	})
}

// payerless is the grant of `product`'s key `keyOf` that each of its
// workspaces nobody has paid for since it last had a payer is wanted to have
// at `now`, from `from` of when it last had one on, `desc` saying why: of
// none paid for now or from the end of the period, none that never had a
// payer, and none destroyed.
func payerless(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time, keyOf func(string) (string, error), from func(time.Time) time.Time, desc func(time.Time) string) ([]grants.Want, error) {
	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return nil, err
	}
	key, err := keyOf(t.Alias)
	if err != nil {
		return nil, err
	}

	ls, err := db.Link.Query().Where(entlink.TenantId(product)).All(ctx)
	if err != nil {
		return nil, err
	}
	ds, err := db.WorkspaceEvent.Query().
		Where(entworkspaceevent.TenantId(product), entworkspaceevent.Kind(int32(levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED))).
		All(ctx)
	if err != nil {
		return nil, err
	}
	destroyed := map[uuid.UUID]bool{}
	for _, d := range ds {
		destroyed[d.WorkspaceId] = true
	}
	since := map[uuid.UUID]time.Time{}
	paid := map[uuid.UUID]bool{}
	for _, l := range ls {
		switch {
		case empty(l):
		case l.DateEnd == nil || l.DateEnd.After(now):
			// Paid for now, or from when it begins.
			paid[l.WorkspaceId] = true
		case l.DateEnd.After(since[l.WorkspaceId]):
			since[l.WorkspaceId] = *l.DateEnd
		}
	}

	var out []grants.Want
	for ws, at := range since {
		if paid[ws] || destroyed[ws] || now.Before(from(at)) {
			continue
		}
		out = append(out, grants.Want{
			Workspace: ws,
			Key:       key,
			Value:     entitle.On,
			Desc:      desc(at),
		})
	}

	return out, nil
}
