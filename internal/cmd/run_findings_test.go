// Fork-only tests for the --findings-from prompt injection
// (run_findings.go). Revert-check comments state which product change
// each test catches.

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Revert-check: the block the model receives must carry all three
// finding kinds — verdict, review text and warnings — in a stable
// order under the fixed title.
func TestFormatFindingsBlockFull(t *testing.T) {
	t.Parallel()
	env := &app.RunResult{
		ReviewVerdict: "pass_with_notes",
		Review:        "1. Fix X.\n2. Fix Y.",
		Warnings:      []string{"warn one", "warn two"},
	}
	block := formatFindingsBlock(env)
	assert.True(t, strings.HasPrefix(block, findingsBlockTitle))
	vIdx := strings.Index(block, "verdict: pass_with_notes")
	rIdx := strings.Index(block, "1. Fix X.")
	yIdx := strings.Index(block, "2. Fix Y.")
	w1 := strings.Index(block, "- warn one")
	w2 := strings.Index(block, "- warn two")
	require.NotEqual(t, -1, vIdx)
	require.NotEqual(t, -1, rIdx)
	require.NotEqual(t, -1, yIdx)
	require.NotEqual(t, -1, w1)
	require.NotEqual(t, -1, w2)
	assert.Less(t, vIdx, rIdx)
	assert.Less(t, rIdx, yIdx)
	assert.Less(t, yIdx, w1)
	assert.Less(t, w1, w2)
}

// Revert-check: an envelope with no findings must still yield a block,
// with the explicit none-recorded body, so the model knows the
// reference run was checked and produced nothing.
func TestFormatFindingsBlockEmpty(t *testing.T) {
	t.Parallel()
	block := formatFindingsBlock(&app.RunResult{})
	assert.Equal(t, findingsBlockTitle+"\n\n(none recorded in the envelope).\n", block)
}

// Revert-check: THE contract that rush run is unchanged without the
// flag — byte equality of the prompt.
func TestApplyFindingsFromNoFlag(t *testing.T) {
	t.Parallel()
	got, err := applyFindingsFrom("my prompt", "")
	require.NoError(t, err)
	assert.Equal(t, "my prompt", got)
}

// Revert-check: append-after-prompt placement (the user's prompt stays
// at the top) and Cyrillic survival in the appended block.
func TestApplyFindingsFromAppends(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "env.out")
	require.NoError(t, os.WriteFile(path, []byte(cyrEnv), 0o644))
	got, err := applyFindingsFrom("my prompt", path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "my prompt\n\n"+findingsBlockTitle))
	assert.Contains(t, got, "Замечаний нет, изменения точны.")
	assert.True(t, utf8.ValidString(got))
}

// Revert-check: fail-before-run-start — a bad reference returns an
// error before any run begins (applyFindingsFrom is pure filesystem;
// nothing here touches a session, database or network).
func TestApplyFindingsFromBadRef(t *testing.T) {
	t.Parallel()
	_, err := applyFindingsFrom("p", filepath.Join(t.TempDir(), "nope.out"))
	require.Error(t, err)
}
