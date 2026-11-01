package benefits

import (
	"bytes"
	"context"
	"time"

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

// Layer is what an allowance may be, said of every write of one, on both
// stacks (`cmd.Build`), so that what is written past [Allowances] is still
// held to it:
//
//   - An Add gives more than nothing of a meter priced by the unit -- none of
//     whose versions is by bands, nor of a meter whose 0 is unbounded, which
//     only bands price -- from a moment no earlier than the end of the last
//     of the product's periods whose close has begun (unset is now, to the
//     microsecond), until a moment after it or never.
//   - A Patch ends it, and nothing else: its end moved to a moment no later
//     than it was, no earlier than its beginning, and in no period whose
//     close has begun -- and who ended it and why, beside.
//   - Apply is refused: a document is held to none of this.
//
// A pool (#43) is held to the same, but that it always expires -- after it
// is in force from -- and that an end moves its expiry, no earlier than the
// end of the last period whose close has begun: a period is taken from by a
// pool in force at any moment of it.
//
// What a period was billed is what it was given then, so that a period
// closed and rated again for its correction is given the same: hence no
// allowance or pool reaches into one. The rule is read on the transaction the write
// is made in, and [Allowances] holds the workspace's row first, as a close
// of it does (`ledger.Lock`), so that a close and an allowance never both
// take the other to have come first.
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

type layerAllowance struct {
	Layer
	levyan.AllowanceServiceServer
}

func (s Layer) Allowance() levyan.AllowanceServiceServer {
	v := layerAllowance{s, s.Next().Allowance()}
	v.AllowanceServiceServer = bare.AllowanceWithAddRule(v.AllowanceServiceServer, v.admit)
	return v
}

func (s layerAllowance) admit(ctx context.Context, req *levyan.AllowanceAddRequest) (*levyan.AllowanceAddRequest, error) {
	switch {
	case req.GetWorkspace() == nil:
		return nil, pderr.Invalidf("workspace", "an allowance is given to a workspace")
	case req.GetMeter() == nil:
		return nil, pderr.Invalidf("meter", "an allowance gives more of a meter")
	case req.GetQuantity() <= 0:
		return nil, pderr.Invalidf("quantity", "an allowance gives more than nothing")
	case req.GetEndedBy() != nil || req.GetEndReason() != "":
		return nil, pderr.Invalidf("ended_by", "an allowance is given before it is ended")
	}

	if err := s.byTheUnit(ctx, req.GetMeter()); err != nil {
		return nil, err
	}

	// To the microsecond, which is what PostgreSQL keeps of a time.
	req = proto.CloneOf(req)
	from := s.now().UTC().Truncate(time.Microsecond)
	if req.HasValidFrom() {
		from = req.GetValidFrom().AsTime().UTC().Truncate(time.Microsecond)
	}
	req.SetValidFrom(timestamppb.New(from))
	if req.HasValidUntil() {
		until := req.GetValidUntil().AsTime().UTC().Truncate(time.Microsecond)
		if !until.After(from) {
			return nil, pderr.Invalidf("valid_until", "an allowance ends after it begins")
		}
		req.SetValidUntil(timestamppb.New(until))
	}

	if err := s.unclosed(ctx, req.GetTenant().GetId(), from, "valid_from"); err != nil {
		return nil, err
	}

	return req, nil
}

// byTheUnit refuses meter `ref` unless it is priced by the unit: no version
// of its price is by bands, nor is it a meter whose 0 is unbounded, which
// only bands price. A band's fee is for a value held, and has nothing
// included to give more of, nor anything billable to take from a pool.
func (s Layer) byTheUnit(ctx context.Context, ref *levyan.MeterRef) error {
	m, err := s.Next().Meter().Get(ctx, levyan.MeterGetRequest_builder{
		Ref:    ref,
		Select: levyan.MeterSelect_builder{UnboundedZero: proto.Bool(true), Key: proto.Bool(true)}.Build(),
	}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return pderr.Invalidf("meter", "no such meter")
		}
		return err
	}
	if m.GetUnboundedZero() {
		return status.Errorf(codes.FailedPrecondition, "%s is priced by bands, whose 0 is above every bound: a band's fee includes nothing to give more of, and a credit is what is given there", m.GetKey())
	}
	banded, err := s.banded(ctx, m.GetId())
	if err != nil {
		return err
	}
	if banded {
		return status.Errorf(codes.FailedPrecondition, "%s is priced by bands: a band's fee includes nothing to give more of, and a credit is what is given there", m.GetKey())
	}

	return nil
}

// banded says whether a version of the price of meter `id` is by bands.
func (s Layer) banded(ctx context.Context, id []byte) (bool, error) {
	after := ""
	for {
		vs, err := s.Next().Price().List(ctx, levyan.PriceListRequest_builder{
			Filters: []*levyan.PriceFilter{levyan.PriceFilter_builder{
				Meter: levyan.MeterRef_builder{Id: id}.Build(),
			}.Build()},
			Size:  200,
			After: after,
		}.Build())
		if err != nil {
			return false, err
		}
		for _, v := range vs.GetItems() {
			if len(v.GetBands().GetBands()) > 0 {
				return true, nil
			}
		}
		if after = vs.GetNext(); after == "" {
			return false, nil
		}
	}
}

// unclosed refuses a moment `at` in a period of `product` whose close has
// begun: before the end of the last of them, which is where the periods
// that may still be rated as they are begin.
func (s Layer) unclosed(ctx context.Context, product []byte, at time.Time, field string) error {
	end, ok, err := s.closedThrough(ctx, product)
	if err != nil || !ok {
		return err
	}
	if at.Before(end) {
		return status.Errorf(codes.FailedPrecondition, "%s: %s is in a period whose close has begun, which ended at %s: what it was billed is what it was given then, and a credit note corrects it", field, at.Format(time.RFC3339), end.Format(time.RFC3339))
	}

	return nil
}

// closedThrough is the end of the last of `product`'s periods whose close
// has begun, and false when none has: the latest of its periods, which
// follow one another and are begun to close in their order.
func (s Layer) closedThrough(ctx context.Context, product []byte) (time.Time, bool, error) {
	after := ""
	for {
		vs, err := s.Next().Period().List(ctx, levyan.PeriodListRequest_builder{
			Size:    200,
			After:   after,
			Reverse: true,
		}.Build())
		if err != nil {
			return time.Time{}, false, err
		}
		for _, v := range vs.GetItems() {
			if bytes.Equal(v.GetTenant().GetId(), product) {
				return v.GetDateEnd().AsTime().UTC(), true, nil
			}
		}
		if after = vs.GetNext(); after == "" {
			return time.Time{}, false, nil
		}
	}
}

// Patch is an allowance ended: its end moved to a moment no later than it
// was, and in no period whose close has begun; and nothing else of it but
// who ended it and why.
func (s layerAllowance) Patch(ctx context.Context, req *levyan.AllowancePatchRequest) (*levyan.Allowance, error) {
	rest := proto.CloneOf(req)
	rest.ClearRef()
	rest.ClearDateUpdated()
	rest.ClearDateUpdatedForce()
	rest.ClearValidUntil()
	rest.ClearEndedBy()
	rest.ClearEndReason()
	switch {
	case proto.Size(rest) > 0:
		return nil, status.Error(codes.FailedPrecondition, "an allowance is what it was given as: only its end moves, as it is ended")
	case !req.HasValidUntil():
		return nil, pderr.Invalidf("valid_until", "an allowance is ended at a moment")
	}

	was, err := s.Next().Allowance().Get(ctx, levyan.AllowanceGetRequest_builder{
		Ref: req.GetRef(),
		Select: levyan.AllowanceSelect_builder{
			All:    proto.Bool(true),
			Tenant: &levyan.TenantSelect{},
		}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	until := req.GetValidUntil().AsTime().UTC().Truncate(time.Microsecond)
	switch {
	case until.Before(was.GetValidFrom().AsTime()):
		return nil, pderr.Invalidf("valid_until", "an allowance ends no earlier than it begins")
	case was.HasValidUntil() && until.After(was.GetValidUntil().AsTime()):
		return nil, status.Error(codes.FailedPrecondition, "an allowance's end moves no later: another is given for longer")
	}
	if err := s.unclosed(ctx, was.GetTenant().GetId(), until, "valid_until"); err != nil {
		return nil, err
	}

	req = proto.CloneOf(req)
	req.SetValidUntil(timestamppb.New(until))
	return s.AllowanceServiceServer.Patch(ctx, req)
}

func (s layerAllowance) Apply(ctx context.Context, req *levyan.AllowanceApplyRequest) (*levyan.Allowance, error) {
	return nil, status.Error(codes.FailedPrecondition, "an allowance is ended by its Patch, which holds what it may be, and never by a document")
}

type layerPool struct {
	Layer
	levyan.PoolServiceServer
}

func (s Layer) Pool() levyan.PoolServiceServer {
	v := layerPool{s, s.Next().Pool()}
	v.PoolServiceServer = bare.PoolWithAddRule(v.PoolServiceServer, v.admit)
	return v
}

func (s layerPool) admit(ctx context.Context, req *levyan.PoolAddRequest) (*levyan.PoolAddRequest, error) {
	switch {
	case req.GetWorkspace() == nil:
		return nil, pderr.Invalidf("workspace", "a pool is given to a workspace")
	case req.GetMeter() == nil:
		return nil, pderr.Invalidf("meter", "a pool is a bundle of a meter")
	case req.GetQuantity() <= 0:
		return nil, pderr.Invalidf("quantity", "a pool holds more than nothing")
	case !req.HasDateExpires():
		return nil, pderr.Invalidf("date_expires", "every pool expires")
	case req.GetEndedBy() != nil || req.GetEndReason() != "":
		return nil, pderr.Invalidf("ended_by", "a pool is given before it is ended")
	}
	if err := s.byTheUnit(ctx, req.GetMeter()); err != nil {
		return nil, err
	}

	req = proto.CloneOf(req)
	from := s.now().UTC().Truncate(time.Microsecond)
	if req.HasValidFrom() {
		from = req.GetValidFrom().AsTime().UTC().Truncate(time.Microsecond)
	}
	req.SetValidFrom(timestamppb.New(from))
	expires := req.GetDateExpires().AsTime().UTC().Truncate(time.Microsecond)
	if !expires.After(from) {
		return nil, pderr.Invalidf("date_expires", "a pool expires after it is in force from")
	}
	req.SetDateExpires(timestamppb.New(expires))

	if err := s.unclosed(ctx, req.GetTenant().GetId(), from, "valid_from"); err != nil {
		return nil, err
	}

	return req, nil
}

// Patch is a pool ended: its expiry moved to a moment no later than it was,
// no earlier than it is in force from, and in no period whose close has
// begun; and nothing else of it but who ended it and why.
func (s layerPool) Patch(ctx context.Context, req *levyan.PoolPatchRequest) (*levyan.Pool, error) {
	rest := proto.CloneOf(req)
	rest.ClearRef()
	rest.ClearDateUpdated()
	rest.ClearDateUpdatedForce()
	rest.ClearDateExpires()
	rest.ClearEndedBy()
	rest.ClearEndReason()
	switch {
	case proto.Size(rest) > 0:
		return nil, status.Error(codes.FailedPrecondition, "a pool is what it was given as: only its expiry moves, as it is ended")
	case !req.HasDateExpires():
		return nil, pderr.Invalidf("date_expires", "a pool is ended at a moment")
	}

	was, err := s.Next().Pool().Get(ctx, levyan.PoolGetRequest_builder{
		Ref: req.GetRef(),
		Select: levyan.PoolSelect_builder{
			All:    proto.Bool(true),
			Tenant: &levyan.TenantSelect{},
		}.Build(),
	}.Build())
	if err != nil {
		return nil, err
	}

	expires := req.GetDateExpires().AsTime().UTC().Truncate(time.Microsecond)
	switch {
	case expires.Before(was.GetValidFrom().AsTime()):
		return nil, pderr.Invalidf("date_expires", "a pool expires no earlier than it is in force from")
	case expires.After(was.GetDateExpires().AsTime()):
		return nil, status.Error(codes.FailedPrecondition, "a pool's expiry moves no later: another is given for longer")
	}
	if err := s.unclosed(ctx, was.GetTenant().GetId(), expires, "date_expires"); err != nil {
		return nil, err
	}

	req = proto.CloneOf(req)
	req.SetDateExpires(timestamppb.New(expires))
	return s.PoolServiceServer.Patch(ctx, req)
}

func (s layerPool) Apply(ctx context.Context, req *levyan.PoolApplyRequest) (*levyan.Pool, error) {
	return nil, status.Error(codes.FailedPrecondition, "a pool is ended by its Patch, which holds what it may be, and never by a document")
}
