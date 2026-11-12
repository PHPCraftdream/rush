package session

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestQueueIngressTrustedAuthorityAndRefusals(t *testing.T) {
	for _, state := range []string{"trusted", "missing", "stale", "cancelled", "sealed"} {
		t.Run(state, func(t *testing.T) {
			f := newFinalizationQueueFixture(t)
			ctx := f.trustedCtx
			switch state {
			case "missing":
				ctx = f.ctx
			case "stale":
				_, err := f.env.conn.ExecContext(f.ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='child'`)
				require.NoError(t, err)
			case "cancelled":
				_, err := f.env.conn.ExecContext(f.ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='child'`)
				require.NoError(t, err)
			case "sealed":
				require.NoError(t, sealFinalizationForTest(f.ctx, f.env.conn, NewAgentFinalizationStore(f.env.conn), f.identities[1], 0))
			}
			err := f.service.EnqueueRunQueueEntry(ctx, "ingress-"+state, f.data.SessionID, f.raw)
			if state != "trusted" {
				require.Error(t, err)
				var rows int
				require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT count(*) FROM session_run_queue WHERE id=?`, "ingress-"+state).Scan(&rows))
				require.Zero(t, rows)
				return
			}
			require.NoError(t, err)
			var payloadHash, identities, ancestry string
			require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT payload_hash, identities, ancestry FROM session_agent_finalization_continuations WHERE row_id='ingress-trusted'`).Scan(&payloadHash, &identities, &ancestry))
			expectedHash, err := finalizationPayloadHash(f.data)
			require.NoError(t, err)
			expectedIdentities, err := canonicalFinalizationRequirement(f.data.FinalizationRequirement)
			require.NoError(t, err)
			require.Equal(t, expectedHash, payloadHash)
			require.Equal(t, expectedIdentities, identities)
			expectedAncestry, err := readFinalizationAncestry(f.ctx, f.env.conn, []string{f.data.SessionID})
			require.NoError(t, err)
			require.Equal(t, finalizationAncestryHash(expectedAncestry), ancestry)
		})
	}
}

func TestOutboxIngressTransfersProvenanceWithoutRevision(t *testing.T) {
	f := newFinalizationQueueFixture(t)
	require.NoError(t, f.service.WriteToOrphanOutbox(f.trustedCtx, "outbox-handoff", f.data.SessionID, f.raw))
	var beforeHash, beforeIdentities, beforeAncestry string
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT payload_hash, identities, ancestry FROM session_agent_finalization_continuations WHERE row_id='outbox-handoff'`).Scan(&beforeHash, &beforeIdentities, &beforeAncestry))
	before, err := NewAgentFinalizationStore(f.env.conn).Snapshot(f.ctx, f.identities[1])
	require.NoError(t, err)
	changed := f.data
	changed.Prompt = "different payload"
	changedRaw, err := json.Marshal(changed)
	require.NoError(t, err)
	require.ErrorIs(t, f.service.WriteToOrphanOutbox(f.trustedCtx, "outbox-handoff", f.data.SessionID, changedRaw), ErrAgentFinalizationChanged)

	drained, err := f.service.DrainOrphanOutboxEntry(f.ctx, "outbox-handoff")
	require.NoError(t, err)
	require.True(t, drained)
	var owner, raw, hash, identities, ancestry string
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT session_id, call_data FROM session_run_queue WHERE id='outbox-handoff'`).Scan(&owner, &raw))
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT payload_hash, identities, ancestry FROM session_agent_finalization_continuations WHERE row_id='outbox-handoff'`).Scan(&hash, &identities, &ancestry))
	require.Equal(t, f.data.SessionID, owner)
	require.Equal(t, string(f.raw), raw)
	require.Equal(t, beforeHash, hash)
	require.Equal(t, beforeIdentities, identities)
	require.Equal(t, beforeAncestry, ancestry)
	after, err := NewAgentFinalizationStore(f.env.conn).Snapshot(f.ctx, f.identities[1])
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Zero(t, after.OpenTickets)
}

func TestInterruptIngressRollbackPreservesSource(t *testing.T) {
	f := newFinalizationQueueFixture(t)
	_, err := db.New(f.env.conn).CreateMessage(f.ctx, db.CreateMessageParams{ID: "ingress-message", SessionID: f.data.SessionID, Role: "user", Parts: "[]"})
	require.NoError(t, err)
	inject := PendingInject{ID: "ingress-inject", SessionID: f.data.SessionID, MessageID: "ingress-message", Interrupt: true, CreatedAt: 9}
	require.NoError(t, f.service.CreatePendingInject(f.trustedCtx, inject))
	_, err = f.env.conn.ExecContext(f.ctx, `CREATE TRIGGER ingress_abort_provenance BEFORE INSERT ON session_agent_finalization_continuations
		BEGIN SELECT RAISE(ABORT, 'provenance rollback'); END`)
	require.NoError(t, err)
	_, err = f.service.ConsumeInterruptInjectAndEnqueue(f.trustedCtx, inject.SessionID, inject.ID, "interrupt-handoff", f.raw)
	require.ErrorContains(t, err, "provenance rollback")
	var sourceCount, queueCount, receiptCount int
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT count(*) FROM pending_injects WHERE id=?`, inject.ID).Scan(&sourceCount))
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT count(*) FROM session_run_queue WHERE id='interrupt-handoff'`).Scan(&queueCount))
	require.NoError(t, f.env.conn.QueryRowContext(f.ctx, `SELECT count(*) FROM session_agent_finalization_continuations WHERE row_id='interrupt-handoff'`).Scan(&receiptCount))
	require.Equal(t, 1, sourceCount)
	require.Zero(t, queueCount)
	require.Zero(t, receiptCount)
}

func TestUnboundQueueRetryCollisionAndLateBindingRollback(t *testing.T) {
	env := finalizationWriterEnv(t)
	svc := NewService(db.New(env.conn), env.conn)
	raw, err := json.Marshal(SessionAgentCallData{SessionID: "owner", Prompt: "legacy"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(env.ctx, "legacy-key", "owner", raw))
	other, err := json.Marshal(SessionAgentCallData{SessionID: "owner", Prompt: "different legacy retry"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(env.ctx, "legacy-key", "owner", other), "unbound legacy duplicate ID retains historical no-op behavior")
	require.ErrorIs(t, svc.EnqueueRunQueueEntry(env.ctx, "legacy-key", "different-owner", raw), ErrAgentFinalizationChanged)
	var provenance int
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT count(*) FROM session_agent_finalization_continuations WHERE row_id='legacy-key'`).Scan(&provenance))
	require.Zero(t, provenance)

	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER ingress_late_bind AFTER INSERT ON session_run_queue
		WHEN NEW.id='late-bind'
		BEGIN INSERT INTO session_agent_finalizations (session_id, logical_call_id, generation) VALUES ('owner', 'late-bound', 1); END`)
	require.NoError(t, err)
	err = svc.EnqueueRunQueueEntry(env.ctx, "late-bind", "owner", raw)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	var count int
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT count(*) FROM session_run_queue WHERE id='late-bind'`).Scan(&count))
	require.Zero(t, count)
}

func TestEmptyInjectDrainDoesNotChangeRevision(t *testing.T) {
	f := newFinalizationQueueFixture(t)
	before, err := NewAgentFinalizationStore(f.env.conn).Snapshot(f.ctx, f.identities[0])
	require.NoError(t, err)
	merged, interrupted, err := f.service.DrainPendingInjects(f.trustedCtx, f.data.SessionID)
	require.NoError(t, err)
	require.Empty(t, merged)
	require.False(t, interrupted)
	after, err := NewAgentFinalizationStore(f.env.conn).Snapshot(f.ctx, f.identities[0])
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Zero(t, after.OpenTickets)
}
