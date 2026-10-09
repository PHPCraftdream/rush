# XXS, раунд 2: будущая интеграция job batches с Rush

**Проверяемый план:** `docs/plans/2026-10-07-job-batches-design.md`, revision 2, строки 1–1378. Зафиксированная главным рецензентом ревизия — `f48f883061cd66be30795c9dcf873f2da4ae79bd` (`f48f8830`), git blob — `88be80856f854198cdf80d2ac29a3a8bb04939b2`.

**Предыдущая ревизия:** `51599fa4`; предыдущий отчёт — `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md`, прочитан целиком. Его семь нумерованных замечаний далее имеют стабильные ID **I1–I7**, соответствующие Appendix A. При пересечениях использованы также первые отчёты `xxs-states` (admission, физическое завершение и reducer) и `xxs-api` (notices, public API, ownership); их выводы не принимались за доказательство.

**Область:** §9 и связанные §§2–4/8; действующие async/ledger/delegation/MCP/store пути, tool construction, shell completion, `await_tasks`, turn arbiter, scope/debt и transactional notice delivery. Смещения исходников ниже получены из текущих файлов, а не перенесены из раунда 1.

## Вердикт и границы фаз

**needs correction — нужны точечные исправления контракта.** Из I1–I7 **5 fixed, 2 partially fixed**; unresolved/regressed/rejected — **0**. Есть **одно новое замечание I8, P2**, о несовпадении объявленного notice-based ожидания с worker-веткой `await_tasks`.

Это не требование сейчас интегрировать Rush:

- **До заморозки A0/A1 автономного компонента:** уточнить достаточное условие waiting-level admission в §3.2 — остаток **I1, P2**. Два выделенных waiting-level group limits ещё не ограничивают все конкурирующие leaves того же forest. Исправление — точная предпосылка гарантии, без lending, DB, agent-specific events или изменений Rush.
- **До будущего Rush cutover, не блокеры самостоятельного batchlab:** закончить singleton root/leaf arbitration mapping (**I2, P2**) и разделить top-level/worker semantics и единицу ожидания `await_tasks` (**I8, P2**).
- Миграции, adapter implementation, numeric defaults, root persistence, cross-process transport, media storage и actual WebUI acceptance законно остаются в отдельном интеграционном пакете §§9.1–9.4. Отсутствие этих будущих реализаций в текущем коде не является замечанием о готовности автономного Engine.

**Все сценарии поведения ниже — `[INFERENCE]`:** это статические контрпримеры и требования к будущей проверке, не результаты исполнения. Appendix A:1370–1378 — только указатель на заявленные исправления; закрытие проверено по нормативному тексту и текущим границам.

## Матрица закрытия I1–I7

| ID | Прежняя проблема | Статус | Текущий план, доказательство и оставшаяся граница |
|---|---|---|---|
| **I1** | Агент удерживает общий executable slot и ждёт child command, которому этот же budget не даёт стартовать; lineage/accounting независимых roots не выбран. | **partially fixed** | §3.2:511–519, §8:1117,1125–1126, §9.2:1201–1210 выбирают механизм: waiting levels в разных группах, global не меньше суммы их limits; child launches учитываются по группе, не structural ancestors agent leaf; один existing leaf не запускается второй раз. Отсутствие рекурсивного `agent` у worker действительно enforced: `coordinator_tools.go:633–653`, независимо от AllowedTools; `workerToolNames` объясняет guard в строках 176–198. Delegation действительно живёт дольше первого turn: `async_tool.go:375–384`, `work_ledger_delegation.go:115–177,221–282`. Но §2:138 и §2.1:299–305 разрешают default/unbounded competitors; §3.2:472–479 не резервирует capacity для waiting levels. Поэтому опубликованное арифметическое условие само по себе недостаточно — контрпример ниже. Требование прежнего отчёта прогрессировать именно при общем limit=1 больше не обязательно: выбран другой корректный механизм, такой config должен отвергаться. Выбранный group-accounting вместо наследования agent ancestor cap также не переиздаётся как отдельный дефект. |
| **I2** | Runner acceptance order и durable CAS независимо выбирают terminal winner; прямое ctx cancellation может опередить запись причины. | **partially fixed** | §9.2:1212–1223 выбирает **durable ledger CAS** и порядок «durable cause → cancel»; planned additive `Settled.Authoritative`, default false, разрешает committed natural outcome победить ранее latched leaf cause. Это соответствует текущим `work_ledger_transition.go:202–254,296–306` и `async_job_store.go:474–557`: loser adopts committed row, claim участвует в CAS. Контракт не требует SQLite в A0, а лаборатория сохраняет §3.3 acceptance order. Однако flag описан для **leaf**, а §3.1:416–419 оставляет независимую latched cause у batch. Target обычного singleton Stop и согласование singleton root с authoritative leaf ещё не определены: root может остаться Cancelled при единственном Completed leaf и committed Completed job. Остаток ниже — не повтор прежней проблемы выбора leaf winner, а незамкнутый consumer bridge. |
| **I3** | Post-commit terminal callback, даже вместе с возвратом asyncTool.run, ошибочно принимается за физическое termination. | **fixed** | §9.1:1151–1159 явно разделяет committed outcome/cause и actual termination, сохраняет completion source в handle после удаления job/shell, ждёт **оба** факта и защищает claim identity. §9.2:1196–1199 дополнительно удерживает admission до real exit; §§3.1.1:444–460 и 3.4:574–582 не разрешают synthetic Settled. Текущий stop действительно commit-ит до cancel (`work_ledger.go:849–865`, `work_ledger_delegation.go:371–406`); delivery удаляет map entry (`work_ledger.go:377–439`), а late finish может уже не найти её (`898–912`). `async_tool.go:409–416` игнорирует bounded kill error; `background.go:608–624` удаляет shell до ожидания done. Независимые реальные источники существуют: `background_shell.go:234–245`, `run_command.go:212–221`; delegation имеет scope gate, не первый Run. План теперь прямо запрещает использовать эти ранние proxy как confirmation. Это исправление дизайна, не заявление, что новые seams уже реализованы. |
| **I4** | Внешний stall-detach wrapper может повторно владеть MCP operation и создавать `stall-*` job рядом с batch leaf. | **fixed** | §9.1:1160–1169 выбирает policy-only construction из pinned filtered set, исключает повторный batch ingress **и stall-detach**, в том числе dynamic MCP names; job controls адресуют исходное исполнение, unmigrated stall policy сохраняется. Текущая опасная граница подтверждена: `coordinator_tools.go:771–824` применяет async, restricted-run, hooks, затем stall wrapper; `async_tool.go:521–531` сейчас не wrap-ит MCP; `turn_stall_tool_detach.go:31–38,93–140,171–184` имеет name-based exclusions и отдельный registry/ID. `job_kill.go:61–65` сначала проверяет stall controller. Исправление требует исключить lifecycle wrapper, не снять permissions/hooks; это прямо записано. Отсутствие нового exclusion в ещё не мигрированном коде не переиздаётся как открытая находка. |
| **I5** | Lifetime root ID, построенный только из owner/tool_call_id, конфликтует с idempotent retries и повторными incarnations. | **fixed** | §9.3:1275–1283 разделяет logical call, durable claim UUID, root/node и Runner token; active matching retry ищет существующий root **до Submit**, mismatched retry rejected, новая incarnation получает новый root, sync без DB claim — local incarnation. Текущий ledger уже присоединяет matching active call (`work_ledger.go:264–269,314–328`, `async_tool.go:142–155`); Store архивирует history и mint-ит новый UUID (`async_job_store.go:352–398,426–431`). Stale transition защищён claim (`work_ledger_transition.go:244–254,274–289`), ack — claim tag (`work_ledger_announce.go:53–87`), отображаемый старый tool ID отделён от archived key (`async_job_store.go:690–709`). Planned mapping сохраняет эти разные identities и не делает launch token cross-process claim. |
| **I6** | Content-only capture теряет type/data/MIME synchronous MCP image/audio результата. | **fixed** | §9.2:1225–1231 требует полный immutable response envelope через opaque `Result.Details` reference, включая media и error shape, и отдельное non-text root delivery; §9.4:1338–1340 требует text/image/audio acceptance. Текущий policy-bearing `tools.Tool.Run` действительно формирует image/media responses (`mcp-tools.go:138–182`); `Owner.RunTool` сохраняет bytes/MIME (`tools/mcp/tools.go:157–175,189–231`). Старый async capture хранит Content/IsError/Metadata (`async_tool.go:348–350,379–384`), sync восстановление всегда text (`247–252`). План теперь запрещает переносить этот lossy path на MCP. Сохраняется контракт **действующего policy-wrapped Tool.Run**, а не обещание заново реализовать весь raw MCP content protocol. |
| **I7** | Ack-before-effects смешивает pre-dispatch reservation с completion delivery; внутренним leaves явного batch неоткуда получить собственные tool_result acks. | **fixed** | §9.2:1233–1244 отдельно задаёт reservation до dispatch и transactional ack до public completion, разрешает finish-before-ack; outer root ack покрывает execution records, leaves explicit batch_run не создают собственных tool results/notices; singleton/aggregate/inline имеют одного delivery owner. §9.3:1294–1305 закрепляет transactional root delivery, не Events feed. Это согласуется с текущими claim-before-launch (`work_ledger.go:283–305`, `async_tool.go:121–155,203–216`), gate (`work_ledger.go:377–379,491–502`), fused ack (`work_ledger_announce.go:92–111,155–186`) и inline election (`work_ledger_inline.go:79–127`). Notice message и delivery CAS уже атомарны (`notice_pull.go:129–164`). Новые root rows, propagation ack на Add и durable representation ещё предстоит реализовать; выбранные reservation/ack/ownership invariants достаточны, их отсутствие в текущей схеме не является повтором I7. |

## Открытые остатки прежних находок

### I1 — P2: сумма waiting-level limits не является достаточным условием при третьем конкурирующем group

**Фаза:** исправление автономной предпосылки до A0; её Rush mapping — позднее. Это общее замечание на пересечении states/admission review, а не второе требование внедрить lending.

**Основание:** §2:138; §2.1:299–305; §3.2:464–479,511–519; §8:1117; §9.2:1201–1210. Frozen core считает все active leaves в global, включая default group. Published waiting-level recipe проверяет сумму выделенных групп, но не запрещает остальную нагрузку в этом же budget.

**Контрпример `[INFERENCE]`:**

1. `ByGroup = {agents: 1, commands: 1}`, `MaxConcurrent = 2`; root A содержит agent-like controlled execution группы agents. Заявленное условие `2 >= 1+1` выполнено.
2. Другой root C содержит execution default group, ожидающее Inject. C уже Running и занимает второй global slot; такой spec разрешён §2:138 и §2.1:300.
3. A submit-ит отдельный root B группы commands и ждёт B. A должен послать Inject в C только после результата B.
4. B Pending из-за global cap, A ждёт B, C ждёт input от A. Свободного слота и нового CapacityAvailable нет. Начальные работы можно поставить за barriers; порядок OS scheduler здесь не нужен.

Разделение agents/commands устраняет исходный двухгрупповой тупик, но не этот allowed forest. Current Rush caps также не становятся частью нового global budget автоматически: `maxAsyncJobsPerSession=50` (`work_ledger.go:24`), admission проверяет одного owner (`271–276`), held delegations считаются nonterminal (`work_ledger_cap.go:45–59`). §9.2:1260–1265 правильно откладывает migration accounting; это не доказательство достаточности автономной формулы.

**Минимальная корректировка:** ограничить guarantee явно закрытым набором competing groups. Для forest, использующего waiting levels, **каждая** competing group должна иметь конечный cap, включая default/other work, и global должен покрывать сумму всего этого набора; либо consumer должен исключить other/unbounded work из данного Runner. Остаётся общая предпосылка: dependency work не проходит общий удерживаемый ancestor cap. Engine по-прежнему не обязан обнаруживать ожидания. Не требуется запрещать default group в обычном Runner без такой liveness guarantee или добавлять transfer-credit API.

**Acceptance:** unsafe трёхкорневой configuration выше не объявляется гарантированно безопасной/отвергается consumer, которому поручена эта гарантия. Safe closed-world вариант с C в своей ограниченной группе и global, покрывающим все три caps, допускает B, после B A inject-ит C, все roots завершаются без изменения limits в процессе. Сохранить E4 с отдельными waiting/dependency roots; добавить проверку default/other competitor. Для Rush будущий config validator учитывает полный selected mapping и migration competitors, не только два named group limits.

### I2 — P2: authoritative leaf не согласован с latched singleton root

**Фаза:** только будущий adapter/cutover. **Не** причина добавлять ledger или authoritative mode в автономную лабораторию.

**Что уже закрыто:** выбор CAS как единственной authority для durable leaf и запрет «cancel до intended cause commit». Frozen laboratory semantics из §3.3:531–538 менять не нужно. Planned additive flag из §9.2:1217–1219 законно можно добавить в интеграционной фазе, а не требовать его наличия в нынешних A0 declarations.

**Основание остатка:** §2.3:340–347; §3.1:416–419; §4.1:626–629; §9.2:1182–1188,1212–1223. SDK adapter ждёт root outcome, ingress нормализуется в singleton, но target cancellation/ordinary job-control и правило применения authority к этому root не выбраны.

**Interleaving `[INFERENCE]`:**

1. Singleton root R имеет один leaf L. Legacy executor физически завершился и CAS committed `completed`; delivery/report в Runner задержан barrier.
2. Адаптер маршрутизирует обычный stop/caller cancellation как `Runner.Stop(R)` — сейчас текст не исключает этот target. Runner принимает stop, batch R и leaf L latch `stop`.
3. Адаптер доставляет committed Completed с `Settled.Authoritative=true`. По новому правилу L становится Completed, а не Cancelled.
4. По неизменённому §3.1 R имеет собственную latched cause и становится Cancelled, с completed count=1. `Batch.Wait`/root `batch_status`/root notice отличаются от committed job outcome. Текущий ledger control, проигравший CAS, отвечает winner-ом (`work_ledger.go:862–875`, `job_kill.go:135–144`).

Для явно остановленного **многоэлементного** batch Cancelled root с completed descendants — нормальная политика, не ошибка. Проблема именно в обещании unchanged singleton results и «every surface shows one winner» без mapping, отделяющего group intent от underlying execution winner.

**Минимальная корректировка:** выбрать bridge правило. Например, обычные singleton job-control/SDK cancellation адресуют **единственный leaf**, так что root определяется reducer-ом от authoritative leaf; explicit `batch_stop(root)` остаётся самостоятельным group operation с собственной batch cause. Если обычный control должен адресовать root, описать узкое правило согласования normalized singleton root с committed winner. Не снимать latched cause произвольных ancestors при любом authoritative descendant result. Также явно ограничить обещание «один winner» execution surfaces, отдельно описав legitimate group cancellation.

**Acceptance:** barrier после completed CAS и real termination, но до Runner report; затем обычный singleton stop и timeout/caller cancellation. Ledger, leaf, singleton Wait/status/consumer result показывают один execution winner, без второй completion. Отдельный explicit multi-leaf root stop сохраняет Cancelled aggregate и ранее Completed leaves. Во время delayed durable cause write legacy ctx ещё не отменён; после записи отмена и два observation seams приводят к одному terminal result. SDK memory-only path, разрешённый §9.3:1282–1283 и существующий в `work_ledger.go:283–305,335–336` / `work_ledger_transition.go:367–388`, должен иметь явно выбранную локальную authority, а не ожидать несуществующий DB commit.

## Новая находка

### I8 — P2: notice-based `await_tasks until:any` в плане не совпадает с blocking worker path

**Фаза:** будущая интеграция; автономный Engine не получает await event, notification transport или session imports.

**План:** §9.2:1251–1258 требует batch work в `LiveWorkForRoots`, root aggregates completion-class и утверждает, что `until:any` просыпается на first delivered notice, не на suppressed leaf. Сразу затем включён worker, остающийся внутри tool call.

**Текущие границы:**

- Top-level `AwaitTasks` читает Own+Descendants, для `all` устанавливает SleepAll; normal tool result имеет StopTurn (`coordinator_await_tasks.go:64–98`, `tools/await_tasks.go:113–137`). Здесь пробуждение действительно проходит durable notice/debt/arbiter путь.
- Worker определяется по `ParentSessionID` и идёт в другую функцию; arbiter не участвует (`coordinator_await_tasks.go:55–62,101–105`). Она poll-ит тот же `LiveWorkForRoots`, а `any` возвращает при **`n < initial`**, `all` при `n == 0` (`106–120,143–155`). Возвращаемый tool response **не** StopTurn (`tools/await_tasks.go:90–111`). Это не ожидание delivered notice.
- `LiveWorkForRoots` читает running rows, а не delivery/selected notices (`async_job_reader_batch.go:96–109,124–170`). Committed outcome, actual termination, selected notification и pulled delivery — разные факты.
- Arbiter 4b использует live counts и CompletionOnly (`turn_arbiter.go:265–275,449–464,501–513`). Notice kinds сейчас completion-class только для bg_shell_done/supervision; job debt классифицируется иначе. `PendingInclusiveDebtRows` включает pending **и done unreacted** rows (`notice_row_ids.go:50–71`). Следовательно, новый root kind и suppression требуют migration этих projections; одного помещения batch rows в live query недостаточно.

**Контрпример `[INFERENCE]`:** delegated worker запустил explicit NotifyAll root R с leaves L1/L2 и вошёл в blocking `await_tasks until:any`. Если, как предложено, execution rows L1/L2 остаются visible в live-work projection, initial count включает их (возможно, также R). L1 завершается, L2 удержан barrier. L1 исчезает из running projection, count уменьшается и worker tool возвращает `Woke: ... finished` — хотя L1 notice suppressed, R ещё nonterminal и **ни одной completion delivery не произошло**. Если projection заменить только на R, worker всё равно возвращает по disappearance R, а не по delivery: notice pull может быть задержан. Это разные контракты, не просто разные implementations одного wake condition.

Также замена live projection на «удерживать всё до delivery/reaction» не может быть неоговорённым исправлением: worker намеренно блокирует текущий turn, а реакция обычно происходит после выхода из tool. Результатом такого переноса может стать self-wait на delivery/reaction, который выделение admission groups не устраняет.

**Минимальная корректировка:** разделить semantics явно:

1. Для top-level `any` — selected/delivered completion notification (с отдельными question/max_wait wake exceptions); для `all` — nonterminal work плюс completion-class debt согласно 4b.
2. Для blocking worker — определить **логические await targets** и criterion собственного tool return: завершившийся root/item, а не обязательно доставленная notice. Если worker должен уважать NotifyAll как root-level ожидание, suppressed internal leaves не входят как самостоятельные await targets. Если ему разрешён осмотр отдельного settled leaf без notification, это прямо отличить от consumer completion policy.
3. Описать, как один root и его execution rows не double-count-ятся, и развести physically-live work для guard/scope от selected-notification debt. Поля схемы/SQL можно выбрать при migration; нужен не готовый новый reader сейчас, а непротиворечивый consumer contract.

**Acceptance:** один NotifyAll root с двумя leaves, worker `any`, barrier после L1 completion и отдельный barrier перед root notice delivery. Tool return происходит только по выбранному worker criterion; suppressing L1 не создаёт user notice или billable turn. Top-level `any` не стартует turn по suppressed leaf и просыпается на разрешённый root notice; `all` при нескольких roots уважает 4b. Child question остаётся non-completion wake и не подавляется NotifyAll. Удержать real exit после stop commit: ask_question guard и scope/await-all всё ещё видят физически nonterminal work. После termination, root ack, delivery и reaction не остаётся suppressed leaf debt, бесконечного await или дублирования aggregate. Это будущие проверки, не сообщения об обнаруженной текущей Rush регрессии.

## Закрытые интеграционные границы и законно отложенная работа

Следующее не добавляет новых замечаний и не переоткрывает I3–I7:

- **Observation seams — не два владельца.** Committed CAS state и actual exit наблюдаются независимо; adapter handle связывает их одной incarnation. Текущий `childScopeDrained` учитывает owner jobs, shells/completion holds, driver busy и CLIScope/debt (`work_ledger_delegation.go:221–282`), поэтому первый child turn не заменяет release. При integration этот scope должен видеть retained physically-live descendants согласно I3, а не просто переименовать старый post-commit callback. Нужны boundary tests, не второй terminal writer/finished-execution registry.
- **Policy-only leaf.** Preserved target проходит тот же pinned AllowedTools/AllowedMCP и restricted-run/permission path (`coordinator_tools.go:574–604,771–821`), но не outer async/stall ownership. Выполнение hooks ровно по прежней top-level/sub-agent policy важнее blanket утверждения, будто hooks должны вновь запускаться на каждом leaf. §9.1 позволяет такую конструкцию без wholesale rewrite.
- **MCP call, не owner connection.** `tools.Tool.Run → Owner.RunTool → CallTool` и operation lease — действующие границы (`mcp-tools.go:138–163`, `tools/mcp/tools.go:166–175`). Lease close освобождает операцию (`lease.go:305–321`), не означает Owner.Close. §9.2:1192–1195 правильно требует call-only cancellation и sibling isolation. Media preservation относится к полному исходному response, а не к Summary/FormatAsyncCompletion.
- **Reservation, ack, retention, recovery — разные задачи.** §9.2:1233–1244 корректно меняет root delivery ownership; current failed-ack path ещё abort-ит legacy job (`work_ledger_announce.go:171–177`, `work_ledger.go:514–546`), поэтому будущая durable root reservation должна сохранять controllable identity отдельно от успеха consumer ack. Это уже acceptance I7, не новый дефект плана. Как именно Add inherits committed root ack и где хранится envelope, можно решить при migration, сохранив заявленные invariants.
- **Suppression — не потеря debt bookkeeping.** Root completion-class, suppressed leaves без billable turns и transactional outbox прямо требуются §§9.2–9.3. Current notice/debt readers ещё не знают batch kinds; они должны измениться при cutover. Сам по себе этот факт не переоткрывает I7. Открыт именно conflicting worker wake criterion в I8.
- **Shutdown не user stop; wake_only не termination.** §9.3:1289–1292 и §9.2:1246–1249 сохраняют различия. Текущий ledger shutdown suppresses terminal write и leaves running rows для recovery (`work_ledger.go:956–997`, `work_ledger_transition.go:233–241`); future reconciliation live Runner Interrupted с durable dead-host recovery — интеграционная работа. Как и cross-process item control из §9.4:1314–1323, это не повод расширять автономный Runner до daemon или повторно использовать session-wide cancel для leaf.

## Сильные стороны revised design

- §0:19–39 сохраняет один обычный Go component и independently runnable batchlab без Rush prerequisites; §§7.1–7.3 отделяют frozen declarations от реальных slice bodies и общей verification.
- I3–I7 теперь закреплены не общими обещаниями, а проверяемыми границами: два execution facts, policy-only construction, incarnation mapping, full media envelope и root-owned transactional delivery.
- Group-based two-level strategy является допустимым отказом от прежнего варианта lending/limit=1; worker recursion guard подтверждён текущим кодом. Осталось точно ограничить её liveness guarantee, а не заново проектировать scheduler.
- Принцип «control intent/error не termination» одинаково проведён через Start/Stop/Close, admission и будущий adapter; bounded Events feed не выдан за durable outbox.
- План учитывает недавно добавленный `await_tasks`, сохраняет held-question resume в прежний execution (§9.3:1285–1287; текущий `async_tool.go:80–89`) и не вводит generic agent-text outcome parser.

## Обязательные изменения в порядке выполнения

1. **До A0:** сделать waiting-level arithmetic условием полного competing budget/closed-world mapping — I1. Не требовать реализации lending или переделки Rush.
2. **Перед future adapter cutover:** выбрать ordinary singleton control target и root/leaf authority mapping; явно отделить execution winner от explicit group cancellation — I2.
3. **В future integration contract:** разделить top-level notification wait и blocking worker await targets/criterion; дать одну согласованную live-work/debt projection для batch rows — I8.
4. При последующей реализации выполнить приведённые acceptance barriers плюс уже выбранные I3–I7 cases: stop до/после ack, удалённая map entry, bounded kill без exit, stale claim, stall policy на MCP, image/audio envelope, inline/aggregate delivery и sibling call isolation. Эти интеграционные проверки не подменять автономными E/R/L tests.

## Покрытие и ограничения доказательств

Статически прочитан весь revised plan (1–1378), весь прежний integration report и релевантные разделы related first-round reviews. Прочитаны текущие named async/ledger/transition/delegation/subagent/MCP/store paths и связанные construction/ack/inline/shell/notice/await/arbiter/scope boundaries. Для ask_question прослежена цепочка `app_run_setup.go:126–139 → SessionActivity/LiveWorkForRoots → tools/ask_question.go:110–125`, а не предположено, что guard читает Runner snapshot.

**Не выполнялись** builds, tests, lint, formatters, runtime execution, smoke, browser checks, agent launches или git verification/commits/push. Не утверждается, что jobbatch уже реализован, контрпримеры воспроизведены или acceptance matrix пройдена. Не проверены все SDK entrypoints, весь Web transport/UI, все platform process-tree гарантии и будущая схема хранения, которой ещё предстоит появиться. Главному владельцу следует проверять автономный residual I1 controlled scenarios до fan-out; I2/I8 и I3–I7 integration acceptance — после соответствующего cutover.

**Единственное изменение задания:** создан новый `docs/reviews/2026-10-08-job-batches-xxs-rush-integration-round2.md`. План, исходники, прежние отчёты, конфигурация и пользовательское удаление `web/dist/.gitkeep` не изменялись.
