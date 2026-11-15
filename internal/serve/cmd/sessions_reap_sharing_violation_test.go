package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A lock that reap has already PROVEN dead must still get removed when the
// unlink briefly loses to an open handle.
//
// This is what broke CI on windows-latest for ce81c814. reap probed the
// lock, acquired it, proved no live holder — and then lost a single-shot
// os.Remove to ERROR_SHARING_VIOLATION:
//
//	failed to remove (remove ...session-....lock: The process cannot access
//	the file because it is being used by another process.) orphan lock
//	... (holder provably dead via OS lock, age 60s)
//	reclaimed 0 lock(s)
//
// The likeliest holder is reap's own probe: SessionLock.Release closes the
// lock handle synchronously but then clears the holder metadata in a
// goroutine that REOPENS the file, and waits only
// releaseMetadataCleanupBound (50ms) for it. Under -race on a loaded runner
// that reopened handle outlives the wait. A scanner or indexer touching a
// freshly written file produces exactly the same failure, so the test does
// not try to pin which one it was — it pins the property that survives
// either: a handle that is on its way out must not cost the operator the
// reclaim.
//
// Windows only, and deliberately so: on Unix unlink succeeds with handles
// open, so the pre-fix single-shot Remove would pass there and the test
// could never fail — a green run elsewhere would be meaningless.
func TestSessionsReapCmdRun_SurvivesTransientSharingViolation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to unlink a file that still has an open handle")
	}

	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	configuredDataDir := filepath.Join(tmp, "reap-sharing-violation")
	ensureRootFlagStandIns(sessionsReapCmd, configuredDataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	const sessionID = "sharing-violation-reap-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	// PID 999999 is guaranteed not to be a live process on any platform, and
	// nothing holds the OS lock, so the probe will classify this as dead.
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	// The transient holder. A plain read handle: it does NOT take the OS
	// lock, so the probe still proves the session dead — exactly like the
	// background cleanup goroutine and the scanners this stands in for. One
	// second is ~10x the time reap needs to reach the unlink, so the first
	// attempt reliably fails, and it is well inside reapRemoveRetryWindow,
	// so the retry reliably succeeds.
	held, err := os.Open(lockPath)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { _ = held.Close() }) }
	t.Cleanup(release)
	go func() {
		time.Sleep(time.Second)
		release()
	}()

	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})
	t.Logf("sessions reap stderr:\n%s", stderr)

	require.Contains(t, stderr, "reclaimed 1 lock",
		"a handle that lets go a moment later must not cost the reclaim — "+
			"a single-shot Remove reports 'failed to remove' and reclaims 0")
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "the orphan lock file must actually be gone")
}

// The retry budget is shared by the whole sweep, and it is spent.
//
// The test above is the one that proves retrying HELPS, but it can only do
// that by letting the handle go mid-sweep — so on a runner slow enough to
// reach the unlink after the handle is already closed, it would pass
// without the retry ever running and nothing would say so. This one cannot:
// the handles are never released, so the assertions are only satisfiable if
// the loop actually ran to its bound.
//
// Three locks, because that is what distinguishes the two designs with
// room to spare. A budget granted per lock — which is what this started as
// — takes ~3x the budget here and scales with the size of the crash backlog
// reap exists for: 40 undeletable locks meant two silent minutes. One
// shared budget takes ~1x no matter how many there are. Measured: 3.09s
// shared vs 9s per-lock, against a 2x threshold, so neither a slow runner
// nor a fast one lands near the line. (Two locks put the per-lock case at
// 6.09s against a 6s threshold — correct, but decided by 90ms.)
func TestSessionsReapCmdRun_RetryBudgetIsSharedAndBounded(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to unlink a file that still has an open handle")
	}

	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	configuredDataDir := filepath.Join(tmp, "reap-retry-budget")
	ensureRootFlagStandIns(sessionsReapCmd, configuredDataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	old := time.Now().Add(-time.Minute)
	for _, id := range []string{"budget-lock-a", "budget-lock-b", "budget-lock-c"} {
		lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(id)+".lock")
		require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
		require.NoError(t, os.Chtimes(lockPath, old, old))

		// Held for the whole command — never released, so every attempt
		// fails and the budget is genuinely exhausted rather than won.
		held, openErr := os.Open(lockPath)
		require.NoError(t, openErr)
		t.Cleanup(func() { _ = held.Close() })
	}

	start := time.Now()
	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})
	elapsed := time.Since(start)
	t.Logf("sessions reap took %s, stderr:\n%s", elapsed, stderr)

	require.GreaterOrEqual(t, elapsed, reapRemoveRetryBudget,
		"the sweep must actually spend the budget retrying — an elapsed time below it "+
			"means the removal was single-shot, which is the bug")
	require.Less(t, elapsed, 2*reapRemoveRetryBudget,
		"the budget must be shared by the sweep: granted per lock, three locks take ~3x, "+
			"and a real crash backlog of 40 takes ~40x")
	require.Contains(t, stderr, "retry budget",
		"the operator has to be told the later locks got one attempt, or a failure "+
			"below that line reads as 'retried and lost' rather than 'did not retry'")
	require.Contains(t, stderr, "reclaimed 0 lock(s)")
}
