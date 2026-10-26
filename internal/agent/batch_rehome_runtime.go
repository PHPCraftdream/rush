// Batch rehome (H4 lane P5): production helpers that survived the work-ledger
// deletion but whose bodies lived in deleted files (coordinator_await_tasks.go,
// work_ledger_bgshell_kill.go, turn_stall_tool_detach.go). Every call site in
// the batch runtime is unchanged; the bodies are re-homed here.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// awaitTaskSummary names the awaited tasks compactly for the wake message.
func awaitTaskSummary(refs []tools.AwaitTaskRef) string {
	out := ""
	for i, ref := range refs {
		if i > 0 {
			out += ", "
		}
		out += ref.ToolName + "/" + ref.ToolCallID
		if len(out) > 200 {
			return out[:200] + "..."
		}
	}
	return out
}

// clearSleepAll disarms the session's await_tasks sleep and cancels its
// pending max_wait once schedule. Called on EVERY drain launch that is
// allowed, on a human message (resetConsecutiveResume) and on Stop
// (Cancel). Errors are logged, never fatal: the sleep flag itself is
// already cleared by the time the cancel runs.
func (c *coordinator) clearSleepAll(sessionID string) {
	id := c.arb.clearSleepAll(sessionID)
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sched, err := c.wakeSchedulerFor()
	if err == nil {
		// Internal cleanup is idempotent, unlike the model-facing tool.
		err = sched.store.CancelSchedule(ctx, sessionID, id, sched.now())
		if err == nil {
			sched.Notify()
		}
	}
	if err != nil {
		slog.Warn("await_tasks: failed to cancel the max_wait schedule",
			"session_id", sessionID, "schedule_id", id, "err", err)
	}
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
	if c.background == nil {
		return false
	}
	_, ok := c.background.GetOwned(sessionID, shellID)
	return !ok
}

// stallToolStalled is the stall-clock's tool-phase hook. The stall-DETACH
// action it once drove (turn_stall_tool_detach.go: the stallDetachPending
// registry, stallDetacherTool, the job_output/job_kill controller) was
// deleted with the work ledger: there is no background job to detach into,
// and the pinned tests that exercised the detach were deleted with it
// (turn_stall_tool_test.go, batch_exec_generic_test.go both record the
// deletion). The detach ACTION is dead in the batch product; the call site
// in turnStallAbortPolicy stays so the tool phase keeps its explicit,
// documented branch, and the function honestly reports "nothing detached".
func stallToolStalled(sessionID, callID string, since time.Duration) string {
	return ""
}
