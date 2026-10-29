# Review backlog: items verified and deliberately not changed

From the r01-r10 architecture review sweep (Review Stop Rule: P2/P3 without a
reproduced failure go to the backlog, not into the same cycle). Each entry says
what was checked, why nothing was changed, and what would unblock it.

## Decided after advisor review (an `oh` agent and the rush reviewer role agreed)

- **Live-work title for a reused tool-call id**: fixed in #291. The title now comes
  from the input the call stored in its task node's `task_payload`; no schema change.
  `announce_message_id` was rejected as the key (it is the id of a server-created
  message, NULL until the ack and for CLI roots), and so was a created_at
  heuristic (second resolution). Open refinement: for roots without a usable
  payload input, resolve `policy.message_id` from the payload and fetch exactly
  that message instead of the first history match.
- **Aged unread completion rows**: fixed in #292, narrowed to roots with
  `origin_cli = 1 AND announced = 0` (their debt is inert and only the live CLI
  settles it). Web and announced roots keep their debt protection. Remaining
  smaller leak: a CLI root whose delegation child session holds unsettled debt
  stays protected by the child terms.

## Not demonstrated (no failing scenario found)

- **jobbatch published entries keep payloads** (`internal/jobbatch/runner.go`,
  `runner_commit.go`): the worker could not establish which references no reader
  needs; published reads go through retained views and snapshots.
- **jobbatch whole-forest snapshot per transition** (`runner_commit.go`,
  `view.go`): a benchmark of `Engine.Apply` grows with roots (3.2 / 8.5 / 184 us at
  10 / 100 / 1000) but does not isolate the durable snapshot path. Benchmark the
  snapshot itself before designing per-root copy-on-write.
- **Agent leaves launched through model `batch_run` skip the captured-child
  handoff** (`batch_model_control.go`, `batch_child_tool_scope.go`): ownership
  path read, no end-to-end scenario run. A test needs the captured-agent fixture
  of `batch_child_tool_scope_test.go` driven through the model `batch_run` path.
- **Stale queue resume** (rush run): two attempts could not reproduce it and the
  original evidence was lost. Reproduce first.
- **Fenced root drain leaves its provider pins open on the wake path**: checked,
  not reproduced. With the fence wired the pins are visible while the turn runs
  and gone after `wakeSession` returns.
- **`sessions locks` is ~80 ms per lock on a real machine**: batching the
  call-tree lookup removes the per-lock query (synthetic 400 stale locks, whole
  command, 9.5 s to 1.9 s per the worker), but the OS-lock probe and process
  liveness checks are untouched and the real-machine cost was not profiled.

## Not touched on purpose

- `TestCLIQuota*` and `TestRunTimeout*` in `internal/app` compute their budgets
  from the real retry backoff (`quotaRetryWait()`), so the wait is the scenario.
