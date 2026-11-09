package agent

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type finalizationPublishProbe struct {
	message.Service
	message.FinalizationReader
	updates      atomic.Int32
	publications atomic.Int32
	sawCommitted atomic.Bool
}

func (p *finalizationPublishProbe) UpdateTx(ctx context.Context, tx *sql.Tx, candidate message.Message) (func(), error) {
	publish, err := p.Service.UpdateTx(ctx, tx, candidate)
	if err != nil {
		return nil, err
	}
	p.updates.Add(1)
	return func() {
		stored, readErr := p.Get(ctx, candidate.ID)
		p.sawCommitted.Store(readErr == nil && stored.IsFinished())
		p.publications.Add(1)
		publish()
	}, nil
}

func finalizationStoredScope(t *testing.T, env fakeEnv, id message.FinalizationIdentity) finalizationPublicationScope {
	t.Helper()
	scope := finalizationPublicationScope{
		store:    session.NewAgentFinalizationStore(env.conn),
		identity: session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: id.Key, Generation: int64(id.Generation)},
	}
	require.NoError(t, scope.store.Bind(t.Context(), scope.identity))
	return scope
}

func finalizationStoredCandidate(t *testing.T, env fakeEnv, id message.FinalizationIdentity) finalizationCandidate {
	t.Helper()
	candidate, err := env.messages.Create(t.Context(), "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "settled final"}},
	})
	require.NoError(t, err)
	require.NoError(t, candidate.MarkFinalizationCandidate(id))
	candidate.CheckpointGeneration = 1
	require.NoError(t, env.messages.Update(t.Context(), candidate))
	stored, err := env.messages.Get(t.Context(), candidate.ID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	return finalizationCandidate{messageID: candidate.ID, hash: hash}
}

func TestFinalizationPublicationCommitsBeforeItsTerminalEvent(t *testing.T) {
	env := testEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 2, Serial: 3}
	scope := finalizationStoredScope(t, env, id)
	_, err := scope.store.Admit(ctx, scope.identity, "completed-work")
	require.NoError(t, err)
	require.NoError(t, scope.store.Release(ctx, scope.identity, "completed-work"))
	candidate := finalizationStoredCandidate(t, env, id)
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	accepted, err := publishFinalizationCandidate(ctx, env.sessions, probe, scope, id, candidate)
	require.NoError(t, err)
	require.True(t, accepted.IsFinished())
	record, err := scope.store.Snapshot(ctx, scope.identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.EqualValues(t, id.Revision, record.Revision)
	require.Equal(t, candidate.messageID, record.CandidateMessageID)
	require.Equal(t, candidate.hash, record.CandidateHash)
	require.True(t, probe.sawCommitted.Load(), "terminal publication must observe the accepted row after commit")
	require.EqualValues(t, 1, probe.updates.Load())
	require.EqualValues(t, 1, probe.publications.Load())
	_, err = publishFinalizationCandidate(ctx, env.sessions, probe, scope, id, candidate)
	require.NoError(t, err, "a verified receipt is idempotent")
	require.EqualValues(t, 1, probe.updates.Load())
	require.EqualValues(t, 1, probe.publications.Load(), "re-reading an accepted receipt must not publish another terminal event")
}

func TestFinalizationPersistedCancelRefusesBeforeCandidateWrite(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	candidate := finalizationStoredCandidate(t, env, id)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), "session"))
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	_, err := publishFinalizationCandidate(t.Context(), env.sessions, probe, scope, id, candidate)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, probe.updates.Load())
	require.Zero(t, probe.publications.Load())
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
}

func TestFinalizationChangedCandidateRollsBackWithoutTerminalEvent(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	candidate := finalizationStoredCandidate(t, env, id)
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	candidate.hash = "different"
	_, err := publishFinalizationCandidate(t.Context(), env.sessions, probe, scope, id, candidate)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
	require.Zero(t, probe.updates.Load())
	require.Zero(t, probe.publications.Load())
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
}

func TestFinalizationPublicationRefusesDurableWorkAndStaleRevision(t *testing.T) {
	for _, release := range []bool{false, true} {
		name := "open-ticket"
		if release {
			name = "released-ticket-requires-fresh-composition"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
			scope := finalizationStoredScope(t, env, id)
			candidate := finalizationStoredCandidate(t, env, id)
			_, err := scope.store.Admit(t.Context(), scope.identity, "productive-write")
			require.NoError(t, err)
			if release {
				require.NoError(t, scope.store.Release(t.Context(), scope.identity, "productive-write"))
			} else {
				id.Revision = 1
				stored, getErr := env.messages.Get(t.Context(), candidate.messageID)
				require.NoError(t, getErr)
				require.NoError(t, stored.MarkFinalizationCandidate(id))
				require.NoError(t, env.messages.Update(t.Context(), stored))
			}
			reader, supported := env.messages.(message.FinalizationReader)
			require.True(t, supported)
			probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
			t.Cleanup(func() {
				if t.Failed() {
					record, readErr := scope.store.Snapshot(t.Context(), scope.identity)
					t.Logf("phase=durable-publication release=%t identity=%+v record=%+v read_error=%v updates=%d publications=%d", release, id, record, readErr, probe.updates.Load(), probe.publications.Load())
				}
			})
			_, err = publishFinalizationCandidate(t.Context(), env.sessions, probe, scope, id, candidate)
			require.ErrorIs(t, err, ErrFinalizationScopeChanged)
			require.Zero(t, probe.updates.Load())
			require.Zero(t, probe.publications.Load())
			stored, err := env.messages.Get(t.Context(), candidate.messageID)
			require.NoError(t, err)
			require.False(t, stored.IsFinished())
			record, err := scope.store.Snapshot(t.Context(), scope.identity)
			require.NoError(t, err)
			require.Equal(t, "executing", record.Phase)
		})
	}
}

type finalizationFailingUpdate struct {
	*finalizationPublishProbe
}

func (p *finalizationFailingUpdate) UpdateTx(context.Context, *sql.Tx, message.Message) (func(), error) {
	return nil, sql.ErrConnDone
}

func TestFinalizationMessageWriteFailureRollsBackDurableSeal(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	scope := finalizationStoredScope(t, env, id)
	candidate := finalizationStoredCandidate(t, env, id)
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	_, err := publishFinalizationCandidate(t.Context(), env.sessions, &finalizationFailingUpdate{probe}, scope, id, candidate)
	require.ErrorIs(t, err, sql.ErrConnDone)
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)
	require.Empty(t, record.CandidateMessageID)
	require.Empty(t, record.CandidateHash)
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
	require.Zero(t, probe.publications.Load())
	accepted, err := publishFinalizationCandidate(t.Context(), env.sessions, probe, scope, id, candidate)
	require.NoError(t, err, "rollback must leave the same candidate publishable")
	require.True(t, accepted.IsFinished())
	require.EqualValues(t, 1, probe.publications.Load())
}

func TestFinalizationPublicationRequiresMatchingDurableReceipts(t *testing.T) {
	for _, messageAccepted := range []bool{false, true} {
		name := "only-scope-accepted"
		if messageAccepted {
			name = "only-message-accepted"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
			scope := finalizationStoredScope(t, env, id)
			candidate := finalizationStoredCandidate(t, env, id)
			if messageAccepted {
				stored, err := env.messages.Get(t.Context(), candidate.messageID)
				require.NoError(t, err)
				require.NoError(t, stored.AcceptFinalizationCandidate(id, candidate.hash))
				require.NoError(t, env.messages.Update(t.Context(), stored))
			} else {
				writer, supported := env.sessions.(session.FinalizationTransaction)
				require.True(t, supported)
				require.NoError(t, writer.WithFinalizationTx(t.Context(), "session", func(tx *sql.Tx) error {
					return scope.store.SealTx(t.Context(), tx, scope.identity, 0, candidate.messageID, candidate.hash)
				}))
			}
			before, err := scope.store.Snapshot(t.Context(), scope.identity)
			require.NoError(t, err)
			reader, supported := env.messages.(message.FinalizationReader)
			require.True(t, supported)
			probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
			_, err = publishFinalizationCandidate(t.Context(), env.sessions, probe, scope, id, candidate)
			require.ErrorIs(t, err, ErrFinalizationScopeChanged)
			require.Zero(t, probe.updates.Load())
			require.Zero(t, probe.publications.Load())
			after, err := scope.store.Snapshot(t.Context(), scope.identity)
			require.NoError(t, err)
			require.Equal(t, before, after, "a receipt mismatch must roll back any attempted seal")
			stored, err := env.messages.Get(t.Context(), candidate.messageID)
			require.NoError(t, err)
			require.Equal(t, messageAccepted, stored.IsFinished())
		})
	}
}
