# Async series, round 9 — 2026-10-05

Delta reviewed: `ea3a979a..4874fb73` (86 commits, ~10.6k lines of non-test Go in
`internal/{agent,app,session,cmd,shell,server,filelock,log,db}`). Three read-only
`ox` reviewers on disjoint areas: **A** store/session/filelock/log/db, **B**
`internal/agent`, **C** `internal/app` + `internal/cmd` + `internal/server`.
Tests were not run by the reviewers; every P1 below was re-verified against the
code by the orchestrator before it was fixed.

Stop rule (CLAUDE.md): P0/P1 with a reproducible scenario are fixed in this
cycle; P2/P3 go to the backlog below and are NOT fixed here.

## Verdicts

| Area | Verdict | P0 | P1 | P2 | P3 |
|---|---|---|---|---|---|
| A store/session/filelock/log/db | GO | 0 | 0 | 2 | 11 |
| B agent | NO-GO | 0 | 1 | 4 | 6 |
| C app/cmd/server | NO-GO | 0 | 3 | 4 | 14 |

## P1 — fixed in this cycle

| Id | Defect | Fix (commit) | Test |
|---|---|---|---|
| B9-1 (= A9-1) | `ClaimShell`: `Claim` commits the bg_shell row, `MarkAnnounced` fails → shell killed, no observer, row stays `running`/`announced=0` on a live host; `rush run` waits until `--timeout`/6 h | `ClaimShell` deletes the row it just inserted (claim-scoped, announced=0) — `f35ad18f` (#1197) | `TestClaimShell_AnnounceFailureLeavesNoRow` (+ keeps-the-announced-row pin) |
| C9-1 | `ExecuteRun` nil-dereferences `primaryResult` when the review turn fails in terse/stream mode without `captureResult` (SDK default) → host process crash | nil-safe `recordReviewFailure` — `8615c12a` (#1198) | `TestReviewerPass_FailureInTerseModeKeepsPrimaryAnswerWithoutPanic` |
| C9-2 | unfinished-todos reminder (A18) fires on top of an unanswered `ask_question`; run ends `end_turn`/exit 0, the question never reaches the caller | `todoNudgeDue` stands down on `AwaitingAnswerError` — `8f96f6fb` (#1199) | `TestRunNonInteractive_OpenTodosDoNotBuryAnAskedQuestion` |
| C9-3 | loop schedule created by a turn after the first scope close survives `rush run` (once-per-run cancel flag, ASYNC-02) | flag removed, every close cancels — `381fd93a` (#1200) | `TestRunNonInteractive_LoopCreatedByTheReminderTurnIsCancelledAtTheFinalClose` |

Verification of the four: mutants S1–S3, R1, N1, L1 red (S2 — a redundant Go-level
guard — was removed instead of kept, the scope is the SQL `announced=0`
predicate, pinned by a test); full `internal/session`, `internal/app`,
`internal/agent` green; `go vet`, `golangci-lint` 0 issues, linux/darwin cross-build.

## Backlog (P2/P3, not fixed in this cycle)

### P2
- **A9-2** `session/async_job_bgshell.go:20-22,33` — a bg_shell row is keyed by a per-manager counter id (`%03X`, `shell/background.go:425`), but `rush.db` is shared: process B's `ClaimShell(S,"001")` can adopt process A's running row as `Existing`; one exit then closes the shared row, the other shell has none (or A's host sweep marks B's live shell `interrupted`). Needs two processes driving one session with overlapping bg shells. Fix: globally unique key, or refuse `Existing` with a foreign `host_id`.
- **B9-2** `agent/bgshell_claim.go:70-74` + `coordinator_background.go:141-146` — for an SDK root the observer commits the terminal row (`reacted=1`) before the `bg_shell_done` notice is inserted (behind the coordinator-wide `bgArrival`, up to 30 s on a busy DB); a CLI loop iteration in that window sees no work and no debt and exits, so the shell's result is not handled in that run. The "closed by the hold" argument in `docs/plans/2026-10-05-bg-shell-remainder.md` is wrong for the root (the hold only feeds `childScopeDrained`); the plan's rejection of the single transaction (R-BG-2) stands for the reason given there, but this window is real.
- **B9-4** `agent/codexprovider/provider.go:423-428` — a mid-stream SSE read error (`io.ErrUnexpectedEOF` on a cut chunked body) is returned raw → `classTerminal`; fantasy/openai wraps the same error as retryable. Same symptom #1191 fixed (worker dies without retry).
- **B9-5** `agent/async_tool.go:85` + `delegation_question.go:168` — a web parent's (or nested parent's) reaction to `child_question` runs as a sync Drain turn, so `answerHeldDelegation` (`!sync`) does not trigger; the resume runs as a sync delegation in parallel with the held one: the parent turn blocks in `awaitSync`, the result arrives twice. The #1157 tests cover `OriginCLI` only.
- **C9-4** `app/app_run_async_nudge.go:86-92` — if both reminder turns fail (provider error, `SessionLockBusyError`), a successful executor turn becomes `exit_reason=error`/non-zero exit; the reviewer pass has `reviewFailureKeepsPrimary`, the nudge has no equivalent.
- **C9-5** `app/app_run_async.go:170` vs 470/479 — on the loop path `applyTo` overwrites `final.Warnings`; the reviewer warnings (`FAIL`, `unverified`, `reviewer pass failed`) vanish from the envelope (`review_verdict` survives).
- **C9-6** (borderline P1) `app/app_run_reviewer_evidence.go:590,616-619,474` — on the CLI path an async command's exit code is never recovered: the completion notice is `Async job <callID> (bash) finished.` without `exit N`, and the `Background job` regexp captures the shellID (`001`) which does not match the ack's callID. Every `go test`/`go build` > 3 s reaches the evidence as `exit=unknown (still running?)` and can push the reviewer to a false FAIL. Fixtures use a hand-written format that the CLI path never produces.
- **C9-7** `cmd/providers_models.go:59-77`, `providers.go:575-579` + `config/load_providers.go:63-77` — `rush providers update openai-codex` writes the catalog's raw `context_window` (272000 for gpt-6-*) into the global `rush.json`; a positive value counts as user-set, so the model_facts floor (1.05M) no longer applies — #1180 is undone at runtime, `models list` and web. (#1183 fixed zero dumps only.)

### P3
- A9-3 `session_update.go:48-90` negative cost delta not guarded; A9-4 `session_workspace.go:29-33` `Owns("", "", false)` true (unreachable today); A9-5 `session_activity.go:281-283` `WaitAnswer` without `LiveDelegations>0`; A9-6 `session_child_question.go:83` reads through the writer `q`; A9-7 `db/sql/sessions.sql:130` dead `IncrementSessionCostIfUnderMax`; A9-8 `session_cost_subtree.go:25` base > spent after deleting a child; A9-9 `migrations/20261005000001_bg_shell_jobs.sql` + `WithAllowOutofOrder` (column loss on out-of-order rebuild); A9-10 `db/diskfree_windows.go:30` `TotalNumberOfFreeBytes` instead of the caller's; A9-11 `log/workspace.go:20` `ws` = cwd outside git vs `sessions.workspace_root=''`; A9-12 `wake_schedules.sql:106` `NextDueWakeScheduleAt` ignores leases; A9-13 `run_queue.sql:132` rows of a vanished workspace are invisible forever (by design; only purge cleans).
- B9-3 `bgshell_claim.go:82-104` `finishBGShellRow` retries `sql.ErrNoRows` forever on `context.Background()` (goroutine leak when the session is deleted; a test pins it); B9-6 `work_ledger_delegation.go:236` detached shell's hold keeps `childScopeDrained=false` for up to 10 min after `job_kill`; B9-7 `coordinator_background.go:131` vs `work_ledger_bgshell_kill.go:59` natural exit racing `job_kill` can deliver both "stopped" and "finished"; B9-8 `bgshell_claim.go:49` shell id counter restarts after a crash → Existing adopted from a dead host; B9-9 `bgshell_claim.go:70-74` observer does not call `noteSubAgentChildRunEnded` (delegation freed by the 60 s pass); B9-10 `fs_write.go:255` N items on one path shrink a file to (1/4)^N, below 2048 B the next item can empty it; B9-11 `agent_run.go:542-551` WS-1 refusal → recheck set (foreign session retried every 60 s).
- C9-8 `app_run_request.go:277` nudge is not mutation-free, `ClearCancelRequest` erases a `sessions cancel`; C9-9 `app_run.go:404-410` `SubtreeSpent` read failure → whole history reported as this run's cost; C9-10 `sessions_audit.go:73-75` "no -wal/-shm created" untested on a real WAL DB; C9-11 `app_run_async_nudge.go:21` prompt says "mark cancelled", the todos tool has no such status; C9-12 `sessions_cost_rows.go:62` `g.Priced` from the last session of a `--by day` group; C9-13 `sessions_list.go:110` COST column is OwnCost, `--json cost_usd` is SubtreeBudget; C9-14 `app_run_reviewer_outcome.go:86-96,125-132` one failed/unknown tool call suffices for a "verified pass"; C9-15 `app_run_reviewer_evidence.go:397,167-168` pre-existing untracked files always in `changed_during_run`, git stderr merged into parsed output; C9-16 `app_run_async.go:682-685` Drain after the loop's review replaces `l.final` (review/verdict lost, `final_text` = reviewer model's answer); C9-17 `app_run_setup.go:131-137` A15 reader counts Own only, the slash-command doc promises delegations too; C9-18 `login_link.go:46` Ctrl-C copies the link on Unix; C9-19 `app_run_reviewer_evidence.go:37,42,283` total limit in bytes, section limits in runes; C9-20 `sessions_compact.go:116` goes through `setupApp` (starts the coder agent, pump, wake scheduler) instead of `setupAppLite`; C9-21 `app_run.go:643-645` SDK terse/stream prints the reviewer's text on a successful review (R5-3 pins the old behaviour, A10 says executor).

### Adjacent (outside the reviewer's area, recorded)
- **config, P2** `config/datadir.go:96-98` + `load.go:78` — `home` is `source != Shared`, so an explicit `--data-dir <main>/.rush` (or `options.data_directory`) from a linked worktree makes the process "home" and lets its pump/wake/`--continue` pick up the main checkout's legacy (`workspace_root=''`) sessions. The orchestrator's reading: by design an explicit data dir is "mine"; reachable only with an explicit override plus pre-WS-1 rows; kept as P2, not P1.
- **config, P2** `config/load_worktree.go:45-81` — `WorkspaceRoot` is empty when `git` is not on PATH or the probe fails, cached for the process lifetime; the design says canonical cwd.
- **agent** `coordinator_run.go:718` — a reviewer slot without its own effort inherits the executor session's `SmartModelReasoningEffort`.
- **web** `web/src/types.ts:19` — `Session.Cost` is stale (the Go side sends `OwnCost`); no consumers found.

## Series status

Four P1 in a delta that grew ~10k lines since round 8 (new subsystems: durable
bg-shell rows, WS-1, reviewer evidence, the CLI loop machine) — a new-code round,
justified by the stop rule. The four fixes are narrow (≤ 30 lines each) with a test
and a mutant apiece; no further round is scheduled for them. A next round is
justified only by a new P0/P1 in new code or an explicit operator request, not by
the P2/P3 list above.

## Backlog resolution and delta review (2026-10-06)

The operator asked for the whole backlog to be resolved, not parked. Every P2/P3
item was re-checked against the then-current code by read-only consultants (three
`@ox` agents, one per area) and either fixed through a worktree-isolated rush agent
(each diff re-verified by the orchestrator: mutants, full packages, lint) or closed
with a reason.

### Fixed

| Item | Task | Commit |
|---|---|---|
| A9-2 / B9-8 shell id collision across processes | #1210 | `92e267ac` |
| B9-2 observer/notice window in `CLIScope` | #1211 | `5ea19d80` |
| B9-5 held-question answer by origin (was P1) | #1212 | `0ff6c988` |
| config home flag by directory; `WorkspaceRoot` without git | #1213, #1214 | `e9ad503e`, `279fa77e` |
| C9-4 / C9-9 / C9-11 reminder failure, cost window, prompt text | #1206, #1209 | `e4b40a2e` |
| C9-5 reviewer warnings kept by `applyTo` | #1205 | `b7fc6e8f` |
| B9-3 `finishBGShellRow` retry loop | #1207 | `34114959` |
| B9-10 `fs_write` compounding shrink | #1208 | `c6ab8a27` |
| A9-12 wake timer vs leases; A9-7 dead query; A9-10 Windows free space | #1216, #1218 | `ad6c0fba` |
| A9-5 / A9-6 `PendingChildQuestions` of a finished delegation | #1217 | `fffc19c1` |
| B9-6 / B9-7 `job_kill` hold and race with the natural exit | #1219 | `9f4a0a0d` |
| B9-9 observer recheck without a notifier | #1220 | `d323d67b` |
| adjacent: reviewer effort inherited across models | #1221 | `6727f0cb` |
| C9-15b / C9-19 evidence stderr and rune cap | #1222 | `0869fdb0` |
| C9-12 / C9-13 / C9-20 sessions cost, list, compact | #1223 | `2c2c1ad6` |
| C9-18 / C9-17 login Ctrl-C, slash-command text | #1224 | `99fc8a84` |
| C9-8 / C9-16 / C9-21 reminder mutation-free, Drain after review, terse reviewer text | #1225 | `5a5f326b`, `c38c1664` |

### Closed with a reason

| Item | Reason |
|---|---|
| A9-3 negative cost delta | unreachable: the only caller passes non-negative prices |
| A9-4 `Owns("", "", false)` | unreachable: a non-home process always has a non-empty root |
| A9-8 subtree base above spent | intentional clamp, documented in `session_cost_subtree.go` |
| A9-9 out-of-order migration column loss | unreachable today; note for future table rebuilds |
| A9-11 `ws` vs `workspace_root=''` | display only; never compared |
| A9-13 vanished-workspace queue rows | by design (WS-1); purge/cascade clears them |
| B9-11 foreign-workspace refusal retried | not reached through any shipped turn-start path (all filter by the same rule); the guard is a documented backstop. Reading-based, not proven |
| C9-10 WAL sidecar claim untested | cosmetic |
| C9-14 one failed tool call = verified pass | by design (reviewer-pass design, test T10; the verdict never changes the exit code) |
| C9-15a untracked files in `changed_during_run` | by design: no numstat for `??`, so a changed existing untracked file is otherwise invisible |
| `web/src/types.ts` `Session.Cost` | no consumers |
| #1226 lost question with a live delegation | not a defect (two independent reads): after `ask_question` the agent suspends automatic turns, so a late completion is Deferred, never Owed; pinned by `TestRunLoop_QuestionWithOpenDelegationExitsAwaitingAnswer` (`529d7c4c`) |

### Delta review of the fix series (reviewer role + `@ox`, three areas each)

Six read-only reviews of `4874fb73..fffc19c1`. Two P1 regressions came out of the
fixes themselves, both confirmed by a second reviewer and fixed in-cycle:

- **#1227** (`65ebf2a0`) -- since #1206 a failed todo-reminder turn was dropped even
  when the failure was the operator's `sessions cancel` / a crossed cap, so the run
  reported success. The stop signals are re-checked before dropping. The in-turn
  cancellation variant could not be driven through the harness; the test pins the
  invariant with a reminder that fails for its own reason while the flag is pending.
- **#1230** (`8f096349`) -- #1221 stopped `--model B --effort high` from sending the
  effort (it reached only the session row). The explicit effort now rides the
  override of its slot; tests assert on the prepared overrides.

P2s from the same reviews, fixed: #1228 (evidence parsing of real async bash notices:
call id key and `Exit code N` tail, `17da6107`), #1229 (terse-mode verdict on stderr,
reviewer-turn stop recheck; `fda7a88e`, which also moved the loop exit path out of
`app_run_async.go` when it hit the 1000-line limit), #1232 (test/comment hygiene, `c613d557`), #1233 (a provider
retry no longer duplicates the cut attempt's text, `8499e9ae`), #1231 (registry
re-anchored, `a432e6dd`).

Accepted, with reasons:

- `bgshell_claim.go` reads the live `notify_on_background_job_done` while the notifier is
  chosen when the turn's tools are built: a hot switch of the option while a shell runs
  can release a hold early. Narrow window, no data loss.
- `job_kill` holds `bgArrival` for up to its 10 s budget while retrying the transition;
  bounded by design. The gate's own exclusion window has no deterministic test (the
  `IsDone` deferral and the under-gate cancelled check are each pinned by mutants).
- Any callback hold counts as open work: a killed process tree that never exits keeps
  `rush run` open up to `completionHoldMax` (10 min).
- The `WorkspaceRoot` `.git`-walk fallback returns the path as written, not
  symlink-resolved like git's answer; resolving it could change drive-letter case or 8.3
  names on Windows and orphan stored rows. Reachable only when git gives no answer.
- Reviewer effort on CLI/codex providers still comes through the context key (outside
  #1221's API-provider scope); `--effort` without `--model` on a fresh session is ignored
  (pre-existing).
- `ask_question` checks only the session's own jobs, so a question over a live
  delegation or a `wakein` schedule is delivered when that work ends (latency, not loss;
  ASYNC-02).

### Process notes

The sweep of the 56 packages on `fffc19c1` before these follow-ups: 45 ok, 11 without
tests, 0 failures. Orchestrating flash-class workers needed zero-trust review every
time: two agent tests were vacuous (they asserted the session row, not what the turn
sent; one passed with and without the fix), one agent's final report was garbled; the
reviewer-role and `@ox` passes disagreed on severity only where reachability was
unproven, and the ones that were reproduced held. The 5-hour provider budget and the
shared heavy-command queue (queue wait counts against `--timeout`) are the real
throughput limits of a parallel agent wave.
