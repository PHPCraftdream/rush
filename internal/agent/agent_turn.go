// runTurn: the single-turn body — one fantasy agent.Stream call plus its DB
// preamble, stream watchdog, checkpointing, error/cancel handling, and
// auto-summarize triggering — along with its extracted helpers
// (drainDueInjects, handleWatchdogFire, logProviderWarnings).
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/stringext"
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
func (a *sessionAgent) runTurn(ctx context.Context, call SessionAgentCall, lk *session.SessionLock, epoch uint64, runCancel context.CancelFunc) (res *fantasy.AgentResult, next SessionAgentCall, hasNext bool, resErr error) {
	// A real turn is starting: any stale keep-alive scheduled for this
	// session's prior idle state is moot and must not race this turn's own
	// request.
	a.cancelCacheKeepAlive(call.SessionID)

	// Copy mutable fields under lock to avoid races with SetTools/SetModels.
	agentTools := a.tools.Copy()
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
	for _, server := range mcp.GetStates() {
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

	if len(agentTools) > 0 {
		// Add Anthropic caching to the last tool.
		agentTools[len(agentTools)-1].SetProviderOptions(a.getCacheControlOptions())
	}

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	sessionLock := sync.Mutex{}

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

	// Closed by the title goroutine when it returns; nil when no title was
	// requested. The deferred bounded join below selects on it. A
	// sync.WaitGroup used to serve this role, but a WaitGroup can only be
	// waited on unconditionally — and an unconditional wait is exactly the
	// defect being fixed here (see the join's own comment).
	var titleDone chan struct{}
	if needsTitle {
		// Derived from genCtx (not the outer ctx runTurn was called with) so
		// the stream watchdog cancelling genCtx — idle timeout, tool
		// timeout, hard cap — also cuts off an in-flight title generation
		// instead of leaving it to run on an unbounded parent context. Also
		// independently capped by effectiveTitleGenerationMaxDuration as a
		// backstop: generateTitle's two model attempts are each a blocking
		// agent.Stream with no timeout of their own, so a provider that
		// never returns (hung connection, stream never closed) must not be
		// able to keep the deferred join below from returning even if
		// genCtx's own cancellation somehow doesn't unblock it.
		titleCtx, titleCancel := context.WithTimeout(genCtx, a.effectiveTitleGenerationMaxDuration())
		titleDone = make(chan struct{})
		// Safe without admission gate: runWg.Add(1) here is always called
		// from inside an already-admitted Run() call, so runWg counter is
		// guaranteed >= 1 at this point. Per sync.WaitGroup contract, Add(1)
		// when counter is > 0 may happen at any time, including concurrently
		// with Wait. The real race P1-1 closes is Add starting when counter is
		// zero and Wait starts between the check and the Add.
		a.runWg.Add(1)
		go func() {
			defer close(titleDone)
			defer a.runWg.Done()
			defer titleCancel()
			a.generateTitle(titleCtx, call.SessionID, call.Prompt, cfg)
		}()
	}
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
		a.timeoutExtendsOnProgress,        // Fork patch: batch 8
		a.timeoutHardCap,                  // Fork patch: batch 8
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
	var titleJoinOnce sync.Once
	joinTitle := func() {
		titleJoinOnce.Do(func() {
			// Disarm the watchdog FIRST, on every path that reaches this
			// Once body — not just the success path. joinTitle is called
			// both explicitly (success path, right before the final
			// cancel()) and via the deferred call registered right after
			// this closure is declared, which is what actually runs on
			// EVERY early return (error paths, cancel-drain, summarize
			// failure, ...). Those early returns used to reach only the
			// bare defer, with disarm() sitting after them on the
			// success-path-only tail of runTurn — so an early return could
			// leave the watchdog armed for the whole titleJoinGrace wait
			// below. The turn's real work is finished by the time ANY
			// caller of joinTitle runs (an early return means the turn is
			// already ending; the success path has already produced its
			// result); all that remains from here is this bounded wait and
			// the eventual cancel(). --timeout-hard-cap is absolute from
			// turn start rather than idle-based, so without this a turn
			// that finished just inside the cap could be pushed over it by
			// the wait alone -- firing a stall dump for a turn that had
			// already finished, and cancelling the very title being waited
			// for. The goroutine still exits on genCtx and is still joined
			// by the deferred <-wd.done regardless of disarm.
			wd.disarm()
			if titleDone == nil {
				return
			}
			grace := titleJoinGrace
			if a.titleJoinGrace > 0 {
				grace = a.titleJoinGrace
			}
			select {
			case <-titleDone:
			case <-time.After(grace):
				slog.Warn(
					"agent: abandoning title generation that outlived its deadline — the turn is not held open for it",
					"session_id", call.SessionID,
					"grace", grace,
				)
			}
		})
	}
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

	var currentAssistant *message.Message
	var stepMessages []fantasy.Message
	// stepTools is the tools list PrepareStep actually decided on for the
	// most recent step (prepared.Tools, re-read from a.tools.Copy() there —
	// see that assignment's own "use latest tools" comment) — NOT the outer
	// agentTools snapshot above, which can go stale if SetTools/an MCP update
	// lands between turn start and a step's PrepareStep. Cache-related code
	// that needs "what was actually sent" (the keep-alive replay) must use
	// this, not agentTools.
	var stepTools []fantasy.AgentTool
	var shouldSummarize bool
	// sanitizedToolCalls tracks tool call IDs whose input JSON was malformed
	// and got replaced with "{}" by sanitizeToolInput, so OnToolResult can
	// surface a clear error to the model instead of letting it silently
	// operate on empty args (or, worse, get stuck resending unparsable input
	// on every subsequent turn).
	sanitizedToolCalls := make(map[string]bool)

	// stepHistory accumulates every fantasy.StepResult seen by OnStepFinish,
	// in arrival order. fantasy's internal Run loop calls OnStepFinish for a
	// step BEFORE it evaluates StopWhen on that same step, so the
	// loop-detection StopWhen closure cannot set a flag in time for that
	// step's OnStepFinish. We therefore recompute loop detection directly in
	// OnStepFinish from our own history (the StopWhen closure still calls
	// hasRepeatedToolCalls independently to decide whether to break the loop
	// — a small amount of redundant compute is simpler than sharing state
	// across fantasy's OnStepFinish-before-StopWhen ordering boundary).
	var stepHistory []fantasy.StepResult

	// loopDetected / loopDetail are computed inside OnStepFinish (from
	// stepHistory) so the AddFinish call in the SAME callback invocation can
	// record a non-empty message/details. The finish REASON stays
	// FinishReasonEndTurn (a loop-detected stop is still a form of "done" and
	// must not be reclassified away from it — see the comment on loopDetail
	// in loop_detection.go); the distinction from a voluntary model finish
	// is carried in the Finish part's message/details text so an
	// operator/orchestrator can tell that a legitimate polling pattern may
	// have been truncated.
	var loopDetected bool
	var loopDetail loopDetail
	// peakHoursAbortErr is stashed by the peak-hours checks when they detect
	// the provider entered its window mid-turn. The checks must call
	// cancelFn() to break fantasy's agent loop (returning an error alone
	// doesn't stop it), but cancel() makes fantasy return context.Canceled —
	// swallowing the specific *PeakHoursError. After agent.Stream() returns,
	// Run() checks this and replaces the generic context.Canceled with the
	// real error so it reaches the coordinator and ultimately
	// RunNonInteractive's stderr output.
	var peakHoursAbortMu sync.Mutex
	var peakHoursAbortErr error
	setPeakHoursAbortErr := func(err error) bool {
		peakHoursAbortMu.Lock()
		defer peakHoursAbortMu.Unlock()
		if peakHoursAbortErr != nil {
			return false
		}
		peakHoursAbortErr = err
		return true
	}
	getPeakHoursAbortErr := func() error {
		peakHoursAbortMu.Lock()
		defer peakHoursAbortMu.Unlock()
		return peakHoursAbortErr
	}

	// silentCompactNeeded records that PrepareStep's sliding-window trim
	// fired this turn. The silent compact runs SYNCHRONOUSLY after the turn's
	// main work completes (under the turn's mailbox ownership), not as a
	// background goroutine — a goroutine that deleted messages concurrently
	// with the active turn was the P0-4 data-corruption bug (#268). Running
	// synchronously under the same ownership guarantees no concurrent turn or
	// compaction can touch the history while the compact's snapshot/delete
	// is in flight.
	var silentCompactNeeded bool

	// Fork patch: batch 8 — auto-checkpoint state for mid-stream
	// persistence. See CHANGELOG.fork.md section 6.
	//
	// Invariant: sessionLock (already declared above) protects EVERY
	// touch of currentAssistant — mutation, Clone(), and even a bare
	// len(Parts)/pointer read — because the checkpoint goroutine below
	// and the streaming callbacks (OnTextDelta, OnReasoningDelta,
	// OnToolInputStart, ...) run concurrently on separate goroutines.
	// message.Message.Clone() has no synchronization of its own, so a
	// snapshot must be taken while holding sessionLock.
	//
	// The lock must NEVER be held across a.messages.Update (the SQLite
	// write): every writer takes the lock, mutates/clones a private
	// snapshot, releases the lock, then calls Update on the snapshot
	// without the lock held. Otherwise each checkpoint tick or
	// DB-writing callback would stall the whole streaming loop for the
	// duration of a disk write. OnStepFinish drains the ticker and
	// stops the goroutine (via stopCheckpoint) before its final write;
	// runTurn's own tail also calls stopCheckpoint() defensively before
	// touching currentAssistant, in case agent.Stream returned before
	// OnStepFinish ever ran (e.g. the very first provider call failed).
	//
	// checkpointGeneration fences concurrent checkpoint writes across turns:
	// each startCheckpoint increments it, the goroutine captures the current
	// value at launch, and stopCheckpoint returns immediately only after
	// observing the goroutine's done signal. This ensures a hung checkpoint
	// from turn N cannot race with a new checkpoint from turn N+1, and that
	// stopCheckpoint's 5s timeout is reflected in runWg (see P0-4 fix below).
	// P0-2: checkpointGeneration and checkpointMu synchronize all access to
	// the fencing state; the old checkpointPartsLen shared variable is gone
	// (each generation now tracks its own lastPartsLen locally).
	//
	// checkpointStop and checkpointDone are reborn on every step.
	// startCheckpoint allocates a fresh pair and launches the ticker
	// goroutine; stopCheckpoint closes checkpointStop — the goroutine's
	// dedicated exit signal — then waits on checkpointDone for it to
	// actually exit, then nils both so the next step starts clean. The
	// exit signal MUST be a dedicated channel, NOT genCtx.Done(): genCtx
	// stays alive for the whole body of Run (cancelled only by the
	// deferred cancel() at function return), so relying on it would force
	// stopCheckpoint to always hit its 5s backstop — the ~10s/turn stall
	// this code replaces. start/stop run on fantasy's single callback
	// goroutine / the Run goroutine (never concurrent with each other);
	// the ticker goroutine captures local channel refs at launch, so
	// nil-ing the outer vars after stop does not affect it. currentAssistant
	// access stays guarded by sessionLock below.
	var checkpointGeneration int64
	var checkpointStop chan struct{}
	var checkpointDone chan struct{}
	var checkpointWriteCancel context.CancelFunc
	// P0-2: Synchronize checkpointGeneration access with a dedicated mutex.
	// This prevents races between startCheckpoint (writes) and the goroutine
	// (reads). The old checkpointPartsLen shared variable is now dead code;
	// each generation tracks its own lastPartsLen locally for coalescing.
	var checkpointMu sync.Mutex
	startCheckpoint := func() {
		if a.checkpointInterval <= 0 || checkpointStop != nil {
			return
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		checkpointStop = stop
		checkpointDone = done
		// Fence the checkpoint writer so a hung write from turn N
		// cannot race with a new writer from turn N+1. The goroutine
		// captures the current generation at launch; stopCheckpoint returns
		// only after observing the goroutine's done signal.
		// P0-2: Access checkpointGeneration under checkpointMu to prevent races.
		checkpointMu.Lock()
		checkpointGeneration++
		myGeneration := checkpointGeneration
		checkpointMu.Unlock()
		// Give the DB write its own cancelable context with a deadline (not genCtx, which
		// stays alive for the whole Run call). This allows stopCheckpoint to actually
		// cancel an in-flight Update, not just wait forever. The deadline (30s) bounds
		// the maximum time a hung write can hold a DB connection even if cancel races.
		writeCtx, writeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		checkpointWriteCancel = writeCancel
		// Register in runWg so a timeout reflects in stillBusy (P0-4).
		a.runWg.Add(1)
		go func() {
			defer a.runWg.Done()
			defer close(done)
			defer writeCancel()
			ticker := time.NewTicker(a.checkpointInterval)
			defer ticker.Stop()
			// P0-2: Keep coalescing state LOCAL to this generation to eliminate
			// cross-generation races. Each writer tracks its own lastPartsLen and
			// only writes if there's new content since its last write.
			lastPartsLen := 0
			for {
				select {
				case <-stop:
					return
				case <-genCtx.Done():
					return
				case <-ticker.C:
					// stopCheckpoint closes `stop` BEFORE cancelling writeCtx
					// (see its comment), so a tick that was already queued
					// while the previous write was blocked can become ready
					// in the SAME instant `stop` does — select's case order
					// above gives no priority, so it can pick ticker.C over
					// stop by chance. Re-checking non-blockingly here closes
					// that window: a write must never be attempted with a
					// writeCtx that stopCheckpoint may have already
					// cancelled, since that write's failure would be
					// indistinguishable from any other cancellation to the
					// caller but still represents wasted, racy work the
					// goroutine was explicitly told to stop before reaching.
					select {
					case <-stop:
						return
					case <-genCtx.Done():
						return
					default:
					}
					sessionLock.Lock()
					var snap message.Message
					haveSnap := false
					var currentPartsLen int
					// P0-2: Access checkpointGeneration under checkpointMu to prevent races.
					checkpointMu.Lock()
					isCurrentGen := myGeneration == checkpointGeneration
					checkpointMu.Unlock()
					if currentAssistant != nil && isCurrentGen {
						currentPartsLen = len(currentAssistant.Parts)
						// Only write if we have new content since our last write.
						// This is per-generation coalescing: each writer independently
						// skips redundant DB writes, but doesn't interfere with other
						// generations.
						if currentPartsLen != lastPartsLen {
							snap = currentAssistant.Clone()
							// Stamp the snapshot with THIS writer's generation
							// so the conditional update can reject it if a
							// newer writer already wrote (P1-3). The isCurrentGen
							// check above is not enough on its own: it runs
							// here, under sessionLock, while the DB write below
							// happens after the lock is released.
							snap.CheckpointGeneration = myGeneration
							snap.AddFinish(message.FinishReasonUnknown, "", "")
							for i := len(snap.Parts) - 1; i >= 0; i-- {
								if f, ok := snap.Parts[i].(message.Finish); ok {
									f.Partial = true
									snap.Parts[i] = f
									break
								}
							}
							haveSnap = true
						}
					}
					sessionLock.Unlock()
					if haveSnap {
						// P0-4: use writeCtx (cancelable) not genCtx, so
						// stopCheckpoint can actually cancel a hung DB write.
						// P0-2: writeCtx now has both cancel AND a 30s deadline.
						if err := a.messages.Update(writeCtx, snap); err != nil {
							// Don't log cancelled errors as failures — they're
							// the expected outcome of stopCheckpoint fencing.
							if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
								slog.Debug(
									"agent: checkpoint flush failed",
									"session_id", call.SessionID,
									"message_id", snap.ID,
									"err", err,
								)
							}
						} else {
							// Update our local coalescing state on successful write.
							// No shared state update: checkpointPartsLen is now dead code.
							lastPartsLen = currentPartsLen
						}
					}
				}
			}
		}()
	}
	stopCheckpoint := func() {
		if checkpointStop == nil {
			return
		}
		close(checkpointStop)
		checkpointStop = nil
		// Cancel the write's own context immediately rather than waiting out
		// the 5s grace below first (P0-4). A checkpoint write in flight when
		// stop is requested has nothing left to accomplish — the turn is
		// ending — so there is no reason to let it keep holding a DB
		// connection for up to 5 more seconds (or longer, unbounded, if the
		// underlying driver never itself times out) before this function
		// even starts waiting. If the goroutine is between ticks (not
		// writing), cancelling here is a harmless no-op; the ticker loop's
		// own <-stop case still handles the ordinary shutdown path.
		if checkpointWriteCancel != nil {
			checkpointWriteCancel()
			checkpointWriteCancel = nil
		}
		select {
		case <-checkpointDone:
		case <-time.After(5 * time.Second):
			slog.Warn(
				"agent: checkpoint goroutine did not exit within 5s of stop signal",
				"session_id", call.SessionID,
			)
		}
		checkpointDone = nil
	}

	// latestMsgCh holds at most one pending UI snapshot (latest-value semantics).
	// A ticker goroutine drains it at ~20fps, decoupling the token arrival rate
	// from the bubbletea render rate so streaming is visible in the UI.
	latestMsgCh := make(chan message.Message, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-genCtx.Done():
				// Flush any final pending snapshot before exiting.
				select {
				case msg := <-latestMsgCh:
					a.messages.Notify(msg)
				default:
				}
				return
			case <-ticker.C:
				select {
				case msg := <-latestMsgCh:
					a.messages.Notify(msg)
				default:
				}
			}
		}
	}()

	// notifyUI enqueues the latest assistant snapshot for the ticker goroutine.
	// It never blocks: if the channel already has a pending snapshot, the old
	// one is discarded and replaced with the newest state.
	notifyUI := func() error {
		sessionLock.Lock()
		if currentAssistant == nil {
			sessionLock.Unlock()
			return nil
		}
		msg := currentAssistant.Clone()
		sessionLock.Unlock()
		select {
		case latestMsgCh <- msg:
		default:
			// Channel full — discard stale snapshot and enqueue fresh one.
			select {
			case <-latestMsgCh:
			default:
			}
			select {
			case latestMsgCh <- msg:
			default:
			}
		}
		return nil
	}

	// Fork patch: batch 8 — track final composition phase for forensic
	// logging. Set to true on each tool boundary; OnTextDelta checks and
	// resets it to emit at most once per step.
	sawToolBoundary := true

	peakHoursWatchDone := make(chan struct{})
	if a.peakHoursCheck != nil {
		go func() {
			defer close(peakHoursWatchDone)
			ticker := time.NewTicker(peakHoursPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-genCtx.Done():
					return
				case <-ticker.C:
					pErr := a.peakHoursCheck()
					if pErr == nil {
						continue
					}
					if !setPeakHoursAbortErr(pErr) {
						return
					}
					slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
						"session_id", call.SessionID, "error", pErr)
					peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
					sessionLock.Lock()
					var snap message.Message
					haveSnap := currentAssistant != nil
					if haveSnap {
						currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
						snap = currentAssistant.Clone()
					}
					sessionLock.Unlock()
					if haveSnap {
						flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
						if uErr := a.messages.Update(flushCtx, snap); uErr != nil {
							slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
						}
						flushCancel()
					}
					if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
						cancelFn()
					}
					return
				}
			}
		}()
		defer func() { cancel(); <-peakHoursWatchDone }()
	} else {
		close(peakHoursWatchDone)
	}

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(call.SessionID),
		ProviderOptions:  call.ProviderOptions,
		MaxOutputTokens:  maxOutputTokens,
		TopP:             call.TopP,
		Temperature:      call.Temperature,
		PresencePenalty:  call.PresencePenalty,
		TopK:             call.TopK,
		FrequencyPenalty: call.FrequencyPenalty,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			// PrepareStep runs before the first token of the step and can
			// take non-trivial time (sliding-window trim, background
			// summarise kickoff, cache-control wiring). Bump first so a
			// slow prepare doesn't trip the watchdog before the stream
			// even starts.
			bumpActivity()
			prepared.Messages = options.Messages
			for i := range prepared.Messages {
				prepared.Messages[i].ProviderOptions = nil
			}

			// Use latest tools (updated by SetTools when MCP tools change).
			prepared.Tools = a.tools.Copy()

			for _, inj := range a.drainDueInjects(call.SessionID, genID, historyIDs) {
				prepared.Messages = append(prepared.Messages, inj.ToAIMessage()...)
			}

			// Cross-process inject drain: rows written by another process
			// (`rush sessions inject`) into pending_injects. The message
			// row already exists in the DB (the CLI created it at inject
			// time for immediate web-UI visibility), so we only load it by
			// message_id and splice it in — no second Create, no dup row.
			// DrainPendingInjects deletes the consumed non-interrupt rows in
			// the same transaction (delete-after-read).
			pending, hasInterrupt, drainErr := a.sessions.DrainPendingInjects(callContext, call.SessionID)
			if drainErr != nil {
				return callContext, prepared, drainErr
			}
			if hasInterrupt {
				// Defensive: interrupt rows are meant to be consumed by the
				// interrupt ticker before PrepareStep runs. If one is still
				// here it is a race, not a normal path.
				slog.Warn("pending interrupt inject present during non-interrupt PrepareStep drain",
					"session_id", call.SessionID)
			}
			for _, inj := range pending {
				injMsg, getErr := a.messages.Get(callContext, inj.MessageID)
				if getErr != nil {
					// The referenced message vanished (e.g. cascade delete):
					// skip it rather than aborting the whole step.
					slog.Warn("pending inject references missing message, skipping",
						"session_id", call.SessionID, "message_id", inj.MessageID, "error", getErr)
					continue
				}
				prepared.Messages = append(prepared.Messages, injMsg.ToAIMessage()...)
				// The row was written by a foreign process (`rush sessions
				// inject`), so its Create() never published through THIS
				// process's message broker. If a web UI happens to be
				// attached to this process for the session, Notify pushes
				// the already-persisted message so it renders live instead
				// of waiting for a page reload.
				a.messages.Notify(injMsg)
			}

			// Sliding-window context management: when the context is nearly
			// full, trim old messages so the agent can keep running without
			// blocking on a synchronous summarisation call.
			if !a.disableAutoSummarize {
				cw := int64(smartModel.CatwalkCfg.ContextWindow)
				if cw > 0 {
					usedTokens := currentSession.CompletionTokens + currentSession.PromptTokens
					remaining := cw - usedTokens
					var slideThreshold int64
					if cw > largeContextWindowThreshold {
						slideThreshold = largeContextWindowBuffer
					} else {
						slideThreshold = int64(float64(cw) * smallContextWindowRatio)
					}
					if remaining <= slideThreshold {
						targetTokens := int64(float64(cw) * contextSlideRatio)
						prepared.Messages = trimMessagesToWindow(prepared.Messages, targetTokens)

						// Record that a silent compact is needed — it runs
						// synchronously AFTER the turn completes (under the
						// turn's mailbox ownership), not as a concurrent
						// goroutine. A goroutine deleting messages while the
						// turn is still streaming was the P0-4 data corruption
						// bug (#268).
						silentCompactNeeded = true
					}
				}
			}

			prepared.Messages = a.workaroundProviderMediaLimitations(prepared.Messages, smartModel)

			lastSystemRoleInx := 0
			systemMessageUpdated := false
			for i, msg := range prepared.Messages {
				// Only add cache control to the last message.
				if msg.Role == fantasy.MessageRoleSystem {
					lastSystemRoleInx = i
				} else if !systemMessageUpdated {
					prepared.Messages[lastSystemRoleInx].ProviderOptions = a.getCacheControlOptions()
					systemMessageUpdated = true
				}
				// Than add cache control to the last 2 messages.
				if i > len(prepared.Messages)-3 {
					prepared.Messages[i].ProviderOptions = a.getCacheControlOptions()
				}
			}

			if promptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(promptPrefix)}, prepared.Messages...)
			}

			sessionLock.Lock()
			stepMessages = cloneFantasyMessages(prepared.Messages)
			stepTools = append([]fantasy.AgentTool(nil), prepared.Tools...)
			sessionLock.Unlock()

			var assistantMsg message.Message
			// Provenance is recorded from the model that ACTUALLY produced
			// the message, not from the configuration that selected it.
			//
			// Both values feed `GROUP BY model, provider` in the usage and
			// cost reports (internal/db/stats.sql.go, messages.sql.go), and
			// the two summarize paths in agent_compaction.go have always
			// recorded Model.Model()/Provider(). While this line recorded
			// ModelCfg instead, a provider whose canonical id differs from
			// the configured one would split a single session into two
			// groups in `rush sessions cost` — with neither number looking
			// wrong enough to notice.
			assistantMsg, err = a.messages.Create(callContext, call.SessionID, message.CreateMessageParams{
				Role:            message.Assistant,
				Parts:           []message.ContentPart{},
				Model:           smartModel.Model.Model(),
				Provider:        smartModel.Model.Provider(),
				ReasoningEffort: currentSession.SmartModelReasoningEffort,
			})
			if err != nil {
				return callContext, prepared, err
			}
			callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
			callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, smartModel.CatwalkCfg.SupportsImages)
			callContext = context.WithValue(callContext, tools.ModelNameContextKey, smartModel.CatwalkCfg.Name)
			sessionLock.Lock()
			currentAssistant = &assistantMsg
			sessionLock.Unlock()
			return callContext, prepared, err
		},
		OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningStart called", "id", id)
			sessionLock.Lock()
			currentAssistant.AppendReasoningContent(reasoning.Text)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnReasoningDelta: func(id string, text string) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningDelta called", "len", len(text))
			sessionLock.Lock()
			currentAssistant.AppendReasoningContent(text)
			sessionLock.Unlock()
			return notifyUI()
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			sessionLock.Lock()
			// handle anthropic signature
			if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
				if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
					currentAssistant.AppendReasoningSignature(reasoning.Signature)
				}
			}
			if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
				if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
					currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
				}
			}
			if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
				if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
					currentAssistant.SetReasoningResponsesData(reasoning)
				}
			}
			currentAssistant.FinishThinking()
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnTextDelta: func(id string, text string) error {
			bumpActivity()
			// Fork patch: batch 8 — start the checkpoint ticker on the
			// first text delta of this step (lazily, once only).
			startCheckpoint()
			sessionLock.Lock()
			// Fork patch: batch 8 — emit final-composition log at most
			// once per step, on the first text delta after a tool boundary.
			if sawToolBoundary && currentAssistant != nil {
				sawToolBoundary = false
				slog.Info(
					"agent: final composition started",
					"session_id", call.SessionID,
					"message_id", currentAssistant.ID,
					"chars_in_message_so_far", len(currentAssistant.FullText()),
				)
			}
			// Strip leading newline from initial text content. This is is
			// particularly important in non-interactive mode where leading
			// newlines are very visible.
			if len(currentAssistant.Parts) == 0 {
				text = strings.TrimPrefix(text, "\n")
			}

			currentAssistant.AppendContent(text)
			sessionLock.Unlock()
			return notifyUI()
		},
		OnToolInputStart: func(id string, toolName string) error {
			bumpActivity()
			sawToolBoundary = true // Fork patch: batch 8
			toolCall := message.ToolCall{
				ID:               id,
				Name:             toolName,
				ProviderExecuted: false,
				Finished:         false,
			}
			sessionLock.Lock()
			currentAssistant.AddToolCall(toolCall)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, snap)
		},
		OnToolInputDelta: func(id string, delta string) error {
			bumpActivity()
			sessionLock.Lock()
			currentAssistant.AppendToolCallInput(id, delta)
			sessionLock.Unlock()
			return nil // don't spam DB on every delta; ToolInputEnd will persist
		},
		OnToolInputEnd: func(id string) error {
			bumpActivity()
			sessionLock.Lock()
			currentAssistant.FinishToolCall(id)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnRetry: func(err *fantasy.ProviderError, delay time.Duration) {
			bumpActivity()
			slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
		},
		OnWarnings: func(warnings []fantasy.CallWarning) error {
			for _, w := range warnings {
				slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
			}
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			bumpActivity()
			// A tool is about to execute — pause the stall watchdog until its
			// result arrives (OnToolResult). fantasy fires every OnToolCall
			// for a step before executing any tool, so the counter brackets
			// the whole executeTools window. The same toolMaxDuration cap
			// bounds every tool, including a sub-agent delegation (the
			// `agent` tool) — see toolExecutionMaxDefault's doc in agent.go.
			toolStarted()
			sawToolBoundary = true // Fork patch: batch 8
			input, wasSanitized := sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
			if wasSanitized {
				sanitizedToolCalls[tc.ToolCallID] = true
			}
			toolCall := message.ToolCall{
				ID:               tc.ToolCallID,
				Name:             tc.ToolName,
				Input:            input,
				ProviderExecuted: false,
				Finished:         true,
			}
			sessionLock.Lock()
			currentAssistant.AddToolCall(toolCall)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, snap)
		},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			bumpActivity()
			// Tool finished — resume the stall watchdog (and restart its idle
			// window so the tool's runtime isn't counted against the provider).
			toolFinished()
			sawToolBoundary = true // Fork patch: batch 8
			toolResult := a.convertToToolResult(result)
			if sanitizedToolCalls[result.ToolCallID] {
				toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
				toolResult.IsError = true
			}
			sessionLock.Lock()
			sessionID := currentAssistant.SessionID
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the message is created
			// even if the request is canceled mid-stream
			_, createMsgErr := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			return createMsgErr
		},
		OnStepFinish: func(stepResult fantasy.StepResult) error {
			bumpActivity()
			// Accumulate this step and recompute loop detection NOW, in this
			// callback invocation, so the AddFinish chain below can use the
			// result for THIS step. Fantasy calls OnStepFinish BEFORE
			// StopWhen for the same step, so relying on the StopWhen closure
			// to set loopDetected would read a stale (still-false) flag for
			// the very step that trips the detector — the loop would break
			// with empty finish text and no later OnStepFinish to fix it.
			// See the comment on stepHistory above for the ordering rationale.
			stepHistory = append(stepHistory, stepResult)
			loopDetected, loopDetail = hasRepeatedToolCalls(stepHistory, loopDetectionWindowSize, loopDetectionMaxRepeats)
			// Surface provider CallWarnings (malformed tool-call sanitization,
			// unsupported settings, etc.) that fantasy otherwise discards
			// silently. Visible in logs only — does not interrupt the turn.
			logProviderWarnings(stepResult.Warnings)
			// Fork patch: batch 8 — stop the checkpoint ticker BEFORE the
			// final write so the ticker doesn't race with OnStepFinish.
			stopCheckpoint()
			sawToolBoundary = true // Fork patch: batch 8 — reset for next step
			finishReason := message.FinishReasonUnknown
			switch stepResult.FinishReason {
			case fantasy.FinishReasonLength:
				finishReason = message.FinishReasonMaxTokens
			case fantasy.FinishReasonStop:
				finishReason = message.FinishReasonEndTurn
			case fantasy.FinishReasonToolCalls:
				finishReason = message.FinishReasonToolUse
			}
			// If a tool result halted the turn (e.g. a hook halt or a
			// permission denial), the step ends on FinishReasonToolCalls but
			// the model will not be called again. Treat it as the end of the
			// turn so the UI can render the assistant footer.
			if finishReason == message.FinishReasonToolUse {
				for _, tr := range stepResult.Content.ToolResults() {
					if tr.StopTurn {
						finishReason = message.FinishReasonEndTurn
						break
					}
				}
			}
			// Fork patch: surface empty-stream as a visible error.
			// Some providers (e.g. z.ai) sometimes close the stream without
			// sending any content (no text, no tool_call, no reasoning) and
			// without an explicit finish reason. The upstream code records this
			// as FinishReasonUnknown with empty parts, which the WUI renders as
			// a blank assistant block — looking like a session lockup. Convert
			// this case to an error so both the WUI fallback and the user see
			// an actionable message. See CHANGELOG.fork.md section 4.D.
			//
			// currentAssistant reads/mutations below are under sessionLock:
			// OnStepFinish never runs concurrently with the other streaming
			// callbacks (fantasy invokes them sequentially from one loop),
			// but it DOES run concurrently with the checkpoint ticker and
			// the peak-hours watcher goroutines, which also touch
			// currentAssistant.
			sessionLock.Lock()
			if finishReason == message.FinishReasonUnknown &&
				currentAssistant.FullText() == "" &&
				currentAssistant.ReasoningContent().Thinking == "" &&
				len(currentAssistant.ToolCalls()) == 0 {
				slog.Warn(
					"agent: empty stream from provider — recording as error",
					"sessionID", call.SessionID,
					"provider", smartModel.ModelCfg.Provider,
					"model", smartModel.ModelCfg.Model,
				)
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Empty response",
					fmt.Sprintf(
						"Provider %q closed the stream for model %q without returning any content. This is usually a transient provider/network issue — please retry.",
						smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model,
					),
				)
			} else if loopDetected {
				// Loop detection force-stopped the turn. The reason stays
				// FinishReasonEndTurn (NOT a new distinct enum value) so
				// reclassifyCrashedAsDone / sessions-why keep treating this as
				// "done" — but the message/details are non-empty so an operator
				// or orchestrator can distinguish "model finished voluntarily"
				// from "we truncated a likely loop (possibly a legitimate poll)".
				loopMsg, loopDetails := loopDetectedFinishText(loopDetail)
				currentAssistant.AddFinish(finishReason, loopMsg, loopDetails)
			} else {
				currentAssistant.AddFinish(finishReason, "", "")
			}
			sessionLock.Unlock()
			// Drain any pending UI snapshot so the ticker goroutine does not
			// publish a stale state after messages.Update writes the final one.
			select {
			case <-latestMsgCh:
			default:
			}

			updatedSession, getSessionErr := a.sessions.Get(ctx, call.SessionID)
			if getSessionErr != nil {
				return getSessionErr
			}
			// Fork merge note (origin/main 6ed8852b "fix(agent): estimate
			// missing streamed usage"): if the provider omits the final
			// usage chunk, use upstream's token estimator so our sliding
			// context window stays accurate. We drop the "estimated" flag
			// (TUI marker — see CHANGELOG.fork.md Section 2).
			usage, estimated := fallbackStepUsage(stepMessages, stepResult)
			// Normalize once, upstream of both updateSessionUsage and
			// recordMessageUsage, so InputTokens is exclusive-of-cache for
			// every provider before either consumer sees it.
			usage = normalizeProviderUsage(smartModel.Model.Provider(), usage)
			costDelta := a.updateSessionUsage(smartModel, &updatedSession, usage, a.openrouterCost(stepResult.ProviderMetadata))
			if costDelta != 0 {
				if _, costErr := a.sessions.IncrementCost(ctx, updatedSession.ID, costDelta); costErr != nil {
					return costErr
				}
			}
			if sessionErr := a.sessions.SetUsage(ctx, updatedSession.ID, updatedSession.PromptTokens, updatedSession.CompletionTokens); sessionErr != nil {
				return sessionErr
			}
			// Per-message breakdown (task #469). The session-level figures
			// above are a last-snapshot overwrite plus a running cost, which
			// cannot answer "how well is the cache working" for a message, a
			// model, or a day. currentAssistant.ID is read under sessionLock
			// because the checkpoint ticker and peak-hours watcher also touch
			// currentAssistant (same reason the AddFinish block above locks).
			sessionLock.Lock()
			assistantID := currentAssistant.ID
			sessionLock.Unlock()
			a.recordMessageUsage(ctx, assistantID, smartModel, usage, costDelta, estimated)
			if usage.CacheCreationTokens > 0 {
				a.scheduleCacheKeepAlive(call.SessionID, smartModel, stepMessages, stepTools, call.ProviderOptions, call.MaxCost)
			}
			currentSession = updatedSession

			// Fork patch: batch 30 — cancel + runaway protection.
			// Check DB cancel flag (cross-process signal) and cost/token caps.
			canc, cancErr := a.sessions.IsCancelRequested(ctx, call.SessionID)
			if cancErr != nil {
				// A failed read is NOT treated as a cancellation: a transient
				// DB error is no evidence the operator asked for one, and
				// aborting on it would turn every hiccup into what looks like
				// a user abort. But it must not be silent either — if the
				// operator did request a cancel and this is the read that
				// failed, the turn runs on with nothing in the log to say why
				// the request appeared to be ignored.
				slog.Warn("could not read the cancel-requested flag; continuing the turn",
					"session_id", call.SessionID, "err", cancErr)
			}
			if cancErr == nil && canc {
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s cancelled by user", call.SessionID)
			}
			// BUG-4 (full-project reviewer audit, 2026-08-11): these abort
			// paths (max-cost, max-tokens, and peak-hours below) stop the
			// turn ONLY via the cancelFunc looked up from activeRequests —
			// returning an error from OnStepFinish alone does NOT break
			// fantasy's loop (see the peak-hours note ~30 lines below). This
			// is safe today ONLY because runTurn stores the turn's genCtx
			// cancel via activeRequests.Set before the agent.Stream call
			// whose OnStepFinish looks it up, and nothing ever calls
			// activeRequests.Del for this key (entries live forever — see
			// IsBusy's doc). Any future change that reclaims an
			// activeRequests entry before the turn ends silently turns these
			// aborts into no-ops: the error is returned but the turn keeps
			// running. Pinned by TestActiveRequests_HoldsLiveCancelDuringTurn.
			if call.MaxCost > 0 && updatedSession.Cost > call.MaxCost {
				slog.Warn(
					"agent: aborting — max-cost exceeded",
					"session_id", call.SessionID,
					"cost", updatedSession.Cost,
					"max", call.MaxCost,
				)
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s aborted: cost $%.4f exceeds max $%.4f",
					call.SessionID, updatedSession.Cost, call.MaxCost)
			}
			totalTokens := updatedSession.PromptTokens + updatedSession.CompletionTokens
			if call.MaxTokens > 0 && totalTokens > call.MaxTokens {
				slog.Warn(
					"agent: aborting — max-tokens exceeded",
					"session_id", call.SessionID,
					"tokens", totalTokens,
					"max", call.MaxTokens,
				)
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s aborted: %d tokens exceeds max %d",
					call.SessionID, totalTokens, call.MaxTokens)
			}

			// Fork patch: peak-hours is normally only checked once, at the
			// START of a turn (coordinator.buildCall/runInternal) — an
			// already-in-flight turn was never re-checked, so a long turn
			// that started before the window opened ran straight through
			// it. Re-check here, once per step, so a turn stops as soon as
			// the provider enters its peak-hours window, not just on the
			// next NEW invocation.
			if a.peakHoursCheck != nil {
				if pErr := a.peakHoursCheck(); pErr != nil {
					if setPeakHoursAbortErr(pErr) {
						slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
							"session_id", call.SessionID, "error", pErr)
						peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
						sessionLock.Lock()
						currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
						snap := currentAssistant.Clone()
						sessionLock.Unlock()
						// Use the parent ctx (not genCtx) for the DB write —
						// genCtx dies as soon as we cancel below.
						if uErr := a.messages.Update(ctx, snap); uErr != nil {
							slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
						}
						if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
							cancelFn()
						}
					}
					// Stash the specific error so Run() can return it
					// AFTER fantasy's agent.Stream exits. We must call
					// cancelFn() to break fantasy's loop (returning an
					// error from OnStepFinish alone doesn't stop it), but
					// cancel() makes fantasy return context.Canceled —
					// swallowing our pErr. The stash lets Run() replace
					// that generic error with the real one.
					return pErr
				}
			}

			sessionLock.Lock()
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		StopWhen: []fantasy.StopCondition{
			func(_ []fantasy.StepResult) bool {
				cw := int64(smartModel.CatwalkCfg.ContextWindow)
				// If context window is unknown (0), skip auto-summarize
				// to avoid immediately truncating custom/local models.
				if cw == 0 {
					return false
				}
				tokens := currentSession.CompletionTokens + currentSession.PromptTokens
				remaining := cw - tokens
				var threshold int64
				if cw > largeContextWindowThreshold {
					threshold = largeContextWindowBuffer
				} else {
					threshold = int64(float64(cw) * smallContextWindowRatio)
				}
				if (remaining <= threshold) && !a.disableAutoSummarize {
					shouldSummarize = true
					return true
				}
				return false
			},
			func(steps []fantasy.StepResult) bool {
				// StopWhen runs AFTER OnStepFinish for the same step, so by the
				// time this executes, OnStepFinish has already appended to
				// stepHistory and recomputed loopDetected/loopDetail. We only
				// need to return the boolean here to tell fantasy to break the
				// loop — do NOT mutate loopDetected/loopDetail here, OnStepFinish
				// owns them (mutating here would race for the last step's
				// finish text and re-introduce the stale-flag bug).
				detected, _ := hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
				return detected
			},
		},
	})
	// Defensive: normally OnStepFinish stops the checkpoint ticker (via
	// stopCheckpoint()) before its own final write. But if agent.Stream
	// returned an error before any step completed (e.g. the very first
	// provider call failed), OnStepFinish never ran and the ticker
	// goroutine may still be alive — it would otherwise race with the
	// unlocked currentAssistant touches below. stopCheckpoint() is safe to
	// call more than once: after the first call the stop channel is nil'd,
	// so subsequent calls hit the nil guard and return immediately (no
	// second wait, no double-close).
	stopCheckpoint()
	err = normalizeTurnError(err, getPeakHoursAbortErr)
	if err != nil {
		isHyper := smartModel.ModelCfg.Provider == hyper.Name
		isCancelErr := errors.Is(err, context.Canceled)
		isWatchdogStall := isCancelErr && wd.stalled.Load()
		// `rush run --timeout` bounds the whole invocation via
		// context.WithTimeout on the root ctx (run.go); when it fires
		// mid-turn, ctx.Err() is context.DeadlineExceeded, NOT
		// context.Canceled, so isCancelErr above never catches it. Without
		// this branch it fell into the generic `else` below as "Provider
		// Error" with a bare "context deadline exceeded" — indistinguishable
		// from a real provider failure and useless to `sessions why`.
		isRunTimeout := errors.Is(err, context.DeadlineExceeded)
		// If userMessageCreated is true (either we just created it or
		// call.ExistingMessageID was set), the call has already left a
		// persistent trace. Wrap the error to prevent duplicate execution
		// on retry (task #339). This handles ALL errors after user message
		// creation, not just those after currentAssistant is set.
		//
		// The wrapping must happen BEFORE we check nilAssistant because
		// errors in PrepareStep (before currentAssistant is set) also need
		// to be wrapped. See call_attempted_error.go for the design rationale.
		if userMessageCreated {
			err = &ErrCallAlreadyAttempted{Err: err}
		}
		// currentAssistant is only ever reassigned (never set back to nil)
		// by PrepareStep, under sessionLock. agent.Stream has already
		// returned by this point so no streaming callback can race this
		// read, but the peak-hours watcher goroutine may still be alive
		// (it only stops when genCtx is cancelled by the deferred cancel()
		// at the end of Run) and touches currentAssistant under the same
		// lock, so guard the read too.
		sessionLock.Lock()
		nilAssistant := currentAssistant == nil
		sessionLock.Unlock()
		if nilAssistant {
			return result, SessionAgentCall{}, false, err
		}
		// All DB writes in the error path use a detached context. The outer
		// ctx may itself be cancelled — in `rush run` it's the
		// signal.NotifyContext from fang, so Ctrl-C cancels it too; in the
		// web UI a request abort cancels it; the stream watchdog above
		// cancels genCtx (whose parent is ctx, so it doesn't cancel ctx,
		// but defensively we still detach). Without a detached ctx the
		// finish part Update fails with context.Canceled and the assistant
		// ends up half-saved in the DB — the "silent dying" pattern
		// observed in 162-promise-all. Codec must surface control: the
		// finish part MUST land on disk before we return.
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer flushCancel()
		// Ensure we finish thinking on error to close the reasoning state.
		// From here to the final flush below, currentAssistant's Parts are
		// mutated in place; every touch (including the plain reads used to
		// build msgs/toolCalls) takes sessionLock to stay consistent with
		// the peak-hours watcher goroutine that may still be running.
		sessionLock.Lock()
		currentAssistant.FinishThinking()
		toolCalls := currentAssistant.ToolCalls()
		sessionID := currentAssistant.SessionID
		sessionLock.Unlock()
		msgs, createErr := a.messages.List(flushCtx, sessionID)
		if createErr != nil {
			return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
		}
		for _, tc := range toolCalls {
			if !tc.Finished {
				tc.Finished = true
				tc.Input = "{}"
				sessionLock.Lock()
				currentAssistant.AddToolCall(tc)
				snap := currentAssistant.Clone()
				sessionLock.Unlock()
				updateErr := a.messages.Update(flushCtx, snap)
				if updateErr != nil {
					return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
				}
			}

			found := false
			for _, msg := range msgs {
				if msg.Role == message.Tool {
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == tc.ID {
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}
			if found {
				continue
			}
			content := "There was an error while executing the tool"
			if isWatchdogStall {
				content = watchdogToolResultMessage(
					watchdogCause(watchdogCauseVal.Load()),
					toolMaxDuration,
					a.timeoutHardCap,
					idleTimeout,
					smartModel.ModelCfg.Provider,
				)
			} else if isCancelErr {
				content = "Error: user cancelled assistant tool calling"
			}
			toolResult := message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    true,
			}
			_, createErr = a.messages.Create(flushCtx, sessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			if createErr != nil {
				return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
			}
		}
		var fantasyErr *fantasy.Error
		var providerErr *fantasy.ProviderError
		var peakErr *PeakHoursError
		var awaitingErr *AwaitingAnswerError
		const defaultTitle = "Provider Error"
		// None of the branches below perform I/O — they only decide which
		// AddFinish to record based on err/isWatchdogStall/etc. — so the
		// whole chain can run under a single lock/unlock pair guarding the
		// currentAssistant mutation, matching the pattern used everywhere
		// else in Run().
		sessionLock.Lock()
		if isWatchdogStall {
			// Close the observability loop: the watchdog goroutine already
			// emitted its slog.Warn at fire-time, but a log reader
			// chasing the trail needs to see that the stall actually
			// made it into the user-visible finish part on this session.
			slog.Info(
				"agent: watchdog stall surfaced as FinishReasonError",
				"session_id", call.SessionID,
				"provider", smartModel.ModelCfg.Provider,
			)
			title, body := watchdogFinishMessage(
				watchdogCause(watchdogCauseVal.Load()),
				toolMaxDuration,
				a.timeoutHardCap,
				idleTimeout,
				smartModel.ModelCfg.Provider,
			)
			currentAssistant.AddFinish(message.FinishReasonError, title, body)
		} else if isCancelErr {
			currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
		} else if isRunTimeout {
			currentAssistant.AddFinish(
				message.FinishReasonError,
				"Run timeout exceeded",
				"The run's --timeout deadline expired while this turn was still in flight (e.g. a long tool call or sub-agent delegation). Re-run into the same --session id with a larger --timeout to continue from here.",
			)
		} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized {
			currentAssistant.AddFinish(message.FinishReasonError, "Unauthorized", `Please re-authenticate with Hyper. You can also run "rush auth" to re-authenticate.`)
		} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusPaymentRequired {
			url := hyper.BaseURL()
			currentAssistant.AddFinish(message.FinishReasonError, "No credits", "You're out of credits. Add more at "+url)
		} else if errors.As(err, &providerErr) {
			if providerErr.Message == "The requested model is not supported." {
				url := "https://github.com/settings/copilot/features"
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Copilot model not enabled",
					fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", smartModel.CatwalkCfg.Name, url),
				)
			} else {
				currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), providerErr.Message)
			}
		} else if errors.As(err, &fantasyErr) {
			currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
		} else if errors.As(err, &peakErr) {
			// Re-derive the same (msg, details) OnStepFinish's peak-hours
			// check already wrote (with the RESUME AT guidance) — this
			// path (err forced to peakHoursAbortErr) ALSO reaches here,
			// and AddFinish always replaces the prior finish part, so
			// without this branch the generic `else` below would
			// overwrite the useful message with a bare "Provider Error:
			// <terse text>" that drops the resume-time guidance entirely.
			peakMsg, peakDetails := peakHoursStoppedFinishText(err)
			currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
		} else if errors.As(err, &awaitingErr) {
			// Same rationale as the peakErr branch above: without this,
			// the generic `else` below would overwrite the question/options/
			// resume-command guidance with a bare "Provider Error: <text>".
			awaitingMsg, awaitingDetails := awaitingAnswerStoppedFinishText(err)
			currentAssistant.AddFinish(message.FinishReasonError, awaitingMsg, awaitingDetails)
		} else {
			currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, err.Error())
		}
		snap := currentAssistant.Clone()
		sessionLock.Unlock()
		// Detached flush (flushCtx is context.WithoutCancel + 15s timeout,
		// created at the top of this error block). This is the call that
		// MUST land on disk — without it the assistant message has tool
		// calls but no finish part, and the WUI/recovery sees it as still
		// in-flight forever.
		updateErr := a.messages.Update(flushCtx, snap)
		if updateErr != nil {
			slog.Error(
				"agent: failed to persist final finish part",
				"session_id", call.SessionID,
				"err", updateErr,
			)
			return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
		}

		// Drain on cancel via the mailbox's generation-aware drain (design
		// §4): an interrupt-and-replace payload (mb.replacement) takes
		// precedence over a plain queued follow-up (mb.submitted). The
		// busy reservation itself stays claimed (Run's loop is about to
		// run another turn for the same sessionID) and is only released
		// by Run() once the loop has no more queued work.
		if isCancelErr {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				cancel()
				return nil, next, true, nil
			}
		}
		// err was already wrapped in ErrCallAlreadyAttempted above (if
		// userMessageCreated), so return it directly rather than wrapping
		// it a second time.
		return nil, SessionAgentCall{}, false, err
	}

	if shouldSummarize {
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
		sessionLock.Lock()
		hasPendingToolCalls := len(currentAssistant.ToolCalls()) > 0
		sessionLock.Unlock()
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
	if !shouldSummarize && silentCompactNeeded {
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
			SessionTitle: currentSession.Title,
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
