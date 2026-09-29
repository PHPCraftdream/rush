# Async phase 4 — review round 1 (three ox reviewers)

Scope: branch `phase4-core` (`d9078329..d1e9b5b1`), design `docs/plans/2026-09-28-async-phase4-durable-core.md` rev 7.
Reviewers: **A** durable core (SQL/schema/store/host lock/recovery/reader), **B** agent layer, **C** app/cmd/server/docs.
Per the operator's explicit exception to the Review Stop Rule, rounds continue until zero P0–P3 and P2/P3 are fixed too.
IDs below (`A1`, `B3`, `C7` …) are the reviewer letter plus the finding number in that reviewer's report; findings that two
reviewers reported are merged under one work item (`W-…`).

## Work items

### W-STORE — db / session layer (wave 1, agent `store`)

- **A1 (P1)** `reacted_failed` is permanent and unscoped. `SettleReactedFailed` marks rows `reacted=1, reacted_failed=1`; nothing
  ever clears it; `ListReactedFailedText` / `refreshSubAgentCompletion` read it unscoped. Scenario: child C's reaction fails (402),
  row J1 settled by failure, T1 reported failed. Operator fixes credits, parent resumes C (T2), C answers fine — T2 still returns
  J1's raw text with `IsError` (until J1 is purged after 7 days). Same inside one delegation: J1 settled, J2 later reacted, the
  delegation still reports failed. Fix direction: "closed by failure" is superseded by a later real reaction and belongs to one
  delegation — the real-reaction tx clears `reacted_failed` on the owner's rows it delivers; claiming a new delegation on a child
  clears stale failure flags of that child; re-pend resets `reacted_failed` and `wake_attempts`; verdict must not read unscoped rows.
- **A4 (P3)** `void` is not terminal: re-pend (`async_jobs.sql`, `session_notices.sql`) has no `delivery` guard, so a retried Rerun
  can turn a voided row back into `pending`. Fix: every writer treats `void` as final (`AND delivery='done'` on re-pend).
- **A5 / C10 (P3)** Retention purge (`async_jobs.sql`, `session_notices.sql`, `async_job_recovery.go`, `async_job_gc.go`,
  `internal/cmd/sessions_gc.go`) deletes debt (`done, wake=1, reacted=0`) after 7 days or immediately with
  `--jobs-older-than 0s`/negative. Rerun also cannot re-pend a purged row (design gap: state how far back a Rerun reaches vs
  retention). Fix: purge and count predicates exclude obligations `NOT (wake=1 AND reacted=0)`; reject ages <= 0.
- **A6 (P3)** `recoveredDelegationText` (`async_job_recovery.go:94-99,154-174`): when `messages.List(child)` fails transiently,
  recovery commits a false "finished with no textual response". Fix: on read error skip the row, leave it `running`.
- **A7 / C15 (P3)** Host lock files leak: every deleter removes the `async_hosts` row first, the file second; Windows sharing
  violation or a failed `RegisterHost` insert leaves a file with no row; retention walks rows only, so §3.6 "retention removes it"
  is false. Fix: retention also scans `hosts/*.lock`, reaps files with no row whose lock it wins.
- **A8 / C9 (P3)** Cross-process readers (`async_job_reader.go`, `annotateLiveDescendantWork`, `LiveJobs`, `JobsInTree`,
  `ReactionDebtExists`, `ListAsyncJobsForOwner`) run on the single writer connection (`SetMaxOpenConns(1)`); web session-list
  re-poll stalls behind write txs up to `busy_timeout` 30s; one probe per row, one query per owner. Fix: give the store a read
  pool connection (`ConnectRead`) for readers; one query for all running rows per poll; cache probe results per host per call.
- **A9 (P3)** `session_notices` has no plain `(owner)` index; `VisibleAsyncReactionDebtExists`, `MarkSessionNoticesReactedForOwner`,
  `ListReactedFailedSessionNoticesForOwner`, `ListSessionNoticesForOwner` full-scan (SQLite does not infer `delivery!='void'`
  from `delivery='done'`). The comment at `async_jobs.sql:203-208` is wrong. Fix: add `(owner)` index or repeat the partial-index
  terms; add a query-plan test.
- **A10 (P3)** Closing debt by failure (`SettleReactedFailed`) and writing the `wake_failed` marker are separate commits (and the
  two-table settle is not atomic): marker insert failure closes debt silently (ASYNC-09). Fix: one store method that settles both
  tables and inserts the marker in a single tx, and reports the number of rows actually settled (marker only if > 0). The agent-layer
  agent (`drain`) consumes this method; **store agent defines the API name `SettleReactedFailedWithMarker` and documents it.**
- **A12 / C8 (P3)** On forced shutdown (`stillBusy`) `asyncJobStore.Close` still releases the host lock (`app_lifecycle.go:213-267`,
  `host_lock.go:280-304`) while Run goroutines still execute — other processes then mark their rows interrupted (DUR-5 only holds for
  a process that exits). Fix: on the forced path keep the lock until process exit (as the DB close is already skipped there).
- **A13 (P3)** Tree walks (`async_job_reader.go:100,181`) stop at depth 16 with a non-empty frontier and leave `walkIncomplete=false`
  — truncated result read as complete. Fix: set `walkIncomplete` on cap exit.
- **A14 (P3)** Probes open the lock file `O_RDWR` (`host_lock.go:140,189`): a 0644 file in a shared data dir gives EACCES → `Unknown`
  forever. `flock`/`LockFileEx` work on read-only handles. Fix: open read-only for probes.
- **A15 (P3)** Pull-time void conditions (`notice_pull.go:246-269`) count `running` rows on dead hosts as scope. Fix: reuse the scope's own
  liveness predicate.
- **A14b — B14 (P3)** Async tool-call id reuse: unique key (owner, `tool_call_id`) now outlives the job (`work_ledger.go:280-289`,
  `async_job_store.go:281-283`) — a provider that numbers calls per response (`call_0`) is refused for 7 days ("already started
  earlier"/"different input"). Before phase 4 the id was freed after delivery. Fix: a row whose result is already in history
  (`delivery='done'`) is history, not an in-flight claim: a repeated id creates a new row; keep old rows addressable for Rerun.
  Choose the smallest sound design and document it in the design doc / registry.
- **Test fixes (A-b):** `TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing` (vacuous — never calls `store.Transition`;
  must prove atomicity of the real `Transition`), `TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows` (notice `wake` never
  asserted; negative case unexercised; REVERT CHECK comment cites a non-existent assertion), `TestMarkReactedWithMessageUpdate_OneTransactionClearsDebt`
  (does not prove atomicity — inject a failure between the two writes), `TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow`
  (passes without the OS lock; fix comment or make it prove the lock-level claim), `TestProbeHostLockShared_AliveDoesNotDisturbHolder`
  and `TestLiveJobs_*` (cannot tell shared from exclusive probe), `TestHostNotDead_SelfAliveDeadUnknown` (no Unknown case),
  `TestPurgeExpired_*` (no unreacted-debt case). Add: DUR-8 test with a closed/failed DB (not only the no-store case).

### W-DRAIN — agent drain/wake policy + CLI loop (wave 1, agent `drain`)

- **B1 (P1)** Release re-check runs the Drain under a 30s deadline: `supervision.go:477-492` builds `WithTimeout(Background,30s)` and passes
  it to `wakeSession` → `agent.Run` → whole `runOwned` loop. A Drain >30s hits `DeadlineExceeded`, writes "Run timeout exceeded … --timeout",
  `classifyProviderError` says terminal → debt settled by failure + `wake_failed` marker; queued user message in the same loop dies at
  30s too; a child's delegation ends failed. Fix: bound only the debt check; submit/run the Drain on a context detached from the check
  deadline (`WithoutCancel` / fire-and-forget mailbox submit).
- **B2 / C2 (P1)** Hot loop while another process holds the session OS lock: lock refusal (`agent_run.go:405-415`) returns through
  `abandonOwnershipWithHandoff` → `onSessionIdle` unconditionally (`agent_ownership.go:309-311`) → `go recheckDebtOnRelease` (pending-inclusive
  debt) → `wakeSession` → lock refused → repeat, no pause (5 queries on the writer connection + 2 WARN lines per turn) for the whole foreign
  tool call. Rule (b) says: lock held elsewhere → shared probe → recheck set, never immediate retry. Fix: a release caused by admission
  refusal counts as no-turn release (skip relaunch; add to recheck set); do the shared-lock probe before submitting.
- **B3 / C6 (P1/P2)** Released or timed-out child gets Drain turns on the root coder agent; delegation released while child owes reaction.
  `childScopeDrained` (`work_ledger_delegation.go:193-214`) ignores DB debt and other-process jobs; `sessionDrainPolicy`
  (`coordinator_drain_policy.go:39-62`) lets a child with no running delegation fall through to the web counter branch;
  `releaseDriverIfScopeClosed` removes the driver so `agentFor` returns `currentAgent`; `drainCallFor` then takes its non-driver branch —
  coder system prompt, full tool set, no `RunAllowlist`, auto-approve inherited (security-relevant). Up to 5 orphaned turns, results never
  seen by the parent. After one transient failure the parent is told failed instead of after K=3. Same for `terminate_and_wake` timeout.
  Fix: key child policy on durable identity (ever a delegation target / `ParentSessionID` set); with no running delegation `wakeSession`
  refuses (never re-routes to `currentAgent`); `childScopeDrained` includes DB debt and running rows on non-dead hosts (= `ScopeOpen`
  without the mid-turn check).
- **B4 (P1)** Auto-summarize during a Drain turn drops the continuation: `agent_turn.go:759-773` copies `call` (keeps `IsDrain`,
  `drainTurnCommitted=false`); next `runTurn` re-gates via `decideDrainTurn`, finds no visible debt (already reacted), takes the no-turn
  branch; run ends with tool results unanswered ("successfully" in `rush run` with an incomplete task). If other debt exists the
  continuation persists a user message ending "initial user request was: ``". Fix: one constructor for continuations that marks the
  Drain committed, or make the compaction continuation a non-Drain call.
- **B5 / C3 (P2)** Settle-by-failure treats cancellation and non-provider errors as terminal and writes the marker unconditionally
  (`coordinator_drain_policy.go:78-89,114-118,159-165`, `coordinator_retry_classify.go:81,129`; CLI: `app_run_async.go:147-150` records the outcome
  before the `ctx.Err()` check). Wrongly settled: user Stop during a Drain, `CancelAll`/shutdown with a Drain in flight (breaks "graceful
  exit = crash"), Ctrl-C / `--timeout`, watchdog stall (reported as Canceled), DB error in preamble, `AwaitingAnswerError`; `runErr` may belong to a
  queued user turn behind a Drain that did react; marker text uses pseudo-ids (`release-recheck`, `cli-loop`). Fix: classify from the Drain's own
  attempt evidence (its assistant-message finish reason, like `shouldRetryTurn`); cancellation/deadline/non-provider errors = "no attempt"
  (recheck set); marker only if rows were actually settled (uses `SettleReactedFailedWithMarker` from W-STORE).
- **B6 / C5a (P2)** Transient failure retried immediately: a failed Drain's own release starts the re-check which re-submits at once
  (rule (a) marker covers only no-turn Drains) → K=3 exhausted within seconds by one 503; violates §3.4 rule (c) and §6 "one-minute outage
  does not close the debt". Fix: extend rule (a) to releases of failed Drains; retries only via recheck set / 60s pass.
- **B7 (P2)** Web auto-turn cap (`coordinator_wake.go:112-124`) counts Drains that never ran a turn (queued/no-turn) and now applies to async-job
  wakes (regression; before phase 4 async/delegation wakes had no cap); a queued Drain refuses itself by its own bump; failing pulls are not
  added to the recheck set. Fix: count only Drains that reached the provider (in `runTurn` after `decideDrainTurn` returns true); add pull failures
  to the recheck set. Decide and document whether async wakes are capped.
- **B8 / C5b,c (P2)** CLI root loop spins without bound when debt is stuck `pending` (`app_run_async.go:231-241` uses pending-inclusive
  `ReactionDebtExists`; `decideDrainTurn` uses visible debt) — no-turn Drain returns `ErrRunQueued`, `drainNoTurn` records nothing, loop
  restarts at once, 100% CPU, never exits; also reaction write that always fails (fantasy swallows `OnStepFinish` error) → unlimited paid turns
  (web stops at 5). `checkStuckDrainProgress` is cited in `coordinator_drain_policy.go:131` but does not exist. Fix: same launch accounting as the
  web path; wait for a hint or real tick after a failed/no-turn Drain; count Drains whose snapshot did not clear; one predicate for "turn needed"
  and "turn run".
- **C1 (P1)** `rush run --session <busy>` now waits silently and keeps mutating another process's session: `app_run_async.go:126-139` retries
  `ExecuteRun` on `SessionLockBusyError` with no `firstTurn` guard and no limit; each retry repeats pre-turn writes on S (`UpdateSystemPrompt`,
  `UpdateReasoningEffort`, `ClearCancelRequest` — erases `sessions cancel` within 500ms —, `SetBudget`, `SetEndedReason("")`, deferred
  `SetEndedReason(fail)`). Before phase 4 this was a fast error; `sessions_kill.go:26` help and `sessionBusyGuidance` still describe it. Fix
  (root cause: `ExecuteRun` is per-invocation and mutates the session, used as per-turn primitive): invocation setup once; Drain turns/retries
  mutation-free; lock-busy retry only for Drain turns with an overall time limit and stderr message; first turn fails fast.
- **C4 (P2)** Loop releases external-driver marker before the on-finish hook and before `App.Shutdown` (`app_run_async.go:67-72,87-91`,
  `cmd/run.go:736`, `run_on_finish.go:21-50`): a job finishing in that window starts a paid Drain on `Background` in a shutting-down process.
  Fix: in a non-persistent (CLI) coordinator keep the root hint-only for the life of the process (or release only after `CancelAll`).
- **B12 / C14 (P3)** 60s pass: `coordinator_recheck.go:75-77` calls `recheckChild(parent)` with owner ids from `parkedParentSessions()` (never re-checks
  a child); `:79-87` runs `wakeSession` synchronously so Drain turns run serially inside the ticker, delaying sweep and purge; `rush run`
  never starts the ticker (`coordinator.go:520-528`, only `root.go:176` calls `SetPersistentMode(true)`), so its recheck set is never drained and
  dead-host sweep/retention never run — CHANGELOG/`--jobs-older-than` help promise them. §3.5 says the 60s pass; the loop polls 5s. Fix: pass
  actually re-checks children, wakes asynchronously; a CLI-only process gets the pass (or the docs say otherwise — prefer fix).
- **B13 (P3)** Stale no-turn marker leaks onto a later real turn (`agent_turn.go:393-401`, `drainOrReleaseMerged`, `supervision.go:455-461`).
- **B17 (P3)** `reclaimReplacementOrKeep` (`mailbox_interrupt.go:295`) re-queues a displaced Drain at the head bypassing `mergeQueuedCall`.
- **B18 (P3)** Child driver agents are not in `CancelAll` (`currentAgent.CancelAll()` only): their Drain turns (Background ctx) survive
  shutdown; the release hook performs an unbounded-retry DB transition (`recheckChild` → `commitTransition`) synchronously in `runOwned`'s defer.
- **C16 (P3)** Reviewer-pass check (`app_run.go:546-550`): DB error on last turn skips the review silently (§3.5 says retry); `ScopeOpen` runs
  recovery writes and exclusive lock probes on every `ExecuteRun`, even without a reviewer, SDK runs included.
- **C17 (P3)** CLI-loop waits/exits: DB error retried forever with only a WARN in the log; `Unknown` host keeps scope open to 6h with no stderr message;
  several exits skip the JSON envelope / terse output / `ExitReason` (lock-busy exit `:135-137`, wait-error `:175-181`, `ctx.Err()` `:166-168`).
- **C18 / B-dev1,2 (P3)** Settle snapshot includes `pending` rows (pull may have failed) — doc says only rows `done` at turn start; settle is written
  after the session is released, not "while still owning".
- **B-dev3** Web Drains have no coordinator retries/continuations (`wakeSession` calls `agent.Run` directly) — doc says "after coordinator retries".
- **Policy table rows not enforced (B-dev6):** "web tab: pull only / no turn", "Background shell: only with `AutoResumeOnJobDone`" (enforced at hint
  time only; release-recheck ignores it); Rerun's leading `Cancel(sessionID)` kills every job (also from kept history) — decide/document.
- **Tests (B-b, C-b):** settle tests whose mock never fires `onSessionIdle` (`TestSettleByFailure_*`) — must go through the real release hook;
  `TestSettleByFailure_AdmissionRefusalDuringShutdown_DoesNotSettle` (add turn cancelled by `CancelAll`);
  `TestRecheckSet_SessionLockBusyGoesIntoRecheckSet_RetriedByPass` (fake bypasses `runOwned`/abandon — cannot see B2);
  `TestStop_OneSecondAfterNaturalFinish_NoNewTurn` (revert-check stale) and scenario 13 (independent revert-check);
  `TestTwoAppScenarioD` (job-not-in-prompt check always passes; row-deletion passes if either recovery point works; REVERT CHECK note impossible —
  re-run the revert-check, either fix the note or find turn-start recovery broken); `TestTwoAppScenarioC` (300ms `Never` window vs ~3s jobs; goes
  through `store.Close` not `CancelAll`); `TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts` (single-process). Missing: Drain >30s after
  release re-check, Drain + compaction continuation, routing of a released child, CLI no-turn loop, `RecordDrainTurnOutcome`/`CaptureDrainSnapshot`
  (incl. exit_reason via `rush run`), Ctrl-C during Drain, busy-session retry, DB-error retry.

### W-LEDGER — work ledger internals (wave 2, agent `ledger`; may touch db for `claim_id` after W-STORE merged)

- **B9 (P3)** `close()` suppresses natural completions racing shutdown: `work_ledger.go:813-815` sets `shutdownCancelled` on every job incl. ones whose
  executor already returned; `work_ledger_transition.go:232-238,253-258` stops retry on `closedCh`. Row stays `running`, next host says "interrupted, output
  not saved" (violates §3.1). Fix: set the latch only when `close()` actually cancelled a still-running executor.
- **B10 (P3)** Stop between Claim and the "started" ack orphans the row (`work_ledger_transition.go:324-331` drops the `stoppedBySession` job without
  checking `announced`; ack then `job==nil`, `handled=false`) → row `announced=0`/`cancelled`/`pending` forever; model saw "started", never learns outcome.
  Fix: drop only when `announced` is set.
- **B11 (P3)** `job_kill` reports "stopped" after losing the race to natural finish (`work_ledger.go:605-627,703-732`): the natural finish committed with
  `wake=1`, kill's own transition skipped, tool still says stopped while a "finished" notice + Drain follow. `killRequested` comment (`work_job.go:156-160`) mismatches
  the bash path (`job_kill.go:110-129` still calls `KillOwned`). Fix: `commitTransition` returns the committed outcome; tool answers from the committed row.
- **B15 (P3)** Ack gate holes: `ErrAsyncJobGone` rolls back the tool-result insert (`notice_pull.go:324-326`, `work_ledger_announce.go:49-51`) leaving a `tool_use` with
  no result; any tool result for a job in the ledger is fused as "started" — a panic between `Start` and `go t.run` (`async_tool.go:77-118`) is announced and the job stays
  running forever. Fix: persist an error tool result when the row is gone; announce only after the executor is actually started.
- **A11 (P3)** Transition CAS key has no claim generation (ABA): abort deletes row R1, provider repeats the id, R2 is created, E1's late result commits as E2's.
  Fix: random `claim_id` from Claim carried by the executor and included in the CAS `WHERE` (needs SQL change → new migration, not an edit).
- **A3 (P2)** `job_kill` delivers a result without recording which message carried it: row `done, reacted=1` has no `notice_message_id` (`work_ledger_transition.go:107-109`,
  `async_jobs.sql:114-119,130-131`; only writer is the pull) — Rerun after a `job_kill` cannot re-pend it. Fix: law "`delivery='done'` ⇒ the row names the message carrying
  the result": write the `job_kill` tool-result message id into `notice_message_id` in the same tx that persists the tool result (pattern of `AnnounceStarted`).
- **B-dev9** `wake_only` for sync jobs still produces a session notice and a wake (`work_ledger_timeout.go:195-226`, no `sync` check).
- **Tests:** `TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed` (finish before close — race untested), `TestWorkLedger_StartFailsClosedWhenStoreUnavailable` (`executorStarted`
  vacuous), `TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes` (checks memory callback, not DB `wake`), `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome`
  (tool answer vs committed outcome), `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction` (passes with two writes),
  `TestPullJobNotices_JobKillProducesExactlyOneStoppedNotice` (stale: pins step-3 contract; production is `causeJobKill` → done/reacted=1). Missing: ack-gate `ErrAsyncJobGone`
  tool-result persistence.

### W-RERUN — Rerun atomicity + durable external driver (wave 3, agent `rerun`)

- **A2 / B16 / C11 (P2)** Rerun commits ledger reconciliation before the last cancel point and separately from deleting the messages (`handlers_agent_rerun.go:215-297`,
  `coordinator_rerun.go:24-43`, `async_job_rerun.go:37-76`): Cancel after reconciliation → running rows killed, notices re-pended (duplicate M/M′), tool call B void with call still in
  history; a failed `Messages.Delete(M)` or failed `RerunTruncate` does the same; reconciliation error is only logged; `SetWakeZeroForOwners` error returns before `RerunTruncate` is called.
  Fix: reconciliation and tail deletion in one DB tx, re-pend/void sets computed from messages actually deleted; a failure aborts the Rerun; reconciliation after the last cancel point.
- **C7 (P2)** The "external driver" marker is per process (`work_ledger_reaction.go:105-135`, `coordinator_wake.go:63-83`): a web process W runs a full paid Drain turn on a CLI
  root S between the CLI's turns (lock free), the CLI loop then exits with turn 1's text and misses the reaction; violates the policy table ("web tab that opened the session — pull only")
  and §6 "root still answers, summary in JSON". Fix: record the driving process durably (session driver row/column keyed to a host id + liveness by host lock); other processes route such
  sessions to transfer-only. New migration, not an edit.
- **B-dev8 / C-dev** Rerun handler's leading `Cancel` stops all jobs incl. kept history (contradicts §6 "Rerun before the notice → result arrives in the new branch") — decide/document.
- **Tests:** Rerun handler ordering/failure tests; two-process test for C7.

### W-CMD — cmd / server small items (wave 3 with W-RERUN or wave 4)

- **C12 (P3)** `sessions why` (`sessions_why.go:451`) says "reaction debt: none" for pending debt (DUR-4 debt includes pending).
- **C13 (P3)** `sessions jobs` hint (`sessions_jobs.go:163-165`) tells to `rush sessions kill <owner>` which does nothing between turns (host holds the host lock, not the session lock). Name host PID and a kill command for that process.
- **C19 (P3)** `sessions gc --jobs-older-than --json` prints no job/notice counts (`sessions_gc.go:149`).
- **Tests:** `TestSessionsGcCmdRun_JobsOlderThan_DryRunCountsOnly` (`_ = stdout`, never checks the count), `..._PurgesTerminalOnly` (no announced/wake=1/done/unreacted row; "old-terminal" row is `announced=0`),
  `TestExplainSessionStatus_AsyncJobsAndDebtSection` (no pending-debt case), `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (cited as DUR-9 proof but performs no Stop).

### W-DOCS — docs / registry / comments (final wave, after code settles)

- **C20** CHANGELOG: "Observable behavior is unchanged … except" is false. Missing: busy-session behavior, web 5-auto-turn cap applying to async/delegation wakes, Stop pauses automatic turns until the next
  human message, Stop leaves a "cancelled" notice per stopped plain job (delivered on the next turn), Rerun stop/void/re-pend, all session notices carry `AutoResumed`/`BackgroundJobNotice` (line ~104 says they do not),
  7-day retention on CLI-only installs. Notices visible only when a turn pulls them.
- **C21 / B-dev11** Wrong comments: CLI 60s tick (`coordinator_reaction_source.go:74-77`, `coordinator.go:520-529`, `coordinator_recheck.go:8-9,94`, `coordinator_background.go:94-100`), `checkStuckDrainProgress`
  (`coordinator_drain_policy.go:131`), `sessionDrainPolicy` "by construction", `killRequested` (`work_job.go:156-160`), `wire.go` `isHumanTyped` (`wakeNoticeCall` removed), `UpdateTx` doc ("nil func"), `async_jobs.sql:203-208`.
- **C22 / A-dev / B-dev10** `docs/async-invariants.md` overclaims: ASYNC-02 (`ScopeOpen` covers "mid-turn OR policy allows" — checks neither), DUR-4 ("Выполняется" though visible-debt-only Drain, retention deletes debt, CLI no limit, cancel settles), DUR-5 (forced shutdown),
  DUR-9 (test performs no Stop), ASYNC-10 (`sessions why` none for pending; CLI root waiting on own job shows done), DUR-1 row omits `closedCh`, DUR-2 cites a vacuous test, DUR-9/ASYNC-08 cite `TestSetWakeZeroForOwners…` (notices half asserts nothing), DUR-4 lists two `reacted=1`
  writers, `job_kill` is a third. Design-doc deviations to re-state: `ScopeOpen` `HostNotDead` uses exclusive `ProbeHost` for a read-only decision; `async_hosts.label` documented `'cli'|'web'` but code writes `"app"`; supervision "moved notice → `reacted=1`" half not implemented.
- **Migration caution (A):** the migration was edited in place three times; a data dir that already ran an intermediate build keeps the old schema. From now on: **new migration files only.**

## Verified correct (do not re-review next round unless touched)

Transactions (`claimOnce`, `Transition`, `pullOne*`, `AnnounceStarted`, `MarkReactedWithMessageUpdate`, `RerunTruncate` are single writer txs, pure build callbacks, events after commit, no self-deadlock);
Claim atomicity and unique partial index; CAS and pull guards; debt predicate identical in SQL/Go/indexes; host liveness handle semantics (CLOEXEC, per-open-file-description locks, no FILE_SHARE_DELETE, SameFile before unlink);
recovery idempotence and "writes no messages"; `CreateTaskSession` idempotency; migration up/down symmetry; transition latch/locking order; pull-per-row txs and `announced=1` requirement; carried splices; `reacted` marking correctness;
Drain calls (no empty user row, `ErrEmptyPrompt` lifted only for Drains, 401 rebuild keeps `IsDrain`, hidden from queue, never durable); `job_kill` single CAS; Stop `cancelTree` (BFS + seen set); ack gate fused tx; Start fails closed; shutdown plumbing selects on `closedCh`;
message service `CreateTx`/`UpdateTx`; `gc` never deletes running rows; read-only commands use shared probes; CLI loop order; `IsOwnHostID`; shutdown order; Rerun reconciliation runs under reservation + shared lock.
