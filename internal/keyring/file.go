package keyring

import (
	"fmt"
	"io"
	"os"
)

// ReadFile is the key in the file at path, for the file kind: [Size] random
// bytes and nothing else -- no newline, no encoding -- in a regular file that
// only its owner may read or write. A file its group or anybody else may
// read, write or run is refused, as is one of another length. What is
// refused is said with the path and the mode or the length, and never with
// anything that is in the file.
//
// It is asked of the file it opened, not of the path again, so that what is
// read is the file whose mode was asked about; a path that is a link is the
// file it links to, as a Kubernetes Secret's are.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch m := fi.Mode(); {
	case !m.IsRegular():
		return nil, fmt.Errorf("%s: not a file, and a key is 32 bytes in one", path)
	case m.Perm()&0o077 != 0:
		return nil, fmt.Errorf("%s: mode %04o lets its group or others at it, and a key is its owner's alone: chmod 400 it", path, m.Perm())
	case fi.Size() != Size:
		return nil, fmt.Errorf("%s: %d bytes, and a key is %d random bytes and nothing else: head -c %d /dev/urandom", path, fi.Size(), Size, Size)
	}

	b, err := io.ReadAll(io.LimitReader(f, Size+1))
	if err != nil {
		return nil, err
	}
	if len(b) != Size {
		clear(b)
		return nil, fmt.Errorf("%s: changed as it was read", path)
	}
	return b, nil
}
