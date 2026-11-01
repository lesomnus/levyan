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
	"github.com/lesomnus/levyan/internal/dunning"
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
	// The day collection on its channel begins first, so that what a case
	// below adds to the end is the merchant's.
	live := "invoices:\n  mor:\n    live_from: 2026-12-01T00:00:00+09:00\n"
	ok := live + `
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
	x.True(time.Date(2026, 11, 30, 15, 0, 0, 0, time.UTC).Equal(*c.Invoices.Mor.LiveFrom), "an instant, with its offset")

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

// A merchant of record named charges what is due on its channel, so the day
// collection on it begins is said with it, or nothing is loaded -- and
// nothing is built of a configuration made with none, before the database is
// reached (Claude's, 2026-10-31). With no merchant, the day is nobody's.
func TestCollectionOnTheMerchantsChannelBeginsOnADaySaid(t *testing.T) {
	x := require.New(t)
	key := "MOR_TEST_API_KEY=pdl_sdbx_apikey_" + strings.Repeat("0", 26) + "_" + strings.Repeat("A", 22) + "_AAA"
	price := "pri_" + strings.Repeat("0", 26)
	merchant := `
mor:
  kind: paddle
  environment: sandbox
  api_key: "${env:MOR_TEST_API_KEY}"
  client_token: test_` + strings.Repeat("0", 27) + `
  plans: {KRW: ` + price + `, USD: ` + price + `}
`
	_, err := load(t, merchant, key)
	x.ErrorContains(err, "invoices.mor.live_from: none, and `mor` names a merchant of record")

	c, err := load(t, merchant, key, "LEVYAN_INVOICES_MOR_LIVE_FROM=2026-12-01T00:00:00Z")
	x.NoError(err, "said in the environment")
	x.True(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Equal(*c.Invoices.Mor.LiveFrom))

	_, err = load(t, merchant+"invoices: {mor: {live_from: 2026-12-01}}\n", key)
	x.ErrorContains(err, "invoices.mor.live_from", "a day is no instant: which zone's midnight it is, is said")

	c, err = load(t, "invoices: {mor: {live_from: 2026-12-01T00:00:00Z}}\n")
	x.NoError(err, "a day with no merchant is nobody's")
	x.Nil(c.Mor)

	// Made in code, not loaded: refused all the same, before the database,
	// which is named nowhere here.
	c, err = load(t, merchant+"invoices: {mor: {live_from: 2026-12-01T00:00:00Z}}\n", key)
	x.NoError(err)
	c.Invoices.Mor.LiveFrom = nil
	_, err = cmd.Build(t.Context(), c)
	x.ErrorContains(err, "invoices.mor.live_from: none")
}

// The policy table of what is done after a payment fails (#11) is the
// defaults where `dunning.kinds` says nothing -- the owner's, 2026-11-01 --
// and what it says row by row, field by field, over them: `other` over what
// `temporary` came to, and a workspace nobody pays for as `payers.grace` was.
// From the file or the environment, the second stage of a restriction too.
func TestTheDunningTableIsTheDefaultsAndWhatTheConfigurationSays(t *testing.T) {
	x := require.New(t)
	day := 24 * time.Hour

	c, err := load(t, "{}\n")
	x.NoError(err)
	p, err := c.Dunning.Policy()
	x.NoError(err)
	x.Equal(dunning.Defaults(), p)
	x.Equal(time.Minute, c.Dunning.DunEvery())
	x.Equal(30*day, p.Payerless.Grace, "what payers.grace defaulted to")

	c, err = load(t, `
dunning:
  every: 5m
  kinds:
    temporary: {retries: [12h, 36h], grace: 96h, readonly_after: 720h}
    replace: {restrict: at_once, readonly_after: 24h}
    fraud: {grace: 24h, restrict: after_grace}
    payerless: {grace: 240h, restrict: AT_ONCE, readonly_after: 480h}
`, "LEVYAN_DUNNING_KINDS_REPLACE_RETRIES=24h,48h", "LEVYAN_DUNNING_KINDS_FRAUD_READONLY_AFTER=168h")
	x.NoError(err)
	p, err = c.Dunning.Policy()
	x.NoError(err)
	x.Equal(5*time.Minute, c.Dunning.DunEvery())
	x.Equal(dunning.Row{Retries: []time.Duration{12 * time.Hour, 36 * time.Hour}, Grace: 96 * time.Hour, ReadonlyAfter: 30 * day}, p.Temporary)
	x.Equal(dunning.Row{Retries: []time.Duration{day, 2 * day}, Grace: 14 * day, AtOnce: true, ReadonlyAfter: day}, p.Replace, "from the environment, and the file: read-only a day after, restricted at once")
	x.Equal(dunning.Row{Grace: day, ReadonlyAfter: 7 * day}, p.Fraud)
	x.Equal(p.Temporary, p.Other, "what other leaves out is what temporary came to")
	x.Equal(dunning.Row{Grace: 10 * day, AtOnce: true, ReadonlyAfter: 20 * day}, p.Payerless)

	c, err = load(t, "dunning: {kinds: {temporary: {grace: 48h}, other: {retries: []}}}\n")
	x.NoError(err)
	p, err = c.Dunning.Policy()
	x.NoError(err)
	x.Equal(dunning.Defaults().Temporary.Retries, p.Temporary.Retries)
	x.Equal(dunning.Row{Grace: 2 * day, ReadonlyAfter: 60 * day}, p.Other, "none, said; the rest as temporary")
}

// A table that says no schedule is refused as it is read, where it is
// written: a retry not after the failure or not after the one before it, a
// grace or a second stage less than nothing, a restriction that is neither
// after the grace nor at once, a kind there is no row of, a row read-only
// sooner than it is restricted -- what it says with its defaults, of
// `other` with what `temporary` came to -- and retries of a workspace nobody
// pays for, which nothing is charged of. `payers.grace` is the table's row
// now. And Build refuses a table made in code before it opens anything.
func TestADunningTableThatSaysNoScheduleIsRefused(t *testing.T) {
	x := require.New(t)

	for yaml, says := range map[string][]string{
		"dunning:\n  kinds:\n    temporary:\n      retries: [72h, 24h]":  {"levyan.yaml:4:", "dunning.kinds.temporary.retries: 24h0m0s is no later than the retry before it, 72h0m0s"},
		"dunning:\n  kinds:\n    other:\n      retries: [0s]":            {"levyan.yaml:4:", "dunning.kinds.other.retries: 0s is no time after the failure"},
		"dunning:\n  kinds:\n    replace:\n      grace: -1h":             {"levyan.yaml:4:", "dunning.kinds.replace.grace: -1h0m0s is less than nothing"},
		"dunning:\n  kinds:\n    replace:\n      grace: soon":            {"levyan.yaml:4:", "dunning.kinds.replace.grace", `"soon"`},
		"dunning:\n  kinds:\n    fraud:\n      restrict: never":          {"levyan.yaml:4:", `dunning.kinds.fraud.restrict: "never" is neither after_grace nor at_once`},
		"dunning:\n  kinds:\n    payerless:\n      restrict: later":      {"levyan.yaml:4:", `dunning.kinds.payerless.restrict: "later" is neither`},
		"dunning:\n  kinds:\n    fruad: {grace: 0s}":                     {"levyan.yaml:3:", "fruad", `"fraud"`},
		"dunning:\n  kinds:\n    payerless:\n      retries: [24h]":       {"levyan.yaml:4:", "retries"},
		"payers:\n  grace: 720h":                                         {"levyan.yaml:2:", "grace"},
		"dunning:\n  kinds:\n    temporary:\n      readonly_after: -1h":  {"levyan.yaml:4:", "dunning.kinds.temporary.readonly_after: -1h0m0s is less than nothing"},
		"dunning:\n  kinds:\n    replace:\n      readonly_after: 100h":   {"dunning.kinds: replace: readonly_after: 100h0m0s is sooner than the restriction, 336h0m0s after the failure"},
		"dunning:\n  kinds:\n    temporary:\n      grace: 2000h":         {"dunning.kinds: temporary: readonly_after: 1440h0m0s is sooner than the restriction, 2000h0m0s after the failure"},
		"dunning:\n  kinds:\n    other:\n      grace: 1500h":             {"dunning.kinds: other: readonly_after: 1440h0m0s is sooner than the restriction, 1500h0m0s after the failure"},
		"dunning:\n  kinds:\n    payerless:\n      readonly_after: 100h": {"dunning.kinds: payerless: readonly_after: 100h0m0s is sooner than the restriction, 720h0m0s after the failure"},
	} {
		_, err := load(t, yaml+"\n")
		x.Error(err, yaml)
		for _, v := range says {
			x.ErrorContains(err, v, yaml)
		}
	}

	_, err := load(t, "{}\n", "LEVYAN_DUNNING_KINDS_TEMPORARY_RETRIES=24h,12h")
	x.ErrorContains(err, "LEVYAN_DUNNING_KINDS_TEMPORARY_RETRIES: 12h0m0s is no later than the retry before it, 24h0m0s", "said where it was said")
	_, err = load(t, "{}\n", "LEVYAN_DUNNING_KINDS_TEMPORARY_READONLY_AFTER=24h")
	x.ErrorContains(err, "temporary: readonly_after: 24h0m0s is sooner than the restriction, 336h0m0s after the failure")

	// Made in code, not loaded: refused all the same, before the database,
	// which is named nowhere here.
	back := cmd.Retries{cmd.Span(48 * time.Hour), cmd.Span(24 * time.Hour)}
	_, err = cmd.Build(t.Context(), cmd.Config{Dunning: cmd.DunningConfig{Kinds: cmd.DunningKinds{Temporary: cmd.DunningKind{Retries: &back}}}})
	x.ErrorContains(err, "dunning.kinds.temporary: retries: 24h0m0s is no later than the retry before it, 48h0m0s")
	soon := cmd.Span(24 * time.Hour)
	_, err = cmd.Build(t.Context(), cmd.Config{Dunning: cmd.DunningConfig{Kinds: cmd.DunningKinds{Fraud: cmd.DunningKind{ReadonlyAfter: &soon, Restrict: "after_grace", Grace: new(cmd.Span(48 * time.Hour))}}}})
	x.ErrorContains(err, "dunning.kinds.fraud: readonly_after: 24h0m0s is sooner than the restriction, 48h0m0s after the failure")
}

// How the loop `levyan.reconcile` holds levyan to its channels is said in
// `reconcile`, of the file or the environment, each with its default; a
// duration of less than nothing is refused where it is written. The
// reconciler a deployment builds is held to what is said.
func TestReconciliationIsConfiguredOrItsDefaults(t *testing.T) {
	x := require.New(t)

	c, err := load(t, "{}\n")
	x.NoError(err)
	x.Equal(cmd.ReconcileConfig{}, c.Reconcile)

	c, err = load(t, `
reconcile:
  every: 12h
  window: 336h
  stale:
    unknown: 2h
    mirror: 72h
`, "LEVYAN_RECONCILE_STALE_PENDING=96h")
	x.NoError(err)
	x.Equal(12*time.Hour, c.Reconcile.Every)
	x.Equal(336*time.Hour, c.Reconcile.Window)
	x.Equal(cmd.StaleConfig{Unknown: 2 * time.Hour, Pending: 96 * time.Hour, Mirror: 72 * time.Hour}, c.Reconcile.Stale)

	_, err = load(t, "reconcile: {stale: {notice: -1h}}\n")
	x.ErrorContains(err, "stale.notice: less than nothing")

	c.Db.Driver, c.Db.Dsn = "sqlite3", "file:"+filepath.Join(t.TempDir(), "levyan.db")+"?_pragma=foreign_keys(1)"
	c.Watch.Broker = "memory"
	s, err := cmd.Build(t.Context(), c)
	x.NoError(err)
	defer s.Close()
	x.Equal(336*time.Hour, s.Reconciler.Window)
	x.Equal(2*time.Hour, s.Reconciler.Stale.Unknown)
	x.Equal(96*time.Hour, s.Reconciler.Stale.Pending)
	x.Equal(72*time.Hour, s.Reconciler.Stale.Mirror)
	x.Nil(s.Reconciler.Subscriptions.Channel, "no merchant of record is named")

	names := []string{}
	for _, v := range s.Spin {
		if n, ok := v.(interface{ SpinName() string }); ok {
			names = append(names, n.SpinName())
		}
	}
	x.Contains(names, "levyan.reconcile")
}
