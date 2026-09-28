# Фаза 3: учёт областей и событие «область закрыта» — спецификация реализации

Статус: спецификация к утверждённому дизайну
(`docs/plans/2026-09-27-async-structured-concurrency.md`), реализация не
начата. Опирается на фазы 1–2, уже слитые в это дерево (`internal/agent/
work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
`work_ledger_timeout.go`, `coordinator_work_scope.go`, `coordinator_wake.go`,
`coordinator_run_await.go`, `coordinator_subagent_drivers.go`,
`coordinator_subagents.go`, `async_tool.go`). Инварианты —
`docs/async-invariants.md`; эта спецификация обязана переанкерить строки,
которые меняет (см. «Реанкеровка» в конце).

Каждая строка ниже сверена с кодом на HEAD этого worktree (`phase3-spec`, база
`main@62647444`); где сверить не удалось — написано «не проверено», а не
предположение. Тесты и продакшн-код не запускались и не менялись (это только
спецификация).

## 0. Что учтено дополнительно к дизайн-документу

### 0.1 Задача #1050 (глубина делегации = 1) уже реализована

Дизайн-документ и фаза 0/1 фиксировали глубину делегации 1 как ВРЕМЕННОЕ
структурное ограничение («Находка этой фазы» в `docs/async-invariants.md`:
вложенная делегация вешает `readyWg`). На HEAD этого дерева ограничение уже
СТАЛО задачей и реализовано: `internal/agent/coordinator_tools.go:620` несёт
guard-комментарий «Recursion guard (task #1050)», и
`internal/agent/nested_agent_tool_recursion_test.go` — регрессионный тест,
который явно падает («nested agent-tool recursion (task #1050) is back»),
если guard когда-нибудь уберут. Это значит: сегодня НИ ОДНА сессия не может
быть одновременно и делегированным ребёнком, и родителем собственного
делегированного ребёнка — дерево делегации имеет ровно два уровня (корень и
его прямые дети).

Ниже эта спецификация не ЗАВИСИТ от этого ограничения как от условия
корректности (проектируется в общем виде, с защитой от циклов там, где это
дёшево), но использует его, чтобы объяснить, почему сегодняшний трассированный
код уже ведёт себя корректно транзитивно — см. §1.

### 0.2 `jobKind`/`HoldsScope` не реализованы фазой 1 — расхождение с §7 wake-tools-contract

Черновик фазы 1 (`docs/plans/2026-09-27-async-phase1-spec.md` §1.1) описывал
`jobKind`/`func (jobKind) HoldsScope() bool` как способ будущим `loop`/`timer`
(фаза 5) не держать область (решение оператора п. 3: «`loop` и надзор не
удерживают область»). На HEAD `internal/agent/work_job.go` этого типа НЕТ
ВООБЩЕ (сверено: `grep jobKind` по `internal/agent/*.go` — ноль совпадений
вне тестов и этого документа) — фаза 1, дойдя до реализации, не завела его,
потому что ни один вызывающий фазы 1–2 не создаёт задачу вида `loop`/`timer`
(некому было бы ветвиться). `docs/plans/2026-09-27-wake-tools-contract.md`
§7 (прочитан для этой спецификации отдельно, как и предписано заданием)
САМ фиксирует, что даже если бы `jobKind.HoldsScope()` был заведён буквально
по черновику, он был бы НЕВЕРНЫМ: одноразовые `wakein`/`wakeon` ДОЛЖНЫ
держать область, а `loop`/надзор — нет, то есть решение не по ВИДУ задачи, а
по КОНКРЕТНОМУ ЭКЗЕМПЛЯРУ (разовый таймер против периодического).

Фаза 3 не заводит `jobKind`/`HoldsScope` — заводить их сейчас означало бы
недостижимый код (ни один создающий вызов не появится раньше фазы 5), тот же
принцип, что фаза 1 уже применяла к `timeoutWakeOnly`'s доставке. Вместо
этого §8 ниже фиксирует ТОЧНОЕ место в счётчике (§2.2), которое фаза 5 обязана
тронуть, когда заведёт `wakein`/`wakeon`/`loop`/надзор — так, чтобы будущему
реализующему не пришлось заново читать весь `work_ledger.go`, чтобы найти,
где именно «держит ли задача область» должно стать per-инстанс, а не
per-вид полем.

### 0.3 Решения оператора, относящиеся к этой фазе

Из дизайн-документа («Решения оператора», п. 2–3): отсоединённой работы нет;
`loop`/надзор не держат область, разовые пробуждения — держат. Ни `loop`, ни
надзор не существуют в коде сегодня (фаза 5) — эти пункты не имеют
действующего кода, который эта спецификация меняла бы; они учтены как
ограничение на ФОРМУ счётчика (§0.2 выше), не как задача для реализации.

## 1. Находка этой фазы: «новый счётчик» из дизайн-документа уже существует

Дизайн-документ (§4 «Учёт областей») предполагает НОВЫЙ явный счётчик
незакрытого на сессию, публикующий одно событие при обнулении, которое
заменяет цикл опроса `descendantWorkPending`/BFS и четыре триггера +
тикер `subagent_outcome.go`. Трассировка кода на HEAD (это заняло основную
часть работы над этой спецификацией) показывает: у этой пары механизмов НЕТ
общей причины существования, кроме взаимного недоверия к соседним частям
системы, и одна из двух (счётчик задач в `workLedger.bySession[x].jobs`) УЖЕ
корректно реализует ровно то свойство, которое дизайн-документ просит
формализовать. Опрос, BFS и тикер — не резервная сеть с измеримой пользой, а
мёртвый вес поверх уже корректного механизма. Обоснование — по пунктам
ниже; §§3–4 переводят его в план удаления, а не переписывания.

### 1.1 Задача-делегация уже держит область родителя открытой на всю глубину дочерней работы

`workLedger.Start` (`work_ledger.go:166-202`) кладёт `*asyncJob` в
`s.jobs[toolCallID]`, где `s = l.bySession[owner]`. Для делегации
(`kind` = `agent`/`agentic_fetch`, `job.childSession != ""`) эта запись
**не удаляется** при возврате первого хода ребёнка:
`armDelegation` (`work_ledger_delegation.go:45-64`) только присваивает
`job.result` и индексирует запись в `l.byChild[childSession]` — сама запись
остаётся в `s.jobs[toolCallID]` до `deliverLocked` (`work_ledger.go:275-329`),
единственного места, которое делает `delete(s.jobs, job.toolCallID)`
(строка 318). `deliverLocked` вызывается для армированной делегации только
из `recheckChild` (`work_ledger_delegation.go:82-120`), и только когда
`childScopeDrained(childID)` (`:133-172`) истинно — то есть когда у ребёнка
НЕТ: (а) собственных незавершённых задач (`l.running(childID)`, проверяет
`len(bySession[childID].jobs) > 0`), (б) активных фоновых shell
(`l.coord.background.ActiveOwned(childID) > 0`), (в) занятого мейлбокса
(`driver.agent.IsSessionBusy(childID)`, драйвер-корректная версия, задача
#1049).

Следствие: `len(l.bySession[owner].jobs)` для родителя **остаётся больше
нуля весь период, пока у ребёнка есть хоть одна из этих трёх причин быть
занятым** — независимо от того, сколько ходов ребёнок сделал, сколько своих
async-задач завёл и снял. Это ровно рекурсивное определение «область
открыта, пока открыта дочерняя область» из ASYNC-02, реализованное не новым
счётчиком, а тем, что делегационная запись — это proxy на всю
транзитивную работу ребёнка, а не на один его ход.

### 1.2 `next()` уже блокируется на всю эту глубину — трассировка на реальном тесте

`workLedger.next()` (`work_ledger.go:481-508`) — единственный
неблокирующий-снаружи, блокирующий-внутри метод, которым `NextAsyncCompletion`
(`coordinator_background.go:93-98`) обслуживает CLI-корень. Его цикл: если
`s.ready` непусто — вернуть готовое; если `len(s.jobs) == 0` — вернуть
`(_, false, nil)` НЕМЕДЛЕННО (это единственная небл окирующая ветка); иначе
заблокироваться на `s.changed` (буферизованный на 1 канал, «пинг» через
`signalWorkSession`, вызываемый из `deliverLocked` при каждой доставке,
`work_ledger.go:326`) до следующего изменения.

Трассировка `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`
(`internal/app/app_run_subagent_root_wait_test.go`) — единственного
чёрного теста, специально написанного для этого свойства — по сценарию:
корень делегирует (`call-agent`, `Start` кладёт запись в
`bySession[root].jobs`); ребёнок стартует СВОЙ фоновый `bash`
(`call-bash`), убивает его, отвечает; родительская запись остаётся
армированной в `byChild[child]` всё это время. Только когда ребёнок
СВОИМ ходом (`child:result`) полностью освобождается — `l.running(child)`
становится `false`, мейлбокс ребёнка не занят (ход только что закончился)
— срабатывает триггер (iv) (`noteSubAgentChildRunEnded`, см. §1.4),
`recheckChild(child)` находит `childScopeDrained(child) == true`,
`deliverLocked(root, job)` удаляет запись из `bySession[root].jobs` И
зовёт `signalWorkSession(bySession[root])` — это и есть событие, которое
будит `next()`, блокировавшийся на `root`'s `changed` весь этот период.
Тест проходит СЕГОДНЯ, ДО этой фазы, и не задействует
`descendantWorkPending`/поллинг вообще: `hasCompletion` у
`app_run_async.go`'s цикла остаётся `false` только в промежутках между
итерациями `ExecuteRun`, но НЕ в момент, когда цикл реально ждёт
результат — тот момент целиком проведён ВНУТРИ блокирующего `next()`.

Вывод: для глубины 1 (единственной сегодня достижимой, §0.1) `next()`
самостоятельно, без какого-либо опроса, корректно держит корень открытым
на всю транзитивную работу ребёнка.

### 1.3 «Сбежавшие» фоновые shell — ради чего заведён `background.ActiveOwned`, недостижимы для CLI/Web origin

`sessionOwnsPendingWork`/`anyPendingWorkInMemory`
(`coordinator_work_scope.go:136-168`) СВЕРХ `l.running`/`l.hasParkedFor`
дополнительно спрашивают `background.ActiveOwned(sessionID)`. Причина
существования этой третьей проверки — трассирована: `asyncTool.Run`
(`async_tool.go:42-106`) вычисляет `sync := origin != CLI && origin != Web`
(строка 49). Для `sync == true` (SDK/unspecified origin) `t.run`
(`:156-206`) **не** форсирует `run_in_background` и **не** зовёт
`awaitShell` — если МОДЕЛЬ САМА передала `run_in_background: true` в
sync-вызове, реальный bash-инструмент (`t.inner.Run`) регистрирует фоновый
shell в `BackgroundShellManager` и возвращает «started» немедленно; `t.run`
финализирует ledger-задачу этим же текстом сразу (`finalize`/`finish`
вызываются, не дожидаясь shell) — сам shell продолжает жить, отслеживаемый
ТОЛЬКО `BackgroundShellManager`, полностью вне `workLedger`. Обнаружение
его завершения идёт ОТДЕЛЬНЫМ путём: `internal/agent/tools/bash.go:242,366`
регистрирует `sh.OnDone(func(){ onBackgroundComplete(sid, sh) })` — но
ТОЛЬКО если `!suppressBackgroundCallback(ctx)` (`bash.go:242`), а
`async_tool.go`'s `t.run` для `!sync` (CLI/Web) ветки БЕЗУСЛОВНО зовёт
`ctx = tools.WithoutBackgroundCallback(ctx)` (`:184`) ПЕРЕД тем, как
принудительно включить фон — то есть для CLI/Web-происхождения этот
callback (а значит, и `notifyBackgroundJobDone`,
`coordinator_background.go:117-178`) **никогда не вызывается**: ledger уже
владеет всей задачей через `awaitShell` (`:231-261`), которая блокирует ТУ
ЖЕ горутину, что держит `bySession[owner].jobs[toolCallID]` живой, до
завершения shell.

Origin сессии распространяется через `ctx` (`CallOriginFrom`/`WithCallOrigin`,
не через `SessionAgentCall`), и делегированный ребёнок исполняется в ctx,
производном от родительского вызова инструмента `agent` — то есть ребёнок
CLI/Web-родителя тоже видит `sync == false` для своих собственных
async-инструментов (не проверено формальным тестом — вывод из чтения
`agentTool`/`buildAgent`/`runSubAgent`'s передачи `ctx`, ни один из этих
файлов не переустанавливает `WithCallOrigin`). Комментарий у
`onBgDone`'s постройки (`coordinator_tools.go:663-671`, «rush run is
single-turn and never receives it») независимо подтверждает тот же вывод
с другой стороны кода.

Итог: путь «сбежавшего» фонового shell, ради которого
`sessionOwnsPendingWork`/`anyPendingWorkInMemory` отдельно спрашивают
`background.ActiveOwned`, **структурно недостижим для всего дерева сессий
одного `rush run`** (корень и любой его делегированный ребёнок). Он остаётся
реальным и нужным для веб/интерактивного режима с SDK/unspecified origin
(embedded-использование библиотеки) — но `descendantWorkPending`,
единственный вызывающий, вызывался ТОЛЬКО из CLI-цикла
(`app_run_async.go`, origin всегда CLI), где эта проверка ничего не может
поймать.

### 1.4 Четыре триггера `recheckChild` уже исчерпывающи для глубины 1

`work_ledger_delegation.go`'s собственный комментарий у
`subAgentOutcomeTickInterval` (`:20-28`) называет тикер «предохранительной
сетью» и перечисляет четыре обычных триггера: (i) сам `armDelegation`
(немедленная попытка, `:59-63`); (ii) завершение собственной async-задачи
ребёнка; (iii) завершение собственного фонового задания ребёнка; (iv) конец
хода на сессии ребёнка. Обнаруженная в этой фазе история багфикса
`docs/plans/2026-09-25-session-fixes-review-backlog.md` («BL-1»,
`BL-2026-09-25-1`, №1 в файле) сама фиксирует: «Event-driven triggers... still
fire in that window, so this is a safety-net gap, not a lost wakeup» — то
есть уже на 2025-09-25 авторы багфикса сочли, что тикер не защищает от
реальной потери пробуждения, только от УЗКОГО окна в СОБСТВЕННОМ
старте/остановке тикера (см. §4.1).

Трассировка каждого триггера на HEAD подтверждает вывод независимо:

- **(iv) — конец хода, универсально.** `runInternal` (`coordinator_run.go:170`)
  `defer c.noteSubAgentChildRunEnded(sessionID)` стоит БЕЗ УСЛОВИЙ в начале
  функции — срабатывает после КАЖДОГО хода на ЛЮБОЙ сессии (корень, ребёнок,
  внук — не только «ребёнок»). Отмена контекста (таймаут, `job.cancel()` из
  `handleTimeout`) не отменяет уже зарегистрированный `defer` — он выполнится
  в любом случае.
- **(ii) — завершение собственной задачи ребёнка.** `deliverLocked`'s
  маршрутизация (`job.cli && (s.drained || onWebDone == nil)`,
  `work_ledger.go:322`) для ЛЮБОЙ сессии, кроме корня, которую
  ЕДИНСТВЕННО помечает `drained` (`markDrained`, вызывается только из
  `ClaimAsyncCompletions`, единственный вызывающий которого —
  `app_run_async.go`'s `onSessionResolved` для КОРНЯ), даёт `s.drained ==
  false` — значит `queued == false`, значит `callback == true` (пока
  `onWebDone != nil`, что верно в продакшене всегда) — то есть КАЖДОЕ
  завершение задачи ребёнка идёт через `notifyAsyncCompletion`
  (`coordinator_background.go:56-83`), которая БЕЗУСЛОВНО делает `defer
  c.noteSubAgentChildRunEnded(completion.SessionID)` (строка 80).
- **(iii) — завершение фонового задания ребёнка.** По §1.3, для CLI/Web
  origin этот путь недостижим (перекрыт (ii), т.к. фоновый bash ребёнка
  ВСЕГДА идёт через `awaitShell`+`finish`, то есть заканчивается как
  обычная async-задача ребёнка, покрытая триггером (ii)). Для
  недостижимого сегодня в дереве `rush run` sync-пути триггер (iii) всё
  равно реализован (`notifyBackgroundJobDone`'s обе ветки,
  `coordinator_background.go:157,177`, обе зовут
  `noteSubAgentChildRunEnded`) — не в объёме этой фазы менять.
- **(i) — арминг.** Синхронный вызов `recheckChild` сразу после
  `l.byChild[job.childSession] = append(...)` в том же вызове
  `armDelegation` — покрывает случай, когда ребёнок УЖЕ был свободен к
  моменту арминга.

Дополнительно трассированы три места, которые МОГЛИ БЫ быть гэпом (не
названы дизайн-документом, найдены при проверке «действительно ли тикер
нужен» для этой спецификации):

- `abort` (`work_ledger.go:361-375`) роняет ещё НЕ армированную задачу
  (случай: запись tool result «начал» не удалась) — на этот момент
  `armDelegation` ещё не могло сработать (оно вызывается позже,
  `asyncTool.finalize`, после завершения хода ребёнка), значит запись ещё
  не могла попасть в `byChild` — `abort` структурно не может оставить
  делегацию «висящей» без триггера.
- `cancelSession` (`work_ledger_delegation.go:174-227`) обрабатывает ОБЕ
  стороны одним проходом: задачи, которыми ВЛАДЕЕТ `sessionID` (первый
  цикл, включая делегацию-как-владелец — доставляется напрямую тем же
  вызовом), И делегации, ПРИПАРКОВАННЫЕ НА `sessionID` как на ребёнке
  (второй цикл, `l.byChild[sessionID]`, тоже доставляется напрямую,
  безусловно, без обращения к `childScopeDrained`). Обе стороны
  форсированы — `cancelSession` не полагается на `recheckChild`/тикер
  вообще, поэтому не может создать гэп, который тикер был бы должен
  закрывать.
- `handleTimeout`'s `timeoutTerminateAndWake`-ветка (`work_ledger_timeout.go:
  142-166`) зовёт `deliverLocked` напрямую — если `job.owner` сам является
  ребёнком (обычная задача, не делегация, у которой истёк таймаут),
  доставка идёт через ТУ ЖЕ маршрутизацию, что и §1.4 (ii): `callback ==
  true` для не-drained сессии → `notifyAsyncCompletion` → триггер (ii).
  Если `job.owner` — сам корень, доставка идёт в `s.ready`, что `next()`
  видит напрямую (не нужен `recheckChild` вообще).

**Вывод, подтверждающий и уточняющий собственную оценку бэклога:** для
глубины 1 у `recheckChild` НЕТ триггера, который был бы нужен, но
отсутствует. Единственный реальный дефект во всей этой подсистеме — гонка
СОБСТВЕННОГО старта/остановки тикера (следующий параграф, §4.1), а не
потеря пробуждения где-то в «основной» логике.

### 1.5 Вывод фазы

Опрос `descendantWorkPending`/`descendantWorkPollInterval`+BFS
(`app_run_async.go`) и предохранительный тикер (`work_ledger_delegation.go`)
— не резервная сеть с измеримой пользой, а защитный код поверх уже
корректного механизма, оправданный историческим недоверием (тем самым,
которое произвело восемь дефектов в design-doc'е за три дня), а не
найденным и не закрытым классом гонки. Задача этой фазы — не построить
новый счётчик, а (а) явно, тестами, подтвердить свойство §1.1–1.4,
(б) удалить опрос, BFS и тикер как больше не несущие уникальной нагрузки,
(в) добавить единственную вещь, которой сегодня действительно не хватает —
очистку драйвера/allowlist при закрытии дочерней области (§6), которую
фаза 2 сама явно отложила («откладывается на фазу 3»,
`docs/plans/2026-09-27-async-phase2-spec.md` §2.4, §6.3).

Это меняет форму фазы 3 относительно дословного прочтения
дизайн-документа: вместо «завести счётчик, событие, заменить механики» —
«формализовать существующий счётчик как инвариант с тестами, снести
дублирующую защиту, закрыть один найденный настоящий гэп». Это явное,
осознанное решение этой спецификации, а не отклонение по недосмотру —
вынесено в «Открытые вопросы» (§12, п. 1) для подтверждения оператором,
поскольку дизайн-документ прозой предполагал бо́льший объём нового кода.

## 2. Модель области — формально, в терминах существующего кода

### 2.1 Определение

Область сессии `S` = `{S}` ∪ все сессии, когда-либо делегированные из `S`
(сегодня — глубина 1, §0.1; определение ниже написано так, чтобы не
требовать переписывания при снятии этого ограничения).

### 2.2 Счётчик

Открытость области `S` — это не новое поле, а следующее (уже вычисляемое)
логическое ИЛИ:

```
open(S) :=
    len(workLedger.bySession[S].jobs) > 0        // §1.1: включает и
                                                   // собственные задачи S,
                                                   // и армированные-но-
                                                   // недоставленные
                                                   // делегации, которыми S
                                                   // владеет как родитель
 OR background.ActiveOwned(S) > 0                 // §1.3: НЕ достижимо для
                                                   // CLI/Web-дерева; нужно
                                                   // для sync-происхождения
 OR (S сам — делегированный ребёнок) AND driver.agent.IsSessionBusy(S)
                                                   // ход S ещё идёт —
                                                   // childScopeDrained's
                                                   // третье условие
```

Ровно то же выражение уже вычисляет `childScopeDrained` (отрицание,
`work_ledger_delegation.go:151-172`) для ОДНОГО хопа «родитель ждёт
ребёнка». Формально фаза 3 не меняет это выражение — она документирует
его как ЗАКОН (реанкеровка ASYNC-02, см. конец документа) и убирает
альтернативный, менее надёжный способ получить тот же ответ
(`sessionOwnsPendingWork`+DB BFS).

**Точка расширения для фазы 5** (§0.2): когда появятся `jobKindLoop`/
`jobKindTimer`/разовые `wakein`/`wakeon`, первое слагаемое (`len(...jobs) >
0`) перестанет быть верным без изменений — часть записей в `jobs` не
должна учитываться (`loop`/надзор), часть должна (разовые пробуждения).
Точка правки: `workLedger.running`/`l.anyRunning`-подобные подсчёты
(`work_ledger.go:517-542`) и любое новое место, суммирующее
`len(s.jobs)`, должны фильтровать по `HoldsScope()`-подобному предикату
НА ЭКЗЕМПЛЯРЕ задачи (не на виде, см. §0.2) — не на уровне `jobPhase`/
`jobKind`, а на уровне поля самой `asyncJob`, которое фаза 5 заведёт.
Явно НЕ вводится этой фазой — недостижимый код (§0.2).

### 2.3 Событие «область закрыта» — где оно материализуется

У «события» нет и не заводится единого канала/типа — оно материализуется
в ДВУХ существующих местах, которые уже и есть его потребители:

1. **Для CLI-хоста (корень).** `workLedger.next()`
   (`work_ledger.go:481-508`) возвращает `(_, false, nil)`, когда
   `len(s.jobs) == 0`. По §2.2 и §1.1, для CLI/Web-дерева (где
   `background.ActiveOwned` недостижимо, §1.3) это ТОЧНО эквивалентно
   `!open(sessionID)`. Момент возврата `false` — и есть момент, когда
   `sessionID`'s область закрылась, ровно один раз (следующий вызов
   `next()` для уже пустой сессии вернёт то же самое — идемпотентно, не
   «второе событие»).
2. **Для делегации (ребёнок → родитель).** `recheckChild`
   (`work_ledger_delegation.go:82-120`)'s цикл выходит через
   `job == nil` (строка ~93-95) ТОЛЬКО когда
   `childScopeDrained(childSessionID)` истинно И нечего больше
   освобождать — это и есть момент «дочерняя область закрыта» для ЭТОГО
   конкретного ребёнка. Именно в этой точке (не раньше — до входа в цикл
   не гарантировано, что drained; не в середине — там ещё есть, что
   доставить) фаза 3 добавляет очистку драйвера/allowlist (§6).

### 2.4 Взаимодействие с таймаутами

`handleTimeout`'s `timeoutTerminateAndWake`-ветка (`work_ledger_timeout.go:
142-166`) переводит задачу в `phaseTimedOut` и зовёт `deliverLocked`
ТЕМ ЖЕ путём, что и `finish`/`recheckChild` — значит она СНИЖАЕТ
`len(s.jobs)` тем же самым образом и участвует в счётчике §2.2 без
особого случая: таймаут — это просто ещё один способ задаче стать
терминальной и быть удалённой из карты. `timeoutWakeOnly`-ветка
(`:167-190`) НЕ трогает `state`/карту вообще (задача остаётся
`phaseRunning`) — значит область НЕ закрывается этим событием, что и
требуется (нетерминальное уведомление не должно освобождать никого).

### 2.5 Взаимодействие с `armDelegation`/`recheckChild`

Без изменений относительно фазы 1–2 (§1.1); эта фаза формализует уже
существующее поведение как закон, не меняет его код (кроме добавления,
§6, в хвосте `recheckChild`).

## 3. Удаление опроса и BFS у CLI-хоста

### 3.1 Таблица удалений

| Было (файл:функция/поле) | Судьба | Почему безопасно (см. §1) |
|---|---|---|
| `app.descendantWorkPending` (`app_run_async.go:173-185`) | **Удалено** | Единственный вызывающий — сама ветка ниже, тоже удаляется |
| `descendantWorkSource` (интерфейс, `:161-171`) | **Удалено** | Не используется больше нигде (сверено: `grep descendantWorkSource` — один файл) |
| `descendantWorkPollInterval` (`:159`) | **Удалено** | Опрос убран |
| `runNonInteractiveWithAsyncResults`'s `if !hasCompletion { if app.descendantWorkPending(...) {...}; ... }` (`:100-120`) | Заменяется прямым переходом к финализации (см. §3.2) | `next()`'s `false` уже означает «нечего больше ждать», §2.3 |
| `coordinator.DescendantWorkPending` (`coordinator_work_scope.go:89-134`) | **Удалено** | Единственный вызывающий — удалённая ветка выше |
| `coordinator.anyPendingWorkInMemory` (`:139-150`) | **Удалено** | Единственный вызывающий — `DescendantWorkPending` |
| `coordinator.sessionOwnsPendingWork` (`:154-168`) | **Удалено** | Единственный вызывающий — `DescendantWorkPending`; `background.ActiveOwned`-часть недостижима для этого дерева (§1.3), `l.running`/`l.hasParkedFor`-часть дублирует §2.2, уже покрытое `next()` |

Не удаляются (используются независимо): `refreshSubAgentCompletion`,
`noteSubAgentChildRunEnded`, `parkedParentSessions`,
`ParkedSubAgentWorkReporter`/`ParkedSubAgentParents` — все в
`coordinator_work_scope.go`, остаются в файле после удаления
вышеперечисленного (файл уменьшается с 210 до ≈100 строк).

### 3.2 Новое тело ветки «нет готового результата»

```go
// internal/app/app_run_async.go, runNonInteractiveWithAsyncResults — правка
completion, hasCompletion, waitErr := source.NextAsyncCompletion(ctx, sessionID)
if waitErr != nil {
    // без изменений
}
if !hasCompletion {
    // next() возвращает false ТОЛЬКО когда у sessionID нет ни готовой
    // записи, ни незавершённой/недоставленной задачи (§2.3, §2.2) — это
    // И ЕСТЬ «область корня закрыта». Никакого отдельного опроса не нужно:
    // next() уже блокировался всё время, пока область была открыта (§1.2).
    if final == nil {
        return nil, runErr
    }
    // ... остальное тело (сборка final.Usage/Warnings/ToolCalls/вывод) —
    // БЕЗ ИЗМЕНЕНИЙ, просто больше не под условием "!descendantWorkPending"
}
```

Разница с текущим кодом — ровно удаление веток `if app.descendantWorkPending
(...) { select {...}; continue }` и всего, что их обслуживало; тело «собрать
final и вернуть» не редактируется по существу (просто перестаёт быть
условным).

### 3.3 Тесты

Существующие (не редактируются, доказывают отсутствие регресса — это и
есть приёмка):

- `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`
  (`app_run_subagent_root_wait_test.go`) — прямое доказательство §1.2,
  должен остаться зелёным БУКВАЛЬНО без изменений тела.
- `TestRunNonInteractiveWaitsForAsyncSubAgentResult`,
  `TestRunNonInteractiveContinuesPastFiveAsyncCompletions`
  (`app_run_async_completion_test.go`).
- `TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON`,
  `TestRunNonInteractiveDefaultCLIModeWaitsForAsyncCommand`,
  `TestWebAsyncCommandCompletionCreatesModelVisibleNotice`
  (то же файл) — не завязаны на делегацию, но проверяют, что обычная
  async-команда (без потомков) всё ещё корректно завершает цикл.
- `TestRunNonInteractiveChildReceivesAsyncBashResultFinishedMidTurn`,
  `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult`,
  `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn`.

Новые:

- **`TestRunNonInteractiveRootWaitsPastPollInterval`**
  (`internal/app`, новый файл или добавление в
  `app_run_subagent_root_wait_test.go`) — тот же сценарий, что
  `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`, но с явной
  проверкой ВРЕМЕНИ: держать барьер (`childJobKilled`-подобный канал)
  закрытым ЗАМЕТНО дольше, чем был `descendantWorkPollInterval` (100 мс)
  — например 500 мс — и убедиться, что `root:final` обслуживается
  практически сразу после открытия барьера (допуск — единицы
  миллисекунд, не «в пределах следующего 100-мс тика»). Это отличает
  «событие» от «опрос совпал по времени» и не проходило бы одинаково
  хорошо на старом коде (там разница была бы не видна, потому что и
  опрос, и событие уложились бы в допуск теста, если бы допуск был
  большим) — таким образом тест ценен ГЛАВНЫМ ОБРАЗОМ как документация
  свойства, а не как единственное доказательство (доказательство — сама
  трассировка §1.2, тест — её защита от будущей регрессии на
  таймингах).
  *Revert-check:* временно поставить `time.Sleep(150 * time.Millisecond)`
  перед `admissionWriteSSE` в `"root:final"`-ветке теста — тест должен
  ловить это как отклонение от «сразу», если допуск подобран туго; при
  ослабленном допуске revert-check не нужен, что и есть слабое место
  этого теста, честно отмеченное здесь, а не скрытое.
- **`TestWorkLedger_ChildScopeClosesWithoutTicker`**
  (`work_ledger_delegation_test.go`) — с УЖЕ удалённым тикером (эта фаза
  сама его удаляет, см. §4.3) прогнать точный сценарий
  `TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain` (арминг → ребёнок
  занят → `finish` детской задачи → доставка родителю) и убедиться, что
  доставка происходит СИНХРОННО в рамках вызывающих триггеров
  (без `time.Sleep`, без ожидания тика) — то есть этот тест ПРОСТО
  переиспользует существующий сценарий, но теперь он ЯВЛЯЕТСЯ
  доказательством «тикер не нужен», а не только «сценарий работает».
  *Revert-check:* не применим отдельно — это тот же тест, что уже
  существует; ценность в том, что он остаётся зелёным ПОСЛЕ удаления
  тикера без единой правки сценария.

## 4. Удаление предохранительного тикера — закрытие #1019/BL-1

### 4.1 Механизм гонки (для протокола, дословно из бэклога)

`docs/plans/2026-09-25-session-fixes-review-backlog.md` п. 1
(`BL-2026-09-25-1`, ссылается на неё же checkpoint
`docs/checkpoints/2026-09-28-0029.md:23` под коротким именем «#1019 BL-1»):
тик-горутина проверяет `hasParked()` в ОДНОЙ критической секции, затем в
ОТДЕЛЬНОЙ секции читает `tickStop`, обнуляет его и закрывает канал вне
блокировки. `armDelegation`, попавший в это окно (после проверки
`hasParked()==false`, до обнуления `tickStop`), вызывает
`startTickerLocked()`, которая видит СТАРЫЙ (ещё не `nil`) `tickStop` и
возвращается, не запуская НОВУЮ горутину — после чего тик-горутина
закрывает старый канал, оставляя реестр с армированной записью, но БЕЗ
работающего тикера. Верифицировано на HEAD: `work_ledger_delegation.go`'s
`tick` (`:338-362`) и `startTickerLocked` (`:312-336`) переняли этот код
почти дословно (фаза 1's таблица замен прямо это фиксирует: «переносятся
почти без изменений... **сохраняются в фазе 1**») — гонка физически
присутствует в коде сегодня.

### 4.2 Почему устранение — удаление, а не починка

Бэклог сам классифицирует находку как P2 («safety-net gap, not a lost
wakeup») именно потому, что «обычные» триггеры покрывают каждый
достижимый путь — §1.4 этой спецификации подтверждает это независимой
трассировкой на текущем коде (не том, что был на 2025-09-25) и явно
перечисляет все не-названные дизайн-документом кандидаты в гэп
(`abort`/`cancelSession`/`handleTimeout`), закрывая каждый как
структурно безопасный без тикера. Значит починка «сделать
старт/остановку тикера одной критической секцией» устранила бы САМУ
гонку, но не изменила бы того факта, что весь МЕХАНИЗМ, который эта
гонка защищала, не нужен — а design-doc прямо предписывает удаление
(«Четыре триггера и тикер `subagent_outcome.go` исчезают»). Починка
без удаления оставила бы код, найденный дефектным дважды за три дня, в
третий раз.

### 4.3 Таблица удалений

| Было (файл:функция/поле) | Судьба |
|---|---|
| `subAgentOutcomeTickInterval` (`work_ledger_delegation.go:28`) | **Удалено** |
| `subAgentOutcomeCancelledText` (`:33`) | Остаётся (используется `cancelSession`, не связано с тикером) |
| `startTickerLocked` (`:312-336`) | **Удалено** |
| `tick` (`:338-362`) | **Удалено** |
| `workLedger.tickStop` (поле, `work_ledger.go:100`) | **Удалено** |
| `l.startTickerLocked()`-вызов в `armDelegation` (`work_ledger_delegation.go:59`) | **Удалено** (арминг продолжает синхронно звать `recheckChild`, триггер (i), без изменений) |
| `stop := l.tickStop; ...; close(stop)` в `workLedger.close` (`work_ledger.go:554-559`) | **Удалено** (нечего останавливать) |

Файл `work_ledger_delegation.go` уменьшается с 362 до ≈300 строк;
`work_ledger.go` — с 564 до ≈555 (минус поле и 6 строк в `close`).

### 4.4 Тесты

Существующие, которые ОБЯЗАНЫ остаться зелёными БЕЗ изменения сценария
(доказательство, что удаление триггера-безопасности не изменило исход):
`TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending`,
`TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain`,
`TestWorkLedger_FailedChildJobDeliveredOnceAsFailure`,
`TestWorkLedger_ConcurrentRecheckDeliversOnce`,
`TestWorkLedger_CancelReleasesArmedDelegation`,
`TestWorkLedger_CancelSurvivesFinishedChildTurn`,
`TestWorkLedger_ResumeAfterNoticeDoesNotReemit`,
`TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds`,
`TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce`
(все — `work_ledger_delegation_test.go`; ни один не манипулирует
`subAgentOutcomeTickInterval`/`tickStop` напрямую — сверено, см. §1.4's
вывод grep — значит удаление тикера не требует правки НИ ОДНОГО из них).

Новый (закрывает саму гонку явно, а не только «функционал остался
зелёным»):

- **`TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation`** —
  заармировать делегацию, дать ей освободиться обычным путём, снять
  снимок `runtime.NumGoroutine()` до и после (с толерантным допуском на
  шум рантайма) — убедиться, что armDelegation НЕ порождает никакой
  дополнительной долгоживущей горутины (тикер, если бы остался, был бы
  ровно такой горутиной). *Revert-check:* вернуть `startTickerLocked`'s
  вызов в `armDelegation` — тест обязан показать рост
  `NumGoroutine()` на 1, устойчиво не убывающий до `close()`.

## 5. Доставка делегации управляется событием «область ребёнка закрыта»

Формализация, не изменение кода (кроме §6): §2.3, п. 2 — `recheckChild`'s
`job == nil`-выход ПОСЛЕ подтверждённого `childScopeDrained` — это и есть
именованное в задании «событие», на котором «завершается делегация».
Ничего не рефакторится в самом `recheckChild`/`childScopeDrained` — они
уже реализуют это ровно так, как просит design doc п. 4, просто без
отдельного типа-события. Единственная правка этой фазы к самой функции —
хвост, добавленный в §6.2 (очистка драйвера).

## 6. Очистка драйвера и allowlist при закрытии дочерней области

### 6.1 Диагноз — рост без очистки, отложенный фазой 2 явно

`subAgentDriverRegistry` (`coordinator_subagent_drivers.go:58-75`) хранит
запись НА ВЕСЬ срок жизни координатора («kept for the coordinator's
lifetime rather than removed once the child goes idle», доккомент строки
58-71) — по конструкции у него нет метода удаления вообще (сверено: `grep
unregister\|delete(r.byChild` по `internal/agent/*.go`, вне тестов — ноль
совпадений). Отдельно, `permission.go`'s `SessionRunAllowlistManager`
(`internal/permission/permission.go:161-199`) хранит per-сессионную запись
allowlist (`runAllowlistBySession`, `:334`), заведённую
`InheritSessionRunAllowlist` (вызывается из `runSubAgent`,
`coordinator_subagents.go:117`, и `wakeSession`'s `wakeNoticeCall`,
`coordinator_wake.go:143-144`) — фаза 2 явно убрала парный `defer
ClearSessionRunAllowlist` в ОБОИХ местах, где он раньше стоял
(`coordinator_subagents.go:118-126`, `async_tool.go:158-163`), с
комментарием «entry lives as long as the driver» и явным флагом «§6.3:
same unbounded-until-phase-3 growth». Это — прямое указание фазы 2, что
подчистка НАЗНАЧЕНА фазе 3, а не забыта.

### 6.2 Решение: compare-and-delete по generation, привязанное к событию §5

```go
// coordinator_subagent_drivers.go, правка subAgentDriver

type subAgentDriver struct {
    agent           SessionAgent
    call            SessionAgentCall
    parentSessionID string
    // generation — монотонный номер регистрации ЭТОЙ записи (не всей
    // задачи агента), назначаемый register. Тот же приём, что уже
    // защищает SetSessionRunAllowlistForEpoch/ClearSessionRunAllowlistForEpoch
    // (permission.go, R2-1) от того, что отложенная очистка одного вызова
    // удалит политику, армированную более новым: без него
    // recheckChild, решивший «область закрыта» на СТАРОМ снимке, мог бы
    // удалить драйвер/allowlist, которые resume_session_id уже успел
    // перерегистрировать заново долей секунды раньше.
    generation uint64
}

type subAgentDriverRegistry struct {
    mu      sync.Mutex
    byChild map[string]subAgentDriver
    nextGen uint64
}

// register (правка тела, сигнатура не меняется — вызывающему generation
// не нужен, releaseIfCurrent сам вычитывает актуальный через get)
func (r *subAgentDriverRegistry) register(childSessionID string, driver subAgentDriver) {
    if r == nil || childSessionID == "" {
        return
    }
    r.mu.Lock()
    r.nextGen++
    driver.generation = r.nextGen
    r.byChild[childSessionID] = driver
    r.mu.Unlock()
}

// releaseIfCurrent удаляет запись childSessionID ТОЛЬКО если она всё ещё
// на generation g — тот же compare-and-delete идиом, что
// ClearSessionRunAllowlistForEpoch уже применяет к другой карте.
func (r *subAgentDriverRegistry) releaseIfCurrent(childSessionID string, g uint64) bool {
    if r == nil || childSessionID == "" {
        return false
    }
    r.mu.Lock()
    defer r.mu.Unlock()
    if current, ok := r.byChild[childSessionID]; ok && current.generation == g {
        delete(r.byChild, childSessionID)
        return true
    }
    return false
}
```

```go
// coordinator_subagent_drivers.go, новая функция

// releaseDriverIfScopeClosed удаляет childSessionID's драйвер и его
// restricted-run allowlist-базу, вызывается ИЗ recheckChild (§5) в момент,
// когда область ребёнка подтверждённо закрыта. Безопасно против гонки с
// параллельным resume_session_id: releaseIfCurrent сравнивает по
// generation ПОД СВОИМ мьютексом с АКТУАЛЬНЫМ значением карты, а не со
// снимком, взятым здесь раньше — новая регистрация всегда выигрывает.
func (c *coordinator) releaseDriverIfScopeClosed(childSessionID string) {
    driver, ok := c.subAgentDrivers.get(childSessionID)
    if !ok {
        return
    }
    if !c.subAgentDrivers.releaseIfCurrent(childSessionID, driver.generation) {
        return // конкурентный resume_session_id уже перерегистрировал — не трогаем
    }
    if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
        mgr.ClearSessionRunAllowlist(childSessionID)
    }
}
```

```go
// work_ledger_delegation.go, правка recheckChild — хвост цикла
func (l *workLedger) recheckChild(childSessionID string) {
    for {
        if !l.childScopeDrained(childSessionID) {
            return
        }
        l.mu.Lock()
        job := oldestArmedLocked(l.byChild[childSessionID])
        if job == nil {
            l.mu.Unlock()
            if l.coord != nil {
                l.coord.releaseDriverIfScopeClosed(childSessionID)
            }
            return
        }
        // ... остальное тело без изменений
    }
}
```

Вызывается КАЖДЫЙ раз, когда `recheckChild` подтверждает «нечего больше
освобождать» — включая случай, когда у `childSessionID` НИКОГДА не было
армированной делегации вообще (`byChild[childSessionID]` пуст с самого
начала: `oldestArmedLocked` вернёт `nil` немедленно) — то есть эта же
функция вызывается и из триггера (iv) (`noteSubAgentChildRunEnded`,
универсальный для КАЖДОГО хода) для сессий, которые вообще не являются
делегированными детьми — `c.subAgentDrivers.get(childSessionID)` для НЕ
зарегистрированной сессии просто вернёт `ok=false`, и функция сразу
выйдет, без побочных эффектов (проверено по коду `get`,
`coordinator_subagent_drivers.go:93-102`).

Следующий `resume_session_id`-вызов на ЭТОТ ЖЕ `childSessionID`
(`runSubAgent`, `coordinator_subagents.go:259`, вызывается КАЖДЫЙ раз —
и на создании, и на резюме) заново регистрирует драйвер (новый
`generation`) И заново зовёт `InheritSessionRunAllowlist`
(`:117`, тоже безусловно на каждый вызов) — то есть удаление здесь не
«теряет» состояние на будущее, оно просто перестаёт держать его, когда
оно фактически не нужно, и оно ВСЕГДА восстанавливается заново перед
тем, как снова понадобится.

### 6.3 Остаточная гонка allowlist-очистки — явный риск, не решённый полностью

`releaseIfCurrent`'s compare-and-delete атомарно защищает КАРТУ ДРАЙВЕРОВ.
Вызов `mgr.ClearSessionRunAllowlist(childSessionID)` СРАЗУ ПОСЛЕ него —
уже НЕ под тем же мьютексом (`permissionService`'s собственный
`runAllowlistBySessionMu`, `permission.go:335`, отдельная блокировка).
Между успешным `releaseIfCurrent` и вызовом `ClearSessionRunAllowlist`
есть узкое окно, в которое МОГ БЫ гипотетически успеть: новый
`resume_session_id`-вызов на тот же `childSessionID` (перерегистрирует
драйвер — это ГОНКУ НЕ создаёт, драйвер уже новый, но `runSubAgent`'s
СОБСТВЕННЫЙ `InheritSessionRunAllowlist` (`:117`) идёт РАНЬШЕ, чем
координатор мог бы вызвать `releaseDriverIfScopeClosed` для СТАРОГО
снимка — то есть порядок «новый inherit» → «старый clear» ВОЗМОЖЕН, если
`recheckChild`'s вызов этой фазы был отложен (например, вызван из
горутины `notifyAsyncCompletion`, которая всегда асинхронна,
`coordinator_background.go:71-82`) настолько, что успел зайти ПОСЛЕ
нового резюме.

`permission.go` уже решает РОВНО ЭТУ форму гонки для ДРУГИХ вызывающих —
`SetSessionRunAllowlistForEpoch`/`ClearSessionRunAllowlistForEpoch`
(`:166-178`) и их call-scoped пара (`:180-198`) существуют именно потому,
что простая пара `Set`/`ClearSessionRunAllowlist` не защищена от
«отложенная чистка одного владельца удаляет свежую политику другого»
(R2-1/R3-4, доккомменты тех же строк). Полное закрытие этого окна
означало бы завести ТРЕТЬЮ epoch-подобную пару специально для
делегационного inherit/clear (или провести существующий
`InheritSessionRunAllowlist`/`ClearSessionRunAllowlist` через
generation этого же драйвера как epoch) — это трогает
`permission.go`'s интерфейс `SessionRunAllowlistManager` и три
вызывающих (`runSubAgent`, `asyncTool.run`, `wakeNoticeCall`), что шире,
чем «учёт областей» этой фазы, и не запрошено заданием напрямую.

**Решение этой фазы**: принять остаточное окно как ЯВНЫЙ, задокументированный
риск (не тихо забытый — см. §11 «Риски» и §12 «Открытые вопросы»), а не
закрывать его epoch-рефакторингом permission-подсистемы. Основание для
приемлемости: окно требует, чтобы В ТУ ЖЕ МИКРОСЕКУНДУ, когда область
ребёнка закрылась (никакой активной работы), пришёл НОВЫЙ
`resume_session_id`-вызов НА ТОТ ЖЕ РОДИТЕЛЬСКИЙ session id (владение
`resume_session_id` уже проверяется — «is not a child of the current
session» отказывает чужим) — то есть родитель должен ОДНОВРЕМЕННО решить
«всё, ребёнок свободен» (что и закрывает область) И «резюмирую его снова»
— эти два события инициируются ОДНИМ И ТЕМ ЖЕ родительским мейлбоксом,
который сериализует свои собственные ходы (не может делать оба
ОДНОВРЕМЕННО) — единственный РЕАЛЬНЫЙ путь к гонке — это если срабатывание
`recheckChild`, закрывающее область, идёт из АСИНХРОННОЙ горутины
(`notifyAsyncCompletion`, не из хода родителя), которая физически может
обгонять/отставать от родительского хода. Оценка вероятности —
не проверено количественно (не запрошено заданием); отмечено как открытый
вопрос, не решено эмпирически.

### 6.4 Тесты

- **`TestReleaseDriverIfScopeClosed_RemovesDriverAndAllowlist`** —
  зарегистрировать драйвер, установить allowlist на ребёнке
  (`InheritSessionRunAllowlist`), вызвать `releaseDriverIfScopeClosed`
  напрямую; проверить, что `subAgentDrivers.get(childID)` теперь
  `ok=false`, и allowlist-шпион зафиксировал ровно один вызов `Clear`.
  *Revert-check:* убрать вызов `releaseDriverIfScopeClosed` из
  `recheckChild`'s хвоста — тест, написанный как ЧЁРНЫЙ (наблюдающий
  `subAgentDrivers.get` ПОСЛЕ полного цикла делегации через
  `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`-подобный
  сценарий, а не только юнит на `releaseDriverIfScopeClosed` напрямую),
  обязан показать, что драйвер остаётся зарегистрированным навсегда.
- **`TestSubAgentDriverRegistry_ReleaseIfCurrentIsGenerationGuarded`** —
  зарегистрировать драйвер (generation g1), ЗАНОВО зарегистрировать ТОТ
  ЖЕ childID (generation g2), вызвать `releaseIfCurrent(childID, g1)`
  (устаревший снимок) — убедиться, что запись НЕ удалена (возврат
  `false`), запись с `g2` остаётся читаемой через `get`. *Revert-check:*
  заменить compare-and-delete на безусловный `delete` — тест обязан
  показать удаление свежей (g2) записи по устаревшему g1.
- **`TestRunSubAgent_ResumeAfterScopeClosedReArmsAllowlist`** — довести
  делегацию до полного закрытия области (allowlist и драйвер удалены,
  §6.2), затем вызвать `agent(resume_session_id=<тот же child>)` ещё раз
  через ПОЛНЫЙ путь `runSubAgent`; убедиться, что allowlist на ребёнке
  снова установлен ДО того, как ход ребёнка успевает сделать первый
  вызов инструмента (то есть очистка на закрытии области не странит
  будущее резюме без политики). *Revert-check:* не применим отдельно —
  это прямая проверка утверждения §6.2 «resume всегда переустанавливает
  заново»; если инвариант неверен, тест ловит его напрямую.

## 7. Кросс-процессный вид: `sessions why`/`sessions list`

Не меняется этой фазой. `internal/session/descendant_liveness.go`'s
`LiveDescendants` (используется `internal/cmd/sessions_why.go:207`,
`internal/cmd/sessions_list.go`, `internal/server/handlers_sessions.go:256`)
остаётся ЕДИНСТВЕННЫМ кросс-процессным источником: он отвечает на вопрос
«жив ли лок-файл потомка» (эвристика PID/mtime), а НЕ «какая конкретно
задача открыта» — этот разрыв (ASYNC-10, «частично») design doc явно
относит к фазе 4 (устойчивый реестр в SQLite, читаемый ЛЮБЫМ процессом).
`workLedger`/`subAgentDriverRegistry` — состояние ОДНОГО процесса; вторая
`rush sessions why` из ДРУГОГО процесса не имеет к нему доступа ни до, ни
после этой фазы.

Единственное, что эта фаза МЕНЯЕТ для кросс-процессного вида —
КОСВЕННО: удаление `DescendantWorkPending` (§3.1) убирает единственный
код, который делал ОДИН DB-запрос (`ListSubSessions`) НА КАЖДУЮ итерацию
опроса ROOT-цикла (каждые 100 мс, пока область открыта) — эта нагрузка
на БД исчезает вместе с опросом, что уменьшает конкуренцию за ту же БД,
которую использует `sessions why`/`sessions list` из других процессов
(не измерено количественно — качественное следствие, не заявленная
цель этой фазы).

`ParkedSubAgentWorkReporter`/`ParkedSubAgentParents`
(`coordinator_work_scope.go:191-210`) — ВНУТРИПРОЦЕССНЫЙ сигнал
(используется ли он веб-сервером в ТОМ ЖЕ процессе — не проверено
исчерпывающе, не входило в список файлов для чтения этой фазы) — не
трогается, продолжает читать `workLedger.byChild` напрямую, не зависит
ни от чего удаляемого в §3.

## 8. Совместимость с надзором (`wake-tools-contract.md` §7)

Надзор (раз в 5 минут будить корень при тишине в чате, design doc §7,
«Решения оператора» п. 5) — первый потребитель сервиса сроков ПОСЛЕ этой
фазы (design doc: «его можно сделать сразу после фазы 3»). Эта фаза не
реализует надзор (нет `jobKindTimer`/сервиса сроков для расписаний —
только для явных per-call таймаутов, фаза 2) — но обязана не построить
модель области так, чтобы надзор оказался структурно несовместим с ней.
Проверено:

- Надзор — задача вида `timer`, `HoldsScope() == false` (design doc,
  «Решения оператора» п. 3; contract §7 уточняет: по ЭКЗЕМПЛЯРУ, не по
  виду, §0.2 этого документа). Счётчик §2.2 суммирует `len(s.jobs)` без
  разбора по видам СЕГОДНЯ, потому что сегодня НЕТ задач, для которых это
  было бы неверно (только `command`/`agent`/`agentic_fetch`, все
  `HoldsScope() == true` по построению). Когда фаза 5 заведёт надзорную
  запись, ей нужно будет ЛИБО не попадать в `bySession[root].jobs` вообще
  (архитектурно чище — надзор не «задача владельца» в том же смысле, что
  async-инструмент), ЛИБО попадать, но исключаться из подсчёта явным
  полем. Эта спецификация НЕ выбирает между этими двумя вариантами за
  фазу 5 (продуктовое решение фазы 5, не входит в объём фазы 3) — но
  фиксирует, что `workLedger.running`/счётчик §2.2 — единственное место,
  которое такой фильтр должен тронуть, что и есть цель contract §7's
  предупреждения.
- Разовые пробуждения (`wakein`/`wakeon`) ДОЛЖНЫ держать область
  (design doc, «Решения оператора» п. 3) — если фаза 5 реализует их как
  `jobKindTimer`-записи В `bySession[owner].jobs` (естественный выбор,
  раз счётчик §2.2 уже суммирует именно эту карту), они автоматически
  будут учтены БЕЗ дополнительного кода — совместимость по построению,
  не требует правки этой фазы.
- Отмена надзора «автоматически, когда у корня не осталось другой
  открытой работы» (design doc, «Решения оператора» п. 3) — раз надзор
  `HoldsScope()==false`, `open(root)` (§2.2) станет `false`, ЕСЛИ надзор
  корректно исключён из подсчёта, ДАЖЕ КОГДА надзорная запись формально
  ещё существует (не удалена) — то есть `next()` вернёт `false`
  (§2.3, п. 1) и CLI завершится, оставляя надзорную запись «висящей» в
  памяти процесса, который уже завершается — не проблема для `rush run`
  (процесс просто выходит), но требует, чтобы фаза 5 сама остановила
  сервис сроков при завершении процесса (уже делает — `workLedger.close`
  зовёт `l.timeouts.close()`, `work_ledger.go:560`, актуально и для
  будущего общего сервиса расписаний, если он окажется тем же объектом).

Никакого кода эта фаза не пишет для §8 — раздел существует, чтобы
зафиксировать: модель §2.2 совместима с contract §7's требованием
per-инстанс `HoldsScope`, и указать ТОЧНОЕ место правки для фазы 5.

## 9. Порядок реализации

Каждый шаг — рабочая сборка; тесты перечислены кумулятивно (тесты
предыдущих шагов остаются зелёными).

**Шаг 0 — доказательство свойства без изменения кода.** Добавить
`TestRunNonInteractiveRootWaitsPastPollInterval` (§3.3) и
`TestWorkLedger_ChildScopeClosesWithoutTicker` (§3.3, второй) К
СУЩЕСТВУЮЩЕМУ коду (тикер и опрос ещё НЕ удалены) — оба обязаны пройти
УЖЕ СЕЙЧАС, что и есть эмпирическое подтверждение §1 ПЕРЕД тем, как
что-либо удалять. Если `TestWorkLedger_ChildScopeClosesWithoutTicker`
не проходит без изменений (тикер ещё стоит, но тест не должен на него
полагаться) — это сигнал, что §1.4's трассировка имеет дыру, и удаление
тикера в шаге 2 нужно остановить до пересмотра диагноза.

**Шаг 1 — удаление CLI-опроса и BFS (§3).** `app_run_async.go` (§3.1,
§3.2), `coordinator_work_scope.go` (`DescendantWorkPending`/
`anyPendingWorkInMemory`/`sessionOwnsPendingWork` удалены). Тесты: весь
список §3.3 (существующие + шаг 0) зелёные без исключений.

**Шаг 2 — удаление тикера (§4).** `work_ledger_delegation.go`/
`work_ledger.go` (§4.3). Тесты: список §4.4 (существующие + новая
`TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation`) зелёные;
шаг 0's тесты по-прежнему зелёные (теперь уже осмысленно, а не «тикер
случайно успел сработать вовремя»).

**Шаг 3 — очистка драйвера/allowlist (§6), отдельный коммит.**
`coordinator_subagent_drivers.go` (`generation`, `releaseIfCurrent`,
`releaseDriverIfScopeClosed`), `work_ledger_delegation.go`'s
`recheckChild`'s хвост. Тесты: три теста §6.4;
`TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob` и весь список
§3.3/§4.4 — по-прежнему зелёные (эта правка добавляет побочный эффект в
УЖЕ существующей точке выхода `recheckChild`, не меняет маршрутизацию
доставки).

**Шаг 4 — реанкеровка `docs/async-invariants.md`.** Обязательна тем же
коммитом, что шаг 1 (наиболее существенное изменение для ASYNC-02) —
см. таблицу в конце документа.

**Шаг 5 (опционально).** Обновить `docs/plans/2026-09-27-wake-tools-
contract.md`'s §9 «Карта по фазам», если ревью найдёт это полезным —
не обязательный шаг фазы 3 (сама эта фаза ничего в контракте не
реализует, только резервирует место, §8).

Статус-строка фазы 3 в `docs/plans/2026-09-27-async-structured-
concurrency.md` НЕ обновляется этим документом (прецедент — фаза 2's
спецификационный коммит `70a26227` тоже не трогал design doc; обновление
статуса — дело коммита РЕАЛИЗАЦИИ, не спецификации).

## 10. Файловый план

| Файл | Изменение | Строк до → после |
|---|---|---|
| `internal/app/app_run_async.go` | Удаление опроса/BFS-интерфейса (§3.1–3.2) | 197 → ≈120 |
| `internal/agent/coordinator_work_scope.go` | Удаление `DescendantWorkPending`/`anyPendingWorkInMemory`/`sessionOwnsPendingWork` (§3.1) | 210 → ≈100 |
| `internal/agent/work_ledger_delegation.go` | Удаление тикера (§4.3); хвост `recheckChild` (§6.2) | 362 → ≈300 |
| `internal/agent/work_ledger.go` | Удаление поля `tickStop` и его остановки (§4.3) | 564 → ≈555 |
| `internal/agent/coordinator_subagent_drivers.go` | `generation`, `releaseIfCurrent`, `releaseDriverIfScopeClosed` (§6.2) | 117 → ≈150 |
| `internal/app/app_run_subagent_root_wait_test.go` или новый файл | Новый тест §3.3 | — |
| `internal/agent/work_ledger_delegation_test.go` | Новые тесты §3.3, §4.4 | — |
| `internal/agent/coordinator_subagent_drivers_test.go` | Новые тесты §6.4 | — |

Ни один файл не приближается к пределу 1000 строк; правки везде — либо
чистое удаление, либо небольшая добавка. `.githooks/file_size_allowlist.txt`
не трогается.

## 11. Риски

- **Основной риск этой спецификации — сама переоценка объёма.** §1
  заключает, что задуманный design doc'ом новый счётчик уже существует, и
  предлагает УДАЛЕНИЕ вместо построения. Если это заключение ошибочно
  (гэп в трассировке §1.4, не найденный этой спецификацией), удаление
  тикера/опроса воскресит именно те восемь дефектов, ради которых
  дизайн-документ и написан. Единственная защита — тесты шага 0 (§9),
  которые ОБЯЗАНЫ пройти на СУЩЕСТВУЮЩЕМ коде до удаления чего-либо;
  если хоть один из них НЕ проходит без тикера/опроса уже сегодня —
  реализация ДОЛЖНА остановиться и пересмотреть §1, а не продолжать
  удаление.
- **Остаточная гонка allowlist-очистки** (§6.3) — не закрыта полностью,
  задокументирована явно, а не тихо принята. Требует либо operator's
  явного согласия на остаточный риск, либо отдельной (не в этой фазе)
  epoch-подобной переработки `InheritSessionRunAllowlist`/
  `ClearSessionRunAllowlist`.
- **Зависимость от инварианта «CLI/Web origin ⇒ sync=false ⇒ никаких
  сбежавших фоновых shell»** (§1.3) — верифицирована чтением кода
  (`async_tool.go`, `bash.go`, `coordinator_tools.go`), НЕ верифицирована
  отдельным чёрным тестом, который бы ЛОМАЛСЯ, если бы кто-то в будущем
  случайно сделал `sync` вычисляемым иначе. Не входит в объём этой
  фазы добавлять такой тест (он защищал бы фазу 2's инвариант, не
  фазу 3's), но реализующий должен знать: если это когда-нибудь
  изменится, §3's удаление `sessionOwnsPendingWork` придётся
  пересмотреть.
- **`TestRunNonInteractiveRootWaitsPastPollInterval`'s допуск по
  времени** — сформулирован в §3.3 как «единицы миллисекунд», что на
  медленной/загруженной машине (CI, `-race`) может потребовать более
  мягкого порога; конкретное число — деталь реализации, не
  зафиксирована здесь как жёсткая константа.

## 12. Открытые вопросы оператору

Только продуктовые/архитектурные решения — технические уже приняты и
обоснованы выше.

1. **Форма фазы — удаление вместо построения (§1.5).** Design doc прозой
   предполагает написание нового механизма («заводится счётчик...
   публикуется событие»); эта спецификация вместо этого удаляет опрос,
   BFS и тикер, аргументируя, что нужное свойство уже реализовано
   существующим кодом (§1.1–1.4). Подтвердить, что такая форма
   приемлема как реализация design doc'а «по духу», а не по букве —
   или потребовать явного счётчика/события как отдельного типа даже
   при доказанной функциональной избыточности (например, ради
   будущей наблюдаемости — `sessions why` могла бы захотеть показать
   «область: N задач, M дочерних», для чего готовый счётчик был бы
   удобнее, чем пересчитывать `len(s.jobs)` по запросу; сегодня это не
   запрошено).
2. **Остаточная гонка allowlist-очистки (§6.3).** Принять как
   документированный риск (вероятность оценивается как очень низкая, но
   не измерена) — или заказать epoch-переработку
   `SessionRunAllowlistManager`'s inherit/clear пары отдельной задачей
   ДО того, как фаза 3 сливается, поскольку это трогает
   `permission.go`'s публичный интерфейс и три вызывающих места
   (`runSubAgent`, `asyncTool.run`, `wakeNoticeCall`).
3. **`ParkedSubAgentWorkReporter`'s фактическое использование в
   веб-сервере** — не проверено исчерпывающе этой спецификацией (вне
   списка файлов), но потенциально релевантно для §7 (кросс-процессный
   вид). Если веб-сервер использует его для отображения статуса «в
   процессе», стоит подтвердить, что фаза 3 (удаляющая ТОЛЬКО
   `DescendantWorkPending`, не эту функцию) не меняет то, что видит
   пользователь веб-UI — по анализу не должна, но заявлено с оговоркой,
   а не как факт.

## Реанкеровка `docs/async-invariants.md`

Обязательна тем же коммитом, что шаг 1 (§9):

| ID | Что меняется |
|---|---|
| ASYNC-02 | Формулировка закона не меняется; «Где сегодня» переписывается ЦЕЛИКОМ: `internal/app/app_run_async.go`'s цикл дренирования БЕЗ `descendantWorkPending`/BFS (§3.2); `workLedger.next()` (`work_ledger.go:481-508`) — единственный держатель корня, эквивалентность обоснована §1.1–1.3/§2.2-2.3; `coordinator_work_scope.go`'s `DescendantWorkPending`/`anyPendingWorkInMemory`/`sessionOwnsPendingWork` — **удалены**, ссылка снимается. «Транзитивность глубже одного уровня» — по-прежнему не проверена чёрным тестом (то же ограничение, что фаза 0 нашла и не сняла, #1050 делает её недостижимой, а не непроверенной по недосмотру), но теперь ОБОСНОВАНА по коду (§1.1: рекурсивное выражение написано так, что не зависит от глубины) с явной оговоркой «не проверено тестом на глубину > 1». Статус: «Выполняется на проверенных путях» → уточнить: «выполняется; ранее отдельный BFS-механизм удалён как доказанно избыточный (эта фаза), не заменён на другой обходной механизм». |
| ASYNC-03 | Добавить пятую строку к таблице конкурентов CAS: `cancelSession`'s собственная безусловная доставка (`work_ledger_delegation.go:174-227`) — не новый конкурент (не менялась этой фазой), но её роль в закрытии §1.4's гипотетических гэпов стоит явно отметить как часть доказательства «тикер не нужен» (перекрёстная ссылка на этот документ §1.4). |
| ASYNC-09 | Без изменения статуса; `refreshSubAgentCompletion`'s Debug (не тронут) остаётся тем же нарушением, что и в фазе 2's реанкеровке. |
| ASYNC-10 | «Внутрипроцессный путь» переписывается: `coordinator.ParkedSubAgentParents`/`DescendantWorkPending` → только `coordinator.ParkedSubAgentParents` (`DescendantWorkPending` удалена этой фазой, §3.1). Кросс-процессный путь (`descendant_liveness.go`) не меняется. Статус не меняется («частично» — фаза 4 по-прежнему нужна для «какая конкретно задача», не «какая сессия»). |

Строки, которые эта фаза НЕ трогает: ASYNC-01, ASYNC-04, ASYNC-05,
ASYNC-06, ASYNC-07, ASYNC-08 — ни один их механизм не редактируется этой
спецификацией.

## Решения оркестратора по открытым вопросам (2026-09-28)

1. **Форма фазы принята: удаление опроса, BFS и тикера — да, но только
   вместе с триггером «сессия стала свободной» по построению.** Вывод
   §1.4 опирается на то, что после КАЖДОГО `Run` любой сессии кто-то зовёт
   `recheckChild`. Сегодня это обеспечивают разные вызывающие по
   отдельности (`runInternal`'s defer, `notifyAsyncCompletion`'s defer,
   `armDelegation`), а ход ребёнка идёт через `params.Agent.Run`/
   `wakeSession` → `agent.Run` драйвера, мимо `runInternal`. Любой будущий
   путь `Run` без такого вызова тихо повесит делегацию, а тикера уже не
   будет. Поэтому триггер (iv) переносится в одну точку: мейлбокс
   `sessionAgent` сообщает координатору о переходе сессии в свободное
   состояние (там, где владение реально освобождается —
   `releaseSessionReservation`/`drainOrReleaseMerged`'s ветка освобождения),
   и координатор зовёт `recheckChild` для этой сессии. Существующие
   точечные вызовы можно оставить (идемпотентны) или убрать — на
   усмотрение реализующего. Тикер удаляется только после этого шага, с
   тестом: ход ребёнка через драйвер, завершившийся мимо `runInternal`,
   доставляет делегацию без тикера.
   Отдельный тип счётчика не заводится. Для надзора (#1043) и веб-панели
   (#1058) добавляется только чтение: снимок живой работы области
   (задачи и делегации по сессии и потомкам) из реестра по запросу.
2. **Остаточная гонка allowlist-очистки (§6.3) не принимается.** Закрыть
   по построению: наследование и очистка allowlist дочерней сессии
   получают эпоху — `generation` драйвера (по образцу существующих
   `SetSessionRunAllowlistForEpoch`/`ClearSessionRunAllowlistForEpoch` в
   `permission.go`); очистка по старой эпохе не трогает запись новой.
   Правка интерфейса `SessionRunAllowlistManager` и трёх вызывающих
   (`runSubAgent`, `asyncTool.run`, `wakeNoticeCall`) входит в фазу 3.
3. **`ParkedSubAgentWorkReporter`** — не вопрос оператору: реализующий
   проверяет его потребителей (`grep` по `internal/server`, `internal/app`,
   `internal/cmd`) и фиксирует результат в документе; видимое в веб-UI не
   должно измениться.

## Итог реализации (2026-09-28, worktree `async-phase3`)

Реализовано в порядке §9 с одной перестановкой (по решению оркестратора
п. 1 — добавление триггера ДО удаления тикера) и одной находкой сверх
списка §3.1:

1. **Шаг 0** (тесты до удаления) — выполнен. `TestWorkLedger_
   ChildScopeClosesWithoutTicker` (`work_ledger_delegation_test.go`) —
   как описано. Вместо app-уровневого `TestRunNonInteractiveRootWaitsPast
   PollInterval` (реальный OS-процесс + HTTP round-trip, шумный таймер на
   разделяемой машине) — детерминированный `TestWorkLedger_
   NextUnblocksOnSignalNotPoll` (`internal/agent`, новый файл): держит
   задачу «в работе» 500 мс, затем завершает её и проверяет, что `next()`
   разблокируется в пределах 50 мс от самого `finish()`, а не по границе
   опроса. Это тест ТОЧНО того примитива, на который опирался app-уровневый
   опрос, без риска флуктуаций реального таймера ОС — сознательное
   отклонение от буквы §3.3, отмеченное как таковое.
2. **Шаг 1** (§3, опрос+BFS) — выполнен как описано. Найден и закрыт гэп в
   собственной трассировке §3.1: таблица удалений называла `app_run_async.go`
   единственным вызывающим `app.descendantWorkPending`, но `internal/app/
   app_run.go:540` (reviewer-pass gate в `ExecuteRun`) вызывал его ВТОРЫМ,
   независимым путём. Заменено на уже читаемый на той же строке
   `HasPendingAsyncJobs(sess.ID)` — по тому же рассуждению §1.1: армированная
   делегация уже держит запись в `bySession[sess.ID].jobs`, отдельный обход
   не нужен и на глубине 1 избыточен.
3. **Шаг 2** (по решению оркестратора п. 1, ПЕРЕД удалением тикера) —
   `sessionAgent.onSessionIdle` (новое поле, `agent.go`), вызывается из
   `abandonOwnershipWithHandoff` (`agent_ownership.go`) — единственной
   функции, через которую проходит каждый выход из `Run()`/
   `RunWithReservedOwnership`/`ReleaseExclusive`, независимо от вызывающего
   (`coordinator.runInternal` ИЛИ `wakeSession`'s прямой `agent.Run` на
   драйвере). Подключено в `buildAgent` (`coordinator_tools.go`) —
   одинаково для корня и каждого делегированного драйвера. Существующие
   точечные вызовы (`runInternal`'s defer, `notifyAsyncCompletion`'s defer)
   оставлены как идемпотентные. Тест: `TestRecheckChild_
   FiresOnRealDriverRunEndWithoutManualTrigger` (`internal/agent`, новый
   файл) — РЕАЛЬНЫЙ `*sessionAgent`-драйвер (не мок), ход которого
   завершается мимо `runInternal`; тест не делает ни одного ручного вызова
   `recheckChild`/`noteSubAgentChildRunEnded` сам.
4. **Шаг 3** (§4, тикер) — выполнен как описано, объединён с шагом 2 в
   ОДИН коммит (а не два раздельных): промежуточный коммит «добавлен
   триггер, тикер ещё жив» дал бы новому тесту шага 2 более слабую гарантию
   (тикер с интервалом по умолчанию мог бы САМ успеть доставить в пределах
   5-секундного окна теста), что либо ослабляло бы тест, либо требовало
   того же самого отключения тикера, которое шаг 3 всё равно делает —
   решено не создавать намеренно более слабую промежуточную версию теста.
   Побочно найдено и закрыто: `workLedger.anyRunning()` осиротела уже
   шагом 1 (её единственный вызывающий, `anyPendingWorkInMemory`, удалён
   там же) — удалена этой же правкой, не отдельным шагом.
5. **Шаг 4** (§6.2, драйвер/allowlist) — выполнен как описано:
   `subAgentDriverRegistry.generation`/`releaseIfCurrent`, `coordinator.
   releaseDriverIfScopeClosed`, `recheckChild`'s хвост.
6. **§6.3/оркестратор п. 2** (allowlist epoch) — выполнен, ОТКЛОНЕНИЕ от
   буквы §6.2: спецификация утверждала «вызывающему generation не нужен,
   releaseIfCurrent сам вычитывает актуальный через get» — это верно для
   УДАЛЕНИЯ драйвера, но не для НОВОГО требования оркестратора (генерация
   должна также стать эпохой allowlist-записи), которое появилось ПОСЛЕ
   этого предложения спецификации и делает его неверным: `runSubAgent`/
   `wakeNoticeCall` должны знать generation В МОМЕНТ вызова `Inherit...`, а
   не постфактум. `register` теперь ВОЗВРАЩАЕТ generation (как `mailbox.
   submit` возвращает `epoch`) вместо `void`; вызывающие используют
   возврат. `runSubAgent`'s вызов `InheritSessionRunAllowlist` перенесён с
   позиции ДО регистрации драйвера НА позицию ПОСЛЕ (между ними в
   исходном коде нет ничего, что читало бы allowlist — только
   `SessionSetup`, который лишь вызывает `AutoApproveSession`, — проверено
   чтением). `asyncTool.Run`'s более ранний вызов (до появления драйвера
   при первой делегации) передаёт `generation=0` (инертное, никогда не
   совпадающее с реальной generation значение, тот же приём, что `ownerEpoch
   0` уже использует) — эта запись всё равно перезаписывается через
   мгновение вызовом `runSubAgent`'s собственным, generation-корректным
   `Inherit...`.
7. **Оркестратор п. 3** (`ParkedSubAgentWorkReporter`) — проверено:
   единственный потребитель во всём дереве — `internal/cmd/sessions_list.go:
   94-95` (`rush sessions list`). `internal/server`, `internal/app` его НЕ
   читают вовсе (`grep` по всем трём деревьям, ноль совпадений вне
   `internal/cmd`) — веб-UI не видит эту сигнализацию ни до, ни после этой
   фазы, значит изменения фазы 3 (которые эту функцию не трогают) не могут
   повлиять на веб-видимое поведение через этот путь.

Не реализовано и не запрошено этой фазой: `jobKind`/`HoldsScope` (§0.2,
явно отложено фазе 5); транзитивная проверка на глубину > 2 (заблокирована
находкой фазы 0, не входит в объём).
