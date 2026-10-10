# Ревью: ядро task tree и standalone tasklab (0f661e36, d3b7b97b)

Дата: 2026-10-09 — 2026-10-10. Ревьюеры: делегированный агент (Fable 5.1,
xhigh) — чтение, прогоны, находки R-001..R-003, R-005; прерван лимитом
провайдера. Продолжение (Opus 5.5): мутационные и property-проверки,
правила репозитория (R-004), D-6, перепроверка находок, закрытие.

Проверяемые коммиты:

- `0f661e36` — "docs: add task tree design and round-two batch reviews"; в
  объёме только `docs/plans/2026-10-08-task-tree-design.md` (blob `1f084e26`).
- `d3b7b97b` — "feat: add autonomous task tree and standalone tasklab";
  59 файлов, +8593/-22; дизайн-документ обновлён до blob `d3322276`.

Вердикт: **GO** — P0: 0, P1: 0, P2: 2 (R-001, R-004), P3: 3 (R-002, R-003,
R-005) + 4 пробела тестов (T-1..T-4, P3). P0/P1 нет — по Review Stop Rule
серия ревью на этом заканчивается; P2/P3 уходят в бэклог, следующий раунд
только при новой P0/P1, новой подсистеме или прямой просьбе оператора.

Уровни доказательности в тексте: `[RUN]` — проверено запуском;
`[READ file:line]` — проверено чтением пути кода до конца; `[INFERENCE]` —
вывод без прямого подтверждения.

## Ход ревью

Статусы: `done` — просмотрено целиком, `partial` — просмотрено частично,
`todo` — не начато.

| Слой / измерение | Статус | Примечание |
| --- | --- | --- |
| Дизайн-документ `docs/plans/2026-10-08-task-tree-design.md` (840 строк) | done | прочитан полностью + diff 0f661e36..d3b7b97b |
| Ядро: `types.go`, `contract.go`, `tree.go`, `transition.go`, `snapshot.go`, `service.go`, `view.go` | done | прочитаны полностью |
| `memory/store.go` | done | прочитан полностью |
| `protocol/{types,decode,api,definition,render}.go` | done | прочитаны полностью |
| `lab/{types,codec,bounds,preflight,runner,repl,expect}.go` | done | прочитаны полностью |
| `cmd/tasklab/{cli,main}.go` | done | прочитаны полностью |
| `internal/csync/maps.go` + `maps_schema_test.go` | done | diff прочитан |
| CHANGELOG.md / CHANGELOG.fork.md | done | diff прочитан |
| Тесты ядра `kernel_*_test.go`, `service_*_test.go` | done | прочитаны полностью (3 + 5 файлов) |
| Тесты `memory/*_test.go`, `protocol/*_test.go` | done | прочитаны полностью (3 + 4 файла) |
| Тесты `lab/*_test.go`, `cmd/tasklab/*_test.go`, фикстуры `lab/testdata/*.json` | done | прочитаны полностью: 6 файлов lab-тестов, 4 файла cmd-тестов, все 8 фикстур |
| Сборка/vet/тесты новых пакетов через capped-wrapper | done | `go vet` rc=0; `go test -p 1 -parallel 1 -count=1` для 5 пакетов — ok; повторно с `-parallel 2` для 5 пакетов + `csync` — ok (D-6) [RUN] |
| `-race` на `internal/tasktree`, `memory`, `protocol` | done | три отдельных прогона `-race -p 1 -parallel 1 -count=1` — ok; повторно с `-parallel 2` — ok (D-6) [RUN] |
| Мутационные проверки (throwaway-правки + откат) | done | 12 мутантов, 12 убиты, см. «Мутационные проверки» [RUN] |
| Property-проверка ядра (80 000 случайных команд) | done | нарушений нет, см. «Property-проверка ядра» [RUN] |
| Запуск скомпилированного `tasklab` на фикстурах, export/inspect/resume, REPL | done | см. «Запуски» [RUN] |
| Изоляция зависимостей (`go list -deps`), goreleaser/workflows | done | `go list -deps ./cmd/tasklab`: только stdlib + 4 пакета `internal/tasktree/...` [RUN]; grep по `.goreleaser.yml`, `.github/workflows/*`, `.githooks/*`: упоминаний `tasktree`/`tasklab` нет; `builds:` в goreleaser собирает только `.` (корневой `rush`), `cmd/tasklab` в релиз не попадает [READ] |
| Правила репозитория (1000 строк, приватные пути, комментарии, CHANGELOG) | done | размеры в норме (max 369); один приватный путь в дизайн-доке — R-004; см. «Правила репозитория» |
| Перепроверка находок R-001..R-005 и ссылок «Проверено и в порядке» | done | цитаты перечитаны; R-001 — уточнены строки; R-003 — учтены строки 234 и 376 документа; остальное подтверждено [READ] |

### Запуски (все [RUN], компилированный `tasklab.exe` из этого worktree)

- `run` на `basic`, `blocked-only`, `nested`, `retries`, `operator-delete`, `boundaries` — exit 0 для всех.
- `run --snapshot board.json export-save.json` → 0; `inspect board.json` → 0; `run --load board.json --snapshot board.json export-resume.json` → 0; повторный `inspect` → 0. Содержимое после resume: `revision=6`, `next_id=9`, квитанции `init@1, move@2, block@3, rm@4, readd@5, continue@6` сохранены, tombstone `n5`, guards пусты (оператор явно пере-добавил «Gone»), `n3` `blocked`/`waiting`, `n2.children=[n4,n3,n8]`; replay шага 0 вернул `replayed=true`, `receipt.committed_revision=1`, `created[n5].removed=true`. Совпадает с §9.3 документа.
- Коды выхода: без аргументов → 2; `--help` → 0 (stdout 976 B, stderr 0); `run --help` → 0, но usage в **stderr** (stdout 0 B, stderr 1133 B) — R-005; `run` без файла → 2; флаг после позиционного → 2; неизвестная подкоманда → 2; отсутствующий файл → 1; `inspect <scenario>` → 1 (`json: unknown field "steps"`).
- REPL (agent): `init` → rev 1, focus `n1`; `block n0` → rev 2, blocked=2, focus/next пусты; `unblock n0` → rev 3, focus пуст, next `n1` (фокус после unblock не выбирается — §3.3); `view` — rev 3 без записи; `rm` агентом → `forbidden`, exit 0, сессия продолжилась.
- REPL с подделанным wrapper-полем `actor` → `line 2: unknown wrapper field "actor"`, exit 1, `--snapshot` не записан, следующая команда не выполнена.
- REPL оператора с `--load board.json` (ключ/лимиты совпадают): replay агентского `init` под оператором → `request_reused` (actor отличается); `rm ids:[n1]` → rev 7, `delta.removed=[n1,n2,n4,n3,n8,n7]` (pre-order, предок до потомков).
- REPL строка 1.1 MB → `read REPL: bufio.Scanner: token too long`, exit 1.
- Формы `expected_revision` через REPL: `2.0`, `"2"`, `18446744073709551616`, `2e0`, `-0` → все `invalid_input` с сообщением `json: cannot unmarshal …`, ревизия не тронута; затем `2` → принято. Закрывает T-1 по поведению (тестом не закреплено).
- `edit active_form` длиной 200 000 байт при `max-title-bytes=1024` → принято, rev 2 — подтверждает R-001.

### Правила репозитория [RUN/READ]

- Размер файлов: `wc -l` по всем 47 `.go`-файлам коммита d3b7b97b —
  максимум 369 строк (`internal/tasktree/protocol/api_test.go`), затем
  `transition.go` 362, `tree.go` 346; всего 8521 строка. Лимит 1000 не
  нарушен, allowlist не трогали — и не нужно.
- Приватные пути: grep по добавленным строкам обоих коммитов (дизайн-док,
  `internal/tasktree/**`, `cmd/tasklab/**`, `internal/csync/**`, оба
  CHANGELOG) на пути с буквой диска, `Users`, `/home/`, имя
  пользователя машины. В коде, тестах, фикстурах и добавленных строках
  CHANGELOG — чисто. **Одно попадание в дизайн-документе** — R-004.
  (Старые пути с буквой диска в `CHANGELOG.fork.md` на строках 304 и
  1772–1774 существовали до этих коммитов — вне объёма ревью.)
- Комментарии: в production-файлах ядра/сервиса/памяти/протокола
  комментариев почти нет (0–3 на файл), в `lab/` — 1–5 коротких
  поясняющих «почему» (`bounds.go`, `codec.go`, `preflight.go`). Стиль
  лаконичный, соответствует правилу. Большинство экспортируемых
  идентификаторов без doc-комментариев; линтер (`revive`/golangci) не
  запускался по правилам этого ревью, поэтому соответствие
  `.golangci.yml` не подтверждено.
- `internal/csync/maps.go:148`: маркер «Fork patch: See CHANGELOG.fork.md,
  "Autonomous task tree"» указывает на существующий раздел
  «Autonomous task tree and lock-free schema alias» в `CHANGELOG.fork.md`.
- CHANGELOG: `CHANGELOG.md` (`[Unreleased]`) — две пользовательские записи
  (компонент + `tasklab`; исправление `csync`-схемы/vet). `CHANGELOG.fork.md`
  — раздел для будущих слияний: держать компонент stdlib-only и не
  терять регрессию map-value-схемы при импорте upstream. Размещение
  правильное (пользовательское — в основной, merge-guidance — в fork).
  Точность: «existing Rush `todos` and WebUI behavior remain unchanged» —
  подтверждается составом коммита (59 файлов, ни одного вне
  `internal/tasktree`, `cmd/tasklab`, `internal/csync`, docs, CHANGELOG)
  [READ `git show --stat`]; «fixes the full-tree `go vet` copy-lock
  failure» — не проверено (полный `go vet ./...` запрещён правилами
  ревью).

## Находки P0

Нет.

## Находки P1

Нет. Ни чтение, ни 12 мутаций, ни 80 000 случайных команд property-проверки
не дали воспроизводимого неверного состояния, краха или зависания.

## Находки P2

| ID | file:line | Утверждение | Доказательство | Сценарий | Направление исправления | Уверенность |
| --- | --- | --- | --- | --- | --- | --- |
| R-001 | `internal/tasktree/transition.go:34-36`, `:58`, `:193-198`; `internal/tasktree/snapshot.go:89`, `:92-110` | Поле `active_form` задачи не ограничено ни одним лимитом: `kernelLabel` применяется только к `Title`/`Reason`; `ValidateSnapshot` длину `ActiveForm` не проверяет; протокол (`protocol/decode.go:112`) тоже не ограничивает. Тест `TestKernelExactLimitsAndLateDraftAtomicity` (`kernel_validation_test.go:105-112`) явно закрепляет приём `active_form` длиннее `MaxTitleBytes` в 400 раз, т.е. это осознанное решение, но документ §3.5 обещает «Limit violations reject the entire request» для всего, что хранится. | [READ] | Агент шлёт `{"op":"edit","id":"n1","active_form":"<10 MiB>","expected_revision":N}` — принято, навсегда в snapshot; при интеграции в SQLite ряд растёт без лимита; lab-чекпоинт потом не проходит `MaxInputBytes` (4 MiB) на экспорт — ожидаемый отказ lab, но доска уже «отравлена». | Добавить `MaxActiveFormBytes` в `Limits` (или переиспользовать `MaxTitleBytes`) и проверять в `planAdd`, `planEdit`, `ValidateSnapshot`; обновить §3.5 документа. | высокая (факт), средняя (что это дефект, а не политика) |
| R-004 | `docs/plans/2026-10-08-task-tree-design.md:43` (с 0f661e36, сохранено в d3b7b97b) | Дизайн-документ содержит абсолютный путь этой машины с буквой диска к соседнему локальному checkout'у (`<диск>:/…/oh-my-pi/packages/coding-agent/src/tools/todo.ts`). Правило оператора запрещает писать в репозиторий частные пути машины; соседняя строка 47 уже использует корректную относительную форму `packages/coding-agent/src/session/todo-tracker.ts`. Оба коммита уже содержатся в `origin/main` (по локальной tracking-ссылке), т.е. опубликованы. | [RUN `git grep` по d3b7b97b и 0f661e36] | Любой читатель публичного репозитория видит раскладку дисков/каталогов машины оператора. Функционального вреда нет. | Новым коммитом заменить на `oh-my-pi: packages/coding-agent/src/tools/todo.ts` (по образцу строки 47); историю не переписывать. | высокая |

## Находки P3

| ID | file:line | Утверждение | Доказательство | Сценарий | Направление исправления | Уверенность |
| --- | --- | --- | --- | --- | --- | --- |
| R-002 | `internal/tasktree/protocol/decode.go:13-48`, `:131` | Декодер протокола не ограничивает размер/глубину входного payload; глубина ограничена только внутренним лимитом `encoding/json` (10000). Документ §5: «Protocol owns transport validation», но байтовых границ для протокола документ не задаёт (только lab-bounds §9.2). | [READ] | Хост передаёт 100 MiB `list` — весь буфер разбирается в `map[string]json.RawMessage` до каких-либо доменных проверок; отказ придёт только из `MaxNodes`. Не крах и не неверный результат — расход памяти пропорционален входу. | Зафиксировать в документе, что байтовая граница payload — ответственность хоста (адаптера), либо добавить опциональный `MaxPayloadBytes` в `protocol.New`. | высокая |
| R-003 | `internal/tasktree/tree.go:170-171` vs `protocol/decode.go:204-206` | Ядро принимает `edit` без `title` и без `active_form` как семантический no-op (`planEdit` → `changed=false`, тест `kernel_transition_test.go:204` закрепляет `Delta{}`), тогда как протокол отвергает такой запрос как `invalid_input`. Документ §3.4: «edit: One node; title and/or task active form», но там же (строка 234) «An edit with no changed fields is a no-op» — формулировка допускает и прочтение ядра, т.е. это асимметрия слоёв, а не нарушение контракта. Расхождение не наблюдаемо через протокол, только для прямых вызовов `Apply`/`Service.Mutate`. | [READ; цитаты перепроверены] | Прямой `tree.Apply(agent, Command{Op: OpEdit, Target: {ID:"n1"}})` → `Delta{}`, nil error; через `Service.Mutate` добавит квитанцию и ревизию — для семантического no-op это по документу (строка 376: «including a semantic no-op, advances…»). | Либо `kernelCommand` требует `title != nil \|\| form != nil` для `OpEdit`, либо явно задокументировать, что пустой edit — no-op ядра. | высокая |
| R-005 | `cmd/tasklab/cli.go:103-105` | `tasklab run --help` / `repl -h`: `flag.ErrHelp` возвращает код 0, но текст usage уходит в `diagnostics` (stderr), а не в stdout, как для `tasklab --help` (`cli.go:62-70`). Документ §6.1: «Help/usage errors exit 2» — для `--help` после подкоманды код 0 с выводом в stderr не описан. | [READ] | `tasklab run --help >/dev/null` печатает usage в stderr и завершается 0. | Унифицировать: либо stdout + 0, либо задокументировать. Косметика. | высокая |

## Проверено и в порядке

Ядро (`tree.go`, `transition.go`, `snapshot.go`, `view.go`) [READ, пути прослежены до конца]:

- Фокус: `applyPlan` (`tree.go:270-291`) снимает фокус при любой записи активного листа не-`in_progress`, затем ставит фокус на единственную `in_progress`-запись плана; `p.advance` выставляется только для `init/add` (`transition.go:61`), `done/block/drop` активного листа (`transition.go:163-165`) и `rm` активного (`transition.go:333-335`). `unblock`/`reopen`/`edit`/`move` фокус не трогают — соответствует §3.3. Повторный `start` на уже активной задаче — no-op (`transition.go:110`).
- Блокировка/дроп группы: терминальные листья пропускаются только в групповом режиме (`transition.go:132-137`, `140-148`), для одиночной задачи — `invalid_transition`; `drop` abandoned-задачи — no-op; `done` abandoned — ошибка. Соответствует §3.4.
- Генерация ID: счётчик берётся из плана и публикуется только при успехе (`tree.go:298`), отказ плана счётчик не тратит; `^uint64(0)` никогда не выдаётся (`transition.go:53-55`); `kernelCounter` требует канонический base-36 без ведущих нулей/верхнего регистра (`snapshot.go:43-49`).
- Удаление: предок/потомок схлопываются обходом в pre-order (`transition.go:326-344`), дубликаты в `ids` — `invalid_input`, корень — `invalid_target_kind`, все цели валидируются до записи; tombstone хранит оператора; лимит tombstones/guards проверяется до публикации (`transition.go:345`).
- Move: самоссылка как родитель/якорь, цикл по цепочке предков, якорь не из целевой группы, глубина поддерева после переноса — все отклоняются до записи (`transition.go:228-270`); перенос на то же место — no-op без структурных записей (`transition.go:288-290`).
- `ValidateSnapshot` закрывает: схему, корень, uninitialized-инвариант, канонические ID, `NextID` > всех живых и удалённых счётчиков, одиночный фокус, причины только у blocked/abandoned, согласование parent/children (один входящий ребёнок, нет сирот), циклы/недостижимые компоненты, глубину, лимиты. Инвариантов, которые живое ядро соблюдает, а импорт пропускает, не найдено, кроме R-001 (`active_form`), где и живое ядро ничего не требует.
- `Restore` клонирует вход (`snapshot.go:200`), `Snapshot()` отдаёт клон (`:218`); `View`/`Summary`/`Briefs` строят свежие срезы (`view.go`). Алиасов внутреннего состояния наружу не найдено.
- `t.depth()`/`t.path()` (`tree.go:130-150`) зациклились бы на сироте — недостижимо, т.к. все входы проходят `ValidateSnapshot` (`service.go:146`, `snapshot.go:197`).

Сервис и хранилище (`service.go`, `memory/store.go`) [READ]:

- Порядок §4.2 соблюдён: декод/контекст → Load → поиск квитанции **до** проверки ревизии (`service.go:225`) → forbidden → conflict → лимиты → Apply → квитанция → `CheckEnvelope` кандидата → `ctx.Err()` → Commit. После успешного Commit отмена контекста не превращается в откат (`service.go:254-273`).
- Fingerprint = SHA-256 от `json.Marshal{expected_revision, command}` (`service.go:179-189`): порядок свойств/пробелы транспорта не влияют (проверено тестом `TestServiceTypedCanonicalFingerprintAndReceiptCap`).
- `memory.Store.Commit` (`store.go:61-117`): CAS по ревизии под mutex, кандидат клонируется до захвата, «ровно одна новая квитанция», история квитанций/tombstones неизменяема, вид узла и `NextID`/`Initialized` монотонны. Отсутствующая доска создаётся только против ревизии 0.
- `CheckEnvelope` (`service.go:54-127`): квитанции с уникальными `CommittedRevision ∈ [1, Revision]`, канонический hex-SHA-256, ID дельт существуют (живые или tombstone), `completed ⊆ updated` и только задачи, `removed` требует tombstone того же оператора, повторное создание ID запрещено. Ревизия 0 допускает только пустую доску.
- Потокобезопасность: `Service` и `protocol.API` без состояния; единственный мьютекс в `memory.Store`; дерево создаётся на каждый вызов из detached-копии. Колбэков под замком нет. Возвращаемые `Envelope` — клоны (`store.go:56`).

Протокол (`protocol/*.go`) [READ]:

- `object()` отвергает дубликаты ключей, trailing JSON, не-объекты; `readField` отвергает `null`; `expected_revision` обязателен для всех мутаций (включая 0), запрещён для `view`; поля проверяются против allow-list операции; `ids` непустой/без дублей/без пробелов; `items` только непустые строки; drafts рекурсивно с allow-list по kind. Неизвестные операции → `invalid_input`.
- `Execute`: для корректируемых ошибок читает текущий `Summary` и возвращает `IsError=true` с проблемой и ревизией (§5); инфраструктурные ошибки и `context` ошибки возвращаются как Go-ошибки, а не как tool-результат.
- Рендер экранирует все пользовательские строки через `strconv.Quote` (`render.go:11`) — инъекция структуры в текст невозможна (закреплено `TestProtocolUnicodeReadableAndControlsEscaped`).

`internal/csync/maps.go` [READ diff]: `JSONSchemaAlias` перенесён на встроенный пустой `mapSchema[K,V]`; метод по-прежнему продвигается на `Map[K,V]` (value receiver у пустой структуры — копирования мьютекса нет); поведение для существующих пользователей (`Map` как поле конфигов с reflect-схемой) не меняется. Тест `TestMapSchemaDescribesMapValues` проверяет реальный `jsonschema.Reflect(&Map[string,int]{})` → `object` с `additionalProperties.type == integer`; не тавтологичен: при переходе на pointer receiver тест падает — подтверждено мутацией M12 [RUN].

## Расхождения документации с кодом

| # | Документ | Код | Кто прав |
| --- | --- | --- | --- |
| D-1 | §2 «`tasktree` imports only the standard library», §9 «only non-stdlib runtime imports are those tasktree packages» | Подтверждено grep: внешние импорты `internal/tasktree` только из его же подпакетов и `cmd/tasklab`; `go list -deps` — см. раздел запусков ниже | — |
| D-2 | §3.5 «Kernel/service limits … title/reason bytes» — `active_form` не упомянут | `active_form` не ограничен (R-001) | Документ и код согласованы между собой, но оба оставляют неограниченное поле |
| D-3 | §4.1 «Problem: … expected/current revisions when relevant» и §4.2 «conflict with current revision and actionable summary» | `Problem` не несёт summary; summary добавляет протокол (`api.go:31-35`) | Код; документ читать как описание tool-result, а не `Problem` |
| D-4 | §3.4 «`edit`: One node; title and/or task active form» | Ядро принимает edit без полей как no-op (R-003); протокол отвергает | Протокол соответствует документу; ядро мягче |
| D-5 | §6.1 «Help/usage errors exit 2» | `tasklab run --help` → 0 с usage в stderr (R-005) | Косметическое |
| D-6 | §9.3 «Normal and race suites passed for all five standalone packages and `csync`, with `-p 1 -parallel 2 -count=1`» | [RUN] `go test -p 1 -parallel 2 -count=1` по одному пакету через capped-wrapper: `internal/tasktree`, `memory`, `protocol`, `lab`, `cmd/tasklab`, `internal/csync` — все ok; `-race -p 1 -parallel 2 -count=1` на `internal/tasktree`, `memory`, `protocol` — все ok. `-race` на `lab`, `cmd/tasklab`, `csync` в этом ревью не запускался | Подтверждено для 6 обычных и 3 race-прогонов; оставшиеся 3 race-прогона из утверждения не воспроизводились |
| D-7 | §4.1 «`MutationReply`: … created-node briefs from that same current revision» | `serviceReplay` берёт `Briefs` из дерева, загруженного тем же `Load`, что и summary (`service.go:199-203`); при обычной мутации — из того же `tree` до `takeSnapshot` (`service.go:242`) | Код соответствует |
| D-8 | §3.1 «Failed mutations do not consume counters» | `p.next` публикуется только в `applyPlan` (`tree.go:298`) | Код соответствует |

## Качество тестов (по чтению)

- Ядро: `kernelTestApply` после каждой принятой команды проверяет дизъюнктность `Delta` и валидность снапшота (`kernel_transition_test.go:40-75`); `kernelTestReject` проверяет, что отказ не изменил снапшот (`:76-86`). Матрица порчи снапшота — 48 кейсов, каждый проверяется и через `ValidateSnapshot`, и через `Restore` (`kernel_snapshot_test.go:85-227`). Не тавтологичны: ожидания — конкретные ID, статусы, пути, порядок детей.
- Сервис: барьерные сторы (`serviceTestBarrierStore`, `serviceTestOrderedReuseStore`) на каналах, без sleep; fault-store для `commit_unknown`/отказа/отмены до и после коммита; проверяется конечное состояние стора, а не вызовы моков. Соответствует §6.2 «Concurrency tests use barriers/channels».
- Память: реальная гонка двух `Commit` против ревизии 0 (`TestMemoryAtomicCAS`); проверка отсутствия алиасов seed/load/commit через порчу входа после публикации.
- Протокол: 29 негативных payload в `TestDecodeBoundary`; атомарность «полуправильного» init; interleaved removal внутри `Commit` (брифы и summary из одной ревизии); экранирование управляющих символов в тексте при сохранении данных в JSON.
- Lab: оракулы фикстур — явные (`TestFixtureOraclesRejectTampering` проверяет, что подмена 7 полей ожиданий ломает прогон); границы 4 MiB/512 шагов/256 KiB/4096 узлов проверяются «ровно на границе» и «+1»; preflight проверяется до валидации загруженного чекпоинта.
- CLI: atomic save проверен по стадиям write/sync/close/rename с инъекцией отказов и проверкой, что назначение и соседние temp-файлы не тронуты; коды выхода 0/1/2 проверены для 15 usage-комбинаций.
- Порядко-зависимых или вакуумных тестов не нашёл. Единственная «слабая» форма — `t.Fatal` без кода ошибки в `TestDecodeBoundary` (T-4).

## Мутационные проверки

Каждая мутация — одна throwaway-правка production-кода в review-worktree,
прогон только затронутого пакета, немедленный откат `git checkout -- <file>`
с проверкой `git status --short`. Команда для всех строк (меняется только
пакет): `heavy.sh capm 2g go test -p 1 -parallel 1 -count=1 ./internal/tasktree/<pkg>/`.
Номера строк — по d3b7b97b. Все результаты [RUN].

| # | Мутация | file:line | Пакет | Результат |
| --- | --- | --- | --- | --- |
| M1 | `unblock` выставляет `p.advance = true` (автовыбор фокуса после unblock) | `internal/tasktree/transition.go:154` | `tasktree` | KILLED: `TestKernelInitialDeltaAndEmptyBoard` (`kernel_transition_test.go:142`), `TestKernelFocusRewindAndNoNormalization` (`:210`), `TestServiceReadsDoNotCommitOrNormalize` (`service_boundaries_test.go:69`) |
| M2 | Поиск квитанции (`serviceReplay`) перенесён после forbidden и проверки ревизии | `internal/tasktree/service.go:225-233` | `tasktree` | KILLED: 7 тестов, в т.ч. `TestServiceReplayIdentityNoopAndCurrentBriefs` (`service_behavior_test.go:68`), `TestServiceOperatorReceiptReuseBeforeAuthorization` (`service_import_test.go:25`: «want request_reused, got forbidden»), `TestServiceCommitUnknownWrappingConflictPreservesDurableReceipt` |
| M3 | `ValidateSnapshot` пропускает две focus-задачи (`active > 1` → `active > 2`) | `internal/tasktree/snapshot.go:115` | `tasktree` | KILLED: `TestKernelSnapshotCorruptionMatrix` (`kernel_snapshot_test.go:204`: «got <nil>; want invalid_snapshot») |
| M4 | CAS по ревизии в `memory.Store.Commit` отключён (`false && current.Revision != expected`) | `internal/tasktree/memory/store.go:81` | `memory` | KILLED: `TestMemoryAtomicCAS` (`store_test.go:107`: «want conflict, got invalid_snapshot: candidate must add exactly one receipt»). Замечание: проигравший кандидат отбивается и без CAS — проверкой «ровно одна новая квитанция» (`store.go:90`); тест ловит мутацию только по коду ошибки, т.е. закрепляет именно `conflict`, что и требуется для пути реконсиляции в `service.go:261` |
| M5 | Проверка цикла в `move` отключена (`false && id == n.ID`) | `internal/tasktree/transition.go:243` | `tasktree` | KILLED: `TestKernelMoveValidationAndDepthBoundaries` (`kernel_validation_test.go:85`: «error limit_exceeded: move exceeds depth limit; want invalid_input»). В этом тесте `MaxDepth=3`, поэтому цикл ловит лимит глубины с другим кодом; при свободном лимите мутант принял бы команду и `kernelTestReject` тоже упал бы — убийство устойчиво |
| M6 | Проверка дублей в `rm ids` отключена (`false && selected[id]`) | `internal/tasktree/transition.go:305` | `tasktree` | KILLED: `TestKernelRemovalGuardsAndAtomicBatches` (`kernel_transition_test.go:270`: «error <nil>; want invalid_input») |
| M7 | Отклонение неизвестных/нерелевантных полей операции отключено (`false && !allowed[f]`) | `internal/tasktree/protocol/decode.go:147` | `protocol` | KILLED: `TestDecodeBoundary` — 6 негативных кейсов (`decode_test.go:44`: «accepted malformed/irrelevant payload») |
| M8 | Обязательность `expected_revision` для мутаций отключена (`!ok && false`) | `internal/tasktree/protocol/decode.go:152` | `protocol` | KILLED: `TestDecodeBoundary` — ровно 1 кейс (`decode_test.go:44`). Закреплено единственным payload — достаточно, но хрупко (см. T-4: проверяется только `err != nil`) |
| M9 | Лимит квитанций off-by-one (`>=` → `>`) | `internal/tasktree/service.go:234` | `tasktree` | KILLED: `TestServiceTypedCanonicalFingerprintAndReceiptCap` (`service_envelope_test.go:162`: «want limit_exceeded, got task tree validate candidate: invalid_snapshot: receipt history exceeds limit»). Без предварительной проверки страховочный `CheckEnvelope` кандидата всё равно не дал бы записать — но как инфраструктурную ошибку; тест закрепляет корректируемый `limit_exceeded` |
| M10 | Replay сравнивает только actor, без fingerprint (тот же request ID с другим payload переигрывается) | `internal/tasktree/service.go:196` | `tasktree` | KILLED: `TestServiceReplayIdentityNoopAndCurrentBriefs` (`service_behavior_test.go:83`: «want request_reused, got <nil>»), `TestServiceConcurrentCASRequestReuse` (`service_reconciliation_test.go:101`) |
| M11 | Лимит tombstones в `rm` off-by-one (`… - len(Tombstones)` → `… - len(Tombstones) + 1`) | `internal/tasktree/transition.go:345` | `tasktree` | KILLED: `TestKernelIndependentHistoryLimits` (`kernel_validation_test.go:134`: «error <nil>; want limit_exceeded») |
| M12 | `JSONSchemaAlias` на pointer receiver (`func (*mapSchema[K, V])`) | `internal/csync/maps.go:150` | `csync` (`-run TestMapSchema`) | KILLED: `TestMapSchemaDescribesMapValues` (`maps_schema_test.go:22`) |

Итог: 12 мутантов, 12 убиты, 0 выжили. Важные поведения — автофокус,
порядок replay → forbidden → conflict, одиночный фокус при импорте, CAS,
цикл в move, дубли `ids`, allow-list полей, обязательная ревизия, лимиты
квитанций и tombstones на границе, привязка replay к fingerprint —
закреплены тестами с проверкой конкретного кода ошибки. Находок по
качеству тестов мутации не дали.

### Property-проверка ядра [RUN]

Throwaway-тест (`internal/tasktree/zz_review_property_test.go`, не
закоммичен, удалён) — 20 сидов (`math/rand`, seed 1..20) × 4000 шагов =
80 000 команд; дерево пересоздаётся каждые 250 шагов. Лимиты нарочно тесные:
`MaxNodes=14, MaxDepth=4, MaxTitleBytes=8, MaxReasonBytes=8,
MaxTombstones=9`. Генератор: все 12 операций, актор agent/operator (2:1),
цели — 70% живые ID, остальные — случайные счётчики (включая удалённые,
`n0` и ещё не выданные) и несуществующие; селекторы по тексту с `within_id`
и пустые; заголовки из набора с дублями, пустыми, пробельными и длиннее
лимита; drafts до глубины 3, в т.ч. невалидные (задача с детьми, группа с
`active_form`, неизвестный kind); `rm` по селектору или `ids` с возможными
дублями; `move` с произвольными родителем/якорем (включая себя и потомков).

Проверки после каждого шага: (1) принятая команда — `ValidateSnapshot`
валиден, `Restore(Snapshot())` даёт `DeepEqual`-снапшот, JSON-раунд-трип
через `Restore` даёт байт-в-байт тот же JSON, `Delta` дизъюнктна, `view`
не даёт дельты; (2) отклонённая — ошибка типа `*Problem`, снапшот
байт-в-байт и `DeepEqual` не изменился, `Delta{}`; (3) не более одной
`in_progress`-задачи и `Summary().ActiveID` совпадает со сканом снапшота
(кэш `t.active` не расходится); (4) дифференциально: та же команда,
применённая к дереву, только что восстановленному из снапшота
(`Restore(before)`), даёт ту же ошибку/дельту/снапшот — производные кэши
(`active`, `order`, `guards`) не влияют на результат.

Результат: PASS, нарушений нет. Принято 18 575, отклонено 61 425
(`invalid_target_kind` 14 757, `not_found` 12 599, `invalid_input` 10 695,
`forbidden` 6 255, `invalid_transition` 5 190, `already_initialized`
4 841, `limit_exceeded` 2 493, `removed` 2 123, `ambiguous_target` 1 085,
`uninitialized` 797, `removed_by_operator` 590). Контроль
чувствительности: с мутантом M5 (цикл в `move`) тест падает на seed 1,
шаг 1891 (`move n6` в самого себя → «accepted but invalid: orphan node is
not listed by its parent»), т.е. оракул рабочий.

## Пробелы тестов

| # | Где | Чего не хватает | Тяжесть |
| --- | --- | --- | --- |
| T-1 | `protocol/decode_test.go` | Нет негативных кейсов для `expected_revision` в виде `1.0`, `1e2`, `"1"`, `18446744073709551616` (uint64+1). По коду `json.Unmarshal` в `uint64` их отвергает, но тестом это не закреплено. | P3 |
| T-2 | `protocol/*_test.go` | Нет теста на вложенность drafts глубже `MaxDepth` через протокол (ядро покрыто в `TestKernelMoveValidationAndDepthBoundaries`, но протокольный путь `decodeDraft` → `planAdd` не проверен сквозным кейсом). | P3 |
| T-3 | `internal/tasktree/*_test.go` | Нет теста на `active_form` длиной в мегабайты (следствие R-001: лимита нет, тест закрепляет обратное). | P3 |
| T-4 | `protocol/decode_test.go` | Все 29 негативных кейсов проверяют только `err != nil`, без кода/сообщения — достаточно для декодера (всегда `invalid_input`), но при добавлении нового кода ошибки регресс не ловится. | P3 |

## Рекомендованный порядок исправления

Всё ниже — бэклог (P2/P3), не блокирует; порядок — по соотношению
ценности и стоимости.

1. R-004 — заменить абсолютный путь в `docs/plans/2026-10-08-task-tree-design.md:43`
   на относительную форму (одна строка, новым коммитом).
2. R-001 + T-3 — решить политику для `active_form`: ввести лимит (отдельный
   или `MaxTitleBytes`) в `planAdd`/`planEdit`/`ValidateSnapshot` и
   переписать закрепляющий приём кейс `kernel_validation_test.go:105-112`,
   либо явно задокументировать неограниченность в §3.5. Сделать до
   интеграции с SQLite/хостом, пока нет сохранённых досок.
3. R-002 — зафиксировать в §5, что байтовая граница payload — обязанность
   хоста, или добавить её в `protocol`.
4. T-4, затем T-1, T-2 — в `TestDecodeBoundary` проверять код/сообщение;
   добавить негативные формы `expected_revision` и сквозной кейс глубины
   drafts через протокол.
5. R-003 — выровнять ядро с протоколом для пустого `edit` или описать
   асимметрию в документе.
6. R-005 — usage для `run/repl --help` в stdout либо описать в §6.1.

## Что не проверялось

- `-race` на `internal/tasktree/lab`, `cmd/tasklab`, `internal/csync`
  (утверждение §9.3 для них не воспроизводилось; обычные прогоны — ok).
- Полный `go vet ./...` / `go build ./...` / golangci-lint (включая
  `revive` `file-length-limit` и doc-комментарии экспортируемых
  идентификаторов) — запрещены правилами ревью; поэтому не подтверждено и
  утверждение CHANGELOG о починке full-tree `go vet` copy-lock.
- Pre-push hook `.githooks/pre-push` не запускался.
- Мутации не делались в `lab/` (codec, bounds, preflight, runner, repl,
  expect), `cmd/tasklab/cli.go`, `protocol/render.go`/`definition.go`,
  `view.go` (подсчёт `Progress`) и в fingerprint-канонизации
  (`service.go:179-189`) — эти места проверены только чтением и
  существующими тестами/прогонами CLI.
- Property-проверка покрывала только ядро (`Tree.Apply`/`Restore`), не
  `Service.Mutate`/`memory.Store` (квитанции, CAS, replay под случайной
  нагрузкой) и не протокол; конкурентная property-проверка не делалась.
- Остальные файлы коммита 0f661e36 (четыре `job-batches-xxs-*-round2`
  ревью) — вне объёма.
- `.github/workflows`, goreleaser — только grep/чтение, не запуск; сборка
  релизных артефактов не проверялась.
- Поведение на не-Windows платформах (atomic save `rename` поверх
  существующего файла и т.п.) не проверялось — все прогоны на Windows.
