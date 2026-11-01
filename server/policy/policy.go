// Package policy says who may call what on the public binary (`levyan
// serve`). It is a table, and what is not in it is refused: a method added to
// the schema is closed until somebody decides who may call it, which is the
// direction a mistake should fall in.
//
// Three kinds of caller are meant to reach levyan, and the table is by kind:
//
//   - A **product** calls with a service key (`server/keys`), framed in its
//     own tenant. It sends what its tenants hold and do -- readings, and its
//     flows through OpenTelemetry's receiver -- and reads its own price book
//     and what it grants its tenants: of itself and of no other product,
//     which the wall keeps whatever a call names ([Policy.Where]).
//   - An **operator** does not call here at all. Products, keys, meters and
//     price versions are written through `levyan-admin serve`, a binary of
//     its own on an address of its own, which this one does not link.
//   - A **payer** -- an account of roster's paying for a workspace -- calls as
//     the account, in no tenant (payday's account frame), relayed by a
//     product with its key (`server/relay`): `levyan.v1.PayerService` and
//     nothing else, each served as the schema's method of its name, which is
//     what payday holds such a call to.
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

	// Payer is an account calling as itself, relayed by a product with its
	// key (`server/relay`).
	Payer
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

	// What its workspaces hold, as readings, into its own ledger.
	allow(Product, "levyan.v1.MeteringService", "Record")

	// What its workspaces come to: the period open, and the statements of
	// the periods closed.
	allow(Product, "levyan.v1.BillingService", "GetUsage", "GetStatement", "ListStatements")

	// And what they do, as OpenTelemetry Sums, into the same ledger
	// (`server/otlp`).
	allow(Product, "opentelemetry.proto.collector.metrics.v1.MetricsService", "Export")

	// What its workspaces are granted, which it keeps a copy of: payday's
	// contract, its stream and its list (`server/api`, #7).
	allow(Product, "payday.GrantService", "Watch", "List")

	// Who pays for its workspaces: one made, an account gone from one, an
	// account asked to pay, and who pays (#4).
	allow(Product, "levyan.v1.WorkspaceService", "Arrive", "Leave", "Offer", "GetPayer")

	// And what became of them, which is when they are billed: one closed,
	// restored, destroyed, and what levyan has heard of that (#14).
	allow(Product, "levyan.v1.WorkspaceService", "Close", "Restore", "Destroy", "GetLifecycle")

	// And an account's own, relayed by a product for one of its users: its
	// billing accounts, the offers made to it and the workspaces it pays for.
	allow(Payer, "levyan.v1.PayerService",
		"ListBillingAccounts", "CreateBillingAccount", "UpdateBillingAccount",
		"ListOffers", "Accept", "Decline", "ListLinks", "Use", "Stop")

	// And what its billing accounts are asked to pay, of the product that
	// relays the call: the invoices once finalized, and their credit notes
	// (#9).
	allow(Payer, "levyan.v1.PayerService", "ListInvoices", "GetInvoice", "ListCreditNotes")

	// And what they paid for, of the product that relays the call: the
	// statements of its workspaces, each of a period one of the account's
	// billing accounts paid for, line by line (#47).
	allow(Payer, "levyan.v1.PayerService", "ListStatements", "GetStatement")

	// And a billing account's subscription with the merchant of record: the
	// billing account with whether it can be collected from, and the
	// checkout that subscribes it (#12).
	allow(Payer, "levyan.v1.PayerService", "GetBillingAccount", "StartCheckout", "CompleteCheckout")
}

// Policy is the table, asked.
type Policy struct{}

var _ gate.Policy = Policy{}

// Of is what kind of caller a call is from, by the row its frame was resolved
// from: a product's key, or an account a product's key relays a call for.
func Of(c gate.Call) Caller {
	if _, ok := c.Row.(*levyan.ServiceKey); ok {
		if c.AsAccount {
			return Payer
		}
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
