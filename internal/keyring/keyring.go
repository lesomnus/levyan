// Package keyring seals what levyan keeps secret at rest -- the billing keys
// a PG issues (internal/pg), which with levyan's own key charge a payer's
// card -- under keys the deployment holds apart from the database, so that a
// copy of the database charges nobody.
//
// Whose it is: that the keys' configuration names a kind, which says where
// the keys come from, and that only `file` is built now, is the owner's
// (levyan#10, 27). That a sealed value names the key that sealed it, so that
// keys can be rotated, and that a KMS kind follows payday's KMS signers, are
// Claude's, the lead's. The rest -- the cipher, the form of a sealed value,
// what a key file has to be -- is the implementation's.
//
// # A sealed value
//
//	v1:<key id>:<the nonce, the ciphertext and its tag, in unpadded base64url>
//
// It is AES-256-GCM, with a nonce of 96 random bits for every seal. The
// associated data is the caller's, and required: what the value is sealed
// for -- the row it is kept in -- so that a value moved to another row opens
// for nobody. The header, the version and the key's id, is sealed in with it,
// so that no part of a sealed value can be changed and still open.
//
// AES-256-GCM rather than XChaCha20-Poly1305: it is the standard library's,
// and an AEAD FIPS 140-3 approves, which ChaCha20-Poly1305 is not -- what an
// audit of payment data asks after; it is what a KMS seals with, for the kms
// kind to come (Vault's Transit aes256-gcm96, the symmetric keys of AWS's and
// Google's KMS), and what payday's sealed sessions are (auth/authsession);
// and the machines levyan runs on do it in hardware. Its one cost is the
// nonce: 96 random bits hold a key to 2^32 seals, and a key seals one value
// for each method a payer registers, and each again when it is rotated --
// thousands, not billions. XChaCha20-Poly1305's 192 bits would buy room
// nothing here uses.
//
// # Rotating a key
//
// Every key of a keyring opens what it sealed, and one of them seals
// ([Keyring.Current]). A key is rotated in four steps, each deployed to every
// process before the next is taken, since a process opens only what its own
// keys sealed:
//
//  1. Add the new key to the keys, and leave which seals as it is.
//  2. Make the new key the one that seals: what is sealed from then on is
//     sealed under it.
//  3. Seal again what the old key sealed ([Keyring.Reseal]), and count what
//     is left of it by the id it names ([KeyOf]; in SQL, values that begin
//     `v1:<old id>:`).
//  4. Take the old key out once nothing that is kept is sealed under it.
//
// A key taken out too early leaves values nothing opens ([ErrNoKey], which
// names the key), and is put back to open them.
//
// # The kinds
//
// A deployment says where its keys are by their kind ([Config], levyan's
// `billing_keys`):
//
//   - `file`: each key 32 random bytes in a file of its own, which its owner
//     alone may read ([ReadFile]): `head -c 32 /dev/urandom > key`, and
//     `chmod 400 key`.
//   - `kms`: named, and not built. A configuration that says it is refused
//     where the process comes up ([Config.Open]).
//
// When kms is built it follows payday's KMS signers (payday's
// config.RegisterSigner, and trail/trailvault beneath it): a package of its
// own for each KMS, which the binary imports as it imports a database
// driver, registering the scheme its keys are named by -- `hashivault://…`,
// `awskms://…` -- and a scheme no linked package registers refused with the
// name of the package that would; the key stays in the KMS, which seals and
// opens with the associated data as its context, and a key the KMS would hand
// out is refused; and every key is tried once where the process comes up,
// before anything is built on it. A value it seals is of the same form, and
// names its key by the same kind of id.
//
// # What it never says
//
// A key, and what was sealed: neither is in an error, a log line or anything
// a keyring prints, which is the ids of its keys.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

// Size is how long a key is: 32 bytes, AES-256's.
const Size = 32

// version is the form of a sealed value, its first part.
const version = "v1"

var (
	// ErrNotSealed is a value that is not one a keyring sealed: not of the
	// form, or of another version of it.
	ErrNotSealed = errors.New("keyring: not a sealed value")

	// ErrNoKey is a value sealed under a key the keyring does not have: one
	// taken out before what it sealed was sealed again (package doc).
	ErrNoKey = errors.New("keyring: sealed under a key this keyring does not have")

	// ErrOpen is a value that does not open under the key it names: sealed
	// for something other than what it is opened for -- moved to another
	// row -- or changed since.
	ErrOpen = errors.New("keyring: does not open")

	errNoAD = errors.New("keyring: a value is sealed for something, the row it is kept in, and nothing was named")

	// errNone is a keyring asked of where there is none: a deployment that
	// names no keys.
	errNone = errors.New("keyring: there is none, since the deployment names no keys")
)

// Keyring is the keys a deployment seals with and opens with. It holds
// nothing that changes, and is used by many goroutines at once.
type Keyring struct {
	current string
	keys    map[string]cipher.AEAD
}

// New is a keyring of keys, by id, of [Size] bytes each, that seals with the
// one whose id is seal. It keeps none of the slices it is handed, which the
// caller may clear.
func New(seal string, keys map[string][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("keyring: no keys")
	}

	r := &Keyring{current: seal, keys: make(map[string]cipher.AEAD, len(keys))}
	for id, k := range keys {
		if !idOK(id) {
			return nil, fmt.Errorf("keyring: %q is no key id: %s", id, idRule)
		}
		if len(k) != Size {
			return nil, fmt.Errorf("keyring: key %q is %d bytes, and a key is %d", id, len(k), Size)
		}
		b, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("keyring: key %q: %w", id, err)
		}
		g, err := cipher.NewGCMWithRandomNonce(b)
		if err != nil {
			return nil, fmt.Errorf("keyring: key %q: %w", id, err)
		}
		r.keys[id] = g
	}
	if _, ok := r.keys[seal]; !ok {
		return nil, fmt.Errorf("keyring: %q, which is to seal, is none of its keys", seal)
	}

	return r, nil
}

// Current is the id of the key that seals.
func (r *Keyring) Current() string {
	if r == nil {
		return ""
	}
	return r.current
}

// IDs is the id of every key of the keyring, which is every key that opens,
// in order.
func (r *Keyring) IDs() []string {
	if r == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(r.keys))
}

// Seal is plain sealed under the current key, for what ad names: the row the
// value is kept in. ad is required, since a value sealed for nothing in
// particular would open wherever it was moved to.
func (r *Keyring) Seal(plain, ad []byte) (string, error) {
	switch {
	case r == nil:
		return "", errNone
	case len(ad) == 0:
		return "", errNoAD
	}
	g, ok := r.keys[r.current]
	if !ok {
		return "", errors.New("keyring: no key to seal with")
	}

	h := header(r.current)
	out := g.Seal(nil, nil, plain, bound(h, ad))
	return h + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open is what v was sealed from, by whichever of the keyring's keys sealed
// it, for what ad names: [ErrNoKey] when that key is not the keyring's, and
// [ErrOpen] when v does not open for ad -- sealed for another row, or changed.
func (r *Keyring) Open(v string, ad []byte) ([]byte, error) {
	switch {
	case r == nil:
		return nil, errNone
	case len(ad) == 0:
		return nil, errNoAD
	}
	id, data, err := parse(v)
	if err != nil {
		return nil, err
	}
	g, ok := r.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoKey, id)
	}
	b, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return nil, ErrNotSealed
	}

	plain, err := g.Open(nil, nil, b, bound(header(id), ad))
	if err != nil {
		// GCM says no more than that it does not open, and nothing of what
		// was sealed is in what it says.
		return nil, fmt.Errorf("%w under key %q: it was sealed for something else, or changed", ErrOpen, id)
	}
	return plain, nil
}

// Reseal is v sealed again under the current key, for the same ad, and
// whether it moved: a value the current key sealed is answered as it is. It
// is the third step of a rotation (package doc). It opens every value it is
// handed, so one that does not open is an error here rather than at the
// charge that needs it.
func (r *Keyring) Reseal(v string, ad []byte) (string, bool, error) {
	plain, err := r.Open(v, ad)
	if err != nil {
		return "", false, err
	}
	defer clear(plain)

	if id, _ := KeyOf(v); id == r.current {
		return v, false, nil
	}
	s, err := r.Seal(plain, ad)
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

// KeyOf is the id of the key that sealed v, read off v without opening it:
// what a rotation counts what is left of an old key by.
func KeyOf(v string) (string, error) {
	id, _, err := parse(v)
	return id, err
}

// header is what a value sealed under the key id begins with.
func header(id string) string { return version + ":" + id + ":" }

// bound is the associated data a value is sealed with: its header, and then
// what the caller named. The header ends at its second colon and an id has
// none, so no two pairs of the two are the same bytes.
func bound(header string, ad []byte) []byte {
	return append([]byte(header), ad...)
}

// parse is the key id and the data of a sealed value.
func parse(v string) (id, data string, err error) {
	ver, rest, ok := strings.Cut(v, ":")
	if !ok || ver != version {
		return "", "", ErrNotSealed
	}
	id, data, ok = strings.Cut(rest, ":")
	if !ok || !idOK(id) || data == "" {
		return "", "", ErrNotSealed
	}
	return id, data, nil
}

const idRule = "1 to 64 letters, digits, '.', '_' or '-', from a letter or a digit"

// idOK is whether s may be a key's id ([idRule]): never with a ':', which
// ends the id in a sealed value.
func idOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// String is what a keyring is: the ids of its keys, and which seals. Nothing
// of a key is printed, whatever the verb.
func (r *Keyring) String() string {
	if r == nil {
		return "no keyring"
	}
	return fmt.Sprintf("keyring of %s, sealing with %s", strings.Join(r.IDs(), ", "), r.current)
}

func (r *Keyring) GoString() string { return r.String() }

// Format writes what [Keyring.String] does, whatever the verb.
func (r *Keyring) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

func (r *Keyring) LogValue() slog.Value { return slog.StringValue(r.String()) }

var (
	_ fmt.Stringer   = (*Keyring)(nil)
	_ fmt.GoStringer = (*Keyring)(nil)
	_ fmt.Formatter  = (*Keyring)(nil)
	_ slog.LogValuer = (*Keyring)(nil)
)
