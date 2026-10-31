package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

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
