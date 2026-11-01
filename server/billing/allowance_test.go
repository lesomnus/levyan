package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	entaudit "github.com/lesomnus/levyan/internal/ent/audit"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/benefits"
)

// What a workspace is given of a meter on top of what its prices include
// (#43), as what its periods come to: added to what is included for the
// share of the period it is in force, the workspace's whoever pays and
// however it is priced, said on the statement's line and in what the product
// reads, and given by an operator alone.

// give is an allowance of `quantity` more of `meter` for workspace `ws`, by
// an operator, from now -- or `from` -- until `until`, or ever.
func (w world) give(ws pdid.Id, meter string, quantity int64, from, until time.Time) pdid.Id {
	w.t.Helper()
	v, err := w.S.Allowances.Give(w.Operator(context.Background(), "alice"), benefits.Give{
		Product: w.product(), Workspace: ws, Meter: meter, Quantity: quantity, From: from, Until: until, Reason: "2027 봄 프로모션",
	})
	w.X.NoError(err)
	return pdid.Id(v.Id)
}

// line is the line of `meter` on statement `s`.
func line(s *levyanv1.Statement, meter string) *levyanv1.Line {
	for _, l := range s.GetLines() {
		if l.GetMeter() == meter && l.GetKind() == levyanv1.LineKind_LINE_KIND_USAGE {
			return l
		}
	}
	return nil
}

// An allowance in force for all of January adds what it gives to what is
// included: 800 held, 100 included and 500 given, 200 at 50.
func TestAnAllowanceInForceForAWholePeriod(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	a := w.give(ws, "loft.assets.active", 500, time.Time{}, time.Time{})

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.close(on(2, 10))

	s := w.statement(ws, on(1, 1))
	l := line(s, "loft.assets.active")
	x.Equal("800", l.GetUsage())
	x.Equal("600", l.GetIncluded(), "what the price includes and what is given")
	x.Equal("500", l.GetAllowance())
	x.Equal("200", l.GetBillable())
	x.Equal("10000", l.GetAmount())
	x.Equal(int64(10000), s.GetTotal())
	x.Len(l.GetParts(), 1)
	x.Equal(int64(100), l.GetParts()[0].GetIncluded())
	x.Equal(int64(500), l.GetParts()[0].GetAllowance())
	x.Equal([]string{a.String()}, l.GetParts()[0].GetAllowances())
	x.GreaterOrEqual(w.row(ws, on(1, 1)).Rating, uint32(4), "rated by the rules that give allowances, 4 and on")

	// A workspace given nothing is rated as it was.
	other := levyantest.Tenant()
	w.At(on(3, 10))
	w.daily(other, "loft.assets.active", "{asset}", on(2, 1), on(3, 1), 800)
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 1), on(3, 1), 800)
	w.close(on(3, 10))
	o := line(w.statement(other, on(2, 1)), "loft.assets.active")
	x.Equal("0", o.GetAllowance())
	x.Equal("100", o.GetIncluded())
	x.Equal("35000", o.GetAmount())
	x.Equal("10000", line(w.statement(ws, on(2, 1)), "loft.assets.active").GetAmount(), "and February is given it again")
}

// One given on the 11th gives twenty-one of January's thirty-one days of
// itself, and one ended on the 11th of February ten of its twenty-eight;
// what the product reads of the period now open says it, and what of the
// period each was in force.
func TestAnAllowanceInForceForPartOfAPeriod(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()

	w.At(on(1, 11))
	a := w.give(ws, "loft.assets.active", 500, time.Time{}, time.Time{})

	w.At(on(1, 20))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(1, 21), 800)
	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: ws.String()}.Build())
	x.NoError(err)
	x.Len(u.GetUsage().GetAllowances(), 1)
	got := u.GetUsage().GetAllowances()[0]
	x.Equal(a.String(), got.GetId())
	x.Equal("loft.assets.active", got.GetMeter())
	x.Equal(int64(500), got.GetQuantity())
	x.True(got.GetFrom().AsTime().Equal(on(1, 11)))
	x.False(got.HasUntil())
	x.Equal("338.709677", line(levyanv1.Statement_builder{Lines: u.GetUsage().GetLines()}.Build(), "loft.assets.active").GetAllowance(), "500 * 21/31")

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 21), on(2, 11), 800)
	w.close(on(2, 10))
	l := line(w.statement(ws, on(1, 1)), "loft.assets.active")
	x.Equal("338.709677", l.GetAllowance())
	x.Equal("361.290322", l.GetBillable(), "700 * 10/31 + 200 * 21/31")
	x.Equal("18064.516129", l.GetAmount())
	x.Len(l.GetParts(), 2)
	x.Equal(int64(0), l.GetParts()[0].GetAllowance())
	x.Equal(int64(500), l.GetParts()[1].GetAllowance())

	w.At(on(2, 11))
	_, err = w.S.Allowances.End(w.Operator(context.Background(), "bob"), a, "프로모션 종료")
	x.NoError(err)
	w.At(on(3, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 11), on(3, 1), 800)
	w.close(on(3, 10))
	l = line(w.statement(ws, on(2, 1)), "loft.assets.active")
	x.Equal("178.571428", l.GetAllowance(), "500 * 10/28")
	x.Equal("26071.428571", l.GetAmount(), "200 * 10/28 + 700 * 18/28, at 50")

	_, err = w.S.Allowances.End(w.Operator(context.Background(), "carol"), a, "again")
	x.Equal(codes.FailedPrecondition, status.Code(err), "it is over already")
}

// A workspace rated from the first unit -- none of its payer's first few --
// is given what it was given: what its price includes is not its, and what
// it is given is.
func TestAWorkspaceRatedFromTheFirstUnitIsGivenWhatItWasGiven(t *testing.T) {
	one := 1
	w := setup(t, func(c *cmd.Config) { c.Payers.Free = &one })
	x := w.X
	owner := levyantest.Account()
	first, second := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 2))
	w.arrive(first, owner, on(1, 1))
	w.arrive(second, owner, on(1, 1).Add(time.Hour))
	w.give(second, "loft.assets.active", 500, on(1, 1), time.Time{})

	w.At(on(2, 10))
	w.daily(first, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.daily(second, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.close(on(2, 10))

	x.True(w.row(second, on(1, 1)).FirstUnit)
	l := line(w.statement(second, on(1, 1)), "loft.assets.active")
	x.Equal("500", l.GetIncluded(), "nothing of the price's, and all it is given")
	x.Equal("500", l.GetAllowance())
	x.Equal("15000", l.GetAmount(), "300 at 50")
	x.Equal("35000", line(w.statement(first, on(1, 1)), "loft.assets.active").GetAmount(), "the first is given nothing")
}

// A price changed during an allowance changes nothing of it: each part is
// given it on top of what that part's version includes.
func TestAPriceChangedDuringAnAllowanceChangesNothingOfIt(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	w.give(ws, "loft.assets.active", 500, time.Time{}, time.Time{})
	assets, err := w.S.Ent.Meter.Query().All(context.Background())
	x.NoError(err)
	var id []byte
	for _, m := range assets {
		if m.Key == "loft.assets.active" {
			id = m.Id[:]
		}
	}
	_, err = w.S.Ungated.Price().Add(context.Background(), levyan.PriceAddRequest_builder{
		Tenant: levyantest.Ref(w.loft), Meter: levyan.MeterRef_builder{Id: id}.Build(),
		Channel: pgkr, Currency: "KRW", Included: 200, UnitPrice: 80,
		DateEffective: timestamppb.New(on(2, 15)),
	}.Build())
	x.NoError(err)

	w.At(on(3, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 25), on(3, 1), 800)
	w.close(on(3, 5))

	l := line(w.statement(ws, on(2, 1)), "loft.assets.active")
	x.Len(l.GetParts(), 2)
	x.Equal(int64(100), l.GetParts()[0].GetIncluded())
	x.Equal(int64(500), l.GetParts()[0].GetAllowance())
	x.Equal("200", l.GetParts()[0].GetBillable())
	x.Equal(int64(200), l.GetParts()[1].GetIncluded())
	x.Equal(int64(500), l.GetParts()[1].GetAllowance())
	x.Equal("100", l.GetParts()[1].GetBillable())
	x.Equal("500", l.GetAllowance())
	x.Equal("9000", l.GetAmount(), "200 at 50 for half of it, and 100 at 80 for the other half")
}

// A meter priced by bands is given nothing: a band's fee includes nothing
// to give more of. Nor is a meter of another product's, nor one by anybody
// but an operator, nor without a reason, nor nothing.
func TestWhatAnAllowanceIsRefused(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	op := w.Operator(context.Background(), "alice")
	give := func(ctx context.Context, g benefits.Give) codes.Code {
		g.Product, g.Workspace = w.product(), ws
		if g.Reason == "" {
			g.Reason = "프로모션"
		}
		_, err := w.S.Allowances.Give(ctx, g)
		return status.Code(err)
	}

	x.Equal(codes.FailedPrecondition, give(op, benefits.Give{Meter: "loft.history.keep", Quantity: 365}), "by bands, whose 0 is forever")

	// A meter priced by bands whose 0 is nothing, the same.
	backups := w.Meter(w.loft, "loft.backups.keep", "d", stock, last)
	_, err := w.S.Ungated.Price().Add(context.Background(), levyan.PriceAddRequest_builder{
		Tenant: levyantest.Ref(w.loft), Meter: levyan.MeterRef_builder{Id: backups.GetId()}.Build(), Channel: pgkr, Currency: "KRW",
		Bands: levyan.PriceBands_builder{Bands: []*levyan.PriceBand{
			levyan.PriceBand_builder{UpTo: proto.Int64(30), Fee: 0}.Build(),
			levyan.PriceBand_builder{Fee: 5000}.Build(),
		}}.Build(),
		DateEffective: timestamppb.New(on(1, 1)),
	}.Build())
	x.NoError(err)
	x.Equal(codes.FailedPrecondition, give(op, benefits.Give{Meter: "loft.backups.keep", Quantity: 30}), "by bands")
	x.Equal(codes.NotFound, give(op, benefits.Give{Meter: "loft.nothing", Quantity: 1}))
	x.Equal(codes.InvalidArgument, give(op, benefits.Give{Meter: "loft.assets.active", Quantity: 0}))
	x.Equal(codes.InvalidArgument, give(op, benefits.Give{Meter: "loft.assets.active", Quantity: 5, Reason: " "}))
	x.Equal(codes.InvalidArgument, give(op, benefits.Give{Meter: "loft.assets.active", Quantity: 5, From: on(1, 10), Until: on(1, 5)}))
	x.Equal(codes.PermissionDenied, give(context.Background(), benefits.Give{Meter: "loft.assets.active", Quantity: 5}), "the deployment's own")
	x.Equal(codes.PermissionDenied, give(w.ctx, benefits.Give{Meter: "loft.assets.active", Quantity: 5}), "a product's key")

	// Another product's meter is not one of this product's.
	other := w.Product("other")
	w.Meter(other, "other.things", "{thing}", stock, twa)
	x.Equal(codes.NotFound, give(op, benefits.Give{Meter: "other.things", Quantity: 5}))

	// What reaches into a period whose close has begun is refused: what it
	// was billed is what it was given then.
	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.close(on(2, 10))
	x.Equal(codes.FailedPrecondition, give(op, benefits.Give{Meter: "loft.assets.active", Quantity: 5, From: on(1, 15)}))
	x.Equal(codes.OK, give(op, benefits.Give{Meter: "loft.assets.active", Quantity: 5, From: on(2, 1)}), "the period now open, from its start")

	// The trail says who gave it.
	vs, err := w.S.Ent.Allowance.Query().All(context.Background())
	x.NoError(err)
	x.Len(vs, 1)
	rec, err := w.S.Ent.Audit.Query().Where(entaudit.ObjectId(vs[0].Id)).First(context.Background())
	x.NoError(err)
	x.Equal(vs[0].GrantedById, rec.ActorId, "the operator")
	x.Equal("프로모션", vs[0].Reason)
}

// What a late reading of a period with an allowance corrects is what the
// period comes to with it: the allowance is given again as it was.
func TestALateReadingOfAPeriodWithAnAllowanceIsCorrectedWithIt(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	w.give(ws, "loft.assets.active", 500, time.Time{}, time.Time{})

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 800)
	w.close(on(2, 10))
	x.Equal(int64(10000), w.statement(ws, on(1, 1)).GetTotal())

	// Half a day of 1,600 that should have been in January: 800 more for 12
	// of its 744 hours, past what is included already, at 50.
	w.At(on(2, 12))
	x.True(w.record(ws, on(1, 15).Add(12*time.Hour), 1600).GetLate())

	w.At(on(3, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 10), on(3, 1), 800)
	w.close(on(3, 5))
	feb := w.statement(ws, on(2, 1))
	x.Equal(map[string]string{"loft.assets.active": "10000", "loft.assets.active corrects 2027-01": "645.16129"}, amounts(feb))
	for _, l := range feb.GetLines() {
		x.Equal("500", l.GetAllowance(), "%s: given as it was", l.GetMeter())
	}
}
