# Ревью job batches: автономная граница и API

**Проверяемый commit:** `51599fa4` (ветка `main`).  
**План:** `docs/plans/2026-10-07-job-batches-design.md`, строки 1–727; основной фокус — §§0–2, 4–5, 8.  
**Вердикт:** **needs correction / нужны исправления перед заморозкой A0**.

Граница поставки выбрана правильно: обычный Go-пакет, один Engine для нескольких корней, Runner как владелец исполнения, отдельный `batchlab` без интеграции с Rush. Однако текущие описания API ещё не образуют полностью замкнутый контракт между Core, Runtime и Laboratory. Три проблемы уровня P1 непосредственно затрагивают заявленные автономные возможности: доступ к выбранным уведомлениям, возврат ошибок управления в чистый Engine и наблюдаемый материал для replay. Остальные замечания — конкретные решения A0, необходимые до независимой реализации интерфейсов, а не предложение расширить функциональность.

Текст прочитан полностью диапазонами; отдельно прочитан blob плана из `51599fa4`. Из текущего кода прочитан только `go.mod:1–3`: один модуль `github.com/PHPCraftdream/rush`, Go `1.26.3`. По поиску путей `internal/jobbatch/` и `cmd/batchlab/` отсутствуют; ниже оценивается **предложенный компонент**, а не существующая реализация. Сценарии отказа помечены `[INFERENCE]`: это выводы из недоопределённых контрактов, не результаты исполнения. Builds, tests, race, lint, formatters и smoke runs не запускались.

## A. Исправления для автономной фазы

### 1. P1 — выбранные уведомления не имеют публичного пути из Runner к потребителю

**Основание:** §1.1:64–68; §2:179–195, 205–220; §4:371–386; §6.2:495–496.

`Transition` содержит `selected notices`, но доступ к переходам есть только у владельца Engine. Публичный Runner предоставляет `Events`, описанный как поток диагностических lifecycle records; среди объявленных полей `Record` нет списка выбранных notices или признака их выбора. Отдельного API уведомлений также нет. `Wait` прямо исключён из механизма пользовательских уведомлений. Поэтому политика выбора определена в Core, но её результат не объявлен на границе Runtime → host/Laboratory.

**Сценарий [INFERENCE]:** два одинаковых дерева с `NotifyAll` и `NotifyEach` дают одинаковые диагностические завершения узлов. Host, читающий только публичный `Events`, не получает объявленного признака, какие из них Engine выбрал для доставки. Laboratory либо повторно реализует правила подавления по предкам, либо игнорирует их и объявляет каждое завершение. Оба варианта расходятся с владением политикой в Engine; L3 не проверяет тот самый выбор, который вычислил Core.

**Минимальное исправление:** перед A1 определить metadata-only `Notice` и сделать выбранные notices доступными через существующий `EventPage`/`Record` либо отдельную неблокирующую cursor-операцию. Нового callback/observer-фреймворка не требуется. Если используется общий feed, диагностический record и selected notice должны оставаться разными понятиями; один переход может выбирать несколько notices. В notices достаточно identity/category/counts: неприватный lifecycle feed не должен автоматически получать `Result.Summary/Details` или вывод. Определить поведение при gap; это не превращает feed в durable/exactly-once доставку.

**Acceptance:** через один публичный Runner запустить корень `NotifyEach` с вложенным `NotifyAll`, затем аналогичный корень `NotifyAll`. Host без чтения Engine и без собственного reducer уведомлений видит объявленные selected child/root notices ровно в соответствии с §4; diagnostic завершения всех листьев при этом доступны. Медленный observer не задерживает Stop, а gap не выдаётся за полный список доставленных уведомлений.

### 2. P1 — результат Stop не может обновить engine-owned control error через объявленный Event

**Основание:** §1.1:59–68; §1.2:72–80; §2:201–210; §3.1:296–304; §3.3:354–357; §4:396–398.

`NodeView` должен содержать `control-error code`, а отказ `Execution.Stop` оставляет работу нетерминальной с видимой ошибкой. Но список `Event` содержит лишь запрос отмены и результаты запуска/завершения; результата управления в нём нет. Runner получает ошибку вне state loop, но объявленного пути применить её к представлению Engine нет. `StartRejected` не подходит для принятого исполнения, а `Settled` означал бы неподтверждённое завершение. Отдельная runtime-таблица, которой Runner подправляет `View`, создаёт второй источник истины для поля, включённого в core view.

Кроме того, формулировка «Executor errors/panics must settle accepted work once» (§3.3:356–357) слишком широкая: ошибка или panic в Stop/Inject не подтверждает прекращение исполнения и противоречит §3.1:301–304 при буквальном применении.

**Сценарий [INFERENCE]:** принятый процесс продолжает работать, `Stop` возвращает ошибку. Runtime либо не может показать её в `Engine.View`, либо синтетически отправляет `Settled(Failed)` и освобождает слот, пока процесс жив. Panic в Stop даёт тот же опасный выбор.

**Минимальное исправление:** добавить событие результата управления, например `StopReported(node, token, safeCode)`, и правила его применения: ошибка меняет диагностическое поле, но не terminal state и не admission; успешное подтверждение запроса тоже не равно termination. Определить поведение позднего результата и очистки/сохранения ранее показанного кода. В journal помещать стабильный безопасный код, не произвольный `error.Error()`. Сузить правило про errors/panics: control failure не заменяет completion; completion callback должен означать подтверждённое прекращение принятой работы. Допустимые terminal `Result.State` и обработку нарушения этого контракта также надо объявить, а не принимать произвольное значение общего `State` как завершение.

**Acceptance:** Stop принятого листа возвращает error/panic, лист остаётся `Cancelling`, ошибка видна через core/runtime view, другой корень не получает его слот. Успешный повторный Stop лишь подтверждает запрос; единственный последующий termination callback освобождает слот. Поздний control report не оживляет terminal лист и не меняет победившую причину отмены.

### 3. P1 — публичный диагностический feed недостаточно определён для privacy-safe replay и сравнения команд

**Основание:** §2:183–189, 201–220; §5:415–416, 438–448; §7.1:516–522.

План требует `input-reference ID` в record и journal, но ни Submit/Add, ни их ответы не объявляют способ передать или однозначно сопоставить такую ссылку с **принятым** событием. Особенно существенны несколько Add к одному узлу с разными opaque payloads. Кроме того, `replay` должен сравнивать command ordering, тогда как объявленный `Record` перечисляет identity/event/state/counts/reference, но не исходный упорядоченный список команд или его сравнимое представление. `Transition` имеет команды, однако Laboratory по контракту использует Runner, не владеет его Engine.

Наличие сценария само по себе не закрывает эти две границы: сценарий задаёт намерения управления, а journal должен сохранить фактически принятые события и эталон порядка команд. Добавление формата journal в A0 без producer/correlation-контракта Runner оставляет Laboratory зависимым от необъявленного runtime hook.

**Сценарий [INFERENCE]:** два Add с разными ссылками к одному batch конкурируют с Settled. Recorder видит accepted records, но не имеет объявленного способа выбрать соответствующий набор specs. В другом прогоне ошибочный dispatcher/Core поменял порядок двух Start-команд; replay вычисляет свой порядок, но не располагает записанным порядком оригинального перехода для сравнения. Простая сериализация всего `Transition` решила бы доступность данных ценой утечки accepted Spec, Result и changed views с outcome text, запрещённой §2/§5.

**Минимальное исправление:** заморозить не только JSON schema, но и путь её наполнения через реальный Runner:

- однозначную корреляцию accepted Submit/Add с external input references, доступную Laboratory; это может быть явная request metadata или строго определённое сопоставление по возвращаемой identity/sequence, но не предположение о порядке вызывающих goroutines;
- безопасную проекцию accepted Event со всеми scheduling-relevant полями: type, root/node/token, typed rejection/cancellation reason, references для specs; opaque Summary/Details для структурного replay не нужны;
- записанный ordered command descriptor (`kind`, node, token; без payload) либо эквивалентный сравнимый эталон;
- использование только этой проекции при записи, не JSON-кодирование полного `Transition`/`NodeView`.

Это уточнение журнала существующего lab, не Store, daemon или механизм recovery.

**Acceptance:** real Runner принимает два разных Add вокруг контролируемого завершения; journal однозначно восстанавливает именно accepted наборы и порядок. Перестановка двух записанных command descriptors делает replay ошибочным. Sentinels в argv, Payload, Inject, Summary и Details отсутствуют в журнале. Replay получает только scenario+journal и core Engine; даже сценарий с записывающим файл процессом не создаёт этот файл и не конструирует executor.

### 4. P2 — не определён источник TaskSpec для StartLeaf на границе Core → Runtime

**Основание:** §1.3:84–87; §2:107–112, 135–141, 173–176, 204–210, 253–257.

`Executor.Start` требует `Launch.Task` вместе с Payload. Для `StartLeaf` объявлены node и token, но не TaskSpec/Launch; `Engine.View` явно исключает Payload, а отдельного core lookup для него нет. Возможен корректный дизайн с Launch в команде, возможна отдельная immutable payload-table Runtime, но владелец этих данных и путь их выдачи сейчас не выбраны. Это не требование, чтобы Engine интерпретировал payload.

**Сценарий [INFERENCE]:** Core реализует указанную команду только как `(node, token)`, Runtime ожидает получить TaskSpec из view. После Submit, а тем более Add вложенного дерева, Runtime не имеет объявленного API получения данных для Start; приходится добавлять lookup либо заново поддерживать неоговорённое соответствие ID → Spec.

**Минимальное исправление:** объявить, что `StartLeaf` несёт полный immutable `Launch`, а `StopLeaf` — только identity/token; либо явно закрепить runtime-owned payload-table и её заполнение/удаление. Первый вариант не требует дополнительного lookup API или повторной интерпретации дерева. В любом варианте отделить полноту execution command от безопасной journal-проекции из замечания 3 и запретить executor мутировать переданные Engine-owned bytes.

**Acceptance:** после Add на глубине больше одного Runtime передаёт kind/group/timeout/payload ровно принятого листа правильному executor. Core по-прежнему не знает process/MCP/agent schema, а snapshot и journal payload не содержат.

### 5. P2 — контексты запросов и значения возврата ещё не задают момент принятия операции

**Основание:** §2:182–195, 205–207; §3.1:301–304; §3.3:359–367; §4:396–398; §6.2:493–494.

Правильное правило «request context не равен task lifetime» не определяет, что происходит с **ещё не принятым** запросом в очереди после истечения его context и как трактовать error, если изменение уже принято, но ответ ещё не получен. Для Add это существенно: операция меняет ordinal-space и возвращает новые IDs. Также не зафиксировано, что означает успешный `Runner.Stop`: принятие intent, завершение вызовов `Execution.Stop` или физическое termination. Требование показать executor errors вызывающему и формулировка L2 «Stop waits for the child to terminate» допускают разные реализации. Наконец, `Wait(Summary,error)` не разделяет отрицательный исход задачи и ошибку самого ожидания.

**Сценарий [INFERENCE]:** Add попал в очередь, caller получил `context.DeadlineExceeded`, а очередь затем применяет Add. Caller не знает, приняты ли новые узлы, и не получил их IDs. В другом случае `Runner.Stop` возвращает nil сразу после intent, хотя вызывающий по L2 считает процесс уже остановленным; поздняя ошибка executor Stop не может попасть в уже возвращённый error.

**Минимальное исправление:** выбрать и записать линейный контракт до A1. Для немедленных mutations Submit/Add достаточно проверять context до acceptance и фиксировать accepted reply атомарно с Apply: после acceptance caller получает committed результат, отмена request не откатывает работу. Если вместо этого выбирается indeterminate-on-error семантика, нужен объявленный способ отличить и восстановить accepted операцию, особенно Add. Для Stop отдельно указать, ждёт ли он control acknowledgment; nil никогда не должен сам по себе обещать termination без callback. Проверка физического остановa в L2 должна использовать определённое ожидание terminal state. Ошибки Stop, случившиеся после завершения caller wait, остаются доступны по правилу замечания 2. Для `Wait` минимальный контракт: task Failed/Cancelled/TimedOut/Interrupted возвращается в Summary; error относится к ожиданию/доступу, не подменяет outcome. Accepted control effects нельзя терять лишь из-за истечения request context.

**Acceptance:** barriers разделяют enqueue, acceptance и reply для Submit/Add; отмена до acceptance не меняет дерево, отмена после acceptance не даёт ошибочно выглядящий «непринятый Add». Отмена `Wait` не останавливает лист. Executor Stop подтверждает запрос до termination: поведение `Runner.Stop` соответствует объявленной точке возврата, а capacity удерживается до callback. `Wait` завершившегося Failed root возвращает доступный immutable Summary по объявленным error-правилам.

### 6. P2 — Release не определяет владение executor-output и гонки с уже выданными handles

**Основание:** §1.3:84–85; §2:140–150, 213–214; §4:388–398.

Вывод принадлежит executor; Runner должен удерживать execution handle до Release и затем освободить tree/output. Но `Execution` предоставляет только Stop, а правила post-terminal владения executor maps/buffers не объявлены. Удаление handle из Runner освобождает output только если executor/fixture уже не удерживает его отдельно. При этом Output/Inject вызываются вне loop, а Release разрешён после terminal независимо от того, есть ли уже начатый внешний вызов Output. Не выбрана и точка проверки terminal для Inject после выдачи handle.

**Сценарии [INFERENCE]:** (а) ControlledExecutor/ProcessExecutor хранит finished execution в собственной map; `Runner.Release` удаляет только свой handle, сохраняя буфер навсегда. (б) Output получил handle, затем completion и Release освободили/очистили buffer, а вне loop продолжается чтение. (в) Inject принят как разрешённый, но ещё до вызова Messenger исполнение завершилось; один executor применяет сообщение, другой возвращает terminal error, хотя обещание §4 читается одинаково.

**Минимальное исправление:** выбрать post-terminal ownership и disposal-протокол. Если хватает Go GC, прямо закрепить Runner как единственного владельца retained execution после completion и запретить executor держать finished entries; дополнительный `Close/Store` интерфейс тогда не нужен. Если есть retained внешние ресурсы, нужен конкретный disposal contract. Для уже выданных capability handles определить lease/pin: Release закрывает выдачу новых handles и освобождает своё владение, активное чтение безопасно завершается или получает объявленную ошибку; последняя lease освобождает остаток. Отдельно выбрать линейную семантику Inject vs Settled и правила concurrency для Output/Inject/Stop на одном Execution. Release не может обещать освобождение копий, которые уже принадлежат caller.

**Acceptance:** завершить лист с retained output, начать заблокированный Output, применить Release, затем разблокировать чтение — нет обращения к уничтоженным данным; последующие Output/View имеют объявленный released/not-found результат. После освобождения последнего handle executor-owned retained buffer недостижим/освобождён по выбранному контракту, immutable `Batch.Wait` Summary остаётся доступным. Гонка Inject/Settled даёт определённый результат, не зависит от скорости внешнего worker и не вводит сообщение в работу, уже признанную terminal до acceptance Inject.

### 7. P2 — правило immutable ownership не охватывает все публичные mutable данные

**Основание:** §2:124–129, 135–150, 182, 190–191, 253–257; §4:396–398.

Runner явно копирует specs/results, а snapshots не должны менять внутреннее состояние. Не зафиксирован режим владения `Limits.ByGroup`, Inject `json.RawMessage`, `Launch.Task.Payload` на стороне executor и `OutputChunk` bytes на выходе. Эти значения пересекают те же асинхронные границы, но не являются spec/result request во всех случаях. «Копировать один раз» без списка ownership edges не решает, кто после передачи вправе менять slice/map.

**Сценарий [INFERENCE]:** caller переиспользует Inject buffer после возврата, пока Messenger ещё держит сообщение в своей очереди; executor мутирует Launch payload, который Engine сохраняет как immutable input; caller меняет `ByGroup` после создания Runner или меняет возвращённые output bytes, совпадающие с retained ring-buffer. Соответственно могут измениться принятые данные, budget или последующее чтение.

**Минимальное исправление:** дать короткую ownership-таблицу: constructor Limits — detached copy либо явная immutable transfer; Inject — устойчивый принятый input с определённым моментом передачи; Launch — read-only immutable data; callback Result — уже предусмотренная копия; OutputReader — detached caller-owned chunk либо явно оговорённое безопасное read-only владение. Не клонировать Launch/forest повторно просто ради defensive programming: если executor соблюдает readonly контракт, повторная копия payload не нужна. Для retained outputs разрешить возвращать уже detached bytes, чтобы Runner не делал вторую копию.

**Acceptance:** мутация исходных spec/result/limits/message containers в разрешённый контрактом момент не меняет принятые данные; executor соблюдает readonly Launch; предыдущий OutputChunk остаётся стабильным после следующей записи, чтения и truncation. Проверка ownership затрагивает не только Snapshot, но каждый перечисленный mutable boundary.

### 8. P2 — BatchView не определяет обнаружение узлов и восстановление дерева после observer gap

**Основание:** §0:37–44; §2:175–176, 183–188, 208–220, 228–236; §4:388–391.

Для `BatchView` и `Summary` перечислены одинаковые root state/counts. У `NodeView` есть parent/ordinal, но нет children; Submit возвращает Batch с Done/Wait, а состав и порядок возвращаемых Add IDs не определены. В описанном минимуме API нет способа перечислить узлы по известному root ID. Deterministic ID paths не заменяют список принятых узлов; потребитель не должен восстанавливать ownership парсингом IDs. Возможность положить nodes в будущий BatchView существует, но пока это не часть объявленного контракта.

**Сценарий [INFERENCE]:** поздний observer знает root ID, пропустил Add и получил feed gap. Snapshot возвращает aggregate counts, но не IDs/parent/state вложенных листьев; такой observer не может адресовать Output/Stop/Inject нужного принятого элемента без собственной копии всей истории сценария.

**Минимальное исправление:** отделить DTO: `Summary` — маленький immutable terminal aggregate для Wait, `BatchView` — согласованное snapshot дерева с упорядоченными NodeViews/явными отношениями. Указать, принимает Snapshot только root или любой batch node; определить Add IDs как direct children в порядке input (либо другой конкретный порядок). Это делает отдельный Snapshot оправданным и не требует ещё одного List/registry API.

**Acceptance:** consumer имеет только root ID и неполный cursor feed; после нескольких Add во вложенные batches один Snapshot даёт все принятые node IDs и parent/ordinal/state. Consumer выбирает глубокий лист для Output/Stop/Inject без собственной reconstruction логики и без разбора ID для ownership. Полученный snapshot нельзя использовать для мутации Engine.

### 9. P2 — live output cursor не имеет объявленных правил EOF и восстановления после truncation

**Основание:** §2:146–148, 190, 213–214; §4:388–391, 397–400; §5:421–422; §6.2:489–494.

Перечень полей OutputChunk полезен, но не задаёт semantics `ReadOutput`: что происходит на текущем конце ещё живого stream, когда именно EOF становится true, каким cursor восстановиться после gap и как отличаются ещё не созданный execution, unsupported capability и released execution. Для process stdout/stderr также нужно выбрать один общий byte stream или явно различимые streams; требование удерживать оба не определяет формат их чтения.

**Сценарий [INFERENCE]:** process выводит `A`, некоторое время ничего не пишет, затем выводит `B`. Adapter считает достижение текущего buffer-end EOF, и caller никогда не читает `B`; другой adapter бесконечно возвращает empty chunk с неизменным cursor. При ring truncation caller получает gap, но не знает, с какой позиции продолжить, сохраняя точно определённый byte cursor.

**Минимальное исправление:** в A0 определить cursor-space и stream model; EOF означает окончательно закрытый output после termination, а не временную пустоту. Выбрать blocking/context-bounded чтение на live end либо явно описанный nonblocking результат; предоставить однозначный oldest-available/recovery cursor и next cursor после truncation. Объявить typed not-ready/unsupported/released semantics. Это контракт существующего OutputReader, не новый транспорт или output backend.

**Acceptance:** controlled/process output `A` → пауза → `B` → completion читается cursor-операциями без ложного EOF, без пропуска `B` и без busy loop, навязанного неясной семантикой. Малый retained-output cap обрезает начало: reader узнаёт факт gap и точку восстановления, затем получает оставшиеся bytes и настоящий EOF. Terminal retained output доступен до Release, pending execution и unsupported capability различимы по объявленным ошибкам.

## B. Решения поздней интеграции — не блокеры автономной поставки

Ни одно замечание выше не требует переносить Rush internals в `internal/jobbatch`.

- §0:29–33 и §8:617–625 правильно исключают DB, tools, model prompts, session ownership и численные Rush defaults из первой фазы. Авторизованное ownership оператора — задача host/adapter; в core достаточно явных root/parent/node identities.
- §9.2:659–678 оставляет adapter ответственным за sync/detached delivery, связь caller cancellation со Stop, реальное завершение agent descendants и MCP cancellation без уничтожения server session. Callback `func(Result)` подходит для асинхронного adapter, если выполняются отдельные условия acceptance и подтверждённого termination; не нужно вводить агентские вопросы в scheduler.
- §8:619–624 и §9.3:685–700 правильно откладывают external admission accounting, durable claims, persistence/recovery и legacy-agent outcome migration. Метаданные notices/journal в автономной фазе не обещают durable exactly-once delivery.
- Выбор UI/CLI controls для Add/вложенности и транспорт вопросов остаются за integration host. Однако уже автономный public API обязан отдавать собственные notices, дерево и безопасные accepted-event данные: это не детали будущей Rush интеграции.

## Сильные стороны и лишние абстракции

- §§0–1:16–33, 59–92 создают достаточно строгую package boundary. Pure Engine без clocks/goroutines/I/O и отдельный serializing Runner — достаточное разделение; lab зависит от компонента, не наоборот. Shared forest budget обоснован несколькими root batches, не является вторым scheduler.
- `Start(ctx, Launch, report) → Execution` позволяет принять работу и закончить её позже, в том числе сообщить completion до возврата Start. Tokens и latched cancellation сохраняют эту модель пригодной для будущих process/MCP/agent adapters. Optional OutputReader/Messenger лучше обязательных no-op capabilities.
- §2.1:234–251 фиксирует single-task ingress без повторного wrapping листьев и machine-independent limits. Kind/payload validation у consumer не требует agent/MCP ветвей в Engine.
- §§2, 5:210–220, 438–448 правильно отделяют explicit inspection от diagnostic logging и pure replay от повторного исполнения. Необходимо закрыть producer/schema контракты, но исходный privacy и no-side-effects принцип верен.
- Запрет обязательного Store (§2:224), daemon/RPC/второго CLI (§5:450) и pre-refactoring (§0:29–33) стоит сохранить. Не нужны plugin registry, observer callback hierarchy, retry framework или универсальный persistence layer для устранения перечисленных проблем. Различие Snapshot/Wait полезно только при определённой роли subtree snapshot и маленького immutable Summary.
- `MaxNodes` — per-root, вывод ограничен executor cap, records имеют bounded retention. План **не обещает глобально ограниченную память Runner**: unreleased roots и lifetime root-ID tombstones растут с числом submissions (§2:124–128; §4:391–394). Это явное ограничение выбранной модели, не повод добавлять незаказанную eviction policy; Release должен освобождать именно своё retained владение, сохраняя намеренные tombstones.

## Границы покрытия

Проверены весь текст плана и замыкание публичных переходов данных: Submit/Add → Engine → Launch, completion/control result → Engine, notices/records → host, Output/Inject/Release и scenario/journal → pure replay. Не проверялись фактические реализации runtime/executors, platform process isolation, Rush ledger/admission/auth hooks и состояние durable delivery. Сcheduling fairness, reducer приоритетов states и подробный shutdown lifecycle не являются предметом данного API-отчёта. Acceptance-сценарии выше предложены для будущей проверки, а не представлены как пройденные.

## Обязательные изменения в порядке выполнения

1. Замкнуть execution protocol в shared declarations: источник полного Launch, событие результата Stop, readonly ownership, допустимые completion states и запрет synthetic termination из control failure (замечания 2, 4).
2. Сделать selected notices публично наблюдаемыми без повторной реализации политики в host (1).
3. Заморозить privacy-safe accepted-event/command projection и корреляцию scenario references через настоящий Runner; затем schema replay/journal (3).
4. Зафиксировать acceptance/return/error правила Submit/Add/Stop/Wait и жизненный цикл capability handles/Release (5–6).
5. Дополнить ownership всех mutable boundary values, tree Snapshot/Add ID semantics и live output cursor contract (7–9).
6. После этих решений отдавать A1 исполнителям один согласованный контракт. Проверку перечисленных acceptance-сценариев и автономных E/R/L gates выполняет integration owner; исправления не должны зависеть от начала §9.
