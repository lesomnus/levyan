//go:build !unix

package keyring

import "io/fs"

// owner is not known where a file has no owner by uid. A key file there is
// held to what a file of the process's own is: no group may read it.
func owner(fs.FileInfo) (int, bool) { return 0, false }
