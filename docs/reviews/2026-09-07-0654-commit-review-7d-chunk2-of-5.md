# Ревью коммитов — 7-дневное окно, chunk 2 из 5 (`7b928bb4b..1d3cdf4a7`)

> Сверка статусов 2026-09-09 по коммиту `4c2f11bd3` (main): закрытия,
> произошедшие после написания ревью, помечены закрывающими коммитами;
> неподтверждённые закрытия оставлены открытыми.

Обзор 43 коммитов от 2026-09-02 09:21 до 2026-09-03 16:34 CEST (39 обычных
+ 4 merge). Тематически диапазон закрывает scoped `fs_*` toolset (T7–T12),
pluggable `DiskProvider` (#856–#859), `/wrush` slash-command, и три волны
security-remediation по review-раундам R5 (7 находок), R6 (4 находки) и
R13/R14. Заканчивается merge-коммитом `1d3cdf4a7`.

Проверялись: корректность, полнота, пропущенные edge cases, невакуумность
новых/изменённых тестов, соответствие commit message и doc-комментариев
фактическому diff, concurrency, утечки goroutine/памяти, error handling.

Режим только чтения: `go build`/`go test`/`go vet`/gofumpt/`.githooks/*`
не запускались. Единственные выполненные команды — read-only git
(`log`/`show`/`diff`/`ls-tree`/`merge-base`/`merge-tree`) и чтение файлов
на текущем tip воркtree.

Все находки ниже выведены самостоятельно из диффов и текущего кода. Там,
где статус находки на сегодняшнем HEAD определяется коммитом ВНЕ моего
диапазона, это указано явно; для `31ea604f`/`abed8946` факт закрытия
дополнительно сверен с уже существующим
`docs/reviews/2026-09-05-2047-commit-review-36h.md` (его разделы 1 и 7) —
но сам факт наличия дыры на конце МОЕГО диапазона установлен независимо,
чтением `noRealWorkspaceForbiddenTools` в состоянии `eab5d508a`.

---

## Сводка коммитов

Хронологически (снизу вверх в `git log`):

| Hash | Тема | Вердикт |
|---|---|---|
| `339cbdeac` | T7: `fs_*` в `allToolNames`/`resolveReadOnlyTools` | корректно |
| `a5342412b` | checkpoint | — |
| `79382dff8` | T8: `CallOptions.FolderScope` + `applyCallFolderScope` | корректно, см. CR-1 |
| `916612573` | T9: отказ scoped-вызова на CLI-провайдере | неполно, см. CR-2 |
| `2d9307965` | T10: `RunOverrides.FolderScopes` + SDK + restricted-run фикс | корректно, см. CR-9 |
| `53aa30d95` | восстановление `slog.Default()` в 3 тестах | корректно, но неполно — добито `cec10b5d2` |
| `4f56929f8` | T11: `<scoped_filesystem>` в coder-prompt | корректно |
| `cec10b5d2` | 4 источника утечки памяти/висов | корректно, сильная работа |
| `1b6b2b0e1` | T12: persist/rebind folder-scope через durable restart | корректно; собственный дефект найден и закрыт в R5-3 |
| `dfce5ac76` | #856: `DiskProvider` + `OSDisk()` | корректно |
| `bd46878a9` | #857: проводка `DiskProvider` через `fs_scope` + 8 `fs_*` | корректно |
| `4bea97e43` | checkpoint | — |
| `baf2da4b7` | #858: `CallOptions.DiskProvider` → `buildTools` | корректно |
| `eff3ce15b` | #859: SDK-поверхность + 3-слойный durable-отказ | корректно, см. CR-3 |
| `a62239242` | merge disk-provider-integration | чистый (проверено `merge-tree`) |
| `b75637475` | `/wrush` slash-command | корректно |
| `f4e30ce4f` | docs: checkpoints/plans/review notes | — |
| `be8369b11` | style: gofumpt (2 теста) | корректно, чисто форматирование |
| `a4361bb03` | R5-1 (P0): fail closed вместо расширения до shared toolset | корректно |
| `e567dd48a` | R5-2 (P0): канонизация корней folder-scope | корректно |
| `54a98e2bf` | R5-3 (P1): полный `CallOptions` на durable replay | корректно |
| `451f8f8cc` | R5-4 (P1): экспорт sentinel-ошибок в `sdk` | корректно, тест невакуумный по построению |
| `83ca7e831` | R5-5 (P1): `fs_write` отказ при нечитаемом snapshot | корректно |
| `336b81c0c` | R5-6 (P1): виртуальный root для ephemeral library mode | корректно; регрессия собственного драфта поймана |
| `159762103` | R5-7/R5-8 (P2): `Origin` на rebuild + stale-комментарий | корректно |
| `98e9f0f9d` | merge sdk-review-r5-integration | чистый (проверено `merge-tree`) |
| `34d692965` | checkpoint | — |
| `2eff9f74b` | R6-1 (P0): default-deny real-disk/command в ephemeral library | корректно, см. CR-4 |
| `87e9c72cb` | R6-2 (P1): resolve результатов `fs_list`/`fs_find` до `Check` | корректно, см. CR-8 |
| `76c883bc5` | review: round 11 | — |
| `3e84a240d` | R6-3 (P1): fail closed на `DisableSubAgents`/`ModelRole` | корректно, см. CR-7 |
| `8d4fd702f` | R6-4 (P2): валидация `CallOptionsSpec.Version` | корректно, см. CR-11 |
| `059b5740f` | review: round 12 | — |
| `e5869ef3e` | docs: trust boundary `/wrush` | корректно и честно |
| `80b6db113` | merge sdk-review-r6-integration | чистый (проверено `merge-tree`) |
| `1092f3285` | style: gofumpt `session_runqueue.go` | **портит doc-комментарий, см. CR-6** |
| `b177206ac` | review: round 13 | — |
| `73878311c` | fix: 2 реальных CI-падения (оба — баги тестов) | корректно, см. CR-12 |
| `4ef3ebcdf` | R13-1 (P2): проброс version-ошибки через `FromSessionAgentCallData` | корректно |
| `b0f9e2d89` | checkpoint | — |
| `8ed45150b` | review: round 14 (1328) | см. CR-5 |
| `eab5d508a` | R14-1/2/3: единая no-real-workspace capability | корректно, но floor неполон — CR-4 |
| `1d3cdf4a7` | merge r14-1-2-3-capability | чистый (проверено `merge-tree`) |

---

## 0. Проверка merge-коммитов

Задание требовало отдельно проверить, что merge-коммиты не содержат
ошибок разрешения конфликтов и не потеряли hunks. Для всех четырёх merge
в диапазоне дерево коммита побитово совпадает с результатом чистого
автоматического 3-way merge его родителей (`git merge-tree --write-tree
<p1> <p2>`, сравнение с `<merge>^{tree}`):

| Merge | Вычисленное дерево | Фактическое дерево |
|---|---|---|
| `a62239242` | `8192cc5e99daff8d32c7e46befd4c550aa504c8a` | совпадает |
| `98e9f0f9d` | `3f7495a50c33125ce9a737aaf7aef0d513777acd` | совпадает |
| `80b6db113` | `cbbc938c6b98ea7c79d6f00f9b0fb1230be1c1b1` | совпадает |
| `1d3cdf4a7` | `cd484129bcd596ee43a0316f607a3c5195b18f9a` | совпадает |

То есть ни один merge не вносил ручных правок поверх автоматического
результата: дропнутых hunks и «случайно откаченных» изменений нет по
построению. Дополнительно: `git diff <merge>^2 <merge>` для `98e9f0f9d`
и `1d3cdf4a7` пуст (дерево merge равно дереву feature-ветки), для
`a62239242` содержит только `docs/checkpoints/2026-09-02-1753.md`, для
`80b6db113` — только 4 doc-файла со стороны первого родителя
(`README.md`, `claude_wrush_command.md`, round-11/round-12 review docs).
Всё это ровно то, что и ожидается.

Отдельно стоит отметить merge-message `1d3cdf4a7`: автор публично
опровергает собственную же находку R14-2 («P0 as filed does NOT
reproduce») с описанием проведённого revert-check. Я проверил это
утверждение независимо по коду и подтверждаю: в
`tools.CanonicalizeFolderScopeSpec`
(`internal/agent/tools/fs_scope.go`) `spec.WorkingDir` резолвится
БЕЗУСЛОВНО и ДО пер-entry канонизации, а для ephemeral
`sdk.ModeLibrary` сессии `spec.WorkingDir` всегда равен sentinel'у
(`sdk/library_mode.go`, `configWorkingDir` fallback), поэтому
`resolveScopedPath` спотыкается о
`rejectRealDiskUnderLibraryVirtualRoot` ещё на WorkingDir — до любого
entry, абсолютного или относительного. Заявленный в round-14 эксплойт
действительно недостижим; фикс тем не менее оставлен как
defense-in-depth, и это правильное решение (он защищает от будущего
рефакторинга порядка резолва внутри `CanonicalizeFolderScopeSpec`).

---

## 1. Scoped `fs_*` toolset (T7–T12)

**Статус: архитектурно корректно.** Одна открытая дыра в escape-hatch
списке (CR-1) и одна неполнота отказа на CLI-провайдере (CR-2).

`339cbdeac` (T7) — чисто механическая регистрация имён; три
существующих теста в `load_providers_test.go` обновлены, включая
`...WithEveryReadOnlyToolDisabled`, чья собственная предпосылка («каждый
read-only tool отключён») иначе перестала бы быть истинной. Это как раз
тот случай, когда правка теста НЕ является ослаблением оракула, и
commit message это объясняет верно.

`79382dff8` (T8) — `applyCallFolderScope` поставлен ПОСЛЕ
`buildToolsAgentConfigForCall`, и это критично: worker-layering
ДОБАВЛЯЕТ `bash`/`edit`/`write` в `AllowedTools` worker-субагента, так
что фильтр обязан видеть финальный список. Ordering-инвариант закреплён
тестом со scoped/unscoped контролем. Списки из commit message сверены с
кодом один в один: 7 legacy-file (`view`/`glob`/`grep`/`ls`/`write`/
`edit`/`multiedit`), 5 escape-hatch, 4 command, 7+1 `fs_*`.
`AllowedMCP = map[string][]string{}` — это существующее в кодовой базе
написание «никаких MCP» (`len == 0` → `break` в MCP-цикле
`buildTools`), а не `nil` («без ограничений»); написано верно.

Конструирование восьми `fs_*` инструментов безусловно (даже для
unscoped-вызова) безопасно: zero-value `permission.FolderScope` не даёт
ничего, а членство решается `slices.Contains(agent.AllowedTools, ...)`,
поэтому лишние конструкции инертны.

`2d9307965` (T10) — самое ценное здесь «footgun fix»: под
`RestrictedRun` пустая таблица `AllowTools` запрещает каждый
не-командный tool, поэтому scoped+restricted прогон запретил бы
собственную первую файловую операцию. `fsToolsForScope`
(`internal/app/app_run_gates.go:133`) дописывает ровно те `fs_*` имена,
что грантит scope. Асимметрия обработки ошибок обоснована верно:
`BuildRunAllowlist` логирует и дропает плохие паттерны (дроп только
СУЖАЕТ доступ), а `BuildFolderScope` падает целиком (дроп deny
carve-out'а РАСШИРИЛ бы доступ).

`4f56929f8` (T11) — самая аккуратная часть: byte-identity golden-тест
для unscoped-рендера плюс отдельный `TestGoldenFile_NoScopedBlock`,
который валит прогон, если сам golden был снят уже с условным блоком.
Это защита оракула от порчи, а не только от регрессии — редкий и
правильный паттерн. `withoutCallOptions` синхронно сбрасывает и
prompt-side флаг (`coordinator_models.go`), так что per-call подсказка
не протекает в глобальную публикацию.

`1b6b2b0e1` (T12) — persist/rebind scope через durable restart.
Направление fail-open/fail-closed выбрано пер-случай, а не копированием
формы `271550b1`, и обоснование выдерживает проверку: «spec есть, но
битый» → zero `FolderScope` (запрещает всё), «spec нет» → unscoped, так
как scoped-вызов может произвести только `ExecuteRun`, а он всегда
прикрепляет и `RunAllowlistSpec`. При этом сам T12 внёс дефект —
`rebuiltCallOptions = &CallOptions{FolderScope: &compiledScope}`
терял ВСЕ остальные поля `CallOptions` — который был найден внешним
ревью и закрыт в этом же диапазоне (`54a98e2bf`, R5-3). Отмечаю это как
свидетельство, что review-цикл здесь работает, а не как открытую
находку.

### CR-1 (P2, ЗАКРЫТО 2026-09-09): folder scope не снимает `rush_logs`

**ЗАКРЫТО** `7c9637381` (2026-09-07): `tools.RushLogsToolName` добавлен
в `folderScopeEscapeHatchTools`
(`internal/agent/coordinator_tools.go:317-322`). Текст ниже —
исторический: на момент написания находка была открыта.

`applyCallFolderScope` снимает legacy-file, escape-hatch, command и
негрантованные `fs_*` инструменты, но `rush_logs` не входит ни в один
из четырёх наборов:
`internal/agent/coordinator_tools.go:309` (`folderScopeEscapeHatchTools`
содержит только `download`, `git_read`, `agentic_fetch`,
`list_mcp_resources`, `read_mcp_resource`). При этом `rush_logs`
зарегистрирован в `allToolNames()` (`internal/config/config.go:832`),
то есть попадает в дефолтный `AllowedTools` coder'а, и конструируется
в `buildTools` для любой сессии с непустым `Options.DataDirectory`
(`internal/agent/coordinator_tools.go:691-693`).

Сценарий отказа: `sdk` (или `rush run`) запускает прогон против
РЕАЛЬНОГО workspace с `RunOverrides.FolderScopes = [{Dir: "src", Ops:
[read]}]` и без `RestrictedRun`. Модель получает `fs_read` со scope на
`src/`, и одновременно `rush_logs`, который открывает
`<DataDirectory>/logs/rush.log` обычным `os.Open` — файл заведомо вне
каждого scope-корня. `NewRushLogsTool` не принимает
`permission.Service` вообще, поэтому запроса разрешения нет даже
формально. Лог содержит строки других сессий и того же прогона:
редактируются только значения ключей из `sensitiveKeys`
(`internal/agent/tools/rush_logs.go`), но не пути, аргументы tool-call
и текст сообщений. Заявленный контракт scoped-режима («`fs_*` —
единственная файловая поверхность») нарушен.

Смягчающее: под `RestrictedRun` `rush_logs` отсутствует в
`runSpec.AllowTools` (туда дописываются только `fs_*` имена), поэтому
run-gate его отклонит; дыра открыта именно для scoped-прогона БЕЗ
`RestrictedRun`. Отмечу, что R15-1 (`abed8946`, вне диапазона) добавил
`rush_logs` в **no-real-workspace floor**, что не то же самое: floor
срабатывает только при `Options.NoRealWorkspace`, а не для
folder-scoped прогона на реальном workspace. На текущем HEAD находка
открыта.

Фикс на один символ: добавить `tools.RushLogsToolName` в
`folderScopeEscapeHatchTools`.

### CR-2 (P2, открыто): T9-отказ не покрывает worker-роль на credentials-пути

Commit message `916612573` утверждает: «`rejectScopedCallOnCLIProvider`
fires from all three model-resolution paths
(`resolveSessionModelsInternal`, `applyModelOverrides`,
`resolveCredentialsModels`) **for both the smart and worker roles**».
Код этого не делает. В `internal/agent/credentials.go:374` вызов один и
только для `"smart"`; worker-проверки там нет ни в исходном коммите, ни
на текущем HEAD (`grep` даёт 5 вызовов: по два в
`coordinator_models.go:217/225` и `:387/395`, один в `credentials.go`).

Сценарий отказа: `sdk.Client.RunWithCredentials` с
`RunOverrides.FolderScopes` и `CredentialSet`, покрывающим оба
обязательных слота (`RoleSmart`, `RoleFast`). Тогда
`resolveCredentialsModels` НЕ вызывает `resolveSessionModels` вовсе
(ветка `base` берётся только при
`AllowConfiguredRoleFallback && (!smartCovered || !fastCovered)`,
`credentials.go:350-357`), и конфигурационный Worker-слот никем не
проверяется. Если он указывает на `cliprovider.ProviderType`
(claude/codex/gemini/qwen), то делегирование субагенту внутри
scoped-хода (tool `agent` намеренно оставлен в scoped-toolset)
уезжает в CLI-подпроцесс, чьи файловые инструменты про
`FolderScope` ничего не знают — ровно тот молчаливый обход, ради
предотвращения которого T9 и существует. Отказа нет, ошибки нет.

### CR-10 (P3, открыто): T9-инвариант не проверяется на durable replay

`RebuildSessionAgentCall` восстанавливает модели напрямую через
`c.buildModelsFromCfg` (`internal/agent/coordinator_interrupt.go:500`),
минуя все три пути, где живёт `rejectScopedCallOnCLIProvider`. Smart-роль
берётся из персистированной строки, так что провайдер тот же, что был
при постановке в очередь; но worker-роль читается из ЖИВОГО конфига.
Сценарий: scoped-вызов durably-orphaned; между постановкой в очередь и
подъёмом pump'ом оператор переключил worker-слот на CLI-провайдера;
рестартованный scoped-ход поднимается, `RunSessionAgentCall`
перестраивает toolset через `pinCallTools`, но T9-отказ не срабатывает
— субагенты уходят в CLI-подпроцесс вне scope. Узко, но это
defense-in-depth дыра ровно того же класса, что CR-2.

---

## 2. `DiskProvider` (#856–#859)

**Статус: корректно.** Дизайн-инверсия («nil = реальный диск», в
противоположность zero-value `FolderScope` = «запретить всё») выбрана
осознанно и задокументирована в самом интерфейсе
(`internal/agent/tools/fs_provider.go`) — «нет осмысленного deny-all
диска». Контракт `Stat` (`errors.Is(err, fs.ErrNotExist)`) выписан с
перечислением пяти call sites, которые на нём ветвятся, и #857
действительно переводит их с `os.IsNotExist` на `errors.Is` — это не
косметика: `os.IsNotExist` не разворачивает `%w`-цепочки, поэтому
обёрнутая ошибка чужого провайдера была бы невидима.

`OSDisk()` — stateless singleton, раздаётся как значение; `List`/`Find`
делегируют в существующие `fsext.ListDirectory`/`globFiles`, `Search`
переиспользует `fsGrepSearchContext`, сохраняя РАЗНУЮ (несогласованную)
нормализацию текста двух движков намеренно и с объяснением почему
«причёсывать» её нельзя — существующие тесты пиннят рендер. Это
правильная приоритизация: сохранить наблюдаемое поведение при вводе
шва, а не улучшать его заодно.

Трёхслойный durable-отказ #859 разобран корректно и полно:
слой 1 — отказ у всех трёх продюсеров (`agent_ownership.go:437`,
`coordinator_interrupt.go:194` и `:339`), слой 2 — маркер
`SessionAgentCallData.HostDiskProvider` (`ToSessionAgentCallData`),
слой 3 — терминальный отказ потребителя (`RebuildSessionAgentCall`,
обёрнутый в `ErrCallAlreadyAttempted`, чтобы pump не крутил retry на
строке, которая никогда не станет исполнимой). Плюс `ExecuteRun`
принудительно ставит `FailIfSessionBusy` для provider-carrying прогона,
чтобы очередь вообще не возникала. Две hard-ошибки валидации
(`DiskProvider` без `FolderScopes`; `DiskProvider` со scope, который
держит command-tools) закрывают именно те две дыры, где модель видела
бы одновременно виртуальный и реальный диск.

### CR-3 (P2, открыто): `sessions inject --interrupt` — молчаливый no-op против DiskProvider-прогона

Слой 1 в `handleInterruptTick` (`internal/agent/coordinator_interrupt.go:194`)
отказывает, если построенный call несёт `DiskProvider`. Но call строится
через `buildCall(ctx, ...)`, который копирует `callOptionsFrom(ctx)`
целиком (`internal/agent/coordinator_run.go:110` и `:123`), а тикер
запускается из `runInternal` на контексте ИДУЩЕГО хода
(`internal/agent/coordinator_run.go:400`). Для SDK-прогона с
`RunOverrides.DiskProvider` этот контекст несёт `DiskProvider`.

Сценарий отказа: оператор из другого процесса делает
`rush sessions inject --interrupt <session> "<msg>"` во время
DiskProvider-прогона. Каждые 3 секунды (`interruptInjectTick`) тикер
пикает строку, строит call, натыкается на отказ, возвращает ошибку;
`startInterruptTicker` пишет `slog.Warn("interrupt-inject tick failed")`
и продолжает (`coordinator_interrupt.go:102-120`). Строка
`pending_injects` не потребляется никогда за весь ход. Прерывание не
срабатывает, CLI никакой обратной связи не получает, а в логе
накапливается по одной Warn-строке каждые 3 секунды до конца хода.

Направление (fail-closed, строка не теряется) выбрано верно — комментарий
про «оператор получает запись, а не потерю сообщения» честен. Проблема в
другом: interrupt-inject строит НОВЫЙ ход, которому чужой per-call
`DiskProvider` предыдущего хода не нужен вовсе. Правильнее было бы
обнулять `DiskProvider` (или весь `CallOptions`) при построении
interrupt-call, а не отказывать; тогда отказ слоя 1 остался бы
недостижимым, каким он и задумывался.

---

## 3. Волна R5 (7 находок)

**Статус: все семь закрыты корректно.** Ниже только то, что стоит
отметить сверх commit messages.

`a4361bb03` (R5-1, P0) — суть верна: `pinCallTools` возвращал `nil` при
внутреннем сбое, а три вызывающих сохраняли этот `nil` без сигнала
ошибки, из-за чего `runTurn` падал на process-shared toolset. Для
вызова с `FolderScope`/`DiskProvider` shared toolset — это ровно то
расширение прав, которое вызов и просил убрать; а поскольку SDK-прогон
авто-аппрувит каждый tool-call, ловить это ниже по стеку некому.
Тест-seam `pinCallToolsReadyGateSeam` детерминирован (срабатывает на той
же горутине между `buildTools` и `readyWg.Wait()`), а не «пытается
поймать гонку».

`e567dd48a` (R5-2, P0) — центральное решение: и корни scope, и
запрошенные пути теперь проходят ОДИН алгоритм
(`resolveScopedPath`: longest-existing-prefix + `EvalSymlinks` +
литеральный хвост) через ОДИН и тот же `DiskProvider`. Персистится при
этом СЫРОЙ spec, а канонизация повторяется на каждом durable-рестарте —
и это правильно: канонизированный снимок протух бы при изменении
symlink-структуры на диске. Остаточная TOCTOU-граница честно выписана в
doc-комментарии `CanonicalizeFolderScopeSpec`, а не замолчана.

`54a98e2bf` (R5-3, P1) — `session.CallOptionsSpec` версионирован с
самого начала, и исключённые поля перечислены с обоснованием
поштучно (`MaxCost`/`MaxTokens` персистятся отдельно; `AllowPeakHours`/
`FailIfSessionBusy` — одноразовые admission-time решения, для
повторного проигрывания бессмысленные; `DiskProvider` не сериализуем).
Отдельно правильно: `TimeoutOptionsSet` round-trip'ится сам по себе,
чтобы «ноль намеренно» отличался от «legacy-строка без таймаутов».

`451f8f8cc` (R5-4, P1) — находка нетривиальная и её нельзя было поймать
in-tree тестом: `internal/agent` неимпортируем снаружи модуля, поэтому
`errors.Is(err, agent.ErrSessionBusy)` внешний потребитель просто не
скомпилирует. Тест живёт во внешнем модуле `testdata/embedder` (свой
`go.mod` + `replace`), и удаление алиасов ломает КОМПИЛЯЦИЮ, а не
ассерт — оракул сильнее обычного.

`83ca7e831` (R5-5, P1) — самая опасная из R5 по последствиям:
`if oldBytes, err := disk.ReadFile(...); err == nil` молча превращал
ошибку чтения в пустой `oldContent` и всё равно писал файл, из-за чего
diff выглядел как «создан из пустого», а undo-базлайн в history был
ложным. Тест `TestFSWriteFailedSnapshotReadBlocksWrite` невакуумен по
построению: считает вызовы `WriteFile` (0), вызовы `history.Create`/
`CreateVersion` (0) и содержимое файла на диске (`"old"`) — то есть
проверяет отсутствие побочного эффекта, а не только текст ошибки.

`336b81c0c` (R5-6, P1) — здесь самое ценное не сам фикс, а
зафиксированная в commit message регрессия СОБСТВЕННОГО первого драфта:
принять `filepathext.SmartIsAbs(dir)` как «уже разрешён» было бы
верно для virtual root, но на Windows это истинно и для `{Dir: "/foo"}`
под реальным `D:\project`, из-за чего существующий deny carve-out стал
бы driveless и НЕДОСТИЖИМЫМ — то есть инертным. Это ровно класс
fail-open, который R5-2 и закрывал, внесённый заново другим механизмом.
Итоговое условие
(`internal/permission/folderscope.go`, `alreadyAbsInThisNamespace`)
гейтится ещё и на namespace самого `spec.WorkingDir` и закреплено
отдельным тестом.

`159762103` (R5-7/R5-8, P2) — `Origin` терялся потому, что
`RebuildSessionAgentCall` строит свой литерал `SessionAgentCall{}`
вместо делегирования в `FromSessionAgentCallData`. Это, кстати,
структурная слабость, а не единичный промах: любое новое поле
`SessionAgentCall`, добавленное в оба конвертера, придётся отдельно
вспомнить и здесь. Тест гоняет все четыре значения `message.Origin`
через настоящий JSON round-trip, а не in-memory копию.

---

## 4. Волна R6 (4 находки)

**Статус: закрыты корректно.** Один остаточный контрактный зазор (CR-7)
и один перформанс-эффект (CR-8).

`2eff9f74b` (R6-1, P0) — находка настоящая и крупная: ephemeral
`sdk.ModeLibrary` рекламировал «файлы не трогаются», но выдавал модели
весь legacy host-disk набор, укоренённый в строку, которую ОС всё ещё
интерпретирует как реальный путь. Три независимых изменения (default-deny
список, per-OS `LibraryVirtualRoot`, fail-closed перед первым реальным
`disk.Stat`) закрывают её с трёх сторон. Отдельно ценно, что commit
message явно признаёт пропуск в первом драфте (`download` писал прямо в
реальную ФС, шва `DiskProvider` у него нет вообще) и что пропуск
подтверждён revert-check'ом с РЕАЛЬНЫМ сетевым dial'ом до фикса.

`rejectRealDiskUnderLibraryVirtualRoot` сравнивает `disk != OSDisk()`.
Это безопасно с точки зрения Go-семантики: динамический тип
`OSDisk()` — неэкспортированный `tools.osDisk`, поэтому чужая
реализация никогда не совпадёт по типу и сравнение интерфейсов не может
паниковать на «uncomparable type» (паника возможна только при равных
динамических типах). Побочный эффект — декоратор, оборачивающий
`OSDisk()` (а `sdk.OSDiskProvider()` в doc-комментарии прямо предлагает
так делать: «real disk, but log every write»), проходит guard насквозь.
Это соответствует заявленному контракту («кастомный провайдер — дело
вызывающего»), так что не находка, но стоит помнить при чтении
`IsCustomDiskProvider` (появившегося уже вне диапазона).

`87e9c72cb` (R6-2, P1) — тот же namespace-mismatch, что R5-2, но на
стороне РЕЗУЛЬТАТОВ: `fastwalk` с `Follow: true` пересекает
directory-symlink'и и сохраняет alias-написание, поэтому лексическая
проверка `filepath.Clean(f)` против канонизированного scope позволяла
обойти deny carve-out через алиас. Revert-check описан конкретно (тест
на fake-провайдере валится с утечкой `secret.txt`). Рендер намеренно
сохраняет исходное написание, а решение принимается по разрешённому —
это правильный компромисс (`createFileTree` требует литерального
префикса `Dir`).

`3e84a240d` (R6-3, P1) — правильный рефакторинг направления: решение
fail-closed переехало ВНУТРЬ `pinCallTools`, вместо того чтобы каждый
из четырёх call sites помнил про `scopedCallToolsRequired(ctx)` — что
как раз и было причиной находки (durable-replay call site про
`DisableSubAgents`/`ModelRole` расширен не был). Предикат теперь один
(`scopedCallOptionsRequireDistinctTools`) и используется и как
ctx-форма, и как struct-форма.

`8d4fd702f` (R6-4, P2) — `CallOptionsSpecVersion` был write-only
метаданными; теперь несовпадение версии — отказ, а не частичный декод.
Обоснование «rollback / mixed-version deployment» реалистично.

### CR-7 (P3, открыто): успешная сборка с пустым toolset снова fail-open

R5-1 трактовал `resolved.tools == nil` как отказ для scoped-вызова.
R6-3 заменил это на «отказ ровно тогда, когда `pinCallTools` вернул
ошибку». Но `buildTools` возвращает `filteredTools`, объявленный как
`var filteredTools []fantasy.AgentTool` и заполняемый только `append`:
если ни один сконструированный tool не попал в `AllowedTools`, он
остаётся `nil`, ошибки нет, и `pinCallTools` вернёт `(nil, nil)`
(`internal/agent/coordinator_models.go:578-608`). Вызывающий положит
`nil` в `resolved.tools`, а `nil` `call.Tools` на старте хода
разворачивается в SHARED (unscoped) toolset.

То есть для scoped-вызова «успешно собрали пустой набор» снова означает
молчаливое расширение до shared toolset, тогда как до R6-3 это был
отказ. На сегодняшнем коде состояние практически недостижимо (после
`applyCallFolderScope` в списке всегда остаются как минимум
не-файловые `todos`/`ask_question`/`agent`), поэтому P3, а не выше —
но контракт ослаблен, и достаточно одного будущего расширения floor'а
(который как раз и вычищает списки), чтобы сделать его достижимым.
Дешёвая страховка: в `pinCallTools` возвращать ошибку и при
`len(tools) == 0 && required`.

### CR-8 (P3, перформанс): `fs_list`/`fs_find` резолвят каждый результат без кэша

`internal/agent/tools/fs_list.go:165` и
`internal/agent/tools/fs_find.go:135` вызывают `resolveScopedPath` на
КАЖДЫЙ элемент результата. Каждый такой вызов делает как минимум один
`Stat` (walk-up по несуществующим компонентам — больше) и один
`EvalSymlinks`, который сам по себе обходит всю цепочку предков
покомпонентно.

Конкретные границы: `fs_list` берёт `Limit = maxLSFiles = 1000`
(`internal/agent/tools/ls.go:53`, `fs_list.go:142`), а батч допускает
`FSBatchMaxItems = 50` (`internal/agent/tools/fs_batch.go:78`). То есть
худший случай одного вызова `fs_list` — 50 000 резолвов, каждый с
полным `EvalSymlinks` глубины пути; на Windows это сотни тысяч
syscall'ов на один tool-call. `fs_grep` в аналогичном месте держит
`resolvedCache` (`internal/agent/tools/fs_grep.go:532`), у `fs_list`/
`fs_find` кэша нет. Поскольку записи одного листинга делят почти весь
префикс, мемоизация по родительскому каталогу убрала бы почти всю
стоимость. Корректность при этом не страдает — это чисто стоимость.

### CR-9 (P3, дрейф): две независимые копии op→tool маппинга

`internal/app/app_run_gates.go:133` (`fsToolsForScope`) и
`internal/agent/coordinator_tools.go:329` (`folderScopeOpForTool`)
описывают одно и то же соответствие «операция → имя `fs_*` tool'а», и
комментарии в обоих местах утверждают, что они «mirror exactly».
Механической сверки нет: `folderScopeOpForTool` неэкспортирован, а
`TestFSToolsForScopeGrantsMirrorOps`
(`internal/app/app_run_folder_scope_test.go`) хардкодит ожидания
списком. При добавлении девятой операции и забытой второй правке
scoped+restricted прогон получит tool в toolset'е, который run-gate
отклонит (или наоборот) — тихая рассинхронизация, которую ни один
текущий тест не поймает.

---

## 5. R14: единая no-real-workspace capability

`eab5d508a` вводит `config.Options.NoRealWorkspace` (`json:"-"`,
выводится один раз в `buildLibraryConfig`) как единственный
авторитетный сигнал, и это правильный ответ на настоящую находку:
`Options.DisabledTools` фильтрует только НАЧАЛЬНЫЙ `AllowedTools`, а
`buildToolsAgentConfigForCall` дописывает `workerToolNames`
(`write`/`bash`/`download`) поверх, вообще не сверяясь с disabled-списком.
`applyNoRealWorkspaceToolFloor` поставлен последним изменением
`AllowedTools` в `buildTools` — после worker-layering и после
folder-scope re-adds — что и делает список полом, а не разовым фильтром.

R14-3 (`var LibraryVirtualRoot` → `func LibraryVirtualRoot()`) — верная
диагностика: `var X = tools.Y` в Go это КОПИЯ значения на init, не
алиас, поэтому embedder мог переприсвоить SDK-сторону и развести её с
внутренним guard'ом. Функция читает внутреннее значение живьём, так что
расхождение стало невозможным, а присваивание — ошибкой компиляции.
Ломающее изменение экспортированного символа задокументировано и в
commit message, и в `sdk/README.md`.

### CR-4 (P1 на конце диапазона, закрыто вне диапазона): floor не содержал `git_read`/`agentic_fetch`/`rush_logs`

В состоянии `eab5d508a`/`1d3cdf4a7` `noRealWorkspaceForbiddenTools`
содержал ровно 10 имён: `bash`, `run_command`, `download`, `edit`,
`multiedit`, `write`, `view`, `glob`, `grep`, `ls`. Не входили:

- `git_read` — запускает реальный бинарник `git` как OS-подпроцесс с
  `cmd.Dir = c.cfg.WorkingDir()`, для такой сессии — sentinel, реальный
  интерпретируемый ОС путь; шва `DiskProvider` у него нет вовсе;
- `agentic_fetch` — строит собственные unscoped `view`/`glob`/`grep` во
  временном каталоге реального диска;
- `rush_logs` — читает `<DataDirectory>/logs/rush.log` обычным
  `os.Open`; при пустом `DataDirectory` путь схлопывается в
  ОТНОСИТЕЛЬНЫЙ `logs/rush.log` и резолвится против cwd host-процесса.

То есть заявленная в `sdk/README.md` гарантия «ephemeral-сессия не
трогает файлы» на конце моего диапазона не выполнялась. Закрыто уже
после диапазона: `abed8946` (R15-1, `rush_logs`) и `31ea604f`
(R15-2/R14-7, `git_read` + `agentic_fetch`), оба от 2026-09-04. Факт
закрытия сверен с `docs/reviews/2026-09-05-2047-commit-review-36h.md`
(раздел 1) и подтверждён чтением текущего
`internal/agent/coordinator_tools.go:427-435`. Оставляю в отчёте, так
как это состояние именно того merge-коммита, которым мой диапазон
заканчивается.

---

## 6. Память, goroutine'ы, ресурсы

**Статус: этот блок — сильнейшая часть диапазона.**

`53aa30d95` — три теста `final_composition_test.go` подменяли
глобальный `slog` default на локальный `bytes.Buffer` и не
восстанавливали его. После их прогона каждый `slog.Info/Warn/Error` из
любой горутины до конца жизни тест-бинаря писал в буфер, который никто
не читает и не освобождает.

`cec10b5d2` — четыре независимых причины, и первая из них по-настоящему
нетривиальна. Утверждение коммита: `slog.SetDefault(prev)` сам по себе
НЕ откатывает вывод пакета `log`, потому что `SetDefault` вызывает
`log.SetOutput` только когда НОВЫЙ handler не является
`*defaultHandler`. Я проверил это по семантике stdlib и подтверждаю:
восстановление на `defaultHandler`-backed логгер (а именно такой стоит
по умолчанию в начале процесса) пропускает `log.SetOutput`, писатель
`log` остаётся направленным на мёртвый локальный буфер, а
`defaultHandler.Handle` маршрутизирует туда каждый последующий
`slog`-вызов. Это одновременно неограниченный сток памяти и ослепление
stderr. Фикс (сохранение/восстановление `log.Writer()`/`log.Flags()`)
применён во всех девяти местах с этим паттерном.

Причина 2 — реальный hang, а не теоретический: `fireCacheKeepAlive`
строил replay-агента без `fantasy.WithStopConditions`, а отказ
`noExecuteTool` — это НОРМАЛЬНЫЙ tool-ответ, не Go-ошибка, поэтому
модель, продолжающая выбирать тот же tool, гонит неограниченный
ping-pong. `fantasy.StepCountIs(2)` — правильная граница для фонового
пинга без наблюдателя. Диагностика (жёсткий 4 GB job-object cap,
принудительный крэш со стеком нужной горутины) описана конкретно и
воспроизводимо.

Причина 4 — `pump.Stop()` перед `db.Release()`: обоснование верное
(два `require.Eventually` выше доказывают лишь, что ход НАЧАЛ писать),
и симптом Windows-специфичен (`RemoveAll` в `t.TempDir()` падает с
«process cannot access the file», а не DB-ошибкой). Замена
`close(cleanupUnblock)`-teardown на atomic-`testDone` guard разобрана
особенно аккуратно: в комментарии явно объясняется, почему WaitGroup
здесь НЕ подходит (потребовалось бы «Add happens before Wait» для
каждого вызова), и почему худший исход выбранного варианта — потерянная
лог-строка, а не паника всего бинаря.

Причина 3 (пейсинг busy-spin горутин на 10 мкс) — единственная,
которую я бы отметил как чуть более слабую: она смягчает симптом
(вытеснение планировщика под внешним давлением), а не устраняет
структурную причину. Но заявлено это честно («amplifying GC/scheduler
pressure»), и структурная гарантия теста (число toggle'ов всё ещё
многократно превышает потребность основного цикла) сохранена.

Новых утечек goroutine/ресурсов в диапазоне не найдено. Тикеры
interrupt-inject в обоих call site'ах (`coordinator_run.go:399-409`,
`coordinator_interrupt.go:761-769`) корректно закрываются парой
`stopTicker(); <-tickerDone` в ОДНОМ deferred-замыкании, с явным
комментарием про LIFO-порядок defer'ов — их нельзя случайно
переставить вставкой третьего defer между ними.

---

## 7. CI, стиль, документация

`73878311c` — оба падения диагностированы верно как баги ТЕСТОВ, а не
регрессии кода, и лечатся по-разному в зависимости от природы:
symlink-тест приводится к контракту `Check` (путь должен приходить
уже разрешённым), а Windows-специфичный тест уезжает под
`//go:build windows` вместо того, чтобы «переформулировать его под
семантику, которой на Unix не существует». Второе решение особенно
правильное: сценарий `SmartIsAbs`-но-не-`IsAbs` на Unix физически
недостижим.

`e5869ef3e` — документация, которая УМЕНЬШАЕТ заявленную гарантию:
явно сказано, что worktree-изоляция `/wrush` это конвенция
slash-command'а, исполняемая LLM-оркестратором, и что в бинаре `rush`
нет проверки, что cwd действительно под `<repo-root>/worktrees/`. Это
ровно тот вид честности, которого обычно не хватает в README.

### CR-6 (P3, открыто): gofumpt-коммит испортил doc-комментарий

`1092f3285` описан как «pure comment reformatting, no logic change».
Логики он действительно не менял, но результат читается неверно.
Исходная строка продолжения `//     + permission.BuildFolderScope) and
kept separate...` начиналась с `+`, который нормализатор doc-комментариев
принял за маркер списка и переписал в `-`, породив бессмысленный новый
пункт. На текущем HEAD
(`internal/session/session_runqueue.go:128-131`, битая строка — `:130`)
фраза выглядит так:

```
//   - FolderScope has its own dedicated spec (FolderScopeSpec above),
//     compiled through a different path (tools.CanonicalizeFolderScopeSpec
//   - permission.BuildFolderScope) and kept separate rather than folded
//     in here.
```

Первое предложение оборвано на незакрытой скобке, а второй «пункт»
начинается с `permission.BuildFolderScope)`. Механическое принятие
вывода форматтера здесь заменило верный текст на неверный; правильная
правка — перенести `+` так, чтобы он не стоял в начале строки.

### CR-5 (P3, traceability): коллизия идентификаторов R14-x

`8ed45150b` (15:20) добавляет `docs/reviews/2026-09-03-round-14-1328.md`,
где R14-1 = «утечка `log.Writer` в
`cliprovider/provider_security_test.go`» (P3, помечена **открытой**),
R14-2 = «sibling path не resolved в symlink-тесте» (закрыта),
R14-3 = «Windows-only тест ломал Unix CI» (закрыта).

Через час `eab5d508a` (16:23) и merge `1d3cdf4a7` (16:34) заявляют, что
исправляют «R14-1/R14-2/R14-3» — но имея в виду СОВЕРШЕННО другие
находки: tool-floor, precondition на `FolderScopes` и
`var`→`func` для `LibraryVirtualRoot`. Их источник —
`docs/reviews/2026-09-03-sdk-library-review-round-14-0928.md`, который
закоммичен отдельно (`c5ee32070`) и **не является предком
`1d3cdf4a7`**: `git ls-tree 1d3cdf4a7 docs/reviews/` показывает только
`round-11`/`12`/`13`/`14-1328`.

Практическое следствие: читатель, идущий от merge-коммита к
единственному доступному в дереве R14-документу, получает не те находки.
Хуже того, R14-1 в этом документе помечена ОТКРЫТОЙ, а commit message
говорит «R14-1 ... fixed» — при том, что `eab5d508a` не трогает
`cliprovider` вообще. Настоящая утечка `log.Writer` в
`cliprovider/provider_security_test.go` закрыта только позже и уже под
третьим номером — `61cc0773a` («R14-6»), вне диапазона. Три
несовместимых нумерации «R14-x» в одной директории `docs/reviews/` за
двое суток.

### CR-11 (P3, закрыто внутри диапазона): временно оторванный doc-комментарий

`8d4fd702f` вставил новую функцию `callOptionsFromCallData` МЕЖДУ
doc-блоком `FromSessionAgentCallData` и самой функцией. В результате
экспортированная `FromSessionAgentCallData` на 3 часа осталась вовсе
без godoc, а `callOptionsFromCallData` получила склейку из чужого и
своего комментария. Закрыто в этом же диапазоне: `4ef3ebcdf` (R13-1)
удалил хелпер и вернул комментарий на место
(`internal/agent/call_data_conversion.go:237-261`).

### CR-12 (P3, закрыто вне диапазона): устаревшая константа в перенесённом тесте

`73878311c` вынес `TestBuildFolderScope_VirtualRootEntryStaysUnjoined`
в новый файл с комментарием «`sdk.LibraryVirtualRoot`'s shape» и
хардкодом `"/rush-library-mode-root"` — при том, что R6-1
(`2eff9f74b`, четырьмя часами ранее в этом же диапазоне) уже сделал
`LibraryVirtualRoot` per-OS, и на Windows он равен
`K:\rush-library-mode-root`. Тест оставался валидным как property-тест
driveless-namespace, но его название и комментарий вводили в
заблуждение. На текущем HEAD исправлено (константа переименована в
`legacyDrivelessVirtualRoot` с пояснением,
`internal/permission/r5_6_virtual_root_regression_test.go:39`) —
коммитом вне диапазона.

---

## Открытые находки

| ID | Severity | Находка | Где | Статус |
|---|---|---|---|---|
| CR-1 | P2 | folder scope не снимает `rush_logs` → чтение host-лога вне scope | `internal/agent/coordinator_tools.go:309` | **ЗАКРЫТО** `7c9637381`: `rush_logs` в `folderScopeEscapeHatchTools` (сверено 2026-09-09) |
| CR-2 | P2 | T9-отказ не покрывает worker-роль на credentials-пути (расходится с commit message) | `internal/agent/credentials.go:374` | открыто на HEAD |
| CR-3 | P2 | `inject --interrupt` — молчаливый no-op против DiskProvider-прогона, Warn каждые 3 с | `internal/agent/coordinator_interrupt.go:194`, `internal/agent/coordinator_run.go:110` | открыто на HEAD |
| CR-4 | P1 | floor без `git_read`/`agentic_fetch`/`rush_logs` на конце диапазона | `internal/agent/coordinator_tools.go:427` @ `eab5d508a` | закрыто вне диапазона (`abed8946`, `31ea604f`) |
| CR-7 | P3 | успешная сборка пустого toolset → fail-open к shared toolset | `internal/agent/coordinator_models.go:578-608` | открыто на HEAD |
| CR-8 | P3 | `fs_list`/`fs_find`: до 50 000 некэшированных `resolveScopedPath` на вызов | `internal/agent/tools/fs_list.go:165`, `fs_find.go:135` | открыто на HEAD |
| CR-9 | P3 | две несверяемые копии op→tool маппинга | `internal/app/app_run_gates.go:133`, `internal/agent/coordinator_tools.go:329` | открыто на HEAD |
| CR-10 | P3 | T9-инвариант не проверяется на durable replay | `internal/agent/coordinator_interrupt.go:500` | открыто на HEAD |
| CR-5 | P3 | три несовместимые нумерации «R14-x»; merge ссылается на документ, отсутствующий в дереве | `docs/reviews/2026-09-03-round-14-1328.md` | открыто |
| CR-6 | P3 | gofumpt-коммит породил бессмысленный пункт списка в doc-комментарии | `internal/session/session_runqueue.go:130` | открыто на HEAD |
| CR-11 | P3 | doc-блок временно оторван от `FromSessionAgentCallData` | `internal/agent/call_data_conversion.go` | закрыто в диапазоне (`4ef3ebcdf`) |
| CR-12 | P3 | устаревшая константа/комментарий virtual-root в перенесённом тесте | `internal/permission/r5_6_virtual_root_regression_test.go` | закрыто вне диапазона |

P0: 0. P1: 1 (закрыт вне диапазона). P2: 3 открытых. P3: 8.

---

## Итоговый вердикт

Диапазон качественный и необычно дисциплинированный. Четыре merge-коммита
проверены машинно и полностью чисты — ни один не содержит ручных правок
поверх автоматического 3-way результата, так что потерянных hunks и
ошибок разрешения конфликтов в них нет по построению. Обе волны
remediation (R5, R6) реально закрывают заявленные находки: я выборочно
перепроверил механизм каждой из одиннадцати и не нашёл ни одного случая,
где diff не делает того, что обещает commit message, — за единственным
исключением T9 (CR-2), где формулировка «for both the smart and worker
roles» на одном из трёх путей просто не соответствует коду.

Особенно стоит отметить три вещи. Первая — качество тестовых оракулов:
byte-identity golden с отдельным тестом «golden не испорчен» (T11),
внешний модуль, который ЛОМАЕТСЯ НА КОМПИЛЯЦИИ при удалении алиасов
(R5-4), проверка отсутствия побочного эффекта, а не текста ошибки (R5-5),
детерминированный seam вместо попытки поймать гонку (R5-1). Вторая —
готовность фиксировать в истории собственные промахи: регрессия
первого драфта R5-6, пропущенный `download` в R6-1, и, отдельно,
merge-message `1d3cdf4a7`, где автор опровергает свою же находку R14-2
после revert-check'а (я перепроверил — опровержение верное). Третья —
разбор утечки памяти в `cec10b5d2`: диагностика поведения
`slog.SetDefault` относительно `log.SetOutput` — это именно тот класс
ошибки, который не находят чтением, и он найден и объяснён точно.

Блокирующего нет: P0 в диапазоне не осталось, единственный P1 (CR-4)
закрыт последующими коммитами вне диапазона. Разумный ближайший
приоритет — три P2: добавить `rush_logs` в `folderScopeEscapeHatchTools`
(CR-1, правка в одну строку с наибольшим отношением пользы к риску),
добавить worker-проверку в `resolveCredentialsModels` (CR-2), и
обнулять `DiskProvider` при построении interrupt-inject call'а вместо
вечного отказа (CR-3). Остальное — P3, ждёт. Сверка 2026-09-09: CR-1 закрыт коммитом `7c9637381`.
