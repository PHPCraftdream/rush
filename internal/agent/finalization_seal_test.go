package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
	candidate := gate.Candidate{MessageID: "m", Hash: "h"}
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
	require.ErrorIs(t, err, gate.ErrFinalizationFactsUnavailable)
	_, err = sealFinalizationCandidate(t.Context(), testFinalizationExecution(), env.messages, id, candidate)
	require.ErrorIs(t, err, gate.ErrFinalizationFactsUnavailable)
	require.Equal(t, messages, messageCount())
	require.Equal(t, finalizations, finalizationCount())

	scope := finalizationStoredScope(t, env, id)
	require.EqualValues(t, 1, finalizationCount(), "the stored scope binds one record")
	mismatch := id
	mismatch.Key = "other"
	_, err = sealFinalizationCandidate(t.Context(), &finalize.FinalizationExecution{SessionID: "session", LogicalID: "logical", Scope: scope}, env.messages, mismatch, candidate)
	require.ErrorIs(t, err, gate.ErrFinalizationScopeChanged)
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
	execution.SessionID = "session"
	execution.Scope = finalizationStoredScope(t, env, id)
	execution.ScopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.Scope, id)
	stamp, err := execution.Gate.BeginCandidate(t.Context(), execution.Read)
	require.NoError(t, err)
	verdict, err := execution.Gate.TrySeal(t.Context(), stamp, candidate, execution.Read, func(ctx context.Context, c gate.Candidate) error {
		_, err := sealFinalizationCandidate(ctx, execution, env.messages, id, c)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, gate.Accepted, verdict)
	record, err := execution.Scope.Store.Snapshot(t.Context(), execution.Scope.Identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.Equal(t, candidate.MessageID, record.CandidateMessageID)
	require.Equal(t, candidate.Hash, record.CandidateHash)
	stored, err := env.messages.Get(t.Context(), candidate.MessageID)
	require.NoError(t, err)
	finish := stored.FinishPart()
	require.NotNil(t, finish)
	require.False(t, finish.Partial)
	require.Equal(t, message.FinishReasonEndTurn, finish.Reason)
	require.Equal(t, candidate.Hash, finish.FinalizationHash)
}

// Revert-check: a wrapper that maps a deterministic refusal to a hard error
// wedges the owner by latching gate.failure — the finalizationCompose and
// nil-failure assertions fail.
func TestFinalizationSealWrapperMapsRefusalsToScopeChanges(t *testing.T) {
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}
	env := testEnv(t)
	execution := testFinalizationExecution()
	execution.SessionID = "session"
	execution.Scope = finalizationStoredScope(t, env, id)
	execution.ScopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.Scope, id)
	_, err := execution.Scope.Store.Admit(t.Context(), execution.Scope.Identity, "productive-write")
	require.NoError(t, err)
	stamp, err := execution.Gate.BeginCandidate(t.Context(), execution.Read)
	require.NoError(t, err)
	verdict, err := execution.Gate.TrySeal(t.Context(), stamp, candidate, execution.Read, func(ctx context.Context, c gate.Candidate) error {
		_, err := sealFinalizationCandidate(ctx, execution, env.messages, id, c)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, gate.Compose, verdict)
	_, _, err = execution.Gate.WaitState(t.Context(), execution.Read)
	require.NoError(t, err, "a deterministic not-yet must never latch a gate failure")
	record, err := execution.Scope.Store.Snapshot(t.Context(), execution.Scope.Identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)
	stored, err := env.messages.Get(t.Context(), candidate.MessageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
	_, err = sealFinalizationCandidate(t.Context(), execution, env.messages, id, candidate)
	require.ErrorIs(t, err, gate.ErrFinalizationScopeChanged)
}

// Revert-check: a wrapper that treats persisted cancel as retryable
// scope-changed would re-enter composition after cancellation — the
// context.Canceled assertion fails.
func TestFinalizationSealWrapperMapsPersistedCancel(t *testing.T) {
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}
	env := testEnv(t)
	execution := testFinalizationExecution()
	execution.SessionID = "session"
	execution.Scope = finalizationStoredScope(t, env, id)
	execution.ScopeFacts = &sessionScopeStub{AgentFinalizationScopeFacts: session.AgentFinalizationScopeFacts{Complete: true}}
	candidate := finalizationStoredCandidate(t, env, execution.Scope, id)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), "session"))
	// The persisted cancel is the one expected change; the refused seal must
	// add nothing on top of it.
	before, err := execution.Scope.Store.Snapshot(t.Context(), execution.Scope.Identity)
	require.NoError(t, err)
	_, err = sealFinalizationCandidate(t.Context(), execution, env.messages, id, candidate)
	require.ErrorIs(t, err, context.Canceled)
	after, err := execution.Scope.Store.Snapshot(t.Context(), execution.Scope.Identity)
	require.NoError(t, err)
	require.Equal(t, before, after)
	stored, err := env.messages.Get(t.Context(), candidate.MessageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
}
