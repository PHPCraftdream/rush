package modelkit

import "github.com/PHPCraftdream/rush/internal/agent/tools"

const AgentToolName = "agent"

// workerToolNames are added on top of the sub-agent's existing (read-only)
// AllowedTools when it is acting as a worker (see workerSubAgentActive):
// enough hands to actually do delegated work, not just look around.
//
// Deliberately excludes "agent": a worker must not spawn sub-workers of its
// own — recursion guard, keeps the delegation tree exactly two levels deep
// (orchestrator -> worker).
//
// Includes "ask_question": runSubAgent now recognizes AwaitingAnswerError
// before its generic error branch and returns a question-shaped SUCCESSFUL
// tool response (see subAgentQuestionText in question_stop.go) instead of
// collapsing it into "Failed to generate response: ...". The orchestrator
// answers via AgentParams.ResumeSessionID, which continues the same
// sub-session rather than starting a fresh one. See
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, phase 3, for the full
// round-trip design and why a synchronous blocking version is impossible.
//
// Includes "await_tasks" (and its post-await batch_output read): the worker's await_tasks BLOCKS
// inside the turn (no StopTurn) and after its wake the worker must read the
// finished jobs' outputs; and a worker that started a background shell had
// no way to read it at all before. await_tasks.md already forbids looping
// on job_output.
var workerToolNames = []string{"edit", "multiedit", "write", "bash", "todos", "download", "fetch", tools.AskQuestionToolName, tools.AwaitTasksToolName}

func WorkerToolNames() []string {
	return workerToolNames
}
