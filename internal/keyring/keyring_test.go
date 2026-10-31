package keyring_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/keyring"
)

func key(t *testing.T) []byte {
	t.Helper()
	return []byte(rand.Text() + rand.Text())[:keyring.Size]
}

func ring(t *testing.T, seal string, keys map[string][]byte) *keyring.Keyring {
	t.Helper()
	r, err := keyring.New(seal, keys)
	require.NoError(t, err)
	return r
}

var (
	row   = []byte("0193e1f2-7a3c-7d4e-9f10-2b3c4d5e6f70")
	other = []byte("0193e1f2-7a3c-7d4e-9f10-2b3c4d5e6f71")
	plain = []byte("Z_t5vOvQxrj4499PeiJcjen28-V2RyqgYTwN44Rdzk0=")
)

// A value opens for what it was sealed for, names the key that sealed it,
// and says nothing of what it holds; sealed twice it is two values.
func TestAValueSealedOpensForWhatItWasSealedFor(t *testing.T) {
	r := ring(t, "k1", map[string][]byte{"k1": key(t)})

	v, err := r.Seal(plain, row)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(v, "v1:k1:"), v)
	require.NotContains(t, v, string(plain))
	id, err := keyring.KeyOf(v)
	require.NoError(t, err)
	require.Equal(t, "k1", id)

	got, err := r.Open(v, row)
	require.NoError(t, err)
	require.Equal(t, plain, got)

	again, err := r.Seal(plain, row)
	require.NoError(t, err)
	require.NotEqual(t, v, again, "a nonce of its own every time")

	empty, err := r.Seal(nil, row)
	require.NoError(t, err)
	got, err = r.Open(empty, row)
	require.NoError(t, err)
	require.Empty(t, got)
}

// Moved to another row, a value opens for nobody; and nothing is sealed or
// opened for nothing in particular.
func TestAValueDoesNotOpenForAnythingElse(t *testing.T) {
	r := ring(t, "k1", map[string][]byte{"k1": key(t)})
	v, err := r.Seal(plain, row)
	require.NoError(t, err)

	_, err = r.Open(v, other)
	require.ErrorIs(t, err, keyring.ErrOpen)
	require.NotContains(t, err.Error(), string(plain))

	_, err = r.Seal(plain, nil)
	require.Error(t, err)
	_, err = r.Open(v, nil)
	require.Error(t, err)
	_, err = r.Open(v, []byte{})
	require.Error(t, err)
}

// A value opens under the key that sealed it and no other: not another key
// of the same id, not a keyring without it, and not with its header made to
// name another key the keyring has.
func TestAValueDoesNotOpenUnderAnotherKey(t *testing.T) {
	k1, k2 := key(t), key(t)
	r := ring(t, "k1", map[string][]byte{"k1": k1, "k2": k2})
	v, err := r.Seal(plain, row)
	require.NoError(t, err)

	impostor := ring(t, "k1", map[string][]byte{"k1": k2})
	_, err = impostor.Open(v, row)
	require.ErrorIs(t, err, keyring.ErrOpen, "another key under the same id")

	without := ring(t, "k2", map[string][]byte{"k2": k2})
	_, err = without.Open(v, row)
	require.ErrorIs(t, err, keyring.ErrNoKey)
	require.Contains(t, err.Error(), `"k1"`, "the key that is missing is named")

	moved := "v1:k2:" + strings.TrimPrefix(v, "v1:k1:")
	_, err = r.Open(moved, row)
	require.ErrorIs(t, err, keyring.ErrOpen, "the header is sealed in")

	same := ring(t, "k3", map[string][]byte{"k1": k1, "k3": k1})
	moved = "v1:k3:" + strings.TrimPrefix(v, "v1:k1:")
	_, err = same.Open(moved, row)
	require.ErrorIs(t, err, keyring.ErrOpen, "the same bytes under another id are another key")
}

// A value changed in any way does not open, and what is no sealed value is
// said to be none.
func TestAChangedValueDoesNotOpen(t *testing.T) {
	r := ring(t, "k1", map[string][]byte{"k1": key(t)})
	v, err := r.Seal(plain, row)
	require.NoError(t, err)

	data := strings.TrimPrefix(v, "v1:k1:")
	b, err := base64.RawURLEncoding.DecodeString(data)
	require.NoError(t, err)
	for i := range b {
		c := bytes.Clone(b)
		c[i] ^= 1
		_, err := r.Open("v1:k1:"+base64.RawURLEncoding.EncodeToString(c), row)
		require.ErrorIs(t, err, keyring.ErrOpen, "byte %d", i)
	}
	for _, n := range []int{0, 1, 12, 27, len(b) - 1} {
		_, err := r.Open("v1:k1:"+base64.RawURLEncoding.EncodeToString(b[:n]), row)
		require.Error(t, err, "%d bytes of it", n)
	}

	for _, s := range []string{
		"", "v1", "v1:", "v1:k1", "v1:k1:", "v2:k1:" + data, "V1:k1:" + data,
		"v1::" + data, "v1:k 1:" + data, "v1:k1:" + data + "*", string(plain),
	} {
		_, err := r.Open(s, row)
		require.ErrorIs(t, err, keyring.ErrNotSealed, "%q", s)
		_, err = keyring.KeyOf(s)
		if !strings.HasPrefix(s, "v1:k1:") {
			require.ErrorIs(t, err, keyring.ErrNotSealed, "%q", s)
		}
	}
}

// A key is rotated in four steps (package doc), and nothing sealed on the way
// is lost: every value opens at every step, and once what the old key sealed
// is sealed again, the old key goes.
func TestAKeyIsRotated(t *testing.T) {
	old, nu := key(t), key(t)

	before := ring(t, "old", map[string][]byte{"old": old})
	v, err := before.Seal(plain, row)
	require.NoError(t, err)

	// 1. The new key is added, and the old one still seals.
	added := ring(t, "old", map[string][]byte{"old": old, "new": nu})
	w, err := added.Seal(plain, other)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(w, "v1:old:"))
	_, err = before.Open(w, other)
	require.NoError(t, err, "a process not yet told of the new key opens what this one sealed")

	// 2. The new key seals; every key opens.
	turned := ring(t, "new", map[string][]byte{"old": old, "new": nu})
	require.Equal(t, "new", turned.Current())
	require.Equal(t, []string{"new", "old"}, turned.IDs())
	x, err := turned.Seal(plain, row)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(x, "v1:new:"))
	for _, s := range []string{v, w, x} {
		_, err := turned.Open(s, map[string][]byte{v: row, w: other, x: row}[s])
		require.NoError(t, err)
	}

	// 3. What the old key sealed is sealed again under the new one, for the
	// same row; what the new one sealed is left as it is.
	moved, ok, err := turned.Reseal(v, row)
	require.NoError(t, err)
	require.True(t, ok)
	id, err := keyring.KeyOf(moved)
	require.NoError(t, err)
	require.Equal(t, "new", id)
	got, err := turned.Open(moved, row)
	require.NoError(t, err)
	require.Equal(t, plain, got)
	_, err = turned.Open(moved, other)
	require.ErrorIs(t, err, keyring.ErrOpen, "sealed again for the same row and no other")

	same, ok, err := turned.Reseal(x, row)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, x, same)

	_, _, err = turned.Reseal(w, row)
	require.ErrorIs(t, err, keyring.ErrOpen, "a value is sealed again only for what it was sealed for")

	// 4. The old key goes: what was sealed again opens, and what was not is
	// said to be of a key that is gone.
	after := ring(t, "new", map[string][]byte{"new": nu})
	got, err = after.Open(moved, row)
	require.NoError(t, err)
	require.Equal(t, plain, got)
	_, err = after.Open(w, other)
	require.ErrorIs(t, err, keyring.ErrNoKey)
	require.Contains(t, err.Error(), `"old"`)
}

// A keyring that is not there says so, rather than sealing nothing or
// panicking: a deployment that names no keys has none.
func TestNoKeyringSealsNothing(t *testing.T) {
	var r *keyring.Keyring
	_, err := r.Seal(plain, row)
	require.Error(t, err)
	_, err = r.Open("v1:k1:AAAA", row)
	require.Error(t, err)
	_, _, err = r.Reseal("v1:k1:AAAA", row)
	require.Error(t, err)
	require.Equal(t, "", r.Current())
	require.Equal(t, "no keyring", fmt.Sprint(r))
}

func TestAKeyringIsMadeOfKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		seal string
		keys map[string][]byte
	}{
		"none":                   {"k1", nil},
		"a short key":            {"k1", map[string][]byte{"k1": key(t)[:31]}},
		"a long key":             {"k1", map[string][]byte{"k1": append(key(t), 'x')}},
		"no id":                  {"", map[string][]byte{"": key(t)}},
		"an id with a colon":     {"k:1", map[string][]byte{"k:1": key(t)}},
		"an id from a dash":      {"-k1", map[string][]byte{"-k1": key(t)}},
		"an id of 65":            {strings.Repeat("k", 65), map[string][]byte{strings.Repeat("k", 65): key(t)}},
		"a seal of none of them": {"k2", map[string][]byte{"k1": key(t)}},
	} {
		_, err := keyring.New(tc.seal, tc.keys)
		require.Error(t, err, name)
	}

	r, err := keyring.New("2026-10.a_1", map[string][]byte{"2026-10.a_1": key(t), strings.Repeat("k", 64): key(t)})
	require.NoError(t, err)
	require.Equal(t, "2026-10.a_1", r.Current())
}

// A keyring prints, logs and errs as the ids of its keys and nothing of
// them, and nothing of what it sealed.
func TestAKeyringSaysNothingOfItsKeys(t *testing.T) {
	k := key(t)
	r := ring(t, "k1", map[string][]byte{"k1": k})
	v, err := r.Seal(plain, row)
	require.NoError(t, err)

	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("keys", "keyring", r)
	slog.New(slog.NewJSONHandler(&b, nil)).Info("keys", "keyring", r)
	_, err = r.Open(v, other)
	for _, s := range []string{
		fmt.Sprintf("%v %+v %#v %s %q %x %d", r, r, r, r, r, r, r),
		fmt.Sprintf("%v %+v", *r, *r),
		b.String(),
		err.Error(),
	} {
		for _, secret := range [][]byte{k, plain} {
			require.NotContains(t, s, string(secret))
			require.NotContains(t, s, hex.EncodeToString(secret))
			require.NotContains(t, s, base64.StdEncoding.EncodeToString(secret))
			require.NotContains(t, s, base64.RawURLEncoding.EncodeToString(secret))
		}
	}
	require.Equal(t, "keyring of k1, sealing with k1", fmt.Sprint(r))
}

// keyFile is a key written to a file of mode m, as an operator writes one.
func keyFile(t *testing.T, b []byte, m fs.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	require.NoError(t, os.Chmod(p, m))
	return p
}

// A key file of the process's own is its alone: one its group or anybody
// else may read, or write, is refused, and said so with its mode and nothing
// of the key. A file of root's, which its group may read too, is perm's test,
// since a test makes no file of root's.
func TestAKeyFileOfTheProcesssOwnIsItsAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file a test makes as root is root's, which its group may read")
	}
	k := key(t)
	for _, m := range []fs.FileMode{0o400, 0o600, 0o700} {
		got, err := keyring.ReadFile(keyFile(t, k, m))
		require.NoError(t, err, "%04o", m)
		require.Equal(t, k, got)
	}
	for _, m := range []fs.FileMode{0o440, 0o640, 0o644, 0o604, 0o444, 0o660, 0o620, 0o602, 0o610} {
		p := keyFile(t, k, m)
		_, err := keyring.ReadFile(p)
		require.Error(t, err, "%04o", m)
		require.Contains(t, err.Error(), fmt.Sprintf("%04o", m))
		require.Contains(t, err.Error(), p)
		require.NotContains(t, err.Error(), string(k))
	}

	// A link is the file it links to, as a Kubernetes Secret's are.
	target := keyFile(t, k, 0o400)
	link := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.Symlink(target, link))
	got, err := keyring.ReadFile(link)
	require.NoError(t, err)
	require.Equal(t, k, got)
	require.NoError(t, os.Chmod(target, 0o644))
	_, err = keyring.ReadFile(link)
	require.Error(t, err)
}

// A key file is 32 random bytes and nothing else: not a newline more, not the
// key in hex or base64, and not a directory or nothing at all.
func TestAKeyFileIsThirtyTwoBytes(t *testing.T) {
	k := key(t)
	for name, b := range map[string][]byte{
		"empty":          {},
		"31 bytes":       k[:31],
		"and a newline":  append(bytes.Clone(k), '\n'),
		"in hex":         []byte(hex.EncodeToString(k)),
		"in base64":      []byte(base64.StdEncoding.EncodeToString(k)),
		"twice the size": append(bytes.Clone(k), k...),
	} {
		p := keyFile(t, b, 0o400)
		_, err := keyring.ReadFile(p)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), fmt.Sprintf("%d bytes", len(b)), name)
		require.NotContains(t, err.Error(), string(k), name)
	}

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	_, err := keyring.ReadFile(dir)
	require.Error(t, err, "a directory")
	_, err = keyring.ReadFile(filepath.Join(dir, "none"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// The configuration says the kind, the keys and which seals, and opens a
// keyring of files that seals and opens.
func TestAConfigurationOpensAKeyringOfFiles(t *testing.T) {
	require.True(t, keyring.Config{}.IsZero())

	k1, k2 := key(t), key(t)
	c := keyring.Config{
		Kind: keyring.KindFile,
		Keys: []keyring.KeyConfig{{ID: "k1", File: keyFile(t, k1, 0o400)}, {ID: "k2", File: keyFile(t, k2, 0o600)}},
		Seal: "k2",
	}
	require.False(t, c.IsZero())
	r, err := c.Open()
	require.NoError(t, err)
	require.Equal(t, "k2", r.Current())
	require.Equal(t, []string{"k1", "k2"}, r.IDs())

	v, err := r.Seal(plain, row)
	require.NoError(t, err)
	got, err := ring(t, "k2", map[string][]byte{"k2": k2}).Open(v, row)
	require.NoError(t, err, "the key in the file is the key")
	require.Equal(t, plain, got)
}

// The kms kind is named, and refused until it is built, with what to do
// instead; and a kind there is not is refused too.
func TestKMSIsNamedAndRefused(t *testing.T) {
	keys := []keyring.KeyConfig{{ID: "k1", File: keyFile(t, key(t), 0o400)}}

	_, err := keyring.Config{Kind: keyring.KindKMS, Keys: keys, Seal: "k1"}.Open()
	require.Error(t, err)
	require.Contains(t, err.Error(), "kms is named and not built yet")
	require.Contains(t, err.Error(), "kind: file")

	_, err = keyring.Config{Kind: keyring.KindKMS}.Open()
	require.Error(t, err, "whatever else it says")

	for _, kind := range []string{"", "vault", "FILE", "files"} {
		_, err := keyring.Config{Kind: kind, Keys: keys, Seal: "k1"}.Open()
		require.Error(t, err, "%q", kind)
		require.True(t, strings.HasPrefix(err.Error(), "kind: "), err.Error())
	}
}

// What a configuration cannot mean is refused, and the field is named.
func TestAConfigurationThatCannotBeIsRefused(t *testing.T) {
	k := key(t)
	f := keyFile(t, k, 0o400)
	copied := keyFile(t, k, 0o400)
	loose := keyFile(t, key(t), 0o644)

	for name, tc := range map[string]struct {
		keys  []keyring.KeyConfig
		seal  string
		field string
	}{
		"no keys":                {nil, "k1", "keys:"},
		"no id":                  {[]keyring.KeyConfig{{File: f}}, "k1", "keys[0].id:"},
		"an id that is no id":    {[]keyring.KeyConfig{{ID: "k:1", File: f}}, "k:1", "keys[0].id:"},
		"an id twice":            {[]keyring.KeyConfig{{ID: "k1", File: f}, {ID: "k1", File: keyFile(t, key(t), 0o400)}}, "k1", "keys[1].id:"},
		"no file":                {[]keyring.KeyConfig{{ID: "k1"}}, "k1", "keys[0].file:"},
		"a file nobody wrote":    {[]keyring.KeyConfig{{ID: "k1", File: filepath.Join(t.TempDir(), "none")}}, "k1", "keys[0].file:"},
		"a file others read":     {[]keyring.KeyConfig{{ID: "k1", File: loose}}, "k1", "keys[0].file:"},
		"the same key twice":     {[]keyring.KeyConfig{{ID: "k1", File: f}, {ID: "k2", File: copied}}, "k2", "keys[1]:"},
		"no key to seal with":    {[]keyring.KeyConfig{{ID: "k1", File: f}}, "", "seal:"},
		"a seal of none of them": {[]keyring.KeyConfig{{ID: "k1", File: f}}, "k2", "seal:"},
	} {
		_, err := keyring.Config{Kind: keyring.KindFile, Keys: tc.keys, Seal: tc.seal}.Open()
		require.Error(t, err, name)
		require.True(t, strings.HasPrefix(err.Error(), tc.field), "%s: %s", name, err)
		require.NotContains(t, err.Error(), string(k), name)
	}
}
