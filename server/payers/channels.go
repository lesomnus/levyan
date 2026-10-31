package payers

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/lesomnus/payday/pderr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/internal/pricing"
)

// Channels is what a billing account is billed through, by the country it is
// in (`payers.channels`): the channel it is sold on and the currency it is
// billed in.
//
// It is asked once, when a billing account is made, and the account keeps the
// answer: a map changed later moves no account already made. Moving one is a
// change of channel at a period's end, which is not built yet
// (docs/design.md §14, §15); until then a payer billed otherwise makes another
// billing account and uses it.
type Channels map[string]Billed

// Billed is what a billing account is billed through.
type Billed struct {
	Channel  levyan.Channel
	Currency string
}

// Default is the key of what a billing account in a country no other key
// names is billed through.
const Default = "default"

// DefaultChannels is what is in force where `payers.channels` says nothing:
// the merchant of record everywhere, in won in Korea and in US dollars
// elsewhere. Korea is in won because the merchant of record (Paddle, the
// candidate) shows Korean cards, KakaoPay and NaverPay only for a price in won
// to a customer with a Korean address.
func DefaultChannels() Channels {
	return Channels{
		"KR":    {Channel: levyan.Channel_CHANNEL_MOR, Currency: "KRW"},
		Default: {Channel: levyan.Channel_CHANNEL_MOR, Currency: "USD"},
	}
}

// With is `cs` with what `over` says in its place, country by country: a
// deployment's `payers.channels` over [DefaultChannels].
func (cs Channels) With(over Channels) Channels {
	out := Channels{}
	maps.Copy(out, cs)
	maps.Copy(out, over)

	return out
}

// Check refuses a map that would bill an account through nothing: a key that
// is neither a country, as ISO 3166-1 alpha-2 writes one in capitals, nor
// [Default]; a channel nothing is sold through; a currency levyan does not
// price in.
func (cs Channels) Check() error {
	for _, k := range slices.Sorted(maps.Keys(cs)) {
		v := cs[k]
		if k != Default && !country.MatchString(k) {
			return fmt.Errorf("%q is neither a country, as ISO 3166-1 alpha-2 writes one in capitals (KR), nor %q", k, Default)
		}
		if err := pricing.CheckChannel(v.Channel); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		if err := money.Check(v.Currency); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}

	return nil
}

// Of is what a billing account in country `c` is billed through: what `cs`
// says of it, or of [Default] when it names it not.
func (cs Channels) Of(c string) (Billed, error) {
	if !country.MatchString(c) {
		return Billed{}, pderr.Invalidf("country", "%q is not ISO 3166-1 alpha-2, in capitals: KR", c)
	}
	v, ok := cs[c]
	if !ok {
		v, ok = cs[Default]
	}
	if !ok {
		return Billed{}, status.Errorf(codes.FailedPrecondition, "country: nothing here bills a billing account in %s", c)
	}

	return v, nil
}

// country is a country as ISO 3166-1 alpha-2 writes one.
var country = regexp.MustCompile(`^[A-Z]{2}$`)
