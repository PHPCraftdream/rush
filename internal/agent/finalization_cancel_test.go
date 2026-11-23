package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/stretchr/testify/require"
)

func finalizationCancelTestExecution(sessionID string, epoch uint64) *finalize.FinalizationExecution {
	return &finalize.FinalizationExecution{
		SessionID: sessionID,
		LogicalID: "logical",
		Gate:      gate.New(epoch),
		Read:      quietFinalizationTestFacts,
	}
}

func TestFinalizationOnCancelHookRefusesSealBeforePublication(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	registry := finalize.NewFinalizationCancelRegistry()
	registry.Register(env.execution)
	hook := finalize.NewFinalizationOnCancelHook(registry)
	stamp, err := env.execution.Gate.BeginCandidate(env.ctx, env.execution.Read)
	require.NoError(t, err)
	hook(env.call.SessionID)
	var published int
	publish := func(context.Context, gate.Candidate) error {
		published++
		return nil
	}
	candidate := gate.Candidate{MessageID: "m", Hash: "h"}
	verdict, err := env.execution.Gate.TrySeal(env.ctx, stamp, candidate, env.execution.Read, publish)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, gate.WaitWork, verdict)
	require.Zero(t, published, "a cancelled owner must not deliver anything")
	_, err = env.execution.Gate.BeginCandidate(env.ctx, env.execution.Read)
	require.ErrorIs(t, err, context.Canceled)
	_, err = env.execution.Gate.TrySeal(env.ctx, stamp, candidate, env.execution.Read, publish)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, published)
	record, err := env.scope.Store.Snapshot(env.ctx, env.scope.Identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)
	require.False(t, record.Cancelled)
	require.Empty(t, record.CandidateMessageID)
	require.NoError(t, env.sessions.RequestCancel(env.ctx, "session"))
	record, err = env.scope.Store.Snapshot(env.ctx, env.scope.Identity)
	require.NoError(t, err)
	require.True(t, record.Cancelled)
}
