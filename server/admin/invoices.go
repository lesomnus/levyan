package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/ledger"
)

// Invoice is what one billing account is asked to pay for a product's
// period, as an operator finds it.
type Invoice struct {
	Id             string     `json:"id"`
	Product        string     `json:"product"`
	BillingAccount string     `json:"billing_account"`
	State          string     `json:"state"`
	Start          time.Time  `json:"start"`
	End            time.Time  `json:"end"`
	Channel        string     `json:"channel"`
	Currency       string     `json:"currency"`
	Subtotal       int64      `json:"subtotal"`
	Tax            int64      `json:"tax"`
	TaxRate        string     `json:"tax_rate,omitempty"`
	Total          int64      `json:"total"`
	Credited       int64      `json:"credited"`
	Due            int64      `json:"due"`
	Paid           int64      `json:"paid"`
	Reduced        int64      `json:"reduced"`
	Remaining      int64      `json:"remaining"`
	Number         int64      `json:"number,omitempty"`
	Carried        uint32     `json:"carried,omitempty"`
	Finalized      *time.Time `json:"finalized,omitempty"`
	Issued         *time.Time `json:"issued,omitempty"`
	Document       string     `json:"document,omitempty"`
	DueDate        *time.Time `json:"due_date,omitempty"`
	Settled        *time.Time `json:"settled,omitempty"`
	ChannelRef     string     `json:"channel_ref,omitempty"`
	ChannelNumber  string     `json:"channel_number,omitempty"`
	Reason         string     `json:"reason,omitempty"`

	// With `invoice show`: its lines, credit notes and attempts, what its
	// billing account is owed in the product now, and what is done of it
	// since its charge failed.
	Lines       []InvoiceLine `json:"lines,omitempty"`
	CreditNotes []CreditNote  `json:"credit_notes,omitempty"`
	Attempts    []Attempt     `json:"attempts,omitempty"`
	Balance     *int64        `json:"balance,omitempty"`
	Dunning     *Dunning      `json:"dunning,omitempty"`
}

// Dunning is what the policy table says now of an invoice still owed whose
// charge failed (#11): the kind of its latest decline, whose row it is held
// to; when its first charge failed, which every other time is measured from;
// when it is charged next, if it is; when its grace ends and what it bills
// is restricted; and when that is read-only, the second stage.
type Dunning struct {
	Kind           string     `json:"kind"`
	Failed         time.Time  `json:"failed"`
	NextRetry      *time.Time `json:"next_retry,omitempty"`
	GraceEnds      time.Time  `json:"grace_ends"`
	RestrictedFrom time.Time  `json:"restricted_from"`
	ReadonlyFrom   time.Time  `json:"readonly_from"`
}

// InvoiceLine is one statement on an invoice.
type InvoiceLine struct {
	Statement string    `json:"statement"`
	Workspace string    `json:"workspace"`
	Label     string    `json:"label,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Total     int64     `json:"total"`
}

// CreditNote takes some of an invoice back.
type CreditNote struct {
	Id       string     `json:"id"`
	Invoice  string     `json:"invoice"`
	Reason   string     `json:"reason"`
	Amount   int64      `json:"amount"`
	Tax      int64      `json:"tax"`
	Total    int64      `json:"total"`
	Reduced  int64      `json:"reduced"`
	Refunded int64      `json:"refunded"`
	Credited int64      `json:"credited"`
	Number   int64      `json:"number,omitempty"`
	Issued   *time.Time `json:"issued,omitempty"`
	Document string     `json:"document,omitempty"`
	Words    string     `json:"words,omitempty"`
	Created  time.Time  `json:"created"`
}

// Attempt is one attempt to move an invoice's money.
type Attempt struct {
	Id         string     `json:"id"`
	Kind       string     `json:"kind"`
	Key        string     `json:"key"`
	Amount     int64      `json:"amount"`
	Currency   string     `json:"currency"`
	State      string     `json:"state"`
	Sends      uint32     `json:"sends"`
	Sent       *time.Time `json:"sent,omitempty"`
	Failure    string     `json:"failure,omitempty"`
	ChannelRef string     `json:"channel_ref,omitempty"`
	Paid       *time.Time `json:"paid,omitempty"`
	Recorded   bool       `json:"recorded,omitempty"`
	Reference  string     `json:"reference,omitempty"`
	Created    time.Time  `json:"created"`

	// Twice is when the channel was found to hold more payments of its key
	// than the ledger accounts for: what reconciliation (#13) gives back.
	Twice *time.Time `json:"twice,omitempty"`
}

func invoiceOf(v *ent.Invoice, product string) Invoice {
	out := Invoice{
		Id: pdid.Id(v.Id).String(), Product: product, BillingAccount: pdid.Id(v.BillingAccountId).String(),
		State: named(levyan.InvoiceState(v.State).String(), "INVOICE_STATE_"),
		Start: v.DateStart.UTC(), End: v.DateEnd.UTC(),
		Channel: named(v.Channel, "CHANNEL_"), Currency: v.Currency,
		Subtotal: v.Subtotal, Tax: v.Tax, TaxRate: v.TaxRate, Total: v.Total,
		Credited: v.Credited, Due: v.Due, Paid: v.Paid, Reduced: v.Reduced, Remaining: invoices.Remaining(v),
		Carried: v.Carried, Finalized: v.DateFinalized, Issued: v.DateIssued, Document: v.Document,
		DueDate: v.DateDue, Settled: v.DateSettled, ChannelRef: v.ChannelRef, ChannelNumber: v.ChannelNumber,
		Reason: v.Reason,
	}
	if v.Number != nil {
		out.Number = *v.Number
	}
	if v.TaxRate == "0" {
		out.TaxRate = ""
	}

	return out
}

func creditNoteOf(v *ent.CreditNote) CreditNote {
	out := CreditNote{
		Id: pdid.Id(v.Id).String(), Invoice: pdid.Id(v.InvoiceId).String(),
		Reason: named(levyan.CreditNoteReason(v.Reason).String(), "CREDIT_NOTE_REASON_"),
		Amount: v.Amount, Tax: v.Tax, Total: v.Total, Reduced: v.Reduced, Refunded: v.Refunded, Credited: v.Credited,
		Issued: v.DateIssued, Document: v.Document, Words: v.Words, Created: v.DateCreated,
	}
	if v.Number != nil {
		out.Number = *v.Number
	}

	return out
}

func attemptOf(v *ent.PaymentAttempt) Attempt {
	out := Attempt{
		Id: pdid.Id(v.Id).String(), Kind: named(levyan.AttemptKind(v.Kind).String(), "ATTEMPT_KIND_"), Key: v.AttemptKey,
		Amount: v.Amount, Currency: v.Currency, State: named(levyan.AttemptState(v.State).String(), "ATTEMPT_STATE_"),
		Sends: v.Sends, Sent: v.DateSent, ChannelRef: v.ChannelRef, Paid: v.DatePaid,
		Recorded: v.Recorded, Reference: v.Reference, Created: v.DateCreated, Twice: v.DateTwice,
	}
	if f := v.Failure; f != nil {
		out.Failure = named(f.GetKind().String(), "FAILURE_KIND_")
		if f.GetKind() == levyan.FailureKind_FAILURE_KIND_DECLINED {
			out.Failure += " " + named(f.GetDecline().String(), "DECLINE_KIND_")
		}
		if f.GetCode() != "" {
			out.Failure += ": " + f.GetCode()
		}
		if f.GetMessage() != "" {
			out.Failure += ": " + f.GetMessage()
		}
	}

	return out
}

// invoiceStates are the states an operator lists invoices by, as a query
// names them.
func invoiceState(v string) (int32, bool) {
	return value(v, "INVOICE_STATE_", levyan.InvoiceState_value)
}

// invoices are a product's invoices, the latest first: of one billing
// account, and in one state, when the query names them.
func (a *Routes) invoices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}

	q := a.S.Ent.Invoice.Query().Where(entinvoice.TenantId(t.Id))
	if v := strings.TrimSpace(r.URL.Query().Get("state")); v != "" {
		s, ok := invoiceState(v)
		if !ok {
			http.Error(w, "state: draft, open, paid, uncollectible, void, written_off or shadow", http.StatusBadRequest)
			return
		}
		q = q.Where(entinvoice.State(s))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("billing_account")); v != "" {
		id, err := pdid.Parse(v)
		if err != nil {
			http.Error(w, "billing_account: a billing account, by its identifier", http.StatusBadRequest)
			return
		}
		q = q.Where(entinvoice.BillingAccountId(id.Uuid()))
	}

	vs, err := q.Order(ent.Desc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).Limit(500).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Invoice{}
	for _, v := range vs {
		out = append(out, invoiceOf(v, t.Alias))
	}
	answer(w, http.StatusOK, out)
}

// invoiceBy is the invoice the path names, or false once it has answered.
func (a *Routes) invoiceBy(w http.ResponseWriter, r *http.Request) (pdid.Id, bool) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "an invoice, by the identifier `invoice ls` lists", http.StatusBadRequest)
		return pdid.Nil, false
	}

	return id, true
}

func (a *Routes) invoice(w http.ResponseWriter, r *http.Request) {
	id, ok := a.invoiceBy(w, r)
	if !ok {
		return
	}
	a.showInvoice(w, r, id, http.StatusOK)
}

func (a *Routes) showInvoice(w http.ResponseWriter, r *http.Request, id pdid.Id, code int) {
	ctx := r.Context()
	v, err := a.S.Ent.Invoice.Query().Where(entinvoice.Id(id.Uuid())).WithTenant().Only(ctx)
	if ent.IsNotFound(err) {
		http.Error(w, "no invoice is "+id.String(), http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	out := invoiceOf(v, v.Edges.Tenant.Alias)

	ls, err := a.S.Ent.InvoiceLine.Query().Where(entinvoiceline.InvoiceId(v.Id)).WithWorkspace().
		Order(ent.Asc(entinvoiceline.FieldDateStart), ent.Asc(entinvoiceline.FieldId)).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, l := range ls {
		line := InvoiceLine{Statement: pdid.Id(l.StatementId).String(), Label: l.Label, Start: l.DateStart.UTC(), End: l.DateEnd.UTC(), Total: l.Total}
		if ws := l.Edges.Workspace; ws != nil {
			line.Workspace = text(ws.Key)
		}
		out.Lines = append(out.Lines, line)
	}

	cns, err := a.S.Ent.CreditNote.Query().Where(entcreditnote.InvoiceId(v.Id)).Order(ent.Asc(entcreditnote.FieldDateCreated)).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, cn := range cns {
		out.CreditNotes = append(out.CreditNotes, creditNoteOf(cn))
	}

	as, err := a.S.Ent.PaymentAttempt.Query().Where(entattempt.InvoiceId(v.Id)).Order(ent.Asc(entattempt.FieldDateCreated)).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, at := range as {
		out.Attempts = append(out.Attempts, attemptOf(at))
	}

	bal, err := invoices.Balance(ctx, a.S.Ent, v.TenantId, v.BillingAccountId)
	if err != nil {
		fail(w, r, err)
		return
	}
	out.Balance = &bal

	ds, err := invoices.Dunning(ctx, a.S.Ent, a.S.Invoices.Dunning, []*ent.Invoice{v}, a.S.Clock())
	if err != nil {
		fail(w, r, err)
		return
	}
	if len(ds) > 0 {
		d := ds[0]
		out.Dunning = &Dunning{Kind: d.Kind.String(), Failed: d.Failed.UTC(), GraceEnds: d.GraceEnds.UTC(), RestrictedFrom: d.Restricted.UTC(), ReadonlyFrom: d.Readonly.UTC()}
		if !d.Retry.IsZero() {
			at := d.Retry.UTC()
			out.Dunning.NextRetry = &at
		}
	}

	answer(w, code, out)
}

// voidInvoice voids an open invoice: an operator's hand, on the trail as
// theirs.
func (a *Routes) voidInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := a.invoiceBy(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}
	if _, err := a.S.Invoices.Void(r.Context(), id, in.Reason); err != nil {
		fail(w, r, err)
		return
	}
	a.showInvoice(w, r, id, http.StatusOK)
}

// creditInvoice writes a credit note against a finalized invoice.
func (a *Routes) creditInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := a.invoiceBy(w, r)
	if !ok {
		return
	}
	var in struct {
		Amount    int64      `json:"amount"`
		Reason    string     `json:"reason"`
		Refund    bool       `json:"refund"`
		Words     string     `json:"words"`
		Reference string     `json:"reference"`
		Date      *time.Time `json:"date"`
	}
	if !read(w, r, &in) {
		return
	}
	reason := levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION
	if v := strings.TrimSpace(in.Reason); v != "" {
		n, ok := value(v, "CREDIT_NOTE_REASON_", levyan.CreditNoteReason_value)
		if !ok || n == int32(levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION) {
			http.Error(w, "reason: correction or refund; a cancellation is the invoice voided", http.StatusBadRequest)
			return
		}
		reason = levyan.CreditNoteReason(n)
	}
	c := invoices.Credit{Reason: reason, Amount: in.Amount, Refund: in.Refund, Words: in.Words, Reference: in.Reference}
	if in.Date != nil {
		c.At = *in.Date
	}
	if _, err := a.S.Invoices.Credit(r.Context(), id, c); err != nil {
		fail(w, r, err)
		return
	}
	a.showInvoice(w, r, id, http.StatusCreated)
}

// paidInvoice records a transfer an operator saw come for an invoice, on a
// channel that moves no money itself.
func (a *Routes) paidInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := a.invoiceBy(w, r)
	if !ok {
		return
	}
	var in struct {
		Amount    int64      `json:"amount"`
		Date      *time.Time `json:"date"`
		Reference string     `json:"reference"`
	}
	if !read(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Reference) == "" {
		http.Error(w, "reference: what the bank says identifies the transfer", http.StatusBadRequest)
		return
	}
	at := time.Time{}
	if in.Date != nil {
		at = *in.Date
	}
	if _, err := a.S.Invoices.Record(r.Context(), id, in.Amount, at, in.Reference); err != nil {
		fail(w, r, err)
		return
	}
	a.showInvoice(w, r, id, http.StatusCreated)
}

// Unpriced is a meter with no price on a channel and currency a statement of
// it may be priced on, while it has one on another.
type Unpriced struct {
	Meter    string   `json:"meter"`
	Channel  string   `json:"channel"`
	Currency string   `json:"currency"`
	Priced   []string `json:"priced"`
}

// unpriced is what of a product's price book is missing, now, on the
// channels and currencies its statements may be priced on: the product's
// own and every billing account's -- what would come to nothing for want of
// a price, before a period closes.
func (a *Routes) unpriced(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	terms, err := billing.TermsOf(t)
	if err != nil {
		fail(w, r, err)
		return
	}
	now := ledger.Moment(a.S.Clock())
	cur, err := billing.Current(ctx, a.S.Ent, t.Id, terms, now)
	if err != nil {
		fail(w, r, err)
		return
	}

	gs, err := billing.Gaps(ctx, a.S.Ent, t, cur.Start, now)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Unpriced{}
	for _, g := range gs {
		u := Unpriced{Meter: g.Meter, Channel: named(g.Channel.String(), "CHANNEL_"), Currency: g.Currency}
		for _, l := range g.Priced {
			u.Priced = append(u.Priced, named(l.Channel.String(), "CHANNEL_")+" "+l.Currency)
		}
		out = append(out, u)
	}
	answer(w, http.StatusOK, out)
}
