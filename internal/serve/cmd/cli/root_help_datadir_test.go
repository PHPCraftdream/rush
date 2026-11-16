package cli

// Help text of the root command's --data-dir flag (SD-D #1143): the flag
// is the only place the data-directory selection is documented, so its
// usage string must carry the whole selection order an operator needs.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// oneLine collapses the help text's hard wraps so assertions do not depend on
// where a line happens to break.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestRootHelp_DataDirDocumentsSelectionOrder: the --data-dir usage must
// name every branch of the selection order -- the flag itself, the
// config's data_directory, the legacy worktree-local DB, the shared
// <main>/.rush for linked worktrees, the dev-build isolation, and the
// <cwd>/.rush fallback.
//
// Revert-check: remove any one clause from the flag usage string in
// root.go's init() and this test fails naming it.
func TestRootHelp_DataDirDocumentsSelectionOrder(t *testing.T) {
	root := NewRootCmd()
	t.Parallel()

	flag := root.PersistentFlags().Lookup("data-dir")
	require.NotNil(t, flag, "--data-dir must stay a root persistent flag")

	usage := flag.Usage
	require.Contains(t, usage, "Selection order", "the help must state there is an ordered selection")
	require.Contains(t, usage, "options.data_directory", "priority 2 must be named")
	require.Contains(t, usage, "legacy", "the worktree-local DB rule must be named")
	require.Contains(t, usage, "linked git worktree", "the linked-worktree redirection must be named")
	require.Contains(t, usage, "<main>/.rush", "the shared target must be named")
	require.Contains(t, usage, "dev", "the dev-build isolation must be named")
	require.Contains(t, usage, ".rush/dev", "the dev isolation target must be named")
	require.Contains(t, usage, "<cwd>/.rush", "the fallback must be named")

	// The root Long deliberately points at the FLAGS section instead of
	// duplicating the flag docs (see the --color-scheme note in root.go).
	require.Contains(t, oneLine(root.Long), "--data-dir")
}
