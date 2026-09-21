package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

type terminalReconciliation struct {
	message   message.Message
	toolCalls map[string]int
	// combinedText, when non-empty, is the terminal message's text
	// prefixed with the texts of the failed assistant attempts a
	// successful continuation chain resumed from (see
	// continuationChainText). finish() prefers it over the terminal
	// message's own FullText so a run whose answer was interrupted
	// mid-stream and then completed returns both halves, not just the
	// continuation's tail.
	combinedText string
}

// reconcileTerminalMessage reads the committed assistant rows for this run.
// The baseline ID set fences out messages that existed before the turn,
// including rows created in the same second as the run.
func (app *App) reconcileTerminalMessage(
	ctx context.Context,
	sessionID string,
	baselineIDs map[string]struct{},
	baselineKnown bool,
	runStart time.Time,
) (terminalReconciliation, error) {
	messages, err := app.Messages.List(ctx, sessionID)
	if err != nil {
		return terminalReconciliation{}, fmt.Errorf("list run messages: %w", err)
	}

	var (
		terminal message.Message
		found    bool
		calls    = make(map[string]int)
		seen     = make(map[string]struct{})
		// runAssistants collects this run's assistant rows in commit
		// order so the continuation chain below can walk backwards from
		// the terminal message.
		runAssistants []message.Message
	)
	for _, msg := range messages {
		if msg.Role != message.Assistant || !isRunMessage(msg, baselineIDs, baselineKnown) {
			continue
		}
		runAssistants = append(runAssistants, msg)
		for _, call := range msg.ToolCalls() {
			if call.ID != "" {
				if _, ok := seen[call.ID]; ok {
					continue
				}
				seen[call.ID] = struct{}{}
			}
			calls[call.Name]++
		}
		if msg.IsFinished() {
			terminal = msg
			found = true
		}
	}
	if !found {
		return terminalReconciliation{toolCalls: calls}, fmt.Errorf(
			"no committed terminal assistant message found for run started at %s",
			runStart.UTC().Format(time.RFC3339),
		)
	}
	return terminalReconciliation{
		message:      terminal,
		toolCalls:    calls,
		combinedText: continuationChainText(runAssistants, terminal),
	}, nil
}

// continuationChainText combines a successful terminal assistant message
// with the failed attempts it resumed from. The coordinator's
// continuation retry (runInternal) deliberately leaves each failed
// attempt's partial text in its own history row and asks the model to
// continue in a fresh message, so the run's full answer is spread across
// several rows and the terminal row alone carries only the tail.
//
// The walk starts at the row before the terminal and consumes only
// consecutive assistant rows whose finish reason is error — exactly the
// rows a continuation chain leaves behind (the continuation prompts
// between them are user rows and are skipped). Any other assistant row —
// a clean tool-loop round, a separate turn — stops the walk, so text is
// never combined across unrelated messages. A terminal message that
// itself ended in error returns "": the run failed, and there is no
// successful continuation to attach anything to.
func continuationChainText(runAssistants []message.Message, terminal message.Message) string {
	if fp := terminal.FinishPart(); fp != nil && fp.Reason == message.FinishReasonError {
		return ""
	}
	termIdx := -1
	for i, msg := range runAssistants {
		if msg.ID == terminal.ID {
			termIdx = i
			break
		}
	}
	if termIdx < 0 {
		return ""
	}
	var parts []string
	for i := termIdx - 1; i >= 0; i-- {
		msg := runAssistants[i]
		fp := msg.FinishPart()
		if fp == nil || fp.Reason != message.FinishReasonError {
			break
		}
		if text := strings.TrimSpace(msg.FullText()); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Reverse(parts)
	if text := strings.TrimSpace(terminal.FullText()); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func isRunMessage(
	msg message.Message,
	baselineIDs map[string]struct{},
	baselineKnown bool,
) bool {
	if baselineKnown {
		_, existed := baselineIDs[msg.ID]
		return !existed
	}
	// Without the identity snapshot, a same-second older row cannot be
	// distinguished safely. Let the live-event fallback carry the result.
	return false
}
