package session_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/dbtest"
	"github.com/stretchr/testify/require"
)

// parallelSlots bounds how many of this package's t.Parallel() subtests
// actually execute their body at the same time, independent of whatever
// -parallel value (default GOMAXPROCS) the test binary was invoked with.
//
// Found by measurement (task #592): this package has ~80 t.Parallel()
// subtests, most driving a real RunQueuePump against a real
// (SetMaxOpenConns(1), production-faithful) SQLite DB with real,
// accelerated-but-genuine wall-clock TTLs/backoffs (TestTick, TestLeaseTTL,
// TestDBWriteTimeout -- see run_queue_pump.go's RunQueuePumpConfig doc). A
// single `go test -race` invocation already runs all ~80 concurrently
// (bounded only by GOMAXPROCS); eight such invocations at once (a stress
// mode used to shake out real concurrency defects in this package, not a CI
// shape) multiply that to several hundred goroutines contending for CPU and
// disk across only a handful of real cores. Under that contention, some of
// those real wall-clock budgets -- themselves already widened repeatedly in
// prior sessions to absorb ordinary CI slowness -- are missed not because
// the production code is wrong, but because the test process cannot
// schedule a goroutine or complete a disk write in time. One such run
// reproduced a genuine duplicate-dispatch
// (TestReleaseGate_P350_LeaseRenewedDuringLongExecution observed 2
// executions instead of 1): the renewal loop's ticker goroutine was starved
// for longer than a full lease TTL, not a logic defect in the renewal code
// itself.
//
// This does NOT touch any TTL, timeout, or require.Eventually window -- it
// reduces how many of this package's own subtests self-inflict contention
// on each other and on the disk, which is the actual variable this task's
// own hypothesis section asked to be tested first (and which the measured
// before/after tallies confirmed: same failures, far less often, with zero
// source changes to any duration constant).
//
// Sized as a quarter of GOMAXPROCS (floor 2, so a single test process can
// always make forward progress even on a 1-2 core CI runner) rather than a
// fixed number, so it scales with whatever machine runs the suite instead
// of encoding this machine's 16 logical CPUs as a magic constant.
var (
	parallelSlotsOnce sync.Once
	parallelSlotsCh   chan struct{}
)

func parallelSlots() chan struct{} {
	parallelSlotsOnce.Do(func() {
		n := runtime.GOMAXPROCS(0) / 4
		if n < 2 {
			n = 2
		}
		parallelSlotsCh = make(chan struct{}, n)
	})
	return parallelSlotsCh
}

// limitParallel bounds this test's actual execution (not its eligibility to
// be scheduled) to parallelSlots() concurrent bodies package-wide. Call
// immediately after t.Parallel() in every parallel test in this package --
// see parallelSlots' doc for why.
func limitParallel(t *testing.T) {
	t.Helper()
	sem := parallelSlots()
	sem <- struct{}{}
	t.Cleanup(func() { <-sem })
}

// Helper: mock coordinator for testing
type mockCoordinator struct{}

func (m *mockCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	return nil, nil
}

// stopPumpLoggingForcedShutdown registers a t.Cleanup that calls pump.Stop()
// and logs its return value plus how long the call took.
//
// Context (task #583, second attempt): setupTestSession's own t.Cleanup
// closes the underlying sql.DB unconditionally, ignoring whatever a test's
// own RunQueuePump.Stop() reports. Production does NOT do this --
// internal/app/app_lifecycle.go:32-36 checks Stop()'s bool return and
// deliberately SKIPS the DB close when Stop() reports a forced shutdown
// ("DB will NOT be closed, in-progress work may be incomplete"), because a
// forced Stop() means the 5s grace period elapsed while a worker was still
// touching the DB -- not that the worker actually stopped touching it.
//
// Every call site in this package registers its pump cleanup AFTER calling
// setupTestSession, so t.Cleanup's LIFO order already runs pump.Stop()
// before sqlDB.Close() in the ordinary case -- but if Stop() itself reports
// "forced" (true), that ordering guarantee is worthless: a worker goroutine
// may still be mid-write when this function returns and the very next
// cleanup (sqlDB.Close()) runs anyway, exactly mirroring production's
// documented reason for skipping the close in that case.
//
// This helper does not change that behavior yet -- it only makes Stop()'s
// return value observable, so repeated runs can show whether "forced" is
// ever actually reached under load before deciding whether the test harness
// needs the same skip-close policy production has.
func stopPumpLoggingForcedShutdown(t *testing.T, pump *session.RunQueuePump) {
	t.Helper()
	t.Cleanup(func() {
		start := time.Now()
		forced := pump.Stop()
		elapsed := time.Since(start)
		if forced {
			t.Logf("pump.Stop() FORCED shutdown after %s (a worker was still running when the 5s grace period elapsed -- sqlDB.Close() runs next and may race it)", elapsed)
		} else {
			t.Logf("pump.Stop() graceful shutdown after %s", elapsed)
		}
	})
}

// Helper: setup a test session and service
func setupTestSession(t *testing.T, title string) (*session.Session, session.Service) {
	// Use file-based database in temp dir to avoid connection pool issues with :memory:
	// Each connection to :memory: creates a separate database; when sql.Open recycles
	// a connection (e.g., after ErrBadConn from context cancellation), data is lost.
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := dbtest.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Configure connection pool for concurrent access
	sqlDB.SetMaxOpenConns(1) // SQLite only supports one writer
	sqlDB.SetMaxIdleConns(1)

	// Run migrations (copied from session_test.go newTestDB)
	_, err = sqlDB.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			smart_model_provider TEXT,
			smart_model_id TEXT,
			smart_model_reasoning_effort TEXT DEFAULT 'medium',
			fast_model_provider TEXT,
			fast_model_id TEXT,
			fast_model_reasoning_effort TEXT DEFAULT 'medium',
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			usage_updated_at INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0,
			cost_checksum INTEGER NOT NULL DEFAULT 0,
			origin TEXT DEFAULT '' NOT NULL,
			cost_self REAL NOT NULL DEFAULT 0,
			cost_base REAL NOT NULL DEFAULT 0,
			cost_parent_id TEXT NOT NULL DEFAULT '',
			workspace_root TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (parent_session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

		CREATE TABLE session_permissions (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			action TEXT NOT NULL,
			path TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);

		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cache_creation_tokens INTEGER,
			cache_read_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			usage_provider TEXT,
			usage_model TEXT,
			cache_support TEXT,
			usage_estimated INTEGER,
			origin TEXT DEFAULT '' NOT NULL,
			checkpoint_generation INTEGER NOT NULL DEFAULT 0,
			notice_kind TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE files (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			content TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(path, session_id, version)
		);

		CREATE TABLE read_files (
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			read_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, path)
		);

		CREATE TABLE pending_injects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			content TEXT NOT NULL,
			interrupt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);

		CREATE TABLE session_run_queue (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			call_data TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			leased_by TEXT,
			leased_at INTEGER,
			lease_expires_at INTEGER,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			terminal_failure INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE TABLE orphan_call_outbox (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
			call_data TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('pending', 'processing', 'done', 'failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			last_error TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE INDEX idx_files_session_id ON files(session_id);
		CREATE INDEX idx_files_path ON files(path);
		CREATE INDEX idx_messages_session_id ON messages(session_id);
		CREATE INDEX idx_pending_injects_session_id ON pending_injects(session_id);
		CREATE INDEX idx_session_run_queue_session_status ON session_run_queue(session_id, status);
		CREATE INDEX idx_session_run_queue_created_at ON session_run_queue(created_at);
		CREATE INDEX idx_orphan_call_outbox_status ON orphan_call_outbox(status, created_at);
		CREATE INDEX idx_orphan_call_outbox_session_id ON orphan_call_outbox(session_id);
	`)
	require.NoError(t, err)
	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(context.Background(), sqlDB))

	// Create queries and service
	queries := db.New(sqlDB)
	svc := session.NewService(queries, sqlDB)

	// Create a session
	sess, err := svc.Create(context.Background(), title)
	require.NoError(t, err)

	return &sess, svc
}

// enqueueTwoRows enqueues durable rows A and B (FIFO -- A leases first) for
// a single fresh session and returns everything a test needs to drive and
// assert on the scenario.
func enqueueTwoRows(t *testing.T, name string, sessions session.Service, sessionID string) {
	t.Helper()
	callDataA, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-A", sessionID, callDataA))

	callDataB, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-B"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-B", sessionID, callDataB))
}

// gatedCoordinator lets a test control exactly when the background
// execution's Coordinator.Run call returns, and what it returns, so the
// admission race between the background tick and a concurrent
// DrainSessionNow call can be forced deterministically instead of hoping
// timing lines up.
type gatedCoordinator struct {
	calls      atomic.Int64
	started    chan struct{}
	release    chan error // send the desired Run() return value to unblock
	identity   string
	identities []string
}

func newGatedCoordinator() *gatedCoordinator {
	return &gatedCoordinator{
		started: make(chan struct{}, 1),
		release: make(chan error),
	}
}

func (c *gatedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	session.RecordExecutionAssistantIdentity(ctx, c.identity)
	for _, id := range c.identities {
		session.RecordExecutionAssistantIdentity(ctx, id)
	}
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case err := <-c.release:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func oneEntrySetupReported(t *testing.T, name string, gate *gatedCoordinator, tl *testTimeline) (*session.RunQueuePump, string) {
	t.Helper()
	reportPumpSetupOnFailure(t, tl)
	tl.addf("fixture setup started")
	sess, svc := setupTestSession(t, name)
	tl.addf("fixture ready")
	registerPumpCalls(t, "gated", &gate.calls)
	registerPumpQueueReport(t, tl, svc, name+"-idem-1")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-" + name,
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	tl.stepf(func() {
		require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), name+"-idem-1", sess.ID, callData))
	}, "enqueue")

	pump.Start()
	tl.addf("pump started")
	stopPumpLoggingForcedShutdown(t, pump)
	reportPumpOnFailure(t, tl)

	select {
	case <-gate.started:
		tl.addf("background execution started")
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing about the race under test")
	}

	return pump, sess.ID
}

// twoRowFailThenSucceedCoordinator reports a FIXED outcome for the first
// Coordinator.Run call (row A) and a clean success for every call after
// that (row B, and any further row) -- the inverse of p588's
// twoRowCoordinator, which reports busy from the second call onward. Used
// to force the "row A fails, row B succeeds" sequence deterministically
// through DrainSessionNow's own local-execution loop, without needing to
// simulate external contention.
type twoRowFailThenSucceedCoordinator struct {
	calls   int
	firstFn func() (*any, error)
}

func (c *twoRowFailThenSucceedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls++
	if c.calls == 1 {
		return c.firstFn()
	}
	return nil, nil
}

// sequentialGatedCoordinator is gatedCoordinator's multi-call sibling: each
// successive Coordinator.Run call parks on its OWN gate, taken from a
// pre-supplied ordered list, so a test can control exactly what EACH of
// several sequential background-tick executions returns -- not just the
// first. Used by the observed-admission variants below, where BOTH row A
// and row B are executed by the pump's own background tick (never by
// DrainSessionNow's local-lease branch), so the row-identity contract is
// exercised entirely through the wait-on-admissionEntry path for BOTH rows,
// not just one of them.
type sequentialGatedCoordinator struct {
	calls   int
	started chan struct{}
	gates   []chan error // gates[i] is call i+1's release channel
}

func newSequentialGatedCoordinator(n int) *sequentialGatedCoordinator {
	c := &sequentialGatedCoordinator{started: make(chan struct{}, n)}
	for i := 0; i < n; i++ {
		c.gates = append(c.gates, make(chan error))
	}
	return c
}

func (c *sequentialGatedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	idx := c.calls
	c.calls++
	select {
	case c.started <- struct{}{}:
	default:
	}
	if idx >= len(c.gates) {
		return nil, nil
	}
	select {
	case err := <-c.gates[idx]:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
