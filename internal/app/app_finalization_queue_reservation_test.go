package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationQueueBusyThenIdleAcceptsOneRealAppTurn(t *testing.T) {
	var calls atomic.Int64
	application, owner, _ := newLockBusyCLITestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"probe\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"settled\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"probe\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	// This test drives two explicit lease attempts, not the background scanner.
	if application.RunQueuePump != nil {
		application.RunQueuePump.Stop()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	identity := session.AgentFinalizationIdentity{SessionID: owner, LogicalCallID: "reservation-call", Generation: 1}
	authority, err := application.Sessions.(session.AgentFinalizationService).BindAgentFinalization(ctx, identity)
	require.NoError(t, err)
	trusted, err := session.WithAgentFinalizationContext(ctx, authority)
	require.NoError(t, err)
	data := session.SessionAgentCallData{
		SessionID: owner, LogicalCallID: identity.LogicalCallID, Prompt: "compose settled results", AutoResumed: true, NonInteractive: true,
		FinalizationRequirement: session.CaptureAgentFinalizationContext(trusted).Requirement(),
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(trusted, "reserved-queue", owner, raw))
	_, epoch, holdCancel, reserved := application.AgentCoordinator.ReserveExclusive(ctx, owner)
	require.True(t, reserved)
	held := true
	defer func() {
		if held {
			application.AgentCoordinator.ReleaseExclusive(owner, epoch, holdCancel)
		}
	}()
	adapter := &coordinatorAdapterImpl{app: application}
	busy := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: application.Sessions, Coordinator: adapter, PumpInstanceID: "busy-pump"})
	defer busy.Stop()
	result, err := busy.DrainSessionNow(ctx, owner)
	require.NoError(t, err)
	require.Equal(t, session.DrainNoWork, result)
	require.Zero(t, calls.Load(), "busy admission must precede any model request")
	var consumed, attempts int64
	var code string
	require.NoError(t, application.DB().QueryRowContext(ctx, `SELECT consumed, failure_code FROM session_agent_finalization_continuations WHERE row_id='reserved-queue'`).Scan(&consumed, &code))
	require.Zero(t, consumed, "busy must not burn continuation proof")
	require.Empty(t, code)
	require.NoError(t, application.DB().QueryRowContext(ctx, `SELECT attempts FROM session_run_queue WHERE id='reserved-queue'`).Scan(&attempts))
	require.Zero(t, attempts)
	application.AgentCoordinator.ReleaseExclusive(owner, epoch, holdCancel)
	held = false
	idle := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: application.Sessions, Coordinator: adapter, PumpInstanceID: "idle-pump"})
	defer idle.Stop()
	result, err = idle.DrainSessionNow(ctx, owner)
	require.NoError(t, err)
	require.Equal(t, session.DrainComplete, result)
	require.EqualValues(t, 1, calls.Load())
	require.NoError(t, application.DB().QueryRowContext(ctx, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='reserved-queue'`).Scan(&consumed))
	require.EqualValues(t, 1, consumed)
	var queued int64
	require.NoError(t, application.DB().QueryRowContext(ctx, `SELECT count(*) FROM session_run_queue WHERE id='reserved-queue'`).Scan(&queued))
	require.Zero(t, queued)
	require.False(t, application.AgentCoordinator.IsSessionBusy(owner))
	state, err := session.NewAgentFinalizationStore(application.DB()).Snapshot(ctx, identity)
	require.NoError(t, err)
	require.Equal(t, "executing", state.Phase, "reservation acceptance is not whole-task finalization")
}
