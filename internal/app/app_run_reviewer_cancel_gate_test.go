package app

import (
	"context"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestExecuteRunReviewerPassGateRefusesCanceledParent pins R2-4: a parent
// cancel landing after the primary turn committed a successful terminal
// message must NOT open the reviewer phase. The event loop can reach the
// committed success through either select ordering — done-first (the turn
// result wins, finish() reconciles live) or ctx.Done-first (the probe
// reconciles, caches the triple and calls finish(ctx.Err()), whose
// authoritativeTerminal suppression turns the cancellation into a clean
// result and sets canceledAfterCommit). Either way the reviewer gate must
// refuse a new phase: via loop.ctx.Err() != nil in the first ordering and
// via canceledAfterCommit in the second — both must yield the same
// outcome: the committed primary text is the run's final answer, the
// reviewer model is never requested, and the envelope's usage comes from
// a live context (a stale cancelled probe read would emit a
// "session usage" warning and zero deltas).
func TestExecuteRunReviewerPassGateRefusesCanceledParent(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "cancel-gate")

	entered := make(chan struct{})
	release := make(chan struct{})
	h.app.AgentCoordinator = &cycle7BarrierCoordinator{
		Coordinator: h.app.AgentCoordinator,
		entered:     entered,
		release:     release,
	}
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan struct {
		result *RunResult
		err    error
	}, 1)
	go func() {
		result, err := h.app.ExecuteRun(ctx, RunRequest{
			Prompt:            "do the thing",
			Mode:              RunModeJSON,
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		resultCh <- struct {
			result *RunResult
			err    error
		}{result: result, err: err}
	}()

	// The barrier closes entered only after the inner Coordinator.Run has
	// fully returned, so the primary turn's terminal row is committed
	// before the cancel fires.
	<-entered
	cancel()
	out := <-resultCh

	require.NoError(t, out.err)
	require.NotNil(t, out.result)
	// The committed primary is the final answer; no reviewer turn may
	// overwrite it.
	require.Equal(t, reviewerPassPrimaryText, out.result.FinalText)
	require.Equal(t, "end_turn", out.result.ExitReason)
	// The reviewer model must never be requested.
	require.Equal(t, []string{"smart-default"}, h.requestedModels())
	// A stale cancelled probe context read would emit exactly this warning.
	for _, warning := range out.result.Warnings {
		require.NotContains(t, warning, "session usage")
	}
	// Usage must come from a live context.
	require.Positive(t, out.result.Usage.DeltaTokens)
}

// TestResetForReviewerPassClearsCachedTerminalTriple pins R2-4's "reset
// clears cache/context/cancel as one unit" requirement: a terminal
// reconciliation triple cached by the primary phase must not survive
// resetForReviewerPass. Its probe context derives from the primary
// phase's own (already canceled) ctx and finish()'s defer has already
// fired its cancel, so reset must RELEASE the stale probe context — not
// merely drop the reference — and drop all three fields together.
func TestResetForReviewerPassClearsCachedTerminalTriple(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reset-triple")
	loop := &executeRunLoop{app: h.app, sess: sess}

	probeCtx, probeCancel := context.WithCancel(context.Background())
	loop.cachedTerminal = &terminalReconciliation{}
	loop.cachedTerminalCtx = probeCtx
	loop.cachedTerminalCancel = probeCancel

	loop.resetForReviewerPass(context.Background())

	require.Nil(t, loop.cachedTerminal)
	require.Nil(t, loop.cachedTerminalCtx)
	require.Nil(t, loop.cachedTerminalCancel)
	require.ErrorIs(t, probeCtx.Err(), context.Canceled)
}
