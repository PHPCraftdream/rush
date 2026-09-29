# W-RERUN design (ox consultation, review round 1)

Design for the two W-RERUN problems of `2026-09-29-async-phase4-round1.md`: atomic Rerun truncation (A2/B16/C11) and a
durable cross-process driver marker (C7). Verified against `phase4-core` at `babd4c8e`.

## 0. Code facts beyond the review text

1. The leading `Cancel` in the Rerun handler is the full Stop (`coordinator_interrupt.go`): stops every job in the tree incl. kept
   history, sets `wake=0` on every pending/done row in the tree, `suspendAutoResume` on every id (only the root re-armed later), and
   for a child session also stops the parent's delegation via `byChild`.
2. **New P2: voiding by `tool_call_id` is unsound when a provider reuses call ids per response (B14).** (a) kept history has a running
   async `call_0`, the tail has a non-async `call_0`: `VoidAsyncJobsByToolCallIDs` voids the kept row and `stopToolCallsForRerun`
   kills the kept job. (b) both `call_0` are in the tail and the first row was archived to `call_0#reused#…` with its notice in the
   tail: re-pend finds it by notice id, void misses it by text → a notice for a deleted call.
3. `stopToolCallsForRerun` calls `l.cancelTree(child)` (ledger jobs only); it never cancels the child's live generation and never
   zeroes wake / suspends like `coordinator.Cancel`. The leading Stop hid this.
4. wake_only notices of voided jobs still deliver: the pull-time void for wake_only (`notice_pull.go`) checks only `state != 'running'`.
5. The CLI process never drains the recheck set (ticker starts only in `SetPersistentMode(true)`).
6. C4 has landed: `ReleaseExternalDriver` is a no-op when `!persistentMode`; the durable release of Problem 2 must bypass that guard.
7. Migration order: goose `Provider.Up` has no out-of-order option; land `…03` before `…04`. Both problems regenerate sqlc — regenerate after merging.
8. `internal/message/message.go` is ~945 lines: put `DeleteTx`'s body in a new file.

## Problem 1 — Rerun truncation is all-or-nothing

Rule: **a Rerun commits exactly once.** One writer transaction T deletes the tail and the target and reconciles the ledger against
exactly the rows T deleted. Before T nothing changed; after T the rerun proceeds. Only the deleted tail's jobs are stopped, only after T commits.

**Migration `20260929000003_add_announce_message_id_to_async_jobs.sql`:** `ALTER TABLE async_jobs ADD COLUMN announce_message_id TEXT;` (Down: drop column; goose StatementBegin/End like the claim_id migration).

**SQL** (`async_jobs.sql`):
- `SetAsyncJobAnnounceMessageID :execrows` — `UPDATE async_jobs SET announce_message_id=?, updated_at=? WHERE owner_session_id=? AND tool_call_id=?`.
- `VoidAsyncJobsByAnnounceMessageIDs :many` — `UPDATE … SET delivery='void', updated_at=@updated_at WHERE owner_session_id=@owner AND announce_message_id IN (sqlc.slice('message_ids')) RETURNING tool_call_id, state, child_session_id, host_id`.
- `VoidAsyncJobsByToolCallIDs` becomes `:many` and only the legacy arm: add `AND announce_message_id IS NULL`, same RETURNING.
`session_notices.sql`: `RependSessionNoticesByMessageIDs` gets `AND kind <> 'wake_failed'`; new `VoidWakeFailedNoticesByMessageIDs :execrows` (`delivery='void' WHERE owner=? AND kind='wake_failed' AND delivery='done' AND notice_message_id IN (…)`).
`messages.sql`: `DeleteSessionMessagesByIDs :many` — `DELETE FROM messages WHERE session_id=@session_id AND id IN (sqlc.slice('ids')) RETURNING *`.

**Message service:** `DeleteTx(ctx, tx *sql.Tx, sessionID string, ids []string) (deleted []Message, publish func(), err error)` in `internal/message/message_delete_tx.go`; deletes unconditionally (same three proofs as today's ForceDelete path: session cancelled + polled idle, handler holds exclusive reservation, shared session-lock probe held); chunks ids at 500; publishes nothing itself — `publish()` bumps `DeleteGeneration` and `PublishMustDeliver(DeletedEvent)` per deleted row, called only after commit.

**Session package** (`async_job_rerun.go`, replaces `AsyncJobStore.RerunTruncate`):
```go
type RerunTruncateParams struct{ Owner, TargetID string; TailIDs []string }
type VoidedAsyncJob struct{ ToolCallID, State, ChildSessionID, HostID string }
type RerunTruncation struct{ Deleted []message.Message; Voided []VoidedAsyncJob }
var ErrRerunTargetGone = errors.New("rerun: target message no longer exists")
var rerunTruncateStepSeam func(step string) error // test-only
func TruncateForRerun(ctx, sqlDB *sql.DB, messages message.Service, p RerunTruncateParams) (RerunTruncation, error)
```
T (one `BeginTx`, deferred Rollback): (1) `DeleteTx(owner, [target]+TailIDs)`; target not among returned rows → `ErrRerunTargetGone`, rollback. (2) From rows actually returned (target excluded) build `deletedIDs` and `tailToolCallIDs`. (3) `RependAsyncJobsByNoticeMessageIDs(deletedIDs)` (keeps `delivery='done'` guard), `RependSessionNoticesByMessageIDs`, `VoidWakeFailedNoticesByMessageIDs`. (4) `VoidAsyncJobsByAnnounceMessageIDs(deletedIDs)` then `VoidAsyncJobsByToolCallIDs(tailToolCallIDs)` (legacy arm) — voids after re-pends so void wins; collect RETURNING into `Voided`. (5) seam, commit, `publish()`. All IN-lists chunked at 500.

**Ack gate:** `AnnounceStarted` (`notice_pull.go`) also runs `SetAsyncJobAnnounceMessageID(msg.ID)` inside its existing tx; archived rows keep the column (fixes the id-reuse cases). **Pull-time:** in `sessionNoticeVoidCondition` wake_only returns `job.State != "running" || job.Delivery == "void"`.

**Agent layer:** `Coordinator` drops `RerunTruncateAsyncJobs`, adds `CancelTurn(sessionID)` (`agentFor(id).Cancel(id)` only — no job stop, no tree walk, no suspend, no wake zeroing) and `StopRerunJobs(ctx, sessionID, voided []session.VoidedAsyncJob)` (for `State=="running"` rows → `stopToolCallsForRerun` with job_kill semantics, this process's executors only, log foreign-host rows; for every row with `ChildSessionID` → `c.stopTree(child)`). `stopTree(root)` = the current body of `Cancel` extracted + nil-agent guard; `Cancel` = `c.stopTree(sessionID)`. `stopToolCallsForRerun` loses its internal `cancelTree` loop and return value.

**Handler flow** (`handlers_agent_rerun.go`): validate as today → `CancelTurn(sessionID)`, `ClearQueue`, idle poll, `ReserveExclusive`, shared probe, `List`, `targetIdx` (unchanged) → `tailIDs` from `allMsgs[targetIdx+1:]` → `rerunHoldingReservationSeam` → **last cancel point:** `holdCtx.Err()` → reply "cancelled" (nothing changed) → `deleteCtx := WithoutCancel(holdCtx)`; `trunc, err := rerunTruncate(deleteCtx, a.DB(), a.Messages, …)` (`rerunTruncate` seam var in `handlers_agent.go`, default `session.TruncateForRerun`); on error: `ErrRerunTargetGone` → "target message not found in session", else "rerun failed, nothing was changed — please retry: …", return (defers release reservation and probe) → **commit point:** register the recreate-prompt defer immediately (`baselineIDs` seeded from `allMsgs`, union the post-commit `List`), fire `rerunPostTruncateSeam`, run `StopRerunJobs(deleteCtx, sessionID, trunc.Voided)` synchronously best-effort → `ResetAutoResumeCounter`, overrides, probe release, handoff (unchanged). Remove `rerunTailDeleteSeam`, `rerunPreTargetDeleteSeam`; add `rerunPostTruncateSeam`, `rerunTruncate`.

**Failure/crash:** cancel before T changes nothing; failure inside T (incl. SQLITE_BUSY after 30s) rolls back, error reply, no stop, no new turn; crash before commit → rollback; crash after commit before stop → rows void, executors die with the process, recovery moves rows to `interrupted` through the single CAS which keeps `void` (no notice); a stop that fails/cannot reach a foreign executor → its completion commits `void` (not debt, not pulled), wake_only notices voided at pull.

**Decisions:** leading `Cancel` → `CancelTurn` (§3.8 limits stopping to the deleted tail; §6 "Rerun before the notice → result arrives in the new branch"; Stop's tree-wide `wake=0`/suspend are Stop semantics; the reservation + shared probe keep writers out). `wake_failed` markers whose message is in the tail → void, not re-pend (they describe an outcome of the deleted branch); other kinds re-pended.

**Rejected:** per-message deletes + reconcile after (multiple commits); deleting in the coordinator (does not own history); deleting by position inside T (also deletes lock-free `sessions inject` rows added after the listing); stopping the tail's jobs before T (kills jobs for a Rerun that can still abort); compensation on failure; keeping void by `tool_call_id`; keeping the leading Stop.

**Production files:** new `20260929000003_*.sql`, `internal/message/message_delete_tx.go`; SQL `async_jobs.sql`, `session_notices.sql`, `messages.sql` + regenerated `internal/db/*`; `internal/message/message.go` (interface line); `internal/session/async_job_rerun.go`, `notice_pull.go`; `internal/agent/coordinator.go`, `coordinator_rerun.go`, `coordinator_interrupt.go`, `work_ledger_rerun.go`; `internal/server/handlers_agent_rerun.go`, `handlers_agent.go`. Test fakes to update: coordinator fakes in `server/p1_6_regression_test.go` (`cancelCalled` → `CancelTurn`), `p2_regression`, `p595_delete_streaming`, `p614_rerun_reservation`, `p623_panic_window`, `readpump_control_starvation`, `app/p1_5_shutdown_test.go`, `app/release_gate_test.go`; message-service mock in `agent/p339_no_duplicate_execution_test.go` needs `DeleteTx`.

**Mandatory tests (real SQLite):** session — `TestTruncateForRerun_InjectedFailureBeforeCommit_ChangesNothing` (revert: run DeleteTx in its own tx), `…_TargetGone_ChangesNothing` (drop target check), `…_SetsFromActuallyDeletedRows` (a tail notice message deleted by someone else first stays `done`; revert: build sets from `p.TailIDs`), `…_VoidByAnnounceMessage_ToolCallIDReuse` (both §0.2 cases; revert: void by tool_call_id), `…_RependsNoticesVoidsWakeFailed` (drop `kind <> 'wake_failed'`), ports of the existing oracles (void beats re-pend, retry never resurrects void, unrelated rows untouched, late transition keeps void), `TestPull_WakeOnlyNoticeOfVoidedRunningJob_IsVoided` (remove `|| Delivery=="void"`). message — `TestDeleteTx_RollbackPublishesNothing` (publish inside DeleteTx). agent — `TestCancelTurn_LeavesJobsWakeAndAutonomy` (CancelTurn=Cancel), `TestStopRerunJobs_StopsRunningAndChildTree` (omit `stopTree(child)`), `TestRerunKeptRunningJob_ResultReachesNewBranch` (use Cancel). server (real App/DB, mock coordinator) — `TestHandleRerunMessage_CancelAtLastPoint_NothingChanged` (move T before the check), `…_TruncateFailure_ErrorNoStopNoRun` (log and continue; also reservation released, target intact), `…_CancelAfterCommit_Proceeds` (replaces the #630 target-delete test), `…_UsesCancelTurnNotStop`. Delete the "between tail deletes" test.

**Risks answered:** target delete moved into T is strictly stronger than #630; unconditional delete of a streaming row rests on the same three proofs; executors between commit and stop have void rows (CAS keeps void, not debt, not pulled); kept jobs finishing during the rerun queue their Drain behind the reservation and the replacement turn pulls the notice; a Drain slipping in between `CancelTurn` and `ReserveExclusive` makes the reservation fail closed with "please retry" (debt durable); legacy rows with NULL `announce_message_id` fall back to the tool_call_id arm restricted to NULL; concurrent reruns serialised by the reservation (loser gets `ErrRerunTargetGone`); tail rows deleted concurrently by someone else are not re-pended (matches plain-delete semantics); foreign-host jobs in the tail are voided but not stopped (documented residual); Rerun reaches back only as far as retention.

## Problem 2 — durable external-driver marker

Rule: **`session_drivers(session_id → host_id)` records which `rush run` loop drives a session. The driver is alive exactly when its host is alive (DUR-5 host lock, no clocks). Any other process never starts a reaction turn for that session; it may transfer notices if a Drain was already admitted.**

**Migration `20260929000004_add_session_drivers.sql`:** `CREATE TABLE session_drivers (session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE, host_id TEXT NOT NULL /* async_hosts.id, no FK */, pid INTEGER NOT NULL /* display */, claimed_at INTEGER NOT NULL /* display, never read for liveness */)` + `idx_session_drivers_host_id ON (host_id)`; Down drops both. Claim runs after `resolveSession`, so the FK is satisfied (`foreign_keys=ON`).

**SQL** (`session_drivers.sql`): `GetSessionDriver :one`; `InsertSessionDriver :execrows` (`ON CONFLICT (session_id) DO NOTHING`); `TakeOverSessionDriver :execrows` (`UPDATE … SET host_id,pid,claimed_at WHERE session_id=@session_id AND host_id=@expected_host_id`); `DeleteSessionDriver :execrows` (`WHERE session_id=? AND host_id=?`); `DeleteSessionDriversForHost :execrows`; `ListSessionDriverHostIDs :many` (`SELECT DISTINCT host_id`). Each write is a single-statement CAS: no tx, no file IO inside a tx.

**Session package** (`internal/session/session_driver.go`): `ErrDriverMarkerUnavailable`; `ErrSessionDrivenElsewhere{SessionID, HostID, PID, Status, ProbeErr}`; `SessionDriver{SessionID, HostID, PID, Status}`; `(*AsyncJobStore).ClaimSessionDriver(ctx, sessionID) error`, `ReleaseSessionDriver`, `ForeignLiveDriver(ctx, sessionID) (SessionDriver, bool, error)`, `purgeDeadSessionDrivers`.
- `ClaimSessionDriver`: `ensureHost` (failure → wrap `ErrDriverMarkerUnavailable`: filesystem without OS locks); up to 3 attempts, each reads the row on the writer connection: row names this host → nil (idempotent); names another host → `HostLiveness(host)` (shared probe; a sibling App in this process counts alive): alive/unknown → `*ErrSessionDrivenElsewhere`, dead → `TakeOverSessionDriver(expected=host)`, 1 row → nil; no row → `InsertSessionDriver`, 1 row → nil; 0 rows changed → re-observe.
- `ForeignLiveDriver`: PK read on `readQuerier()`; no row or row names this store's own host → not foreign; else `HostLiveness`; foreign iff status is not dead (unknown counts as alive).
- `ReleaseSessionDriver`: `DeleteSessionDriver(sessionID, s.HostID())` (only own claim). `Close`: `DeleteSessionDriversForHost(h.ID)` (log on failure) before `h.Close`; `CloseKeepLock` keeps the rows. `PurgeExpired`: call `purgeDeadSessionDrivers` first — for each distinct host that is not this process's, `ProbeHostShared`; dead → `DeleteSessionDriversForHost` (no lock needed: host ids are uuids never reused, death is irreversible).

**Agent layer:** `work_ledger_reaction.go`: `(l *workLedger) foreignLiveDriver(ctx, owner) (bool, error)` (nil store → false). `coordinator_drain_policy.go`: first branch of `sessionDrainPolicy` (covers `wakeSession` before submit and `decideDrainTurn` at a Drain's turn start, the latter giving "transfer only"):
```go
if c.asyncJobs != nil && !c.asyncJobs.isExternalDriver(sessionID) { // own loop: skip lookup
    foreign, err := c.asyncJobs.foreignLiveDriver(ctx, sessionID)
    switch {
    case err != nil && c.persistentMode.Load(): // web: fail closed, 60s pass retries
        c.addToRecheckSet(sessionID); return false, false, nil
    case err == nil && foreign:
        if debt, _ := c.asyncJobs.reactionDebtExists(ctx, sessionID); debt { c.addToRecheckSet(sessionID) }
        return false, false, nil
    } // err in a CLI coordinator: fall through (existing fail-open; it has no recheck ticker)
}
```
`coordinator_reaction_source.go`: `ClaimExternalDriver(ctx, sessionID) error` (durable claim first; `ErrDriverMarkerUnavailable` → WARN and carry on with the in-memory marker only; other errors returned; then set the in-memory marker); `ReleaseExternalDriver(ctx, sessionID)`: durable release always runs; the in-memory release stays behind the C4 `persistentMode` guard.

**App layer:** `app_run_request.go`: `onSessionResolved func(string) error`; `app_run.go:~87`: `if err := req.onSessionResolved(sess.ID); err != nil { return nil, err }` — before `drainPendingBeforeRun` and before every session write, so a refused claim changes nothing. `app_run_async.go`: a `driverClaimed` flag; the callback claims once then sets `sessionID`; after `ExecuteRun`, `if err != nil && !driverClaimed { return final, err }` (first turn never ran: no waiting on scope); the deferred release runs only if `driverClaimed`, with `WithTimeout(WithoutCancel(ctx), cleanupTimeout)`.

**Behaviour:** other process never submits a Drain for a session with a live foreign driver; an already-admitted Drain pulls notices at turn start (pull does not clear debt) and ends without calling the provider; the session stays in its 60s recheck set while debt exists; the CLI loop sees the debt within 5s (existing `WaitForHint` fallback) and reacts itself. Handover CLI→web: loop exit deletes the row; web→CLI: `rush run --continue S` claims and W defers. A second `rush run` on a driven session fails fast naming the pid. kill -9 of the CLI: host lock dropped → W's next hint / 60s pass sees it dead → takes over; Ctrl-C: defer deletes the row, `Close` is the backstop; forced shutdown: row counts alive until the process exits.

**Rejected:** per-session driver lock file under `drivers/` (second lock namespace, invisible to DB readers); a column on `sessions`; `driving_session_id` on `async_hosts`; heartbeat timestamp (violates DUR-5); CLI holding a shared session lock between turns (blocks web human turns/transfers, upgrade race, collides with `sessions kill/locks`); web always submitting a transfer-only Drain; a cross-process hint mechanism.

**Production files:** new `20260929000004_add_session_drivers.sql`, `internal/db/sql/session_drivers.sql` + generated `internal/db/session_drivers.sql.go`, `internal/session/session_driver.go`; regenerated `internal/db/{models,querier,db}.go`; `internal/session/async_job_store.go` (`Close`), `async_job_recovery.go` (`PurgeExpired`); `internal/agent/work_ledger_reaction.go`, `coordinator_drain_policy.go`, `coordinator_reaction_source.go`; `internal/app/app_run_async.go`, `app_run_request.go`, `app_run.go`. Test fakes: `app/app_run_reviewer_scope_retry_test.go`, `app/app_run_async_wait_test.go`, `agent/coordinator_external_driver_release_test.go`, `agent/coordinator_wake_reaction_debt_test.go`.

**Mandatory tests:** session (two stores on one data dir) — `TestClaimSessionDriver_IdempotentReleaseAndClose` (drop the `Close` delete), `…_LiveRefused_DeadTakenOver` (A sibling alive blocks B; after `A.SimulateCrashForTest` B wins; skip liveness check), `…_ConcurrentTakeoverOneWinner` (8 stores race on a dead row, exactly one nil; unconditional UPDATE), `TestForeignLiveDriver_Matrix` (none/own/sibling/crashed/unknown(lock path is a directory) → false/false/true/false/true; unknown→dead, remove own check), `TestPurgeExpired_DeletesOnlyDeadDriverRows` (delete without probing). agent — `TestSessionDrainPolicy_ForeignDriver_NoDrainParkedWhileDebt` (mock `Run` count 0; recheck set holds S exactly when debt exists; remove the branch), `TestDecideDrainTurn_ForeignDriver_TransferOnly` (notice pulled, provider not called, `reacted=0`), `TestSessionDrainPolicy_OwnLoopNotBlocked` (in-memory marker cleared, own durable row → allowed; remove own-store check). app (two Apps, real providers) — `TestTwoAppScenarioE_WebJobWhileCLIDrives_RootAnswersInCLI` (W has an async bash on S, C loops `--continue S`; W's provider gets 1 request, C's gets 2, C's JSON `FinalText` is the reaction; remove the policy branch), `TestTwoAppScenarioF_DriverCrash_WebTakesOver` (crash C, run `RecheckPass` → W reacts; treat row as live without probing), `TestRunLoop_DrivenSession_FailsBeforeTurnOrMutation` (provider 0 calls; system prompt and ended_reason untouched; ignore the claim error), `TestRunLoop_DurableDriverReleasedInNonPersistentMode` (no row after the loop, in-memory marker still set (C4); put the durable release behind the `persistentMode` guard).

**Risks answered:** `rush run` now registers a host without jobs (deliberate: one lock file + one `async_hosts` row per live loop, removed on clean exit, reaped by the existing no-jobs dead-host purge after a crash; record in DUR-5); unknown is treated as alive (same rule as §3.6; debt durable, 60s pass retries, claim error names host/pid/probe error); host alive ≠ loop alive only if both release and `Close` delete fail (bounded by process lifetime); fail-closed on read errors only in the web coordinator; a second `rush run` on a driven session now fails fast (CHANGELOG entry); deleting stale rows without holding the lock is safe (host ids never reused, death irreversible, deletes scoped to the host); latency: CLI discovers web completion within 5s, web takeover after a CLI crash up to 60s; a web human turn on a driven session may react to the notice (allowed by §3.4; the CLI's summary will not contain that text).

Docs to update in the docs wave: §3.4, §3.6 (hosts also register on a driver claim), §3.8, a new DUR-10, the DUR-5 anchor, CHANGELOG.
