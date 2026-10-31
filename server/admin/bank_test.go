package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/xli/cfg"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/taxinvoice"
	"github.com/lesomnus/levyan/internal/taxinvoice/taxinvoicetest"
	"github.com/lesomnus/levyan/server/admin"
	"github.com/lesomnus/levyan/server/invoices/bank"
	"github.com/lesomnus/levyan/server/ledger"
)

// Made-up businesses: no registration number here is anybody's.
const (
	sellerRegistration = "000-00-00001"
	buyerRegistration  = "000-00-00002"
)

// openBank is the operator's entry point beside a deployment whose
// bank-transfer channel is configured, its tax invoices issued by the test's
// issuer.
func openBank(t *testing.T) *door {
	t.Helper()
	w := levyantest.New(t, func(c *cmd.Config) {
		c.Invoices.BankKR = cmd.BankConfig{
			Issuer: taxinvoicetest.Kind,
			Seller: cmd.SellerConfig{Registration: sellerRegistration, Name: "레비안 예시", Representative: "대표", Kind: "서비스", Item: "소프트웨어", Email: "tax@example.com"},
		}
	})
	ks := map[string]cfg.Secret{"alice": secret(t, aliceKey)}
	ops, err := admin.Operators(ks)
	w.X.NoError(err)
	r := &admin.Routes{S: w.S, Operators: ops, Expect: ledger.Expect{Gap: 36 * time.Hour, Recent: 7 * 24 * time.Hour}}
	w.X.NoError(r.Sync(t.Context()))
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	return &door{w: w, r: r, url: srv.URL}
}

func issuerOf(d *door) *taxinvoicetest.Issuer {
	return taxinvoicetest.Of(d.w.S.Invoices.Collectors[levyan.Channel_CHANNEL_BANK_KR].(*bank.Channel).Issuer)
}

// An institution pays by bank transfer with a tax invoice, end to end: an
// operator opens its billing account after a contract; its month is
// invoiced as the legal invoice, numbered, its tax apart, due in thirty days,
// with the tax invoice issued once by its number; the operator records the
// transfers as they come, gives some back by hand with an amended tax invoice
// behind the credit note, and voids the next month's, cancelling its tax
// invoice -- each write on the trail as theirs.
func TestABankTransferIsInvoicedWithATaxInvoice(t *testing.T) {
	d := openBank(t)
	x := d.w.X
	seoul, err := time.LoadLocation("Asia/Seoul")
	x.NoError(err)
	at := func(m, day int) time.Time { return time.Date(2027, time.Month(m), day, 0, 0, 0, 0, seoul) }
	d.w.At(at(1, 1).Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "Asia/Seoul", levyan.Channel_CHANNEL_BANK_KR, "KRW")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_BANK_KR, "KRW", 100, 50, at(1, 1))
	key, _ := d.w.Key(loft, "loft-prod", nil)
	conn := d.w.Serve()
	product := levyantest.As(context.Background(), key)

	// An account paying by transfer is opened by an operator alone, for a
	// business a tax invoice names.
	account := levyantest.Account()
	open := func(body map[string]any) (int, []byte) {
		return d.ask(aliceKey, http.MethodPost, "/admin/billing-accounts", body)
	}
	business := admin.Business{Registration: buyerRegistration, Name: "예시 재단", Representative: "이사장", Kind: "비영리", Item: "연구", Email: "tax@example.org"}
	code, b := open(map[string]any{"owner": account.String(), "channel": "mor", "business": business})
	x.Equal(http.StatusBadRequest, code, "%s", b)
	code, b = open(map[string]any{"owner": account.String(), "channel": "bank_kr", "business": admin.Business{Name: "예시 재단", Email: "tax@example.org"}})
	x.Equal(http.StatusBadRequest, code, "no registration: %s", b)
	var ba admin.BillingAccount
	d.as(aliceKey, http.MethodPost, "/admin/billing-accounts", map[string]any{
		"owner": account.String(), "channel": "bank_kr", "name": "본부", "recipient": "회계팀", "email": "billing@example.org", "business": business,
	}, &ba)
	x.Equal("bank_kr", ba.Channel)
	x.Equal("KRW", ba.Currency)
	x.Equal(d.operator("alice"), d.actorOf(ba.Id))

	_, err = d.w.S.Ungated.BillingAccount().Add(context.Background(), levyan.BillingAccountAddRequest_builder{
		Owner: account.Bytes(), Country: "KR", Channel: levyan.Channel_CHANNEL_BANK_KR.String(),
		Business: levyan.BillingBusiness_builder{Registration: buyerRegistration, Name: "예시 재단", Email: "tax@example.org"}.Build(),
	}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "nobody but an operator opens one")
	id, err := pdid.Parse(ba.Id)
	x.NoError(err)
	_, err = d.w.S.Payers.Update(context.Background(), account, id, levyan.BillingAccountPatchRequest_builder{
		Business: levyan.BillingBusiness_builder{Registration: "000-00-00003", Name: "다른 곳"}.Build(),
	}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err), "its business is its contract's")

	// Its workspace, January's and February's assets.
	d.w.At(at(1, 1))
	ws := levyantest.Tenant()
	_, err = levyanv1.NewWorkspaceServiceClient(conn).Arrive(product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: account.String(), Label: "연구소"}.Build())
	x.NoError(err)
	record := func(t time.Time, v int64) {
		_, err := levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
			levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(t), Value: v, Unit: "{asset}"}.Build(),
		}}.Build())
		x.NoError(err)
	}
	d.w.At(at(2, 1).Add(time.Hour))
	record(at(1, 20), 200)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)

	// The first pass's request for the tax invoice never arrives: it is
	// asked again by the next, under the same number, and issued once.
	issuer := issuerOf(d)
	issuer.Then(taxinvoicetest.Dropped)
	x.Error(d.w.S.Invoices.Run(context.Background()))
	var vs []admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices", nil, &vs)
	x.Len(vs, 1)
	x.Nil(vs[0].Issued, "not issued yet")
	x.NoError(d.w.S.Invoices.Run(context.Background()))

	var jan admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/invoices/"+vs[0].Id, nil, &jan)
	x.Equal("open", jan.State)
	x.Equal(int64(1), jan.Number)
	x.Equal(int64(5000), jan.Subtotal, "100 past what is included, at 50")
	x.Equal(int64(500), jan.Tax)
	x.Equal("1/10", jan.TaxRate)
	x.Equal(int64(5500), jan.Due)
	x.NotNil(jan.DueDate)
	x.True(jan.DueDate.Sub(*jan.Finalized) == bank.DefaultDue, "due thirty days after")
	x.NotNil(jan.Issued)
	x.NotEmpty(jan.Document, "the approval number")
	x.Empty(jan.Attempts, "nothing is sent on a transfer's channel")

	docs := issuer.Issued()
	x.Len(docs, 1)
	ti := docs[0]
	x.Equal("LV-INV-1", ti.Key)
	x.Equal(sellerRegistration, ti.Supplier.Registration)
	x.Equal(buyerRegistration, ti.Buyer.Registration)
	x.Equal("예시 재단", ti.Buyer.Name)
	x.Equal("tax@example.org", ti.Buyer.Email)
	x.Equal(int64(5000), ti.Supply)
	x.Equal(int64(500), ti.Tax)
	x.Equal("2027-01-31", ti.Written.Format(time.DateOnly), "the month's last day, in Seoul")
	x.Equal("loft 2027-01 이용료", ti.Items[0].Name)
	x.Nil(ti.Amends)

	// The transfers, as the operator sees them come.
	d.w.At(at(2, 6))
	var got admin.Invoice
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+jan.Id+"/paid", map[string]any{"amount": 3000, "reference": "예시재단", "date": at(2, 5)}, &got)
	x.Equal("open", got.State)
	x.Equal(int64(2500), got.Remaining)
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+jan.Id+"/paid", map[string]any{"amount": 2500, "reference": "예시재단 잔액"}, &got)
	x.Equal("paid", got.State)
	x.Len(got.Attempts, 2)
	for _, a := range got.Attempts {
		x.True(a.Recorded)
		x.Equal(d.operator("alice"), d.actorOf(a.Id))
	}

	// Some given back, by a transfer the operator makes and records; the
	// credit note behind it is numbered, with an amended tax invoice.
	d.w.At(at(2, 11))
	code, b = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+jan.Id+"/credit", map[string]any{"amount": 1000, "reason": "refund"})
	x.Equal(http.StatusBadRequest, code, "a refund by hand says its transfer: %s", b)
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+jan.Id+"/credit", map[string]any{"amount": 1000, "reason": "refund", "reference": "환불 이체", "date": at(2, 10)}, &got)
	x.Len(got.CreditNotes, 1)
	cn := got.CreditNotes[0]
	x.Equal(int64(1), cn.Number)
	x.Equal(int64(100), cn.Tax)
	x.Equal(int64(1100), cn.Refunded)
	refunds := 0
	for _, a := range got.Attempts {
		if a.Kind == "refund" {
			refunds++
			x.Equal("succeeded", a.State)
			x.Equal("환불 이체", a.Reference)
		}
	}
	x.Equal(1, refunds)
	x.NoError(d.w.S.Invoices.Run(context.Background()))
	docs = issuer.Issued()
	x.Len(docs, 2)
	amended := docs[1]
	x.Equal("LV-CN-1", amended.Key)
	x.Equal(int64(-1000), amended.Supply)
	x.Equal(int64(-100), amended.Tax)
	x.NotNil(amended.Amends)
	x.Equal("LV-INV-1", amended.Amends.Key)
	x.Equal(jan.Document, amended.Amends.Approval)
	x.Equal(taxinvoice.Changed, amended.Amends.Cause)
	x.Equal("당초 작성일 2027-01-31", amended.Note)

	// February's, voided: what is left of it taken back, and its tax
	// invoice cancelled.
	d.w.At(at(3, 1).Add(time.Hour))
	record(at(2, 20), 200)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)
	x.NoError(d.w.S.Invoices.Run(context.Background()))
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices?state=open", nil, &vs)
	x.Len(vs, 1)
	feb := vs[0]
	x.Equal(int64(2), feb.Number)
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+feb.Id+"/void", map[string]any{"reason": "계약 해지"}, &got)
	x.Equal("void", got.State)
	x.Equal(int64(2), got.CreditNotes[0].Number)
	x.Equal(d.operator("alice"), d.actorOf(got.CreditNotes[0].Id))
	x.NoError(d.w.S.Invoices.Run(context.Background()))
	docs = issuer.Issued()
	x.Len(docs, 4)
	cancelled := docs[3]
	x.Equal("LV-CN-2", cancelled.Key)
	x.Equal(taxinvoice.Cancelled, cancelled.Amends.Cause)
	x.Equal(int64(-5000), cancelled.Supply)
	x.Equal(int64(-500), cancelled.Tax)

	// Each document was issued once, however often the loop asked.
	x.NoError(d.w.S.Invoices.Run(context.Background()))
	x.Len(issuer.Issued(), 4)
}

// The channel is configured or it is not: a seller no tax invoice can name,
// or an issuer of a kind no binary links, stops the deployment where it comes
// up; and no country's billing accounts are opened on it by default.
func TestTheBankChannelIsConfiguredOrRefused(t *testing.T) {
	x := levyantest.New(t).X

	for _, c := range []cmd.BankConfig{
		{Issuer: taxinvoicetest.Kind, Seller: cmd.SellerConfig{Registration: "12-34", Name: "레비안"}},
		{Issuer: taxinvoicetest.Kind, Seller: cmd.SellerConfig{Registration: sellerRegistration}},
		{Issuer: "popbill", Seller: cmd.SellerConfig{Registration: sellerRegistration, Name: "레비안"}},
	} {
		conf := cmd.Config{}
		conf.Db.Driver, conf.Db.Dsn = "sqlite3", "file:bank?mode=memory&cache=shared&_fk=1"
		conf.Watch.Broker = "memory"
		conf.Invoices.BankKR = c
		_, err := cmd.Build(t.Context(), conf)
		x.ErrorContains(err, "invoices.bank_kr.")
	}

	conf := cmd.Config{}
	conf.Payers.Channels = cmd.ChannelsConfig{"KR": {Channel: "BANK_KR", Currency: "KRW"}}
	_, err := conf.Payers.ChannelsOf()
	x.ErrorContains(err, "opened by an operator")
}
