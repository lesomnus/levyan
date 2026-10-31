package cmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/xli/cfg"
	"github.com/stretchr/testify/require"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/server/payers"
)

// load is the configuration `levyan` reads from a file of `yaml` and the
// environment `env`.
func load(t *testing.T, yaml string, env ...string) (cmd.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "levyan.yaml")
	require.NoError(t, os.WriteFile(p, []byte(yaml), 0o600))

	c := cmd.Config{}
	_, err := cfg.New(cmd.Name, &c).Load(p, env)
	return c, err
}

// What a billing account is billed through, by its country, is the
// defaults where `payers.channels` says nothing, and what it says country by
// country over them -- from the file or the environment; a map that would
// bill through nothing is refused as it is read, and by Build.
func TestPayersChannelsAreTheDefaultsAndWhatTheConfigurationSays(t *testing.T) {
	x := require.New(t)
	mor, pgkr := levyan.Channel_CHANNEL_MOR, levyan.Channel_CHANNEL_PG_KR

	c, err := load(t, "{}\n")
	x.NoError(err)
	cs, err := c.Payers.ChannelsOf()
	x.NoError(err)
	x.Equal(payers.Channels{
		"KR":      {Channel: mor, Currency: "KRW"},
		"default": {Channel: mor, Currency: "USD"},
	}, cs)

	c, err = load(t, `
payers:
  channels:
    KR: {channel: pg_kr, currency: KRW}
    default: {channel: MOR, currency: EUR}
`, "LEVYAN_PAYERS_CHANNELS={JP: {channel: CHANNEL_MOR, currency: JPY}}")
	x.NoError(err)
	cs, err = c.Payers.ChannelsOf()
	x.NoError(err)
	x.Equal(payers.Channels{
		"KR":      {Channel: mor, Currency: "KRW"},
		"JP":      {Channel: mor, Currency: "JPY"},
		"default": {Channel: mor, Currency: "USD"},
	}, cs, "the environment's map is the whole of it, over the defaults")

	c, err = load(t, `
payers:
  channels:
    KR: {channel: pg_kr, currency: KRW}
    default: {channel: MOR, currency: EUR}
`)
	x.NoError(err)
	cs, err = c.Payers.ChannelsOf()
	x.NoError(err)
	x.Equal(payers.Billed{Channel: pgkr, Currency: "KRW"}, cs["KR"])
	x.Equal(payers.Billed{Channel: mor, Currency: "EUR"}, cs["default"])

	for yaml, says := range map[string]string{
		"payers: {channels: {kr: {channel: MOR, currency: KRW}}}":  `"kr" is neither a country`,
		"payers: {channels: {KR: {channel: TOSS, currency: KRW}}}": `KR: channel: "TOSS" is none of MOR, PG_KR`,
		"payers: {channels: {KR: {channel: MOR, currency: KWR}}}":  `KR: money: "KWR" is not a currency levyan prices in`,
		"payers: {channels: {KR: {channel: MOR}}}":                 `KR: money: "" is not a currency`,
		"payers: {channels: {KR: {chanel: MOR, currency: KRW}}}":   `chanel`,
	} {
		_, err := load(t, yaml+"\n")
		x.ErrorContains(err, says, yaml)
	}

	// No database is named: it is not reached.
	_, err = cmd.Build(t.Context(), cmd.Config{Payers: cmd.PayersConfig{Channels: cmd.ChannelsConfig{
		"KR": {Channel: "MOR", Currency: "KWR"},
	}}})
	x.ErrorContains(err, `payers.channels: KR: money: "KWR"`)
}

// The merchant of record is named by a block of its own, and with none there
// is none; a block that could not reach it, or would take no checkout, is
// refused as it is read -- a part wrong of itself where it is written -- and
// by Build, before the database is opened, one with no plan for a currency
// `payers.channels` bills on it.
func TestTheMerchantOfRecordIsNamedOrNone(t *testing.T) {
	x := require.New(t)

	c, err := load(t, "{}\n")
	x.NoError(err)
	x.Nil(c.Mor, "none named, none configured")

	// The key is a reference, as a deployment names one -- here to a
	// variable of the environment the configuration is read from, made up as
	// a run of zeros in a key's shape: no key of anybody's is written in this
	// repository, or anywhere else.
	key := "MOR_TEST_API_KEY=pdl_sdbx_apikey_" + strings.Repeat("0", 26) + "_" + strings.Repeat("A", 22) + "_AAA"
	secret := "MOR_TEST_WEBHOOK_SECRET=pdl_ntfset_" + strings.Repeat("0", 26) + "_" + strings.Repeat("A", 32)
	price := "pri_" + strings.Repeat("0", 26)
	token := "test_" + strings.Repeat("0", 27)
	ok := `
mor:
  kind: paddle
  environment: sandbox
  api_key: "${env:MOR_TEST_API_KEY}"
  webhook_secret: "${env:MOR_TEST_WEBHOOK_SECRET}"
  client_token: ` + token + `
  plans: {KRW: ` + price + `, USD: ` + price + `}
`
	c, err = load(t, ok, key, secret)
	x.NoError(err)
	x.NotNil(c.Mor)
	pc, err := c.Mor.Paddle()
	x.NoError(err)
	x.True(pc.Sandbox)
	x.Equal(price, pc.Plans["KRW"])
	x.Equal(time.Minute, c.Mor.PullEvery())
	x.False(c.Mor.WebhookSecret.IsZero())

	for yaml, says := range map[string][]string{
		// Wrong of itself: said where it is written.
		"mor: {kind: polar, environment: live}":                            {"levyan.yaml:1:", `mor.kind: "polar" is none of paddle`},
		"mor:\n  kind: paddle\n  environment: production":                  {"levyan.yaml:3:", `mor.environment: "production" is neither sandbox nor live`},
		"mor:\n  kind: paddle\n  environment: live\n  plans: {KWR: x}":     {"levyan.yaml:4:", `mor.plans: money: "KWR"`},
		"mor:\n  kind: paddle\n  environment: live\n  plans: {KRW: pro_0}": {"levyan.yaml:4:", `mor.plans: the plan in KRW, "pro_0", is no price`},
		// Missing, or wrong of the others.
		"mor: {kind: paddle, environment: live}":                                  {`mor: api_key: none`},
		strings.Replace(ok, "environment: sandbox", "environment: live", 1):       {`mor: the API key is the sandbox's, and the channel is live`},
		strings.Replace(ok, "  client_token: "+token+"\n", "", 1):                 {`mor: client_token: none`},
		strings.Replace(ok, "client_token: "+token, "client_token: pdl_x", 1):     {`mor: the client token is an API key`},
		strings.Replace(ok, "  plans: {KRW: "+price+", USD: "+price+"}\n", "", 1): {`mor: plans: none`},
		ok + "  tolerance: -1s\n":                                                 {`mor: tolerance: less than nothing`},
	} {
		_, err := load(t, yaml+"\n", key, secret)
		for _, v := range says {
			x.ErrorContains(err, v, yaml)
		}
	}

	// The environment alone names one, and says no more of it than that.
	_, err = load(t, "{}\n", "LEVYAN_MOR_KIND=paddle")
	x.ErrorContains(err, "mor.environment")

	// A currency the deployment bills on the merchant with no plan in it:
	// refused before the database is reached, which here is named nowhere.
	c, err = load(t, strings.Replace(ok, ", USD: "+price, "", 1), key, secret)
	x.NoError(err, "plans need not say more than there is to sell")
	_, err = cmd.Build(t.Context(), c)
	x.ErrorContains(err, "mor: plans: none in USD, which payers.channels bills on the merchant of record")
}
