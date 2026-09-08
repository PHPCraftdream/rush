package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type cycle7MessageSeam struct {
	message.Service
	dropEvents bool
	failList   bool
	listCalls  int
}

func (s *cycle7MessageSeam) Subscribe(ctx context.Context) <-chan pubsub.Event[message.Message] {
	if s.dropEvents {
		ch := make(chan pubsub.Event[message.Message])
		close(ch)
		return ch
	}
	return s.Service.Subscribe(ctx)
}

func (s *cycle7MessageSeam) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	s.listCalls++
	if s.failList && s.listCalls > 1 {
		return nil, errors.New("terminal lookup unavailable")
	}
	return s.Service.List(ctx, sessionID)
}

type cycle7BarrierCoordinator struct {
	agent.Coordinator
	entered  chan struct{}
	release  <-chan struct{}
	afterRun func(string) error
}

func (c *cycle7BarrierCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	result, err := c.Coordinator.Run(ctx, sessionID, prompt, attachments...)
	if c.afterRun != nil {
		if afterErr := c.afterRun(sessionID); afterErr != nil {
			err = afterErr
		}
	}
	close(c.entered)
	<-c.release
	return result, err
}

type cycle7DoneEventMessages struct {
	message.Service
	events chan pubsub.Event[message.Message]
}

func (s *cycle7DoneEventMessages) Subscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return s.events
}

type cycle7ShrinkingMessages struct {
	message.Service
	sessionID string
	start     <-chan struct{}
}

func (s *cycle7ShrinkingMessages) Subscribe(ctx context.Context) <-chan pubsub.Event[message.Message] {
	events := make(chan pubsub.Event[message.Message])
	go func() {
		defer close(events)
		select {
		case <-s.start:
		case <-ctx.Done():
			return
		}
		events <- pubsub.Event[message.Message]{
			Type: pubsub.UpdatedEvent,
			Payload: message.Message{
				ID:        "shrinking-message",
				SessionID: s.sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "long"}},
			},
		}
		events <- pubsub.Event[message.Message]{
			Type: pubsub.UpdatedEvent,
			Payload: message.Message{
				ID:        "shrinking-message",
				SessionID: s.sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "x"}},
			},
		}
	}()
	return events
}

func TestExecuteRunCycle7ReconcilesDroppedTerminalEvent(t *testing.T) {
	h := newCycle6RunApp(t)
	h.app.Messages = &cycle7MessageSeam{Service: h.app.Messages, dropEvents: true}

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Equal(t, "terminal text", result.FinalText)
	require.Equal(t, "end_turn", result.ExitReason)
}

func TestExecuteRunCycle7DroppedTerminalEventTerseOutputOnce(t *testing.T) {
	h := newCycle6RunApp(t)
	h.app.Messages = &cycle7MessageSeam{Service: h.app.Messages, dropEvents: true}
	var output bytes.Buffer

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeTerse,
		ContinueSessionID: h.sess.ID,
		Stdout:            &output,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, "terminal text\n", output.String())
}

func TestExecuteRunCycle7ReconciliationFencesOlderAssistant(t *testing.T) {
	h := newCycle6RunApp(t)
	old, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old final"}},
	})
	require.NoError(t, err)
	old.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), old))
	h.app.Messages = &cycle7MessageSeam{Service: h.app.Messages, dropEvents: true}

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Equal(t, "terminal text", result.FinalText)
}

func TestExecuteRunCycle7ReconciliationFailureReportsDiagnostic(t *testing.T) {
	h := newCycle6RunApp(t)
	h.app.Messages = &cycle7MessageSeam{Service: h.app.Messages, failList: true}

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Equal(t, "terminal text", result.FinalText)
	require.Contains(t, result.Warnings, "authoritative terminal message reconciliation failed: list run messages: terminal lookup unavailable; using live run events")
}

func TestExecuteRunCycle7CancelAfterCommitUsesAuthoritativeTerminal(t *testing.T) {
	h := newCycle6RunApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.app.AgentCoordinator = &cycle7BarrierCoordinator{
		Coordinator: h.app.AgentCoordinator,
		entered:     entered,
		release:     release,
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
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, "terminal text", outcome.result.FinalText)
	require.Equal(t, "end_turn", outcome.result.ExitReason)
	require.Equal(t, int64(10), outcome.result.Usage.DeltaTokens)
}

func TestExecuteRunCycle7CancelAfterCommitKeepsTerminalError(t *testing.T) {
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
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role != message.Assistant {
					continue
				}
				messages[i].AddFinish(message.FinishReasonError, "Synthetic title", "Synthetic details")
				return underlying.Update(context.Background(), messages[i])
			}
			return errors.New("assistant row not found")
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
	require.Error(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, "terminal text", outcome.result.FinalText)
	require.Equal(t, "error", outcome.result.ExitReason)
	require.Equal(t, "Synthetic title: Synthetic details", outcome.result.Error)
}

func TestExecuteRunCycle7CancelBeforeTerminalKeepsPromptCancellation(t *testing.T) {
	h := newCycle6RunApp(t)
	underlying := h.app.Messages
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
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role == message.Assistant {
					return underlying.ForceDelete(context.Background(), messages[i].ID)
				}
			}
			return errors.New("assistant row not found")
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
	require.Nil(t, outcome.result)
	require.ErrorIs(t, outcome.err, context.Canceled)
}

func TestExecuteRunCycle7ReconcileToolCallsUsesOnlyNewRows(t *testing.T) {
	h := newCycle6RunApp(t)
	old, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "old", Name: "old_tool"}},
	})
	require.NoError(t, err)
	old.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), old))
	baseline := map[string]struct{}{old.ID: {}}

	first, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "edit-1", Name: "edit"},
			message.ToolCall{ID: "duplicate", Name: "bash"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.app.Messages.Update(context.Background(), first))
	second, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "authoritative final"},
			message.ToolCall{ID: "duplicate", Name: "bash"},
			message.ToolCall{ID: "view-1", Name: "view"},
		},
	})
	require.NoError(t, err)
	second.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), second))

	reconciled, err := h.app.reconcileTerminalMessage(context.Background(), h.sess.ID, baseline, true, time.Now())
	require.NoError(t, err)
	require.Equal(t, "authoritative final", reconciled.message.FullText())
	require.Equal(t, map[string]int{"edit": 1, "bash": 1, "view": 1}, reconciled.toolCalls)
}

func TestExecuteRunCycle7ShrinkingStreamEventRemainsHardFailure(t *testing.T) {
	h := newCycle6RunApp(t)
	start := make(chan struct{})
	h.app.Messages = &cycle7ShrinkingMessages{Service: h.app.Messages, sessionID: h.sess.ID, start: start}
	executeRunBeforeTurnLaunchSeam = func() { close(start) }
	t.Cleanup(func() { executeRunBeforeTurnLaunchSeam = nil })

	var output bytes.Buffer
	_, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeStream,
		ContinueSessionID: h.sess.ID,
		Stdout:            &output,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.EqualError(t, err, "message content is shorter than read bytes: 1 < 4")
}

func TestExecuteRunCycle7DoneFirstDrainDoesNotDuplicateTerseOrStream(t *testing.T) {
	for _, mode := range []RunMode{RunModeTerse, RunModeStream} {
		t.Run(map[RunMode]string{RunModeTerse: "terse", RunModeStream: "stream"}[mode], func(t *testing.T) {
			h := newCycle6RunApp(t)
			underlying := h.app.Messages
			events := make(chan pubsub.Event[message.Message], 1)
			h.app.Messages = &cycle7DoneEventMessages{Service: underlying, events: events}
			executeRunDoneCaseSeam = func() {
				messages, err := underlying.List(context.Background(), h.sess.ID)
				require.NoError(t, err)
				for i := len(messages) - 1; i >= 0; i-- {
					if messages[i].Role == message.Assistant {
						events <- pubsub.Event[message.Message]{Type: pubsub.UpdatedEvent, Payload: messages[i]}
						close(events)
						return
					}
				}
				t.Fatal("assistant row not found")
			}
			t.Cleanup(func() { executeRunDoneCaseSeam = nil })

			var output bytes.Buffer
			result, err := h.app.ExecuteRun(context.Background(), RunRequest{
				Prompt:            "answer immediately",
				Mode:              mode,
				ContinueSessionID: h.sess.ID,
				Stdout:            &output,
				Stderr:            io.Discard,
				HideSpinner:       true,
			})
			require.NoError(t, err)
			require.Nil(t, result)
			require.Equal(t, "terminal text\n", output.String())
		})
	}
}
