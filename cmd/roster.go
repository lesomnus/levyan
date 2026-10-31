package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/roster/standing"
)

// rosterAt is the header a deployment key's call names the directory it is
// about in, and how a directory is written there (roster's `front.HeaderAt`,
// `front.TenantMark`).
const (
	rosterAt   = "roster-at"
	rosterMark = "@"
)

// Roster is the roster `c` names, as roster's `standing` asks it: dialed --
// lazily, so that a roster down is not a levyan that will not start -- with
// the key levyan holds there, and the directory its accounts are in. Nil, and
// nothing to close, when `c` names no roster.
func Roster(ctx context.Context, c RosterConfig) (*standing.Accounts, func() error, error) {
	if !c.Configured() {
		return nil, func() error { return nil }, nil
	}

	key, err := c.Key.Value()
	if err != nil {
		return nil, nil, fmt.Errorf("auth.roster.key: %w", err)
	}
	key = strings.TrimSpace(key)

	var as func(ctx context.Context) (context.Context, error)
	switch {
	case strings.HasPrefix(key, "rk_"):
		if c.Directory == "" {
			return nil, nil, errors.New("auth.roster.directory: a deployment key names the directory its accounts are in on every call, and this one names none")
		}
		at := rosterMark + strings.TrimPrefix(c.Directory, rosterMark)
		as = func(ctx context.Context) (context.Context, error) {
			return metadata.AppendToOutgoingContext(auth.BearerProvider(key).Provide(ctx), rosterAt, at), nil
		}
	case strings.HasPrefix(key, "rt_"):
		if c.Directory != "" {
			return nil, nil, errors.New("auth.roster.directory: a tenant key is its own directory's, and names no other")
		}
		as = func(ctx context.Context) (context.Context, error) {
			return auth.BearerProvider(key).Provide(ctx), nil
		}
	default:
		return nil, nil, errors.New("auth.roster.key: neither rk_ nor rt_, which is how roster says what a key is")
	}

	var creds credentials.TransportCredentials
	switch {
	case c.Insecure:
		creds = insecure.NewCredentials()
	case c.CaFile != "":
		pem, err := os.ReadFile(c.CaFile)
		if err != nil {
			return nil, nil, fmt.Errorf("auth.roster.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("auth.roster.ca_file: %s holds no certificate", c.CaFile)
		}
		creds = credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	default:
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}

	conn, err := grpc.NewClient(c.Addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("auth.roster.addr: %w", err)
	}
	accounts, err := standing.New(standing.Config{Conn: conn, As: as})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}

	shape := "a tenant key, of its own directory"
	if strings.HasPrefix(key, "rk_") {
		shape = "a deployment key, in @" + strings.TrimPrefix(c.Directory, rosterMark)
	}
	log.From(ctx).InfoContext(ctx, "roster", "addr", c.Addr, "key", shape)

	return accounts, conn.Close, nil
}
