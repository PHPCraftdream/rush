# Общий каталог данных для linked worktree

Статус: дизайн архитектора (@oxx), 2026-10-01, код по состоянию на `ea3a979a`.
Решение оператора (не пересматривается): существующие БД в worktree не трогаем —
в общую БД идут только новые worktree. Мотивация оператора: аномалии и историю
агентских сессий нужно собирать по проекту в одном месте (одна БД, один лог), а
`git worktree remove` сейчас уничтожает их вместе с `<wt>/.rush`.

## Цель и приёмка

- `rush run` в новом linked worktree пишет сессии, сообщения, `async_jobs` и логи
  в `<main>/.rush`; `git worktree remove` их не уничтожает; `rush sessions
  list/locks/watch` и web из корня видят агентов.
- Ход сессии никогда не исполняется в чужом checkout — ни по явной команде, ни
  фоновыми механизмами.
- Сборка ветки (`go run .`, `go build`, `go test`) из worktree не мигрирует общую
  БД и не пишет в неё по умолчанию.
- Один `logs/rush.log` на проект без потерянных и перезаписанных строк; по каждой
  строке видно, какой процесс и какой worktree её записал.

## Термины и инварианты

- **Workspace процесса** — канонический корень checkout (`worktreeRoot(workingDir)`;
  вне git — канонический cwd). Подкаталоги одного checkout дают один workspace.
- **Shared-from-linked** — процесс в linked worktree, чей каталог данных =
  `<main>/.rush`. **Home** — любой другой процесс.
- **WS-1.** Ход сессии S (CLI, web, pump, wake, drain) исполняет только процесс с
  workspace = `S.workspace_root`; без привязки (legacy) — только home-процесс.
- **WS-2.** Общую БД мигрирует только процесс с `MayMigrate` и только под
  межпроцессным `migrate.lock`; при разошедшейся истории схемы автоматической
  миграции нет.
- **WS-3.** Каталог данных и его источник вычисляются один раз в `Load` и до конца
  процесса не меняются (reload их фиксирует).

## 1. Пространство id сессий

Факт: поля рабочего каталога у сессии нет (`FolderScopeSpec.WorkingDir` в
`session_run_queue.call_data` на маршрутизацию не влияет; `projects.json` —
cwd→data_dir без сессий; `async_hosts` — pid/label).

Схема (аддитивная миграция): `sessions.workspace_root TEXT NOT NULL DEFAULT ''`,
`sessions.git_branch TEXT NOT NULL DEFAULT ''`, индекс
`(workspace_root, updated_at) WHERE parent_session_id IS NULL` для `--continue`,
`queue_tasks.workspace_root TEXT NOT NULL DEFAULT ''`. Старые бинарники
совместимы (sqlc разворачивает `*` в явный список; INSERT без колонок пишет `''`).

Заполнение — в одной точке `CreateSession` (через неё идут `Create*`,
`CreateTaskSession`, `CreateTitleSession`, `ForkSessionTx`); значения подставляет
сервис сессий, созданный в `app.New` с workspace процесса из
`ConfigStore.WorkingDir()` (не `os.Getwd()` — у SDK-хоста различаются). Ветка —
из `<git-dir>/HEAD` в момент создания, без запуска git; detached → `''`.
Для аудита хранится `workspace_root` (basename = имя worktree) и `git_branch`;
cwd и коммит — в лог-записи `process start`, не в `sessions`.

Предикат владения: `owns(S) = S.workspace_root == ws || (S.workspace_root == '' && home)`.

| Путь | Решение |
|---|---|
| `rush run --session X`, X нет | создаётся и привязывается к текущему workspace |
| X свой | как сейчас |
| X чужой, или legacy при shared-from-linked | **отказ** в `resolveSession` до лока и до записей в строку сессии: `session X belongs to <root> (branch b); run it there or "rush sessions fork X" here` (+ «workspace no longer exists», если каталог исчез); раньше «already in use» |
| `rush run -C`, fold модели по `--role` | `GetLast` только среди `owns` |
| дочерние сессии (delegation, title, agent-tool, agentic_fetch) | создаются в процессе родителя — workspace наследуется автоматически |
| RunQueuePump | `ListPendingRunQueueEntries` фильтрует по `owns` (JOIN `sessions`); чужие строки не арендуются, `attempts` не растёт |
| wake scheduler | один и тот же фильтр в `ClaimDue` **и** `NextDue` (иначе `NextDue` вернёт момент в прошлом, claim пуст — вечный цикл) |
| drain/reaction | факт `ForeignWorkspace` в `readTurnFacts`, арбитр → `VDefer` без recheck |
| любой ход (страховка) | guard в `sessionAgent.runOwned` перед `TryAcquireSessionLockWithOptions` → `ErrForeignWorkspace`; pump превращает его в nack без штрафа, не в dead-letter |
| web: список, просмотр, tree, jobs | все workspace |
| web: inject | сохраняет сообщение (ход исполнит владелец) |
| web: send, interrupt | guard не даёт запустить ход, в UI — ошибка |
| web: rerun | проверка до `TruncateForRerun` |
| web: delete other sessions | только `owns` |
| `sessions fork X` | новая сессия привязывается к workspace форкающего процесса (`--cwd`) — единственный способ продолжить историю в другом checkout |
| `sessions reset/inject X` | из любого места; привязка не меняется; ход исполняет владелец |
| cancel, kill, delete, purge, gc, reap, why, watch | из любого места |

Отказ, а не предупреждение: оркестратор читает `.error` JSON-конверта, stderr не
видит; молча продолженная чужая история тратит бюджет и работает по путям другого
checkout; обход дешёвый (другой id или fork). Legacy-строки (`''`) home-процессы
ведут как сейчас; shared-from-linked их не трогают; backfill не нужен.

## 2. Миграции из dev-сборок

Сейчас: `db.connect` → `Migrate` → goose `Up`; своей проверки версии нет; версии
БД, неизвестные бинарю, goose игнорирует (поднимают dbMax); неприменённая версия
бинаря ниже dbMax → `missing (out-of-order)` → процесс не стартует; межпроцессного
лока нет.

«Только аддитивные» недостаточно: (1) ломается порядок (dev-сборка применила M_b,
потом в main вливается M_a < M_b → задеплоенный бинарь не стартует); (2) goose
помнит версию, не содержимое — правка применённой миграции тихо расходит схему;
(3) «аддитивный» DDL ломает старых писателей (NOT NULL без default, CHECK, UNIQUE,
триггеры), data-миграции не аддитивны, брошенная ветка оставляет миграцию навсегда;
(4) **параллельный старт N агентов после деплоя — воспроизводимый P1**: все
запускают `Up`, второй падает на `ALTER TABLE … ADD COLUMN` (`duplicate column name`).

Защита в `internal/db/connect.go`:
- `<dataDir>/migrate.lock` (OS-лок) на всю последовательность «прочитать версии →
  решить → Up». `FileLock` переносится из `internal/session` в листовой пакет
  (session импортирует db — иначе цикл); чистый перенос.
- A — применённые версии, E — встроенные в бинарь; `unknown = A∖E`, `pending = E∖A`:
  `unknown=∅` → применить pending с `goose.WithAllowOutofOrder(true)` (при
  `MayMigrate`); `unknown≠∅, pending=∅` → открыть без миграций, один WARN;
  `unknown≠∅, pending≠∅` → `ErrSchemaDiverged` (версии + путь лечения).
- `MayMigrate=false` только у dev-сборки в режиме shared-from-linked →
  `ErrSchemaMigrationNotAllowed` («укажите `--data-dir` на временный каталог»).
  Задеплоенный бинарь мигрирует из любого места; dev-сборка в главном checkout —
  как сейчас.
- Dev-сборка в linked worktree по умолчанию берёт `<wt>/.rush/dev` (не `<wt>/.rush`
  — появившийся там `rush.db` включил бы legacy-правило). Dev-сборка определяется
  по `os.Executable()`: под `os.TempDir()`/`$GOTMPDIR` (`go run`) или внутри
  главного/текущего checkout (`go build -o`); цель деплоя на PATH, npm и
  `go install` — не dev. В общую БД dev-сборка попадает только через явный
  `--data-dir`, и тогда без миграций.
- Под `testing.Testing()` переадресация выключена (seam для тестов config) —
  `go test` в worktree, включая pre-push, общую БД не видит.
- «Только аддитивные, применённую миграцию не править» остаётся дисциплиной ревью.

## 3. Определение linked worktree и главного корня

Probe кэшируется по каноническому каталогу (как `worktreeRootCache`); из env
убираются `GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE` (git
экспортирует их в хуки, pre-push гоняет тесты).

1. `worktreeRoot(dir) == ""` → не git → без изменений.
2. `git rev-parse --git-dir --git-common-dir` (`cmd.Dir=dir`; относительный вывод
   резолвится от `cmd.Dir`, результат канонизируется). Равны → main checkout или
   submodule → не linked. Этот же git-dir даёт ветку.
3. Иначе `git worktree list --porcelain`: первая запись — главный worktree.
   `bare` → без изменений. Путь (`FromSlash` + `canonicalConfigPath`) = свой
   toplevel → не linked. Иначе это главный корень: обязан существовать как
   каталог, на Unix владелец = владелец cwd; иначе без изменений + WARN.

Почему: `--path-format=absolute` требует git ≥ 2.31 (Ubuntu 20.04 — 2.25); разбор
`.git`/`commondir` и «главный = родитель common-dir» ошибаются для bare с именем
`.git`, linked worktree подмодуля и `--separate-git-dir`. Нужен git ≥ 2.7; любая
ошибка — без изменений. Стоимость: +1 git на процесс, +1 в linked worktree, 0 в тестах.

| Случай | Итог |
|---|---|
| main checkout и подкаталоги | `<main>/.rush`, как сейчас |
| linked worktree внутри/вне каталога репо | `<main>/.rush` |
| submodule (в т.ч. внутри linked worktree суперпроекта) | без изменений |
| linked worktree подмодуля | главный — checkout подмодуля |
| bare с worktree (любое имя) | без изменений |
| нет git / ошибка / старый git | без изменений |

## 4. Что лежит в каталоге данных и какие связки ломаются

| Объект | После переключения | Решение |
|---|---|---|
| `rush.db` (+wal/shm) | общий | цель |
| `rush.json`, `rush.json.lock` | общий проектный конфиг | принять (`--scope workspace` из любого worktree меняет для всех; относительные пути резолвятся от cwd) |
| `locks/` | общий | обязательно (то же пространство имён, что БД) |
| `hosts/`, `async_hosts`, `session_drivers` | общие | обязательно; dead-host recovery пишет только факты |
| `logs/` | общий | writer — ниже |
| `attachments/` | общий | имена timestamp+uuid |
| `commands/`, `init` | проектные | принять |
| `queue.lock`, `queue_tasks` | были на checkout | разделить по workspace |
| `rush-fetch-*` | в общем каталоге | defer-удаление; после краша мусор в main |
| skills, `.rush/system-prompts`, `qwen/gemini-mcp-id`, `.rush/stdin` | не в каталоге данных | без изменений |

Связки:
- **P0 — исполнение «любым процессом».** RunQueuePump стартует в каждом процессе
  (`setupApp`: web, `rush run`, `rush sessions *`, `queue run`), первый tick сразу;
  `ListPendingRunQueueEntries` берёт строки всех сессий и исполняет через
  `Coordinator.Run` в cwd этого процесса; `call_data` workspace не несёт. Такие
  строки дают `sessions inject --interrupt`, `InterruptAndSend`, abandon-with-handoff.
  Итог: web в корне выполнит ход агента в main. То же — `ListDueWakeSchedules` и
  drain. Лечение — WS-1.
- **P0 — вечный цикл wake scheduler**, если фильтр только в `ClaimDue`.
- **P1** — `--continue` и fold модели через `GetLast` берут чужую сессию.
- **P1** — `rush queue run`: общий `queue.lock`, claim/reclaim по всем задачам,
  подпроцесс с cwd раннера. Лечение: `queue_tasks.workspace_root` в `Add`, фильтр
  claim/reclaim, лок `queue-<hash(ws)>.lock`.
- **P1** — web `handleDeleteOtherSessions` удаляет сессии всех worktree.
- **P1** — логи (ниже) и параллельные миграции (§2).
- Корректны без изменений: `recoverInterruptedTurns` (session-lock), dead-host
  recovery, очистка `session_drivers`, `sessions reap/kill/cancel/gc/purge` с
  охватом проекта.

**Логи сейчас** (`internal/log.NewLogger`: lumberjack, 10 MB × 3, 30 дней, gzip,
атрибут `pid`) небезопасны между процессами — **уже сегодня** (в корне web и
`rush run` пишут в один файл):
- `openNew` (создание и после ротации) — `O_CREATE|O_WRONLY|O_TRUNC` без
  `O_APPEND`: процесс пишет по своему offset и затирает строки других;
- Windows: без `FILE_SHARE_DELETE` `os.Rename` при ротации падает, пока файл
  держит другой rush (web держит всегда); дескриптор уже закрыт → каждая запись
  снова пытается ротировать → процесс не пишет вовсе; процесс, стартовавший при
  файле ≥ 10 MB, не пишет вообще;
- POSIX: остальные пишут в переименованный бэкап, mill его сжимает и удаляет —
  строки теряются;
- `rush logs prune` утверждает «rush does not auto-rotate».

Решение: в `NewLogger` writer только с дозаписью
(`os.OpenFile(O_APPEND|O_CREATE|O_WRONLY)`, одна запись на record), без ротации в
процессе; размер — `rush logs prune` (truncate безопасен для `O_APPEND`);
межпроцессная ротация — backlog. Атрибуция: константный атрибут `ws` (тот же
канонический корень, что `sessions.workspace_root`) в каждой строке + одна INFO
`process start` (pid, ppid, version, `dev_build`, команда, cwd, `ws`, ветка,
`data_dir`, `data_dir_source`, `--session`); argv не писать (там промпты); ветку в
каждую строку не писать.

## 5. Порядок выбора каталога данных

1. `--data-dir`.
2. `options.data_directory` из глобального/проектного конфига.
3. Ближайший существующий `.rush` (поиск как сейчас, граница — `worktreeRoot`):
   не linked → использовать; linked → только если в нём есть `rush.db` **и** у
   worktree нет маркера shared-режима (источник `legacy-local`, WARN), иначе
   игнорировать (WARN «stray local DB ignored», если `rush.db` есть).
4. Linked worktree, главный корень определён, не под тестом: dev-сборка →
   `<wt>/.rush/dev` (`dev-isolated`); иначе `<main>/.rush` (`shared`), `setupApp`
   создаёт маркер `<main>/.rush/workspaces/<hash(wt)>`.
5. `<cwd>/.rush` (`default`).

Поправки к предложению: признак legacy — файл `rush.db`, не каталог (`.rush/`
появляется и без БД: skills, system-prompts, mcp-id, `.rush/dev`); маркер нужен,
чтобы pre-feature dev-сборка, создавшая `<wt>/.rush/rush.db`, не перевела worktree
в legacy молча; разрешать один раз и хранить источник в `ConfigStore` (setDefaults
перезапускается после merge и при reload); `ResolveDataDirectory` (rescue-команды)
обязан давать тот же результат. Env-переключатель не нужен: отказ —
`options.data_directory: ".rush"` в проектном `rush.json` или `--data-dir`; откат —
передеплой предыдущего бинаря.

## Шаги реализации

- **A. Логи:** `internal/log/log.go` (`NewLogger`, `Setup`), `internal/cmd/root.go`
  (`setupApp`, `setupAppLite` — атрибуты и `process start`), `sdk/sdk.go`,
  `sdk/library_mode.go`, `internal/cmd/logs_prune.go` (справка), `go.mod`
  (уходит lumberjack).
- **B. Миграции:** `internal/session/file_lock*.go` → листовой пакет (вызовы:
  `internal/config/atomicwrite.go`, `internal/projects/projects.go`,
  `internal/deploy/deploy.go`, `internal/agent/cliprovider/mcpserver_settings.go`);
  `internal/db/connect.go` (`connect`, `Migrate` — лок, множества, опция политики,
  типизированные ошибки); вызовы `db.Connect` в `internal/cmd/root.go` и `sdk/`.
- **C. Привязка** (в раскладке «одна БД — один checkout» поведение не меняется):
  миграция `<ts>_add_workspace_to_sessions.sql` (sessions, queue_tasks);
  `internal/db/sql/sessions.sql` (`CreateSession`, `GetLastSession`),
  `run_queue.sql`, `wake_schedules.sql` (`ListDueWakeSchedules`,
  `NextDueWakeScheduleAt`) + sqlc; `internal/session/session.go` (поля,
  Workspace/`owns`, опция сервиса), `session_lifecycle.go`, `session_fork.go`,
  `session_read.go`, `session_runqueue.go`, `run_queue_entry_*.go`
  (`ErrForeignWorkspace`), `wake_schedule_store.go`; `internal/queue/queue.go`;
  `internal/app/app.go` (`New`), `app_run_session.go` (`resolveSession`);
  `internal/agent/agent_run.go` (`runOwned`), `turn_arbiter.go` (`readTurnFacts`);
  `internal/server/handlers_agent_rerun.go`, `handlers_sessions.go`
  (`handleDeleteOtherSessions`, поля в payload); `internal/cmd/queue.go`; JSON
  `sessions list/show`.
- **D. Переключение:** `internal/config/load_worktree.go` (probe, кэш, очистка
  env), `load_defaults.go`, `load.go` (`Load`, `ResolveDataDirectory`),
  `store_reload.go`, `store.go`; определение dev-сборки (seam пути exe); маркер в
  `setupApp`/`setupAppLite`/sdk; `MayMigrate` в `db.Connect`; справка `--data-dir`.
- **E. Документация оркестратора:** `internal/cmd/claude_wrush_command.md`,
  `claude_slash_command.yaml`, `multi_cli_convert.go`, golden в
  `internal/cmd/testdata/`: id сессий проектные; продолжить сессию в другом
  worktree — только `sessions fork` с `--cwd`; `rush sessions` из корня видит агентов.

Порядок: A, B, C → D → E. После деплоя сразу перезапустить долгоживущие rush в
главном checkout (у старого бинаря нет WS-1 — его pump выполнит durable-строки
новых worktree в main); новые worktree — только после перезапуска; деплоить
только из main. `worktrees/rloop`, `worktrees/wtitle`,
`worktrees/wrush-four-session-bugs-20260924` дорабатывают на своих БД
(legacy-local).

## Обязательные тесты (у каждого revert-check)

| Тест (пакет) | Ловит | Revert-check |
|---|---|---|
| config: linked worktree внутри/вне репо, из подкаталога → `<main>/.rush` | основное правило | убрать шаг 4 → `<wt>/.rush` |
| config: `<wt>/.rush/rush.db` без маркера → local; с маркером → shared; `<wt>/.rush/skills` без БД → shared | признак legacy, маркер | «каталог есть» или без маркера |
| config: main, submodule, bare + worktree, bare с именем `.git`, нет git → без изменений | классификация | «главный = dir(--git-common-dir)» / «.git-файл = linked» |
| config: `--data-dir` и `data_directory` побеждают | приоритет | переставить шаги |
| config: `Load` с `<main>/.rush/rush.json` (merge) и reload = `ResolveDataDirectory` (каталог и источник) | WS-3, паритет rescue | источник из `dataDir!=""` в setDefaults |
| config: без seam под тестом нет переадресации и git; `GIT_DIR`/`GIT_WORK_TREE` не влияют | изоляция тестов | убрать guard/очистку env |
| config: dev-сборка (seam пути exe) в linked worktree → `<wt>/.rush/dev` | изоляция dev | убрать определение |
| db: два `*sql.DB` на БД, отстающей на одну ALTER-миграцию; seam держит A после чтения версий | `migrate.lock` | без лока → `duplicate column name` |
| db: unknown+pending → `ErrSchemaDiverged`, схема цела; только unknown → открытие + WARN; надмножество с pending ниже dbMax → применено | правила множеств | голый goose `Up` |
| db: `MayMigrate=false` + pending → отказ, схема цела | dev не мигрирует | игнорировать политику |
| session: привязка во всех путях создания (fork — workspace форкающего) | заполнение | убрать параметр |
| app: `--session` чужой / legacy из shared-from-linked → типизированный отказ без записей и лок-файла; своя и legacy из home проходят | §1 | убрать проверку |
| app: `--continue` только свои | `GetLast` | старый запрос |
| session: pump исполняет свою строку, чужая остаётся pending, `attempts=0`; `ErrForeignWorkspace` → nack без штрафа | P0 pump | нефильтрованный список / обычный nack |
| agent: wake scheduler на fake clock — чужая due-строка не клеймится, чтений `NextDue` ограничено | P0 вечный цикл | фильтр только в `ClaimDue` |
| agent: `Run` на чужой → `ErrForeignWorkspace` до лока и до assistant-сообщения; арбитр `ForeignWorkspace` → `VDefer` | страховка WS-1 | убрать guard/правило |
| server: rerun чужой → отказ, история цела; delete other не трогает чужие | деструктивные действия | проверка только в агенте |
| cmd: `queue run` берёт/reclaim'ит только свои; локи разных workspace не конфликтуют | P1 queue | старые запросы / общий лок |
| log: два writer'а (две ручки, A создаёт файл) по 1000 строк → 2000 целых строк | затирание | lumberjack |
| log (Windows): файл ≥ 10 MB открыт другой ручкой → новый writer пишет | отключение логирования | lumberjack |
| log: в каждой строке `pid` и `ws`; ровно одна `process start` с `data_dir_source` | атрибуция | убрать атрибуты |

## Не-цели

Перенос/слияние существующих worktree-БД; backfill `workspace_root`; исполнение
чужой сессии из другого checkout и флаг перепривязки (только `sessions fork`);
бейджи и фильтры workspace в web UI (поля в payload — да); изменение 7-дневного
retention `async_jobs`/`session_notices`; межпроцессная ротация логов; колонки cwd
и коммита в `sessions`; env-переключатели.

## Риски

- Окно выкатки: старые долгоживущие процессы в main исполняют чужие durable-строки
  — перезапуск сразу после деплоя.
- Pre-feature бинарь первым в новом worktree создаст `<wt>/.rush/rush.db` →
  worktree навсегда legacy-local (видно по `data_dir_source`; лечится удалением
  `<wt>/.rush`).
- Эвристика dev-сборки: бинарь, собранный вне temp и вне checkout, считается
  задеплоенным и может мигрировать общую БД из worktree.
- Общий проектный конфиг: `--scope workspace` из любого worktree влияет на всех.
- Алиасы путей (subst-диск и т.п.) → разные `workspace_root` → взаимные отказы
  (fail-closed, данные не портятся).
- Осиротевшие durable-строки удалённого worktree навсегда pending (уборка — backlog).
- Нагрузка на одну SQLite: pump tick 3 с в каждом процессе, `busy_timeout` 30 с.
- Retention 7 дней чистит общий ledger из любого процесса — аудит должен успевать.
- `ErrSchemaDiverged` останавливает проект до ручного ремонта (источник — только
  home dev-сборка с миграцией в главном checkout).
- `sessions gc/purge` из любого места чистят весь проект — документировать.

## Backlog (P2)

Межпроцессная ротация `rush.log` (rename под `logs/rush.log.lock`, повторное
открытие при смене identity, `FILE_SHARE_DELETE` на Windows); `rush_logs` по
умолчанию фильтр по своему `ws`; `NextDue` без строк, арендованных другим
процессом; очистка `GIT_*` env в `worktreeRoot`; GC durable-строк исчезнувших
workspace; защита legacy-строк при `--data-dir` на БД другого checkout.
