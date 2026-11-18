package cmd

// Root test fixtures byte-copied from the sesslocks-moved test files
// (rule 18): staying root tests still need them, and package cmd test
// files cannot import sesslocks in-package. Bodies byte-identical to
// their originals.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	locks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/locks"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	"github.com/stretchr/testify/require"
)

// sanitiseSessionIDForFilename forwards to the exported sesslocks symbol
// (moved with the locks production family; staying root tests call it
// unqualified).
func sanitiseSessionIDForFilename(id string) string { return locks.SanitiseSessionIDForFilename(id) }

// errBatchKillScopeUnconfirmed forwards to the sesslocks sentinel so
// require.ErrorIs in staying root tests keeps matching.
var errBatchKillScopeUnconfirmed = locks.ErrBatchKillScopeUnconfirmed

// killTestHelperProcessEnv — byte-copy of sesslocks kill_test's marker for
// the re-exec'd lock-holding helper process.
const killTestHelperProcessEnv = "RUSH_SESSIONS_KILL_LOCK_HELPER"

// captureStdout runs f while capturing os.Stdout output. Mirrors
// claude_init_test.go's cmdtest.CaptureStderr / models_use_test.go's inline
// stdout-pipe pattern, factored out here since sessionsLocksCmdRun writes
// its table (and --json output) to os.Stdout, not os.Stderr.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

type killTestLockHolder struct {
	cmd     *exec.Cmd
	pid     int
	stdinW  io.WriteCloser
	waitErr chan error
}

// spawnKillTestLockHolder starts a real child process that holds a
// session lock on (dataDir, sessionID) via session.TryAcquireSessionLock,
// blocking until stop() kills it. Blocks until the child reports it
// actually holds the lock.
//
// c.Stdin is deliberately wired to a pipe this function itself controls
// (stdinW), rather than left nil. exec.Cmd treats a nil Stdin as "read from
// the null device" — and reading from the null device returns EOF
// immediately, not "block forever". TestHelperKillTestLockHold's blocking
// read (`os.Stdin.Read(buf)`) therefore returned immediately with a nil
// Stdin, so the child released its lock and exited right after printing
// "LOCKED", instead of staying alive until stop() explicitly killed it. That
// was invisible to callers who probe the lock immediately after spawn
// returns (the child's own exit hadn't yet propagated to the OS lock
// release), but any caller doing a bit more work first — e.g. a second
// setupApp() call — could lose the race and see "no live holder" against a
// holder that looked alive moments earlier. Giving the child a real pipe
// that only closes in stop() makes "blocking until stop()" the helper's
// doc comment promises actually true.
//
// reapInBackground controls whether the child is Wait()-ed concurrently
// with whatever the caller does next, rather than only inside stop().
// This is NOT a "better default" toggle — the two behaviors serve opposite
// test intents and neither is strictly safer than the other:
//
//   - reapInBackground=true (used by TestProbeThenKillHolder_
//     LiveHolderStillKilled and TestSessionsReset_ForceStillKillsLiveHolder):
//     these tests measure whether forceKillHolder's SIGKILL-then-poll
//     actually observes the victim's death within its wait budget. On
//     Unix, a SIGKILL'd child that nobody has Wait()-ed is a zombie, and
//     procinfo.IsProcessAlive (kill(pid, 0)) reports a zombie as "alive"
//     forever — so without a concurrent reaper these tests' own poll loop
//     can never see the exit, independent of whether forceKillHolder's
//     logic is correct. See TestForceKillHolder_LiveProcess's identical
//     concurrent-reap rationale a few dozen lines above for the same
//     mechanism against a plain (non-lock-holding) child.
//   - reapInBackground=false (used by TestProbeThenKillHolder_
//     CapturesVictimGenerationWhileHolderAlive and
//     TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire in
//     sessions_kill_sweep_unix_test.go): these tests deliberately WANT the
//     victim to remain an unreaped zombie for a while after the kill —
//     that window is what lets a second process race in and steal the
//     session lock (proving the generation-capture ordering fix) before
//     the test's own explicit holder.stop() call finally reaps it. Reaping
//     eagerly in the background would collapse that window and the race
//     they exist to exercise would never happen, making forceKillHolder's
//     poll return (and the test's own require.Eventually on the new
//     owner's generation) racy or simply return before the steal occurs.
//
// If a future reader sees an unreaped holder here and "fixes" it by
// flipping this to true unconditionally, they will silently break the
// ordering tests above — read this comment first.
func spawnKillTestLockHolder(t *testing.T, dataDir, sessionID string, reapInBackground bool) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := platform.Command(ctx, exe, "-test.run=^TestHelperKillTestLockHold$")
	c.Env = append(os.Environ(),
		killTestHelperProcessEnv+"=1",
		"RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR="+dataDir,
		"RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID="+sessionID,
	)
	stdoutR, err := c.StdoutPipe()
	require.NoError(t, err)
	c.Stderr = nil

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	c.Stdin = stdinR

	require.NoError(t, c.Start())
	// The child inherited its own copy of the read end via fork/exec; close
	// our reference so the pipe's only remaining open end is stdinW, which
	// stop() closes to signal the child.
	_ = stdinR.Close()

	// See reapInBackground's doc comment above for why this is opt-in
	// rather than always-on. waitErr is buffered so the goroutine can never
	// leak even if stop() ends up being the one to call Wait() instead (it
	// never reads from waitErr when reapInBackground is false).
	var waitErr chan error
	if reapInBackground {
		waitErr = make(chan error, 1)
		go func() { waitErr <- c.Wait() }()
	}

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			lineCh <- line
			return
		}
		lineCh <- ""
	}()

	select {
	case line := <-lineCh:
		require.True(t, strings.HasPrefix(line, "LOCKED"), "helper failed to lock: %s", line)
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatalf("timed out waiting for kill-test helper process to report lock status")
	}

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid, stdinW: stdinW, waitErr: waitErr}
}

// reapedFromWaitErr adapts the background reaper's error channel to the
// struct{}-close shape waitOrDump expects. The background goroutine in
// spawnKillTestLockHolder does `waitErr <- c.Wait()`, so draining it (as the
// old bare `<-h.waitErr` did) is what signals the child has been reaped.
func reapedFromWaitErr(waitErr <-chan error) <-chan struct{} {
	reaped := make(chan struct{})
	go func() {
		<-waitErr
		close(reaped)
	}()
	return reaped
}

func (h *killTestLockHolder) stop(t *testing.T) {
	if h.stdinW != nil {
		_ = h.stdinW.Close()
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	if h.waitErr != nil {
		// A background reap goroutine (spawnKillTestLockHolder with
		// reapInBackground=true) already claimed Wait() on this *exec.Cmd —
		// calling it a second time here would be undefined behavior. Block
		// until that goroutine has actually reaped the child instead, so
		// stop() keeps its original promise that the process is gone by the
		// time it returns.
		if err := waitOrDump(t, reapedFromWaitErr(h.waitErr), 30*time.Second, "background reaper goroutine of lock-holder process"); err != nil {
			t.Error(err)
		}
		return
	}
	// No background reaper (reapInBackground=false, or this holder was
	// built by a different constructor such as spawnSweepTestNewOwner that
	// never sets waitErr at all): nobody has called Wait() on this cmd yet,
	// so stop() is the first and only caller — reap it directly, exactly as
	// before this file introduced the opt-in background reaper.
	waited := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(waited) }()
	if err := waitOrDump(t, waited, 30*time.Second, "Wait() on killed lock-holder process"); err != nil {
		t.Error(err)
	}
}

// TestSessionsKillCmdRun_HonorsConfiguredDataDir is the regression test for
// task #219: sessionsKillCmdRun used to hardcode the lock/data path as
// filepath.Join(cwd, ".rush"), completely ignoring --data-dir. This test
// points --data-dir at a directory that is deliberately NOT <cwd>/.rush,
// writes a real lock file (naming this test process's own PID, mirroring
// the stale-PID pattern used elsewhere in this file) at the path the FIX
// computes (<configured-data-dir>/locks/session-<id>.lock), and runs the
// real sessionsKillCmd.RunE. Before the fix this would print "no lock file
// at <cwd>/.rush/locks/..." (wrong path) and no-op; after the fix it must
// find the lock at the configured location and remove it.

// TestHelperKillTestLockHold is the re-exec entry point of spawnKillTestLockHolder
// in this test binary (byte-copy of the sesslocks one: the child is this very
// binary, so the entry point has to exist here too). A normal `go test` run
// (sentinel env var unset) does nothing.
func TestHelperKillTestLockHold(t *testing.T) {
	if os.Getenv(killTestHelperProcessEnv) != "1" {
		return
	}
	dataDir := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	// Block until killed / stdin closes.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}
