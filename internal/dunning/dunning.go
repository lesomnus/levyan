// Package dunning is what is done of an invoice whose charge failed (#11),
// by the policy table: a row for each kind of decline (`pg.Decline`) -- when
// the invoice is charged again, how long its grace is, whether what it bills
// is restricted once the grace is over or at once, and when it is read-only
// -- and a row for a workspace nobody pays for, which is restricted as an
// unpaid invoice is (`payers.grace`, #4), so that there is one table.
//
// That a decline's kind says what follows it is the owner's (2026-10-31,
// #26). The table is Claude's design. Its values ([Defaults]) and the two
// stages of a restriction are the owner's (2026-11-01, levyan#1, open
// question 3): retried a day, three days and a week after the first failure,
// a fortnight's grace, and a workspace with no payer thirty days; restricted
// first -- nothing that would grow what it is billed for, everyday work going
// on -- and read-only sixty days after the first failure. A deployment says
// its own (`dunning.kinds`).
//
// # Two stages
//
// Restricted is the first stage: from the end of the grace, or at once.
// Read-only is the second, a span of its own from the first failure
// (`readonly_after`; of a workspace nobody pays for, from when it last had a
// payer), never sooner than the first: an app given it refuses every write
// but reading, exporting and downloading, paying, and closing or leaving the
// workspace (docs/apps.md). That each row has a span of its own for it, and
// that a workspace nobody pays for has one from when it last had a payer, is
// Claude's.
//
// # Measured from the first failure
//
// Every time a row says is a span from the invoice's first failed charge:
// its retries, each later than the one before, and its grace. The row is the
// one of its latest failure's kind -- what the payer's method is like now --
// so a retry declined as another kind moves the invoice to that kind's row,
// measured from the same first failure (an implementation decision). A
// failure that is no decline at all -- a request the channel refused -- is
// read as a decline that says no more than that it is one: the other kind.
//
// # A retry
//
// A retry is due once a retry's time has come and no charge of the invoice
// was made at or after it: one made since covers it, whatever it was made
// for, so a retry is made once however often it is asked about, and of
// several whose times passed while nothing ran, only the latest is made. A
// charge is also due as soon as the invoice's billing account has become
// collectable again since its last charge was sent -- the payer began a
// subscription anew, a payment that waited for them was made -- whatever its
// row schedules. Nothing is due while a charge of it is out.
//
// # Given up on
//
// An invoice with no retry left to make, nothing out, nothing due and its
// grace over is spent: given up on, uncollectible. It is still owed -- what
// it bills stays restricted -- and a charge that succeeds after pays it.
//
// It is pure: what is known of an invoice's charges goes in, and the times
// come out, the same however often it is asked.
package dunning

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lesomnus/levyan/internal/pg"
)

// Row is what is done of an invoice whose latest failure is of one kind, or
// of a workspace nobody pays for.
type Row struct {
	// Retries are when the invoice is charged again, each a span from its
	// first failure and each later than the one before: none is none.
	Retries []time.Duration

	// Grace is how long after its first failure an invoice may go unpaid
	// before what it bills is restricted -- unless AtOnce says sooner -- and
	// before it is given up on, once no retry is left.
	Grace time.Duration

	// AtOnce restricts what an invoice bills from its first failure on,
	// rather than once its grace is over.
	AtOnce bool

	// ReadonlyAfter is how long after its first failure an invoice may go
	// unpaid before what it bills is read-only, the second stage of its
	// restriction: never sooner than the first ([Row.Restricts]).
	ReadonlyAfter time.Duration
}

// Restricts is from when the row restricts what an invoice whose first
// charge failed at `failed` bills: at once, or once the grace is over. Of a
// workspace nobody pays for, `failed` is when it last had a payer.
func (r Row) Restricts(failed time.Time) time.Time {
	if r.AtOnce {
		return failed
	}

	return failed.Add(r.Grace)
}

// Readonly is from when the row makes what an invoice whose first charge
// failed at `failed` bills read-only: its ReadonlyAfter after it. Of a
// workspace nobody pays for, `failed` is when it last had a payer.
func (r Row) Readonly(failed time.Time) time.Time { return failed.Add(r.ReadonlyAfter) }

// Check refuses a row that says no schedule: a retry not after the failure,
// or not after the one before it, a grace less than nothing, or a read-only
// stage less than nothing after the failure or sooner than the restriction.
func (r Row) Check() error {
	if err := Retries(r.Retries); err != nil {
		return fmt.Errorf("retries: %w", err)
	}
	if r.Grace < 0 {
		return errors.New("grace: less than nothing")
	}
	if r.ReadonlyAfter < 0 {
		return errors.New("readonly_after: less than nothing")
	}

	return r.Stages()
}

// Stages refuses a row whose second stage, read-only, comes sooner than its
// first, the restriction: a ReadonlyAfter shorter than the grace a row that
// restricts after it has. A row that restricts at once restricts before any
// span after the failure.
func (r Row) Stages() error {
	if !r.AtOnce && r.ReadonlyAfter >= 0 && r.ReadonlyAfter < r.Grace {
		return fmt.Errorf("readonly_after: %s is sooner than the restriction, %s after the failure, as the grace ends: read-only is the stage after it", r.ReadonlyAfter, r.Grace)
	}

	return nil
}

// Retries refuses retries that are no schedule: each a span after the
// failure, and after the one before it.
func Retries(vs []time.Duration) error {
	for i, v := range vs {
		switch {
		case v <= 0:
			return fmt.Errorf("%s is no time after the failure", v)
		case i > 0 && v <= vs[i-1]:
			return fmt.Errorf("%s is no later than the retry before it, %s", v, vs[i-1])
		}
	}

	return nil
}

// Policy is the table: a row for each kind of decline, and one for a
// workspace nobody pays for.
type Policy struct {
	// Temporary is a decline that may go through later on the same method:
	// too little in the account, a limit reached, a subscription paused or
	// past due.
	Temporary Row

	// Replace is a method that will not be charged as it is: expired, lost,
	// none -- no subscription, or one canceled -- or a payment that waits for
	// its payer to act.
	Replace Row

	// Fraud is a charge refused as suspected fraud, or blocked by a check of
	// risk.
	Fraud Row

	// Other is a refusal that says no more than that it is one, and a failure
	// that is no decline.
	Other Row

	// Payerless is a workspace nobody has paid for since it last had a
	// payer: its grace and whether it is restricted at once. Nothing is
	// charged of it, so it retries nothing.
	Payerless Row
}

const day = 24 * time.Hour

// Defaults is the table as the owner decided it (2026-11-01, levyan#1, open
// question 3), taking Claude's proposal:
//
//	kind       retries          grace    restricted       read-only
//	temporary  1, 3 and 7 days  14 days  after the grace  60 days after
//	replace    none             14 days  after the grace  60 days after
//	fraud      none             none     at once          60 days after
//	other      as temporary
//	payerless  none             30 days  after the grace  60 days after
//
// A method to replace is retried never, its payer being told at once -- by
// the product, which shows what the table says (docs/apps.md) -- and a
// workspace nobody pays for keeps the grace `payers.grace` gave it (#4).
// Read-only is sixty days after the first failure for every kind, as the
// owner said, and for a workspace nobody pays for sixty days after it last
// had a payer, which is Claude's.
func Defaults() Policy {
	temporary := Row{Retries: []time.Duration{1 * day, 3 * day, 7 * day}, Grace: 14 * day, ReadonlyAfter: DefaultReadonlyAfter}
	other := temporary
	other.Retries = slices.Clone(temporary.Retries)

	return Policy{
		Temporary: temporary,
		Replace:   Row{Grace: 14 * day, ReadonlyAfter: DefaultReadonlyAfter},
		Fraud:     Row{AtOnce: true, ReadonlyAfter: DefaultReadonlyAfter},
		Other:     other,
		Payerless: Row{Grace: 30 * day, ReadonlyAfter: DefaultReadonlyAfter},
	}
}

// DefaultReadonlyAfter is when every row's second stage comes by default:
// sixty days after the first failure, the owner's (2026-11-01).
const DefaultReadonlyAfter = 60 * day

// Of is the row of a decline of kind `d`; one of no kind the table has is
// the other kind's.
func (p Policy) Of(d pg.Decline) Row {
	switch d {
	case pg.DeclineTemporary:
		return p.Temporary
	case pg.DeclineReplace:
		return p.Replace
	case pg.DeclineFraud:
		return p.Fraud
	default:
		return p.Other
	}
}

// Named is a row of the table, by the name the configuration says it under.
type Named struct {
	Name string
	Row  Row
}

// Rows are the table's rows, by name, in the order the configuration says
// them.
func (p Policy) Rows() []Named {
	return []Named{
		{"temporary", p.Temporary},
		{"replace", p.Replace},
		{"fraud", p.Fraud},
		{"other", p.Other},
		{"payerless", p.Payerless},
	}
}

// Check refuses a table a row of which says no schedule, and a row of a
// workspace nobody pays for that retries: nothing is charged of one.
func (p Policy) Check() error {
	for _, r := range p.Rows() {
		if err := r.Row.Check(); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
	}
	if len(p.Payerless.Retries) > 0 {
		return errors.New("payerless: retries: nothing is charged of a workspace nobody pays for")
	}

	return nil
}

// Charge is one charge of an invoice's, as dunning reads it.
type Charge struct {
	// Made is when it was written, and Sent when it was last sent: zero for
	// never.
	Made time.Time
	Sent time.Time

	// Out is whether what came of it is not settled yet: written to be sent,
	// out, or held by the channel.
	Out bool

	// Failed is when it failed, zero for one that did not, and Decline the
	// kind of its decline: [pg.DeclineOther] for a failure that is no
	// decline.
	Failed  time.Time
	Decline pg.Decline
}

// State is what the table says of an invoice still owed, at a moment.
type State struct {
	// Kind is the kind of its latest failure, whose row it is held to.
	Kind pg.Decline

	// Failed is when its first charge failed: what every other time is
	// measured from.
	Failed time.Time

	// Retry is when it is charged next: a retry whose time has come and that
	// no charge covers, or the moment its billing account became collectable
	// again -- both past, and so due -- or else the next retry to come; zero
	// for none. A retry due while a charge is out is made once that one has
	// failed.
	Retry time.Time

	// GraceEnds is when its grace is over, and Restricted from when what it
	// bills is restricted: at once, or as its grace ends. Readonly is from
	// when it is read-only, the second stage: never sooner.
	GraceEnds  time.Time
	Restricted time.Time
	Readonly   time.Time

	// Due says a charge of it is to be made now.
	Due bool

	// Spent says it is to be given up on, uncollectible: no retry left,
	// nothing out or due, and its grace over.
	Spent bool
}

// Dun is what the table says at `now` of an invoice still owed whose charges
// are `cs`, in the order they were made, and whose billing account became
// collectable at `collectable` -- zero while it is not, and on a channel that
// says nothing of it -- or false for one none of whose charges failed, which
// is in no dunning.
func (p Policy) Dun(cs []Charge, collectable, now time.Time) (State, bool) {
	var (
		first, made, sent time.Time
		latest            *Charge
		out               bool
	)
	for i := range cs {
		c := &cs[i]
		out = out || c.Out
		if !c.Failed.IsZero() {
			if first.IsZero() || c.Failed.Before(first) {
				first = c.Failed
			}
			latest = c
		}
		made = later(made, c.Made)
		if c.Sent.IsZero() {
			sent = later(sent, c.Made)
		} else {
			sent = later(sent, c.Sent)
		}
	}
	if latest == nil {
		return State{}, false
	}

	row := p.Of(latest.Decline)
	st := State{
		Kind:       latest.Decline,
		Failed:     first,
		GraceEnds:  first.Add(row.Grace),
		Restricted: row.Restricts(first),
		Readonly:   row.Readonly(first),
	}

	// The latest retry whose time has come, unless a charge was made at or
	// after it, and the first whose time is to come.
	var due, next time.Time
	for _, d := range row.Retries {
		at := first.Add(d)
		if at.After(now) {
			next = at
			break
		}
		due = at
	}
	if !due.IsZero() && !made.Before(due) {
		due = time.Time{}
	}
	again := !collectable.IsZero() && collectable.After(sent)

	switch {
	case !due.IsZero():
		st.Retry = due
	case !next.IsZero():
		st.Retry = next
	}
	if again && (st.Retry.IsZero() || collectable.Before(st.Retry)) {
		st.Retry = collectable
	}
	st.Due = !out && (!due.IsZero() || again)
	st.Spent = !out && !again && due.IsZero() && next.IsZero() && !now.Before(st.GraceEnds)

	return st, true
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Korean is a span as a grant's description says it: `30일`, `36시간`, or
// as Go writes a duration when it is neither whole days nor whole hours.
func Korean(d time.Duration) string {
	switch {
	case d > 0 && d%day == 0:
		return fmt.Sprintf("%d일", d/day)
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%d시간", d/time.Hour)
	}
	return d.String()
}
