package batchlife

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// AwaitTaskSummary names the awaited tasks compactly for the wake message.
func AwaitTaskSummary(refs []tools.AwaitTaskRef) string {
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

// StallToolStalled is the stall-clock's tool-phase hook. The stall-DETACH
// action it once drove (turn_stall_tool_detach.go: the stallDetachPending
// registry, stallDetacherTool, the job_output/job_kill controller) was
// deleted with the work ledger: there is no background job to detach into,
// and the pinned tests that exercised the detach were deleted with it
// (turn_stall_tool_test.go, batch_exec_generic_test.go both record the
// deletion). The detach ACTION is dead in the batch product; the call site
// in turnStallAbortPolicy stays so the tool phase keeps its explicit,
// documented branch, and the function honestly reports "nothing detached".
func StallToolStalled(sessionID, callID string, since time.Duration) string {
	return ""
}
