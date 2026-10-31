package invoices_test

import (
	"testing"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
	"github.com/lesomnus/levyan/server/pd"
)

// What a key was charged past what the ledger accounts for is given back by
// a refund of its own (#13): of the extra payment, under a key of its own,
// sent as any refund is -- and nothing of the invoice moves, which its
// charge paid once. Asked for again, it is the same refund; the payment the
// charge settled by is no extra one.
func TestAnExtraPaymentIsGivenBackOfItself(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	inv := w.only(ba)
	x.Equal(paid, inv.State)
	charge := w.attempts(inv)[0]

	_, err := w.S.Invoices.RefundExtra(w.ctx, pdid.Id(charge.Id), charge.ChannelRef, 5000)
	x.Equal(codes.FailedPrecondition, status.Code(err), "what it settled by is no extra payment")
	_, err = w.S.Invoices.RefundExtra(w.ctx, pdid.Id(charge.Id), "ref-extra", 0)
	x.Equal(codes.InvalidArgument, status.Code(err))

	r, err := w.S.Invoices.RefundExtra(w.ctx, pdid.Id(charge.Id), "ref-extra", 5000)
	x.NoError(err)
	x.Equal(int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND), r.Kind)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED), r.State)
	x.Equal(charge.AttemptKey, r.ExtraOf)
	x.Equal("ref-extra", r.Refunds)
	x.Equal([16]byte{}, [16]byte(r.CreditNoteId), "no credit note says it")
	_, err = w.S.Invoices.RefundExtra(w.ctx, pdid.Id(r.Id), "ref-extra", 5000)
	x.Equal(codes.FailedPrecondition, status.Code(err), "of a charge's")

	again, err := w.S.Invoices.RefundExtra(w.ctx, pdid.Id(charge.Id), "ref-extra", 5000)
	x.NoError(err)
	x.Equal(r.Id, again.Id, "asked for already")

	v, err := w.S.Invoices.Drive(w.ctx, pdid.Id(r.Id))
	x.NoError(err)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), v.State)
	sends := w.ch.Sends()
	s := sends[len(sends)-1]
	x.Equal("ref-extra", s.Refunds, "given back of the extra payment")
	x.Equal(charge.AttemptKey, s.Charge, "of its line of the key")
	x.Equal("중복 청구 환불", s.Reason)

	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Paid)
	x.Zero(inv.Reduced)
	n, err := w.S.Ent.CreditNote.Query().Where(entcreditnote.InvoiceId(inv.Id)).Count(w.ctx)
	x.NoError(err)
	x.Zero(n)
	x.Zero(w.balance(ba))
}

// A refund gives back what a credit note says, or what a key was charged
// twice, and nothing else: written past the ledger, one that says neither
// is refused, as is one that says half of the other, or both.
func TestARefundSaysWhatItGivesBack(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	inv := w.only(ba)
	cn, err := w.S.Invoices.Credit(w.ctx, id(inv), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 1000})
	x.NoError(err)

	add := func(f func(*levyan.PaymentAttemptAddRequest)) error {
		at := pdid.New(pd.PaymentAttemptDomain)
		req := levyan.PaymentAttemptAddRequest_builder{
			Id:         at.Bytes(),
			Tenant:     levyan.TenantRef_builder{Id: inv.TenantId[:]}.Build(),
			Invoice:    levyan.InvoiceRef_builder{Id: inv.Id[:]}.Build(),
			Kind:       levyan.AttemptKind_ATTEMPT_KIND_REFUND,
			AttemptKey: invoices.KeyOf(at),
			Channel:    inv.Channel,
			Currency:   inv.Currency,
			Amount:     100,
			State:      levyan.AttemptState_ATTEMPT_STATE_REQUESTED,
		}.Build()
		f(req)
		_, err := w.S.Ungated.PaymentAttempt().Add(w.ctx, req)
		return err
	}
	x.Equal(codes.InvalidArgument, status.Code(add(func(*levyan.PaymentAttemptAddRequest) {})), "neither")
	x.Equal(codes.InvalidArgument, status.Code(add(func(r *levyan.PaymentAttemptAddRequest) { r.SetExtraOf("lv-0001") })), "the charge, and no payment")
	x.Equal(codes.InvalidArgument, status.Code(add(func(r *levyan.PaymentAttemptAddRequest) {
		r.SetExtraOf("lv-0001")
		r.SetRefunds("ref-1")
		r.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn.Id[:]}.Build())
	})), "both")
	x.Equal(codes.InvalidArgument, status.Code(add(func(r *levyan.PaymentAttemptAddRequest) {
		r.SetKind(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
		r.SetExtraOf("lv-0001")
		r.SetRefunds("ref-1")
	})), "a charge gives back nothing")
	x.NoError(add(func(r *levyan.PaymentAttemptAddRequest) {
		r.SetCreditNote(levyan.CreditNoteRef_builder{Id: cn.Id[:]}.Build())
	}))
	x.NoError(add(func(r *levyan.PaymentAttemptAddRequest) {
		r.SetExtraOf("lv-0001")
		r.SetRefunds("ref-1")
	}))
}

// An attempt that is out is asked about as the loop asks: what the channel
// says came of it is written. One that is not out is answered as it is, and
// nothing is asked.
func TestAnAttemptIsAskedAboutAsTheLoopAsks(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.ch.Then(invoicestest.Lost)
	w.run()
	inv := w.only(ba)
	a := w.attempts(inv)[0]
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN), a.State)

	asks := len(w.ch.Asks())
	v, err := w.S.Invoices.Ask(w.ctx, pdid.Id(a.Id))
	x.NoError(err)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), v.State)
	x.Equal(paid, w.again(inv).State)
	x.Len(w.ch.Asks(), asks+1)

	v, err = w.S.Invoices.Ask(w.ctx, pdid.Id(a.Id))
	x.NoError(err)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), v.State)
	x.Len(w.ch.Asks(), asks+1, "ended: nothing is asked")

	byKey, err := w.S.Invoices.AttemptOf(w.ctx, a.AttemptKey)
	x.NoError(err)
	x.Equal(a.Id, byKey.Id)
	_, err = w.S.Invoices.AttemptOf(w.ctx, "lv-nothing")
	x.Equal(codes.NotFound, status.Code(err))
}
