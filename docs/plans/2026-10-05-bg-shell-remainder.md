# R-BG-1, остаток: переход строки bg_shell и `job_kill` по shell_id

Дата: 2026-10-05. Статус: дизайн (консультация @om по коду main 093efb3c), к реализации — шаги 1 и 2.
Связано: `docs/plans/2026-10-01-bg-shell-ledger.md` (§7), `docs/async-invariants.md` (ASYNC-13, DUR-1, DUR-4),
коммит faa2101f (#1126).

## Вывод

Реальные проблемы остатка не там, где их ждали. Удержание завершения (`PendingCompletionsOwned`) не может
«залипнуть» после падения процесса: оно живёт в памяти и умирает вместе с процессом, а строка `running` на
мёртвом хосте recovery переводит в `interrupted`. Расходится обратное: строка `bg_shell` остаётся `running`
при ЖИВОМ хосте, хотя процесс давно завершён, — регрессия R-BG-1, P1 (область сессии не закрывается, делегация
и `rush run` зависают). Второй P1: `job_kill` по shell_id для процесса, который не умирает.

## Факты по коду (main 093efb3c)

«Одна правда» реализована не полностью: итог и долг пишутся двумя коммитами.
- `coordinator_bgshell_cap.go:84` `transitionBGShellRow` пишет строку терминальной (`delivery=done`, `reacted=1`,
  `:145-153`), затем `:92` — отдельный `InsertSessionNoticeReturningID(bg_shell_done, wake=1)`. Ключ капы — id уведомления
  (`:85-91`). ASYNC-13 закрепляет этот гибрид: wake-долг остаётся на `session_notices`.
- Удержание берётся при регистрации колбэка (`background_shell.go:186`), снимается в `MarkCompletionRecorded`
  (`coordinator_background.go:136`), при выходе последнего колбэка (`background_shell.go:193-196`) и по сроку
  `completionHoldMax` = 10 мин (`background_completion.go:23,77`); читается в одном месте —
  `work_ledger_delegation.go:236`.
- Единственный переводчик строки `bg_shell` в терминальное состояние — `transitionBGShellRow` из
  `persistBGShellCompletion`, вызываемый только колбэком `OnDone` → `notifyBackgroundJobDone`.
- Колбэк не регистрируется при `notify_on_background_job_done: false` (`coordinator_tools.go:683-693`,
  `bash.go:255,379`), а claim строки от флага не зависит (`async_tool.go:345-347`, `bgshell_claim.go:47`).
- `KillOwned` (`background.go:582-589`) по таймауту контекста возвращается, а `done` остаётся открытым
  (процесс-наследник держит pipe, случай R8B-2): `OnDone` не срабатывает никогда.
- Ошибка БД в `transitionBGShellRow` (`:141-158`) возвращает false без повтора: строка остаётся `running`.
- `running`-строка на живом хосте держит область открытой: `runningWorkOpen`
  (`coordinator_reaction_source.go:298-301`) → `CLIScope` (`:233`), `childScopeOpenAcrossProcesses`
  (`work_ledger_delegation.go:255,277`); recovery (`async_job_recovery.go:97-103`) трогает только мёртвые хосты.
- `job_kill` по голому shell_id (`tools/job_kill.go:98,153,169`): регистр не трогается, `KillOwned` убивает процесс,
  `ExitCode`=1 (`shell.go:274`) → `OnDone` → строка `failed` («finished: exit 1») и уведомление с `wake=1` — лишний
  платный автоход. Обычный `job_kill` для сравнения: `cancelled`/`job_kill`/`done`/`wake=0`/`reacted=1`
  (`work_ledger_transition.go:107-109`). `job_kill(job_id=<shell id>)` для `running` bg_shell отвечает ложным
  «job not found» (`work_ledger.go:618`, `work_ledger_pending.go:24`).

## Вердикты

| Сценарий | Итог | Класс |
|---|---|---|
| Падение процесса / рестарт хоста | удержания умирают с процессом, строка → `interrupted`; расхождения нет | — |
| Падение между `:84` и `:92`; ошибка вставки уведомления после выигранного `Transition` | итог терминален, уведомления нет, модель его не увидит (до R-BG-1 терялось всё) | P2 → backlog R-BG-2 |
| Два процесса на общей БД в окне `:84`–`:92` | не воспроизведено | P3 |
| `notify_on_background_job_done:false` + фоновый bash в Drain-ходе | строка `running` навсегда при живом хосте; `rush run` ждёт до потолка 6 ч | **P1, шаг 1** |
| Ошибка БД в `transitionBGShellRow` | то же вечное `running` | закрывается шагом 1 |
| `job_kill` по shell_id, процесс умер | `failed` вместо `cancelled` + лишнее пробуждение (так было и до R-BG-1) | P2, шаг 2 |
| `job_kill` по shell_id, процесс не умирает (R8B-2) | `OnDone` не срабатывает, строка `running` навсегда; раньше удержание истекало через 10 мин | **P1, шаг 2** |

Удержание нельзя вывести из строки, пока `Transition` и вставка уведомления не пойдут одной SQLite-транзакцией:
для shell'ов со строкой оно нужно только в окне между `:84` (`reacted=1`) и `:92` (уведомление).

## Шаг 1. Переход строки не зависит от уведомления (P1)

- `bgshell_claim.go`: после успешного `ClaimShell` взять shell через `manager.GetOwned` и повесить
  `sh.OnDone(func(){ c.finishBGShellRow(...) })` — единственный писатель итога; тот же `Transition`, что в
  `transitionBGShellRow`, с ограниченным повтором (как `retryAsyncStoreOp`). Shell не найден (вычищен `Cleanup`) →
  сразу `Transition(failed, "output unavailable")`: строка не должна остаться `running`.
- `coordinator_bgshell_cap.go:84`: убрать вызов `transitionBGShellRow` из `persistBGShellCompletion`; уведомление и
  решение о слоте капы не меняются. Окно «Transition выиграл, уведомления ещё нет» по-прежнему закрыто удержанием.
- Законы: ASYNC-13 — уточнить якорь писателя (наблюдатель, регистрируемый claim'ом, независимо от
  `NotifyOnBackgroundJobDone`). DUR-1 без изменений. Миграция не нужна.
- Тесты: (1) `internal/agent/bgshell_notify_off_test.go`: notify off, sync bash с `run_in_background` на `sleep` →
  строка `completed`, `CLIScope.WorkOpen=false`, уведомлений нет; revert-check: убрать регистрацию наблюдателя →
  строка `running`. (2) `internal/app/app_run_bgshell_notify_off_test.go` по образцу `app_run_job_kill_by_id_test.go`:
  настоящий цикл `rush run`, ребёнок в wake-ходе запускает фон, notify off → цикл завершается, делегация освобождена;
  revert-check тот же.
- Объём ~60 строк кода, ~220 тестов. Риск: два колбэка `OnDone` на одном shell (удержание считается по `onDoneCount`).

## Шаг 2. `job_kill` по shell_id для строки bg_shell (P1 R8B-2 + P2 текст/пробуждение)

- Новый файл `internal/agent/work_ledger_bgshell_kill.go`: `StopBackgroundShellRow(owner, shellID) (text, claimID,
  verdict JobStopVerdict)`: `store.Get`; не `bg_shell` → `JobStopNotFound` (дальше старый путь). `running`: снимок
  частичного вывода (`GetOwned` + `GetOutput`), затем `Transition(cancelled, notice_kind=job_kill, delivery=done,
  wake=0, reacted=1, ClaimID=row.ClaimID)` — тот же контракт, что `causeJobKill`; нового писателя `reacted` нет
  (DUR-4). Проигрыш CAS → `JobStopAlreadyTerminal` с текстом из `result_summary`.
- `tools/job_kill.go`: опциональный интерфейс `BGShellStopper` (type assertion), вызывается до `GetOwned`, когда задан
  `shell_id`, и как запасной путь для `job_id`, на который `ResolveJobShellID` ответил «not found». В метаданные —
  `JobID=shellID` и `KilledClaimID`: склейка и re-pend (`work_ledger_announce.go:258`) работают без изменений.
  Kill идёт ПОСЛЕ перехода: R8B-2 закрыт по построению (строка терминальна, даже если процесс жив).
- `persistBGShellCompletion`: если строка уже `cancelled`, уведомление и слот не пишутся; остаётся только
  `MarkCompletionRecorded` и `noteSubAgentChildRunEnded`.
- Законы: ASYNC-13 — «job_kill по shell_id → `cancelled` по запросу, а не по смерти процесса; несклеенный результат
  re-pend'ится и pull'ится (R2A-8)»; DUR-4 — в якорь третьего писателя добавить `bg_shell`. Миграция не нужна.
- Тесты: (1) kill по shell_id → строка `cancelled`/`job_kill`, `reacted=1`, `bg_shell_done` нет; revert-check: убрать
  ветку в `job_kill` → `failed` + уведомление. (2) shell с незакрывающимся `done` + kill → `childScopeDrained=true`
  сразу (приём из `background_completion_test.go:228-242`); revert-check: строка `running`. (3)
  `internal/app/app_run_bgshell_kill_test.go`: kill по shell_id в цикле `rush run` → без лишнего хода, цикл выходит.
  (4) `tools`: `job_id=<shell id>` больше не отвечает «not found»; revert-check: убрать запасной путь.
- Объём ~130 строк кода, ~350 тестов. Риск: pull должен корректно строить текст для re-pend'нутой строки kind
  `bg_shell`; гонка «kill против естественного завершения» решается CAS.

Порядок: шаг 1 с тестами (самодостаточен) → шаг 2 с тестами (опирается на шаг 1: уведомитель больше не пишет
переход). Правки `docs/async-invariants.md` — в том же коммите, что и каждый шаг.

## Backlog (не делать в этом цикле)

- **R-BG-2**: `Transition` строки bg_shell и вставка `bg_shell_done` одной транзакцией (метод store через `WithTx`);
  затем удалить `background_completion.go` и `work_ledger_delegation.go:236`. Закрывает P2 «падение или ошибка между
  `:84` и `:92`».
- `job_kill` по shell_id для path-1 (async bash): маршрутизировать в `MarkJobStopped` владеющего задания (сейчас итог
  `failed`, а не `cancelled`). P2.
- Известное ограничение до R-BG-2: пробуждение по shell'у со строкой теряется, если процесс упал или вставка
  уведомления не удалась после коммита перехода; итог при этом виден в `sessions jobs`.
