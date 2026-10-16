<!-- Read-only code map produced for the job-batches Rush integration plan. Anchors are as of commit 48890aaa; re-read the code before relying on a line number. OBSERVED = seen in source, INFERRED = integration conclusion, neither is executed verification. -->

# Area B — durable model, notices and recovery

**Read-only report.** No files changed; no build, test, vet, lint, hook or selftest commands run. **OBSERVED** below means inspected source/schema behavior, not runtime verification. **INFERRED/PROPOSED** identifies integration conclusions.

**Coverage:** Direct SQL/store consumers and the identified interface chains are mapped below. I cannot certify every arbitrarily distant transitive caller or every test fixture; that remaining completeness limit should not be mistaken for a clean cutover audit.

## 1. CURRENT STATE

### Schema, queries and generated boundary

**OBSERVED — `async_jobs`:** Its current definition is `internal/db/migrations/20261005000001_bg_shell_jobs.sql:17–46`.

| Group | Exact columns/gates |
|---|---|
| Identity | `owner_session_id TEXT NOT NULL`, FK to sessions with delete cascade; `tool_call_id TEXT NOT NULL`; composite primary key |
| Invocation | `kind TEXT NOT NULL`, CHECK `command/agent/fetch/bg_shell`; `tool_name TEXT NOT NULL DEFAULT ''`; `timeout_seconds INTEGER NOT NULL DEFAULT 0`; `input_hash TEXT NOT NULL`; nullable `child_session_id TEXT`; `origin_cli INTEGER NOT NULL DEFAULT 0` |
| Execution | `state TEXT NOT NULL`, CHECK `running/completed/failed/cancelled/timed_out/interrupted`; `notice_kind TEXT NOT NULL DEFAULT ''`; `host_id TEXT NOT NULL` |
| Announcement | `announced INTEGER NOT NULL DEFAULT 0`; nullable `announce_message_id TEXT` |
| Delivery | `delivery TEXT NOT NULL DEFAULT 'none'`, CHECK `none/pending/done/void`; nullable `notice_message_id TEXT` |
| Reaction | `wake/reacted/wake_attempts/reacted_failed INTEGER NOT NULL DEFAULT 0` |
| Result/deadline | nullable `deadline_at INTEGER`, `timeout_kind TEXT`, `result_summary TEXT`, `result_is_error INTEGER` |
| Incarnation/time | `claim_id TEXT NOT NULL DEFAULT ''`; `created_at/updated_at INTEGER NOT NULL` |

There is no Pending, Starting, Cancelling or Skipped state. Indexes cover host, owner-pending, running, unique running child session, announced reaction debt and terminal retention (`internal/db/migrations/20261005000001_bg_shell_jobs.sql:63–74`).

**OBSERVED — retained tables:**

- `async_hosts`: `id TEXT PRIMARY KEY`, `pid INTEGER NOT NULL`, `label TEXT NOT NULL DEFAULT ''`, `started_at INTEGER NOT NULL`; no job→host FK (`internal/db/migrations/20260928000002_add_async_phase4_core.sql:10–19`).
- `session_notices`: autoincrement integer ID; owner session FK/cascade; required kind/text; delivery four-value CHECK/default pending; wake/reacted/attempts/failed counters; nullable notice-message ID and `job_tool_call_id`; required timestamps. Its job binding is textual, not a foreign key (`internal/db/migrations/20260928000002_add_async_phase4_core.sql:120–154`).

**OBSERVED — generation:** SQLite migrations and query SQL feed sqlc; generated output includes models, prepared-query plumbing and Querier (`sqlc.yaml:1–14`; `internal/db/models.go:11–45`; `internal/db/db.go:13–60`). Removing only `AsyncJob` is insufficient.

### Current authority and delivery flow

**OBSERVED:** `AsyncJobStore` owns the writer, queries, lazy host registration and separate atomic read-query pointer. Its mutex is intentionally never held across writer I/O; registration uses a different mutex (`internal/session/async_job_store.go:160–196`).

Current flow:

1. **Claim:** transaction reserves a running, unannounced row; archives reusable keys, repoints notice bindings and generates a claim UUID (`internal/session/async_job_store.go:345–471`).
2. **Settlement:** DB-first terminal CAS writes state, cause, result, delivery, wake and reacted together; checks running plus claim ID; preserves void. Losing writers adopt the stored winner (`internal/db/sql/async_jobs.sql:158–193`; `internal/session/async_job_store.go:474–558`).
3. **Started acknowledgement:** history insertion, announced flag and announcing-message identity commit together (`internal/session/notice_pull.go:329–376`).
4. **Notice pull:** pending+announced CAS, history insertion and notice-message identity commit in one transaction; publication follows commit. Failed pulls remain retryable (`internal/session/notice_pull.go:105–170`; `internal/db/sql/async_jobs.sql:237–259`).
5. **Inline-close:** result history, acknowledgement and terminal delivery consumption commit together; delivery becomes done, wake=0, reacted=1 (`internal/session/notice_pull.go:443–502`; `internal/db/sql/async_jobs.sql:261–275`).
6. **Reaction:** pending-inclusive launch debt and history-visible reaction debt are distinct. Both require wake=1/reacted=0; jobs additionally require announced. Visible debt requires done (`internal/db/sql/async_jobs.sql:299–346`).

**OBSERVED:** Session-notice pull has its own transactional CAS/history path. Bound questions/wake-only notices void when the job disappears, ceases running or becomes void; supervision checks nonvoid work on a host not definitely dead (`internal/session/notice_pull.go:173–300`).

Reaction snapshots fence job claim+message identity and notice row+message identity; failure closure and marker insertion are transactional (`internal/session/async_job_reaction.go:124–207,365–403`). Ordinary error/canceled finishes do not react, but an awaiting-answer finish containing real content has an explicit reaction path (`internal/agent/agent_turn_step.go:780–807`; `internal/agent/agent_turn_failure.go:311–325`).

### Dependency table

All entries are **OBSERVED dependencies**. “Replace” describes the proposed cutover responsibility. Grouped anchors enumerate files, not permission to preserve their old contracts.

| Layer | Readers/writers and replacement responsibility |
|---|---|
| Core store | `internal/session/async_job_store.go:307–558` — claim/archive/terminal CAS; `internal/session/async_job_bgshell.go:41–103` — shell claim/announcement. Replace with reservation/projection; eliminate secondary background-escape identity. |
| Outbox/history | `internal/session/notice_pull.go:109–301,340–543`; `internal/session/notice_row_ids.go:26–72`; `internal/session/async_job_reaction.go:28–533` — pull, ack, inline, kill repair, debt, attempts, reaction and wake-zeroing. Replace job branch with selected-node debt. |
| Readers | `internal/session/async_job_reader.go:52–276`; `internal/session/async_job_reader_batch.go:19–177` — host facts, own/descendant/live/historical trees, chunked traversal and incomplete-read semantics. Nonterminal roots replace running-job predicates. |
| Recovery/retention | `internal/session/async_job_recovery.go:92–465`; `internal/session/async_job_recovery_question.go:9–26`; `internal/session/async_job_gc.go:22–49` — sweep, recovered text, retention/counts, host reaping. |
| History mutations | `internal/session/async_job_rerun.go:68–171`; `internal/session/async_job_reset.go:54–113`; `internal/session/session_lifecycle.go:55–88,128–155` — repend/void/stop-after-commit, reset refusal and owner cascade. Must understand live roots and node identity. |
| Retained infrastructure | `internal/session/session_child_question.go:78–107`; `internal/session/session_driver.go:95–199`; `internal/session/host_lock.go:210–217,355–425`; `internal/session/wake_schedule_fire.go:62–159` — question binding, driver ownership, host registration and occurrence→notice transaction. |
| Launch/terminal writers | `internal/agent/work_ledger.go:226–330,454–549`; `internal/agent/work_ledger_transition.go:202–308`; `internal/agent/work_ledger_bgshell_kill.go:38–127`; `internal/agent/bgshell_claim.go:49–164` — claims, sync bypass, retrying settlement, kill and shell callbacks. Remove live DB terminal authority. |
| Ack/output | `internal/agent/work_ledger_announce.go:53–280`; `internal/agent/work_ledger_inline.go:173–193`; `internal/agent/work_ledger_pending.go:17–33`; `internal/agent/tools/job_output.go:70–113,202–221`; `internal/agent/tools/job_kill.go:67–209` — acknowledgement, durable fallback and controller routing. |
| Delegation/input/timeouts | `internal/agent/work_ledger_delegation.go:57–190,221–277`; `internal/agent/coordinator_work_scope.go:52–110`; `internal/agent/delegation_question.go:124–210`; `internal/agent/work_ledger_timeout.go:163–242`; `internal/agent/supervision.go:145–223,405` — scope termination, held input, wake-only and supervision notices. |
| Pull/reaction callers | `internal/agent/agent_notice_pull.go:22–124`; `internal/agent/agent_turn.go:371–381`; `internal/agent/agent_turn_step.go:113,780–807`; `internal/agent/agent_turn_failure.go:311–325`; `internal/agent/work_ledger_reaction.go:107–128`; `internal/agent/drain_attempt.go:268,382–401,479`. |
| Arbiter/await | `internal/agent/turn_arbiter.go:235–308,407–532`; `internal/agent/coordinator_drain_policy.go:54–160,192–264`; `internal/agent/coordinator_await_tasks.go:45–188`; `internal/agent/cli_active_work.go:24–100` — debt, CompletionOnly, live-work gates and captured row-key polling. |
| Wake/maintenance/cancel | `internal/agent/coordinator_background.go:62–198`; `internal/agent/coordinator_bgshell_cap.go:58–117`; `internal/agent/coordinator_reaction_chain.go:60`; `internal/agent/coordinator_reaction_source.go:121–312`; `internal/agent/coordinator_recheck.go:80–179`; `internal/agent/coordinator_interrupt.go:57–137`; `internal/agent/coordinator_rerun.go:25`; `internal/agent/work_ledger_rerun.go:24`; `internal/session/wake_scheduler.go:172–243` — hints, maintenance, occurrence delivery and tree cancellation. |
| Construction/driver | `internal/agent/coordinator.go:423,459–463`; `internal/agent/coordinator_tools.go:706–716,809`; `internal/agent/work_ledger_driver.go:13`; `internal/app/app.go:370–386`; `internal/app/app_agent_setup.go:137–170`; `internal/app/app_accessors.go:50`. |
| App consumers/lifetime | `internal/app/app_lifecycle.go:266–297,340–366`; `internal/app/app_session_activity.go:62–246`; `internal/app/app_session_drivers.go:31–56`; `internal/app/app_run_async_wait.go:28–72`; `internal/app/app_run_quota.go:66–164`; `internal/app/app_run_setup.go:131–139`; `internal/app/app_run_gates.go:159–188`; `internal/app/app_run_async.go:311,352–361,696,790–835`. |
| CLI direct consumers | `internal/cmd/sessions_jobs.go:119–172`; `internal/cmd/sessions_why.go:143,282–313`; `internal/cmd/sessions_show.go:206`; `internal/cmd/sessions_list.go:128`; `internal/cmd/sessions_gc.go:173–175`; `internal/cmd/sessions_reset.go:153–171`. |
| CLI indirect consumers | `internal/cmd/sessions_watch.go:457`; `internal/cmd/sessions_tail.go:194`; `internal/cmd/sessions_inject.go:108`; `internal/cmd/sessions_cancel.go:128`; `internal/cmd/sessions_locks.go:262`; `internal/cmd/sessions_compact.go:315`; `internal/cmd/sessions_kill.go:550` — activity/lock/process-control interfaces. |
| Raw SQL outside sqlc | `internal/cmd/sessions_audit_db.go:156–196`; `internal/cmd/sessions_audit_rules.go:285–340`; caller `internal/cmd/sessions_audit_findings.go:43–55`. Missing old table currently disables that audit rule rather than supplying batch auditing. |
| Server | `internal/server/handlers_livework.go:75–245`; `internal/server/handlers_sessions.go:245–264`; `internal/server/handlers_agent.go:29`; `internal/server/handlers_agent_rerun.go:292–294`; `internal/server/protocol.go:707` — live-work protocol, questions and rerun alias. |
| Web handoff | `web/src/types.ts:268–299` — flat live-work shape; hierarchy/protocol changes belong to area D, not this persistence implementation. |

### Host death and recovery

**OBSERVED:** Confirmed-dead is an OS-lock verdict, not host PID/timestamp expiry. Shared probe contention means Alive; absent file or successful shared probe means Dead; unexpected errors mean Unknown. Own process identities are not probed (`internal/session/host_lock.go:231–268`). Registration verifies the locked pathname before publishing the host (`internal/session/host_lock.go:306–393`).

Recovery does not retain the shared lock: UUID host identities never revive. Writes remain identity-guarded for concurrent recoverers (`internal/session/async_job_recovery.go:63–103`). Current recovery handles running rows individually: delete unannounced; interrupt announced with wake=false; delete terminal-unannounced leaks; repair unnamed kill-result delivery (`internal/session/async_job_recovery.go:106–200`).

**OBSERVED:** Forced shutdown can retain the host lock until process exit (`internal/session/async_job_store.go:631–653`; `internal/app/app_lifecycle.go:282–296`). Session heartbeat and lock-inspection/display are separate mechanisms; heartbeat mtime must not become batch recovery authority (`internal/session/lock_heartbeat.go:39–59,96–123`; `internal/session/session_lock_fact.go:27–51`).

### Migration machinery and “without migration”

**OBSERVED:** Migration SQL is embedded; versions are decimal filename prefixes sorted numerically, not validated calendar timestamps (`internal/db/connect.go:24–25`; `internal/db/migrate_policy.go:267–288`). Existing authoring uses Goose Up/Down and StatementBegin/End (`internal/db/migrations/20261005000001_bg_shell_jobs.sql:1,16,75–81`). Provider permits out-of-order application; unknown/pending combinations have explicit divergence rules (`internal/db/migrate_policy.go:112–190`).

**INFERRED:** “Drop without migration” should say **“no legacy-row conversion/preservation; use a new destructive Goose schema migration.”** Merely editing historical migrations does not drop the table in existing databases. Discarded rows cannot be restored by Down. The previous rebuild copied rows; that is expressly not this cutover (`internal/db/migrations/20261005000001_bg_shell_jobs.sql:48–62`).

**OBSERVED:** `check_db_release_pairing*.sh` checks **connection-release pairing**, not product-release/schema pairing: test-file Connect/ConnectRead counts versus Release/ReleaseAll/ReleaseConn (`.githooks/check_db_release_pairing.sh:26–64,98–168`). Selftests include two connects/one release failing (`.githooks/check_db_release_pairing_selftest.sh:69–104`). New DB tests must balance each connection acquisition; dropping a table does not otherwise affect this guard.

## 2. What the design should say against current HEAD

**OBSERVED → required correction:**

- The DB CAS remains live terminal authority; replacing storage is an authority change, not a schema rename (`internal/db/sql/async_jobs.sql:158–193`).
- A stop path writes cancelled/done/reacted before its caller completes physical termination. Do not carry that contract forward (`internal/agent/work_ledger_bgshell_kill.go:90–118`).
- Current sync versus claimed async branches and background escape are not universal ordinary-batch ingress (`internal/agent/work_ledger.go:226–330`; `internal/agent/bgshell_claim.go:49–119`).
- Current Runner has no commit hook. Its apply publishes views/feed/Done before dispatch; these boundaries need restructuring, not attaching a post-publication callback (`internal/jobbatch/runtime_types.go:5–11`; `internal/jobbatch/runner.go:224–308`).
- Keep transactional history delivery, but replace claim identities with node incarnation identities and gate every internal notice through root announcement. Only engine-selected notices after elision create debt (`docs/plans/2026-10-07-job-batches-design.md:2202–2205,2263–2270`).
- Recovery must cover **all nonterminal nodes**, including Pending; emit one recovered root notice, preserve prior selected records and delete unannounced subtrees—not repeat current per-job recovery (`docs/plans/2026-10-07-job-batches-design.md:2357–2371`).

## 3. Proposed hook and transaction contract

**PROPOSED signature:**

```go
type CommitHook func(context.Context, Transition) error
```

Add an optional RunnerConfig field; nil preserves autonomous behavior. `Transition` already carries accepted event, changed views, commands, selected notices and sequence; Submit/Add specs and Settled Details are available through its event (`internal/jobbatch/events.go:35–109`).

**PROPOSED placement:** A Rush session persistence adapter owns transactions and immutable host/root metadata; the Runner loop invokes it serially. Do not hold session/store mutexes while acquiring the writer connection (`internal/session/async_job_store.go:170–179`). Sequence identity must include host/Runner incarnation, not sequence alone.

Required atomic boundaries:

- Submit/Add: reserve all accepted nodes before Starts.
- Stop: project cause/state and corresponding durable control answer.
- Settlement: changed nodes, outcome/envelope reference and selected notice debt together.
- Ack: started history plus root announcement.
- Pull/inline/reaction: separate consumer transactions, preserving current history atomicity.
- Recovery: **proposed root-atomic** subtree reconciliation, preventing partially repaired ancestor/descendant views.

Containment Stop/cancellation must not wait for commit; Starts, timers, published views/feed/replies/Done and delivery must. Commit error or unknown outcome is never retried; fault propagation must unblock Wait/public callers, preserve unpublished Done as open, contain executions and trigger process exit (`docs/plans/2026-10-07-job-batches-design.md:2217–2254`).

**Partially landed commit:** After exit, inspect committed rows under confirmed-dead proof. Preserve terminal rows/selected outboxes actually committed; interrupt remaining nonterminal nodes with `host_lost`; delete unannounced subtrees. Never replay execution. Atomic transition transactions prevent partial row-level landing; ambiguous commit success is reconciled from SQLite, not callback memory (`docs/plans/2026-10-07-job-batches-design.md:2248–2254,2357–2366`).

## 4. Independently mergeable breakdown

Tests below are **proposed, not run**. File scopes follow the dependency table.

| Step | Goal; touch / do not touch | Dependencies; size; risks | Focused verification |
|---|---|---|---|
| B0 | Freeze hook, schema, envelope/binding/control identities. Touch design and declarations; **not** ingress or registrations. | None; **S**; ambiguous metadata/ack meaning. | Review delayed ack, stale incarnation and unknown-commit cases against §9.2/9.3. |
| B1 | Optional commit boundary/fault lifecycle. Touch `internal/jobbatch/runner*.go`, runtime declarations/tests; **not** SQLite or lab scheduler. | B0; **L**; blocked containment, premature Done. | Fake held/failing hook; Submit/Add/Settled/HostClosing; in-flight Start after fault; Wait/feed faults. |
| B2 | Inert projection store and queries/generated code. Touch `internal/session/batch_*.go`, DB SQL/generated/schema support; **not** old production launches or destructive activation. | B0; **L**; live uniqueness, sequence scope, FKs. | Disposable schema; reservation atomicity; ten states; matching-live attach/input mismatch; owner cascade. |
| B3 | Selected-node outbox, root ack, inline and reaction. Touch notice/reaction session files and agent delivery adapters; **not** executors or admission. | B1–B2; **L**; duplicate/historyless delivery. | Concurrent pulls, failed history insert, finish-before-ack, inline race, elision, visible/pending debt and question reaction. |
| B4 | Root recovery, retention and durable controls. Touch recovery/GC/host cleanup, control SQL/store/pump; **not** process resumption or session-wide stop substitution. | B2–B3; **L**; unknown-host writes, stale controls, partial repair. | Concurrent recoverers; Pending/Cancelling; preserved terminal debt; unannounced subtree; two-process nested stop. |
| B5 | Root/node read APIs; coordinated consumer replacement. Touch reader/activity/arbiter/await/rerun/reset/CLI audit adapters; **not** web presentation or policy-wrapped executor construction. | B2–B4; **L**; falsely finished work, double counts. | AW-a/AW-b; later Add/captured roots; rule 4b; incomplete reads; reset refusal and audit coverage. |
| B6 | Atomic production cutover and destructive migration. Touch DB migration/generated removals, old writer removal and app lifetime/fatal-exit wiring; executor owner supplies universal ingress. **Not** retained host/wake infrastructure redesign. | B1–B5 plus execution integration; **L**; dual authority/mixed binaries. | Populated upgrade with zero row copy; no old SQL callers; commit-fault subprocess exit/recovery; shutdown writer retained through drain. |

**INFERRED merge constraint:** Additive/inert modules can merge independently. Actually dropping `async_jobs` cannot activate while old readers/writers remain; sqlc and raw audit SQL both depend on it (`internal/db/db.go:24–60`; `internal/cmd/sessions_audit_rules.go:285–340`).

## 5. Decisions still needed

1. **Literal “without migration”:** confirm destructive Goose DDL with no row conversion, rather than requiring disposable databases only (`internal/db/migrate_policy.go:112–190`).
2. **Mixed-version exclusion:** how are old live binaries excluded before drop? Migration locking is not an execution-host fence (`internal/db/connect.go:277–327`).
3. **Fatal exit ownership/bound:** which app/CLI/server boundary exits, and what bounds stuck commit/containment? Exit itself is fixed, not optional (`docs/plans/2026-10-07-job-batches-design.md:2231–2254`).
4. **Retained notice bindings:** replace `job_tool_call_id`, and explicitly dispose of legacy bound questions/check-ins (`internal/session/notice_pull.go:261–278`).
5. **Control acknowledgement and envelope persistence:** freeze accepted-and-committed versus termination semantics and durable response references (`docs/plans/2026-10-07-job-batches-design.md:2200–2207,2281–2283,2402–2409`).
6. **Owner deletion, retention and recovery wake:** define live-Runner handling of owner cascade, root/subtree retention and host-loss notification wake policy (`internal/session/session_lifecycle.go:128–155`; `docs/plans/2026-10-07-job-batches-design.md:2357–2371`).
7. **Agent outcome declaration:** mandatory or optional remains explicitly undecided; it belongs to the adapter, not SQLite or core inference (`docs/plans/2026-10-07-job-batches-design.md:2332–2335`).

**Final status:** research only; no files changed and no tests executed. The critical integration boundary is replacing DB-CAS terminal authority with ordered Runner commits while retaining transactional consumer delivery and OS-lock-confirmed recovery.
