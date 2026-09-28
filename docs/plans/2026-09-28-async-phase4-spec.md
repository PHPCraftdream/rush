# Фаза 4: реестр работы в SQLite, восстановление, наблюдаемость — спецификация реализации

Статус: спецификация к утверждённому дизайну
(`docs/plans/2026-09-27-async-structured-concurrency.md`, §5 «Долговечность и
наблюдаемость», строка миграции «Фаза 4»), реализация не начата. Задача
**#1048**. Опирается на фазы 1–3, уже слитые в это дерево (`main@998fd465`):
`internal/agent/work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
`work_ledger_timeout.go`, `coordinator_work_scope.go`, `coordinator_wake.go`,
`coordinator_subagent_drivers.go`, `async_tool.go`. Инварианты —
`docs/async-invariants.md`; эта спецификация обязана переанкерить строки,
которые меняет (§8).

Каждая строка ниже сверена с кодом на HEAD worktree `phase4-spec`
(`main@998fd465`); где сверить не удалось — написано «не проверено», а не
предположение. Код не менялся и не запускался — это только спецификация.

## 0. Что учтено дополнительно к дизайн-документу

### 0.1 Что входит

Ровно пп. 1–6 задания (см. заголовок): схема (миграция+sqlc), учёт
владения in-memory/DB и ack-гейт как свойство БД; host identity + lease +
`interrupted`-восстановление; идемпотентный старт (#1038) и старт делегации;
кросс-процессные читатели (`sessions why/list`, веб, замена
`descendant_liveness.go`); ограниченное хранение терминальных строк,
безопасность миграции, откат; пошаговый план реализации с тестами и
файловым планом. Плюс явно поставленные оркестратором задачи: **#1040**
(нет терминальной записи для прерванных async-задач), **#1038**
(`CreateTaskSession` не идемпотентен), **BL-4/BL-5** (см. §0.3 — точная
привязка к идентификаторам бэклога), **#1063** (различимая причина отмены;
гонка `job_kill` с естественным завершением обязана сохранить настоящий
исход — см. §3.4, это единственный пункт задания, для которого при чтении
кода нашёлся реальный, воспроизводимый по трассировке дефект, а не только
отсутствующая durability).

### 0.2 Что НЕ входит

- **`loop`/`wakein`/`wakeon`, надзор, долговечный планировщик (#1025)** —
  фаза 5. Эта спецификация заводит ровно ту таблицу и тот lease-механизм,
  которые дизайн-документ прямо называет общими («долговечность расписаний
  — та же таблица и тот же lease, что у задач»), но не добавляет
  `jobKindLoop`/`jobKindTimer` — по той же причине, что фаза 3 не добавила
  `HoldsScope()`: ни один вызывающий эту фазу их не создаёт (недостижимый
  код). Точка расширения зафиксирована в §1.2 (колонка `kind`) и §8.
- **Надзор (#1043) и веб-панель (#1058)** реализуются параллельно (задание:
  «being implemented in parallel on top of the in-memory ledger snapshot»).
  На HEAD этого дерева существует ветка `supervision` (`998fd465`, ещё без
  своих коммитов на момент чтения) — эта спецификация её **не трогает и не
  читает** (чужая параллельная работа, вне разрешённого каталога). Вместо
  координации через код эта спецификация проектирует читающий API (§4) так,
  чтобы обе будущие фичи могли использовать его без знания о durable-слое:
  см. §4.5 (снимок живой работы, потребляемый как in-memory, так и durable
  источником).
- **`stop_agent`/`inject_agent`/явные операции «остановить работу» для
  модели** — план пробуждений, этап 3 (#1024), не эта фаза. §4.4 предлагает
  ТОЛЬКО человеческий CLI-эквивалент (`rush sessions jobs`/`rush jobs
  kill`), не новый агентский инструмент.
- **Само восстановление ПРЕРВАННОГО ХОДА** (`session_run_queue`,
  `coordinator_run_queue_call.go`) — отдельный, уже существующий и не
  трогаемый механизм (durable replay хода, не async-задачи). Эта фаза
  **переиспользует его проверенные идиомы** (lease по TTL, `ON CONFLICT DO
  NOTHING RETURNING`, scoped-by-owner Ack/Nack), но не касается его кода
  или таблицы `session_run_queue` — см. §1.6 за явным сравнением.
- **Замена файлового lock+heartbeat сессий** (`internal/session/lock*.go`).
  CLAUDE.md и дизайн-документ («Что сохраняется…») фиксируют его как
  намеренный, боевой механизм — не трогается. Заводится **отдельная**,
  специфичная для этой фазы пара host-identity/heartbeat (§2), которая
  живёт в БД, а не в файлах — они решают разные вопросы («жив ли держатель
  ЭТОЙ сессии сейчас» против «жив ли процесс, начавший ЭТУ async-задачу»)
  и один не может заменить другой без потери гарантии, которую даёт другой.

### 0.3 BL-4/BL-5 — точная привязка (не проверено дословное совпадение ID)

`docs/plans/2026-09-25-session-fixes-review-backlog.md` использует ID вида
`BL-2026-09-25-N`, не `BL-4`/`BL-5`. Дизайн-документ и это задание пишут
`BL-4`/`BL-5` без даты-префикса. Прямого текстового совпадения нет — это
**вывод по контексту**, не найденная запись:

- Design doc называет `BL-4`/`BL-5` в диагнозе «Комбинации читаются
  неатомарно… опрос, страховочные тикеры» и в пункте 5 («Состояние только в
  памяти процесса… #1040, BL-4, BL-5») и фиксирует, что фаза 4 «закрывает…
  BL-4, BL-5; удаляет `descendant_liveness.go`».
- Единственные две записи бэклога, чей anchor — `descendant_liveness.go`,
  это **BL-2026-09-25-4** («False-alive edges in descendant liveness», P3,
  `internal/session/descendant_liveness.go:119`, PID-reuse/only-just-
  released ложные срабатывания heuristic'и) и **BL-2026-09-25-5**
  («`annotateLiveDescendantWork` cost per session list reply», P3,
  `internal/server/handlers_sessions.go:250`, O(sessions×descendants) на
  каждый ответ списка).

Обе описывают ровно тот файл, который design doc называет удаляемым этой
фазой, и обе исчезают структурно, если `descendant_liveness.go`'s
PID/mtime-эвристика заменяется прямым чтением durable-таблицы: (а)
false-alive исчезает, потому что «жив» больше не выводится из
PID/mtime-эвристики, а читается из host-lease-факта (§2); (б) стоимость на
ответ списка исчезает, потому что один `JOIN`-запрос по индексу заменяет
BFS-обход с одним `InspectSessionLock`-вызовом на узел. Вывод: `BL-4` =
`BL-2026-09-25-4`, `BL-5` = `BL-2026-09-25-5` — принято как рабочая
гипотеза с высокой обоснованностью, но **не подтверждено** буквальным
текстом ни в одном прочитанном документе; если у оператора есть другая
привязка этих ID, часть §4 (кросс-процессные читатели), закрывающая их,
не меняется по существу — меняется только эта сноска.

## 1. Схема (миграция + sqlc)

### 1.1 Отношение in-memory ↔ БД: write-through, не единственный источник

`workLedger` (в памяти, один процесс) **остаётся единственным источником
истины для решений внутри своего процесса** — CAS-переходы
(`transitionToTerminal`), маршрутизация доставки (`deliverLocked`),
busy-гейт делегации (`childScopeDrained`) не переезжают в БД и не начинают
советоваться с ней на горячем пути. Причина — явный риск design doc'а:
«Нагрузка на SQLite (у оператора уже были простои БД дольше 45 с)». Держать
`l.mu` во время SQL-вызова превратило бы ЛЮБОЙ простой БД в остановку ВСЕХ
`Start`/`finish`/`cancelSession` процесса одновременно — тот же принцип,
по которому `timeoutService` уже держит СВОЙ отдельный `s.mu`, а не
`l.mu` (`work_ledger_timeout.go:59-62`), и по которому `recheckChild`'s DB-
рефреш (`coordinator.refreshSubAgentCompletion`) уже выполняется СНАРУЖИ
`l.mu` (`work_ledger_delegation.go:70-78`).

БД — **write-through проекция**, применяемая синхронно (та же горутина,
её же ошибка возвращается вызывающему или логируется — см. ниже), но
**вне** `l.mu`, в ровно трёх точках жизни задачи:

| Момент in-memory | DB-операция | Где вызывается (новый код) | Блокирует ли `l.mu` |
|---|---|---|---|
| `Start`, новый ключ | `INSERT … ON CONFLICT (owner_session_id, tool_call_id) DO NOTHING RETURNING *` (durable claim, §3) | `asyncTool.Run`, ДО вызова `workLedger.Start` (не внутри него) | нет — до входа в `l.mu` вообще |
| `onToolResult`'s `acknowledged` (ack-гейт) | `UPDATE async_jobs SET announced=1 …` В ТОЙ ЖЕ транзакции, что `INSERT INTO messages` для tool-result-сообщения | `agent_turn_stream.go:383-394`, новая функция `RecordAsyncJobAnnounced` (§1.4) | нет — транзакция коммитится ДО вызова `workLedger.acknowledged`, которая держит `l.mu` как и сегодня |
| `finish`/`cancelSession`/`handleTimeout`'s терминальный переход, ПОСЛЕ того как `transitionToTerminal` вернул `true` (выиграл CAS) | `UPDATE async_jobs SET state=…, notice_kind=…, result_summary=…, result_is_error=…, updated_at=… WHERE owner_session_id=? AND tool_call_id=? AND state='running'` | новая `work_ledger_durable.go`, вызывается СРАЗУ ПОСЛЕ разблокировки `l.mu`, до или после `onWebDone` — порядок не важен, обе стороны независимы | нет |

Если write-through DB-вызов **проигрывает** (SQLite занята,
`SQLITE_BUSY`, простой): текущий процесс **не откатывает** и не блокирует
in-memory переход — этот факт уже случился и уже доставлен модели/владельцу
по in-memory пути (ASYNC-04 для ЭТОГО процесса не деградирует). Ошибка
логируется `slog.Warn` (не Debug — тот же принцип ASYNC-09 уже
устанавливает) с полем `owner_session_id`/`tool_call_id`/`err`. Последствие
рассинхронизации — durable-строка может на какое-то время остаться в
состоянии `running`, хотя в памяти задача уже терминальна и доставлена
— это НЕ дыра в наблюдаемости: следующий host-heartbeat-тик того же
процесса (§2.2) делает лучший эффорт повторной записи для строк со своим
`host_id`, обнаруженных в состоянии `running`, чей in-memory аналог уже не
существует (это дешёвая, идемпотентная сверка — не полноценная очередь
ретраев; см. §7 риск).

**Ack-гейт как свойство БД (задание, п. 1).** Сегодня «объявлено» —
булево поле `asyncJob.announced`, устанавливаемое `acknowledged`
(`work_ledger.go:339-357`) ПОСЛЕ того как `onToolResult`
(`agent_turn_stream.go:383-394`) уже вызвал `ts.a.messages.Create(...)` —
две отдельные, не атомарные операции: `messages.Create` коммитит
независимо, `acknowledged` — отдельный, чисто in-memory, вызов мгновением
позже. Разрыв между ними сегодня не наблюдаем ВНЕ процесса (ack-гейт живёт
только в памяти), но фаза 4 делает его наблюдаемым СНАРУЖИ через колонку
`announced` — так что тот же разрыв, будь он оставлен неатомарным на
уровне БД, дал бы окно: tool-result уже виден в транскрипте (модель
считает задачу начатой), а durable `announced` — ещё `0`, потому что
процесс упал МЕЖДУ двумя записями. Восстановление (§2.3), читающее
`announced=0` как «ничего не сообщено, не доставлять», в этом окне
скрыло бы уже показанный tool-result — расхождение с ASYNC-05 наоборот
(не «доставлено раньше объявления», а «объявлено, но БД считает иначе»).
Поэтому §1.4 делает `INSERT INTO messages` (tool-result) и `UPDATE
async_jobs SET announced=1` ОДНОЙ транзакцией — «ack-гейт становится
свойством БД» дословно по формулировке design doc'а.

### 1.2 Таблица `async_jobs`

Новая миграция `internal/db/migrations/20260929000001_add_async_job_ledger.sql`
(следующий свободный timestamp после `20260928000001_add_notice_kind_to_messages.sql`,
сверено `ls internal/db/migrations`).

```sql
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS async_hosts (
    id           TEXT PRIMARY KEY,      -- uuid v4, сгенерирован один раз за жизнь процесса
    pid          INTEGER NOT NULL,      -- os.Getpid(), только для диагностики/отображения
    label        TEXT NOT NULL DEFAULT '', -- 'cli'|'web', для `rush sessions jobs`
    started_at   INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL       -- обновляется heartbeat-тиком (§2.2); равен started_at при INSERT
);

CREATE INDEX IF NOT EXISTS idx_async_hosts_heartbeat ON async_hosts (heartbeat_at);

CREATE TABLE IF NOT EXISTS async_jobs (
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    tool_call_id     TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('command', 'agent', 'fetch')),
    input_hash       TEXT NOT NULL,        -- sha256(call.Input); реюз id с ДРУГИМ input -- другой вызов, не идемпотентный повтор (см. §3.1)
    child_session_id TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    origin_cli       INTEGER NOT NULL DEFAULT 0, -- byte-for-byte то же, что asyncJob.cli, для маршрутизации при восстановлении
    state            TEXT NOT NULL CHECK (
        state IN ('running', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')
    ),
    -- notice_kind reuses message.NoticeKind's vocabulary (migration
    -- 20260928000001) plus two values this phase introduces
    -- ('session_cancel', 'interrupted') -- see §3.4/§3.5 for why 'cancelled'
    -- alone is not enough to tell job_kill and CancelAll/session-cancel apart
    -- (#1063), and §2.3 for 'interrupted'. '' for an ordinary finish/fail.
    notice_kind      TEXT NOT NULL DEFAULT '',
    host_id          TEXT NOT NULL REFERENCES async_hosts (id),
    announced        INTEGER NOT NULL DEFAULT 0, -- ack-gate (§1.1/§1.4); 1 only inside the SAME tx as the tool-result message insert
    lease_expires_at INTEGER,             -- NULL once state != 'running'; renewed by the owning host's heartbeat tick (§2.2)
    deadline_at      INTEGER,             -- explicit per-call timeout deadline (TimeoutSpec.Deadline), NULL if none
    timeout_kind     TEXT,                -- 'wake_only'|'terminate_and_wake', NULL if none
    result_summary   TEXT,                -- tools.TruncateOutput'd content; NULL while running
    result_is_error  INTEGER,
    delivered_at     INTEGER,             -- set once a terminal notice was durably persisted as a session message (§2.3/§3.3); NULL while running or if never announced (see abort, §3.2)
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (owner_session_id, tool_call_id)
);

CREATE INDEX IF NOT EXISTS idx_async_jobs_lease ON async_jobs (lease_expires_at) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS idx_async_jobs_child ON async_jobs (child_session_id) WHERE child_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_state ON async_jobs (owner_session_id, state);
CREATE INDEX IF NOT EXISTS idx_async_jobs_retention ON async_jobs (state, updated_at) WHERE state != 'running';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_async_jobs_retention;
DROP INDEX IF EXISTS idx_async_jobs_owner_state;
DROP INDEX IF EXISTS idx_async_jobs_child;
DROP INDEX IF EXISTS idx_async_jobs_lease;
DROP TABLE IF EXISTS async_jobs;
DROP INDEX IF EXISTS idx_async_hosts_heartbeat;
DROP TABLE IF EXISTS async_hosts;
-- +goose StatementEnd
```

Отличия от буквального списка задания («id, owner session, tool call id,
kind, input hash, child session, state, cancel reason, host id, lease
expiry, timestamps, result summary»):

- **PK — составной `(owner_session_id, tool_call_id)`, не отдельный
  `id`.** Ключ идемпотентности (задание §3, #1038) — это ИМЕННО эта пара
  (совпадает с in-memory `sessionJobs.jobs` ключом,
  `work_ledger.go:77,178`); заводить отдельный суррогатный `id` поверх нёс
  бы вторую уникальную колонку без потребителя — `session_run_queue`'s
  отдельный `id` там оправдан тем, что ЕГО идемпотентный ключ генерируется
  ВЫЗЫВАЮЩИМ на каждый enqueue независимо от кортежа полей; здесь кортеж
  ключевых полей УЖЕ стабилен и уникален по конструкции (`toolCallID`
  привязан к конкретному tool-call в конкретной сессии).
- **«cancel reason» реализован как `notice_kind`, не отдельная
  `cancel_reason` колонка.** Обоснование — §3.4/§3.5: значения нужны не
  только для `state='cancelled'` (`job_stopped` vs `session_cancel`), но и
  переиспользуют уже существующий, провалидированный на message-уровне
  словарь (`message.NoticeKind`, миграция `20260928000001`) — заводить
  параллельный, слегка другой словарь на соседней таблице было бы
  дублированием источника истины без выгоды.
- **`origin_cli`, `deadline_at`/`timeout_kind`, `input_hash`,
  `delivered_at`** добавлены сверх буквального списка: без `origin_cli`
  восстановление (§2.3) не могло бы воспроизвести маршрутизацию
  `deliverLocked`'s CLI/web-условия для ready-queue-доставки; без
  `deadline_at`/`timeout_kind` восстановленная строка теряла бы
  информацию, нужную операторским командам (§4.4) для показа «сколько ещё
  оставалось»; `input_hash` — прямое перенесение in-memory-инварианта
  `existing.input != input` → отказ (`work_ledger.go:179-181`) на durable
  слой (см. §3.1); `delivered_at` — отдельно от `state`, потому что
  «доставлено» и «терминально» не одно и то же для восстановленных строк
  (терминальный переход и доставка уведомления — два разных момента, §2.3).

### 1.3 sqlc-запросы

`internal/db/sql/async_jobs.sql` (новый файл, следующий по алфавиту после
`stats.sql`/`run_queue.sql` в `internal/db/sql/`):

```sql
-- name: RegisterAsyncHost :one
INSERT INTO async_hosts (id, pid, label, started_at, heartbeat_at)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: TouchAsyncHostHeartbeat :execrows
-- Renews this host's own heartbeat AND every running job it currently owns,
-- in one statement each (two statements total, same idiom as
-- RenewRunQueueLease scoped by owner). execrows on the jobs UPDATE reports
-- how many rows this tick actually touched -- purely diagnostic (logged at
-- Debug on a sudden drop to 0 while jobs are believed running), not a
-- correctness signal.
UPDATE async_hosts SET heartbeat_at = ? WHERE id = ?;

-- name: RenewAsyncJobLeasesForHost :execrows
UPDATE async_jobs SET lease_expires_at = ?, updated_at = ?
WHERE host_id = ? AND state = 'running';

-- name: ClaimAsyncJob :one
-- Durable idempotent start (#1038, §3.1). ON CONFLICT DO NOTHING mirrors
-- EnqueueRunQueueEntry's own P2-1 rationale verbatim: a caller retrying the
-- same (owner_session_id, tool_call_id) after a crash must not error just
-- because an earlier attempt already committed the row. Returns zero rows
-- (sql.ErrNoRows) on conflict; the caller (asyncTool.Run) treats that as
-- "already claimed -- read the existing row instead", never as a failure.
INSERT INTO async_jobs (
    owner_session_id, tool_call_id, kind, input_hash, child_session_id,
    origin_cli, state, host_id, announced, lease_expires_at,
    deadline_at, timeout_kind, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, 'running', ?, 0, ?, ?, ?, ?, ?
)
ON CONFLICT (owner_session_id, tool_call_id) DO NOTHING
RETURNING *;

-- name: GetAsyncJob :one
SELECT * FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: MarkAsyncJobAnnounced :execrows
-- Issued through the SAME *sql.Tx as the tool-result message INSERT
-- (§1.1/§1.4) -- never called standalone in production. Scoped to
-- state='running': a job that raced to a terminal state before its own
-- "started" tool-result committed (a very fast command) must not have this
-- overwrite a terminal row's announced flag out of order; the in-memory
-- deliverLocked path already handles that ordering correctly and this
-- write is a best-effort mirror of it, not its source of truth.
UPDATE async_jobs SET announced = 1, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND state = 'running';

-- name: RecordAsyncJobTerminal :execrows
-- Best-effort mirror of an in-memory transitionToTerminal that already won
-- its CAS (§1.1). Scoped to state='running' so a repeated/racing call from
-- a process that lost the SAME in-memory CAS (should not happen -- this is
-- only ever called after l's own CAS already decided -- kept as a second,
-- cheap guard, same reasoning as MarkAsyncJobAnnounced above) is a no-op,
-- not a corruption of an already-terminal row.
UPDATE async_jobs
SET state = ?, notice_kind = ?, result_summary = ?, result_is_error = ?,
    lease_expires_at = NULL, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND state = 'running';

-- name: MarkAsyncJobDelivered :execrows
UPDATE async_jobs SET delivered_at = ? WHERE owner_session_id = ? AND tool_call_id = ? AND delivered_at IS NULL;

-- name: DeleteUnannouncedAsyncJob :execrows
-- Mirrors workLedger.abort (work_ledger.go:364-378): the "started" tool
-- result write itself failed, so there is nothing to preserve (ASYNC-05).
DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0;

-- name: ListStaleAsyncJobs :many
-- The recovery sweep's only query (§2.3): every row still 'running' whose
-- lease has expired, regardless of which host_id owned it or whether that
-- host row still exists at all (a host row is never deleted -- see §2.1 --
-- but a defensive LEFT JOIN-free query is simpler and the lease column
-- alone is authoritative, same as ListStaleLeasedRunQueueEntries).
SELECT * FROM async_jobs WHERE state = 'running' AND lease_expires_at < ?
ORDER BY lease_expires_at ASC;

-- name: ListAsyncJobsForOwner :many
SELECT * FROM async_jobs WHERE owner_session_id = ? ORDER BY created_at ASC;

-- name: ListRunningAsyncJobsForOwners :many
-- Cross-process reader (§4): the durable replacement for
-- session.LiveDescendants' per-descendant lock inspection. Takes the
-- CALLER-computed set of owner ids (the descendant walk itself stays in Go,
-- §4.1 -- sqlite's own recursive CTE support is not used here to avoid a
-- second, harder-to-test traversal implementation living in SQL).
SELECT * FROM async_jobs WHERE owner_session_id IN (sqlc.slice('owner_ids')) AND state = 'running';

-- name: PurgeTerminalAsyncJobsOlderThan :execrows
-- Bounded retention (§5): a terminal row older than the cutoff is deleted
-- outright, same "no soft-delete" precedent as sessions gc's own row
-- deletion. delivered_at IS NOT NULL guards against purging a row whose
-- terminal notice was never actually confirmed delivered (should not
-- normally happen -- delivery is attempted synchronously at the same
-- transition -- but purging an undelivered terminal row would silently
-- erase the one thing the recovery sweep still needs to retry).
DELETE FROM async_jobs
WHERE state != 'running' AND delivered_at IS NOT NULL AND updated_at < ?;
```

Генерируется в `internal/db/async_jobs.sql.go` тем же `sqlc generate`,
что и остальные `*.sql.go` (не проверено — команда генерации самого sqlc
не запускалась этой спецификацией; предполагается идентичной команде,
которой сгенерирован `run_queue.sql.go`).

### 1.4 Ack-гейт: транзакция, связывающая tool-result и `announced`

Место сегодня: `internal/agent/agent_turn_stream.go:383-396`
(`onToolResult`):

```go
_, createMsgErr := ts.a.messages.Create(ts.ctx, sessionID, message.CreateMessageParams{...})
if ts.a.asyncJobs != nil {
    if createMsgErr != nil {
        ts.a.asyncJobs.abort(sessionID, result.ToolCallID)
    } else {
        ts.a.asyncJobs.acknowledged(sessionID, result.ToolCallID)
    }
}
```

`ts.a` — `*sessionAgent` (`internal/agent/agent.go:577`), который уже
несёт `messages message.Service` (`agent.go:656`) и `asyncJobs *workLedger`
(`agent.go:583`), но **не** `session.Service` — ни один существующий
`sessionAgent`-конструктор не передаёт его (не проверено исчерпывающе —
`grep sessions\b` по `agent.go` не дал совпадений на уровне поля; при
реализации нужно добавить `sessions session.Service` полем `sessionAgent`,
заполняемым координатором из `c.sessions`, симметрично `messages`).

Транзакционная запись живёт в `internal/session` (пакет уже импортирует
`internal/message`, см. `session_lifecycle.go:14`; обратной зависимости
`message → session` нет — сверено `grep -rn PHPCraftdream/rush/internal/session
internal/message/*.go`, ноль совпадений). Механизм:

```go
// internal/message/message.go — новый метод, минимальная добавка

// CreateWithQuerier is Create against an EXPLICIT db.Querier instead of
// s.q -- lets a caller in a different package fold a message insert into a
// larger transaction it owns (phase 4's async-job ack-gate,
// internal/session's RecordAsyncJobAnnounced). Create becomes a thin
// wrapper: CreateWithQuerier(ctx, s.q, sessionID, params).
func (s *service) CreateWithQuerier(ctx context.Context, q db.Querier, sessionID string, params CreateMessageParams) (Message, error)
```

```go
// internal/session/async_job_ack.go (новый файл)

// RecordAsyncJobAnnounced persists the tool-result message for
// (ownerSessionID, toolCallID) AND marks the durable async_jobs row
// announced=1, in ONE transaction (§1.1: the ack-gate as a DB property).
// Either both commit or neither -- there is no window where the tool
// result is visible in the transcript but the durable ack-gate still reads
// announced=0, or vice versa.
//
// A conflict with #1038's own idempotent claim (job already announced by a
// DIFFERENT, since-dead attempt) cannot happen here: acknowledged only
// ever fires for a job THIS process's own workLedger.Start just created in
// memory (existing=false branch, async_tool.go:82-94) -- a replayed call
// that hit the existing=true branch never spawns t.run and never reaches
// onToolResult's Create call a second time for the SAME toolCallID.
func (s *service) RecordAsyncJobAnnounced(
    ctx context.Context,
    messages message.Service,
    ownerSessionID, toolCallID string,
    msgParams message.CreateMessageParams,
) (message.Message, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return message.Message{}, fmt.Errorf("begin ack transaction: %w", err)
    }
    defer tx.Rollback() //nolint:errcheck
    qtx := s.q.WithTx(tx)
    msg, err := messages.CreateWithQuerier(ctx, qtx, ownerSessionID, msgParams)
    if err != nil {
        return message.Message{}, err
    }
    now := time.Now().Unix()
    if _, err := qtx.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
        OwnerSessionID: ownerSessionID, ToolCallID: toolCallID, UpdatedAt: now,
    }); err != nil {
        return message.Message{}, fmt.Errorf("mark async job announced: %w", err)
    }
    if err := tx.Commit(); err != nil {
        return message.Message{}, fmt.Errorf("commit ack transaction: %w", err)
    }
    return msg, nil
}
```

`onToolResult`'s правка — минимальна, обёртка существующей ветки:

```go
var createMsgErr error
var createdMsg message.Message
if ts.a.asyncJobs != nil && ts.a.sessions != nil {
    createdMsg, createMsgErr = ts.a.sessions.RecordAsyncJobAnnounced(ts.ctx, ts.a.messages, sessionID, result.ToolCallID, message.CreateMessageParams{Role: message.Tool, Parts: []message.ContentPart{toolResult}})
} else {
    createdMsg, createMsgErr = ts.a.messages.Create(ts.ctx, sessionID, message.CreateMessageParams{...}) // unchanged
}
if ts.a.asyncJobs != nil {
    if createMsgErr != nil {
        ts.a.asyncJobs.abort(sessionID, result.ToolCallID)
    } else {
        ts.a.asyncJobs.acknowledged(sessionID, result.ToolCallID)
    }
}
_ = createdMsg
return createMsgErr
```

`RecordAsyncJobAnnounced` вызывается только тогда, когда `result.ToolCallID`
действительно относится к async-задаче — сегодня `onToolResult`
обрабатывает TOOL-результаты для **любого** инструмента, не только
async-обёрнутых четырёх. `MarkAsyncJobAnnounced`'s `execrows`-возврат `0`
для tool-call, не являющегося async-задачей, — ожидаемый, дешёвый no-op
(один `UPDATE`, индексированный по PK, промах не сканирует таблицу), а не
ошибка — так что вызывать этот путь БЕЗУСЛОВНО для каждого tool-result
безопасно и не требует отдельной ветки «это async или нет» перед вызовом
(симметрично тому, как `ts.a.asyncJobs.acknowledged` уже сегодня
безусловно вызывается для каждого tool-call, `work_ledger.go:339-357`
тривиально уходит в `s == nil`/`job == nil`-ветки для неasync-задач).

**Не проверено:** точное имя поля/конструктора, которым `sessionAgent`
получает `messages`/`asyncJobs` сегодня (`agent.go`'s строительная функция
— не прочитана целиком), поэтому добавление `sessions session.Service`
туда же — по аналогии, не по прочитанному коду постройки. Риск —
низкий (симметричная добавка рядом с двумя уже существующими полями того
же класса), но реализующий должен найти реальное место и убедиться, что
координатор передаёт `c.sessions` при каждой постройке `sessionAgent`
(включая delegated-child driver'ы, `coordinator_subagents.go`'s
`buildAgent`-путь — не проверено, что там нет ВТОРОГО места постройки,
которое легко забыть).

## 2. Host identity + lease renewal; восстановление → `interrupted`

### 2.1 Host identity

Один `async_hosts` ряд на процесс, создаваемый ОДИН раз при постройке
координатора (симметрично `c.asyncJobs = newWorkLedger(...)`,
`coordinator.go:394`):

```go
// internal/session/async_job_host.go (новый файл)

type AsyncHost struct {
    ID       string
    heartbeat *time.Ticker
    stop      chan struct{}
    ...
}

// RegisterAsyncHost inserts this process's host row (heartbeat_at =
// started_at, so a sweep run immediately after registration never treats
// its own just-registered host as dead) and starts its heartbeat loop.
// label is "cli" or "web" (informational, `rush sessions jobs`, §4.4).
func (s *service) RegisterAsyncHost(ctx context.Context, label string) (*AsyncHost, error)
```

`ID` — `uuid.New().String()` (тот же генератор, что `session.Create`
уже использует, `session_lifecycle.go:16`). Ряд **никогда не удаляется**
явным кодом этой фазы — тот же выбор, что `subAgentDriverRegistry` фаза
1/2 изначально приняли для своих записей («живёт весь срок жизни
координатора») и что дизайн-документ фазы 3 переоценил и УДАЛИЛ для
драйверов (§6.2 фазы 3) — здесь удаление НЕ требуется тем же
рассуждением: `async_hosts`' строки растут на ОДНУ запись за запуск
процесса (`rush run` инстанс или веб-сервер), не за задачу и не за
сессию — порядок роста тот же, что у `sessions`/`messages` (никогда не
подчищаемых, только `purge`/`gc` по явной команде оператора). §5 добавляет
`sessions gc`-подобную ручную очистку старых host-строк как часть той же
retention-команды, не отдельный механизм.

### 2.2 Heartbeat-тик: интервал и порог смерти

Числа выбраны **не** по аналогии с `lockHeartbeatInterval`/
`lockStaleDuration` (10s/20s, `internal/session/lock.go:15-16`) — те
защищают файловый lock ОДНОЙ сессии на ОДНОМ диске, а этот heartbeat
защищает durable-факт «жив ли ЦЕЛЫЙ ПРОЦЕСС» через SQLite-запись, и
дизайн-документ явно фиксирует наблюдавшийся у оператора простой БД
**дольше 45 секунд** («Риски»). Порог смерти короче ~2×45с рисковал бы
объявить ЖИВОЙ процесс мёртвым во время именно такого простоя —
воспроизводя в точности класс ложного срабатывания (BL-4/BL-5, §0.3),
который эта фаза обязана закрыть, только на новом механизме вместо
старого.

| Константа | Значение | Обоснование |
|---|---|---|
| `asyncHostHeartbeatInterval` | 20s | Даёт ≥2 попытки обновления внутри 45-секундного окна простоя ДО того, как порог смерти вообще мог бы сработать — один пропущенный тик не приближает к порогу настолько, чтобы гонка стала вероятной. |
| `asyncJobLeaseStaleAfter` | 90s (= 4.5×`asyncHostHeartbeatInterval`) | Больше вдвое задокументированного 45-секундного простоя — переживает ОДИН полный такой простой, оставляя запас на джиттер планировщика и на то, что сам heartbeat-запрос тоже может встать в очередь SQLite позади других писателей. |

Реализация — тот же паттерн, что `timeoutService.run`
(`work_ledger_timeout.go:76-105`): один тикер на процесс (не на
задачу/хост), первая итерация до входа в цикл (чтобы host-строка
получила свежий `heartbeat_at` сразу после `RegisterAsyncHost`, а не
только через 20с):

```go
// internal/session/async_job_host.go, продолжение

func (h *AsyncHost) run(s *service) {
    t := time.NewTicker(asyncHostHeartbeatInterval)
    defer t.Stop()
    for {
        h.tick(s) // renew own heartbeat + own running jobs' leases, THEN sweep (§2.3)
        select {
        case <-h.stop:
            return
        case <-t.C:
        }
    }
}
```

`h.tick` делает ДВА независимых, не транзакционных запроса
(`TouchAsyncHostHeartbeat`, `RenewAsyncJobLeasesForHost`) — не
транзакционных **намеренно**: если процесс падает МЕЖДУ ними, хуже некуда
получается «heartbeat свежий, но чья-то задача осталась с чуть более
старым lease» — которое следующий же тик (20с спустя, если процесс
выжил) или следующий тик ЛЮБОГО другого живого процесса (§2.3 сканирует
ГЛОБАЛЬНО, не только свои задачи) закроет без вреда: `lease_expires_at`
всего на 20с старее, чем могло бы быть — далеко не 90-секундный порог.
Оборачивание в транзакцию не даёт здесь дополнительной гарантии, только
лишний `BEGIN`/`COMMIT` на каждый тик каждого процесса.

### 2.3 Sweep: `running` + lease истёк → `interrupted`, доставка ровно один раз

`h.tick` заканчивается вызовом дешёвого, идемпотентного sweep'а — **любой**
живой процесс подчищает **чужие** мёртвые хосты, не только свои
(эмерджентное свойство: если работает хоть один `rush run`/веб-сервер
где угодно на этом `RUSH_GLOBAL_DATA`, зависшие задачи мёртвого хоста
обнаружатся в течение одного 20-секундного тика ЭТОГО живого процесса;
если живых процессов вообще нет — sweep'ить некому, но и доставлять
уведомление тоже некуда, наблюдаемость всё равно восстановится при
следующем запуске любого `rush`-процесса, что и есть design doc'овское
«при старте процесса»):

```go
// internal/session/async_job_recovery.go (новый файл)

// SweepInterruptedAsyncJobs finds every 'running' async_jobs row whose
// lease has expired and reconciles it exactly once:
//   - announced == 0 (the model was never told this job started at all,
//     ASYNC-05): delete the row silently, mirroring workLedger.abort --
//     nothing was ever shown, so there is nothing to notify about.
//   - announced == 1: transition state -> 'interrupted', notice_kind ->
//     'interrupted', persist a session message for owner_session_id via
//     message.Service.CreateWithQuerier (NOT wakeSession -- see the doc
//     note below for why this sweep never starts a live turn), mark
//     delivered_at, all inside ONE transaction per row (so a crash mid-
//     sweep leaves each row EITHER fully reconciled or untouched, never
//     half-updated -- the next sweep, by this or any other process,
//     naturally retries a row whose transaction never committed, because
//     its state is still 'running' with the SAME expired lease).
//
// Why no live wake: a cold sweep (at RegisterAsyncHost time, or on this
// process's own heartbeat tick) has no reason to believe THIS process is
// about to run owner_session_id's next turn -- forcing one would mean
// picking an arbitrary SessionAgent for a session this process may never
// touch again. The persisted message is enough: ASYNC-09's own
// wake-failure marker already established the "next turn picks it up from
// history, no live wake required" pattern (coordinator_wake.go's
// persistWakeFailedMarker) for exactly the same reason -- a message
// visible in a session's history needs no separate delivery mechanism.
func SweepInterruptedAsyncJobs(ctx context.Context, s *service, messages message.Service) (reconciled int, err error)
```

Формат уведомления — новая ветка `FormatAsyncCompletion`
(`internal/agent/coordinator_background.go:34-53`), т.к. и модель, и веб
уже умеют рендерить этот формат:

```go
if completion.Interrupted {
    return fmt.Sprintf("Async job %s (%s) was interrupted: the process running it stopped responding and did not report a result. Last known output:\n\n%s",
        completion.ToolCallID, completion.ToolName, content)
}
```

**Восстановление вне `internal/agent` не может вызвать
`FormatAsyncCompletion` напрямую** (циклический импорт: `internal/session`
не может импортировать `internal/agent`, `internal/agent` уже импортирует
`internal/session`). Два варианта, выбран второй:

1. Продублировать форматирование в `internal/session` — расхождение
   формулировок гарантировано со временем (тот же класс риска, что
   `docs/mcp-invariants.md` описывает для дублирующихся законов).
2. **Выбрано.** Перенести `FormatAsyncCompletion` (и `AsyncCompletion`'s
   определение полей, которые она читает — `ToolCallID`/`ToolName`/
   `Content`/`IsError`/`TimedOut`/`TimeoutSeconds`/`Stopped`/новое
   `Interrupted`) в **`internal/message`** (пакет, который уже сидит ниже
   и `internal/agent`, и `internal/session` в графе импортов — сверено:
   `internal/session` импортирует `internal/message`, `internal/message`
   не импортирует ни `internal/session`, ни `internal/agent`). `internal/agent`
   продолжает использовать функцию под тем же именем через
   реэкспорт/алиас (`var FormatAsyncCompletion = message.FormatAsyncCompletion`
   либо простое `type AsyncCompletion = message.AsyncCompletion` — деталь
   реализации, не меняющая ни одного вызывающего снаружи пакета).

Это единственное предлагаемое перемещение кода МЕЖДУ пакетами во всей
спецификации; обосновано тем, что без него либо дублируется форматирование
(риск расхождения текста уведомлений для доставленных «вживую» и
восстановленных после интеррапта исходов), либо `internal/session`
получает зависимость на `internal/agent` (недопустимо — обратный слой).

### 2.4 Восстановление после краха ПОСРЕДИ доставки

Требование задания: «включая после краха посреди доставки». Разобрано по
шагам транзакции восстановления одной строки:

| Момент краха | Состояние строки после падения | Что делает следующий sweep |
|---|---|---|
| До `BEGIN` | `state='running'`, старый `lease_expires_at` | Находит строку снова (тот же `ListStaleAsyncJobs` запрос), начинает заново — идемпотентно |
| Внутри транзакции, до `COMMIT` (включая между `INSERT INTO messages` и `UPDATE async_jobs`) | Транзакция откатывается СУБД при следующем открытии файла (SQLite WAL/rollback journal) — строка НЕ МЕНЯЛАСЬ, `state` всё ещё `'running'` | То же — начинает заново, message-инсерт (если был) тоже откачен, значит повторный `INSERT` не дублирует сообщение |
| После `COMMIT`, до `MarkAsyncJobDelivered` (если бы это была отдельная стадия) | Не применимо — `delivered_at` пишется ВНУТРИ той же транзакции, что и `state`/message-insert (см. псевдокод ниже), не отдельным шагом | — |

Ключевое проектное решение: **`delivered_at` устанавливается в ТОЙ ЖЕ
транзакции**, что `state='interrupted'` и `INSERT INTO messages` — не
отдельным последующим вызовом (в отличие от живой доставки, где
`deliverLocked`'s in-memory переход и `wakeSession`'s persisted message —
раздельные шаги, потому что живая доставка вообще может решить НЕ будить
немедленно, §1 фазы 2). Для sweep'а разделять их не даёт выгоды (нет
понятия «отложенного пробуждения» — сообщение и есть вся доставка) и
вводит РОВНО то окно «половина сделана», которого задание просит избежать
явно. Один `*sql.Tx` на строку, ОДИН `COMMIT` — до него строка
неизменна и будет подхвачена заново, после него — полностью и до конца
реконсилирована, ровно один раз (SQL-транзакция — сама CAS; второй
`ListStaleAsyncJobs`, читающий эту же строку ДО того, как первая
транзакция закоммитилась, увидит её как `state='running'` и попробует
ту же работу параллельно — см. следующий абзац).

**Параллельный sweep из двух живых процессов одновременно.** Возможная
гонка: два процесса читают `ListStaleAsyncJobs`, оба видят одну и ту же
просроченную строку, оба открывают транзакцию. Их `UPDATE ... WHERE
state='running'` оба МОГУТ совпасть по условию, если ни один ещё не
закоммитился — но SQLite сериализует писателей (один `*sql.DB`-соединение
на файл в этом приложении, `internal/db/connect*.go` — не проверено
исчерпывающе, что оба процесса НЕ используют WAL с несколькими писателями
одновременно, но даже в WAL-режиме SQLite гарантирует один активный
писатель за раз на уровне файла) — второй `UPDATE` физически ждёт первую
транзакцию, видит `state` уже `'interrupted'` (не `'running'`), матчит
0 строк (`execrows=0`, тот же WHERE-guard, что `MarkAsyncJobAnnounced`
уже использует), не создаёт второе сообщение. Гарантия — ROW-LEVEL,
не отдельная блокировка приложения: тот же принцип, на котором уже
держится `AckRunQueueEntry`'s `WHERE ... AND leased_by = ?` (`run_queue.sql`).

## 3. Идемпотентный старт (#1038) и старт делегации

### 3.1 Durable claim в `asyncTool.Run`

Сегодня (`internal/agent/async_tool.go:77-94`): `workLedger.Start` —
единственный гейт идемпотентности, ПРОЦЕССО-ЛОКАЛЬНЫЙ. Механизм бага
#1038 (сверено чтением, не предположение): `CreateTaskSession`
(`internal/session/session_lifecycle.go:41-53`) делает простой `INSERT
INTO sessions` без `ON CONFLICT`, а `sessions.id TEXT PRIMARY KEY`
(`internal/db/migrations/20250424200609_initial.sql:5`) — повторный вызов
с тем же `toolCallID`-производным id (детерминирован:
`CreateAgentToolSessionID(messageID, toolCallID)`,
`session_lifecycle.go:100-103`, просто конкатенация — совпадает на
каждом вызове с теми же аргументами) **упадёт с UNIQUE constraint
violation**. Это воспроизводимо при ЛЮБОМ повторном вызове
`runSubAgent`'s create-ветки (`coordinator_subagents.go:76-83`) с тем же
`(messageID, toolCallID)` — в частности после падения процесса ПОСЕРЕДИ
делегации и последующего REPLAY того же вызова (через durable
`session_run_queue`, восстанавливающую прерванный ход, §0.2) в НОВОМ
процессе, чей `workLedger` пуст и не может знать, что этот вызов уже
когда-то начинался.

Решение — durable claim СНАРУЖИ `l.mu`, ДО вызова `workLedger.Start`:

```go
// internal/agent/async_tool.go, правка Run (после childSessionID/timeoutSpec,
// до текущего workLedger.Start)

inputHash := sha256Hex(call.Input)
claim, claimed, err := t.coordinator.asyncHosts.ClaimJob(ctx, sessionID, call.ID, kindFor(t.name), inputHash, childSessionID, origin == message.OriginCLI, timeoutSpec)
if err != nil {
    // Durable claim failed for a reason OTHER than conflict (DB error) --
    // fail OPEN, not closed: the in-memory Start below is still the
    // primary correctness gate for THIS process's own lifetime, and a
    // durability outage must not block ordinary operation (§1.1's
    // write-through philosophy applied to the start path too).
    slog.Warn("durable async job claim failed, proceeding without durable idempotency for this call", "session_id", sessionID, "tool_call_id", call.ID, "err", err)
    claimed = true // in-memory Start below is the only gate this call gets
}
if !claimed {
    // Cross-process replay of an ALREADY durably claimed call (#1038): a
    // prior, now-dead attempt already has a row for this key. Do not spawn
    // a second executor -- report "already started" using the DURABLE
    // row's own recorded child session, exactly like workLedger.Start's
    // existing=true branch does for a same-process retry.
    existingJob, getErr := t.coordinator.asyncHosts.GetJob(ctx, sessionID, call.ID)
    if getErr == nil && existingJob.InputHash == inputHash {
        if timeoutSpec != nil {
            // no-op: this call's own timeout spec is discarded, the durable
            // row's original deadline (if any) wins -- same rule as
            // in-memory Start's existing=true branch, which never re-arms a
            // timeout for a job it did not just create (work_ledger.go:195-200
            // only calls l.timeouts.arm on the FRESH-job path).
        }
        return t.startedResponse(call.ID, existingJob.ChildSessionID), nil
    }
    // InputHash mismatch (or the row vanished under us, e.g. it was already
    // reaped between Claim and Get -- narrow, benign): same refusal as
    // in-memory Start's own "different input" branch (work_ledger.go:179-181).
    return fantasy.NewTextErrorResponse(fmt.Sprintf("async job %s is already running with different input", call.ID)), nil
}
job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
// ... unchanged from here
```

Порядок проверок: durable claim СНАЧАЛА, `workLedger.Start` — ВТОРЫМ.
Обратный порядок (in-memory сначала) добавил бы DB-вызов на ГОРЯЧИЙ путь
повторного `existing=true` (самый частый случай — провайдер не ретраит
большинство вызовов), тогда как «durable сначала» платит DB-вызовом
**на каждый** async tool call, включая первый/единственный — это
компромисс, принятый явно: без него durable-гарантия не покрывает СЛУЧАЙ,
когда САМЫЙ ПЕРВЫЙ вызов в этом процессе — на самом деле повтор из
мёртвого процесса (пустой in-memory ledger не отличит их). Стоимость —
один индексированный `INSERT` на каждый `bash`/`run_command`/`agent`/
`agentic_fetch` вызов; для дальнейшей оценки нагрузки на SQLite нужен
замер (не проверено, не измерено этой спецификацией) — риск отмечен в
§7.

### 3.2 `abort`'а durable-версия

`workLedger.abort` (`work_ledger.go:364-378`) — если запись «started»
tool-result не удалась. Durable-версия: `DeleteUnannouncedAsyncJob`
(§1.3), вызывается из того же места, что и in-memory `abort` (симметрично
`AbortUnannounced`'s таблице переходов у фазы 1, §1.3 того документа).
Порядок не важен (оба идемпотентны и независимы), но проще сделать
durable-удаление ПЕРВЫМ (до `l.abort`), чтобы не держать `l.mu` во время
сетевого/файлового I/O SQLite — тот же принцип §1.1.

### 3.3 Идемпотентность старта делегации специфично

`CreateTaskSession`'s собственная не-идемпотентность (§3.1's диагноз)
**дополнительно** чинится напрямую, НЕЗАВИСИМО от durable claim'а выше —
причина: durable claim закрывает «не запускать исполнителя дважды», но
если РЕАЛЬНЫЙ краш произошёл МЕЖДУ `ClaimAsyncJob`'s успешным `INSERT` (с
ещё пустым `child_session_id`, т.к. дочерняя сессия создаётся уже ПОСЛЕ
клейма — временной зазор при чтении `coordinator_subagents.go:76-83`
внутри `t.run`, которое стартует уже ПОСЛЕ клейма в `asyncTool.Run`) и
собственно `CreateTaskSession`, повторный (в НОВОМ процессе) заход
увидит `claimed=false`, но `existingJob.ChildSessionID == ""` — клейм
уже есть, а дочерней сессии ещё нет. `startedResponse(call.ID, "")` в
этом случае был бы ложью (нет реального `child_session_id`, на который
можно было бы сослаться) — вместо этого нужен ТРЕТИЙ исход:

```go
if getErr == nil && existingJob.InputHash == inputHash {
    if existingJob.ChildSessionID == "" && (t.name == AgentToolName || t.name == tools.AgenticFetchToolName) {
        // Claimed but the child session row was never created (crash in the
        // narrow window between ClaimAsyncJob and CreateTaskSession). Safe
        // to retry CreateTaskSession itself here: it is now idempotent
        // (below), so a second attempt either creates the row (first time
        // it actually succeeds) or finds it already there (a DIFFERENT
        // process finished this exact window a moment earlier) -- either
        // way this call proceeds as a FRESH start from this point on, using
        // the ALREADY-CLAIMED row instead of a new one.
        goto proceedAsFreshStartWithExistingClaim // pseudocode marker, see prose below
    }
    return t.startedResponse(call.ID, existingJob.ChildSessionID), nil
}
```

(Псевдокод; `goto` — не буквальное предложение, а обозначение того, что
реализация должна структурировать `Run` так, чтобы этот путь мог дойти до
`t.run`'s делегационной ветки, используя УЖЕ существующий durable-клейм
вместо повторного `ClaimAsyncJob`.)

Сама `CreateTaskSession` получает собственную идемпотентность —
**независимая, более узкая правка**, полезная сама по себе (закрывает
буквальную формулировку #1038 «`CreateTaskSession` не идемпотентен» даже
для гипотетического вызывающего вне async-обёртки):

```sql
-- internal/db/sql/sessions.sql, новый запрос
-- name: CreateSessionIfNotExists :one
INSERT INTO sessions (id, parent_session_id, title, ...)
VALUES (?, ?, ?, ...)
ON CONFLICT (id) DO NOTHING
RETURNING *;
```

```go
// internal/session/session_lifecycle.go, правка CreateTaskSession
func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
    dbSession, err := s.q.CreateSessionIfNotExists(ctx, db.CreateSessionIfNotExistsParams{...})
    if errors.Is(err, sql.ErrNoRows) {
        // Conflict: a row for this id already exists -- return IT, not an
        // error, so a replayed call is idempotent regardless of whether the
        // caller is the new durable-claim path above or any future caller.
        existing, getErr := s.q.GetSessionByID(ctx, toolCallID)
        if getErr != nil {
            return Session{}, fmt.Errorf("create task session: row exists but could not be read back: %w", getErr)
        }
        return s.fromDBItem(existing), nil
    }
    if err != nil {
        return Session{}, err
    }
    session := s.fromDBItem(dbSession)
    s.Publish(pubsub.CreatedEvent, session)
    return session, nil
}
```

Публикация `pubsub.CreatedEvent` **пропускается** на пути «уже
существует» — сессия не создана ЭТИМ вызовом, повторная публикация
`Created` для уже существующей (возможно, уже отображённой в UI) сессии
была бы ложным сигналом. Не проверено, подписан ли кто-то на `Created`
таким образом, что пропуск публикации при `ErrNoRows`-ветке меняет
видимое поведение UI при НОРМАЛЬНОМ (не-idempотентном) первом создании —
первый вызов проходит по `err == nil`-ветке и публикует как раньше, так
что различие видно только на повторе, который до этой фазы всегда был
ошибкой (никогда не публиковал `Created` дважды за неимением второго
успешного вызова вообще) — поведенчески строго более мягкое изменение,
не регрессия.

## 4. Кросс-процессные читатели: замена `descendant_liveness.go`

### 4.1 Удаляется целиком

| Файл/функция | Заменяется на |
|---|---|
| `internal/session/descendant_liveness.go` (`LiveDescendants`, `LiveDescendant`, `SubSessionLister`, `maxDescendantWalkDepth`) | `internal/session/async_job_reader.go`'s `LiveJobs` (ниже) — тот же BFS-по-`parent_session_id` каркас (глубина 16, cycle-guard, `walkIncomplete`), но узел проверяется запросом к `async_jobs`, не `InspectSessionLock` |
| `internal/session/descendant_liveness_test.go` | `async_job_reader_test.go` — сценарии переносятся (grandchild holds live lock → grandchild holds a running job), только фикстура меняется с lock-файлов на durable-строки |
| `internal/cmd/sessions_why.go:207` (`session.LiveDescendants(ctx, a.Sessions, dataDir, sessionID)`) и всё чтение `liveDescendants`/`walkIncomplete`/`descendantCaveat` до конца функции | `session.LiveJobs(ctx, a.Sessions, sessionID)` — сигнатура теряет `dataDir` (durable-чтение не завязано на путь к lock-файлам вообще) |
| `internal/cmd/sessions_list.go:103` (`markDelegatingLiveDescendants`) и сама функция (`:368-430`, не проверено точное число строк тела — заголовок сверен) | Новая `markDelegatingLiveJobs`, тот же сигнал (`live, _ := session.LiveJobs(...)`), без `dataDir` |
| `internal/server/handlers_sessions.go:250-267` (`annotateLiveDescendantWork`) | Тело меняется на вызов `session.LiveJobs`, JSON-поля `HasLiveDescendantWork`/`LiveDescendantIDs` **не переименовываются** (веб/TS-совместимость, `web/src/types.ts:52,54` их уже ожидает) |
| `docs/plans/2026-09-25-session-fixes-review-backlog.md`'s BL-2026-09-25-4/-5 | Помечаются закрытыми (см. §8) — сам файл backlog НЕ редактируется этой фазой (исторический протокол), закрытие фиксируется только в `docs/async-invariants.md` |

**НЕ удаляется:** `internal/session/lock*.go` целиком (файловый lock+
heartbeat — другой механизм, §0.2); `coordinator.ParkedSubAgentParents`/
`ParkedSubAgentWorkReporter` (`internal/agent/coordinator_work_scope.go:80-108`)
— внутрипроцессный сигнал остаётся, `sessions list`'s
`markParkedDelegationSessions` (`sessions_list.go:94-96`) продолжает его
читать НЕЗАВИСИМО от durable-слоя (быстрый in-process путь для процесса,
который САМ держит координатора — durable-чтение обязательно только
когда работа принадлежит ДРУГОМУ процессу).

### 4.2 `internal/session/async_job_reader.go` (новый файл)

```go
// LiveJob names one session (at any depth below root) that durably owns a
// running async job, or is itself running one -- the durable-state
// replacement for LiveDescendant.
type LiveJob struct {
    SessionID  string
    Depth      int    // 0 = root itself owns a running job; 1 = direct child; ...
    ToolCallID string
    Kind       string // 'command'|'agent'|'fetch'
    HostID     string
    StartedAt  time.Time
}

// LiveJobs returns every session at or below rootSessionID (root itself
// included, unlike LiveDescendants which only walked BELOW it -- see the
// note on depth 0 above) that durably owns a 'running' async_jobs row,
// walking parent_session_id the same bounded, cycle-guarded way
// LiveDescendants did (maxDescendantWalkDepth = 16).
//
// A running row backed by a DEAD host (lease expired, not yet swept) is
// NOT reported as live: the query already filters lease_expires_at, so a
// caller sees the SAME "interrupted" verdict the next sweep would produce,
// without waiting for that sweep to actually run first (read-time
// reconciliation, cheaper than forcing an out-of-band sweep on every
// status query).
func LiveJobs(ctx context.Context, lister SubSessionLister, store AsyncJobReader, rootSessionID string) (live []LiveJob, walkIncomplete bool)
```

Ключевое отличие от `LiveDescendants`, требующее аккуратности: **root
сам** теперь тоже проверяется (глубина 0) — потому что durable-строка не
привязана к «эта сессия ещё держит СВОЙ lock», а напрямую говорит «эта
сессия владеет незавершённой задачей» — `sessions why <root>`'s
собственная секция (`explainSessionStatus`'s `!hasLock` ветка,
`sessions_why.go:222-250`) уже вычисляет `liveDescendants` ДО switch'а и
использует `len(liveDescendants) > 0` как признак «не done»; при переходе
на `LiveJobs` эта проверка автоматически покрывает и «у самого root есть
незавершённая, но ещё не объявленная world-visible задача» — случай,
которого `LiveDescendants` в принципе не мог видеть (root'а lock уже нет,
а работа, тем не менее, идёт — ИМЕННО производственный баг, который
`descendant_liveness.go`'s файловый комментарий (`:9-17`) описывает как
уже однажды наблюдавшийся для ДЕТЕЙ; при durable-чтении та же логика
естественно распространяется и на сам корень без отдельного кода).

`store AsyncJobReader` — узкий интерфейс (`ListRunningAsyncJobsForOwners`
сверху, батч по всем узлам одного уровня BFS за один SQL-вызов, а не по
одному запросу на узел — эта фаза УЛУЧШАЕТ на BL-2026-09-25-5's находку,
не просто переносит её: сегодняшний `InspectSessionLock` — один вызов на
узел; `ListRunningAsyncJobsForOwners(ctx, []string{n1,n2,...})` — один
`SELECT ... WHERE owner_session_id IN (...)` на ВЕСЬ уровень обхода).

### 4.3 `sessions_why.go`/`sessions_list.go`: точечные правки

Обе функции сохраняют СВОЮ структуру verdict-переключателя (
`explainSessionStatus`'s `switch` на `statFailed`/`!hasLock`/`hasLock &&
pidAlive`/`default`, `sessions_why.go:218-...` — не редактируется по
существу), меняется только источник `liveDescendants`→`liveJobs` и текст
описания (`describeLiveDescendants` → новая `describeLiveJobs`,
называющая `ToolCallID`/`Kind` вместо lock-эвристики: «session X is
running a live command/agent job (started Ns ago)» вместо «lock is held,
PID N alive»). `walkIncomplete`'s семантика не меняется (частичный обход
из-за ошибки `ListSubSessions` — тот же случай, что раньше).

### 4.4 CLI-поверхность для человека: `rush sessions jobs`, `rush jobs kill`

Предложение (пункт задания «propose CLI surface»), НЕ решённое здесь
окончательно — операторское решение (§9, открытый вопрос 1), но с
конкретной формой для утверждения:

- **`rush sessions jobs [<session-id>]`** — список durable-строк
  (`ListAsyncJobsForOwner`, либо `ListRunningAsyncJobsForOwners` по
  умолчанию + `--all` для терминальных в пределах retention-окна),
  колонки: `TOOL_CALL_ID KIND STATE HOST STARTED AGE RESULT`. Без
  аргумента — по всем сессиям (аналог `sessions list`, не `sessions
  why`). Файл `internal/cmd/sessions_jobs.go`, тот же `cobra.Command`-
  каркас, что `sessions_list.go`.
- **`rush jobs kill <owner-session-id> <tool-call-id>`** — человеческий
  эквивалент `job_kill`-инструмента: для строки с `state='running'` и
  живым host'ом, если это `kind='command'`, шлёт то же, что модель шлёт
  через `job_kill` (нужен способ достучаться до ЖИВОГО host'а — если
  этот CLI-инстанс не тот же процесс, прямой вызов невозможен: команда
  честно отвечает «job is owned by a different live process (host <id>,
  pid <pid> on this machine); ask that process's session to stop it, or
  wait for its lease to expire» вместо попытки IPC, которого сегодня нет
  ни у одного `sessions`-подкоманды (`sessions kill`, для сравнения, уже
  умеет `taskkill /F /T` по PID — не проверено, стоит ли `jobs kill`
  повторять этот трюк для async-задач; вероятно, да, но это отдельная,
  не тривиальная работа с PID-деревом конкретного background-shell'а, а
  не с PID всего `rush run` — оставлено как реализационная деталь,
  не решаемая этой спецификацией).
- Для `kind IN ('agent','fetch')` — `jobs kill` отказывается («use
  `rush sessions kill <child-session-id>` instead» — уже существующая
  команда, не дублируется).

Названо в §9 как открытый вопрос: заводить ли `jobs kill`'s
процесс-к-процессу доставку сейчас (не тривиально, требует какого-то
IPC/сигнала — вне готовых примитивов этого кодового дерева) или оставить
только НАБЛЮДЕНИЕ (`sessions jobs`) в этой фазе и отложить `kill` до
плана пробуждений (`stop_agent`/`inject_agent`, #1024), который уже
проектирует аналогичную доставку для АГЕНТА, а не оператора.

### 4.5 Снимок живой работы для #1043/#1058 (форвард-совместимость)

Не реализуется этой фазой (параллельная работа, §0.2), но API спроектирован
так, чтобы не потребовать пересмотра:

```go
// internal/session — уже достаточно для будущего потребителя
func LiveJobs(ctx, lister, store, rootSessionID) ([]LiveJob, bool)
func (s *service) ListAsyncJobsForOwner(ctx, sessionID) ([]AsyncJobRow, error)
```

Оба читают ТОЛЬКО БД — работают из ЛЮБОГО процесса, включая тот, что
держит in-memory `workLedger` для этой самой сессии (durable write-through,
§1.1, гарантирует, что живой процесс видит СВОИ же задачи в БД с
задержкой не больше одной DB-транзакции). Надзору (#1043, «сводка что
запущено, сколько идёт, последняя строка вывода») нужен ТОЛЬКО
`ListAsyncJobsForOwner` плюс `result_summary`/`updated_at`, уже
присутствующие в схеме — отдельного API писать не придётся. Веб-панели
(#1058, «живые команды/агенты по сессии») — тот же `LiveJobs`, что и
`sessions why` использует, уже кросс-процессный по конструкции (веб-сервер
и `rush run` — разные процессы, ровно случай, для которого эта фаза и
существует).

## 5. Retention терминальных строк, безопасность миграции, откат

### 5.1 Retention

`PurgeTerminalAsyncJobsOlderThan` (§1.3) — не автоматический фоновый
таймер (design doc не просит демона; фоновый cron уже отдельно запрещён
CLAUDE.md, «Do NOT enable any cron on the fork»). Вызывается:

- Вручную: `rush sessions gc` **расширяется** (не новая команда) —
  добавляется четвёртый пункт к уже существующему списку
  (`sessions_gc.go:16-19`, «Sessions older than…», «ID prefix ping-…»,
  «Child sessions whose parent no longer exists»): «Terminal async job
  rows older than `--jobs-older-than` (default 7 days, matching sessions
  gc's own 7-day default for consistency, not a separately-justified
  number)».
- Автоматически, дёшево, на каждом host heartbeat-тике (§2.2) — НЕ
  вызывает `PurgeTerminalAsyncJobsOlderThan` с 7-дневным окном на каждом
  20-секундном тике (излишняя нагрузка на индекс без выгоды); вместо
  этого heartbeat-тик делает **только** sweep (§2.3), retention остаётся
  ручной/`gc`-only. Явное разделение: sweep — корректность (наблюдаемость
  «не зависло молча»), retention — гигиена (не дать таблице расти
  бесконечно) — разные бюджеты, разные вызывающие.

### 5.2 Безопасность миграции для существующих БД

- `CREATE TABLE IF NOT EXISTS` — идемпотентна на повторный прогон миграций
  (тот же стиль, что `20260809000001_add_session_run_queue.sql`).
- `async_jobs.owner_session_id REFERENCES sessions(id) ON DELETE CASCADE` —
  удаление сессии (`session.Delete`, `session_lifecycle.go:69-98`, уже
  транзакционно удаляет messages/files/session) автоматически подчищает
  её async-строки без отдельной правки `Delete` — тот же паттерн, что
  `session_run_queue.session_id REFERENCES sessions (id) ON DELETE
  CASCADE` уже использует (`20260809000001…sql`).
- **Не проверено**: включён ли `PRAGMA foreign_keys = ON` в это
  соединение (без него `ON DELETE CASCADE` — не более чем документация,
  СУБД не применяет её) — при реализации сверить `internal/db/connect*.go`;
  если выключен, `session.Delete`'s транзакция должна явно добавить
  `DELETE FROM async_jobs WHERE owner_session_id = ?` тем же образом, что
  уже делает для `messages`/`files`.
- Пустая база (свежая установка) — таблицы создаются пустыми, `Start`
  сразу начинает работать в durable-режиме с первого вызова, никакой
  отдельной инициализации не требуется.
- Существующая база БЕЗ этой миграции, у которой уже накопились
  сессии/сообщения — миграция не читает и не мигрирует НИКАКИЕ старые
  данные (async-задачи прошлых запусков были чисто in-memory и
  физически не существуют нигде, кроме, возможно, текстовых уведомлений
  уже в `messages` — те не трогаются, не переинтерпретируются
  задним числом).

### 5.3 Откат (`+goose Down`)

Приведён в §1.2. Откат ПОСЛЕ того, как процессы уже писали в
`async_jobs`/`async_hosts`, теряет ВСЕ durable-записи (обычный DROP TABLE,
без резервного копирования) — приемлемо, поскольку (а) это чисто
наблюдательная/восстановительная durability-надстройка, не источник
истины для активно исполняющейся работы (§1.1: in-memory ledger её не
использует для собственных решений в рамках своего процесса), (б) откат
миграции — операция, которую этот кодекс уже не предполагает выполнять
на боевой БД без отдельного плана (`goose down` не имеет здесь
прецедента использования в проде — не проверено, но ни один существующий
`+goose Down` блок в `internal/db/migrations/` не содержит логики
сохранения данных, все — простые `DROP`).

**Откат КОДА (не БД)** — более реалистичный сценарий (регресс фичи,
таблицы уже созданы, но код фазы 4 отключается): `workLedger`'s durable
поле (§6, `l.durable AsyncJobDurableStore`) — `nil`-safe на каждом вызове
(симметрично `l.timeouts`/`l.driverFor`'s уже принятому паттерну,
`work_ledger.go`/`work_ledger_timeout.go`). Установка `l.durable = nil`
(флаг конфигурации, `--experimental-async-durability=false` либо просто
не вызывать `newAsyncJobDurableStore` при постройке координатора)
возвращает систему К ПОВЕДЕНИЮ ФАЗ 1–3 БЕЗ отката миграции — таблицы
остаются в схеме (пустые или устаревшие), просто не читаются/не
пишутся, что не мешает откату конкретно этой части при сохранении
остальной БД рабочей.

## 6. Пошаговый план реализации

Каждый шаг — рабочая сборка; тесты перечислены кумулятивно.

**Шаг 0 — миграция + sqlc, без подключения.** `internal/db/migrations/
20260929000001_add_async_job_ledger.sql`, `internal/db/sql/async_jobs.sql`,
сгенерированный `internal/db/async_jobs.sql.go`, плюс
`internal/db/sql/sessions.sql`'s `CreateSessionIfNotExists`. Тесты:
`internal/db/migrate_test.go`-подобный прогон миграции вверх/вниз на
пустой БД (не проверено точное имя существующего теста миграций — при
реализации найти и расширить, не дублировать инфраструктуру); никакой
Go-код фаз 1–3 не тронут, сборка зелёная тривиально.

**Шаг 1 — host identity + heartbeat, без подключения.**
`internal/session/async_job_host.go`. Тесты: новый
`async_job_host_test.go` — регистрация хоста, heartbeat тикает, лизы
продлеваются для тестовых строк, оба Tick-теста используют `TestTick`-
подобный сид (аналог `RunQueuePumpConfig.TestTick`, тот же приём —
переопределяемый интервал для детерминизма вместо реального 20с sleep).
Не подключено ни к одному вызывающему — существующий набор
`internal/agent`/`internal/session` зелён без изменений.

**Шаг 2 — durable claim + ack-gate транзакция, без подключения.**
`internal/session/async_job_store.go` (`ClaimJob`, `GetJob`,
`DeleteUnannouncedAsyncJob`-обёртка), `internal/session/async_job_ack.go`
(`RecordAsyncJobAnnounced`), `internal/message/message.go`'s
`CreateWithQuerier` (аддитивная правка, `Create` делегирует ей — тест
`TestMessageService_CreateDelegatesToCreateWithQuerier` подтверждает
байт-в-байт то же поведение). Тесты: `async_job_store_test.go` (ON
CONFLICT DO NOTHING возвращает существующую строку, не ошибку),
`async_job_ack_test.go` (транзакционность — форс-падение ПОСЛЕ
message-insert, ДО async_jobs-update, через тестовый `db.Querier`-мок,
показывает откат ОБОИХ, не только одного). Существующий стек всё ещё
использует старый двухшаговый путь `onToolResult` — не переключено.

**Шаг 3 — recovery sweep, без подключения.**
`internal/session/async_job_recovery.go`. Тесты:
`TestSweepInterruptedAsyncJobs_AnnouncedJobBecomesInterrupted`,
`TestSweepInterruptedAsyncJobs_UnannouncedJobIsDeletedSilently`,
`TestSweepInterruptedAsyncJobs_ConcurrentSweepsDeliverOnce` (два
одновременных вызова над одной просроченной строкой — ровно одно
сообщение, см. §2.4). Не вызывается ни из одного heartbeat-тика ещё.

**Шаг 4 — переключение (главный рискованный шаг, отдельный коммит).**
Одновременно:
- `internal/agent/async_tool.go`: durable claim в `Run` (§3.1, §3.3).
- `internal/agent/agent_turn_stream.go`: `onToolResult` зовёт
  `RecordAsyncJobAnnounced` (§1.4); `sessionAgent` получает поле
  `sessions session.Service`, заполняемое координатором.
- `internal/agent/coordinator.go:394-397`: `c.asyncHosts, _ =
  c.sessions.RegisterAsyncHost(ctx, hostLabel)` рядом с существующей
  постройкой `c.asyncJobs`; `c.asyncJobs.durable = adapterOver(c.asyncHosts)`.
- Новый `internal/agent/work_ledger_durable.go`: интерфейс
  `asyncJobDurableStore` (нужные `workLedger` методы — `RecordTerminal`,
  ничего больше синхронно на горячем пути), вызовы из `finish`
  (`work_ledger.go:562-599`), `cancelSession`
  (`work_ledger_delegation.go:198-233` — ОБЕ ветки, включая ПЛОСКУЮ
  задачу, которая сегодня не проходит через `deliverLocked` вообще, см.
  §3.5 находку ниже), `handleTimeout`
  (`work_ledger_timeout.go:135-191`, обе ветки).
- `internal/agent/coordinator_background.go`/`internal/message`:
  `AsyncCompletion`/`FormatAsyncCompletion` переезжают в
  `internal/message` (§2.3), `internal/agent` реэкспортирует.
- `internal/session/session_lifecycle.go`: `CreateTaskSession`
  идемпотентна (§3.3).
- `internal/agent/work_ledger.go`/`internal/agent/tools/job_kill.go`:
  #1063 CAS-фикс (§3.4/§3.5 находка ниже — детали в отдельном подразделе,
  т.к. это правка, не покрытая предыдущими пунктами).

  Тесты, обязанные быть зелёными СРАЗУ после этого шага:
  - весь существующий набор `internal/agent`/`internal/app`, включая все
    восемь чёрных тестов, которые фазы 1–3 уже защищали (список — фаза 1
    §3, шаг 3; не редактируются);
  - новые (§7 ниже, тест-план).

**Шаг 5 — CLI/веб читатели.** `internal/session/async_job_reader.go`,
удаление `descendant_liveness.go`+тестов, правки `sessions_why.go`/
`sessions_list.go`/`handlers_sessions.go` (§4). Новый
`internal/cmd/sessions_jobs.go` (если оператор утверждает §4.4 — иначе
шаг пропускается, помечается открытым вопросом). Тесты: перенесённые
сценарии `descendant_liveness_test.go` (grandchild running job → live),
`sessions_why_descendant_test.go`/`sessions_list_parked_delegation_test.go`
(имена файлов не меняются, сценарии — да, данные — durable вместо
lock-файлов).

**Шаг 6 — retention.** `sessions gc`'s расширение (§5.1),
`PurgeTerminalAsyncJobsOlderThan`'s вызывающий код. Тест:
`TestSessionsGc_PurgesTerminalAsyncJobsOlderThanCutoff`.

**Шаг 7 — реанкеровка `docs/async-invariants.md`.** Тем же коммитом, что
шаг 4 (см. §8).

## 7. Тесты

### 7.1 Гонка `job_kill` против естественного завершения (#1063) — находка и фикс

**Диагноз (сверено чтением, воспроизводимый механизм, не гипотеза).**
`StopRunCommandJob` (`work_ledger.go:521-548`) и `MarkJobStopped`
(`:461-474`) устанавливают `job.stopRequested = true` под `l.mu`, **не
проверяя `job.state`** — только присутствие записи в карте (которая
остаётся истинной и для терминальной-но-необъявленной задачи, и, что
важнее здесь, для задачи, чей executor УЖЕ вычислил РЕАЛЬНЫЙ результат
(`t.inner.Run` вернулся) но ЕЩЁ НЕ ВЫЗВАЛ `finish`. `finish`
(`:562-599`) читает `job.stopRequested` и, если `true`, **безусловно**
заменяет РЕАЛЬНЫЙ переданный `result` на «stopped (job_kill)» текст и
`state = phaseCancelled` — даже если исполнитель только что успешно
завершился (`result.isError == false`, настоящий вывод в `result.content`)
и `job_kill` физически опоздал: процесс уже закончился, а
`stopRequested` — единственное, что несёт эту информацию `finish`, без
метки времени и без знания, ЧТО было раньше — реальное завершение или
запрос остановки.

Точная гонка: `t.run` (`async_tool.go:166-225`) вызывает
`response, err := t.inner.Run(ctx, call)` (для `bash`/`run_command`
блокируется до реального завершения процесса), затем — до вызова
`t.finalize`→`finish` — есть окно (несколько инструкций планировщика
Go, JSON-операции, `strings.TrimSpace`/`TruncateOutput`), в течение
которого `job_kill`, обратившись именно к ЭТОЙ, всё ещё присутствующей в
карте, записи, устанавливает `stopRequested=true` И **возвращает модели
успех** («kill requested», `job_kill.go:105-109`) — хотя момент, когда
это тело `job_kill`-инструмента выполнилось, находится СТРОГО ПОСЛЕ
реального (успешного или неуспешного) завершения задачи. `finish`,
вызванный чуть позже, лжёт: реальный успешный результат подменяется
«stopped»-текстом, и `AsyncCompletion.Stopped=true`/`notice_kind=
'job_stopped'` доставляются владельцу вместо настоящего исхода. Это
прямое нарушение «job_kill, racing with natural completion, must keep the
real outcome» — единственный пункт задания с реальным найденным дефектом
в уже смёрженном коде (wakes stage 2, коммит `593bcf2d`), не только с
отсутствующей durability.

**Фикс — прямой CAS вместо флага, консультируемого позже.** `finish`
перестаёт БЫТЬ ЕДИНСТВЕННЫМ писателем цепочки `stopRequested→cancelled`;
вместо этого `StopRunCommandJob`/`MarkJobStopped` **сами** пытаются
`transitionToTerminal(phaseCancelled, …)` В МОМЕНТ запроса остановки —
тот же принцип, которым `cancelSession` уже разрешает свою собственную
гонку с `finish` (ASYNC-03, «чей бы вызов ни захватил мьютекс первым, тот
и выигрывает»):

```go
// work_ledger.go, правка StopRunCommandJob (аналогично MarkJobStopped)
func (l *workLedger) StopRunCommandJob(owner, jobID string) error {
    l.mu.Lock()
    var job *asyncJob
    if s := l.bySession[owner]; s != nil {
        job = s.jobs[jobID]
    }
    if job == nil || job.toolName != tools.RunCommandToolName {
        l.mu.Unlock()
        return fmt.Errorf(...)
    }
    if !job.transitionToTerminal(phaseCancelled, jobResult{content: partialFor(job)}) {
        // Lost the race: finish() already committed the REAL outcome for
        // this job (natural completion beat the stop request). Report that
        // honestly instead of a fabricated "stop succeeded" -- this is the
        // #1063 fix: the real outcome is preserved because finish() already
        // WON the CAS by the time this call arrives, and this call becomes
        // a documented no-op rather than a silent overwrite.
        l.mu.Unlock()
        return fmt.Errorf("job %s already finished before the stop request reached it; its real result was delivered instead", jobID)
    }
    cancel := job.cancel
    completion, callback := l.deliverLocked(owner, job)
    l.mu.Unlock()
    if cancel != nil {
        cancel()
    }
    if callback {
        l.onWebDone(completion)
    }
    return nil
}
```

`finish` теряет свою `stopRequested`-ветку целиком (`:578-592`) —
больше не читает и не устанавливает это поле; `stopRequested`-поле
(`work_job.go:123-129`) удаляется. Симметрично для `MarkJobStopped`
(bash-путь): она НЕ отменяет контекст сама (это делает
`bgManager.KillOwned` отдельно в `job_kill.go`) — она должна попытаться
ту же CAS-транзакцию, но БЕЗ вызова `job.cancel()` (bash's kill идёт
через `BackgroundShellManager`, не через `job.cancel`), и её "стоп"-текст
— `capturePartial`-подобный снимок вывода shell'а (тот же путь, что
таймаут уже использует, `work_ledger_timeout.go:216-234`), а не
что-то, что `finish` больше не построит.

Прямое следствие для `job_kill.go` (`:88-94`): вызывающий должен
проверить возврат `MarkJobStopped`/`StopRunCommandJob` и, при ошибке
«already finished», **не** говорить модели «kill requested» — вернуть
её настоящий текст («job already finished; see its own result message»),
а НЕ звать `bgManager.KillOwned` вообще (для bash — раз job уже
терминален, kill'ить уже нечего; текущий код звонит `KillOwned`
безусловно ПОСЛЕ `MarkJobStopped`, независимо от её исхода — эта
безусловность тоже устраняется).

### 7.2 Новые тесты (сводно)

| Тест | Сценарий | Revert-check |
|---|---|---|
| `TestAsyncJobStore_ClaimIsIdempotentAcrossProcesses` | Два `ClaimJob` с тем же `(owner, toolCallID, inputHash)` из двух отдельных `*workLedger`/DB-хендлов (симулирует два процесса) — второй видит `claimed=false`, читает ПЕРВЫЙ клейм | Убрать `ON CONFLICT DO NOTHING` — тест ловит ошибку вместо мирного возврата существующей строки |
| `TestAsyncJobStore_ClaimRefusesDifferentInputSameKey` | Тот же ключ, другой `inputHash` — отказ, не тихий успех | Убрать `input_hash`-сравнение — тест ловит ложный `claimed=true` |
| `TestRecordAsyncJobAnnounced_MessageAndFlagCommitTogether` | Форс-ошибка на втором `UPDATE` — проверить, что message-insert ТОЖЕ откатился (запрос той же сессии после — ноль строк) | Разбить на два отдельных, не-транзакционных вызова — тест ловит осиротевшее сообщение без `announced=1` |
| `TestSweepInterruptedAsyncJobs_DeliversExactlyOnceUnderConcurrentSweep` | §2.4 — две горутины, один `ListStaleAsyncJobs`-результат, обе пытаются реконсилировать одну строку | Убрать `WHERE state='running'` guard из `UPDATE` — тест ловит два сообщения |
| `TestAsyncTool_ReplayedCallAfterCrashDoesNotRerunExecutor` | Симулировать «новый процесс» (свежий `*workLedger`, старая DB-строка) — `t.run`'s исполнитель НЕ вызывается второй раз (шпион-счётчик) | Убрать durable claim из `Run` — тест ловит второй вызов исполнителя (двойной побочный эффект) |
| `TestCreateTaskSession_RetryReturnsExistingRow` | Два вызова с тем же `toolCallID` — второй возвращает ТУ ЖЕ строку, не ошибку | Вернуть `CreateSession` без `ON CONFLICT` — тест ловит ошибку на втором вызове |
| `TestStopRunCommandJob_LosingRaceAgainstFinishKeepsRealOutcome` (#1063) | `finish` выигрывает CAS ПЕРВЫМ (реальный успех) — последующий `StopRunCommandJob` получает ошибку «already finished», доставленный владельцу текст — РЕАЛЬНЫЙ, не «stopped» | Вернуть `stopRequested`-флаг вместо прямой CAS — тест ловит подменённый на «stopped» результат |
| `TestStopRunCommandJob_WinningRaceProducesStoppedOutcome` | `StopRunCommandJob` выигрывает ПЕРВЫМ — `finish`, вызванный позже executor'ом, — no-op (проигранный CAS), доставленный текст — «stopped (job_kill)» | Убрать CAS-проверку у `StopRunCommandJob` (безусловный `transitionToTerminal`) — тест ловит двойную доставку |
| `TestLiveJobs_RootItselfWithRunningJobIsLive` | Депth-0 случай (§4.2) — root без lock-файла (эвристика больше не участвует), но с running-строкой — `LiveJobs` находит его на глубине 0 | Начать обход с глубины 1 (буквальный перенос `LiveDescendants`) — тест ловит пропуск |
| `TestLiveJobs_DeadHostRunningRowNotReportedLive` | Строка `state='running'`, `lease_expires_at` в прошлом, sweep ещё не прошёл — `LiveJobs` не считает её живой (read-time reconciliation, §4.2) | Убрать `lease_expires_at`-фильтр из `ListRunningAsyncJobsForOwners` — тест ловит ложно-живую строку |

Существующие тесты, обязанные остаться зелёными без правки сценария
(доказательство «поведение не изменилось» для всего, что НЕ входит в
явный список выше): весь `work_ledger_test.go`/
`work_ledger_delegation_test.go`/`work_ledger_timeout_test.go`'s набор
из фаз 1–3, восемь чёрных тестов `internal/app` (фаза 1 §3, шаг 3).

## 8. Реанкеровка `docs/async-invariants.md`

Обязательна тем же коммитом, что шаг 4 (§6):

| ID | Что меняется |
|---|---|
| ASYNC-01 | «Не больше одной активной делегации на дочернюю сессию» — статус остаётся «не проверено async-специфично» (не входит в объём этой фазы), но добавляется примечание: durable `ClaimAsyncJob`'s PK-конфликт даёт ВТОРУЮ, кросс-процессную линию защиты от двойного клейма того же `(owner, toolCallID)`, независимую от mailbox-эксклюзивности. |
| ASYNC-03 | Добавляется примечание: `StopRunCommandJob`/`MarkJobStopped` (§7.1) теперь САМИ участвуют в CAS через `transitionToTerminal`, а не консультируют флаг ПОСЛЕ факта — закрывает #1063, находку ЭТОЙ фазы, не спецификации design doc'а буквально. |
| ASYNC-04 | «Доставка на 1 уровень… смерть хоста — фаза 4» → «смерть хоста: реализовано (§2), доставляется через персистентное сообщение при следующем ресюме владельца, не живым пробуждением (см. §2.3's обоснование отличия от `wakeSession`)». Статус: «частично» → «частично, сужено»: глубина >1 всё ещё не проверена (то же ограничение, что фазы 1–3). |
| ASYNC-09 | `refreshSubAgentCompletion`'s Debug (не тронут этой фазой тоже) остаётся нарушением; ДОБАВЛЯЕТСЯ: строка write-through DB-сбоя (§1.1) — `slog.Warn`, не Debug — новая, но однотипная, видимая деградация. |
| ASYNC-10 | «Кросс-процессный путь» переписывается целиком: `descendant_liveness.go`'s PID/mtime эвристика **удалена**; `session.LiveJobs` (§4.2) называет КОНКРЕТНУЮ задачу (`tool_call_id`/`kind`), не только «сессия жива» — закрывает ключевую часть формулировки закона («называет конкретную задачу»), которая до этой фазы не выполнялась НИКЕМ. Статус: «частично» → «выполняется» (первое полное закрытие этого закона за все 4 фазы). |

## 9. Открытые вопросы оператору

Только продуктовые/архитектурные решения — технические уже приняты и
обоснованы выше.

1. **`rush jobs kill`'s кросс-процессная доставка (§4.4).** Заводить ли
   в ЭТОЙ фазе попытку IPC/сигнала к чужому живому host'у, или оставить
   только наблюдение (`sessions jobs`) и явный отказ для `kill` на чужом
   host'е, отложив реальную остановку до плана пробуждений (#1024)? Эта
   спецификация предполагает второе (дешевле, меньше нового
   инфраструктурного риска), но это продуктовое решение — сколько
   ценности несёт человеческий `kill` без него против стоимости первого
   IPC-механизма в кодовой базе.
2. **BL-4/BL-5's привязка (§0.3).** Принять `BL-2026-09-25-4/-5` как
   верную привязку без доступа к другому источнику этих коротких ID —
   или указать оператору реальный источник, если он существует вне
   прочитанных документов.
3. **Стоимость durable claim на КАЖДЫЙ async-вызов (§3.1).** Один
   дополнительный `INSERT`-round-trip на каждый `bash`/`run_command`/
   `agent`/`agentic_fetch` вызов, включая самый частый случай (НЕ повтор)
   — принять эту стоимость как плату за кросс-процессную идемпотентность,
   или сузить durable claim ТОЛЬКО до `kind IN ('agent','fetch')`
   (делегации — единственная часть #1038's буквальной формулировки,
   `CreateTaskSession`), оставив `bash`/`run_command`'s идемпотентность
   исключительно in-memory (как сегодня), раз design doc называет
   именно повторный запуск СТОРОННЕГО ПРОЦЕССА (`rm -rf`) риском, а не
   абстрактным принципом — компромисс между полнотой (#1038's буквальный
   заголовок — только про `CreateTaskSession`, т.е. только делегации) и
   единообразием (одна durable-гарантия на все четыре инструмента, не
   две разные модели идемпотентности рядом).
4. **`sessions gc`'s расширение вместо новой команды (§5.1).** Принять
   переиспользование `sessions gc --jobs-older-than` — или завести
   отдельную `sessions jobs gc`/`rush jobs gc`, если ретеншн терминальных
   job-строк концептуально должен управляться отдельно от ретеншна
   сессий (разные типичные объёмы: строк на порядки больше, чем сессий,
   при активном использовании async-инструментов).
5. **Перенос `FormatAsyncCompletion`/`AsyncCompletion` в `internal/message`
   (§2.3).** Единственное предлагаемое межпакетное перемещение кода в
   этой спецификации — подтвердить, что это приемлемо (альтернатива —
   дублирование форматирования, риск расхождения текста уведомлений
   между «живой» и «восстановленной после интеррапта» доставкой).

## Итог

Документ: `docs/plans/2026-09-28-async-phase4-spec.md`. Задачи, закрываемые
этой спецификацией при реализации: #1040, #1038, #1063, BL-2026-09-25-4/-5
(при подтверждении §0.3), плюс подготовка (не реализация) для #1043/#1058
(§4.5) и для durable-планировщика фазы 5/#1025 (общая таблица/lease,
§0.2).

## Решения оркестратора по открытым вопросам (2026-09-28)

1. **`rush jobs kill` на чужом живом host'е — в этой фазе без IPC.**
   Наблюдение (`sessions jobs`) работает из любого процесса; `kill` для
   задачи чужого живого host'а отказывает понятным текстом с PID
   host'а-владельца и подсказкой `rush sessions kill <session>` (он уже
   умеет остановить процесс-владельца целиком). Остановка отдельной задачи
   через границу процесса — вместе с планом пробуждений (#1024/#1026).
2. **BL-4/BL-5** — других источников этих ID нет; привязка к
   `BL-2026-09-25-4/-5` принимается как рабочая, в документе остаётся
   пометка «выведено, не найдено дословно».
3. **Durable claim — для всех четырёх инструментов.** Повторный запуск
   стороннего процесса (`bash`/`run_command`) — ровно тот вред, от
   которого защищает идемпотентность; одна модель на все виды задач
   вместо двух. Стоимость одного `INSERT` на вызов принимается.
4. **Ретеншн — расширение `sessions gc`** (`--jobs-older-than`), без
   новой команды. Значение по умолчанию — ограниченное; строки живых
   задач не удаляются никогда.
5. **Перенос `FormatAsyncCompletion`/`AsyncCompletion` в
   `internal/message` принимается**, если без него восстановление
   вынуждено дублировать форматирование. Перенос — отдельным чистым
   коммитом, с проверкой «чистого переноса» из CLAUDE.md (сортированные
   списки объявлений до/после совпадают), без изменения текста.
