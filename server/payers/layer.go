package payers

import (
	"context"
	"strings"

	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/taxinvoice"
	"github.com/lesomnus/levyan/server/bare"
)

// Layer is what a billing account and a link may be, said of every write of
// one, on both stacks (`cmd.Build`): an account's billing account says whose
// it is and the country it is billed in, and its channel and currency are
// what [Layer.Channels] says of that country, whatever a request says; a link
// says who pays and from when.
type Layer struct {
	levyan.Overlay

	// Channels is what a billing account is billed through, by its country:
	// asked as one is made, and never of one made already.
	Channels Channels
}

// NewLayer is the layer over `next`, billing accounts through `channels` --
// [DefaultChannels] when nil.
func NewLayer(next levyan.Server, channels Channels) Layer {
	if channels == nil {
		channels = DefaultChannels()
	}

	return Layer{Overlay: levyan.NewOverlay(next), Channels: channels}
}

// Build makes a builder of this layer so that it can be stacked.
func Build(channels Channels) levyan.Builder { return builder{channels} }

type builder struct{ channels Channels }

func (b builder) Build(next levyan.Server) (levyan.Server, error) {
	return NewLayer(next, b.channels), nil
}

// WithDriver is this layer over the stack put on `drv`.
func (s Layer) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return NewLayer(next, s.Channels), nil
}

var (
	_ levyan.Server               = Layer{}
	_ enttx.Binder[levyan.Server] = Layer{}
)

func (s Layer) BillingAccount() levyan.BillingAccountServiceServer {
	return bare.BillingAccountWithAddRule(s.Next().BillingAccount(), s.admitBillingAccount)
}

func (s Layer) admitBillingAccount(ctx context.Context, req *levyan.BillingAccountAddRequest) (*levyan.BillingAccountAddRequest, error) {
	if len(req.GetOwner()) != 16 {
		return nil, pderr.Invalidf("owner", "a billing account is an account's, at roster")
	}
	if req.GetChannel() == levyan.Channel_CHANNEL_BANK_KR.String() {
		return admitBank(ctx, req)
	}
	billed, err := s.Channels.Of(req.GetCountry())
	if err != nil {
		return nil, err
	}
	if req.HasDateForgotten() {
		return nil, pderr.Invalidf("date_forgotten", "a billing account is made with what says who it is billed to")
	}

	req = proto.CloneOf(req)
	req.SetChannel(billed.Channel.String())
	req.SetCurrency(billed.Currency)

	return req, nil
}

// admitBank is what a billing account on the bank-transfer channel is
// (#9): opened by an operator, after a contract -- paying after the fact is
// credit, so it is not an account's to open itself (Claude's) -- in Korea, in
// won, billed to a business by its registration, with where its tax invoices
// go.
func admitBank(ctx context.Context, req *levyan.BillingAccountAddRequest) (*levyan.BillingAccountAddRequest, error) {
	if !Operator(ctx) {
		return nil, status.Error(codes.PermissionDenied, "a billing account paid by transfer is opened by an operator, after a contract")
	}
	if req.GetCountry() != "KR" {
		return nil, pderr.Invalidf("country", "a bank transfer with a tax invoice is in KR")
	}
	b := req.GetBusiness()
	switch {
	case b == nil || !taxinvoice.Registration(b.GetRegistration()):
		return nil, pderr.Invalidf("business", "a tax invoice is to a business by its registration number, or its 고유번호: ten digits")
	case strings.TrimSpace(b.GetName()) == "":
		return nil, pderr.Invalidf("business", "a tax invoice names the business")
	case strings.TrimSpace(b.GetEmail()) == "":
		return nil, pderr.Invalidf("business", "a tax invoice goes to an address: the business's email")
	case req.HasDateForgotten():
		return nil, pderr.Invalidf("date_forgotten", "a billing account is made with what says who it is billed to")
	}

	req = proto.CloneOf(req)
	req.SetCurrency("KRW")

	return req, nil
}

// Operator says whether `ctx` is framed as an operator, by their row: what
// the operator's entry point frames each request as.
func Operator(ctx context.Context) bool {
	f, ok := frame.From(ctx)
	if !ok {
		return false
	}
	_, ok = f.Row.(*levyan.Operator)
	return ok
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
