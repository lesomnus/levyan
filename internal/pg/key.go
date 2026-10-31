package pg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// BillingKey is a PG's key to a registered method (빌링키). With levyan's
// secret key it charges the payer's card, and nothing else is asked for -- so
// it is a secret, and says nothing of itself: printed or logged it is
// "[billing key]", and marshalled it is an error, never the key. [Reveal] is
// the one way to its value, and [BillingKey.Seal] the one way it is kept.
type BillingKey struct{ v string }

const hidden = "[billing key]"

// errMarshal is a billing key asked to be written out.
var errMarshal = errors.New("pg: a billing key is not marshalled: Seal it for the row that keeps it")

// NewBillingKey is v as a billing key, from the PG that issued it. One the
// store kept is [OpenBillingKey]'s.
func NewBillingKey(v string) BillingKey { return BillingKey{v} }

// Reveal is the key itself, for the request that sends it to the PG (package
// doc). Nothing else asks for it: the store has [BillingKey.Seal].
func (k BillingKey) Reveal() string { return k.v }

// Sealer is what keeps billing keys sealed at rest: the deployment's keyring
// (internal/keyring), which seals under a key held apart from the database,
// for what the associated data names, and opens what any of its keys sealed.
type Sealer interface {
	Seal(plain, ad []byte) (string, error)
	Open(sealed string, ad []byte) ([]byte, error)
}

// sealedFor is what a billing key is sealed for: that it is a billing key,
// and the row it is kept in. The first is what keeps one from opening as
// anything else sealed for the same row under the same keys -- another
// column of it.
func sealedFor(row string) []byte {
	return append([]byte("levyan pg.BillingKey\x00"), row...)
}

var errNoRow = errors.New("pg: a billing key is sealed for the row it is kept in, and none was named")

// Seal is the key sealed by s for the row it is kept in -- row is that row's
// id -- which is what the store writes in its place. It opens for that row
// alone ([OpenBillingKey]): copied to another, it charges nobody.
func (k BillingKey) Seal(s Sealer, row string) (string, error) {
	switch {
	case k.IsZero():
		return "", errors.New("pg: there is no billing key to seal")
	case row == "":
		return "", errNoRow
	}
	return s.Seal([]byte(k.v), sealedFor(row))
}

// OpenBillingKey is the billing key [BillingKey.Seal] sealed for the row whose
// id row is. One sealed for another row, or changed since, does not open, and
// the error says nothing of what was sealed.
func OpenBillingKey(s Sealer, sealed, row string) (BillingKey, error) {
	if row == "" {
		return BillingKey{}, errNoRow
	}
	b, err := s.Open(sealed, sealedFor(row))
	if err != nil {
		return BillingKey{}, fmt.Errorf("pg: the billing key does not open: %w", err)
	}
	if len(b) == 0 {
		return BillingKey{}, errors.New("pg: what was sealed is no billing key")
	}
	return BillingKey{string(b)}, nil
}

// IsZero is whether there is no key.
func (k BillingKey) IsZero() bool { return k.v == "" }

// Digest is the key's SHA-256, in hex: what finds the method of a key a
// webhook names without opening every sealed one. The key is the PG's, long
// and random, so its digest gives nothing of it away.
func (k BillingKey) Digest() string { return Digest(k.v) }

// Digest is the digest a key of value v has ([BillingKey.Digest]), for a key
// a webhook names, which is never held as a [BillingKey].
func Digest(v string) string {
	d := sha256.Sum256([]byte(v))
	return hex.EncodeToString(d[:])
}

func (k BillingKey) String() string   { return hidden }
func (k BillingKey) GoString() string { return hidden }

// Format writes what [BillingKey.String] does, whatever the verb.
func (k BillingKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, hidden) }

func (k BillingKey) LogValue() slog.Value { return slog.StringValue(hidden) }

// MarshalJSON refuses: a key written out is a key kept somewhere nobody
// meant, and an error says so where a placeholder would lose it quietly.
func (k BillingKey) MarshalJSON() ([]byte, error) { return nil, errMarshal }

// MarshalText refuses, as [BillingKey.MarshalJSON] does.
func (k BillingKey) MarshalText() ([]byte, error) { return nil, errMarshal }

var (
	_ fmt.Stringer   = BillingKey{}
	_ fmt.GoStringer = BillingKey{}
	_ fmt.Formatter  = BillingKey{}
	_ slog.LogValuer = BillingKey{}
)
