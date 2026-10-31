package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/admin"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
)

// An operator finds a product's invoices and looks at one, records a
// transfer that came for it, writes a credit note against one and voids
// another -- each on the trail as theirs -- and is refused what an invoice
// may not have done to it.
func TestAnOperatorKeepsAProductsInvoices(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_MOR, "KRW", 100, 50, jan)
	key, _ := d.w.Key(loft, "loft-prod", nil)
	ch := &invoicestest.Channel{After: 30 * 24 * time.Hour}
	d.w.S.Invoices.Collectors[levyan.Channel_CHANNEL_MOR] = ch
	conn := d.w.Serve()
	product := levyantest.As(context.Background(), key)

	d.w.At(jan)
	a, b := levyantest.Account(), levyantest.Account()
	wa, wb := levyantest.Tenant(), levyantest.Tenant()
	for _, v := range []struct {
		account, ws pdid.Id
	}{{a, wa}, {b, wb}} {
		_, err := d.w.S.Payers.Create(context.Background(), v.account, levyan.BillingAccount_builder{Country: "KR", Recipient: "수민"}.Build())
		x.NoError(err)
		_, err = levyanv1.NewWorkspaceServiceClient(conn).Arrive(product, levyanv1.ArriveRequest_builder{Workspace: v.ws.String(), Creator: v.account.String()}.Build())
		x.NoError(err)
	}

	d.w.At(jan.AddDate(0, 1, 0))
	for _, ws := range []pdid.Id{wa, wb} {
		_, err := levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
			levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 0, 20)), Value: 200, Unit: "{asset}"}.Build(),
		}}.Build())
		x.NoError(err)
	}
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)
	x.NoError(d.w.S.Invoices.Run(context.Background()))

	var vs []admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices?state=open", nil, &vs)
	x.Len(vs, 2)
	for _, v := range vs {
		x.Equal(int64(5000), v.Due)
		x.Equal("mor", v.Channel)
		x.NotNil(v.DueDate, "a transfer's channel says when")
	}
	code, _ := d.ask(aliceKey, http.MethodGet, "/admin/products/loft/invoices?state=lost", nil)
	x.Equal(http.StatusBadRequest, code)

	// A transfer of all of the first, by hand.
	first, second := vs[0], vs[1]
	var got admin.Invoice
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/paid", map[string]any{"amount": 5000, "reference": "수민 1월분"}, &got)
	x.Equal("paid", got.State)
	x.Len(got.Attempts, 1)
	x.True(got.Attempts[0].Recorded)
	x.Equal("수민 1월분", got.Attempts[0].Reference)
	x.Equal(d.operator("alice"), d.actorOf(got.Attempts[0].Id), "the trail says who saw it")
	code, b2 := d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/paid", map[string]any{"amount": 1})
	x.Equal(http.StatusBadRequest, code, "%s", b2)

	// A correction of it, credited.
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/credit", map[string]any{"amount": 700, "words": "단가 착오"}, &got)
	x.Len(got.CreditNotes, 1)
	x.Equal("correction", got.CreditNotes[0].Reason)
	x.Equal(int64(700), got.CreditNotes[0].Credited)
	x.Equal("단가 착오", got.CreditNotes[0].Words)
	x.Equal(int64(700), *got.Balance)
	x.Equal(d.operator("alice"), d.actorOf(got.CreditNotes[0].Id))
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/credit", map[string]any{"amount": 700, "reason": "cancellation"})
	x.Equal(http.StatusBadRequest, code, "a cancellation is a void")
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/credit", map[string]any{"amount": 9999})
	x.Equal(http.StatusBadRequest, code, "more than is left of it")

	// The second, voided.
	d.as(aliceKey, http.MethodPost, "/admin/invoices/"+second.Id+"/void", map[string]any{"reason": "계약 해지"}, &got)
	x.Equal("void", got.State)
	x.Equal("계약 해지", got.Reason)
	x.Len(got.CreditNotes, 1)
	x.Equal("cancellation", got.CreditNotes[0].Reason)
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+second.Id+"/void", map[string]any{})
	x.Equal(http.StatusConflict, code, "void already")
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+first.Id+"/void", map[string]any{})
	x.Equal(http.StatusConflict, code, "paid")

	var shown admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/invoices/"+first.Id, nil, &shown)
	x.Len(shown.Lines, 1)
	x.Equal(int64(5000), shown.Lines[0].Total)
	x.Len(shown.CreditNotes, 1)

	// On a channel that collects by itself nothing is recorded by hand.
	ch.Push = true
	d.w.At(jan.AddDate(0, 2, 0))
	_, err := levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: wa.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 1, 20)), Value: 200, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)
	x.NoError(d.w.S.Invoices.Run(context.Background()))
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices?billing_account="+first.BillingAccount, nil, &vs)
	x.Len(vs, 2)
	x.Equal("paid", vs[0].State, "February's, the latest first: collected, after the credit took 700")
	x.Equal(int64(700), vs[0].Credited)
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/invoices/"+vs[0].Id+"/paid", map[string]any{"amount": 1, "reference": "x"})
	x.Equal(http.StatusConflict, code)
	n, err := d.w.S.Ent.PaymentAttempt.Query().Where(entattempt.Recorded(false)).Count(context.Background())
	x.NoError(err)
	x.Equal(1, n, "February's was sent")
}

// What a statement of a product would come to nothing for, for want of a
// price on its channel and currency while its meter is priced on another, is
// what an operator is told before a period closes: of the product's own
// terms and of every billing account's. A meter priced nowhere is not sold,
// and is not said.
func TestAnOperatorIsToldWhatIsPricedElsewhereAndNotHere(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "UTC", levyan.Channel_CHANNEL_PG_KR, "KRW")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Meter(loft, "loft.members", "{member}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_PG_KR, "KRW", 100, 50, jan)

	d.w.At(jan.AddDate(0, 0, 10))
	var gs []admin.Unpriced
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/unpriced", nil, &gs)
	x.Empty(gs, "priced on the product's own terms, and nobody is billed otherwise")

	// A billing account in Korea, on the merchant of record in won, as the
	// deployment sells Korea by default: assets are priced there on nothing.
	_, err := d.w.S.Payers.Create(context.Background(), levyantest.Account(), levyan.BillingAccount_builder{Country: "KR"}.Build())
	x.NoError(err)
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/unpriced", nil, &gs)
	x.Len(gs, 1)
	x.Equal(admin.Unpriced{Meter: "loft.assets.active", Channel: "mor", Currency: "KRW", Priced: []string{"pg_kr KRW"}}, gs[0])
}

// An operator sees what the shadow billing gathered before collection on the
// merchant's channel began, closed as a shadow -- by its state, and shown as
// any invoice is -- which is never collected: no attempt of it, and nothing
// left of it to collect.
func TestAnOperatorSeesAShadow(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_MOR, "KRW", 100, 50, jan)
	key, _ := d.w.Key(loft, "loft-prod", nil)
	d.w.S.Invoices.Collectors[levyan.Channel_CHANNEL_MOR] = invoicestest.New()
	d.w.S.Invoices.LiveFrom = map[levyan.Channel]time.Time{levyan.Channel_CHANNEL_MOR: jan.AddDate(0, 2, 0)}
	conn := d.w.Serve()
	product := levyantest.As(context.Background(), key)

	d.w.At(jan)
	a, ws := levyantest.Account(), levyantest.Tenant()
	_, err := d.w.S.Payers.Create(context.Background(), a, levyan.BillingAccount_builder{Country: "KR", Recipient: "수민"}.Build())
	x.NoError(err)
	_, err = levyanv1.NewWorkspaceServiceClient(conn).Arrive(product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: a.String()}.Build())
	x.NoError(err)
	d.w.At(jan.AddDate(0, 1, 0))
	_, err = levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 0, 20)), Value: 200, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)
	x.NoError(d.w.S.Invoices.Run(context.Background()))

	var vs []admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices?state=shadow", nil, &vs)
	x.Len(vs, 1)
	x.Equal("shadow", vs[0].State)
	x.Equal(int64(5000), vs[0].Subtotal, "what it would have come to")
	x.Zero(vs[0].Remaining, "and nothing of it to collect")
	x.Nil(vs[0].Finalized)
	x.NotNil(vs[0].Settled)
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices", nil, &vs)
	x.Len(vs, 1, "among the rest")

	var got admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/invoices/"+vs[0].Id, nil, &got)
	x.Equal("shadow", got.State)
	x.Len(got.Lines, 1)
	x.Empty(got.Attempts, "never collected")
}

// An operator sees what is done of an invoice whose charge failed (#11): the
// kind of its decline, when it failed, when it is charged next and when its
// grace ends and what it bills is restricted -- and, given up on, the
// invoice uncollectible with no retry left.
func TestAnOperatorSeesWhatIsDoneOfAnInvoiceWhoseChargeFailed(t *testing.T) {
	d := open(t, map[string]string{"alice": aliceKey})
	x := d.w.X
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	d.w.At(jan.Add(-time.Hour))

	loft := d.w.Terms(d.w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	assets := d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_LAST)
	d.w.Price(loft, assets, levyan.Channel_CHANNEL_MOR, "KRW", 100, 50, jan)
	key, _ := d.w.Key(loft, "loft-prod", nil)
	ch := invoicestest.New()
	ch.Then(invoicestest.Decline)
	d.w.S.Invoices.Collectors[levyan.Channel_CHANNEL_MOR] = ch
	conn := d.w.Serve()
	product := levyantest.As(context.Background(), key)

	d.w.At(jan)
	a, ws := levyantest.Account(), levyantest.Tenant()
	_, err := d.w.S.Payers.Create(context.Background(), a, levyan.BillingAccount_builder{Country: "KR", Recipient: "수민"}.Build())
	x.NoError(err)
	_, err = levyanv1.NewWorkspaceServiceClient(conn).Arrive(product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: a.String()}.Build())
	x.NoError(err)
	failed := jan.AddDate(0, 1, 0)
	d.w.At(failed)
	_, err = levyanv1.NewMeteringServiceClient(conn).Record(product, levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(jan.AddDate(0, 0, 20)), Value: 200, Unit: "{asset}"}.Build(),
	}}.Build())
	x.NoError(err)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/periods/close", map[string]any{}, nil)
	x.NoError(d.w.S.Invoices.Run(context.Background()))

	var vs []admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/invoices", nil, &vs)
	x.Len(vs, 1)
	var got admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/invoices/"+vs[0].Id, nil, &got)
	x.Equal("open", got.State)
	x.NotNil(got.Dunning)
	x.Equal("temporary", got.Dunning.Kind)
	x.True(failed.Equal(got.Dunning.Failed))
	x.NotNil(got.Dunning.NextRetry)
	x.True(failed.AddDate(0, 0, 1).Equal(*got.Dunning.NextRetry), "a day after")
	x.True(failed.AddDate(0, 0, 7).Equal(got.Dunning.GraceEnds))
	x.True(failed.AddDate(0, 0, 7).Equal(got.Dunning.RestrictedFrom))

	for _, days := range []int{1, 3, 5, 7} {
		ch.Then(invoicestest.Decline)
		d.w.At(failed.AddDate(0, 0, days))
		x.NoError(d.w.S.Invoices.Dun(context.Background(), &d.w.S.Grants))
	}
	var spent admin.Invoice
	d.as(aliceKey, http.MethodGet, "/admin/invoices/"+vs[0].Id, nil, &spent)
	x.Equal("uncollectible", spent.State)
	x.Len(spent.Attempts, 4)
	x.NotNil(spent.Dunning, "still owed")
	x.Nil(spent.Dunning.NextRetry, "no retry left")
}
