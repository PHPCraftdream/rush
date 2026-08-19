//go:build !windows

package session

// Regression coverage for the 2026-08-19 release-readiness static
// follow-up review (task #591, part 3): the cross-process child-group
// registry's write path and its sweep-time retention policy.
//
//  1. Atomic write: writeChildGroupFileLocked previously used a plain
//     truncate-and-write os.WriteFile. An external kill (or crash) landing
//     mid-write left a later sweep reading an empty or partially-written
//     registry at exactly the moment the registry exists to be read. The
//     fix is a temp-file-in-the-same-directory + fsync + rename sequence.
//     A genuine kill-mid-write race cannot be reproduced deterministically
//     from a Go test (that would need to interrupt the write syscall at an
//     exact byte offset), so this file instead pins the OBSERVABLE
//     properties that the atomic-rename design guarantees and a
//     truncate-and-write does not: no reader ever observes a truncated or
//     missing file between two successful writes, and no stray temp file
//     is left behind after a successful write.
//
//  2. Retention: KillRegisteredChildGroups previously removed the registry
//     file unconditionally after one sweep attempt, including when a
//     kill(2) call failed with something other than "confirmed already
//     gone" (ESRCH) -- discarding the only durable pointer to a child tree
//     that might still be reachable on a later retry. The fix only drops
//     entries with a CONFIRMED outcome (killed, or confirmed already
//     dead/mismatched) and writes back everything else. killpgFunc (a
//     package-level test seam, see its own doc) lets this be proven
//     deterministically without needing root or a real permission-denied
//     process group.
//
// THIS FILE'S TESTS RUN ONLY WHERE THEY WERE WRITTEN AND CROSS-COMPILED
// (Windows dev machine): go build/vet and `go test -c` both succeed for
// GOOS=linux and GOOS=darwin (see the task's own verification section),
// but the tests themselves have NOT been executed on real Unix from this
// machine. That is the same residual gap already documented on this
// package's other //go:build !windows tests (see e.g.
// TestStartTimeToken_SelfProcess's own doc) -- a real Linux/macOS run is
// what would additionally confirm these pass in practice, not just compile
// and typecheck.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteChildGroupFileLocked_NoStrayTempFileAfterSuccess pins that a
// successful write leaves exactly the final file behind -- no leftover
// ".tmp-*" file from the atomic-rename sequence. A regression to the old
// truncate-and-write os.WriteFile would still pass this (it never created a
// temp file to begin with), but a BROKEN version of the new sequence that
// forgets the rename step, or renames to the wrong path, would fail it --
// either by leaving the temp file behind or by the final path not existing.
func TestWriteChildGroupFileLocked_NoStrayTempFileAfterSuccess(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-atomic-write-no-strays"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader, _ := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	path := childGroupRegistryPath(dataDir, sessionID)
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	sawFinal := false
	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(path) {
			sawFinal = true
			continue
		}
		require.False(t, strings.Contains(name, ".tmp-"),
			"a stray temp file %q was left behind after a successful atomic write", name)
	}
	require.True(t, sawFinal, "the final registry file must exist after a successful write")
}

// TestWriteChildGroupFileLocked_NeverObservedTruncated writes the registry
// repeatedly (simulating RegisterChildGroup/UnregisterChildGroup churn from
// a long-lived crush process starting and stopping many CLI streams) while
// a second goroutine continuously reads the SAME file directly with
// os.ReadFile, outside childGroupFileMu -- exactly how an external
// "sessions kill" process would read it, with no coordination available
// across processes. Every read must be either empty (file briefly absent
// between an old rename-away and... no: rename replaces atomically, so
// "absent" should never happen once the file has been created once) or a
// COMPLETE, parseable set of whole lines -- never a partial line, which is
// what a truncate-then-write (the pre-fix os.WriteFile shape) could produce
// if a reader's os.ReadFile happened to land between the truncate and the
// new bytes landing.
func TestWriteChildGroupFileLocked_NeverObservedTruncated(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-atomic-write-no-partial-reads"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	path := childGroupRegistryPath(dataDir, sessionID)

	const writers = 40
	pgids := make([]int, 0, writers)
	for i := 0; i < writers; i++ {
		leader, _ := spawnGroupChild(t, true)
		pgids = append(pgids, leader.Process.Pid)
	}

	stop := make(chan struct{})
	readErrs := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				// Not-yet-created is fine (first Register call has not run
				// yet); any other error, or a file that exists but is
				// mid-write-visible as some OTHER error, is not.
				if os.IsNotExist(err) {
					continue
				}
				select {
				case readErrs <- "unexpected read error: " + err.Error():
				default:
				}
				return
			}
			content := strings.TrimRight(string(data), "\n")
			if content == "" {
				continue
			}
			for _, line := range strings.Split(content, "\n") {
				fields := strings.Fields(line)
				// Every line written by writeChildGroupFileLocked has at
				// least 2 fields (pgid, generation). A partial write from a
				// truncate-then-write race could produce a line with 0 or 1
				// fields (a half-written pgid, or a pgid with no
				// generation yet) -- that is exactly the corruption this
				// test exists to catch.
				if len(fields) < 2 {
					select {
					case readErrs <- "observed a partial/malformed line: " + line:
					default:
					}
					return
				}
			}
		}
	}()

	for _, pgid := range pgids {
		RegisterChildGroup(dataDir, sessionID, pgid, generation)
	}
	for _, pgid := range pgids {
		UnregisterChildGroup(dataDir, sessionID, pgid)
	}

	close(stop)
	select {
	case msg := <-readErrs:
		t.Fatal(msg)
	default:
	}
}

// TestVerifyGroupStillPlausibleOutcome_MismatchIsConfirmedNotInconclusive
// pins that a start-time MISMATCH against a live, still-a-leader pgid is
// classified as plausibilityConfirmedMismatch (confirmed, safe to drop),
// not plausibilityInconclusive (retained) -- the two must not be conflated,
// or a genuinely reused pgid would be retried forever instead of being
// recognised as belonging to a different process now.
func TestVerifyGroupStillPlausibleOutcome_MismatchIsConfirmedNotInconclusive(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	pgid := leader.Process.Pid

	realStartTime, ok := startTimeToken(pgid)
	if !ok {
		t.Skip("startTimeToken unavailable on this platform (e.g. macOS) -- mismatch classification cannot be exercised without it")
	}
	require.NotEmpty(t, realStartTime)

	outcome := verifyGroupStillPlausibleOutcome(pgid, realStartTime+"-does-not-match")
	require.Equal(t, plausibilityConfirmedMismatch, outcome)

	// And the plain-bool wrapper must agree: false (not plausible).
	require.False(t, verifyGroupStillPlausible(pgid, realStartTime+"-does-not-match"))
}

// TestVerifyGroupStillPlausibleOutcome_LiveMatchIsConfirmedLive is the
// positive control: a live, still-leading pgid with a matching start time
// (when the platform can report one) is plausibilityConfirmedLive.
func TestVerifyGroupStillPlausibleOutcome_LiveMatchIsConfirmedLive(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	pgid := leader.Process.Pid

	startTime, _ := startTimeToken(pgid) // "" is fine -- covers the no-facility platform path too
	outcome := verifyGroupStillPlausibleOutcome(pgid, startTime)
	require.Equal(t, plausibilityConfirmedLive, outcome)
}

// TestVerifyGroupStillPlausibleOutcome_GoneIsConfirmedGone is the other
// positive control: a pgid that plausibly never existed is
// plausibilityConfirmedGone.
func TestVerifyGroupStillPlausibleOutcome_GoneIsConfirmedGone(t *testing.T) {
	const bogusPGID = 999999
	require.Equal(t, plausibilityConfirmedGone, verifyGroupStillPlausibleOutcome(bogusPGID, ""))
}

// TestKillRegisteredChildGroups_UnresolvedKillIsRetainedNotDiscarded is the
// core regression test for task #591 part 3's retention half, and the one
// this session's verification discipline specifically requires: driven
// through the real production call site (KillRegisteredChildGroups), not
// by asserting on the classification function alone. killpgFunc is
// substituted to return a non-nil, non-ESRCH error -- exactly the "neither
// confirmed killed nor confirmed already dead" shape the review's EPERM
// example describes -- for a registered entry that is otherwise completely
// legitimate: real pgid, real matching generation, real matching start
// time (verifyGroupStillPlausibleOutcome independently reports
// plausibilityConfirmedLive for it, proven directly before the sweep
// runs). The ONLY reason this entry is not killed is the injected kill(2)
// failure.
//
// Revert-check: change the `default:` branch (the one handling "any kill
// error other than ESRCH") in KillRegisteredChildGroups so it also counts
// toward result.Implausible instead of result.Retained, i.e. restore the
// old "any non-nil kill error is treated as confirmed-gone" behavior.
// Rerun this test: it FAILS, because the entry is now (wrongly) reported
// as Implausible instead of Retained, and the registry file is removed
// (nothing left to write back), so require.NoError(t, statErr) on the
// registry path fails with a not-exist error.
func TestKillRegisteredChildGroups_UnresolvedKillIsRetainedNotDiscarded(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-retain-on-unresolved-kill"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader, waited := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	// Confirm independently (outside the seam) that this entry would
	// otherwise be judged confirmedLive -- so the test result below can
	// only be explained by the injected kill(2) failure, not by the
	// plausibility check itself already excluding it.
	require.Equal(t, plausibilityConfirmedLive,
		verifyGroupStillPlausibleOutcome(leader.Process.Pid, startTimeTokenOrEmpty(leader.Process.Pid)))

	injectedErr := syscall.EPERM
	original := killpgFunc
	killpgFunc = func(pid int, sig syscall.Signal) error {
		return injectedErr
	}
	t.Cleanup(func() { killpgFunc = original })

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
	require.Zero(t, result.Killed, "the injected kill failure must not be reported as a successful kill")
	require.Zero(t, result.Implausible, "an unresolved kill error is not a confirmed-gone outcome")
	require.Equal(t, 1, result.Retained, "the entry must be reported as retained, not silently dropped")

	// The real regression: the registry file must still exist and still
	// contain the entry, so a SECOND sweep (a second `sessions kill`) has
	// something to retry against.
	_, statErr := os.Stat(childGroupRegistryPath(dataDir, sessionID))
	require.NoError(t, statErr, "the registry file must survive an unresolved kill attempt")

	// Restore the real kill function and let a second sweep actually
	// finish the job -- proving the retained entry was not just present
	// on disk but genuinely re-actionable.
	killpgFunc = original
	result2 := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
	require.Equal(t, 1, result2.Killed, "a second sweep with the real kill function must finish killing the retained entry")

	select {
	case <-waited:
	case <-t.Context().Done():
	}
}

// startTimeTokenOrEmpty is a small helper so the independent plausibility
// check above reads naturally regardless of whether this platform can
// produce a start-time token at all.
func startTimeTokenOrEmpty(pid int) string {
	s, _ := startTimeToken(pid)
	return s
}

// TestKillRegisteredChildGroups_MixedOutcomesRetainOnlyUnresolvedEntries
// covers three registered entries in ONE sweep with three different
// outcomes -- killed, confirmed-gone, and unresolved -- and asserts the
// registry file, read back afterward, contains ONLY the unresolved one.
// This is the scenario the review's "the rest should survive... still
// behind the same generation and start-time fence" language describes
// directly: a sweep is not all-or-nothing, and the write-back must reflect
// exactly the entries that still need a retry.
func TestKillRegisteredChildGroups_MixedOutcomesRetainOnlyUnresolvedEntries(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-mixed-outcomes"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	killableLeader, killableWaited := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, killableLeader.Process.Pid, generation)

	const bogusPGID = 888888
	RegisterChildGroup(dataDir, sessionID, bogusPGID, generation)

	unresolvedLeader, _ := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, unresolvedLeader.Process.Pid, generation)

	original := killpgFunc
	killpgFunc = func(pid int, sig syscall.Signal) error {
		if -pid == unresolvedLeader.Process.Pid {
			return syscall.EPERM
		}
		return original(pid, sig)
	}
	t.Cleanup(func() { killpgFunc = original })

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
	require.Equal(t, 1, result.Killed)
	require.Equal(t, 1, result.Implausible)
	require.Equal(t, 1, result.Retained)

	select {
	case <-killableWaited:
	case <-t.Context().Done():
	}

	remaining := readChildGroupFileLockedForTest(t, dataDir, sessionID)
	require.Len(t, remaining, 1, "only the unresolved entry must survive the sweep")
	require.Equal(t, unresolvedLeader.Process.Pid, remaining[0].PGID)
	require.Equal(t, generation, remaining[0].Generation)
}

// readChildGroupFileLockedForTest reads the registry file back through the
// package's own (locked) reader, for assertions that need to inspect
// exactly which entries survived a sweep.
func readChildGroupFileLockedForTest(t *testing.T, dataDir, sessionID string) []childGroupEntry {
	t.Helper()
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	defer childGroupFileMu.Unlock()
	return readChildGroupFileLocked(path)
}
