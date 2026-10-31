package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	entaudit "github.com/lesomnus/levyan/internal/ent/audit"
	entnotice "github.com/lesomnus/levyan/internal/ent/notice"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/internal/mor/paddle/paddletest"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// A billing account on the merchant of record's channel is collected through
// a subscription the payer starts in the merchant's checkout (#12): begun and
// completed as the account, relayed by a product, as #4's calls are.

type subscribing struct {
	*paying
	srv *paddletest.Server
}

func subscribed(t *testing.T, with ...func(*cmd.Config)) *subscribing {
	t.Helper()
	srv := paddletest.New(t)
	w := levyantest.New(t, append([]func(*cmd.Config){levyantest.Merchant(srv, true)}, with...)...)
	loft := w.Product("loft")
	key, _ := w.Key(loft, "loft-prod", nil)

	return &subscribing{
		paying: &paying{w: w, conn: w.Serve(), loft: loft, key: key, product: levyantest.As(context.Background(), key)},
		srv:    srv,
	}
}

// account is a billing account of `a`'s, made in `country`.
func (p *paying) account(t *testing.T, a pdid.Id, country string) *levyanv1.BillingAccount {
	t.Helper()
	ba, err := p.payers().CreateBillingAccount(p.as(a), levyanv1.CreateBillingAccountRequest_builder{
		Name: "회사용", Country: country,
		Recipient: levyanv1.Recipient_builder{Name: "수민", Email: "billing@example.com"}.Build(),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	return ba.GetBillingAccount()
}

const back = "https://loft.example.com/billing/subscribed"

// opened is the checkout a page opens from the window `w`, as the fake's
// payer finishes it.
func opened(w *levyanv1.CheckoutWindow) mor.Checkout {
	return mor.Checkout{ID: w.GetTransaction(), Window: mor.Window{Values: map[string]string{
		"token":         w.GetClientToken(),
		"environment":   w.GetEnvironment(),
		"transactionId": w.GetTransaction(),
	}}}
}

// A payer begins a checkout of their billing account, finishes it in the
// merchant's window, and comes back: the subscription is mirrored, the
// billing account can be collected from, and the product that relayed it is
// told so of the workspace it pays for -- and nothing of the card.
func TestAPayerSubscribesInTheMerchantsCheckout(t *testing.T) {
	p := subscribed(t)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := p.account(t, a, "KR")
	x.Equal(levyanv1.Channel_CHANNEL_MOR, ba.GetChannel())

	got, err := p.payers().GetBillingAccount(p.as(a), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION, got.GetCollection().GetState())
	x.Nil(got.GetSubscription())

	started, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.NoError(err)
	co := started.GetCheckout()
	x.Equal(levyanv1.CheckoutState_CHECKOUT_STATE_OPEN, co.GetState())
	x.Equal(ba.GetId(), co.GetBillingAccount())
	win := co.GetWindow()
	x.Equal("paddle", win.GetMerchant())
	x.Equal("sandbox", win.GetEnvironment())
	x.Equal(p.srv.ClientToken, win.GetClientToken())
	x.Contains(win.GetTransaction(), "txn_")
	x.Contains(win.GetUrl(), "_ptxn="+win.GetTransaction())
	x.Equal("https://cdn.paddle.com/paddle/v2/paddle.js", win.GetScript())
	x.Equal("KR", win.GetCountry(), "what a page prefills the checkout's address with")

	// What it began, the trail says the account began.
	id, err := pdid.Parse(co.GetId())
	x.NoError(err)
	row, err := p.w.S.Ent.Audit.Query().Where(entaudit.ObjectId(id.Uuid())).First(context.Background())
	x.NoError(err)
	x.Equal(a.Uuid(), row.ActorId)

	open, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: co.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CheckoutState_CHECKOUT_STATE_OPEN, open.GetCheckout().GetState(), "the payer has not finished it yet")
	x.Nil(open.GetSubscription())

	x.NoError(p.srv.Pay(opened(win)))
	done, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: co.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CheckoutState_CHECKOUT_STATE_COMPLETED, done.GetCheckout().GetState())
	x.NotNil(done.GetCheckout().GetDone())
	sub := done.GetSubscription()
	x.Equal(levyanv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE, sub.GetStatus())
	x.Equal("KRW", sub.GetCurrency(), "in the billing account's currency, by its plan")
	x.Equal("paddle", sub.GetMerchant())
	x.NotNil(sub.GetNextBilled())
	x.Equal("card", sub.GetMethod().GetType())
	x.Equal("visa", sub.GetMethod().GetBrand())
	x.Equal("4242", sub.GetMethod().GetLast4())
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_COLLECTABLE, done.GetCollection().GetState())

	again, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: co.GetId()}.Build())
	x.NoError(err)
	x.Equal(sub.GetId(), again.GetSubscription().GetId(), "completed twice is completed once")
	n, err := p.w.S.Ent.Subscription.Query().Count(context.Background())
	x.NoError(err)
	x.Equal(1, n)

	got, err = p.payers().GetBillingAccount(p.as(a), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_COLLECTABLE, got.GetCollection().GetState())
	x.Equal(sub.GetId(), got.GetSubscription().GetId())

	// The product reads it of the workspace the billing account pays for.
	ws := levyantest.Tenant()
	_, err = p.workspaces().Arrive(p.product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: a.String()}.Build())
	x.NoError(err)
	payer, err := p.workspaces().GetPayer(p.product, levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(ba.GetId(), payer.GetPayer().GetCurrent().GetBillingAccount())
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_COLLECTABLE, payer.GetPayer().GetCollection().GetState())

	// And once the subscription is scheduled to cancel, from when.
	p.srv.ScheduleCancel(p.mirrored(t, sub.GetId()))
	_, err = p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err), "subscribed until it ends")
	payer, err = p.workspaces().GetPayer(p.product, levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_COLLECTABLE, payer.GetPayer().GetCollection().GetState())
	x.NotNil(payer.GetPayer().GetCollection().GetEnds(), "asked again as the checkout was refused: canceled at the period's end")
}

// mirrored is the merchant's id of the subscription levyan mirrors as `id`.
func (p *subscribing) mirrored(t *testing.T, id string) string {
	t.Helper()
	v, err := pdid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.w.S.Ent.Subscription.Get(context.Background(), v.Uuid())
	if err != nil {
		t.Fatal(err)
	}
	return m.MorSubscription
}

// A payer who begins a checkout twice is handed the first while the
// merchant takes it -- one checkout, one subscription -- and another once it
// does not; one subscribed is refused another, until the subscription it has
// is canceled, by the payer at the merchant as well.
func TestASecondCheckoutIsTheFirstWhileItIsOpen(t *testing.T) {
	p := subscribed(t)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := p.account(t, a, "US")
	start := func() (*levyanv1.Checkout, error) {
		v, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
		return v.GetCheckout(), err
	}

	first, err := start()
	x.NoError(err)
	second, err := start()
	x.NoError(err)
	x.Equal(first.GetId(), second.GetId())
	x.Equal(first.GetWindow().GetTransaction(), second.GetWindow().GetTransaction())
	x.Equal(1, p.srv.Requests(paddletest.Checkout), "nothing begun at the merchant again")

	p.srv.Abandon(first.GetWindow().GetTransaction())
	third, err := start()
	x.NoError(err)
	x.NotEqual(first.GetId(), third.GetId(), "the merchant takes the first no longer")
	was, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: first.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CheckoutState_CHECKOUT_STATE_CANCELED, was.GetCheckout().GetState())

	x.NoError(p.srv.Pay(opened(third.GetWindow())))
	_, err = start()
	x.Equal(codes.FailedPrecondition, status.Code(err), "finished meanwhile: subscribed, and no other begun")
	done, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: third.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CheckoutState_CHECKOUT_STATE_COMPLETED, done.GetCheckout().GetState())
	x.Equal("USD", done.GetSubscription().GetCurrency())

	// The payer cancels it at the merchant, and levyan has heard nothing:
	// asked again, it is canceled, and another may begin.
	p.srv.CancelNow(p.mirrored(t, done.GetSubscription().GetId()))
	fourth, err := start()
	x.NoError(err)
	x.NotEqual(third.GetId(), fourth.GetId())
	got, err := p.payers().GetBillingAccount(p.as(a), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_CANCELED, got.GetCollection().GetState(), "until another is finished")
	x.Equal(levyanv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED, got.GetSubscription().GetStatus())
}

// A checkout is the account's own: nobody else begins one of its billing
// account, completes one of its, or reads its billing account; a product
// alone calls none of it; and another product's workspace of the same
// identifier is told nothing of it. A billing account is its account's
// through any product.
func TestACheckoutIsTheAccountsOwn(t *testing.T) {
	p := subscribed(t)
	x := pdtest.NewX(t)
	a, b := levyantest.Account(), levyantest.Account()
	ba := p.account(t, a, "KR")

	_, err := p.payers().StartCheckout(p.as(b), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.Equal(codes.NotFound, status.Code(err), "another's billing account is nothing of theirs")
	_, err = p.payers().GetBillingAccount(p.as(b), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err))
	_, err = p.payers().StartCheckout(p.product, levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.Equal(codes.PermissionDenied, status.Code(err), "a product is no account")

	other := p.w.Product("other")
	otherKey, _ := p.w.Key(other, "other-prod", nil)
	viaOther := levyantest.AsAccount(context.Background(), otherKey, a)
	started, err := p.payers().StartCheckout(viaOther, levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.NoError(err, "the account's, whichever product relays it")
	co := started.GetCheckout()

	_, err = p.payers().CompleteCheckout(p.as(b), levyanv1.CompleteCheckoutRequest_builder{Checkout: co.GetId()}.Build())
	x.Equal(codes.NotFound, status.Code(err), "another's checkout is nothing of theirs")
	x.NoError(p.srv.Pay(opened(co.GetWindow())))
	_, err = p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: co.GetId()}.Build())
	x.NoError(err)

	ws := levyantest.Tenant()
	_, err = p.workspaces().Arrive(p.product, levyanv1.ArriveRequest_builder{Workspace: ws.String(), Creator: a.String()}.Build())
	x.NoError(err)
	there, err := p.workspaces().GetPayer(levyantest.As(context.Background(), otherKey), levyanv1.GetPayerRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Nil(there.GetPayer().GetCollection(), "the other's workspace of that identifier is its own, and nobody pays for it")
}

// A billing account on another channel than the merchant of record's takes
// no checkout, and is said nothing of; and with no merchant configured, no
// checkout is begun of any, and nothing is received or pulled.
func TestACheckoutIsOnlyOfTheMerchantsChannel(t *testing.T) {
	p := subscribed(t, func(c *cmd.Config) {
		c.Payers.Channels = cmd.ChannelsConfig{"KR": {Channel: "PG_KR", Currency: "KRW"}}
	})
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := p.account(t, a, "KR")
	x.Equal(levyanv1.Channel_CHANNEL_PG_KR, ba.GetChannel())

	_, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err))
	got, err := p.payers().GetBillingAccount(p.as(a), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_UNSPECIFIED, got.GetCollection().GetState())

	none := paid(t)
	x.Nil(none.w.S.Inbox, "no merchant, no inbox")
	x.False(none.w.S.Subscriptions.Configured())
	for _, s := range none.w.S.Spin {
		if n, ok := s.(interface{ SpinName() string }); ok {
			x.NotContains(n.SpinName(), "levyan.mor.")
		}
	}
	b := levyantest.Account()
	bb := none.account(t, b, "KR")
	_, err = none.payers().StartCheckout(none.as(b), levyanv1.StartCheckoutRequest_builder{BillingAccount: bb.GetId(), SuccessUrl: back}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err))
	_, err = none.payers().CompleteCheckout(none.as(b), levyanv1.CompleteCheckoutRequest_builder{Checkout: pdid.New(1).String()}.Build())
	x.Equal(codes.FailedPrecondition, status.Code(err))
	nb, err := none.payers().GetBillingAccount(none.as(b), levyanv1.GetBillingAccountRequest_builder{Id: bb.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION, nb.GetCollection().GetState(), "said all the same")
}

// The merchant's webhooks come to levyan's public listener, at a path that
// names the channel and its adapter, and are kept in the inbox once each --
// checked, and answered with no request of levyan's to the merchant -- and
// taken by the loop after: a subscription the payer canceled at the merchant
// is mirrored canceled, and its billing account collectable no longer.
func TestTheMerchantsWebhooksComeToThePublicListener(t *testing.T) {
	p := subscribed(t)
	x := pdtest.NewX(t)
	ctx := context.Background()
	a := levyantest.Account()
	ba := p.account(t, a, "US")
	started, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: back}.Build())
	x.NoError(err)
	x.NoError(p.srv.Pay(opened(started.GetCheckout().GetWindow())))
	done, err := p.payers().CompleteCheckout(p.as(a), levyanv1.CompleteCheckoutRequest_builder{Checkout: started.GetCheckout().GetId()}.Build())
	x.NoError(err)
	sub := p.mirrored(t, done.GetSubscription().GetId())

	h, err := p.w.S.Http(ctx, p.w.C, grpc.NewServer())
	x.NoError(err)
	public := httptest.NewServer(h)
	t.Cleanup(public.Close)
	x.Equal("/webhooks/mor/paddle", subscriptions.WebhookPath(p.w.S.Inbox.Channel))
	post := func(hdr http.Header, body []byte) int {
		req, err := http.NewRequest(http.MethodPost, public.URL+"/webhooks/mor/paddle", bytes.NewReader(body))
		x.NoError(err)
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		res, err := public.Client().Do(req)
		x.NoError(err)
		res.Body.Close()
		return res.StatusCode
	}

	// The payer follows the link in Paddle's email: canceled at once,
	// without a word to levyan, which hears it as a webhook.
	gets := p.srv.Requests(paddletest.Get)
	p.srv.CancelNow(sub)
	hdr, body := p.srv.Webhook("subscription.canceled", sub)
	x.Equal(http.StatusOK, post(hdr, body))
	x.Equal(gets, p.srv.Requests(paddletest.Get), "answered with no request of levyan's to Paddle")
	x.Equal(http.StatusOK, post(hdr, body), "the same again")
	x.Equal(http.StatusOK, post(p.srv.Sign(body, p.srv.Now()), body), "sent again, signed again")
	n, err := p.w.S.Ent.Notice.Query().Where(entnotice.Type("subscription.canceled")).All(ctx)
	x.NoError(err)
	x.Len(n, 1, "kept once")
	x.Equal(int32(levyan.NoticeVia_NOTICE_VIA_WEBHOOK), n[0].Via)
	x.Nil(n[0].DateDone, "taken after it is answered")

	bad := p.srv.Sign(body, p.srv.Now())
	bad.Set("Paddle-Signature", "ts=1;h1=00")
	x.Equal(http.StatusBadRequest, post(bad, body), "a signature that does not hold")
	x.Equal(http.StatusBadRequest, post(http.Header{}, body), "and none")
	x.Equal(http.StatusRequestEntityTooLarge, post(hdr, bytes.Repeat([]byte(" "), subscriptions.MaxBody+1)))
	res, err := public.Client().Get(public.URL + "/webhooks/mor/paddle")
	x.NoError(err)
	res.Body.Close()
	x.Equal(http.StatusMethodNotAllowed, res.StatusCode)

	taken, err := subscriptions.Taker{In: p.w.S.Inbox, Subs: p.w.S.Subscriptions}.Take(ctx)
	x.NoError(err)
	x.GreaterOrEqual(taken, 1)
	got, err := p.payers().GetBillingAccount(p.as(a), levyanv1.GetBillingAccountRequest_builder{Id: ba.GetId()}.Build())
	x.NoError(err)
	x.Equal(levyanv1.CollectionState_COLLECTION_STATE_CANCELED, got.GetCollection().GetState())
	x.Equal(levyanv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED, got.GetSubscription().GetStatus())

	// With no webhook secret, there is no endpoint: the events are pulled.
	pulled := subscribed(t, func(c *cmd.Config) { c.Mor.WebhookSecret = paddletest.Secret("") })
	h, err = pulled.w.S.Http(ctx, pulled.w.C, grpc.NewServer())
	x.NoError(err)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/mor/paddle", bytes.NewReader(body)))
	x.Equal(http.StatusNotFound, rec.Code)
}

// A checkout says where it sends the payer back, as a page of the product's.
func TestACheckoutSendsThePayerBackToAPage(t *testing.T) {
	p := subscribed(t)
	x := pdtest.NewX(t)
	a := levyantest.Account()
	ba := p.account(t, a, "KR")
	for _, u := range []string{"", "/billing", "javascript:alert(1)"} {
		_, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: ba.GetId(), SuccessUrl: u}.Build())
		x.Equal(codes.InvalidArgument, status.Code(err), "%q", u)
	}
	_, err := p.payers().StartCheckout(p.as(a), levyanv1.StartCheckoutRequest_builder{BillingAccount: "nope", SuccessUrl: back}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err))
	x.Zero(p.srv.Requests(paddletest.Checkout), "nothing begun at the merchant")
}
