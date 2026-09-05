# Ревью последних коммитов и общий аудит SDK — раунд 16 (1209)

Метка запроса: 2026-09-04, round 16 — 1209.

Проверенный срез основной репозитории:
`62db7f49a9771b40ebcbc79cc71c9fe5264979f9`
(`docs: add SDK library review round 15`).

Предыдущее ревью: `62db7f49a9771b40ebcbc79cc71c9fe5264979f9`,
отчёт `docs/reviews/2026-09-04-sdk-library-review-round-15-6928.md`.

Основной диапазон исправлений после проверенного в раунде 15 source snapshot:
`62d191ad1127186144f95da8435cda878dc598d1..62db7f49a9771b40ebcbc79cc71c9fe5264979f9`.
В диапазоне пять коммитов, 12 файлов, 1038 добавленных и 20 удалённых строк.
Из них три production/test fix-коммита (`abed8946`, `31ea604f`, `0cd9dd3f`) и
два review-only коммита (`239c8943`, `62db7f49`).

Метод: read-only статическое ревью committed-кода, diff'ов, тестовых oracle,
публичного SDK-контракта и путей владения ресурсами. Production/test-файлы не
изменялись. Тесты, сборка, subprocess probes и race detector не запускались,
чтобы не мешать параллельным агентам и не добавлять нагрузки к уже известной
флейковой среде. Поэтому race/leak-выводы ниже основаны на достижимых путях и
правилах Go memory model, а не на одном удачном динамическом прогоне.

## Итоговый вердикт

Исправления R15-1, R15-2 и R15-4 реализованы точно и достаточно для своих
локальных задач. R15-3 не исправлен: валидный non-comparable `DiskProvider`
по-прежнему вызывает runtime panic.

Общий SDK пока нельзя считать готовым к заявленному concurrent/multi-tenant
использованию. Найден новый P0 в process-wide менеджере background jobs:
один tenant/client может читать или останавливать job другого, а `Close()`
одного клиента вызывает глобальный `KillAll`. Кроме него найдены четыре новых
P1 в concurrent `Open`, subscriptions, shared writers и MCP lifecycle, плюс
один P2 в zero-value `Client` contract.

Открыто в этом срезе: **1 P0, 5 P1 (включая перенесённый R15-3), 1 P2**.
P0 и P1 являются release blockers для формулировок «multiple simultaneous
library-mode Clients are supported» и «one Client, many concurrent calls».

## Проверка исправлений раунда 15

| Finding | Статус в `62db7f49` | Оценка |
| --- | --- | --- |
| R15-1, `rush_logs` читает host cwd | Закрыт | `abed8946` добавляет tool и в initial disabled list, и в финальный no-real-workspace floor; при пустом `DataDirectory` log path больше не строится, сам tool не создаётся. Behavioral test проверяет actual schema, not-found response и отсутствие host marker в provider bodies/transcript. |
| R15-2, `git_read` запускает host `git` | Закрыт | `31ea604f` добавляет `git_read` в оба слоя запрета и проверяет top-level/worker schema. Одновременно `agentic_fetch` добавлен в финальный floor, устраняя прежнюю случайную зависимость от positive re-add lists. |
| R15-3, interface equality для `DiskProvider` | **Открыт** | Все три небезопасных сравнения остались: `internal/app/app_run.go:660-662`, `internal/agent/coordinator_tools.go:462-466`, `internal/agent/tools/fs_library_virtual_root.go:72-74`. |
| R15-4, retry теряет `-parallel/-p` | Закрыт | `0cd9dd3f` передаёт flags отдельно и применяет их в первом запуске и обеих retry-ветках. Self-test извлекает реальную функцию, подменяет `go` и проверяет оба пути. Word splitting здесь намеренный и безопасен для двух текущих констант. |

Новые тесты R15-1/R15-2 заметно сильнее старого ручного denylist oracle: они
смотрят actual provider schema и результат прямой попытки вызова. Но общий
архитектурный риск denylist-модели сохраняется: безопасный ephemeral toolset
всё ещё доказывается перечислением известных опасных tools, а не allowlist'ом
явно diskless capabilities. Новый `job_output` gap ниже — ещё один пример того,
как новый тип внешней capability проходит мимо классификации.

## Находки

### R16-1 — P0: background jobs глобальны и не имеют client/session ownership

`internal/shell/background.go:329-348` создаёт один process-wide
`BackgroundShellManager`; `internal/shell/background.go:381` выдаёт
предсказуемые последовательные ID (`001`, `002`, ...). Все jobs всех App/SDK
clients хранятся в одной map.

`job_output` получает job только по ID и возвращает stdout/stderr, command и
working directory (`internal/agent/tools/job_output.go:45-120`). `job_kill`
так же получает и останавливает job только по ID
(`internal/agent/tools/job_kill.go:33-57`). Ни один путь не проверяет client,
session или tenant owner.

Особенно опасна комбинация с ephemeral library mode: `job_output` и
`job_kill` присутствуют в общем tool catalog, отсутствуют и в
`sdk.libraryEphemeralDisabledTools`, и в
`noRealWorkspaceForbiddenTools` (`sdk/library_mode.go:209-215`,
`internal/agent/coordinator_tools.go:426-450`). Поэтому полностью ephemeral
tenant, который сам не может запустить `bash`, всё равно получает инструменты
чтения/остановки чужого process-wide job. Угадывание `001` достаточно, чтобы
stdout со сборочными логами, путями или секретами попал в запрос внешнему
provider.

Вторая сторона того же ownership-дефекта: каждый `App.Shutdown` вызывает
`shell.GetBackgroundShellManager().KillAll` (`internal/app/app_lifecycle.go:212-215`).
Следовательно, `Client.Close()` одного поддерживаемого library client
останавливает jobs другого живого client. `KillAll` к тому же не синхронизирован
с `Start` через `startMu`: job другого клиента, вставленный между snapshot и
`Reset`, может потеряться из registry без cancel; вставленный после snapshot
останется жив после чужого `Close` (`internal/shell/background.go:542-558`).

Это нельзя исправить только случайными ID: manager должен принадлежать App/
Client и передаваться в bash/job tools; внутри одного multi-tenant Client job
нужен также session/tenant owner, проверяемый на output/kill. `Close` должен
останавливать только jobs владельца. Нужны regressions как минимум для двух
clients и двух session IDs: чужой output/kill отказан, закрытие A не влияет на B,
concurrent close/start не оставляет untracked process.

### R16-2 — P1: concurrent ephemeral `Open` имеет data race в глобальном goose state

Каждый `openMemoryDB` вызывает `goose.SetDialect("sqlite3")`, затем
`goose.UpContext` (`sdk/library_mode.go:562-574`). В используемом
`github.com/pressly/goose/v3 v3.27.1` `SetDialect` без mutex присваивает
package-global interface `store` (`dialect.go:38-73`); migration path затем
читает этот же global `store`.

Два одновременных `sdk.Open(...ModeLibrary...)` делают concurrent writes к
этому interface и могут одновременно читать его во время migration. Это
формальная data race; одинаковый SQLite dialect не делает несинхронизированный
interface assignment допустимым и не исключает torn observation/panic.

Публичная документация прямо поддерживает несколько simultaneous library-mode
clients (`sdk/sdk.go:21-29`, `sdk/README.md:510-516`). Однако headline test
`TestOpenLibraryMode_TwoEphemeralClientsAreIsolated` открывает A, а затем B
последовательно (`sdk/library_mode_test.go:267-281`), проверяя одновременную
жизнь, но не concurrent construction, поэтому race detector этот путь не видит.

Требуется единая инициализация goose на уровне `internal/db`, разделяемая file
и memory paths, либо отказ от legacy globals в пользу per-instance
`goose.Provider`. Нужен barrier-based test, который действительно вызывает два
`Open` одновременно под `-race`.

### R16-3 — P1: SDK subscriptions не закрываются на `Client.Close` и удерживают память

`pubsub.Broker.Subscribe` создаёт buffered channel на 4096 events и goroutine,
которая ждёт только `ctx.Done()` (`internal/pubsub/broker.go:114-147`). Корректный
`Broker.Shutdown` существует и закрывает все subscriber channels
(`internal/pubsub/broker.go:86-112`), но `App.releaseResources` не вызывает его
ни для `app.Messages`, ни для `app.Sessions`; cleanup list содержит только
глобальный `mcp.Close` (`internal/app/app.go:261-283`,
`internal/app/app_lifecycle.go:209-227`). Service interfaces экспортируют только
`pubsub.Subscriber`, поэтому shutdown даже не входит в их контракт
(`internal/message/message.go:55-64`, `internal/session/session.go:115-127`).

Если host делает естественный `SubscribeMessages(context.Background())` или
`SubscribeSessions(context.Background())`, затем закрывает Client, канал не
закрывается, goroutine остаётся навсегда, а непрочитанный буфер может удерживать
до 4096 больших message/session payloads. Consumer, ожидающий EOF, зависает.
Это также противоречит doc comment, где сказано, что подписка «stops receiving
events once the App has shut down» (`sdk/sdk.go:595-599`). Фактически она лишь
перестаёт получать новые публикации, но остаётся открытой.

Требуется App-owned shutdown обоих brokers до освобождения DB, с тестом
`subscribe(background) -> Close -> channel closed` и проверкой отсутствия
оставшегося subscriber/goroutine.

### R16-4 — P1: advertised concurrent runs гоняются на Options-level writers

SDK обещает concurrent `RunWithCredentials` на одном Client и concurrent runs
разных sessions (`sdk/sdk.go:520-549`, `sdk/README.md:121-158`). Одновременно
`Options.Stdout`/`Stderr` документированы как defaults для каждого последующего
Run (`sdk/sdk.go:270-278`). `Client.Run` и `RunWithCredentials` просто копируют
одни и те же interface values в каждый request (`sdk/sdk.go:498-507`,
`sdk/sdk.go:561-570`), после чего независимые `ExecuteRun` многократно вызывают
`fmt.Fprint/Fprintf` на них без общей синхронизации
(`internal/app/app_run.go:1233-1243`, `1504-1561`).

Обычный и уже используемый в SDK tests `*bytes.Buffer` не concurrency-safe.
Если host задаёт такой Options-level writer и запускает два документированно
разрешённых turns разных sessions, получает data race, возможную порчу buffer и
неразделимый interleaved output.

SDK должен либо завернуть Options-level defaults в один mutex-protected writer
на Client (и честно описать interleaving), либо явно потребовать concurrency-safe
writer и рекомендовать отдельные request-level writers. Нужен `-race` test с
двумя одновременно пишущими runs и общим default buffer. Request-level writer,
который caller сам повторно передал в несколько calls, может оставаться
ответственностью caller; SDK-owned автоматическое переиспользование default —
неявная часть собственного concurrent contract.

### R16-5 — P1: MCP initialize/close не имеют одного владельца и handshake

`app.New` запускает `go mcp.Initialize(...)` и возвращает App до завершения
инициализации (`internal/app/app.go:245-259`). Shutdown сразу запускает
`mcp.Close`, который делает snapshot текущей process-wide `sessions` map,
закрывает увиденные sessions и навсегда shutdown'ит глобальный broker
(`internal/agent/tools/mcp/init.go:56-62`, `140-163`). Он не ждёт конкретный
initialize этого App и не очищает process-wide sessions/tools/states maps.

Если `Close` происходит во время медленного MCP startup, `initClient` может
создать session после snapshot и затем записать её в global registry
(`internal/agent/tools/mcp/init.go:247-283`). Такой subprocess/transport уже не
попадёт в завершившийся cleanup и останется жить после `Client.Close`.

Process-wide ownership создаёт и cross-client эффект: даже library-mode App с
пустым MCP config регистрирует тот же глобальный `mcp.Close` в cleanup. Закрытие
такого library client закрывает MCP sessions единственного допустимого
application-mode client и его общий broker. Документированное ограничение
запрещает два application clients, но не запрещает один application client
вместе с library clients; текущая формулировка поэтому неполна.

Требуется либо per-App MCP registry, либо явный process owner/reference count и
initialize/close barrier. Минимальные regressions: immediate close во время
заблокированного startup не оставляет session/process; close library client не
трогает application owner's MCP; после полного close нет stale tools/states и
новый допустимый lifecycle не получает навсегда закрытый broker.

### R16-6 — P1: R15-3 остаётся открытым — non-comparable `DiskProvider` паникует

Публичный `sdk.DiskProvider` не требует comparable dynamic type. Реализация
value-type с `map` или `slice` и value receiver methods законно удовлетворяет
interface, но любое сравнение такого interface через `==`/`!=` вызывает
`panic: runtime error: comparing uncomparable type`.

Три достигаемых сравнения остались без изменений:

- ранняя ephemeral precondition — `internal/app/app_run.go:660-662`;
- финальный no-real-workspace tool floor —
  `internal/agent/coordinator_tools.go:462-466`;
- sentinel backstop —
  `internal/agent/tools/fs_library_virtual_root.go:72-74`.

Первое происходит до agent goroutine и его panic recovery, поэтому panic
выходит прямо из `Client.Run` в host process. Нужен закрытый type marker/
predicate для canonical OS provider без interface equality и regression с
value-provider, содержащим map.

### Correction note (post-review, 2026-09-05)

R16-6 is disconfirmed by the independent 36h review, section 4
(`docs/reviews/2026-09-05-2047-commit-review-36h.md`). This finding and the
R15-3 text it carries forward are retained as historical review text, not
silently rewritten. The former comparisons against `nil` or `OSDisk()` could
not panic for an external provider: `nil` comparison is safe, while a custom
provider has a distinct dynamic type from the comparable, unexported
`osDisk` value. The marker predicate is useful for explicit canonical
identity, but the specific threat model in R15-3/R16-6 was unreachable.

### R16-7 — P2: `Close` zero-value Client не выполняет собственный closed contract

`Client` — экспортируемый struct, поэтому внешний consumer может иметь его
zero value. `Close` специально обрабатывает `c.app == nil`, но ставит только
`closed=true`, не `closing=true` (`sdk/sdk.go:754-767`). `admit` проверяет только
`closing` (`sdk/sdk.go:807-814`). Поэтому после успешного `var c sdk.Client;
c.Close()` следующий `c.Run(...)` будет admitted и упадёт на nil `c.app`, хотя
документация обещает `ErrClientClosed` после начала/завершения Close.

Либо zero-value Client нужно явно объявить недопустимым и убрать создающую
обратное впечатление special case, либо nil-App branch должен перевести тот же
admission state machine в closing/closed и все методы должны безопасно вернуть
closed result.

## Общий обзор SDK

Сильные стороны текущей архитектуры:

- публичный пакет остаётся тонким facade над `internal/app`, а type aliases не
  создают дрейф wire/result contracts;
- `ModeLibrary` действительно отделяет явную provider/model конфигурацию от
  `rush.json`, а unique in-memory DSN изолирует данные последовательно открытых
  ephemeral clients;
- per-call credentials и immutable `CallOptions` убрали большую часть прежнего
  shared coordinator state и дают хорошую основу для concurrent turns;
- FolderScope + DiskProvider имеют ранние fail-closed проверки и защиту от
  durable replay на real disk;
- admission/drain/forced-close state machine существенно лучше обычного
  «closed bool»: ресурсы не освобождаются до cooperative drain, а DB остаётся
  открытой под неподчинившимися writers;
- R15 fixes показывают хороший layered подход: initial policy, финальный floor
  и behavioral provider-schema test.

Главный архитектурный разрыв: Client уже оформлен как владелец DB, coordinator
и admission lifecycle, но несколько старых подсистем всё ещё считают владельцем
весь процесс (`shell` jobs, MCP, goose globals, optional logging). Для CLI
«один процесс — один App» это было приемлемо; для SDK это становится
cross-client authority, гонками и утечками. Следующий этап должен не расширять
denylist, а провести ownership inventory всех process globals и разделить их на:

1. immutable/process-safe infrastructure;
2. явно документированные host-owned singletons;
3. App/Client-owned runtime resources, которые обязаны быть injected и закрыты
   только своим владельцем.

До закрытия R16-1, R16-2, R16-3, R16-4, R16-5 и R16-6 формулировки о строгой
изоляции и полной concurrent safety следует считать сильнее фактических
гарантий реализации.

## Рекомендуемый порядок исправлений

1. R16-1: вынести background manager из singleton, добавить owner checks и
   per-owner shutdown.
2. R16-6: убрать все сравнения произвольного `DiskProvider` interface.
3. R16-2: централизовать goose migration state или перейти на per-instance API.
4. R16-3: закрывать message/session brokers как часть App lifecycle.
5. R16-5: связать MCP initialization с owner lifecycle и дождаться его перед
   cleanup; отделить library clients от чужого process-global MCP.
6. R16-4: определить и реализовать concurrency contract для default writers.
7. R16-7: согласовать zero-value behavior с документацией.

После исправлений нужен отдельный race/leak gate с реально одновременными
`Open`, `RunWithCredentials`, `Subscribe/Close`, `MCP Initialize/Close` и
background job start/output/kill/close сценариями. Текущие sequential
«два клиента одновременно живы» tests не заменяют concurrent construction и
cross-owner lifecycle tests.
