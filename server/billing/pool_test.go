package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/benefits"
)

// What a workspace's pools take (#43), as what its periods come to: what is
// billable past what is included is taken out of them first, and only the
// rest is priced; what each period took stays what it took, said on its
// statement and on every correction of it, so that a period rated again
// takes of what the others left and never takes twice.

// fill is a pool of `quantity` of `meter` for workspace `ws`, by an
// operator, from now until a year from now.
func (w world) fill(ws pdid.Id, meter string, quantity int64) pdid.Id {
	w.t.Helper()
	v, err := w.S.Pools.Grant(w.Operator(context.Background(), "alice"), benefits.Fill{
		Product: w.product(), Workspace: ws, Meter: meter, Quantity: quantity, Reason: "자산·월 프로모션",
	})
	w.X.NoError(err)
	return pdid.Id(v.Id)
}

func pooledOf(l *levyanv1.Line) map[string]string {
	out := map[string]string{}
	for _, t := range l.GetPools() {
		out[t.GetPool()] = t.GetTaken()
	}
	return out
}

func correction(s *levyanv1.Statement, meter string) *levyanv1.Line {
	for _, l := range s.GetLines() {
		if l.GetMeter() == meter && l.GetKind() == levyanv1.LineKind_LINE_KIND_CORRECTION {
			return l
		}
	}
	return nil
}

func (w world) usage(ws pdid.Id) *levyanv1.Usage {
	w.t.Helper()
	u, err := levyanv1.NewBillingServiceClient(w.conn).GetUsage(w.ctx, levyanv1.GetUsageRequest_builder{Workspace: ws.String()}.Build())
	w.X.NoError(err)
	return u.GetUsage()
}

// A pool of 1,000 asset-months takes January's 700 past what is included,
// and February's first 300; March is priced in full. What is left of it is
// what the periods closed left.
func TestAPoolTakesWhatIsBillableBeforeItIsPriced(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	v := w.fill(ws, "loft.assets.active", 1000)

	w.At(on(1, 20))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(1, 21), 800)
	u := w.usage(ws)
	x.Len(u.GetPools(), 1)
	x.Equal(v.String(), u.GetPools()[0].GetId())
	x.Equal("loft.assets.active", u.GetPools()[0].GetMeter())
	x.Equal(int64(1000), u.GetPools()[0].GetGranted())
	x.Equal("1000", u.GetPools()[0].GetLeft())
	l := line(levyanv1.Statement_builder{Lines: u.GetLines()}.Build(), "loft.assets.active")
	x.Equal("700", l.GetPooled(), "what January would take, were it to close now")
	x.Equal("0", l.GetAmount())

	w.At(on(4, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 21), on(4, 1), 800)
	w.close(on(4, 10))

	jan := line(w.statement(ws, on(1, 1)), "loft.assets.active")
	x.Equal("700", jan.GetBillable(), "billable past what is included, as it was")
	x.Equal("700", jan.GetPooled())
	x.Equal(map[string]string{v.String(): "700"}, pooledOf(jan))
	x.Equal("0", jan.GetAmount())
	x.Equal("700", jan.GetParts()[0].GetPooled())
	x.Equal(uint32(5), w.row(ws, on(1, 1)).Rating)

	feb := line(w.statement(ws, on(2, 1)), "loft.assets.active")
	x.Equal("300", feb.GetPooled())
	x.Equal("20000", feb.GetAmount(), "400 at 50")

	mar := line(w.statement(ws, on(3, 1)), "loft.assets.active")
	x.Equal("0", mar.GetPooled())
	x.Empty(mar.GetPools())
	x.Equal("35000", mar.GetAmount())

	u = w.usage(ws)
	x.Len(u.GetPools(), 1)
	x.Equal("0", u.GetPools()[0].GetLeft())
}

// What an allowance gives is included before a pool is taken from, and a
// workspace rated from the first unit takes from its pool what its price no
// longer includes.
func TestAPoolTakesWhatIsBillablePastAllowancesAndTheFirstUnit(t *testing.T) {
	one := 1
	w := setup(t, func(c *cmd.Config) { c.Payers.Free = &one })
	x := w.X
	owner := levyantest.Account()
	first, second := levyantest.Tenant(), levyantest.Tenant()
	w.At(on(1, 2))
	w.arrive(first, owner, on(1, 1))
	w.arrive(second, owner, on(1, 1).Add(time.Hour))
	w.give(first, "loft.assets.active", 500, on(1, 1), time.Time{})
	w.fill(first, "loft.assets.active", 1000)
	w.fill(second, "loft.assets.active", 1000)

	w.At(on(2, 10))
	w.daily(first, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.daily(second, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.close(on(2, 10))

	a := line(w.statement(first, on(1, 1)), "loft.assets.active")
	x.Equal("200", a.GetPooled(), "800 past 100 and 500")
	x.Equal("0", a.GetAmount())
	b := line(w.statement(second, on(1, 1)), "loft.assets.active")
	x.True(w.row(second, on(1, 1)).FirstUnit)
	x.Equal("800", b.GetPooled(), "from the first unit")
	x.Equal("0", b.GetAmount())
}

// What a late reading changes of a period takes of what the other periods
// left -- never what it took before as well -- and what it took is said on
// the correction, though what the period comes to does not move.
func TestALateReadingChangesWhatAPeriodTookAndTakesOnce(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	v := w.fill(ws, "loft.assets.active", 1000)

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 800)
	w.close(on(2, 10))
	x.Equal("700", line(w.statement(ws, on(1, 1)), "loft.assets.active").GetPooled())

	// Twelve hours of 1,600 that should have been in January: 400/31 more
	// billable, which the pool takes too.
	w.At(on(2, 12))
	x.True(w.record(ws, on(1, 15).Add(12*time.Hour), 1600).GetLate())
	w.At(on(3, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 10), on(3, 1), 800)
	w.close(on(3, 5))

	feb := w.statement(ws, on(2, 1))
	fix := correction(feb, "loft.assets.active")
	x.NotNil(fix, "what January took moved, though what it comes to did not")
	x.Equal("0", fix.GetAmount())
	x.Equal("712.903225", fix.GetPooled(), "700 and 400/31")
	x.Equal(map[string]string{v.String(): "712.903225"}, pooledOf(fix))
	own := line(feb, "loft.assets.active")
	x.Equal("287.096774", own.GetPooled(), "what January left: 1,000 less 22,100/31")
	x.Equal("20645.16129", own.GetAmount(), "12,800/31 at 50")

	// Again, in March: January needs 400/31 more, and the pool has only
	// what February left it of the rest -- what January took is what it
	// takes, and not that again.
	w.At(on(3, 10))
	x.True(w.record(ws, on(1, 20).Add(12*time.Hour), 1600).GetLate())
	w.At(on(4, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(3, 1), on(4, 1), 800)
	w.close(on(4, 5))
	mar := w.statement(ws, on(3, 1))
	fix = correction(mar, "loft.assets.active")
	x.NotNil(fix)
	x.Equal("712.903225", fix.GetPooled(), "what was left for it")
	x.Equal("645.16129", fix.GetAmount(), "the 400/31 the pool had not, at 50")
	own = line(mar, "loft.assets.active")
	x.Equal("0", own.GetPooled(), "nothing left")
	x.Equal("35000", own.GetAmount())

	// April corrects nothing again.
	w.At(on(5, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(4, 1), on(5, 1), 800)
	w.close(on(5, 5))
	x.Nil(correction(w.statement(ws, on(4, 1)), "loft.assets.active"))
}

// A late reading that lowers what a period is billable for gives back to
// the pool what it no longer takes, for the periods after it.
func TestALateReadingThatLowersAPeriodGivesThePoolBack(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	w.fill(ws, "loft.assets.active", 1000)

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 800)
	w.close(on(2, 10))

	w.At(on(2, 12))
	x.True(w.record(ws, on(1, 15).Add(12*time.Hour), 0).GetLate())
	w.At(on(3, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 10), on(3, 1), 800)
	w.close(on(3, 5))

	feb := w.statement(ws, on(2, 1))
	fix := correction(feb, "loft.assets.active")
	x.NotNil(fix)
	x.Equal("0", fix.GetAmount())
	x.Equal("687.096774", fix.GetPooled(), "700 less 400/31")
	own := line(feb, "loft.assets.active")
	x.Equal("312.903225", own.GetPooled(), "1,000 less 21,300/31")
	x.Equal("19354.838709", own.GetAmount(), "12,000/31 at 50")
}

// A pool an operator ends is taken from by no period from the one now open,
// and what the periods before took stays taken.
func TestAPoolEndedIsTakenFromNoLonger(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	v := w.fill(ws, "loft.assets.active", 1000)

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 10), 800)
	w.close(on(2, 10))

	w.At(on(2, 15))
	got, err := w.S.Pools.Expire(w.Operator(context.Background(), "bob"), v, "프로모션 종료")
	x.NoError(err)
	x.True(got.DateExpires.Equal(on(2, 1)), "from the period now open")
	x.Equal("프로모션 종료", got.EndReason)
	x.Empty(w.usage(ws).GetPools(), "nothing February takes from")
	_, err = w.S.Pools.Expire(w.Operator(context.Background(), "bob"), v, "again")
	x.Equal(codes.FailedPrecondition, status.Code(err))

	w.At(on(3, 5))
	w.daily(ws, "loft.assets.active", "{asset}", on(2, 10), on(3, 1), 800)
	w.close(on(3, 5))
	x.Equal("700", line(w.statement(ws, on(1, 1)), "loft.assets.active").GetPooled(), "January's, as it was")
	feb := line(w.statement(ws, on(2, 1)), "loft.assets.active")
	x.Equal("0", feb.GetPooled())
	x.Equal("35000", feb.GetAmount())
}

// A meter priced by bands has no pool, and neither has anybody but an
// operator's word, nor a reason, nor nothing; one expires after it begins,
// and none reaches into a period whose close has begun.
func TestWhatAPoolIsRefused(t *testing.T) {
	w := setup(t)
	x := w.X
	ws := levyantest.Tenant()
	op := w.Operator(context.Background(), "alice")
	fill := func(ctx context.Context, f benefits.Fill) codes.Code {
		f.Product, f.Workspace = w.product(), ws
		if f.Reason == "" {
			f.Reason = "프로모션"
		}
		_, err := w.S.Pools.Grant(ctx, f)
		return status.Code(err)
	}

	x.Equal(codes.FailedPrecondition, fill(op, benefits.Fill{Meter: "loft.history.keep", Quantity: 365}), "by bands")
	x.Equal(codes.NotFound, fill(op, benefits.Fill{Meter: "loft.nothing", Quantity: 1}))
	x.Equal(codes.InvalidArgument, fill(op, benefits.Fill{Meter: "loft.assets.active", Quantity: 0}))
	x.Equal(codes.InvalidArgument, fill(op, benefits.Fill{Meter: "loft.assets.active", Quantity: 5, Reason: " "}))
	x.Equal(codes.InvalidArgument, fill(op, benefits.Fill{Meter: "loft.assets.active", Quantity: 5, From: on(3, 1), Expires: on(2, 1)}))
	x.Equal(codes.PermissionDenied, fill(context.Background(), benefits.Fill{Meter: "loft.assets.active", Quantity: 5}))

	w.At(on(2, 10))
	w.daily(ws, "loft.assets.active", "{asset}", on(1, 1), on(2, 1), 800)
	w.close(on(2, 10))
	x.Equal(codes.FailedPrecondition, fill(op, benefits.Fill{Meter: "loft.assets.active", Quantity: 5, From: on(1, 15)}))
	x.Equal(codes.OK, fill(op, benefits.Fill{Meter: "loft.assets.active", Quantity: 5}))

	vs, err := benefits.PoolsOf(context.Background(), w.S.Ent, w.id(w.loft))
	x.NoError(err)
	x.Len(vs, 1)
	x.WithinDuration(on(2, 10).AddDate(1, 0, 0), vs[0].DateExpires, time.Second, "a year, unless said")
}
