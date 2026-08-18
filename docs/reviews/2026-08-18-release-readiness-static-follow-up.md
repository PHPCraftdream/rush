# Follow-up аудит готовности к релизу — 2026-08-18

## Вердикт

**NO-GO.** Четыре блокера из статического аудита 2026-08-13 исправлены по
существу, но fix live-continuation для короткоживущего `crush run` создал новый
параллельный путь исполнения durable queue. В нём найдены два новых блокера:

1. `DrainSessionNow` может вернуть `(drained=true, err=nil)`, хотя continuation
   не исполнился и был только возвращён в `pending` из-за занятого session lock;
2. execution внутри `DrainSessionNow` игнорирует переданный context, не входит
   в lifecycle accounting pump-а и может продолжить писать после timeout,
   возврата `RunNonInteractive` и начала shutdown.

Дополнительно успешный provider turn с неуспешным `Ack` возвращается как
успех, а `inFlight` не способен корректно представить две пересекающиеся
дорожки для одной session. Это сохраняет окна ложного успеха и повторного
исполнения.

Классического взаимного deadlock-а (`A -> B -> A`) в просмотренных изменениях
не найдено. Исправлен прежний defer-order deadlock interrupt ticker-а, его
операции теперь имеют deadline. Но пользовательский симптом «сессия зависла»
всё ещё достижим через detached execution, которое переживает timeout, и через
потерю корректного состояния owner/terminal outcome.

## Область ревью

- HEAD: `b6127e9df21e40ffa922f76378c2111cd21893f0` (`main`).
- База: `f9a70d045973ae9c8b09288d928ada70c6f2cc1a`, точка прошлого аудита.
- Диапазон: 111 коммитов, 351 файл, `+57875/-38587`.
- Проверены fixes прежних P0/P1, связанные production-пути и новые изменения
  agent/session/config/app/shell.
- Большая часть объёма — разделение файлов и тестов, но в диапазоне также есть
  новые model/usage/tool/process-tree/background-job функции.
- Ревью только статическое: тесты, build, lint и race detector не запускались.
- Существующие пользовательские изменения `web/dist/.gitkeep` и `dev/` не
  трогались.

## Что из прошлого отчёта закрыто

### Закрыто: post-compaction continuation

`runTurn` больше не отправляет внутреннее продолжение через внешний
`mailbox.submit`. При незавершённых tool calls оно возвращается напрямую как
`next, hasNext=true` (`internal/agent/agent_turn.go:1759-1789`). Поэтому
durable row не может быть `Ack`-нут до выполнения continuation.

### Закрыто: абсолютный lease deadline

Watchdog хранит ровно тот absolute `lease_expires_at`, который был записан в
SQLite, и сравнивает время с `expiry - margin`
(`internal/session/run_queue_entry_exec.go:198-242`). После успешного renew он
публикует в atomic именно `newExpiresAt`, а не время возврата DB-вызова
(`:281-322`). Прежнее окно slow-successful-renew закрыто.

Остаётся честно задокументированная at-least-once семантика: cancellation не
является fencing token для уже начатых provider/tool side effects
(`:155-167`).

### Закрыто: orphan outbox `processing` forever

Перенос теперь выполняется одной SQLite-транзакцией: idempotent enqueue в main
queue и delete outbox row (`internal/session/session_orphan_outbox.go:52-101`).
Промежуточный runtime state `processing` больше не используется. Initial drain
также запускается сразу при старте pump-а
(`internal/session/run_queue_lifecycle.go:121-145`).

### Закрыто: in-place provider removal

`RemoveProviderAPIKey` копирует `Config` и Providers map, затем публикует новую
generation (`internal/config/store_oauth.go:353-379`). Старый snapshot больше
не мутируется задним числом.

### Частично закрыто: config snapshot threading

Model pair, provider config и worker predicate теперь берутся из одной
generation; 401 rebuild использует `pinned.providerCfg`. Это устраняет
конкретные torn reads прошлого отчёта. Но system prompt и agent construction
всё ещё повторно читают live store — см. P1-2 ниже.

### Закрыто: interrupt ticker join

Каждый tick получает собственный 10-секундный context
(`internal/agent/coordinator_interrupt.go:31-40`, `:85-113`). Cancel-before-join
сохранён. Явного бесконечного join на этой дорожке больше нет, если downstream
соблюдает Go context contract.

## Новые release blockers

### P0-1. Lock contention ошибочно превращается в успешный durable continuation

`DrainSessionNow` устанавливает `drained=true` сразу после получения lease,
до реального исполнения (`internal/session/run_queue_drain_session.go:71-77`).
Если `executeEntrySync` возвращает `ErrCallQueuedNotExecuted` или
`SessionLockBusyError`, durable row корректно освобождается без attempt penalty
(`internal/session/run_queue_entry_exec.go:436-444`, `:460-473`). Но затем
`DrainSessionNow` возвращает `drained, nil`
(`internal/session/run_queue_drain_session.go:120-135`). Поскольку `drained`
уже true, фактический результат — `(true, nil)`.

`RunNonInteractive` трактует эту пару как «continuation ran in this process» и
завершает исходный отменённый run успехом
(`internal/app/app_run.go:738-763`). На деле row остаётся `pending` и будет
исполнен позднее другим pump/process. Получаются одновременно:

- ложный exit code 0 / успешный JSON envelope;
- отсутствие continuation в текущем процессе;
- позднее неожиданное исполнение уже после сообщения об успехе.

Комментарий в `DrainSessionNow` утверждает обратное: busy outcome должен
оставить original outcome. Реализация нарушает собственный контракт.

**Исправление:** различать `leased/observed`, `executed`, `committed` и
`deferred-busy`. `drained=true` допустим только после terminal commit данной
logical execution. Busy должен вернуть `drained=false` либо отдельный outcome,
который восстанавливает original cancellation, а не успех.

### P0-2. Синхронный drain не синхронен с context и shutdown

`executeEntrySync(ctx, leased)` принимает context, но вообще его не использует:
единственное упоминание параметра — сигнатура. Context для
`Coordinator.Run` создаётся от `context.Background()`
(`internal/session/run_queue_entry_exec.go:31-58`). Поэтому timeout/cancel
caller-а не останавливает continuation.

В `RunNonInteractive` drain запускается отдельной goroutine
(`internal/app/app_run.go:738-764`). Select может получить `ctx.Done()` и
вернуть управление (`:840-843`), пока эта goroutine продолжает provider/tool и
DB работу. В отличие от обычного background pump worker-а, `DrainSessionNow`
не регистрируется в `workerWg`; `RunQueuePump.Stop()` его не ждёт. Он также сам
ведёт `inFlight/execSem`, минуя admission gate.

Следствия:

- `--timeout` не ограничивает уже начатый durable continuation;
- `App.Shutdown` может считать pump idle и закрыть DB под живым drain;
- goroutine может писать сообщения после того, как CLI уже вернул ошибку;
- при живом процессе остаётся ресурсный leak; при повторном lease возможна
  конкуренция старого и нового исполнителей.

Это прямой путь к прежнему симптому «команда закончилась/отменилась, а сессия
продолжает жить или зависла».

**Исправление:** сделать execution parent явным. Обычный pump должен передавать
долгоживущий `p.ctx`, а `DrainSessionNow` — caller `ctx`; scan context передавать
как execution context нельзя. Любой synchronous drain должен проходить тот же
admission/workerWg lifecycle либо иметь собственный join, который учитывает
`Stop`. Outcome writes могут сохранить отдельные bounded contexts от
`Background`, но сам `Coordinator.Run` обязан наследовать execution parent.

### P0-3. Ошибка `Ack` логируется, но возвращается как успех

Документированный контракт `executeEntrySync` говорит: nil возвращается только
при ack-нутом успехе (`internal/session/run_queue_entry_exec.go:23-30`). Код при
ошибке `AckRunQueueEntry` лишь пишет log и всё равно возвращает nil
(`:388-400`).

После выполненного provider/tool turn row остаётся leased; позднее lease
истечёт и работа может исполниться повторно. Для `DrainSessionNow` это ещё и
немедленный ложный успех: следующий lease не видит leased row, функция
возвращает `(true, nil)`, а CLI подтверждает completion.

Полностью устранить at-least-once retry без fencing/idempotent side effects
нельзя, но API не должен утверждать, что terminal commit состоялся.

**Исправление:** возвращать ack error отдельным typed outcome. Не запускать
немедленное повторное provider execution, но и не подтверждать terminal
success пользователю. Для стабильной семантики нужен durable execution result
или commit/fencing protocol, позволяющий повторить только Ack, а не весь turn.

## Высокий приоритет

### P1-1. `inFlight map[string]struct{}` сломан появлением второй dispatch-дорожки

`processEntry` сначала проверяет `inFlight`, затем lease-ит и ставит boolean
marker (`internal/session/run_queue_entry_dispatch.go:24-37`, `:125-146`). Это
было рассчитано на один последовательный `tick`. `DrainSessionNow` lease-ит до
проверки marker-а и без общей атомарной admission operation, затем безусловно
присваивает и удаляет тот же key
(`internal/session/run_queue_drain_session.go:71-77`, `:97-125`).

Если background worker уже исполняет row A, drain может lease-нуть row B той же
session. Оба считают себя represented одним boolean. Тот, кто завершится
первым, удалит key, хотя второй ещё жив. Следующий tick/drain снова увидит
session свободной. OS session lock обычно не даст двум turns выполнить модель
одновременно, но возникнут лишние lease/Nack циклы, ложные drain outcomes и
некорректный shutdown visibility.

**Исправление:** одна атомарная per-session admission API для background и
synchronous paths. Marker должен нести owner/execution ID и done channel либо
refcount; удалять его может только создавший owner.

### P1-2. System prompt всё ещё смешивает поколения config

`resolveSessionModels` действительно pin-ит `cfg` и передаёт его в
`workerSubAgentActive`, но затем вызывает `prompt.Build` с live `ConfigStore`
(`internal/agent/coordinator_models.go:68-70`, `:191-207`). Внутри
`promptData` снова выполняется `store.Config()` и из него читаются Options,
context paths, skills, Models и вся `PromptDat.Config`
(`internal/agent/prompt/prompt.go:238-298`). Reload между model resolve и
prompt build по-прежнему создаёт model/provider из generation N и prompt из
N+1.

`buildAgent` ещё шире смешивает live reads: после `buildAgentModels` отдельно
читает provider, Options, worker flag, tools, hooks, MCP и model metadata
(`internal/agent/coordinator_tools.go:24-99`, `:130-236`). В комментарии прямо
сказано, что snapshot threading для buildTools оставлен out of scope
(`:130-135`). Для multi-agent системы это production race, а не только smell.

**Исправление:** `Prompt.Build` должен принимать pinned config data, а
`buildAgent/buildTools` — один captured snapshot на всю construction operation.
Live reload policy следует применять на следующем turn, кроме явно
документированных live checks вроде peak-hours.

### P1-3. Checkpoint writer после timeout может перекрыть более новый partial state

`stopCheckpoint` ждёт writer не более пяти секунд, затем обнуляет
`checkpointDone` и разрешает следующую generation
(`internal/agent/agent_turn.go:794-821`). Старому writer посылается cancel, но
весь этот код существует именно для случая, когда DB/filesystem может зависнуть
и не сразу соблюсти context.

DB guard защищает только terminal row (`finished_at IS NULL` в
`internal/db/sql/messages.sql:48-60`). Он не различает две partial generation.
Старый зависший partial update, вернувшийся после нового partial update, может
перезаписать более свежие parts. Terminal update в конце исправит DB, но crash
между stale overwrite и terminal commit оставит recovery с устаревшим
checkpoint и может повторить уже выполненные tool actions.

**Исправление:** checkpoint generation/revision должна участвовать в
conditional SQL update, а не только в pre-write memory check. Например,
`UPDATE ... WHERE finished_at IS NULL AND checkpoint_generation < ?`.

## Прочие замечания и code smells

### Orphan outbox может ретраить poison row бесконечно

После отказа от claim-state удалены attempts/terminal-failed transitions.
Malformed, никогда не enqueue-able row теперь логируется и повторяется каждые
15 секунд бесконечно (`internal/session/run_queue_orphan_drain.go:64-82`,
`internal/db/sql/orphan_outbox.sql:1-16`). Это лучше silent loss, но создаёт
вечный log/DB churn. Atomic transfer и bounded quarantine совместимы: failure
counter/last error можно обновлять отдельной транзакцией без промежуточного
`processing` owner-state.

### `crush sessions kill` на Unix не гарантирует kill CLI child tree

`KillProcess` делает group kill только если целевой Crush PID уже является
group leader; иначе убивает один PID (`internal/session/kill_unix.go:12-65`).
CLI provider child специально запускается в отдельной process group
(`internal/agent/cliprovider/procgroup_unix.go:34-49`). Поэтому внешний
`sessions kill`, знающий PID Crush из lock metadata, может оставить provider и
его descendants живыми. Это уже отмечено в checkpoint 2026-08-18 и требует
явного product decision/registry handoff.

### Context-параметр, который можно молча игнорировать, — опасный API

`executeEntrySync(ctx, ...)` компилируется при полностью неиспользуемом `ctx`.
Именно такой API превращает timeout из гарантии в комментарий. Для ownership
критичных функций полезно разделять `scanCtx`, `executionCtx`, `outcomeCtx` в
именах/типах и иметь contract tests, доказывающие cancellation propagation.

### Разделение файлов улучшило навигацию, но не уменьшило сложность state machine

Production split в целом выглядит механическим и полезным. Однако
`agent_turn.go` всё ещё 1842 строки, а `runTurn` около 1577 строк. Решение не
разбивать его без безопасной seam обосновано: механическое дробление closures
действительно может ухудшить invariants. Но это не отменяет корневую проблему:
mailbox, pending inject, durable queue, synchronous drain, orphan outbox и OS
lock остаются несколькими взаимодействующими state machines.

Длинные исторические комментарии уже начали противоречить реализации:

- `DrainSessionNow` назван bounded caller context, хотя execution его игнорирует;
- `executeEntrySync` обещает nil только после Ack, но возвращает nil при Ack
  error;
- `inFlight` описан исходя из единственного tick dispatcher-а, хотя появился
  concurrent synchronous dispatcher.

Комментарии полезны как forensic record, но release invariants должны быть
выражены типами, terminal outcomes и сквозными тестами.

## Общая оценка кода

Положительные изменения заметны:

- прошлые четыре P0 исправлены адресно и снабжены более содержательными tests;
- absolute lease deadline и atomic outbox transfer проще прежних локальных
  recovery guards;
- interrupt ticker, title generation, shutdown и process-tree paths получили
  bounded waits/diagnostics;
- крупные файлы в большинстве случаев разделены по ответственности;
- tool errors теперь логируются и model-correctable failures не обязаны
  завершать весь turn;
- model usage/provenance и web model scope стали явнее.

Главный системный риск не изменился: исправления добавляются как новые
side-paths вокруг общей state machine. `DrainSessionNow` — точный пример:
локально он закрывает liveness одного CLI сценария, но дублирует admission,
inFlight, execution context и terminal-outcome semantics pump-а. В результате
один P0 закрыт и сразу появились новые.

## Рекомендуемый порядок работ

1. Исправить propagation context и lifecycle registration synchronous drain.
2. Ввести typed terminal outcome для `executeEntrySync`; Ack error не должен
   быть nil/success.
3. Исправить busy outcome: `drained` означает committed execution, не lease.
4. Объединить per-session admission background pump и `DrainSessionNow`, с
   owner token/done channel вместо boolean map.
5. Провести один config snapshot через prompt/agent/tools construction.
6. Добавить DB-side checkpoint generation fence.
7. Определить policy для poison outbox и Unix external tree kill.
8. После point fixes вернуться к единой execution state machine; не добавлять
   третий dispatcher для следующего edge case.

## Минимальный release gate

До GO нужны как минимум сквозные проверки следующих контрактов:

- busy session во время live-continuation не даёт ложного exit 0;
- `--timeout` отменяет уже начатый synchronous durable execution;
- shutdown ждёт drain либо явно помечает forced shutdown и не закрывает DB;
- Ack failure никогда не сообщается как committed success;
- background tick и drain не создают двух owners одной session;
- config reload в каждой seam build/prompt/tools не смешивает generations;
- поздний checkpoint generation не может быть перезаписан ранним;
- затем отдельно должны пройти test/lint/race/build gates, которые намеренно не
  запускались в этом статическом read-only ревью.
