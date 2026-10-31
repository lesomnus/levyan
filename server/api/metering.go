package api

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/payday/gate"

	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/server/ledger"
)

// Metering is `levyan.v1.MeteringService`: a product's readings, into the
// ledger, as the product.
type Metering struct {
	levyanv1.UnimplementedMeteringServiceServer

	// L writes through the walled stack, with the caller's frame.
	L ledger.Ledger
}

func (m Metering) Record(ctx context.Context, req *levyanv1.RecordRequest) (*levyanv1.RecordResponse, error) {
	f, err := gate.Actor(ctx)
	if err != nil {
		return nil, err
	}
	if n := len(req.GetReadings()); n > ledger.Most {
		return nil, status.Errorf(codes.InvalidArgument, "readings: %d in one call, and %d is the most", n, ledger.Most)
	}

	// A reading that says no moment is read as the epoch, which the ledger
	// refuses as none.
	rs := make([]ledger.Reading, 0, len(req.GetReadings()))
	for _, r := range req.GetReadings() {
		rs = append(rs, ledger.Reading{
			Workspace:  r.GetWorkspace(),
			Meter:      r.GetMeter(),
			MeasuredAt: r.GetMeasuredAt().AsTime(),
			Value:      r.GetValue(),
			Unit:       r.GetUnit(),
		})
	}

	vs, err := m.L.Record(ctx, f.Tenant, rs)
	if err != nil {
		return nil, err
	}

	out := make([]*levyanv1.Recorded, 0, len(vs))
	for _, v := range vs {
		out = append(out, recorded(v))
	}

	return levyanv1.RecordResponse_builder{Results: out}.Build(), nil
}

func recorded(v ledger.Result) *levyanv1.Recorded {
	r := levyanv1.Recorded_builder{Why: v.Why, Late: v.Late}.Build()
	switch v.Outcome {
	case ledger.Kept:
		r.SetOutcome(levyanv1.Outcome_OUTCOME_KEPT)
	case ledger.Already, ledger.Nothing:
		r.SetOutcome(levyanv1.Outcome_OUTCOME_ALREADY)
	case ledger.Refused:
		r.SetOutcome(levyanv1.Outcome_OUTCOME_REFUSED)
		r.SetRefusal(refusal(v.Refusal))
	default:
		panic(fmt.Sprintf("api: %s is no outcome the API has", v.Outcome))
	}

	return r
}

func refusal(r ledger.Refusal) levyanv1.Refusal {
	switch r {
	case ledger.Invalid:
		return levyanv1.Refusal_REFUSAL_INVALID
	case ledger.NoMeter:
		return levyanv1.Refusal_REFUSAL_NO_METER
	case ledger.OtherKind:
		return levyanv1.Refusal_REFUSAL_FLOW
	case ledger.Unit:
		return levyanv1.Refusal_REFUSAL_UNIT
	case ledger.Conflict:
		return levyanv1.Refusal_REFUSAL_CONFLICT
	case ledger.Gone:
		return levyanv1.Refusal_REFUSAL_DESTROYED
	}

	return levyanv1.Refusal_REFUSAL_UNSPECIFIED
}
