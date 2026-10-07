// REVERT-CHECK: dropping the `output += "\n\n" + note` branch in the glob
// tool's Run must fail TestGlobToolReportsIncompleteSearch.
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestGlobToolReportsIncompleteSearch(t *testing.T) {
	setBudgets(t, 5, 0)
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o600))
	}
	resp, err := NewGlobTool(dir).Run(t.Context(), fantasy.ToolCall{ID: "g1", Name: GlobToolName, Input: `{"pattern":"*.txt"}`})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "results are incomplete", "the model must be told the search was cut short")
}
