// Package reconcile is levyan's backstop (#13, docs/design.md §15, "대조와
// 경보"): it holds what levyan keeps to what the channels answer, and to
// itself, and says to an operator what has drifted -- a finding
// ([levyan.Finding]) of each, in the log at its severity and in a gauge a
// dashboard alerts on.
//
// # The backstop, not the path
//
// Money is assumed to be told of late, twice, out of order or never
// (levyan#1, "정합성 원칙"), and the paths that move it are built to hold
// all the same: an attempt is written down before it is sent and asked about
// before it is sent again, a notice is a hint that is asked again, an older
// answer never undoes a newer one. Reconciliation is what finds what those
// paths could not -- a key the merchant of record charged twice whose notice
// never came, a subscription cancelled at the merchant that levyan was never
// told of, a state left where it should have moved on -- and says it.
//
// # It changes no money
//
// A pass asks the channel again where that is what is wanted, through the
// paths that ask it anyway, which are idempotent: an attempt nobody knows the
// outcome of is asked about ([invoices.Invoices.Ask]), a mirror of a
// subscription not checked in a while is asked of again
// ([subscriptions.Subscriptions.Observe]). What moves money, or what a
// record says of it, is an operator's correction through the operator's
// entry point, on the trail as theirs: a finding accepted with a reason
// ([Reconciler.Accept]), what a key was charged twice given back
// ([Reconciler.Refund]), an attempt settled from the channel's answer
// ([Reconciler.Settle]), a rule followed again ([Reconciler.Rule]).
//
// # A finding is kept once
//
// Each finding is of a kind and a subject -- the row it is about -- and is
// written once: changed when what it says changes, resolved when a pass no
// longer finds it, opened again when one does. A check that could not be
// made resolves nothing of its own, and is a finding itself. Findings are
// the deployment's own: global, no product's and no payer's, and in no
// policy's table.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entfinding "github.com/lesomnus/levyan/internal/ent/finding"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/merchant"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/subscriptions"
)

// Loop is the name of the loop that reconciles.
const Loop = "levyan.reconcile"

// The gauges a dashboard alerts on.
const (
	// FindingsGauge is how many findings are open, by kind and severity:
	// every kind, none included, so that it goes back down.
	FindingsGauge = "levyan.reconcile.findings"

	// RanGauge is when the last pass ended, in seconds since the epoch:
	// what says reconciliation has stopped.
	RanGauge = "levyan.reconcile.ran"
)

// The defaults of what is stale, when the configuration says nothing.
const (
	DefaultEvery     = 24 * time.Hour
	DefaultWindow    = 7 * 24 * time.Hour
	DefaultUnknown   = time.Hour
	DefaultPending   = 72 * time.Hour
	DefaultPeriod    = 24 * time.Hour
	DefaultDraft     = 24 * time.Hour
	DefaultStatement = 24 * time.Hour
	DefaultNotice    = time.Hour
	DefaultMirror    = 7 * 24 * time.Hour
)

// DefaultPace is how long a pass waits between two requests of the merchant
// of record's: half of what Paddle lets an address ask, which leaves the rest
// to the loops that collect.
const DefaultPace = 500 * time.Millisecond

// Stale is how long each state is left before it is a finding.
type Stale struct {
	// Unknown is how long after it was last sent -- or written, for one never
	// sent -- an attempt may be requested or unknown; Pending, how long it may
	// be pending: the channel holding it, a refund waiting for the merchant's
	// approval.
	Unknown time.Duration
	Pending time.Duration

	// Period is how long after its end and its grace a period may be left
	// unclosed; Draft, how long after its last period closed a draft may be
	// left undecided; Statement, how long a statement with a billing account
	// may be on no invoice.
	Period    time.Duration
	Draft     time.Duration
	Statement time.Duration

	// Notice is how long a notice of the merchant's may wait to be taken;
	// Mirror, how long a live subscription's mirror may go unchecked.
	Notice time.Duration
	Mirror time.Duration
}

// Reconciler holds levyan to the channels and to itself.
type Reconciler struct {
	// S is the stack findings are written through: the deployment's own
	// (`cmd.Server.Ungated`), as whoever the context is framed as -- the
	// deployment's loop, or an operator by their row.
	S levyan.Server

	// Drv is what is read, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time

	// Invoices is the ledger: what an attempt is asked of, and what a
	// refund of an extra payment is written in.
	Invoices *invoices.Invoices

	// Grants is what the rules are, and how a rule is followed again; Renew
	// how often the loop that keeps them runs (`grants.renew`), which a grant
	// is held to having been given or ended by.
	Grants *grants.Grants
	Renew  time.Duration

	// Closer is how periods are closed: what is next of a product, and its
	// grace.
	Closer billing.Closer

	// Subscriptions is the merchant of record's mirrors, and its Channel the
	// merchant: nil when the deployment names none, and nothing is held to
	// one. Merchant is its collector, which settles what a transaction says.
	Subscriptions subscriptions.Subscriptions
	Merchant      *merchant.Collector

	// Window is how far back the merchant's transactions are read, by when
	// they last changed, and its adjustments, by when they were made, each
	// with its transaction: what was billed or adjusted in it is held to the
	// ledger, and every finding open is held to it again wherever it is.
	Window time.Duration

	// Recent is how long ago a workspace's reading may be for it to be
	// expected to read still (`readings.recent`).
	Recent time.Duration

	// Stale is how long each state is left before it is a finding.
	Stale Stale

	// Pace is how long a pass waits between two requests of the merchant's.
	Pace time.Duration
}

func (r *Reconciler) now() time.Time {
	if r.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(r.Now())
}

func (r *Reconciler) db() *ent.Client { return ent.NewClient(ent.Driver(r.Drv)) }

// channel is the merchant of record, or nil for none.
func (r *Reconciler) channel() mor.Channel { return r.Subscriptions.Channel }

func or(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (r *Reconciler) window() time.Duration { return or(r.Window, DefaultWindow) }

// kind is what is said of every finding of a kind: how much it asks of an
// operator, and what they do about it.
type kind struct {
	severity   levyan.FindingSeverity
	correction levyan.FindingCorrection
}

var (
	warning  = levyan.FindingSeverity_FINDING_SEVERITY_WARNING
	errored  = levyan.FindingSeverity_FINDING_SEVERITY_ERROR
	critical = levyan.FindingSeverity_FINDING_SEVERITY_CRITICAL

	investigate = levyan.FindingCorrection_FINDING_CORRECTION_INVESTIGATE
)

// kinds is what each kind of finding is.
var kinds = map[levyan.FindingKind]kind{
	levyan.FindingKind_FINDING_KIND_CHARGE_UNMATCHED:      {errored, investigate},
	levyan.FindingKind_FINDING_KIND_KEY_UNKNOWN:           {critical, investigate},
	levyan.FindingKind_FINDING_KIND_TWICE:                 {critical, levyan.FindingCorrection_FINDING_CORRECTION_REFUND},
	levyan.FindingKind_FINDING_KIND_ATTEMPT_STALE:         {warning, levyan.FindingCorrection_FINDING_CORRECTION_SETTLE},
	levyan.FindingKind_FINDING_KIND_REFUND_UNMATCHED:      {errored, investigate},
	levyan.FindingKind_FINDING_KIND_ADJUSTMENT_UNKNOWN:    {errored, investigate},
	levyan.FindingKind_FINDING_KIND_INVOICE_PAID_DIFFERS:  {errored, investigate},
	levyan.FindingKind_FINDING_KIND_INVOICE_STATE_DIFFERS: {errored, investigate},
	levyan.FindingKind_FINDING_KIND_CREDIT_DIFFERS:        {errored, investigate},
	levyan.FindingKind_FINDING_KIND_REFUND_NOT_MADE:       {errored, investigate},
	levyan.FindingKind_FINDING_KIND_STATEMENT_UNINVOICED:  {errored, investigate},
	levyan.FindingKind_FINDING_KIND_LINES_DIFFER:          {errored, investigate},
	levyan.FindingKind_FINDING_KIND_GRANT_MISSING:         {errored, levyan.FindingCorrection_FINDING_CORRECTION_RULE},
	levyan.FindingKind_FINDING_KIND_GRANT_LEFT:            {errored, levyan.FindingCorrection_FINDING_CORRECTION_RULE},
	levyan.FindingKind_FINDING_KIND_PERIOD_UNCLOSED:       {warning, levyan.FindingCorrection_FINDING_CORRECTION_CLOSE},
	levyan.FindingKind_FINDING_KIND_DRAFT_STALE:           {warning, investigate},
	levyan.FindingKind_FINDING_KIND_NOTICE_STUCK:          {warning, levyan.FindingCorrection_FINDING_CORRECTION_REASK},
	levyan.FindingKind_FINDING_KIND_MIRROR_STALE:          {warning, levyan.FindingCorrection_FINDING_CORRECTION_REASK},
	levyan.FindingKind_FINDING_KIND_READINGS_MISSING:      {warning, investigate},
	levyan.FindingKind_FINDING_KIND_CHECK_FAILED:          {errored, investigate},
	levyan.FindingKind_FINDING_KIND_OVERCOLLECTED:         {errored, investigate},
}

// SeverityOf is how much a finding of kind `k` asks of an operator.
func SeverityOf(k levyan.FindingKind) levyan.FindingSeverity { return kinds[k].severity }

// CorrectionOf is what an operator does about a finding of kind `k`.
func CorrectionOf(k levyan.FindingKind) levyan.FindingCorrection { return kinds[k].correction }

// KindOf is the kind a finding's row names, by its name.
func KindOf(name string) (levyan.FindingKind, bool) {
	v, ok := levyan.FindingKind_value[name]
	if !ok || v == 0 {
		return 0, false
	}

	return levyan.FindingKind(v), true
}

// Found is what a check finds of one subject, before it is written.
type Found struct {
	Kind    levyan.FindingKind
	Subject string

	// Product is the product it is of: zero for none.
	Product uuid.UUID

	// Summary is what it says in a sentence, and Facts what it says as a
	// pass compares it with the next: neither holds a moment that moves on
	// by itself, so that what is still so is not written again.
	Summary string
	Facts   string

	Rows     []Row
	Amount   int64
	Currency string

	// Resolved is a finding put right by the pass that found it -- a mirror
	// behind the merchant, asked again -- written as it was found and as it
	// was put right at once, with Resolution saying how.
	Resolved   bool
	Resolution string
}

// Row is a row a finding is about.
type Row struct {
	Entity string
	Ref    string
}

// RowOf is levyan's row `id` of `entity`.
func RowOf(entity string, id uuid.UUID) Row { return Row{Entity: entity, Ref: pdid.Id(id).String()} }

// Subject is what a finding of levyan's row `id` of `entity` is about.
func Subject(entity string, id uuid.UUID) string { return entity + "/" + pdid.Id(id).String() }

// check is one of what a pass holds levyan to.
type check struct {
	name  string
	kinds []levyan.FindingKind

	// merchant says it holds levyan to the merchant of record, and is not
	// made with none.
	merchant bool

	run func(r *Reconciler, ctx context.Context, p *pass) ([]Found, error)
}

// checks is every check, in the order a pass makes them: what asks the
// channel again before what reads what that wrote.
var checks = []check{
	{name: "mirrors", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_MIRROR_STALE}, merchant: true, run: (*Reconciler).mirrors},
	{name: "notices", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_NOTICE_STUCK}, merchant: true, run: (*Reconciler).notices},
	{name: "attempts", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_ATTEMPT_STALE}, run: (*Reconciler).attempts},
	{name: "merchant", kinds: []levyan.FindingKind{
		levyan.FindingKind_FINDING_KIND_CHARGE_UNMATCHED,
		levyan.FindingKind_FINDING_KIND_KEY_UNKNOWN,
		levyan.FindingKind_FINDING_KIND_TWICE,
		levyan.FindingKind_FINDING_KIND_REFUND_UNMATCHED,
		levyan.FindingKind_FINDING_KIND_ADJUSTMENT_UNKNOWN,
	}, merchant: true, run: (*Reconciler).merchant},
	{name: "invoices", kinds: []levyan.FindingKind{
		levyan.FindingKind_FINDING_KIND_INVOICE_PAID_DIFFERS,
		levyan.FindingKind_FINDING_KIND_INVOICE_STATE_DIFFERS,
		levyan.FindingKind_FINDING_KIND_OVERCOLLECTED,
		levyan.FindingKind_FINDING_KIND_REFUND_NOT_MADE,
		levyan.FindingKind_FINDING_KIND_LINES_DIFFER,
	}, run: (*Reconciler).invoices},
	{name: "credit", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_CREDIT_DIFFERS}, run: (*Reconciler).credit},
	{name: "statements", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_STATEMENT_UNINVOICED}, run: (*Reconciler).statements},
	{name: "grants", kinds: []levyan.FindingKind{
		levyan.FindingKind_FINDING_KIND_GRANT_MISSING,
		levyan.FindingKind_FINDING_KIND_GRANT_LEFT,
	}, run: (*Reconciler).grants},
	{name: "periods", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_PERIOD_UNCLOSED}, run: (*Reconciler).periods},
	{name: "drafts", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_DRAFT_STALE}, run: (*Reconciler).drafts},
	{name: "readings", kinds: []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_READINGS_MISSING}, run: (*Reconciler).readings},
}

// Checks is the name of every check a pass makes, in its order.
func Checks() []string {
	out := []string{}
	for _, c := range checks {
		out = append(out, c.name)
	}

	return out
}

// pass is what one pass knows as it goes.
type pass struct {
	now time.Time
	db  *ent.Client

	// open is the findings that were found and are not resolved, of each
	// kind, by subject: what a check holds to again wherever it is.
	open map[levyan.FindingKind]map[string]*ent.Finding

	// asked is how many requests of the merchant's this pass has made.
	asked int

	// What more than one check reads of the ledger, read once.
	ledger *ledgerRows
	stmts  *statementRows

	statements Statements
}

// subjects is the subjects of the findings of `k` that are open or accepted.
func (p *pass) subjects(k levyan.FindingKind) []string {
	out := []string{}
	for s := range p.open[k] {
		out = append(out, s)
	}
	slices.Sort(out)

	return out
}

// Report is what a pass found, and what it did of it.
type Report struct {
	At time.Time

	// Checks is each check made, or passed over, in order.
	Checks []Checked

	// Found is how many findings are so as the pass ends: open or accepted.
	// New is how many were written as new, Changed how many said something
	// other than they did, and Resolved how many were found no longer.
	Found    int
	Open     int
	New      int
	Changed  int
	Resolved int

	// Statements is how many statements with a billing account are on one
	// invoice, on none, and on more than one.
	Statements Statements
}

// Checked is one check of a pass.
type Checked struct {
	Name string

	// Skipped is a check of the merchant of record's, with none named.
	Skipped bool

	// Found is how many it found, and Err why it could not be made.
	Found int
	Err   string
}

// Statements is how many statements with a billing account are on one
// invoice, on none, and on more than one: the invariant every invoice keeps
// (`InvoiceLine` is unique by its statement), counted all the same.
type Statements struct {
	WithAccount int
	OnOne       int
	OnNone      int
	OnMore      int
}

// Run is one pass: every check made, and what each found written --
// as new, as changed, or resolved -- as whoever `ctx` is framed as; said in
// the log, a line at its severity for each finding that is so; and the gauges
// recorded. A check that fails is a finding of its own, and what it found
// before is left as it was.
func (r *Reconciler) Run(ctx context.Context) (Report, error) {
	p := &pass{now: r.now(), db: r.db()}
	out := Report{At: p.now}

	open, err := p.db.Finding.Query().
		Where(entfinding.StateIn(int32(levyan.FindingState_FINDING_STATE_OPEN), int32(levyan.FindingState_FINDING_STATE_ACCEPTED))).
		All(ctx)
	if err != nil {
		return out, err
	}
	p.open = map[levyan.FindingKind]map[string]*ent.Finding{}
	for _, f := range open {
		k, ok := KindOf(f.Kind)
		if !ok {
			continue
		}
		if p.open[k] == nil {
			p.open[k] = map[string]*ent.Finding{}
		}
		p.open[k][f.Subject] = f
	}

	var errs []error
	failed := []Found{}
	ran := map[string]bool{}
	for _, c := range checks {
		if c.merchant && r.channel() == nil {
			out.Checks = append(out.Checks, Checked{Name: c.name, Skipped: true})
			continue
		}

		found, err := c.run(r, ctx, p)
		if err != nil {
			log.From(ctx).ErrorContext(ctx, "reconcile: a check could not be made, and what it found before is left as it was", slog.String("check", c.name), slog.Any("err", err))
			out.Checks = append(out.Checks, Checked{Name: c.name, Err: err.Error()})
			failed = append(failed, Found{
				Kind:    levyan.FindingKind_FINDING_KIND_CHECK_FAILED,
				Subject: "check/" + c.name,
				Summary: fmt.Sprintf("the %s check could not be made: %s", c.name, firstLine(err)),
				Facts:   firstLine(err),
			})
			continue
		}
		ran[c.name] = true
		out.Checks = append(out.Checks, Checked{Name: c.name, Found: len(found)})

		w, err := r.write(ctx, p, c.kinds, found)
		out.New, out.Changed, out.Resolved = out.New+w.added, out.Changed+w.changed, out.Resolved+w.resolved
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}

	// A check that failed is a finding until it is made again: of those that
	// were made, what it said before is resolved.
	keep := slices.Clone(failed)
	for s, f := range p.open[levyan.FindingKind_FINDING_KIND_CHECK_FAILED] {
		name := strings.TrimPrefix(s, "check/")
		if ran[name] || slices.ContainsFunc(failed, func(v Found) bool { return v.Subject == s }) {
			continue
		}
		// A check that was passed over -- the merchant of record is named
		// no longer -- says nothing of whether it would be made now.
		keep = append(keep, Found{Kind: levyan.FindingKind_FINDING_KIND_CHECK_FAILED, Subject: s, Summary: f.Summary, Facts: f.Facts})
	}
	w, err := r.write(ctx, p, []levyan.FindingKind{levyan.FindingKind_FINDING_KIND_CHECK_FAILED}, keep)
	out.New, out.Changed, out.Resolved = out.New+w.added, out.Changed+w.changed, out.Resolved+w.resolved
	if err != nil {
		errs = append(errs, err)
	}
	out.Statements = p.statements

	if err := r.say(ctx, &out); err != nil {
		errs = append(errs, err)
	}

	return out, errors.Join(errs...)
}

// say is what a pass found, in the log and in the gauges: a line for each
// finding that is so, at its severity, and a line of the whole.
func (r *Reconciler) say(ctx context.Context, out *Report) error {
	vs, err := r.db().Finding.Query().
		Where(entfinding.StateIn(int32(levyan.FindingState_FINDING_STATE_OPEN), int32(levyan.FindingState_FINDING_STATE_ACCEPTED))).
		Order(ent.Asc(entfinding.FieldDateCreated), ent.Asc(entfinding.FieldId)).
		All(ctx)
	if err != nil {
		return err
	}

	counts := map[levyan.FindingKind]int64{}
	for _, f := range vs {
		out.Found++
		k, _ := KindOf(f.Kind)
		if f.State != int32(levyan.FindingState_FINDING_STATE_OPEN) {
			continue
		}
		out.Open++
		counts[k]++
		level := slog.LevelWarn
		if f.Severity != int32(warning) {
			level = slog.LevelError
		}
		log.From(ctx).Log(ctx, level, "reconcile: "+f.Summary,
			slog.String("finding", pdid.Id(f.Id).String()),
			slog.String("kind", short(f.Kind)),
			slog.String("severity", severityName(levyan.FindingSeverity(f.Severity))),
			slog.String("subject", f.Subject),
			slog.String("correction", correctionName(levyan.FindingCorrection(f.Correction))),
		)
	}

	g := otx.Int64Gauge(ctx, FindingsGauge,
		metric.WithDescription("findings of reconciliation that are open, by kind and severity: what levyan keeps that has drifted from what a channel answers, or from itself"),
		metric.WithUnit("{finding}"),
	)
	for k, v := range kinds {
		g.Record(ctx, counts[k], metric.WithAttributes(
			attribute.String("levyan.finding.kind", short(k.String())),
			attribute.String("levyan.finding.severity", severityName(v.severity)),
		))
	}
	otx.Int64Gauge(ctx, RanGauge,
		metric.WithDescription("when the last pass of reconciliation ended"),
		metric.WithUnit("s"),
	).Record(ctx, r.now().Unix())

	attrs := []any{
		slog.Int("open", out.Open),
		slog.Int("accepted", out.Found-out.Open),
		slog.Int("new", out.New),
		slog.Int("changed", out.Changed),
		slog.Int("resolved", out.Resolved),
		slog.Int("statements", out.Statements.WithAccount),
		slog.Int("statements_on_none", out.Statements.OnNone),
		slog.Int("statements_on_more", out.Statements.OnMore),
	}
	for _, c := range out.Checks {
		if c.Err != "" {
			attrs = append(attrs, slog.String("failed", c.Name))
		}
	}
	log.From(ctx).InfoContext(ctx, "reconcile: a pass", attrs...)

	return nil
}

// Reconciles is the loop [Loop]: a pass every `every`, in one process, as
// the deployment. A pass that fails is said in the log, and the next makes
// every check again.
func Reconciles(r *Reconciler, every time.Duration) spin.Spinner {
	return spin.Called(Loop, spin.Every(or(every, DefaultEvery), func(ctx context.Context) error {
		if _, err := r.Run(ctx); err != nil {
			log.From(ctx).WarnContext(ctx, "reconcile: a pass", slog.Any("err", err))
		}
		return nil
	}))
}

// pace waits before a request of the merchant's but the first of a pass,
// so that a pass asks no faster than [Reconciler.Pace].
func (r *Reconciler) pace(ctx context.Context, p *pass) error {
	p.asked++
	if p.asked == 1 || r.Pace <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.Pace):
		return nil
	}
}

// short is a kind's name as an operator reads it: `twice`.
func short(name string) string {
	return strings.ToLower(strings.TrimPrefix(name, "FINDING_KIND_"))
}

func severityName(s levyan.FindingSeverity) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "FINDING_SEVERITY_"))
}

func correctionName(c levyan.FindingCorrection) string {
	return strings.ToLower(strings.TrimPrefix(c.String(), "FINDING_CORRECTION_"))
}

// firstLine is what `err` says, on one line.
func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	return s
}
