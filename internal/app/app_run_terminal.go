package app

import (
	"context"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

type terminalReconciliation struct {
	message   message.Message
	toolCalls map[string]int
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
	)
	for _, msg := range messages {
		if msg.Role != message.Assistant || !isRunMessage(msg, baselineIDs, baselineKnown) {
			continue
		}
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
	return terminalReconciliation{message: terminal, toolCalls: calls}, nil
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
