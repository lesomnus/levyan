package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/usage"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// periods finds every product's period that is not closed longer than
// [Stale.Period] after its end and the grace it waits for what comes late
// (`periods.grace`): nothing of it is billed until it is. A product priced
// nothing is closed never, and is not found.
func (r *Reconciler) periods(ctx context.Context, p *pass) ([]Found, error) {
	ps, err := p.db.Tenant.Query().Order(ent.Asc(enttenant.FieldId)).All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, t := range ps {
		next, ok, err := r.Closer.Next(ctx, t)
		if err != nil {
			out = append(out, Found{
				Kind:    levyan.FindingKind_FINDING_KIND_PERIOD_UNCLOSED,
				Subject: "Period/" + pdid.Id(t.Id).String(),
				Product: t.Id,
				Summary: fmt.Sprintf("no period of %s can be closed: %s", t.Alias, firstLine(err)),
				Facts:   firstLine(err),
				Rows:    []Row{RowOf("Tenant", t.Id)},
			})
			continue
		}
		if !ok {
			continue
		}
		due := next.End.Add(r.Closer.Grace).Add(or(r.Stale.Period, DefaultPeriod))
		if p.now.Before(due) {
			continue
		}
		why := "and no close of it began"
		row, err := p.db.Period.Query().Where(entperiod.TenantId(t.Id), entperiod.DateStart(next.Start)).Only(ctx)
		switch {
		case err == nil:
			why = "and its close, begun at " + stamp(row.DateCreated) + ", has not ended"
		case !ent.IsNotFound(err):
			return nil, err
		}
		f := Found{
			Kind:    levyan.FindingKind_FINDING_KIND_PERIOD_UNCLOSED,
			Subject: "Period/" + pdid.Id(t.Id).String() + "/" + stamp(next.Start),
			Product: t.Id,
			Summary: fmt.Sprintf("the period of %s from %s to %s was to be closed by %s, %s", t.Alias, stamp(next.Start), stamp(next.End), stamp(due), why),
			Facts:   fmt.Sprintf("start=%s end=%s %s", stamp(next.Start), stamp(next.End), why),
			Rows:    []Row{RowOf("Tenant", t.Id)},
		}
		if row != nil {
			f.Rows = append(f.Rows, RowOf("Period", row.Id))
		}
		out = append(out, f)
	}

	return out, nil
}

// drafts finds every draft that should have been decided and was not: its
// last period closed longer than [Stale.Draft] ago, and it not waiting for
// the next period for being less than its channel collects. A draft whose
// period is not closed is the period's finding.
func (r *Reconciler) drafts(ctx context.Context, p *pass) ([]Found, error) {
	before := p.now.Add(-or(r.Stale.Draft, DefaultDraft))
	ds, err := p.db.Invoice.Query().
		Where(entinvoice.State(int32(levyan.InvoiceState_INVOICE_STATE_DRAFT))).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, d := range ds {
		if d.DateCarried != nil && d.DateCarried.Equal(d.DateEnd) {
			// Waiting for the next period, as it should.
			continue
		}
		pd, err := p.db.Period.Query().
			Where(entperiod.TenantId(d.TenantId), entperiod.DateEnd(d.DateEnd), entperiod.DateClosedNotNil()).
			First(ctx)
		if ent.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !pd.DateClosed.Before(before) {
			continue
		}
		why := "and it was not decided"
		if r.Invoices != nil && r.Invoices.Collectors[levyan.Channel(levyan.Channel_value[d.Channel])] == nil {
			why = "and nothing collects on its channel, " + d.Channel + ", so its drafts wait"
		}
		out = append(out, Found{
			Kind:    levyan.FindingKind_FINDING_KIND_DRAFT_STALE,
			Subject: Subject("Invoice", d.Id),
			Product: d.TenantId,
			Summary: fmt.Sprintf("draft %s of billing account %s, of %s to %s, came to %s, its last period closed at %s, %s",
				pdid.Id(d.Id), pdid.Id(d.BillingAccountId), stamp(d.DateStart), stamp(d.DateEnd), money.String(d.Subtotal, d.Currency), stamp(*pd.DateClosed), why),
			Facts:    fmt.Sprintf("end=%s subtotal=%d %s", stamp(d.DateEnd), d.Subtotal, why),
			Rows:     []Row{RowOf("Invoice", d.Id), RowOf("BillingAccount", d.BillingAccountId), RowOf("Period", pd.Id)},
			Amount:   d.Subtotal,
			Currency: d.Currency,
		})
	}

	return out, nil
}

// most is how many of the workspaces a finding of missing readings names
// among its rows.
const most = 20

// readings finds, of each product's stock meters, the workspaces that have
// gone without a reading of it for longer than a reading holds (`usage.Hold`,
// the rating's, so that what is found is what is billed short): of the
// workspaces that have sent a reading of any of the product's stock meters
// within [Reconciler.Recent], each meter any of them sent one of in it, that
// the workspace has no reading of since a hold ago -- one that stopped, and
// one that never sent it. A workspace closed, or destroyed, is billed nothing
// while it is, and is not looked for (#14). One finding of each product and
// meter, with how many and which: a product whose reader stopped is every
// workspace at once.
func (r *Reconciler) readings(ctx context.Context, p *pass) ([]Found, error) {
	recent := or(r.Recent, 7*24*time.Hour)
	ps, err := p.db.Tenant.Query().Order(ent.Asc(enttenant.FieldId)).All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, t := range ps {
		ms, err := p.db.Meter.Query().
			Where(entmeter.TenantId(t.Id), entmeter.Kind(int32(levyan.MeterKind_METER_KIND_STOCK))).
			Order(ent.Asc(entmeter.FieldKey)).
			All(ctx)
		if err != nil || len(ms) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		ids := []uuid.UUID{}
		for _, m := range ms {
			ids = append(ids, m.Id)
		}

		var vs []struct {
			WorkspaceId key     `json:"workspace_id"`
			MeterId     key     `json:"meter_id"`
			Last        instant `json:"last"`
		}
		err = p.db.Reading.Query().
			Where(entreading.TenantId(t.Id), entreading.MeterIdIn(ids...), entreading.MeasuredAtGTE(p.now.Add(-recent))).
			GroupBy(entreading.FieldWorkspaceId, entreading.FieldMeterId).
			Aggregate(ent.As(ent.Max(entreading.FieldMeasuredAt), "last")).
			Scan(ctx, &vs)
		if err != nil {
			return nil, err
		}

		last := map[[2]uuid.UUID]time.Time{}
		ws, sent := []uuid.UUID{}, map[uuid.UUID]bool{}
		for _, v := range vs {
			w, m := uuid.UUID(v.WorkspaceId), uuid.UUID(v.MeterId)
			last[[2]uuid.UUID{w, m}] = ledger.Moment(time.Time(v.Last))
			if !slices.Contains(ws, w) {
				ws = append(ws, w)
			}
			sent[m] = true
		}
		lives, err := billing.Lives(ctx, p.db, t.Id, ws...)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(ws, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })

		for _, m := range ms {
			if !sent[m.Id] {
				continue
			}
			missing, never, oldest := []uuid.UUID{}, 0, time.Time{}
			for _, w := range ws {
				if l, ok := lives[w]; ok && !l.Closed.IsZero() {
					continue
				}
				at, ok := last[[2]uuid.UUID{w, m.Id}]
				switch {
				case !ok:
					never++
				case p.now.Sub(at) <= usage.Hold:
					continue
				case oldest.IsZero() || at.Before(oldest):
					oldest = at
				}
				missing = append(missing, w)
			}
			if len(missing) == 0 {
				continue
			}

			h := sha256.New()
			rows := []Row{RowOf("Meter", m.Id)}
			for i, w := range missing {
				h.Write(w[:])
				if i < most {
					rows = append(rows, RowOf("Workspace", w))
				}
			}
			summary := fmt.Sprintf("%d workspaces of %s have sent no reading of %s for longer than a reading holds (%s), and are billed short", len(missing), t.Alias, m.Key, usage.Hold)
			if never > 0 {
				summary += fmt.Sprintf(": %d of them never sent one", never)
			}
			if !oldest.IsZero() {
				summary += ", and the longest since " + stamp(oldest)
			}
			out = append(out, Found{
				Kind:    levyan.FindingKind_FINDING_KIND_READINGS_MISSING,
				Subject: Subject("Meter", m.Id),
				Product: t.Id,
				Summary: summary,
				Facts:   fmt.Sprintf("workspaces=%d never=%d of=%s", len(missing), never, hex.EncodeToString(h.Sum(nil))[:16]),
				Rows:    rows,
			})
		}
	}

	return out, nil
}

// key is an identifier an aggregate answers, which a SQL driver may answer
// as bytes or as text.
type key uuid.UUID

func (v *key) Scan(src any) error {
	switch u := src.(type) {
	case [16]byte:
		*v = key(u)
	case []byte:
		if len(u) == 16 {
			*v = key(uuid.UUID(u))
			return nil
		}
		return v.Scan(string(u))
	case string:
		id, err := uuid.Parse(u)
		if err != nil {
			return fmt.Errorf("reconcile: %q is not an identifier: %w", u, err)
		}
		*v = key(id)
	default:
		return fmt.Errorf("reconcile: %T is not an identifier", src)
	}

	return nil
}

// instant is a moment an aggregate answers, which SQLite answers as
// nanoseconds.
type instant time.Time

func (v *instant) Scan(src any) error {
	switch u := src.(type) {
	case nil:
		*v = instant{}
	case time.Time:
		*v = instant(u)
	case int64:
		*v = instant(time.Unix(0, u))
	case float64:
		*v = instant(time.Unix(0, int64(u)))
	case []byte:
		return v.Scan(string(u))
	case string:
		t, err := time.Parse(time.RFC3339Nano, u)
		if err != nil {
			return fmt.Errorf("reconcile: %q is not a moment: %w", u, err)
		}
		*v = instant(t)
	default:
		return fmt.Errorf("reconcile: %T is not a moment", src)
	}

	return nil
}
