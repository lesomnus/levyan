package reconcile_test

import (
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/reconcile"
)

// What an invoice says was paid of it is held to its attempts: written past
// the servers -- what no correction of levyan's would write -- a paid amount
// that is not what its charges collected is found, and so is a paid invoice
// with some of it left to pay; put back, nothing is.
func TestAnInvoiceIsHeldToWhatItsChargesCollected(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, _, _ := w.charged()
	w.reconcile()
	w.nothing()

	x.NoError(w.S.Ent.Invoice.UpdateOneId(inv.Id).SetPaid(4000).Exec(w.ctx))
	w.reconcile()
	got := w.findings()
	x.Len(got, 2)
	f := got["invoice_paid_differs "+reconcile.Subject("Invoice", inv.Id)]
	x.NotNil(f)
	x.Equal(int64(1000), f.Amount, "what its charges collected past what it says was paid")
	x.Equal(int32(levyan.FindingSeverity_FINDING_SEVERITY_ERROR), f.Severity)
	s := got["invoice_state_differs "+reconcile.Subject("Invoice", inv.Id)]
	x.NotNil(s)
	x.Contains(s.Summary, "is paid, and 1,000")

	x.NoError(w.S.Ent.Invoice.UpdateOneId(inv.Id).SetPaid(5000).SetState(int32(levyan.InvoiceState_INVOICE_STATE_OPEN)).Exec(w.ctx))
	w.reconcile()
	got = w.findings()
	x.Len(got, 1)
	x.Contains(got["invoice_state_differs "+reconcile.Subject("Invoice", inv.Id)].Summary, "is open, and nothing of it is left")

	x.NoError(w.S.Ent.Invoice.UpdateOneId(inv.Id).SetState(paid).Exec(w.ctx))
	rep := w.reconcile()
	x.Equal(1, rep.Resolved)
	w.nothing()
}

// An invoice's subtotal is held to its lines, and every statement with a
// billing account to being on one invoice: a line lost -- what no write of
// levyan's does -- leaves the invoice short of its lines and the statement
// on none, which is found once it has been for longer than the loop takes to
// gather it; the line put back, nothing is.
func TestAStatementOnNoInvoiceAndAnInvoiceShortOfItsLinesAreFound(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, _, _ := w.charged()
	ls, err := w.S.Ent.InvoiceLine.Query().Where(entinvoiceline.InvoiceId(inv.Id)).All(w.ctx)
	x.NoError(err)
	x.Len(ls, 1)
	l := ls[0]
	x.NoError(w.S.Ent.InvoiceLine.DeleteOneId(l.Id).Exec(w.ctx))

	rep := w.reconcile()
	x.Equal(reconcile.Statements{WithAccount: 1, OnNone: 1}, rep.Statements)
	got := w.findings()
	x.Len(got, 1, "on none, and not for long enough yet")
	f := got["lines_differ "+reconcile.Subject("Invoice", inv.Id)]
	x.NotNil(f)
	x.Contains(f.Summary, "its subtotal is 5,000")

	w.to(w.srv.Now().Add(reconcile.DefaultStatement + time.Hour))
	w.reconcile()
	got = w.findings()
	x.Len(got, 2)
	s := got["statement_uninvoiced "+reconcile.Subject("Statement", l.StatementId)]
	x.NotNil(s)
	x.Contains(s.Summary, "is on no invoice")
	x.Equal(int64(5000), s.Amount)

	x.NoError(w.S.Ent.InvoiceLine.Create().
		SetId(l.Id).SetTenantId(l.TenantId).SetWorkspaceId(l.WorkspaceId).SetInvoiceId(l.InvoiceId).SetStatementId(l.StatementId).
		SetLabel(l.Label).SetDateStart(l.DateStart).SetDateEnd(l.DateEnd).SetTotal(l.Total).SetDateCreated(l.DateCreated).
		Exec(w.ctx))
	rep = w.reconcile()
	x.Equal(2, rep.Resolved)
	x.Equal(reconcile.Statements{WithAccount: 1, OnOne: 1}, rep.Statements)
	w.nothing()
}

// A billing account's credit balance is what its rows add up to, and is held
// to them: a movement of credit no invoice took -- written by hand, around
// the ledger -- leaves the balance less than nothing and the invoice it
// names paid by credit it does not say, and both are found. An operator who
// has seen to it accepts them, and nothing is open.
func TestACreditBalanceIsHeldToItsRows(t *testing.T) {
	w := setup(t)
	x := w.X

	inv, _, _ := w.charged()
	_, err := w.S.Ungated.Credit().Add(w.ctx, levyan.CreditAddRequest_builder{
		Tenant:         levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
		BillingAccount: levyan.BillingAccountRef_builder{Id: inv.BillingAccountId[:]}.Build(),
		Invoice:        levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
		Amount:         -1000,
		Currency:       "KRW",
		Kind:           levyan.CreditKind_CREDIT_KIND_APPLIED,
	}.Build())
	x.NoError(err)

	w.reconcile()
	got := w.findings()
	x.Len(got, 2)
	b := got["credit_differs Balance/"+w.product.String()+"/"+pdid.Id(inv.BillingAccountId).String()]
	x.NotNil(b)
	x.Contains(b.Summary, "less than nothing")
	x.Equal(int64(-1000), b.Amount)
	p := got["invoice_paid_differs "+reconcile.Subject("Invoice", inv.Id)]
	x.NotNil(p)
	x.Contains(p.Summary, "its credit paid 0")

	for _, f := range got {
		_, err := w.S.Reconciler.Accept(w.op, pdid.Id(f.Id), "a test's row")
		x.NoError(err)
	}
	rep := w.reconcile()
	x.Zero(rep.Open)
	x.Equal(2, rep.Found)

	// What a movement of credit comes of is held to it: a credit note's, and
	// an invoice that came to less than nothing's.
	cn, err := w.S.Invoices.Credit(w.ctx, pdid.Id(inv.Id), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 1000})
	x.NoError(err)
	x.Equal(int64(1000), cn.Credited)
	w.reconcile()
	x.Zero(w.reconcile().Open, "what it credited is what the balance was given")
	for kind, amount := range map[levyan.CreditKind]int64{levyan.CreditKind_CREDIT_KIND_CREDIT_NOTE: 500, levyan.CreditKind_CREDIT_KIND_NEGATIVE: 300} {
		req := levyan.CreditAddRequest_builder{
			Tenant:         levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
			BillingAccount: levyan.BillingAccountRef_builder{Id: inv.BillingAccountId[:]}.Build(),
			Invoice:        levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			Amount:         amount,
			Currency:       "KRW",
			Kind:           kind,
		}.Build()
		if kind == levyan.CreditKind_CREDIT_KIND_CREDIT_NOTE {
			req.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn.Id[:]}.Build())
		}
		_, err := w.S.Ungated.Credit().Add(w.ctx, req)
		x.NoError(err)
	}
	w.reconcile()
	got = w.findings()
	n := got["credit_differs "+reconcile.Subject("CreditNote", cn.Id)]
	x.NotNil(n)
	x.Contains(n.Summary, "credits 1,000 KRW, and the credit balance was given 1,500 KRW of it")
	i := got["credit_differs "+reconcile.Subject("Invoice", inv.Id)]
	x.NotNil(i)
	x.Contains(i.Summary, "came to 5,000 KRW, and the credit it owed the billing account is 300 KRW")
}

// A charge the ledger says succeeded for another amount than the merchant
// charged of its key -- written past the servers -- is found.
func TestAChargeOfAnotherAmountIsFound(t *testing.T) {
	w := setup(t)
	x := w.X

	_, a, _ := w.charged()
	_, err := w.S.Db.ExecContext(w.ctx, "UPDATE "+entattempt.Table+" SET "+entattempt.FieldAmount+" = $1 WHERE "+entattempt.FieldAttemptKey+" = $2", 4000, a.AttemptKey)
	x.NoError(err)
	w.reconcile()
	fs := w.of(levyan.FindingKind_FINDING_KIND_CHARGE_UNMATCHED)
	x.Len(fs, 1)
	x.Contains(fs[0].Summary, "charge "+a.AttemptKey+" of 4,000 KRW succeeded")
	x.Contains(fs[0].Summary, "which charged 5,000 KRW of its key")
}
