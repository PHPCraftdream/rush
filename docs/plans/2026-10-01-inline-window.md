# Окно немедленного ответа для коротких CLI/web-команд (A14, задача #1131)

Статус: решение архитектора (@oxx, 2026-10-01); реализация — после R-ARB-2
(та же зона `internal/agent`).

## Проблема

В CLI и web каждый `bash`/`run_command` оборачивается `asyncTool`
(`internal/agent/async_tool.go`) и сразу отвечает «Async bash job <id> started»,
даже 50-мс `grep`; вывод приходит позже отдельным уведомлением. Агент, читающий
код, делает сотни мелких команд: одна сессия — 892 сообщения / 204 async-задачи,
каждая команда — лишний круг, результаты приходят не по порядку. Внутренний
`bash` уже возвращает команду, завершившуюся за 1 с, сразу
(`internal/agent/tools/bash.go`, фастпас), но обёртка отвечает «started» раньше.

## Решение: делать, окно N = 3 с

Состояние строки пишет только нынешний `transition` исполнителя (DUR-1);
ack-gate лишь переводит доставку `pending→done` одной транзакцией с
tool-result, несущим результат — та же форма, что pull (DUR-3) и job_kill
(DUR-11). Отвергнуто: сплавить терминальный переход с ack (второй писатель
`state`, окно «результат только в памяти»).

### Контракт строки (две транзакции)
- **Tx1, без изменений** (`finish → commitTransition(causeNaturalFinish)`):
  `state ∈ {completed, failed}`, result, `delivery='pending'`, `wake=1`,
  `reacted=0`, `announced=0`. Строку никто не видит: pull и долг требуют
  `announced=1`, `deliverLocked` держит по `!announced` — ни подсказки, ни Drain.
- **Решение inline** — под `l.mu` по новому `job.settled` (закрывается в
  `transitionToTerminal`, после коммита Tx1). Условие: job текущий, фаза
  `phaseCompleted|phaseFailed`, `!announced && !acking && !stoppedBySession &&
  !killRequested`. Таймер N, `ctx.Done`, `closedCh` или другая фаза → ответ
  «started», ровно как сегодня.
- **Tx2, ack-gate** — новый `AnnounceInlineResult` рядом с `AnnounceStarted`
  (`internal/session/notice_pull.go`): INSERT tool-result + `announced=1`,
  `announce_message_id=msg`; затем CAS `WHERE claim_id=? AND state<>'running'
  AND delivery='pending'` → `delivery='done'`, `notice_message_id=msg`,
  `wake=0`, `reacted=1` (как у job_kill), `notice_kind` не трогать.
  `ErrAsyncJobGone` → откат + обычный Create (как B15); 0 строк в CAS (только
  void от Rerun) → коммит announce-половины и обычный хвост; иная ошибка →
  `abort`, как сегодня.
- **Хвост в памяти**: `finishInlineLocally` = `deliverLocked` без `onWebDone`
  (без подсказки и `recheckChild`).

### События в окне — везде «started» по сегодняшнему пути
Stop (`ctx`/`stoppedBySession`, нотис cancelled, wake=0); job_kill (модель ещё
не знает id; если случится — `phaseCancelled`); новый `timeout{}` (≥5 с) в окно
не попадает, legacy `run_command.timeout_seconds` 1–2 → `phaseTimedOut` +
timeout-нотис; shutdown (`closedCh`, латч оставляет строку running).

### Краш
До Tx1: running/announced=0 → удаляется (ASYNC-05); между Tx1 и Tx2:
терминальная announced=0 → удаляется (R2A-7); после Tx2 восстанавливать нечего
(`RependJobKillRows*` не совпадает — `notice_message_id` задан). Висящий
tool_use закрывает `syntheticToolResultsForOrphanedCalls`. Rerun: оба id
указывают на одно сообщение, void побеждает re-pend.

### N и область
N = 3 с — фиксированная константа, только `bash` и `run_command`;
`agent`/`agentic_fetch` — никогда (финал через `armDelegation`). SDK/sync не
меняется. Нижняя граница — фастпас `bash` (фиксированный `time.Sleep(1s)` +
старт процесса), верхняя — assert `N < timeoutSecondsFloor` (5 с), тогда новый
таймаут в окне структурно невозможен. Не настройка; `coordinator.inlineWindow`
(0 = выключено) — только тестовый шов. Отдельно, вне законов: заменить
`time.Sleep(1s)` в `bash.go` на `WaitContext` ≤ 1 с (иначе каждый 50-мс grep
стоит 1 с).

### Открытая работа и долг — никогда после Tx2
Строка не running; `wake=0` — не долг и не в `DebtSnapshot`; `done` — не в
pull; подсказки нет. `CLIScope` оценивается между ходами, окно — внутри хода.
Арбитр (`turn_arbiter.go`): правило 1 (`!PendingIncl`). Обязательные смежные
правки: `noteWorkStarted` только для «started», после окна (иначе inline-grep
снимает паузу надзора, `supervision.go`); `recordChainEvidence`
(`agent_turn_step.go`): у inline-метаданных нет `async`, inline `sleep 2`
засчитался бы прогрессом цепочки #1113 — ветка «inline idle = idle, без claim».

### Форма реализации
`async_tool.go`: `launchExecutor` заканчивается на `go t.run` (recover B15 не
накрывает окно), затем `awaitInline` → inline-ответ
`asyncToolMetadata{Async:false, Inline:true, JobID, ClaimID, Status}` или
`noteWorkStarted` + `startedResponse`; `Info()` — «завершилось за 3 с —
результат сразу». Новый файл `internal/agent/work_ledger_inline.go`
(`awaitInline`, `finishInlineLocally`; `work_ledger.go` уже ~974 строки).
`ackTag.Inline`; `claimAck` отдаёт тег; ветка в `persistToolResult`;
sqlc-запрос `DeliverAsyncJobInline` в `internal/db/sql/async_jobs.sql`.
`async:false` обязателен: иначе `isPendingJob`
(`web/src/components/Message/ActionRow.tsx`) вечно показывает «running…».

### Тесты (реальная SQLite, у каждого revert-check)
1. Быстрый job: ответ = вывод; после `persistToolResult` строка
   completed / announced=1 / done / wake=0 / reacted=1, notice_id = announce_id;
   `PullJobNotices` пуст, долга нет, `onWebDone` не вызван. Revert: inline-тег →
   `AnnounceStarted` даёт нотис и долг.
2. Атомарность `AnnounceInlineResult`: триггер валит UPDATE доставки → нет
   сообщения, announced=0. Revert: два коммита.
3. Медленный job (окно 50 мс) → «started», ровно один нотис.
4. Таблица не-natural исходов (Stop в окне, legacy timeout 1 с, отмена ctx,
   `close()`) → «started» + сегодняшний нотис.
5. Надзор на паузе не снимается; inline `sleep 1` ≠ прогресс цепочки.
6. Rerun, удаливший inline-результат → void; dead-host sweep строку не трогает.
7. e2e `internal/app`: `rush run` с `echo hi` → 2 вызова провайдера, ни Drain,
   ни нотиса. Revert: окно 0 → 3 вызова.
8. Web-юнит: inline-метаданные не считаются pending.

### Риски
- Каждый долгий запуск получает «started» на ≤3 с позже; если fantasy исполняет
  tool calls шага последовательно, K долгих запусков = K×3 с (проверить).
- Stop в окне: если fantasy после отмены не вызывает OnToolResult, job
  осиротеет в памяти (держит `running()`, место в лимите 50) — закрепить тестом
  через настоящий turnStream до мержа.
- e2e в `internal/app`, где быстрый CLI bash служит «async-работой», перевести
  на шов окна 0, иначе они молча станут inline.
- Реестр `docs/async-invariants.md`: переякорить ASYNC-04, DUR-4 (четвёртый
  писатель `reacted=1`), DUR-7, DUR-11; CHANGELOG — модели увидят смену
  поведения.

Отвергнутые альтернативы: вернуть CLI/web на родной sync+auto-background bash
(две схемы доставки, нет job_kill по id, timeout и recovery — откат фазы 2);
sync по `isSafeReadOnlyCommand` (эвристика; медленный `grep -r` блокирует ход).
