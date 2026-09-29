// Dead-host recovery coverage (docs/plans/2026-09-28-async-phase4-durable-
// core.md sec.3.6/3.7, step 5): DUR-5's liveness classification feeding
// recovery, DUR-6's "no messages, no wake" guarantee, the recovery matrix
// (announced/unannounced/no-rows-left/racing recoverers/unknown host), own-
// scope recovery, and retention. Real SQLite + real on-disk lock files
// throughout -- a dead host is simulated by acquiring then releasing the OS
// lock of a fabricated host id, exactly like a crashed process leaves its
// lock file behind (host_lock_test.go's own TestRemoveDeadHostFile_*
// pattern).
package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// fabricateDeadHost simulates a crashed host: its lock file exists on disk
// (acquired then released, exactly like TestRemoveDeadHostFile_* does) and
// it has a display-only async_hosts row, but nothing holds its lock.
func fabricateDeadHost(t *testing.T, ctx context.Context, q *db.Queries, dataDir, hostID string) {
	t.Helper()
	seed, err := TryAcquireFileLock(HostLockPath(dataDir, hostID))
	require.NoError(t, err)
	require.NoError(t, seed.Release())
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: hostID, Pid: 12345, Label: "dead", StartedAt: 1700000000})
	require.NoError(t, err)
}

// seedRunningJob inserts a 'running' async_jobs row owned by hostID.
// childSessionID == "" means a plain (non-delegation) job.
func seedRunningJob(t *testing.T, ctx context.Context, q *db.Queries, owner, toolCallID, hostID, childSessionID string, announced bool) {
	t.Helper()
	var childParam sql.NullString
	kind := "command"
	if childSessionID != "" {
		childParam = sql.NullString{String: childSessionID, Valid: true}
		kind = "agent"
	}
	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: owner, ToolCallID: toolCallID, Kind: kind, ToolName: "bash",
		InputHash: "h-" + toolCallID, HostID: hostID, ChildSessionID: childParam,
		CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	if announced {
		_, err := q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
			UpdatedAt: 1700000001, OwnerSessionID: owner, ToolCallID: toolCallID,
		})
		require.NoError(t, err)
	}
}

// finishedAssistantText inserts a finished assistant message with text into
// sessionID's history -- recoveredDelegationText's own read target.
func finishedAssistantText(t *testing.T, ctx context.Context, messages message.Service, sessionID, text string) {
	t.Helper()
	_, err := messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
}

// ============================================================
// DUR-5: liveness classification feeding recovery.
// ============================================================

// TestRecoverDeadHost_AliveHostUntouched pins DUR-5: a live host is never
// "dead" under any probe -- RecoverDeadHost must be a total no-op (zero
// outcome, nil error) and leave the running row exactly as it was.
func TestRecoverDeadHost_AliveHostUntouched(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	holder, err := TryAcquireFileLock(HostLockPath(store.dataDir, "alive-host"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "alive-host", Pid: 1, Label: "alive", StartedAt: 1700000000})
	require.NoError(t, err)
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "alive-host", "", true)

	outcome, err := store.RecoverDeadHost(ctx, "alive-host", nil)
	require.NoError(t, err)
	require.Equal(t, RecoveryOutcome{}, outcome)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "a live host's row must never be touched")
}

// TestRecoverDeadHost_UnknownProbeLeavesRowsAlone pins DUR-5: an unknown
// probe outcome (not ENOENT, not a lock win, not contention) must not yield
// "dead" -- rows stay exactly as they were, and the probe's error is
// surfaced to the caller.
func TestRecoverDeadHost_UnknownProbeLeavesRowsAlone(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	// A directory at the lock path forces ProbeHostLock's "some other error"
	// branch (host_lock_test.go's own TestProbeHostLock_Unknown technique).
	lockPath := HostLockPath(store.dataDir, "unknown-host")
	require.NoError(t, os.MkdirAll(lockPath, 0o755))
	_, err := q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "unknown-host", Pid: 1, Label: "unknown", StartedAt: 1700000000})
	require.NoError(t, err)
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "unknown-host", "", true)

	outcome, err := store.RecoverDeadHost(ctx, "unknown-host", nil)
	require.Error(t, err, "an unknown probe outcome must surface its error, not be swallowed")
	require.Equal(t, RecoveryOutcome{}, outcome)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "an unknown probe must never yield 'dead' -- the row must stay untouched")
}

// TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile pins
// DUR-5's lazy-registration half from the other side: a store that never
// calls Claim must register no host at all -- no HostID, and no lock file
// anywhere under hosts/.
func TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile(t *testing.T) {
	t.Parallel()
	store, _, _ := newTestStore(t)

	require.Empty(t, store.HostID(), "a store that never claimed anything must have no host id")
	entries, err := os.ReadDir(HostsDir(store.dataDir))
	if err != nil {
		require.True(t, os.IsNotExist(err), "hosts/ must not even exist when nothing was ever claimed")
		return
	}
	require.Empty(t, entries, "no lock file may exist for a store that never claimed a job")
}

// ============================================================
// DUR-6 + recovery matrix.
// ============================================================

// TestRecoverDeadHost_AnnouncedDelegationInterruptedWithChildText pins the
// recovery matrix's delegation branch: an announced, running delegation row
// on a dead host becomes 'interrupted', wake=0, delivery='pending', and its
// result text is the child session's own last finished assistant message.
func TestRecoverDeadHost_AnnouncedDelegationInterruptedWithChildText(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "child-1"))
	messages := message.NewService(db.New(store.sqlDB))
	finishedAssistantText(t, ctx, messages, "child-1", "the sub-agent's final answer")

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "parent-1", "call-1", "dead-host-1", "child-1", true)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", messages)
	require.NoError(t, err)
	require.Equal(t, 1, outcome.Interrupted)
	require.Equal(t, 0, outcome.Deleted)

	row, err := store.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State)
	require.Equal(t, "interrupted", row.NoticeKind)
	require.EqualValues(t, 0, row.Wake, "recovery must never set wake")
	require.Equal(t, "pending", row.Delivery, "an interrupted row is an ordinary pull candidate")
	require.Equal(t, "the sub-agent's final answer", row.ResultSummary.String)
	require.EqualValues(t, 1, row.ResultIsError.Int64)
}

// TestRecoverDeadHost_AnnouncedDelegationInterrupted_NoChildText pins the
// "if the child has no text, say so" branch: a child with no finished
// assistant message at all falls back to the fixed no-text wording, never an
// empty result.
func TestRecoverDeadHost_AnnouncedDelegationInterrupted_NoChildText(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "child-1"))
	messages := message.NewService(db.New(store.sqlDB))
	// Child has no assistant messages at all.

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "parent-1", "call-1", "dead-host-1", "child-1", true)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", messages)
	require.NoError(t, err)
	require.Equal(t, 1, outcome.Interrupted)

	row, err := store.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, interruptedNoChildTextText, row.ResultSummary.String)
	require.NotEmpty(t, row.ResultSummary.String, "recovery must never leave an empty result summary")
}

// TestRecoverDeadHost_AnnouncedBashInterruptedFixedText pins the recovery
// matrix's non-delegation branch: doc sec.3.7's exact fixed wording.
func TestRecoverDeadHost_AnnouncedBashInterruptedFixedText(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", true)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", nil)
	require.NoError(t, err)
	require.Equal(t, 1, outcome.Interrupted)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State)
	require.Equal(t, interruptedBashText, row.ResultSummary.String)
}

// failingListMessages is a minimal message.Service stub whose List always
// errors -- A6's transient-read-failure scenario. RecoverDeadHost/
// recoveredDelegationText only ever call List; any other method being
// reached here would panic via the nil embedded Service, which fails the
// test loudly rather than silently doing the wrong thing.
type failingListMessages struct {
	message.Service
}

func (failingListMessages) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	return nil, fmt.Errorf("simulated transient read failure")
}

// TestRecoverDeadHost_TransientChildReadErrorSkipsRowLeavesRunning is A6's
// fix: a TRANSIENT messages.List error (not "no messages") must never commit
// a false "finished with no textual response" -- the row must stay
// 'running' for a later sweep to retry.
//
// REVERT CHECK: temporarily made recoveredDelegationText ignore the read
// error and fall back to interruptedNoChildTextText (the pre-fix behavior).
// This test's `require.Equal(t, "running", ...)` FAILED (state was
// "interrupted" instead), proving the assertion catches the false-completion
// bug. Restored the skip-on-error fix; re-ran, passed.
func TestRecoverDeadHost_TransientChildReadErrorSkipsRowLeavesRunning(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "child-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "parent-1", "call-1", "dead-host-1", "child-1", true)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", failingListMessages{})
	require.NoError(t, err, "a per-row transient read failure must not fail the whole recovery pass")
	require.Equal(t, 0, outcome.Interrupted, "a row must not be committed to 'interrupted' off a transient read failure")

	row, err := store.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "A6: the row must stay running for a later sweep to retry")
}

// TestPurgeOrphanHostLockFiles_ReapsFileWithNoRow is A7/C15's fix: a lock
// file left behind by, e.g., a failed RegisterHost DB insert (lock acquired,
// insert failed, lock released but the file never deleted) has NO
// async_hosts row at all -- purgeEmptyDeadHostFiles (which only walks rows)
// can never find it. The retention pass must also scan hosts/*.lock directly
// and reap a dead, rowless file.
func TestPurgeOrphanHostLockFiles_ReapsFileWithNoRow(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestStore(t)

	lockPath := HostLockPath(store.dataDir, "orphan-host-1")
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	_, err = os.Stat(lockPath)
	require.NoError(t, err, "sanity: the orphan file exists before the purge")

	require.NoError(t, store.PurgeExpired(ctx, 7*24*time.Hour))

	_, err = os.Stat(lockPath)
	require.True(t, os.IsNotExist(err), "A7: an orphan lock file with no async_hosts row must be reaped by retention")
}

// TestPurgeOrphanHostLockFiles_NeverTouchesALiveOrphan proves the A7 fix is
// still liveness-gated: a rowless lock file that is genuinely STILL HELD
// must never be reaped, exactly like every other reaper in this file.
func TestPurgeOrphanHostLockFiles_NeverTouchesALiveOrphan(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestStore(t)

	lockPath := HostLockPath(store.dataDir, "orphan-host-live")
	holder, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	require.NoError(t, store.PurgeExpired(ctx, 7*24*time.Hour))

	_, err = os.Stat(lockPath)
	require.NoError(t, err, "a live host's lock file, even with no row, must never be reaped")
}

// TestAsyncJobStore_CloseKeepLock_LeavesLockHeld is A12's fix: on a forced
// shutdown, the host's OS lock must stay held (not released) so another
// process cannot see this host as dead and recover its still-in-flight rows
// while this process's own goroutines might still be writing.
func TestAsyncJobStore_CloseKeepLock_LeavesLockHeld(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	hostID := store.HostID()
	require.NotEmpty(t, hostID)

	require.NotNil(t, store.host.Load().lock)

	store.CloseKeepLock()
	t.Cleanup(func() {
		releaseRetainedHostLocksForTest()
		unmarkOwnHostID(hostID)
	})
	// No test-side reference to the lock is kept: without the package-level
	// pin the *os.File finalizer releases it at GC. REVERT CHECK: drop
	// retainHostLockUntilExit from CloseKeepLock -> the probe reads Dead.
	for range 3 {
		runtime.GC()
		time.Sleep(30 * time.Millisecond)
	}

	status, lock, err := ProbeHostLock(HostLockPath(store.dataDir, hostID))
	if lock != nil {
		_ = lock.Release()
	}
	require.NoError(t, err)
	require.Equal(t, HostStatusAlive, status, "CloseKeepLock must leave the OS lock held, not release it")
}

// TestRecoverDeadHost_UnannouncedDeleted pins the recovery matrix's
// unannounced branch (ASYNC-05): a running, announced=0 row on a dead host
// is deleted without a trace, not transitioned.
func TestRecoverDeadHost_UnannouncedDeleted(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", false)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", nil)
	require.NoError(t, err)
	require.Equal(t, 0, outcome.Interrupted)
	require.Equal(t, 1, outcome.Deleted)

	_, err = store.Get(ctx, "owner-1", "call-1")
	require.ErrorIs(t, err, sql.ErrNoRows, "an unannounced row must be deleted without a trace")
}

// TestRecoverDeadHost_NoRowsLeftRemovesHostAndFile pins the recovery
// matrix's tail: once a dead host references zero async_jobs rows of any
// state, its async_hosts row and lock file are both removed.
func TestRecoverDeadHost_NoRowsLeftRemovesHostAndFile(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	// Only an unannounced row -- deleted outright, leaving zero rows behind.
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", false)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", nil)
	require.NoError(t, err)
	require.True(t, outcome.HostRemoved)

	_, err = q.GetAsyncHost(ctx, "dead-host-1")
	require.ErrorIs(t, err, sql.ErrNoRows, "the host row must be removed once it references no jobs")
	_, statErr := os.Stat(HostLockPath(store.dataDir, "dead-host-1"))
	require.True(t, os.IsNotExist(statErr), "the lock file must be removed alongside the host row")
}

// TestRecoverDeadHost_RowsRemainKeepsHostAndFile is the tail's negative
// case: an announced row survives recovery as 'interrupted' (still
// referencing the host), so the host row and file must NOT be removed yet.
func TestRecoverDeadHost_RowsRemainKeepsHostAndFile(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", true)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", nil)
	require.NoError(t, err)
	require.False(t, outcome.HostRemoved)

	_, err = q.GetAsyncHost(ctx, "dead-host-1")
	require.NoError(t, err, "the host row must survive while its (now-interrupted) row still exists")
	_, statErr := os.Stat(HostLockPath(store.dataDir, "dead-host-1"))
	require.NoError(t, statErr, "the lock file must survive alongside its surviving row")
}

// TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow pins the
// recovery matrix's concurrency requirement: two independent recoverers
// racing the SAME dead host's SAME row must produce exactly one transition.
//
// A-b test fix: this does NOT isolate the OS-lock guarantee from the SQL
// CAS guarantee -- with two real recoverers, the OS exclusive lock on
// dead-host-1's lock file is what stops the LOSER from ever attempting
// Transition at all (its own ProbeHost call sees HostStatusAlive, since the
// winner is mid-recovery holding the lock), so this test cannot tell "only
// one recoverer even tried" (OS-lock layer) apart from "both tried, but the
// CAS let only one commit" (SQL layer) -- either layer alone would make this
// exact assertion pass. Both layers exist and both are load-bearing in
// production (the CAS also protects a recoverer racing the row's OWN
// legitimate winner, which no host-lock probe is involved in at all); this
// test proves the END-TO-END two-recoverer outcome, not one layer in
// isolation.
func TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow(t *testing.T) {
	t.Parallel()
	storeA, q, ctx := newTestStore(t)
	dataDir := storeA.dataDir
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	fabricateDeadHost(t, ctx, q, dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", true)

	// A second, independent AsyncJobStore over its OWN *sql.DB connection to
	// the same on-disk file -- the closest a single test process gets to "a
	// second recoverer process" (notice_pull_test.go's twoConnStores pattern).
	connB, err := sql.Open("sqlite", dataDir+"/rush.db?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = connB.Close() })
	storeB := NewAsyncJobStore(connB, dataDir, 2, "recoverer-b")

	var wg sync.WaitGroup
	outcomes := make([]RecoveryOutcome, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		outcomes[0], errs[0] = storeA.RecoverDeadHost(ctx, "dead-host-1", nil)
	}()
	go func() {
		defer wg.Done()
		<-start
		outcomes[1], errs[1] = storeB.RecoverDeadHost(ctx, "dead-host-1", nil)
	}()
	close(start)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	totalInterrupted := outcomes[0].Interrupted + outcomes[1].Interrupted
	require.Equal(t, 1, totalInterrupted, "exactly one recoverer may have won the dead host's lock and transitioned the row")

	row, err := storeA.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State)
}

// TestRecoverDeadHost_UnknownHostIsUntouchedInMatrix is the recovery
// matrix's "unknown host" row, restated against a row-bearing fixture (vs.
// TestRecoverDeadHost_UnknownProbeLeavesRowsAlone's narrower probe-only
// check) to pin it as an explicit matrix case.
func TestRecoverDeadHost_UnknownHostIsUntouchedInMatrix(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	lockPath := HostLockPath(store.dataDir, "weird-host")
	require.NoError(t, os.MkdirAll(lockPath, 0o755))
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "weird-host", "", true)

	_, err := store.RecoverDeadHost(ctx, "weird-host", nil)
	require.Error(t, err)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
}

// TestRecoverDeadHost_CreatesNoMessages pins DUR-6: recovery never writes
// session history and never wakes anyone. Message counts for BOTH the owner
// and the (read-only) child session are unchanged by recovery -- the only
// DB writes recovery makes are to async_jobs/async_hosts.
func TestRecoverDeadHost_CreatesNoMessages(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "child-1"))
	messages := message.NewService(db.New(store.sqlDB))
	finishedAssistantText(t, ctx, messages, "child-1", "child's answer")

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "parent-1", "call-1", "dead-host-1", "child-1", true)
	seedRunningJob(t, ctx, q, "parent-1", "call-2", "dead-host-1", "", true)

	parentBefore, err := messages.List(ctx, "parent-1")
	require.NoError(t, err)
	childBefore, err := messages.List(ctx, "child-1")
	require.NoError(t, err)

	outcome, err := store.RecoverDeadHost(ctx, "dead-host-1", messages)
	require.NoError(t, err)
	require.Equal(t, 2, outcome.Interrupted)

	parentAfter, err := messages.List(ctx, "parent-1")
	require.NoError(t, err)
	childAfter, err := messages.List(ctx, "child-1")
	require.NoError(t, err)
	require.Len(t, parentAfter, len(parentBefore), "recovery must never insert a history message for the owner")
	require.Len(t, childAfter, len(childBefore), "recovery must only READ the child's messages, never write to it")

	row, err := store.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.False(t, row.NoticeMessageID.Valid, "recovery never pulls -- no notice_message_id is ever set by it")
}

// ============================================================
// SweepDeadHosts / RecoverOwnerScope.
// ============================================================

// TestSweepDeadHosts_RecoversEveryDeadHost_SkipsAliveAndOwn pins the
// host-level sweep (doc sec.3.6/3.7): recovers every dead host with a
// running row, skips a live one, and never probes this store's own host id.
func TestSweepDeadHosts_RecoversEveryDeadHost_SkipsAliveAndOwn(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	require.NoError(t, seedSession(ctx, q, "owner-2"))
	require.NoError(t, seedSession(ctx, q, "owner-3"))

	// This store's own host: claim something to register it, so its id would
	// otherwise be a sweep candidate too (it owns a running row) -- must be
	// skipped, never self-probed.
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-3", ToolCallID: "own-call", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", true)

	holder, err := TryAcquireFileLock(HostLockPath(store.dataDir, "live-host-1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "live-host-1", Pid: 1, Label: "live", StartedAt: 1700000000})
	require.NoError(t, err)
	seedRunningJob(t, ctx, q, "owner-2", "call-2", "live-host-1", "", true)

	outcomes, err := store.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, outcomes, "dead-host-1")
	require.NotContains(t, outcomes, "live-host-1")
	require.NotContains(t, outcomes, store.HostID())

	row1, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row1.State)
	row2, err := store.Get(ctx, "owner-2", "call-2")
	require.NoError(t, err)
	require.Equal(t, "running", row2.State, "the live host's row must be untouched")
	rowOwn, err := store.Get(ctx, "owner-3", "own-call")
	require.NoError(t, err)
	require.Equal(t, "running", rowOwn.State, "this store's own host's row must never be self-recovered")
}

// TestRecoverOwnerScope_RecoversOnlyOwnersDeadHostRows pins own-scope
// recovery (doc sec.3.5): recovers every dead host referenced by owner's
// OWN running rows, deduplicated per host, and never touches a different
// owner's rows on the SAME dead host beyond what that host's own recovery
// naturally does (recovery is per-host, not per-owner, by design -- see
// RecoverDeadHost's own doc).
func TestRecoverOwnerScope_RecoversOnlyOwnersDeadHostRows(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	require.NoError(t, seedSession(ctx, q, "owner-2"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-host-1")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-host-1", "", true)
	seedRunningJob(t, ctx, q, "owner-1", "call-2", "dead-host-1", "", true)

	holder, err := TryAcquireFileLock(HostLockPath(store.dataDir, "live-host-1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "live-host-1", Pid: 1, Label: "live", StartedAt: 1700000000})
	require.NoError(t, err)
	seedRunningJob(t, ctx, q, "owner-2", "call-3", "live-host-1", "", true)

	store.RecoverOwnerScope(ctx, "owner-1", nil)

	row1, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row1.State)
	row2, err := store.Get(ctx, "owner-1", "call-2")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row2.State)
	row3, err := store.Get(ctx, "owner-2", "call-3")
	require.NoError(t, err)
	require.Equal(t, "running", row3.State, "a different owner's row on a LIVE host must never be touched by this owner's scope recovery")
}

// ============================================================
// Retention.
// ============================================================

// TestPurgeExpired_OldDoneRowsPurged_RecentAndLiveKept pins doc sec.3.7's
// retention pass: a terminal, delivered-or-voided row past the cutoff is
// deleted; a recent one, and a still-running one, are kept.
func TestPurgeExpired_OldDoneRowsPurged_RecentAndLiveKept(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	recent := time.Now().Add(-1 * time.Hour).Unix()

	// An old, terminal, delivered row -- must be purged.
	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "old-done", Kind: "command", ToolName: "bash",
		InputHash: "h1", HostID: "gone-host", CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", Delivery: "done", NoticeKind: "", ResultSummary: sql.NullString{String: "x", Valid: true},
		ResultIsError: sql.NullInt64{Valid: true}, Wake: 0, UpdatedAt: old, OwnerSessionID: "owner-1", ToolCallID: "old-done",
	})
	require.NoError(t, err)

	// A RECENT, terminal, delivered row -- must survive.
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "recent-done", Kind: "command", ToolName: "bash",
		InputHash: "h2", HostID: "gone-host", CreatedAt: recent, UpdatedAt: recent,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", Delivery: "done", NoticeKind: "", ResultSummary: sql.NullString{String: "y", Valid: true},
		ResultIsError: sql.NullInt64{Valid: true}, Wake: 0, UpdatedAt: recent, OwnerSessionID: "owner-1", ToolCallID: "recent-done",
	})
	require.NoError(t, err)

	// A STILL-RUNNING row, old updated_at -- must survive (state='running'
	// is excluded from the purge predicate regardless of age).
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "old-running", Kind: "command", ToolName: "bash",
		InputHash: "h3", HostID: "gone-host", CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)

	// A5 (A-b test fix): an OLD, terminal, delivered='done' row that is STILL
	// UNREACTED DEBT (wake=1, reacted=0) must survive too -- it is not yet
	// "just old history", it is an owed reaction the owner has not gotten to
	// yet. Without the purge predicate's NOT(wake=1 AND reacted=0) guard, a
	// slow/stuck owner would silently lose its own obligation forever.
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "old-debt", Kind: "command", ToolName: "bash",
		InputHash: "h4", HostID: "gone-host", CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", Delivery: "done", NoticeKind: "", ResultSummary: sql.NullString{String: "z", Valid: true},
		ResultIsError: sql.NullInt64{Valid: true}, Wake: 1, Reacted: 0, UpdatedAt: old, OwnerSessionID: "owner-1", ToolCallID: "old-debt",
	})
	require.NoError(t, err)

	require.NoError(t, store.PurgeExpired(ctx, 7*24*time.Hour))

	_, err = store.Get(ctx, "owner-1", "old-done")
	require.ErrorIs(t, err, sql.ErrNoRows, "an old, delivered, terminal row must be purged")
	_, err = store.Get(ctx, "owner-1", "recent-done")
	require.NoError(t, err, "a recent delivered row must survive the same pass")
	_, err = store.Get(ctx, "owner-1", "old-running")
	require.NoError(t, err, "a still-running row must never be purged regardless of age")
	_, err = store.Get(ctx, "owner-1", "old-debt")
	require.NoError(t, err, "an old row that is still unreacted debt must never be purged")
}

// TestPurgeExpired_RemovesEmptyDeadHostFiles_KeepsLiveAndNonEmpty pins doc
// sec.3.7's host-file retention: a dead host with zero referencing rows has
// its file/row reaped; a live host with zero rows (e.g. an idle web server)
// is left alone; a dead host that STILL has rows is left alone too.
func TestPurgeExpired_RemovesEmptyDeadHostFiles_KeepsLiveAndNonEmpty(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	// Dead, empty -- must be reaped.
	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-empty")

	// Dead, but still has a row -- must survive this pass.
	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-with-rows")
	seedRunningJob(t, ctx, q, "owner-1", "call-1", "dead-with-rows", "", true)

	// Live, empty -- must survive (a live host with no CURRENT jobs is not
	// "dead", regardless of ListAsyncHostsWithNoJobs including it as a
	// zero-jobs candidate).
	holder, err := TryAcquireFileLock(HostLockPath(store.dataDir, "live-empty"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "live-empty", Pid: 1, Label: "live", StartedAt: 1700000000})
	require.NoError(t, err)

	require.NoError(t, store.PurgeExpired(ctx, 7*24*time.Hour))

	_, err = q.GetAsyncHost(ctx, "dead-empty")
	require.ErrorIs(t, err, sql.ErrNoRows, "an empty dead host must be reaped")
	_, statErr := os.Stat(HostLockPath(store.dataDir, "dead-empty"))
	require.True(t, os.IsNotExist(statErr))

	_, err = q.GetAsyncHost(ctx, "dead-with-rows")
	require.NoError(t, err, "a dead host that still has rows must survive this pass")
	_, statErr = os.Stat(HostLockPath(store.dataDir, "dead-with-rows"))
	require.NoError(t, statErr)

	_, err = q.GetAsyncHost(ctx, "live-empty")
	require.NoError(t, err, "a LIVE host with zero current jobs must never be reaped")
	_, statErr = os.Stat(HostLockPath(store.dataDir, "live-empty"))
	require.NoError(t, statErr)
}

// TestSweepDeadHosts_UnstoppedRunningRowsYieldExactlyNFactsThenNone is the
// session-level half of the restart story (doc sec.6: "Stop сессии с N
// под-агентами ... после перезапуска — ни одного лишнего"): N sub-agent
// sessions each own a 'running' bash job on the SAME dead host that nobody
// stopped; a restart (this sweep) recovers EXACTLY N facts (interrupted,
// wake=0), and a SECOND sweep -- standing in for yet another restart cycle --
// must add nothing further. It performs NO Stop; the real-Stop proof (rows the
// Stop already ended are not re-labelled) is agent.TestSweepDeadHosts_
// RestartAfterStopAddsNoExtraFacts (coordinator_stop_restart_test.go), which
// needs coordinator.Cancel and so cannot live in this package.
func TestSweepDeadHosts_UnstoppedRunningRowsYieldExactlyNFactsThenNone(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	const n = 3
	hostID := "dead-host-stop-tree"
	fabricateDeadHost(t, ctx, q, store.dataDir, hostID)
	for i := 0; i < n; i++ {
		childID := fmt.Sprintf("stop-tree-child-%d", i)
		require.NoError(t, seedSession(ctx, q, childID))
		seedRunningJob(t, ctx, q, childID, "bash-call", hostID, "", true)
	}

	outcomes, err := store.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	total := 0
	for _, o := range outcomes {
		total += o.Interrupted
	}
	require.Equal(t, n, total, "exactly N facts, one per sub-agent -- no more, no fewer")

	for i := 0; i < n; i++ {
		childID := fmt.Sprintf("stop-tree-child-%d", i)
		row, err := store.Get(ctx, childID, "bash-call")
		require.NoError(t, err)
		require.Equal(t, "interrupted", row.State)
		require.EqualValues(t, 0, row.Wake, "a recovered row must never wake anyone")
	}

	// A second restart/sweep cycle must recover nothing further: the rows
	// are already terminal, so ListDistinctRunningHostIDs no longer even
	// names this host.
	outcomes2, err := store.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, outcomes2, "a later restart must add no extra facts over the first recovery")
}
