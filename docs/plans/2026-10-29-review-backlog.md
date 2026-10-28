# Review backlog: items verified and deliberately not changed

From the r01-r10 architecture review sweep (Review Stop Rule: P2/P3 without a
reproduced failure go to the backlog, not into the same cycle). Each entry says
what was checked, why nothing was changed, and what would unblock it.

## Needs a decision

### Live-work title for a reused tool-call id
`internal/server/handlers_livework.go` (`liveWorkTitleKey`, `titleCache.titles`).
Providers number tool calls per response, so one session can retain two
assistant messages with the same tool-call id and different commands. The cache
key carries `input_hash`, but the history lookup matches only session and
tool-call id, reads oldest-first and fills every matching missing key from the
first match: both incarnations get the first command's title.

Not fixed because the batch root stores neither the submitting message id nor a
hash the server can recompute from a message's tool input: the stored hash is
`batchCommitSpecHash` (sha256 of the marshalled `jobbatch.Spec`,
`internal/session/batch_commit.go`), a parsed spec, not the raw input. Options:
(a) store the submitting message id on the batch root (schema change), or
(b) export the spec parse + hash and let the server recompute the hash per
candidate tool call. The existing reuse test deletes the old message first, so it
never covers retention of both.

### Aged unread completion rows are never purged
`BatchPurgeExpired` removes "whole terminal, debt-free, idle delegations". A
completion with unconsumed wake debt (`delivery='pending'`, or `wake=1` and
`reacted=0`) is protected indefinitely, so a crashed CLI that never settled its
debt leaves rows that accumulate. A rush attempt dropped the debt predicate
altogether; rejected, because an unread notice of a dormant session must survive.
Product call: purge debt older than the retention age, possibly only for
CLI-origin roots (`origin_cli = 1`), whose notices no interactive channel consumes
(`BatchSettleCLIWakeDebt` is the only settler).

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
