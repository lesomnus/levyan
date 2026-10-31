package cli

import (
	"context"

	"github.com/lesomnus/xli"

	"github.com/lesomnus/levyan/cmd"
)

// NewCmdMigrate is `levyan migrate`: the database brought into the shape this
// build's schema says, and nothing served.
//
// It is the step a deployment takes before it serves a new build, with
// `db.migrate: false` on what serves -- which then refuses a database whose
// shape is not its own rather than altering tables itself. Running it twice
// changes nothing the second time.
//
// There is no `init`. The first rows of a deployment are a product and its
// key, and those are an operator's to write, through `levyan-admin`.
func NewCmdMigrate(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "migrate",
		Brief: "bring the database into this build's shape, and serve nothing",
		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			ctx, done, err := cmd.Telemetry(ctx, c)
			if err != nil {
				return err
			}
			defer done()

			s, err := cmd.Build(ctx, *c)
			if err != nil {
				return err
			}
			defer s.Close()

			if err := Migrate(ctx, s); err != nil {
				return err
			}
			self.Printf("the database is in this build's shape\n")

			return next(ctx)
		}),
	}
}
