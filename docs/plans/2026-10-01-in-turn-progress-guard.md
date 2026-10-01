# Страж «нет прогресса внутри хода» (A22, A23, A24; задачи #1149, #1150)

Статус: дизайн, код не меняется. Улики — `.rush/audit/rush_quality.md`
(«Session audit #2», A22–A24); сырьё — `worktrees/wguard/.rush/rush.db`,
`worktrees/wa14/.rush/rush.db`, архивы `.rush/audit/sessions/*.db`. Цифры ниже
пересчитаны заново read-only прогоном модели §1 по 11 БД (wguard, wa14, rloop,
wrush-four, rarb2, wcache, wjob, wlog, wloop, wtitle, main); ход —
assistant-сообщения подряд до `finish != tool_use`, уведомление внутри хода —
user-строка с `background_job_notice`.

## 0. Диагноз

### 0.1 Наблюдения (OBSERVED)

| Ход | Шагов | Серия без прогресса | Что | Промпт/шаг |
|---|---|---|---|---|
| wguard 19:55–21:38 (A22) | 774 | 520 (перечитываний всего 763) | `view` git_writes.go: в 19:59 файл (165 строк) прочитан целиком `(0,163)`, дальше окна `limit` 1–3 (813 из 848), сдвиг `+1` (799 переходов), 24 перезапуска назад; `agent` — 0 | 188K |
| wguard 18:16–18:36 (A23) | 184 | 133 | `echo wait-marker-N`; перед серией — 4 прогона heavy.sh, `sleep 1` «No-op», `true` «Idle turn end»: модель пытается закончить ход вызовом инструмента | 100K |
| wguard 19:36–19:46 | 95 | 60 | `echo …-wait-N` | 123K |
| wa14 19:36–19:58 (A23) | 118 | 56 | `sleep 45…180; echo greenwaitN` + `timeout{wake_only, sleep+30}`; каждый «started» за ~8 с, пока шёл свой `heavy.sh go test`; дважды лимит 50 задач | 268K |
| wa14 20:24–20:43 | 102 | 80 | `view` work_ledger_inline_test.go, строки 176–210, окна 1–18; `edit` в наборе БЫЛ | 299K |
| wlog 17:51 / wrush-four 09-25 08:43 | 338 / 242 | 200 / 78 | `sleep N; echo …` (в wrush-four 10:49 ещё и чередование `sleep 170; echo ok` ↔ `view test_agent.log`, серия 14) | 113K / 80K |

Сводно: 410 ходов, 7748 шагов, 1398M токенов промпта. Шагов без прогресса
(модель §1) — 2209 (28%); за порогом H=5 внутри серий — 1837 шагов и 338M
токенов (24% всех). Серия ≥2 — в 24 ходах, и все 24 — ожидание sleep/echo,
опрос файла или перечитывание; законной серии ≥2 нет. Серия 1 — 10 ходов
(одиночный таймер, точечное перечитывание).

### 0.2 Механизмы

- **A23.** В CLI/web каждый `bash` — async-задача: `asyncTool.Run` сразу отвечает
  «started», внутри хода команда не ждёт. `echo` завершается за ~0 с, его
  уведомление забирается на границе следующего шага ТОГО ЖЕ хода (`prepareStep`
  → `pullPendingNoticesForStep`, DUR-3): у модели всегда есть «результат», ход
  не кончается. `sleep N` просто плодит задачи. #1113 считает звенья между
  ходами (`accountDrainAttempt` → `arb.chainLink`, правило 5 `decide`) — внутри
  хода слеп; `loop_detection` ждёт одинаковых вызова и результата.
- **A22.** Перечитывание уже полученного: окна новые по аргументам, но не по
  содержанию. В wguard причина — A24; wa14 показывает тот же режим и с `edit`.
- **A24.** Набор инструментов собирается на вызов из текущего конфига
  (`pinCallTools` → `buildToolsAgentConfigForCall`: воркер есть → сняты
  `orchestratorStrippedToolNames`), а системный промпт — сохранённый при
  создании сессии: `resolveSessionSystemPrompt` персистирует его один раз,
  `buildCall`/`runInternal` кладут его в `SystemPromptOverride`, и
  `resolveTurnConfig` ставит его выше свежего `call.SystemPrompt`, собранного
  вместе с набором. OBSERVED: промпт wguard (42425 симв., сессия создана до
  настройки воркера в 19:48) без правила 7 «Orchestrator mode»; правило 1
  требует «Read before editing … offset/limit», правило 3 — `edit`/`multiedit`/
  `write`, которых нет. После 19:55: `view` 943, `bash` 6, `agent` 0. wguard4
  (новая сессия, правило 7 в промпте) вызвала `agent` за 20 мин. Это разрыв
  поколений «промпт ↔ набор», который #341/P1-1 и #576/P1-3 закрыли для сборки,
  но не для сохранённого промпта сессии.

## 1. Модель «шаг без прогресса»

### 1.1 Один классификатор вызова

Новый `internal/agent/step_progress.go`, `classifyStepCall(call, result, step)`
— единственное место, где решается, чем был вызов. Его читают страж хода и
улики #1113 (`recordChainEvidence`).

| Класс | Вызовы | Прогресс хода | Покрытие чтения |
|---|---|---|---|
| `act` | может менять мир: `edit`/`multiedit`/`write`/`fs_write*`/`fs_replace`/`fs_delete`/`download`, `bash`/`run_command` с реальной командой, `agent`/`agentic_fetch`/`fetch`, `job_kill`, `inject_agent`/`stop_agent`, `wakein`/`wakeon`/`loop`/`wake_cancel`, неизвестные (MCP). Ошибка в результате не важна: неудачная правка — тоже повод перечитать | да | очищает |
| `read` | действие `read`/`list` в `restrictedToolActions` (grep, glob, ls, fs_list/find/grep, git_read, web_*, wake_list, …) и оконное чтение (`view`, `fs_read`) хотя бы с одной непокрытой строкой | да | добавляет окна |
| `reread` | `view`/`fs_read`, все окна которого покрыты в этом ходе | нет | — |
| `wait` | async-запуск wait-only (§3) | нет | — |
| `refused` | ответ стража (метаданные `progress_guard`), инструмент не исполнялся | нет | — |
| `neutral` | `job_output`, `todos` (ровно `chainNeutralTools`); `Invalid` (инструмента нет в наборе) | не меняет | — |

Класс шага: прогресс, если есть `act`/`read`; иначе «без прогресса», если есть
`reread`/`wait`/`refused`; иначе нейтральный. Текст ответа — не прогресс (как в
#1113); шаг без инструментов ход завершает сам.

#1113 сохраняет своё отображение: нейтральны `job_output`/`todos`, холостой
запуск — `wait` с claim из `ClientMetadata`, всё прочее (в т.ч. `reread`) —
прогресс. Единственное изменение: `refused` нейтрален — иначе отказ считался бы
прогрессом и обнулял цепочку.

### 1.2 Покрытие чтения

- Окно `view`: `[offset, offset+limit)`, `limit 0` → 500 (`DefaultReadLimit`);
  `fs_read`: `[start_line-1, end_line)` или `[line-1-radius, line+radius)`, без
  границ — весь файл. Ключ — `filepath.Clean` пути из ввода. Окно берётся из
  запроса, вывод не разбирается (хвост за EOF — допустимая неточность).
- Шаг классифицируется по покрытию на свой старт; его окна добавляются после
  (порядок параллельных вызовов не важен).
- Покрытие очищается: шаг с `act`; любая вставка на границе шага — inject или
  забор уведомления (файл мог поменять воркер/задача — например, проверка по
  правилу 7 после делегации); обрезка окна в `prepareStep`
  (`trimMessagesToWindow`: прочитанное ушло из промпта).

### 1.3 Где живёт и под какой блокировкой

- Состояние — поле `progress turnProgress` в `turnStream`, группа
  «callback-sequence only, no lock» (как `stepHistory`/`loopDetected`):
  `streak int`, `seen map[string][]span`, счётчики `waits`/`rereads` для
  текста, `stopped bool`, `step stepGuard` (снимок текущего шага). Другого
  нового изменяемого состояния нет.
- Пишут только `prepareStep` (сбросы покрытия, снимок, обёртка) и
  `onStepFinish` → `recordStepHistory` (класс шага, `streak`, окна, `stopped` —
  после `toolExecutionWg.Wait`, все результаты шага на руках); читает StopWhen.
  Это последовательные колбэки одного цикла `agent.Stream`.
- Не `onToolResult`: для `Parallel`-инструментов fantasy зовёт его из горутин
  исполнения, параллельно.
- Обёртка инструментов (§2.2) читает только `stepGuard` — значение, собранное в
  `prepareStep`: `streak`, `ownWork`, `async`, `canEdit`/`canDelegate` (по
  именам набора шага), копия `seen` (только когда возможен отказ `reread`).
  Параллельные вызовы читают значение — блокировок нет.
- `ownWork` = `workLedger.running(sessionID)` (у сессии есть недоставленная
  задача или делегация) — под `l.mu`, в `prepareStep` после забора уведомлений,
  без `ts.mu`: новых пар блокировок нет. Только для async.
- Область — один ход: новый ход начинает с нуля. Цепочки между ходами — #1113.

### 1.4 Оракул

Чистое ядро: `refuseWait(streak, ownWork, async)`, `refuseReread(streak)`,
`nextStreak(streak, class)`, `stopAfter(streak)`; S=2, H=5 (§2.1).
W — wait, D — reread, R — новое чтение, A — act, N — `todos`/`job_output`,
X — отказ; `[…]` — событие на границе шага; `ownWork` — вход на старте шага.

| # | Шаги | streak после шагов | Решение |
|---|---|---|---|
| 1 | W W W W W, `ownWork` нет | 1 2 3 4 5 | 1–2 исполняются, 3–5 → X; стоп после 5-го |
| 2 | `sleep 60`, затем W×4 | 1 2 3 4 5 | 1-й исполняется (таймер); дальше `ownWork` да (сам таймер) → X; стоп после 5-го |
| 3 | W×5 при живой своей задаче/делегации | 1 2 3 4 5 | все X, задач 0; стоп после 5-го |
| 4 | R(0,163), затем D(68+i,3)×5 (A22: окна разные, `loop_detection` молчит) | 0 1 2 3 4 5 | 2-й и 3-й исполняются, 4–6 → X (+ подсказка `agent` без `edit`); стоп после 6-го шага |
| 5 | D D A D | 1 2 0 0 | после A покрытие пусто: последний D — это R |
| 6 | W W N N N W | 1 2 2 2 2 3 | N счёт не трогает; 6-й → X, стопа нет |
| 7 | [W + A] ×7 | 0 … | прогресс в шаге побеждает (W при `ownWork` отказан, шаг всё равно прогресс) |
| 8 | W W [notice] W | 1 2 3 | уведомление счёт не сбрасывает (иначе echo-цикл вечен: его завершения приходят каждый шаг); 3-й → X |
| 9 | R D [notice] D | 0 1 0 | уведомление очищает покрытие |
| 10 | D D [trim] D | 1 2 0 | обрезка окна очищает покрытие |
| 11 | W×7, origin SDK | 0 … | sync-`sleep` реально ждёт → `act`, как в #1113 |
| 12 | W, затем ответ текстом | 1 | одиночный таймер санкционирован; ход кончился сам |
| 13 | [X + R] | 0 | прогресс |
| 14 | Drain-нога из одних X | — | #1113: ни прогресса, ни claim |
| 15 | вызов несуществующего `read` ×N | без изменений | `neutral` |

## 2. Реакция

### 2.1 Пороги

```go
const (
	inTurnRefuseAfter = 2 // S: с 3-го подряд шага без прогресса — отказы
	inTurnStopAfter   = 5 // H: ход завершается после 5-го подряд
)
```

- S=2: законный максимум в уликах — 1 (10 ходов); все 24 серии ≥2 —
  патология. Двукратный запас.
- H=5: после S — три отказа с инструкцией. Больше не нужно: A13b показал, что
  текст glm не останавливает; отказ сильнее, но предохранитель обязателен.
  Итог: ≤5 платных шагов без прогресса на ход вместо 56–520; оценка экономии —
  1837 шагов / 338M токенов промпта (верхняя граница: часть работы переедет в
  следующие ходы).
- Свои имена: это не K/N/D арбитра (`turn_arbiter.go`) — другой закон.

### 2.2 Мягкая: отказ инструмента

- `prepareStep` после вставок и обрезки окна, до создания assistant-сообщения,
  собирает `stepGuard`. Если он может отказать (`async && ownWork` или
  `streak ≥ S`), каждый инструмент шага оборачивается `progressGuardTool`;
  иначе срез шага не трогается. Одно место для закреплённого `call.Tools` и
  общего набора — там же, где `withProviderOptionsOnLast`. Обёртка делегирует
  `Info()` (имя, схема, `Parallel`), `ProviderOptions`/`SetProviderOptions`,
  `RestrictedRunAction`: запрос к провайдеру, кэш и R3-1 не меняются.
- `Run`: `classifyStepCall` по вводу и снимку. `wait` при `refuseWait` и
  `reread` при `refuseReread` → `fantasy.NewTextErrorResponse(текст)` +
  метаданные `{"progress_guard":{"refused":"wait|reread"}}`. Внутренний
  инструмент не вызывается: нет `workLedger.Start` → нет claim, строки
  `async_jobs`, уведомления; hooks и разрешения не запускаются; результат пишется
  обычным `Create` (ack gate не касается — тега claim нет). Обёртка внешняя,
  поэтому `loggedTool` отказ не видит — страж пишет `slog.Warn` сам (первый отказ
  хода и остановка).
- Тексты (английские, как у инструментов; финальная правка — в коде):
  - `wait` при своей работе: «Refused: `<cmd>` does not wait — commands here
    start as background jobs and return at once. Your running job/delegation
    reports by itself as a session message and starts your next turn. End your
    turn now: reply with a short status and NO tool call — a no-op tool call
    keeps the turn going.»
  - `wait` без своей работы (`streak ≥ S`): «…and your last N steps made no
    progress. To wait for time, call `wakein` and end your turn; otherwise
    continue the task or end your turn with no tool call.»
  - `reread`: «Refused: lines A–B of <path> were already returned in this turn
    and are in your context. Act on them or end your turn. If you are waiting
    for a running job to write this file, end your turn — its completion wakes
    you.» При `!canEdit && canDelegate` добавляется: «This session cannot edit
    files (orchestrator mode): delegate the change with the `agent` tool — file
    path, the exact change, how to verify it.»
  - при `streak == H-1` в конец: «If this step makes no progress, rush ends the
    turn now.»

### 2.3 Жёсткая: конец хода

- `recordStepHistory` ставит `stopped = streak ≥ H`; третье условие
  `stopConditions` возвращает `stopped` (как `loop_detection`: OnStepFinish идёт
  раньше StopWhen того же шага, флаг свежий). `recordStepFinish` — ветка после
  `loopDetected`: `AddFinish(FinishReasonEndTurn, msg, details)`; `msg`
  начинается с экспортируемой `agent.InTurnGuardStopTitle` («Stopped: no
  progress in this turn»), `details` — счёт waits/re-reads, была ли своя работа,
  что делать. `EndTurn` — по правилу `classifyStepFinishReason` для `StopTurn`:
  модель больше не вызывается, UI нужен футер. Порядок фаз в doc-комментарии
  `onStepFinish` дополнить флагом стража.
- Почему StopWhen, а не `ToolResponse.StopTurn` в ответе отказа: решение
  принимается по шагу целиком, после результатов, и шаг, где рядом с отказом
  был прогресс (строка 13), ход не обрывает. `StopTurn` решался бы по одному
  вызову, не видя соседей.
- CLI: `buildRunResult` добавляет warning, если
  `agent.IsInTurnGuardStop(finalErrTitle)` (прецедент —
  `agent.IsContinuationPrompt` в `app_run_terminal.go`): «turn stopped by the
  in-turn progress guard: <msg>». Не `finalTextWarnings` — `loopTotals` сохранит
  его, даже если ответом станет другой ход. `handleMessageEvent` печатает одну
  строку stderr на сообщение. `exit_reason` не трогается. Web: finish-текст в
  сообщении, как у `loop_detection`.
- Дальше штатно: своя работа есть → `CLIScope` = WorkOpen, цикл ждёт,
  завершение → Drain (новый ход, счёт с нуля). Работы нет → область закрыта;
  A18-напоминания о todos ограничены `cliTodoNudgeLimit = 2`, каждое ≤H шагов.

### 2.4 Согласование с #1113 и ASYNC-законами

- #1113: правило 5, N=3, `arbiterState`, маркер web — без изменений.
  `recordChainEvidence` переходит на `classifyStepCall`; `refused` нейтрален.
  Отказанный wait не создаёт claim → в ноге нет холостого запуска → звено не
  засчитывается: цепочка голодает, а не ломается. Без своей работы ход
  по-прежнему может запустить 1–2 wait (строка 1) — межходовую цепочку дальше
  ведёт #1113.
- ASYNC-02: страж ход только завершает; открытость области решает `CLIScope` по
  БД — живая работа держит `rush run`.
- ASYNC-04, DUR-7: отказ задач не создаёт; запущенные до остановки доставляются
  как обычно (забор в начале следующего хода).
- DUR-3: страж уведомлений не пишет и забор не трогает.
- DUR-4: шаг из отказов — шаг с вызовами (реальное содержимое) → `reacted=1`
  на видимые в его промпте строки, как у любого шага; новых писателей нет.
- ASYNC-06: страж ходов не начинает.
- ASYNC-09: остановка видима (finish, warning, stderr, лог). Строка ASYNC-09
  якорит «Idle-launch classification» на `turnStream.recordChainEvidence` и
  `shell.IsNoOpCommand` — переякорить на `classifyStepCall` в том же коммите.

## 3. Wait-only (A23)

- **Критерий** — один, общий с #1113 (сейчас зашит в `chainIdleClaim`,
  переезжает в `classifyStepCall`): `bash`, чья команда проходит
  `shell.IsNoOpCommand` (разбор mvdan: только литеральные вызовы
  `sleep`/`echo`/`printf`/`true`/`:`, между ними `;` или `&&`; без
  перенаправлений, пайпов, `||`, подстановок и переменных); `run_command` с
  `program` ∈ {`sleep`, `timeout`}. Поле `timeout` (в т.ч. `wake_only`) класс
  не меняет: у команды-таймера нечего проверять, а `seconds > sleep` (wa14)
  делает его пустым.
- **Только async:** origin CLI/web, тот же предикат, что в `asyncTool.Run`. В
  sync (SDK) `sleep` блокирует по-настоящему → `act`, как в #1113.

| Своя недоставленная работа (`running`) | streak | Вердикт |
|---|---|---|
| нет | < S | исполнить: одиночный таймер «запустил и закончил ход»; лучше `wakein` (долговечный, держит `rush run` по SCHED-10), но и bash-таймер держит область, пока бежит |
| да (задача, делегация, в т.ч. предыдущий таймер) | любой | отказ сразу: её завершение и так будит сессию; промежуточная проверка — `timeout{kind:"wake_only"}` на самой работе или `wakein` |
| нет | ≥ S | отказ (+ совет `wakein`) |

- wguard 18:15 («`true` Idle turn end») показывает, что модель считает
  вызов-пустышку концом хода; поэтому текст отказа прямо говорит «NO tool call».
- **Тексты инструментов не меняем.** «started» уже говорит «Do not wait with
  sleep/echo commands: each completion wakes you again», `job_output` — «do not
  loop on wait»; A13b/A13c: glm подсказки игнорирует. Настойчивость — в отказе
  в момент действия.

## 4. Режим оркестратора (A24)

- Системная заметка в истории не нужна: модель не «не заметила» смену набора —
  ей не сказали (правила 7 нет в промпте, по которому она работала). Чинится
  согласование промпта с набором вызова.
- Новый `internal/agent/coordinator_session_prompt.go`,
  `sessionPromptForCall(ctx, sessionID, pinned)`. Режим набора вызова —
  `pinned.orchestrator` (новое поле `resolvedOverrides`: ровно тот bool, что
  ушёл в `prompt.Build`, из того же снимка конфига, что `pinned.tools`). Режим
  сохранённого промпта — наличие маркера правила 7 (константа
  `prompt.OrchestratorRuleMarker`). Совпали — сохранённый (кэш цел). Разошлись —
  `pinned.systemPrompt` и пересохранение в сессию: один промах кэша на смену
  режима, обратная смена так же. Вызовы `resolveSessionSystemPrompt` в
  `buildCall` и `runInternal` (через `buildCall` идёт и Drain корня) заменяются
  на неё. Приоритет `resolveTurnConfig` (закреплён `turn_config_test.go`) не
  меняется — меняется то, что становится per-session промптом.
- Делегирование: правило 7 в промпте + отказ `reread` с подсказкой `agent` при
  наборе без `edit` (§2.2) — ровно в момент ползания. Смежно: #1147 (подсказка
  для неизвестного инструмента, в работе в `worktrees/wguard`,
  `augmentUnknownToolResult`) — после вливания для `edit`/`multiedit`/`write` в
  наборе без них ответ должен называть `agent` (одна строка там, не часть
  стража).

## 5. План реализации

Каждый шаг — `go build`, `go vet`, `gofmt`, тесты затронутых пакетов через
heavy.sh; revert-check — в скобках. Полные `internal/agent`/`internal/app` —
оркестратор, после всех шагов.

1. **Классификатор.** `internal/agent/step_progress.go` (`classifyStepCall`,
   `callKind`, окна чтения, `isWaitOnlyCall` вместо тела `chainIdleClaim`);
   `recordChainEvidence` на нём, `refused` → нейтрально; переякорить ASYNC-09;
   в `2026-10-01-reaction-chain-guard.md` §4 «цепочка внутри одной ноги — задача
   `loop_detection`» заменить ссылкой сюда.
   - T1 таблица `classifyStepCall`: `echo x` CLI → wait, SDK → act;
     `sleep 5 && go test` → act; `echo x > f` → act; `run_command sleep` → wait;
     `view` новое/покрытое/частично → read/reread/read; `fs_read` (все items
     покрыты) → reread; grep → read; `job_output`/`todos` → neutral; `edit` с
     ошибкой → act; `Invalid` → neutral; метаданные отказа → refused.
     (Пропускать перенаправления → строка `echo x > f`; убрать покрытие → строка
     покрытого `view`.)
   - T2 #1113: шаг с отказанным результатом не даёт ни `chainProgress`, ни
     claim. (Считать отказ прогрессом → красный.) `coordinator_reaction_chain_test.go`,
     `app_run_chain_guard_test.go` — зелёные без правок.
2. **Страж хода.** `internal/agent/turn_progress_guard.go` (`turnProgress`,
   `stepGuard`, `progressGuardTool`, тексты, S/H, `InTurnGuardStopTitle`/
   `IsInTurnGuardStop`); правки: `agent_turn_stream.go` (поле),
   `agent_turn_step.go` (`prepareStep`, `recordStepHistory`, `stopConditions`,
   `recordStepFinish`). `workLedger.running` уже есть.
   - T3 оракул §1.4 — табличный тест ядра, все 15 строк (сравниваются и
     вердикты, и ряд streak). (N как «без прогресса» → строка 6 останавливает
     ход на 5-м шаге; сброс счёта по уведомлению → строка 8 без отказа; без
     очистки покрытия по A → строка 5 даёт 1 2 0 1.)
   - T4 обёртка: при `ownWork` и streak 0 отказ wait — шпион внутреннего
     инструмента не вызван, строк `async_jobs` 0; без `ownWork` — проход.
     (Убрать ветку `ownWork` → шпион вызван.)
   - T5 форма A23 на харнессе `drain_attempt_fixture_test.go` (реальные
     coordinator/ledger/SQLite, httptest-провайдер), origin CLI: шаг 1 —
     удерживаемая W, далее провайдер отвечает `echo wait-N`. До отпускания W —
     ровно 6 запросов, 0 echo-задач, finish с `InTurnGuardStopTitle`, W жива;
     после — один Drain. (Без условия StopWhen → 7-й запрос до отпускания,
     провайдер отвечает 400 «unexpected model call».)
   - T6 форма A22: `view(0,163)`, затем `view(68+i,3)`×8 (окна разные, как в
     A22, — `loop_detection` не вмешивается), набор без `edit`, с `agent`: 2-й и
     3-й исполняются, 4–6 — отказ с текстом про `agent`, стоп после 6-го. (Без
     покрытия → все 9 запросов.)
3. **Видимость (internal/app).** `app_run_result.go` (warning по
   `agent.IsInTurnGuardStop`), `app_run_reviewer.go` `handleMessageEvent` (одна
   строка stderr).
   - T7 `buildRunResult`: finish стража → ровно одно предупреждение,
     `exit_reason` `end_turn`; finish `loop_detection` → нет. (Убрать ветку →
     нет предупреждения.)
   - T8 e2e `rush run` на харнессе `app_run_chain_guard_e2e_test.go`: при
     живой W echo в Drain отказан → звеньев нет, «reaction chain stopped» нет,
     цикл ждёт W, после W — один Drain и ответ. Это **переписывает**
     `TestRunNonInteractive_ChainGuardStopsAfterThreeDrainsWaitsForWork`: его
     сценарий (три echo-звена при живой W) теперь недостижим по замыслу;
     `…ChainGuardExitsWithoutWork` не меняется и остаётся e2e-доказательством
     #1113. (Убрать ветку `ownWork` → снова 3 звена — красный.)
4. **Промпт ↔ набор (A24).** `internal/agent/coordinator_session_prompt.go`
   (новый); `resolvedOverrides.orchestrator` в `coordinator_models.go` (957
   строк — только поле и два присваивания, логика в новом файле); два вызова в
   `coordinator_run.go`; `prompt.OrchestratorRuleMarker`.
   - T9: сохранённый промпт без маркера + вызов с воркером → системный промпт
     хода содержит правило 7, строка сессии пересохранена; второй вызов — без
     пересохранения; обратная смена; тот же режим — промпт не тронут. (Вернуть
     `resolveSessionSystemPrompt` → маркера нет.)
   - T10 шаблон: маркер есть ⇔ `WorkerAvailable`. (Переименовать заголовок
     правила 7 мимо константы → красный.)

## 6. Риски

- **Ложный отказ перечитывания** (модель «фокусируется», перечитывая полученное).
  Смягчено: два бесплатных шага, очистка покрытия на действии/уведомлении/
  вставке/обрезке, отказ называет строки и говорит, что они в контексте. В
  уликах таких серий ≥2 нет.
- **Обход смешанным шагом** `[echo + view]`: завершение echo очищает покрытие,
  `view` снова «новый». В уликах не встречен (чередовались шаги, не вызовы в
  шаге). Если появится — покрытие с эпохой (перечитывание после уведомления —
  нейтрально). P3.
- **Ползание по непрочитанному** (окно 1 строка, сдвиг +1) — каждый шаг
  «новый»; ограничено длиной файла, дальше — покрыто. Порог «≥K новых строк» не
  вводим: в wguard все 763 шага были перечитыванием. P3.
- **Настоящий результат пришёл в последнем шаге**, а модель ответила ещё одним
  ожиданием: уведомление отреагировано (DUR-4), ход кончился; без другой работы
  run заканчивается с предупреждением — видно и возобновляемо. «Свежий шанс»
  после чужого уведомления потребовал бы множества claim'ов хода и проводки через
  забор — не делаем.
- **Ребёнок делегации,** остановленный без своей работы, закрывает делегацию с
  неполным текстом → родитель видит и может `resume_session_id`.
- **A24 и кэш:** смена режима — один промах кэша; сессию, которую гоняют
  попеременно разными ролями, будет пересобирать на каждой смене (редко).
- `ownWork` видит только ledger процесса: активный `wakein` не считается — один
  лишний таймер допустим.
- Конфликт правок с #1147 в `agent_turn_stream.go` (там `onToolResult`, здесь —
  только поле). Размеры: `agent_turn_step.go` 707 → ~740,
  `app_run_reviewer.go` 932 → ~940, `coordinator_models.go` 957 → ~961.

## 7. Не делаем

- Не меняем #1113 (кроме `refused` → нейтрально в уликах), `loop_detection`
  (одинаковые вызовы — его), `job_output` (A13 закрыт его `StopTurn`; опрос без
  `wait` — бэклог, в уликах не встречен).
- Ни миграций, ни колонок, ни видов уведомлений; страж ничего не пишет в БД.
- Счёт — только внутри хода; межходовое — #1113.
- Описания инструментов не меняем; заметку о смене набора в историю не пишем;
  stderr-объявление режима оркестратора при старте `rush run` — отдельная мелочь.
- Inline-окно async (#1131/A14) — отдельно.
- Чтение через `bash` (`sed -n`, `cat`) — прогресс по определению.

## 8. Открытые вопросы оператору (с рекомендацией)

1. S=2, H=5. Рекомендация: принять; H=4 экономит шаг, но оставляет два отказа
   вместо трёх.
2. Отказ wait сразу при своей работе (`ownWork`). Делает недостижимым сценарий
   e2e #1113 с живой W (T8 переписывает тест); без правила ход при живой работе
   запускает до двух лишних wait-задач, а межходовая цепочка доходит до трёх
   звеньев #1113. Рекомендация: принять.
3. A24 — маркер правила 7 в тексте сохранённого промпта или новая колонка
   «режим промпта». Рекомендация: маркер (без миграции, держится тестом
   шаблона).
4. Подавлять ли A18-напоминание о todos после остановки стражем. Рекомендация:
   нет — уже ограничено двумя напоминаниями, каждое ≤H шагов.
