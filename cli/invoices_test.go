package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
)

// What a payer is asked to pay, as a product's user reads it through the
// product (#9): the invoices of its own billing accounts in that product,
// once finalized, with their lines -- each named as the product calls its
// workspace -- and their credit notes. Another account's are nothing of
// theirs, and another product's invoices of theirs are not this product's to
// show.
func TestAPayerReadsItsInvoicesThroughTheProduct(t *testing.T) {
	x := pdtest.NewX(t)
	w := levyantest.New(t)
	at := func(m, d int) time.Time { return time.Date(2027, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	w.At(at(1, 1).Add(-24 * time.Hour))

	loft := w.Terms(w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE)
	w.Price(loft, assets, levyan.Channel_CHANNEL_MOR, "KRW", 100, 50, at(1, 1))
	other := w.Terms(w.Product("other"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	otherKey, _ := w.Key(other, "other-prod", nil)
	ch := invoicestest.New()
	w.S.Invoices.Collectors[levyan.Channel_CHANNEL_MOR] = ch

	conn := w.Serve()
	product := levyantest.As(context.Background(), key)
	payers := levyanv1.NewPayerServiceClient(conn)
	workspaces := levyanv1.NewWorkspaceServiceClient(conn)
	as := func(a pdid.Id) context.Context { return levyantest.AsAccount(context.Background(), key, a) }

	// A payer with a billing account, paying for a workspace the product
	// calls "디자인팀"; and another account, paying for nothing.
	me, stranger := levyantest.Account(), levyantest.Account()
	ba, err := payers.CreateBillingAccount(as(me), levyanv1.CreateBillingAccountRequest_builder{
		Name: "회사용", Country: "KR", Recipient: levyanv1.Recipient_builder{Name: "수민"}.Build(),
	}.Build())
	x.NoError(err)
	w.At(at(1, 1))
	ws := levyantest.Tenant()
	_, err = workspaces.Arrive(product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: me.String(), Created: timestamppb.New(at(1, 1)), Label: "디자인팀"}.Build())
	x.NoError(err)

	w.At(at(2, 10))
	rs := []*levyanv1.Reading{}
	for d := at(1, 1); d.Before(at(2, 1)); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(d), Value: 200, Unit: "{asset}"}.Build())
	}
	_, err = levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	x.NoError(err)
	p, err := w.S.Ent.Tenant.Get(context.Background(), pdid.Id(loft.GetId()).Uuid())
	x.NoError(err)
	_, err = w.S.Closer.Close(context.Background(), p, at(2, 10))
	x.NoError(err)
	x.NoError(w.S.Invoices.Run(context.Background()))

	// The payer's, through the product: one invoice, paid, of 5,000.
	list, err := payers.ListInvoices(as(me), &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Len(list.GetInvoices(), 1)
	inv := list.GetInvoices()[0]
	x.Equal(ba.GetBillingAccount().GetId(), inv.GetBillingAccount())
	x.Equal(levyanv1.InvoiceState_INVOICE_STATE_PAID, inv.GetState())
	x.Equal(levyanv1.Channel_CHANNEL_MOR, inv.GetChannel())
	x.Equal(int64(5000), inv.GetTotal())
	x.Zero(inv.GetRemaining())
	x.Zero(inv.GetNumber(), "the merchant of record numbers what it sells")
	x.NotEmpty(inv.GetChannelNumber())
	x.Empty(inv.GetLines(), "a list has no lines")

	got, err := payers.GetInvoice(as(me), levyanv1.GetInvoiceRequest_builder{Id: inv.GetId()}.Build())
	x.NoError(err)
	x.Len(got.GetInvoice().GetLines(), 1)
	line := got.GetInvoice().GetLines()[0]
	x.Equal(ws.String(), line.GetWorkspace())
	x.Equal("디자인팀", line.GetLabel(), "as the product calls it")
	x.Equal(int64(5000), line.GetTotal())
	x.Equal("UTC", line.GetPeriod().GetZone())
	x.NotEmpty(line.GetStatement())
	x.Empty(got.GetCreditNotes())

	// A credit note against it, which the payer reads too.
	id, err := pdid.Parse(inv.GetId())
	x.NoError(err)
	_, err = w.S.Invoices.Credit(context.Background(), id, invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 500})
	x.NoError(err)
	cns, err := payers.ListCreditNotes(as(me), &levyanv1.ListCreditNotesRequest{})
	x.NoError(err)
	x.Len(cns.GetCreditNotes(), 1)
	x.Equal(levyanv1.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, cns.GetCreditNotes()[0].GetReason())
	x.Equal(int64(500), cns.GetCreditNotes()[0].GetCredited())
	got, err = payers.GetInvoice(as(me), levyanv1.GetInvoiceRequest_builder{Id: inv.GetId()}.Build())
	x.NoError(err)
	x.Len(got.GetCreditNotes(), 1)

	// Another account's are nothing of theirs.
	none, err := payers.ListInvoices(as(stranger), &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Empty(none.GetInvoices())
	_, err = payers.GetInvoice(as(stranger), levyanv1.GetInvoiceRequest_builder{Id: inv.GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err))
	nocns, err := payers.ListCreditNotes(as(stranger), &levyanv1.ListCreditNotesRequest{})
	x.NoError(err)
	x.Empty(nocns.GetCreditNotes())

	// And through another product the payer's invoice of loft is not there.
	viaOther := levyantest.AsAccount(context.Background(), otherKey, me)
	elsewhere, err := payers.ListInvoices(viaOther, &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Empty(elsewhere.GetInvoices(), "loft's is not another product's to show")
	_, err = payers.GetInvoice(viaOther, levyanv1.GetInvoiceRequest_builder{Id: inv.GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err))

	// A product calls none of it as itself, and the generated methods are
	// nobody's.
	_, err = payers.ListInvoices(product, &levyanv1.ListInvoicesRequest{})
	x.Equal(codes.PermissionDenied, status.Code(err))
	_, err = levyan.NewInvoiceServiceClient(conn).List(product, levyan.InvoiceListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err))
	_, err = levyan.NewInvoiceServiceClient(conn).List(as(me), levyan.InvoiceListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err))
}
