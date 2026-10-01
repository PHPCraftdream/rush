# Арбитр автоматического хода (R-ARB-D): единая точка решения

Статус: дизайн (R-ARB-D, задача #1120, ветка `rarbd`). Код не меняется.
Реализация: R-ARB-1 (чистая функция + чтение фактов + тень), затем R-ARB-2
(перевод вызывающих, удаление состояния). Контракт — раздел R-ARB в
`docs/plans/2026-10-01-structural-refactor.md`. База — текущий код после
8 раундов ревью и дизайна попыток W2-ATTEMPTS
(`docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md`, в коде это
`internal/agent/drain_attempt.go` и гейт в `work_ledger_reaction.go`), плюс
поправки (x)/(aa)/(ab)…(ao) дизайна фазы 4.

## 0. Диагноз

Решение «давать ли сессии автоматический ход» сегодня принимает не одна
функция: `drainPolicy` (`internal/agent/coordinator_drain_policy.go:56`)
собирает вердикт из ~10 независимых запросов, часть которых читает
собственное изменяемое состояние, а часть — БД; `decideDrainTurn`
(`internal/agent/agent_drain_decision.go:21`) повторяет часть решения в
момент коммита хода; `CLIScope` (`internal/agent/coordinator_reaction_source.go:229`)
и `wakeSession` (`internal/agent/coordinator_wake.go:27`) транслируют тот же
вердикт в свои перечисления. Состояние размазано по четырём owner'ам:

- `coordinator` под `autoResumeMu` (`coordinator.go:332–358`):
  `consecutiveAutoResumes`, `bgShellOverCap`, `autoTurnsSuspended`,
  `turnHolds`, `consecutiveDrainLinks`, `reactionChainClaims`,
  `reactionChainNoticed`;
- `coordinator` под `recheckMu` (`coordinator.go:360–382`): `recheckSet`,
  `recheckWakeInFlight`;
- `workLedger` под `l.mu` (`work_ledger.go`, рядом с `hintSeq`): `drainGate`
  (`retryAt`, `hintAt`, `hintOpens`, `freeStreak`, `paidStreak`);
- БД: колонки `wake_attempts` и `reacted_failed` на строках задач/уведомлений
  (`internal/db/migrations/20260928000002_add_async_phase4_core.sql:81,:139`).

Три разных «3» с разным смыслом (предел цепочки `ReactionChainLimit = 3`,
`internal/agent/coordinator_reaction_chain.go:24`; сон гейта
`drainDormantStreak = 3`, `drain_attempt.go:45`; закрытие по K=3
`drainFailureSettleThreshold = 3`, `drain_attempt.go:39`) — законны, но без
имён, вынесенных в арбитр, читатель принимает одно за другое.

Цель рефакторинга — не новое поведение, а одно место, где решение
принимается, и одна структура, где живут его входы.

## 1. Инвентаризация

«Под-решение» — какая часть решения «ход сейчас?» принимается здесь.
«Находки» — раунды ревью фазы 4, которые касались именно этого места
(`docs/reviews/2026-09-29-async-phase4-round1.md`,
`docs/reviews/2026-09-30-async-phase4-round{2..8}.md`).

| Функция / состояние | Под-решение | Находки |
|---|---|---|
| `drainPolicy` (`coordinator_drain_policy.go:56`) | сводная политика: чужой живой ведущий, удержание Rerun, суспензия, предохранитель цепочек, идущая делегация, выпущенный ребёнок, предел bg-shell, bg-only при выключенной автономии | R2B-4/R2B-5 (отказ в допуске не закрывает долг), R2B-10 (fail-closed), R3C-5 (удержание = paced без часов), R6B-1 (ребёнок freed от капы, не от предохранителя), R2B-17 |
| `drainPermitted` (`coordinator_drain_policy.go:164`) | политика + гейт; ЕДИНЫЙ предикат запуска | R4B-1 → R5B-1/R5B-2 (два читателя гейта расходились) |
| `decideDrainTurn` (`agent_drain_decision.go:21`) | решение в момент коммита хода (queued Drain решает по долгу, которого его запуск не видел; `spent=false`) | R2B-1(C), drain_bgcap_ids (`spent=true` мутант) |
| `claimAutoResume` (`coordinator.go:673`) | трата слота bg-shell при допуске; Stop-суспензия отказывает слот | R2B-16 (двойной счёт), R3B-6, поправка (aa) |
| `bgShellCapDeferred` (`coordinator_bgshell_cap.go:144`) | «весь долг — строки поверх капы» → deferred | R2B-16, R3B-6 |
| `drainGateOpen` / `paceDrainGate` (`work_ledger_reaction.go:169`, `:190`) | пейсинг и сон: открыт ли гейт, спит ли (free/paid), `retryAt` | R2B-1(C)/(D) (hot loop), R3B-4 (две серии вместо одной) |
| `accountDrainAttempt` (`drain_attempt.go:223`) | ЕДИНСТВЕННЫЙ учёт попытки: посчитать, уснуть, закрыть, ставить в recheck | R2B-1 (учёт в запускателях давал дыры), R2B-9 (снимок после переноса) |
| `drainAttemptExempt` / `operatorStop` / `drainFailureTerminal` (`drain_attempt.go:197`, `:190`, `:208`) | исключение (Ctrl-C/Stop/таймаут хода) и терминальный класс | R3B-1 (дедлайн ≠ отмена), R3B-3 (нога, оборванная сообщением человека), R3B-7 (401) |
| `noteDrainRefused` (`drain_attempt.go:377`) | пейсинг отказа в допуске (не счёт, не закрытие) | R2B-4, R2B-5, A11 (lock busy → hot loop) |
| `settleDrainDebt` (`drain_attempt.go:407`) | закрытие долга неудачей (маркер, `reacted_failed=1`) | R3B-5 (K по собственным попыткам строки), R2B-4 (peak hours не закрывает) |
| `snapshotAttempts` (`drain_attempt.go:337`) | чтение `wake_attempts` по снимку | R3B-5 |
| `wakeSession` (`coordinator_wake.go:27`) | запускатель web/детей: hint по факту, verdict → запуск / recheck | R2B-7 (проба lock удалена), R2B-18 (без координаторных повторов) |
| `RecheckPass` / `recheckSet` / `recheckWakeInFlight` (`coordinator_recheck.go:80`, `coordinator.go:368–382`) | 60-с проход: кто переоценивается, без параллельных будильников | R6B-1 P1 («долг есть, а запускателя нет»), R2B-17 |
| `onSessionIdleHook` / `afterRelease` (`supervision.go:449`, `:461`) | запускатель освобождения мейлбокса | R2B-8 (ошибка проверки долга → recheck) |
| `notifyAsyncCompletion` / `notifyBackgroundJobDone` / `delegatedChildDriven` (`coordinator_background.go:71`, `:126`, `:207`) | факт → hint; bg-shell → право на слот | R2B-16 (один bump), R6B-1 |
| `isDurableDelegationChild` (`coordinator_drain_policy.go:223`) | выпущенный ребёнок не падает на root-агента | B3/C6, R2B-5 (durable identity) |
| `childScopeDrained` / `childScopeOpenAcrossProcesses` (`work_ledger_delegation.go:213`, `:263`) | когда делегация выпускает ребёнка (область ребёнка закрыта?) | R3C-5 (удержание держит делегацию), R7B-1 |
| `recheckChild` (`work_ledger_delegation.go:98`) | запускатель припаркованной делегации | R2B-17 (early return) |
| `CLIScope` (`coordinator_reaction_source.go:229`) | тот же вердикт, переведённый в `DrainOwed/Paced/Deferred/Stuck` для CLI-цикла | R2C-6, R8B-3 |
| предохранитель цепочек: `consecutiveDrainLinks`, `reactionChainClaims`, `reactionChainNoticed` (`coordinator.go:356–358`), `chainGuardDeferred` (`coordinator_reaction_chain.go:110`) | N=3 «холостых» звена, весь долг — свои завершения → deferred + один маркер | B5/B6 → R2B-1/R2B-18, #1113 |
| `autoTurnsSuspended` (`coordinator.go:346`) | Stop/вопрос ребёнка: автоматика до сообщения человека | R2B-1(B) (вопрос = реакция + суспензия) |
| `turnHolds` (`coordinator.go:349`) | Rerun держит автоматику на время отмены/передачи | R2C-4, R3C-5 |
| `consecutiveAutoResumes` / `bgShellOverCap` (`coordinator.go:333–338`) | предел bg-shell автоходов (5 на сообщение человека) и строки поверх капы | R2B-16, R3B-6, (aa) |
| `drainGate{retryAt, hintAt, hintOpens, freeStreak, paidStreak}` (`work_ledger.go`, под `l.mu`) | пейсинг/сон, открывание по новому факту | R3B-4, R3B-8 |
| БД `wake_attempts`, `reacted_failed` (`…_core.sql:81`, `:139`) | долговечный счёт попыток и отметка закрытия неудачей | R3B-5, A5 |

Дополнение к списку задачи: `drainDecision` (долг + `drainPermitted` одним
вызовом), `sessionDebtIsBGShellOnly`, `autoResumeSuspended`,
`automaticTurnsHeld`, `isExternalDriver`/`foreignLiveDriver` — это те же
под-решения, читающие перечисленное состояние, отдельных решений не
добавляют.

## 2. Модель

### 2.1 `TurnFacts` — один снимок входов

```go
// internal/agent/turn_arbiter.go
type TurnFacts struct {
    Now      time.Time
    Site     LaunchSite // siteFact, siteRelease, siteTick, siteCommit, siteCLI, siteChild
    Session  SessionFacts
    Debt     DebtFacts
    Gate     GateFacts
}

type SessionFacts struct {
    ExternallyDrivenSelf bool // isExternalDriver: этот процесс ведёт
    ForeignDriverLive    bool // чужой живой ведущий (БД-маркер)
    Held                 bool // turnHolds > 0 (Rerun)
    Suspended            bool // Stop / вопрос ребёнка
    ChainLinks           int  // consecutiveDrainLinks
    ChainOwnsAllDebt     bool // весь долг — завершения собственных холостых запусков
    ChainNoticed         bool // маркер уже вставлен
    RunningDelegation    bool // строка делегации running
    DurableChild         bool // был создан как ребёнок делегации
    AutonomyEnabled      bool // AutoResumeOnJobDone
    AutoResumes          int  // с последнего сообщения человека
}

type DebtFacts struct {
    Visible      session.DebtSnapshot // delivery='done', wake=1, reacted=0
    PendingIncl  bool                 // долг с учётом pending (перенос не снимает)
    BGShellOnly  bool                 // весь долг — завершения фоновых shell
    OverCapRows  int                  // строки, чей слот истрачен и запуска не дождались
}

type GateFacts struct {
    Paced      bool          // retryAt не нулевой
    RetryAt    time.Time
    HintOpens  bool          // новый факт открывает раньше
    HintSeen   uint64        // hintAt, с которым гейт закрылся
    HintNow    uint64        // текущий hintSeq
    FreeStreak int
    PaidStreak int
}
```

**Как читается.** БД-половина (`DebtFacts`: снимок, `PendingIncl`,
`BGShellOnly`, `OverCapRows`, `ForeignDriverLive`, `RunningDelegation`,
`DurableChild`) — **одна read-транзакция/один набор запросов** через
`AsyncJobStore` (то, что сегодня делают `ReactionDebtExists`,
`PendingInclusiveDebtRows`, `CaptureDebtSnapshot`, `HasRunningDelegationFor`,
`ForeignLiveDriver` по отдельности, растянуто во времени между чтениями —
это источник расхождений «факт пришёл между двумя проверками»). In-process
половина (`SessionFacts`, `GateFacts`) — один захват `autoResumeMu` + один
захват `l.mu`, скопированный в значение; арбитр не держит оба мьютекса
одновременно. Порядок: сначала БД, потом память — факт, пришедший между
чтениями, видим в памяти и потому не потерян (инверсия могла бы потерять).

**Что обязано быть долговечным:** только то, что переживает процесс по
смыслу, а не по механике — `reacted`, `reacted_failed`, `wake_attempts`
(счёт на строке: переживает и разделяется между процессами), `delivery`,
`wake`. Всё остальное (`GateFacts`, суспензия, удержание, счётчик капы,
цепочка) — состояние процесса: падение процесса = открытый гейт и первая
попытка считается (решение §1.8 дизайна попыток, оно остаётся).

### 2.2 `Verdict`

```go
type Verdict struct {
    Kind      VerdictKind // VRun, VDefer, VClose, VNone
    Agent     string      // VRun: какой агент (root/child текущий — решает вызывающий)
    Counted   bool        // VRun: слот bg-shell тратится этим запуском
    Reason    string
    RecheckAt time.Time   // VDefer: нулевой = «тиком или релизом», не часами
    ReopenOnHint bool     // VDefer: новый факт открывает раньше RetryAt
    Rows      session.DebtSnapshot // VClose: что закрывать
    Marker    string               // VClose: текст маркера
}
```

Отображение на сегодняшние виды: `drainAllow` → `VRun{Counted: !spent}`;
`drainPaced` (удержание, гейт) → `VDefer{ReopenOnHint: …}` с
`Reason`/`RecheckAt`; `drainDeferred` → `VDefer`; `drainStuck` (сон гейта) →
`VDefer{ReopenOnHint: false, RecheckAt: RetryAt}`; «долга нет» → `VNone`.
`VClose` — вердикт, которого в `drainVerdict` нет: сегодня решение «закрыть
долг» принимает `accountDrainAttempt` вне политики; оно переносится в
арбитр (см. §3), исполняет его по-прежнему точка учёта. Уточнение к контракту
структурного плана: `Close` выдаётся только арбитром на стороне учёта
(входы — снимок и счётчики строк), никогда — на стороне запуска.

### 2.3 Где живёт изменяемое состояние арбитра

**Выбор: одна структура под одним мьютексом, не строка БД.**

```go
// internal/agent/turn_arbiter.go
type arbiterState struct {  // одна на сессию
    suspended, held            bool
    autoResumes                int    // с последнего сообщения человека
    overCap                    map[int64]struct{} // id строк поверх капы
    chainLinks                 int
    chainClaims                map[string]struct{}
    chainNoticed               bool
    gate                       GateFacts // retryAt, hintAt, hintOpens, streaks
}

type arbiter struct {
    mu    sync.Mutex
    bySession map[string]*arbiterState
}
```

Обоснование против «строки БД на сессию»:

1. **Многопроцессность уже решена на нужном уровне.** Web + несколько
   `rush run` на одной БД согласуются по **долговечной** правде: строкам
   (`reacted`, `wake_attempts`, `reacted_failed`, `delivery`, маркер
   внешнего ведущего). Пейсинг и сон — часовое состояние процесса:
   `time.Now()` в БД требует «часов в БД» (уже отвергнуто в §3
   «Rejected alternatives» дизайна попыток), recovery полузаписанного
   пейсинга, и ничего не даёт: после падения процесса гейт и так обязан
   открыться. Процесс B, начавший реакцию на сессию A, считается с
   открытого гейта — это корректно и безопасно: долговечная граница
   (K=3 на строку) общая.
2. **Один писатель.** Сегодня гейт пишут три точки (`accountDrainAttempt`,
   `noteDrainRefused`, сброс по сообщению человека) через два owner'а
   (`coordinator.autoResumeMu` и `workLedger.l.mu`). Сведение в одну
   структуру делает писателя одним, а правило «арбитр не делает DB I/O под
   мьютексом» — проверяемым.
3. **Уборка имеет одно место.** R3B-8 (снятие записей без задач) — один
   проход по `bySession`, а не четыре разных map под двумя мьютексами.

Пары `arbiterState`/строка сессии держатся там же, где сегодня `bySession`
workLedger'а; реализация R-ARB-1 может сначала просто агрегировать четыре
map в одну структуру, не двигая владельца.

## 3. Чистая функция `decide(TurnFacts) → Verdict`

Функция без часов (`Now` — вход), без I/O, без чтения глобального
состояния. Правила в порядке приоритета; каждая строка — ветка табличного
теста. Обозначения: K=3 (`drainFailureSettleThreshold`), N=3
(`ReactionChainLimit`), D=3 (`drainDormantStreak`), CAP=5
(`maxConsecutiveAutoResumes`), R=60s (`drainRetryAfterFailure`).

| # | Условие (в порядке проверки) | Вердикт | Источник правила |
|---|---|---|---|
| 1 | `Site != siteAccount` и `Debt.PendingIncl == false` | `VNone` | §3.4: нет долга — нет решения |
| 2 | `ForeignDriverLive && !ExternallyDrivenSelf` | `VDefer{"another process drives", recheck}` | §3.4 внешний ведущий |
| 3 | `Held` | `VDefer{"rerun in progress", RecheckAt: 0, ReopenOnHint: true}` — paced, не deferred | R3C-5 |
| 4 | `Suspended` | `VDefer{"suspended", recheck=false}` | §3.4: после Stop; R2B-1(B): вопрос |
| 5 | `ChainLinks >= N && ChainOwnsAllDebt` | `VDefer{"reaction chain", recheck=false}` (+ один маркер, если `!ChainNoticed` и не CLI) | #1113, R6B-1 (до ветки 6) |
| 6 | `RunningDelegation` | `VRun{Counted: false}` (ребёнок под делегацией — вне капы) | R6B-1 |
| 7 | `DurableChild && !ExternallyDrivenSelf` | `VDefer{"released delegation child"}` | B3/C6 |
| 8 | `Gate.PaidStreak >= D` | `VDefer{stuck, ReopenOnHint: false, RecheckAt: Gate.RetryAt}` — только человек/рестарт | R3B-4 |
| 9 | `Gate.Paced && Gate.RetryAt > Now` | открыть раньше ⇔ `Gate.HintOpens && Gate.HintNow != Gate.HintSeen`; иначе `VDefer{paced, RecheckAt}` | R3B-4 |
| 10 | `Gate.FreeStreak >= D` | как 8, но `ReopenOnHint: true` | R3B-4 |
| 11 | `Site ∈ {siteTick, siteRelease, siteCommit, siteCLI}` (не тратит слот) и `AutonomyEnabled` и `Debt.OverCapRows > 0 && OverCapRows покрывает весь bg-shell долг` | `VDefer{"bg-shell cap"}` | R3B-6, (aa) |
| 12 | `!AutonomyEnabled && Debt.BGShellOnly` | `VDefer{"auto-resume off"}` | §3.4 таблица |
| 13 | иначе | `VRun` (`Counted` — описательное поле: повторяет сайт; слот тратится в точке прихода, не исполнением `VRun`) | §3.4 |

Правила стороны учёта (`Site == siteAccount` — вызывает `accountDrainAttempt`
после конца ноги, снимок и ошибка хода в фактах):

| #a | Условие | Вердикт |
|---|---|---|
| A1 | попытка не дошла до провайдера (отказ в допуске) | `VDefer{refused, R (CLI root — 0.5s в бюджете 30s), ReopenOnHint: true}`; не считается | 
| A2 | нет хода, `PendingIncl` остался (перенос падает) | пауза R, `ReopenOnHint: true`, recheck; `FreeStreak++` |
| A3 | нет хода, долга нет вовсе | сброс гейта |
| A3' | нет хода, коммит отказан при видимом долге (paced/stuck-гейт, hold, суспензия, цепочка, капа, чужой ведущий, нечитаемый вход) | гейт НЕ трогается (P1-1 ревью R-ARB-2); recheck по `commitNo.recheck`, как старая ветка `default` |
| A4 | ход, исключение (Ctrl-C/Stop/дедлайн контекста хода/лимит сработки caps) | ничего (не считается) | 
| A5 | ход, `max wake_attempts(sнимка) == 0` (все отреагировали) | сброс гейта, `ChainLinks = 0` |
| A6 | ход, строка достигла K своими попытками или терминальный класс | `VClose{Rows: строки с K или все при терминальном}`; после закрытия — если долг остался, пауза R (`ReopenOnHint: false`) |
| A7 | ход, иначе | счёт + пауза R (`HintOpens: false`), recheck; `PaidStreak++`, `FreeStreak = 0` |

### Противоречия и дубликаты в сегодняшних правилах

- **Дубликат: капа bg-shell в двух местах.** `claimAutoResume`
  (`coordinator.go:673`) решает «тратить ли слот», `bgShellCapDeferred`
  («отложен ли долг поверх капы») — та же капа второй раз, с другим
  ответом (deferred). В таблице это одна строка (11): сравнение капы —
  часть вердикта. Трата слота остаётся в точке прихода завершения
  (`persistBGShellCompletion` → `claimAutoResumeSlot`, под `bgArrival` —
  утверждено ревью R-ARB-2): перенос её на исполнение `VRun{Counted}`
  сломал бы ASYNC-09 — завершение, пришедшее при закрытом гейте, не
  тратило бы слот, и правило 11 потеряло бы over-cap множество, которым
  оно измеряется. Поле `Verdict.Counted` — описательное (повторяет сайт),
  исполнители его не читают.
- **Дубликат: paced без часов vs deferred+recheck.** Удержание Rerun
  (R3C-5) принудительно делают `drainPaced`, потому что `deferred` читается
  потребителями как «долг можно бросить». В вердикте это одно значение
  (`VDefer` с `RecheckAt: 0`), а различие «бросать ли долг» переходит
  потребителям в явном поле `ReopenOnHint`/`RecheckAt`; `childScopeDrained`
  читает `VDefer{RecheckAt: 0}` как «держу делегацию».
- **Трение: правила 5 и 6.** R6B-1 специально ставит предохранитель ВЫШЕ
  ярлыка «идущая делегация разрешает». Порядок строк таблицы — контракт;
  перестановка 5 и 6 — мутант для ревертчека.
- **Трение: `ReopenOnHint` после оплачиваемой неудачи.** Правила 8–9:
  новый факт НЕ сокращает паузу оплаченной серии (R3B-4: иначе минутный
  сбой закрывал бы долг), но открывает после отказа/бесплатного сна. Сегодня
  это закодировано в `drainGateOpen` неявно (`hintOpens && !paidDormant`);
  в вердикте — явное поле.
- **Три разных «3».** K/N/D остаются тремя константами с именами; правило 5
  (цепочка) и правило A6 (закрытие) не взаимозаменяемы. Допустимая будущая
  унификация — только явным решением оператора.
- **Известное несоответствие, не чинится здесь:** правило 2 (чужой живой
  ведущий) разрешает ЭТОМУ процессу ход, если маркер ведущего этого
  процесса (`ExternallyDrivenSelf`) — верно; но web-процесс при
  `ForeignDriverLive` всё равно переносит уведомления (не в арбитре — в
  пути забора). Арбитр не отвечает на «кто переносит», только «кто ходит».

## 4. Запускатели и свойство «у каждого долга ровно один запускатель»

Запускатели (читают вердикт и исполняют его):

| Запускатель | Сайт | Что исполняет |
|---|---|---|
| `wakeSession` по факту (`notifyAsyncCompletion`, `notifyBackgroundJobDone`, `wake_fired`, надзор) | `siteFact` | `VRun` → подача Drain; иное → recheck-набор |
| освобождение мейлбокса (`onSessionIdleHook` → `afterRelease`) | `siteRelease` | то же, `Counted=false` |
| 60-с тик (`RecheckPass` по `recheckSet`) | `siteTick` | то же |
| коммит хода (`decideDrainTurn` в `runTurn`) | `siteCommit` | только `VRun/VNone`: queued Drain не перезапускает себя |
| CLI-цикл (`CLIScope` → `nextStep`) | `siteCLI` | `VRun` → Drain на параметрах вызова; `VDefer` → `WaitForHint(min(RetryAt, now+5s))` или выход |
| `recheckChild` (припаркованная делегация) | `siteRelease` ребёнка | запуск Drain ребёнка |

**Свойство:** каждый вердикт, означающий не закрытый долг (`VRun`, `VDefer`),
исполняется ровно одним запускателем в любой момент времени, и каждый
`VDefer` с долгом гарантирует, что хотя бы один запускатель проснётся
(recheck-набор ИЛИ `RetryAt` тика ИЛИ `WaitForHint` CLI ИЛИ живой
внешний ведущий). Нарушение — это класс R6B-1 («долг есть, а запускателя
нет») и его зеркала (два запускателя на один вердикт → двойной ход).

**Property-тест** (`internal/agent/turn_arbiter_launchers_test.go`,
model-based, без реального провайдера): модель из (сессия, долг, вердикт,
набор запланированных пробуждений); случайные последовательности событий
{commit fact, release, tick, drain end (outcome ∈ {reaction, stall, terminal,
cancel}), human message, process restart}; инварианты после каждого шага:

1. `debt ∧ verdict != VClose ∧ launchers(session) == ∅` — нарушение
   (висящий долг);
2. `count(pending drains submitted по одному вердикту) ≤ 1` — каждый
   переход арбитра `→ VRun` потребляется одним `Run`, повторный запуск
   возможен только после нового вердикта;
3. `VDefer ⇒ recheckSet ∪ {RetryAt-тик} ∪ {CLI-wait} ∪ {external driver} ≠ ∅`
   при живом процессе-хозяине.

Реализация: реальный `decide` + тонкие заглушки запускателей, считающие
подачи; `recheckPassIntervalNS`/`drainRetryAfterFailure` сжаты. Revert-check:
удалить `addToRecheckSet` из ветки paced в `wakeSession` — инвариант 1
краснеет на сценарии «tick пропустил, факт не пришёл».

## 5. План перевода

### R-ARB-1 — чистая функция + чтение фактов + тень

1. Новый файл `internal/agent/turn_arbiter.go`: `TurnFacts`, `Verdict`,
   `decide`, `readTurnFacts` (БД одним набором запросов + снимок памяти),
   `arbiterState` как **агрегат существующих четырёх map + гейта** — пока
   без переноса владельца, писатели старые.
2. Тень в тестах: табличный тест `decide` (все строки §3 + каждая находка
   из колонки «Находки» §1 как кейс) И сравнительный тест: для каждого
   фикстурного состояния фактов `decide(facts)` согласован с
   `drainPermitted`/`decideDrainTurn`/`CLIScope` (расхождения = список
   причин, не фейл, пока R-ARB-2 не перевёл). Табличный тест — оракул;
   сравнительный — страховка переноса.
3. Property-тест из §4 против тени.

### R-ARB-2 — перевод вызывающих, удаление состояния

1. Писатели арбитра: `accountDrainAttempt`, `noteDrainRefused`, сброс по
   сообщению человека, `HoldAutomaticTurns`, `suspendAutoResume`,
   `reactionChainLinkLocked` — пишут в `arbiterState`. Четыре map +
   `drainGate` удаляются после перевода всех читателей.
2. Читатели: `drainPolicy`/`drainPermitted`/`drainDecision`/`decideDrainTurn`/
   `CLIScope` сводятся к `readTurnFacts` + `decide` (+ перевод вердикта в
   локальные перечисления, где тип уже публичный — `CLIScopeState`).
3. `claimAutoResume`/`bgShellCapDeferred` удаляются; трата слота остаётся в
   точке прихода завершения (`claimAutoResumeSlot` под `bgArrival`,
   утверждено); `VRun.Counted` — описательное поле, не побочный эффект.
4. `chainGuardDeferred` остаётся только как исполнитель маркера (вставка
   уведомления — I/O, арбитр её не делает); условие — строка 5 таблицы.
5. Сборка/ветер/тесты затронутых пакетов, затем полный прогон
   `internal/agent` и `internal/app` по правилам heavy.sh.

**Существующие тесты, проверяющие удаляемое внутреннее состояние, —
переписать на наблюдаемые утверждения** (вердикт арбитра или наблюдаемый
эффект: число запросов, строка в БД, stderr CLI):

- `drain_bgcap_test.go`, `drain_bgcap_ids_test.go` — читают
  `bgShellCapDeferred`/`claimAutoResume` напрямую → вердикт + счётчик
  запусков;
- `drain_launch_gate_test.go`, `drain_gate_streaks_test.go`,
  `drain_transport_timeout_test.go`, `drain_handover_test.go`,
  `coordinator_run_drain_test.go` — читают `drainGateOpen` → вердикт;
- `coordinator_drain_cap_test.go`, `coordinator_bgshell_policy_test.go`,
  `coordinator_child_policy_durable_test.go`,
  `coordinator_external_driver_durable_test.go`,
  `coordinator_turn_hold_test.go`, `coordinator_stop_tree_test.go`,
  `coordinator_wake_reaction_debt_test.go`,
  `coordinator_reaction_chain_test.go`, `coordinator_cli_scope_test.go`,
  `coordinator_session_state_sweep_test.go` — читают вердикт, но
  выставляют внутренние map/счётчики руками → выставляют `arbiterState`
  через единственный тестовый конструктор фактов (одна точка, не
  раскиданные поля);
- `app_run_chain_guard_e2e_test.go`,
  `async_phase4_driver_scenarios_test.go` (internal/app) — e2e; остаются,
  проверяют наблюдаемое, правится только подготовка.

Ни один тест не ослабляется: e2e-утверждения (число запросов к
провайдеру, содержимое БД, `exit_reason`) переносятся как есть.

## 6. Что исчезает и что остаётся рисковым

**Классы находок, становящиеся невыразимыми:**

- «Два читателя одного гейта разошлись» (R4B-1 → R5B-1/R5B-2): читатель
  один — `decide`; `drainPermitted`/`CLIScope` не имеют собственных веток.
- «Вердикт без запускателя / запускатель без вердикта» (R6B-1 P1, R2B-17):
  свойство §4 проверяется моделью, а не перечислением путей.
- «Капа посчитана дважды / не посчитана» (R2B-16, R3B-6, (aa)): у капы один
  читатель (строка 11) и один писатель — точка прихода завершения
  (`claimAutoResumeSlot` под `bgArrival`).
- «Hint, скормленный не-фактом, открыл гейт» (R2B-1…): `HintNow` — часть
  фактов, bump — только у `siteFact`, правило 9 — явное.
- «Состояние забыто или утекло» (R3B-8): один владелец, одна уборка.
- «Отказ в допуске закрыл долг» (R2B-4/R2B-5): отказ — `VDefer` по правилу
  A1, `VClose` недостижим из сайта запуска структурно (сайт — часть фактов).

**Остаётся рисковым:**

- Пейсинг остаётся per-process: web (тик 60–120 с) и CLI root (ровно
  `RetryAt`) просыпаются по-разному — это решение дизайна попыток, не
  дефект, но тесты на тайминги хрупки.
- Гонка «факт пришёл между БД-чтением и снимком памяти»: закрыта порядком
  чтения (БД → память), но только если `readTurnFacts` не разложат обратно
  на отдельные запросы — это нужно закрепить комментарием-якорем в
  `docs/async-invariants.md` (ASYNC-02/09) в том же коммите.
- `VClose` по-прежнему требует I/O после вердикта; отказ самой записи
  закрытия — прежний путь (сон paid-серии, R3B-4).
- Правило 5 сравнивает `chainClaims` (память) с долгом (БД): после рестарта
  цепочка пуста и предохранитель разряжается — осознанное поведение,
  Documented in `coordinator_reaction_chain.go`.

## 7. Не цели

- Не менять наблюдаемое поведение: тексты, `exit_reason`, числа попыток,
  тайминги пауз — без изменений.
- Не трогать долговечный слой: ни новых миграций, ни новых колонок; новые
  SQL-запросы — только чтение, внутри `internal/session` (зона ract не
  затрагивается: классификатор активности — отдельная работа).
- Не менять CLI-цикл (`app_run_async*.go`) — зона rloop; арбитр отдаёт ему
  `CLIScopeState` в текущей форме.
- Не переносить фоновые shell в строки `async_jobs` (R-BG идёт после
  R-ARB-2); строка 11 таблицы при R-BG просто теряет условие.
- Не вводить новых флагов/таймеров «на всякий случай»: всё состояние
  арбитра — агрегат существующего; новое поле появляется только если того
  требует таблица §3, и в отчёте R-ARB-1 объясняется почему.

## 8. Открытые вопросы оператору (с рекомендацией)

1. **Долговечность суспензии/удержания.** Stop-суспензия и вопрос ребёнка
   живут в памяти; рестарт web поднимает их. Рекомендация: оставить
   in-memory и закрепить в ASYNC-02 (уже задокументировано в дизайне
   попыток §6); долговечная суспензия — новый столбец и новый писатель
   без находимого дефекта.
2. **Унификация трёх «3».** Рекомендация: не унифицировать; K/N/D —
   разные законы (закрытие долга / холостая цепочка / сон гейта), у
   каждого свои тесты.
3. **`VClose` на стороне арбитра vs учёт.** Рекомендация: вердикт — в
   арбитре (правила A5–A7), исполнение — в `accountDrainAttempt`; так
   `decide` остаётся чистым, а I/O — тестируемым по текущей схеме
   SQLite-триггеров.
4. **Порядок правил 5/6 (предохранитель над идущей делегацией).** Сегодня
   так велит R6B-1. Рекомендация: сохранить; если когда-нибудь понадобятся
   исключения — это отдельное правило-строку, а не разворот порядка.
5. **Тень в R-ARB-1: ронять ли сборку при расхождении.** Рекомендация:
   нет — расхождения фиксируются списком в отчёте R-ARB-1 и гасятся в
   R-ARB-2; это позволяет держать тень в обычных (не `-update`) тестах без
   флаков на порядок чтения.

## 9. Решения оркестратора (2026-10-01, обязательны для R-ARB-1/2)

1. **Исправление порядка таблицы §3 (ошибка дизайна).** Правило 6
   (`RunningDelegation → VRun`) в таком виде обходит гейт (правила 8–10), а
   сегодня гейт применяется ко ВСЕМ сессиям: `drainPermitted`
   (`coordinator_drain_policy.go:164`) проверяет `drainGateOpen` после любого
   `drainAllow` политики, включая ребёнка с идущей делегацией. Перенос как есть
   снял бы паузу после неудачной попытки с Drain-ходов ребёнка (горячий цикл).
   Правильный порядок: 1, 2, 3, 4, 5, 7 (выпущенный ребёнок — только без идущей
   делегации), затем гейт 8–10 для всех, затем 11–12, которые при
   `RunningDelegation` пропускаются (ребёнок под делегацией вне капы и вне
   «auto-resume off»), затем 13. Табличный тест обязан содержать кейс «ребёнок с
   идущей делегацией + оплаченная пауза → `VDefer{paced}`»; revert-check —
   поставить правило 6 перед 8.
2. **Код — оракул поведения.** Где таблица §3 и текущий код расходятся, R-ARB-1
   следует коду (рефакторинг без изменения поведения); расхождение, которое
   выглядит дефектом, — пункт отчёта, а не правка.
3. **Тень (вопрос 5):** в R-ARB-1 расхождения не роняют тесты, но перечисляются
   в отчёте; R-ARB-2 не вливается, пока список расхождений не пуст или каждое
   не объяснено как намеренное и не утверждено.
4. **Вопросы 1–4:** принять рекомендации документа (суспензия/удержание в
   памяти; три «3» не унифицировать; `VClose` решает арбитр, исполняет учёт;
   правило 5 выше 6).

## Итог R-ARB-2 (2026-10-01, ветка `rarb2`)

Перевод выполнен; поведение не менялось (оракул — код, решение §9.2).

**Состояние.** Все семь map координатора (`consecutiveAutoResumes`,
`bgShellOverCap`, `autoTurnsSuspended`, `turnHolds`, `consecutiveDrainLinks`,
`reactionChainClaims`, `reactionChainNoticed`) и гейт `drainGate` в
`workLedger` удалены; всё живёт в `arbiterState` под одним мьютексом
(`internal/agent/turn_arbiter_state.go`), одна запись на сессию. Уборка
R3B-8 одна: `sweepArbiterEntries(At)` в `RecheckPass` (идл-записи) плюс
удаление целиком для удалённых сессий (`sweepDeletedSessionState`).
`recheckMu/recheckSet` — не состояние решения, остаются.

**Читатели.** `drainPolicy`/`drainPermitted`/`drainDecision`/`decideDrainTurn`/
`CLIScope` — тонкие адаптеры над `readTurnFacts` + `decide` с отображением
вердикта в прежние перечисления (`drainVerdictOf`: held/пауза гейта → paced,
dormant → stuck, чужой ведущий → deferred+recheck, остальное → deferred без
тика; `VNone`/`VRun` → allow). `claimAutoResume`/`bgShellCapDeferred`/
`drainGateOpen`/`drainPolicy`-ветки удалены; предохранитель цепочки —
правило 5, `chainGuardDeferred` остался только исполнителем маркера
(`chainGuardMarker`).

**Трата слота.** Оставлена точкой прихода завершения
(`persistBGShellCompletion` → `claimAutoResumeSlot`, логика прежнего
`claimAutoResume` внутри арбитра) — УТВЕРЖДЕНО ревью от 2026-10-01
(`docs/reviews/2026-10-01-r-arb-2-review.md`): перенос траты на исполнение
`VRun{Counted}` сломал бы связь ASYNC-09 — завершение, пришедшее при
закрытом гейте, не тратило бы слот, и правило 11 потеряло бы over-cap
множество, которым оно измеряется. Поле `Verdict.Counted` объявлено
описательным (комментарий: повторяет сайт, исполнители не читают) — не
удалено, его читают тест правила 13 и тень. Писатель капы один
(`claimAutoResumeSlot`); после P1-3 (устранена подрезка из `readTurnFacts`)
писатель и множества over-cap тоже один — утверждение теперь верно.

**Расхождения тени (шаг 0):** две ветки делегации не были покрыты
(`TestShadow_RunningDelegationChild`, `TestShadow_ReleasedDelegationChild`);
расхождений нет (все прогоны зелёные). Одно уточнение фактов:
`DurableChild` читается и при идущей делегации (условие в `readTurnFacts`
смотрит только на `ExternallyDrivenSelf`, который в БД-половине ещё
нулевой) — на вердикт не влияет (правило 7 требует `!RunningDelegation`),
оставлено как есть.

**Тесты.** Тесты, трогавшие удалённые поля, переведены на наблюдаемое через
единственный конструктор (`internal/agent/turn_arbiter_testhelpers_test.go`:
`seedArbiterState`, `setOverCap`, `setReactionChain`, `gateOpen`,
`capDeferred`); утверждения на поведение (вердикт, число запросов, строки
БД, счётчики) не ослаблены. Тень стала тавтологией (переведённый путь сам
считает `decide`) и оставлена как регрессионный тест пути: сравнение
`decide(facts)` с `drainPermitted` сохранено, обязательные утверждения — на
слова вердикта пути.

**Известные микро-расхождения окна гонки** (два чтения раньше, одно сейчас):
`drainDecision` без долга больше не консультирует гейт (раньше мог ответить
paced на исчезнувший долг) — допустимо (решение ревью: `drainPolicy`
повторно вызывает `decide` с `PendingIncl=true`; остаток — узкое безвредное
окно). Второе («`PendingLeft=false` и непустой снимок») оказалось дефектом
P1-1 и исправлено ниже.

**Исправленные P1 ревью от 2026-10-01**
(`docs/reviews/2026-10-01-r-arb-2-review.md`):

- **P1-1. No-turn при видимом долге сбрасывал гейт.** Теперь отказ коммита
  при непустом снимке — отдельный исход `AttemptFacts.CommitRefused`
  (строка A3' таблицы §3): `decideAccount` отвечает `VNone "commit refused:
  gate untouched"`, исполнитель гейт не трогает и возвращает сессию в
  recheck-набор по `commitNo.recheck` (как старая ветка `default`). Тесты:
  `TestDecideAccount_RowsA1toA7` (случай A3'),
  `TestDrainAttempt_CommitRefusalKeepsTheGateAndRechecks` (-paced-гейт
  переживает учёт, серия цела, sid в recheckSet; отказ без recheck не
  добавляет запись).
- **P1-2. Сообщение человека стирало удержание rerun.**
  `resetForHumanMessage` сохраняет `holds`; запись удаляется только при
  holds==0. Тесты: `TestRerunHold_SurvivesTheHumanMessageReset`
  (`HoldAutomaticTurns` → `ResetAutoResumeCounter` → `drainPermitted` при
  долге остаётся paced «rerun in progress»),
  `TestRerunHold_NestedHoldsSurviveTheHumanMessageReset` (hold A → reset →
  hold B → release A не снимает B).
- **P1-3. Over-cap множество подрезалось по устаревшему чтению.**
  `readTurnFacts` теперь берёт `bgArrival` ДО чтения строк долга: и
  `BGShellNotices`, и `OverCapRows` — из ОДНОГО `PendingInclusiveDebtRows`
  (Kind там есть, `PendingInclusiveDebtSummary` из чтения фактов удалён);
  подрезка `retainOverCap` из чтения убрана (достаточно подрезки в точке
  прихода под `bgArrival`), функция удалена. Комментарий чтения и строка
  ASYNC-09 поправлены. Тесты: `TestBGShellCap_OverCapCompletionInTheSeamIsKeptAndDefers`
  (детерминированный шов `readTurnFactsGateSeam` между БД-половиной и
  гейтом: завершение сверх капы в шве сохраняет пометку, решение — defer,
  не VRun); `TestBGShellCapDeferred_BlockedByAnArrivalInFlightFailsClosed`
  оракул возвращён (гейт держится тестом, проверка блокируется ДО чтений —
  раньше тест проходил из-за отменённого ctx на первом DB-чтении);
  `TestBGShellCapDeferred_PrunesOverCapIDsThatLeftTheDebt` переписан на
  наблюдаемый вердикт (`TestBGShellCapDeferred_FollowsTheDurableDebt`) —
  он проверял удалённую читающую подрезку.

**P2/P3 ревью** — перенесены в бэклог дизайна арбитра (ниже), в этом цикле
не чинились (Review Stop Rule).

### Бэклог дизайна арбитра (P2/P3 ревью R-ARB-2, не чинить без нового P0/P1)

- P2: уборка ledger больше не держит запись при живом гейте
  (`coordinator_session_state_sweep.go`), HintSeen живёт в арбитре — после
  уборки hintSeq обнуляется, free-dormant гейт может проигнорировать первый
  факт или открыться без факта; комментарий «harmless» неверен.
- P2: гейт (8–10) теперь раньше 11/12: прежний deferred становится
  paced/stuck; в CLI вместо Deferred-выхода — ожидание до RetryAt или
  Stuck-выход с ошибкой. Буква §9.1, но противоречит §9.2/§7.
- P2: тень тавтологична — учёт и переходы состояния (reset, sweep, hold) со
  старым кодом не сравнивались; «расхождений нет» по §9.3 не обосновано.
- P3: CLIScope и decideDrainTurn не ставят маркер цепочки.
- P3: `readTurnFacts` читает всё безусловно — новые fail-closed поверхности
  для CLI-корня.
- P3: глобальный `bgArrival` на каждом решении — конкуренция между сессиями.
- P3: ветвление по строкам Reason хрупкое; причины нечитаемого входа слиты в
  одну строку.
- P3: тестовый `bumpConsecutiveResume` отказывает при суспензии и на капе.
- P3: `chainLink` плодит пустые записи арбитра.
