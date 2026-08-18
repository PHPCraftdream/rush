// Effective timeout and duration resolution: these helpers fold the agent's
// configured overrides (SessionAgentOptions fields, SetTimeoutOptions) onto
// the package-level defaults, one resolved value per Run()/runTurn() call.
package agent

import (
	"time"
)

// SetTimeoutOptions configures the stream watchdog deadline extension.
// Fork patch: batch 8.
func (a *sessionAgent) SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	a.timeoutExtendsOnProgress = extendsOnProgress
	a.timeoutHardCap = hardCap
}

// effectiveToolMaxDuration resolves the stream watchdog's never-freeze
// backstop (the max wall-clock a single tool may run while the watchdog is
// paused between OnToolCall/OnToolResult) for THIS Run() call. One value
// applies to every tool, including a sub-agent delegation via the "agent"
// tool — see toolExecutionMaxDefault's doc for why a plain/orchestrator
// split was removed in favor of a single generous cap. Precedence:
//
//  1. toolExecutionMaxDefault (45m) — the baseline.
//  2. a.toolMaxDuration (> 0) — the EXPLICIT OPERATOR OVERRIDE, from
//     Options.StreamToolTimeoutSeconds. Applied last, unconditionally, so
//     it always wins over (1) in either direction.
func (a *sessionAgent) effectiveToolMaxDuration() time.Duration {
	toolMaxDuration := toolExecutionMaxDefault
	if a.toolMaxDuration > 0 {
		toolMaxDuration = a.toolMaxDuration
	}
	return toolMaxDuration
}

// effectiveToolCleanupGrace resolves the buffer added on top of
// effectiveToolMaxDuration before the stream watchdog force-cancels a
// tool-in-flight. See toolCleanupGraceDefault's doc for why this exists.
// Precedence:
//
//  1. a.toolCleanupGrace (> 0) — an EXPLICIT OPERATOR OVERRIDE (or test
//     override via SessionAgentOptions.ToolCleanupGrace). Checked FIRST and
//     applied unconditionally, so it wins even for a sub-agent that
//     explicitly opts back in.
//  2. Otherwise: toolCleanupGraceDefault (90s) for a top-level (non-sub-agent)
//     session, or 0 (no grace) for a sub-agent session.
//
// Fork patch, task #205 (reopens #200): the grace exists ONLY to let a
// nested (child) sub-agent watchdog fire on its OWN cap and unwind cleanly
// before the PARENT's watchdog (whose clock started earlier — at OnToolCall
// for the `agent`-tool delegation, before the child's own turn has even
// begun executing) force-cancels genCtx out from under it. Giving the
// SAME grace to the sub-agent's own watchdog (task #200's original,
// symmetric fix) defeated that purpose: with identical
// toolMaxDuration+grace on both sides, the 90s cancels out of the
// "child must fire before parent" inequality algebraically, so the
// parent's unconditional head start remained the only deciding factor and
// it still always won the race. A sub-agent can never itself be waiting on
// a nested `agent`-tool delegation — the `agent` tool is excluded from
// workerToolNames for sub-agents (see coordinator.go's
// buildToolsAgentConfig/workerToolNames), so a sub-agent is always the
// deepest watchdog in the chain and never needs runway for a nested one to
// go first. Only a top-level (!isSubAgent) session's watchdog is ever the
// one waiting on a delegation, so only it gets the default grace.
func (a *sessionAgent) effectiveToolCleanupGrace() time.Duration {
	if a.toolCleanupGrace > 0 {
		return a.toolCleanupGrace
	}
	if a.isSubAgent {
		return 0
	}
	return toolCleanupGraceDefault
}

// effectiveSessionPreambleMaxDuration resolves the bound on Run()'s DB
// preamble (sessions.Get, getSessionMessages, createUserMessage) for THIS
// agent. See sessionPreambleMaxDurationDefault's doc for why this exists.
// 0 falls back to the default.
func (a *sessionAgent) effectiveSessionPreambleMaxDuration() time.Duration {
	sessionPreambleMaxDuration := sessionPreambleMaxDurationDefault
	if a.sessionPreambleMaxDuration > 0 {
		sessionPreambleMaxDuration = a.sessionPreambleMaxDuration
	}
	return sessionPreambleMaxDuration
}

// effectiveTitleGenerationMaxDuration resolves the bound on the background
// title-generation goroutine for THIS agent. See
// titleGenerationMaxDurationDefault's doc for why this exists. 0 falls back
// to the default.
func (a *sessionAgent) effectiveTitleGenerationMaxDuration() time.Duration {
	titleGenerationMaxDuration := titleGenerationMaxDurationDefault
	if a.titleGenerationMaxDuration > 0 {
		titleGenerationMaxDuration = a.titleGenerationMaxDuration
	}
	return titleGenerationMaxDuration
}

// effectiveStreamWatchdogTick resolves the interval at which the stream
// watchdog checks for stalls for THIS agent. 0 falls back to the default
// (streamWatchdogTick, 30s). Primarily exposed for tests that need fast
// watchdog behavior (e.g., P2_3 regression tests).
func (a *sessionAgent) effectiveStreamWatchdogTick() time.Duration {
	tick := streamWatchdogTick
	if a.streamWatchdogTick > 0 {
		tick = a.streamWatchdogTick
	}
	return tick
}
