package merchant_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/merchant"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// recorder is a merchant of record that keeps what it is asked, and answers
// what a test tells it to.
type recorder struct {
	mor.Channel

	charges  []mor.Charge
	asked    []mor.Charge
	refunds  []mor.Refund
	refunded []mor.Refund

	charged     map[string]mor.Transaction // what Charged answers, by subscription
	transaction mor.Transaction
}

func (r *recorder) Name() string { return "paddle" }
func (r *recorder) Minimum(currency string) money.Amount {
	return money.Amount{Minor: 980, Currency: currency}
}

func (r *recorder) Charge(_ context.Context, c mor.Charge) (mor.Transaction, error) {
	r.charges = append(r.charges, c)
	return mor.Transaction{ID: "txn_00000000000000000000000001", Status: mor.Paid, InvoiceNumber: "325-00001",
		Lines: []mor.Line{{ID: "txnitm_00000000000000000000000001", Key: c.Key, Amount: c.Amount}}}, nil
}

func (r *recorder) Charged(_ context.Context, c mor.Charge) (mor.Transaction, error) {
	r.asked = append(r.asked, c)
	if t, ok := r.charged[c.Subscription]; ok {
		return t, nil
	}
	return mor.Transaction{}, &mor.Error{Op: "charge", Kind: mor.NotFound}
}

func (r *recorder) Transaction(context.Context, string) (mor.Transaction, error) {
	return r.transaction, nil
}

func (r *recorder) Refund(_ context.Context, f mor.Refund) (mor.Adjustment, error) {
	r.refunds = append(r.refunds, f)
	return mor.Adjustment{ID: "adj_00000000000000000000000001", Kind: mor.RefundAdjustment, Status: mor.Requested, Key: f.Key}, nil
}

func (r *recorder) Refunded(_ context.Context, f mor.Refund) (mor.Adjustment, error) {
	r.refunded = append(r.refunded, f)
	return mor.Adjustment{ID: "adj_00000000000000000000000001", Kind: mor.RefundAdjustment, Status: mor.Approved, Key: f.Key}, nil
}

func (r *recorder) Webhook(http.Header, []byte) (mor.Notice, error) { return mor.Notice{}, nil }

// The merchant is handed each charge and each refund as the ledger wrote it
// down: the attempt's key, now, for what is due, as the invoice's
// description, and the send before it as when it was last sent -- which is
// what holds the MoR to sending nothing within `mor.Settle` of a send whose
// answer never came. An ask is told when the attempt was last sent, of every
// subscription the billing account has had; a refund gives back of the line
// of the charge, all of it when it is all that was charged.
func TestTheMerchantIsHandedWhatTheLedgerWroteDown(t *testing.T) {
	w := levyantest.New(t)
	x := w.X
	ctx := context.Background()
	rec := &recorder{charged: map[string]mor.Transaction{}}

	account := levyantest.Account()
	v, err := w.S.Payers.Create(ctx, account, levyan.BillingAccount_builder{Name: "회사용", Country: "KR"}.Build())
	x.NoError(err)
	ba, err := pdid.From(v.GetId())
	x.NoError(err)
	subs := subscriptions.Subscriptions{S: w.S.Ungated, Drv: w.S.Drv, Dialect: w.S.Dialect, Channel: rec}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i, id := range []string{"sub_00000000000000000000000001", "sub_00000000000000000000000002"} {
		st := mor.Canceled
		if i == 1 {
			st = mor.Active
		}
		_, err := subs.Observe(ctx, ba.Uuid(), mor.Subscription{ID: id, Customer: "ctm_00000000000000000000000001", Account: ba.String(),
			Status: st, Currency: "KRW", Next: now.Add(24 * time.Hour), Updated: now}, nil)
		x.NoError(err)
	}

	c := &merchant.Collector{Channel: rec, Db: w.S.Ent}
	x.Equal(int64(980), c.Minimum("KRW"))
	x.True(c.Pushes())
	x.True(c.Due(now).IsZero())
	_, err = c.Issue(ctx, invoices.Document{})
	x.Error(err, "the merchant issues its own")

	sent := now.Add(-time.Minute)
	a := invoices.Attempt{
		Key: "lv-0001", Kind: levyan.AttemptKind_ATTEMPT_KIND_CHARGE, Amount: krw(5000),
		Invoice: invoices.Invoice{Account: ba, Channel: morCh, Description: "loft 2027-01"},
		Sends:   1, Sent: sent,
	}
	o, err := c.Send(ctx, invoices.Send{Attempt: a, N: 2, Key: "lv-0001-2", Previous: sent})
	x.NoError(err)
	x.Equal(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, o.State)
	x.Equal("txn_00000000000000000000000001", o.Ref)
	x.Equal("325-00001", o.Number)
	x.Equal([]mor.Charge{{
		Subscription: "sub_00000000000000000000000002", Key: "lv-0001", When: mor.Now,
		Amount: krw(5000), Description: "loft 2027-01", Sent: sent,
	}}, rec.charges, "the live subscription, under the attempt's key, told when it was last sent")

	_, err = c.Ask(ctx, a)
	x.Equal(invoices.NotFound, invoices.KindOf(err), "found in none of them")
	x.Len(rec.asked, 2, "asked of every subscription the billing account has had")
	for _, q := range rec.asked {
		x.Equal("lv-0001", q.Key)
		x.True(sent.Equal(q.Sent), "told when it was last sent")
	}
	rec.charged["sub_00000000000000000000000001"] = mor.Transaction{ID: "txn_00000000000000000000000009", Status: mor.Paid}
	o, err = c.Ask(ctx, a)
	x.NoError(err)
	x.Equal("txn_00000000000000000000000009", o.Ref, "found in the one canceled since")

	rec.transaction = mor.Transaction{ID: "txn_00000000000000000000000001", Lines: []mor.Line{
		{ID: "txnitm_00000000000000000000000000"},
		{ID: "txnitm_00000000000000000000000001", Key: "lv-0001", Amount: krw(5000)},
	}}
	r := invoices.Attempt{Key: "lv-0002", Kind: levyan.AttemptKind_ATTEMPT_KIND_REFUND, Amount: krw(2000),
		Invoice: a.Invoice, Refunds: "txn_00000000000000000000000001", Reason: "환불", Sent: sent}
	o, err = c.Send(ctx, invoices.Send{Attempt: r, N: 1, Key: "lv-0002-1", Previous: sent})
	x.NoError(err)
	x.Equal(levyan.AttemptState_ATTEMPT_STATE_PENDING, o.State, "waiting for the merchant's approval")
	r.Amount = krw(5000)
	_, err = c.Send(ctx, invoices.Send{Attempt: r, N: 1, Key: "lv-0002-1", Previous: sent})
	x.NoError(err)
	x.Equal([]mor.Refund{
		{Transaction: "txn_00000000000000000000000001", Line: "txnitm_00000000000000000000000001", Amount: krw(2000), Reason: "환불", Key: "lv-0002", Sent: sent},
		{Transaction: "txn_00000000000000000000000001", Line: "txnitm_00000000000000000000000001", All: true, Reason: "환불", Key: "lv-0002", Sent: sent},
	}, rec.refunds, "of the charge's line: an amount of it, or all of it")
	o, err = c.Ask(ctx, r)
	x.NoError(err)
	x.Equal(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED, o.State, "approved")
	x.Equal([]mor.Refund{{Transaction: "txn_00000000000000000000000001", Key: "lv-0002", Sent: sent}}, rec.refunded)
}
