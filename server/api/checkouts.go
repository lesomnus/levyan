package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entcheckout "github.com/lesomnus/levyan/internal/ent/checkout"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/pd"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// What an account asks of its billing accounts' subscriptions with the
// merchant of record (#12): a billing account with its collection, and the
// checkout that begins its subscription. What it writes -- a checkout, the
// mirror of the subscription it made, the billing account's collection -- is
// its own and no product's, and is written as itself, through the walled
// stack, once it has been read with ent that the account may
// (`server/subscriptions`).

// ownCheckouts is what an account reads of checkouts: its own.
func ownCheckouts(ctx context.Context, account pdid.Id) (predicate.Checkout, error) {
	return entcheckout.Account(account.Uuid()), nil
}

// ownSubscriptions is what an account reads of the mirrors of subscriptions:
// those of its own billing accounts.
func ownSubscriptions(ctx context.Context, account pdid.Id) (predicate.Subscription, error) {
	return entsubscription.HasBillingAccountWith(entbillingaccount.Owner(account.Uuid())), nil
}

// checkoutReads is what a checkout of an account's reads, and writes, through
// the walled stack: its billing account, its checkouts, and their mirrors.
var checkoutReads = pd.Reads{BillingAccount: ownBillingAccounts, Checkout: ownCheckouts, Subscription: ownSubscriptions}

func (h Payers) GetBillingAccount(ctx context.Context, req *levyanv1.GetBillingAccountRequest) (*levyanv1.GetBillingAccountResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_GetBillingAccount_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_GetBillingAccount_FullMethodName, pd.Reads{BillingAccount: ownBillingAccounts, Subscription: ownSubscriptions})
	if err != nil {
		return nil, err
	}
	id, err := pdid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %q is not a billing account's identifier", req.GetId())
	}

	v, err := h.S.BillingAccount().Get(ctx, levyan.BillingAccountGetRequest_builder{
		Ref: levyan.BillingAccountRef_builder{Id: id.Bytes()}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	if v.HasDateForgotten() {
		return nil, status.Errorf(codes.NotFound, "no billing account of the account's is %s", id)
	}
	b, err := h.Db.BillingAccount.Get(ctx, id.Uuid())
	if err != nil {
		return nil, err
	}
	col, err := subscriptions.CollectionOf(ctx, h.Db, b)
	if err != nil {
		return nil, err
	}

	// What is done of its invoices whose charge failed, of the relaying
	// product's alone: a billing account is the account's in every product,
	// and what it is billed in one is no other product's to show.
	ds, err := invoices.AccountDunning(ctx, h.Db, h.Dunning, product.Uuid(), b.Id, clock(h.Now))
	if err != nil {
		return nil, err
	}

	out := levyanv1.GetBillingAccountResponse_builder{
		BillingAccount: billingAccount(v),
		Collection:     collectionOf(col),
		Dunning:        dunningsOf(ds),
	}.Build()
	if col.Mirror != nil {
		m, err := h.mirror(ctx, col.Mirror.Id[:])
		if err != nil {
			return nil, err
		}
		out.SetSubscription(m)
	}

	return out, nil
}

func (h Payers) StartCheckout(ctx context.Context, req *levyanv1.StartCheckoutRequest) (*levyanv1.StartCheckoutResponse, error) {
	ctx, me, _, err := as(ctx, levyan.PayerService_StartCheckout_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_StartCheckout_FullMethodName, checkoutReads); err != nil {
		return nil, err
	}
	billing, err := pdid.Parse(req.GetBillingAccount())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", req.GetBillingAccount())
	}

	co, w, err := h.M.On(h.S).Start(ctx, me, billing, req.GetSuccessUrl())
	if err != nil {
		return nil, err
	}
	b, err := h.Db.BillingAccount.Get(ctx, co.BillingAccountId)
	if err != nil {
		return nil, err
	}

	out := checkoutOf(co)
	out.SetWindow(levyanv1.CheckoutWindow_builder{
		Merchant:    co.Mor,
		Environment: w.Environment,
		Transaction: co.MorTransaction,
		ClientToken: w.Token,
		Script:      w.Script,
		Url:         w.URL,
		Country:     b.Country,
	}.Build())

	return levyanv1.StartCheckoutResponse_builder{Checkout: out}.Build(), nil
}

func (h Payers) CompleteCheckout(ctx context.Context, req *levyanv1.CompleteCheckoutRequest) (*levyanv1.CompleteCheckoutResponse, error) {
	ctx, me, _, err := as(ctx, levyan.PayerService_CompleteCheckout_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_CompleteCheckout_FullMethodName, checkoutReads); err != nil {
		return nil, err
	}
	id, err := pdid.Parse(req.GetCheckout())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "checkout: %q is not a checkout's identifier", req.GetCheckout())
	}

	co, m, err := h.M.On(h.S).Complete(ctx, me, id)
	if err != nil {
		return nil, err
	}
	b, err := h.Db.BillingAccount.Get(ctx, co.BillingAccountId)
	if err != nil {
		return nil, err
	}
	col, err := subscriptions.CollectionOf(ctx, h.Db, b)
	if err != nil {
		return nil, err
	}

	out := levyanv1.CompleteCheckoutResponse_builder{
		Checkout:   checkoutOf(co),
		Collection: collectionOf(col),
	}.Build()
	if m != nil {
		v, err := h.mirror(ctx, m.Id[:])
		if err != nil {
			return nil, err
		}
		out.SetSubscription(v)
	}

	return out, nil
}

// mirror is the mirror of a subscription, read through the walled stack --
// the account's own, as its call declared -- as the API says one.
func (h Payers) mirror(ctx context.Context, id []byte) (*levyanv1.Subscription, error) {
	v, err := h.S.Subscription().Get(ctx, levyan.SubscriptionGetRequest_builder{
		Ref: levyan.SubscriptionRef_builder{Id: id}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	return subscriptionOf(v), nil
}

func checkoutOf(v *ent.Checkout) *levyanv1.Checkout {
	out := levyanv1.Checkout_builder{
		Id:             pdid.Id(v.Id).String(),
		BillingAccount: pdid.Id(v.BillingAccountId).String(),
		State:          levyanv1.CheckoutState(v.State),
		Started:        timestamppb.New(v.DateCreated),
	}.Build()
	if v.DateDone != nil {
		out.SetDone(timestamppb.New(*v.DateDone))
	}

	return out
}

func subscriptionOf(v *levyan.Subscription) *levyanv1.Subscription {
	out := levyanv1.Subscription_builder{
		Id:          text(v.GetId()),
		Merchant:    v.GetMor(),
		Status:      levyanv1.SubscriptionStatus(v.GetStatus()),
		Currency:    v.GetCurrency(),
		PeriodStart: stamp(v.GetDatePeriodStart()),
		PeriodEnd:   stamp(v.GetDatePeriodEnd()),
		NextBilled:  stamp(v.GetDateNext()),
		Change:      levyanv1.SubscriptionChange(v.GetChange()),
		ChangeAt:    stamp(v.GetDateChange()),
		Canceled:    stamp(v.GetDateCanceled()),
		Updated:     stamp(v.GetDateMorUpdated()),
		Checked:     stamp(v.GetDateChecked()),
	}.Build()
	if v.GetMethodType() != "" {
		out.SetMethod(levyanv1.PaymentMethod_builder{
			Type:  v.GetMethodType(),
			Brand: v.GetMethodBrand(),
			Last4: v.GetMethodLast4(),
		}.Build())
	}

	return out
}

func collectionOf(c subscriptions.Collection) *levyanv1.Collection {
	out := levyanv1.Collection_builder{State: levyanv1.CollectionState(c.State)}.Build()
	if !c.Since.IsZero() {
		out.SetSince(timestamppb.New(c.Since))
	}
	if !c.Ends.IsZero() {
		out.SetEnds(timestamppb.New(c.Ends))
	}

	return out
}

// collectionOfLink is the collection of the billing account link `billing`
// pays with: nil for none.
func collectionOfLink(ctx context.Context, db *ent.Client, billing [16]byte) (*levyanv1.Collection, error) {
	if billing == ([16]byte{}) {
		return nil, nil
	}
	b, err := db.BillingAccount.Get(ctx, billing)
	if err != nil {
		return nil, err
	}
	if b.Channel != levyan.Channel_CHANNEL_MOR.String() {
		return nil, nil
	}
	c, err := subscriptions.CollectionOf(ctx, db, b)
	if err != nil {
		return nil, err
	}

	return collectionOf(c), nil
}
