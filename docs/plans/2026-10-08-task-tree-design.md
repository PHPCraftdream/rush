# Task tree — autonomous planning core and later Rush integration

Status: autonomous A0-A2 implemented and exercised; A3-A5 remain a separate Rush
integration gate. The design originated on 2026-10-08. Operator authorization
for this implementation used `/wcrush`; existing Rush `todos`, WebUI, DB, and
job-batch launch behavior are unchanged.

## 0. Decisions and scope

- No Rush pre-refactoring is required to develop and run the autonomous code.
- Before integration, replace the three existing todo writers with one versioned
  mutation service. Do not first move their obsolete full-list merge algorithms
  into a new shared helper.
- Use an ordered, nested task tree with stable IDs. Groups organize work; leaves
  describe work. This is a planning board, not an execution scheduler.
- Agents use one `tasks` tool with short incremental operations. An omitted task
  is never deleted, and renaming a task never changes its identity.
- Keep the domain, in-memory store, agent protocol, and development executable
  independent of Rush's app, sessions, SQLite, Fantasy, providers, and MCP.
- Use the same package in the development executable and eventual Rush adapters.
  No second implementation, isolated module, or copied model of the rules.
- The autonomous implementation was written in four isolated parallel Rush
  sessions after a shared-contract freeze, then verified by the orchestrator.

The first delivery includes real state transitions, atomic mutations,
idempotent retries, a working neutral agent API, serialization, and standalone
runs. It excludes DB migrations, Rush tool registration, reminders that invoke
models, UI changes, and binding to job batches.

Not in scope: task deadlines, priorities, dependencies/DAGs, execution attempts,
scheduling limits, automatic retries of work, natural-language outcome parsing,
multiple simultaneous focus tasks, or a generic workflow framework. Parallel
implementation of this component does not require parallel execution semantics
inside a planning board.

## 1. Evidence and pre-refactoring assessment

### 1.1 What OMP actually implements

The inspected OMP implementation has **phases containing tasks**, not an
arbitrary-depth domain tree:

- `D:/dev/go/oh-my-pi/packages/coding-agent/src/tools/todo.ts`: incremental
  `init/start/done/rm/drop/block/unblock/append/view`; exact-content selectors;
  duplicate-content rejection; focus normalization; recoverable tool errors;
  successful mutations expose canonical phases in tool-result details.
- `packages/coding-agent/src/session/todo-tracker.ts`: branch rehydration and
  bounded host reminders, separate from the transition functions.
- `packages/tui/src/tools/todo.ts`: five leaf statuses and presentation. Its
  heuristic matching of pending task text to running agents is a UI hint,
  not canonical task identity or completion evidence.

A direct smoke of the installed OMP `applyOpsToPhases` function, whose source
matched the inspected clone, produced these transitions:

1. Initialize A/B: A becomes active, B pending.
2. Block A: A blocked, B active.
3. Complete B: A blocked, B completed, no active task.
4. Unblock A: OMP immediately makes A active.

Borrow incremental operations, ordered advancement, blocker visibility, and
bounded host reminders. Deliberately change identity to stable IDs and support
nested groups. For Rush, **unblock returns pending without starting work**;
section 3 defines the exact focus policy rather than copying OMP normalization.
The smoke did not exercise OMP UI, session persistence, or model calls.

### 1.2 Rush integration seams

| Existing surface | Observed behavior | Integration change |
| --- | --- | --- |
| `internal/agent/tools/todos.go` | Full-list replacement; content identity; forward status ranking; operator-deletion filtering | Thin `tasks` adapter over the shared API |
| `internal/agent/cliprovider/mcpserver_tools.go` | Separate `mergeMCPTodos`; forward ranking, but no native-tool tombstone filtering | Same API and policy as the native adapter |
| `internal/server/handlers_sessions.go` | Operator full-list replacement and content tombstones | Versioned operator commands |
| `internal/session/session_update.go` | `SetTodos` narrows the SQL write, but has no compare-and-swap revision | Atomic task snapshot and receipt storage |
| `internal/agent/agent_prompt.go`, `agent_compaction.go` | Current flat list injected into prompts and summaries | Current IDs, paths, statuses, blockers, revision |
| `internal/app/app_run_async_nudge.go` | Non-completed counting and bounded reminder budget keyed by content/status | Explicit actionable facts; no nudges for blocked-only work |
| `internal/session/session_fork.go` | Copies current todos and tombstones in the fork transaction | Transactionally clone the canonical tree into the child scope |
| `web/src/components/TodoList.tsx`, `web/src/store.ts` | Whole-list optimistic edits, index keys, status cycle including operator reopening | Stable-ID commands, revision checks, explicit reopen |

LSP found the native tool, MCP bridge, and WebSocket handler as the production
callers of `session.Service.SetTodos`. [INFERENCE] Their Get/merge/Set sequence
permits a concurrent task writer to overwrite another task writer even though
unrelated session columns are protected. No Rush concurrency failure was
reproduced during this design investigation.

### 1.3 Required versus unnecessary pre-refactoring

**Before autonomous implementation: none.** Add a self-contained package and
lab without changing any of the surfaces above.

**Before integrated cutover:** establish one persistence adapter and one current
view projection; route all three writers through the mutation service; migrate
prompt, compaction, reminder, session lifecycle, and UI consumers. These are
integration prerequisites, not separate rewrites of the coordinator or app.

Do not refactor the work ledger, agent admission, MCP connection management,
provider configuration, or generic tool construction for this feature. Those
are not task-tree ownership boundaries. Do not create an intermediate shared
full-list `mergeTodos` convention that the cutover immediately discards.

## 2. Package boundary and ownership

Planned layout, not files already implemented:

```text
internal/tasktree/
  types.go                    Shared domain DTOs, enums, limits, errors.
  contract.go                 Store/service DTOs and interfaces.
  tree.go                     Owned state, indexing, transition validation.
  transition.go               Incremental mutation rules.
  view.go                     Read-only views, progress, attention facts.
  snapshot.go                 Export/import and invariant validation.
  service.go                  Revision, receipts, atomic commit orchestration.
  memory/store.go             Independent concurrent in-memory Store.
  protocol/                   Decode, agent schema, results, text rendering.
  lab/                        Scenario runner, JSONL session, fixture oracles.
  testdata/                   Domain boundary fixtures.
cmd/tasklab/                   Stdlib CLI; no Rush initialization.
```

The domain imports only the standard library. `memory` and `protocol` import
`tasktree`; `lab` consumes these packages; `cmd/tasklab` consumes `lab`.
`tasktree` must not import its consumers. The existing Go module is retained,
but running these packages must not initialize unrelated Rush subsystems.

The kernel owns transition validity and focus. The service owns optimistic
concurrency and receipt semantics. Store owns atomic publication. Protocol
owns transport validation and presentation, not a second transition engine.
The host owns current-tree selection, caller authority, and reminder policy.

No clocks, goroutines, file I/O, model SDKs, or process launch in the kernel.
Only the store needs synchronization. File I/O belongs to the lab or later
persistence adapter. Import checks are development commands, not source-text
unit tests.

## 3. Domain contract

### 3.1 Nodes and identity

A tree has an implicit root group `n0`. A group has an ordered child-ID list and
no writable task status. A task is a leaf with title, optional `active_form`,
status, and a reason when blocked or abandoned. Groups may nest; tasks may not
have children. Groups have no active form or reason. A task reason is required
only for blocked/abandoned and is empty in other states. Empty groups are valid
structural nodes, not completed work.

Node IDs are generated by the kernel: `n` plus a monotonically increasing
base-36 counter, starting at `n1`. Failed mutations do not consume counters.
IDs are never reused, including after removal. The identity is `(TreeKey,
NodeID)`; the same short ID in a different tree is a different node. Actors
cannot supply IDs for newly created nodes.

Titles are labels, not keys. Preserve their text; validate nonblank content and
caller-supplied byte limits. Duplicate titles are allowed, including in separate
phases. A text selector uses exact stored text, optionally within one group;
zero matches and multiple matches are errors. Ambiguity returns candidate IDs
and paths. No first-match choice, case folding, substring matching, or fuzzy
agent-to-task binding. IDs are the preferred mutation selectors.

Serialized nodes contain ID, parent ID, kind, title, optional active form,
task-only state/reason, and group-only child IDs. Import validates that the
parent and child indexes agree: one parent per live non-root node, every child
listed exactly once, no cycles, no orphans, no unknown IDs, and no reused IDs.
The kernel may cache indexes internally; exported state is detached.

### 3.2 Leaf states and progress

Statuses remain familiar: `pending`, `in_progress`, `completed`, `blocked`,
`abandoned`. Completed and abandoned are closed, but abandoned is not success.
Blocked remains unfinished but is not actionable.

Progress counts **task leaves once**, never groups:

- `actionable = pending + in_progress`;
- `unfinished = pending + in_progress + blocked`;
- `settled = completed + abandoned`;
- `all_settled = total > 0 && unfinished == 0`;
- `all_completed = total > 0 && completed == total`.

A tree/group with no leaves has both completion flags false. Derived group
progress is immediately correct even if no descendant was ever active. Adding
a task under a previously settled group is allowed: its aggregate changes,
but its previously completed/abandoned leaves do not reopen.

There is at most one `in_progress` leaf in a tree. It is the planning focus,
not a count of running commands or delegated agents. Child sessions may have
independent trees. A worker must not mutate its parent's tree merely by knowing
an ID; host binding determines the scope.

### 3.3 Focus rules

Order means depth-first traversal in stored sibling order. Retain an existing
focus unless the operation explicitly changes or closes/removes it.

- `init`: select the earliest pending leaf, if any.
- `add`: if it adds pending leaves and there is no focus, select the earliest
  pending leaf in the resulting tree.
- `start`: require a pending/active task; demote another active task to pending,
  then activate the target. This is the only ordinary explicit focus switch.
- `done/drop/block/rm`: if the operation actually closes, blocks, or removes
  the active leaf, select the earliest remaining pending leaf.
- `unblock` and operator `reopen`: return affected leaves to pending; do not
  start them or change another task's focus.
- `edit/move`, read-only `view`, failed requests, and semantic no-ops do not
  select a focus. A read never repairs or normalizes stored state.

Thus pending work with no active task is valid after unblock/reopen. `next_id`
is the active ID if present, otherwise the earliest pending ID, otherwise empty.
Finishing a later task can advance back to an earlier pending phase; completed
leaves are never implicitly reopened. Starting a blocked task requires unblock.

### 3.4 Operations and authority

| Operation | Agent contract | Effect |
| --- | --- | --- |
| `view` | Optional node selector; defaults to root | Read without normalization, receipt, or revision change |
| `init` | Nonempty drafts/items; no existing initialized tree | Create the board; never replace an existing board |
| `add` | Nonempty drafts/items; group parent, root by default | Append ordered tasks/groups; no omission-based deletion |
| `start` | One task | Explicit focus selection |
| `done` | One task | Pending/active/blocked to completed; completed is a no-op; abandoned is an error |
| `block` | Task/group plus nonblank reason | Open descendant leaves to blocked; preserve terminal leaves |
| `unblock` | Task/group | Blocked descendant leaves to pending; clear blocker |
| `drop` | Task/group plus nonblank reason | Open descendant leaves to abandoned; retain them visibly; preserve terminal leaves in groups |
| `edit` | One node; title and/or task active form | Change labels, never identity/status |
| `move` | One non-root node; group parent; optional `before_id` | Reparent/reorder; no cycles or status changes |
| `rm` | Operator only; one selector or a nonempty `ids` list | Remove selected subtrees atomically; keep tombstones |
| `reopen` | Operator only; one completed/abandoned task | Explicit return to pending; never automatically focus |

Dropping an already abandoned task is a no-op; dropping a completed task is an
error. Group block/unblock/drop act on the eligible leaves present **now**;
there is no persistent blocked/abandoned latch on the group. Later additions
start pending. A bulk operation with no eligible leaves is a no-op.
Blocking a completed/abandoned leaf is an invalid transition; unblocking a
nonblocked leaf is a no-op. An edit with no changed fields is a no-op. Before
init, only view/init are valid; other operations return `uninitialized`.


Root may be viewed, receive additions, or be the explicit block/unblock/drop
scope. Root cannot be edited, moved, removed, completed, or reopened. A move
cannot target itself as parent or insertion anchor. `before_id` must be a direct
child of the destination group. For multi-ID removal, reject duplicate IDs,
validate all targets first, and collapse ancestor/descendant selections so each
node is removed once.

Agent removal is intentionally narrower than OMP: agents use `drop(reason)` for
abandoned work rather than making unfinished requested items disappear. Only
operators delete or reopen. Operator commands use the same kernel/service,
not another reducer. Actor kind and identity come from trusted host context,
never from model JSON. Authentication itself remains a host responsibility.

### 3.5 Deletion protection and limits

Keep ID tombstones. A stale removed ID returns `removed`, not `not_found`, and
can never address a later task. Preserve Rush's existing operator-deletion
protection: operator removal also sets an exact `(kind,title)` guard across the
board. Agent add or rename-to-that-title fails with `removed_by_operator`;
never silently filter the requested node. Existing live duplicate-title nodes
are not deleted or forbidden from progressing. Operator add/rename explicitly
using that guarded label clears its title guard, but receives/retains a live ID;
removed IDs are not resurrected. Legacy `DeletedTodos` become task-title guards.

Kernel/service limits are positive caller-supplied values: maximum live nodes
(including root), depth (root is depth zero), title/reason bytes, tombstones,
and receipts. Limit violations reject the entire request. No silent truncation,
automatic receipt eviction, or ID recycling. A host can archive a board and
select a new **TreeKey** when its retained-history limit is reached; it must not
clear a live board behind the agent's back. Initial agent API has no reset or
replace operation. A session can append another phase after settling old work.
Default numeric limits are integration policy; lab fixtures supply explicit
limits and exercise exact boundaries.

## 4. Atomic service and persistence contract

### 4.1 Shared types to freeze at A0

Shared domain/service declarations live in `types.go` and `contract.go`;
protocol/lab boundary DTOs live in their frozen `types.go` files. Fix full fields,
JSON shapes, and error codes before parallel work:

- Scalar types: `TreeKey`, `NodeID`, `RequestID`, `Revision`; enums for kind,
  status, operation, actor kind, and problem code.
- `Actor`: trusted kind and host identity.
- `Selector`: exactly one of ID or exact text, with optional group scope for
  text. `Draft`: kind, title, active form, recursive children; no supplied IDs
  or statuses. `Command`: operation and its typed selector/drafts/edit/move/
  removal fields. It contains no Rush session, job, or executor payload.
- `Snapshot`: schema version, initialized flag, root ID, next-ID counter, nodes,
  tombstones, and title guards. Task/group field constraints are explicit.
- `Delta`: created, updated, removed IDs, and completed-transition IDs. Created,
  updated, and removed sets are disjoint; completed is a subset of updated.
- `Progress`, `Summary`, `NodeView`, `View`: revision, scope, ordered structure,
  active/next IDs, separate counters and completion flags. Groups have progress,
  not an invented leaf status. `Summary` is a small projection without a tree.
- `Receipt`: request ID, actor identity/kind, canonical payload fingerprint,
  committed revision, and delta. No full snapshot per receipt.
- `Envelope`: current revision, snapshot, and receipt map. Missing board is
  revision zero with no receipts; stored envelopes have positive revisions.
- `Problem`: code, message, expected/current revisions when relevant, target
  candidates when relevant. `MutationReply`: receipt, replayed flag, current
  summary, and created-node briefs from that same current revision. A brief
  contains ID, parent ID, kind, title, task status/reason, or a removed marker;
  it has no recursive children. Domain problems and infrastructure errors
  remain distinct.

The public API signatures below are documentation, **not forward declarations
or placeholder Go bodies**:

```text
EmptySnapshot() Snapshot
Restore(snapshot Snapshot, limits Limits) (*Tree, error)
(*Tree).Apply(actor Actor, command Command) (Delta, error)
(*Tree).Snapshot() Snapshot
(*Tree).View(target Selector) (View, error)
(*Tree).Summary() Summary

NewService(store Store, limits Limits) *Service
CheckEnvelope(envelope Envelope, limits Limits) error
(*Service).Summary(ctx context.Context, key TreeKey) (Summary, error)
(*Service).View(ctx context.Context, key TreeKey, target Selector) (View, error)
(*Service).Mutate(ctx context.Context, key TreeKey, actor Actor,
    requestID RequestID, expected Revision, command Command) (MutationReply, error)

Store.Load(ctx context.Context, key TreeKey) (Envelope, error)
Store.Commit(ctx context.Context, key TreeKey, expected Revision,
    candidate Envelope) error
memory.NewStore(limits Limits, seeds map[TreeKey]Envelope) (*memory.Store, error)

protocol.New(backend protocol.Backend) *protocol.API
(*protocol.API).Definition(actorKind ActorKind) protocol.Definition
(*protocol.API).Execute(ctx context.Context, invocation protocol.Invocation,
    payload json.RawMessage) (protocol.ToolResult, error)

lab.Run(ctx context.Context, scenario lab.Scenario,
    loaded *lab.Checkpoint) (lab.Report, error)
lab.ReadCheckpoint(reader io.Reader) (lab.Checkpoint, error)
lab.WriteCheckpoint(writer io.Writer, checkpoint lab.Checkpoint) error
```

The service stamps revisions on domain views/summaries; the kernel does not
increment the storage revision. EmptySnapshot has schema version 1,
initialized=false, root `n0` (group titled `Tasks`, no parent/children), next
counter 1, only that root in its node map, and empty tombstones/title guards.
Missing Store.Load returns this snapshot with revision 0 and no receipts.
Empty selector means root only for view. A text selector with within_id searches
that group's complete subtree, including the group itself; absent within_id
searches the whole board. View contains the selected subtree and its progress
plus the board-wide summary; focus/next are board-wide.

Memory seeds are optional, validated with CheckEnvelope, and detached before
publication. The service owner implements CheckEnvelope; the kernel owner
implements EmptySnapshot/Restore. Protocol owns Invocation, Definition (name,
description, input schema), ToolResult/details, and its API body; lab owns
Scenario, Checkpoint, Report, and its bodies. A0 creates these real shared DTO
declarations in `protocol/types.go` and `lab/types.go`, then freezes them; A1
owns their remaining directories. No downstream agent infers a missing type.

### 4.2 Mutation algorithm and retry semantics

For one command, in this order:

1. Validate trusted context and decode/validate the operation.
2. Load one detached envelope from the store.
3. Look up the request receipt **before** checking expected revision. Matching
   actor and canonical payload return the original receipt with `replayed=true`
   and the current summary, without reapplying the action. A reused key with a
   different actor/payload returns `request_reused`.
4. Check expected revision. A mismatch returns `conflict` with current revision
   and actionable summary. Do not silently rebase intent onto a new snapshot.
5. Apply the command to privately owned state. Validate the complete mutation,
   including group targets and limits, before publishing any changes.
6. Build candidate revision `expected + 1` and its receipt. Store commits state,
   revision, and receipt together under compare-and-swap.
   Reject revision or ID-counter exhaustion; never wrap to zero/reuse IDs.
7. Only after successful commit return success and let the host publish hints.

Every accepted **new mutating request**, including a semantic no-op, advances
revision once: a new receipt is itself durable state. Replaying the same request
and viewing do not advance it. No-op replies have an empty transition delta;
replay never emits another completed transition or duplicate host notification.

Fingerprint the canonical decoded operation **and expected revision**. JSON
whitespace/property ordering must not change it. An exact transport retry uses
the same host request ID and payload. After an explicit conflict, reread the
board and issue a newly considered command with a new request ID. Do not reuse
an old key with a changed expected revision.

Request IDs are supplied by the host, not the model: they identify a logical
invocation, scoped to the board and actor, and survive a transport retry. A
bare MCP numeric request ID or a provider tool-call ID that can be reused in
another conversation is insufficient without its invocation namespace.

Concurrent loads may see the same revision; exactly one conflicting commit
wins. Losers return a conflict, never partial state or IDs from an uncommitted
creation. If both deliveries were the same invocation, a loser may reread only
to discover that exact committed receipt; this is reconciliation, not a silent
re-execution against newer state.

Cancellation before publication has no effect. A successful durable commit is
not reported as rolled back merely because context cancellation arrives later.
A store unable to determine whether its commit occurred returns a distinct
`commit_unknown` infrastructure error: reconcile with the same request ID,
never retry under a fresh key or report a definite failure. The memory store
has no ambiguous commit result.

### 4.3 Store, serialization, and ownership

`Load` returns a detached envelope. `Commit` must atomically validate expected
revision, require candidate revision `expected + 1`, reject malformed candidates,
and publish a detached candidate; no mutable alias may escape through caller
slices/maps or returned views. Missing board can be created only by
compare-and-swap against revision zero. A definite store failure leaves the old
envelope and receipt set untouched; commit_unknown preserves uncertainty.

Snapshot export/import is real serialization with a schema version. Restore
rejects unsupported schema versions, broken ownership/order, invalid field
combinations, duplicate/malformed IDs, counters that permit ID reuse, multiple
active leaves, and invalid reasons/limits. Envelope import additionally checks
receipt revisions, actor/payload data, and envelope consistency. Loading must
not repair or normalize corrupt persisted state.

Bounded snapshot-CAS copying is acceptable at ownership boundaries; do not copy
again merely to render the same state. Do not retain a full tree for every
receipt, rescan the tree once per descendant in a bulk operation, or render the
whole board on every mutation. Mutable caches are private and invalidated by
structural changes. Pure replay and terminal-board reads remain available.

## 5. Agent-facing API

Neutral protocol provides `Execute(ctx, bound Invocation, JSON payload)` and an
agent schema/description without Fantasy/MCP imports. `Invocation` holds the
trusted TreeKey, actor, and stable request ID. Protocol validation errors are
correctable tool results, not successful no-ops or fatal process failures.
Infrastructure errors remain ordinary wrapped Go errors.

All mutations require `expected_revision`; initial empty view reports zero.
Responses always expose the current revision so the next command is compact.
For a correctable decode error, read the bound board's summary to supply that
revision. If this read fails, return the infrastructure error rather than
inventing a revision. Created-node briefs and summary share one snapshot;
protocol does not perform a later independent read per created ID.
`init/add` accept **exactly one** of recursive `list` or shorthand `items`.
Shorthand strings become task drafts. Mutations select one `id` or exact `text`
when required; text may additionally provide `within_id`. Init/add have no
existing-node selector; add uses `parent_id`, defaulting to root.

Examples, in sequence, on a new board:

```json
{"op":"init","expected_revision":0,"list":[{"kind":"group","title":"Design","children":[{"kind":"task","title":"Assess pre-refactoring"},{"kind":"task","title":"Freeze independent API"}]}]}
```

Creates group `n1`, active task `n2`, pending task `n3`; returns revision 1 and
the ordered created-ID list.

```json
{"op":"done","id":"n2","expected_revision":1}
```

Completes `n2`, activates `n3`, returns revision 2.

```json
{"op":"block","id":"n3","reason":"Waiting for operator decision","expected_revision":2}
```

Returns revision 3 with blocked=1, actionable=0, no active/next task. This is
unfinished work, not permission to report everything completed.

```json
{"op":"unblock","id":"n3","expected_revision":3}
```

Returns revision 4, `n3` pending, no active task, next=`n3`.

```json
{"op":"start","id":"n3","expected_revision":4}
```

Returns revision 5 with `n3` active.

```json
{"op":"add","parent_id":"n0","items":["Implement autonomous core","Run tasklab scenarios"],"expected_revision":5}
```

Creates `n4/n5`, retains focus `n3`, returns revision 6.

```json
{"op":"view","id":"n0"}
```

Returns the current ordered tree without a write. Rename uses `edit` with
`title` and/or task `active_form`; move uses `parent_id` and optional `before_id`.
Operation-specific unknown/irrelevant fields, conflicting selectors, and wrong
node kinds are errors. Model schema does not expose actor, TreeKey, request ID,
`rm`, or `reopen`. Operator protocol may expose the last two, but service
authority checks remain mandatory.

A0 freezes `ToolResult` as text plus structured details and an error flag:
mutation details contain receipt, replayed flag, current summary, and brief
current views for created IDs; `view` contains the requested subtree. Removed
created IDs on an old receipt replay are reported as removed, not reconstructed
as live. Text shows IDs, current focus/path, separate completed/blocked/
abandoned counts, and the next useful operation. Full tree output is requested
through view rather than repeated after every status change.

`conflict`, `ambiguous_target`, `removed`, `removed_by_operator`,
`invalid_transition`, `invalid_target_kind`, `limit_exceeded`, `uninitialized`,
`already_initialized`, `request_reused`, `forbidden`, and malformed input have
concrete corrective messages. Invalid requests leave the published tree unchanged.
Keep schema and decoder operation descriptions in one protocol definition;
state-transition validity remains the kernel's responsibility.

## 6. Standalone development and acceptance

### 6.1 tasklab

The lab consumes the real service and neutral protocol. Available commands:

```text
go run ./cmd/tasklab run ./internal/tasktree/lab/testdata/basic.json
go run ./cmd/tasklab run ./internal/tasktree/lab/testdata/retries.json
go run ./cmd/tasklab run ./internal/tasktree/lab/testdata/operator-delete.json
go run ./cmd/tasklab repl --tree-key lab
go run ./cmd/tasklab inspect ./board.snapshot.json
```

Runtime acceptance exercised a compiled `tasklab` executable on these paths.
`run` executes a bounded JSON scenario; `repl` accepts JSONL commands on stdin
against one in-memory board. Both exercise the same protocol used by adapters.
The executable never loads rush.json, opens the Rush DB, starts LSP/MCP, calls a
model, or launches work. An input fixture is not a fake execution load.

Scenario schema version 1 has `tree_key`, explicit `limits`, and ordered `steps`.
Each step has a unique `label`, trusted lab `actor` (kind/ID), `request_id`,
`payload` (the actual protocol JSON), and `expect`. Expectation fields identify
error flag/code, current and receipt revisions, replayed flag, active/next IDs,
progress counters, selected live node fields, and removed/absent IDs. Core
fixtures may pin generated IDs because numbering is deterministic. No oracle
is generated from the implementation under test. Repeating a request ID is
allowed only in explicitly named replay/reuse steps; no random tasks, sleeps,
or timing-based concurrency expectations.

REPL lines are `{request_id,payload}` wrappers, not model-tool payloads with
extra hidden fields. Tree binding is fixed for the REPL; actor defaults to
agent and may be selected explicitly with `--actor operator` for local probes.

`run --snapshot <path>` exports a versioned Checkpoint containing TreeKey,
limits, and the full validated envelope, including receipts. `inspect`
validates and prints it. `run --load <path>` resumes from it using memory-store
seeds, not a fabricated sequence of old commits. Loaded key/limits must match
the scenario. Replay a previous request without duplicating nodes, then accept
a new request: this proves restart behavior without SQLite/transcript recovery.
Malformed or expectation-mismatching scenarios exit nonzero. Help/usage errors
exit 2; scenario/validation failure exits 1; fulfilled expectations exit 0,
including a scenario intentionally expecting a correctable tool error.

### 6.2 Behavioral acceptance matrix

| ID | Required observable result | Owning slice |
| --- | --- | --- |
| T1 | Nested group ordering; stable IDs across rename/move; duplicate text is ambiguous without scope/ID | Kernel |
| T2 | Init/advance/focus switch; later completion can return to earlier pending work without reopening terminal tasks | Kernel |
| T3 | Block active advances outside the blocked subtree; unblock leaves pending even when no focus exists | Kernel |
| T4 | Mixed groups preserve terminal descendants; blocked-only remains unfinished; empty groups are not successful work | Kernel |
| T5 | All-abandoned versus all-completed flags differ; adding to a settled group does not reopen old leaves | Kernel |
| T6 | Cyclic/self moves, invalid bulk targets, exact limit boundaries, and invalid transitions leave state unchanged | Kernel |
| T7 | Two writers coordinated at the same revision: one commit, one conflict; unrelated accepted updates never disappear | Service/store |
| T8 | Exact replay before stale-revision check preserves original IDs/revision; key reuse rejects changed payload/actor; semantic no-op adds one receipt/revision | Service/store |
| T9 | Failed/cancelled-before-commit writes leave no receipt/state; after-commit cancellation does not invent rollback | Service/store |
| T10 | Operator delete blocks stale IDs and label resurrection; explicit operator re-add uses a new ID; operator reopen is distinct from agent completion | Kernel + service |
| T11 | Export/import preserves ordering, blockers, counter, guards, and replay receipts; mutation of caller-owned data cannot corrupt stored state | Kernel + service |
| T12 | Actual protocol commands produce the documented focus/count/error transitions; malformed/ambiguous input cannot partially create a tree | Protocol |
| T13 | Basic, blocked-only, nested/reordered, retry/reuse, operator-delete/reopen, exact-boundary, and export/resume fixtures run through tasklab with explicit expected outcomes | Lab |

Concurrency tests use barriers/channels around loads and commits, not sleeps.
Use real memory storage for its guarantees and a fault-injection store only for
transaction outcomes unavailable from memory storage. Those tests assert
consumer-visible state/receipts, not mock forwarding. Codec tests include a
plausible corrupt snapshot or retry boundary, not mere nonempty JSON or schema
wiring. Do not pin rendered wording, enum source text, imports, or copies.

After all implementations are handed off, the integration owner runs focused
tests for these packages, the race detector where supported, and the real
`tasklab` commands above. On Windows, run an additional export/load/inspect
scenario and show the same receipt, blocked reason, ordering, and next-ID
continuity after reload. Unsupported race tooling must be reported, not called
a pass. No provider credentials, external servers, or full-app test boot are
required. Dependency isolation is checked with `go list -deps` during acceptance,
not by committing a test of import-source text.

## 7. Parallel implementation stages

### A0 — shared contract, one owner

Create only real enums, DTOs, error types, and Store interfaces in `types.go`,
`contract.go`, `protocol/types.go`, and `lab/types.go`; fix the operation field
matrix, protocol signatures, snapshot/scenario formats, limits, ownership, and
all T1-T13 oracles in this plan.
Use documented function signatures for methods whose bodies belong to A1.
Do not add forward-declared Go functions, fake concrete types, panic methods,
TODO bodies, no-op executors, or pretend implementations to make agents compile.

Freeze the exact file ownership and exported names. Shared-contract changes
require the integration owner's update and notification before dependent edits.
Unresolved compilation between parallel handoffs is not solved with stubs.
This stage does not refactor Rush or register the new tool.

### A1 — four parallel implementation slices

| Slice | Exclusive files | Implementation and handoff |
| --- | --- | --- |
| Kernel | `tree.go`, `transition.go`, `view.go`, `snapshot.go`, corresponding `kernel_*_test.go`, domain testdata | Real transition engine, restore/export, aggregate and focus semantics; T1-T6 and kernel portions of T10-T11 |
| Service/storage | `service.go`, `service_*_test.go`, `memory/` | Real CAS/receipt orchestration and concurrent memory Store; T7-T11 |
| Agent protocol | `protocol/` | Real decoder, schema, corrective errors, structured results, concise renderer; T12 |
| Development lab | `lab/`, `cmd/tasklab/` | Real CLI, scenario/repl, export/load/inspect, explicit fixtures/oracles; T13 |

Each agent gets the whole user goal, this frozen contract, its owned files,
non-goals, and acceptance IDs. No agent edits another slice or the shared
contract. Kernel/service root-package test helpers use `kernelTest*`/
`serviceTest*` prefixes; store tests importing a consumer use an external test
package to avoid cycles. Protocol and lab have their own package namespaces.
One owner retains shared contracts, this document, integration fixes, and
cross-slice acceptance.

During `/wcrush`, agents wrote code/tests without executing them. Once real
dependencies were synchronized, the same sessions resumed for static commands.
Behavior tests, race checks, mutation overlays, and CLI acceptance were run
sequentially by the integration owner. No temporary production stubs were used.
Future implementation launches still require explicit operator authorization
and must use the operator's selected registered agents when names are given.

### A2 — joint integration and autonomous acceptance

Resolve real symbol/import mismatches without compatibility shims; format
written Go code using repository conventions; run focused tests and available
race checks; execute the actual lab paths, not only tests. Prove the API changes
the canonical state and preserves it through export/reload. Update this plan's
status/evidence and user-facing help/docs after the smoke. Remove temporary
probes; retain behavioral regressions and useful lab fixtures.

**The autonomous deliverable is complete only after T1-T13 and direct CLI
proof.** A compilable type package, mocked API, or missing persistence/replay
behavior is not an acceptable intermediate delivery.

### A3 — later Rush integration prerequisite, one owner

Reinspect the then-current repository. Add a concrete SQLite Store using a
canonical `session_task_trees` table (session key, revision, snapshot, receipts)
so tree commits cannot overwrite session title/counters. State/revision/receipt
CAS is one transaction, including absent-board insertion. Use repository SQL
query/migration conventions; do not route through legacy `SetTodos`.

Migrate existing todos into stable ordered task IDs and preserve titles,
active forms, completed states, and exact deletion guards. If legacy data has
multiple active tasks, retain the earliest source-order active task and move
the others to pending, with an explicit migration diagnostic; never reopen
completed tasks. Invalid legacy states abort conversion with a diagnostic and
retain original data; do not coerce them into success. An empty legacy list
with deletion guards stays uninitialized with those guards retained. Migration
is transactional/idempotent, not a per-request fallback. Once cutover is
complete, remove obsolete columns, structs, merge helpers, tests of full-list
semantics, and old tool/protocol paths.

Freeze the new read-only session/wire projection and current-tree binding before
parallel consumer migration. Session Get/list consumers must not expose private
receipt data. Resume loads the canonical DB state; message text and compaction
are not a second authority. A fork clones the **current** tree and guards in
the existing fork transaction into the new tree scope, preserving scoped node
IDs but not the parent's invocation receipts. This matches the current fork's
current-state semantics, not OMP's historical-branch replay. Historical rewind
is not silently introduced; a future rewind feature needs its own snapshot
contract. Session removal removes its tree within the same lifecycle boundary.

### A4 — parallel consumers after the storage/projection contract

- **Agent/MCP adapters:** native `tasks` tool and external CLI MCP registration;
  tool inventory, permissions/reviewer allowlists, hook/concurrency settings,
  and tool-name fixtures. Both call the same neutral protocol with trusted
  session/actor/invocation context. Remove `todos` rather than retaining an alias.
- **Host/prompt policy:** prompt and compaction projections, progress-guard
  classification, continuation/reminder paths, and their behavioral tests.
  Keep planning focus separate from running jobs; preserve bounded reminders
  and question/cancel/async-wait precedence.
- **WebUI/protocol:** server command handler and wire types, browser store,
  tree component and affected consumers. Stable-ID keys; incremental edit/move/
  remove/reopen; versioned acknowledgements; conflicts refresh canonical state
  rather than overwriting it with a stale optimistic array. Clear-completed
  issues one atomic operator removal of the selected completed IDs.

One integration owner owns session/storage/shared wire contracts and final
cross-surface tests. Consumer slices have disjoint files fixed before launch;
none rewrites shared session structs independently. Any existing terminal UI
consumer found at cutover is assigned explicitly, with its applicable context
read before changes. Do not invent a TUI subsystem that is absent from the
inspected task surface.

### A5 — integrated acceptance and clean cutover

Prove native-agent, external-CLI MCP, and WebUI edits reach the same tree and
revision; concurrent operator edits cannot vanish; restart and fork preserve
the promised scope; stale invocations cannot restore deleted tasks. Exercise
the actual WebUI surface and real command/API paths, not only mock adapter
forwarding. Update affected SDK/session serialization consumers and their
contract tests; use LSP references before removing exported legacy symbols.

Prompt facts distinguish actionable, blocked, abandoned, empty, and successful.
A blocked-only tree is visible but does not cause paid reminder loops. Model
final prose, successful CLI exit, or a returned MCP result never completes a
planning task automatically. Reminder budgets are host policy, bounded even
across resets; revision/no-op/label changes alone must not reset the budget.
Do not expose an executor failure as a completed planning goal to silence it.

Remove obsolete full-list API paths, aliases, old status-ranking helpers,
content-keyed persistence, and tests that only pin old wording/implementation.
Retain tests protecting operator deletion, status authority, concurrency, and
session lifecycle, migrated to the new observable contract. No commits/pushes
or worktree cleanup without a separate operator request.

## 8. Relationship to job batches

Related documents:

- [Job-batches design](2026-10-07-job-batches-design.md).
- [Second-round review summary](../reviews/2026-10-08-job-batches-xxs-round2-summary.md).

Task-tree leaves describe intended work. Job-batch leaves describe executions.
Their IDs, statuses, lifetimes, aggregation rules, and terminal authorities are
not interchangeable. No import or prerequisite from `tasktree` to `jobbatch`;
either autonomous component can be implemented and run first.

Later an adapter may explicitly associate a planning node with one or more
execution references. That association is metadata outside this kernel.
A successful execution is evidence presented to the agent/operator, not an
implicit `done`: one goal may require several executions or verification.
Blocking/dropping a planning task does not implicitly kill work; killing work
does not silently abandon its goal. Do not infer links from matching titles.

The batch plan's universal CLI/MCP/agent singleton ingress is unchanged. If it
wraps a task-control MCP call, completion of that service call is not completion
of the planning task being edited. Task-tree development neither fixes nor
assumes closure of the batch plan's outstanding second-round findings.

Carry over the review lessons: aggregate never-active descendants correctly;
keep final state readable; specify CAS/replay/cancellation ownership; use exact
fixture oracles and unique invocation labels; freeze real interfaces without
placeholder implementations; centralize the later integrated authority. None
requires importing the batch scheduler or refactoring it now.

## 9. Implemented API, development runs, and verification

Production code is in `internal/tasktree`, `memory`, `protocol`, and `lab`;
the standalone executable is `cmd/tasklab`. Its only non-stdlib runtime imports
are those tasktree packages. There is no Rush startup, provider/config lookup,
MCP/LSP connection, SQLite database, or job scheduler in this dependency graph.
The neutral definition is named `tasks`; it is **not registered in Rush yet**.

### 9.1 Host API

Create `memory.NewStore(limits, seeds)`, `tasktree.NewService(store, limits)`,
then `protocol.New(service)`. Protocol accepts its small consuming `Backend`
interface; the concrete service implements it. Bind `protocol.Invocation`
with TreeKey, trusted Actor, and stable RequestID before calling Execute.
Definition(agent/operator) exposes the corresponding operation schema.

The kernel exports EmptySnapshot, Restore, ValidateSnapshot, ValidateLimits,
CloneSnapshot, Apply, Snapshot, View, Summary, and Briefs. Service exposes
CheckEnvelope and CloneEnvelope in addition to versioned read/mutate methods.
Ownership transfer inside the service uses validated private kernel state;
public inputs/outputs and Store boundaries remain detached.

Positive-revision imports may contain baseline nodes/deletion guards and
partial or empty invocation receipts. A snapshot does not need fabricated
bootstrap calls to become a new board scope. Retained receipts are validated
and conserved; each new Commit adds exactly one receipt at its new revision.
An uninitialized imported board can retain legacy title guards and reports its
actual revision, not an assumed zero.

### 9.2 Standalone operator commands

Flags precede positional arguments. Save and resume into the same file:

```text
go run ./cmd/tasklab run --snapshot board.json internal/tasktree/lab/testdata/export-save.json
go run ./cmd/tasklab inspect board.json
go run ./cmd/tasklab run --load board.json --snapshot board.json internal/tasktree/lab/testdata/export-resume.json
go run ./cmd/tasklab repl --tree-key lab --actor agent --actor-id developer
go run ./cmd/tasklab repl --tree-key lab --actor operator --actor-id operator
go run ./cmd/tasklab --help
```

REPL input is a host wrapper, not extra fields in the model payload:

```json
{"request_id":"init-1","payload":{"op":"init","expected_revision":0,"items":["Implement core","Verify behavior"]}}
```

EOF/blank line ends REPL; malformed wrappers terminate with exit 1.
Correctable task errors emit structured results and permit another command.
Checkpoint publication uses a same-directory temporary file, Sync, Close,
then rename. Failed validation/encoding/write/sync/close/rename preserves the
previous destination; successful files satisfy the reader's size bound.

Lab-only bounds, independent of caller-supplied domain limits:

| Boundary | Limit |
| --- | ---: |
| Scenario/checkpoint encoded input, including whitespace | 4 MiB |
| Scenario steps | 512 |
| Retained report text | 256 KiB |
| Retained view/created-node occurrences | 4096 |
| JSON nesting | 128 |
| REPL line bytes | 1 MiB |

Codec rejects duplicate object members at every depth. Direct typed scenarios
and checkpoints are preflighted before large avoidable encoding allocations.
Overflow is an error, never truncation or an implicitly successful scenario.

### 9.3 Observed autonomous acceptance

- Focused build and vet of `internal/tasktree/...` and `cmd/tasklab` passed.
- Normal and race suites passed for all five standalone packages and `csync`,
  with `-p 1 -parallel 2 -count=1`; heavy runs were sequential and memory-capped.
- Compiled CLI fulfilled basic, blocked-only, nested, retries, operator-delete,
  boundaries, and export-save fixture oracles.
- Export/inspect/resume advanced revision 4 to 6, preserved the original
  receipt at revision 1, reported removed created ID `n5`, retained blocker
  `waiting`, and continued the ID counter at 9.
- Real agent REPL showed blocked-only unfinished work with no actionable task;
  unblock left pending with no focus. Unicode labels remained readable.
  Operator REPL explicitly reopened/removed work; forged actor wrapper exited 1.
- Six throwaway compiler overlays produced expected behavioral failures:
  unblock auto-focus, receipt/revision ordering, missing CAS, permissive decoder,
  disabled fixture comparisons, and pointer-only map-schema alias. Unmodified
  targeted runs passed afterwards; live production sources were never neutered.
- `go list -deps` confirmed the standalone runtime isolation described above.

Full-tree vet exposed an existing lock-copy receiver on csync.Map's schema
alias. A lock-free embedded receiver preserves value-type schema discovery;
the actual reflector/map-JSON smoke and its schema regression passed. This is
a verification blocker fix, not task-tree integration into Rush.
