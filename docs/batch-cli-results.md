# Batch CLI results and lifecycle

`rush batch run` is synchronous. It waits until the root settles, then emits the root state and one result per task leaf in both text and `--json` modes: node ID, state, outcome code, summary, actual tool output, and response details where present. The output bytes come from the Runner's output reader, with the existing detached-output cache as the fallback after Runner release; persisted `response_envelope` alone is not process output. If the per-task cache is unavailable, result retrieval fails rather than silently presenting an empty output. A wait cancellation/error does not claim a final result.

The next process runs bounded startup `BatchRecoverPass` in `startBatchMaintenance`. Dead-host recovery deletes unannounced non-CLI reservations; unannounced CLI roots are retained. If a CLI root has live nodes, recovery interrupts them and closes pending controls; terminal roots with no live work are left untouched, including roots with pending notice delivery. The canonical recovery query and bounded Go-side predicate select unannounced non-CLI roots, live roots, and roots with pending controls.

`BatchPurgeExpired` runs in coordinator maintenance and explicit `rush sessions gc --jobs-older-than`. CLI batches use `NotifyAll`: notice election selects the root and suppresses descendant notices. The CLI presents results synchronously rather than pulling the interactive notice. Retention therefore ignores pending notice delivery on CLI roots (including a task root), while preserving protections for recent rows, live work, pending controls, wake/reaction debt, and delegated child notices. Old eligible rows can disappear after retention; `show` then reports not found. Do not claim indefinite durable-row retention.

JSON example:

```json
{"root_id":"...","owner_session_id":"...","state":"completed","leaves":[{"node_id":"....1","state":"completed","code":"","summary":"...","output":"tool output","details":{"...":"..."}}]}
```
