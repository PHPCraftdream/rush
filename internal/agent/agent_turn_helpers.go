// Helpers extracted from runTurn (agent_turn.go): provider-warning logging,
// mailbox-inject drain, stream-watchdog onFire handling, and turn-error
// normalization.
package agent

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// stepToolCapabilities reports what this step's tool set allows: editing files
// and delegating to a sub-agent. Read from the set's own names, since a stale
// session prompt cannot make a tool appear (§4 of the in-turn plan).
func stepToolCapabilities(set []fantasy.AgentTool) (canEdit, canDelegate bool) {
	for _, tool := range set {
		switch tool.Info().Name {
		case tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName,
			tools.FSWriteToolName, tools.FSReplaceToolName, tools.FSWriteLinesToolName,
			tools.FSDeleteToolName:
			canEdit = true
		case modelkit.AgentToolName:
			canDelegate = true
		}
	}
	return canEdit, canDelegate
}

// logProviderWarnings emits each fantasy CallWarning from a step at WARN
// level. Without this, warnings such as malformed-tool-call input
// sanitization are silently dropped and never reach the logs. Optional
// fields (setting, tool, details) are attached only when present so the
// line stays terse for the common type+message case.
func logProviderWarnings(warnings []fantasy.CallWarning) {
	for _, w := range warnings {
		attrs := []any{"type", w.Type}
		if w.Message != "" {
			attrs = append(attrs, "message", w.Message)
		}
		if w.Setting != "" {
			attrs = append(attrs, "setting", w.Setting)
		}
		if w.Tool != nil && w.Tool.GetName() != "" {
			attrs = append(attrs, "tool", w.Tool.GetName())
		}
		if w.Details != "" {
			attrs = append(attrs, "details", w.Details)
		}
		slog.Warn("Provider warning", attrs...)
	}
}

func (a *sessionAgent) drainDueInjects(sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	return a.turnBudget.drainDueInjects(a, sessionID, genID, historyIDs)
}

// drainDueInjects is PrepareStep's mailbox-inject drain (design §5, stage
// 2.4), pulled out into a named method for the same reason handleWatchdogFire
// below was (task #243): a test must be able to drive the REAL production
// logic instead of a copy of it. The first version of this lived inline in
// the PrepareStep closure and its tests re-implemented the drain-and-dedup
// in a local helper — a mirror that passes whether or not production still
// matches it, which is exactly the shape #243 exists to stop.
//
// The rows are ALREADY in the DB (InjectMessage persisted them via
// createUserMessage), so callers only splice them into the current prompt —
// no second Create call.
//
// Two rules, one per half of P1-1:
//   - drainInjects(genID) returns only entries stamped at or before THIS
//     turn's generation, so an inject that landed after a previous turn's
//     last PrepareStep is picked up by this turn rather than stranded (the
//     LOSS half).
//   - historyIDs skips any inject whose row the preamble's
//     getSessionMessages already loaded, so a DB write that raced ahead of
//     the preamble does not appear both in history and in the splice (the
//     DUPLICATION half).
func (t *turnBudget) drainDueInjects(h contextBudgetHost, sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	due := h.getMailbox(sessionID).drainInjects(genID)
	return turnstep.DedupInjects(due, historyIDs,
		func(inj pendingInject) string { return inj.msg.ID },
		func(inj pendingInject) message.Message { return inj.msg },
	)
}

func (a *sessionAgent) handleWatchdogFire(
	cause watchdogCause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
) {
	a.turnBudget.handleWatchdogFire(a, cause, elapsed, sessionID, watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
}

// handleWatchdogFire is the EX-0H root route: the real fire-handler body
// lives in the turnstep leaf as turnstep.HandleWatchdogFire (no state
// instantiated there — everything is a parameter). This delegate forwards
// the cause/durations/model snapshot unchanged and passes h.timeoutHardCapCfg
// as a method value, so the config read still happens only in the leaf's
// causeHardCap branch, at the original point. See the leaf doc for the
// full invariant commentary (first synchronous Store, sync capture, async
// write, per-cause logs — all preserved verbatim).
func (t *turnBudget) handleWatchdogFire(
	h contextBudgetHost,
	cause watchdogCause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
) {
	turnstep.HandleWatchdogFire(
		cause, elapsed, sessionID, watchdogCauseVal,
		toolMaxDuration, idleTimeout,
		agentcall.Model(smartModel), // Model = agentcall.Model alias
		h.timeoutHardCapCfg,
	)
}

// normalizeTurnError folds the out-of-band way a turn can end (the
// ask-question tool error) into the single error every downstream consumer
// reads, so nothing past this point has to know either shape.
//
// Extracted from runTurn — one of only two blocks in that function that
// cross no boundary worth worrying about: no defer, no early return, no
// closure created, no lock held, two inputs and one output. See the
// "runTurn is not being decomposed" commit for why the rest of runTurn
// stays where it is.
func normalizeTurnError(err error) error {
	// The ask_question tool reports "agent asked a question" as the Go
	// error its Run() returns; fantasy's executeSingleTool treats a
	// non-nil tool error as critical and propagates it as the whole
	// Stream() call's error, so it surfaces here exactly like any other
	// stream error. tools.AskQuestionError
	// (package tools) exists only because package tools cannot import
	// this package back (this package already imports tools — see the
	// comment on AskQuestionError in ask_question.go for the full import
	// cycle rationale); normalize it into AwaitingAnswerError here so
	// every downstream consumer — the errors.As(err, &awaitingErr) branch
	// at the call site, RunNonInteractive's exit_reason mapping, sessions
	// why/diff, … — only ever has to know about the one agent-level type.
	var askErr *tools.AskQuestionError
	if errors.As(err, &askErr) {
		err = &AwaitingAnswerError{
			Question:  askErr.Question,
			Options:   askErr.Options,
			SessionID: askErr.SessionID,
		}
	}
	return err
}

func asyncLaunchCommand(call fantasy.ToolCallContent) (string, bool) {
	var command string
	switch call.ToolName {
	case tools.BashToolName:
		var params tools.BashParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = params.Command
	case tools.RunCommandToolName:
		var params tools.RunCommandParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = (tools.RunCommandPermissionsParams{Program: params.Program, Args: params.Args}).RunAllowlistCommand()
	default:
		return "", false
	}
	return strings.TrimSpace(command), true
}
