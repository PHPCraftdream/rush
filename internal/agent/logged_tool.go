package agent

import (
	"context"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// Every tool failure reaches the log, whichever way it fails.
//
// Before this wrapper existed, none of them did. A run died 42 seconds into
// a 75k-character prompt because a todos item carried an unusable status,
// and the whole failure window in crush.log contained no ERROR record at
// all — the only carriers were the process's stderr and the JSON envelope,
// and both are overwritten when the same session id is run again. Diagnosing
// it meant reading the source.
//
// The wrapper is deliberately unconditional, unlike wrapToolsWithHooks,
// which returns the slice untouched when no hook runner is configured or
// when the caller is a sub-agent. Those are exactly the cases the incident
// above fell into, so hooking the logging onto that wrapper would have left
// the original blind spot in place.
//
// It is applied inside NewSessionAgent and SetTools — the only two doors
// tools have into a sessionAgent — rather than at each place a tool slice
// is assembled. That distinction is not cosmetic: it was first applied at
// the buildTools call site, which left agentic_fetch's own six tools
// (assembled in agentic_fetch_tool.go, never passed through buildTools)
// silently uncovered, so a fatal error inside a nested fetch logged as
// tool=agentic_fetch and named neither the failing tool nor its message.
// Wrapping at the door means a slice assembled somewhere new cannot miss
// it by omission.
//
// It sits OUTSIDE the hook wrapper, so a tool call a hook refuses is logged
// too: from an operator's point of view "the hook blocked it" and "the tool
// rejected it" are the same question — why did this call not do anything.
type loggedTool struct {
	inner fantasy.AgentTool
}

func newLoggedTool(inner fantasy.AgentTool) *loggedTool {
	return &loggedTool{inner: inner}
}

// wrapToolsWithErrorLogging wraps every tool so its failures are logged.
// Nothing gates it — no runner, no sub-agent, no configuration.
//
// Wrapping an already-wrapped tool is a no-op rather than a second layer:
// the function is called from both doors into a sessionAgent, and a caller
// that hands the same slice to both (or wraps defensively before calling)
// would otherwise write every failure to the log twice.
func wrapToolsWithErrorLogging(list []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(list))
	for i, tool := range list {
		if _, already := tool.(*loggedTool); already {
			out[i] = tool
			continue
		}
		out[i] = newLoggedTool(tool)
	}
	return out
}

// maxLoggedContentRunes bounds the tool-error body written to the log.
//
// Only the recoverable side is truncated, and the asymmetry is the point.
// An error RESPONSE is something the model is expected to produce
// repeatedly — it can retry a malformed call in a loop — and the bodies are
// not bounded anywhere: sourcegraph's JSON error quotes the offending byte
// range of the remote reply, fetch and download embed server responses,
// bash embeds the command line. Left whole, a retry loop appends multi-KB
// records carrying remote payload fragments to crush.log. A returned ERROR
// ends the run, so it is written at most once per run and stays whole:
// that one record is the only account of why the run stopped.
//
// The limit is generous enough to keep the head of a message greppable,
// which is what a log record is for; the full text still reaches the model,
// which is who has to act on it.
const maxLoggedContentRunes = 512

// truncateForLog shortens s to at most maxRunes runes, counting in runes so
// a multi-byte character is never split, and says how much it dropped —
// a silently clipped message reads as a complete one.
func truncateForLog(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	kept := 0
	for i := range s {
		if kept == maxRunes {
			return s[:i] + fmt.Sprintf("… (truncated, %d of %d runes shown)",
				maxRunes, utf8.RuneCountInString(s))
		}
		kept++
	}
	return s
}

func (l *loggedTool) Info() fantasy.ToolInfo { return l.inner.Info() }

func (l *loggedTool) ProviderOptions() fantasy.ProviderOptions {
	return l.inner.ProviderOptions()
}

func (l *loggedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	l.inner.SetProviderOptions(opts)
}

func (l *loggedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := l.inner.Run(ctx, call)

	// Levels are split on purpose. A returned error is level 3 in the tool
	// error contract (internal/agent/tools/tools.go): it unwinds the agent
	// loop and ends the run, so it is always ERROR. An error RESPONSE is
	// level 1 — the model is expected to misuse a tool occasionally and
	// correct itself, and logging every mistyped path at ERROR would drain
	// the level of meaning for the failures that actually end a session.
	// WARN keeps them greppable without that cost.
	//
	// The attribute is level_kind, not level: slog owns "level" in its own
	// output, and a second one would emit a duplicate JSON key.
	switch {
	case err != nil:
		slog.Error("tool call failed, ending the run",
			"tool", call.Name,
			"session_id", tools.GetSessionFromContext(ctx),
			"tool_call_id", call.ID,
			"level_kind", "fatal",
			"err", err,
		)
	case resp.IsError:
		slog.Warn("tool call returned an error to the model",
			"tool", call.Name,
			"session_id", tools.GetSessionFromContext(ctx),
			"tool_call_id", call.ID,
			"level_kind", "recoverable",
			"stop_turn", resp.StopTurn,
			"content", truncateForLog(resp.Content, maxLoggedContentRunes),
		)
	}

	return resp, err
}
