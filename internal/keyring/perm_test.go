package keyring

import (
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

// Who may hold a key file is PostgreSQL's rule for its server.key: the
// process's own file, nobody else's at all; or root's, which its group may
// read as well -- the Kubernetes Secret mounted 0440 with an fsGroup -- and
// never anybody else's.
func TestAKeyFileIsHeldToPostgreSQLsRule(t *testing.T) {
	const self, someone = 65532, 1000

	for _, tc := range []struct {
		mode  fs.FileMode
		uid   int
		known bool
		ok    bool
	}{
		// The process's own: its alone.
		{0o400, self, true, true},
		{0o600, self, true, true},
		{0o700, self, true, true},
		{0o440, self, true, false},
		{0o640, self, true, false},
		{0o460, self, true, false},
		{0o604, self, true, false},
		{0o644, self, true, false},

		// Root's: its group may read it too, and no more.
		{0o400, 0, true, true},
		{0o600, 0, true, true},
		{0o440, 0, true, true},
		{0o640, 0, true, true},
		{0o460, 0, true, false},
		{0o450, 0, true, false},
		{0o660, 0, true, false},
		{0o444, 0, true, false},
		{0o604, 0, true, false},
		{0o644, 0, true, false},

		// Anybody else's, whatever its mode.
		{0o400, someone, true, false},
		{0o440, someone, true, false},

		// An owner the system does not say: as the process's own.
		{0o400, 0, false, true},
		{0o440, 0, false, false},
	} {
		err := perm(tc.mode, tc.uid, tc.known, self)
		name := fmt.Sprintf("%04o of uid %d (known %v)", tc.mode, tc.uid, tc.known)
		if tc.ok {
			require.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		if tc.uid == someone {
			require.Contains(t, err.Error(), "uid 1000", name)
		} else {
			require.Contains(t, err.Error(), fmt.Sprintf("%04o", tc.mode), name)
		}
	}

	require.NoError(t, perm(0o440, 0, true, 0), "a process of root's reads root's file its group may read")
	require.NoError(t, perm(0o600, 0, true, 0))
}
