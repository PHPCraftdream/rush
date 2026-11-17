package sessionlock

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

func TryAcquireSessionLockWithTimeout(dataDir, sessionID string, timeoutSec int64) (*SessionLock, error) {
	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		return nil, err
	}
	if timeoutSec > 0 && lk.f != nil {
		// Append the timeout on the second line; reader handles missing line gracefully.
		_, _ = fmt.Fprintf(lk.f, "%d\n", timeoutSec)
		_ = lk.f.Sync()
		writePIDSidecar(lk.Path, lk.HolderPID, timeoutSec)
	}
	return lk, nil
}

func TryAcquireSessionLock(dataDir, sessionID string) (*SessionLock, error) {
	return TryAcquireSessionLockWithOptions(dataDir, sessionID)
}

// TryAcquireSessionLockWithOptions attempts to acquire an exclusive lock for the
// given sessionID under <dataDir>/locks/. Returns a *SessionLock on
// success (caller MUST Release()). Returns *SessionLockBusyError if
// another live process holds the lock. Other errors returned as-is.
// Accepts optional LockOption parameters for test configuration.
func TryAcquireSessionLockWithOptions(dataDir, sessionID string, opts ...LockOption) (*SessionLock, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("TryAcquireSessionLock: dataDir is empty")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("TryAcquireSessionLock: sessionID is empty")
	}
	locksDir := filepath.Join(dataDir, "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("TryAcquireSessionLock: create locks dir: %w", err)
	}
	path := filepath.Join(locksDir, "session-"+sanitiseSessionID(sessionID)+".lock")

	// logStaleDiagnostics is a best-effort, decision-free hint: it never
	// removes anything and never influences whether we succeed or fail
	// below. The ONLY thing that may ever decide "the previous holder is
	// gone" is actually winning the OS-level lock (flock/LockFileEx) on
	// this exact path. mtime stopped being trustworthy as a removal
	// trigger the day someone's GC pause or a transient Chtimes failure
	// let a live holder's heartbeat go stale while it still held the OS
	// lock — reclaiming on mtime alone in that case gives two "owners"
	// of the same session at once (see package doc).
	logStaleDiagnostics(path)

	lk, err := acquireSessionLockFileWithOptions(path, opts)
	if err == nil {
		return lk, nil
	}

	var busyErr *SessionLockBusyError
	if !errors.As(err, &busyErr) {
		return nil, err
	}
	return nil, busyErr
}

// SharedLockProbe is a SHARED, non-blocking OS lock held on a session's
// lock file by a destructive consumer (web-server rerun / orphan-rescue)
// for the duration of its history mutations. It exists because any
// encoding of "held" vs "released" in disk BYTES has an irreducible race
// — there is always an instant after a new holder wins tryLockFile and
// before its first write completes where the disk still shows whatever
// the previous release left, and symmetrically a marker whose removal is
// release's last act survives taskkill /F. The only thing that retracts
// atomically with BOTH process death AND lock acquisition is the kernel
// lock itself, so this is how the destructive path asks the kernel
// directly (task #631 redesign, following @fh's design review):
// winning a SHARED lock is proof no exclusive holder exists at that
// instant, and — because shared and exclusive range locks conflict — no
// exclusive holder can APPEAR while the probe is held, on any platform.
//
// Deliberately invisible to display consumers (InspectSessionLock,
// annotateExternalOwnership): no writes, no truncate, no sidecars, no
// heartbeat, no Chtimes. A probe in flight may make the lock file
// temporarily unreadable on Windows (mandatory range lock), which display
// already tolerates as PID 0 — fail-open there, as designed.
type SharedLockProbe struct {
	f *os.File
}

// TryHoldSessionLockShared takes a non-blocking SHARED OS lock on
// sessionID's lock file under <dataDir>/locks/. Success is kernel-
// attested proof that no exclusive holder exists at this instant — and,
// while the returned probe is held, that none can appear. Contention
// (a live exclusive holder on any handle, in any process, including this
// one) returns *SessionLockBusyError. Any other error is returned as-is;
// destructive callers must treat "could not probe" as refuse, not allow.
//
// The file is opened O_CREATE: if no lock file exists yet, the probe
// CREATES it and pins that inode with the shared lock, so an external
// acquirer arriving during the hold opens the same inode and fails
// against the probe — closing the "owner appears during the delete"
// window that a stat-then-lock two-step would leave open. The empty file
// this leaves behind after Release is inert (no holder, no sidecar;
// acquire-side writePIDSidecar-first ordering keeps byte-heuristic
// readers from misreading it as anything else).
//
// Release is nil-safe, matching SessionLock.Release's convention, so
// callers can defer unconditionally.
func TryHoldSessionLockShared(dataDir, sessionID string) (*SharedLockProbe, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("TryHoldSessionLockShared: dataDir is empty")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("TryHoldSessionLockShared: sessionID is empty")
	}
	locksDir := filepath.Join(dataDir, "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("TryHoldSessionLockShared: create locks dir: %w", err)
	}
	path := SessionLockPath(dataDir, sessionID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("TryHoldSessionLockShared: open lock file: %w", err)
	}
	if err := tryLockFileShared(f); err != nil {
		holderPID := readLockHolderPID(path)
		f.Close()
		if isLockContentionError(err) {
			return nil, &SessionLockBusyError{Path: path, HolderPID: holderPID}
		}
		// Not contention — permission, IO, filesystems where range locks
		// don't work (some NFS/SMB). Surface as-is; callers fail closed.
		return nil, fmt.Errorf("TryHoldSessionLockShared: lock file %s: %w", path, err)
	}
	return &SharedLockProbe{f: f}, nil
}

// Release drops the shared lock and closes the handle. Nil-safe and
// idempotent-enough (a second call closes an already-closed file, which
// returns an error that is deliberately ignored — callers use it once).
func (p *SharedLockProbe) Release() error {
	if p == nil {
		return nil
	}
	_ = unlockFile(p.f)
	return p.f.Close()
}

// acquireMidStampSeam is a test-only hook (task #631 follow-up): fires in
// acquireSessionLockFileWithOptions after the OS lock is won and the .pid
// sidecar is stamped, but while the primary lock file is still EMPTY (our
// PID not yet written into it) — the exact on-disk state the fourth
// ownership state (held, but byte-for-byte a "released leftover") used to
// be observable in. Nil in every production path; receives the lock path so
// a test can scope itself to its own acquire. See the ordering comment
// on the writePIDSidecar call in acquireSessionLockFileWithOptions.
var acquireMidStampSeam func(path string)

// acquireSessionLockFileWithOptions is like acquireSessionLockFile but accepts
// LockOption parameters for test configuration. It opens the lock file at
// path and attempts to take the OS-level advisory lock on THAT SAME
// file/inode/handle. There is no "remove the file, make a new one"
// fallback anywhere in this path: unlinking a path out from under an
// OS lock does not release the lock (flock is bound to the inode, not
// the path) — it just lets a second process create a fresh inode at
// the same path and believe it owns the session while the original
// holder, if still alive, keeps running against the old inode. Two
// "owners" of one session id is exactly the bug this file exists to
// prevent.
//
// If tryLockFile succeeds here, that is authoritative proof the
// previous holder is gone at the OS level — the kernel releases flock/
// LockFileEx automatically when the holding process dies or closes the
// descriptor, regardless of what its lock file's mtime says. Only then
// do we truncate and stamp our own PID into the file and start our own
// heartbeat.
//
// If tryLockFile reports contention, that is likewise authoritative:
// somebody else holds the OS lock right now, full stop. No mtime
// threshold, no PID liveness probe, nothing overrides that — we return
// SessionLockBusyError unconditionally.
func acquireSessionLockFileWithOptions(path string, opts []LockOption) (*SessionLock, error) {
	// If our own process's most recent Release() of this exact path
	// handed the OS unlock off to its heartbeat finalizer and hasn't
	// completed it yet, wait for it here — bounded — before attempting
	// the real OS lock. Otherwise a same-process re-acquire arriving in
	// that window sees our own outgoing lock as a foreign busy holder
	// and fails outright (task #1031: agent.Run resuming a sub-agent
	// gets "session already in use" with no retry). Waiting is the only
	// way this acquire can succeed at all — our own handle still holds
	// the OS lock until the handoff's unlock actually runs. If the bound
	// elapses, fall through to the normal attempt and its normal busy
	// error; never block indefinitely on our own past release.
	waitForPendingHandoff(path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if busyErr, ok := busyIfPermissionDeniedOpenHasActiveLock(path, err); ok {
			return nil, busyErr
		}
		// Not a live exclusive holder (busyIfPermissionDeniedOpenHasActive
		// found no lock contention on the read-only probe). For a permission
		// failure specifically that is usually mundane — a stale read-only
		// attribute or an ACL denying write, with NO active holder — and is
		// emphatically not "busy". Say so while keeping the ErrPermission
		// chain intact via %w, so callers can still tell it apart from a
		// live holder without an errors.As busy match.
		if os.IsPermission(err) {
			return nil, fmt.Errorf("TryAcquireSessionLock: open lock file %s: lock file not writable (check ACL/read-only attribute); no active holder: %w", path, err)
		}
		return nil, fmt.Errorf("TryAcquireSessionLock: open lock file: %w", err)
	}
	if err := tryLockFile(f); err != nil {
		holderPID := readLockHolderPID(path)
		f.Close()
		if isLockContentionError(err) {
			return nil, &SessionLockBusyError{Path: path, HolderPID: holderPID}
		}
		// Genuinely unidentified failure (permission denied, IO error,
		// etc — not "someone else holds it"). Surface it as-is so the
		// caller can tell the difference between "busy" and "broken";
		// see agent.Run's handling, which now treats anything that
		// isn't a *SessionLockBusyError as fatal rather than silently
		// running without lock protection.
		return nil, fmt.Errorf("TryAcquireSessionLock: lock file %s: %w", path, err)
	}

	myPID := os.Getpid()
	generation := fmt.Sprintf("%d-%d", myPID, time.Now().UnixNano())
	// Task #631 follow-up: the .pid sidecar is stamped BEFORE the primary
	// lock file is emptied. The OS lock is already held at this point, but
	// byte-heuristic readers (display's InspectSessionLock consumers,
	// `sessions why`/`sessions list`) see only on-disk state: with the old
	// order (truncate, stamp, THEN sidecar) there was a window — several
	// syscalls wide, including an f.Sync — where the primary was EMPTY and
	// no sidecar existed, byte-for-byte the "released leftover" shape those
	// readers classify as unowned. The DESTRUCTIVE path no longer depends
	// on this (it now holds a shared kernel probe, TryHoldSessionLockShared,
	// which conflicts with the exclusive lock already won here and so
	// cannot be fooled by bytes at all) — this ordering is kept as
	// defense-in-depth for the byte-heuristic consumers that remain.
	// Writing the sidecar first means every state after tryLockFile
	// succeeds that a byte-reader can observe carries either our sidecar
	// (PID readable) or the previous holder's untouched content — never
	// "empty and sidecar-less". Residual window, accepted: if this
	// best-effort sidecar write FAILS (writePIDSidecar logs and swallows),
	// the truncate below re-creates the empty-and-sidecarless shape until
	// our PID lands in the primary a few syscalls later; on Windows the
	// primary is independently unreadable-while-held, and the destructive
	// path is unaffected on every platform.
	writePIDSidecar(path, myPID, 0)
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	// acquireMidStampSeam fires mid-acquire — OS lock genuinely held,
	// sidecar stamped, primary emptied but our PID not yet written into
	// it — the worst observable point of the acquire sequence. Nil in
	// every production path; task #631's window test proves here that the
	// shared probe (not the bytes) is the source of truth: it must report
	// busy at this instant.
	if acquireMidStampSeam != nil {
		acquireMidStampSeam(path)
	}
	_, _ = fmt.Fprintf(f, "%d\n", myPID)
	_ = f.Sync()
	writeGenerationSidecar(path, generation)

	// Touch the file now so mtime is fresh from the start. mtime is a
	// diagnostic aid only (surfaced via InspectSessionLock / `sessions
	// locks`) — see the package doc on SessionLock. It is never used to
	// decide whether a lock may be reclaimed.
	now := time.Now()
	if err := touchLockFile(f, now); err != nil {
		slog.Warn("session lock: failed to set initial heartbeat mtime",
			"path", path, "err", err)
	}

	stop := make(chan struct{})
	heartbeatExited := make(chan struct{})
	lk := &SessionLock{
		Path:                  path,
		HolderPID:             myPID,
		generation:            generation,
		f:                     f,
		stop:                  stop,
		heartbeatExited:       heartbeatExited,
		handoffDone:           make(chan struct{}),
		clearHolderMetadataFn: clearHolderMetadata, // Default implementation
	}
	// Apply any lock options (e.g., test injection of blocking cleanup)
	for _, opt := range opts {
		opt(lk)
	}
	if lk.heartbeatInterval <= 0 {
		lk.heartbeatInterval = lockHeartbeatInterval
	}
	go heartbeat(f, path, stop, &lk.active, lk.heartbeatInterval, heartbeatExited, &lk.closeState, lk.handoffDone, lk.heartbeatExitSeam)

	return lk, nil
}

// Release stops the heartbeat, unlocks and closes the lock file.
// Safe to call on nil. Idempotent and concurrency-safe.
//
// P0 fix (2026-08-09): The order of operations is now:
//  1. Stop the heartbeat goroutine (close(l.stop)).
//  2. Wait, bounded by heartbeatHandoffBound, for the heartbeat goroutine to
//     exit. In the overwhelmingly common case it exits promptly (a healthy
//     local disk completes a touch in microseconds) and Release proceeds to
//     unlock/close itself. If it is still blocked inside a heartbeat touch
//     syscall on the file handle (slow/hung SetFileTime or Chtimes on
//     AV/SMB) at the bound, Release hands unlock+close to the heartbeat's
//     finalizer and returns — "unlock in progress", never lost, and never
//     ownerless: a single atomic CompareAndSwap (closeState), raced against
//     the identical CAS in the heartbeat finalizer's defer, decides exactly
//     once who closes (see the CAS argument in the function body and on
//     the closeState field). A same-process re-acquire of this same path
//     that arrives before that pending close finishes waits for it instead
//     of bouncing off the still-held OS lock (see waitForPendingHandoff).
//  3. Unlock the OS-level lock (unlockFile) - this is the critical correctness-critical step.
//  4. Close the file descriptor (f.Close).
//  5. Clear diagnostic metadata (clearHolderMetadata) - best-effort cleanup that can hang.
//     IMPORTANT: This now runs in a BACKGROUND goroutine. The function returns
//     immediately after unlock/close, ensuring that callers (mailbox state machine)
//     are not blocked by potentially-infinite filesystem I/O.
//
// The old order (clear metadata BEFORE unlock/close) meant that a hung filesystem,
// AV scan, or SMB share during clearHolderMetadata (Truncate/Seek/Sync/Remove) would
// prevent the OS lock from ever being released, wedging the session forever.
//
// Even after the P1-2 reordering (unlock/close first, THEN cleanup), Release() still
// waited synchronously for cleanup to complete before returning. This meant that
// if cleanup hung, the mailbox state machine would be stuck in mbReleasing state
// forever - no new owner could take over, even though the OS lock was already free.
//
// The new order prioritizes releasing the OS lock AND closing the file descriptor AND
// returning control to the caller BEFORE any diagnostic cleanup that can hang. If
// clearHolderMetadata hangs in the background, the OS lock is already gone, the file
// descriptor is already closed, and the mailbox has already transitioned to mbIdle -
// no session wedged state, no resource leak. The only downside is that clearHolderMetadata
// must now operate on a closed file, so we pass only the path (not the open file handle)
// and let it reopen the file just for the metadata operations.
//
// Before actually unlocking, it wipes the PID it stamped into both the
// primary lock file and the sidecar (see clearHolderMetadata). This
// matters for `rush sessions kill`: without it, a process that exits
// cleanly (Release() runs, e.g. via a normal `defer`) leaves its old PID
// sitting in the lock file/sidecar on disk. If the OS later reuses that
// PID number for a completely unrelated process — routine on a busy
// CI/dev box — a later `sessions kill <id>` invocation would read a
// "plausible" PID from the stale file and forcibly kill that unrelated
// process. The lock FILE itself is deliberately left in place — see
// acquireSessionLockFile and the package doc for why unlinking the path
// is unsafe/unnecessary; only its content is cleared.
//
// Data race safety: The background goroutine captures `path` by value, so there's
// no race on `l.Path` even if `l` is garbage-collected after Release() returns.
// The sync.Once ensures the release body (including goroutine spawn) runs at most once.
