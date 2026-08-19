# Статический review после исправлений multi-agent/session freeze — 2026-08-19

## Вердикт

**NO-GO для стабильного релиза.** Большинство блокеров из review 2026-08-18
исправлено по существу, и классического взаимного deadlock-а вида `A -> B -> A`
в просмотренных изменениях не найдено. Однако контракт synchronous drain всё ещё
имеет release-blocking гонку: если background pump первым получил admission,
`DrainSessionNow` не знает результат фонового выполнения и заранее ставит
`drained=true`. Ошибка, terminal failure, потеря lease или lock contention фонового
выполнения могут превратиться в успешный exit code 0.

Кроме того, hard rename `large/small -> smart/fast` завершён не полностью. В web
остались два исполняемых пути со старыми ключами, один из которых может отправить
обычный smart-запрос вместо явно выбранного fast-запроса. В CLI help и встроенном
config skill пользователю всё ещё предлагаются уже удалённые ключи и флаги.

До исправления P0 ниже утверждать, что зависания/ложные завершения устранены,
нельзя. После исправления P0 и web-регрессий нужен отдельный динамический release
gate; в этом review он намеренно не выполнялся.

## Область и метод

- HEAD: `da10303fc7514f484d928b473c08e0d8e5c3ba86`.
- База: `e0890a1b09d1f7dd1ed9160a9003acb74dd51fe5`, предыдущий статический follow-up.
- Диапазон: 21 commit, 224 файла, `+5384/-2497`.
- Просмотрены история Git, diff и связанные production-пути в
  `internal/session`, `internal/agent`, `internal/app`, `internal/message`,
  `internal/db`, `internal/server`, `internal/cmd` и `web/src`.
- Проверка была только статической. Тесты, build, lint, race detector, генераторы
  и приложение не запускались по прямому указанию пользователя.
- Существующий untracked `dev/` не изменялся.

## Release blockers

### P0-1. Background drain всё ещё может сообщить успех без успешного результата

`DrainSessionNow` сначала пытается получить общий per-session admission. Если его
уже держит background worker, функция без знания результата этого worker-а сразу
делает `drained = true`, ждёт освобождения admission и повторяет цикл
(`internal/session/run_queue_drain_session.go:90-116`). Сам комментарий рядом
признаёт только вариант `ErrCallQueuedNotExecuted`, но дефект шире: outcome вообще
не передаётся.

Ложный успех возможен как минимум при следующих фоновых исходах:

- `ErrCallQueuedNotExecuted` или `SessionLockBusyError`: row возвращена в pending;
  повторная попытка самого drain снова видит внешнего владельца и возвращает
  `drained=true, err=nil`;
- terminal `AlreadyAttempted`: background worker помечает row terminal-failed и
  возвращает ошибку, но drain после освобождения admission видит пустую pending
  queue и возвращает успех;
- `AckRunQueueEntry` failed: turn выполнился, но не committed, row остаётся leased;
  drain не видит pending row и снова возвращает успех;
- потеря lease: outcome принадлежит новому owner-у, но один факт ранее занятого
  admission уже был принят за выполненную continuation.

`RunNonInteractive` трактует `drained=true, err=nil` как успешно завершённую
durable continuation и заменяет исходную cancellation на nil
(`internal/app/app_run.go:738-770`). Поэтому внешне это даёт exit code 0 и
успешный envelope при невыполненной, terminal-failed либо незафиксированной работе.

Это остаток той же проблемы, ради которой вводились commits `3ba00874` и
`9a9c70d6`, но direct path исправлен, а raced-background path — нет.

**Что исправить:** admission entry должен содержать owner/execution identity,
`done` и typed terminal outcome. `DrainSessionNow`, встретив чужой локальный owner,
должен дождаться его `done` и обработать фактический outcome тем же кодом, что и
собственное `executeEntrySync`. Boolean `inFlight`/предположение «был занят — значит
выполнился» для этого контракта недостаточно.

**Минимальный regression contract:** отдельно зафиксировать background-wins гонку
для success, busy/no-execution, terminal failure, Ack failure и lease loss. Только
первый случай может дать `(true, nil)`.

## Высокий приоритет

### P1-1. Кнопка fast в web всё ещё читает удалённый `config.models.small`

`sendWithFastModel` берёт session override из `FastModel*`, но fallback к общей
конфигурации всё ещё читает `config.models.small`
(`web/src/store.ts:583-595`). После hard rename конфигурация содержит
`models.fast`. Если у сессии нет собственного override, `fastModel` остаётся
undefined и payload уходит без `smartModel` override. В результате явно выбранная
операция «send with fast» может выполниться обычной smart-моделью: это и
функциональная ошибка, и неожиданный расход дорогой модели.

**Что исправить:** читать `config.models.fast`; проверить путь с отсутствующим
session override и с одним глобальным/workspace fast slot.

### P1-2. Durable recovery смешивает два поколения provider config

`RebuildSessionAgentCall` получает один atomic snapshot в `cfg` и из него строит
`smartModel/fastModel` (`internal/agent/coordinator_interrupt.go:436-455`), но затем
provider options и sampling knobs берёт из нового live
`c.cfg.Config()` (`:459-466`). Reload между этими точками создаёт model/client из
generation N и provider options из generation N+1. Это особенно опасно именно на
durable recovery path, где восстановленный вызов должен быть детерминированным.

**Что исправить:** использовать `cfg.Providers.Get(...)` из уже захваченного
snapshot и передавать этот snapshot дальше по всей операции rebuild.

### P1-3. Agent/tools construction всё ещё состоит из серии live config reads

Prompt теперь правильно получает pinned config (`7879683e`), но `buildAgent` и
`buildTools` продолжают независимо читать provider, Options, worker predicate,
model metadata, hooks, MCP, grep/ls options, attribution и skills paths через
многочисленные `c.cfg.Config()` (`internal/agent/coordinator_tools.go:24-104`,
`:135-246`). Код в `buildToolsAgentConfig` прямо документирует, что snapshot
threading оставлен вне scope (`:135-140`).

Reload во время параллельной сборки prompt/tools способен создать один agent с
несогласованным набором: prompt считает worker доступным, а toolset — нет; модель
и bash metadata происходят из разных поколений; hooks/MCP не соответствуют
остальным options.

**Что исправить:** захватывать один `*config.Config` в начале `buildAgent` и
передавать его в `buildAgentModels`, prompt и `buildTools`. Осознанно live должны
оставаться только явно заявленные runtime-policy checks, например peak hours.

### P1-4. Hard rename оставил исполняемый баг очистки session slot

`clearSessionModelSlot` принимает `"smart" | "fast" | ...`, но lookup map всё ещё
содержит ключи `large` и `small` (`web/src/store.ts:383-395`). Для smart/fast
`prefix` равен `undefined`, поэтому optimistic update создаёт поля
`undefinedProvider` и `undefinedID`, а реальные `SmartModel*`/`FastModel*` остаются
на экране до следующей полной синхронизации.

Server payload ниже сформирован новым именем и, вероятно, сохраняет очистку, но UI
немедленно показывает противоречивое состояние. Это прямой regression commit-а
`9c4033b6`, а не косметический комментарий.

**Что исправить:** ключи map должны быть `smart`/`fast`; тип map лучше сделать
`Record<ModelType, ...>`, чтобы TypeScript запрещал такие пропуски.

### P1-5. Hard rename оставил пользователю команды и конфиг, которые больше не работают

При заявленной политике «hard, no backwards compatibility» следующие тексты уже
не являются безопасным historical alias:

- `crush models use --help` предлагает `--large` и `--small`, хотя зарегистрированы
  только `--smart` и `--fast` (`internal/cmd/models_use.go:27-35`, `:86-93`,
  `:241-248`);
- runtime validation errors также требуют несуществующие `--large/--small`
  (`internal/cmd/models_use.go:108-127`);
- пример `crush models unset small --global` не проходит собственный список
  допустимых аргументов `smart|fast|...` (`internal/cmd/models_unset.go:15-47`);
- builtin `crush-config` skill генерирует `models.large/models.small`, которые
  новая конфигурация больше не читает
  (`internal/skills/builtin/crush-config/SKILL.md:90-110`);
- описание model slots для `crush_info` также обучает старым именам
  (`internal/agent/tools/crush_info.md:11-16`).

Это особенно рискованно для agent-driven configuration: сам агент получает
устаревший skill и может молча записать неработающий config.

**Что исправить:** сделать отдельный sweep пользовательских строк, examples,
skills и runtime errors; не ограничивать rename компилируемыми идентификаторами.

## Средний приоритет

### P2-1. `DrainSessionNow` сохраняет старую ошибку после успешной повторной попытки

После любого реально запущенного `executeEntrySync` функция делает
`err = execErr` и продолжает цикл (`internal/session/run_queue_drain_session.go:226-233`).
Если retryable attempt был nacked, а следующая попытка той же row успешно Ack-нута,
`execErr` становится nil, но накопленный named return `err` явно не очищается.
После опустевшей queue функция возвращает старую ошибку, хотя последний retry
успешно committed.

Возможно, product contract хочет сообщать любую промежуточную ошибку, но тогда
немедленный in-process retry противоречит итоговой семантике background pump,
который после успешного следующего tick считает row завершённой. Outcome следует
определить явно: либо success очищает retryable error той же logical row, либо
drain прекращается на первой ошибке вместо скрытого retry.

### P2-2. Outbox fallback повторно использует уже истёкший context

`restartOrphanedWithRetry` создаёт один 30-секундный `enqueueCtx`, сначала вызывает
основной enqueue, а при ошибке тем же context вызывает `WriteToOrphanOutbox`
(`internal/agent/agent_ownership.go:354-388`). Если основной DB call исчерпал
deadline, fallback гарантированно получает уже cancelled context и не имеет ни
одной собственной попытки. Для быстрой constraint/serialization-like ошибки путь
работает, но для зависшего/locked DB — именно того класса отказа, где fallback
нужнее всего, — нет.

Обе таблицы находятся в одной DB, поэтому новый context не гарантирует успех, но
он хотя бы даёт fallback отдельный bounded budget и честно реализует заявленный
контракт.

### P2-3. Unix external kill всё ещё не гарантирует остановку provider child tree

Предыдущий review установил, что `sessions kill` знает PID Crush из lock metadata,
а CLI provider запускается в отдельной process group. Убийство одного Crush PID
может оставить provider и descendants жить отдельно. В просмотренном диапазоне
это не исправлено. Для симптома «сессия убита, но работа/процесс продолжает жить»
это остаётся эксплуатационным риском; нужен registry/handoff child process groups
либо другой явный process-tree contract.

## Deadlock и freeze assessment

### Что выглядит закрытым

- `executeEntrySync` теперь действительно наследует execution context caller-а;
  background path получает `p.ctx`, synchronous drain — caller ctx.
- Synchronous drain зарегистрирован в `workerWg`, а admission относительно `Stop`
  защищён `admitMu`; shutdown больше не считает такой worker отсутствующим.
- Background и synchronous dispatch используют одну атомарную
  `admitSession`-операцию, поэтому прежнего параллельного выполнения двух rows одной
  session внутри одного pump instance не видно.
- Ошибка Ack больше не возвращается как nil на direct execution path.
- Lease renewal/watchdog и DB outcome writes имеют deadlines; Stop pump-а имеет
  единый пятисекундный bound.
- Watchdog disarm перед bounded title join закрывает ложный timeout уже завершённого
  turn. Defer order `cancel()` перед ожиданием `wd.done` корректен.
- Checkpoint generation участвует в атомарном SQL predicate, поэтому более старая
  partial generation не может перезаписать новую.
- Orphan outbox transfer остаётся одной transaction, а poison rows теперь имеют
  bounded quarantine.

### Что не позволяет обещать «freeze-free»

- P0-1 оставляет несовпадение наблюдаемого и фактического outcome. Это не mutex
  deadlock, но пользовательский симптом тот же: команда завершилась не тем
  результатом, durable row осталась жить/повторилась позже.
- Watchdog может отменить context, но физически не способен заставить произвольный
  provider/tool вернуться, если downstream игнорирует cancellation. Для shell/CLI
  путей добавлены kill/wait bounds, но универсальной гарантии для каждого MCP и
  provider implementation статический обзор не даёт.
- At-least-once queue по-прежнему допускает повтор provider/tool side effects после
  Ack failure или lease-loss race. Typed error сообщает правду, но fencing/idempotent
  commit protocol не появился.
- Несколько взаимодействующих owner-машин всё ещё существуют отдельно: mailbox,
  OS session lock, durable queue, synchronous drain и orphan outbox. Общий admission
  убрал одну гонку, но отсутствие outcome handoff в P0-1 показывает, что state
  machine пока не едина.

Итог: нового классического lock-order deadlock-а в изменениях не найдено, но
release-level гарантия отсутствия зависаний/ложных завершений пока не доказана.

## Общий обзор качества кода

Сильные стороны текущего состояния:

- fixes в основном адресуют причинные контракты, а не только симптомы;
- context ownership, lease ownership и Ack semantics стали заметно явнее;
- DB-side generation fence и atomic outbox transfer надёжнее memory-only guards;
- bounded waits добавлены в title, checkpoint, pump stop, DB writes и process wait;
- hard rename проведён через Go/DB/wire большую часть пути.

Системные слабости:

- `internal/agent/agent_turn.go` всё ещё около 1859 строк и содержит крупную
  callback-driven state machine с множеством closure и shared state. Forensic
  comments помогают расследованию, но уже местами расходятся с кодом.
- В `stream_watchdog.go:166-181` и call-site
  `agent_turn.go:478-480` комментарии утверждают, что `recordActivity` вызывается
  на каждом tool-in-flight tick, тогда как текущая реализация специально этого не
  делает (`stream_watchdog.go:356-384`). Это опасно именно в concurrency-коде:
  неверный комментарий превращается в ложную гарантию heartbeat.
- Rename sweep ориентировался на компилятор и отдельные fixture, но не поставил
  единый запрет/проверку старых runtime strings и bundled skills. Найденные web и
  help regressions показывают, что release gate по rename был неполным.
- `internal/db/sql` защищён ASCII pre-push guard из-за известного sqlc defect, но
  CI всё ещё не сверяет committed generated files со свежим `sqlc generate`.
- Closing checkpoint фиксирует 37 pre-existing Playwright failures. Даже если они
  не созданы этим диапазоном, красный baseline снижает способность e2e suite
  обнаруживать следующие rename/session regressions.
- Два ранее замеченных sub-agent tool defects (append overwrite и ложный success
  write в отсутствующий path) остаются неоформленными/неразобранными.

## Рекомендуемый порядок исправлений

1. Заменить boolean admission observation на outcome handoff и закрыть все пять
   background-wins вариантов из P0-1.
2. Исправить `sendWithFastModel` и `clearSessionModelSlot`; добавить type-safe slot
   mapping без строковых legacy keys.
3. Полностью вычистить runtime/help/skill остатки `large/small` согласно выбранной
   hard-rename policy.
4. Использовать pinned snapshot в `RebuildSessionAgentCall`, затем протянуть один
   snapshot через `buildAgent/buildTools`.
5. Определить retry outcome synchronous drain и отдельный context budget outbox.
6. После point fixes выполнить release gates отдельно: targeted race scenarios,
   full Go tests/lint/race, web typecheck/build и исправленный e2e baseline.
7. До stable release задокументировать at-least-once side effects и решить Unix
   child-tree kill либо как fix, либо как явно принятый limitation.

## Минимальный GO gate

GO возможен только если одновременно выполняются следующие контракты:

- background-wins drain возвращает success только после фактического Ack;
- busy, terminal, Ack-failed и lease-lost outcomes не превращаются в exit 0;
- fast action без session override реально выбирает `models.fast`;
- очистка smart/fast slot сразу корректно обновляет UI и server state;
- CLI help, errors и builtin skill используют только `smart/fast`;
- durable rebuild и agent/tool construction не смешивают config generations;
- динамические test/lint/race/web gates проходят на не-красном baseline.

До этого текущий HEAD следует считать **release candidate для следующего раунда
исправлений, но не стабильным релизом**.
