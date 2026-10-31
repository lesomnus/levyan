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
// the one way to its value.
type BillingKey struct{ v string }

const hidden = "[billing key]"

// errMarshal is a billing key asked to be written out.
var errMarshal = errors.New("pg: a billing key is not marshalled: Reveal it to the store that seals it")

// NewBillingKey is v as a billing key: from the PG that issued it, or from
// the store that kept it sealed.
func NewBillingKey(v string) BillingKey { return BillingKey{v} }

// Reveal is the key itself, for the request that sends it to the PG and the
// store that seals it (package doc). Nothing else asks for it.
func (k BillingKey) Reveal() string { return k.v }

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
