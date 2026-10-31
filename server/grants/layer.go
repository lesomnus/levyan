package grants

import (
	"context"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/bare"
)

// Lag is how far behind the server's clock a moment written as now may be:
// what a write worked out of the clock before it was made -- a beginning, an
// end -- having waited on a row another write held.
const Lag = time.Minute

// Layer is what a grant may be, said of every write of one, on both stacks
// (`cmd.Build`): so that an operator, a rule and a test are held to the same,
// and a write past [Grants] is still one the product can apply.
//
//   - An Add gives something, to be in force: a key, one value, an end after
//     its beginning and after now, a beginning no earlier than now, a source
//     -- and a rule's name exactly when the source is a rule. Its version is
//     1, whatever the request says. "Now" is the server's clock, less [Lag].
//   - A Patch moves its end, or says it renews no longer, and nothing else
//     (the rest is immutable): its version is one more than the row's,
//     whatever the request says, and an end is never moved before now, nor
//     later once the grant has ended -- but for one that renews and lapsed.
//     The row is read where the Patch writes, so [Grants] holds it first
//     (`FOR UPDATE`) and two changes of one grant are never one version.
//   - Apply is refused: a document is not held to any of this.
//   - Erase takes a grant out of every list once it has been over for
//     [Listed], which is what an erase of one is for; one that has not is
//     ended rather than erased, or the product would keep it until its end.
type Layer struct {
	levyan.Overlay

	// now is the deployment's clock.
	now func() time.Time
}

// New is the layer over `next`, telling the time by `now`.
func New(next levyan.Server, now func() time.Time) Layer {
	if now == nil {
		now = time.Now
	}

	return Layer{Overlay: levyan.NewOverlay(next), now: now}
}

// Build makes a builder of this layer so that it can be stacked.
func Build(now func() time.Time) levyan.Builder { return builder{now} }

type builder struct{ now func() time.Time }

func (b builder) Build(next levyan.Server) (levyan.Server, error) { return New(next, b.now), nil }

// WithDriver is this layer over the stack put on `drv`, with its clock.
func (s Layer) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return New(next, s.now), nil
}

var (
	_ levyan.Server               = Layer{}
	_ enttx.Binder[levyan.Server] = Layer{}
)

type layerGrant struct {
	Layer
	levyan.GrantServiceServer
}

func (s Layer) Grant() levyan.GrantServiceServer {
	v := layerGrant{s, s.Next().Grant()}
	v.GrantServiceServer = bare.GrantWithAddRule(v.GrantServiceServer, v.admit)
	return v
}

func (s layerGrant) at() time.Time { return Moment(s.now()) }

// admit is what a grant given may be.
func (s layerGrant) admit(ctx context.Context, req *levyan.GrantAddRequest) (*levyan.GrantAddRequest, error) {
	if err := entitle.CheckKey(req.GetKey()); err != nil {
		return nil, pderr.Invalidf("key", "%s", err)
	}
	if _, err := valueOf(req.GetValue(), req.GetLimit()); err != nil {
		return nil, pderr.Invalidf("value", "%s", err)
	}

	switch req.GetSource() {
	case levyan.GrantSource_GRANT_SOURCE_RULE:
		if req.GetRule() == "" {
			return nil, pderr.Invalidf("rule", "a grant of a rule says which rule")
		}
	case levyan.GrantSource_GRANT_SOURCE_OPERATOR, levyan.GrantSource_GRANT_SOURCE_CONTRACT, levyan.GrantSource_GRANT_SOURCE_TRIAL:
		if req.GetRule() != "" {
			return nil, pderr.Invalidf("rule", "only a grant of a rule names one")
		}
	default:
		return nil, pderr.Invalidf("source", "a grant says where it comes from: an operator, a rule, a contract or a trial")
	}

	now := s.at()
	req = proto.CloneOf(req)

	from := now
	if req.HasValidFrom() {
		from = Moment(req.GetValidFrom().AsTime())
		if from.Before(now.Add(-Lag)) {
			return nil, pderr.Invalidf("valid_from", "%s is before now: a grant does not reach back", from.Format(time.RFC3339))
		}
	}
	if !req.HasValidUntil() {
		return nil, pderr.Invalidf("valid_until", "every grant says when it ends, so that a product that stops hearing from levyan does not keep it forever")
	}
	until := Moment(req.GetValidUntil().AsTime())
	if !until.After(from) || !until.After(now.Add(-Lag)) {
		return nil, pderr.Invalidf("valid_until", "%s is not after its beginning and now: a grant is given to be in force", until.Format(time.RFC3339))
	}

	req.SetValidFrom(timestamppb.New(from))
	req.SetValidUntil(timestamppb.New(until))
	req.SetVersion(1)

	return req, nil
}

// Patch is a grant's end moved, or its renewal stopped, at the version after
// the row's.
func (s layerGrant) Patch(ctx context.Context, req *levyan.GrantPatchRequest) (*levyan.Grant, error) {
	was, err := s.Next().Grant().Get(ctx, levyan.GrantGetRequest_builder{
		Ref:    req.GetRef(),
		Select: levyan.GrantSelect_builder{All: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	now := s.at()
	req = proto.CloneOf(req)
	if req.GetRenews() && !was.GetRenews() {
		return nil, pderr.Invalidf("renews", "a grant renews from when it is given, or not at all")
	}
	if req.HasValidUntil() {
		until := Moment(req.GetValidUntil().AsTime())
		ended := !now.Before(was.GetValidUntil().AsTime())
		switch {
		case until.Before(now.Add(-Lag)):
			return nil, pderr.Invalidf("valid_until", "%s is before now: a grant is ended now, and never before", until.Format(time.RFC3339))
		case ended && !was.GetRenews() && until.After(was.GetValidUntil().AsTime()):
			return nil, status.Errorf(codes.FailedPrecondition, "the grant ended at %s and stays ended: give another", was.GetValidUntil().AsTime().Format(time.RFC3339))
		}
		req.SetValidUntil(timestamppb.New(until))
	}

	// The version is the server's: one more than the row's, whatever was
	// asked, so that every change of a grant is a greater one.
	req.SetVersion(was.GetVersion() + 1)

	return s.GrantServiceServer.Patch(ctx, req)
}

// Apply is refused: a grant's end and its renewal are moved by a Patch, which
// holds them to what they may be, and nothing else of a grant changes.
func (s layerGrant) Apply(ctx context.Context, req *levyan.GrantApplyRequest) (*levyan.Grant, error) {
	return nil, status.Error(codes.FailedPrecondition, "a grant is changed by a Patch, which holds what it may be, and never by a document")
}

// Erase takes a grant out of every list, once it has been over for [Listed].
func (s layerGrant) Erase(ctx context.Context, req *levyan.GrantRef) (*levyan.GrantEraseResponse, error) {
	was, err := s.Next().Grant().Get(ctx, levyan.GrantGetRequest_builder{
		Ref:    req,
		Select: levyan.GrantSelect_builder{ValidUntil: proto.Bool(true), Renews: proto.Bool(true)}.Build(),
	}.Build())
	if status.Code(err) == codes.NotFound {
		return s.GrantServiceServer.Erase(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if was.GetRenews() || s.at().Before(was.GetValidUntil().AsTime().Add(Listed)) {
		return nil, status.Errorf(codes.FailedPrecondition, "a grant is erased once it has been over for %s, and this one has not: end it", Listed)
	}

	return s.GrantServiceServer.Erase(ctx, req)
}
