package keyring

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ReadFile is the key in the file at path, for the file kind: [Size] random
// bytes and nothing else -- no newline, no encoding -- in a regular file whose
// owner and mode are what PostgreSQL asks of its server.key ([perm]): the
// process's own and nobody else's (0600 at most), or root's and readable by
// its group as well (0640 at most), as a Kubernetes Secret mounted with
// defaultMode 0440 and an fsGroup is. Anybody else's file, any other mode, or
// another length is refused, and said so with the path and the owner, the
// mode or the length -- never with anything that is in the file.
//
// It is asked of the file it opened, not of the path again, so that what is
// read is the file that was asked about; a path that is a link is the file it
// links to, as a Kubernetes Secret's are.
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
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a file, and a key is 32 bytes in one", path)
	}
	uid, known := owner(fi)
	if err := perm(fi.Mode(), uid, known, os.Geteuid()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fi.Size() != Size {
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

// perm is why a file of mode m may not hold a key, or nil if it may: the
// rule PostgreSQL holds its server.key to (Claude's, the lead's, levyan#23).
// uid is who owns the file, when known says the system said; self is the
// process's effective user.
//
// A key file is the process's own, and then nobody else may do anything with
// it -- 0600 at most -- or root's, and then its group may read it too -- 0640
// at most -- which is how a system shares a credential with a service's
// group, and how Kubernetes mounts a Secret with defaultMode 0440 and an
// fsGroup. Anybody else's is refused whatever its mode, since its owner could
// put another key in it, and a key sealed under a key somebody put there is
// theirs to open; so a group never writes one, nor runs it.
func perm(m fs.FileMode, uid int, known bool, self int) error {
	m = m.Perm()
	root := known && uid == 0
	switch {
	case known && uid != self && !root:
		return fmt.Errorf("owned by uid %d, which is neither this process's user nor root, and could put another key in it", uid)
	case m&0o007 != 0:
		return fmt.Errorf("mode %04o lets others at it, and nobody but its owner, or the group of a file of root's, may read a key: chmod 400 it", m)
	case m&0o030 != 0:
		return fmt.Errorf("mode %04o lets its group write or run it, and a group may at most read a key, of root's: chmod 440 it", m)
	case m&0o040 != 0 && !root:
		return fmt.Errorf("mode %04o lets its group read it, and a group may read a key only of root's: chmod 400 it", m)
	}
	return nil
}
