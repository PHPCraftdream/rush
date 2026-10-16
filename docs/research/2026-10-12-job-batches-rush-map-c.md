<!-- Read-only code map produced for the job-batches Rush integration plan. Anchors are as of commit 48890aaa; re-read the code before relying on a line number. OBSERVED = seen in source, INFERRED = integration conclusion, neither is executed verification. -->

# Area C: await, arbiter, delivery, and operator surfaces

**Read-only investigation completed with one worker, followed by direct source inspection. Files changed: none. Tests/builds/heavy commands run: none.**  
**OBSERVED** below means inspected source or test definitions—not executed behavior. **INFERRED** marks integration implications and proposed work.

## 1. CURRENT STATE

### A. `await_tasks`: two different waiting mechanisms

**OBSERVED — tool contract.** `AwaitTasksParams` exposes `until`, `max_wait_seconds`, and `note`; `AwaitTaskRef` identifies work by tool-call ID and optional child-session ID, not batch/node identity. Top-level success sets `StopTurn`; worker success returns normally using `Blocked`, `TimedOut`, and `FinishedCount`. Errors become model-safe tool responses.  
Anchors: `internal/agent/tools/await_tasks.go:19`, `internal/agent/tools/await_tasks.go:36`, `internal/agent/tools/await_tasks.go:85`, `internal/agent/tools/await_tasks.go:90`, `internal/agent/tools/await_tasks.go:134`.

**OBSERVED — top-level coordinator path.** `AwaitTasks` requires the legacy async store. It selects worker behavior through `ParentSessionID`; otherwise it reads `LiveWorkForRoots`, combines own work and descendant delegations, rejects an empty set, arms SleepAll for `all`, and optionally creates a once-wake schedule. Both modes retain the max-wait schedule ID. This is a display snapshot, not a captured collection of root handles/Done channels.  
Anchor: `internal/agent/coordinator_await_tasks.go:45`.

**OBSERVED — worker path.** `awaitTasksForWorker` blocks within the tool invocation. `awaitTasksWorker` captures `(owner, tool_call_id)` pairs, removes disappeared identities only after complete reads, excludes later launches, deduplicates identities, and permanently latches observed disappearance. ANY returns when one captured identity disappears; ALL when none remain. Cancellation and timeout do not stop the work. Current limits are one-second polling, a forty-minute cap, and max-wait bounds 60–21600 seconds.  
Anchors: `internal/agent/coordinator_await_tasks.go:25`, `internal/agent/coordinator_await_tasks.go:101`, `internal/agent/coordinator_await_tasks.go:111`, `internal/agent/coordinator_await_tasks.go:157`.

**OBSERVED — timer lifecycle.** `clearSleepAll` clears sleep and cancels the remembered max-wait schedule using a detached bounded context; cleanup errors are logged. The Drain commit decision clears it when a launch is allowed.  
Anchors: `internal/agent/coordinator_await_tasks.go:219`, `internal/agent/agent_drain_decision.go:34`.

### B. Arbiter and CLI lifetime

**OBSERVED — rules 1 and 4b.** `decide` checks pending-inclusive debt first: no debt means `VNone`. After driver/hold/suspension gates, rule 4b defers when SleepAll, CompletionOnly, and either RunningOwnJobs or RunningDelegation hold. Reaction-chain, released-child, pacing, and autonomy gates follow.  
Anchor: `internal/agent/turn_arbiter.go:235`.

The inputs are **not interchangeable**:

- RunningOwnJobs counts own async work plus descendant delegation rows.
- RunningDelegation is the child-side fact that a delegation claims this session; it also affects released-child and autonomy policies.
- CompletionOnly treats legacy job debt as completion-class and classifies session notices by kind.

Anchors: `internal/agent/turn_arbiter.go:76`, `internal/agent/turn_arbiter.go:98`, `internal/agent/turn_arbiter.go:438`, `internal/agent/turn_arbiter.go:469`, `internal/agent/turn_arbiter.go:501`.

**OBSERVED — conservative-read mismatch.** Worker waiting checks incomplete flags, but the arbiter’s SleepAll count uses returned slice lengths without checking those flags. Its comment explicitly permits zero/unreadable work to lift sleep. **INFERRED:** this must change for the design’s “lag toward live, never falsely finished” requirement; no runtime failure was reproduced.  
Anchors: `internal/agent/coordinator_await_tasks.go:119`, `internal/agent/turn_arbiter.go:501`, `docs/plans/2026-10-07-job-batches-design.md:2297`.

**OBSERVED — CLI scope.** `CLIScopeState` separates WorkOpen from Drain and exposes OnceWakeOpen. `CLIScope` reads outstanding shell completion holds before running rows, then once schedules, then debt. An active once schedule or outstanding completion hold keeps WorkOpen true. Hints cause rereads; the cross-process fallback is five seconds.  
Anchors: `internal/agent/coordinator_reaction_source.go:44`, `internal/agent/coordinator_reaction_source.go:192`, `internal/agent/coordinator_reaction_source.go:229`.

**OBSERVED — schedule lifetime.** Active once wakes keep CLI execution open; loops do not. Ordinary scope-close cancels loop schedules, whereas cancellation/timeout is distinguished from scope-close.  
Anchor: `internal/app/app_run_wake_hold.go:11`.

**INFERRED:** root liveness must replace job-row liveness without conflating three concepts: await targets, reaction debt, and CLI lifetime—including once wakes. RunningDelegation must not be mechanically removed from unrelated child-driver policies.  
Anchors: `internal/agent/turn_arbiter.go:281`, `internal/agent/turn_arbiter.go:293`, `internal/agent/coordinator_reaction_source.go:243`.

### C. Started responses, inline delivery, SDK waiting, and reactions

**OBSERVED — launch path.** `asyncTool.Run` treats CLI/web calls as detached and SDK/unspecified calls as synchronous. Retry attaches to existing work. A fresh execution launches once; synchronous callers wait, while asynchronous callers receive started metadata or an inline result. Inline fallback retains that same execution. Metadata currently carries `job_id`, child-session ID, claim ID, and async/inline/status fields.  
Anchors: `internal/agent/async_tool.go:64`, `internal/agent/async_tool.go:114`, `internal/agent/async_tool.go:142`, `internal/agent/async_tool.go:196`, `internal/agent/async_tool.go:224`, `internal/agent/async_tool.go:265`.

**OBSERVED — SDK/server distinction.** SDK `Client.Run` defaults to OriginSDK, fails fast on an in-process busy session, and forwards caller context to ExecuteRun. Web handlers deliberately detach execution from WebSocket lifetime; closing a browser connection is not explicit cancellation.  
Anchors: `sdk/sdk.go:562`, `internal/server/handlers_agent.go:242`.

**INFERRED — cancellation replacement.** Current synchronous execution inherits caller cancellation through its execution context. Integration must instead explicitly request `Runner.Stop` when the caller cancels; cancelling `Batch.Wait` alone is insufficient and must not manufacture settlement or release admission.  
Anchors: `internal/agent/async_tool.go:106`, `internal/agent/async_tool.go:247`, `docs/plans/2026-10-07-job-batches-design.md:795`, `docs/plans/2026-10-07-job-batches-design.md:2167`.

**OBSERVED — durable delivery.**

1. Started history and `announced` commit together.
2. Pending announced job notices are pulled through per-row transactions combining delivery CAS, history insertion, and message identity.
3. Publication happens after commit; per-row pull failure leaves debt pending.

Anchors: `internal/session/notice_pull.go:105`, `internal/session/notice_pull.go:129`, `internal/session/notice_pull.go:329`.

**OBSERVED — reaction path.** `wakeSession` persists nothing: it hints an external CLI driver or submits a gated Drain. Delegation callers have a separate blocking queued-turn helper; wake callers intentionally do not use it. Drain requires visible debt after pull, rechecks arbiter at commit, and clears await sleep on allowed execution.  
Anchors: `internal/agent/coordinator_wake.go:21`, `internal/agent/coordinator_run_await.go:13`, `internal/agent/agent_drain_decision.go:13`.

**INFERRED — “elected inline-close.”** Preserve the transaction principle, not legacy row identities: after committed root settlement, the elected synchronous/inline tool result must atomically consume that root’s selected delivery. It must prevent a second completion notice/turn. A generic Stop acknowledgment is not that election.  
Anchors: `internal/session/notice_pull.go:329`, `docs/plans/2026-10-07-job-batches-design.md:2263`.

**OBSERVED — incompatible legacy stop semantics.** `job_kill` marks work stopped before physical kill and can return stopped despite kill failure. Legacy cause mapping sets delivery done/reacted and suppresses later completion. Existing store operations also have retry/backoff machinery.  
Anchors: `internal/agent/tools/job_kill.go:130`, `internal/agent/tools/job_kill.go:188`, `internal/agent/work_ledger_transition.go:52`, `internal/agent/work_ledger_transition.go:83`.

**INFERRED:** these are replacement sites, not behaviors to preserve. Stop intent, control acknowledgment/error, and confirmed termination need separate presentation; transition projection failure must follow fatal commit handling rather than legacy retry.  
Anchors: `docs/plans/2026-10-07-job-batches-design.md:2217`, `docs/plans/2026-10-07-job-batches-design.md:2231`.

### D. Model, CLI, and browser surfaces

**OBSERVED — tool assembly.** Current registration includes job output/kill, inspect/inject/stop agent, awaits, wakes, and ordinary tools. MCP candidates are filtered before wrappers. Assembly applies async wrapping, restricted-run, top-level hooks, then outer stall-detach.  
Anchor: `internal/agent/coordinator_tools.go:701`, `internal/agent/coordinator_tools.go:783`.

**OBSERVED — descriptions/prompts.** The await description names only commands/delegations and explains top-level StopTurn versus worker blocking. Orchestrator await guidance in the coder template is conditional on WorkerAvailable. Agent injection addresses a child session through AgentControl.  
Anchors: `internal/agent/tools/await_tasks.md:3`, `internal/agent/templates/coder.md.tpl:15`, `internal/agent/tools/inject_agent.go:16`.

**INFERRED:** descriptions, references, registration, and policy assembly need batch semantics, including no-worker command sessions; leaf dispatch must reuse the pinned policy-wrapped target without recursively applying ingress or creating another stall job.  
Anchor: `docs/plans/2026-10-07-job-batches-design.md:2152`.

**OBSERVED — CLI placement/control.**

- `sessions` registers as a root command; jobs registers beneath it.
- `sessions jobs` is observation-only and explicitly lacks cross-process individual-job stop.
- `sessions cancel` writes session-wide cancellation requests.
- `sessions inject` persists a user message plus pending-inject signal.
- `sessions kill` resolves the session lock and probes/kills its holder.

Anchors: `internal/cmd/sessions.go:63`, `internal/cmd/sessions_jobs.go:68`, `internal/cmd/sessions_jobs.go:11`, `internal/cmd/sessions_cancel.go:62`, `internal/cmd/sessions_inject.go:198`, `internal/cmd/sessions_kill.go:157`.

**INFERRED:** implement `rush batch` as a sibling root family in `internal/cmd/batch*.go`; do not repurpose session rescue/whole-session cancellation as nested-node Stop. Cross-process Stop must use owning-host requests/acks.  
Anchors: `internal/cmd/sessions.go:64`, `docs/plans/2026-10-07-job-batches-design.md:2402`.

**OBSERVED — web flow.** `get_session_live_work` returns a snapshot from legacy rows across the delegation tree, split into commands and agents/fetches. Wire items are built around tool-call IDs, child sessions, status/reason, timestamps, and held-question badges. `LiveWorkList` renders flat rows keyed by tool-call ID, with transcript navigation—not hierarchy controls. Separately, transcript completion indexing parses human-readable notice text.  
Anchors: `internal/server/handlers_livework.go:57`, `internal/server/handlers_livework.go:82`, `internal/server/handlers_livework.go:118`, `web/src/components/LiveWorkList.tsx:86`, `web/src/components/LiveWorkList.tsx:141`, `web/src/asyncJobCompletion.ts:31`.

**INFERRED:** hierarchy/status must come from node projection independently of notification arrival: suppressed notices and held delivery cannot keep settled nodes visually running. Keep transcript navigation, but replace text-derived completion authority with structured identity/state.  
Anchors: `web/src/asyncJobCompletion.ts:52`, `docs/plans/2026-10-07-job-batches-design.md:2303`.

## 2. What section 9 should say against current HEAD

The replacement contracts remain applicable; update its TODAY/navigation text as follows:

| Topic | Current fact and required wording |
|---|---|
| Worker capture | Already captures owner/tool-call pairs, ignores late launches, and tolerates incomplete reads—not merely total-count polling. Replace with captured root Done plus Snapshot levels. `internal/agent/coordinator_await_tasks.go:111` |
| Top-level capture | Current waiting references are not retained handles. Specify entry-captured roots separately from **session-wide live roots for 4b**, which includes later B in AW-b. `internal/agent/coordinator_await_tasks.go:64`; `docs/plans/2026-10-07-job-batches-design.md:1868` |
| ANY/ALL | NotifyAll ANY observes root termination; NotifyEach ANY can observe a terminal direct child, including later Add children. ALL requires every captured root Done. Worker delivery is irrelevant; top-level relaunch requires durable selected-notice debt and gates. `docs/plans/2026-10-07-job-batches-design.md:2303` |
| Debt | Suppressed/sole-child-elided nodes create no debt; selected batch notices are completion-class too. Stop intent creates none. `docs/plans/2026-10-07-job-batches-design.md:946`, `docs/plans/2026-10-07-job-batches-design.md:2320` |
| Scope | Preserve running-before-debt observation and once-wake lifetime independently of await capture. `internal/agent/coordinator_reaction_source.go:229` |
| Persistence | This is a replacement of legacy ledger consumers, not compatibility wrapping: drop async_jobs, retain host locks/session notices, use node outboxes/control requests. `docs/plans/2026-10-07-job-batches-design.md:2195` |
| Web | Two current state paths exist: durable flat snapshots and text-parsed transcript completions. Both require coordinated cutover. `internal/server/handlers_livework.go:82`; `web/src/asyncJobCompletion.ts:52` |

AW-a must fence worker return before held delivery; AW-b must distinguish Stop acceptance/return from confirmed termination and retain independent D debt when proving rule 4b. These remain **unexecuted integration requirements**, not batchlab receipts.  
Anchor: `docs/plans/2026-10-07-job-batches-design.md:1836`.

## 3. Independently mergeable implementation breakdown

**INFERRED proposals.** Merge seams first; activate consumer cutover only when projection/execution dependencies exist. No step should introduce a second terminal authority.

| Step | Goal; touch / do not touch | Focused verification; dependencies; size and risks |
|---|---|---|
| **C1 — Freeze consumer contracts** | Define root capture/liveness, selected-node debt, envelope and authenticated ownership/control interfaces. Touch await result/interface declarations and session reader boundaries. **Do not touch:** scheduler/lab, numeric defaults, legacy-row preservation. Basis: `internal/agent/tools/await_tasks.go:28`; design `:2195`. | Contract tests: Pending/Cancelling live, incarnation identity, incomplete reads conservative, suppressed nodes debt-free. Depends on persistence/executor owners and commit-hook agreement. **M**; identity mistakes affect every later surface. |
| **C2 — Worker await** | Replace row-disappearance polling with Done/level reads; retain captured-set semantics. Touch `coordinator_await_tasks.go`, tool rendering, worker tests. **Do not touch:** arbiter ordering, SQL writer, physical killing. Basis: coordinator `:111`. | ANY/ALL, NotifyEach later Add, new roots excluded, held delivery/termination, cancellation/cap. Depends on C1 and committed handles/state. **M**; lost wakes/self-parent capture. |
| **C3 — Arbiter/CLI readers** | Replace 4b liveness and completion classification; expose batch IDs; preserve timer cleanup and once wakes. Touch arbiter, reaction source, await/app scope tests. **Do not touch:** unrelated child-driver exceptions or rescue locks. Basis: arbiter `:235`, reaction source `:229`. | Rule1-before-4b; AW-b D/R/B; question/max-wait breaks sleep; incomplete reads; no premature CLI exit. Depends on node/debt readers. **M**; duplicate paid turns or premature exit. |
| **C4 — Delivery transactions** | Move pull/ack/elected inline-close/reaction identities to selected node rows. Touch notice pull, reaction accounting, response formatting/metadata and Drain seams. **Do not touch:** engine authority or retain legacy terminal CAS. Basis: notice pull `:129`, `:329`. | Delayed/failed ack; inline exactly once; rollback/publish-after-commit; pending versus visible debt; suppressed child no turn. Depends on atomic node/outbox projection. **L**; duplicate/lost delivery. |
| **C5 — Sync cancellation/delivery** | Connect caller cancellation to Stop; preserve same batch through background escape; keep web wake admission nonblocking. Touch async adapter, SDK-facing integration tests, queued admission seams. **Do not touch:** browser-disconnect policy or synthesize settlement. Basis: async tool `:114`, queued helper `:13`. | Caller cancellation with held termination; admission retained; inline/detached one completion; busy-mailbox Drain queues without waiting for delegation outcome. Depends on executor adapters/C4. **M**; leaks or premature termination. |
| **C6 — Model surface** | Add five batch tools, descriptions, batch IDs and prompt guidance; update policy/stall classifications. Touch tool assembly, new tool files, await `.md`, templates/tests. **Do not touch:** raw factories, shared MCP cancellation, Notify arguments on ordinary tools. Basis: assembly `:701`, `:809`. | Restricted/scoped availability, no recursive ingress, dynamic MCP no duplicate stall job, OneItem all versus explicit each. Depends on adapter/C1. **M–L**; policy bypass; coordinate assembly ownership. |
| **C7 — Operator commands** | Add sibling batch run/show/add/stop; migrate job observation and implement requester/owner control flow. **Do not touch:** session rescue semantics or use session cancellation for leaf Stop. Basis: sessions jobs `:11`, `:68`. | Separate processes, nested leaf stop/siblings unchanged, stale incarnation, unknown host, ownership rejection. Depends on control-request dispatcher/readers. **L**; cross-process identity/security. Read-only show can merge first. |
| **C8 — Web hierarchy** | Structured node wire/store, hierarchy renderer and controls, transcript metadata update. Touch server livework/protocol, React livework/types/completion indexing, browser tests. **Do not touch:** scheduler or DB settlement. Basis: livework handler `:118`, list `:141`. | Nested Add/subtree Stop/output/held input; reload; Pending/Cancelling/outcomes; delayed/suppressed notice; inline no duplicate; actual backend browser flow. Depends on C1/C4/C7. **L**; divergent snapshot/transcript state. |

**Existing verification infrastructure, OBSERVED:** worker tests explicitly cover late launches/reappearance/owner distinction; mock-WebSocket Playwright tests cover started-to-notice status and livework counts/navigation. Main Playwright config uses the port wrapper/dev server; a separate e2e config targets a real backend and warns that it creates sessions/sends messages. None was run.  
Anchors: `internal/agent/coordinator_await_tasks_worker_test.go:276`, `web/tests/async-job-status.spec.ts:7`, `web/tests/live-work-panel.spec.ts:62`, `web/playwright.config.ts:14`, `web/tests/e2e/playwright.config.ts:3`.

**INFERRED:** final acceptance needs deterministic AW-a/AW-b plus NotifyEach/Add/elision, faulted-commit containment, and real-browser hierarchy tests—not mock-only completion rendering.  
Anchors: `docs/plans/2026-10-07-job-batches-design.md:1865`, `docs/plans/2026-10-07-job-batches-design.md:2416`.

## 4. Open decisions

1. **Commit-hook/fatal-exit contract and ownership:** freeze signature, fault notification, containment Close, and process-exit routing before delivery wiring. Design explicitly defers the signature.  
   `docs/plans/2026-10-07-job-batches-design.md:2212`
2. **Await defaults:** choose worker cap/max-wait bounds; current forty-minute/one-second/60–21600 values are not selected integration defaults.  
   `docs/plans/2026-10-07-job-batches-design.md:2326`
3. **Legacy model tool disposition:** remove job_kill/job_output or make node aliases; never retain independent control authority.  
   `docs/plans/2026-10-07-job-batches-design.md:2399`
4. **Rank/group/cap policy:** jointly freeze with executor owner; worker await holds agent admission and must target lower-ranked separate roots.  
   `docs/plans/2026-10-07-job-batches-design.md:2289`
5. **Agent outcome declaration:** requirement remains undecided and affects displayed failure/results.  
   `docs/plans/2026-10-07-job-batches-design.md:2332`
6. **Universal ingress versus control-tool recursion:** clarify the permitted execution path for await/batch controls themselves while prohibiting generic tasks from recursively invoking scheduler/control tools.  
   `docs/plans/2026-10-07-job-batches-design.md:2162`, `docs/plans/2026-10-07-job-batches-design.md:2411`
7. **Delivery-error boundary:** distinguish fatal transition projection failure from consumer pull/history failure; current pulls retain pending debt on failure.  
   `docs/plans/2026-10-07-job-batches-design.md:2231`, `internal/session/notice_pull.go:105`
