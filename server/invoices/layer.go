package invoices

import (
	"context"

	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/bare"
)

// Layer is what an invoice, its lines, a credit note, an attempt and a
// movement of credit may be, said of every write of one, on both stacks
// (`cmd.Build`): so that what is written past [Invoices] is still held to it.
//
//   - An invoice is begun a draft, numbered by nothing, with nothing paid or
//     taken off. While it is a draft it is gathered into; once it is not, what
//     it asked for -- its end, its amounts, its number, its bill-to, when it
//     is due -- is what it stays, and only how it is settled moves: its
//     state, along the ways a state may go, what was paid of it and what
//     credit notes took off, and the channel's word on it.
//   - A line is of a statement of the invoice's billing account, for the
//     statement's total, on a draft.
//   - A credit note is of a finalized invoice, for more than nothing, and
//     what it does adds up to what it takes back.
//   - An attempt is written to be sent -- or as one an operator saw succeed
//     -- and its state goes on from there and never back from an end. Of
//     one that ended, only a mark for reconciliation and the channel's
//     number of it, where it had none, are written after.
//   - A movement of credit moves it one way, as its kind says.
//
// Apply is refused for all of them: a document is held to none of this.
type Layer struct {
	levyan.Overlay
}

// New is the layer over `next`.
func New(next levyan.Server) Layer { return Layer{Overlay: levyan.NewOverlay(next)} }

// Build makes a builder of this layer so that it can be stacked.
func Build() levyan.Builder { return builder{} }

type builder struct{}

func (builder) Build(next levyan.Server) (levyan.Server, error) { return New(next), nil }

// WithDriver is this layer over the stack put on `drv`.
func (s Layer) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return New(next), nil
}

var (
	_ levyan.Server               = Layer{}
	_ enttx.Binder[levyan.Server] = Layer{}
)

var refuseApply = status.Error(codes.FailedPrecondition, "an invoice's rows are changed by the ledger's Patch, which holds what they may be, and never by a document")

// next is where an invoice may go from each state.
var next = map[levyan.InvoiceState][]levyan.InvoiceState{
	levyan.InvoiceState_INVOICE_STATE_DRAFT: {
		levyan.InvoiceState_INVOICE_STATE_DRAFT,
		levyan.InvoiceState_INVOICE_STATE_OPEN,
		levyan.InvoiceState_INVOICE_STATE_PAID,
		levyan.InvoiceState_INVOICE_STATE_WRITTEN_OFF,
		levyan.InvoiceState_INVOICE_STATE_SHADOW,
	},
	levyan.InvoiceState_INVOICE_STATE_OPEN: {
		levyan.InvoiceState_INVOICE_STATE_OPEN,
		levyan.InvoiceState_INVOICE_STATE_PAID,
		levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE,
		levyan.InvoiceState_INVOICE_STATE_VOID,
	},
	levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE: {
		levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE,
		levyan.InvoiceState_INVOICE_STATE_PAID,
		levyan.InvoiceState_INVOICE_STATE_VOID,
	},
	levyan.InvoiceState_INVOICE_STATE_PAID:        {levyan.InvoiceState_INVOICE_STATE_PAID},
	levyan.InvoiceState_INVOICE_STATE_VOID:        {levyan.InvoiceState_INVOICE_STATE_VOID},
	levyan.InvoiceState_INVOICE_STATE_WRITTEN_OFF: {levyan.InvoiceState_INVOICE_STATE_WRITTEN_OFF},
	levyan.InvoiceState_INVOICE_STATE_SHADOW:      {levyan.InvoiceState_INVOICE_STATE_SHADOW},
}

type layerInvoice struct {
	Layer
	levyan.InvoiceServiceServer
}

func (s Layer) Invoice() levyan.InvoiceServiceServer {
	v := layerInvoice{s, s.Next().Invoice()}
	v.InvoiceServiceServer = bare.InvoiceWithAddRule(v.InvoiceServiceServer, admitInvoice)
	return v
}

func admitInvoice(ctx context.Context, req *levyan.InvoiceAddRequest) (*levyan.InvoiceAddRequest, error) {
	switch {
	case req.GetState() != levyan.InvoiceState_INVOICE_STATE_DRAFT:
		return nil, pderr.Invalidf("state", "an invoice is begun a draft")
	case req.GetBillingAccount() == nil:
		return nil, pderr.Invalidf("billing_account", "an invoice bills a billing account")
	case req.GetPeriod() == nil:
		return nil, pderr.Invalidf("period", "an invoice is begun for a period")
	case req.GetChannel() == "" || req.GetCurrency() == "":
		return nil, pderr.Invalidf("channel", "an invoice is on its billing account's channel, in its currency")
	case req.HasNumber() || req.HasDateFinalized() || req.HasDateIssued() || req.HasDateSettled() || req.HasBillTo():
		return nil, pderr.Invalidf("number", "a draft is numbered, issued and settled by nothing yet")
	case req.GetPaid() != 0 || req.GetReduced() != 0 || req.GetCredited() != 0 || req.GetDue() != 0:
		return nil, pderr.Invalidf("paid", "a draft asks for nothing yet, and nothing is paid of it")
	}

	return req, nil
}

// Patch is an invoice gathered into while it is a draft, and settled after.
func (s layerInvoice) Patch(ctx context.Context, req *levyan.InvoicePatchRequest) (*levyan.Invoice, error) {
	was, err := s.Next().Invoice().Get(ctx, levyan.InvoiceGetRequest_builder{
		Ref:    req.GetRef(),
		Select: levyan.InvoiceSelect_builder{All: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	from := was.GetState()
	to := from
	if req.HasState() {
		to = req.GetState()
	}
	ok := false
	for _, v := range next[from] {
		ok = ok || v == to
	}
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "an invoice %s does not become %s", stateName(int32(from)), stateName(int32(to)))
	}

	if from != levyan.InvoiceState_INVOICE_STATE_DRAFT {
		asked := req.HasDateEnd() || req.HasSubtotal() || req.HasTax() || req.HasTaxRate() || req.HasTotal() ||
			req.HasCredited() || req.HasDue() || req.HasNumber() || req.GetNumberNull() || req.HasDateFinalized() ||
			req.HasBillTo() || req.GetBillToNull() || req.HasDateDue() || req.HasCarried() || req.HasDateCarried()
		if asked {
			return nil, status.Error(codes.FailedPrecondition, "a finalized invoice is what it was finalized as: a correction is a credit note")
		}
	}
	if req.HasPaid() && req.GetPaid() < was.GetPaid() || req.HasReduced() && req.GetReduced() < was.GetReduced() {
		return nil, pderr.Invalidf("paid", "what was paid of an invoice, and taken off it, only grows")
	}

	return s.InvoiceServiceServer.Patch(ctx, req)
}

func (s layerInvoice) Apply(ctx context.Context, req *levyan.InvoiceApplyRequest) (*levyan.Invoice, error) {
	return nil, refuseApply
}

type layerInvoiceLine struct {
	Layer
	levyan.InvoiceLineServiceServer
}

func (s Layer) InvoiceLine() levyan.InvoiceLineServiceServer {
	v := layerInvoiceLine{s, s.Next().InvoiceLine()}
	v.InvoiceLineServiceServer = bare.InvoiceLineWithAddRule(v.InvoiceLineServiceServer, v.admit)
	return v
}

func (s layerInvoiceLine) admit(ctx context.Context, req *levyan.InvoiceLineAddRequest) (*levyan.InvoiceLineAddRequest, error) {
	inv, err := s.Next().Invoice().Get(ctx, levyan.InvoiceGetRequest_builder{
		Ref: req.GetInvoice(),
		Select: levyan.InvoiceSelect_builder{
			State:          proto.Bool(true),
			BillingAccount: &levyan.BillingAccountSelect{},
		}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	if inv.GetState() != levyan.InvoiceState_INVOICE_STATE_DRAFT {
		return nil, status.Errorf(codes.FailedPrecondition, "a line is put on a draft, and the invoice is %s", stateName(int32(inv.GetState())))
	}
	st, err := s.Next().Statement().Get(ctx, levyan.StatementGetRequest_builder{
		Ref: req.GetStatement(),
		Select: levyan.StatementSelect_builder{
			Total:          proto.Bool(true),
			BillingAccount: &levyan.BillingAccountSelect{},
		}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	if !proto.Equal(st.GetBillingAccount(), inv.GetBillingAccount()) || st.GetBillingAccount() == nil {
		return nil, pderr.Invalidf("statement", "a statement goes on an invoice of the billing account it names")
	}
	if req.GetTotal() != st.GetTotal() {
		return nil, pderr.Invalidf("total", "a line is for what its statement comes to: %d", st.GetTotal())
	}

	return req, nil
}

type layerCreditNote struct {
	Layer
	levyan.CreditNoteServiceServer
}

func (s Layer) CreditNote() levyan.CreditNoteServiceServer {
	v := layerCreditNote{s, s.Next().CreditNote()}
	v.CreditNoteServiceServer = bare.CreditNoteWithAddRule(v.CreditNoteServiceServer, v.admit)
	return v
}

func (s layerCreditNote) admit(ctx context.Context, req *levyan.CreditNoteAddRequest) (*levyan.CreditNoteAddRequest, error) {
	inv, err := s.Next().Invoice().Get(ctx, levyan.InvoiceGetRequest_builder{
		Ref:    req.GetInvoice(),
		Select: levyan.InvoiceSelect_builder{State: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	switch inv.GetState() {
	case levyan.InvoiceState_INVOICE_STATE_OPEN, levyan.InvoiceState_INVOICE_STATE_PAID, levyan.InvoiceState_INVOICE_STATE_UNCOLLECTIBLE:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "a credit note is written against a finalized invoice, and this one is %s", stateName(int32(inv.GetState())))
	}
	switch {
	case req.GetReason() == levyan.CreditNoteReason_CREDIT_NOTE_REASON_UNSPECIFIED:
		return nil, pderr.Invalidf("reason", "a credit note says why it takes back")
	case req.GetAmount() <= 0:
		return nil, pderr.Invalidf("amount", "a credit note takes back more than nothing")
	case req.GetTax() < 0 || req.GetReduced() < 0 || req.GetRefunded() < 0 || req.GetCredited() < 0:
		return nil, pderr.Invalidf("total", "a credit note takes nothing back less than nothing")
	case req.GetTotal() != req.GetAmount()+req.GetTax():
		return nil, pderr.Invalidf("total", "a credit note's total is its amount and its tax")
	case req.GetReduced()+req.GetRefunded()+req.GetCredited() != req.GetTotal():
		return nil, pderr.Invalidf("total", "what a credit note takes off, gives back and credits is what it takes back")
	case req.HasDateIssued():
		return nil, pderr.Invalidf("date_issued", "a credit note is issued after it is written")
	}

	return req, nil
}

func (s layerCreditNote) Apply(ctx context.Context, req *levyan.CreditNoteApplyRequest) (*levyan.CreditNote, error) {
	return nil, refuseApply
}

type layerAttempt struct {
	Layer
	levyan.PaymentAttemptServiceServer
}

func (s Layer) PaymentAttempt() levyan.PaymentAttemptServiceServer {
	v := layerAttempt{s, s.Next().PaymentAttempt()}
	v.PaymentAttemptServiceServer = bare.PaymentAttemptWithAddRule(v.PaymentAttemptServiceServer, admitAttempt)
	return v
}

func admitAttempt(ctx context.Context, req *levyan.PaymentAttemptAddRequest) (*levyan.PaymentAttemptAddRequest, error) {
	switch {
	case req.GetAttemptKey() == "":
		return nil, pderr.Invalidf("attempt_key", "an attempt has a key of its own, which keeps it once at the channel")
	case req.GetAmount() <= 0:
		return nil, pderr.Invalidf("amount", "an attempt moves more than nothing")
	case req.GetKind() == levyan.AttemptKind_ATTEMPT_KIND_UNSPECIFIED:
		return nil, pderr.Invalidf("kind", "an attempt is a charge or a refund")
	case req.GetSends() != 0 || req.HasDateSent():
		return nil, pderr.Invalidf("sends", "an attempt is written before it is sent")
	case req.GetRecorded() && req.GetState() != levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED:
		return nil, pderr.Invalidf("state", "an attempt an operator records is one they saw succeed")
	case !req.GetRecorded() && req.GetState() != levyan.AttemptState_ATTEMPT_STATE_REQUESTED:
		return nil, pderr.Invalidf("state", "an attempt is written to be sent")
	case req.GetKind() == levyan.AttemptKind_ATTEMPT_KIND_REFUND && req.GetCreditNote() == nil:
		return nil, pderr.Invalidf("credit_note", "a refund gives back what a credit note says")
	}

	return req, nil
}

// Patch is an attempt taken on: never back from where it ended, and its sends
// never fewer. Of one that ended, what may still be written is that its
// channel holds more of its key than the ledger accounts for, and the
// channel's number of it where it had none.
func (s layerAttempt) Patch(ctx context.Context, req *levyan.PaymentAttemptPatchRequest) (*levyan.PaymentAttempt, error) {
	was, err := s.Next().PaymentAttempt().Get(ctx, levyan.PaymentAttemptGetRequest_builder{
		Ref:    req.GetRef(),
		Select: levyan.PaymentAttemptSelect_builder{State: proto.Bool(true), Sends: proto.Bool(true), ChannelNumber: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	switch was.GetState() {
	case levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, levyan.AttemptState_ATTEMPT_STATE_FAILED:
		if !marks(req, was) {
			return nil, status.Errorf(codes.FailedPrecondition, "an attempt that %s is what it is", attemptState(was.GetState()))
		}
	}
	if req.HasSends() && req.GetSends() < was.GetSends() {
		return nil, pderr.Invalidf("sends", "an attempt's sends only grow")
	}
	if req.HasState() && req.GetState() == levyan.AttemptState_ATTEMPT_STATE_UNSPECIFIED {
		return nil, pderr.Invalidf("state", "an attempt is somewhere")
	}

	return s.PaymentAttemptServiceServer.Patch(ctx, req)
}

func (s layerAttempt) Apply(ctx context.Context, req *levyan.PaymentAttemptApplyRequest) (*levyan.PaymentAttempt, error) {
	return nil, refuseApply
}

// marks says whether `req` writes of attempt `was`, which ended, only what may
// still be written of it: when its channel was found to hold more of its key
// than the ledger accounts for, and the channel's number of it where it had
// none -- and one of the two.
func marks(req *levyan.PaymentAttemptPatchRequest, was *levyan.PaymentAttempt) bool {
	fills := was.GetChannelNumber() == "" && req.GetChannelNumber() != ""
	if !req.HasDateTwice() && !fills {
		return false
	}
	rest := proto.CloneOf(req)
	rest.ClearRef()
	rest.ClearDateUpdatedForce()
	rest.ClearDateTwice()
	if fills {
		rest.ClearChannelNumber()
	}

	return proto.Size(rest) == 0
}

func (s Layer) Credit() levyan.CreditServiceServer {
	return bare.CreditWithAddRule(s.Next().Credit(), admitCredit)
}

func admitCredit(ctx context.Context, req *levyan.CreditAddRequest) (*levyan.CreditAddRequest, error) {
	switch req.GetKind() {
	case levyan.CreditKind_CREDIT_KIND_APPLIED:
		if req.GetAmount() >= 0 {
			return nil, pderr.Invalidf("amount", "what the balance pays of an invoice is taken from it: less than nothing")
		}
	case levyan.CreditKind_CREDIT_KIND_NEGATIVE, levyan.CreditKind_CREDIT_KIND_CREDIT_NOTE, levyan.CreditKind_CREDIT_KIND_OVERPAID, levyan.CreditKind_CREDIT_KIND_RETURNED:
		if req.GetAmount() <= 0 {
			return nil, pderr.Invalidf("amount", "what is owed is more than nothing")
		}
	default:
		return nil, pderr.Invalidf("kind", "a movement of credit says where it comes from or goes")
	}

	return req, nil
}

func attemptState(v levyan.AttemptState) string {
	switch v {
	case levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED:
		return "succeeded"
	case levyan.AttemptState_ATTEMPT_STATE_FAILED:
		return "failed"
	default:
		return v.String()
	}
}
