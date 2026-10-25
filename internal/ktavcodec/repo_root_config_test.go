package ktavcodec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The repository root ships rush.ktav (Ktav), not the legacy rush.json.
// This guard decodes the committed file and pins the gopls options the
// toolchain depends on, and fails if the legacy JSON file reappears.
func TestRepoRootRushKtav(t *testing.T) {
	t.Parallel()

	ktavPath := filepath.Join("..", "..", "rush.ktav")
	raw, err := os.ReadFile(ktavPath)
	require.NoError(t, err)

	tree, err := DecodeKtav(raw)
	require.NoError(t, err)

	jsonBytes, err := ToJSON(tree)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &doc))

	schema, ok := doc["$schema"].(string)
	require.True(t, ok, "$schema must be a string")
	require.Equal(t, "https://charm.land/crush.json", schema)

	lsp, ok := doc["lsp"].(map[string]any)
	require.True(t, ok, "lsp must be an object")
	gopls, ok := lsp["gopls"].(map[string]any)
	require.True(t, ok, "lsp.gopls must be an object")
	options, ok := gopls["options"].(map[string]any)
	require.True(t, ok, "lsp.gopls.options must be an object")

	require.Equal(t, true, options["gofumpt"])
	require.Equal(t, true, options["staticcheck"])
	require.Equal(t, true, options["semanticTokens"])

	directoryFilters, ok := options["directoryFilters"].([]any)
	require.True(t, ok, "directoryFilters must be an array")
	require.Equal(t, []any{"-.git", "-node_modules"}, directoryFilters)

	hints, ok := options["hints"].(map[string]any)
	require.True(t, ok, "hints must be an object")
	require.Equal(t, true, hints["assignVariableTypes"])

	analyses, ok := options["analyses"].(map[string]any)
	require.True(t, ok, "analyses must be an object")
	require.Equal(t, true, analyses["nilness"])

	_, err = os.Stat(filepath.Join("..", "..", "rush.json"))
	require.True(t, errors.Is(err, os.ErrNotExist),
		"legacy rush.json must not exist in the repo root")
}
