# Job batches: ревью A0/A1, границ параллельной работы и исполнимой приёмки

**Проверяемый commit:** `51599fa4` (указан в задании), ветка `main`.
**План:** `docs/plans/2026-10-07-job-batches-design.md`, прочитан полностью, строки 1–727.
**Фокус:** §2, §5–7, матрица E/R/L; замораживание контрактов, четыре независимых среза, совместная компиляция, deterministic scenarios/replay, consumer-visible proofs и cleanup.
**Вердикт: needs correction — нужны уточнения до запуска A1.**

План корректно отделяет самостоятельный ordinary-Go компонент от будущей интеграции в Rush. Четыре среза пригодны для параллельного написания, но A0 пока допускает несовместимые трактовки фабричных деклараций и управляемых сценариев. A2 сформулирован как реальная совместная проверка, однако часть назначенных acceptance IDs не принадлежит указанному слою, а replay и smoke могут доказать только согласованность реализации с самой собой. Это ревью предлагаемого компонента, не утверждение о наличии реализованного `jobbatch`.

## 1. Блокеры и обязательные уточнения самостоятельной фазы

### 1. P1 — фабричные «declarations в schema.go» нельзя понимать как Go-прототипы с последующими Go-реализациями

**Основание:** §2, строки 154–168; строки 222–223; §7.1, строки 516–522; §7.2, строки 550–559.

A0 обязан определить lab factory declarations в `lab/schema.go` и предоставить Laboratory callable process factory contract. Одновременно тела фабрик принадлежат A1, concrete `ControlledExecutor` — Laboratory, а пустые concrete structs и тела-placeholder запрещены. Здесь требуется явное различие между декларацией контракта в документе и объявлением функции в `.go`.

В Go объявление функции без тела предназначено для реализации **вне Go**, например в assembly; это не C-подобный forward declaration. Повторное объявление `NewProcessExecutor` в `schema.go` и `process.go` имеет одно имя в package block. Соответствующие правила: [Function declarations](https://go.dev/ref/spec#Function_declarations), [Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope). Аналогично нельзя положить в A0 фиктивный `ControlledExecutor`, а затем определить настоящий тип в `controlled.go`.

**Конкретный конфликт:** владелец A0 буквально записывает `func NewProcessExecutor(ProcessOptions) (jobbatch.Executor, error)` без тела в `schema.go`; Process добавляет функцию с телом в своём `process*.go`. На совместном gate остаются два объявления. Если вместо этого A0 пишет временное тело или пустой `ControlledExecutor`, он нарушает запрет placeholders и вынуждает A1 менять чужой shared file. Это статический вывод о таком варианте выполнения плана, не результат запущенной сборки.

**Минимальная коррекция:** A0 владеет реальными data declarations — payloads, `ProcessOptions`, enums/errors, scenario/journal structs. Сигнатуры функций и методов замораживаются как документированный контракт; единственное Go-объявление с настоящим телом создаёт owning slice: process factory — Process, controlled factory/type/methods — Laboratory, `Engine` — Core, `Runner`/`Batch` — Runtime. В §7.1 явно написать, что контракт callable **после совместного handoff**, а не реализован или обязательно компилируем отдельно в A0. Не вводить function variables, заглушки или дополнительные интерфейсы только ради ранней компиляции.

**Приёмочный сценарий:** владелец A2 получает все четыре среза, совместно компилирует `internal/jobbatch`, `internal/jobbatch/lab` и `cmd/batchlab` и использует обе настоящие фабрики. Для успешного gate не требуется удалить/заменить временный public API из A0; в shared schema нет одноимённых prototypes или concrete executor placeholders. До этого gate отсутствие тел в ещё не переданных срезах не считается дефектом и не разрешает агентам писать их за соседа.

### 2. P1 — контракт controlled gates и сценарных действий ещё не связывает действия с наблюдаемой попыткой запуска

**Основание:** §2, строки 135–142, 158–166; §3.1, строки 286–299; §3.3, строки 340–356; §5, строки 415–420; §6, строки 469–477, 483–490, 495–503; §7.1, строки 518–523.

A0 обещает freeze gate semantics, но опубликованные операции fixture — только `Complete(ctx, ID, Result)` и `ReleaseStart(ctx, ID)`. В описании сценария перечислены Add/Stop/Inject; способ управляемо вызвать completion, release start, capacity refusal/resume и дождаться конкретной границы не определён. Идентичность engine attempt включает `Token`, тогда как контроль fixture адресует только node ID.

**Конкретное interleaving:** Runner уже показывает `Starting`, но effect goroutine ещё не вошла в `ControlledExecutor.Start`. Скрипт немедленно вызывает `ReleaseStart(root.1)` или `Complete(root.1)`. В зависимости от недоопределённого поведения операция либо теряется/ошибается, либо latch относится к будущему запуску. После capacity refusal тот же ID имеет следующую попытку; запоздалое действие по ID способно освободить не ту gate. Sleep вместо rendezvous не докажет R1/E5, а добавление Laboratory своей очереди допусков нарушит запрет второго scheduler. [INFERENCE] Без уточнения разные агенты могут выбрать все эти несовместимые варианты; текущих реализаций для проверки нет.

**Минимальная коррекция:** до fan-out заморозить небольшой полный vocabulary сценария: ожидание регистрации start attempt/нужного состояния, release start, explicit completion/refusal, `CapacityAvailable`, Add/Stop/Inject и необходимые explicit output assertions. Определить, что ожидание имеет bound, какой факт оно наблюдает и как сообщает ошибку. Для controlled API выбрать один конкретный способ привязки к попытке: например, await возвращает реальный `Launch`, а Complete/ReleaseStart принимают его token; либо использовать заранее определённые gate keys, однозначно связанные с attempt. Зафиксировать поведение действия до регистрации, повторного/устаревшего действия и Stop до возврата Start. Само тело fixture остаётся Laboratory. Нельзя оставлять агенту выбор между «ошибка», «автоматически сохранить на следующий запуск» и «неявно подождать».

**Приёмочный сценарий:** без sleep выполнить: первая попытка capacity-refused; после явного разрешения начинается вторая с другим token; поздняя release/completion первой попытки не затрагивает вторую. Отдельно дождаться фактического входа Start, принять Stop до его возврата и лишь затем release gate: нет side effects после отменённого допуска, accepted execution останавливается, slot освобождается только по подтверждению. Две заданные последовательности Add/last-settle должны давать две воспроизводимые, разные ветви результата.

### 3. P1 — replay не имеет явной границы полноты trace и привязки к содержимому scenario

**Основание:** §1.3, строки 89–92; §2, строки 205–220; §5, строки 438–448; L4, строки 497–498; §7.1, строки 518–523; §7.3, строки 589–594.

`Runner.Events` — bounded diagnostic feed, который допускает gap и восстановление текущего состояния через Snapshot/Wait, но не восстановление потерянного журнала. `--events` нужен для replay, которому пропуски запрещены. Последовательные номера обнаруживают внутренний gap, но сами по себе не обнаруживают потерянный хвост. Также «mismatched references» не определено: одинаковое имя reference не означает одинаковый scenario/payload.

**Конкретные отказы:**

1. Wait возвращает финальный результат, а journal consumer ещё не записал последний Settled. `run` отменяет consumer и закрывает файл. Все записанные sequence подряд, но хвост отсутствует. Если replay сравнивает только то, что есть в trace, он не узнаёт, что исходный run уже был терминальным.
2. Медленный файл/observer теряет records из bounded retention. Восстановление snapshot не даёт потерянных команд и interleavings; нельзя вставить snapshot и считать журнал непрерывным.
3. В `scenario.json` меняют spec/limits или содержимое payload под прежним reference ID. Проверка существования ID проходит, хотя реконструируется другой ввод. Менять сырые payloads на логирование ради проверки также нельзя.

Это пробел формата и правил завершения, а не запрос durable storage/exactly-once delivery.

**Минимальная коррекция:** A0 определяет versioned header с однозначной привязкой trace к scenario и используемым references — например, canonical content digest с явно заданными правилами канонизации. Определить, что именно считается mismatch. Определить финальную границу: footer с авторитетной конечной engine sequence и ожидаемыми структурными root summaries, а не только последний увиденный observer record. Laboratory завершает запись через эту границу; gap/ошибка записи/отсутствующий обязательный footer не выдаются за пригодный полный replay. Для `run --events` такие потери должны явно влиять на итоговый status, не блокируя event loop или cancellation. Отдельно отличать **полный trace незавершённой execution** от **неполного trace**: timed-out cleanup не разрешает ложно записать terminal summary. Public transport accepted-input references и command comparison также должен быть заморожен владельцем A0; детали этого API относятся к отдельному API-ревью.

**Приёмочные сценарии:** успешный run сохраняет trace до заявленного финального sequence; удаление последней записи, удаление footer, внутренний gap и incompatible version отвергаются. Изменение spec/limits/содержимого reference при сохранённом ID распознаётся как mismatch. При переполнении diagnostic retention журнал не «восстанавливается» как будто полный. Replay валидного failed/cancelled trace воспроизводит состояние и counts без запуска фабрик или process; file-writing sentinel из L4 остаётся нетронутым.

## 2. Пробелы распределения приёмки и A2 executable proofs

### 4. P2 — acceptance ownership не совпадает с consumer-visible границами E1, E9 и L1/L2

**Основание:** §2.1, строки 234–236, 253–257; §4, строки 383–398; E1/E8/E9, строки 460–461, 476–479; L1/L2, строки 491–494; §7.2, строки 550–553; §7.3, строки 583–603.

Core получает «E1–E9», но root Task не является допустимым input engine: singleton normalization принадлежит consumer. Pure Core также не может один доказать runtime diagnostic filtering, retained output handles и release поведения consumer. Process получает L1/L2, хотя failed aggregate и завершение через Runner/CLI принадлежат не одному adapter. Laboratory получает только L3/L4, хотя владеет реальным ingress и executable для L1/L2.

**Конкретная ложноположительная приёмка:** тест Core вручную создаёт one-task Batch в обеих ветвях E1, сравнивает одинаковые Spec и проходит. Настоящий `batchlab run` тем временем отклоняет standalone task или оборачивает каждый nested leaf дополнительным Batch. Тест Process проверяет только exit `Result.State`; неверный CLI aggregate/exit или преждевременное освобождение Runner slot остаются незамеченными. Общая обязанность A2 удовлетворить матрицу не определяет, кто обязан поставить эти cross-slice tests и runnable fixtures.

**Минимальная коррекция:** разнести для IDs producer-level и consumer-level обязательства. Core сохраняет pure-event проверки; Runtime явно получает public-API части E8/E9; Laboratory получает E1 через настоящий ingress, все L1–L4 runnable scenarios и их CLI assertions; Process предоставляет process-level проверки и fixtures для L1/L2, но не заявляет полное доказательство aggregate/CLI. Владелец A2 owns окончательную карту «ID → fixture/test → наблюдаемая граница → ожидаемый результат». При необходимости заранее зарезервировать конкретный cross-slice acceptance file за владельцем, а не поручать двум агентам один набор тестов.

**Приёмочные сценарии:** подать raw standalone Task и явный one-task Batch через поддержанный Laboratory ingress с одинаковой выбранной политикой; проверить actual Runner tree, один leaf launch и один consumer result. Подать nested tree и убедиться, что в snapshot не появились лишние wrapper nodes. Для E9 прочитать terminal output до Release, выполнить Release, сохранить доступность immutable Wait summary и запретить повторный root ID. L1 проходит через настоящий CLI с тремя настоящими процессами, а не только через вызовы executor напрямую.

### 5. P2 — named smoke IDs и run→replay comparison не заменяют независимый oracle, особенно для multi-root и ожидаемых ошибок

**Основание:** §3.2, строки 324–332; §5, строки 429–448; E4/E5, строки 467–471; L1/L3/L4, строки 491–498; §7.1, строки 523–524; §7.3, строки 589–603.

A0 должен назвать smoke scenarios и IDs, но нет обязательного формата ожидаемых результатов и schedules. Run и replay используют один Engine: неправильный scheduler может одинаково ошибиться в обоих режимах. L3 «exercises» несколько операций, но без заранее заданного trace/oracle можно выполнить roots по очереди и ни разу не доказать общий budget или пробуждение ещё ни разу не стартовавшего root. L1 содержит ожидаемый nonzero process exit; по §5 настоящий CLI также обязан закончиться nonzero. Поэтому ни «команда должна завершиться 0», ни «любое nonzero означает ожидаемую Failed» не являются корректной проверкой.

**Конкретный пропуск:** реализация создаёт отдельный Runner для каждого root либо рассматривает waiting leaves только в tree завершившегося task. Отдельные Core tests и формально multi-root scenario без одновременно pending roots проходят. При expected-failure smoke ошибка разбора JSON или отсутствие helper program может быть ошибочно принята за требуемый Failed aggregate.

**Минимальная коррекция:** A0 фиксирует для именованных scenarios независимые expectations: структурные IDs/parent relationships, terminal leaf counts, root states, разрешённые/запрещённые start boundaries, selected notices и ожидаемый успешный/nonzero CLI outcome. Для controlled schedules — точный порядок или явно заданные rendezvous/partial order; для реальных процессов — не требовать детерминированного порядка OS completion, проверять устойчивые invariants. В каждом multi-root scenario один Runner/Engine на весь scenario, roots подаются до ожидания полного завершения предыдущего. A2 сверяет oracle, а не только равенство run/replay; expected failure принимается лишь при нужных individual outcomes и aggregate, не по одному exit code.

**Минимальные доказательные сценарии:**

- `MaxConcurrent=1`: A.1 accepted и удерживается, B.1 pending без единого предыдущего старта. Подтвердить завершение A.1; B.1 должен стартовать без Add/CapacityAvailable к B и без создания второго Runner. Счётчик реально accepted executions никогда не превышает один.
- Отдельный сценарий с несколькими groups: capacity-refused root/group не блокирует eligible другую group; после relevant settlement/CapacityAvailable отказанная попытка запускается ровно один раз с новым token. Проверять start counts и запрет busy-loop, а не только final Completed.
- L3 продолжает проверять nested Stop, failure policy и оба notify modes по заранее известным assertions, не смешивая diagnostic record с user notice.
- L1 ожидает nonzero CLI, правильные состояния всех трёх leaves и Failed aggregate; panic, malformed scenario или missing program вместо требуемых трёх запусков не засчитываются. L4 сравнивается также с независимым structural oracle.

### 6. P2 — матрица не доказывает cleanup при отмене самой CLI invocation и исчерпании scenario wait bound

**Основание:** §3.1, строки 301–304; §3.3, строки 359–367; §5, строки 429–436; R2, строки 485–486; L2, строки 493–494; §7.3, строки 589–590.

Обязательство bounded `Runner.Close` для cancellation invocation и scenario bound есть в §5, но L2 проверяет explicit Stop, R2 — различие HostClosing/Wait cancellation. Нет явного executable test двух путей завершения самой `batchlab` и negative case, где termination ещё не подтверждена. Формулировка L2 «Stop waits for the child» также должна назвать наблюдаемый факт termination, а не приравнивать nil из control operation к физическому завершению.

**Конкретный отказ:** command исполняется, invocation context уже cancelled, cleanup вызывает `Close` с ним же вместо отдельного bounded cleanup context. Вызов сразу возвращает, command/tree ещё живы. Обычный тест `Runner.Stop` и normal CLI smoke этого пути не проходят. В другом случае scenario bound истёк, а CLI печатает completed только потому, что control script закончился. Такие consumer-visible ошибки прямо запрещены строками 433–436, но текущие именованные проверки их не выделяют.

**Минимальная коррекция:** добавить обязательные подслучаи L2/R2 и A2 executable gate для invocation cancellation, overall wait-bound expiry и timeout Close без termination confirmation. До fan-out определить источник/способ cancellation для поддержанных платформ и независимый bounded cleanup context. Проверять цепочку «accepted cancellation → Close/termination request → authoritative confirmation либо явно unfinished outcome», не только успешный возврат Stop. Не объявлять неизбежно nonterminal controlled execution завершённой ради выхода fixture.

**Приёмочные сценарии:** настоящий child, его descendant и отдельно unrelated sentinel подтверждают готовность через pipe/file handshake, не sleep. Отменить CLI invocation; затем отдельно исчерпать scenario bound. В обоих случаях CLI возвращает nonzero, собственные child/descendant завершены перед успешным cleanup result, sentinel жив, partial output доступен при explicit inspection. Controlled execution, удерживающая termination confirmation дольше bounded Close, даёт error/unfinished, не terminal success; пока confirmation отсутствует, admission не освобождается. Проверка процесса после `Stop` использует termination/Wait, а не nil как доказательство.

## 3. Практические границы срезов и текущие репозиторные зависимости

### 7. P2 — выбор process helper в A0 нельзя заменить общей ссылкой на «existing platform isolation helpers»

**Основание плана:** §0, строки 22–31; §5, строки 421–424; §7.1, строки 523–527; §7.2, строка 552.

**Проверенное текущее code evidence:**

- `internal/platform/command.go:8–21,40–43`: `platform.Command` — санкционированный spawn wrapper; сам предоставляет console-window hardening, не per-task tree isolation.
- `internal/platform/spawn_guard_test.go:17–28,30–46,86–108`: existing guard запрещает прямые `exec.Command`/`exec.CommandContext` и `exec.Cmd` construction под `internal/`, кроме конкретных exemptions. Новая `lab/process*.go` в exemptions не названа.
- `internal/platform/job_windows.go:14–18,45–55`: `AssignToNewJobObject` присоединяет **текущий host process**, а не выбранный leaf child, и держит handle до конца host lifetime.
- `internal/session/track_windows.go:31–57,79–110,114–127`: per-child tree tracking есть в `session`; `internal/session/kill_windows.go:14–29,45–46,59–88` описывает его связь с kill и не обещает подтверждённого reap по nil.
- `internal/session/kill_unix.go:20–32,37–58`: безопасный group kill зависит от leader identity и предшествующего Setpgid; nil также не является подтверждением reap.
- `internal/session/session.go:3–15`: импорт package `session` включает DB/message/pubsub dependencies; нельзя импортировать только его process-helper file.

**Конкретная неоднозначность задания Process:** трактовать `platform.Command` как полноценный tree isolation недостаточно; host-level Job Object нельзя использовать как Stop конкретного leaf, не затронув соседей. Импортировать существующий `session.KillProcess` как будто это изолированный platform-only package также не соответствует заявленной независимой границе. [INFERENCE] Если A0 оставит выбор агенту, один вариант приведёт к orphan descendants, другой — к unwanted coupling, третий потребует правок shared helper files вне Process ownership. Это не утверждение, что будущая реализация уже делает какую-либо из этих ошибок.

**Минимальная коррекция:** сделать результат «establish helper behavior» конкретным A0 artifact/input: approved spawn helper `platform.Command`, точная per-child isolation/termination strategy на поддержанных OS, permitted imports, identity/confirmation obligations и owning files. Если подходящего узкого tree helper нет, Process реализует соответствующие platform-specific детали в своих `lab/process*.go`; это не требует предварительной интеграции с `session` или общего project refactor. Не позволять Process менять existing guards/shared helpers за пределами разрешённого ownership без owner decision. Заморозить также единую трактовку output cap: что задаёт `ProcessOptions`, что может задавать payload и какое ограничение побеждает.

**Приёмочный сценарий:** через настоящий Runner одновременно запущены два независимых процесса, один из них имеет descendant. Stop первого прекращает только его tree, второй продолжает и отдаёт output. Missing program и nonzero exit различаются по предусмотренному результату, retained output ограничен выбранным cap. A2 выполняет existing platform guard в рамках repository checks и реальную поддержанную process-termination проверку; отсутствие выполнения другой платформы указывается как limit, а не как успех. Сценарий L2 не доказывается одним host Job Object или nil из kill helper.

### 8. P3 — «helpers в своих test files» не разделяет package-level namespace

**Основание:** §7.2, строки 548–553, 561–564; A2, строки 585–586.

Core и Runtime создают файлы в одном Go package; Process и Laboratory — в другом общем package `lab`. Отдельные файлы и testdata directories предотвращают concurrent edits, но не duplicate declarations. Правило «package-local helpers в своих test files» не задаёт уникальных имён этих helpers.

**Конкретный compile conflict:** два агента независимо объявляют `testExecutor`/`expectStates`/`newTestRunner` в собственных `_test.go` одного package. Ни один чужой файл не изменён, но joint package не компилируется. [INFERENCE] Это возможное столкновение, не найденная существующая duplicate declaration; оно не оправдывает сериализацию A1 и ловится A2.

**Минимальная коррекция:** в A0 ownership manifest добавить conventions для package-level test helper symbols и точные fixture subdirectories: например, `coreTest*`, `runtimeTest*`, `processTest*`, `scenarioTest*`. Не создавать shared mutable helper file; public declarations по-прежнему принадлежат A0/owning slice. Случайное столкновение устраняет A2 owner, без просьб обоим агентам одновременно редактировать общий helper.

**Приёмочный сценарий:** совместная test-package compilation включает все четыре набора тестов без duplicate symbols и без выбрасывания одного набора ради gate. Фикстуры каждого среза находятся в разных, заранее указанных subdirectories; cross-slice acceptance files имеют одного named owner.

## 4. Реальные зависимости и корректная граница A2

Следующая зависимость не требует последовательного **написания**, но требует совместной **компиляции/исполнения**:

| Срез | Что ему реально нужно | Что не должен временно реализовывать |
|---|---|---|
| Core | A0 data/events/errors и нормативный scheduling contract | Runner, process routing, CLI normalization |
| Runtime | Замороженные Engine signatures и точный смысл Transition/attempt; настоящий Core нужен на executable gate | Временный Engine или второй scheduler |
| Process | A0 payload/options, Executor/OutputReader, утверждённый platform contract | Runner, scenario parser, session services |
| Laboratory | Runner signatures, настоящий Process factory на joint gate, controlled gates и journal schema | Заглушки Runner/Process, отдельный admission/scheduling engine |

§7.2:555–559 правильно разрешает писать against API без ранней успешной сборки. Доступ Laboratory к Process factory не является зависимостью от завершения Process **до fan-out**; зависимость появляется на A2. Иначе четыре среза ошибочно превратятся в последовательный pipeline.

Владелец интеграции должен до A1 разрешить пункты 1–3 и конкретизировать input из пункта 7; сохранить одного владельца shared contracts, docs/changelog и cross-slice reconciliation. На A2 он один выполняет совместный compile, focused behavioral tests, доступный `-race`, все реальные CLI smoke и repository checks. Проверки не заменяются agent report или static source assertions. Для каждого acceptance ID сохраняются actual command/result и предел доказательства; platform prerequisites указываются явно. Любая corrective правка перепроверяется на затронутом consumer path, а не только локальным compile среза.

## 5. Что относится только к будущей интеграции Rush

Ни один finding выше не требует tool registrations, DB migration, новых prompts, извлечения ledger methods или запуска настоящих MCP/agent workloads. §0:29–33, §8:617–628 и §9:631–727 правильно откладывают эти работы.

- E1 сейчас должен доказать abstract singleton ingress и отсутствие двойной wrapping в Laboratory, а не claim о текущих Rush launch paths.
- Численные default caps Rush, внешняя ledger admission/accounting, durable notification/recovery и legacy-agent outcome остаются последующими решениями. Они не должны использоваться, чтобы заблокировать standalone tests.
- Поддержка обычного child process и safe cleanup сейчас обязательна; остановка MCP без разрушения shared connection, authenticated cross-process control и WebUI — отдельная последующая приёмка.
- Replay diagnostic trace не является crash recovery или доказательством exactly-once Rush delivery.

## 6. Сильные стороны

- Есть чёткая самостоятельная delivery boundary: engine/runtime на standard library, отдельный consumer `batchlab`, никакого второго scheduler и mandatory Store.
- A0 как inline prerequisite, single ownership shared files и запрет placeholders — правильная база; основной конфликт устраняется уточнением трактовки declarations, а не перестройкой архитектуры.
- Отличены ordering event loop, accepted execution, completion и подтверждённая cancellation; launch token и shared forest budgets дают конкретные проверяемые invariants.
- Матрица требует multi-root, capacity refusal, immediate callback, stale reports, output/injection и side-effect-free replay; controlled executor и synctest/barriers подходят для детерминированной проверки.
- A1 не запускает checks на half-finished общей кодовой базе; A2 не считает compiling skeleton или mock-only command завершённой работой и требует настоящий executable proof.
- Privacy boundary для opaque payload/result и явные diagnostic gaps полезны, если journal completeness не будет притворяться stronger guarantee.

## 7. Пределы покрытия

План прочитан полностью в диапазонах. Дополнительно статически просмотрены `go.mod:1–65`, `AGENTS.md:1–180` и перечисленные process/platform/session участки. `go.mod:1–3` задаёт module `github.com/PHPCraftdream/rush` и Go `1.26.3`; следовательно, предложение synctest не требует само по себе library/toolchain bump в плане. Фактическая доступность toolchain, `-race`, shell/helper programs и process isolation на конкретной execution platform не проверялась.

Сборки, тесты, lint, formatters и smoke не выполнялись по ограничениям задания. Не исследовались credentials, DB содержимое или runtime workloads. Текущие helpers рассмотрены только для actual dependency/ownership boundary; полная Rush integration security/state-machine review не входит в этот отчёт. Все будущие failure scenarios — статически выведенные проверки, не наблюдавшиеся failures реализованного компонента.

## 8. Упорядоченные обязательные изменения

1. **До A1:** исправить A0 ownership factory signatures/Go declarations; убрать возможность prototypes, placeholders и пустых concrete structs — finding 1.
2. **До A1:** заморозить полный минимальный controlled/scenario vocabulary, attempt-bound gates и rendezvous/errors — finding 2.
3. **До A1:** заморозить scenario/trace binding, versioning, completeness footer, gap/write-failure/tail policy и правило replay неполного execution — finding 3; согласовать accepted-input/command transport с владельцем API-контракта.
4. **До Process task:** зафиксировать конкретную platform strategy, permitted dependencies, output-cap precedence и platform proof obligations, без session coupling/pre-refactoring — finding 7.
5. **До раздачи slice tasks:** переназначить consumer portions E1/E8/E9/L1/L2, named cross-slice owner, fixture paths и test helper namespaces — findings 4 и 8.
6. **До A2:** утвердить независимые expected outcomes/partial orders/CLI outcome для smoke, включая одновременно живые roots и expected-failure L1; одного run→replay сравнения недостаточно — finding 5.
7. **В обязательном A2 gate:** выполнить named executable cancellation/bound/unfinished-cleanup scenarios с child-tree termination и unrelated-process isolation; зафиксировать фактически выполненные проверки и платформенные пределы — finding 6.
