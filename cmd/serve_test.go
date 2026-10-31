package cmd_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/keyring"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/internal/pg"
)

func keyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "billing.key")
	require.NoError(t, os.WriteFile(p, []byte(rand.Text() + rand.Text())[:keyring.Size], 0o600))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

// The keys billing keys are sealed under are opened where a process comes
// up, and what cannot be opened refuses it there, before the database is
// touched: a kind that is not built, or a key file that is not its owner's.
func TestBillingKeysAreOpenedWhereTheProcessComesUp(t *testing.T) {
	for name, tc := range map[string]struct {
		c    keyring.Config
		says string
	}{
		"kms": {
			keyring.Config{Kind: keyring.KindKMS, Keys: []keyring.KeyConfig{{ID: "k1", File: keyFile(t, 0o400)}}, Seal: "k1"},
			"billing_keys.kind: kms is named and not built yet",
		},
		"a key others may read": {
			keyring.Config{Kind: keyring.KindFile, Keys: []keyring.KeyConfig{{ID: "k1", File: keyFile(t, 0o644)}}, Seal: "k1"},
			"billing_keys.keys[0].file: ",
		},
	} {
		// No database is named: it is not reached.
		_, err := cmd.Build(t.Context(), cmd.Config{BillingKeys: tc.c})
		require.ErrorContains(t, err, tc.says, name)
	}

	w := levyantest.New(t)
	require.Nil(t, w.S.BillingKeys, "a deployment that names none has none")
	_, err := pg.NewBillingKey("a-billing-key").Seal(w.S.BillingKeys, "a-row")
	require.Error(t, err, "and seals nothing")

	w = levyantest.New(t, func(c *cmd.Config) {
		c.BillingKeys = keyring.Config{Kind: keyring.KindFile, Keys: []keyring.KeyConfig{{ID: "2026-10", File: keyFile(t, 0o400)}}, Seal: "2026-10"}
	})
	require.Equal(t, "2026-10", w.S.BillingKeys.Current())
	sealed, err := pg.NewBillingKey("a-billing-key").Seal(w.S.BillingKeys, "a-row")
	require.NoError(t, err)
	k, err := pg.OpenBillingKey(w.S.BillingKeys, sealed, "a-row")
	require.NoError(t, err)
	require.Equal(t, "a-billing-key", k.Reveal())
}
