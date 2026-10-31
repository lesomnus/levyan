package money_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/money"
)

func TestAnAmountIsToldInItsCurrency(t *testing.T) {
	x := require.New(t)

	x.Equal("915,000 KRW", money.String(915000, "KRW"))
	x.Equal("12.50 USD", money.String(1250, "USD"))
	x.Equal("0.05 USD", money.String(5, "USD"))
	x.Equal("1,234.567 KWD", money.String(1234567, "KWD"))
	x.Equal("-1,000 JPY", money.String(-1000, "JPY"))
	x.Equal("-0.05 USD", money.String(-5, "USD"))
	x.Equal("-9,223,372,036,854,775,808 KRW", money.String(math.MinInt64, "KRW"), "the one negative with no positive")
	x.Equal("100", money.String(100, ""), "a currency it names none of")
	x.Equal("12.50 USD", money.Amount{Minor: 1250, Currency: "USD"}.String())
}

func TestTwoCurrenciesAreNeverAdded(t *testing.T) {
	x := require.New(t)

	won := money.Amount{Minor: 1000, Currency: "KRW"}
	dollars := money.Amount{Minor: 1000, Currency: "USD"}

	_, err := won.Add(dollars)
	x.ErrorIs(err, money.ErrCurrency)

	_, err = money.Sum("KRW", won, won, dollars)
	x.ErrorIs(err, money.ErrCurrency, "however many of the one there are")

	_, err = money.Sum("USD", won)
	x.ErrorIs(err, money.ErrCurrency, "and a sum is in the currency it says")

	v, err := money.Sum("KRW", won, won)
	x.NoError(err)
	x.Equal(money.Amount{Minor: 2000, Currency: "KRW"}, v)

	v, err = money.Sum("USD")
	x.NoError(err)
	x.Equal(money.Zero("USD"), v, "nothing is still in a currency")
}

func TestASumDoesNotWrap(t *testing.T) {
	x := require.New(t)

	most := money.Amount{Minor: math.MaxInt64, Currency: "KRW"}
	_, err := most.Add(money.Amount{Minor: 1, Currency: "KRW"})
	x.ErrorIs(err, money.ErrOverflow)

	least := money.Amount{Minor: math.MinInt64, Currency: "KRW"}
	_, err = least.Add(money.Amount{Minor: -1, Currency: "KRW"})
	x.ErrorIs(err, money.ErrOverflow)

	v, err := most.Add(money.Amount{Minor: -1, Currency: "KRW"})
	x.NoError(err)
	x.Equal(int64(math.MaxInt64-1), v.Minor)
}

func TestOnlyTheCurrenciesLevyanPricesInAreTaken(t *testing.T) {
	x := require.New(t)

	for _, v := range []string{"KRW", "USD", "EUR", "JPY"} {
		x.NoError(money.Check(v), v)
	}
	for _, v := range []string{"", "krw", "KWR", "KPW", "XAU", "US"} {
		x.Error(money.Check(v), "%q", v)
	}
	x.Equal([]string{"EUR", "JPY", "KRW", "USD"}, money.Currencies())

	// And what it takes, it knows the minor unit of.
	x.Equal(0, money.Digits("KRW"))
	x.Equal(0, money.Digits("JPY"))
	x.Equal(2, money.Digits("USD"))
	x.Equal(2, money.Digits("EUR"))
}
