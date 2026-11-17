# D1: дизайн разрезки `internal/serve/cmd` (бывший `internal/cmd`) — задача #85 / #74

Статус: черновик D1, ждёт ворота Г1. Пакет после этапа C живёт в `internal/serve/cmd` (принцип «последняя часть пути не меняется», §2 плана), рядом с переезжающими по В1 движками `internal/queue` → `serve/cmd/queue` и `internal/ui/notification` → `serve/cmd/notification` (проверено по `mapping.c.json`). Дизайн описывает разрезку уже на новых путях; все пути ниже — от `internal/serve/cmd`.

Источники данных: план §3/§5 (0б-4 #85, D2 #75), исследование И§2, И§3, И§5, И§9.7, И§11.2 (строка `cmd`), выводы скриптов `prereq.py`, `entangle.py 12 300`, `analyze.py clusters|imports internal/cmd` (прогоны 2026-11-15, цитируются в §1). Все факты из кода проверены чтением и снабжены ссылкой `файл:символ`.

## 1. Текущее состояние

### 1.1. Объём

`internal/cmd` — один пакет `cmd`: **166 не-тестовых `.go`-файлов (все рукописные, кроме `models_cah_gen.go` — сгенерированный по маркеру `Code generated … DO NOT EDIT`, строка 1 файла), 36 092 строки в них, 225 тестовых файлов, 1254 тестовые функции**. Счёт проверен перечислением каталога (166 не-тестовых `.go`) и подсчётом `func Test` по всем тестовым файлам (1254). Каталог `testdata/` — 3 файла (golden `api_command_tree.golden`, `claude_crush_fallback_command.original.md`, `claude_slash_command.original.md`). Ресурсы рядом с кодом: каталог `gitignore/` (`old`, `default` — go:embed), каталог `stats/` (`index.html`, `index.css`, `index.js`, `AGENTS.md`), четыре установочных шаблона в корне пакета (`claude_wrush_command.md`, `claude_wcrush_command.md`, `claude_slash_command.yaml`, `claude_crush_fallback_command.yaml`). Сгенерированный `models_cah_gen.go` в лимит элементов каталогов не входит (маркер), в таблице §2 помечен как сгенерированный.

Единственный импортёр пакета — корневой `main.go`: `cmd.InstallProcessStop()` и `cmd.Execute()` (проверено `analyze.py imports internal/cmd`: `internal/cmd <- ['.']`, и чтением `main.go:23` (`InstallProcessStop`) и `main.go:49` (`Execute`)). Публичная поверхность всего пакета из 166 файлов — **два идентификатора** (`process_stop.go:33 InstallProcessStop`, `root.go:353 Execute`).

Кластеры по префиксам файлов (`analyze.py clusters internal/cmd 60`, всего
58 кластеров; ниже — все кластеры с prod ≥ 300 строк):

| кластер | файлов | prod строк | test строк | c90 |
|---|---|---|---|---|
| sessions | 122 | 12229 | 16694 | 108 |
| models | 31 | 3078 | 3484 | 30 |
| run | 33 | 1924 | 3031 | 55 |
| providers | 19 | 2729 | 2098 | 17 |
| migrate | 8 | 1875 | 2471 | 18 |
| api | 23 | 1595 | 1739 | 4 |
| serve | 13 | 521 | 1851 | 6 |
| logs | 8 | 899 | 885 | 10 |
| worktree | 6 | 983 | 675 | 6 |
| queue | 6 | 582 | 1064 | 9 |
| root | 3 | 971 | 589 | 36 |
| ps | 5 | 895 | 641 | 7 |
| batch | 8 | 436 | 1090 | 14 |
| claude | 4 | 577 | 794 | 10 |
| ping | 5 | 790 | 560 | 16 |
| tokens | 6 | 286 | 938 | 4 |
| mcp | 2 | 675 | 484 | 10 |
| command | 5 | 355 | 795 | 5 |
| codex | 3 | 310 | 598 | 17 |
| multi | 2 | 455 | 361 | 15 |
| login | 8 | 563 | 208 | 12 |
| lock | 4 | 82 | 684 | 7 |
| cli | 2 | 307 | 252 | 3 |
| process | 7 | 119 | 392 | 1 |
| прочие 34 кластера | 43 | ~2400 | ~3100 | — |

(Сумма файлов по всем 58 кластерам = 391 = 166 не-тестовых + 225 тестовых.)

Пакет импортирует 33 внутренних пакета — от `internal/agent` и `internal/app` до `web` (`analyze.py imports internal/cmd`), то есть стоит на вершине графа слоёв (группа `serve`): любой тест пакета тянет почти весь репозиторий. Мотив разрезки — И§3 (`cmd` sessions 12 229 строк кода / 16 694 тестовых в 122 файлах тянут `internal/app` в 43 файлах) и И§5 (узкий «открыватель хранилища» вместо `App`).

### 1.2. Циклы между будущими подпакетами

Грубая кластеризация `prereq.py` (`['admin','api','core','models','run','sessions']`)
даёт **полный цикл из шести кластеров**. Веса рёбер (идентификаторов через границу),
все рёбра с весом ≥ 6:

| ребро | ид. | примеры идентификаторов |
|---|---|---|
| core → models | 24 | atom, codexSkillsDir, geminiCommandsDir |
| models → run | 21 | cancelSignals, loadSkillSource, parseSlashCommandSource |
| run → admin | 14 | claudeSlashCommandSentinel, gitIn, isGitIgnored, listGitWorktrees |
| run → core | 14 | MaybePrependStdin, ResolveCwd, acquireSpawnLock, commandSignalContext |
| core → admin | 10 | claudeCommandsDir, logsCmd, removeFallbackCommandFromDir |
| models → core | 9 | ResolveCwd, commandSignalContext, exitWithAudit, initCommandConfig |
| api → core | 8 | applyGlobalDir, cmdFlagString, commandSignalContext, exitCodeError |
| admin → core | 7 | Execute, ResolveCwd, initCommandConfig, rootCmd |
| core → run | 7 | cancelSignals, resolveSessionID, psCmd |
| остальные 18 рёбер цикла | 1–6 | — |

Тонкая кластеризация (`entangle.py internal/cmd 12 300`) показывает: тяжёлых рёбер
между предметными кластерами почти нет — единственное ребро ≥ 12 идентификаторов
это `run → worktree: 13` (gitIn, isGitIgnored, listGitWorktrees, planWorktree,
validateWorktreeSlug, worktreeEntry и др.; проверено чтением `run_worktree.go`).
Остальной сцеп — «звезда» вокруг общих помощников: `root` (rootCmd, setupApp,
ResolveCwd…), `command` (command_journal), `process` (сигналы и консоль),
`sessions` (short/truncate), `batch` (парсер batch-спецификации), `envelope`,
`exitcode`. Вывод: разрезка простым переносом кластеров невозможна — сначала нужен
нижний пакет помощников (шов 0б-4, §3.1), после чего межсемейные рёбра сходятся
к 1–5 идентификаторам (сводка в §2.4).

### 1.3. init(), глобальные переменные, тесты

- **init(): 68** в не-тестовых файлах (проверено `grep -n 'func init()'`;
  совпадает с И§11.2). Полный список с классификацией — §4.1.
- **Глобальные переменные пакета**: `prereq.py` насчитал 21 переменную, пересекающую
  его грубые границы (`rootCmd`, `logsCmd`, `statsCmd`, `tokensCmd`, `apiCmd`,
  `serveCmd`, `processStopInstalled`, `skillSourceFiles` и др.). Полная опись всех
  153 объявлений `^var` пакета (включая ~130 `*cobra.Command`) с назначением
  каждой — §4.2.
- **Тесты**: 225 из 225 файлов — `package cmd`, то есть белые (`prereq.py`);
  19 тестовых файлов обращаются к `rootCmd`, 25 вызывают `Execute()`;
  56 идентификаторов тестовых помощников ходят через грубые границы в 23
  направлениях (`prereq.py`). Расселение — §4.4–§4.6.
- **Фляк-история**: `TestRunCustomRoleProviderDispatch` — Cobra переносит контекст
  корня на подкоманду, только пока её собственный контекст `nil`; `runCmd` хранил
  отменённый контекст прошлого подтеста (план §11, задача #111; фикс
  `resetRunCmdContext`, `run_custom_role_dispatch_test.go:556`). Правило для
  разрезки: помощники, гоняющие дерево несколько раз, обязаны сбрасывать контексты
  подкоманд — обобщается в `cmdtest.ResetCommandContexts` (§3.5).

### 1.4. Файлы у предела 951–1000 строк (решение В7 — режутся в этом плане)

| файл | строк | что делает | лечение |
|---|---|---|---|
| `providers_peak_hours.go` | 985 | `rush providers peak-hours` — 10 подкоманд | делится на `providers_peak_hours.go` (родитель + list/show) и `providers_peak_hours_forms.go` (set/add/rm/tz/clear/pause/resume), обе ≤ 800, едут в `admin/serverctl/…/providers/peakhours/` (§2.1) |
| `providers.go` | 973 | `rush providers list/show/set/add/enable/disable/unset/update/grep/fetch-models` | делится на `providers.go` (чтение: list/show/grep/fetch-models/test) и `providers_write.go` (set/add/enable/disable/unset/update) |
| `root.go` | 972 | корень дерева, Execute, setupApp | делится между `cli/root.go`, `cli/execute.go`, `cli/cmdkit/appopen.go`, `cli/cmdkit/appdb.go` (§2.1, §3.2) |
| `migrate.go` | 935 | `rush migrate` | делится на `migrate.go` (команда, обнаружение legacy-каталогов) и `migrate_walk.go` (обход дерева, обновление `.gitignore`) — цель ≤ 800; порог 900 к приёмке станет блокирующим (В7) |

Рядом с пределом, но в допуске: `sessions_kill.go` 887 — делится на `kill.go` +
`kill_process.go` при переезде в `sesslocks` (§2.1); `sessions_watch.go` 790,
`ping.go` 767, `sessions_locks.go` 734, `mcp.go` 675 остаются целыми.

### 1.5. Пары под ОС/платформу и go:embed

Шесть платформенных пар (каждая едет со своим кодом, §4.3):
`console_ctrl_{windows,other}`, `console_detach_{windows,other}`,
`login_link_{windows,other}`, `run_sigpipe_{unix,windows}`,
`spawnlock_{windows,other}`, `sessions_audit_driver_{modernc,ncruces}`
(build-теги последней повторяют `internal/db/connect_modernc.go` — разлучать
нельзя, иначе драйвер `sqlite` регистрируется дважды; комментарий в шапке файла).

Восемь go:embed-директив (`grep -n 'go:embed'`): `claude_wrush_command.md`,
`claude_wcrush_command.md` (`claude_init.go:33,46`), `*.md *.yaml`
(`multi_cli_convert.go:27`), `gitignore/old`, `gitignore/default`
(`root.go:968,972`), `stats/index.html|css|js` (`stats.go:22–28`).
Ресурсы едут вместе с читающим их кодом (§4.3).

## 2. Целевое дерево

### 2.1. Дерево каталогов и счёт элементов

Лимит — 7 элементов на каталог (код-подкаталоги + рукописные не-тестовые файлы;
тесты, сгенерированные по маркеру, testdata и некодовые ресурсы не считаются).
В serve/cmd два слота заранее заняты движками `queue/` и `notification/` (решение
В1), поэтому верхний уровень держит ровно 5 каталогов, без файлов.

```
internal/serve/cmd/            = 7 элементов: cli/ sessions/ run/ modelctl/ admin/ queue/ notification/
├── cli/                       = 4: root.go execute.go cmdkit/ cmdtest/          — сборка дерева (пакет cli)
│   ├── cmdkit/                = 7: flags.go parse.go appopen.go appdb.go journal/ out/ proc/
│   │   ├── out/               = 4: display.go envelope.go exitcode.go cliui.go  (пакет cliout)
│   │   └── proc/              = 7: signals.go run_sigpipe_unix.go run_sigpipe_windows.go
│   │                               console_ctrl_windows.go console_ctrl_other.go
│   │                               console_detach_windows.go console_detach_other.go (пакет cliproc)
│   └── cmdtest/               = 3: isolate.go seeds.go wait.go                  — тест-помощники (пакет cmdtest)
├── sessions/                  = 7: sessions.go locks/ cleanup/ view/ state/ live/ report/   (пакет sessions)
│   ├── locks/                 = 7 (пакет sesslocks)
│   ├── cleanup/               = 4 (пакет sessclean)
│   ├── view/                  = 6: render print show tree subagents + scan/      (пакет sessview)
│   │   └── scan/              = 6 (пакет sessscan)
│   ├── state/                 = 6 (пакет sessstate)
│   ├── live/                  = 6 (пакет sesslive)
│   └── report/                = 3 каталога: audit/ batch/ cost/
│       ├── audit/             = 7 (пакет sessaudit)
│       ├── batch/             = 3 (пакет sessbatch)
│       └── cost/              = 4 (пакет sesscost)
├── run/                       = 7: run.go run_helpers.go run_help.go run_setup.go run_worktree.go finish/ wtree/ (пакет run)
│   ├── finish/                = 7 (пакет runfinish)
│   └── wtree/                 = 4 (пакет wtree)
├── modelctl/                  = 3 каталога: models/ providers/ ping/             (каталог без кода)
│   ├── models/                = 4: models.go atoms/ use/ opts/                   (пакет models)
│   │   ├── atoms/             = 2 элемента: models_atoms.go, models_effort.go; models_cah_gen.go — сгенерированный, не считается (пакет modelatoms)
│   │   ├── use/               = 6 (пакет modeluse)
│   │   └── opts/              = 5 (пакет modelopts)
│   ├── providers/             = 7: providers.go providers_write.go providers_models.go providers_patch.go providers_ping.go providers_refresh_confirmed.go peakhours/ (пакет providers)
│   │   └── peakhours/         = 3 (пакет provpeak)
│   └── ping/                  = 2 (пакет ping)
├── admin/                     = 7 каталогов: serverctl/ migrate/ diag/ login/ settings/ reports/ install/
│   ├── serverctl/             = 3 каталога: serve/ api/ queuecmd/
│   │   ├── serve/             = 2 (пакет serve)
│   │   ├── api/               = 7: api.go api_misc.go api_signal.go api_client.go runs/ sessions/ remote/ (пакет apictl)
│   │   │   ├── runs/          = 3 (пакет apiruns)
│   │   │   ├── sessions/      = 2 (пакет apisessions)
│   │   │   └── remote/        = 3 (пакет remotectl)
│   │   └── queuecmd/          = 5 (пакет queuecmd)
│   ├── migrate/               = 4 (пакет migrate)
│   ├── diag/                  = 7 (пакет diag)
│   ├── login/                 = 4 (пакет login)
│   ├── settings/              = 7 (пакет settingsctl)
│   ├── reports/               = 5 + каталог ресурсов stats/ (не считается)      (пакет reports)
│   └── install/               = 5: multi_cli_convert.go claude_init.go claude_del.go cli_refresh.go targets/ (пакет install)
│       └── targets/           = 6 (пакет targets)
├── queue/                     (движок, этап C, не меняется)
└── notification/              (движок, этап C, не меняется)
```

Смежные ресурсы едут со своим кодом: `gitignore/{old,default}` → `cli/cmdkit/`
(их читает `createDotRushDir`); `stats/{index.html,index.css,index.js,AGENTS.md}`
→ `admin/reports/`; `claude_wrush_command.md`, `claude_wcrush_command.md`,
`claude_slash_command.yaml`, `claude_crush_fallback_command.yaml` → `admin/install/`
(их читают `claude_init.go` и `multi_cli_convert.go` через `//go:embed *.md *.yaml`).
`testdata/` остаётся при тестах золотого дерева (§4.6: файл `api_command_tree.golden`
едет в `api/`, остальные два — с их тестами в `install/`).

Имена файлов при переносе не меняются, кроме помеченного в §2.2 (деление,
слияние, перенос общих помощников). Итог: 166 исходных файлов распределены
(таблица §2.2, ровно 166 строк-источников), целевых файлов 168 (§2.3).

### 2.2. Полная таблица «исходный файл → цель» (все 166 не-тестовых файлов)

Формат: источник → цель/пакет. «+» после цели — новые файлы от деления/слияния
детализированы в §2.3. Пути цели — от `internal/serve/cmd/`.

| # | исходный файл | цель (пакет) |
|---|---|---|
| 1 | api.go | api/api.go (apictl) |
| 2 | api_batch.go | api/runs/api_batch.go (apiruns) |
| 3 | api_client.go | api/api_client.go (apictl) |
| 4 | api_events.go | api/remote/api_events.go (remotectl) |
| 5 | api_inject.go | api/sessions/api_inject.go (apisessions) |
| 6 | api_misc.go | api/api_misc.go (apictl) |
| 7 | api_permissions.go | api/remote/api_permissions.go (remotectl) |
| 8 | api_remote.go | api/remote/api_remote.go (remotectl) |
| 9 | api_run.go | api/runs/api_run.go (apiruns) |
| 10 | api_runs.go | api/runs/api_runs.go (apiruns) |
| 11 | api_sessions.go | api/sessions/api_sessions.go (apisessions) |
| 12 | api_signal.go | api/api_signal.go (apictl) |
| 13 | batch_cli.go | queuecmd/batch_cli.go (queuecmd) |
| 14 | batch_spec_input.go | queuecmd/batch_spec_input.go (queuecmd) |
| 15 | claude_del.go | install/claude_del.go (install) |
| 16 | claude_init.go | install/claude_init.go (install) |
| 17 | cli_refresh.go | install/cli_refresh.go (install) |
| 18 | codex_del.go | install/targets/codex.go (targets) — слияние с codex_init.go |
| 19 | codex_init.go | install/targets/codex.go (targets) |
| 20 | color_scheme.go | cli/execute.go (cli) — слияние |
| 21 | command_journal.go | cli/cmdkit/journal/journal.go (clijournal) |
| 22 | console_ctrl_other.go | cli/cmdkit/proc/console_ctrl_other.go (cliproc) |
| 23 | console_ctrl_windows.go | cli/cmdkit/proc/console_ctrl_windows.go (cliproc) |
| 24 | console_detach_other.go | cli/cmdkit/proc/console_detach_other.go (cliproc) |
| 25 | console_detach_windows.go | cli/cmdkit/proc/console_detach_windows.go (cliproc) |
| 26 | dirs.go | settings/dirs.go (settingsctl) |
| 27 | envelope.go | cli/cmdkit/out/envelope.go (cliout) |
| 28 | exitcode_api.go | cli/cmdkit/out/exitcode.go (cliout) — слияние с exitcode_tokens.go |
| 29 | exitcode_tokens.go | cli/cmdkit/out/exitcode.go (cliout) |
| 30 | gemini_del.go | install/targets/gemini.go (targets) — слияние с gemini_init.go |
| 31 | gemini_init.go | install/targets/gemini.go (targets) |
| 32 | grok_del.go | install/targets/grok.go (targets) — слияние с grok_init.go |
| 33 | grok_init.go | install/targets/grok.go (targets) |
| 34 | lock.go | settings/lock.go (settingsctl) |
| 35 | login.go | login/login.go (login) |
| 36 | login_link.go | login/login_link.go (login) |
| 37 | login_link_other.go | login/login_link_other.go (login) |
| 38 | login_link_windows.go | login/login_link_windows.go (login) |
| 39 | logs.go | diag/logs.go (diag) |
| 40 | logs_path.go | diag/logs_path.go (diag) |
| 41 | logs_prune.go | diag/logs_prune.go (diag) |
| 42 | logs_summary.go | diag/logs_summary.go (diag) |
| 43 | mcp.go | settings/mcp.go (settingsctl) |
| 44 | migrate.go | migrate/migrate.go + migrate/migrate_walk.go (migrate) — деление |
| 45 | migrate_artifacts.go | migrate/migrate_artifacts.go (migrate) |
| 46 | migrate_ktav.go | migrate/migrate_ktav.go (migrate) |
| 47 | models.go | models/models.go (models) |
| 48 | models_atoms.go | models/atoms/models_atoms.go (modelatoms) |
| 49 | models_bump.go | models/use/models_bump.go (modeluse) |
| 50 | models_cache.go | models/use/models_cache.go (modeluse) |
| 51 | models_cah_gen.go (сгенерированный) | models/atoms/models_cah_gen.go (modelatoms) |
| 52 | models_effort.go | models/atoms/models_effort.go (modelatoms) |
| 53 | models_efforts.go | models/opts/models_efforts.go (modelopts) |
| 54 | models_list.go | models/opts/models_list.go (modelopts) |
| 55 | models_preset.go | models/opts/models_preset.go (modelopts) |
| 56 | models_set.go | models/opts/models_set.go (modelopts); splitModelEffort → modelatoms (SplitModelEffort), см. §3.4 |
| 57 | models_state.go | models/use/models_state.go (modeluse) |
| 58 | models_state_restore.go | models/use/models_state_restore.go (modeluse) |
| 59 | models_unset.go | models/opts/models_unset.go (modelopts) |
| 60 | models_use.go | models/use/models_use.go (modeluse) |
| 61 | models_use_verify.go | models/use/models_use_verify.go (modeluse) |
| 62 | multi_cli_convert.go | install/multi_cli_convert.go (install) |
| 63 | omp_del.go | install/targets/omp.go (targets) — слияние с omp_init.go |
| 64 | omp_init.go | install/targets/omp.go (targets) |
| 65 | opencode_del.go | install/targets/opencode.go (targets) — слияние с opencode_init.go |
| 66 | opencode_init.go | install/targets/opencode.go (targets) |
| 67 | ping.go | ping/ping.go (ping); resolveModelRole → modeluse (ResolveModelRole), см. §3.4 |
| 68 | ping_peak_hours.go | ping/ping_peak_hours.go (ping) |
| 69 | process_stop.go | cli/cmdkit/proc/signals.go (cliproc) — слияние с run_signals.go |
| 70 | process_stop_ui.go | cli/cmdkit/out/cliui.go (cliout) |
| 71 | projects.go | reports/projects.go (reports) |
| 72 | providers.go | providers/providers.go + providers/providers_write.go (providers) — деление |
| 73 | providers_models.go | providers/providers_models.go (providers) |
| 74 | providers_patch.go | providers/providers_patch.go (providers); scopeFromFlags → cmdkit/flags.go, dash и maskKey → cmdkit/out/display.go |
| 75 | providers_peak_hours.go | providers/peakhours/providers_peak_hours.go + providers/peakhours/providers_peak_hours_forms.go (provpeak) — деление |
| 76 | providers_peak_hours_write.go | providers/peakhours/providers_peak_hours_write.go (provpeak) |
| 77 | providers_ping.go | providers/providers_ping.go (providers) |
| 78 | providers_refresh_confirmed.go | providers/providers_refresh_confirmed.go (providers) |
| 79 | ps.go | diag/ps.go (diag) |
| 80 | ps_render.go | diag/ps_render.go (diag) |
| 81 | ps_stale.go | diag/ps_stale.go (diag) |
| 82 | queue.go | queuecmd/queue.go (queuecmd) |
| 83 | qwen_del.go | install/targets/qwen.go (targets) — слияние с qwen_init.go |
| 84 | qwen_init.go | install/targets/qwen.go (targets) |
| 85 | result.go | reports/result.go (reports) |
| 86 | roles.go | reports/roles.go (reports) |
| 87 | root.go | cli/root.go + cli/execute.go + cli/cmdkit/appopen.go + cli/cmdkit/appdb.go (cli, cmdkit) — деление на 4, состав в §2.3 |
| 88 | run.go | run/run.go (run) |
| 89 | run_deadline.go | run/finish/run_deadline.go (runfinish) |
| 90 | run_fail_review.go | run/finish/run_fail_review.go (runfinish) |
| 91 | run_finalization.go | run/finish/run_finalization.go (runfinish) |
| 92 | run_finalization_gate.go | run/finish/run_finalization_gate.go (runfinish) |
| 93 | run_findings.go | run/finish/run_findings.go (runfinish) |
| 94 | run_format.go | run/finish/run_format.go (runfinish) |
| 95 | run_helpers.go | run/run_helpers.go (run); resolveSessionID → cmdkit/appopen.go (ResolveSessionID), см. §3.4 |
| 96 | run_help.go | run/run_help.go (run) |
| 97 | run_role_model.go | run/finish/run_role_model.go (runfinish) |
| 98 | run_setup.go | run/run_setup.go (run) |
| 99 | run_signals.go | cli/cmdkit/proc/signals.go (cliproc) |
| 100 | run_sigpipe_unix.go | cli/cmdkit/proc/run_sigpipe_unix.go (cliproc) |
| 101 | run_sigpipe_windows.go | cli/cmdkit/proc/run_sigpipe_windows.go (cliproc) |
| 102 | run_worktree.go | run/run_worktree.go (run) |
| 103 | schema.go | settings/schema.go (settingsctl) |
| 104 | serve.go | serve/serve.go (serve) |
| 105 | serve_pins.go | serve/serve_pins.go (serve) |
| 106 | sessions.go | sessions/sessions.go (sessions); short и truncate → cmdkit/out/display.go |
| 107 | sessions_activity.go | sessions/state/sessions_activity.go (sessstate) |
| 108 | sessions_audit.go | sessions/report/audit/sessions_audit.go (sessaudit) |
| 109 | sessions_audit_db.go | sessions/report/audit/sessions_audit_db.go (sessaudit) |
| 110 | sessions_audit_driver_modernc.go | sessions/report/audit/sessions_audit_driver_modernc.go (sessaudit) |
| 111 | sessions_audit_driver_ncruces.go | sessions/report/audit/sessions_audit_driver_ncruces.go (sessaudit) |
| 112 | sessions_audit_findings.go | sessions/report/audit/sessions_audit_findings.go (sessaudit) |
| 113 | sessions_audit_report.go | sessions/report/audit/sessions_audit_report.go (sessaudit) |
| 114 | sessions_audit_rules.go | sessions/report/audit/sessions_audit_rules.go (sessaudit) |
| 115 | sessions_batch_act.go | sessions/report/batch/sessions_batch_act.go (sessbatch) |
| 116 | sessions_batch_audit.go | sessions/report/batch/sessions_batch_audit.go (sessbatch) |
| 117 | sessions_batch_read.go | sessions/report/batch/sessions_batch_read.go (sessbatch) |
| 118 | sessions_budget.go | sessions/report/cost/sessions_budget.go (sesscost) |
| 119 | sessions_cache.go | sessions/state/sessions_cache.go (sessstate) |
| 120 | sessions_cancel.go | sessions/live/sessions_cancel.go (sesslive) |
| 121 | sessions_compact.go | sessions/cleanup/sessions_compact.go (sessclean) |
| 122 | sessions_cost.go | sessions/report/cost/sessions_cost.go (sesscost); parseSinceDuration и parsePlainInt → cmdkit/parse.go |
| 123 | sessions_cost_models.go | sessions/report/cost/sessions_cost_models.go (sesscost) |
| 124 | sessions_cost_rows.go | sessions/report/cost/sessions_cost_rows.go (sesscost) |
| 125 | sessions_delete.go | sessions/cleanup/sessions_delete.go (sessclean) |
| 126 | sessions_diff.go | sessions/view/scan/sessions_diff.go (sessscan) |
| 127 | sessions_fork.go | sessions/live/sessions_fork.go (sesslive) |
| 128 | sessions_gc.go | sessions/cleanup/sessions_gc.go (sessclean) |
| 129 | sessions_grep.go | sessions/view/scan/sessions_grep.go (sessscan) |
| 130 | sessions_inject.go | sessions/live/sessions_inject.go (sesslive) |
| 131 | sessions_jobs.go | sessions/view/scan/sessions_jobs.go (sessscan) |
| 132 | sessions_kill.go | sessions/locks/kill.go + sessions/locks/kill_process.go (sesslocks) — деление |
| 133 | sessions_kill_batch_scope.go | sessions/locks/sessions_kill_batch_scope.go (sesslocks) |
| 134 | sessions_last.go | sessions/view/scan/sessions_last.go (sessscan) |
| 135 | sessions_list.go | sessions/view/scan/sessions_list.go (sessscan) |
| 136 | sessions_live_owner.go | sessions/locks/sessions_live_owner.go (sesslocks) |
| 137 | sessions_locks.go | sessions/locks/sessions_locks.go (sesslocks) |
| 138 | sessions_pick.go | sessions/live/sessions_pick.go (sesslive) |
| 139 | sessions_print.go | sessions/view/sessions_print.go (sessview) |
| 140 | sessions_purge.go | sessions/locks/sessions_purge.go (sesslocks) |
| 141 | sessions_reap.go | sessions/locks/sessions_reap.go (sesslocks) |
| 142 | sessions_render.go | sessions/view/sessions_render.go (sessview) |
| 143 | sessions_reset.go | sessions/live/sessions_reset.go (sesslive) |
| 144 | sessions_show.go | sessions/view/sessions_show.go (sessview) |
| 145 | sessions_stall.go | sessions/state/sessions_stall.go (sessstate) |
| 146 | sessions_status.go | sessions/state/sessions_status.go (sessstate) |
| 147 | sessions_status_label.go | sessions/state/sessions_status_label.go (sessstate) |
| 148 | sessions_stop_purge.go | sessions/cleanup/sessions_stop_purge.go (sessclean) |
| 149 | sessions_subagents.go | sessions/view/sessions_subagents.go (sessview) |
| 150 | sessions_tail.go | sessions/view/scan/sessions_tail.go (sessscan) |
| 151 | sessions_tree.go | sessions/view/sessions_tree.go (sessview) |
| 152 | sessions_watch.go | sessions/live/sessions_watch.go (sesslive) |
| 153 | sessions_why.go | sessions/state/sessions_why.go (sessstate) |
| 154 | spawnlock_other.go | queuecmd/spawnlock_other.go (queuecmd) |
| 155 | spawnlock_windows.go | queuecmd/spawnlock_windows.go (queuecmd) |
| 156 | stats.go | reports/stats.go (reports) |
| 157 | system_prompt.go | reports/system_prompt.go (reports) |
| 158 | tokens.go | settings/tokens.go (settingsctl) |
| 159 | tokens_binding.go | settings/tokens_binding.go (settingsctl) |
| 160 | unknown_subcommand.go | cli/execute.go (cli) — слияние |
| 161 | update_providers.go | settings/update_providers.go (settingsctl) |
| 162 | web.go | cli/root.go (cli) — слияние |
| 163 | worktree_cmd.go | run/wtree/worktree_cmd.go (wtree) |
| 164 | worktree_git.go | run/wtree/worktree_git.go (wtree) |
| 165 | worktree_mirror.go | run/wtree/worktree_mirror.go (wtree) |
| 166 | worktree_plan.go | run/wtree/worktree_plan.go (wtree) |

В таблице ровно 166 строк — по одной на каждый не-тестовый файл исходного
каталога (сверено с листингом каталога).

### 2.3. Новые и изменённые файлы (не 1:1)

Новые служебные файлы, собранные из кусочков чужих файлов (пометка в §2.2):

| новый файл | пакет | из чего собран |
|---|---|---|
| cli/cmdkit/flags.go | cmdkit | scopeFromFlags (providers_patch.go:133), cmdFlagString (root.go:512) |
| cli/cmdkit/parse.go | cmdkit | parseSinceDuration, parsePlainInt (sessions_cost.go) |
| cli/cmdkit/appopen.go | cmdkit | initCommandConfig (root.go:538), ResolveCwd (root.go:929), applyGlobalDir (root.go:474), setupApp (root.go:560), setupAppWithOptions (root.go:564), setupAppLite (root.go:638), mcpAppOptions (root.go:498), setSettingsAudit (root.go:244), checkAppPassword (root.go:216), ResolveSessionID (run_helpers.go:14) |
| cli/cmdkit/appdb.go | cmdkit | connectAppDB (root.go:525), createDotRushDir (root.go:945), embed gitignore/old и gitignore/default (root.go:968–972) |
| cli/cmdkit/out/display.go | cliout | short (sessions.go:72), truncate (sessions.go:83), dash (providers_patch.go:177), maskKey (providers_patch.go:148), age (sessions_kill.go:859) |

Деления (правило В7, цель ≤ 800 строк, состав части — решение полосы с
проверкой оракула):

| новый файл | из | по какому шву |
|---|---|---|
| sessions/locks/kill.go + kill_process.go | sessions_kill.go (887) | kill.go — команды, разбор аргументов, объём блокировки; kill_process.go — работа с процессами (пробы, taskkill/kill по PID) |
| providers/providers.go + providers_write.go | providers.go (973) | providers.go — list/show/grep/fetch-models/test; providers_write.go — set/add/enable/disable/unset/update |
| providers/peakhours/providers_peak_hours.go + providers_peak_hours_forms.go | providers_peak_hours.go (985) | peak_hours.go — родитель + list/show; forms.go — set/add/rm/tz/clear/pause/resume |
| migrate/migrate.go + migrate_walk.go | migrate.go (935) | migrate.go — команда, обнаружение legacy-каталогов; walk.go — обход дерева и обновление файлов |

Слияния (внутри одного будущего пакета, оракул соблюдается):

| целевой файл | слились | итог строк |
|---|---|---|
| cli/root.go | root.go (часть) + web.go | ≤ 500 |
| cli/execute.go | root.go (часть) + color_scheme.go + unknown_subcommand.go | ≤ 400 |
| cli/cmdkit/out/exitcode.go | exitcode_api.go + exitcode_tokens.go | 134 |
| cli/cmdkit/proc/signals.go | process_stop.go + run_signals.go | ≈ 102 |
| install/targets/{codex,gemini,grok,omp,opencode,qwen}.go | пары `*_init.go` + `*_del.go` одного CLI | 76–310 |

Состав cli/root.go и cli/execute.go: root.go — var rootCmd, конструктор дерева
(вызовы конструкторов семейств), persistent-флаги (root.go:46–106),
PersistentPreRunE-цепочка (root.go:50–63), settingsPasswordPreRun (root.go:228),
runWebMode + webReadyBanner (root.go:253–299), webCmd (web.go);
execute.go — Execute (root.go:353), recoverAndLogPanic + crashLogMarker
(root.go:301–352), commandErrorHandler (root.go:~310–351), resolveColorScheme и
colorSchemeFlagFromArgs (color_scheme.go), guardUnknownSubcommands
(unknown_subcommand.go), делегат InstallProcessStop (5 строк).

### 2.4. Проверка имён пакетов и граф зависимостей

Имена 39 новых пакетов проверены на уникальность `git grep -h '^package <имя>$'`
(по всем нуля совпадений, 2026-11-15; сквозная проверка с дизайнами `config` и `server` изменила три имени: `api` -> `apictl` (занято `rest/api` в `server`), `apiremote` -> `remotectl` и `settings` -> `settingsctl` (заняты в `config`); каталоги остаются `api/`, `remote/`, `settings/`, имя пакета в них иное, как в дизайне `mcp`): cli, cmdkit, cliout, cliproc, clijournal, cmdtest,
sessions, sesslocks, sessclean, sessview, sessscan, sessstate, sesslive,
sessaudit, sessbatch, sesscost, run, runfinish, wtree, models, modelatoms,
modeluse, modelopts, providers, provpeak, ping, serve, apictl, apiruns, apisessions,
remotectl, queuecmd, migrate, diag, login, settingsctl, reports, install, targets
(39 имён с учётом clijournal и трёх подпакетов api). Осмысленность без пути: команды названы
по команде (`sessions`, `run`, `providers`…), служебные — префиксом области
(`sess*` — сессии, `model*` — модели, `api*` — API-клиент, `cli*` — общая
обвязка CLI). Запрещённых имён (core/util/common/base) нет. `run` — имя пакета
свободно (проверено; не путать с `internal/session` группой `run`).

Граф зависимостей (стрелки = импортирует; внешние импорты не показаны):

```
cli ─→ sessions, run, wtree, models, providers, ping, serve, api, queuecmd,
      migrate, diag, login, settings, reports, install  (по конструктору на семейство)
cli ─→ cmdkit;  cli (тесты) ─→ cmdtest
sessions ─→ cmdkit;  sessions ─→ sesslocks, sessclean, sessview, sessstate, sesslive, sessreport*
sesslocks/sessclean/sessview/sessscan/sessstate/sesslive/sessaudit/sessbatch/sesscost ─→ cmdkit
run ─→ wtree (13 ид., §3.4), run ─→ models (1 ид.), run ─→ cmdkit
runfinish ─→ cmdkit;  wtree ─→ cmdkit
models ─→ modelatoms, modeluse, modelopts;  modeluse ─→ modelatoms;  modelopts ─→ modelatoms
providers ─→ cmdkit;  provpeak ─→ cmdkit;  ping ─→ models (1 ид.), ping ─→ cmdkit
serve ─→ cmdkit;  api ─→ queuecmd (2 ид.);  queuecmd ─→ cmdkit
migrate/diag/login/settings/reports/install ─→ cmdkit
sesslive ─→ queuecmd (1 ид. RunQueueTask)
cmdtest ─→ config, testguard, db, session, app, message (внешние, слой serve > app)
```
*sessreport — каталог `sessions/report/` без кода (только подкаталоги), пакета нет.

Циклов нет: единственный общий лист — cmdkit; латеральные рёбра (run→wtree,
run→models, ping→models, api→queuecmd, sesslive→queuecmd) ацикличны, у каждого
приёмник не импортирует источник (проверено по описи импортов в §1.1 и спискам
использования в §3.4).

Примечание для M1: после пересмотра владельцев в §5 этот граф следует читать как
целевую схему, а не как компиляторное подтверждение cycle-free для всех M1-рёбер.
Остальные M1-предложения требуют проверки полных closures и импортов при реализации.

### 2.5. Публичная поверхность

- Для `main.go`: пакет `cli` экспортирует те же два имени — `Execute` и
  `InstallProcessStop` (имена не меняются, меняется путь импорта; правка
  `main.go:23` (`InstallProcessStop`) и `main.go:49` (`Execute`) — единственная вне `internal/serve/cmd`).
- Для тестов и сборки дерева: пакет `cli` экспортирует `NewRootCmd() *cobra.Command`
  (новое имя, §3.2) — единственная точка сборки; тесты золотого дерева и
  контрактные тесты вызывают её.
- Каждое семейство экспортирует конструкторы верхних команд (список — §4.1,
  колонка «конструктор»): например `sessions.NewSessionsCmd()`,
  `run.NewRunCmd()`, `models.NewModelsCmd()`, `providers.NewProvidersCmd()`,
  `ping.NewPingCmd()`, `serve.NewServeCmd()`, `api.NewAPICmd()`,
  `queuecmd.NewQueueCmd()`, `queuecmd.NewBatchCmd()`, `migrate.NewMigrateCmd()`,
  `diag.NewLogsCmd()`, `diag.NewPsCmd()`, `login.NewLoginCmd()`,
  `settings.NewLockCmd()`, `settings.NewUnlockCmd()`, `settings.NewTokensCmd()`,
  `settings.NewSchemaCmd()`, `settings.NewDirsCmd()`,
  `settings.NewUpdateProvidersCmd()`, `settings.NewMcpCmd()`,
  `reports.NewStatsCmd()`, `reports.NewRolesCmd()`, `reports.NewResultCmd()`,
  `reports.NewProjectsCmd()`, `reports.NewSystemPromptCmd()`,
  `install.NewClaudeInitCmd()`, `install.NewClaudeDelCmd()`,
  `install.NewCliRefreshCmd()`, `wtree.NewWorktreeCmd()`. Конструкторы sub-команд
  (например `runfinish.NewRunDeadlineCmd()`) нужны только родителю своего дерева
  и остаются неэкспортированными, если родитель в том же пакете; экспортируются
  только те, что пересекают границу пакета (родитель в другом пакете).
- Поверхность cmdkit — §3.1; сумма имён через каждую границу ≤ 15 (§3).

## 3. Швы

Правило (план §3.2, В6): сначала шов — полоса без переноса, потом перенос.
Оракул чистого переноса (§3.1 плана) действует для каждого перемещаемого файла;
для швов разрешённое изменение строки объявления — ровно одно: смена регистра
первой буквы имени из списка экспорта данной полосы (имя не меняется, меняется
только видимость; `git diff` перенесённого файла показывает только это и блок
импортов). Тела функций байт-в-байт.

### 3.1. Шов «нижний пакет помощников» — `cli/cmdkit` + `cli/cmdkit/journal`

Что уезжает: все общие помощники из §2.3 (flags.go, parse.go, appopen.go,
appdb.go, out/, proc/, journal/). Кто импортирует: все 39 пакетов дерева.

Экспорт через границу (≤ 15 на пакет):

| пакет | экспортируемые имена (счёт) | откуда |
|---|---|---|
| cmdkit | SetupApp, SetupAppLite, SetupAppWithOptions, InitCommandConfig, ResolveCwd, ApplyGlobalDir, McpAppOptions, ScopeFromFlags, CmdFlagString, SetSettingsAudit, CheckAppPassword, ResolveSessionID, ParseSinceDuration, ParsePlainInt (14) | root.go:216–675, 929–943; providers_patch.go:133; sessions_cost.go; run_helpers.go:14 |
| clijournal | JournalCommandStart, JournalExecutionStarted, FinalizeCommandJournalWithCode, ExitWithAudit (4) | command_journal.go:86–194, 355 |
| cliout | Short, Truncate, Dash, MaskKey, Age, LoadEnvelope, ResolveEnvelopePath, ExitCodeError, APIExitCode, TokenCLIError, TokensExitCode, NewCLIProgram (12) | sessions.go:72,83; providers_patch.go:148,177; sessions_kill.go:859; envelope.go:26,114; exitcode_api.go:8,16; exitcode_tokens.go:9,~95; process_stop_ui.go:18 |
| cliproc | InstallProcessStop, ProcessStopInstalled, CommandSignals, CommandSignalContext, CancelSignals, KeepBrokenPipeAsError (6) | process_stop.go:17,33,89; run_signals.go:8; run_sigpipe_unix.go |

Состояние внутри шва: `settingsAuditOnce` (root.go:242, используется только
command_journal.go:86 и сбрасывается в тесте command_journal_fallback_test.go:82)
— уезжает в clijournal/journal.go вместе с единственным потребителем; `exitFunc`
(command_journal.go:22) и `commandJournal` (command_journal.go:77) — тест-швы,
остаются неэкспортированными внутри clijournal (их белые тесты едут в clijournal,
§4.4). `processStopInstalled` (process_stop.go:17) — неэкспортированная атомарная
переменная в cliproc; serve.go читает её через экспорт `ProcessStopInstalled`.

Сохранение поведения: все помощники вызываются в те же моменты (RunE/PreRunE
исполняются после полной сборки дерева — как сегодня после init()); порядок
вычислений не меняется, ленивого остаётся ленивым (`settingsAuditOnce.Do`
срабатывает при первой журнальной записи, не при импорте). Никаких новых
экспортируемых изменяемых переменных шов не создаёт.

### 3.2. Шов «регистрация без init() и без глобального rootCmd» (0б-4)

- Пакет cli получает `NewRootCmd() *cobra.Command`: строит свежий корень,
  регистрирует persistent-флаги (root.go:47–105), ставит PersistentPreRunE-цепочку
  ровно в нынешнем порядке `applyGlobalDir → journalCommandStart →
  settingsPasswordPreRun → apiTokenArgumentGuard` (root.go:50–63), добавляет
  семейства вызовами их конструкторов, ставит `webCmd` и делегата
  `InstallProcessStop`. `Execute()` (cli/execute.go) вызывает `NewRootCmd()` —
  глобального `rootCmd` больше нет; пакетная переменная не создаётся вовсе.
- Каждое семейство экспортирует конструктор верхнего уровня (список §2.5),
  который строит свои команды, вешает их флаги (содержимое сегодняшних init
  по классу «флаги/дети», §4.1) и возвращает `*cobra.Command`.
  Пакетные переменные `*cobra.Command` внутри семейства остаются
  (неэкспортированные, мутируют только внутри конструктора — правило В6
  не нарушено).
- Тела 30 init-ов, звавших `rootCmd.AddCommand`, заменяются вызовом конструктора
  в cli; тела 38 остальных init-ов переезжают в конструктор своего пакета
  (§4.1). После 0б-4 `grep -n 'func init('` по не-тестовым файлам дерева
  даёт пустой результат.
- Сохранение поведения: Cobra по умолчанию сортирует `Commands()`
  (`EnableCommandSorting`), порядок добавления на вывод `--help` и на
  `CommandPath()` не влияет; момент постройки команд смещается с init-фазы
  пакета на вызов `NewRootCmd()` — по-прежнему до `fang.Execute` и до любого
  RunE, однопоточно, без наблюдаемой разницы. Поведение цепочки PreRunE
  (включая journalling до проверки пароля, root.go:48–49) не меняется.
- Приёмка шва: новый тест `TestRootCommandTreeGolden` (по образцу
  `api_command_tree_golden_test.go`: обход дерева, сортировка строк, golden-файл
  `testdata/root_command_tree.golden`) пишется ДО переносов (полоса K2) и
  фиксирует Use-строки и флаги каждой команды; контракт
  `TestRunnableLeafCommandsDeclareArgs` (command_args_test.go) переезжает в cli
  и ходит по `NewRootCmd()`.

### 3.3. Шов «контекст команд» (фляк #111)

- `cmdtest.ResetCommandContexts(root *cobra.Command)` — обобщение
  `resetRunCmdContext` (run_custom_role_dispatch_test.go:556): обходит дерево и
  ставит `SetContext(nil)` каждой команде. Обязательна для любого помощника,
  гоняющего дерево (`NewRootCmd()`+`Execute`-подобный прогон) больше одного раза
  в процессе теста.
- Дополнительно `NewRootCmd()` при каждом вызове строит свежие объекты команд —
  класс фляка «унаследованный контекст подкоманды» исчезает для новых тестов;
  старые помощники, переезжающие в семейства, переводятся на конструктор своего
  семейства (свежий объект на вызов).

### 3.4. Межсемейные швы (после cmdkit остаются только эти)

| шов | имена через границу (счёт) | кто кого | места вызова, которые меняются |
|---|---|---|---|
| run→wtree | GitIn, IsGitIgnored, ListGitWorktrees, PlanWorktree, ValidateWorktreeSlug, WorktreeEntry + до 8 имён из worktree_mirror/plan (итог 13, ≤ 15; точный список фиксирует полоса компиляцией пакета run) | run импортирует wtree | run_worktree.go:102 (2 вызова gitIn, isGitIgnored, listGitWorktrees, planWorktree, validateWorktreeSlug) |
| run→models | ResolveModelRole (1) | run, ping импортируют modeluse | run/finish/run_role_model.go:23,34; ping.go:706 (определение переезжает из ping.go:725 в modeluse) |
| api→queuecmd | ReadBatchSpec, ParseBatchSpec (2) | api импортирует queuecmd | api/runs/api_batch.go:~124 (вызов parseBatchSpec) |
| sesslive→queuecmd | RunQueueTask (1) | sesslive импортирует queuecmd | sessions/live/sessions_pick.go:~вызов runQueueTask |
| modelatoms←modeluse/modelopts | SplitModelEffort (1, переезжает из models_set.go:33 в atoms — atoms вызывает его на :432 и :500) | modeluse, modelopts импортируют modelatoms | models/use/models_use.go, models/opts/models_set.go |
| cli→api | APITokenArgumentGuard (1, экспорт apiTokenArgumentGuard, api.go:73) | cli импортирует api | cli/root.go:62 (PersistentPreRunE) |

Сцепление моделей↔провайдеров из `entangle.py` (`models → providers: 1`,
`ping → providers: 3`) растворяется в cmdkit: это вызовы scopeFromFlags/dash/
maskKey (проверено: `models_use.go`, `models_unset.go`, `models_bump.go`,
`ping.go` ссылаются только на них; `defaultBaseURLFor` (providers_ping.go:201)
кроме тестов никем вне providers не вызывается). Латерального ребра
models↔providers в целевом графе нет (§2.4).

Сохранение поведения для всех швов таблицы: границы проходят по чистым функциям
и типам без состояния; момент вызова не меняется (переименование в экспорт не
меняет семантики); доказательство — оракул переноса по каждому файлу-источнику
плюс `go vet` и прогон затронутых пакетов оркестратором.

### 3.5. Тест-шов `cmdtest`

Пакет `cli/cmdtest` (не `_test`, обычный пакет, импортируется только тестами):
`Main(m *testing.M)` (изоляция testguard.Isolate + config.SetKtavAutoMigrateForTest(false)
+ config.SetSettingsPasswordIterationsForTest(1000) — в точности содержимое
сегодняшних TestMain и settings_lock_kdf_testinit_test.go), `KtavGateOn(t)`,
`KtavIsolateGlobalEnv(t)`, `LowerSettingsPasswordIterations()`,
`ResetCommandContexts` (§3.3), `WaitOrDump` (testwait_test.go),
`SeedCmdBatchDelegation` (sessions_batch_fixture_helpers_test.go:28),
`SeedAuditRulesMigratedDB` + `AuditRulesFixture` (sessions_fixture_db_test.go).
Каждый пакет с тестами получает файл main_test.go-шим: `func TestMain(m
*testing.M) { cmdtest.Main(m) }` — поведение изоляции сохраняется для всех
тестов всех пакетов (сегодня оно пакетное для всего cmd).

## 4. Глобалы, init(), embeds, ОС-файлы, белые тесты, помощники

### 4.1. Все 68 init() и их судьба

Классы: «rootCmd» — init только добавляет команду в rootCmd: тело исчезает, вызов
конструктора появляется в cli; «mix» — init вешает детей своего семейства и
добавляет в rootCmd: часть детей в конструктор семейства, вызов в cli; «дети» —
init вешает детей внутри семейства: конструктор семейства; «флаги» — init
регистрирует только флаги своих команд: конструктор семейства. Классификация:
вызовы AddCommand — по grep всех тел init (полный список); тела «флаги» и «mix»
прочитаны напрямую (помечено). Порядок не несёт семантики: ни один init не читает
флаг или команду другого семейства (проверено по списку вызовов и чтению тел);
единственная кросс-ссылка — rootCmd; сортировка команд в Cobra делает порядок
добавления невидимым (§3.2). Регистрацию commands без собственного init
(dirsCmd, schemaCmd, statsCmd, apiCmd = newAPICmd()) забирает конструктор cli.

- rootCmd-only → тело исчезает, вызов конструктора в cli (21): claude_del,
  claude_init, cli_refresh, codex_del, codex_init, gemini_del, gemini_init,
  grok_del, grok_init, migrate (прочитано), models (прочитано), omp_del,
  omp_init, opencode_del, opencode_init, ping (прочитано: pingCmd, pingFastCmd),
  qwen_del, qwen_init, result, roles (прочитано: 235 newRolesCmd() уже
  конструктор), system_prompt.
- mix → пакет семейства + cli (8): batch_cli (прочитано: 59 дети, 60 rootCmd;
  пакет queuecmd), mcp (прочитано: 582 девять детей, 583 rootCmd; settings),
  providers (прочитано: 942 десять детей, 943 rootCmd), queue (прочитано:
  563+ флаги детей, 580 дети, 581 rootCmd; queuecmd), serve (прочитано:
  флаги listen/tls/route/pin-session, 69 rootCmd), sessions (прочитано:
  38–66 флаги десяти детей, 68 двадцать четыре ребёнка, 69 rootCmd), web
  (прочитано: 5–9 флаги, 10 rootCmd; слит с cli/root.go), worktree_cmd
  (прочитано: 78 флаги, 85 дети, 86 rootCmd; wtree).
- дети → конструктор семейства (16): logs (прочитано: logsCmd.AddCommand ×3;
  diag), models_bump, models_cache (models), models_efforts, models_list,
  models_preset, models_set, models_unset (modelopts), models_state,
  models_use (modeluse), providers_patch, providers_ping (providers),
  providers_peak_hours (provpeak; прочитано: 979 восемь детей, 984 в
  providersCmd), sessions_inject, sessions_jobs (sessions), tokens (settingsctl).
- флаги → конструктор семейства (22): lock (прочитано: global/local на
  lockCmd/unlockCmd; settings), login (прочитано: force/device), logs_prune
  (прочитано: audit-days, heartbeat-days, json), logs_summary (прочитано:
  since, file, gap, json), ps (прочитано: watch, json, model, all) — diag;
  projects (прочитано: json; регистрация projectsCmd — root.go:107–121,
  уходит в cli; reports), run (прочитано: SilenceUsage + quiet; run),
  sessions_audit (прочитано: all/since/worktrees; sessaudit),
  sessions_cancel (прочитано: all; sesslive), sessions_compact (прочитано:
  dry-run/force/json/role; sessclean), sessions_cost (прочитано:
  since/by/json/top; sesscost), sessions_diff (прочитано: stat/json;
  sessscan), sessions_fork (прочитано: at/session/title/child; sesslive),
  sessions_gc (прочитано: dry-run/older-than/jobs-older-than/max-sessions/
  json; sessclean), sessions_grep (прочитано: n/session/role/json; sessscan),
  sessions_kill (прочитано: keep-lock/wait; sesslocks), sessions_pick
  (прочитано: tail; sesslive), sessions_purge (прочитано:
  dry-run/yes/matching; sesslocks), sessions_reap (прочитано:
  dry-run/all/idle-older-than; sesslocks), sessions_tree (прочитано:
  depth/json; sessview), sessions_watch (прочитано: interval; sesslive),
  update_providers (регистрация updateProvidersCmd — root.go:113, уходит в
  cli; settings).
- cli (1): root (флаги 47–105, PreRunE 50–63, AddCommand тринадцати команд
  107–121).

Сумма классов: 21 + 8 + 16 + 22 + 1 = 68.

### 4.2. Судьба всех пакетных переменных

Командные переменные (~130 `var xCmd = &cobra.Command{…}`) — неэкспортированные,
мутируют только в конструкторе своего пакета до регистрации; правило В6
(«не экспортировать изменяемые глобалы ради разрезки») соблюдено: наружу идёт
конструктор, а не переменная. Прочие переменные по целевым пакетам:

| пакет | переменные | тип |
|---|---|---|
| cli | webAppFactory (root.go:251) | тест-шов, неэкспортированная |
| clijournal | commandJournal (:77), exitFunc (:22), settingsAuditOnce (root.go:242), journalFlagValueAllowlist (:35), journalPositionalAllowlist (:46) | состояние + швы; тесты clijournal сбрасывают их как сегодня |
| cmdkit | oldGitIgnore, defaultGitIgnore (root.go:969,972 — embed-строки) | данные (embed) |
| cliproc | processStopInstalled (:17), var-блок console_ctrl_windows.go:11 (syscall-проки), procFreeConsole (console_detach_windows.go:11), cancelSignals (run_signals.go:8) | состояние/данные |
| sesslocks | lockProbeOptions, preAutoDeleteRemoveHook, lockEntryInfo (sessions_locks.go:210–221), reapAfterProbeHook, reapAfterUnlinkHook, reapPreIdentitySeam (sessions_reap.go:74–81), errBatchKillScopeUnconfirmed (sessions_kill_batch_scope.go:17) | тест-швы + ошибка |
| sessscan | tailFollowStartedHook (sessions_tail.go:54), fileWritingTools (sessions_diff.go:46) | тест-шов + данные |
| sessstate | readOnlyToolNames (sessions_stall.go:29) | данные |
| sessaudit | auditWaitOnlyPatterns (sessions_audit.go:51), auditDigits (sessions_audit_rules.go:41), auditRequiredSessionsColumns, auditRequiredMessagesColumns (sessions_audit_db.go:22–25 — колонки обязательной схемы аудита) | данные |
| modelatoms | atomRegistry (:119), atomGroupOrder (:188), atomDisplayOrder (:194), zaiReasoningLevels (:71), zai53ReasoningLevels (:99), zaiBooleanThinkingLevels (:117), apparentCAHCode (:415), removedLegacyCode (:418), claudeEffortSource (:79), cliEffortSource (:26), defaultEffortLevels (models_effort.go:18), cahVersion и cahNameMap (сгенерированный файл) | данные/реестр (read-only по контракту) |
| modelopts | providerEffortDocs (:48), unsetEffortDefaults (:316) | данные |
| providers | peakWindowRe (:25) | данные |
| serve | serveSignals (:22), serveNow, serveListen, serveAppFactory, serveAppShutdown, serveConsumeBootstrapToken, serveAPIStop, serveAuthFactory, serveResolver, servePinSpecs (serve.go:31–44) | тест-швы (все 10 уезжают вместе с serve.go) |
| api | apiNotifyContext (api_signal.go:18 — шов = commandSignalContext), apiRunStatePollInterval (api_run.go:80), var-блок api_sessions.go:138 (messageColumns) | швы/данные |
| queuecmd | queueTaskExecOverride (queue.go:423) | тест-шов |
| migrate | migrateFileRenamePauseSeam (migrate.go:24), handledLegacyEnvVars (:357), legacySkillRenames (:700) | шов + данные |
| diag | psSlotNames (ps_stale.go:15) | данные |
| settings | batchToolNamePattern (tokens.go:23), tokenIDPattern (tokens.go:46), updateProvidersSource (update_providers.go:13) | данные/шов |
| reports | dayNames (stats.go:40) | данные |
| install | skillSourceFiles (embed FS, multi_cli_convert.go:28), claudeWrushCommandTemplate, claudeWcrushCommandTemplate (embed, claude_init.go:34,47), claudeInitBlockPattern (:53), wrushLaunchGuidanceByTarget (:352), wcrushBackgroundGuidanceByTarget (:357) | embed + данные |

Ни одна переменная не становится экспортированной; пересечение границ — только
через функции и типы из §3.

### 4.3. ОС-пары и embed-ресурсы

| пара/ресурс | источник | цель |
|---|---|---|
| console_ctrl_{windows,other}.go | root-обвязка остановки | cli/cmdkit/proc/ |
| console_detach_{windows,other}.go | run.go:34 | cli/cmdkit/proc/ |
| login_link_{windows,other}.go | login | login/ |
| run_sigpipe_{unix,windows}.go | run.go | cli/cmdkit/proc/ |
| spawnlock_{windows,other}.go | queue | queuecmd/ |
| sessions_audit_driver_{modernc,ncruces}.go | sessions audit (build-теги = internal/db) | sessions/report/audit/ |
| gitignore/{old,default} | createDotRushDir (root.go:945–967) | cli/cmdkit/ (embed в appdb.go) |
| claude_wrush_command.md, claude_wcrush_command.md | claude_init.go:33,46 | install/ |
| claude_slash_command.yaml, claude_crush_fallback_command.yaml | multi_cli_convert.go:27 (`*.md *.yaml`) | install/ |
| stats/{index.html,index.css,index.js,AGENTS.md} | stats.go:22–28 | reports/ |

Каждая пара едет одной полосой одним файловым списком; accept-проверка полосы —
`go build` обеих целевых GOOS (windows и !windows через `GOOS=linux go build`).

### 4.4. Белые тесты и тесты через корень

Все 225 тестовых файлов — `package cmd` (белые). Правила расселения:

1. Тест едет в пакет кода, чьё поведение он проверяет (§2.2), и вызывает
   неэкспортированные имена СВОЕГО нового пакета напрямую; имена соседних
   пакетов — только через экспорт из §3.
2. Тест, входивший через `rootCmd.Execute()`/`SetArgs` ради диспетчеризации
   (`rush models …`, `rush sessions …`), переписывается на конструктор своего
   семейства (`models.NewModelsCmd().SetArgs(…)` и т. п.) — свежий объект на
   прогон, класс фляка #111 исчезает.
3. Тест, проверяющий поведение САМОГО корня (журнал запуска, unknown-subcommand
   guard, exit-коды Execute, help-контракт, golden дерева, --global-dir,
   runWebMode/cleartext), едет в cli и ходит по `NewRootCmd()`.
4. Тесты с `-cover`-зависимостью от чужих семейств (operator_help_contract —
   help четырёх семейств, command_journal_test — запись журнала при прогоне
   дерева) делятся по функциям: часть в пакет поведения, часть в cli.

Список 19 тестовых файлов, обращающихся к rootCmd, и 25, вызывающих Execute()
(grep, имена файлов): batch_cli_test, command_journal_fallback, command_journal_test,
execute_tokens_subprocess, api_args, api_process_helper, lock_test,
logs_prune_audit, logs_prune_heartbeat, models_integrity, models_state_restore,
models_use, ps_test, result_test, roles_test, run_custom_role_dispatch,
serve_api_process, serve_shutdown, serve_tls_process, sessions_audit_test,
sessions_compact_conversation_test, tokens_test, unknown_subcommand_test,
web_cleartext_command, web_cleartext_execute . root_test.go делится: TestMaybePrependStdin* → run, TestMcpAppOptions* → cmdkit, TestWebReadyBanner и TestRecoverAndLogPanic* → cli. Судьба: в cli остаются
execute_tokens_subprocess, command_journal_fallback, unknown_subcommand_test,
web_cleartext_command/execute, serve_api_process, serve_tls_process,
serve_shutdown (process-часть), root_help_datadir, command_args,
operator_help_contract (корневые проверки), + e2e-часть command_journal_test
(TestJournalSuccessCommand, TestJournalFailedCommand, TestJournalTokenExitCodes,
TestJournalWrongPasswordJournalled, TestJournalExecuteEndToEnd,
TestJournalGroupUsageErrorNotHelp — новые файлы cli/journal_e2e_test.go);
остальные переписываются на конструктор своего семейства (правило 2):
batch_cli_test→queuecmd, lock_test→settings (e2e-функция про журнал — в cli),
logs_prune_*→diag, models_integrity/state_restore/use→modeluse, ps_test→diag,
result_test→reports, roles_test→reports, run_custom_role_dispatch→run,
sessions_audit_test→sessaudit, sessions_compact_conversation→sessclean,
tokens_test→settings, api_args/api_process_helper→api (дерево api целиком
строит NewAPICmd — golden уже так делает, api_command_tree_golden_test.go:20).

### 4.5. Расселение тестовых файлов: правило по префиксу + исключения

Префикс → целевой пакет (все 225 файлов, префикс = имя до первого «_»):

| префикс | пакет | файлов |
|---|---|---|
| sessions_ | по второму токену: locks|kill|reap|purge|live_owner → sesslocks; gc|delete|stop_purge|compact → sessclean; render|print|show|tree|subagents → sessview; list|last|grep|diff|tail|jobs → sessscan; status|stall|why|activity|cache → sessstate; pick|watch|inject|cancel|fork|reset → sesslive; audit → sessaudit; batch → sessbatch; cost|budget → sesscost | 74 |
| run_ | run или runfinish (deadline|fail_review|finalization|finalization_gate|findings|role_model|format|timeout|flags_removed|help_reviewer → runfinish; rest → run) | 18 |
| models_ | modeluse (use|state|bump|integrity) или modelopts (list|unset|efforts|visibility) или modelatoms (atoms) | 16 |
| providers_ , provider_ | providers | 13 |
| serve_ | serve | 11 |
| api_ | api/… (по подтеме) | 11 |
| batch_ , queue_ | queuecmd | 11 |
| process_ , console_ , ps_ | cliproc | 8 |
| migrate_ | migrate | 5 |
| tokens_ , lock_ , settings_ | settings | 8 |
| logs_ | diag | 4 |
| login_ | login | 4 |
| command_ | clijournal (unit) или cli (e2e, §4.4) | 4 |
| ping_ | ping | 3 |
| worktree_ | wtree | 2 |
| claude_ , installer_ , multi_ , cli_ | install / targets (cli_refresh_test → install) | 5 |
| codex_ , gemini_ , grok_ , omp_ , opencode_ , qwen_ | targets | 6 |
| web_ , root_ , unknown_ , color_ , global_ , execute_ | cli | 10 |
| mcp_ | settings | 1 |
| ktav_isolate | cmdtest (сам хелпер) | 1 |
| helpers_ , testwait_ , main_ | делятся (см. ниже) | 3 |
| p1273_ | modelatoms (CAH-коды; вызов modelsUseCmd переписывается на NewModelsCmd) | 1 |
| p475_ | sesscost (parseSinceDuration/parsePlainInt) | 1 |
| regression_ | providers (TestProviders*, прочитано имён функций) | 1 |
| operator_ | cli (кроме трёх sessions/batch help-тестов: sessions_why/locks/watch help → sessstate/sesslocks/sesslive, batch help → queuecmd) | 1 |
| resolve_ , envelope_ | cmdkit / cliout | 2 |
| roles_ , result_ , projects_ | reports | 3 |

Девять файлов `sessions_*_test.go`, которые правило «по второму токену» буквально не покрывает, расселяются так (решение, не догадка исполнителя; при расхождении с импортируемыми идентификаторами файл едет к пакету, чьи неэкспортированные имена он использует, это покажет компилятор в полосе M1): `sessions_locksafety`, `sessions_live_lock_maintenance` → sesslocks (проверяют замки и их обслуживание); `sessions_ended_reason`, `sessions_own_job_status`, `sessions_run_driver_status` → sessstate (причина завершения, статус своей работы и драйвера); `sessions_wake_livework`, `sessions_reset_async` → sesslive (живая работа и сброс; `reset` уже в списке sesslive); `sessions_truncate` → sessview (усечение вывода render/print/show); `sessions_help_text` → корневой пакет `sessions` с `cmdtest` (проверяет справку всего дерева семейства, нужны все подпакеты).


Итого: 74+18+16+13+11+11+6+5+8+5+8+4+4+4+3+2+5+6+8+1+1+3+1+1+1+1+2+3 = 225 (sessions 74 уже включает sessions_batch_fixture_helpers и sessions_fixture_db — они уходят в cmdtest как исключение, см. список ниже).

Явные исключения из префиксного правила (файл → судьба):
- helpers_test.go — делится: TestShort/TestTruncate/TestDash/TestMaskKey/
  TestScopeFromFlags → cmdkit/cliout, TestSplitModelEffort и
  TestDefaultBaseURLFor → modelatoms/providers.
- testwait_test.go → cmdtest/wait.go (хелпер WaitOrDump; потребители
  migrate_context, sessions_kill, sessions_locksafety импортируют cmdtest).
- main_test.go → cmdtest.Main + шим TestMain в каждом пакете с тестами.
- settings_lock_kdf_testinit_test.go → вызов cmdtest.LowerSettingsPasswordIterations()
  из cmdtest.Main (сегодня — init в тесте пакета).
- ktav_isolate_test.go → cmdtest.KtavIsolateGlobalEnv + ktavGateOn (потребители:
  lock_ktav, migrate_ktav, ps_stale_ktav).
- sessions_batch_fixture_helpers_test.go и sessions_fixture_db_test.go → хелперы (SeedCmdBatchDelegation, SeedAuditRulesMigratedDB, AuditRulesFixture) в cmdtest/seeds.go; их вызовы в тестах sessstate/sesslive/sessaudit/sessclean импортируют cmdtest.
- command_journal_test.go — делится по §4.4 (unit → clijournal, e2e → cli).
- operator_help_contract_test.go — делится по четырём семействам + cli (README).
- command_args_test.go → cli (ходит по NewRootCmd).
- p1273_cah_integration_test.go → modelatoms.

### 4.6. testdata и golden

- testdata/api_command_tree.golden → api/testdata/ (тест ходит по NewAPICmd).
- testdata/claude_*_original.md → install/testdata/ (installer_template_contract
  сравнивает с ними встроенные шаблоны).
- Новый testdata/root_command_tree.golden → cli/testdata/ (пишет полоса K2, §3.2).

## 5. План исполнения

Все полосы идут по `internal/serve/cmd` (после этапа C). Правило §3.7 плана:
агенты гоняют только `gofumpt`, `go build`, `go vet` затронутых пакетов через
`.rush/tmp/golock.sh`; тесты серийно — оркестратор (`go test -p 1 -parallel 1
-count=1` под `capm 2g`). Не более 5 агентов одновременно. Полосы внутри одного
пакета не параллелятся (конфликт файлов); параллелятся полосы с непересекающимися
файлами. Каждая полоса обновляет запись `serve/cmd` в
`.githooks/dir_entries_allowlist.txt` (число только падает; расписание ниже —
без провалов ниже фактического значения).

Оракулы каждой полосы переноса: (о1) sorted `^(func|type|var|const)` строк
перенесённых файлов совпадает с точностью до экспорт-регистра из §3; (о2) число
тестовых функций в репозитории не меняется; (о3) `go build ./...` + `go vet`
затронутых пакетов; (о4) `TestRootCommandTreeGolden` и
`TestRunnableLeafCommandsDeclareArgs` зелёные; (о5) серийный прогон затронутых
пакетов у оркестратора.

| полоса | содержание | исходные строки §2.2 (файлов) | ждёт |
|---|---|---|---|
| K1 | cli/cmdkit: flags.go, parse.go, appopen.go, appdb.go (куски из root.go, providers_patch.go, sessions_cost.go, run_helpers.go), out/ (envelope.go, exitcode.go-слияние, display.go из sessions.go/providers_patch.go/sessions_kill.go, cliui.go), proc/ (signals.go-слияние, run_sigpipe_×2, console_×4), cli/cmdkit/journal (command_journal.go), cli/cmdtest (из 6 тестовых хелперов), TestMain-шим | 21–25, 27–29, 69–70, 99–101 (13) + части 87, 74, 95, 106, 122, 132 | — |
| K2 | cli: root.go + execute.go (слить 20, 87, 160, 162), NewRootCmd, register-вызовы вместо 30 init с rootCmd.AddCommand, golden-тест дерева, правка main.go | 20, 87, 160, 162 (4) | K1 |
| K3 | cmdtest-шимы тестов, ResetCommandContexts, разбор helpers_test, testwait, main_test, settings_lock_kdf, ktav_isolate | тестовые файлы | K2 |
| M1 | sessions → 10 пакетов, деление sessions_kill.go | 106–153 (48) | K1–K3 |
| M2 | run (88–98, 102), wtree (163–166) | 16 | K1–K3 |
| M3 | models (47–61), ping (67–68), providers (72–78), деления providers.go и providers_peak_hours.go | 24 | K1–K3 |
| M4 | api (1–12), queuecmd (13, 14, 82, 154, 155), serve (104–105) | 19 | K1–K3 |
| M5 | login (35–38), diag (39–42, 79–81), migrate (44–46), деление migrate.go | 14 | K1–K3 |
| M6 | install (15–17, 62), targets (18–19, 30–33, 63–66, 83–84), settings (26, 34, 43, 103, 158, 159, 161), reports (71, 85, 86, 156, 157), слияния targets | 28 | K1–K3 |

#### M1 ownership override: downward session-helper edges

Эта таблица уточняет только владение перечисленными символами из §2.2 и заменяет для них конфликтующие назначения; владельцы остальных источников §2.2 остаются без изменения. Направление: пакеты-команды импортируют `sessions/sesskit`, а `sesskit` не импортирует своих потребителей. Планировка каталогов должна соблюдать лимит ≤7 элементов в каждом каталоге; сам лимит — задача размещения/группировки, не основание останавливать полосу. Останавливать и эскалировать следует только если перенос без изменения поведения невозможен; назвать точные блокирующие символы и ребро зависимости как witness.

Добавление `sessions/sesskit/` требует соответственно сгруппировать целевое дерево `sessions/` из §2.1 так, чтобы каждый каталог оставался в пределах 7 элементов. Это корректировка размещения, не стоп-условие; остановка допустима только при доказанной необходимости изменить поведение, с точными блокирующими символами и ребром зависимости.

| исходный файл / символы | пересмотренный владелец | зависимости обеих сторон / граница |
|---|---|---|
| `sessions_budget.go`: `sessionBudget`, `sessionBudgets`, `orderBySubtreeActivity`; извлечённый `formatInt64` из `sessions_cost.go` | `sessions/sesskit/budget.go`: `SessionBudget`, `SessionBudgets`, `OrderBySubtreeActivity`; `sessions/sesskit/format.go`: `FormatInt64` | `sesscost` и команды-сессии → `sesskit`; leaf использует `internal/session` и stdlib. Root private wrappers остаются для потребителей/тестов, пока нужны. |
| `sessions_cost.go`, `sessions_cost_rows.go`, `sessions_cost_models.go` и их тесты: команда, строки, модели/группировка cost | `sessions/report/cost/` (`sesscost`) | `cmd`-агрегатор → `sesscost`; `sesscost` → `sesskit`, `cmdkit`, `cliout`, `app`, `session`, без импорта `cmd`. Фабрика `NewSessionsCostCmd(setup func(*cobra.Command) (*app.App, error))` получает локальный `setupApp` wrapper от `cmd`, сохраняющий установку `commandLoadOptions` через `installCfgSeams`. Результат присваивается ровно один раз локальной неэкспортированной `cmd.sessionsCostCmd`: сохраняются root-проверка указателя и флаги. Срез cost реализован; для `cost`, `sesskit` и root успешно выполнен `go test -c` (тестовые бинарии скомпилированы, тесты не запускались по требованию). Для root успешны `go build` и `go vet` на linux/darwin/windows; официальный fmt diff пуст, selfcheck завершился с rc=0. Эти прежние проверки cost не подтверждают текущий shared closure activity/facts. |
| `sessions_locks.go`, `sessions_activity.go`, `sessions_print.go`: `kindIsLive`, call-tree activity и её freshness/note helpers, duration formatter | `sessions/sesskit/activity.go`: exported `CallTreeActivity`, compute/batch/fresher/latest/note helpers и `KindIsLive`; `sessions/sesskit/format.go`: `FormatDurationShort` | `sesslocks`, `sesslive`, `sessclean` и display consumers → `sesskit`; зависимости activity ограничены низкими `app`, `session`, `message` и `cmdkit/out`. Root private compatibility wrappers remain for staying consumers/tests. |
| `sessions_compact.go`: `compactHint`, `humanBytes`, `percentOf` | `sessions/sesskit/maintenance.go`: `CompactHint`, `HumanBytes`, `PercentOf` | `sessclean`/команды → `sesskit`; root `compactMinReclaimBytes`, `compactHintMinBytes` и `compactHintMinFraction` остаются private. `CompactHint` получает providers-функции, сначала читает `Stats`, затем отдельно вызывает provider для каждого порога по мере проверки; сохранить этот порядок/раздельные reads. |
| `sessions_audit_rules.go`: `auditPlaceholders`, `auditQueryChunk` | `sessions/sesskit/maintenance.go`: `AuditPlaceholders`, `AuditQueryChunk` | `sessaudit`, `sessbatch` → `sesskit`; только SQL placeholders и размер chunk, без зависимости на report-пакеты. Существующие root private wrapper/constant остаются для потребителей/тестов. |
| `sessions_batch_audit.go` + `sessions_batch_read.go`: общий `reduceBatchSessionFacts` и shared fact/runtime/type/question closure | `sessions/sesskit/facts.go`: exported `BatchRootFact`, `BatchSessionFacts`, reducers, runtime readers, and question helpers | `sessaudit`, `sessstate`, `sessview`, `sessclean`, `sesslive` → `sesskit`; `sesskit` может зависеть от `app`, `session`, `store/db`, но не от `cmd`/потребителей; audit-specific reports остаются в `sessaudit`. `AwaitingAnswer` — exported callsite, вызывается `sessions list`; private compatibility wrappers в root сохраняют прежние имена для потребителей/тестов. |
| `sessions_batch_act.go`, `sessions_reset.go`: `batchResetSessionHistory`, `batchResetRefusedError`, `resetRefusedError` и live-loop refusal | `sessions/sesskit/reset.go`: `BatchResetSessionHistory`, `BatchResetRefusedError`, `ResetLiveLoopRefusal`, `ResetRefusedError` | live/reset/batch-потребители → `sesskit`; зависимости ограничены низкими `app`/`session` и reset-типами, без импорта потребителей или верхнего `cmd`. Root private wrappers остаются для staying consumers/tests. |
| `sessions_status_label.go`, `sessions_list.go`: status labels, question reader/interface, awaiting-question status mapping | `sessions/sesskit/status.go`: labels, `PendingChildQuestionReader`, `SessionQuestionReaderForApp`, `ListStatusesWithAwaiting` | `sessstate`, `sessview`, `sessscan` → `sesskit`; closure может зависеть от `app`/`session`, но не от потребителей. Root private compatibility wrappers остаются для потребителей/тестов. |

| `sessions_grep.go`, `sessions_diff.go` (grep + diff): команды и их общий обход путей | `sessions/view/scan/` (`sessscan`): фабрики `NewSessionsGrepCmd(setup)` / `NewSessionsDiffCmd(setup)`; exported `ExtractFilePaths` (общий извлекатель путей файлов) | `cmd`-агрегатор → `sessscan`; `sessscan` → `sesskit`, `cmdkit`, `app`, `session`, без импорта `cmd`. Root-обёртка `ExtractFilePaths` остаётся в корне пакета `cmd` для regression-теста. Root `sessions.go` подключает фабрики; дублирующиеся регистрации флагов удалены. Срез scan компиляторно доказан (compile-only `sessscan` и root, см. статус ниже). |
| `sessions_kill.go`: форматтер `formatAgo` (возраст) | `sessions/sesskit/format.go`: exported `FormatAgo` | `sesslocks`/display-потребители → `sesskit`; root private wrapper `formatAgo` остаётся для staying-потребителей/тестов. |
| `sessions_tree.go` (tree): команда и рендер дерева | `sessions/view/` (`sessview`): фабрика `NewSessionsTreeCmd(setup)`, exported `TreeJSON` и `PrintTreeNode`; root aliases `treeJSON`/`printTreeNode` остаются | `cmd`-агрегатор → `sessview`; root `sessions.go` подключает фабрику. Срез tree входит в компиляторно доказанный scan/view pass. |

Следующий запланированный срез по boundary-аудиту: **fork → `sesslive`** (live slice), затем **locks вложенным под live**, при этом `gcDropMaintenanceProtected` остаётся владельцем `sessclean`; `indexByID` должен переехать в `sessview` вместе с `watch`; `status`/`stall` остаются в `sessstate`; известная опасность — split hazard вокруг `liveOwnerProtection` при разрезке GC/locks.

Статус scan pass (M1 SCAN PASS GREEN): pass полностью доказан компилятором — compile-only `sesskit`/`cost`/`view`/`scan`/root зелёные; для root успешны `go build` и `go vet` на linux/darwin/windows; шесть guards rc=0, fmt diff пуст, selfcheck rc=0. Тесты не запускались (запрет исполнения тестов); вся M1 ещё не завершена.

Текущая реализация `sessions/sesskit/` состоит ровно из 7 production-файлов: `activity.go`, `budget.go`, `facts.go`, `format.go`, `maintenance.go`, `reset.go`, `status.go`.

Сумма исходных строк полос: 13 + 4 + 48 + 16 + 24 + 19 + 14 + 28 = 166 (все не-тестовые файлы; тестовые K3 сверх счёта). Параллельность: K1 → K2 → K3 строго последовательно (общие файлы root.go и тестовые шимы); M1–M4 — один раунд до четырёх агентов (непересекающиеся списки), M5 и M6 — следующий раунд.

Расписание храповика `serve/cmd` (элементы = не-тестовые файлы в корне каталога +
код-подкаталоги; тесты, testdata и ресурсы не считаются; queue и notification в
счёте). Числа выведены из счёта строк полос (сумма 166); в allowlist вносится
фактическое значение после каждой полосы, направление только вниз, конец ровно 7.
Ratchet 2026-11-17: guard завершается с rc=0 при текущем значении allowlist 136
(guard-verified count; значение оставлено без изменения).

| после | элементов serve/cmd | изменение |
|---|---|---|
| этап C | 168 | 166 файлов + queue + notification (вход в allowlist) |
| K1 | 156 | −13 файлов в cli/cmdkit, +каталог cli |
| K2 | 152 | −4 файла в cli |
| M1 | 105 | −48 файлов, +каталог sessions |
| M2 | 90 | −16 файлов, +каталог run |
| M3 | 67 | −24 файла, +каталог modelctl |
| M4 | 49 | −19 файлов, +каталог admin |
| M5 | 35 | −14 файлов |
| M6 | 7 | −28 файлов; осталось 7 каталогов: cli, sessions, run, modelctl, admin, queue, notification |

## 6. Риски и открытые вопросы оператору

1. sessions — самая тяжёлая полоса (49 файлов, 10 пакетов): скрытые
   внутрисемейные связи (например, использование хелперов sessview в sessstate)
   вскрываются при переносе. Владельцы символов M1 уточнены в подразделе выше;
   остальные рёбра — карта проекта, не доказательство отсутствия циклов до компиляторной проверки.
2. Порядок регистрации: золотой тест дерева (K2) — единственная защита от
   дрейфа help/flags; он должен быть зелёным ДО первой M-полосы.
3. cahgen/cahsync пишут `models_cah_gen.go` по зашитому пути — при переносе в
   modelctl/models/atoms путь в генераторе (internal/cahgen или tools/) правит
   полоса M3; проверить `grep -rn models_cah_gen tools/ internal/cahgen`.
4. Конфигурация CI по путям (`ci-policy.toml`, shards.toml, golangci) после
   разрезки теряет префикс `internal/cmd` — переезд B переписывает пути этапа C,
   но 39 новых подпакетов нужно распределить по шардам; вопрос оркестратору на
   этап D2, не блокирует полосы.
5. `sessions_audit_driver_*` build-теги должны остаться байт-в-байт — риск
   двойной регистрации драйвера sqlite; проверка о4 + `go vet`.
6. Не проверено мной (вскроется компиляцией в полосах): полный состав 13 имён
   run→wtree (5 подтверждены чтением: gitIn, isGitIgnored, listGitWorktrees,
   planWorktree, validateWorktreeSlug). Остальные пункты прежнего списка
   сомнения проверены чтением: init update_providers — флаг source;
   settingsAuditOnce — только command_journal.go:86 и его тест; var-блоки
   sessions_audit_db.go:22 (колонки схемы) и api_sessions.go:138 (колонки
   таблиц).
