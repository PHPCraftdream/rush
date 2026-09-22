package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
)

type terminalReconciliation struct {
	message   message.Message
	toolCalls map[string]int
	// toolCallByID maps each distinct tool-call ID in this phase's rows
	// to its tool name (F8, 2026-09-22 audit): the run-wide tool
	// inventory merges the phases' reconciliations by ID, so a call
	// listed in both phases is still counted once.
	toolCallByID map[string]string
	// toolCallsNoID counts this phase's tool calls that carry no ID and
	// therefore cannot be ID-deduplicated across phases; each phase
	// contributes them as reported.
	toolCallsNoID map[string]int
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
//
// ownID, when non-empty, additionally restricts which row may become the
// terminal to the exact assistant message THIS call's own runInternal
// invocation produced (R8-3, 2026-09-22 audit): without it, a committed A
// that releases its session before its own reconciliation runs can have a
// legitimately later B's answer on the same session picked up instead,
// since both rows are equally "new" relative to A's baseline. ownID is the
// additional filter; baseline stays in effect either way. Empty ownID
// (the recorder was never armed, or the call ended before any message was
// created) falls back to the historical baseline-only selection.
func (app *App) reconcileTerminalMessage(
	ctx context.Context,
	sessionID string,
	baselineIDs map[string]struct{},
	baselineKnown bool,
	runStart time.Time,
	ownID string,
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
		byID     = make(map[string]string)
		noID     = make(map[string]int)
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
				byID[call.ID] = call.Name
			} else {
				noID[call.Name]++
			}
			calls[call.Name]++
		}
		if msg.IsFinished() && (ownID == "" || msg.ID == ownID) {
			terminal = msg
			found = true
		}
	}
	if !found {
		return terminalReconciliation{toolCalls: calls, toolCallByID: byID, toolCallsNoID: noID}, fmt.Errorf(
			"no committed terminal assistant message found for run started at %s",
			runStart.UTC().Format(time.RFC3339),
		)
	}
	return terminalReconciliation{
		message:       terminal,
		toolCalls:     calls,
		toolCallByID:  byID,
		toolCallsNoID: noID,
		combinedText:  continuationChainText(runMessages, terminal),
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
//     tool step's narration does not. An attempt whose text is empty
//     or whitespace-only joins the chain too (R2-2, 2026-09-22
//     audit): the join is byte-exact, so a lone space cut between two
//     attempts is real content, not noise to strip.
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
				parts = append(parts, msg.FullText())
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

// joinContinuationText appends next to acc byte-for-byte. The fragments
// are pieces of ONE answer stream that a transient failure cut at an
// arbitrary byte offset, and the coordinator's continuation prompt tells
// the model to continue exactly where it left off, so whatever belonged
// at the cut — a space, a paragraph break, nothing at all mid-word — is
// the continuation's own first bytes, not something to synthesize here.
// A cut with no whitespace on either side is the common mid-token or
// mid-structure case: partial `{"text":"hel` plus continuation `lo"}`
// must become `{"text":"hello"}`, and a guessed `"\n\n"` corrupts the
// payload into invalid JSON (R2-2 / F3, 2026-09-22 audit). Nothing
// observable distinguishes a paragraph-boundary cut from a mid-token cut
// — a stalled stream carries the same error finish either way — so no
// separator heuristic can be reliable. Where a break is genuinely lost
// because the model did not re-emit one after a boundary cut, the cost
// is cosmetic; an injected separator is corruption.
func joinContinuationText(acc, next string) string {
	return acc + next
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
