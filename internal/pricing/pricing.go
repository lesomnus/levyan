// Package pricing is what a meter costs: the versions of a price, which of
// them is in force for a customer at a moment, and what a period's usage of a
// meter comes to under one.
//
// It is pure. No database, no clock and no tenant: what it is handed is the
// versions as they were published ([VersionOf] reads a stored `Price`) and
// what a period held or did of a meter ([Usage]), and what it answers is the
// same however often it is asked -- which is what a statement asked again a
// year later relies on, and why the price book keeps its versions unchanged.
//
// # Rounding
//
// Once, at the end, and down. A period's usage is a fraction as often as not
// -- a time-weighted average of what was held, a share of a period spent in a
// band -- and every step on the way is exact (`math/big`); what is rounded is
// the amount a version comes to, to a whole minor unit, toward zero. Toward
// zero because when in doubt levyan charges less (levyan#1, 정합성 원칙), and
// once because a rounding at every step is a different bill for the same usage
// depending on how the steps were cut.
package pricing

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/money"
)

// Version is one published version of a price: the meter it prices, who it is
// sold through and in what currency, how it prices it, and from when.
type Version struct {
	Id       pdid.Id
	Meter    pdid.Id
	Channel  levyan.Channel
	Currency string

	Rate Rate

	// Effective is from when it applies to a customer who starts on it or
	// later, and Existing from when to a customer who was there before it --
	// the same moment or later, which is the notice a rise gives.
	Effective time.Time
	Existing  time.Time

	// Published is when it was published, which of two versions of one
	// Effective says which is in force: the one published last.
	Published time.Time
}

// Line is what versions are versions of: one meter, sold on one channel, in
// one currency. A customer is priced by one line of each meter -- their
// channel's, in their currency -- and never by two.
type Line struct {
	Meter    pdid.Id
	Channel  levyan.Channel
	Currency string
}

// Line is the line v is a version of.
func (v Version) Line() Line { return Line{Meter: v.Meter, Channel: v.Channel, Currency: v.Currency} }

// Rate is how a version prices its meter: [PerUnit] or [Banded].
type Rate interface {
	// Check refuses a rate nobody could be billed by, with a [Refusal].
	Check() error

	charge(u Usage) *big.Rat
}

// Refusal is a rate refused, and which of a price's fields it is about --
// `included`, `unit_price`, `per`, `bands` -- so that whoever wrote the price
// is told where.
type Refusal struct {
	Field string
	Why   string
}

func (r Refusal) Error() string { return "pricing: " + r.Field + ": " + r.Why }

// PerUnit prices a meter by the unit: what a period includes for nothing, and
// what each Per units past that cost.
type PerUnit struct {
	// Included is how much of the meter a period includes for nothing, in the
	// meter's unit.
	Included int64

	// UnitPrice is what each Per units past the included ones cost, in the
	// minor unit of the version's currency.
	UnitPrice int64

	// Per is how many units UnitPrice is for: 1 for a price of each asset,
	// 1073741824 for a price of each GiB of a meter in bytes.
	Per int64
}

func (p PerUnit) Check() error {
	switch {
	case p.Included < 0:
		return Refusal{"included", "what is included is not less than nothing"}
	case p.UnitPrice < 0:
		return Refusal{"unit_price", "a unit price is not less than nothing"}
	case p.Per < 1:
		return Refusal{"per", "a unit price is for one unit or more"}
	}

	return nil
}

// charge is what u costs past what is included: exact, and never less than
// nothing -- a period that used less than it includes costs nothing, and does
// not carry over.
func (p PerUnit) charge(u Usage) *big.Rat {
	billable := new(big.Rat).Sub(u.Quantity(), new(big.Rat).SetInt64(p.Included))
	if billable.Sign() <= 0 {
		return new(big.Rat)
	}

	v := billable.Mul(billable, new(big.Rat).SetInt64(p.UnitPrice))
	return v.Quo(v, new(big.Rat).SetInt64(p.Per))
}

// Band is one band of a [Banded] price: the values up to a bound, and what a
// period in it costs.
type Band struct {
	// UpTo is the most a value in this band may be, in the meter's unit, when
	// Bounded; the last band has no bound, and holds every value above the one
	// before it.
	UpTo    int64
	Bounded bool

	// Fee is what a whole period in this band costs, in the minor unit of the
	// version's currency.
	Fee int64
}

// Banded prices a meter by bands of its value, each a fee for the period: the
// retention window a workspace keeps its history for -- a year, three years,
// forever -- each with a price.
type Banded struct {
	// Bands go up: each holds the values above the bound of the one before it
	// and up to its own, and the last has no bound.
	Bands []Band

	// UnboundedZero reads a value of 0 as above every bound, in the last band:
	// the meter's say (`Meter.unbounded_zero`), for a value whose 0 means
	// forever.
	UnboundedZero bool
}

func (b Banded) Check() error {
	if len(b.Bands) == 0 {
		return Refusal{"bands", "a banded price has a band"}
	}
	for i, v := range b.Bands {
		last := i == len(b.Bands)-1
		switch {
		case v.Fee < 0:
			return Refusal{"bands", fmt.Sprintf("band %d: a fee is not less than nothing", i+1)}
		case last && v.Bounded:
			return Refusal{"bands", fmt.Sprintf("band %d: the last band has no bound, so that every value is in a band", i+1)}
		case !last && !v.Bounded:
			return Refusal{"bands", fmt.Sprintf("band %d: only the last band is without a bound", i+1)}
		case v.Bounded && v.UpTo < 0:
			return Refusal{"bands", fmt.Sprintf("band %d: a bound is not below nothing", i+1)}
		case v.Bounded && i > 0 && v.UpTo <= b.Bands[i-1].UpTo:
			return Refusal{"bands", fmt.Sprintf("band %d: bands go up, and %d is not above %d", i+1, v.UpTo, b.Bands[i-1].UpTo)}
		}
	}

	return nil
}

// Band is the band a value is in.
func (b Banded) Band(value int64) Band {
	if value != 0 || !b.UnboundedZero {
		for _, v := range b.Bands {
			if v.Bounded && value <= v.UpTo {
				return v
			}
		}
	}

	return b.Bands[len(b.Bands)-1]
}

// charge is each share of the period at the fee of the band its value was in.
func (b Banded) charge(u Usage) *big.Rat {
	v := new(big.Rat)
	for _, s := range u {
		f := new(big.Rat).SetInt64(b.Band(s.Value).Fee)
		v.Add(v, f.Mul(f, s.Of))
	}

	return v
}

// Usage is what a period held or did of a meter: each value, and the share of
// the period it was held for.
//
// A sum, a maximum or a last value is the whole period at one value
// ([Whole]). A time-weighted average is a share for each value held, for as
// long as it was held -- which is what a banded price needs, and why a usage
// is not one number: a band is chosen by a value, and the average of a year and
// forever is no band at all.
type Usage []Share

// Share is one value a period held, for a share of the period.
type Share struct {
	Value int64

	// Of is the share of the period: more than nothing, and the shares of one
	// usage together no more than the whole. Less than the whole is a period
	// the meter was not held for all of -- a customer who started in it.
	Of *big.Rat
}

// Whole is a usage of one value for the whole period.
func Whole(value int64) Usage { return Usage{{Value: value, Of: big.NewRat(1, 1)}} }

// Check refuses what is not a usage: a value below nothing, a share that is
// not more than nothing, shares that add up to more than the period.
func (u Usage) Check() error {
	total := new(big.Rat)
	for i, s := range u {
		switch {
		case s.Value < 0:
			return fmt.Errorf("pricing: share %d: %d is less than nothing, which nothing holds or does", i+1, s.Value)
		case s.Of == nil || s.Of.Sign() <= 0:
			return fmt.Errorf("pricing: share %d: a share is of more than nothing of the period", i+1)
		}
		total.Add(total, s.Of)
	}
	if total.Cmp(big.NewRat(1, 1)) > 0 {
		return fmt.Errorf("pricing: the shares come to %s of the period, which is more than all of it", total.RatString())
	}

	return nil
}

// Quantity is the usage as one number in the meter's unit, each value weighed
// by its share: for a time-weighted average, the average.
func (u Usage) Quantity() *big.Rat {
	q := new(big.Rat)
	for _, s := range u {
		v := new(big.Rat).SetInt64(s.Value)
		q.Add(q, v.Mul(v, s.Of))
	}

	return q
}

// ErrTooMuch is an amount past what an int64 of minor units holds.
var ErrTooMuch = errors.New("pricing: the amount is more than an amount holds")

// CheckChannel refuses what is not a channel anything is sold through.
func CheckChannel(c levyan.Channel) error {
	switch c {
	case levyan.Channel_CHANNEL_MOR, levyan.Channel_CHANNEL_PG_KR:
		return nil
	}

	return fmt.Errorf("pricing: %s is not a channel anything is sold through", c)
}

// Check refuses what is not a version anybody could be billed by.
func (v Version) Check() error {
	if err := CheckChannel(v.Channel); err != nil {
		return err
	}
	if err := money.Check(v.Currency); err != nil {
		return err
	}
	if v.Rate == nil {
		return errors.New("pricing: a version prices its meter, by the unit or by bands")
	}
	if err := v.Rate.Check(); err != nil {
		return err
	}
	if v.Existing.Before(v.Effective) {
		return errors.New("pricing: what a version is for the customers already there is not before what it is for the new ones")
	}

	return nil
}

// Charge is what usage u of a period comes to under v, in v's currency,
// rounded down to a whole minor unit (see the package comment).
func (v Version) Charge(u Usage) (money.Amount, error) {
	if err := v.Check(); err != nil {
		return money.Amount{}, err
	}
	if err := u.Check(); err != nil {
		return money.Amount{}, err
	}

	r := v.Rate.charge(u)
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return money.Amount{}, ErrTooMuch
	}

	return money.Amount{Minor: n.Int64(), Currency: v.Currency}, nil
}

// starts is when v begins to apply to a customer who started at `since`: as
// it is effective for one who starts on it or later, and from Existing for one
// who was there before.
func (v Version) starts(since time.Time) time.Time {
	if !since.Before(v.Effective) {
		return v.Effective
	}

	return v.Existing
}

// supersedes says whether v is in force rather than u when both apply: the
// later Effective, and of one Effective the one published later -- and of one
// moment for both, the greater identifier, so that the answer never turns on
// the order the versions were handed over in.
func (v Version) supersedes(u Version) bool {
	if c := v.Effective.Compare(u.Effective); c != 0 {
		return c > 0
	}
	if c := v.Published.Compare(u.Published); c != 0 {
		return c > 0
	}

	return bytes.Compare(v.Id[:], u.Id[:]) > 0
}

// Book is the published versions of prices, of any number of lines.
type Book []Version

// InForce is the version of line l in force at `at` for a customer who started
// at `since`, and false when none is: before `since`, when they were nobody's
// customer, and before the first version of the line applied to them.
func (b Book) InForce(l Line, since, at time.Time) (Version, bool) {
	if at.Before(since) {
		return Version{}, false
	}

	var (
		in Version
		ok bool
	)
	for _, v := range b {
		if v.Line() != l || v.starts(since).After(at) {
			continue
		}
		if !ok || v.supersedes(in) {
			in, ok = v, true
		}
	}

	return in, ok
}

// Segment is a stretch of time, [From, To), through which one version was in
// force -- or none was, when Priced is false.
type Segment struct {
	From time.Time
	To   time.Time

	Version Version
	Priced  bool
}

// Timeline is which version of line l was in force over [from, to) for a
// customer who started at `since`: segments one after another that cover all
// of it, each as long as what was in force did not change.
//
// What is in force changes only where a version begins to apply to the
// customer, so those moments are where it is asked again; two segments of one
// version in a row are one.
func (b Book) Timeline(l Line, since, from, to time.Time) []Segment {
	if !from.Before(to) {
		return nil
	}

	cuts := []time.Time{from}
	cut := func(t time.Time) {
		if t.After(from) && t.Before(to) {
			cuts = append(cuts, t)
		}
	}
	cut(since)
	for _, v := range b {
		if v.Line() == l {
			cut(v.starts(since))
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)

	var out []Segment
	for i, at := range cuts {
		end := to
		if i+1 < len(cuts) {
			end = cuts[i+1]
		}

		v, ok := b.InForce(l, since, at)
		if n := len(out); n > 0 && out[n-1].Priced == ok && out[n-1].Version.Id == v.Id {
			out[n-1].To = end
			continue
		}
		out = append(out, Segment{From: at, To: end, Version: v, Priced: ok})
	}

	return out
}

// VersionOf reads a stored price, priced by bands with `unboundedZero` as its
// meter says.
func VersionOf(p *levyan.Price, unboundedZero bool) (Version, error) {
	id, err := pdid.From(p.GetId())
	if err != nil {
		return Version{}, fmt.Errorf("pricing: the price: %w", err)
	}
	meter, err := pdid.From(p.GetMeter().GetId())
	if err != nil {
		return Version{}, fmt.Errorf("pricing: the meter of %s: %w", id, err)
	}

	v := Version{
		Id:        id,
		Meter:     meter,
		Channel:   p.GetChannel(),
		Currency:  p.GetCurrency(),
		Rate:      RateOf(p.GetIncluded(), p.GetUnitPrice(), p.GetPer(), p.GetBands(), unboundedZero),
		Effective: p.GetDateEffective().AsTime(),
		Existing:  p.GetDateExisting().AsTime(),
		Published: p.GetDateCreated().AsTime(),
	}

	return v, nil
}

// RateOf is how a price's fields price its meter: by bands when it has any,
// and by the unit when it has none, a Per of 0 being 1.
func RateOf(included, unitPrice, per int64, bands *levyan.PriceBands, unboundedZero bool) Rate {
	if vs := bands.GetBands(); len(vs) > 0 {
		b := Banded{UnboundedZero: unboundedZero}
		for _, v := range vs {
			b.Bands = append(b.Bands, Band{UpTo: v.GetUpTo(), Bounded: v.HasUpTo(), Fee: v.GetFee()})
		}
		return b
	}
	if per == 0 {
		per = 1
	}

	return PerUnit{Included: included, UnitPrice: unitPrice, Per: per}
}
