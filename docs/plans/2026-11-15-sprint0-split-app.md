# D1-app: дизайн разрезки `internal/app` (задача #74, 2026-11-15)

- Источник истины: `docs/plans/2026-11-15-sprint0-plan.md` (§3, §5 D1/0б, §6), данные — `docs/plans/2026-11-15-sprint0-modules-research.md` (И§2–§5, И§9, И§11.2).
- Область: пакет `internal/app` (композиционный корень: `App`, `New`, конвейер запуска `app_run_*`, batch-runtime, жизненный цикл, восстановление) + пакет `internal/app/itest`.
- Код в этом документе не пишется; документ — план полос 0б и D2. Пути к скриптам и результаты их прогонов приведены как данные; все ключевые факты проверены чтением кода (цитаты `файл:символ`).
- Редакция 2 (после ревью): пересчитаны все счётчики по свежему `ls`/`wc -l` (61 файл кода, 13 650 строк; кластер `app_run_*` — 29 файлов, 8 150 строк, что совпадает с И§3 «8150»); добавлены пропущенные в таблице `app_run_turn.go` и `sub_agents.go`; устранено противоречие по `protocol.go` (1193 > 1000) — протокол и результаты переехали в `review`, `strip_json.go` — тоже в `review`; числа `cutover.go` выведены из таблицы (356), итог appbatch 2206 сходится; уточнено размещение `quota`/`wake_hold`/`wait_interrupt` (все — методы `cliLoop`, едут в `cliloop`).
- Написано до этапа C: в этом worktree `internal/projects` ещё стоит на верхнем уровне, а `internal/app/itest` уже существует (волна 1). Все счётчики элементов ниже даны для состояния **после** этапа C, как требует задача.

## 1. Текущее состояние

### 1.1. Объём (пересчитано: `ls *.go | grep -v _test` = 61 файл; `cat` всех файлов = 13 650 строк; `ls *_test.go` = 226)

| Показатель | Значение | Источник |
|---|---|---|
| Рукописных кодовых файлов (не тесты) | **61 (13 650 строк)** | свежий пересчёт `ls` + `wc -l` |
| Тестовых файлов | 226 (43 118 строк), все `package app` (белые) | `prereq.py`: "white-box test files (package app): 226 of 226"; `ls *_test.go` = 226 |
| Импортируют пакет | 4: `internal/cmd`, `internal/server`, `internal/server/r15bfixture`, `sdk` | `analyze.py imports internal/app` |
| Файлов, импортирующих путь `internal/app` | 146 | `git grep -l "internal/app"` |
| Время `-short` | 878 с (самый тяжёлый набор; И§1) | `docs/ci-speed.md` |
| `init()` в производственном коде | **0** | проверено `grep -n "^func init" *.go` — пусто; совпадается с `prereq.py` ("init(): 0") |
| `go:embed` | **0** | `grep -rln "go:embed" *.go` — пусто |
| Пары файлов под ОС (производственные) | **0** | `ls *_windows* *_unix*` — только тестовая пара `app_batch_cutover_hosts_identity_{windows,unix}_test.go` |

### 1.2. Кластеры по префиксам файлов (пересчитано по свежему `wc -l`)

| Кластер | Файлов кода | Строк кода | Строк тестов (`analyze.py clusters`) | Содержание |
|---|---|---|---|---|
| `app_run_*` | **29** | **8 150** | ~22 700 (121 тест + `async` 6 + `run` 5 + `strip` 1 + `resolve` 2 + `admission` 1 + `p1` 2 + `p348` 2 + `r12` 1) | конвейер запуска: `ExecuteRun`, `cliLoop`, async-машина, reviewer, quota, admission; 29 файлов/8 150 строк совпадают с И§3 «8150» |
| `app_batch_*` + `batch_*` | 13 | 2 206 | ~7 900 (24 + 18 файлов) | batch-runtime, install, cutover, CLI, maintenance, fault, timeout-wake |
| `app_lifecycle*` | 1 | 452 | ~670 (1 + `shutdown` 1 + `release` 1) | `ShutdownResult`, `Shutdown*`, `releaseResources` |
| `app_recovery*` | 1 | 357 | ~960 (4 файла) | `recoverInterruptedTurns` при старте |
| ядро (остальное) | 17 | 2 485 | ~10 900 (21 `app_*` + билетные) | `App`, `New`, accessors, agent setup, config sync, session activity, финализация, провайдеры; префиксно сюда же попадают `strip_json.go`, `run_on_finish.go`, `sub_agents.go` — дизайном они едут в apprun/review (§2.5) |

Сумма: 29+13+1+1+17 = 61 файл; 8 150+2 206+452+357+2 485 = 13 650 строк ✓ (сходится с §1.1).

Совпадение с И§11.2 и `prereq.py`: предложенные подпакеты `['batch', 'core', 'lifecycle', 'recovery', 'run']`; цикл `batch <-> core <-> lifecycle <-> run`, `recovery` вне цикла. Замер `entangle.py internal/app 12 300` (префиксная кластеризация грубее — весь `app_*` одним кластером) даёт согласованную картину рёбер: `app -> batch: 8`, `batch -> app: 8`, `app -> strip: 3`.

### 1.3. Рёбра между кластерами (`prereq.py`, проверено чтением)

| Ребро | Идентификаторов | Проверка по коду |
|---|---|---|
| `run -> core` | 9: `App`, `ErrInvalidStripJSON`, `New`, `SessionActivity`, ... | `App` — приёмник методов; `ErrInvalidStripJSON` — `strip_json.go:21` (используется самим `strip_json.go`, в `app_run_reviewer.go:656` — только комментарий); `New` в `app_run_*` — это `errors.New` (шум счётчика, вызова `app.New` из run нет — проверено `grep "New(" app_run*.go` без `errors.New`); `SessionActivity` — метод ядра, вызывается `app_run_setup.go:221` |
| `core -> run` | 3: `RunResult`, `SubAgentOutput`, `cliLoopStderr` | `RunResult`/`SubAgentOutput` — `app_run_result.go:21,119` (типы результатов); `cliLoopStderr` — `app_run_async.go:270`, в ядре только комментарий `app_agent_setup.go:244` (шум) |
| `batch -> core` | 9: `App`, `New`, `apiServiceOwner`, `batchCommitDiagnostics`, ... | `App` — приёмник 38 методов batch; `apiServiceOwner` определён в batch (`batch_timeout_wake.go:37`) и вызывается только `app_batch_install.go:89` — внутри batch (шум направления); `batchCommitDiagnostics` определён и используется внутри batch (`batch_commit_diagnostics.go`, `app_batch_runtime.go:109`) |
| `core -> batch` | 8: `BatchRuntime`, `BatchRuntimeConfig`, `NewBatchRuntime`, `_` | поля `App`: `batchRuntime *BatchRuntime` (`app.go:116`), `batchTimeoutWake *batchTimeoutWakeDispatcher` (`app.go:145`), `batchFaultWriter *batchFaultEnvelopeWriter` (`app.go:154`); `New` вызывает `installBatchRuntime` (`app.go:457`), `peReleaseBatchRuntimeOnNewError` трогает `batchTimeoutWake.Stop`/`batchInstall` (`app_new_error_release.go`); `_` — заглушка `var _ batchRunner = (*jobbatch.Runner)(nil)` (`app_batch_runtime.go:34`) |
| `batch -> run` | 5: `RunResult`, `ToolCallStat`, `UsageInfo`, `persistSessionExecutionIntent` | `persistSessionExecutionIntent` — `app_run_setup.go:95`, вызывается `app_batch_cli.go:103`; типы `RunResult*` в производственном batch не используются (единственное упоминание — комментарий в `app_batch_fault_output.go`; ребро в основном из тестов `app_batch_finished_work_export_test.go`) |
| `run -> batch` | 2: `_`, `quotaWorkRow` | `quotaWorkRow` — `app_batch_cutover_facts.go:133`, используется `app_run_quota.go:217` (`cliLoop.quotaJobsInTree`); плюс методы batch, вызываемые из run: `batchFinishedWork` (`app_run_quota.go:218` — единственный производственный вызов), `batchCutoverReader/Writer` (`app_batch_cutover_facts.go:30,46`) |
| `lifecycle -> core` | 1: `App` | приёмник методов `Shutdown*` (`app_lifecycle.go:99,426`) |
| `lifecycle -> run` | 1: `transition` | **не подтверждается**: `transition` — `app_run_async_fsm.go:89`, единственный вызов `app_run_async.go:590`; в файлах lifecycle идентификатора нет (шум счётчика) |
| `core -> lifecycle`, `batch -> lifecycle` | 1: `ShutdownResult` | `ShutdownResult` — `app_lifecycle.go:19`; в batch — только комментарий (`app_batch_drain.go:24`), у batch свой тип `BatchDrainResult` (`app_batch_drain.go:18`) |

Дополнительный факт, найденный при ревизии редакции 2 и влияющий на размещение типов: **цикл merge-ит и строит результаты сам** — `app_run_async.go:63,116-192` (`flushLoopExit`, `loopTotals.add/applyTo/warningsFor` работают с `*RunResult`, `RunMode`, `SubAgentOutput`, `ToolCallStat`), `app_run_async.go:311` конструирует `RunRequest`, `app_run_wait_interrupt.go:60,78` вызывает `stripAndExtractJSON` и `buildSessionUsageInfo`, reviewer (`app_run_reviewer.go:48-50,385-393,665,694,707`) использует `RunMode`, `RunOverrides`, `stripAndExtractJSON`, суб-агентские сборщики. Поэтому весь протокол хода (`RunRequest`/`RunOverrides`/`RunMode`, `app_run_request.go`) и все типы результатов (`app_run_result.go`) обязаны лежать не выше самого нижнего потребителя — см. Ш1 и дерево §2.2.

Вывод: цикл тонкий и целиком объясняется тремя группами имён — (а) типы протокола/результатов, (б) поля `App`, ссылающиеся на batch-типы, (в) общие помощники (`persistSessionExecutionIntent`, `validateResolvedSession`, чтение cutover, `runAgentTurnRecovered`, суб-агентские сборщики). Отдельные ложные рёбра (`New`, `transition`, `cliLoopStderr`, `apiServiceOwner`, `ErrInvalidStripJSON`) — шум счётчика по комментариям и `errors.New`; в дизайне они не участвуют.

### 1.4. Развесовка методов `*App` (ограничение Go: методы одного типа живут в одном пакете)

Суммарно ~97 методов `*App` и 47 методов `cliLoop` (пересчитано `grep -c "func (l *cliLoop)"`)

| Кластер | Методов `*App` | Файлы |
|---|---|---|
| run | 23 | `app_run.go` 3, `app_run_async.go` 1, `app_run_drain_pending.go` 2, `app_run_gates.go` 3, `app_run_reviewer*.go` 5, `app_run_role.go` 1, `app_run_session.go` 6, `app_run_setup.go` 2, `app_run_terminal.go` 1 |
| batch | 38 | `app_batch_cli.go` 18, `app_batch_cutover_facts.go` 6, `app_batch_cutover_hosts.go` 6, `app_batch_fault_output.go` 1, `app_batch_install.go` 4, `app_batch_maintenance.go` 3 |
| lifecycle | 6 | `app_lifecycle.go` |
| recovery | 3 | `app_recovery.go` |
| ядро | ~27 | `app.go`, `app_accessors.go` 10, `app_agent_setup.go` 4, `app_config_sync.go` 6, `app_ephemeral_disk_binding.go` 1, `app_finalization_*` 3, `app_new_error_release.go` 4, `app_session_activity.go` 5, `app_session_drivers.go` 1, `finalization_gate.go` 2, `provider.go` 1, `sub_agents.go` 2 |

Файлы без приёмников `*App` (свободные функции): `app_run_turn.go` (`runAgentTurnRecovered`, `drainOutcomeError` — 0 методов), `strip_json.go`, `app_run_request.go`, `app_run_result.go` (5 функций без приёмников — проверено), `run_on_finish.go`. Методы `cliLoop` (47): async-файлы 38, `app_run_quota.go` 6, `app_run_wake_hold.go` 2, `app_run_wait_interrupt.go` 1 (итого 47, прочих нет).

Это главный механический факт дизайна: тело любого метода, чей файл уезжает из пакета `App`, обязано либо остаться в пакете `App`, либо превратиться в свободную функцию с явными параметрами. Отсюда порядок полос: сначала швы внутри одного пакета (метод → функция + набор возможностей), потом физический перенос.

### 1.5. Тесты и помощники

- 226/226 файлов — `package app`; распределение по префиксам: `app_run_*` 121, `app_batch_*` 24, `batch_*` 18, прочие `app_*` 21, `async_*` 6, `run_*` 5, `recovery` 4, билетные (`p1` 2, `p348` 2, `p480`, `p593`, `p616`, `p778`, `p421`, `p350`, `r5`, `r12`, ...) 12, остальные по 1.
- 16 тестовых файлов без `Test`-функций (помощники/фикстуры): `batch_timeout_wake_test_helpers_test.go`, `batch_fixture_port_f6{,a2,a3}_test.go`, `async_phase4_crash_process_test.go`, `async_phase4_batch_helpers_test.go`, `app_run_wait_batch_fixture_test.go`, `app_run_six_completion_routing_test.go`, `app_run_router_batch_test.go`, `app_run_loop_round4_test.go`, `app_run_e3_helpers_test.go`, `app_run_chain_guard_gates_test.go`, `app_finalization_e2e_harness_test.go`, `app_batch_provider_crash_child_test.go` (ре-exec дочернего процесса), ОС-пара `app_batch_cutover_hosts_identity_{windows,unix}_test.go`.
- `prereq.py`: "test helpers shared across proposed boundaries: 101 ids in 11 directions" — второй по величине показатель после `agent`. Основные направления: фикстуры batch, используемые тестами run (`app_run_wait_batch_fixture_test.go`, `app_run_router_batch_test.go`, `batch_facts_app_run_test.go`); общий конструктор `App` в тестах; harness финализации (`app_finalization_e2e_harness_test.go`).
- Тестовая пара под ОС: `app_batch_cutover_hosts_identity_windows_test.go` + `_unix_test.go` — едут вместе (§3 п. 5 плана).
- Медленные наборы (878 с; И§7 — много фиксированных ожиданий; ускорение вне спринта): async-ожидания (`app_run_await_tasks_test.go` 17.4 КБ, `app_run_async_completion_test.go` 15.9 КБ, `app_run_continuation_*_test.go` 24+30 КБ, `app_run_chain_guard_e2e_test.go` 15.7 КБ) — кластер run/cliloop; `app_batch_provider_crash_test.go` 18.6 КБ и `app_batch_cutover_hosts_test.go` 39.4 КБ — batch. Полосы D2 по этим пакетам обязаны запускать только серийные точечные прогоны затронутых пакетов (§3 п. 7 плана).

## 2. Целевое дерево

### 2.1. Принципы, из которых выведено дерево

1. **Все методы `*App` остаются в пакете типа** (`appkernel`): тип `App` с ~97 методами переезжает целиком, поэтому снаружи (146 файлов) меняется только... ничего: путь `github.com/PHPCraftdream/rush/internal/app` сохраняется фасадом (2.3), синтаксис вызовов не меняется ни в одном из 146 файлов.
2. **Конвейер запуска и batch становятся свободными функциями над явными наборами возможностей** (Ш2, Ш3): тела методов конвертируются в функции, принимающие `RunEnv`/параметры вместо `*App`, ещё внутри старого пакета (полоса 0б), после чего физический перенос становится чистым.
3. **Протокол и результаты живут в листе `review`**: `cliloop` строит `RunRequest` и merge-ит `*RunResult` (`app_run_async.go:63-192,311`), reviewer использует `RunMode`/`RunOverrides` и суб-агентские сборщики — значит, типы и общие помощники хода (`runAgentTurnRecovered`, `stripAndExtractJSON`, сбор суб-агентских выводов) лежат в `review`, который ниже и `cliloop`, и корня apprun. Направление рёбер: `apprun -> cliloop -> review`, `apprun -> review`, `apprun -> appbatch`, `cliloop -> appbatch`.
4. Имена пакетов уникальны в репозитории (проверено `git grep -h '^package ' -- '*.go' | sort -u`: имён `appkernel`, `apprun`, `appbatch`, `apprecovery`, `cliloop`, `appreview`, `apptest` нет ни в одном виде); запрещённых (`core`, `util`, `common`, `base`) нет; каждое имя понятно без пути.

### 2.2. Дерево и счёт элементов (лимит 7 на каталог)

```
internal/app/                        package app — ПУБЛИЧНЫЙ API (фасад), путь импорта не меняется
├── app.go                           фасад: синонимы типов + функции-обёртки (~250 строк)     [файл]
├── appkernel/                       package appkernel — App, New, жизненный цикл, shared     7 элементов
│   ├── app.go                       App, New, Option, adapter, heartbeatDir, PE-release (795)
│   ├── lifecycle.go                 Shutdown*, releaseResources, ShutdownResult (452)
│   ├── agent_setup.go               InitCoderAgent, GetDefaultFastModel (252)
│   ├── accessors.go                 accessors + provider + config sync (379)
│   ├── session.go                   SessionActivity + session drivers (302)
│   ├── wiring.go                    finalization gate/queue/authority/wiring + turn_stall + ephemeral disk (222)
│   └── apptest/                     package apptest — тестовые помощники поддерева app       7-й элемент
├── apprun/                          package apprun — конвейер запуска                        6 элементов
│   ├── run.go                       ExecuteRun + роли (795)
│   ├── setup.go                     prepareExecuteRun (696)
│   ├── admission.go                 gates + session resolve + drain_pending (491)
│   ├── terminal.go                  terminal + timeout + run_on_finish + errors (653)
│   ├── cliloop/                     package cliloop — цикл cliLoop, async-машина, quota      5 элементов
│   │   ├── async.go                 cliLoop, главный цикл (~800; резка из 993)
│   │   ├── async_tail.go            хвост async.go (~193)
│   │   ├── async_states.go          fsm + ended + exit (430)
│   │   ├── waits.go                 wait + stop + nudge + wake_hold + wait_interrupt (572)
│   │   └── quota.go                 app_run_quota.go целиком — все функции с приёмником cliLoop (233)
│   └── review/                      package appreview — reviewer-ход, протокол и результаты  7 элементов
│       ├── reviewer.go              reviewer.go после резки (~800 из 982)
│       ├── reviewer_tail.go         хвост reviewer (~182) + sub_agents.go (121) (~303)
│       ├── evidence.go              evidence.go после резки (~800 из 908)
│       ├── evidence_tail.go         хвост evidence (~108)
│       ├── outcome.go               outcome + drain_identity + prompt + stall + timeout + turn (690)
│       ├── strip_json.go            чистый перенос (364)
│       └── protocol.go              request (285) + result (544): RunRequest/RunOverrides/RunMode + RunResult-семейство (829)
├── appbatch/                        package appbatch — batch-исполнение                      7 элементов
│   ├── cli.go                       app_batch_cli + app_batch_doc (441)
│   ├── cutover.go                   cutover_facts целиком + cutover_hosts (356)
│   ├── install.go                   app_batch_install + batch_install_factory (413)
│   ├── runtime.go                   app_batch_runtime + batch_commit_diagnostics (322)
│   ├── maintenance.go               app_batch_maintenance + app_batch_drain (244)
│   ├── fault.go                     app_batch_fault + app_batch_fault_output (160)
│   └── timeout_wake.go              batch_timeout_wake (270)
├── apprecovery/                     package apprecovery — восстановление при старте          1 элемент
│   └── recovery.go                  recoverInterruptedTurns (357)
├── itest/                           package itest — сценарные тесты (существует, 10 файлов)   не меняется
└── projects/                        package projects — приезжает этапом C (3 файла)          не меняется
```

Итоговые счётчики элементов: корень `internal/app` = **7** (фасад `app.go` + 6 каталогов; `testdata` — некодовый ресурс, не считается); `appkernel` = **7** (6 файлов + `apptest/`); `apprun` = **6** (4 файла + `cliloop/` + `review/`); `cliloop` = **5**; `review` = **7**; `appbatch` = **7**; `apprecovery` = **1**; `itest` = 10 (существует); `projects` = 3 (после этапа C). Лимит 7 соблюдён везде.

Имена файлов-приёмников — новые (склейка мелких файлов в один пакет — чистый перенос: список объявлений не меняется, меняются только имена файлов).

### 2.3. Публичный API и фасад

Путь `github.com/PHPCraftdream/rush/internal/app`, имя пакета `app` сохраняются. Фасад `app.go` содержит:

- синонимы типов: `App = appkernel.App`, `Option = appkernel.Option`, `ShutdownResult = appkernel.ShutdownResult`, `SessionActivityBatchResult = appkernel.SessionActivityBatchResult`; `RunRequest = appreview.RunRequest`, `RunOverrides = appreview.RunOverrides`, `RunMode = appreview.RunMode`, `RunResult = appreview.RunResult`, `SessionUsageInfo = appreview.SessionUsageInfo`, `UsageInfo = appreview.UsageInfo`, `ToolCallStat = appreview.ToolCallStat`, `SubAgentOutput = appreview.SubAgentOutput`; `RunTimeoutError = apprun.RunTimeoutError`, `RunIncompleteError = apprun.RunIncompleteError`, `RecoveredPartial = apprun.RecoveredPartial` — синонимы не создают изменяемых глобалов и сохраняют множество методов.
- функции-обёртки: `New`, `SkipAgentSetup`, `SkipMCP`, `RestrictMCPToCLI`, `WithMCPOwner`, `WithBatchFaultJSONOutput`, `NewRunIncompleteError` — по одной строке делегирования каждая.
- Больше ничего. Все методы (`ExecuteRun`, `RunNonInteractive`, `Shutdown`, `Store`, `ProcessHome`, `BatchCLIStore`, ...) видны через синоним `App` и не дублируются. Экспортированные поля (`Sessions`, `Messages`, `Permissions`, `AgentCoordinator`, `DB`, `RunQueuePump`) переезжают вместе с `App`. Фасад импортирует `appkernel`, `appreview` и `apprun` (только для трёх ошибок run) — фасад стоит на вершине поддерева, направление рёбер вниз.

Фасад заменяет сегодня-экспортируемые из `internal/app` имена один в один; полный список внешних имён снят grep'ом потребителей (`cmd`, `server`, `sdk`, `r15bfixture`): `App` ×202, `Sessions` ×84, `RunResult` ×42, `AgentCoordinator` ×42, `New` ×30, `Permissions` ×19, `Messages` ×15, `Store` ×14, `BatchCLIStore` ×10, `Option` ×8, `DB` ×7, опции-конструкторы, `Shutdown`/`ShutdownWithResult`/`ShutdownAfterDrain`, `RunTimeoutError`, `RunOverrides`, `RunNonInteractive`, `RunMode*`, `ResolveModel`, `ProcessHome`, `ExecuteRun`, `UsageInfo`, `ToolCallStat`, `SubAgentOutput`, `SessionUsageInfo`, `SessionActivityBatchResult`, `RunRequest`, `RunIncompleteError`, `NewRunIncompleteError`, `ResolveConfiguredModel`, `RecoveredPartial`, `DataDirectory`, `BatchWait`, `BatchSubmitRefWithQuestionRoute`, `BatchSettleDebt`. Всё перечисленное покрыто синонимами/обёртками/методами `App`.

Правило слоёв: поддерево `app` лежит в слое `app`; рёбра между подпакетами ацикличны (2.4).

### 2.4. Граф зависимостей новых пакетов (стрелки = импортирует)

```
appreview   → (только нижние слои: agent, session, message, config)
appbatch    → (только нижние слои: agent, session, config, db, jobbatch)
apprecovery → (только нижние слои: session, message, config, db)
cliloop     → appreview, appbatch
apprun      → cliloop, appreview, appbatch
appkernel   → apprun, appbatch, apprecovery
app (фасад) → appkernel, appreview, apprun
itest       → всё (разрешено явно; остаётся единственным «импортирующим всё»)
apptest     → appkernel, apprun, appbatch, apprecovery (тестовые помощники; в производственных замыканиях не участвует)
```

Циклов нет. Горизонтальные рёбра производственного кода: `apprun → appbatch` (6 идентификаторов, Ш4), `cliloop → appbatch` (2: `batchFinishedWork`, `quotaWorkRow` для quota-цикла), `cliloop → review` и `apprun → review` (протокол/результаты/помощники хода, Ш1/Ш3). Рёбра вверх запрещены и будут проверяться линтером слоёв (A2, #88).

### 2.5. Полная таблица «файл → целевой пакет» (61 рукописный файл; пересчитано `ls *.go | grep -v _test` = 61)

| Файл (строк) | Цель | Примечание |
|---|---|---|
| `app.go` (656) | `appkernel/app.go` | + хвост из `app_new_error_release.go`; без изменений объявлений |
| `app_new_error_release.go` (139) | `appkernel/app.go` | PE-хвост `New`; склейка 656+139=795 |
| `app_lifecycle.go` (452) | `appkernel/lifecycle.go` | `ShutdownResult` остаётся здесь (ниже ядра никто его не использует — §1.3) |
| `app_accessors.go` (76) | `appkernel/accessors.go` | склейка с `provider.go`, `app_config_sync.go` |
| `provider.go` (148) | `appkernel/accessors.go` | `ResolveModel`, `ResolveConfiguredModel` |
| `app_config_sync.go` (155) | `appkernel/accessors.go` | + глобалы `ConfigNotificationsPublished`, `modelsSyncBeforeJoinSeam`; склейка 76+148+155=379 |
| `app_agent_setup.go` (252) | `appkernel/agent_setup.go` | `InitCoderAgent`; комментарий `cliLoopStderr` здесь — шум |
| `app_session_activity.go` (236) | `appkernel/session.go` | `SessionActivity`, `SessionActivityBatchResult` |
| `app_session_drivers.go` (66) | `appkernel/session.go` | склейка 236+66=302 |
| `finalization_gate.go` (59) | `appkernel/wiring.go` | + глобал `appFinalizationGates` |
| `app_finalization_queue.go` (27) | `appkernel/wiring.go` | `runMarkedFinalizationQueue` (вызывается из `app.go:61`) |
| `app_finalization_authority.go` (28) | `appkernel/wiring.go` | `finalizationAuthorityRegistry` |
| `app_finalization_wiring.go` (50) | `appkernel/wiring.go` | склейка 59+27+28+50+23+35=222 |
| `app_turn_stall_option.go` (23) | `appkernel/wiring.go` | |
| `app_ephemeral_disk_binding.go` (35) | `appkernel/wiring.go` | `newEphemeralDiskBindings`, `resolveDiskBindings` |
| `app_run.go` (723) | `apprun/run.go` | `ExecuteRun` → функция `executeRun(env, req)` + обёртка-метод в ядре |
| `app_run_role.go` (72) | `apprun/run.go` | `runRoleName` нужен `classifyRunTimeout`'у в run.go; склейка 723+72=795; `resolveRunRole` — свободная функция над env |
| `app_run_setup.go` (624) | `apprun/setup.go` | `prepareExecuteRun`; `persistSessionExecutionIntent` уезжает в appbatch (Ш4) |
| `app_run_gates.go` (225) | `apprun/admission.go` | склейка с session/drain_pending |
| `app_run_session.go` (168) | `apprun/admission.go` | `resolveRunSession`; `validateResolvedSession` уезжает в appbatch (Ш4) |
| `app_run_drain_pending.go` (98) | `apprun/admission.go` | + глобал `drainPendingAfterAttemptSeam`; склейка 225+168+98=491 |
| `app_run_terminal.go` (265) | `apprun/terminal.go` | склейка с timeout/run_on_finish/errors |
| `app_run_timeout.go` (76) | `apprun/terminal.go` | `classifyRunTimeout`, `RunTimeoutError` |
| `run_on_finish.go` (50) | `apprun/terminal.go` (eventual grouping; Task #14 delivered standalone `apprun/run_on_finish.go`) | |
| `app_run_errors.go` (262) | `apprun/terminal.go` | `ErrRunQueued`, `RunIncompleteError`, `NewRunIncompleteError`, `RecoveredPartial`; склейка 265+76+50+262=653 |
| `app_run_request.go` (285) | `review/request.go` | `RunMode` используется cliloop (`app_run_async.go:63-72,203`) и review (`app_run_reviewer.go:48,385-393`) — тип обязан быть ниже обоих |
| `app_run_result.go` (544) | `review/results.go` | `RunResult` merge-ится в cliloop (`loopTotals`, `app_run_async.go:116-192`), `buildSessionUsageInfo` вызывается из `app_run_wait_interrupt.go:78` — типы и сборщики ниже cliloop |
| `strip_json.go` (364) | `review/strip_json.go` | чистый перенос; вызовы `app_run_reviewer.go:665` (review) и `app_run_wait_interrupt.go:60` (cliloop) — оба импортируют review вниз |
| `app_run_turn.go` (202) | `review/outcome.go` | `runAgentTurnRecovered` вызывается только из `app_run_reviewer.go:132`, `drainOutcomeError` — из `app_run_reviewer.go:241,328`; склейка 220+108+104+44+12+202=690 |
| `sub_agents.go` (121) | `review/reviewer_tail.go` | `subAgentSummaryStats`/`collectSubAgentOutputs` вызываются из `app_run_reviewer.go:694,707` (review) и `app_run_quota.go:169`, `app_run_wait_interrupt.go:74` (cliloop → импорт review); склейка ~182+121≈303 |
| `app_run_async.go` (993) | `cliloop/async.go` + `async_tail.go` | резка по В7: ≤ 800 + ~193; + глобалы `cliLoopStderr`, `cliLoopTurnDoneSeam`, `cliLoopBeforeDrainSeam`, тайминги |
| `app_run_async_fsm.go` (210) | `cliloop/async_states.go` | `transition`, `cliPhase`, `cliEvent` |
| `app_run_async_ended.go` (98) | `cliloop/async_states.go` | склейка 210+98+122=430 |
| `app_run_async_exit.go` (122) | `cliloop/async_states.go` | |
| `app_run_async_wait.go` (138) | `cliloop/waits.go` | склейка с wait-семейством |
| `app_run_async_stop.go` (115) | `cliloop/waits.go` | |
| `app_run_async_nudge.go` (160) | `cliloop/waits.go` | |
| `app_run_wake_hold.go` (66) | `cliloop/waits.go` | оба метода — `(l *cliLoop)`; склейка 138+115+160+66+93=572 |
| `app_run_wait_interrupt.go` (93) | `cliloop/waits.go` | метод `(l *cliLoop)` |
| `app_run_quota.go` (233) | `cliloop/quota.go` | все функции файла — методы `(l *cliLoop)` (6 шт.) + `quotaQuestionText`; отдельным файлом |
| `app_run_reviewer.go` (982) | `review/reviewer.go` + `reviewer_tail.go` | резка по В7: ~800 + ~182 |
| `app_run_reviewer_evidence.go` (908) | `review/evidence.go` + `evidence_tail.go` | резка по В7: ~800 + ~108; + 5 regexp-глобалов, `reviewCommandFilters` |
| `app_run_reviewer_outcome.go` (220) | `review/outcome.go` | склейка в outcome.go |
| `app_run_reviewer_drain_identity.go` (108) | `review/outcome.go` | |
| `app_run_reviewer_prompt.go` (104) | `review/outcome.go` | |
| `app_run_reviewer_stall.go` (44) | `review/outcome.go` | + `ErrReviewerUnresponsive` |
| `app_run_reviewer_timeout.go` (12) | `review/outcome.go` | |
| `app_batch_cli.go` (358) | `appbatch/cli.go` | + `app_batch_doc.go`: 358+83=441; 18 методов `*App` → функции (Ш5) |
| `app_batch_doc.go` (83) | `appbatch/cli.go` | |
| `app_batch_cutover_facts.go` (221) | `appbatch/cutover.go` | файл едет ЦЕЛИКОМ: чтение cutover (trio + `quotaWorkRow`) остаётся здесь и экспортируется для apprun/cliloop (Ш4) |
| `app_batch_cutover_hosts.go` (135) | `appbatch/cutover.go` | склейка 221+135=356; + глобалы `batchHostStores`, `batchNoticeStores` |
| `app_batch_install.go` (266) | `appbatch/install.go` | + `batch_install_factory.go`: 266+147=413; + 3 hook-глобала |
| `batch_install_factory.go` (147) | `appbatch/install.go` | |
| `app_batch_runtime.go` (264) | `appbatch/runtime.go` | + `batch_commit_diagnostics.go`: 264+58=322; `BatchRuntime`, `NewBatchRuntime`, `batchRunner` |
| `batch_commit_diagnostics.go` (58) | `appbatch/runtime.go` | |
| `app_batch_maintenance.go` (130) | `appbatch/maintenance.go` | + `app_batch_drain.go`: 130+114=244; + `batchMaintenanceLoops` |
| `app_batch_drain.go` (114) | `appbatch/maintenance.go` | `BatchDrainResult`, `releaseBatchRuntimeGate` |
| `app_batch_fault.go` (79) | `appbatch/fault.go` | склейка 79+81=160; + глобал `batchFaultExit` |
| `app_batch_fault_output.go` (81) | `appbatch/fault.go` | `batchFaultEnvelopeWriter`, `handleBatchFaultOutput` |
| `batch_timeout_wake.go` (270) | `appbatch/timeout_wake.go` | `batchTimeoutWakeDispatcher`, `apiServiceOwner` |
| `app_recovery.go` (357) | `apprecovery/recovery.go` | + глобалы `recoverSessionListSeam`, `recoverSessionPreWriteSeam` |

Итог по строкам, выведенный из таблицы (сходится с пересчётом §1.1: 61 файл / 13 650 строк):

- `appkernel` = 795+452+252+379+302+222 = **2 402** (6 файлов);
- `apprun` корень = 795+696+491+653 = **2 563** (4 файла);
- `cliloop` = 800+193+430+572+233 = **2 228** (5 файлов);
- `review` = 800+303+800+108+690+364+829 = **3 894** (7 файлов; `reviewer_tail` = 182+121=303, `evidence_tail` = 108, `protocol.go` = 285+544=829);
- `appbatch` = 441+356+413+322+244+160+270 = **2 206** (7 файлов);
- `apprecovery` = **357** (1 файл); фасад ~250 (новый файл, в 13 650 не входит).

Контроль: поддерево apprun = 2 563+2 228+3 894 = **8 685**; всего 2 402 + 8 685 + 2 206 + 357 = **13 650** ✓ — сумма источников таблицы равна пересчёту §1.1 (61 файл, 13 650 строк). Максимальный файл после резки В7 в D2-3 — **829** (`protocol.go`), три режущихся файла (`async.go`, `reviewer.go`, `evidence.go`) режутся до цели 800 с жёстким пределом приёмки 829; до резки тремя самыми большими остаются исходные 993/982/908, которые режутся в той же полосе D2-3, поэтому жёсткий предел 1000 и предупредительный 900 соблюдены на каждом шаге; `protocol.go` = 829 — под целью 800, но в пределах 900, и дальше режется только вместе с ростом числа элементов review (уже 7), что запрещено лимитом.

### 2.6. Тесты: распределение по префиксам (по поведению)

Правило: тест переезжает в пакет, чьё поведение он проверяет (идентификаторы, которые он трогает); сценарии на несколько модулей — в `itest`. Первая грубая нарезка по имени файла, финальная — по используемым идентификаторам в полосе переноса:

| Тесты (префикс) | Файлов | Цель |
|---|---|---|
| `app_run_async_*`, `app_run_loop_*`, `app_run_quota_*` (все методы cliLoop), `async_*` | ~18 | `apprun/cliloop/` |
| `app_run_reviewer_*`, `strip_*` (тест strip_json), `sub_*` (тест sub_agents), билетные тесты протокола (`app_run_result*`, `app_run_request*`) | ~11 | `apprun/review/` |
| остальные `app_run_*` (121 − ~29), `run_*` 5, `resolve` 2, `admission` 1, `p1` 2, `p348` 2, `r12` 1 | ~109 | `apprun/` |
| `app_batch_*` 24, `batch_*` 18, ОС-пара | 44 | `appbatch/` |
| `app_recovery`/`recovery` 4 + `agent_turn_recover_test.go` | 5 | `apprecovery/` |
| прочие `app_*` 21 + `lifecycle`/`shutdown`/`release` 3 + `provider` 1 + `finalization` 1 + `peak` 1 + `catalog` 1 + `main` 1 + билетные ~8 | ~38 | `appkernel/` |
| общие помощники из 16 без-Test файлов: batch-фикстуры, harness финализации, конструктор `App` | — | `appkernel/apptest/` |
| помощники одного кластера (`async_phase4_*`, `batch_timeout_wake_test_helpers`) | — | едут со своим кластером |

Сид второго храповика (A1b, «все файлы каталога с тестами ≤ 21, число только падает») на момент разрезки переопределяется по факту: в `apprun/` тестовых файлов останется ~110 — это осознанный сид, распределение по поведению продолжает этап F (#77), а не D2.

## 3. Швы

### Ш1. Протокол и результаты — в `appreview` (request.go/results.go); `ShutdownResult` — в `appkernel/lifecycle.go`

- Идентификаторы: `RunRequest`, `RunOverrides`, `RunMode` (+константы) — `app_run_request.go` (285); `RunResult`, `SessionUsageInfo`, `UsageInfo`, `ToolCallStat`, `SubAgentOutput` + сборщики `buildRunResult`/`buildSessionUsageInfo`/`tailN`/`synthesiseEmptyFinalSummary`/`peakPauseFinalText` — `app_run_result.go` (544). Всё уже экспортировано; экспорт **не расширяется**.
- Обоснование размещения: `cliloop` строит `RunRequest` и merge-ит `*RunResult` (`app_run_async.go:63-72,116-192,311`), вызывает `stripAndExtractJSON` (`app_run_wait_interrupt.go:60`) и `buildSessionUsageInfo` (`:78`); review использует `RunMode`/`RunOverrides` (`app_run_reviewer.go:48-50,385-393`) и суб-агентские сборщики (`:694,707`). Потребители лежат в `cliloop` и `apprun`, значит типы — в `review` (лист под обоих). Размещение типов в корне apprun (редакция 1) создавало бы цикл `apprun → cliloop → apprun` и отвергнуто.
- Граница: `cliloop → review` (типы + `runAgentTurnRecovered` + `stripAndExtractJSON`, ~8-10 имён), `apprun → review` (`executeRun` собирает итог через `buildRunResult`, ~8-10 имён), `appkernel → apprun` (обёртки-методы возвращают `*RunResult`, 3-4 имени). Все ≤ 15.
- `ShutdownResult` (`app_lifecycle.go:19`) не переезжает: потребители — ядро и фасад.
- Сохранение поведения: типы данных и чистые функции; перенос чистый (5 функций `app_run_result.go` не имеют приёмников — проверено; `app_run_turn.go` — две свободные функции, приёмников нет).

### Ш2. Порт run: `RunEnv` + конвертация методов run в функции (полоса 0б, до переноса)

Run-cluster читает из `App` ровно 7 неэкспортированных полей и ~15 экспортированных членов (замер §1.3 + grep; полный список различимых обращений `app.X`/`a.X`: `Sessions`, `AgentCoordinator`, `Messages`, `Permissions`, `config`, `DB`, `RunQueuePump`, `WakeScheduleStore`, `dataDir`, `workspaceRoot`, `home`, `mcpOwner`, `batchFaultWriter`/`batchFaultOutputMu`/`batchFaultOutput`, `SessionActivity`, `ProcessHome`, `resolveDiskBindings`, `batchCutoverReader/Writer/batchFinishedWork`).

- В `package app` вводится `type RunEnv struct { Sessions session.Service; Coord agent.Coordinator; Messages message.Service; Perms permission.Service; Store *config.ConfigStore; DB func() *sql.DB; DataDir, WorkspaceRoot string; Home bool; MCPOwner *mcp.Owner; Pump *session.RunQueuePump; WakeStore *session.WakeScheduleStore; SessionActivity func(context.Context, string) (SessionActivityBatchResult, error); ProcessHome func() bool; DiskBindings func() *agent.DiskProviderRegistry; WriteFault func(error) }` (~16 полей — чуть выше ориентира 15, но это один тип-набор возможностей, а не 16 экспортируемых имён; поля — нижнеслойные типы или замыкания).
- Тела методов run-кластера конвертируются: `func (app *App) ExecuteRun(...)` → `func executeRun(env RunEnv, req RunRequest) ...` + тонкая обёртка-метод в ядре `func (app *App) ExecuteRun(...) { return executeRun(app.runEnv(), req) }`. Аналогично `RunNonInteractive`, `RunNonInteractiveWithResult`. Внутренние методы run (`prepareExecuteRun`, `resolveRunSession`, `resolveSession`, ...) — свободные функции с `env`-параметром.
- Точки изменения вызовов: только внутри run-кластера (`app.X` → `env.X` механически); вызовы снаружи не меняются (обёртки сохраняют методы).
- Сохранение поведения: замыкания `SessionActivity`/`DiskBindings`/`WriteFault` захватывают `app` в момент конструирования `env` — те же значения читаются в те же моменты; ленивые вызовы остаются ленивыми (§3 п. 12 плана): `WriteFault` вызывается только при факте fault, `DiskBindings` — на каждом resolve (как `resolveDiskBindings` сегодня).
- Момент вычисления полей `env`: конструируется в обёртке на входе метода; поля типа `func` намеренно ленивые, значения (`Sessions` и т. п.) — неизменяемые ссылки на сервисы, создаваемые в `New` до первого run.

### Ш3. Порт cliloop: `LoopHost` (инверсия драйвера)

`cliLoop` (52 метода, `app_run_async.go:198`) — собственный тип run-кластера; его подпакет `cliloop` лежит ниже `apprun`, поэтому он не может вызывать функции `apprun` напрямую.

- В `cliloop` объявляется `type LoopHost interface { ... }` — обратные вызовы цикла в конвейер (исполнение хода — цикл сам вызывает `app.ExecuteRun` в `app_run_async.go:311`, значит этот вызов становится `host.executeRun`, — классификация таймаута, admission-решения). Ожидаемый размер 4–8 методов; точный список — артефакт полосы 0б (шаг инвентаризации: все вызовы `apprun`-функций из файлов `app_run_async*`, `app_run_quota.go`, `app_run_wake_hold.go`, `app_run_wait_interrupt.go`). Реализуется `apprun`'ом неявно; `apprun` конструирует цикл: `cliloop.New(env LoopEnv, host LoopHost)`.
- Если инвентаризация даст > 15 методов — запасной вариант, фиксируемый в 0б-полосе до переноса: тело главного цикла переезжает в `cliloop`, а `apprun` оставляет себе только `prepareExecuteRun` + терминальную часть (граница тогда `apprun → cliloop` по типам из `review`, направление сохраняется).
- Сохранение поведения: `transition(phase, ev)` (`app_run_async_fsm.go:89`) остаётся внутри `cliloop`; порядок наблюдаемых событий (подписка на события сообщений до старта хода, seam-вызовы `executeRunBeforeTurnLaunchSeam`/`executeRunDoneCaseSeam`) не меняется — seam-переменные переезжают в тот пакет, чьё событие они сторожат.

### Ш4. Общие помощники run+batch — вниз, в `appbatch` (устраняет `batch -> run`)

- `persistSessionExecutionIntent` (`app_run_setup.go:95`; вызовы `app_run.go:384`, `app_batch_cli.go:103`) переезжает в `appbatch` и принимает `sessions session.Service` вместо `*App` (внутри она читает только сервис сессий — проверить при переносе; если найдутся поля `App` — расширить сигнатуру явными параметрами). `apprun` вызывает её через импорт `appbatch` — направление совпадает с Ш2.
- `validateResolvedSession` (`app_run_session.go:50`; используется run и batch) — аналогично: `ValidateResolvedSession(sess session.Session, id, workspaceRoot string, home bool) error` в `appbatch`.
- Чтение cutover: `batchCutoverWriter`, `batchCutoverReader`, `batchFinishedWork`, тип `quotaWorkRow` (`app_batch_cutover_facts.go`) остаются в `appbatch` ЦЕЛИКОМ (файл 221 строка едет без расщепления), экспортируются (`CutoverWriter`, `CutoverReader`, `BatchFinishedWork`); потребители после переноса — `apprun` и `cliloop` (оба импортируют `appbatch`).
- Граница `apprun → appbatch`: 6 идентификаторов (≤ 15). Сохранение поведения: тело функций не меняется, меняется только приёмник/параметры; порядок вызовов `persistSessionExecutionIntent` внутри `BatchCLISpec` (`app_batch_cli.go:103`) не сдвигается.

### Ш5. Поля `App` batch-типов → порты ядра; реестры keyed-by-App → владелец в `appbatch`

Ядро (`appkernel`) стоит выше `appbatch`, поэтому конкретные batch-типы на полях `App` легальны (`appkernel → appbatch`). Но `appbatch` не может читать поля `App`, поэтому:

- `installBatchRuntime` (`app_batch_install.go`) → `appbatch.InstallRuntime(params InstallParams) (rt *BatchRuntime, wake *TimeoutWakeDispatcher, ingress ..., err error)`; ядро-обёртка раскладывает результаты по полям `App`. `InstallParams` — явный набор (dataDir, DB/readDB-провайдеры, writer/release-замыкания, `batchFaultOutput`, hooks) ~10 полей.
- Граница `appkernel → appbatch` — самая широкая в дизайне: `NewBatchRuntime`, `BatchRuntimeConfig`, `InstallRuntime`, `BatchCLIStore`, `BatchSubmitRefWithQuestionRoute`, `BatchWait`, `BatchSettleDebt`, `BatchAutoApproveOwner`, `BatchOwnerRuntime`, `DataDirectory` (остаётся accessor'ом ядра), `ReleaseBatchRuntimeGate`, `StopBatchMaintenance`, `HandleBatchFaultOutput`, `HostRelease`/`HostReleaseKeepLock`, `Attach`/`Stop` wake-диспетчера ≈ 14–15 идентификаторов — на верхней границе ориентира. Если инвентаризация полосы 0б даст > 15: порт сжимается одним типом `type Runtime struct` с методами (смежный вариант — два порта: install-набор и lifecycle-набор); решение фиксируется в 0б-полосе, разрезка не начинается до фиксации.
- Реестры `batchHostStores`, `batchNoticeStores` (`app_batch_cutover_hosts.go:23,26`), `batchMaintenanceLoops` (`app_batch_maintenance.go:48`) — `sync.Map` с ключом `*App`. Переносятся в `appbatch` с ключом `*Owner` — тип, объявленный в `appbatch`; `App` хранит одно поле `batchOwner *appbatch.Owner` (создаётся в `New`, несёт dataDir и замыкания writer/reader). Ленивая сборка `HostStore` «при первом использовании» (`cutover_hosts.go:44`) сохраняется дословно — меняется только тип ключа.
- `appFinalizationGates` (`finalization_gate.go:19`) остаётся в ядре (`wiring.go`): финализация — домен ядра, вне batch.
- Сохранение поведения: `sync.Map`-мемоизация живёт ровно столько же (время жизни `Owner` = время жизни `App`; `batchCutoverStoresForget` вызывается из того же `releaseResources`-пути).

### Ш6. Жизненный цикл остаётся в ядре, восстановление — в `apprecovery`

- `Shutdown`/`ShutdownWithResult`/`ShutdownAfterDrain`/`releaseResources` остаются методами `App` в `appkernel/lifecycle.go`: они пишут приватное состояние (`shutdownOnce`, `shutdownResult`, `cleanupFuncs` — `app_lifecycle.go`) и вызывают по одному-два входа batch (`StopBatchMaintenance`, `ReleaseBatchRuntimeGate`, `batchRuntime.Stop` через порт Ш5, `HostRelease*`) и ядра (`stopConfigDiskSync`). Вынесение их из пакета `App` потребовало бы экспортировать эти поля — запрещено правилом В6/§3 п. 3.
- `recoverInterruptedTurns` + `recoverSessionInterruptedTurn` (`app_recovery.go`) → `apprecovery.RecoverInterruptedTurns(p RecoverParams)`: параметры — `Sessions`, `Messages`, `Store`, `dataDir`, тест-швы `recoveryOrphanAge *time.Duration`, `recoveryDataDir *string` (поля `App` `app.go:231,238` передаются явно). Вызов из `New` (`app.go:517`) сохраняет позицию и порядок (после установки `mcpOwner`, до `InitCoderAgent`).
- Ребро `lifecycle -> run: transition` из `prereq.py` не существует (шум, §1.3) — шва не требует.

### Ш7. Фасад (сохранение пути и 146 импортёров)

Один файл `internal/app/app.go` (синонимы + обёртки, §2.3). Поведение: синоним типа в Go сохраняет множество методов и тождественность типа, поэтому `app.App` ↔ `appkernel.App` — один тип; обёртки-функции — прямое делегирование. Никаких новых экспортируемых переменных.

## 4. Глобалы, init(), embeds, ОС-файлы, белые тесты, помощники

### 4.1. Глобальные переменные пакета (полный список производственных `var`, каждому — ровно один владелец)

| Глобал | Где сегодня | Владелец после | Тип/обработка |
|---|---|---|---|
| `batchHostStores`, `batchNoticeStores` | `app_batch_cutover_hosts.go:23,26` | `appbatch` | `sync.Map`, ключ `*App` → `*Owner` (Ш5) |
| `batchMaintenanceLoops` | `app_batch_maintenance.go:48` | `appbatch` | то же (Ш5) |
| `appFinalizationGates` | `finalization_gate.go:19` | `appkernel` | `sync.Map`, ключ `*App` остаётся (ядро владеет `App`) |
| `batchFaultExit` | `app_batch_fault.go:20` | `appbatch` | тестовый шов (`os.Exit`), неэкспортируемый, едет с кластером |
| `batchRuntimeConfigHook`, `batchInstallDepsHook`, `batchInstallWiringHook` | `app_batch_install.go:29-37` | `appbatch` | тестовые швы, неэкспортируемые |
| `recoverSessionListSeam`, `recoverSessionPreWriteSeam` | `app_recovery.go:24,44` | `apprecovery` | тестовые швы |
| `messageEventsClosedSeam`, `executeRunBeforeTurnLaunchSeam`, `executeRunDoneCaseSeam` | `app_run.go:40-49` | `apprun` | тестовые швы |
| `cliLoopTurnDoneSeam`, `cliLoopBeforeDrainSeam`, `cliLoopStderr`, `cliOpenScopeWaitNoticeInterval`, `cliDBErrorRetryOverallLimit`, `cliQuotaActiveWorkSeam` | `app_run_async.go:25,50,56,270,790,795`, `app_run_quota.go:200` | `cliloop` | тестовые швы и константы |
| `drainPendingAfterAttemptSeam` | `app_run_drain_pending.go:15` | `apprun` | тестовый шов |
| `cliLoopInterruptWaitSeam` | `app_run_wait_interrupt.go:12` | `cliloop` | тестовый шов |
| `modelsSyncBeforeJoinSeam` | `app_config_sync.go:26` | `appkernel` | тестовый шов |
| `ConfigNotificationsPublished` | `app_config_sync.go:22` | `appkernel` | **существующий** экспортируемый счётчик (`atomic.Int64`); создан не разрезкой — остаётся как есть (внешних потребителей нет — проверено grep по `cmd`/`server`/`sdk`); запись в список P3: сделать приватным отдельной задачей |
| `ErrRunQueued` | `app_run_errors.go:136` | `apprun` | сентинелл-ошибка (неизменяемая по факту) |
| `ErrReviewerUnresponsive` | `app_run_reviewer_stall.go:20` | `review` | сентинелл-ошибка |
| `ErrInvalidStripJSON` | `strip_json.go:21` | `review` | сентинелл-ошибка |
| `subAgentToolNames` | `app_run_gates.go:26` | `apprun` | константный срез |
| `reviewCommandFilters`, `reviewSyncExitRe`, `reviewAsyncExitRe`, `reviewAsyncStatusRe`, `reviewAsyncTailRe`, `reviewNoticeIDRes`, `reviewEvidenceTerminalRunes`, `reviewVerdictPattern` | `app_run_reviewer_evidence.go:445-852`, `app_run_reviewer_outcome.go:49` | `review` | неизменяемые regexp/срезы |
| `var _ batchRunner`, `var _ io.Writer` | `app_batch_runtime.go:34`, `app_batch_fault_output.go:81` | `appbatch` | compile-time проверки |

Правило §3 п. 3 соблюдено: ни один глобал не экспортируется ради разрезки; все тестовые швы остаются неэкспортированными в своих пакетах и остаются доступными белым тестам этих пакетов. Сквозных экспортируемых переменных между подпакетами нет.

### 4.2. `init()`, embeds, ОС-файлы

- `init()` — **ноль** в пакете (производственный и тестовый код; `prereq.py` согласен). Порядок инициализации при разрезке не меняется; регистрация через `init()` отсутствует — это упрощает перенос по сравнению с `cmd`/`agent`.
- `go:embed` — **ноль**; ресурсы не переезжают.
- ОС-пары: одна тестовая — `app_batch_cutover_hosts_identity_windows_test.go` + `app_batch_cutover_hosts_identity_unix_test.go` → `appbatch/` вместе. Производственных пар нет.

### 4.3. Белые тесты и помощники

- 226/226 тестов — `package app`: правило «тест остаётся в пакете, чьи идентификаторы он трогает» — основная работа полос D2 (таблица §2.6). Тест, трогающий неэкспортированные имена двух подпакетов (например, batch-фикстуры + тела run), либо перепривязывается к экспортированным швам (Ш2–Ш5 дают для этого экспортируемые входы), либо уходит в `itest` как сценарий. Список таких тестов фиксируется в полосе D2 по факту компиляции, а не по номеру задачи.
- Помощники через границы (101 id, 11 направлений): конструктор `App` в тестах, batch-фикстуры (`app_run_wait_batch_fixture_test.go` и др.), harness финализации (`app_finalization_e2e_harness_test.go`) → `appkernel/apptest` (экспортируемые `*ForTest`-функции, шаблон `db/dbtest`). Помощники одного кластера (`async_phase4_*`, `batch_timeout_wake_test_helpers`, `app_run_e3_helpers`) едут со своим кластером без экспорта. Помощник объявляется в **нижней** из двух целей; вверх никто не импортирует.
- `itest` остаётся единственным пакетом, импортирующим всё (волна 1 уже перенесла туда 9 файлов + `TestMain` с `testguard.Isolate`, `RUSH_PROVIDER_CACHE_TTL=0`, подменой `cliprovider.AvailableFunc`, `heartbeat.QuiesceForTest` — `itest/main_test.go:19,30`); разрезка не добавляет ему работы, кроме приёма новых сценарных тестов из п. выше.
- Переносимый тест переносит окружение своего `TestMain` (§3 п. 12): у `internal/app` отдельного `TestMain` в производственных файлах нет; тестовая изоляция, живущая в `itest/main_test.go`, не дублируется.

## 5. План исполнения (полосы)

Не более 5 агентов одновременно (§3 п. 7). Полосы одной пачки файлов упорядочены; независимые — параллельны. Каждая полоса: `gofumpt` → `go build` → `go vet` затронутых пакетов через `.rush/tmp/golock.sh`; серийные тесты — оркестратор (`golock.sh go test -p 1 -parallel 1 -count=1 -timeout 300s ./internal/app/...`). Полные прогоны — только оркестратор под `capm 2g`.

| № | Полоса | Файловая область | Ждёт | Приёмка | Параллельно с |
|---|---|---|---|---|---|
| 0б-a | **Швы внутри `package app`** (без переносов): `RunEnv` + конвертация методов run/batch/life в свободные функции с тонкими методами-обёртками `*App`; `LoopHost`-инверсия для `cliLoop`; `Owner`-хэндл для трёх реестров; `PersistSessionIntent`/`ValidateResolvedSession` как свободные функции; инвентаризация `LoopHost` и границы `appkernel → appbatch` (письменный список идентификаторов в итоговом отчёте полосы) | все 61 файл (только правка тел/сигнатур, `git mv` нет) | — | `go build ./...`, `go vet ./internal/app`; оракул-модификация: мультимножество имён объявлений сохранено (метод → функция: имя сохранено, приёмник стал первым параметром — счётчик `func (…)` до/после); серийные тесты `./internal/app` зелёные | никем (затрагивает всё) |
| D2-1 | **`appbatch`**: `git mv` 13 файлов batch → `internal/app/appbatch/`, склейки по §2.2, ОС-пара тестов вместе, batch-помощники на месте; `apprun`/`cliloop` ещё в старом пакете — вызовы через `appbatch.X` | 13 кода + ~44 теста | 0б-a | оракул чистого переноса на каждый файл (сортированные `^(func|type|var|const)` и число `Test`-функций без изменений); `grep -c "func Test"` per-file; серийные тесты `./internal/app/appbatch/...` | D2-2, D2-3 (файлы не пересекаются) |
| D2-2 | **`apprecovery`**: `git mv` `app_recovery.go` + 4 теста → `internal/app/apprecovery/`; швы `recoverSession*Seam` едут | 1 + 4 | 0б-a | оракул; серийные тесты `./internal/app/apprecovery/...` | D2-1, D2-3 |
| D2-3 | **`apprun` + `cliloop` + `review`**: `git mv` 32 файлов run-области (29 `app_run_*` + `strip_json.go` + `run_on_finish.go` + `sub_agents.go`) → `apprun/` (4 файла), `cliloop/` (5), `review/` (7) по §2.2; резка трёх файлов В7 (993/982/908 → ≤ 800) при склейках; `LoopHost`-реализация из 0б-a переезжает с телом; `RunEnv` → `apprun`; тесты по §2.6 | 32 кода + ~142 теста | 0б-a, D2-1 (импорт `appbatch`) | оракул на каждый файл + счётчик тестовых функций на пакет (раскладка §2.6); серийные тесты `./internal/app/apprun/...` | D2-1, D2-2 |
| D2-4 | **`appkernel` + фасад**: `git mv` ядра (17 файлов-источников → 6 по §2.2); создание `internal/app/app.go`-фасада; пути в `.github/ci-policy.toml`/`shards.toml` (если перечисляют `internal/app`), `.githooks/check_db_release_pairing.sh` (15 путей `internal/app/...` — И§11.3), запись в `.githooks/dir_entries_allowlist.txt` корректируется (каталог выходит из списка) | 17 кода + ~38 тестов + конфиги | D2-1, D2-2, D2-3 | оракул; `git grep -l '"github.com/PHPCraftdream/rush/internal/app"'` = те же 146 файлов, синтаксис вызовов не изменился (`git grep -c "app\." internal/cmd` — дельта 0); серийные тесты `./internal/app/...`, `./internal/cmd`, `./sdk` точечно | — (последняя из D2-разрезки) |
| D2-5 | **`apptest` + помощники**: создание `appkernel/apptest/`, перенос 16 без-Test файлов по правилу §4.3; тесты, чьи фикстуры уехали, переключаются на `apptest.X` | 16 helper-файлов + правки импортов тестов | D2-4 | сборка всех тестовых пакетов поддерева; `go vet`; серийные тесты точечно; `grep -rn "package app$"` внутри `internal/app` — только `itest` не остаётся | — |
| D2-6 | **`testdata` и хвост**: проверка `internal/app/testdata` (некодовый, не считается), `itest` не меняется; обновление сидов храповиков A1b (тестовые файлы per-dir); запись в `docs/package-moves.md` | конфиги, доки | D2-5 | линтеры `check_dir_entries.sh` + `check_file_size.sh` зелёные; итоговые счётчики §2.2 подтверждены `ls` | — |

Параллелизм: волна A = 0б-a (1 агент). Волна B = D2-1 ∥ D2-2 ∥ D2-3 (3 агента, области файлов не пересекаются; общий риск — одновременные тяжёлые сборки, поэтому каждый агент строит только свои пакеты). Волна C = D2-4 → D2-5 → D2-6 (последовательно, 1 агент). Итого 4 волны, ≤ 3 агентов одновременно.

Примечание о памяти машины (CLAUDE.md): полосы не запускают `go test ./...`; точечные прогоны — через `golock.sh`; два параллельных агента не гоняют сборки одного большого пакета одновременно.

## 6. Риски и открытые вопросы оператору

1. **Размер `LoopHost` (Ш3) не измерен** — оценка 4–8 методов по вызовам из `app_run_async*`; проверяется чтением в 0б-a без сборки. Если > 15 — запасной вариант (цикл уезжает в `cliloop`) должен быть выбран **до** D2-3, иначе перенос перестаёт быть чистым. Это главный нерешённый пункт дизайна.
2. **Границы `appkernel → appbatch` ≈ 14–15 и `apprun/cliloop → review` ≈ 8-10 идентификаторов** — первая на пределе ориентира. Сжатие в один порт-тип `Runtime` предусмотрено (Ш5); выбор — за 0б-a.
3. **sdk**: `sdk/sdk.go` (999 строк) — тонкая надстройка над `app` (34 обращения `app.*`: `app.New`, `app.App`, `app.SkipMCP`, `app.WithMCPOwner`, доступ к полям); фасад сохраняет путь и все имена, поэтому **для sdk ничего не меняется** механически. Резка самого `sdk/sdk.go` — полоса E волны 5 (#94), сюда не входит. Проверить в D2-4, что sdk не начинает импортировать подпакеты напрямую (запретить в ревью).
4. **`internal/agent/supervision_notice_binding_app_test.go`** импортирует `app` (волна 0а-2 перенесла 7 из 9; этот файл остался — 0а-2b #95 ждёт швов). После D2-4 он компилируется без изменений (фасад); его судьба — 0а-2b, не эта разрезка.
5. **Тесты — главная масса работы** (~142 файла в D2-3): часть тестов run использует batch-фикстуры; правило переключения — экспортированные входы Ш2–Ш5 или `itest`. Возможен откат отдельных тестов в `itest`, если перепривязка меняет наблюдаемое поведение. Оценка объёма — самая неточная в документе.
6. **Медленные наборы** (878 с; async-ожидания в cliloop, provider-crash/cutover в batch): серийные точечные прогоны полос D2-1/D2-3 будут долгими (десятки минут); ускорение — вне спринта (И§7), полосы обязаны не запускать пакеты целиком без нужды.
7. **`ConfigNotificationsPublished`** — существующий экспортируемый изменяемый счётчик (`atomic.Int64`, `app_config_sync.go:22`); разрезка его не экспортирует повторно и не мигрирует (остаётся в ядре). P3-запись: приватизировать вне спринта.
8. **Этап C ещё не исполнен** в текущем worktree: таблица §2.2 предполагает `internal/app/projects` после C. Если C задержится, D2-полосы не зависят от него (`internal/projects` импортируется только потребителями, не `app`), но запись храповика для `internal/app` поднимается только при обоих `itest` и `projects`.
9. **Счётчики `prereq.py` содержат шум** (§1.3: `New`, `transition`, `cliLoopStderr`, `apiServiceOwner`, `ErrInvalidStripJSON` — комментарии и `errors.New`); дизайн опирается на проверенные по коду списки, а не на сырые числа скрипта. Кластерные числа §1.2 пересчитаны по `wc -l` (29/8 150 против «28/7 888» в редакции 1 — префиксный пересчёт совпал с И§3 «8150»).
10. **`protocol.go` = 829 строк** — выше цели 800 (мягкой), но в пределах жёсткого 1000 и предупредительного 900; дальнейшая резка невозможна без девятого элемента в `review` (уже 7). Если оператор хочет ≤ 800 — единственный путь: вынести `RunRequest`/`RunOverrides` в `cliloop` (их строит цикл) и оставить в `results.go` только `RunResult`-семейство (544); решение не блокирует D2.
