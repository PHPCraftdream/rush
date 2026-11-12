package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationDurableRevisionSurvivesLocalGateReconstruction(t *testing.T) {
	env := testEnv(t)
	identity := message.FinalizationIdentity{Key: "logical", Generation: 7, Revision: 2, Serial: 1}
	scope := finalizationStoredScope(t, env, identity)
	_, err := scope.store.Admit(t.Context(), scope.identity, "before-driver")
	require.NoError(t, err)
	require.NoError(t, scope.store.Release(t.Context(), scope.identity, "before-driver"))
	execution := &finalizationExecution{
		sessionID: "session", logicalID: "logical", gate: newFinalizationGate(7),
		read: func(ctx context.Context) (finalizationFacts, error) {
			record, readErr := scope.store.Snapshot(ctx, scope.identity)
			if readErr != nil {
				return finalizationFacts{}, readErr
			}
			return finalizationFacts{complete: true, durableRevision: uint64(record.Revision), own: int(record.OpenTickets), cancelled: record.Cancelled}, nil
		},
	}
	created, err := env.messages.Create(t.Context(), "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "settled result"}},
	})
	require.NoError(t, err)
	checkpoint := &turnCheckpointWriter{}
	ts := &turnStream{finalization: execution, currentAssistant: &created, fenceCheckpoint: checkpoint.fenceFinalization}
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	first := ts.finalizationStamp
	require.Zero(t, first.revision, "a reconstructed local gate has its own counter")
	require.EqualValues(t, 2, first.durableRevision)
	require.NoError(t, ts.markFinalizationStep())
	require.EqualValues(t, 2, ts.currentAssistant.FinishPart().FinalizationRevision)
	require.NoError(t, env.messages.Update(t.Context(), *ts.currentAssistant))
	hash, err := ts.currentAssistant.FinalizationContentHash()
	require.NoError(t, err)
	candidate := finalizationCandidate{messageID: created.ID, hash: hash}

	// A separate store represents work that did not signal this local gate.
	external := session.NewAgentFinalizationStore(env.conn)
	_, err = external.Admit(t.Context(), scope.identity, "external-work")
	require.NoError(t, err)
	require.NoError(t, external.Release(t.Context(), scope.identity, "external-work"))
	publications := 0
	publish := func(ctx context.Context, candidate finalizationCandidate) error {
		publications++
		stamp := ts.finalizationStamp
		_, publishErr := publishFinalizationCandidate(ctx, env.sessions, env.messages, scope, message.FinalizationIdentity{
			Key: execution.logicalID, Generation: stamp.epoch, Revision: stamp.durableRevision, Serial: stamp.serial,
		}, candidate)
		return publishErr
	}
	t.Cleanup(func() {
		if t.Failed() {
			record, readErr := scope.store.Snapshot(t.Context(), scope.identity)
			t.Logf("phase=durable-revision first=%+v current=%+v record=%+v read_error=%v publications=%d", first, ts.finalizationStamp, record, readErr, publications)
		}
	})
	verdict, err := execution.gate.trySeal(t.Context(), first, candidate, execution.read, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict)
	require.Zero(t, publications, "quiet external work still invalidates pre-work composition")
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	require.Zero(t, ts.finalizationStamp.revision)
	require.EqualValues(t, 4, ts.finalizationStamp.durableRevision)
	require.NoError(t, ts.markFinalizationStep())
	require.NoError(t, env.messages.Update(t.Context(), *ts.currentAssistant))
	verdict, err = execution.gate.trySeal(t.Context(), ts.finalizationStamp, candidate, execution.read, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationAccepted, verdict)
	require.Equal(t, 1, publications)
	record, err := scope.store.Snapshot(t.Context(), scope.identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.EqualValues(t, 4, record.Revision)
	stored, err := env.messages.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, stored.IsFinished())
	require.EqualValues(t, 4, stored.FinishPart().FinalizationRevision)
}

func TestFinalizationRefusesUnrepresentableDurableRevision(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(1)
	_, err := gate.beginCandidate(t.Context(), func(context.Context) (finalizationFacts, error) {
		return finalizationFacts{complete: true, durableRevision: uint64(1) << 63}, nil
	})
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
}
