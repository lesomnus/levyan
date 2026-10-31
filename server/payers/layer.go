package payers

import (
	"context"

	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/protobuf/proto"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/bare"
)

// Layer is what a billing account and a link may be, said of every write of
// one, on both stacks (`cmd.Build`): an account's billing account says whose
// it is and the country it is billed in, and its channel and currency are
// what that country says, whatever a request says; a link says who pays and
// from when.
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

func (s Layer) BillingAccount() levyan.BillingAccountServiceServer {
	return bare.BillingAccountWithAddRule(s.Next().BillingAccount(), admitBillingAccount)
}

func admitBillingAccount(ctx context.Context, req *levyan.BillingAccountAddRequest) (*levyan.BillingAccountAddRequest, error) {
	if len(req.GetOwner()) != 16 {
		return nil, pderr.Invalidf("owner", "a billing account is an account's, at roster")
	}
	channel, currency, err := ChannelOf(req.GetCountry())
	if err != nil {
		return nil, pderr.Invalidf("country", "%q is not ISO 3166-1 alpha-2, in capitals: KR", req.GetCountry())
	}
	if req.HasDateForgotten() {
		return nil, pderr.Invalidf("date_forgotten", "a billing account is made with what says who it is billed to")
	}

	req = proto.CloneOf(req)
	req.SetChannel(channel)
	req.SetCurrency(currency)

	return req, nil
}

func (s Layer) Link() levyan.LinkServiceServer {
	return bare.LinkWithAddRule(s.Next().Link(), admitLink)
}

func admitLink(ctx context.Context, req *levyan.LinkAddRequest) (*levyan.LinkAddRequest, error) {
	switch {
	case len(req.GetPayer()) != 16:
		return nil, pderr.Invalidf("payer", "a link says who pays: an account, at roster")
	case !req.HasDateStart():
		return nil, pderr.Invalidf("date_start", "a link says from when it is in force")
	case req.GetBegan() == levyan.LinkBegan_LINK_BEGAN_UNSPECIFIED:
		return nil, pderr.Invalidf("began", "a link says how it began")
	case req.HasDateEnd() || req.GetEnded() != levyan.LinkEnded_LINK_ENDED_UNSPECIFIED:
		return nil, pderr.Invalidf("date_end", "a link is ended once it is in, by a Patch")
	}

	return req, nil
}
