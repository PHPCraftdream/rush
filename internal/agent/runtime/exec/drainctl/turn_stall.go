package drainctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/kit/diag/log"
)

// This file is the Phase 1 infrastructure of the turn-stall detector: an
// in-process progress clock, a phase tracker, and ONE process-wide sampler
// goroutine. It is deliberately diagnostics-only — NO abort/kill logic
// lives here. The abort policy is added later via setTurnStallPolicy (see
// its doc); with no policy installed the sampler only warns and captures
// a goroutine dump.

// StreamIdleTimeoutDefault is the default tolerance for "no streaming
// event for this long" before the watchdog cancels the LLM request.
// Configurable per-app via Options.StreamIdleTimeoutSeconds, plumbed
// through SessionAgentOptions.StreamIdleTimeout. Read at Run()-time via
// the sessionAgent's effective idle timeout.
//
// Raised to 10 min on 2026-06-17. Extended-thinking models (GLM-5.2
// on max effort, Opus 4.7+ with large thinking budgets, Sonnet 4.5
// with thinking_budget ~32k) routinely go silent at the wire while
// reasoning server-side — no reasoning_content deltas are streamed
// until the final answer arrives. The previous 3-minute default
// killed those runs prematurely. 10 minutes covers every observed
// case so far without letting truly hung streams sit forever.
// Operators who want the old behaviour can set the value back via
// Options.StreamIdleTimeoutSeconds.
const StreamIdleTimeoutDefault = 10 * time.Minute

// turnPhase is an immutable snapshot of what the turn was last seen doing.
// kind is one of "provider", "tool" or "internal"; name carries the model
// name, the tool name, or the internal stage label ("preamble",
// "compaction", "persist"); callID is the tool-call id for kind=="tool".
// Names and ids ONLY — never prompt text or tool input.
type turnPhase struct {
	kind   string
	name   string
	callID string
	since  time.Time
}

// StallClock is one turn's stall-detection state, shared by turnStream,
// the runTurn body and the sampler via the armed registry below. Every
// field is an atomic or an atomic.Pointer, so the sampler can read a
// consistent-enough snapshot without locks.
type StallClock struct {
	// lastProgress is UnixNano of the last DURABLE progress point
	// (tool call, tool result, step finish, checkpoint persist, text
	// delta). Reasoning deltas deliberately do NOT move it — extended
	// thinking can stream for a long time without producing output.
	lastProgress atomic.Int64
	// lastByte mirrors the stream watchdog's liveness clock (bumped on
	// EVERY stream callback, keepalives and retries included); it is
	// diagnostics-only, to distinguish "provider silent at the wire"
	// from "provider chattering but not progressing".
	lastByte atomic.Int64
	// reasoningCharsSinceProgress counts reasoning-delta bytes since the
	// last durable progress point. Diagnostics only.
	reasoningChars atomic.Int64
	// toolsInFlight counts tool executions bracketed by the turn's
	// OnToolCall/OnToolResult hooks (the watchdog's own counter is
	// internal to stream_watchdog.go and not exposed).
	toolsInFlight atomic.Int64
	phase         atomic.Pointer[turnPhase]
	// lastWarnNanos rate-limits warn+dump to one per warnWindow per
	// armed clock.
	lastWarnNanos atomic.Int64
	// abortTimeout is this call's TurnStallTimeout (0 = abort
	// disabled). Written once at arm time, before the entry becomes
	// visible to the sampler, so no atomic is needed. The WARN/DUMP
	// threshold is always StreamIdleTimeoutDefault; only the (later)
	// abort policy reads this.
	abortTimeout time.Duration
	// attemptCancel is the in-flight provider attempt's cancel, armed by
	// runTurn and cancelled by the stall abort policy on a provider fire.
	// Stored via atomic.Value: the sampler goroutine reads it.
	attemptCancel atomic.Value // context.CancelFunc
	// abortFn is the turn's abort closure (causeStall fire path + genCtx
	// cancel), armed by runTurn. Same atomic.Value pattern.
	abortFn atomic.Value // func(elapsed time.Duration)
	// stallRetries counts provider stall retries issued by the policy for
	// THIS turn; runStallRetried reads it to attribute a context.Canceled.
	stallRetries atomic.Int32
	// lastStallRetryNanos is when the policy last re-issued a stalled
	// provider request (0 = never).
	lastStallRetryNanos atomic.Int64
	// toolStalled is the tool-phase hook (the root's stallToolStalled),
	// the ONLY cross-root seam: the process-wide policy stays the original
	// dynamic global (turnStallPolicy, read by the sampler at every
	// crossing), but the tool-detach path is root-owned, so the hook is
	// injected once at arm time and retained immutably on the clock. nil
	// only for clocks built directly by lower-package tests.
	toolStalled atomic.Value // func(sessionID, callID string, since time.Duration) string
}

// stallWarnWindow rate-limits warn+dump per armed clock: at most one warn
// (and dump capture) every 30 minutes while a stall persists.
const stallWarnWindow = 30 * time.Minute

// stallSamplerTick is how often the process-wide sampler re-checks every
// armed clock.
const stallSamplerTick = 15 * time.Second

// stallMaxRetries is how many times a stalled provider request is cancelled
// and re-issued before the turn is aborted with causeStall.
const stallMaxRetries = 2

// armedStallClocks holds the per-session stall clocks, keyed by session
// id. An entry exists only while a turn for that session is running
// (armed at the top of runTurn, disarmed in runTurn's defer).
var armedStallClocks sync.Map // string -> *StallClock

// stallNowFn is the injectable clock behind stallNow. A package seam so
// tests can drive a fake clock deterministically; atomic because the
// process-wide sampler goroutine (started by the first armed turn of any
// test) reads it while a later test swaps it.
var stallNowFn atomic.Pointer[func() time.Time]

// stallNow is the clock the sampler and phase stamps read.
func stallNow() time.Time {
	if fn := stallNowFn.Load(); fn != nil {
		return (*fn)()
	}
	return time.Now()
}

// setTurnStallPolicy installs the stall-policy seam invoked by the
// sampler on every threshold crossing. The default (nil) policy means
// warn+dump only; turnStallPolicyInstall installs the abort policy at
// the first arm. The function receives identifiers and durations only —
// never prompt text or tool input. Safe to call at any time; reads are
// nil-safe. Package-private: the only production caller is
// installTurnStallPolicy; no verified external caller needs it.
func setTurnStallPolicy(fn func(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration)) {
	turnStallPolicy.Store(fn)
}

// turnStallPolicy holds the injectable policy hook. Stored via
// atomic.Value so the sampler never takes a lock on the hot path, and
// re-READ at every crossing so a test (or a later worker) can swap the
// policy dynamically without re-arming the clocks.
var turnStallPolicy atomic.Value // func(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration)

// stallSamplerOnce guards the process-wide sampler goroutine: started
// lazily exactly once by the first armTurnStall, never per-turn
// (pattern-matches internal/heartbeat's ensureWorker).
var stallSamplerOnce sync.Once

// armTurnStall creates (or reuses) call.SessionID's stall clock, arms it
// in the registry, stamps the initial phase, and lazily starts the
// sampler — and (new, the one added seam) retains the root-owned
// tool-stalled hook on the clock. Returns the clock for the turn's hook
// lines.
func ArmTurnStall(sessionID string, call *agentcall.CallOptions, toolStalled func(sessionID, callID string, since time.Duration) string) *StallClock {
	installTurnStallPolicy()
	clock := &StallClock{}
	if call != nil {
		clock.abortTimeout = call.TurnStallTimeout
	}
	if toolStalled != nil {
		clock.toolStalled.Store(toolStalled)
	}
	now := stallNow()
	clock.lastProgress.Store(now.UnixNano())
	clock.lastByte.Store(now.UnixNano())
	clock.SetPhase("internal", "preamble", "")
	armedStallClocks.Store(sessionID, clock)
	stallSamplerOnce.Do(func() { go stallSampler() })
	return clock
}

// installFakeStallNow swaps stallNow for a controllable fake and restores
// it on cleanup, returning a setter. Exact mixed-root port of the root
// test helper of the same name: the fake reads an atomic.Int64nanos value
// and the setter does an explicit Store(at.UnixNano()) — no auto-increment.
func InstallFakeStallNow(tb testing.TB) func(at time.Time) {
	tb.Helper()
	var current atomic.Int64
	current.Store(time.Now().UnixNano())
	prev := stallNowFn.Load()
	fake := func() time.Time { return time.Unix(0, current.Load()) }
	stallNowFn.Store(&fake)
	tb.Cleanup(func() { stallNowFn.Store(prev) })
	return func(at time.Time) { current.Store(at.UnixNano()) }
}

// disarmTurnStall removes sessionID's clock. nil-safe semantics: a stale
// disarm (turn ended after another turn re-armed) only deletes the entry
// present, which the newer turn immediately re-creates on its own arm.
func DisarmTurnStall(sessionID string) {
	armedStallClocks.Delete(sessionID)
}

// StallClockForTest returns sessionID's armed clock, if any. Exported
// so original-root tests can load their specific clock before advancing
// the fake clock; no production use.
func StallClockForTest(sessionID string) (*StallClock, bool) {
	clock, ok := armedStallClocks.Load(sessionID)
	if !ok {
		return nil, false
	}
	sc, _ := clock.(*StallClock)
	return sc, sc != nil
}

// StallSetPhase stamps an internal phase onto sessionID's armed clock.
// nil-safe: called from paths (e.g. manual compaction) that may run
// without an armed clock. One-line hook target.
func StallSetPhase(sessionID, kind, name string) {
	if clock, ok := armedStallClocks.Load(sessionID); ok {
		clock.(*StallClock).SetPhase(kind, name, "")
	}
}

// SetPhase replaces the phase snapshot atomically.
func (sc *StallClock) SetPhase(kind, name, callID string) {
	if sc == nil {
		return
	}
	sc.phase.Store(&turnPhase{kind: kind, name: name, callID: callID, since: stallNow()})
}

// MarkProgress records a durable progress point (text delta, tool call,
// tool result, step finish, checkpoint persist). One-line hook target.
func (sc *StallClock) MarkProgress() {
	if sc == nil {
		return
	}
	sc.lastProgress.Store(stallNow().UnixNano())
}

// AddReasoning records reasoning-delta bytes for diagnostics only; it never
// moves lastProgress. One-line hook target.
func (sc *StallClock) AddReasoning(n int64) {
	if sc == nil {
		return
	}
	sc.reasoningChars.Add(n)
}

// MarkByte mirrors the stream watchdog's every-callback liveness clock.
func (sc *StallClock) MarkByte() {
	if sc == nil {
		return
	}
	sc.lastByte.Store(stallNow().UnixNano())
}

// ToolEnter brackets a tool execution: counts it in flight and stamps the
// tool phase. One-line hook target (OnToolCall).
func (sc *StallClock) ToolEnter(name, callID string) {
	if sc == nil {
		return
	}
	sc.toolsInFlight.Add(1)
	sc.SetPhase("tool", name, callID)
}

// ToolExit closes a tool execution; when the last one finishes the phase
// returns to the provider. One-line hook target (OnToolResult).
func (sc *StallClock) ToolExit() {
	if sc == nil {
		return
	}
	remaining := sc.toolsInFlight.Add(-1)
	if remaining <= 0 {
		sc.toolsInFlight.Store(0)
		sc.SetPhase("provider", "", "")
	}
}

// ArmAttemptCancel stores (or clears, with nil) the live attempt's cancel.
func (sc *StallClock) ArmAttemptCancel(cancel context.CancelFunc) {
	sc.attemptCancel.Store(cancel)
}

// ArmStallAbort stores the turn's abort closure.
func (sc *StallClock) ArmStallAbort(abort func(elapsed time.Duration)) {
	sc.abortFn.Store(abort)
}

// stallSampler is the process-wide sampler goroutine: one 15 s tick loop
// for every armed clock in the process, started exactly once.
func stallSampler() {
	ticker := time.NewTicker(stallSamplerTick)
	defer ticker.Stop()
	for range ticker.C {
		stallSamplerTickOnce()
	}
}

// stallSamplerTickOnce evaluates every armed clock once. Split out of
// stallSampler so tests can drive ticks deterministically.
func stallSamplerTickOnce() {
	now := stallNow()
	armedStallClocks.Range(func(key, value any) bool {
		sessionID, _ := key.(string)
		clock, _ := value.(*StallClock)
		if clock == nil {
			return true
		}
		stallSamplerCheck(sessionID, clock, now)
		return true
	})
}

// abortAfter returns the clock's configured abort timeout.
func (sc *StallClock) abortAfter() time.Duration {
	return sc.abortTimeout
}

// StallSamplerCheck evaluates one armed clock at the current stallNow —
// the verbatim stallSamplerCheck for mixed-root tests that hold a clock.
func StallSamplerCheck(sessionID string, clock *StallClock) {
	stallSamplerCheck(sessionID, clock, stallNow())
}

// stallSamplerCheck evaluates one armed clock. The policy seam is invoked
// on EVERY threshold crossing (so provider retries and tool detaches are
// not gated by the warn window); only the warn log + goroutine dump are
// rate-limited to one per stallWarnWindow. The clock's abortTimeout is
// diagnostics-only here; the abort policy reads it via abortAfter.
func stallSamplerCheck(sessionID string, clock *StallClock, now time.Time) {
	threshold := StreamIdleTimeoutDefault
	sinceProgress := now.Sub(time.Unix(0, clock.lastProgress.Load()))
	sinceByte := now.Sub(time.Unix(0, clock.lastByte.Load()))
	if sinceProgress < threshold {
		return
	}
	phase := clock.phase.Load()

	// The policy must see every crossing: provider retries (chunk 2) and
	// tool detaches (chunk 3) would otherwise be 30 minutes apart.
	if policy, ok := turnStallPolicy.Load().(func(string, turnPhase, time.Duration, time.Duration)); ok && policy != nil {
		var p turnPhase
		if phase != nil {
			p = *phase
		}
		policy(sessionID, p, sinceProgress, sinceByte)
	}

	if now.UnixNano()-clock.lastWarnNanos.Load() < int64(stallWarnWindow) {
		return
	}
	clock.lastWarnNanos.Store(now.UnixNano())

	phaseKind, phaseName := "", ""
	if phase != nil {
		phaseKind, phaseName = phase.kind, phase.name
	}
	// Reason carries identifiers and ages ONLY — never prompt text or
	// tool input.
	reason := fmt.Sprintf("turn-stall session=%s phase=%s/%s since_progress=%s since_byte=%s",
		sessionID, phaseKind, phaseName, sinceProgress.Truncate(time.Second), sinceByte.Truncate(time.Second))
	dump := log.CaptureGoroutineStack(reason)
	prefix := stallDumpPrefix(sessionID)
	slog.Warn("turn-stall detector: no durable progress",
		"session_id", sessionID,
		"phase", phaseKind+"/"+phaseName,
		"since_progress", sinceProgress.Truncate(time.Second),
		"since_byte", sinceByte.Truncate(time.Second),
		"reasoning_chars_since_progress", clock.reasoningChars.Load(),
		"tools_in_flight", clock.toolsInFlight.Load(),
		"dump_prefix", prefix,
	)
	// onFire must never block on I/O: dispatch the dump write
	// asynchronously, in its own goroutine.
	stallDumpWrites.Add(1)
	go func() {
		defer stallDumpWrites.Done()
		path, err := log.WriteGoroutineDumpNamed(dump, prefix)
		if err != nil {
			slog.Warn("turn-stall detector: failed to write goroutine dump", "err", err)
			return
		}
		slog.Warn("turn-stall detector: wrote goroutine dump", "path", path)
	}()
}

// StallWaitDumps waits for in-flight async dump writes so a test can wait
// for them before its log dir is removed. Mixed-root test seam; no
// production use.
func StallWaitDumps() {
	stallDumpWrites.Wait()
}

// stallDumpWrites counts in-flight async dump writes so a test can wait for
// them before its log dir is removed.
var stallDumpWrites sync.WaitGroup

// stallDumpPrefix builds the dump filename prefix for a session:
// "stall-<first8-of-session-id>-".
func stallDumpPrefix(sessionID string) string {
	id := sessionID
	if len(id) > 8 {
		id = id[:8]
	}
	return "stall-" + id + "-"
}

// This file is the abort layer of the turn-stall policy (chunk 2). The
// sampler above only warns and dumps; the policy installed here acts on a
// fire: provider phases get the stuck request cancelled and the step
// re-issued (at most stallMaxRetries times), internal phases abort the
// turn immediately. abortTimeout == 0 never aborts — warn+dump still
// happen. Principle: cancel the stuck request, retry twice, then die with
// a clear reason.

// turnStallPolicyInstall registers turnStallAbortPolicy exactly once, on
// the first armTurnStall — the least invasive startup hook (no coordinator
// or app wiring needed).
var turnStallPolicyInstall sync.Once

// installTurnStallPolicy lazily arms the abort policy; called from
// armTurnStall.
func installTurnStallPolicy() {
	turnStallPolicyInstall.Do(func() { setTurnStallPolicy(turnStallAbortPolicy) })
}

// turnStallAbortPolicy reacts to one sampler fire. Identifiers and
// durations only — never prompt text or tool input. Runs on the sampler
// goroutine: every action it takes is a cancel or a non-blocking closure.
// The tool-phase dispatch goes through the clock's retained toolStalled
// hook (the injected root seam); nil only for clocks built directly by
// lower-package tests, which then simply skip the detach.
func turnStallAbortPolicy(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration) {
	clock, ok := armedStallClocks.Load(sessionID)
	if !ok {
		return
	}
	sc, _ := clock.(*StallClock)
	if sc == nil {
		return
	}
	// Detaching a stalled synchronous tool is not destructive: it wakes the
	// model at the warn threshold (the sampler only calls past it), whatever
	// the abort timeout. Delegations and background/async jobs are excluded
	// inside the root's tool-stalled hook; a non-detachable call is left
	// running.
	if phase.kind == "tool" {
		if toolStalled, ok := sc.toolStalled.Load().(func(string, string, time.Duration) string); ok && toolStalled != nil {
			toolStalled(sessionID, phase.callID, sinceProgress)
		}
		return
	}
	// Off switch: abortTimeout == 0 disables abort only.
	if sc.abortTimeout <= 0 || sinceProgress < sc.abortTimeout {
		return
	}
	switch phase.kind {
	case "provider":
		// A re-issued request gets a full warn window to show progress
		// before the next retry or the abort.
		if last := sc.lastStallRetryNanos.Load(); last != 0 && stallNow().UnixNano()-last < int64(StreamIdleTimeoutDefault) {
			return
		}
		sc.lastStallRetryNanos.Store(stallNow().UnixNano())
		n := sc.stallRetries.Add(1)
		if n <= stallMaxRetries {
			slog.Warn("turn-stall policy: cancelling stalled provider request, retrying",
				"session_id", sessionID,
				"retry", n,
				"max_retries", stallMaxRetries,
				"since_progress", sinceProgress.Truncate(time.Second),
			)
			sc.cancelAttempt()
			return
		}
		slog.Warn("turn-stall policy: provider retries exhausted, aborting turn",
			"session_id", sessionID,
			"retries", n,
			"since_progress", sinceProgress.Truncate(time.Second),
		)
		sc.stallAbort(sinceProgress)
	case "internal":
		// No retry for internal phases: one fire past abortTimeout aborts.
		slog.Warn("turn-stall policy: internal phase stalled, aborting turn",
			"session_id", sessionID,
			"phase", phase.name,
			"since_progress", sinceProgress.Truncate(time.Second),
		)
		sc.stallAbort(sinceProgress)
	}
}

// cancelAttempt cancels the in-flight provider attempt, if one is armed.
func (sc *StallClock) cancelAttempt() {
	if cancel, ok := sc.attemptCancel.Load().(context.CancelFunc); ok && cancel != nil {
		cancel()
	}
}

// stallAbort invokes the turn's abort closure (handleWatchdogFire with
// causeStall + genCtx cancel), if one is armed.
func (sc *StallClock) stallAbort(elapsed time.Duration) {
	if abort, ok := sc.abortFn.Load().(func(time.Duration)); ok && abort != nil {
		abort(elapsed)
	}
}

// RunStallRetried runs attempt until it succeeds or the stall policy's
// retries are exhausted. A context.Canceled return counts as a stall retry
// only when the policy recorded a new retry for THIS attempt (stallRetries
// advanced past try) — an operator cancel leaves the counter alone and
// returns immediately. onRetry (which flags the cut attempt's partial
// content stale, mirroring fantasy's OnRetry) runs before each re-issue.
func RunStallRetried(stall *StallClock, attempt func() error, onRetry func(retry int)) error {
	if stall != nil {
		defer stall.ArmAttemptCancel(nil)
	}
	for try := 0; ; try++ {
		// Attribute the cancel: only a retry the POLICY issued for THIS
		// attempt (counter advanced past base) is re-issued; an operator
		// cancel leaves the counter alone and returns immediately.
		var base int32
		if stall != nil {
			base = stall.stallRetries.Load()
		}
		err := attempt()
		if err == nil || !errors.Is(err, context.Canceled) {
			return err
		}
		if stall == nil || stall.stallRetries.Load() <= base || try >= stallMaxRetries {
			return err
		}
		onRetry(try + 1)
	}
}

// the abort/cancel assertion recorders and registers cleanup. Exact port
// of the root test helper newArmedZeroClock, stopping just before its
// recorder arming. Mixed-root test seam; no production use.
func NewStallClockForTest(t testing.TB, sid string, call *agentcall.CallOptions) *StallClock {
	t.Helper()
	sc := &StallClock{}
	if call != nil {
		sc.abortTimeout = call.TurnStallTimeout
	}
	now := stallNow()
	sc.lastProgress.Store(now.UnixNano())
	sc.lastByte.Store(now.UnixNano())
	sc.SetPhase("internal", "preamble", "")
	armedStallClocks.Store(sid, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sid) })
	return sc
}

// StallSnapshot reads the diagnostics snapshot: lastProgress, lastByte and
// reasoningChars (UnixNano for the timestamps). Mixed-root diagnostic
// read seam; no production use.
func StallSnapshot(sc *StallClock) (lastProgress, lastByte, reasoningChars int64) {
	if sc == nil {
		return 0, 0, 0
	}
	return sc.lastProgress.Load(), sc.lastByte.Load(), sc.reasoningChars.Load()
}

// StallSetLastProgress overwrites the clock's lastProgress stamp (UnixNano)
// for a fixture. Mixed-root diagnostic write seam; no production use.
func StallSetLastProgress(sc *StallClock, nanos int64) {
	if sc == nil {
		return
	}
	sc.lastProgress.Store(nanos)
}

// Same-value aliases for the mixed-root tests' legacy lowercase spellings.
type stallClock = StallClock

const streamIdleTimeoutDefault = StreamIdleTimeoutDefault
