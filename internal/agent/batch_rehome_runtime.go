// Batch rehome (H4 lane P5): production helpers that survived the work-ledger
// deletion but whose bodies lived in deleted files (coordinator_await_tasks.go,
// work_ledger_bgshell_kill.go, turn_stall_tool_detach.go). Every call site in
// the batch runtime is unchanged; the bodies are re-homed here.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
)

// clearSleepAll disarms the session's await_tasks sleep and cancels its
// pending max_wait once schedule. Called on EVERY drain launch that is
// allowed, on a human message (resetConsecutiveResume) and on Stop
// (Cancel). Errors are logged, never fatal: the sleep flag itself is
// already cleared by the time the cancel runs.
func (c *coordinator) clearSleepAll(sessionID string) {
	(&batchRehomeRuntimeComponent{host: c}).clearSleepAll(sessionID)
}

func (c *coordinator) cancelAwaitTimer(sessionID, id string) {
	(&batchRehomeRuntimeComponent{host: c}).cancelAwaitTimer(sessionID, id)
}

// bgShellRowCancelled reports whether a stop control has already cancelled
// shellID in sessionID: its completion must then produce neither a wake
// notice nor an auto-resume slot, because the stop's own result IS the
// shell's answer. It is also called under the coordinator's bgArrival gate
// from persistBGShellCompletion, after the gate a winning stop decision
// holds.
//
// Batch-world representation: the work-ledger bg_shell row (state
// "cancelled") is gone, and the batch runtime's batch_stop control targets
// batch roots, never SDK background shells. A background shell leaves the
// coordinator's manager ONLY through an explicit cancellation control --
// BackgroundShellManager.Kill/KillOwned (tools/bash.go's caller-cancellation
// path) or KillAll (session stop) -- each of which Takes the shell out of
// m.shells before its completion callback can fire; a shell that finished on
// its own stays in the manager until a later bash tool call Removes it, and
// such a synchronous completion never reaches notifyBackgroundJobDone at
// all. Absence from the manager is therefore this build's only honest fact
// of "cancelled by a stop control".
func (c *coordinator) bgShellRowCancelled(sessionID, shellID string) bool {
	return (&batchRehomeRuntimeComponent{host: c}).bgShellRowCancelled(sessionID, shellID)
}

type batchRehomeRuntimeHost interface {
	arbClearSleepAll(sessionID string) string
	wakeSchedulerFor() (*wakeScheduler, error)
	bgShellManager() *shellbg.BackgroundShellManager
}

type batchRehomeRuntimeComponent struct{ host batchRehomeRuntimeHost }

func (w *batchRehomeRuntimeComponent) clearSleepAll(sessionID string) {
	w.cancelAwaitTimer(sessionID, w.host.arbClearSleepAll(sessionID))
}

func (w *batchRehomeRuntimeComponent) cancelAwaitTimer(sessionID, id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sched, err := w.host.wakeSchedulerFor()
	if err == nil {
		// Internal cleanup is idempotent, unlike the model-facing tool.
		err = sched.CancelSchedule(ctx, sessionID, id)
		if err == nil {
			sched.Notify()
		}
	}
	if err != nil {
		slog.Warn("await_tasks: failed to cancel the max_wait schedule",
			"session_id", sessionID, "schedule_id", id, "err", err)
	}
}

func (w *batchRehomeRuntimeComponent) bgShellRowCancelled(sessionID, shellID string) bool {
	if w.host.bgShellManager() == nil {
		return false
	}
	_, ok := w.host.bgShellManager().GetOwned(sessionID, shellID)
	return !ok
}

func (c *coordinator) arbClearSleepAll(sessionID string) string {
	return c.arb.clearSleepAll(sessionID)
}

func (c *coordinator) bgShellManager() *shellbg.BackgroundShellManager {
	return c.background
}
