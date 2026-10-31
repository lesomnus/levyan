package api

import (
	"cmp"
	"context"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/watch"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/pd"
)

// Grants is payday's `GrantService`, served to products: what levyan gives
// their workspaces, which a product keeps a copy of with payday's
// `entitle.Follower` (payday#185, #7).
//
// Whose grants a caller hears of is its own product's and no other's: every
// read is through the walled stack with the caller's frame, so the wall keeps
// a product to its own grants here as it does everywhere, and the request
// names no product. The workspace a grant is of is the product's own
// identifier of it, its payday tenant's.
type Grants struct {
	pdpb.UnimplementedGrantServiceServer

	// S is the stack a caller reaches: the wall on it, and the gate.
	S levyan.Server

	// W is what every grant's change is published to once it is committed,
	// by the drain of the outbox it is written to in its transaction.
	W *watch.Watch
}

// grantService is the prefix of every method of the schema's GrantService,
// which a change of a grant is written by.
var grantService = watch.ServiceOf(levyan.GrantService_Add_FullMethodName)

// Watch sends `ready` once every change from then on will be sent, and then
// each grant of the caller's product as it is after it changed.
//
// A consumer that cannot keep up is cut off rather than buffered for: its
// subscription is closed once it is [watch.Backlog] changes behind, and its
// stream then ends, ResourceExhausted, for it to open again and compare --
// which is what a follower does anyway after any stream that ended. Nothing
// is kept for a stream past that, and no write waits for one.
func (g Grants) Watch(_ *pdpb.GrantWatchRequest, out grpc.ServerStreamingServer[pdpb.GrantWatchResponse]) error {
	ctx := out.Context()
	if _, err := gate.Actor(ctx); err != nil {
		return err
	}
	if g.W == nil || g.W.Broker() == nil {
		return watch.ErrNoBroker
	}

	// Before ready, so that nothing committed after it is missed.
	events, stop := g.W.Subscribe()
	defer stop()

	if err := out.Send(pdpb.GrantWatchResponse_builder{Ready: &pdpb.GrantWatchReady{}}.Build()); err != nil {
		return err
	}

	for {
		ks, err := watch.Next(ctx, events, grantService)
		if err != nil {
			return err
		}

		// Each grant read as it is now, once however often it changed while
		// the stream was busy: what is sent is state.
		ids := make([]pdid.Id, 0, len(ks))
		for k := range ks {
			ids = append(ids, k)
		}
		slices.SortFunc(ids, func(a, b pdid.Id) int { return cmp.Compare(a.String(), b.String()) })
		for _, id := range ids {
			v, err := g.S.Grant().Get(ctx, levyan.GrantGetRequest_builder{
				Ref: levyan.GrantRef_builder{Id: id.Bytes()}.Build(),
				Select: levyan.GrantSelect_builder{
					All:       proto.Bool(true),
					Workspace: levyan.WorkspaceSelect_builder{Key: proto.Bool(true)}.Build(),
				}.Build(),
			}.Build())
			if status.Code(err) == codes.NotFound {
				// Another product's, or one erased out of every list -- which
				// has ended, and is forgotten at the next comparison.
				continue
			}
			if err != nil {
				return err
			}

			m, err := entitlementGrant(v)
			if err != nil {
				return err
			}
			if err := out.Send(pdpb.GrantWatchResponse_builder{Grant: m}.Build()); err != nil {
				return err
			}
		}
	}
}

// List reads the grants of the caller's product's workspaces, or of one when
// the request names its tenant, a page at a time: every grant that has not
// ended, and those that have for [grants.Listed] after.
func (g Grants) List(ctx context.Context, req *pdpb.GrantListRequest) (*pdpb.GrantListResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}

	q := levyan.GrantListRequest_builder{Size: req.GetSize(), After: req.GetCursor()}.Build()
	if b := req.GetTenantId(); len(b) > 0 {
		ws, err := pdid.From(b)
		if err != nil || ws.IsZero() || ws.Domain() != pd.TenantDomain {
			return nil, status.Error(codes.InvalidArgument, "tenant_id: the identifier of a payday tenant of the caller's")
		}
		q.SetFilters([]*levyan.GrantFilter{levyan.GrantFilter_builder{
			Workspace: levyan.WorkspaceRef_builder{Keyed: levyan.WorkspaceRefByKeyed_builder{
				Key:    ws.Bytes(),
				Tenant: levyan.TenantRef_builder{Id: f.Tenant.Bytes()}.Build(),
			}.Build()}.Build(),
		}.Build()})
	}

	vs, err := g.S.Grant().List(ctx, q)
	if err != nil {
		return nil, err
	}

	out := pdpb.GrantListResponse_builder{Next: vs.GetNext()}.Build()
	for _, v := range vs.GetItems() {
		m, err := entitlementGrant(v)
		if err != nil {
			return nil, err
		}
		out.SetItems(append(out.GetItems(), m))
	}

	return out, nil
}

// entitlementGrant is a grant as payday's contract says one: its identifier,
// its workspace by the product's identifier of it, its key, version, value
// and window.
func entitlementGrant(v *levyan.Grant) (*pdpb.EntitlementGrant, error) {
	ws := v.GetWorkspace().GetKey()
	if len(ws) == 0 {
		return nil, status.Errorf(codes.Internal, "grant %s: its workspace was not read with it", text(v.GetId()))
	}

	b := pdpb.EntitlementGrant_builder{
		Id:         v.GetId(),
		TenantId:   ws,
		Key:        v.GetKey(),
		Version:    v.GetVersion(),
		ValidFrom:  timestamppb.New(v.GetValidFrom().AsTime()),
		ValidUntil: timestamppb.New(v.GetValidUntil().AsTime()),
	}
	switch value := grants.Value(v); value.Kind() {
	case entitle.KindOn:
		b.On = proto.Bool(true)
	case entitle.KindLimit:
		n, _ := value.Number()
		b.Limit = proto.Int64(n)
	case entitle.KindUnlimited:
		b.Unlimited = &pdpb.EntitlementGrant_Unlimited{}
	default:
		return nil, status.Errorf(codes.Internal, "grant %s gives nothing a grant gives", text(v.GetId()))
	}

	return b.Build(), nil
}
