# XXS: рецензия будущей интеграции job batches с Rush

**Проверяемый коммит плана:** `51599fa4`.  
**План:** `docs/plans/2026-10-07-job-batches-design.md`, прочитан полностью, строки 1–727.  
**Основная область:** §9 и связанные требования §§0–4, §6 и §8. Текущие исходники использованы как свидетельства существующих контрактов, а не как реализация предложенного `jobbatch`.

## Вердикт

**needs correction — требуется корректировка интеграционного контракта.** Дорожная карта правильно отделяет автономный компонент от Rush, но пока не разрешает несколько конфликтов между новым Runner и существующими владельцами исполнения, терминального состояния и доставки. Наиболее существенны тупик admission у делегированных агентов, две разные точки линеаризации отмены и отсутствие достаточного подтверждения физического завершения.

**Блокеров автономного этапа в рамках этой рецензии не обнаружено.** Замечания ниже относятся к последующей интеграции. Они не требуют сейчас менять Rush, добавлять Store в автономный API, выполнять миграции или расширять batchlab до RPC/daemon. Это не заключение о выполнении всей автономной acceptance matrix: предложенный компонент здесь не запускался и не считался реализованным.

Все истории отказа будущей интеграции помечены `[INFERENCE]`: это выводы из прочитанных контрактов и исходников, а не воспроизведённые регрессии.

## Нумерованные замечания

### 1. P1 — удерживающий слот агент может заблокировать работу, от которой зависит его собственное завершение

**Этап:** решение об accounting до интеграционного cutover; не предварительный рефакторинг автономного этапа.

**План:** §0, строки 48–50; §2, строки 107–129; §3.1, строки 296–299 и 313–315; §3.2, строки 319–328; §9.2, строки 655–656, 669–671 и 680–683.

**Текущий контракт:** `async_tool.go:375–384` передаёт возврат агентского turn в `armDelegation`, а не завершает delegation. `work_ledger_delegation.go:115–120, 160–177, 221–258` удерживает её до закрытия собственных jobs ребёнка, background shells, занятого driver и межпроцессного scope/debt. Существующий admission проверяет jobs отдельного owner (`work_ledger.go:263–276`; `work_ledger_cap.go:45–59`), поэтому job родителя и jobs child session не занимают один общий глобальный пул.

**Отказ `[INFERENCE]`:** Runner с допустимым `MaxConcurrent=1` запускает singleton agent leaf A. Агент запускает команду B; её ingress тоже должен стать singleton batch. A удерживает единственный slot до завершения child scope, B остаётся Pending из-за A. Освободить slot некому. При лимите N то же возникает, если все N слотов заняты агентами, ожидающими собственные команды. Увеличение числового default лишь отодвигает этот тупик.

Есть и структурная сторона: `Spec` допускает либо task, либо batch, а agent представлен task. Новый child launch нельзя просто добавить под этот leaf как структурного потомка. Если оформить его независимым root, общий cap создаёт описанный тупик, а ограничения явного ancestor batch сами по себе не распространяются на этот новый root. Условие «не ждать свой parent batch» из §9.4, строк 709–712, этого случая не покрывает: A ждёт своего ребёнка, не родителя.

**Минимальная корректировка:** до интеграции зафиксировать lineage и правило распределения executable admission между живой delegation и её child work. Нужен выбранный механизм передачи/разделения execution credit или иной явно утверждённый способ не занимать весь executable бюджет ожидающими оркестраторами, сохраняя agent item nonterminal и учитывая child execution ровно один раз. При этом вопросы сами по себе не должны освобождать capacity вопреки §3.1. Текущий автономный API не содержит передачи credit или связи между независимо submitted roots; одного `TaskSpec.Group` недостаточно, поскольку остаётся общий `MaxConcurrent`.

**Acceptance:** при глобальном лимите 1 агент запускает одну короткую команду и затем завершается; при лимите N все N агентов запускают зависимую работу и прогресс сохраняется без повышения лимита. Отдельно проверить, что child launches внутри agent item не обходят утверждённый ancestor/accounting limit и не создают второй executor для уже существующего leaf.

### 2. P1 — Runner acceptance order и durable ledger CAS могут выбрать разные терминальные причины

**Этап:** обязательный контракт арбитража перед подключением адаптеров; не требование добавить SQLite в core.

**План:** §1.1, строки 64–68; §3.3, строки 338–352; §9.1, строки 643–645; §9.2, строки 659–678; §9.3, строки 692–695.

**Текущий контракт:** `work_ledger_transition.go:202–215, 244–254, 296–306` принимает результат DB CAS и усваивает именно состояние победившей строки. `AsyncJobStore.Transition` также возвращает committed winner при проигрыше (`async_job_store.go:474–480, 519–557`). `StopRunCommandJob` сначала делает snapshot/terminal commit и лишь затем вызывает cancel (`work_ledger.go:799–804, 849–875`); проигравший stop возвращает результат победителя. Существующий `job_kill` не выдаёт отмену за победившую, если row уже завершилась естественно (`tools/job_kill.go:135–144`).

**Interleaving `[INFERENCE]`:** executor естественно заканчивается; DB уже committed `completed`, но post-commit callback адаптера задержан. В это время `Runner.Stop` принят event loop раньше callback. По §3.3 Runner обязан latch `Cancelled`, хотя существующий CAS и job-control ответ говорят `Completed`. `batch_status`, singleton response и durable notice могут разойтись.

Обратная проблема появляется при прямой передаче Start context в существующий executor: §3.3 разрешает отменять его после принятия Stop, тогда как legacy path требует «snapshot → commit причины → cancel». Если контекст отменить раньше durable stop-записи, `inner.Run` может вернуть cancellation error, а `asyncTool.finalize`/`finish` успеют committed natural `failed` с wake=1 (`async_tool.go:342–346, 375–384`; `work_ledger_transition.go:93–109`) до intended `session_cancel` с wake=0. Post-commit observer сам по себе не исправляет уже выбранного победителя.

**Минимальная корректировка:** назвать одну точку линеаризации интегрированного исполнения и описать мост для всех Stop/job_kill/timeout/SDK-cancel путей. При сохранении нынешнего durable CAS адаптер должен согласовывать принятие Runner intent/report с committed winner и не отменять legacy execution context до записи предназначенной причины. Альтернатива — явно утверждённый cutover всех legacy terminal writers на новую единую authority, а не два независимых правила «кто первый». Нужен cause-aware control/admission seam; одного терминального callback недостаточно. Это решение интеграционной фазы, а не изменение автономной детерминированности.

**Acceptance:** поставить barrier после DB commit естественного результата и перед его доставкой Runner, затем запросить stop: все поверхности показывают одного победителя. Проверить также stop/timeout с задержанной записью причины и быстрым возвратом отменённого executor: нет `Failed` вместо отмены, ошибочного wake, повторного notice или relabel уже завершённого leaf.

### 3. P1 — seam «post-commit terminal» недостаточен даже вместе с возвратом asyncTool.run для подтверждения физического завершения

**Этап:** обязательный узкий предварительный рефакторинг §9.1 перед интеграцией. Сейчас выполнять его не требуется.

**План:** §2, строки 140–145; §3.1, строки 296–304; §9.1, строки 640–645; §9.2, строки 675–678.

**Текущие свидетельства:**

- `StopRunCommandJob` committed terminal row до cancel (`work_ledger.go:849–875`). `stopTargets` делает terminal transition, затем cancel (`work_ledger_delegation.go:371–406`).
- `deliverLocked` может удалить terminal job из owner map (`work_ledger.go:377–395, 410–439`). После этого поздний `finish` проходит ветку `!currentLocked` и не даёт нового terminal event (`work_ledger.go:902–920`). Таким образом, единственный post-commit seam может сработать слишком рано, а второго ledger terminal transition уже не будет.
- Даже возврат `asyncTool.run` для background bash не доказывает exit: `awaitShell` при отмене ждёт `KillOwned` только с 5-секундным контекстом, игнорирует ошибку и возвращается (`async_tool.go:409–416`). `BackgroundShellManager.KillOwned` удаляет shell из manager до ожидания `done` и при deadline возвращает ошибку, пока процесс ещё может жить (`internal/shell/background.go:607–624`; описанный контракт также в строках 580–587).
- Настоящий completion handle существует отдельно: `BackgroundShell.Wait/WaitContext` и `OnDone` ждут `bs.done` (`internal/shell/background_shell.go:158–171, 188–189, 234–245`). Для run_command используется реальный `cmd.Wait` (`tools/run_command.go:212–221`).

**Отказ `[INFERENCE]`:** stop committed `cancelled`, observer адаптера вызывает completion callback, Runner освобождает slot и запускает следующий leaf, хотя предыдущий процесс ещё завершается или не подчинился kill. Если вместо row наблюдать лишь возврат `asyncTool.run`, timeout bounded kill оставляет ту же ошибку. Для delegation возврат первого model turn также недостаточен, а cancellation row может появиться до прекращения child turn и его work.

**Минимальная корректировка:** расширить перечень seam в §9.1 до двух независимых фактов: committed outcome/cause и actual execution termination. Execution handle должен сохранять физический completion source и immutable claim/attempt identity независимо от удаления job из ledger или shell из manager. Callback `Result` разрешён после требуемого подтверждения, а bounded kill/Stop error остаётся control error, не ложным Settled. Для агента дополнительно требуется существующий delegation-scope release, а не только возврат первого Run. Не добавлять второй terminal writer/registry: это наблюдение жизненного цикла существующего исполнения.

**Acceptance:** заблокировать физический exit после committed stop и после возврата bounded kill; при cap=1 следующий leaf не стартует. Позже отпустить настоящий completion handle — slot освобождается один раз. Повторить с stop до ack, с удалённой из map job и со stale claim; никакая старая completion не завершает новый leaf.

### 4. P2 — нынешний stall-detach wrapper остаётся отдельным владельцем jobs и не исключает MCP

**Этап:** обязательное согласование construction/lifecycle границы при предварительном рефакторинге §9.1; не общий рефакторинг stall policy.

**План:** §0, строки 48–50; §9.1, строки 646–651; §9.2, строки 655–668.

**Текущий код:** MCP tools уже попадают в filtered slice, но `wrapAsyncTools` оборачивает только bash/run_command/agent/agentic_fetch (`coordinator_tools.go:783–809`; `async_tool.go:521–531`). После restricted-run и hooks поверх всех tools устанавливается `wrapToolsWithStallDetach` (`coordinator_tools.go:815–824`). Его исключения перечислены по именам и не включают MCP (`turn_stall_tool_detach.go:26–45`). При срабатывании policy он сохраняет execution в `stallDetachJobs` и возвращает `stall-<callID>` (`turn_stall_tool_detach.go:101–140, 171–184`). `job_kill` сначала проверяет именно этот controller (`tools/job_kill.go:61–65`).

**Отказ `[INFERENCE]`:** если новый MCP ingress поставить в нынешнюю точку async wrapping, а остальные wrappers просто сохранить, долгий synchronous MCP singleton может быть повторно detached внешним stall wrapper. Появятся Runner root и `stall-*` job для одной операции, два control namespace и изменившаяся связь SDK caller cancellation с root. Если policy-wrapped outer tool использовать как executor leaf, возврат stall wrapper может быть ошибочно принят за окончание MCP call, пока inner Run продолжает работу.

**Минимальная корректировка:** единый construction path должен различать policy-wrapped executable leaf и wrappers, которые сами владеют lifetime/ingress/delivery. Уже batch-managed исполнения не должны повторно попадать в stall detach registry; это нужно определять и для динамических MCP имён. Существующие hooks/restricted-run/permissions сохраняются, а legacy stall policy для неподключённых tools не требует wholesale rewrite. При миграции control names должны разрешаться в исходное исполнение, не в новый псевдо-job.

**Acceptance:** вызвать stall policy во время SDK MCP singleton и во время явного MCP leaf: нет второго job/root, ложного Settled или утраты caller Stop. `job_output`/`job_kill` и batch control адресуют одно исполнение; PreToolUse и restricted-run check выполняются по предусмотренному пути без двойного запуска.

### 5. P2 — lifetime root ID нужно связать с incarnation claim, а не только с tool_call_id

**Этап:** будущий durable identity/mapping contract до DB migration. Это законная интеграционная работа, не отсутствующий метод автономного Engine.

**План:** §2.1, строки 228–236; §4, строки 391–394; §9.1, строки 643–645; §9.3, строки 692–700; §9.4, строки 709–721.

**Текущий код:** текущий ключ `(owner, toolCallID)` не является вечной execution identity. `Start` присоединяет повторную активную call только при совпадении tool/input (`work_ledger.go:264–269, 314–328`), а `AsyncJobStore.claimOnce` архивирует историю под другим key и допускает новую call с тем же provider ID (`async_job_store.go:352–398`). Fresh claim получает новый UUID (`async_job_store.go:426–432`). Executor-side CAS сравнивает claim (`work_ledger_transition.go:244–254, 274–289`), а claimAck проверяет tagged result (`work_ledger_announce.go:53–87`).

**Отказ `[INFERENCE]`:** адаптер назначает singleton root ID как функцию owner+tool_call_id. Активный retry либо повторно вызывает `Submit` и получает duplicate-root error вместо текущего idempotent started/await, либо создаёт второй root. После завершения call провайдер законно использует её ID повторно; ledger разрешает новый claim, но Runner tombstone запрещает прежний root ID. Если решить это простым удалением mapping, поздние ack/completion/control старой incarnation могут попасть в новую.

**Минимальная корректировка:** зафиксировать mapping отдельно для logical call, root/node, durable claim и admission token. Активный matching retry должен находить существующий root/handle до `Submit`; намеренное повторное использование tool ID после архивирования истории получает новую incarnation и новый root ID. Durable claim ID и Runner token не взаимозаменяемы: второй различает попытки admission внутри leaf, первый — исполнения при повторном claim/процессе. Для sync paths без DB claim нужна локальная incarnation identity, а не обязательная новая durable запись.

**Acceptance:** concurrent одинаковый retry запускает ровно один executor; несовпадающий input активной call отклоняется; после history архивирования тот же tool ID с новой call запускается под новым root. Задержанные ack, stop и terminal report старой claim не меняют новую; старое completion notice сохраняет своё отображаемое tool ID без привязки к новому root.

### 6. P2 — повторное использование content-only async результата разрушит synchronous MCP image/media responses

**Этап:** уточнить adapter result contract перед расширением ingress на MCP; core остаётся независимым от Fantasy.

**План:** §2, строки 130–134; §9.1, строки 640–648; §9.2, строки 657–674; §9.4, строки 716–717.

**Текущий код:** `tools.Tool.Run` возвращает image/media response, включая текстовый content, либо текст (`tools/mcp-tools.go:166–182`). `Owner.RunTool` сохраняет байты и MIME type (`tools/mcp/tools.go:210–230`). Однако существующий async путь захватывает только `Content`, `IsError`, `Metadata` (`async_tool.go:348–350, 379–384`; `work_job.go:69–78`) и `awaitAndFinish` всегда восстанавливает `Type: "text"` (`async_tool.go:242–252`). Нынешнее отсутствие MCP в `wrapAsyncTools` объясняет, почему это не заявляется текущей MCP регрессией.

**Отказ `[INFERENCE]`:** synchronous MCP call после singleton cutover выполняется ровно один раз и даже получает Completed, но SDK вместо image/audio получает лишь подпись, потому что leaf прошёл через content-only capture. Для CLI/web переход прежнего MCP tool result к aggregate delivery также не должен незаметно выбросить media. Сохранение Summary/terminal counts не сохраняет response contract.

**Минимальная корректировка:** integration adapter должен удерживать полный immutable response envelope или ссылку на него, с прежними type, media bytes/MIME, metadata и error shape. Это можно выразить через opaque Result.Details/adapter-owned retained result, не добавляя Fantasy в Engine. Отдельно определить, как выбранный root completion доставляет non-text result; не использовать `FormatAsyncCompletion` как единственный формат всех MCP результатов.

**Acceptance:** singleton SDK MCP возвращает text, image и audio с теми же type/data/MIME/metadata, что policy-wrapped прямой вызов; denied/error path не превращается в успех. CLI/web получают один completion с сохранённым результатом, а не отдельные root и leaf notices.

### 7. P2 — формулировка root/leaf ack смешивает разрешение side effects и разрешение completion delivery

**Этап:** уточнить root/leaf delivery contract до интеграционной схемы хранения; не добавлять ack API в автономный Runner.

**План:** §2, строки 224–224; §4, строки 377–386; §9.1, строки 640–645; §9.2, строки 666–668; §9.4, строки 720–721.

**Текущий порядок:** non-sync durable claim предшествует launch (`work_ledger.go:283–305`; `async_tool.go:121–155, 203–216`), но executor уже работает до сохранения started response. Ack gate запрещает delivery terminal job, пока нет announced (`work_ledger.go:372–379`). Tool result и announced committed вместе (`work_ledger_announce.go:92–111, 155–186`; `session/notice_pull.go:332–379`). Inline path намеренно ждёт natural terminal commit до ack, затем объединяет outcome message и delivery=done (`work_ledger_inline.go:79–89, 105–127`; `session/notice_pull.go:450–505`).

**Двусмысленность `[INFERENCE]`:** буквальное чтение «root/leaf ack gate and durable claim precede externally visible effects» в строке 666 как требования *получить ack до исполнения* ломает inline и противоречит acceptance «immediate finish before ack». Другое чтение — независимый существующий ack для каждой внутренней leaf — тоже не определено: у явного `batch_run` есть одна outer tool call, а не собственная tool_use/tool_result для каждого объявленного task. Нынешний claimAck ищет job по `result.ToolCallID` (`work_ledger_announce.go:61–70`); такой запуск не даст leaves их обычных tagged acknowledgements. Просто ждать их означает orphan announced=0, а создавать искусственные leaf tool results — дублировать consumer protocol.

**Минимальная корректировка:** явно разделить durable root/leaf reservation до dispatch и transactional ack перед публичной completion delivery. Определить, как acknowledgement outer root распространяется на внутренние execution records без самостоятельных consumer tool results, и кто является единственным владельцем delivery для singleton/aggregate/inline. Finish-before-ack допускается; незакоммиченный результат не публикуется. Существующие ledger row notices не должны параллельно создавать пользовательские completion, уже принадлежащие root.

**Acceptance:** два leaf завершаются до задержанного root ack; после его commit приходит ровно один aggregate, нет вечных unannounced execution records и самостоятельных leaf result messages. Singleton command внутри inline window возвращает результат текущей call и не создаёт последующего completion/wake turn. Ошибка root ack не оставляет запущенную работу без управляемой identity; recovery не повторяет side effects.

## Границы, уже корректно обозначенные в плане, и необходимые будущие адаптеры

Это не дополнительные замечания о якобы реализованном компоненте. Здесь отделены реальные потребности Rush от задач автономной фазы.

| Граница | Прочитанные свидетельства и решение для интеграции |
|---|---|
| Sync SDK, detached CLI/web и inline | `async_tool.go:64–71, 106–153, 224–252` различает origin, attached/detached execution context и inline outcome. §3.3, строки 359–367, и §9.2, строки 659–665, правильно оставляют Wait cancellation отдельной от Stop. Адаптеру нужна явная привязка **оригинального sync caller** к Stop и отдельная inline delivery election; detached turn cancellation не должна автоматически останавливать root. Для SDK background escape отдельно учитывать shell identity: `bgshell_claim.go:49–60, 70–119` показывает, что synchronous tool response может завершиться раньше underlying command. Это тот же execution, не повод создать новый batch. |
| User Stop, CancelTurn, host shutdown | `coordinator_interrupt.go:41–51, 57–115, 119–137` разделяет отмену generation, полного дерева и остановку host. `work_ledger.go:956–997` на shutdown не делает user terminal transition; `commitTransition` подавляет shutdown-caused write (`work_ledger_transition.go:233–241`). Forced shutdown удерживает host lock (`async_job_store.go:631–653`), recovery позднее выставляет Interrupted только доказанно мёртвому host (`async_job_recovery.go:92–149`). §9.3, строки 692–700, справедливо требует сохранить эту разницу. Нельзя вести Runner.Close через legacy user-stop путь или считать `context.Canceled` достаточным кодом причины; live Runner Interrupted и durable recovery row требуют явного согласования, а не синтетического post-commit event. |
| Held questions, descendants, task_outcome | §9.2, строки 669–671, и §4, строки 377–401, верно запрещают считать первый Run завершением всей delegation и подавлять input requests под NotifyAll. `delegation_question.go:173–207, 214–258` отвечает **в held job**, использует answerHold, не делает новый claim и сохраняет original origin. Этот resume должен маршрутизироваться как Inject/control прежнего agent execution, не как независимый новый agent batch. `coordinator_subagents.go:327–347` сейчас считает AwaitingAnswerError обычным question response, а обычный непустой text — успешным tool response. §8, строки 621–624, и §9.3, строки 687–690, правильно оставляют required task_outcome только явно выбранному контракту и запрещают менять legacy success semantics. Хранилище декларации привязать к incarnation/attempt, не только child session. |
| MCP call, не connection | §9.2, строки 672–674, соответствует `tools/mcp-tools.go:138–163` и `tools/mcp/tools.go:157–177`: permission-bearing Tool.Run → Owner.RunTool → CallTool с operation lease. Lease close только освобождает lease (`tools/mcp/lease.go:305–321`); promoted connection живёт у owner, а не у call (`tools/mcp/lease.go:715–720`). Для leaf Stop отменяется call context; Owner.Close/server disable не заменяют его. Проверка двух одновременных calls одного owner, из которых остановлена только одна, остаётся обязательной интеграционной acceptance, а не поводом рефакторить MCP protocol. |
| Filtered policy-wrapped tools | §9.1, строки 646–648, и §9.4, строки 709–712, правильно требуют одну policy construction границу. Сейчас per-call DisableSubAgents/role/folder/reviewer floor применяются до construction (`coordinator_tools.go:568–604`), AllowedTools/AllowedMCP — до ingress (`coordinator_tools.go:771–809`), restricted-run/hooks — внешними wrappers (`coordinator_tools.go:811–824`). `tools/bash.go:142–159` содержит agentguard до process launch. Executor должен разрешать target из того же pinned разрешённого набора и policy path, а не восстанавливать произвольный tool из Payload через сырой factory. Выделение policy leaf не означает снятие фильтров; исключение lifecycle wrappers описано в замечании 4. Широкий security audit не выполнялся. |
| Root/leaf caps и legacy work во время миграции | §9.2, строки 680–683, верно требует учитывать work вне batch и сигнал освобождения. В текущем ledger cap=50 на owner и в счётчик входят nonterminal held delegations/timers, но не terminal-awaiting-ack (`work_ledger.go:24, 271–276`; `work_ledger_cap.go:45–59`). Запись orchestration root не является executable slot. До миграции выбрать соответствие owner/kind/group и источник CapacityAvailable для legacy work; не ждать только settlement в том же Runner. Это дополняется замечанием 1 о зависимостях и замечанием 3 о physical termination, а не заменяется ими. |
| Cross-process control | §9.4, строки 704–712 и 718–721, честно выделяет отдельную Rush работу. Методы Runner по своему контракту локальны; запуск второго Runner в операторском CLI не управляет handle первого. Сейчас `workLedger` хранит handles в памяти, а `rush sessions cancel` пишет session-wide request (`session/session_update.go:309–339`; `cmd/sessions_cancel.go:74–101`). Такой request нельзя безусловно переиспользовать для остановки leaf: он остановит sibling work той же session. Нужны durable owner/root/item/claim routing и request/ack исполнения на host, владеющем handle, включая idle detached roots. Реализация транспорта не является автономным prerequisite. Acceptance: второй процесс останавливает один вложенный leaf и не затрагивает sibling; Add/Inject доходят до настоящего owner, stale control не действует на новую claim. |
| Durable notices и Web Drain | В §1.3, строки 89–92, и §9.3, строки 697–700, корректно сказано, что Events/callback не являются outbox. Нынешний `notifyAsyncCompletion` — только nonblocking wake hint после commit (`coordinator_background.go:62–92`), а history message и delivery CAS объединяются в pull transaction (`session/notice_pull.go:105–160`). Root aggregate должен использовать такой же durable delivery ownership, не отправляться непосредственно из bounded Events feed и не запускать отдельный billable turn для каждого suppressed leaf. Внешний CLI driver получает hint, не конкурирующий Drain (`work_ledger.go:95–100`; `coordinator_background.go:95–101`). WebUI hierarchy/control проверяются позднее в браузере; batchlab этого не доказывает. |
| Timeout intent | Автономный `TaskSpec.Timeout` означает termination и подтверждённый TimedOut (§5, строки 426–429). Нынешний Rush `wake_only` не завершает работу, а создаёт отдельный check-in (`work_ledger_timeout.go:211–249`); terminate_and_wake сначала записывает причину и затем отменяет executor (строки 170–205). Адаптер не должен отображать wake_only в engine DeadlineExpired. Таймеры legacy и Runner нужно согласовать для одного execution, не включать два независимых termination deadline по неявному правилу. |

## Что действительно должно предшествовать интеграционному рефакторингу

1. Утвердить accounting/lineage зависимых agent executions и единственную точку выбора terminal winner — замечания 1–2. Это проектные решения интеграционной фазы, а не работы A0/A1 автономного компонента.
2. Определить узкие seams: committed outcome, actual execution completion, cause-aware control и policy-only executable construction. При выделении launch setup сохранить разрешения/driver generation, полноценный response и исключить повторное stall/ingress владение — замечания 3–4 и 6.
3. До DB migration выбрать durable identity и root/leaf delivery/ack правила — замечания 5 и 7; root records не должны сами занимать executable slots. Локальный launch token не становится cross-process claim.
4. Затем реализовывать собственно future integration: singleton normalization во всех named ingress, adapters, persistence/outbox/recovery и межпроцессное управление. Это не «предварительные исправления» ради запуска batchlab.
5. После совместного cutover выполнить интеграционные сценарии из замечаний и §9.4, включая mixed legacy admission, held answer race, Stop vs shutdown, MCP sibling isolation и реальные WebUI operations. Сейчас эти проверки не выполнялись.

## Сильные стороны

- Граница автономного deliverable выражена прямо (§0, строки 13–33; §9, строки 633–634). Не требуется второй scheduler, зависимость от coordinator или преждевременная Store/DB абстракция.
- Зафиксированы singleton ingress без повторного wrapping nested leaf, явные executor outcomes, удержание Starting/Cancelling slots и различие intent/termination. Это правильные исходные инварианты для последующего адаптера.
- Сохранены ownership checks, policy construction, shared MCP owner и legacy-agent success contract; future task_outcome не навязан всем существующим agents.
- Durable delivery и cross-process recovery не выданы за свойства диагностического feed, а WebUI/browser acceptance не подменена успехом batchlab.

## Пределы покрытия

Статически прочитан весь план и релевантные реализации named async/ledger/delegation/MCP/store путей, а также вызванные ими construction, held-question, stall-detach, shell completion, notice transaction и session cancel границы. Старые offsets не использовались. Файлы существующих отчётов, конфигурация и credentials не изучались; код не менялся.

Не выполнялись builds, tests, lint, formatters, smoke, browser checks, agent launches, commits или проверки git. Все acceptance сценарии выше — необходимые проверки для владельца последующей интеграции, не заявления об их прохождении. Не проверены все возможные сторонние SDK entrypoints, полная web transport/UI реализация и все платформенные process-tree гарантии. Единственный новый файл этой рецензии — `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md`.
