package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestWriteWouldShrinkFileBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		oldN, newN   int
		wantRefusing bool
	}{
		{"small old file is never guarded", writeShrinkMinOldBytes - 1, 0, false},
		{"at the minimum, emptied", writeShrinkMinOldBytes, 0, true},
		{"just under a quarter", 2048, 511, true},
		{"exactly a quarter passes", 2048, 512, false},
		{"large file cut to one row", 100_000, 400, true},
		{"large file halved passes", 100_000, 50_000, false},
		{"growth passes", 3000, 9000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.wantRefusing, writeWouldShrinkFile(tc.oldN, tc.newN))
		})
	}
}

func runWrite(t *testing.T, workingDir string, params WriteParams) fantasy.ToolResponse {
	t.Helper()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	tool := NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir)
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: WriteToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

// A write that would delete most of a large existing file is refused with the
// way out spelled out; allow_shrink=true makes it deliberate; small files and
// growing writes are untouched.
//
// Revert-check: drop the writeWouldShrinkFile branch from the write tool and
// the refusal subtests go red (the file is overwritten).
func TestWriteToolRefusesToShrinkALargeFile(t *testing.T) {
	// Sequential on purpose: the subtests share one working directory and
	// depend on each other's outcome.
	workingDir := t.TempDir()
	big := strings.Repeat("| ASYNC-xx | a registry row that is long enough to matter |\n", 120) // ~6 KB
	path := filepath.Join(workingDir, "registry.md")
	require.NoError(t, os.WriteFile(path, []byte(big), 0o644))

	t.Run("one row over a large file is refused and nothing is written", func(t *testing.T) {
		resp := runWrite(t, workingDir, WriteParams{FilePath: "registry.md", Content: "| ASYNC-13 | one row |\n"})
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "allow_shrink=true")
		require.Contains(t, resp.Content, "edit or multiedit")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, big, string(got), "the refused write must leave the file untouched")
	})

	t.Run("emptying a large file is refused", func(t *testing.T) {
		resp := runWrite(t, workingDir, WriteParams{FilePath: "registry.md", Content: ""})
		require.True(t, resp.IsError)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, big, string(got))
	})

	t.Run("allow_shrink makes the replacement deliberate", func(t *testing.T) {
		resp := runWrite(t, workingDir, WriteParams{FilePath: "registry.md", Content: "short\n", AllowShrink: true})
		require.False(t, resp.IsError, resp.Content)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "short\n", string(got))
	})

	t.Run("a small existing file may be replaced by anything", func(t *testing.T) {
		small := filepath.Join(workingDir, "small.txt")
		require.NoError(t, os.WriteFile(small, []byte(strings.Repeat("x", 500)), 0o644))
		resp := runWrite(t, workingDir, WriteParams{FilePath: "small.txt", Content: "y"})
		require.False(t, resp.IsError, resp.Content)
	})

	t.Run("a growing rewrite of a large file passes", func(t *testing.T) {
		grow := filepath.Join(workingDir, "grow.txt")
		require.NoError(t, os.WriteFile(grow, []byte(strings.Repeat("x", 3000)), 0o644))
		resp := runWrite(t, workingDir, WriteParams{FilePath: "grow.txt", Content: strings.Repeat("z", 6000)})
		require.False(t, resp.IsError, resp.Content)
	})

	t.Run("a new file is never guarded", func(t *testing.T) {
		resp := runWrite(t, workingDir, WriteParams{FilePath: "fresh.txt", Content: "hi"})
		require.False(t, resp.IsError, resp.Content)
	})
}
