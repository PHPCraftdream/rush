# Job batches: независимое ревью состояний, admission и гонок

**Рассматриваемый коммит:** `51599fa4`, ветка `main`.  
**Документ:** [`docs/plans/2026-10-07-job-batches-design.md`](../plans/2026-10-07-job-batches-design.md).  
**Вердикт:** **needs correction / нужны исправления до заморозки A0**.

План прочитан полностью, строки 1–727; основной предмет — §§1–4, E1–E9 и R1–R4. Ниже ссылки на строки относятся к этому документу. Рассматривается предложенный контракт, а не поведение реализованного компонента. `[INFERENCE]` обозначает мысленное исполнение последовательности событий или вывод о возможной реализации; эти сценарии не запускались.

Самостоятельная поставка не требует предварительной интеграции с Rush. Однако остаются две существенные опасности для liveness/safety и три неоднозначности, которые затрагивают общий контракт Core/Runtime/Laboratory. Их можно устранить в самом плане/A0 без DB, MCP-сервера, модели или изменения существующих запусков.

## 1. Находки автономной фазы

### 1. P1 — поздний capacity refusal может потерять уже принятый сигнал доступности

**Основание:** §1.2, строки 72–80 — результаты executor поступают через сериализующий loop, но работа выполняется вне него; §2, строки 135–141, 186, 201–204 — token идентифицирует попытку, `CapacityAvailable` адресует группу; §3.2, строки 329–332 — отказ возвращает leaf в Pending и блокирует группу до CapacityAvailable/релевантного settlement; E5, строки 469–471 — отсутствие spin и последующий единственный запуск.

**[INFERENCE] Interleaving:**

1. Единственный leaf `L` группы `g` получает `StartLeaf(L, token=1)` и становится Starting. Внутренняя reservation занята.
2. Executor обнаруживает отсутствие своей capacity. Принятого исполнения нет; результат `Start` с typed refusal ещё не доставлен в loop.
3. Capacity освобождается. Loop принимает `CapacityAvailable(g)`. `L` пока Starting, поэтому повторно его запускать нельзя; уже заблокированных pending leaf в группе может вообще не быть.
4. Затем loop принимает `StartRejected(L, 1, capacity)`. По опубликованному правилу leaf возвращается в Pending, reservation освобождается, группа блокируется.
5. Capacity фактически уже свободна, но новых сигналов и других accepted executions нет. Работа остаётся Pending навсегда.

Сериализация не устраняет этот порядок: событие доступности законно может обогнать результат асинхронного `Start`. Token защищает от отчётов чужой попытки, но сам по себе не хранит факт более нового изменения доступности. Аналогична гонка с релевантным settlement, принятым между авторизацией попытки и её отказом.

**Минимальная правка:** определить поколение доступности группы либо эквивалентную защёлку wake-up. Попытка запоминает поколение при авторизации; каждый признанный сигнал доступности обновляет его, даже если группа ещё не заблокирована. Поздний отказ не должен стирать сигнал, принятый после авторизации этой попытки: допускается одна повторная попытка за такой сигнал, затем новый отказ снова блокирует группу. Явно определить, какие accepted-work settlements обновляют это поколение. Это внутреннее состояние Engine; новый публичный scheduler или обязательный Store не нужны.

**Приёмочный сценарий:** controlled executor задерживает возврат refusal после проверки capacity. Принять `CapacityAvailable(g)` перед `StartRejected`, затем разрешить возврат. Без второго сигнала `L` должен получить новую попытку с другим token и исполниться ровно один раз. Повторить с обратным порядком событий и с релевантным settlement вместо CapacityAvailable. Старый отказ от другой in-flight попытки не должен повторно заблокировать группу после более нового wake-up; без новых сигналов отказавшие попытки не должны крутиться. Связанные критерии: E4/E5/E8/R1.

### 2. P1 — общее требование «errors/panics settle accepted work» нельзя применять к ошибкам управления или неизвестному состоянию setup

**Основание:** §3.1, строки 286–304 — rejected Start подтверждает отсутствие accepted execution, Starting/Cancelling держат slot до rejection/подтверждённого termination, ошибка Stop оставляет item nonterminal; §3.3, строки 354–357 — без ограничения по месту ошибки сказано «Executor errors/panics must settle accepted work once»; §4, строки 396–398 — внешние операции выполняются вне loop и возвращают ошибки; E6, строки 472–473, и R1, строки 483–484 — отсутствие ложного terminal и утечек reservation при panic.

**[INFERENCE] Interleaving для accepted work:**

1. `A` уже Running при global limit 1; `B` ждёт.
2. Stop для `A` принят; состояние Cancelling, причина зафиксирована.
3. `Execution.Stop` возвращает ошибку или паникует, но фактическое исполнение `A` продолжает работать и completion callback ещё не вызван.
4. Если общий обработчик errors/panics переводит `A` в terminal и освобождает slot, стартует `B` при ещё живом `A`. Если он не переводит `A` в terminal, буквальная общая формулировка о settle не выполнена, зато выполнено специальное правило Stop failure.

Есть отдельная граница setup: `Start` мог создать дочерний процесс, а затем запаниковать до возврата `Execution`. У Runner нет handle, чтобы установить факт termination. Recovery паники сам по себе не является ни подтверждением завершения процесса, ни корректным capacity refusal.

**Минимальная правка:** разделить правила ошибок по фазам:

- Failed/rejected Start, включая поддерживаемый контрактом panic setup, должен гарантировать отсутствие оставшегося исполнения. Cleanup setup — обязанность executor, а не вывод Runner из наличия error/panic. Для R1 явно указать эту гарантию и её границу.
- После принятого Start terminal/освобождение reservation допустимы только по completion, подтверждающему прекращение исполнения. Ошибка или panic Stop/Inject/Output — control failure, а не самостоятельное доказательство termination.
- Если executor не может подтвердить cleanup, состояние остаётся незавершённым; bounded Close вправе вернуть ошибку. Не маскировать это автоматическим Failed/Cancelled ради «no leaked slot».
- Паника внутри собственной goroutine executor требует его собственного протокола cleanup/completion; её нельзя обещать поймать общим recovery вокруг вызова Start.

Проблема здесь именно в границе safety/settlement. Не предлагается расширять движок до наблюдателя произвольных процессов или считать недостоверный callback доказательством остановки.

**Приёмочный сценарий:** execution паникует/ошибается в Stop, но удерживает реальную работу до отдельного completion gate. До gate `A` nonterminal, `B` не стартует, Done/terminal notice отсутствуют; control error доступен для инспекции. После единственного подтверждённого completion — одно settlement и одно освобождение slot. Для Start проверить panic до side effects и panic после создания ресурса с подтверждённым executor-owned cleanup: rejected attempt не оставляет живого ресурса перед запуском следующего leaf. Связанные критерии: E6/E8/R1/R2.

### 3. P2 — stop-on-fail пропускает вложенный batch, но общий reducer объявляет его Completed

**Основание:** §3.1, строки 306–311 — любой не explicitly stopped batch после terminal children получает Failed, Cancelled либо «otherwise Completed»; §3.2, строки 319–320 — Sequential обрабатывает direct children; E3, строки 464–466 — pending successors пропускаются; §4, строки 377–381 — nested aggregate может быть объявлен NotifyEach-родителем. Отдельного перехода/приоритета policy-Skipped для batch node нет.

**[INFERENCE] Сценарий:** `S = Sequential(StopOnFail, NotifyEach)` содержит `[a, B]`, где `B = Parallel(NotifyAll)` содержит pending leaves `[b, c]`. После Failed у `a` successor `B` не запускается, `b` и `c` получают Skipped. Но `B` не был явно остановлен; у его terminal children нет Failed/TimedOut/Interrupted/Cancelled. Опубликованный reducer даёт `B = Completed`. Альтернативная реализация пометит сам `B` как Skipped по E3, то есть получатся разные NodeView и selected notice при одной последовательности событий.

Root `S` остаётся Failed в обоих вариантах, поэтому тест только root state и leaf counts этот дефект контракта не обнаружит. Особенно заметен он в уведомлении `B`: «завершён» вместо «пропущен» для неисполненного successor.

**Минимальная правка:** отдельно определить terminal state policy-пропущенного batch и его приоритет перед обычным aggregation. Наиболее прямой вариант — Pending successor batch получает Skipped; его ещё Pending descendants получают Skipped, уже terminal descendants не переписываются. Обычный reducer применяется только к реально обработанному batch без latched stop/skip причины. Дополнить batch lifecycle, а не оставлять его под неявной интерпретацией leaf diagram. Уже Completed empty batch не должен задним числом переименовываться в Skipped.

**Приёмочный сценарий:** для приведённого дерева проверить не только `S = Failed` и counts `Failed=1, Skipped=2`, но и `B = Skipped`, отсутствие StartLeaf для `b/c`, одно выбранное notice `B` со state Skipped и отсутствие обычных leaf notices под `B.NotifyAll`. Повторить с вложенным Sequential и с уже terminal descendant; прежний outcome не должен измениться. Связанные критерии: E2/E3/E7/E8.

### 4. P2 — «MaxParallel zero inherits the applicable budget» не задаёт вычислимую однозначную семантику для Sequential и смешанных групп

**Основание:** §2.1, строки 243–251 — zero наследует applicable budget, Sequential допускает только zero/one, defaults разрешаются до validation; §3.2, строки 319–325 — Sequential ограничивает порядок direct children, nested mode управляет внутренней работой, любой ancestor MaxParallel считает все активные leaves; E2/E4, строки 462–468.

**[INFERENCE] Неоднозначный пример:** global limit 3; root `Sequential(MaxParallel=0)` содержит `Parallel(MaxParallel=3)` с тремя leaves и следующий direct sibling. Возможны разные прочтения:

- Zero заранее нормализуется в global budget 3; последующая проверка «Sequential permits only zero/one» отвергает исходно допустимый spec.
- Sequential zero означает effective 1; ancestor limit полностью сериализует leaves вложенного Parallel.
- Zero означает отсутствие дополнительного local cap; Sequential закрывает следующие direct children, но внутри текущего Parallel могут работать три leaves. Explicit `Sequential(MaxParallel=1)` при этом намеренно ограничивает всё поддерево одним leaf.

Последние два варианта неэквивалентны. Формулировка про собственный nested mode не выбирает между ними, поскольку рядом задан общий ancestor cap. У batch также нет Group: если в нём есть leaves разных групп с разными лимитами, «applicable budget» нельзя без дополнительного правила заменить одним inherited group числом.

**Минимальная правка:** опубликовать точный admission predicate и порядок normalization/validation. Вариант, минимально сохраняющий правило direct-child sequencing: `MaxParallel=0` не добавляет собственного ancestor cap; каждый leaf отдельно проходит global, свой group и все explicit ancestor caps. Sequential gate разрешает только текущего direct child; explicit MaxParallel=1 дополнительно ограничивает все leaves под ним. Zero не заменяется group budget при validation. Если выбран другой смысл Sequential zero, его надо явно записать вместе с последствиями для вложенного Parallel, а не оставлять двум slices выбор разных трактовок.

Это не предложение выбрать новые численные defaults: они по-прежнему принадлежат caller. Требуется определить смысл уже опубликованных zero/one и разделить group budget от subtree budget.

**Приёмочный сценарий:** закрепить ожидаемый результат для Sequential zero/one с nested Parallel при global 3. Для предложенной семантики: zero допускает overlap внутри текущего ребёнка, one ограничивает его одним leaf; следующий direct sibling не стартует до settlement вложенного batch в обоих случаях. Отдельно tree со смешанными группами `g:1`, `h:2` и MaxParallel zero не получает общий cap 1 лишь потому, что первый leaf относится к `g`; global/ancestor пределы всё равно соблюдаются. Повторить с Starting и Cancelling, которые продолжают занимать reservations. Связанные критерии: E2/E4/E6.

### 5. P2 — не зафиксирован lifecycle Runner после deadline у Close

**Основание:** §1.2, строки 72–75 — snapshots и executor reports проходят через единственный loop; §1.3, строки 89–92 — final Snapshot/Wait авторитетны; §3.3, строки 359–367 — public contexts ограничивают submission/waiting, host shutdown требует Interrupted и не позволяет falsely terminal при timed-out Close; §4, строки 391–394 — handles удерживаются до допустимого Release; R2/R3, строки 485–488.

**[INFERENCE] Interleaving:** `A` accepted и Running; HostClosing принят; Stop acknowledged, но execution ещё не завершилось. Close достигает deadline и возвращает error. После этого executor корректно подтверждает termination своим callback. По правилам latched cause результат должен наконец стать Interrupted, а не оставаться Cancelling навсегда.

Требование «не сообщать terminal при timeout Close» задаёт snapshot в момент возврата, но не описывает, кто дальше принимает late Start results/completion, можно ли повторно ждать Close и какие state/control методы ещё доступны. Реализация loop с выходом по `hostCtx.Done()` или `closeCtx.Done()` не создаст ложный terminal, но потеряет позднее подтверждение, оставит Batch.Done открытым и не позволит освободить retained execution. Публичный контекст ожидания и lifetime drain-loop здесь нельзя отождествлять.

**Минимальная правка:** явно разделить переход в closing и окончательное завершение loop. Host cancellation запускает Interrupted intent и запрет новых Submit/Add, но не прекращает обработку результатов in-flight Start и accepted executions. Close context ограничивает только ожидание cleanup; после его ошибки drain продолжается до подтверждений. Зафиксировать возможность повторного Close и доступность View/Snapshot/Wait во время drain, а также момент окончательного закрытия API. Если adapter не подтверждает termination, оставлять unfinished work и ошибку Close, не освобождать slot фиктивным settlement. Контексты termination cleanup не должны становиться бесполезными лишь из-за уже отменённого host/request context.

**Приёмочный сценарий:** после HostClosing удержать completion за barrier, добиться deadline Close и проверить: новых запусков нет, item nonterminal, Release отказан, snapshot остаётся доступным. Затем открыть barrier, принять natural completion с уже latched Interrupted cause, получить один terminal root result/notice и закрытие Done; повторное bounded Close завершается. Повторить со Start, возвращающим handle уже после первого Close deadline. Связанные критерии: E6/E8/E9/R1/R2/R3.

## 2. Конкретно проверенные границы без отдельных находок

Эти выводы о согласованности текста не являются результатами исполнения.

### Starting / Running / Cancelling и callback

- §3.1, строки 286–294, совместим с immediate callback: Runner удерживает report до результата Start; при успешном Start публикуются Started, затем Settled. Callback плюс failed Start не превращается в успех: authoritative rejection и diagnostics уже предусмотрены.
- Stop, принятый до результата Start, фиксирует cause. Успешный Started после этого не должен сбросить Cancelling в Running; rejected Start подтверждает отсутствие исполнения и позволяет settlement с прежней причиной. Это следует из правил latched cause и reservation, а не требует повторного запуска leaf.
- Completion, физически произошедший до Stop, но ещё buffered до возврата Start, может проиграть Stop по acceptance order. Это не отдельная ошибка плана: §3.3, строки 338–348, намеренно выбирает acceptance order, а не wall-clock callback order.
- Nil от Execution.Stop — acknowledgement, не terminal. Physical termination определяется completion. L2, строки 493–494, можно согласованно прочитать как более сильную обязанность именно process executor ждать свой child; это не универсальная гарантия любого Execution.Stop. Return-семантику Runner.Stop стоит зафиксировать в A0, не подменяя её Batch.Wait.

### Token identity, timers и ограничения

- Callback привязывается к Launch.Token через closure; отдельный token в `func(Result)` для этого не необходим. Rejected attempts получают новые tokens; terminal/stale reports не вправе повторно освободить reservation — §3.1, строки 296–299, §3.3, строки 349–352.
- Timeout начинается при авторизации, включая Starting, а не при попадании в Pending очередь — §5, строки 426–429. Для совместимости с этим правилом DeadlineExpired должен быть attempt-scoped: после rejection timer старой попытки не вправе отменить Pending или новую попытку. Тип Event допускает token; concrete applicability/retirement нужно явно закрепить в A0.
- Первый принятый Stop/timeout/HostClosing cause остаётся победителем; поздний timer не переименовывает operator cancellation. Подтверждающий callback может иметь natural outcome, сохранённый отдельно от winning state.
- Единый forest budget и подсчёт Starting/Running/Cancelling у всех ancestors совместимы с global/group caps. Batch nodes не потребляют slot. Остановка без подтверждения не является capacity release. Проблема zero-inheritance отдельно разобрана в находке 4.

### Fairness и Add / Stop / Settled

- Round-robin по eligible roots и ordinal-порядок eligible leaves допускают обход заблокированной группы, а не head-of-line ожидание первого pending leaf. Конкретные границы E4: blocked root A не удерживает eligible root B; в Parallel root с `[g-blocked, h-eligible]` второй leaf остаётся eligible. У Sequential такого обхода direct children нет — это ordering, а не дефект fairness. Потеря wake-up в находке 1 не покрывается одним round-robin правилом.
- Для Add перед последним Settled дерево расширяется; при обратном порядке Add отклоняется. Atomic validation всего subtree до mutation исключает частично принятые additions — §2.1, строки 238–241, §3.3, строки 340–341.
- §8, строка 611, дополнительно разрешает вопрос Add во время cancellation: Add в Cancelling batch запрещён. Add в ещё Running ancestor вне остановленного поддерева не обязан отменять sibling isolation.
- Cancelled alone не запускает StopOnFail; Failed/TimedOut/Interrupted запускают его только для pending successors. Repeated/late Stop не должен менять уже terminal победителя. Для skipped batch остаётся отдельная коллизия находки 3.
- Empty batch немедленно Completed с нулём leaves и больше не принимает Add — это явно заданная закрытая группа, не механизм ожидания будущих additions.

### Aggregation, notices, inspection

- Для обычного обработанного batch reducer Failed выше Cancelled выше Completed; explicit stop задаёт state batch, не стирая уже существующие descendant outcomes/counts. Leaf counts не должны включать сам nested batch. Недостаток reducer для policy-skip не означает, что нужен второй scheduler.
- Из §4, строк 377–381, получается определённая матрица обычных descendant notices: root NotifyAll подавляет все внутренние notices; root NotifyEach + child batch NotifyAll показывает child aggregate, но не его leaves; NotifyEach на всей цепочке допускает direct-child notices на каждом уровне. Root notice выбран один раз; Batch.Wait не является ещё одним выбранным notice.
- Singleton NotifyAll не требует leaf notice. Его consumer выбирает synchronous result либо root notice, а не обе полные доставки — строки 383–386. Это применимо к автономному consumer и не требует интеграции модели.
- Question/input transport отделён от completion policy и принадлежит executor/host — строки 399–401. Введение агентского question-state или отдельного question Event не требуется для сохранения slot у paused execution.
- Batch Output/Inject и capability-less execution возвращают unsupported error; terminal Inject запрещён, settled output сохраняется до Release, Release nonterminal запрещён. Immutable summary и root-ID tombstone предотвращают потерю результата и повторный запуск старого root — строки 388–394.
- Boundary ownership spec/result и immutable snapshots описаны в §2.1, строки 253–257. E9 не требует копировать весь forest на каждом событии.
- R3 и §1.3 согласованно отделяют bounded diagnostic journal с gap от authoritative Snapshot/Wait. Нельзя превращать slow observer в условие освобождения slot или разрешения Stop; crash-proof exactly-once из этого feed не следует.

## 3. Поздняя интеграция — не блокеры автономного компонента

Ни одна из пяти находок не требует начинать §9. В частности, lost wake-up воспроизводится с самостоятельным controlled executor и `CapacityAvailable`; это не только проблема сигналов существующего Rush ledger.

Отложенными остаются numeric defaults/сопоставление с session caps, DB representation, durable delivery/recovery, hooks/permissions ingress, legacy agent outcome contract и соединение cancellation синхронного caller со Stop. Их отсрочка прямо задана §8, строки 617–625, и §9, строки 631–700. Нельзя требовать SQLite-миграцию или outcome parser, чтобы устранить текущую неоднозначность states. Нельзя также обещать crash recovery на основании replay diagnostic trace.

## 4. Сильные стороны и пределы покрытия

**Сильные стороны:** один pure Engine для forest и shared admission; explicit events вместо core clocks; reservation до подтверждённого termination; authoritative Start rejection и per-attempt latch для synchronous callback; явная acceptance linearization; отдельные state/outcome и diagnostic/selected notice/Wait; самостоятельные controlled/process adapters и сценарии без модели. E1–E9/R1–R4 охватывают реальные поведения, а не форму исходников.

**Пределы:** проведено только статическое ревью полностью прочитанного плана. Реализация, Rush launch paths и существующие отчёты не аудитировались. Builds, tests, lint, formatters, smoke/replay и проверки git не запускались. Не заявляется, что предложенный компонент уже существует, что приведённые гонки воспроизведены в коде или что acceptance matrix пройдена. Единственное изменение этого задания — создание данного нового отчёта.

## 5. Обязательные изменения в порядке выполнения

1. **P1:** закрепить сохранение wake-up через in-flight capacity refusal: generation/latch, релевантные settlements, token и отсутствие spin; добавить перестановки из находки 1 к E5/R1.
2. **P1:** ограничить общий текст errors/panics по фазам и задать executor-owned setup cleanup; никогда не terminalize control error без подтверждения termination. Отразить это в E6/R1.
3. **P2:** задать lifecycle/aggregation policy-Skipped batch и проверить nested state/notice, а не только root counts, в E3/E8.
4. **P2:** записать admission predicate, точный смысл MaxParallel zero/one и порядок normalization/validation для Sequential и mixed groups; зафиксировать сценарии E2/E4.
5. **P2:** определить closing/draining lifetime и повторное ожидание после Close deadline; проверить late Start/completion при R1/R2/R3 без фиктивного Interrupted settlement.

После этих уточнений A0 может заморозить единый контракт для параллельных slices. Из данного ревью не следует разрешение реализовывать batches, запускать агентов или выполнять отложенную интеграцию.
