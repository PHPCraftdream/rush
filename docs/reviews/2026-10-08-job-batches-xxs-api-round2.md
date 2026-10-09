# Job batches: второе независимое ревью автономного API

**Текущая ревизия:** `f48f883061cd66be30795c9dcf873f2da4ae79bd` (`f48f8830`), ветка `main`; git blob плана — `88be80856f854198cdf80d2ac29a3a8bb04939b2`.  
**Предыдущая ревизия:** `51599fa4`; предыдущий API-отчёт сохранён: [`2026-10-08-job-batches-xxs-api.md`](2026-10-08-job-batches-xxs-api.md).  
**План:** [`2026-10-07-job-batches-design.md`](../plans/2026-10-07-job-batches-design.md), revision 2, строки 1–1378. Все ссылки на разделы и строки ниже относятся к текущему плану, если прямо не сказано обратное.  
**Вердикт:** **needs correction — нужны ограниченные исправления автономного контракта до заморозки A0/A1.** Не blocked: недостающие решения доступны владельцу автономного компонента и не требуют начала интеграции Rush.

План прочитан полностью диапазонами; целиком прочитан предыдущий API-отчёт, дополнительно — предыдущие parallel-testing и states отчёты для пересекающихся вопросов. Идентичность commit/blob задана исходным scope основного агента; собственного запуска git-проверок не было. Appendix A рассмотрен как список заявленных исправлений, а не доказательство их достаточности.

Реальные изменения значительны: появился публичный selected-notice transport, `ControlReported`, полный `Launch`, committed replies, ownership-таблица, tree snapshot и рабочая семантика live output. Повторять прежнее утверждение об отсутствии этих механизмов было бы неверно. Остатки относятся к однозначности journal references и полноте pin/disposal-протокола; новые проблемы — к введённым Stop retry/timeout и окончательному закрытию Runner.

**Сводка:** A1–A9: **7 fixed, 2 partially fixed**, 0 unresolved, 0 regressed, 0 rejected. Ниже отдельно описаны два остатка этих API-findings, один пересекающийся остаток прежнего P3 и пять новых находок. Всего открыто восемь конкретных пунктов: **1 P1, 6 P2, 1 P3**. Это статическое ревью предлагаемого контракта; все мысленные исполнения и будущие acceptance-сценарии — **[INFERENCE]**, не результаты запуска компонента.

## 1. Обязательная closure matrix A1–A9

Стабильные A-ID соответствуют девяти пронумерованным находкам предыдущего API-отчёта. `fixed` означает закрытие именно прежнего дефекта в тексте контракта, а не успешное исполнение ещё не проверенной реализации.

| ID | Прежняя проблема | Статус | Точные текущие основания и результат независимой проверки |
|---|---|---|---|
| **A1** | Selected notices вычислялись Core, но не имели публичного пути к host/lab. | **fixed** | §2:253–256 задаёт metadata-only `Notice`; §4.1:596–608 объявляет отдельный `notice` record для каждого выбранного notice, включая несколько за переход. Диагностика и выбор уведомлений не смешаны (§4:588–592); gap и bounded retention объявлены (§4.1:611–617). Политика вложенности — 619–629, реальная public-API приёмка — R3:939–943. Доступ больше не требует собственного notice reducer у consumer. Новый shutdown/feed дефект R2-API-06 ниже не является прежним отсутствием notice API. |
| **A2** | Stop result не мог изменить engine-owned control error; общий текст errors/panics допускал synthetic settlement. | **fixed** | `ControlReported{node, token, op, code}` явно включён в Event (§2:244–249), поле control error есть в `NodeView` (257–261). §3.1.1:444–452 задаёт безопасные фиксированные Stop-коды, сохранение state/reservation и очистку ошибки только при nil acknowledgement; §3.3:534–536 ограничивает поздние reports. Допустимые completion states и invalid-state normalization определены в §3.1:377–381. Start/setup, accepted work, inspection и executor-owned goroutines разделены в §3.1.1:431–460; нет прежнего общего требования завершать accepted work из control error. Новые retry/timeout вопросы R2-API-03/04 не переоткрывают отсутствие Event. |
| **A3** | Не было producer/correlation-пути accepted Submit/Add и безопасного эталона порядка commands для replay. | **partially fixed** | `SubmitRequest.Ref`/`AddRequest.Ref` появились (§2:194–203, 209–211), accepted records несут `ref` и созданные IDs, commands и notices имеют собственные records (§4.1:600–610). §5.5:831–846 связывает их с reconstruction и comparison. Прежний недоступный transport и отсутствие ordered command descriptors закрыты. Но допустимость пустых/повторяющихся refs сценарных действий не ограничена: §5.2:731 и §5.5:832–844 обещают lookup именно по `ref`, без уникальности или occurrence-контракта. Оставшийся one-to-one correlation дефект — R2-API-01. Авторитетная граница trace отдельно относится к прежнему P3, см. R2-API-06. |
| **A4** | Runtime не имел источника полного `TaskSpec` для StartLeaf. | **fixed** | Полный `Launch` определён (§2:165–169); StartLeaf прямо несёт identity, token, kind, group, timeout, payload (§2:250–252). Read-only edge объявлен (§2.2:318); R5:956–958 проверяет глубокий Add и правильный Launch. Отдельные payload lookup/table или интерпретация payload в Engine больше не нужны. |
| **A5** | Не были определены acceptance/reply/context правила Submit/Add, значение Stop nil и разделение Wait error/outcome. | **fixed** | §2.3:329–339 выбирает атомарный queued→accepted/abandoned протокол: abandonment не имеет effect, acceptance возвращает committed результат даже после отмены caller context. Stop возвращается при intent acceptance, не при acknowledgement/termination (340–344); Wait любого terminal outcome возвращает Summary с nil error (345–347). §3.3:555–558 отделяет operation context от work lifetime; R5:951–955 содержит обе стороны acceptance barrier. Capability execution — отдельный этап после выдачи lease (§4.5:674–675), с caller errors (§3.1.1:453–454), context-bounded Output (§4.3:647–648) и `ErrExecutionDone` после accepted Inject (§3.3:544–548). Это не обещание доставки сообщения в момент выдачи lease. Прежняя неопределённость accepted Add и Stop/Wait return закрыта. |
| **A6** | Не было post-terminal ownership/disposal и защиты Output/Inject handles от Release; Inject/Settled гонка не была выбрана. | **partially fixed** | §4.5:672–685 запрещает finished registry executor, закрепляет единственного retained owner, leases для Output/Inject, запрет новых accesses после Release и Dispose после последней lease. Optional `Disposer` объявлен (§2:184–186). Inject acceptance/settlement и terminal/cancelling refusal определены (§3.3:544–548; §4.4:658–663), R4:944–950 требует in-flight read safety. Прежние Output/Inject гонки закрыты, однако вызов `Execution.Stop`, также выполняющийся вне loop, в перечень pins не входит; прежнее требование concurrency на одном Execution не замкнуто. Конкретный остаток — R2-API-02. Закрытие Runner добавляет отдельную новую ветку disposal, R2-API-05. |
| **A7** | Не были охвачены mutable boundaries limits, Inject, Launch payload и output bytes. | **fixed** | Все перечисленные прежде Runtime edges явно покрыты §2.2:314–322: copy `RunnerConfig.Limits.ByGroup`, deep-copy specs до queue, shared read-only Launch, copy Result, copy Inject, detached caller-owned output, non-aliasing returned values. §2.2:324–325 выбирает transfer для прямых Engine events и запрещает forest clone за переход. E9:921–924 привязан к этой таблице. Повторную копию Launch или output требовать не нужно. Отдельный дополнительный constructor edge прямого `NewEngine(Limits)` не покрыт таблицей — новая R2-API-08, а не повтор старых исправленных Runner edges. |
| **A8** | Snapshot не позволял обнаружить дерево по root после feed gap; Add IDs не имели порядка. | **fixed** | `BatchView` теперь содержит собственный batch view, counts и все descendants в pre-order (§2:265–267), Snapshot принимает любой batch node (§4.2:633–636). Parent/ordinal — явные поля (§2:257–258); Add возвращает direct children в input order (§2.1:295–297). R5:955–956 проверяет discovery по одному root ID после nested Adds. ID parsing у consumer не требуется. |
| **A9** | Не были определены live-end/EOF, stream model и recovery cursor после truncation. | **fixed** | §4.3:640–650 выбирает один absolute byte stream, merge stdout/stderr, чтение с `max(cursor, Oldest)`, `Truncated`/`Next`, ожидание data/EOF/context на live end и настоящий EOF после termination/выдачи retained bytes. Typed not-started/unsupported/released/unknown результаты объявлены в 652–654. Process cap и recent-byte retention согласованы (§5.4:792–795), R4:944–947 проверяет A→pause→B и recovery. Прежняя ложная EOF/busy-poll неоднозначность закрыта. |

**Закрытые находки:** A1, A2, A4, A5, A7, A8, A9.  
**Оставшиеся части прежних API-findings:** A3 → R2-API-01; A6 → R2-API-02. Ни один closed finding ниже не переиздаётся под другим названием.

## 2. Оставшиеся части прежних API-findings

### R2-API-01 — P2: references есть, но связь accepted input с единственным scenario action ещё не замкнута

**Классификация:** остаток A3; пересекается с producer/correlation-частью прежнего parallel-testing P3.

**Основание:** §2:194–203, 209–211; §5.2:729–733; §5.5:831–844. В ordinary Runner `Ref` намеренно optional. В сценарии submit/add перечисляют `ref`, но нет требования непустоты и уникальности между этими действиями. Reconstruction описан как получение specs из scenario **по ref**, а не по объявленному action index или `(ref, occurrence)`.

**Контрпример [INFERENCE]:** сценарий содержит два последовательных Add к `r`, оба с `ref: "grow"`, но первый добавляет leaf группы `g` с payload A, второй — leaf группы `h` с payload B. Оба запроса принимаются и получают разные ordinal IDs. Header digest корректен; оба accepted records содержат один ref. Реализация lab, индексирующая scenario actions map-ом по ref, подставляет B в оба replay events; другая реализация считает occurrences и подставляет A/B. Наличие ordered script даёт возможность придумать второе сопоставление, но план не объявляет его нормативным. Само поле ref и created IDs не выбирают одну из этих реализаций. Пустые refs дают аналогичную проблему.

**Минимальное исправление:** только в scenario validation потребовать непустые уникальные submit/add refs во всём scenario и проверять их до любых launches; при replay проверять соответствие ref типу действия и root/parent, а не одно существование ключа. `Runner.Ref` для произвольного автономного host оставить optional: mandatory replay metadata не должна становиться требованием каждого consumer. Альтернативный occurrence-контракт сложнее и здесь не нужен.

**Acceptance:** два Add с разными refs и payloads вокруг controlled settlement восстанавливают именно принятые inputs; повторяющиеся или пустые scenario refs отклоняются до Start. Один ref не может сослаться на действие другого типа/parent. Обычный Runner по-прежнему принимает корректную batch-операцию без Ref. Ordered command/notice comparison выполняется через уже добавленный public feed, без runtime hook и без сериализации полного Transition.

### R2-API-02 — P2: pin-протокол не охватывает ещё выполняющийся Stop

**Классификация:** остаток A6, конкретно прежнего требования concurrency/lifetime для Output/Inject/Stop на одном Execution.

**Основание:** §1.2:84–87 — blocking executor calls вне loop; §2:174–186 — Stop и Dispose обращаются к одному Execution; §3.1.1:444–450 — report и возврат Stop не обязаны совпадать; §4.5:674–681 — leases перечислены только для Output/Inject, Dispose разрешён после terminal и последней такой lease.

**Interleaving [INFERENCE]:**

1. Leaf Running; accepted Stop вызывает `Execution.Stop` вне loop. Вызов удерживается barrier-ом после запроса termination, но ещё использует свои ресурсы.
2. Независимая executor goroutine подтверждает termination через report. Loop принимает Settled; root terminal. Это допустимо: report подтверждает завершение работы, не завершение каждого управляющего вызова.
3. Caller принимает Release. Output/Inject leases нет, поэтому описанный счётчик равен нулю и Runner вызывает Dispose.
4. Dispose закрывает retained native handle или очищает структуру, которую продолжающийся Stop читает после снятия barrier.

Процесс уже остановлен, поэтому state/admission правила не нарушены; нарушена безопасность владения handle. Ни сохранение Go-reference, ни root terminal не исключает доступ к disposed ресурсам. Та же опасность возникает после control timeout: истечение context не уничтожает goroutine вызванной Go-функции.

**Минимальное исправление:** pin любой out-of-loop вызов на Execution, включая Stop, с получением pin до dispatch. Pin снимается при фактическом возврате вызова, а не при публикации `stop_timeout` или Settled. Dispose запускается ровно один раз после relinquish retained owner и **всех** active-call pins. Отдельно закрепить concurrency contract Output/Inject/Stop для executor; Dispose ни с одним из них не пересекается. Start, ещё не вернувший handle, требует учёта in-flight setup, а не выдуманной Execution lease.

**Acceptance:** compliant fixture подтверждает termination, пока Stop ещё удерживается barrier-ом; Release возвращает свой объявленный committed результат, новые accesses запрещены, Dispose ещё не начат. После возврата Stop Dispose происходит один раз. Повторить с Output lease и с control timeout; timeout не разрешает ранний Dispose. Выполняющиеся capabilities не блокируют state loop, а Wait сохраняет terminal Summary.

## 3. Новые находки revision 2 и пересекающийся остаток P3

### R2-API-03 — P2: timeout и retry Stop не имеют полного invocation-order протокола

**Классификация:** новая проблема введённых `ControlTimeout` и повторной выдачи StopLeaf; A2 остаётся закрытым.

**Основание:** §2:191, 246–252; §3.1.1:446–452; §3.3:534–536. Token идентифицирует admission attempt, а не конкретный вызов Stop. На одном token разрешены несколько control calls. Нет per-call one-shot правила, pending-control gate или явно выбранной сериализации их результатов. Запрет позднего результата terminal/stale-token не помогает, пока leaf всё ещё Cancelling на том же token.

**Interleaving [INFERENCE]:** Stop S1 превышает ControlTimeout; loop принимает `ControlReported(stop_timeout)`, но физический S1 ещё не вернулся. Повторный Runner.Stop выдаёт S2. S2 возвращает ошибку и устанавливает `stop_failed`; затем S1 поздно возвращает nil. Если worker публикует этот nil как ещё один ControlReported, он очищает более новую ошибку S2: node/token/op у него всё ещё корректны. И наоборот, поздняя ошибка старого вызова способна затереть новый успешный acknowledgement. Также два повторных Stop до ответа S2 оба видят «last control report failed» и могут выдавать повторные effects.

Это не доказательство, что будущая реализация обязательно отправит оба ответа: правильный one-shot latch может их исключить. Но сейчас такое правило явно установлено только для completion (§3.3:550–553), а не для control timeout/results.

**Минимальное исправление:** выбрать boring per-Execution control serialization: не более одного физического Stop in flight, определённое coalescing/pending поведение повторных intent, ровно один логический ControlReported на вызов. Если timeout победил, поздний фактический возврат лишь снимает pin, но не меняет control error. Retry разрешается по выбранному pending/physical-return правилу; loop при этом не ждёт executor. Это можно реализовать внутренними invocation latches, без расширения публичного callback и без нового Core scheduler. Если намеренно допускаются concurrent Stop calls, тогда нужен отдельный control invocation ID, включая command/event/journal; launch token его не заменяет.

**Acceptance:** barriers воспроизводят timeout S1, retry S2, поздний nil/error S1 и два повторных caller Stop во время S2. В feed ровно один логический результат на вызов; старый S1 не очищает/не заменяет актуальную ошибку S2. Одновременно другой root продолжает обслуживаться. Slot освобождается только по одному termination report, а pins — по физическому окончанию своих calls.

### R2-API-04 — P2: повторный Stop batch/root не определён через failed descendant controls

**Классификация:** новая недоопределённость retry policy, а не отсутствие ControlReported из A2.

**Основание:** §2:244–252; §2.3:340–344; §3.1.1:451–452; E6:910–914. Stop адресует любой node/subtree/root, но StopLeaf и token-based ControlReported относятся к leaf execution. Правило reissue проверяет «node whose last control report failed»; отдельного roll-up/обхода для batch node нет.

**Контрпример [INFERENCE]:** `Stop(r)` останавливает root с leaves `r.1` и `r.2`. Stop `r.1` возвращает error, Stop `r.2` — nil; root остаётся Cancelling, control error виден у `r.1`. Operator снова вызывает `Stop(r)`. У batch `r` собственного failed ControlReported нет, поэтому буквальная ветка «otherwise repeated Stop is a no-op» не повторяет failed descendant effect. Исполнение `r.1` могло бы успешно остановиться при retry, но для повторения исходной subtree-операции контракт не даёт правила. Другая реализация обойдёт потомков и retry выполнит.

**Минимальное исправление:** определить repeated Stop для batch/root как обход адресуемого поддерева: повторять controls только для ещё accepted nonterminal leaves с failed last control result, учитывая pending gate R2-API-03; уже acknowledged/in-flight/terminal leaves не трогать. Первый cancellation cause не меняется. Никакого агрегированного control error у batch или consumer-side reducer для этого не требуется.

**Acceptance:** root и nested-batch Stop с одним failed и одним acknowledged child; повтор той же batch/root операции выдаёт retry только failed leaf, не sibling и не terminal leaf. Успешный retry не завершает root до termination callback. После подтверждений один root notice/Wait outcome с прежней cause. Расширить E6 этой consumer-visible веткой, а не тестировать повтор только на leaf ID.

### R2-API-05 — P2: Closed закрывает единственный Release-путь, но не берёт disposal на себя

**Классификация:** новая lifetime-ветка optional Disposer и Closed; прежние Output/Inject leases A6 уже признаны добавленными.

**Основание:** §3.4:564–580; §4.5:672–685; §2:184–192. При terminal roots loop выходит и любой метод, кроме Close/Batch handles, возвращает ErrClosed. Вызов Dispose объявлен только после Release и последней lease. Правила implicit Release/disposal при Closed нет, как нет и результата error/panic/timeout самого Dispose.

**Контрпример [INFERENCE]:** после terminal report Execution удерживает retained output в открытом внешнем ресурсе и реализует Disposer; это соответствует заявленному optional capability. Host context отменяется до explicit Release. HostClosing обрабатывается, все roots уже terminal, Runner становится Closed. `Release(r)` теперь получает ErrClosed; executor не вправе держать finished registry, но Runner остаётся доступен host и продолжает держать handle. Ни событие Closed, ни Go GC не обязаны вызвать Dispose. Native retained resource остаётся без объявленного owner cleanup path. Аналогичный порядок возможен при автоматическом окончании drain последним report.

**Минимальное исправление:** при окончательном закрытии Runner должен сам relinquish retained ownership всех terminal execution handles и организовать Dispose после последних pins, независимо от существования state loop. Успешный bounded Close должен иметь определённую cleanup гарантию; ошибку/timeout Dispose нельзя молча выдавать за выполненное освобождение, но и нельзя превращать в task settlement. Зафиксировать результат failed disposal в существующем Release/Close lifetime-контракте, включая once-only правило и late actual return. Здесь не нужен отдельный Store, DB, registry finished executions или Rush hook.

**Acceptance:** terminal root с counted native-resource Disposer, затем host cancellation без Release: успешный Close освобождает ресурс ровно один раз, cached Batch.Wait остаётся доступным. Повторить с активным Output/Stop pin: disposal только после его окончания. Отдельные error/panic/timeout Dispose дают объявленный cleanup result, не nil, означающий успешно выполненную очистку; root state/counts при этом не переименовываются и execution admission не освобождается повторно.

### R2-API-06 — P1: footer не привязан к авторитетному final producer cursor, а Closed отрезает drain feed

**Классификация:** пересекающийся незакрытый остаток **P3** предыдущего parallel-testing отчёта; новый API shutdown/feed конфликт. Не повтор прежней недоступности notices A1 или commands A3.

**Основание:** §4.1:596–617; §3.4:574–580; §5.2:733 допускает scripted Close; §5.5:831–849 задаёт footer `last_cursor` и обещает отвергать removed tail; L6:979–982. EventPage имеет текущий Head, но нет frozen end boundary и обязательного drain-through неё. `last_cursor` не определён как значение от producer; его может заполнить writer своим последним полученным cursor. После Closed Events по общему правилу получает ErrClosed, даже если retention содержит непрочитанные records.

**Interleaving [INFERENCE]:**

1. Корень уже Completed; writer прочитал все его Settled/command/notice records до cursor H. Footer states/counts можно получить из cached Batch.Wait.
2. Следующее script action `close` принимает HostClosing, создающий record H+1. Он не меняет уже terminal root state/counts и может не породить commands/notices.
3. Loop становится Closed до следующего Events writer. Writer больше не может прочитать H+1 и не получает объявленную авторитетную final boundary.
4. Footer с `last_cursor: H`, `complete: true` и корректными states/counts удовлетворяет указанным structural checks: prefix cursors contiguous, digest тот же, replay prefix даёт те же terminal roots. Ни final counts, ни equality run/replay не доказывают присутствие последнего HostClosing.

Если writer вместо этого всегда объявляет такой journal incomplete, валидный run со штатным Close не может гарантированно получить обещанный полный trace. «Читать continuously» не исключает эту гонку, а прибавление одного record retention без протокола не устраняет её.

**Минимальное исправление:** freeze producer boundary при завершении run/Closed: immutable final Head (с однозначной inclusive/exclusive семантикой) и соответствующая transition boundary. Footer получает `last_cursor` именно из неё. Events после остановки loop должен позволять прочитать retained suffix до frozen boundary и возвращать объявленный конец только после неё; это может быть read-only cached feed, не новый observer framework. Writer перед footer обязан drain до boundary; ErrGap/write failure остаётся неполнотой, Snapshot не чинит потерянную историю. Live state loop и cancellation не ждут writer. Если финальный cutoff выбран до Close, он должен быть явным согласованным barrier-ом с запретом дальнейших учитываемых accepted events, а не writer-prefix эвристикой.

**Acceptance:** удержать writer перед terminal/Close переходом, штатно закрыть Runner, затем разрешить чтение: trace доходит до авторитетного final cursor и содержит последний выбранный root notice и HostClosing. Повторить с tail CapacityAvailable, который не меняет counts: отсутствие такого accepted tail должно обнаруживаться. Удалённый last record при сохранённом authoritative footer отвергается; overflow даёт incomplete, не восстановленный snapshot trace. Replay выполняет только Engine, не создаёт executor/process. R3/L4/L6 проверяют этот endpoint, включая штатное закрытие, а не только EOF JSONL-файла.

### R2-API-07 — P2: sanitization Result.Code не охватывает Code() ошибки Start

**Классификация:** новый privacy boundary introduced Start error coding; безопасный Stop error transport A2 закрыт.

**Основание:** §2:159–164, 209–211; §3.1.1:431–437; §4.1:600–611. Ограничение длины/алфавита и замена на `invalid_code` объявлены для **Result.Code на report boundary**. Но operational Start error получает код напрямую из `Code() string`; этот путь не является Result callback. Затем code входит в публичный start_rejected record и journal.

**Контрпример [INFERENCE]:** consumer executor возвращает Start error с `Code()` равным `missing program: PRIVATE_SENTINEL / argv=...`, либо строкой длиной существенно больше 64 bytes. Он не возвращает execution и соблюдает setup-cleanup правило. Буквальный §3.1.1:434 сохраняет эту строку как код Failed; нормализация из §2:210–211 для Result callback здесь не выполнялась. Feed получает непредусмотренный текст/arguments, хотя §4.1:610–611 их запрещает. Речь именно об отсутствии общего code boundary, а не о необходимости угадывать секреты в корректном коротком machine code.

**Минимальное исправление:** один shared code validation/normalization contract для всех runtime-produced code fields: Result, optional Start error Code(), StartRejected и ControlReported; никакого `error.Error()`/panic text в records. На прямой Engine boundary недопустимый code должен иметь определённое reject/normalization правило, а не рассчитывать на runtime callback, которого там нет. Валидный stable code сохраняется; invalid получает предусмотренный safe fallback. Это не новый error taxonomy/framework.

**Acceptance:** invalid/oversized Start Code(), invalid Result.Code и panic/error text с sentinels не появляются в records/journal; сохраняются безопасные `invalid_code`/фазовые fallback codes. Валидный `missing_program` и `exit_<n>` не теряются. Stop error/panic/timeout по-прежнему дают только фиксированные control codes и не settlement. Дополнить privacy gate E9 именно Start-error boundary, которой в текущем списке sentinels нет.

### R2-API-08 — P3: constructor ownership прямого NewEngine(Limits) остаётся вне таблицы

**Классификация:** новая обнаруженная граница прямого pure API; закрытые Runtime edges A7 не переиздаются.

**Основание:** `Limits.ByGroup` — map (§2:153–158); публичен `NewEngine(Limits)` (§2:216), replay создаёт fresh Engine (§5.5:842–845). §2.2:316 задаёт copy только для `RunnerConfig.Limits`/NewRunner; 324 задаёт transfer только для **events** в Apply. Constructor argument не является Event.

**Контрпример [INFERENCE]:** direct Engine consumer создаёт engine из map `g:1`, затем переиспользует тот же limits container для варианта `g:2` и подаёт identical events исходному Engine. Реализация с constructor copy остаётся на лимите 1; реализация с map alias становится на лимите 2. Для самого NewEngine ни разрешение дальнейшей caller mutation, ни ownership transfer не установлены. Это не утверждение о наличии alias в текущем коде.

**Минимальное исправление:** добавить одну constructor ownership строку. Наиболее экономно в существующем стиле pure API — явно transfer `Limits`/ByGroup в NewEngine; прямые lab/core callers передают собственный map и больше его не меняют, а NewRunner остаётся единственным copy-boundary для внешнего caller. Допустим и detached-copy вариант, если NewRunner делегирует единственную копию, а не клонирует limits дважды. Не нужен immutable-map wrapper или второй constructor.

**Acceptance:** выбранное правило отражено в документации shared constructor; direct replay/tests используют этот режим владения. В transfer варианте отдельные Engines получают отдельно принадлежащие им карты, изменение исходного caller-owned шаблона не меняет их budgets; в copy варианте mutation аргумента после NewEngine не меняет accepted scheduling. Runtime mutation gate из E9 остаётся прежним и проходит без лишних копий forest/Launch/output.

## 4. Проверка пересечения с прежним parallel-testing P1–P3

- **P1: fixed в проверенной API/factory части.** §2:116–126 ясно отделяет реальные Go data declarations A0 от документированных signatures; bodies создаёт единственный owning slice. §5.3:759–771 и §5.4:787–790 задают настоящие фабрики, §7.1:1000–1005 не пишет Go prototypes/empty concrete executors в schema. §7.2:1041–1045 запрещает подменять их placeholders ради ранней компиляции. Прежний конфликт двух Go function declarations не сохраняется; callable dependency наступает на joint gate, не до fan-out.
- **P2: основной прежний API дефект ID-only gates закрыт.** `AwaitStart` возвращает реальный Launch, остальные actions используют token (§5.3:764–780); late/unknown/settled attempts не latch-ятся на будущий start. §5.2:729–744 задаёт vocabulary и bounded awaits. Этого достаточно, чтобы не требовать fixture второго scheduler или sleeps. В данном API-отчёте не заявляется полное закрытие всех деталей сценарного слоя P2: они принадлежат parallel-testing review. В частности, lifetime вызова Stop и его completion — разные gates; их не заменяет факт AwaitStop.
- **P3: partially fixed.** Header version/scenario digest, footer, gap/write-failure handling и negative integrity gates реально добавлены (§5.5:828–849; L6:979–982). Но они не дают producer-owned final boundary; конкретный остаток — R2-API-06. Однозначность scenario refs также требует R2-API-01. Эти замечания не означают, что added digest/footer не существуют, и не требуют durable/exactly-once storage.

## 5. Аудит новой поверхности API и лишнего coupling

В текущем документе имя options-типа — **`RunnerConfig`**, не `RuntimeOptions` (§2:188–193). `Limits`, `Executor`, positive `ControlTimeout` и positive `RecordRetention` относятся к собственным обязанностям Runtime; второй options-type или Rush configuration facade не нужен. Два последних поля полезны, но порождают конкретные lifetime/endpoint обязательства R2-API-03/05/06, а не автоматически решают их.

`Execution` остаётся минимальным Stop interface; OutputReader/Messenger/Disposer — optional assertions (§2:174–186). Для autonomous component это нормальное разделение: capability-less task не должен иметь no-op output/input, а Native cleanup может действительно потребовать Disposer. Ошибка текущего текста — в незамкнутом lifetime этого capability, не в необходимости mandatory Close/Store на каждом executor. Routing process/controlled kinds и payload validation остаются consumer concern (§1.3:95–99; §5.2:726–727), не ветвями Engine.

Полный immutable Launch и безопасный command descriptor теперь намеренно различаются. Journal описывает **команды, произведённые Engine**, а не доказывает физический порядок OS execution; реальный dispatcher ordering проверяется Runtime gates, не вторым scheduler/replay executor. Accepted Settled projection сохраняет outcome state, а winning cancellation cause — отдельное engine правило (§3.3:531–533; §5.6:868–871). Summary/Details, message и output для структурного replay не нужны. При материализации A0 record union следует сохранить полную accepted-event форму, включая ControlReported.op либо явно фиксированную единственную stop operation, и один общий порядок event/command/notice records внутри transition. Это конкретизация уже заявленного projection contract (§4.1:600–608), а не самостоятельное повторение закрытого отсутствия transport.

Snapshot/Wait DTO теперь оправданно разные: согласованное дерево для discovery и маленький cached immutable aggregate для окончания ожидания. Не требуются children registry, List API, consumer ID parsing или сериализация NodeView в journal. Output cursors/retention/EOF выбраны достаточно для прежних acceptance-сценариев. Не переоткрываю A9 требованиями другого stream backend, polling mode или новым transport.

Generic request acceptance из §2.3 относится к linearization в loop; Output/Inject выполняются после lease и могут иметь executor/context errors без rollback задач. При A0 следует явно обозначить этот internal-reply/public-effect этап и исключение Close/Wait как waiters, чтобы слово «prompt reply» не читалось как обещание немедленного завершения ReadOutput. По совокупности §§2.3, 3.3, 4.3–4.5 это не оставшийся прежний Add ambiguity и не отдельный blocker настоящего отчёта.

## 6. Автономные A0/A1 blockers и законно отложенная интеграция

**До A1 обязательно замкнуть автономно:** producer final boundary/read-after-close; pins всех execution calls и once-only control result; repeated subtree Stop; Closed disposal responsibility/failure semantics; lab ref validation; safe code normalization. Constructor ownership — меньший P3, также решаемый одним явным правилом при A0. Эти вопросы воспроизводятся с обычными Go fixtures и не зависят от агента, MCP, ledger или модели.

**Не являются blockers A0/A1:**

- Rush numeric defaults и mapping к session/ledger admission, waiting-level group configuration (§8:1123–1126; §9.2:1201–1210).
- Durable claims, recovery, reservation/ack, transactional notification delivery (§9.2:1233–1244; §9.3:1275–1305). Bounded Events не становится durable delivery queue после исправления R2-API-06.
- Integrated terminal authority и future additive `Settled.Authoritative` (§9.2:1212–1223). Обычный lab callback и current acceptance-order semantics не обязаны сейчас принимать DB committed winner/cause или импортировать ledger.
- MCP connection/session cancellation, tool policy/auth routing, held questions, legacy-agent outcome migration и media envelope (§9.1:1160–1169; §9.2:1180–1199, 1225–1231; §9.3:1269–1287).
- Operator cross-process control, model tools и WebUI (§9.4:1307–1340). Второй process-local Runner не является их реализацией, но автономный API и не должен её включать.

Исправление lifecycle error/retention не требует менять эти отложенные решения или добавлять Rush internals в standard-library Core/Runtime.

## 7. Сильные стороны и пределы покрытия

**Сильные стороны:**

1. Самостоятельная граница one-module/one-implementation, stdlib Engine/Runtime и lab-consumer сохранена (§0:19–39).
2. Accepted execution, cancellation intent, control result и confirmed termination разделены; setup cleanup явно обязанность executor (§3.1:377–398; §3.1.1:431–460).
3. Большинство прежних producer/consumer дыр закрыто конкретными public types/records, а не будущим hook или обязательным Store.
4. Ownership, tree discovery, output cursor/EOF и mutation replies получили observable gates R3–R5; read-only Launch избегает бессмысленной повторной копии (§2.2; §6.2:939–958).
5. Factory ownership и joint compilation boundary исправлены без prototypes, mocks и serial implementation pipeline (§2:116–126; §7.1–7.3).

**Пределы:** проверен весь текущий план, но не реализация будущего jobbatch, не actual process isolation и не текущие Rush launch paths. State reducer/fairness/generation и интеграционные authority/security вопросы не являются полнотой этого API-отчёта; родственные first-round отчёты использованы для происхождения/пересечения findings, не как доказательство. Никакие builds, tests, race, lint, formatters, smoke, replay, runtime или git verification не запускались. Acceptance-сценарии предложены будущему integration owner; ни один не объявлен прошедшим. Предыдущие отчёты и shared plan не изменялись; единственная созданная репозиторная сущность этой работы — настоящий round-two report.

## 8. Обязательные изменения в порядке выполнения

1. **P1, R2-API-06:** замкнуть producer-owned final cursor/transition boundary, terminal retained-feed чтение и journal drain-through-footer. Включить no-state-change tail и Closed interleaving в R3/L4/L6.
2. **P2, R2-API-02/03:** распространить pins на Stop, выбрать per-handle control serialization/pending правило и один логический результат на вызов; timeout не освобождает физический pin и late return не меняет новую ошибку.
3. **P2, R2-API-05:** дать Closed собственную cleanup responsibility для unreleased terminal handles, defined bounded disposal result и once-only поведение; cached Wait не терять и task state из disposal failure не менять.
4. **P2, R2-API-04:** определить повтор Stop root/subtree через failed descendant controls и добавить именно batch/root retry gate к E6.
5. **P2, R2-API-01/07:** заморозить nonempty/unique lab refs и общую safe-code границу для Start errors/Result/control inputs; ordinary Runner Ref оставить optional. Проверить через реальные journal/public records, не через приватную таблицу recorder.
6. **P3, R2-API-08:** дописать constructor ownership прямого NewEngine без двойного cloning limits.
7. После этих решений A0 materializes один согласованный набор declarations/schema; implementation и проверки остаются отдельным поручением. На A2 один владелец запускает актуальные E6/E9/R3–R5/L4/L6 и новые barrier/disposal cases, затем фиксирует фактические commands/results и platform limits. Этот review не даёт разрешения на implementation, agent launches или позднюю Rush интеграцию.
