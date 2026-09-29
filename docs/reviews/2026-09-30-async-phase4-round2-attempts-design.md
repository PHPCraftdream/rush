# W2-ATTEMPTS design (ox consultation, review round 2)

One accounting point and one launch gate for Drain turns, replacing the scattered heuristics (release markers, pseudo-ids, probe,
per-launcher accounting) that two review rounds keep finding holes in. Verified by the consultant against the code at `1528e5a4`.
Findings addressed: section W2-ATTEMPTS of `2026-09-30-async-phase4-round2.md`.

## 0. What the code adds to the findings

- R2B-1(C)/(D) are worse in the CLI: a CLI Drain goes through `runInternal`'s retry loop (`coordinator_run.go`), and `shouldRetryTurn` retries
  "Stream stalled" and "nil err + error finish without progress" up to 2 more times → up to 3 paid calls per loop iteration, then immediate relaunch.
  Web: (D) is paced (B6 block in `agent_run.go`) but never counted (`Canceled` → recheck set): one stall per ~60s forever; (C) empty stream returns
  `err==nil` → no B6 marker → hot loop via `onSessionIdleHook`.
- R2B-1(A): `enforceRunawayCaps`' error is swallowed by fantasy (`_ = opts.OnStepFinish(...)`), the turn ends as `context.Canceled` with
  `FinishReasonCanceled "User canceled request"`.
- `wakeSession` bumps the hint unconditionally (`coordinator_wake.go`), incl. release and tick launches; `recheckDebtOnRelease` also bumps for an external
  driver (`supervision.go`): hint-based pacing is fed by non-facts → those bumps go.
- R2B-17 second path: `runInternal`'s `defer c.noteSubAgentChildRunEnded` (`coordinator_run.go`) runs the same synchronous `ScopeOpen` on every `coordinator.Run`.
- Latent: a child's Stop suspension is lifted only by a human message on that child; a parent resuming the child via `resume_session_id` after a Stop
  never lifts it → child Drains refused forever. The design reuses suspension for pending questions, so the lift is added (`runSubAgent`).
- R2B-7: `TryHoldSessionLockShared` is also used by server rerun/silence-proof code; only the probe inside `wakeSession` is deleted.
- R2B-4 CLI: a peak-hours refusal reaches `RecordDrainTurnOutcome`; `isProviderClassifiable(errProviderPeakHours)` is true → `classTerminal` → `settleAndMark` settles the pre-turn snapshot.

## 1. The design

### 1.1 Principle
- **Accounting:** a Drain attempt is accounted by the turn loop that ran it, exactly once per leg, after the leg ends and before the session is released,
  from the DB's own verdict (are the rows this attempt could see still unreacted?). The launcher never accounts (it cannot see queued/merged Drains; the release
  hook fires inside `Run` before the launcher regains control — the reason all the release markers exist).
- **Launching:** every launch decision reads one predicate `drainPermitted` = session policy, then a per-session in-memory launch gate. The gate is written only by the
  accounting function, the refusal note, and the human-message reset. Consumers: `wakeSession` (web + children), `CLIScope` (CLI root), `decideDrainTurn` (commit time),
  the child's delegation release, the reviewer gate.

### 1.2 Types and signatures
New file `internal/agent/drain_attempt.go` (~230 lines):
```go
type drainOutcome uint8
const ( drainNotAttempted drainOutcome = iota /* refused/preamble error before the provider */; drainNoTurn /* admitted, commit said no */; drainAttempted /* agent.Stream called */ )
type drainAttempt struct {
    sessionID string
    hintAt uint64                 // hintSeq read BEFORE the turn-start pull
    snapshot session.DebtSnapshot // visible debt AFTER this Drain's own pull (store2-keyed)
    outcome drainOutcome
    pendingLeft bool              // no-turn: pending-inclusive debt remains (pull failing)
    commitNo drainVerdict        // no-turn: policy/gate verdict at commit
    stalled bool                  // watchdog fire (surfaces as context.Canceled)
    capAbort bool                 // --max-cost/--max-tokens (enforceRunawayCaps)
    closed bool                   // exactly-once guard
}
func (a *sessionAgent) closeDrainAttempt(att *drainAttempt, turnErr error)          // idempotent -> coord.accountDrainAttempt
func (c *coordinator) accountDrainAttempt(ctx context.Context, att *drainAttempt, turnErr error) // THE accounting function
func (c *coordinator) noteDrainRefused(sessionID string, cause error)               // the one admission-refusal handler
func (c *coordinator) settleDrainDebt(ctx context.Context, sessionID string, snap session.DebtSnapshot, cause string) // was settleAndMark
func drainAttemptExempt(att *drainAttempt, err error) bool  // !stalled && !capAbort && (Canceled || DeadlineExceeded)
func drainFailureTerminal(err error) bool                   // *fantasy.ProviderError classTerminal (401/402/quota/4xx/ctx-too-large); NOT peak hours
var ErrDrainNotAttempted = errors.New("drain turn not attempted")
type DrainNotAttemptedError struct{ Err error } // Unwrap()->Err, Is(ErrDrainNotAttempted)
var drainRetryAfterFailure = 60 * time.Second       // var: tests shrink it
var drainRefusalPauseLoop  = 500 * time.Millisecond // CLI root (external driver) only
const drainDormantStreak = 3
```
Gate: fields on `sessionJobs` (`work_ledger.go`, under `l.mu`, next to `hintSeq`), methods in `work_ledger_reaction.go`; they replace `noTurnDrainRelease`, `noTurnDrainHintSeq`, `admissionRefusedRelease`.
```go
type drainGate struct { retryAt time.Time; hintAt uint64; hintOpens bool /* may a NEWER fact hint open the gate early? false after a paid failure */; streak int /* consecutive unreacted outcomes; >=3 = dormant */ }
func (l *workLedger) drainGateOpen(owner string, now time.Time) (open, dormant bool, retryAt time.Time)
func (l *workLedger) paceDrainGate(owner string, hintAt uint64, wait time.Duration, hintOpens, unreacted bool)
func (l *workLedger) resetDrainGate(owner string)
// open = retryAt.IsZero() || (hintOpens && hintSeq != hintAt) || (streak < 3 && now >= retryAt)
```
Decision functions in `coordinator_drain_policy.go`:
```go
type drainVerdict struct { kind drainVerdictKind /* allow|deferred|paced|stuck */; retryAt time.Time; recheck bool /* deferred but worth a tick: foreign driver, rerun hold, unreadable policy */; reason string }
func (c *coordinator) drainPolicy(ctx, s string) drainVerdict     // allow|deferred; read error => deferred+recheck (FAIL CLOSED, every session)
func (c *coordinator) drainPermitted(ctx, s string) drainVerdict  // drainPolicy, then the gate
func (c *coordinator) drainDecision(ctx, s string) (debt bool, v drainVerdict, err error) // pending-inclusive debt + drainPermitted
func (a *sessionAgent) decideDrainTurn(ctx, s string, snap session.DebtSnapshot) (bool, drainVerdict) // !snap.Empty() && drainPermitted
func (c *coordinator) wakeSession(ctx context.Context, sessionID string, fact bool) error // fact => bumpHint first
```
`drainPolicy` = today's `sessionDrainPolicy` rows minus the `counted` return and minus the capped branch; adds: pending question (reuses `autoTurnsSuspended`), rerun hold (`turnHolds` refcount),
a session driven by this process's own loop (`isExternalDriver`) skips the delegation-child refusal (R2C-5).
`CLIScope` (`coordinator_reaction_source.go`): `type DrainState uint8 // DrainNone|DrainOwed|DrainPaced|DrainDeferred|DrainStuck`; `type CLIScopeState struct { WorkOpen bool; Drain DrainState; RetryAt time.Time; Reason string }`.
`ReactionDebtSource` keeps `ClaimExternalDriver` (now also starts the ticker), `ReleaseExternalDriver`, `CLIScope`, `WaitForHint(ctx, s, until time.Time)`, `RunMaintenanceSweep`; loses `ScopeOpen`, `CaptureDrainSnapshot`, `RecordDrainTurnOutcome`.
Rerun hold: optional interface `AutoTurnHolder interface{ HoldAutomaticTurns(sessionID string) (release func()) }`, type-asserted like `ParkedSubAgentWorkReporter`.

### 1.3 Persistence
DB: NO new columns/migrations — only existing `wake_attempts`, `reacted`, `reacted_failed` + the `wake_failed` marker, through store2's rekeyed snapshot/settle API.
In memory per coordinator: the gate, `autoTurnsSuspended` (Stop + pending questions), `turnHolds` (rerun), `consecutiveAutoResumes` (bg-shell cap only). Pacing is a per-process clock concern; the durable bound is K=3 per row.

### 1.4 `accountDrainAttempt` (detached ctx, 30s bound; DB I/O outside `l.mu`, only gate writes under it)
```
notAttempted            -> noteDrainRefused(s)
noTurn, pendingLeft     -> pace(att.hintAt, 60s, hintOpens=true, unreacted=true); recheck set
noTurn, no debt at all  -> resetDrainGate
noTurn, commit verdict  -> gate unchanged; recheck set iff verdict.recheck
attempted, exempt       -> nothing (Stop, shutdown, Ctrl-C, --timeout, interrupt/replace, `sessions cancel`)
attempted, otherwise:
   IncrementWakeAttempts(snapshot)      // only rows still wake=1, reacted=0, delivery='done', same notice_message_id
   max := MaxWakeAttempts(snapshot)     // 0 => every visible row reacted
   max == 0                             -> resetDrainGate
   max >= 3 || drainFailureTerminal(e)  -> settleDrainDebt (marker names the snapshot's real tool_call_ids)
   pace(att.hintAt, 60s, hintOpens=false, unreacted=true); recheck set
   increment/read error                 -> pace + recheck (not counted); dormant stops it after 3
```
`noteDrainRefused`: pace with `hintAt = hintSeq now`, `hintOpens=true`, `streak` unchanged; wait = `drainRefusalPauseLoop` if `isExternalDriver(s)` else 60s; add to recheck set unless external driver. Never counted/settled.
Inputs: "reached the provider?" = a flag set immediately before `agent.Stream` (replaces `onDrainTurnStarting`, which fired before `getSessionMessages`). Finish reason is NOT an input: the `reacted` column after the attempt is the evidence
(robust to fantasy swallowing `OnStepFinish` errors, `stepIsReaction` false on empty streams, swallowed reaction writes); the two facts an error cannot carry are flags `stalled`, `capAbort`. Snapshot = all visible unreacted debt AFTER the Drain's own pull
(not only rows this pull moved: else a row pulled by a failed attempt 1 is never counted again and K=3 is unreachable). Turn error is used only for exemption and terminal classification.

### 1.5 Life of one Drain leg
L launch decision (`wakeSession`/`CLIScope` via `drainDecision`; reads only) → S submit (`Run` → mailbox; queued ⇒ accounted later in the owner's loop) → R pre-loop refusal in `runOwned` (lock busy, ANY lock error, `persistCallModels`): `noteDrainRefused`, Drain returns `DrainNotAttemptedError` →
P `att.hintAt = hintSeq`; pull (`runTurn` preamble) → N snapshot after the pull (read error ⇒ `notAttempted`) → C commit `decideDrainTurn(snap)` (skipped for the compaction continuation, `drainTurnCommitted`) → A `att.outcome = attempted` just before `agent.Stream` →
T stream (reaction via `persistStepFinish`, now BEFORE the cap/peak checks, or via the awaiting-answer finish) → X `closeDrainAttempt` before each of the two in-`runTurn` `drainOrReleaseMerged` calls, otherwise in `runOwned` right after `runTurn` returns → Z release (`go afterRelease(s)` from the hook).
In `runOwned` after every `runTurn`: `afterTurn(call, att, err)` closes `att` (no-op if closed); on `AwaitingAnswerError` for any call kind → `suspendAutoResume(s)`; pushes the supervision deadline unless a Drain never reached the provider; for a non-Drain turn ending in a non-exempt provider error paces the gate with `hintOpens=false` and no counting (no immediate paid Drain after a failed user turn).

### 1.6 Call sites
- Web/children: facts (`notifyAsyncCompletion`, `notifyBackgroundJobDone`, supervision, `wake_only`) → `wakeSession(ctx, s, fact=true)`; release: `onSessionIdleHook` → `go afterRelease(s)` → `noteSubAgentChildRunEnded(s)` + debt check → `wakeSession(ctx, s, false)`; tick: `RecheckPass` → `launchRecheckWake` → `wakeSession(tickCtx, s, false)`. `wakeSession` (~40 lines): bump if fact; external driver ⇒ return; `drainDecision`; `allow` ⇒ `drainCallFor` (error ⇒ `noteDrainRefused`) and `agentFor(s).Run`; `paced`/`deferred+recheck` ⇒ recheck set; `stuck` ⇒ log once. Nothing runs after `Run` returns.
- CLI root: loop → `CLIScope` → `ExecuteRun(drainTurn)` → `runInternal` → `currentAgent.Run` → same `runOwned`/`runTurn` accounting. The loop never accounts.
- CLI children + recheck set: the same 60s ticker as web; `ClaimExternalDriver` calls `StartRecheckTicker()`; `CancelAll` stops it (one scheduler implementation in both processes).
- `runInternal` for `IsDrain`: one attempt (no transient-retry/continuation loop, no 401 retry leg — credential refresh only as a side effect); pre-agent refusals (`checkPeakHours`, provider not configured, `ErrAgentShuttingDown`) ⇒ `noteDrainRefused` + `DrainNotAttemptedError`; `rebuildCall` loses the `IsDrain`/`drainTurnCommitted` lines; retry loop loses the `drainTurnCommitted` line.
- CLI loop by `CLIScope` state: `Owed` → pre-check `--max-cost`/`--max-tokens` (same comparison as `enforceRunawayCaps`) and `IsCancelRequested`, then run a Drain; `Paced` → `WaitForHint(until=min(RetryAt, now+5s))`, re-evaluate; `Stuck`+`!WorkOpen` → exit `exit_reason:"error"` + one stderr line naming the stuck debt; `Deferred`/`None`+`!WorkOpen` → exit (a pending question exits with `awaiting_answer` from the last real turn); `WorkOpen` → wait (heartbeat). Refusals (`agent.IsDrainNotAttempted`) keep the 30s budget (`cliLockBusyRetryOverallLimit`, generalized to all refusal kinds), then exit with that refusal's error.

### 1.7 Pacing and bounds
After an unreacted paid attempt: no launch before `retryAt = +60s`; a new fact hint does not open the gate early → K=3 needs ≥120s of failures (1-minute outage never closes the debt; ≥2 min closes with marker; terminal classes settle on the first attempt). CLI root retries at exactly `retryAt`; web/children at the first tick after (60–120s).
Free no-turn Drains with pending debt: 60s pacing; after 3 consecutive the gate is dormant (only a newer fact hint opens it); CLI exits `Stuck`. Refusals: CLI root every 0.5s within a 30s budget; web/children at the next tick; never counted/settled. Permanently failing reaction write: 3 counted attempts then settle; if the settle write fails too: dormant, zero further paid attempts until a human message or restart. bg-shell cap: exactly 5 submissions per human message; the counter is bumped once, synchronously, at `autoResumeEligible`, never re-checked by the policy.

### 1.8 Crash and shutdown
`CancelAll`: ticker stops first; in-flight Drains see `ctx.Canceled` (exempt: nothing written; "graceful exit = crash"). Forced shutdown: same; a late accounting write against a closing DB is logged and has no effect. Crash mid-attempt: that attempt is uncounted (≤1 per process life); a restarted process has an open gate and its first attempt is counted. Crash between increment and settle: next unreacted attempt increments past K and settles; `SettleReactedFailedWithMarker` writes the marker only if rows were settled (no double marker). Rerun cannot race a late settle (accounting happens while the Drain owns the mailbox; Rerun needs `ReserveExclusive`).

### 1.9 Tricky cases
fantasy swallowing `OnStepFinish` errors → rows stay unreacted → counted, paced, settled at K=3 "reaction not recorded"; `stepIsReaction` false on empty stream → same, paced; stall retries: fantasy never retries a stall (`isAbortError`), its in-step 5xx/429/network retries are inside one `Stream` call = one attempt, coordinator stall retries no longer apply to Drains; coordinator 401 rebuild not applied to Drains (settles terminal first attempt); queued user turn behind a Drain that reacted: accounting is per Drain leg on that leg's own error; a Drain queued before a failure commits under the now-closed gate → no-turn (pull only).

## 2. Finding → resolution
- R2B-1(A): `onStepFinish` order becomes record → usage → `persistErr := persistStepFinish()` → `enforceRunawayCaps` → `recheckPeakHours` → `return persistErr` (the step that crossed the cap is a recorded reaction); `enforceRunawayCaps` sets `att.capAbort`; the CLI pre-launch budget check ends the loop.
- R2B-1(B): `handleStreamFailure`'s awaiting branch persists its finish via `MarkReactedWithMessageUpdate` (fallback `messages.Update`): the question is a reaction; `afterTurn` suspends automatic turns until a human message (web) or ends the CLI loop with `awaiting_answer` once no work is running. Child: `childScopeDrained` treats deferred debt as drained so the delegation releases; `refreshSubAgentCompletion` uses the awaiting finish title/details when the text is empty (parent gets the question); `runSubAgent` lifts the child's suspension (`resetConsecutiveResume(child)`) when a delegation starts (also fixes the latent Stop defect).
- R2B-1(C)/(D): counted from the DB verdict; `stalled` not exempt; paced; K=3.
- R2B-2/R2C-1: CLI runs the same ticker (`recheckChild` for parked children + recheck-set draining); hint captured before the pull (a completion hint is no longer absorbed); first attempt counted.
- R2B-4/R2B-5: every pre-turn refusal → `noteDrainRefused` (`wakeSession` build/peak errors, `runInternal` peak/config/shutdown, `runOwned` lock busy/any lock error/`persistCallModels`, `runTurn` pre-commit read errors). "Policy" in this category = unreadable policy input (fail closed); a policy "no" is `Deferred`.
- R2B-7: delete the shared-lock probe from `wakeSession`. R2B-8: release-time debt-check error → recheck set; a failing pull = no-turn Drain with `pendingLeft` (paced + recheck set). R2B-9: snapshot after the pull. R2B-10: policy read errors fail closed for every session (delete the `persistentMode` split; fix the false "cap still bounds" comment; the retention half is store2's R2A-10).
- R2B-15: a Drain's compaction continuation keeps `Prompt ""`; when `drainTurnCommitted` is set `runTurn`'s non-persisted nudge reads "The conversation was summarized; continue reacting to the notice(s) above."
- R2B-16: single bump stays in `notifyBackgroundJobDone`; delete the capped policy branch, `capAlreadyCountedCtxKey`, `autoTurnCapAppliesCtxKey` and `wakeSession`'s bump; cap = 5.
- R2B-17: `recheckChild` returns immediately when `byChild[id]` has no armed job and no driver is registered; `onSessionIdleHook` spawns one goroutine; the `runInternal` defer benefits from the same early return.
- R2B-18: no coordinator retries for Drains + gate `retryAt` +60s. R2B-19: dormant after 3 no-turn Drains; CLI exits `Stuck`; Drain `ExecuteRun`s write nothing to the session (R2C-7).
- R2C-2: first-turn refusal path sets `final = result`, flushes, returns (on-finish hook via the existing defer). R2C-3: a `loopTotals` accumulator applied on every exit path; a cancelled/refused Drain never replaces `final.FinalText` but its usage is added. R2C-5: own-loop session skips the delegation-child refusal.
- R2C-6: `reviewerPassScopeStillOpen` uses `CLIScope`: skip on `WorkOpen`/`Owed`/`Paced`/`Stuck`, run on `Deferred`; every skip gets one stderr line.
- R2C-7: new unexported `RunRequest.drainTurn`: skips `UpdateSystemPrompt`, `UpdateReasoningEffort`, `ClearCancelRequest`, `SetBudget`, `SetEndedReason("")`, auto-title; the `ended_reason` defer is written only when the Drain reached the provider.
- R2C-4: the rerun handler takes `HoldAutomaticTurns(s)` before `CancelTurn` and releases immediately before the `RunWithReservedOwnership` handoff or on any bailout; held = `Deferred` with a recheck-set entry.
- R2C-12: `StopRerunJobs` runs in a detached goroutine after the truncation commit (its retry loop stays bounded by `closedCh`). R2C-11: orthogonal, store side (store2): `ClaimSessionDriver` returns `ErrDriverMarkerUnavailable` only for lock-capability failures; DB errors propagate.

## 3. Rejected alternatives
Accounting in launchers with better evidence (release hook fires inside `Run` before the launcher returns; queued/merged Drains have no launcher); pass-level accounting keeping coordinator retries for CLI Drains (needs an accounting point after release; pacing differs from web); a durable pacing column `next_wake_at` (a clock in the DB for a per-process concern; K=3 is already the durable bound); count only rows this pull moved (K unreachable); CLI calling `RecheckPass` from its 5s wait (a second clock); refresh-aware 401 classification inside accounting; exit the CLI immediately on a question (kills running jobs/delegations).

## 4. Production files
New `internal/agent/drain_attempt.go`. Changed: `coordinator_wake.go` (delete probe, snapshot, evidence, `recordDrainOutcome`/`checkStuckDrainProgress` calls, cap bump, `wakeSessionAttemptSeam`, `jobIdentity`), `coordinator_drain_policy.go` (`drainPolicy`/`drainPermitted`/`drainDecision`; delete `counted`, capped branch, `persistentMode` fail-open, `isPseudoJobID`, `recordDrainOutcome`, `settleOrRetryDrainFailure`, `incrementThenSettleIfThreshold`, `checkStuckDrainProgress`, `errDrainProgressNotRecorded`; `settleAndMark`→`settleDrainDebt`), `work_ledger.go`+`work_ledger_reaction.go` (gate; delete `noTurnDrainRelease`, `noTurnDrainHintSeq`, `admissionRefusedRelease`, their mark/consume methods, `visibleReactionDebtExists`), `agent_run.go` (per-iteration `att`, `afterTurn`, `noteDrainRefused` on every pre-loop error; delete admission marker, B6 block), `agent_turn.go` (`hintAt` before pull, snapshot after, `decideDrainTurn(snap)`, attempted flag before `Stream`, close before both `drainOrReleaseMerged` calls, R2B-15; delete `onDrainTurnStarting` call and no-turn marker), `agent_drain_decision.go`, `agent_drain.go`, `agent.go` (delete `onDrainTurnStarting`), `agent_notice_pull.go` (drop `anyWake`), `agent_turn_step.go` (persist before caps; `capAbort`), `agent_turn_failure.go` (awaiting-answer reaction write; `att.stalled`), `agent_turn_stream.go` (`att` in `turnStreamConfig`), `supervision.go` (hook = `go afterRelease`; debt-check error → recheck set; delete marker consumption and the external-driver hint bump), `coordinator_recheck.go`, `coordinator_reaction_source.go` (new `CLIScopeState`; ticker start in `ClaimExternalDriver`; `WaitForHint(until)`; delete `ScopeOpen`, `CaptureDrainSnapshot`, `RecordDrainTurnOutcome`), `coordinator_background.go`, `work_ledger_timeout.go`, `agent_prompt.go` (new `wakeSession` signature; delete cap ctx keys), `coordinator_turn_admission.go` (delete `markReachedProvider`/`didReachProvider`), `coordinator_run.go` (Drain = one attempt; refusal wrap), `work_ledger_delegation.go` (`recheckChild` early return; child scope open iff `WorkOpen`/`Owed`/`Paced`), `coordinator_work_scope.go` (question text), `coordinator_subagents.go` (lift child suspension), `coordinator.go` (`turnHolds`, `HoldAutomaticTurns`; human reset also resets the gate), `internal/app/app_run_async.go` (state-driven loop, `loopTotals`, refusal budget, pre-checks; delete `waitAfterNoTurnDrain`, snapshot/record calls), `app_run.go`+`app_run_request.go` (`drainTurn`), `app_run_gates.go` (`CLIScope` gate + stderr), `internal/server/handlers_agent_rerun.go` (hold; async `StopRerunJobs`). All files stay <1000 lines; net production code shrinks.
Composition with store2 (their new API shape): `CaptureDebtSnapshot`, `IncrementWakeAttempts`, `MaxWakeAttempts`, `SettleReactedFailedWithMarker` keyed by `(tool_call_id, claim_id)` + `delivery='done'` + `notice_message_id`, `DebtSnapshot.Empty()`, and a way to list the snapshot's tool_call_ids for the marker text; snapshot treated as opaque. Unchanged store APIs used: `ReactionDebtExists`, `PendingInclusiveDebtSummary`, `MarkReactedWithMessageUpdate`, `HasRunningDelegationFor`, `ListAsyncJobsForOwner`, `ListReactedFailedText`, `ForeignLiveDriver`, `ClaimSessionDriver`. ledger2: no API; only the head of `recheckChild` overlaps (their R2B-12 makes my guard return earlier — no conflict).

## 5. Mandatory tests (real SQLite, real `sessionAgent`; agent-level: `OnSessionIdle: coord.onSessionIdleHook` + `httptest` provider like `TestFailedDrain_OwnReleaseDoesNotImmediatelyRelaunch`; app-level: `newAdmissionRaceApp` + `RunNonInteractiveWithResult`)
Faults via SQLite triggers, not code seams: `BEFORE UPDATE OF reacted ON async_jobs WHEN NEW.reacted=1 AND NEW.reacted_failed=0 BEGIN SELECT RAISE(ABORT,'x'); END` blocks real reactions only; the same without the `reacted_failed` clause also blocks settles; a `delivery` trigger (pending→done) blocks pulls. Shrink `drainRetryAfterFailure` and `recheckPassIntervalNS` per test.
Agent: A1 `TestDrainAttempt_FreshPendingNoticeFirstAttemptCounted` (pending row + empty-stream provider ⇒ `wake_attempts=1`, 1 request in 2s, recheck set; revert: snapshot before pull ⇒ 0); A2 `…_EmptyStreamPacedThenSettledAtK` (no relaunch on release; 3 forced passes ⇒ exactly 3 requests, 1 marker, `reacted_failed=1`; revert: drop gate read in `afterRelease`); A3 `…_StallIsCountedNotExempt` (revert: exempt on `Canceled` regardless of `stalled`); A4 `…_AskQuestionReactsAndSuspends` (row `reacted=1`; later fact hint makes no request until `ResetAutoResumeCounter`; revert: drop awaiting reaction write / suspension); A5 `…_SwallowedReactionWrite_SettlesAtK` (reaction-only trigger; revert: remove increment ⇒ unbounded); A6 `…_ReactionAndSettleFail_DormantAfterThree` (both triggers: 3 requests over 6 passes; revert: remove dormant ⇒ 6); A7 `…_PermanentPullFailure_BoundedNoTurns` (0 requests, 3 launches over 6 passes); A8 `…_401SettlesFirstAttempt`; A9 `…_ShutdownAndCancelTurnAreExempt` (no counters/marker); A10 `…_QueuedUserTurnFailureNotCharged`; A11 `TestAdmissionRefusal_NonBusyLockError_NoHotLoop` (lock dir unwritable: ≤1 `Run` in 2s, recheck set, no counters); A12 `TestReleaseHook_NonChildSession_DoesNotBlockOnDB` (foreign `BEGIN IMMEDIATE` held: hook returns <50ms); A13 `TestBGShellCap_ExactlyFiveAutoResumes` (rewrites `TestSessionDrainPolicy_BgShellWake_CapsAtFive`); A14 `TestDrainPolicy_ReadErrorFailsClosed`; A15 `TestDrain_CompactionContinuation_NoEmptyPrompt`; A16 `TestChildDrainQuestion_ReachesParent`; A17 `TestRerun_HoldsAutomaticTurnsUntilHandoff` (`internal/server`: no Drain request between `CancelTurn` and handoff).
App: B1 `TestRunNonInteractive_TokenCapDrainEndsLoop` (usage in the SSE finish chunk: 2 requests, error exit, row reacted); B2 `…_DrainAsksQuestion_AwaitingAnswer` (2 requests, `awaiting_answer`); B3 `…_FailedDrainPacedThenSettled` (handler timestamp gaps ≥ shrunk interval; 3 Drain attempts; error exit); B4 `…_PermanentPullFailure_ExitsStuck` (exits before deadline, stderr stuck line, ≤3 no-turn iterations, budget/cancel columns untouched); B5 `…_ChildFailedDrainRetriedByTick` (child's 1st Drain 503, 2nd OK ⇒ run completes with the child's reaction; revert: no ticker in CLI ⇒ deadline); B6 `…_PeakRefusalNeverSettles` (`reacted=0`, no marker, exit after the shrunk budget); B7 `…_BusyFirstTurnJSONEnvelopeAndHook`; B8 `…_CtrlCDuringDrain_KeepsAnswerSumsUsage`; B9 `…_FormerDelegationChildReacts`; B10 `…_DrainTurnsMutationFree` (`sessions cancel` between turns ⇒ exit `canceled`, no request); B11 `…_ReviewerRunsWithDeferredDebt`.
Delete/rewrite: `coordinator_stuck_drain_progress_test.go`, `coordinator_settle_cancellation_test.go`, the `settleOrRetryDrainFailure` units in `coordinator_settle_by_failure_test.go` (replaced by A1–A10), marker parts of `coordinator_wake_lock_refusal_test.go`, `drain_compaction_submit_test.go`, `coordinator_wake_reaction_debt_test.go`, `coordinator_drain_cap_test.go`, fakes in `app_run_async_wait_test.go` and `app_run_reviewer_scope_retry_test.go`, and the four vacuous tests `TestPermanentPullFailure_EveryNoTurnIterationPaces`, `TestWaitForNextCLITurn_PendingDebt_ReturnsImmediately`, `TestFlushLoopExit_*`, `TestReviewerPassScopeStillOpen_*`. Every test needs a real revert-check.

## 6. Risks answered
DB writes on the turn's critical path: only Drain legs, normally one no-op UPDATE + one read, no DB I/O under `l.mu`, lock order unchanged. The gate replaces three markers, two snapshots, attempt evidence, the probe and the double cap count (3 writers, 1 reader predicate, 4 fields). No coordinator retries for CLI Drains is deliberate (web never had them; fantasy's in-step retries still apply). 401 without a retry leg settles first attempt (refresh still runs). Exempting `Canceled` cannot loop (nothing cancels Drains automatically; Stop suspends and zeroes wake; rerun holds; shutdown ends the process). `childScopeDrained` becomes policy-aware (running rows on live hosts + owed/retrying debt hold the delegation; only deferred (question/Stop) or stuck debt releases it; stuck rows stay pending and visible in `sessions why`; re-anchor ASYNC-02). Suspension is in memory (same as Stop; a web restart lifts it — document). The ticker runs in `rush run` (same pass as web; stopped by `CancelAll`; a tick against the CLI root is a no-op). Snapshot rows pulled mid-turn are counted by the next attempt (K still holds). Two web processes on one data dir: pacing per process, K=3 durable and shared. Docs needed after: DUR-4 (new `reacted=1` writer: the awaiting finish), ASYNC-09 (stalls and non-provider post-reach errors counted; a question is a reaction), ASYNC-02, amendments (a)(b)(c)(l) superseded, new amendment (m).

## 7. Commit order (each built/vetted/gofmt-checked on touched packages and verified by its own tests)
1. `fix(agent): persist a step's reaction before cap/peak checks; ask_question is a reaction` (tests A4 part, B1 unit part).
2. `feat(agent): Drain attempt accounting in the owning turn loop` (adds `drain_attempt.go`, gate writes, `runTurn`/`runOwned` wiring; removes `wakeSession`'s accounting; CLI record/capture become no-op stubs; gate readers not wired yet; tests A1, A3, A5, A8, A9, A10).
3. `refactor(agent): launches read the gate; delete release markers, probe, pseudo ids` (incl. R2B-16 cap, R2B-10 fail-closed, R2C-5 policy half, question suspension, supervision push move; tests A2, A4, A6, A7, A11, A13, A14).
4. `fix(agent): a Drain is one provider attempt in runInternal; refusals typed` (R2B-15; test A15).
5. `feat(agent): CLIScope drain states, commit-time gate, CLI ticker, child scope decision` (R2B-17, question text for the parent, child suspension lift; tests A12, A16).
6. `fix(app): rush run loop follows the coordinator's drain decision` (accumulator, R2C-2/3/6/7, remove stubs; tests B1–B11).
7. `fix(server): rerun holds automatic turns; StopRerunJobs off the handler` (test A17).
8. docs re-anchor (DUR-4, ASYNC-02, ASYNC-09; amendment (m)) — may be handed to the docs agent.
