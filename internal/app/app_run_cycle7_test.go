package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
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
	underlying := s.Service.Subscribe(ctx)
	go func() {
		defer close(events)
		select {
		case <-s.start:
		case <-ctx.Done():
			return
		}
		assistantID := ""
		for assistantID == "" {
			select {
			case event, ok := <-underlying:
				if !ok {
					return
				}
				msg := event.Payload
				if msg.SessionID == s.sessionID && msg.Role == message.Assistant {
					assistantID = msg.ID
				}
			case <-ctx.Done():
				return
			}
		}
		events <- pubsub.Event[message.Message]{
			Type: pubsub.UpdatedEvent,
			Payload: message.Message{
				ID:        assistantID,
				SessionID: s.sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "long"}},
			},
		}
		events <- pubsub.Event[message.Message]{
			Type: pubsub.UpdatedEvent,
			Payload: message.Message{
				ID:        assistantID,
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

	reconciled, err := h.app.reconcileTerminalMessage(context.Background(), h.sess.ID, baseline, true, time.Now(), second.ID, map[string]struct{}{first.ID: {}, second.ID: {}})
	require.NoError(t, err)
	require.Equal(t, "authoritative final", reconciled.message.FullText())
	require.Equal(t, map[string]int{"edit": 1, "bash": 1, "view": 1}, reconciled.toolCalls)
}

func TestExecuteRunCycle7EmptyRecorderCannotSelectAnotherTerminal(t *testing.T) {
	h := newCycle6RunApp(t)
	foreign, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "foreign result"}},
	})
	require.NoError(t, err)
	foreign.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), foreign))

	_, err = h.app.reconcileTerminalMessage(context.Background(), h.sess.ID, map[string]struct{}{}, true, time.Now(), "", nil)
	require.Error(t, err, "an empty recorder after fail-fast must not claim another session owner's result")
}

func TestDrainIdentityRequiresExecutedOwnedAttempt(t *testing.T) {
	loop := &executeRunLoop{}
	original := agent.NewCallResultRecorder()
	originalID := "original"
	original.Capture()(originalID)
	loop.callResultRec = original
	loop.eventOwner = original

	noWork := agent.NewCallResultRecorder()
	noWork.Capture()("unconfirmed")
	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainNoWork, recorder: noWork, originalOwner: original,
		assistantIDs:        map[string]struct{}{"unconfirmed": {}},
		terminalAssistantID: "unconfirmed",
	}))
	require.Same(t, original, loop.callResultRec)
	require.Same(t, original, loop.eventOwner)
	emptyComplete := agent.NewCallResultRecorder()
	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: emptyComplete, originalOwner: original,
		assistantIDs:        map[string]struct{}{"missing": {}},
		terminalAssistantID: "missing",
	}))
	require.Same(t, original, loop.callResultRec)
	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainPartial, recorder: noWork, originalOwner: original,
	}))
	require.Same(t, original, loop.callResultRec)

	queued := agent.NewCallResultRecorder()
	queued.Capture()("earlier-queued-call")
	lateCallback := queued.Capture()
	queued.BeginCall()
	lateCallback("late-earlier-queued-call")
	queued.RecordConfirmed("assistant-tool-step")
	queued.RecordConfirmed("completed-continuation")
	require.True(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: queued, originalOwner: original,
		assistantIDs:        map[string]struct{}{"assistant-tool-step": {}, "completed-continuation": {}},
		terminalAssistantID: "completed-continuation",
	}))
	require.Equal(t, "completed-continuation", loop.callResultRec.Resolve())
	require.True(t, loop.eventOwner.Owns("assistant-tool-step"))
	require.True(t, loop.eventOwner.Owns("completed-continuation"))
	require.False(t, loop.callResultRec.Owns("earlier-queued-call"))
	loop.callResultRec.Seal()
	queued.Capture()("late-after-completion")
	require.False(t, loop.callResultRec.Owns("late-after-completion"))
}

func TestDrainIdentityRejectsForeignAndMismatchedConfirmedRows(t *testing.T) {
	original := agent.NewCallResultRecorder()
	original.Capture()("original")
	loop := &executeRunLoop{callResultRec: original, eventOwner: original}
	drained := agent.NewCallResultRecorder()
	drained.RecordConfirmed("actual")
	drained.RecordConfirmed("resolved-elsewhere")

	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: drained, originalOwner: original,
		assistantIDs:        map[string]struct{}{"foreign": {}, "resolved-elsewhere": {}},
		terminalAssistantID: "resolved-elsewhere",
	}))
	require.Same(t, original, loop.callResultRec)
	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: drained, originalOwner: original,
		assistantIDs:        map[string]struct{}{"actual": {}},
		terminalAssistantID: "actual",
	}))
	require.Same(t, original, loop.callResultRec)
	require.Same(t, original, loop.eventOwner)

	mismatched := agent.NewCallResultRecorder()
	mismatched.RecordConfirmed("actual")
	mismatched.RecordConfirmed("late-or-foreign")
	require.False(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: mismatched, originalOwner: original,
		assistantIDs:        map[string]struct{}{"actual": {}},
		terminalAssistantID: "actual",
	}))
	require.Same(t, original, loop.eventOwner)
}

func TestDrainIdentityKeepsAllAssistantRowsForLiveEvents(t *testing.T) {
	recorder := agent.NewCallResultRecorder()
	recorder.RecordConfirmed("assistant-tool-step")
	recorder.RecordConfirmed("assistant-final")
	loop := &executeRunLoop{sess: session.Session{ID: "session"}, mode: RunModeStream, stopSpinner: func() {}}
	loop.eventOwner = recorder
	loop.seenToolCalls = make(map[string]bool)
	loop.toolCallCounts = make(map[string]int)
	loop.messageReadBytes = make(map[string]int)
	var output bytes.Buffer
	loop.stdout = &output
	loop.stderr = io.Discard
	for _, item := range []struct {
		id   string
		text string
		call string
	}{
		{id: "assistant-tool-step", text: "step", call: "edit"},
		{id: "assistant-final", text: "final", call: "view"},
	} {
		loop.handleMessageEvent(pubsub.Event[message.Message]{Payload: message.Message{
			ID: item.id, SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: item.text}, message.ToolCall{ID: item.id + "-call", Name: item.call}},
		}})
	}
	require.Equal(t, "stepfinal", output.String())
	require.Equal(t, map[string]int{"edit": 1, "view": 1}, loop.toolCallCounts)
}

func TestDrainIdentityProcessesToolRowBeforeCompletion(t *testing.T) {
	original := agent.NewCallResultRecorder()
	original.Capture()("original")
	recorder := agent.NewCallResultRecorder()
	loop := &executeRunLoop{
		sess:             session.Session{ID: "session"},
		mode:             RunModeStream,
		stopSpinner:      func() {},
		stdout:           &bytes.Buffer{},
		stderr:           io.Discard,
		seenToolCalls:    make(map[string]bool),
		toolCallCounts:   make(map[string]int),
		messageReadBytes: make(map[string]int),
		callResultRec:    original,
		eventOwner:       recorder,
	}
	toolRow := "assistant-tool-step"
	recorder.RecordConfirmed(toolRow)
	require.NoError(t, loop.handleMessageEvent(pubsub.Event[message.Message]{Payload: message.Message{
		ID: toolRow, SessionID: "session", Role: message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working"}, message.ToolCall{ID: "tool-call", Name: "edit"}},
	}}))
	require.Equal(t, "working", loop.stdout.(*bytes.Buffer).String())
	require.Equal(t, map[string]int{"edit": 1}, loop.toolCallCounts)

	finalID := "assistant-final"
	recorder.RecordConfirmed(finalID)
	require.True(t, loop.adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: recorder, originalOwner: original,
		assistantIDs: map[string]struct{}{toolRow: {}, finalID: {}}, terminalAssistantID: finalID,
	}))
	require.True(t, loop.eventOwner.Owns(toolRow))
	require.True(t, loop.eventOwner.Owns(finalID))
}

func TestDrainIdentityConfirmsTerminalIDAfterFullSet(t *testing.T) {
	for attempt := range 100 {
		recorder := agent.NewCallResultRecorder()
		confirmed := make(map[string]struct{})
		ids := map[string]struct{}{"assistant-tool-step": {}, "assistant-terminal": {}}
		terminal := recordDrainAssistantIDs(&sync.Mutex{}, recorder, confirmed, nil, ids, "assistant-terminal")
		require.Equal(t, "assistant-terminal", terminal)
		require.Equal(t, "assistant-terminal", recorder.Resolve(), "attempt %d", attempt)
		require.True(t, recorder.Owns("assistant-tool-step"))
		require.True(t, recorder.Owns("assistant-terminal"))
		loop := &executeRunLoop{}
		require.True(t, loop.adoptDrainIdentity(drainCompletion{
			result: session.DrainComplete, recorder: recorder,
			assistantIDs: ids, terminalAssistantID: terminal,
		}))
	}

	missingTerminal := agent.NewCallResultRecorder()
	confirmed := make(map[string]struct{})
	terminal := recordDrainAssistantIDs(&sync.Mutex{}, missingTerminal, confirmed, nil,
		map[string]struct{}{"assistant-tool-step": {}}, "assistant-terminal")
	require.Empty(t, terminal)
	require.False(t, (&executeRunLoop{}).adoptDrainIdentity(drainCompletion{
		result: session.DrainComplete, recorder: missingTerminal,
		assistantIDs: map[string]struct{}{"assistant-tool-step": {}}, terminalAssistantID: "assistant-terminal",
	}))
}

func TestLiveMessageEventsIgnoreForeignOwner(t *testing.T) {
	loop := &executeRunLoop{sess: session.Session{ID: "session"}, mode: RunModeStream, stopSpinner: func() {}}
	loop.eventOwner = agent.NewCallResultRecorder()
	loop.eventOwner.Capture()("owned")
	loop.seenToolCalls = make(map[string]bool)
	loop.toolCallCounts = make(map[string]int)
	loop.messageReadBytes = make(map[string]int)
	var output bytes.Buffer
	loop.stdout = &output
	loop.stderr = io.Discard
	loop.handleMessageEvent(pubsub.Event[message.Message]{Payload: message.Message{
		ID: "foreign", SessionID: "session", Role: message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "foreign"}, message.ToolCall{ID: "foreign-call", Name: "bash"}},
	}})
	loop.handleMessageEvent(pubsub.Event[message.Message]{Payload: message.Message{
		ID: "owned", SessionID: "session", Role: message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "owned"}, message.ToolCall{ID: "owned-call", Name: "view"}},
	}})
	require.Equal(t, "owned", output.String())
	require.Equal(t, map[string]int{"view": 1}, loop.toolCallCounts)
}

func TestTerminalReconciliationExcludesForeignInterleavedToolCalls(t *testing.T) {
	h := newCycle6RunApp(t)
	baseline := map[string]struct{}{}
	add := func(parts ...message.ContentPart) message.Message {
		msg, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: parts})
		require.NoError(t, err)
		return msg
	}
	first := add(message.ToolCall{ID: "ours", Name: "view"})
	require.NoError(t, h.app.Messages.Update(context.Background(), first))
	toolResult, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "ours", Name: "view", Content: "ok"}},
	})
	require.NoError(t, err)
	require.NoError(t, h.app.Messages.Update(context.Background(), toolResult))
	foreign := add(message.ToolCall{ID: "theirs", Name: "bash"})
	foreign.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), foreign))
	last := add(message.TextContent{Text: "done"})
	last.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), last))

	reconciled, err := h.app.reconcileTerminalMessage(context.Background(), h.sess.ID, baseline, true, time.Now(), last.ID, map[string]struct{}{first.ID: {}, last.ID: {}})
	require.NoError(t, err)
	require.Equal(t, "done", reconciled.message.FullText())
	require.Equal(t, map[string]int{"view": 1}, reconciled.toolCalls)
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
