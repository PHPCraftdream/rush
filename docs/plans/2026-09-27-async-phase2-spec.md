# Фаза 2: единая доставка и драйвер — спецификация реализации

Статус: спецификация к утверждённому дизайну
(`docs/plans/2026-09-27-async-structured-concurrency.md`), реализация не
начата. Опирается на фазу 1, уже слитую в это дерево (коммит `90daae7e`,
`internal/agent/work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
`coordinator_work_scope.go`, плюс независимо влитую задачу #1049
`coordinator_subagent_drivers.go`). Инварианты — `docs/async-invariants.md`;
эта спецификация обязана переанкерить строки, которые меняет (§7).

Каждая строка ниже сверена с кодом на HEAD worktree `phase2-spec` (та же
точка, что и `main` на момент написания: `90daae7e`); где сверить не удалось
или проверка требует запуска — написано «не проверено», а не предположение.

**Учтён `docs/plans/2026-09-27-wake-tools-contract.md`** (закоммичен на
`main`/`fix-job-kill-by-id` ПОСЛЕ ветвления этого worktree — прочитан оттуда
напрямую, не из этого дерева). Контракт — источник истины для формы параметра
`timeout` (§5) и явно назначает этой фазе задачу #1054 (§2.5). Согласовано с
оркестратором отдельно: задача #1053 (`job_output`/`job_kill` разрешают
`job_id` в `shell_id` через новое поле `asyncJob.shellID`, устанавливаемое в
`asyncTool.awaitShell`) чинится ПАРАЛЛЕЛЬНО другим агентом — эта
спецификация её не переопределяет и не трогает `job_kill.go`/`job_output.go`;
единственная точка соприкосновения — `awaitShell`'s вызов из `t.run` (§4.3),
куда эта спецификация добавляет условие `!sync` ВОКРУГ существующего вызова,
не меняя тело `awaitShell` самого. Раздел §2 контракта (унификация
`job_id`/`shell_id`, различимый `cancelled` для `job_kill`) в остальном не
входит в объём этого документа — он назначен фазе 2 контрактом, но
реализуется задачей #1053, не этой спецификацией.

## 0. Объём фазы 2 и решения, принятые для этой спецификации

### 0.1 Что входит

Ровно пп. 1–6 задания (см. заголовок) плюс их тесты и переанкеровка
инвариантов (пп. 7–8): факт-потом-будильник, единый инициатор хода, дескриптор
вместо `(nil, nil)`, один путь исполнения `asyncTool`, явные таймауты двух
видов с единым сервисом сроков, время жизни restricted-run политики для
разбуженного дочернего хода. Плюс задача **#1054** (добавлена оркестратором
после первого черновика этого документа, см. §2.5): `coordinator.Cancel`/
`InjectMessage` не учитывают зарегистрированного драйвера дочерней сессии —
та же природа дефекта, что #1049 уже исправила для `notifyAsyncCompletion`,
но не распространила на эти два метода. Естественно ложится сюда же: §2 уже
вводит единственную точку выбора драйвера для будильника, #1054 обобщает её
на `Cancel`/`InjectMessage`.

Явные таймауты (§5) и сервис сроков дизайн-документ («Миграция», п.
«Фаза 5») и черновик фазы 1 (`docs/plans/2026-09-27-async-phase1-spec.md`
§0.1.4, §5, «Решения оркестратора» п. 3) относили к фазе 5, вместе с
`wakein`/`wakeon`/`loop`. Задание на ЭТУ спецификацию прямо требует включить
их в фазу 2 (пп. 5 задания) — это учтено как осознанное решение оператора,
данное через постановку задачи, а не как отклонение от дизайн-документа: сама
фаза 1 уже подготовила ровно то место в типах (`TimeoutSpec`/`deadline`/
`timeoutKind`), которое требовалось «для той фазы, которая их доставляет»
(§0.1.4) — этой фазой оказывается фаза 2, а не 5. Ниже используются типы из
черновика фазы 1 §1.1 дословно там, где они не противоречат тому, что фаза 1
реально реализовала (`docs/async-invariants.md`'s ASYNC-03 подтверждает: в
итоге слитый `work_job.go` их не содержит — см. §5.1 ниже, откуда они
добавляются заново).

### 0.2 Что НЕ входит

- **Учёт областей событием** (счётчик, событие «область закрыта», удаление
  цикла `descendantWorkPollInterval`/BFS) — фаза 3. `app_run_async.go`
  (`runNonInteractiveWithAsyncResults`, `descendantWorkPending`) не
  трогается.
- **Долговечность** (SQLite, host-lease, `phaseInterrupted`,
  `sessions why/list` из реестра, `descendant_liveness.go`) — фаза 4.
- **`wakein`/`wakeon`/`loop`, надзор корня** — фаза 5. Явные таймауты (§5
  ниже) — это таймаут ОДНОГО вызова инструмента, а не расписание; они не
  вводят `jobKindTimer` (зарезервирован в фазе 1's комментарии, но
  создаётся только фазой 5).
- **`agentic_fetch`** явным таймаутом не наделяется в этой фазе — задание
  (п. 5) прямо называет `bash`/`run_command`/`agent`; `agentic_fetch` остаётся
  как есть (её собственный HTTP-клиент уже несёт 30-секундный таймаут,
  `agentic_fetch_tool.go:61`, не проверено, что это то же самое по духу, но
  вне запроса на эту фазу).
- **Вложенная делегация** — структурно исключена задачей #1050 (фаза 1
  §0.1.5); эта спецификация не рассматривает `childSession` с собственным
  `childSession`.
- Дурная очередь (`coordinator_run_queue_call.go`, durable replay после
  рестарта процесса) НЕ переводится на `wakeSession` (§2) в этой фазе:
  прочитан `coordinator_run_queue_call.go` ровно настолько, чтобы увидеть, что
  её функция — восстановить ПРЕРВАННЫЙ ход (`ExistingMessageID: data.
  ExistingMessageID`, строка 227), а не доставить уведомление о завершённой
  async-задаче; она не строит `AsyncCompletion` и не читает `workLedger`. Не
  проверено исчерпывающе (не весь файл прочитан построчно), но нет
  оснований считать её источником уведомлений в смысле п. 2 задания — явно
  выведена из объёма, а не пропущена по недосмотру.

## 1. Доставка: сначала факт, потом будильник

### 1.1 Диагноз (сверено с кодом)

Сегодня (`internal/agent/coordinator_background.go:47-95`,
`notifyAsyncCompletion`) «факт» и «будильник» — ОДНО действие: уведомление
превращается в текст промпта (`FormatAsyncCompletion`) и уходит НАПРЯМУЮ в
`c.Run`/`driver.agent.Run` как `call.Prompt`. Персистентное сообщение с этим
текстом появляется только внутри вызванного хода — `createUserMessage`
(`agent_prompt.go:101-119`), которая срабатывает не раньше, чем
`sessionAgent.Run` реально получит владение мейлбоксом и дойдёт до
`agent_turn.go:417-432`. Если мейлбокс занят, `Run` СТАВИТ В ОЧЕРЕДЬ САМ
ВЫЗОВ (структуру `SessionAgentCall`, в памяти) и возвращает `(nil, nil)`
(`agent_run.go:91-109`) — до этого момента уведомление нигде не персистентно.
Если ход вообще не начинается (`checkPeakHours` в `runInternal`, истёкший
OAuth-токен без восстановления, `ErrAgentShuttingDown`, занятый
`session.SessionLock` — `agent_run.go:399-433`), уведомление НЕ ПОЯВЛЯЕТСЯ
НИГДЕ: ни в транскрипте, ни в логе кроме `slog.Debug`
(`coordinator_background.go:211`, `runAutoResumeRecovered`; `:178-181`,
`notifyBackgroundJobDone`'s `InjectMessage` failure). Это ASYNC-05
(«уведомление не опережает tool result») выполняется случайно — потому что
до персистентности вообще не доходит, а не потому что порядок гарантирован.

### 1.2 Механизм: `ExistingMessageID`, не новый API

Вопрос задания: «как ход начинается над уже персистентным уведомлением, если
`Run` требует promt». В кодовой базе уже есть ГОТОВЫЙ, используемый в
продакшене механизм именно для этого случая — `rush sessions inject
--interrupt` (`coordinator_interrupt.go:240`, `:361`): персистентное сообщение
создаётся ОТДЕЛЬНО (до и НЕЗАВИСИМО от вызова хода), затем строится
`SessionAgentCall{Prompt: <тот же текст>, ExistingMessageID: msg.ID}` и
передаётся `Run`/`InterruptAndReplace`. Приёмный конец —
`agent_turn.go:414-432`:

```go
userMessageCreated := call.ExistingMessageID != ""
if call.ExistingMessageID == "" {
    createdMsg, err := a.createUserMessage(preambleCtx, call)
    ...
}
```

— если `ExistingMessageID` задан, `createUserMessage` вообще не вызывается;
ход просто читает историю сессии (которая уже содержит эту строку) и
генерирует ответ. Это ИМЕННО то поведение, которое нужно: `Run` не создаёт
дубликат, а «начинает ход над уже персистентным уведомлением» — ретраи и
переигровки идемпотентны по построению (см. §1.3), а не по дополнительной
проверке.

Более того, есть готовый персистирующий примитив с атомарной семантикой
«если занято — слить, если свободно — просто оставить в БД»:
`sessionAgent.InjectMessage` (`agent_control.go:124-136`):

```go
func (a *sessionAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
    msg, err := a.createUserMessage(ctx, call)
    ...
    a.getMailbox(call.SessionID).injectIfBusy(msg)
    return msg, nil
}
```

`injectIfBusy` (`mailbox_inject.go:53-64`) атомарно (под `mb.mu`) проверяет
`mb.state == mbOwned` и, если да, добавляет `msg` в `mb.injects`, откуда его
заберёт `drainInjects` ТЕКУЩЕЙ активной генерации на следующем `PrepareStep`
— это и есть «слияние нескольких уведомлений в один ход» из §1.3 задания,
уже реализованное и не требующее нового кода. Если сессия свободна,
`injectIfBusy` возвращает `false`, и сообщение просто лежит в БД — это ровно
половина работы (факт), вторую половину (будильник) сегодня InjectMessage
не делает вообще (это и есть Phase-3-fallback у `notifyBackgroundJobDone`,
`coordinator_background.go:170-184`: персистентность есть, хода не будет, пока
кто-то не позовёт `Run` независимо).

**Решение**: единая функция `wakeSession` (§2) объединяет оба примитива:

1. Персистирует факт через `createUserMessage`-эквивалент, атомарно проверяя
   занятость (`injectIfBusy`-подобный путь) — если сессия занята, этим и
   заканчивается: уведомление сольётся в текущий или следующий ход этой же
   генерации, будить не нужно.
2. Если сессия свободна, СТРОИТ `SessionAgentCall{Prompt: text,
   ExistingMessageID: msg.ID, AutoResumed: true, BackgroundJobNotice: true,
   ...}` и зовёт `Run` на правильном драйвере (§2) — это и есть «будильник»,
   и он идемпотентен по построению: `ExistingMessageID` гарантирует, что
   повторный вызов `wakeSession` для УЖЕ персистентного `msg.ID` (например,
   ретрай после неудачного `Run`, см. §1.4) не создаёт вторую строку в
   транскрипте, а самое большее — второй ход над той же самой строкой.

### 1.3 Идемпотентность будильника (ключ — job id)

«Ключ — id задачи» реализуется так: `wakeSession` вызывается с `jobIdentity
{owner, toolCallID}` (уникальная пара по конструкции реестра,
`work_ledger.go:111-134`; точная форма аргумента и причина, по которой это
НЕ `*asyncJob`, — §2.4, читается вместе с этим разделом) и хранит в
`workLedger.noticedBefore` (§2.4) запись `"owner\x00toolCallID" ->
noticeMessageID` — ID персистентного сообщения, записанный ОДИН РАЗ при
первом успешном шаге 1 (§1.2). Повторный вызов `wakeSession` для ТОЙ ЖЕ
задачи (ретрай после неудачного `Run`, §1.4, или гонка нескольких триггеров)
проверяет наличие записи в `noticedBefore` и, если уже персистентно,
ПРОПУСКАЕТ шаг 1 целиком и сразу пытается шаг 2 (будильник) с уже известным
`ExistingMessageID` — так что даже параллельный/повторный вызов не может
создать вторую строку. Это ортогонально существующей идемпотентности
`workLedger.Start` (фаза 1, «повторный старт того же вызова возвращает
существующую задачу», #1038) — та защищает от повторного ЗАПУСКА исполнителя,
эта защищает от повторной ПУБЛИКАЦИИ уведомления об уже завершённом
исполнителе.

### 1.4 Неудачное пробуждение — видимое состояние, не Debug-строка (ASYNC-09)

Три места сегодня используют `slog.Debug` там, где дизайн требует видимого
состояния:

| Файл:функция | Что молчит |
|---|---|
| `coordinator_background.go:201-213` `runAutoResumeRecovered` | ход, которым разбужена сессия, вернул ошибку (`checkPeakHours`, `session lock busy`, `ErrAgentShuttingDown`, провайдерская ошибка) |
| `coordinator_background.go:170-184` `notifyBackgroundJobDone`'s Phase-3 ветка | `c.InjectMessage` вернул ошибку (сессия закрыта, БД недоступна) |
| `coordinator_work_scope.go:36-60` `refreshSubAgentCompletion` | чтение БД для обновления результата делегации перед доставкой не удалось |

Фаза 2 закрывает ПЕРВЫЕ ДВА (это и есть «неудачное пробуждение» в терминах
задания — сбой шага 2 из §1.2). Третье — не пробуждение, а «свежесть
содержимого уже доставляемого результата»; у него уже есть безопасный
фолбэк (используется зафиксированный на арминге результат), и он не входит
в объём этой фазы (не запрошено заданием; остаётся как есть, статус в
`docs/async-invariants.md`'s ASYNC-09 остаётся «частично»).

**Видимая форма**: не заводить новую таблицу/схему БД (это территория фазы
4). Вместо этого `wakeSession`, если шаг 2 (`Run`) вернул НАСТОЯЩУЮ ошибку
(не `(nil, nil)` — см. §3 про то, что `(nil, nil)`-очередь не ошибка), делает
ДВА действия синхронно, оба дёшевы и не блокируют исполнителя задачи (сама
задача уже завершена, это чисто путь доставки):

1. `slog.Warn("wake failed after notice was persisted", "session_id", ...,
   "job_id", ..., "notice_message_id", noticeMessageID, "err", err)` (значение
   — то, что `wakeSession` только что записало/нашло в `noticedBefore`, §1.3) —
   не Debug.
2. Второе персистентное сообщение в ТУ ЖЕ сессию, с тем же
   `BackgroundJobNotice`-подобным тегом (см. ниже про новый тег), текстом
   вида `"Rush could not resume this session automatically after the notice
   above (%s); the notice itself is saved — send any message to continue."`
   — это и есть «видимый маркер» из задания: он появляется в транскрипте и,
   через существующий пайплайн рендеринга системных сообщений, в веб-UI,
   без новой схемы БД. `sessions why`/programmatic-чтение такого маркера —
   не в объёме (фаза 4 даёт реестр, из которого это станет структурированным
   полем, а не текстом; текстовый маркер — переходное, но НЕ Debug-логовое
   решение, что и требуется).

Не заводится отдельное поле `wakeFailed` на `asyncJob`: к моменту, когда
`wakeSession` пытается шаг 2, задача УЖЕ терминальна и доставлена (маркер —
про сам ПРОЦЕСС доставки, не про состояние задачи в реестре) — хранить это
в `workLedger` было бы задачей без потребителя (задание не просит `sessions
why` читать это в фазе 2).

### 1.5 Новый тег сообщения

`message.CreateMessageParams` уже несёт `AutoResumed`/`BackgroundJobNotice`
(`agent_prompt.go:108-114`, `message.go` — не проверено точное имя файла со
структурой `Message`, но использование подтверждено по вызовам). Для маркера
неудачного пробуждения переиспользуется `BackgroundJobNotice: true` (тот же
рендер-путь веба, что и обычное уведомление о задаче) — НЕ заводится третий
булевый тег ради одной строки текста; отличить маркер от обычного
уведомления модель/пользователь может по содержанию текста. Если ревью найдёт
это недостаточным для веб-UI (разный визуальный стиль), это косметика веба,
не эта спецификация.

### 1.6 Типы и API

```go
// work_job.go (правка существующего файла)

```

Никакого нового поля на `asyncJob` не заводится: `wakeSession` вызывается
уже ПОСЛЕ того, как `deliverLocked` удалил живую запись задачи (§2.4) — идти
за идемпотентностью некуда, кроме отдельной, переживающей удаление, карты на
`workLedger` (`noticedBefore`, введена в §2.4 вместе с точным обоснованием
и `jobIdentity`).

```go
// coordinator_wake.go (новый файл)

// wakeSession is THE single fact-then-wake primitive (design doc §2/§3;
// task items 1-2). It persists notice as a session message (idempotent by
// l.noticedBefore[job], §2.4), merges it into a live generation if the
// session is busy, and -- only if the session is idle AND wake is true --
// starts a turn over the persisted message via ExistingMessageID. Every
// completion source (notifyAsyncCompletion, notifyBackgroundJobDone) calls
// this instead of building its own SessionAgentCall/InjectMessage/c.Run.
//
// wake=false reproduces today's Phase-3 InjectMessage behavior exactly
// (persist + merge-if-busy, never force a turn on an idle session) --
// notifyBackgroundJobDone's Phase-3 branch keeps that contract by calling
// wakeSession with wake=false instead of c.InjectMessage directly, so
// "InjectMessage used for job notices" (task item 2) also goes through this
// one function.
//
// job is a flat identity, not *asyncJob: by the time any caller can reach
// this function, deliverLocked has already deleted the live record from
// bySession[owner].jobs (work_ledger.go:179, unchanged) -- see §2.4 for why
// the idempotency key (§1.3) lives in a separate noticedBefore map instead.
//
// noticeKind, when non-empty, is persisted on the new message.NoticeKind
// field (§5.3bis) -- "" for every ordinary finish/fail/cancel notice
// (today's contract-undefined value), "timeout_wake_only"/
// "timeout_terminated" for the two timeout events §5.5 introduces.
func (c *coordinator) wakeSession(ctx context.Context, job jobIdentity, notice, noticeKind string, wake bool) error

// jobIdentity is the (owner, toolCallID) pair that survives a job's removal
// from the live ledger map -- the only handle wakeSession needs.
type jobIdentity struct {
    owner      string
    toolCallID string
}
```

### 1.7 Таблица замен (§1)

| Было | Стало | Кто вызывающий меняется |
|---|---|---|
| `notifyAsyncCompletion`'s `notify := func(ctx) {...}` (`coordinator_background.go:66-81`), прямой `c.Run`/`driver.agent.Run` | `noticeKind := ""; if completion.TimedOut { noticeKind = "timeout_terminated" }; c.wakeSession(ctx, jobIdentity{completion.SessionID, completion.ToolCallID}, FormatAsyncCompletion(completion), noticeKind, true)` (§5.5) | `coordinator_background.go` (см. §2 — тот же коммит, что и единый инициатор) |
| `notifyBackgroundJobDone`'s Phase-4 ветка (`:159-167`), `c.Run` | `c.wakeSession(ctx, jobIdentity{sessionID, sh.ID}, summary, "", true)` | то же |
| `notifyBackgroundJobDone`'s Phase-3 ветка (`:174-184`), `c.InjectMessage` | `c.wakeSession(ctx, jobIdentity{sessionID, sh.ID}, summary, "", false)` | то же |
| `runAutoResumeRecovered`'s `slog.Debug` (`:210-213`) | остаётся для КАЖДОГО прочего вызова (compaction auto-retry и т.п., если такие есть — не проверено, использует ли что-то ещё `runAutoResumeRecovered`), но путь job-уведомлений (два пункта выше) БОЛЬШЕ НЕ идёт через `runAutoResumeRecovered`: `wakeSession` сама делает `slog.Warn`+маркер (§1.4) и не нуждается в отдельном panic-recover — она не запускает провайдерский ход напрямую, это делает `Run` внутри неё, у которого своя защита | `coordinator_wake.go` |

`runAutoResumeRecovered`'s паник-recover (`coordinator_background.go:201-214`)
переносится ВНУТРЬ `wakeSession`'s собственного вызова `Run`, той же формой
(`defer recover()`), а не убирается — паника внутри хода, вызванного из
`wakeSession`, не должна убивать процесс так же, как сегодня.

## 2. Единый инициатор хода

### 2.1 Диагноз

`docs/async-invariants.md`'s ASYNC-06 фиксирует: `notifyAsyncCompletion` и
`notifyBackgroundJobDone`'s Phase-4 ветка делают `go runAutoResumeRecovered(ctx,
..., func(ctx) {...})` — горутина-колбэк напрямую зовёт `c.Run`/`driver.agent.
Run`, а не публикует событие драйверному мейлбоксу. Выбор драйвера (задача
#1049, уже влитая) уже происходит корректно внутри `notifyAsyncCompletion`
(`c.subAgentDrivers.get(completion.SessionID)`, `:69-81`) — но САМ ВЫЗОВ
по-прежнему в ad-hoc горутине конкретно этой функции, а не в единой точке.

### 2.2 Решение: `agentFor` — общая точка выбора `SessionAgent`

Выбор драйвера (`c.subAgentDrivers.get(sessionID)`, откат на `c.currentAgent`)
сегодня закодирован ВРУЧНУЮ и по отдельности в `notifyAsyncCompletion`
(`coordinator_background.go:69-81`) и (после фикса #1049) нигде больше —
`Cancel`/`InjectMessage` его вообще не используют (см. §2.5, задача #1054).
Вместо повторения этой логики в `wakeSession`, выносится ОДНА функция,
которую переиспользуют все три места:

```go
// coordinator_subagent_drivers.go, новая функция рядом с subAgentDriverRegistry

// agentFor resolves the SessionAgent that actually owns sessionID's mailbox:
// the registered delegation driver, if any (a delegated child session runs
// on its own task SessionAgent, never on c.currentAgent -- task #1049),
// else c.currentAgent for every non-delegated session. Single choke point
// for every coordinator method that dispatches a call/cancel/inject onto
// "whichever SessionAgent owns this session id" -- wakeSession (§1-2),
// Cancel, InjectMessage (§2.5, task #1054) all call this instead of each
// re-implementing the same fallback.
func (c *coordinator) agentFor(sessionID string) SessionAgent {
    if driver, ok := c.subAgentDrivers.get(sessionID); ok {
        return driver.agent
    }
    return c.currentAgent
}
```

`wakeSession` (§1.6) — единый инициатор хода: она ОДНА вызывает `c.agentFor
(sessionID)` и ОДНА вызывает `Run` на результате (для доступа к
`parentSessionID`/`call`-шаблону, нужным для переустановки allowlist, §6.2,
она дополнительно зовёт `c.subAgentDrivers.get` напрямую — `agentFor` не
скрывает полный `subAgentDriver`, только даёт `SessionAgent`). `notifyAsyncCompletion`
и `notifyBackgroundJobDone` перестают строить `SessionAgentCall`/выбирать
драйвера сами — они становятся тонкими обёртками, которые только собирают
текст уведомления и решают `wake bool` (true для Phase-4/async-completion,
false для Phase-3-fallback), а `go runAutoResumeRecovered(...)` в них
заменяется на `go func() { if err := c.wakeSession(ctx, id, text, "", wake);
err != nil { /* уже залогировано и персистировано внутри wakeSession, здесь
нечего делать */ } }()` (`id` — `jobIdentity`, §1.6) — горутина остаётся
(доставка не должна блокировать исполнителя, который её вызвал), но её
тело — ровно один вызов, не самостоятельная сборка хода.

### 2.3 Таблица замен (§2)

| Было (file:func) | Стало |
|---|---|
| `coordinator_background.go:47-95` `notifyAsyncCompletion`, строит `notify`, выбирает драйвер, зовёт `go runAutoResumeRecovered(ctx, ..., notify)` | тело сокращается до: собрать `AsyncCompletion` текст (`FormatAsyncCompletion`) и `noticeKind` (§5.5), построить `jobIdentity{completion.SessionID, completion.ToolCallID}` (см. §2.4 — почему не `*asyncJob`), `go func() { c.wakeSession(ctx, id, text, noticeKind, true) }()` |
| `coordinator_background.go:136-185` `notifyBackgroundJobDone` | Phase-4 ветка (`:140-169`) и Phase-3 ветка (`:171-184`) обе вызывают `wakeSession` (см. §1.7); `c.autoResumeEligible`/`c.bumpConsecutiveResume` — без изменений (это Phase-4 ПОЛИТИКА, не путь доставки) |

### 2.4 Открытый технический вопрос, закрытый здесь: `AsyncCompletion` vs `*asyncJob`

`notifyAsyncCompletion` сегодня получает уже собранный `AsyncCompletion` value
(из `deliverLocked`'s возврата), а НЕ указатель на `*asyncJob` (запись уже
удалена из `bySession[owner].jobs` к этому моменту — `deliverLocked` делает
`delete(s.jobs, job.toolCallID)` ДО возврата, `work_ledger.go:179`). Поэтому
`wakeSession` (§1.6) принимает не `*asyncJob`, а плоскую `jobIdentity{owner,
toolCallID}` — этого достаточно как ключа для идемпотентности (§1.3), а
персистентный `noticeMessageID` хранится ОТДЕЛЬНО от самой записи задачи —
на мьютекс-защищённой карте НА САМОМ `workLedger`
(`bySession`-подобной, но переживающей удаление job из `jobs`, т.к. нужна
именно ПОСЛЕ доставки, не до):

```go
// work_ledger.go (правка): добавить рядом с bySession
noticedBefore map[string]string // "owner\x00toolCallID" -> persisted notice message id; grows unboundedly across the process's life (see §7 risk note, same growth order as subAgentDriverRegistry) until phase 4's durable ledger replaces the whole map with a DB column.
```

Это НЕ добавляет второй мьютекс — читается/пишется под тем же `l.mu`, что и
`bySession`, из `wakeSession` (которая держит ссылку на `l *workLedger` через
`c.asyncJobs`). Обоснование неограниченного роста — то же, что фаза 1 уже
приняла для `subAgentDriverRegistry` (`coordinator_subagent_drivers.go:52-61`):
одна запись на когда-либо доставленную задачу, тот же порядок роста, что и
`coordinator.agents`/таблицы сессий/сообщений, ни одна из которых не
подчищается в памяти. Фаза 4 заменяет карту на колонку в БД (та же миграция,
что incorporates `deadline`/`timeoutKind`, §5).

### 2.5 Задача #1054: `Cancel`/`InjectMessage` становятся driver-aware

**Диагноз (сверено с кодом, независимо подтверждено
`docs/plans/2026-09-27-wake-tools-contract.md` §4.4 — та же находка, там же
формализован требуемый фикс байт-в-байт с решением ниже).**
`coordinator_interrupt.go:41-52,625-641`:

```go
func (c *coordinator) Cancel(sessionID string) {
    if c.asyncJobs != nil {
        c.asyncJobs.cancelSession(sessionID)
    }
    c.currentAgent.Cancel(sessionID) // <-- всегда coder-агент, никогда драйвер
}

func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
    ...
    return c.currentAgent.InjectMessage(ctx, call) // <-- то же самое
}
```

Для делегированного `child_session_id` реальная генерация выполняется на
ОТДЕЛЬНОМ `*sessionAgent` (зарегистрированном `coordinator_subagents.go:258`
в `c.subAgentDrivers`), не на `c.currentAgent` — `c.currentAgent.Cancel
(childID)` находит пустой, никогда не тронутый мейлбокс для этого id и
молча не делает ничего (не ошибка, не лог — `agent_control.go:35-46`'s
`genCancel == nil` ветка просто не срабатывает); `c.currentAgent.InjectMessage
(ctx, call)` персистирует сообщение корректно (запись в БД сессионно-
агностична), но `injectIfBusy`'s проверка занятости идёт по ЧУЖОМУ мейлбоксу,
который всегда `mbIdle` для этого id — слияние в текущий ход ребёнка (§1.3)
никогда не срабатывает для сообщений, отправленных через `Cancel`/
`InjectMessage` этим путём. Сегодня это недостижимо в продакшене (ни один
существующий вызывающий не зовёт `Cancel`/`InjectMessage` с
`child_session_id` — находка контракта, не регрессия), но БЛОКИРУЕТ будущие
`stop_agent`/`inject_agent` (план пробуждений, этап 3) до исправления —
контракт прямо называет это предусловием.

**Решение — тем же коммитом, что и §2.2's `agentFor`:**

```go
// coordinator_interrupt.go, правка Cancel
func (c *coordinator) Cancel(sessionID string) {
    if c.asyncJobs != nil {
        c.asyncJobs.cancelSession(sessionID)
    }
    c.agentFor(sessionID).Cancel(sessionID)
}

// coordinator_interrupt.go, правка InjectMessage
func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
    if err := c.readyWg.Wait(); err != nil {
        return message.Message{}, err
    }
    pinned, err := c.resolveSessionModels(ctx, sessionID)
    if err != nil {
        return message.Message{}, fmt.Errorf("failed to resolve session models for inject: %w", err)
    }
    call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
    if err != nil {
        return message.Message{}, err
    }
    return c.agentFor(sessionID).InjectMessage(ctx, call)
}
```

Поведение для НЕ-делегированных сессий (нет зарегистрированного драйвера) не
меняется: `agentFor` откатывается на `c.currentAgent` байт-в-байт. Изменение
затрагивает ТОЛЬКО путь, для которого сегодня нет ни одного продакшн-
вызывающего — безопасное расширение, не хотфикс живого пути.

**Не входит в этот шаг** (по прямому ограничению задания — только `Cancel`/
`InjectMessage`, ничего сверх): `InterruptAndReplace`, `ActiveCall`,
`ActiveCallState`, `IsSessionBusy`, `IsBusy` — эти координаторские методы
тоже читают `c.currentAgent` напрямую (не проверено исчерпывающе, все ли
из них подвержены той же природе бага), но не были названы ни заданием, ни
контрактом; включать их сюда было бы расширением объёма без прямого запроса.
Отмечено как вероятный follow-up, не как забытая часть этой задачи.

**Тесты:**

- `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent` — зарегистрировать
  фейковый драйвер для `childID`, занять его мейлбокс живой генерацией
  (шпион-`SessionAgent`, `Cancel` записывает вызов), вызвать
  `c.Cancel(childID)`, убедиться что вызван драйверный `Cancel`, а не
  `c.currentAgent.Cancel`. *Revert-check:* вернуть `c.currentAgent.Cancel
  (sessionID)` — тест обязан показать, что драйверный `Cancel` НЕ вызван
  (счётчик 0).
- `TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent` —
  аналогично для `InjectMessage`, проверяя что персистентное сообщение
  сливается (`injectIfBusy` возвращает `true`) в ЗАНЯТЫЙ драйверный
  мейлбокс, а не в идле `c.currentAgent`'s.
- Чёрный тест (контракт §10, `stop_agent`'s revert-check): временно вернуть
  `Cancel` к прежней форме — тест «`stop_agent` реально останавливает
  генерацию ребёнка» (когда фаза, реализующая `stop_agent`, добавит его)
  обязан упасть тихим no-op'ом; этот чёрный тест сам по себе НЕ входит в
  объём фазы 2 (у неё ещё нет инструмента `stop_agent`), но приведён здесь
  как критерий, которым фаза 3 плана пробуждений подтвердит, что фикс #1054
  действительно устраняет блокировку.

## 3. Очередь — не `(nil, nil)`

### 3.1 Диагноз (сверено с кодом)

`sessionAgent.Run` (`agent_run.go:91-109`):

```go
became, epoch := a.tryReserveSession(call, runCancel)
if !became {
    if call.FailIfSessionBusy {
        return nil, fmt.Errorf(...ErrSessionBusy)
    }
    if adm != nil {
        adm.markQueued()
    }
    return nil, nil
}
```

`(nil, nil)` — единственный сигнал «поставлено в очередь» для НЕ-fail-fast
вызовов. Внутри `coordinator_run.go`'s `runInternal` уже ЕСТЬ механизм
различения (`turnAdmission`, `coordinator_turn_admission.go`, R7-1) — но он
СТРОГО ВНУТРЕННИЙ: `attemptAdmission.wasQueued()` управляет только
классификацией ретраев ВНУТРИ `runInternal` (`coordinator_run.go:529,601`);
наружу, к вызывающему `c.Run`, по-прежнему уходит `(nil, nil)` без разбора.

**Подтверждённый баг (#1036)**: `runSubAgent` (`coordinator_subagents.go:260-275`):

```go
run := func() (*fantasy.AgentResult, error) {
    return params.Agent.Run(ctx, SessionAgentCall{...})
}
...
result, runErr = run()
...
output := subAgentOutput(result)   // "" if result == nil
if output == "" {
    return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
}
```

Если дочерний мейлбокс занят (например, `notifyAsyncCompletion` только что
разбудила его по завершению собственной async-команды ребёнка, а родитель
СИНХРОННО вызывает `agent(resume_session_id=X, ...)` секундой позже),
`params.Agent.Run` вернёт `(nil, nil)` — не потому что ребёнок ничего не
сказал, а потому что его ход ЕЩЁ НЕ НАЧАЛСЯ. `subAgentOutput(nil)` → `""` →
родитель получает ОШИБКУ «sub-agent completed but produced no text output»
про ход, который не то что не завершился — не начинался.

### 3.2 Решение: блокирующий помощник для «должен знать» вызывающих

Не меняется: публичная сигнатура `Coordinator.Run`/`sessionAgent.Run`
(`(*fantasy.AgentResult, error)`, `(nil, nil)` при очереди) — CLI-цикл
(`app_run_async.go`) и веб-обработчики продолжают получать `(nil, nil)`
ровно как сегодня; они не читают результат синхронно, а либо не вызывают
`Run` напрямую вовсе (веб — через `ExecuteRun`/очередь сообщений, не
рассмотрено здесь как не требующее изменений — задание прямо говорит «не
менять, если не доказано, что нужно», а нужды не найдено: веб уже
воспринимает свою отправку как fire-and-forget), либо (CLI) вызывают
`Run` НЕ напрямую через `sessionAgent`, а через `app.ExecuteRun`, которая не
зависит от `(nil, nil)` иначе, чем сегодня.

Добавляется НОВЫЙ, аддитивный механизм для внутренних вызывающих, которым
нужен реальный исход:

```go
// agent.go (правка SessionAgentCall — рядом с OnAssistantMessageCreated)

// onQueueResolved, if non-nil, is invoked exactly once with the outcome of
// THIS specific call's own turn -- whether it ran immediately or was queued
// behind a busy mailbox and executed later by runOwned's own dispatch loop
// (agent_run.go's `for` loop, one call per iteration). Unlike
// OnAssistantMessageCreated (fires per assistant row, mid-turn), this fires
// once, after the turn this exact call became has fully returned. nil for
// every existing caller -- added for coordinator.runAwaitingAdmission
// (#1036; task item 3), not a general-purpose callback.
// json:"-": in-process only, never durable-queue-persisted (same rationale
// as OnUserMessageCreated/OnAssistantMessageCreated).
onQueueResolved func(*fantasy.AgentResult, error) `json:"-"`
```

```go
// agent_run.go (правка runOwned, внутри цикла `for`, сразу после
// result, next, hasNext, err := a.runTurn(...) и до `if !hasNext {...}`)

if call.onQueueResolved != nil {
    call.onQueueResolved(result, err)
}
```

Это безопасно для КАЖДОГО существующего вызывающего: поле `nil` для всех, кто
его не устанавливает (весь текущий код), значит условие никогда не
срабатывает вне нового пути. `SessionAgentCall` уже копируется по значению
через `mb.submitted`/`mb.queue`/`popFirstSubmitted`/`abandonOwnershipAndPop*`
(`mailbox_queue.go`, сверено полностью) — значение поля-функции переживает
постановку в очередь и извлечение оттуда БЕЗ дополнительного кода: тот же
механизм, что уже несёт `OnAssistantMessageCreated`/`OnUserMessageCreated`
через тот же путь.

```go
// coordinator_run_await.go (новый файл)

// runAwaitingAdmission runs call on agent and, if THIS invocation queues
// the call instead of executing it (mailbox busy, non-fail-fast), blocks
// (bounded by ctx) until the queued call's own eventual turn -- run by
// whichever goroutine drains the mailbox -- resolves, and returns THAT
// turn's outcome instead of (nil, nil). Blocking (not a returned future) is
// deliberate: every existing caller that needs this (runSubAgent) already
// calls Run synchronously and immediately consumes *fantasy.AgentResult, so
// a blocking helper needs no restructuring of that code, and per the
// design doc's own structured-concurrency principle, waiting on live work
// is correct behavior here, not a hang -- the queued turn WILL end.
//
// queued reports whether this call's own turn was admitted immediately
// (false) or had to wait behind another owner (true) -- callers that only
// need OBSERVABILITY (wakeSession's failure-visibility bookkeeping, §1.4)
// read this without blocking by NOT calling this helper at all: they call
// agent.Run directly with a turnAdmission armed on ctx and check
// wasQueued() themselves (see §3.3) -- queued is an EXPECTED, non-failure
// outcome for a wake (the notice is already durable, §1), so there is
// nothing to await there.
func (c *coordinator) runAwaitingAdmission(ctx context.Context, agent SessionAgent, call SessionAgentCall) (result *fantasy.AgentResult, err error, queued bool) {
    type outcome struct {
        result *fantasy.AgentResult
        err    error
    }
    done := make(chan outcome, 1)
    call.onQueueResolved = func(r *fantasy.AgentResult, e error) {
        select {
        case done <- outcome{r, e}:
        default:
        }
    }
    admission := newTurnAdmission()
    result, err = agent.Run(withTurnAdmission(ctx, admission), call)
    if !(result == nil && err == nil && admission.wasQueued()) {
        return result, err, false
    }
    select {
    case o := <-done:
        return o.result, o.err, true
    case <-ctx.Done():
        return nil, ctx.Err(), true
    }
}
```

### 3.3 Применение: `runSubAgent`

```go
// coordinator_subagents.go, правка run (было :260-275)
run := func() (*fantasy.AgentResult, error) {
    result, err, _ := c.runAwaitingAdmission(ctx, params.Agent, SessionAgentCall{
        SessionID:        session.ID,
        Prompt:           params.Prompt,
        ...
    })
    return result, err
}
```

Остальное тело `runSubAgent` (стоимость, `AwaitingAnswerError`,
`subAgentOutput`) не меняется — `runAwaitingAdmission` возвращает ТОТ ЖЕ
типизированный результат, только теперь никогда `(nil, nil)` для
действительно поставленного в очередь, а не «пустого», ответа.

### 3.4 `notifyAsyncCompletion`/`wakeSession` — только наблюдение, не ожидание

Как обосновано в §3.2's doc-комментарии `queued`: `wakeSession`'s шаг 2 (§1.2)
НЕ вызывает `runAwaitingAdmission` — она вызывает `agent.Run` напрямую с
армированным `turnAdmission`, чтобы РАЗЛИЧИТЬ три исхода для целей §1.4
(видимая ошибка), а не чтобы дождаться результата:

```go
admission := newTurnAdmission()
result, err := driverAgent.Run(withTurnAdmission(ctx, admission), wakeCall)
switch {
case err != nil:
    // настоящая ошибка (§1.4): Warn + маркер
case admission.wasQueued():
    // ожидаемо: уведомление уже персистентно (§1), сольётся в текущий
    // или следующий ход этой же генерации через drainInjects/очередь —
    // ничего дополнительно делать не нужно, это не ошибка
default:
    // ход выполнен непосредственно этим вызовом
}
```

### 3.5 Тесты

- `TestRunAwaitingAdmission_ReturnsQueuedTurnsResult` — фейковый `SessionAgent`,
  первый `Run` занимает мейлбокс (блокируется на канале), второй (через
  `runAwaitingAdmission`) должен вернуть `(nil, nil, ...)` до разблокировки,
  затем после того как первый ход завершается и извлекает второй `call` из
  очереди (тестовый фейк должен САМ вызвать `call.onQueueResolved`, раз
  тестовый `SessionAgent` — не настоящий `sessionAgent`) —
  `runAwaitingAdmission` обязана вернуть РЕАЛЬНЫЙ результат, не `(nil, nil)`.
  *Revert-check:* убрать блокирующий `select` (вернуть `(nil, nil, true)`
  сразу на `wasQueued()`) — тест обязан упасть на несовпадении результата.
- `TestRunAwaitingAdmission_ImmediateRunPassesThrough` — идле-мейлбокс,
  `queued=false`, результат ровно тот, что вернул `agent.Run`.
  *Revert-check:* всегда возвращать `queued=true`/блокироваться — тест ловит
  зависание (таймаут теста).
- `TestRunSubAgent_ResumeOfBusyChildDoesNotReportNoOutput` (`internal/agent`,
  порт сценария #1036: занять дочерний мейлбокс фейковым долгим ходом,
  вызвать `runSubAgent` с `ResumeSessionID`, убедиться что ответ — НЕ
  `"Sub-agent completed but produced no text output."`, а реальный текст
  после того как долгий ход завершается). *Revert-check:* вернуть `run` к
  прямому `params.Agent.Run(ctx, ...)` — тест обязан упасть с текстом
  "no text output".
- Существующие тесты `coordinator_run.go`'s R7-1 (`coordinator_retry_admission_test.go`)
  не редактируются — `turnAdmission`/`wasQueued` не меняют поведение,
  используются как есть.

## 4. Один путь исполнения в `asyncTool`

### 4.1 Диагноз (сверено с кодом)

`async_tool.go:42-46`:

```go
func (t *asyncTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    origin := CallOriginFrom(ctx)
    if origin != message.OriginCLI && origin != message.OriginWeb {
        return t.inner.Run(ctx, call)
    }
```

Исполняется синхронно, БЕЗ реестра, БЕЗ CAS, БЕЗ явного таймаута (§5) для
`message.OriginSDK` и `message.OriginUnspecified` (`internal/message/
content.go:35-38` — ровно четыре значения `Origin`, других нет). Именно этот
разрыв — причина #1037 («лимит 45 минут действует только в синхронной
ветке» относится к СТАРОМУ смыслу «синхронная = не через реестр»; после
фазы 2 «синхронная» означает «тот же реестр, тот же таймаут, доставка —
внутри хода»).

### 4.2 Решение: `sync bool`, не отдельная ветка

```go
// async_tool.go, правка Run
func (t *asyncTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    origin := CallOriginFrom(ctx)
    sync := origin != message.OriginCLI && origin != message.OriginWeb
    sessionID := tools.GetSessionFromContext(ctx)
    if sessionID == "" || call.ID == "" {
        return fantasy.ToolResponse{}, fmt.Errorf("async %s requires session and tool call IDs", t.name)
    }
    childSessionID, err := t.childSessionID(ctx, sessionID, call)
    if err != nil {
        return fantasy.NewTextErrorResponse(err.Error()), nil
    }
    timeoutSpec, err := parseTimeoutParam(t.name, call.Input) // §5.2
    if err != nil {
        return fantasy.NewTextErrorResponse(err.Error()), nil
    }
    // Only CLI/web jobs are detached from the triggering turn's ctx: they
    // must survive the turn that started them ending (their result is
    // delivered LATER, as a notice -- §1). A sync job has no "deliver
    // later" path at all (its only consumer is this same call, blocked
    // below in awaitAndFinish) -- cancelling the caller's ctx must cancel
    // it too, exactly like the pre-async-wrapper t.inner.Run(ctx, call)
    // did. Detaching it anyway would leak a goroutine and a ledger entry
    // nobody ever collects once the caller gives up (see §4.4).
    var jobCtx context.Context
    var cancel context.CancelFunc
    if sync {
        jobCtx, cancel = context.WithCancel(ctx)
    } else {
        jobCtx, cancel = context.WithCancel(context.WithoutCancel(ctx))
    }
    job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
    if err != nil {
        cancel()
        return fantasy.NewTextErrorResponse(err.Error()), nil
    }
    if existing {
        cancel()
        if sync {
            return t.awaitAndFinish(ctx, job)
        }
        return t.startedResponse(call.ID, job.childSession), nil
    }
    if childSessionID != "" && t.coordinator.permissions != nil {
        t.coordinator.permissions.InheritSessionAutoApprove(sessionID, childSessionID)
        if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
            mgr.InheritSessionRunAllowlist(sessionID, childSessionID)
        }
    }
    go t.run(jobCtx, cancel, sessionID, childSessionID, call, sync)
    if sync {
        return t.awaitAndFinish(ctx, job)
    }
    return t.startedResponse(call.ID, childSessionID), nil
}
```

### 4.3 `t.run`: bash не должен незаметно перейти в фон для sync-вызовов

**Найдено при проверке «ничего внешне видимого не меняется» (задание §4
прямо этого требует).** `t.run` (`async_tool.go:116-153`) для
`t.name == tools.BashToolName` СЕГОДНЯ безусловно принудительно
устанавливает `run_in_background: true` в JSON-параметрах (`:131-140`) и
после `t.inner.Run` дожидается shell через `t.awaitShell`
(`:149-151`,`:178-203`), которая форматирует итог через
`backgroundJobSummary` (`coordinator_background.go:119-127`) — СОВЕРШЕННО
ИНОЙ текстовый формат, чем обычный синхронный ответ `bash`-инструмента
(`BashResponseMetadata`, `tools/bash.go:44-52`, свой формат вывода). Если
безусловно завернуть SDK-вызовы в ЭТУ ЖЕ `t.run`, синхронный `bash` для
`OriginSDK` начнёт возвращать текст в формате `"Background job %s (...)
finished: exit %d, ran %s.\n\n%s"` вместо своего обычного форматированного
ответа — это и есть «externally visible change», которого задание требует
избежать.

**Решение**: `t.run` получает пятый параметр `sync bool` и форк только для
`bash`:

```go
func (t *asyncTool) run(ctx context.Context, cancel context.CancelFunc, sessionID, childSessionID string, call fantasy.ToolCall, sync bool) {
    defer cancel()
    ...
    completion := AsyncCompletion{SessionID: sessionID, ToolCallID: call.ID, ToolName: t.name}
    defer func() {
        if recovered := recover(); recovered != nil { ... }
        t.finalize(ctx, sessionID, childSessionID, completion, sync)
    }()
    if t.name == tools.BashToolName && !sync {
        // unchanged: force run_in_background, so awaitShell below can
        // watch it through BackgroundShellManager -- this is what lets a
        // CLI/web bash job survive the triggering turn ending (§4.2's jobCtx
        // detachment). A sync caller's ctx already spans the whole wait, so
        // there is nothing to survive past -- the underlying bash tool's
        // OWN synchronous exec path already blocks correctly.
        ctx = tools.WithoutBackgroundCallback(ctx)
        var params map[string]json.RawMessage
        if json.Unmarshal([]byte(call.Input), &params) == nil && params != nil {
            params["run_in_background"] = json.RawMessage("true")
            if input, err := json.Marshal(params); err == nil {
                call.Input = string(input)
            }
        }
    }
    response, err := t.inner.Run(ctx, call)
    if err != nil {
        completion.IsError = true
        completion.Content = err.Error()
        return
    }
    completion.IsError = response.IsError
    completion.Content = response.Content
    if t.name == tools.BashToolName && !sync {
        t.awaitShell(ctx, sessionID, response, &completion)
    }
    completion.Content = tools.TruncateOutput(strings.TrimSpace(completion.Content))
}
```

Эффект для `sync=true` `bash`: `t.inner.Run(ctx, call)` вызывается РОВНО как
раньше (тот же `call.Input`, без принудительного фона), и его собственный
`ToolResponse` становится `completion` без переформатирования — байт-в-байт
то же содержимое, что раньше возвращал прямой `t.inner.Run(ctx, call)` из
удалённой ветки origin. `run_command`/`agent`/`agentic_fetch` не имеют этой
принудительной ветки сегодня (проверено — `if t.name == tools.BashToolName`
единственное место, где `t.run` трогает `call.Input`), так что для них
`sync`/`!sync` в `t.run` ничем не отличаются — риск только у `bash`.

### 4.4 `finalize`, `deliverLocked` и синхронное ожидание

```go
// async_tool.go, правка finalize — добавлен sync bool
func (t *asyncTool) finalize(_ context.Context, _, childSessionID string, completion AsyncCompletion, sync bool) {
    if t.coordinator == nil || t.coordinator.asyncJobs == nil {
        return
    }
    result := jobResult{content: completion.Content, isError: completion.IsError}
    if childSessionID != "" && (t.name == AgentToolName || t.name == tools.AgenticFetchToolName) {
        t.coordinator.asyncJobs.armDelegation(completion.SessionID, completion.ToolCallID, result)
        return
    }
    t.coordinator.asyncJobs.finish(completion.SessionID, completion.ToolCallID, result)
}
```

`finalize` сам не знает про `sync` семантику доставки — это уже решается
внутри `deliverLocked` (единая точка, вызываемая и `finish`, и
`recheckChild`, фаза 1's дизайн не нарушается):

```go
// work_job.go, правка asyncJob — добавлено поле
type asyncJob struct {
    // ... existing fields ...
    sync bool          // true for a non-CLI/non-Web origin: this job's ONLY
                        // consumer is the goroutine blocked in awaitSync
                        // below; it is never routed to the ready queue or
                        // onWebDone.
    done chan struct{} // non-nil only when sync; closed exactly once, by
                        // deliverLocked, when this job's outcome is ready
                        // for awaitSync to read.
}
```

```go
// work_ledger.go, правка Start — добавлены sync, timeout параметры (см. §5
// для timeout; announced=sync closes the ack-gate immediately for a sync
// job, since there is no separate "started" tool result to wait for -- the
// FINAL response IS this call's only tool result, exactly like any ordinary
// synchronous tool, so ASYNC-05 has nothing to protect here)
func (l *workLedger) Start(owner, toolCallID, input, toolName, childSession string, cli, sync bool, timeout *TimeoutSpec, cancel context.CancelFunc) (*asyncJob, bool, error) {
    ...
    job := &asyncJob{
        owner: owner, toolCallID: toolCallID, input: input, toolName: toolName,
        childSession: childSession, cli: cli, cancel: cancel,
        sync: sync, announced: sync,
    }
    if sync {
        job.done = make(chan struct{})
    }
    if timeout != nil {
        job.deadline = timeout.Deadline
        job.timeoutKind = timeout.Kind
        job.timeoutSeconds = timeout.Seconds
        l.timeouts.arm(job) // §5.4
    }
    s.jobs[toolCallID] = job
    signalWorkSession(s)
    return job, false, nil
}
```

```go
// work_ledger.go, правка deliverLocked — sync short-circuit ПЕРЕД
// вычислением routing (ready queue / onWebDone)
func (l *workLedger) deliverLocked(owner string, job *asyncJob) (AsyncCompletion, bool) {
    s := l.bySession[owner]
    if s == nil {
        return AsyncCompletion{}, false
    }
    current, present := s.jobs[job.toolCallID]
    if !present || current != job || !job.state.terminal() || !job.announced {
        return AsyncCompletion{}, false
    }
    if job.sync {
        delete(s.jobs, job.toolCallID)
        if job.childSession != "" {
            l.gcChildLocked(job.childSession)
        }
        close(job.done)
        return AsyncCompletion{}, false // no ready-queue entry, no onWebDone callback
    }
    completion := AsyncCompletion{
        SessionID: owner, ToolCallID: job.toolCallID, ToolName: job.toolName,
        Content: job.result.content, IsError: job.result.isError,
        TimedOut: job.state == phaseTimedOut, // §5.5: selects the contract's timeout wording/NoticeKind in notifyAsyncCompletion instead of the generic finished/failed one
        TimeoutSeconds: job.timeoutSeconds,     // quoted verbatim in the contract's "timed out after {seconds}s" text (FormatAsyncCompletion below); zero/unused when !TimedOut
    }
    delete(s.jobs, job.toolCallID)
    if job.childSession != "" {
        l.gcChildLocked(job.childSession)
    }
    queued := job.cli && (s.drained || l.onWebDone == nil)
    if queued {
        s.ready = append(s.ready, completion)
    }
    signalWorkSession(s)
    completion.cli = job.cli
    return completion, !queued && l.onWebDone != nil
}
```

(Тело после sync-ветки — то же самое, что фаза 1 уже реализовала
(`work_ledger.go:172-189`), с ОДНОЙ добавленной строкой — `TimedOut:
job.state == phaseTimedOut`; маршрутизация `queued`/`onWebDone` не меняется.)

```go
// work_ledger.go, новый метод
// awaitSync blocks until job's outcome is ready (deliverLocked closed
// job.done) or ctx is done. job.sync must be true. Reads job.result under
// l.mu -- deliverLocked writes it (via transitionToTerminal) under the same
// lock before closing done, so there is no torn read.
func (l *workLedger) awaitSync(ctx context.Context, job *asyncJob) (jobResult, error) {
    select {
    case <-job.done:
        l.mu.Lock()
        res := job.result
        l.mu.Unlock()
        return res, nil
    case <-ctx.Done():
        // The job's own executor context is EITHER ctx itself (sync path,
        // §4.2) or derived from it -- cancelling ctx already cancels the
        // executor, which will reach finalize/deliverLocked on its own and
        // eventually close job.done. This branch does not need to cancel
        // anything itself; it only stops THIS caller from waiting further.
        return jobResult{}, ctx.Err()
    }
}
```

```go
// async_tool.go, новый метод
func (t *asyncTool) awaitAndFinish(ctx context.Context, job *asyncJob) (fantasy.ToolResponse, error) {
    result, err := t.coordinator.asyncJobs.awaitSync(ctx, job)
    if err != nil {
        return fantasy.ToolResponse{}, err
    }
    if result.isError {
        return fantasy.NewTextErrorResponse(result.content), nil
    }
    return fantasy.NewTextResponse(result.content), nil
}
```

### 4.5 Проверка по каждому origin

| Origin | До фазы 2 | После фазы 2 | Изменилось внешне? |
|---|---|---|---|
| `OriginCLI` | async: `startedResponse`, доставка через ready-queue/`wakeSession` | не меняется (тот же путь, `sync=false`) | нет |
| `OriginWeb` | async: `startedResponse`, доставка через `onWebDone` | не меняется (`sync=false`) | нет |
| `OriginSDK` | синхронно, `t.inner.Run(ctx, call)` напрямую | через реестр: `Start(sync=true)` → горутина `t.run` (bash: без принудительного фона, §4.3) → `awaitAndFinish` блокирует до `job.done` | контент/ошибка ToolResponse — байт-в-байт та же (§4.3); タайминг — раньше блокировка была внутри `t.inner.Run`, теперь блокировка в `awaitSync`, разница непринципиальна (то же дерево вызовов, тот же контекст) |
| `OriginUnspecified` | то же, что SDK (`origin != CLI && != Web`) | то же, что SDK выше | нет |

### 4.6 Тесты

- `TestAsyncTool_SDKOriginBlocksAndReturnsInnerResponse` — origin=SDK,
  `t.inner` — фейковый инструмент с задержкой; проверить что `Run`
  БЛОКИРУЕТСЯ до завершения фейка и возвращает РОВНО его `ToolResponse`
  (включая `Metadata`, если есть). *Revert-check:* убрать `sync`-разбор,
  вернуть старую раннюю ветку `if origin != CLI && != Web { return t.inner.
  Run(ctx, call) }` — тест всё ещё проходит (не различает пути) — ЭТОТ тест
  один не доказывает регресс; см. следующий.
- `TestAsyncTool_SDKOriginBashDoesNotForceBackground` — origin=SDK,
  `t.name=="bash"`, `t.inner` — шпион, записывающий полученный `call.Input`;
  проверить что `run_in_background` НЕ появился/не был перезаписан в
  `true`, если модель его не передавала. *Revert-check:* убрать `!sync`
  условие вокруг форс-фона в `t.run` — тест обязан упасть (input содержит
  `"run_in_background":true`).
- `TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout` — origin=SDK,
  проверить что во время выполнения `t.coordinator.asyncJobs.running(sessionID)`
  истинно (задача реально в реестре, не побочный путь), и что `deliverLocked`
  для нее не создаёт запись в `s.ready`/не зовёт `onWebDone` (шпион на
  `onWebDone`, счётчик вызовов 0).
- `TestAsyncTool_SyncCallerCtxCancelUnblocksAwait` — отменить `ctx`,
  переданный в `Run`, пока фейковый `t.inner` ещё выполняется; убедиться что
  `Run` возвращается быстро с ошибкой отмены, а не висит до завершения
  фейка. *Revert-check:* вернуть безусловный `context.WithoutCancel(ctx)`
  для `jobCtx` (убрать разбор `sync` в §4.2) — тест обязан зависнуть/не
  уложиться в таймаут теста.
- Существующие `TestAsyncToolReturnsBeforeCommandFinishes`,
  `TestAsyncToolWebCompletionWaitsForToolResult` (`async_tool_test.go`) —
  не редактируются, origin CLI/web, `sync=false` путь не тронут по
  поведению.

## 5. Явные таймауты (#1037), два вида

### 5.1 Типы (форма зафиксирована `docs/plans/2026-09-27-wake-tools-contract.md`
§3, поверх чернового дизайна фазы 1 §1.1)

**Выравнивание с контрактом (изменяет более ранний черновик этого раздела):**
имя параметра — `timeout`, не `async_timeout`; вид — `kind`, не `on_timeout`.
Значения `kind` те же (`"wake_only"`/`"terminate_and_wake"`). Контракт также
явно требует унификации с легаси `run_command.timeout_seconds` (§5.2 ниже) —
черновик, ранее считавший эти два поля независимыми осями вне объёма фазы 2,
устарел: контракт делает их одной осью с явным правилом конфликта.

```go
// work_job.go, правка

type jobPhase int32

const (
    phaseRunning jobPhase = iota
    phaseCompleted
    phaseFailed
    phaseCancelled
    phaseTimedOut // NEW: has a production caller now (fireTimeout, §5.4) — unlike phase 1's draft, which left this uncalled and therefore unbuilt
)

// timeoutKind is the explicit, no-default kind chosen PER CALL (operator
// decision, docs/plans/2026-09-24-agent-wakes-and-async-job-control.md,
// "Решено 2026-09-27" item 4; wire values fixed by
// docs/plans/2026-09-27-wake-tools-contract.md §3). timeoutNone is the zero
// value: "no timeout" requires no special-casing at any call site.
type timeoutKind uint8

const (
    timeoutNone timeoutKind = iota
    timeoutWakeOnly         // "wake_only": deadline passed -> job keeps running; owner gets a NON-terminal "time's up, still running" event with a summary
    timeoutTerminateAndWake // "terminate_and_wake": deadline passed -> job -> phaseTimedOut (terminal), partial output kept, owner gets the normal terminal notice
)

// TimeoutSpec is an optional, always-explicit per-call deadline. nil means
// "no ledger-level timeout" -- the existing 45-minute tool watchdog
// (agent.go's toolExecutionMaxDefault/toolMaxDuration) is a SEPARATE,
// unrelated mechanism and is not touched by this type. Seconds is kept
// alongside Deadline (redundant with it, Deadline == now+Seconds at
// construction) purely so the eventual notice text (contract §5.1: "timed
// out after {seconds}s") can quote the ORIGINALLY REQUESTED duration
// instead of a recomputed, possibly-off-by-scheduling-jitter value.
type TimeoutSpec struct {
    Deadline time.Time
    Kind     timeoutKind
    Seconds  int
}

type asyncJob struct {
    // ... existing + sync/done from §4.4 ...
    deadline        time.Time   // zero = no timeout
    timeoutKind     timeoutKind
    timeoutSeconds  int         // for notice text only (§5.1's exact wording), see TimeoutSpec.Seconds
    timeoutNotified bool        // one-shot guard for timeoutWakeOnly (§5.5): the notice fires at most once per deadline, never repeats
}
```

### 5.2 Форма параметра инструмента

Один вложенный объект `timeout`, ОДНО имя во всех трёх инструментах
(контракт §3):

```go
// добавляется в BashParams (tools/bash.go), RunCommandParams
// (tools/run_command.go), AgentParams (agent_tool.go) — идентичное поле
Timeout *TimeoutParams `json:"timeout,omitempty" description:"Optional explicit deadline: {\"seconds\": N, \"kind\": \"wake_only\"|\"terminate_and_wake\"}. \"wake_only\" lets the work keep running and just notifies you it's taking a while; \"terminate_and_wake\" stops it and reports partial output. No default -- set it only when you actually want a deadline."`

type TimeoutParams struct {
    Seconds int    `json:"seconds" description:"5-604800 (7 days). Seconds from call start."`
    Kind    string `json:"kind" description:"Required when seconds is set: \"wake_only\" or \"terminate_and_wake\"."`
}
```

**Легаси `run_command.timeout_seconds` — синоним, не отдельная ось**
(контракт §3, «Решения оркестратора» п.1: поле остаётся, его нельзя убрать —
им уже пользуются существующие вызовы модели). Семантически
`timeout_seconds: N` эквивалентно `timeout: {seconds: N, kind:
"terminate_and_wake"}`. Оба поля ОДНОВРЕМЕННО заданные для одного вызова
`run_command` — ошибка валидации (решение оркестратора явно отменяет
первоначальное предложение RFC «новое поле молча побеждает» — расхождение
между `docs/plans/2026-09-27-wake-tools-contract.md`'s §3 телом и его же
разделом «Решения оркестратора»; эта спецификация следует финальному
решению).

Разбор — ОДИН раз, в `asyncTool.Run` (§4.2), генерически по сырому
`call.Input`, без привязки к типизированным per-tool структурам (тот же
паттерн, что уже использует `childSessionID` для `AgentParams` и `t.run`
для `run_in_background`):

```go
// async_tool.go, новая функция
type timeoutParamInput struct {
    Timeout *struct {
        Seconds int    `json:"seconds"`
        Kind    string `json:"kind"`
    } `json:"timeout,omitempty"`
    // TimeoutSeconds mirrors run_command.RunCommandParams.TimeoutSeconds --
    // read generically here (like Timeout above) rather than importing the
    // typed struct, so this function stays tool-agnostic. Zero for bash/agent
    // (they have no such field; a stray value would mean the model sent an
    // unknown field, silently ignored like any other unexpected JSON key).
    TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// parseTimeoutParam extracts and validates the optional explicit timeout for
// bash/run_command/agent (NOT agentic_fetch -- out of scope, §0.2). Both new
// timeout{} and legacy run_command.timeout_seconds set on the SAME call is a
// validation error (operator decision, wake-tools-contract.md §3): the
// legacy field is a "terminate_and_wake" ALIAS, not an independent axis, so
// both present is an unresolvable ambiguity, not a silent precedence rule.
func parseTimeoutParam(toolName, input string) (*TimeoutSpec, error) {
    if toolName != tools.BashToolName && toolName != tools.RunCommandToolName && toolName != AgentToolName {
        return nil, nil
    }
    var parsed timeoutParamInput
    if err := json.Unmarshal([]byte(input), &parsed); err != nil {
        return nil, nil
    }
    if parsed.Timeout != nil && parsed.TimeoutSeconds != 0 {
        return nil, fmt.Errorf("provide at most one of timeout or the legacy timeout_seconds, not both")
    }
    if parsed.Timeout == nil {
        if parsed.TimeoutSeconds == 0 {
            return nil, nil
        }
        // Legacy alias: run_command.timeout_seconds's OWN validation
        // (existing runCommandDefaultTimeoutSeconds/runCommandMaxTimeoutSeconds
        // clamp, tools/run_command.go) is untouched and runs separately, as
        // it does today, for the process-kill mechanism itself -- this only
        // additionally records the SAME deadline in the ledger so the owner
        // gets a distinguishable timed_out outcome (contract §3) instead of
        // today's generic failed.
        return &TimeoutSpec{Deadline: time.Now().Add(time.Duration(parsed.TimeoutSeconds) * time.Second), Kind: timeoutTerminateAndWake, Seconds: parsed.TimeoutSeconds}, nil
    }
    t := parsed.Timeout
    if t.Seconds < 5 || t.Seconds > 604800 {
        return nil, fmt.Errorf("timeout.seconds must be between 5 and 604800 (7 days), got %d", t.Seconds)
    }
    var kind timeoutKind
    switch t.Kind {
    case "wake_only":
        kind = timeoutWakeOnly
    case "terminate_and_wake":
        kind = timeoutTerminateAndWake
    default:
        return nil, fmt.Errorf("timeout.kind must be %q or %q, got %q", "wake_only", "terminate_and_wake", t.Kind)
    }
    return &TimeoutSpec{Deadline: time.Now().Add(time.Duration(t.Seconds) * time.Second), Kind: kind, Seconds: t.Seconds}, nil
}
```

Границы `5 ≤ seconds ≤ 604800` — из контракта §3/§6, подтверждены
оркестратором («Решения оркестратора» п.2: «потолок 7 суток... принят как
защитная граница от опечаток; не продуктовое ограничение, меняется
константой»). Применены ОДИНАКОВО к обоим видам (`wake_only`/
`terminate_and_wake`): контракт поднимал вопрос, не нужен ли `wake_only`
вообще без верхней границы (он не убивает работу), но «Решения оркестратора»
подтвердили только ЧИСЛО 604800, не различие по виду — эта спецификация не
домысливает разделение, которого решение не содержит.

### 5.3 Валидация

- `timeout` и `timeout_seconds` (легаси, только у `run_command`) оба
  отсутствуют → `nil, nil` (нет таймаута, ничего не меняется).
- `timeout` и `timeout_seconds` оба присутствуют → ошибка `"provide at most
  one of timeout or the legacy timeout_seconds, not both"` (оркестраторское
  решение, §5.2).
- Только легаси `timeout_seconds` → трактуется как `timeout{seconds:
  timeout_seconds, kind: "terminate_and_wake"}`; СОБСТВЕННАЯ валидация
  `timeout_seconds` (клэмп на `runCommandMaxTimeoutSeconds=600`,
  `tools/run_command.go:34-37`) не меняется и не связана с диапазоном
  `5-604800` — это разные оси одного и того же числа (см. §5.2's
  doc-комментарий у `parseTimeoutParam`).
- `timeout.seconds` вне `[5, 604800]` → ошибка инструмента
  (`fantasy.NewTextErrorResponse`), РОВНО как сегодня возвращаются прочие
  ошибки валидации `asyncTool.Run` (`childSessionID`'s ошибки, `:52-54`) —
  модель получает текст ошибки как tool result, ход не прерывается.
- `timeout.kind` не `"wake_only"`/`"terminate_and_wake"` (включая пустую
  строку при заданном `seconds`) → ошибка инструмента, называющая оба
  допустимых значения (нет значения по умолчанию — операторское решение,
  `2026-09-24-...md`, «Решено 2026-09-27» п. 4; контракт §3 подтверждает).
- `agentic_fetch` с `timeout` в теле — поле просто не читается
  (`parseTimeoutParam`'s ранний `return nil, nil` для неизвестного toolName)
  — МОЛЧА игнорируется, а не ошибка: `agentic_fetch` не описывает это поле в
  своей JSON-схеме (не добавляется в её Params), так что модель не должна
  его прислать; если пришлёт (модель импровизирует) — не ошибка ценой
  простоты, тот же принцип, что `json.Unmarshal` с `,omitempty` уже
  применяет ко всем неизвестным полям.

### 5.3bis `NoticeKind` — новое поле сообщения (контракт §5.2, «Решения
оркестратора» п.3)

Контракт откладывал `message.NoticeKind` (необязательное строковое поле для
различения вида уведомления в вебе — иконка «завершилось» vs «пробуждение»
vs «таймаут» и т.д., §5.2 контракта) до «фазы, где появится первый
потребитель». «Решения оркестратора» называют этим потребителем ИМЕННО эту
фазу («фаза 2 — событие «время истекло»»). Значит поле заводится ЗДЕСЬ, но
ТОЛЬКО со значениями, которые эта фаза реально производит — `"timeout_wake_
only"` и `"timeout_terminated"` (два новых события §5.5); остальные значения
из контракта's таблицы §5.1 (`"wake"`, `"loop"`, `"agent_stopped"`,
`"interrupted"`, `"supervision"`, `"job_stopped"`) зарезервированы именами в
контракте, но НЕ устанавливаются никаким кодом этой фазы (их производят
фазы 3/4/5 и задача #1053) — заводить константы для них здесь означало бы
недостижимый код, что фаза 1 уже отвергала как принцип.

```go
// internal/message/content.go или message.go — не проверено точное текущее
// расположение AutoResumed/BackgroundJobNotice (см. §1.5 ниже, тот же файл);
// миграция того же вида, что 20260626000001_add_background_job_notice_to_
// messages.sql (не проверено точное имя следующего номера миграции)
type Message struct {
    // ... existing fields ...
    NoticeKind string // "", "timeout_wake_only", "timeout_terminated" in this phase; other values reserved by the contract for later phases
}
```

`message.CreateMessageParams` получает симметричное поле `NoticeKind string`,
прокидываемое туда же, куда сегодня `AutoResumed`/`BackgroundJobNotice`
(`agent_prompt.go:108-114`). Пустая строка — сегодняшнее поведение для ВСЕХ
существующих уведомлений (обычное завершение/ошибка, §1's маркер неудачного
пробуждения из §1.4 — он НЕ входит в зафиксированный контрактом словарь
значений, поэтому остаётся с пустым `NoticeKind`, как и раньше). Веб-рендер
нового поля (разные иконки) — контракт прямо отмечает как необязательное,
не входит в критерий готовности этой фазы (`docs/plans/2026-09-27-wake-tools-
contract.md` §11 «Решения оркестратора» п.3 говорит только «ввести поле»,
не «отрендерить его по-новому»).

### 5.4 Единый сервис сроков

Одна горутина на процесс (не на задачу), с min-heap по `deadline`:

```go
// work_ledger_timeout.go (новый файл)

// timeoutService is the single per-process timer for every asyncJob with a
// non-zero deadline: ONE goroutine sleeping until the nearest deadline,
// never one goroutine per job (task item 5's explicit requirement). Lazy
// deletion: an entry popped from the heap is re-checked against the job's
// CURRENT state under l.mu before acting -- a job that already reached a
// terminal state via finish/cancelSession/recheckChild (the ordinary CAS
// race, ASYNC-03) makes the popped entry a silent no-op, exactly like every
// other transitionToTerminal race.
type timeoutService struct {
    ledger *workLedger
    mu     sync.Mutex
    heap   jobDeadlineHeap // container/heap over []*asyncJob, Less by deadline
    wake   chan struct{}   // buffered 1: "recompute the nearest deadline"
    stop   chan struct{}
}

func newTimeoutService(l *workLedger) *timeoutService {
    s := &timeoutService{ledger: l, wake: make(chan struct{}, 1), stop: make(chan struct{})}
    go s.run()
    return s
}

// arm adds job to the heap. Caller must hold l.mu (called from Start,
// §4.4) -- arm itself takes s.mu separately (a different lock than l.mu) to
// avoid making the ledger's hot path (every Start/finish/cancelSession)
// contend on the SAME mutex the timer goroutine polls; the two locks are
// never held nested in either order (arm/fire never call back into l.mu
// while holding s.mu, and vice versa -- fire acquires l.mu only AFTER
// releasing s.mu, see run() below), so this cannot deadlock.
func (s *timeoutService) arm(job *asyncJob) {
    s.mu.Lock()
    heap.Push(&s.heap, job)
    s.mu.Unlock()
    select {
    case s.wake <- struct{}{}:
    default:
    }
}

func (s *timeoutService) run() {
    timer := time.NewTimer(time.Hour)
    defer timer.Stop()
    for {
        s.mu.Lock()
        var d time.Duration = time.Hour
        if s.heap.Len() > 0 {
            d = time.Until(s.heap[0].deadline)
            if d < 0 {
                d = 0
            }
        }
        s.mu.Unlock()
        if !timer.Stop() {
            select {
            case <-timer.C:
            default:
            }
        }
        timer.Reset(d)
        select {
        case <-s.stop:
            return
        case <-s.wake:
            continue
        case <-timer.C:
            s.fireDue()
        }
    }
}

func (s *timeoutService) fireDue() {
    now := time.Now()
    for {
        s.mu.Lock()
        if s.heap.Len() == 0 || s.heap[0].deadline.After(now) {
            s.mu.Unlock()
            return
        }
        job := heap.Pop(&s.heap).(*asyncJob)
        s.mu.Unlock()
        s.ledger.handleTimeout(job) // §5.5, acquires l.mu itself
    }
}

func (s *timeoutService) close() { close(s.stop) }
```

`workLedger.close()` (`work_ledger.go:329-346`) добавляет `l.timeouts.close()`
рядом с существующей остановкой `tickStop` — симметрично, тот же паттерн
жизненного цикла, что и защитный тикер фазы 1.

### 5.5 `handleTimeout` и взаимодействие с CAS

```go
// work_ledger_delegation.go либо новый work_ledger_timeout.go — не
// проверено, в каком файле меньше строк после добавления (см. §8, лимит
// 1000)

// handleTimeout re-checks job under l.mu (the ONLY writer of state/deadline
// fields) before acting -- this is the same "lazy deletion" contract every
// other terminal-race caller already follows (ASYNC-03): a job the timer
// popped stale (already terminal via finish/cancelSession/recheckChild) is
// silently skipped, not double-processed.
func (l *workLedger) handleTimeout(job *asyncJob) {
    l.mu.Lock()
    if job.state != phaseRunning || job.deadline.IsZero() {
        l.mu.Unlock()
        return
    }
    switch job.timeoutKind {
    case timeoutTerminateAndWake:
        if job.cancel != nil {
            job.cancel() // best-effort: ask the executor to stop; partial output captured below regardless of whether it stops in time
        }
        partial := l.capturePartialLocked(job) // §5.6
        job.transitionToTerminal(phaseTimedOut, partial)
        completion, callback := l.deliverLocked(job.owner, job)
        l.mu.Unlock()
        if callback {
            l.onWebDone(completion)
        }
        // Delivery reaches wakeSession through the ORDINARY path
        // (deliverLocked -> notifyAsyncCompletion, §1/§2), exactly like
        // finish/recheckChild -- no separate wake call here. The
        // timeout-specific TEXT and NoticeKind are selected by
        // notifyAsyncCompletion/FormatAsyncCompletion below, keyed off
        // completion.TimedOut (set by deliverLocked from job.state), not by
        // this function calling wakeSession a second time.
    case timeoutWakeOnly:
        if job.timeoutNotified {
            l.mu.Unlock()
            return
        }
        job.timeoutNotified = true
        owner, toolCallID, toolName := job.owner, job.toolCallID, job.toolName
        deadline := job.deadline
        summary := l.capturePartialLocked(job) // best-effort progress summary, job stays phaseRunning
        l.mu.Unlock()
        if l.coord != nil {
            text := fmt.Sprintf("Timeout reached for async job %s (%s) — it is still running (elapsed %s). Latest output:\n\n%s\n\nThis was a one-time check-in; it will not repeat automatically. Decide whether to keep waiting, check again later, or stop it with job_kill (bash/run_command) or stop_agent (agent).",
                toolCallID, toolName, time.Since(deadline).Round(time.Second), summary.content)
            id := jobIdentity{owner: owner, toolCallID: toolCallID}
            go l.coord.wakeSession(context.Background(), id, text, "timeout_wake_only", true)
        }
    default:
        l.mu.Unlock()
    }
}
```

**Исправление найдено при написании (не в первом черновике):** `deliverLocked`
для `phaseTimedOut` доставляется ТЕМ ЖЕ путём, что и `phaseCompleted`/
`phaseFailed` — через `notifyAsyncCompletion`/`wakeSession`, которые сегодня
всегда используют `FormatAsyncCompletion` (обычный текст «finished»/
«failed»). Контракт (§5.1) требует ДРУГОЙ текст для таймаута
(`"...timed out after {seconds}s and was stopped. Partial output:\n\n..."`)
и `NoticeKind="timeout_terminated"`. Решение: `FormatAsyncCompletion`
(`coordinator_background.go:34-45`) получает ветку по терминальному виду —
не новый параметр у `deliverLocked`/`AsyncCompletion` (та структура не несёт
`jobPhase`, только `IsError bool`, `work_ledger.go:23-32`), а НОВОЕ булево
поле `AsyncCompletion.TimedOut bool`, устанавливаемое в `deliverLocked` из
`job.state == phaseTimedOut` (так же, как `IsError` уже устанавливается из
`job.result.isError`):

```go
// work_ledger.go, правка AsyncCompletion (§4.4's структура, уже
// расширяется этой фазой полем cli — TimedOut добавляется рядом)
type AsyncCompletion struct {
    // ... existing fields ...
    TimedOut       bool // true when the job's terminal state was phaseTimedOut (set by deliverLocked); selects the contract's timeout wording and NoticeKind instead of the generic finished/failed one
    TimeoutSeconds int  // job.timeoutSeconds, quoted verbatim in the timeout text; meaningless when !TimedOut
}

// coordinator_background.go, правка FormatAsyncCompletion — text matches
// docs/plans/2026-09-27-wake-tools-contract.md §5.1's table verbatim
func FormatAsyncCompletion(completion AsyncCompletion) string {
    content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
    if content == "" {
        content = "(no output)"
    }
    if completion.TimedOut {
        return fmt.Sprintf("Async job %s (%s) timed out after %ds and was stopped. Partial output:\n\n%s",
            completion.ToolCallID, completion.ToolName, completion.TimeoutSeconds, content)
    }
    status := "finished"
    if completion.IsError {
        status = "failed"
    }
    return fmt.Sprintf("Async job %s (%s) %s.\n\n%s", completion.ToolCallID, completion.ToolName, status, content)
}
```

`notifyAsyncCompletion`/`wakeSession`'s единый вызов (§2) передаёт `noticeKind
= "timeout_terminated"` при `completion.TimedOut`, `""` иначе — одна ветка в
одном месте (`notifyAsyncCompletion`'s тело), не разброс по `handleTimeout`.
Это устраняет ложное «второе сообщение» из первого черновика этого раздела
(`handleTimeout`'s `timeoutTerminateAndWake`-ветка сама НЕ зовёт `wakeSession`
— доставка идёт исключительно через обычный `deliverLocked`→
`notifyAsyncCompletion` путь, ровно как для `finish`/`recheckChild`; только
`timeoutWakeOnly` зовёт `wakeSession` напрямую, потому что она НЕ проходит
через `deliverLocked` вообще — задача остаётся `phaseRunning`).

CAS-гарантия, дословно: `timeoutTerminateAndWake` конкурирует за
`transitionToTerminal` НАРАВНЕ с `finish`/`cancelSession`/`recheckChild` —
если исполнитель успел завершиться (или отмениться) РОВНО в момент, когда
таймер выстрелил, ровно одна сторона выигрывает CAS (`work_job.go:79-86`,
не тронуто этой фазой), другая — no-op; `handleTimeout`'s собственная
предпроверка `job.state != phaseRunning` под ТЕМ ЖЕ `l.mu` — вторая, более
дешёвая линия той же гарантии (избегает лишнего `capturePartialLocked`,
если задача уже точно терминальна), а не замена CAS. `timeoutWakeOnly`
никогда не участвует в CAS (не трогает `state`), поэтому у неё нет
конкурентного исхода с finish/cancel — только собственный `timeoutNotified`
one-shot guard под тем же `l.mu`.

### 5.6 Захват частичного вывода — не проверено полностью

```go
// capturePartialLocked (both branches). For a plain command job
// (childSession == ""), the executor's own output buffer is what needs
// reading; for a delegation, the child's transcript.
func (l *workLedger) capturePartialLocked(job *asyncJob) jobResult { ... }
```

**Не проверено**: точный API `internal/shell.BackgroundShellManager`/
`BackgroundShell` для БЕЗОПАСНОГО конкурентного чтения ЕЩЁ РАБОТАЮЩЕГО
процесса (файл не открывался в ходе этой спецификации — вне списка файлов,
данных для чтения). `asyncTool.awaitShell` (`async_tool.go:178-203`) читает
`sh.GetOutput()` только ПОСЛЕ `sh.WaitContext(ctx)` возвращает — то есть
только когда процесс УЖЕ завершился (или был убит с таймаутом ожидания
завершения). Является ли `GetOutput()` безопасным для вызова, пока процесс
ЕЩЁ выполняется (для `timeoutWakeOnly`, который не убивает процесс) — не
проверено; требует чтения `internal/shell/background.go` до реализации.
Если небезопасно, `capturePartialLocked` для `jobKindCommand` в
`timeoutWakeOnly`-ветке ограничивается текстом-заглушкой («job %s is still
running past its deadline; use job_output to inspect progress» — переиспользуя
уже существующий инструмент просмотра, `job_output`, упомянутый в плане
пробуждений, а не изобретая новое чтение вывода) — реализующий фазу 2 должен
проверить это ПЕРЕД тем, как писать `capturePartialLocked`'s тело, а не
предполагать.

Для делегации (`childSession != ""`) — тот же приём, что уже использует
`refreshSubAgentCompletion` (`coordinator_work_scope.go:36-60`): прочитать
последнее сообщение дочерней сессии из `c.messages.List` — но БЕЗ требования
`IsFinished()` (делегация ещё НЕ закончена, `timeoutWakeOnly` не отменяет
её) — просто взять последний текст любого сообщения как индикатор прогресса,
с явной пометкой в тексте уведомления, что это НЕ финальный результат.

### 5.7 Тесты

- `TestTimeoutService_SingleGoroutineForManyJobs` — заармировать N задач с
  разными дедлайнами; проверить (через `runtime.NumGoroutine()`-дельту до/
  после, с толерантным допуском) что не появилось N новых горутин, а
  устойчиво одна. *Revert-check:* реализовать `arm` через
  `time.AfterFunc(deadline, ...)` на каждую задачу (естественная, но
  запрещённая заданием реализация) — тест обязан показать рост горутин
  пропорционально N.
- `TestTimeoutService_FiresNearestFirst` — заармировать задачи с дедлайнами
  T+50ms и T+200ms (сжатые константы для теста, не production-значения);
  убедиться что первая обработана раньше второй, с точностью, допускающей
  race на таймере (проверка ПОРЯДКА вызовов `handleTimeout`, не абсолютного
  времени).
- `TestWorkLedger_TerminateAndWakeTransitionsToTimedOut` — job с
  `timeoutTerminateAndWake`, дедлайн в прошлом сразу после `Start` (для
  детерминизма — не полагаться на реальный сон); вызвать
  `handleTimeout` напрямую; проверить `phaseTimedOut`, `job.cancel` вызван,
  доставка произошла (через шпион на `wakeSession`/`onWebDone`).
- `TestNotifyAsyncCompletion_TimedOutUsesContractTextAndNoticeKind` —
  `AsyncCompletion{TimedOut: true, TimeoutSeconds: 900, ...}`; проверить
  `FormatAsyncCompletion`'s текст ровно совпадает с контрактом
  (`"...timed out after 900s and was stopped. Partial output:\n\n..."`) и
  что `wakeSession`-шпион получил `noticeKind == "timeout_wake_only"` для
  `wake_only`-события (§5.5's прямой вызов) и `"timeout_terminated"` для
  `TimedOut` через `notifyAsyncCompletion`. *Revert-check:* вернуть
  `FormatAsyncCompletion` к безусловному «finished/failed» тексту — тест
  обязан показать неверный текст для таймаута.
- `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome` — одновременно
  `finish(success)` и `handleTimeout` для одной задачи с истёкшим
  `timeoutTerminateAndWake` дедлайном; ровно один исход доставлен (прямое
  продолжение ASYNC-03's теста фазы 1, `TestWorkLedger_
  ConcurrentTerminalRaceYieldsExactlyOneOutcome`, добавить `handleTimeout`
  как ещё одну конкурирующую горутину в ТОТ ЖЕ тест, а не отдельный).
  *Revert-check:* убрать проверку `job.state != phaseRunning` в
  `handleTimeout` (полагаться только на `transitionToTerminal`'s CAS) — тест
  ДОЛЖЕН по-прежнему проходить (это вторичная, не единственная защита) —
  этот конкретный revert-check ПРОВЕРЯЕТ, что защита избыточна, а не что она
  обязательна; обязательности CAS доказывает уже существующий тест фазы 1.
- `TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning` — `timeoutWakeOnly`,
  дедлайн в прошлом; вызвать `handleTimeout` ДВАЖДЫ (симулируя гонку с самим
  сервисом или ретрай); проверить ровно ОДИН вызов `wakeSession`-шпиона и
  `job.state` остаётся `phaseRunning` после обоих вызовов. *Revert-check:*
  убрать `timeoutNotified` guard — тест обязан показать 2 вызова.
- `TestParseTimeoutParam_ValidationTable` — табличный тест: ничего не задано
  (nil,nil); `timeout.seconds=4`/`604801` (ошибка границы); `timeout.kind`
  пустое/неизвестное при заданном `seconds` (ошибка); валидные `wake_only`/
  `terminate_and_wake` (успех, правильный `Kind`, `Seconds` сохранён);
  только легаси `timeout_seconds=90` у `run_command` (успех, alias на
  `terminate_and_wake`, `Seconds=90`); ОБА `timeout` и `timeout_seconds`
  одновременно (ошибка «provide at most one of...»); `agentic_fetch` с
  `timeout` в теле (нет ошибки, `nil, nil`).

## 6. Restricted-run политика для разбуженных дочерних ходов

### 6.1 Диагноз (найдено ревью #1049, сверено с кодом)

Два места безусловно ОСВОБОЖДАЮТ дочернюю сессию от унаследованной
allowlist-политики, как только ПЕРВЫЙ (запускающий) вызов возвращается —
что не совпадает с реальным окончанием работы ребёнка под структурной
конкурентностью (ребёнок может ещё владеть async-задачами/фоновыми shell,
арминг в `byChild`):

```go
// coordinator_subagents.go:107-126, runSubAgent
if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
    mgr.InheritSessionRunAllowlist(params.SessionID, session.ID)
    defer mgr.ClearSessionRunAllowlist(session.ID) // <-- слишком рано
}
```

```go
// async_tool.go:118-121, t.run
if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
    defer mgr.ClearSessionRunAllowlist(childSessionID) // <-- слишком рано
}
```

Позже, `wakeSession` (§1-2) стартует НОВЫЙ ход на ТОЙ ЖЕ дочерней сессии
(через `driver.agent.Run`, драйвер из `c.subAgentDrivers.get`) — `Run`'s
`SessionAgentCall`, построенный `wakeSession`/`subAgentDriver.callFor`
(`coordinator_subagent_drivers.go:42-46`), НЕ несёт `RunAllowlist`
(проверено: `callTemplate` в `coordinator_subagents.go:245-257` не
устанавливает это поле) — значит этот ход попадает под F2-фолбэк
(`agent_run.go:544-548`'s doc: «session baseline... и только потом
process-wide policy»), а СЕССИОННЫЙ БАЗОВЫЙ уже стёрт указанными выше
`defer`. Результат — разбуженный дочерний ход СУДИТСЯ ПО ПРОЦЕСС-ШИРОКОЙ
политике вместо политики делегации, которую унаследовал первый ход.

### 6.2 Решение: время жизни = время жизни драйвера, не время жизни первого хода

Тот же принцип, что `subAgentDriverRegistry` уже применяет к себе
(`coordinator_subagent_drivers.go:48-61`, дословная цитата: «resume_session_id
может повторно вести ТУ ЖЕ дочернюю сессию сколь угодно далеко в будущем...
удаление записи раньше времени молча откатилось бы к `c.currentAgent`»).
Точно то же верно для allowlist-базовой строки: она должна жить, пока жив
драйвер, и переустанавливаться при КАЖДОМ ходе на этой дочерней сессии — не
только при первом.

1. Убрать `defer mgr.ClearSessionRunAllowlist(session.ID)`
   (`coordinator_subagents.go:124`) и `defer mgr.ClearSessionRunAllowlist(childSessionID)`
   (`async_tool.go:120`) целиком.
2. Добавить `parentSessionID` в `subAgentDriver`
   (`coordinator_subagent_drivers.go:35-38`):

```go
type subAgentDriver struct {
    agent           SessionAgent
    call            SessionAgentCall
    parentSessionID string // needed to re-inherit the allowlist baseline on every wake (§6.2), not just the first turn
}
```

3. `runSubAgent` (`coordinator_subagents.go:258`) передаёт его при
   регистрации: `c.subAgentDrivers.register(session.ID, subAgentDriver{agent:
   params.Agent, call: callTemplate, parentSessionID: params.SessionID})`.
4. `wakeSession` (§1.6), перед вызовом `Run` на найденном драйвере,
   ПЕРЕУСТАНАВЛИВАЕТ базовую политику заново — она уже последняя актуальная
   политика родителя (переинаследование, а не замороженный снимок — если
   политика родителя менялась между делегациями, разбуженный ребёнок видит
   СВЕЖУЮ, что не хуже сегодняшнего поведения и, возможно, точнее):

```go
if driver, ok := c.subAgentDrivers.get(sessionID); ok {
    if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok && driver.parentSessionID != "" {
        mgr.InheritSessionRunAllowlist(driver.parentSessionID, sessionID)
    }
    // ... build call from driver, Run ...
}
```

`asyncTool.run`'s аналогичный `InheritSessionRunAllowlist` при СТАРТЕ
(`:118-121`, для `agent`/`agentic_fetch` под `asyncTool`) не убирается —
он остаётся первичным armed'ом для ПЕРВОГО хода; убирается только его
парный `defer Clear`.

### 6.3 Принятый рост без очистки

Как и `subAgentDriverRegistry`, процесс-широкая карта allowlist-записей
(`permissionService`'s внутреннее хранилище, `internal/permission/
permission.go` — не проверено имя внутреннего поля, не открывался этот файл
дальше сигнатур интерфейса) теперь растёт на одну запись за КАЖДУЮ
когда-либо делегированную дочернюю сессию, без явной очистки в фазе 2. Это
тот же порядок роста, что уже принят для `coordinator.agents`,
`subAgentDriverRegistry`, таблиц сессий/сообщений — ни одна не подчищается
в памяти сегодня. Явная точка очистки (когда область ребёнка ОКОНЧАТЕЛЬНО
закрыта — событие, которого у фазы 2 ещё нет) откладывается на фазу 3
(«область закрыта») — там же, где закрывается `noticedBefore` (§2.4) и
аналогичные карты; отмечено как связанный follow-up, не как забытая работа.

### 6.4 Тесты

- `TestWakeSession_ReArmsChildRunAllowlistBeforeWaking` — зарегистрировать
  драйвер с `parentSessionID`, установить allowlist на родителе, ВЫЗВАТЬ
  `ClearSessionRunAllowlist` на ребёнке вручную (симулируя то, что раньше
  делал убранный `defer`), затем вызвать `wakeSession`; проверить, что
  allowlist на ребёнке снова установлен ДО вызова `Run`. *Revert-check:*
  убрать переустановку в `wakeSession` — тест обязан показать отсутствие
  записи на ребёнке в момент вызова `Run` (шпион на `permission.Service`).
- `TestRunSubAgent_DoesNotClearChildAllowlistOnReturn` — вызвать `runSubAgent`
  до конца, проверить что `ClearSessionRunAllowlist(childID)` НЕ был вызван
  (шпион-счётчик). *Revert-check:* вернуть `defer mgr.ClearSessionRunAllowlist(...)` —
  тест обязан показать вызов.
- `TestAsyncTool_DoesNotClearChildAllowlistOnDelegationReturn` — то же для
  `t.run`'s пути (`agent`/`agentic_fetch` под async-обёрткой).

## 7. Что не меняется, что закрывает, переанкеровка инвариантов

### 7.1 Не меняется

- Учёт областей (`DescendantWorkPending`, `descendantWorkPollInterval`,
  BFS) — фаза 3, `internal/app/app_run_async.go` не редактируется.
- Долговечность/SQLite/`sessions why`/`descendant_liveness.go` — фаза 4.
- `wakein`/`wakeon`/`loop`/надзор корня — фаза 5.
- Публичная сигнатура `Coordinator.Run`/`sessionAgent.Run` и её `(nil, nil)`
  контракт для ВНЕШНИХ вызывающих (CLI, веб) — не меняется (§3.2).
- `BackgroundShellManager`, mailbox (submit/drainOrRelease*/interruptAndReplace) —
  не тронуты; `wakeSession`/`runAwaitingAdmission` — новые вызывающие
  существующих примитивов (`InjectMessage`-подобная персистентность,
  `injectIfBusy`, `ExistingMessageID`), не новая инфраструктура мейлбокса.
- `agentic_fetch` — без явного таймаута (§0.2); её собственная доставка
  через `asyncTool`/`workLedger` МЕНЯЕТСЯ п. 4 (единый путь), это отдельно
  от таймаута.

### 7.2 Закрывает

- **#1035** — полностью в части путей `notifyAsyncCompletion`/
  `notifyBackgroundJobDone` (§1.4): `slog.Debug` → `slog.Warn` + персистентный
  маркер. `refreshSubAgentCompletion`'s отдельный `Debug` (не про
  пробуждение) НЕ входит — см. §1.4.
- **#1036** — полностью для `runSubAgent`'s resume-пути (§3.3): очередь
  больше не читается как «пустой ответ».
- **#1037** — полностью: `asyncTool.Run` больше не имеет ветки по origin
  (§4), явный таймаут двух видов существует с единым сервисом сроков (§5).
- **#1054** — полностью (§2.5): `Cancel`/`InjectMessage` маршрутизируются
  через `agentFor`, ту же точку выбора, что `wakeSession`. Разблокирует
  будущие `stop_agent`/`inject_agent` (план пробуждений, этап 3), которые
  сами не входят в объём фазы 2.
- **#1053** — НЕ закрывается этой спецификацией (параллельная задача, §0.2);
  упомянута только для координации по общему файлу (`work_job.go`, §10).

### 7.3 Переанкеровка `docs/async-invariants.md`

Обязательна тем же коммитом, что и переключение (см. §8, шаг с пометкой
«переключение»):

| ID | Что меняется |
|---|---|
| ASYNC-04 | ack-гейт (`announced`) теперь ТАКЖЕ верен для `sync`-задач (устанавливается в `Start`, не только в `onToolResult`) — переписать формулировку «ack-gate» с явной оговоркой про `sync` (§4.4). Ссылка на `deliverLocked` остаётся, но добавляется ссылка на `awaitSync`/§4.4. |
| ASYNC-06 | статус меняется с «нарушается — закрывается фазой 2» на «выполняется»: `wakeSession` — единственный инициатор (§2), `notifyAsyncCompletion`/`notifyBackgroundJobDone` его вызывают, не строят ход сами; `agentFor` (§2.2) обобщает выбор драйвера и на `Cancel`/`InjectMessage` (§2.5, задача #1054), которые раньше обходили его вовсе. Доказывающий тест — §2/§2.5's разделы тестов (добавить). |
| ASYNC-07 | статус «частично» → уточнить: маршрут теперь не зависит и от РЕЖИМА исполнения (sync/async), не только от сессии — ссылка на `deliverLocked`'s `job.sync` ветку (§4.4). Ещё не «выполняется» полностью — `notifyBackgroundJobDone`'s разделение Phase-3/Phase-4 остаётся (это политика автономности, не маршрут доставки — не смешивать). |
| ASYNC-09 | статус «нарушается — закрывается фазами 4-5» → «частично»: путь `wakeSession` (§1.4) закрыт (Warn + маркер); `refreshSubAgentCompletion`'s DB-read Debug — по-прежнему нарушение, вне объёма фазы 2 (см. §1.4 явно). |
| ASYNC-03 | добавить строку/пример: гонка `finish`/`cancelSession`/`recheckChild`/`handleTimeout` (новый пятый конкурент) — ссылка на §5.7's тест. |

Строки, которые эта фаза НЕ трогает и не обязана переанкеровывать:
ASYNC-01, ASYNC-02, ASYNC-05, ASYNC-08, ASYNC-10 (ни один их механизм не
редактируется этой спецификацией).

## 8. Порядок реализации

Каждый шаг — рабочая сборка. Тесты, перечисленные после шага, обязаны быть
зелёными сразу после него (список кумулятивный: тесты предыдущих шагов
остаются зелёными).

**Шаг 0 — типы, без подключения.** Правка `work_job.go`: добавить
`phaseTimedOut`, `timeoutKind`, `TimeoutSpec`, поля `deadline`/`timeoutKind`/
`timeoutSeconds`/`timeoutNotified`/`sync`/`done` на `asyncJob`; правка
`work_ledger.go` — добавить `noticedBefore map[string]string` на
`workLedger` (§2.4) — БЕЗ изменения сигнатуры `Start`/`deliverLocked` (новые
поля/карта просто не заполняются нигде). Сборка зелёная, ничего не вызывает
новые поля. Тесты: весь существующий набор `internal/agent` без изменений.

**Шаг 1 — `workLedger.Start`/`deliverLocked` новая сигнатура, без
подключения продакшн-вызывающих.** Изменить `Start`/`deliverLocked`
(§4.4), добавить `awaitSync` (§4.4), `noticedBefore` (§2.4). Обновить
ЕДИНСТВЕННОГО существующего вызывающего `Start` — `async_tool.go` — с
временными значениями (`sync=false` везде, `timeout=nil` везде) — сборка
компилируется, поведение БАЙТ-В-БАЙТ то же, что до шага (проверяется
существующими тестами `async_tool_test.go`, не редактируются). Тесты:
`work_ledger_test.go`'s текущий набор + новый `TestWorkLedger_
SyncJobBypassesReadyQueueAndWebDone` (напрямую вызвать `Start(sync=true)`,
`finish`, проверить что `s.ready` пуст и `onWebDone`-шпион не вызван, канал
`done` закрыт).

**Шаг 2 — `runAwaitingAdmission` + `onQueueResolved`, без подключения
`runSubAgent`.** Новый файл `coordinator_run_await.go`, правка `agent.go`
(поле) и `agent_run.go` (хук в `runOwned`). Тесты: §3.5's первые два теста
проходят изолированно; весь существующий набор `internal/agent` зелёный
(поле `nil` для всех старых вызывающих).

**Шаг 3 — `runSubAgent` переключается на `runAwaitingAdmission`.** Правка
`coordinator_subagents.go` (§3.3). Тесты: §3.5's `TestRunSubAgent_
ResumeOfBusyChildDoesNotReportNoOutput` (новый, доказывает #1036 закрыт);
существующие тесты `coordinator_subagents.go` (не проверено точное имя
файла тестов — вероятно `coordinator_subagents_test.go` или подобный,
искать при реализации) зелёные без изменений.

**Шаг 4 — `wakeSession` + единый инициатор (главный шаг, отдельный
коммит).** Новый файл `coordinator_wake.go` (§1.6, §2.2), правка
`coordinator_background.go` (`notifyAsyncCompletion`/`notifyBackgroundJobDone`
переключаются на `wakeSession`, §1.7/§2.3), правка `coordinator_subagent_drivers.go`
(`parentSessionID`, §6.2) и `coordinator_subagents.go` (передать
`parentSessionID` при регистрации, убрать `defer Clear`, §6.2) и
`async_tool.go` (убрать `defer Clear`, §6.2). Тесты, обязанные быть
зелёными СРАЗУ после этого шага:
  - весь `internal/agent`;
  - §1.4/§2's новые тесты (Warn+маркер на неудачном wake — нужен фейковый
    провайдер, который отказывает `Run`, например `checkPeakHours`-подобная
    ошибка через тестовый конфиг, или прямой шпион на `SessionAgent.Run`);
  - §6.4's три теста;
  - восемь чёрных тестов `internal/app`, перечисленных в фазе 1's §3 шаг 3
    (`TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON` и
    остальные семь) — НЕ редактируются, доказывают отсутствие внешнего
    регресса.

**Шаг 5 — единый путь `asyncTool` (§4).** Правка `async_tool.go` целиком
(`Run`/`run`/`finalize`/новый `awaitAndFinish`), `Start`'s вызов получает
реальные `sync`/`timeout` аргументы (заменяя временные значения шага 1).
Тесты: §4.6's пять тестов; существующие `async_tool_test.go` без
редактирования.

**Шаг 6 — явные таймауты, параметр и сервис (§5).** Новый
`work_ledger_timeout.go`, правка `work_job.go` (подключить `deadline`/
`timeoutKind`/`timeoutSeconds` реально — они уже определены с шага 0, теперь
заполняются), `work_ledger.go`'s `Start`/`deliverLocked` (арминг таймера,
`AsyncCompletion.TimedOut`/`TimeoutSeconds`), `coordinator_background.go`'s
`FormatAsyncCompletion` (ветка по `TimedOut`), `async_tool.go`'s
`parseTimeoutParam` + правка `BashParams`/`RunCommandParams`/`AgentParams`
(добавить `Timeout` поле; `RunCommandParams`'s существующий `TimeoutSeconds`
не переименовывается — легаси-алиас, §5.2). Миграция + поле `NoticeKind`
(§5.3bis) — тем же шагом, устанавливается для `"timeout_wake_only"`/
`"timeout_terminated"`. Тесты: §5.7's полный список.

**Шаг 7 — задача #1054, `Cancel`/`InjectMessage` driver-aware (§2.5).**
Новая функция `agentFor` (`coordinator_subagent_drivers.go`), правка
`coordinator_interrupt.go`'s `Cancel`/`InjectMessage`; `wakeSession` (шаг 4)
переключается на `agentFor` вместо собственной инлайновой копии логики
выбора (безопасно делать ПОСЛЕ шага 4, т.к. это чистый рефакторинг вызова
без изменения исхода). Тесты: §2.5's два теста.

**Шаг 8 — переанкеровка `docs/async-invariants.md`.** Тем же коммитом, что
шаг 4 (единый инициатор — самое существенное изменение для инвариантов) ЛИБО
отдельным коммитом сразу после шага 7, если таймауты/#1054 тоже меняют
формулировки строк (ASYNC-03) — на усмотрение реализующего, но ДО открытия
PR/мержа фазы 2 целиком, не откладывать на «потом» (правило самого файла).

**Шаг 9 (опционально).** Обновить статус-строку фазы 2 в
`docs/plans/2026-09-27-async-structured-concurrency.md` и в
`docs/plans/2026-09-27-wake-tools-contract.md`'s §9 «Карта по фазам»
(отметить §3/§4.4 выполненными).

## 9. Файловый план (≤1000 строк, один пакет)

| Файл | Содержимое | Оценка строк после правки | Новый/правка |
|---|---|---|---|
| `work_job.go` | + `phaseTimedOut`, `timeoutKind`, `TimeoutSpec`, новые поля `asyncJob` | 86 → ~130 | правка |
| `work_ledger.go` | + `sync`/`timeout` параметры `Start`, `awaitSync`, `noticedBefore`, sync-ветка `deliverLocked` | 346 → ~410 | правка |
| `work_ledger_delegation.go` | без структурных изменений (таймаут не меняет делегационную логику напрямую) | 362 | без изменений |
| `work_ledger_timeout.go` | `timeoutService`, `handleTimeout`, `capturePartialLocked`, min-heap | ~180 | новый |
| `coordinator_wake.go` | `wakeSession`, видимый маркер неудачи (§1.4) | ~150 | новый |
| `coordinator_run_await.go` | `runAwaitingAdmission` | ~60 | новый |
| `coordinator_background.go` | `notifyAsyncCompletion`/`notifyBackgroundJobDone` сокращаются до вызовов `wakeSession`; `FormatAsyncCompletion`'s `TimedOut`-ветка | 222 → ~190 | правка |
| `coordinator_subagent_drivers.go` | + `parentSessionID` поле, + `agentFor` (§2.2/§2.5) | 92 → ~115 | правка |
| `coordinator_subagents.go` | − 2 строки (`defer Clear`), + передача `parentSessionID` | 508 → ~510 | правка |
| `coordinator_interrupt.go` | `Cancel`/`InjectMessage` → `c.agentFor(sessionID)` (§2.5, задача #1054) | 644 → ~646 | правка |
| `async_tool.go` | `sync` разбор, `parseTimeoutParam`, `awaitAndFinish`, убрать origin-ветку и `defer Clear` | 216 → ~305 | правка |
| `agent_run.go` | + хук `onQueueResolved` в `runOwned` (1 условие) | 625 → ~630 | правка |
| `agent.go` | + поле `onQueueResolved` на `SessionAgentCall` | без данных (не проверено точное число строк файла — не открывался целиком) | правка |
| `tools/bash.go`, `tools/run_command.go`, `agent_tool.go` | + поле `Timeout` в трёх Params-структурах (`run_command`'s легаси `TimeoutSeconds` не трогается) | без изменения порядка величины | правка |
| `internal/message` (файл не открывался целиком — не проверено точное имя) + новая миграция | + поле `NoticeKind` (§5.3bis), симметрично `AutoResumed`/`BackgroundJobNotice` | без данных | правка + новая миграция |

Ни один файл не приближается к пределу 1000 строк — записи в
`.githooks/file_size_allowlist.txt`/`.golangci.yml` не требуются. `wc -l`
по факту — обязателен после написания, не по оценке (как и фаза 1 сама
себе предписывала).

## 10. Риски

- **`l.mu` vs `timeoutService.mu`.** Две разные блокировки (§5.4); порядок
  захвата НИКОГДА не вложенный в обе стороны одновременно — заявлено по
  конструкции (`arm`/`fire` отпускают `s.mu` до захвата `l.mu`), но не
  проверено `go test -race` (не запускался в рамках этой спецификации по
  прямому запрету задания). Реализующий ОБЯЗАН прогнать `-race` на
  `work_ledger_timeout_test.go` до мержа.
- **`capturePartialLocked`'s зависимость от непрочитанного файла**
  (`internal/shell/background.go`, §5.6) — единственное место в этой
  спецификации, где техническое решение отложено до чтения кода, которого
  не было в списке для этой задачи. Помечено «не проверено» явно, не
  угадано.
- **`noticedBefore`/allowlist-карты растут без очистки в фазе 2** (§2.4,
  §6.3) — принято по прецеденту `subAgentDriverRegistry`, но это ВТОРАЯ и
  ТРЕТЬЯ подобная карта, добавленная этой фазой; если фаза 3 задержится,
  накопленный риск памяти для очень долгоживущего веб-процесса стоит
  переоценить количественно (не проверено — оценки нагрузки не запрошены
  этой задачей).
- **Координация с параллельной задачей #1053.** `asyncJob.shellID` (её
  поле) и поля этой спецификации (`sync`, `done`, `deadline`, `timeoutKind`,
  `timeoutSeconds`, `timeoutNotified`, §4.4/§5.1) редактируют ОДИН И ТОТ ЖЕ
  тип `asyncJob` в ОДНОМ И ТОМ ЖЕ файле (`work_job.go`) в двух параллельных
  ветках. Слияние потребует ручного объединения структуры (не конфликт
  логики — оба набора полей независимы друг от друга), но порядок мержа
  веток имеет значение: реализующий фазу 2 должен слить #1053 первым (она,
  по словам оркестратора, уже в работе) и накатить эту спецификацию поверх,
  а не наоборот.
- **Оракул «чистого переноса»** (`CLAUDE.md`'s before/after diff) неприменим
  ни к одному файлу этой фазы — все правки функциональные (новые параметры,
  новая ветка `sync`, новый файл), не чистое перемещение. Правильная
  проверка — списки тестов по шагам (§8) и восемь неизменяемых чёрных
  тестов `internal/app`.

## 11. Открытые вопросы оператору

Только продуктовые решения; технические уже приняты и обоснованы выше
(верхняя граница `timeout.seconds`, судьба легаси `run_command.
timeout_seconds`, форма `NoticeKind`, число `20` для расписаний — все четыре
уже разрешены `docs/plans/2026-09-27-wake-tools-contract.md`'s «Решения
оркестратора» и применены в §5 этого документа, повторно не выносятся).

1. **Видимая форма неудачного пробуждения (§1.4, §1.5).** Спецификация
   выбрала «второе персистентное сообщение с тегом `BackgroundJobNotice`,
   без `NoticeKind`» как наименее инвазивный вариант — этот случай (сбой
   САМОГО пробуждения, не исход задачи) не входит в зафиксированный
   контрактом словарь `NoticeKind` (§5.3bis). Если оператор хочет визуально
   отличать «результат задачи» от «маркер сбоя пробуждения» в веб-UI уже
   сейчас — нужно либо расширить словарь `NoticeKind` значением вне
   контракта (например, `"wake_failed"`), либо явно принять, что это
   отличие видно только по тексту до момента, когда контракт (или его
   ревизия) определит для него собственное значение.
2. **Рост карт без очистки (§2.4, §6.3, §10).** Принято по прецеденту
   `subAgentDriverRegistry`, но накапливается ТРЕТЬЯ подобная структура за
   одну фазу. Приемлемо ли это до фазы 3, или нужен временный TTL/лимит
   размера уже в фазе 2 (дополнительная, не запрошенная заданием сложность)?
3. **Порядок слияния с #1053 (§10).** Подтвердить, что #1053 мержится в
   `main` ПЕРВЫМ, а эта спецификация реализуется поверх её результата — не
   параллельно на разных базовых коммитах, что удвоило бы работу по
   разрешению конфликта в `work_job.go`.

## Решения оркестратора по открытым вопросам (2026-09-28)

1. **Маркер неудачного пробуждения** — отдельное системное сообщение в
   транскрипте владельца с `NoticeKind = "wake_failed"`: «Не удалось
   продолжить работу после события <job id>: <причина>. Событие сохранено;
   продолжение — при следующем ходе». Веб показывает его как системное
   уведомление, модели оно видно на следующем ходу. Вводится в этой фазе
   вместе с `NoticeKind`.
2. **Рост карт до фазы 3** не принимается: `noticedBefore` очищается, когда
   уведомление поглощено ходом владельца, и дополнительно ограничено по
   размеру (вытеснение самых старых); записи allowlist удаляются вместе с
   драйвером. Никакого неограниченного роста в долгоживущем веб-процессе.
3. **Порядок слияния**: сначала #1053 (`job_id` в job_kill/job_output,
   `asyncJob.shellID`), затем фаза 2 на его основе.
