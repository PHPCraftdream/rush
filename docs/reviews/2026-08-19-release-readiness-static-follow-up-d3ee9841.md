# Статический follow-up review multi-agent/session stability — `d3ee9841`

## Вердикт

**NO-GO для стабильного релиза.** Семь последних коммитов закрывают почти все
конкретные замечания предыдущего review в правильном направлении: admission ABA
устранён, partial drain больше не превращается в успех при встрече с busy,
shutdown/unknown ошибки outbox больше не карантинятся как permanent, edit
активного assistant message запрещён, stale sticky envelope подавляется, а Unix
child-group registry стал атомарнее и сохраняет неразрешённые записи.

Нового классического lock-order deadlock вида `A -> B -> A` в просмотренном коде
не найдено. Однако утверждать, что зависания и ложные завершения устранены,
нельзя. Статический анализ обнаружил два новых release-blocker сценария:

1. `DrainSessionNow` может стереть ошибку row A успешным выполнением другой row B
   и вернуть `(true, nil)`;
2. `sessions kill` и `sessions reset --force` на Unix могут после смерти старого
   holder-а прочитать generation уже нового владельца и послать `SIGKILL` его
   CLI-provider process group.

Кроме того, защита streaming message добавлена только для edit/part edit, но не
для delete. Удаление активного assistant message способно разойтись с живым
in-memory turn и оставить UI и SQLite в разных состояниях.

## Область и метод

- HEAD: `d3ee9841aa8dd1b511d6ac3b1cf45acb7811d5c8`.
- База follow-up: `0635c631ea50a259b4e60b1761a9a65f4a5d75ca`.
- Последний диапазон: 7 коммитов, 37 файлов, `+3476/-504`.
- Для контекста просмотрена история за две недели: 312 коммитов. Детально
  исследованы последние изменения и связанные production-пути в
  `internal/session`, `internal/app`, `internal/cmd`, `internal/message`,
  `internal/server`, `internal/agent` и `web/src`.
- Review выполнен только чтением Git history/diff и исходников. По прямому
  указанию пользователя не запускались tests, race detector, build, lint,
  generators, приложение или e2e. Упоминания зелёных тестов в commit messages и
  checkpoint не считаются независимо подтверждённым результатом этого review.
- Существующие пользовательские изменения `web/dist/.gitkeep` и `dev/` не
  изменялись. Во время финальной проверки параллельно появились незакоммиченные
  изменения во многих `internal/session/*_test.go` и новый
  `internal/session/parallel_gate_test.go`. Они не принадлежат reviewed HEAD,
  не редактировались и не оценивались в этом отчёте.

## Release blockers

### P0-1. Ошибка одной durable row стирается успехом следующей row

`DrainSessionNow` использует один named return `err` как аккумулятор результата
всей сессии (`internal/session/run_queue_drain_session.go:145`). В обеих ветках
каждое новое выполненное outcome безусловно присваивается этому аккумулятору:

- outcome наблюдаемой admission entry: `err = outcomeErr` в
  `internal/session/run_queue_drain_session.go:210-221`;
- outcome локально выполненной row: `err = outcomeErr` в
  `internal/session/run_queue_drain_session.go:375-394`.

Это правильно только для retry **той же самой** logical row: retryable failure,
затем подтверждённый success этой row действительно должен очистить временную
ошибку. Но код не связывает accumulator с row identity. В observed-admission
ветке identity вообще не переносится, а в local ветке `lastErrRowID` используется
только для специальной recheck-логики ordinary retryable failure.

Воспроизводимая последовательность со stacked rows:

1. row A реально запускается;
2. A получает terminal failure и удаляется через terminal-fail, либо её Ack
   завершается ошибкой/теряется lease; `drained=true`, `err!=nil`;
3. цикл находит отдельную pending row B;
4. B исполняется и успешно Ack-ается;
5. `outcomeErr == nil` для B перезаписывает ошибку A;
6. очередь оказывается пустой, и drain возвращает `(true, nil)`.

Caller воспринимает эту пару как успешно завершённую continuation
(`internal/app/app_run.go:740-808`). В результате принятая работа A потеряна или
имеет неподтверждённый commit, но non-interactive run получает успешный exit.
Это тот же пользовательский класс дефекта «сессия закончилась не тем
результатом», который серия последних исправлений должна была закрыть.

Добавленные regression tests проверяют `success/error -> busy`
(`internal/session/p588_partial_drain_test.go:85-263`), но не проверяют
`terminal/Ack/lease error row A -> successful distinct row B`. Поэтому текущая
ошибка не противоречит их контракту.

**Что исправить:** заменить `(drained bool, err error)` и неявный overwrite на
typed aggregate outcome с identity row: как минимум `NoWork / Complete /
Partial / Failed`, `rowID`, `finalized` и причина. Success может очистить
retryable error только при доказанном совпадении той же row. Terminal failure,
Ack failure, lease loss и unconfirmed outcome одной row не должны очищаться
успехом другой row.

**Минимальные regression contracts:** для обеих веток — local execution и
observed admission — проверить `terminal A -> success B`, `Ack failure A ->
success B`, `lease loss A -> success B`; ни один сценарий не должен вернуть
чистый success. Отдельно сохранить существующий контракт `retryable A ->
successful retry A`, который должен завершаться успехом.

### P0-2. Unix child-group sweep может убить process group нового владельца

`probeThenKillHolder` сначала доказывает, что OS lock занят, и выбирает PID
holder-а (`internal/cmd/sessions_kill.go:315-337`). `forceKillHolder` убивает PID,
ждёт его смерти и **после смерти** вызывает `reportChildGroupSweep`
(`internal/cmd/sessions_kill.go:351-392`). На Unix смерть процесса автоматически
освобождает OS lock.

Следовательно, между подтверждением смерти старого holder-а и sweep новый
`crush run --session <id>` может:

1. приобрести тот же session lock;
2. записать новый generation;
3. запустить CLI provider и зарегистрировать его process group с новым
   generation.

Но sweep не получает generation убитого holder-а. Он заново читает **текущий**
generation из lock path (`internal/session/childgroup_registry_unix.go:411-425`)
и сигналит все entries, совпавшие с ним
(`internal/session/childgroup_registry_unix.go:432-465`). Поэтому новый entry
проходит fence и получает `killpg(..., SIGKILL)`, тогда как старые entries как
раз отбрасываются из-за generation mismatch.

Поздняя финальная проверка в `sessions kill`
(`internal/cmd/sessions_kill.go:171-197`) защищает только удаление lock-файла:
к моменту её выполнения child нового владельца уже мог быть убит. В
`sessions reset --force` re-acquire также выполняется после `forceKillHolder`, а
значит после sweep (`internal/cmd/sessions_kill.go:448-476`). Он может корректно
отказаться стирать DB из-за нового owner-а, но уже повредить его живой turn.

Есть и связанная cross-process lost-update гонка: `childGroupFileMu` защищает
только goroutines одного процесса. Sweep читает registry под mutex, отпускает
его на время проверок/`killpg`, затем записывает старый retained snapshot
(`internal/session/childgroup_registry_unix.go:411-475`). Новый процесс может
между read и rewrite зарегистрировать entry, который будет затёрт старым
snapshot даже если его не успели просигналить.

**Что исправить:** target sweep должен задаваться неизменяемой identity жертвы,
захваченной в момент доказанного busy lock: `(holder PID, victim generation)`.
После смерти holder-а rescue-процесс должен сам приобрести OS session lock,
удерживать его на всём read/verify/kill/rewrite registry и обрабатывать только
entries с **victim generation**, а не с «generation, который сейчас лежит в
файле». Для `reset --force` это означает перенести sweep после успешного
re-acquire; для `sessions kill` безопаснее также временно acquire lock, sweep под
ним, release и не unlink-ать без необходимости — пустой lock file уже считается
безопасным остальными комментариями к коду.

**Минимальный regression contract:** остановить старого holder-а после busy
probe, до sweep впустить нового holder-а с новым generation и реальным child
group, затем доказать, что новый child не получил сигнал, старый registry entry
обработан по сохранённому victim generation, а новая registry запись не
потеряна. Нужны отдельные end-to-end сценарии для `sessions kill` и
`sessions reset --force` на Unix.

## Высокий приоритет

### P1-1. Активный streaming message нельзя edit-ить, но всё ещё можно удалить

Коммит `547b0815` корректно запретил content/part edits assistant message без
terminal Finish через `updateMessageAndVerify`. Web также скрывает Pencil для
такого сообщения. Но целое сообщение удаляется другим путём без аналогичной
проверки:

- single delete напрямую вызывает `a.Messages.Delete`
  (`internal/server/handlers_messages.go:112-123`);
- bulk delete делает то же для каждого ID, проглатывает отдельные ошибки и всё
  равно отвечает `status: ok` (`internal/server/handlers_messages.go:126-137`);
- `message.service.Delete` выполняет read, безусловный `DELETE`, затем публикует
  terminal DeletedEvent (`internal/message/message.go:172-192`);
- Trash остаётся доступен для streaming assistant message
  (`web/src/components/Message/AssistantHoverActions.tsx:29-37`), а selection
  позволяет попасть в bulk path.

Живой agent turn продолжает владеть своим `currentAssistant`. После delete его
partial checkpoint затронет 0 строк и не опубликуется, но terminal update идёт
через обычный `UPDATE`, после которого код без проверки считает
`rowsAffected = 1` (`internal/message/message.go:292-358`). Для отсутствующей
SQLite row такой `UPDATE` может вернуть nil, после чего `UpdatedEvent` всё равно
будет опубликован. Возможный результат: сообщение удалилось, затем «воскресло»
только в live UI, но отсутствует в DB и снова исчезает после reload. Иной
subscriber может увидеть другую последовательность событий.

**Что исправить:** применить server-side streaming guard ко всем destructive
message mutations, включая single/bulk delete и pin/fork/rerun, если они меняют
тот же живой объект. Для assistant delete надёжнее DB predicate/CAS вроде
`DELETE ... WHERE finished_at IS NOT NULL`, а не только read-before-write. Web
должен скрывать или блокировать Trash/selection delete до terminal Finish.
Terminal `Update` должен проверять реальный rows-affected/not-found outcome, а
bulk handler — не отвечать безусловным успехом при частичных отказах.

## Средний приоритет и остаточные риски

### P2-1. Full sticky channel всё ещё может лишить уже подключённого клиента newest value

Sequence suppression корректно решает `v2 from map -> queued stale v1` для
нового клиента (`internal/server/hub.go:416-461`). Но `BroadcastSticky` сначала
обновляет map/seq, затем делает non-blocking send и отбрасывает newest envelope
при полном `stickyBroadcast` (`internal/server/hub.go:576-600`).

Если v1 уже стоит в полном channel, v2 обновляет map, но не попадает в channel.
Run затем правильно подавляет v1 как superseded. Поздний client получит v2 из
map, но **уже подключённый** client не получит ни v1, ни v2. Существующий тест
сам конструирует именно dropped v2, но проверяет только клиента, регистрируемого
после обновления (`internal/server/p547_sticky_update_notice_test.go:300-352`).
Сейчас sticky используется только для редкого update notice, поэтому это не
release blocker, но комментарий «one path or the other — never neither» шире
фактической гарантии.

Надёжная модель — coalescing wakeup: channel сообщает лишь «есть обновление
type», а Hub при dequeue читает актуальный envelope из map. Тогда полный channel
не теряет newest state для уже подключённых клиентов.

### P2-2. MCP tools остаются осознанным исключением из config snapshot

`ff9efec1` честно сузил обещание: prompt/options/обычные tools строятся из pinned
config, но MCP implementations по-прежнему читаются из live registry
(`internal/agent/coordinator_tools.go:170-198,303-308`,
`internal/agent/tools/mcp-tools.go:23-43`). Это исправление документации, не
исправление torn-generation риска. Reload посередине build всё ещё может
соединить snapshot N с MCP registry N+1. Для текущего релиза риск допустимо
оставить P2, если exception явно принят, но claim «one agent from one generation»
не должен распространяться на MCP.

### P2-3. Durable registry защищён от torn write, но не полностью от power loss

Новая запись registry использует same-directory temp, `Sync`, close и atomic
rename (`internal/session/childgroup_registry_unix.go:247-325`), что закрывает
главную гонку внешнего `SIGKILL` с truncate. После rename, однако, directory не
fsync-ится. Это не влияет на обычное process kill, но при power loss rename
может не быть durable на некоторых filesystems. Если слово «durable» включает
machine crash, нужен fsync parent directory либо явно документированный более
узкий контракт «process-crash safe».

## Что из предыдущего review действительно закрыто

| Предыдущее замечание | Статический результат на `d3ee9841` |
|---|---|
| Admission ABA между отказом и повторным lookup | Закрыто: `admitSession` атомарно возвращает конкретную observed entry (`run_queue_admission.go:127-154`), drain ждёт именно её (`run_queue_drain_session.go:166-210`). |
| Partial `success/error -> busy` возвращает success | Закрыто: обе ветки сохраняют ошибку либо возвращают `ErrDrainIncomplete` (`run_queue_drain_session.go:223-245,396-427`), app проверяет `drainErr`. |
| Shutdown/unknown outbox errors ошибочно permanent | Закрыто: cancellation pump context не считается, permanent положительно распознаётся только как SQLite constraint (`run_queue_orphan_drain.go:93-137,204-276`). |
| Edit активного partial assistant | Закрыто для edit и part mutation server-side; найден соседний delete gap P1-1. |
| Queued stale sticky envelope побеждает newest map value | Закрыто sequence check; остаётся delivery gap при full channel P2-1. |
| Unix registry torn truncate и потеря entry после kill error | Закрыто atomic temp/rename и retained rewrite; найден более глубокий owner-generation race P0-2. |
| MCP входит в общий pinned snapshot | Не закрыто по дизайну; комментарий теперь корректно описывает исключение. |

## Deadlock и freeze assessment

В новых изменениях не найдено обратного порядка mutex-ов или канального цикла,
образующего классический взаимный deadlock:

- `admissionEntry.err` записывается до `close(done)`, поэтому waiter имеет
  happens-before для конкретной entry;
- `inFlightMu` не удерживается во время ожидания `done` или выполнения turn;
- Hub отпускает `stickyMu` до fan-out client-ам;
- Unix registry не удерживает process-local mutex во время `killpg`;
- outbox операции имеют deadlines и реагируют на pump cancellation.

Но отсутствие mutex-cycle не означает freeze-free систему:

- P0-1 возвращает ложный успех после частичной потери work; очередь/сессия затем
  выглядит «застывшей» до нового owner/tick;
- P0-2 способен оборвать provider нового owner-а уже после успешного session
  acquire;
- P1-1 оставляет streaming turn живым после удаления его persisted message;
- watchdog/cancellation остаются кооперативными: произвольный provider, MCP или
  tool, игнорирующий context, нельзя принудительно заставить вернуть управление;
- at-least-once queue принципиально допускает повтор внешнего side effect после
  Ack failure/lease loss — видимость ошибок стала лучше, но transactional
  boundary вокруг model/tool side effects отсутствует.

Итого: **явного нового deadlock нет, но гарантия корректного и конечного
завершения сессии ещё не достигнута.**

## Общий обзор качества кода

Сильные стороны:

- ownership всё чаще выражается явными admission entries, lease owner,
  generation и process start-time tokens вместо timing assumptions;
- DB-side fencing и atomic transactions заметно укрепили persistence paths;
- последние tests по названиям и структуре детерминированно фиксируют negative
  outcomes, а не полагаются на sleeps;
- комментарии хорошо сохраняют forensic context и причины нетривиальных
  решений.

Системные запахи и источники новых регрессий:

- `run_queue_drain_session.go` уже 689 строк, `agent_turn.go` — 1880,
  `childgroup_registry_unix.go` — 595, `hub.go` — 618. Критические state machines
  всё ещё кодируются named returns, несколькими bool/error значениями, closures
  и длинными комментариями вместо исчерпывающих sum-like outcome types;
- P0-1 — прямой результат двусмысленного accumulator-а: `drained` означает
  одновременно «хоть что-то запускалось» и используется caller-ом как «всё
  нужное успешно закончено», а `err` одновременно означает outcome текущей row
  и всей drain operation;
- generation применяется как правильная идея, но иногда читается слишком
  поздно. P0-2 показывает общее правило: identity объекта, над которым началась
  destructive операция, должна захватываться до освобождения ownership и
  передаваться вниз неизменяемым token-ом;
- гарантии распределены между production comments и task-number tests. После
  сотен быстрых исправлений комментарии местами длиннее кода, но всё равно не
  образуют единую спецификацию переходов. Нужен короткий стабильный design doc
  со state diagrams для admission/drain, session lock generation и
  orphan/outbox lifecycle;
- 312 коммитов за две недели, многие из которых исправляют предыдущие
  concurrency fixes, — сигнал слишком высокой churn-зоны перед stable release.
  После закрытия blocker-ов полезен feature freeze для session/agent lifecycle,
  а не ещё один широкий рефакторинг.

## Рекомендуемый release gate

1. Исправить P0-1: row-aware typed drain aggregate; не позволять success другой
   row очищать terminal/Ack/lease/unconfirmed failure.
2. Исправить P0-2: capture victim generation до kill, acquire OS lock до sweep,
   удерживать его через registry read/kill/rewrite; никогда не выбирать цель по
   generation, появившемуся после смерти holder-а.
3. Закрыть P1-1 server-side CAS для delete активного assistant message и
   проверять rows affected у terminal update; синхронно убрать destructive UI
   controls во время stream.
4. Добавить детерминированные regression scenarios, перечисленные в P0-1/P0-2,
   и already-connected client для full sticky channel.
5. Только после исправлений выполнить отдельный динамический gate: targeted
   tests, `go test -race` для session/agent/cmd/server, multi-process
   inject/drain stress, repeated shutdown/outbox, реальный Unix process-tree
   replacement race и web e2e. В рамках этого review ничего из этого не
   запускалось.

До закрытия пунктов 1–3 стабильный релиз не рекомендуется.
