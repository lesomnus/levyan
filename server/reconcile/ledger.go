package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/invoices"
)

var (
	requested = int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED)
	unknown   = int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN)
	pending   = int32(levyan.AttemptState_ATTEMPT_STATE_PENDING)
)

// attemptState is an attempt's state as a sentence says it: "unknown".
func attemptState(v int32) string {
	return strings.ToLower(strings.TrimPrefix(levyan.AttemptState(v).String(), "ATTEMPT_STATE_"))
}

// attempts finds every attempt left requested or unknown for longer than
// [Stale.Unknown] since it was last sent -- or written, for one never sent --
// and pending for longer than [Stale.Pending]: what came of it is not known,
// or the channel has not finished with it. One that is out is asked about
// again first, as the loop asks ([invoices.Invoices.Ask]): one the answer
// settles, or takes on, was put right by asking, and one it does not is
// found, with why the asking did not.
func (r *Reconciler) attempts(ctx context.Context, p *pass) ([]Found, error) {
	as, err := p.db.PaymentAttempt.Query().
		Where(entattempt.StateIn(requested, unknown, pending)).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, a := range as {
		since := a.DateCreated
		if a.DateSent != nil {
			since = *a.DateSent
		}
		limit := or(r.Stale.Unknown, DefaultUnknown)
		if a.State == pending {
			limit = or(r.Stale.Pending, DefaultPending)
		}
		if p.now.Sub(since) < limit {
			continue
		}

		f := Found{
			Kind:     levyan.FindingKind_FINDING_KIND_ATTEMPT_STALE,
			Subject:  Subject("PaymentAttempt", a.Id),
			Product:  a.TenantId,
			Rows:     []Row{RowOf("PaymentAttempt", a.Id), RowOf("Invoice", a.InvoiceId)},
			Amount:   a.Amount,
			Currency: a.Currency,
		}
		state := a.State
		asked := ""
		if (a.State == unknown || a.State == pending) && r.Invoices != nil {
			if a.Channel == onMerchant && r.channel() != nil {
				if err := r.pace(ctx, p); err != nil {
					return nil, err
				}
			}
			v, err := r.Invoices.Ask(ctx, pdid.Id(a.Id))
			switch {
			case v == nil:
				return nil, err
			case v.State != state:
				// Asked again, and answered: what it was is no more.
				if v.State == requested {
					continue
				}
				f.Summary = fmt.Sprintf("attempt %s (%s of %s) had been %s since %s, and asking the channel again settled it as %s",
					a.AttemptKey, attemptKind(a.Kind), money.String(a.Amount, a.Currency), attemptState(state), stamp(since), attemptState(v.State))
				f.Facts = fmt.Sprintf("state=%s since=%s", attemptState(state), stamp(since))
				f.Resolved, f.Resolution = true, "put right by asking the channel again"
				out = append(out, f)
				continue
			case err != nil:
				asked = invoices.KindOf(err).String()
			}
		}

		f.Summary = fmt.Sprintf("attempt %s (%s of %s) has been %s since %s", a.AttemptKey, attemptKind(a.Kind), money.String(a.Amount, a.Currency), attemptState(state), stamp(since))
		if asked != "" {
			f.Summary += ", and asking the channel again failed: " + asked
		}
		f.Facts = fmt.Sprintf("state=%s since=%s asked=%s", attemptState(state), stamp(since), asked)
		out = append(out, f)
	}

	return out, nil
}

func attemptKind(v int32) string {
	return strings.ToLower(strings.TrimPrefix(levyan.AttemptKind(v).String(), "ATTEMPT_KIND_"))
}

// ledgerRows is what the checks of invoices and of credit read of the
// ledger, once a pass: every invoice, attempt, movement of credit, line and
// credit note.
type ledgerRows struct {
	invoices    []*ent.Invoice
	attempts    map[uuid.UUID][]*ent.PaymentAttempt // by invoice
	credits     map[uuid.UUID][]*ent.Credit         // by invoice
	lines       map[uuid.UUID][]*ent.InvoiceLine    // by invoice
	creditNotes []*ent.CreditNote
	byNote      map[uuid.UUID][]*ent.PaymentAttempt // refunds, by credit note
	noteCredit  map[uuid.UUID]int64                 // credit, by credit note
	balances    map[[2]uuid.UUID][]*ent.Credit      // by product and billing account
}

func (r *Reconciler) ledgerRows(ctx context.Context, p *pass) (*ledgerRows, error) {
	if p.ledger != nil {
		return p.ledger, nil
	}
	out := &ledgerRows{
		attempts: map[uuid.UUID][]*ent.PaymentAttempt{},
		credits:  map[uuid.UUID][]*ent.Credit{},
		lines:    map[uuid.UUID][]*ent.InvoiceLine{},
		byNote:   map[uuid.UUID][]*ent.PaymentAttempt{},

		noteCredit: map[uuid.UUID]int64{},
		balances:   map[[2]uuid.UUID][]*ent.Credit{},
	}
	var err error
	if out.invoices, err = p.db.Invoice.Query().Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).All(ctx); err != nil {
		return nil, err
	}
	as, err := p.db.PaymentAttempt.Query().
		Select(entattempt.FieldId, entattempt.FieldInvoiceId, entattempt.FieldCreditNoteId, entattempt.FieldKind, entattempt.FieldState,
			entattempt.FieldAmount, entattempt.FieldAttemptKey, entattempt.FieldExtraOf).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		out.attempts[a.InvoiceId] = append(out.attempts[a.InvoiceId], a)
		if a.Kind == refund && a.CreditNoteId != (uuid.UUID{}) {
			out.byNote[a.CreditNoteId] = append(out.byNote[a.CreditNoteId], a)
		}
	}
	cs, err := p.db.Credit.Query().
		Select(entcredit.FieldId, entcredit.FieldTenantId, entcredit.FieldBillingAccountId, entcredit.FieldInvoiceId, entcredit.FieldCreditNoteId,
			entcredit.FieldKind, entcredit.FieldAmount, entcredit.FieldCurrency).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.InvoiceId != (uuid.UUID{}) {
			out.credits[c.InvoiceId] = append(out.credits[c.InvoiceId], c)
		}
		if c.CreditNoteId != (uuid.UUID{}) && c.Kind == int32(levyan.CreditKind_CREDIT_KIND_CREDIT_NOTE) {
			out.noteCredit[c.CreditNoteId] += c.Amount
		}
		k := [2]uuid.UUID{c.TenantId, c.BillingAccountId}
		out.balances[k] = append(out.balances[k], c)
	}
	ls, err := p.db.InvoiceLine.Query().
		Select(entinvoiceline.FieldId, entinvoiceline.FieldInvoiceId, entinvoiceline.FieldStatementId, entinvoiceline.FieldTotal).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		out.lines[l.InvoiceId] = append(out.lines[l.InvoiceId], l)
	}
	if out.creditNotes, err = p.db.CreditNote.Query().Order(ent.Asc(entcreditnote.FieldDateCreated), ent.Asc(entcreditnote.FieldId)).All(ctx); err != nil {
		return nil, err
	}
	p.ledger = out

	return out, nil
}

// invoices holds every invoice to the ledger: what was paid of it to what its
// charges that succeeded collected, less what of that was more than was left
// and is credit; what its credit paid of it to what its credit took; one
// owed -- open, or uncollectible -- to something left to pay, and a paid one
// to nothing; its subtotal to its lines, and each line to its statement. And every credit note to what
// it gives back: what its refunds gave back, once none of them is out.
func (r *Reconciler) invoices(ctx context.Context, p *pass) ([]Found, error) {
	l, err := r.ledgerRows(ctx, p)
	if err != nil {
		return nil, err
	}
	sts, err := r.statementRows(ctx, p)
	if err != nil {
		return nil, err
	}

	out := []Found{}
	for _, inv := range l.invoices {
		rows := []Row{RowOf("Invoice", inv.Id), RowOf("BillingAccount", inv.BillingAccountId)}
		of := func(k levyan.FindingKind, summary, facts string, amount int64) Found {
			return Found{Kind: k, Subject: Subject("Invoice", inv.Id), Product: inv.TenantId, Summary: summary, Facts: facts, Rows: rows, Amount: amount, Currency: inv.Currency}
		}

		charged, overpaid, applied := int64(0), int64(0), int64(0)
		for _, a := range l.attempts[inv.Id] {
			if a.Kind == charge && a.State == succeeded {
				charged += a.Amount
			}
		}
		for _, c := range l.credits[inv.Id] {
			switch levyan.CreditKind(c.Kind) {
			case levyan.CreditKind_CREDIT_KIND_OVERPAID:
				overpaid += c.Amount
			case levyan.CreditKind_CREDIT_KIND_APPLIED:
				applied -= c.Amount
			}
		}
		wrong := []string{}
		if charged-overpaid != inv.Paid {
			wrong = append(wrong, fmt.Sprintf("paid %s, and its charges that succeeded collected %s, of which %s is credit", money.String(inv.Paid, inv.Currency), money.String(charged, inv.Currency), money.String(overpaid, inv.Currency)))
		}
		if applied != inv.Credited {
			wrong = append(wrong, fmt.Sprintf("its credit paid %s, and the credit balance gave %s", money.String(inv.Credited, inv.Currency), money.String(applied, inv.Currency)))
		}
		if len(wrong) > 0 {
			out = append(out, of(levyan.FindingKind_FINDING_KIND_INVOICE_PAID_DIFFERS,
				fmt.Sprintf("invoice %s says it was %s", pdid.Id(inv.Id), strings.Join(wrong, "; ")),
				fmt.Sprintf("paid=%d charged=%d overpaid=%d credited=%d applied=%d", inv.Paid, charged, overpaid, inv.Credited, applied),
				charged-overpaid-inv.Paid))
		}

		left := invoices.Remaining(inv)
		switch st := levyan.InvoiceState(inv.State); st {
		case levyan.InvoiceState_INVOICE_STATE_OPEN, levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE:
			// Owed -- open, or given up on by dunning (#11) -- and a charge
			// that pays what is left settles it.
			if left == 0 {
				name := strings.ToLower(strings.TrimPrefix(st.String(), "INVOICE_STATE_"))
				out = append(out, of(levyan.FindingKind_FINDING_KIND_INVOICE_STATE_DIFFERS,
					fmt.Sprintf("invoice %s is %s, and nothing of it is left to pay: %s due, %s paid, %s taken off", pdid.Id(inv.Id), name, money.String(inv.Due, inv.Currency), money.String(inv.Paid, inv.Currency), money.String(inv.Reduced, inv.Currency)),
					fmt.Sprintf("state=%s due=%d paid=%d reduced=%d", name, inv.Due, inv.Paid, inv.Reduced), 0))
			}
		case levyan.InvoiceState_INVOICE_STATE_PAID:
			if left > 0 {
				out = append(out, of(levyan.FindingKind_FINDING_KIND_INVOICE_STATE_DIFFERS,
					fmt.Sprintf("invoice %s is paid, and %s of it is left to pay", pdid.Id(inv.Id), money.String(left, inv.Currency)),
					fmt.Sprintf("state=paid due=%d paid=%d reduced=%d", inv.Due, inv.Paid, inv.Reduced), left))
			}
		}

		sum := int64(0)
		wrong = []string{}
		for _, ln := range l.lines[inv.Id] {
			sum += ln.Total
			st, ok := sts.byId[ln.StatementId]
			switch {
			case !ok:
				wrong = append(wrong, fmt.Sprintf("line %s is of no statement", pdid.Id(ln.Id)))
			case st.BillingAccountId != inv.BillingAccountId:
				wrong = append(wrong, fmt.Sprintf("line %s is of statement %s, of another billing account", pdid.Id(ln.Id), pdid.Id(st.Id)))
			case st.Total != ln.Total:
				wrong = append(wrong, fmt.Sprintf("line %s is for %s, and its statement %s comes to %s", pdid.Id(ln.Id), money.String(ln.Total, inv.Currency), pdid.Id(st.Id), money.String(st.Total, inv.Currency)))
			}
		}
		if sum != inv.Subtotal {
			wrong = append(wrong, fmt.Sprintf("its subtotal is %s, and its lines come to %s", money.String(inv.Subtotal, inv.Currency), money.String(sum, inv.Currency)))
		}
		if len(wrong) > 0 {
			out = append(out, of(levyan.FindingKind_FINDING_KIND_LINES_DIFFER,
				fmt.Sprintf("invoice %s: %s", pdid.Id(inv.Id), strings.Join(wrong, "; ")),
				strings.Join(wrong, "; "), sum-inv.Subtotal))
		}
	}

	for _, cn := range l.creditNotes {
		if cn.Refunded == 0 && len(l.byNote[cn.Id]) == 0 {
			continue
		}
		back, out_ := int64(0), false
		for _, a := range l.byNote[cn.Id] {
			switch a.State {
			case succeeded:
				back += a.Amount
			case requested, unknown, pending:
				out_ = true
			}
		}
		if out_ || back == cn.Refunded {
			continue
		}
		out = append(out, Found{
			Kind:    levyan.FindingKind_FINDING_KIND_REFUND_NOT_MADE,
			Subject: Subject("CreditNote", cn.Id),
			Product: cn.TenantId,
			Summary: fmt.Sprintf("credit note %s gives back %s through the channel, and its refunds gave back %s",
				pdid.Id(cn.Id), money.String(cn.Refunded, cn.Currency), money.String(back, cn.Currency)),
			Facts:    fmt.Sprintf("refunded=%d back=%d", cn.Refunded, back),
			Rows:     []Row{RowOf("CreditNote", cn.Id), RowOf("Invoice", cn.InvoiceId)},
			Amount:   cn.Refunded - back,
			Currency: cn.Currency,
		})
	}

	return out, nil
}

// credit holds each billing account's credit balance in each product to its
// rows: never less than nothing, in its billing account's currency alone;
// and each movement to what it comes of -- what an invoice that came to less
// than nothing owed, what a credit note credited.
func (r *Reconciler) credit(ctx context.Context, p *pass) ([]Found, error) {
	l, err := r.ledgerRows(ctx, p)
	if err != nil {
		return nil, err
	}
	k := levyan.FindingKind_FINDING_KIND_CREDIT_DIFFERS

	out := []Found{}
	keys := []([2]uuid.UUID){}
	for v := range l.balances {
		keys = append(keys, v)
	}
	slices.SortFunc(keys, func(a, b [2]uuid.UUID) int {
		if c := slices.Compare(a[0][:], b[0][:]); c != 0 {
			return c
		}
		return slices.Compare(a[1][:], b[1][:])
	})
	accounts := map[uuid.UUID]*ent.BillingAccount{}
	for _, v := range keys {
		cs := l.balances[v]
		sum, currencies := int64(0), []string{}
		for _, c := range cs {
			sum += c.Amount
			if !slices.Contains(currencies, c.Currency) {
				currencies = append(currencies, c.Currency)
			}
		}
		ba, ok := accounts[v[1]]
		if !ok {
			if ba, err = p.db.BillingAccount.Get(ctx, v[1]); err != nil {
				return nil, err
			}
			accounts[v[1]] = ba
		}
		slices.Sort(currencies)
		wrong := []string{}
		if sum < 0 {
			wrong = append(wrong, "its rows add up to "+money.String(sum, ba.Currency)+", less than nothing")
		}
		if len(currencies) != 1 || currencies[0] != ba.Currency {
			wrong = append(wrong, fmt.Sprintf("its rows are in %s, and the billing account is billed in %s", strings.Join(currencies, ", "), ba.Currency))
		}
		if len(wrong) == 0 {
			continue
		}
		out = append(out, Found{
			Kind:     k,
			Subject:  "Balance/" + pdid.Id(v[0]).String() + "/" + pdid.Id(v[1]).String(),
			Product:  v[0],
			Summary:  fmt.Sprintf("the credit balance of billing account %s: %s", pdid.Id(v[1]), strings.Join(wrong, "; ")),
			Facts:    fmt.Sprintf("sum=%d currencies=%s", sum, strings.Join(currencies, ",")),
			Rows:     []Row{RowOf("BillingAccount", v[1])},
			Amount:   sum,
			Currency: ba.Currency,
		})
	}

	for _, inv := range l.invoices {
		owed := int64(0)
		for _, c := range l.credits[inv.Id] {
			if c.Kind == int32(levyan.CreditKind_CREDIT_KIND_NEGATIVE) {
				owed += c.Amount
			}
		}
		want := int64(0)
		if inv.DateFinalized != nil && inv.Total < 0 {
			want = -inv.Total
		}
		if owed == want {
			continue
		}
		out = append(out, Found{
			Kind:    k,
			Subject: Subject("Invoice", inv.Id),
			Product: inv.TenantId,
			Summary: fmt.Sprintf("invoice %s came to %s, and the credit it owed the billing account is %s",
				pdid.Id(inv.Id), money.String(inv.Total, inv.Currency), money.String(owed, inv.Currency)),
			Facts:    fmt.Sprintf("total=%d owed=%d", inv.Total, owed),
			Rows:     []Row{RowOf("Invoice", inv.Id), RowOf("BillingAccount", inv.BillingAccountId)},
			Amount:   owed - want,
			Currency: inv.Currency,
		})
	}

	for _, cn := range l.creditNotes {
		if got := l.noteCredit[cn.Id]; got != cn.Credited {
			out = append(out, Found{
				Kind:    k,
				Subject: Subject("CreditNote", cn.Id),
				Product: cn.TenantId,
				Summary: fmt.Sprintf("credit note %s credits %s, and the credit balance was given %s of it",
					pdid.Id(cn.Id), money.String(cn.Credited, cn.Currency), money.String(got, cn.Currency)),
				Facts:    fmt.Sprintf("credited=%d given=%d", cn.Credited, got),
				Rows:     []Row{RowOf("CreditNote", cn.Id), RowOf("Invoice", cn.InvoiceId)},
				Amount:   got - cn.Credited,
				Currency: cn.Currency,
			})
		}
	}

	return out, nil
}

// statementRows is every statement with a billing account, by id, read once
// a pass, and how many lines each is.
type statementRows struct {
	byId  map[uuid.UUID]*ent.Statement
	lines map[uuid.UUID]int
}

func (r *Reconciler) statementRows(ctx context.Context, p *pass) (*statementRows, error) {
	if p.stmts != nil {
		return p.stmts, nil
	}
	sts, err := p.db.Statement.Query().
		Where(entstatement.BillingAccountIdNotNil()).
		Select(entstatement.FieldId, entstatement.FieldTenantId, entstatement.FieldBillingAccountId, entstatement.FieldTotal,
			entstatement.FieldCurrency, entstatement.FieldDateStart, entstatement.FieldDateEnd, entstatement.FieldDateCreated).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ls, err := p.db.InvoiceLine.Query().Select(entinvoiceline.FieldStatementId).All(ctx)
	if err != nil {
		return nil, err
	}
	out := &statementRows{byId: map[uuid.UUID]*ent.Statement{}, lines: map[uuid.UUID]int{}}
	for _, st := range sts {
		out.byId[st.Id] = st
	}
	for _, l := range ls {
		out.lines[l.StatementId]++
	}
	p.stmts = out

	return out, nil
}

// statements holds every statement with a billing account to the invariant
// the invoices keep -- each is on exactly one invoice -- and counts it, even
// though the line's unique index says so: one on none for longer than
// [Stale.Statement] is found, as is one on more than one.
func (r *Reconciler) statements(ctx context.Context, p *pass) ([]Found, error) {
	sts, err := r.statementRows(ctx, p)
	if err != nil {
		return nil, err
	}
	before := p.now.Add(-or(r.Stale.Statement, DefaultStatement))

	ids := []uuid.UUID{}
	for id := range sts.byId {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })

	out := []Found{}
	for _, id := range ids {
		st := sts.byId[id]
		p.statements.WithAccount++
		n := sts.lines[id]
		why := ""
		switch {
		case n == 1:
			p.statements.OnOne++
			continue
		case n == 0:
			p.statements.OnNone++
			if !st.DateCreated.Before(before) {
				continue
			}
			why = "is on no invoice"
		default:
			p.statements.OnMore++
			why = fmt.Sprintf("is on %d invoices", n)
		}
		out = append(out, Found{
			Kind:    levyan.FindingKind_FINDING_KIND_STATEMENT_UNINVOICED,
			Subject: Subject("Statement", st.Id),
			Product: st.TenantId,
			Summary: fmt.Sprintf("statement %s, of %s to %s for billing account %s, %s",
				pdid.Id(st.Id), stamp(st.DateStart), stamp(st.DateEnd), pdid.Id(st.BillingAccountId), why),
			Facts:    fmt.Sprintf("lines=%d", n),
			Rows:     []Row{RowOf("Statement", st.Id), RowOf("BillingAccount", st.BillingAccountId)},
			Amount:   st.Total,
			Currency: st.Currency,
		})
	}

	return out, nil
}
