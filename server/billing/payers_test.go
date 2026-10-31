package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
)

// Who pays (#4), as what a period comes to: a statement on its payer's
// billing account's channel and currency, a payer changed at a period's end
// billing each period to its own, and each payer's first few workspaces
// having what their prices include.

// abroad is setup's loft with its active assets priced on the merchant of
// record too, in cents: one each past 100.
func abroad(t *testing.T) world {
	w := setup(t)
	assets, err := w.S.Ent.Meter.Query().All(context.Background())
	w.X.NoError(err)
	for _, m := range assets {
		if m.Key != "loft.assets.active" {
			continue
		}
		_, err := w.S.Ungated.Price().Add(context.Background(), levyan.PriceAddRequest_builder{
			Tenant:        levyantest.Ref(w.loft),
			Meter:         levyan.MeterRef_builder{Id: m.Id[:]}.Build(),
			Channel:       levyan.Channel_CHANNEL_MOR,
			Currency:      "USD",
			Included:      100,
			UnitPrice:     1,
			DateEffective: timestamppb.New(on(1, 1)),
		}.Build())
		w.X.NoError(err)
	}
	return w
}

func (w world) product() pdid.Id {
	p, err := pdid.From(w.loft.GetId())
	w.X.NoError(err)
	return p
}

// billing is a billing account of `owner`'s in `country`.
func (w world) billing(owner pdid.Id, country string) pdid.Id {
	w.t.Helper()
	v, err := w.S.Payers.Create(context.Background(), owner, levyan.BillingAccount_builder{Country: country, Recipient: "수민"}.Build())
	w.X.NoError(err)
	id, err := pdid.From(v.GetId())
	w.X.NoError(err)
	return id
}

func (w world) arrive(ws, creator pdid.Id, at time.Time) {
	w.t.Helper()
	_, err := w.S.Payers.Arrive(context.Background(), w.product(), ws, creator, at)
	w.X.NoError(err)
}

// row is the statement of workspace `ws` and the period beginning at
// `start`, as levyan keeps it.
func (w world) row(ws pdid.Id, start time.Time) *ent.Statement {
	w.t.Helper()
	v, err := w.S.Ent.Statement.Query().
		Where(entstatement.HasWorkspaceWith(entworkspace.Key(ws.Bytes())), entstatement.DateStart(start)).
		Only(context.Background())
	w.X.NoError(err)
	return v
}

// A statement is priced on the channel and in the currency of the billing
// account its workspace's payer paid with: a Korean one on the gateway in
// won, one abroad on the merchant of record in cents -- and on the product's
// own for a payer with none, or no payer.
func TestAStatementIsPricedOnItsPayersBillingAccount(t *testing.T) {
	w := abroad(t)
	x := w.X

	kr, us, none := levyantest.Account(), levyantest.Account(), levyantest.Account()
	krb, usb := w.billing(kr, "KR"), w.billing(us, "US")
	a, b, c, d := levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()

	w.At(on(1, 1))
	w.arrive(a, kr, on(1, 1))
	w.arrive(b, us, on(1, 1))
	w.arrive(c, none, on(1, 1))

	w.At(on(2, 10))
	for _, ws := range []pdid.Id{a, b, c, d} {
		w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 200)
	}
	w.close(on(2, 10))

	sa := w.statement(a, on(1, 1))
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, sa.GetChannel())
	x.Equal("KRW", sa.GetCurrency())
	x.Equal(int64(5000), sa.GetTotal(), "100 past what is included, at 50 won")
	x.Equal(kr.Uuid(), *w.row(a, on(1, 1)).Payer)
	x.Equal(krb.Uuid(), w.row(a, on(1, 1)).BillingAccountId)

	sb := w.statement(b, on(1, 1))
	x.Equal(levyanv1.Channel_CHANNEL_MOR, sb.GetChannel())
	x.Equal("USD", sb.GetCurrency())
	x.Equal(int64(100), sb.GetTotal(), "100 past what is included, at a cent")
	x.Equal(usb.Uuid(), w.row(b, on(1, 1)).BillingAccountId)

	for _, ws := range []pdid.Id{c, d} {
		s := w.statement(ws, on(1, 1))
		x.Equal(levyanv1.Channel_CHANNEL_PG_KR, s.GetChannel(), "the product's")
		x.Equal("KRW", s.GetCurrency())
		x.Equal(int64(5000), s.GetTotal())
	}
	x.Equal(none.Uuid(), *w.row(c, on(1, 1)).Payer)
	x.Nil(w.row(d, on(1, 1)).Payer, "nobody paid for it")
}

// A payer changed at a period's end bills each period to its own: January
// to the one who accepted nothing, on its gateway in won; February to the
// one who accepted in January, abroad.
func TestAPayerChangedBillsEachPeriodToItsOwn(t *testing.T) {
	w := abroad(t)
	x := w.X

	kr, us := levyantest.Account(), levyantest.Account()
	w.billing(kr, "KR")
	usb := w.billing(us, "US")
	ws := levyantest.Tenant()

	w.At(on(1, 1))
	w.arrive(ws, kr, on(1, 1))
	w.At(on(1, 15))
	o, err := w.S.Payers.Offer(context.Background(), w.product(), ws, us)
	x.NoError(err)
	oid, err := pdid.From(o.GetId())
	x.NoError(err)
	_, err = w.S.Payers.Accept(context.Background(), w.product(), us, oid, usb)
	x.NoError(err)

	w.At(on(3, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(3, 1), 200)
	w.close(on(3, 10))

	jan, feb := w.statement(ws, on(1, 1)), w.statement(ws, on(2, 1))
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, jan.GetChannel())
	x.Equal(int64(5000), jan.GetTotal())
	x.Equal(kr.Uuid(), *w.row(ws, on(1, 1)).Payer)
	x.Equal(levyanv1.Channel_CHANNEL_MOR, feb.GetChannel())
	x.Equal("USD", feb.GetCurrency())
	x.Equal(int64(100), feb.GetTotal())
	x.Equal(us.Uuid(), *w.row(ws, on(2, 1)).Payer)
}

// Each payer's first three workspaces of a product have what their prices
// include, and the others are rated from the first unit -- counted by when
// the payer began paying for each, and per period: a workspace counts once,
// under the one payer of its period, however many own it, and one the payer
// stops paying for leaves room for the next.
func TestEachPayersFirstWorkspacesHaveTheirAllowances(t *testing.T) {
	w := setup(t)
	x := w.X

	owner, coOwner := levyantest.Account(), levyantest.Account()
	ws := []pdid.Id{levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant(), levyantest.Tenant()}
	shared := levyantest.Tenant()

	w.At(on(1, 2))
	for i, v := range ws {
		w.arrive(v, owner, on(1, 1).Add(time.Duration(i+1)*time.Hour))
	}
	// Both own it, and the co-owner pays: it is the co-owner's first.
	w.arrive(shared, coOwner, on(1, 1))

	// The owner stops paying for the first, from February.
	w.At(on(1, 20))
	_, err := w.S.Payers.Stop(context.Background(), w.product(), owner, ws[0])
	x.NoError(err)

	w.At(on(3, 10))
	for _, v := range append(ws, shared) {
		w.daily(v, "loft.assets.active", "{asset}", on(1, 1), on(3, 1), 50)
	}
	w.close(on(3, 10))

	for i, v := range ws[:3] {
		s := w.statement(v, on(1, 1))
		x.Equal(int64(0), s.GetTotal(), "the %d. is free", i+1)
		x.False(w.row(v, on(1, 1)).FirstUnit)
	}
	fourth := w.statement(ws[3], on(1, 1))
	x.Equal(int64(2500), fourth.GetTotal(), "the fourth is rated from the first unit: 50 at 50")
	x.Equal("0", fourth.GetLines()[0].GetIncluded())
	x.True(w.row(ws[3], on(1, 1)).FirstUnit)
	x.Equal(int64(0), w.statement(shared, on(1, 1)).GetTotal(), "the co-owner's first")

	// In February the owner pays for three: the fourth is among them.
	x.Equal(int64(0), w.statement(ws[3], on(2, 1)).GetTotal())
	x.False(w.row(ws[3], on(2, 1)).FirstUnit)
	x.Nil(w.row(ws[0], on(2, 1)).Payer, "and nobody pays for the first")
	x.Equal(int64(0), w.statement(ws[0], on(2, 1)).GetTotal(), "whose allowances are its own: when in doubt, less")

	// What the period now open would come to is worked out the same way.
	w.At(on(3, 10))
	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: ws[3].String()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, u.GetUsage().GetChannel())
}
