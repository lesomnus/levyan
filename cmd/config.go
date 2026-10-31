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
	"time"

	"github.com/lesomnus/xli/cfg"

	"github.com/lesomnus/payday/config"
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
