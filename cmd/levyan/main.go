// Command levyan is levyan's public binary: what products call, with their
// service keys (`levyan serve`), and the migration a deployment runs before it
// serves a build (`levyan migrate`).
//
// What to read first is `cmd/serve.go`: it is the stack, written out -- which
// layers, in which order, and which server the wall is on. `cli` is the
// command line over it and the door this binary puts on it. The operator's
// door is another binary, `levyan-admin`, which this one does not link.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/lesomnus/levyan/cli"
	"github.com/lesomnus/levyan/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var c cmd.Config
	if err := cli.Cmd(&c).Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "levyan:", err)
		os.Exit(1)
	}
}
