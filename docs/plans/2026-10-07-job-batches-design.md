# Job batches — design

Status: design only, nothing implemented. Grounded in a read-only pass over
HEAD `322705af`; every `file:line` below is as of that commit and will drift.

## 0. Requirements

From the operator:

- A batch is a list of items. An item is a nested batch, a CLI command, a tool
  call, an MCP call, or an agent launch.
- A batch is parallel (everything starts at once, the batch waits for all) or
  sequential (one by one), and a sequential batch either stops on the first
  failed item or continues with the next one.
- Per batch, one of two notification modes: notify on every item (finished or
  failed), or notify only once all items have stopped.
- Several batches may run at the same time.
- Any batch accepts new items while it runs; any item in any batch can be
  stopped; the output of any item can be inspected; at any nesting level a
  message can be injected into an agent item.

Added while this design was being written:

- An agent item must be able to end in two distinct ways, success or failure,
  and that outcome must drive the batch (stop-on-fail, summaries).
- The batch mechanism must be separate, abstract code that is tested on its
  own and is easy to apply later from other surfaces.

Naming: "batch" is already taken twice in the tree — the fs_* batch runner
(`internal/agent/tools/fs_batch.go`) and the batched live-work reader
(`internal/session/async_job_reader_batch.go`). This feature is called
**job batches**; the package is `jobbatch`, tools are `batch_run`,
`batch_add`, `batch_status`, `batch_stop`.

## 1. Who creates and drives batches

**v1: the model, through new tools. The operator CLI is phase 3, the web UI
phase 5.**

Reasons:

1. All execution machinery is keyed to a session and a tool call: the work
   ledger's job key is `(owner, toolCallID)` (`internal/agent/work_ledger.go:254`),
   a non-sync job is announced through the ack gate on its own tagged "started"
   tool result (`work_ledger.go:442-503`, `async_tool.go:265-270`), and
   completions reach the agent as pulled notices plus a wake
   (`coordinator_background.go:71-93`, `coordinator_wake.go:27-79`). A model
   tool gets all of that unchanged.
2. Jobs belong to the process that hosts them. There is no cross-process
   job-level stop today: `rush sessions jobs` is observation-only, explicitly
   because only the owning host can stop its jobs
   (`internal/cmd/sessions_jobs.go:11-18`). Operator control from the CLI
   needs a new control channel (phase 3).
3. The operator is not blind in v1. Leaf items are ordinary `async_jobs` rows,
   so `rush sessions jobs` and the web live-work panel
   (`internal/server/handlers_livework.go:1-13`) list them, and
   `rush sessions inject <child-session-id>` reaches any agent item
   cross-process (`internal/cmd/sessions_inject.go:202-235`). The operator can
   also tell the model to add or stop items through that inject.

## 2. Architecture: an abstract engine plus rush adapters

The batch logic lives in its own package, `internal/jobbatch`, which imports
nothing from `internal/` (no agent, session, db, fantasy). It is a functional
core with a thin imperative shell:

```go
package jobbatch

type Kind string   // KindBatch, KindCommand, KindAgent, KindTool, KindMCP
type Mode string   // ModeParallel, ModeSequential
type OnFail string // OnFailStop, OnFailContinue
type Notify string // NotifyEach, NotifyAll
type State string  // Pending, Running, Completed, Failed, Cancelled,
                   // TimedOut, Interrupted, Skipped

type NodeID string // "<rootID>.<ordinal>[.<ordinal>...]", ordinals append-only

type Spec struct {
	Kind        Kind
	Payload     json.RawMessage // opaque to the engine; the executor parses it
	Mode        Mode            // batch nodes only
	OnFail      OnFail          // sequential batch nodes only
	Notify      Notify          // batch nodes only
	MaxParallel int             // batch nodes only
	Items       []Spec          // batch nodes only
}

// Tree is the pure state machine: no goroutines, no locks, no clock, no I/O.
func New(root NodeID, spec Spec, lim Limits) (*Tree, []Command, error)
func (t *Tree) Apply(ev Event) ([]Command, error) // Added, StopRequested,
                                                  // Started, StartFailed, Settled
func (t *Tree) View(id NodeID) (NodeView, bool)

// Commands the tree emits: StartLeaf{ID, Kind, Payload, Notify bool},
// StopLeaf{ID}, Notice{ID, ...}, Persist{Nodes}, Done{Summary}.

// The shell: the only part with a mutex, and it never calls out while
// holding it (commands are executed after unlock).
type Executor interface {
	Start(ctx context.Context, id NodeID, kind Kind, payload json.RawMessage, notify bool) error
	Stop(id NodeID) error
}
type Sink interface{ Deliver(n Notice) }
type Store interface{ Save(nodes []NodeView) error }

type Runner struct{ /* mu, tree, exec, sink, store, done chan */ }
func (r *Runner) Settle(id NodeID, s State, summary string) // idempotent
func (r *Runner) Add(parent NodeID, items []Spec) ([]NodeID, error)
func (r *Runner) Stop(id NodeID) error
func (r *Runner) Done() <-chan struct{}
```

Why this shape:

- Testable alone: every invariant below is checked by feeding events to
  `Tree.Apply` and asserting on the emitted commands, with no database, no
  provider and no goroutines; `Runner` is tested with a fake `Executor` under
  `-race`; a randomized test drives random event sequences and checks the
  invariants after every step.
- Easy to apply later: the only rush-specific seam is `Executor` (plus `Sink`
  and `Store`). The same engine can back the model tools (v1), the operator
  CLI (phase 3), a model-less `rush batch run spec.json` (open question 6),
  and the web UI, without touching the core.
- Deadlock-free by construction at this layer: the core has no lock, the
  runner holds one lock only around `Apply`, and adapters call back into the
  runner only after releasing the ledger lock (the ledger already promises
  that for its own callbacks, `work_ledger.go:370-371`).

Core invariants (each one a table-driven test in `internal/jobbatch`):

- **E1** A leaf gets at most one `StartLeaf`. `Settled` for an unknown or
  already-terminal node is a no-op.
- **E2** Sequential: `StartLeaf(k+1)` is emitted only after `Settled(k)`.
- **E3** Sequential stop-on-fail: after a child ends Failed, TimedOut or
  Interrupted, every Pending sibling becomes Skipped and nothing else starts.
- **E4** Every batch node emits exactly one terminal notice, never before all
  its children are terminal. Once terminal it refuses `Add`.
- **E5** Running leaves stay within `MaxParallel` per batch and the global
  limits. `StartFailed{Retryable: true}` (capacity) keeps the item Pending,
  never Failed, and it is retried on the next `Settled`.
- **E6** `Stop(node)`: no new starts in the subtree, `StopLeaf` for each
  running leaf, Pending children become Cancelled. The node is terminal only
  once every running leaf has settled.
- **E7** Notices: a child's settle is announced iff its parent batch has
  `NotifyEach`; the root's completion is always announced, exactly once.

Rush adapters live in `internal/agent` (section 5).

## 3. Data model

**Identity.** The batch id is the tool call id of the `batch_run` call, which
is also the root's `async_jobs` key — the same id the model already uses for
jobs. An item id is `<batchID>.<path>` (for example `toolu_x.2.1`). Ordinals
are append-only, so adding items never renumbers anything. A leaf's
`async_jobs.tool_call_id` is its item id. An agent item's child session id is
`CreateAgentToolSessionID(<batch_run message id>, <item id>)`
(`internal/session/session_lifecycle.go:160`). Item ids must not contain
`#reused#` (`internal/session/async_job_store.go:699-709`).

**Tree.** The root batch is one ledger job (one `async_jobs` row,
`kind='batch'`). Nested batches are pure tree nodes with no row. Each running
leaf is one ledger job with a row of its existing kind (`command` or `agent`).

**States.** Pending → Running → {Completed, Failed, Cancelled, TimedOut,
Interrupted}. Pending → {Skipped, Cancelled}. A batch node is Running while
any child is non-terminal; Completed if every child Completed; Failed if any
child Failed, TimedOut or Interrupted (after the on-fail policy has played
out); Cancelled if it was stopped.

**What "failed" means per kind.**

| Kind | Failed when | Notes |
|---|---|---|
| command (`run_command`) | the tool response is an error: non-zero exit, program not found, the process-kill timeout, an agentguard refusal, a hook deny (`internal/agent/tools/run_command.go:236-268`) | `causeNaturalFinish` maps `isError` to `failed` (`work_ledger_transition.go:96-102`) |
| agent | the delegation is released with `isError`: provider error or no text output (`coordinator_subagents.go:339-346`), last turn finished with an error (`coordinator_work_scope.go:109`), settle-by-failure (`coordinator_work_scope.go:53-68`), and, new, a declared failure (section 4) | a question (`AwaitingAnswerError`) is not terminal: the delegation is held and the item stays Running |
| tool / MCP (phase 4) | an error response or a Go error. `Owner.RunTool` errors already become error responses (`internal/agent/tools/mcp-tools.go:161-164`) | a hook Halt has no turn to stop: the item fails and its batch is stopped |
| nested batch | any descendant failed after the policy ran | — |
| any leaf | Interrupted: its host died and recovery transitioned the row (`internal/session/async_job_recovery.go:110-161`) | wake=0 |

**Cancellation propagation.**

- Stop of a batch node: `Runner.Stop` → per running leaf `Executor.Stop`.
  The command adapter calls `StopRunCommandJob` (snapshot, then transition,
  then kill; `work_ledger.go:817-875`). The agent adapter calls
  `coordinator.Cancel(child)` exactly as `stop_agent` does
  (`coordinator_agent_control.go:274-286`), which delivers the cancelled
  delegation.
- Stop of one item inside a nested parallel batch stops only that leaf; its
  siblings keep running. In a sequential batch a Cancelled item does not
  trigger stop-on-fail (open question 2).
- Session Stop: `cancelSession` already cancels every job the session owns —
  the root and all leaves (`work_ledger_delegation.go:321-360`). The root's
  executor context is cancelled too, and the runner treats "root context
  done" as `Stop(root)`. The same rule covers the root's ack-abort
  (`work_ledger.go:513-546`).
- Process shutdown: `workLedger.close` leaves rows running for the next host
  to recover (`work_ledger.go:976-997`). The runner must not persist
  Cancelled states on shutdown.

**Persistence and restart.** One new table, `job_batch_nodes`: `id` (PK, the
item id), `owner_session_id` (FK to sessions, cascade on delete),
`root_tool_call_id`, `parent_id`, `ordinal`, `kind`, `spec` (JSON), `mode`,
`on_fail`, `notify`, `max_parallel`, `state`, `child_session_id`,
`result_summary`, `created_at`, `updated_at`. The engine's `Store` writes
through on every state change. `async_jobs.kind` gains `'batch'` through a
table rebuild, following `internal/db/migrations/20261005000001_bg_shell_jobs.sql:17-75`.

After a host dies, the existing dead-host sweep turns the root row and the
leaf rows `interrupted`. The tree stays readable: readers derive "not started
(batch interrupted)" for Pending nodes whose root row is terminal. v1 does not
resume batches (open question 5).

## 4. Agent outcome: success or failure

Today a sub-agent that concludes "this cannot be done, the tests still fail"
ends its turn with ordinary text, so its delegation is reported as finished
(`coordinator_work_scope.go:99-110`, `coordinator_subagents.go:343-347`). A
batch cannot tell that apart from success.

Design:

- New tool **`task_outcome`**, offered to sub-agent builds only:
  `{"status": "success" | "failure", "summary": "..."}`. It returns its result
  with `StopTurn = true`, the existing turn-ending contract
  (`internal/agent/tools/tools.go:78-80`), so the declaration is the last act
  of the turn.
- Durable for free: the call and its result are in the child's history, the
  same way the question stop is read back from history
  (`question_stop.go:98-128`). There is no new table.
- Read back in `refreshSubAgentCompletion` (`coordinator_work_scope.go:52-113`),
  through a new helper in `coordinator_task_outcome.go`. Order: settle-by-failure
  first (unchanged), then a question (unchanged, still not terminal), then the
  newest `task_outcome` (Content = summary plus the last text,
  IsError = status is failure), then the existing last-text rule. The
  sync/SDK first-turn path in `runSubAgent` (`coordinator_subagents.go:343-347`)
  applies the same reader.
- The latest declaration wins: an agent resumed or injected later may
  re-declare.
- Undeclared outcome: for batch agent items it is **failed** by default
  ("the agent ended without task_outcome; its last text: …"); fail-closed,
  see open question 1. Plain `agent` delegations keep today's behaviour unless
  they declare.
- Batch agent items get a fixed prompt appendix that requires ending with
  `task_outcome`. The tool name is added to `allToolNames`
  (`internal/config/config.go:416`) and to the sub-agent tool set
  (`coordinator_tools.go:192`).
- The declared outcome reaches the engine as `Settled(Failed)` or
  `Settled(Completed)` when the delegation is released, so stop-on-fail and
  the summaries follow it.

## 5. Execution: mapping onto existing machinery

**Root (`batch_run`).** Added to `wrapAsyncTools`
(`async_tool.go:527`), so the root gets `Start`/claim, the ack gate, the
"started" response, and on completion a normal wake=1 notice through
`finalize` → `finish` (`async_tool.go:375-385`). Its inner `Run` constructs
the `Runner` and blocks until `Done`, returning the summary as the job result.

It is **never sync** — an explicit exception at `async_tool.go:71`. Web Drain
turns carry no origin and would otherwise take the sync branch (stated at
`internal/db/migrations/20261005000001_bg_shell_jobs.sql:4-6`), blocking a
Drain turn for the batch's whole life and running into the 45-minute tool
watchdog. Delivery routing does not depend on the origin flag
(`work_ledger.go:362-368`). SDK origin is refused in v1. `batch_run` accepts
the shared `timeout` parameter (`async_tool.go:451-519`); `terminate_and_wake`
on the root stops the whole tree.

**Leaves.** `Executor.Start` calls a new ledger entry point,
`startBatchLeaf`, in a new file `work_ledger_batch.go`. `work_ledger.go` is
already at 997 lines and must not grow. `startBatchLeaf`:

- applies the same per-session cap as `Start` (`work_ledger.go:271-277`) and
  maps `asyncCapError` to `StartFailed{Retryable}`;
- claims the row and marks it announced in one step, like `ClaimShell`
  (`internal/session/async_job_bgshell.go:50-82`), because a leaf has no
  "started" tool result to wait for;
- records the batch reference and a `quiet` flag on the job.

It then launches the existing executor unchanged: an
`&asyncTool{inner: itemTool, name: ...}` whose `run`/`finalize`
(`async_tool.go:296-385`) already handle bash backgrounding, the run_command
live output sink and delegation arming. For agent leaves, the
permission-inheritance block of `launchExecutor` (`async_tool.go:180-195`) is
extracted into a helper and shared, not copied. The context carries
`SessionIDContextKey`, plus `MessageIDContextKey` = the `batch_run` message
id that the agent tool requires (`agent_tool.go:59-67`,
`internal/agent/tools/tools.go:20-52`).

**Two ledger hooks**, both in `work_ledger_transition.go` (366 lines):

1. After `causeStateNoticeKindWake` (`:231`): a `quiet` leaf (its parent has
   NotifyAll) commits `delivery='done', wake=0, reacted=1` — the existing
   job_kill shape (`:107-109`). A NotifyEach leaf is unchanged: its own row is
   its per-item notice.
2. At the in-memory adoption (`:297`): only when `transitionToTerminal`
   returns true, i.e. once per job whichever cause won, call
   `Runner.Settle` after `l.mu` is released. `dropLocked` (gone or ABA) settles
   the leaf as Failed.

**Is a direct tool call outside a model turn possible today?** Yes. A tool is
`Run(ctx, ToolCall)` with the session/message ids in `ctx`, and the codebase
already runs tools off the turn goroutine (`async_tool.go:296`,
`turn_stall_tool_detach.go:15-24`). Policy must still hold:

- Item tools are taken from the same per-call filtered set the calling turn
  has, wrapped by restricted-run and hooks (`coordinator_tools.go:808-814`)
  but not by `asyncTool` or stall-detach. Agentguard runs inside bash and
  run_command themselves (`internal/agent/tools/bash.go:142`,
  `run_command.go:141`).
- A batch can only call tools its caller may call, so orchestrator mode's
  edit stripping (`coordinator_tools.go:194-208`) cannot be bypassed.
- Permissions: web sessions are auto-approved (`internal/server/handlers.go:164-175`);
  a `rush run` root is auto-approved and its children inherit
  (`coordinator_subagents.go:117-118`).

**MCP (phase 4)** goes through the existing `tools.Tool.Run` →
`Owner.RunTool` (`mcp-tools.go:132-184`). Stop cancels the item context,
which INV-19/INV-20 already define (`docs/mcp-invariants.md`). There is no
change in `internal/agent/tools/mcp`.

**Batch tools in sub-agents.** Batch tools exist only in top-level builds,
like `agent` (`coordinator_tools.go:627-641`). The delegation tree stays two
levels (`coordinator_tools.go:180-182`), and every agent item is a direct
child of the batch owner at any batch depth.

**Concurrency limits** (defaults, open question 3):

- `max_parallel` per batch: 4, hard maximum 16.
- Running agent leaves per process: 4.
- Running command leaves per process: 2 — the machine's memory ceiling, where
  two concurrent heavy builds/tests already hit `errno=1455` (CLAUDE.md).
- Tree size: depth ≤ 3, ≤ 100 nodes per tree.
- The per-session cap of 50 (`work_ledger.go:24`) still counts the root and
  every leaf; the engine queues against it instead of failing items.

**Ordering.** Sequential: item k+1 is claimed only after item k's terminal
transition committed. Parallel: starts in ordinal order, completion order
unspecified. Added items get the next ordinal; in a sequential batch they run
after the current queue. Notices are pulled oldest first
(`internal/session/notice_pull.go:105-108`).

**Timeouts.** Per item: the existing `TimeoutSpec` (`work_job.go:52-64`).
run_command keeps its own 600 s process cap (`run_command.go:43`), which is
why bash items (no such cap) are phase 4.

## 6. Notifications

**To the owning agent.**

- NotifyEach: each leaf's own row is a pending wake=1 notice. It wakes a
  Drain (web: `coordinator_background.go:89-91`) or hints the `rush run` loop,
  which reads debt through `CLIScope` (`coordinator_reaction_source.go:229-291`).
  Pending notices that land before a turn starts are pulled into that one
  turn (`agent_notice_pull.go:28`).
- A nested batch's own settle under a NotifyEach parent is a new
  `session_notices` kind, `batch_item`, bound to the root row through
  `job_tool_call_id`. Its void rule must be "root row voided by a Rerun", not
  wake_only's "job not running" (`notice_pull.go:266-281`) — otherwise it
  would void after the root completes.
- NotifyAll: only the root's terminal notice. In NotifyEach mode the root
  summary is short (counts and ids), so outputs are not repeated.
- Never suppressed by quiet mode: `child_question`
  (`work_ledger_delegation.go:116-120`), `timeout_wake_only`, supervision.

**To the operator.**

- `rush run`: the root row keeps the scope open
  (`coordinator_reaction_source.go:295`), and the heartbeat already names
  running rows, leaves included (`internal/app/app_run_async_wait.go:21-62`).
  One stderr line per settled item is phase 3.
- Web: leaf rows already appear in the live-work panel. A Batches tab is
  phase 5.

## 7. Operations

| Operation | Model (v1) | Operator CLI (phase 3) | Missing today |
|---|---|---|---|
| add item | `batch_add(batch_id, parent, items)` → `Runner.Add`; the owner is checked | `rush batch add` → control row | the tool; a cross-process channel |
| stop item | `job_kill(item id)`: a run_command leaf works as is (`work_ledger.go:621-625` → `:817`); agent leaf → `stop_agent(child)`; a batch node → new branch → `Runner.Stop`; or `batch_stop(id)` | `rush batch stop` → control row polled by the host | the batch-node branch; the control table |
| view output | `job_output(item id)` is live for run_command (`work_ledger.go:773-793`); `batch_status` returns state plus `result_summary` for settled and quiet items; `inspect_agent` / `read_delegation_transcript` for agents | `rush sessions jobs` (exists); `rush batch show` | `batch_status` |
| inject into an agent item | `inject_agent(child)`; ownership holds at any batch depth (`coordinator_agent_control.go:24-36`) | `rush sessions inject <child>` works today | an idle child whose delegation is held does not run on inject (`coordinator_agent_control.go:246-249`) |

Prerequisite (phase 0): `ResolveJobShellID` and the job-control methods live
in `work_ledger.go:586-875`. They move, as a pure move, into
`work_ledger_jobctl.go` before the batch branch is added there.

## 8. Risks and invariants

- **Deadlock, sequential batch ↔ agent.** A child can never wait on its
  parent's batch: children get no batch tools, and job addressing is
  owner-scoped (`work_ledger.go:596-604`). A child that asks a question holds
  its item, and the `child_question` notice wakes the owner even in quiet
  mode. The owner answers through `agent(resume_session_id)`
  (`async_tool.go:83-90`). `batch_*`, `job_*`, `wake*`, `agent`,
  `ask_question` and `task_outcome` are never allowed as tool items.
- **Lock order.** The runner's mutex is never held across ledger calls; the
  ledger calls the runner only after releasing `l.mu`.
- **Runaway fan-out.** Section 5's caps; `batch_add` is refused past them.
  NotifyEach on a large batch means many paid Drain turns. The 5-resume cap
  applies to background shells only (`coordinator.go:132`,
  `coordinator_bgshell_cap.go:102`), and the reaction-chain guard stops only
  sleep/echo chains (`coordinator_reaction_chain.go:27`). Hence the default
  is NotifyAll (open question 4).
- **Double delivery.** One notice source per fact: a leaf's row (NotifyEach),
  or nothing (quiet), plus exactly one root notice. A job_kill's answer is the
  tool result itself (delivery `done`). `Settle` fires once per job, at the
  first in-memory adoption, and is idempotent in the core (E1).
- **Turn-stall watchdog.** Add `batch_run` to `stallDetachExcludedTools`
  (`turn_stall_tool_detach.go:31-36`). Leaves run outside any turn, so no
  stall clock is involved.
- **Drain turns.** `batch_run` is never sync (section 5); `batch_add`,
  `batch_status` and `batch_stop` return immediately.
- **1000-line rule.** `work_ledger.go` is at 997 lines; new ledger code goes
  in new files only. `coordinator_tools.go` is at 826, so a handful of lines
  at most. No new file may exceed 1000 lines.
- **MCP.** Nothing in `internal/agent/tools/mcp` changes.
- **Review stop rule.** The acceptance tests in section 9 define "done".
  Only a reproducible P0/P1 reopens the work; P2/P3 go to the backlog.
  Interleaving speculation inside the engine is answered by the core's
  property test, not by new guards.

## 9. Phased plan

**Phase 0 — pure move.** `work_ledger.go:586-875` → `work_ledger_jobctl.go`.
Acceptance: the declaration diff from CLAUDE.md is empty, and the test-function
count is unchanged.

**Phase 1 — the engine, no rush wiring.** New files in `internal/jobbatch/`:
`spec.go`, `tree.go`, `tree_apply.go`, `runner.go`, `view.go` (each under 400
lines), plus `tree_test.go`, `runner_test.go` and `property_test.go`.

Acceptance:
- E1–E7 each covered by table tests;
- the runner passes with a fake executor under `-race`;
- the randomized property test holds for 10k sequences;
- an import test asserts the package imports nothing under `internal/`.

**Phase 2 — v1 in rush: model tools, command and agent leaves, task_outcome,
persistence.**

New files:
- `internal/db/migrations/20261007000001_job_batches.sql` (kind rebuild plus
  the new table), the sqlc queries and generated code;
- `internal/session/job_batch_store.go`;
- in `internal/agent/`: `work_ledger_batch.go`, `batch_adapter.go`
  (Executor/Sink/Store), `batch_tool.go`, `coordinator_task_outcome.go`;
- in `internal/agent/tools/`: `batch_control.go` with `batch_run.md`,
  `batch_add.md`, `batch_status.md`, `batch_stop.md`, and `task_outcome.go`
  with `task_outcome.md`.

Edits:
- `async_tool.go` (`:71`, `:452`, `:527`, the extracted inheritance helper);
- `work_ledger_transition.go` (`:231`, `:297`);
- `work_ledger_jobctl.go` (the batch-node branch);
- `coordinator_tools.go`, `coordinator_work_scope.go`,
  `coordinator_subagents.go`, `turn_stall_tool_detach.go:31`,
  `internal/config/config.go:416`.

Acceptance tests (each with a revert-check):

- **A1.** A parallel batch of 3 run_command items, one exiting 1, NotifyAll:
  exactly one notice, which reports 2 completed and 1 failed; the leaf rows
  are `delivery='done'`.
- **A2.** Sequential stop-on-fail where item 2 fails: item 3 never claims a
  row and ends Skipped.
- **A3.** Sequential continue: all items run, in order of the leaf rows'
  creation.
- **A4.** NotifyEach: 3 leaf notices plus 1 root summary, and nothing else.
- **A5.** `job_kill(item id)` on a running leaf: the item is Cancelled and the
  sequential batch moves to the next item.
- **A6.** `batch_stop(root)` while an agent leaf runs: the delegation is
  cancelled, Pending items are Cancelled, one root notice is delivered.
- **A7.** An agent that calls `task_outcome(failure)`: the item is Failed and
  the batch stops. An agent that declares nothing: the item is Failed with the
  "no task_outcome" text.
- **A8.** `batch_add` to a running sequential batch: the new item runs last.
  `batch_add` to a settled batch is refused.
- **A9.** `rush run` end to end: the run stays open while the batch runs and
  exits after the root notice has been reacted to.
- **A10.** `batch_run` in an origin-less (web Drain) turn returns "started"
  immediately.
- **A11.** A simulated host crash: the root and leaf rows end `interrupted`;
  `batch_status` shows Pending nodes as not started.
- **A12.** A parallel batch of 60 items with `max_parallel` 16: the 50 cap is
  never exceeded and no item fails for capacity.

**Phase 3 — operator CLI.** Control table `job_batch_controls` (add/stop
requests), consumed by the host runner on its hint wait or a 2 s tick. This is
the same pattern as the cancel flag (`internal/session/session_update.go:311-320`)
and pending injects. Commands: `rush batch show|add|stop`, plus stderr lines
for settled items in `rush run`. Acceptance: a stop issued from a second
process stops a leaf within 5 s; `show` matches `batch_status`.

**Phase 4 — more item kinds:** bash, generic tool, MCP. Adds
`async_jobs.kind 'tool'` (a table rebuild) and the tool-item denylist.
Acceptance: an MCP item stopped mid-call settles Cancelled, and the server's
session survives (INV-19).

**Phase 5 — web.** A Batches tab in `web/src/components/LiveWorkPanel.tsx`,
with stop/add through the phase-3 control table. Playwright e2e.

## 10. Open questions for the operator

1. **An agent item that never declares `task_outcome`: failed or completed?**
   Recommended: failed (fail-closed). Plain `agent` delegations are unchanged.
2. **Does a Cancelled item (stopped by the model or operator) trigger
   stop-on-fail?** Recommended: no — to halt the batch, stop the batch.
3. **Concurrency defaults.** Recommended: `max_parallel` 4 (hard 16), command
   leaves 2 per process, agent leaves 4 per process.
4. **Default notification mode.** Recommended: notify only when all items
   have stopped, since every per-item notice can cost a paid model turn.
5. **Resume a batch after a process restart?** Recommended: not in v1; the
   tree stays readable, interrupted items are reported, and the model or the
   operator re-adds them.
6. **Operator-created batches with no model** (`rush batch run spec.json`, the
   same engine with a stderr Sink)? Recommended: yes, after phase 3; the
   engine's design already allows it.
