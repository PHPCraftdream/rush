// Tests for `rush logs prune`'s heartbeat pruning. Snapshots are produced by
// the heartbeat package's own writer API, then only mutated by decode->edit->
// re-encode of a writer-produced file.
//
// Revert-checks (one single-line production change each; the orchestrator
// runs the mutants, never this file):
//
//	TestLogsPruneHeartbeatRemovesOldKeepsFresh: make pruneHeartbeatFiles skip
//	  its heartbeat.Prune call (or point the dir func at the data dir root
//	  instead of the heartbeat/ subdirectory) — the old snapshot survives and
//	  the outside-the-dir guard file disappears.
//	TestLogsPruneHeartbeatJSONReportsCount: drop the HeartbeatFilesPruned
//	  assignment in logs prune's RunE — the JSON count would be 0.
//	TestLogsPruneHeartbeatDaysZeroDisables: remove the days==0 early return
//	  in pruneHeartbeatFiles — the old snapshot would be pruned anyway.
package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pruneIsolateEnv points the two global config paths at throwaway dirs (no
// RUSH_HEARTBEAT_DIR: logs prune must reach the heartbeat dir through its
// own dir func, the same way production does).
func pruneIsolateEnv(t *testing.T) (dataDir, hbDir string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	hbDir = filepath.Join(dataDir, "heartbeat")
	require.NoError(t, os.MkdirAll(hbDir, 0o755))
	return dataDir, hbDir
}

// resetLogsPruneFlags resets every logs prune flag value and Changed state
// between runs; cobra commands are package-level vars shared by all tests.
func resetLogsPruneFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"audit-days", "heartbeat-days", "json"} {
		if f := logsPruneCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	logsPruneCmd.SetArgs(nil)
}

// runLogsPruneFull runs `rush logs prune` through the root command,
// capturing stdout (--json) and stderr (text) separately.
func runLogsPruneFull(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetLogsPruneFlags(t)
	outPath := filepath.Join(t.TempDir(), "stdout.txt")
	errPath := filepath.Join(t.TempDir(), "stderr.txt")
	outF, oerr := os.Create(outPath)
	require.NoError(t, oerr)
	errF, eerr := os.Create(errPath)
	require.NoError(t, eerr)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outF, errF
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		_ = outF.Close()
		_ = errF.Close()
		resetLogsPruneFlags(t)
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs(append([]string{"logs", "prune"}, args...))
	runErr := rootCmd.Execute()
	_ = outF.Sync()
	_ = errF.Sync()
	_, _ = outF.Seek(0, 0)
	_, _ = errF.Seek(0, 0)
	ob, _ := io.ReadAll(outF)
	eb, _ := io.ReadAll(errF)
	return string(ob), string(eb), runErr
}

func TestLogsPruneHeartbeatRemovesOldKeepsFresh(t *testing.T) {
	dataDir, hbDir := pruneIsolateEnv(t)
	oldSnap := psSeedStoppedRow(t, hbDir, "hb-t-old", "prov", "m-old")
	psRewriteEntry(t, oldSnap, func(doc map[string]any) {
		doc["pid"] = float64(4000000000)
		doc["last_beat"] = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})
	freshSnap := psSeedStoppedRow(t, hbDir, "hb-t-fresh", "prov", "m-fresh")
	// Guard files outside the heartbeat directory must never be touched.
	auditFile := filepath.Join(dataDir, "audit-2020-01-01.jsonl")
	require.NoError(t, os.WriteFile(auditFile, []byte("{}\n"), 0o600))
	otherFile := filepath.Join(dataDir, "other.txt")
	require.NoError(t, os.WriteFile(otherFile, []byte("keep me"), 0o600))

	_, stderr, err := runLogsPruneFull(t, "--heartbeat-days", "7", "--audit-days", "3650", "--data-dir", t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, stderr, "pruned 1 heartbeat file(s)")

	_, statErr := os.Stat(oldSnap)
	require.True(t, os.IsNotExist(statErr), "the 8-day-old dead-process snapshot must be removed")
	_, statErr = os.Stat(freshSnap)
	require.NoError(t, statErr, "the fresh snapshot must be kept")
	_, statErr = os.Stat(auditFile)
	require.NoError(t, statErr, "nothing outside the heartbeat directory may be deleted")
	_, statErr = os.Stat(otherFile)
	require.NoError(t, statErr, "nothing outside the heartbeat directory may be deleted")
}

func TestLogsPruneHeartbeatJSONReportsCount(t *testing.T) {
	_, hbDir := pruneIsolateEnv(t)
	oldSnap := psSeedStoppedRow(t, hbDir, "hb-t-json", "prov", "m-old")
	psRewriteEntry(t, oldSnap, func(doc map[string]any) {
		doc["pid"] = float64(4000000000)
		doc["last_beat"] = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})

	stdout, stderr, err := runLogsPruneFull(t, "--json", "--heartbeat-days", "7", "--audit-days", "3650", "--data-dir", t.TempDir())
	require.NoError(t, err)
	var doc struct {
		LogTruncated         bool `json:"log_truncated"`
		AuditFilesPruned     int  `json:"audit_files_pruned"`
		HeartbeatFilesPruned int  `json:"heartbeat_files_pruned"`
		HeartbeatDays        int  `json:"heartbeat_days"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, 1, doc.HeartbeatFilesPruned)
	assert.Equal(t, 0, doc.AuditFilesPruned)
	assert.Equal(t, 7, doc.HeartbeatDays)
	assert.False(t, doc.LogTruncated)
	assert.NotContains(t, stderr, "pruned 1 heartbeat file(s)", "--json mode must not print the text lines")
}

func TestLogsPruneHeartbeatDaysZeroDisables(t *testing.T) {
	_, hbDir := pruneIsolateEnv(t)
	oldSnap := psSeedStoppedRow(t, hbDir, "hb-t-zero", "prov", "m-old")
	psRewriteEntry(t, oldSnap, func(doc map[string]any) {
		doc["pid"] = float64(4000000000)
		doc["last_beat"] = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})

	stdout, _, err := runLogsPruneFull(t, "--json", "--heartbeat-days", "0", "--audit-days", "3650", "--data-dir", t.TempDir())
	require.NoError(t, err)
	var doc struct {
		HeartbeatFilesPruned int `json:"heartbeat_files_pruned"`
		HeartbeatDays        int `json:"heartbeat_days"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, 0, doc.HeartbeatFilesPruned)
	assert.Equal(t, 0, doc.HeartbeatDays)
	_, statErr := os.Stat(oldSnap)
	require.NoError(t, statErr, "--heartbeat-days 0 must disable the prune")

	_, stderr, err := runLogsPruneFull(t, "--heartbeat-days", "0", "--audit-days", "3650", "--data-dir", t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, stderr, "disabled")
	_, statErr = os.Stat(oldSnap)
	require.NoError(t, statErr)
}
