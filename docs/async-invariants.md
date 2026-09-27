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
| ASYNC-01 | У каждой задачи ровно один владелец; у дочерней сессии в каждый момент не больше одной активной делегации. | Владелец задачи: `asyncJobRegistry.start` (`internal/agent/async_job_registry.go:90-109`) — одна запись на ключ `(sessionID, toolCallID)`, повторный `start` с тем же ключом отклоняется. «Не больше одной активной делегации» на дочернюю сессию обеспечивается не async-кодом, а обычной эксклюзивностью mailbox сессии (`internal/agent/agent_control.go`, admission gate одной генерации на сессию) — делегирование в уже занятую дочернюю сессию либо встаёт в очередь, либо (при `FailIfSessionBusy`) отклоняется. | Нет теста именно на отклонение повторного `start` с тем же `toolCallID` (побочно упражняется в `TestAsyncJobRegistryWaitsForPersistedToolResult`, но не проверяется как assertion). Эксклюзивность mailbox — см. `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerPolicy` (`internal/app/app_run_admission_race_test.go`), не специфично для делегации. | Частично — владение одной записью на задачу выполняется по конструкции, но не проверено отдельным тестом; «не больше одной активной делегации на дочернюю сессию» — не async-специфичный механизм и не проверен для делегации конкретно. |
| ASYNC-02 | Область открыта, пока у неё есть незавершённая или недоставленная задача (кроме `loop` и надзора), идущий ход, ожидающее событие или открытая дочерняя область. CLI-хост завершается только при закрытой области корня, по `--timeout` или по отмене оператора. | `internal/app/app_run_async.go`'s `runNonInteractiveWithAsyncResults` (цикл `NextAsyncCompletion`/`descendantWorkPending`, держит корневой цикл открытым, пока `DescendantWorkPending` истинно); транзитивный BFS-обход — `coordinator.DescendantWorkPending` (`internal/agent/subagent_outcome.go:291-336`) над `asyncJobRegistry.running`, `BackgroundShellManager.ActiveOwned`, `subAgentOutcomeRegistry.hasParkedFor`. Выход CLI-хоста по `--timeout`/Ctrl-C — `internal/cmd/run.go:619-719` (ctx-отмена, не явный `CancelAll`). | Одноуровневая делегация: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob` (`internal/app/app_run_subagent_root_wait_test.go`), `TestRunNonInteractiveWaitsForAsyncSubAgentResult`, `TestRunNonInteractiveContinuesPastFiveAsyncCompletions` (`internal/app/app_run_async_completion_test.go`); отмена корневого `ctx` посреди живой делегации — новый `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (`internal/app/async_scenarios_cancel_test.go`, эта фаза). Транзитивность BFS на глубину > 1 (grandchild) НЕ проверена и не может быть безопасно проверена сегодня чёрным тестом — см. «Находка этой фазы» ниже: единственный публичный путь создать сессию-«внука» (вложенная делегация через инструмент `agent`) зависает. | Выполняется на проверенных путях (1 уровень + отмена ctx); транзитивность глубже одного уровня — не проверено (заблокировано найденным дефектом, не отсутствием желания). `loop`/надзор из этого закона сегодня не существуют вовсе (появляются в фазе 5) — для них закон не применим. |
| ASYNC-03 | Задача достигает терминального состояния ровно один раз; гонка finish/stop/timeout/cancel/interrupted даёт один исход. | `asyncJobRegistry.finish` / `.finishParked` / `.abort` / `.cancelSession` (`internal/agent/async_job_registry.go:149-255`) — каждый метод по отдельности защищён (`job.completion != nil` возврат, `job == nil` возврат), но НЕТ единого CAS между `finish` и `cancelSession`: `cancelSession` заменяет `s.jobs` целиком до того, как конкурентный `finish` для той же сессии успевает найти свою запись — тогда `finish` не находит `job` (`job == nil`) и молча теряет доставку. Явного `deadline`/timeout на задачу сегодня нет вообще (появится в фазе 1 как `Job.deadline`). | `TestAsyncJobRegistryWebCallbackExactlyOnce` (finish×finish), `TestAsyncJobRegistryConcurrentFinishAndAcknowledge` (finish×acknowledged), `TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce` (parked-release race). Ни один тест не гоняет `finish` против `cancelSession`/`abort` для одной и той же задачи. | Нарушается — закрывается фазой 1. Соответствует диагностике design-doc №4 и BL-2/#1032 («двойная или потерянная доставка при гонке finish/cancel/timeout»). |
| ASYNC-04 | Каждая задача, о старте которой сообщено модели, доставляется владельцу ровно один раз — в том числе отменённая, просроченная и прерванная смертью хоста. | Ack-gate: `onToolResult` (`internal/agent/agent_turn_stream.go:389-395`) вызывает `asyncJobs.acknowledged`/`abort` только после того, как tool result персистентно записан; доставка — `releaseLocked` (`async_job_registry.go:216-230`), ровно один раз при `acknowledged ∧ completion != nil`. Транзитивная доставка для делегаций — `subAgentOutcomeRegistry` (`internal/agent/subagent_outcome.go`), фикс #1029 для CLI-дочерних сессий. | `TestAsyncJobRegistryWaitsForPersistedToolResult` (ack-gate); `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult` (`internal/app/app_run_subagent_async_result_test.go`, регрессия #1029, 1 уровень). Доставка на глубину > 1 (внук доставляет ребёнку, ребёнок доставляет корню) не проверена — см. «Находка этой фазы»: нет безопасного чёрного пути создать сессию-«внука» сегодня. | Частично — выполняется для живой (в памяти) доставки на 1 уровень; глубина > 1 не проверена (заблокировано найденным дефектом); нарушается для смерти хоста: задача без host/lease (#1040) теряется без следа при падении процесса и не переживает рестарт — закрывается фазой 4. |
| ASYNC-05 | Уведомление никогда не опережает сохранённый tool result, объявивший задачу. | `releaseLocked` требует `job.acknowledged == true` (установлено только `onToolResult` после успешной записи tool result) прежде чем что-либо доставить (`async_job_registry.go:216-230`); `finishParked` для уже удалённой строки создаёт её заново уже помеченной `acknowledged: true` — но это ветка «строка исчезла ПОСЛЕ настоящего ack», не обход ack-гейта. | `TestAsyncJobRegistryWaitsForPersistedToolResult`, `TestAsyncToolWebCompletionWaitsForToolResult` (`internal/agent/async_tool_test.go`). | Выполняется. |
| ASYNC-06 | Ход начинает только драйвер сессии; остальные компоненты публикуют события. | Нарушается двумя местами в `internal/agent/coordinator_background.go`: `notifyAsyncCompletion` (~строка 47-64) и `notifyBackgroundJobDone`'s Phase-4 ветка (~строка 105-138) оба делают `go runAutoResumeRecovered(ctx, ..., func(ctx) { return c.Run(ctx, sessionID, ...) })` — колбэк-горутина напрямую запускает ход через `c.Run`, а не публикует событие для драйвера сессии (mailbox). Тот же корень, что диагностика design-doc №6 («поставлено в очередь» = `(nil, nil)` от `sessionAgent.Run` для занятой сессии). | Нет теста, проверяющего закон как ПОЗИТИВНОЕ требование («ход стартует только из mailbox») — существующие тесты (`TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON` и соседние в `internal/app/app_run_async_completion_test.go`) закрепляют ТЕКУЩУЮ, нарушающую закон форму (прямой `c.Run` из колбэка), а не целевую. | Нарушается — закрывается фазой 2 (design doc: «удаляет горутины `c.Run` в `notifyAsyncCompletion` и `notifyBackgroundJobDone`»). Связано с #1036. |
| ASYNC-07 | Маршрут доставки и пробуждения не зависит от origin и глубины сессии. | `asyncJobRegistry.queuesLocked` (`internal/agent/async_job_registry.go:125-127`): `job.cli && (s.drained || onWebDone == nil)` — маршрут теперь решается по ТОМУ, разбирает ли цикл именно ЭТУ сессию (`s.drained`, устанавливается `markDrained` только для корня CLI-цикла), а не по чистому origin — это и есть фикс #1029 (диагностика №2 в design-doc, «маршрут выбирается по origin, а не по сессии»). Но `job.cli` всё ещё участвует в условии, и по РОДУ работы путей доставки по-прежнему четыре разных (диагностика design-doc №1): `asyncJobRegistry` ready-queue/callback, `subAgentOutcomeRegistry` (4 триггера + тикер), `notifyBackgroundJobDone` для фоновых shell вне async-обёртки, `InjectMessage` Phase-3 fallback. | `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult` — доказывает именно фикс #1029 (дочерняя CLI-сессия получает свой async-результат, а не теряет его в недренируемой очереди). Нет теста, показывающего единый маршрут НЕЗАВИСИМО от вида работы. | Частично — маршрут по сессии (#1029) исправлен и проверен; независимость от ВИДА работы (4 разных пути) не выполняется — закрывается фазами 2–3. |
| ASYNC-08 | Отмена области отменяет её задачи и дочерние области; прерывание хода (interrupt) задачи не отменяет. | Первая половина: `coordinator.Cancel`/`CancelAll` (`internal/agent/coordinator_interrupt.go:41-64`) освобождают запаркованные делегации как отменённые, `asyncJobs.cancelSession`/`.close()` отменяют контексты задач, `sessionAgent.CancelAll` (`internal/agent/agent_control.go:161-200+`) проходит по КАЖДОМУ mailbox (корень и любой потомок) и жёстко останавливает и генерацию, и диспетчер. Вторая половина: путь interrupt-inject (`handleInterruptTick`/`InterruptAndReplace`, `coordinator_interrupt.go`) трогает только машину замены генерации mailbox, никогда `asyncJobs`/`subAgentOutcomes`; контекст задачи делегированного инструмента структурно отделён от контекста хода вызывающего (`context.WithCancel(context.WithoutCancel(ctx))`, `internal/agent/async_tool.go:55`), так что прерывание хода технически не может её достать. | Белые тесты: `TestSubAgentOutcome_CancelReleasesParkedOutcome`, `TestSubAgentOutcome_CancelSurvivesFinishedChildTurn` (`internal/agent/subagent_outcome_test.go`). Новый чёрный тест этой фазы: `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn` (`internal/app/async_scenarios_cancel_test.go`) — доказывает первую половину end-to-end через реальный вложенный mailbox-ход, и явно документирует, что ОДНА ТОЛЬКО отмена ctx недостаточна (только двухшаговая последовательность `--timeout`/Ctrl-C + `Shutdown`→`CancelAll`, как в `cmd/run.go`, закрывает область целиком). Вторая половина («прерывание не отменяет задачи») теста не имеет вовсе — см. раздел «Пропущенные сценарии». | Первая половина выполняется (двумя шагами: отмена ctx хода + `CancelAll`); вторая половина — не проверено (механизм есть — `WithoutCancel` — но не запротоколирован тестом). |
| ASYNC-09 | Неудачная доставка или пробуждение становятся видимым состоянием области, а не записью в Debug-логе. | Нарушается: `runAutoResumeRecovered` (`internal/agent/coordinator_background.go:170-183`) — неудачный auto-resume ход логируется `slog.Debug`; `notifyBackgroundJobDone`'s `InjectMessage` failure — тоже `slog.Debug` (~строка 146-151); `refreshSubAgentCompletion`'s DB-read failure — тоже `slog.Debug` (`internal/agent/subagent_outcome.go:246-248`). Ни один из трёх не становится состоянием, которое `sessions why`/реестр может прочитать. | Нет и не может быть теста «на отсутствие видимого состояния» без самого состояния — не проверено (по построению). | Нарушается — закрывается фазами 4–5 (#1035; наблюдаемость через будущий реестр/`sessions why`). |
| ASYNC-10 | Любой ответ «почему сессия ждёт» выводится из данных, доступных любому процессу, и называет конкретную задачу. | Кросс-процессный путь — `internal/session/descendant_liveness.go`'s `LiveDescendants` (эвристика: lock-файл + heartbeat потомка), питает `sessions why`/`sessions list`. Внутрипроцессный путь — `coordinator.ParkedSubAgentParents`/`DescendantWorkPending` (`internal/agent/subagent_outcome.go`). Это ДВА РАЗНЫХ механизма, отвечающих на один вопрос: кросс-процессный называет «дочерняя сессия жива», а не конкретную задачу/tool-call. | `TestLiveDescendants_GrandchildHoldsLiveLock` (`internal/session/descendant_liveness_test.go`), `TestExplainSessionStatus_GrandchildLiveIsDelegating` (`internal/cmd/sessions_why_descendant_test.go`), `TestMarkParkedDelegationSessions_*` (`internal/cmd/sessions_list_parked_delegation_test.go`) — все на глубину до grandchild включительно. | Частично — отвечает «какая сессия ждёт», не «какая задача»; использует отдельный эвристический (lock-файл) механизм вместо данных из процесса, которому реально принадлежит работа — закрывается фазой 4 (единый реестр, читаемый любым процессом). |

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
на прямое наблюдение за живым HTTP-запросом ребёнка. Стоит учесть в фазе 1:
единый реестр работы должен либо не зависеть от того, на каком `*sessionAgent`
исполняется задача, либо явно документировать эту границу.

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
