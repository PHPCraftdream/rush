# Вопрос воркера при открытой области ребёнка: будить оркестратора, принимать ответ (#1157, A30)

Статус: дизайн (код не менялся). Улики: `.rush/audit/sessions/wskills.db` (read-only),
`.rush/audit/rush_quality.md` A30, `.rush/tasks/1157-worker-ask-question-deadlock.md`.

## 0. Наблюдения (wskills.db)

| Время | Факт |
|---|---|
| 23:56:32 | Корень `wskills` (CLI): `agent` → «Async agent job call_32fb… started». Строка `async_jobs(wskills, call_32fb…)` = running. |
| 23:56–00:12 | Ребёнок `4751f786-…$$call_32fb…` — ОДИН ход (уведомления подтягиваются на границах шагов), 145 задач: `sleep 295` с `timeout{wake_only}`, `go test` без heavy.sh. |
| 00:11:45–57 | 3× «maximum number of async jobs (50) reached» (A27, #1155) → `ask_question` («очередь полна: ждать или отчитаться?»). |
| 00:12:02 | finish `error` «Stopped: agent asked a question…»; ПОСЛЕ него — tool-сообщение с результатом `ask_question` (`is_error`, finish `stop`) — последнее сообщение ребёнка. |
| на 00:12:02 | У ребёнка running 47: 42 таймера (кончились к 00:16:40 → completed, `delivery=pending, wake=1, reacted=0` — долг, отложенный суспензией) и 5 `go test`, висевших до смерти процесса (01:10:07 → interrupted); их wake_only-дедлайны 00:07–00:55 прошли (3 `timeout_wake_only` pending у ребёнка). |
| 00:01 / 00:12 / 00:32 | Supervision корню: «0 background job(s) running, 1 sub-agent delegation(s) in progress … running 35m51s, last activity: Now mutant m2:» → «продолжаю ждать». Вопроса в тексте нет. |
| 01:10:08 | После kill/relaunch строка делегации → interrupted с «the sub-agent session finished with no textual response» — вопрос потерян и при восстановлении. |
| 01:10:17 | `resume_session_id "4751f786-…"` (короткий id) → not found: нужен полный `…$$call_32fb…`. |

Бинарь: установлен 03060c5c (10-01 14:02), без 342af913 (подсказка `ask_question` при своих задачах) — поэтому ход закрылся вопросом при 47 своих задачах.

## 1. Механизм (почему вопрос не дошёл)

**А. Информация.** Для паузы ребёнка при открытой области уведомления нет.
1. В CLI/web `agent` асинхронный: `async_tool.go:68` (sync только вне CLI/web), `:94` (job отвязан от хода), `:175`/`:225` «started… result will arrive as a new session message».
2. Ребёнок: `tools/ask_question.go:120-124` → `AskQuestionError` → `agent_turn.go:237-243` → `AwaitingAnswerError`; `drain_attempt.go:497-502` суспендирует его автоходы.
3. `coordinator_subagents.go:321-332`: `runSubAgent` превращает вопрос в УСПЕШНЫЙ ответ — но в async это лишь конец хода ребёнка.
4. `async_tool.go:336-338` finalize → `armDelegation` (`work_ledger_delegation.go:57-74`): вопрос ложится в `job.result` в памяти, строка X остаётся running.
5. Единственный путь к родителю — release в `recheckChild` (`work_ledger_delegation.go:98-178`), гейт `:115` `childScopeDrained`: `:213` `l.running(child)`, `:224` bg-шеллы, `:243`/`:270` `CLIScope.WorkOpen` (running-строки, `wakein`-расписания, `coordinator_reaction_source.go:233-254`). Отложенный ДОЛГ (вопрос) делегацию отпускает (`:253-255`, `TestChildScope_DeferredDebtReleasesDelegation`), живая РАБОТА — нет. Ни notice, ни wake родителю больше нигде не порождается.
6. Supervision (`supervision.go:400-431`) видит X как «running»; `capturePartialDelegation` (`work_ledger_timeout.go:317-338`) берёт последнее ассистентское сообщение С ТЕКСТОМ — у вопроса текста нет (он в tool_call и `finish.details`) → «Now mutant m2:» из давнего шага; совет `:423` «Keep waiting…».
7. `inspect_agent` тоже слеп: `childLastActivity` (`coordinator_agent_control.go:89-99`) читает `msgs[len-1]` = tool-сообщение с результатом `ask_question` → awaiting=false → `InspectAgent` `:112-123` отвечает `idle`. Тест `coordinator_agent_control_test.go:213-223` создаёт только ассистентское сообщение — не реальная раскладка.

**Б. Ответить нельзя.**
8. `agent resume_session_id=C` → `async_tool.go:96` Start → `AsyncJobStore.Claim` (`session/async_job_store.go:307-337`): индекс `idx_async_jobs_child_running` (ASYNC-01) → `ErrAsyncChildSessionBusy` «the sub-agent still has work running from a previous delegation; wait for its result» (`:55-56`). Это осознанное решение фазы 4 (durable-core.md §3.8: «resume при делегации, ход ребёнка которой закончен, а задачи идут → ошибка»), но вопрос в нём не учтён: результата не будет, пока не ответят.
9. `inject_agent` при idle-ребёнке (`coordinator_agent_control.go:183-186`) кладёт сообщение в очередь «nothing runs until you call agent with resume_session_id» — тот же отказ. Выход только `stop_agent` (убивает задачи ребёнка), модели не подсказан.

Итог: тупик длится, пока бежит хоть одна задача ребёнка; в wskills — 5 зависших `go test` → вечно.

**Почему класс открыт и на main.** 342af913 (`ask_question.go:112-118`) возвращает подсказку вместо остановки, но читатель своих задач подключён только к ctx `ExecuteRun` (`rush run`/SDK, `app/app_run_setup.go:131-137`) и к тому, что его наследует (первый ход делегации). Drain-ходы ребёнка стартуют из `wakeSession(context.Background())` (`coordinator_wake.go:27,73`; afterRelease, supervision, ledger) — читателя нет → `ok=false` → старая остановка. Web (`server/handlers_agent.go:255`) читателя не ставит. Открытую область держат и bg-шеллы, и `wakein`. Плюс вариант без вопроса: ребёнок послушался подсказки и ждёт свои зависшие задачи — родитель видит то же «running».

Побочное: восстановление (`session/async_job_recovery.go:249-268`, `recoveredDelegationText`) берёт `FullText` → вопрос без текста → «no textual response».

## 2. Контракт и состояние

Контракт (`templates/agent_tool.md:7`, `coder.md.tpl` правило 7, `cmd/claude_slash_command.yaml:162-170`): вопрос воркера приходит оркестратору как пауза; тот отвечает `agent(resume_session_id=…)` и продолжает внутри процесса.

**Состояние «делегация ждёт ответа»** — выводимое, без колонки:
X running ∧ X армирована (`byChild[C]`) ∧ ребёнок не занят ∧ последнее ЗАВЕРШЁННОЕ ассистентское сообщение C — question-stop (`subAgentQuestionFromFinish`) ∧ область C открыта.
X НЕ отпускается: область корня открыта (ASYNC-02), Stop транзитивен (ASYNC-08), вторая делегация не нужна (ASYNC-01). Один предикат `childAwaitingQuestion` (новый файл) читают: событие, путь ответа, supervision, `inspect_agent`.

**Событие `child_question`** — строка `session_notices` родителя: `wake=1`, `job_tool_call_id = X`. Void, если X уже не running/void — тот же случай, что `timeout_wake_only` (`session/notice_pull.go:262-275`): если X успели отпустить, вопрос несёт release, дубля нет.
- Точка: `recheckChild`, ветка «не осушена» → `noteChildQuestion(C)`. Её зовут все существующие триггеры: arm (вопрос в первом ходе), конец хода ребёнка (afterRelease — вопрос в Drain-ходе), завершения его задач, 60-с `RecheckPass` (`coordinator_recheck.go:92-94`, страховка).
- Фильтр до чтения БД (в памяти): `!IsSessionBusy(C) ∧ autoResumeSuspended(C) ∧ X.announced ∧ !X.questionNoticed`. `X.announced` — ASYNC-05 (неанонсированную X догонит 60-с проход).
- Дедуп: `X.questionNoticed` ставится под `l.mu` до вставки; сбрасывается путём ответа (новый вопрос → новое событие).
- Вставка + `wakeSession(parent, fact=true)` в горутине — как supervision (`supervision.go:362-366`). CLI-корень — внешний драйвер: hint → цикл видит долг → `CLIScope=Owed` → ход сразу; web/вложенный родитель — Drain; занятый родитель — забор на границе шага. Не по тику.
- Ошибка вставки: `slog.Error`, флаг сброшен, повтор — следующий триггер/60-с проход.

Текст (одна функция рядом с `subAgentQuestionFrame`, префикс тот же — модель его знает):
```
<преамбула ребёнка, если есть>

SUB-AGENT QUESTION (session <C полностью>): <вопрос>
Suggested options: …
The sub-agent is paused on this question. Its own <N> background job(s) are still
running, so job <X> stays open — waiting will not produce a result.
Answer: call `agent` with resume_session_id="<C>" and your answer as prompt; the
result then arrives with job <X>.
Give up: stop_agent(child_session_id="<C>") — this also stops its running jobs.
```

**Путь ответа** (снимает п.8): `asyncTool.Run` для `agent` с `resume_session_id`, origin CLI/web, ДО Start (`async_tool.go:96`) → `answerHeldDelegation(ctx, owner, C, prompt)`:
1. Под `l.mu`: X армирована у `(owner, C)`, running. Нет → не наш случай, обычный путь (Claim; при не-вопросе отказ фазы 4 остаётся).
2. Вне lock — предикат «ждёт ответа»; снова под `l.mu`: X всё ещё running → `X.answerHold=true`, `X.questionNoticed=false`. Иначе → обычный путь (X отпущена — Claim пройдёт).
3. Как у `runSubAgent`: `resetConsecutiveResume(C)` (`coordinator_subagents.go:87`), `clearCancelRequest` (`:91-93`), переподкрепить allowlist (`agent_drain.go:77-79`); `call = driver.callFor(prompt)`.
4. Горутина на `context.WithoutCancel(ctx)` (origin CLI/web сохраняется): `runAwaitingAdmission(agentFor(C), call)`; по возврату `answerHold=false`, `recheckChild(C)`.
5. Ответ инструмента сразу, без строки в ledger (`async:false`, `child_session_id`): «Answer delivered to sub-agent C (paused under job X); it is running again and its result arrives with job X. End your turn if nothing else is left.»
6. `recheckChild`: если у самой старой армированной X `answerHold` → return (без этого гонка «область осушилась между решением и допуском хода» отпустила бы X с вопросом, а ход ответа остался бы вне делегации).

Итог X после ответа — финальное сообщение ребёнка (`refreshSubAgentCompletion` уже читает последнее). Новой строки нет → ASYNC-01/04 не задеты.

## 3. Supervision check-in (`buildSupervisionSummary`, `supervision.go:385-432`)

- Заголовок: «…, N sub-agent delegation(s) in progress (K awaiting your answer)».
- X «ждёт ответа»: `- X (sub-agent, child session C): AWAITING YOUR ANSWER for 20m — it asked: <вопрос ≤300 симв.>. Its own 5 job(s) still run. Answer: agent(resume_session_id="C", prompt="…"); give up: stop_agent(child_session_id="C").`
- X с idle-ребёнком без вопроса, но с его открытой работой (вариант main): `sub-agent idle, waiting on its own N job(s); oldest: <tool> running 58m` вместо устаревшего «last activity».
- Совет: при K>0 первой строкой «A sub-agent is blocked on your answer; waiting will not progress it.», дальше прежний.
- Интервалы, backoff, пауза после 6 тиков — без изменений. Забор `child_question` — прогресс (`agent_notice_pull.go:54-64`), backoff сбрасывается; тик — лишь повтор вопроса для проигнорированного события.
- `inspect_agent`: `childLastActivity` на общем предикате (последнее завершённое ассистентское) → `awaiting_answer`.

## 4. Ограничения

- **ASYNC-01**: ответ не создаёт строку; индекс и Claim не трогаем. **ASYNC-02/08**: X при открытой области не отпускается. Отвергнуто «отпустить X с вопросом»: задачи ребёнка выпали бы из области корня (`treeSessionIDs` идёт только по running-делегациям) — `rush run` вышел бы при живых задачах, Stop не дошёл бы.
- **ASYNC-03/04**: терминальный переход X — только существующие release/cancel; hold лишь откладывает. `child_question` — notice, не задача; void исключает устаревшее. **ASYNC-05**: только после `X.announced`.
- **ASYNC-06/07**: ход родителя — только `wakeSession`; ход ребёнка — его драйвер (`agentFor`/`runAwaitingAdmission`), как в `runSubAgent`; маршрут одинаков для CLI/web/глубины. **ASYNC-09**: ошибка видна (лог + повтор + состояние в supervision/`inspect_agent`).
- **DUR-3/4/11**: один механизм забора; новых писателей `reacted` нет; новый kind — `TEXT` без CHECK, миграции нет.
- **R-ARB**: вердикты — существующий арбитр; правило 4 у родителя не срабатывает (суспендирован ребёнок); `resetConsecutiveResume` — существующий писатель.
- **#1113**: `child_question` — не claim холостого запуска → правило 5 не откладывает (`ChainOwnsAllDebt=false`, `turn_arbiter.go:223-227`), звено «на другой факт»; вызов `agent` — прогресс.
- **#1149**: файлы стража (`agent_turn_step.go`, `agent_turn_stream.go`, `step_progress.go`, `turn_progress_guard.go`) не трогаем; `agent` там — прогресс. Текст его отказа wait «завершение и так разбудит» для X, ждущей ответа, неточен — одна строка после его вливания, не зависимость. Правило 7 в `coder.md.tpl` не трогаем: #1149 вводит `prompt.OrchestratorRuleMarker`.
- wguardrail: перечисленные файлы — вне плана. `work_ledger.go` (994 строки) не трогаем.

## 5. План (3 шага; каждый — build/vet/gofmt + тесты пакетов через heavy.sh; revert-check в скобках)

**Шаг 1. Предикат + событие + `inspect_agent`.**
Новый `internal/agent/delegation_question.go` (`childAwaitingQuestion`, `noteChildQuestion`, текст); `work_job.go` (`questionNoticed`, `answerHold`); `work_ledger_delegation.go` (вызов в ветке «не осушена»); `coordinator_agent_control.go` (`childLastActivity` на предикате); `session/notice_pull.go` (`NoticeKindChildQuestion`, void как у wake_only). Переякорить ASYNC-02, словарь DUR-3.
Тесты на фейковом провайдере (`childDelegationBase`/`attemptFixture`: httptest-провайдер, реальный SQLite), новый `delegation_question_test.go`:
- T1 `TestChildQuestion_OpenScopeWakesParent`: ребёнок держит свою задачу без исполнителя (= зависший `go test`), ход ребёнка (`textThenAskQuestionResponse`, ctx без читателя своих задач — как Drain-ход на main) кончается вопросом, X армирована. ≤5 с: у родителя pending `child_question`, `wake=1`, `job_tool_call_id=X`, текст с вопросом, полным `resume_session_id="C"` и `stop_agent`; hint родителя вырос; X не доставлена, `running(parent)`. Сегодня строки нет — тупик воспроизведён. (Убрать вызов в `recheckChild` → красный.)
- T1b: после T1 задачу ребёнка завершить → X отпущена с вопросом; при заборе `child_question` void, ровно одно сообщение с «SUB-AGENT QUESTION». (Убрать void-кейс → два → красный.)
- T1c: три триггера подряд → одна строка. (Убрать флаг → три.)
- T1d `TestInspectAgent_AwaitingAfterQuestionToolResult`: раскладка wskills (ассистент с вопросом + tool-результат) → `awaiting_answer`; сегодня `idle`.

**Шаг 2. Путь ответа.**
`async_tool.go` (перехват до Start, ~8 строк), `delegation_question.go` (`answerHeldDelegation`), `work_ledger_delegation.go` (проверка `answerHold`). ASYNC-01 — пометка «ответ в удерживаемую делегацию строку не создаёт»; CHANGELOG.
- T2 `TestChildQuestion_ResumeAnswersHeldDelegation`: после T1 `asyncTool{name: agent}.Run` (origin CLI) с `resume_session_id=C, prompt="report now"` → не ошибка, текст называет X, внутренний инструмент не вызван; провайдер получил ход ребёнка с «report now»; в БД одна running-строка с `child_session_id=C`; после завершения задачи ребёнка X доставлена один раз с финальным текстом, не вопросом. Сегодня — `ErrAsyncChildSessionBusy`. (Снять перехват → красный.)
- T2b гонка: шов задерживает допуск хода ответа, задача ребёнка завершается в окне → X не отпущена до конца хода ответа. (Снять проверку `answerHold` → X уходит с вопросом.) X уже отпущена → перехват не срабатывает, Claim проходит.
- T2c e2e `internal/app` (скриптованный провайдер, харнесс `app_run_wake_hold_e2e_test.go`), supervision 1 ч, дедлайн 60 с: корень → `agent`; ребёнок → `bash sleep 120` + `ask_question`; корень на «SUB-AGENT QUESTION» → `agent(resume_session_id=…)`; ребёнок → `job_kill` + текст; корень → финал. Exit `end_turn` < 60 с, supervision-тиков нет. Без фикса — дедлайн. (Выключить событие → таймаут; выключить перехват → отказ resume ломает скрипт.)

**Шаг 3. Supervision и тексты.**
`supervision.go` (§3); `templates/agent_tool.md:7` («или отдельным сообщением сразу, если у ребёнка ещё бегут задачи»); `cmd/claude_slash_command.yaml:162-170` («получает вопрос сообщением сразу, даже пока бегут задачи воркера») + golden в `cmd/testdata`; CHANGELOG.
- T3 `TestSupervisionSummary_NamesDelegationAwaitingAnswer`: после T1 текст тика содержит «1 awaiting your answer», «AWAITING YOUR ANSWER», вопрос, `resume_session_id="C"`, `stop_agent`, «waiting will not progress it». Сегодня — «running …, last activity: …».
- T3b: ребёнок кончил ход текстом, держит задачу → «sub-agent idle, waiting on its own 1 job(s)».

## 6. Риски

- Двойной вопрос: событие забрано, затем задачи ребёнка осушились без ответа → release X снова несёт вопрос. Верно по сути (ребёнок всё ещё ждёт); повторный resume после терминальной X идёт обычным путём (дубль ответа воркеру, без поломки). Смягчение — фраза «still unanswered» в release, если `questionNoticed`.
- Ход ответа падает (провайдер, peak hours) после «answer delivered»: X держится до осушения, затем «failed»; видно в `inspect_agent`/supervision.
- Модель после немедленного ответа resume ждёт через `sleep` — ловят #1149/#1113; текст велит закончить ход.
- Web: результат resume `async:false` → в UI сразу «готово», X остаётся pending до release — корректно.
- Рост `recheckChild`: чтение сообщений только для суспендированного, не занятого ребёнка с неотмеченной X.

## 7. Не-цели

- Менять ASYNC-01/Claim, отпускать X при открытой области, автоматически гасить задачи ребёнка.
- «Resume в удерживаемую делегацию» для idle-ребёнка без вопроса (отказ фазы 4 остаётся; см. вопрос 3).
- Зависшие `go test`, семантика `wake_only` (не убивает), лимит 50 (#1155), подсказка 342af913 и её читатель.
- `sessions kill`/`inject` между ходами; web-рендер состояния; `sessions why`.
- Sync (SDK) делегации: синхронный вызов блокируется до осушения и событию не поможет.

## 8. Бэклог (Review Stop Rule: P2/P3 не чиним в этом цикле)

- P2: `recoveredDelegationText` теряет вопрос (нужен разбор finish без импорта agent → session).
- P2: читателя своих задач `ask_question` нет в Drain-ходах и web — подсказка 342af913 работает только в ходах `ExecuteRun` и унаследовавших его ctx.
- P3: `resume_session_id` принимает только полный id; короткий префикс → «not found».

## 9. Открытые вопросы оператору

1. Ответ — в удерживаемую X (итог под X, без нового job id) или «замена»: X терминальна + новая Y одной транзакцией в store? **Рекомендация:** в X — без изменений Claim/схемы, ASYNC-01/02/04 нетронуты.
2. Бросить вопрос при открытой области — только явный `stop_agent` (гасит задачи ребёнка)? **Рекомендация:** да; текст говорит это прямо, «просто продолжай сам» оставил бы `rush run` висеть на X.
3. Разрешить resume в удерживаемую X и для idle-ребёнка БЕЗ вопроса (вариант main: ребёнок ждёт зависшие задачи)? **Рекомендация:** не сейчас — в шаге 3 только видимость (строка supervision); расширение — отдельной задачей, оно отменяет решение фазы 4.
4. `sessions why` / web: показывать «ждёт ответа» (уточнение ASYNC-10)? **Рекомендация:** следующей задачей; закон уже выполняется — X названа.
