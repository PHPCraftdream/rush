# Статический follow-up review multi-agent/session freeze — 2026-08-19

## Вердикт

**NO-GO для стабильного релиза.** Основная масса дефектов из предыдущего
review исправлена по существу: watchdog теперь снимается на всех выходах,
результат background execution передаётся ожидающему drain, конфигурация
моделей в основном захватывается одним snapshot, smart/fast rename доведён до
пользовательских путей, а Unix получил cross-process registry дочерних process
groups.

Нового классического взаимного deadlock вида `A -> B -> A` в просмотренном
коде не найдено. Однако утверждать, что зависания и ложные успешные завершения
устранены, всё ещё нельзя. В новом outcome-handoff остаются две
release-blocking гонки:

1. отказавший `admitSession` не возвращает ту admission entry, которой он
   проиграл; отдельный `waitForAdmission` может уже увидеть следующее
   исполнение той же сессии;
2. после успешно или неуспешно выполненной записи встреча с занятой следующей
   записью возвращает накопленный `drained=true`, но без ошибки, хотя очередь
   обработана не полностью.

Кроме того, outbox считает `context.Canceled`, `DeadlineExceeded` и почти все
SQLite-ошибки постоянными. Несколько штатных shutdown/timeout-событий способны
навсегда перевести здоровую принятую работу в `failed`.

## Область и метод

- HEAD: `0635c631ea50a259b4e60b1761a9a65f4a5d75ca`.
- База: `da10303fc7514f484d928b473c08e0d8e5c3ba86`, HEAD предыдущего статического
  review.
- Диапазон: 14 commits, 96 files, `+5959/-1855`.
- Просмотрены Git history/diff и связанные production-пути в
  `internal/session`, `internal/agent`, `internal/app`, `internal/message`,
  `internal/server`, `internal/cmd`, `internal/config` и `web/src`.
- Проверка была только статической. По прямому указанию пользователя не
  запускались tests, race detector, build, lint, генераторы, приложение или
  e2e suite. Формулировка commits о «зелёном e2e» в этом отчёте не считается
  независимо подтверждённым результатом.
- Существующие изменения `web/dist/.gitkeep`, `dev/`, checkpoint и предыдущий
  untracked review не изменялись.

## Release blockers

### P0-1. Admission outcome может быть взят у следующего, а не у проигранного исполнения

`admitSession` проверяет и создаёт admission entry под `inFlightMu`, но при
занятой сессии возвращает `(nil, nil, false)`, отбрасывая уже найденную entry
(`internal/session/run_queue_admission.go:107-129`). Затем
`DrainSessionNow` отдельным вызовом заново читает текущую entry через
`waitForAdmission` (`internal/session/run_queue_drain_session.go:149-175`).

Между этими двумя критическими секциями возможна ABA-последовательность:

1. background execution A владеет entry A;
2. drain получает `admitted=false`;
3. A завершает работу, закрывает A.done и удаляет entry A;
4. background tick или второй drain успевает зарегистрировать execution B;
5. первый drain вызывает `waitForAdmission` и получает entry B.

Теперь drain ждёт и классифицирует outcome B как результат A. Ошибка,
terminal failure, Ack failure или lease loss A могут быть потеряны; успешный B
может превратить их в `(true, nil)`. И наоборот, короткое A может заставить
команду ждать совсем другое, долгое B, что снаружи выглядит как очередной
freeze. Комментарий обрабатывает только случай «между вызовами entry исчезла»,
но не случай «исчезла и была заменена» (`run_queue_drain_session.go:161-169`).

**Что исправить:** операция отказа admission должна атомарно вернуть указатель
на уже существующую entry: например,
`admitSession(...) (release, observedEntry, admitted)`. Отдельный lookup после
отказа следует удалить. Альтернатива — version/token, который проверяется до
ожидания, но прямой возврат entry проще и не оставляет окна.

**Минимальный regression contract:** детерминированно остановить drain между
отказом admission и ожиданием, завершить A, допустить B, затем доказать, что
drain получает outcome A и никогда не ждёт B.

### P0-2. Частичный drain возвращает успех и отбрасывает уже накопленную ошибку

`classifyBackgroundOutcome` трактует busy/queued/foreign session lock как
`(false, nil, stopNow=true)` (`internal/session/run_queue_drain_session.go:594-601`).
Обе ветви `DrainSessionNow` при `stopNow` возвращают `drained, nil`
(`:184-199` и `:328-363`). Значение `drained` при этом накопительное.

Реальный сценарий со stacked durable rows:

1. row A действительно выполняется, поэтому `drained=true`;
2. до обработки row B другой владелец получает session lock/mailbox ownership;
3. B получает busy outcome и остаётся pending;
4. функция возвращает `(true, nil)`.

`RunNonInteractive` напрямую преобразует это в успешное завершение
continuation (`internal/app/app_run.go:738-770`), хотя B не выполнена. Сценарий
ещё опаснее, если A завершилась ошибкой: `err` уже содержит terminal/Ack/lease
ошибку, но `return drained, nil` явно выбрасывает её и также выдаёт успех.

Это не теоретическое противоречие интерфейса: между двумя итерациями OS lock
может законно перейти другому процессу, а mailbox owner — появиться в этом же
процессе.

**Что исправить:** busy после любой предыдущей попытки должен возвращать как
минимум накопленную ошибку и признак незавершённой очереди. Пары
`(drained bool, error)` уже недостаточно: `drained` означает «хоть что-то
запускалось», а caller использует его как «continuation полностью завершена».
Нужен typed result вроде `NoWork / Complete / Partial / Failed`, либо отдельный
`complete bool`. Только `Complete` без ошибки может заменить исходную
cancellation на success.

**Минимальные regression contracts:** `success -> busy`, `retryable error ->
busy`, `terminal error -> busy`, `Ack error -> busy`; ни один не должен давать
успешный exit, и последний pending row должен оставаться наблюдаемым.

## Высокий приоритет

### P1-1. «Permanent-only quarantine» считает штатную отмену постоянной ошибкой

`processOrphanOutboxEntry` правильно использует собственный 10-секундный
budget, производный от `p.ctx` (`internal/session/run_queue_orphan_drain.go:49-93`).
Но классификатор считает transient только SQLite primary codes 5 и 6
(`SQLITE_BUSY`/`SQLITE_LOCKED`) (`:177-249`). В комментарии намеренно
зафиксировано, что `context.Canceled` и `context.DeadlineExceeded` считаются
постоянными (`:219-237`).

Это ломает заявленный контракт commit-а:

- обычный `Stop()` отменяет `p.ctx` в момент транзакции;
- 10-секундный deadline может истечь на медленном диске;
- SQLite `IOERR`, `FULL`, временный `READONLY`, сетевой/FS сбой и неизвестная
  обёртка также попадают в «permanent»;
- каждый такой исход вызывает `RecordOrphanOutboxFailure` на detached
  background context (`:133-174`);
- после пяти рестартов, shutdown races или временных сбоев здоровая row
  становится `failed` и больше никогда не сканируется.

Комментарий называет ложную permanent-классификацию безопасным направлением,
но после исчерпания бюджета это уже постоянная потеря автоматического
восстановления принятой пользовательской работы.

**Что исправить:** cancellation, вызванную `p.ctx`, не учитывать вообще;
deadline и известные retryable SQLite-коды считать transient. Для quarantine
безопаснее позитивно распознавать только доказанную permanent-причину
(сегодня фактически constraint/FK конкретной row), а неизвестную ошибку
оставлять pending с backoff и громкой диагностикой. Если нужен общий предел,
quarantine должна быть явно recoverable оператором и не называться
«permanent-only» без такой классификации.

### P1-2. Edit активного partial message подтверждается до того, как следующий checkpoint его сотрёт

Hydration `CheckpointGeneration` исправлена корректно
(`internal/message/message.go:544-566`). Новый `updateMessageAndVerify`
повторно читает partial row и проверяет, что edit присутствует сразу после
conditional update (`internal/server/handlers_messages.go:16-71`). Это
обнаруживает zero-row write из-за уже проигранного fence, но не делает edit
долговечным.

Web UI не запрещает редактировать текущий streaming assistant message:
кнопка Edit рендерится без проверки partial/busy state
(`web/src/components/Message/Message.tsx:111-129`). При этом agent продолжает
держать исходный `currentAssistant` в памяти. Следующий checkpoint той же
generation или финальная unconditional terminal write снова сохранит
in-memory parts и перезапишет только что подтверждённую операторскую правку.
Handler уже успеет ответить `status: ok`.

**Что исправить:** либо запрещать editing partial/active messages и отвечать
конфликтом до окончания stream, либо вводить настоящий edit protocol с
revision/CAS и синхронизацией с live in-memory message. Немедленный read-back
сам по себе не является гарантией сохранения.

## Средний приоритет и остаточные риски

### P2-1. Один config snapshot всё ещё не охватывает фактический набор MCP tools

`buildAgent` и большинство `buildTools` теперь используют один pinned
`*config.Config`, что закрывает основную часть предыдущего P1-3. Но фактические
MCP tools по-прежнему берутся из глобального live registry через
`tools.GetMCPTools(c.permissions, c.cfg, ...)`
(`internal/agent/coordinator_tools.go:263-299`,
`internal/agent/tools/mcp-tools.go:23-37`). Сам registry обновляется отдельно
от snapshot и фильтруется по live `cfg.Config()` в
`internal/agent/tools/mcp/tools.go`.

Reload в середине build может поэтому дать prompt/options/allow-list поколения
N и MCP implementations поколения N+1. Это уже не общий torn read всего
agent-а, но заявление «one agent from one config generation» для MCP остаётся
неполным.

### P2-2. Sticky map не устраняет stale envelopes, уже стоящие в stickyBroadcast

Sticky envelopes убраны из replay ring, и поздний client теперь получает map
до replay — основной баг update badge закрыт. Но `BroadcastSticky` сначала
заменяет `h.sticky[type]`, затем кладёт готовый envelope в отдельный buffered
channel (`internal/server/hub.go:491-521`). Старые поколения того же type могут
уже ждать в этом channel.

Если `v1` и `v2` опубликованы быстро, а registration обрабатывается после
обновления map до `v2`, новый client может получить `v2` из map, затем `v1` из
очереди (`hub.go:301-403`). В зависимости от заполнения `c.send` последующий
`v2` может быть отброшен, оставив stale value последним. Сейчас production
использует sticky только для редкого update notice, поэтому риск невысок, но
общий контракт «latest per type» реализацией не обеспечен. Нужен envelope с
type/version и suppression superseded queued values в Hub loop.

### P2-3. Unix child-group registry остаётся best-effort в самой точке внешнего SIGKILL

Cross-process registry существенно улучшает `sessions kill`, но запись файла
делается через truncate-and-write `os.WriteFile`
(`internal/session/childgroup_registry_unix.go:225-242`). Внешний kill может
остановить holder ровно между truncate и завершением записи; последующий sweep
прочитает пустой/частичный registry и не найдёт живой provider tree. Запись
должна быть atomic temp-file + fsync/rename.

Кроме того, sweep безусловно удаляет registry после попытки, включая ошибки
`killpg`/implausible outcome (`:293-322`). После временного отказа повторный
`sessions kill` уже не имеет durable указателя. Для заявленной rescue-функции
лучше удалять только подтверждённо мёртвые/успешно убитые entries, а остальные
сохранять для retry с generation/start-time fence.

На текущей Windows-машине Unix behavior не запускался и не мог быть
динамически подтверждён в рамках read-only/no-tests review.

## Что из предыдущего review действительно закрыто

- Старый background outcome теперь переносится через `admissionEntry.err` и
  `done`; five-way false-success defect закрыт для случая, когда ожидается
  именно правильная entry. P0-1 выше — новое окно выбора entry, а не отсутствие
  outcome как раньше.
- Web paths `models.fast` и optimistic clear для smart/fast исправлены.
- CLI help, errors, builtin skill и `crush_info` переведены на smart/fast;
  добавлены local/CI guards.
- `RebuildSessionAgentCall` использует provider options из того же snapshot.
- `buildAgent`/prompt/обычные tools в основном собираются из одного поколения.
- Успешный retry больше не сохраняет старую локальную ошибку; неизвестный
  cross-process outcome теперь не объявляется успехом.
- Outbox fallback получил отдельный bounded context.
- Watchdog disarm находится внутри `joinTitle` и выполняется до bounded wait на
  всех early-return и success paths (`internal/agent/agent_turn.go:532-575`).
- Sticky update survives replay-ring eviction; checkpoint generation
  гидратируется; transcript line count больше не считает wrapper.
- Unix provider child group регистрируется и доступна внешнему sweep в обычном
  полностью записанном случае.

## Deadlock и freeze assessment

В новых изменениях не найдено обратного порядка двух mutex-ов или канального
цикла, образующего классический deadlock. В частности:

- `admissionEntry.err` записывается до `close(done)`, поэтому ожидатель получает
  корректный happens-before для самой entry;
- watchdog defer order корректен: disarm происходит перед title join, cancel —
  перед ожиданием `wd.done`;
- Hub не держит `stickyMu` во время отправки client-у;
- child registry не держит свой mutex во время `killpg`;
- checkpoint не держит `sessionLock` во время SQLite update.

Но freeze-free гарантия всё ещё отсутствует:

- P0-1 может заставить drain ждать следующее, не относящееся к нему исполнение;
- P0-2 оставляет durable work pending после успешного exit, после чего сессия
  выглядит «застывшей» до следующего owner/tick;
- watchdog может отменить context, но произвольный provider/MCP/tool, который
  игнорирует cancellation, нельзя принудительно заставить вернуть управление;
- at-least-once queue по-прежнему допускает повтор внешних side effects после
  Ack failure/lease loss — ошибки теперь виднее, но transactional/idempotency
  boundary вокруг provider/tools не появился.

Итог: **явного нового lock-order deadlock нет, но все пользовательские формы
«сессия фризится/завершилась не тем результатом» ещё не закрыты.**

## Общий обзор качества кода

Сильные стороны:

- fixes всё чаще формулируют state-machine contracts и покрывают негативные
  outcomes, а не только happy path;
- ownership context/lease/admission и bounded waits стали заметно явнее;
- DB-side predicates, generation tokens и process identity tokens сильнее
  прежних memory-only предположений;
- hard rename теперь защищён автоматическими guard-ами в local hook и CI.

Системные проблемы:

- `agent_turn.go` достиг 1880 строк, `provider_stream.go` — 944,
  `run_queue_drain_session.go` — 625. Ключевые state machines по-прежнему
  кодируются named returns, тремя bool/error значениями, closures и длинными
  forensic comments. P0-2 — прямое следствие того, что `drained` одновременно
  означает «что-то запускалось» и «всё успешно закончено».
- Комментарии часто длиннее исполняемого кода и местами заявляют более сильный
  контракт, чем реализация: «specific entry» при повторном lookup, «permanent
  only» при default-permanent классификации, «latest sticky» при очереди старых
  envelopes, «edit landed» при последующем overwrite.
- Test names и production comments жёстко привязаны к номерам исторических
  задач. Это полезно для расследования, но затрудняет чтение актуальной
  спецификации. Нужен короткий стабильный design document/state diagram, а
  tests должны ссылаться на named invariants.
- Удаление stale e2e tests выглядит обоснованным по истории, но текущий зелёный
  статус не заменяет release matrix для concurrency: admission ABA, partial
  drain, shutdown-cancel outbox и live edit требуют отдельных детерминированных
  сценариев и race runs.

## Рекомендуемый release gate

1. Исправить P0-1 атомарным возвратом observed admission entry.
2. Заменить двусмысленный drain result и закрыть все partial/busy комбинации
   из P0-2.
3. Не учитывать shutdown cancellation в outbox budget; quarantine разрешать
   только для позитивно распознанной permanent-причины.
4. Запретить edits active partial messages либо сделать revision-aware live
   edit protocol.
5. После этого выполнить отдельный динамический gate: targeted deterministic
   tests, `go test -race` для session/agent, multi-process inject/drain stress,
   repeated shutdown during outbox transaction, реальный Unix process-tree
   kill и полный web e2e. В этом review ничего из перечисленного не запускалось.

До закрытия пунктов 1–3 стабильный релиз не рекомендован.
