// Batch rehome (H4 lane P5, phase A): the tool-result metadata shapes moved
// verbatim out of async_tool.go and work_ledger_cap.go before their deletion.
// They are JSON contracts read back by step_progress.go's step classifier
// (asyncCapRefused, isWaitOnlyCall) and written by delegation_question.go and
// the batch delivery paths; only the ledger-owned *producers* were deleted,
// not the shapes.
package agent

import "charm.land/fantasy"

// asyncToolMetadata is the tool-response metadata of a delegated or
// backgrounded call.
type asyncToolMetadata struct {
	Async bool `json:"async"`
	// Inline marks the A14 immediate-answer form: this result IS the job's
	// outcome (a natural completed/failed finish that committed within the
	// inline window). Web must treat async:false as not-pending
	// (isPendingJob, ActionRow.tsx).
	Inline         bool   `json:"inline,omitempty"`
	JobID          string `json:"job_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status"`
	// ClaimID tags this as the job's OWN "started" result (ack tag): the
	// delivery gate fuses only a result carrying the job's claim, never one
	// that merely shares its tool_call_id.
	ClaimID string `json:"claim_id,omitempty"`
}

// asyncCapMetadata is the JSON shape the async-cap refusal emits as its
// metadata: the step classifier's asyncCapRefused keys off exactly these
// names.
type asyncCapMetadata struct {
	AsyncCap struct {
		Running    int `json:"running"`
		Limit      int `json:"limit"`
		WaitTimers int `json:"wait_timers"`
	} `json:"async_cap"`
}

// wrapToolsWithStallDetach is retained as an identity seam: the stall-detach
// REGISTRY and detacher ACTION were deleted with the work ledger
// (turn_stall_tool_detach.go); the generic stall clock no longer detaches a
// running tool. The retained composition site (coordinator_tools.go) keeps
// calling through this seam so the toolset layering stays reviewable and the
// call ordering does not change shape.
func wrapToolsWithStallDetach(tools []fantasy.AgentTool) []fantasy.AgentTool {
	return tools
}

// wrapAsyncTools is retained as an identity seam, the twin of
// wrapToolsWithStallDetach above: the asyncTool WRAPPER and the workLedger
// behind it were deleted with the ledger (batch_install_tools.go owns the
// batch arming now). The retained composition site (coordinator_tools.go's
// finishToolAssembly OFF branch, unreachable under the always-on batch
// binding) keeps calling through this seam so the toolset layering stays
// reviewable and the call ordering does not change shape.
func (c *coordinator) wrapAsyncTools(tools []fantasy.AgentTool) []fantasy.AgentTool {
	return tools
}
