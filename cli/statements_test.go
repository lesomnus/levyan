package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/benefits"
	"github.com/lesomnus/levyan/server/invoices/invoicestest"
)

// What a payer's billing accounts paid for, as a product's user reads it
// through the product (#47): the statements of the product's workspaces, each
// of a period one of the account's billing accounts paid for, line by line --
// what levyan's operator gave included -- as the product reads them itself.
// A period somebody else paid for is theirs, another account's are nothing of
// its, and another product's are not this one's to show.

// billed is loft on the merchant of record in won, active assets at 50 won
// each past 100, a channel to collect its invoices on, and another product.
type billed struct {
	w        *levyantest.World
	x        *pdtest.X
	loft     pdid.Id
	key      string
	otherKey string
	conn     *grpc.ClientConn
	product  context.Context
}

func statements(t *testing.T) *billed {
	t.Helper()
	w := levyantest.New(t)
	x := pdtest.NewX(t)
	w.At(month(1, 1).Add(-24 * time.Hour))

	loft := w.Terms(w.Product("loft"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	key, _ := w.Key(loft, "loft-prod", nil)
	assets := w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE)
	w.Price(loft, assets, levyan.Channel_CHANNEL_MOR, "KRW", 100, 50, month(1, 1))
	other := w.Terms(w.Product("other"), "UTC", levyan.Channel_CHANNEL_MOR, "KRW")
	otherKey, _ := w.Key(other, "other-prod", nil)
	w.S.Invoices.Collectors[levyan.Channel_CHANNEL_MOR] = invoicestest.New()

	id, err := pdid.From(loft.GetId())
	x.NoError(err)

	return &billed{w: w, x: x, loft: id, key: key, otherKey: otherKey, conn: w.Serve(), product: levyantest.As(context.Background(), key)}
}

// month is a day of 2027 at midnight UTC.
func month(m, d int) time.Time { return time.Date(2027, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func (b *billed) as(a pdid.Id) context.Context {
	return levyantest.AsAccount(context.Background(), b.key, a)
}

func (b *billed) payers() levyanv1.PayerServiceClient { return levyanv1.NewPayerServiceClient(b.conn) }

func (b *billed) workspaces() levyanv1.WorkspaceServiceClient {
	return levyanv1.NewWorkspaceServiceClient(b.conn)
}

// account is a billing account of `a`'s in Korea: on the merchant of record,
// in won.
func (b *billed) account(a pdid.Id, name string) string {
	b.x.TB().Helper()
	v, err := b.payers().CreateBillingAccount(b.as(a), levyanv1.CreateBillingAccountRequest_builder{
		Name: name, Country: "KR", Recipient: levyanv1.Recipient_builder{Name: "수민"}.Build(),
	}.Build())
	b.x.NoError(err)
	return v.GetBillingAccount().GetId()
}

func (b *billed) arrive(ws, creator pdid.Id, label string) {
	b.x.TB().Helper()
	_, err := b.workspaces().Arrive(b.product, levyanv1.ArriveRequest_builder{
		Workspace: ws.String(), Creator: creator.String(), Created: timestamppb.New(month(1, 1)), Label: label,
	}.Build())
	b.x.NoError(err)
}

// held is what `ws` holds of active assets each midnight of [from, to): `v`
// of them, said now.
func (b *billed) held(ws pdid.Id, v int64, from, to time.Time) {
	b.x.TB().Helper()
	rs := []*levyanv1.Reading{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		rs = append(rs, levyanv1.Reading_builder{Workspace: ws.String(), Meter: "loft.assets.active", MeasuredAt: timestamppb.New(d), Value: v, Unit: "{asset}"}.Build())
	}
	_, err := levyanv1.NewMeteringServiceClient(b.conn).Record(b.product, levyanv1.RecordRequest_builder{Readings: rs}.Build())
	b.x.NoError(err)
}

// closed is what has ended by `through` closed then, and invoiced.
func (b *billed) closed(through time.Time) {
	b.x.TB().Helper()
	b.w.At(through)
	p, err := b.w.S.Ent.Tenant.Get(context.Background(), b.loft.Uuid())
	b.x.NoError(err)
	_, err = b.w.S.Closer.Close(context.Background(), p, through)
	b.x.NoError(err)
	b.x.NoError(b.w.S.Invoices.Run(context.Background()))
}

// mine is every statement `ctx`'s account reads through `ctx`'s product, of
// `billing` or of every billing account, a page of `size` at a time.
func (b *billed) mine(ctx context.Context, billing string, size int32) []*levyanv1.Statement {
	b.x.TB().Helper()
	out := []*levyanv1.Statement{}
	token := ""
	for {
		res, err := b.payers().ListStatements(ctx, levyanv1.PayerServiceListStatementsRequest_builder{
			BillingAccount: billing, PageSize: size, PageToken: token,
		}.Build())
		b.x.NoError(err)
		b.x.LessOrEqual(len(res.GetStatements()), int(size))
		out = append(out, res.GetStatements()...)
		if token = res.GetNextPageToken(); token == "" {
			return out
		}
	}
}

// periods is the period each statement is of, by its workspace.
func periods(ss []*levyanv1.Statement) map[string][]time.Time {
	out := map[string][]time.Time{}
	for _, s := range ss {
		out[s.GetWorkspace()] = append(out[s.GetWorkspace()], s.GetPeriod().GetStart().AsTime())
	}
	return out
}

// A payer reads the statements of the workspaces it pays for, through the
// product: each with its lines as the product reads them -- what levyan's
// operator gave, and what a pool took, among them -- the latest period first,
// a page at a time, of one of its billing accounts or of all of them; and
// what each line of its invoices is, by the statement it names.
func TestAPayerReadsTheStatementsItsBillingAccountsPaidFor(t *testing.T) {
	b := statements(t)
	x := b.x

	me := levyantest.Account()
	first := b.account(me, "회사용")
	b.w.At(month(1, 1))
	design, sales := levyantest.Tenant(), levyantest.Tenant()
	b.arrive(design, me, "디자인팀")
	b.arrive(sales, me, "영업팀")

	// What levyan's operator gave the design team: 500 more assets each
	// period, and a pool of 150 asset-months.
	op := b.w.Operator(context.Background(), "alice")
	allowance, err := b.w.S.Allowances.Give(op, benefits.Give{Product: b.loft, Workspace: design, Meter: "loft.assets.active", Quantity: 500, Reason: "봄 프로모션"})
	x.NoError(err)
	pool, err := b.w.S.Pools.Grant(op, benefits.Fill{Product: b.loft, Workspace: design, Meter: "loft.assets.active", Quantity: 150, Reason: "자산·월 프로모션"})
	x.NoError(err)

	// And the sales team paid for with another of the payer's billing
	// accounts from February.
	second := b.account(me, "개인용")
	b.w.At(month(1, 20))
	_, err = b.payers().Use(b.as(me), levyanv1.UseRequest_builder{Workspace: sales.String(), BillingAccount: second}.Build())
	x.NoError(err)

	b.w.At(month(3, 10))
	b.held(design, 800, month(1, 1), month(3, 1))
	b.held(sales, 300, month(1, 1), month(3, 1))
	b.closed(month(3, 10))

	all := b.mine(b.as(me), "", 3)
	x.Len(all, 4, "two workspaces, two periods, a page of three and one more")
	x.Equal(map[string][]time.Time{
		design.String(): {month(2, 1), month(1, 1)},
		sales.String():  {month(2, 1), month(1, 1)},
	}, periods(all), "the latest period first")
	x.Equal(map[string][]time.Time{
		design.String(): {month(2, 1), month(1, 1)},
		sales.String():  {month(1, 1)},
	}, periods(b.mine(b.as(me), first, 20)), "of the first billing account")
	x.Equal(map[string][]time.Time{
		sales.String(): {month(2, 1)},
	}, periods(b.mine(b.as(me), second, 20)), "of the second")

	// January of the design team: 800 held, 100 included by the price and
	// 500 by the allowance, and of the 200 billable past them the pool took
	// 150: 50 at 50 won.
	var jan *levyanv1.Statement
	for _, s := range all {
		if s.GetWorkspace() == design.String() && s.GetPeriod().GetStart().AsTime().Equal(month(1, 1)) {
			jan = s
		}
	}
	x.NotNil(jan)
	x.Equal(levyanv1.Channel_CHANNEL_MOR, jan.GetChannel())
	x.Equal("KRW", jan.GetCurrency())
	x.Equal("UTC", jan.GetPeriod().GetZone())
	x.Equal(int64(2500), jan.GetTotal())
	x.Len(jan.GetLines(), 1)
	l := jan.GetLines()[0]
	x.Equal("loft.assets.active", l.GetMeter())
	x.Equal(levyanv1.LineKind_LINE_KIND_USAGE, l.GetKind())
	x.Equal("800", l.GetUsage())
	x.Equal("600", l.GetIncluded())
	x.Equal("500", l.GetAllowance())
	x.Equal("200", l.GetBillable())
	x.Equal("150", l.GetPooled())
	x.Len(l.GetPools(), 1)
	x.Equal(pdid.Id(pool.Id).String(), l.GetPools()[0].GetPool())
	x.Equal("150", l.GetPools()[0].GetTaken())
	x.Equal("2500", l.GetAmount())
	x.Len(l.GetParts(), 1)
	x.Equal([]string{pdid.Id(allowance.Id).String()}, l.GetParts()[0].GetAllowances())

	// One of them, as the product reads it.
	got, err := b.payers().GetStatement(b.as(me), levyanv1.PayerServiceGetStatementRequest_builder{Id: jan.GetId()}.Build())
	x.NoError(err)
	theirs, err := levyanv1.NewBillingServiceClient(b.conn).GetStatement(b.product, levyanv1.GetStatementRequest_builder{Id: jan.GetId()}.Build())
	x.NoError(err)
	x.True(proto.Equal(theirs.GetStatement(), got.GetStatement()), "as the product reads it")
	x.True(proto.Equal(jan, got.GetStatement()), "as the list says it")

	// Every line of the payer's invoices is a statement it reads.
	invs, err := b.payers().ListInvoices(b.as(me), &levyanv1.ListInvoicesRequest{})
	x.NoError(err)
	x.Len(invs.GetInvoices(), 3, "the first billing account's two periods, and the second's February")
	lines := 0
	for _, inv := range invs.GetInvoices() {
		v, err := b.payers().GetInvoice(b.as(me), levyanv1.GetInvoiceRequest_builder{Id: inv.GetId()}.Build())
		x.NoError(err)
		for _, il := range v.GetInvoice().GetLines() {
			s, err := b.payers().GetStatement(b.as(me), levyanv1.PayerServiceGetStatementRequest_builder{Id: il.GetStatement()}.Build())
			x.NoError(err)
			x.Equal(il.GetWorkspace(), s.GetStatement().GetWorkspace())
			x.Equal(il.GetTotal(), s.GetStatement().GetTotal())
			x.True(il.GetPeriod().GetStart().AsTime().Equal(s.GetStatement().GetPeriod().GetStart().AsTime()))
			lines++
		}
	}
	x.Equal(4, lines)

	_, err = b.payers().GetStatement(b.as(me), levyanv1.PayerServiceGetStatementRequest_builder{Id: "nothing"}.Build())
	x.ErrCode(codes.InvalidArgument, err)
	_, err = b.payers().ListStatements(b.as(me), levyanv1.PayerServiceListStatementsRequest_builder{BillingAccount: "nothing"}.Build())
	x.ErrCode(codes.InvalidArgument, err)
}

// A payer changed at a period's end leaves each period to its own: the
// workspace's creator reads January, which it paid for, and the one who
// accepted in January reads February -- each the other's not at all, though
// the product reads both. What came of January late is corrected on
// February's statement, and is its payer's, who is billed for it.
func TestAPayerChangedReadsItsOwnPeriodsAlone(t *testing.T) {
	b := statements(t)
	x := b.x

	creator, next := levyantest.Account(), levyantest.Account()
	b.account(creator, "회사용")
	nextBilling := b.account(next, "본사")
	b.w.At(month(1, 1))
	ws := levyantest.Tenant()
	b.arrive(ws, creator, "디자인팀")

	b.w.At(month(1, 15))
	o, err := b.workspaces().Offer(b.product, levyanv1.OfferRequest_builder{Workspace: ws.String(), Account: next.String()}.Build())
	x.NoError(err)
	_, err = b.payers().Accept(b.as(next), levyanv1.AcceptRequest_builder{Offer: o.GetOffer().GetId(), BillingAccount: nextBilling}.Build())
	x.NoError(err)

	// January closed with its first twenty days heard of, and the rest of it
	// heard of late, with February.
	b.w.At(month(2, 10))
	b.held(ws, 300, month(1, 1), month(1, 21))
	b.closed(month(2, 10))
	b.w.At(month(3, 10))
	b.held(ws, 300, month(1, 21), month(3, 1))
	b.closed(month(3, 10))

	all, err := levyanv1.NewBillingServiceClient(b.conn).ListStatements(b.product, levyanv1.ListStatementsRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Len(all.GetStatements(), 2, "the product reads both")
	feb, jan := all.GetStatements()[0], all.GetStatements()[1]
	x.True(jan.GetPeriod().GetStart().AsTime().Equal(month(1, 1)))
	x.True(feb.GetPeriod().GetStart().AsTime().Equal(month(2, 1)))

	x.Equal(map[string][]time.Time{ws.String(): {month(1, 1)}}, periods(b.mine(b.as(creator), "", 20)), "the creator paid for January")
	x.Equal(map[string][]time.Time{ws.String(): {month(2, 1)}}, periods(b.mine(b.as(next), "", 20)), "and the one who accepted, from February")

	// February is the next payer's, and January was the creator's.
	_, err = b.payers().GetStatement(b.as(creator), levyanv1.PayerServiceGetStatementRequest_builder{Id: feb.GetId()}.Build())
	x.ErrCode(codes.NotFound, err)
	_, err = b.payers().GetStatement(b.as(next), levyanv1.PayerServiceGetStatementRequest_builder{Id: jan.GetId()}.Build())
	x.ErrCode(codes.NotFound, err)
	got, err := b.payers().GetStatement(b.as(next), levyanv1.PayerServiceGetStatementRequest_builder{Id: feb.GetId()}.Build())
	x.NoError(err)
	x.True(proto.Equal(feb, got.GetStatement()))

	// What January came to late is a line of February's, which bills it.
	corrects := []time.Time{}
	for _, l := range got.GetStatement().GetLines() {
		if l.GetKind() == levyanv1.LineKind_LINE_KIND_CORRECTION {
			corrects = append(corrects, l.GetCorrects().GetStart().AsTime())
		}
	}
	x.Equal([]time.Time{month(1, 1)}, corrects)
}

// Another account reads none of a payer's statements -- not through its own
// billing accounts, and not by naming the payer's, which is no billing
// account of its -- and through another product the payer reads none of
// this product's. A product reads none of them as itself, and the
// generated methods are nobody's.
func TestNobodyElseReadsAPayersStatements(t *testing.T) {
	b := statements(t)
	x := b.x

	me, stranger := levyantest.Account(), levyantest.Account()
	mine := b.account(me, "회사용")
	theirs := b.account(stranger, "회사용")
	b.w.At(month(1, 1))
	ws := levyantest.Tenant()
	b.arrive(ws, me, "디자인팀")
	b.w.At(month(3, 10))
	b.held(ws, 300, month(1, 1), month(3, 1))
	b.closed(month(3, 10))
	ss := b.mine(b.as(me), "", 20)
	x.Len(ss, 2)
	id := ss[0].GetId()

	x.Empty(b.mine(b.as(stranger), "", 20))
	x.Empty(b.mine(b.as(stranger), theirs, 20), "its own billing account paid for nothing")
	// Another's billing account is none of its.
	_, err := b.payers().ListStatements(b.as(stranger), levyanv1.PayerServiceListStatementsRequest_builder{BillingAccount: mine}.Build())
	x.ErrCode(codes.NotFound, err)
	_, err = b.payers().GetStatement(b.as(stranger), levyanv1.PayerServiceGetStatementRequest_builder{Id: id}.Build())
	x.ErrCode(codes.NotFound, err)

	// Through another product, the payer's own billing account paid for
	// nothing of that product's.
	viaOther := levyantest.AsAccount(context.Background(), b.otherKey, me)
	x.Empty(b.mine(viaOther, "", 20), "loft's are not another product's to show")
	x.Empty(b.mine(viaOther, mine, 20))
	_, err = b.payers().GetStatement(viaOther, levyanv1.PayerServiceGetStatementRequest_builder{Id: id}.Build())
	x.ErrCode(codes.NotFound, err)

	// A product calls none of it as itself, and the generated methods are
	// nobody's.
	_, err = b.payers().ListStatements(b.product, &levyanv1.PayerServiceListStatementsRequest{})
	x.ErrCode(codes.PermissionDenied, err)
	_, err = b.payers().GetStatement(b.product, levyanv1.PayerServiceGetStatementRequest_builder{Id: id}.Build())
	x.ErrCode(codes.PermissionDenied, err)
	_, err = levyan.NewStatementServiceClient(b.conn).List(b.as(me), levyan.StatementListRequest_builder{}.Build())
	x.ErrCode(codes.PermissionDenied, err)
	_, err = levyan.NewStatementServiceClient(b.conn).List(b.product, levyan.StatementListRequest_builder{}.Build())
	x.ErrCode(codes.PermissionDenied, err)
}
