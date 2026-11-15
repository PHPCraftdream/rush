# Спринт 0, D1: дизайн разрезки `internal/agent/tools/mcp` и `internal/agent/tools`

Задача: #74 (D1-mcp), ворота Г1, решение В10. Дата: 2026-11-15.
Источники: план спринта (`docs/plans/2026-11-15-sprint0-plan.md`, §3, §5 «D1»/«0b», §6 В10, §11), исследование (`docs/plans/2026-11-15-sprint0-modules-research.md`, И§2–§5, И§9, И§11), реестр инвариантов `docs/mcp-invariants.md` (25 законов), `CLAUDE.md` (раздел «The MCP Subsystem»: решение «12 глобалов не мигрируют на `Owner`» — окончательное, не предлагать).
Данные: скрипты `.rush/tmp/sprint0/` (вне репозитория): `analyze.py clusters|imports`, `entangle.py`. `prereq.py` шесть больших пакетов покрывает, `mcp` и `tools` — нет (проверено: секций `internal/agent/tools*` в его выводе нет); для них использованы `analyze.py` и `entangle.py`, важные факты проверены чтением кода (ссылки `файл:символ`).
Этот документ — только дизайн. Кода в этой задаче нет; исполнение — полосы волны 5 (D2) после 0б.

## 0. Резюме решения

- `internal/agent/tools/mcp` (ядро с состоянием) **остаётся пакетом `mcp` на прежнем пути** — все 25 инвариантов, чьи якоря указывают на файлы ядра, не переякориваются. Единственный вынос — бессостояный транспортный слой в новый подпакет `mcp/mcpdial` (вне `mcp/internal/`, чтобы не попадать под Go-правило internal-видимости; само `mcp/internal/contextlock` не меняется). Ядро после выноса — 16 файлов + подкаталог `internal` = **17 элементов** → запись в списке-храповике каталогов с обоснованием (В10 прямо разрешает остаток ядра сверх 7 в храповике).
- `internal/agent/tools` (77 рукописных файлов) режется на **7 подкаталогов без единого файла в родителе**: `toolcontract`, `fs`, `shell`, `web`, `mcptools`, `orchestrate`, `hooks`. Родитель держит только подкаталоги; единого фасадного пакета нет — импортёры (138 файлов: 54 код + 84 тест) механически переезжают на импорты листьев. Фасад отвергнут сознательно: он сохранил бы линковку всех tools-пакетов каждым импортёром и обнулил бы главную цель спринта (сужение тестовой матрицы, И§2).
- Циклов между будущими подпакетами нет ни в `mcp`, ни в `tools`; швы (их четыре, §3) — это подъём видимости существующих функций и перенос общих помощников, а не новые порты. Полосы «0b» для обоих пакетов вырождаются в перенос+подъём видимости внутри одной полосы.
- Реестр инвариантов переякоривается в том же коммите, что и переезд: меняются якоря только INV-20, INV-21 (Where) и колонка «File» у ~10 тестов из карты «Test → invariant» (§5.3).

## 1. Текущее состояние

### 1.1. `internal/agent/tools/mcp`

Размер (измерено `wc -l` по HEAD ветки): 18 рукописных файлов, 8 470 строк кода; 45 тестовых файлов, 14 324 строки (совпадает с И§1). Базовое число тестов по реестру: 230 записей `go test -list` = 229 реальных тестов + 2 субпроцессных помощника с именами `Test*` (`TestMCPStdioHelper`, `TestMCPStdioDiagnosticHelper`); счёт по платформам не меняется из-за взаимно исключающих build-тегов (`process_unix_test.go` / `process_windows_test.go`).

Кластеры (`analyze.py clusters internal/agent/tools/mcp 60`, код/тесты/коммитов за 90 дней):

| Кластер | Код | Тесты | c90 | | Кластер | Код | Тесты | c90 |
|---|---|---|---|---|---|---|---|---|
| mutation | 967 | 0 | 7 | | transport | 460 | 150 | 3 |
| addreplace | 906 | 0 | 3 | | init | 443 | 1495 | 74 |
| lease | 902 | 512 | 11 | | refresh | 381 | 203 | 5 |
| owner | 881 | 259 | 6 | | resources | 289 | 0 | 9 |
| admission | 864 | 972 | 11 | | trusted | 214 | 512 | 3 |
| session | 831 | 58 | 3 | | uncertainty | 260 | 180 | 5 |
| tools | 461 | 117 | 10 | | prompts | 227 | 0 | 8 |
| publish | 257 | 0 | 3 | | process (4 файла) | 80 | 233 | 9 |
| output | 47 | 43 | 1 | | прочее (тесты) | 0 | ~5800 | — |

Сцепление кластеров (`entangle.py internal/agent/tools/mcp 12 300`), исходящие рёбра (различные идентификаторы, взятые из чужого кластера): `mutation→owner:17, admission:8`; `addreplace→owner:13, init:7, mutation:6`; `lease→owner:11, init:8, admission:6`; `owner→init:11`; `admission→owner:15, init:7`; `session→owner:15, admission:7`; `tools→owner:8, lease:5`; `transport→process:1, owner:1`; `init→owner:10, admission:9`; `refresh→owner:10, init:6`. Входящие к `owner` — от всех десяти кластеров (5–17 идентификаторов): ядро — один ком вокруг пары «глобалы + Owner» (И§11.2, та же картина, что в шести больших пакетах).

Вывод о разрезаемости: разрезка по кластерам (`owner`, `admission`, `lease`, …) невозможна без миграции глобалов, которая запрещена (`CLAUDE.md`; решение подтверждено стоимостью в `docs/plans/mcp-ownership-design.md` — 33 строки выгоды). Состояние концентрировано в 13 пакетных переменных (§4.1); бессостояная часть — ровно четыре файла: `transport.go`, `process_unix.go`, `process_other.go`, `output_admission.go`.

Проверка чтением кода (не скриптом):

- `transport.go` **не содержит** обращений ни к одной из 13 глобалов: grep по `\b(owner|lifecycleMu|sessions|states|stateOwners|broker|leases|allTools|allPrompts|allResources|generation|initDone|standaloneOwners)\b` по файлу пуст (единственное ребро `transport→owner:1` из `entangle.py` — ложное срабатывание по слову в комментарии).
- `session.go` глобалы трогает: `sessions.Get/Set` (session.go:387–389, в `createSessionWithAdmission`), `owner` (session.go:451, в `WaitForInit`-подобной проверке текущего владельца), `lifecycleMu` (session.go:450–457).
- `uncertainty.go` трогает `lifecycleMu` (:31, :100–101, :170–172) и глобал `owner` (:101, :171, :191).
- `output_admission.go` (47 строк): только context-key и `admitToolText` — глобалов нет, но это 1 файл, ради которого не создаётся пакет; остаётся в ядре.
- `init()`: в коде пакета нет ни одного (grep `func init(` по не-тестам пуст); единственный `init()` — тестовый, `ktav_testgate_test.go:31`.
- `go:embed`: нет ни одного.
- OS-файлы: пара `process_unix.go` (`//go:build !windows`, `configureStdioProcess`) и `process_other.go` (`//go:build windows`, тот же символ); тестовые пары `process_unix_test.go` / `process_windows_test.go` (последний содержит ещё и `TestMain` — см. §4.6).
- Импортёры пакета (prod): `internal/agent` (agent_turn.go, cache_prefix_mcp.go, coordinator.go, coordinator_providers.go), `internal/app/app.go`, `internal/cmd/mcp.go`, `internal/commands/commands.go`, `internal/server/events.go`, `sdk/sdk.go`, плюс обёртки в `internal/agent/tools` (mcptools-кластер). Замечание к И: скрипт `analyze.py imports` показывал ребро `mcp → internal/session`; по prod-файлам его нет (grep по импорту пуст — `session.*` в коде ядра это методы SDK-типа `*mcp.ClientSession`), ребро приходит из тестов.

### 1.2. `internal/agent/tools`

Размер (измерено; И§1 даёт 79 файлов кода / 15 202 строки — расхождение +2 файла к измеренным 77 объясняется подсчётом на другом HEAD, в таблицах ниже полный список из 77): 77 рукописных `.go`-файлов, ~15,2 тыс. строк; 84 тестовых файла; 40 директив `go:embed` (по одной на файл инструмента, файлы `.md`/`.md.tpl` едут вместе с кодом, §4.4); `testdata/grep.txt` — тестовые данные grep-кластера. Импортируют пакет: `internal/agent`, `internal/app`, `internal/proto`, `sdk` — 138 файлов (54 prod + 84 тест, измерено grep по пути импорта).

Кластеры (`analyze.py clusters internal/agent/tools 60`; код/тесты/c90): fs 32ф/3685+4059/26, grep 8ф/1031+1495/19, view 4ф/1016+975/14, rush 4ф/928+1064/3, read 5ф/881+981/15, run 11ф/726+1242/14, bash 9ф/567+996/24, edit 7ф/581+459/9, multiedit 389+522/6, glob 352+157/4, fetch 451/4, sourcegraph 332+108/3, git 276+439/3, ssrf 193+352/2, ls 273/3, search(движок DDG) 256/0, write 240+180/6, safe 251+79/3, regular 265/1, os 103+228/1, mcp-tools 229+80/7, mcp 208+304/3, todos 235+222/2, download 269+190/6, wake 199+175/1, ask 128+169/4, await 137+112/4, web 137+74/0, tools(tools.go) 177/1, agent_control 77+232/7, rg 93+50/4, timeout 40+45/2, command 24/1, inject/inspect/stop/wakein/wakeon — по 38–73 строк.

Сцепление (`entangle.py internal/agent/tools 12 300`), исходящие рёбра кластеров ≥300 строк кода: `fs→grep:16, view:5, edit:4, tools:3, rg:3, ls:3, glob:3, write:2, search:1, read:1, os:1, mcp-tools:1`; `view→tools:5, fs:5, read:3, regular:2, os:1`; `edit→tools:2, fs:2, forbidden:2, os:1`; `multiedit→edit:5, tools:2, forbidden:2, os:1`; `grep→tools:2, rg:2, read:1`; `glob→tools:2, rg:1, read:1, fs:1`; `run→tools:3, bash:3, command:2, timeout:1, git:1`; `bash→tools:2, command:2, timeout:1, safe:1, rg:1, mcp:1`; `fetch→tools:4, ssrf:1`; `web→fetch:6`; `sourcegraph→tools:1, ssrf:1, grep:1`; `rush→tools:1`; `read→tools:2, mcp:1`; `ls→fs:1`.

Проверка ключевых рёбер чтением кода (скрипт считает и совпадения имён методов — часть рёбер шум):

- `fs→grep:16` — это `fs_grep.go` (grep-машина: опции, запуск rg, обработка вывода — тот же код, что у `grep.go`); `grep.go` сам ядро fs-семейства не трогает.
- `run→bash:2` реальных — только `truncateOutput` (определена bash.go:456, используется run_command.go:251,264,283,295 и git_read.go:254,263); идентификатор `RunAllowlistCommand` в ребре — ложное срабатывание: это одноимённые МЕТОДЫ двух разных типов (`BashPermissionsParams.RunAllowlistCommand` bash.go:46 и `RunCommandPermissionsParams.RunAllowlistCommand` run_command.go:68), один другой не вызывает.
- `bash→rg:1` — `getRg` (rg.go:18, sync.OnceValue-поиск ripgrep), используется bash.go, fs_grep.go, rg.go; `grep→rg:2`, `glob→rg:1`.
- `web→fetch:6` — web_fetch.go пользуется fetch-помощниками; `fetch→ssrf:1` и `sourcegraph→ssrf:1` — SSRF-страж (ssrf_guard.go, свой `init()`:39 строит список CIDR).
- `view→fs:5` — DiskProvider-машина: view.go использует `DiskProvider`, `OSDisk`, `diskOrOS` (view.go:readTextFileFrom/readTextFileWindow); `edit→fs:2` — тот же DiskProvider (edit.go в списке пользователей DiskProvider).
- `forbidden` — `CheckForbiddenWrite` (forbidden_writes.go) используют: edit.go ×4, multiedit.go ×2, write.go ×2, fs_replace.go, fs_write.go, fs_write_lines.go (по разу) и download.go ×1.
- `read`-кластер (read_authorization.go) используют view ×3, grep ×1, glob ×1, ls ×1 и всё fs-семейство.
- Пакетные переменные (§4.2): 79 однострочных `var`-объявлений (66 в tools, 13 в mcp) плюс многострочные `var (`-блоки — почти все это `template.Must`-описания инструментов и неизменяемые ошибки/слайсы; настоящие общие памяти: `testCommandSpawn` (command_policy_spawn.go:24, тестовый шов), `getRg` (rg.go), `ghAvailable` (tools.go:145), `osDiskSingleton` (fs_provider_os.go:30), `LibraryVirtualRoot` (fs_library_virtual_root.go:40, экспортирована, читается агентом ×10), `editMaxFileSize` (edit.go:65), кэши регулярок (grep.go:126, search.go:210), `lastSearchMu/lastSearchTime` (search.go:210), `reservedSpecialPurposeCIDRs` (ssrf_guard.go:37).
- `init()` в коде: два, оба самодостаточные — safe.go:245 (добавляет Windows-команды в `safeNoArgCommands`) и ssrf_guard.go:39 (строит `reservedSpecialPurposeCIDRs`); порядок инициализации при переносе не меняется (каждый заполняет только свои данные).
- OS-пары кода: `fs_library_virtual_root_path_unix/windows`, `os_failure_unix/windows`, `regular_open_unix/windows/other` (`//go:build plan9 || js || wasip1`), `run_command_process_unix/windows`; тестовые пары: `edit_size_guard_sparse_unix/windows_test.go`, `regular_open_special_test_unix/windows_test.go`, `c7_safe_shell_test.go`/`c7_safe_shell_windows_test.go`.
- permission/agentguard: `agentguard.*` в коде — ровно два файла, bash.go:110,127,145,160–161 и run_command.go:126,144,161–162; `permission.Service` — параметр конструктора почти каждого инструмента (26 файлов). Ни один файл tools не импортирует ядро `internal/agent` — порт-интерфейсы уже существуют (`AgentControl`, `WakeControl`, `AwaitControl` реализует координатор, agent_control.go:16–32, wake_control.go:9–27, await_tasks.go:46).
- Тесты, строящие весь набор инструментов: `error_contract_test.go` (753 строки) конструирует view/edit/write/multiedit/todos/glob/grep/ls/download/fetch/sourcegraph/fs_list/fs_find/fs_read — контрактный тест, пересекающий все будущие подпакеты (§4.7).

## 2. Целевое дерево

### 2.1. `mcp`: ядро остаётся, транспорт уезжает

Принцип (В10): всё, что читает или пишет хотя бы одну из 13 пакетных переменных (§4.1), — один пакет `mcp` на **прежнем пути** `internal/agent/tools/mcp`; бессостояные части — подпакеты. Проверка по каждому файлу (глобалы: `sessions, states, stateOwners, broker, leases, lifecycleMu, owner, initDone, generation, standaloneOwners, allTools, allPrompts, allResources`; метод проверки: grep по словным границам + чтение):

| Файл (строк) | Цель | Основание |
|---|---|---|
| init.go (443) | ядро | объявляет 10 из 13 глобалов (init.go:19–31); `Initialize`, `InitializeSingle`, `WaitForInit` |
| owner.go (881) | ядро | `Owner`, `acquire/acquireStandalone/acquireImplicit`, `currentOwner`, сброс реестра (читает и чистит все три реестра, owner.go:338, 516–524, 612–620) |
| admission.go (864) | ядро | `serverAdmission`, `setState` (states/stateOwners, admission.go:752–761), швы `mcpInitTestHooks`/`mcpReloadAfterSuccessHook` |
| lease.go (902) | ядро | `leases`, `lifecycleMu`, `serverLeaseHooks`/`addAdmissionHooks`, `getOrRenewClient*` |
| session.go (831) | ядро | `sessions.Get/Set` (session.go:387–389), глобал `owner` (session.go:451), `promote`/`sessionContext` (INV-01) |
| publish.go (257) | ядро | публикация подготовленного клиента: пишет реестры и `broker` (INV-08) |
| addreplace.go (906) | ядро | транзакции Add/Replace: sessions/states/lifecycleMu/owner (addreplace.go:405, 426–455, 885) |
| mutation.go (967) | ядро (файл режется, полоса M2 в §5.2) | Disable/Enable/Remove: states, `owner` (mutation.go:543), lifecycleMu |
| refresh.go (381) | ядро | очередь обновлений: `broker.Publish` (refresh.go:179, 264, 312), admission-фенсинг (INV-16/17) |
| uncertainty.go (260) | ядро | `lifecycleMu` + глобал `owner` (uncertainty.go:101, 171, 191), шов `mcpReloadAfterSuccessHook` |
| trusted_ui.go (214) | ядро | `ensureOwner()` → глобал `owner` |
| tools.go (461) | ядро | реестр `allTools` (tools.go:30, 371–382), `broker`, `states` |
| prompts.go (227) | ядро | реестр `allPrompts` (prompts.go:16) |
| resources.go (289) | ядро | реестр `allResources` (resources.go:21), шов `resourcesBeforePublishHook` |
| output_admission.go (47) | ядро | глобалов нет, но 1 файл не становится пакетом; используется `RunTool` ядра и `mcptools` |
| transport.go (460) | **mcpdial** | глобалов нет (grep пуст; §1.1); HTTP/stdio-транспорт, INV-20/21 |
| process_unix.go (36) | **mcpdial** | пара к transport.go (`configureStdioProcess`, `!windows`) |
| process_other.go (24) | **mcpdial** | пара (`//go:build windows`) |

Итог: ядро 15 файлов → после разреза `mutation.go` (967 строк, В7: ≥951) — 16 файлов; плюс подкаталог `internal/contextlock` = **17 элементов** в `internal/agent/tools/mcp`. `mcpdial` — 3 файла кода + тесты (§4.6) = 3 элемента. `mcp/internal/contextlock` не меняется вовсе (contextlock.go + 2 теста; его импортируют только init.go, lease.go и два теста ядра — mcpdial его не импортирует и по Go-правилам internal-видимости импортировать не может).

Зависимости: `mcp → mcpdial` (единственное новое ребро, 6 имён, §3.1), `mcp → internal/agent/tools/mcp/internal/contextlock`, `mcpdial → config/home/platform/go-sdk` (внешние). Циклов нет.

### 2.2. `tools`: семь подкаталогов, родитель без файлов

Скетч плана (`tools/{fs,search,shell,web,mcp,hooks,rush}`) проверен против кода и **исправлен** с тем же числом 7: кластеры `edit/view/write/ls/glob/todos/wake/...` в скетче не размещены; `search` и `rush` как места для grep/glob и rush_info не покрывают связей (fs_grep↔grep 16 идентификаторов, общие rg/read_authorization). Итоговое дерево (имена каталогов могут повторяться с другими деревьями, уникальность требуется только от имён пакетов):

```
internal/agent/tools/
├── toolcontract/   package toolcontract   (2 файла + contracttest/)
│   └── contracttest/  package contracttest (каркас контрактного теста)
├── fs/             (только подкаталоги)
│   ├── access/     package fsaccess   (7 файлов)
│   ├── open/       package fileguard  (7 файлов)
│   ├── read/       package fsread     (7 файлов)
│   ├── write/      package fswrite    (6 файлов)
│   ├── edit/       package fsedit     (3 файла)
│   ├── search/     package search     (5 файлов)
│   └── fakedisk/   package fakedisk   (1 файл, тестовый помощник)
├── shell/
│   ├── shelltool/  package shelltool  (6 файлов)
│   ├── bash/       package bash       (4 файла)
│   └── gitread/    package gitread    (1 файл)
├── web/
│   ├── webget/     package webget     (5 файлов)
│   └── webread/    package webread    (4 файла)
├── mcptools/       package mcptools   (4 файла)
├── orchestrate/
│   ├── delegate/   package delegate   (7 файлов)
│   ├── wake/       package wake       (6 файлов)
│   └── selfinfo/   package selfinfo   (3 файла)
└── hooks/          package hooks      (3 файла — приезжает из этапа C: internal/hooks)
```

Элементы: `tools/` = 7; `toolcontract/` = 3; `fs/` = 7; `shell/` = 3; `web/` = 2; `mcptools/` = 4; `orchestrate/` = 3; `hooks/` = 3. Все листья ≤ 7. Публичный API-пакет: **единого пакета `tools` больше нет** — путь `internal/agent/tools` становится чистой группировкой; импортёры импортируют листья напрямую. Обоснование: фасад в родителе (обёртки над ~50 именами) сохранил бы каждому импортёру замыкание на все tools-пакеты — тестовая матрица не сузилась бы (цель спринта, И§2/§7), и родитель не смог бы остаться ≤7 элементов (7 подкаталогов + файл-фасад = 8). Контрактное ядро, которое нужно всем листьям (контекстные ключи, контракт ошибок, ответ «permission denied», рендер описаний, `TimeoutParams`), — отдельный нижний лист `toolcontract`, чтобы листья не импортировали родителя-группировку (в ней нет пакета — цикл невозможен по построению).

Полная таблица «файл → цель» (все 77 рукописных файлов; `.md`/`.md.tpl`-ресурсы едут со своим файлом инструмента):

| Файлы | Цель (пакет) | Примечание |
|---|---|---|
| tools.go | toolcontract | контекстные ключи, контракт ошибок, `NewPermissionDeniedResponse`, `renderToolDescription`, `ghAvailable`; переименовать в toolcontract.go |
| timeout_params.go | toolcontract | `TimeoutParams` — общая форма bash/run_command (и schema agent); в toolcontract, чтобы bash и shelltool не сцеплялись |
| fs_provider.go, fs_provider_os.go, fs_scope.go, fs_library_virtual_root.go, fs_library_virtual_root_path_unix.go, fs_library_virtual_root_path_windows.go, fs_batch.go | fsaccess | DiskProvider/OSDisk/ErrNotExist, FolderScope, LibraryVirtualRoot, раннер батчей `RunFSBatch`; fs_scope_tools.go (38 строк) слить с fs_scope.go — 7 файлов |
| read_authorization.go, forbidden_writes.go, os_failure_unix.go, os_failure_windows.go, regular_open_unix.go, regular_open_windows.go, regular_open_other.go | fileguard | безопасное открытие (os.Root), errno-классификация, RUSH_FORBID_WRITES — 7 файлов |
| fs_read.go, fs_list.go, fs_find.go, view.go, view_suggest.go, ls.go | fsread | view.go 957 строк (В7) режется на view.go + view_content.go → 7 файлов |
| fs_write.go, fs_write_lines.go, fs_replace.go, fs_delete.go, write.go, write_shrink_guard.go | fswrite | 6 файлов |
| edit.go, edit_effect.go, multiedit.go | fsedit | 3 файла |
| grep.go, grep_ripgrep.go, rg.go, glob.go, fs_grep.go | search | rg-поиск (`getRg`), кэши рег (`searchRegexCache`/`globRegexCache`/`newRegexCache`, grep.go:57–128), grep/glob/fs_grep — 5 файлов |
| bash.go, bash_callback_context.go, bash_leaf.go, safe.go | bash | `truncateOutput` уезжает в shelltool (§3.3) |
| run_command.go, run_command_leaf.go, run_command_output_buffer.go, run_command_process_unix.go, run_command_process_windows.go, command_policy_spawn.go | shelltool | + принятый из bash.go `truncateOutput` — 6 файлов |
| git_read.go | gitread | 1 файл |
| fetch.go, fetch_helpers.go, fetch_types.go, download.go, ssrf_guard.go | webget | fetch-машина + SSRF-страж со своим `init()` |
| web_fetch.go, web_search.go, search.go, sourcegraph.go | webread | движок DDG (`userAgents`/`acceptLanguages`/`lastSearch*`), sourcegraph; `searchRegexCache` из grep.go им не нужен (пользователей вне search не найдено — grep по `searchRegexCache` вне grep.go пуст) |
| mcp-tools.go, mcp_output.go, list_mcp_resources.go, read_mcp_resource.go | mcptools | обёртки над `internal/agent/tools/mcp` |
| agent_control.go, ask_question.go, await_tasks.go, inject_agent.go, inspect_agent.go, stop_agent.go, read_delegation_transcript.go | delegate | 7 файлов |
| wake_control.go, wakein.go, wakeon.go, wake_loop.go, wake_list.go, wake_cancel.go | wake | 6 файлов |
| rush_info.go, rush_logs.go, todos.go | selfinfo | 3 файла |
| (этап C) hooks.go, input.go, runner.go из internal/hooks | hooks | переезд этапа C — путь `internal/agent/tools/hooks`, без изменений |

Тесты — по поведению (84 файла; кластерные префиксы целиком, отдельные — поимённо):

| Тестовые файлы | Цель |
|---|---|
| bash_test, bash_leaf_test, bash_retained_test, bash_start_failure_test, bash_window_safety_test, bash_git_writes_test, c7_safe_shell_test, c7_safe_shell_windows_test, deny_exec_policy_test, job_test | bash |
| run_command_test, run_command_leaf_test, run_command_git_writes_test, run_command_last_write_test, run_command_output_buffer_test, run_command_process_kill_test | shelltool |
| git_read_test | gitread |
| grep_test, grep_fallback_test, grep_file_filter_test, grep_output_test, grep_pattern_test, grep_ripgrep_overflow_test, glob_partial_note_test, glob_search_memory_test, rg_argv_test, fs_grep_test, bounded_walk_tools_test* | search (*если по идентификаторам относится к glob/ls-обходу; иначе fsread — решить в полосе по компиляции) |
| fs_read_test, fs_list_test, fs_find_test, view_test, view_suggest_test, r6_2_fs_list_find_result_resolution_test | fsread (+ тестовая половина от разреза view.go) |
| fs_write_test, fs_write_lines_test, fs_write_shrink_test, fs_replace_test, fs_delete_test, fs_batch_test, mutation_size_guard_test, create_parent_permission_test* | fswrite (*проверить в полосе: если тест про permission-запрос fs_write — fswrite) |
| edit_test, edit_effect_test, multiedit_test, edit_size_guard_sparse_unix_test, edit_size_guard_sparse_windows_test | fsedit |
| fs_provider_test, fs_scope_test, fs_library_virtual_root_path_test, fs_library_virtual_root_path_unix_test, fs_library_virtual_root_path_windows_test, r5_2_canonicalize_folder_scope_test | fsaccess |
| fs_provider_fake_test.go | fakedisk (фальшивый DiskProvider переезжает из `_test.go` в обычный файл тестового пакета-помощника; все Test-функции, если есть внутри, остаются в fsaccess) |
| unreadable_file_other_test, unreadable_file_windows_test, regular_open_special_test_unix, regular_open_special_test_windows, os_failure* | fileguard |
| download_test, fetch-часть error_contract (см. contracttest), ssrf_guard_test | webget |
| web_fetch_test, sourcegraph_test | webread |
| mcp-tools_test, mcp_output_test | mcptools |
| agent_control_test, agent_control_schema_test, ask_question_test, await_tasks_test, read_delegation_transcript_test | delegate |
| wake_tools_test | wake |
| rush_info_test, rush_logs_test, p483_todos_invalid_status_test | selfinfo |
| context_test, timeout_params_test | toolcontract |
| error_contract_test (753 строки, целиком) | toolcontract/contracttest — исключение из правила «тест едет с кодом»: субъект теста — сам контракт ошибок, он конструирует инструменты всех будущих пакетов; каркас (моки permission/history/filetracker) остаётся с ним. Альтернатива «резать таблицу по пакетам» отклонена как чистый churn без выигрыша в матрице (тест и так compile-зависит от всех листьев). |

Проверка имён пакетов: `git grep -h '^package ' -- '*.go' | sort -u` — имён `toolcontract, contracttest, fsaccess, fileguard, fsread, fswrite, fsedit, search, shelltool, bash, gitread, webget, webread, mcptools, delegate, wake, selfinfo, fakedisk, mcpdial` в репозитории сегодня нет; запрещённых родовых (`core`, `util`, `common`, `base`) нет. Осмысленность без пути: `fileguard` — стражи доступа к файлам, `selfinfo` — сведения о самом rush, `mcptools` — инструменты поверх MCP (в отличие от пакета `mcp` — его жизненный цикл).

Граф зависимостей новых пакетов (стрелки = «импортирует», всё ациклично):

```
            coordinator/агент/app/sdk/cmd (вне tools)
                │ импортируют листья напрямую
toolcontract ← { все листья }
fsaccess ← { fsread, fswrite, fsedit, search }
fileguard ← { fsread, fswrite, fsedit, search, webget }
search ← { bash (getRg) }          fsedit ← { — }     fswrite ← { — }     fsread ← { — }
shelltool ← { bash (TruncateOutput, политика запуска), gitread }
webget ← { webread (fetch-помощники, ssrf) }
mcp ← { mcptools }                 mcp → mcpdial
delegate/wake/selfinfo → { session.Service, message.Service, toolcontract }
mcptools → { mcp, toolcontract, permission, config }
```

Публичная поверхность, которую потребляют снаружи (измерено; «tools.Get/New/List/Find/Copy» из первого прохода — это поле `tools` структуры агента (fantasy toolset), не наш пакет): `toolcontract`: SessionIDContextKey ×48, GetSessionFromContext ×20, GetMessageFromContext ×9, GetModelNameFromContext ×6, GetSupportsImagesFromContext ×5; `fsaccess`: DiskProvider ×50, OSDisk ×23, IsCustomDiskProvider ×10, HasDiskProvider ×10, LibraryVirtualRoot ×10; `bash`: BashToolName ×35, NewBashTool ×10, BashParams/BashPermissionsParams; `shelltool`: RunCommandToolName ×23; `fsedit`: EditToolName ×21, MultiEditToolName ×13; `fsread`: ViewToolName ×20, LSToolName ×10, ListResult ×15, FindResult ×15, ListRequest ×8, FindRequest ×8; `search`: GlobToolName ×13, DiskSearchResult ×15, SearchRequest ×8, FSGrep…; `fswrite`: FSWriteToolName ×10, WriteToolName ×8; `delegate`: AskQuestionToolName ×52, AwaitTasksToolName ×12, AwaitTasksResult ×20, AwaitTaskRef ×6, AgentControl ×6; `wake`: WakeScheduleView ×25, WakeControl ×8; `mcptools`: GetMCPTools ×7, Tool, MCPOutputAccess; `webget`: AgenticFetchToolName ×28; `gitread`: GitReadToolName ×11; `selfinfo`: RushLogsToolName ×9; `mcptools`/`mcp`: ReadMCPResourceToolName ×10. Регистрация набора — `internal/agent/coordinator_tools.go` (сборка ~35 конструкторов, buildTools) — меняет только блок импортов и квалификаторы; сама последовательность конструирования не меняется (поведение).

## 3. Швы

Циклов между целевыми пакетами нет (граф §2.2 — DAG; `mcp → mcpdial` — единственное ребро ядра наружу). Все швы — подъём видимости существующих функций или перенос помощника вниз по графу; ни один не создаёт экспортируемой изменяемой переменной и не меняет порядка вычислений.

### 3.1. `mcp` → `mcpdial` (главный шов задачи)

Граница проходит по четырём функциям и двум ошибкам `transport.go`, единственным потребителем которых вне транспортного кода является ядро:

| Имя сегодня | Имя в `mcpdial` | Экспортируется | Места вызова в ядре |
|---|---|---|---|
| createTransport (transport.go:86) | CreateTransport | да | session.go:728 (`createSession`) |
| mcpTimeout (transport.go:334) | Timeout | да | session.go:688, publish.go:30, refresh.go:230, lease.go:585 |
| maybeStdioErr (transport.go:72) | MaybeStdioErr | да | session.go:773 |
| transportCleanup (transport.go:48) | Cleanup | да | session.go:791 |
| ErrStdioDiagnosticTooLarge (transport.go:32), StdioDiagnosticOutputLimitError (transport.go:36) | без изменений | уже экспортированы | тесты stdio_diagnostic_test.go |

Итого 6 имён через границу (лимит ~15 соблюдён). Обратных имён нет: `mcpdial` не импортирует `mcp` (проверено: в transport.go/process_*.go нет ссылок на глобалы и типы ядра; `configureStdioProcess`, `headerRoundTripper`, `ownerResponseBody`, `cloneHTTPTransport`, `stdioCheck`, `diagnosticCommand`, `stdioDiagnosticWriter`, `sanitizeStdioDiagnosticPrefix`, `effectivePort`, `checkMCPRedirect` остаются неэкспортированными внутри `mcpdial`).

Аргумент сохранения поведения: `CreateTransport(ctx, config.MCPConfig, config.VariableResolver)` — чистая функция контекста и конфигурации; она и сегодня вызывается ровно из одного места (session.go:728), до/после неё в ядре ничего не вычисляется по-другому. `Timeout` — детерминированная функция конфига; `MaybeStdioErr` и `Cleanup` — пост-обработка транспорта. Подъём видимости не меняет момент вычисления (урок §3 п. 12 плана: ленивое остаётся ленивым — ничего eager не появляется). Порядок `init()` не задействован (в обоих пакетах `init()` кода нет). Инварианты INV-20 (собственный пул HTTP-транспортов и отмена по контексту владельца) и INV-21 (killer дерева stdio-процесса и ограниченная диагностика) продолжают выполняться тем же кодом — меняется только путь файла; их якоря в реестре переякориваются тем же коммитом (§5.3).

### 3.2. `toolcontract` — разрыв цикла «родитель ↔ листья»

Без отдельного контракта листьям нужен `GetSessionFromContext`/`NewPermissionDeniedResponse` из родителя, а родителю-фасаду — конструкторы листьев: цикл. Шов: контрактное ядро (tools.go целиком + timeout_params.go) переезжает в нижний лист `toolcontract`, родитель остаётся без файлов. Через границу лист→toolcontract: `GetSessionFromContext, GetMessageFromContext, GetSupportsImagesFromContext, GetModelNameFromContext, SessionIDContextKey, MessageIDContextKey, SupportsImagesContextKey, ModelNameContextKey, NewPermissionDeniedResponse, renderToolDescription, ghAvailable, TimeoutParams` — 12 имён (внутри репозитория ниже tools их никто не использует; 52+20+15+9+6+5 внешних употреблений переезжают на импорт `toolcontract` механически в полосе A5). Поведение: код контракта не меняется ни строкой — меняется только путь; `ghAvailable` (tools.go:145, `testing.Testing()` + `exec.LookPath`) вычисляется один раз при инициализации пакета, как и сегодня; порядок инициализации contract→листья гарантируется самим графом импортов (Go инициализирует импортируемое раньше импортирующего).

### 3.3. `truncateOutput` → `shelltool` (разрыв «run ↔ bash»)

Единственное реальное ребро `run→bash` (§1.2): функция bash.go:456, потребители run_command.go:251,264,283,295 и git_read.go:254,263. Шов: функция переносится в shelltool (в run_command_output_buffer.go — её предмет, обрезка вывода) и экспортируется как `shelltool.TruncateOutput`; bash и gitread импортируют её. 3 имени через границу (TruncateOutput + уже экспортируемые `WithCommandSpawnProbe`-политика: bash.go и run_command.go оба используют command_policy_spawn.go — он в shelltool, поэтому bash→shelltool: command policy (2 имени) + timeout (1) + TruncateOutput (1) = 4). Поведение: тело функции не меняется; момент вызова не меняется (она вызывается в тех же выражениях). Ложное ребро `RunAllowlistCommand` — одноимённые методы разных типов, ничего не переезжает.

### 3.4. `fs_grep` → `search` (рёбра fs→grep:16 и fs→rg:3)

fs_grep.go переезжает в `search` вместе с grep.go/grep_ripgrep.go/rg.go/glob.go: машинерия grep (опции, запуск rg, разбор вывода, `boundedRipgrepDiagnostic`) становится внутренней для одного пакета, ребро ≥15 идентификаторов исчезает. Наружу из `search`: `GetRg` (бывш. `getRg`, sync.OnceValue) для bash (bash→search: 1 имя). fs_grep сохраняет свои параметры (`DiskProvider`, FolderScope из fsaccess; search→fsaccess: 3–5 имён — `DiskProvider, ErrNotExist, ResolveScopedPath, LibraryVirtualRoot` и типы запроса). Контингенция: если в полосе вскроется, что fs_grep рендерит результаты через неэкспортированные помощники view (`fs→view:5`), помощники рендера `<file>…</file>` переезжают в fsaccess (общее место fs-семейства), а не экспортируются из fsread — направление search→fsaccess уже существует. Проверка в полосе A2 по списку неразрешённых идентификаторов компилятора.

### 3.5. `webget`/`webread` — разрыв «web ↔ fetch»

`web_fetch.go` пользуется fetch-помощниками (`web→fetch:6`), а `fetch→ssrf:1` и `sourcegraph→ssrf:1` тянут в обратную сторону, если всё в одном пакете с подпакетами. Шов: fetch-машина + SSRF-страж — нижний лист `webget` (fetch.go, fetch_helpers.go, fetch_types.go, download.go, ssrf_guard.go); `webread` (web_fetch, web_search, search, sourcegraph) импортирует `webget`. Через границу: fetch-помощники и типы (`AgenticFetchToolName`, `SearchRequest` и 4–6 внутренних имён — уточнить в полосе по неразрешённым идентификаторам; ориентир ≤15). Поведение: `ssrf_guard.go` несёт свой `init()` с собой (порядок инициализации webget раньше webread — как сегодня внутри одного пакета: init заполняет только собственный срез CIDR, внешних эффектов нет).

### 3.6. `contracttest` — каркас контрактного теста

`error_contract_test.go` конструирует инструменты всех будущих пакетов. Шов: пакет `toolcontract/contracttest` — обычный (не `_test`) тестовый помощник по образцу `internal/db/dbtest`: каркас (`runContractTool`, моки permission/history/filetracker) — обычный файл пакета, сам таблицный тест — `_test.go` того же пакета. Пакет импортирует все листья и `toolcontract`; листья его не импортируют — цикла нет. В матрице тестов ничего не меняется: тест и сегодня собирается со всем tools.

## 4. Глобалы, init(), embeds, OS-файлы, белые тесты, помощники

### 4.1. Глобалы `mcp` (13 + 5 тестовых швов)

Реестр инвариантов говорит «12» (девять из init.go:18–29 + три реестра); на текущем HEAD в блоке уже 10 переменных — добавился `standaloneOwners` (init.go:30, изоляция standalone-владельцев, INV-02). Факт: 13. Каждой — ровно один владелец: ядро `mcp`. Таблица читателей/писателей (файл:символ, prod; тесты не перечислены — их сотни, все остаются в ядре):

| Глобал | Объявление | Читатели/писатели (файл:символ) |
|---|---|---|
| sessions | init.go:20 | addreplace.go: publishPreparedClientLocked (Get/Del/Set, :426–454), откат транзакции (:889); lease.go: getOrRenewClientOnceForOwner (Get :504, Set :755), commitRenewal (:614–705), currentClientLease (:838, :882); admission.go: validLocked (:257); owner.go: canReclaimLocked (:338 Len), resetRegistryLocked (:523) |
| states | init.go:21 | addreplace.go: publishPreparedClientLocked (:427–455); admission.go: setState (:752), cleanupFailedAdmission (:829); lease.go: health-проба (:584); mutation.go: mutation-пути (:705, :746, :841, :859); owner.go: resetRegistryLocked (:590–619), GetStates (:746), GetState (:790), IsConfigured (:612); tools.go/prompts.go/resources.go: Refresh* |
| stateOwners | init.go:22 | admission.go: setState (:753, :761, :830); owner.go: resetRegistryLocked (:612–620), detach (:845) |
| broker | init.go:23 | owner.go: resetBrokerLocked (:651–652), eventBroker (:737); publish.go: publishPreparedClientLocked (:245); refresh.go: publish (:179, :264, :312); tools/prompts/resources: publish событий |
| leases | init.go:24 | lease.go: lockServerLeasesContext (:267, :387), unlockServerLeases (:299–301), serverLeaseFor; owner.go: resetRegistryLocked (:624 deleteIfUnused) |
| lifecycleMu | init.go:26 | все десять stateful-файлов (порядок «lease → lifecycleMu», INV-06); точечно: addreplace.go:292–857, owner.go:227–635, session.go:450–457, uncertainty.go:31–191, init.go beginInit/beginInitialize/finishInitialize |
| owner (текущий) | init.go:27 | писатели: acquire/acquireImplicit (owner.go:396), close (owner.go:559–562); читатели: currentOwner (owner.go:404), init.go:133, mutation.go:543–547, addreplace.go:405/:885, admission.go:465/:594, session.go:451, uncertainty.go:101/:171, owner.go:227–635 |
| initDone (зеркало) | init.go:28 | beginInitialize (:59), WaitForInit (:258), owner.go:397 (установка), :563 (сброс) — INV-22b, S-часть: зеркальная переменная; не мигрирует, остаётся в ядре |
| generation | init.go:29 | owner.go: isCurrentLocked (:413), acceptsGeneration (:440), bump при acquire (:366); admission.go: :141, :206, :244, :325, :385, :500, :723; addreplace.go: :405, :885; lease.go: :347 |
| standaloneOwners | init.go:30 | owner.go: acquireStandalone (:292–315), resetRegistryLocked (:572) |
| allTools | tools.go:30 | tools.go: Tools/GetServerToolNames/updateTools; owner.go: canReclaimLocked (читает все три реестра, owner.go:284–286), resetRegistryLocked (чистит, :516–524); publish.go/refresh.go — через updateTools |
| allPrompts | prompts.go:16 | prompts.go, owner.go (там же), publish.go/refresh.go |
| allResources | resources.go:21 | resources.go, owner.go (там же), publish.go/refresh.go |
| serverLeaseHooks | lease.go:55 | тестовый шов; вызовы lease.go:165–239; пользователи — 7 тестовых файлов; остаётся в ядре рядом с `serverLease` (реестр предупреждает: швы связаны с оракулами INV-06/07/08, перенос запрещён) |
| addAdmissionHooks | lease.go:62 | тестовый шов; lease.go:65–67; lease_followup_test.go |
| mcpInitTestHooks | admission.go:552 | тестовый шов; admission.go:563–583; 6 тестовых файлов |
| mcpReloadAfterSuccessHook | admission.go:550 | тестовый шов; uncertainty.go:59–60; 2 тестовых файла |
| resourcesBeforePublishHook | resources.go:23 | тестовый шов; resources.go:29–31; lease_completeness_test.go |

Через границу `mcp → mcpdial` не идёт ни одна переменная; экспортируемых переменных ради разрезки не создаётся (правило В6/§3 п. 3). Записи в `docs/mcp-invariants.md`, упоминающие эти переменные (раздел Classification: nine globals + три реестра), остаются верными: размещение не меняется.

### 4.2. Глобалы `tools`

| Переменная | Сегодня | Цель | Комментарий |
|---|---|---|---|
| ghAvailable | tools.go:145 | toolcontract | вычисляется лениво один раз; `testing.Testing()`-заглушка сохраняется |
| getRg | rg.go:18 | search | sync.OnceValue; экспортируется как `GetRg` для bash |
| testCommandSpawn | command_policy_spawn.go:24 | shelltool | тестовый шов; единственный владелец; bash обращается через экспортируемые `WithCommandSpawnProbe`/`commandSpawnFromContext` (вторую придётся экспортировать — 1 имя) |
| osDiskSingleton | fs_provider_os.go:30 | fsaccess | синглетон диска; не экспортируется |
| LibraryVirtualRoot | fs_library_virtual_root.go:40 | fsaccess | экспортирована, читается агентом ×10; писателей нет (grep по `LibraryVirtualRoot =` в тестах пуст); остаётся переменной fsaccess, импортёры читают fsaccess.LibraryVirtualRoot |
| editMaxFileSize | edit.go:65 | fsedit | писателей в тестах не найдено (grep пуст); контролируется полосой: если найдутся — шов-сеттер в fsedit |
| searchRegexCache, globRegexCache, newRegexCache, globBraceRegex | grep.go:57–128, glob.go:28 | search | все пользователи внутри search (вне grep.go/glob.go ссылок на searchRegexCache нет) |
| lastSearchMu/lastSearchTime | search.go:210 | webread | троттлинг движка DDG |
| userAgents, acceptLanguages | search.go:26, 40 | webread | неизменяемые |
| reservedSpecialPurposeCIDRs | ssrf_guard.go:37 | webget | заполняется своим init() |
| reservedFields, sensitiveKeys | rush_logs.go:53, 61 | selfinfo | неизменяемые |
| whitelistDockerTools | mcp-tools.go:17 | mcptools | неизменяемый |
| safeNoArgCommands | safe.go:10 | bash | заполняется init() safe.go |
| шаблоны описаний (×20 `template.Must`) и `*Description` (×18) | рядом с каждым инструментом | пакет своего инструмента | каждая едет со своим файлом и своим `go:embed` |

### 4.3. init() — полный список и порядок

- `internal/agent/tools/mcp`: в коде нет; в тестах один — ktav_testgate_test.go:31 (регистрирует testgate; остаётся в ядре рядом с инициализацией пакета).
- `internal/agent/tools`: два, оба самодостаточные: safe.go:245 (bash) и ssrf_guard.go:39 (webget). Каждый заполняет только собственные данные пакета, внешних эффектов и зависимостей от чужой инициализации нет — разнос по пакетам порядок не меняет (Go инициализирует импортируемое раньше импортирующего; оба пакета не имеют init-зависимостей от соседей по tools).
- `internal/hooks` (этап C): свои init-ов нет.

### 4.4. Встроенные ресурсы

В `mcp` embeds нет. В `tools` 40 директив `go:embed` — по одной на файл инструмента (список §1.2); каждый `.md`/`.md.tpl` переезжает вместе со своим `.go` в тот же каталог (embed-пути относительные — не ломаются). `testdata/grep.txt` — данные grep-тестов → `fs/search/testdata/`. В `mcptools` embeds нет (у list_mcp_resources/read_mcp_resource есть `.md` — их два, едут с ними).

### 4.5. OS-файлы (пары не разлучать)

| Пара | Цель |
|---|---|
| mcp/process_unix.go (!windows) + mcp/process_other.go (windows) | mcpdial |
| mcp/process_unix_test.go (!windows) + mcp/process_windows_test.go (windows) | mcpdial (windows-файл режется: TestMain остаётся в ядре, §4.6) |
| fs_library_virtual_root_path_unix/windows.go | fsaccess |
| os_failure_unix/windows.go | fileguard |
| regular_open_unix/windows/other.go (!windows / windows / plan9‖js‖wasip1) | fileguard |
| run_command_process_unix/windows.go | shelltool |
| edit_size_guard_sparse_unix/windows_test.go | fsedit |
| regular_open_special_test_unix/windows_test.go | fileguard |
| c7_safe_shell_test.go / c7_safe_shell_windows_test.go | bash |

### 4.6. Белые тесты и TestMain

Все 45 тестов `mcp` и 84 теста `tools` — белые (`package mcp` / `package tools`, внешних `*_test`-пакетов нет). Правило: тест остаётся в пакете тех неэкспортированных идентификаторов, которых касается; тест, трогающий два пакета, делится по тест-функциям (не по файлам целиком), сумма Test-функций неизменна.

- Перечень переездов `mcp` — §4.6.1 ниже; **все остальные тесты остаются в ядре** (они трогают lifecycleMu/leases/owner/admission/serverAdmission — идентификаторы ядра).
- TestMain сегодня: `main_test.go` (!windows: `testguard.Main(m)`) и `process_windows_test.go:36` (windows: `testguard.Isolate()` + диспетчеризация субпроцессных помощников `RUSH_MCP_TEST_HELPER` + `m.Run()`). После переезда INV-21-теста с его помощниками (`runLevel1Helper/runLevel2Helper/helperEnvVar`, process_windows_test.go:23–34, 55+) в `mcpdial`: (а) в ядре появляется `main_windows_test.go` (windows: только Isolate + m.Run — без чужой диспетчеризации); (б) `mcpdial` получает собственный TestMain на обеих платформах: `main_test.go` (!windows: `testguard.Main(m)`) и windows-TestMain в перенесённом process_windows_test.go (Isolate + диспетчеризация RUSH_MCP_TEST_HELPER + m.Run) — субпроцесс-помощник INV-21 порождает `os.Executable()`, т.е. бинарник mcpdial-тестов, диспетчер обязан жить в нём. Без этого на одной из платформ теряется изоляция testguard — приёмка полосы проверяет наличие ровно одного TestMain на платформу в каждом пакете.
- Счёт тестов: 230 записей `go test -list` до и после (по сумме mcp+mcpdial на каждой платформе; два помощника с именами Test* остаются: `TestMCPStdioHelper` — ядро, `TestMCPStdioDiagnosticHelper` — mcpdial).

#### 4.6.1. Переезд тестов `mcp` → `mcpdial` (по идентификаторам)

| Из файла | Переезжают в mcpdial | Почему |
|---|---|---|
| transport_redirect_test.go | целиком | только checkMCPRedirect/effectivePort |
| stdio_diagnostic_test.go | целиком (включая TestMCPStdioDiagnosticHelper) | stdioCheck/diagnosticCommand/stdioDiagnosticWriter/sanitizeStdioDiagnosticPrefix/ErrStdioDiagnosticTooLarge/MaybeStdioErr |
| process_unix_test.go (!windows) | целиком | configureStdioProcess |
| process_windows_test.go (windows) | целиком, кроме TestMain | TestMain — ядру (см. выше); тест INV-21 и помощники — mcpdial |
| init_test.go: 5 функций — TestCreateTransport_URLResolution, _StdioResolution, _HeadersResolution, _PassesLifetimeContextToEveryRuntimeField (INV-25), _PreCanceledContextWinsFieldValidation (INV-20) | новый transport_resolution_test.go | только createTransport + резолверы; остальные init_test-тесты (Owner/admission) остаются |
| init_lifecycle_test.go: TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose, TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose (+ type closeAfterContextBody) | новый transport_cancellation_test.go | headerRoundTripper/ownerResponseBody/cloneHTTPTransport |
| state_regression_test.go: TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse | туда же | то же |
| state_regression_refresh_test.go: TestCloneHTTPTransportFallsBackFromCustomDefault, TestHeaderRoundTripperUsesOwnedTransportPool | новый transport_pool_test.go | cloneHTTPTransport |

Локальные помощники `signalStarted` (state_regression_refresh_test.go:151) и `waitForRequest` (state_regression_test.go:77) используются и переезжающими, и остающимися тестами: полоса выполняет аудит по каждому переезжающему Test; помощник дублируется в mcpdial (по 3–5 строк), в ядре остаётся для своих — это единственная осознанная дубликация, в помощьники-пакет их выносить нельзя (белые, без общего кода).

#### 4.6.2. Белые тесты `tools`

Перечислены в таблице §2.2. Дополнительные правила: (а) тест, чьи идентификаторы расщепляются между пакетами (например, тест fs_grep с моками fsaccess), импортирует `fakedisk`/лист; (б) `deny_exec_policy_test.go` — решается по идентификаторам: если тестирует политику spawn (command_policy_spawn) — shelltool, если тело bash — bash; приоритет: где объявлен мок; (в) `bounded_walk_tools_test.go` — по идентификаторам glob/ls (search) или fs_list (fsread); (г) `create_parent_permission_test.go` — fswrite, если речь про permission-запрос fs_write/fs_replace, иначе fsaccess.

### 4.7. Тестовые помощники через границы

- `fs_provider_fake_test.go` (863 строки) → пакет `fs/fakedisk` (обычный файл `fake.go` + `fakedisk`-пакет): фальшивый DiskProvider нужен тестам fsaccess, fsread, fswrite, search после разъезда. Test-функции, если внутри файла есть, остаются в fsaccess. Если фейк трогает неэкспортированные идентификаторы fsaccess — это сигнал экспортировать конструктор/опции фейка, не подмешивать пакет.
- Моки permission/history/filetracker (mockPermissionService и др.) — в error_contract_test.go/contracttest; у leaf-тестов свои локальные моки (полоса проверяет компиляцией; глобального разъезда моков нет).
- В `mcp` общий пакет помощников не создаётся: белые тесты ядра не разъезжаются, помощники остаются в пакете (плюс дубль-копии §4.6.1).

## 5. План исполнения (полосы)

### 5.1. Порядок и параллельность

Дерево `mcp` и дерево `tools` независимы (общих файлов нет) — M-полосы идут параллельно с A-полосами. Внутри `tools` полосы **строго последовательны**: каждая переносит имена, на которые ссылается общий импортёр `internal/agent/coordinator_tools.go` (и остальные 137 файлов импортёров), поэтому параллельные полосы столкнулись бы на одних и тех же файлах. Одновременно живых агентов ≤ 5 (§3 п. 7 плана): M1 + M2 + максимум три готовящихся A-полосы (сборка материалов, без правок). Все полосы — в worktree, интеграционная ветка волны 5, приёмка оркестратором между полосами (oracle, `golock.sh go build/vet`, серийные тесты затронутых пакетов под `capm 2g -p 1 -parallel 1 -count=1`), CI-зелёный — на конце волны (§8 плана). Запретов 0б для этих пакетов нет: циклов в целевом графе нет, швы встроены в первые полосы каждой группы.

### 5.2. Полосы

| Полоса | Объём (файлы) | Предпосылки | Приёмка |
|---|---|---|---|
| **M1** mcpdial | перенос transport.go, process_unix.go, process_other.go → `mcp/mcpdial/`; подъём видимости 6 имён (§3.1); правка 4 мест вызова: session.go:688,728,773,791; publish.go:30; refresh.go:230; lease.go:585; тесты §4.6.1 (4 файла целиком + 3 новых файла из 10 извлечённых Test-функций + TestMain-расщепление: ядро получает main_windows_test.go, mcpdial — main_test.go/windows-TestMain) | нет | оракул чистого переноса: сортированный `^(func|type|var|const)`-список множества «старый transport.go+process_*.go+извлечённые Test» равен объединению новых файлов; сумма Test-функций mcp+mcpdial = 230 на каждой платформе; `go build`/`go vet` обоих пакетов через golock.sh; grep: в ядре не осталось неэкспортированных транспортных идентификаторов (`headerRoundTripper|cloneHTTPTransport|ownerResponseBody|stdioCheck|diagnosticCommand|stdioDiagnosticWriter|checkMCPRedirect|effectivePort|configureStdioProcess|createTransport|mcpTimeout|maybeStdioErr|transportCleanup` — только квалифицированные `mcpdial.*`); ровно один TestMain на платформу в каждом пакете; переякоривание реестра (§5.3) — тот же коммит |
| **M2** разрез mutation.go | mutation.go (967) → mutation.go + mutation_pending.go (оба ≤800): граница по Pending-Add-условной записи (INV-12, `resolveMCPMutationScope`/`pendingGlobalAdds` остаются рядом с callers); чистое разрезание внутри пакета | M1 не обязателен (файлы не пересекаются), но запись храповика — после обеих | оракул: объединение сортированных списков объявлений двух файлов равно списку старого; число Test-функций не меняется (тестов в mutation.go нет); `wc -l` ≤800 у обоих; build/vet |
| **A1** toolcontract | tools.go → toolcontract/toolcontract.go, timeout_params.go → toolcontract/timeout_params.go, context_test.go и timeout_params_test.go → toolcontract; импортёрам — 12 имён контракта (§3.2) | нет | оракул (список объявлений, Test-функции context_test); build/vet tools + импортёров; grep: `tools.GetSessionFromContext` и др. — 0 вхождений вне toolcontract |
| **A2** fs-группа | 35 файлов кода → fsaccess (7, со слиянием fs_scope_tools в fs_scope), fileguard (7), fsread (7, с разрезом view.go 957 → view.go+view_content.go), fswrite (6), fsedit (3), search (5, включая fs_grep.go); fs_provider_fake_test.go → fs/fakedisk; тесты по таблице §2.2; импортёрам — имена fs/search-групп | A1 (contracttest-имена уже у toolcontract) | оракул по каждому перемещённому множеству; счёт Test-функций без изменений; элементы: fs/ = 7, все листья ≤7; grep: `truncateOutput` в bash.go нет; `CheckForbiddenWrite` определён только в fileguard; `DiskProvider` определён только в fsaccess; неразрешённых идентификаторов нет (компилятор = список границы); контингенция §3.4 (helpers рендера → fsaccess) |
| **A3** shell+web | bash×4 (bash.go без truncateOutput), run_command×5 + command_policy_spawn (+ принятый truncateOutput) → shelltool; git_read → gitread; fetch×3+download+ssrf → webget; web_fetch+web_search+search+sourcegraph → webread; тесты §2.2; импортёрам — имена групп | A2 (search уже существует: bash→search getRg) | оракул; build/vet; grep: `truncateOutput` определён один раз (shelltool); ssrf init() уехал с файлом; элементы shell/ = 3, web/ = 2 |
| **A4** mcptools+orchestrate | mcp-tools, mcp_output, list_mcp_resources, read_mcp_resource → mcptools; delegate 7; wake 6; selfinfo 3; тесты §2.2; импортёрам — имена групп | A1 | оракул; build/vet; grep: `GetMCPTools` только в mcptools; элементы mcptools/ = 4, orchestrate/ = 3 |
| **A5** финал tools | error_contract_test.go → toolcontract/contracttest (каркас — обычный файл пакета, тест — `_test.go`); исчезновение пакета `tools` (в `internal/agent/tools` не остаётся .go); сверка итогового дерева с §2.2 | A1–A4 | в `internal/agent/tools` ровно 7 подкаталогов и 0 файлов; build/vet всего замыкания (agent, app, sdk, cmd, proto, server); финальный свод оракулов всех A-полос; серийные тесты затронутых пакетов; обновление CI-конфигов по путям (ci-policy.toml, shards.toml, .golangci.yml — скрипт этапа B) |

Храповики и сиды:

- `.githooks/dir_entries_allowlist.txt`: запись `internal/agent/tools/mcp 17` с комментарием (В10: «ядро с 13 немигрирующими глобалами: 15 файлов-обязанцев состояния + разрез mutation.go + internal/contextlock; число упадёт только при пересмотре решения о миграции глобалов (CLAUDE.md)» — вносится последней из M-полос); строка `internal/agent/tools` (сид 80) удаляется — каталог вышел на 7; запись `internal/agent/tools/mcp/internal` не нужна (сам каталог contextlock = 1–3 элемента).
- Второй храповик (A1b, все файлы с тестами, ≤21, число только падает): новые сиды фиксируются при создании каталогов той же полосой: mcp 57 (было 63), mcpdial 10, toolcontract 4 (2+contracttest), fsaccess 14, fileguard ~12, fsread ~13, fswrite ~13, fsedit 8, search ~16, fakedisk 1, shelltool ~15, bash ~14, gitread 2, webget 8, webread 6, mcptools 6, delegate ~11, wake 8, selfinfo 6, contracttest 2. Точные значения — момент фиксации полосы; все ниже сегодняшних сидов.

### 5.3. Реестр инвариантов (переякоривание в том же коммите, что и M1)

- **INV-20**: Where — `transport.go` → `mcpdial/transport.go` (headerRoundTripper.RoundTrip, ownerResponseBody.Read/Close, createTransport→CreateTransport); файл тестов «Proven by»: TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse, TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose, TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose, TestCreateTransport_PreCanceledContextWinsFieldValidation → новые файлы mcpdial.
- **INV-21**: Where — transport.go (stdioCheck/diagnosticCommand/stdioDiagnosticWriter) → `mcpdial/transport.go`; process_unix.go/process_other.go → `mcpdial/`; process_unix_test.go/process_windows_test.go → mcpdial.
- **INV-25**: колонка «File» карты тестов — TestCreateTransport_PassesLifetimeContextToEveryRuntimeField (init_test.go → mcpdial/transport_resolution_test.go).
- **Раздел аудитов**: упоминания `internal/agent/tools/mcp/transport.go:headerRoundTripper.RoundTrip` (Re-anchoring findings, INV-20) и путей process_*_test.go (INV-21) — обновить пути.
- Все остальные строки (INV-01…19, 22…25 и их тесты) **не меняются**: файлы ядра остаются на прежнем пути — это главное свойство дизайна «ядро на месте».

## 6. Риски и открытые вопросы

Риски:

1. **Широта механического переезда импортёров** (138 файлов, 54 кода + 84 теста, пять последовательных полос): ошибка = сломанная сборка, ловится `go build`/`go vet` каждой полосы; смыслового дрейфа нет (логика не меняется). Смягчение: переписывание импортов скриптом (по образцу tools/pkgmove этапа B), а не руками.
2. **Непроверенные хвосты границ**: списки идентификаторов fsaccess/fileguard/search/webget подтверждены только частично (entangle-счётчики содержат шум совпадений имён методов); точные списки даёт компилятор в полосе. Контингенции прописаны (§3.4).
3. **Храповик 17 у ядра mcp** — потолок навсегда, пока не пересмотрено решение о глобалах; риск не роста, а «замороженности» каталога. Принято осознанно (В10).
4. **TestMain-расщепление по платформам** (§4.6): тихая потеря изоляции testguard на одной платформе. Приёмка M1 проверяет ровно один TestMain на платформу в каждом пакете и прогон на обеих платформах в CI.
5. **Псевдонимы в реестре инвариантов**: переякоривание INV-20/21/25 обязано попасть в тот же коммит, что M1 (ручная дисциплина оркестратора; CI этого не проверяет).
6. **Коллизии имён с параллельными D1-документами** (у server-черновика есть `transport` — мы его избегнули через `mcpdial`; короткие `search`/`bash`/`wake` могут предложить другие): перед влитием волны — контрольный `git grep -h '^package '` по объединённому дереву; при коллизии переименование локально внутри полосы.
7. **Файлы 951–1000 строк** (view.go 957, mutation.go 967): разрезы — единственные правки содержимого файлов в дизайне; точки разреза не должны менять порядок инструкций (оракул объявлений + build/vet + тесты).
8. **Сиды второго храповика**: если новые каталоги зафиксировать позже их создания, CI падает раньше записи сида — сид вносится той же полосой, что создала каталоги (§5.2).

Открытые вопросы оператору:

1. Подтвердить **отказ от фасада**: импортёры переезжают на листья (138 файлов). Альтернатива — фасад-обёртки в родителе: минус два элемента в лимите (8 вместо 7 → отдельная запись храповика) и нулевое сужение тестовой матрицы.
2. Подтвердить исключение для **error_contract_test.go** (целиком в contracttest, §2.2) — формально нарушает «тест едет с кодом», но субъект теста — сам контракт.
3. Подтвердить имя `mcpdial` и запись храповика `internal/agent/tools/mcp 17`.
4. Что нельзя решить без запуска кода (честно): точные составы границ fsaccess/fileguard/search/webget/webread (даёт компилятор в полосах A2/A3); принадлежность трёх тестов (bounded_walk_tools_test, deny_exec_policy_test, create_parent_permission_test) — по идентификаторам в полосе; наличие скрытых пользователей `searchRegexCache` вне search (grep сегодня пуст).
