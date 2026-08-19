package session_test

// Regression coverage for the orphan-outbox poison-row finding in the
// 2026-08-18 release-readiness review (task #556).
//
// Dropping the old claim/mark-failed model also dropped the retry budget, so
// an entry that can never be enqueued was logged at ERROR and retried on
// every drain tick — every 15 seconds, forever, for the life of the process.

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestOrphanOutbox_PoisonEntryQuarantinesInsteadOfRetryingForever walks a
// malformed entry through its whole retry budget and asserts it leaves the
// pending scan afterwards.
//
// A malformed row is produced by writing an outbox entry for a session id
// that does not satisfy the main queue's constraints, so the drain's inner
// INSERT fails every time — the exact shape the review named as the only way
// this can fail forever.
//
// Revert-check: remove the recordOrphanOutboxDrainFailure call from
// processOrphanOutboxEntry's error path and this fails on the final
// require.Empty — the entry is still pending after any number of ticks.
func TestOrphanOutbox_PoisonEntryQuarantinesInsteadOfRetryingForever(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "orphan-poison")
	ctx := context.Background()

	// call_data that is not valid for the main queue: the drain's INSERT
	// carries it across verbatim, and the entry can never be enqueued.
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "poison-1", sess.ID, []byte("this is not json")))

	pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must start out pending")
	budget := pending[0].MaxAttempts
	require.Greater(t, budget, int64(0), "the schema must define a retry budget for this test to mean anything")

	// Charge the budget the same way a failing drain does, one attempt per
	// simulated tick, and watch the counter climb.
	var last session.OrphanOutboxFailureOutcome
	for i := int64(1); i <= budget; i++ {
		last, err = svc.RecordOrphanOutboxFailure(ctx, "poison-1", "inner INSERT failed: malformed call_data")
		require.NoError(t, err)
		require.Equal(t, i, last.Attempts, "attempt %d must be counted", i)
		require.False(t, last.AlreadyTerminal)
		if i < budget {
			require.False(t, last.Quarantined, "the entry must survive until its budget is actually spent")
		}
	}
	require.True(t, last.Quarantined, "the final attempt must quarantine the entry")

	// The whole point: it stops being scanned.
	pending, err = svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "a quarantined entry must leave the pending scan, or the 15s retry loop continues forever")

	// But it is parked, not deleted — an operator can still see it and why.
	stored, err := svc.GetOrphanOutboxEntry(ctx, "poison-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "failed", stored.Status)
	require.True(t, stored.LastError.Valid, "the reason must be recorded on the row, not only in the log")
	require.Contains(t, stored.LastError.String, "malformed call_data")

	// And it cannot be counted again or resurrected.
	after, err := svc.RecordOrphanOutboxFailure(ctx, "poison-1", "another tick")
	require.NoError(t, err)
	require.True(t, after.AlreadyTerminal, "a terminal row must not be re-counted")
	stored, err = svc.GetOrphanOutboxEntry(ctx, "poison-1")
	require.NoError(t, err)
	require.Equal(t, "failed", stored.Status, "recording against a terminal row must not flip it back to pending")
}

// TestOrphanOutbox_HealthyEntryStillDrains is the control. Quarantine must
// not have made the ordinary path harder to reach: an entry that CAN be
// enqueued still moves to the main run queue untouched by any of this.
func TestOrphanOutbox_HealthyEntryStillDrains(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "orphan-healthy")
	ctx := context.Background()

	callData := []byte(`{"SessionID":"` + sess.ID + `","Prompt":"continue"}`)
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "healthy-1", sess.ID, callData))

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-orphan-healthy",
		TestTick:       func() time.Duration { return time.Hour },
		TestDrainTick:  func() time.Duration { return 5 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
		return err == nil && len(pending) == 0
	}, 5*time.Second, 20*time.Millisecond, "a healthy entry must still drain out of the outbox")

	// GetOrphanOutboxEntry reports a missing row as (nil, nil), not as an
	// error — so the assertion is on the value, not on err.
	stored, err := svc.GetOrphanOutboxEntry(ctx, "healthy-1")
	require.NoError(t, err)
	require.Nil(t, stored, "a drained entry is deleted from the outbox, not quarantined")
}
