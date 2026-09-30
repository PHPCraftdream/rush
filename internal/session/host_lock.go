// Host liveness for the phase-4 durable async core
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.6): a host
// (a process executing async jobs) is alive iff it holds the exclusive OS
// lock on its own <dataDir>/hosts/<host_id>.lock file. No clock is ever
// consulted -- "dead" means either the lock was just won by someone else,
// or the file is simply gone (ENOENT). Any other outcome is "unknown", and
// an unknown host's rows are never treated as reapable.
//
// This file only implements the primitive (registration, probe, the two
// deletion paths); AsyncJobStore.ensureHost wires it lazily into the first
// Claim of a job or ClaimSessionDriver. async_hosts rows stay display-only
// bookkeeping throughout -- no FK from async_jobs, no timestamp used for
// liveness.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

// HostLockStatus is what ProbeHostLock/ProbeHost reduce every OS outcome
// to. Doc sec.3.6 is explicit that this is a 3-way, not a 2-way, verdict:
// "unknown" is a real, distinct outcome from "dead", not an error case
// folded into either "alive" or "dead".
type HostLockStatus int

const (
	// HostStatusUnknown means the probe could not determine liveness
	// (permission error, IO error, a filesystem without lock support, ...).
	// Callers MUST NOT treat rows owned by this host as reapable.
	HostStatusUnknown HostLockStatus = iota
	// HostStatusAlive means another process currently holds the exclusive
	// lock (EWOULDBLOCK/EAGAIN on POSIX, ERROR_LOCK_VIOLATION/
	// ERROR_SHARING_VIOLATION on Windows -- isLockContentionError).
	HostStatusAlive
	// HostStatusDead means either the lock file does not exist (ENOENT) or
	// this probe itself just won the exclusive lock. In the latter case
	// ProbeHostLock/ProbeHost return the won *FileLock; the caller now
	// legitimately owns it and is responsible for Release()/deletion.
	HostStatusDead
)

func (s HostLockStatus) String() string {
	switch s {
	case HostStatusAlive:
		return "alive"
	case HostStatusDead:
		return "dead"
	default:
		return "unknown"
	}
}

// HostsDir returns <dataDir>/hosts, the directory holding one lock file per
// registered host.
func HostsDir(dataDir string) string {
	return filepath.Join(dataDir, "hosts")
}

// HostLockPath returns the lock file path for hostID under dataDir.
func HostLockPath(dataDir, hostID string) string {
	return filepath.Join(HostsDir(dataDir), hostID+".lock")
}

// ownHostMu/ownHostIDs remembers every host id THIS OS PROCESS has ever
// registered, across every App instance created in it (tests routinely
// instantiate more than one App in-process against the same or different
// data dirs). Doc sec.3.6 requires never probing this process's own host id
// or a sibling App's in the same process, as a flat rule -- not because the
// OS lock primitives get the answer wrong here: flock is scoped per open
// file description and LockFileEx per handle, so a second open+lock from
// this same process on its own already-held lock file correctly contends
// (reports "alive"), the same as a genuinely different process would see.
// The guard exists so that if a caller ever DOES pass its own id (a bug --
// e.g. an id slipping into a recovery candidate set it should have been
// filtered out of), that is a loud, explicit ErrProbeOwnHost instead of a
// silent extra lock/contention cycle against a file this process already
// owns.
var (
	ownHostMu  sync.Mutex
	ownHostIDs = map[string]struct{}{}
)

// retainedHostLocks pins OS host locks whose owner store was closed on the
// forced-shutdown path (AsyncJobStore.CloseKeepLock). An *os.File is closed by
// its finalizer once unreachable, which would silently release the lock at the
// next GC; the lock must live until the process exits.
var (
	retainedHostMu    sync.Mutex
	retainedHostLocks []*FileLock
)

func retainHostLockUntilExit(l *FileLock) {
	if l == nil {
		return
	}
	retainedHostMu.Lock()
	defer retainedHostMu.Unlock()
	retainedHostLocks = append(retainedHostLocks, l)
}

// releaseRetainedHostLocksForTest releases every pinned lock (tests only).
func releaseRetainedHostLocksForTest() {
	retainedHostMu.Lock()
	defer retainedHostMu.Unlock()
	for _, l := range retainedHostLocks {
		_ = l.Release()
	}
	retainedHostLocks = nil
}

func markOwnHostID(id string) {
	ownHostMu.Lock()
	defer ownHostMu.Unlock()
	ownHostIDs[id] = struct{}{}
}

func unmarkOwnHostID(id string) {
	ownHostMu.Lock()
	defer ownHostMu.Unlock()
	delete(ownHostIDs, id)
}

// IsOwnHostID reports whether hostID was registered by this OS process
// (this App instance or a sibling one created in the same process).
func IsOwnHostID(hostID string) bool {
	ownHostMu.Lock()
	defer ownHostMu.Unlock()
	_, ok := ownHostIDs[hostID]
	return ok
}

// ErrProbeOwnHost is returned by ProbeHost when asked to probe a host id
// this process itself owns. Doc sec.3.6 forbids self-probes outright;
// callers building a recovery/liveness candidate set are expected to
// exclude their own ids themselves. This guard turns a caller bug that
// fails to do so into an explicit, loud error instead of letting it
// silently reach the OS lock (which would correctly report "alive" here,
// not "dead" -- see the ownHostIDs comment above -- but doc sec.3.6 still
// wants the attempt itself refused, not merely harmless).
var ErrProbeOwnHost = errors.New("host lock: refusing to probe this process's own host id")

// ProbeHostLock answers "is the process behind lockPath alive?" without
// creating the file (doc sec.3.6). It does not know or care whose id this
// is -- callers that must not probe their own id should use ProbeHost
// instead, which enforces that via the registered-own-ids set.
//
//   - HostStatusDead + non-nil *FileLock: the lock was WON. The caller now
//     legitimately owns it and MUST eventually Release() it (typically via
//     RemoveDeadHostFile, which also deletes the file after verifying its
//     identity). Dropping this lock silently re-opens the exact race the
//     probe exists to close.
//   - HostStatusDead + nil lock: the file does not exist (ENOENT). Nothing
//     to release, nothing to delete.
//   - HostStatusAlive: another process holds the lock right now. No lock
//     returned, no error -- this is an ordinary, expected outcome, not a
//     failure.
//   - HostStatusUnknown: some other error. No lock returned; err carries
//     the cause. Rows owned by this host must NOT be treated as reapable.
func ProbeHostLock(lockPath string) (HostLockStatus, *FileLock, error) {
	// A14: opened read-only -- flock/LockFileEx work on a read-only handle,
	// and a 0644 lock file in a shared data dir (owned by another uid/process)
	// would otherwise give EACCES on O_RDWR and report HostStatusUnknown
	// forever, never recovering that host's rows.
	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostStatusDead, nil, nil
		}
		return HostStatusUnknown, nil, fmt.Errorf("host lock probe: open %s: %w", lockPath, err)
	}
	// O_RDONLY opens a directory successfully on POSIX (unlike O_RDWR, which
	// EISDIRs immediately) -- and flock(2) does not refuse a directory fd
	// either, so without this guard a directory sitting at lockPath would
	// silently report "dead, lock won" instead of the "something is wrong
	// here" HostStatusUnknown a corrupt/unexpected path must produce.
	if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
		_ = f.Close()
		return HostStatusUnknown, nil, fmt.Errorf("host lock probe: %s is a directory", lockPath)
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		if isLockContentionError(err) {
			return HostStatusAlive, nil, nil
		}
		return HostStatusUnknown, nil, fmt.Errorf("host lock probe: lock %s: %w", lockPath, err)
	}
	return HostStatusDead, &FileLock{Path: lockPath, f: f}, nil
}

// ProbeHost is ProbeHostLock scoped to dataDir/hostID, with the doc sec.3.6
// self-probe guard: refuses (HostStatusUnknown, ErrProbeOwnHost) for any
// host id this process itself registered.
func ProbeHost(dataDir, hostID string) (HostLockStatus, *FileLock, error) {
	if IsOwnHostID(hostID) {
		return HostStatusUnknown, nil, ErrProbeOwnHost
	}
	return ProbeHostLock(HostLockPath(dataDir, hostID))
}

// AsyncHostStore is the DB surface the host-lock module needs. Satisfied
// structurally by *db.Queries (including one bound to a *sql.Tx via
// db.New), without this package importing anything beyond the generated
// param/row types.
type AsyncHostStore interface {
	RegisterAsyncHost(ctx context.Context, arg db.RegisterAsyncHostParams) (db.AsyncHost, error)
	DeleteAsyncHostIfNoJobs(ctx context.Context, id string) (int64, error)
}

// ProbeHostLockShared answers "is the process behind lockPath alive?" via
// the SHARED, non-acquiring probe (doc sec.5 step 7 / sec.3.8's "readers
// between processes"): unlike ProbeHostLock, it never takes the EXCLUSIVE
// lock itself, so a reader can never win the lock a live recoverer is
// legitimately about to hold, nor make a live host look dead to itself or
// anyone else. Winning the SHARED lock is kernel-attested proof no
// exclusive holder exists right now (dead); contention
// (EWOULDBLOCK/EAGAIN on POSIX, ERROR_LOCK_VIOLATION/ERROR_SHARING_VIOLATION
// on Windows -- isLockContentionError) means alive; anything else is
// unknown. The probe does not create the file, and releases its own shared
// lock before returning in every case -- callers never receive a lock to
// manage, and a genuine holder is never disturbed.
func ProbeHostLockShared(lockPath string) (HostLockStatus, error) {
	// A14: same read-only rationale as ProbeHostLock.
	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostStatusDead, nil
		}
		return HostStatusUnknown, fmt.Errorf("host lock probe (shared): open %s: %w", lockPath, err)
	}
	defer f.Close()
	// See ProbeHostLock's identical guard for why this check matters now
	// that both probes open read-only.
	if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
		return HostStatusUnknown, fmt.Errorf("host lock probe (shared): %s is a directory", lockPath)
	}
	if err := tryLockFileShared(f); err != nil {
		if isLockContentionError(err) {
			return HostStatusAlive, nil
		}
		return HostStatusUnknown, fmt.Errorf("host lock probe (shared): lock %s: %w", lockPath, err)
	}
	// Won the shared lock: no exclusive holder exists right now. Release
	// immediately (doc: "release the shared lock right after the probe") --
	// this probe never holds anything past its own return.
	if err := unlockFile(f); err != nil {
		return HostStatusUnknown, fmt.Errorf("host lock probe (shared): unlock %s: %w", lockPath, err)
	}
	return HostStatusDead, nil
}

// ProbeHostShared is ProbeHostLockShared scoped to dataDir/hostID, with the
// same doc sec.3.6 self-probe guard as ProbeHost: this process's own host
// ids are alive by definition and are never probed.
func ProbeHostShared(dataDir, hostID string) (HostLockStatus, error) {
	if IsOwnHostID(hostID) {
		return HostStatusUnknown, ErrProbeOwnHost
	}
	return ProbeHostLockShared(HostLockPath(dataDir, hostID))
}

// HostIdentity is this process's (or, in-process, this App instance's)
// lazily-registered async host: a random id, a display-only async_hosts
// row, and a live OS lock on <dataDir>/hosts/<id>.lock held for as long as
// this HostIdentity exists. h.lock is a strong reference by design --
// nothing finalizes or garbage-collects it; only Close releases it.
type HostIdentity struct {
	ID      string
	DataDir string

	lock *FileLock
}

// ErrHostLockUnavailable is RegisterHost's typed refusal for the ONE failure
// class that means "this data dir cannot host an OS lock file": the lock file
// cannot be created or opened (missing/unwritable hosts dir, a file where the
// dir must be) or the lock call fails for a reason other than contention (a
// filesystem without lock support). Database and context errors, and an
// exhausted retry against active peers, are NOT this: they are plain errors
// the caller must surface (R3A-1).
var ErrHostLockUnavailable = errors.New("host lock unavailable")

// registerHostAttempts bounds RegisterHost's retry loop: each retry follows
// a lock file a reaper or prober touched between its creation and our lock
// (see acquireFreshHostLock), which needs a peer active at that instant.
const registerHostAttempts = 16

// errHostLockReplaced means the lock file this attempt created was unlinked
// (or replaced) by a reaper before the attempt held the lock on it.
var errHostLockReplaced = errors.New("host lock: lock file was removed before it was locked")

// registerHostAfterCreateSeam is a test-only hook fired between creating a
// host lock file and locking it -- the window a reaper can win. nil in
// production.
var registerHostAfterCreateSeam func(lockPath string)

// acquireFreshHostLock creates lockPath, locks it exclusively and proves the
// path still names the locked file. Creating and locking are two steps, so a
// reaper (purgeOrphanHostLockFiles/purgeEmptyDeadHostFiles: "a file nobody
// holds") can win the lock on the new file first and unlink it; the caller
// would then hold a lock on a file no probe can find (every probe of the id
// sees ENOENT = dead). Two halves close it: after locking, the path is
// re-checked against the held handle (errHostLockReplaced otherwise), and
// every remover unlinks only while holding the lock (RemoveDeadHostFile), so
// a file held-and-verified here can no longer be unlinked. Losing the lock
// race itself is *ErrLockContended.
func acquireFreshHostLock(lockPath string) (*FileLock, error) {
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if registerHostAfterCreateSeam != nil {
		registerHostAfterCreateSeam(lockPath)
	}
	lock, err := classifyAndLock(f, lockPath)
	if err != nil {
		return nil, err
	}
	held, statErr := lock.f.Stat()
	pathInfo, pathErr := os.Stat(lockPath)
	if statErr != nil || pathErr != nil || !os.SameFile(held, pathInfo) {
		_ = lock.Release()
		return nil, errHostLockReplaced
	}
	return lock, nil
}

// RegisterHost is the registration primitive doc sec.3.6 describes: a fresh
// random uuid, an exclusive OS lock acquired on its lock file (created
// fresh -- never pre-existing, so the only contention here is a reaper or
// prober touching the just-created file, which costs one retry under a new
// id, see acquireFreshHostLock), and a display-only async_hosts row.
//
// RegisterHost is NOT idempotent -- every call registers a brand-new host
// id and lock file, even if this process already holds one. Doc sec.3.6's
// "lazy at first claim" means calling this ONCE, the first time a process
// needs a host identity, and reusing the result -- that lazy-once wrapper is
// AsyncJobStore.ensureHost (memoized per store); this function is the
// unconditional primitive it calls.
//
// Registration FAILS outright if the lock cannot be acquired (doc sec.3.6:
// a filesystem without lock support must fail registration; that class, and
// only that class, is ErrHostLockUnavailable) or if the DB insert fails (a
// plain error; the just-acquired lock is released before returning, so a
// failed registration never leaks a held lock).
func RegisterHost(ctx context.Context, dataDir string, pid int, label string, store AsyncHostStore) (*HostIdentity, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("host lock: RegisterHost: empty dataDir")
	}
	var (
		id       string
		lockPath string
		lock     *FileLock
		err      error
	)
	for attempt := 0; attempt < registerHostAttempts; attempt++ {
		id = uuid.NewString()
		lockPath = HostLockPath(dataDir, id)
		lock, err = acquireFreshHostLock(lockPath)
		if err == nil {
			break
		}
		if !isContentionError(err) && !errors.Is(err, errHostLockReplaced) {
			break
		}
	}
	if err != nil {
		if isContentionError(err) || errors.Is(err, errHostLockReplaced) {
			return nil, fmt.Errorf("host lock: register: no stable lock file after %d attempts (last %s): %w",
				registerHostAttempts, lockPath, err)
		}
		return nil, fmt.Errorf("%w: register: acquire %s: %w", ErrHostLockUnavailable, lockPath, err)
	}
	if _, err := store.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{
		ID:        id,
		Pid:       int64(pid),
		Label:     label,
		StartedAt: time.Now().Unix(),
	}); err != nil {
		_ = lock.Release()
		return nil, fmt.Errorf("host lock: register: db insert: %w", err)
	}
	markOwnHostID(id)
	return &HostIdentity{ID: id, DataDir: dataDir, lock: lock}, nil
}

// Close is the owner-exit path (doc sec.3.6: "файл удаляет владелец при
// выходе, если строк не осталось"). If this host has zero referencing
// async_jobs rows, its async_hosts row and lock file are both removed;
// otherwise only the OS lock is released, leaving the DB row and file for
// a later recoverer (RemoveDeadHostFile) or retention pass. Safe to call on
// nil or an already-closed identity.
func (h *HostIdentity) Close(ctx context.Context, store AsyncHostStore) error {
	if h == nil || h.lock == nil {
		return nil
	}
	lockPath := h.lock.Path
	rows, delErr := store.DeleteAsyncHostIfNoJobs(ctx, h.ID)
	relErr := h.lock.Release()
	h.lock = nil
	unmarkOwnHostID(h.ID)
	if delErr != nil {
		// Still release the lock (already done above) so the process can
		// exit cleanly; the DB row and file survive for a recoverer/
		// retention to find later.
		return fmt.Errorf("host lock: close: delete host row: %w", delErr)
	}
	if relErr != nil {
		return fmt.Errorf("host lock: close: release: %w", relErr)
	}
	if rows > 0 {
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("host lock: close: remove %s: %w", lockPath, err)
		}
	}
	return nil
}

// removeDeadHostFileBeforeRemoveSeam is a test-only hook fired immediately before
// RemoveDeadHostFile unlinks the file, so a test can observe whether the lock
// is still held at that instant (the POSIX ordering law). nil in production.
var removeDeadHostFileBeforeRemoveSeam func(lockPath string)

// RemoveDeadHostFile is the recoverer path (doc sec.3.6: "восстановитель,
// держащий блокировку, после проверки, что открытый файл тот же, что по
// пути ... удаляет [его]"). held must be a *FileLock won from
// ProbeHostLock/ProbeHost's HostStatusDead result -- i.e. proof this
// process just won the exclusive lock on a file that existed at probe
// time.
//
// Before deleting, re-verifies (via os.SameFile) that the path still names
// the exact inode/file object this process has open: time may have passed
// between the probe and this call, and another process could have deleted
// and recreated the file at the same path in between. Losing that race is
// not an error -- the file this process holds open is simply released and
// left alone; retention reaps whatever, if anything, is left.
//
// Order is platform-specific. POSIX unlinks WHILE STILL HOLDING the lock and
// releases afterwards (unlinkBeforeUnlock): a registrant that created this
// same path and is about to lock it can then never lock-and-verify the file in
// the gap between our release and our unlink (acquireFreshHostLock), which
// would leave it holding an unlinked inode. Windows cannot delete an open file
// (no FILE_SHARE_DELETE), so it closes first (doc sec.3.6: "на Windows —
// закрыть, затем удалить"); there a registrant's open handle makes the remove
// fail with a sharing violation instead, leaving its file intact.
func RemoveDeadHostFile(lockPath string, held *FileLock) error {
	if held == nil {
		return fmt.Errorf("host lock: RemoveDeadHostFile: nil lock")
	}
	heldInfo, statErr := held.f.Stat()
	if statErr != nil {
		_ = held.Release()
		return fmt.Errorf("host lock: stat held fd: %w", statErr)
	}
	pathInfo, err := os.Stat(lockPath)
	if err != nil {
		_ = held.Release()
		if errors.Is(err, os.ErrNotExist) {
			return nil // already gone; nothing left to remove
		}
		return fmt.Errorf("host lock: stat %s: %w", lockPath, err)
	}
	if !os.SameFile(heldInfo, pathInfo) {
		// Lost the identity race: some other owner replaced lockPath after
		// this process opened/locked the old inode. Leave the CURRENT file
		// alone -- release our (now-orphaned) handle and let retention
		// reap it if it's ever empty.
		_ = held.Release()
		return nil
	}
	if unlinkBeforeUnlock {
		if removeDeadHostFileBeforeRemoveSeam != nil {
			removeDeadHostFileBeforeRemoveSeam(lockPath)
		}
		rmErr := os.Remove(lockPath)
		relErr := held.Release()
		if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("host lock: remove %s: %w", lockPath, rmErr)
		}
		if relErr != nil {
			return fmt.Errorf("host lock: release after remove: %w", relErr)
		}
		return nil
	}
	if err := held.Release(); err != nil {
		return fmt.Errorf("host lock: release before remove: %w", err)
	}
	if removeDeadHostFileBeforeRemoveSeam != nil {
		removeDeadHostFileBeforeRemoveSeam(lockPath)
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A race lost after the identity check (rare). Not fatal --
		// retention reaps it later (doc sec.3.6/3.7).
		return fmt.Errorf("host lock: remove %s: %w", lockPath, err)
	}
	return nil
}

// AsyncHostStoreFromConn adapts any db.DBTX (a *sql.DB, a *sql.Tx, ...)
// into an AsyncHostStore, so callers holding a raw connection/transaction
// rather than an already-built *db.Queries can pass it directly to
// RegisterHost/Close.
func AsyncHostStoreFromConn(dbtx db.DBTX) AsyncHostStore {
	return db.New(dbtx)
}
