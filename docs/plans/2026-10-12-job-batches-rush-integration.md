# Job batches — Rush integration plan

Status: plan against commit `48890aaa` (2026-10-12). It refines section 9 of
`2026-10-07-job-batches-design.md` (revision 4) with the real code. The
autonomous engine, Runner, process executor and `batchlab` are implemented and
verified locally (the acceptance map and usage documentation are produced by
phase A2b of the design, `docs/jobbatch-acceptance.md` and `docs/jobbatch.md`). Operator decisions (2026-10-11) stand: every tool invocation is a batch
(a single call is a batch of one via `jobbatch.OneItem`), Notify is chosen at
batch launch (default `all`), no old contract is preserved, a commit failure
is fatal.

Evidence: three read-only code maps, `docs/research/2026-10-12-job-batches-rush-map-{a,b,c}.md`
(A: tool execution, B: durable model, C: await/arbiter/surfaces). A line
anchor in them is a hint, not a contract; every package re-reads its code.

## 1. Strategy

1. **One terminal authority, always.** Until the cutover the old async system
   (`async_jobs`, work ledger, stall jobs) stays the only authority and the
   new path is inert. At the cutover the old authority is deleted in the same
   change. Never two live authorities for the same work.
2. **Inert until flipped.** Packages R1-R11 are additive or pure refactors
   and keep `main` shippable: the new path sits behind one internal switch
   (`RUSH_BATCH_RUNTIME`, default off, not a user feature and not documented
   for users). R12 flips it on, deletes the old path and the switch.
3. **Each package ends green**: focused tests, `-race` where the package is
   concurrent, lint 0, and a short report; the orchestrator verifies by its
   own run, merges by hand and commits per package.
4. **No compatibility wrappers over the old ledger.** An adapter never calls
   the old Claim/Transition terminal machinery; it is written against
   `jobbatch.Executor`.
5. Review Stop Rule applies to every package review.

## 2. Corrections to design section 9 (from the code maps)

| # | Design says | Reality | Plan consequence |
|---|---|---|---|
| 1 | "One tool construction path" (9.1) | Coordinator pipeline exists, but `agentic_fetch` builds private tools, `SessionAgent.SetTools` wraps partially, the CLI-provider bridge executes tools on its own (map A.A) | R1 consolidates behind a pinned policy-leaf registry; the CLI bridge and agentic_fetch resolve leaves from it (R7d) |
| 2 | "Confirm termination from the real completion source" (9.1) | The source is lost today: shell lookup vanishes during Kill, async bash and the CLI provider return before the real wait completes (map A.B) | R2/R3 retain the source; an adapter reports only from it |
| 3 | Terminal on stop intent is legacy | `job_kill` and run_command cancel commit "stopped" before physical termination (map A.C, C.C) | Removed, not ported; stop intent, control answer and termination are three facts |
| 4 | Worker await "polls rows" | It already captures `(owner, tool_call_id)` pairs and tolerates incomplete reads; the arbiter does NOT check the incomplete flag when lifting SleepAll (map C.A/B) | R10 replaces both with captured roots + Done + level reads; incomplete reads must keep work live (lag toward live) |
| 5 | Held questions at any depth | New behavior: synchronous delegations are excluded and `awaiting_answer` returns a successful question-shaped response (map A.B) | R7c builds it for batch agent leaves; not a port |
| 6 | MCP results carry text/image/audio and errors | Owner conversion ignores upstream `IsError`; CLI proxy keeps only `Content` (map A.2) | R7a defines the full outcome envelope and fixes both boundaries |
| 7 | Runner "commit hook" | Runner publishes views/feed/Done before dispatch; there is no hook (map B.2) | R4 restructures the apply path; a post-publication callback is not a commit boundary |
| 8 | "Drop `async_jobs` without migration" | Existing databases still need a migration to drop it; `check_db_release_pairing*` checks connection release pairing, not schema (map B.1) | A new destructive Goose migration, no row conversion (D2) |
| 9 | "Every invocation is a batch" vs "await holds no admission unit" | Both are in the design; `await_tasks` waits for other roots (map A.4, C.4) | Control-plane tools are not batch leaves (D1) |
| 10 | Web | Two state paths: flat livework snapshot and text-parsed completions (map C.D) | R15 replaces both with structured node state |

## 3. Decisions (made here; reviewable, overridable by the operator)

| ID | Decision | Why / status |
|---|---|---|
| D1 | **Control-plane tools are not batch leaves.** Exact exclusion set: `await_tasks`; `batch_run`, `batch_add`, `batch_status`, `batch_output`, `batch_stop`; `ask_question`; `wakein`, `wakeon`, `loop`, `wake_list`, `wake_cancel`; `inspect_agent`, `inject_agent`, `stop_agent`. They execute directly but still through the pinned policy, hooks and permissions (direct never means raw dispatch), and they are rejected as generic batch task targets. Everything else (built-ins, fs, fetch, download, bash, run_command, MCP, CLI-provider calls, agent, agentic_fetch) enters via `OneItem`. | A waiter inside a leaf holds an admission unit while waiting for other roots (deadlock class of 3.2); Inject must stay available to release a held execution even when admission is saturated. The design amendment qualifies "every invocation" to "every work invocation". Reviewer-confirmed (`jb-plan-review`); **operator may still override**. |
| D2 | "Without migration" = a new destructive Goose migration drops `async_jobs`, `Down` recreates it empty; no row conversion; `batch_nodes` and `batch_control_requests` are new tables in an earlier additive migration (R5). | Machine-readable, works on existing databases; matches "no preservation". |
| D3 | Commit hook: `type CommitHook func(context.Context, Transition) error` as an optional `RunnerConfig.CommitHook` together with a **mandatory positive `RunnerConfig.CommitTimeout` when a hook is set**. A hook error, a panic, or the timeout elapsing without a return are all an unknown outcome: the Runner becomes faulted (`ErrCommitFailed`), suppresses all outward publication, and begins containment (design 9.2) independently of the hook goroutine, which may still be running: the writer is never reopened or retried while it might be. The Runner exposes `Faulted() <-chan struct{}` and the fault error. The Rush adapter owns the transactions; its sequence identity includes the Runner incarnation. | Reviewer P1-4: a hook that never returns must still reach containment and exit. |
| D4 | Fatal exit: a process-wide handler owned by the app boundary (`internal/app`) reacts to the Runner fault signal for every host kind (CLI `rush run`, the long-lived server, SDK hosts - an SDK host also exits the process, not only returns an error): bounded containment wait (default 10 s), log, process exit code 70. On a fault the host lock and writer are retained until exit; no normal cleanup path may release them first. In ordinary shutdown the Runner drain precedes projection/DB/host-identity release, and a Close timeout retains the still-draining writer (today `internal/app/app_lifecycle.go` releases both on agent-idle, which is not proof of a drained Runner; R11 changes that). | Reviewer D3/D4 answer. |
| D5 | Keep today's await numbers for the worker path (cap 40 min, 1 s poll replaced by Done/level events with 1 s safety poll, `max_wait` 60-21600 s). | The design leaves them unselected; changing them is out of scope. |
| D6 | Remove `job_kill` and `job_output`; `batch_stop` / `batch_output` / `batch_status` accept item ids. No aliases. | "No legacy preservation". |
| D7 | `task_outcome` is not introduced now. The agent adapter reports Completed when the scope closes cleanly, Failed on a run error or recovery loss; no prose inference. | Design leaves it undecided; adding a tool surface is out of scope. |
| D8 | Initial profile (all groups finite, global >= sum): rank 0 `io` (fs, fetch, download, run_command, MCP), rank 1 `cli_driver`, rank 2 `agent_driver` (agent, agentic_fetch); the default group `""` is capped too. Numeric caps start from the existing background-shell caps and are tuned by measurement in R11. Validator rejects equal/ascending dependencies at Submit and Add. Classification is capability-based (reentrant MCP and nested agent dependencies cannot be ranked by kind label alone). | Map A.3; reviewer P2. |
| D9 | MCP termination = the local call's return (remote side effects are not provable); this is documented as a verification limit, never claimed as stronger. | Map A.4. |
| D10 | Notify exposure follows the design: only on batch specs (`batch_run`, nested batches in `batch_run`/`batch_add` items); direct calls are `all`. | Design 9.4. |
| D11 | Per-call cost: every work-tool call costs a Submit/Started/Settled commit. Budgets are fixed BEFORE R11 starts (provisional): added p95 latency <= 15 ms per ordinary sequential call, and with 8 concurrent sessions at least 80% of the old aggregate throughput, both on the local SQLite WAL configuration; both measured with concurrent sessions and writer contention, not isolated runs. A breach is an explicit operator decision before R12; the durable projection is never bypassed to make numbers pass. | Performance risk of universal ingress; reviewer D11 answer. |

## 4. Work packages

Size S/M/L is a relative effort guide. Tests named per package are the
acceptance of that package. "Flag" = behind `RUSH_BATCH_RUNTIME` until R12.

### Phase A — pure refactors and inert additions (parallel where the file sets are disjoint)

| ID | Goal | Main files | Deps | Size | Verification |
|---|---|---|---|---|---|
| R1 | Pinned policy-leaf registry: one decoration of hooks, restricted-run, permissions, agentguard and scope; a leaf resolves its target from the pinned filtered set, never a raw factory | `internal/agent/coordinator_tools.go`, `hooked_tool.go`, `restricted_tool.go`, `agentic_fetch_tool.go`, `agent.go` | - | M | hooks rewrite/deny once; dynamic MCP restriction; folder/custom-disk floors; child hook omission; unknown target fails closed |
| R2 | Shell lifetime seam: retained cancel/completion/output handle independent of manager lookup | `internal/shell/background*.go`, `internal/agent/tools/bash.go` | - | M | Kill timeout leaves done open; repeated Stop; late done; output until Dispose |
| R3 | CLI-provider lifetime seam: expose the real PTY/pipe Wait independent of iterator cancellation | `internal/agent/cliprovider/provider_stream.go` | - | M-L | held completion, late Wait, final output, cleanup once; cross-platform vet |
| R4 | Runner commit boundary + fault lifecycle (`CommitHook`, `CommitTimeout`, `Faulted()`, `ErrCommitFailed`, independent containment, Wait/Events/Close semantics of 9.2) | `internal/jobbatch/runner*.go`, `runtime_types.go`, `errors.go` (owner-approved contract addition), lab hook seam for tests | D3 reviewed | L | fake held/failing/PANICKING/NEVER-RETURNING hook: Submit/Add/Settled/HostClosing; Start in flight at fault; Stop containment while the hook is stuck; Wait/feed faults; `-race -count=20`; batchlab fixtures unchanged; a subprocess acceptance case: a never-returning hook, Stop containment and bounded exit |
| R5 | Inert projection store: additive migration (`batch_nodes`, `batch_control_requests`), sqlc queries, session store API, reservation/projection/attach-to-live/input-mismatch | `internal/db/migrations`, `internal/db/sql`, generated, `internal/session/batch_*.go` | D2 | L | disposable-DB tests: reservation atomicity, ten states, matching-live attach, owner cascade; release-pairing guard passes |

### Phase B — adapters and delivery (inert; need Phase A packages)

| ID | Goal | Deps | Size | Verification |
|---|---|---|---|---|
| R6 | Managed-call stall-detach exclusion (including dynamic MCP) | R1 | S | managed calls create zero `stall-*` identities; unmanaged behavior unchanged |
| R7a | Generic + MCP executor adapters with the full outcome envelope (`IsError`, text/image/audio) at Owner and CLI-proxy boundaries | R1, R6 | M | report-before-Start-return, cancel, denied/IsError, panic containment, two shared MCP calls stop one |
| R7b | Process adapters: shell and argv (`run_command`), CLI-provider executor | R2, R3 | L (split by family) | Stop ack/timeout never settles; real completion settles once; cursor/EOF/truncation; Dispose barrier; one termination timer |
| R7c | Agent scope adapter: driver + scope report after confirmed scope closure; held question / Inject / Stop at depth | R1, live-root seam from R10a | L | turn-ended/descendants-live; held question with and without work; Inject-vs-Stop race; generation cleanup |
| R7d | CLI bridge and agentic_fetch resolve pinned leaves via the shared ingress | R1, R7a, R7b | M | guards/restrictions once; no raw-factory bypass; one identity |
| R8 | Selected-node outbox, root ack, inline-close and reaction transactions on node rows | R4, R5 | L | concurrent pulls; failed history insert; finish-before-ack; inline race; elision; visible/pending debt; question reaction |
| R9 | Recovery sweep (all nonterminal nodes -> Interrupted `host_lost`, one root notice, unannounced subtree deleted), retention, durable stop requests (requester writes, owning host accepts) | R5, R8 | L | concurrent recoverers; Pending/Cancelling; two-process nested stop; stale incarnation; partial-commit reconcile |

### Phase C — consumers (still flagged)

| ID | Goal | Deps | Size | Verification |
|---|---|---|---|---|
| R10a | Root/node read API (live roots, captured roots, level reads) with the conservative-read rule | R5, R8 | M | incomplete read keeps work live; Pending/stopped-awaiting-termination live |
| R10b | Await mapping: worker (`awaitTasksForWorker`) and top-level (`AwaitTasks`, arbiter rules 1/4b, CLI scope) on roots; scope lifetime and once-wakes kept independent | R10a | L | AW-a/AW-b harness; ANY/ALL; NotifyEach later Add; new roots excluded; held delivery/termination; question/max_wait breaks sleep |
| R10c | History and activity consumers: `sessions reset`, rerun (`internal/session/async_job_rerun.go`, `internal/agent/coordinator_rerun.go`), owner deletion, `sessions jobs/why/show/list/gc/audit`, server handlers (`handlers_livework.go` backend, sessions, agent rerun) and the remaining app/CLI consumers of map B's dependency table, each ported to node rows: atomic reset refusal for every nonterminal root state, selected-node delivery reconciliation with deleted history, incarnation-scoped post-commit Stop routing, disposal of legacy bound `session_notices` | R5, R8, R9 | L | reset refused for Pending and Cancelling roots; deleted ack/notice history reconciled; late completion after rerun never reaches a new incarnation; grep inventory of every `AsyncJob`/`async_jobs` consumer with its replacement test |
| R11 | Ingress: `OneItem` for every work tool (D1), profile validator (D8), kind->group->rank mapping, sync caller cancellation = `Runner.Stop`, background escape keeps the same execution, Web Drain stays nonblocking; the Rush Runner wiring (commit hook adapter, process-wide fault exit D4 for CLI/server/SDK, Runner drain before DB/host release in `app_lifecycle.go`) | R4-R10c | L | one-item CLI/MCP/agent launches sync/inline/detached with one completion; sync cancel with held termination cannot release admission; D11 measurement |

### Phase D — cutover

| ID | Goal | Deps | Size | Verification |
|---|---|---|---|---|
| R12 | Flip: new path is the only path; delete work ledger terminal authority, `async_jobs` readers/writers, stall jobs, `job_kill`/`job_output`; destructive migration behind a **pre-migration live-host gate** (probe every `async_hosts` identity with the OS host-lock probe; refuse the destructive DDL while any host is Alive or Unknown, with a clear message to stop other rush processes); remove the switch | R1-R11, R10c, R13, R15a | L | populated-DB upgrade with zero row copy; an already-running OLD host (a process holding a host lock) makes the new binary refuse the destructive DDL; no remaining `async_jobs` callers (grep gate backed by R10c's inventory); commit-fault subprocess exits and recovery sweeps; shutdown drain; full repository checks once |

### Phase E - surfaces: the controls and the web data path are PREREQUISITES of R12 (implemented flagged and verified before it); presentation polish and the operator CLI follow

| ID | Goal | Deps | Size | Verification |
|---|---|---|---|---|
| R13 | **Before R12.** Model tools `batch_run`, `batch_add`, `batch_status`, `batch_output`, `batch_stop` + prompts/descriptions (they replace `job_kill`/`job_output`); `await_tasks` exposes batch ids | R11 | M-L | restricted/scoped availability; no recursive ingress; OneItem all vs explicit each; the model can inspect and stop a detached execution through them (live run in the flagged mode) |
| R15a | **Before R12.** Web backend/wire/store cutover: structured node snapshot replaces the flat `async_jobs` livework rows and text-parsed completions (`internal/server/handlers_livework.go`, `web/src/types.ts`, completion indexing) | R10a | M-L | server tests; the existing web unit and Playwright suites pass with the structured wire; active work does not silently disappear |
| R14 | `rush batch run|show|add|stop` (sibling root family), requester/owner control flow, ownership checks | R9, R11 | L | separate processes: nested leaf stop, siblings unchanged, stale incarnation, unknown host |
| R15b | Web hierarchy presentation: hierarchy renderer, subtree Stop, output, held input | R12, R15a | M-L | Playwright against the real backend: hierarchy, Add, subtree Stop, reload, delayed/suppressed notice; Drain nonblocking |

## 5. Gates

- **Before R4**: D3 (hook, timeout, fault signal) and the R4 restructure of
  the apply path reviewed by `rush run --role reviewer` against design 9.2
  (publication order 3.4).
- **Before R11**: D11 budgets fixed (provisional values above, operator may
  change them).
- **Before R5 merges**: migration reviewed for existing databases; the
  connection-release pairing guard and the migration policy
  (`internal/db/migrate_policy.go`) still pass.
- **Before R12**: all of R1-R11, R10c, R13 and R15a merged and green with the
  switch ON in the full test run (not only focused tests); AW-a/AW-b executed
  (not static); D11 measurement recorded against its budget; the old-host
  refusal test passes.
- **After R12**: repository-required checks once (`go build -p 1 ./...`, lint
  of touched packages, spawn guard, file-size guard, the web checks for R15).

## 6. Risks and watch items

1. Universal ingress cost (D11) and SQLite write contention on the commit
   path; the single writer must never hold a session-store mutex across I/O.
2. Mixed binaries during the destructive migration (D2): the migration policy
   opens a database that holds unknown versions only (`internal/db/migrate_policy.go`,
   "unknown only -> open without migrating"), so it does NOT keep an old
   binary out, and an already-running old host never re-checks. The enforceable
   fence is the pre-migration live-host gate of R12 (host-lock probe); a
   binary started later against the new schema is an unsupported mix and is
   documented as such (it fails on its first job query), not fenced.
3. Fatal-exit semantics for a long-lived web server: an external supervisor
   restarts it; R11 documents this and tests the exit path in a subprocess.
4. The held-question feature (R7c) is new behavior with its own race surface;
   it gets its own review round.
5. Windows and Unix process termination differences (R2, R3, R7b); Unix is
   verified only by CI.
6. Size: R7b/R7c/R8/R9/R10b/R11/R12 are each large; each is split into
   independently green commits by its owner.

## 7. Orchestration

Packages are run with `/wrush` (worktree per package, the agent writes,
the orchestrator verifies, merges by hand and commits). Disjoint packages run
in parallel; concurrent heavy commands serialize through the repository's
heavy-command wrapper. Hard design questions go to `rush run --role reviewer`.
