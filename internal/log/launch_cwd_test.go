package log

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/stretchr/testify/require"
)

// TestNewLoggerTagsRecordsWithLaunchCwd: every record — on the file
// handler and on extra-writer handlers — carries launch_cwd equal to
// audit.LaunchCwd(), and it stays the launch value after an os.Chdir.
//
// Revert-check: removing the launch_cwd attr from NewLogger turns this red.
func TestNewLoggerTagsRecordsWithLaunchCwd(t *testing.T) {
	// Not t.TempDir: the log file handle stays open for the life of the
	// logger, so Windows cannot unlink it during cleanup.
	dir, err := os.MkdirTemp("", "rush-log-launchcwd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	other := t.TempDir()
	t.Chdir(other)

	var extra bytes.Buffer
	logPath := filepath.Join(dir, "rush.log")
	logger := NewLogger(logPath, false, &extra)
	logger.Info("one")
	logger.Info("two")

	bts, err := os.ReadFile(logPath)
	require.NoError(t, err)
	want := audit.LaunchCwd()
	require.NotEmpty(t, want)
	require.NotEqual(t, other, want, "launch_cwd must be the launch snapshot, not the current cwd")

	var lines []string
	lines = append(lines, splitLines(string(bts))...)
	lines = append(lines, splitLines(extra.String())...)
	require.Len(t, lines, 4, "two records on the file handler, two on the extra writer")
	for _, line := range lines {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "line: %s", line)
		require.Equal(t, want, rec["launch_cwd"], "line: %s", line)
	}
}

// splitLines trims the trailing newline and splits a JSONL stream.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(s), "\n")
}
