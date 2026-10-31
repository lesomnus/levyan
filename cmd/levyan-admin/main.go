// Command levyan-admin is the operator's entry point and its tool:
// `levyan-admin serve` beside a deployment, on a listener only the operators'
// network reaches, and every other command asking it from wherever the
// operator is. `levyan`, the binary products reach, links none of it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	admincli "github.com/lesomnus/levyan/cli/admin"
	"github.com/lesomnus/levyan/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var c cmd.Config
	if err := admincli.Cmd(&c).Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "levyan-admin:", err)
		os.Exit(1)
	}
}
