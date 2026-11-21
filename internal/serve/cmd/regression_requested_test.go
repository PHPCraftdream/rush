package cmd

import (
	"os"
	"strings"
	"testing"

	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func TestExtractFilePathsPreservesEscapedQuotesAndWindowsSeparators(t *testing.T) {
	input := `{"file_path":"C:\\work\\a\"b.go"}`
	require.Equal(t, []string{`C:\work\a"b.go`}, extractFilePaths("edit", input))
}

func TestPrintTreeRootChildrenConnectors(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tree")
	require.NoError(t, err)
	root := session.Session{ID: "root", Title: "root"}
	children := map[string][]session.Session{
		"root":   {{ID: "child1", Title: "child1"}, {ID: "child2", Title: "child2"}},
		"child1": {{ID: "grandchild", Title: "grandchild"}},
	}
	printTreeNode(f, root, children, "", true, 0, 0)
	require.NoError(t, f.Close())
	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 4)
	require.Contains(t, lines[1], "├── child1")
	require.Contains(t, lines[2], "│   └── grandchild")
	require.Contains(t, lines[3], "└── child2")
}
