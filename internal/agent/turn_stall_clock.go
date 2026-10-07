package agent

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/log"
)

// This file is the Phase 1 infrastructure of the turn-stall detector: an
// in-process progress clock, a phase tracker, and ONE process-wide sampler
// goroutine. It is deliberately diagnostics-only — NO abort/kill logic
// lives here. The abort policy is added later via SetTurnStallPolicy (see
// its doc); with no policy installed the sampler only warns and captures a
// goroutine dump.

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

// stallClock is one turn's stall-detection state, shared by turnStream,
// the runTurn body and the sampler via the armed registry below. Every
// field is an atomic or an atomic.Pointer, so the sampler can read a
// consistent-enough snapshot without locks.
type stallClock struct {
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
	// threshold is always streamIdleTimeoutDefault; only the (later)
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
}

// stallWarnWindow rate-limits warn+dump per armed clock: at most one warn
// (and dump capture) every 30 minutes while a stall persists.
const stallWarnWindow = 30 * time.Minute

// stallSamplerTick is how often the process-wide sampler re-checks every
// armed clock.
const stallSamplerTick = 15 * time.Second

// armedStallClocks holds the per-session stall clocks, keyed by session
// id. An entry exists only while a turn for that session is running
// (armed at the top of runTurn, disarmed in runTurn's defer).
var armedStallClocks sync.Map // string -> *stallClock

// stallNow is the clock the sampler and phase stamps read. A package
// variable so tests can inject a fake clock deterministically.
var stallNow = time.Now

// SetTurnStallPolicy installs the stall-policy seam invoked by the
// sampler on every threshold crossing. The default (nil) policy means
// warn+dump only; a later worker installs an abort policy here. The
// function receives identifiers and durations only — never prompt text
// or tool input. Safe to call at any time; reads are nil-safe.
func SetTurnStallPolicy(fn func(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration)) {
	turnStallPolicy.Store(fn)
}

// turnStallPolicy holds the injectable policy hook. Stored via
// atomic.Value so the sampler never takes a lock on the hot path.
var turnStallPolicy atomic.Value // func(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration)

// stallSamplerOnce guards the process-wide sampler goroutine: started
// lazily exactly once by the first armTurnStall, never per-turn
// (pattern-matches internal/heartbeat's ensureWorker).
var stallSamplerOnce sync.Once

// armTurnStall creates (or reuses) call.SessionID's stall clock, arms it
// in the registry, stamps the initial phase, and lazily starts the
// sampler. Returns the clock for the turn's hook lines.
func armTurnStall(sessionID string, call *CallOptions) *stallClock {
	installTurnStallPolicy()
	clock := &stallClock{}
	if call != nil {
		clock.abortTimeout = call.TurnStallTimeout
	}
	now := stallNow()
	clock.lastProgress.Store(now.UnixNano())
	clock.lastByte.Store(now.UnixNano())
	clock.setPhase("internal", "preamble", "")
	armedStallClocks.Store(sessionID, clock)
	stallSamplerOnce.Do(func() { go stallSampler() })
	return clock
}

// disarmTurnStall removes sessionID's clock. nil-safe semantics: a stale
// disarm (turn ended after another turn re-armed) only deletes the entry
// present, which the newer turn immediately re-creates on its own arm.
func disarmTurnStall(sessionID string) {
	armedStallClocks.Delete(sessionID)
}

// stallSetPhase stamps an internal phase onto sessionID's armed clock.
// nil-safe: called from paths (e.g. manual compaction) that may run
// without an armed clock. One-line hook target.
func stallSetPhase(sessionID, kind, name string) {
	if clock, ok := armedStallClocks.Load(sessionID); ok {
		clock.(*stallClock).setPhase(kind, name, "")
	}
}

// setPhase replaces the phase snapshot atomically.
func (sc *stallClock) setPhase(kind, name, callID string) {
	if sc == nil {
		return
	}
	sc.phase.Store(&turnPhase{kind: kind, name: name, callID: callID, since: stallNow()})
}

// markProgress records a durable progress point (text delta, tool call,
// tool result, step finish, checkpoint persist). One-line hook target.
func (sc *stallClock) markProgress() {
	if sc == nil {
		return
	}
	sc.lastProgress.Store(stallNow().UnixNano())
}

// markByte mirrors the stream watchdog's every-callback liveness clock.
func (sc *stallClock) markByte() {
	if sc == nil {
		return
	}
	sc.lastByte.Store(stallNow().UnixNano())
}

// toolEnter brackets a tool execution: counts it in flight and stamps the
// tool phase. One-line hook target (OnToolCall).
func (sc *stallClock) toolEnter(name, callID string) {
	if sc == nil {
		return
	}
	sc.toolsInFlight.Add(1)
	sc.setPhase("tool", name, callID)
}

// toolExit closes a tool execution; when the last one finishes the phase
// returns to the provider. One-line hook target (OnToolResult).
func (sc *stallClock) toolExit() {
	if sc == nil {
		return
	}
	remaining := sc.toolsInFlight.Add(-1)
	if remaining <= 0 {
		sc.toolsInFlight.Store(0)
		sc.setPhase("provider", "", "")
	}
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
		clock, _ := value.(*stallClock)
		if clock == nil {
			return true
		}
		stallSamplerCheck(sessionID, clock, now)
		return true
	})
}

// abortAfter returns the clock's configured abort timeout.
func (sc *stallClock) abortAfter() time.Duration {
	return sc.abortTimeout
}

// stallSamplerCheck evaluates one armed clock. The policy seam is invoked
// on EVERY threshold crossing (so provider retries and tool detaches are
// not gated by the warn window); only the warn log + goroutine dump are
// rate-limited to one per stallWarnWindow. The clock's abortTimeout is
// diagnostics-only here; the abort policy reads it via abortAfter.
func stallSamplerCheck(sessionID string, clock *stallClock, now time.Time) {
	threshold := streamIdleTimeoutDefault
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
