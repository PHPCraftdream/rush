package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

type finalizationQueueFixture struct {
	env        *commitHookEnv
	service    *service
	ctx        context.Context
	trustedCtx context.Context
	data       SessionAgentCallData
	raw        []byte
	identities []AgentFinalizationIdentity
}

func newFinalizationQueueFixture(t *testing.T) finalizationQueueFixture {
	t.Helper()
	env := finalizationAncestorEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	env.conn.SetMaxOpenConns(1)
	svc := NewService(db.New(env.conn), env.conn).(*service)
	parent := finalizationCommitAuthority(t, svc, "owner", "parent-call", 1)
	child := finalizationCommitAuthority(t, svc, "child", "child-call", 1)
	_, err := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token=1 WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	grandchild, err := svc.BindAgentFinalization(ctx, AgentFinalizationIdentity{
		SessionID: "grandchild", LogicalCallID: "queue-call", Generation: 1, NodeID: "child-root.1", NodeToken: 1,
	})
	require.NoError(t, err)
	trusted, err := WithAgentFinalizationContext(ctx, parent, child, grandchild)
	require.NoError(t, err)
	data := SessionAgentCallData{
		SessionID: "grandchild", LogicalCallID: "queue-call", Prompt: "compose settled results", AutoResumed: true,
		MaxCost: 2, MaxTokens: 100, RequestedModelRole: "reviewer",
		FinalizationRequirement: CaptureAgentFinalizationContext(trusted).Requirement(),
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	identities := []AgentFinalizationIdentity{parent.identity, child.identity, grandchild.identity}
	finalizationCommitFailureDiagnostics(t, env, identities...)
	return finalizationQueueFixture{env: env, service: svc, ctx: ctx, trustedCtx: trusted, data: data, raw: raw, identities: identities}
}

func (f finalizationQueueFixture) enqueue(t *testing.T, rowID string) {
	t.Helper()
	require.NoError(t, f.service.EnqueueRunQueueEntry(f.trustedCtx, rowID, f.data.SessionID, f.raw))
}

func (f finalizationQueueFixture) lease(t *testing.T, pump string) *RunQueueEntry {
	t.Helper()
	entry, err := f.service.LeaseRunQueueEntry(f.ctx, f.data.SessionID, pump, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.NotNil(t, entry.finalizationLease)
	return entry
}

func TestFinalizationQueueRestoresTrustedLeasedContinuationOnce(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	fixture.enqueue(t, "queued") // Same immutable row remains idempotent.
	entry := fixture.lease(t, "pump")
	// A reconstructed service has no original authority objects or context.
	restoredService := NewService(db.New(fixture.env.conn), fixture.env.conn).(AgentFinalizationQueueService)
	leaseCtx := withFinalizationQueueLease(fixture.ctx, entry)
	snapshot, err := restoredService.RestoreAgentFinalizationQueue(leaseCtx, entry.ID, fixture.data)
	require.NoError(t, err)
	require.Equal(t, fixture.data.FinalizationRequirement, snapshot.Requirement())
	require.True(t, snapshot.RequiresManagedDriver())
	ctx, err := snapshot.RestoreCall(fixture.ctx, fixture.data.SessionID, fixture.data.LogicalCallID)
	require.NoError(t, err)
	require.NoError(t, NewSessionNoticeStore(fixture.env.conn).InsertSessionNotice(ctx, "grandchild", NoticeKindWakeOnly, "settled", true, ""))
	_, err = restoredService.RestoreAgentFinalizationQueue(leaseCtx, entry.ID, fixture.data)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "the same proof cannot dispatch a second call")
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
	for _, identity := range fixture.identities {
		state, err := NewAgentFinalizationStore(fixture.env.conn).Snapshot(fixture.ctx, identity)
		require.NoError(t, err)
		require.Zero(t, state.OpenTickets)
		require.Equal(t, "executing", state.Phase)
	}
}

func TestFinalizationQueueRawIDsCannotCreateProvenance(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	err := fixture.service.EnqueueRunQueueEntry(fixture.ctx, "forged", fixture.data.SessionID, fixture.raw)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue`))
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_agent_finalization_continuations`))
	// An unsupported raw outbox/interrupt writer cannot manufacture provenance.
	now := time.Now().Unix()
	_, err = db.New(fixture.env.conn).EnqueueRunQueueEntry(fixture.ctx, db.EnqueueRunQueueEntryParams{
		ID: "raw-outbox", SessionID: fixture.data.SessionID, CallData: string(fixture.raw), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	entry, err := fixture.service.LeaseRunQueueEntry(fixture.ctx, fixture.data.SessionID, "pump", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Nil(t, entry.finalizationLease)
	_, err = fixture.service.RestoreAgentFinalizationQueue(withFinalizationQueueLease(fixture.ctx, entry), entry.ID, fixture.data)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
}

func TestFinalizationQueueDeclarationMustMatchEveryTrustedIdentityField(t *testing.T) {
	for _, field := range []string{"session", "logical", "generation", "node", "token", "version"} {
		t.Run(field, func(t *testing.T) {
			fixture := newFinalizationQueueFixture(t)
			requirement := fixture.data.FinalizationRequirement
			for i := range requirement.Authorities {
				identity := &requirement.Authorities[i]
				if identity.SessionID != "grandchild" {
					continue
				}
				switch field {
				case "session":
					identity.SessionID = "unrelated"
				case "logical":
					identity.LogicalCallID = "unrelated"
				case "generation":
					identity.Generation++
				case "node":
					identity.NodeID = "other.1"
				case "token":
					identity.NodeToken++
				case "version":
					requirement.Version++
				}
			}
			raw, err := json.Marshal(fixture.data)
			require.NoError(t, err)
			err = fixture.service.EnqueueRunQueueEntry(fixture.trustedCtx, "mismatch", fixture.data.SessionID, raw)
			require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue`))
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_agent_finalization_continuations`))
		})
	}
}

func TestFinalizationQueueRejectsTamperStaleScopeAndAncestorChange(t *testing.T) {
	for _, change := range []string{"payload", "caller_data", "accepted", "replaced", "ancestor"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFinalizationQueueFixture(t)
			fixture.enqueue(t, "queued")
			entry := fixture.lease(t, "pump")
			data := fixture.data
			switch change {
			case "payload":
				tampered := data
				tampered.Prompt = "replay original work"
				raw, err := json.Marshal(tampered)
				require.NoError(t, err)
				_, err = fixture.env.conn.ExecContext(fixture.ctx, `UPDATE session_run_queue SET call_data=? WHERE id='queued'`, string(raw))
				require.NoError(t, err)
			case "caller_data":
				data.MaxCost = 200
			case "accepted":
				identity := fixture.identities[2]
				state, err := NewAgentFinalizationStore(fixture.env.conn).Snapshot(fixture.ctx, identity)
				require.NoError(t, err)
				require.NoError(t, sealFinalizationForTest(fixture.ctx, fixture.env.conn, NewAgentFinalizationStore(fixture.env.conn), identity, state.Revision))
			case "replaced":
				_, err := fixture.env.conn.ExecContext(fixture.ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='grandchild'`)
				require.NoError(t, err)
				_, err = fixture.service.BindAgentFinalization(fixture.ctx, AgentFinalizationIdentity{SessionID: "grandchild", LogicalCallID: "replacement", Generation: 2})
				require.NoError(t, err)
			case "ancestor":
				_, err := fixture.env.conn.ExecContext(fixture.ctx, `UPDATE batch_nodes SET token=token+1 WHERE node_id='parent-root.1'`)
				require.NoError(t, err)
			}
			_, err := fixture.service.RestoreAgentFinalizationQueue(withFinalizationQueueLease(fixture.ctx, entry), entry.ID, data)
			require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
		})
	}
}

func TestFinalizationQueueRequiresExactLiveLease(t *testing.T) {
	for _, change := range []string{"missing", "expired", "cross_entry", "replacement_same_pump"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFinalizationQueueFixture(t)
			fixture.enqueue(t, "queued")
			entry := fixture.lease(t, "pump")
			ctx := withFinalizationQueueLease(fixture.ctx, entry)
			rowID := entry.ID
			switch change {
			case "missing":
				ctx = WithRunQueueRowID(fixture.ctx, entry.ID) // Public IDs are not a lease capability.
			case "expired":
				_, err := fixture.env.conn.ExecContext(fixture.ctx, `UPDATE session_run_queue SET lease_expires_at=? WHERE id='queued'`, time.Now().Unix()-1)
				require.NoError(t, err)
			case "cross_entry":
				fixture.enqueue(t, "other")
				other := fixture.lease(t, "pump")
				rowID = other.ID
			case "replacement_same_pump":
				err := fixture.service.NackRunQueueEntry(fixture.ctx, entry.ID, "pump", "unstarted lease replaced")
				require.NoError(t, err)
				next := fixture.lease(t, "pump")
				require.NotEqual(t, entry.finalizationLease.token, next.finalizationLease.token)
			}
			_, err := fixture.service.RestoreAgentFinalizationQueue(ctx, rowID, fixture.data)
			require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_agent_finalization_continuations WHERE consumed<>0`))
		})
	}
}

func TestFinalizationQueueCrashConsumptionSurvivesQueueDeletion(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	entry := fixture.lease(t, "pump")
	_, err := fixture.service.RestoreAgentFinalizationQueue(withFinalizationQueueLease(fixture.ctx, entry), entry.ID, fixture.data)
	require.NoError(t, err)
	// No result was confirmed; a fresh lease cannot replay the consumed call.
	err = fixture.service.NackRunQueueEntry(fixture.ctx, entry.ID, "pump", "unknown execution after crash")
	require.NoError(t, err)
	next := fixture.lease(t, "new-pump")
	_, err = fixture.service.RestoreAgentFinalizationQueue(withFinalizationQueueLease(fixture.ctx, next), next.ID, fixture.data)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	require.NoError(t, fixture.service.TerminalFailRunQueueEntry(fixture.ctx, next.ID, "new-pump"))
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
	err = fixture.service.EnqueueRunQueueEntry(fixture.trustedCtx, "queued", fixture.data.SessionID, fixture.raw)
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "deleted queue rows cannot erase crash accounting")
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue`))
	state, err := NewAgentFinalizationStore(fixture.env.conn).Snapshot(fixture.ctx, fixture.identities[2])
	require.NoError(t, err)
	require.Equal(t, "executing", state.Phase, "unresolved is not accepted or completed")
}

func TestFinalizationQueueWriterAndLeaseRollbackPreserveProof(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	_, err := fixture.env.conn.ExecContext(fixture.ctx, `CREATE TRIGGER provenance_abort BEFORE INSERT ON session_agent_finalization_continuations
		BEGIN SELECT RAISE(ABORT, 'provenance rollback'); END`)
	require.NoError(t, err)
	err = fixture.service.EnqueueRunQueueEntry(fixture.trustedCtx, "queued", fixture.data.SessionID, fixture.raw)
	require.ErrorContains(t, err, "provenance rollback")
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue`))
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0`))
	_, err = fixture.env.conn.ExecContext(fixture.ctx, `DROP TRIGGER provenance_abort`)
	require.NoError(t, err)
	fixture.enqueue(t, "queued")
	_, err = fixture.env.conn.ExecContext(fixture.ctx, `CREATE TRIGGER lease_proof_abort BEFORE UPDATE OF lease_token ON session_agent_finalization_continuations
		BEGIN SELECT RAISE(ABORT, 'lease proof rollback'); END`)
	require.NoError(t, err)
	_, err = fixture.service.LeaseRunQueueEntry(fixture.ctx, fixture.data.SessionID, "pump", time.Minute)
	require.ErrorContains(t, err, "lease proof rollback")
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue WHERE status='pending'`))
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_agent_finalization_continuations WHERE lease_token='' AND consumed=0`))
}

func TestFinalizationQueueConsumptionCASAndCancellationFailClosed(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ignored_cas", true: "durable_cancel"}[cancelled], func(t *testing.T) {
			fixture := newFinalizationQueueFixture(t)
			fixture.enqueue(t, "queued")
			entry := fixture.lease(t, "pump")
			if cancelled {
				_, err := fixture.env.conn.ExecContext(fixture.ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='grandchild'`)
				require.NoError(t, err)
			} else {
				_, err := fixture.env.conn.ExecContext(fixture.ctx, `CREATE TRIGGER ignore_continuation_consume
					BEFORE UPDATE OF consumed ON session_agent_finalization_continuations BEGIN SELECT RAISE(IGNORE); END`)
				require.NoError(t, err)
			}
			snapshot, err := fixture.service.RestoreAgentFinalizationQueue(withFinalizationQueueLease(fixture.ctx, entry), entry.ID, fixture.data)
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrBatchProjectionConflict)
			}
			require.Nil(t, snapshot.Requirement(), "no capability escapes a refused writer transaction")
			require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
		})
	}
}

func TestFinalizationQueueLegacyCannotDowngradeNewlyBoundScope(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "bound_after_enqueue"}[bound], func(t *testing.T) {
			env := finalizationWriterEnv(t)
			svc := NewService(db.New(env.conn), env.conn).(*service)
			data := SessionAgentCallData{SessionID: "owner", LogicalCallID: "legacy-call", Prompt: "legacy"}
			raw, err := json.Marshal(data)
			require.NoError(t, err)
			require.NoError(t, svc.EnqueueRunQueueEntry(env.ctx, "legacy", "owner", raw))
			if bound {
				_, err = svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "legacy-call", Generation: 1})
				require.NoError(t, err)
			}
			entry, err := svc.LeaseRunQueueEntry(env.ctx, "owner", "pump", time.Minute)
			require.NoError(t, err)
			require.NotNil(t, entry)
			snapshot, err := svc.RestoreAgentFinalizationQueue(withFinalizationQueueLease(env.ctx, entry), entry.ID, data)
			if bound {
				require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			} else {
				require.NoError(t, err)
				require.Nil(t, snapshot.Requirement())
			}
		})
	}
}
