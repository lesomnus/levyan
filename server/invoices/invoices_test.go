package invoices_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entcreditnote "github.com/lesomnus/levyan/internal/ent/creditnote"
	entinvoice "github.com/lesomnus/levyan/internal/ent/invoice"
	entinvoiceline "github.com/lesomnus/levyan/internal/ent/invoiceline"
	entattempt "github.com/lesomnus/levyan/internal/ent/paymentattempt"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
)

var (
	mor   = levyan.Channel_CHANNEL_MOR
	pgkr  = levyan.Channel_CHANNEL_PG_KR
	stock = levyan.MeterKind_METER_KIND_STOCK
	twa   = levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE

	draft     = int32(levyan.InvoiceState_INVOICE_STATE_DRAFT)
	open      = int32(levyan.InvoiceState_INVOICE_STATE_OPEN)
	paid      = int32(levyan.InvoiceState_INVOICE_STATE_PAID)
	void      = int32(levyan.InvoiceState_INVOICE_STATE_VOID)
	writtenOf = int32(levyan.InvoiceState_INVOICE_STATE_WRITTEN_OFF)
)

// on is a day of 2027 at midnight UTC.
func on(month, d int) time.Time { return time.Date(2027, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

type world struct {
	*levyantest.World
	t       *testing.T
	ctx     context.Context
	loft    *levyan.Tenant
	product pdid.Id
	key     string
	conn    *grpc.ClientConn
	as      context.Context
	ch      *invoicestest.Channel
}

// setup is loft, sold on `channel` in won from 2027 -- active assets at 50
// won each past 100 -- with a channel of the test's collecting on it.
func setup(t *testing.T, channel levyan.Channel, with ...func(*cmd.Config)) world {
	w := levyantest.New(t, with...)
	w.At(on(1, 1).Add(-24 * time.Hour))

	loft := w.Product("loft")
	loft = w.Terms(loft, "UTC", channel, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", stock, twa)
	w.Price(loft, assets, channel, "KRW", 100, 50, on(1, 1))
	p, err := pdid.From(loft.GetId())
	w.X.NoError(err)

	ch := invoicestest.New()
	w.S.Invoices.Collectors[channel] = ch

	return world{
		World: w, t: t, ctx: context.Background(), loft: loft, product: p, key: key,
		conn: w.Serve(), as: levyantest.As(context.Background(), key), ch: ch,
	}
}

// payer is an account of roster's with a billing account in Korea, the
// workspaces it made from New Year's day on, paid for with it.
func (w world) payer(ws ...pdid.Id) (pdid.Id, pdid.Id) {
	w.t.Helper()
	account := levyantest.Account()
	v, err := w.S.Payers.Create(w.ctx, account, levyan.BillingAccount_builder{
		Name: "회사용", Country: "KR", Recipient: "수민", Email: "billing@example.com",
	}.Build())
	w.X.NoError(err)
	ba, err := pdid.From(v.GetId())
	w.X.NoError(err)
	if len(ws) > 0 && w.S.Clock().Before(on(1, 1)) {
		w.At(on(1, 1))
	}
	for _, v := range ws {
		_, err := w.S.Payers.Arrive(w.ctx, w.product, v, account, on(1, 1))
		w.X.NoError(err)
	}

	return account, ba
}

// daily records a reading of active assets for `ws` at each midnight of
// [from, to), of `v`.
func (w world) daily(ws pdid.Id, from, to time.Time, v int64) {
	w.t.Helper()
	rs := []*levyanv1.Reading{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(d), Value: v, Unit: "{asset}"}.Build())
	}
	w.record(rs...)
}

func (w world) record(rs ...*levyanv1.Reading) {
	w.t.Helper()
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(w.as, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	w.X.NoError(err)
	for _, r := range res.GetResults() {
		w.X.Equal(levyanv1.Outcome_OUTCOME_KEPT, r.GetOutcome(), r.GetWhy())
	}
}

// close closes what has ended by `through`, as an operator's hand does.
func (w world) close(through time.Time) {
	w.t.Helper()
	p, err := w.S.Ent.Tenant.Get(w.ctx, w.product.Uuid())
	w.X.NoError(err)
	_, err = w.S.Closer.Close(w.ctx, p, through)
	w.X.NoError(err)
}

// run is a pass of the loop `levyan.invoices.collect`.
func (w world) run() {
	w.t.Helper()
	w.X.NoError(w.S.Invoices.Run(w.ctx))
}

// invoices are billing account `ba`'s, in the order of their periods.
func (w world) invoices(ba pdid.Id) []*ent.Invoice {
	w.t.Helper()
	vs, err := w.S.Ent.Invoice.Query().
		Where(entinvoice.BillingAccountId(ba.Uuid())).
		Order(ent.Asc(entinvoice.FieldDateStart), ent.Asc(entinvoice.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	return vs
}

func (w world) only(ba pdid.Id) *ent.Invoice {
	w.t.Helper()
	vs := w.invoices(ba)
	w.X.Len(vs, 1)
	return vs[0]
}

func (w world) lines(inv *ent.Invoice) []*ent.InvoiceLine {
	w.t.Helper()
	vs, err := w.S.Ent.InvoiceLine.Query().Where(entinvoiceline.InvoiceId(inv.Id)).All(w.ctx)
	w.X.NoError(err)
	return vs
}

func (w world) attempts(inv *ent.Invoice) []*ent.PaymentAttempt {
	w.t.Helper()
	vs, err := w.S.Ent.PaymentAttempt.Query().
		Where(entattempt.InvoiceId(inv.Id)).
		Order(ent.Asc(entattempt.FieldDateCreated), ent.Asc(entattempt.FieldId)).
		All(w.ctx)
	w.X.NoError(err)
	return vs
}

func (w world) again(inv *ent.Invoice) *ent.Invoice {
	w.t.Helper()
	v, err := w.S.Ent.Invoice.Get(w.ctx, inv.Id)
	w.X.NoError(err)
	return v
}

func (w world) balance(ba pdid.Id) int64 {
	w.t.Helper()
	v, err := invoices.Balance(w.ctx, w.S.Ent, w.product.Uuid(), ba.Uuid())
	w.X.NoError(err)
	return v
}

func id(v *ent.Invoice) pdid.Id { return pdid.Id(v.Id) }

// Every statement a period's close writes with a billing account is on one
// invoice of that billing account -- the one of the period -- and on no other,
// however often the loop runs, and wherever; a statement of a workspace
// nobody paid for, or whose payer has no billing account, is on none.
func TestEveryStatementWithABillingAccountIsOnOneInvoice(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a1, a2, b1 := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()
	none, nobody := levyantest.Tenant(), levyantest.Tenant()
	_, a := w.payer(a1, a2)
	_, b := w.payer(b1)
	_, err := w.S.Payers.Arrive(w.ctx, w.product, none, levyantest.Account(), on(1, 1))
	x.NoError(err, "its creator has no billing account")

	w.At(on(2, 10))
	for _, ws := range []pdid.Id{a1, a2, b1, none, nobody} {
		w.daily(ws, on(1, 1), on(2, 1), 200)
	}
	w.close(on(2, 10))

	// Twice, and twice at once: what is on an invoice is on it once.
	w.run()
	w.run()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _ = w.S.Invoices.Run(w.ctx) })
	}
	wg.Wait()

	ia := w.only(a)
	x.Len(w.lines(ia), 2)
	x.Equal(int64(10000), ia.Subtotal, "two workspaces, each 100 past what is included, at 50")
	x.Equal(paid, ia.State, "collected")
	x.True(ia.DateStart.Equal(on(1, 1)))
	x.True(ia.DateEnd.Equal(on(2, 1)))
	ib := w.only(b)
	x.Len(w.lines(ib), 1)
	x.Equal(int64(5000), ib.Subtotal)

	// The invariant, read off the rows: every statement with a billing
	// account is on one line of an invoice of it, and the others on none.
	sts, err := w.S.Ent.Statement.Query().All(w.ctx)
	x.NoError(err)
	x.Len(sts, 5)
	for _, st := range sts {
		ls, err := w.S.Ent.InvoiceLine.Query().Where(entinvoiceline.StatementId(st.Id)).WithInvoice().All(w.ctx)
		x.NoError(err)
		if st.BillingAccountId == (uuid.UUID{}) {
			x.Empty(ls, "nobody to bill")
			continue
		}
		x.Len(ls, 1)
		x.Equal(st.BillingAccountId, ls[0].Edges.Invoice.BillingAccountId)
		x.Equal(st.Total, ls[0].Total)
	}
	n, sum := w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
	x.Equal(2, n, "each invoice charged once")
	x.Equal(int64(15000), sum)

	// The next period is an invoice of its own.
	w.At(on(3, 10))
	for _, ws := range []pdid.Id{a1, a2, b1} {
		w.daily(ws, on(2, 1), on(3, 1), 200)
	}
	w.close(on(3, 10))
	w.run()
	vs := w.invoices(a)
	x.Len(vs, 2)
	x.True(vs[1].DateStart.Equal(on(2, 1)))
	x.Len(w.lines(vs[1]), 2)
}

// A product that closes several periods at once -- a deployment back after
// a while -- still has an invoice of each: a statement of a later period
// waits until its draft's own period is decided.
func TestPeriodsClosedTogetherAreInvoicedApart(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(4, 10))
	w.daily(ws, on(1, 1), on(4, 1), 200)
	w.close(on(4, 10))
	w.run()

	vs := w.invoices(ba)
	x.Len(vs, 3)
	for i, v := range vs {
		x.True(v.DateStart.Equal(on(i+1, 1)))
		x.Len(w.lines(v), 1)
		x.Equal(paid, v.State)
	}
}

// Scenario 5: a charge whose answer never came is asked about, and never
// sent again blind. One the channel made is found, and the invoice is paid
// with nothing sent again; one it never had is sent again as a send of its
// own, under the same key and the send's own.
func TestAnUnknownChargeIsAskedAboutBeforeAnythingIsSentAgain(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	lost, dropped := levyantest.Tenant(), levyantest.Tenant()
	_, a := w.payer(lost)
	_, b := w.payer(dropped)
	w.At(on(2, 10))
	w.daily(lost, on(1, 1), on(2, 1), 200)
	w.daily(dropped, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))

	// The first billing account's charge is made and its answer lost; the
	// second's never arrives.
	w.ch.Then(invoicestest.Lost, invoicestest.Dropped)
	w.run()

	ia, ib := w.only(a), w.only(b)
	x.Equal(open, ia.State)
	x.Equal(open, ib.State)
	for _, inv := range []*ent.Invoice{ia, ib} {
		as := w.attempts(inv)
		x.Len(as, 1)
		x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN), as[0].State)
		x.Equal(uint32(1), as[0].Sends)
	}
	x.Len(w.ch.Sends(), 2)

	// Asked: the first is found paid, and nothing is sent for it again. The
	// second is not there, and waits to be sent again.
	w.At(on(2, 10).Add(time.Hour))
	w.run()
	ia, ib = w.again(ia), w.again(ib)
	x.Equal(paid, ia.State)
	x.Equal(int64(5000), ia.Paid)
	x.NotEmpty(ia.ChannelRef)
	x.NotEmpty(ia.ChannelNumber, "the merchant of record's invoice number")
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), w.attempts(ia)[0].State)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_REQUESTED), w.attempts(ib)[0].State)
	x.Len(w.ch.Sends(), 2, "nothing sent blind")

	w.At(on(2, 10).Add(2 * time.Hour))
	w.run()
	ib = w.again(ib)
	x.Equal(paid, ib.State)
	as := w.attempts(ib)
	x.Len(as, 1, "the same attempt")
	x.Equal(uint32(2), as[0].Sends)

	sends := w.ch.Sends()
	x.Len(sends, 3)
	first, again := sends[1], sends[2]
	x.Equal(first.Attempt.Key, again.Attempt.Key, "the same order")
	x.Equal(invoices.SendKey(as[0].AttemptKey, 1), first.Key)
	x.Equal(invoices.SendKey(as[0].AttemptKey, 2), again.Key, "a key of its own")
	x.True(first.Previous.IsZero())
	x.True(again.Previous.Equal(first.Attempt.Sent), "told when the send before it was")

	n, _ := w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
	x.Equal(2, n, "each charged once")
}

// Each send is written down and committed before it is made: what the
// deployment would find had it gone down as the send went out is an attempt
// out, whose outcome is to be asked about.
func TestASendIsWrittenDownBeforeItIsMade(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))

	seen := 0
	w.ch.Before = func(s invoices.Send) {
		seen++
		v, err := w.S.Ent.PaymentAttempt.Query().Where(entattempt.AttemptKey(s.Attempt.Key)).Only(context.Background())
		x.NoError(err)
		x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_UNKNOWN), v.State, "out, as far as anybody can tell")
		x.Equal(uint32(s.N), v.Sends)
		x.NotNil(v.DateSent)
	}
	w.ch.Then(invoicestest.Busy)
	w.run()
	x.Equal(1, seen)

	// The channel did nothing of it for now: it is sent again, as a send of
	// its own, and written down first again.
	w.At(on(2, 10).Add(time.Hour))
	w.run()
	x.Equal(2, seen)
	x.Equal(invoices.SendKey(w.ch.Sends()[0].Attempt.Key, 2), w.ch.Sends()[1].Key)
}

// A decline ends the attempt and leaves the invoice open, past due: trying
// again is dunning's (#11), and the loop sends nothing more of it.
func TestADeclineEndsTheAttemptAndLeavesTheInvoiceOpen(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))

	w.ch.Then(invoicestest.Decline)
	w.run()
	inv := w.only(ba)
	x.Equal(open, inv.State)
	as := w.attempts(inv)
	x.Len(as, 1)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_FAILED), as[0].State)
	x.Equal(levyan.FailureKind_FAILURE_KIND_DECLINED, as[0].Failure.GetKind())
	x.Equal(levyan.DeclineKind_DECLINE_KIND_TEMPORARY, as[0].Failure.GetDecline())
	x.NotNil(as[0].DateResolved)

	w.At(on(2, 11))
	w.run()
	x.Len(w.attempts(inv), 1, "nothing tried again here")
	x.Len(w.ch.Sends(), 1)
}

// A billing account's amount that is more than nothing and less than its
// channel collects is not collected: its statements wait, and the next
// period's invoice is of both (the owner's, 2026-10-31).
func TestAnAmountLessThanTheChannelCollectsWaitsForTheNextPeriod(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Least = map[string]int64{"KRW": 6000}

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	w.run()

	inv := w.only(ba)
	x.Equal(draft, inv.State, "5,000 is less than 6,000")
	x.Equal(uint32(1), inv.Carried, "once, however often it is looked at")
	x.Empty(w.ch.Sends())

	w.At(on(3, 10))
	w.daily(ws, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()

	inv = w.only(ba)
	x.Equal(paid, inv.State)
	x.Len(w.lines(inv), 2, "January's and February's")
	x.True(inv.DateStart.Equal(on(1, 1)))
	x.True(inv.DateEnd.Equal(on(3, 1)))
	x.Equal(int64(10000), inv.Due)
	_, sum := w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
	x.Equal(int64(10000), sum)
}

// A small amount to which nothing more will come -- every link of its
// billing account ended -- is written off, as a state of its own, rather
// than collected or dropped (Claude's proposal, which the owner has not
// confirmed).
func TestASmallAmountNothingMoreComesToIsWrittenOff(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Least = map[string]int64{"KRW": 6000}

	ws := levyantest.Tenant()
	account, ba := w.payer(ws)
	w.At(on(1, 20))
	_, err := w.S.Payers.Stop(w.ctx, w.product, account, ws)
	x.NoError(err)

	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()

	inv := w.only(ba)
	x.Equal(writtenOf, inv.State)
	x.NotNil(inv.DateSettled)
	x.Len(w.lines(inv), 1, "its statement is on it still")
	x.Empty(w.ch.Sends())
}

// What is owed back -- a month whose corrections outweigh what it used -- is
// never paid out: it is the billing account's credit, taken off its next
// invoice, and the channel sees only what is left.
func TestWhatIsOwedBackIsCreditTakenOffTheNextInvoice(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(paid, jan.State)

	// Half a day of nothing that should have been in January: January held
	// 200 for twelve hours less, which is 5,000/31 less.
	w.At(on(2, 12))
	w.record(levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(on(1, 15).Add(12 * time.Hour)), Value: 0, Unit: "{asset}"}.Build())

	// February uses nothing past what is included.
	w.At(on(3, 10))
	w.close(on(3, 10))
	w.run()
	vs := w.invoices(ba)
	x.Len(vs, 2)
	feb := vs[1]
	x.Equal(int64(-162), feb.Subtotal, "-161.29..., rounded down")
	x.Equal(paid, feb.State, "nothing to collect")
	x.Zero(feb.Due)
	x.Equal(int64(162), w.balance(ba))

	// March: 5,000, of which the credit pays 162.
	w.At(on(4, 10))
	w.daily(ws, on(3, 1), on(4, 1), 200)
	w.close(on(4, 10))
	w.run()
	mar := w.invoices(ba)[2]
	x.Equal(int64(5000), mar.Total)
	x.Equal(int64(162), mar.Credited)
	x.Equal(int64(4838), mar.Due)
	x.Equal(paid, mar.State)
	x.Zero(w.balance(ba))
	sends := w.ch.Sends()
	x.Equal(int64(4838), sends[len(sends)-1].Attempt.Amount.Minor, "the channel sees what is left")
}

// Scenario 7: an invoice finalized is never changed. What the ledger finds
// late corrects the period on the next statement, and so the next invoice;
// what it does not correct by itself is a credit note against the invoice,
// which takes it back as the billing account's credit.
func TestAFinalizedInvoiceStaysWhatItWas(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	was := proto.Clone(jan.Proto())

	// Half a day of 400 that should have been in January, sent in February:
	// the correction is on February's statement, and its invoice.
	w.At(on(2, 12))
	w.record(levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(on(1, 15).Add(12 * time.Hour)), Value: 400, Unit: "{asset}"}.Build())
	w.At(on(3, 10))
	w.daily(ws, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	feb := w.invoices(ba)[1]
	x.Equal(int64(5161), feb.Subtotal, "5,000 and 5,000/31 for January")
	x.Equal(paid, feb.State)

	// A rating found wrong for January, which the ledger would not correct:
	// a credit note against January's invoice, credited.
	cn, err := w.S.Invoices.Credit(w.ctx, id(jan), invoices.Credit{
		Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 1000, Words: "계산 버전 1의 오류",
	})
	x.NoError(err)
	x.Equal(int64(1000), cn.Total)
	x.Zero(cn.Reduced, "nothing was due of it any more")
	x.Zero(cn.Refunded)
	x.Equal(int64(1000), cn.Credited)
	x.Equal(int64(1000), w.balance(ba))

	// January's invoice is what it was, but for the time it was last
	// written, which is nothing a credit note changes.
	now := w.again(jan).Proto()
	x.True(proto.Equal(was, now), "%v\n%v", was, now)
	x.Len(w.lines(jan), 1)

	// April's invoice takes it off.
	w.At(on(4, 10))
	w.daily(ws, on(3, 1), on(4, 1), 200)
	w.close(on(4, 10))
	w.run()
	mar := w.invoices(ba)[2]
	x.Equal(int64(1000), mar.Credited)
	x.Equal(int64(4000), mar.Due)
}

// Scenario 13: a refund gives back what was paid, through the channel, a part
// and then the rest -- and takes nothing back from the ledger: what was used
// stays what the statements said. More than was paid is not given back.
func TestARefundGivesBackWhatWasPaid(t *testing.T) {
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
	statements, err := w.S.Ent.Statement.Query().Where(entstatement.BillingAccountId(ba.Uuid())).All(w.ctx)
	x.NoError(err)

	refund := func(amount int64) *ent.CreditNote {
		cn, err := w.S.Invoices.Credit(w.ctx, id(inv), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: amount, Words: "부분 환불"})
		x.NoError(err)
		return cn
	}

	cn := refund(2000)
	x.Equal(int64(2000), cn.Refunded)
	x.Zero(cn.Credited)
	w.At(on(2, 11))
	w.run()
	n, sum := w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_REFUND)
	x.Equal(1, n)
	x.Equal(int64(2000), sum)

	// The rest: its refund's answer is lost, and it is asked about.
	w.ch.Then(invoicestest.Lost)
	cn = refund(3000)
	x.Equal(int64(3000), cn.Refunded)
	w.run()
	w.At(on(2, 11).Add(time.Hour))
	w.run()
	n, sum = w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_REFUND)
	x.Equal(2, n)
	x.Equal(int64(5000), sum)
	rs, err := w.S.Ent.PaymentAttempt.Query().Where(entattempt.InvoiceId(inv.Id), entattempt.Kind(int32(levyan.AttemptKind_ATTEMPT_KIND_REFUND))).All(w.ctx)
	x.NoError(err)
	x.Len(rs, 2)
	for _, r := range rs {
		x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), r.State)
	}
	x.Len(w.ch.Sends(), 3, "a charge and two refunds, each sent once")

	// Nothing is left to give back.
	_, err = w.S.Invoices.Credit(w.ctx, id(inv), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: 1})
	x.Equal(codes.InvalidArgument, status.Code(err))

	// The invoice is as it was paid, and the statements as they were.
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Paid)
	again, err := w.S.Ent.Statement.Query().Where(entstatement.BillingAccountId(ba.Uuid())).All(w.ctx)
	x.NoError(err)
	x.Equal(len(statements), len(again))
	for i := range statements {
		x.Equal(statements[i].Total, again[i].Total)
	}
}

// On a channel that moves no money itself, a transfer an operator saw is
// recorded: a part leaves the rest open, and what is more than was due is
// the billing account's credit. Nothing is recorded on a channel that
// collects by itself.
func TestATransferIsRecordedOnAChannelThatMovesNoMoney(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Push = false

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	inv := w.only(ba)
	x.Equal(open, inv.State)
	x.Empty(w.attempts(inv), "nothing sent")

	_, err := w.S.Invoices.Record(w.ctx, id(inv), 3000, on(2, 9), "수민 3000")
	x.NoError(err)
	inv = w.again(inv)
	x.Equal(open, inv.State)
	x.Equal(int64(2000), invoices.Remaining(inv))

	a, err := w.S.Invoices.Record(w.ctx, id(inv), 3000, on(2, 10), "수민 3000")
	x.NoError(err)
	x.True(a.Recorded)
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), a.State)
	inv = w.again(inv)
	x.Equal(paid, inv.State)
	x.Equal(int64(5000), inv.Paid)
	x.Equal(int64(1000), w.balance(ba), "paid past what was due")

	_, err = w.S.Invoices.Record(w.ctx, id(inv), 1, on(2, 10), "더")
	x.Equal(codes.FailedPrecondition, status.Code(err), "settled")

	w.ch.Push = true
	_, err = w.S.Invoices.Record(w.ctx, id(inv), 1, on(2, 10), "더")
	x.Equal(codes.FailedPrecondition, status.Code(err), "it collects by itself")
}

// A void takes back what is left of an open invoice with a credit note of its
// cancellation, and what the credit balance paid of it is credit again. An
// invoice paid in any part is not voided.
func TestAVoidTakesBackWhatIsLeft(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Push = false

	ws := levyantest.Tenant()
	_, ba := w.payer(ws)
	w.At(on(2, 10))
	w.daily(ws, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)

	// A credit of 1,000 on January, which February's takes.
	_, err := w.S.Invoices.Record(w.ctx, id(jan), 5000, on(2, 10), "수민")
	x.NoError(err)
	_, err = w.S.Invoices.Credit(w.ctx, id(jan), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 1000})
	x.NoError(err)
	x.Equal(int64(1000), w.balance(ba))

	w.At(on(3, 10))
	w.daily(ws, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	feb := w.invoices(ba)[1]
	x.Equal(open, feb.State)
	x.Equal(int64(1000), feb.Credited)
	x.Equal(int64(4000), feb.Due)
	x.Zero(w.balance(ba))

	_, err = w.S.Invoices.Void(w.ctx, id(jan), "")
	x.Equal(codes.FailedPrecondition, status.Code(err), "January was paid")

	got, err := w.S.Invoices.Void(w.ctx, id(feb), "계약 해지")
	x.NoError(err)
	x.Equal(void, got.State)
	x.Equal("계약 해지", got.Reason)
	cns, err := w.S.Ent.CreditNote.Query().Where(entcreditnote.InvoiceId(feb.Id)).All(w.ctx)
	x.NoError(err)
	x.Len(cns, 1)
	x.Equal(int32(levyan.CreditNoteReason_CREDIT_NOTE_REASON_CANCELLATION), cns[0].Reason)
	x.Equal(int64(5000), cns[0].Total)
	x.Equal(int64(4000), cns[0].Reduced)
	x.Equal(int64(1000), cns[0].Credited)
	x.Nil(cns[0].Number, "the merchant of record numbers what it sells")
	x.Equal(int64(1000), w.balance(ba), "what the credit paid of it is credit again")
	x.Zero(invoices.Remaining(got))

	_, err = w.S.Invoices.Void(w.ctx, id(feb), "")
	x.Equal(codes.FailedPrecondition, status.Code(err), "void already")
}

// On a channel levyan sells on itself its invoice is the legal one: numbered
// as it is finalized, one after another across the deployment, with the
// seller's tax apart -- a tenth, toward nothing to the won -- and its tax
// invoice issued once by its number. A credit note against it is numbered in
// a series of its own, with an amended document of the same amounts less
// than nothing; a void's too.
func TestASellersInvoiceIsNumberedAndIssued(t *testing.T) {
	w := setup(t, pgkr, func(c *cmd.Config) {
		c.Payers.Channels = cmd.ChannelsConfig{"KR": {Channel: "PG_KR", Currency: "KRW"}}
	})
	x := w.X

	a1, b1 := levyantest.Tenant(), levyantest.Tenant()
	_, a := w.payer(a1)
	_, b := w.payer(b1)
	w.At(on(2, 10))
	w.daily(a1, on(1, 1), on(2, 1), 200)
	w.daily(b1, on(1, 1), on(2, 1), 107)
	w.close(on(2, 10))
	w.run()
	w.run()

	ia, ib := w.only(a), w.only(b)
	numbers := []int64{*ia.Number, *ib.Number}
	x.ElementsMatch([]int64{1, 2}, numbers)
	x.Equal(int64(5000), ia.Subtotal)
	x.Equal(int64(500), ia.Tax)
	x.Equal("1/10", ia.TaxRate)
	x.Equal(int64(5500), ia.Total)
	x.Equal(int64(350), ib.Subtotal, "7 past what is included, at 50")
	x.Equal(int64(35), ib.Tax)
	x.Equal(paid, ia.State)
	_, sum := w.ch.Made(levyan.AttemptKind_ATTEMPT_KIND_CHARGE)
	x.Equal(int64(5500+385), sum, "with its tax")

	docs := w.ch.Documents()
	x.Len(docs, 2)
	x.Equal(2, w.ch.Issues(), "issued once each, however often the loop runs")
	d := docs["1"]
	x.Equal(int64(1), d.Number)
	x.False(d.CreditNote)
	x.True(d.Written.Equal(on(1, 31)), "the month's last day")
	x.Equal(ia.BillTo.GetRecipient(), d.BillTo.GetRecipient())
	x.NotNil(ia.DateIssued)
	x.NotEmpty(ia.Document)

	// A credit note of 1,005 of the subtotal: 100 of tax, a tenth of it
	// toward nothing, and given back, as a refund is.
	cn, err := w.S.Invoices.Credit(w.ctx, id(ia), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_REFUND, Amount: 1005})
	x.NoError(err)
	x.Equal(int64(100), cn.Tax)
	x.Equal(int64(1105), cn.Refunded)
	x.Equal(int64(1), *cn.Number, "the credit notes' own series")
	// The rest takes what is left of the tax, whatever a tenth of it is.
	cn, err = w.S.Invoices.Credit(w.ctx, id(ia), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 3995})
	x.NoError(err)
	x.Equal(int64(400), cn.Tax)
	x.Equal(int64(2), *cn.Number)
	w.run()
	docs = w.ch.Documents()
	x.Len(docs, 4)
	amended := docs["cn-1"]
	x.True(amended.CreditNote)
	x.Equal(int64(-1005), amended.Supply)
	x.Equal(int64(-100), amended.Tax)
	x.Equal(*ia.Number, amended.Amends)
	x.True(amended.Original.Equal(on(1, 31)), "the day its invoice was written for")
	x.Equal(ia.Document, amended.Document)
}

// A charge the channel has and has not finished -- collecting, waiting for
// the payer -- is asked about again until it is done; one the channel will
// not do as asked fails, with its code, and is not sent again.
func TestAPendingChargeIsAskedUntilItIsDoneAndARefusedOneFails(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	held, refused := levyantest.Tenant(), levyantest.Tenant()
	_, a := w.payer(held)
	_, b := w.payer(refused)
	w.At(on(2, 10))
	w.daily(held, on(1, 1), on(2, 1), 200)
	w.daily(refused, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))

	w.ch.Then(invoicestest.Hold, invoicestest.Refuse)
	w.run()
	ia, ib := w.only(a), w.only(b)
	pending := w.attempts(ia)[0]
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_PENDING), pending.State)
	x.NotEmpty(pending.ChannelRef)
	failed := w.attempts(ib)[0]
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_FAILED), failed.State)
	x.Equal(levyan.FailureKind_FAILURE_KIND_INVALID, failed.Failure.GetKind())
	x.Equal("REFUSED", failed.Failure.GetCode())
	x.Equal(open, w.again(ib).State)

	w.At(on(2, 10).Add(time.Hour))
	w.run()
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_PENDING), w.attempts(ia)[0].State, "asked, and still collecting")
	x.Len(w.ch.Asks(), 1)

	w.ch.Finish(pending.AttemptKey, true)
	w.At(on(2, 10).Add(2 * time.Hour))
	w.run()
	x.Equal(int32(levyan.AttemptState_ATTEMPT_STATE_SUCCEEDED), w.attempts(ia)[0].State)
	x.Equal(paid, w.again(ia).State)
	x.Len(w.ch.Sends(), 2, "each sent once")
}
