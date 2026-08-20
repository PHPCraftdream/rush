package session

// Task #631, redesigned round: the destructive external-ownership proof is
// no longer byte-heuristic (mtime/PID/Released inference) but a
// kernel-attested SHARED lock probe, TryHoldSessionLockShared. These tests
// exercise the primitive against REAL OS-level exclusive holders — the
// thing state-4 byte forgery could never do — plus the acquire-window
// (state 4) deterministic observation via acquireMidStampSeam.
//
// History: rounds 1-2 of #631 added a Released field to LockState and a
// releasedLeftover byte classifier at the InspectSessionLock seam. The
// redesign removed both — the destructive path asks the kernel instead —
// but KEPT the writePIDSidecar-before-Truncate ordering in
// acquireSessionLockFileWithOptions as defense-in-depth for the remaining
// byte-heuristic consumers (display, `sessions why`/`list`).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTryHoldSessionLockShared_ExclusiveHeldReportsBusy is the core
// primitive proof: with a REAL exclusive lock held on the session's lock
// file (acquired exactly as production acquires it,
// TryAcquireSessionLock), a shared probe from a different handle in this
// same process must report contention. flock/LockFileEx conflict between
// separate open file descriptions/handles even within one process, so
// this exercises the kernel mechanism without needing a second process.
//
// Revert-check: make TryHoldSessionLockShared skip tryLockFileShared
// (return a probe without probing) and this test fails at
// "expected *session.SessionLockBusyError".
func TestTryHoldSessionLockShared_ExclusiveHeldReportsBusy(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	lk, err := TryAcquireSessionLock(dataDir, "s-exclusive")
	require.NoError(t, err)
	defer func() { require.NoError(t, lk.Release()) }()

	probe, err := TryHoldSessionLockShared(dataDir, "s-exclusive")
	require.Error(t, err, "a genuinely held exclusive lock must deny the shared probe")
	var busyErr *SessionLockBusyError
	require.ErrorAs(t, err, &busyErr,
		"contention must surface as *SessionLockBusyError, not a generic error")
	require.Nil(t, probe, "no probe may be returned on contention")

	// After the exclusive holder releases, the probe must grant — proving
	// the busy above was the live holder, not a wedged file.
	require.NoError(t, lk.Release())
	probe, err = TryHoldSessionLockShared(dataDir, "s-exclusive")
	require.NoError(t, err, "probe must grant once the exclusive holder is gone")
	require.NoError(t, probe.Release())
}

// TestTryHoldSessionLockShared_ReleasedLeftoverGrants is the #631 symptom,
// proven via the kernel this time: after a real acquire and Release, the
// leftover empty lock file must grant the shared probe IMMEDIATELY — no
// 20s window where a cancel-then-rerun bounces with "owned by another
// process". No Eventually needed: the probe never consults bytes or
// mtime, so there is nothing to wait for. (Release itself waits up to
// releaseMetadataCleanupBound for background metadata cleanup, which does
// not affect the probe.)
//
// Revert-check: same as the exclusive test — skip the probe and this
// still fails trivially; the meaningful one is that a heuristic restored
// IN PLACE of the probe would need the leftover to be old before
// allowing, which this test pins by requiring the grant immediately.
func TestTryHoldSessionLockShared_ReleasedLeftoverGrants(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	lk, err := TryAcquireSessionLock(dataDir, "s-released")
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	probe, err := TryHoldSessionLockShared(dataDir, "s-released")
	require.NoError(t, err,
		"a released leftover (empty file, fresh truncate mtime) must grant the probe immediately")
	require.NoError(t, probe.Release())
}

// TestTryHoldSessionLockShared_MidAcquireWindowDenies asserts state 4
// deterministically: while a lock is genuinely held but the on-disk bytes
// are still mid-stamp (primary truncated to empty, PID not yet written),
// the shared probe must report busy — proving the KERNEL, not the bytes,
// is the source of truth. This is exactly the window the byte-heuristic
// rounds could only narrow, never close.
//
// Uses acquireMidStampSeam (nil in production, path-scoped) to pause the
// acquire at the worst observable point; the probe runs from the test
// goroutine. No sleeps, no racing.
//
// Revert-check: make TryHoldSessionLockShared skip tryLockFileShared and
// this test fails at "expected an error" (the probe would grant mid-held).
func TestTryHoldSessionLockShared_MidAcquireWindowDenies(t *testing.T) {
	// Deliberately NOT t.Parallel(): acquireMidStampSeam is a
	// package-level var that fires for EVERY acquire while set — see the
	// seam's own comment. Sequential execution plus the path filter below
	// keep the seam scoped to this test's own acquire.
	dataDir := t.TempDir()
	path := SessionLockPath(dataDir, "s-window")

	entered := make(chan struct{})
	proceed := make(chan struct{})
	acquireMidStampSeam = func(seenPath string) {
		if seenPath != path {
			return
		}
		close(entered)
		select {
		case <-proceed:
		case <-time.After(10 * time.Second):
			// Fail fast instead of wedging the acquire goroutine forever.
			panic("acquireMidStampSeam: proceed channel never closed")
		}
	}
	t.Cleanup(func() { acquireMidStampSeam = nil })

	type result struct {
		lk  *SessionLock
		err error
	}
	done := make(chan result, 1)
	go func() {
		lk, err := TryAcquireSessionLock(dataDir, "s-window")
		done <- result{lk, err}
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("acquire never reached the mid-stamp seam")
	}

	// Defense-in-depth still holds (kept from round 2 for the byte-heuristic
	// consumers): our sidecar is stamped before the primary was emptied.
	_, statErr := os.Stat(pidSidecarPath(path))
	require.NoError(t, statErr,
		"the .pid sidecar must be stamped BEFORE the primary is emptied")

	// The decisive assert: the kernel denies even though the bytes are at
	// their most ambiguous.
	_, err := TryHoldSessionLockShared(dataDir, "s-window")
	require.Error(t, err,
		"mid-acquire, with bytes at their most ambiguous, the kernel must still deny")
	var busyErr *SessionLockBusyError
	require.ErrorAs(t, err, &busyErr)

	close(proceed)
	res := <-done
	require.NoError(t, res.err)
	require.NoError(t, res.lk.Release())
}

// TestTryHoldSessionLockShared_NoLockFileGrantsAndCreates pins the
// no-lock-file case: the probe grants AND creates the file, pinning the
// inode so a subsequent exclusive acquirer opens the same inode (and a
// concurrent external acquirer during a held probe would fail against the
// probe's shared lock). After the probe releases, exclusive acquisition
// must work normally — proving the probe leaves nothing wedged.
//
// Revert-check: drop the O_CREATE from TryHoldSessionLockShared's OpenFile
// and the "probe created the lock file" assert fails on the stat.
func TestTryHoldSessionLockShared_NoLockFileGrantsAndCreates(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	path := SessionLockPath(dataDir, "s-absent")

	probe, err := TryHoldSessionLockShared(dataDir, "s-absent")
	require.NoError(t, err, "no lock file means no owner — the probe must grant")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the probe must create (and so inode-pin) the lock file")
	require.NoError(t, probe.Release())

	// The probe's leftover empty file must not wedge the next real acquire.
	lk, err := TryAcquireSessionLock(dataDir, "s-absent")
	require.NoError(t, err, "a probe-released lock file must be acquirable")
	require.NoError(t, lk.Release())
}

// TestTryHoldSessionLockShared_SharedProbesCoexist pins that the probe is
// genuinely SHARED: many probes may hold simultaneously (they conflict
// only with EXCLUSIVE holders), so concurrent destructive handlers on
// different sessions — or repeated probes — never deadlock each other.
func TestTryHoldSessionLockShared_SharedProbesCoexist(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	p1, err := TryHoldSessionLockShared(dataDir, "s-shared")
	require.NoError(t, err)
	defer p1.Release()

	// A second probe on the SAME session must also grant — shared locks
	// coexist with each other.
	p2, err := TryHoldSessionLockShared(dataDir, "s-shared")
	require.NoError(t, err, "shared probes must coexist with each other")
	require.NoError(t, p2.Release())

	// But an exclusive acquire while a probe is held must fail — this is
	// the mechanism that keeps external writers out during a delete.
	_, err = TryAcquireSessionLock(dataDir, "s-shared")
	require.Error(t, err, "an exclusive acquire must fail while a shared probe is held")
	var busyErr *SessionLockBusyError
	require.ErrorAs(t, err, &busyErr)
}

// TestHeldLockPrimaryReadability_ExercisesRealOSLock is a platform reality
// pin, not a code-path test: it acquires a REAL SessionLock in this process
// and observes what a plain os.ReadFile of the primary sees while the OS
// lock is genuinely held. This exercises the platform assumption the old
// byte-heuristic leaned on — on Windows, LockFileEx is a MANDATORY range
// lock, so the primary is unreadable-while-held; on POSIX, flock is
// advisory and the primary reads fine (which is why the byte heuristic was
// racy on POSIX and the kernel probe was needed). No revert-check
// possible: it asserts operating-system behavior, not ours.
func TestHeldLockPrimaryReadability_ExercisesRealOSLock(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	lk, err := TryAcquireSessionLock(dataDir, "s-held-real")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lk.Release()) })

	path := SessionLockPath(dataDir, "s-held-real")
	bts, readErr := os.ReadFile(path)
	if runtime.GOOS == "windows" {
		// Real mandatory LockFileEx in action: a second handle (even in
		// the same process) cannot read the locked byte range.
		require.Error(t, readErr,
			"Windows LockFileEx must make the held primary unreadable")
	} else {
		require.NoError(t, readErr,
			"POSIX flock is advisory — the held primary must be readable")
		require.Equal(t, fmt.Sprintf("%d\n", os.Getpid()), string(bts))
	}
}

// writeRawLockFile is retained for byte-forgery negative controls where a
// test wants an inert (never OS-locked) lock file with arbitrary content.
func writeRawLockFile(t *testing.T, dataDir, sessionID, content string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := SessionLockPath(dataDir, sessionID)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(path, now, now))
	return path
}

// TestTryHoldSessionLockShared_InertBytesNeverMatter pins that forged
// bytes — a lock file that LOOKS owned (foreign PID content, garbage
// content) but has no OS lock behind it — grant the probe. This is the
// correctness inversion the redesign accepts: bytes alone never deny;
// only a real kernel holder does. It is what makes the old
// forged-foreign-PID server tests obsolete as security proofs.
func TestTryHoldSessionLockShared_InertBytesNeverMatter(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	writeRawLockFile(t, dataDir, "s-foreign-bytes", "999999\n")
	writeRawLockFile(t, dataDir, "s-garbage-bytes", "not-a-pid\n")
	writeRawLockFile(t, dataDir, "s-empty-bytes", "")

	for _, id := range []string{"s-foreign-bytes", "s-garbage-bytes", "s-empty-bytes"} {
		probe, err := TryHoldSessionLockShared(dataDir, id)
		require.NoError(t, err, "%s: inert bytes without a kernel holder must never deny", id)
		require.NoError(t, probe.Release())
	}
}
