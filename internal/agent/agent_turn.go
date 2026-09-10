// runTurn: the single-turn body — one fantasy agent.Stream call plus its DB
// preamble, stream watchdog, checkpointing, error/cancel handling, and
// auto-summarize triggering — along with its extracted helpers
// (drainDueInjects, handleWatchdogFire, logProviderWarnings).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

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
func (a *sessionAgent) drainDueInjects(sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	due := a.getMailbox(sessionID).drainInjects(genID)
	if len(due) == 0 {
		return nil
	}
	spliced := make([]message.Message, 0, len(due))
	for _, inj := range due {
		if _, ok := historyIDs[inj.msg.ID]; ok {
			continue
		}
		spliced = append(spliced, inj.msg)
	}
	return spliced
}

// handleWatchdogFire is runTurn's stream-watchdog onFire callback, pulled out
// into a named method (task #243) so a unit test can invoke the REAL
// production logic directly — constructing a minimal *sessionAgent and
// calling this method with a synthetic watchdogCauseVal — instead of only
// exercising a test-local copy of this shape that could silently drift from
// what runTurn actually wires up. The onFire closure inside runTurn is now
// just a thin dispatch: `func(elapsed, cause) { a.handleWatchdogFire(...) }`.
//
// watchdogCauseVal is runTurn's local atomic.Int32 (not a sessionAgent
// field: it is genuinely per-turn state, reset fresh on every call), passed
// in by pointer so this method can store into the caller's copy.
// toolMaxDuration/idleTimeout are likewise runTurn locals (the resolved,
// possibly-overridden effective durations for THIS turn) rather than
// sessionAgent fields, so they are passed explicitly instead of read off a.
// smartModel follows the same rule, and then some: runTurn does NOT re-read
// a.smartModel here at fire time — it passes in the value it took once,
// before the turn, from the immutable turnConfig snapshot built by
// resolveTurnConfig (agent.go:302-308), a per-call value copy of every
// shared model/prompt field (task #265 P0-1) that runTurn reads as
// `smartModel := cfg.smartModel`. a.smartModel is mutable mid-turn via
// SetModels (coordinator.UpdateModels / web-UI override path), so
// re-reading at fire time would name the model the user SWITCHED TO after
// the hang started rather than the one that actually hung (task #252 —
// the #243 extraction regressed exactly this by re-reading a.smartModel
// here).
//
// INVARIANT (task #227 + #232, preserved verbatim by this extraction): the
// cause is stored FIRST, synchronously, before any other work in this
// method. startStreamWatchdog invokes onFire strictly before cancel(), so
// whatever this method does before returning is on the critical path to
// unblocking agent.Stream. If the OUTER ctx is also cancelled independently
// of this watchdog (Ctrl-C, a --timeout firing at the same moment) while
// this method is still running, the main goroutine reading watchdogCauseVal
// after observing stalled==true must never see the zero value
// (causeIdleStall) for what was actually a hard-cap/tool-timeout fire — that
// would misreport the cause via a DIFFERENT race than the one #227 fixed
// (external cancellation racing this callback, rather than this callback
// racing its own cancel()).
func (a *sessionAgent) handleWatchdogFire(
	cause watchdogCause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
) {
	watchdogCauseVal.Store(int32(cause))
	// The watchdog firing IS the hang, caught at the only moment the
	// evidence still exists. Capture every goroutine's stack now,
	// SYNCHRONOUSLY: pprof is gated behind RUSH_PROFILE (so it can't be
	// turned on after the fact) and release builds strip symbols (so a
	// debugger attach yields nothing and merely kills the process). Without
	// this, every production hang is diagnosed by guesswork.
	//
	// CaptureGoroutineStack does no I/O — it's runtime.Stack plus a string
	// header — so it cannot block on a stuck disk, and running it
	// synchronously here is what guarantees the snapshot reflects the
	// actual moment of the hang. Capturing it from an async goroutine
	// instead (as an earlier version of this fix did) would let it run
	// AFTER cancel()/unwind had already started — or, if the process exited
	// or was force-killed first, never run at all — defeating the entire
	// point of a diagnostic taken "at the only moment it is still
	// available" (see the doc comment on CaptureGoroutineStack).
	stackDump := rushlog.CaptureGoroutineStack("stream watchdog fired")
	// Only the WRITE is dispatched async and NOT awaited: WriteGoroutineDump
	// does a synchronous os.WriteFile with no timeout of its own (see its
	// doc in internal/log/goroutine_dump.go). Since onFire now runs
	// strictly before cancel(), awaiting a write that hangs (e.g. the log
	// directory sits on a stuck network/SMB mount) would mean cancel()
	// never runs, agent.Stream never unblocks, and runTurn's deferred
	// <-wd.done blocks forever — a full process freeze, exactly the failure
	// mode this watchdog exists to prevent. The dump is best-effort
	// diagnostics only; nothing downstream needs the write to complete
	// before the turn can safely unwind, so firing it off and returning
	// immediately preserves the already-captured evidence without putting
	// an unbounded disk write on cancellation's critical path.
	go func() {
		if dumpPath, dumpErr := rushlog.WriteGoroutineDump(stackDump); dumpErr != nil {
			slog.Warn("agent: failed to write goroutine dump for watchdog fire", "err", dumpErr)
		} else {
			slog.Warn("agent: wrote goroutine dump for watchdog fire", "path", dumpPath)
		}
	}()
	switch cause {
	case causeToolTimeout:
		slog.Warn(
			"agent: watchdog firing — tool execution exceeded cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"cap", toolMaxDuration.String(),
		)
	case causeHardCap:
		slog.Warn(
			"agent: watchdog firing — turn exceeded --timeout-hard-cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"hard_cap", a.timeoutHardCap.String(),
		)
	default:
		slog.Warn(
			"agent: stream watchdog firing — no provider activity, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"idle_duration", elapsed.String(),
			"threshold", idleTimeout.String(),
		)
	}
}

// normalizeTurnError folds the two out-of-band ways a turn can end into the
// single error every downstream consumer reads, so nothing past this point
// has to know either shape.
//
// Extracted from runTurn — one of only two blocks in that function that
// cross no boundary worth worrying about: no defer, no early return, no
// closure created, no lock held, two inputs and one output. See the
// "runTurn is not being decomposed" commit for why the rest of runTurn
// stays where it is.
func normalizeTurnError(err error, getPeakHoursAbortErr func() error) error {
	// If the peak-hours mid-turn check fired, it had to call cancelFn()
	// to break fantasy's loop (OnStepFinish errors alone don't stop it).
	// Depending on exactly when fantasy notices the cancellation relative
	// to finishing the in-flight step, agent.Stream can come back with
	// context.Canceled OR — if the step's own work had already fully
	// completed by the time cancelFn() fired — a nil error, as if the
	// turn ended cleanly. Either way, once peakHoursAbortErr is set it is
	// authoritative for this Run() call: force it in unconditionally so
	// the coordinator and RunNonInteractive never mistake this abort for
	// a successful completion or a bare, unexplained cancellation.
	if peakErr := getPeakHoursAbortErr(); peakErr != nil {
		err = peakErr
	}
	// The ask_question tool reports "agent asked a question" as the Go
	// error its Run() returns; fantasy's executeSingleTool treats a
	// non-nil tool error as critical and propagates it as the whole
	// Stream() call's error, so it surfaces here exactly like the
	// peak-hours abort err normalized just above. tools.AskQuestionError
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

// runTurnToolsSnapshotSeam is a test-only hook — see its call site in
// runTurn. nil (a no-op) in every production path.
var runTurnToolsSnapshotSeam func()

// runTurn executes exactly one agent turn (one call into fantasy's
// agent.Stream, plus all of Run's surrounding bookkeeping: DB preamble,
// stream watchdog, checkpointing, error/cancel handling, and auto-summarize
// triggering). It assumes the caller (Run) already holds call.SessionID's
// busy reservation and, when configured, the inter-process OS lock — runTurn
// itself never acquires either.
//
// hasNext reports whether another turn should run immediately (a message was
// queued during this turn, e.g. via the "interrupt and send" flow, a normal
// end-of-turn queue check, or a /compact drain) with next set to that call;
// the caller's loop is expected to invoke runTurn(ctx, next) again in that
// case. When hasNext is false, result/err are Run's final return values.
//
// The callback set fantasy invokes during the Stream call below lives on
// turnStream (agent_turn_stream.go/agent_turn_step.go/agent_turn_failure.go)
// — this function builds it, calls Stream, and dispatches on the result;
// the preamble above and the tail below are deliberately NOT moved there,
// since neither is fantasy callback state.
func (a *sessionAgent) runTurn(ctx context.Context, call SessionAgentCall, lk *session.SessionLock, epoch uint64, runCancel context.CancelFunc) (res *fantasy.AgentResult, next SessionAgentCall, hasNext bool, resErr error) {
	// A real turn is starting: any stale keep-alive scheduled for this
	// session's prior idle state is moot and must not race this turn's own
	// request.
	a.cancelCacheKeepAlive(call.SessionID)

	// R3-1: consume THIS call's pinned tool slice when it carries one, so
	// no concurrent call's SetTools (UpdateModels' global rebuild) can
	// change this turn's tools — not at start, and not between steps
	// (PrepareStep below resolves from the same slice). nil keeps the
	// legacy shared behavior: copy the live slice under its lock to avoid
	// races with SetTools/SetModels.
	agentTools := call.Tools
	if agentTools == nil {
		agentTools = a.tools.Copy()
	}
	// Test-only seam: fires right after the snapshot above, before PrepareStep
	// re-reads a.tools for the actual request (see stepTools' doc). Lets a
	// test deterministically simulate a SetTools/MCP update landing in that
	// exact window, on the same goroutine, without needing real concurrency.
	// nil (a no-op) in every production path.
	if runTurnToolsSnapshotSeam != nil {
		runTurnToolsSnapshotSeam()
	}
	// One immutable snapshot for the whole turn (task #265). Resolving these
	// individually here used to mean a concurrent session's
	// applyModelOverrides could land BETWEEN the reads, so a single turn ran
	// with a mismatched model/prompt pair — and the next turn silently
	// inherited another session's model.
	cfg := a.resolveTurnConfig(call)
	smartModel := cfg.smartModel
	systemPrompt := cfg.systemPrompt
	promptPrefix := cfg.promptPrefix

	slog.Info("SessionAgent.Run: starting", "sessionID", call.SessionID, "model", smartModel.ModelCfg.Model, "promptLen", len(systemPrompt))

	var instructions strings.Builder
	for name, server := range mcp.GetStates() { // deliberately on the package wrappers: read-only getters; the IsConfigured filter below is the isolation mechanism
		if !mcp.IsConfigured(a.config, name) {
			continue
		}
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			instructions.WriteString(s)
			instructions.WriteString("\n\n")
		}
	}

	if s := instructions.String(); s != "" {
		systemPrompt += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}

	// Add Anthropic caching to the last tool. Via a per-turn wrapper, NOT
	// SetProviderOptions on the shared tool object: a.tools' ELEMENTS are one
	// set of pointers shared by every concurrent turn on this agent, and two
	// turns marking "their" last tool wrote the same field (a real -race
	// finding). See withProviderOptionsOnLast.
	agentTools = withProviderOptionsOnLast(agentTools, a.getCacheControlOptions())

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	// Bounded: see sessionPreambleMaxDurationDefault doc. No watchdog is
	// running yet at this point in Run(), so an unbounded ctx here can hang
	// the turn forever with zero diagnostics if the single DB writer
	// connection is wedged.
	preambleCtx, preambleCancel := context.WithTimeout(ctx, a.effectiveSessionPreambleMaxDuration())
	currentSession, err := a.sessions.Get(preambleCtx, call.SessionID)
	if err != nil {
		preambleCancel()
		// #284: the preamble runs inside a per-turn cancelable context
		// (turnCtx, derived from runCtx). An InterruptAndReplace during
		// the preamble cancels that context without killing the
		// dispatcher. If the preamble failed because of a cancellation,
		// the mailbox may hold a replacement that runTurn's own
		// drainAfterCancel path never reached (it only runs after
		// agent.Stream returns, and we never got there). Recover it now
		// so the loop runs it as the next turn.
		if errors.Is(err, context.Canceled) {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				return nil, next, true, nil
			}
		}
		return nil, SessionAgentCall{}, false, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(preambleCtx, currentSession)
	if err != nil {
		preambleCancel()
		if errors.Is(err, context.Canceled) {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				return nil, next, true, nil
			}
		}
		return nil, SessionAgentCall{}, false, fmt.Errorf("failed to get session messages: %w", err)
	}

	// Generate the title on the first message — OR self-heal on a later turn
	// when the session is still nameless. Title generation is best-effort and
	// a transient provider blip (z.ai overload, a token-limit truncation) on
	// turn 1 used to doom the session to "Untitled Session" forever, since it
	// only ever fired at len(msgs)==0. Retrying while the title is still
	// empty/default lets the next message recover it; it stops the moment a
	// real title lands. needsTitle is decided here (before the preamble ctx
	// is cancelled below) but the goroutine itself is launched further down,
	// after genCtx exists — see the wg.Go call site near genCtx's creation.
	needsTitle := len(msgs) == 0 ||
		currentSession.Title == "" ||
		currentSession.Title == DefaultSessionName

	// Add the user message to the session. Skip creation when the call
	// references a message that already exists in the DB (interrupt-inject
	// path: `rush sessions inject --interrupt` created the row before
	// signalling this process). Creating it again would duplicate it in
	// history — the referenced message is already the newest user message.
	//
	// Track user message creation status for task #339: errors AFTER this
	// point (whether or not currentAssistant is set) should be wrapped in
	// ErrCallAlreadyAttempted to prevent duplicate execution on retry.
	// If call.ExistingMessageID is set, the user message already exists,
	// so we're already in the "attempted" state.
	userMessageCreated := call.ExistingMessageID != ""
	if call.ExistingMessageID == "" {
		createdMsg, err := a.createUserMessage(preambleCtx, call)
		if err != nil {
			preambleCancel()
			if errors.Is(err, context.Canceled) {
				if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
					return nil, next, true, nil
				}
			}
			return nil, SessionAgentCall{}, false, err
		}
		userMessageCreated = true
		if call.OnUserMessageCreated != nil {
			call.OnUserMessageCreated(createdMsg.ID)
		}
	}
	preambleCancel()

	// Add the session to the context.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.ReasoningEffortContextKey, currentSession.SmartModelReasoningEffort)
	// Compose this turn's activity-notify callback with any ancestor's (see
	// withActivityNotify) BEFORE deriving genCtx, so every fantasy stream
	// callback below — via bumpActivity -> notifyActivity(genCtx) — records
	// activity on this session's own lock AND propagates up the whole
	// delegation chain to every ancestor session currently blocked waiting
	// on this one (task #214, the "pulse on any activity of the agent OR
	// its sub-agent(s)" directive).
	ctx = withActivityNotify(ctx, lk)

	genCtx, cancel := context.WithCancel(ctx)
	// Overwrite the placeholder no-op CancelFunc that tryReserveSession
	// stored in Run() with the real one for this turn. The reservation
	// itself (i.e. the map entry existing at all under call.SessionID) is
	// owned and released by Run(), not per-turn — see the removed
	// `defer a.activeRequests.Del(call.SessionID)` note below.
	a.activeRequests.Set(call.SessionID, cancel)
	// Record this turn's genCtx cancel as the mailbox's current generation
	// cancel (design §4): InterruptAndReplace returns THIS func (so a
	// mid-stream interrupt cancels only this generation, leaving the Run
	// turn loop alive to drain the replacement), and Cancel(sessionID)
	// targets it for a bare abort. Redundant with Run's loop-level
	// beginGeneration(runCancel) only for the brief preamble window this
	// call closes; from here until the turn returns, THIS is the live
	// cancel an interrupt must hit.
	genID := a.getMailbox(call.SessionID).beginGeneration(cancel)

	// Launches the title-generation goroutine; joined later via
	// turnTitleJoiner once the stream watchdog exists. See
	// startTitleGeneration's doc in agent_turn_title.go for why launch and
	// join are deliberately NOT the same constructor.
	titleDone := startTitleGeneration(a, genCtx, needsTitle, call.SessionID, call.Prompt, cfg)
	// The bounded join for this goroutine is declared AFTER `defer cancel()`
	// below, which by LIFO makes it run BEFORE it. That ordering is the
	// whole point — see the comment at the join itself.

	// Stream-progress watchdog (see streamWatchdog doc in stream_watchdog.go
	// for the invariant). Every fantasy stream callback below calls
	// bumpActivity(); if no callback fires for idleTimeout, the watchdog
	// cancels genCtx and the agent.Stream call below returns with
	// context.Canceled, routing into the error path that records
	// FinishReasonError("Stream stalled") on the assistant message.
	idleTimeout := streamIdleTimeoutDefault
	if a.streamIdleTimeout > 0 {
		idleTimeout = a.streamIdleTimeout
	}
	toolMaxDuration := a.effectiveToolMaxDuration()
	toolCleanupGrace := a.effectiveToolCleanupGrace()
	// R1-1: resolve the watchdog's deadline-extension policy per call.
	// The sessionAgent's shared timeoutExtendsOnProgress/timeoutHardCap
	// fields are written by SetTimeoutOptions — previously per-run from
	// ExecuteRun, so two overlapping runs raced: one run's
	// --timeout-extends-on-progress/--timeout-hard-cap could land in the
	// other's watchdog. A call carrying CallOptions uses its own values;
	// everyone else keeps the shared fields exactly as before.
	// R3-6: presence (TimeoutOptionsSet), not zero-ness, decides — the
	// old (extends || hardCap > 0) heuristic made a deliberate "no
	// extension, no cap" CallOptions fall through to the shared fields,
	// possibly another run's.
	timeoutExtends, timeoutHardCap := a.watchdogTimeoutPolicyForCall(call.CallOptions)
	var watchdogCauseVal atomic.Int32 // stores watchdogCause
	wd := startStreamWatchdog(
		genCtx, cancel, idleTimeout, a.effectiveStreamWatchdogTick(),
		// The closure itself is deliberately a thin dispatch to a named
		// method (task #243): a unit test can construct a minimal
		// *sessionAgent and call handleWatchdogFire directly, exercising
		// the REAL cause-store/dump-capture/dump-write ordering instead of
		// a test-local copy of this shape that could silently drift from
		// what agent.go actually does.
		func(elapsed time.Duration, cause watchdogCause) {
			a.handleWatchdogFire(cause, elapsed, call.SessionID, &watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
		},
		timeoutExtends,                    // Fork patch: batch 8 (per-call via CallOptions, R1-1)
		timeoutHardCap,                    // Fork patch: batch 8 (per-call via CallOptions, R1-1)
		toolMaxDuration,                   // never-freeze backstop, applies to every tool
		toolCleanupGrace,                  // buffer for a nested watchdog to unwind first
		func() { notifyActivity(genCtx) }, // task #222/#300: recordActivity is
		// invoked on every REAL bump() (stream progress) only — deliberately
		// NOT on a timer while a tool is merely in flight. See
		// startStreamWatchdog's recordActivity doc for why the tool-tick case
		// was removed.
	)
	// The watchdog now calls recordActivity (== notifyActivity(genCtx))
	// internally on every bump(), so this wrapper can be just wd.bump.
	bumpActivity := wd.bump
	// Store wd.bump in genCtx so runSummarizeBody and runSummarizeSilent
	// can report LLM streaming progress during compaction (task #310).
	genCtx = withWatchdogBump(genCtx, wd.bump)
	// toolStarted/toolFinished bracket tool execution so the watchdog pauses
	// its idle timer while a (possibly long) tool runs — see streamWatchdog.
	toolStarted := wd.toolStarted
	toolFinished := wd.toolFinished
	// Defer order matters: <-wd.done is deferred FIRST so it runs LAST
	// (LIFO), AFTER cancel() has signalled the goroutine to exit.
	// Without this the wait would deadlock the function return.
	defer func() { <-wd.done }()
	defer cancel()

	// Bounded join (P1-B) for the title goroutine.
	//
	// It is a named function called from TWO places, and #525 is why. The
	// join used to exist only as a defer, and runTurn calls cancel()
	// EXPLICITLY near its end — before any defer runs — so on the success
	// path titleCtx (derived from genCtx) was already cancelled by the time
	// anything waited for it. The observed failure: a session left
	// "Untitled Session" with "Error generating title with fast model;
	// trying next err=context canceled" for both models. It surfaced as a
	// ~1-in-27 flake only because the mock in the older test answers
	// instantly and usually wins that race.
	//
	// So the success path joins BEFORE that explicit cancel, and the defer
	// remains for every early return that never reaches it. sync.Once keeps
	// a turn from paying the grace period twice.
	//
	// It stays bounded, which is what the original P1-B fix added:
	// generateTitle's attempts are blocking agent.Stream calls with no
	// timeout of their own, so a provider that ignores context cancellation
	// never returns, and waiting unconditionally once held runTurn — and
	// with it Run, the session's mailbox ownership and its OS lock — open
	// forever on a turn whose work had finished. We wait up to a grace
	// period and otherwise abandon it: the goroutine exits whenever its
	// provider unblocks, but abandoning it DOES lose the real title.
	// generateTitle's actual a.sessions.Rename call runs on titleCtx itself
	// (cancellable, derived from genCtx below), not a detached context —
	// only its FALLBACK path (stamping the default "Untitled Session" name)
	// uses context.WithoutCancel, precisely so that fallback can still land
	// after the caller gives up. So once cancel() fires below, a
	// late-finishing title attempt fails to save and the fallback stamps
	// the default instead. See agent_title.go's titleJoinGrace doc for the
	// full accounting.
	titleJoiner := newTurnTitleJoiner(wd, titleDone, a.titleJoinGrace, call.SessionID)
	joinTitle := titleJoiner.join
	defer joinTitle()
	// NOTE: no `defer a.activeRequests.Del(call.SessionID)` here (unlike the
	// pre-turn-loop code). The busy reservation for call.SessionID is
	// claimed once and released once by Run(), covering every turn in the
	// loop — a per-turn Del here would drop the reservation between queued
	// turns, reopening the exact race tryReserveSession exists to close.
	// runTurn itself never calls a.activeRequests.Del(call.SessionID) at
	// all — only Run()'s own releaseSessionReservation does, exactly once,
	// after the whole turn loop ends (see Run()'s defer). Earlier revisions
	// of this comment described mid-loop Del(call.SessionID) calls inside
	// runTurn (a cancel-drain path and an end-of-turn queue-check); both
	// were removed when the turn loop was extracted into Run() — this
	// comment previously went stale describing code that no longer exists.
	//
	// Fork merge note (origin/main 6938dedd "perf: batch streaming message
	// updates"): upstream introduced a debounced flush layer in
	// message.Service. We removed that layer (see message/message.go fork
	// patch); our Notify() path goes through pubsub directly and Update()
	// writes synchronously, so there is nothing to flush here.

	history, files := a.preparePrompt(msgs, currentSession.Todos, call.Attachments...)

	// historyIDs is the dedup set for mailbox-injected messages (design §5):
	// an inject whose DB row was already loaded into msgs by this turn's
	// preamble must not be spliced again from the mailbox's injects queue.
	// drainInjects + this ID check together guarantee exactly-once delivery
	// regardless of whether the DB write landed before or after the
	// preamble's getSessionMessages call.
	historyIDs := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		historyIDs[m.ID] = struct{}{}
	}

	// stepHistory/loopDetected/loopDetail/sanitizedToolCalls/shouldSummarize/
	// silentCompactNeeded/currentAssistant/currentSession/stepMessages/
	// stepTools/the tool-boundary phase tracker all live on turnStream now
	// (agent_turn_stream.go) — the fantasy callbacks that touch them are
	// turnStream methods, not inline closures, so this function no longer
	// declares any of them itself.
	ts := newTurnStream(turnStreamConfig{
		a:                a,
		ctx:              ctx,
		genCtx:           genCtx,
		cancel:           cancel,
		call:             call,
		genID:            genID,
		smartModel:       smartModel,
		promptPrefix:     promptPrefix,
		historyIDs:       historyIDs,
		currentSession:   currentSession,
		bumpActivity:     bumpActivity,
		toolStarted:      toolStarted,
		toolFinished:     toolFinished,
		wd:               wd,
		watchdogCauseVal: &watchdogCauseVal,
		toolMaxDuration:  toolMaxDuration,
		timeoutHardCap:   timeoutHardCap,
		idleTimeout:      idleTimeout,
	})

	// Aborts this turn when the provider enters its peak-hours window
	// mid-stream. See peakHoursWatcher's doc in agent_turn_peakhours.go.
	peakHours := newPeakHoursWatcher(a, call.SessionID, ctx, genCtx, &ts.mu, &ts.currentAssistant)
	ts.setPeakHoursAbortErr = peakHours.setAbortErr
	getPeakHoursAbortErr := peakHours.getAbortErr

	// Mid-stream persistence (Fork patch: batch 8). See
	// turnCheckpointWriter's doc in agent_turn_checkpoint.go for the full
	// design -- generation fencing, why the exit signal is a dedicated
	// channel and not genCtx, and the mu/no-lock-across-Update invariant it
	// shares with turnStream's other callbacks.
	checkpoint := newTurnCheckpointWriter(a, call.SessionID, genCtx, &ts.mu, &ts.currentAssistant)
	ts.startCheckpoint = checkpoint.start
	ts.stopCheckpoint = checkpoint.stop

	// Decouples token arrival from UI render rate. See turnUINotifier's
	// doc in agent_turn_ui_notify.go.
	uiNotifier := newTurnUINotifier(a, genCtx, &ts.mu, &ts.currentAssistant)
	uiNotifier.start()
	ts.notifyUI = uiNotifier.notify
	ts.drainPendingUI = uiNotifier.drainPending

	peakHoursWatchDone := peakHours.start()
	if a.peakHoursCheck != nil {
		defer func() { cancel(); <-peakHoursWatchDone }()
	}

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err := agent.Stream(genCtx, ts.streamCall(history, files, maxOutputTokens))
	// Defensive: normally OnStepFinish stops the checkpoint ticker (via
	// stopCheckpoint()) before its own final write. But if agent.Stream
	// returned an error before any step completed (e.g. the very first
	// provider call failed), OnStepFinish never ran and the ticker
	// goroutine may still be alive — it would otherwise race with the
	// unlocked currentAssistant touches below. stopCheckpoint() is safe to
	// call more than once: after the first call the stop channel is nil'd,
	// so subsequent calls hit the nil guard and return immediately (no
	// second wait, no double-close).
	ts.stopCheckpoint()
	err = normalizeTurnError(err, getPeakHoursAbortErr)
	if err != nil {
		return ts.handleStreamFailure(result, err, userMessageCreated)
	}

	if ts.shouldSummarize {
		// Run the compaction inline (runSummarizeBody, not the public
		// Summarize/runSummarize path) so it never calls back into Run():
		// Run() is still on the stack here, holding the OS lock and the
		// busy reservation for call.SessionID for the whole turn loop. This
		// call already holds the mailbox via the turn loop's submit, so
		// runSummarizeBody does not touch ownership at all — it only performs
		// the summarisation body itself. Anything queued during the summarize
		// stream simply stays parked in mailbox.submitted for THIS function's
		// own end-of-turn drainOrRelease call, a few lines below, to pick up
		// — the single true final drain point for the whole turn, summarize
		// included.
		summarizeErr := a.runSummarizeBody(genCtx, call.SessionID, call.ProviderOptions, smartModel, promptPrefix)
		if summarizeErr != nil {
			return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: summarizeErr}
		}
		// If the agent wasn't done...
		ts.mu.Lock()
		hasPendingToolCalls := len(ts.currentAssistant.ToolCalls()) > 0
		ts.mu.Unlock()
		if hasPendingToolCalls {
			// P0-2 fix: create continuation call and return it directly as the
			// next turn. This is INTERNAL continuation of the same logical execution,
			// NOT a new external submit, so it bypasses the P0-1 durable guard in
			// mailbox.submit. The continuation MUST execute before any Ack, and
			// returning it here guarantees it runs in the same process/loop sequence.
			// Returning early here means drainOrReleaseMerged below is never reached,
			// which is correct: we're not releasing ownership yet, we're continuing.
			continuationCall := call
			continuationCall.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			return nil, continuationCall, true, nil
		}
	}

	// Silent compact of the oldest half (P0-4/#268): runs synchronously under
	// the turn's mailbox ownership — not as a background goroutine — so no
	// concurrent turn or compaction can delete/rewrite history while it is in
	// flight. Skipped when shouldSummarize already ran a full compaction
	// above (the two would be redundant, and running both would race for
	// SummaryMessageID). genCtx is still alive here (cancel() hasn't fired
	// yet), so Cancel(sessionID) can interrupt this if needed.
	if !ts.shouldSummarize && ts.silentCompactNeeded {
		if silentErr := a.runSummarizeSilent(genCtx, call.SessionID, call.ProviderOptions, smartModel, promptPrefix); silentErr != nil {
			slog.Warn("silent summarise failed", "session_id", call.SessionID, "err", silentErr)
		}
	}

	// Wait for the title BEFORE cancelling: titleCtx is derived from genCtx,
	// so cancelling first would kill a title that is merely slower than the
	// turn — which is exactly what #525 was. Bounded, and a no-op when the
	// title already landed or none was requested.
	joinTitle()

	cancel()

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions).
	if !call.NonInteractive && a.notify != nil {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:    call.SessionID,
			SessionTitle: ts.currentSession.Title,
			Type:         notify.TypeAgentFinished,
		})
	}

	// Atomic final drain-or-release (design §3, closes P0-3). Before the
	// mailbox migration, the equivalent check was a separate
	// messageQueue.PopFront call, with the actual reservation release
	// happening SEPARATELY and LATER — only once Run's own deferred
	// releaseSessionReservation ran, after this whole call had already
	// returned hasNext=false. That gap was the lost-wakeup window: a
	// concurrent submit landing in it would see the session still "busy",
	// queue itself, and never be drained by anyone, since this function had
	// already decided "nothing queued" and moved on. drainOrReleaseMerged
	// makes the emptiness check, the ownership release, and the OS lock
	// release one atomic operation under the mailbox's own lock — no
	// concurrent submit can land in a gap that no longer exists.
	firstQueuedMessage, ok := a.drainOrReleaseMerged(call.SessionID, epoch, lk, runCancel)
	if !ok {
		return result, SessionAgentCall{}, false, err
	}
	// There are queued messages — the caller's loop runs another turn.
	return nil, firstQueuedMessage, true, nil
}
