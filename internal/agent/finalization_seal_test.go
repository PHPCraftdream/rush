package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// sessionScopeStub installs fixed scope facts for wrapper tests.
type sessionScopeStub struct {
	session.AgentFinalizationScopeFacts
}

func (s *sessionScopeStub) FinalizationSealScopeFacts(context.Context, *sql.Tx) (session.AgentFinalizationScopeFacts, error) {
	return s.AgentFinalizationScopeFacts, nil
}

// Revert-check: a wrapper reachable without an installed finalization scope
// (fail-open) or a default that seals on missing facts fails this test.
func TestFinalizationSealWrapperIsInertWithoutScope(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	candidate := finalizationCandidate{messageID: "m", hash: "h"}
	messageCount := func() int64 {
		var n int64
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT count(*) FROM messages WHERE session_id='session'`).Scan(&n))
		return n
	}
	finalizationCount := func() int64 {
		var n int64
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT count(*) FROM session_agent_finalizations`).Scan(&n))
		return n
	}
	messages, finalizations := messageCount(), finalizationCount()

	_, err := sealFinalizationCandidate(t.Context(), nil, env.messages, id, candidate)
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	_, err = sealFinalizationCandidate(t.Context(), testFinalizationExecution(), env.messages, id, candidate)
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	require.Equal(t, messages, messageCount())
	require.Equal(t, finalizations, finalizationCount())

	scope := finalizationStoredScope(t, env, id)
	require.EqualValues(t, 1, finalizationCount(), "the stored scope binds one record")
	mismatch := id
	mismatch.Key = "other"
	_, err = sealFinalizationCandidate(t.Context(), &finalizationExecution{sessionID: "session", logicalID: "logical", scope: scope}, env.messages, mismatch, candidate)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
	require.Equal(t, messages, messageCount())
	require.EqualValues(t, 1, finalizationCount())

	require.Nil(t, newTurnStream(turnStreamConfig{
		call: SessionAgentCall{SessionID: "session", LogicalCallID: "logical"},
	}).finalization, "no installed finalization scope means nothing can reach the wrapper")
}

// Revert-check: a wrapper that reports success without a durable accepted
// record, or the gate composition that lets a refused seal mark the owner
// sealed, fails.
func TestFinalizationSealWrapperSealsThroughGate(t *testing.T) {
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}
	env := testEnv(t)
	execution := testFinalizationExecution()
	execution.sessionID = "session"
	execution.scope = finalizationStoredScope(t, env, id)
	execution.scopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.scope, id)
	stamp, err := execution.gate.beginCandidate(t.Context(), execution.read)
	require.NoError(t, err)
	verdict, err := execution.gate.trySeal(t.Context(), stamp, candidate, execution.read, func(ctx context.Context, c finalizationCandidate) error {
		_, err := sealFinalizationCandidate(ctx, execution, env.messages, id, c)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, finalizationAccepted, verdict)
	record, err := execution.scope.store.Snapshot(t.Context(), execution.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.Equal(t, candidate.messageID, record.CandidateMessageID)
	require.Equal(t, candidate.hash, record.CandidateHash)
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	finish := stored.FinishPart()
	require.NotNil(t, finish)
	require.False(t, finish.Partial)
	require.Equal(t, message.FinishReasonEndTurn, finish.Reason)
	require.Equal(t, candidate.hash, finish.FinalizationHash)
}

// Revert-check: a wrapper that maps a deterministic refusal to a hard error
// wedges the owner by latching gate.failure — the finalizationCompose and
// nil-failure assertions fail.
func TestFinalizationSealWrapperMapsRefusalsToScopeChanges(t *testing.T) {
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}
	env := testEnv(t)
	execution := testFinalizationExecution()
	execution.sessionID = "session"
	execution.scope = finalizationStoredScope(t, env, id)
	execution.scopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.scope, id)
	_, err := execution.scope.store.Admit(t.Context(), execution.scope.identity, "productive-write")
	require.NoError(t, err)
	stamp, err := execution.gate.beginCandidate(t.Context(), execution.read)
	require.NoError(t, err)
	verdict, err := execution.gate.trySeal(t.Context(), stamp, candidate, execution.read, func(ctx context.Context, c finalizationCandidate) error {
		_, err := sealFinalizationCandidate(ctx, execution, env.messages, id, c)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict)
	require.NoError(t, execution.gate.failure, "a deterministic not-yet must never latch a gate failure")
	record, err := execution.scope.store.Snapshot(t.Context(), execution.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
	_, err = sealFinalizationCandidate(t.Context(), execution, env.messages, id, candidate)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
}

// Revert-check: a wrapper that treats persisted cancel as retryable
// scope-changed would re-enter composition after cancellation — the
// context.Canceled assertion fails.
func TestFinalizationSealWrapperMapsPersistedCancel(t *testing.T) {
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}
	env := testEnv(t)
	execution := testFinalizationExecution()
	execution.sessionID = "session"
	execution.scope = finalizationStoredScope(t, env, id)
	execution.scopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.scope, id)
	before, err := execution.scope.store.Snapshot(t.Context(), execution.scope.identity)
	require.NoError(t, err)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), "session"))
	_, err = sealFinalizationCandidate(t.Context(), execution, env.messages, id, candidate)
	require.ErrorIs(t, err, context.Canceled)
	after, err := execution.scope.store.Snapshot(t.Context(), execution.scope.identity)
	require.NoError(t, err)
	require.Equal(t, before, after)
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
}
