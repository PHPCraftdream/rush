package csync

import (
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestMapSchemaDescribesMapValues(t *testing.T) {
	t.Parallel()

	schema := jsonschema.Reflect(&Map[string, int]{})
	definition := schema
	if schema.Ref != "" {
		definition = schema.Definitions[strings.TrimPrefix(schema.Ref, "#/$defs/")]
	}
	require.NotNil(t, definition)
	require.Equal(t, "object", definition.Type)
	require.NotNil(t, definition.AdditionalProperties)
	require.Equal(t, "integer", definition.AdditionalProperties.Type)
}
