// RETAINED host lifecycle, job-independent (package R12-D chunk 1): the
// register/ensure/Close trio the host lock module describes (doc sec.3.6),
// implemented WITHOUT any async_jobs query so it survives R12-H dropping the
// async_jobs table. This file ADDS a new implementation next to
// AsyncJobStore's existing one; the OFF path (RUSH_BATCH_RUNTIME off) keeps
// using AsyncJobStore untouched.
//
// Two async_jobs couplings are deliberately NOT carried over:
//   - ensureHost's first-registration dead-host sweep (SweepDeadHosts) reads
//     async_jobs and stays a job-table concern -- HostStore.EnsureHost does
//     not sweep.
//   - "delete host if no jobs" becomes "delete host if no LIVE batch_nodes
//     rows" (state in the live set) via db.Queries.DeleteAsyncHostIfNoNodes.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/db"
)

// hostNodeStore adapts *db.Queries' NEW host methods (async_hosts.sql) onto
// the AsyncHostStore interface host_lock.go already defines, so RegisterHost
// and HostIdentity.Close can be REUSED verbatim (read-only reuse: host_lock.go
// is not edited) without ever touching an async_jobs query.
type hostNodeStore struct {
	q *db.Queries
}

// hostNodeStore implements AsyncHostStore.
var _ AsyncHostStore = hostNodeStore{}

func (a hostNodeStore) RegisterAsyncHost(ctx context.Context, arg db.RegisterAsyncHostParams) (db.AsyncHost, error) {
	return a.q.RegisterAsyncHostRow(ctx, db.RegisterAsyncHostRowParams{
		ID:        arg.ID,
		Pid:       arg.Pid,
		Label:     arg.Label,
		StartedAt: arg.StartedAt,
	})
}

// DeleteAsyncHostIfNoJobs is the AsyncHostStore method name host_lock.go's
// Close path calls; the implementation routes to the nodes-based guard
// (DeleteAsyncHostIfNoNodes) instead of the jobs-based one.
func (a hostNodeStore) DeleteAsyncHostIfNoJobs(ctx context.Context, id string) (int64, error) {
	return a.q.DeleteAsyncHostIfNoNodes(ctx, id)
}

// HostStore is the retained, job-independent host lifecycle: one lazily
// registered HostIdentity per store (memoized exactly like
// AsyncJobStore.ensureHost), plus the clean/forced teardown paths.
type HostStore struct {
	sqlDB   *sql.DB
	q       *db.Queries
	dataDir string
	pid     int
	label   string

	// regMu serialises host registration and teardown (EnsureHost, Close,
	// CloseKeepLock, SimulateCrashForTest) exactly like AsyncJobStore's.
	// It IS held across the registration I/O, so nothing that can run inside
	// a writer transaction ever takes it.
	regMu sync.Mutex
	// host is this store's registered identity; nil before the first
	// EnsureHost. Published only after RegisterHost fully succeeded.
	host atomic.Pointer[HostIdentity]
}

// NewHostStore builds a HostStore bound to sqlDB (the writer connection) and
// dataDir (for the host lock file). Registration is deferred to the first
// EnsureHost (lazy), not performed here.
func NewHostStore(sqlDB *sql.DB, dataDir string, pid int, label string) *HostStore {
	return &HostStore{sqlDB: sqlDB, q: db.New(sqlDB), dataDir: dataDir, pid: pid, label: label}
}

// hostQueries returns the AsyncHostStore view of this store's writer
// queries, routed at the nodes-based host methods.
func (s *HostStore) hostQueries() AsyncHostStore {
	return hostNodeStore{q: s.q}
}

// EnsureHost lazily registers this store's host identity exactly once (doc
// sec.3.6), returning its id. Safe for concurrent callers. Same memoization
// pattern as AsyncJobStore.ensureHost, MINUS the first-registration
// dead-host sweep: that sweep is an async_jobs concern and is deliberately
// not performed here (see the file's doc).
func (s *HostStore) EnsureHost(ctx context.Context) (string, error) {
	if h := s.host.Load(); h != nil {
		return h.ID, nil
	}
	s.regMu.Lock()
	if h := s.host.Load(); h != nil {
		s.regMu.Unlock()
		return h.ID, nil
	}
	h, err := RegisterHost(ctx, s.dataDir, s.pid, s.label, s.hostQueries())
	if err != nil {
		s.regMu.Unlock()
		return "", err
	}
	s.host.Store(h)
	s.regMu.Unlock()
	return h.ID, nil
}

// HostID returns this store's registered host id, or "" before the first
// EnsureHost. Lock-free.
func (s *HostStore) HostID() string {
	if h := s.host.Load(); h != nil {
		return h.ID
	}
	return ""
}

// Close releases this store's host identity (doc sec.3.6's owner-exit path),
// if one was ever registered. Safe to call on a store that never ensured a
// host (no-op). The host row survives iff the host still owns batch_nodes
// rows (DeleteAsyncHostIfNoNodes, live-set rule); a clean exit also takes this host's
// session-driver markers with it, exactly like AsyncJobStore.Close.
func (s *HostStore) Close(ctx context.Context) error {
	h := s.takeHost()
	if h == nil {
		return nil
	}
	if _, err := s.q.DeleteSessionDriversForHost(ctx, h.ID); err != nil {
		slog.Warn("host store: close: delete session driver markers failed", "host_id", h.ID, "err", err)
	}
	return h.Close(ctx, s.hostQueries())
}

// CloseKeepLock is Close's forced-shutdown counterpart (A12): it forgets the
// in-memory host handle WITHOUT releasing the OS lock, deleting the
// async_hosts row, or touching the lock file -- identical semantics to
// AsyncJobStore.CloseKeepLock (the lock is pinned until process exit).
// Safe to call on a store that never ensured a host (no-op).
func (s *HostStore) CloseKeepLock() {
	h := s.takeHost()
	// Pin the OS lock: the *os.File finalizer would release it at next GC.
	if h != nil {
		retainHostLockUntilExit(h.lock)
	}
}

// SimulateCrashForTest releases this store's own OS host lock and forgets
// its "own id" marking, WITHOUT deleting the async_hosts/batch_nodes rows or
// the lock file itself -- the same crash model as
// AsyncJobStore.SimulateCrashForTest, so a later ProbeHost call in the same
// test process sees this host as dead. Test-only.
func (s *HostStore) SimulateCrashForTest() error {
	h := s.takeHost()
	if h == nil {
		return nil
	}
	err := h.lock.Release()
	unmarkOwnHostID(h.ID)
	return err
}

// takeHost detaches the registered identity, first waiting out an in-flight
// registration (regMu) so a Close racing EnsureHost cannot leave a host
// published after teardown.
func (s *HostStore) takeHost() *HostIdentity {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.host.Swap(nil)
}

// ClaimSessionDriver records this store's host as sessionID's driver: the
// job-independent twin of AsyncJobStore.ClaimSessionDriver. Same contract:
// idempotent for the same host; refuses (*ErrSessionDrivenElsewhere) while
// another host that is alive or unknown holds the marker; takes a dead
// host's marker over. NO async_jobs work happens on this path -- unlike the
// AsyncJobStore version, EnsureHost here never runs the recovery sweep
// (SweepDeadHosts), which is the whole point of this duplicate surface.
// The ErrDriverMarkerUnavailable mapping is identical (session_driver.go).
func (s *HostStore) ClaimSessionDriver(ctx context.Context, sessionID string) error {
	hostID, err := s.EnsureHost(ctx)
	if err != nil {
		if errors.Is(err, ErrHostLockUnavailable) {
			return fmt.Errorf("%w: %w", ErrDriverMarkerUnavailable, err)
		}
		return fmt.Errorf("session driver: register host: %w", err)
	}
	return claimSessionDriverFor(ctx, hostID, s.q, s.dataDir, s.pid, sessionID)
}

// ReleaseSessionDriver deletes this store's own claim on sessionID (never
// another host's). A no-op for a store that never registered a host.
func (s *HostStore) ReleaseSessionDriver(ctx context.Context, sessionID string) error {
	return releaseSessionDriverFor(ctx, s.HostID(), s.q, sessionID)
}

// ForeignLiveDriver reports whether ANOTHER host that is alive or unknown
// drives sessionID: no marker, or a marker naming this store's own host, is
// not foreign; a dead host's marker is not foreign either. Same semantics as
// AsyncJobStore.ForeignLiveDriver; this store has a single connection, so
// the PK read goes through s.q.
func (s *HostStore) ForeignLiveDriver(ctx context.Context, sessionID string) (SessionDriver, bool, error) {
	return foreignLiveDriverFor(ctx, s.HostID(), s.q, s.dataDir, sessionID)
}
