// Package book is the price book's rules: what a meter has to be, and what a
// version of a price may be, said of every row an Add writes.
//
// It is a layer on both stacks -- the one a caller reaches and the one the
// deployment does its own work through (`cmd.Build`) -- so a product, an
// operator and a test are all held to the same: there is no door to the price
// book that a rule is missing from. Each is a rule on the server behind
// (`bare.<E>WithAddRule`) rather than an Add overridden here, which is what
// payday's server guide asks of a rule about the row an Add writes.
//
// What is checked is what a row says about itself and the meter it prices.
// What is not checked here is the wall: an edge a caller cannot see is refused
// by the generated gate in front of this, and a price whose meter is in
// another product is refused by the sink beneath it (`agrees:`).
package book

import (
	"context"
	"errors"
	"time"

	"github.com/lesomnus/payday/meter"
	"github.com/lesomnus/payday/pderr"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/server/bare"
)

// Book is the layer.
type Book struct {
	levyan.Overlay

	// now is the deployment's clock: what "not before it is published" is
	// measured against, and when a version is published.
	now func() time.Time
}

// New is the layer over `next`, telling the time by `now`.
func New(next levyan.Server, now func() time.Time) Book {
	if now == nil {
		now = time.Now
	}

	return Book{Overlay: levyan.NewOverlay(next), now: now}
}

// Build makes a builder of this layer so that it can be stacked.
func Build(now func() time.Time) levyan.Builder { return builder{now} }

type builder struct{ now func() time.Time }

func (b builder) Build(next levyan.Server) (levyan.Server, error) { return New(next, b.now), nil }

// WithDriver is this layer over the stack put on `drv`, with its clock.
func (s Book) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return New(next, s.now), nil
}

var (
	_ levyan.Server               = Book{}
	_ enttx.Binder[levyan.Server] = Book{}
)

type bookMeter struct {
	Book
	levyan.MeterServiceServer
}

func (s Book) Meter() levyan.MeterServiceServer {
	v := bookMeter{s, s.Next().Meter()}
	v.MeterServiceServer = bare.MeterWithAddRule(v.MeterServiceServer, v.admit)
	return v
}

// admit is what a meter has to be: a key payday's meters can be filed under,
// a unit, a kind, and an aggregation the kind has -- a sum of what was held at
// moments depends on how often somebody looked, and the most of what happened
// is a rate nobody measured.
func (s bookMeter) admit(ctx context.Context, req *levyan.MeterAddRequest) (*levyan.MeterAddRequest, error) {
	if err := meter.CheckKey(req.GetKey()); err != nil {
		return nil, pderr.Invalidf("key", "%s", err)
	}
	if err := meter.CheckUnit(req.GetUnit()); err != nil {
		return nil, pderr.Invalidf("unit", "%s", err)
	}

	switch req.GetKind() {
	case levyan.MeterKind_METER_KIND_STOCK:
		switch req.GetAggregation() {
		case levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE, levyan.Aggregation_AGGREGATION_MAX, levyan.Aggregation_AGGREGATION_LAST:
		default:
			return nil, pderr.Invalidf("aggregation", "what is held comes to a time-weighted average, a maximum or the last value, and not to %s", req.GetAggregation())
		}
	case levyan.MeterKind_METER_KIND_FLOW:
		if req.GetAggregation() != levyan.Aggregation_AGGREGATION_SUM {
			return nil, pderr.Invalidf("aggregation", "what happens comes to a sum, and not to %s", req.GetAggregation())
		}
		if req.GetUnboundedZero() {
			return nil, pderr.Invalidf("unbounded_zero", "what happens is never without bound")
		}
	default:
		return nil, pderr.Invalidf("kind", "a meter is of what is held or of what happens")
	}

	return req, nil
}

type bookPrice struct {
	Book
	levyan.PriceServiceServer
}

func (s Book) Price() levyan.PriceServiceServer {
	v := bookPrice{s, s.Next().Price()}
	v.PriceServiceServer = bare.PriceWithAddRule(v.PriceServiceServer, v.admit)
	return v
}

// admit is what a version of a price may be: of a meter, on a channel, in a
// currency levyan prices in, by the unit or by bands and not both -- and from
// a moment no earlier than now, since a price never reaches back into a
// period somebody has used. It is published now, whatever the request says.
func (s bookPrice) admit(ctx context.Context, req *levyan.PriceAddRequest) (*levyan.PriceAddRequest, error) {
	m, err := s.Next().Meter().Get(ctx, levyan.MeterGetRequest_builder{
		Ref:    req.GetMeter(),
		Select: levyan.MeterSelect_builder{UnboundedZero: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, pderr.Invalidf("meter", "no such meter")
		}
		return nil, err
	}

	// To the microsecond, which is what PostgreSQL keeps of a time: what the
	// Add answers with is then what every read of the row says after it, on
	// either database.
	now := s.now().Truncate(time.Microsecond)
	req = proto.CloneOf(req)

	// When it was published is when it was, and what orders it against
	// another of its date.
	req.SetDateCreated(timestamppb.New(now))

	effective := now
	if req.HasDateEffective() {
		effective = req.GetDateEffective().AsTime().Truncate(time.Microsecond)
		if effective.Before(now) {
			return nil, pderr.Invalidf("date_effective", "%s is before now: a price does not reach back, and what a period was billed is corrected by a correction", effective.Format(time.RFC3339))
		}
	}
	existing := effective
	if req.HasDateExisting() {
		existing = req.GetDateExisting().AsTime().Truncate(time.Microsecond)
	}
	req.SetDateEffective(timestamppb.New(effective))
	req.SetDateExisting(timestamppb.New(existing))

	if len(req.GetBands().GetBands()) > 0 {
		if req.GetIncluded() != 0 || req.GetUnitPrice() != 0 || req.GetPer() != 0 {
			return nil, pderr.Invalidf("bands", "a price by bands has no unit price, nothing included and no units it is per")
		}
	} else {
		req.ClearBands()
		if req.GetPer() == 0 {
			req.SetPer(1)
		}
	}

	if err := pricing.CheckChannel(req.GetChannel()); err != nil {
		return nil, pderr.Invalidf("channel", "%s", err)
	}
	if err := money.Check(req.GetCurrency()); err != nil {
		return nil, pderr.Invalidf("currency", "%s", err)
	}
	if err := pricing.RateOf(req.GetIncluded(), req.GetUnitPrice(), req.GetPer(), req.GetBands(), m.GetUnboundedZero()).Check(); err != nil {
		var r pricing.Refusal
		if errors.As(err, &r) {
			return nil, pderr.Invalidf(r.Field, "%s", r.Why)
		}
		return nil, err
	}
	if existing.Before(effective) {
		return nil, pderr.Invalidf("date_existing", "what a version is for the customers already there is not before what it is for the new ones")
	}

	return req, nil
}
