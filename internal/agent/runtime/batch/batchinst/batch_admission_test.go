package batchinst

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// batchExpectedIOTools is the fixed literal expectation for the io allowlist.
// It is independent of the production map so dropping an allowlist entry
// fails a test.
var batchExpectedIOTools = []string{
	"view", "glob", "grep", "ls", "edit", "write", "multiedit", "fetch",
	"download", "web_fetch", "web_search", "run_command", "bash", "sourcegraph",
	"git_read", "fs_read", "fs_list", "fs_find", "fs_grep", "fs_write",
	"fs_write_lines", "fs_replace", "fs_delete",
	// Round-2 completeness (KNOWING tripwire update): todos MUTATES session
	// state, so batchIOTools classifies it WORK/io — the literal expectation
	// must include it or the symmetry test fails.
	"todos",
}

// TestBatchClassifyIOTableSymmetry asserts the allowlist equals the literal
// expectation in both directions.
func TestBatchClassifyIOTableSymmetry(t *testing.T) {
	t.Parallel()
	literal := make(map[string]bool, len(batchExpectedIOTools))
	for _, name := range batchExpectedIOTools {
		literal[name] = true
	}
	for name := range batchIOTools {
		require.True(t, literal[name], "allowlist entry %q missing from the literal expectation", name)
	}
	for name := range literal {
		require.True(t, IsIOTool(name), "literal expectation %q absent from the allowlist", name)
	}
	require.Len(t, literal, 24)
}
