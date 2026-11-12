# Batch CLI results and lifecycle

See [session liveness, batch state, and completion](session-operations.md) for session state, process-exit checks, and safe retry/recovery boundaries.

`rush batch run` is synchronous. It waits until the root settles, then emits the root state, one result per task leaf, and—JSON mode only—response details: node ID, state, outcome code, summary, actual tool output, and optional details. Its text output prints each leaf's node ID, state, code, summary, and output, not `details`. It returns exit 0 only for a completed root; terminal but non-completed roots return a run-incomplete error. A wait cancellation/error does not claim a final result. Output bytes come from the Runner's output reader, with a best-effort process-local terminal-output cache as fallback after Runner release; `response_envelope` alone is not process output. The cache is volatile, size/TTL-limited, and may be unavailable after pressure or process exit; retrieval failure is reported rather than silently presented as empty output.

A session's `end_turn`, `between_turns` status, incomplete `unknown` batch read, or stop acknowledgement do not show that a batch task has physically terminated. Before retrying after an interrupted/uncertain run, verify that the original process has exited and that no live or unknown batch scope remains; inspect the root and all task node states via the batch tree. Only then decide whether an explicit resubmission is safe. Continue with the original checkout/worktree, role, and session. The table's root and leaf summaries/outcomes are durable metadata; captured output bytes are best-effort only and can be unavailable after process exit or cache eviction.

`BatchPurgeExpired` runs in coordinator maintenance and explicit `rush sessions gc --jobs-older-than`. CLI batches use `NotifyAll`: notice election selects the root and suppresses descendant notices. The CLI presents results synchronously rather than pulling the interactive notice. Retention therefore ignores pending notice delivery on CLI roots (including a task root), while preserving protections for recent rows, live work, pending controls, wake/reaction debt, and delegated child notices. Old eligible rows can disappear after retention; `show` then reports not found. Do not claim indefinite durable-row retention.

JSON example:

```json
{"root_id":"...","owner_session_id":"...","state":"completed","leaves":[{"node_id":"....1","state":"completed","code":"","summary":"...","output":"tool output","details":{"...":"..."}}]}
```
