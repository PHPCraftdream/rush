# Статическое ревью перед релизом — 2026-08-26 14:26:01 (Europe/Berlin)

Режим: только чтение. Тесты, сборка и линтеры не запускались.

## Итог

Вердикт: **NO-GO для стабильного релиза**, если включён `RUSH_CACHE_KEEPALIVE=true` или если приложение должно надёжно переживать параллельные старты/остановки. Основные исправления многосессионности действительно закрыли прежний самодедлок на OS-lock, гонку admission и несколько проблем shutdown. Однако остались два релизных риска:

1. keep-alive создаёт Fantasy-агента с реальными tools и `ToolChoiceAuto`, поэтому фоновый «прогрев» может повторно выполнить Bash/edit/MCP-инструмент;
2. startup recovery проверяет кандидата и пишет его отдельным запросом, поэтому при появлении нового сообщения между этими операциями может быть помечен уже не актуальный assistant-message.

Даже при выключенном keep-alive остаются риски зависаний/долгих пауз: утечка DB-ссылок при ошибке `app.New`, неограниченный cleanup через `context.Background()`, abandoned hook-процессы, а также возможное вращение на закрытом pub/sub-канале.

Рабочее дерево уже содержало удалённый `web/dist/.gitkeep`; файл не изменялся.

## Что исправлено последними коммитами

| Коммит | Оценка |
|---|---|
| `e87dc8f7` | Только тестовый бюджет timeout; runtime не меняет. |
| `cc980617` | `setupAppLite` убирает тяжёлую инициализацию для config-only команд. Классификация команд выглядит корректной, но путь ошибки `app.New` всё ещё не освобождает DB-ссылки. |
| `0d58434b` | Убрано `Sessions.ListAll` + полный `Messages.List` для каждой сессии; добавлен индексированный кандидатный запрос. Это заметное улучшение, но сам SQL всё ещё ранжирует историю кандидатов, а `findOrphanPartial` по-прежнему читает весь transcript. |
| `c1b09e15` | Детерминизм теста миграции; runtime не меняет. |
| `14999e2f` | Исправлены нормализация usage, provider-aware cache support, snapshot tools и большая часть keep-alive race. Ниже перечислены оставшиеся дефекты новой реализации. |
| `f6aa416d` | Введены cache-key affinity и keep-alive. Полезная оптимизация, но функция пока opt-in и недостаточно безопасна для фонового выполнения. |

## Релизные находки

### K-0 / P1 — keep-alive может выполнить реальные инструменты

`internal/agent/agent_cache_keepalive.go:201-211` создаёт `fantasy.NewAgent` с `fantasy.WithTools(tools...)` и вызывает `Stream` без `ToolChoice`. В Fantasy это означает `ToolChoiceAuto` по умолчанию (`fantasy@v0.41.2/agent.go:922-1004`), а `processStepStream` затем вызывает `executeTools` для tool calls (`agent.go:548-568`).

Следствие: запрос, предназначенный только для обновления prompt-cache, может повторно вызвать Bash, edit, MCP или другой side-effecting tool. Это может изменить файлы, запустить процессы, захватить session lock, породить дочерние сессии и объяснить редкие фризы/дублирование действий.

Рекомендация: replay должен идти с отключёнными executable tools и явным `ToolChoiceNone` (или через отдельный provider cache-probe API). До исправления keep-alive следует оставлять выключенным по умолчанию и не считать feature release-ready.

### K-1 / P1 — окно между cancel и re-arm keep-alive всё ещё существует

В `agent_cache_keepalive.go:227-252` после успешного replay сначала проверяется `ctx.Err()`, а затем под lock проверяется только наличие более новой записи. Между этими действиями `cancelCacheKeepAlive` может удалить in-flight entry и вызвать cancel. После этого `fireCacheKeepAlive` видит пустую map и снова ставит старый timer.

Сценарий особенно вероятен, если новая реальная сессия отменена/завершилась до собственного schedule. Старый replay тогда может выполниться поверх нового turn. Нужен атомарный tombstone/generation check, включающий состояние отмены, либо удержание in-flight состояния до завершения решения о re-arm.

### K-2 / P1 — ограничение `maxCost` не атомарно

`agent_cache_keepalive.go:308-323` читает `sess.Cost`, затем отдельным запросом вызывает `IncrementCost`. SQL `IncrementSessionCost` (`internal/db/sql/sessions.sql:106-116`) делает только `cost = cost + ?` без предиката бюджета.

Два параллельных turn/replay могут одновременно пройти проверку и превысить лимит (например, с 0.09 до 0.14 при max 0.10). Нужен условный атомарный `UPDATE ... WHERE cost + delta <= max_cost` с проверкой `RowsAffected`, либо транзакционная резервация бюджета.

### K-3 / P2 — ошибка списания считается успехом

В `agent_cache_keepalive.go:320-323` ошибка `IncrementCost` логируется, но функция возвращает `true`; вызывающий код продолжает re-arm. При проблемах DB это создаёт повторные неоплаченные replay и скрывает нарушение accounting. Ошибка должна прекращать re-arm (или переводить сессию в явное quarantine/alert состояние).

### R-1 / P1 — startup recovery может затереть более новое сообщение

`internal/app/app_recovery.go:201-267` получает кандидат, проверяет `finished`, проверяет OS-lock и затем отдельным `Messages.Update` ставит `Process restarted`. SQL-кандидат (`internal/db/sql/messages.sql:446-474`) доказывает, что сообщение было последним только в момент чтения.

Если новый user/assistant message вставлен между `Get` и `Update`, старый assistant всё ещё будет помечен как прерванный; `Update` также переписывает весь Parts blob из устаревшего snapshot. Это TOCTOU и нарушение transcript semantics в параллельных `rush run`.

Рекомендация: условный update, проверяющий, что target остаётся последним rowid/created_at и незавершённым assistant, в одной транзакции; при несоответствии повторно читать latest и пропускать старый кандидат.

### A-1 / P1 — утечка DB pool при ошибке `app.New`

`internal/cmd/root.go:345-355` сначала вызывает `db.Connect`, затем `app.New`. `app.New` дополнительно вызывает `db.ConnectRead` (`internal/app/app.go:166-195`), но при ошибке `InitCoderAgent` (`app.go:281-283`) возвращает `nil` без `Shutdown`/`db.Release`.

Каждый неудачный старт оставляет writer/read pool references и file handles. На Windows это может проявиться как `database is locked`, невозможность удалить data directory и «зависание» следующих запусков. Нужен локальный rollback ownership в `app.New`/`setupApp` на каждом post-connect error.

### A-2 / P2 — cleanup после run может блокироваться до 30 секунд

`internal/app/app_run.go:603-622` в defer использует `context.Background()` для `SetEndedReason` и `Sessions.Get`. SQLite настроен с `busy_timeout=30000` (`internal/db/connect.go:17-26`). После отмены или остановки агент уже завершён, но defer всё ещё может ждать DB lock до 30 секунд, что для пользователя выглядит как freeze после вывода.

Нужен короткий bounded cleanup context (например, отдельный 1-2s budget) и политика пропуска необязательной записи при forced shutdown. `SetEndedReason`/`SetBudget` также не проверяют число затронутых строк, поэтому потеря записи может быть тихой.

### H-1 / P2 — hook timeout оставляет worker/process вне владельца

`internal/hooks/runner.go:172-207` после timeout ждёт `abandonGrace=1s`, затем возвращает результат, оставляя goroutine выполнять `runShell`. Комментарий прямо допускает продолжение записи/процесса после возврата.

Обычный Windows exec path умеет tree-kill, но отказ kill, вложенный interpreter или процесс, удерживающий pipe, всё равно оставляет worker. При серии timeout это накапливает goroutines/processes и удерживает ресурсы. Нужны отслеживание abandoned workers, жёсткий process-group kill и ограничитель числа одновременно abandoned hooks.

### H-2 / P2 — чтение закрытого `messageEvents` может вращаться

`internal/app/app_run.go:846-969` читает `case event := <-messageEvents` без проверки `ok`. `message.Subscribe(ctx)` закрывает канал при отмене контекста (`internal/pubsub/broker.go:114-145`). До выбора ветки `ctx.Done()` select может многократно получать zero-value event и крутить CPU; если broker закрыт независимо, цикл потенциально не завершится.

Использовать `event, ok := <-messageEvents`; при `!ok` отключать ветку/завершать drain.

## Горячие пути и общий обзор

### Agent/mailbox/multi-agent

Хорошие изменения: `internal/agent/agent_run.go:18-70` заменил прежнюю рекурсивную обработку queued turns на один dispatcher loop, поэтому прежний self-deadlock на non-reentrant OS lock устранён. `tryAdmitRunWg` вместе с `admitMu` закрывает race между `CancelAll` и `Wait`; mailbox generation/release-aware state не оставляет idle window во время снятия OS-lock. Циклической блокировки mutex в этих путях статически не обнаружено.

Оставшийся smell: `mailbox.submitted` неограничен (`mailbox.go:147-152`, `mailbox_queue.go:20-30`), а `QueueMessage` не возвращает backpressure. При застрявшем turn или flood запросов память растёт вместе с attachments/prompts. `restartOrphanedWithRetry` дополнительно запускает goroutine на каждый orphan (`agent_ownership.go:393-405`). Нужны bounded queue, явный reject/spill-to-DB и ограниченный worker pool.

`activeRequests` и `mailboxes` создаются лениво и никогда не удаляются (`agent.go:588-614`); это постоянный рост примерно по одному объекту на каждую когда-либо использованную session ID. Для долгоживущего web процесса нужен bounded registry или удаление после финальной era.

### Run queue и DB

`RunQueuePump` имеет отдельные worker/control gates, watchdog lease и bounded tick contexts. Это снижает вероятность классического deadlock. Но durable queue остаётся at-least-once: при потере lease/ошибке terminal Ack возможна повторная side-effecting execution (`run_queue_pump.go:126-140`). Это не исправляется одним mutex; командам, которые меняют внешнее состояние, нужны idempotency keys/операторская дедупликация.

`0d58434b` улучшил discovery, но `ListCandidateInterruptedAssistantSessions` всё ещё делает window ranking по всей истории каждого candidate session (`messages.sql:446-464`), а `findOrphanPartial` загружает полный список сообщений (`app_recovery.go:283-311`). На сессии с большой историей startup/JSON completion остаются дорогими. Нужен latest-only indexed query и bounded pagination.

### Hooks, shell и Windows process spawning

Статический поиск production-кода показал единственный прямой `exec.Command/CommandContext` в `internal/platform/command.go`; остальные пути используют `platform.Command`. Windows `HideWindow=true` применяется также в shell exec handler, MCP и `sessions kill`. Предыдущий дефект видимых окон в обычных spawn paths выглядит закрытым. ConPTY явно исключён и использует собственный headless CreateProcess path; это задокументированное исключение, а не найденный bypass.

Остаются описанные выше abandoned hook workers и узкий accepted gap Unix для процессов, которые сами делают `setsid`/включают job control: такие grandchildren могут пережить group kill и удерживать stdio. Для обычного `foo &` текущая process-group схема выглядит корректной.

### Server/WebSocket

Hub имеет фиксированный pool 12 workers, очередь 64, отдельный control semaphore 256 и replay caps 2000 events/16 MiB. `readPump` закрывает socket/workQueue в правильном порядке, а register race защищён `tryRegister` до и после send. Классического deadlock здесь не найдено.

Риск остаётся операционный: уже принятые queued handlers продолжают работу после disconnect, а durable queue допускает повторное выполнение. Это соответствует текущей архитектуре, но должно быть явно учтено в idempotency и лимитах нагрузки.

### Cache-hit changes

Нормализация usage и provider-aware `CacheSupport` в `14999e2f` исправляют прежние false-native и double-counting случаи. Snapshot фактических `prepared.Tools` также исправляет mismatch cache-key affinity.

Оставшиеся проблемы:

- keep-alive opt-in через `RUSH_CACHE_KEEPALIVE=true` и не описан в пользовательской документации; в default production cache-hit improvement фактически не работает;
- `cloneFantasyMessages` копирует только outer slice и `Content` slice (`usage_fallback.go:36-43`), но вложенные provider options/maps/tool payloads остаются shared; последующая мутация может менять replay snapshot;
- `sessions_cache.go:241-245` жёстко использует TTL 5 минут, тогда как provider profiles владеют разными TTL;
- `detectCacheInvalidations` не сбрасывает `prev` на unknown/estimated usage (`sessions_cache.go:257-279`), поэтому последовательность warm-native → unknown → cold-native может дать ложную инвалидацию.

## Приоритет исправлений до стабильного релиза

1. Запретить executable tools в keep-alive replay; затем закрыть generation race и сделать budget reservation атомарной. До этого не включать keep-alive.
2. Сделать startup recovery условным атомарным update относительно последнего сообщения.
3. Добавить rollback DB Connect/ConnectRead при ошибке `app.New`.
4. Ограничить cleanup contexts и lifecycle hook processes; исправить `messageEvents` closed-channel handling.
5. Ввести bounded mailbox/agent registries и ограничить recovery fan-out; заменить full transcript scans latest-only запросами.
6. Синхронизировать cache diagnostics с provider profiles, сделать deep/immutable snapshot и сбрасывать baseline на неизвестном usage.
7. Добавить idempotency strategy для side-effecting durable queue calls и исправить drift комментариев timestamp в initial migration (`internal/db/migrations/20250424200609_initial.sql`).

После этих изменений повторить статический аудит и отдельно проверить shutdown/parallel-start traces. В рамках этого ревью тесты не запускались по инструкции пользователя.
