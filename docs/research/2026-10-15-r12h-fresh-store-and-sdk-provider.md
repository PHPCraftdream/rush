# R12-H revised under operator decisions D1-D4 + SDK DiskProvider design (strongest-reviewer pass, 2026-10-15)

Operator decisions (2026-10-15): D1 D11 budget accepted (p95 +15 ms per call, 8-session throughput >= 80%), never hide a slowdown; D2+D4 no release yet: one fresh squashed schema in a new file rush_store_v1.db, old rush.db left untouched (no DROP), its removal planned a couple of releases later, CLI JSON async_jobs_* may be renamed; D3 SDK custom DiskProvider must be supported under the batch runtime (no blanket refusal).

Source: read-only review jb-r12h-plan against origin/ci/job-batches at d5c3b0d9. Supersedes the destructive-migration parts of docs/research/2026-10-14-r12-cutover-decomposition.md.

# SECTION 1 — Revised R12-H/R12-I plan under D1–D4

## 1. Review scope and revised decision

**Evidence labels:** **OBSERVED** means inspected source or repository state, not executed behavior. **PROPOSED** means the recommended implementation. **UNKNOWN** identifies evidence not obtained in this read-only review.

No edits, git writes, tests, builds, or benchmarks were performed. The working tree was clean at the final status inspection.

**The revised cutover is a fresh-store activation, not a destructive upgrade.**

The new binary must:

1. Open `rush_store_v1.db`.
2. Initialize it from one fresh, batch-only Goose migration.
3. Never open `rush.db` for migration, copying, checkpointing, compaction, or cleanup.
4. Use the batch runtime as its only execution authority.
5. Support caller-supplied SDK `DiskProvider` objects as described in Section 2.

This supersedes the destructive-migration portions of:

- `docs/research/2026-10-14-r12-cutover-decomposition.md:151`
- `docs/plans/2026-10-12-job-batches-rush-integration.md:54`
- `docs/plans/2026-10-12-job-batches-rush-integration.md:105`

It does **not** weaken write-ahead projection, confirmed termination, fatal commit-failure handling, incarnation isolation, or recovery.

## 2. Fresh schema: table-by-table disposition

### 2.1 Keep list

**PROPOSED:** preserve the final definitions needed by these consumers, including their constraints, foreign keys, indexes, defaults, and triggers. Do not merely concatenate historical migrations: write the final schema directly, without historical `ALTER`, conversion, deduplication, or table-rebuild steps.

“Batch-only” does not mean deleting the session turn queue, inject queue, permissions, or file history. Those are not competing work-tool terminal authorities.

| Table | Disposition and purpose | Inspected reader/writer anchors |
|---|---|---|
| `sessions` | **Keep.** Session hierarchy, workspace identity, models, budgets, cancellation, todos, cost accounting and history ownership. | Creation: `internal/session/session_lifecycle.go:34`; fork: `internal/session/session_fork.go:116`; SQL: `internal/db/sql/sessions.sql:6`. |
| `messages` | **Keep.** Conversation history, full tool results, usage, summary/checkpoint/origin/notice metadata. Preserve message-count and timestamp triggers. | Writer: `internal/message/message.go:458`; fork writer: `internal/session/session_fork.go:197`; initial triggers: `internal/db/migrations/20250424200609_initial.sql:61`. |
| `files` | **Keep.** Versioned file-history snapshots; unrelated to ledger execution state. | Writer: `internal/history/file.go:108`; readers: `internal/history/file.go:144`, `:152`, `:163`; SQL: `internal/db/sql/files.sql:25`. |
| `read_files` | **Keep.** Read-before-write/file-tracking state. | Writer: `internal/filetracker/service.go:39`; readers: `:50`, `:83`; SQL: `internal/db/sql/read_files.sql:1`. |
| `session_permissions` | **Keep.** Persisted permission rules, including enabled state and the uniqueness index required by `ON CONFLICT`. | Writer: `internal/permission/permission.go:412`; reader: `:928`; SQL: `internal/db/sql/permissions.sql:7`, `:15`, `:25`. |
| `queue_tasks` | **Keep.** Operator `rush queue` workload queue; it is not `async_jobs`. Preserve workspace ownership. | Insert: `internal/queue/queue.go:129`; reads: `:148`, `:183`, `:211`; claim/result writes: `:239`, `:252`. |
| `pending_injects` | **Keep.** Cross-process session input transport. | Raw SQL writes: `internal/session/session_inject.go:48`, `:285`; reads/deletes: `:76`, `:110`; generated-query explanation: `internal/db/sql/pending_injects.sql:4`. |
| `session_run_queue` | **Keep.** Durable scheduling of session turns, not a work-tool completion authority. | Enqueue: `internal/session/session_runqueue.go:300`; pending reader: `:517`; pump: `internal/session/run_queue_lifecycle.go:198`. |
| `orphan_call_outbox` | **Keep.** Transactional handoff fallback for session calls. | Write/read: `internal/session/session_orphan_outbox.go:22`, `:39`; atomic handoff into run queue: `:92`, `:106`; drain: `internal/session/run_queue_orphan_drain.go:25`. |
| `async_hosts` | **Keep under its existing name.** Host bookkeeping remains useful; the name does not make it legacy execution authority. No gratuitous host-table rename is required. | Retained insert/read/delete SQL: `internal/db/sql/async_hosts.sql:17`, `:22`, `:50`; retained implementation: `internal/session/host_store.go:39`, `:88`, `:121`. |
| `session_drivers` | **Keep.** Prevents competing reaction drivers for a session. | Retained claim/release/read: `internal/session/host_store.go:177`, `:190`, `:199`; liveness policy: `internal/session/session_driver.go:75`. |
| `session_notices` | **Keep.** Wakes, supervision, held questions and reaction markers. Batch completion outboxes stay on nodes. | Pull/read/CAS: `internal/session/session_notice_store.go:145`, `:149`, `:176`; SQL insert: `internal/db/sql/session_notices.sql:6`; wake occurrence transport: `internal/session/wake_schedule_fire.go:55`. |
| `wake_schedules` | **Keep.** Durable once/loop schedules and leases. | Store: `internal/session/wake_schedule_store.go:65`, `:89`; transactional firing/read/update: `internal/session/wake_schedule_fire.go:62`, `:70`, `:89`. |
| `batch_nodes` | **Keep.** Sole durable projection of work-tree identity, outcomes, delivery and reaction debt. | Reservation/projection: `internal/session/batch_store.go:79`, `internal/session/batch_commit.go:89`, `:244`; live notice-bound read: `internal/session/session_notice_store.go:93`; recovery: `internal/session/batch_recovery.go:104`. |
| `batch_control_requests` | **Keep.** Cross-process Stop intent and answers, never terminal authority. | Control pump: `internal/session/batch_control.go:137`; recovery closure: `internal/session/batch_recovery.go:119`; recovery/retention SQL: `internal/db/sql/batch_recovery.sql:39`, `:52`. |
| `batch_runner_checkpoints` | **Keep.** Rootless Runner transition sequence/final checkpoint, fenced by host and Runner incarnation. | SQL upsert: `internal/db/sql/batch_commit.sql:13`; reservation-side updates: `internal/session/batch_store.go:116`, `:195`; final schema definition: `internal/db/migrations/20261012000002_batch_runner_incarnation.sql:10`. |
| `goose_db_version` | **Keep as Goose-managed infrastructure.** Do not duplicate its creation in application schema SQL. | Provider/version reconciliation: `internal/db/migrate_policy.go:142–161`. |
| SQLite internal tables | **Engine-managed only.** For example, `sqlite_sequence` if retained definitions require it. | Do not create application copies. |

Important details:

- Fold `runner_id` and `response_envelope` directly into `batch_nodes`, and `runner_id` directly into `batch_control_requests`; they currently arrive through `internal/db/migrations/20261012000002_batch_runner_incarnation.sql:7–9`.
- Preserve the composite node identity constraints, live attach uniqueness, delivery-state checks and indexes from `internal/db/migrations/20261012000001_batch_projection.sql:64–95`.
- Preserve final workspace/model/cost/usage columns, not just the initial session/message schema. The generated final shapes are visible in `internal/db/models.go:143–173` and `:220–259`.
- Keep `session_notices.job_tool_call_id` for H unless its batch-bound replacement is changed coherently. It is **still used**, despite its name: `internal/session/session_notice_store.go:41–45`, `:92–106`. Removing it solely because it says “job” would break retained notices.
- Retain only indexes/triggers applicable to retained tables. Do not carry obsolete job indexes or data-cleanup migrations.

**Schema/query mismatch to resolve explicitly:** `internal/db/sql/files.sql:58–61` contains a `ListNewFiles` query against `is_new`, while the inspected `File` model has no such field (`internal/db/models.go:133–141`). Its live use was not established here. Before finalizing the squash, classify that query: remove it if unused, or establish its intended schema. Do not invent an `is_new` column merely to preserve an unexplained query.

### 2.2 Omit list

“Omit” means **never create in the new database**, not `DROP` from the old database.

| Object | Disposition |
|---|---|
| `async_jobs` | Omit entirely, including its indexes, constraints, generated API and all Claim/Transition/read/recovery/retention callers. |
| `async_jobs_new`, `async_jobs_old_schema` | Omit. Historical table-rebuild intermediates, not product tables. |
| Work-ledger terminal state | Delete its Go authority; no replacement ledger table. |
| Stall-job identities/controllers | Delete their separate authority; no stall-job table. Preserve watchdog/stall timing and progress policy where still needed. |
| Historical conversion/dedup/rename SQL | Omit from the new embedded lineage. Directly create final definitions instead. |
| R12-A destructive-drop SQL | Do not embed it. Remove the obsolete destructive-cutover fixture/tests or replace their acceptance with fresh-store isolation tests. |

**OBSERVED:** no distinct SQL ledger/stall-job table appeared in the inspected migration creation inventory. Those authorities are primarily Go structures plus `async_jobs`.

## 3. Goose, sqlc, connection policy and the live-host gate

### 3.1 One embedded migration

**PROPOSED:**

- Replace `internal/db/migrations/*.sql` with one new initial migration, for example `20261015000001_store_v1_initial.sql`.
- Its `Up` creates only the retained final schema.
- Its `Down` removes that new schema in foreign-key-safe order. It is not an old-store rollback or conversion.
- Keep the existing embed mechanism at `internal/db/connect.go:24` and migration filesystem construction at `internal/db/migrate_policy.go:125`.
- Do not reuse a historical migration version for the fresh initial schema.
- Existing development **batch rows in `rush.db` are also abandoned**. D2+D4 do not preserve preparation-era batch data.

Historical migration tests such as slot renaming and run-cost conversion cannot remain dependent on embedded files that no longer exist. Replace them with final-schema/API contract tests where their behavior remains relevant. Preserve historical fixtures only outside the embedded migration directory if they still prove a deliberate contract.

### 3.2 sqlc

`sqlc.yaml:4–5` already points to the migrations directory and `internal/db/sql`. That configuration can remain unchanged if those locations remain unchanged.

The schema/query/generation change is one dependency-closed change:

1. Remove `internal/db/sql/async_jobs.sql`.
2. Retain `internal/db/sql/async_hosts.sql`, already separated by R12-D.
3. Remove queries whose only purpose is old job authority, including obsolete notice-repoint operations.
4. Keep retained notice, driver, wake, history, queue and batch queries.
5. Regenerate all affected `*.sql.go`, `models.go`, `querier.go` and `db.go`.
6. Delete stale generated files for removed query inputs.
7. Check generated Prepare/Close/WithTx wiring, not just generated models.

The raw SQL consumers in `internal/queue` and `internal/session/session_inject.go` make a sqlc-only schema audit insufficient.

### 3.3 Filename and connection pool

There is currently **no single filename definition**. Production connection lifecycle hardcodes it three times:

- Open/pool key: `internal/db/connect.go:235`
- Release path: `internal/db/connect.go:388`
- Release-all path: `internal/db/connect.go:467`

**PROPOSED:** introduce one leaf-level store filename/path definition usable by DB, config discovery and CLI without import cycles. Update all three lifecycle paths together. Otherwise opening the new file while releasing the old key leaks pooled handles.

Retain one writer connection, read-pool ordering, WAL and synchronous settings: `internal/db/connect.go:13–21`, `:282`, `:303`, `:323`.

### 3.4 MayMigrate and PreMigrationChecks

- **Keep `MayMigrate`.** A dev binary prohibited from changing a shared store should still be prohibited from initializing or upgrading the new shared store. A fresh filename is not permission to bypass that policy.
- Retain migration decision serialization for concurrent **new** processes initializing the same database.
- Prefer a store-specific migration lock such as `rush_store_v1.migrate.lock`; the existing shared `migrate.lock` is at `internal/db/connect.go:302`. Sharing the old lock is conservative but creates needless cross-generation blocking.
- Keep generic `PreMigrationChecks` only if they have an actual continuing caller/contract. Otherwise remove the now-unused R12-A extension and tests as obsolete preparation, rather than carrying an unused cutover hook indefinitely.
- Do not conflate existing version-policy cleanup with H. The unknown-only branch currently permits opening without applying migrations (`internal/db/migrate_policy.go:166–176`); that is not a schema compatibility guarantee.

### 3.5 Is R12-A’s old-host gate still needed?

**No—not for this cutover’s schema safety.**

`CheckLiveHosts` enumerates `async_hosts` from the connection supplied to it (`internal/db/migrate_live_hosts.go:30–31`). On a fresh store before initial creation, that table does not exist. Reading the old database to recover the old gate would introduce exactly the old-file dependency D2+D4 removed.

**PROPOSED:** remove the destructive-cutover gate and replace its acceptance with:

- a live old-format host does not cause new-store initialization to query or modify `rush.db`;
- old DB and sidecars remain unchanged by the new binary;
- concurrent new initializers create one coherent schema;
- ordinary batch recovery still refuses Alive/Unknown hosts.

The **runtime dead-host probe remains essential**. It is independent of migration safety: `internal/session/batch_recovery.go:25–35`.

Also, do not retain the claim that holding `migrate.lock` prevents arbitrary host registration. The original decomposition already identified lazy registration and mixed-binary limitations (`docs/research/2026-10-14-r12-cutover-decomposition.md:184`).

## 4. Mixed old/new binaries: shared artifacts

**Separate database filenames prevent cross-generation SQL corruption. They do not make a shared data directory operationally isolated.**

| Artifact | Assessment and H treatment |
|---|---|
| `rush.db`, its `-wal`, `-shm`, journal | **Safe only because the new binary leaves them alone.** No automatic read, migration, checkpoint, rename or deletion. Old processes may still change their own files. |
| `rush_store_v1.db` and sidecars | Separate new lineage. Never seed it by renaming or copying an old DB. |
| `migrate.lock` | Current common path is conservative but unnecessarily coupled. Use a new-store-specific lock. |
| `hosts/<uuid>.lock` | Existing path: `internal/session/host_lock.go:65–71`. UUID identities make ordinary per-host ownership separable, but directory-wide maintenance can see both generations. Keep dead-only probing; never infer “not in my DB” means removable. |
| `locks/session-<id>.lock`, `.pid` and associated sidecars | Shared namespace: `internal/session/lock_inspect.go:44`; lock creation: `internal/session/lock.go:302–306`; PID-sidecar ordering is documented at `:414–421`. Same session ID in separate databases can block each other. Lock listings/kill/reap can observe foreign-generation processes. **Not isolated.** |
| `locks/session-<id>.childgroups` | Unix child-process registry: `internal/session/childgroup_registry_unix.go:109`. Same collision/management concern as session locks. |
| `queue.lock`, `queue-<workspace-hash>.lock` | Names are independent of DB generation (`internal/cmd/queue.go:36–39`). They can block queue runners that now have separate queues. Conservative exclusion, not corruption, but not transparent coexistence. |
| Workspace `rush.json` and `.lock` | Still shared configuration. Changing models, permissions or MCP definitions affects both generations. A filename change does not isolate this. |
| Global configuration and global managed data | Two distinct locations: `internal/config/load_paths.go:16`, `:56`. Both must be isolated in acceptance subprocesses. Cross-version write compatibility is **UNKNOWN**; do not promise it. |
| Shared-mode/worktree markers | Affect data-dir selection. Preserve old-file recognition as a location hint while adding recognition of the new filename. Never claim that selecting the same directory preserves history. |
| `logs/rush.log` | Common configured path: `internal/config/store.go:714`. Shared logs mix generations. Compatibility with arbitrary old log writers/pruners is **UNKNOWN**. Not a DB safety fence. |
| `attachments/` | Shared path: `internal/server/handlers_agent.go:148`. Ordinary unique creation is distinct from cleanup safety; cross-generation cleanup safety is **UNKNOWN**. Do not purge attachments merely because the fresh DB has no references. |
| `projects.json` and lock | Shared directory registry: `internal/projects/projects.go:18`, `:115`. It identifies data directories, not DB generations. Audit must select the new filename explicitly. |
| Commands/init, skills, prompts, `.rush/stdin`, fetch scratch/cache and provider files | DB rename does not isolate these. Read-only use can coexist; concurrent installation, cleanup or configuration mutation is not established safe here. |

**PROPOSED operating contract for H:** stop old Rush processes and automatic restarts before switching production use. This is now an operational shared-artifact rule, **not a prerequisite to destructive DDL**.

Do not claim rolling mixed-generation operation is supported. Full side-by-side support would require version-scoping process-control namespaces and auditing every direct CLI path; that is a separate, explicit feature, not something the removed migration gate can provide.

## 5. `rush.db` hardcode inventory

### Production paths

| Location | Required disposition |
|---|---|
| `internal/db/connect.go:235`, `:388`, `:467` | Change together to central new-store path. |
| `internal/config/datadir.go:206` | Recognize new-store presence **and retain old-file detection as a directory-selection compatibility hint**. Do not open/migrate the old file. |
| `internal/cmd/sessions_compact.go:124`, `:407` | New filename in path and output. Update help at `:27–32`. |
| `internal/cmd/sessions_audit_db.go:69`, `:75` | Audit only the new store; absence must remain read-only and must not create one. |
| `internal/cmd/sessions_audit.go:206` | Worktree/project enumeration checks the new store. Update help at `:74`, `:89`, `:115`. |
| `internal/cmd/sessions_audit_report.go:61` | New report heading. |
| `internal/cmd/sessions.go:33`, `internal/cmd/root.go:55` | Correct current help and directory-selection explanation. |
| `internal/cmd/migrate_artifacts.go:32`; `internal/cmd/migrate.go:59` | **Legacy compatibility exception, not a new-store path.** Never change `crush.db → rush.db` into `crush.db → rush_store_v1.db`: that would bypass the fresh-schema decision. Keep the old migration command explicitly historical, with truthful warning/help. Removing that support is deferred. |
| `internal/cmd/claude_wrush_command.md:56` | Replace “keeps using it” with the new-store/history consequence and actual directory-selection rule. |

**OBSERVED:** no `rush.db` content hits were returned under `internal/server`, `internal/app`, `web`, `testdata`, `npm`, `scripts`, `tools`, `.github`, `.githooks`, or `.agents` in the scoped searches. The server inherits the filename through DB/app wiring.

### Tests with actual path/sidecar dependencies

- SDK: `sdk/library_mode_test.go:144`.
- Session: `session_driver_test.go:36`, `notice_pull_test.go:59`, `batch_recovery_test.go:24`, `batch_pull_test.go:75`, `batch_history_reset_test.go:49`, `async_job_store_test.go:393`, `:443`, `async_job_recovery_test.go:430`.
- DB: `connect_pool_test.go:25`; `compact_test.go:101`, `:148`, `:177`; `migrate_policy_test.go:86`, `:241`, `:270`, `:321`; numerous destructive-gate fixture paths in `migrate_live_hosts_test.go`.
- Config: `datadir_test.go:151`, `:168`, `:207`, `:478`.
- CLI: `sessions_compact_test.go:114`, `:126`; `sessions_audit_test.go:67`, `:184`, `:194`, `:249`, `:293`, `:342`, `:451`; `sessions_audit_rules_test.go:100`; `sessions_audit_flags_test.go:91`, `:100`; `sessions_audit_help_test.go:27`; `queue_p1_3_test.go:230`, `:354`.
- Agent legacy authority: `work_ledger_durable_test.go:38`, `:59`.

Port retained tests to the central new-store path; delete obsolete authority tests only with scenario disposition/replacement. Legacy `migrate_test.go` and `migrate_merge_test.go` retain old filenames deliberately and must assert that old artifacts never become the new store.

Additional comment-only references occur in SDK library-mode documentation and agent/CLI cleanup tests. Update current contract comments; historical regression descriptions need not be rewritten mechanically.

### Documentation

Current user-facing locations include:

- `README.md:147`, `:966`, `:1016`
- `internal/cmd/claude_wrush_command.md:56`
- Current changelog explanations at `CHANGELOG.md:144`, `:149`, `:259`, `:309`

Add a new cutover entry rather than falsifying historical release notes. Historical shared-data-dir/vacuum plans, checkpoints and reviews should remain historical, with a supersession note where necessary.

**Completeness boundary:** the tools respect ignore rules and searches have output caps. The scoped executable-path searches above completed without truncation; this is not certification of every ignored/untracked file. H must produce an uncapped tracked-file filename inventory in addition to refreshing the old-path manifest.

## 6. Ordered H work: smallest honest green boundaries

The existing manifest is an input, not a deletion command. It records **148 unclassified files** (`docs/research/2026-10-14-r12-old-path-manifest.md:32`). It also classifies `batch_cli.go` as a switch-family deletion target (`:347`), although only its disabled guard should disappear—not the batch CLI.

**PROPOSED commit sequence:**

| Commit | File-level work | CI acceptance |
|---|---|---|
| **H0 — Freeze revised contracts and inventory** | Update the two R12 research documents and integration R12 text; regenerate/classify the manifest with A–G changes included. Map each removed test to replacement or obsolete contract. Record filename exceptions. | Inventory reproducibility; no production behavior change. |
| **H1 — Native batch controls and shared-helper extraction** | `batch_allowed_controls.go`, `coordinator_tools.go`, `coordinator_tools_reviewer.go`, `config.go`, relevant batch classification/inventory files; move genuinely shared inline/timeout/question/retention helpers out of deletion targets such as `work_ledger_inline.go` and `async_job_gc.go`. Keep old runtime functional until activation. | Default/worker/reviewer/scoped/restricted inventory tests in both preparation modes; no old-name dependency for native controls. |
| **H2 — SDK provider support** | Section 2’s dependency-closed SDK/agent/app patch, replacing refusal tests with supported-runtime tests. | Full SDK and relevant ingress/executor/ownership/recovery acceptance with batch mode enabled. This is a prerequisite, not post-cutover repair. |
| **H3 — Fresh-store preparation without activation** | Add central filename/path support and nonembedded final-schema fixture; prepare filename/isolation tests and migration-policy fixtures. Do not replace the active embedded schema or select the new file yet. | Fresh-schema fixture and SQL/API coverage; legacy default remains shippable. |
| **H4 — Atomic batch-only/fresh-store activation** | Select new filename; replace embedded migrations; remove old SQL and regenerate; wire `HostStore`/`SessionNoticeStore`; make app/coordinator/runtime consumers batch-only; remove the legacy production dependency closure and dependent tests; update prompt/tool guidance together. | Default startup, no old table, no fallback under false/unset environment; old-file isolation; all retained CLI/server/SDK consumers execute against the fresh schema. |
| **H5 — Remaining dormant surface/fixture cleanup** | Remove obsolete legacy wire/prose-completion implementation, flag-off fixtures and historical authority-only helpers that no longer have production reachability. Update CLI JSON and docs if not already required by H4. | Web reload/once-completion; CLI schema snapshots; refreshed zero-authority grep gate. |
| **H6 — Final evidence and benchmark harness transition** | Adapt G’s baseline mechanism so it survives deletion of the old runtime; retain report tooling and explicit benchmark CI invocation. | Required normal CI, shadow evidence, AW artifacts, D11 local and CI comparison, fault/drain/recovery evidence. |

**H4’s dependency closure cannot honestly be split into independent green “drop table first / delete callers later” merges.** Removing generated APIs while their consumers compile, or deleting legacy helpers still used by batch code, forces those edits into the same activation commit unless earlier preparation has genuinely removed the dependency.

H4’s concrete ownership includes:

- `internal/app/{app.go,app_agent_setup.go,app_lifecycle.go,app_batch_install.go}` and remaining prepared fallback branches.
- `internal/agent/{coordinator.go,coordinator_tools.go,coordinator_interrupt.go,agent.go,agent_options.go,agent_turn*.go}` and remaining ledger consumer families.
- `work_ledger*.go`, `work_job.go`, `async_tool.go`, `bgshell_claim.go`, and the legacy portions of `delegation_question.go`.
- Stall-detach authority and `tools/stall_detach_controller.go`, **not wholesale watchdog/progress deletion**.
- `tools/job_{kill,kill_bgshell,output,output_pending,shell_resolver}*` and associated descriptions.
- `internal/session/async_job_*` authority, legacy halves of notices/drivers/recovery, and their classified tests.
- `internal/db/migrations`, `internal/db/sql`, generated DB files.
- Current tool descriptions/templates: bash, run_command, await, coder, prompt inventories and suggestions.
- CLI/server consumer branches that still mention/query old rows.

### CLI JSON

Replace misleading compatibility names with explicit node semantics:

- `async_jobs_total` → `batch_tasks_total`
- `async_jobs_running` → `batch_tasks_active`

Their current substitution of task counts is visible at `internal/cmd/sessions_show.go:132–139`. “Active” is preferable to “running” if counts include starting/cancelling.

Remove `legacy_async_jobs_absent` and the obsolete legacy-rule applicability branch from the current audit output (`internal/cmd/sessions_batch_audit.go:112–129`). An absent `async_jobs` table is now the required schema, not an exceptional audit condition. Retain distinct live, pending, held, debt and unknown facts.

### Parallel worktrees

Conflict-free only with explicit ownership:

- **Schema/DB lane:** `internal/db/**`, `sqlc.yaml`, new DB fixtures.
- **SDK lane:** `sdk/**`, provider-binding files, designated `batch_install*`/`batch_exec*`/`batch_ingress*`, designated app run-override files.
- **CLI lane:** sessions commands/audit/JSON/tests, excluding any separately owned central path definition.
- **Web/server observation lane:** batch/livework protocol/store/rendering files; no rerun ownership overlap.
- **Documentation/manifest lane:** research/plan/current docs and generator ownership.

The SDK lane overlaps H1/H4 in `coordinator_tools.go`; assign that file one owner and integrate serially. App composition and SQL generation also need sole owners. Independent implementation can proceed in parallel; the final activation and heavy verification are serialized.

## 7. D11: accepted budget, not accepted slowdown

**D1 makes the numerical bounds mandatory:**

- added p95 ≤15 ms per ordinary sequential call;
- eight-session throughput ≥80% of baseline;
- no errors, durability bypass, omitted transition commits, or concealed slowdown.

**OBSERVED source-level evidence gap in G’s harness:**

- The “old” call is direct `leaf.Run`, not the pinned old production invocation path: `internal/app/app_batch_bench_helpers_test.go:267–272`.
- The reported “commit count” counts completed task rows, not Submit/Started/Settled transactions: `:287–295`.
- Latency samples run sequentially before the eight-session throughput phase: `:331–355`. This does not establish p95 under the specified concurrent writer contention.
- Assertions currently require only `commitsOn >= commitsOff`: `internal/app/app_batch_bench_test.go:113`.
- The shadow workflow is evidence-only and trigger-limited, not automatically required: `.github/workflows/job-batches-shadow.yml:5–19`.

These are reasons not to treat “G exists” as proof that D11 is satisfied. **No performance result was measured here.**

**PROPOSED:** before deleting the baseline path, pin a baseline revision and provide a hermetic baseline/candidate driver using equivalent populated databases and production invocation boundaries. Record actual transition/transaction accounting, durability, latency under contention, throughput, errors and both revisions.

A miss blocks cutover. Investigate code and preserve failing artifacts; do not raise the budget, switch the baseline, weaken persistence, or hide the result through benchmark skips.

## 8. What R12-I becomes

I remains a **behavior-preserving switch erasure**, not another activation:

- delete `internal/config/batch_runtime_flag*.go`;
- delete app/agent/server forwarding helpers;
- remove disabled guards/messages from `internal/cmd/batch_cli.go`, not the command;
- collapse prompt conditionals and obsolete flag-off tests;
- remove flag environment dependencies from current CI/tests;
- retain the baseline benchmark as a separately pinned revision, not as a hidden legacy runtime in production.

Unset, true, false and garbage flag values must all select identical batch-only behavior already after H4. I then removes the meaningless parser and branches.

Deletion of old `rush.db` files, old-directory detection or historical migration-command support is **not R12-I**. It remains the separate “couple of releases later” task.

## 9. Consequences and acceptance

There is **no import requirement** under the operator’s decision.

The consequence is plain: all old sessions, messages, todos, costs, queued turns, operator queue tasks, schedules and preparation-era batch history become invisible to the new binary. They remain in `rush.db`; they are neither migrated nor deleted. `--continue` cannot continue an old-store session through the new binary. Returning to the old binary sees the old store but not new-store activity.

The new binary must not silently treat an existing `rush_store_v1.db` as disposable. “Fresh start” describes first creation, not permission to reset an existing new store on every startup or schema error.

Acceptance must include:

1. Populated old DB and sidecars remain untouched while the new store starts empty.
2. New-store reopen preserves new sessions/history.
3. Read-only audit creates neither store when absent.
4. Compact/GC/purge affect only the new store and never clean old attachments by fresh-DB absence.
5. New schema has every retained table and no `async_jobs`.
6. CLI, server, maintenance, rerun, shutdown and SDK work against that schema.
7. AW-a/AW-b, actual host-lock recovery, held termination and commit-fault subprocess cases remain mandatory.
8. D11 passes its unchanged budget with adequate baseline evidence.

# SECTION 2 — SDK custom DiskProvider design under the batch runtime

## 1. Why it was refused—and why that refusal is now overbroad

**OBSERVED:** `DiskProvider` is an arbitrary Go interface, not serializable state (`internal/agent/tools/fs_provider.go:34`). It may close over a map, HTTP client, database connection or tenant-specific capability.

The original durable-turn refusal protects against a real failure mode:

1. Serialize a session call without its live provider.
2. Restart.
3. Rebuild with `DiskProvider == nil`.
4. Normalize nil to OS disk.
5. Execute intended virtual-filesystem operations on the real filesystem.

That mechanism is documented at `internal/agent/disk_provider_durable.go:6–17`; the durable rebuild guard is at `internal/agent/coordinator_run_queue_call.go:26–41`.

But the batch rollout expanded that protection into blanket refusal:

- Tool construction: `internal/agent/coordinator_tools.go:867–868`
- Invocation validation: `internal/agent/batch_invocation.go:156–157`
- Direct-call ingress: `internal/agent/batch_install_ingress.go:175–180`
- Explicit batch controls: `internal/agent/batch_model_tools.go:58–59`
- Executor routing: `internal/agent/batch_exec.go:90–91`

It even refuses explicit canonical OS disk because it tests presence rather than custom identity. The distinction already exists at `internal/agent/tools/fs_provider_os.go:40`, `:55`, `:62`.

**The key counter-evidence:** R9 does **not replay batch side effects after restart**. It interrupts dead-host live nodes and deletes unannounced roots:

- `internal/session/batch_recovery.go:74–75`, `:104–119`
- `internal/db/sql/batch_recovery.sql:18–37`
- Design requirement: `docs/plans/2026-10-07-job-batches-design.md:2360–2369`

The batch executor already depends on process-local pinned leaf registries. Those are maps of live tool objects, not reconstructible durable factories (`internal/agent/batch_install_adapters.go:312–352`).

Therefore **durable outcome projection does not require a durable executable Go object**, provided restart never re-executes it and future session turns cannot silently lose its required provider.

## 2. Realistic designs

| Design | Evaluation against invariants |
|---|---|
| **A. Process-local provider binding with durable batch projection; dead-host recovery interrupts without replay** | Fits existing R9. Preserves one Runner authority, ordinary delivery election, held questions and Stop semantics. Requires explicit lifetime ownership, binding identity, fail-closed lookup and protection against later session-turn rebuild falling back to OS disk. **Recommended.** |
| **B. Named provider registry re-registered at SDK Open** | Useful for future durable session-turn continuation. A stable name alone is insufficient: namespace/tenant/version, authority and policy must match. Missing registrations must block execution, never imply OS disk. Re-registration must not replay old batch leaves; R9 still interrupts them. More API and recovery-order complexity than required now. |
| **C. Sync-only custom-provider calls, no background escape** | Reduces lifetime exposure but is not a full durability solution: crashes can occur during sync execution, cancellation can return before termination, and held questions/delegations outlive a turn. It also removes useful explicit batch/detached behavior unnecessarily. |
| **D. Serialize provider descriptors and reconstruct through factories** | Viable only for opt-in providers with a defined descriptor/security contract. Cannot represent arbitrary existing Go implementations. Credentials and runtime handles must not be dumped into node payloads. Does not solve arbitrary caller-provider support by itself. |
| **E. Proxy custom disk through a separately durable service** | Can support reconnectable operations, but requires authentication, service lifetime, idempotency and remote termination limits. Excessive for H and changes deployment assumptions. |
| **F. Run custom disk outside batches** | Reject. Reintroduces a bypass and potentially another completion authority. Violates the selected universal work ingress. |

“Exactly once” must be stated precisely: one accepted execution/terminal publication per incarnation and transactional completion delivery—not guaranteed exactly-once external side effects across crashes. A provider may have committed an external write immediately before process death. Recovery reports interrupted/unknown execution outcome; it does not undo or replay the write.

## 3. Selected contract

**PROPOSED: choose A—process-bound execution dependency, durable state, no execution replay.**

Do not call it a non-durable batch if node identity, outcomes and notices remain durable. The **provider binding is process-local**; the batch projection remains fully durable.

### 3.1 Public SDK behavior

Keep `RunOverrides.DiskProvider` usable without new mandatory methods.

Document:

- The provider is borrowed from the caller; Rush does not close it.
- The caller must keep it usable until all associated executions and provider-held readers have drained, including detached work and held delegations.
- Concurrent explicit batches may invoke it concurrently. Providers must support that or synchronize internally.
- Cancellation is a request. Return from SDK waiting does not prove the provider operation stopped.
- Crash/restart never resumes old provider-backed work.
- New runs after reopening supply their provider explicitly.
- No missing binding ever falls back to OS disk.

Retain the existing safety floors:

- a provider requires folder scopes;
- a scope exposing real command tools alongside virtual disk is refused.

Those guards are at `internal/app/app_run_setup.go:324–344`. D3 does not authorize weakening them.

### 3.2 Binding identity

Use an app/client-owned immutable binding registry:

- opaque binding ID;
- owning app/runtime and host/Runner generation;
- original provider and pinned policy/toolset;
- explicit process-bound marker;
- references held by pending/admitted work and held scopes.

Persist only an opaque binding identity and requirement marker in trusted invocation metadata. Do not persist pointers, credentials or provider implementation state.

The binding is **not** selected by “latest provider for session.” Two calls using the same session but different providers must never cross-bind. Include the binding requirement/identity in canonical invocation identity so live attach with a different provider is rejected rather than silently sharing execution.

Do not use interface equality: valid provider values need not be comparable.

### 3.3 Lookup and lifecycle

- Registration occurs before submission.
- Executor lookup validates the required binding before any leaf side effect.
- Lookup failure produces an explicit binding-unavailable error, not a nil provider.
- Pending work retains the binding until it can no longer start.
- Accepted execution retains it through real termination and disposal, not just Stop acknowledgement.
- Detached delivery retains the same execution and binding.
- Held questions retain the binding across turns and Inject at the same incarnation/depth.
- Add uses the trusted pinned binding/policy. It cannot redirect an existing tree to a different caller’s disk.
- Registry entries/tool closures must not retain arbitrary SDK providers forever after work finishes. Existing pinned registries have no removal operation in the inspected implementation (`internal/agent/batch_install_adapters.go:316–352`); provider support needs an explicit bounded release strategy.

### 3.4 Session-turn recovery is a separate problem

Allowing batch execution must **not** indiscriminately remove the existing durable-session-call guards.

A provider-dependent session can otherwise survive in history while a queue pump, reaction or wake later rebuilds an ordinary turn with default OS tools.

**PROPOSED:**

- Mark provider-dependent turn/continuation state with the required process binding.
- While the binding lives, same-process continuations inherit the exact binding through trusted call state.
- Do not serialize that continuation as an ordinary replayable call.
- If a handoff cannot retain the binding, produce a typed terminal/nonretryable “provider binding lost” outcome; never strip the marker and enqueue.
- After restart, provider-dependent automatic continuation is blocked pending an explicit SDK run with a newly supplied provider.
- Generic CLI/server continuation must not silently clear the requirement.
- Keep the existing fail-fast session-busy behavior initially (`internal/app/app_run_setup.go:347–352`). Supporting durable queueing behind another turn is unnecessary for D3.

This is a narrow unavailable-dependency rule, not blanket refusal of the SDK run.

## 4. Invariant mapping

| Invariant | Selected design |
|---|---|
| One terminal authority | All work still enters the same Runner; provider registry supplies a capability, not terminal state. |
| Write-ahead boundary | Submit/Started/Settled persist normally. No fast path around commit hooks. |
| Exactly-once completion/delivery | Existing incarnation/token fences and elected sync/inline/outbox transactions remain authoritative. Binding cleanup cannot publish completion. |
| Recovery R9 | Dead-only host recovery writes `Interrupted / host_lost`; announced root gets one root notice; unannounced subtree is deleted; no provider reconstruction or execution replay. |
| Held questions | Binding survives turn-end and remains attached to the held execution; Inject does not create a new provider-bound batch. Restart interrupts, not resumes, the held scope. |
| Stop/admission | Stop cancels provider contexts. A provider ignoring cancellation keeps execution/admission live until actual return. Stop success is not terminal confirmation. |
| Retention | Node retention remains whole-root and debt-aware. Live provider references are released through execution lifecycle, not by deleting DB rows. |
| Fatal commit failure | Provider containment follows normal cancellation/disposal; host identity/writer remain retained until process exit. No SDK-only exception to exit policy. |
| Missing dependency | Explicit failure or blocked continuation, never default filesystem promotion. |

Preserve R9’s standard `host_lost` code rather than introducing a different provider-specific terminal-state machine. A more specific diagnostic may explain that the process-local binding was lost, without claiming that external side effects were rolled back.

## 5. File-level implementation plan

### Provider/API side

- `sdk/sdk.go`: document the supported process-bound provider contract and lifecycle.
- `sdk/README.md`: remove blanket batch-runtime refusal; explain detached work, cancellation, restart and explicit re-supply.
- `sdk/sdk_batch_cutover_test.go`: replace “must return ErrDiskProviderNotDurable” with successful supported execution and lifecycle cases.
- `sdk/sdk_disk_provider_test.go`, `r16_6_disk_provider_test.go`, library-mode/scoped tests: run the same capability expectations under the batch runtime.
- `testdata/embedder/embed_test.go`: update the external-consumer contract test if sentinel usage changes.

`ErrDiskProviderNotDurable` may remain narrowly for forbidden durable handoff/rebuild during this package. It must no longer classify an ordinary valid provider-backed batch invocation. A new public `ErrDiskProviderUnavailable`-style sentinel should identify lost bindings.

### Ingress/executor side

- New agent binding-registry/lifetime files: app-owned dependency, immutable registration, trusted resolution and release.
- `call_options.go`, `batch_invocation.go`: distinguish nil/default disk from a required process binding; remove unconditional rejection.
- `coordinator_tools.go`: remove `HasDiskProvider` refusal; pin the correct provider-backed tools and binding.
- `batch_install_ingress.go`, `batch_install_tools.go`: stamp/retain binding requirements at direct ingress.
- `batch_model_tools.go`, `batch_model_control.go`: support explicit nested batches and Add without losing provider identity.
- `batch_ingress.go`: incorporate binding identity in canonical submit/live-attach checks.
- `batch_exec.go`: replace blanket marked-payload refusal with required-binding validation.
- `batch_exec_generic.go`: execute the already pinned provider-backed leaf; no reconstruction with nil disk.
- `batch_exec_agent.go`, `batch_install_agent_driver.go`, private-fetch ingress and child call construction: propagate the binding through held/nested scopes.
- `batch_install_adapters.go`: bound registry/provider ownership and disposal; do not retain caller objects indefinitely.

### App/session/recovery side

- `internal/app/app_run_setup.go`, `app_run_request.go`: supported override contract and binding registration; retain folder/command safety floors and initial busy refusal.
- App batch install/factory/lifecycle: inject the registry, drain it after Runner completion, retain it on forced shutdown.
- `disk_provider_durable.go`, `coordinator_interrupt.go`, `agent_ownership.go`, `coordinator_run_queue_call.go`: separate “valid live process-bound execution” from “unavailable durable handoff.”
- Session call-data serialization/rebuild and automatic continuation producers: persist the requirement, never the object; fail closed if it is unavailable.
- `internal/session/batch_recovery.go` and recovery SQL: preserve no-replay R9 behavior; add provider-backed acceptance rather than a competing recovery path.
- H’s fresh schema: include any narrowly required session continuation marker. This is why the SDK contract must settle before final schema activation.

## 6. Acceptance tests

These are proposed tests, **not executed evidence**.

1. **Real supported SDK call:** mock model invokes scoped `fs_read` and `fs_write` against a caller provider; provider state changes, real disk does not; ordinary batch rows and elected response are present.
2. **Provider shapes:** explicit `OSDiskProvider`, non-comparable custom implementation, and defined typed-nil behavior.
3. **Isolation:** overlapping clients/runs with identical paths and different providers never cross-route.
4. **Attach identity:** matching binding/input attaches once; same live call ID with another binding is rejected without executing either extra write.
5. **Explicit batches:** sequential, parallel, nested and Add operations retain the intended binding; hooks/restrictions run once.
6. **Stop with held return:** provider observes cancellation but does not return; node remains live and admission held. Release return; settle once.
7. **Detached lifetime:** SDK wait returns while work remains live; caller binding remains retained until completion/disposal.
8. **Held question:** child fs operation, question, Inject, another fs operation—all use the same binding. Stop while held settles only after scope closure.
9. **Graceful/forced close:** graceful Close drains before releasing references; timeout retains writer/host/binding; no premature provider teardown.
10. **Crash after announced start:** child process holds a real host lock and blocks inside provider work; terminate it; restart without provider; sweep yields `host_lost`, one root recovery notice, no execution replay and no OS-disk writes.
11. **Crash before announcement:** unannounced subtree is deleted according to R9, not replayed.
12. **Crash after committed completion:** preserve completed outcome; no duplicate completion or provider invocation.
13. **Alive/Unknown recovery:** no provider-backed node is interrupted while the host probe is Alive or Unknown.
14. **Restarted continuation:** queued/automatic provider-dependent turn cannot rebuild with nil disk; explicit SDK re-supply is required for a new run.
15. **Retention and reference release:** pending delivery/debt protects rows; completed/disposed work releases live provider references independently of later durable retention.
16. **Commit-fault subprocess:** no outward publication for failed settlement, bounded containment, required process exit, then standard R9 recovery.
17. **External SDK consumer:** public types/errors remain usable without importing `internal/agent`.

**Final recommendation:** implement supported process-bound providers before H activation, retain durable projection and no-replay recovery, and remove only the blanket refusal—not the safeguards against silently rebuilding provider-dependent session work on real disk.

**Files changed:** none. **Tests/builds/benchmarks run:** none, as requested. **Notable:** D11 performance compliance, runtime acceptance, and mixed-generation shared-artifact compatibility remain unverified by execution.
