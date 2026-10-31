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
	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return nil, err
	}
	key, err := grants.Restricted(t.Alias)
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
		if paid[ws] || destroyed[ws] || now.Before(r.Row.Restricts(at)) {
			continue
		}
		desc := fmt.Sprintf("%s부터 지불자가 없다", at.UTC().Format(time.RFC3339))
		if !r.Row.AtOnce {
			desc = fmt.Sprintf("%s부터 지불자가 없고, %s이 지났다", at.UTC().Format(time.RFC3339), dunning.Korean(r.Row.Grace))
		}
		out = append(out, grants.Want{
			Workspace: ws,
			Key:       key,
			Value:     entitle.On,
			Desc:      desc,
		})
	}

	return out, nil
}
