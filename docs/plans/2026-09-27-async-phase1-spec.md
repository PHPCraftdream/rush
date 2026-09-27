# Фаза 1: реестр работы в памяти — спецификация реализации

Статус: спецификация к утверждённому дизайну
(`docs/plans/2026-09-27-async-structured-concurrency.md`), реализация не
начата. Инварианты — `docs/async-invariants.md` (anchored в worktree
`async-phase0`, тот же HEAD, что и этот документ).

Область: **только фаза 1** — «Одна машина состояний; `asyncJobRegistry` и
паркинг сливаются; исполнители сообщают только «готово». Класс BL-1/BL-2
исчезает вместе с `finishParked`.» Всё остальное (единая доставка и драйвер,
учёт областей событием, долговечность, надзор) — фазы 2–5, не в этом
документе, кроме одного явно оговорённого исключения (§0.2, тип `deadline`).

Каждая строка ниже сверена с кодом на HEAD этого worktree; где сверить не
удалось — написано «не проверено», а не предположение.

## 0. Что учтено дополнительно к дизайн-документу

### 0.1 Решения оператора от 2026-09-27 (после утверждения дизайна)

Помимо решений, уже записанных в дизайн-документе («Решения оператора»,
пп. 1–5), в ходе подготовки этой спецификации получены четыре новых решения
и одна независимая задача, которые спецификация обязана отразить в типах
фазы 1, даже если исполнение большинства из них — дело более поздних фаз:

1. Отсоединённой работы нет — уже было в дизайне (п. 2 «Решений оператора»),
   подтверждено повторно. Фаза 1 ничем не рискует здесь: `Job` всегда имеет
   `owner` и живёт в карте владельца до доставки — по конструкции нет пути
   создать работу без владельца (см. §1).
2. `loop` и надзор **не удерживают область**: снимаются автоматически, когда
   корень обычным образом завершил работу и другой открытой работы нет.
   Разовые пробуждения (`wakein`/`wakeon` одноразовые) область удерживают.
   → §1.2 (`jobKind`, метод `HoldsScope`).
3. Пропущенные во время простоя Rush дедлайны **не разыгрываются задним
   числом** при восстановлении. → §5 (что фаза 1 не делает) и заметка при
   поле `deadline` в §1.1: это решение для фазы 4 (восстановление после
   смерти хоста), фаза 1 без персистентности этого сценария не имеет вовсе,
   но тип не должен потребовать пересмотра, когда фаза 4 придёт.
4. Таймаут — только явный, по каждому вызову, без значений по умолчанию, и
   ДВУХ видов, выбираемых в вызове:
   - «только разбудить» (`timeoutWakeOnly`): задача продолжает работать,
     владельцу приходит событие «время вышло, работа продолжается» со
     сводкой; решение — за владельцем;
   - «остановить и разбудить» (`timeoutTerminateAndWake`): задача переходит
     в `timed_out`, частичный вывод сохраняется, владелец получает событие.
   → §1.1 (поля `deadline`/`timeoutKind`), §1.3 (таблица переходов),
   §5 (граница реализации: тип и CAS — в фазе 1; сервис сроков,
   инициирующий переход, — в фазе 5, неявный-но-неотправленный вызов
   `wakeOnly`-уведомления — тоже позже, обоснование ниже).
5. Вложенная делегация (внук через `agent`/`agentic_fetch` внутри
   `agent`/`agentic_fetch`) не поддерживается и делается структурно
   невозможной отдельной задачей **#1050**. Глубина делегации — 1. Это
   снимает необходимость рекурсивной модели у реестра и у драйвера
   (§1.4): у `childSession` не может быть СВОЕГО `childSession`, поэтому
   индекс `byChild` и carта драйверов — плоские, без обхода дерева. Заодно
   это ровно то же ограничение, которое `docs/async-invariants.md`
   зафиксировал как «Находка этой фазы» (зависание `readyWg` при разрешённой
   вложенной делегации) — фаза 1 не обязана защищаться от этого случая
   отдельно: он устраняется на уровне конфигурации задачей #1050, а не этим
   реестром.

### 0.2 Задача #1049 («драйвер») — независимая параллельная работа

Другой агент параллельно чинит: делегированная дочерняя сессия исполняется
на ОТДЕЛЬНОМ `*sessionAgent` (task-агент, построенный `coordinator.buildAgent`
внутри `agentTool()`/`agenticFetchTool()` — `internal/agent/agent_tool.go:44`,
`internal/agent/coordinator_tools.go:27`), а не на `c.currentAgent`
координатора; но пробуждение (`notifyAsyncCompletion`) сегодня безусловно
идёт через `c.Run` → `runInternal` → `c.currentAgent.Run`
(`internal/agent/coordinator.go:433`, `internal/agent/coordinator_run.go:352`)
— то есть через ЧУЖОЙ `*sessionAgent`. Тем же путём страдает busy-гейт:
`subAgentWorkTerminal` (`internal/agent/subagent_outcome.go:684`) спрашивает
`c.currentAgent.IsSessionBusy(childSessionID)`, а мейлбокс `childSessionID`
живёт в мейлбоксах task-агента, а не корневого — поэтому проверка ВСЕГДА
возвращает `false`, что уже задокументировано как «Побочная находка» в
`docs/async-invariants.md` (раздел после таблицы инвариантов). Фикс
регистрирует пер-дочернюю «пару-драйвер» (тот же `SessionAgent` + шаблон
вызова), которым `notifyAsyncCompletion` и `subAgentWorkTerminal` должны
пользоваться вместо `c.currentAgent`.

Этот код сегодня не существует в дереве, проверить его точную форму нельзя
(не проверено). Фаза 1 не может ни блокироваться на нём, ни угадывать его
внутренности. Решение — **точка расширения**, а не встраивание чужой
реализации:

```go
// subAgentDriver называет, КАКОЙ SessionAgent реально держит мейлбокс
// childSessionID, и как собрать для него следующий ход с той же моделью/
// инструментами/учётными данными, что и первый ход делегации. Заполняется
// задачей #1049 (ожидаемо — рядом с runSubAgent,
// internal/agent/coordinator_subagents.go:50); реестр фазы 1 этот файл НЕ
// трогает и не предполагает, что регистрация уже есть.
type subAgentDriver interface {
    Agent() SessionAgent
    BuildWakeCall(prompt string) SessionAgentCall
}
```

`workLedger` не хранит карту драйверов сам — он принимает единственную
функцию-сеанс при постройке координатором:

```go
driverFor func(childSessionID string) (subAgentDriver, bool)
```

и два новых вызывающих метода координатора, которые заменяют прямые
`c.Run(...)`/`c.currentAgent.IsSessionBusy(...)` в местах пробуждения и
busy-гейта:

```go
func (c *coordinator) wakeOwner(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
    if driver, ok := c.driverFor(sessionID); ok {
        return driver.Agent().Run(ctx, driver.BuildWakeCall(prompt))
    }
    return c.Run(ctx, sessionID, prompt) // не проверено-безопасный откат: сегодняшнее (дефектное для делегаций) поведение, byte-for-byte
}

func (c *coordinator) sessionBusy(sessionID string) bool {
    if driver, ok := c.driverFor(sessionID); ok {
        return driver.Agent().IsSessionBusy(sessionID)
    }
    return c.currentAgent.IsSessionBusy(sessionID)
}
```

Если `c.driverFor` не задан (`nil`) или пока не регистрирует делегации (т.е.
фаза 1 доехала до продакшена раньше #1049), поведение — **дословно
сегодняшнее**, включая дефект: фаза 1 ни чинит, ни ломает #1049, только
готовит место для его починки без переписывания `wakeOwner`/`sessionBusy`
заново. Глубина 1 (§0.1.5) означает, что `driverFor` — плоская функция без
обхода дерева: делегат уровня 1 не может сам иметь делегата.

**Требование по слиянию (риск, не техническое решение):** файл
`work_ledger_driver.go` (только он) должен быть первым, самостоятельным
коммитом фазы 1, до остальных файлов реестра, чтобы #1049 мог перебазироваться
на готовый `driverFor`/`wakeOwner`/`sessionBusy`, а не столкнуться с
одновременной правкой `notifyAsyncCompletion`/`subAgentWorkTerminal` в двух
ветках одновременно. Явно: фаза 1 **не трогает**
`internal/agent/coordinator_subagents.go` вообще — это гарантия того, что
#1049 может делать там что угодно без конфликта с этой спецификацией.

## 1. Единый реестр: типы и API

Пакет — `internal/agent`, без подпакета (см. §6). Новые файлы заменяют
`async_job_registry.go` (353 строки) и `subagent_outcome.go` (740 строк)
целиком — оба удаляются в фазе 1 (см. §2).

### 1.1 Типы

```go
// work_job.go

// jobKind — какой исполнитель стоит за задачей. jobKindLoop/jobKindTimer
// зарезервированы (см. HoldsScope ниже) и не создаются фазой 1 — их вводит
// фаза 5 вместе с сервисом сроков и разовыми/периодическими пробуждениями.
type jobKind uint8

const (
    jobKindCommand jobKind = iota // bash, run_command — childSession всегда ""
    jobKindAgent                   // инструмент `agent`
    jobKindFetch                    // инструмент `agentic_fetch`
    // jobKindLoop и jobKindTimer добавляются фазой 5. Значения здесь не
    // резервируются заранее (iota сдвинется, когда фаза 5 допишет свои
    // константы) — тип не экспортируется наружу пакета, так что это
    // безопасная эволюция, а не breaking change.
)

// HoldsScope сообщает, держит ли задача этого вида область владельца
// открытой (решение оператора 2026-09-27, §0.1.2). true для всех трёх видов,
// которые создаёт фаза 1; jobKindLoop/jobKindTimer (фаза 5) вернут false —
// точка ветвления существует с фазы 1, чтобы её не пришлось искать по всему
// коду в фазе 5.
func (k jobKind) HoldsScope() bool {
    switch k {
    case jobKindCommand, jobKindAgent, jobKindFetch:
        return true
    default:
        return false // недостижимо до фазы 5
    }
}

// jobPhase — положение задачи в машине состояний ASYNC-03: ровно один
// терминальный исход, достигнутый не более одного раза, затем не более
// одной доставки. "Доставлено" НЕ отдельное значение jobPhase (см. §1.3) —
// это представлено удалением задачи из sessionJobs.jobs, как и сегодня в
// releaseLocked (async_job_registry.go:216-230): отдельное bool-поле
// дублировало бы состояние карты и могло бы разойтись с ней.
type jobPhase int32

const (
    phaseRunning jobPhase = iota
    phaseCompleted
    phaseFailed
    phaseCancelled
    phaseTimedOut    // фаза 1: значение и CAS есть, ни один вызывающий фазы 1 его не порождает (см. §1.3, §5)
    phaseInterrupted // фаза 1: значение и CAS есть, ни один вызывающий фазы 1 его не порождает (см. §1.3, §5) — #1040/host-lease это фаза 4
)

func (p jobPhase) terminal() bool { return p != phaseRunning }

// jobResult — итог задачи, без владельца/kind/toolName (те уже есть в самой
// задаче и не дублируются). Собирается в AsyncCompletion только на границе
// доставки (deliverLocked, §1.6) — в отличие от сегодняшнего
// asyncJobState.completion *AsyncCompletion, где поля SessionID/ToolCallID/
// ToolName дублируют то, что уже есть в ключе карты и в самой задаче.
type jobResult struct {
    content string
    isError bool
}

// timeoutKind — вид явного таймаута (решение оператора 2026-09-27, §0.1.4).
// Значение по умолчанию timeoutNone: "нет таймаутов по умолчанию" в самом
// типе, а не в отдельной проверке у каждого вызывающего.
type timeoutKind uint8

const (
    timeoutNone timeoutKind = iota
    timeoutWakeOnly             // дедлайн истёк -> НЕТЕРМИНАЛЬНОЕ уведомление, задача продолжает работать (см. §5 — механизм доставки этого уведомления фаза 1 не реализует)
    timeoutTerminateAndWake     // дедлайн истёк -> phaseTimedOut (терминально), частичный вывод сохраняется, доставка как у любого терминального исхода
)

// TimeoutSpec — необязательный явный таймаут вызова. nil = сегодняшнее
// поведение (таймаутов на уровне задачи нет; общий 45-минутный watchdog
// хода — отдельный, не тронутый фазой 1 механизм, agent_turn_stream.go /
// toolMaxDuration). НИ ОДИН вызывающий фазы 1 не строит непустой
// TimeoutSpec: сегодня ни один инструмент (`bash`, `run_command`, `agent`,
// `agentic_fetch`) не принимает параметр таймаута от модели — это заведёт
// сама задача плана пробуждений, которая добавит JSON-поле в параметры
// инструмента. Здесь только место для значения, чтобы Start не пришлось
// потом менять сигнатуру повторно.
type TimeoutSpec struct {
    Deadline time.Time
    Kind     timeoutKind
}

// asyncJob — одна запись реестра. Замещает asyncJobState
// (async_job_registry.go:33-38) и subAgentOutcomeEntry
// (subagent_outcome.go:55-77) единым типом: у делегации (kind agent/fetch)
// сегодня было ДВЕ записи в двух реестрах на весь жизненный цикл одного
// вызова; здесь — одна, с момента Start до момента доставки.
type asyncJob struct {
    owner      string // сессия, которой уходит уведомление
    toolCallID string // ключ вместе с owner; стабилен, задаёт идемпотентность (см. §1.4)
    kind       jobKind
    toolName   string // "bash"/"run_command"/"agent"/"agentic_fetch" — для AsyncCompletion.ToolName и kindForTool
    childSession string // непусто только для kind agent/fetch; см. §0.1.5 — не может иметь СВОЙ childSession
    cli        bool   // origin, дословно сегодняшний asyncJobState.cli — маршрут доставки от него не меняется в фазе 1 (см. §1.6)

    state      jobPhase
    announced  bool // ack-гейт: "начал" tool result сохранён (onToolResult)
    cancel     context.CancelFunc // executor-контекст этого job'а; см. §1.5 про его реальный охват для делегаций
    result     jobResult          // валиден когда state.terminal(); для armed-но-ещё-running делегации — захваченный на первом ходу результат (см. §1.4)

    deadline    time.Time   // zero = нет дедлайна; см. TimeoutSpec
    timeoutKind timeoutKind // хранится с фазы 1 (§0.1.4), не читается ничем в фазе 1 (см. §5)
}
```

Все поля `asyncJob`, кроме идентифицирующих (`owner`, `toolCallID`, `kind`,
`toolName`, `childSession`, `cli`), меняются исключительно под мьютексом
`workLedger.mu` — как и сегодняшний `asyncJobState`, отдельного мьютекса на
задачу нет (см. §1.7 про то, почему не введён).

### 1.2 Реестр

```go
// work_ledger.go

// sessionJobs — замещает asyncJobSession (async_job_registry.go:40-48)
// дословно, только jobs теперь map[string]*asyncJob.
type sessionJobs struct {
    jobs    map[string]*asyncJob
    ready   []AsyncCompletion
    changed chan struct{}
    drained bool
}

// workLedger — единственный владелец состояния работы в памяти. Замещает
// asyncJobRegistry (async_job_registry.go:50-65) и хранилище
// subAgentOutcomeRegistry (subagent_outcome.go:82-89) одним типом.
type workLedger struct {
    mu        sync.Mutex
    bySession map[string]*sessionJobs        // владелец -> его задачи (было asyncJobRegistry.sessions)
    byChild   map[string][]*asyncJob          // childSession -> задачи делегаций, ЗАПАРКОВАННЫЕ на нём (было subAgentOutcomeRegistry.byChild); все записи здесь по конструкции ещё не доставлены — доставленная запись удаляется отсюда в том же деле, что и из bySession (см. §1.4 gc)
    onWebDone func(AsyncCompletion)
    driverFor func(sessionID string) (subAgentDriver, bool) // см. §0.2; может быть nil

    coord *coordinator // для childScopeDrained (см. §1.4): background.ActiveOwned + sessionBusy — оба вне реестра. Тот же паттерн, что subAgentOutcomeRegistry.coord сегодня (subagent_outcome.go:83)

    tickStop chan struct{} // защитный тикер — СОХРАНЯЕТСЯ в фазе 1, см. §1.4 и §5
    closed   bool
}
```

### 1.3 Таблица переходов

| Из | Событие | В | Кто вызывает | Реализовано в фазе 1? |
|---|---|---|---|---|
| (нет записи) | `Start(owner, id, …)`, новый ключ | `phaseRunning` | `asyncTool.Run` | да |
| (нет записи) | `Start(owner, id, …)`, СУЩЕСТВУЮЩИЙ ключ | `phaseRunning` (без изменений; возвращается существующая задача) | `asyncTool.Run` | да — закрывает #1038 (§1.4) |
| `phaseRunning` | `Finish(success)` | `phaseCompleted` | `asyncTool.run` (kind=command) | да |
| `phaseRunning` | `Finish(error/panic)` | `phaseFailed` | `asyncTool.run` (kind=command) | да |
| `phaseRunning` | `ArmDelegation(...)` | `phaseRunning` (без изменения состояния; захватывается `result`, индексируется `byChild`) | `asyncTool.finalize` (kind=agent/fetch) | да |
| `phaseRunning`, armed-делегация | `RecheckChild` находит область осушённой, обновлённый результат — успех | `phaseCompleted` | внутренний вызов реестра, триггеры (i)–(iv) (§1.4) | да |
| `phaseRunning`, armed-делегация | `RecheckChild`, обновлённый результат — ошибка | `phaseFailed` | то же | да |
| `phaseRunning` | `CancelSession(owner)` (обход задач владельца) | `phaseCancelled` | `Cancel`/`CancelAll` | да |
| `phaseRunning`, делегация в `byChild[X]` | `CancelSession(X)` (обход со стороны ребёнка) | `phaseCancelled` | `Cancel`/`CancelAll` | да |
| `phaseRunning` | `AbortUnannounced` | запись удаляется, `state` не устанавливается | `onToolResult`, ветка ошибки записи tool result | да, форма не меняется (см. §1.5) |
| `phaseRunning` | `FireTimeout` (только `timeoutTerminateAndWake`) | `phaseTimedOut` | будущий сервис сроков (фаза 5) | тип/CAS есть, вызывающего в проде нет — покрыт тестом (см. §4) |
| `phaseRunning`, `timeoutWakeOnly` | дедлайн истёк | `phaseRunning` (без изменений; нетерминальное уведомление) | будущий сервис сроков (фаза 5) | НЕ реализовано (см. §5) |
| `phaseRunning` | смерть хоста (будущий lease) | `phaseInterrupted` | будущее восстановление (фаза 4) | тип/CAS есть, вызывающего нет |
| любое терминальное | любое из вышеперечисленных снова | без изменений (проигравший CAS) | — | да — это и есть гарантия ASYNC-03 |
| терминальное ∧ `announced` | `deliverLocked` | запись удаляется из `bySession[owner].jobs` (= «доставлено») | вызывается изнутри той функции, что либо ставит `announced=true` (если уже терминально), либо совершает терминальный переход (если уже `announced`) | да |

### 1.4 API

```go
// Start регистрирует задачу для (owner, toolCallID). Идемпотентно по этому
// ключу (закрывает #1038 для делегаций, design-doc §1): если запись уже
// есть, existing=true и возвращается СУЩЕСТВУЮЩАЯ задача — вызывающий не
// должен повторно запускать исполнителя (см. §2, правку asyncTool.Run).
// err — только для валидации (пустой owner/id), закрытого реестра и
// maxAsyncJobsPerSession; НЕ для дублирующегося ключа — это больше не
// ошибка.
func (l *workLedger) Start(owner, toolCallID, toolName, childSession string, cli bool, timeout *TimeoutSpec, cancel context.CancelFunc) (job *asyncJob, existing bool, err error)

// Announce — ack-гейт: помечает, что "начал" tool result сохранён
// (agent_turn_stream.go's onToolResult, ПОСЛЕ успешного messages.Create).
// Дословно сегодняшний acknowledged (async_job_registry.go:129-147), имя не
// меняется — вызывающий (onToolResult) не трогается.
func (l *workLedger) acknowledged(sessionID, toolCallID string)

// AbortUnannounced роняет запись и отменяет её executor-контекст. Вызывается
// ТОЛЬКО когда запись tool result "начал" НЕ удалась — то есть строго до
// того, как Announce вообще мог быть вызван для этого id, так что сохранять
// нечего (ASYNC-05 здесь нечего защищать). Дословно сегодняшний abort
// (async_job_registry.go:241-255), имя не меняется.
func (l *workLedger) abort(sessionID, toolCallID string)

// Finish — терминальный переход для ПРОСТЫХ задач (kind=command). Для
// kind=agent/fetch НЕ вызывается — см. ArmDelegation.
func (l *workLedger) finish(owner, toolCallID string, result jobResult)

// ArmDelegation захватывает результат ПЕРВОГО хода делегации (kind agent/
// fetch) и делает её кандидатом на переоценку готовности — замещает
// subAgentOutcomeRegistry.park (subagent_outcome.go:101-116). Состояние
// задачи остаётся phaseRunning: возврат первого хода — конец ОДНОГО хода
// модели, а не работы ребёнка (см. файловый комментарий subagent_outcome.go
// целиком, переносится дословно как комментарий к этому методу).
// Синхронно, в том же вызове, пробует немедленное освобождение (триггер
// (i) — было отдельным вызовом tryRelease после park; здесь один вызов).
func (l *workLedger) armDelegation(owner, toolCallID string, captured jobResult)

// RecheckChild переоценивает армированные, ещё не терминальные задачи
// byChild[childSessionID] в порядке FIFO и завершает каждую, чья область
// осушена childScopeDrained. Единая точка для триггеров (ii)-(iv); триггер
// (i) теперь внутри armDelegation (см. выше). Заменяет
// subAgentOutcomeRegistry.tryRelease/nextReleasable (subagent_outcome.go:
// 121-148).
func (l *workLedger) recheckChild(childSessionID string)

// childScopeDrained — готовность к освобождению делегации: не осталось
// НИ ОДНОЙ незавершённой задачи, которой владеет childSessionID (проверка
// внутри реестра — тривиальна теперь, что и раньше и являлось
// asyncJobRegistry.running), НИ ОДНОГО активного фонового шелла
// (l.coord.background.ActiveOwned, вне реестра), И мейлбокс не занят
// (l.coord.sessionBusy — драйвер-совместимая версия IsSessionBusy, §0.2).
// Замещает coordinator.subAgentWorkTerminal (subagent_outcome.go:674-688).
func (l *workLedger) childScopeDrained(childSessionID string) bool

// CancelSession принудительно завершает КАЖДУЮ задачу, которой владеет
// sessionID (как cancelled), И каждую делегацию, запаркованную НА НЕМ как
// на ребёнке (byChild[sessionID]) — оба направления, одним проходом под
// одним мьютексом. Замещает asyncJobRegistry.cancelSession
// (async_job_registry.go:324-340) + coordinator.
// releaseSubAgentOutcomesForParentCancel/ChildCancel
// (subagent_outcome.go:437-476) вместе. Делегация, отменённая здесь,
// НИКОГДА не читает refreshSubAgentCompletion — результат сразу
// {isError:true, content: subAgentOutcomeCancelledText} через тот же
// transitionToTerminal, что и у обычных задач (регрессия:
// TestSubAgentOutcome_CancelSurvivesFinishedChildTurn, портируется без
// изменений, см. §4).
func (l *workLedger) cancelSession(sessionID string)

// Close отменяет все задачи во всех сессиях (как cancelSession для каждой)
// и запрещает дальнейший Start. Замещает asyncJobRegistry.close
// (async_job_registry.go:342-353) + coordinator.
// releaseAllSubAgentOutcomesCanceled (subagent_outcome.go:734-740) +
// subAgentOutcomeRegistry.close (subagent_outcome.go:562-578).
func (l *workLedger) close()

// FireTimeout — терминальный переход для timeoutTerminateAndWake. В фазе 1
// не имеет вызывающего в продакшн-коде (сервис сроков — фаза 5, см. §0.1.4,
// §5); реализован и покрыт тестом (§4), чтобы CAS был доказан ДО того, как
// появится первый настоящий вызывающий.
func (l *workLedger) fireTimeout(owner, toolCallID string, partial jobResult)

// Не реализовано в фазе 1 (см. §5): FireWakeOnlyNotice — нетерминальное
// "время вышло, работа продолжается" уведомление для timeoutWakeOnly.
```

Методы `next`/`pending`/`running`/`anyRunning`/`markDrained`/`hasParked`/
`hasParkedFor`/`parkedParentSessions` переносятся **без изменения имени и
сигнатуры** с `*asyncJobRegistry`/`*subAgentOutcomeRegistry` на `*workLedger`
— их тела читают `bySession`/`byChild` вместо `sessions`/`byChild`
соответственно, логика не меняется. Это те методы, ради которых
`async_tool_test.go` не нужно переписывать (см. §4).

### 1.5 CAS для терминального состояния и для доставки

```go
// transitionToTerminal — ЕДИНСТВЕННЫЙ писатель state дальше phaseRunning.
// Вызывается под l.mu из finish/cancelSession/close/fireTimeout. Возвращает
// false (no-op), если задача уже терминальна — это и есть CAS: чей бы
// вызов ни захватил мьютекс первым, тот и выигрывает; все последующие для
// ТОЙ ЖЕ задачи — no-op. Закрывает BL-2/#1032 (docs/async-invariants.md,
// строка ASYNC-03): сегодняшний cancelSession подменяет всю карту s.jobs
// целиком (async_job_registry.go:331-332) ДО того, как конкурентный finish
// успевает найти свою запись — здесь обе стороны меняют ОДНО И ТО ЖЕ поле
// одного и того же *asyncJob под одним и тем же мьютексом, так что "карта
// подменена из-под конкурента" структурно невозможно.
func (j *asyncJob) transitionToTerminal(state jobPhase, result jobResult) bool {
    if j.state != phaseRunning {
        return false
    }
    j.state = state
    j.result = result
    return true
}
```

"CAS" здесь — не lock-free-атомик, а мьютекс-защищённое
проверить-и-установить в одной критической секции (как и сегодня фактически
делают `finish`/`abort`/`cancelSession`, просто без общей точки входа).
Явный `sync/atomic` не нужен: реестр и сегодня один мьютекс на всё, вводить
второй уровень блокировок — новый риск взаимоблокировки без measured выгоды.

Вторая CAS — доставка:

```go
// deliverLocked — вторая CAS: удаляет задачу из bySession[owner].jobs, ЕСЛИ
// state.terminal() && announced. Присутствие в карте — это "ещё не
// доставлено" (как и сегодня releaseLocked, async_job_registry.go:216-230);
// отдельное bool-поле "delivered" не заводится (см. §1.1) — оно продублировало
// бы карту без пользы. Маршрутизация (готовая очередь vs onWebDone) — см.
// §1.6, не меняется в фазе 1.
func (l *workLedger) deliverLocked(owner string, j *asyncJob) (AsyncCompletion, bool /* callback */)
```

### 1.6 Доставка: маршрут не меняется в фазе 1

`queuesLocked`'s условие (`job.cli && (s.drained || onWebDone == nil)`,
`async_job_registry.go:125-127`) переносится **побайтово**: `deliverLocked`
собирает `AsyncCompletion{SessionID: owner, ToolCallID: j.toolCallID,
ToolName: j.toolName, Content: j.result.content, IsError: j.result.isError,
cli: j.cli}` и применяет то же самое условие для выбора между
`s.ready = append(s.ready, completion)` и возвратом `(completion, true)` для
вызова `onWebDone`. Это прямое требование задания (п. 1: «как маршрутизация
доставки... остаётся поведенчески идентичной в фазе 1») и явный пункт §5
(«что фаза 1 не меняет»).

### 1.7 Идемпотентный старт (детали)

`Start` для повторного `(owner, toolCallID)`:

- НЕ вызывает исполнителя повторно (защита от двойного побочного эффекта —
  например, повторного `rm -rf` при повторе одного и того же tool call
  провайдером);
- возвращает `existing=true` и указатель на СУЩЕСТВУЮЩУЮ `*asyncJob` —
  вызывающий (`asyncTool.Run`) строит "started"-ответ из ЕЁ полей
  (`job.childSession`), а не из заново вычисленных на этом вызове (хотя они
  и детерминированы — `sessions.CreateAgentToolSessionID(messageID,
  call.ID)`, `internal/agent/async_tool.go:94` — совпадение не проверялось
  формально, доверие реестру строже);
- `err` при этом остаётся `nil` — единственные реальные ошибки: пустые
  `owner`/`toolCallID`, `maxAsyncJobsPerSession` (не проверено под другим
  углом — это лимит НА СЕССИЮ, не на ключ, поэтому не пересекается с
  идемпотентностью) и закрытый реестр.

Точный механизм бага **#1038** (что именно порождает повторный `Start` с тем
же ключом на практике) в прочитанных документах не описан — не проверено;
дизайн-документ формулирует только целевое поведение («повторный старт того
же вызова возвращает существующую задачу»), которое воспроизведено выше
дословно.

## 2. Таблица замен

| Было (файл:функция/поле) | Стало | Кто вызывающий должен измениться |
|---|---|---|
| `asyncJobRegistry` (тип, `async_job_registry.go:50`) | `workLedger` (`work_ledger.go`) | `coordinator.asyncJobs` — тип поля |
| `newAsyncJobRegistry` (`:67`) | `newWorkLedger` | `coordinator.go` (место постройки координатора — не проверено точное имя функции/строка постройки `c.asyncJobs`, нужно найти при реализации) |
| `asyncJobState` (тип, `:33-38`) | `asyncJob` (`work_job.go`) | внутренне |
| `asyncJobSession` (тип, `:40-48`) | `sessionJobs` (`work_ledger.go`) | внутренне |
| `.start(sessionID, toolCallID, cli, cancel) error` (`:90-109`) | `.Start(owner, toolCallID, toolName, childSession, cli, timeout, cancel) (*asyncJob, bool, error)` | `internal/agent/async_tool.go:56` (`asyncTool.Run`) — новые параметры, новая обработка `existing` |
| `.markDrained` (`:114-121`) | без изменений | `internal/agent/coordinator_background.go:70` (`ClaimAsyncCompletions`) |
| `.queuesLocked` (`:125-127`) | внутри `deliverLocked` (§1.6), логика побайтово та же | — (внутренний метод) |
| `.acknowledged` (`:129-147`) | без изменений | `internal/agent/agent_turn_stream.go:393` (`onToolResult`) |
| `.finish` (`:149-172`) | без изменений, но только для kind=command | `internal/agent/async_tool.go:161` (`asyncTool.finalize`) — оборачивается веткой kind |
| `.finishParked` (`:191-214`) | **удалено** — заменено на `armDelegation` (арминг) + внутренний вызов `transitionToTerminal` из `recheckChild` (освобождение) | `internal/agent/subagent_outcome.go:187` (`subAgentOutcomeRegistry.emit`) — вся функция `emit` удаляется вместе с файлом |
| `.releaseLocked` (`:216-230`) | `deliverLocked` (§1.5) | внутренний |
| `.setJobCompletedHook`/`onJobCompleted` (`:63-65`, `:235-239`) | **удалено** — триггер (ii) внутренний вызов `recheckChild` сразу после успешного `transitionToTerminal`, без индирекции через колбэк | `internal/agent/subagent_outcome.go:208-220` (`installSubAgentOutcomeHooks`) — функция удаляется целиком, вызов из конструктора координатора убирается |
| `.abort` (`:241-255`) | без изменений | `internal/agent/agent_turn_stream.go:391` |
| `.next`/`.pending`/`.running`/`.anyRunning`/`.cancelSession`/`.close` (`:257-353`) | без изменений имени/сигнатуры, тело читает новые поля | `coordinator_background.go` (`NextAsyncCompletion`, `HasPendingAsyncJobs`), `subagent_outcome.go`-преемник (`coordinator_work_scope.go`, см. ниже) |
| `subAgentOutcomeRegistry` (тип, `subagent_outcome.go:82-89`) | **удалено** — состояние (`byChild`) переехало в `workLedger` | — |
| `.park` (`:101-116`) | `workLedger.armDelegation` (§1.4) | `internal/agent/async_tool.go:150-162` (`asyncTool.finalize`) |
| `.tryRelease`/`.nextReleasable` (`:121-148`) | `workLedger.recheckChild` (§1.4) | все триггеры ниже |
| `.release`/`.claim`/`.emit` (`:162-191`) | слиты в один путь внутри `recheckChild` → `transitionToTerminal` → `deliverLocked` | внутренний |
| `installSubAgentOutcomeHooks` (`:208-220`) | **удалено** (см. выше) | место постройки координатора |
| `coordinator.refreshSubAgentCompletion` (`:238-262`) | без изменений, остаётся МЕТОДОМ КООРДИНАТОРА (нужен `c.messages`) | вызывается из `workLedger.recheckChild` через `l.coord.refreshSubAgentCompletion(...)` |
| `coordinator.DescendantWorkPending` (`:291-336`) | без изменений тела, только имена полей внутри | новый файл `coordinator_work_scope.go` |
| `coordinator.anyPendingWorkInMemory` (`:341-352`) | без изменений тела | то же |
| `coordinator.sessionOwnsPendingWork` (`:356-370`) | тело меняется минимально: `c.subAgentOutcomes.hasParkedFor(id)` → `c.asyncJobs.hasParkedFor(id)` | то же |
| `.noteChildRunEnded` (`:375-380`) | `workLedger.recheckChild` (то же имя роли, новый метод) | `coordinator_run.go:170`, `coordinator_background.go:61,134,153` |
| `.parkedParentSessions` (`:386-407`) | без изменений имени | `coordinator.ParkedSubAgentParents` |
| `ParkedSubAgentWorkReporter`/`.ParkedSubAgentParents` (`:409-431`) | без изменений (публичный интерфейс координатора) | `coordinator_work_scope.go` |
| `.releaseCanceledForParent`/`.releaseCanceledForChild`/`.releaseAllCanceled`/`.releaseCanceled` (`:437-512`) | слиты в `workLedger.cancelSession`/`.close` (§1.4) — больше не нужны как отдельные функции | `coordinator_interrupt.go:47-48,59` |
| `.hasParked`/`.hasParkedFor` (`:516-558`) | без изменений имени/сигнатуры | `coordinator.anyPendingWorkInMemory`/`.sessionOwnsPendingWork` |
| `.close` (registry, `:562-578`) | слито в `workLedger.close` | `coordinator_interrupt.go:59-62` (`CancelAll`) |
| `.startTickerLocked`/`.tick` (`:582-638`) | переносятся почти без изменений в `work_ledger_delegation.go`, зовут `recheckChild` вместо `tryRelease` — **сохраняются в фазе 1** (см. §5 почему не удаляются) | внутренний |
| `.gcLocked` (`:642-655`) | без изменений роли, оперирует `[]*asyncJob` вместо `[]*subAgentOutcomeEntry` | внутренний |
| `coordinator.subAgentWorkTerminal` (`:674-688`) | `workLedger.childScopeDrained` (§1.4) — с ИСПРАВЛЕННЫМ через `sessionBusy`/`driverFor` busy-гейтом ТОЛЬКО если #1049 уже дал драйвер, иначе побайтово тот же (всегда-`false`) дефект | вызывается из `recheckChild` |
| `.parkSubAgentOutcome` (`:693-705`) | `workLedger.armDelegation` (см. выше) | `async_tool.go` |
| `.noteSubAgentChildRunEnded` (`:709-714`) | тонкая обёртка `c.asyncJobs.recheckChild(sessionID)` — имя МОЖЕТ остаться для минимального диффа в трёх вызывающих | `coordinator_run.go:170`, `coordinator_background.go:61,134,153` |
| `.releaseSubAgentOutcomesForParentCancel`/`.releaseSubAgentOutcomesForChildCancel`/`.releaseAllSubAgentOutcomesCanceled` (`:716-740`) | **удалено** — заменено прямыми вызовами `c.asyncJobs.cancelSession`/`.close` | `coordinator_interrupt.go:41-64` (`Cancel`, `CancelAll`) — тела УПРОЩАЮТСЯ (см. §3, шаг 3) |
| `asyncTool.finalize` (`async_tool.go:150-162`) | тело: ветка kind=agent/fetch зовёт `armDelegation`, иначе `finish` | не проверено, нужно ли менять сигнатуру — сегодняшняя годится |
| `coordinator.notifyAsyncCompletion` (`coordinator_background.go:47-64`) | `c.Run(ctx, completion.SessionID, ...)` → `c.wakeOwner(ctx, completion.SessionID, ...)` (§0.2) | правка в теле, сигнатура не меняется |
| `coordinator.notifyBackgroundJobDone` (`:105-154`) | Phase 4 ветка: `c.Run(ctx, sessionID, summary)` → `c.wakeOwner(ctx, sessionID, summary)`; вызов `c.noteSubAgentChildRunEnded` не меняется | правка в теле |

Функции, НЕ входящие в эту таблицу (`FormatAsyncCompletion`,
`filterNonEmpty`, `backgroundJobSummary`, `runAutoResumeRecovered`,
`IsBusy`/`IsSessionBusy` координатора, весь `internal/shell/background.go`,
весь `internal/app/app_run_async.go`) — не трогаются фазой 1 (см. §5).

## 3. Порядок реализации

Каждый шаг — рабочая сборка; для каждого шага перечислены тесты, которые
обязаны быть зелёными СРАЗУ ПОСЛЕ него.

**Шаг 0 — `work_ledger_driver.go` (§0.2) отдельным коммитом.**
`subAgentDriver`, `driverFor` (поле у `workLedger`, изначально нигде не
инициализируется — только определение типа), `wakeOwner`/`sessionBusy` как
методы `*coordinator`, НЕ подключённые ни к чему (пока `c.asyncJobs` —
старый тип, эти методы просто не вызываются). Сборка зелёная, всё
существующее поведение не тронуто. Тесты: весь набор `internal/agent` и
`internal/app` зелёный без изменений (ничего не вызывает новый файл).
*Причина отдельного коммита — §0.2, «Требование по слиянию».*

**Шаг 1 — новые типы, без подключения.** `work_job.go` (§1.1) и
`work_ledger.go` (§1.2, §1.4 частично: `Start`/`acknowledged`/`abort`/
`finish`/`transitionToTerminal`/`deliverLocked`/`next`/`pending`/`running`/
`anyRunning`/`markDrained`/`close`) как отдельный, самостоятельно
компилируемый тип, БЕЗ переключения `coordinator.asyncJobs` на него. Тесты:
существующий набор не тронут (зелёный тривиально); новый
`work_ledger_test.go` (§4) пишется и проходит ПРОТИВ нового типа в изоляции.

**Шаг 2 — делегационная логика, без подключения.** `work_ledger_delegation.go`
(`byChild`, `armDelegation`, `recheckChild`, `childScopeDrained`,
`cancelSession`'s делегационная половина, тикер, `fireTimeout`, `gcLocked`).
`childScopeDrained` уже зовёт `l.coord.background.ActiveOwned`/
`l.coord.sessionBusy` — значит `workLedger.coord *coordinator` заполняется
здесь. Тесты: `work_ledger_delegation_test.go` (§4, портированные сценарии
`subagent_outcome_test.go`) проходят против нового типа напрямую; старый
стек (`async_job_registry.go`+`subagent_outcome.go`) всё ещё РЕАЛЬНО
используется продакшн-кодом и его тесты зелены без изменений.

**Шаг 3 — переключение (главный рискованный шаг, отдельный коммит).**
Одновременно, в одном коммите:
- `internal/agent/async_tool.go`: `Start` (новая сигнатура), `finalize`
  (ветка `armDelegation`/`finish`) — см. §2.
- `internal/agent/coordinator_background.go`: `notifyAsyncCompletion`,
  `notifyBackgroundJobDone` (ветка Phase 4) зовут `c.wakeOwner` вместо
  `c.Run`; `ClaimAsyncCompletions`/`NextAsyncCompletion`/`HasPendingAsyncJobs`
  — тип `c.asyncJobs` меняется, тела не меняются (методы переносятся с теми
  же именами).
- `internal/agent/coordinator_interrupt.go`: `Cancel` — три вызова
  (`releaseSubAgentOutcomesForParentCancel`, `...ChildCancel`,
  `asyncJobs.cancelSession`) схлопываются в один
  `c.asyncJobs.cancelSession(sessionID)`; `CancelAll` — два вызова
  (`releaseAllSubAgentOutcomesCanceled`, `asyncJobs.close`) схлопываются в
  один `c.asyncJobs.close()`.
- `internal/agent/agent_turn_stream.go`: **без изменений** (имена
  `acknowledged`/`abort` сохранены).
- Новый `internal/agent/coordinator_work_scope.go`: `DescendantWorkPending`,
  `anyPendingWorkInMemory`, `sessionOwnsPendingWork`, `ParkedSubAgentParents`,
  `ParkedSubAgentWorkReporter` — тела почти без изменений (см. §2).
- Постройка координатора: `c.asyncJobs = newWorkLedger(...)`,
  `c.asyncJobs.coord = c` (или через параметр конструктора — не проверено,
  где именно сегодня `subAgentOutcomes.coord` присваивается; при реализации
  найти симметричное место для `asyncJobs.coord`), `c.asyncJobs.driverFor =
  <то, что #1049 предоставит, либо nil>`.
- **Удаляются** `internal/agent/async_job_registry.go` и
  `internal/agent/subagent_outcome.go` целиком.

  Тесты, обязанные быть зелёными СРАЗУ после этого шага (это и есть
  приёмка «поведение не изменилось»):
  - весь `internal/agent` (после шага 4 ниже — старые тестовые файлы уже
    заменены новыми, см. §4);
  - все 8 чёрных тестов `internal/app`: `TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON`,
    `TestRunNonInteractiveDefaultCLIModeWaitsForAsyncCommand`,
    `TestWebAsyncCommandCompletionCreatesModelVisibleNotice`,
    `TestRunNonInteractiveWaitsForAsyncSubAgentResult`,
    `TestRunNonInteractiveContinuesPastFiveAsyncCompletions`
    (`app_run_async_completion_test.go`),
    `TestRunNonInteractiveChildReceivesAsyncBashResultFinishedMidTurn`
    (`app_run_subagent_async_midturn_test.go`),
    `TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult`
    (`app_run_subagent_async_result_test.go`),
    `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`
    (`app_run_subagent_root_wait_test.go`),
    `TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn`
    (`async_scenarios_cancel_test.go`) — **ни один не редактируется**, они
    чёрный ящик и не знают о `asyncJobRegistry`/`subAgentOutcomeRegistry`.

**Шаг 4 — тесты.** Удаляются `async_job_registry_test.go`,
`subagent_outcome_test.go`; финализируются (доводятся до реального
использования продакшн-типа) `work_ledger_test.go`,
`work_ledger_delegation_test.go`; `async_tool_test.go` правится МИНИМАЛЬНО
(`newAsyncJobRegistry` → `newWorkLedger`, тип поля `coordinator.asyncJobs`).
Добавляются новые тесты §4(a)-(e). `go test ./internal/agent/...` и
восемь тестов `internal/app`, перечисленных выше, — зелёные.

**Шаг 5 — реанкеринг `docs/async-invariants.md`.** Обязателен тем же
коммитом, что и шаг 3 (правило самого файла, см. его шапку: «каждая фаза...
MUST re-anchor every row its changes touch, in the same commit»). Строки,
чьи ссылки `файл:функция` меняются: ASYNC-01 (владелец задачи), ASYNC-03
(вся строка переписывается — CAS теперь единый), ASYNC-04 (ack-гейт),
ASYNC-05 (ack-гейт), ASYNC-06 (`notifyAsyncCompletion`/
`notifyBackgroundJobDone` — заметить, что `c.wakeOwner` ВСЁ ЕЩЁ зовёт
`c.Run`/`driver.Agent().Run` напрямую из горутины, а не публикует событие —
эта строка остаётся «нарушается — закрывается фазой 2», фаза 1 её не
меняет по существу, только по ссылке на функцию), ASYNC-07 (маршрут не
меняется, ссылка на `queuesLocked` → `deliverLocked`).

Шаг 6 (опционально, не блокирует фазу 1): обновить статус-строку фазы 1 в
`docs/plans/2026-09-27-async-structured-concurrency.md`.

## 4. Тесты

### 4.1 Удаляемые/переписываемые белые тесты и какой сценарий каждый нёс

| Тест (файл) | Сценарий | Куда переезжает |
|---|---|---|
| `TestAsyncJobRegistryWaitsForPersistedToolResult` | ack-гейт: доставка ждёт `acknowledged`, FIFO готовой очереди | `work_ledger_test.go`, тот же сценарий против `workLedger` |
| `TestAsyncJobRegistryWebCallbackExactlyOnce` | двойной `finish` → колбэк вызван ровно один раз | то же |
| `TestAsyncJobRegistryConcurrentFinishAndAcknowledge` | гонка `finish`×`acknowledged` не теряет и не дублирует | то же |
| `TestAsyncJobRegistryCancelAndClose` | отмена контекста executor'а, `close` отклоняет новый `Start` | то же |
| `TestAsyncAgentTool_NoFinishedNoticeWhileChildOwnedJobsPending` | нет уведомления родителю, пока ребёнок владеет незавершённой работой (свойство 1) | `work_ledger_delegation_test.go`, через `armDelegation`+`recheckChild` вместо `park`+`tryRelease` |
| `TestAsyncAgentTool_SingleFinalNoticeAfterChildJobsDrain` | ровно одно финальное уведомление после осушения (свойство 2) | то же |
| `TestSubAgentOutcome_FailedChildJobDeliveredOnceAsFailure` | провалившийся ход ребёнка доставляется как ошибка, не успех (свойство 3) | то же |
| `TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce` | конкурентные `tryRelease`(→`recheckChild`) доставляют ровно один раз (свойство 5а) | то же, барьерный харнесс (`barrieredMessages`/`refreshBarrier`) переносится без изменений |
| `TestSubAgentOutcome_CancelReleasesParkedOutcome` | отмена освобождает запаркованное уведомление, не теряет его (свойство 4а) | то же, через `cancelSession` |
| `TestSubAgentOutcome_CancelSurvivesFinishedChildTurn` | отмена НЕ читает `refreshSubAgentCompletion` — не подменяет отмену успехом (свойство 5б) | то же |
| `TestSubAgentOutcome_ResumeAfterNoticeDoesNotReemit` | резюме той же дочерней сессии не переигрывает уже доставленное уведомление (свойство 4б) | то же |
| `TestSubAgentOutcome_BusyChildDefersReleaseUntilTurnEnds` | занятый ребёнок откладывает освобождение до конца хода (негативный busy-гейт R1) | то же, `childScopeDrained` |

`TestAsyncToolReturnsBeforeCommandFinishes` и
`TestAsyncToolWebCompletionWaitsForToolResult` (`async_tool_test.go`) — НЕ
переписываются по сценарию, только по имени конструктора (`newAsyncJobRegistry`
→ `newWorkLedger`), см. §3 шаг 4.

Восемь чёрных тестов `internal/app` (список в §3, шаг 3) — не редактируются
вовсе; это и есть подтверждение «поведение снаружи не изменилось».

### 4.2 Новые тесты

**(a) Гонка finish/stop/timeout/cancel → ровно один исход.**
`TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome`
(`work_ledger_test.go`): один `Start`, затем одновременно (горутины)
`finish(success)`, `finish(failure)`, `cancelSession(owner)`,
`fireTimeout(...)` — проверить, что ровно ОДИН исход доставлен
(`require.Len(delivered, 1)`), остальные — no-op. Прямая проверка ASYNC-03 и
закрытия BL-2/#1032.
*Revert-check:* вернуть `cancelSession` к сегодняшней форме (подмена всей
карты `s.jobs = make(...)`, `async_job_registry.go:331-332`) вместо
поштучного `transitionToTerminal` — тест обязан упасть (0 или 2+ доставок,
либо гонка `finish` на `job==nil`).

**(b) Двойной `finishParked`-эквивалент не доставляет дважды.**
`TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce` переносится как есть
(п. 4.1). Добавляется НОВЫЙ, непроверявшийся сегодня случай:
`TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce` — одновременно
`recheckChild(child)` (готовность истинна) и `cancelSession(parent)` для
ТОЙ ЖЕ армированной делегации; проверить ровно одну доставку и что она не
"порванная" (либо чистый успех через recheck, либо чистая отмена через
cancel, не смесь полей).
*Revert-check:* если `cancelSession` и `recheckChild` проверяют
"уже отдано" РАЗНЫМИ ad-hoc условиями вместо общего `transitionToTerminal`
— тест обязан показать двойную доставку или панику на двойном `delete` из
карты.

**(c) Ack-гейт.** `TestAsyncJobRegistryWaitsForPersistedToolResult`
переносится как есть. Новый:
`TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce` — `Start` делегации,
`armDelegation` с ЗАВЕДОМО осушённой областью ребёнка (готовность истинна
немедленно), но `Announce` ещё не вызван; проверить, что доставки НЕТ, пока
`Announce` не вызван. Сегодня это утверждение НИКОГДА не проверяется
отдельно: тестовый хелпер `parkDelegation` (`subagent_outcome_test.go:74-88`)
всегда зовёт `acknowledged` ДО парковки — реальный гэп в покрытии, закрытый
здесь.
*Revert-check:* убрать проверку `announced` из `deliverLocked` — тест
обязан упасть (доставка до объявления).

**(d) Идемпотентный старт (#1038).**
`TestWorkLedger_StartIsIdempotentPerToolCallID` — два `Start` с одним
`(owner, id)`, проверить `existing=true` на втором, тот же указатель задачи,
и что счётчик вызовов исполнителя (шпион в `asyncTool.run`, либо прямая
проверка на уровне `asyncTool.Run` в `async_tool_test.go`) не увеличился.
*Revert-check:* вернуть `Start` к сегодняшнему поведению "ошибка на
дублирующемся ключе" — тест обязан упасть (ошибка вместо `existing=true`,
либо второй вызов исполнителя, если вызывающий код проигнорирует ошибку).

**(e) Драйверный сеанс (#0.2).**
`TestCoordinator_WakeOwnerUsesRegisteredDriverNotCurrentAgent` — регистрация
фейкового `driverFor`, вызов `wakeOwner`, проверка, что вызван ИМЕННО
драйверный `Agent().Run`, а не `c.currentAgent.Run`. Второй кейс в том же
тесте (или отдельный) — без регистрации драйвера `wakeOwner`/`sessionBusy`
обязаны звать `c.Run`/`c.currentAgent.IsSessionBusy` — то есть СЕГОДНЯШНЕЕ
(дефектное для делегаций) поведение, дословно.
*Revert-check:* захардкодить `wakeOwner` на безусловный `c.Run` — первая
часть теста (драйвер используется) обязана упасть.

## 5. Что фаза 1 НЕ меняет и что закрывает

**Не меняется:**

- маршрут доставки (готовая очередь vs `onWebDone`) — §1.6, побайтово;
- цикл опроса в `internal/app/app_run_async.go`
  (`runNonInteractiveWithAsyncResults`, `descendantWorkPollInterval = 100 *
  time.Millisecond`) — событийный учёт областей это фаза 3;
- защитный тикер (`subAgentOutcomeTickInterval`, было `subagent_outcome.go:46`)
  — **сохраняется**, переносится в `work_ledger_delegation.go` без удаления.
  Дизайн-документ относит устранение «четырёх триггеров и тикера» к разделу
  «Учёт областей» (§4 целевой модели), а раздел «Миграция» явно назначает
  это **фазе 3** («Фаза 3 — учёт областей... Четыре триггера и тикер
  subagent_outcome.go исчезают»), а не фазе 1. Внутренняя доставка триггеров
  (i)/(ii) становится детерминированной как побочный эффект слияния типов
  (см. §1.4), но это не повод убирать защитную сетку раньше срока, который
  сам дизайн-документ ей назначает;
- `internal/session/descendant_liveness.go` (эвристики lock-файлов для
  `sessions why/list`) — фаза 4;
- ветвление `if origin != CLI && origin != Web { return inner.Run }` в
  `asyncTool.Run` (`async_tool.go:44-46`) — закрывает #1037, фаза 2;
- `internal/shell/background.go` (`BackgroundShellManager`) — не тронут
  вовсе, дизайн-документ явно требует сохранить его поведение;
- механизм доставки `timeoutWakeOnly`-уведомления («время вышло, работа
  продолжается») и сам сервис сроков (планировщик, будящий по ближайшему
  дедлайну) — реализуются в **фазе 5** вместе с `wakein`/`wakeon`/`loop`
  (дизайн-документ: «Надзор... первый потребитель сервиса сроков» — сам
  сервис вводится там же). Причина не делать это раньше: `wakeOnly`
  нуждается в НОВОЙ форме доставки (нетерминальное событие поверх
  терминальной по своей природе `AsyncCompletion`), а форма доставки в
  целом переопределяется фазой 2 — строить новую форму дважды не нужно;
- host/lease-восстановление для #1040 (`phaseInterrupted` без вызывающего)
  — фаза 4; решение оператора «пропущенные дедлайны при простое не
  разыгрываются» (§0.1.3) адресовано ЭТОЙ фазе, фаза 1 записывает его здесь,
  чтобы реализующий фазу 4 не передумывал заново;
- поддержка вложенной делегации — не цель НИКОГДА (структурно запрещается
  отдельной задачей #1050, §0.1.5).

**Закрывает:**

- **#1032 / BL-2** — полностью. Единый `transitionToTerminal` под одним
  мьютексом устраняет ИМЕННО ту гонку (`cancelSession` подменяет карту
  → конкурентный `finish` видит `job==nil` → доставка теряется или,
  через `finishParked`'s ветку пересоздания строки, задваивается), которую
  `docs/async-invariants.md` фиксирует как нарушение ASYNC-03 сегодня. Тест
  §4.2(a) — прямое доказательство.
- **#1019 / BL-1** — **частично**. Диагностика design-doc (пункт 3
  диагностической таблицы) описывает вычисляемую (не отслеживаемую)
  завершённость поверх ПЯТИ разных хранилищ с разными мьютексами. Слияние
  `asyncJobRegistry`+`subAgentOutcomeRegistry` в `workLedger` устраняет
  конкретно НЕАТОМАРНОСТЬ МЕЖДУ ЭТИМИ ДВУМЯ (сегодня `sessionOwnsPendingWork`
  читает `c.asyncJobs.running(id)` и `c.subAgentOutcomes.hasParkedFor(id)`
  под ДВУМЯ разными мьютексами последовательно — окно между чтениями
  реально существует). После фазы 1 это ОДНО чтение под ОДНИМ мьютексом.
  Остаточная гонка — с ТРЕТЬИМ хранилищем, `BackgroundShellManager`
  (`background.ActiveOwned`, свой собственный `csync.Map`/атомики,
  сознательно не тронут дизайном), которую слияние двух ИЗ ПЯТИ хранилищ не
  устраняет — это остаётся фазе 3 (событийный счётчик области).
  Точный воспроизводящий сценарий #1019 в прочитанных документах не описан
  — не проверено, какая ИМЕННО комбинация чтений его вызывала; утверждение
  выше — структурное (какая гонка перестаёт существовать по конструкции), а
  не подтверждение, что именно эта гонка была репродюсером #1019.

## 6. Риски и раскладка по файлам

**Риски:**

- **Общий мьютекс для команд и делегаций.** Сегодня `asyncJobRegistry.mu` и
  `subAgentOutcomeRegistry.mu` — два разных мьютекса; после слияния — один.
  При `maxAsyncJobsPerSession = 50` и типичном числе сессий вклад в
  contention незначителен на глаз, но не измерялся — не проверено; перед
  мержем стоит короткий бенчмарк (`go test -bench` на `workLedger` с 50
  параллельными `Start`/`Finish`/`recheckChild`), а не полагаться на
  интуицию.
- **Порядок слияния с #1049.** `driverFor`/`wakeOwner`/`sessionBusy` —
  единственная точка соприкосновения с параллельной работой. Требование
  §0.2 (отдельный первый коммит, `coordinator_subagents.go` не трогается
  фазой 1) снижает риск конфликта, но не устраняет его полностью, если
  #1049 назовёт что-то из этих трёх символов иначе или расположит
  регистрацию в файле, который фаза 1 всё же трогает
  (`coordinator_background.go`) — координация нужна перед слиянием обеих
  веток, не после.
- **Реанкеринг `docs/async-invariants.md`.** Минимум 5 строк меняют ссылки
  (§3, шаг 5); пропуск любой оставляет реестр, который «врёт о том, где
  живёт его закон» — именно то, что сам файл называет хуже отсутствия
  реестра.
- **Оракул «чистого переноса» неприменим.** `CLAUDE.md`'s `before.txt`/
  `after.txt`-диф (сравнение множества `func/type/var/const` до и после)
  предназначен для ЧИСТОГО разделения файла без изменения поведения — здесь
  происходит функциональное СЛИЯНИЕ двух типов в один с реальным
  изменением внутренней формы (убирается индирекция колбэка, убирается
  двойная бухгалтерия по одному tool-call), так что этот оракул не
  применим. Правильная проверка — §3's список из 8 чёрных тестов
  `internal/app` (полностью не редактируемых) плюс полное прохождение
  портированных белых тестов §4.1 со СОХРАНЁННЫМИ сценариями.
- **Лимит 1000 строк.** Ни один новый файл не приближается к пределу
  (оценки ниже); после написания — проверить `wc -l` по факту, не по
  оценке.

**Раскладка по файлам** (пакет `internal/agent`, без подпакета — «в папку»
здесь означает «файлы в этом же каталоге», как того требует `CLAUDE.md`):

| Файл | Содержимое | Оценка строк | Замещает |
|---|---|---|---|
| `work_job.go` | `jobKind`, `jobPhase`, `jobResult`, `timeoutKind`, `TimeoutSpec`, `asyncJob`, `transitionToTerminal` | ~150 | часть `asyncJobState`, часть `subAgentOutcomeEntry` |
| `work_ledger.go` | `sessionJobs`, `workLedger`, `Start`/`acknowledged`/`abort`/`finish`/`deliverLocked`/`next`/`pending`/`running`/`anyRunning`/`markDrained`/`close` | ~380 | `async_job_registry.go` (353, удаляется) |
| `work_ledger_delegation.go` | `byChild`, `armDelegation`, `recheckChild`, `childScopeDrained`, `cancelSession`, `fireTimeout`, тикер, `gcLocked`, `hasParked`/`hasParkedFor`/`parkedParentSessions` | ~340 | делегационная половина `subagent_outcome.go` |
| `work_ledger_driver.go` | `subAgentDriver`, `driverFor`, `wakeOwner`, `sessionBusy` | ~80 | новое — точка стыковки с #1049 |
| `coordinator_work_scope.go` | `DescendantWorkPending`, `anyPendingWorkInMemory`, `sessionOwnsPendingWork`, `ParkedSubAgentParents`, `ParkedSubAgentWorkReporter` | ~180 | координаторская половина `subagent_outcome.go` |
| `async_tool.go` (правка на месте) | `Start`-вызов, ветка `finalize` | 202 → ~210 | — |
| `coordinator_background.go` (правка на месте) | `wakeOwner` вместо `c.Run` в двух местах | 191 → ~193 | — |
| `coordinator_interrupt.go` (правка на месте) | `Cancel`/`CancelAll` — по одному вызову вместо трёх/двух | 644 → ~635 | — |
| `agent_turn_stream.go` | без изменений | 397 | — |

Удаляются целиком: `async_job_registry.go` (353), `subagent_outcome.go`
(740). Ни один файл выше не подходит к 1000 строкам — записи в
`.githooks/file_size_allowlist.txt`/`.golangci.yml` не требуются.

## Открытые вопросы оператору

1. **Тикер (§5).** Спецификация оставляет защитный 2-секундный тикер живым
   в фазе 1, ссылаясь на то, что дизайн-документ явно назначает его
   устранение фазе 3. Если оператор считает, что слияние регистров уже
   достаточно детерминирует триггеры (i)/(ii) и хочет убрать тикер СРАЗУ в
   фазе 1 (не дожидаясь фазы 3) — это меняет объём фазы 1 и требует
   отдельного теста «без тикера все 4 триггера всё ещё покрывают все
   пути» (в частности — гонки с фоновыми shell-задачами, которые остаются
   вне реестра).
2. **Порядок с #1049.** Нужно ли фазе 1 в самом деле резервировать
   `driverFor` как функцию-инъекцию, или лучше согласовать с автором #1049
   точное имя/сигнатуру интерфейса ДО написания `work_ledger_driver.go`,
   чтобы не потребовалась повторная правка после его мержа? Спецификация
   выбрала интерфейс минимального размера (`Agent()`+`BuildWakeCall`)
   намеренно как наиболее вероятную форму, но это не проверено против
   реального кода #1049 (он не существует в дереве на момент написания).
3. **Таймауты (§0.1.4, §1.1).** Подтверждить, что хранение `deadline`/
   `timeoutKind` в `asyncJob` УЖЕ в фазе 1 (без вызывающего в проде, кроме
   теста) — то, что нужно, а не преждевременное усложнение типа. Альтернатива
   — не трогать тип в фазе 1 вообще и добавить оба поля вместе с сервисом
   сроков в фазе 5 (аддитивное расширение структуры, без обратной
   несовместимости). Спецификация выбрала «завести сейчас» по прямому
   указанию из корректировки задания; стоит явно подтвердить, что это
   намерение, а не побочный эффект того, что корректировка пришла посреди
   работы над документом.
4. **`FireTimeout`/`phaseInterrupted` без продакшн-вызывающего.**
   Реализованы и покрыты тестом в фазе 1, но не вызываются ничем в
   продакшене вплоть до фазы 5 (`FireTimeout`) и фазы 4 (переход в
   `phaseInterrupted`). Это осознанный выбор (тест раньше реализации,
   CAS доказан заранее), но стоит подтвердить, что line-coverage-требования
   CI (если такие есть в `.golangci.yml`/pre-push) не потребуют доп.
   аннотаций для методов, вызываемых только тестом — не проверено, есть ли
   в проекте такое правило.

## Решения оркестратора по открытым вопросам (2026-09-27)

1. **Страховочный тикер** в фазе 1 не удалять: он уходит в фазе 3 вместе с
   заменой на событие «область закрыта», как в дизайн-документе.
2. **Форма драйвера** — не новый интерфейс, а уже влитая реализация #1049:
   `internal/agent/coordinator_subagent_drivers.go` (`subAgentDriver{agent
   SessionAgent, call SessionAgentCall}`, `subAgentDriverRegistry`,
   `c.subAgentDrivers.get/register`), используемая в `notifyAsyncCompletion` и
   `subAgentWorkTerminal`. §0.2 применять с этой заменой: `driverFor` =
   `c.subAgentDrivers.get`, отдельный шаг «сначала шов драйвера» не нужен.
3. **`deadline` / `timeoutKind` / `FireTimeout` / `phaseInterrupted`** в фазе 1
   НЕ добавлять: поля и функции без производственного вызова нарушают принцип
   «не добавлять то, чего не требует приёмка» и ловятся линтером (unused).
   Таймауты (#1037, два вида) добавляются в той фазе, которая их доставляет
   (фаза 2 — доставка события «время истекло»), `interrupted` — в фазе 4
   (восстановление после смерти хоста).
4. **Правил покрытия в CI нет**; ограничение одно — линтер (см. п. 3).
