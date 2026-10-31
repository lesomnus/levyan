// Package cmd is this app's own wiring, and it is short on purpose.
//
// Everything that does not change from one app to the next is in payday. What
// is left is here, and it is deliberately **not** hidden behind a
// `payday.Serve(cfg)`: the stack, the order of the interceptors and which
// server the wall is on are the decisions a reader of an app most needs to be
// able to see, and a framework that hid them would be hiding the only part
// worth reading.
//
// It is what both binaries share. `levyan` (`cli`) serves products, and
// `levyan-admin` (`cli/admin`) serves operators; each puts its own door on
// what [Build] makes, and neither links the other's (cmd/levyan's test).
package cmd

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/payday/config"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/keyring"
	"github.com/lesomnus/levyan/internal/mor/paddle"
	"github.com/lesomnus/levyan/internal/taxinvoice"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/invoices/bank"
	"github.com/lesomnus/levyan/server/payers"
)

// Name is what this app is called, and it is the only place it is written.
// The environment prefix and the names of the configuration files are derived
// from it -- LEVYAN_DB_DSN, levyan.yaml -- by the loader `cli` makes with it,
// so there is nothing to keep in step.
const Name = "levyan"

// Config is what this app is configured with.
//
// The framework cannot own this struct, since what an app is configured with is
// the app's. What it owns is the pieces: each of these is a payday type, and
// what is written here is only which of them this app has.
type Config struct {
	Server config.ServerConfig `yaml:"server"`
	Db     config.DbConfig     `yaml:"db"`
	Otel   config.OtelConfig   `yaml:"otel"`
	Watch  config.WatchConfig  `yaml:"watch"`
	Spin   config.SpinConfig   `yaml:"spin"`

	// How long the trail keeps a row, per kind of thing. Every write this app
	// makes is recorded, and nothing else applies a window, so a deployment
	// that names none keeps all of it forever -- which is this field left
	// empty, and is the only honest default: a version upgrade is not the right
	// thing to decide how long somebody's evidence lasts.
	//
	// What the values are is what this app is regulated as, which payday cannot
	// know. `trail.Profiles` carries the sentence each number comes from.
	Audit config.AuditConfig `yaml:"audit"`

	// Admin is the operator's entry point, which `levyan-admin serve` serves
	// and `levyan serve` does not.
	Admin AdminConfig `yaml:"admin"`

	// Readings is what is expected of the readings products send.
	Readings ReadingsConfig `yaml:"readings"`

	// Periods is when a product's periods are closed.
	Periods PeriodsConfig `yaml:"periods"`

	// Grants is how what levyan gives products' workspaces is kept.
	Grants GrantsConfig `yaml:"grants"`

	// BillingKeys is where the keys are that the billing keys a PG issues are
	// sealed under before they are written (internal/keyring): its kind --
	// `file`, the one built -- every key that opens, and the one that seals.
	// Named nowhere, there is no keyring, and nothing stores a billing key yet
	// (#4); named wrong, the process is refused where it comes up.
	BillingKeys keyring.Config `yaml:"billing_keys"`

	// Auth is who levyan asks about the people it bills: roster.
	Auth AuthConfig `yaml:"auth"`

	// Payers is who pays for products' workspaces (#4).
	Payers PayersConfig `yaml:"payers"`

	// Invoices is what billing accounts are asked to pay (#9).
	Invoices InvoicesConfig `yaml:"invoices"`

	// Dunning is what is done after a payment fails, and of a workspace
	// nobody pays for (#11): the policy table, a row for each kind of
	// decline and one for a workspace with no payer.
	Dunning DunningConfig `yaml:"dunning"`

	// Mor is the merchant of record a billing account on its channel is
	// collected through (#12): Paddle. With no block, none is configured --
	// a checkout is refused, nothing is received from or pulled of a
	// merchant, and nothing collects on its channel -- and nothing else
	// changes. Named, its channel is collected from `invoices.mor.live_from`
	// on, which is then required.
	Mor *MorConfig `yaml:"mor"`
}

// InvoicesConfig is what billing accounts are asked to pay, and how often
// the loop `levyan.invoices.collect` keeps it.
type InvoicesConfig struct {
	// Every is how often statements are invoiced, drafts decided, a
	// seller's documents issued and what is due moved: ten minutes when
	// unset, as often as a merchant of record lets a charge whose answer
	// never came be asked about.
	Every time.Duration `yaml:"every"`

	// Tax is the seller's tax, as a percentage of an invoice's subtotal, on
	// a channel levyan sells on itself -- not a merchant of record, which
	// adds its own: 10 when unset, Korea's VAT (부가가치세법 제30조). It may
	// be a fraction, `7.5`; `0` is none.
	Tax *string `yaml:"tax"`

	// BankKR is the bank-transfer channel (`CHANNEL_BANK_KR`): an
	// institution pays by transfer, with a tax invoice. Not configured --
	// no issuer named -- it collects nothing, and its drafts wait.
	BankKR BankConfig `yaml:"bank_kr"`

	// Mor is the merchant of record's channel (`CHANNEL_MOR`), which `mor`
	// names the merchant of: when collection on it begins.
	Mor MorInvoicesConfig `yaml:"mor"`
}

// MorInvoicesConfig is when collection on the merchant of record's channel
// begins.
type MorInvoicesConfig struct {
	// LiveFrom is when collection on the merchant of record's channel
	// begins, an instant with its offset (RFC 3339:
	// 2026-12-01T00:00:00+09:00). A draft of it whose first period ended
	// before then is the shadow billing's, and is closed as a shadow, never
	// collected; a period that ends at or after it is invoiced and charged.
	// Required once `mor` names a merchant, so that nobody turns charging on
	// without choosing when it begins (Claude's, 2026-10-31).
	LiveFrom *time.Time `yaml:"live_from"`
}

// Validate refuses what one block of the configuration says against another,
// as it is loaded: a merchant of record named, which charges what is due on
// its channel, with no day collection on it begins.
func (c Config) Validate() error {
	return c.liveFrom()
}

// liveFrom is nil when collection on the merchant of record's channel has a
// day it begins, or no merchant collects on it.
func (c Config) liveFrom() error {
	if c.Mor != nil && c.Invoices.Mor.LiveFrom == nil {
		return errors.New("invoices.mor.live_from: none, and `mor` names a merchant of record, which charges what is due on its channel: say when collection on it begins (RFC 3339, 2026-12-01T00:00:00+09:00), and the drafts of the periods before it stay the shadow billing's, never charged")
	}

	return nil
}

// BankConfig is the bank-transfer channel: who issues its tax invoices, the
// business that sells, the least it collects and when an invoice is due.
type BankConfig struct {
	// Issuer is the kind of service that issues its tax invoices, as the
	// package that implements it registers itself (`internal/taxinvoice`).
	// None is linked yet: which service it is, is the owner's to choose.
	Issuer string `yaml:"issuer"`

	// Seller is the deployment's own business, the supplier of every tax
	// invoice: its registration number, name, representative, address,
	// 업태 and 종목, and its email.
	Seller SellerConfig `yaml:"seller"`

	// Minimum is the least an invoice is collected for, in won: none when
	// unset. Less waits for the next period -- which a tax invoice issued for
	// two months makes risky (docs/design.md §17, 64).
	Minimum int64 `yaml:"minimum"`

	// Due is how long after it is finalized an invoice is due: 720h, thirty
	// days, when unset.
	Due time.Duration `yaml:"due"`
}

// SellerConfig is the business that sells on a channel levyan sells on.
type SellerConfig struct {
	Registration   string `yaml:"registration"`
	Name           string `yaml:"name"`
	Representative string `yaml:"representative"`
	Address        string `yaml:"address"`
	Kind           string `yaml:"kind"`
	Item           string `yaml:"item"`
	Email          string `yaml:"email"`
}

// Configured says whether the channel is: whether an issuer is named.
func (c BankConfig) Configured() bool { return c.Issuer != "" }

// Collector is the channel's collector, its issuer opened: refused when the
// seller is not a business a tax invoice can name, or the issuer is of no
// kind linked into this binary.
func (c BankConfig) Collector() (*bank.Channel, error) {
	if !taxinvoice.Registration(c.Seller.Registration) || strings.TrimSpace(c.Seller.Name) == "" {
		return nil, errors.New("seller: the business that sells, by its registration number of ten digits and its name, which every tax invoice names")
	}
	if c.Minimum < 0 {
		return nil, errors.New("minimum: no less than nothing")
	}
	iss, err := taxinvoice.Open(c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("issuer: %w", err)
	}

	return &bank.Channel{
		Issuer: iss,
		Seller: taxinvoice.Party{
			Registration:   strings.TrimSpace(c.Seller.Registration),
			Name:           strings.TrimSpace(c.Seller.Name),
			Representative: strings.TrimSpace(c.Seller.Representative),
			Address:        strings.TrimSpace(c.Seller.Address),
			Kind:           strings.TrimSpace(c.Seller.Kind),
			Item:           strings.TrimSpace(c.Seller.Item),
			Email:          strings.TrimSpace(c.Seller.Email),
		},
		Least: c.Minimum,
		After: c.Due,
	}, nil
}

// CollectEvery is [InvoicesConfig.Every], or its default.
func (c InvoicesConfig) CollectEvery() time.Duration {
	if c.Every <= 0 {
		return invoices.DefaultEvery
	}
	return c.Every
}

// TaxOf is [InvoicesConfig.Tax] as a rate of the subtotal, or its default.
func (c InvoicesConfig) TaxOf() (*big.Rat, error) {
	if c.Tax == nil {
		return invoices.DefaultTax, nil
	}
	v, ok := new(big.Rat).SetString(strings.TrimSuffix(strings.TrimSpace(*c.Tax), "%"))
	if !ok || v.Sign() < 0 || v.Cmp(big.NewRat(100, 1)) > 0 {
		return nil, fmt.Errorf("%q is not a percentage from 0 to 100", *c.Tax)
	}

	return v.Quo(v, big.NewRat(100, 1)), nil
}

// MorConfig is the merchant of record levyan sells through, and how it is
// reached (docs/design.md §12).
type MorConfig struct {
	// Kind is which merchant of record it is: `paddle`, the one there is.
	Kind MorKind `yaml:"kind"`

	// Environment is `sandbox`, where test cards move no money, or `live`.
	// Each has keys of its own and an API of its own.
	Environment MorEnvironment `yaml:"environment"`

	// APIKey is levyan's key at the merchant, a secret (`${file:/run/…}`):
	// for Paddle a pdl_sdbx_apikey_… or pdl_live_apikey_… of the
	// environment's, with the permissions to read and write transactions,
	// subscriptions and adjustments, and to read notifications (its event
	// stream). It is read every time it is used, so a key rotated into its
	// file is the next request's, and it is printed as its reference.
	APIKey cfg.Secret `yaml:"api_key"`

	// WebhookSecret is the secret the merchant signs its webhooks with -- for
	// Paddle, the notification destination's endpoint secret key,
	// pdl_ntfset_… -- a secret too. With none, no webhook is received: the
	// endpoint is not served, and the merchant's events are read by the pull
	// alone (`pull`).
	WebhookSecret cfg.Secret `yaml:"webhook_secret"`

	// ClientToken is what the merchant's checkout is opened with in a page --
	// for Paddle, its client-side token, test_… or live_… -- which is public,
	// and no key.
	ClientToken string `yaml:"client_token"`

	// Plans is the plan a checkout subscribes a billing account to, by the
	// currency it is billed in -- for Paddle, a catalog price, pri_…, of a
	// month of nothing, every amount being levyan's one-time charges
	// (docs/design.md §17, 41). Every currency `payers.channels` bills on the
	// merchant of record needs one.
	Plans MorPlans `yaml:"plans"`

	// Tolerance is how far from now a webhook may say it was signed: 5s
	// when unset (Paddle's SDKs' own), against one replayed.
	Tolerance time.Duration `yaml:"tolerance"`

	// Pull is how often the merchant's list of its events is read into the
	// inbox beside its webhooks: a minute when unset.
	Pull time.Duration `yaml:"pull"`

	// URL is the merchant's API, and Clock its channel's clock: a test's
	// fake's. A deployment says neither, and is the environment's API on
	// the wall clock.
	URL   string           `yaml:"-"`
	Clock func() time.Time `yaml:"-"`
}

// MorKind is which merchant of record levyan sells through: `paddle`.
type MorKind string

// Validate refuses a merchant of record there is no adapter of, where it is
// written.
func (k MorKind) Validate() error {
	if strings.ToLower(strings.TrimSpace(string(k))) != "paddle" {
		return fmt.Errorf("%q is none of paddle", string(k))
	}
	return nil
}

// MorEnvironment is the merchant's environment: `sandbox` or `live`.
type MorEnvironment string

// Validate refuses an environment that is neither, where it is written.
func (e MorEnvironment) Validate() error {
	_, err := e.sandbox()
	return err
}

func (e MorEnvironment) sandbox() (bool, error) {
	switch strings.ToLower(strings.TrimSpace(string(e))) {
	case "sandbox":
		return true, nil
	case "live":
		return false, nil
	}
	return false, fmt.Errorf("%q is neither sandbox nor live", string(e))
}

// MorPlans is the plan a checkout subscribes a billing account to, by the
// currency it is billed in.
type MorPlans map[string]string

// Validate refuses a plan in a currency levyan does not price in, or one that
// is no price of the merchant's, where it is written.
func (p MorPlans) Validate() error { return paddle.CheckPlans(p) }

// Validate refuses a merchant of record that could not be reached, or would
// take no checkout, as the configuration is loaded: what is missing from it,
// and what its parts say of one another -- a key of the other environment's,
// a key where the page's token goes. A part wrong of itself is said where it
// is written, by its own.
func (c MorConfig) Validate() error {
	for _, v := range []cfg.Validator{c.Kind, c.Environment, c.Plans} {
		if v.Validate() != nil {
			return nil
		}
	}
	_, err := c.Paddle()

	return err
}

// Paddle is the configuration of the adapter `kind: paddle` is, checked.
func (c MorConfig) Paddle() (paddle.Config, error) {
	if err := c.Kind.Validate(); err != nil {
		return paddle.Config{}, fmt.Errorf("kind: %w", err)
	}
	sandbox, err := c.Environment.sandbox()
	if err != nil {
		return paddle.Config{}, fmt.Errorf("environment: %w", err)
	}
	switch {
	case c.APIKey.IsZero():
		return paddle.Config{}, errors.New("api_key: none, and the merchant is reached with nothing else")
	case c.ClientToken == "":
		return paddle.Config{}, errors.New("client_token: none, and a checkout is opened with it")
	case len(c.Plans) == 0:
		return paddle.Config{}, errors.New("plans: none, and a checkout subscribes a billing account to the plan of its currency")
	case c.Tolerance < 0:
		return paddle.Config{}, errors.New("tolerance: less than nothing")
	case c.Pull < 0:
		return paddle.Config{}, errors.New("pull: less than nothing")
	}
	if err := c.Plans.Validate(); err != nil {
		return paddle.Config{}, fmt.Errorf("plans: %w", err)
	}

	out := paddle.Config{
		APIKey:        c.APIKey,
		ClientToken:   c.ClientToken,
		WebhookSecret: c.WebhookSecret,
		Sandbox:       sandbox,
		Plans:         c.Plans,
		Tolerance:     c.Tolerance,
		URL:           c.URL,
		Clock:         c.Clock,
	}
	if _, err := paddle.New(out); err != nil {
		return paddle.Config{}, errors.New(strings.TrimPrefix(err.Error(), "paddle: "))
	}

	return out, nil
}

// PullEvery is [MorConfig.Pull], or its default.
func (c MorConfig) PullEvery() time.Duration {
	if c.Pull <= 0 {
		return time.Minute
	}

	return c.Pull
}

// PlansFor refuses plans that leave a currency `channels` bills on the
// merchant of record with none: a billing account in it could begin no
// checkout.
func (c MorConfig) PlansFor(channels payers.Channels) error {
	need := map[string]bool{}
	for _, b := range channels {
		if b.Channel == levyan.Channel_CHANNEL_MOR {
			need[b.Currency] = true
		}
	}
	for _, cur := range slices.Sorted(maps.Keys(need)) {
		if c.Plans[cur] == "" {
			return fmt.Errorf("plans: none in %s, which payers.channels bills on the merchant of record", cur)
		}
	}

	return nil
}

// AuthConfig is who levyan asks about the people it bills.
type AuthConfig struct {
	// Roster is the roster whose accounts pay: asked whether an account a
	// product relays a call for is in good standing, and heard saying when
	// one is suspended or erased (roster's `standing`, its
	// `docs/relying-app.md`).
	Roster RosterConfig `yaml:"roster"`
}

// RosterConfig is the roster levyan asks, and who levyan is there.
//
// With no address, no account's standing can be asked, so levyan serves no
// call an account makes ( `levyan.v1.PayerService`) -- it refuses them all
// rather than serve one on nobody's word -- and hears nothing of roster.
type RosterConfig struct {
	// Addr is roster's data plane, as gRPC dials it: `roster:50051`.
	Addr string `yaml:"addr"`

	// Key is what levyan calls roster with, a secret (`${file:/run/…}`):
	// a deployment key, `rk_…`, which every call names the directory beside
	// (`roster-at`), or a tenant key, `rt_…`, of the directory itself (roster's
	// `docs/apps.md`). Its role holds `standing.Methods`.
	Key cfg.Secret `yaml:"key"`

	// Directory is the roster tenant the accounts are in, by its alias: one
	// for the whole deployment. Required with a deployment key; a tenant key
	// is its own tenant's, and names none.
	Directory string `yaml:"directory"`

	// Insecure dials roster in plaintext, for a lab; CaFile pins the CA its
	// certificate chains to, and empty is the system's.
	Insecure bool   `yaml:"insecure"`
	CaFile   string `yaml:"ca_file"`
}

// Configured says whether a roster is named at all.
func (c RosterConfig) Configured() bool { return c.Addr != "" }

// PayersConfig is who pays for products' workspaces (#4).
type PayersConfig struct {
	// Free is how many of each payer's workspaces of a product have the
	// allowances their prices include: 3 when unset, as many as an account
	// may own in loft (`app.workspaces.owned`). The others are rated from
	// the first unit. Zero is written `0` and is none.
	Free *int `yaml:"free"`

	// How long a workspace may go with nobody paying for it before it is
	// restricted was `payers.grace`, and is the policy table's row of it now,
	// `dunning.kinds.payerless` (#11): one table.

	// Settle is how often what roster said of an erased account is asked
	// again, and acted on once roster has nobody by them: an hour when unset.
	Settle time.Duration `yaml:"settle"`

	// Reconcile is how often roster is asked about every account levyan
	// bills, for what its stream missed: a day when unset.
	Reconcile time.Duration `yaml:"reconcile"`

	// Channels is what a billing account is billed through, by the country
	// it is in -- ISO 3166-1 alpha-2, in capitals -- and `default` for every
	// country no other key names. A key left out is as the defaults say: the
	// merchant of record everywhere, in won in KR and in US dollars
	// elsewhere ([payers.DefaultChannels]).
	//
	// It is asked as a billing account is made, and never again of it:
	// changing it moves no billing account made before.
	Channels ChannelsConfig `yaml:"channels"`
}

// ChannelsConfig is what a billing account in each country is billed
// through, by the country.
type ChannelsConfig map[string]ChannelConfig

// ChannelConfig is what a billing account in a country is billed through.
type ChannelConfig struct {
	// Channel is what it is sold on: MOR, the merchant of record, or PG_KR,
	// the Korean payment gateway.
	Channel string `yaml:"channel"`

	// Currency is what it is billed in, as ISO 4217 writes it: KRW.
	Currency string `yaml:"currency"`
}

// Validate refuses channels that would bill a billing account through
// nothing, as the configuration is loaded.
func (c ChannelsConfig) Validate() error {
	_, err := c.Over(payers.DefaultChannels())

	return err
}

// ChannelsOf is [PayersConfig.Channels] over its defaults.
func (c PayersConfig) ChannelsOf() (payers.Channels, error) {
	return c.Channels.Over(payers.DefaultChannels())
}

// Over is `base` with what these say in its place, country by country.
func (c ChannelsConfig) Over(base payers.Channels) (payers.Channels, error) {
	over := payers.Channels{}
	for _, k := range slices.Sorted(maps.Keys(c)) {
		v := c[k]
		name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(v.Channel)), "CHANNEL_")
		ch, ok := levyan.Channel_value["CHANNEL_"+name]
		if !ok || ch == 0 {
			return nil, fmt.Errorf("%s: channel: %q is none of %s", k, v.Channel, strings.Join(channelNames(), ", "))
		}
		over[k] = payers.Billed{Channel: levyan.Channel(ch), Currency: v.Currency}
	}

	out := base.With(over)
	if err := out.Check(); err != nil {
		return nil, err
	}

	return out, nil
}

// channelNames is the channels a billing account may be billed through, as
// [ChannelConfig.Channel] writes them.
func channelNames() []string {
	out := []string{}
	for v, name := range levyan.Channel_name {
		if v != 0 && levyan.Channel(v) != levyan.Channel_CHANNEL_BANK_KR {
			out = append(out, strings.TrimPrefix(name, "CHANNEL_"))
		}
	}
	slices.Sort(out)

	return out
}

// FreeOf is [PayersConfig.Free], or its default.
func (c PayersConfig) FreeOf() int {
	if c.Free == nil {
		return 3
	}
	return max(*c.Free, 0)
}

// SettleEvery is [PayersConfig.Settle], or its default.
func (c PayersConfig) SettleEvery() time.Duration {
	if c.Settle <= 0 {
		return time.Hour
	}
	return c.Settle
}

// ReconcileEvery is [PayersConfig.Reconcile], or its default.
func (c PayersConfig) ReconcileEvery() time.Duration {
	if c.Reconcile <= 0 {
		return 24 * time.Hour
	}
	return c.Reconcile
}

// DunningConfig is what is done after a payment fails (#11): how often the
// loop `levyan.invoices.dunning` looks, and the policy table it follows.
type DunningConfig struct {
	// Every is how often the loop makes the retries that are due -- and the
	// charge due once a billing account is collectable again -- gives up on
	// what has spent its grace and its retries, and restricts, and lets go,
	// what the table says: a minute when unset.
	Every time.Duration `yaml:"every"`

	// Kinds is the table: a row for each kind of decline, and one for a
	// workspace nobody pays for. What a row leaves out is its default
	// (`internal/dunning.Defaults`, Claude's, provisional: the values are
	// the owner's open question 3).
	Kinds DunningKinds `yaml:"kinds"`
}

// DunningKinds is the policy table, by kind.
type DunningKinds struct {
	// Temporary is a decline that may go through later on the same method:
	// tried again 1, 3 and 5 days after the first failure, with 7 days of
	// grace, restricted after it.
	Temporary DunningKind `yaml:"temporary"`

	// Replace is a method that will not be charged as it is, or none: never
	// tried again -- its payer is told at once, by the product -- with 7 days
	// of grace, restricted after it.
	Replace DunningKind `yaml:"replace"`

	// Fraud is a charge refused as suspected fraud: never tried again, no
	// grace, restricted at once.
	Fraud DunningKind `yaml:"fraud"`

	// Other is a refusal that says no more, and a failure that is no
	// decline: what it leaves out is what `temporary` says.
	Other DunningKind `yaml:"other"`

	// Payerless is a workspace nobody pays for: 30 days of grace from when it
	// last had a payer, restricted after it -- what `payers.grace` said, and
	// still the default. Nothing is charged of it, so it has no retries.
	Payerless PayerlessKind `yaml:"payerless"`
}

// DunningKind is a row of the table: its retries, its grace and when it
// restricts.
type DunningKind struct {
	// Retries are when an invoice is charged again, each a span from its
	// first failed charge and each later than the one before: `[24h, 72h]`.
	// Unset is the row's default; `[]` is none.
	Retries *Retries `yaml:"retries"`

	// Grace is how long after its first failure an invoice may go unpaid
	// before it is restricted -- unless `restrict` says at once -- and given
	// up on once no retry is left: `168h`. Unset is the row's default.
	Grace *Span `yaml:"grace"`

	// Restrict is when what an unpaid invoice bills is restricted:
	// `after_grace` or `at_once`. Unset is the row's default.
	Restrict Restrict `yaml:"restrict"`
}

// PayerlessKind is the table's row of a workspace nobody pays for.
type PayerlessKind struct {
	// Grace is how long after it last had a payer a workspace may go with
	// none before it is restricted: 720h, thirty days, when unset.
	Grace *Span `yaml:"grace"`

	// Restrict is `after_grace`, as when unset, or `at_once`.
	Restrict Restrict `yaml:"restrict"`
}

// Span is a stretch of time as a duration writes it, `72h`, and no less than
// nothing.
type Span time.Duration

// UnmarshalText reads a duration.
func (s *Span) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	*s = Span(v)
	return nil
}

// MarshalText writes it as a duration.
func (s Span) MarshalText() ([]byte, error) { return []byte(time.Duration(s).String()), nil }

// Validate refuses less than nothing, where it is written.
func (s Span) Validate() error {
	if s < 0 {
		return fmt.Errorf("%s is less than nothing", time.Duration(s))
	}
	return nil
}

// Retries are a row's retries, each a span from the first failure.
type Retries []Span

// Validate refuses retries that are no schedule -- one not after the
// failure, or not after the one before it -- where they are written.
func (r Retries) Validate() error { return dunning.Retries(r.durations()) }

// durations is them as the table holds them: nil for none.
func (r Retries) durations() []time.Duration {
	var out []time.Duration
	for _, v := range r {
		out = append(out, time.Duration(v))
	}
	return out
}

// Restrict is when a row restricts: `after_grace`, or `at_once`.
type Restrict string

// Validate refuses a word that is neither, where it is written.
func (r Restrict) Validate() error {
	_, _, err := r.atOnce()
	return err
}

// atOnce is whether it says at once, and whether it says anything.
func (r Restrict) atOnce() (atOnce, said bool, err error) {
	switch strings.ToLower(strings.TrimSpace(string(r))) {
	case "":
		return false, false, nil
	case "after_grace":
		return false, true, nil
	case "at_once":
		return true, true, nil
	}
	return false, false, fmt.Errorf("%q is neither after_grace nor at_once", string(r))
}

// over is `row` with what `k` says in its place.
func (k DunningKind) over(row dunning.Row) (dunning.Row, error) {
	if k.Retries != nil {
		row.Retries = k.Retries.durations()
	} else {
		row.Retries = slices.Clone(row.Retries)
	}
	if k.Grace != nil {
		row.Grace = time.Duration(*k.Grace)
	}
	atOnce, said, err := k.Restrict.atOnce()
	if err != nil {
		return row, fmt.Errorf("restrict: %w", err)
	}
	if said {
		row.AtOnce = atOnce
	}

	return row, nil
}

// Policy is the table this configuration says: each row's defaults with what
// it says in their place -- `other` over what `temporary` came to, being as
// it is -- and refused when it says no schedule.
func (c DunningConfig) Policy() (dunning.Policy, error) {
	out := dunning.Defaults()
	var err error
	for _, r := range []struct {
		name string
		k    DunningKind
		row  *dunning.Row
	}{
		{"temporary", c.Kinds.Temporary, &out.Temporary},
		{"replace", c.Kinds.Replace, &out.Replace},
		{"fraud", c.Kinds.Fraud, &out.Fraud},
	} {
		if *r.row, err = r.k.over(*r.row); err != nil {
			return out, fmt.Errorf("kinds.%s: %w", r.name, err)
		}
	}
	if out.Other, err = c.Kinds.Other.over(out.Temporary); err != nil {
		return out, fmt.Errorf("kinds.other: %w", err)
	}
	if out.Payerless, err = (DunningKind{Grace: c.Kinds.Payerless.Grace, Restrict: c.Kinds.Payerless.Restrict}).over(out.Payerless); err != nil {
		return out, fmt.Errorf("kinds.payerless: %w", err)
	}
	if err := out.Check(); err != nil {
		return out, fmt.Errorf("kinds.%w", err)
	}

	return out, nil
}

// DunEvery is [DunningConfig.Every], or its default.
func (c DunningConfig) DunEvery() time.Duration {
	if c.Every <= 0 {
		return invoices.DefaultDunEvery
	}
	return c.Every
}

// GrantsConfig is how what levyan gives products' workspaces is kept
// (`server/grants`): how far a grant that renews reaches, and how often the
// loop `levyan.grants.renew` keeps them.
type GrantsConfig struct {
	// Horizon is how far a grant that renews reaches from its last renewal:
	// a week when unset. It is how long a product that stops hearing from
	// levyan keeps what it was given -- the most a copy of a grant can be
	// stale by -- and a grant is renewed once less than half of it is left.
	Horizon time.Duration `yaml:"horizon"`

	// Renew is how often the loop gives what the rules want, ends what they
	// no longer want, renews what is due and erases what has been over for
	// long enough: an hour when unset.
	Renew time.Duration `yaml:"renew"`
}

// HorizonOf is [GrantsConfig.Horizon], or its default.
func (c GrantsConfig) HorizonOf() time.Duration {
	if c.Horizon <= 0 {
		return 7 * 24 * time.Hour
	}
	return c.Horizon
}

// RenewEvery is [GrantsConfig.Renew], or its default.
func (c GrantsConfig) RenewEvery() time.Duration {
	if c.Renew <= 0 {
		return time.Hour
	}
	return c.Renew
}

// PeriodsConfig is when a product's periods are closed: by the loop
// `levyan.periods.close`, once a period has ended and waited out a grace for
// what comes late.
type PeriodsConfig struct {
	// Grace is how long after a period ends what comes late is waited for
	// before the loop closes it: 48h when unset. What comes later still is
	// kept, and corrected on a later statement.
	Grace time.Duration `yaml:"grace"`

	// Every is how often periods that are due are closed: an hour when unset.
	Every time.Duration `yaml:"every"`
}

// GraceAfter is [PeriodsConfig.Grace], or its default.
func (c PeriodsConfig) GraceAfter() time.Duration {
	if c.Grace <= 0 {
		return 48 * time.Hour
	}
	return c.Grace
}

// CloseEvery is [PeriodsConfig.Every], or its default.
func (c PeriodsConfig) CloseEvery() time.Duration {
	if c.Every <= 0 {
		return time.Hour
	}
	return c.Every
}

// ReadingsConfig is what is expected of the readings products send: how long
// a workspace that has been sending them may go without one before it is a
// gap, warned of in the log and in the gauge `levyan.readings.gaps` by the
// loop that looks for them (`levyan.readings.gaps`).
//
// It is about the warning and nothing else. How long a reading speaks for its
// value when no later one comes is the rating's (`usage.Hold`), since a bill
// moved by a threshold somebody tuned for an alert is a bill nobody can
// explain.
type ReadingsConfig struct {
	// Gap is how long a workspace may go without a reading of a stock meter it
	// has been sending readings of: 36h when unset, a day's reading and half a
	// day more.
	Gap time.Duration `yaml:"gap"`

	// Recent is how long ago a workspace's last reading of a meter may be for
	// it still to be looked for: a week when unset. One silent for longer is
	// gone from the product, most likely, and has been warned of all that
	// while.
	Recent time.Duration `yaml:"recent"`

	// Every is how often they are looked for: an hour when unset.
	Every time.Duration `yaml:"every"`
}

// GapAfter is [ReadingsConfig.Gap], or its default.
func (c ReadingsConfig) GapAfter() time.Duration {
	if c.Gap <= 0 {
		return 36 * time.Hour
	}
	return c.Gap
}

// RecentWithin is [ReadingsConfig.Recent], or its default.
func (c ReadingsConfig) RecentWithin() time.Duration {
	if c.Recent <= 0 {
		return 7 * 24 * time.Hour
	}
	return c.Recent
}

// LookEvery is [ReadingsConfig.Every], or its default.
func (c ReadingsConfig) LookEvery() time.Duration {
	if c.Every <= 0 {
		return time.Hour
	}
	return c.Every
}

// AdminConfig is the operator's entry point: a listener of its own that only
// the operators' network reaches.
type AdminConfig struct {
	// Addr is where it listens: the loopback by default, and an address on
	// the operators' network at most -- never one the public reaches.
	Addr string `yaml:"addr"`

	// Keys are the operators, by name, each with the key they call with: a
	// request carries one as a bearer token, and what it writes is on the
	// trail as theirs (`Operator`). A key is a secret -- `${file:/run/…}` or
	// `${env:…}`, never written into a file somebody reads -- of 32
	// characters or more, since this door opens every product there is. None
	// is a door nobody opens, and `levyan-admin serve` refuses to stand one up.
	Keys map[string]cfg.Secret `yaml:"keys"`
}

// ListenAddr is where the operator's entry point listens.
func (c AdminConfig) ListenAddr() string {
	if c.Addr == "" {
		return "127.0.0.1:8081"
	}

	return c.Addr
}
