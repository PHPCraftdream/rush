package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newFinalizationCandidateMessage(t *testing.T, env fakeEnv, id message.FinalizationIdentity) message.Message {
	t.Helper()
	m, err := env.messages.Create(t.Context(), "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "provisional"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.MarkFinalizationCandidate(id))
	m.CheckpointGeneration = 1
	return m
}

func TestPersistFinalizationCandidateCommitsMessageAndIdentityTogether(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	m := newFinalizationCandidateMessage(t, env, id)

	persisted, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, id, m)
	require.NoError(t, err)
	require.Equal(t, m.ID, persisted.messageID)
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Equal(t, persisted.messageID, record.CandidateMessageID)
	require.Equal(t, persisted.hash, record.CandidateHash)
	require.EqualValues(t, 1, record.CandidateSerial)
	require.Equal(t, "executing", record.Phase, "a candidate is provisional, not accepted")
	stored, err := env.messages.Get(t.Context(), m.ID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
	require.NoError(t, stored.VerifyFinalizationCandidate(id, persisted.hash))

	accepted, err := publishFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, id, persisted.finalizationCandidate)
	require.NoError(t, err, "publication follows the candidate commit")
	require.True(t, accepted.IsFinished())
}

func TestPersistFinalizationCandidateStaleWritesNothing(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	m := newFinalizationCandidateMessage(t, env, id)
	_, err := scope.store.Admit(t.Context(), scope.identity, "late-work")
	require.NoError(t, err)

	_, err = persistFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, id, m)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Empty(t, record.CandidateMessageID)
	stored, err := env.messages.Get(t.Context(), m.ID)
	require.NoError(t, err)
	require.Nil(t, stored.FinishPart(), "the stale candidate must not reach the row")

	wrong := id
	wrong.Key = "other"
	_, err = persistFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, wrong, m)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
}

func TestPublicationRefusesCandidateIdentityOrHashMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*message.FinalizationIdentity, *finalizationCandidate){
		"serial":  func(id *message.FinalizationIdentity, _ *finalizationCandidate) { id.Serial = 9 },
		"hash":    func(_ *message.FinalizationIdentity, c *finalizationCandidate) { c.hash = "other" },
		"message": func(_ *message.FinalizationIdentity, c *finalizationCandidate) { c.messageID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
			scope := finalizationStoredScope(t, env, id)
			m := newFinalizationCandidateMessage(t, env, id)
			persisted, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, id, m)
			require.NoError(t, err)
			presented, candidate := id, persisted.finalizationCandidate
			mutate(&presented, &candidate)
			_, err = publishFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, presented, candidate)
			require.Error(t, err)
			stored, err := env.messages.Get(t.Context(), m.ID)
			require.NoError(t, err)
			require.False(t, stored.IsFinished(), "a mismatched presentation must not publish")
			record, err := scope.store.Snapshot(t.Context(), scope.identity)
			require.NoError(t, err)
			require.Equal(t, "executing", record.Phase)
		})
	}
}

func TestRecoverFinalizationCandidateResumesStepCounter(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 5}
	scope := finalizationStoredScope(t, env, id)
	m := newFinalizationCandidateMessage(t, env, id)
	_, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, scope, id, m)
	require.NoError(t, err)
	quiet := func(context.Context) (finalizationFacts, error) { return finalizationFacts{complete: true}, nil }

	restarted := newFinalizationGate(1)
	verdict, err := recoverFinalizationCandidate(t.Context(), env.messages, scope, restarted)
	require.NoError(t, err)
	require.Equal(t, session.AgentFinalizationCandidateAdopted, verdict)
	stamp, err := restarted.beginCandidate(t.Context(), quiet)
	require.NoError(t, err)
	require.EqualValues(t, 6, stamp.serial, "a restarted owner continues above the durable high-water mark")

	_, err = env.conn.ExecContext(t.Context(), `DELETE FROM messages WHERE id=?`, m.ID)
	require.NoError(t, err)
	failedGate := newFinalizationGate(1)
	verdict, err = recoverFinalizationCandidate(t.Context(), env.messages, scope, failedGate)
	require.NoError(t, err)
	require.Equal(t, session.AgentFinalizationCandidateFailed, verdict)
	stamp, err = failedGate.beginCandidate(t.Context(), quiet)
	require.NoError(t, err)
	require.EqualValues(t, 6, stamp.serial, "a failed candidate still raises the counter")
}

func finalizationCandidateStream(env fakeEnv, scope finalizationPublicationScope, m *message.Message) *turnStream {
	execution := testFinalizationExecution()
	execution.scope = scope
	return &turnStream{
		a:                &sessionAgent{sessions: env.sessions, messages: env.messages},
		genCtx:           context.Background(),
		ctx:              context.Background(),
		call:             SessionAgentCall{SessionID: "session", LogicalCallID: "logical"},
		currentAssistant: m,
		finalization:     execution,
	}
}

func TestPersistStepFinishRecordsFinalizationCandidate(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	m := newFinalizationCandidateMessage(t, env, id)
	ts := finalizationCandidateStream(env, scope, &m)

	require.NoError(t, ts.persistStepFinish())
	require.Equal(t, m.ID, ts.finalizationCandidate.messageID)
	require.Equal(t, id, ts.finalizationCandidate.identity)
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Equal(t, m.ID, record.CandidateMessageID)

	// A later step that is already stale records nothing and is not a turn error.
	_, err = scope.store.Admit(t.Context(), scope.identity, "work")
	require.NoError(t, err)
	next := newFinalizationCandidateMessage(t, env, message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 2})
	ts.currentAssistant = &next
	require.NoError(t, ts.persistStepFinish())
	require.Empty(t, ts.finalizationCandidate.messageID)
	record, err = scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Equal(t, m.ID, record.CandidateMessageID)
}

func TestPersistStepFinishFailsClosedWithoutDurableScope(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	m := newFinalizationCandidateMessage(t, env, id)
	ts := finalizationCandidateStream(env, finalizationPublicationScope{}, &m)
	require.ErrorIs(t, ts.persistStepFinish(), ErrFinalizationFactsUnavailable)
	stored, err := env.messages.Get(t.Context(), m.ID)
	require.NoError(t, err)
	require.Nil(t, stored.FinishPart())
}

func TestPersistStepFinishWithoutFinalizationKeepsPlainWrite(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	m := newFinalizationCandidateMessage(t, env, id)
	ts := finalizationCandidateStream(env, scope, &m)
	ts.finalization = nil

	require.NoError(t, ts.persistStepFinish())
	stored, err := env.messages.Get(t.Context(), m.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.FinishPart(), "with the feature off the step is written exactly as before")
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Empty(t, record.CandidateMessageID, "the feature-off path never touches the finalization record")
}
