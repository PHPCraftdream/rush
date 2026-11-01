package session

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionCredentialRequirementMarkerRoundTripAndQueueVisibility(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()
	sess, err := svc.Create(ctx, "credential-marker")
	require.NoError(t, err)

	has, err := svc.HasSessionCredentialRequirement(ctx, sess.ID)
	require.NoError(t, err)
	assert.False(t, has)
	require.NoError(t, svc.StoreSessionCredentialRequirement(ctx, sess.ID))
	require.NoError(t, svc.StoreSessionCredentialRequirement(ctx, sess.ID))
	has, err = svc.HasSessionCredentialRequirement(ctx, sess.ID)
	require.NoError(t, err)
	assert.True(t, has)

	row, err := svc.GetRunQueueEntry(ctx, SessionCredentialMarkerRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, row)
	var markerCount int
	require.NoError(t, svc.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_run_queue WHERE session_id = ? AND id = ?`,
		sess.ID, SessionCredentialMarkerRowID(sess.ID)).Scan(&markerCount))
	assert.Equal(t, 1, markerCount, "storing the marker twice must remain idempotent")
	assert.Equal(t, "acked", row.Status)
	assert.True(t, row.TerminalFailure)
	assert.Empty(t, row.LeasedBy)
	assert.Zero(t, row.LeasedAt)
	assert.Zero(t, row.LeaseExpiresAt)
	assert.True(t, IsSessionCredentialRequirementCallData(row.CallData))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.CallData), &payload))
	assert.Equal(t, SessionCredentialRequirementKind, payload["kind"])
	assert.Equal(t, float64(SessionCredentialRequirementVersion), payload["version"])
	assert.Len(t, payload, 2, "marker must contain no credential data")
	hasOutstanding, err := svc.HasOutstandingRunQueueEntriesForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.False(t, hasOutstanding, "acked metadata marker is not outstanding work")

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "credential-marker-call", sess.ID, []byte(`{}`)))
	first, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "tester", time.Second)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "credential-marker-call", first.ID)
	second, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "tester", time.Second)
	require.NoError(t, err)
	assert.Nil(t, second, "metadata marker must not be leased")
	hasOutstanding, err = svc.HasOutstandingRunQueueEntriesForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.True(t, hasOutstanding, "only the call row remains outstanding")
}

func TestIsSessionCredentialRequirementCallData(t *testing.T) {
	if !IsSessionCredentialRequirementCallData(`{"kind":"session_credential_requirement","version":1}`) {
		t.Fatal("credential marker envelope must be recognized by pump guard")
	}
	if IsSessionCredentialRequirementCallData(`{"Prompt":"hello"}`) {
		t.Fatal("ordinary session call must not be recognized as metadata")
	}
	if IsSessionCredentialRequirementCallData(`{not json`) {
		t.Fatal("malformed JSON must not be recognized as credential metadata")
	}
}

func TestSessionCredentialRequirementMarkerPurgePreservesMarker(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()
	sess, err := svc.Create(ctx, "credential-marker-purge")
	require.NoError(t, err)
	require.NoError(t, svc.StoreSessionCredentialRequirement(ctx, sess.ID))
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "credential-marker-purge-call", sess.ID, []byte(`{}`)))
	purged, err := svc.PurgeQueuedWorkForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, purged.RunQueue)
	row, err := svc.GetRunQueueEntry(ctx, SessionCredentialMarkerRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, row)
	has, err := svc.HasSessionCredentialRequirement(ctx, sess.ID)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestCredentialMarkerRejectedByRealPumpExecutionStep(t *testing.T) {
	for _, state := range []string{"pending", "leased"} {
		t.Run(state, func(t *testing.T) {
			sqlDB, q := newTestDB(t)
			svc := NewService(q, sqlDB).(*service)
			ctx := t.Context()
			sess, err := svc.Create(ctx, "forged-marker-"+state)
			require.NoError(t, err)
			marker := `{"kind":"session_credential_requirement","version":1}`
			if state == "leased" {
				_, err = sqlDB.ExecContext(ctx,
					`INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, leased_by, leased_at, lease_expires_at, terminal_failure, created_at, updated_at) VALUES (?, ?, ?, 'leased', 0, 'marker-pump', 1, 4102444800, 0, 1, 1)`,
					"forged-credential-marker", sess.ID, marker)
				require.NoError(t, err)
			} else {
				require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "forged-credential-marker", sess.ID, []byte(marker)))
			}

			var calls atomic.Int32
			pump := NewRunQueuePump(RunQueuePumpConfig{
				Sessions: svc, Coordinator: credentialMarkerCountingCoordinator{calls: &calls},
				PumpInstanceID: "marker-pump",
			})
			pending, err := svc.ListPendingRunQueueEntries(ctx)
			require.NoError(t, err)
			if state == "pending" {
				require.Len(t, pending, 1)
				pump.processEntry(ctx, &pending[0])
				pump.workerWg.Wait()
			} else {
				leased, err := svc.GetRunQueueEntry(ctx, "forged-credential-marker")
				require.NoError(t, err)
				require.NotNil(t, leased)
				_, _, err = pump.executeEntrySync(ctx, leased, &admissionEntry{})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "metadata row")
				terminal, getErr := svc.GetRunQueueEntry(ctx, leased.ID)
				require.NoError(t, getErr)
				require.Nil(t, terminal)
			}
			row, getErr := svc.GetRunQueueEntry(ctx, "forged-credential-marker")
			require.NoError(t, getErr)
			if row != nil {
				assert.True(t, row.TerminalFailure, "metadata row must be terminally rejected")
			}
			assert.Zero(t, calls.Load(), "metadata payload must never reach the executor")
		})
	}
}

type credentialMarkerCountingCoordinator struct{ calls *atomic.Int32 }

func (c credentialMarkerCountingCoordinator) Run(context.Context, SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}

func TestForkSessionTx_PropagatesCredentialRequirementMarker(t *testing.T) {
	t.Run("source marker", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB).(*service)
		ctx := t.Context()
		src, err := svc.Create(ctx, "credential-marker-fork")
		require.NoError(t, err)
		require.NoError(t, svc.StoreSessionCredentialRequirement(ctx, src.ID))
		fork, _, err := svc.ForkSessionTx(ctx, src.ID, ForkOptions{NewID: "fork-with-marker"})
		require.NoError(t, err)
		has, err := svc.HasSessionCredentialRequirement(ctx, fork.ID)
		require.NoError(t, err)
		assert.True(t, has)
	})

	t.Run("marked ancestor", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB).(*service)
		ctx := t.Context()
		root, err := svc.Create(ctx, "credential-root")
		require.NoError(t, err)
		child, _, err := svc.ForkSessionTx(ctx, root.ID, ForkOptions{NewID: "credential-child", ParentID: root.ID})
		require.NoError(t, err)
		grandchild, _, err := svc.ForkSessionTx(ctx, child.ID, ForkOptions{NewID: "credential-grandchild", ParentID: child.ID})
		require.NoError(t, err)
		// The lineage is deliberately unmarked when both descendants are created.
		require.NoError(t, svc.StoreSessionCredentialRequirement(ctx, root.ID))
		for _, sourceID := range []string{root.ID, child.ID, grandchild.ID} {
			forkID := "credential-descendant-fork-" + sourceID
			fork, _, err := svc.ForkSessionTx(ctx, sourceID, ForkOptions{NewID: forkID})
			require.NoError(t, err)
			has, err := svc.HasSessionCredentialRequirement(ctx, fork.ID)
			require.NoError(t, err)
			assert.True(t, has, "root marker must propagate when forking root, child, or grandchild")
		}
	})

	t.Run("unmarked lineage", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB).(*service)
		ctx := t.Context()
		root, err := svc.Create(ctx, "clear-root")
		require.NoError(t, err)
		child, _, err := svc.ForkSessionTx(ctx, root.ID, ForkOptions{NewID: "clear-child", ParentID: root.ID})
		require.NoError(t, err)
		fork, _, err := svc.ForkSessionTx(ctx, child.ID, ForkOptions{NewID: "clear-fork"})
		require.NoError(t, err)
		has, err := svc.HasSessionCredentialRequirement(ctx, fork.ID)
		require.NoError(t, err)
		assert.False(t, has)
	})

	t.Run("marker read error fails closed", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB).(*service)
		ctx := t.Context()
		src, err := svc.Create(ctx, "marker-read-error")
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, `DROP TABLE session_run_queue`)
		require.NoError(t, err)
		_, _, err = svc.ForkSessionTx(ctx, src.ID, ForkOptions{NewID: "marker-read-error-fork"})
		require.Error(t, err)
		_, getErr := svc.Get(ctx, "marker-read-error-fork")
		require.Error(t, getErr, "transaction must roll back the incomplete fork")
	})

	t.Run("missing parent fails closed", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB).(*service)
		ctx := t.Context()
		src, err := svc.Create(ctx, "orphan")
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, `UPDATE sessions SET parent_session_id = 'missing-parent' WHERE id = ?`, src.ID)
		require.NoError(t, err)
		_, _, err = svc.ForkSessionTx(ctx, src.ID, ForkOptions{NewID: "orphan-fork"})
		require.Error(t, err)
		_, getErr := svc.Get(ctx, "orphan-fork")
		require.Error(t, getErr, "transaction must roll back the incomplete fork")
	})
}
