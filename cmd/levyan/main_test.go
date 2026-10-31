package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// deps is every package the binary in `dir` links, as the toolchain says.
func deps(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)

	return strings.Fields(string(out))
}

// TestTheTwoBinariesLinkNeitherDoorOfTheOther: `levyan` is what products
// reach, and the operator's entry point is a binary of its own
// (`levyan-admin`), so that no path in this one writes as the deployment --
// and the operator's has none of this one's door: no service key is read
// there, and neither `levyan.v1` nor the receiver of flows is served. Each holds only as long as nothing
// imports what the other is made of, which a single import anywhere would
// undo without a word; so it is asked of the toolchain (`go list -deps`),
// every run.
func TestTheTwoBinariesLinkNeitherDoorOfTheOther(t *testing.T) {
	x := require.New(t)

	const m = "github.com/lesomnus/levyan/"
	public, operator := deps(t, "."), deps(t, "../levyan-admin")
	x.Contains(public, m+"cli", "the list is this binary's")
	x.Contains(operator, m+"cli/admin", "and that one is the operator's")

	for _, pkg := range []string{m + "server/admin", m + "cli/admin"} {
		x.NotContains(public, pkg, "levyan links %s", pkg)
	}
	for _, pkg := range []string{m + "cli", m + "server/api", m + "server/otlp", m + "server/policy", m + "api/levyan/v1"} {
		x.NotContains(operator, pkg, "levyan-admin links %s", pkg)
	}

	// And they open and hear what the one configuration they share can name.
	for _, pkg := range []string{
		"config/dbsqlite3", "config/dbpgx", "config/brokerpg", "config/leasepg",
		"config/witnesslog", "config/witnessstore",
	} {
		x.Contains(public, "github.com/lesomnus/payday/"+pkg)
		x.Contains(operator, "github.com/lesomnus/payday/"+pkg)
	}
}
