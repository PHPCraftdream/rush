# Job batches: второй независимый разбор состояний, admission и гонок

**Текущая ревизия плана:** `f48f883061cd66be30795c9dcf873f2da4ae79bd` (`f48f8830`), ветка `main`.  
**Git blob плана:** `88be80856f854198cdf80d2ac29a3a8bb04939b2`.  
**Предыдущая рассмотренная ревизия:** `51599fa4`.  
**Документ:** [docs/plans/2026-10-07-job-batches-design.md](../plans/2026-10-07-job-batches-design.md), revision 2, строки 1–1378.  
**Вердикт:** **needs correction — нужны точечные исправления автономного контракта до завершения A0 и запуска A1**.

Все пять исходных замечаний **S1–S5 закрыты в их первоначальном содержании**. Это не означает, что revision 2 уже можно замораживать: обнаружены один новый пробел batch lifecycle и два межконтрактных остатка — область применимости waiting-level гарантии и доступность финального feed при завершении draining. Ни одно из этих исправлений не требует подключения Rush, БД, MCP, модели или реализации lending.

Идентификаторы commit/blob взяты из зафиксированного задания; отдельная git-проверка не выполнялась. Номера строк ниже относятся исключительно к текущему плану `f48f8830`, если явно не указан предыдущий отчёт.

## 1. Метод и граница заключения

Полностью прочитаны текущий план, включая §9 и Appendix A, и [предыдущий states-отчёт](2026-10-08-job-batches-xxs-states.md). Для пересечений прочитаны соответствующие места [API-отчёта](2026-10-08-job-batches-xxs-api.md) — прежде всего findings 1–3 о Events и ControlReported, [parallel/testing-отчёта](2026-10-08-job-batches-xxs-parallel-testing.md) — controlled gates, полнота журнала и cleanup, и [integration-отчёта](2026-10-08-job-batches-xxs-rush-integration.md) — I1 и physical termination.

Appendix A:1344–1378 использован как указатель на предполагаемые исправления, **не как доказательство закрытия**. Для каждой S-находки проверены новое нормативное правило, путь данных через API и соответствующая acceptance matrix. Ни текст предыдущего отчёта, ни объявление в Appendix A о разрешении проблемы не считались доказательством.

Это статическое ревью предлагаемого самостоятельного Go-компонента. **[INFERENCE]** означает мысленное исполнение допустимого порядка событий либо вывод о реализации, следующей буквальному контракту. Сценарии не запускались. Статус **fixed** означает устранение прежнего противоречия в дизайне, а не прохождение будущего теста.

## 2. Матрица закрытия всех предыдущих findings

Стабильные идентификаторы S1–S5 соответствуют пяти нумерованным findings первого states-отчёта; эта нумерация сохранена в Appendix A:1348–1352.

| ID | Прежняя проблема | Статус | Точное текущее основание и причина решения |
|---|---|---|---|
| **S1** | CapacityAvailable или релевантный settlement обгоняет асинхронный capacity refusal; поздний отказ стирает уже принятый wake-up и оставляет leaf Pending навсегда. Прежний отчёт:13–29. | **fixed** | §3.2:495–506 хранит `gen[g]`, увеличивает его при каждом принятом CapacityAvailable и settlement **accepted execution той же группы**, запоминает generation при авторизации StartLeaf и блокирует группу только при отсутствии более нового сигнала. §3.3:534–536 и Event-контракт §2:244–249 отвергают старые tokens без повторного изменения admission. E5:905–909 закрепляет оба порядка, новый token, отсутствие spin и stale refusal. Теперь факт wake-up существует независимо от того, было ли на момент сигнала уже Pending/blocked work. |
| **S2** | Общее «errors/panics settle accepted work» позволяет освобождать reservation по ошибке Stop либо считать panic setup доказательством отсутствия живого исполнения. Прежний отчёт:31–53. | **fixed** | §3.1.1:431–443 возлагает отсутствие оставшегося исполнения после error/panic Start на executor-owned cleanup; Runner не объявлен наблюдателем произвольных ресурсов. §3.1.1:444–460 отделяет Stop error/panic/timeout, inspection/input и executor goroutines от подтверждённого termination. `ControlReported` присутствует в §2:244–247, его код меняет view, но не state/admission:447–450. E6:910–914 и R1:928–932 проверяют обе стороны границы. Неподтверждённое accepted work остаётся nonterminal, даже если cleanup никогда не завершится. |
| **S3** | Неисполненный successor nested batch под StopOnFail получает Completed из обычного reducer вместо Skipped. Прежний отчёт:55–65. | **fixed** | Batch lifecycle §3.1:405 и текст:419–423 прямо задают Skipped самому never-started Pending successor и его Pending descendants; уже terminal descendants не переименовываются, включая завершённый empty batch. §4.1:619–623 допускает именно Skipped aggregate notice от NotifyEach-родителя. E3:894–898 проверяет nested state, отсутствие запусков, выбранный notice и сохранение terminal descendants. Обычная агрегация больше не является альтернативной трактовкой policy-skip. |
| **S4** | MaxParallel zero/one неоднозначен для Sequential с nested Parallel и смешанных групп; zero мог превращаться в global/group число или в cap 1. Прежний отчёт:67–83. | **fixed** | §2.1:299–310 разделяет numeric limits, validation MaxParallel «как написан» и normalization только объявленных policy defaults. §3.2:464–485 даёт полный conjunction predicate: Sequential gate, global, собственная group, все explicit ancestor caps и capacity block. Zero не наследует число; Sequential one ограничивает все active leaves своего поддерева. E2:888–893 задаёт ожидаемый overlap/serialization и mixed-group случай, включая Starting/Cancelling. |
| **S5** | После deadline Close неизвестно, кто принимает late Start/completion; loop мог завершиться вместе с wait context и навсегда оставить Done открытым. Прежний отчёт:85–95. | **fixed** | §1.2:89–91 и §3.4:564–582 явно отделяют closing, draining и closed. Deadline возвращает ErrCloseTimeout с unfinished nodes, но не останавливает loop; поздние Start/report/control results продолжают приниматься. Повторный Close разрешён; View/Snapshot/Events/Output/Stop/Release работают во время drain; Stop/Dispose получают detached bounded cleanup context. R2:933–938 отдельно требует late confirmation и late-returning Start после первого deadline, один Interrupted outcome, Done и успешный второй Close. Новая проблема **S-R2-3** относится к доступу наблюдателя **после** подтверждённого окончания drain, а не к потере поздних executor reports из S5. |

**Итог S1–S5:** fixed **5**, partially fixed **0**, unresolved **0**, regressed **0**, rejected **0**. Незакрытых прежних states-findings нет. Исправления их первоначального содержания ниже повторно не предъявляются.

### 2.1 Контрольные перестановки для закрытых S-находок

Следующие выводы — **[INFERENCE]**, не результаты выполнения:

- **S1, wake-up раньше отказа:** авторизация при generation 0 → CapacityAvailable(g) даёт 1 → отказ старой попытки сравнивается с 0 и не теряет сигнал → повторная попытка получает новый token и generation 1. Если она снова capacity-refused без нового сигнала, группа блокируется. Обратный порядок сначала блокирует на 0, затем generation 1 разблокирует. Settlement accepted execution в g заменяет явный сигнал; settlement другой группы автоматически им не является. Availability generation и Launch.Token решают разные задачи и не взаимозаменяемы.
- **S2, control failure:** A уже accepted при global cap 1; Stop latch → stop_panic через ControlReported → A остаётся Cancelling, B не получает slot. Только report о действительно прекратившемся A позволяет settlement. Panic Start после созданного ресурса допустим как rejected start лишь при executor-owned cleanup; отсутствие этой гарантии было бы нарушением executor-контракта, а не разрешённой Runner synthetic termination.
- **S3, nested skip:** Sequential StopOnFail содержит failed leaf и ещё не начатый batch B. B и его Pending leaves становятся Skipped; NotifyEach parent может выбрать B notice, а B.NotifyAll подавляет leaf notices. Уже Completed empty child остаётся Completed. Это устраняет прежнюю неоднозначность именно policy-skip, независимо от новой проблемы обычного Pending reducer в S-R2-1.
- **S4, zero/one:** при global 3 Sequential(0) допускает три leaves своего текущего Parallel child; Sequential(1) — один. В обоих случаях следующий direct child остаётся закрыт gate до terminal текущего nested batch. По группе учитываются leaves всего forest, по explicit ancestor cap — leaves конкретного поддерева; batch nodes собственного slot не занимают.
- **S5, поздний handle:** Start вошёл до cancellation, возврат handle задержан до истечения первого Close. Handle не запускает leaf повторно, не сбрасывает latched host_closing и получает Stop с полезным cleanup context. Nil Stop — лишь acknowledgement. Поздний termination report завершает leaf/root Interrupted, после чего cached Batch Summary позволяет Wait и повторный Close. Если Start ещё не вошёл и его context уже cancelled, side effects запрещены §3.3:527–530.

## 3. Открытые и новые проблемы автономного контракта

| ID | Severity | Тип | Когда исправить |
|---|---|---|---|
| **S-R2-1** | **P1** | Новый пробел обычного batch reducer: never-admitted batch не получает aggregate outcome после отмены всех его leaves. | До заморозки lifecycle в A0; затем Core/Runtime acceptance в A1/A2. |
| **S-R2-2** | **P2** | Остаток общего waiting-level решения I1, впервые выделенный здесь на автономном admission: сумма только waiting groups не защищает от разрешённых uncapped/default competitors. | Уточнить автономные предпосылки §3.2/E4 в A0; конкретные Rush mappings/validation остаются §9. |
| **S-R2-3** | **P2** | Новое взаимодействие closed lifetime и публичного feed: финальные retained notices становятся недоступны сразу после drain. Пересекается с прежними API A1 и testing P3, но не повторяет S5. | Заморозить Runtime/Laboratory termination boundary в A0; проверить через публичный Runner в A2. |

### S-R2-1 — P1: отмена всех Pending leaves не завершает никогда не стартовавший batch

**Текущее evidence:**

- §3.1:403 переводит batch Pending → Running только при admission descendant leaf.
- §3.1:404–407 перечисляет Pending → Completed для empty batch, Skipped для policy skip, Cancelled при stop самого batch и Interrupted при host closing.
- Reducer-переходы на диаграмме существуют только из Running:409–411; текст:416–418 ограничивает обычную агрегацию формулировкой **«A batch that ran without a latched cause»**.
- Pending leaf допускает самостоятельный Stop до dispatch:359, §3.3:527. API разрешает Stop любого item:§0:48–50, §2:228; E6:910 требует leaf/subtree/root Stop и sibling isolation.
- Batch.Wait должен возвращать terminal Summary независимо от положительного исхода:§2.3:345–347; Release запрещён, пока есть nonterminal node:§4.5:677–678.

**[INFERENCE] Конкретный порядок:**

1. `MaxConcurrent=1`. Root A уже держит accepted execution `A.1`; его termination gate закрыт.
2. Submit root B с единственным leaf `B.1`. Global budget занят A, поэтому B и B.1 остаются Pending. Ни одного StartLeaf для B не было.
3. Принят `StopRequested(B.1)`, не `StopRequested(B)`. B.1 становится Cancelled без executor call, поскольку он никогда не был admitted.
4. У B теперь все leaves terminal, counts `cancelled=1, active=0, pending=0`, но сам B не «ran» и у него нет собственного latched stop. Перечисленные Pending batch переходы и текст reducer не дают ему aggregate Cancelled.
5. Реализация, следующая опубликованной диаграмме и ограничению «that ran», оставляет B Pending: B.Done не закрывается, B.Wait не возвращает результат, root notice отсутствует и Release(B) отказан. Поздний settlement A не помогает: у B нет eligible leaves, admission никогда не произойдёт.

Считать Stop leaf одновременно explicit Stop всех его ancestors нельзя: это другой контракт и нарушило бы sibling isolation в общем случае. Считать B Running из-за stop события тоже нельзя без добавления нового перехода: текущий текст требует именно admission.

Это **не S3**, переименованный другими словами. В S3 node был прямым policy-skipped successor и теперь явно имеет Skipped. Здесь policy-skip вообще отсутствует: проблема обычной агрегации batch, который не успел начать ни одного leaf.

**Минимальная коррекция:** обычный reducer должен применяться к Pending и Running nonterminal batch независимо от факта предыдущего admission, как только все его direct children terminal. Сохранить текущий способ вычисления aggregate outcome и приоритет собственных latched stop/skip causes; обновлять дерево снизу вверх. Добавить соответствующие reducer-переходы из Pending. Не создавать фиктивный Start/Running и не расходовать slot, чтобы заставить reducer сработать. Уточнение «все direct children terminal» также закрывает дерево из одних уже Completed empty nested batches без terminal root над ещё Pending batch node.

**Приёмочный сценарий:** удерживать A при cap 1; подать B и остановить B.1 до любого admission. До освобождения A проверить B=Cancelled, единственный root notice B, закрытый Done, `Wait` с Summary и nil error, `Add(B, …)` → ErrTerminal и допустимый Release(B). A остаётся Running и не получает Stop. Повторить для нескольких Pending leaves с отдельными Stop и для Pending nested batch: последний terminal child должен завершить всех готовых ancestors снизу вверх. Отдельный root из nested empty batches получает Completed с нулём leaves и не оставляет Pending descendants. Дополнить E6/E7/E8 и public-API R5, а не проверять только leaf counts.

### S-R2-2 — P2: waiting-level гарантия требует закрытого accounting domain

**Текущее evidence:**

- `TaskSpec.Group == ""` — default group, ограниченная только глобально:§2:136–140.
- Любая group без ByGroup limit тоже ограничена только глобально:§2.1:299–300.
- Global admission считает **все** active leaves Runner:§3.2:464–474, в том числе Starting и Cancelling.
- Waiting levels:§3.2:511–517 требует distinct groups и `MaxConcurrent` не меньше суммы **«those groups' limits»**; не сказано, что это полный набор потребителей global budget.
- E4:902–904 обещает progress зависимой работы в отдельном root, когда global покрывает обе group limits. §8:1117 и :1120 закрепляет waiting groups и общий forest budget.

**[INFERENCE] Конкретный порядок, без общих structural ancestors:**

1. `ByGroup = {g:1, h:1}`, `MaxConcurrent=2`, все root MaxParallel=0. A в g — execution, который будет ждать запущенную им работу B в h. Это удовлетворяет опубликованному числовому правилу: `2 >= 1+1`.
2. До запуска B приняты A и независимый execution C с `Group=""`. C — message-capable paused work; ждёт Inject, который A намерен отправить после получения результата B. В §4.4:665–668 такое ожидание допустимо и удерживает slot; C не ждёт работу, которую он сам запустил.
3. Host принимает запуск B как отдельный singleton root в h. Group h свободна, нет Sequential/ancestor caps и capacity refusal, но global active = 2: A + C. B остаётся Pending.
4. A ждёт B, C ждёт последующее сообщение A, B не может быть admitted. Ни fairness, ни CapacityAvailable не освобождают reservation accepted A/C. Выбранная сумма waiting-group caps не оставила promised child capacity.

Engine при этом правильно соблюдает global limit. Ошибка — **недостаточная предпосылка утверждения о liveness**, а не неправильный admission predicate. Даже без циклического сообщения долго живущий C может на неопределённое время вытеснить child credit из global pool. Counterexample не использует child под ancestor agent batch: положение §9.2:1206–1207 о таком accounting само по себе его не устраняет.

При двух **единственных** bounded группах и `MaxConcurrent >= g_limit + h_limit` выбранное решение корректно устраняет исходный однородный агентский тупик I1. Это полезная design choice, не основание требовать lending или сохранять прежний неверный пример с global 1 при одновременно живых parent и child. Остаток возникает при расширении этой гарантии на допускаемые тем же API дополнительные/uncapped groups.

**Минимальная коррекция:** явно определить область применимости no-lending waiting profile. Все executions, совместно использующие его global pool, должны входить в закрытый набор bounded groups; global покрывает сумму **всех этих** caps, включая независимое/paused work. Uncapped/default work нельзя незаметно смешивать с таким профилем. Кроме того, зависимые launches не должны закрываться общим с waiter Sequential gate или недостаточным explicit subtree cap; модель отдельных roots из E4 и §9.2 это обеспечивает. Это caller/consumer precondition, не новое состояние Engine и не причина запретить uncapped groups для обычного, не обещающего такую liveness, Runner.

**Приёмочный сценарий:** зафиксировать не только положительный двухгрупповой пример E4, но и его область допустимости. Для полного профиля `g:1, h:1, z:1`, global 3, удерживать A(g) и paused C(z), затем подать B(h) отдельным root: B должен стартовать, затем A получает результат и возобновляет C; все три reservations считаются ровно один раз. Вариант `g:1, h:1`, global 2 с дополнительным uncapped C должен быть явно **вне** заявленной progress-гарантии, а не считаться безопасным только по `2 >= 1+1`. Для будущего Rush adapter проверка полного mapping/config остаётся интеграционным acceptance, не работой над текущим Rush в A0.

### S-R2-3 — P2: closed Runner отрезает retained финальные notices без feed gap

**Текущее evidence:**

- §3.4:568–570 обслуживает Events и Snapshot во время drain, но :578–580 завершает loop сразу при terminal всех roots; после этого **все** Runner methods, кроме Close, возвращают ErrClosed. Исключение сохранено только для существующих Batch.Done/Wait.
- §4.1:596 объявляет Events единственным public stream; :607–608 включает в него выбранные notices.
- §4.1:611–617 задаёт bounded retention, Head/Oldest, explicit ErrGap и recovery, а :619–623 выбирает root и nested aggregate notices.
- R3:939–943 требует выбранные notices через public feed без consumer-side policy, не блокирующих loop observers, explicit gaps и корректные final snapshots.
- Laboratory пишет журнал именно из Events:§5.5:831–840. Вопрос producer final Head/footer подробно относится к testing-ревью; здесь рассматривается более узкий state/consumer boundary.

**[INFERENCE] Конкретный порядок:**

1. Root R — NotifyEach, его nested batch B — NotifyAll с одним accepted leaf. Ring retention достаточно для всего прогона; переполнения нет. Observer прочитал старые records и временно задержался перед следующим Events.
2. HostClosing принят. Первый Close достигает своего deadline; R/B/leaf остаются nonterminal, drain продолжает работать, что правильно закрывает S5.
3. После deadline executor подтверждает termination. Один принятый Settled завершает leaf, B и R с latched Interrupted, выбирает notices B и R, подавляет ordinary leaf notice внутри B.NotifyAll и записывает финальные records.
4. Все roots terminal; loop немедленно переходит в Closed. Observer теперь вызывает Events со своим всё ещё retained cursor и получает ErrClosed, не страницы с выбранными B/R notices. Snapshot(R) и Snapshot(B) также ErrClosed.
5. Batch.Wait(R) работает, но даёт только root Summary. Он не заменяет объявленный feed selected notices и не возвращает nested views/notice list. Восстановить B notice через повторную host policy — именно путь, который R3 запрещает.

Record producer не потерял данные, executor reports не потеряны, slot освобождён правильно, retention не переполнился. Потеря доступности возникает между terminal publication и первым последующим consumer read. Простое «читайте Events непрерывно» не исключает этот порядок: file writer/observer может законно отставать, а loop не должен ждать его.

Это **не повтор S5**: старый дефект завершал loop слишком рано **до** late termination и оставлял Done незавершённым. Revision 2 это исправила. Новое противоречие — окончание lifetime API сразу **после** принятого terminal transition, пока публично обещанные retained records ещё не прочитаны. Это также не требование durable/exactly-once доставки после crash.

**Минимальная коррекция:** отдельно заморозить окончание producer и возможность consumer дочитать его bounded retained tail. Например, после остановки engine loop Events остаётся доступен из неизменяемого retained feed с финальным Head: возвращает имеющиеся records/ErrGap, а ErrClosed только для cursor за Head. Loop и Close при этом никогда не ждут reader. Исправить blanket правило «other Runner methods ErrClosed» для этого read-only исключения и согласовать с final state recovery; root Wait может остаться cached. Laboratory должен знать final producer boundary перед завершением журнала, а не делать вывод о полном feed из одного закрытого Done. Новый transport, Store или observer ack framework не нужен.

**Приёмочный сценарий:** с большим RecordRetention специально не читать Events во время последнего Settled, дождаться успешного завершения второго Close и только затем запросить старый retained cursor. Получить terminal event и selected B/R notices ровно по одному, без leaf notice, с неизменным финальным Head; запрос за Head заканчивается объявленным closed результатом. Повторить с реальным retention overflow: ErrGap остаётся отличим от clean end. Проверить, что заблокированный reader не задержал termination/Close. Добавить совместный R2/R3 boundary test; journal/footer часть проверяет Laboratory/A2.

## 4. Остальные рассмотренные новые контракты

Эти пункты — проверка согласованности текста, не дополнительные findings и не заявление о пройденных тестах.

- **Per-group generation и per-leaf admission token.** Группа хранит availability, token идентифицирует конкретную попытку; §2:167, §3.1:390–393, §3.2:495–506 и §3.3:534–543 разделяют их роли. Ретрай после отказа не является вторым accepted execution того же leaf. Старый token не освобождает reservation новой попытки и не продлевает её timer. Сам факт control acknowledgement generation не увеличивает: это правильно, поскольку capacity ещё не освобождена.
- **Relevant settlement.** В текущем плане это settlement accepted execution именно в g, а не любой terminal Pending/Skipped leaf и не settlement соседней группы. Cross-group external capacity release должен быть явно представлен нужными CapacityAvailable(g). Нельзя подразумевать универсальный внешний pool по одному ErrCapacity; source/mapping legacy сигналов в §9.2:1260–1265 — законная последующая работа.
- **Cancelled Start и поздний handle.** §3.1:383–393 и §3.3:527–530 совместимы с immediate completion и Start, вошедшим до cancellation. Успешный late Start публикуется перед buffered Settled, но не стирает latched cause; rejected Start с такой cause завершает leaf без выдуманного Failed/Pending. Stop после возврата handle не является новым admission. Controlled.Start §5.3:773–775 после cancellation возвращает context error без side effects: он проверяет rejection branch, а R2 late-successful-handle branch вправе потребовать отдельный Runtime-owned fixture. Отсутствие такого режима в обычном controlled scenario само по себе не блокер A0.
- **Timers.** §3.3:539–543 определяет начало при авторизации/dispatch StartLeaf, включая Starting, retirement на rejection/settlement и token в DeadlineExpired. Сценарий «refusal старой попытки → retry → старый timer» не должен отменять новую или Pending попытку. Первый принятый cancellation cause выигрывает по :537–538; timeout не означает физическое termination:§5.6:856–859. Прежняя неявность timer identity теперь закрыта.
- **Fairness.** §3.2:487–493 требует persistent round-robin по roots и lowest eligible pre-order leaf внутри root; group-blocked leaf обходится, а освобождение reservation рассматривает весь forest. Это устраняет прежний риск пробуждать только стартовавшее tree. Sequential gate намеренно не обходит текущего direct child. Эти правила не обещают progress, если global/ancestor gate физически не оставляет child slot — поэтому S-R2-2 нельзя исправить только перестановкой очереди. Не заявляется starvation-freedom при бесконечном враждебном потоке новых событий или произвольных циклах зависимостей.
- **Stop confirmation.** §2.3:340–344 теперь явно отделяет return Runner.Stop от ответа Execution.Stop и termination. Ошибка control видна в Engine-owned view через ControlReported; accepted work удерживает capacity до report. Протокол повторных control calls, pin ещё работающего Stop при Release/Dispose и roll-up retry subtree/root относятся к смежному API-разбору. Они не возвращают к прежнему правилу S2 «panic settle»: его разделение фаз остаётся исправленным.
- **Add и settlement.** §2.1:292–297 сохраняет atomic acceptance subtree и direct-child IDs; §3.3:525–526 выбирает порядок Add/last completion; §8:1104 запрещает Add terminal/cancelling batch. Новая S-R2-1 важна именно потому, что отсутствие parent settlement иначе оставляет Add ошибочно разрешённым после отмены всех исходных leaves.

## 5. Автономные блокеры и законно отложенная интеграция

### Требует решения сейчас

- **S-R2-1:** lifecycle/aggregation, которое Core и Runtime должны одинаково понимать до A1.
- **S-R2-2:** точные предпосылки общего самостоятельного waiting-level обещания §3.2/E4; это не выбор конкретных Rush defaults.
- **S-R2-3:** граница Runner → consumer при окончании producer; Laboratory уже зависит от Events и не может ждать подключения durable Rush delivery.

Для каждого достаточно изменения самостоятельного документированного контракта и будущего engine/runtime/lab acceptance. Сейчас не требуется писать реализацию или запускать implementers.

### Не является блокером A0/A1 самостоятельного компонента

§8:1123–1133 и §9:1139–1342 правильно откладывают численные Rush defaults, существующие owner/session caps и полный mapping waiting groups, учет legacy work, source CapacityAvailable из ledger, DB representation и CAS/ack arbitration, durable notices/recovery, hooks/permissions ingress, SDK cancellation bridge, MCP connection ownership и cross-process controls.

В частности, будущий `Settled.Authoritative` §9.2:1212–1223 должен быть отдельно согласован с latched causes **batch ancestors**, а не только leaf winner; это интеграционный мост с durable authority, не повод менять standalone acceptance-order semantics или добавлять БД в A0. Agent outcome declaration, agent questions и live-work/await_tasks projection также остаются adapter/host решениями §9.2–9.4. Их отсутствие не делает standalone fixture ложной реализацией MCP/agent.

Замечание S-R2-2 не требует выполнять этот roadmap сейчас: выбранную двухуровневую схему можно документировать корректно уже сейчас, а реальные Rush mappings/validation проверить в интеграционной фазе. Замечание S-R2-3 не требует durable delivery: оно о retained данных живого process-local Runner до их удаления/недоступности, не о crash recovery.

## 6. Сильные стороны и пределы покрытия

**Сильные стороны revision 2:**

1. Полный admission predicate теперь вычислим без придуманных inherited чисел; reservation соответствует Starting/Running/Cancelling и shared forest.
2. Availability generation решает реальную асинхронную гонку, а не только serialized single-thread пример; typed refusal отделён от operational failure.
3. Setup cleanup, control failure и physical termination разведены по owner и event path; ControlReported не создаёт ложный terminal.
4. Skipped batch, empty terminal descendants и выбранные aggregate notices стали явным контрактом с собственными acceptance assertions.
5. Close wait context больше не равен lifetime drain loop; detached cleanup context и repeated Close делают поздние подтверждения осмысленными.
6. API Events/Notice и новый controlled attempt-bound rendezvous позволяют проверять поведение через настоящий Runner без повторного scheduler/policy в lab. Pure core, standard-library runtime и независимая поставка сохранены.

**Пределы:** прочитаны design contract и связанные first-round reports, не аудитировалась фактическая реализация Engine/Runner/executors или действующие Rush launch paths. Не проверены platform process-tree guarantees, race behavior, toolchain и ОС cleanup. Builds, tests, lint, formatters, smoke/replay, runtime execution, git-команды, credentials access и agent launches не выполнялись. Все interleavings и acceptance proposals — **[INFERENCE]**. Этот отчёт не утверждает наличие реализованного компонента, прохождение E/R/L matrix или готовность Rush integration.

Единственный файловый результат задания — этот новый round-two отчёт. План, код, конфигурация, предыдущие отчёты и существующее пользовательское удаление `web/dist/.gitkeep` не изменялись.

## 7. Обязательные изменения в порядке выполнения

1. **P1 / S-R2-1:** расширить ordinary reducer на готовый Pending batch, зафиксировать bottom-up terminal propagation и добавить never-admitted leaf-stop/empty-nesting acceptance. Это должно быть сделано до заморозки batch lifecycle.
2. **P2 / S-R2-2:** ограничить waiting-level liveness закрытым bounded accounting domain и отсутствием мешающих structural gates; явно отделить обычный uncapped Runner от такого профиля. Добавить третий competitor group к доказательному E4 примеру и не выдавать unsafe default-group смесь за безопасную.
3. **P2 / S-R2-3:** выбрать frozen final producer boundary и доступ к retained Events после закрытия engine loop; согласовать blanket ErrClosed, R2/R3 и Laboratory journal completion. Close не должен ждать observers.
4. После этих решений integration owner в A0 фиксирует один согласованный contract для A1. Проверку добавленных сценариев и всех E/R/L gates выполняет он в A2 по плану; ни один такой gate в этом review не исполнялся.

**Передача:** прежние S1–S5 — **5 fixed / 0 open**. Открыты **3** описанных здесь пункта: **1×P1** (never-admitted batch reducer) и **2×P2** (полнота waiting accounting domain; финальный Events drain). Это автономные contract corrections, не разрешение реализовывать batches, начинать Rush integration или запускать проверки.
