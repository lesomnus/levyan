package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entfinding "github.com/lesomnus/levyan/internal/ent/finding"
	entnotice "github.com/lesomnus/levyan/internal/ent/notice"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/ledger"
)

// maxPages is the most pages of the merchant's transactions a pass reads of
// one list: 30,000 transactions in the window, or of a subscription. One
// that has more is not read on, and the check is not made: what it would
// have found is not known.
const maxPages = 1000

var (
	charge = int32(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
	refund = int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND)

	succeeded = int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED)
	failed    = int32(levyan.AttemptState_ATTEMPT_STATE_FAILED)

	onMerchant = levyan.Channel_CHANNEL_MOR.String()
)

// unreachable says whether `err`, of asking the merchant, says nothing of
// what was asked about: the merchant could not be asked, or refused levyan,
// or asked to wait -- which fails the check rather than say a row drifted.
func unreachable(err error) bool {
	switch mor.KindOf(err) {
	case mor.NotFound, mor.Invalid:
		return false
	}
	return true
}

// mirrors asks the merchant again of every live subscription whose mirror has
// not been checked with it for longer than [Stale.Mirror], and writes down
// what it answers ([subscriptions.Subscriptions.Observe]: an older answer
// changes nothing). A mirror the answer is newer than was behind the
// merchant -- a notice that never came -- and is found, and put right by the
// asking. One the merchant has no subscription of, or refuses, stays found.
func (r *Reconciler) mirrors(ctx context.Context, p *pass) ([]Found, error) {
	ch := r.channel()
	before := p.now.Add(-or(r.Stale.Mirror, DefaultMirror))
	ms, err := p.db.Subscription.Query().
		Where(
			entsubscription.Mor(ch.Name()),
			entsubscription.StatusNEQ(int32(levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED)),
			entsubscription.DateCheckedLT(before),
		).
		Order(ent.Asc(entsubscription.FieldDateChecked), ent.Asc(entsubscription.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, m := range ms {
		if err := r.pace(ctx, p); err != nil {
			return nil, err
		}
		f := Found{
			Kind:    levyan.FindingKind_FINDING_KIND_MIRROR_STALE,
			Subject: Subject("Subscription", m.Id),
			Rows: []Row{
				RowOf("Subscription", m.Id),
				RowOf("BillingAccount", m.BillingAccountId),
				{Entity: m.Mor + ".subscription", Ref: m.MorSubscription},
			},
		}
		got, err := ch.Subscription(ctx, m.MorSubscription)
		if err != nil {
			if unreachable(err) {
				return nil, fmt.Errorf("subscription %s: %w", m.MorSubscription, err)
			}
			f.Summary = fmt.Sprintf("the mirror of subscription %s has not been checked with the merchant of record since %s, which refuses it when asked: %s",
				m.MorSubscription, stamp(m.DateChecked), mor.KindOf(err))
			f.Facts = fmt.Sprintf("checked=%s refused=%s", stamp(m.DateChecked), mor.KindOf(err))
			out = append(out, f)
			continue
		}
		behind := ledger.Moment(got.Updated).After(ledger.Moment(m.DateMorUpdated))
		if _, err := r.Subscriptions.Observe(ctx, m.BillingAccountId, got, nil); err != nil {
			return nil, fmt.Errorf("subscription %s: %w", m.MorSubscription, err)
		}
		if behind {
			f.Summary = fmt.Sprintf("the mirror of subscription %s was behind the merchant of record, which changed it at %s while the mirror held it as of %s: no notice of it came",
				m.MorSubscription, stamp(got.Updated), stamp(m.DateMorUpdated))
			f.Facts = fmt.Sprintf("held=%s merchant=%s", stamp(m.DateMorUpdated), stamp(got.Updated))
			f.Resolved, f.Resolution = true, Reasked
			out = append(out, f)
		}
	}

	return out, nil
}

// notices finds every notice of the merchant's that has waited to be taken
// for longer than [Stale.Notice]: the inbox asks of it again, later each
// time, and has not been answered.
func (r *Reconciler) notices(ctx context.Context, p *pass) ([]Found, error) {
	ch := r.channel()
	before := p.now.Add(-or(r.Stale.Notice, DefaultNotice))
	ns, err := p.db.Notice.Query().
		Where(entnotice.Mor(ch.Name()), entnotice.DateDoneIsNil(), entnotice.DateCreatedLT(before)).
		Order(ent.Asc(entnotice.FieldDateCreated), ent.Asc(entnotice.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, n := range ns {
		rows := []Row{RowOf("Notice", n.Id), {Entity: n.Mor + ".event", Ref: n.MorEvent}}
		for _, v := range [][2]string{{"transaction", n.MorTransaction}, {"subscription", n.MorSubscription}, {"adjustment", n.MorAdjustment}} {
			if v[1] != "" {
				rows = append(rows, Row{Entity: n.Mor + "." + v[0], Ref: v[1]})
			}
		}
		out = append(out, Found{
			Kind:    levyan.FindingKind_FINDING_KIND_NOTICE_STUCK,
			Subject: Subject("Notice", n.Id),
			Summary: fmt.Sprintf("the merchant of record's notice %s (%s) has waited to be taken since %s, and the merchant could not be asked of what it names %d times",
				n.MorEvent, n.Type, stamp(n.DateCreated), n.Tries),
			Facts: fmt.Sprintf("event=%s tries=%d", n.MorEvent, n.Tries),
			Rows:  rows,
		})
	}

	return out, nil
}

// held is what the merchant check knows as it goes: the merchant's
// transactions, by id, and the ledger's attempts, by key.
type held struct {
	r *Reconciler
	p *pass

	txns     map[string]mor.Transaction
	missing  map[string]bool
	attempts map[string]*ent.PaymentAttempt

	// extras is every payment of a key the ledger does not account for, by
	// the transaction it is.
	extras map[string]bool
}

// transaction is the merchant's transaction `id`, read once a pass: false
// for one the merchant has not.
func (h *held) transaction(ctx context.Context, id string) (mor.Transaction, bool, error) {
	if t, ok := h.txns[id]; ok {
		return t, true, nil
	}
	if h.missing[id] {
		return mor.Transaction{}, false, nil
	}
	if err := h.r.pace(ctx, h.p); err != nil {
		return mor.Transaction{}, false, err
	}
	t, err := h.r.channel().Transaction(ctx, id)
	switch {
	case err == nil:
		h.txns[id] = t
		return t, true, nil
	case !unreachable(err):
		h.missing[id] = true
		return mor.Transaction{}, false, nil
	}

	return mor.Transaction{}, false, fmt.Errorf("transaction %s: %w", id, err)
}

// list is every transaction of the merchant's `l` reads, page by page.
func (h *held) list(ctx context.Context, l mor.List) ([]mor.Transaction, error) {
	out := []mor.Transaction{}
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, errors.New("the merchant has more transactions than a pass reads")
		}
		if err := h.r.pace(ctx, h.p); err != nil {
			return nil, err
		}
		got, err := h.r.channel().Transactions(ctx, l)
		if err != nil {
			return nil, err
		}
		for _, t := range got.Transactions {
			h.txns[t.ID] = t
		}
		out = append(out, got.Transactions...)
		if !got.More || got.Last == "" {
			return out, nil
		}
		l.After = got.Last
	}
}

// attempt is the ledger's attempt keyed `key`, or nil for none.
func (h *held) attempt(ctx context.Context, key string) (*ent.PaymentAttempt, error) {
	if a, ok := h.attempts[key]; ok {
		return a, nil
	}
	a, err := h.p.db.PaymentAttempt.Query().Where(entattempt.AttemptKey(key)).Only(ctx)
	if ent.IsNotFound(err) {
		h.attempts[key] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.attempts[key] = a

	return a, nil
}

// merchant holds the payment ledger to the merchant of record: every charge
// it says succeeded to a paid transaction of its key and amount, every paid
// transaction that carries a key of levyan's to an attempt that accounts for
// it, every key marked charged twice to its payments given back, every
// refund it says succeeded to an approved refund of its key and amount, and
// what the merchant gave or took back of levyan's charges to a refund of the
// ledger's. What is read of the merchant is what changed in the window
// ([Reconciler.Window]), and what each finding open names, wherever it is.
func (r *Reconciler) merchant(ctx context.Context, p *pass) ([]Found, error) {
	h := &held{r: r, p: p, txns: map[string]mor.Transaction{}, missing: map[string]bool{}, attempts: map[string]*ent.PaymentAttempt{}, extras: map[string]bool{}}
	since := p.now.Add(-r.window())

	listed, err := h.list(ctx, mor.List{Since: since})
	if err != nil {
		return nil, fmt.Errorf("the merchant's transactions since %s: %w", stamp(since), err)
	}

	out := []Found{}
	for _, step := range []func(*Reconciler, context.Context, *held, time.Time, []mor.Transaction) ([]Found, error){
		(*Reconciler).charges,
		(*Reconciler).keys,
		(*Reconciler).twice,
		(*Reconciler).refunds,
		(*Reconciler).adjustments,
	} {
		vs, err := step(r, ctx, h, since, listed)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}

	return out, nil
}

// subjectIds is the attempts the findings of `k` that are open or accepted
// are of.
func subjectIds(p *pass, k levyan.FindingKind, entity string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, s := range p.subjects(k) {
		v, ok := strings.CutPrefix(s, entity+"/")
		if !ok {
			continue
		}
		if id, err := pdid.Parse(v); err == nil {
			out = append(out, id.Uuid())
		}
	}

	return out
}

// charges holds every charge on the merchant's channel the ledger says
// succeeded in the window -- or that a finding open is of -- to a paid
// transaction of the merchant's that carries its key, for its amount.
func (r *Reconciler) charges(ctx context.Context, h *held, since time.Time, _ []mor.Transaction) ([]Found, error) {
	k := levyan.FindingKind_FINDING_KIND_CHARGE_UNMATCHED
	as, err := h.p.db.PaymentAttempt.Query().
		Where(
			entattempt.Channel(onMerchant),
			entattempt.Kind(charge),
			entattempt.State(succeeded),
			entattempt.Recorded(false),
			within(since, subjectIds(h.p, k, "PaymentAttempt")),
		).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, a := range as {
		h.attempts[a.AttemptKey] = a
		f := Found{
			Kind:     k,
			Subject:  Subject("PaymentAttempt", a.Id),
			Product:  a.TenantId,
			Rows:     []Row{RowOf("PaymentAttempt", a.Id), RowOf("Invoice", a.InvoiceId)},
			Amount:   a.Amount,
			Currency: a.Currency,
		}
		why := ""
		if a.ChannelRef == "" {
			why = "names no transaction of the merchant's"
		} else {
			f.Rows = append(f.Rows, Row{Entity: r.channel().Name() + ".transaction", Ref: a.ChannelRef})
			t, ok, err := h.transaction(ctx, a.ChannelRef)
			if err != nil {
				return nil, err
			}
			line, has := t.Line(a.AttemptKey)
			switch {
			case !ok:
				why = "names transaction " + a.ChannelRef + ", which the merchant has not"
			case t.Status != mor.Paid:
				why = fmt.Sprintf("names transaction %s, which the merchant holds as %s", t.ID, t.Status)
			case !has:
				why = fmt.Sprintf("names transaction %s, which holds no charge of its key", t.ID)
			case line.Amount != (money.Amount{Minor: a.Amount, Currency: a.Currency}):
				why = fmt.Sprintf("names transaction %s, which charged %s of its key", t.ID, line.Amount)
			}
		}
		if why == "" {
			continue
		}
		f.Summary = fmt.Sprintf("charge %s of %s succeeded, as the ledger says, and %s", a.AttemptKey, money.String(a.Amount, a.Currency), why)
		f.Facts = fmt.Sprintf("ref=%s %s", a.ChannelRef, why)
		out = append(out, f)
	}

	return out, nil
}

// keys holds every paid transaction of the window that carries a key of
// levyan's to the attempt that accounts for it: a key no attempt has is money
// taken that the ledger has no record of, and one whose attempt settled by
// another payment, or failed, is a key charged twice (below).
func (r *Reconciler) keys(ctx context.Context, h *held, _ time.Time, listed []mor.Transaction) ([]Found, error) {
	k := levyan.FindingKind_FINDING_KIND_KEY_UNKNOWN
	ts := slices.Clone(listed)
	for _, s := range h.p.subjects(k) {
		// One found before, read again wherever it is now.
		id, ok := strings.CutPrefix(s, r.channel().Name()+".transaction/")
		if !ok || slices.ContainsFunc(ts, func(t mor.Transaction) bool { return t.ID == id }) {
			continue
		}
		t, ok, err := h.transaction(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			ts = append(ts, t)
		}
	}

	out := []Found{}
	for _, t := range ts {
		if t.Status != mor.Paid {
			continue
		}
		unknown := []mor.Line{}
		for _, l := range t.Lines {
			if l.Key == "" {
				continue
			}
			a, err := h.attempt(ctx, l.Key)
			if err != nil {
				return nil, err
			}
			switch {
			case a == nil:
				unknown = append(unknown, l)
			case a.Kind != charge:
			case a.State == failed || (a.State == succeeded && a.ChannelRef != t.ID):
				// The ledger accounts for no payment of the key, or for
				// another: a key charged twice.
				h.extras[t.ID] = true
			}
		}
		if len(unknown) == 0 {
			continue
		}
		given := back(t)
		keys, amount, currency := []string{}, int64(0), ""
		for _, l := range unknown {
			keys = append(keys, l.Key)
			amount, currency = amount+l.Amount.Minor, l.Amount.Currency
		}
		if given.Minor >= amount {
			// Given back in full at the merchant: nothing of it is held.
			continue
		}
		slices.Sort(keys)
		out = append(out, Found{
			Kind:     k,
			Subject:  r.channel().Name() + ".transaction/" + t.ID,
			Summary:  fmt.Sprintf("the merchant of record holds transaction %s as paid, with %s of key %s, which no attempt of this deployment's has", t.ID, money.String(amount, currency), strings.Join(keys, ", ")),
			Facts:    fmt.Sprintf("keys=%s amount=%d given=%d", strings.Join(keys, ","), amount, given.Minor),
			Rows:     []Row{{Entity: r.channel().Name() + ".transaction", Ref: t.ID}, {Entity: r.channel().Name() + ".subscription", Ref: t.Subscription}},
			Amount:   amount - given.Minor,
			Currency: currency,
		})
	}

	return out, nil
}

// back is what the merchant gave back of transaction `t`, approved, in the
// terms of a charge: every refund of it, levyan's and any other's.
func back(t mor.Transaction) money.Amount {
	out := money.Amount{Currency: t.Total.Currency}
	for _, a := range t.Adjustments {
		if a.Kind == mor.RefundAdjustment && (a.Status == mor.Approved || a.Status == mor.Reversed) {
			out.Minor += a.Asked.Minor
			out.Currency = a.Asked.Currency
		}
	}

	return out
}

// twice holds every key the ledger has marked charged twice
// (`PaymentAttempt.date_twice`) -- and every one found so in the window, and
// every one a finding open is of -- to what the merchant holds of it: each
// payment of the key but the one the attempt settled by is to give back, and
// the finding stands until each is given back at the merchant, or is
// accepted. A key marked that a finding was resolved of is looked at again
// only when the window finds another payment of it.
func (r *Reconciler) twice(ctx context.Context, h *held, _ time.Time, _ []mor.Transaction) ([]Found, error) {
	k := levyan.FindingKind_FINDING_KIND_TWICE
	marked, err := h.p.db.PaymentAttempt.Query().
		Where(entattempt.Channel(onMerchant), entattempt.Kind(charge), entattempt.DateTwiceNotNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[uuid.UUID]bool{}
	for _, a := range marked {
		if _, ok := h.p.open[k][Subject("PaymentAttempt", a.Id)]; ok {
			ids[a.Id] = true
			continue
		}
		was, err := h.p.db.Finding.Query().
			Where(findingOf(k, Subject("PaymentAttempt", a.Id))...).
			Exist(ctx)
		if err != nil {
			return nil, err
		}
		if !was {
			ids[a.Id] = true
		}
	}
	for _, id := range subjectIds(h.p, k, "PaymentAttempt") {
		ids[id] = true
	}
	for id, t := range h.txns {
		if !h.extras[id] {
			continue
		}
		for _, l := range t.Lines {
			if a, _ := h.attempt(ctx, l.Key); a != nil && a.Kind == charge {
				ids[a.Id] = true
			}
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}
	as, err := h.p.db.PaymentAttempt.Query().
		Where(entattempt.IdIn(slices.Collect(maps.Keys(ids))...)).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, a := range as {
		extra, err := h.extra(ctx, a)
		if err != nil {
			return nil, err
		}
		left := []string{}
		rows := []Row{RowOf("PaymentAttempt", a.Id), RowOf("Invoice", a.InvoiceId)}
		owed := int64(0)
		for _, e := range extra {
			h.extras[e.txn.ID] = true
			rows = append(rows, Row{Entity: r.channel().Name() + ".transaction", Ref: e.txn.ID})
			if e.given.Minor >= e.line.Amount.Minor {
				continue
			}
			owed += e.line.Amount.Minor - e.given.Minor
			left = append(left, fmt.Sprintf("%s (%s)", e.txn.ID, money.String(e.line.Amount.Minor-e.given.Minor, a.Currency)))
		}
		if len(left) == 0 {
			continue
		}

		asked, err := h.p.db.PaymentAttempt.Query().
			Where(entattempt.ExtraOf(a.AttemptKey), entattempt.StateNEQ(failed)).
			Order(ent.Asc(entattempt.FieldDateCreated)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		being := []string{}
		for _, v := range asked {
			rows = append(rows, RowOf("PaymentAttempt", v.Id))
			if v.State != succeeded {
				being = append(being, v.AttemptKey)
			}
		}
		summary := fmt.Sprintf("key %s was charged %s more than the ledger accounts for, by the merchant of record: %s", a.AttemptKey, money.String(owed, a.Currency), strings.Join(left, ", "))
		if len(being) > 0 {
			summary += "; being given back by " + strings.Join(being, ", ")
		}
		out = append(out, Found{
			Kind:     k,
			Subject:  Subject("PaymentAttempt", a.Id),
			Product:  a.TenantId,
			Summary:  summary,
			Facts:    fmt.Sprintf("left=%s being=%s", strings.Join(left, ","), strings.Join(being, ",")),
			Rows:     rows,
			Amount:   owed,
			Currency: a.Currency,
		})
	}

	return out, nil
}

// extraPayment is a payment of a key the ledger does not account for: the
// transaction it is, the key's line of it, and what of it the merchant has
// given back.
type extraPayment struct {
	txn   mor.Transaction
	line  mor.Line
	given money.Amount
}

// extra is every payment of attempt `a`'s key that the ledger does not
// account for, as the merchant has it: of every subscription its billing
// account has had, each paid transaction with a line of the key but the one
// the attempt settled by -- every one, of an attempt that did not succeed.
func (h *held) extra(ctx context.Context, a *ent.PaymentAttempt) ([]extraPayment, error) {
	inv, err := h.p.db.Invoice.Get(ctx, a.InvoiceId)
	if err != nil {
		return nil, err
	}
	ms, err := h.p.db.Subscription.Query().
		Where(entsubscription.BillingAccountId(inv.BillingAccountId), entsubscription.Mor(h.r.channel().Name())).
		Order(ent.Asc(entsubscription.FieldDateCreated)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// What the attempt settles by: the payment it settled by, when it
	// succeeded; none, when it failed; and the first that was paid, as an
	// ask of it settles by, when it is not settled yet.
	by, first := "", a.State != succeeded && a.State != failed
	if a.State == succeeded {
		by = a.ChannelRef
	}
	out := []extraPayment{}
	for _, m := range ms {
		ts, err := h.list(ctx, mor.List{Subscription: m.MorSubscription})
		if err != nil {
			return nil, fmt.Errorf("the transactions of subscription %s: %w", m.MorSubscription, err)
		}
		for _, t := range ts {
			l, ok := t.Line(a.AttemptKey)
			if !ok || t.Status != mor.Paid || t.ID == by {
				continue
			}
			if first {
				by, first = t.ID, false
				continue
			}
			given := money.Amount{Currency: l.Amount.Currency}
			for _, adj := range t.Adjustments {
				if adj.Kind == mor.RefundAdjustment && (adj.Status == mor.Approved || adj.Status == mor.Reversed) {
					given.Minor += adj.Asked.Minor
				}
			}
			out = append(out, extraPayment{txn: t, line: l, given: given})
		}
	}

	return out, nil
}

// refunds holds every refund on the merchant's channel the ledger says
// succeeded in the window -- or that a finding open is of -- to an approved
// refund of the merchant's of the payment it gave back from, under its key,
// for its amount.
func (r *Reconciler) refunds(ctx context.Context, h *held, since time.Time, _ []mor.Transaction) ([]Found, error) {
	k := levyan.FindingKind_FINDING_KIND_REFUND_UNMATCHED
	as, err := h.p.db.PaymentAttempt.Query().
		Where(
			entattempt.Channel(onMerchant),
			entattempt.Kind(refund),
			entattempt.State(succeeded),
			entattempt.Recorded(false),
			within(since, subjectIds(h.p, k, "PaymentAttempt")),
		).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, a := range as {
		h.attempts[a.AttemptKey] = a
		from := a.Refunds
		if from == "" {
			paid, err := h.p.db.PaymentAttempt.Query().
				Where(entattempt.InvoiceId(a.InvoiceId), entattempt.Kind(charge), entattempt.State(succeeded)).
				Order(ent.Asc(entattempt.FieldDateCreated)).
				First(ctx)
			switch {
			case err == nil:
				from = paid.ChannelRef
			case !ent.IsNotFound(err):
				return nil, err
			}
		}
		f := Found{
			Kind:     k,
			Subject:  Subject("PaymentAttempt", a.Id),
			Product:  a.TenantId,
			Rows:     []Row{RowOf("PaymentAttempt", a.Id), RowOf("Invoice", a.InvoiceId)},
			Amount:   a.Amount,
			Currency: a.Currency,
		}
		if a.CreditNoteId != (uuid.UUID{}) {
			f.Rows = append(f.Rows, RowOf("CreditNote", a.CreditNoteId))
		}
		why := ""
		if from == "" {
			why = "gives back of no payment the merchant holds"
		} else {
			f.Rows = append(f.Rows, Row{Entity: r.channel().Name() + ".transaction", Ref: from})
			t, ok, err := h.transaction(ctx, from)
			if err != nil {
				return nil, err
			}
			adj, has := t.Adjustment(a.AttemptKey)
			switch {
			case !ok:
				why = "gives back of transaction " + from + ", which the merchant has not"
			case !has:
				why = fmt.Sprintf("gives back of transaction %s, which has no refund of its key", t.ID)
			case adj.Status != mor.Approved && adj.Status != mor.Reversed:
				why = fmt.Sprintf("is refund %s of transaction %s, which the merchant holds as %s", adj.ID, t.ID, adj.Status)
			case adj.Asked != (money.Amount{Minor: a.Amount, Currency: a.Currency}):
				why = fmt.Sprintf("is refund %s of transaction %s, which gave back %s", adj.ID, t.ID, adj.Asked)
			}
		}
		if why == "" {
			continue
		}
		f.Summary = fmt.Sprintf("refund %s of %s succeeded, as the ledger says, and %s", a.AttemptKey, money.String(a.Amount, a.Currency), why)
		f.Facts = fmt.Sprintf("from=%s %s", from, why)
		out = append(out, f)
	}

	return out, nil
}

// adjustments holds what the merchant gave back, or took back, of a charge of
// levyan's to the ledger's refunds of it: a refund of a key no refund of the
// ledger's has, one made in the merchant's own pages, a chargeback. What is
// given back of a payment the ledger does not account for -- a key charged
// twice -- is what is owed, and nothing found. What is read is every
// transaction this pass read, and every one a finding open names.
func (r *Reconciler) adjustments(ctx context.Context, h *held, _ time.Time, _ []mor.Transaction) ([]Found, error) {
	k := levyan.FindingKind_FINDING_KIND_ADJUSTMENT_UNKNOWN
	for _, s := range h.p.subjects(k) {
		f := h.p.open[k][s]
		for _, row := range f.Rows.GetRows() {
			if row.GetEntity() == r.channel().Name()+".transaction" {
				if _, _, err := h.transaction(ctx, row.GetRef()); err != nil {
					return nil, err
				}
			}
		}
	}

	ids := []string{}
	for id := range h.txns {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	out := []Found{}
	for _, id := range ids {
		t := h.txns[id]
		if h.extras[id] {
			continue
		}
		// A charge of levyan's: a line of a key an attempt has.
		var of *ent.PaymentAttempt
		for _, l := range t.Lines {
			if l.Key == "" {
				continue
			}
			a, err := h.attempt(ctx, l.Key)
			if err != nil {
				return nil, err
			}
			if a != nil && a.Kind == charge {
				of = a
				break
			}
		}
		if of == nil {
			continue
		}
		for _, adj := range t.Adjustments {
			why := ""
			switch adj.Kind {
			case mor.RefundAdjustment:
				if adj.Status == mor.Rejected {
					continue
				}
				if adj.Key == "" {
					why = "a refund made in the merchant's own pages"
					break
				}
				a, err := h.attempt(ctx, adj.Key)
				if err != nil {
					return nil, err
				}
				if a != nil && a.Kind == refund {
					continue
				}
				why = "a refund of key " + adj.Key + ", which no refund of this deployment's has"
			case mor.Chargeback:
				why = "a chargeback"
			default:
				continue
			}
			out = append(out, Found{
				Kind:    k,
				Subject: r.channel().Name() + ".adjustment/" + adj.ID,
				Product: of.TenantId,
				Summary: fmt.Sprintf("the merchant of record holds %s (%s, %s) of charge %s in transaction %s, which the ledger has no refund of",
					why, adj.ID, adj.Status, of.AttemptKey, t.ID),
				Facts: fmt.Sprintf("status=%s amount=%d", adj.Status, adj.Asked.Minor),
				Rows: []Row{
					{Entity: r.channel().Name() + ".adjustment", Ref: adj.ID},
					{Entity: r.channel().Name() + ".transaction", Ref: t.ID},
					RowOf("PaymentAttempt", of.Id),
					RowOf("Invoice", of.InvoiceId),
				},
				Amount:   adj.Asked.Minor,
				Currency: adj.Asked.Currency,
			})
		}
	}

	return out, nil
}

// stamp is a moment as a finding says one: in UTC, to the second.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// findingOf is the finding of `k` of `subject`, as a query asks for it.
func findingOf(k levyan.FindingKind, subject string) []predicate.Finding {
	return []predicate.Finding{entfinding.Kind(k.String()), entfinding.Subject(subject)}
}

// within is attempts resolved since `since`, or one of `ids`.
func within(since time.Time, ids []uuid.UUID) predicate.PaymentAttempt {
	if len(ids) == 0 {
		return entattempt.DateResolvedGTE(since)
	}
	return entattempt.Or(entattempt.DateResolvedGTE(since), entattempt.IdIn(ids...))
}
