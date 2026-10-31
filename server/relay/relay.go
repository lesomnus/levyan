// Package relay is a product calling levyan for one of its own users, as
// that user (#4): its service key, and the account it signed the user in as,
// in `levyan-account`. The call is then the account's, made as itself and in
// no product's tenant (payday's account frame, payday#184), and reaches the
// methods an account calls and nothing else.
//
// # Whose word it is
//
// The product's. levyan signs nobody in: the product does, at roster, and
// vouches for the account by sending it beside its key -- so a product's key
// is a credential that may act for any account in what an account may do
// here, and is issued to a product an operator trusts with that (`levyan-admin
// key issue`). What levyan holds the account to is roster's word on it now:
// every call is asked of roster (`standing.Accounts.Held`), and one roster
// says is suspended, erased or in a closed directory is refused -- as is every
// call while no roster is configured, since then nothing can say. The product
// that relayed each call is on the frame (its key's row), so which product's
// workspaces a call is about is the product's that sent it, and the log says
// which key spoke for which account.
//
// That a payer signs in to levyan through roster's issuer itself, as a relying
// party, and calls it directly is the later step (levyan#1, the seventh open
// question).
//
// It is the public binary's door, and the operator's links none of it.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/payday/account"
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/roster/standing"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/payers"
)

// Header is the metadata a product names the account it relays a call for
// in: the canonical text of the account's identifier at roster.
const Header = "levyan-account"

// authority is what an identity a product's key vouches for an account with
// carries as its authority, with the key after it.
const authority = "levyan product key "

// Handler reads a product's key with `keys`, and with [Header] the account it
// vouches for: an identity of the account, whose authority is the key. A call
// with no such header is the product's, as `keys` reads it.
func Handler(keys auth.Handler) auth.Handler {
	return auth.HandlerFunc(func(ctx context.Context) (auth.Identity, error) {
		id, err := keys.Handle(ctx)
		if err != nil {
			return id, err
		}

		md, _ := metadata.FromIncomingContext(ctx)
		vs := md.Get(Header)
		switch len(vs) {
		case 0:
			return id, nil
		case 1:
		default:
			return auth.Identity{}, status.Errorf(codes.InvalidArgument, "%s names %d accounts, and a call is one's", Header, len(vs))
		}
		a, err := pdid.Parse(strings.TrimSpace(vs[0]))
		if err != nil || a.IsZero() {
			return auth.Identity{}, status.Errorf(codes.InvalidArgument, "%s: %q is not an account's identifier", Header, vs[0])
		}

		return auth.Identity{
			Authority: authority + id.Id,
			Changed:   id.Changed,
			Method:    id.Method + " for " + Header,
			Id:        a.String(),
			Grant:     id.Grant,
			Expires:   id.Expires,
		}, nil
	})
}

// errNotAnAccounts is a call relayed for an account to a method that is not
// one an account calls.
var errNotAnAccounts = status.Error(codes.PermissionDenied, "an account a product relays a call for calls what an account calls as itself, and nothing else")

// Relayed says whether `id` is an account a product's key vouches for.
func Relayed(id auth.Identity) bool { return strings.HasPrefix(id.Authority, authority) }

// Resolver is the frame a call is served in: a product's key's, by Keys; and
// an account a product relays a call for, held to what roster says of it
// now, framed as the account by Accounts, with the relaying key's row on the
// frame.
type Resolver struct {
	// Keys is the frame of a product's key (`keys.Resolver`).
	Keys auth.Resolver

	// Accounts is payday's account resolver, whose Names is [Relayed].
	Accounts account.Resolver

	// Standing is what roster says of an account: nil is no roster
	// configured, and every account's call is refused.
	Standing payers.Standing

	// Now is the deployment's clock.
	Now func() time.Time
}

var _ auth.Resolver = Resolver{}

func (r Resolver) Resolve(ctx context.Context, id auth.Identity) (*frame.Frame, error) {
	if !Relayed(id) {
		return r.Keys.Resolve(ctx, id)
	}

	// The key that vouches, resolved as the product it is: one revoked or
	// run out vouches for nobody.
	key, err := r.Keys.Resolve(ctx, auth.Identity{Id: strings.TrimPrefix(id.Authority, authority), Grant: id.Grant})
	if err != nil {
		return nil, err
	}
	row, ok := key.Row.(*levyan.ServiceKey)
	if !ok {
		return nil, status.Error(codes.Internal, "could not say who is calling")
	}

	// An account here has no membership of anything: what it may call is
	// what an account calls as itself, and the rest is refused as such
	// rather than looked for a membership of.
	if m, ok := grpc.Method(ctx); !ok || r.Accounts.Methods == nil || !r.Accounts.Methods(m) {
		return nil, errNotAnAccounts
	}
	if r.Standing == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"levyan asks roster whether an account is in good standing before it serves a call made as one, and no roster is configured (auth.roster)")
	}
	a, err := pdid.Parse(id.Id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s: %q is not an account's identifier", Header, id.Id)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if err := r.Standing.Held(ctx, a, now); err != nil {
		switch {
		case errors.Is(err, standing.ErrRefused):
			return nil, status.Error(codes.PermissionDenied, "roster says this account is not in good standing: suspended, erased or in a directory closed")
		case ctx.Err() != nil:
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		log.From(ctx).WarnContext(ctx, "relay: roster could not be asked about an account", slog.Any("err", err))
		return nil, status.Error(codes.Unavailable, "roster could not be asked whether this account is in good standing; try again")
	}

	f, err := r.Accounts.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	if !f.AsAccount() {
		return nil, errNotAnAccounts
	}

	log.From(ctx).InfoContext(ctx, "relayed",
		slog.String("key", key.Actor.String()),
		slog.String("key.name", key.Name),
		slog.String("product", key.Tenant.String()),
		slog.String("account", a.String()))

	return f.WithRow(row), nil
}

// Product is the product that relayed the call `ctx` belongs to, framed as
// an account by [Resolver], and false for any other.
func Product(ctx context.Context) (pdid.Id, bool) {
	f, ok := frame.From(ctx)
	if !ok || !f.AsAccount() {
		return pdid.Nil, false
	}
	row, ok := f.Row.(*levyan.ServiceKey)
	if !ok {
		return pdid.Nil, false
	}
	p, err := pdid.From(row.GetTenant().GetId())
	if err != nil {
		return pdid.Nil, false
	}

	return p, true
}
