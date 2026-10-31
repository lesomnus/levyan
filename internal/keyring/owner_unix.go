//go:build unix

package keyring

import (
	"io/fs"
	"syscall"
)

// owner is the user the file fi describes belongs to, by uid, as the system
// says.
func owner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
