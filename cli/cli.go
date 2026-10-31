// Package cli is `levyan`, the binary products call: its command line, and
// the door it puts on the server `cmd` builds ([Front]).
//
// `cmd` is the wiring both binaries share. This package is the public one's
// side of it -- service keys, the policy table, `levyan.v1` -- and
// `cli/admin` is the operator's, and neither imports the other: the public
// binary has no path that writes as the deployment, and the operator's has no
// API a product calls (cmd/levyan's test asks the toolchain, every run).
//
// It is also what only a process needs and a library does not: the database
// engines it opens, the language a Watch filter is evaluated in, the
// migration.
package cli

import (
	"context"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/cfg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/payday/pdcmd"
	"github.com/lesomnus/payday/rename"
	"github.com/lesomnus/payday/valid"

	"github.com/lesomnus/levyan/cmd"
	entmigrate "github.com/lesomnus/levyan/internal/ent/migrate"
	"github.com/lesomnus/levyan/server/pd"
)

// Cmd is this app's own command line: what payday and xli supply, plus
// whatever the app has of its own.
//
// `config`, `config env` and `version` are the commands that run against a
// **deployment** rather than against a checkout, and every one of them needs
// something only the app can hand over. `config env` is the clearest: listing
// the variables a deployment can set means walking this struct, and the struct
// is the app's. `version` is payday's; the two `config` are xli's, beside what
// reads the configuration.
//
// `serve` is not among them and will not be. It is the one command whose body
// is the stack -- which layers, in which order, with the wall on which server
// -- and that is the most important thing a reader of an app can see.
//
// The configuration is read on the **root**, so it has happened whichever
// subcommand runs -- `config` prints what came out, `serve` listens on what it
// says. A command that loaded it for itself would be one more place for the
// order to be wrong. What `c` holds when the loader is made is the defaults:
// every load starts from it, and the file, the environment and the flags go
// over the top.
func Cmd(c *cmd.Config) *xli.Command {
	l := cfg.New(cmd.Name, c)

	// `version` needs no configuration, and is what somebody runs to ask a
	// deployment whose configuration is wrong what build it is.
	version := pdcmd.NewCmdVersion()

	return &xli.Command{
		Name:  cmd.Name,
		Brief: "usage-based billing for payday apps: what products call",

		Flags: flg.Flags{cfg.ConfigFlag()},

		Commands: []*xli.Command{
			version,
			cfg.NewCmdConfig(l),
			NewCmdMigrate(c),
			NewCmdServe(c),
		},

		Handler: xli.Chain(cfg.Load(l, version), xli.RequireSubcommand()),
	}
}

// Migrate brings the database s runs on into the shape this app's schema says:
// `levyan migrate`, which a deployment runs before it serves a build, and the
// one binary that alters tables.
//
// The indexes an earlier build made under another name are moved to this
// build's first: a migration finds an index by its name, and one it does not
// find it makes again, from every row, beside the old one, which it leaves --
// it drops no index. See `rename`.
//
// And for the same reason an index this build declares that PostgreSQL left
// invalid -- a build of it `CONCURRENTLY` that did not finish -- is refused
// before anything is made: found by its name, it would be taken for made, and
// read by nothing from then on. See `valid`.
func Migrate(ctx context.Context, s *cmd.Server) error {
	if err := rename.Indexes(ctx, s.Db, s.Dialect, pd.IndexRenames); err != nil {
		return err
	}
	if err := valid.Indexes(ctx, s.Db, s.Dialect, entmigrate.Tables); err != nil {
		return err
	}

	return entmigrate.NewSchema(s.Drv).Create(ctx)
}
