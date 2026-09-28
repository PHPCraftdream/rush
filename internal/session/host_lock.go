// Host liveness for the phase-4 durable async core
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.6): a host
// (a process executing async jobs) is alive iff it holds the exclusive OS
// lock on its own <dataDir>/hosts/<host_id>.lock file. No clock is ever
// consulted -- "dead" means either the lock was just won by someone else,
// or the file is simply gone (ENOENT). Any other outcome is "unknown", and
// an unknown host's rows are never treated as reapable.
//
// This file only implements the primitive (registration, probe, the two
// deletion paths). Wiring it into the async job claim path is a later step
// (doc sec.5 step 4/6); async_hosts rows stay display-only bookkeeping
// throughout -- no FK from async_jobs, no timestamp used for liveness.
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
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostStatusDead, nil, nil
		}
		return HostStatusUnknown, nil, fmt.Errorf("host lock probe: open %s: %w", lockPath, err)
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

// RegisterHost is the registration primitive doc sec.3.6 describes: a fresh
// random uuid, an exclusive OS lock acquired on its lock file (created
// fresh -- never pre-existing, so contention here would mean an
// astronomically unlikely uuid collision, not a real host), and a
// display-only async_hosts row.
//
// RegisterHost is NOT idempotent -- every call registers a brand-new host
// id and lock file, even if this process already holds one. Doc sec.3.6's
// "lazy at first claim" means calling this ONCE, the first time a process
// needs a host identity, and reusing the result -- that lazy-once wrapper
// (memoizing per process/App instance) arrives with the claim wiring in a
// later step; this function is the unconditional primitive it will call.
//
// Registration FAILS outright if the lock cannot be acquired (doc sec.3.6:
// a filesystem without lock support must fail registration) or if the DB
// insert fails (in which case the just-acquired lock is released before
// returning, so a failed registration never leaks a held lock).
func RegisterHost(ctx context.Context, dataDir string, pid int, label string, store AsyncHostStore) (*HostIdentity, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("host lock: RegisterHost: empty dataDir")
	}
	id := uuid.NewString()
	lockPath := HostLockPath(dataDir, id)
	lock, err := TryAcquireFileLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("host lock: register: acquire %s: %w", lockPath, err)
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
// Order is release-then-remove on every platform (doc sec.3.6: "на Windows
// — закрыть, затем удалить"), which is also safe on POSIX since flock is
// keyed off the inode, not the path.
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
	if err := held.Release(); err != nil {
		return fmt.Errorf("host lock: release before remove: %w", err)
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
