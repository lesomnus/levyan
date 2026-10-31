package admincli

// What this binary opens and hears, as `levyan`'s `cli` imports it: the
// databases, what PostgreSQL makes of several processes, and the trail's
// witnesses a configuration may name. The same lists as there, since the two
// read one configuration (cmd/levyan's test holds them to each other).
import (
	_ "github.com/lesomnus/payday/config/brokerpg"
	_ "github.com/lesomnus/payday/config/dbpgx"
	_ "github.com/lesomnus/payday/config/dbsqlite3"
	_ "github.com/lesomnus/payday/config/leasepg"
	_ "github.com/lesomnus/payday/config/witnesslog"
	_ "github.com/lesomnus/payday/config/witnessstore"
)
