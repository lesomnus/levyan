package keyring

import (
	"bytes"
	"errors"
	"fmt"
)

// The kinds a keyring's keys may be of (package doc).
const (
	// KindFile is keys in files of their own ([ReadFile]).
	KindFile = "file"

	// KindKMS is keys in a KMS, which is named and not built: a
	// configuration that says it is refused ([Config.Open]).
	KindKMS = "kms"
)

// Config is where a deployment's keys are: levyan's `billing_keys`
// (cmd.Config), for the keys billing keys are sealed under.
//
//	billing_keys:
//	  kind: file
//	  keys:
//	    - id: "2026-10"
//	      file: /run/secrets/levyan/billing-2026-10.key
//	  seal: "2026-10"
type Config struct {
	// Kind is where the keys are: `file`, the one there is. `kms` is named,
	// and refused until it is built.
	Kind string `yaml:"kind"`

	// Keys is every key that opens what it sealed, each by an id of its own,
	// which every value it seals carries.
	Keys []KeyConfig `yaml:"keys"`

	// Seal is the id of the key that seals.
	Seal string `yaml:"seal"`
}

// KeyConfig is one key of a [Config].
type KeyConfig struct {
	// ID is what the values the key seals name it by: 1 to 64 letters,
	// digits, '.', '_' or '-', from a letter or a digit. It is never taken
	// again by another key, since a value names its key by it.
	ID string `yaml:"id"`

	// File is where a key of the file kind is: 32 random bytes, which its
	// owner alone may read ([ReadFile]).
	File string `yaml:"file"`
}

// IsZero is whether c names nothing at all: a deployment that keeps no
// billing key yet, and so has no keyring.
func (c Config) IsZero() bool {
	return c.Kind == "" && len(c.Keys) == 0 && c.Seal == ""
}

// Open is the keyring c says, refused for what cannot be: a kind there is
// not, or `kms`, which is not built yet; no keys, an id that is no id, the
// same id or the same key twice; a key file that is not its owner's alone,
// or not 32 bytes; and a key to seal with that is none of them. Each refusal
// names the field it is of, as the configuration names it.
func (c Config) Open() (*Keyring, error) {
	switch c.Kind {
	case KindFile:
	case KindKMS:
		return nil, errors.New("kind: kms is named and not built yet, and the keys are files for now (kind: file). A KMS's kind comes as payday's KMS signers came: a package of its own that the binary links")
	case "":
		return nil, errors.New("kind: none, and it says where the keys are: file")
	default:
		return nil, fmt.Errorf("kind: %q is no kind of key: file, or kms once it is built", c.Kind)
	}

	if len(c.Keys) == 0 {
		return nil, errors.New("keys: none, and a keyring has one at least")
	}
	keys := make(map[string][]byte, len(c.Keys))
	defer func() {
		// The keyring has what it needs of them; nothing else keeps them.
		for _, k := range keys {
			clear(k)
		}
	}()
	for i, k := range c.Keys {
		_, twice := keys[k.ID]
		switch {
		case !idOK(k.ID):
			return nil, fmt.Errorf("keys[%d].id: %q is no key id: %s", i, k.ID, idRule)
		case twice:
			return nil, fmt.Errorf("keys[%d].id: %q is another key's id too", i, k.ID)
		case k.File == "":
			return nil, fmt.Errorf("keys[%d].file: none, and a key of the file kind is in one", i)
		}

		b, err := ReadFile(k.File)
		if err != nil {
			return nil, fmt.Errorf("keys[%d].file: %w", i, err)
		}
		for id, other := range keys {
			if bytes.Equal(b, other) {
				// A rotation to a copy of the old key, most likely, which
				// would rotate nothing.
				clear(b)
				return nil, fmt.Errorf("keys[%d]: the same key as %q, and a key is one key", i, id)
			}
		}
		keys[k.ID] = b
	}

	_, ok := keys[c.Seal]
	switch {
	case c.Seal == "":
		return nil, errors.New("seal: none, and it is the id of the key that seals")
	case !ok:
		return nil, fmt.Errorf("seal: %q is none of the keys", c.Seal)
	}

	return New(c.Seal, keys)
}
