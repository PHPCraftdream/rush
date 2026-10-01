# Предохранитель «цепочка реакций без прогресса» (#1113)

Статус: дизайн утверждён (консультация `ox`, 2026-10-01); реализация по этому документу.

## Проблема

Агент, ждущий результат async-команды, «ждёт» через `sleep 90`, `echo tick`,
`echo tick2`… Каждая такая команда асинхронна (и в CLI, и в web), её
завершение создаёт уведомление-долг реакции с `wake`, Drain запускает ход, модель
снова запускает `echo`. Наблюдалось ~140 пустых ходов примерно каждые 5 с.
Drain-gate (`work_ledger_reaction.go`, `freeStreak`/`paidStreak`) считает только
отказы и «платные, но не закрывшие долг» попытки; успешная реакция закрывает
долг и выглядит здоровой. `loop_detection` молчит (команды различаются). В web
`claimAutoResume` (5 на сообщение человека) стоит только на `bg_shell_done`.
Мягкая мера (тексты описаний `bash`/`run_command`, #1109) уже внесена; нужен
жёсткий предохранитель.

## Решения

### 1. Где считать
- Состояние на сессию в процессе, решение — в предикате запуска: новая ветка
  `drainPolicy` (её читают все запускатели: `wakeSession`, release hook, 60-секундный
  проход, `CLIScope`, `decideDrainTurn` у Drain из очереди). Паритет web/CLI и
  закон ASYNC-09 сохраняются.
- Счёт ведётся в `accountDrainAttempt` (единая точка учёта ноги, до release).
- Хранить в координаторе рядом с `bgShellOverCap`/`autoTurnsSuspended` (под
  `autoResumeMu`), отдельным файлом по образцу `coordinator_bgshell_cap.go`, а
  НЕ в `drainGate`/`sessionJobs`: успешная реакция вызывает `resetDrainGate`
  (`rows.open == 0`) и обнулила бы счётчик ровно тем событием, которое маскирует
  цепочку; `sweepIdleSessionsAt` удаляет запись без jobs с открытым гейтом за
  ≤60 с. Чистка — `resetConsecutiveResume` и `sweepDeletedSessionState`.
- `cliLoop` только отображает результат.

### 2. Что считать прогрессом
- Источник: `fantasy.StepResult` шагов ноги, собранные в
  `turnStream.recordStepHistory` (`ts.att`) — те же данные читает `loop_detection`;
  одинаковы для web, CLI и ребёнка, без чтения БД.
- Классифицировать команду, не имя инструмента (в CLI/web каждый `bash`
  асинхронный). «Холостой запуск» — `bash`, чья команда разбирается парсером
  mvdan `syntax` (как в `shell.commandCalls`) только в литеральные вызовы
  `sleep|echo|printf|true|:`, без редиректов и подстановок; `;` и `&&` между
  ними допустимы. Для `run_command` — program `sleep`/`timeout`. Нейтральны:
  текст, `job_output`, `todos`. Прогресс: edit/write/view/grep, `bash` с реальной
  командой, `agent`/`agentic_fetch`, `job_kill`.
- Текст ответа прогрессом не считается (модель в холостом цикле всегда пишет
  «жду…»). Ход без инструментов обрывает цепочку сам.
- Ход, который и запустил холостую команду, и сделал действие, — прогресс
  (счётчик сбрасывается).
- Звено — нога с ≥1 холостым запуском и без прогресса. Счёт только по
  «самопитающейся» цепочке:
  - звено, чей снимок долга целиком состоит из завершений прошлых холостых
    запусков → `count++`;
  - звено в ответ на другой факт (завершение W, supervision, результат ребёнка,
    `sleep` из хода человека) → `count = 1`;
  - прогресс → `0` и очистка набора id;
  - сообщение человека (`resetConsecutiveResume`) → `0`;
  - неудачная нога, непопытка, ход только текстом → без изменений.
- Ключ набора — `claim_id` из `ClientMetadata` ответа started (тот же тег, что
  у ack gate), не `tool_call_id` (провайдеры переиспользуют id, ASYNC-01).

### 3. N = 3 и поведение на N+1
- `drainPolicy` возвращает `drainDeferred("reaction chain without progress")`,
  если `count >= 3` и каждая pending-inclusive строка долга — claim из набора
  (паттерн `bgShellCapDeferred`; `PendingInclusiveDebtRows` фильтрует job-строки
  в Go, достаточно вернуть их claim-id; SQL не меняется). Любая другая строка в
  долге → Allow. Ветку ставить ДО шортката «running delegation → allow»
  (R6B-1 освобождает ребёнка от политики авто-возобновления, не от
  предохранителя).
- CLI: W бежит → существующая ветка WorkOpen ждёт W; ничего не бежит →
  существующая ветка Deferred, выход. `exit_reason` не трогать (публичный
  контракт обёрток). В `warnings` конверта: «reaction chain stopped: 3 automatic
  turns only ran sleep/echo; their results stay for the next turn». В stderr —
  одна строка в момент срабатывания. Признак передавать типизированным полем
  `CLIScopeState`, не сравнением строки `Reason`.
- Web: ход не стартует, один `slog.Warn`; при срабатывании вставляется один
  маркер-notice с `wake=0` (новый kind), по DUR-3 он попадает в историю со
  следующим ходом; текст маркера советует: «чтобы ждать — заверши ход; ждать и
  проверить — одной командой `sleep N && check`». `sessions why` показывает
  Deferred с причиной. Kind маркера добавить к `wake_failed` в запросах Rerun
  (`RependSessionNoticesByMessageIDs`/`VoidWakeFailedNoticesByMessageIDs`).
- Законы: нового состояния нет (существующий Deferred); строки остаются
  `wake=1/reacted=0`; по DUR-4 новых писателей `reacted` нет; K не движется
  (`wake_attempts` не растёт); по ASYNC-02 Deferred закрывает цикл только когда
  ничего не бежит. Рестарт web теряет память: ещё ≤N звеньев и повторное
  срабатывание.
- Обязательно: `pullPendingNotices` не вызывает `recordProgress` за завершения из
  набора (иначе Drain по тику supervision сбрасывает backoff и пауза после
  6 тиков не наступает).

### 4. Ложные срабатывания
Работа через bash снимается классификацией команды. Поллинг вида
`sleep 90; gh run view` содержит реальную команду и не считается. Проверка после
ожидания сбрасывает счёт. `sleep 5` + `go test` в одной ноге — прогресс. По
замыслу режутся только ≥3 подряд чистых ожидания без проверки.
Вне объёма (P2, backlog): цепочка внутри одной ноги (предикат между ходами её не
видит — задача `loop_detection`); короткий `wakein` после этапа 4b.

## Тесты (revert-check в скобках)
1. `internal/shell`, таблица `IsNoOpCommand`. true: `sleep 90`, `echo tick2`,
   `sleep 5 && echo done`, `true`. false: `sleep 90; gh run view`, `echo x > f`,
   `echo $(date)`, `sleep $N`, `cat f`. (Пропускать редиректы → падает `echo x > f`.)
2. agent, фейковый store: 3 холостые ноги через `accountDrainAttempt` → `CLIScope`
   = Deferred при долге только от 3-го звена; с добавленной строкой W → Owed.
   (Убрать проверку «каждая строка в наборе» → падает кейс с W; убрать `count++`
   → Deferred не наступает.)
3. Успешная реакция (`rows.open == 0`) не обнуляет счёт — наблюдённый баг.
   (Перенести счётчик в `drainGate` → падает.)
4. Сбросы: echo+edit и bash `cat f` дают 0; текст+echo считается звеном;
   `ResetAutoResumeCounter` даёт Owed. (Считать текст прогрессом → падает.)
5. После `sweepIdleSessionsAt(now+2m)` без jobs — по-прежнему Deferred.
   (Хранить в `sessionJobs` → падает.)
6. app e2e со скриптованным провайдером: первый ход — удерживаемый W и
   `echo tick`, каждый Drain — `echo tickN`. Ровно 3 Drain, одна stderr-строка,
   цикл ждёт W; после отпускания W — 1 Drain, текст, выход; warning в конверте,
   `exit_reason` от последнего хода. Вариант без W: выход после 3 Drain, долг
   остаётся. (Выключить ветку → скрипт падает на 5-м вызове провайдера.)
7. Паритет web: persistent-координатор, только `wakeSession` — остановка после 3.
   Drain по supervision, подтянувший звено, не сбрасывает `tickCount`. (Убрать
   исключение в `pullPendingNotices` → падает.)

## Файлы
Новые: `internal/shell/noop_command.go`, `internal/agent/coordinator_reaction_chain.go`
(+ тесты). Правки: `internal/agent/agent_turn_step.go` (`recordStepHistory` пишет в
`att`), `drain_attempt.go`, `coordinator_drain_policy.go`, `coordinator.go`,
`coordinator_session_state_sweep.go`, `coordinator_reaction_source.go`,
`agent_notice_pull.go`, `coordinator_bgshell_cap.go`; `internal/session/notice_row_ids.go`,
`internal/session/notice_pull.go`, `internal/db/sql/session_notices.sql` (+ sqlc);
`internal/app/app_run_async.go`; `docs/async-invariants.md` (ASYNC-09, DUR-4, ASYNC-02).
