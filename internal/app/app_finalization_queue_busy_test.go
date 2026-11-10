package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type finalizationBusyCoordinator struct {
	agent.Coordinator
	store      session.AgentFinalizationQueueService
	rebuilds   int
	dispatches int
}

func (c *finalizationBusyCoordinator) RebuildSessionAgentCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	c.rebuilds++
	if _, err := c.store.RestoreAgentFinalizationQueue(ctx, rowID, data); err != nil {
		return agent.SessionAgentCall{}, &agent.ErrCallAlreadyAttempted{Err: err}
	}
	return agent.SessionAgentCall{SessionID: data.SessionID, LogicalCallID: data.LogicalCallID, Prompt: data.Prompt}, nil
}

func (c *finalizationBusyCoordinator) RunSessionAgentCall(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	c.dispatches++
	return nil, nil // Existing mailbox-busy contract, not an executed turn.
}

func TestFinalizationQueueBusyAppMappingRetainsConsumedFailure(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	queries := db.New(conn)
	_, err = queries.CreateSession(ctx, db.CreateSessionParams{ID: "owner", Title: "owner"})
	require.NoError(t, err)
	sessions := session.NewService(queries, conn)
	identity := session.AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1}
	authority, err := sessions.(session.AgentFinalizationService).BindAgentFinalization(ctx, identity)
	require.NoError(t, err)
	trusted, err := session.WithAgentFinalizationContext(ctx, authority)
	require.NoError(t, err)
	data := session.SessionAgentCallData{
		SessionID: "owner", LogicalCallID: "call", Prompt: "compose settled results",
		FinalizationRequirement: session.CaptureAgentFinalizationContext(trusted).Requirement(),
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(trusted, "queued", "owner", raw))
	probe := &finalizationBusyCoordinator{store: sessions.(session.AgentFinalizationQueueService)}
	adapter := &coordinatorAdapterImpl{app: &App{AgentCoordinator: probe}}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: adapter, PumpInstanceID: "pump"})
	defer pump.Stop()
	result, err := pump.DrainSessionNow(ctx, "owner")
	// Initial contention is DrainNoWork/nil, never a completed continuation.
	require.NoError(t, err)
	require.Equal(t, session.DrainNoWork, result)
	require.Equal(t, 1, probe.dispatches, "actual nil/nil app mapping does not report execution")
	var consumed, attempts int64
	var code, status string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT consumed, failure_code FROM session_agent_finalization_continuations WHERE row_id='queued'`).Scan(&consumed, &code))
	require.EqualValues(t, 1, consumed)
	require.Equal(t, "continuation_busy_unknown", code)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT status, attempts FROM session_run_queue WHERE id='queued'`).Scan(&status, &attempts))
	require.Equal(t, "pending", status)
	require.Zero(t, attempts, "existing busy no-penalty release is preserved")
	// A new pump has no local busy backoff: durable consumption must still
	// refuse before any second dispatch, then terminally remove the queue row.
	next := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: adapter, PumpInstanceID: "next-pump"})
	defer next.Stop()
	result, err = next.DrainSessionNow(ctx, "owner")
	require.Error(t, err)
	require.Equal(t, session.DrainFailed, result)
	require.Equal(t, 2, probe.rebuilds)
	require.Equal(t, 1, probe.dispatches, "consumed refusal cannot dispatch a model/effect retry")
	var pending int64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM session_run_queue WHERE id='queued'`).Scan(&pending))
	require.Zero(t, pending)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT failure_code FROM session_agent_finalization_continuations WHERE row_id='queued'`).Scan(&code))
	require.Equal(t, "continuation_busy_unknown", code, "later refusal retains the first precise failure")
	state, err := session.NewAgentFinalizationStore(conn).Snapshot(ctx, identity)
	require.NoError(t, err)
	require.Equal(t, "executing", state.Phase, "unresolved is not accepted/completed")
}
