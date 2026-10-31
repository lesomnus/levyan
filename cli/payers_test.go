package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"github.com/lesomnus/roster/standing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	entaudit "github.com/lesomnus/levyan/internal/ent/audit"
	"github.com/lesomnus/levyan/internal/levyantest"
)

// Who pays, as products and their users call it (#4): a product says who
// made a workspace and asks an account to pay, and the account -- relayed by
// the product, as itself -- answers, keeps its billing accounts, and stops.

type paying struct {
	w       *levyantest.World
	conn    *grpc.ClientConn
	loft    *levyan.Tenant
	key     string
	product context.Context
}

func paid(t *testing.T) *paying {
	t.Helper()
	w := levyantest.New(t)
	w.At(time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC))
	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)

	return &paying{w: w, conn: w.Serve(), loft: loft, key: key, product: levyantest.As(context.Background(), key)}
}

func (p *paying) as(a pdid.Id) context.Context {
	return levyantest.AsAccount(context.Background(), p.key, a)
}

func (p *paying) workspaces() levyanv1.WorkspaceServiceClient {
	return levyanv1.NewWorkspaceServiceClient(p.conn)
}

func (p *paying) payers() levyanv1.PayerServiceClient { return levyanv1.NewPayerServiceClient(p.conn) }

// A product says a workspace was made and by whom, asks another account to
// pay for it, and the account answers as itself through the product: it
// makes a billing account, sees the offer, accepts it, and pays from the end
// of the period -- which the product reads back.
func TestAProductsUserAnswersAsItself(t *testing.T) {
	p := paid(t)
	x := pdtest.NewX(t)

	creator, other := levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()

	got, err := p.workspaces().Arrive(p.product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: creator.String()}.Build())
	x.NoError(err)
	x.Equal(creator.String(), got.GetPayer().GetCurrent().GetAccount())
	x.Empty(got.GetPayer().GetCurrent().GetBillingAccount(), "none made yet")

	o, err := p.workspaces().Offer(p.product, levyanv1.OfferRequest_builder{Workspace: ws.String(), Account: other.String()}.Build())
	x.NoError(err)

	ba, err := p.payers().CreateBillingAccount(p.as(other), levyanv1.CreateBillingAccountRequest_builder{
		Name: "회사용", Country: "kr",
		Recipient: levyanv1.Recipient_builder{Name: "수민", Email: "billing@example.com"}.Build(),
		Business:  levyanv1.Business_builder{Registration: "123-45-67890", Name: "예시"}.Build(),
	}.Build())
	x.NoError(err)
	x.Equal("KR", ba.GetBillingAccount().GetCountry())
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, ba.GetBillingAccount().GetChannel())
	x.Equal("KRW", ba.GetBillingAccount().GetCurrency())

	// What it wrote of its own, the trail says it wrote.
	id, err := pdid.Parse(ba.GetBillingAccount().GetId())
	x.NoError(err)
	row, err := p.w.S.Ent.Audit.Query().Where(entaudit.ObjectId(id.Uuid())).First(context.Background())
	x.NoError(err)
	x.Equal(other.Uuid(), row.ActorId)

	upd, err := p.payers().UpdateBillingAccount(p.as(other), levyanv1.UpdateBillingAccountRequest_builder{
		Id: ba.GetBillingAccount().GetId(), Name: ptr("본사"), ClearBusiness: true,
	}.Build())
	x.NoError(err)
	x.Equal("본사", upd.GetBillingAccount().GetName())
	x.False(upd.GetBillingAccount().HasBusiness())

	list, err := p.payers().ListBillingAccounts(p.as(other), &levyanv1.ListBillingAccountsRequest{})
	x.NoError(err)
	x.Len(list.GetBillingAccounts(), 1)
	list, err = p.payers().ListBillingAccounts(p.as(creator), &levyanv1.ListBillingAccountsRequest{})
	x.NoError(err)
	x.Empty(list.GetBillingAccounts(), "another's is nothing of theirs")
	_, err = p.payers().UpdateBillingAccount(p.as(creator), levyanv1.UpdateBillingAccountRequest_builder{Id: ba.GetBillingAccount().GetId(), Name: ptr("내 것")}.Build())
	x.Equal(codes.NotFound, status.Code(err))

	offers, err := p.payers().ListOffers(p.as(other), &levyanv1.ListOffersRequest{})
	x.NoError(err)
	x.Len(offers.GetOffers(), 1)
	x.Equal(ws.String(), offers.GetOffers()[0].GetWorkspace())
	none, err := p.payers().ListOffers(p.as(creator), &levyanv1.ListOffersRequest{})
	x.NoError(err)
	x.Empty(none.GetOffers())

	_, err = p.payers().Accept(p.as(creator), levyanv1.AcceptRequest_builder{Offer: o.GetOffer().GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err), "made to another")
	acc, err := p.payers().Accept(p.as(other), levyanv1.AcceptRequest_builder{Offer: o.GetOffer().GetId(), BillingAccount: ba.GetBillingAccount().GetId()}.Build())
	x.NoError(err)
	x.Equal(ws.String(), acc.GetLink().GetWorkspace())
	x.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), acc.GetLink().GetStart().AsTime())

	got2, err := p.workspaces().GetPayer(p.product, levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(creator.String(), got2.GetPayer().GetCurrent().GetAccount(), "until the end of the period")
	x.Equal(other.String(), got2.GetPayer().GetNext().GetAccount())
	x.Equal(ba.GetBillingAccount().GetId(), got2.GetPayer().GetNext().GetBillingAccount())

	links, err := p.payers().ListLinks(p.as(other), &levyanv1.ListLinksRequest{})
	x.NoError(err)
	x.Len(links.GetLinks(), 1)

	stop, err := p.payers().Stop(p.as(other), levyanv1.StopRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(acc.GetLink().GetId(), stop.GetLink().GetId())

	p.w.At(time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC))
	left, err := p.workspaces().Leave(p.product, levyanv1.LeaveRequest_builder{Workspace: ws.String(), Account: creator.String()}.Build())
	x.NoError(err)
	x.NotNil(left.GetPayer().GetPayerlessSince(), "nobody pays for it, now or next")
	x.NotNil(left.GetPayer().GetRestrictedFrom())
}

// An account is served only as roster says it may be now: refused when
// roster says it is not in good standing, told to come back when roster
// cannot be asked, and refused everything while no roster is configured --
// never served on nobody's word.
func TestAnAccountIsHeldToRostersWordOnIt(t *testing.T) {
	w := levyantest.New(t)
	x := pdtest.NewX(t)
	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)
	roster := &levyantest.Standing{}
	w.S.Standing = roster
	payers := levyanv1.NewPayerServiceClient(w.Serve())

	good, suspended, away := levyantest.Account(), levyantest.Account(), levyantest.Account()
	roster.Refuse(suspended, standing.ErrRefused)
	roster.Refuse(away, standing.ErrUnreachable)

	ask := func(a pdid.Id) error {
		_, err := payers.ListBillingAccounts(levyantest.AsAccount(context.Background(), key, a), &levyanv1.ListBillingAccountsRequest{})
		return err
	}
	x.NoError(ask(good))
	x.Equal(codes.PermissionDenied, status.Code(ask(suspended)))
	x.Equal(codes.Unavailable, status.Code(ask(away)))

	// No roster at all.
	none := levyantest.New(t)
	p := none.Product("loft")
	k, _ := none.Key(p, "loft-prod", nil)
	none.S.Standing = nil
	_, err := levyanv1.NewPayerServiceClient(none.Serve()).ListBillingAccounts(levyantest.AsAccount(context.Background(), k, good), &levyanv1.ListBillingAccountsRequest{})
	x.Equal(codes.FailedPrecondition, status.Code(err))
}

// An account relayed by a product calls what an account calls and nothing
// else -- not the product's methods, not the schema's -- and a product with
// no account beside its key calls none of an account's.
func TestAnAccountCallsAnAccountsMethodsAlone(t *testing.T) {
	p := paid(t)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ws := levyantest.Tenant()

	_, err := p.workspaces().GetPayer(p.as(a), levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "the product's to call")
	_, err = levyanv1.NewPriceBookServiceClient(p.conn).GetProduct(p.as(a), &levyanv1.GetProductRequest{})
	x.Equal(codes.PermissionDenied, status.Code(err))
	_, err = levyan.NewBillingAccountServiceClient(p.conn).List(p.as(a), levyan.BillingAccountListRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "the schema's own are nobody's")
	_, err = levyan.NewLinkServiceClient(p.conn).Get(p.product, levyan.LinkGetRequest_builder{}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err))

	_, err = p.payers().ListBillingAccounts(p.product, &levyanv1.ListBillingAccountsRequest{})
	x.Equal(codes.PermissionDenied, status.Code(err), "a product is no account")

	_, err = p.payers().ListBillingAccounts(levyantest.AsAccount(context.Background(), "lvk_nothing", a), &levyanv1.ListBillingAccountsRequest{})
	x.Equal(codes.Unauthenticated, status.Code(err), "a key that is none vouches for nobody")
}

// One product's payers are not another's: what one offered is no offer
// through the other, and a workspace of the same identifier in both is two.
// A billing account is its account's in every product.
func TestOneProductsPayersAreNotAnothers(t *testing.T) {
	p := paid(t)
	x := pdtest.NewX(t)
	other := p.w.Product("other")
	otherKey, _ := p.w.Key(other, "other-prod", nil)
	viaOther := func(a pdid.Id) context.Context { return levyantest.AsAccount(context.Background(), otherKey, a) }

	a, b := levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()
	_, err := p.workspaces().Arrive(p.product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: a.String()}.Build())
	x.NoError(err)
	o, err := p.workspaces().Offer(p.product, levyanv1.OfferRequest_builder{Workspace: ws.String(), Account: b.String()}.Build())
	x.NoError(err)

	offers, err := p.payers().ListOffers(viaOther(b), &levyanv1.ListOffersRequest{})
	x.NoError(err)
	x.Empty(offers.GetOffers(), "made by another product")
	_, err = p.payers().Accept(viaOther(b), levyanv1.AcceptRequest_builder{Offer: o.GetOffer().GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err))
	links, err := p.payers().ListLinks(viaOther(a), &levyanv1.ListLinksRequest{})
	x.NoError(err)
	x.Empty(links.GetLinks())

	there, err := p.workspaces().GetPayer(levyantest.As(context.Background(), otherKey), levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Nil(there.GetPayer().GetCurrent(), "the other's workspace of that identifier is its own")

	ba, err := p.payers().CreateBillingAccount(viaOther(a), levyanv1.CreateBillingAccountRequest_builder{Country: "US"}.Build())
	x.NoError(err)
	mine, err := p.payers().ListBillingAccounts(p.as(a), &levyanv1.ListBillingAccountsRequest{})
	x.NoError(err)
	x.Len(mine.GetBillingAccounts(), 1, "the account's, whichever product it was made through")
	x.Equal(ba.GetBillingAccount().GetId(), mine.GetBillingAccounts()[0].GetId())
}

func ptr[T any](v T) *T { return &v }
