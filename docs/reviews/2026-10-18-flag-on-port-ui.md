# Flag-ON port — user-facing surfaces (F8/F9/F10), 2026-10-18

Scope: port the three user-facing families of the flag-ON shadow triage
(docs/reviews/2026-10-18-flag-on-shadow-triage.md, run 39139589128) onto
batch-aware fixtures so RUSH_BATCH_RUNTIME=1 stops failing on legacy-seeded
fixtures. Rules honored throughout: flag-OFF behavior byte-identical; every
assertion byte-identical (no weaken/skip/delete/special-case); no
compilation on this machine (static checks only; CI compiles and runs).

Revision 2 (round-2 work only): the P1 value-flow contradiction is resolved —
TestHandleListSessions_LiveWorkAnnotationIsBatched is reverted to the
pre-port state (d1a043ea) and moved to QUESTIONS; the three user-facing
families were re-ported onto batch-aware fixtures (real Reserve seeding,
flag-ON gating and dual-mode seeds/helpers), env pins re-dispositioned into
the H4 table, and the audit DDL gated on the flag. Round-2 did NOT close
P2-1..P2-4; see Revision 3 and Revision 4 for what actually addressed them.

Revision 3 (round-3 review): the round-2 P1 (unused "context" import in
internal/server/livework_seed_test.go) is fixed. P2-1: batchRuntimeFacts now
degrades a configless App (nil DB func / nil handle) to an EXPLICIT UNKNOWN
facts map (unavailableBatchFacts), never to the flag-off nil, so an
unreadable batch can no longer render "at rest". P2-2: a new flag-ON
instrumentation test (internal/server/descendant_work_batch_on_test.go)
pins the chunked batch live walk through the batchSourceFor seam. P2-3:
doc corrections applied (cancel-purge narrative trimmed to the observed
fact, debt-read claim corrected, "second connection handle" wording
replaced with the db.Connect pooled-handle truth). Every green claim here
remains a STATIC derivation; CI pending — nothing is verified green.

Revision 4 (round-4 review): the CI tparallel lint failure in
TestResetSessionHistory_RefusesWhileLiveWork is fixed by deleting the
SUBTESTS' t.Parallel (the F11 t.Setenv pin forces the whole test — parent
and subtests — sequential under the Go 1.26 Setenv rule; the top level stays
non-parallel by design). P2-1/P2-2: the ANNOTATION path is now instrumented
for real over N listed sessions
(TestHandleListSessions_AnnotationReadsBoundedInOwners,
descendant_work_annotation_reads_on_test.go) via a counting database/sql
driver under the hand-built &app.App{} literal seam; the round-3 snapshot
test is renamed to say it instruments the FLAT SNAPSHOT reader
(batchSourceFor seam). P2-4: the two remaining configless branches
(nil App receiver, nil DB handle) are pinned
(TestBatchRuntimeFacts_UnavailableDBVariantsMarkUnknown). P2-3: this doc's
truth sweep applied. All checks remain static derivations; CI pending.

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
   H4 users keep. Static value flow: the pinned batch-ON test's
   (TestSessionsListCmdRun_BatchFlagOnStatusAndJSON) row renders
   "running (batch live) (awaiting answer)" under the fix — every one of its
   Contains assertions holds; CI pending.
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
4. batchRuntimeFacts (internal/cmd/sessions_batch_read.go): flag off → nil
   (callers keep the legacy path; renderSessionsJobsBatch's nil branch now
   means flag-off ONLY). Flag ON with a configless App (nil DB func or nil
   handle) → an explicit unknown facts map (unavailableBatchFacts, one
   Unknown+Live entry per id), so the configless flag-ON renders the
   unknown item / "batch unknown (incomplete read)" — never "at rest" on
   unreadable evidence.

Dual-mode helpers (one shared set per package): claimOwnJob /
claimDelegation / finishOwnJob converted to take *app.App and seed the
flag-appropriate substrate (legacy Claim/Transition; batch Reserve root with
a 'command' task child, ChildSessionID set on the delegation child), plus
batchSeedHost, settleBatchRoot, and the reset wrappers seedResetDebrisDual /
assertResetCleanDual (legacy debris verbatim; flag ON seeds a completed
debt-bearing batch root + the same supervision notice and additionally
asserts the batch debt is gone after the reset).

Tests ported (flag OFF byte-identical; flag ON statically derived green
(CI pending)): 
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

Pin dispositions: of the 27 pins, 26 are H4 delete candidates (table below)
and 1 (TestListStatus_DriverShapes) was ported to the dual-mode helpers and
unpinned. Pins on legacy-path units are sanctioned; they keep
t.Setenv("RUSH_BATCH_RUNTIME","false") and therefore no t.Parallel (a Go 1.26
t.Setenv test cannot be parallel) — stated here explicitly, not silently.

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
- TestSessionsCancel_PurgesQueuedWork: red under the flag (the observed
  failure). Fact: app.New starts a RunQueuePump whenever a data dir is set
  and the agent config resolves (internal/app/app.go:572-579), and a pump
  ticks immediately on start
  (internal/session/run_queue_lifecycle.go:149-150). Cause NOT established.
  Needs a test seam (pump disable/interval) or a single-app fixture; no
  production change is indicated.

## H4 delete candidates (pinned legacy-path tests)

| Test | file | Legacy unit under test (H4 deletes) | Where the flag-ON behaviour is covered |
|---|---|---|---|
| TestExplainSessionStatus_Crashed_NoCleanFinish (:85) | sessions_why_test.go | legacy branch of explainSessionStatus (internal/cmd/sessions_why.go:127-239; H4 deletes the gate and the legacy body; twin: explainWhyBatch) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir; explainWhyBatch has no crashed verdict: a dead-holder lock with no live batch work reads "at rest" |
| TestExplainSessionStatus_StaleLockCleanFinish (:127) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir; a stale lock with no batch work reads "at rest" flag-ON |
| TestExplainSessionStatus_Running (:198) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir |
| TestExplainSessionStatus_ErrorFinishSurfacesErrorText (:303) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir |
| TestExplainSessionStatus_AgedLockWithLivePIDIsRunning (:349) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir |
| TestExplainSessionStatus_DeadPIDIsCrashed (:390) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld / TestSessionsWhyCmdRun_HonorsConfiguredDataDir; explainWhyBatch has no crashed verdict: a dead-holder lock with no live batch work reads "at rest" |
| TestExplainSessionStatus_StatFailureIsFailOpenInTurn (:426) | sessions_why_test.go | legacy branch of explainSessionStatus (same unit) | the fail-open in-turn property stays classifier-level, covered by the list paths |
| TestExplainSessionStatus_StaleLockLiveChildIsDelegating (:148) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (async_jobs section, H4-deleted; twin: renderBatchRootsForWhy) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_AtRestLiveChildIsDelegating (:189) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_ChildReleasedReturnsToDone (:223) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_GrandchildLiveIsDelegating (:262) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_DeadHolderWithLiveChildIsDelegating (:301) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_AsyncJobsAndDebtSection (:348) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted (:412) | sessions_why_descendant_test.go | legacy why render + describeAsyncJobsAndDebt (same unit) | TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_AtRestOwnRunningJobIsRunning (:122) | sessions_own_job_status_test.go | legacy why render | flag-ON list equivalent: ported TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone |
| TestExplainSessionStatus_DeadPIDLockWithOwnJobIsRunningNotCrashed (:149) | sessions_own_job_status_test.go | legacy why render | flag-ON list equivalent: ported TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone |
| TestExplainSessionStatus_DeadPIDLockWithoutJobIsCrashed (:172) | sessions_own_job_status_test.go | legacy why render | flag-ON list equivalent: ported TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone |
| TestListStatus_OwnJobShapes (:201) | sessions_own_job_status_test.go | classifier table whose deadHost row pins the legacy-only dead-host-running fact (app_session_activity.go:249, flag-OFF only; the batch authority keeps dead-host roots live until recovery) | the other three rows' flag-ON equivalents live in the ported list tests |
| TestExplainSessionStatus_LiveRunDriverBetweenTurnsIsRunning (:54) | sessions_run_driver_status_test.go | legacy why render | driver halves covered flag-ON by ported TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone and the ported DriverShapes |
| TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed (:91) | sessions_run_driver_status_test.go | legacy why render | same: ported TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone and the ported DriverShapes |
| TestExplainSessionStatus_DeadHostRunDriverIsCrashed (:134) | sessions_run_driver_status_test.go | legacy why render | same: ported TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone and the ported DriverShapes |
| TestSessionsActivity_OneTableEveryCommand (:99) | sessions_activity_cmd_test.go | cross-command oracle whose whyStatus column pins the legacy render | the listStatus/kindIsLive halves keep flag-ON coverage via the ported list/inject tests |
| TestExplainSessionStatus_AwaitingAnswerNamesChildAndAnswerPath (:51) | sessions_why_awaiting_test.go | legacy #1158 section | flag-ON badge covered by TestSessionsJobsCmdRun_BatchLiveRootFlagOn and TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestExplainSessionStatus_AwaitingAnswer_WithLiveDriverOmitsRelay (:86) | sessions_why_awaiting_test.go | legacy #1158 section | flag-ON badge covered by TestSessionsJobsCmdRun_BatchLiveRootFlagOn and TestSessionsWhyCmdRun_BatchCancellingRootHeld |
| TestResetSessionHistory_RefusesWhileLiveWork (:193) | sessions_reset_async_test.go | resetSessionHistory (legacy unit; H4 replaces it with batchResetSessionHistory + resetLiveLoopRefusal) | TestSessionsReset_Batch*RefusesFlagOn and TestSessionsReset_LiveLoopRefusesEvenWithForce |
| TestResetSessionHistory_DeadHostRowsAreRecoveredAndVoided (:241) | sessions_reset_async_test.go | resetSessionHistory; also exercises RecoverOwnerScope (legacy dead-host recovery) | TestSessionsReset_Batch*RefusesFlagOn and TestSessionsReset_LiveLoopRefusesEvenWithForce |

## F9 — internal/server live-work handlers

Production consumers were already flag-aware (buildSessionLiveWork branches
to buildSessionLiveWorkFromBatch; annotateSessionActivity reads the flag-aware
classifier) — fixture-only family.

Dual-mode helpers (internal/server/livework_seed_test.go, new):
liveWorkSeedSpec + seedLiveWorkRow (OFF: a real AsyncJobStore Claim with
EXACTLY the inputs the original fixtures used — same Input text, same
ToolName, same kind; ON: session.BatchStore.Reserve — root kind=batch, mode
parallel, ControlState none, carrying the tool call, plus one task child when
the spec has a task kind, carrying the delegation child session; the host id
is deliberately unregistered, so the reader probes it Dead, which never
settles a node and never marks a read incomplete), projectLiveWorkRoot (one
seeded root settled — every node of it — to the terminal state through
BatchStore.Project, looked up via GetLiveBatchRoot/ListBatchNodes).

Tests ported:
TestHandleListSessions_LiveDescendantWorkAnnotation,
TestHandleListSessions_LiveOwnWorkAnnotation,
TestHandleGetSessionLiveWork_SubAgentDelegation,
TestHandleGetSessionLiveWork_CrossProcessRowsReadFromDB (flag ON: the batch
root is reserved through db.Connect's pooled handle for the same data
directory, via the production session.BatchStore.Reserve API),
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
- TestHandleListSessions_LiveWorkAnnotationIsBatched: reverted to the
  pre-port state (d1a043ea; round 2). Under the flag every depth-0 live batch root is the owner's own
  work (batch_read_live.go:133-138 → app_batch_cutover_facts.go:86-90 →
  handlers_sessions.go:276; pinned TRUE by
  handlers_sessions_batch_cutover_test.go:112-117), so a parent owning a
  live delegation can never satisfy HasLiveOwnWork=false; the legacy
  nested-delegation shape (parent-owned agent row, own-work = command rows
  only), the dead-host-not-live annotation and the AsyncJobStore-reader
  query-count property have no batch equivalent. Red under the flag at :136
  (the original shadow failure).

## F10 — audit fixtures pre-dating the batch_nodes migration

Fixture-only family: the audit's production paths are already flag-aware,
but the hand-rolled audit fixtures build a pre-batch schema, and with
RUSH_BATCH_RUNTIME=1 the audit refuses a rush.db without a batch_nodes table
(internal/cmd/sessions_audit_db.go:98-103, by design) and runs a batch scan
over the same read-only handle (sessions_audit_db.go:129-135,
auditScanBatchFacts at internal/cmd/sessions_batch_audit.go:49).

The seeders now GATE the DDL on the flag — `auditDDLForTest()` in
sessions_audit_test.go (schema := auditSchemaFixture; auditBatchProjectionDDL
appended only when config.BatchRuntimeEnabled()) used by seedAuditFixtureDB
and seedAuditWALFixtureDB, and the same gate inlined after the default-schema
resolution in seedAuditRulesDB (internal/cmd/sessions_audit_rules_test.go),
with zero assertion changes:

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
- Three seeders execute it through the gate: seedAuditFixtureDB
  (sessions_audit_test.go, `auditDDLForTest()`), seedAuditWALFixtureDB
  (same), and seedAuditRulesDB's default-schema resolution
  (internal/cmd/sessions_audit_rules_test.go, gated after `schema` resolves,
  so every rules schema variant — auditSchemaCore,
  auditSchemaFixtureNoJobs, auditSchemaFixtureJobsNoKind — carries the
  projection flag-ON only).
- No batch rows are seeded anywhere: the ported tests assert selection,
  health, message rules and legacy async_jobs only. Verified statically
  that a batch_nodes table with zero rows reads complete and at rest:
  BatchLiveForSessions (internal/session/batch_read_live.go:18) fetches
  every owner once with no rows and no failures, so batchLiveReplay leaves
  Complete=true with no roots; reduceBatchSessionFacts then produces no
  root facts, an EMPTY (not skipped) debt read —
  BatchReactionPendingInclusiveDebtRows runs whenever the live read is
  complete regardless of root count (sessions_batch_read.go:198-215) and
  empty tables return an empty debt — GuardDebt=false and no unfinished
  entries —
  the additive JSON block reports roots 0, held 0, debt 0, unknown false,
  and the text block prints the "batch runtime on: unfinished_jobs counted
  from batch_nodes (batch runtime on)" header plus one inert per-session
  line (sessions_batch_audit.go:157-178). The QUESTIONS jobs fixtures are
  untouched.

Flag-OFF byte-identity: with the DDL gated on config.BatchRuntimeEnabled(),
the flag-OFF fixtures are schema- and output-identical to HEAD; flag-ON
fixtures carry batch_nodes + session_notices for the schema guard and the
batch scan.

Tests ported (flag OFF byte-identical; flag ON statically derived green, CI
pending; zero assertion changes):
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

Round-2 truth: static-only checks. gofumpt -l and gofmt -e -l rc 0 (empty)
on every touched .go file; bash .githooks/check_file_size.sh rc 0 from the
repo root; git status --short shows only the intended files;
internal/server/descendant_work_batch_test.go reverted byte-identical to
its pre-port state (d1a043ea; git diff d1a043ea on it empty). No
compilation, no tests and no benchmarks ran
locally; CI is pending. Every green claim in this document is a static
value-flow derivation, not an observed run.

Round 4: after the last edit, these changed files were re-read IN FULL:
internal/cmd/sessions_reset_async_test.go,
internal/cmd/sessions_batch_read_test.go,
internal/server/descendant_work_batch_on_test.go,
internal/server/descendant_work_annotation_reads_on_test.go (new), and this
document.

Expected green on the next shadow round (static derivation):

- F9: TestHandleListSessions_LiveDescendantWorkAnnotation,
  TestHandleListSessions_LiveOwnWorkAnnotation,
  TestHandleGetSessionLiveWork_SubAgentDelegation,
  TestHandleGetSessionLiveWork_CrossProcessRowsReadFromDB,
  TestHandleGetSessionLiveWork_AwaitingAnswerBadge,
TestHandleGetSessionLiveWork_BatchOnSnapshotReaderChunkedReads (renamed
from TestHandleGetSessionLiveWork_BatchOnInstrumentedChunkedRead,
descendant_work_batch_on_test.go: flag-ON instrumentation over the FLAT
SNAPSHOT reader — the batchSourceFor seam, a different surface from the
annotation handler; all assertions and the seam disclosure unchanged),
  TestLiveWorkPusher_CoalescesBurstIntoOneSnapshot,
  TestLiveWorkTitles_IsolatedPerSession,
  TestLiveWorkTitles_InputChangeInvalidates,
  TestLiveWorkTitles_CacheAvoidsHistoryReread, TestMaybeBroadcastSessionLiveWork.
- F8: TestSessionsListCmdRun_StatusHonorsConfiguredDataDir,
  TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone,
  TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone,
  TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone,
  TestSessionsJobsCmdRun_NoJobs,
  TestSessionsInjectCmdRun_OwnJobOrDelegationBetweenTurnsIsRunning,
  TestSessionsShowCmdRun_AsyncJobCounts,
  TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork,
  TestSessionsReset_VoidsAsyncNoticesAndDebt,
  TestSessionsReset_LiveLoopRefusesEvenWithForce,
  TestListStatus_DriverShapes (newly ported),
  TestBatchRuntimeFacts_ConfiglessAppDegradesToUnknownNotPanics (reworked:
  configless under the flag degrades to an explicit unknown),
  TestExplainWhyBatch_EmptyReadableDBReadsAtRest (new: a readable DB with
  no batch rows and no lock IS at rest),
  TestBatchRuntimeFacts_UnavailableDBVariantsMarkUnknown (new, round 4:
  the nil-App-receiver and nil-DB-handle branches of batchRuntimeFacts
  both mark UNAVAILABLE, never nil, never idle), plus
  TestExplainSessionStatus_NoQuestionNotice_StaysSilent — it does NOT pin
  the env, but its app IS DB-wired (whyAwaitingTestEnv) and it asserts no
  question section, never "at rest", so under the flag it exercises
  explainWhyBatch and is statically expected to pass under the flag (its
  assertions are NotContains-only and its app is DB-wired); CI pending —
  and the pre-existing
  batch-ON tests re-derived under the classifier-first list
  (TestSessionsListCmdRun_BatchFlagOnStatusAndJSON).
- F10: the 14 ported audit tests.
- F9 (new): TestHandleGetSessionLiveWork_BatchOnSnapshotReaderChunkedReads
  (renamed; the flat snapshot reader via the batchSourceFor seam; seam
  disclosure in the F9 section above) and
  TestHandleListSessions_AnnotationReadsBoundedInOwners (new,
  descendant_work_annotation_reads_on_test.go — round 4, P2-1/P2-2: the
  ANNOTATION path instrumented for real over N listed sessions via a
  counting database/sql driver under the hand-built &app.App{} literal
  seam; asserts the exact derived relation stmts == 5 + 2*N and txs == 1).

QUESTIONS (red under the flag; missing behaviour named):
- TestExplainSessionStatus_AtRest_CleanFinish,
  TestExplainSessionStatus_AtRest_NoAssistantMessage,
  TestExplainSessionStatus_Running_PIDUnreadableFreshHeartbeat,
  TestExplainSessionStatus_PIDlessRecordIsReleased (sessions_why_test.go):
  none pins the env (no t.Setenv("RUSH_BATCH_RUNTIME", ...)) and each
  asserts "status: at rest" through a CONFIGLESS &app.App{} — under the
  flag that why read now renders "batch unknown (incomplete read)" (the
  conservative unknown of the P2-1 fix), not the legacy "at rest"; the
  tests pin the legacy render.
- TestHandleListSessions_LiveWorkAnnotationIsBatched (semantics above).
- TestSessionsJobsCmdRun_TableAndForeignLiveHostHint, TestSessionsJobsCmdRun_JSON
  (no flag-ON host-liveness/kill-hint surface).
- TestSessionsGcCmdRun_JobsOlderThan_PurgesTerminalOnly / _DryRunCountsOnly /
  _JSONPrintsCounts (legacy wording/kind vs pinned batch_retention;
  session_notices retention has no batch equivalent —
  batch_retention.go:17-19).
- TestSessionsCancel_PurgesQueuedWork (cause not established; above).
- TestHandleGetSessionLiveWork_RunningAndTerminalCommands,
  TestHandleGetSessionLiveWork_TerminalCausesAreDistinct,
  TestBuildSessionLiveWork_TerminalCap (flat ON path reads nonterminal roots
  only, batch_read.sql:1-6; cause CHECK vocabulary, migration :30).
- TestAuditRules_UnfinishedJobs_Boundary, TestAuditRules_Integration
  (flag-ON detail is toolCallID:rootState, interrupted is terminal),
  TestAuditRules_SchemaDegradation_IsSilent (batch block always printed
  flag-ON; no-batch_nodes refused by design).
