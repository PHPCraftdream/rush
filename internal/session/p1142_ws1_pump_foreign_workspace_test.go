package session_test

// WS-1 pump branch (task #1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1/§4 P0): when executing a
// run-queue row returns session.ErrForeignWorkspace, the pump must release it
// with NackRunQueueEntryNoAttemptPenalty — a nack WITHOUT an attempt penalty —
// and never with the ordinary NackRunQueueEntry (+1 attempt per cycle) or a
// terminal fail (row deleted). The row is not broken: it belongs to a
// different workspace, whose owner process will claim it. An ordinary nack
// would grow attempts on every tick and eventually dead-letter accepted work
// this pump was never allowed to run, and a terminal fail would delete another
// workspace's work outright.
//
// The scan-side half of the same invariant (ListPendingRunQueueEntries never
// even SEES a foreign row) is covered by TestListPendingRunQueueEntries_
// OwnershipFilter in session_workspace_test.go; the tests here cover the
// execution-side half, for the leak those filters cannot close by
// construction: a row leased before the filter existed, or a coordinator that
// refuses a row it was handed anyway.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// foreignWorkspaceCoordinator always returns an error wrapping
// session.ErrForeignWorkspace — the shape the WS-1 guard (agent.runOwned) or a
// coordinator refusal produces when asked to drive another workspace's
// session. Never succeeds.
type foreignWorkspaceCoordinator struct {
	calls atomic.Int64
}

func (c *foreignWorkspaceCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, fmt.Errorf("run refused in this workspace: %w", session.ErrForeignWorkspace)
}

// awaitPendingRunQueueRow waits until the row is observable as pending. A row
// mid-flight is briefly 'leased' and invisible to the pending scan, and
// GetRunQueueEntry reports the leased state, so polling (rather than one read)
// is the only race-free way to observe a settled outcome.
func awaitPendingRunQueueRow(t *testing.T, svc session.Service, id string) *session.RunQueueEntry {
	t.Helper()
	var got *session.RunQueueEntry
	require.Eventually(t, func() bool {
		e, err := svc.GetRunQueueEntry(context.Background(), id)
		if err != nil || e == nil {
			return false
		}
		if e.Status != session.RunQueueStatusPending {
			return false
		}
		got = e
		return true
	}, 20*time.Second, 20*time.Millisecond, "the pump must settle the row into pending (never delete it)")
	return got
}

// TestWS1_Pump_ForeignWorkspace_NackWithoutAttemptPenalty proves the P0-pump
// WS-1 branch survives far more pump ticks than RunQueueMaxAttempts: the row
// is released without an attempt penalty every single time, so the
// attempts-exhausted branch never dead-letters it and its owner process still
// finds it pending.
//
// NO EXTERNAL POKE: the test only starts a real RunQueuePump (20ms TestTick)
// against a fake coordinator and asserts the durable row's observable state.
//
// REVERT-CHECK PROCEDURE (mutant: the `errors.Is(err, ErrForeignWorkspace)`
// branch removed from run_queue_entry_exec.go, so the wrapped error falls
// through to the ordinary NackRunQueueEntry at the bottom of
// executeEntrySync):
//  1. Delete the ErrForeignWorkspace branch.
//  2. Run: go test ./internal/session/ -run
//     TestWS1_Pump_ForeignWorkspace_NackWithoutAttemptPenalty -v
//  3. FAIL at p1142_ws1_pump_foreign_workspace_test.go:120
//     (`require.Eventually(... coord.calls.Load() > RunQueueMaxAttempts+2)`):
//     with the branch gone the wrapped error takes the ordinary NackRunQueueEntry
//     at the bottom of executeEntrySync, attempts grows once per cycle, and the
//     attempts-exhausted branch terminal-fails (deletes) the row at attempts=10
//     -- observed in the run: "entry exceeded max attempts, terminal failing
//     ... attempts=10", coordinator call count frozen at 10, so the condition
//     is never satisfied. (The attempts==0 assertion at line 134 is the
//     same claim's row-state twin.)
//  4. Restore the branch and PASS.
func TestWS1_Pump_ForeignWorkspace_NackWithoutAttemptPenalty(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-foreign-workspace")
	ctx := context.Background()

	const entryID = "foreign-workspace-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, entryID, sess.ID, callDataJSON))

	coord := &foreignWorkspaceCoordinator{}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "foreign-workspace-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Wait for well over RunQueueMaxAttempts ticks. If the branch were
	// missing, the ordinary nack would increment attempts each cycle and the
	// attempts-exhausted branch would terminal-fail (delete) this row long
	// before this many calls. Waiting on the coordinator's OWN call counter,
	// never on "pending is empty" (a mid-flight lease reads identically to
	// gone through the pending scan).
	require.Eventually(t, func() bool {
		return coord.calls.Load() > int64(session.RunQueueMaxAttempts)+2
	}, 30*time.Second, 20*time.Millisecond,
		"the coordinator must be retried far past RunQueueMaxAttempts — a foreign-workspace "+
			"row is released without an attempt penalty, so it is never dead-lettered")

	// The refusal reason is observed WHILE the pump runs: Stop() below can catch
	// a cycle between its lease and its execution, and that cycle legitimately
	// releases the row with the pump's own shutdown message (still without an
	// attempt penalty), overwriting last_error. Asserting the reason only after
	// Stop failed in 3 of 25 loaded runs.
	require.Eventually(t, func() bool {
		e, getErr := svc.GetRunQueueEntry(ctx, entryID)
		return getErr == nil && e != nil && strings.Contains(e.LastError, "different workspace")
	}, 10*time.Second, 20*time.Millisecond, "the refusal reason must be recorded on the row")

	// Freeze the pump before reading the row's settled state, so no in-flight
	// lease can race the assertions below.
	pump.Stop()

	entry := awaitPendingRunQueueRow(t, svc, entryID)
	require.Equal(t, session.RunQueueStatusPending, entry.Status)
	require.Zero(t, entry.Attempts, "nack without attempt penalty: attempts must stay 0 across many ticks")
	require.NotEmpty(t, entry.LastError, "a release always records its reason on the row")
	require.True(t,
		strings.Contains(entry.LastError, "different workspace") ||
			entry.LastError == "run_queue_pump: shutting down, releasing lease without executing",
		"last_error must be the refusal or the pump's own shutdown release, got %q", entry.LastError)

	// The row must still be claimable by its OWNER process — proof the release
	// left it exactly as a fresh pending row, not as a dead-letter.
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "owner-process", 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased, "a foreign-workspace row must stay pending and leasable by its owner")
	require.Equal(t, entryID, leased.ID)
	require.EqualValues(t, 0, leased.Attempts)
	require.Equal(t, sess.ID, leased.SessionID, "the row must keep pointing at its own (foreign) session")
}

// blockingForeignCoordinator refuses with a wrapped ErrForeignWorkspace on its
// first call and then BLOCKS inside its second call until released (or the
// execution context dies). Blocking the second call freezes the row in the
// 'leased' state with its first cycle's outcome already written, so the test
// can read exactly one nack's effect with no timing window: the ordinary nack
// would have written attempts=1, the no-penalty nack writes attempts=0.
type blockingForeignCoordinator struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func newBlockingForeignCoordinator() *blockingForeignCoordinator {
	return &blockingForeignCoordinator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *blockingForeignCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	if c.calls.Add(1) == 1 {
		return nil, fmt.Errorf("run refused in this workspace: %w", session.ErrForeignWorkspace)
	}
	close(c.entered)
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	var result any = "ok"
	return &result, nil
}

// TestWS1_Pump_ForeignWorkspace_NoPenaltyIsTheWrite proves WHICH write the
// branch makes, not merely that the row survives: after exactly one
// foreign-workspace failure the row's attempts must still be 0 — the ordinary
// NackRunQueueEntry would have written 1.
//
// REVERT-CHECK PROCEDURE (mutant: the branch's
// NackRunQueueEntryNoAttemptPenalty swapped for the ordinary
// NackRunQueueEntry):
//  1. In the branch, call NackRunQueueEntry instead of
//     NackRunQueueEntryNoAttemptPenalty.
//  2. Run: go test ./internal/session/ -run
//     TestWS1_Pump_ForeignWorkspace_NoPenaltyIsTheWrite -v
//  3. FAIL at p1142_ws1_pump_foreign_workspace_test.go:234
//     (`require.Zero(t, entry.Attempts, ...)`): observed
//     "Should be zero, but was 1" -- the ordinary nack's +1 attempt lands on
//     the re-leased row that the blocked second call is holding.
//  4. Restore the no-penalty write and PASS.
func TestWS1_Pump_ForeignWorkspace_NoPenaltyIsTheWrite(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-foreign-workspace-write")
	ctx := context.Background()

	const entryID = "foreign-workspace-write-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, entryID, sess.ID, callDataJSON))

	coord := newBlockingForeignCoordinator()

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "foreign-workspace-write-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()

	// The second call is in flight, so the row is leased and stable: its first
	// cycle's outcome write has already landed and no further write can
	// happen until this test releases the coordinator.
	select {
	case <-coord.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the pump must retry the released row (second coordinator call never started)")
	}

	entry, err := svc.GetRunQueueEntry(ctx, entryID)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, session.RunQueueStatusLeased, entry.Status,
		"the pump must have re-leased the released row for its retry")
	require.Zero(t, entry.Attempts,
		"the foreign-workspace refusal must NOT increment attempts (one ordinary nack would leave 1)")
	require.NotEmpty(t, entry.LastError, "the refusal reason must be recorded on the row")
	require.Contains(t, entry.LastError, "different workspace")

	// Release the blocked retry: it succeeds and is acked, proving the release
	// left the row executable rather than terminally broken.
	close(coord.release)
	require.Eventually(t, func() bool {
		gone, err := runQueueGoneEverywhere(ctx, svc)
		return err == nil && gone
	}, 20*time.Second, 20*time.Millisecond,
		"the released foreign-workspace row must be retriable and then acked by its owner")

	pump.Stop()
	require.EqualValues(t, 2, coord.calls.Load(), "exactly one refusal plus one successful retry")
}
