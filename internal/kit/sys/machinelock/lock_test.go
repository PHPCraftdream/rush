package machinelock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStaleHolderTakeover(t *testing.T) {
	t.Parallel()
	name := "dead-holder-" + uniqueSuffix(t)
	base := lockDir(name)
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, writeJSONExclusive(filepath.Join(base, "holder.json"), Record{PID: 99999999, Token: "dead"}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := Acquire(ctx, name, Record{Command: "test"}, time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Release())
}

func TestFIFOConcurrentAcquire(t *testing.T) {
	name := "fifo-" + uniqueSuffix(t)
	first, err := Acquire(context.Background(), name, Record{Command: "first"}, time.Second)
	require.NoError(t, err)
	var mu sync.Mutex
	order := []string{}
	started := make(chan struct{}, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for _, label := range []string{"second", "third"} {
		label := label
		go func() {
			defer wg.Done()
			started <- struct{}{}
			h, acquireErr := Acquire(context.Background(), name, Record{Command: label}, 10*time.Second)
			if acquireErr != nil {
				t.Errorf("Acquire(%s): %v", label, acquireErr)
				return
			}
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			_ = h.Release()
		}()
		<-started
		time.Sleep(20 * time.Millisecond)
	}
	require.Eventually(t, func() bool {
		entries, _ := List()
		for _, e := range entries {
			if e.Holder != nil && e.Holder.Command == "first" && len(e.Waiters) == 2 {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, first.Release())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"second", "third"}, order)
}

func TestRunExitCode(t *testing.T) {
	t.Parallel()
	var argv []string
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "exit 3"}
	} else {
		argv = []string{"sh", "-c", "exit 3"}
	}
	code, err := Run(context.Background(), "exit-code-"+uniqueSuffix(t), Record{}, time.Second, time.Second, argv, os.Stdout, os.Stderr)
	require.NoError(t, err)
	require.Equal(t, 3, code)
}

func TestStuckReleaseQuarantinesAndLeavesProcess(t *testing.T) {
	name := "stuck-release-" + uniqueSuffix(t)
	argv := testSleepCommand("")
	if runtime.GOOS == "windows" {
		argv = []string{"powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 300"}
	}
	code, err := RunWithOptions(context.Background(), name, Record{Command: "sleep fixture"}, Options{Wait: time.Second, Idle: 250 * time.Millisecond, OnStuck: StuckRelease}, argv, os.Stdout, os.Stderr)
	require.NoError(t, err)
	require.Zero(t, code)
	entries, err := List()
	require.NoError(t, err)
	var quarantined *Record
	for _, entry := range entries {
		if entry.Name != filepath.Base(lockDir(name)) {
			continue
		}
		for i := range entry.Quarantined {
			if quarantined == nil || entry.Quarantined[i].StartedAt.After(quarantined.StartedAt) {
				quarantined = &entry.Quarantined[i]
			}
		}
	}
	require.NotNil(t, quarantined)
	require.True(t, processAlive(quarantined.PID), "release must not terminate the process")
	proc, err := os.FindProcess(quarantined.PID)
	require.NoError(t, err)
	_ = proc.Kill()
}

func TestStuckDefaultLeavesTreeAndRecordsSnapshot(t *testing.T) {
	name := "stuck-default-" + uniqueSuffix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	argv := testSleepCommand("")
	_, runErr := RunWithOptions(ctx, name, Record{Command: "sleep fixture"}, Options{Wait: time.Second, Idle: 250 * time.Millisecond}, argv, os.Stdout, os.Stderr)
	require.Error(t, runErr)
	var holder Record
	require.NoError(t, readJSON(filepath.Join(lockDir(name), "holder.json"), &holder))
	require.True(t, holder.Stuck)
	require.Contains(t, holder.Snapshot, "Tree")
	require.Contains(t, holder.Snapshot, "checkout")
	require.True(t, processAlive(holder.PID), "default stuck handling must leave the tree running")
	proc, err := os.FindProcess(holder.PID)
	require.NoError(t, err)
	_ = proc.Kill()
	_ = os.Remove(filepath.Join(lockDir(name), "holder.json"))
}

func TestStuckStopGracefulThenForce(t *testing.T) {
	name := "stuck-stop-force-" + uniqueSuffix(t)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	require.NoError(t, err)
	defer stderr.Close()
	argv := []string{"sh", "-c", "sleep 30 & wait"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "ping -n 30 127.0.0.1 >NUL"}
	}
	code, runErr := RunWithOptions(context.Background(), name, Record{Command: "stalled process tree"}, Options{Wait: time.Second, Idle: time.Second, Grace: 200 * time.Millisecond, OnStuck: StuckStop, ForceSafe: true}, argv, os.Stdout, stderr)
	require.NoError(t, runErr)
	require.NotEqual(t, 0, code)
	var holder Record
	require.ErrorIs(t, readJSON(filepath.Join(lockDir(name), "holder.json"), &holder), os.ErrNotExist)
}

func TestStuckStopRefusesForceWithoutForceSafe(t *testing.T) {
	name := "stuck-stop-no-force-" + uniqueSuffix(t)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	require.NoError(t, err)
	defer stderr.Close()
	// The Unix fixture ignores SIGTERM (children inherit the ignore), like a Windows console
	// program survives a graceful taskkill: only then does the stuck command outlive the stop.
	argv := []string{"sh", "-c", "trap '' TERM; sleep 30 & wait"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "ping -n 30 127.0.0.1 >NUL"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, runErr := RunWithOptions(ctx, name, Record{Command: "stalled process tree"}, Options{Wait: time.Second, Idle: time.Second, Grace: 100 * time.Millisecond, OnStuck: StuckStop}, argv, os.Stdout, stderr)
	require.ErrorIs(t, runErr, context.DeadlineExceeded)
	require.NoError(t, stderr.Sync())
	_, err = stderr.Seek(0, 0)
	require.NoError(t, err)
	output, err := os.ReadFile(stderr.Name())
	require.NoError(t, err)
	require.Contains(t, string(output), "Force-kill skipped: command class is not declared --force-safe.")
	var holder Record
	require.NoError(t, readJSON(filepath.Join(lockDir(name), "holder.json"), &holder))
	proc, err := os.FindProcess(holder.PID)
	require.NoError(t, err)
	cleanup := &exec.Cmd{Process: proc}
	forceStop(cleanup)
	_ = os.Remove(filepath.Join(lockDir(name), "holder.json"))
}

func TestProgressingTreeNotStuck(t *testing.T) {
	name := "progressing-tree-" + uniqueSuffix(t)
	progressPath := filepath.Join(os.TempDir(), "rush-progress-"+filepath.Base(t.TempDir()))
	t.Cleanup(func() { _ = os.Remove(progressPath) })
	require.NoError(t, os.WriteFile(progressPath, nil, 0o600))
	argv := []string{"sh", "-c", "while :; do date +%s >> \"$1\"; sleep 0.5; done", "sh", progressPath}
	if runtime.GOOS == "windows" {
		argv = []string{"powershell", "-NoProfile", "-Command", "[IO.File]::AppendAllText('" + progressPath + "', 'progress' + [Environment]::NewLine); for ($i = 0; $i -lt 60; $i++) { Start-Sleep -Milliseconds 500; [IO.File]::AppendAllText('" + progressPath + "', 'progress' + [Environment]::NewLine) }"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		_, runErr := RunWithOptions(ctx, name, Record{Command: "progressing process tree"}, Options{Wait: time.Second, Idle: 10 * time.Second}, argv, os.Stdout, os.Stderr)
		runDone <- runErr
	}()

	var holderPID int
	t.Cleanup(func() {
		if holderPID != 0 {
			if proc, err := os.FindProcess(holderPID); err == nil {
				_ = proc.Kill()
			}
		}
		_ = os.Remove(filepath.Join(lockDir(name), "holder.json"))
	})
	falseSamples := 0
	for {
		select {
		case runErr := <-runDone:
			require.ErrorIs(t, runErr, context.DeadlineExceeded)
			contents, _ := os.ReadFile(progressPath)
			require.GreaterOrEqual(t, len(strings.Fields(string(contents))), 3)
			require.GreaterOrEqual(t, falseSamples, 3, "holder should remain not stuck across several samples")
			return
		case <-time.After(250 * time.Millisecond):
			var holder Record
			err := readJSON(filepath.Join(lockDir(name), "holder.json"), &holder)
			require.NoError(t, err)
			holderPID = holder.PID
			require.False(t, holder.Stuck, "activity should prevent stuck diagnosis")
			falseSamples++
		}
	}
}

// The Unix snapshot is built from portable ps: it lists the live tree and says plainly that
// exit codes of exited children cannot be recovered (a known gap, see the backlog).
func TestStuckSnapshotShowsProcessTreeUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("tree enumeration not implemented on windows")
	}
	name := "stuck-child-exited-" + uniqueSuffix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
	defer cancel()
	argv := []string{"sh", "-c", "(true; exit 0) & sleep 30"}
	_, runErr := RunWithOptions(ctx, name, Record{Command: "parent hangs after child exits"}, Options{Wait: time.Second, Idle: time.Second}, argv, os.Stdout, os.Stderr)
	require.ErrorIs(t, runErr, context.DeadlineExceeded)
	var holder Record
	require.NoError(t, readJSON(filepath.Join(lockDir(name), "holder.json"), &holder))
	require.True(t, holder.Stuck)
	require.Contains(t, holder.Snapshot, "Tree (exit codes are lost once anything is killed")
	require.Contains(t, holder.Snapshot, "sleep 30")
	require.True(t, processAlive(holder.PID), "default stuck handling must leave the parent running")
	proc, err := os.FindProcess(holder.PID)
	require.NoError(t, err)
	_ = proc.Kill()
	_ = os.Remove(filepath.Join(lockDir(name), "holder.json"))
}

func testSleepCommand(_ string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "ping -n 30 127.0.0.1 >NUL"}
	}
	return []string{"sh", "-c", "sleep 30"}
}

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "rush-lock-test-")
	require.NoError(t, err)
	name := filepath.Base(f.Name())
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
	return name
}
