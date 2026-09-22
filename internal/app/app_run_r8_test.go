package app

// R8-2/R8-3 (2026-09-22 round-8 audit) acceptance tests. Both reuse the
// cycle7 fixtures (newCycle6RunApp, cycle7BarrierCoordinator) already
// proven for the sibling R2-4 scenarios in app_run_cycle7_test.go.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestExecuteRunCycle7CancelAfterCommitDoesNotTreatToolUseAsSuccess pins
// R8-2: a committed row whose Finish is tool_use proves only that ONE
// intermediate step landed -- the coordinator always issues a further step
// after tool_use, and cancellation before THAT step reaches a determinate
// outcome must not be silenced into a clean "success" the way a genuinely
// completed end_turn row is allowed to (R2-4,
// TestExecuteRunCycle7CancelAfterCommitUsesAuthoritativeTerminal).
//
// afterRun downgrades the real committed terminal ("terminal text",
// end_turn) to tool_use and appends a fresh, still-unfinished assistant row
// -- the continuation step that was in flight when the parent context gets
// canceled.
func TestExecuteRunCycle7CancelAfterCommitDoesNotTreatToolUseAsSuccess(t *testing.T) {
	h := newCycle6RunApp(t)
	underlying := h.app.Messages
	h.app.Messages = &cycle7MessageSeam{Service: underlying, dropEvents: true}
	entered := make(chan struct{})
	release := make(chan struct{})
	h.app.AgentCoordinator = &cycle7BarrierCoordinator{
		Coordinator: h.app.AgentCoordinator,
		entered:     entered,
		release:     release,
		afterRun: func(sessionID string) error {
			messages, err := underlying.List(context.Background(), sessionID)
			if err != nil {
				return err
			}
			var last *message.Message
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role == message.Assistant {
					last = &messages[i]
					break
				}
			}
			if last == nil {
				return errors.New("assistant row not found")
			}
			last.AddFinish(message.FinishReasonToolUse, "", "")
			if err := underlying.Update(context.Background(), *last); err != nil {
				return err
			}
			// Step B: the continuation the coordinator always issues after
			// tool_use, still in flight (no Finish part at all) when the
			// cancellation below lands.
			_, err = underlying.Create(context.Background(), sessionID, message.CreateMessageParams{
				Role:  message.Assistant,
				Parts: []message.ContentPart{message.TextContent{Text: "continuing"}},
			})
			return err
		},
	}
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result *RunResult
		err    error
	}, 1)
	go func() {
		result, err := h.app.ExecuteRun(ctx, RunRequest{
			Prompt:            "answer immediately",
			Mode:              RunModeJSON,
			ContinueSessionID: h.sess.ID,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		resultCh <- struct {
			result *RunResult
			err    error
		}{result: result, err: err}
	}()
	<-entered
	cancel()
	outcome := <-resultCh

	require.Error(t, outcome.err, "a canceled turn with only an intermediate tool_use commit must not report success")
	require.NotNil(t, outcome.result)
	require.NotEqual(t, "tool_use", outcome.result.ExitReason,
		"an intermediate step's Finish must never become the reported outcome")
}

// TestExecuteRunReconciliationIgnoresLaterSessionOwnersRow pins R8-3: A's
// own terminal reconciliation must not pick up a legitimately later B's
// answer on the same session, even though B's row is newer than A's
// baseline and nothing else distinguishes it. afterRun runs synchronously
// right after the real Coordinator.Run (and therefore runInternal, and
// therefore the R8-3 call-result recorder's write) has already returned --
// exactly the ordering the audit describes: B's whole turn commits before
// A's event loop gets around to reconciling.
func TestExecuteRunReconciliationIgnoresLaterSessionOwnersRow(t *testing.T) {
	h := newCycle6RunApp(t)
	underlying := h.app.Messages
	entered := make(chan struct{})
	release := make(chan struct{})
	h.app.AgentCoordinator = &cycle7BarrierCoordinator{
		Coordinator: h.app.AgentCoordinator,
		entered:     entered,
		release:     release,
		afterRun: func(sessionID string) error {
			created, err := underlying.Create(context.Background(), sessionID, message.CreateMessageParams{
				Role:  message.Assistant,
				Parts: []message.ContentPart{message.TextContent{Text: "hijacked answer"}},
			})
			if err != nil {
				return err
			}
			created.AddFinish(message.FinishReasonEndTurn, "", "")
			return underlying.Update(context.Background(), created)
		},
	}
	go func() {
		<-entered
		close(release)
	}()

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "terminal text", result.FinalText,
		"A's own reconciliation must not pick up B's later row on the same session")
}
