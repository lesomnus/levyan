package billing

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entpoint "github.com/lesomnus/levyan/internal/ent/point"
	entreading "github.com/lesomnus/levyan/internal/ent/reading"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/rating"
	"github.com/lesomnus/levyan/server/ledger"
)

// CloseLoop is the name of the loop that closes periods.
const CloseLoop = "levyan.periods.close"

// Closer closes products' periods: a statement of each workspace for each,
// written through the servers.
type Closer struct {
	// S is the stack a statement and a period are written through: the
	// deployment's own (`cmd.Server.Ungated`), as whoever closes -- the
	// deployment, or an operator by their hand.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time

	// Grace is how long after a period ends it waits for what comes late
	// before the loop closes it.
	Grace time.Duration

	// Free is how many of each payer's workspaces of a product have their
	// allowances ([Rater.Free]).
	Free int
}

func (c Closer) rater(db *ent.Client) Rater { return Rater{Db: db, Free: c.Free} }

func (c Closer) now() time.Time {
	if c.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(c.Now())
}

func (c Closer) db() *ent.Client { return ent.NewClient(ent.Driver(c.Drv)) }

// ErrUnpriced is a product whose terms price nothing, which has no period to
// close: it names no channel or no currency yet.
var ErrUnpriced = errors.New("billing: the product names no channel or no currency, so nothing of it is priced and no period of it closes")

// Due closes every period of every product that ended at least the grace ago.
// A product that is not priced yet is passed over; one that could not be
// closed is said in the log, and the others are still closed.
func (c Closer) Due(ctx context.Context) error {
	ps, err := c.db().Tenant.Query().Order(ent.Asc(enttenant.FieldId)).All(ctx)
	if err != nil {
		return err
	}

	through := c.now().Add(-c.Grace)
	var errs []error
	for _, p := range ps {
		if _, err := c.Close(ctx, p, through); err != nil && !errors.Is(err, ErrUnpriced) {
			log.From(ctx).ErrorContext(ctx, "billing: a product's periods were not closed", slog.String("product", p.Alias), slog.Any("err", err))
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Close closes every period of `product` that has ended by `through`, oldest
// first, and answers with them: a period whose close stopped half way is
// finished first, and one closed already is passed over.
func (c Closer) Close(ctx context.Context, product *ent.Tenant, through time.Time) ([]*ent.Period, error) {
	terms, err := TermsOf(product)
	if err != nil {
		return nil, err
	}
	if !terms.Priced() {
		return nil, ErrUnpriced
	}

	out := []*ent.Period{}
	for {
		p, ok, err := c.next(ctx, product.Id, terms)
		if err != nil {
			return out, err
		}
		if !ok || p.End.After(through) {
			return out, nil
		}

		row, err := c.close(ctx, product, p, terms)
		if err != nil {
			return out, fmt.Errorf("%s: %w", p.Start.Format(time.RFC3339), err)
		}
		out = append(out, row)
	}
}

// Next is the period of `product` to close next, and false when there is none
// yet or the product is priced nothing ([ErrUnpriced]): what reconciliation
// (#13) holds the loop that closes periods to.
func (c Closer) Next(ctx context.Context, product *ent.Tenant) (rating.Period, bool, error) {
	terms, err := TermsOf(product)
	if err != nil {
		return rating.Period{}, false, err
	}
	if !terms.Priced() {
		return rating.Period{}, false, nil
	}

	return c.next(ctx, product.Id, terms)
}

// next is the period of `product` to close next, and false when there is none
// yet: one begun and not finished, the one after the last closed, or the month
// its ledger begins in.
func (c Closer) next(ctx context.Context, product uuid.UUID, t Terms) (rating.Period, bool, error) {
	last, err := c.db().Period.Query().
		Where(entperiod.TenantId(product)).
		Order(ent.Desc(entperiod.FieldDateStart)).
		First(ctx)
	switch {
	case err == nil:
		if last.DateClosed == nil {
			return rating.Period{Start: ledger.Moment(last.DateStart), End: ledger.Moment(last.DateEnd)}, true, nil
		}
		return From(ledger.Moment(last.DateEnd), t.Zone), true, nil
	case !ent.IsNotFound(err):
		return rating.Period{}, false, err
	}

	first, ok, err := Begins(ctx, c.db(), product)
	if err != nil || !ok {
		return rating.Period{}, false, err
	}

	return MonthOf(first, t.Zone), true, nil
}

// Begins is when a product's ledger begins: its earliest reading or point.
func Begins(ctx context.Context, db *ent.Client, product uuid.UUID) (time.Time, bool, error) {
	var (
		at time.Time
		ok bool
	)
	r, err := db.Reading.Query().Where(entreading.TenantId(product)).Order(ent.Asc(entreading.FieldMeasuredAt)).First(ctx)
	switch {
	case err == nil:
		at, ok = ledger.Moment(r.MeasuredAt), true
	case !ent.IsNotFound(err):
		return time.Time{}, false, err
	}

	p, err := db.Point.Query().Where(entpoint.TenantId(product)).Order(ent.Asc(entpoint.FieldCountedAt)).First(ctx)
	switch {
	case err == nil:
		if v := ledger.Moment(p.CountedAt); !ok || v.Before(at) {
			at, ok = v, true
		}
	case !ent.IsNotFound(err):
		return time.Time{}, false, err
	}

	return at, ok, nil
}

// Current is the period of `product` that `now` is in: after the last that is
// closed or being, a month at a time, or the month `now` is in when none is.
func Current(ctx context.Context, db *ent.Client, product uuid.UUID, t Terms, now time.Time) (rating.Period, error) {
	last, err := db.Period.Query().
		Where(entperiod.TenantId(product)).
		Order(ent.Desc(entperiod.FieldDateStart)).
		First(ctx)
	if ent.IsNotFound(err) {
		return MonthOf(now, t.Zone), nil
	}
	if err != nil {
		return rating.Period{}, err
	}

	p := rating.Period{Start: ledger.Moment(last.DateStart), End: ledger.Moment(last.DateEnd)}
	for !now.Before(p.End) {
		p = From(p.End, t.Zone)
	}

	return p, nil
}

// close closes one period of `product`: its row, then each workspace's
// statement, then the row stamped closed.
func (c Closer) close(ctx context.Context, product *ent.Tenant, p rating.Period, t Terms) (*ent.Period, error) {
	row, err := c.begin(ctx, product, p, t)
	if err != nil {
		return nil, err
	}
	if row.DateClosed != nil {
		return row, nil
	}

	b, err := c.rater(c.db()).Book(ctx, product.Id)
	if err != nil {
		return nil, err
	}

	// What came to nothing for want of a price on a statement's channel and
	// currency, while the meter was priced on another: said once a period,
	// of each meter and terms, with how many workspaces.
	unpriced := map[Unpriced]int{}
	defer func() {
		for _, k := range slices.SortedFunc(maps.Keys(unpriced), Unpriced.compare) {
			log.From(ctx).WarnContext(ctx, "billing: a meter priced on another channel or currency than its statements' had no price on theirs for a stretch of the period, and came to nothing there",
				slog.String("product", product.Alias),
				slog.String("meter", k.Meter),
				slog.String("channel", k.Channel),
				slog.String("currency", k.Currency),
				slog.Time("period", p.Start),
				slog.Int("workspaces", unpriced[k]))
		}
	}()

	var after uuid.UUID
	for {
		ws, err := c.db().Workspace.Query().
			Where(entworkspace.TenantId(product.Id), entworkspace.IdGT(after)).
			Order(ent.Asc(entworkspace.FieldId)).
			Limit(200).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			marked, err := c.workspace(ctx, product, row, p, t, b, w)
			if err != nil {
				return nil, fmt.Errorf("workspace %s: %w", w.Id, err)
			}
			for _, k := range marked {
				unpriced[k]++
			}
			after = w.Id
		}
		if len(ws) < 200 {
			break
		}
	}

	v, err := c.S.Period().Patch(ctx, levyan.PeriodPatchRequest_builder{
		Ref:              levyan.PeriodRef_builder{Id: row.Id[:]}.Build(),
		DateClosed:       timestamppb.New(c.now()),
		DateUpdatedForce: proto.Bool(true),
	}.Build())
	if err != nil {
		return nil, err
	}
	closed := v.GetDateClosed().AsTime()
	row.DateClosed = &closed

	return row, nil
}

// begin is the period's row, written if it is not there yet: from the moment
// it is, what speaks for the period is late.
func (c Closer) begin(ctx context.Context, product *ent.Tenant, p rating.Period, t Terms) (*ent.Period, error) {
	read := func() (*ent.Period, error) {
		return c.db().Period.Query().Where(entperiod.TenantId(product.Id), entperiod.DateStart(p.Start)).Only(ctx)
	}

	row, err := read()
	if err == nil || !ent.IsNotFound(err) {
		return row, err
	}

	_, err = c.S.Period().Add(ctx, levyan.PeriodAddRequest_builder{
		Tenant:    levyan.TenantRef_builder{Id: product.Id[:]}.Build(),
		DateStart: timestamppb.New(p.Start),
		DateEnd:   timestamppb.New(p.End),
		Zone:      t.Zone.String(),
	}.Build())
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return nil, err
	}

	return read()
}

// workspace closes period `row` for workspace `w`, in a transaction that holds
// the workspace's row: its statement, when the ledger says anything of it
// over the period or late of an earlier one, and nothing when it has one.
func (c Closer) workspace(ctx context.Context, product *ent.Tenant, row *ent.Period, p rating.Period, t Terms, b Book, w *ent.Workspace) ([]Unpriced, error) {
	tdrv, tx, err := dialect.BeginTx(ctx, c.Drv)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(c.S, tdrv)
	if err != nil {
		return nil, err
	}
	db := ent.NewClient(ent.Driver(tdrv))
	if err := ledger.Lock(ctx, db, c.Dialect == config.DialectPostgres, w.Id); err != nil {
		return nil, err
	}

	had, err := db.Statement.Query().Where(entstatement.TenantId(product.Id), entstatement.WorkspaceId(w.Id), entstatement.PeriodId(row.Id)).Exist(ctx)
	if err != nil || had {
		return nil, err
	}

	// On the terms of who paid for the workspace over the period.
	r := c.rater(db)
	t, who, err := r.TermsFor(ctx, product.Id, w.Id, p, t)
	if err != nil {
		return nil, err
	}
	st, err := Write(ctx, r, b, product.Id, w.Id, p, t)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, nil
	}

	req := levyan.StatementAddRequest_builder{
		Tenant:    levyan.TenantRef_builder{Id: product.Id[:]}.Build(),
		Workspace: levyan.WorkspaceRef_builder{Id: w.Id[:]}.Build(),
		Period:    levyan.PeriodRef_builder{Id: row.Id[:]}.Build(),
		DateStart: timestamppb.New(p.Start),
		DateEnd:   timestamppb.New(p.End),
		Channel:   t.Channel.String(),
		Currency:  t.Currency,
		Lines:     levyan.StatementLines_builder{Lines: st.Lines}.Build(),
		Total:     st.Total,
		Exact:     fraction(st.Exact),
		Rating:    rating.Version,
		FirstUnit: proto.Bool(t.FirstUnit),
	}.Build()
	if who.Link != nil {
		req.SetPayer(who.Link.Payer[:])
	}
	if who.Account != nil {
		req.SetBillingAccount(levyan.BillingAccountRef_builder{Id: who.Account.Id[:]}.Build())
	}
	if _, err := s.Statement().Add(ctx, req); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	marked := []Unpriced{}
	for _, l := range st.Lines {
		if len(l.GetUnpriced()) > 0 && l.GetKind() == levyan.LineKind_LINE_KIND_USAGE {
			marked = append(marked, Unpriced{Meter: l.GetKey(), Channel: t.Channel.String(), Currency: t.Currency})
		}
	}

	return marked, nil
}

// Unpriced is a meter that had no price on a statement's channel and
// currency for a stretch while it had one on another.
type Unpriced struct {
	Meter    string
	Channel  string
	Currency string
}

func (u Unpriced) compare(v Unpriced) int {
	return cmp.Or(cmp.Compare(u.Meter, v.Meter), cmp.Compare(u.Channel, v.Channel), cmp.Compare(u.Currency, v.Currency))
}

// Statement is what a workspace's period comes to: its lines -- of its own
// usage, then of corrections -- and their total.
type Statement struct {
	Lines []*levyan.StatementLine

	// Exact is the lines' amounts added, and Total that rounded once, down.
	Exact *big.Rat
	Total int64

	// Skipped are periods a late row speaks for whose correction is in
	// another channel or currency than this statement, and waits.
	Skipped []*ent.Period
}

// Write is what workspace `ws`'s statement of `p` says, read from the ledger
// through `r`, and nil when it would say nothing: no usage in the period, and
// nothing late of an earlier one to correct.
func Write(ctx context.Context, r Rater, b Book, product, ws uuid.UUID, p rating.Period, t Terms) (*Statement, error) {
	return compose(ctx, r, b, product, ws, p, t, time.Time{})
}

// Estimate is what workspace `ws`'s statement of `p` would say were the
// period to close with the ledger as it is now, its latest readings held to
// the period's end: what a customer is told the period comes to so far.
func Estimate(ctx context.Context, r Rater, b Book, product, ws uuid.UUID, p rating.Period, t Terms) (*Statement, error) {
	st, err := compose(ctx, r, b, product, ws, p, t, p.End)
	if st == nil && err == nil {
		st = &Statement{Exact: new(big.Rat)}
	}
	return st, err
}

func compose(ctx context.Context, r Rater, b Book, product, ws uuid.UUID, p rating.Period, t Terms, until time.Time) (*Statement, error) {
	// What the workspace's pools have left (#43), taken from in the order
	// the periods are rated: the corrected ones, oldest first, and then this
	// one, each of what those before it left.
	taps, err := r.Taps(ctx, product, ws)
	if err != nil {
		return nil, err
	}

	var (
		cs      []Correction
		skipped []*ent.Period
	)
	if t.Priced() {
		if cs, skipped, err = r.Corrections(ctx, b, product, ws, p.Start, t, taps); err != nil {
			return nil, err
		}
	}
	lines, err := r.Lines(ctx, b, product, ws, p, t, until, taps)
	if err != nil {
		return nil, err
	}
	for _, pr := range skipped {
		log.From(ctx).ErrorContext(ctx, "billing: a correction is in another channel or currency than the statement it would be on, and waits",
			slog.String("workspace", ws.String()), slog.Time("period", pr.DateStart))
	}
	if len(lines) == 0 && len(cs) == 0 {
		return nil, nil
	}

	out := &Statement{Skipped: skipped}
	amounts := []*big.Rat{}
	for _, l := range lines {
		out.Lines = append(out.Lines, Line(l))
		amounts = append(amounts, l.Amount)
	}
	for _, c := range cs {
		out.Lines = append(out.Lines, Corrects(c))
		amounts = append(amounts, c.Moves)
	}
	if out.Exact, out.Total, err = rating.Total(amounts...); err != nil {
		return nil, err
	}

	return out, nil
}

// ClosePeriods is the loop that closes every period that has waited out its
// grace, every `every`. A pass that fails is said in the log, and the next
// one goes on where it stopped: a close is twice the same as once.
func ClosePeriods(c Closer, every time.Duration) spin.Spinner {
	return spin.Called(CloseLoop, spin.Every(every, func(ctx context.Context) error {
		if err := c.Due(ctx); err != nil {
			log.From(ctx).WarnContext(ctx, "billing: closing periods", slog.Any("err", err))
		}
		return nil
	}))
}
