package agent

import (
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

type SessionAgentOptions struct {
	SmartModel           Model
	FastModel            Model
	SystemPromptPrefix   string
	SystemPrompt         string
	IsSubAgent           bool
	DisableAutoSummarize bool
	IsYolo               bool
	Sessions             coresession.Service
	Messages             message.Service
	Tools                []fantasy.AgentTool
	// Coord is the owning coordinator (batch facts, reaction store, notice
	// pull route through it). Nil on bare fixtures.
	Coord agentcall.CoordinatorPort
	// OnSessionIdle -- see sessionAgent.onSessionIdle's doc. Nil in every
	// test that builds a bare sessionAgent directly (no coordinator to
	// notify).
	OnSessionIdle func(sessionID string)
	// Config is the MCP ownership scope for this agent. MCP runtime state is
	// process-wide, so every turn filters registry-derived instructions by
	// this consuming ConfigStore.
	Config *config.ConfigStore
	Notify pubsub.Publisher[notify.Notification]
	// StreamIdleTimeout overrides streamIdleTimeoutDefault when > 0.
	// Plumbed from Options.StreamIdleTimeoutSeconds in the coordinator.
	StreamIdleTimeout time.Duration
	// TitleJoinGrace overrides the package-level titleJoinGrace const
	// (10s) when > 0. Test-only seam (task #454) — production callers leave
	// this unset.
	TitleJoinGrace time.Duration
	// CancelAllGrace overrides CancelAll's own 5s runWg.Wait grace period
	// when > 0. Test-only seam (task #454) — production callers leave this
	// unset.
	CancelAllGrace time.Duration
	// DataDirectory is the absolute path to .rush/. Used by Run() to
	// acquire an inter-process file lock per session (prevents two
	// rush processes from accidentally working on the same session
	// id — see internal/session/lock.go).
	DataDirectory string
	// CheckpointInterval controls how often in-progress streaming
	// text is flushed to the DB mid-step. When > 0, a coalescing
	// ticker writes the in-memory Parts to the message row (with
	// finished_at still NULL) at most once per interval — but only
	// when Parts have actually changed since the last flush. This
	// bounds the text lost to a SIGTERM during final composition.
	// 0 (default) disables mid-stream checkpointing entirely.
	// Fork patch: batch 8 — see CHANGELOG.fork.md section 6.
	CheckpointInterval time.Duration
	// TimeoutExtendsOnProgress, when true, makes the stream watchdog
	// reset its deadline every time streaming progress occurs. This
	// prevents killing healthy long compositions. Default: false.
	// Fork patch: batch 8.
	TimeoutExtendsOnProgress bool
	// TimeoutHardCap is the maximum wall-clock time the watchdog will
	// allow even with continuous progress. Default: 0 (no cap, but
	// callers typically set 4x the idle timeout when extending).
	// Fork patch: batch 8.
	TimeoutHardCap time.Duration
	// ToolMaxDuration bounds the watchdog's tool-pause (never-freeze
	// backstop). Past it the watchdog fires with a "tool timeout" reason
	// so the turn ends instead of hanging on a stuck tool. 0 = use the
	// built-in toolExecutionMaxDefault (45m), applied uniformly to every
	// tool including sub-agent delegations. Explicitly set (> 0), this
	// ALWAYS wins over the built-in default — plumbed from
	// Options.StreamToolTimeoutSeconds in the coordinator.
	ToolMaxDuration time.Duration
	// ToolCleanupGrace overrides the resolved grace when > 0 — the buffer
	// added on top of the resolved tool-max-duration before the watchdog
	// force-cancels a tool-in-flight, giving a nested (child) watchdog
	// inside an `agent`-tool delegation a chance to fire on its own cap and
	// unwind cleanly first. See toolCleanupGraceDefault's and
	// effectiveToolCleanupGrace's docs for the full rationale. 0 = no
	// explicit override: the built-in default (toolCleanupGraceDefault)
	// applies only to a top-level (non-sub-agent) session; a sub-agent
	// session gets 0 (no grace) by default, since it can never itself be
	// waiting on a nested delegation (task #205). Primarily exposed for
	// tests that want a short, non-zero grace instead of waiting out the
	// real default.
	ToolCleanupGrace time.Duration
	// SessionPreambleMaxDuration overrides sessionPreambleMaxDurationDefault
	// when > 0 — the bound on Run()'s DB preamble before the stream watchdog
	// starts. See sessionPreambleMaxDurationDefault's doc for the full
	// rationale. 0 = use the built-in default; primarily exposed for tests
	// that want a short bound instead of waiting out the real one.
	SessionPreambleMaxDuration time.Duration
	// diskCaps is the coordinator's App-owned capability manager
	// (R12-H2b §2.4), wired unexported at construction: the durable enqueue
	// producer on this agent (restartOrphanedWithRetry) holds row-keyed
	// Continuation owners through it. Nil on bare fixtures — those never
	// enqueue provider-carrying calls.
	diskCaps *DiskProviderRegistry
	// TitleGenerationMaxDuration overrides titleGenerationMaxDurationDefault
	// when > 0 — the bound on the background title-generation goroutine. See
	// titleGenerationMaxDurationDefault's doc for the full rationale. 0 = use
	// the built-in default; primarily exposed for tests that want a short
	// bound instead of waiting out the real one.
	TitleGenerationMaxDuration time.Duration
	// StreamWatchdogTick overrides streamWatchdogTick when > 0 — the interval
	// at which the stream watchdog checks for stalls. 0 = use the built-in
	// default (30s); primarily exposed for tests that need fast watchdog
	// behavior (e.g., P2_3 regression tests).
	StreamWatchdogTick time.Duration
	// LockOptions allows tests to inject options into SessionLock acquisition
	// (e.g., WithClearHolderMetadataFn for hung cleanup tests). Passed to
	// session.TryAcquireSessionLockWithOptions when the agent acquires
	// inter-process locks. Primarily exposed for regression tests like
	// TestP0_338_FinalizerReachableDespiteHungCleanup.
	LockOptions []sessionlock.LockOption
	// RunAllowlists, when non-nil, lets the turn loop activate each call's
	// carried restricted-run policy (SessionAgentCall.RunAllowlist) at
	// turn start and clear it at loop end (R3-4). Nil (tests, bare
	// fixtures) disables the mechanism entirely — no session entry is
	// ever armed.
	RunAllowlists permission.SessionRunAllowlistManager
	// RestrictedRuns is the final dispatch gate for per-call tool allowlists.
	RestrictedRuns permission.RestrictedRunAuthorizer
}

// Effective timeout and duration resolution: these helpers fold the agent's
// configured overrides (SessionAgentOptions fields, SetTimeoutOptions) onto
// the package-level defaults, one resolved value per Run()/runTurn() call.
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

// watchdogTimeoutPolicyForCall resolves the stream watchdog's
// (extendsOnProgress, hardCap) policy for one call: the call's own values
// when it carries a TimeoutOptionsSet CallOptions (even all-zero — a
// deliberate policy, R3-6), otherwise the sessionAgent's shared
// SetTimeoutOptions fields for legacy callers.
func (a *sessionAgent) watchdogTimeoutPolicyForCall(call *CallOptions) (extendsOnProgress bool, hardCap time.Duration) {
	if call != nil && call.TimeoutOptionsSet {
		return call.TimeoutExtendsOnProgress, call.TimeoutHardCap
	}
	return a.timeoutExtendsOnProgress, a.timeoutHardCap
}

// effectiveIdleTimeoutForCall resolves the per-turn stream watchdog's
// idle-stall threshold: the call's own override when positive, otherwise
// the sessionAgent's shared streamIdleTimeout, otherwise
// streamIdleTimeoutDefault. Mirrors watchdogTimeoutPolicyForCall's
// call-wins-over-shared-state precedence.
func (a *sessionAgent) effectiveIdleTimeoutForCall(call *CallOptions) time.Duration {
	if call != nil && call.IdleTimeout > 0 {
		return call.IdleTimeout
	}
	if a.streamIdleTimeout > 0 {
		return a.streamIdleTimeout
	}
	return streamIdleTimeoutDefault
}
