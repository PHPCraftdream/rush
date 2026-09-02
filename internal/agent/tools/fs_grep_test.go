package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsGrepTestRun marshals the items, runs the fs_grep tool end-to-end
// against the given scope and returns the response for assertions.
func fsGrepTestRun(t *testing.T, ctx context.Context, workingDir string, scope permission.FolderScope, items []FSGrepItem) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(FSGrepParams{Items: items})
	require.NoError(t, err)
	tool := NewFSGrepTool(workingDir, scope, nil)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSGrepToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

// fsGrepCarvedScope builds a scope where the working directory root is
// grep-allowed but a subdirectory is carved out as a bare deny entry.
func fsGrepCarvedScope(t *testing.T, workingDir, carvedRel string) permission.FolderScope {
	t.Helper()
	root, err := resolveScopedPath(context.Background(), OSDisk(), workingDir, ".")
	require.NoError(t, err)
	carved, err := resolveScopedPath(context.Background(), OSDisk(), workingDir, carvedRel)
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: carved},
			{Dir: root, Ops: []permission.FileOp{permission.FileOpGrep}},
		},
	})
	require.NoError(t, err)
	return scope
}

func TestFSGrepFallbackContextMergesOverlappingWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Twenty newline-terminated lines with hits at 5, 8 and 20, so the
	// first two windows overlap and must merge while the last stands
	// alone with its end clamped to the file's final line.
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[4] = "line 05 needle"
	lines[7] = "line 08 needle"
	lines[19] = "line 20 needle"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	resp := fsGrepTestRun(t, t.Context(), dir, fsBatchTestScope(t, dir, permission.FileOpGrep),
		[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: 2}})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "fs_grep: 1 of 1 items ok")

	// Windows [3,7] and [6,10] overlap: exactly one merged block, never
	// the unmerged [3,7] form.
	require.Equal(t, 1, strings.Count(resp.Content, `lines="3-10" hit="5"`))
	require.NotContains(t, resp.Content, `lines="3-7"`)

	// Hit 20's window is clamped to the file's last line.
	require.Contains(t, resp.Content, `lines="18-20" hit="20"`)

	// Marked hits and two-space context rows. The renderer pads line
	// numbers with spaces to the window's width (2), so hit 5 renders
	// as ">  5" and context 3 as "   3".
	require.Contains(t, resp.Content, ">  5 | line 05 needle")
	require.Contains(t, resp.Content, ">  8 | line 08 needle")
	require.Contains(t, resp.Content, "   3 | line 03")
	require.Contains(t, resp.Content, "  10 | line 10")

	// Line 01 is outside every window and must not leak.
	require.NotContains(t, resp.Content, "line 01")

	// Two blocks total: the merged window and the tail window.
	require.Equal(t, 2, strings.Count(resp.Content, "<match "))
}

func TestFSGrepMergesAdjacentWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Twelve lines with hits at 3 and 8: the windows [1,5] and [6,10]
	// merely touch, which must still coalesce into a single block.
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[2] = "line 03 needle"
	lines[7] = "line 08 needle"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	resp := fsGrepTestRun(t, t.Context(), dir, fsBatchTestScope(t, dir, permission.FileOpGrep),
		[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: 2}})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `lines="1-10" hit="3"`)
	require.Equal(t, 1, strings.Count(resp.Content, "<match "))
}

func TestFSGrepDropsOutOfScopeMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "secret"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle visible\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret", "b.txt"), []byte("needle hidden\n"), 0o644))

	// The root is in scope for grep but the secret subtree is carved
	// out, so only the a.txt match may survive rendering.
	scope := fsGrepCarvedScope(t, dir, "secret")
	resp := fsGrepTestRun(t, t.Context(), dir, scope,
		[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: 1}})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "fs_grep: 1 of 1 items ok")
	require.Contains(t, resp.Content, "a.txt")
	require.Contains(t, resp.Content, "needle visible")
	require.NotContains(t, strings.ToLower(resp.Content), "secret")
	require.NotContains(t, resp.Content, "needle hidden")
}

func TestFSGrepPreflightRejectsContextLinesOutOfRange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("needle\n"), 0o644))
	scope := fsBatchTestScope(t, dir, permission.FileOpGrep)

	for _, contextLines := range []int{FSBatchMaxContextLines + 1, -1} {
		resp := fsGrepTestRun(t, t.Context(), dir, scope,
			[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: contextLines}})
		// A failed-only batch is surfaced as an error response by the
		// runner (succeeded == 0), so IsError is not asserted here.
		var meta FSBatchResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
		require.Len(t, meta.Items, 1)
		require.Equal(t, FSStatusFailed, meta.Items[0].Status)
		require.Contains(t, meta.Items[0].Error, "context_lines")
		require.Contains(t, meta.Items[0].Error, "out of range")
		require.NotContains(t, resp.Content, "<match ")
	}

	// The upper boundary itself is accepted and runs the search.
	resp := fsGrepTestRun(t, t.Context(), dir, scope,
		[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: FSBatchMaxContextLines}})
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Contains(t, resp.Content, "<match ")
}

func TestFSGrepFallbackLineBudgetCapsOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// 300 identical lines: every line is a hit, all windows are
	// adjacent, so rendering produces one block capped at the budget.
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = "needle"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	resp := fsGrepTestRun(t, t.Context(), dir, fsBatchTestScope(t, dir, permission.FileOpGrep),
		[]FSGrepItem{{Pattern: "needle", Path: dir, ContextLines: 0}})
	require.False(t, resp.IsError)
	require.Equal(t, 100, strings.Count(resp.Content, "| needle"))
	require.Contains(t, resp.Content, "truncated at 100 rendered lines")
	require.Equal(t, 1, strings.Count(resp.Content, "<match "))
}

func TestParseRipgrepContextStream(t *testing.T) {
	t.Parallel()

	// A canned rg --json stream: a begin event, context-before/match/
	// context-after around a hit in f.txt, an end and a summary, a hit
	// in g.txt, garbage, a zero-line match to skip, and finally a
	// context-then-match upgrade for f.txt line 6 that must not spend
	// the budget twice.
	stream := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"f.txt"}}}`,
		`{"type":"context","data":{"path":{"text":"f.txt"},"line_number":4,"lines":{"text":"before\n"}}}`,
		`{"type":"match","data":{"path":{"text":"f.txt"},"line_number":5,"lines":{"text":"hit\n"},"submatches":[{"match":{"text":"hit"}}]}}`,
		`{"type":"context","data":{"path":{"text":"f.txt"},"line_number":6,"lines":{"text":"after\n"}}}`,
		`{"type":"end","data":{"path":{"text":"f.txt"}}}`,
		`{"type":"summary","data":{"elapsed_total":{"secs":0}}}`,
		`{"type":"match","data":{"path":{"text":"g.txt"},"line_number":2,"lines":{"text":"hit\n"}}}`,
		`not json at all`,
		`{"type":"match","data":{"path":{"text":"z.txt"},"line_number":0,"lines":{"text":"nope\n"}}}`,
		`{"type":"context","data":{"path":{"text":"f.txt"},"line_number":6,"lines":{"text":"after\n"}}}`,
		`{"type":"match","data":{"path":{"text":"f.txt"},"line_number":6,"lines":{"text":"after\n"}}}`,
	}, "\n")

	files := map[string]*fsGrepFileHits{}
	budget := newFSGrepBudget()
	require.NoError(t, parseRipgrepContextStream(strings.NewReader(stream), files, &budget))

	require.ElementsMatch(t, []int{5, 6}, files["f.txt"].hits)
	require.Equal(t, fsGrepLine{text: "before", hit: false}, files["f.txt"].lines[4])
	require.True(t, files["f.txt"].lines[5].hit)
	require.True(t, files["f.txt"].lines[6].hit)
	require.ElementsMatch(t, []int{2}, files["g.txt"].hits)
	require.NotContains(t, files, "z.txt")

	// Four accepted takes: f.txt lines 4 (context) and 5 (match) and
	// the first context arrival of line 6, plus g.txt line 2. The
	// context-then-match upgrade of f.txt line 6 spends nothing.
	require.Equal(t, 96, budget.remaining)
}

func TestRipgrepContextSearchWithRealRg(t *testing.T) {
	t.Parallel()
	rgPath, lookErr := exec.LookPath("rg")
	if lookErr != nil {
		t.Skip("rg is not in $PATH")
	}
	dir := t.TempDir()

	// Twelve lines with hits at 5 and 8 so context windows [3,7] and
	// [6,10] overlap: line 07 must be kept as pure context and line 10
	// proves the -C 2 after-context was collected for hit 8.
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[4] = "line 05 needle"
	lines[7] = "line 08 needle"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	cmd := buildRgSearchCmd(t.Context(), rgPath, "needle", dir, "", 2)
	files := map[string]*fsGrepFileHits{}
	budget := newFSGrepBudget()
	require.NoError(t, runRipgrepContextSearch(t.Context(), cmd, files, &budget))

	require.Len(t, files, 1)
	var collector *fsGrepFileHits
	for _, c := range files {
		collector = c
	}
	require.ElementsMatch(t, []int{5, 8}, collector.hits)
	require.False(t, collector.lines[4].hit)
	require.Equal(t, "line 04", collector.lines[4].text)
	require.True(t, collector.lines[5].hit)
	require.False(t, collector.lines[7].hit)
	require.Equal(t, "line 07", collector.lines[7].text)
	require.False(t, collector.lines[10].hit)
	require.Equal(t, "line 10", collector.lines[10].text)
}
