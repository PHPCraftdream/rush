# Ревью последних исправлений и общий аудит SDK — раунд 17 (1433)

Метка запроса: 2026-09-04, round 17 — 1433.

Проверенный source snapshot основного репозитория:
`5f2dbebca4220bdb3d4d8e2944e90d2aa3b73f24`
(`fix: own MCP lifecycle per application`).

Основной диапазон: `041bbaed..5f2dbebc`. В нём семь production fix-коммитов,
43 изменённых файла, 1627 добавленных и 165 удалённых строк:

- `a7f657b2` — `fix: isolate goose migrations per database provider`;
- `75ccf190` — `fix: handle non-comparable disk providers`;
- `07cd3184` — `fix: close SDK subscriptions with their client`;
- `ee09c205` — `fix: close zero-value SDK clients safely`;
- `93a616b0` — `fix: synchronize SDK default writers`;
- `0b25848c` — `fix: isolate background jobs by client and session`;
- `5f2dbebc` — `fix: own MCP lifecycle per application`.

Метод: read-only анализ diff'ов, production-кода, тестовых oracle, публичного
SDK-контракта и lifecycle ресурсов; targeted race tests; полный тестовый прогон
с ограниченным параллелизмом. Production/test-файлы в ходе ревью не менялись.
Попытка независимого review через `hs`-агента выполнялась в отдельном detached
worktree, но сервис завершил агента по usage limit до выдачи результата. Поэтому
ни одна находка ниже не приписывается агенту: это локально проверенные выводы.

## Итоговый вердикт

Исправления goose, non-comparable `DiskProvider`, zero-value `Client` и
сериализации default writers выполнены полно. Public message/session
subscriptions теперь корректно закрываются вместе с клиентом.

Background ownership и MCP ownership закрыли исходные межклиентские дефекты,
но оба решения неполны по lifecycle. В MCP reconnect остаётся путь создания
сессии мимо close barrier, cancellation streaming HTTP body отсоединяется после
получения headers, а `Owner.Close` может навсегда оставить process-wide owner в
состоянии `closing`. Background manager удерживает уже удалённые foreground jobs
через retention timers и не выполняет глобальную очистку истёкших записей других
session.

Открыто: **5 P1 и 4 P2; P0 и P3 не обнаружены**. P1 блокируют release с
гарантиями leak-free lifecycle и надёжного CI gate. После исправлений нужен новый
полный review-цикл до отсутствия подтверждённых P0–P3.

## Проверка семи коммитов

| Коммит | Статус | Оценка |
| --- | --- | --- |
| `a7f657b2` | Закрыт | Migration state переведён на per-provider API; concurrent file/memory `Open` покрыты настоящим barrier-based тестом. Production-вызовов legacy `goose.SetDialect` не осталось. |
| `75ccf190` | Закрыт | Небезопасные interface equality для `DiskProvider` удалены; non-comparable реализации проходят через безопасную классификацию. |
| `07cd3184` | Частично | `Messages`/`Sessions` brokers закрываются корректно, но остальные App-owned brokers не входят в shutdown, см. R17-6. Комментарий публичного метода устарел, см. R17-8. |
| `ee09c205` | Закрыт | Zero-value `Client.Close` и вызовы после закрытия согласованы с заявленным узким контрактом «zero value safe to close». |
| `93a616b0` | Закрыт | Запись в общие default writers сериализована на уровне каждого `Write`; это соответствует документированному контракту и устраняет race/interleaving. |
| `0b25848c` | Частично | Межклиентский и межсессионный доступ к jobs закрыт, `Close` ограничен manager конкретного App. Retention/cleanup остаются неполными, см. R17-4. |
| `5f2dbebc` | Частично | Явный App owner устраняет уничтожение MCP ресурсов чужим клиентом, но reconnect и HTTP body не подчинены полному lifecycle, а тестовый suite теперь загрязняется незакрытым owner, см. R17-1, R17-2, R17-3 и R17-5. |

## Находки

### R17-1 — P1: MCP reconnect создаёт сессию вне owner close barrier

`internal/agent/tools/mcp/init.go:678-703` (`getOrRenewClient`) после неудачного
ping вызывает `createSession`, затем `updateState` и `sessions.Set`. В отличие от
initialization path около `internal/agent/tools/mcp/init.go:454`, renewal не
вызывает `owner.beginInit/endInit`, не проверяет `closing` и не подтверждает
поколение owner перед публикацией сессии.

Одновременно `Owner.close` (`internal/agent/tools/mcp/init.go:175-215`) переводит
owner в `closing`, отменяет init context, ждёт только `initWG`, снимает snapshot
текущих sessions, закрывает его и очищает registry. Renewal может завершить
`createSession` после snapshot и вставить новую сессию. Последующий reset удалит
её из registry без `Close`: transport/subprocess утечёт и переживёт App lifecycle.
Путь достижим из tools, prompts и resources.

Исправление: включить каждую renewal operation в тот же owner barrier, привязать
создание к owner context/generation и перед `Set` атомарно подтвердить, что owner
не closing. Если проверка не проходит, только что созданная сессия должна быть
закрыта. Нужен regression `Close` против заблокированного renewal.

### R17-2 — P1: owner cancellation отвязывается от streaming HTTP response после headers

`headerRoundTripper.RoundTrip` (`internal/agent/tools/mcp/init.go:905-915`)
регистрирует `context.AfterFunc(rt.ctx, cancel)`, но делает `defer stop()` в самом
`RoundTrip`. По контракту `http.RoundTripper` возвращает управление после
получения response headers, когда SSE/streamable body ещё может жить долго.
Именно тогда bridge owner cancellation уже удаляется; derived cancel также не
владеет lifetime body.

В результате `App.Close` не гарантирует отмену открытого HTTP/SSE body через
добавленный owner transport. Исправление: оборачивать `resp.Body` и держать
`stop/cancel` до EOF или `Body.Close`; при ошибке `RoundTrip` освобождать их
немедленно. Regression должен вернуть headers, оставить body открытым, закрыть
owner и увидеть cancellation на server/request side.

### R17-3 — P1: deadline `Owner.Close` игнорируется, process-wide owner может зависнуть навсегда

`Owner.close` явно не использует переданный `ctx`
(`internal/agent/tools/mcp/init.go:175-193`), без ограничения ждёт `initWG`, затем
последовательно вызывает `ClientSession.Close` (`:193-207`). Внешний
`App.releaseResources` ограничивает ожидание cleanup десятью секундами
(`internal/app/app_lifecycle.go:194-258`), но по таймауту возвращает только
вызывающий goroutine. Сам cleanup goroutine может остаться навсегда, а owner — в
состоянии `closing`; все будущие App/SDK clients в процессе будут отклонены.

Исправление должно одновременно сохранять lifecycle fence из R16 и делать
transport close/ожидание ограниченными context deadline. Нельзя просто сбросить
owner по timeout: поздняя init/renewal снова пересечёт границу следующего App.
Нужен тест с некооперативным session close и последующей возможностью безопасно
создать новый owner без поздней публикации старой сессии.

### R17-4 — P1: completed background jobs удерживаются после удаления и не очищаются между sessions

Completion path всегда ставит `time.AfterFunc(bufferRetention, closure)` с
захватом всего `BackgroundShell` (`internal/shell/background.go:448-480`). При
foreground completion bash сразу вызывает `RemoveOwned`
(`internal/agent/tools/bash.go:293,414`), но `RemoveOwned`
(`internal/shell/background.go:521-531`) только удаляет map entry. Timer ещё 15
минут удерживает объект и до 6 MiB output buffers, хотя job уже невозможно
запросить. При интенсивном SDK workload это создаёт большой временный heap.

Кроме того, bash теперь вызывает только `CleanupOwned(sessionID)`
(`internal/agent/tools/bash.go:274,340`). Metadata retention заявлена как восемь
часов, но завершённая session A, больше не запускающая bash, никогда не будет
очищена активностью session B; записи живут до `App.Close`.

Исправление: хранить timer в `BackgroundShell`, останавливать его и немедленно
release buffers при `Remove/RemoveOwned`; housekeeping manager должен глобально
удалять истёкшие metadata, тогда как `Get/Kill/Remove` обязаны сохранить owner
authorization. Нужны тесты на немедленное освобождение удалённого foreground job
и на удаление истёкшей записи неактивной session при активности другой.

### R17-5 — P1: новый MCP owner ломает полный `internal/app` test suite

`TestAppNew_DefaultPath_StillRunsRecoveryAndInitCoderAgent` создаёт полноценный
App (`internal/app/app_new_skip_agent_setup_test.go:184-185`), но cleanup
(`:186-193`) останавливает только `RunQueuePump` и вручную освобождает DB refs.
`application.Shutdown()` не вызывается, поэтому owner из `5f2dbebc` остаётся
process-wide активным.

В свежем процессе `go test -p=1 -count=1 ./internal/app` после этого теста
стабильно даёт cascade `failed to acquire MCP application owner: mcp:
application owner is already active`; temp SQLite files также остаются открыты.
Полный suite больше не является рабочим release gate и маскирует последующие
регрессии.

Исправление локально тестовое: cleanup должен вызывать полный App shutdown и не
дублировать DB release. Нужен небольшой последовательный regression, создающий,
закрывающий и снова создающий App в одном process.

### R17-6 — P2: App shutdown закрывает не все принадлежащие App brokers

`internal/app/app_lifecycle.go:152-161` вызывает `Shutdown` только для
`Messages` и `Sessions`. App также создаёт `History` broker
(`internal/history/file.go:30,45,52`), permission request/notification brokers
(`internal/permission/permission.go:227-229,726-727`), `agentNotifications` и
`events` (`internal/app/app.go:121-122,244-245`). Некоторые доступны подписчикам
через App accessors.

Подписка с `context.Background()` на эти brokers не завершается при App close и
удерживает goroutine/channel/payload до внешней отмены или GC недостижимого
цикла. Public `sdk.Client` пока выставляет только Messages/Sessions, поэтому это
ниже P1, но общий App lifecycle и будущий SDK surface остаются неполными.
Исправление: дать всем App-owned pubsub services единый idempotent `Shutdown` и
закрывать их до resource cleanup; покрыть каждый exposed subscription.

### R17-7 — P2: `pathLocks` бессрочно растёт в long-lived SDK host

`internal/db/connect.go:60-66,144` хранит `absPath -> *sync.Mutex` в package-wide
`sync.Map`; записи никогда не удаляются при `Release`, `ReleaseAll` или закрытии
последнего pool reference. Для CLI это практически один path за process, но SDK
явно поддерживает повторное открытие разных data directories в долгоживущем
host. Количество ключей теперь не ограничено архитектурой.

Нужен refcounted path-lock entry, удаляемый только после ухода последнего opener
и при отсутствии waiter, либо lock lifecycle, объединённый с pool entry. Тест
должен многократно открыть/закрыть уникальные paths и проверять возврат registry
к исходному размеру без гонки с concurrent `Connect`.

### R17-8 — P2: публичный комментарий subscriptions противоречит новой семантике

Комментарий `Client.SubscribeMessages` (`sdk/sdk.go:614-618`) всё ещё говорит,
что admitted subscription лишь перестаёт получать events после App shutdown.
После `07cd3184` App активно закрывает broker и channel. Это наблюдаемая часть
публичного SDK-контракта; документация и поведение должны совпадать. Нужно прямо
обещать закрытие channel при `Client.Close`, сохранив правило caller context.

### R17-9 — P2: Windows regression test принимает настоящий WSL launcher за «real bash»

`requireRealBash` (`internal/agent/cliprovider/provider_windows_test.go:56-71`)
использует plain `exec.LookPath("bash")` и не исключает системный WSL launcher.
На машине, где он первый в исходном PATH, helper возвращает именно его. После
подмены `SystemRoot` тест уже не распознаёт этот путь как launcher, добавляет его
как «real bash» и end-to-end test падает с `exit status 1`
(`provider_windows_test.go:149-186`). Это воспроизведено в последовательном
полном прогоне.

Production resolver при этом прошёл прямые unit tests; подтверждён дефект test
fixture, а не production path. Helper должен искать runnable non-WSL bash тем же
WSL-aware resolver или валидировать candidate до запуска теста; если Git
Bash/MSYS отсутствует, тест должен `Skip`.

## Race, leak и test evidence

Успешно:

```text
go test -race -count=1 ./internal/db ./internal/pubsub ./internal/shell ./internal/agent/tools/mcp ./sdk
ok internal/db                 34.341s
ok internal/pubsub              1.362s
ok internal/shell               3.645s
ok internal/agent/tools/mcp      3.433s
ok sdk                          83.206s
```

Первый `go test ./...` не дал корректного сигнала из-за исчерпания Windows
virtual memory при параллельной компиляции (`VirtualAlloc`, errno 1455). Повтор
с `GOMAXPROCS=2` и `-p=1` прошёл рассматриваемые core packages, но выявил R17-9
и затем R17-5. Изолированный `go test -p=1 -count=1 ./internal/app` подтвердил
R17-5 независимо от полного прогона.

Зелёный race detector не опровергает находки: R17-1/R17-3 — порядок lifecycle и
ресурсная публикация, R17-2 — lifetime response body, R17-4/R17-6/R17-7 —
достижимое удержание ресурсов. Текущие тесты не создают соответствующие
close-vs-renew, headers-vs-body и many-distinct-path сценарии.

## Общий обзор SDK

Сильные стороны текущего состояния:

- library mode изолирует in-memory DB, virtual filesystem и tool surface;
- concurrent `Open`, non-comparable providers, shared writers и public
  subscriptions теперь имеют прямые regressions;
- admission/Close и zero-value Close contracts существенно лучше формализованы;
- background jobs и MCP больше не управляются чужим App без owner checks.

Оставшиеся release blockers:

- MCP App mode пока не имеет доказанного bounded, leak-free shutdown во всех
  init/reconnect/streaming paths;
- background manager не соответствует долгоживущему высокочастотному SDK host
  по памяти и metadata retention;
- полный test gate сломан owner leak в fixture и не может считаться зелёным.

Library mode без MCP уже близок к пригодному состоянию, но заявлять общий SDK
готовым к долгоживущему concurrent host преждевременно до закрытия R17-1—R17-7
и повторного полного race/review цикла.

## Рекомендуемый порядок исправлений

1. R17-1, R17-2 и R17-3 как единый MCP lifecycle patch; они меняют один модуль
   и должны иметь согласованный barrier/body/deadline design.
2. R17-5, чтобы восстановить надёжный полный test gate.
3. R17-4 — background timers и cross-session housekeeping.
4. R17-6 и R17-8 — полный broker shutdown и точный public contract.
5. R17-7 — bounded DB path-lock registry.
6. R17-9 — детерминированный Windows fixture.
7. Новый независимый `hs` review и следующий HL fix cycle до отсутствия P0–P3.
