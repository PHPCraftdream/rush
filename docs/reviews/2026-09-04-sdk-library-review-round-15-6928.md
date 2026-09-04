# Ревью последних коммитов — раунд 15 (6928)

Метка запроса: 2026-09-04, round 15 — 6928.

Проверенный срез основного репозитория:
`62d191ad1127186144f95da8435cda878dc598d1` (`checkpoint: 2026-09-04-0623`).

Предыдущее ревью: `8ed45150bfb9e59a03f862942e7b013a1bff0e1c`,
отчёт `docs/reviews/2026-09-03-sdk-library-review-round-14-0928.md`.

Основной диапазон:
`8ed45150bfb9e59a03f862942e7b013a1bff0e1c..62d191ad1127186144f95da8435cda878dc598d1`.
В нём 19 коммитов, 28 затронутых файлов, 1422 добавленные и 99 удалённых
строк. Диапазон включает интеграцию исправлений R14-1…R14-6, изменения
pre-push/Makefile против Windows commit-limit OOM, шесть test-only
антифлейк-исправлений и checkpoint.

Метод: статическое read-only ревью committed-кода, diff'ов, тестов,
публичной SDK-документации и checkpoint. Основной worktree не изменялся.
Тесты, сборка, model servers, subprocess probes и реальные записи на диск не
запускались из-за запрошенного read-only режима и параллельной работы других
агентов. Указанные в checkpoint три зелёных CI rerun приняты как свидетельство
авторов изменений, а не как независимая проверка этого ревью.

## Итоговый вердикт

Большая часть исправлений раунда 14 реализована аккуратно:

- R14-3 закрыт: публичный mutable `sdk.LibraryVirtualRoot` заменён функцией;
- R14-4 закрыт для текущих путей построения toolset: `agentic_fetch` исчез из
  default ephemeral schema и не возвращается существующим layering;
- R14-5 и R14-6 закрыты корректно;
- R14-1 устраняет конкретный возврат `write`/`bash`/`download` через worker
  layering, но заявленная общая no-real-workspace модель остаётся неполной;
- R14-2 добавляет полезный ранний fail-closed contract, хотя исходная история
  об exploitable absolute-scope bypass была неточной: checkpoint прямо
  фиксирует, что revert-check не воспроизвёл bypass из-за уже существующего
  отказа при canonicalization `WorkingDir`.

Release verdict для ephemeral SDK пока отрицательный. В default tool schema
остались два host-backed инструмента, отсутствующие одновременно и в
`libraryEphemeralDisabledTools`, и в новом финальном floor: `rush_logs`
читает реальный файл относительно cwd процесса, а `git_read` запускает реальный
`git` в OS-интерпретации sentinel root. Кроме того, публично допустимая
реализация `DiskProvider` может вызвать неперехваченный runtime panic из-за
сравнения interface values через `==`.

Открыто: **1 P0, 2 P1, 1 P2**. P0/P1 являются release blockers для заявления
«ephemeral session gets NO real-disk or command-execution tools by default».

## Находки

### R15-1 — P0: `rush_logs` читает host `logs/rush.log` в ephemeral mode

`allToolNames` включает `rush_logs` (`internal/config/config.go:801-824`).
Ephemeral-фильтр его не удаляет (`sdk/library_mode.go:147-192`), а новый
`noRealWorkspaceForbiddenTools` также не содержит его
(`internal/agent/coordinator_tools.go:396-412`). Поэтому top-level ephemeral
coder реально получает `rush_logs` в отправленной provider schema.

Для ephemeral клиента `Options.DataDirectory == ""`. `buildTools` строит путь
как

```go
logFile = filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log")
```

что даёт относительный `logs/rush.log`
(`internal/agent/coordinator_tools.go:558-564`). Затем tool без scope,
`DiskProvider` или sentinel guard выполняет `os.Stat(logFile)` и открывает файл
для чтения (`internal/agent/tools/rush_logs.go:76-112`). Относительный путь
разрешается против реального process cwd. Если host-приложение имеет обычный
`./logs/rush.log`, его последние записи возвращаются модели и уходят внешнему
provider. Встроенная redaction по известным именам полей не превращает чтение
чужого host-файла в допустимую операцию и не гарантирует удаление произвольных
секретов из message text.

Это прямое нарушение публичных обещаний:

- «NO real-disk ... tools by default» (`sdk/README.md:77-91`);
- «custom DiskProvider ... is still the only way an ephemeral session's model
  gets file access at all» (`sdk/README.md:343-362`).

Текущий headline test не замечает дефект: `r6_1DangerousToolNames` вручную
зеркалит тот же неполный список и не содержит `rush_logs`
(`sdk/r6_1_library_mode_diskless_test.go:49-57`, `170-217`). Тест проверяет
actual schema, но классификатор опасных tools у него общий по ошибке с
production allowlist, поэтому это не независимый oracle.

Требуемое исправление:

1. Добавить `RushLogsToolName` в безусловный no-real-workspace floor; одного
   изменения default-disabled списка недостаточно.
2. Не строить реальный относительный log path при пустом `DataDirectory`;
   желательно вообще не создавать tool для этой capability.
3. Добавить SDK regression с marker в host `logs/rush.log`, который проверяет
   отсутствие `rush_logs` в actual schema и невозможность вернуть marker.
4. Перестать вручную поддерживать независимые списки «опасных tools» в
   production и tests: классификация host-disk/command capability должна иметь
   один канонический источник.

### R15-2 — P1: `git_read` остаётся host-process/host-disk tool у top-level и worker

`git_read` также присутствует в общем catalog
(`internal/config/config.go:801-824`) и отсутствует и в ephemeral disabled
list (`sdk/library_mode.go:187-192`), и в финальном
`noRealWorkspaceForbiddenTools` (`internal/agent/coordinator_tools.go:407-412`).
Более того, `resolveReadOnlyTools` сознательно включает его, поэтому он
остаётся не только у top-level coder, но и в базовом toolset task/worker
sub-agent (`internal/config/config.go:846-856`). Исправление R14-1 проверяет
worker schema только по вручную перечисленным `write`/`bash`/`download` и
нескольким legacy/fs names; `git_read` в assertion отсутствует
(`sdk/r14_library_mode_capability_test.go:191-205`).

Это не виртуальный read API. `NewGitReadTool` вызывает
`platform.Command(ctx, "git", args...)`, задаёт `cmd.Dir = workingDir` и запускает
`cmd.Run()` (`internal/agent/tools/git_read.go:191-230`). В ephemeral mode
`workingDir` равен `sdk.LibraryVirtualRoot()`, то есть настоящему
OS-интерпретируемому `/rush-library-mode-root` или
`K:\\rush-library-mode-root`. Если такой путь отсутствует, вызов обычно
завершится ошибкой chdir, но tool всё равно противоречит обещанию «no command
execution». Если sentinel совпал с существующим host Git repository, `status`,
`diff`, `log`, `show` и `blame` читают его реальные данные. Production comments
разрешают такое совпадение и называют его безопасным только потому, что
`rejectRealDiskUnderLibraryVirtualRoot` блокирует `fs_*`; `git_read` через этот
guard не проходит (`internal/agent/tools/fs_library_virtual_root.go:31-40`,
`51-85`).

`git` также нельзя считать безусловно «только чтением» на уровне процесса:
поведение diff/textconv и других helpers может зависеть от host Git config.
Даже без этого чтение repository metadata уже нарушает SDK isolation contract.

Требуемое исправление:

- добавить `GitReadToolName` в безусловный capability-floor и default-disabled
  policy;
- расширить top-level и worker actual-schema tests именно этим именем;
- использовать allow-by-capability модель: при `NoRealWorkspace` оставлять
  только явно классифицированные diskless tools, а не пытаться бесконечно
  дополнять denylist после появления каждого нового built-in tool.

Связанный completeness gap: `agentic_fetch` после `9c82c0c2` безопасно
отсутствует в текущем ephemeral schema через initial disabled list, но его тоже
нет в финальном `noRealWorkspaceForbiddenTools`. Поэтому комментарий о «single
authoritative signal» пока формально неверен: новое или изменённое layering
снова сможет вернуть tool, не встретив capability-floor. Каноническая
классификация должна закрыть и этот разрыв.

### R15-3 — P1: корректный non-comparable `DiskProvider` вызывает runtime panic

Публичный `sdk.DiskProvider` — interface alias. Его contract перечисляет методы,
но нигде не требует, чтобы concrete dynamic value был comparable. Например,
вполне корректный value-provider

```go
type memoryDisk struct {
    files map[string][]byte
}
```

с value-receiver методами может реализовать весь interface, но interface value с
таким concrete struct нельзя сравнивать через `==`/`!=`: Go вызывает runtime
panic `comparing uncomparable type`.

Новый R14 код выполняет именно такие сравнения:

- ранний SDK precondition:
  `overrides.DiskProvider == tools.OSDisk()`
  (`internal/app/app_run.go:660-662`);
- финальный tool floor:
  `disk != tools.OSDisk()`
  (`internal/agent/coordinator_tools.go:440-444`).

До них в `fs_*` path уже существовало третье такое сравнение
(`internal/agent/tools/fs_library_virtual_root.go:72-74`), поэтому проблема
системная, а не локальная. Но R14-2 теперь гарантированно достигает panic ещё до
provider traffic для документированного ephemeral вызова
`FolderScopes + custom DiskProvider`. `sdk.Client.Run` является прямым
pass-through к `App.ExecuteRun`; recovery вокруг этой precondition нет
(`sdk/sdk.go:498-517`). Паника выходит в host-приложение вместо обычного
`(*RunResult, error)` и может уронить процесс.

Текущие тестовые providers не покрывают контракт: они передаются указателями
либо comparable empty structs. `fakeFloorDisk{}` в новом unit test особенно
создаёт ложное ощущение проверки произвольного custom provider
(`internal/agent/r14_no_real_workspace_floor_test.go:20-73`, `89-104`).

Требуемое исправление:

- не сравнивать произвольное interface value с singleton через interface
  equality;
- ввести безопасный internal predicate/marker для canonical OSDisk (например,
  type assertion к закрытому concrete marker type), используемый всеми тремя
  местами;
- добавить value-provider с `map` или `slice` и проверить, что documented
  `FolderScopes + DiskProvider` проходит без panic и получает `fs_*` schema.

### R15-4 — P2: retry в pre-push теряет именно те throttling flags, ради которых сделан fix

`run_test_segment` правильно получает полный argv первого запуска и повторяет
его целиком, если парсер не нашёл failed package
(`.githooks/pre-push:236-250`). Но обычная ветка, где строки `FAIL <package>`
найдены, запускает только

```sh
go test -short -failfast $failed_pkgs
```

(`.githooks/pre-push:247-254`). Все исходные build/test flags теряются.

Для isolated `internal/agent` первый запуск получает `-parallel 2`; retry уже
идёт с default `-parallel`, хотя комментарий называет 172+ `t.Parallel()` sites
главным contributor commit-limit OOM (`.githooks/pre-push:261-266`). Для
остального segment первый запуск получает `-p 2`, retry теряет и его
(`.githooks/pre-push:268-273`). То есть наиболее вероятный путь после реального
flake возвращает ровно ту неограниченную concurrency, которую `627219bf` и
`885989fb` должны были устранить. `run_capped` ограничивает память только на
одной персональной Windows-машине с существующим `safego.ps1`; portable fallback
не компенсирует потерянные Go flags.

Дополнительная неточность: комментарий обещает «`-p 2` everywhere», но первый
isolated agent run получает только `-parallel 2`, без `-p 2`
(`.githooks/pre-push:52-70`, `261-266`). Для одного target package влияние
меньше, но compilation его dependency graph всё равно не соответствует
заявленной политике.

Исправление: сохранять и применять segment flags отдельно от package patterns
при selective retry либо безопаснее повторять исходный segment целиком через
неизменённый `"$@"`. Добавить shell-level regression, который подменяет `go`,
форсирует один `FAIL` и утверждает, что второй argv сохраняет `-parallel 2` или
`-p 2`.

## Проверка полноты исправлений раунда 14

| ID | Статус в `62d191ad` | Оценка |
| --- | --- | --- |
| R14-1 | Частично закрыт | Конкретный worker возврат `write`/`bash`/`download` закрыт; общий floor пропускает `rush_logs`, `git_read`, а также не содержит уже запрещённый initial-фильтром `agentic_fetch`. |
| R14-2 | Runtime hardening реализован | Ранний отказ понятен и fail-closed, но использует unsafe interface equality (R15-3). Исходный absolute-scope P0, согласно checkpoint/revert-check, не воспроизводился и не должен дальше описываться как доказанная эксплуатация. |
| R14-3 | Закрыт | Function export устраняет внешнее присваивание и process-global mutable SDK alias. |
| R14-4 | Закрыт для текущего layering | `agentic_fetch` отсутствует в actual ephemeral schema и scope его не возвращает; для архитектурной полноты имя должно войти и в capability-floor. |
| R14-5 | Закрыт | State-before/after и current-per-OS sentinel tests исправляют ложные host assumptions. |
| R14-6 | Закрыт | Cleanup восстанавливает `slog.Default`, `log.Writer` и `log.Flags`; порядок согласован с существующими исправленными tests. |

## Оценка test/CI серии

Статически корректными выглядят:

- canonicalization `t.TempDir()`/`testEnv` через `filepath.EvalSymlinks` в
  symlink-sensitive tests (`b253bb70`, `c108a2d8`, `678b5103`);
- bounded three-attempt deadline test: последний attempt сохраняет исходные
  assertions и не превращает реальную постоянную регрессию в pass (`4d4e2e05`);
- ожидание завершения async lock metadata cleanup до искусственного backdate
  (`85623b1c`);
- увеличение исключительно верхней границы асинхронного 10-cycle ожидания до
  принятого в соседних tests значения (`10b14a3b`);
- status-agnostic pending/leased/already-dispatched oracle для реального pump с
  immediate initial tick (`9cf16671`).

Эти изменения не ослабляют целевые production-инварианты до тривиального pass.
Основная ошибка серии находится в orchestration retry R15-4, а не в последних
пяти test-local oracle changes.

## Рекомендуемый порядок исправлений

1. Удалить `rush_logs` из ephemeral schema и добавить независимый marker test
   (R15-1).
2. Удалить `git_read` из top-level и worker ephemeral schemas (R15-2).
3. Заменить все сравнения произвольного `DiskProvider` interface на безопасный
   OSDisk predicate и добавить non-comparable provider regression (R15-3).
4. Сделать единую capability-классификацию built-in tools и строить
   no-real-workspace allowlist из неё; включить `agentic_fetch` в final floor.
5. Сохранить `-p`/`-parallel` во всех retry paths pre-push (R15-4).
6. После исправлений выполнить targeted SDK schema/call tests, hook argv test,
   затем несколько полных CI rerun. Зелёный build сам по себе не обнаружит
   R15-1/R15-2, потому что текущий test oracle повторяет production denylist.

## Ограничения проверки

Ревью привязано к точному commit `62d191ad`. На момент финальной проверки
основной worktree содержал чужое незакоммиченное удаление
`web/dist/.gitkeep`; оно не читалось как часть diff, не изменялось и не
стейджилось. Все выводы основаны на статически прослеженных публичных execution
chains. Для R15-1/R15-2 нужны поведенческие regressions до release; для R15-3
достаточен детерминированный unit/SDK test с non-comparable dynamic type; для
R15-4 — изолированный shell argv test без запуска полного suite.
