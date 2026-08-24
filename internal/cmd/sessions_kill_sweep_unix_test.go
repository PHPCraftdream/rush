//go:build !windows

package cmd

// Caller-level regression coverage for the 2026-08-19 P0-2 fix
// (static-follow-up review, task #591): the Unix child-group sweep must
// act ONLY on the lock generation that was current at the moment
// contention with the victim was PROVEN, and must run only while the
// session's OS lock is held (or provably acquirable) by the sweeper.
//
// The session package's own tests call KillRegisteredChildGroups with an
// explicit victimGeneration and prove it fences correctly GIVEN the right
// argument. They cannot catch the actual P0 regression — moving the
// session.ReadLockGeneration capture to AFTER forceKillHolder — because
// that is a bug about WHEN the argument is computed, not about what the
// callee does with it. These three tests attack exactly that, from the
// calling side:
//
//   - TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive:
//     the generation token must be read while the victim still holds the
//     OS lock, so a new owner that grabs the session after the kill
//     cannot silently retarget the sweep.
//   - TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire: reset
//     --force must not sweep when its re-acquire of the session lock
//     fails because a new owner already holds the session.
//   - TestSweepChildGroupsUnderOwnLock_BusyLockRefusesAndRetains: the
//     `sessions kill` sweep must refuse to touch the registry while
//     another process holds the session lock.
//
// Determinism note: on Unix, a killed direct child of THIS process stays
// a zombie (still answering kill(pid,0), so still "alive" to
// session.IsProcessAlive) until we explicitly reap it. The first two
// tests exploit that deliberately: forceKillHolder keeps polling inside
// its wait budget while the holder is an unreaped zombie, which gives
// the test a fully controlled window to let a NEW owner acquire the
// session (stamping a new generation) BEFORE the holder's death is
// observed and the post-kill code path runs. That removes every race
// from the moment-ordering assertions below.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// Cross-process helpers.
// ---------------------------------------------------------------------

const sweepTestNewOwnerHelperEnv = "RUSH_SESSIONS_SWEEP_NEWOWNER_HELPER"

// TestHelperSweepTestNewOwner is the re-exec entry point for
// spawnSweepTestNewOwner: it spins trying to acquire the session lock
// (modeling a brand-new `rush run --session <id>` racing in the instant
// the old holder dies), prints LOCKED once it holds it, then blocks
// until its stdin pipe closes.
func TestHelperSweepTestNewOwner(t *testing.T) {
	if os.Getenv(sweepTestNewOwnerHelperEnv) != "1" {
		return
	}
	dataDir := os.Getenv("RUSH_SESSIONS_SWEEP_NEWOWNER_DATADIR")
	sessionID := os.Getenv("RUSH_SESSIONS_SWEEP_NEWOWNER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	var lk *session.SessionLock
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		lk, err = session.TryAcquireSessionLock(dataDir, sessionID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("FAILED %v\n", err)
			os.Exit(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

// spawnSweepTestNewOwner starts the retry-acquiring new-owner helper. It
// deliberately does NOT wait for the helper to acquire: in the tests
// below the OLD holder still holds the lock at this point, so the helper
// is expected to still be spinning. Ownership of the moment at which the
// holder is reaped (and thus the new owner can win the lock) belongs to
// each test.
func spawnSweepTestNewOwner(t *testing.T, dataDir, sessionID string) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := exec.CommandContext(ctx, exe, "-test.run=^TestHelperSweepTestNewOwner$")
	c.Env = append(os.Environ(),
		sweepTestNewOwnerHelperEnv+"=1",
		"RUSH_SESSIONS_SWEEP_NEWOWNER_DATADIR="+dataDir,
		"RUSH_SESSIONS_SWEEP_NEWOWNER_SESSIONID="+sessionID,
	)
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	c.Stdin = stdinR
	c.Stdout = nil
	c.Stderr = nil

	require.NoError(t, c.Start())
	_ = stdinR.Close()

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid, stdinW: stdinW}
}

// spawnSweepGroupLeader starts a live process-group leader (the stand-in
// for a CLI-provider child tree a real holder would have registered) and
// reaps it in the background so that, once something killpg's it,
// session.IsProcessAlive observes the exit instead of stalling on a
// zombie.
func spawnSweepGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	// Not a round duration: matches on command line must not catch other
	// packages' concurrently running sleep children.
	cmd := exec.CommandContext(t.Context(), "sleep", "43.17")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	return cmd
}

func sweepTestRegistryPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".childgroups")
}

// ---------------------------------------------------------------------
// Crash-style lock holder (task #602): a helper that acquires the
// session lock and then blocks with NO clean-release path at all, so the
// only way to end it is a SIGKILL that never runs SessionLock.Release().
// This is deliberately different from spawnKillTestLockHolder /
// spawnSweepTestNewOwner, both of which release cleanly (Release() runs)
// the instant their stdin pipe closes -- useful for modeling a live
// process being asked to shut down, but WRONG for modeling a genuine
// crash, because a clean Release() is exactly the thing that overwrites
// (via the next acquirer) or clears (via clearHolderMetadata) the
// generation sidecar this test needs to survive the holder's death.
// ---------------------------------------------------------------------

const crashTestHelperProcessEnv = "RUSH_SESSIONS_CRASH_LOCK_HELPER"

// TestHelperCrashTestLockHold is the re-exec entry point for
// spawnCrashTestLockHolder: acquires the lock, prints LOCKED, then blocks
// forever on a read from a pipe whose write end is never closed by the
// test -- the ONLY way this process ends is the test's own
// syscall.Kill(pid, SIGKILL), which bypasses Release() entirely, exactly
// like a real crash.
func TestHelperCrashTestLockHold(t *testing.T) {
	if os.Getenv(crashTestHelperProcessEnv) != "1" {
		return
	}
	dataDir := os.Getenv("RUSH_SESSIONS_CRASH_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("RUSH_SESSIONS_CRASH_LOCK_HELPER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	// Block forever. Deliberately no stdin read / no Release() call on any
	// path -- this process must never voluntarily give up the lock or
	// touch its own generation sidecar. select{} rather than blocking on
	// stdin so there is no accidental release path via a closed pipe.
	select {}
}

// spawnCrashTestLockHolder starts a real child process that acquires the
// session lock and then hangs unconditionally. Blocks until the child
// reports it actually holds the lock. Callers MUST end it with
// crashKill(), never stop()/Kill() alone followed by Wait() without
// reaping care -- see crashKill's own doc comment for why a SIGKILL'd
// direct child of this test process still answers kill(pid,0) as "alive"
// until explicitly reaped.
func spawnCrashTestLockHolder(t *testing.T, dataDir, sessionID string) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	c := exec.CommandContext(t.Context(), exe, "-test.run=^TestHelperCrashTestLockHold$")
	c.Env = append(os.Environ(),
		crashTestHelperProcessEnv+"=1",
		"RUSH_SESSIONS_CRASH_LOCK_HELPER_DATADIR="+dataDir,
		"RUSH_SESSIONS_CRASH_LOCK_HELPER_SESSIONID="+sessionID,
	)
	stdoutR, err := c.StdoutPipe()
	require.NoError(t, err)
	c.Stderr = nil

	require.NoError(t, c.Start())
	// Safety net only: every test using this helper is expected to call
	// crashKill explicitly (which SIGKILLs and reaps), but if the test
	// fails/panics before reaching that call, this Cleanup still ensures
	// the hung helper (it blocks in select{} forever, see
	// TestHelperCrashTestLockHold) does not outlive the test binary.
	// Best-effort Wait: if crashKill already reaped it, this Kill is a
	// harmless no-op error against an already-dead pid, and Wait returns
	// promptly either way.
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})

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
		require.True(t, strings.HasPrefix(line, "LOCKED"), "crash helper failed to lock: %s", line)
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatalf("timed out waiting for crash-test helper process to report lock status")
	}

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid}
}

// crashKill SIGKILLs the holder started by spawnCrashTestLockHolder and
// reaps it, WITHOUT ever closing a stdin pipe or otherwise giving it a
// chance to run Release() -- modeling a genuine crash (or `kill -9` from
// outside rush entirely), as opposed to killTestLockHolder.stop()'s
// close-stdin-then-kill sequence, which races a real clean release.
func crashKill(t *testing.T, h *killTestLockHolder) {
	t.Helper()
	require.NoError(t, syscall.Kill(h.pid, syscall.SIGKILL))
	_ = h.cmd.Wait()
}

// TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive is
// the direct regression test for the P0 ordering defect: probeThenKillHolder
// must read the lock's generation token INSIDE the busy branch, BEFORE
// forceKillHolder signals the victim — because after the kill a new
// owner may already have acquired the session and stamped a NEW
// generation, and reading it then silently retargets the later sweep at
// the new owner's child tree.
//
// Revert check: move the single `victimGeneration := session.ReadLockGeneration(...)`
// line in probeThenKillHolder to after the forceKillHolder call and this
// test fails on the final assertion (VictimGeneration would be the NEW
// owner's token).
func TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")
	const sessionID = "victim-gen-capture-id"

	// The OLD holder: a real second process holding the real OS lock. It
	// is our direct child and is deliberately NOT reaped until the test
	// says so — after the probe SIGKILLs it, it stays a zombie, so
	// forceKillHolder's liveness poll keeps waiting inside its budget.
	// reapInBackground=false is load-bearing here: this test's whole point
	// is to occupy that zombie window with a second process stealing the
	// lock generation. See spawnKillTestLockHolder's doc comment.
	holder := spawnKillTestLockHolder(t, dataDir, sessionID, false)
	require.True(t, session.IsProcessAlive(holder.pid))

	lockPath := session.SessionLockPath(dataDir, sessionID)
	oldGeneration := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, oldGeneration, "a live holder's lock must carry a generation token")

	// A brand-new owner, spinning to acquire the session the instant the
	// holder's death releases the OS lock.
	newOwner := spawnSweepTestNewOwner(t, dataDir, sessionID)
	defer newOwner.stop()

	type result struct {
		kr killResult
	}
	krCh := make(chan result, 1)
	go func() {
		kr := probeThenKillHolder(dataDir, sessionID, holder.pid, 15*time.Second)
		krCh <- result{kr: kr}
	}()

	// Wait until the new owner has demonstrably taken over: the on-disk
	// generation sidecar now carries a token DIFFERENT from the victim's.
	// This can only happen after the probe killed the holder (the OS lock
	// was held until then), and while the holder is still an unreaped
	// zombie — i.e. forceKillHolder has NOT returned yet.
	require.Eventually(t, func() bool {
		g := session.ReadLockGeneration(lockPath)
		return g != "" && g != oldGeneration
	}, 10*time.Second, 10*time.Millisecond,
		"the new owner should acquire the session and stamp its own generation while the killed holder is still an unreaped zombie")

	// NOW let forceKillHolder observe the victim's death by reaping it.
	holder.stop()

	var kr killResult
	select {
	case r := <-krCh:
		kr = r.kr
	case <-time.After(30 * time.Second):
		t.Fatal("probeThenKillHolder did not return")
	}
	t.Logf("report: %s", kr.Report)

	require.True(t, kr.ConfirmedDead, "the killed holder must be confirmed dead once reaped")

	// Prove the window really was occupied: at the moment
	// probeThenKillHolder returned, the lock on disk belonged to the NEW
	// owner under the NEW generation.
	currentGeneration := session.ReadLockGeneration(lockPath)
	require.NotEqual(t, oldGeneration, currentGeneration,
		"precondition: the new owner's generation must be what a post-kill read would see")

	require.Equal(t, oldGeneration, kr.VictimGeneration,
		"VictimGeneration must be the token captured while the VICTIM still held the lock — "+
			"a value equal to the current (new owner) generation proves the read was moved after forceKillHolder")
}

// TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire pins the
// ordering half of the P0-2 fix on the reset --force path: the
// child-group sweep must run strictly AFTER acquireSessionLockForReset
// has re-acquired the session lock, never before. Here a new owner
// grabs the session in the window between the old holder's death and
// the re-acquire, so the re-acquire FAILS — and the correct behavior is
// to return an error WITHOUT sweeping anything: the registered victim
// entry stays alive and retained for a later rescue run.
//
// Revert check: move the sweep (sweepChildGroupsWithHeldLock call, or an
// equivalent direct session.KillRegisteredChildGroups call) to before
// the re-acquire in acquireSessionLockForReset, and this test fails: the
// registered live process group gets killed.
func TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")
	const sessionID = "reset-sweep-order-id"

	// reapInBackground=false is load-bearing here too, for the same reason
	// as TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive
	// above: the test needs the killed holder to stay an unreaped zombie
	// long enough for a second process to steal the session lock before
	// acquireSessionLockForReset's own re-acquire is attempted (see
	// spawnKillTestLockHolder's doc comment).
	holder := spawnKillTestLockHolder(t, dataDir, sessionID, false)
	require.True(t, session.IsProcessAlive(holder.pid))

	lockPath := session.SessionLockPath(dataDir, sessionID)
	oldGeneration := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, oldGeneration)

	// A live process-group leader registered under the VICTIM's
	// generation — exactly what the dead holder's sweep would target.
	leader := spawnSweepGroupLeader(t)
	session.RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, oldGeneration)

	newOwner := spawnSweepTestNewOwner(t, dataDir, sessionID)
	defer newOwner.stop()

	type result struct {
		lk  *session.SessionLock
		kr  killResult
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		lk, kr, err := acquireSessionLockForReset(dataDir, sessionID, holder.pid, 15*time.Second)
		resCh <- result{lk: lk, kr: kr, err: err}
	}()

	// Let the new owner take over the session while the killed holder is
	// still an unreaped zombie (so forceKillHolder cannot have returned
	// yet and the re-acquire has not been attempted).
	require.Eventually(t, func() bool {
		g := session.ReadLockGeneration(lockPath)
		return g != "" && g != oldGeneration
	}, 10*time.Second, 10*time.Millisecond,
		"the new owner should acquire the session in the kill-to-reacquire window")

	// Reap the holder so forceKillHolder can confirm death; the
	// re-acquire that follows must then hit the new owner's live lock.
	holder.stop()

	var res result
	select {
	case res = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatal("acquireSessionLockForReset did not return")
	}
	t.Logf("kill report: %s", res.kr.Report)
	t.Logf("error: %v", res.err)

	require.Error(t, res.err,
		"a session whose lock a NEW owner already holds must not be re-acquired")
	require.Nil(t, res.lk)
	require.True(t, res.kr.ConfirmedDead, "the OLD holder itself must still be confirmed dead")
	require.NotContains(t, res.kr.Report, "swept",
		"no sweep may run when the re-acquire failed — a new owner holds the session")
	require.True(t, session.IsProcessAlive(leader.Process.Pid),
		"the victim's registered child group must NOT be signalled by a reset whose re-acquire failed; "+
			"a dead leader here means the sweep ran before (or without) the re-acquire")
	require.FileExists(t, sweepTestRegistryPath(dataDir, sessionID),
		"the victim's registry entry must be retained, not consumed, when the reset aborts")
}

// TestSweepChildGroupsUnderOwnLock_BusyLockRefusesAndRetains pins the
// refusal half: sweepChildGroupsUnderOwnLock must acquire the session's
// OS lock itself before touching the registry. When another process
// holds the session, it must report that it left everything untouched —
// and genuinely leave it untouched: the registry entry and the process
// it points at must both survive, still actionable by a later sweep.
//
// Revert check: strip the TryAcquireSessionLock gate from
// sweepChildGroupsUnderOwnLock (call session.KillRegisteredChildGroups
// directly) and this test fails: the live registered leader is killed
// while the lock is held by someone else.
func TestSweepChildGroupsUnderOwnLock_BusyLockRefusesAndRetains(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")
	const sessionID = "sweep-busy-refuse-id"

	// reapInBackground=false here too, but for a different reason than the
	// two ordering tests above: this test never calls forceKillHolder or
	// probeThenKillHolder against this holder at all (it exercises
	// sweepChildGroupsUnderOwnLock's busy-lock refusal directly, while the
	// holder is still alive, then calls holder.stop() once at the end and
	// waits for the OS lock to become acquirable via TryAcquireSessionLock —
	// a check that is independent of process-table zombie status). So the
	// zombie window is neither needed nor harmful here; false is kept only
	// to match this helper's pre-existing behavior (stop() reaping inline)
	// rather than introducing a third, untested combination. See
	// spawnKillTestLockHolder's doc comment for the two cases that DO care.
	holder := spawnKillTestLockHolder(t, dataDir, sessionID, false)
	require.True(t, session.IsProcessAlive(holder.pid))

	lockPath := session.SessionLockPath(dataDir, sessionID)
	generation := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader := spawnSweepGroupLeader(t)
	session.RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	// The holder (someone else) holds the session lock right now.
	var sb strings.Builder
	sweepChildGroupsUnderOwnLock(&sb, dataDir, sessionID, generation)
	report := sb.String()
	t.Logf("sweep report while lock is busy: %s", report)

	require.Contains(t, report, "could not acquire the session lock to sweep",
		"a busy session lock must be reported, not silently retried or ignored")
	require.NotContains(t, report, "swept",
		"nothing may be swept while another process holds the session lock")
	require.True(t, session.IsProcessAlive(leader.Process.Pid),
		"the registered child group must not be signalled while its owner's lock is held by someone else")
	require.FileExists(t, sweepTestRegistryPath(dataDir, sessionID),
		"the registry entry must be left in place, untouched")

	// Prove the retained entry is still ACTIONABLE: once the holder
	// releases, the very same sweep must reach and kill it.
	// holder.stop() closes the helper's stdin AND kills it; whichever
	// wins, the OS lock dies with the process. Wait for the lock to
	// become genuinely acquirable again (graceful Release or process
	// death — either way), then let any background metadata cleanup
	// settle so the sweep's own acquire below cannot spuriously hit
	// contention against it (see waitForLockMetadataSettled).
	holder.stop()
	require.Eventually(t, func() bool {
		lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
		if err != nil {
			return false
		}
		_ = lk.Release()
		return true
	}, 5*time.Second, 20*time.Millisecond, "the session lock should become acquirable once the holder is gone")
	waitForLockMetadataSettled(lockPath, 2*time.Second)

	var sb2 strings.Builder
	sweepChildGroupsUnderOwnLock(&sb2, dataDir, sessionID, generation)
	t.Logf("sweep report after release: %s", sb2.String())
	require.Contains(t, sb2.String(), "swept 1 registered",
		"after the lock is free the same sweep must act on the retained entry")
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(leader.Process.Pid)
	}, 5*time.Second, 10*time.Millisecond, "the retained entry's process group must die once swept")
}

// TestProbeThenKillHolder_OrphanedGenerationCapturedBeforeProbeAcquire is
// the direct regression test for task #602 (follow-up to #594/#591 P0-2):
// when a holder crashes (or exits without running Release()) BEFORE
// "sessions kill" ever runs against it, probeThenKillHolder's own
// TryAcquireSessionLock succeeds (nobody holds the OS lock), which used to
// report ConfirmedDead with an EMPTY VictimGeneration -- the busy branch
// that captures a generation never ran, because there was no contention to
// prove. With no VictimGeneration, sessionsKillCmdRun's sweep call was
// skipped entirely (see its own "kr.VictimGeneration != ”" gate), so a
// process-group tree orphaned by a crash could never be reached by
// "sessions kill" at all, and its registry entry could never leave the
// file either (a mismatched-generation sweep RETAINS, never discards, per
// #594).
//
// This test builds exactly that scenario with a REAL crashed holder (see
// spawnCrashTestLockHolder/crashKill: SIGKILL with no clean-release path
// at all, so the holder's own generation sidecar is left exactly as it
// wrote it, untouched by any Release()) and a real registered process
// group, then asserts probeThenKillHolder reports a non-empty
// VictimGeneration equal to the crashed holder's own token.
//
// Revert check: delete the "orphanedGeneration := session.ReadLockGeneration(...)"
// line in probeThenKillHolder (the read that happens BEFORE its own
// TryAcquireSessionLock call) and drop the "VictimGeneration: orphanedGeneration"
// field from the holderAlreadyDead return -- this test fails on the
// VictimGeneration assertion (it reverts to "").
func TestProbeThenKillHolder_OrphanedGenerationCapturedBeforeProbeAcquire(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")
	const sessionID = "orphan-gen-capture-id"

	holder := spawnCrashTestLockHolder(t, dataDir, sessionID)
	lockPath := session.SessionLockPath(dataDir, sessionID)
	crashedGeneration := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, crashedGeneration, "a live holder's lock must carry a generation token")

	// Crash it: SIGKILL with no stdin/Release path at all. The generation
	// sidecar this holder wrote is now permanently orphaned on disk --
	// nothing will ever clear or overwrite it until a NEW acquirer does.
	crashKill(t, holder)
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(holder.pid)
	}, 5*time.Second, 10*time.Millisecond, "the crashed holder must actually be gone before the probe runs")

	// Give the OS a moment to fully release the flock after the SIGKILL;
	// TryAcquireSessionLock inside the probe below is the authoritative
	// check, this just avoids a flaky race against kernel lock teardown.
	require.Eventually(t, func() bool {
		lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
		if err != nil {
			return false
		}
		require.NoError(t, lk.Release())
		return true
	}, 5*time.Second, 10*time.Millisecond, "the OS lock must become free once the crashed holder is gone")
	// The probe/release above just overwrote-then-cleared the sidecar via
	// its OWN acquire+Release cycle -- exactly the destructive read this
	// test exists to prove happens. Re-crash a SECOND holder under a WATCHED
	// generation so the real assertion below observes a fresh, untouched
	// sidecar exactly like a real "sessions kill" invocation would.
	holder2 := spawnCrashTestLockHolder(t, dataDir, sessionID)
	crashedGeneration = session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, crashedGeneration)
	crashKill(t, holder2)
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(holder2.pid)
	}, 5*time.Second, 10*time.Millisecond)

	kr := probeThenKillHolder(dataDir, sessionID, holder2.pid, 5*time.Second)
	t.Logf("report: %s", kr.Report)

	require.Equal(t, holderAlreadyDead, kr.State)
	require.True(t, kr.ConfirmedDead)
	require.Equal(t, crashedGeneration, kr.VictimGeneration,
		"VictimGeneration must be the crashed holder's own token, read before this probe's own acquire overwrote the sidecar")
}

// TestSessionsKillCmdRun_SweepsOrphanedGroupAfterCrash is the end-to-end
// counterpart of the test above: a real registered CLI-provider child
// process group, registered under a holder that then crashes (never runs
// Release()), must actually be reached and killed by a real
// sessionsKillCmdRun invocation -- not merely have probeThenKillHolder
// report a non-empty VictimGeneration in isolation.
//
// Revert check: same as TestProbeThenKillHolder_OrphanedGenerationCapturedBeforeProbeAcquire
// (delete the orphanedGeneration capture/threading in probeThenKillHolder)
// and this test fails: the registered leader survives and the registry
// file is left behind unchanged, because sessionsKillCmdRun's
// "kr.VictimGeneration != ”" gate never fires.
func TestSessionsKillCmdRun_SweepsOrphanedGroupAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(workDir, ".rush")
	const sessionID = "orphan-sweep-e2e-id"

	holder := spawnCrashTestLockHolder(t, dataDir, sessionID)
	lockPath := session.SessionLockPath(dataDir, sessionID)
	generation := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader := spawnSweepGroupLeader(t)
	session.RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	crashKill(t, holder)
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(holder.pid)
	}, 5*time.Second, 10*time.Millisecond, "the crashed holder must actually be gone")

	ensureRootFlagStandIns(sessionsKillCmd, dataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "true"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())

	stderr := captureStderr(t, func() {
		runErr := sessionsKillCmd.RunE(sessionsKillCmd, []string{sessionID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions kill stderr:\n%s", stderr)

	require.Contains(t, stderr, "swept 1 registered",
		"a process group registered under a holder that crashed before 'sessions kill' ran must still be reached and killed")
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(leader.Process.Pid)
	}, 5*time.Second, 10*time.Millisecond,
		"the registered process group must actually be dead once the sweep reports it killed")

	_, statErr := os.Stat(sweepTestRegistryPath(dataDir, sessionID))
	require.True(t, os.IsNotExist(statErr), "the registry file must be cleared once its only entry is confirmed killed")
}

// TestAcquireSessionLockForReset_SweepsOrphanedGroupAfterCrash is the
// `sessions reset --force` counterpart of
// TestSessionsKillCmdRun_SweepsOrphanedGroupAfterCrash (task #602,
// coordinator follow-up review): acquireSessionLockForReset's OWN
// TryAcquireSessionLock can equally succeed on the FIRST attempt when the
// previous holder crashed (or exited without Release()) before reset
// --force ever ran against it -- err == nil, no busy branch, so the
// generation-capture line that branch relies on never executes. Before this
// fix that meant VictimGeneration stayed empty and sweepChildGroupsWithHeldLock
// was never called at all for this branch: a process-group tree orphaned by
// a crash survived `sessions reset --force` exactly as it survived
// `sessions kill` before the sibling fix.
//
// Uses acquireSessionLockForReset directly (unit-level), the same style
// TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire above already
// uses for this function's busy-branch behavior -- this test exercises the
// OTHER branch, the one that never goes through forceKillHolder at all.
//
// Revert check: move the "orphanedGeneration := session.ReadLockGeneration(...)"
// read in acquireSessionLockForReset from BEFORE its TryAcquireSessionLock
// call to AFTER it (mirroring the exact revert-check already applied to
// probeThenKillHolder) and this test fails: the registered leader survives
// and the registry file is left behind unchanged.
func TestAcquireSessionLockForReset_SweepsOrphanedGroupAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real child processes; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")
	const sessionID = "reset-orphan-sweep-id"

	holder := spawnCrashTestLockHolder(t, dataDir, sessionID)
	lockPath := session.SessionLockPath(dataDir, sessionID)
	generation := session.ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader := spawnSweepGroupLeader(t)
	session.RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	crashKill(t, holder)
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(holder.pid)
	}, 5*time.Second, 10*time.Millisecond, "the crashed holder must actually be gone")

	lk, kr, err := acquireSessionLockForReset(dataDir, sessionID, holder.pid, 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, lk, "a free lock (no live holder) must still be returned held to the caller")
	t.Cleanup(func() { _ = lk.Release() })
	t.Logf("report: %s", kr.Report)

	require.Equal(t, holderAlreadyDead, kr.State)
	require.True(t, kr.ConfirmedDead)
	require.Contains(t, kr.Report, "swept 1 registered",
		"a process group registered under a holder that crashed before 'sessions reset --force' ran must still be reached and killed")
	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(leader.Process.Pid)
	}, 5*time.Second, 10*time.Millisecond,
		"the registered process group must actually be dead once the sweep reports it killed")

	_, statErr := os.Stat(sweepTestRegistryPath(dataDir, sessionID))
	require.True(t, os.IsNotExist(statErr), "the registry file must be cleared once its only entry is confirmed killed")
}
