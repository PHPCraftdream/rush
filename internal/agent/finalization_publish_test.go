package agent

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
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
	candidate := finalizationStoredCandidate(t, env, id)
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	accepted, err := publishFinalizationCandidate(ctx, env.sessions, probe, "session", id, candidate)
	require.NoError(t, err)
	require.True(t, accepted.IsFinished())
	require.True(t, probe.sawCommitted.Load(), "terminal publication must observe the accepted row after commit")
	require.EqualValues(t, 1, probe.updates.Load())
	require.EqualValues(t, 1, probe.publications.Load())
	_, err = publishFinalizationCandidate(ctx, env.sessions, probe, "session", id, candidate)
	require.NoError(t, err, "a verified receipt is idempotent")
	require.EqualValues(t, 1, probe.updates.Load())
	require.EqualValues(t, 1, probe.publications.Load(), "re-reading an accepted receipt must not publish another terminal event")
}

func TestFinalizationPersistedCancelRefusesBeforeCandidateWrite(t *testing.T) {
	env := testEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	candidate := finalizationStoredCandidate(t, env, id)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), "session"))
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	_, err := publishFinalizationCandidate(t.Context(), env.sessions, probe, "session", id, candidate)
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
	candidate := finalizationStoredCandidate(t, env, id)
	reader, supported := env.messages.(message.FinalizationReader)
	require.True(t, supported)
	probe := &finalizationPublishProbe{Service: env.messages, FinalizationReader: reader}
	candidate.hash = "different"
	_, err := publishFinalizationCandidate(t.Context(), env.sessions, probe, "session", id, candidate)
	require.ErrorIs(t, err, ErrFinalizationScopeChanged)
	require.Zero(t, probe.updates.Load())
	require.Zero(t, probe.publications.Load())
	stored, err := env.messages.Get(t.Context(), candidate.messageID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished())
}
