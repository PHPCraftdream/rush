package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The any variant must cancel its max_wait just like the existing all test.
// Complete only after the await turn returns, so schedule creation wins without
// a wall-clock sleep or a goroutine that can outlive the test.
func TestRunLoop_AwaitTasksMaxWaitAny_TaskFinishesFirst_ScheduleCancelled(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"any","max_wait_seconds":60}`),
				admissionSSEStop("aw", "tool_calls"),
			})
		default:
			loopText(w, "f", "final", 11, 3)
		}
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 10*time.Second)
	defer cancel()
	child, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child.ID)
	h.afterFirstTurn(func() {
		rows, err := h.app.WakeScheduleStore().ListSchedules(context.Background(), h.sessionID)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "active", rows[0].State)
		completeDelegation(t, context.Background(), h.app, h.sessionID, "deleg-1", "done-fast", true)
	})

	start := time.Now()
	res, _, runErr := h.run(ctx, RunOverrides{})

	assert.NoError(t, runErr)
	assert.NoError(t, ctx.Err(), "the task finishing first must free the run immediately")
	assert.Less(t, time.Since(start), 8*time.Second,
		"the cancelled max_wait schedule must not hold the run open")
	require.NotNil(t, res)
	assert.Equal(t, "end_turn", res.ExitReason)
	assert.False(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen, "no once schedule is left open")
	rows, err := h.app.WakeScheduleStore().ListSchedules(context.Background(), h.sessionID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "cancelled", rows[0].State, "the max_wait schedule is cancelled by the clear")
	assert.EqualValues(t, 2, h.requests.Load(), "the await turn and the completion drain only")
}

// A durable timer from a previous process has no arbiter entry to clear.
// Preseed before the invocation to exercise the run-start maintenance sweep,
// not the current process's AwaitTasks path. The two-hour deadline never fires.
func TestRunLoop_AwaitTasksMaxWaitRestart_OrphanScheduleCancelled(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "f", "final", 11, 3)
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 10*time.Second)
	defer cancel()
	now := time.Now()
	row, err := h.app.WakeScheduleStore().CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: h.sessionID, Kind: session.WakeKindOnce,
		Message: "await_tasks max_wait elapsed: 1 task(s) may still be running: agent/deleg-old",
		RunAt:   now.Add(2 * time.Hour),
	}, now)
	require.NoError(t, err)
	require.Equal(t, "active", row.State)
	state := cliScopeOf(t, h.app, h.sessionID)
	require.True(t, state.OnceWakeOpen)
	require.True(t, state.WorkOpen)
	if n, ok := h.app.AgentCoordinator.(interface{ WakeSchedulerNotify() }); ok {
		n.WakeSchedulerNotify()
	}

	start := time.Now()
	res, _, runErr := h.run(ctx, RunOverrides{})

	assert.NoError(t, runErr)
	assert.NoError(t, ctx.Err(), "a restarted run must not wait on an orphan max_wait timer")
	assert.Less(t, time.Since(start), 8*time.Second, "the provider final must exit promptly")
	require.NotNil(t, res)
	assert.Equal(t, "end_turn", res.ExitReason)
	assert.Equal(t, "final", res.FinalText)
	assert.EqualValues(t, 1, h.requests.Load(), "no timer reaction is needed")
	assert.Equal(t, "cancelled", wakeScheduleState(t, h.app, row.ID), "the run-start sweep cancels the orphan")
	assert.False(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen, "the orphan is no longer open work")
}

// Model-owned once schedules still hold the run after startup cleanup.
func TestRunLoop_AwaitTasksMaxWaitModelOnceStillHolds(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "f", "final", 11, 3)
	})
	now := time.Now()
	row, err := h.app.WakeScheduleStore().CreateSchedule(t.Context(), session.CreateWakeScheduleParams{
		Owner: h.sessionID, Kind: session.WakeKindOnce, Message: "model-owned reminder",
		RunAt: now.Add(2 * time.Hour),
	}, now)
	require.NoError(t, err)
	h.afterFirstTurn(func() {
		require.True(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, _, runErr := h.run(ctx, RunOverrides{})
	require.ErrorIs(t, runErr, context.DeadlineExceeded)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, "active", wakeScheduleState(t, h.app, row.ID))
	require.EqualValues(t, 1, h.requests.Load())
}

// Start cleanup uses the resolved owner for --last and new invocations too.
func TestRunLoop_AwaitTasksMaxWaitStartSessionResolution(t *testing.T) {
	for _, mode := range []string{"last", "new"} {
		t.Run(mode, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "f", "final", 11, 3)
			})
			now := time.Now()
			row, err := h.app.WakeScheduleStore().CreateSchedule(t.Context(), session.CreateWakeScheduleParams{
				Owner: h.sessionID, Kind: session.WakeKindOnce, Message: session.AwaitTasksMaxWaitPrefix + " old",
				RunAt: now.Add(2 * time.Hour),
			}, now)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var out syncBuffer
			res, err := h.app.RunNonInteractiveWithResult(ctx, &out, "do it",
				RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, "", mode == "last")
			require.NoError(t, err)
			require.NotNil(t, res)
			require.Equal(t, "end_turn", res.ExitReason)
			if mode == "last" {
				require.Equal(t, h.sessionID, res.SessionID)
				require.Equal(t, "cancelled", wakeScheduleState(t, h.app, row.ID))
			} else {
				require.NotEqual(t, h.sessionID, res.SessionID)
				require.Equal(t, "active", wakeScheduleState(t, h.app, row.ID))
			}
			if mode == "last" {
				require.EqualValues(t, 1, h.requests.Load())
			} else {
				// A fresh session also requests its generated title.
				require.EqualValues(t, 2, h.requests.Load())
			}
		})
	}
}
