package api_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
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
