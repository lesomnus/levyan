// Package policy says who may call what on the public binary (`levyan
// serve`). It is a table, and what is not in it is refused: a method added to
// the schema is closed until somebody decides who may call it, which is the
// direction a mistake should fall in.
//
// Three kinds of caller are meant to reach levyan, and the table is by kind:
//
//   - A **product** calls with a service key (`server/keys`), framed in its
//     own tenant. It sends what its tenants hold and do, and reads its own
//     price book and what it grants its tenants -- of itself and of no other
//     product, which the wall keeps whatever a call names ([Policy.Where]).
//   - An **operator** does not call here at all. Products, keys, meters and
//     price versions are written through `levyan-admin serve`, a binary of
//     its own on an address of its own, which this one does not link.
//   - A **payer** -- an account paying for a workspace, at roster -- will call
//     as the account, in no tenant, once payday says how an account calls
//     (payday#184). Until then nothing here is theirs, and a frame of any
//     other kind is refused everything.
//
// The generated methods of the entities are in nobody's column. A product
// reads its price book through `levyan.v1` (`server/api`), which answers in
// messages of its own; what the schema generates is how levyan works on its
// own rows, and is not an API anybody is promised.
package policy

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/gate"

	levyan "github.com/lesomnus/levyan"
)

// Caller is a kind of caller.
type Caller int

const (
	// Nobody is a method no caller is given.
	Nobody Caller = iota

	// Product is a product calling with its service key.
	Product
)

// Table is every method somebody may call, by the name gRPC knows it by, and
// who.
var Table = map[string]Caller{}

func allow(c Caller, service string, methods ...string) {
	for _, m := range methods {
		Table["/"+service+"/"+m] = c
	}
}

func init() {
	// A product's own price book: who it is, its meters, and every version of
	// what each costs.
	allow(Product, "levyan.v1.PriceBookService", "GetProduct", "ListMeters", "ListPrices")
}

// Policy is the table, asked.
type Policy struct{}

var _ gate.Policy = Policy{}

// Of is what kind of caller a call is from, by the row its frame was resolved
// from.
func Of(c gate.Call) Caller {
	if _, ok := c.Row.(*levyan.ServiceKey); ok {
		return Product
	}

	return Nobody
}

func (Policy) May(ctx context.Context, c gate.Call) error {
	who, ok := Table[c.Action]
	if !ok {
		if strings.HasPrefix(c.Action, "/grpc.") {
			// Health and reflection, which a frame never reaches anyway.
			return nil
		}
		return status.Errorf(codes.PermissionDenied, "%s is not something a caller may call here", c.Action)
	}
	if who == Nobody || Of(c) != who {
		return status.Errorf(codes.PermissionDenied, "%s is not this caller's to call", c.Action)
	}

	return nil
}

// Where is a product's own tenant, and nothing for anybody else: levyan has no
// caller who sees across products from this binary.
func (Policy) Where(ctx context.Context, c gate.Call) (frame.Tenants, error) {
	if Of(c) == Product {
		return frame.Only(c.Tenant), nil
	}

	return frame.Tenants{}, nil
}
