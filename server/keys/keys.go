// Package keys is how a product proves it is calling: a key an operator
// issued it, sent as `authorization: Bearer lvk_…`.
//
// A key is a row of its product's tenant (`ServiceKey`), found by the SHA-256
// of what was sent. The call is then served as the **key**, in the product's
// tenant: the trail names which key wrote what, revoking one is an erase, and
// the wall keeps the call to that product's rows whatever it asks for. What a
// product may call is `server/policy`'s, and a key widens none of it.
//
// What this is not is a scheme of its own. payday reads `authorization:
// Bearer` and exchanges the token for a name (`auth.Bearer`); [Store] is what
// it exchanges against, and [Resolver] is the frame the name is served in.
//
// # The key itself
//
// Thirty-two bytes from `crypto/rand` behind a prefix, so that a leaked one is
// recognisable to a secret scanner and to whoever finds it in a log it should
// not be in. It is said once, when it is issued ([Issue]); what is kept is its
// hash, and nothing here can say what a key was.
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/server/pd"
)

// Prefix is what every key begins with.
const Prefix = "lvk_"

// Mint makes a key: the one to hand over, and the verifier to keep.
func Mint() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("keys: %w", err)
	}

	token := Prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Sum(token), nil
}

// Sum is the verifier of a key, and the way its row is found.
//
// SHA-256, unsalted, which is right for a key and would be wrong for a
// password: there is no dictionary for 256 random bits, and a salt per row
// would make finding the row a scan of every row on every call.
func Sum(token string) []byte {
	v := sha256.Sum256([]byte(token))
	return v[:]
}

// Issue makes a key for a product and writes its row through `s`, answering
// with the key -- the one time it is said -- and the row.
func Issue(ctx context.Context, s levyan.Server, product *levyan.TenantRef, alias, desc string, expires *time.Time) (string, *levyan.ServiceKey, error) {
	token, sum, err := Mint()
	if err != nil {
		return "", nil, err
	}

	req := levyan.ServiceKeyAddRequest_builder{
		Tenant: product,
		Alias:  alias,
		Desc:   desc,
		Secret: sum,
	}.Build()
	if expires != nil {
		req.SetDateExpires(timestamppb.New(*expires))
	}

	v, err := s.ServiceKey().Add(ctx, req)
	if err != nil {
		return "", nil, err
	}

	return token, v, nil
}

// errNo is every refusal about a key, whatever was wrong with it: unknown,
// revoked and expired told apart are an oracle for "this string was a key
// once".
var errNo = errors.New("keys: no such key")

// Store is who a key stands for, read through `s` -- the server with no wall,
// since who is calling is asked before there is anybody to be walled by.
func Store(s levyan.Server, now func() time.Time) auth.TokenStore {
	if now == nil {
		now = time.Now
	}

	return auth.TokenStoreFunc(func(ctx context.Context, token string) (auth.Identity, error) {
		if !strings.HasPrefix(token, Prefix) {
			return auth.Identity{}, errNo
		}

		v, err := s.ServiceKey().Get(ctx, levyan.ServiceKeyGetRequest_builder{
			Ref: levyan.ServiceKeyRef_builder{Secret: Sum(token)}.Build(),
			Select: levyan.ServiceKeySelect_builder{
				Tenant:      levyan.TenantSelect_builder{}.Build(),
				DateExpires: proto.Bool(true),
			}.Build(),
		}.Build())
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return auth.Identity{}, errNo
			}

			// Not the caller's fault: told their key is bad, a product throws
			// away a perfectly good one.
			return auth.Identity{}, fmt.Errorf("%w: %w", auth.ErrUnavailable, err)
		}
		if expired(v, now()) {
			return auth.Identity{}, errNo
		}

		id, err := pdid.From(v.GetId())
		if err != nil {
			return auth.Identity{}, err
		}
		tenant, err := pdid.From(v.GetTenant().GetId())
		if err != nil {
			return auth.Identity{}, err
		}

		out := auth.Identity{
			Id:       id.String(),
			TenantId: tenant.String(),

			// What it may call is the policy's to say of a product, so the key
			// narrows nothing of it.
			Grant: frame.Whole(),
		}
		if u := v.GetDateExpires(); u != nil {
			// So that a stream is cut when the key runs out.
			out.Expires = u.AsTime()
		}

		return out, nil
	})
}

// Resolver is the frame a key's call is served in: the key, in its product's
// tenant, as the row says -- read again by the identifier the store answered,
// so that a frame is only ever made of a key that is there.
func Resolver(s levyan.Server, now func() time.Time) auth.Resolver {
	if now == nil {
		now = time.Now
	}

	return auth.ResolverFunc(func(ctx context.Context, id auth.Identity) (*frame.Frame, error) {
		k, err := pdid.Parse(id.Id)
		if err != nil || k.Domain() != pd.ServiceKeyDomain {
			return nil, fmt.Errorf("%w: names no key", auth.ErrNoCredential)
		}

		v, err := s.ServiceKey().Get(ctx, levyan.ServiceKeyGetRequest_builder{
			Ref: levyan.ServiceKeyRef_builder{Id: k.Bytes()}.Build(),
			Select: levyan.ServiceKeySelect_builder{
				All:    proto.Bool(true),
				Tenant: levyan.TenantSelect_builder{All: proto.Bool(true)}.Build(),
			}.Build(),
		}.Build())
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, fmt.Errorf("%w: no such key", auth.ErrNoCredential)
			}

			return nil, fmt.Errorf("%w: %w", auth.ErrUnavailable, err)
		}
		if expired(v, now()) {
			return nil, fmt.Errorf("%w: no such key", auth.ErrNoCredential)
		}

		tenant, err := pdid.From(v.GetTenant().GetId())
		if err != nil {
			return nil, err
		}

		// The grant is not set here: the interceptor takes it from the
		// credential, so a resolver cannot widen a key by being generous.
		f := frame.New(k, tenant, frame.Grant{}).WithRow(v)
		f.Name = v.GetAlias()
		f.TenantName = v.GetTenant().GetAlias()

		return f, nil
	})
}

func expired(v *levyan.ServiceKey, now time.Time) bool {
	u := v.GetDateExpires()
	return u != nil && !now.Before(u.AsTime())
}
