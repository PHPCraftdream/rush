package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func testFinalizationExecution() *finalize.FinalizationExecution {
	return &finalize.FinalizationExecution{
		SessionID: "session", LogicalID: "logical", Gate: gate.New(7),
		Read: func(context.Context) (gate.Facts, error) {
			return gate.Facts{Complete: true}, nil
		},
	}
}

func TestFinalizationExecutionDoesNotLeakIntoAnotherCall(t *testing.T) {
	t.Parallel()
	execution := testFinalizationExecution()
	ctx, err := finalize.WithFinalizationExecution(t.Context(), execution)
	require.NoError(t, err)
	for _, call := range []SessionAgentCall{
		{SessionID: "child", LogicalCallID: "logical"},
		{SessionID: "session", LogicalCallID: "unrelated"},
		{SessionID: "session"},
	} {
		require.Nil(t, finalizationExecutionFor(ctx, call))
	}
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	require.Same(t, execution, finalizationExecutionFor(ctx, call))
	require.Same(t, execution, newTurnStream(turnStreamConfig{ctx: ctx, call: call}).finalization)
	require.Nil(t, finalizationExecutionFor(context.Background(), call))
	require.Nil(t, newTurnStream(turnStreamConfig{call: call}).finalization)
	_, err = finalize.WithFinalizationExecution(ctx, &finalize.FinalizationExecution{SessionID: "session", LogicalID: "logical"})
	require.ErrorIs(t, err, gate.ErrFinalizationFactsUnavailable)
}

func TestFinalizationStepRefreshesStampAfterOwnedWork(t *testing.T) {
	t.Parallel()
	execution := testFinalizationExecution()
	ts := &turnStream{finalization: execution}
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	first := ts.finalizationStamp
	release, err := execution.Gate.AdmitMutation(true)
	require.NoError(t, err)
	release()
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	second := ts.finalizationStamp
	require.Greater(t, second.Serial(), first.Serial())
	candidate := gate.Candidate{MessageID: "message", Hash: "hash"}
	publications := 0
	publish := func(context.Context, gate.Candidate) error {
		publications++
		return nil
	}
	verdict, err := execution.Gate.TrySeal(t.Context(), first, candidate, execution.Read, publish)
	require.NoError(t, err)
	require.Equal(t, gate.Compose, verdict)
	require.Zero(t, publications)
	verdict, err = execution.Gate.TrySeal(t.Context(), second, candidate, execution.Read, publish)
	require.NoError(t, err)
	require.Equal(t, gate.Accepted, verdict)
	require.Equal(t, 1, publications)
}

func TestFinalizationStepReadFailureClearsPreviousStamp(t *testing.T) {
	t.Parallel()
	execution := testFinalizationExecution()
	ts := &turnStream{finalization: execution}
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	require.NotZero(t, ts.finalizationStamp.Serial())
	execution.Read = nil
	require.ErrorIs(t, ts.beginFinalizationStep(t.Context()), gate.ErrFinalizationFactsUnavailable)
	require.Zero(t, ts.finalizationStamp.Serial())
}

func TestFinalizationStepFinishIsProvisionalBeforeUsageFailure(t *testing.T) {
	t.Parallel()
	execution := testFinalizationExecution()
	ctx, err := finalize.WithFinalizationExecution(t.Context(), execution)
	require.NoError(t, err)
	ts := newTurnStream(turnStreamConfig{
		ctx: ctx, genCtx: ctx,
		a:            &sessionAgent{sessions: sentinelSessionService{}},
		call:         SessionAgentCall{SessionID: "session", LogicalCallID: "logical"},
		bumpActivity: func() {}, stopCheckpoint: func() {}, drainPendingUI: func() {},
	})
	checkpoint := &turnCheckpointWriter{}
	ts.fenceCheckpoint = checkpoint.fenceFinalization
	ts.currentAssistant = &message.Message{
		ID: "assistant", SessionID: "session", Role: message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "candidate"}},
	}
	require.NoError(t, ts.beginFinalizationStep(ctx))
	err = ts.onStepFinish(fantasy.StepResult{FinishReason: fantasy.FinishReasonStop})
	require.ErrorIs(t, err, errStopAfterStepFinish)
	require.False(t, ts.currentAssistant.IsFinished(), "a provider ending cannot publish success before usage and acceptance")
	finish := ts.currentAssistant.FinishPart()
	require.NotNil(t, finish)
	require.True(t, finish.Partial)
	require.Equal(t, execution.LogicalID, finish.FinalizationKey)
	require.Equal(t, ts.finalizationStamp.Serial(), finish.FinalizationSerial)
	require.EqualValues(t, 1, ts.currentAssistant.CheckpointGeneration)
}

func TestPrepareStepCapturesFinalizationForEveryProviderStep(t *testing.T) {
	f := newAttemptFixture(t, "finalization-step-boundary", attemptFixtureOpts{noIdle: true})
	ts := guardPrepareStream(t, f, f.model, 0)
	execution := testFinalizationExecution()
	execution.SessionID = f.sessID
	ts.call.LogicalCallID = execution.LogicalID
	ts.finalization = execution
	created := 0
	ts.call.OnAssistantMessageCreated = func(string) { created++ }
	options := fantasy.PrepareStepFunctionOptions{Messages: []fantasy.Message{fantasy.NewUserMessage("continue")}}
	_, _, err := ts.prepareStep(t.Context(), options)
	require.NoError(t, err)
	first := ts.finalizationStamp
	require.NotZero(t, first.Serial(), "the actual PrepareStep must acquire a stamp")
	release, err := execution.Gate.AdmitMutation(true)
	require.NoError(t, err)
	release()
	_, _, err = ts.prepareStep(t.Context(), options)
	require.NoError(t, err)
	require.Greater(t, ts.finalizationStamp.Serial(), first.Serial())
	require.Equal(t, 2, created)
	execution.Read = nil
	_, _, err = ts.prepareStep(t.Context(), options)
	require.ErrorIs(t, err, gate.ErrFinalizationFactsUnavailable)
	require.Zero(t, ts.finalizationStamp.Serial())
	require.Equal(t, 2, created, "unavailable facts must refuse before creating another assistant")
	require.Zero(t, f.requests.Load(), "preparing a step never invokes the provider")
}

func TestFinalizationCandidateFencesAnOlderCheckpoint(t *testing.T) {
	env := testEnv(t)
	execution := testFinalizationExecution()
	created, err := env.messages.Create(t.Context(), "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "candidate"}},
	})
	require.NoError(t, err)
	old := created.Clone()
	old.Parts = []message.ContentPart{message.TextContent{Text: "old checkpoint"}, message.Finish{Reason: message.FinishReasonUnknown, Partial: true}}
	old.CheckpointGeneration = 3
	require.NoError(t, env.messages.Update(t.Context(), old))
	checkpoint := &turnCheckpointWriter{generation: 3}
	execution.Scope = finalizationStoredScope(t, env, message.FinalizationIdentity{Key: "logical", Generation: 7})
	ts := &turnStream{
		a: &sessionAgent{sessions: env.sessions, messages: env.messages}, genCtx: t.Context(),
		call:         SessionAgentCall{SessionID: "session", LogicalCallID: "logical"},
		finalization: execution, currentAssistant: &created,
		fenceCheckpoint: checkpoint.fenceFinalization, drainPendingUI: func() {},
	}
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	require.NoError(t, ts.recordStepFinish(message.FinishReasonEndTurn))
	require.NoError(t, ts.persistStepFinish())
	require.NoError(t, env.messages.Update(t.Context(), old), "stale checkpoints are rejected as a no-op")
	stored, err := env.messages.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "candidate", stored.FullText())
	require.False(t, stored.IsFinished())
	require.EqualValues(t, 4, stored.CheckpointGeneration)
	require.Equal(t, "logical", stored.FinishPart().FinalizationKey)
}
