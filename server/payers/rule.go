package payers

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/payday/entitle"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entworkspaceevent "github.com/lesomnus/levyan/internal/ent/workspaceevent"
	"github.com/lesomnus/levyan/server/grants"
)

// Payerless is the rule that restricts a workspace nobody has paid for since
// `payers.grace` ago (#4): it wants `<product>.restricted` ON of it, through
// the grants' rules hook ([grants.Rule]), until somebody pays for it again or
// will from the end of the period. A workspace that never had a payer is not
// one it restricts: levyan has heard nothing of who should. Nor is one
// destroyed (#14), which nobody pays for because there is nothing of it left.
//
// The restriction is a grant that restricts when it is there, as every
// restriction is (docs/design.md §13), so a gap in levyan lifts it rather than
// stopping every workspace.
type Payerless struct {
	// Grace is how long a workspace may go with no payer: [DefaultGrace] when
	// zero.
	Grace time.Duration
}

var _ grants.Rule = Payerless{}

// RuleName is what its grants say they come from.
const RuleName = "payers.grace"

func (Payerless) Name() string { return RuleName }

// Restricted is the key a product's workspace is restricted by: its alias,
// as a key's first word is written, and `.restricted` -- `loft.restricted`.
func Restricted(alias string) (string, error) {
	key := strings.ReplaceAll(alias, "-", "_") + ".restricted"
	if err := entitle.CheckKey(key); err != nil {
		return "", fmt.Errorf("payers: a product called %q has no key to restrict its workspaces by: %w", alias, err)
	}

	return key, nil
}

func (r Payerless) Want(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) ([]grants.Want, error) {
	grace := r.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return nil, err
	}
	key, err := Restricted(t.Alias)
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
		if paid[ws] || destroyed[ws] || now.Before(at.Add(grace)) {
			continue
		}
		out = append(out, grants.Want{
			Workspace: ws,
			Key:       key,
			Value:     entitle.On,
			Desc:      fmt.Sprintf("%s부터 지불자가 없고, %d일이 지났다", at.UTC().Format(time.RFC3339), int(grace.Hours()/24)),
		})
	}

	return out, nil
}
