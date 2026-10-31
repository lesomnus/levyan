package payers

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entworkspaceevent "github.com/lesomnus/levyan/internal/ent/workspaceevent"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// A workspace's life (#14, Claude's): what its product says became of it --
// closed, restored, destroyed -- each at a moment, kept as a
// [levyan.WorkspaceEvent] through the stack it is said through, as the
// product, in a transaction that holds the workspace's row, as the ledger
// does. What it was at any moment is all of them read in the order of their
// moments ([rating.Lived]), so each is said once of a moment -- said again it
// changes nothing -- and one said late takes its place: an older one never
// undoes what a newer one says.
//
//   - **Closed**, a workspace is no customer until it is restored: what it
//     holds is still sent and kept, and billed for none of that stretch
//     (`server/billing`). Its payer stays its payer.
//   - **Restored**, it is billed again from then.
//   - **Destroyed**, it is no customer from then on, for good: its last
//     reading holds no further, a reading measured from then is refused
//     (`server/ledger`), each link that would pay for it past then ends
//     there ([levyan.LinkEnded_LINK_ENDED_DESTROYED]), and what was offered
//     of it is withdrawn. A close or a restore said of a moment after it is
//     refused.
//
// An event that reaches into a period whose close had begun when it came is
// late, as a reading can be: what the period was billed is corrected on a
// later statement, and nothing written is changed.

// Said is what came of an event a product said of a workspace.
type Said struct {
	// Life is what became of the workspace, as everything said of it says.
	Life rating.Life

	// Late is an event kept that reaches into a period whose close had begun
	// when it came; false for one that was kept before, or changed nothing.
	Late bool
}

// Close is `product` saying it closed workspace `key` at `at`.
func (p Payers) Close(ctx context.Context, product, key pdid.Id, at time.Time) (Said, error) {
	return p.say(ctx, product, key, levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_CLOSED, at)
}

// Restore is `product` saying it opened workspace `key` again at `at`.
func (p Payers) Restore(ctx context.Context, product, key pdid.Id, at time.Time) (Said, error) {
	return p.say(ctx, product, key, levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_RESTORED, at)
}

// Destroy is `product` saying it destroyed workspace `key` at `at`, for good.
func (p Payers) Destroy(ctx context.Context, product, key pdid.Id, at time.Time) (Said, error) {
	return p.say(ctx, product, key, levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED, at)
}

// Life is what became of workspace `key` of `product`, as it has said.
func (p Payers) Life(ctx context.Context, product, key pdid.Id) (rating.Life, error) {
	ws, ok, err := Workspace(ctx, p.db(), product.Uuid(), key)
	if err != nil || !ok {
		return rating.Life{}, err
	}

	return billing.Life(ctx, p.db(), product.Uuid(), ws)
}

// verb is what a product says of a workspace with an event of `kind`.
func verb(kind levyan.WorkspaceEventKind) string {
	return strings.ToLower(strings.TrimPrefix(kind.String(), "WORKSPACE_EVENT_KIND_"))
}

func (p Payers) say(ctx context.Context, product, key pdid.Id, kind levyan.WorkspaceEventKind, at time.Time) (Said, error) {
	if at.IsZero() || at.Unix() <= 0 {
		return Said{}, status.Errorf(codes.InvalidArgument, "at: when the workspace was %s", verb(kind))
	}
	at = ledger.Moment(at)
	if at.After(p.now().Add(ledger.Skew)) {
		return Said{}, status.Errorf(codes.InvalidArgument, "at: %s is ahead of now", at.Format(time.RFC3339Nano))
	}
	ws, ok, err := Workspace(ctx, p.db(), product.Uuid(), key)
	if err != nil {
		return Said{}, err
	}
	if !ok {
		return Said{}, notArrived(key)
	}

	var out Said
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		since, ok, err := billing.Rater{Db: db}.Since(ctx, product.Uuid(), ws)
		if err != nil {
			return err
		}
		if !ok {
			return notArrived(key)
		}
		if at.Before(since) {
			return status.Errorf(codes.InvalidArgument, "at: %s is before workspace %s arrived, at %s", at.Format(time.RFC3339Nano), key, since.Format(time.RFC3339Nano))
		}

		life, err := billing.Life(ctx, db, product.Uuid(), ws)
		if err != nil {
			return err
		}
		out.Life = life
		if d := life.Destroyed; !d.IsZero() && !at.Before(d) {
			if kind == levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED {
				// Destroyed already, and for good: nothing to say again.
				return nil
			}
			return status.Errorf(codes.FailedPrecondition, "workspace %s was destroyed at %s, for good: nothing is %s after it", key, d.Format(time.RFC3339Nano), verb(kind))
		}

		kept, err := db.WorkspaceEvent.Query().
			Where(entworkspaceevent.TenantId(product.Uuid()), entworkspaceevent.WorkspaceId(ws), entworkspaceevent.HappenedAt(at)).
			Only(ctx)
		switch {
		case err == nil:
			if kept.Kind == int32(kind) {
				return nil
			}
			// A moment is one event, and two kinds of it are a fault
			// somewhere, which is not levyan's to pick between: said in the
			// log, as a reading of two values is.
			was := levyan.WorkspaceEventKind(kept.Kind)
			log.From(ctx).WarnContext(ctx, "payers: refused an event that says another thing became of a workspace at a moment already kept",
				slog.String("product", product.String()),
				slog.String("workspace", key.String()),
				slog.Time("at", at),
				slog.String("kept", verb(was)),
				slog.String("sent", verb(kind)))
			return status.Errorf(codes.FailedPrecondition, "workspace %s was %s at %s, and is not %s then", key, verb(was), at.Format(time.RFC3339Nano), verb(kind))
		case !ent.IsNotFound(err):
			return err
		}

		req := levyan.WorkspaceEventAddRequest_builder{
			Tenant:     levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
			Workspace:  levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
			Kind:       kind,
			HappenedAt: tsOf(at),
		}.Build()
		through, late, err := billing.LateThrough(ctx, db, product.Uuid(), at)
		if err != nil {
			return err
		}
		if late {
			req.SetLateThrough(tsOf(through))
		}
		if _, err := s.WorkspaceEvent().Add(ctx, req); err != nil {
			return err
		}

		if kind == levyan.WorkspaceEventKind_WORKSPACE_EVENT_KIND_DESTROYED {
			if err := p.destroyed(ctx, s, db, product.Uuid(), ws, at); err != nil {
				return err
			}
		}

		out.Late = late
		out.Life, err = billing.Life(ctx, db, product.Uuid(), ws)
		return err
	})

	return out, err
}

// destroyed is what a workspace destroyed at `at` takes with it, written
// through `s` on the transaction `db` is on: every link that would pay for it
// past `at` ends there, and what was offered of it is withdrawn.
func (p Payers) destroyed(ctx context.Context, s levyan.Server, db *ent.Client, product, ws uuid.UUID, at time.Time) error {
	ls, err := links(ctx, db, product, ws)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if _, err := end(ctx, s, l, at, levyan.LinkEnded_LINK_ENDED_DESTROYED); err != nil {
			return err
		}
	}

	return withdraw(ctx, s, db, product, ws, pdid.Nil, p.now())
}

// notArrived is the refusal of an event of a workspace levyan has heard
// nothing of -- NotFound, as another product's is: nothing of it is billed,
// so there is nothing to close, restore or destroy until the product says it
// arrived.
func notArrived(key pdid.Id) error {
	return status.Errorf(codes.NotFound, "workspace %s has not arrived: levyan has heard nothing of it, so nothing of it is billed; say it arrived first (Arrive)", key)
}

// gone refuses what would begin to be paid for, or offered, of a workspace
// destroyed, which is nobody's to pay for.
func gone(ctx context.Context, db *ent.Client, product, ws uuid.UUID, key pdid.Id) error {
	d, err := ledger.Destroyed(ctx, db, product, ws)
	if err != nil {
		return err
	}
	if at, ok := d[ws]; ok {
		return status.Errorf(codes.FailedPrecondition, "workspace %s was destroyed at %s, for good", key, at.Format(time.RFC3339Nano))
	}

	return nil
}
