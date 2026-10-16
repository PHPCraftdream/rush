# R12 cutover decomposition (strongest-reviewer pass, 2026-10-14)

Source: read-only design pass `jb-r12-plan` against origin/ci/job-batches at 4083948e (R1-R11F, R13, R14, R15a merged; R11G in flight). PROPOSED items are not yet decisions. Refines package R12 of `docs/plans/2026-10-12-job-batches-rush-integration.md`. The inventory is source-anchored, NOT an exhaustive manifest: R12-A produces the uncapped tracked-file manifest before H.


**OBSERVED:** R12 is not presently “flip a boolean, delete unused files.” Several consumers still unconditionally read the old authority, and some new surfaces depend on legacy tool names.

- `internal/agent/coordinator_tools.go:835` selects batch assembly when a binding exists; its fallback still installs async and stall wrappers at lines 886–899.
- `internal/agent/coordinator_reaction_source.go:179` still reads ledger debt; maintenance at line 210 still sweeps/purges old jobs.
- `internal/app/app_session_activity.go:79` still obtains activity/debt/live-work facts from `AsyncJobStore`.
- `internal/app/app_run_quota.go:105` still collects finished work from old rows.
- `internal/server/handlers_livework.go:84` still reads old rows **even with the switch ON**; the structured push is additive at line 407.
- `internal/agent/batch_allowed_controls.go:29` makes `batch_status`/`batch_output` depend on `job_output`, and `batch_stop` depend on `job_kill`.
- `internal/server/handlers_batch_doc.go:33` explicitly leaves terminal-root completion delivery to R12.
- R10c supplies session-side incarnation-aware rerun reconciliation, but I found **no `BatchRerunStop*` implementation in `internal/agent`**. The old server call still passes `[]session.VoidedAsyncJob` at `internal/server/handlers_agent_rerun.go:429`.

**Inventory coverage limitation:** directory-wide searches timed out or truncated at 100 matches. Consequently, the following is a source-anchored family inventory, **not a certified exhaustive enumeration of every matching test/doc**. Do not use it as the final deletion manifest. The first package must produce the uncapped tracked-file manifest below; claiming completeness from the available search results would be false.

### Old-path families and disposition

Paths below are repository-relative; wildcard sets include existing matching tests, not permission to edit arbitrary neighboring files.

| Family | Observed files / test families | R12 disposition |
|---|---|---|
| Ledger terminal authority | `internal/agent/work_ledger*.go`; `work_job.go`; `async_tool.go`, `async_tool_test.go`, `async_tool_sync_test.go`; `bgshell_claim.go`; `delegation_question.go` | Delete authority and execution wrappers; preserve/move genuinely shared timeout, inline-window, question-formatting helpers first. `work_ledger_inline.go:28` defines the window used by batch installation; wholesale deletion would break it. |
| Ledger consumers | `coordinator.go`, `coordinator_tools.go`, `coordinator_background.go`, `coordinator_interrupt.go`, `coordinator_work_scope.go`, `coordinator_recheck.go`, `coordinator_reaction_source.go`, `coordinator_reaction_chain.go`, `coordinator_rate_limit.go`, `coordinator_session_state_sweep.go`, `coordinator_drain_policy.go`, `coordinator_wake.go`, `coordinator_rerun.go`, `coordinator_bgshell_cap.go`, `coordinator_agent_control.go`, `coordinator_await_tasks.go`, `turn_arbiter.go`, `drain_attempt.go`, `supervision.go`, `agent_notice_pull.go`, `agent.go`, `agent_options.go`, `agent_turn*.go` | Port terminal/debt/scope/control reads; retain mailbox, driver, wake, supervision and arbiter policy independently of ledger state. R10c’s earlier inventory is `internal/session/batch_consumers_doc.go:21–44`; current searches reveal additional consumers such as `drain_attempt.go`. |
| SQL/sqlc | `internal/db/sql/async_jobs.sql`, `async_jobs.sql.go`, `models.go`, `querier.go`; retained host queries currently live in the old SQL file | Split host queries before removing job queries. `DeleteAsyncHostIfNoJobs` currently queries `async_jobs` (`internal/db/sql/async_jobs.sql:61–84`). Removing only Claim/Transition is insufficient. |
| Session old store | `internal/session/async_job_{store,bgshell,reader,reader_batch,reaction,recovery,recovery_question,gc,reset,rerun}.go`, `async_job_store_testhook.go`; their matching tests | Delete job APIs after consumers move. Preserve host lifecycle, session notices, driver registration and shared history types in nonterminal-authority infrastructure. |
| Mixed retained infrastructure | `notice_pull.go`, `notice_row_ids.go`, `session_driver.go`, `session_child_question.go`, `host_lock.go`, `wake_schedule_{fire,store}.go`, `wake_scheduler.go` on the agent side; session activity/lifecycle comments | Separate retained functionality from old-table reads. `notice_pull.go:263` still resolves bound notices through `GetAsyncJob`; `notice_row_ids.go:50` still reads old pending-inclusive debt. |
| Stall jobs | `internal/agent/turn_stall_tool_detach.go`, `turn_stall_policy.go`; `turn_stall_tool_test.go`, `turn_stall_detach_cleanup_test.go`, `turn_stall_policy_spacing_test.go`, `restricted_stall_detach_test.go`; batch managed-call exclusion tests | Delete detached-job maps/controller/init registration and their call sites. Do **not** indiscriminately delete stall clock/watchdog/progress policy. The distinct job authority is visible at `turn_stall_tool_detach.go:43`, `:173`, `:247`. |
| Old model tools | `internal/agent/tools/job_{kill,kill_bgshell,output,output_pending,shell_resolver}.go`, `stall_detach_controller.go`, `job_kill.md`, `job_output.md`; matching tests, `job_run_command_control_test.go`, `r16_background_ownership_test.go` | Delete, no aliases. Keep retained shell/process execution and output-buffer seams needed by batch executors. |
| Prompts/tool inventories | `internal/config/config.go:416`; `internal/agent/coordinator_tools.go:198`, `:347`, `:711`; `batch_allowed_controls.go`; batch target classification; `agent_turn_step.go:399`; `tools/bash.go`, `bash.md.tpl`, `run_command.md.tpl`, `await_tasks.md`; `templates/coder.md.tpl:20`; `prompt/prompt.go:800` | Replace inventory dependencies and guidance together. Tests include coordinator worker/reviewer inventories, `batch_model_flag_test.go`, prompt/scoped tests and bash start-failure assertions. |
| App/lifecycle/CLI loop | `internal/app/app.go`, `app_accessors.go`, `app_agent_setup.go`, `app_lifecycle.go`, `app_session_activity.go`, `app_session_drivers.go`, `app_recovery.go`, `app_run_async_wait.go`, `app_run_quota.go`; `app_batch_install.go`, `batch_install_factory.go` | Retain host identity independently of `AsyncJobStore`; port all old-row readers and maintenance. App installation presently needs `AsyncJobStore.EnsureHost`, Close and CloseKeepLock (`app_batch_install.go:48`, `:96`). |
| App tests | `async_phase4_*`, `async_scenarios_*`, `app_run_*` tests seeding Claim/Transition/Get; activity/driver/forced-shutdown tests; batch factory/install tests | Port behavioral tests rather than merely deleting them. Recovery, quota, held questions, Ctrl-C, billing and await tests currently seed old rows. |
| CLI sessions | `internal/cmd/sessions_{jobs,why,show,list,gc,reset,kill,compact}.go`, `sessions_audit_{db,rules,findings}.go`; indirect activity consumers watch/tail/inject/cancel/locks | Rewire to history/live/debt APIs. Audit must stop silently disabling old-table rules when the table disappears. Tests include `sessions_jobs_test.go`, `sessions_show_jobs_test.go`, `sessions_why_{descendant,awaiting}_test.go`, `sessions_reset_async_test.go`, own-job/run-driver/list status tests. |
| Server/rerun | `internal/server/handlers_livework.go`, `handlers_batch*.go`, `handlers_agent_rerun.go`, `handlers_sessions.go`, `protocol.go`, associated hub/dispatch/wire references | Replace flat reads, not merely hide their output. Port rerun interface/fakes together; relevant tests include livework/awaiting, descendant-work, run-driver, rerun-hold and reservation/cancel/panic-window families. |
| Web completions | `web/src/asyncJobCompletion.ts`, `store.ts`, `store_livework.ts`, `batchWork.ts`, `types.ts`; `components/Chat.tsx`, `LiveWorkList.tsx`, `Message/{Message,ActionRow,ToolActivityGroup}.tsx` | Structured completion index must include terminal roots and survive reload. Current batch snapshot reads only live roots; current prose index remains at `store.ts:20`. Preserve rendering of historical messages without retaining a live legacy authority. |
| SDK / overrides | `sdk/sdk.go:77` aliases `app.RunOverrides`; `internal/app/app_run_request.go:72` exposes background default timeout; scoped/custom-disk tests | Do not delete SDK capabilities as collateral damage. Batch tool assembly currently rejects custom disk at `coordinator_tools.go:867`; explicitly test/document the intended supported SDK behavior before cutover. |
| Switch | `internal/config/batch_runtime_flag*.go`; app/agent/server helpers; `internal/cmd/batch_cli.go:41`; `prompt/prompt.go`; flag-off tests and subprocess environment settings | Remove last, after every caller has batch-only behavior. Flag-off tests must explicitly set their environment before the shadow suite runs. |
| Docs | Integration/design/R11 decomposition; `batch_*_doc.go`, `handlers_batch_doc.go`; SDK/runtime/operator documentation; historical async plans/reviews | Update current contracts and operator upgrade warning. Historical plans/reviews are allowed historical references, not grep failures and not mass-rewrite targets. |

### Measured lengths

**OBSERVED from numbered file endings:**

| File | Lines | Note |
|---|---:|---|
| `internal/agent/work_ledger.go` | 977 | Near 1000; deletion target, do not grow it |
| `web/src/store.ts` | 978 | Near 1000; completion replacement must shrink/extract within scope |
| `internal/agent/prompt/prompt.go` | 904 | Near limit |
| `internal/agent/coordinator_tools.go` | 901 | Near limit; shared cutover hotspot |
| `internal/server/protocol.go` | 815 | Shared wire hotspot |
| `internal/session/async_job_store.go` | 709 | Host lifecycle must survive job deletion |
| `internal/agent/coordinator.go` | 651 | Shared interface/construction hotspot |
| `internal/app/app.go` | 579 | App composition owner |
| `internal/agent/async_tool.go` | 556 | Deletion target; shared helpers remain |
| `internal/agent/work_ledger_delegation.go` | 554 | Deletion target |
| `internal/session/notice_pull.go` | 544 | Mixed retained/deleted functionality |
| `internal/server/handlers_livework.go` | 417 | Keep coalescing/wake-schedule push |
| `internal/agent/work_ledger_transition.go` | 388 | Delete terminal CAS/retry authority |
| `internal/agent/work_ledger_timeout.go` | 338 | Separate retained timeout contracts |
| `internal/agent/turn_stall_tool_detach.go` | 324 | Delete stall-job authority |

The file-size allowlist I read is empty apart from its header. I did **not** obtain a repository-wide line-count census.

## PACKAGES:

**PROPOSED:** separate *preparation* from *activation*. The plan’s “default off and shippable until cutover” requirement prevents arbitrary old-path deletion packages from independently merging while default-off production still uses those files.

Use an immutable manifest generated by R12-A. In this table, `tests(X)` means **existing test files explicitly listed beside X in that manifest**, not unrestricted package-wide editing. New filenames are proposed.

| Package | Goal / exact ownership boundary | Dependencies; size | CI acceptance |
|---|---|---|---|
| **R12-A — migration preparation** | `internal/db/migrate_policy.go`, new `migrate_live_hosts.go`, new `migrate_live_hosts_test.go`, `migrate_policy_test.go`; proposed destructive SQL under **`internal/db/testdata/`**, not embedded migrations; new pure probe files/tests in `internal/filelock`; minimal delegation of the existing probe in `internal/session/host_lock.go`. New read-only inventory script/report. | Start now; M | Synthetic migration FS proves populated upgrade, zero conversion, Alive/Unknown refusal, unchanged version/schema on refusal, fresh DB, already-applied no-op, MayMigrate=false, divergence matrix; actual child process holds old-format host lock. |
| **R12-B — sessions CLI preparation** | Only `internal/cmd/sessions_{jobs,why,show,list,gc,reset,kill,compact}.go`, `sessions_audit_{db,rules,findings}.go`, `tests(those files)`, new `sessions_batch*.go`. No app/session/agent/SQL edits. | Existing R10c store APIs; start now; M–L, split read/status and reset/audit commits | Flag-ON command tests against real node rows; Pending/Cancelling reset refusal; conservative unknown/read-error status; retention debt protection; audit counts; flag-OFF unchanged until activation. |
| **R12-C — web/server observation preparation** | Only `internal/server/{handlers_livework*,handlers_batch*,protocol.go,hub.go,wire.go}` plus dispatch file identified in manifest; listed livework/descendant tests; `web/src/{batchWork*,store.ts,store_livework*,types.ts,asyncJobCompletion.ts}`, named Chat/LiveWorkList/Message components above and their listed tests; new cutover Playwright fixture/spec. **Exclude rerun files/fakes.** | Start now; L, two serial commits inside one owner | Switch-ON has no old SQL reads; terminal-root completion appears once, reload works, incomplete reads preserve active work, two reused tool-call IDs stay distinct, Drain remains nonblocking. No R15b hierarchy/control expansion. |
| **R12-D — retained session infrastructure preparation** | Only `internal/session/{async_job_store.go,async_job_reaction.go,async_job_recovery.go,notice_pull.go,notice_row_ids.go,session_driver.go,session_child_question.go,wake_schedule_fire.go,wake_schedule_store.go}`, `tests(those files)` and new `host_store*.go` / `session_notice_store*.go`. **Exclude `host_lock.go`, `batch_read*.go`, `batch_adapters*.go`.** No SQL generation. | A’s probe contract; can start after ownership freeze; L | Host registration/CloseKeepLock/drivers/notices work without job methods; no table dependency in the new retained implementation; existing old store remains usable preactivation. Preserve shared helpers used by new batch tests. |
| **R12-E — runtime consumer preparation** | `internal/agent/{coordinator_reaction_source.go,coordinator_drain_policy.go,turn_arbiter.go,drain_attempt.go,agent_notice_pull.go,coordinator_wake.go,coordinator_work_scope.go,coordinator_recheck.go,coordinator_rate_limit.go,coordinator_reaction_chain.go,supervision.go}` and listed tests; new `batch_cutover_facts*.go`, `batch_cutover_await_test.go`. No R11G-owned files or coordinator construction edits. | **Merge after R11G**; L, debt/scope then delivery/reaction commits | Real Runner + SQLite AW-a/AW-b; selected debt, reactions/failure markers, held questions, wake/max_wait breakers, scope and admission under held termination. No duplicate completion authority. |
| **R12-F — app/SDK and rerun preparation** | `internal/app/{app_accessors.go,app_session_activity.go,app_session_drivers.go,app_recovery.go,app_run_async_wait.go,app_run_quota.go,app_run_request.go,app_run_setup.go,app_batch_install.go,batch_install_factory.go}` and listed tests; `internal/agent/coordinator_rerun.go`, proposed `batch_cutover_rerun*.go`; `internal/server/handlers_agent_rerun.go` and **all manifest-listed coordinator interface fakes**; `sdk` manifest-listed tests/docs. No `coordinator.go` interface change yet unless kept additive. | D/E, R11G; L | Exact incarnation post-commit stop, stale completion isolation; quota finished-work export; activity without provider; SDK cancellation/custom-disk contract; real locked-host fault subprocess plus subsequent recovery; shutdown drain. |
| **R12-G — CI evidence** | New `.github/workflows/job-batches-shadow.yml`, explicitly listed flag-off test repairs, new D11 benchmark files under `internal/app`, proposed benchmark/report script. No production rewiring. Assign benchmark files separately from F’s tests. | Scaffold now; meaningful green after R11G and preparations; M | Full switched-ON suite including agent shards/catch-all, AW artifacts, three-OS host refusal and process tests, web checks; contended D11 comparison against pinned old baseline. |
| **R12-H — atomic authority cutover** | Sole owner of `internal/agent/{coordinator.go,coordinator_tools.go,coordinator_interrupt.go,agent.go,agent_options.go,agent_turn*.go,batch_allowed_controls.go,batch_install*.go,batch_exec*.go,batch_invocation.go,batch_ingress*.go}`, `internal/app/{app.go,app_agent_setup.go,app_lifecycle.go}`, `internal/config/config.go`; old ledger/wrapper/stall-job/model-tool deletion sets; associated inventories/prompts/tests; session old-job APIs/tests; SQL/query generation; **new embedded destructive migration**; current upgrade documentation. Use the A manifest as the closed file list. | A–G and all operator gates; **L unavoidable**, mostly prepared deletions/wiring | Default invocation is batch-only; no fallback even if environment says false; populated real `db.Connect` upgrade; old-host refusal; grep gate; no old SQL callers; fault/recovery/drain; full CI once. |
| **R12-I — switch erasure** | Only surviving flag parser/helpers/callers and their obsolete tests, as enumerated after H; prompt conditional and CLI disabled-message removal; no behavior change. | H green; S | Unset/true/false/garbage all have identical batch-only inventory/runtime; no runtime switch references outside historical docs/fixtures. |

**Important boundary:** H is the atomic destructive/authority boundary; I is the genuinely tiny switch-removal commit. Do not describe H as independently safe family deletions before its prerequisites are green.

## PARALLEL-NOW:

**PROPOSED, conflict-free with the supplied R11G file set:**

- Start **A, B, C and G’s scaffolding/benchmark work** now, in separate worktrees.
- D can start after A and D freeze ownership of `host_lock.go` versus retained-store files.
- E/F may be researched now, but their integration/acceptance must wait for R11G. They depend semantically on the ingress and invocation contracts even where their filenames are disjoint.
- H/I must wait. H necessarily intersects R11G’s `batch_install*`, `batch_exec*`, `batch_invocation.go`, `batch_ingress*`, `coordinator_subagents.go`, `batch_read*`, `batch_adapters*` footprint.

**A can be written and CI-proven before flipping anything — but not by immediately embedding the drop migration.** `db.Connect` applies pending embedded migrations independently of the runtime flag (`internal/db/connect.go:301`; `migrate_policy.go:158`). Embedding DDL early would destroy the default-off runtime’s table.

All `/wrush` worker prompts must explicitly override local verification conventions: **write/read/format only; no local tests or test-containing pre-push hook; CI supplies execution evidence.**

## FLIP-ORDER:

1. **Freeze the uncapped inventory and ownership manifest.** Include every production/test reference and its disposition/replacement test. Compare it against R10c’s original list, not only filename prefixes.
2. Merge A and preparation packages while preserving default-off functionality. Flag-ON branches read only nodes; neither branch mirrors terminal state into the other.
3. Merge R11G; rebase preparation work on its actual contracts.
4. Make the shadow workflow required and obtain **full-suite ON evidence** before H.
5. H atomically:
   - activates batch-only production behavior;
   - removes legacy dispatch/terminal readers and writers;
   - embeds guarded destructive DDL;
   - updates all inventories/prompts;
   - temporarily leaves flag helpers returning the sole batch behavior.
6. I deletes the now-meaningless switch and helpers.

**This is a necessary adjustment to “flip last”:** keeping a real default-OFF switch until after deleting the code it selects cannot yield independently shippable intermediate merges. The last *behavioral flip* belongs with destructive activation; the last *switch deletion* can be tiny.

### Model-tool removal

Replace `batchControlAvailability`’s old-name triggers **before deleting the factories**. Add native control names to config/role inventories, preserving scoped/reviewer/restricted availability. Update worker lists and progress classification, not only `allToolNames()`.

Update bash/run_command schema descriptions and return guidance, await/coder text, and prompt inventory tests in the same activation package. No `job_kill`/`job_output` aliases; stop answers remain intent/control facts, not fabricated settlement.

### Grep gate

**PROPOSED manifest generator:**

```bash
git grep -n -I -E \
 'async_jobs|AsyncJob|asyncJob|work_ledger|workLedger|WorkLedger|awaitSync|job_kill|job_output|stallDetach|stallDetacher|stall-|RUSH_BATCH_RUNTIME' \
 -- internal sdk web docs .github .githooks
```

Use this output to classify **every hit**. Final gate:

- zero executable old-table queries, generated old-job APIs, ledger authority, stall-job/controller references, or old model-tool inventory/prompt instructions;
- narrowly allowlisted historical migrations, upgrade/DDL fixtures and historical docs;
- retained `async_hosts` is **not** forbidden;
- no broad exclusion of tests, generated files, SDK or SQL;
- final I additionally rejects the switch outside explicit historical documentation.

Back this text gate with a real migrated database where `async_jobs` does not exist: CLI, server, maintenance, shutdown and model invocation tests must all execute.

### CI shadow workflow

**PROPOSED:** PR-triggered, no cron, `RUSH_BATCH_RUNTIME: "1"`, `CGO_ENABLED: "0"`, `GOEXPERIMENT: greenteagc`; isolated global config/data directories.

Reuse the full package partition and agent sharding from `.github/workflows/build.yml:146`, including its computed catch-all. Serialize heavy package runs with `-p 1 -parallel 1`; Windows heavy commands use the mandated memory wrapper. Do not restrict the job to batch-named tests.

**OBSERVED trap:** `TestBatchRuntimeFlagOffGolden`’s “unset” case does not unset an inherited ON environment (`internal/app/app_batch_install_test.go:77–86`). Repair that fixture explicitly; do not exclude flag-off tests from the shadow suite.

Existing normal CI uses `-short`. New AW/upgrade/fault acceptance must run explicitly without a short-mode skip; network/VCR exclusions do not justify skipping these hermetic acceptance cases.

## MIGRATION:

**OBSERVED:** retain `async_hosts`, host locks and `session_notices`; drop `async_jobs` without copying rows (`design.md:2211`; integration D2 at line 54).

**PROPOSED Up transaction:**

1. Dispose unreacted legacy **job-bound** session notices: set delivery void and wake zero; preserve unrelated wakes/notices and historical messages.
2. `DROP TABLE async_jobs;` — its table-owned indexes disappear with it.
3. Record the Goose migration only on successful commit.

No `INSERT … SELECT` into batch tables. Existing batch rows, control requests, messages, sessions, drivers and schedules remain intact.

Down recreates the **final pre-cutover empty schema**, including `bg_shell`, `claim_id`, `announce_message_id` and its indexes—not merely the original September schema.

**There is no data rollback after DDL.** Down restores table structure, not jobs, output, notice debt or execution identities. Operator instructions must say: stop every Rush host, disable automatic restarts during upgrade, make a consistent database backup, and deploy one binary generation. Restoring an old binary alone is not rollback.

### Gate placement and API

Gate immediately before applying the destructive migration, while the migration decision lock is held; **not in `App.New`**, which occurs after connection migration.

Probe **every `async_hosts` identity**, including hosts with no jobs. Do not use `ListDistinctRecoverableHostIDs`.

Use the existing semantics of `session.ProbeHostShared(dataDir, hostID)` / `ProbeHostLockShared` (`host_lock.go:231–269`):

- Dead only: proceed.
- Alive: refuse.
- Unknown or any probe/enumeration error: refuse.
- Own process host: refuse, not skip.
- Missing lock file is Dead; permission/I/O/unsupported-lock errors are Unknown.
- Message identifies blocking hosts and instructs the operator to stop other Rush processes.

**Import-cycle constraint:** db cannot import session, because session imports db. A should move the DB-independent probe primitive into `internal/filelock`, retaining session wrappers with identical semantics; migration uses that leaf API.

**Mixed-binary limit:** unknown-only versions open without migration (`migrate_policy.go:149`). This is not an old-binary fence. Also, old host registration is lazy: an old process that has not registered yet can race an upgrade. The host gate protects the registered, already-running old-host case; it does **not** prove an arbitrary rolling upgrade safe. Operational quiescence remains mandatory.

## OPERATOR-GATES:

### D11

**OBSERVED budget:** added p95 ≤15 ms per ordinary sequential call; eight-session aggregate throughput ≥80% of old path, measured with shared SQLite WAL writer contention (`integration.md:63`).

**PROPOSED evidence owner:** G’s benchmark author produces artifacts; orchestrator checks them; operator accepts budgets or explicitly accepts a breach. Hosted CI is a reproducible regression signal, **not a substitute for the specified local SQLite environment**.

Benchmark old and candidate revisions on the same CI runner, fresh equivalent populated databases, same WAL/synchronous configuration and mock provider/leaf workload. Include:

- eight sessions sharing a database and contending writers;
- sequential calls within each session;
- full Submit/Started/Settled persistence and elected delivery;
- latency samples/p95, calls/s, errors, contention settings and commit counts;
- alternating baseline/candidate runs, warmup and repeated measurements.

Pass both numerical bounds with no errors, disabled durability or reduced commit count. Record the local-target measurement too, or obtain explicit operator approval to change the measurement environment.

### AW-a / AW-b

**OBSERVED contracts:** `design.md:1868–1907`.

- **AW-a:** a NotifyAll child settlement does not unblock the worker or create selected completion debt; root settlement unblocks the worker **before held delivery**. Top-level ANY wakes only on root debt; ALL remains asleep with another live root; a child question breaks sleep without settling it.
- **AW-b:** Stop acceptance and successful Stop return leave R live and B Pending. Only confirmed completion frees admission. Worker ALL captures R, not later B; top-level 4b still includes B and waits despite independent held D debt. Elected R completion is consumed once; D yields one delivery. Include the separate debt-free variant.

Execute these through **real Runner → commit hook → SQLite → production await/arbiter/delivery** with controlled executor channels and bounded fences. Archive logs and asserted database/node/history snapshots. Static declarations, fake-row mutation tests and batchlab-only runs are insufficient.

### Genuine operator decisions

- Accept/change D11 budget or waive a measured breach.
- Authorize the irreversible upgrade window and supervisor behavior.
- Decide any intentional SDK capability loss, especially custom-disk/library mode, rather than silently breaking existing tests.
- Decide CLI output-schema changes where old `async_jobs_*` JSON names disappear.

D6 removal and zero row conversion are already selected decisions; no need to reopen them.

## RISKS:

| Risk | Pinning acceptance |
|---|---|
| Embedded DDL lands before old-path removal | A uses testdata only; H tests default startup through real `db.Connect` and then all consumers against missing old table |
| Gate runs too late / ignores empty hosts / treats Unknown as Dead | Actual old-host subprocess, host with zero jobs, deterministic Unknown path, schema/version unchanged on refusal |
| False confidence from migration-version policy | Old binary unknown-only fixture; explicit unsupported mixed-binary documentation |
| Host teardown still queries dropped table | Register/driver/normal Close/CloseKeepLock/recovery/retention after DDL; sqlc generation drift check |
| Deleting legacy names removes replacement controls | Default/worker/reviewer/scoped/disabled/restricted inventory tests; detached execution inspected and stopped via native controls |
| Stop settles early or releases admission | AW-b with held termination and queued B; actual process Wait on Windows and Unix |
| Selected debt/reaction semantics lost | AW-a, finish-before-ack, inline election, failed history insert, reaction failure marker and question breaker |
| Rerun reaches a new incarnation | Commit truncation, delay Stop/report, create new incarnation, release old report; assert new root unchanged |
| Root completion disappears from web | Complete root, reload browser, structured outcome appears once; no prose parsing and no legacy SQL |
| Fault test mistakes file existence for lock retention | Existing subprocess test only creates an ordinary file (`app_batch_subprocess_test.go:36`); new test must hold a real host OS lock, probe it before exit, then sweep real persisted nodes afterward |
| Shutdown releases DB/identity on agent-idle rather than Runner drain | Held executor/report/disposal; drain timeout retains writer/lock; late report commits; graceful drain releases in order |
| Test deletion erases behavior coverage | Manifest maps every removed scenario to a replacement or a deliberately obsolete contract; CI mutation/revert checks for critical guards |
| SDK silently regresses | Full SDK suite ON, library/application mode, scoped/custom-disk, cancellation and subprocess fatal-exit evidence |
| Review series expands indefinitely | Fix reproducible P0/P1; backlog P2/P3; end when none remain |

