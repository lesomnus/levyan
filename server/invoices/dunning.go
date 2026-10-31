package invoices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/pg"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/ledger"
)

const (
	// DunLoop is the name of the loop that does what the policy table says
	// after a charge fails (#11).
	DunLoop = "levyan.invoices.dunning"

	// DefaultDunEvery is how often it runs when `dunning.every` says nothing:
	// a minute, so that a billing account that is collectable again is
	// charged within one, and a workspace whose invoice was paid is let go
	// within one too (implementation decision).
	DefaultDunEvery = time.Minute

	// RestrictionRule is the name of the grants' rule an invoice still owed
	// restricts what it bills by ([Restriction]).
	RestrictionRule = "dunning"
)

// owed is the states of an invoice finalized and still owed: what dunning
// is of.
var owed = []int32{
	int32(levyan.InvoiceState_INVOICE_STATE_OPEN),
	int32(levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE),
}

// Dunned is an invoice in dunning, and what the policy table says of it now.
type Dunned struct {
	Invoice *ent.Invoice
	dunning.State
}

// Dunning is what policy table `p` says at `now` of those of `invs` that are
// in dunning: finalized, still owed, and a charge of which failed. Their
// charges and their billing accounts' collection are read with `db`, which
// is all that is read: what the table says of an invoice is worked out from
// them every time it is asked, and written nowhere. They are answered in the
// order they were given.
func Dunning(ctx context.Context, db *ent.Client, p dunning.Policy, invs []*ent.Invoice, now time.Time) ([]Dunned, error) {
	var ids, bas []uuid.UUID
	for _, inv := range invs {
		if slices.Contains(owed, inv.State) && remaining(inv) > 0 {
			ids = append(ids, inv.Id)
			bas = append(bas, inv.BillingAccountId)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	as, err := db.PaymentAttempt.Query().
		Where(entattempt.InvoiceIdIn(ids...), entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE))).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	charges := map[uuid.UUID][]dunning.Charge{}
	for _, a := range as {
		charges[a.InvoiceId] = append(charges[a.InvoiceId], chargeOf(a))
	}
	bs, err := db.BillingAccount.Query().Where(entbillingaccount.IdIn(bas...)).All(ctx)
	if err != nil {
		return nil, err
	}
	collectable := map[uuid.UUID]time.Time{}
	for _, b := range bs {
		if b.Collection == int32(levyan.CollectionState_COLLECTION_STATE_COLLECTABLE) && b.DateCollection != nil {
			collectable[b.Id] = ledger.Moment(*b.DateCollection)
		}
	}

	var out []Dunned
	for _, inv := range invs {
		cs, ok := charges[inv.Id]
		if !ok || !slices.Contains(owed, inv.State) || remaining(inv) == 0 {
			continue
		}
		if st, ok := p.Dun(cs, collectable[inv.BillingAccountId], now); ok {
			out = append(out, Dunned{Invoice: inv, State: st})
		}
	}

	return out, nil
}

// chargeOf is charge attempt `a` as dunning reads it.
func chargeOf(a *ent.PaymentAttempt) dunning.Charge {
	out := dunning.Charge{Made: ledger.Moment(a.DateCreated)}
	if a.DateSent != nil {
		out.Sent = ledger.Moment(*a.DateSent)
	}
	switch levyan.AttemptState(a.State) {
	case levyan.AttemptState_ATTEMPT_STATE_REQUESTED, levyan.AttemptState_ATTEMPT_STATE_UNKNOWN, levyan.AttemptState_ATTEMPT_STATE_PENDING:
		out.Out = true
	case levyan.AttemptState_ATTEMPT_STATE_FAILED:
		out.Failed = ledger.Moment(a.DateUpdated)
		if a.DateResolved != nil {
			out.Failed = ledger.Moment(*a.DateResolved)
		}
		out.Decline = Decline(a.Failure)
	}

	return out
}

// Decline is the kind of decline failure `f` is, as the policy table reads
// it: a failure that is no decline -- a request the channel refused, a reason
// of its own -- is a decline that says no more than that it is one.
func Decline(f *levyan.AttemptFailure) pg.Decline {
	if f == nil || f.GetKind() != levyan.FailureKind_FAILURE_KIND_DECLINED {
		return pg.DeclineOther
	}

	return pg.Decline(f.GetDecline())
}

// AccountDunning is every invoice of billing account `ba` in `product` that is
// in dunning at `now`, by policy table `p`: the one that restricts soonest
// first.
func AccountDunning(ctx context.Context, db *ent.Client, p dunning.Policy, product, ba uuid.UUID, now time.Time) ([]Dunned, error) {
	invs, err := db.Invoice.Query().
		Where(entinvoice.TenantId(product), entinvoice.BillingAccountId(ba), entinvoice.StateIn(owed...)).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ds, err := Dunning(ctx, db, p, invs, now)
	if err != nil {
		return nil, err
	}

	return soonest(ds), nil
}

// WorkspaceDunning is every invoice of `product` in dunning at `now`, by
// policy table `p`, that bills workspace `ws` -- has a line of it, the
// workspace being one its billing account paid for in its periods -- which is
// what restricts the workspace: the one that restricts soonest first.
func WorkspaceDunning(ctx context.Context, db *ent.Client, p dunning.Policy, product, ws uuid.UUID, now time.Time) ([]Dunned, error) {
	invs, err := owedOf(ctx, db, product)
	if err != nil || len(invs) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(invs))
	for i, inv := range invs {
		ids[i] = inv.Id
	}
	ls, err := db.InvoiceLine.Query().
		Where(entinvoiceline.TenantId(product), entinvoiceline.InvoiceIdIn(ids...), entinvoiceline.WorkspaceId(ws)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	bills := map[uuid.UUID]bool{}
	for _, l := range ls {
		bills[l.InvoiceId] = true
	}
	invs = slices.DeleteFunc(invs, func(inv *ent.Invoice) bool { return !bills[inv.Id] })

	ds, err := Dunning(ctx, db, p, invs, now)
	if err != nil {
		return nil, err
	}

	return soonest(ds), nil
}

// productDunning is every invoice of `product` in dunning at `now`, the one
// that restricts soonest first.
func productDunning(ctx context.Context, db *ent.Client, p dunning.Policy, product uuid.UUID, now time.Time) ([]Dunned, error) {
	invs, err := owedOf(ctx, db, product)
	if err != nil {
		return nil, err
	}
	ds, err := Dunning(ctx, db, p, invs, now)
	if err != nil {
		return nil, err
	}

	return soonest(ds), nil
}

// owedOf is every invoice of `product` finalized and still owed, in the
// order of its periods.
func owedOf(ctx context.Context, db *ent.Client, product uuid.UUID) ([]*ent.Invoice, error) {
	return db.Invoice.Query().
		Where(entinvoice.TenantId(product), entinvoice.StateIn(owed...)).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(ctx)
}

// soonest is `ds` in the order they restrict, the soonest first.
func soonest(ds []Dunned) []Dunned {
	slices.SortStableFunc(ds, func(a, b Dunned) int { return a.Restricted.Compare(b.Restricted) })
	return ds
}

// Restriction is the grants' rule `dunning` (#11): what an invoice still owed
// bills -- every workspace of its lines, which its billing account paid for
// in its periods -- is restricted from when the policy table says, at once or
// once its grace is over, and for as long as it is owed: `<product>.restricted`
// ON, the key a workspace nobody pays for is restricted by too
// ([grants.Restricted]), each rule with a grant of its own. Paid -- by a
// retry, or any charge after -- voided, or taken back whole by a credit note,
// it is owed no more, the rule wants nothing of it, and its grants end. A
// workspace destroyed (#14) is restricted by nothing.
//
// The rule is followed by the grants' loop with every other, and by the loop
// [DunLoop] each pass, so that a payment lets a workspace go within a pass
// rather than within the grants' hour.
type Restriction struct {
	// Policy is the table.
	Policy dunning.Policy
}

var _ grants.Rule = Restriction{}

func (Restriction) Name() string { return RestrictionRule }

func (r Restriction) Want(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) ([]grants.Want, error) {
	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return nil, err
	}
	key, err := grants.Restricted(t.Alias)
	if err != nil {
		return nil, err
	}

	ds, err := productDunning(ctx, db, r.Policy, product, now)
	if err != nil {
		return nil, err
	}
	ds = slices.DeleteFunc(ds, func(d Dunned) bool { return now.Before(d.Restricted) })
	if len(ds) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(ds))
	for i, d := range ds {
		ids[i] = d.Invoice.Id
	}
	ls, err := db.InvoiceLine.Query().
		Where(entinvoiceline.TenantId(product), entinvoiceline.InvoiceIdIn(ids...)).
		Order(ent.Asc(entinvoiceline.FieldDateStart), ent.Asc(entinvoiceline.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	bills := map[uuid.UUID][]uuid.UUID{}
	var wss []uuid.UUID
	for _, l := range ls {
		bills[l.InvoiceId] = append(bills[l.InvoiceId], l.WorkspaceId)
		wss = append(wss, l.WorkspaceId)
	}
	gone, err := ledger.Destroyed(ctx, db, product, wss...)
	if err != nil {
		return nil, err
	}

	var out []grants.Want
	seen := map[uuid.UUID]bool{}
	for _, d := range ds {
		for _, ws := range bills[d.Invoice.Id] {
			if _, ok := gone[ws]; ok || seen[ws] {
				continue
			}
			seen[ws] = true
			out = append(out, grants.Want{
				Workspace: ws,
				Key:       key,
				Value:     entitle.On,
				Desc:      fmt.Sprintf("청구서 %s의 결제가 %s부터 실패했다", pdid.Id(d.Invoice.Id), d.Failed.UTC().Format(time.RFC3339)),
			})
		}
	}

	return out, nil
}

// Dun is one pass of dunning (#11), through the stack, as whoever ctx is
// framed as -- the deployment, by the loop [DunLoop]: of every invoice still
// owed a charge of which failed, on a channel that pushes, what the policy
// table says is due is made and sent at once through its channel -- a retry
// whose time has come, or a charge as its billing account is collectable
// again -- and what has spent its retries and its grace is given up on,
// uncollectible. Then, when `g` is given, what the table restricts is
// restricted and what it no longer does is let go ([Restriction], through
// `g`).
//
// Each invoice is decided again in a transaction of its own that holds its
// billing account's row, as every write of its attempts does, so two passes
// at once make one charge, and a charge made covers what it was made for
// however often the table is asked. What fails is said in the log and tried
// again by the next pass; one invoice's does not stop another's.
func (v Invoices) Dun(ctx context.Context, g *grants.Grants) error {
	var errs []error

	ds, err := v.dunned(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, d := range ds {
		if _, c := v.collector(d.Invoice.Channel); c == nil || !c.Pushes() {
			continue
		}
		if !d.Due && !(d.Spent && d.Invoice.State == int32(levyan.InvoiceState_INVOICE_STATE_OPEN)) {
			continue
		}
		a, err := v.dun(ctx, d.Invoice.Id, d.Invoice.BillingAccountId)
		if err != nil {
			logErr(ctx, "invoices: what dunning says of an invoice was not done", err, slog.String("invoice", pdid.Id(d.Invoice.Id).String()))
			errs = append(errs, err)
			continue
		}
		if a == nil {
			continue
		}
		if err := v.drive(ctx, a); err != nil {
			logErr(ctx, "invoices: a charge dunning made was not sent", err, slog.String("attempt", pdid.Id(a.Id).String()))
			errs = append(errs, err)
		}
	}

	if g != nil {
		if err := g.Follow(ctx, Restriction{Policy: v.Dunning}); err != nil {
			errs = append(errs, fmt.Errorf("restriction: %w", err))
		}
	}

	return errors.Join(errs...)
}

// dunned is every invoice in dunning now: still owed, a charge of which
// failed.
func (v Invoices) dunned(ctx context.Context) ([]Dunned, error) {
	as, err := v.db().PaymentAttempt.Query().
		Where(
			entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)),
			entattempt.State(int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)),
			entattempt.HasInvoiceWith(entinvoice.StateIn(owed...)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, a := range as {
		if !slices.Contains(ids, a.InvoiceId) {
			ids = append(ids, a.InvoiceId)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	invs, err := v.db().Invoice.Query().
		Where(entinvoice.IdIn(ids...)).
		Order(ent.Asc(entinvoice.FieldDateFinalized), ent.Asc(entinvoice.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	return Dunning(ctx, v.db(), v.Dunning, invs, v.now())
}

// dun does what the table says of invoice `id` now, decided again on the
// transaction that holds its billing account `ba`'s row: the charge that is
// due written, to be sent, and answered -- or the invoice given up on.
func (v Invoices) dun(ctx context.Context, id, ba uuid.UUID) (*ent.PaymentAttempt, error) {
	var made uuid.UUID
	err := v.held(ctx, ba, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		inv, err := db.Invoice.Get(ctx, id)
		if err != nil {
			return err
		}
		ds, err := Dunning(ctx, db, v.Dunning, []*ent.Invoice{inv}, v.now())
		if err != nil || len(ds) == 0 {
			return errors.Join(err, errSkip)
		}
		d := ds[0]
		attrs := []any{
			slog.String("invoice", pdid.Id(inv.Id).String()),
			slog.String("decline", d.Kind.String()),
			slog.Time("failed", d.Failed),
		}

		switch {
		case d.Due:
			a, err := addAttempt(ctx, s, inv, levyan.AttemptKind_ATTEMPT_KIND_CHARGE, remaining(inv), uuid.UUID{})
			if err != nil {
				return err
			}
			at, err := pdid.From(a.GetId())
			if err != nil {
				return err
			}
			made = at.Uuid()
			log.From(ctx).InfoContext(ctx, "invoices: an invoice whose charge failed is charged again", append(attrs, slog.Time("for", d.Retry))...)
			return nil

		case d.Spent && inv.State == int32(levyan.InvoiceState_INVOICE_STATE_OPEN):
			if _, err := s.Invoice().Patch(ctx, levyan.InvoicePatchRequest_builder{
				Ref:              levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
				State:            levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE.Enum(),
				DateUpdatedForce: proto.Bool(true),
			}.Build()); err != nil {
				return fmt.Errorf("invoice %s: %w", pdid.Id(inv.Id), err)
			}
			log.From(ctx).WarnContext(ctx, "invoices: an invoice spent its retries and its grace unpaid, and is uncollectible", attrs...)
			return nil
		}

		return errSkip
	})
	if err != nil || made == (uuid.UUID{}) {
		return nil, err
	}

	return v.db().PaymentAttempt.Get(ctx, made)
}

// Duns is the loop [DunLoop]: [Invoices.Dun] every `every`, of `v` as it is
// at each, restricting through `g`, in one process.
func Duns(v *Invoices, g *grants.Grants, every time.Duration) spin.Spinner {
	return spin.Called(DunLoop, spin.Every(every, func(ctx context.Context) error {
		if err := v.Dun(ctx, g); err != nil {
			log.From(ctx).WarnContext(ctx, "invoices: a pass of dunning", slog.Any("err", err))
		}
		return nil
	}))
}
