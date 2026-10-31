package api

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/batch"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/payers"
	"github.com/lesomnus/levyan/server/pd"
	"github.com/lesomnus/levyan/server/relay"
)

// Payers is `levyan.v1.PayerService`: what an account asks of levyan, as
// itself, through the product that relays its call (`server/relay`).
//
// Each method is served as the schema's method of the same name
// (`levyan.PayerService`, `proto/levyan/payer.proto`), which is what payday
// holds an account's call to: it reads through the walled stack only what
// its layer declares (`pd.AccountReads`) -- the account's own billing
// accounts -- and what it makes in the relaying product's tenant, a link or
// an offer's answer, it writes there as the deployment, within that product
// and no other, once it has read with ent that the account may (payday's
// accounts guide, "What it writes").
type Payers struct {
	levyanv1.UnimplementedPayerServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server

	// P is who pays, written through S.
	P payers.Payers

	// Db is what is read of the workspaces and periods an answer names, once
	// the walled stack has said the account may see what names them.
	Db *ent.Client
}

// PayerMethods is the methods of `levyan.v1.PayerService` by the names gRPC
// knows them by, each with the schema's method it is served as.
var PayerMethods = func() map[string]string {
	out := map[string]string{}
	for _, m := range levyanv1.PayerService_ServiceDesc.Methods {
		out["/"+levyanv1.PayerService_ServiceDesc.ServiceName+"/"+m.MethodName] = "/" + levyan.PayerService_ServiceDesc.ServiceName + "/" + m.MethodName
	}
	return out
}()

// Calls is what a call an account makes as itself may be of: the schema's
// methods, which payday holds it to, and the API's that are served as them.
func Calls(method string) bool {
	_, ok := PayerMethods[method]
	return ok || pd.AccountMethod(method)
}

// as is the call `ctx` belongs to, served as the schema's `method`: the
// account it is made as, and the product that relays it.
func as(ctx context.Context, method string) (context.Context, pdid.Id, pdid.Id, error) {
	f, ok := frame.From(ctx)
	product, relayed := relay.Product(ctx)
	if !ok || !f.AsAccount() || !relayed {
		return nil, pdid.Nil, pdid.Nil, status.Error(codes.PermissionDenied, "an account's own, relayed by a product")
	}

	return batch.AsOp(ctx, method), f.Account, product, nil
}

// inProduct is `ctx` framed as the deployment within `product`'s tenant:
// what an account's answer makes there is written so, through the walled
// stack, which keeps it to that product.
func inProduct(ctx context.Context, product pdid.Id) context.Context {
	return frame.Into(ctx, frame.New(pdid.Nil, product, frame.Whole()).WithScope(frame.Only(product)))
}

// ownBillingAccounts is what an account reads of billing accounts: its own.
func ownBillingAccounts(ctx context.Context, account pdid.Id) (predicate.BillingAccount, error) {
	return entbillingaccount.Owner(account.Uuid()), nil
}

func (h Payers) ListBillingAccounts(ctx context.Context, _ *levyanv1.ListBillingAccountsRequest) (*levyanv1.ListBillingAccountsResponse, error) {
	ctx, _, _, err := as(ctx, levyan.PayerService_ListBillingAccounts_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListBillingAccounts_FullMethodName, pd.Reads{BillingAccount: ownBillingAccounts})
	if err != nil {
		return nil, err
	}

	out := &levyanv1.ListBillingAccountsResponse{}
	after := ""
	for {
		vs, err := h.S.BillingAccount().List(ctx, levyan.BillingAccountListRequest_builder{Size: page, After: after}.Build())
		if err != nil {
			return nil, err
		}
		for _, v := range vs.GetItems() {
			if v.HasDateForgotten() {
				continue
			}
			out.SetBillingAccounts(append(out.GetBillingAccounts(), billingAccount(v)))
		}
		if after = vs.GetNext(); after == "" {
			return out, nil
		}
	}
}

func (h Payers) CreateBillingAccount(ctx context.Context, req *levyanv1.CreateBillingAccountRequest) (*levyanv1.CreateBillingAccountResponse, error) {
	ctx, me, _, err := as(ctx, levyan.PayerService_CreateBillingAccount_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_CreateBillingAccount_FullMethodName, pd.Reads{BillingAccount: ownBillingAccounts})
	if err != nil {
		return nil, err
	}

	v := levyan.BillingAccount_builder{
		Name:      strings.TrimSpace(req.GetName()),
		Country:   strings.ToUpper(strings.TrimSpace(req.GetCountry())),
		Recipient: strings.TrimSpace(req.GetRecipient().GetName()),
		Email:     strings.TrimSpace(req.GetRecipient().GetEmail()),
		Address:   strings.TrimSpace(req.GetRecipient().GetAddress()),
	}.Build()
	if req.HasBusiness() {
		v.SetBusiness(business(req.GetBusiness()))
	}
	got, err := h.P.On(h.S).Create(ctx, me, v)
	if err != nil {
		return nil, err
	}

	return levyanv1.CreateBillingAccountResponse_builder{BillingAccount: billingAccount(got)}.Build(), nil
}

func (h Payers) UpdateBillingAccount(ctx context.Context, req *levyanv1.UpdateBillingAccountRequest) (*levyanv1.UpdateBillingAccountResponse, error) {
	ctx, me, _, err := as(ctx, levyan.PayerService_UpdateBillingAccount_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_UpdateBillingAccount_FullMethodName, pd.Reads{BillingAccount: ownBillingAccounts})
	if err != nil {
		return nil, err
	}
	id, err := pdid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %q is not a billing account's identifier", req.GetId())
	}

	patch := levyan.BillingAccountPatchRequest_builder{}.Build()
	if req.HasName() {
		patch.SetName(strings.TrimSpace(req.GetName()))
	}
	if r := req.GetRecipient(); r != nil {
		patch.SetRecipient(strings.TrimSpace(r.GetName()))
		patch.SetEmail(strings.TrimSpace(r.GetEmail()))
		patch.SetAddress(strings.TrimSpace(r.GetAddress()))
	}
	switch {
	case req.GetClearBusiness():
		patch.SetBusinessNull(true)
	case req.HasBusiness():
		patch.SetBusiness(business(req.GetBusiness()))
	}

	got, err := h.P.On(h.S).Update(ctx, me, id, patch)
	if err != nil {
		return nil, err
	}

	return levyanv1.UpdateBillingAccountResponse_builder{BillingAccount: billingAccount(got)}.Build(), nil
}

func (h Payers) ListOffers(ctx context.Context, _ *levyanv1.ListOffersRequest) (*levyanv1.ListOffersResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_ListOffers_FullMethodName)
	if err != nil {
		return nil, err
	}
	// Nothing through the walled servers: an offer of the product's is read
	// with ent, of the product and the account.
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListOffers_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}

	vs, err := h.P.Offers(ctx, product, me)
	if err != nil {
		return nil, err
	}
	out := &levyanv1.ListOffersResponse{}
	for _, v := range vs {
		out.SetOffers(append(out.GetOffers(), offerOf(v)))
	}

	return out, nil
}

func (h Payers) Accept(ctx context.Context, req *levyanv1.AcceptRequest) (*levyanv1.AcceptResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_Accept_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_Accept_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}
	offer, err := pdid.Parse(req.GetOffer())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "offer: %q is not an offer's identifier", req.GetOffer())
	}
	billing := pdid.Nil
	if v := req.GetBillingAccount(); v != "" {
		if billing, err = pdid.Parse(v); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", v)
		}
	}

	l, err := h.P.On(h.S).Accept(inProduct(ctx, product), product, me, offer, billing)
	if err != nil {
		return nil, err
	}

	return levyanv1.AcceptResponse_builder{Link: h.link(ctx, l)}.Build(), nil
}

func (h Payers) Decline(ctx context.Context, req *levyanv1.DeclineRequest) (*levyanv1.DeclineResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_Decline_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_Decline_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}
	offer, err := pdid.Parse(req.GetOffer())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "offer: %q is not an offer's identifier", req.GetOffer())
	}

	if err := h.P.On(h.S).Decline(inProduct(ctx, product), product, me, offer); err != nil {
		return nil, err
	}

	return &levyanv1.DeclineResponse{}, nil
}

func (h Payers) ListLinks(ctx context.Context, _ *levyanv1.ListLinksRequest) (*levyanv1.ListLinksResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_ListLinks_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListLinks_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}

	vs, err := h.P.Links(ctx, product, me)
	if err != nil {
		return nil, err
	}
	out := &levyanv1.ListLinksResponse{}
	for _, v := range vs {
		out.SetLinks(append(out.GetLinks(), linkOf(v, v.Edges.Workspace)))
	}

	return out, nil
}

func (h Payers) Use(ctx context.Context, req *levyanv1.UseRequest) (*levyanv1.UseResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_Use_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_Use_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	billing, err := pdid.Parse(req.GetBillingAccount())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", req.GetBillingAccount())
	}

	l, err := h.P.On(h.S).Use(inProduct(ctx, product), product, me, ws, billing)
	if err != nil {
		return nil, err
	}

	return levyanv1.UseResponse_builder{Link: h.link(ctx, l)}.Build(), nil
}

func (h Payers) Stop(ctx context.Context, req *levyanv1.StopRequest) (*levyanv1.StopResponse, error) {
	ctx, me, product, err := as(ctx, levyan.PayerService_Stop_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_Stop_FullMethodName, pd.Reads{}); err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	l, err := h.P.On(h.S).Stop(inProduct(ctx, product), product, me, ws)
	if err != nil {
		return nil, err
	}

	return levyanv1.StopResponse_builder{Link: h.link(ctx, l)}.Build(), nil
}

// link is a link as the API says one, its workspace read by its identifier.
func (h Payers) link(ctx context.Context, v *levyan.Link) *levyanv1.Link {
	if v == nil {
		return nil
	}
	ws, err := pdid.From(v.GetWorkspace().GetId())
	if err != nil {
		return nil
	}
	w, err := h.P.WorkspaceOf(ctx, ws)
	if err != nil {
		return nil
	}

	l := levyanv1.Link_builder{
		Id:             text(v.GetId()),
		Workspace:      text(w.Key),
		Account:        text(v.GetPayer()),
		BillingAccount: text(v.GetBillingAccount().GetId()),
		Start:          stamp(v.GetDateStart()),
		Began:          levyanv1.LinkBegan(v.GetBegan()),
		Ended:          levyanv1.LinkEnded(v.GetEnded()),
	}.Build()
	if v.HasDateEnd() {
		l.SetEnd(stamp(v.GetDateEnd()))
	}

	return l
}

func linkOf(v *ent.Link, w *ent.Workspace) *levyanv1.Link {
	if v == nil {
		return nil
	}
	l := levyanv1.Link_builder{
		Id:      pdid.Id(v.Id).String(),
		Account: pdid.Id(v.Payer).String(),
		Start:   timestamppb.New(v.DateStart),
		Began:   levyanv1.LinkBegan(v.Began),
		Ended:   levyanv1.LinkEnded(v.Ended),
	}.Build()
	if w != nil {
		l.SetWorkspace(text(w.Key))
	}
	if b := v.BillingAccountId; b != [16]byte{} {
		l.SetBillingAccount(pdid.Id(b).String())
	}
	if v.DateEnd != nil {
		l.SetEnd(timestamppb.New(*v.DateEnd))
	}

	return l
}

func offerOf(v *ent.Offer) *levyanv1.Offer {
	o := levyanv1.Offer_builder{
		Id:      pdid.Id(v.Id).String(),
		Account: pdid.Id(v.Account).String(),
		Offered: timestamppb.New(v.DateCreated),
	}.Build()
	if w := v.Edges.Workspace; w != nil {
		o.SetWorkspace(text(w.Key))
	}

	return o
}

func billingAccount(v *levyan.BillingAccount) *levyanv1.BillingAccount {
	out := levyanv1.BillingAccount_builder{
		Id:       text(v.GetId()),
		Name:     v.GetName(),
		Country:  v.GetCountry(),
		Currency: v.GetCurrency(),
		Recipient: levyanv1.Recipient_builder{
			Name:    v.GetRecipient(),
			Email:   v.GetEmail(),
			Address: v.GetAddress(),
		}.Build(),
		Created: stamp(v.GetDateCreated()),
	}.Build()
	if c, ok := levyanv1.Channel_value[v.GetChannel()]; ok {
		out.SetChannel(levyanv1.Channel(c))
	}
	if b := v.GetBusiness(); v.HasBusiness() {
		out.SetBusiness(levyanv1.Business_builder{
			Registration:   b.GetRegistration(),
			Name:           b.GetName(),
			Representative: b.GetRepresentative(),
			Address:        b.GetAddress(),
			Kind:           b.GetKind(),
			Item:           b.GetItem(),
			Email:          b.GetEmail(),
		}.Build())
	}

	return out
}

func business(b *levyanv1.Business) *levyan.BillingBusiness {
	return levyan.BillingBusiness_builder{
		Registration:   strings.TrimSpace(b.GetRegistration()),
		Name:           strings.TrimSpace(b.GetName()),
		Representative: strings.TrimSpace(b.GetRepresentative()),
		Address:        strings.TrimSpace(b.GetAddress()),
		Kind:           strings.TrimSpace(b.GetKind()),
		Item:           strings.TrimSpace(b.GetItem()),
		Email:          strings.TrimSpace(b.GetEmail()),
	}.Build()
}
