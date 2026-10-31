package api_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/server/api"
	"github.com/lesomnus/levyan/server/pd"
)

// The API's enums are the schema's, value for value: `server/api` hands one
// over by its number, so a value of the schema the API does not have, or has
// under another name, would be a meter told to a product as something else.
// The API may gain a value only as the schema does.
func TestTheAPIsEnumsAreTheSchemas(t *testing.T) {
	x := require.New(t)

	for _, pair := range [][2]protoreflect.EnumDescriptor{
		{levyan.MeterKind(0).Descriptor(), levyanv1.MeterKind(0).Descriptor()},
		{levyan.Aggregation(0).Descriptor(), levyanv1.Aggregation(0).Descriptor()},
		{levyan.Channel(0).Descriptor(), levyanv1.Channel(0).Descriptor()},
		{levyan.LineKind(0).Descriptor(), levyanv1.LineKind(0).Descriptor()},
		{levyan.LinkBegan(0).Descriptor(), levyanv1.LinkBegan(0).Descriptor()},
		{levyan.LinkEnded(0).Descriptor(), levyanv1.LinkEnded(0).Descriptor()},
		{levyan.InvoiceState(0).Descriptor(), levyanv1.InvoiceState(0).Descriptor()},
		{levyan.CreditNoteReason(0).Descriptor(), levyanv1.CreditNoteReason(0).Descriptor()},
		{levyan.CheckoutState(0).Descriptor(), levyanv1.CheckoutState(0).Descriptor()},
		{levyan.CollectionState(0).Descriptor(), levyanv1.CollectionState(0).Descriptor()},
		{levyan.SubscriptionStatus(0).Descriptor(), levyanv1.SubscriptionStatus(0).Descriptor()},
		{levyan.SubscriptionChange(0).Descriptor(), levyanv1.SubscriptionChange(0).Descriptor()},
	} {
		schema, api := pair[0], pair[1]
		x.Equal(schema.Values().Len(), api.Values().Len(), "%s", schema.FullName())
		for i := range schema.Values().Len() {
			v := schema.Values().Get(i)
			w := api.Values().ByNumber(v.Number())
			x.NotNil(w, "%s has no %d", api.FullName(), v.Number())
			x.Equal(string(v.Name()), string(w.Name()), "%s %d", schema.FullName(), v.Number())
		}
		x.True(strings.HasPrefix(string(api.FullName()), "levyan.v1."))
	}
}

// Every method of the API's PayerService is served as the schema's method
// of its name, which payday marks an account's own: the two declare the same
// methods, so that none is served as an account's without payday holding it
// to what it declared it reads, and none the schema declares goes unserved.
func TestThePayersMethodsAreTheSchemas(t *testing.T) {
	x := require.New(t)

	x.Len(api.PayerMethods, len(levyan.PayerService_ServiceDesc.Methods))
	for v1, schema := range api.PayerMethods {
		x.True(pd.AccountMethod(schema), "%s is served as %s, which is no account's method", v1, schema)
		x.True(api.Calls(v1))
		x.True(api.Calls(schema))
	}
	for _, m := range pd.AccountMethods {
		found := false
		for _, schema := range api.PayerMethods {
			found = found || schema == m
		}
		x.True(found, "%s is an account's, and nothing of the API's is served as it", m)
	}
	x.False(api.Calls(levyanv1.WorkspaceService_GetPayer_FullMethodName))
}
