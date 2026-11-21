package provpeak

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestProviderSJSONPathEscapingPreservesDottedIDAndSpecialComponents(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, tc := range []struct {
		id, key string
	}{
		{"team.prod", "field.name"},
		{"team*prod", "field*name"},
		{"team?prod", "field?name"},
		{"team#prod", "field#name"},
		{"team|prod", "field|name"},
		{"team@prod", "field@name"},
		{`team\prod`, `field\name`},
	} {
		t.Run(tc.id+"/"+tc.key, func(t *testing.T) {
			path := "providers." + EscapeSJSONPathComponent(tc.id) + "." + EscapeSJSONPathComponent(tc.key)
			doc, err := sjson.Set(`{}`, path, "value")
			require.NoError(t, err)
			var got map[string]map[string]map[string]string
			require.NoError(t, json.Unmarshal([]byte(doc), &got))
			require.Equal(t, "value", got["providers"][tc.id][tc.key])
		})
	}
}
