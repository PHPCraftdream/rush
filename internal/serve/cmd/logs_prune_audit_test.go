package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runLogsPrune executes `rush logs prune` through the root command with
// stderr captured: the command writes to os.Stderr directly, so cobra's
// SetErr does not see its output. Cobra keeps flag values on the shared
// command var, so audit-days is reset around every run.
func runLogsPrune(t *testing.T, args ...string) (string, error) {
	t.Helper()

	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	capture, err := os.Create(stderrPath)
	require.NoError(t, err)
	oldStderr := os.Stderr
	os.Stderr = capture
	require.NoError(t, logsPruneCmd.Flags().Set("audit-days", "30"))
	rootCmd.SetArgs(append([]string{"logs", "prune"}, args...))
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = capture.Close()
		rootCmd.SetArgs(nil)
		require.NoError(t, logsPruneCmd.Flags().Set("audit-days", "30"))
	})

	runErr := rootCmd.Execute()
	_ = capture.Sync()
	bts, readErr := os.ReadFile(stderrPath)
	require.NoError(t, readErr)
	return string(bts), runErr
}

// seedAuditDir points RUSH_GLOBAL_DATA and RUSH_GLOBAL_CONFIG at
// throwaway directories so no test touches the real global config.
func seedAuditDir(t *testing.T) string {
	t.Helper()

	dataDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	return dataDir
}

func writeAuditFile(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	return path
}

func TestLogsPruneAuditDefaultRetention(t *testing.T) {
	dataDir := seedAuditDir(t)
	oldFile := writeAuditFile(t, dataDir, "audit-2020-01-01.jsonl")
	todayFile := writeAuditFile(t, dataDir, "audit-"+time.Now().Format("2006-01-02")+".jsonl")
	other := writeAuditFile(t, dataDir, "other.txt")

	logsDir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logPath := filepath.Join(logsDir, "rush.log")
	require.NoError(t, os.WriteFile(logPath, []byte("log line\n"), 0o644))

	stderr, err := runLogsPrune(t, "--data-dir", filepath.Dir(logsDir))
	require.NoError(t, err)
	require.Contains(t, stderr, "pruned 1 audit file(s)")

	_, statErr := os.Stat(oldFile)
	require.True(t, os.IsNotExist(statErr), "the old audit file must be deleted")
	for _, p := range []string{todayFile, other} {
		_, statErr := os.Stat(p)
		require.NoError(t, statErr, "%s must survive", p)
	}
	info, statErr := os.Stat(logPath)
	require.NoError(t, statErr)
	require.Zero(t, info.Size(), "rush.log must still be truncated to zero bytes")
}

func TestLogsPruneAuditDaysZeroDeletesAll(t *testing.T) {
	dataDir := seedAuditDir(t)
	a := writeAuditFile(t, dataDir, "audit-2020-01-01.jsonl")
	b := writeAuditFile(t, dataDir, "audit-2021-01-01.jsonl")
	today := writeAuditFile(t, dataDir, "audit-"+time.Now().Format("2006-01-02")+".jsonl")

	stderr, err := runLogsPrune(t, "--audit-days", "0", "--data-dir", t.TempDir())
	require.NoError(t, err)
	require.Contains(t, stderr, "pruned 3 audit file(s)")
	for _, p := range []string{a, b, today} {
		_, statErr := os.Stat(p)
		require.True(t, os.IsNotExist(statErr), "%s must be deleted", p)
	}
}

func TestLogsPruneAuditDirAbsentTruncatesLog(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-central-dir")
	t.Setenv("RUSH_GLOBAL_DATA", missing)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())

	logsDir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logPath := filepath.Join(logsDir, "rush.log")
	require.NoError(t, os.WriteFile(logPath, []byte("log line\n"), 0o644))

	stderr, err := runLogsPrune(t, "--data-dir", filepath.Dir(logsDir))
	require.NoError(t, err)
	require.Contains(t, stderr, "pruned 0 audit file(s)")
	info, statErr := os.Stat(logPath)
	require.NoError(t, statErr)
	require.Zero(t, info.Size(), "rush.log must be truncated even without an audit dir")
}

func TestLogsPruneAuditDirUnreadable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o644))
	t.Setenv("RUSH_GLOBAL_DATA", blocked)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())

	logsDir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logPath := filepath.Join(logsDir, "rush.log")
	require.NoError(t, os.WriteFile(logPath, []byte("log line\n"), 0o644))

	stderr, err := runLogsPrune(t, "--data-dir", filepath.Dir(logsDir))
	require.Error(t, err, "an unreadable audit dir must fail the command")
	require.Contains(t, stderr, "prune audit files")
	info, statErr := os.Stat(logPath)
	require.NoError(t, statErr)
	require.Zero(t, info.Size(), "rush.log must be truncated before the audit prune fails")
}

func TestLogsPruneAuditDaysNegative(t *testing.T) {
	dataDir := seedAuditDir(t)
	p := writeAuditFile(t, dataDir, "audit-2020-01-01.jsonl")

	_, err := runLogsPrune(t, "--audit-days", "-1", "--data-dir", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "--audit-days")
	_, statErr := os.Stat(p)
	require.NoError(t, statErr, "nothing must be deleted on a negative retention")
}
