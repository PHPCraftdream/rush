# Async Invariant Registry

Phase 0 (`docs/plans/2026-09-27-async-structured-concurrency.md`, "Фаза 0 —
законы и сетка") of the async structured-concurrency rewrite.

Each row records one law ASYNC-01…ASYNC-10, worded exactly as in the design
doc, so it survives a total rewrite of the mechanism (`asyncJobRegistry`,
`subAgentOutcomeRegistry`, `descendant_liveness.go`, …) that later phases
replace. Format: `ID | закон | где сегодня (файл:функция) | доказывающие
тесты | статус`. Status is one of «выполняется», «частично» (с уточнением,
какая часть), or «нарушается — закрывается фазой N» with the concrete known
defect (task numbers from the design doc). Every claim below was checked
against the code at this HEAD; where a claim could not be verified with
confidence, the cell says «не проверено» rather than guessing.

**Anchored 2026-09-27, worktree `async-phase0`; re-anchored 2026-09-28,
worktree `async-phase2` (phase 2, ASYNC-03/04/06/07/08/09 rows); re-anchored
2026-09-28, worktree `wakes-stage2` (task #1023, ASYNC-03 row only --
`job_kill`'s `stopRequested` flag and `run_command`'s new control path);
re-anchored 2026-09-28, worktree `async-phase3` (task #1047, closes #1019
BL-1: ASYNC-02/03/10 rows -- CLI poll/BFS and the safety-net ticker deleted,
replaced by `sessionAgent.onSessionIdle`'s by-construction release trigger;
`recheckChild` now also releases the delegation driver/allowlist entry via
`coordinator.releaseDriverIfScopeClosed`); re-anchored 2026-09-28, worktree
`supervision` (task #1043, ASYNC-02 row only -- root-session supervision
check-in implemented, `internal/agent/supervision.go`).**
Line numbers drift; re-anchor before relying on them. Each later migration phase
(1–5) MUST re-anchor every row its changes touch, in the same commit that
makes the change — a registry that lies about where its law lives is worse
than no registry (see `docs/mcp-invariants.md`'s own rule, which this
registry copies).

## Invariants

| ID | Закон | Где сегодня (файл:функция) | Доказывающие тесты | Статус |
|---|---|---|---|---|
| ASYNC-01 | У каждой задачи ровно один владелец; у дочерней сессии в каждый момент не больше одной активной делегации. | Владелец задачи: `workLedger.Start` (`internal/agent/work_ledger.go:110-130`) — одна запись `*asyncJob` на ключ `(owner, toolCallID)`. Фаза 1 меняет поведение на повторном ключе: вместо отказа `Start` теперь идемпотентно возвращает существующую запись (`existing=true, err=nil`), закрывая #1038 — вызывающий (`asyncTool.Run`) не должен повторно запускать исполнителя. «Не больше одной активной делегации» на дочернюю сессию по-прежнему обеспечивается не async-кодом, а обычной эксклюзивностью mailbox сессии (`internal/agent/agent_control.go`, admission gate одной генерации на сессию). | `TestWorkLedger_StartIsIdempotentPerToolCallID` (`internal/agent/work_ledger_test.go`) — прямая проверка идемпотентности на дублирующемся `(owner, toolCallID)`, закрывающая прежний пробел покрытия. Эксклюзивность mailbox — см. `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerPolicy` (`internal/app/app_run_admission_race_test.go`), не специфично для делегации. | Частично — владение одной записью на задачу теперь проверено тестом и идемпотентно по конструкции (#1038 закрыт); «не больше одной активной делегации на дочернюю сессию» — по-прежнему не async-специфичный механизм и не проверен для делегации конкретно. |
| ASYNC-02 | Область открыта, пока у неё есть незавершённая или недоставленная задача (кроме `loop` и надзора), идущий ход, ожидающее событие или открытая дочерняя область. CLI-хост завершается только при закрытой области корня, по `--timeout` или по отмене оператора. | Фаза 3 удалила опрос и BFS: `internal/app/app_run_async.go`'s `runNonInteractiveWithAsyncResults` теперь идёт напрямую к финализации, как только `NextAsyncCompletion` возвращает `hasCompletion == false` — никакого `descendantWorkPending`/`select`/`time.After` цикла больше нет. Единственный держатель корня — `workLedger.next()` (`internal/agent/work_ledger.go`), который блокируется на `sessionJobs.changed`, пока `len(s.jobs) > 0`; армированная-но-недоставленная делегация остаётся записью В ЭТОЙ ЖЕ карте (`work_ledger_delegation.go`'s `armDelegation`/`recheckChild`), так что `next()` уже эквивалентен транзитивной проверке на глубине 1 без отдельного механизма (доказательство — `docs/plans/2026-09-28-async-phase3-spec.md` §1.1-1.3). `coordinator.DescendantWorkPending`/`anyPendingWorkInMemory`/`sessionOwnsPendingWork` — **удалены** (`coordinator_work_scope.go`), ссылка снята. Освобождение сессии теперь ТАКЖЕ триггерит `recheckChild` по построению — `sessionAgent.onSessionIdle` (`internal/agent/agent.go`), вызываемый из `abandonOwnershipWithHandoff` (`agent_ownership.go`), единственной точки, через которую проходит каждый `Run()`/`RunWithReservedOwnership`/`ReleaseExclusive`, — а не только из разрозненных ручных вызовов (`runInternal`'s defer, `notifyAsyncCompletion`'s defer), которые остаются как идемпотентные, но более не единственные триггеры. Выход CLI-хоста по `--timeout`/Ctrl-C — `internal/cmd/run.go:619-719` (ctx-отмена, не явный `CancelAll`), не тронут этой фазой. | Одноуровневая делегация: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob` (`internal/app/app_run_subagent_root_wait_test.go`), `TestRunNonInteractiveWaitsForAsyncSubAgentResult`, `TestRunNonInteractiveContinuesPastFiveAsyncCompletions` (`internal/app/app_run_async_completion_test.go`); отмена корневого `ctx` посреди живой делегации — `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (`internal/app/async_scenarios_cancel_test.go`, все три не редактировались этой фазой, доказывают отсутствие регресса). Новое (эта фаза): `TestWorkLedger_NextUnblocksOnSignalNotPoll`, `TestWorkLedger_ChildScopeClosesWithoutTicker`, `TestRecheckChild_FiresOnRealDriverRunEndWithoutManualTrigger` (`internal/agent`) — событийность (не опрос) и by-construction триггер, доказанные до и после удаления тикера. Транзитивность на глубину > 1 (grandchild) НЕ проверена и не может быть безопасно проверена сегодня чёрным тестом — см. «Находка этой фазы» ниже: единственный публичный путь создать сессию-«внука» (вложенная делегация через инструмент `agent`) зависает; #1050 делает эту глубину недостижимой, а не непроверенной по недосмотру. | Выполняется; ранее отдельный опрос/BFS-механизм и предохранительный тикер удалены как доказанно избыточные (эта фаза), не заменены на другой обходной механизм — `workLedger.next()`+`sessionAgent.onSessionIdle` уже достаточны на проверенной (глубина 1) глубине. `loop` из этого закона сегодня не существует вовсе (появляется в фазе 5). Надзор (задача #1043, `internal/agent/supervision.go`) реализован и подтверждает закон конструктивно, а не по совпадению: `supervisionState` — НЕ запись `workLedger` (никогда не кладётся в `bySession[x].jobs`), так что `l.running`/`next()` вычисляются из карты задач ровно как раньше, не видя надзор вообще -- `rush run` завершается, когда закрывается остальная область, а таймер надзора просто перестаёт быть кем-либо вызван (процесс завершается вместе с его горутиной), без явной отмены. |
| ASYNC-03 | Задача достигает терминального состояния ровно один раз; гонка finish/stop/timeout/cancel/interrupted даёт один исход. | Единый CAS: `asyncJob.transitionToTerminal` (`internal/agent/work_job.go`) — единственный писатель `state` дальше `phaseRunning`, вызывается под одним `workLedger.mu` из `.finish`, `.cancelSession`, `.recheckChild`, и, с фазы 2, `workLedger.handleTimeout` (`internal/agent/work_ledger_timeout.go`) — ПЯТЫЙ конкурент за ту же CAS. `handleTimeout`'s `timeoutTerminateAndWake` branch: `job.cancel()` (best-effort), best-effort `capturePartial` (I/O — bash shell read or delegation DB read — done OUTSIDE `l.mu`, mirroring `recheckChild`'s own established snapshot-then-refresh-then-relock pattern rather than holding the ledger lock across it), then `transitionToTerminal(phaseTimedOut, partial)` + `deliverLocked`, both under a SECOND `l.mu` hold. `handleTimeout`'s own `job.state != phaseRunning` pre-check under `l.mu` is a second, cheaper line of the same guarantee (skips the I/O if already terminal), not a replacement for the CAS — `deliverLocked`'s "safe to call unconditionally, win or lose" contract (its `present` map-membership check) is what actually prevents double delivery when `handleTimeout` loses the race. `timeoutWakeOnly` never participates in this CAS (never touches `state`) — its own `timeoutNotified` one-shot guard under the same `l.mu` is a separate, independent guarantee. `deadline`/`timeoutKind` (unused in phase 1) now have production callers: `timeoutService.fireDue` → `handleTimeout`. **Wakes stage 2 (task #1023, `internal/agent/work_ledger.go`'s `finish`):** `MarkJobStopped`/`StopRunCommandJob` set a new `asyncJob.stopRequested` bool under `l.mu`, but do NOT add a sixth competitor for the CAS itself — they only flip a flag `finish` reads before calling the SAME `transitionToTerminal`, so `job_kill`-initiated stop still funnels through the existing `.finish` call site, one CAS writer as before. **Phase 3 (task #1047):** `cancelSession`'s own unconditional delivery (`work_ledger_delegation.go:174-227`, not edited this phase) is not a new CAS competitor either, but its role closing every hypothetical gap the now-deleted safety-net ticker might have covered is part of the evidence the ticker was never protecting a real race (`docs/plans/2026-09-28-async-phase3-spec.md` §1.4) — `abort`/`cancelSession`/`handleTimeout`'s `timeoutTerminateAndWake` were traced and each shown to already route through the SAME CAS/`deliverLocked` pair this row describes, so the ticker's removal added no new interleaving. Separately, `recheckChild`'s tail now calls `coordinator.releaseDriverIfScopeClosed` (`coordinator_subagent_drivers.go`), which applies the SAME compare-and-delete idiom this row's CAS is built on to a DIFFERENT pair of maps (`subAgentDriverRegistry.byChild`, `permission.go`'s `runAllowlistBySession`) keyed by a driver registration generation instead of `asyncJob.state` -- not a change to this row's own CAS, noted here because it is the same pattern applied one layer up. | `TestWorkLedger_WebCallbackExactlyOnce`, `TestWorkLedger_ConcurrentFinishAndAcknowledge`, `TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome` (all unedited). **New (phase 2):** `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome` (`internal/agent/work_ledger_timeout_test.go`) — adds `handleTimeout` as a fourth concurrent racer against `finish` for the same job, 30 iterations, exactly-one-delivery. **New (wakes stage 2, task #1023):** `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome` (`internal/agent/work_ledger_job_kill_test.go`) — races `MarkJobStopped` against `finish` for the same job, 30 iterations, exactly-one-delivery. | Выполняется — фаза 2 добавляет `handleTimeout` в CAS без ослабления гарантии (доказано новым тестом); wakes stage 2's `stopRequested` flag changes only what `finish` records, not the CAS's writer count (доказано новым тестом). |
| ASYNC-04 | Каждая задача, о старте которой сообщено модели, доставляется владельцу ровно один раз — в том числе отменённая, просроченная и прерванная смертью хоста. | Ack-gate: `onToolResult` (`internal/agent/agent_turn_stream.go:389-395`, не тронут фазой 2) вызывает `asyncJobs.acknowledged`/`.abort` только после того, как tool result персистентно записан; доставка — единая `workLedger.deliverLocked` (`internal/agent/work_ledger.go`), ровно один раз при `announced ∧ state.terminal() ∧` запись ещё присутствует в карте владельца. **Фаза 2 (§4.4):** ack-gate теперь ТАКЖЕ верен для `sync`-задач (SDK/unspecified origin, `asyncTool.Run`'s единый путь) — `workLedger.Start` устанавливает `announced = sync` сразу при регистрации, а не только через `onToolResult`, потому что sync-задача не имеет отдельного "started"-результата: финальный ответ И ЕСТЬ единственный tool result этого вызова. `deliverLocked`'s sync-ветка коротко замыкает доставку через `job.done`/`workLedger.awaitSync`, минуя ready-queue/`onWebDone` (см. ASYNC-07 ниже). Транзитивная доставка для делегаций не изменилась: `workLedger.armDelegation`/`.recheckChild`. | `TestWorkLedger_WaitsForPersistedToolResult`, `TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce`; **новый (фаза 2):** `TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone` (`internal/agent/work_ledger_test.go`) — доказывает `announced=sync` и sync-доставку через `job.done`. | Частично — не изменился относительно фазы 1 (доставка на 1 уровень, глубина > 1 не проверена, отменённая ПРОСТАЯ задача не доставляется, смерть хоста — фаза 4); ack-gate формулировка уточнена для sync-задач. |
| ASYNC-05 | Уведомление никогда не опережает сохранённый tool result, объявивший задачу. | `workLedger.deliverLocked` требует `job.announced == true` (установлено только `.acknowledged`, вызываемым `onToolResult` после успешной записи tool result) прежде чем что-либо доставить (`internal/agent/work_ledger.go:159-183`). Слияние регистров убрало прежнюю `finishParked`'s ветку пересоздания удалённой строки целиком — задача теперь ОДНА запись от `Start` до доставки, пересоздавать нечего (класс бага BL-2/#1032 в этой части закрыт как побочный эффект). | `TestWorkLedger_WaitsForPersistedToolResult`, `TestAsyncToolWebCompletionWaitsForToolResult` (`internal/agent/async_tool_test.go`, не редактирован по сценарию); `TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce` (новый — та же гарантия для армированной, но ещё не объявленной делегации, ранее непроверенный пробел). | Выполняется. |
| ASYNC-06 | Ход начинает только драйвер сессии; остальные компоненты публикуют события. | **Выполняется с фазы 2.** `coordinator.wakeSession` (`internal/agent/coordinator_wake.go`) — единственный инициатор хода для доставки: `notifyAsyncCompletion` и `notifyBackgroundJobDone` (`internal/agent/coordinator_background.go`) больше не строят `SessionAgentCall`/не выбирают драйвер сами — они собирают только текст+`noticeKind`, решают `wake bool` и передают всё `wakeSession`, единственное место, вызывающее `Run`. `coordinator.agentFor` (`internal/agent/coordinator_subagent_drivers.go`) — единая точка выбора `SessionAgent` (зарегистрированный драйвер делегированного ребёнка, иначе `c.currentAgent`); `wakeSession`, `Cancel`, `InjectMessage` (задача #1054, см. ASYNC-08) все вызывают именно её вместо повторной реализации того же отката. `runAutoResumeRecovered` удалена целиком — её паник-recover перенесён внутрь `wakeSession`'s собственного `defer recover()` вокруг вызова `Run`. | `TestWakeSession_RunPanicIsRecovered`, `TestWakeSession_RunErrorIsVisibleNotDebug` (`internal/agent/coordinator_autoresume_test.go`) — доказывают единый инициатор и его паник-изоляцию; `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent`, `TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent` (`internal/agent/coordinator_agentfor_test.go`) — доказывают `agentFor` как единый choke point за пределами `wakeSession`; `TestSubAgentWorkTerminal_DriverBusyIsNotTerminal` (не редактирован) — драйверный выбор для busy-гейта. | Выполняется. |
| ASYNC-07 | Маршрут доставки и пробуждения не зависит от origin и глубины сессии. | `workLedger.deliverLocked`'s условие для CLI/web: `job.cli && (s.drained || l.onWebDone == nil)` — не изменилось (фикс #1029 сохранён). **Фаза 2 (§4.4):** маршрут теперь ТАКЖЕ не зависит от РЕЖИМА исполнения (sync/async) — `deliverLocked`'s sync-ветка (проверяет `job.sync` ПЕРЕД вычислением `queued`) коротко замыкает доставку через `job.done`/`awaitSync`, полностью в обход и ready-queue, и `onWebDone`, для КАЖДОГО origin, для которого `asyncTool.Run` устанавливает `sync=true` (SDK/unspecified) — единый путь `asyncTool` (§4) убрал прежнюю раннюю ветку по origin, так что `bash`/`run_command`/`agent` теперь ВСЕГДА проходят через один и тот же `workLedger`/`deliverLocked`, независимо от происхождения вызова. Всё ещё НЕ «выполняется» полностью: `notifyBackgroundJobDone`'s разделение Phase-3/Phase-4 остаётся отдельной осью (это политика автономности — когда именно будить, — а не маршрут доставки, не смешивать); транзитивная глубина > 1 сессии по-прежнему не проверена. | `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult` (не редактирован); **новый (фаза 2):** `TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone`, `TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout` (`internal/agent/work_ledger_test.go`, `internal/agent/async_tool_sync_test.go`) — доказывают маршрут независимый от режима исполнения. | Частично — маршрут по сессии (#1029) и по режиму исполнения (sync/async, фаза 2) оба исправлены и проверены; глубина > 1 и Phase-3/4 автономность вне объёма. |
| ASYNC-08 | Отмена области отменяет её задачи и дочерние области; прерывание хода (interrupt) задачи не отменяет. | Первая половина не изменилась по существу: `coordinator.Cancel`/`CancelAll` (`internal/agent/coordinator_interrupt.go`) — `asyncJobs.cancelSession(sessionID)` / `.close()`. **Фаза 2, задача #1054:** `Cancel` (и, отдельно, `InjectMessage`) теперь маршрутизируются через `c.agentFor(sessionID)` вместо безусловного `c.currentAgent` — та же природа дефекта, что #1049 уже закрыла для `notifyAsyncCompletion`, распространена на эти два метода: раньше `Cancel(childID)` на делегированного ребёнка молча не находил его (пустой mailbox `c.currentAgent`'а), а `InjectMessage`'s `injectIfBusy`-проверка занятости смотрела на ЧУЖОЙ (всегда idle) mailbox. Недостижимо в проде сегодня (нет вызывающего с `child_session_id`), но блокировало будущие `stop_agent`/`inject_agent` (план пробуждений) — теперь разблокировано. Вторая половина не изменилась: `context.WithCancel(context.WithoutCancel(ctx))` для async (CLI/web) jobs в `asyncTool.Run`; **фаза 2 добавляет** симметричный, но ОБРАТНЫЙ случай для sync (SDK) jobs — `context.WithCancel(ctx)` (§4.2): a sync job's only consumer is the calling turn itself, so cancelling the caller's ctx MUST cancel it too, unlike a detached CLI/web job. | Белые тесты (не редактированы): `TestWorkLedger_CancelReleasesArmedDelegation`, `TestWorkLedger_CancelSurvivesFinishedChildTurn`. **Новый (фаза 2, задача #1054):** `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent`, `TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent` (`internal/agent/coordinator_agentfor_test.go`). **Новый (§4.2's sync/async ctx split):** `TestAsyncTool_SyncCallerCtxCancelUnblocksAwait` (`internal/agent/async_tool_sync_test.go`). Чёрный тест `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (не редактирован). | Первая половина выполняется, теперь включая `Cancel`/`InjectMessage` на a delegated child id directly (#1054); вторая половина частично проверено (async-detach не тестировано напрямую, sync-attach — да, фаза 2). |
| ASYNC-09 | Неудачная доставка или пробуждение становятся видимым состоянием области, а не записью в Debug-логе. | **Частично, было «нарушается».** Фаза 2 (§1.4) закрывает путь `wakeSession`: `runAutoResumeRecovered`'s Debug и `notifyBackgroundJobDone`'s Phase-3 `InjectMessage`-failure Debug оба заменены — `wakeSession`'s единый `Run`-вызов, на ошибке, делает `slog.Warn` (не Debug) И персистирует видимый маркер в транскрипт владельца: второе сообщение, `NoticeKind = "wake_failed"`, текст «Не удалось продолжить работу после события \<job id\>: \<причина\>. Событие сохранено; продолжение — при следующем ходе.» (orchestrator decision 2026-09-28 item 1) — виден в веб-UI и модели на следующем ходу через обычный пайплайн рендеринга системных сообщений, без новой схемы БД. `refreshSubAgentCompletion`'s DB-read `slog.Debug` (`coordinator_work_scope.go`) остаётся нарушением — вне объёма фазы 2 (см. спецификации §1.4: «это не пробуждение, а свежесть содержимого уже доставляемого результата», свой безопасный фолбэк уже есть). | **Новый (фаза 2):** `TestWakeSession_RunErrorIsVisibleNotDebug` (`internal/agent/coordinator_autoresume_test.go`) — проверяет, что провал `Run` после успешного персиста создаёт ВТОРОЙ вызов `InjectMessage` с `NoticeKind == "wake_failed"`, содержащий job id. | Частично — путь `wakeSession` (доминирующий источник авто-resume уведомлений) закрыт; `refreshSubAgentCompletion`'s DB-read Debug — по-прежнему нарушение, явно вне объёма фазы 2, остаётся для фазы 4/5. |
| ASYNC-10 | Любой ответ «почему сессия ждёт» выводится из данных, доступных любому процессу, и называет конкретную задачу. | Кросс-процессный путь — `internal/session/descendant_liveness.go`'s `LiveDescendants` (эвристика: lock-файл + heartbeat потомка), питает `sessions why`/`sessions list`. Внутрипроцессный путь — только `coordinator.ParkedSubAgentParents` (`internal/agent/coordinator_work_scope.go`); `DescendantWorkPending` **удалена фазой 3** (§3, не заменена — `workLedger.next()` уже отвечает на «есть ли ещё работа», не на «какая сессия ждёт», это другой вопрос). `ParkedSubAgentWorkReporter`/`ParkedSubAgentParents` не тронуты этой фазой; единственный потребитель — `internal/cmd/sessions_list.go:94-95` (`rush sessions list`), сверено `grep` по `internal/server`, `internal/app`, `internal/cmd` (orchestrator decision 2026-09-28 item 3) — веб-сервер его не читает вовсе, так что ничего видимого в веб-UI эта фаза не меняет. Это ДВА РАЗНЫХ механизма, отвечающих на один вопрос: кросс-процессный называет «дочерняя сессия жива», а не конкретную задачу/tool-call. | `TestLiveDescendants_GrandchildHoldsLiveLock` (`internal/session/descendant_liveness_test.go`), `TestExplainSessionStatus_GrandchildLiveIsDelegating` (`internal/cmd/sessions_why_descendant_test.go`), `TestMarkParkedDelegationSessions_*` (`internal/cmd/sessions_list_parked_delegation_test.go`) — все на глубину до grandchild включительно; не редактированы этой фазой. | Частично — отвечает «какая сессия ждёт», не «какая задача»; использует отдельный эвристический (lock-файл) механизм вместо данных из процесса, которому реально принадлежит работа — закрывается фазой 4 (единый реестр, читаемый любым процессом). Статус не меняется этой фазой. |

## Находка этой фазы: вложенная делегация (`agent`-инструмент внутри
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
делегации) заслуживает отдельного P0/P1-разбора у оператора.

Следствие для сценариев (a): нет сегодня БЕЗОПАСНОГО чёрного пути создать
сессию-«внука» через реальный `rush run`, чтобы проверить транзитивность
`DescendantWorkPending`/`subAgentWorkTerminal` глубже одного уровня:

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

Сценарий (a) поэтому убран из набора тестов этой фазы (см. ниже), а не
дописан «в обход» — правка production-кода запрещена рамками фазы 0.

## Побочная находка: `IsSessionBusy` не видит делегированного под-агента

При отладке сценария (d) выяснилось, что `AgentCoordinator.IsSessionBusy
(childSessionID)` для делегированной дочерней сессии ВСЕГДА возвращает
`false`, независимо от реального состояния ребёнка: делегированный под-агент
исполняется на ОТДЕЛЬНОМ `*sessionAgent`, построенном заново `coordinator.
buildAgent` внутри `agentTool()`/`agenticFetchTool()`
(`coordinator_tools.go:27`, `agent_tool.go:44`), а не на `c.currentAgent`
координатора, единственном экземпляре, который `IsSessionBusy` опрашивает.
Существующий тест `async_scenarios_cancel_test.go` полагается вместо этого
на прямое наблюдение за живым HTTP-запросом ребёнка.

**Статус на конец фазы 1:** закрыто задачей #1049 (влито в это дерево до
начала фазы 1) — `internal/agent/coordinator_subagent_drivers.go`'s
`subAgentDriverRegistry` (`c.subAgentDrivers`) регистрирует, какой
`*sessionAgent` реально ведёт ходы `childSessionID`. `workLedger.
childScopeDrained` (`internal/agent/work_ledger_delegation.go:151-172`,
замещает `coordinator.subAgentWorkTerminal`) консультирует именно этот
драйвер (`driver.agent.IsSessionBusy(childID)`), с откатом на
`c.currentAgent` только для сессии без зарегистрированного драйвера
(голые тестовые фикстуры). Доказывающий тест:
`TestSubAgentWorkTerminal_DriverBusyIsNotTerminal`
(`internal/agent/coordinator_subagent_drivers_test.go`).

## Пропущенные/убранные сценарии (эта фаза)

Кандидаты a/b/c/d из задания рассмотрены; только (d) добавлен как новый
чёрный тест:

- **(a) Grandchild — убрано.** См. «Находка этой фазы» выше: единственные
  два реальных способа создать сессию-«внука» либо вешают координатор,
  либо требуют недетерминированной сетевой зависимости. Заменить нечем без
  изменения production-кода, что вне рамок фазы 0.
- **(b) «Прерывание не отменяет задачи» (вторая половина ASYNC-08) — пропущено.**
  Механизм существует и обоснован по коду (`context.WithoutCancel`,
  `async_tool.go:55` — контекст задачи отделён от контекста хода ДО того,
  как что-либо может его отменить), но нет способа детерминированно
  проверить его через `RunNonInteractiveWithResult` без гонки одного из двух
  фрагильных путей: (1) заставить реальную генерацию «зависнуть» ровно в
  момент, когда фиксированный 3-секундный `interruptInjectTick`
  (`internal/agent/coordinator_interrupt.go:28`, не переменная — в отличие
  от `subAgentOutcomeTickInterval`) успевает сработать, что не укладывается
  надёжно в бюджет «быстрый путь < 10 с»; либо (2) отправить interrupt на
  ПРОСТАИВАЮЩУЮ сессию, что уводит доставку через `RunQueuePump`'s
  независимый тикер — второй недетерминированный механизм только ради
  наблюдения свойства первого. Зафиксировано в реестре как «не проверено»
  вместо теста.
- **(c) «Корень держится, пока жив процесс, которым владеет ребёнок» — не
  дублируется.** Уже существует и проходит с самого начала работы над
  делегацией: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`
  (`internal/app/app_run_subagent_root_wait_test.go`) — барьер там прямо
  проверяет, что финальный ход корня не может начаться, пока дочерний
  процесс жив, и что он начинается сразу после. Переписывать тот же сценарий
  во втором файле добавило бы дублирующую поверхность без нового покрытия;
  вместо этого он процитирован как доказывающий тест для ASYNC-02/ASYNC-04
  выше.
- **(d) Отмена ctx + `CancelAll` — реализовано.** См.
  `internal/app/async_scenarios_cancel_test.go`
  (`TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn`).
