package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFinalizationQueuePreparationDoesNotConsumeOrAliasPayload(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	entry := fixture.lease(t, "pump")
	ctx := withFinalizationQueueLease(fixture.ctx, entry)
	prepared, err := fixture.service.PrepareAgentFinalizationQueue(ctx, entry.ID, fixture.data)
	require.NoError(t, err)
	require.True(t, prepared.RequiresManagedDriver())
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
	fixture.data.MaxCost = 200
	fixture.data.FinalizationRequirement.Authorities[0].Generation++
	snapshot, frozen, err := prepared.Consume(ctx)
	require.NoError(t, err)
	require.Equal(t, float64(2), frozen.MaxCost)
	require.Equal(t, snapshot.Requirement(), frozen.FinalizationRequirement)
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
}

func TestFinalizationQueuePreparationRechecksLeaseBeforeConsumption(t *testing.T) {
	for _, change := range []string{"expired", "replaced"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFinalizationQueueFixture(t)
			fixture.enqueue(t, "queued")
			entry := fixture.lease(t, "pump")
			ctx := withFinalizationQueueLease(fixture.ctx, entry)
			prepared, err := fixture.service.PrepareAgentFinalizationQueue(ctx, entry.ID, fixture.data)
			require.NoError(t, err)
			var fresh *RunQueueEntry
			switch change {
			case "expired":
				_, err = fixture.env.conn.ExecContext(fixture.ctx, `UPDATE session_run_queue SET lease_expires_at=? WHERE id='queued'`, time.Now().Unix()-1)
				require.NoError(t, err)
			case "replaced":
				require.NoError(t, fixture.service.NackRunQueueEntryNoAttemptPenalty(fixture.ctx, entry.ID, "pump", "not started"))
				fresh = fixture.lease(t, "new-pump")
			}
			_, _, err = prepared.Consume(ctx)
			require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			require.ErrorIs(t, err, ErrAgentFinalizationLeaseChanged)
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
			if fresh == nil {
				require.NoError(t, fixture.service.NackRunQueueEntryNoAttemptPenalty(fixture.ctx, entry.ID, "pump", "expired before acceptance"))
				fresh = fixture.lease(t, "new-pump")
			}
			freshCtx := withFinalizationQueueLease(fixture.ctx, fresh)
			next, err := fixture.service.PrepareAgentFinalizationQueue(freshCtx, fresh.ID, fixture.data)
			require.NoError(t, err)
			_, _, err = next.Consume(freshCtx)
			require.NoError(t, err)
		})
	}
}

func TestFinalizationQueuePreparationOnlyClearsUnconsumedRefusal(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	entry := fixture.lease(t, "pump")
	ctx := withFinalizationQueueLease(fixture.ctx, entry)
	require.NoError(t, recordFinalizationQueueFailure(fixture.ctx, entry, ErrAgentFinalizationDriverUnavailable))
	var code string
	require.NoError(t, fixture.env.conn.QueryRowContext(ctx, `SELECT failure_code FROM session_agent_finalization_continuations WHERE row_id='queued'`).Scan(&code))
	require.Equal(t, "continuation_driver_unavailable", code)
	prepared, err := fixture.service.PrepareAgentFinalizationQueue(ctx, entry.ID, fixture.data)
	require.NoError(t, err)
	_, _, err = prepared.Consume(ctx)
	require.NoError(t, err)
	require.NoError(t, fixture.env.conn.QueryRowContext(ctx, `SELECT failure_code FROM session_agent_finalization_continuations WHERE row_id='queued'`).Scan(&code))
	require.Empty(t, code, "only a verified zero-to-one admission clears a pre-admission refusal")
	_, _, err = prepared.Consume(ctx)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "consumed unknown execution is never reset")
}

func TestFinalizationQueuePreparationStopBeforeConsumeDoesNotWaitForWriter(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	entry := fixture.lease(t, "pump")
	ctx := withFinalizationQueueLease(fixture.ctx, entry)
	prepared, err := fixture.service.PrepareAgentFinalizationQueue(ctx, entry.ID, fixture.data)
	require.NoError(t, err)
	tx, err := fixture.env.conn.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = prepared.Consume(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, tx.Rollback())
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
}
