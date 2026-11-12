package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// recordCandidateForTest records a candidate identity directly, bypassing the
// message write, for tests that exercise only the seal.
func recordCandidateForTest(ctx context.Context, tx *sql.Tx, id AgentFinalizationIdentity, revision int64) AgentFinalizationCandidate {
	c := AgentFinalizationCandidate{MessageID: "candidate", Hash: "hash", Revision: revision, Serial: 1}
	_, _ = tx.ExecContext(ctx, `UPDATE session_agent_finalizations SET candidate_message_id=?, candidate_hash=?, candidate_serial=?
		WHERE session_id=? AND logical_call_id=? AND generation=?`, c.MessageID, c.Hash, c.Serial, id.SessionID, id.LogicalCallID, id.Generation)
	return c
}

func sealFinalizationForTest(ctx context.Context, conn *sql.DB, store *AgentFinalizationStore, id AgentFinalizationIdentity, revision int64) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	candidate := recordCandidateForTest(ctx, tx, id, revision)
	if err := store.SealTx(ctx, tx, id, candidate); err != nil {
		return err
	}
	return tx.Commit()
}

func TestAgentFinalizationDurableTicketsFenceSealAcrossStoreInstances(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	ctx := t.Context()
	owner := NewAgentFinalizationStore(conn)
	producer := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	require.NoError(t, owner.Bind(ctx, id))
	initial, err := owner.Snapshot(ctx, id)
	require.NoError(t, err)
	require.Zero(t, initial.Revision)
	held, err := producer.Admit(ctx, id, "mutation-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, held.OpenTickets)
	require.EqualValues(t, 1, held.Revision)
	retried, err := producer.Admit(ctx, id, "mutation-1")
	require.NoError(t, err)
	require.Equal(t, held, retried, "an admission retry does not create another physical hold")
	require.ErrorIs(t, sealFinalizationForTest(ctx, conn, owner, id, held.Revision), ErrAgentFinalizationChanged,
		"matching revision is insufficient while a persisted mutation is in flight")
	require.NoError(t, producer.Release(ctx, id, "mutation-1"))
	quiet, err := owner.Snapshot(ctx, id)
	require.NoError(t, err)
	require.Zero(t, quiet.OpenTickets)
	require.EqualValues(t, 2, quiet.Revision)
	require.NoError(t, producer.Release(ctx, id, "mutation-1"))
	duplicate, err := owner.Snapshot(ctx, id)
	require.NoError(t, err)
	require.Equal(t, quiet, duplicate, "duplicate completion does not decrement twice or advance revision")
	_, err = producer.Admit(ctx, id, "mutation-1")
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "a released ticket cannot be resurrected")
	require.ErrorIs(t, sealFinalizationForTest(ctx, conn, owner, id, initial.Revision), ErrAgentFinalizationChanged,
		"the pre-work candidate cannot become valid merely because work finished")
	require.NoError(t, sealFinalizationForTest(ctx, conn, owner, id, quiet.Revision))
	restarted := NewAgentFinalizationStore(conn)
	accepted, err := restarted.Snapshot(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "accepted", accepted.Phase)
	require.Equal(t, "candidate", accepted.CandidateMessageID)
	require.Equal(t, "hash", accepted.CandidateHash)
	require.ErrorIs(t, sealFinalizationForTest(ctx, conn, restarted, id, quiet.Revision), ErrAgentFinalizationAccepted)
	_, err = producer.Admit(ctx, id, "late-mutation")
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "another store instance cannot admit work after the seal")
}

func TestAgentFinalizationSealRollsBackWithPublicationTransaction(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "rollback", Generation: 1}
	require.NoError(t, store.Bind(t.Context(), id))
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, store.SealTx(t.Context(), tx, id, recordCandidateForTest(t.Context(), tx, id, 0)))
	require.NoError(t, tx.Rollback(), "a failed candidate publication must roll back the same authority seal")
	restarted := NewAgentFinalizationStore(conn)
	state, err := restarted.Snapshot(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "executing", state.Phase)
	require.Empty(t, state.CandidateMessageID)
	_, err = restarted.Admit(t.Context(), id, "after-known-rollback")
	require.NoError(t, err)
}

func TestAgentFinalizationOwnerAndBatchIncarnationCannotBeReplaced(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "first", Generation: 3, NodeID: "r.1", NodeToken: 4}
	require.NoError(t, store.Bind(t.Context(), id))
	require.NoError(t, store.Bind(t.Context(), id), "binding retry is idempotent")
	mismatch := id
	mismatch.NodeToken++
	require.ErrorIs(t, store.Bind(t.Context(), mismatch), ErrAgentFinalizationChanged)
	other := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "second", Generation: 1}
	require.Error(t, store.Bind(t.Context(), other), "a second active logical execution cannot replace the owner")
	require.NoError(t, sealFinalizationForTest(t.Context(), conn, store, id, 0))
	require.NoError(t, store.Bind(t.Context(), other), "a later logical execution has its own authority row")
	_, err := store.Admit(t.Context(), id, "old-callback")
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	state, err := store.Snapshot(t.Context(), other)
	require.NoError(t, err)
	require.Zero(t, state.Revision, "the old incarnation cannot mutate the next execution")
}

func TestAgentFinalizationRevisionOverflowRollsBackAdmission(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "overflow", Generation: 1}
	require.NoError(t, store.Bind(t.Context(), id))
	_, err := conn.ExecContext(t.Context(), "UPDATE session_agent_finalizations SET revision=9223372036854775807")
	require.NoError(t, err)
	_, err = store.Admit(t.Context(), id, "cannot-advance")
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	require.Zero(t, commitHookCount(t, conn, "SELECT count(*) FROM session_agent_finalization_tickets"))
	state, err := store.Snapshot(t.Context(), id)
	require.NoError(t, err)
	require.EqualValues(t, int64(9223372036854775807), state.Revision)
}

func TestAgentFinalizationSealRequiresActualWriterAcceptance(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "writer-noop", Generation: 1}
	require.NoError(t, store.Bind(t.Context(), id))
	_, err := conn.ExecContext(t.Context(), `CREATE TRIGGER finalization_seal_ignore
		BEFORE UPDATE OF phase ON session_agent_finalizations BEGIN SELECT RAISE(IGNORE); END`)
	require.NoError(t, err)
	require.ErrorIs(t, sealFinalizationForTest(t.Context(), conn, store, id, 0), ErrAgentFinalizationChanged)
	state, err := store.Snapshot(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "executing", state.Phase)
}

func TestAgentFinalizationPersistedCancelRefusesAdmissionAndSeal(t *testing.T) {
	_, conn, _ := batchStoreTestDB(t)
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "cancelled", Generation: 1}
	require.NoError(t, store.Bind(t.Context(), id))
	sessions := NewService(db.New(conn), conn)
	require.NoError(t, sessions.RequestCancel(t.Context(), "owner"))
	_, err := store.Admit(t.Context(), id, "after-cancel")
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, sealFinalizationForTest(t.Context(), conn, store, id, 0), context.Canceled)
	state, err := store.Snapshot(t.Context(), id)
	require.NoError(t, err)
	require.Zero(t, state.Revision)
	require.Zero(t, state.OpenTickets)
	require.NotEqual(t, "accepted", state.Phase)
}
