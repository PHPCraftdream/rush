# Ревью коммитов, чанк 3 из 5 (`1d3cdf4a7..7b3c935ef`)

43 коммита, 2026-09-03 16:52 — 2026-09-04 16:04 CEST. Один из пяти
непересекающихся срезов 7-дневного окна `da53fe42a..f2914d53c`.

Режим только чтения: `go build`/`go test`/`go vet`/hooks/pnpm не
запускались. Единственный запуск — standalone Go-программа во временном
каталоге **вне модуля** (`C:\Users\Computer\AppData\Local\Temp\evalsym-probe`),
чтобы проверить чисто языковой вопрос: что `filepath.EvalSymlinks`
возвращает на Windows для rooted-but-driveless пути (см. C3-6).

Текущий tip worktree — `f2914d53c`; все проверки «держится ли это
сегодня» сделаны против него, а не против конца диапазона.

---

## Границы диапазона и что уже отревьюено

Важное уточнение к постановке задачи: **`0e4b9ed3` НЕ входит в мой
диапазон**. `git log --format='%h %p' -1 0e4b9ed3` даёт
`0e4b9ed38 7b3c935ef` — то есть `0e4b9ed3` является потомком
`7b3c935ef`, а мой диапазон заканчивается на `7b3c935ef`. Поэтому:

| Подмножество | Коммитов | Статус |
|---|---|---|
| `1d3cdf4a7..239c89437` | 18 | **свежее ревью, ниже разделы 1-6** |
| `239c89437..7b3c935ef` | 25 | покрыто `docs/reviews/2026-09-05-2047-commit-review-36h.md` (его диапазон `239c8943..0e4b9ed3` = 26 коммитов, из них 25 внутри моего) — цитируется, раздел 7 |

Заявленный в задании «хвост после `0e4b9ed3` до `7b3c935ef`, database
pool lifecycle» отдельным необревьюенным куском не существует:
`7b3c935e` («make pooled database lifecycle generation safe») —
последний коммит моего диапазона и он **уже** разобран в 36h-ревью
(раздел 8). Я его перепроверил самостоятельно (раздел 7.2), но это
именно спот-чек, а не первичное ревью.

---

## Сводка свежих коммитов (`1d3cdf4a7..239c89437`)

Хронологически (снизу вверх в `git log`):

| Hash | Тема | Вердикт |
|---|---|---|
| `9c82c0c2` | R14-4: `agentic_fetch` в `libraryEphemeralDisabledTools` | корректно; test-mirror рассинхронизирован (C3-5) |
| `980462a3` | Merge r14-4 | корректно |
| `ae09c3b7` | R14-5: host-topology assumptions → state comparison | корректно; оракул поверхностный на collision-host (C3-7) |
| `deb34af6` | Merge r14-5 | корректно |
| `61cc0773` | R14-6: restore `log.Writer()`/`log.Flags()` | корректно |
| `e8ff00bd` | Merge r14-6 | корректно |
| `0b5bd37c` | Merge sdk-review-r14-integration | no-op merge, ломает `--first-parent` историю (C3-12) |
| `627219bf` | pre-push: `run_capped` + сегментация тестов | **вносит P2 (C3-1)**, плюс C3-2/C3-3/C3-4 |
| `b253bb70` | canonicalize `t.TempDir()` в 3 symlink-тестах | корректно |
| `885989fb` | cap остальных `go build`/`go test`/`go run` | корректно; timeout-риск C3-3 |
| `c108a2d8` | canonicalize `t.TempDir()` в 12 fake-disk/rebuild-тестах | корректно |
| `678b5103` | canonicalize `testEnv`/`absWorkingDir` | корректно; Windows-атрибуция не подтверждается (C3-6) |
| `4d4e2e05` | retry для `TestFileMatchesHonoursDeadlineMidHugeLine` | корректно; комментарий расходится с кодом (C3-8) |
| `85623b1c` | барьер перед back-dating в recovery-тесте | корректно; остаточное Windows-окно (C3-9) |
| `10b14a3b` | таймауты `TestReleaseGate_P0_2_*` 5s → 20s | корректно; не ловит pace-регрессию (C3-10) |
| `9cf16671` | status-agnostic проверка для initial-tick гонки pump'а | корректно |
| `62d191ad` | checkpoint | docs |
| `239c8943` | docs: review round 15 | пропустил C3-1; мелкая фактическая ошибка (C3-11) |

Из 18 свежих коммитов 14 имеют содержательное тело commit message с
описанием механизма — заметно лучше, чем в покрытой части диапазона
(там 23 из 26 пустые, CR-9). Шесть из них явно фиксируют
revert-check / call-site audit; это проверяемые утверждения, и там, где
я их проверял, они подтвердились.

---

## 1. Ephemeral tool-surface: R14-4 (`9c82c0c2`, `980462a3`)

**Статус: корректно.**

Механизм подтверждён по коду, а не по commit message:
`internal/agent/agentic_fetch_tool.go:100` действительно делает
`os.MkdirTemp(c.cfg.Config().Options.DataDirectory, "rush-fetch-*")`, и
для ephemeral-сессии `DataDirectory == ""`, что по контракту
`os.MkdirTemp` означает fallback на реальный OS temp. Утверждение
коммита про отсутствие re-add-путей тоже проверено:
`internal/agent/coordinator_tools.go:174` — `workerToolNames` содержит
`"fetch"`, но не `agentic_fetch`; `:309-318` —
`folderScopeEscapeHatchTools` содержит `tools.AgenticFetchToolName`,
а `applyCallFolderScope` (`:368`) его вырезает, а не добавляет.

Тест `sdk/r14_4_agentic_fetch_ephemeral_test.go` невакуумный и хорошо
устроен: перенаправляет `TMP`/`TEMP`/`TMPDIR` в контролируемый каталог
и сканирует его **изнутри provider-handler'а**, то есть в момент, когда
живой `agentic_fetch` сидел бы между `MkdirTemp` и своим
`defer os.RemoveAll` — транзиентное создание было бы поймано. Есть
positive control (`require.Contains(t, names, "agent")`), так что пустой
toolset не проходит проверку вакуумно. `os.TempDir()` в Go не кэширует
результат ни на Windows, ни на Unix, поэтому `t.Setenv` работает.

### C3-5 (P3): test-mirror `r6_1DangerousToolNames` разошёлся с продовым списком

`sdk/r6_1_library_mode_diskless_test.go:49-57` объявляет, что
`r6_1DangerousToolNames` «mirrors `libraryEphemeralDisabledTools`
(sdk/library_mode.go)». `9c82c0c2` добавил
`tools.AgenticFetchToolName` в продовый список
(`sdk/library_mode.go:209`), но не в зеркало. Проверено на текущем tip
`f2914d53c`: продовый список — `bash, run_command, download,
agentic_fetch, rush_logs, git_read, edit, …`; зеркало —
`bash, run_command, download, rush_logs, git_read, edit, …`.
`rush_logs` и `git_read` позже добавили в **оба** списка
(`abed8946`/`31ea604f`), `agentic_fetch` — только в один.

Конкретное последствие: headline-тест
`TestSDKLibraryModeEphemeralDefaultToolsetExcludesRealDiskAndCommandTools`
(`:207-210` — цикл по зеркалу) **не упадёт**, если кто-то уберёт
`agentic_fetch` из `libraryEphemeralDisabledTools`. Регрессию поймает
только выделенный `sdk/r14_4_agentic_fetch_ephemeral_test.go`.
Не дыра сегодня, но зеркало со своим комментарием сейчас врёт.

Замечание по цепочке: на момент `9c82c0c2` `agentic_fetch` защищался
только исходным списком плюс отсутствием в двух positive-списках —
ровно то, что round-15 отметил как R14-7 и что закрыл `31ea604f`,
добавив его в `noRealWorkspaceForbiddenTools`
(`internal/agent/coordinator_tools.go:428`, проверено на tip). Так что
финальное состояние правильное, а `9c82c0c2` — промежуточный шаг.

---

## 2. Host-topology assumptions: R14-5 (`ae09c3b7`, `deb34af6`)

**Статус: корректно, но новый оракул мельче старого на одном классе хостов.**

Замена «sentinel root не существует» на before/after state comparison
обоснована: `rejectRealDiskUnderLibraryVirtualRoot` действительно
отказывает до первого `disk.Stat` независимо от коллизии, так что
старый assert кодировал топологию хоста, а не свойство продакшена.
Перенос `r5_6_virtual_root_regression_test.go` в generic-формулировку и
добавление `internal/permission/r14_5_current_virtual_root_regression_test.go`
во внешнем пакете `permission_test` (обход цикла импорта
`tools` → `permission`) — тоже корректно; `BuildFolderScope`/`Check`
чисто строковые, реальный `K:` не нужен.

### C3-7 (P3): снимок покрывает только корень сентинела

`sdk/r6_1_library_mode_diskless_test.go:123-152` (`r6_1PathSnapshot` /
`r6_1SnapshotPath`) записывает `statErr`, `mode`, `size`, `modTime`
корня и **только непосредственные** имена детей. Сценарий, при котором
новый оракул молча пропускает реальную запись под сентинелом: хост, на
котором `K:\rush-library-mode-root` (или `/rush-library-mode-root`)
существует **и** уже содержит подкаталог `scoped`. Реальная запись в
`<sentinel>/scoped/f.txt` не меняет ни mtime корня (меняется mtime
`scoped`), ни список непосредственных детей → `r6_1RequirePathUnchanged`
проходит. На хосте без коллизии (все CI-runner'ы) оракул сильный:
`statErr` перещёлкивается с «not exist» на nil.

Зеркальный побочный эффект того же дизайна: на collision-хосте снимок
сравнивает mtime **живого** каталога, так что любой посторонний писатель
под сентинелом во время теста даёт ложный fail. Это ровно тот класс
host-зависимости, который коммит и устранял, просто в другой форме.

Дешёвое усиление: рекурсивный walk (путь+размер+mtime) вместо одного
уровня, либо, если корень существовал до теста, сравнивать только
поддерево, а не корень.

---

## 3. `log` package leak: R14-6 (`61cc0773`, `e8ff00bd`)

**Статус: корректно.**

Механизм реален и подтверждается семантикой stdlib: `slog.SetDefault`
перенаправляет `log` только когда handler нового логгера **не**
`*defaultHandler`; при восстановлении `slog.SetDefault(prev)` с
`defaultHandler`-логгером ветка перенаправления не выполняется, поэтому
`log.SetOutput` остаётся указывать на мёртвый `bytes.Buffer` теста до
конца процесса. Порядок восстановления в cleanup
(`internal/agent/cliprovider/provider_security_test.go:24-29`) верен в
обе стороны: явные `log.SetOutput`/`log.SetFlags` идут после
`slog.SetDefault`, поэтому даже если `prev` был не-default handler'ом
(и `SetDefault` сам бы переустановил вывод), финальное состояние
совпадает с исходным.

---

## 4. Pre-push hook и Makefile (`627219bf`, `885989fb`)

Самая проблемная пара свежего диапазона: это push-gate, и три из
четырёх находок здесь про то, что gate тише, чем выглядит.

### C3-1 (P2): retry-ветка `run_test_segment` теряет throttling-флаги — ровно те, ради которых коммит и написан

`.githooks/pre-push` в редакции `627219bf`, функция `run_test_segment`:
первая попытка и «не удалось распарсить упавший пакет»-retry вызывают
`go test -short -failfast "$@"` (то есть с `-parallel 2` / `-p 2`,
которые пришли в `"$@"`), а identified-package retry —
`go test -short -failfast $failed_pkgs`, **без** флагов.

Конкретный сценарий отказа: сегмент
`run_test_segment "internal/agent (isolated, throttled)" 2g 300 -parallel 2 ./internal/agent/`
падает один раз (в hook'е это ожидаемый и явно задокументированный
сценарий — «known rare Windows flakiness»), парсер вытаскивает
`FAIL github.com/PHPCraftdream/rush/internal/agent`, и retry запускает
этот пакет с **полным дефолтным `-parallel` (GOMAXPROCS)** — то есть
именно ту конфигурацию 172+ `t.Parallel()`-сайтов, которую сам же
коммит называет «the single biggest commit-limit contributor found this
session». Retry-путь воспроизводит `ERROR_COMMITMENT_LIMIT`, ради
предотвращения которого написан весь коммит, причём под уже поднятым
memory pressure.

**Статус: закрыт** в `0cd9dd3f` (R15-4, покрытая часть диапазона):
сигнатура стала `<label> <size> <timeout> <flags> <pkgs...>`, `$flags`
подставляется во все три invocation, и добавлен self-test
`.githooks/check_run_test_segment_retry_flags.sh`, проверяющий argv обеих
retry-веток. На tip `f2914d53c` подтверждено:
`.githooks/pre-push:248` (`local … flags="$4"`), `:254`, `:263`, `:267`.

Отдельно: round-15 ревью (`239c8943`) оценило `627219bf` как
«корректно» и этой находки не заметило — см. C3-11.

### C3-2 (P3): `run_test_segment` течёт mktemp-файлами на любом падении

`627219bf` удалил `trap 'rm -f "$test_log"' EXIT` и заменил его на
`rm -f "$test_log"` в конце функции. Но все три ветки внутри вызывают
`fail`, а `fail` делает `exit 1` — то есть при **любом** реально красном
сегменте временный лог остаётся в системном temp. Проверено на tip:
`grep -n "trap" .githooks/pre-push` не даёт ни одного совпадения,
`rm -f "$test_log"` живёт на `:271`, ниже всех `fail`-путей. Ущерб —
мусор в temp на каждый неудачный push; чинится одной строкой
`trap` внутри функции или `rm -f` перед каждым `fail`.

### C3-3 (P3): фиксированные wall-clock таймауты `run_capped` дают ложные red-фейлы на холодном кэше

`run_capped <size> <timeout> …` на Windows делегирует в
`C:\Users\Computer\AppData\Local\safego\safego.ps1`. Я не запускал этот
скрипт, но прочитал его: на timeout он делает `exit 124`, иначе
`exit ([int]$exitCode)` — то есть код возврата пробрасывается корректно
и `|| fail` срабатывает. Значит таймаут = **hard fail push'а**, а не
тихий пропуск. Это хорошо (fail-closed), но делает величины таймаутов
частью гейта:

- `run_capped 3g 300 go run -p 2 "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13" run`
  (`885989fb`, `.githooks/pre-push:116`) — на холодном module cache это
  скачивание + компиляция всего дерева golangci-lint **при `-p 2`**;
  5 минут здесь недостаточно с большим запасом. Ветка срабатывает
  только когда бинаря golangci-lint нет в PATH — то есть ровно у нового
  контрибьютора/на свежей машине.
- `run_capped 3g 240 go build -p 2 ./...` (`:85`) — холодный полный
  билд репозитория при половинной параллельности за 4 минуты тоже не
  гарантирован.

Верификация в commit message («full hook run end to end … exit 0»)
проводилась на прогретых кэшах, где эти величины с запасом проходят;
холодный путь не покрыт. Смягчает риск то, что `safego.ps1` есть только
на машине автора (`ls` подтвердил: 14 028 байт, 2026-09-03), а везде
ещё `run_capped` выполняет команду голой, без таймаута вообще.

Это же и оборотная сторона всей конструкции: **самый сильный гейт
репозитория (полный test suite) на машине автора проходит через
некоммиченный, неревьюируемый скрипт**, а у всех остальных — нет. Две
разные семантики одного и того же hook'а.

### C3-4 (P3): пустой вывод `go list` превращает второй сегмент в почти пустой прогон

`.githooks/pre-push:297`:

```sh
run_test_segment "./... (minus internal/agent)" 3g 480 "-p 2" \
  $(go list ./... | grep -v '^github.com/PHPCraftdream/rush/internal/agent$')
```

Подстановка не квотирована и её код возврата не проверяется (`set -e`
не установлен — только `set -uo pipefail`). Если `go list ./...` по
любой причине не отдаст пакетов, `run_test_segment` получит ноль
пакетных аргументов, `go test -short -failfast -p 2` протестирует
**пакет в текущем каталоге** (корень репозитория) и hook напечатает
`(tests passed on the first attempt: ./... (minus internal/agent))`.
Push пройдёт, фактически не прогнав ничего. Окно узкое (шагом раньше
`go build ./...` обычно упадёт первым и вызовет `fail`), но проверка
`[ -n "$pkgs" ]` стоит одну строку.

### Makefile

`885989fb` добавил `-p 2` в `go build -o rush .` (`Makefile:8`). Это
машинно-специфичная митигация Windows commit limit, применённая ко
**всем** контрибьюторам на всех ОС; на многоядерной Linux-машине это
чистая потеря времени сборки. Комментарий это честно называет
(«proven trigger on this machine»), так что это осознанный trade-off, не
баг — но зафиксирую как решение, а не как данность.

---

## 5. Канонизация temp-путей в тестах (`b253bb70`, `c108a2d8`, `678b5103`)

**Статус: корректно, единый паттерн, 17 тестов + 2 общих хелпера.**

Класс бага реален: продовые пути (`resolveScopedPath`,
`tools.CanonicalizeFolderScopeSpec`, реальный `fs_list`) прогоняют путь
через `EvalSymlinks`, а тесты строили `permission.FolderScope` из сырого
`t.TempDir()`. На runner'е, где temp-корень сам под symlink/junction,
две стороны сравнения оказываются в разных namespace'ах и всё
запрещается. `filepath.EvalSymlinks` идемпотентен на уже
канонизированном пути, так что фикс безопасен для уже-чистых хостов.

`c108a2d8` явно отклонил альтернативу «починить общие хелперы
`fsBatchTestScope`/`fsWriteTestScope`» с верным обоснованием: хелпер
резолвит только сторону scope, а item-пути каждый тест строит сам из
своего сырого `workingDir` — правка хелпера закрыла бы половину
рассинхронизации.

### C3-6 (P3): Windows-часть атрибуции `678b5103` не подтверждается кодом

Commit message утверждает, что канонизация `testEnv`'s `workingDir`
объясняет живой CI-фейл `TestBuildTools_SubAgentInheritsCallerDiskProvider`
на **обоих** runner'ах — `build/windows-latest` и `test-agent/macos-latest`.

`internal/agent/common_test.go:73` строит
`workingDir := filepath.Join("/tmp/rush-test/", t.Name())`. На Windows
это **drive-less rooted** путь. Проверил standalone-программой вне
модуля на этой Windows-машине:

```
joined      = "\\tmp\\rush-test\\TestProbe_EvalSymlinks"  IsAbs=false
evalsym     = "\\tmp\\rush-test\\TestProbe_EvalSymlinks" err=<nil> IsAbs=false
abs         = "C:\\tmp\\rush-test\\TestProbe_EvalSymlinks"
abs+evalsym = "C:\\tmp\\rush-test\\TestProbe_EvalSymlinks" err=<nil>
```

То есть `filepath.EvalSymlinks` на Windows не добавляет том к
driveless-пути и на обычном (не junction'нутом) каталоге возвращает вход
без изменений — новая строка `common_test.go:87-89` там тождественна.
Вторая половина коммита (`absWorkingDir`,
`internal/agent/coordinator_disk_provider_test.go:186-192`) добавила
`EvalSymlinks` **после** уже существовавшего `filepath.Abs`, то есть на
том же обычном Windows-хосте она тоже сводится к нормализации регистра.

Вывод: macOS-часть механизма (`/tmp` → `/private/tmp`) реальна и
объясняет `test-agent/macos-latest`. Windows-часть объясняется только
если у runner'а `\tmp` — настоящий junction (тогда `EvalSymlinks`
перейдёт на цель и том появится); из коммита это не проверено и не
заявлено. Практический риск: windows-latest фейл мог просто не
повториться, и его настоящая причина остаётся неизвестной. Не блокирует —
изменение безвредно и правильно для macOS.

Побочное наблюдение (не находка): после `678b5103` `testEnv` жёстко
падает, если `EvalSymlinks` вернёт ошибку. `MkdirAll` выполняется
строкой выше, так что окно узкое; на всех платформах, где я это
проверял, ошибки не возникает.

---

## 6. Стабилизация тестов (`4d4e2e05`, `85623b1c`, `10b14a3b`, `9cf16671`)

Четыре фикса флейков. Все четыре — test-only, продакшен не тронут; в
`85623b1c` отказ от продового фикса явно обоснован (расширение
`releaseMetadataCleanupBound` вернуло бы hang-риск #337). Это
правильный scope.

### `4d4e2e05` — bounded retry для `TestFileMatchesHonoursDeadlineMidHugeLine`

Оракул («deadline соблюдён и вторая читка быстрее полной») по природе
сравнивает два живых замера, а не константу; ретрай не может замаскировать
реальную регрессию, потому что при неработающем `ctx.Err()` все три
попытки дают `err == nil`. Revert-check в commit message описывает
именно этот эксперимент. Импорт `errors` в файле уже был (`:11`).

**C3-8 (P3):** doc-комментарий хелпера
(`internal/agent/tools/grep_fallback_test.go`, шапка
`fileMatchesHonoursDeadlineMidHugeLineAttempt`) утверждает: «Returns
false only for the specific timing-noise shapes described above … for
any non-timing failure (I/O error, no return within 10s), it fails the
test immediately via require/t.Fatal instead of returning false». Код
такой дискриминации не делает: предикат —
`invariantHeld := err != nil && errors.Is(err, context.DeadlineExceeded) && elapsed < fullElapsed`,
и `if !invariantHeld && attempt < attempts { return false }` глотает
любую не-DeadlineExceeded ошибку (в т.ч. I/O) на попытках 1-2.
Поведенчески это безопасно — детерминированная ошибка повторится и
провалит третью попытку, — но комментарий описывает механизм, которого
нет. (Про `t.Fatal` при 10s комментарий верен: там ветка `time.After`,
`Fatal` вызывается сразу.)

### `85623b1c` — барьер перед back-dating

Механизм проверен по коду независимо, а не принят на веру.
`readLockFile` (`internal/session/lock.go:1086-1093`) читает **сначала
PID-sidecar, и только при ошибке — основной файл**;
`clearHolderMetadata` (`:737-750`) делает `Truncate(0)` → `Seek` →
`Sync()` → `os.Remove(pidSidecar)` → `os.Remove(genSidecar)`. Значит
`ReadLockPID(lockPath) == 0` действительно достижимо только когда
случились **обе** операции — утверждение комментария верно.
`InspectSessionLock` (`:979-1017`) считает `Live` из `st.ModTime()` и
PID-fallback; `.gen`-sidecar в это не входит.

**C3-9 (P3, остаточное, Windows-only):** на момент, когда барьер
отпускает тест, у cleanup-goroutine ещё не выполнены
`os.Remove(generationSidecarPath)` и отложенный `f.Close()`. На Linux
ни то, ни другое не трогает mtime основного файла, и наблюдённый фейл
был как раз на ubuntu. На Windows закрытие handle может опубликовать
отложенное обновление last-write-time уже **после** `os.Chtimes` теста,
что вернуло бы `Live=true`. Окно узкое, потому что `f.Sync()`
(`FlushFileBuffers`) выполняется до обоих `Remove`, то есть штамп
обычно уже сброшен. Если этот тест когда-нибудь мигнёт на
windows-latest — искать здесь, а не в новом барьере.

### `10b14a3b` — таймауты 5s → 20s

Обоснование корректное: guarded-регрессия (double-increment) плато'ит
`coord.calls` на 5 при `RunQueueMaxAttempts == 10`, поэтому `>= 10`
не станет истинным ни за 5, ни за 20 секунд. Значение приведено к уже
существующей в файле конвенции для 25-цикловых siblings.

**C3-10 (P3):** расширение окна ослабляет тест против **другого**
класса регрессии — замедления цикла. Изменение, которое сделает каждый
retry-цикл вчетверо медленнее (например, лишний fsync или backoff), с
20-секундным окном пройдёт молча, тогда как с 5-секундным упало бы.
Per-cycle-ассерта (например, «10 циклов уложились в N секунд») не
добавлено. Не блокирует — pace здесь не является тестируемым
инвариантом, — но это цена, которую commit message не называет.

### `9cf16671` — status-agnostic проверка гонки с initial tick

Корректно и не вакуумно. Механизм подтверждён: `App.New` стартует
реальный `RunQueuePump`, его `run()` даёт немедленный тик до входа в
3s-цикл, а leased-строка невидима для pending-only скана. Новый
предикат `outstanding || providerCalls.Load() >= 1` не может пройти
вырожденно: `HasOutstandingRunQueueEntryForSession`
(`internal/db/sql/run_queue.sql:132-140`) проверяет `pending` OR
`leased`, а `providerCalls` в этом тесте инкрементирует только
httptest-handler, и до `EnqueueRunQueueEntry` провайдера дёрнуть некому
(генерация заголовка исключена: сессия имеет и не-дефолтный заголовок,
и seed-сообщение — это отдельно зафиксировано в комментарии теста).
Если бы `EnqueueRunQueueEntry` молча ничего не записал, оба терма
остались бы ложными.

---

## 6.5. Docs и merge-гигиена (`62d191ad`, `239c8943`, `0b5bd37c`)

**C3-11 (P3):** `docs/reviews/2026-09-04-round-15-0628.md:96` пишет
«macOS `/tmp` → `/private/var`». На macOS `/tmp` → `/private/tmp`;
`/private/var` — это цель `/var`. Сами коммиты
(`b253bb70`/`c108a2d8`/`678b5103`) написаны правильно, ошибка только в
review doc. Там же (`:111-114`) `627219bf` оценён как «корректно» —
находка C3-1 пропущена; её же поймало следующее ревью как R15-4 сутками
позже. Это не претензия к автору ревью, а полезная калибровка: hook'и
в этом репозитории читаются менее внимательно, чем Go-код.

**C3-12 (P3, историческое):** `0b5bd37c` — no-op merge. Его родители —
`8ed45150b` и `e8ff00bd5`, причём первый является предком второго;
`git diff e8ff00bd5 0b5bd37c8` пуст. Побочный эффект: первым родителем
стал **до**-R14 коммит, поэтому
`git log --first-parent 8ed45150b..627219bf5` показывает всего два
коммита — вся R14-интеграция (`1d3cdf4a7`, три под-мержа и четыре
fix-коммита) уходит с first-parent линии. Для инструментов и людей,
читающих историю через `--first-parent` (частая практика для
release notes), R14 выглядит как «ничего не произошло». История уже
опубликована и не переписывается; фиксирую как контекст на будущее.

`62d191ad` — только `docs/checkpoints/2026-09-04-0623.md`, кода нет.

---

## 7. Покрытая часть диапазона (`239c8943..7b3c935e`): что цитирую, что перепроверил

Ниже — **цитаты** из `docs/reviews/2026-09-05-2047-commit-review-36h.md`
(CR-1..CR-12) плюс мои собственные спот-чеки против текущего tip
`f2914d53c`. Явно разделяю.

### 7.1. Что я проверил сам (и результат)

| Находка 36h-ревью | Что проверял | Результат на `f2914d53c` |
|---|---|---|
| CR-1 (P0, `01e909d0`): `defer cancel()` в `Owner.Initialize` убивает transport'ы MCP | читал `internal/agent/tools/mcp/init.go` | `defer cancel()` **на месте** (`init.go:2026`), но появился механизм передачи: `sessionContext.promote()` (`:4687-4712`) снимает candidate-cancel и оставляет сессию на `owner`-контексте, `promoteContext()` вызывается во всех точках публикации (`:1717`, `:2409`, `:3037`, `:3452`). Форма закрытия соответствует заявленной («session-context promotion») |
| CR-5 (P3): ложная модель угрозы в комментариях `fs_provider_os.go` | читал файл | **Исправлено**: `internal/agent/tools/fs_provider_os.go:49-54` теперь прямо пишет «It is not a workaround for a reachable panic in the former comparisons». `osDisk struct{}` (`:18`) — неэкспортируемый и comparable, так что дисконфирмация R15-3/R16-6 подтверждается и структурно |
| CR-8 (P3, помечен «Открыт»): brokers закрываются до `RunQueuePump.Stop()` | читал `internal/app/app_lifecycle.go` | **Больше не держится.** Порядок обратный: `RunQueuePump.Stop()` на `:165-171`, broker-shutdown'ы на `:178-194`, с явным комментарием `:155-164`/`:172-177` о том, почему pump идёт первым. CR-8 следует считать закрытым |
| CR-12 (P3, помечен «Открыт»): пустой output в `notifyBackgroundJobDone` для job'ов, detached до завершения | читал `internal/shell/background.go` | **Больше не держится.** `armBufferReleaseTimer` (`:755-772`) для detached job'а проверяет `bs.onDoneCount.Load() == 0` и, если callback зарегистрирован, вместо немедленного release ставит `armDetachedReleaseTimerLocked` (`:797-825`) с bounded grace. CR-12 следует считать закрытым |
| CR-10 (P3): `trap` до присваивания `count_file` | читал `.githooks/check_run_test_segment_retry_flags.sh` | Закрыто: `cleanup()` (`:45-57`) проверяет `[ -n "$var" ]` для каждой из трёх переменных, `trap cleanup EXIT` (`:58`) безопасен при любом частичном `mktemp` failure |
| CR-11 (P3): мёртвые `goose.SetBaseFS`/`SetLogger` в `init()` | grep по `internal/db/connect.go` | Закрыто: ни одного совпадения |

### 7.2. Спот-чек `7b3c935e` (граничный коммит моего диапазона)

36h-ревью помечает его «корректно»; перепроверил самостоятельно
ключевые инварианты:

- Generation-safety работает как заявлено: `ReleaseConn` ищет entry по
  handle под `poolMu`, отпускает `poolMu`, и `releaseEntry(absPath, entry)`
  **повторно** проверяет `!ok || (expected != nil && entry != expected)`
  под `poolMu` (`internal/db/connect.go` в редакции `7b3c935e`,
  `:358-374`). Поздний release хендла умершего поколения — no-op.
  ABA невозможен: пока вызывающий держит `*sql.DB`, GC не переиспользует
  адрес.
- Дисциплина блокировок в `releaseEntryLocked` (`:376-397`) корректна:
  функция вызывается с уже взятым `poolMu` и отпускает его на **обоих**
  выходах (`refCount > 0` и `delete`+close); `pathMu` снимается
  `defer`'ом в `releaseEntry`. Паттерн «unlock в callee» хрупкий, но
  сейчас верный.
- Полнота: прогнал grep по всем не-тестовым `conn.Close()` на
  `7b3c935e`. Из совпадений `internal/server/*` — websocket-соединения,
  `internal/db/connect.go:261,266` — error-path открытия (ещё не в
  пуле), `sdk/sdk.go:943` — `closeConns`, которые заполняются только в
  ephemeral in-memory ветке (`sdk/library_mode.go:279-282`; в
  file-backed ветке `closeConns == nil`). То есть `internal/cmd/stats.go:128`
  был последним местом, где pooled handle закрывался мимо refcount, и
  коммит его действительно закрыл.
- Совместимость: `app.dbConns` против retained `dbReleasesNeeded`
  (`internal/app/app_lifecycle.go:283-299`) — fallback берётся только
  когда `len(app.dbConns) == 0`, то есть для `App{}`, собранных
  вручную в тестах. Двойного release нет: `app.New` кладёт в `dbConns`
  и caller's `conn`, и `readConn`, а все error-path'ы
  (`internal/cmd/root.go:385,447`, `sdk/sdk.go:474`,
  `sdk/library_mode.go:330`) освобождают только свой `conn`, потому что
  `app.New` на своих error-path'ах уже освободил `readConn`.

### 7.3. Что цитирую без независимой перепроверки

Всё остальное из 36h-ревью принимаю на веру и **не** перевыводил:
CR-2 (renewal-сессии на `operationCtx`), CR-3 (`WaitForInit` на
pre-Close `initDone`), CR-4 (convoy на `serverLease.RLock`), CR-6
(изоляция background job'ов по сессии), CR-7 (противоречие
`sdk.go`/`library_mode.go` про «no off mode»), CR-9 (пустые тела
коммитов), а также его вердикты «корректно» по `abed8946`, `31ea604f`,
`0cd9dd3f`, `a7f657b2`, `75ccf190`, `07cd3184`, `ee09c205`, `93a616b0`,
`0b25848c`, `5f2dbebc`, `5434ea14`, `0563e181`, `f362c996`, `60c81f78`,
`b995d135`, `6805f351`, `3cd1c724`, `0d9f3a93`, `963d584a`, `18cf22f7`.
Из этого списка я самостоятельно подтвердил только те пункты, которые
перечислены в 7.1 и 7.2.

Отдельно: 36h-ревью помечает CR-8 и CR-12 как «Открыт, note» — по моим
спот-чекам оба на текущем tip уже не воспроизводятся (см. 7.1). Это
единственное расхождение с его итоговой таблицей.

---

## Утечки памяти и горутин (свежая часть)

**Новых unbounded утечек: 0.** Свежие 18 коммитов не добавляют ни одной
горутины в продовый код — 14 из 18 трогают только `_test.go`,
`.githooks/`, `Makefile` и `docs/`.

Что проверено точечно:

- `9c82c0c2` — единственный коммит, меняющий продовое поведение
  (`sdk/library_mode.go`), и он только удаляет имя из списка; новых
  ресурсов не создаёт. Наоборот, закрывает канал создания
  `rush-fetch-*` каталогов в реальном OS temp.
- `4d4e2e05` — ретрай создаёт до 3 × 16 MiB файлов через отдельные
  `t.TempDir()`; все убираются `t.Cleanup`. Горутина `go func(){ done <- … }()`
  в attempt'е не joined'ится при уходе по `time.After(10s)`, но там
  `t.Fatal`, то есть тест уже завершается. Пре-существующая форма, не
  изменена коммитом.
- `85623b1c`/`10b14a3b`/`9cf16671` — только `require.Eventually`
  и смена предиката; ничего не создают.
- `C3-2` — не утечка памяти, а утечка файлов в temp на неудачном push'е.

---

## Открытые findings

| ID | Severity | Коммит | Описание | Статус |
|---|---|---|---|---|
| C3-1 | **P2** | `627219bf` | identified-package retry в `run_test_segment` вызывал `go test` без `-parallel 2`/`-p 2` — ретрай воспроизводил именно тот Windows commit-limit OOM, ради которого коммит написан | **Закрыт в `0cd9dd3f` (R15-4)**, подтверждено на tip (`.githooks/pre-push:248,254,263,267`) |
| C3-2 | P3 | `627219bf` | удалён `trap … EXIT`; `rm -f "$test_log"` (`.githooks/pre-push:271`) недостижим на всех `fail`-путях → mktemp-файл течёт на каждом красном сегменте | Открыт |
| C3-3 | P3 | `627219bf`, `885989fb` | фиксированные таймауты `run_capped` (240s build, 300s `go run golangci-lint`) верифицированы только на прогретом кэше; `safego.ps1` возвращает 124 → `fail`, то есть холодный кэш = ложный red push-gate. Плюс: сильнейший гейт проходит через некоммиченный скрипт только у автора | Открыт |
| C3-4 | P3 | `627219bf` | `$(go list ./... \| grep -v …)` не квотирован и не проверяется; пустой вывод → `go test` только корневого пакета с сообщением «tests passed» | Открыт |
| C3-5 | P3 | `9c82c0c2` | `r6_1DangerousToolNames` (`sdk/r6_1_library_mode_diskless_test.go:52-57`) не получил `agentic_fetch`, хотя его комментарий `:49-51` заявляет зеркалирование `libraryEphemeralDisabledTools` (`sdk/library_mode.go:209`). Headline-тест не поймает удаление записи из продового списка | Открыт (проверено на tip) |
| C3-6 | P3 | `678b5103` | Windows-часть атрибуции не подтверждается: `testEnv`'s путь driveless, `EvalSymlinks` на нём — тождество (проверено standalone-программой). macOS-часть корректна | Открыт (docs/attribution) |
| C3-7 | P3 | `ae09c3b7` | `r6_1SnapshotPath` снимает только корень сентинела и его непосредственных детей: на collision-хосте запись в `<sentinel>/<существующий подкаталог>/f.txt` не детектится | Открыт |
| C3-8 | P3 | `4d4e2e05` | doc-комментарий обещает пропускать в retry только timing-noise; код возвращает false на любой `!invariantHeld`, включая I/O-ошибку | Открыт (docs) |
| C3-9 | P3 | `85623b1c` | остаточное Windows-окно: после барьера у cleanup-горутины ещё висят `os.Remove(genSidecar)` и отложенный `f.Close()`, который на Windows может опубликовать mtime после `os.Chtimes` | Открыт (узкое, наблюдаемого фейла нет) |
| C3-10 | P3 | `10b14a3b` | окно 20s не ловит 4× замедление retry-цикла; per-cycle-ассерта нет | Открыт (осознанный trade-off, не задокументирован) |
| C3-11 | P3 | `239c8943` | review doc: `/tmp` → `/private/var` (верно `/private/tmp`); `627219bf` оценён «корректно», C3-1 пропущен | Открыт (docs) |
| C3-12 | P3 | `0b5bd37c` | no-op merge ставит до-R14 коммит первым родителем; `--first-parent` история скрывает всю R14-интеграцию | Историческое, не исправляется |
| CR-8 | — | `b995d135` | (из 36h-ревью, помечен открытым) brokers до `RunQueuePump.Stop()` | **Не воспроизводится на tip**: порядок обратный (`app_lifecycle.go:165-194`) |
| CR-12 | — | `0563e181` | (из 36h-ревью, помечен открытым) пустой output в `notifyBackgroundJobDone` | **Не воспроизводится на tip**: bounded detached grace (`background.go:757-768, 797-825`) |

---

## Итог

Свежая часть диапазона (`1d3cdf4a7..239c8943`, 18 коммитов) —
**блокирующих находок на текущем tip нет**. Единственная находка выше
P3, C3-1, была реальной регрессией push-gate'а (retry воспроизводил тот
самый Windows OOM, который коммит устранял) и закрыта сутками позже в
`0cd9dd3f`.

Продуктовый код в свежей части меняет ровно один коммит — `9c82c0c2`
(`agentic_fetch` вон из ephemeral toolset), и он корректен: механизм
`os.MkdirTemp("")` → реальный OS temp подтверждён по коду, отсутствие
re-add-путей подтверждено по `workerToolNames`/`folderScopeOpForTool`,
тест невакуумный и ловит даже транзиентное создание каталога. Остальные
17 — тесты, hooks, Makefile и docs.

Качество test-фиксов высокое: во всех четырёх «флейк»-коммитах механизм
доведён до конкретного места в продовом коде (async
`clearHolderMetadata`, initial tick pump'а, два живых замера, стоимость
retry-цикла на не-WAL SQLite), а не заменён увеличением таймаута
наугад; `85623b1c` явно отклонил продовый фикс с верным обоснованием
про hang-риск #337. Три «канонизационных» коммита — тоже один
последовательно применённый механизм, а не 17 разных заплаток.

**Подождут:** C3-2, C3-4, C3-5 — три однострочных фикса
(`trap`/`rm`, проверка `[ -n "$pkgs" ]`, добавить `agentic_fetch` в
зеркальный список). C3-3 стоит закрыть, сняв фиксированные таймауты с
`go build`/`go run golangci-lint` или подняв их до заведомо
холодно-кэшевых величин. C3-7 — усилить снимок до рекурсивного.
C3-6, C3-8, C3-10, C3-11 — правки комментариев/атрибуции.
C3-9 и C3-12 фиксируются как контекст.

Из покрытой части диапазона (`239c8943..7b3c935e`, 25 коммитов)
самостоятельно перепроверено шесть пунктов; два из них — CR-8 и CR-12,
помеченные в 36h-ревью как открытые, — на текущем tip `f2914d53c` уже
не воспроизводятся и должны считаться закрытыми. Остальные вердикты
36h-ревью цитируются без независимого перевывода (список — раздел 7.3).
