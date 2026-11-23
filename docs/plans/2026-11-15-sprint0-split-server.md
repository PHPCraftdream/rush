# Спринт 0, D1: дизайн разрезки `internal/server` (#86 / #74)

Дата: 2026-11-15. Статус: дизайн, кода нет. Исполнение — 0б-5 (#86) и D2 (#75).
Пути: целевые пути в этом документе — после этапа C (`internal/serve/server/...`); до C им взаимно-однозначно соответствуют пути `internal/server/...`.
Смежные дизайн-документы D1: `agent`, `session`, `config`, `cmd`, `mcp`, `app`. Типы результатов `app` (RunResult и т.п.) — нижний пакет полосы `app` (0б-5), здесь не проектируются; от server зависит только через `internal/app`.

## 1. Текущее состояние

### 1.1. Цифры (проверено листингом каталога, не из скрипта)

| Показатель | Значение |
|---|---|
| Рукописных непроизводных файлов | 84 = 82 в корне пакета + 2 в `r15bfixture/` |
| Строк в них | 17 069 (корень 16 798 + `r15bfixture` 271) |
| Тестовых файлов | 213, 37 883 строк; все `package server` (белые) |
| Сгенерированных файлов | 0 (маркера «Code generated» нет ни в одном файле) |
| `init()` в непроизводных файлах | 0 (prereq.py: «init(): 0»; grep `^func init` пуст) |
| OS-файлов (`_windows`/`_unix`/…) | 0 (prereq.py: «os-specific prod files: 0») |
| `go:embed` | 0 (prereq.py: «go:embed: 0 []») |
| Файлы >800 строк (код) | `hub.go` 992, `protocol.go` 841 (предел 1000, цель ≤800) |
| Файлы >800 строк (тесты) | `p655_rerun_idset_regression_test.go` 863 |

`TestMain` один, в `main_test.go:12` (`testguard.Main(m)`) — изоляция глобального конфига для тестов, доходящих до `config.Init`/`app.New`.
Встроенный dist веб-клиента живёт НЕ здесь: `web/embed.go` (`//go:embed all:dist`, `web.FS()`); `internal/cmd/root.go:273` передаёт `rushweb.FS()` в `server.NewWithCleartext` как `fs.FS` — server встраивания не содержит и после разрезки содержать не будет.

### 1.2. Кластеры (analyze.py clusters internal/server 60; prod/test в строках, c90 — коммиты за 90 дней)

| Кластер | файлов | prod | test | c90 | Проверка чтением |
|---|---|---|---|---|---|
| api | 98 | 5089 | 9803 | 26 | REST-каркас, run/batch/permissions/events-SSE, mux, openapi, idempotency |
| handlers | 43 | 5816 | 4999 | 102 | WS-команды по ресурсам (самая горячая часть) |
| apiauth | 15 | 438 | 2901 | 7 | Principal, Chain, аудит-обвязка |
| hub | 7 | 1195 | 852 | 19 | Hub/Client/dispatch, pin-фильтр |
| ws | 13 | 294 | 1375 | 3 | ws_serve_*: origin, scope, revoke, subprotocol |
| tls | 8 | 517 | 820 | 1 | tls_*: store, reload, self-signed, pins, mtls |
| listener | 10 | 461 | 821 | 6 | ListenerPolicy, pins, валидация |
| protocol | 3 | 942 | 0 | 26 | protocol.go 841 + custom_roles 40 + peak_hours 61 — лист-протокол |
| finalization | 4 | 236 | 678 | 3 | помпа удержания терминальных событий |
| server | 3 | 665 | 205 | 19 | server.go (компоновщик) |
| events | 3 | 252 | 235 | 7 | events.go — мост app pubsub → Hub |
| wire | 1 | 216 | 0 | 8 | wire.go — проекция message → MessageWire |
| peak | 7 | 58 | 782 | 6 | peak_hours_* |
| ratelimit | 2 | 216 | 117 | 2 | ratelimit_core.go |
| auth | 4 | 143 | 177 | 5 | auth.go — cookie-аутентификация UI |
| scope | 2 | 107 | 120 | 3 | scope_table.go — таблица команд→скоупы |
| update | 1 | 80 | 0 | 26 | update_notice.go |
| остальные | ~40 | ~150 | ~9600 | — | тесты по номерам задач (p*, task*, descendant*) |

Всего кластеров 68. Кластер `protocol` кодово чист (см. §1.3).

### 1.3. Сцепления (entangle.py internal/server 12 300, идентификаторы между кластерами)

Исходящие: handlers→protocol:167, api→protocol:49, hub→protocol:29 (payload-типы — все 67 структур в protocol*.go); api→apiauth:10 (Chain, ChainConfig, AuthConfig, Principal…); hub→handlers:4; server→{ws:8, listener:8, hub:8, tls:7, protocol:5}; apiauth→api:6; tls→apiauth:3; listener→api:3.
Входящие: protocol←{handlers:167, api:49, hub:29, events:10, ws:5, server:5, update:2, peak:2, wire:1}.

Проверка скрипта чтением кода (важно — скрипт приближает):
- «protocol → handlers:3, protocol → server:1» — вхождения в КОММЕНТАРИЯХ (`protocol.go:55,58,106,110,113`); импорты `protocol.go`/`protocol_custom_roles.go` — только `encoding/json`, `protocol_peak_hours.go` — без импортов. Кластер `protocol` — кодовый лист.
- «handlers → core:171» и «api → core:59» — по большей части вызовы методов внедрённого `*app.App` (не рёбра импорта) и локальные замыкания (пример: `run := func(...)` — локальная переменная в `handlers_models.go:93`, а не межкластерная ссылка).
- Реальные циклы перечислены в §1.4.

### 1.4. Циклы (prereq.py на наивном разрезе ['api','auth','core','handlers','hub','transport']: цикл api ↔ auth ↔ core ↔ handlers ↔ hub ↔ transport)

Существенные циклы (каждый проверен на конкретных строках):
1. **api ↔ apiauth.** `apiauth_chain.go:118 Chain` вызывает `auditMiddleware` (`api_audit_middleware.go:63`), `setAuditPrincipal` (`:37`), `setAuditToken` (`:29`) и константу `routeBatch` (`api_batch_only.go:8`); при этом api строит Chain (`api_serve.go:30`) и использует `writeProblem`/`retryResponse` (`api_response.go`). Цикл: аудито-ответная обвязка лежит в api, а нужна apiauth.
2. **hub ↔ handlers.** `hub.go:381` — поле `Hub.liveWorkTitles *titleCache`, тип объявлен в `handlers_livework.go:96`; handlers читают `c.hub.liveWorkTitles` (`handlers_livework.go:54,293`). Замыкание: кэш — состояние хаба, а тип живёт в обработчиках.
3. **finalization ↔ api.** `finalization_notify.go:182,203` (`FinalizationGatedAPIEventStream`, `finalizationAPIEventTerminal`) используют `APIEvent` (`api_events.go:15`) и `terminalAPIRunState`/`APIRunState` (`api_runs.go:24,189`); при этом помпу вызывают `api_events_handler.go:57`, `api_events_feeder.go:40`, `events.go:59`. Цикл через wire-типы, застрявшие в api.
4. **api ↔ семьи маршрутов.** `api_routes.go:23 APIRoutes` собирает `apiBatchRoutePlaceholders`, `apiRunRoutesWithPlaceholders`, `apiPermissionRouteDeclarations`; сборщик `apiRoutesWithBroker` (`api_read_handlers.go:92`) создаёт и batch-сервис. Каркас (тип `APIRoute`, mux, ответы) нужен семьям, а семьи нужны сборщику.
5. **dispatch ↔ семьи.** `handlers.go:29 handleIncoming` адресно вызывает ~60 `handleX` всех ресурсных семей и живёт в том же пакете, что и все семьи.
6. **auth ↔ ws.** `apiauth_chain.go:232,242,251` используют `websocketUpgrade`/`offeredSubprotocol`/`hasBearerSubprotocol` (`ws_serve_auth.go`); тот же файл использует `server.go:306` (компоновщик).
Глубокое ребро без цикла: server → transport 23 (ListenerPolicy, DenyAllAuth, IsLoopbackHost, authRecheckInterval…) — сборка.

### 1.5. Глобалы и тестовые швы

Пакетных var в непроизводных файлах — 40+; все перечислены и распределены в §4.1. Здесь — только межпакетные тестовые переопределения (prereq.py: «package-level vars used across proposed boundaries: 10»):
- `serveAuthTickerFactory`, `serveAuthSlotReleased` (`apiauth_chain.go:113-117`) переопределяют тесты двух будущих пакетов: hub (`ws_serve_revoke_test.go`, `ws_serve_slots_test.go`) и server/ (root: `api_events_revoke_integration_test.go`, `api_events_revoke_resume_test.go`). Шов: у `ChainConfig` уже есть поля `TickerFactory`/`SlotReleased` (`apiauth_chain.go:76-86`) — var-фолбэки удаляются, тесты передают явно (§3, S2).
- `rerunPreHandoffSeam` переопределяет `p623_panic_window_test.go` — subject файла это rerun (паника внутри rerun-обработчика), поэтому файл едет в wsapi, а не в hub.
- Прочие швы переопределяются только тестами своего будущего пакета: `batchSourceFor`, `batchAddForWeb`, `batchPendingQuestionsFor` → wswork; `rerunPostTruncateSeam`/`rerunPostIdlePollSeam`/`rerunHoldingReservationSeam`/`rerunPreHandoffSeam`/`batchRerunReconcile` → wsapi; `serveAPIRoutes`/`serveAPINow` → server/; `testBeforeWrite` → hub.

### 1.6. Тестовые помощники, общие между будущими пакетами (prereq.py: «80 ids in 21 directions»; проверка grep'ом)

| Помощник | Определён | Используют |
|---|---|---|
| `startPolicyServer` | serve_listener_test.go:14 | 27 файлов: hub/ws/api/batch/listener/root |
| `newServeWSScopeFixture`+`exchange`/`sendCommand`/`awaitReply` | ws_serve_scope_fixture_test.go:25 | 6 файлов (hub, wsconfig) |
| `newAttachmentsTestApp` | handlers_test.go:95 | ~20 файлов всех семей (фабрика `*app.App` на временных каталогах) |
| `dialTestWS` | readpump_control_starvation_test.go | root (`handlers_shutdown_test`) + hub |
| `panicTimeline` | listener_failure_report_test.go | root + hub (`panic_timeline`); `p623_*` — wsapi (§2.5) |
| `describeTempTree`/`logTempTreeBeforeRemoval` | tempdir_diag_test.go | root + apiauth (`apiauth_chain_test`) |
| `setIsolatedGlobalPaths` | serve_token_log_test.go | 5 файлов: 4 root + `shutdown_ack` → hub |

Вывод: нужен тест-помощник пакет `servertest` (§4.5).

### 1.7. Внешние потребители

- `internal/cmd` — единственный импортер: `root.go:269,273` (`IsLoopbackHost`, `NewWithCleartext`); `serve.go` — 14 идентификаторов (полный список §2.3); тесты cmd: `server.ServeAPI` (`serve_api_shutdown_order_test.go:37`), `server.Listen` (`web_cleartext_command_test.go:58-70`, `web_cleartext_execute_test.go:32`).
- `sdk` — импорта НЕТ (проверено grep'ом импорта; `sdk/sdk.go:181-186` — комментарии: server «re-shapes» сырые `message.Message`/`session.Session`). Контракт sdk ↔ server идёт через типы `internal/app` (чужая полоса 0б-5 app). Лист-протокол — контракт с веб-клиентом `web/src` (TS-зеркала: `types.ts`, `customRoleTypes.ts`, `batchWork.ts`).
- `r15bfixture` (`package main`, 2 файла, 271 строка) — ручной стенд r15b: импортирует server, из репозитория не вызывается (упомянут только в `docs/plans/2026-11-15-per-tree-store-plan.md`). Остаётся без изменений под `server/r15bfixture/`.

## 2. Целевое дерево

### 2.1. Дерево и счёт элементов (правило ≤7)

```
internal/serve/server/          7 элементов: только подкаталоги
├── server/       package server   — компоновщик;      6 = 5 файлов + r15bfixture/
├── wire/         package wire     — лист-протокол;    6 = 5 файлов + finalgate/
│   └── finalgate/ package finalgate — помпа удержания; 1 файл
├── hub/          package hub      — WS-транспорт;     6 файлов
├── apiauth/      package apiauth  — REST/WS-аутентификация; 6 файлов
├── listener/     package listener — политика слушателей + TLS; 5 файлов
├── ws/           3 элемента (группировка без кода, как internal/ после C)
│   ├── wsapi/    package wsapi    — WS-команды диалога;  4 файла
│   ├── wswork/   package wswork   — WS-команды работы;   3 файла
│   └── wsconfig/ package wsconfig — WS-команды конфигурации; 4 файла
└── rest/         3 элемента
    ├── api/      package api      — REST-каркас+read+run+events+permissions; 7 файлов
    ├── apibatch/ package apibatch — REST batch; 2 файла
    └── servertest/ package servertest — тест-помощники; 3 файла
```

Состав файлов каждого каталога и все 84 файла — в §2.4; строки после разрезки: каждый рукописный файл ≤800 (лимит 1000), крупнейшие: `wswork/livework.go` ~700, `wswork/batch.go` ~730, `wsapi/rerun.go` 598, `api/runhandlers.go` ~586, `hub/client.go` ~440, `hub/hub.go` ~460, `apiauth/chain.go` ~525, `wsconfig/providers.go` 550, `wsconfig/models.go` 604, `api/routes.go` ~447.

Имена пакетов уникальны в репозитории (проверено `git grep -h '^package '`): `wire`, `hub`, `api`, `apibatch`, `apiauth`, `listener`, `wsapi`, `wswork`, `wsconfig`, `finalgate`, `servertest` — свободны; имя `protocol` ЗАНЯТО (`internal/tasktree/protocol`), поэтому лист назван `wire`; корень-компоновщик сохраняет имя `server` (принцип «последняя часть пути не меняется»: `internal/server` → `internal/serve/server`). Названий из запрещённых (`core`, `util`, `common`, `base`) нет.

### 2.2. Публичный API пакетов (экспортируемые имена, нужные другим пакетам)

- **server (компоновщик)**: `New`, `NewWithCleartext`, `NewWithPolicy`, `Server` (методы `Start`, `ServeListener`, `RequestShutdown`, `Token`, `TLSPins`), `Listen` (var-шов для тестов cmd), `ErrUnexpectedNonLoopbackBind`, `ServeAPI`, `NewServeAPI` + 16 алиасов совместимости (§2.3).
- **wire (лист)**: `WSMessage`; константы `Event*` (26) и `Cmd*` (60); payload-структуры (67, без переименований — JSON-контракт); `HelloPayload` (переименован из `helloPayload`), `MustJSONRaw` (из `mustJSON`); `APIEvent` (переехал из api_events.go), `APIRunState` с константами состояний и `TerminalAPIRunState` (из api_runs.go); проекции store→wire: `PartWire`, `UsageWire`, `MessageWire`, `ToMessagesWire`/`ToMessageWire`/`ToUsageWire` (переименование toXxxWire), `APISession`/`APIMessage`/`APIModel` + `APISessionFromModel`/`APIMessageFromModel` (из apiSessionFromModel/apiMessageFromModel), `APITimestamp` (из apiTimestamp); `ConfigWire`, `ModelEntryWire`, `ProviderWire`, `ModelInfoWire`, `MCPSnapshot`, `MCPServerInfo`, `LSPSnapshot`, `PeakHoursWirePayload`, `PeakHoursWindowWire`, типы ролей из protocol_custom_roles.go.
- **finalgate**: `FinalizationGatedStream`, `FinalizationGatedMessageStream`, `FinalizationGatedSessionStream`, `FinalizationGatedAPIEventStream`, `GateFor` (новый; §3 S3). 5 имён.
- **hub**: `Hub`, `NewHub`, `Client`; `Hub.Broadcast`, `BroadcastSticky`, `BroadcastConfig`, `WaitDelivery`, `TryRegister`, `LiveWorkTitles() *TitleCache`, `FilterSessionsForPinnedClient` (переименование filterSessionsForPinnedClient); `Client.Reply`, `ReplyAndWait`, `Dispatch`, `DispatchControl`, `AuthorizeIncoming` (новый; §3 S5); `Gate`, `GateOptions`, `NewGate` (новые; §3 S4); `ScopeFor`, `CommandPolicy`, `AuthorizeCommand`, `ErrCommandUnmapped`.
- **apiauth**: `Principal`, `PrincipalFrom`, `WithPrincipal`, `Chain`, `ChainConfig`, `AuthConfig`, `TokenVerifier`, `TokenFlusher`, `Ticker`, `FingerprintAuth` (переименование mtlsFingerprintAuth), `WriteAPIProblem`, `WriteAPIResponse`, `ParsePagination`, `WritePaginatedResponse`, `Pagination`, `RecordScope`, `RecordSession`, `Redact`, `RedactURL`, `RedactHeader`, `ResolvePinnedCommand`, `ResolvePinnedRoute`, `ResolveBatchOnlyRoute`, `ResolveBatchOnlyCommand`, `RouteFull`, `RouteBatch`.
- **listener**: `ListenerPolicy`, `ListenerSurface` + `SurfaceWS`/`SurfaceREST`/`SurfaceUI`, `ListenerTLS`, `ListenerLimits`, `ListenerPolicyError`, `DenyAllAuth`, `ValidateListenerSet`, `EffectiveListenerPins`, `ValidatePinnedPermissionModes`, `ValidatePinnedPermissionModesAcrossPolicies`, `PinnedTokenIDs`, `ListenerDeclaresPins`, `ValidateEffectivePins`, `EnsureSelfSignedCertificate`, `EnsureSelfSignedCertificateForListener`, `TLSPins`, `TLSPinsForCertificatePEM`, `IsLoopbackHost`, `OriginsAllowed` (переименование serveOriginAllowed), `ValidateAllowedOrigins` (из validateAllowedOrigins).
- **wsapi / wswork / wsconfig**: ровно по одному экспорту — `Dispatch(ctx, a *app.App, c *hub.Client, msg wire.WSMessage) bool` (§3 S5); внутри пакета обработчики остаются неэкспортированными. Единственное исключение: wsconfig вызывает `wsapi.AnnotateSessionExternalOwnership` (уже экспортирована).
- **api**: `APIRoute`, `APIParam`, `APIRoutes`, `NewAPIHandler`, `NewAPIHandlerSplit`, `BuildOpenAPIDocument`, `APIEventLog`, `NewAPIEventLog`, `APIRunRegistry`, `NewAPIRunRegistry`, `APIRun`, `APIRunBackend`, `RunOutcome`, `APIPermissionBroker`, `NewAPIPermissionBroker`, `APIPermissionRequest`, `StartEventFeeder` (переименование startAPIEventFeeder), ошибки `ErrAPIRunNotFound`, `ErrAPIRunBackendBusy`, `ErrAPIPermissionUnknown`.
- **apibatch**: `Routes` (конструктор таблицы маршрутов batch для app; склейка apiBatchRoutePlaceholders + service.apiBatchRoutes), ошибку `ErrBatchOverloaded`.
- **servertest**: `NewAppFixture` (из newAttachmentsTestApp), `StartServeFixture` (политика+Chain+REST-mux+WS-шлюз без `Server`), `DialWS`, `Exchange`, `SendCommand`, `AwaitReply`, `AwaitType`, `PanicTimeline` (из panicTimeline), `DescribeTempTree`/`LogTempTreeBeforeRemoval`.

### 2.3. Поверхность для `internal/cmd` — алиасы совместимости в server

cmd использует (проверено по `internal/cmd/root.go`, `serve.go`, тестам): `New`, `NewWithCleartext` (root.go:273), `IsLoopbackHost` (root.go:258,269), `NewWithPolicy`, `Server`, `ServeAPI`, `NewServeAPI`, `ValidateListenerSet` (serve.go:258), `EffectiveListenerPins` (serve.go:332), `ValidatePinnedPermissionModesAcrossPolicies` (serve.go:338), `EnsureSelfSignedCertificateForListener` (serve.go:362), `ValidateEffectivePins` (serve.go:381), `DenyAllAuth` (serve.go:232), `ListenerSurface`/`SurfaceUI` (serve.go:96,221,223,226), `ListenerPolicy`/`ListenerLimits`/`ListenerTLS` (serve.go:216,234,235), `Listen` (тесты cmd).

Чтобы cmd продолжал импортировать один путь, корень server объявляет 16 алиасов (type/var-алиасы — те же типы и значения, поведение не меняется): `ListenerPolicy = listener.ListenerPolicy`, `ListenerSurface` (+3 константы поверхностей), `ListenerTLS`, `ListenerLimits`, `ListenerPolicyError`, `DenyAllAuth = listener.DenyAllAuth`, `ValidateListenerSet`, `EffectiveListenerPins`, `ValidatePinnedPermissionModesAcrossPolicies`, `ValidateEffectivePins`, `EnsureSelfSignedCertificateForListener`, `TLSPins`, `IsLoopbackHost` — все из listener; `internal/cmd` не меняется вообще. sdk не меняется (импорта не было).

### 2.4. Таблица «файл → цель» (все 84 непроизводных файла; число строк = 84)

Формат: строки в текущем файле → цель; расщепления отмечены прямо в столбце «Цель». Межпакетных расщеплений восемь (строки 6, 16, 27, 39, 47, 71, 72, 80 таблицы — швы S1–S6 и полосы Л2/Л3, §3 и §5); ещё два расщепления внутри одного пакета (57, 64). Всё прочее — чистый перенос.

| # | Файл | Строк | Цель |
|---|---|---|---|
| 1 | api_audit_middleware.go | 141 | apiauth/audit.go |
| 2 | api_batch_handlers.go | 375 | apibatch/batch.go |
| 3 | api_batch_only.go | 50 | apiauth/resolution.go (+RouteFull/RouteBatch сюда) |
| 4 | api_batch_policy.go | 176 | apibatch/batch.go |
| 5 | api_batch_service.go | 576 | apibatch/service.go |
| 6 | api_events.go | 193 | api/events.go + wire/snapshots.go (APIEvent:15-20) |
| 7 | api_events_feeder.go | 84 | api/events.go |
| 8 | api_events_handler.go | 218 | api/events.go |
| 9 | api_events_revoke.go | 30 | api/events.go |
| 10 | api_events_route.go | 17 | api/events.go |
| 11 | api_idempotency.go | 245 | api/idempotency.go |
| 12 | api_mux.go | 135 | api/routes.go |
| 13 | api_openapi.go | 137 | api/routes.go |
| 14 | api_permissions.go | 434 | api/permissions.go |
| 15 | api_permissions_handlers.go | 65 | api/permissions.go |
| 16 | api_read_handlers.go | 264 | api/read.go + server/serve.go (apiRoutesWithBroker:92-107) |
| 17 | api_response.go | 111 | apiauth/response.go |
| 18 | api_routes.go | 38 | api/routes.go |
| 19 | api_routes_core.go | 23 | api/routes.go |
| 20 | api_routes_permissions.go | 14 | api/routes.go |
| 21 | api_routes_read.go | 47 | api/routes.go |
| 22 | api_routes_runs.go | 38 | api/routes.go |
| 23 | api_routes_sessions.go | 15 | api/routes.go |
| 24 | api_run_backend.go | 434 | api/runbackend.go |
| 25 | api_run_handlers.go | 338 | api/runhandlers.go |
| 26 | api_run_role.go | 12 | api/runbackend.go |
| 27 | api_runs.go | 278 | api/runhandlers.go + wire/snapshots.go (APIRunState:24-33, terminalAPIRunState:189-191) |
| 28 | api_serve.go | 182 | server/serve.go |
| 29 | api_serve_pins.go | 12 | server/serve.go |
| 30 | api_session_handlers.go | 191 | api/read.go |
| 31 | api_session_resolution.go | 133 | apiauth/resolution.go |
| 32 | api_types.go | 83 | wire/wire.go |
| 33 | apiauth_chain.go | 413 | apiauth/chain.go |
| 34 | apiauth_doc.go | 25 | apiauth/chain.go |
| 35 | auth.go | 143 | apiauth/ui_auth.go |
| 36 | events.go | 252 | server/events.go |
| 37 | finalization_notify.go | 218 | wire/finalgate/pump.go |
| 38 | finalization_wiring.go | 18 | wire/finalgate/pump.go (var удаляется, S3) |
| 39 | handlers.go | 239 | server/dispatch.go (каркас) + плечи переключателя в Dispatch семей (S5) |
| 40 | handlers_agent.go | 531 | wsapi/agent.go |
| 41 | handlers_agent_rerun.go | 598 | wsapi/rerun.go |
| 42 | handlers_batch.go | 539 | wswork/batch.go |
| 43 | handlers_batch_control.go | 177 | wswork/batch.go |
| 44 | handlers_batch_doc.go | 17 | wswork/batch.go |
| 45 | handlers_config.go | 462 | wsconfig/config.go |
| 46 | handlers_custom_roles.go | 163 | wsconfig/models.go |
| 47 | handlers_livework.go | 317 | wswork/livework.go + hub/titlecache.go (titleCache/newTitleCache:93-125) |
| 48 | handlers_livework_batch.go | 413 | wswork/livework.go |
| 49 | handlers_mcp.go | 166 | wsconfig/providers.go |
| 50 | handlers_messages.go | 456 | wsapi/messages.go |
| 51 | handlers_models.go | 441 | wsconfig/models.go |
| 52 | handlers_peak_hours.go | 257 | wsconfig/peakhours.go |
| 53 | handlers_providers.go | 384 | wsconfig/providers.go |
| 54 | handlers_sessions.go | 470 | wsapi/sessions.go |
| 55 | handlers_shutdown.go | 18 | server/dispatch.go |
| 56 | handlers_wake_schedules.go | 168 | wswork/wake.go |
| 57 | hub.go | 992 | hub/hub.go (Hub:369-776, 939-992) + hub/client.go (Client:90-368, reply/writePump:777-938) |
| 58 | hub_pin_filter.go | 180 | hub/pin.go |
| 59 | hub_pin_sessions.go | 23 | hub/pin.go |
| 60 | listener_pin_validation.go | 83 | listener/pins.go |
| 61 | listener_pins.go | 126 | listener/pins.go |
| 62 | listener_policy.go | 252 | listener/policy.go |
| 63 | peak_hours_wire_json.go | 58 | wire/peakhours.go |
| 64 | protocol.go | 841 | wire/protocol.go (1-732) + wire/snapshots.go (732-841) |
| 65 | protocol_custom_roles.go | 40 | wire/custom_roles.go |
| 66 | protocol_peak_hours.go | 61 | wire/peakhours.go |
| 67 | providers_peak_hours_write_fields.go | 12 | wsconfig/peakhours.go |
| 68 | ratelimit_core.go | 216 | apiauth/ratelimit.go |
| 69 | redact_secrets.go | 61 | apiauth/audit.go |
| 70 | scope_table.go | 107 | hub/scope.go |
| 71 | server.go | 665 | server/server.go + hub/gate.go (handleWS:504-572, readPump:574-645) |
| 72 | tls_mtls.go | 53 | listener/tls.go (loadClientCAPool:12-37; вызывается tls_server.go:128) + apiauth/chain.go (mtlsFingerprintAuth:39-53; вызывается api_serve.go:86) |
| 73 | tls_pins.go | 49 | listener/tls.go |
| 74 | tls_reload.go | 82 | listener/tls_reload.go |
| 75 | tls_selfsigned.go | 182 | listener/tls_selfsigned.go |
| 76 | tls_server.go | 151 | listener/tls.go |
| 77 | update_notice.go | 80 | server/update_notice.go |
| 78 | wire.go | 216 | wire/wire.go |
| 79 | ws_serve_auth.go | 34 | apiauth/chain.go |
| 80 | ws_serve_origin.go | 79 | listener/policy.go (serveOriginAllowed/validateAllowedOrigins:24-79) + wire/snapshots.go (helloPayload/mustJSON:11-21 → HelloPayload/MustJSONRaw) |
| 81 | ws_serve_revoke.go | 38 | hub/gate.go |
| 82 | ws_serve_scope.go | 143 | hub/scope.go |
| 83 | r15bfixture/main.go | 248 | server/r15bfixture/main.go (без изменений) |
| 84 | r15bfixture/provider.go | 23 | server/r15bfixture/provider.go (без изменений) |

Сумма строк столбца «Строк» = 17 069; число строк таблицы = 84 = числу непроизводных файлов (82 корневых + 2 в r15bfixture). Итоговые файлы по каталогам: server 5, wire 5, wire/finalgate 1, hub 6, apiauth 6, listener 5, ws/wsapi 4, ws/wswork 3, ws/wsconfig 4, rest/api 7, rest/apibatch 2, rest/servertest 3 (§4.5) — все каталоги ≤7 элементов.

### 2.5. Тесты (правило: белый тест живёт в пакете, чьи неэкспортированные идентификаторы использует; на несколько модулей — в server/ или servertest)

Префиксные правила (файлы `*_test.go` классифицированы скриптом по префиксу и точечной проверкой subject'а, сумма = 213):

| Правило | Пакет | Файлов |
|---|---|---|
| `api_read_*` (5), `api_mux_*` (2), `api_openapi_*` (3), `api_response_*` (1), `api_routes_*` (1), `api_permissions_*` (4, включая `api_permissions_test`), `api_run_*` (6), `api_runs_*` (2), `task76_*` (2), `api_idempotency_*` (2), `api_feature` (1), `api_events_{behavior,handler,isolation,revoke,stop,test}` (6), `api_session_{handlers,lock,openapi,resolution}` (4) | rest/api | 39 |
| `apiauth_*` (13), `auth_*` (3), `ratelimit_eviction` | apiauth | 17 |
| `api_batch_{answered,client,intake,limits,only,policy,question,role}` | rest/apibatch | 8 |
| `finalization_notify_test` | wire/finalgate | 1 |
| `hub_*` (4), `ws_serve_{origin,revoke,slots}`, `ws_serve_scope_*` (5), `readpump_*` (2), `p723_ws_register_shutdown_race`, `panic_timeline`, `p547_sticky_update_notice`, `shutdown_ack`, `settings_authority_transport`, `scope_table` | hub | 20 |
| `listener_*` кроме `listener_failure_report` (6), `tls_*` (3) | listener | 9 |
| `main`, `server*` (2), `serve_listener`, `serve_shutdown`, `serve_token_log`, `shutdown_quiescent_conn`, `shutdown_update_notice`, `web_cleartext_*` (3), `events_config*` (2), `finalization_wiring`, `dispatch`, `pin_production`, `tempdir_diag`, `ws_serve_integration`, `api_events_{integration,revoke_integration,revoke_resume,wire}` (4), `api_permissions_integration`, `api_run_integration`, `api_serve_*` (3), `api_session_integration`, `api_matrix_*` (9), `api_batch_{fixture,fixture_diag}` (2), `handlers_shutdown`, `listener_failure_report` | server | 41 |
| `peak_hours_wire_{json,v2}`, `p479_usage_wire` | wire | 3 |
| `handlers_{sessions,sessions_batch_cutover,agent_effort,agent_rerun_cutover,messages_part,messages_thinking}` (6), `rerun_hold`, `rerun_batch_fx`, `batch_rerun_notices`, `batch_rerun_stop_voided_mocks`, `attachments_lifecycle`, `session_title_broadcast`, `run_driver_liveness`, `web_composer_history`, `web_session_preference_restart` (9), `p1142`, `p1_6`, `p2`, `p569`, `p595` (2), `p614`, `p615`, `p622`, `p623` (2), `p630`, `p631`, `p638`, `p644`, `p651`, `p655` (2), `p684` (19) | ws/wsapi | 34 |
| `handlers_config_*` (5), `handlers_custom_roles`, `handlers_models`, `p1060`, `p1061`, `p460`, `p461`, `p462`, `p466`, `p695`, `p696`, `p698`, `catalog_effort_wire`, `model_visibility_wire`, `zai_context_window_wire`, `peak_hours_{authority,scope_policy,scope_resume,test}` — у `handlers_peak_hours.go` тестового файла нет | ws/wsconfig | 23 |
| `handlers_{batch,batch_control,batch_completions,wake_schedules}`, `handlers_livework_*` (6), `descendant_work_*` (5), `batch_child_bind_walk`, `livework_seed` | ws/wswork | 17 |
| `handlers_test.go` — расщепляется: помощники `isolateAllGlobalConfigPaths`/`forceLocalCLIProviderForTest`/`newAttachmentsTestApp` → rest/servertest/appfixture.go, а его 5 attachment-тестов (`TestSaveAttachmentToDisk_*`, `TestAttachmentsDataDir_*`) вливаются в wsapi (к `attachments_lifecycle_test.go`) | servertest + ws/wsapi | 1 |

Сумма = 213 (проверено скриптом классификации по префиксам; каждое правило выверено grep'ом по вызываемым handleX/швам — см. отклонения `p623_*` → wsapi (шов rerunPreHandoffSeam), `batch_rerun_*` → wsapi (rerun), `batch_child_bind_walk` → wswork (batchSourceFor), `handlers_shutdown_test` → server (handleShutdownServer в компоновщике), `dispatch_test` → server (белый на handleIncoming), `run_driver_liveness` → wsapi (handleListSessions), `web_composer_history`/`web_session_preference_restart` → wsapi (handleLoadMessages/handleSendMessage)).

Файлы с тестами (второй храповик A1b, включая тесты): server 46, api 46, wsapi 38, wsconfig 27, hub 26, apiauth 23, wswork 20, listener 14, apibatch 10, wire 8, finalgate 2, servertest 3 — все существенно ниже сида 295; сиды A1b пересаживаются на новые значения при разрезке.

`TestMain(testguard.Main)` копируется в каждый пакет с тестами, кроме `wire` и `wire/finalgate` (чистые, конфиг не трогают); `TestGlobalConfigPathsAreIsolated` остаётся только в server.

### 2.5a. Проверка §2.5 по живому дереву (lane server-2, 2026-11-23)

Инвентарь снят с дерева, а не с таблицы. У компоновщика `internal/serve/server/server` 133 тестовых файла. 130 из них читают `New`/`Server`/`ServeAPI` (`serve.go`) или общие фикстуры компоновщика (`startPolicyServer`, `newAttachmentsTestApp`, `dialServeWS`) — такой тест нельзя опустить ниже компоновщика, пока нет пакета `rest/servertest` (§4.5): переезд был бы портом, а не переносом. Без нового пакета доказуемо переезжают 8 файлов: они не читают ни одного неэкспортированного идентификатора компоновщика и не зовут ни одного из перечисленных конструкторов.

Переехали (проверено оракулом объявлений и тел; число Test-функций не изменилось):

| Файл | Из | В | Правка, кроме `package` |
|---|---|---|---|
| `api_routes_test.go` | server | rest/api | — |
| `api_session_openapi_test.go` | server | rest/api | — |
| `api_openapi_docs_golden_test.go` | server | rest/api | путь к `docs/api/openapi.json` на один сегмент глубже (`..`×5) |
| `api_idempotency_expiry_test.go` | server | rest/api | `newIdempotencyStore`→`NewIdempotencyStore`, `idempotencyMaxEntries`→`IdempotencyMaxEntries`, `idempotencyTTL`→`IdempotencyTTL`, `idempotencyStarted`→`IdempotencyStarted` (только регистр первой буквы) |
| `api_batch_policy_test.go` | server | rest/apibatch | `batchIntakePolicy`→`BatchIntakePolicy`, `batchIntakeMaxNodes`→`BatchIntakeMaxNodes`, `batchIntakeDefaultTimeout`→`BatchIntakeDefaultTimeout` |
| `web_cleartext_policy_test.go` | server | listener | — (`IsLoopbackHost` объявлен в listener/policy.go:18) |
| `peak_hours_wire_json_test.go` | server | ws/wsconfig | типы `wire` квалифицируются как `wirePkg.*` |
| `peak_hours_wire_v2_test.go` | server | ws/wsconfig | то же плюс `hub.ScopeFor`/`hub.AuthorizeCommand`/`hub.ErrScopeForbidden`/`hub.ErrAdminDisallowed` |

Поправки к §2.5 (премисы, ложные на живом дереве):

- `peak_hours_wire_{json,v2}` принадлежат `ws/wsconfig`, а не `wire`: конверсии `peakHoursToWire`/`peakHoursFromWire` объявлены в `ws/wsconfig/providers.go:101,135`, а `wire` — лист (§2.6) и не может импортировать `hub`, который нужен `peak_hours_wire_v2` для `ScopeFor`/`AuthorizeCommand`. Строка «`wire` 3» неверна: в `wire` один тестовый файл (`p479_usage_wire_test.go`).
- `server` 41 и остальные строки таблицы §2.5 — проектное намерение Л7, а не текущее состояние: 125 файлов из этого списка всё ещё в компоновщике и ждут `rest/servertest`.
- Храповики: `internal/serve/server/server` 138 → 130; `internal/serve/server/ws/wsconfig` 29 → 31 (два теста приехали).

### 2.6. Граф зависимостей (production-импорты, стрелка = «импортирует»)

```
kit < store < session < config < agent < app < serve          (группы; serve = вся эта разрезка)

message, session, config, app, pubsub, db, jobbatch, permission, agent, update, version  (ниже server)
        │
wire ──→ message, session
finalgate ──→ wire, app, session, message, pubsub
apiauth ──→ config
listener ──→ apiauth (RouteFull/RouteBatch в Validate), config, app, session
hub ──→ wire, apiauth, config, session
wsapi ──→ wire, hub, app
wswork ──→ wire, hub, app, session, db
wsconfig ──→ wire, hub, app, wsapi, config, session
api ──→ wire, finalgate, apiauth, app, config, session, message, permission
apibatch ──→ api, apiauth, app, agent, db, jobbatch, message, session
server ──→ listener, apiauth, hub, wire, finalgate, wsapi, wswork, wsconfig, api, apibatch, app, config, pubsub, update
servertest ──→ app, config, wire, apiauth, api, hub, listener   (только тесты; server НЕ импортирует)
```

Циклов нет: каждое ребро направлено вниз по этому списку; межсемейные рёбра ровно два — `wsconfig → wsapi` (AnnotateSessionExternalOwnership) и `apibatch → api` (APIRoute). Компоновщик server — единственная точка сборки (маршруты, поверхности, шлюз WS). Внутри одного пакета-«семьи» DAG тривиален.

## 3. Швы (по циклам §1.4; каждый — отдельная полоса до переноса семей)

Общие принципы: поведение не меняется; ни один глобал не экспортируется ради разрезки; имена через границу ≤15 (константы wire-протокола — общая лексика листа, в счёт поведенческих имён не входят); оракул чистого переноса (отсортированный список `^(func|type|var|const) ` и число Test-функций по объединению перенесённых наборов) — для каждой полосы ниже.

### S1. Лист `wire` (рвёт циклы 1.4-3 частично и все тяготения к protocol)

Что: protocol.go (1-732) → wire/protocol.go; protocol.go (732-841, секция «Live work panel») + APIEvent (api_events.go:15-20) + APIRunState/его константы/terminalAPIRunState (api_runs.go:24-33,189-191) + helloPayload/mustJSON (ws_serve_origin.go:11-21, переименованы HelloPayload/MustJSONRaw) → wire/snapshots.go; protocol_custom_roles.go → wire/custom_roles.go; protocol_peak_hours.go + peak_hours_wire_json.go → wire/peakhours.go; wire.go + api_types.go → wire/wire.go (конвертеры переименованы ToMessagesWire/ToMessageWire/ToUsageWire/APISessionFromModel/APIMessageFromModel/APITimestamp).
Имена через границу: потребители импортируют `wire.WSMessage`, `wire.Event*`/`Cmd*`, payload-структуры, `wire.APIEvent`, `wire.APIRunState` — типы и константы, не поведение.
Точки изменения вызовов: все файлы, использовавшие эти имена из package server (перечислены в §2.4), правят квалификатор.
Сохранение поведения: wire-типы — пассивные структуры с JSON-тегами; конвертеры чистые; `MarshalJSON` пиковых часов не меняется. Оракул: объединение объявлений protocol.go+custom_roles+peak_hours+wire.go+api_types.go+перечисленных кусков до/после совпадает побайтно после сортировки. JSON-контракт накрывают тесты, переезжающие вместе: `peak_hours_wire_json_test`, `peak_hours_wire_v2_test`, `p479_usage_wire_test` (wire) — плюс сквозные `web_composer_history_test`, `api_read_contract_test` (точные JSON-формы), `api_openapi_docs_golden_test` (золотой OpenAPI).

### S2. `apiauth` (рвёт цикл 1.4-1 и 1.4-6)

Что: api_audit_middleware.go → apiauth/audit.go; redact_secrets.go → apiauth/audit.go; api_response.go → apiauth/response.go; api_session_resolution.go + api_batch_only.go → apiauth/resolution.go; ratelimit_core.go → apiauth/ratelimit.go; auth.go → apiauth/ui_auth.go; ws_serve_auth.go → apiauth/chain.go; mtlsFingerprintAuth (tls_mtls.go:39-53) → apiauth/chain.go (переименован FingerprintAuth); apiauth_chain.go+apiauth_doc.go → apiauth/chain.go. RouteFull/RouteBatch (из api_batch_only.go:7-8) объявляются в apiauth.
Шов var-фолбэков: `serveAuthTickerFactory`/`serveAuthSlotReleased` (apiauth_chain.go:113-117) удаляются. У `ChainConfig` уже есть поля `TickerFactory`/`SlotReleased` (apiauth_chain.go:76-86): продакшн оставляет их nil (дефолт `realTicker` внутри Chain — ветка apiauth_chain.go:122-125 — не меняется), тесты `ws_serve_revoke_test`, `ws_serve_slots_test`, `api_events_revoke_integration_test`, `api_events_revoke_resume_test` передают заглушки явно в ChainConfig.
Шов сигнатуры: `resolvePinnedRoute(pin string, route APIRoute, r *http.Request)` → `ResolvePinnedRoute(pin, operationID, pathID string) int`; `resolveBatchOnlyRoute(route APIRoute)` → `ResolveBatchOnlyRoute(operationID string) int`. Точка изменения ровно одна: `protectAPIRoute` (api_mux.go, переезжает в api/routes.go) передаёт `route.OperationID` и `r.PathValue("id")`; аналогично `routeClassIsKnown(route APIRoute)` → `RouteClassIsKnown(operationID string)` (вызовы: openapi-валидация маршрутов — grep `routeClassIsKnown` при переносе). Порядок проверок внутри resolvers не меняется (видимость → класс; fail-closed).
Имена через границу (потребители api, hub, listener, server): 26 (§2.2) — выше ориентира 15, но это уже существующий экспортный договорChain/Principal/ответов, новых сущностей шов не создаёт; сузить нельзя без второго шва-интерфейса, который ничего бы не сэкономил (см. §6).
Сохранение поведения: auditMiddleware оборачивает next и пишет slog-строку теми же полями (api_audit_middleware.go:84-93); Chain — тот же порядок backoff→authenticate→rate limits→workspace→inflight→body-limit (apiauth_chain.go:127-215); порядок auditor-полей и кодов ответа не трогается. Тесты: apiauth_* (13) + auth_* (3) + ratelimit_eviction едут вместе; сквозные `api_matrix_*` (root) ловят транспортный контракт.

### S3. `finalgate` (рвёт цикл 1.4-3; ликвидирует var-шов)

Что: finalization_notify.go + finalization_wiring.go → wire/finalgate/pump.go; var `finalizationTerminalGateFor` (finalization_wiring.go:13) удаляется, вместо него экспортируется функция `GateFor(a *app.App) session.FinalizationTerminalGate` с тем же телом (nil-App → nil; выключенный флаг → nil).
Шов вызовов: три точки чтения гейта сохраняют момент вычисления — `newAPIEventsHandler` (api_events_handler.go:57): ветка «явный gate из аргументов выигрывает, иначе GateFor(store.App)» остаётся той же конструкцией конструктора; `startAPIEventFeeder` (api_events_feeder.go:40, вызывается api_serve.go:101): сигнатура получает вариадик `gates ...session.FinalizationTerminalGate` (как у хендлера) — продакшн не передаёт, берётся GateFor(app) там же, где читался var; `subscribeAndBroadcast` (events.go:59, root): то же — параметр `gate` с продакшн-значением GateFor(a) в точке вызова (server.go:294).
Сохранение поведения: помпа инертна при nil-гейте (возврат input без горутин, finalization_notify.go:60-62); порядок удержания/флаша не меняется; гейт читается на admission (документация finalization_notify.go:36-40 сохраняется). Тест-шов: `finalization_wiring_test.go` (переезжает в server/) подставляет заглушку через явные параметры вместо переопределения var; `finalization_notify_test.go` едет в finalgate.
Блокировки/горутины (доказательство чистоты): помпа не берёт замков hub/APIEventLog/реестра — её AwaitSealed работает в собственных горутинах releaser'ов (комментарий finalization_notify.go:41-43); полоса не меняет ни одного lock-порядка, только адрес вызова.

### S4. `hub`: шлюз WS, TitleCache, AuthorizeIncoming (рвёт циклы 1.4-2 и 1.4-6)

Что: hub.go расщепляется на hub/hub.go (Hub) + hub/client.go (Client, dispatch/dispatchControl/runRecovered, replayBuffer, writePump, reply*); hub_pin_filter.go+hub_pin_sessions.go → hub/pin.go; ws_serve_scope.go+scope_table.go → hub/scope.go; titleCache/newTitleCache (handlers_livework.go:93-125) → hub/titlecache.go (экспорт TitleCache/NewTitleCache); handleWS/readPump (server.go:504-645) + ws_serve_revoke.go → hub/gate.go в виде компонента `Gate`.
Шов Gate (выделение компонента по решению оператора для несущих типов): `NewGate(GateOptions{Hub *Hub; CheckOrigin func(*http.Request) bool; UIAuth bool; AllowAdmin bool; OnShutdownRequest func(); Workers *sync.WaitGroup}) http.Handler` — это тело handleWS/readPump без изменений, только поля `s.*` заменены полями options. Компоновщик (server.go, секция mux `/ws`, строки 304-321) строит шлюз: `hub.NewGate(hub.GateOptions{Hub: s.hub, CheckOrigin: s.checkOrigin, UIAuth: s.uiAuth, AllowAdmin: s.policy.AllowAdmin, OnShutdownRequest: s.requestShutdown, Workers: &s.wsWorkers})`. Тонких делегирующих методов в Server не остаётся (mux монтирует шлюз напрямую).
Шов TitleCache: `Hub.liveWorkTitles` остаётся полем хаба; handlers получают его методом `Hub.LiveWorkTitles() *TitleCache` (тип экспортируется). Точки изменения: handlers_livework.go:54,293 → `c.hub.LiveWorkTitles()`.
Шов AuthorizeIncoming: авторизационный блок handleIncoming (handlers.go:35-50: `resolvePinnedCommand`, `resolveBatchOnlyCommand`, `authorizeWSCommand` по `c.auth`) переносится в метод `Client.AuthorizeIncoming(msgType string, payload json.RawMessage) int` (hub/scope.go); hub внутри вызывает apiauth.ResolvePinnedCommand/ResolveBatchOnlyCommand/ScopeFor. Точка изменения: handleIncoming (server/dispatch.go) вызывает один метод.
Имена через границу hub: 12 (§2.2) ≤15. hub → apiauth: Principal, PrincipalFrom, ScopeFor, CommandPolicy, ResolvePinnedCommand, ResolveBatchOnlyCommand, Ticker — 7.
Сохранение поведения и замки (критический путь — заказ #723): кластер горутин и каналов не меняется: `tryRegister` с двойной проверкой `hub.stopped`; readPump держит безусловный `defer c.conn.Close()` и select-отмену отправки в unregister (server.go:576-592); дисциплина закрытия `workQueue` и решение «queued items дорабатывают» (#698) переносятся дословно; dispatchControl остаётся отдельным семафором для control-plane (hub.go:255-272); `wsWorkers.WaitGroup` передаётся в GateOptions — Join-порядок в shutdown-наблюдателе server.go:404-418 не меняется. Оракул: объединение объявлений hub.go+pin+scope+ws_serve_revoke+кусков server.go до/после совпадает; поведение накрывают `p723_ws_register_shutdown_race_test`, `readpump_control_starvation_test`, `shutdown_ack_test`, `ws_serve_slots/scope/integration` (все — hub/server).

### S5. Инверсия dispatch: `Dispatch` семей (рвёт цикл 1.4-5)

Что: переключатель handleIncoming (handlers.go:56-203) разбирается по семьям. wsapi/wswork/wsconfig получают по одной экспортной функции:
`func Dispatch(ctx context.Context, a *app.App, c *hub.Client, msg wire.WSMessage) bool` — обрабатывает свои команды через `c.Dispatch`/`c.DispatchControl` и возвращает false, если команда не её.
- wsapi.Dispatch: send/interrupt/inject/cancel, summarize, sessions CRUD, system prompt, messages (CRUD/rerun), rename, track usage;
- wswork.Dispatch: live work, wake schedules, batch status/output/stop/add/drain;
- wsconfig.Dispatch: config/logs/theme/keep-alive/debug, context/skills paths, custom roles, MCP/LSP, models/scoped models, providers/keys/peak hours, initialize project, log_client_* — НЕТ: log_client_* остаются в root (см. ниже).
Root `server/dispatch.go` сохраняет: разбор JSON, `c.AuthorizeIncoming`, цепочку `wsapi.Dispatch` → `wswork.Dispatch` → `wsconfig.Dispatch`, затем локальные команды root: `CmdShutdownServer` (handleShutdownServer — нужен `s.requestShutdown`, handlers_shutdown.go) и `log_client_event`/`log_client_error` (handlers.go:216-233), плюс `autoApproveWebSession` (handlers.go:209).
Имена через границу: 3 Dispatch + типы hub/app/wire. Межсемейное ребро wsconfig → wsapi (AnnotateSessionExternalOwnership внутри handleSetSessionModels) сохраняется.
Сохранение поведения: команды матчатся точными константами — порядок плеч переключателя семантики не имеет; контроль-плейн (CmdInterruptAndSend, CmdCancelAgent — wsapi через DispatchControl; CmdShutdownServer — root) сохраняет «не стоят в очереди за длинными ходами» (документация handlers.go:9-27 переносится дословно). Имена в логах dispatch (`"handleSendMessage"` и т.п.) сохраняются байт-в-байт. Оракул: grep — каждый `Cmd*`/литерал из sessionResolutionTable встречается в ровно одном Dispatch; число ветвей = числу команд до разрезки. Тесты: `dispatch_test.go` (root, белый на handleIncoming) + p-семья wsapi.

### S6. Инверсия сборки маршрутов (рвёт цикл 1.4-4)

Что: `apiRoutesWithBroker` (api_read_handlers.go:92-107) переезжает в `server/serve.go` (рядом с NewServeAPI, api_serve.go) и собирает таблицу из семьи-пакетов: `api.Routes(...)` (core+read+session+permission+events), `apirun`-часть остаётся внутри api (run-маршруты — api/routes.go), `apibatch.Routes(a)` — batch-таблица. `NewServeAPI` вызывает сборщик в той же точке (api_serve.go:66), где вызывался `serveAPIRoutes`.
Шов OpenAPI: `apiOpenAPIHandler` (api_routes_core.go:21) сегодня читает `APIRoutes()` в момент запроса; после разрезки каркас принимает провайдер таблицы: `api.Routes(openAPIRoutes func() []APIRoute, deps …)` — root передаёт замыкание над собранной полной таблицей. Момент вычисления сохраняется: лениво, на запрос `GET /api/v1/openapi.json` (золотой тест `api_openapi_docs_golden_test.go` — api — остаётся арбитром побайтового равенства).
Шов видимости семей: `APIRoutesWithDeps`/`APIRoutesWithApp` (api_read_handlers.go:77-89) остаются в api/read.go (используются тестами read и openapi-фичи); batch-часть APIRoutesWithDeps заменяется на `apibatch.Placeholders()` — она вернёт те же placeholder-маршруты, пока app == nil (ветка api_read_handlers.go:96-98 сохраняется).
Имена через границу: root→apibatch: `Routes`, `ErrBatchOverloaded`; root→api: список §2.2.
Сохранение поведения: таблицы маршрутов, их порядок в OpenAPI (append-порядок api_routes.go:23-30), public/protected-разделение NewAPIHandlerSplit и лимит health (api_serve.go:63-72) не меняются. Замки: `serveFlushers` (реестр flusher'ов по `*config.APITokenStore`, api_serve.go:46-52) остаётся в root рядом с единственным писателем; `stopOnce` у ServeAPI не трогается.

### Сводная проверка швов

| Шов | Рвёт | Имена через границу |
|---|---|---|
| S1 wire | тяготение protocol (§1.3) | типы/константы листа (лексика) |
| S2 apiauth | цикл 1.4-1, 1.4-6 | 26 (существующий экспортный договор) |
| S3 finalgate | цикл 1.4-3 | 5 |
| S4 hub | цикл 1.4-2 | 12 (hub) + 7 (hub→apiauth) |
| S5 Dispatch | цикл 1.4-5 | 3 |
| S6 маршруты | цикл 1.4-4 | Routes, ErrBatchOverloaded + api-список |

## 4. Глобалы, init(), embeds, OS-файлы, белые тесты, помощники

### 4.1. Пакетные переменные (каждой — ровно один владелец; ни одна не экспортируется ради разрезки)

| var | Где сейчас | Владелец после | Примечание |
|---|---|---|---|
| Listen | server.go:25 | server | экспортный var-шов тестов cmd — остаётся |
| ErrUnexpectedNonLoopbackBind | server.go:24 | server | сентинел |
| serveAPIRoutes, serveAPINow | api_serve.go:22-23 | server/serve.go | шовы тестов server (api_serve_test, api_serve_scope, api_events_revoke_resume) |
| serveFlushers | api_serve.go:46-52 | server/serve.go | реестр flusher'ов, sync.Mutex внутри; единственный писатель — NewServeAPI |
| finalizationTerminalGateFor | finalization_wiring.go:13 | — ликвидируется (S3) | явный GateFor/параметры |
| serveAuthTickerFactory, serveAuthSlotReleased | apiauth_chain.go:113-117 | — ликвидируются (S2) | поля ChainConfig |
| rerunPostTruncateSeam, rerunPostIdlePollSeam, rerunHoldingReservationSeam, rerunPreHandoffSeam | handlers_agent.go | wsapi/agent.go | шовы rerun, тесты wsapi |
| batchRerunReconcile | handlers_agent_rerun.go | wsapi/rerun.go | шов, тесты wsapi |
| batchSourceFor | handlers_batch.go:144 | wswork/batch.go | шов, тесты wswork |
| batchAddForWeb | handlers_batch_control.go | wswork/batch.go | шов, тесты wswork |
| batchPendingQuestionsFor, batchAgentTaskKinds | handlers_livework_batch.go | wswork/livework.go | шов/данные, тесты wswork |
| scopedModelSlots | handlers_models.go | wsconfig/models.go | данные |
| apiBatchServices | api_batch_service.go | apibatch/service.go | реестр сервисов по app |
| testHookMu, testBeforeWrite | hub.go:820-826 | hub/client.go | шов до записи кадра, тест hub (shutdown_ack) |
| configNotificationsProcessed/Enqueued | events.go:22-24 | server/events.go | атомарные счётчики |
| batchOnlyOperations; sessionResolutionTable; restSessionResolution | api_batch_only.go; api_session_resolution.go | apiauth/resolution.go | таблицы авторизации (read-only) |
| commandScopes, ErrCommandUnmapped; eventClasses; configLocalPathKeys | scope_table.go; hub_pin_filter.go; ws_serve_scope.go:112 | hub/scope.go; hub/pin.go; hub/scope.go | read-only таблицы |
| bearerPattern; errAmbiguousWebSocketCredentials/errMissingWSProtocol | apiauth_chain.go:111,218-222 | apiauth/chain.go | — |
| secretPattern | redact_secrets.go:12 | apiauth/audit.go | — |
| requestIDPattern | api_response.go:15 | apiauth/response.go | — |
| idempotencyKeyPattern | api_idempotency.go:26 | api/idempotency.go | — |
| ErrAPIPermissionUnknown | api_permissions.go | api/permissions.go | сентинел |
| ErrAPIRunNotFound, ErrAPIRunBackendBusy | api_runs.go:17; api_run_backend.go:20 | api | сентинелы |
| errBatchOverloaded | api_batch_service.go | apibatch/service.go | сентинел |

Правило: межпакетный доступ к перечисленному — только через API §2.2/§3; экспортируемых изменяемых var шов не создаёт (Listen остаётся var по договору с cmd-тестами, он и сегодня экспортируемый).

### 4.2. init()

`init()` в непроизводных файлах нет (prereq.py «init(): 0», grep пуст). Порядок инициализации после разрезки не зависит от новых пакетов: нет ни одного init, регистрация всего — явная (компоновщик server). Единственная неявная инициализация — срабатывание конструкторов пакетов при импорте; новых side-effect-пакетов (регистраций, glue-файлов) дизайн не добавляет.

### 4.3. Embeds и OS-файлы

`go:embed` в internal/server нет (prereq.py «go:embed: 0»); OS-пар нет. Встроенный dist веб-клиента — `web/embed.go` (пакет `web` в корне, `//go:embed all:dist`, `web.FS()`); его потребитель — `internal/cmd/root.go:273`, передающий `fs.FS` в `server.NewWithCleartext` → поле `Server.static` → `spaHandler`. Разрезка этот путь не меняет: static остаётся полем компоновщика, `spaHandler` — в server/server.go. OS-специфика (об наружение процессов, сигналы) в server отсутствует.

### 4.4. Белые тесты и TestMain

Все 213 тестов — `package server`; распределение по правилу «владелец неэкспортированных идентификаторов» — §2.5. Особые случаи:
- `p623_panic_window_test`(+diag) — шов rerun → wsapi, хотя тестирует восстановление паники: runRecovered остаётся в hub, но инъекция паники идёт через rerunPreHandoffSeam (wsapi); сам механизм паники отдельно накрыт `panic_timeline_test` (hub).
- `dispatch_test` — белый на handleIncoming → server.
- `handlers_shutdown_test` — белый на handleShutdownServer (root-команда) → server.
- Тесты нескольких модулей → server/ (компоновщик): все `api_matrix_*`, интеграционные events/serve/session/run/batch-фикстуры, pin_production, web_cleartext_*, finalization_wiring. В internal/app/itest ничего не добавляется: все сценарии server замкнуты на server-пакеты и не требуют app+server+session одновременно вне server.
- `TestMain(testguard.Main)` копируется во все пакеты с тестами, кроме wire и wire/finalgate; изоляция глобального конфига (`isolateAllGlobalConfigPaths`) уезжает в servertest и вызывается оттуда.
- Переезжающий тест несёт окружение своего TestMain (урок §3 п.12 плана): testguard.Main не имеет локального состояния — копия идентична.

### 4.5. servertest (тест-помощники, общий пакет по образцу internal/db/dbtest)

Файлы-источники → servertest:
- `rest/servertest/appfixture.go` ← handlers_test.go:33-149 (`isolateAllGlobalConfigPaths`, `forceLocalCLIProviderForTest`, `newAttachmentsTestApp` → NewAppFixture);
- `rest/servertest/wsdial.go` ← dialServeWS (ws_serve_slots_test.go), sendCommand/exchange/awaitReply/awaitType/containsType (ws_serve_scope_fixture_test.go), dialTestWS (readpump_control_starvation_test.go);
- `rest/servertest/fixture.go` ← startPolicyServer (serve_listener_test.go:14) в двух вариантах: полный (остаётся в server/ для root-тестов — он белый на Server через listenerSnapshot) и StartServeFixture на servertest-примитивах (chain + REST-mux + hub.Gate на plain http.Server, без трекинга соединений и shutdown-sequencing компоновщика); плюс panicTimeline (panic_timeline_test.go:20) → PanicTimeline и describeTempTree/logTempTreeBeforeRemoval (tempdir_diag_test.go) → DescribeTempTree/LogTempTreeBeforeRemoval.
servertest НЕ импортирует корневой server (иначе круг: тесты подпакетов → servertest → server → подпакет). Фикстуры на servertest воспроизводят наблюдаемое поведение цепочки (заголовки, статусы, SSE-поток, WS-рукопожатие с subprotocol/origin), но не shutdown-sequencing компоновщика — его тестируют root-тесты (serve_shutdown, shutdown_quiescent_conn). hub/wsconfig-тесты, использовавшие newServeWSFixture/newServeWSScopeFixture (startPolicyServer от `*Server`), переписываются на StartServeFixture с той же наблюдаемой поверхностью; root-версия остаётся для root-тестов. Отличия фикстуры от компоновщика перечислены явно ( ConnState-трекинг, admission-защита при shutdown, UI-surface) — тестов, проверяющих именно их вне server/, после разреза нет.

### 4.6. r15bfixture

`package main`, ручной стенд r15b (fake-LLM провайдер + сервер). Ни тесты, ни код его не импортируют. Остаётся без изменений: `server/r15bfixture/` (2 файла). Элемент каталога server/ — учтён в счёте §2.1 (6 = 5 файлов + r15bfixture/).

## 5. План исполнения (полосы)

Все полосы правят код через `gofumpt`, `go build`, `go vet` затронутых пакетов (через `.rush/tmp/golock.sh`); тесты серийно гоняет оркестратор (`golock.sh go test -p 1 -parallel 1 -count=1`, полные прогоны под `capm 2g`, §3 п.7 плана спринта). Одновременно не больше 2–3 полос (лимит 5 не выбирается до предела). Пути — после этапа C; до C замена путей механическая (`internal/server/...` вместо `internal/serve/server/...`), порядок полос не меняется.

| Полоса | Этап | Содержание (файлы по §2.4) | Ждёт | Может идти параллельно с |
|---|---|---|---|---|
| Л1 | 0б-5a | wire: protocol.go (1-732 / 732-841), protocol_custom_roles.go, protocol_peak_hours.go, peak_hours_wire_json.go, wire.go, api_types.go; куски: APIEvent (строка 6), APIRunState+terminalAPIRunState (строка 27), helloPayload/mustJSON (строка 80, первая часть); finalgate: finalization_notify.go, finalization_wiring.go (S1+S3). Тесты: peak_hours_wire_json, peak_hours_wire_v2, p479_usage_wire → wire; finalization_notify_test → finalgate | — | Л2 |
| Л2 | 0б-5a | apiauth: api_audit_middleware.go, redact_secrets.go, api_response.go, api_session_resolution.go, api_batch_only.go, ratelimit_core.go, auth.go, ws_serve_auth.go, apiauth_chain.go, apiauth_doc.go, mtlsFingerprintAuth из tls_mtls.go (S2): ликвидация serveAuthTickerFactory/serveAuthSlotReleased, Resolve-сигнатуры. Тесты: apiauth_* (13), auth_* (3), ratelimit_eviction → apiauth | — | Л1 |
| Л3 | 0б-5a | listener: listener_policy.go (+остаток ws_serve_origin.go), listener_pins.go, listener_pin_validation.go, tls_server.go, tls_pins.go, tls_reload.go, tls_selfsigned.go, loadClientCAPool из tls_mtls.go. Тесты: listener_* (6), tls_* (3) → listener | Л1, Л2 (общие файлы ws_serve_origin.go, tls_mtls.go) | — |
| Л4 | 0б-5b / D2 | hub: расщепление hub.go, hub_pin_filter.go, hub_pin_sessions.go, ws_serve_scope.go, scope_table.go, titleCache из handlers_livework.go, handleWS/readPump из server.go, ws_serve_revoke.go (S4): Gate/GateOptions, TitleCache, AuthorizeIncoming. Тесты: 20 файлов → hub | Л1, Л2, Л3 | — |
| Л5 | D2 | семьи ws: handlers_sessions.go, handlers_messages.go, handlers_agent.go, handlers_agent_rerun.go → wsapi; handlers_batch.go, handlers_batch_control.go, handlers_batch_doc.go, handlers_livework.go (остаток), handlers_livework_batch.go, handlers_wake_schedules.go → wswork; handlers_config.go, handlers_custom_roles.go, handlers_models.go, handlers_providers.go, handlers_mcp.go, handlers_peak_hours.go, providers_peak_hours_write_fields.go → wsconfig; handlers.go → server/dispatch.go + Dispatch семей (S5). Тесты: wsapi 34 (+вливание 5 attachment-тестов из handlers_test.go), wsconfig 23, wswork 17 | Л4 | Л6 |
| Л6 | D2 | rest: api_routes*.go, api_mux.go, api_openapi.go, api_read_handlers.go (−сборщик), api_session_handlers.go, api_run_backend.go, api_run_role.go, api_run_handlers.go, api_runs.go (−кусок), api_idempotency.go, api_events.go (−кусок), api_events_feeder.go, api_events_handler.go, api_events_revoke.go, api_events_route.go, api_permissions.go, api_permissions_handlers.go → api; api_batch_handlers.go, api_batch_policy.go, api_batch_service.go → apibatch (S6). servertest: appfixture.go из handlers_test.go (33-149), wsdial.go, fixture.go (WS-вариант сразу, REST-вариант после появления api). Тесты: api 39, apibatch 8 | Л1, Л2 | Л5 |
| Л7 | D2 | компоновщик и сквозные тесты: server.go (остаток), api_serve.go+api_serve_pins.go → serve.go (+сборщик из api_read_handlers.go), events.go, update_notice.go; алиасы §2.3; TestMain во все пакеты; перестановка fixture-тестов hub/wsconfig на servertest (ws_serve_slots/revoke/scope_*, settings_authority_transport, handlers_config_lock_trusted, hub_pin_filter_test, listener_* с startPolicyServer); переезд интеграционных и matrix-тестов в server (их список — строка «server 41» §2.5). Тесты: 41 файл → server | Л5, Л6 | — |

Приёмка каждой полосы (кроме сборки/vet/gofumpt):
- Л1: оракул объединения объявлений (sorted `^(func|type|var|const) ` и число Test-функций по строкам 6, 27, 64, 78, 80 §2.4 до/после); grep: импорты wire не содержат `internal/serve/server/{hub,api,server,...}`; тесты wire/finalgate зелёные (оркестратор).
- Л2: grep: `serveAuthTickerFactory|serveAuthSlotReleased` — 0 вхождений; grep: `route APIRoute` в apiauth — 0 (сигнатуры на operationID/pathID); тесты apiauth зелёные.
- Л3: build listener; grep: listener не импортирует hub/api; тесты listener зелёные (те, что звали startPolicyServer от `*Server`, временно остаются в server до Л7 — пометить в PR описании).
- Л4: оракул объединения объявлений hub-набора (строки 57-59, 70-71, 80-82 §2.4 + scope_table.go); тесты hub зелёные, серийный race-прогон оркестратором по hub (p723, readpump_control_starvation, shutdown_ack — заказы #723/#698).
- Л5: grep: каждый `Cmd*`/литерал sessionResolutionTable встречается в ровно одной Dispatch; число dispatch-ветвей до/после равно; тесты wsapi/wswork/wsconfig зелёные.
- Л6: golden OpenAPI тест побайтно зелёный; тесты api/apibatch зелёные.
- Л7: тесты server зелёные; `git grep 'internal/server'` по кодовой базе — только в комментариях/доках; cmd собирается без правок (алиасы §2.3); полный серийный прогон затронутых пакетов оркестратором, затем fork CI волны.

Храповики в тот же коммит каждой полосы: `.githooks/dir_entries_allowlist.txt` — запись для `internal/server` (появление первых подкаталогов поднимает число элементов; расписание падения: после Л7 каталог = 7 элементов, запись удаляется); списки файлов 992/841 (hub.go, protocol.go) закрываются расщеплением Л4/Л1.

## 6. Риски и открытые вопросы

Риски (по убыванию):
1. **Л4 (hub) — гонки WS-жизненного цикла.** Заказы #723 (register/shutdown), #698 (доработка очереди после закрытия), #631 (ownlock) зафиксированы комментариями в переносимом коде; перенос — дословный, но перестановка дефера или select-ветвей ломает её незаметно. Защита: дословный перенос строк-диапазонов §2.4, оракул объявлений, серийный race-прогон оркестратором по hub, тесты p723/readpump_control_starvation/shutdown_ack как арбитры.
2. **Wire-контракт.** 67 payload-структур, 26 Event/60 Cmd констант, golden OpenAPI и точные JSON-тесты (api_read_contract, web_composer_history, peak_hours_wire_*) — любой сдвиг тега или имени поля ломает веб-клиент. Переименований, видимых снаружи пакетов, 13 (§2.2: HelloPayload, MustJSONRaw, ToMessagesWire/ToMessageWire/ToUsageWire, APISessionFromModel/APIMessageFromModel/APITimestamp, FingerprintAuth, OriginsAllowed, ValidateAllowedOrigins, StartEventFeeder, FilterSessionsForPinnedClient) — все внутрипакетные потребители перечислены; JSON-имена не меняются ни одним из них.
3. **Тестовая перестановка Л5/Л7 — самая объёмная** (105 файлов тестов). Фикстуры на servertest воспроизводят цепочку без компоновщика: различия (нет ConnState-трекинга, admission-защиты, UI-surface) перечислены в §4.5; если какой-то тест матрицы окажется зависим от них, он переезжает в server/ (правило владельца), а не подгоняется под фикстуру.
4. **apibatch → agent** (api_batch_service.go:16 импортирует internal/agent) — ребро вниз по слоям (serve > agent) законно, но делает тесты apibatch зависимыми от агента; сокращение этого ребра — вне спринта (замечено и оставлено как есть, поведение не меняется).

Не удалось проверить без запуска кода (честно):
- точные построчные границы расщеплений hub.go/server.go/protocol.go даны по состоянию файлов на 2026-11-15; если файлы изменились с момента замера, границы пересчитываются по анкерам символов (имена в §2.4/§3 неизменны);
- число точек вызова `routeClassIsKnown` в непроизводном коде (в тестах есть) — grep при Л2;
- поведение `p623_*` после переезда в wsapi (зависимость от порядка инициализации шва rerunPreHandoffSeam) — проверить серийным прогоном wsapi после Л5; альтернатива, если упадёт: файл остаётся в hub с переносом шва в параметры — решение по факту прогона, на дизайн других полос не влияет.

Открытых вопросов для оператора нет: все решения (8 межпакетных расщеплений, 13 переименований, ликвидация трёх var-швов, алиасы cmd, размещение r15bfixture и servertest) приняты в этом документе и обоснованы в §2-§4.
