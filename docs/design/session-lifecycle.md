# Жизненный цикл сессии: admission, drain, lock

Документ адресован тому, кто будет менять `internal/session/run_queue_*.go`
или `internal/session/lock.go` через полгода и не помнит контекста. Он не
пересказывает doc-комментарии кода — он объясняет, зачем код выглядит именно
так, и на что нельзя наступать при следующем изменении.

Контракт `DrainSessionNow` чинили четыре раза подряд за 2026-08-18/19
(коммиты `a1c5f074`, `638bc777`, `b788eb01`, `344dd37c`, плюс промежуточные
`3ba00874`, `0d272f70`, `f627cbac`). Каждый фикс закрывал одну комбинацию
состояний и оставлял открытой соседнюю. Раздел 4 («Кладбище») — самая ценная
часть документа: она объясняет, почему нельзя «упростить» код обратно к
плоскому `(bool, error)`.

## Оглавление

1. [Состояния и переходы](#1-состояния-и-переходы)
2. [Таблица исходов DrainSessionNow](#2-таблица-исходов-drainsessionnow)
3. [Инварианты, которые нельзя нарушать](#3-инварианты-которые-нельзя-нарушать)
4. [Кладбище](#4-кладбище)
5. [Что ещё не покрыто](#5-что-ещё-не-покрыто)
6. [Где напрашиваются границы автомата](#6-где-напрашиваются-границы-автомата)
7. [Расхождения, найденные при написании](#7-расхождения-найденные-при-написании)

---

## 1. Состояния и переходы

Здесь одновременно работают три отдельных, но связанных автомата:
durable-очередь строки (`session_run_queue`), in-process admission
(`admissionEntry`), и межпроцессный OS-лок сессии (`SessionLock` +
generation-токен). Ключевая трудность кода в том, что все три должны
согласованно давать один ответ вызывающему, при этом ни один процесс не
видит состояние двух других напрямую — только через свои файлы/DB-строки.

### 1.1 Durable-строка run_queue (per-row)

Состояния строки в `session_run_queue`: `pending` → `leased` → (удалена —
Ack/TerminalFail) или `pending` (Nack). Переходы:

- `pending → leased`: `LeaseRunQueueEntry` (атомарно на уровне БД — см.
  `run_queue_drain_session.go:DrainSessionNow`, строка с `LeaseRunQueueEntry`
  внутри цикла). Два конкурирующих lease-запроса на одну строку никогда не
  выигрывают оба — это гарантия БД, не in-process.
- `leased → (удалена)`: `AckRunQueueEntry` (успех) или
  `TerminalFailRunQueueEntry` (терминальный отказ, `AlreadyAttempted`). Обе
  функции — плоский `DELETE ... RETURNING id` (см. комментарий в
  `run_queue_drain_session.go` над re-check блоком, ссылающийся на
  `sql/run_queue.sql`) — **после удаления строки нет способа отличить Ack от
  TerminalFail**. Это ядро инварианта №4 в разделе 3.
- `leased → pending`: `NackRunQueueEntry` (обычный retryable-отказ, считает
  attempt) или `NackRunQueueEntryNoAttemptPenalty` (busy/queued/shutdown —
  не считает attempt; см. `run_queue_pump.go:ErrCallQueuedNotExecuted`'s
  doc про `busyBackoffUntil`).
- `leased → leased` (другой владелец): `errLeaseLost` — рассинхронизация
  между локальным рендером аренды и БД: истекла TTL и другой pump-инстанс
  перелицензировал строку, пока текущий исполнитель ещё писал в неё
  (см. `run_queue_pump.go:errLeaseLost`'s doc).

Владелец состояния: любой процесс, у которого есть доступ к БД — durable-
очередь по определению межпроцессна.

### 1.2 In-process admission (per session, per pump instance)

`admitSession` (`run_queue_admission.go`) — единственная точка входа для
ОБОИХ диспетчеров: фонового тика (`processEntry`) и синхронного
`DrainSessionNow`. Состояния: `свободно` (нет записи в `p.inFlight`) →
`занято` (`*admissionEntry` в карте, `done` не закрыт) → `свободно`
(`done` закрыт, запись удалена).

- `свободно → занято`: `admitSession` под `p.inFlightMu`, атомарно —
  «проверить и занять» одна операция, не две (см. `admitSession`'s doc,
  абзац про «P1-1 of the 2026-08-18 review»).
- `занято → свободно`: ТОЛЬКО через closure `release`, которую вернул
  ИМЕННО ТОТ вызов `admitSession`, что занял слот — `sync.Once` внутри
  гарантирует однократность (`admitSession`, строки 142–154). Второй
  вызов `release` — no-op.
- Наблюдатель, проигравший гонку admission (`admitted == false`), получает
  ссылку на ТЕКУЩИЙ `*admissionEntry` прямо из критической секции отказа —
  не отдельным поиском после. Это разрывает ABA-гонку задачи #587 (см.
  раздел 4, дефект №3½ — на самом деле отдельный от четырёх основных, но
  структурно родственный).

Владелец состояния: один процесс (карта `p.inFlight` — не персистентна,
не видна другим процессам). Это единственный автомат из трёх, что живёт
и умирает вместе с процессом.

### 1.3 Межпроцессный OS-лок сессии (per session, cross-process)

`SessionLock` (`lock.go`) — `flock`/`LockFileEx` на файле
`<dataDir>/locks/session-<id>.lock`. Состояния: `свободен` → `занят
(generation=G)` → `свободен` → `занят (generation=G')`...

- `свободен → занят`: `TryAcquireSessionLock` /
  `TryAcquireSessionLockWithOptions` — открывает файл, пытается
  `tryLockFile`. Успех = **единственное авторитетное доказательство**, что
  предыдущий держатель мёртв на уровне ядра ОС (`lock.go:
  acquireSessionLockFileWithOptions`'s doc: «If tryLockFile succeeds here,
  that is authoritative proof the previous holder is gone at the OS
  level»). При успехе генерируется новый `generation` — токен вида
  `"PID-nanoseconds"` (`lock.go:317`), записывается в `.gen`-sidecar.
- `занят → свободен`: `SessionLock.Release()` — сначала снимает OS-лок и
  закрывает fd, ТОЛЬКО ПОТОМ (в фоновой горутине, с бюджетом
  `releaseMetadataCleanupBound` = 50мс) чистит метаданные
  (`clearHolderMetadata`). Порядок «сначала OS-лок, потом diagnostics»
  зафиксирован в doc-комментарии `Release()` как P0-фикс от 2026-08-09 —
  **не проверено по коммит-истории этой сессии**, взято из текста
  комментария.
- mtime-heartbeat (`heartbeat`, каждые `lockHeartbeatInterval`=10с, только
  если было `RecordActivity()`) и `lockStaleDuration`=20с — это ТОЛЬКО
  diagnostics (`sessions locks`/`sessions why`). Явно и многократно
  подчёркнуто в коде: «It must NEVER remove the file or otherwise
  influence acquireSessionLockFile's decision» (`lock.go:
  logStaleDiagnostics`'s doc). Решение о переиспользовании лока принимает
  ИСКЛЮЧИТЕЛЬНО реальная попытка `tryLockFile`.

Владелец состояния: ОС (ядро) — единственный источник истины. Файл и его
sidecar-ы (`.pid`, `.gen`) — это кэш/диагностика поверх ОС-состояния,
которую любой процесс может прочитать, но НЕ должен трактовать как решение
без повторной проверки через реальный лок (см. `sessions_kill.go`'s
`probeThenKillHolder` — убивает только после того, как САМ подтвердил
контеншн через `TryAcquireSessionLock`, а не по чужому prochtению
lock-файла).

### 1.4 Дочерний реестр процесс-групп (generation-fenced, cross-process)

`childgroup_registry_unix.go` добавляет четвёртый, узкий автомат:
регистрация pgid CLI-провайдера (`claude`/`gemini`/`codex`/`qwen`) —
привязанная к generation-токену держателя, который её зарегистрировал.

- `RegisterChildGroup(dataDir, sessionID, pgid, generation)` — вызывается
  живым держателем сессии при спавне CLI-провайдер-процесса. Не проверяет
  ОС-лок сам — просто пишет запись, generation берётся вызывающим ЗАРАНЕЕ.
- `KillRegisteredChildGroups(dataDir, sessionID, victimGeneration)` —
  вызывается ТОЛЬКО `sessions kill`/`sessions reset --force`, ТОЛЬКО пока
  вызывающий держит реальный OS-лок сессии (жёсткое требование в doc-
  комментарии: «CALLER MUST HOLD THE SESSION'S OS LOCK ... across this
  ENTIRE call»). `victimGeneration` — неизменяемый токен, зафиксированный
  в момент, когда `probeThenKillHolder` ДОКАЗАЛ контеншн (через
  `SessionLockBusyError`), ДО того как `forceKillHolder` кого-либо убил
  (`sessions_kill.go:377`, комментарий «Capture the victim's generation
  token HERE, before forceKillHolder signals anything»). Это и есть фикс
  задачи #594 — см. раздел 3, инвариант №2.
- Записи, чья generation не совпадает с `victimGeneration`, НЕ считаются
  доказанно устаревшими — они просто не трогаются в этом sweep'е
  (`GenerationMismatch`, см. doc-комментарий над этим полем — явно
  переписан после #591 blocker P0-2, который трактовал несовпадение как
  «безопасно удалить», что было ошибкой в обратную сторону).

Владелец состояния: связка (сессия, generation) — тот же generation
токен, что и у `SessionLock`, поэтому этот автомат структурно подчинён
разделу 1.3, а не независим от него.

### 1.5 Диаграмма (текстовая)

```
                       ОС-лок сессии (per session, cross-process)
     ┌──────────┐  TryAcquireSessionLock success   ┌──────────────────┐
     │ свободен │ ────────────────────────────────▶│ занят (gen=G)    │
     └──────────┘◀──────────────────────────────── └──────────────────┘
        ▲   ▲         Release() (unlock во flock,          │
        │   │          затем best-effort очистка)          │ RegisterChildGroup(pgid, G)
        │   │                                               ▼
        │   └── probeThenKillHolder: busy ⇒ читает G,   childgroup registry (fenced by G)
        │        затем forceKillHolder убивает PID,        │
        │        ОС автоматически снимает flock            │ KillRegisteredChildGroups(victimG=G)
        │                                                   │   (только держа ОС-лок!)
        └───────────────────────────────────────────────────┘

                admission (per session, per PUMP INSTANCE, in-memory)
     ┌──────────┐   admitSession: check+mark атомарно   ┌───────────────────┐
     │ свободно │ ──────────────────────────────────────▶│ занято            │
     └──────────┘◀──────────────────────────────────────  │ (*admissionEntry) │
                    release(outcome) — ТОЛЬКО тем же       └───────────────────┘
                    closure, once.Do, закрывает done

                run_queue строка (per row, durable/cross-process)
  pending ──LeaseRunQueueEntry──▶ leased ──Ack/TerminalFail──▶ (удалена, DELETE)
     ▲                              │  │
     │        NackRunQueueEntry*    │  └─ errLeaseLost (другой pump перелицензировал)
     └──────────────────────────────┘
```

Ключевая связь между тремя автоматами: **сама попытка выполнить строку
run_queue проходит через `Coordinator.Run`, который внутри пытается
`TryAcquireSessionLock`** (см. `run_queue_pump.go`'s package doc: «Never
acquires the session OS lock itself — it drives execution through
Coordinator.Run»). Т.е. admission — это чисто in-process гейт «не
дай ДВУМ горутинам этого процесса одновременно исполнять одну сессию»;
OS-лок — это гейт «не дай ДВУМ ПРОЦЕССАМ одновременно исполнять одну
сессию». `DrainSessionNow` обязан пройти admission ПЕРЕД лизингом строки
(P1-1 фикс, коммит `0d272f70`) именно потому, что раньше порядок был
обратным и позволял двум исполнениям одной сессии решить, что каждое из
них «единственное».

---

## 2. Таблица исходов DrainSessionNow

`DrainSessionNow` (`run_queue_drain_session.go:403`) возвращает
`(DrainResult, error)`. Четыре значения `DrainResult` (итерация `iota` в
файле, строка 60) взаимоисключающи и исчерпывающи по построению —
`rowLedger.verdict()` (строка 218) — единственное место, где пара
формируется.

| DrainResult | Условие возникновения | Парная ошибка | Что делает вызывающий |
|---|---|---|---|
| `DrainNoWork` | `!ledger.anyExecuted` — ничего не выполнилось в ЭТОМ вызове и ничего не отказало. Может произойти даже если `contended==true` (сессия была занята с самой первой строки) — это НЕ меняется исправлениями #588/#592, преднамеренно оставлено как есть (`verdict()`, строки 219–226). | `nil`, ЕСЛИ вызывающий дошёл сюда без отмены собственного `ctx`; но если `ctx.Err() != nil` в момент проверки и `anyExecuted==false`, возвращается **`ctx.Err()` напрямую** (строки 417–427) — не `nil`. Т.е. `DrainNoWork` НЕ всегда парен с `nil` в возвращаемом значении функции — только внутри `rowLedger.verdict()`, где `err` всегда `nil` для этого случая. | `app_run.go:drainOutcomeError`, ветка `default` — возвращает `originalErr` (исходную отмену) как есть. Это единственный случай, где `DrainSessionNow` не имеет мнения об исходе — «здесь нечего было дренировать». |
| `DrainComplete` | Каждая строка, которую этот вызов выполнил САМ или дождался через `admissionEntry.done`, разрешилась подтверждённым success. `verdict()` строка 253 — единственный `return` без ошибки после проверки `len(l.failed) > 0` и `contended`. | Всегда `nil` — единственная пара `(DrainResult, error)`, которую можно читать как «продолжение полностью завершилось» (см. `DrainResult`'s doc, строки 34–39). | `drainOutcomeError` — defensively проверяет `drainErr != nil` (contract violation, логирует и возвращает `drainErr` как отказ вместо доверия); иначе `nil` → в `RunNonInteractive`'s `finish(nil)` → в JSON-режиме exit code 0. |
| `DrainPartial` | Минимум одна строка выполнилась и подтверждённо закоммитилась, но `stopNow==true` (сессия стала busy/contended — ДРУГОЙ живой владелец) ДО того, как были обработаны все ожидающие строки. `verdict()` строки 246–251: проверяется ПОСЛЕ `len(l.failed) > 0`, т.е. Failed побеждает Partial при равных условиях (см. раздел 3, инвариант №3). | Всегда `ErrDrainIncomplete` (`run_queue_pump.go:251`). | `drainOutcomeError`, ветка `DrainPartial, DrainFailed` — если `drainErr==nil` (contract violation), логирует и синтезирует `ErrDrainFailureUnspecified`; иначе возвращает `drainErr` как отказ run'а. |
| `DrainFailed` | `len(l.failed) > 0` в момент вызова `verdict()` — минимум одна строка, тронутая этим вызовом, закончилась отказом/неподтверждённым исходом, который НЕ был вытеснен более поздним разрешением ДЛЯ ТОЙ ЖЕ строки. Побеждает `DrainPartial` независимо от того, что сделали другие строки (`verdict()`, комментарий строк 227–237). | `mostRecentFailure()` — «самая свежая по порядку вставки строка, ещё присутствующая в `l.failed`» (строки 262–269, детерминировано через параллельный слайс `order`, не через итерацию по map). Если `l.failed` непусто, но `mostRecentFailure()` вернула `nil` (инвариантное нарушение — не должно происходить) — safety net возвращает обёрнутый `ErrDrainFailureUnspecified` (строки 238–243). | Та же ветка `drainOutcomeError`, что и `DrainPartial` — оба ведут себя одинаково с точки зрения вызывающего (что и является причиной, почему их можно объединить в один `case` — оба ДОЛЖНЫ иметь ненулевую ошибку). |

### Почему именно `DrainComplete` — единственный, кто идёт с `nil`

Потому что это единственный исход, для которого у `rowLedger` есть
**позитивное** доказательство коммита: либо `executeEntrySync` вернула
`nil` локально (реальный вызов провайдера в ЭТОМ процессе завершился
успешно и Ack записан), либо наблюдение через `admissionEntry.done`
вернуло `outcomeErr == nil` — а это поле пишется ТОЛЬКО из `executeEntrySync`
или явного `errNoExecutionAttempted` (см. `admissionEntry.err`'s doc,
`run_queue_admission.go:32-63` — «nil is ALSO executeEntrySync's own
"clean commit" return value»). Все остальные три исхода — либо «ничего не
известно» (`NoWork`), либо «известно неполное» (`Partial`), либо «известно
плохое или неизвестное» (`Failed`) — ни у одного из них нет способа
подтвердить success положительно, потому что схема БД физически не
позволяет отличить Ack от TerminalFail постфактум (см. раздел 1.1 и
инвариант №4 ниже).

---

## 3. Инварианты, которые нельзя нарушать

### Инвариант 1 — Идентичность строки при записи отказа

Отказ должен записываться под ключом (row ID), и вытеснить его может
ТОЛЬКО более позднее разрешение ДЛЯ ТОЙ ЖЕ строки. Разные строки никогда
не «лечат» отказ друг друга.

- Механизм: `rowLedger.failed map[string]error`, `recordFailure(rowID, err)`
  добавляет, `recordSuccess(rowID)` удаляет ТОЛЬКО запись под тем же
  `rowID` (`run_queue_drain_session.go:172-192`).
- Безымянные (`observed-admission`) отказы получают синтетический ключ
  `__unattributed_N` (`recordUnattributed`, строки 203-212) именно потому,
  что у них нет стабильного ID, по которому их мог бы вытеснить будущий
  retry — они не могут быть «вылечены» в принципе.
- Пиннится тестами: `TestDrainSessionNow_LocalExecution_TerminalThenSuccess_ReportsFailed`,
  `TestDrainSessionNow_LocalExecution_AckFailureThenSuccess_ReportsFailed`,
  `TestDrainSessionNow_LocalExecution_LeaseLossThenSuccess_ReportsFailed`,
  `TestDrainSessionNow_ObservedAdmission_TerminalThenSuccess_ReportsFailed`,
  `TestDrainSessionNow_ObservedAdmission_AckFailureThenSuccess_ReportsFailed`,
  `TestDrainSessionNow_ObservedAdmission_LeaseLossThenSuccess_ReportsFailed`
  (все в `internal/session/p592_cross_row_identity_test.go`) — каждый
  проверяет пару «строка A отказывает, строка B коммитится» → результат
  `DrainFailed`, не `DrainComplete`.
- Симметричный положительный случай (retry ТОЙ ЖЕ строки лечит её же
  отказ) пиннится `TestDrainSessionNow_SameRowRetrySucceedsClearsErr`
  (`p578_stale_error_test.go:65`).

### Инвариант 2 — Захват идентичности ДО отказа от владения (правило #594)

Любое действие, которое СНАЧАЛА докажет владение чем-то (лок, admission),
а ПОТОМ собирается это владение отдать/разрушить (убить процесс, снять
лок), обязано зафиксировать неизменяемый токен идентичности В МОМЕНТ
доказательства — не читать его заново позже, когда владение уже могло
перейти к другому.

- Место: `sessions_kill.go:probeThenKillHolder`, строка ~377 —
  `victimGeneration := session.ReadLockGeneration(...)` читается СРАЗУ
  после того, как `errors.As(err, &busyErr)` доказал, что СТАРЫЙ держатель
  ещё владеет локом, и ДО вызова `forceKillHolder`, который может убить
  этого держателя и тем самым освободить лок для нового владельца.
- Почему это важно: на Unix убийство PID-держателя освобождает `flock`.
  Новый `crush run --session <id>` может успеть захватить лок и
  зарегистрировать СВОЙ дочерний pgid под НОВЫМ generation прежде, чем
  `KillRegisteredChildGroups` вообще запустится. Если бы `generation`
  читался заново внутри `KillRegisteredChildGroups` (это и была версия
  ДО фикса #594), sweep увидел бы generation НОВОГО владельца как
  «текущий» — и либо убил бы живой процесс нового владельца, либо (в
  версии после первого фикса, но до #591 P0-2) отбросил бы записи
  настоящей жертвы как «generation mismatch» вместо того, чтобы их
  сохранить.
- `KillRegisteredChildGroups` документирует это явным требованием
  вызывающему: `victimGeneration` передаётся АРГУМЕНТОМ, функция
  сознательно НЕ читает generation с диска сама
  (`childgroup_registry_unix.go:474-497`, doc-комментарий явно называет
  прежнюю версию «root cause of a real defect»).
- Тест, пиннящий эту последовательность, существует и живёт в
  `internal/cmd`, а не в `internal/session`:
  `TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive`
  (`internal/cmd/sessions_kill_sweep_unix_test.go`). Он эксплуатирует окно
  зомби: жертва убита, но ещё не reaped, поэтому `IsProcessAlive` истинна и
  `forceKillHolder` крутится в поллинге — за это время тест даёт новому
  владельцу захватить лок и записать своё поколение, ловя момент по смене
  `.gen`-сайдкара, и только затем reap'ает жертву. Гонка превращена в
  детерминированный инструмент вместо `sleep`. Перенос чтения generation с
  «до `forceKillHolder`» на «после» роняет ровно этот тест — проверено
  откатом одного места.
  Два соседних теста в том же файле пиннят порядок свипа в
  `acquireSessionLockForReset` и отказ свипать при живом новом владельце.
- **Внимание при прогоне**: все три начинаются с
  `if testing.Short() { t.Skip }`, а CI гоняет `go test -short`. Плюс файл
  под build-тегом `!windows`. То есть в CI этот инвариант не проверяется
  НИГДЕ — ни на Windows, ни на ubuntu. Прогонять вручную на Linux без
  `-short` (см. задачу #600).

### Инвариант 3 — Failed побеждает Partial

Если в одном вызове `DrainSessionNow` есть И необработанный отказ какой-то
строки, И контеншн после успешного коммита другой строки — результат
`DrainFailed`, никогда `DrainPartial`.

- Механизм: `rowLedger.verdict()` проверяет `len(l.failed) > 0` (строка
  227) СТРОГО ДО проверки `contended` (строка 246) — порядок if-веток, не
  приоритет значений enum.
- Пиннится: `TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError`,
  `TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError`,
  `TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError`
  (`p588_partial_drain_test.go`) — все три комбинируют «строка отказала»
  с «следующая строка встретила contention» и проверяют `DrainFailed`.
  Чистый positive-путь (успех + busy, без отказов) пиннится
  `TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess`
  (тот же файл) — проверяет именно `DrainPartial`, не `DrainComplete`.

### Инвариант 4 — Ноль строк при терминальной записи не публикует событие

Не проверено напрямую в терминах "0 строк" — но связанный инвариант
подтверждён кодом: **терминальная запись строки (Ack/TerminalFail) в этой
схеме НЕ оставляет флага, различающего успех от отказа** — обе операции
физически один и тот же `DELETE ... RETURNING id`
(`run_queue_drain_session.go`, комментарий над re-check блоком, строки
792-799: «AckRunQueueEntry and TerminalFailRunQueueEntry are BOTH a plain
DELETE FROM session_run_queue ... RETURNING id query ... the
terminal_failure COLUMN exists in the schema, but no query ever sets it to
1 on a surviving row»). Практическое следствие: когда re-check
(`lastRowID != ""` ветка, строки 845-873) находит строку `current == nil`
(строка исчезла благодаря ДРУГОМУ процессу), это принципиально
неразличимо между «тот процесс успешно закоммитил» и «тот процесс
терминально уронил работу» — код обязан трактовать это консервативно как
`ErrRowOutcomeUnconfirmed`, НИКОГДА не как success.

- Формулировка «ноль строк не публикует событие» в терминах кода: если
  `!l.anyExecuted` (ничего не выполнилось в этом вызове ВООБЩЕ), `verdict`
  возвращает `DrainNoWork` без обращения к `contended` — то есть ни при
  каких обстоятельствах не синтезирует `DrainComplete`/`DrainFailed` из
  пустого состояния. Это тот же код, что и в инварианте выше (строки
  219-226), просто другой ракурс: «ноль строк» здесь означает «ноль
  РАЗРЕШЁННЫХ строк в ЭТОМ вызове», а не «ноль сообщений в шину
  `message.Service`».
- Пиннится: `TestP593NilFailureViaRecordFailure`,
  `TestP593NilFailureViaRecordUnattributed`,
  `TestP593NilFailureVerdictSafetyNet`,
  `TestP593RecordSuccessClearsNilFailure`
  (`p593_nil_failure_verdict_test.go`) — эти тесты, судя по именам,
  проверяют именно случай nil-ошибки внутри `rowLedger`, что структурно
  соседствует с этим инвариантом, но **не проверено построчно**, что они
  покрывают именно «ноль строк → нет события»; их точное содержимое не
  читалось для этого документа за пределами имён функций.
- Связанный, чётко подтверждённый факт: `ErrRowOutcomeUnconfirmed`'s doc
  прямо формулирует консервативный выбор — «a false "still might have
  failed" costs the operator an unnecessary retry; a false "succeeded"
  silently loses their work, which is the worse of the two failure modes»
  (`run_queue_pump.go:158-163`).

---

## 4. Кладбище

Четыре дефекта, зачинявшиеся один за другим в `DrainSessionNow` и его
окружении. Каждый описан по схеме: что было → почему выглядело правильно
→ чем ловится теперь.

### Дефект 1 — `nil` означал одновременно «зафиксировано» и «не исполнялось»

**Было.** До задачи #575 (коммит `638bc777`) ветка, ожидающая исход
чужого выполнения (`otherEntry.done`), не смотрела на опубликованную
ошибку вообще — любое закрытие `done` читалось как «продолжение
завершилось», и `drained=true` выставлялся безусловно. Более того, ранние
early-return пути в `processEntry` (busy-backoff, worker-pool-full,
attempts-exhausted и т.д.) публиковали через `release(nil)` — то есть
буквально ТОТ ЖЕ `nil`, что и настоящий успешный `executeEntrySync`.

**Почему выглядело правильно.** «Admission освободилась → что-то там
исполнилось и закончилось» — интуитивно кажется достаточным условием: раз
слот занимался, значит был реальный `executeEntrySync`. Но слот занимает
и путь, который решил СРАЗУ отказаться от выполнения (сессия busy, пул
воркеров полон, лизинг проиграл гонку) — занятие admission-слота ⇏
вызов `executeEntrySync`.

**Чем ловится теперь.** Отдельный сентинел `errNoExecutionAttempted`
(`run_queue_pump.go:211`) — единственное значение, которое разрешено
публиковать early-return путям вместо `nil`. `classifyBackgroundOutcome`
(`run_queue_drain_session.go:954`) проверяет его ПЕРВЫМ, до любой другой
классификации, именно чтобы не спутать с настоящим `nil`-успехом
(«nil is ALSO executeEntrySync's own "clean commit" return value»,
строки 963-966). `admissionEntry.err`'s doc прямо формулирует правило:
«a release call that has nothing executed to report must publish
errNoExecutionAttempted, never a bare nil».

### Дефект 2 — Занятая вторая строка после успешной первой возвращала успех

**Было.** До задачи #588 (коммит `b788eb01`) `stopNow`-ветки
(«сессия занята другим владельцем ПРЯМО СЕЙЧАС») в обоих местах — и в
ветке ожидания чужого admission, и в ветке собственного выполнения —
безусловно возвращали `(drained=true, err=nil)`, если хоть одна строка до
этого успела выполниться. Комментарий над `ErrDrainIncomplete` формулирует
это прямо: «DrainSessionNow's stopNow branches collapsing this case to
(drained=true, err=nil) -- indistinguishable from "the whole continuation
finished"».

**Почему выглядело правильно.** Раз строка A уже успешно закоммитилась —
разве не разумно сказать «что-то сделали, зовём это успехом»? Проблема в
том, что `app_run.go`'s `finish()`/`drainOutcomeError` использует ИМЕННО
эту пару как сигнал «продолжение ПОЛНОСТЬЮ завершилось» и превращает её в
exit code 0. Если строка B осталась нетронутой (сессия ушла к другому
живому владельцу до того, как этот вызов до неё добрался), реальная
незавершённая работа молча репортится как полный успех.

**Чем ловится теперь.** Новое значение `DrainPartial` плюс обязательный
`ErrDrainIncomplete` (`run_queue_pump.go:213-251`). `rowLedger.verdict()`
(до появления row-identity в задаче #592, но уже в #588) отличало
«строка выполнилась и всё чисто, но недошли до конца» от «полностью
дошли». Пиннится тестами семейства `p588_partial_drain_test.go`
(раздел 3, инвариант 3).

### Дефект 3 — Чистый Ack поздней строки стирал терминальный отказ ранней

**Было.** До задачи #592 (коммит `344dd37c`) весь ledger был одной
плоской переменной `err error`, без привязки к конкретной строке.
Комментарий в `rowLedger`'s doc называет это явно: «a single named-return
`err` unconditionally overwritten by every classified outcome». Если
строка A терминально отказывала, а строка B (СОВЕРШЕННО НЕ СВЯЗАННАЯ)
чисто коммитилась ПОЗЖЕ в том же вызове, присвоение `err = nil` для B
стирало отказ A.

**Почему выглядело правильно.** Дефект 1 приучил думать в терминах «был ли
error», и естественным следующим шагом было — «раз протекция от ложного
успеха уже стоит на уровне ОДНОЙ строки (через `errNoExecutionAttempted`),
плоский `err`, обновляемый на каждой итерации цикла, выглядит достаточным
для остального». Ускользнула ось, ортогональная первым двум дефектам:
дефекты 1 и 2 — про «что случилось с ОДНОЙ строкой/попыткой», дефект 3 —
про то, что случай СТЕКА строк (несколько durable-continuations,
накопившихся за время простоя) требует различать РАЗНЫЕ строки, а не
только разные исходы одной.

**Чем ловится теперь.** `rowLedger` с картой `failed map[string]error`,
где единственный путь удаления записи — `recordSuccess(ТОТ ЖЕ rowID)`
(раздел 3, инвариант 1). Правило сформулировано в doc-комментарии
`rowLedger` буквально как контракт: «A later outcome may only supersede an
earlier FAILURE if it is a later resolution FOR THE SAME ROW». Это же
решение (не «остановиться на первой ошибке») — сознательный выбор:
`CONTRACT DECISION` комментарий над `DrainSessionNow`
(`run_queue_drain_session.go:357-381`) объясняет, почему «стоп на первой
ошибке» был отвергнут — это хуже для доминирующего случая (один
транзиентный сбой, следующий тик сам всё чинит).

### Дефект 4 — Пара `(DrainFailed, nil)` оставалась выразимой

**Было.** Даже после дефектов 1-3 закрытых, типовая система всё ещё
позволяла В ПРИНЦИПЕ написать код, который вернёт `DrainFailed` с `err ==
nil` (например, если бы кто-то ошибся при рефакторинге и забыл
подставить sentinel в `recordFailure`/`recordUnattributed`, или если бы
`mostRecentFailure()` вернула `nil` из-за логической ошибки в `order`).
Само по себе `(DrainResult, error)` как пара типов не запрещает такую
комбинацию компилятором — и на стороне потребителя (`app_run.go`) ничего
не мешало переслать `nil` дальше как есть.

**Почему выглядело правильно.** После задачи #592 казалось, что
row-identity уже закрывает все дыры — `rowLedger.verdict()` сама решает,
какой `error` парен с каким `DrainResult`, и выглядело, что раз ОДНА
функция формирует обе части пары, рассинхронизация невозможна.
Пропущенная ось: потребитель (`RunNonInteractive`/`drainOutcomeError`) —
ОТДЕЛЬНАЯ функция в другом пакете, и её код доверял входным данным без
проверки контракта.

**Чем ловится теперь.** `drainOutcomeError` (`app_run.go:251-281`)
защитно проверяет обе стороны контракта: для `DrainComplete` — что
`drainErr == nil`, ИНАЧЕ логирует «contract violation, treating as
failure» и возвращает `drainErr` как отказ; для `DrainPartial`/
`DrainFailed` — что `drainErr != nil`, ИНАЧЕ логирует и синтезирует
`session.ErrDrainFailureUnspecified`. Симметрично на стороне
`rowLedger.verdict()` есть собственный safety net (раздел 2, строки
238-243) на случай, если `l.failed` непусто, но `mostRecentFailure()`
почему-то вернула `nil`. Пиннится `p593_nil_failure_verdict_test.go`
(в `internal/session`) и `TestDrainOutcomeError_NilGuard`
(`internal/app/p593_drain_outcome_nil_guard_test.go`) — **обе стороны
защиты покрыты тестами по обе стороны границы пакетов**, что и является
структурным ответом на «зачем защита в ДВУХ местах, если она и так
гарантирована одной функцией» — потому что дефект был именно в том, что
гарантия одной функции не транзитивна через границу пакета без явной
проверки на другой стороне.

---

## 5. Что ещё не покрыто

- **Сквозной тест через `App.New()` невозможен из-за гонки тика фонового
  pump'а.** Зафиксировано как открытая задача #600 («Run the dynamic
  release gate none of the five reviews could run») — продакшн `tick()`
  срабатывает каждые `RunQueuePumpInterval` = 3с
  (`run_queue_pump.go:33`), и любая инжектируемая через `App.New()` строка
  рискует быть подхвачена фоновым тиком РАНЬШЕ, чем тест успевает
  спровоцировать нужную гонку с `DrainSessionNow` детерминированно.
  Существующие E2E-тесты (`p421_p0_1_interrupt_live_continuation_test.go`)
  используют реальный `App`, реальный `RunQueuePump`, но избегают этой
  гонки за счёт того, что тестируемый сценарий (interrupt → durable
  continuation → `DrainSessionNow` подхватывает её раньше фонового тика)
  структурно требует БЫСТРОГО дренирования, а не состязания с тиком —
  остаётся открытым вопрос, можно ли вообще детерминированно
  воспроизвести гонку «фоновый тик и `DrainSessionNow` видят строку
  одновременно» без test-seam на сам интервал тика (`TestTick` в
  `RunQueuePumpConfig` существует и, вероятно, решает именно это — **не
  проверено**, использует ли текущий тест этот seam или нет).

- **Orphaned child-group entries могут стать окончательно недостижимыми**
  (задача #602, «Orphaned child-group entries are now unreachable
  forever» — открыта на момент написания). Судя по логике
  `KillRegisteredChildGroups` (раздел 1.4): запись с несовпадающей
  generation остаётся в реестре НАВСЕГДА, пока не найдётся будущий sweep,
  ЧЬЯ `victimGeneration` совпадёт именно с этой записью. Если процесс,
  зарегистривавший её, умер, а никто больше никогда не станет "victim" с
  этой конкретной generation (например, сессия просто больше никогда не
  становится predметом `sessions kill` в состоянии contention) — запись
  зависает в файле реестра бессрочно. Это ПРОЧИТАНО из кода
  (`GenerationMismatch`'s doc, раздел 1.4) и согласуется с формулировкой
  задачи #602 в TaskList, но полный сценарий воспроизведения не
  прослежен до конца в рамках этого документа — оставлено как отдельная
  задача, не как находка этого документа.

- Не найдено (за пределами написанного) других структурных пробелов при
  чтении затронутых файлов; специально не выдумывались.

---

## 6. Где напрашиваются границы автомата

Не рекомендация к рефакторингу — только описание, где по факту проходят
(и куда, судя по структуре кода, тяготеют) границы между
«диспетчер/оркестрация» и «чистая логика конечного автомата».

- **`run_queue_drain_session.go` (1024 строки) уже структурно разделён**
  на три вещи, которые физически могли бы быть тремя файлами: (1)
  типы-значения `DrainResult`/`rowLedger` и их методы (строки 1-269 —
  чистая, тестируемая без единого канала/горутины логика конечного
  автомата, УЖЕ покрыта отдельным семейством тестов `p593_*`); (2) сама
  функция `DrainSessionNow` (строки 271-877) — оркестрация с циклом,
  `select`, работой с БД и admission; (3) вспомогательные предикаты
  `isOrdinaryRetryableOutcome`/`classifyBackgroundOutcome`/
  `isSessionLockBusyErr` (строки 879-1024) — тоже чистая классификация
  ошибок, не требующая состояния функции. Граница (1) и (3) против (2) —
  «состояние конечного автомата» против «шаги, которые его меняют» — уже
  проведена структурно комментариями и порядком объявлений, просто не
  файлами.

- **`agent_turn.go`** не читался для этого документа (не входил в список
  ключевых файлов задачи) — про его внутренние границы утверждений не
  делается.

- **`app_run.go`'s `drainOutcomeError`** (30 строк, строки 246-281) — уже
  сама по себе маленькая чистая функция-мост между типами `session` и
  решением о финальном `error` для `RunNonInteractive`. Она физически
  отделена от 862-строчного `app_run.go` только тем, что живёт в том же
  файле — перенос в отдельный файл (например, `drain_outcome.go`) не
  менял бы поведение, только видимость границы. Решение — за оператором.

- Внутри `run_queue_pump.go` (539 строк) большая часть объёма —
  doc-комментарии к константам/сентинелам, не исполняемый код (см. секцию
  сентинелов `ErrCallQueuedNotExecuted`...`ErrDrainFailureUnspecified`,
  строки 90-252 — это ~160 строк комментариев на 6 объявлений ошибок).
  Собственно код структуры `RunQueuePump` и `NewRunQueuePump` — компактны.
  Это не повод выносить их в отдельный файл — комментарии здесь несут
  значимую часть контракта (см. раздел 4 «Кладбище», где именно эти
  doc-комментарии — единственный письменный источник причин четырёх
  фиксов).

---

## 7. Расхождения, найденные при написании

Ни одного случая, где текст doc-комментария противоречил бы поведению
кода, обнаружено не было при чтении перечисленных файлов для этого
документа. Единственная зона неопределённости — не расхождение, а
пробел в подтверждении (см. раздел 3, инвариант 2: не найден
конкретный regression-тест, пиннящий последовательность «читать
generation → потом убивать» в `sessions_kill.go`, хотя сам код и его
doc-комментарий взаимно согласованы).

Отдельно стоит отметить формально проверяемый факт, не расхождение, а
уточнение: `DrainResult`'s doc (`run_queue_drain_session.go:14-53`)
называет `run_queue_drain_session.go` файлом «~860 строк» нигде явно —
это число фигурировало в постановке задачи, а не в самом коде; текущий
размер файла на момент написания — 1024 строки (проверено `wc -l`).
