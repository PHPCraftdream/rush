# Async Invariant Registry

Phase 0 (`docs/plans/2026-09-27-async-structured-concurrency.md`, "Фаза 0 —
законы и сетка") of the async structured-concurrency rewrite. Extended by
phase 4 (`docs/plans/2026-09-28-async-phase4-durable-core.md`, step 8):
DUR-1…DUR-9, the durable core's own laws.

Each row records one law, worded exactly as in the owning design doc, so it
survives a total rewrite of the mechanism that later phases replace. Format:
`ID | закон | где сегодня (файл:функция) | доказывающие тесты | статус`.
Status is one of «выполняется», «частично» (с уточнением, какая часть), or
«нарушается — закрывается фазой N» with the concrete known defect. Every
claim below was checked against the code at this HEAD (phase 4 step 8,
`d14f930d` + step 8's own commits): every file:function anchor was grepped
and read, every test name was grepped and (for anything not obviously
matching its row) opened to confirm it actually exercises the law, not just
mentions it in a comment.

**History:** anchored 2026-09-27 (phase 0); re-anchored across phases
1–3 (async-phase1/2/3, wakes-stage2, supervision — see git history for the
per-phase detail this file used to carry inline). Phase 4
(`d14f930d`..step 8, this revision) replaced the async job's entire
mechanism: `asyncJobRegistry`/`subAgentOutcomeRegistry`'s in-memory-only
state, the CLI loop's poll/BFS, and `internal/session/descendant_liveness.go`
are all gone, superseded by `internal/session/async_job_store.go`'s durable
`async_jobs`/`session_notices` tables, `internal/agent/work_ledger_*.go`'s
DB-backed `transition`, and `internal/session/async_job_reader.go`'s
DB-based cross-process readers. Every ASYNC-0x row below is re-anchored to
this mechanism; DUR-1…DUR-9 are new rows for the durable core's own laws
(design doc sec.4). Re-anchor before relying on line numbers; they drift.
Each later phase MUST re-anchor every row its changes touch, in the same
commit that makes the change — a registry that lies about where its law
lives is worse than no registry (`docs/mcp-invariants.md`'s own rule, which
this registry copies).

## Invariants: ASYNC-01…ASYNC-10 (phase 0, re-anchored phase 4)

| ID | Закон | Где сегодня (файл:функция) | Доказывающие тесты | Статус |
|---|---|---|---|---|
| ASYNC-01 | У каждой задачи ровно один владелец; у дочерней сессии в каждый момент не больше одной активной делегации. | Owner-per-task: `AsyncJobStore.Claim`/`claimOnce` (`internal/session/async_job_store.go:232-336`) — one durable row per `(owner, tool_call_id)`, idempotent on a repeated call with the same input hash. "Not more than one active delegation per child session" is now DB-enforced, not just mailbox exclusivity: a partial unique index on `child_session_id WHERE state='running'` (migration `20260928000002_add_async_phase4_core.sql`) makes a second concurrent delegation to the same child a `claimOnce` conflict; `Claim` recovers the conflicting row's host if provably dead and retries once, else returns `ErrAsyncChildSessionBusy` **before** "started" (design doc sec.3.8, the `resume_session_id` behavior change — CHANGELOG). | `TestAsyncJobStore_ClaimIsIdempotent`, `TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal`, `TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted`, `TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted`, `TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused` (all `internal/session/async_job_store_test.go`). | Выполняется — both halves of the law are now DB-enforced and tested directly (closes the old row's "не async-специфичный механизм" gap for the child-delegation half). |
| ASYNC-02 | Область открыта, пока у неё есть незавершённая или недоставленная задача (кроме `loop` и надзора), идущий ход, ожидающее событие или открытая дочерняя область. CLI-хост завершается только при закрытой области корня, по `--timeout` или по отмене оператора. | The scope predicate is now a single DB read, not memory: `coordinator.ScopeOpen` (`internal/agent/coordinator_reaction_source.go:113-140`) — a running row on a live host, OR mid-turn, OR a reaction-debt row whose policy allows a turn; it also recovers this owner's own dead-host rows before answering (DUR-6). The CLI loop's exit condition (`internal/app/app_run_async.go:226-260 waitForNextCLITurn`) and the reviewer-pass gate (`internal/app/app_run.go:547-550`) both call it directly — the old in-memory `workLedger.next()`/BFS/safety-net ticker are gone. A child's delegation row stays `running` until its own scope closes, so parent-open-while-child-open is structural (one DB row per level), not a bounded in-memory walk. | `TestScopeOpen_LiveHostRunningRowKeepsScopeOpen`, `TestScopeOpen_RecoversDeadHostRowBeforeAnswering` (`internal/agent/coordinator_scope_recovery_test.go`); root-waits-for-child (unedited by phase 4, still passing against the new predicate): `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`, `TestRunNonInteractiveWaitsForAsyncSubAgentResult`, `TestRunNonInteractiveContinuesPastFiveAsyncCompletions`. | Выполняется for depth 1 (proven end-to-end) and structurally for depth > 1 (each level's own row, proven individually); a live black-box grandchild scenario through `rush run` remains blocked by the unrelated coordinator hang described below (not re-verified this phase: `internal/agent/coordinator_tools.go`/`agent_tool.go` untouched by phase 4). `loop`/надзор scoping unchanged from phase 3. |
| ASYNC-03 | Задача достигает терминального состояния ровно один раз; гонка finish/stop/timeout/cancel/interrupted даёт один исход. | The CAS moved from memory to the DB, but the shape is the same: `workLedger.transition`/`commitTransition` (`internal/agent/work_ledger_transition.go:198-339`) is the only caller of `AsyncJobStore.Transition` (`internal/session/async_job_store.go:345-407`), itself the ONE terminal-state SQL CAS (`UPDATE ... WHERE state='running'`, DUR-1/DUR-2). Every terminal-transition call site (finish, `handleTimeout`, job_kill, `cancelSession`, `recheckChild`) funnels through `transition`; the loser re-reads and adopts the committed row instead of retrying its own cause. | `TestWorkLedger_WebCallbackExactlyOnce`, `TestWorkLedger_ConcurrentFinishAndAcknowledge`, `TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome`, `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome`, `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome` (in-process races); `TestAsyncJobStore_TransitionWonThenLostThenGone`, `TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing` (DB CAS itself); `TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow` (cross-process race via recovery). | Выполняется. |
| ASYNC-04 | Каждая задача, о старте которой сообщено модели, доставляется владельцу ровно один раз — в том числе отменённая, просроченная и прерванная смертью хоста. | Ack-gate is now the durable DUR-7 fusion: `workLedger`'s ack path (`internal/agent/work_ledger_announce.go`, `work_ledger.go:383`) + `AsyncJobStore.AnnounceStarted` (`internal/session/notice_pull.go:296`) commit "started" and `announced=1` in ONE transaction; `onToolResult` (`internal/agent/agent_turn_stream.go:389-395`) is still the call site. Delivery to the owner is the driver's pull (DUR-3, `internal/agent/agent_notice_pull.go`) — every terminal cause, including host death (recovery marks `interrupted`, DUR-6), produces a `delivery='pending'` row the SAME pull mechanism picks up; "delivered exactly once across a host death" is now durable, not a gap. Sync (SDK) jobs are unchanged: `deliverLocked`'s sync branch, never DB-backed. | `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction`, `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`, `TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck` (`internal/agent/work_ledger_ack_gate_test.go`); host-death delivery: `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted` (`internal/app`). | Выполняется — phase 4 closes the host-death gap the earlier phases explicitly left open ("смерть хоста — фаза 4"); delivery depth remains 1 hop per delegation link, transitively structural as ASYNC-02 describes. |
| ASYNC-05 | Уведомление никогда не опережает сохранённый tool result, объявивший задачу. | DB-enforced directly: every pull query requires `announced=1` (`ListPendingAsyncJobNoticesForOwner`, `internal/db/sql/async_jobs.sql`, DUR-7); an unannounced row from a dead host is deleted without a trace by recovery rather than ever surfacing (DUR-6), closing the class of bug the old `finishParked` re-insert path used to risk. | `TestPullJobNotices_InsertsMessageWithNoticeInvariant` (announced row pulled correctly); `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`; `TestRecoverDeadHost_UnannouncedDeleted`; `TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck` (`internal/app`, new this step: two-App, host dies before ack, row deleted, no notice ever appears). | Выполняется. |
| ASYNC-06 | Ход начинает только драйвер сессии; остальные компоненты публикуют события. | Unchanged in shape from phase 2, now feeding off DB-driven hints instead of in-memory completions: `coordinator.wakeSession` (`internal/agent/coordinator_wake.go:47`) is still the sole turn initiator for a completion/notice hint; `coordinator.agentFor` (`internal/agent/coordinator_subagent_drivers.go:161`) is still the single choke point for which `SessionAgent` drives a given session id. Phase 4 adds the Drain call (`agent_drain_decision.go:26 decideDrainTurn`) as a normal `SessionAgentCall` submitted through `Run`/`submit`, never a bespoke turn constructor. | `TestWakeSession_RunPanicIsRecovered`, `TestWakeSession_RunErrorIsVisibleNotDebug` (`internal/agent/coordinator_autoresume_test.go`); `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent`, `TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent` (`internal/agent/coordinator_agentfor_test.go`); `TestSubAgentWorkTerminal_DriverBusyIsNotTerminal` (`internal/agent/coordinator_subagent_drivers_test.go`). | Выполняется. |
| ASYNC-07 | Маршрут доставки и пробуждения не зависит от origin и глубины сессии. | `deliverLocked`'s CLI/web branch and its sync-job short-circuit are unchanged from phase 2; phase 4 adds that the terminal cause itself (natural finish, timeout, job_kill, cancel, dead-host recovery) is computed once by `causeStateNoticeKindWake` (`internal/agent/work_ledger_transition.go:93-112`) and committed by the SAME `Transition` CAS regardless of origin — a delegation, a CLI bash job, and a dead-host recovery all produce the same `delivery='pending'` row shape the ONE pull mechanism (DUR-3) picks up, so "route independent of origin" now extends to "independent of whether the process that started the job is even still alive". | `TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone`, `TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout` (mode independence, phase 2, unedited); `TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage` (route independent of which process's driver pulls it). | Частично — origin/mode independence and host-death independence are now proven; transitive depth > 1 delivery remains untested end-to-end (same caveat as ASYNC-02/04). |
| ASYNC-08 | Отмена области отменяет её задачи и дочерние области; прерывание хода (interrupt) задачи не отменяет. | Cancellation is now transitive across the WHOLE live delegation tree via the DB, not just the direct owner/child maps: `coordinator.Cancel` (`internal/agent/coordinator_interrupt.go:41-85`) calls `workLedger.cancelTree` (`internal/agent/work_ledger_delegation.go:383`) to walk every still-`running` delegation row from `sessionID` down, cancels each session's own driver via `agentFor`, and durably zeroes `wake` on every pending/done row across the whole tree (`AsyncJobStore.SetWakeZeroForOwners`, `internal/session/async_job_reaction.go:280-300`, DUR-9) — closing the race where a job finishes with `wake=1` a moment before Stop. `context.WithoutCancel`/`context.WithCancel`'s async/sync split (interrupt does not cancel a job) is unchanged from phase 2. | `TestStop_OneSecondAfterNaturalFinish_NoNewTurn`, `TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere` (`internal/agent/coordinator_stop_tree_test.go`); `TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows` (`internal/session/async_job_reaction_test.go`); `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent` (phase 2, unedited); `TestAsyncTool_SyncCallerCtxCancelUnblocksAwait` (interrupt-does-not-cancel, sync side). | Выполняется — the whole-tree half is now proven at N>1 children (`TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere`), closing phase 2's "async-detach not tested directly" gap for the cancellation-transitivity half; the interrupt-does-not-cancel half's async/CLI-detach direction remains proven by code reading, not a black-box test (same reasoning as phase 0/1: the two available triggers are both too timing-fragile to be worth a flaky test over a property already guaranteed by `context.WithoutCancel` at construction). |
| ASYNC-09 | Неудачная доставка или пробуждение становятся видимым состоянием области, а не записью в Debug-логе. | Fully closed this phase: every wake-failure path -- `wakeSession`'s own `Run` error (phase 2) AND the durable settle-by-failure path (`coordinator.recordDrainOutcome`, `internal/agent/coordinator_drain_policy.go:74-183`) -- persists a visible `wake_failed` session notice (`session.NoticeKindWakeFailed`) via `InsertSessionNotice`, not a Debug log line, and sets `reacted=1` on the settled rows so the debt closes with a visible marker instead of silently vanishing. `refreshSubAgentCompletion`'s own DB-read Debug (unrelated: content freshness of an already-delivered result, not a wake failure) is untouched, as phase 2 already scoped it out. | `TestWakeSession_RunErrorIsVisibleNotDebug` (phase 2, unedited); `TestSettleByFailure_QuotaMarkerSettlesImmediately`, `TestSettleByFailure_OneTransientFailureDoesNotSettle`, `TestSettleByFailure_KThreeTemporaryFailuresSettleOnce`, `TestSettleByFailure_AdmissionRefusalDuringShutdown_DoesNotSettle` (`internal/agent/coordinator_settle_by_failure_test.go`); `TestSettleByFailure_ScopedIncrementAndSettle_GoLayer` (`internal/session/async_job_reaction_test.go`). | Выполняется — phase 2's "частично" is resolved: the settle-by-failure path this phase adds is the other dominant source of a silent wake failure (K=3 exhausted retries, quota/401), and it is now visible by the same mechanism. |
| ASYNC-10 | Любой ответ «почему сессия ждёт» выводится из данных, доступных любому процессу, и называет конкретную задачу. | The lock-file/heartbeat heuristic (`internal/session/descendant_liveness.go`) is DELETED (step 7). The cross-process AND in-process paths are now the SAME mechanism: `AsyncJobStore.LiveJobs`/`LiveDescendantJobs` (`internal/session/async_job_reader.go:94-200`) walks `child_session_id` transitively over the DB, checking each row's host liveness via the shared probe (DUR-5) -- readable by ANY process, and naming the actual `tool_call_id`/`kind`, not just "a descendant session is alive". `rush sessions jobs` (`internal/cmd/sessions_jobs.go`), `sessions why`'s Async jobs/debt section (`internal/cmd/sessions_why.go`), and `sessions list`/`sessions show` are all rewired onto this reader (step 7). | `TestLiveJobs_OwnRunningRowIsLive`, `TestLiveJobs_ForeignAliveHostIsLive`, `TestLiveJobs_DeadHostRowIsNotLive`, `TestLiveJobs_UnknownHostRowStaysVisible`, `TestLiveJobs_TerminalRowIsNeverLive`, `TestLiveJobs_DelegationTreeTransitivity` (depth 2: root->child->grandchild, `internal/session/async_job_reader_test.go`); `TestSessionsJobsCmdRun_TableAndForeignLiveHostHint`, `TestSessionsJobsCmdRun_JSON` (`internal/cmd/sessions_jobs_test.go`); `TestExplainSessionStatus_AsyncJobsAndDebtSection`, `TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted` (`internal/cmd/sessions_why_descendant_test.go`). | Выполняется -- first full closure of this law across all 4 phases: names a specific task (not just "session is alive"), readable by any process, and proven transitively past depth 1 (`TestLiveJobs_DelegationTreeTransitivity`), which the old lock-file mechanism never was. |

## Invariants: DUR-1…DUR-9 (phase 4, `docs/plans/2026-09-28-async-phase4-durable-core.md` sec.4)

| ID | Закон | Где сегодня (файл:функция) | Доказывающие тесты | Статус |
|---|---|---|---|---|
| DUR-1 | Состояние async-задачи меняет только `transition`: одна транзакция с SQL-CAS; память сходится к БД; держатель исхода повторяет до коммита. | `workLedger.transition`/`commitTransition` (`internal/agent/work_ledger_transition.go:198-339`) is the only caller of `AsyncJobStore.Transition` (`internal/session/async_job_store.go:345-407`, the one `UPDATE ... WHERE state='running'` CAS). `commitTransition` retries with doubling backoff (`asyncStoreRetryBackoff`, 10ms→1s cap) until the store commits, sees the row terminal/gone, or the shutdown latch fires (`job.shutdownCancelled` + `l.closed`); the winner writes `job.transitionToTerminal` from the COMMITTED row, the loser adopts the same committed row -- memory never decides independently of the DB read. | `TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits`, `TestWorkLedger_StartFailsClosedWhenStoreUnavailable` (shutdown-latch sibling test in the same file) (`internal/agent/work_ledger_durable_test.go`); `TestAsyncJobStore_TransitionWonThenLostThenGone` (loser adopts winner's state, `internal/session/async_job_store_test.go`); `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome`, `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome` (own-cause vs natural-finish races, each keeping its own distinguishable cause on a win). | Выполняется. |
| DUR-2 | Терминальный переход, `delivery='pending'` и `wake` по причине — одна транзакция. | `TransitionAsyncJobTerminalPreserveVoid` (`internal/db/sql/async_jobs.sql`, generated into `internal/db/async_jobs.sql.go:992-1060`) sets `state`/`notice_kind`/`result_summary`/`delivery`/`wake`/`reacted` in ONE `UPDATE` statement, scoped to `state='running'`; `causeStateNoticeKindWake` (`internal/agent/work_ledger_transition.go:93-112`) computes the wake/delivery/reacted triple per cause BEFORE calling `Transition`, so the commit is a single atomic write of all three, never a sequence of separate writes. | `TestAsyncJobStore_TransitionWonThenLostThenGone` (won direction: state+delivery+wake set together); `TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing` (new this step -- fault-injection direction: a transaction running the SAME CAS but rolled back instead of committed leaves state/delivery/wake/reacted ALL unchanged, proving the three are bound to one transaction, not independently written; revert-checked by temporarily committing instead of rolling back and observing the assertions fail). | Выполняется. |
| DUR-3 | Уведомления пишет в историю только ведущий сессии, только в начале хода или на границе шага; уведомление и `delivery='done'` — одна транзакция. | `sessionAgent.pullPendingNotices`/`pullPendingNoticesForStep` (`internal/agent/agent_notice_pull.go:24-67`), called from `runTurn` before history loads and from `PrepareStep` at each step boundary (never from a compaction step). The actual pull is `AsyncJobStore.PullJobNotices`/`PullSessionNotices`/`pullOneJobNotice` (`internal/session/notice_pull.go`): one `UPDATE ... WHERE delivery='pending' ... RETURNING` per row, in the SAME transaction as the history `INSERT` and the `notice_message_id` write-back. | `TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage` (two independent `*sql.DB` connections racing the SAME row -- exactly one message, DUR-3's two-process leader race); `TestDrain_NoticeBetweenTwoToolCalls_OnlyAtStepBoundaryAndCarriesForward` (a notice never lands between a tool call and its own result); `TestCompaction_DoesNotPullNotices`; `TestPullJobNotices_CreateTxErrorLeavesRowPending` (a pull error never fails the turn, row stays pending). | Выполняется. |
| DUR-4 | Долг реакции — строка с `wake=1`, `delivery` ∈ {pending, done} и `reacted=0`; `reacted=1` ставится только транзакцией шага модели с реальным содержимым (или закрытием долга неудачей). «Нужен ход» и «область открыта» решаются по долгу и по БД. Каждое освобождение мейлбокса проверяет долг и при необходимости подаёт вызов-забор обычным путём `Run`. Подсказка лишь будит. | Debt predicate: `AsyncJobStore.ReactionDebtExists`/`VisibleReactionDebtExists` (`internal/session`), wrapped by `workLedger.reactionDebtExists`/`visibleReactionDebtExists` (`internal/agent/work_ledger_reaction.go:137-155`). Reaction marker: `AsyncJobStore.MarkReactedWithMessageUpdate` (`internal/session/async_job_reaction.go:76`), called from ONLY `turnStream.persistStepFinish` (`internal/agent/agent_turn_step.go:491-507`) in the SAME transaction as the step's own final message write; settle-by-failure (`coordinator.recordDrainOutcome`, `internal/agent/coordinator_drain_policy.go:74-183`) is the other writer, after K=3 (`drainFailureSettleThreshold`) exhausted retries or an unrecoverable classification. Every mailbox release checks debt: `coordinator.onSessionIdleHook`/`recheckDebtOnRelease` (`internal/agent/supervision.go:451-490`), wired onto `sessionAgent.onSessionIdle`, fired from `abandonOwnershipWithHandoff` (`internal/agent/agent_ownership.go:297-310`) -- the single choke point every `Run`/`RunWithReservedOwnership`/`ReleaseExclusive` passes through. The Drain call itself is an ordinary `SessionAgentCall` through `Run`/`submit` (`agent_drain_decision.go:26 decideDrainTurn`), never a bespoke turn path. | `TestMarkReactedWithMessageUpdate_OneTransactionClearsDebt`, `TestMarkReactedWithMessageUpdate_SessionNoticesHalf` (`internal/session/async_job_reaction_test.go`); `TestStepFinishWriteFailure_DebtStaysThenOneMoreDrainClearsIt`, `TestCompaction_DoesNotClearUnreactedDebt`, `TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts`, `TestInterruptDuringDrain_OperatorMessageReachesProvider` (`internal/agent/drain_debt_edge_cases_test.go`); `TestSettleByFailure_QuotaMarkerSettlesImmediately`, `TestSettleByFailure_OneTransientFailureDoesNotSettle`, `TestSettleByFailure_KThreeTemporaryFailuresSettleOnce`, `TestSettleByFailure_AdmissionRefusalDuringShutdown_DoesNotSettle` (`internal/agent/coordinator_settle_by_failure_test.go`); `TestDrain_WakeNoticeCallsProviderWithNoticeAndNoTextPrompt`, `TestDrain_NothingPendingSkipsProviderAndFinishesTurn` (`internal/agent/drain_notice_pull_test.go`). | Выполняется. |
| DUR-5 | Процесс жив ⇔ держит ОС-блокировку своего файла хоста; «мёртв» — только при захвате или отсутствии файла. Часы не используются. | `ProbeHostLock`/`ProbeHost` (exclusive, dead-host recovery) and `ProbeHostLockShared`/`ProbeHostShared` (non-acquiring, reader-safe) (`internal/session/host_lock.go:139-250`); `RegisterHost` (`:251`) lazily registers on first `Claim`, never eagerly. No timestamp comparison anywhere in the liveness decision. | `TestProbeHostLock_DeadViaENOENT`, `TestProbeHostLock_DeadViaWonLock`, `TestProbeHostLock_Alive`, `TestProbeHostLock_Unknown`, `TestProbeHostLockShared_AliveDoesNotDisturbHolder`, `TestProbeHostLockShared_DeadReleasedLockIsReusable`, `TestProbeHostLockShared_DeadViaENOENT`, `TestProbeHostLockShared_Unknown`, `TestProbeHost_RefusesOwnHostID`, `TestProbeHostShared_RefusesOwnHostID` (`internal/session/host_lock_test.go`); `TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile` (`internal/session/async_job_recovery_test.go`). | Выполняется. |
| DUR-6 | Восстановление только переводит `running` мёртвого хоста в `interrupted`/удаляет неанонсированное; историю не пишет и сессии не будит. | `AsyncJobStore.RecoverDeadHost` (`internal/session/async_job_recovery.go:61-200`) and `SweepDeadHosts`/`RecoverOwnerScope` (same file), called from `ensureHost`'s first-registration sweep, the host's own 60s pass, and every turn's own preamble/`ScopeOpen` check (`internal/agent/agent_turn.go:362-369`, `coordinator_reaction_source.go:117-123`). `announced=1` rows transition to `interrupted`/`wake=0` through the SAME `Transition` CAS as any other terminal cause (DUR-1/DUR-2); `announced=0` rows are deleted outright (`DeleteUnannouncedAsyncJob`). Neither path calls `messages.Create`/`wakeSession` itself. | `TestRecoverDeadHost_CreatesNoMessages` (direct proof: zero messages created by recovery); `TestRecoverDeadHost_AnnouncedDelegationInterruptedWithChildText`, `TestRecoverDeadHost_AnnouncedBashInterruptedFixedText`, `TestRecoverDeadHost_UnannouncedDeleted`, `TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow`, `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (`internal/session/async_job_recovery_test.go`); end-to-end: `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted`, `TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck` (`internal/app`). | Выполняется. |
| DUR-7 | «started» и `announced=1` — одна транзакция; неанонсированная задача никогда не даёт уведомления. | The ack gate's fused write: `internal/agent/work_ledger_announce.go` (workLedger side) + `AsyncJobStore.AnnounceStarted` (`internal/session/notice_pull.go:296-...`) commit the "started" tool-result message and `announced=1` in ONE transaction, no state condition; `onToolResult` (`internal/agent/agent_turn_stream.go:389-395`) is the call site. Every pull query filters `announced=1` (`internal/db/sql/async_jobs.sql`). | `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction`, `TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled`, `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`, `TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck` (`internal/agent/work_ledger_ack_gate_test.go`). | Выполняется. |
| DUR-8 | БД недоступна при старте async-задачи — задача не запускается. | `workLedger.Start` (`internal/agent/work_ledger.go:216-263`): a non-sync job's `Start` calls `store.Claim` FIRST -- a Claim error (including "no store wired") returns the error and the executor callback is never invoked (fail-closed). A sync job never touches the store at all (design doc sec.3.1), so it still works with none wired. | `TestWorkLedger_StartFailsClosedWhenStoreUnavailable` (nil store -- the literal "no store wired" fail-closed case; a broken real `*sql.DB` is covered indirectly, every store-level error path in `internal/session/async_job_store_test.go` already returns a plain error `Start` propagates verbatim, not a silent retry); `TestWorkLedger_StartSyncJobWorksWithoutStore` (the sync counter-case). | Выполняется — the literal "no store" case is proven directly; a genuinely-unavailable-but-configured DB (e.g. a closed `*sql.DB` mid-process) is proven only by the store layer's own error propagation, not a dedicated `Start`-level test against a closed connection, since `Start`'s own logic does not distinguish "no store" from "store errors" (both hit the same `err != nil` branch) -- not re-tested separately as it would exercise the identical code path. |
| DUR-9 | Stop отменяет дерево делегаций; отменённая и прерванная работа не будит никого. | `coordinator.Cancel` (`internal/agent/coordinator_interrupt.go:41-85`) walks the live tree via `workLedger.cancelTree` (`internal/agent/work_ledger_delegation.go:383`), cancels each session's own driver (`agentFor`), and durably zeroes `wake` on every pending/done row across the WHOLE stopped tree via `AsyncJobStore.SetWakeZeroForOwners` (`internal/session/async_job_reaction.go:280-300`) -- closing the race where a job finishes `wake=1` a moment before Stop. Recovery (DUR-6) never wakes; a Stop-then-restart adds no extra facts. | `TestStop_OneSecondAfterNaturalFinish_NoNewTurn` (race-won natural completion, wake zeroed, a late hint produces no turn); `TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere` (N=2 sub-agents, each with its own running bash job -- Cancel(root) zeroes every descendant's wake, no session in the tree gets a turn); `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (N=3 sub-agents on a dead host -- exactly N facts on the first restart sweep, zero on a second); `TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows` (`internal/session/async_job_reaction_test.go`). | Выполняется. |

## Находка фазы 0 (не исправлено фазой 4): вложенная делегация (`agent`-инструмент внутри
`agent`-инструмента) вешает координатор

Обнаружено при попытке написать сценарий (a) (grandchild): предоставление
`config.AgentTask.AllowedTools` инструмента `agent` (т.е. разрешение
делегированному под-агенту самому делегировать дальше) вешает ВЕСЬ
координатор — не только вложенную делегацию — на неопределённый срок,
подтверждено эмпирически (`go test ... -timeout 5m`, лог: ни один запрос ни
разу не дошёл до мок-провайдера, даже самый первый ход корня, до истечения
60-секундного `ctx`).

Механизм (прочитан в коде, не догадка): `buildTools` (`internal/agent/
coordinator_tools.go:611-618`) при виде `AgentToolName` в `agent.AllowedTools`
синхронно вызывает `c.agentTool(ctx)` (`internal/agent/agent_tool.go:34-77`),
который БЕЗУСЛОВНО (независимо от глубины/происхождения вызова) строит
задачного под-агента через `c.buildAgent(ctx, prompt, config.AgentTask,
true)`. `buildAgent` (`coordinator_tools.go:27-165`) регистрирует построение
ИНСТРУМЕНТОВ этого под-агента как ОТДЕЛЬНУЮ горутину на разделяемом барьере
`c.readyWg` (`coordinator.go:266-273`, `c.readyWg.Go(func() { buildTools(...) })`,
строка ~155-162) — и ЭТА внутренняя `buildTools` СНОВА видит `AgentToolName`
в (том же) `config.AgentTask.AllowedTools`, снова вызывает `c.agentTool(ctx)`,
снова строит `buildAgent`, снова регистрирует ещё одну горутину на
`c.readyWg` — без каких-либо условий останова. Поскольку КАЖДАЯ точка входа
хода (`coordinator_run.go:658`, `coordinator_models.go:617`,
`coordinator_interrupt.go:394/629`, `credentials.go:310`,
`coordinator_run_queue_call.go:258` — везде `c.readyWg.Wait()`) блокируется
на этом же барьере до завершения ВСЕХ зарегистрированных горутин, а горутины
регистрируются быстрее, чем могут завершиться (каждая порождает ещё две),
`readyWg.Wait()` не возвращается никогда — так что не проходит ни один ход
вообще, включая самый первый ход КОРНЯ, чей запрос даже не требует
делегации.

Это НЕ входит в список дефектов design-doc (#1029…#1040) — новая находка
этой фазы, вне её объёма для исправления (фаза 0 меняет только тесты и
документацию). По серьёзности (зависание всего координатора от одной
конфигурационной настройки, даже без реального запуска вложенной
делегации) заслуживает отдельного P0/P1-разбора у оператора. Фаза 4 не
трогала этот код (см. выше) -- находка остаётся открытой.

Следствие для сценариев (a): нет сегодня БЕЗОПАСНОГО чёрного пути создать
сессию-«внука» через реальный `rush run`, чтобы проверить транзитивность
глубже одного уровня через живой координатор:

- вложенная делегация через `agent` — вешает координатор (выше);
- вложенная делегация через `agentic_fetch` технически НЕ подвержена этому
  дефекту (её под-агент получает фиксированный, не из
  `config.AgentTask.AllowedTools`, набор инструментов —
  `internal/agent/agentic_fetch_tool.go:171-178`), но требует реального
  HTTP-запроса, а её клиент собран как `tools.NewSSRFGuardedClient(30s,
  false)` (`agentic_fetch_tool.go:61`, `allowPrivate=false`) — локальный
  `httptest`-сервер (loopback) гарантированно отклоняется SSRF-охраной
  (`internal/agent/tools/ssrf_guard.go:112-113`), а реальный внешний URL
  сделал бы тест недетерминированным и сетезависимым.

Phase 4's `internal/session/async_job_reader.go` (`LiveJobs`/
`LiveDescendantJobs`) sidesteps this entirely for the READER side (ASYNC-10)
by testing transitivity directly against DB rows
(`TestLiveJobs_DelegationTreeTransitivity`, depth 2) instead of needing a live grandchild
session through the coordinator -- so ASYNC-10's depth>1 gap is now closed
even though the coordinator hang above is not. The coordinator's own
in-process scope/delivery transitivity (ASYNC-02/04/07) remains untested
past depth 1 for the same reason this section originally gave.

## Побочная находка: `IsSessionBusy` не видит делегированного под-агента

При отладке сценария (d) выяснилось, что `AgentCoordinator.IsSessionBusy
(childSessionID)` для делегированной дочерней сессии ВСЕГДА возвращает
`false`, независимо от реального состояния ребёнка: делегированный под-агент
исполняется на ОТДЕЛЬНОМ `*sessionAgent`, построенном заново `coordinator.
buildAgent` внутри `agentTool()`/`agenticFetchTool()`
(`coordinator_tools.go:27`, `agent_tool.go:44`), а не на `c.currentAgent`
координатора, единственном экземпляре, который `IsSessionBusy` опрашивает.

**Статус: закрыто задачей #1049 (влито до начала фазы 1).**
`internal/agent/coordinator_subagent_drivers.go`'s `subAgentDriverRegistry`
(`c.subAgentDrivers`) registers which `*sessionAgent` actually drives
`childSessionID`'s turns; `workLedger.childScopeDrained`
(`internal/agent/work_ledger_delegation.go:193-214`) consults that driver
(`driver.agent.IsSessionBusy(childID)`), falling back to `c.currentAgent`
only for a session with no registered driver (bare test fixtures). Proof:
`TestSubAgentWorkTerminal_DriverBusyIsNotTerminal`
(`internal/agent/coordinator_subagent_drivers_test.go`).

## Пропущенные/убранные сценарии

Phase 0's candidates a/b/c/d, unchanged by phase 4 (the grandchild-hang
blocker above is still the reason (a) has no live black-box test):

- **(a) Grandchild — убрано** for a live black-box scenario through the
  coordinator (grandchild-hang finding above); the READER side (ASYNC-10)
  now has direct DB-level depth-2 coverage instead
  (`TestLiveJobs_DelegationTreeTransitivity`).
- **(b) «Прерывание не отменяет задачи» (вторая половина ASYNC-08) —
  пропущено** as a black-box scenario for the same timing-fragility reason
  phase 0 gave; proven by construction (`context.WithoutCancel`) and by the
  sync-side unit test (`TestAsyncTool_SyncCallerCtxCancelUnblocksAwait`).
- **(c) «Корень держится, пока жив процесс, которым владеет ребёнок» —
  не дублируется**: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`
  (`internal/app/app_run_subagent_root_wait_test.go`) already proves it,
  now against ASYNC-02's DB-based `ScopeOpen` predicate rather than the old
  in-memory one.
- **(d) Отмена ctx + `CancelAll` — реализовано**:
  `internal/app/async_scenarios_cancel_test.go`
  (`TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn`).

Phase 4's own sec.6 two-App scenario list (`internal/app/
async_phase4_recovery_scenarios_test.go` and
`async_phase4_recovery_unannounced_test.go`) is covered in full:

- (а) host crashes mid-job, announced --
  `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted`.
- (б) delivered in A, B adds nothing --
  `TestTwoAppScenarioB_DeliveredInADoesNotResurfaceInB`.
- (в) Ctrl-C through the real `rush run` cancel flow (ctx cancellation of
  `RunNonInteractiveWithResult`, not just `Close()`), then restart -- one
  "interrupted" per task --
  `TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask`.
- (new, step 8) host killed BEFORE the ack gate commits (announced=0) --
  the row is deleted by recovery, no notice ever appears --
  `TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck`.
