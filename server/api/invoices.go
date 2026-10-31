package api

import (
	"context"
	"uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/pd"
)

// What an account reads of what its billing accounts are asked to pay (#9):
// of the product that relays the call, its billing accounts' invoices once
// each is finalized -- a draft is levyan's still, a draft written off was
// never asked for, and a shadow is the shadow billing's, never collected --
// their lines, and their credit notes.

// finalized is the states of an invoice its payer reads: those of one
// finalized. A state named nowhere here is shown to nobody.
var finalized = []int32{
	int32(levyan.InvoiceState_INVOICE_STATE_OPEN),
	int32(levyan.InvoiceState_INVOICE_STATE_PAID),
	int32(levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE),
	int32(levyan.InvoiceState_INVOICE_STATE_VOID),
}

func theirInvoices(product pdid.Id) func(context.Context, pdid.Id) (predicate.Invoice, error) {
	return func(_ context.Context, account pdid.Id) (predicate.Invoice, error) {
		return entinvoice.And(
			entinvoice.TenantId(product.Uuid()),
			entinvoice.HasBillingAccountWith(entbillingaccount.Owner(account.Uuid())),
			entinvoice.StateIn(finalized...),
		), nil
	}
}

func theirLines(product pdid.Id) func(context.Context, pdid.Id) (predicate.InvoiceLine, error) {
	return func(ctx context.Context, account pdid.Id) (predicate.InvoiceLine, error) {
		inv, err := theirInvoices(product)(ctx, account)
		if err != nil {
			return nil, err
		}
		return entinvoiceline.And(entinvoiceline.TenantId(product.Uuid()), entinvoiceline.HasInvoiceWith(inv)), nil
	}
}

func theirCreditNotes(product pdid.Id) func(context.Context, pdid.Id) (predicate.CreditNote, error) {
	return func(_ context.Context, account pdid.Id) (predicate.CreditNote, error) {
		return entcreditnote.And(
			entcreditnote.TenantId(product.Uuid()),
			entcreditnote.HasBillingAccountWith(entbillingaccount.Owner(account.Uuid())),
		), nil
	}
}

func (h Payers) ListInvoices(ctx context.Context, req *levyanv1.ListInvoicesRequest) (*levyanv1.ListInvoicesResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_ListInvoices_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListInvoices_FullMethodName, pd.Reads{Invoice: theirInvoices(product)}); err != nil {
		return nil, err
	}

	size := req.GetPageSize()
	switch {
	case size <= 0:
		size = 20
	case size > 100:
		size = 100
	}
	list := levyan.InvoiceListRequest_builder{Size: size, After: req.GetPageToken(), Reverse: true}.Build()
	if v := req.GetBillingAccount(); v != "" {
		id, err := pdid.Parse(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", v)
		}
		list.SetFilters([]*levyan.InvoiceFilter{levyan.InvoiceFilter_builder{
			BillingAccount: levyan.BillingAccountRef_builder{Id: id.Bytes()}.Build(),
		}.Build()})
	}

	vs, err := h.S.Invoice().List(ctx, list)
	if err != nil {
		return nil, err
	}
	out := &levyanv1.ListInvoicesResponse{}
	for _, v := range vs.GetItems() {
		out.SetInvoices(append(out.GetInvoices(), invoiceOf(v)))
	}
	out.SetNextPageToken(vs.GetNext())

	return out, nil
}

func (h Payers) GetInvoice(ctx context.Context, req *levyanv1.GetInvoiceRequest) (*levyanv1.GetInvoiceResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_GetInvoice_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_GetInvoice_FullMethodName, pd.Reads{
		Invoice:     theirInvoices(product),
		InvoiceLine: theirLines(product),
		CreditNote:  theirCreditNotes(product),
	})
	if err != nil {
		return nil, err
	}
	id, err := pdid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %q is not an invoice's identifier", req.GetId())
	}

	v, err := h.S.Invoice().Get(ctx, levyan.InvoiceGetRequest_builder{Ref: levyan.InvoiceRef_builder{Id: id.Bytes()}.Build()}.Build())
	if err != nil {
		return nil, err
	}
	out := invoiceOf(v)

	// Its lines, read through the wall; the workspaces they are of, and the
	// zone of the periods, by what the wall answered.
	var lines []*levyan.InvoiceLine
	after := ""
	for {
		vs, err := h.S.InvoiceLine().List(ctx, levyan.InvoiceLineListRequest_builder{
			Filters: []*levyan.InvoiceLineFilter{levyan.InvoiceLineFilter_builder{Invoice: levyan.InvoiceRef_builder{Id: id.Bytes()}.Build()}.Build()},
			Size:    500,
			After:   after,
		}.Build())
		if err != nil {
			return nil, err
		}
		lines = append(lines, vs.GetItems()...)
		if after = vs.GetNext(); after == "" {
			break
		}
	}
	ids := []uuid.UUID{}
	for _, l := range lines {
		w, err := pdid.From(l.GetWorkspace().GetId())
		if err != nil {
			return nil, err
		}
		ids = append(ids, w.Uuid())
	}
	ws, err := h.Db.Workspace.Query().Where(entworkspace.TenantId(product.Uuid()), entworkspace.IdIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	keys := map[uuid.UUID]string{}
	for _, w := range ws {
		keys[w.Id] = text(w.Key)
	}
	zone := ""
	if p, err := pdid.From(v.GetPeriod().GetId()); err == nil {
		if row, err := h.Db.Period.Query().Where(entperiod.TenantId(product.Uuid()), entperiod.Id(p.Uuid())).Only(ctx); err == nil {
			zone = row.Zone
		}
	}
	for i, l := range lines {
		out.SetLines(append(out.GetLines(), levyanv1.InvoiceLine_builder{
			Statement: text(l.GetStatement().GetId()),
			Workspace: keys[ids[i]],
			Label:     l.GetLabel(),
			Period:    period(l.GetDateStart().AsTime(), l.GetDateEnd().AsTime(), zone),
			Total:     l.GetTotal(),
		}.Build()))
	}

	cns, err := h.creditNotes(ctx, levyan.CreditNoteFilter_builder{Invoice: levyan.InvoiceRef_builder{Id: id.Bytes()}.Build()}.Build())
	if err != nil {
		return nil, err
	}
	res := levyanv1.GetInvoiceResponse_builder{Invoice: out, CreditNotes: cns}.Build()

	// What is done of it since its charge failed, of the invoice the wall
	// answered.
	inv, err := h.Db.Invoice.Get(ctx, id.Uuid())
	if err != nil {
		return nil, err
	}
	ds, err := invoices.Dunning(ctx, h.Db, h.Dunning, []*ent.Invoice{inv}, clock(h.Now))
	if err != nil {
		return nil, err
	}
	if len(ds) > 0 {
		res.SetDunning(dunningOf(ds[0]))
	}

	return res, nil
}

func (h Payers) ListCreditNotes(ctx context.Context, req *levyanv1.ListCreditNotesRequest) (*levyanv1.ListCreditNotesResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_ListCreditNotes_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListCreditNotes_FullMethodName, pd.Reads{CreditNote: theirCreditNotes(product)}); err != nil {
		return nil, err
	}

	f := &levyan.CreditNoteFilter{}
	if v := req.GetBillingAccount(); v != "" {
		id, err := pdid.Parse(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", v)
		}
		f.SetBillingAccount(levyan.BillingAccountRef_builder{Id: id.Bytes()}.Build())
	}
	if v := req.GetInvoice(); v != "" {
		id, err := pdid.Parse(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invoice: %q is not an invoice's identifier", v)
		}
		f.SetInvoice(levyan.InvoiceRef_builder{Id: id.Bytes()}.Build())
	}

	cns, err := h.creditNotes(ctx, f)
	if err != nil {
		return nil, err
	}

	return levyanv1.ListCreditNotesResponse_builder{CreditNotes: cns}.Build(), nil
}

// creditNotes is every credit note `f` names that the wall lets through.
func (h Payers) creditNotes(ctx context.Context, f *levyan.CreditNoteFilter) ([]*levyanv1.CreditNote, error) {
	out := []*levyanv1.CreditNote{}
	filters := []*levyan.CreditNoteFilter{}
	if f.HasInvoice() || f.HasBillingAccount() {
		filters = append(filters, f)
	}
	after := ""
	for {
		vs, err := h.S.CreditNote().List(ctx, levyan.CreditNoteListRequest_builder{
			Filters: filters,
			Size:    page,
			After:   after,
		}.Build())
		if err != nil {
			return nil, err
		}
		for _, v := range vs.GetItems() {
			out = append(out, creditNoteOf(v))
		}
		if after = vs.GetNext(); after == "" {
			return out, nil
		}
	}
}

func invoiceOf(v *levyan.Invoice) *levyanv1.Invoice {
	out := levyanv1.Invoice_builder{
		Id:             text(v.GetId()),
		BillingAccount: text(v.GetBillingAccount().GetId()),
		State:          levyanv1.InvoiceState(v.GetState()),
		Start:          stamp(v.GetDateStart()),
		End:            stamp(v.GetDateEnd()),
		Currency:       v.GetCurrency(),
		Subtotal:       v.GetSubtotal(),
		Tax:            v.GetTax(),
		Total:          v.GetTotal(),
		Credited:       v.GetCredited(),
		Due:            v.GetDue(),
		Paid:           v.GetPaid(),
		Reduced:        v.GetReduced(),
		Remaining:      max(v.GetDue()-v.GetPaid()-v.GetReduced(), 0),
		Number:         v.GetNumber(),
		ChannelNumber:  v.GetChannelNumber(),
	}.Build()
	if c, err := billing.Channel(v.GetChannel()); err == nil {
		out.SetChannel(levyanv1.Channel(c))
	}
	if v.HasDateFinalized() {
		out.SetFinalized(stamp(v.GetDateFinalized()))
	}
	if v.HasDateDue() {
		out.SetDueDate(stamp(v.GetDateDue()))
	}
	if v.HasDateSettled() {
		out.SetSettled(stamp(v.GetDateSettled()))
	}

	return out
}

func creditNoteOf(v *levyan.CreditNote) *levyanv1.CreditNote {
	return levyanv1.CreditNote_builder{
		Id:       text(v.GetId()),
		Invoice:  text(v.GetInvoice().GetId()),
		Reason:   levyanv1.CreditNoteReason(v.GetReason()),
		Amount:   v.GetAmount(),
		Tax:      v.GetTax(),
		Total:    v.GetTotal(),
		Reduced:  v.GetReduced(),
		Refunded: v.GetRefunded(),
		Credited: v.GetCredited(),
		Number:   v.GetNumber(),
		Created:  timestamppb.New(v.GetDateCreated().AsTime()),
	}.Build()
}
