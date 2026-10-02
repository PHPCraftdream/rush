package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appendLines opens its own O_APPEND handle on path (creator=true
// creates the file first, simulating process A which started when the
// log did not exist yet) and writes n JSON-shaped records. This mirrors
// what a second rush process does: its own handle, opened independently
// of NewLogger's.
func appendLines(t *testing.T, path string, creator bool, n int, tag string) {
	t.Helper()
	flags := os.O_APPEND | os.O_WRONLY
	if creator {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o644)
	require.NoError(t, err)
	defer f.Close()
	for i := 0; i < n; i++ {
		_, err := fmt.Fprintf(f, "{\"tag\":%q,\"i\":%d}\n", tag, i)
		require.NoError(t, err)
	}
}

// readLines returns the file's newline-terminated lines, failing when
// the file ends with a partial (non-newline-terminated) record.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, bts)
	require.Equal(t, byte('\n'), bts[len(bts)-1], "file must not end with a partial record")
	return strings.Split(strings.TrimSuffix(string(bts), "\n"), "\n")
}

// TestTwoWritersNoClobber is the core inter-process safety contract:
// two independent O_APPEND handles (A creates the file, as a rush
// process starting on a fresh workspace would) interleaving records
// must yield exactly all whole records — no truncation, no overwritten
// lines.
//
// Revert-check: restoring lumberjack (O_TRUNC open, no O_APPEND) makes
// writer B truncate the file at open and both writers race offsets, so
// the file loses or corrupts records and this test fails.
func TestTwoWritersNoClobber(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "rush-log-shared")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "rush.log")

	// Process A: creates the file and writes its first half first.
	appendLines(t, path, true, 1000, "A")

	// Process B: the NewLogger under test — an independent handle on
	// the same path, opened after A's records exist. B's handle must
	// NOT truncate what A wrote.
	logger := NewLogger(path, false)
	for i := 0; i < 1000; i++ {
		logger.Info("B record", "i", i)
	}

	// Process A appends another round after B started.
	appendLines(t, path, false, 1000, "A2")

	lines := readLines(t, path)
	require.Len(t, lines, 3000, "every record from both handles must survive")

	aCount, a2Count, bCount := 0, 0, 0
	for i, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record),
			"line %d must be a whole JSON record: %s", i, line)
		switch {
		case record["tag"] == "A":
			aCount++
		case record["tag"] == "A2":
			a2Count++
		case record["msg"] == "B record":
			bCount++
		}
	}
	assert.Equal(t, 1000, aCount, "all of A's pre-B records must survive")
	assert.Equal(t, 1000, a2Count, "all of A's post-B records must survive")
	assert.Equal(t, 1000, bCount, "all of B's records must survive")
}

// TestLargeFileStillWritable covers the Windows rotation lockup: with
// lumberjack, a file at or past MaxSize (10 MB) held open by another
// process made rotation fail (os.Rename cannot move a pinned file on
// Windows) and — the handle being already closed by the failed rotate —
// every subsequent write retried and failed, so a process starting at
// that moment never logged at all. With the append-only writer there
// is no rotation and no size gate: the new handle must append to a
// large file without disturbing the existing bytes. Runs on every OS
// (the mechanism is universal; the failure was Windows-specific).
func TestLargeFileStillWritable(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "rush-log-large")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "rush.log")

	// Another process holds the file at ≥ lumberjack's old MaxSize.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	big := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 11; i++ { // 11 MB > 10 MB
		_, err := f.Write(big)
		require.NoError(t, err)
	}

	logger := NewLogger(path, false)
	logger.Info("survived a large shared log")
	require.NoError(t, f.Close())

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	prefixLen := 11 << 20
	require.Equal(t, bytes.Repeat([]byte("x"), prefixLen), bts[:prefixLen],
		"existing bytes must be untouched")
	tail := bts[prefixLen:]
	require.Contains(t, string(tail), `"msg":"survived a large shared log"`,
		"an append-only writer must keep logging regardless of file size")
}

// TestAttribution checks the per-record contract: every line carries
// pid and ws, and ProcessStart produces exactly one record with the
// start facts. Uses Setup (not NewLogger) so the record goes through
// the real default-logger path; this test must stay serial because
// both Setup and ProcessStart are process-once, and it must therefore
// live in exactly one test binary run per package.
func TestAttribution(t *testing.T) {
	dir, err := os.MkdirTemp("", "rush-log-attr")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "logs", "rush.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	Setup(path, false)
	ProcessStart("rush run", "sess-123")
	ProcessStart("rush run", "sess-123") // second call must be a no-op

	slog.Default().Info("after start")

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(bts), "\n"), "\n")
	require.NotEmpty(t, lines)

	startCount := 0
	for _, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "whole record expected: %s", line)
		assert.Equal(t, float64(os.Getpid()), record["pid"])
		assert.Contains(t, record, "ws", "every record must carry ws")
		if record["msg"] == "process start" {
			startCount++
			assert.Equal(t, float64(os.Getppid()), record["ppid"])
			assert.NotEmpty(t, record["version"])
			assert.Equal(t, "rush run", record["command"])
			assert.Equal(t, "sess-123", record["session"])
			assert.Equal(t, dir, record["data_dir"])
			cwd, _ := os.Getwd()
			assert.Equal(t, cwd, record["cwd"])
			branch, ok := record["branch"].(string)
			require.True(t, ok)
			_ = branch // may legitimately be "" (detached / shallow copy)
		}
	}
	assert.Equal(t, 1, startCount, "exactly one process start record")
}

// TestWorkspaceRootAndBranch pins the ws computation and branch
// resolution against the real repository this test runs in: the
// worktree root must be an ancestor of the test dir and currentBranch
// must agree with `git rev-parse --abbrev-ref HEAD` (skipped when git
// or a branch is unavailable, e.g. detached CI checkouts).
func TestWorkspaceRootAndBranch(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	require.NoError(t, err)
	root := WorkspaceRoot()
	require.NotEmpty(t, root)
	abs, err := filepath.Abs(wd)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(abs, root+string(os.PathSeparator)) || abs == filepath.Clean(root),
		"ws %q must be an ancestor of the test dir %q", root, abs)

	toplevel, gitDir := locateGit(wd)
	require.NotEmpty(t, toplevel)
	require.NotEmpty(t, gitDir)
	_, err = os.Stat(filepath.Join(gitDir, "HEAD"))
	require.NoError(t, err, "gitDir must be the real git directory containing HEAD")

	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	out, err := platform.Command(t.Context(), git, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Skip("cannot resolve current branch (detached or no repo)")
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" {
		t.Skip("detached HEAD")
	}
	assert.Equal(t, branch, currentBranch())
}

// recordingWriter verifies the invariant inter-process safety rests
// on: the slog JSON handler hands each record to the writer in a
// single Write call, so a record cannot be split into chunks that
// could interleave with another process's writes.
type recordingWriter struct {
	writes [][]byte
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestAppendAtomicOneWritePerRecord(t *testing.T) {
	t.Parallel()

	rw := &recordingWriter{}
	logger := NewLogger("", false, rw)
	logger.Info("one record", "k", "v")
	require.Len(t, rw.writes, 1, "one record must be exactly one Write")
	require.True(t, bytes.HasSuffix(rw.writes[0], []byte("\n")))
	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimRight(rw.writes[0], "\n"), &record))
	assert.Equal(t, "one record", record["msg"])
}
