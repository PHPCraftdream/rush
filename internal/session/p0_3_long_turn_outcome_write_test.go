// P0-3 regression: a fresh outcome-write context starts after Coordinator.Run returns.
package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type outcomeContextService struct {
	session.Service
	preRun      context.Context
	returnedAt  time.Time
	ackCtx      context.Context
	writeBudget time.Duration
}

func (s *outcomeContextService) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	s.ackCtx = ctx
	// Outcome context is captured for the timing oracle; delegate real Ack with
	// an independent context so storage latency cannot exhaust that budget.
	return s.Service.AckRunQueueEntry(context.Background(), id, leasedBy)
}

type observingCoordinator struct {
	service *outcomeContextService
}

func (c *observingCoordinator) Run(ctx context.Context, _ session.SessionAgentCallData) (*any, error) {
	c.service.preRun = ctx
	var result any = "executed"
	return &result, nil
}

func TestP0_3_LongTurnOutcomeWriteSurvivesDBWriteTimeout(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc, sqlDB := setupTestSessionWithDB(t, "test-session-p0-3-long-turn")
	ctx := t.Context()
	wrapped := &outcomeContextService{Service: svc, writeBudget: time.Hour}
	idempotencyKey := "p0-3-long-turn-probe"
	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callData))
	coord := &observingCoordinator{service: wrapped}
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(wrapped, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions: probe, Coordinator: coord, PumpInstanceID: "p0-3-long-turn-pump",
		TestTick:     func() time.Duration { return 100 * time.Millisecond },
		TestLeaseTTL: 30 * time.Second, TestClock: clk,
		TestDBWriteTimeout: time.Hour,
		TestOnCancelCause: func(session.CancelCauseForTest) {
			wrapped.returnedAt = time.Now()
		},
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })
	awaitPump(t, func() bool { return clk.liveTickers() >= 1 }, "scan ticker must be registered")
	// Watchdog ticks may fire, but 100ms does not reach the lease deadline.
	// Lease loss skips outcome writes and leaves the row leased.
	clk.Advance(100 * time.Millisecond)
	awaitPump(t, func() bool {
		var exists bool
		row := sqlDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM session_run_queue WHERE id = ?)", idempotencyKey)
		if err := row.Scan(&exists); err != nil {
			return false
		}
		return !exists
	}, "entry must be Acked after Run returns")
	pump.Stop()
	require.NotSame(t, wrapped.preRun, wrapped.ackCtx)
	require.NotZero(t, wrapped.returnedAt)
	deadline, ok := wrapped.ackCtx.Deadline()
	require.True(t, ok)
	startedAt := deadline.Add(-wrapped.writeBudget)
	require.False(t, startedAt.Before(wrapped.returnedAt))
}
