# R11 ingress decomposition (strong-review answer, 2026-10-12)

Source: read-only design pass `jb-r11-arch` against main at 3b0eecfa; PROPOSED items are not yet decisions. Refines package R11 of `docs/plans/2026-10-12-job-batches-rush-integration.md`.

## Basis and recommendation

**OBSERVED:** the merged pieces are seams, not an integrated runtime: executor routing is explicitly inert (`internal/agent/batch_exec.go:63`), and construction still installs async/stall wrappers (`internal/agent/coordinator_tools.go:814`). R8 explicitly defers envelope persistence, question binding and activation (`internal/session/batch_outbox_doc.go:70`).

**PROPOSED:** split R11 into six merge packages—not new Go subpackages—and make only the last install production wiring. The earlier additions must have no initialization side effects.

### 1. Decomposition and order

All filenames below are proposed unless already present.

| Order | Package; size | Goal / main files | Dependencies | Acceptance tests |
|---|---|---|---|---|
| A | **Admission contract; M** | Immutable profile, capability classification, dependency validator; `agent/batch_profile.go`, `batch_admission.go` | R1 pinned capabilities; R6/R7 routing | Reject uncovered/default groups, invalid caps, ascending/equal dependencies and invalid Add; saturated descending-rank work progresses. |
| B | **Projection contract and hook; L** | Repair R5 identity/transaction gaps, encode transitions/envelopes; `session/batch_store.go`, new `batch_commit.go`, SQL/migration additions | R4/R5/R8 | Exact Runner/store IDs, atomic reservation-plus-view, Add ordinals, notice ordering, envelope durability and rollback; zero publication before commit. |
| C | **Ingress core; M** | One invocation API, live-attach coordination, pinned invocation metadata; `agent/batch_ingress.go`, `batch_invocation.go`, `batch_runtime.go` | A/B, R1/R6/R7 families | Concurrent matching retries execute once; mismatches fail; missing identity fails closed; controls cannot become leaves. |
| D | **Delivery/cancellation; M** | Sync/inline/detached elections and same-execution escape; `agent/batch_delivery.go`, `batch_cancel.go`, new history persistence adapter | C, R8/R10a/R10b; R7c for agents | Finish-before-ack, failed ack, inline-vs-started race, caller cancellation with held termination, escape without second launch, nonblocking Drain. |
| E | **Host lifecycle; L** | Dormant app runtime owner, fault watcher and drain gate; `app/app_batch_runtime.go`, `app_batch_fault.go`, `app_batch_drain.go` | B/C, R4/R9; R7 lifetime seams | CLI/server/SDK subprocesses exit 70 on stuck hook; fault/shutdown race retains lock/writer; Close timeout retains draining resources. |
| F | **Flagged installation; M** | Install only when flag enabled; `coordinator_tools.go`, agent tool-setting/call boundaries, `cli_tool_bridge.go`, `agentic_fetch_policy.go`, `app.go`, `app_lifecycle.go` | A–E, R7c/R10b/R10c | Flag-off behavior unchanged; flag-on invocation inventory has one authority, one policy pass and one completion for every family. |

**Parallelism:** A, B and E’s dormant scaffolding have disjoint file sets. After contracts freeze, D and E can proceed independently; C precedes D. F alone edits shared construction/lifecycle entry points. Coordinate B’s session/SQL ownership with in-flight R9/R10a rather than editing their files concurrently.

This preserves the plan’s one-authority rule (`docs/plans/2026-10-12-job-batches-rush-integration.md:18`): flag-on selects the new path, never “also records” old-ledger work.

## 2. CommitHook ownership, mapping and gaps

**PROPOSED:** `internal/session` owns the persistence adapter and transactions; `internal/agent` supplies immutable owner/invocation metadata; `internal/app` owns Runner installation and fatal lifecycle. Session may import jobbatch, never agent.

Build one `BatchProjection` by grouping `Transition.Changed` and `Notices` by root. Map every NodeView lifecycle/control/outcome field to update parameters; preserve existing envelope/child-session references when unrelated events change a node. Notice ordinal is its position in `Transition.Notices`, not structural node ordinal.

Responsibilities:

- **Submit:** reserve the complete accepted tree, with the Runner’s exact IDs, and project initial Changed views atomically before Start/reply.
- **Add:** reserve appended subtrees at accepted parent ordinals inside `Project`; do not call root `Reserve` to create another batch.
- **StopRequested:** persist Changed states and first-latched causes; never infer settlement from cancellation.
- **Started:** persist Running/token/attempt/control facts; this is not root announcement.
- **Settled:** persist effective Changed states, reported outcome and full response envelope/reference, plus precisely selected notices/pending delivery/wake in one transaction. Never reconstruct selection from terminal rows.
- **HostClosing:** persist all Changed interruption/cancellation facts; host-closing notices have wake=0 without rewriting already-latched causes.
- **Final:** persist a Runner lifecycle checkpoint if required; Final is a Transition boolean, not an event. No synthetic settlements.
- Also handle **StartRejected, ControlReported, DeadlineExpired, CapacityAvailable and Release**, including transitions affecting other roots through admission.

**OBSERVED R5 gaps:**

1. `Reserve` generates a UUID itself (`internal/session/batch_store.go:75`), whereas Runner roots are caller-supplied (`internal/jobbatch/runtime_types.go:18`). Reserve needs explicit root identity and atomic initial projection; separate Reserve/Project commits are insufficient.
2. Live attachment checks hash but not owning host (`batch_store.go:64`). Attachment needs owning-Runner routing, not resubmission to another Runner.
3. `BatchProjection` accepts host/sequence/roots only (`batch_store.go:119`). It lacks **Runner incarnation** and an event/lifecycle checkpoint for rootless transitions/Final/Release.
4. `Event.Result.Details` has no persistence input beyond a reference string; atomically store the envelope and reference.
5. `Changed` omits immutable task kind/payload/timeout: reserve these from Submit Spec/Add Items; Launch alone cannot describe never-admitted tasks.

**No missing scalar NodeView/Notice lifecycle field was found:** SQL already accepts state, cause, token, attempts, control fields, outcome, notice metadata/counts/sequence/ordinal/wake (`internal/db/sql/batch_nodes.sql:61`). Event Type/Ref/Reason/Op/Code/Group, Commands and SettledRoots are not all mandatory node columns; decide audit/checkpoint needs explicitly rather than inventing duplicate terminal authority.

Sequence identity must be **(host identity, fresh Runner-incarnation UUID, transition sequence)**; root UUID remains execution incarnation. Fence updates and controls against the corresponding incarnation; reject overflow converting uint64 sequences/tokens to SQL integers.

The adapter must not read unpublished Runner views to translate a transition, dispatch work, deliver history, hold metadata/store locks across writer I/O, retry unknown commits, reopen a faulted writer, or synthesize terminal outcomes. R4 already independently times out the hook (`internal/jobbatch/runner_commit.go:66`).

**Dependency gaps:** R8 lacks post-announcement sync consumption/general elected-answer handling, envelope loading and node-bound question delivery (`batch_inline.go:19`, `batch_outbox_doc.go:25`). R9 must supply host registration/probes, partial-commit reconciliation, retention/Release ordering and durable owner-side Stop acceptance. R10a must provide conservative captured-root/Done lookup and incarnation routing, including durable-live rows not present locally. Their in-flight implementations were not available here.

## 3. One ingress API

**PROPOSED:**

```go
func (r *batchRuntime) Invoke(
    ctx context.Context, inv BatchInvocation,
    leaf fantasy.AgentTool, call fantasy.ToolCall,
) (fantasy.ToolResponse, error)
```

`BatchInvocation` carries session/message IDs, trusted restricted-run/policy context, pinned registry **identity**, origin, delivery mode/window, background-escape policy and current waiter node/rank. `call.ID` is the tool-call identity. Direct calls always use `OneItem`/NotifyAll; explicit specs expose Notify through R13 only (`internal/jobbatch/runner_requests.go:11`).

The executor receives the invocation’s immutable policy context—not just session ID—and its exact pinned registry. **OBSERVED gap:** current payload has only tool/call/session/input, while registry lookup is session-keyed (`batch_exec.go:25`). Concurrent calls with different folder/disk/restricted toolsets must not collide. Generic execution presently rebuilds a context copying only three policy values (`batch_exec_generic.go:31`); transport must be audited across every family.

Sync waits; caller cancellation submits Stop using a bounded noncancelled control context. Execution/admission remains live until its real report. Inline elects durable completion or started history. Detached returns started and later selected notices.

Auto-background switches **delivery of the existing root**, not execution: the retained shell already runs; no ClaimShell, new root or second executor. The existing leaf waits for real Done (`internal/agent/tools/bash_leaf.go:44`).

D1’s exact set—await; five batch controls; ask_question; wakein/wakeon/loop/wake_list/wake_cancel; inspect/inject/stop_agent—executes the pinned policy leaf directly and is forbidden as a generic target (`batch_exec_generic.go:43`). Hooks remain once, including rewrite/approval and child omission.

R7d’s injected `pinnedInvoke` closes over `BatchInvocation` and delegates here. Bind ToolBridge to the enclosing trusted invocation; retain its single generated call ID and no-fallback behavior (`internal/agent/cliprovider/tool_bridge.go:70`).

## 4. Rank/admission

Profile belongs in agent, not jobbatch: finite `io=0`, `cli_driver=1`, `agent_driver=2`, explicitly capped default group, global ≥ sum of **all** caps.

At Submit **and** Add validate capabilities, supported executors, canonical groups, pinned availability, control exclusion, tree limits and dependency edges. Agent rank 2 may wait only on separate rank 0/1 roots; descendants do not consume an extra structural-ancestor unit. Reject same-rank nested delegation and dependencies sharing sequential/capped-ancestor gates. Reentrant MCP requires explicit capability restrictions/classification, not blind “MCP=0”.

The progress premises include eventual report, Stop independent of dependency admission and capacity-refusal wake/retry progress (`docs/plans/2026-10-07-job-batches-design.md:692`).

## 5. Before activation

Record and resolve:

- All projection/context/identity/delivery gaps above; envelope serialization failure must not silently lose media (`batch_exec.go:105`).
- Complete R7c held-question/Inject/descendant scope, R10b AW-a/AW-b and R10c reset/rerun/history reconciliation.
- Read translator: end_line=0 currently means default window, not EOF; filtering occurs after view slicing (`cli_tool_bridge.go:85`, `:118`).
- Bridge Todos intentionally changes legacy behavior to native tombstone filtering; approve/test this semantic change (`tools/todos.go:56`, `cliprovider/mcpserver_tools.go:431`).
- Confirm production CLI-driver executor/StreamSession integration: retained session exists, but inspected agent router lists no CLI family (`batch_exec.go:15`).
- D9 local MCP return limitation; D1 operator confirmation; D11 budget approval and contended measurements, never projection bypass.
- Unix real process termination acceptance, not cross-vet alone.
- R4 P2 backlog: containment requests outside publication bound; Release-retained attempts—track, not opportunistically fix.
- Single termination timer versus wake-only timers; notification kill-switch policy; restricted actions for new controls.
- D4 supervisor/SDK exit documentation and fault-versus-normal-cleanup fencing.
- R12 old-host Alive/Unknown pre-DDL refusal, destructive migration/no copying, complete legacy-authority removal, R13 controls and R15a structured web prerequisites.

## 6. Highest-risk gates

1. **Identity/atomic projection — gate B with strong review:** concurrent same-call retry plus finish-before-ack must yield exact matching IDs, one side effect and one delivered envelope.
2. **Admission/context integration — gate C/F:** saturate agent/CLI groups, launch lower-rank work from differently restricted concurrent calls; verify progress, policy isolation and rejected equal-rank Add.
3. **Fault/shutdown race — gate E:** never-returning hook plus idle agents and held physical termination; prove exit 70 within containment bound with no lock/DB release or failed-transition publication.

End each review when no reproducible P0/P1 remains; backlog P2/P3 without another confirmation round.

**Files changed:** none. **Tests/heavy commands run:** none; read-only source/design inspection only.