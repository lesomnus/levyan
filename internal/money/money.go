// Package money is what an amount is: a whole number of its currency's minor
// unit, as ISO 4217 says -- won for KRW, yen for JPY, cents for USD -- with the
// currency beside it.
//
// Two amounts of different currencies are never added. [Amount.Add] refuses
// them, so a bill in won and one in dollars stay two bills and never become a
// sum nobody could pay; the rate between them is a question this package has
// no answer to, and does not pretend to.
//
// It is loft's `internal/money`, which says how an amount is told, and the
// part a billing service needs beside it: which currencies it prices in, and
// arithmetic that neither mixes currencies nor wraps.
package money

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Amount is how much, in which currency.
type Amount struct {
	// Minor is how many of the currency's minor unit: 1250 is 12.50 USD and
	// 1,250 KRW.
	Minor int64

	// Currency is the ISO 4217 code, in capitals.
	Currency string
}

// ErrCurrency is two amounts of different currencies asked to be one.
var ErrCurrency = errors.New("money: amounts of two currencies are never added")

// ErrOverflow is a sum past what an amount holds.
var ErrOverflow = errors.New("money: the sum is more than an amount holds")

// Zero is nothing, in a currency.
func Zero(currency string) Amount { return Amount{Currency: currency} }

// Add is a and b together, of one currency, or an error: [ErrCurrency] for
// two currencies, [ErrOverflow] for a sum an int64 does not hold.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, fmt.Errorf("%w: %s and %s", ErrCurrency, a.Currency, b.Currency)
	}
	if (b.Minor > 0 && a.Minor > math.MaxInt64-b.Minor) || (b.Minor < 0 && a.Minor < math.MinInt64-b.Minor) {
		return Amount{}, ErrOverflow
	}

	return Amount{Minor: a.Minor + b.Minor, Currency: a.Currency}, nil
}

// Sum is every amount of vs together, in `currency`, which each of them has
// to be in -- a sum of nothing is still in a currency.
func Sum(currency string, vs ...Amount) (Amount, error) {
	t := Zero(currency)
	for _, v := range vs {
		var err error
		if t, err = t.Add(v); err != nil {
			return Amount{}, err
		}
	}

	return t, nil
}

// String is the amount as a sentence tells it: "915,000 KRW", "12.50 USD".
func (a Amount) String() string { return String(a.Minor, a.Currency) }

// currencies is what levyan prices in, and how many digits of each are after
// the point (ISO 4217). A price in any other is refused (see [Check]).
//
// A short list on purpose. A currency is taken because a channel charges it
// and somebody decided to sell in it, not because three letters were typed:
// "KWR" for "KRW" would otherwise be a currency of two decimal digits, and a
// price of 5000 in it fifty of something.
var currencies = map[string]int{
	"KRW": 0,
	"JPY": 0,
	"USD": 2,
	"EUR": 2,
}

// Check refuses a currency levyan does not price in.
func Check(currency string) error {
	if _, ok := currencies[currency]; !ok {
		return fmt.Errorf("money: %q is not a currency levyan prices in (%s)", currency, strings.Join(Currencies(), ", "))
	}

	return nil
}

// Currencies is what [Check] takes, in order.
func Currencies() []string { return slices.Sorted(maps.Keys(currencies)) }

// digits is the minor unit of every current ISO 4217 currency that has other
// than two digits after the point, as loft's `internal/money` has it: what an
// amount in any currency is told with, priced in or not.
var digits = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	"CLF": 4, "UYW": 4,
}

// Digits is how many of an amount's digits in currency are after the point.
func Digits(currency string) int {
	if v, ok := digits[currency]; ok {
		return v
	}

	return 2
}

// String is an amount as a sentence tells it: "915,000 KRW", "12.50 USD". One
// that names no currency is the whole number it is.
func String(n int64, currency string) string {
	sign := ""
	v := strconv.FormatInt(n, 10)
	if n < 0 {
		sign, v = "-", v[1:]
	}
	d := 0
	if currency != "" {
		d = Digits(currency)
	}
	frac := ""
	if d > 0 {
		if len(v) <= d {
			v = strings.Repeat("0", d-len(v)+1) + v
		}
		v, frac = v[:len(v)-d], "."+v[len(v)-d:]
	}
	var b strings.Builder
	for i, c := range v {
		if i > 0 && (len(v)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	s := sign + b.String() + frac
	if currency == "" {
		return s
	}

	return s + " " + currency
}
