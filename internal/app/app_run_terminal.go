package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/agent"
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
		// runMessages collects this run's rows in commit order —
		// assistant steps, the coordinator's continuation prompts
		// between them, and tool results — so the continuation chain
		// walk below can tell a same-attempt tool-loop step apart from
		// a separate turn's boundary.
		runMessages []message.Message
	)
	for _, msg := range messages {
		if !isRunMessage(msg, baselineIDs, baselineKnown) {
			continue
		}
		runMessages = append(runMessages, msg)
		if msg.Role != message.Assistant {
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
	return terminalReconciliation{
		message:      terminal,
		toolCalls:    calls,
		combinedText: continuationChainText(runMessages, terminal),
	}, nil
}

// continuationChainText combines a successful terminal assistant message
// with the failed attempts it resumed from. The coordinator's
// continuation retry (runInternal) deliberately leaves each failed
// attempt's partial text in its own history row and asks the model to
// continue in a fresh message, so the run's full answer is spread across
// several rows and the terminal row alone carries only the tail.
//
// The walk starts at the row before the terminal and moves backward over
// whole messages (not just assistants) so it can recognize the boundaries
// it crosses:
//   - a tool result row belongs to the tool-loop step of the attempt
//     being walked and is skipped without breaking the chain;
//   - a clean assistant row is skipped the same way only while it is a
//     tool-loop step (it carries the tool calls, or ends on
//     FinishReasonToolUse) — any other clean row is a completed turn and
//     stops the walk;
//   - a user row is a turn boundary. Only the coordinator's own
//     continuation prompt (agent.IsContinuationPrompt, matched against
//     the producer agent's continuationPrompt verbatim) keeps the chain
//     alive across it; a genuine user turn — including this run's own
//     original prompt — stops the walk, so text is never combined across
//     unrelated messages. A failed attempt's text joins the chain; a
//     tool step's narration does not.
//
// A terminal message that itself ended in error returns "": the run
// failed, and there is no successful continuation to attach anything to.
func continuationChainText(runMessages []message.Message, terminal message.Message) string {
	if fp := terminal.FinishPart(); fp != nil && fp.Reason == message.FinishReasonError {
		return ""
	}
	termIdx := -1
	for i, msg := range runMessages {
		if msg.ID == terminal.ID {
			termIdx = i
			break
		}
	}
	if termIdx < 0 {
		return ""
	}
	var parts []string
collect:
	for i := termIdx - 1; i >= 0; i-- {
		msg := runMessages[i]
		switch msg.Role {
		case message.Tool:
			// A tool result of the attempt being walked.
		case message.User:
			if !agent.IsContinuationPrompt(msg.FullText()) {
				break collect
			}
		case message.Assistant:
			if fp := msg.FinishPart(); fp != nil && fp.Reason == message.FinishReasonError {
				if text := msg.FullText(); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
				continue
			}
			if len(msg.ToolCalls()) == 0 && msg.FinishReason() != message.FinishReasonToolUse {
				break collect
			}
		default:
			break collect
		}
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Reverse(parts)
	combined := ""
	for _, part := range parts {
		combined = joinContinuationText(combined, part)
	}
	return joinContinuationText(combined, terminal.FullText())
}

// joinContinuationText appends next to acc the way the coordinator's
// continuation retry split one answer stream. When either side already
// carries the boundary whitespace, the fragments are joined byte-for-byte:
// an interruption cut mid-content (inside a JSON string, a word, an
// indented code line) leaves that whitespace to the continuation's first
// token, so inserting a separator would corrupt the payload (R2-2,
// mechanism B): partial `{"text":"hello` plus continuation ` world"}`
// must become `{"text":"hello world"}`, never
// `{"text":"hello\n\n world"}`. Only when both sides are bare does it
// synthesize the paragraph break between two independently readable
// blocks.
func joinContinuationText(acc, next string) string {
	if acc == "" {
		return next
	}
	if next == "" {
		return acc
	}
	if endsWithSpace(acc) || startsWithSpace(next) {
		return acc + next
	}
	return acc + "\n\n" + next
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

func endsWithSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsSpace(r)
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
