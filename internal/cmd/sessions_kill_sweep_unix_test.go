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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// Cross-process helpers.
// ---------------------------------------------------------------------

const sweepTestNewOwnerHelperEnv = "CRUSH_SESSIONS_SWEEP_NEWOWNER_HELPER"

// TestHelperSweepTestNewOwner is the re-exec entry point for
// spawnSweepTestNewOwner: it spins trying to acquire the session lock
// (modeling a brand-new `crush run --session <id>` racing in the instant
// the old holder dies), prints LOCKED once it holds it, then blocks
// until its stdin pipe closes.
func TestHelperSweepTestNewOwner(t *testing.T) {
	if os.Getenv(sweepTestNewOwnerHelperEnv) != "1" {
		return
	}
	dataDir := os.Getenv("CRUSH_SESSIONS_SWEEP_NEWOWNER_DATADIR")
	sessionID := os.Getenv("CRUSH_SESSIONS_SWEEP_NEWOWNER_SESSIONID")
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
		"CRUSH_SESSIONS_SWEEP_NEWOWNER_DATADIR="+dataDir,
		"CRUSH_SESSIONS_SWEEP_NEWOWNER_SESSIONID="+sessionID,
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
	dataDir := filepath.Join(dir, ".crush")
	const sessionID = "victim-gen-capture-id"

	// The OLD holder: a real second process holding the real OS lock. It
	// is our direct child and is deliberately NOT reaped until the test
	// says so — after the probe SIGKILLs it, it stays a zombie, so
	// forceKillHolder's liveness poll keeps waiting inside its budget.
	holder := spawnKillTestLockHolder(t, dataDir, sessionID)
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
	dataDir := filepath.Join(dir, ".crush")
	const sessionID = "reset-sweep-order-id"

	holder := spawnKillTestLockHolder(t, dataDir, sessionID)
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
	dataDir := filepath.Join(dir, ".crush")
	const sessionID = "sweep-busy-refuse-id"

	holder := spawnKillTestLockHolder(t, dataDir, sessionID)
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
