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

**Anchored 2026-09-27, worktree `async-phase0`.** Line numbers drift; re-anchor
before relying on them. Each later migration phase (1–5) MUST re-anchor every
row its changes touch, in the same commit that makes the change — a registry
that lies about where its law lives is worse than no registry (see
`docs/mcp-invariants.md`'s own rule, which this registry copies).

## Invariants

| ID | Закон | Где сегодня (файл:функция) | Доказывающие тесты | Статус |
|---|---|---|---|---|
| ASYNC-01 | У каждой задачи ровно один владелец; у дочерней сессии в каждый момент не больше одной активной делегации. | Владелец задачи: `workLedger.Start` (`internal/agent/work_ledger.go:110-130`) — одна запись `*asyncJob` на ключ `(owner, toolCallID)`. Фаза 1 меняет поведение на повторном ключе: вместо отказа `Start` теперь идемпотентно возвращает существующую запись (`existing=true, err=nil`), закрывая #1038 — вызывающий (`asyncTool.Run`) не должен повторно запускать исполнителя. «Не больше одной активной делегации» на дочернюю сессию по-прежнему обеспечивается не async-кодом, а обычной эксклюзивностью mailbox сессии (`internal/agent/agent_control.go`, admission gate одной генерации на сессию). | `TestWorkLedger_StartIsIdempotentPerToolCallID` (`internal/agent/work_ledger_test.go`) — прямая проверка идемпотентности на дублирующемся `(owner, toolCallID)`, закрывающая прежний пробел покрытия. Эксклюзивность mailbox — см. `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerPolicy` (`internal/app/app_run_admission_race_test.go`), не специфично для делегации. | Частично — владение одной записью на задачу теперь проверено тестом и идемпотентно по конструкции (#1038 закрыт); «не больше одной активной делегации на дочернюю сессию» — по-прежнему не async-специфичный механизм и не проверен для делегации конкретно. |
| ASYNC-02 | Область открыта, пока у неё есть незавершённая или недоставленная задача (кроме `loop` и надзора), идущий ход, ожидающее событие или открытая дочерняя область. CLI-хост завершается только при закрытой области корня, по `--timeout` или по отмене оператора. | `internal/app/app_run_async.go`'s `runNonInteractiveWithAsyncResults` (цикл `NextAsyncCompletion`/`descendantWorkPending`, держит корневой цикл открытым, пока `DescendantWorkPending` истинно); транзитивный BFS-обход — `coordinator.DescendantWorkPending` (`internal/agent/coordinator_work_scope.go:89-124`, was `subagent_outcome.go:291-336`) над `workLedger.running`, `BackgroundShellManager.ActiveOwned`, `workLedger.hasParkedFor` (merged registry, phase 1). Выход CLI-хоста по `--timeout`/Ctrl-C — `internal/cmd/run.go:619-719` (ctx-отмена, не явный `CancelAll`). | Одноуровневая делегация: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob` (`internal/app/app_run_subagent_root_wait_test.go`), `TestRunNonInteractiveWaitsForAsyncSubAgentResult`, `TestRunNonInteractiveContinuesPastFiveAsyncCompletions` (`internal/app/app_run_async_completion_test.go`); отмена корневого `ctx` посреди живой делегации — новый `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (`internal/app/async_scenarios_cancel_test.go`, эта фаза). Транзитивность BFS на глубину > 1 (grandchild) НЕ проверена и не может быть безопасно проверена сегодня чёрным тестом — см. «Находка этой фазы» ниже: единственный публичный путь создать сессию-«внука» (вложенная делегация через инструмент `agent`) зависает. | Выполняется на проверенных путях (1 уровень + отмена ctx); транзитивность глубже одного уровня — не проверено (заблокировано найденным дефектом, не отсутствием желания). `loop`/надзор из этого закона сегодня не существуют вовсе (появляются в фазе 5) — для них закон не применим. |
| ASYNC-03 | Задача достигает терминального состояния ровно один раз; гонка finish/stop/timeout/cancel/interrupted даёт один исход. | Единый CAS: `asyncJob.transitionToTerminal` (`internal/agent/work_job.go:78-84`) — единственный писатель `state` дальше `phaseRunning`, вызывается под одним `workLedger.mu` из `.finish` (`internal/agent/work_ledger.go:236`), `.cancelSession` (`internal/agent/work_ledger_delegation.go:192`) и `.recheckChild` (`work_ledger_delegation.go:82`). `cancelSession` больше не подменяет карту сессии целиком (как делал `asyncJobRegistry.cancelSession`) — оно проходит владельца и `byChild`-записи поштучно под тем же мьютексом, поэтому конкурентный `finish`/`recheckChild` для ТОЙ ЖЕ задачи либо находит её и проигрывает CAS, либо не находит вовсе (уже доставлена) — окно «карта подменена из-под конкурента» устранено структурно. `deadline`/`timeoutKind`/`FireTimeout`/`phaseInterrupted` сознательно НЕ добавлены в фазе 1 (нет вызывающего в проде — см. `docs/plans/2026-09-27-async-phase1-spec.md`, «Решения оркестратора» п.3), так что гонка с timeout/interrupted остаётся вне области до фаз 4–5. | `TestWorkLedger_WebCallbackExactlyOnce`, `TestWorkLedger_ConcurrentFinishAndAcknowledge` (порты прежних тестов); `TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome` (новый — finish×finish×cancelSession для ОДНОЙ задачи, прямое доказательство CAS); `TestWorkLedger_ConcurrentRecheckDeliversOnce` (порт `..._ConcurrentTryReleaseDeliversOnce`); `TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce` (новый — recheckChild×cancelSession для армированной делегации, доказывает отсутствие «порванного» состояния). | Выполняется — закрыто фазой 1 (#1032/BL-2). Соответствует диагностике design-doc №4; таймаут/interrupted вне области фазы 1 (тип/CAS не заведены — нет вызывающего). |
| ASYNC-04 | Каждая задача, о старте которой сообщено модели, доставляется владельцу ровно один раз — в том числе отменённая, просроченная и прерванная смертью хоста. | Ack-gate: `onToolResult` (`internal/agent/agent_turn_stream.go:389-395`, не тронут фазой 1) вызывает `asyncJobs.acknowledged`/`.abort` только после того, как tool result персистентно записан; доставка — единая `workLedger.deliverLocked` (`internal/agent/work_ledger.go:159-183`), ровно один раз при `announced ∧ state.terminal() ∧` запись ещё присутствует в карте владельца. Транзитивная доставка для делегаций слита в тот же тип: `workLedger.armDelegation`/`.recheckChild` (`internal/agent/work_ledger_delegation.go:45,82`), фикс #1029 сохранён byte-for-byte. Отменённая делегация ВСЕГДА доставляется через `deliverLocked` (текст `subAgentOutcomeCancelledText`); отменённая ПРОСТАЯ (не делегация) задача по-прежнему молча отбрасывается без доставки — то же поведение, что и сегодняшний `asyncJobRegistry.cancelSession`, сохранено намеренно (см. `workLedger.cancelSession`'s doc, `work_ledger_delegation.go:192`, и задание «внешне видимое поведение не меняется»). | `TestWorkLedger_WaitsForPersistedToolResult` (ack-gate, порт); `TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce` (новый — ack-gate специфично для делегации, ранее непроверенный пробел покрытия); `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult` (`internal/app/app_run_subagent_async_result_test.go`, регрессия #1029, 1 уровень, не редактирован). Доставка на глубину > 1 по-прежнему не проверена (см. «Находка этой фазы» ниже, вне области фазы 1). | Частично — выполняется для живой (в памяти) доставки на 1 уровень, включая отменённые делегации; глубина > 1 не проверена; отменённая ПРОСТАЯ задача по-прежнему не доставляется вовсе (сохранённое, не регрессия фазы 1); нарушается для смерти хоста — закрывается фазой 4. |
| ASYNC-05 | Уведомление никогда не опережает сохранённый tool result, объявивший задачу. | `workLedger.deliverLocked` требует `job.announced == true` (установлено только `.acknowledged`, вызываемым `onToolResult` после успешной записи tool result) прежде чем что-либо доставить (`internal/agent/work_ledger.go:159-183`). Слияние регистров убрало прежнюю `finishParked`'s ветку пересоздания удалённой строки целиком — задача теперь ОДНА запись от `Start` до доставки, пересоздавать нечего (класс бага BL-2/#1032 в этой части закрыт как побочный эффект). | `TestWorkLedger_WaitsForPersistedToolResult`, `TestAsyncToolWebCompletionWaitsForToolResult` (`internal/agent/async_tool_test.go`, не редактирован по сценарию); `TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce` (новый — та же гарантия для армированной, но ещё не объявленной делегации, ранее непроверенный пробел). | Выполняется. |
| ASYNC-06 | Ход начинает только драйвер сессии; остальные компоненты публикуют события. | Файл не тронут фазой 1: `internal/agent/coordinator_background.go`'s `notifyAsyncCompletion` (~строка 47-95) и `notifyBackgroundJobDone`'s Phase-4 ветка (~строка 136-169) по-прежнему делают `go runAutoResumeRecovered(ctx, ..., func(ctx) {...})` — колбэк-горутина напрямую запускает ход, а не публикует событие. `notifyAsyncCompletion` (задача #1049, уже влито в это дерево ДО начала фазы 1) выбирает МЕЖДУ `c.Run` и зарегистрированным `c.subAgentDrivers.get(sessionID)`'s `driver.agent.Run(call)` — корректный `SessionAgent` для делегированного ребёнка вместо безусловного `c.currentAgent` — но выбор по-прежнему происходит из горутины, не через публикацию события драйверному mailbox. Спецификация фазы 1 (`docs/plans/2026-09-27-async-phase1-spec.md` §0.2) предполагала новый метод `c.wakeOwner`; «Решения оркестратора» отменили эту точку расширения, т.к. #1049 уже реализовал драйверный выбор напрямую внутри `notifyAsyncCompletion`/`subAgentWorkTerminal` (последний теперь `workLedger.childScopeDrained`). | Нет теста, проверяющего закон как ПОЗИТИВНОЕ требование. Существующие тесты (`internal/app/app_run_async_completion_test.go`, не редактированы) закрепляют ТЕКУЩУЮ, нарушающую закон форму. Драйверный выбор — `TestSubAgentWorkTerminal_DriverBusyIsNotTerminal` (`internal/agent/coordinator_subagent_drivers_test.go`). | Нарушается — закрывается фазой 2. Фаза 1 не меняет это по существу и не трогает этот файл. Связано с #1036. |
| ASYNC-07 | Маршрут доставки и пробуждения не зависит от origin и глубины сессии. | `workLedger.deliverLocked`'s условие (`internal/agent/work_ledger.go:179`): `job.cli && (s.drained || l.onWebDone == nil)` — байт-в-байт то же условие, что было в `asyncJobRegistry.queuesLocked` (удалён фазой 1); маршрут по-прежнему решается по тому, разбирает ли цикл именно ЭТУ сессию (`s.drained`), а не по origin — фикс #1029 сохранён. По РОДУ работы путей доставки по-прежнему четыре разных (диагностика design-doc №1), но слияние в фазе 1 объединило ДВА из них (`asyncJobRegistry`+`subAgentOutcomeRegistry` → `workLedger`) в ОДНУ точку доставки (`deliverLocked`) вместо двух реализаций в двух регистрах; `notifyBackgroundJobDone` (фоновые shell вне async-обёртки) и `InjectMessage` Phase-3 fallback остаются отдельными путями — фазы 2–3. | `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult` (не редактирован) — доказывает фикс #1029 byte-for-byte. Нет теста, показывающего единый маршрут НЕЗАВИСИМО от вида работы. | Частично — маршрут по сессии (#1029) исправлен и проверен, теперь через одну функцию для обоих видов работы (план vs делегация); независимость от ВИДА работы (4 разных пути) не выполняется — закрывается фазами 2–3. |
| ASYNC-08 | Отмена области отменяет её задачи и дочерние области; прерывание хода (interrupt) задачи не отменяет. | Первая половина: `coordinator.Cancel`/`CancelAll` (`internal/agent/coordinator_interrupt.go:41-63`) — фаза 1 схлопнула прежние три/два вызова (`releaseSubAgentOutcomesForParentCancel`+`...ForChildCancel`+`asyncJobs.cancelSession`, все удалены) в ОДИН каждый: `asyncJobs.cancelSession(sessionID)` / `.close()` (`internal/agent/work_ledger_delegation.go:192`, `internal/agent/work_ledger.go:325`). `cancelSession` теперь само проходит ОБА направления (задачи, которыми владеет sessionID, и делегации, запаркованные на нём как на ребёнке) под ОДНИМ мьютексом — см. ASYNC-03. `sessionAgent.CancelAll` (`internal/agent/agent_control.go:161-200+`, не тронуто) по-прежнему проходит по КАЖДОМУ mailbox и жёстко останавливает генерацию и диспетчер. Вторая половина не изменилась: interrupt-inject (`handleInterruptTick`/`InterruptAndReplace`, `coordinator_interrupt.go`, не тронуто по существу) трогает только машину замены генерации mailbox, никогда `asyncJobs`; `context.WithCancel(context.WithoutCancel(ctx))` в `asyncTool.Run` (`internal/agent/async_tool.go:55`, не тронуто по существу) по-прежнему отделяет контекст задачи от контекста хода. | Белые тесты (порты): `TestWorkLedger_CancelReleasesArmedDelegation`, `TestWorkLedger_CancelSurvivesFinishedChildTurn` (`internal/agent/work_ledger_delegation_test.go`, была `internal/agent/subagent_outcome_test.go`, удалён фазой 1). Чёрный тест: `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (`internal/app/async_scenarios_cancel_test.go`, не редактирован) — доказывает первую половину end-to-end. Вторая половина по-прежнему без теста — см. раздел «Пропущенные сценарии». | Первая половина выполняется (двумя шагами: отмена ctx хода + `CancelAll`); вторая половина — не проверено (механизм есть — `WithoutCancel` — но не запротоколирован тестом). |
| ASYNC-09 | Неудачная доставка или пробуждение становятся видимым состоянием области, а не записью в Debug-логе. | Нарушается: `runAutoResumeRecovered` (`internal/agent/coordinator_background.go:170-183`) — неудачный auto-resume ход логируется `slog.Debug`; `notifyBackgroundJobDone`'s `InjectMessage` failure — тоже `slog.Debug` (~строка 146-151); `refreshSubAgentCompletion`'s DB-read failure — тоже `slog.Debug` (`internal/agent/coordinator_work_scope.go:44`, was `subagent_outcome.go:246-248`, moved unchanged by phase 1). Ни один из трёх не становится состоянием, которое `sessions why`/реестр может прочитать. | Нет и не может быть теста «на отсутствие видимого состояния» без самого состояния — не проверено (по построению). | Нарушается — закрывается фазами 4–5 (#1035; наблюдаемость через будущий реестр/`sessions why`). |
| ASYNC-10 | Любой ответ «почему сессия ждёт» выводится из данных, доступных любому процессу, и называет конкретную задачу. | Кросс-процессный путь — `internal/session/descendant_liveness.go`'s `LiveDescendants` (эвристика: lock-файл + heartbeat потомка), питает `sessions why`/`sessions list`. Внутрипроцессный путь — `coordinator.ParkedSubAgentParents`/`DescendantWorkPending` (`internal/agent/coordinator_work_scope.go`, was `subagent_outcome.go`, moved unchanged by phase 1). Это ДВА РАЗНЫХ механизма, отвечающих на один вопрос: кросс-процессный называет «дочерняя сессия жива», а не конкретную задачу/tool-call. | `TestLiveDescendants_GrandchildHoldsLiveLock` (`internal/session/descendant_liveness_test.go`), `TestExplainSessionStatus_GrandchildLiveIsDelegating` (`internal/cmd/sessions_why_descendant_test.go`), `TestMarkParkedDelegationSessions_*` (`internal/cmd/sessions_list_parked_delegation_test.go`) — все на глубину до grandchild включительно. | Частично — отвечает «какая сессия ждёт», не «какая задача»; использует отдельный эвристический (lock-файл) механизм вместо данных из процесса, которому реально принадлежит работа — закрывается фазой 4 (единый реестр, читаемый любым процессом). |

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
