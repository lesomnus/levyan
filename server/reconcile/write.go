package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entfinding "github.com/lesomnus/levyan/internal/ent/finding"
	"github.com/lesomnus/levyan/server/pd"
)

// What a finding that is no longer found is resolved as, and one put right by
// the pass that found it.
const (
	NoLonger = "no longer so"
	Reasked  = "put right by asking the merchant of record again"
)

type wrote struct{ added, changed, resolved int }

// write writes what the checks of `ks` found, through the stack, as whoever
// `ctx` is framed as: a finding not written before as new, one that says
// something other than it said as changed -- an accepted one opened again,
// when what it says is not what was accepted -- one resolved before as open
// again; and every finding of `ks` that is open or accepted and was not found,
// resolved.
func (r *Reconciler) write(ctx context.Context, p *pass, ks []levyan.FindingKind, found []Found) (wrote, error) {
	var (
		out  wrote
		errs []error
		seen = map[levyan.FindingKind]map[string]bool{}
	)
	for _, f := range found {
		if seen[f.Kind] == nil {
			seen[f.Kind] = map[string]bool{}
		}
		if seen[f.Kind][f.Subject] {
			continue
		}
		seen[f.Kind][f.Subject] = true

		was := p.open[f.Kind][f.Subject]
		if was == nil {
			v, err := p.db.Finding.Query().Where(entfinding.Kind(f.Kind.String()), entfinding.Subject(f.Subject)).Only(ctx)
			switch {
			case err == nil:
				was = v
			case !ent.IsNotFound(err):
				errs = append(errs, err)
				continue
			}
		}
		did, err := r.keep(ctx, p, was, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", short(f.Kind.String()), f.Subject, err))
			continue
		}
		switch did {
		case added:
			out.added++
		case changed:
			out.changed++
		case resolved:
			out.resolved++
		}
	}

	for _, k := range ks {
		for s, was := range p.open[k] {
			if seen[k][s] {
				continue
			}
			if err := r.resolve(ctx, p, was, NoLonger); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", short(k.String()), s, err))
				continue
			}
			out.resolved++
		}
	}

	return out, errors.Join(errs...)
}

type did int

const (
	nothing did = iota
	added
	changed
	resolved
)

// keep writes `f`, found by a check, over `was`, what was written of its
// subject before: nil for nothing.
func (r *Reconciler) keep(ctx context.Context, p *pass, was *ent.Finding, f Found) (did, error) {
	k := kinds[f.Kind]
	if was == nil {
		req := levyan.FindingAddRequest_builder{
			Id:         pdid.New(pd.FindingDomain).Bytes(),
			Kind:       f.Kind.String(),
			Subject:    f.Subject,
			Severity:   k.severity,
			State:      levyan.FindingState_FINDING_STATE_OPEN,
			Summary:    f.Summary,
			Facts:      f.Facts,
			Rows:       rowsOf(f.Rows),
			Amount:     f.Amount,
			Currency:   f.Currency,
			Correction: k.correction,
		}.Build()
		if f.Product != (uuid.UUID{}) {
			req.SetProduct(levyan.TenantRef_builder{Id: f.Product[:]}.Build())
		}
		if f.Resolved {
			req.SetState(levyan.FindingState_FINDING_STATE_RESOLVED)
			req.SetDateResolved(timestamppb.New(p.now))
			req.SetResolution(f.Resolution)
		}
		_, err := r.S.Finding().Add(ctx, req)
		if status.Code(err) == codes.AlreadyExists {
			// Written by another pass at the same moment: what it wrote is
			// what this one holds to.
			v, err := p.db.Finding.Query().Where(entfinding.Kind(f.Kind.String()), entfinding.Subject(f.Subject)).Only(ctx)
			if err != nil {
				return nothing, err
			}
			return r.keep(ctx, p, v, f)
		}
		if err != nil {
			return nothing, err
		}
		if f.Resolved {
			say(ctx, f, "reconcile: found, and put right")
			return resolved, nil
		}
		say(ctx, f, "reconcile: found")
		return added, nil
	}

	req := levyan.FindingPatchRequest_builder{
		Ref:              levyan.FindingRef_builder{Id: was.Id[:]}.Build(),
		DateUpdatedForce: proto.Bool(true),
	}.Build()
	same := was.Severity == int32(k.severity) && was.Summary == f.Summary && was.Facts == f.Facts &&
		sameRows(was.Rows, f.Rows) && was.Amount == f.Amount && was.Currency == f.Currency &&
		was.Correction == int32(k.correction)
	content := func() {
		req.SetSeverity(k.severity)
		req.SetSummary(f.Summary)
		req.SetFacts(f.Facts)
		req.SetRows(rowsOf(f.Rows))
		req.SetAmount(f.Amount)
		req.SetCurrency(f.Currency)
		req.SetCorrection(k.correction)
	}
	reopen := func() {
		req.SetState(levyan.FindingState_FINDING_STATE_OPEN)
		req.SetDateResolvedNull(true)
		req.SetResolution("")
		req.SetReason("")
		req.SetDateAcceptedNull(true)
		req.SetAcceptedByNull(true)
	}

	state := levyan.FindingState(was.State)
	out := changed
	switch {
	case f.Resolved:
		// Found, and put right at once: written as it was found, and
		// resolved.
		content()
		req.SetState(levyan.FindingState_FINDING_STATE_RESOLVED)
		req.SetDateResolved(timestamppb.New(p.now))
		req.SetResolution(f.Resolution)
		out = resolved
		say(ctx, f, "reconcile: found, and put right")
	case state == levyan.FindingState_FINDING_STATE_RESOLVED:
		content()
		reopen()
		say(ctx, f, "reconcile: found again")
	case state == levyan.FindingState_FINDING_STATE_ACCEPTED && was.Facts != f.Facts:
		// What was accepted is not what is so now.
		content()
		reopen()
		say(ctx, f, "reconcile: an accepted finding says something else now, and is open again")
	case same:
		return nothing, nil
	default:
		content()
	}
	if _, err := r.S.Finding().Patch(ctx, req); err != nil {
		return nothing, err
	}

	return out, nil
}

// resolve writes finding `was` resolved, `how`.
func (r *Reconciler) resolve(ctx context.Context, p *pass, was *ent.Finding, how string) error {
	_, err := r.S.Finding().Patch(ctx, levyan.FindingPatchRequest_builder{
		Ref:              levyan.FindingRef_builder{Id: was.Id[:]}.Build(),
		State:            levyan.FindingState_FINDING_STATE_RESOLVED.Enum(),
		DateResolved:     timestamppb.New(p.now),
		Resolution:       proto.String(how),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	if err != nil {
		return err
	}
	log.From(ctx).InfoContext(ctx, "reconcile: resolved, "+how,
		slog.String("finding", pdid.Id(was.Id).String()),
		slog.String("kind", short(was.Kind)),
		slog.String("subject", was.Subject),
	)

	return nil
}

// say is a finding written, in the log.
func say(ctx context.Context, f Found, msg string) {
	log.From(ctx).InfoContext(ctx, msg,
		slog.String("kind", short(f.Kind.String())),
		slog.String("subject", f.Subject),
		slog.String("summary", f.Summary),
	)
}

// sameRows says whether `was` is the rows `rs` are.
func sameRows(was *levyan.FindingRows, rs []Row) bool {
	vs := was.GetRows()
	if len(vs) != len(rs) {
		return false
	}
	for i, v := range vs {
		if v.GetEntity() != rs[i].Entity || v.GetRef() != rs[i].Ref {
			return false
		}
	}

	return true
}

func rowsOf(rs []Row) *levyan.FindingRows {
	out := &levyan.FindingRows{}
	for _, v := range rs {
		out.SetRows(append(out.GetRows(), levyan.FindingRow_builder{Entity: v.Entity, Ref: v.Ref}.Build()))
	}

	return out
}
