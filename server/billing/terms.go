// Package billing is what a workspace's period comes to, from the ledger and
// the price book, and the close that writes it down: one statement of each
// workspace and period, never changed, with what came late corrected on a
// later one (#6).
//
// # Periods
//
// A product's period is a calendar month in its zone ([Terms.Zone]), and its
// periods follow one another: each begins where the one before it ended,
// whatever the zone was then, so that a zone changed is a change from the next
// period on, with neither a gap nor an overlap. The first is the month the
// product's ledger begins in.
//
// # Closing
//
// A period is closed once it has waited out a grace for late data
// (`periods.grace`): by the loop `levyan.periods.close`, or by an operator's
// hand. Its row is written first, and from then on what speaks for it is late
// (`server/ledger`). Then each of the product's workspaces is closed for it in
// a transaction of its own that holds the workspace's row: its statement, if
// the ledger says anything of it in the period or something late of an earlier
// one, written once. Then the period is stamped closed. Closing again finds
// what is written and writes nothing, so twice is once, and one that stopped
// half way goes on where it stopped.
//
// # Corrections
//
// A statement is never changed. What a closed period comes to now -- rated
// again from the ledger as it is -- less what was billed for it, statement and
// corrections, is a correction line on the next statement written, naming the
// period it corrects. Which periods to rate again is what came late says: each
// late row names the periods it speaks for. A correction already made is found
// by the same arithmetic, and comes to nothing.
//
// # Who pays
//
// A workspace's period is billed to its payer over it ([Rater.PayerOf]), on
// the channel and in the currency of the billing account they paid with, and
// on the product's ([Terms]) when they have none or nobody paid (#4). What a
// workspace's prices include is its own when it is one of its payer's first
// few of the product, and when nobody paid for it; otherwise it is rated from
// the first unit.
package billing

import (
	"errors"
	"fmt"
	"time"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/internal/rating"
)

// Terms is how a product is billed: the zone its periods are months in, and
// the channel and currency its statements are priced on and in -- a
// workspace's, once its payer's billing account says ([Rater.TermsFor]).
type Terms struct {
	Zone     *time.Location
	Channel  levyan.Channel
	Currency string

	// FirstUnit is a workspace rated from the first unit, what its prices
	// include not being its: none of its payer's first few (#4).
	FirstUnit bool
}

// Priced says whether the terms price anything: a product that names no
// channel or no currency yet is not billed, and its periods are not closed.
func (t Terms) Priced() bool {
	return t.Channel != levyan.Channel_CHANNEL_UNSPECIFIED && t.Currency != ""
}

// TermsOf is a product's terms, as its row says them.
func TermsOf(t *ent.Tenant) (Terms, error) {
	zone, err := Zone(t.Zone)
	if err != nil {
		return Terms{}, err
	}

	out := Terms{Zone: zone, Currency: t.Currency}
	if t.Channel != "" {
		if out.Channel, err = Channel(t.Channel); err != nil {
			return Terms{}, err
		}
	}
	if t.Currency != "" {
		if err := money.Check(t.Currency); err != nil {
			return Terms{}, err
		}
	}

	return out, nil
}

// Zone is the zone a product's name for one says: an IANA name, and UTC for
// none. The zone of the machine a process runs on is no product's.
func Zone(name string) (*time.Location, error) {
	switch name {
	case "":
		return time.UTC, nil
	case "Local":
		return nil, errors.New("billing: Local is this machine's zone and no product's; name one, as Asia/Seoul")
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("billing: %q is not a zone: an IANA name, as Asia/Seoul", name)
	}

	return loc, nil
}

// Channel is the channel a product's name for one says: a value of
// [levyan.Channel], CHANNEL_PG_KR.
func Channel(name string) (levyan.Channel, error) {
	v, ok := levyan.Channel_value[name]
	if !ok || v == 0 {
		return 0, fmt.Errorf("billing: %q is not a channel anything is sold through", name)
	}
	c := levyan.Channel(v)
	if err := pricing.CheckChannel(c); err != nil {
		return 0, err
	}

	return c, nil
}

// MonthOf is the calendar month in `zone` that `t` is in.
func MonthOf(t time.Time, zone *time.Location) rating.Period {
	u := t.In(zone)
	start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, zone)

	return rating.Period{Start: start.UTC(), End: start.AddDate(0, 1, 0).UTC()}
}

// From is the period that begins at `start`: until the end of the calendar
// month in `zone` that its first day is in. Where `start` is the first moment
// of a month in `zone` -- every period but one -- that is the month. Where it
// is not, the zone changed: `start` is a month's first moment in the zone the
// period before was cut in, within a day of one in this, and the period runs
// to the end of the month it mostly is -- February cut at its end by the new
// zone rather than nine hours of January (implementation decision).
func From(start time.Time, zone *time.Location) rating.Period {
	u := start.Add(24 * time.Hour).In(zone)
	end := time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, zone)

	return rating.Period{Start: start.UTC(), End: end.UTC()}
}
