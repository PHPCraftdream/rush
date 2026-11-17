package session

import (
	"context"
	"database/sql"
	"testing"

	session "github.com/PHPCraftdream/rush/internal/session/service"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

type candidateEnv struct {
	conn     *sql.DB
	store    *session.AgentFinalizationStore
	messages message.Service
	writer   message.FinalizationWriter
	reader   message.FinalizationReader
	id       session.AgentFinalizationIdentity
}

func newCandidateEnv(t *testing.T) candidateEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	messages := message.NewService(db.New(conn))
	store := session.NewAgentFinalizationStore(conn)
	id := session.AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	require.NoError(t, store.Bind(ctx, id))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	reader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	return candidateEnv{conn: conn, store: store, messages: messages, writer: writer, reader: reader, id: id}
}

// candidate creates an assistant row and marks it as the provisional candidate of step.
func (e candidateEnv) candidate(t *testing.T, text string, step message.FinalizationIdentity, checkpoint int64) message.Message {
	t.Helper()
	m, err := e.messages.Create(t.Context(), e.id.SessionID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	require.NoError(t, m.MarkFinalizationCandidate(step))
	m.CheckpointGeneration = checkpoint
	return m
}

func (e candidateEnv) step(revision, serial uint64) message.FinalizationIdentity {
	return message.FinalizationIdentity{Key: e.id.LogicalCallID, Generation: uint64(e.id.Generation), Revision: revision, Serial: serial}
}

// store runs StoreCandidateTx in its own transaction and commits only on success.
func (e candidateEnv) storeCandidate(t *testing.T, m message.Message, step message.FinalizationIdentity) (message.Message, error) {
	t.Helper()
	tx, err := e.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	stored, publish, err := e.store.StoreCandidateTx(t.Context(), tx, e.id, e.writer, m, step)
	if err != nil {
		return message.Message{}, err
	}
	require.NoError(t, tx.Commit())
	publish()
	return stored, nil
}

func (e candidateEnv) rowParts(t *testing.T, id string) string {
	t.Helper()
	var parts string
	require.NoError(t, e.conn.QueryRowContext(t.Context(), `SELECT parts FROM messages WHERE id=?`, id).Scan(&parts))
	return parts
}

func TestStoreCandidateTxPersistsMessageAndIdentityInOneTransaction(t *testing.T) {
	e := newCandidateEnv(t)
	m := e.candidate(t, "answer", e.step(0, 1), 1)
	before := e.rowParts(t, m.ID)

	tx, err := e.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	stored, _, err := e.store.StoreCandidateTx(t.Context(), tx, e.id, e.writer, m, e.step(0, 1))
	require.NoError(t, err)
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	var inTx string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT candidate_message_id FROM session_agent_finalizations WHERE session_id='owner'`).Scan(&inTx))
	require.Equal(t, m.ID, inTx)
	require.NoError(t, tx.Rollback())

	rolledBack, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Empty(t, rolledBack.CandidateMessageID, "a rolled-back message write must not leave a candidate record")
	require.Zero(t, rolledBack.CandidateSerial)
	require.Equal(t, before, e.rowParts(t, m.ID), "a rolled-back record write must not leave the candidate message")

	_, err = e.storeCandidate(t, m, e.step(0, 1))
	require.NoError(t, err)
	restarted, err := session.NewAgentFinalizationStore(e.conn).Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Equal(t, m.ID, restarted.CandidateMessageID)
	require.Equal(t, hash, restarted.CandidateHash)
	require.EqualValues(t, 1, restarted.CandidateSerial)
	require.Contains(t, e.rowParts(t, m.ID), "finalization_serial")
}

func TestStoreCandidateTxRejectsStaleGeneration(t *testing.T) {
	e := newCandidateEnv(t)
	m := e.candidate(t, "answer", e.step(0, 1), 1)
	before := e.rowParts(t, m.ID)

	// A step stamped with another generation never matches this record.
	other := e.step(0, 1)
	other.Generation = 2
	_, err := e.storeCandidate(t, m, other)
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)

	// The old incarnation stops being active when the next generation binds.
	_, err = e.conn.ExecContext(t.Context(), `UPDATE session_agent_finalizations SET phase='interrupted' WHERE session_id='owner'`)
	require.NoError(t, err)
	next := e.id
	next.Generation = 2
	require.NoError(t, e.store.Bind(t.Context(), next))
	_, err = e.storeCandidate(t, m, e.step(0, 1))
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged, "an older generation cannot write a candidate")

	require.Equal(t, before, e.rowParts(t, m.ID))
	for _, id := range []session.AgentFinalizationIdentity{e.id, next} {
		record, err := e.store.Snapshot(t.Context(), id)
		require.NoError(t, err)
		require.Empty(t, record.CandidateMessageID)
		require.Zero(t, record.CandidateSerial)
	}
}

func TestStoreCandidateTxRejectsOlderStepAfterNewer(t *testing.T) {
	e := newCandidateEnv(t)
	older := e.candidate(t, "older answer", e.step(0, 1), 1)
	newer := e.candidate(t, "newer answer", e.step(0, 2), 1)
	stored, err := e.storeCandidate(t, newer, e.step(0, 2))
	require.NoError(t, err)
	newerHash, err := stored.FinalizationContentHash()
	require.NoError(t, err)

	before := e.rowParts(t, older.ID)
	_, err = e.storeCandidate(t, older, e.step(0, 1))
	require.ErrorIs(t, err, session.ErrAgentFinalizationCandidateStale)
	require.Equal(t, before, e.rowParts(t, older.ID))
	record, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Equal(t, newer.ID, record.CandidateMessageID, "an older step must not overwrite the newer candidate")
	require.Equal(t, newerHash, record.CandidateHash)
	require.EqualValues(t, 2, record.CandidateSerial)

	_, err = e.storeCandidate(t, newer, e.step(0, 2))
	require.ErrorIs(t, err, session.ErrAgentFinalizationCandidateStale, "a step is recorded once; a replay is refused and the owner composes again")
}

func TestStoreCandidateTxRejectsStaleRevision(t *testing.T) {
	e := newCandidateEnv(t)
	m := e.candidate(t, "answer", e.step(0, 1), 1)
	before := e.rowParts(t, m.ID)
	_, err := e.store.Admit(t.Context(), e.id, "late-work")
	require.NoError(t, err)

	_, err = e.storeCandidate(t, m, e.step(0, 1))
	require.ErrorIs(t, err, session.ErrAgentFinalizationCandidateStale)
	require.Equal(t, before, e.rowParts(t, m.ID), "a stale candidate must not reach the message row")
	record, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Empty(t, record.CandidateMessageID)

	fresh := e.candidate(t, "answer", e.step(1, 2), 1)
	_, err = e.storeCandidate(t, fresh, e.step(1, 2))
	require.NoError(t, err, "the same owner may record a candidate composed at the current revision")
}

func TestStoreCandidateTxRejectsOlderCheckpointGenerationWithoutRecording(t *testing.T) {
	e := newCandidateEnv(t)
	m := e.candidate(t, "answer", e.step(0, 1), 1)
	_, err := e.conn.ExecContext(t.Context(), `UPDATE messages SET checkpoint_generation=5 WHERE id=?`, m.ID)
	require.NoError(t, err)
	_, err = e.storeCandidate(t, m, e.step(0, 1))
	require.ErrorIs(t, err, message.ErrFinalizationCandidateChanged)
	record, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Empty(t, record.CandidateMessageID)
}

func TestStoreCandidateTxRefusesCancelledAndForeignMessage(t *testing.T) {
	e := newCandidateEnv(t)
	require.NoError(t, seedSession(t.Context(), db.New(e.conn), "foreign"))
	foreign, err := e.messages.Create(t.Context(), "foreign", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "x"}}})
	require.NoError(t, err)
	require.NoError(t, foreign.MarkFinalizationCandidate(e.step(0, 1)))
	_, err = e.storeCandidate(t, foreign, e.step(0, 1))
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged, "a candidate must belong to the owner session")

	m := e.candidate(t, "answer", e.step(0, 1), 1)
	require.NoError(t, session.NewService(db.New(e.conn), e.conn).RequestCancel(t.Context(), "owner"))
	_, err = e.storeCandidate(t, m, e.step(0, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestRecoverCandidateAdoptsIntactAndFailsOutdated(t *testing.T) {
	adopt := func(t *testing.T, e candidateEnv) (session.AgentFinalizationRecovery, session.AgentFinalizationRecord) {
		t.Helper()
		verdict, record, err := session.NewAgentFinalizationStore(e.conn).RecoverCandidate(t.Context(), e.id, e.reader)
		require.NoError(t, err)
		return verdict, record
	}
	setup := func(t *testing.T) (candidateEnv, message.Message) {
		e := newCandidateEnv(t)
		m := e.candidate(t, "answer", e.step(0, 3), 1)
		_, err := e.storeCandidate(t, m, e.step(0, 3))
		require.NoError(t, err)
		return e, m
	}

	t.Run("no candidate", func(t *testing.T) {
		e := newCandidateEnv(t)
		verdict, _ := adopt(t, e)
		require.Equal(t, session.AgentFinalizationNoCandidate, verdict)
	})
	t.Run("intact candidate is adopted and stays sealable", func(t *testing.T) {
		e, m := setup(t)
		verdict, record := adopt(t, e)
		require.Equal(t, session.AgentFinalizationCandidateAdopted, verdict)
		require.Equal(t, m.ID, record.CandidateMessageID)
		require.EqualValues(t, 3, record.CandidateSerial)
		again, err := e.store.Snapshot(t.Context(), e.id)
		require.NoError(t, err)
		require.Equal(t, record, again, "adoption does not mutate the record")
	})
	failed := map[string]func(t *testing.T, e candidateEnv, m message.Message){
		"revision moved": func(t *testing.T, e candidateEnv, _ message.Message) {
			_, err := e.store.Admit(t.Context(), e.id, "work")
			require.NoError(t, err)
			require.NoError(t, e.store.Release(t.Context(), e.id, "work"))
		},
		"stored parts changed": func(t *testing.T, e candidateEnv, m message.Message) {
			res, err := e.conn.ExecContext(t.Context(), `UPDATE messages SET parts=replace(parts, 'answer', 'edited') WHERE id=?`, m.ID)
			require.NoError(t, err)
			n, err := res.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			require.Contains(t, e.rowParts(t, m.ID), "edited")
		},
		"message deleted": func(t *testing.T, e candidateEnv, m message.Message) {
			_, err := e.conn.ExecContext(t.Context(), `DELETE FROM messages WHERE id=?`, m.ID)
			require.NoError(t, err)
		},
	}
	for name, damage := range failed {
		t.Run("fails when "+name, func(t *testing.T) {
			e, m := setup(t)
			damage(t, e, m)
			verdict, record := adopt(t, e)
			require.Equal(t, session.AgentFinalizationCandidateFailed, verdict)
			require.Empty(t, record.CandidateMessageID)
			persisted, err := e.store.Snapshot(t.Context(), e.id)
			require.NoError(t, err)
			require.Empty(t, persisted.CandidateMessageID)
			require.Empty(t, persisted.CandidateHash)
			require.EqualValues(t, 3, persisted.CandidateSerial, "the high-water mark survives a failed candidate")
			verdict, _ = adopt(t, e)
			require.Equal(t, session.AgentFinalizationNoCandidate, verdict, "recovery is deterministic and repeatable")

			stale := e.candidate(t, "late", e.step(uint64(persisted.Revision), 2), 1)
			_, err = e.storeCandidate(t, stale, e.step(uint64(persisted.Revision), 2))
			require.ErrorIs(t, err, session.ErrAgentFinalizationCandidateStale, "a failed candidate cannot be resurrected by an older step")
		})
	}
	t.Run("non-active record is refused", func(t *testing.T) {
		e, _ := setup(t)
		_, err := e.conn.ExecContext(t.Context(), `UPDATE session_agent_finalizations SET phase='failed'`)
		require.NoError(t, err)
		_, _, err = e.store.RecoverCandidate(t.Context(), e.id, e.reader)
		require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)
	})
}

func TestSealTxRefusesCandidateIdentityOrHashMismatch(t *testing.T) {
	e := newCandidateEnv(t)
	m := e.candidate(t, "answer", e.step(0, 4), 1)
	stored, err := e.storeCandidate(t, m, e.step(0, 4))
	require.NoError(t, err)
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	good := session.AgentFinalizationCandidate{MessageID: m.ID, Hash: hash, Revision: 0, Serial: 4}
	seal := func(c session.AgentFinalizationCandidate) error {
		tx, err := e.conn.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer tx.Rollback()
		if err := e.store.SealTx(t.Context(), tx, e.id, c); err != nil {
			return err
		}
		return tx.Commit()
	}
	for name, mutate := range map[string]func(*session.AgentFinalizationCandidate){
		"hash":    func(c *session.AgentFinalizationCandidate) { c.Hash = "other" },
		"message": func(c *session.AgentFinalizationCandidate) { c.MessageID = "other" },
		"serial":  func(c *session.AgentFinalizationCandidate) { c.Serial = 3 },
		"newer":   func(c *session.AgentFinalizationCandidate) { c.Serial = 5 },
	} {
		bad := good
		mutate(&bad)
		require.ErrorIs(t, seal(bad), session.ErrAgentFinalizationChanged, name)
		record, err := e.store.Snapshot(t.Context(), e.id)
		require.NoError(t, err)
		require.Equal(t, "executing", record.Phase, name)
	}
	require.NoError(t, seal(good))
	require.ErrorIs(t, seal(good), session.ErrAgentFinalizationAccepted)
}

func TestSealTxRefusesWhenNoCandidateWasRecorded(t *testing.T) {
	e := newCandidateEnv(t)
	tx, err := e.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	err = e.store.SealTx(t.Context(), tx, e.id, session.AgentFinalizationCandidate{MessageID: "m", Hash: "h", Serial: 1})
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged, "publication needs a previously committed candidate")
}
