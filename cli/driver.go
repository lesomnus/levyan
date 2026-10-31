package cli

// The drivers this binary runs on. They are blank-imported by the app rather
// than by payday so that an app does not carry an engine it never opens, and
// `cli/admin` imports the same ones (cmd/levyan's test holds the two lists to
// each other).
//
// SQLite is what a checkout and the tests run on; PostgreSQL is what a
// deployment runs on, with what it makes of several processes on the same
// database: `watch.broker: postgres` (LISTEN/NOTIFY) and `spin.lease:
// postgres` (an advisory lock, so that one process runs each loop).
import (
	_ "github.com/lesomnus/payday/config/brokerpg"
	_ "github.com/lesomnus/payday/config/dbpgx"
	_ "github.com/lesomnus/payday/config/dbsqlite3"
	_ "github.com/lesomnus/payday/config/leasepg"
)
