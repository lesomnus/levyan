package invoices

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/ent/dialect/sql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/benefits"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// What an operator grants (#43): money taken off what is billed, with a
// reason and an expiry, to a billing account in a product or to one
// workspace of it -- a lot of the credit ledger of its own, apart from the
// balance (`Credit`). That the owner wants it is the owner's (2026-11-01);
// its shape -- an expiry always, a year unless said, the workspace's by
// default or the whole billing account's, granted by an operator alone, the
// soonest to expire spent first -- is Claude's proposal, which the owner had
// carried out ("진행해줘").
//
// A lot is spent as an invoice is decided ([Invoices.Account]), before the
// balance, of the total -- what the channel is asked for is what is left,
// and nothing else of the invoice moves: on the merchant of record, whose
// tax is on what it is asked for, it is a discount; on a channel levyan sells
// on, it pays the total as the balance does, its tax invoice for the supply
// in full -- whether a promotion's credit is rather an 에누리, off the supply,
// is open (docs/design.md §17, 128). A shadow's draft spends none of it, and
// what a credit note takes back of what it paid comes back to it
// ([Invoices.Credit], [Invoices.Void]).
//
// A lot pays an invoice whose period began before it expired -- the earliest
// of them, for a draft that waited and covers several -- however long after
// the invoice is decided: what it was granted for was the usage of the time
// it was in force, and a period is decided only once it has closed (Claude's,
// 2026-11-02, docs/design.md §17, 129). What is left of it is taken off by a
// movement of its own once nothing may spend it any more ([Invoices.Expired])
// -- no period of its product that began before it expired is still to
// close, nor any statement of its scope of one still to be invoiced, nor any
// draft or open invoice of its scope begun before -- so that every lot comes
// to nothing in the end and says where all of it went. An operator who ends
// one ends it at once ([Invoices.Expire]): nothing more of it is spent, for
// any period.

// Lot is a lot of credit an operator granted, and what is left of it.
type Lot struct {
	Row  *ent.Credit
	Left int64

	// Ended is when it was ended before it expired -- what is left of it
	// taken off by an operator -- and zero for one nobody ended: nothing more
	// of it is spent from then, and what comes back to it after is taken off
	// it again.
	Ended time.Time
}

// Pays says whether the lot may pay an invoice whose first period began at
// `start`: one that began before it expired, of a lot nobody ended.
func (l Lot) Pays(start time.Time) bool {
	return l.Ended.IsZero() && l.Expires().After(ledger.Moment(start))
}

// Workspace is the workspace the lot is of, and none for a billing
// account's.
func (l Lot) Workspace() uuid.UUID { return l.Row.WorkspaceId }

// Expires is when it expires.
func (l Lot) Expires() time.Time {
	if l.Row.DateExpires == nil {
		return time.Time{}
	}

	return ledger.Moment(*l.Row.DateExpires)
}

// held is what is left of a lot, and when it was taken off before it
// expired, if it was.
type held struct {
	left  int64
	ended time.Time
}

// lefts is what is left of each lot of `ids` -- what its movements add up to
// -- and when the first of what was taken off it as it expired was, which
// for one still in force is when an operator ended it.
func lefts(ctx context.Context, db *ent.Client, ids ...uuid.UUID) (map[uuid.UUID]held, error) {
	out := map[uuid.UUID]held{}
	if len(ids) == 0 {
		return out, nil
	}
	vs, err := db.Credit.Query().
		Where(entcredit.LotIn(ids...)).
		Select(entcredit.FieldLot, entcredit.FieldAmount, entcredit.FieldKind, entcredit.FieldDateCreated).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		h := out[*v.Lot]
		h.left += v.Amount
		if at := ledger.Moment(v.DateCreated); v.Kind == int32(levyan.CreditKind_CREDIT_KIND_EXPIRED) && (h.ended.IsZero() || at.Before(h.ended)) {
			h.ended = at
		}
		out[*v.Lot] = h
	}

	return out, nil
}

// Lots is every lot of `product` that `where` names -- each grant, and what
// is left of it -- the soonest to expire first.
func Lots(ctx context.Context, db *ent.Client, product uuid.UUID, where ...predicate.Credit) ([]Lot, error) {
	q := db.Credit.Query().
		Where(entcredit.TenantId(product), entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_GRANTED)))
	for _, w := range where {
		q.Where(w)
	}
	vs, err := q.Order(ent.Asc(entcredit.FieldDateExpires), ent.Asc(entcredit.FieldDateCreated), ent.Asc(entcredit.FieldId)).All(ctx)
	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(vs))
	for _, v := range vs {
		ids = append(ids, v.Id)
	}
	left, err := lefts(ctx, db, ids...)
	if err != nil {
		return nil, err
	}

	out := make([]Lot, 0, len(vs))
	for _, v := range vs {
		out = append(out, lotOf(v, left[v.Id]))
	}

	return out, nil
}

// lotOf is lot `c` with what `h` says is left of it.
func lotOf(c *ent.Credit, h held) Lot {
	l := Lot{Row: c, Left: h.left}
	if !h.ended.IsZero() && (c.DateExpires == nil || h.ended.Before(ledger.Moment(*c.DateExpires))) {
		l.Ended = h.ended
	}

	return l
}

// Live is what of `ls` is left to spend at `now`: not spent, nor ended, and
// in force -- or expired, and still owed to a period that began before it
// did, and that is not invoiced yet ([pending]).
func Live(ctx context.Context, db *ent.Client, ls []Lot, now time.Time) ([]Lot, error) {
	out := []Lot{}
	for _, l := range ls {
		if l.Left <= 0 || !l.Ended.IsZero() {
			continue
		}
		if !l.Expires().After(now) {
			ok, err := pending(ctx, db, l.Row, false)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, l)
	}

	return out, nil
}

// pending says whether something may still spend lot `c`: a period of its
// product that began before the lot expired and is not closed yet, a
// statement of the lot's scope of such a period that is on no invoice yet,
// or a draft of its scope whose first period began before the lot expired --
// and, with `open`, an open invoice of its scope whose first period did,
// which spends no credit itself but may give some back (Claude's,
// 2026-11-02). The scope of a billing account's lot is that billing
// account's statements and invoices in the product; of a workspace's, that
// workspace's statements and the invoices that bill it, whoever's.
func pending(ctx context.Context, db *ent.Client, c *ent.Credit, open bool) (bool, error) {
	if c.DateExpires == nil {
		return false, nil
	}
	expires := ledger.Moment(*c.DateExpires)

	// A period that began before it expired, not closed yet: its statements
	// are still to come.
	next, ok, err := unclosed(ctx, db, c.TenantId)
	if err != nil {
		return false, err
	}
	if ok && next.Before(expires) {
		return true, nil
	}

	states := []int32{int32(levyan.InvoiceState_INVOICE_STATE_DRAFT)}
	if open {
		states = append(states, int32(levyan.InvoiceState_INVOICE_STATE_OPEN))
	}
	if c.WorkspaceId != (uuid.UUID{}) {
		ok, err := db.Statement.Query().
			Where(
				entstatement.TenantId(c.TenantId),
				entstatement.WorkspaceId(c.WorkspaceId),
				entstatement.BillingAccountIdNotNil(),
				entstatement.DateStartLT(expires),
				Unlined(),
			).
			Exist(ctx)
		if err != nil || ok {
			return ok, err
		}
		return db.InvoiceLine.Query().
			Where(
				entinvoiceline.TenantId(c.TenantId),
				entinvoiceline.WorkspaceId(c.WorkspaceId),
				entinvoiceline.HasInvoiceWith(entinvoice.StateIn(states...), entinvoice.DateStartLT(expires)),
			).
			Exist(ctx)
	}

	ok, err = db.Statement.Query().
		Where(
			entstatement.TenantId(c.TenantId),
			entstatement.BillingAccountId(c.BillingAccountId),
			entstatement.DateStartLT(expires),
			Unlined(),
		).
		Exist(ctx)
	if err != nil || ok {
		return ok, err
	}
	return db.Invoice.Query().
		Where(
			entinvoice.TenantId(c.TenantId),
			entinvoice.BillingAccountId(c.BillingAccountId),
			entinvoice.StateIn(states...),
			entinvoice.DateStartLT(expires),
		).
		Exist(ctx)
}

// unclosed is when the first of `product`'s periods not closed yet begins --
// one whose close began and did not end, or the one after the last closed,
// or the month its ledger begins in -- and false when no period of it will
// close: it is priced nothing, or its ledger says nothing yet.
func unclosed(ctx context.Context, db *ent.Client, product uuid.UUID) (time.Time, bool, error) {
	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return time.Time{}, false, err
	}
	terms, err := billing.TermsOf(t)
	if err != nil || !terms.Priced() {
		return time.Time{}, false, err
	}

	last, err := db.Period.Query().Where(entperiod.TenantId(product)).Order(ent.Desc(entperiod.FieldDateStart)).First(ctx)
	switch {
	case err == nil:
		if last.DateClosed == nil {
			return ledger.Moment(last.DateStart), true, nil
		}
		return ledger.Moment(last.DateEnd), true, nil
	case !ent.IsNotFound(err):
		return time.Time{}, false, err
	}

	first, ok, err := billing.Begins(ctx, db, product)
	if err != nil || !ok {
		return time.Time{}, false, err
	}

	return billing.MonthOf(first, terms.Zone).Start, true, nil
}

// Spendable is what billing account `ba` may spend of what was granted in
// `product` at `now`: its own lots, and the lots of the workspaces it pays
// for now or will from the end of the period -- what its next invoice may be
// paid with, the soonest to expire first.
func Spendable(ctx context.Context, db *ent.Client, product, ba uuid.UUID, now time.Time) ([]Lot, error) {
	ls, err := db.Link.Query().
		Where(
			entlink.TenantId(product),
			entlink.BillingAccountId(ba),
			entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(now)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	wss := []uuid.UUID{}
	for _, l := range ls {
		if l.DateEnd == nil || l.DateEnd.After(l.DateStart) {
			wss = append(wss, l.WorkspaceId)
		}
	}

	vs, err := Lots(ctx, db, product, entcredit.Or(
		entcredit.And(entcredit.BillingAccountId(ba), entcredit.WorkspaceIdIsNil()),
		entcredit.WorkspaceIdIn(wss...),
	))
	if err != nil {
		return nil, err
	}

	return Live(ctx, db, vs, now)
}

// take is what of an invoice one lot -- or the balance, for none -- paid.
type take struct {
	lot    uuid.UUID
	amount int64
}

// spend is what credit pays of invoice `inv`'s total, `total`, and of
// which: of the lots granted in its product and its currency to its billing
// account or to a workspace of its lines, that pay it -- nobody ended them,
// and they expire after its first period began ([Lot.Pays]) -- the soonest
// to expire first, a workspace's no more than what that workspace's lines
// come to with their tax at `rate`; and then the balance, which never
// expires. On PostgreSQL each lot spent of is held
// until the transaction ends, so that two billing accounts' invoices of one
// workspace never both spend the same of its lot; the billing account's row
// is held already.
//
// A workspace's lot pays no more than what is left of what its workspace's
// lines come to, less what its other lots paid: what a billing account's own
// lot or the balance paid is the invoice's as a whole, and is taken to have
// paid the rest of it first -- which keeps every workspace's lot to its own
// lines, and spends it as far as they go.
func (v Invoices) spend(ctx context.Context, db *ent.Client, inv *ent.Invoice, total int64, rate *big.Rat) ([]take, int64, error) {
	if total <= 0 {
		return nil, 0, nil
	}

	lines, err := db.InvoiceLine.Query().Where(entinvoiceline.InvoiceId(inv.Id)).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	of := map[uuid.UUID]int64{}
	for _, l := range lines {
		of[l.WorkspaceId] += l.Total
	}
	share := map[uuid.UUID]int64{}
	wss := []uuid.UUID{}
	for ws, n := range of {
		if n > 0 {
			share[ws] = n + TaxOf(rate, n)
		}
		wss = append(wss, ws)
	}

	cands, err := db.Credit.Query().
		Where(
			entcredit.TenantId(inv.TenantId),
			entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_GRANTED)),
			entcredit.Currency(inv.Currency),
			entcredit.DateExpiresGT(inv.DateStart),
			entcredit.Or(
				entcredit.And(entcredit.BillingAccountId(inv.BillingAccountId), entcredit.WorkspaceIdIsNil()),
				entcredit.WorkspaceIdIn(wss...),
			),
		).
		Order(ent.Asc(entcredit.FieldId)).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	if v.postgres() && len(cands) > 0 {
		ids := make([]uuid.UUID, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.Id)
		}
		q := db.Credit.Query().Where(entcredit.IdIn(ids...)).Order(ent.Asc(entcredit.FieldId))
		q.Modify(func(s *sql.Selector) { s.ForUpdate() })
		if _, err := q.Ids(ctx); err != nil {
			return nil, 0, err
		}
	}
	ids := make([]uuid.UUID, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Id)
	}
	left, err := lefts(ctx, db, ids...)
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(cands, func(a, b *ent.Credit) int {
		return cmp.Or(a.DateExpires.Compare(*b.DateExpires), a.DateCreated.Compare(b.DateCreated), slices.Compare(a.Id[:], b.Id[:]))
	})

	out := []take{}
	rest := total
	for _, c := range cands {
		if rest == 0 {
			break
		}
		if !lotOf(c, left[c.Id]).Pays(inv.DateStart) {
			continue
		}
		n := min(left[c.Id].left, rest)
		if c.WorkspaceId != (uuid.UUID{}) {
			n = min(n, share[c.WorkspaceId])
			share[c.WorkspaceId] -= max(n, 0)
		}
		if n <= 0 {
			continue
		}
		out = append(out, take{lot: c.Id, amount: n})
		rest -= n
	}

	if rest > 0 {
		bal, err := Balance(ctx, db, inv.TenantId, inv.BillingAccountId)
		if err != nil {
			return nil, 0, err
		}
		if n := min(max(bal, 0), rest); n > 0 {
			out = append(out, take{amount: n})
			rest -= n
		}
	}

	return out, total - rest, nil
}

// applied is what each lot paid of invoice `inv`, net of what came back to
// it since -- and none for the balance's part.
func applied(ctx context.Context, db *ent.Client, inv uuid.UUID) (map[uuid.UUID]int64, error) {
	vs, err := db.Credit.Query().
		Where(
			entcredit.InvoiceId(inv),
			entcredit.LotNotNil(),
			entcredit.KindIn(int32(levyan.CreditKind_CREDIT_KIND_APPLIED), int32(levyan.CreditKind_CREDIT_KIND_RETURNED)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]int64{}
	for _, c := range vs {
		out[*c.Lot] -= c.Amount
	}

	return out, nil
}

// returns is what of `credited`, which a credit note of invoice `inv` gives
// back to credit, goes back to the lots that paid the invoice -- as far as
// each paid it, the latest to expire first, so that what comes back lasts
// as long as it may -- and the rest to the balance. A lot's credit never
// becomes the balance's, which never expires, nor money given back.
func returns(ctx context.Context, db *ent.Client, inv uuid.UUID, credited int64) ([]take, error) {
	if credited <= 0 {
		return nil, nil
	}
	paid, err := applied(ctx, db, inv)
	if err != nil || len(paid) == 0 {
		return nil, err
	}
	ids := []uuid.UUID{}
	for id, n := range paid {
		if n > 0 {
			ids = append(ids, id)
		}
	}
	lots, err := db.Credit.Query().Where(entcredit.IdIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(lots, func(a, b *ent.Credit) int {
		return cmp.Or(b.DateExpires.Compare(*a.DateExpires), b.DateCreated.Compare(a.DateCreated), slices.Compare(b.Id[:], a.Id[:]))
	})

	out := []take{}
	for _, l := range lots {
		if credited == 0 {
			break
		}
		n := min(paid[l.Id], credited)
		out = append(out, take{lot: l.Id, amount: n})
		credited -= n
	}

	return out, nil
}

// Grant is credit an operator grants.
type Grant struct {
	Product pdid.Id

	// BillingAccount is the billing account it is granted to, the whole of
	// it; or Workspace, the workspace of the product it is granted to, by
	// the product's identifier of it -- whose lines alone it pays, whoever
	// pays for it. One of the two.
	BillingAccount pdid.Id
	Workspace      pdid.Id

	// Amount is how much, in the currency's minor unit, and Currency what
	// in: a billing account's, which it is billed in; and of a workspace's,
	// unless it is said, what the billing account that pays for it now is
	// billed in, or the product's.
	Amount   int64
	Currency string

	// Expires is when it expires: a year from now when zero (Claude's
	// proposal, #43).
	Expires time.Time

	// Reason is why, in the operator's words.
	Reason string
}

// Grant grants a lot of credit, as the operator `ctx` is framed as.
func (v Invoices) Grant(ctx context.Context, g Grant) (Lot, error) {
	who, err := benefits.Operator(ctx)
	if err != nil {
		return Lot{}, err
	}
	reason := strings.TrimSpace(g.Reason)
	now := v.now()
	expires := ledger.Moment(g.Expires)
	if g.Expires.IsZero() {
		expires = now.AddDate(1, 0, 0)
	}
	switch {
	case reason == "":
		return Lot{}, status.Error(codes.InvalidArgument, "reason: why it is granted, in your words")
	case g.Amount <= 0:
		return Lot{}, status.Error(codes.InvalidArgument, "amount: more than nothing, in the currency's minor unit")
	case g.BillingAccount.IsZero() == g.Workspace.IsZero():
		return Lot{}, status.Error(codes.InvalidArgument, "a credit is granted to a workspace, or to a billing account: one of the two")
	case !expires.After(now):
		return Lot{}, status.Errorf(codes.InvalidArgument, "expires: %s is not after now", expires.Format(time.RFC3339))
	}
	product, err := v.db().Tenant.Get(ctx, g.Product.Uuid())
	if ent.IsNotFound(err) {
		return Lot{}, status.Errorf(codes.NotFound, "no product is %s", g.Product)
	}
	if err != nil {
		return Lot{}, err
	}

	id := pdid.New(pd.CreditDomain)
	req := levyan.CreditAddRequest_builder{
		Id:          id.Bytes(),
		Tenant:      levyan.TenantRef_builder{Id: g.Product.Bytes()}.Build(),
		Amount:      g.Amount,
		Kind:        levyan.CreditKind_CREDIT_KIND_GRANTED,
		Lot:         id.Bytes(),
		DateExpires: timestamppb.New(expires),
		Reason:      proto.String(reason),
		Operator:    levyan.OperatorRef_builder{Id: who.Bytes()}.Build(),
	}.Build()

	write := func(s levyan.Server, db *ent.Client, currency string) error {
		currency = strings.ToUpper(strings.TrimSpace(currency))
		if err := money.Check(currency); err != nil {
			return status.Errorf(codes.InvalidArgument, "currency: %s", err)
		}
		req.SetCurrency(currency)
		if _, err := s.Credit().Add(ctx, req); err != nil {
			return fmt.Errorf("credit: %w", err)
		}
		return nil
	}

	if !g.BillingAccount.IsZero() {
		err = v.held(ctx, g.BillingAccount.Uuid(), func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
			ba, err := db.BillingAccount.Get(ctx, g.BillingAccount.Uuid())
			if ent.IsNotFound(err) {
				return status.Errorf(codes.NotFound, "no billing account is %s", g.BillingAccount)
			}
			if err != nil {
				return err
			}
			if c := strings.ToUpper(strings.TrimSpace(g.Currency)); c != "" && c != ba.Currency {
				return status.Errorf(codes.InvalidArgument, "currency: billing account %s is billed in %s, and a credit of it is in what it is billed in", g.BillingAccount, ba.Currency)
			}
			req.SetBillingAccount(levyan.BillingAccountRef_builder{Id: ba.Id[:]}.Build())
			return write(s, db, ba.Currency)
		})
	} else {
		var ws uuid.UUID
		if ws, err = ledger.Known(ctx, v.S, v.db(), g.Product, g.Workspace); err != nil {
			return Lot{}, err
		}
		err = v.tx(ctx, func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
			if err := ledger.Lock(ctx, db, v.postgres(), ws); err != nil {
				return err
			}
			d, err := ledger.Destroyed(ctx, db, g.Product.Uuid(), ws)
			if err != nil {
				return err
			}
			if at, ok := d[ws]; ok {
				return status.Errorf(codes.FailedPrecondition, "the workspace was destroyed at %s, and is granted nothing", at.Format(time.RFC3339))
			}
			currency := g.Currency
			if strings.TrimSpace(currency) == "" {
				currency = product.Currency
				l, err := db.Link.Query().
					Where(entlink.TenantId(g.Product.Uuid()), entlink.WorkspaceId(ws), entlink.DateStartLTE(now), entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(now))).
					WithBillingAccount().
					Order(ent.Desc(entlink.FieldDateStart)).
					First(ctx)
				switch {
				case err == nil:
					if b := l.Edges.BillingAccount; b != nil && b.Currency != "" {
						currency = b.Currency
					}
				case !ent.IsNotFound(err):
					return err
				}
				if currency == "" {
					return status.Error(codes.InvalidArgument, "currency: nothing says what the workspace is billed in -- no billing account pays for it, and the product names none -- so say it")
				}
			}
			req.SetWorkspace(levyan.WorkspaceRef_builder{Id: ws[:]}.Build())
			return write(s, db, currency)
		})
	}
	if err != nil {
		return Lot{}, err
	}

	return v.lot(ctx, v.db(), id.Uuid())
}

// lot is lot `id` as it is now.
func (v Invoices) lot(ctx context.Context, db *ent.Client, id uuid.UUID) (Lot, error) {
	c, err := db.Credit.Get(ctx, id)
	if ent.IsNotFound(err) || err == nil && c.Kind != int32(levyan.CreditKind_CREDIT_KIND_GRANTED) {
		return Lot{}, status.Errorf(codes.NotFound, "no credit granted is %s", pdid.Id(id))
	}
	if err != nil {
		return Lot{}, err
	}
	left, err := lefts(ctx, db, id)
	if err != nil {
		return Lot{}, err
	}

	return lotOf(c, left[id]), nil
}

// Expire ends lot `id` now, as the operator `ctx` is framed as, for
// `reason`: what is left of it is taken off by a movement of its own, and
// nothing more of it is spent.
func (v Invoices) Expire(ctx context.Context, id pdid.Id, reason string) (Lot, error) {
	who, err := benefits.Operator(ctx)
	if err != nil {
		return Lot{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Lot{}, status.Error(codes.InvalidArgument, "reason: why it is ended, in your words")
	}
	l, err := v.lot(ctx, v.db(), id.Uuid())
	if err != nil {
		return Lot{}, err
	}
	if !l.Ended.IsZero() {
		return Lot{}, status.Errorf(codes.FailedPrecondition, "it was ended at %s", l.Ended.UTC().Format(time.RFC3339))
	}
	if err := v.expire(ctx, l.Row, who, reason); err != nil {
		return Lot{}, err
	}

	return v.lot(ctx, v.db(), id.Uuid())
}

// errSpent is a lot an operator ends that has nothing left.
var errSpent = status.Error(codes.FailedPrecondition, "nothing is left of it: it was spent, or it expired")

// expire takes what is left of lot `c` off it, in a transaction that holds
// its billing account's row -- for a billing account's -- and its own: as
// an operator `who` ends it for `reason`, or as it expired, for neither --
// and then only once nothing may still spend it ([pending]), which is asked
// on the same transaction, unless it was ended.
func (v Invoices) expire(ctx context.Context, c *ent.Credit, who pdid.Id, reason string) error {
	f := func(s levyan.Server, db *ent.Client, _ dialect.Driver) error {
		if v.postgres() {
			q := db.Credit.Query().Where(entcredit.Id(c.Id))
			q.Modify(func(s *sql.Selector) { s.ForUpdate() })
			if _, err := q.Ids(ctx); err != nil {
				return err
			}
		}
		left, err := lefts(ctx, db, c.Id)
		if err != nil {
			return err
		}
		if who.IsZero() && lotOf(c, left[c.Id]).Ended.IsZero() {
			wait, err := pending(ctx, db, c, true)
			if err != nil {
				return err
			}
			if wait {
				return errSkip
			}
		}
		n := left[c.Id].left
		if n <= 0 {
			if who.IsZero() {
				return errSkip
			}
			return errSpent
		}
		req := levyan.CreditAddRequest_builder{
			Tenant:   levyan.TenantRef_builder{Id: c.TenantId[:]}.Build(),
			Amount:   -n,
			Currency: c.Currency,
			Kind:     levyan.CreditKind_CREDIT_KIND_EXPIRED,
			Lot:      c.Id[:],
		}.Build()
		if c.BillingAccountId != (uuid.UUID{}) {
			req.SetBillingAccount(levyan.BillingAccountRef_builder{Id: c.BillingAccountId[:]}.Build())
		}
		if !who.IsZero() {
			req.SetOperator(levyan.OperatorRef_builder{Id: who.Bytes()}.Build())
			req.SetReason(reason)
		}
		if _, err := s.Credit().Add(ctx, req); err != nil {
			return fmt.Errorf("credit: %w", err)
		}
		return nil
	}

	if c.BillingAccountId != (uuid.UUID{}) {
		return v.held(ctx, c.BillingAccountId, f)
	}
	return v.tx(ctx, f)
}

// Expired takes what is left of every lot that can be spent no longer off
// it, by a movement of its own: what the loop does each pass, so that what a
// lot was granted is accounted for. That is a lot that expired, once nothing
// may still spend it ([pending]) -- a lot that expires on December's last
// day is spent on December's invoice, decided after the period closes, and
// taken off after -- and a lot an operator ended, with something come back
// to it since, at once.
func (v Invoices) Expired(ctx context.Context) error {
	now := v.now()
	ended, err := v.db().Credit.Query().
		Where(entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_EXPIRED))).
		Select(entcredit.FieldLot).
		All(ctx)
	if err != nil {
		return err
	}
	of := []uuid.UUID{}
	for _, c := range ended {
		of = append(of, *c.Lot)
	}
	vs, err := v.db().Credit.Query().
		Where(
			entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_GRANTED)),
			entcredit.Or(entcredit.DateExpiresLTE(now), entcredit.IdIn(of...)),
		).
		Order(ent.Asc(entcredit.FieldDateExpires), ent.Asc(entcredit.FieldId)).
		All(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(vs))
	for _, c := range vs {
		ids = append(ids, c.Id)
	}
	left, err := lefts(ctx, v.db(), ids...)
	if err != nil {
		return err
	}

	var errs []error
	for _, c := range vs {
		if left[c.Id].left <= 0 {
			continue
		}
		if err := v.expire(ctx, c, pdid.Nil, ""); err != nil {
			logErr(ctx, "invoices: what is left of a credit that expired was not taken off it", err, slog.String("credit", pdid.Id(c.Id).String()))
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d credits that expired were not taken off: %w", len(errs), errs[0])
	}

	return nil
}
