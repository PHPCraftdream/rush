// Package turnstep leaf helpers for per-turn step logic. turn_budget.go
// holds the stream-watchdog onFire handler moved verbatim from the agent
// package's turnBudget component (EX-0H): a pure helper with no package
// state — every value it touches arrives as a parameter, including the
// hard-cap configuration, which the agent-side root passes as the
// h.timeoutHardCapCfg method value so it is only ever evaluated in the
// causeHardCap branch, at the original point.
package turnstep

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

// HandleWatchdogFire is runTurn's stream-watchdog onFire callback, moved out
// into a named leaf function (task #243, EX-0H) so a unit test can invoke the
// REAL production logic directly — calling this function with a synthetic
// watchdogCauseVal — instead of only exercising a test-local copy of this
// shape that could silently drift from what runTurn actually wires up. The
// onFire closure inside runTurn is just a thin dispatch:
// `func(elapsed, cause) { a.handleWatchdogFire(...) }`.
//
// watchdogCauseVal is runTurn's local atomic.Int32 (not a sessionAgent
// field: it is genuinely per-turn state, reset fresh on every call), passed
// in by pointer so this function can store into the caller's copy.
// toolMaxDuration/idleTimeout are likewise runTurn locals (the resolved,
// possibly-overridden effective durations for THIS turn) rather than
// sessionAgent fields, so they are passed explicitly instead of read off a.
// smartModel follows the same rule, and then some: runTurn does NOT re-read
// a.smartModel here at fire time — it passes in the value it took once,
// before the turn, from the immutable turnConfig snapshot built by
// resolveTurnConfig, a per-call value copy of every shared model/prompt
// field (task #265 P0-1) that runTurn reads as `smartModel := cfg.smartModel`.
// a.smartModel is mutable mid-turn via SetModels (coordinator.UpdateModels /
// web-UI override path), so re-reading at fire time would name the model the
// user SWITCHED TO after the hang started rather than the one that actually
// hung (task #252 — the #243 extraction regressed exactly this by re-reading
// a.smartModel here).
//
// hardCap is the host's timeoutHardCapCfg method value (config lookup), NOT
// a resolved duration: it is called only in the causeHardCap branch below,
// exactly where the original body called h.timeoutHardCapCfg() — other fire
// causes pay no config-read cost.
//
// INVARIANT (task #227 + #232, preserved verbatim by this extraction): the
// cause is stored FIRST, synchronously, before any other work in this
// function. startStreamWatchdog invokes onFire strictly before cancel(), so
// whatever this function does before returning is on the critical path to
// unblocking agent.Stream. If the OUTER ctx is also cancelled independently
// of this watchdog (Ctrl-C, a --timeout firing at the same moment) while
// this function is still running, the main goroutine reading
// watchdogCauseVal after observing stalled==true must never see the zero
// value (CauseIdleStall) for what was actually a hard-cap/tool-timeout fire —
// that would misreport the cause via a DIFFERENT race than the one #227
// fixed (external cancellation racing this callback, rather than this
// callback racing its own cancel()).
func HandleWatchdogFire(
	cause watchdog.Cause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel agentcall.Model,
	hardCap func() time.Duration,
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
	case watchdog.CauseToolTimeout:
		slog.Warn(
			"agent: watchdog firing — tool execution exceeded cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"cap", toolMaxDuration.String(),
		)
	case watchdog.CauseHardCap:
		slog.Warn(
			"agent: watchdog firing — turn exceeded --timeout-hard-cap, force-cancelling",
			"session_id", sessionID,
			"provider", smartModel.ModelCfg.Provider,
			"model", smartModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"hard_cap", hardCap().String(),
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

// RequeueSummarizeSnapshot puts a compaction back in the summarize queue after a
// lock-acquisition failure so the session's next owner drains and runs it.
// GetOrSet (not Set) never clobbers a newer queued request, and no compaction
// is spawned here, so a long-held lock cannot cause a busy retry loop.
func RequeueSummarizeSnapshot(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string, snapshot *agentcall.SummarizeSnapshot) {
	queue.GetOrSet(sessionID, func() *agentcall.SummarizeSnapshot { return snapshot })
	slog.Info("agent.runSummarize: re-queued compaction after session lock failure",
		"session_id", sessionID)
}

func SummarizeQueued(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) bool {
	_, ok := queue.Get(sessionID)
	return ok
}

func TakeSummarizeQueue(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) (*agentcall.SummarizeSnapshot, bool) {
	snapshot, ok := queue.Take(sessionID)
	return snapshot, ok
}

func CancelQueuedSummarize(queue *csync.Map[string, *agentcall.SummarizeSnapshot], sessionID string) {
	queue.Del(sessionID)
}
