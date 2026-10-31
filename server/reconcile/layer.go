package reconcile

import (
	"context"
	"strings"

	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/bare"
)

// Layer is what a finding may be, said of every write of one, on both
// stacks (`cmd.Build`): it is of a kind there is, and of something; it is
// found open, or found and put right at once, and never accepted as it is
// found; accepted, it says by whom and why.
//
// Apply is refused: a finding is what a pass wrote, or an operator accepted,
// and no document's.
type Layer struct {
	levyan.Overlay
}

// NewLayer is the layer over `next`.
func NewLayer(next levyan.Server) Layer { return Layer{Overlay: levyan.NewOverlay(next)} }

// Build makes a builder of this layer so that it can be stacked.
func Build() levyan.Builder { return builder{} }

type builder struct{}

func (builder) Build(next levyan.Server) (levyan.Server, error) { return NewLayer(next), nil }

// WithDriver is this layer over the stack put on `drv`.
func (s Layer) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return NewLayer(next), nil
}

var (
	_ levyan.Server               = Layer{}
	_ enttx.Binder[levyan.Server] = Layer{}
)

type layerFinding struct {
	Layer
	levyan.FindingServiceServer
}

func (s Layer) Finding() levyan.FindingServiceServer {
	v := layerFinding{s, s.Next().Finding()}
	v.FindingServiceServer = bare.FindingWithAddRule(v.FindingServiceServer, admitFinding)
	return v
}

func admitFinding(ctx context.Context, req *levyan.FindingAddRequest) (*levyan.FindingAddRequest, error) {
	k, ok := KindOf(req.GetKind())
	switch {
	case !ok:
		return nil, pderr.Invalidf("kind", "a finding is of a kind there is, by its name: FINDING_KIND_TWICE")
	case strings.TrimSpace(req.GetSubject()) == "":
		return nil, pderr.Invalidf("subject", "a finding is of something")
	case req.GetSeverity() != SeverityOf(k):
		return nil, pderr.Invalidf("severity", "a finding of its kind is %s", severityName(SeverityOf(k)))
	case req.GetState() != levyan.FindingState_FINDING_STATE_OPEN && req.GetState() != levyan.FindingState_FINDING_STATE_RESOLVED:
		return nil, pderr.Invalidf("state", "a finding is found open, or found and put right")
	case req.GetState() == levyan.FindingState_FINDING_STATE_RESOLVED && !req.HasDateResolved():
		return nil, pderr.Invalidf("date_resolved", "a finding put right says when")
	case req.HasDateAccepted() || req.GetAcceptedBy() != nil || req.GetReason() != "":
		return nil, pderr.Invalidf("accepted_by", "a finding is accepted by an operator once it is found")
	}

	return req, nil
}

// Patch is a finding changed: accepted, it says by whom and why.
func (s layerFinding) Patch(ctx context.Context, req *levyan.FindingPatchRequest) (*levyan.Finding, error) {
	if !req.HasState() || req.GetState() != levyan.FindingState_FINDING_STATE_ACCEPTED {
		if req.HasState() && req.GetState() == levyan.FindingState_FINDING_STATE_UNSPECIFIED {
			return nil, pderr.Invalidf("state", "a finding is somewhere")
		}
		return s.FindingServiceServer.Patch(ctx, req)
	}

	was, err := s.Next().Finding().Get(ctx, levyan.FindingGetRequest_builder{
		Ref:    req.GetRef(),
		Select: levyan.FindingSelect_builder{State: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}
	switch {
	case was.GetState() == levyan.FindingState_FINDING_STATE_RESOLVED:
		return nil, status.Error(codes.FailedPrecondition, "a finding that is resolved is not accepted")
	case strings.TrimSpace(req.GetReason()) == "" || req.GetAcceptedBy() == nil || !req.HasDateAccepted():
		return nil, pderr.Invalidf("reason", "a finding accepted says why, by whom and when")
	}

	return s.FindingServiceServer.Patch(ctx, req)
}

func (s layerFinding) Apply(ctx context.Context, req *levyan.FindingApplyRequest) (*levyan.Finding, error) {
	return nil, status.Error(codes.FailedPrecondition, "a finding is what reconciliation wrote, or an operator accepted, and no document's")
}
