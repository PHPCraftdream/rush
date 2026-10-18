# Flag-ON port — user-facing surfaces (F8/F9/F10), 2026-10-18

Scope: port the three user-facing families of the flag-ON shadow triage
(docs/reviews/2026-10-18-flag-on-shadow-triage.md, run 39139589128) onto
batch-aware fixtures so RUSH_BATCH_RUNTIME=1 stops failing on legacy-seeded
fixtures. Rules honored throughout: flag-OFF behavior byte-identical; every
assertion byte-identical (no weaken/skip/delete/special-case); no
compilation on this machine (static checks only; CI compiles and runs).

## F8 — internal/cmd sessions commands

Production fixes (the flag-ON consumer had regressed user-facing behavior):

1. `sessions list` STATUS (internal/cmd/sessions_list.go, RunE flag-ON
   branch): the branch built STATUS from batchRuntimeFacts alone
   ("batch live"/"batch unknown"), a second status machinery that violated
   this file's own invariant ("the STATUS column is the one session-activity
   classifier's verdict ... there is no second status machinery here"). It
   now reads App.SessionActivityBatch once under the flag and maps the
   classifier verdict through listStatus (running/delegating/crashed/done),
   with the batch facts as annotations: " (batch unknown)" / " (batch live)",
   then the unchanged "(awaiting answer)" decoration. Under the flag a live
   lock reads "running" again, a dead-PID crash reads "crashed", a live
   delegation reads "delegating" — the vocabulary the legacy tests pin and
   H4 users keep. The pinned batch-ON test
   (TestSessionsListCmdRun_BatchFlagOnStatusAndJSON) still passes: its row
   renders "running (batch live) (awaiting answer)".
2. `sessions jobs` empty state (internal/cmd/sessions_batch_read.go): the
   flag-ON "(no batch work)" line is the legacy "(no jobs)" (user-facing
   continuity; nothing pinned the old string).
3. `sessions reset` live-loop gate (internal/cmd/sessions_reset.go flag-ON
   branch + resetLiveLoopRefusal in internal/cmd/sessions_batch_act.go): the
   batch reset's atomic refusal covers batch work, but a live `rush run`
   loop between turns holds no batch root and no lock — under the flag
   `reset --force` would have wiped a session a loop still drives (the
   R8A-3 bug). The flag-ON path now refuses through the same
   resetRefusedError message before calling the batch reset.

Dual-mode helpers (one shared set per package): claimOwnJob /
claimDelegation / finishOwnJob converted to take *app.App and seed the
flag-appropriate substrate (legacy Claim/Transition; batch Reserve root with
a 'command' task child, ChildSessionID set on the delegation child), plus
batchSeedHost, settleBatchRoot, and the reset wrappers seedResetDebrisDual /
assertResetCleanDual (legacy debris verbatim; flag ON seeds a completed
debt-bearing batch root + the same supervision notice and additionally
asserts the batch debt is gone after the reset).

Tests ported (flag OFF byte-identical, flag ON green): 
TestSessionsListCmdRun_StatusHonorsConfiguredDataDir (lock-only fixture; fix 1),
TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone (delegation via
claimDelegation; phase 2 settles the batch root),
TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone,
TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone (driver marker is
flag-neutral; fix 1),
TestSessionsJobsCmdRun_NoJobs (fix 2),
TestSessionsInjectCmdRun_OwnJobOrDelegationBetweenTurnsIsRunning
(inject reads the flag-aware classifier; BetweenTurns is derived from the
driver fact, sessions_inject.go:114,:138),
TestSessionsShowCmdRun_AsyncJobCounts (flag ON: one batch root, one child
completed + one running => "2 total, 1 running"),
TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork (classifier is
flag-aware),
TestSessionsReset_VoidsAsyncNoticesAndDebt (dual debris wrappers).

Env pins (the F11 convention, t.Setenv("RUSH_BATCH_RUNTIME","false"), no
assertion changes; their t.Parallel() removed because Go 1.26 testing panics
on Setenv-then-Parallel): the legacy-path unit tests that drive
resetSessionHistory/explainSessionStatus directly or pin the dead-host crash
fact (which the batch authority deliberately lacks): the two
TestResetSessionHistory_* tests, TestListStatus_OwnJobShapes,
TestListStatus_DriverShapes, the TestExplainSessionStatus_* legacy-render
tests in sessions_own_job_status_test.go, sessions_run_driver_status_test.go,
sessions_why_test.go, sessions_why_descendant_test.go,
sessions_why_awaiting_test.go, and TestSessionsActivity_OneTableEveryCommand.

QUESTIONS (left untouched; red under the flag, missing behaviour named):
- TestSessionsJobsCmdRun_TableAndForeignLiveHostHint, TestSessionsJobsCmdRun_JSON:
  the flag-ON `sessions jobs` table/JSON has no host-liveness or foreign-host
  kill-hint surface (renderSessionsJobsBatch renders state/held/debt only);
  the batch authority has no foreign-host kill concept.
- TestSessionsGcCmdRun_JobsOlderThan_PurgesTerminalOnly / _DryRunCountsOnly /
  _JSONPrintsCounts: pinned legacy wording ("would purge 1 async job row")
  and JSON kind "async_jobs" cannot hold under the flag — the flag-ON gc
  emits the "batch_retention" line, pinned by
  TestSessionsGc_BatchRetentionSummaryFlagOn (sessions_batch_act_test.go) —
  and batch retention deliberately never purges session_notices
  (internal/session/batch_retention.go), so the notice half of the retention
  rule has no flag-ON equivalent yet.
- TestSessionsCancel_PurgesQueuedWork: fixture-level race with the test's
  own RunQueuePumps (app.New starts one on every full app —
  internal/app/app.go — NOT flag-gated; the flag widens the window because
  installBatchRuntime runs synchronously in each of the test's ~4 app
  builds, past the pump's 3s tick). Needs a test seam (pump interval or
  disable) or a single-app-lifetime fixture restructure; production change
  would be wrong.

## F9 — internal/server live-work handlers

Production consumers were already flag-aware (buildSessionLiveWork branches
to buildSessionLiveWorkFromBatch; annotateSessionActivity reads the flag-aware
classifier) — fixture-only family.

Dual-mode helpers (internal/server/livework_seed_test.go, new):
liveWorkSeedSpec + seedLiveWorkRow (OFF: real AsyncJobStore
Claim/Transition; ON: batch_nodes root+child raw INSERTs with the
insertBatchRootAndTask column set), insertLiveWorkBatchRows (over any
handle, for the cross-process test), projectLiveWorkRoot (batch-side
terminal settle).

Tests ported: TestHandleListSessions_LiveWorkAnnotationIsBatched (rootDead's
"work that must not read as live" is seeded as TERMINAL batch roots under
the flag — the batch authority has no dead-host-running fact; the oracle
loop over the single-root readers is flag-routed: OFF the store readers, ON
the classifier facts, require lines byte-identical),
TestHandleListSessions_LiveDescendantWorkAnnotation,
TestHandleListSessions_LiveOwnWorkAnnotation,
TestHandleGetSessionLiveWork_SubAgentDelegation,
TestHandleGetSessionLiveWork_CrossProcessRowsReadFromDB (flag ON: the batch
root is inserted through a separate db.Connect handle),
TestLiveWorkPusher_CoalescesBurstIntoOneSnapshot,
TestLiveWorkTitles_IsolatedPerSession, TestLiveWorkTitles_InputChangeInvalidates,
TestLiveWorkTitles_CacheAvoidsHistoryReread, TestMaybeBroadcastSessionLiveWork,
TestHandleGetSessionLiveWork_AwaitingAnswerBadge.

QUESTIONS (left untouched; no equivalent flag-ON behaviour):
- TestHandleGetSessionLiveWork_RunningAndTerminalCommands,
  TestHandleGetSessionLiveWork_TerminalCausesAreDistinct,
  TestBuildSessionLiveWork_TerminalCap: the flat ON-path snapshot reads
  BatchLiveNodesForOwners ONLY (internal/db/sql/batch_read.sql:1-6 —
  nonterminal roots), so terminal rows never appear in it; and the legacy
  terminal-cause vocabulary (timeout_terminated/session_cancel/job_kill)
  cannot be seeded into batch_nodes.cause, whose CHECK allows only
  ('', 'stop', 'timeout', 'host_closing', 'stop_on_fail')
  (internal/db/migrations/20261012000001_batch_projection.sql:30). Terminal
  work is visible flag-ON only through the structured batch snapshot
  (handlers_batch.go), not the flat one.

## F10 — audit fixtures pre-dating the batch_nodes migration

Fixture-only family: the audit's production paths are already flag-aware,
but the hand-rolled audit fixtures build a pre-batch schema, and with
RUSH_BATCH_RUNTIME=1 the audit refuses a rush.db without a batch_nodes table
(internal/cmd/sessions_audit_db.go:98-103, by design) and runs a batch scan
over the same read-only handle (sessions_audit_db.go:129-135,
auditScanBatchFacts at internal/cmd/sessions_batch_audit.go:49).

The fix appends the real production DDL to the fixture schema, in one new
const, with zero assertion changes:

- `auditBatchProjectionDDL` (internal/cmd/sessions_audit_test.go:75, next to
  `auditSchemaFixture` at :32) carries, verbatim from production (without
  the goose comment markers):
  - the `CREATE TABLE batch_nodes (...)` statement and its five CREATE
    INDEX/UNIQUE INDEX statements (idx_batch_nodes_live_key,
    idx_batch_nodes_host_live, idx_batch_nodes_owner_roots,
    idx_batch_nodes_parent, idx_batch_nodes_pending_notice,
    idx_batch_nodes_debt), copied verbatim from
    internal/db/migrations/20261012000001_batch_projection.sql lines 4-95
    (exact column/constraint text, including the CHECK constraints);
  - the `CREATE TABLE session_notices (...)` statement, copied verbatim
    from internal/db/migrations/20260928000002_add_async_phase4_core.sql
    lines 120-148 (the migration's SQL comments dropped, columns and
    constraints exact).
- session_notices had to be included: the flag-ON batch scan reads it
  through reduceBatchSessionFacts' bound-notice query
  (BatchListBoundNoticesByToolCallID, internal/cmd/sessions_batch_read.go:176-191)
  and debt query (BatchReactionPendingInclusiveDebtRows, :199), whose SQL
  (internal/db/sql/batch_outbox.sql:50-52) is
  `SELECT * FROM session_notices WHERE owner = ? AND wake = 1 AND
  reacted = 0 AND delivery IN ('pending','done')` — a missing
  session_notices table would mark sessions Unknown and add a spurious
  "unfinished_jobs ... batch:unknown" finding
  (unfinishedEntries, sessions_batch_audit.go:72-88), breaking
  TestSessionsAudit_SinceLimitsTheWindowNotTheReport's
  `require.Empty(t, reports[0].Findings)`.
- Three seeders execute it: seedAuditFixtureDB
  (sessions_audit_test.go:205, `auditSchemaFixture+auditBatchProjectionDDL`),
  seedAuditWALFixtureDB (:385, same), and seedAuditRulesDB's default-schema
  resolution (internal/cmd/sessions_audit_rules_test.go:121,
  `schema+auditBatchProjectionDDL`, so every rules schema variant —
  auditSchemaCore, auditSchemaFixtureNoJobs, auditSchemaFixtureJobsNoKind —
  carries the projection too).
- No batch rows are seeded anywhere: the ported tests assert selection,
  health, message rules and legacy async_jobs only. Verified statically
  that a batch_nodes table with zero rows reads complete and at rest:
  BatchLiveForSessions (internal/session/batch_read_live.go:18) fetches
  every owner once with no rows and no failures, so batchLiveReplay leaves
  Complete=true with no roots; reduceBatchSessionFacts then produces no
  root facts, no debt read, GuardDebt=false and no unfinished entries —
  the additive JSON block reports roots 0, held 0, debt 0, unknown false,
  and the text block prints the "batch runtime on: unfinished_jobs counted
  from batch_nodes (batch runtime on)" header plus one inert per-session
  line (sessions_batch_audit.go:157-178). The QUESTIONS jobs fixtures are
  untouched.

Flag-OFF inertness (verified by reading auditInspectSchema and
auditFindings): with the flag off, no audit query touches batch_nodes or
session_notices. auditInspectSchema runs PRAGMA table_info(batch_nodes) only
(sessions_audit_db.go:193-197 — an existence probe, not a scan); the
unfinished_jobs rule goes through auditScanAsyncJobs over async_jobs
(sessions_audit_findings.go:53-60) and the notice rule reads messages.notice_kind,
not session_notices. The flag-OFF report is byte-identical; the fixture DB
simply has two more inert tables.

Tests ported (flag OFF byte-identical, flag ON green, zero assertion
changes):
- sessions_audit_test.go: TestSessionsAudit_IsStrictlyReadOnly,
  TestSessionsAudit_ReadOnlyWithPendingWAL, TestSessionsAudit_PrefixSelection,
  TestSessionsAudit_JSONOutput — the flag-ON report adds the additive
  `batch` JSON block and the "batch runtime on: ..." text block, which none
  of their Contains/NotContains/length assertions conflict with, and the
  ambiguous-prefix case returns before the batch scan.
- sessions_audit_flags_test.go: TestSessionsAudit_WorktreesScansEveryRegisteredDatabase
  (both fixture DBs pass the gate; the report count and health blocks are
  unaffected), TestSessionsAudit_SinceLimitsTheWindowNotTheReport,
  TestSessionsAudit_JSONCarriesFindings.
- sessions_audit_rules_test.go: TestAuditRules_RepeatedView_Boundary,
  TestAuditRules_RepeatedCommand_Boundary, TestAuditRules_WaitOnly_Boundary,
  TestAuditRules_ToolErrors_Boundary, TestAuditRules_Notices_Boundary,
  TestAuditRules_SinceWindow, TestAuditRules_BrokenRowsDoNotStopTheAudit.
  BrokenRowsDoNotStopTheAudit was read in full before porting: its
  assertions are message/healthy-row-scoped only (the repeated-view finding
  beside the broken rows, and `require.Empty(t, reports[0].Notices)`); it
  pins nothing about unfinished_jobs evidence, interrupted-state counting
  or async_jobs row shapes, so it ports cleanly — under the flag the batch
  scan sees zero batch rows and adds no findings.

QUESTIONS (left untouched; red under the flag by design):
- TestAuditRules_UnfinishedJobs_Boundary and TestAuditRules_Integration: the
  flag-ON unfinished_jobs detail is "<toolCallID>:<rootState>"
  (unfinishedEntries, sessions_batch_audit.go:72-88, pinned by
  sessions_batch_audit_test.go:79) while these tests pin
  "<toolCallID>:<kind>"; and batch 'interrupted' is terminal while the
  legacy rule counts interrupted as unfinished.
- TestAuditRules_SchemaDegradation_IsSilent: its
  `require.NotContains(t, stdout, "unfinished_jobs")` can never hold
  flag-ON because the report always prints the "batch runtime on:
  unfinished_jobs ..." block (sessions_batch_audit.go:164), and the
  no-batch_nodes degradation it exercises is refused by design under the
  flag (sessions_audit_db.go:98-103).

## Verification

Every changed file is gofumpt/gofmt parse-only clean (gofumpt -l and
gofmt -e -l empty on internal/cmd/sessions_audit_test.go,
internal/cmd/sessions_audit_rules_test.go and this document). git status
shows only the intended files: the two audit test files and this new
document, on top of the pre-existing F8/F9 modifications, which were left
untouched. The production diff is limited to the F8-listed files
(sessions_list.go, sessions_batch_read.go, sessions_reset.go,
sessions_batch_act.go) plus their tests — none of the F10 edits touch
production code. No compilation, no tests and no benchmarks ran locally;
all verification was static (file reads, grep, gofumpt/gofmt parse-only
checks, read-only git). No artifacts were left on disk.
