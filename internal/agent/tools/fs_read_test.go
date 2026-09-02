package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsReadTestFixture writes one 10-line file "line 1".."line 10" in dir
// and returns its path.
func fsReadTestFixture(t *testing.T, dir string) string {
	t.Helper()
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i+1)
	}
	path := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644))
	return path
}

func fsReadRun(t *testing.T, scope permission.FolderScope, workingDir string, items []FSReadItem) fantasy.ToolResponse {
	t.Helper()
	tool := NewFSReadTool(scope, mockFileTrackerService{}, workingDir, nil)
	raw, err := json.Marshal(FSReadParams{Items: items})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-fs-read", Input: string(raw)})
	require.NoError(t, err)
	return resp
}

// blockOf returns the <file> block starting at header, through its
// closing tag, so assertions stay per-item when one call reads the same
// file several times.
func blockOf(t *testing.T, content, header string) string {
	t.Helper()
	start := strings.Index(content, header)
	require.GreaterOrEqual(t, start, 0, "block header not found")
	end := strings.Index(content[start:], "</file>")
	require.GreaterOrEqual(t, end, 0)
	return content[start : start+end]
}

func TestFSReadAddressingModes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := fsReadTestFixture(t, dir)
	scope := fsBatchTestScope(t, dir, permission.FileOpRead)

	resp := fsReadRun(t, scope, dir, []FSReadItem{
		{Path: path},
		{Path: path, StartLine: 3, EndLine: 5},
		{Path: path, Line: 7, Radius: 2},
	})
	require.False(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 3)
	for _, item := range meta.Items {
		require.Equal(t, FSStatusOK, item.Status)
	}

	// %q escapes the path, so on Windows the attribute carries doubled
	// backslashes; build the expected string with %q to match exactly.
	wantFull := fmt.Sprintf("<file path=%q lines=\"1-10\" status=\"ok\">", path)
	require.Contains(t, resp.Content, wantFull)
	require.Contains(t, resp.Content, "     1|line 1")
	require.Contains(t, resp.Content, "    10|line 10")

	wantRange := fmt.Sprintf("<file path=%q lines=\"3-5\" status=\"ok\">", path)
	require.Contains(t, resp.Content, wantRange)
	rangeBlock := blockOf(t, resp.Content, wantRange)
	require.Contains(t, rangeBlock, "     3|line 3")
	require.Contains(t, rangeBlock, "     5|line 5")
	require.NotContains(t, rangeBlock, "     1|line 1")
	require.NotContains(t, rangeBlock, "     6|line 6")

	wantRadius := fmt.Sprintf("<file path=%q lines=\"5-9\" status=\"ok\">", path)
	require.Contains(t, resp.Content, wantRadius)
	radiusBlock := blockOf(t, resp.Content, wantRadius)
	require.Contains(t, radiusBlock, "     5|line 5")
	require.Contains(t, radiusBlock, "     9|line 9")
	require.NotContains(t, radiusBlock, "    10|line 10")

	// Only the full-file read fits within its limit, so only it has no
	// has-more hint.
	require.NotContains(t, blockOf(t, resp.Content, wantFull), "(File has more lines")
}

func TestFSReadMixedScopeBatch(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	pathA := filepath.Join(dirA, "a.txt")
	pathB := filepath.Join(dirB, "b.txt")
	require.NoError(t, os.WriteFile(pathA, []byte(strings.Join([]string{"alpha", "beta"}, "\n")), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("secret beta"), 0o644))

	resp := fsReadRun(t, fsBatchTestScope(t, dirA, permission.FileOpRead), dirA, []FSReadItem{
		{Path: "a.txt"},
		{Path: pathB},
	})
	require.False(t, resp.IsError, "one ok item keeps the call successful")

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 2)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusDenied, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "outside every folder scope")

	require.Contains(t, resp.Content, `<file path="a.txt" lines="1-2" status="ok">`)
	require.Contains(t, resp.Content, "     1|alpha")
	require.NotContains(t, resp.Content, "secret beta")
}

func TestFSReadRejectsBothAddressingModes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(pathA, []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("y"), 0o644))

	resp := fsReadRun(t, fsBatchTestScope(t, dir, permission.FileOpRead), dir, []FSReadItem{
		{Path: pathA, StartLine: 1, EndLine: 2, Line: 3, Radius: 1},
		{Path: pathB, StartLine: 1, EndLine: 1},
	})

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 2)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "not both")
	require.Equal(t, FSStatusOK, meta.Items[1].Status, "the valid item is unaffected")
	require.NotContains(t, resp.Content, fmt.Sprintf("<file path=%q", pathA))

	// End without start is its own structural failure.
	resp = fsReadRun(t, fsBatchTestScope(t, dir, permission.FileOpRead), dir, []FSReadItem{
		{Path: pathA, StartLine: 0, EndLine: 5},
	})
	meta = fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "start_line must be")
}

func TestFSReadFullFileTooLargeFailsPerItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = strings.Repeat("x", 1000)
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644))

	resp := fsReadRun(t, fsBatchTestScope(t, dir, permission.FileOpRead), dir, []FSReadItem{
		{Path: path},
	})
	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "too large")
	require.Contains(t, meta.Items[0].Error, "start_line/end_line")
	require.NotContains(t, resp.Content, "<file path=")
}
