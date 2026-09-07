package sdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryEphemeralDisabledToolsAreExhaustive(t *testing.T) {
	expected := []string{
		"bash", "run_command", "download", "agentic_fetch", "rush_logs", "git_read",
		"edit", "multiedit", "glob", "grep", "ls", "view", "write",
		"fs_list", "fs_find", "fs_grep", "fs_read",
		"fs_write", "fs_replace", "fs_write_lines", "fs_delete",
	}
	require.ElementsMatch(t, expected, libraryEphemeralDisabledTools)
}
