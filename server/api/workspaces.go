package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/payers"
)

// Workspaces is `levyan.v1.WorkspaceService`: what a product says of who pays
// for its workspaces, written through the walled stack as the product, and of
// the product's own workspaces only.
type Workspaces struct {
	levyanv1.UnimplementedWorkspaceServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server

	// P is who pays, written through S.
	P payers.Payers

	// Db is what the collection of the billing account a workspace is paid
	// for with is read with.
	Db *ent.Client
}

func (h Workspaces) product(ctx context.Context) (pdid.Id, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return pdid.Nil, err
	}

	return f.Tenant, nil
}

func account(v, what string) (pdid.Id, error) {
	a, err := pdid.Parse(v)
	if err != nil || a.IsZero() {
		return pdid.Nil, status.Errorf(codes.InvalidArgument, "%s: %q is not an account's identifier", what, v)
	}

	return a, nil
}

func (h Workspaces) Arrive(ctx context.Context, req *levyanv1.ArriveRequest) (*levyanv1.ArriveResponse, error) {
	product, err := h.product(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	creator, err := account(req.GetCreator(), "creator")
	if err != nil {
		return nil, err
	}
	var created time.Time
	if req.HasCreated() {
		created = req.GetCreated().AsTime()
	}

	st, err := h.P.On(h.S).Arrive(ctx, product, ws, creator, created)
	if err != nil {
		return nil, err
	}
	if err := h.P.On(h.S).Label(ctx, product, ws, req.GetLabel()); err != nil {
		return nil, err
	}
	p, err := h.payer(ctx, st)
	if err != nil {
		return nil, err
	}

	return levyanv1.ArriveResponse_builder{Payer: p}.Build(), nil
}

func (h Workspaces) Leave(ctx context.Context, req *levyanv1.LeaveRequest) (*levyanv1.LeaveResponse, error) {
	product, err := h.product(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	a, err := account(req.GetAccount(), "account")
	if err != nil {
		return nil, err
	}

	st, err := h.P.On(h.S).Leave(ctx, product, ws, a)
	if err != nil {
		return nil, err
	}
	p, err := h.payer(ctx, st)
	if err != nil {
		return nil, err
	}

	return levyanv1.LeaveResponse_builder{Payer: p}.Build(), nil
}

func (h Workspaces) Offer(ctx context.Context, req *levyanv1.OfferRequest) (*levyanv1.OfferResponse, error) {
	product, err := h.product(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	a, err := account(req.GetAccount(), "account")
	if err != nil {
		return nil, err
	}

	o, err := h.P.On(h.S).Offer(ctx, product, ws, a)
	if err != nil {
		return nil, err
	}

	return levyanv1.OfferResponse_builder{Offer: levyanv1.Offer_builder{
		Id:        text(o.GetId()),
		Workspace: ws.String(),
		Account:   a.String(),
		Offered:   stamp(o.GetDateCreated()),
	}.Build()}.Build(), nil
}

func (h Workspaces) GetPayer(ctx context.Context, req *levyanv1.GetPayerRequest) (*levyanv1.GetPayerResponse, error) {
	product, err := h.product(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	st, err := h.P.State(ctx, product, ws)
	if err != nil {
		return nil, err
	}
	p, err := h.payer(ctx, st)
	if err != nil {
		return nil, err
	}

	return levyanv1.GetPayerResponse_builder{Payer: p}.Build(), nil
}

// Close, Restore and Destroy are what became of a workspace at `at`, as the
// product says (#14): kept once of a moment, read in the order of their
// moments, and written through the walled stack as the product.
func (h Workspaces) Close(ctx context.Context, req *levyanv1.CloseRequest) (*levyanv1.CloseResponse, error) {
	ws, said, err := h.say(ctx, req.GetWorkspace(), req.HasAt(), req.GetAt().AsTime(), payers.Payers.Close)
	if err != nil {
		return nil, err
	}

	return levyanv1.CloseResponse_builder{Lifecycle: lifecycleOf(ws, said.Life), Late: said.Late}.Build(), nil
}

func (h Workspaces) Restore(ctx context.Context, req *levyanv1.RestoreRequest) (*levyanv1.RestoreResponse, error) {
	ws, said, err := h.say(ctx, req.GetWorkspace(), req.HasAt(), req.GetAt().AsTime(), payers.Payers.Restore)
	if err != nil {
		return nil, err
	}

	return levyanv1.RestoreResponse_builder{Lifecycle: lifecycleOf(ws, said.Life), Late: said.Late}.Build(), nil
}

func (h Workspaces) Destroy(ctx context.Context, req *levyanv1.DestroyRequest) (*levyanv1.DestroyResponse, error) {
	ws, said, err := h.say(ctx, req.GetWorkspace(), req.HasAt(), req.GetAt().AsTime(), payers.Payers.Destroy)
	if err != nil {
		return nil, err
	}

	return levyanv1.DestroyResponse_builder{Lifecycle: lifecycleOf(ws, said.Life), Late: said.Late}.Build(), nil
}

func (h Workspaces) GetLifecycle(ctx context.Context, req *levyanv1.GetLifecycleRequest) (*levyanv1.GetLifecycleResponse, error) {
	product, err := h.product(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := ledger.Workspace(req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	l, err := h.P.Life(ctx, product, ws)
	if err != nil {
		return nil, err
	}

	return levyanv1.GetLifecycleResponse_builder{Lifecycle: lifecycleOf(ws, l)}.Build(), nil
}

func (h Workspaces) say(ctx context.Context, workspace string, has bool, at time.Time, f func(payers.Payers, context.Context, pdid.Id, pdid.Id, time.Time) (payers.Said, error)) (pdid.Id, payers.Said, error) {
	product, err := h.product(ctx)
	if err != nil {
		return pdid.Nil, payers.Said{}, err
	}
	ws, err := ledger.Workspace(workspace)
	if err != nil {
		return pdid.Nil, payers.Said{}, status.Error(codes.InvalidArgument, err.Error())
	}
	if !has {
		// No moment is none, and never now: what is said late is said of
		// when it happened, and a retry said of now would be another event.
		return pdid.Nil, payers.Said{}, status.Error(codes.InvalidArgument, "at: when it happened, which the product's own record of it says")
	}

	said, err := f(h.P.On(h.S), ctx, product, ws, at)
	return ws, said, err
}

// lifecycleOf is what became of a workspace, as the API says it.
func lifecycleOf(ws pdid.Id, l rating.Life) *levyanv1.Lifecycle {
	out := levyanv1.Lifecycle_builder{Workspace: ws.String()}.Build()
	if !l.Closed.IsZero() {
		out.SetClosedSince(timestamppb.New(l.Closed))
	}
	if !l.Destroyed.IsZero() {
		out.SetDestroyed(timestamppb.New(l.Destroyed))
	}

	return out
}

// payer is who pays for a workspace, as the API says it, with the collection
// of the billing account it is paid for with now: of whether what it is
// billed can be collected, and why not, and nothing of how the payer pays.
func (h Workspaces) payer(ctx context.Context, st payers.State) (*levyanv1.Payer, error) {
	out := payerOf(st)
	if st.Current == nil || h.Db == nil {
		return out, nil
	}
	c, err := collectionOfLink(ctx, h.Db, st.Current.BillingAccountId)
	if err != nil {
		return nil, err
	}
	if c != nil {
		out.SetCollection(c)
	}

	return out, nil
}

// payerOf is who pays for a workspace, as the API says it.
func payerOf(st payers.State) *levyanv1.Payer {
	out := levyanv1.Payer_builder{
		Workspace: st.Workspace.String(),
		Current:   linkOf(st.Current, nil),
		Next:      linkOf(st.Next, nil),
	}.Build()
	for _, l := range []*levyanv1.Link{out.GetCurrent(), out.GetNext()} {
		if l != nil {
			l.SetWorkspace(st.Workspace.String())
		}
	}
	if o := st.Offer; o != nil {
		v := offerOf(o)
		v.SetWorkspace(st.Workspace.String())
		out.SetOffer(v)
	}
	if st.Payerless() {
		out.SetPayerlessSince(timestamppb.New(st.Since))
		out.SetRestrictedFrom(timestamppb.New(st.Restricted))
	}

	return out
}
