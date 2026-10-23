package agent

/*
batch_exec_agent_doc.go — R7c agent scope adapter: behavior mapping and
assumed contracts. The adapter is the LIVE delegation authority: a batch
executor runs it through the registered batchAgentFamily (the deleted
asyncTool path is gone).

The adapter is one engine leaf for a delegation (agent / agentic_fetch). Its
driver is an injected run factory (one agent turn loop), its scope is
batchAgentScope, and the live-descendant fact comes from an injected reader
over the R10a live-root seam. No global state; everything is injected.

Old agent behavior -> new counterpart:

| Old behavior (async agent path) | New counterpart |
| --- | --- |
| Agent tool wraps a nested Run in a goroutine; tool response is not termination (the deleted asyncTool path) | Run factory launches one turn loop (batchAgentRunFactory); report comes only from the scope watcher, never from the tool response. |
| Finalization parks results; release checks child work, shells, holds (the deleted ledger delegation release) | Scope closes only when the turn ended AND no question is held AND descendants are confirmed terminal; report after closure, exactly once. |
| Live child work discovered via lag-tolerant scans (the deleted ledger) | Injected batchAgentDescendantReader over R10a; incomplete or unknown reads look live (conservative-read rule) and keep the scope open. |
| Held question: the driver is resumed via the held-answer bridge; ask_question returns a question-shaped response | Universal: any depth holds via batchAgentHost.Ask; the ask_question tool returns a successful question-shaped response; the node stays live; the answer arrives by Inject and is routed to exactly the addressed depth. |
| Inject via inject_agent into a child session (AgentControl) | Execution.Inject parses batchAgentInjectMessage{depth,text}; rejection is deterministic (stopped, stale generation, no matching depth) — never a silent loss, never delivery to a new incarnation. |
| Stop/run cancel commit "stopped" before physical termination | Removed per plan correction 3: Stop records intent, cancels the run context (the addressed subtree at depth) and returns the turn acknowledgement; only confirmed closure settles (Cancelled). Siblings are separate executions and untouched. |
| Stop ack / timeout racing settlement | Stop acknowledgement never settles; batchAgentScopeResult reports Cancelled only from confirmed closure facts. |
| Generation-bound policy inheritance, driver registration before execution (coordinator_subagents) | Per-incarnation state (question channel, injected messages, watchers) lives in one batchAgentExecution; Dispose waits for the late kill (driver Exit) and releases it; a finished incarnation rejects Inject as stale. |
| Stream watchdog panic containment around nested Run | Run factory panics are contained at Start (agent_start_panic) and classification happens outside the factory goroutine. |
| Denied/failed nested runs surface as prose | A run error is Failed agent_run_error, an error response is Failed agent_error, with the envelope retained as Details and the summary readable via ReadOutput. |

Contracts assumed from the engine (internal/jobbatch):

- Start returns a non-nil error only when no execution exists; report states
  the execution stopped and is called exactly once (executor.go).
- Stop may overlap ReadOutput, Inject and report, never another Stop.
- Messenger, OutputReader and Disposer are optional capabilities; Dispose is
  called once after termination is reported and the handle is given up, and
  is the release barrier for generation state.
- Launch.ID/Token identify the admission attempt; Payload carries
  batchLeafPayload; the pinned policy leaf resolves the tool (R1) — the
  agent family does not resolve leaves, the run factory does.
- Result.State must be Reportable; codes follow the Runner machine-code rule
  (agent_start_panic, agent_run_error, agent_error are new nonreserved
  codes, consistent with the sibling adapters' tool_run_error/tool_error).

Open decisions (smallest choice made, per plan row R7c):

- task_outcome (design 9.3): D7 says it is not introduced now. The adapter
  carries an optional Declared outcome on the turn result that may only
  reclassify a clean closure to a Reportable state, is scoped to this
  incarnation and can never overwrite a terminal node. Whether any tool
  surface will set it remains undecided upstream; nothing sets it today.
- Descendant reads have no push event in the R10a seam, so the watcher polls
  at batchAgentDeps.Poll (default 10 ms) plus change signals; a push seam
  would remove the poll and is deferred.
*/
