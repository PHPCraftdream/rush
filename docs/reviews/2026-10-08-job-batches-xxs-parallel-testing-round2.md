# Job batches: второе независимое ревью A0/A1 и доказательной приёмки

**Вердикт: needs correction — контракт автономной фазы ещё требует исправлений до заморозки A0 и запуска A1.**

**Текущая версия:** `f48f883061cd66be30795c9dcf873f2da4ae79bd` (`f48f8830`), git blob плана `88be80856f854198cdf80d2ac29a3a8bb04939b2`, revision 2, 1378 строк. Координаты committed target предоставлены Main в задании; отдельная git-проверка в этом ревью не выполнялась.

**Предыдущая версия:** `51599fa4`; предыдущий отчёт — [`2026-10-08-job-batches-xxs-parallel-testing.md`](2026-10-08-job-batches-xxs-parallel-testing.md). Его находки 1–8 ниже сохраняют стабильные IDs **P1–P8**, как в Appendix A нынешнего плана. ID P1 не следует путать с уровнем серьёзности P1.

**Источник текущих ссылок:** [`docs/plans/2026-10-07-job-batches-design.md`](../plans/2026-10-07-job-batches-design.md), далее «план». Весь план прочитан диапазонами, а не только Appendix A. Предыдущий parallel/testing-отчёт прочитан полностью, включая строки, усечённые в первом выводе инструмента. Для пересечений прочитаны первичные API- и states-отчёты. Appendix A:1344–1378 и заявления предыдущих reviewers не использовались как доказательство исправления.

## 1. Итог и граница вердикта

Из восьми прежних находок **5 fixed, 3 partially fixed**. Нет unresolved, regressed или rejected среди P1–P8. Закрыты **P1, P4, P6, P7, P8**; остатки **P2, P3, P5** разобраны отдельно. Найдены **две новые автономные проблемы N1/N2**, которые не являются переименованием закрытых находок.

Ревизия существенно улучшилась: больше не предлагает Go-прототипы, предоставляет attempt-scoped rendezvous, публикует accepted-event/command/notice transport, выбирает process-isolation strategy, разделяет consumer-level ownership, вводит независимый `expect` и отдельные L5/L6. Четыре среза можно независимо **писать**, не дожидаясь готовой реализации соседнего среза; прежняя архитектурная претензия к самой возможности четырёхстороннего A1 не сохраняется.

Однако ещё нельзя отдать всем четырём исполнителям полностью согласованную data schema: неизвестен конечный срез журнала и способ его дочитать после Close; не замкнут lookup сценарных specs по `ref`; controlled actions не имеют полного phase/duplicate contract; declared oracle не может выразить обещанную проверку кодов; singleton policy ссылается на отсутствующее поле; новый helper одновременно обязан быть standard-library-only и пользоваться не-standard-library spawn wrapper.

Это **review плана**, а не констатация дефектов уже работающего `jobbatch`. Все контрпримеры ниже — **[INFERENCE]**, статически выведенные исполнения контрактов. Ни один из них не запускался. Никакие результаты builds/tests/race/smoke не заявляются.

## 2. Обязательная closure matrix P1–P8

| ID | Прежняя проблема | Статус | Текущее основание и причина |
|---|---|---|---|
| **P1** | A0 мог записать фабричные Go-prototypes или пустые concrete executors, затем столкнуться с реальными объявлениями A1. | **fixed** | §2:116–126 прямо отличает data/interface/error declarations и малые общие pure methods от единственных реальных тел owning slices; запрещает prototypes, function variables и placeholders. §7.1:1000–1005 оставляет фабричные сигнатуры в документе; §7.2:1041–1045 переносит совместную компиляцию на joint gate. `NewProcessExecutor` существует как контракт §5.4:787–790, не как второе объявление в `schema.go`. Старый redeclaration-контрпример больше не разрешён планом. |
| **P2** | Действия по node ID не были связаны с реальным start attempt; отсутствовали vocabulary и rendezvous, действия могли потеряться или воздействовать на следующий запуск. | **partially fixed** | §5.2:729–744 вводит ordered script, bound, `await_start`, token bindings, release/refusal/completion, Stop/Inject/output waits. §5.3:762–783 связывает actions с token, возвращает каждый зарегистрированный attempt один раз и запрещает очередь для неизвестной/завершённой попытки. Исходные проблемы адресации и sleep-based ожидания закрыты. Но registered-not-yet-accepted и already-released-not-yet-settled token не имеют объявленной допустимости `Complete`/повторного `ReleaseStart`; прежнее требование определить повторные и преждевременные действия закрыто не полностью. Остаток — §3.1 отчёта. |
| **P3** | Не было binding к содержимому scenario, полноты trace/хвоста, authoritative footer и различия неполного журнала с незавершённой execution. | **partially fixed** | §5.5:828–835 задаёт version, digest всего decoded/re-encoded scenario, непрерывные cursors и footer; :837–852 задаёт gap/write-failure и side-effect-free replay; L6:979–982 требует отрицательные проверки. `Ref` появился в Submit/Add (§2:194–203), а accepted events, команды и notices — в публичном feed (§4.1:596–617): прежняя полная недоступность producer transport больше не актуальна. Но footer не связан с независимо зафиксированным конечным producer Head; §3.4:578–580 закрывает Events до гарантированного drain; `complete: false` смешивает потерю trace и полноценный trace незавершённой execution; scenario lookup по `ref` не имеет требования непустоты/уникальности. Остаток — §3.2. |
| **P4** | Core/Process получали IDs, consumer-visible часть которых они сами не могут доказать; не был назначен владелец cross-slice tests. | **fixed** | §7.2:1034–1039 назначает Core только pure-event E2–E8 и engine-side E1, Runtime — R1–R5/public E8/E9, Process — process-level L1/L2 без CLI aggregate, Laboratory — ingress E1, CLI L1–L6 и journal privacy. §7.2:1049–1051 резервирует `lab/acceptance_test.go` за integration owner; §7.3:1076–1080 требует executable проверки и итоговую карту ID → test/scenario → boundary → expected/actual. E1:884–887 явно идёт через настоящий ingress. Прежнее несоответствие ownership закрыто; новое отсутствие поля singleton policy — отдельная N1, не повтор P4. |
| **P5** | Run/replay одного Engine и named smoke IDs подменяли независимый oracle; multi-root и ожидаемый nonzero могли проходить без нужного поведения. | **partially fixed** | §5.1:702–717 требует один Runner, exact exit codes и различает usage/unfinished/journal/oracle/failed outcomes. §5.2:745–751 требует `expect` для named smoke; E4/E5:899–909 фиксируют конкурентные roots, пробуждение never-started root, новые tokens и запрет spin; L3:967–969 сверяется с oracle. Это реально закрывает отсутствие oracle и возможность считать любой nonzero успехом L1. Но `expect.nodes` не содержит outcome code, а `starts` не определён как попытки или accepted executions; обещание :715–717 о mismatch при разных leaf codes не выражается замороженной схемой. Остаток — §3.3. |
| **P6** | Не было executable приёмки cancellation самой CLI, wait-bound expiry и неподтверждённого termination после bounded Close. | **fixed** | §5.6:861–866 прямо называет Ctrl-C и wait bound, свежий независимо bounded cleanup context, exit 3 и удержание admission при отсутствии confirmation. L5:974–978 выделяет все три пути, реальные process trees и unrelated process. §3.4:568–582 оставляет drain-loop после timeout, а R2:933–938 проверяет позднее подтверждение и повторный Close. §7.2:1039/§7.3:1076–1077 назначает реальные CLI checks Laboratory/A2. Блокер матрицы закрыт; это не доказательство того, что Ctrl-C/cleanup уже реализованы или проверены на Windows/Unix. |
| **P7** | Ссылка на существующие helpers не выбирала безопасную per-child strategy, разрешённые imports/файлы и output-cap precedence. | **fixed** | §0:31–33 ограничивает dependencies; §5.4:792–795 задаёт ceiling и запрет payload повышать cap. :797–815 выбирает `platform.Command`, Unix process group, Windows suspended creation → отдельный Job Object → resume, authoritative termination conditions, запрет `session` import и Process-owned `process_*.go`. Host Job Object больше не предлагается как leaf isolation. :1006–1008 передаёт strategy как A0 input; L2:962–966 проверяет tree/sibling/sentinel isolation. Это закрывает прежнюю неопределённость strategy. Новое противоречие helper/guard в N2 не отменяет данного выбора. |
| **P8** | Разные `_test.go`/testdata не предотвращали одинаковые package-level test helpers. | **fixed** | §7.2:1036–1039 задаёт `coreTest*`, `runtimeTest*`, `processTest*`, `labTest*` и четыре отдельные fixture directories; :1047–1051 запрещает общий mutable helper file и резервирует cross-slice acceptance file. §7.1:1009–1010 делает manifest частью A0. Предыдущий конфликт `testExecutor`/`newTestRunner` противоречит новой naming rule, а не остаётся разрешённым вариантом. Существующие prefix rules не нужно переоткрывать под другим названием. |

Статус **fixed** означает закрытие дефекта текста/распределения ответственности, а не пройденную приёмку реализации. Статус **partially fixed** относится только к явно описанным остаткам: закрытые части не переиздаются как новые findings.

## 3. Остатки прежних находок: блокеры автономного A0/A1

### 3.1 P2, остаток — phase и повтор controlled actions ещё выбирает implementer

**Серьёзность: P2.** Заморозить до выпуска `StartDecision`/scenario types в A0, §7.1:1002–1005.

**Основание:** §5.2:734–742; §5.3:762–783. Зафиксированы registered token, cancellation, accepted completion внутри `accept_complete`, неизвестный/завершённый token. Не зафиксированы допустимые actions в промежуточных фазах и результат повторной release. Registered attempt ещё не является accepted execution (§3.1:383–393), а released accepted attempt ещё не является settled.

**[INFERENCE] Контрпример:**

1. `await_start(A.1, bind=t)` вернул зарегистрированную, но удерживаемую внутри Start попытку.
2. Script вызывает `complete(t, Completed)` до `release_start(t, accept)`. Один Laboratory implementation считает token существующим и вызывает report; другой отвечает ошибкой «ещё не accepted». Нынешний текст не выбирает между ними, хотя отдельный `accept_complete` уже предназначен для immediate completion внутри Start.
3. Отдельный вариант: `release_start(t, accept)`, затем повторный `release_start(t, fail)` до completion. Token не unknown и не settled. Возможны error, idempotent acknowledgement или повторная запись gate decision. Fixtures с разными исходами не могут быть независимым эталоном одного контракта.

Это **не** прежняя ошибка node-ID addressing: обе операции именуют правильный token, и новая попытка не затрагивается. Это оставшаяся phase/duplicate часть прежнего P2.

**Наименьшая коррекция:** кратко задать фазы attempt: registered → decision committed → accepted или retired rejected → settled. Назвать допустимую фазу каждого mutating action. Например, `ReleaseStart` коммитит решение ровно один раз; `Complete` разрешён только accepted execution, ранний report разрешён только через `accept_complete`; повторная release, действие к retired rejection и неправильная фаза немедленно возвращают один объявленный lab error и никогда не меняют gate/не ставятся в очередь. Можно использовать уже названный `ErrUnknownAttempt` для недоступной фазы, если именно это решение явно принято; новый framework не нужен. Указать observable результат соответствующей ошибочной script action.

**Приёмочный сценарий:** без sleep проверить ранний Complete и противоречащую повторную release; в обоих случаях один заранее объявленный результат, ни одного скрытого callback/изменения первого решения. Отдельно first attempt capacity-refused → сигнал → новый token: actions старого token ошибаются и не влияют на новый. Нормальная последовательность `await_start → release accept → await_state Running → complete` и `accept_complete` работают по-разному только в документированной границе callback. Runtime-owned fixture, а не отменяемый Controlled.Start, проверяет accepted handle, возвращённый после cancellation/первого Close timeout из R2:937–938; это законная межсрезовая зависимость, не дефект controlled API.

### 3.2 P3, остаток — footer пока не доказывает полноту фактически принятого input

**Серьёзность: P1.** Это главный shared producer/consumer blocker для Runtime/Laboratory; исправить до A1, не лечить sleeps или blocking observer.

**Основание:** §2:253–254, 266–267; §3.4:574–580; §4.1:596–617; §5.5:828–850. Producer cursor и transition sequence различаются: один accepted event может породить несколько records. `Summary.Sequence` корня не заменяет конечный cursor всего Runner.

#### A. Независимой конечной границы и разрешённого final drain нет

Footer содержит `last_cursor`, но план не говорит, что он должен равняться независимо зафиксированному конечному **producer** Head, а не последнему cursor recorder. «Reads Events continuously» не обеспечивает успевание consumer. Одновременно Closed означает, что все прочие Runner methods возвращают `ErrClosed` (§3.4:578–580), включая Events; исключения для дочитывания retained records нет.

**[INFERENCE] Конкретный хвост:** root уже terminal, recorder записал его event/command/notice records. Последняя script action `capacity_available(g)` принята и ничего не меняет в terminal root states/counts. При cleanup принимается HostClosing. Recorder ещё не прочитал эти records; Runner переходит в Closed. Если Laboratory пишет footer из своего последнего cursor с `complete: true`, prefix содержит непрерывные cursors, correct root states/counts, а prefix replay совпадает с commands/notices. Ни root `expect`, ни digest не доказывают, что два принятых события отсутствуют в trace. Если Laboratory честно откажется от complete, нормальный run будет терять пригодный журнал даже при достаточном retention. Дополнительный случай — задержать recorder до final Settled/root notice при Close: конечные records созданы, но недоступны через разрешённый Closed API.

Требование «removed tail record rejected» (§5.5:847–849, L6) помогает проверить физическое удаление record при сохранённом корректном footer. Оно **не** проверяет ошибку изначального создания footer для укороченного producer trace.

**Наименьшая коррекция:** определить finite capture boundary, независимую от скорости writer. Для normal Closed run достаточно сохранить окончательный producer Head/transition boundary и разрешить Events читать retained tail после Closed до этой границы, затем сообщать конец; остальные mutation APIs можно оставить Closed. Footer `last_cursor` берётся из этой границы; writer обязан дойти до неё или объявить потерю. Для timeout/незавершённого run также определить конечный согласованный cut и footer state именно на этом cut, а не смешивать snapshots разных последующих transitions. Конкретный wire/API shape выбирает владелец A0; обязательны boundary и drain semantics, не Store, не durable recovery и не блокировка event loop.

**Приёмочный сценарий:** управляемо задержать recorder без overflow, принять trailing CapacityAvailable/HostClosing и окончательное settlement, закрыть Runner, затем дать recorder продолжить. Полный trace либо дочитывается до authoritative Head, либо явно отвергается как потерянный; непрерывный укороченный prefix никогда не получает complete. Повторить с gap, write failure, удалённым footer/последним record и подменённым footer cursor. Успешный run сравнивается с заранее известным количеством/типами принятых tail events, а не с последним cursor самого файла.

#### B. Полнота trace и завершённость executions остаются одним bool

§5.5:837–840 выставляет `complete: false` и при gap/write error, и при execution, которая всё ещё Cancelling после timeout. §5.5:849–850 обещает replay incomplete journal с exit 3, хотя `run` для journal loss имеет exit 5 (§5.1:709–710), а внутренний gap вообще должен быть rejected. Непустые `unfinished` IDs не исключают одновременную потерю records. Поэтому footer не различает целый captured prefix незавершённой execution и повреждённый captured prefix; прежнее требование P3 осталось открытым.

**Наименьшая коррекция:** разделить два факта: capture integrity до объявленного cut и наличие terminal outcome у всех roots. Например, `complete` обозначает только capture integrity, а `unfinished` и явно названная execution status — завершённость работы. Определить replay outcome для целого unfinished trace отдельно от gap/write-loss и правила частичного чтения до первой повреждённой границы. Не обещать replay через пропущенное scheduling event.

**Приёмочный сценарий:** held controlled execution → bounded Close timeout, но trace до finite cut цел; replay восстанавливает nonterminal Cancelling/held admission и сообщает unfinished, не corruption. Отдельно такой же unfinished run с потерянным record имеет иной integrity status и не получает доказательство полного capture. Successful terminal run с journal gap не считается валидным полным replay.

#### C. `ref` transport появился, но однозначный scenario lookup ещё не заморожен

§2:194–203 оставляет обычный caller Ref optional; §2:209–210 задаёт только alphabet/length. §5.2:731 не требует непустые/уникальные refs script actions, тогда как §5.5:832–844 восстанавливает specs по `ref`.

**[INFERENCE] Контрпример:** два Add к одному parent с одинаковым `ref`, но разными specs/payload references. Оба запроса валидны по описанным validation rules и получают разные created IDs. Digest фиксирует наличие обеих actions, но не выбирает, какой spec lookup является ответом `scenario[ref]`. Можно придумать occurrence-order matching, однако этот дополнительный контракт сейчас не объявлен и не должен выбираться Laboratory самостоятельно.

**Наименьшая коррекция:** требовать непустой уникальный `ref` для каждой Submit/Add action replayable lab scenario и проверять это до Runner/process launch. Не менять optional Ref ordinary Runner ради Laboratory. Другой вариант допустим только с полностью объявленным one-to-one action identity, не с догадкой по goroutine timing.

**Приёмочный сценарий:** два разных Add вокруг controlled settlement с разными refs однозначно восстанавливают оба accepted subtree и их порядок; duplicate/empty lab refs отвергаются до side effects. Перестановка command descriptors обнаруживается; payload/message/summary/details sentinels не попадают в journal. Producer-projection детали (`ControlReported.op`, layout event/command/notice records) относятся также к API A3; настоящие `Ref` и command/notice transports уже добавлены и не объявляются повторно отсутствующими.

### 3.3 P5, остаток — oracle не выражает обещанную проверку leaf codes

**Серьёзность: P2.** Нужна малая коррекция frozen `expect` schema перед A0, не отказ от нового oracle.

**Основание:** §5.1:715–717; §5.2:745–751; §5.4:816–818; L1:959–961. `expect.nodes` перечисляет `{id, parent, state, starts}`, но не `code`. Отдельный `expect_output` проверяет bytes/EOF, не `Result.Code`. Прямо обещанный oracle mismatch «when the leaf codes differ» не выражается этой схемой. Значение `starts` также не разделяет Start calls/admission attempts и реально accepted executions.

**[INFERENCE] Контрпример, не зависящий от трактовки `starts`:** три настоящих процесса L1 действительно стартовали, два Completed, третий вышел с 7. Ошибочный adapter сообщает `Failed/exit_9` вместо `Failed/exit_7`. Root Failed, counts, parent/IDs, число starts, max_active, before relations, notices и CLI exit 1 совпадают. Journal faithfully хранит неверный safe code; replay тем же Engine даёт ту же структуру. Замороженный `expect` не способен принять правильный run и отвергнуть этот неправильный. Вариант с `missing_program` вместо ожидаемого Failed leaf дополнительно показывает, почему нельзя оставлять `starts` неоднозначным.

**Наименьшая коррекция:** добавить exact machine outcome `code` в node expectations; для необходимых cancellation checks отличать winning state/cause от reported outcome, не требуя Summary/Details. Дать один точный смысл существующему `starts` и, если scenarios проверяют capacity retries, отдельный count attempts/accepted starts либо эквивалентное assertion. `max_active` должен измеряться на executor-acceptance/termination границе fixture, не копироваться из engine budget counter, иначе ошибочный dispatcher может согласоваться с собственной диагностикой. Не вводить general telemetry subsystem: достаточно независимого fixture oracle и безопасных уже существующих records.

**Приёмочный сценарий:** L1 fixture ожидает у конкретного leaf `exit_7`; настоящий helper exit 7 проходит с CLI 1, неправильный код и вариант missing program не проходят matched expectations. Их нельзя засчитать как ожидаемый failure по одному nonzero. E4: A accepted и удерживается, B уже submitted в том же Runner и никогда не стартовала; только подтверждение termination A допускает B, без сигнала к B. E5: first refusal, заданный relevant signal, новый token, ровно один accepted start, никакого retry без следующего signal. Checks сравнивают независимые expected constants/history с наблюдением fixture; run→replay equality остаётся дополнительной, не главной проверкой.

## 4. Новые находки: не переиздание закрытых P1/P4/P6/P7/P8

### 4.1 N1 — singleton normalization ссылается на policy, которой нет в scenario schema

**Серьёзность: P2; новый A0/Laboratory blocker.**

**Основание:** §2.1:287–290 требует нормализацию один раз с сохранением selected policy. §5.2:753–754 говорит «with the scenario's policy». Но список top-level fields/actions §5.2:724–751 содержит version, limits, runner, wait bound, payloads, messages, script и expect — **не policy**. Submit action содержит только ref/root/spec; raw TaskSpec §2:136–141 также не содержит batch policy. Неизвестные поля decoder отвергает (§5.2:721–722).

**[INFERENCE] Контрпример:** вызывающий хочет raw standalone task с Sequential/NotifyEach и сравнивает её с explicit one-task Batch той же policy в E1. В task shape нет места передать выбранную batch policy. Добавленный `policy` будет unknown field; без него Laboratory либо отвергает raw task, либо самостоятельно выбирает defaults, либо вводит недокументированную схему. E1 ownership уже исправлен в P4, но владелец не получил полный ingress input.

**Наименьшая коррекция:** добавить одно конкретно названное scenario/submit policy field для singleton normalization, со значениями batch Mode/OnFail/Notify/MaxParallel и объявленными defaults/validation. Если выбирается scenario-level `policy`, прямо определить его применение только к raw root tasks и отсутствие вмешательства в explicit batches/nested leaves. Сохранить existing policy/default conventions из §2.1, не придумывать второй нормализатор.

**Приёмочный сценарий:** через настоящий ingress подать raw task и explicit singleton при двух выбранных policies, сравнить actual tree, policy views, один leaf launch и selected result/notices. При nested Submit/Add число узлов не меняется из-за wrapping. Неизвестная policy field/value отвергается до factory/side effects. Нельзя вручную строить одинаковые BatchSpec в Core test и выдавать это за ingress proof.

### 4.2 N2 — новый descendant helper противоречит обязательному spawn wrapper и ownership guard

**Серьёзность: P2; новый A0/Process fixture blocker.**

**Основание плана:** §5.4:799–801 требует spawn через `platform.Command`; :819–822 требует portable helper под `internal/jobbatch/lab/testdata/process/helper`, **standard library only**, со spawning descendant mode. Process владеет helper (§7.2:1038), но изменение guard/shared helpers требует owner decision (§5.4:814–815).

**Проверенное текущее code evidence:**

- `internal/platform/command.go:8–21,40–43`: wrapper — единственный sanctioned constructor; вызывает hardening.
- `internal/platform/spawn_guard_test.go:17–28,44–46,50–61,78–108`: guard обходит все `.go` под `internal/`, включая testdata, ловит прямые `exec.Command`, `exec.CommandContext`, `exec.Cmd` literals; нового helper в exemptions нет.
- `internal/platform/hide_window_windows.go:53–58`: wrapper сохраняет HideWindow и добавляет CREATE_NO_WINDOW; импорт platform не является standard-library-only зависимостью.

**[INFERENCE] Контрпример:** Process пишет обычный переносимый descendant mode через `os/exec`. Его helper standard-library-only, но предусмотренный repository gate считает файл offender; Process не вправе самостоятельно добавить exemption в чужой guard. Если helper использует требуемый `platform.Command`, guard соблюдён, но нарушено standard-library-only. Обход распознаваемых AST конструкций через `os.StartProcess` не исправляет требование единственного approved wrapper. Это не возврат к старой неопределённости Unix/Windows isolation P7: isolation strategy теперь конкретна; противоречие создано новым helper contract.

**Наименьшая коррекция:** разрешить portable helper импортировать `internal/platform` для descendant spawn и убрать только его требование standard-library-only. Standard-library boundary самого Engine/Runtime остаётся неизменной; session/app/DB по-прежнему запрещены. Альтернатива — явное owner-approved исключение с описанным hardening и A0 ownership соответствующего guard change, но она шире и не должна неявно передаваться Process.

**Приёмочный сценарий:** A2 единожды строит выбранный helper и выполняет existing spawn guard без исключения/обхода, скрыто сделанного Process. Через CLI/Runner helper подтверждает ready parent и ready descendant, Stop завершает только их tree, sibling/sentinel остаются живы. Held mode должен действительно удерживать процесс до отмены: документированный `wait until stdin closes` при обычном `Cmd.Stdin == nil` может получить EOF сразу. Поэтому выбранный named fixture обязан иметь явный liveness rendezvous/механизм удержания, а не считать «Start вернул nil» достаточным доказательством живого cancellation target. Это требование доказательного fixture, не просьба добавлять production process Messenger или новый транспорт.

## 5. Четырёхсторонний A1: зависимости допустимые и ещё не замкнутые

| Срез | Реальная зависимость | Почему она не требует последовательного написания |
|---|---|---|
| Core | A0 types/events/errors, admission/lifecycle rules | Настоящие `Engine` и его методы принадлежат Core. Runtime/Lab не должны создавать временные аналоги. |
| Runtime | Замороженный Engine API, Launch/Transition, phase и drain/record producer semantics | Может писать against signatures; реальный Core требуется на A2. Остаток P3 касается frozen producer contract, а не необходимости дождаться Core до fan-out. |
| Process | Payload/ProcessOptions, Execution/OutputReader, approved per-platform strategy и согласованный helper contract | Factory с настоящим телом и `process_*.go` принадлежат Process; N2 требует одного A0 решения о helper dependency, не shared refactor. |
| Laboratory | Runner API, Process factory, собственный controlled executor, singleton/scenario/journal/oracle types | Настоящие Runtime/Process нужны на joint gate. P2/P3/P5/N1 — недостающие данные/семантика общего контракта, не основание писать placeholders или второй scheduler. |

§7.2:1041–1051 правильно отделяет editing independence от joint compilation. Shared data files, README/changelog и cross-slice acceptance file имеют одного владельца. Prefixes и fixture directories устраняют прежнюю namespace проблему. Process-level tests не присваивают себе CLI aggregate, а Core не присваивает consumer normalization.

Следовательно, **не нужны** пятый scheduler slice, сериализация Core→Runtime→Process→Laboratory, общие mutable test helpers или импорт session ради реализации helper. Нужны перечисленные небольшие contract decisions владельца A0 и затем одна совместная проверка A2.

## 6. Доказательная приёмка A2: что уже правильно назначено и что действительно наблюдать

Это рекомендации для исполнения **существующей** матрицы после исправления блокеров, не результаты данного ревью и не новые findings к закрытым P4/P6.

1. **Сборка совместного продукта:** настоящие Engine/Runner/process/controlled factories и `cmd/batchlab`, без замены shared declarations, удаления tests или временных тел. Затем focused tests и доступный `-race`, после handoff всех четырёх slices. Prefix rules распространять на все создаваемые helper symbols.
2. **E1 и E9 на consumer boundary:** использовать real lab parser/ingress, Snapshot actual tree, Output до Release, Released errors после него, сохранённый immutable Batch.Wait и запрещённый повтор root ID. Privacy sentinels должны отсутствовать в actual journal/feed, а не только в struct definitions.
3. **Независимые E4/E5/L3:** roots одновременно присутствуют в одном Runner; held execution/attempt и signals задаются rendezvous, не sleep. Проверять actual calls/acceptances, разрешённый partial order, counts и notices по заранее заданным expectations, не вычислять ожидаемое тем же reducer.
4. **L1 expected failure:** compiled command с точным exit code и per-leaf machine outcomes. `go run` examples §5.1:692–694 полезны для ручного запуска, но program codes 3/4/5 нельзя доказывать status самого `go run`: проверяется непосредственно собранный `batchlab` executable. Missing helper/usage/oracle failure не равны matched Failed aggregate.
5. **L2/L5 cleanup:** реальный child, descendant, sibling и unrelated sentinel с readiness handshake. Отдельно explicit Stop, cancellation самой CLI через поддержанный платформой signal path и scenario bound. Observer `context.Cancel` внутри unit test или nil от Stop не заменяет cancellation запущенного executable/подтверждение termination. Cleanup использует свежий context; controlled no-confirmation остаётся nonterminal/held и даёт exit 3. Способ подачи Ctrl-C в Windows/headless test environment должен быть назван в executable fixture и фактически проверен либо явно указан как platform limit, не заменён убийством самой CLI до её cleanup.
6. **L4/L6 journal:** normal, Failed и Cancelled replay сверяются с независимым oracle; file-writing sentinel не создаётся и factories не конструируются. Дополнительно проверяются delayed final recorder/authoritative Head, полный unfinished cut, реальный retention gap/write failure и duplicate refs. Нельзя чинить потерю events Snapshot-ом и считать восстановленным непрерывный trace.
7. **Integration owner:** записывает окончательную карту всех E/R/L IDs с actual command/result и пределом наблюдения (§7.3:1078–1084), проверяет platform guard/repository checks единожды после интеграции, затем перепроверяет затронутый consumer path после коррекций. Agent handoff и static source assertions не считаются runtime evidence.

## 7. Что законно отложено до Rush, а что нельзя отложить

**Законно отложено:** Rush numeric defaults/group mapping, external ledger capacity signals, durable admission/ack/terminal authority, recovery, legacy-agent outcome migration, MCP result envelopes и shared-connection cancellation, SDK/inline/background delivery, authenticated cross-process controls и WebUI. Это отдельная §9:1139–1342; ни один finding этого отчёта не требует начать эти работы, model/API workloads, DB migration или существующие tool registrations.

**Нельзя отложить:** ordinary-Go scenario normalization/policy, data-only shared Go declarations, actual controlled attempt semantics, конечную journal boundary, real independent oracle и автономный safe process cleanup. Они непосредственно нужны самостоятельному компоненту и `batchlab` из §0:19–39. Bounded Events/replay не являются durable exactly-once notifications или crash recovery.

## 8. Сильные стороны и пределы покрытия

### Сильные стороны текущей ревизии

- Прямое признание Go declaration semantics и единственных owning bodies устранило реальный A0/A1 compile conflict без function-variable shim.
- Token-bound controlled rendezvous и explicit refusal/completion vocabulary позволяют воспроизводить capacity/Stop/Add races без второго scheduler.
- Один Runner для forest, precise E4/E5 и независимый `expect` значительно сильнее прежних named smoke labels.
- Consumer ownership и единственный A2 acceptance file теперь совпадают с реальными наблюдаемыми границами.
- Per-child Unix/Windows strategy, output ceiling и запрет session coupling дают Process конкретное задание.
- L5/L6 делают command cancellation, bounded unfinished cleanup и journal loss явными обязательствами; loop draining и no-synthetic-termination остаются правильными гарантиями.
- Согласованные prefixes/fixture directories и отсутствие mid-flight checks сохраняют настоящую параллельность написания.

### Пределы

Проверены все 1378 строк revised plan, весь прежний parallel/testing report, related API/states reports и указанные узкие platform/guard участки. Не исследовались Rush credentials, DB содержимое, workloads или широкая реализация integration paths. Не проверялись фактический compiler/toolchain, availability `-race`, Go/x/sys suspended-resume implementation, Unix group/Windows Job Object behavior, signal delivery и реальное выполнение fixtures.

Сборки, тесты, runtime commands, lint и formatters **не выполнялись**. Git commands/commits/push/worktree changes также не выполнялись. Единственная запись задания — этот новый report; plan, исходники, configs и предыдущие отчёты не изменялись. Пользовательский deleted `web/dist/.gitkeep` не затрагивался. Все proposed acceptance scenarios — [INFERENCE], а не якобы пройденные проверки.

## 9. Упорядоченные обязательные изменения

1. **До заморозки A0:** закрыть residual **P3**: authoritative finite capture boundary/Head, final Events drain после Closed, согласованный unfinished cut, отдельные trace-integrity/execution-finished факты и непустые уникальные lab action refs. Согласовать единый producer/consumer contract Runtime и Laboratory.
2. **В A0 scenario/oracle types:** исправить **N1** и residual **P5**: объявить источник singleton policy, exact leaf code expectations и точный смысл attempt/accepted-start counters. Сохранить независимый oracle, exact CLI outcomes и один Runner.
3. **До Laboratory task:** закрыть residual **P2**: допустимые фазы/повторные controlled actions, retired rejected token и observable error policy. Не менять уже исправленную token addressing model.
4. **До Process task:** исправить **N2**: согласовать helper imports с approved wrapper/guard и заморозить executable fixtures с настоящим held/readiness behavior. Не менять shared platform files силами Process и не возвращать session coupling.
5. **На A2 одним владельцем:** исполнить совместные E/R/L consumer proofs, direct-binary L1–L6, platform guard и доступные repository/race checks; сохранить actual commands/results/platform limits. Ни обещание Appendix A, ни run→replay equality, ни report implementer не заменяет этот gate.

**Итог передачи:** 5/8 прежних findings закрыты; 3/8 закрыты частично (**P2 — phases/duplicates; P3 — final cut/drain/integrity/refs; P5 — code/counter oracle**). Новые **N1 — отсутствующий singleton policy input**, **N2 — helper standard-library-only против approved spawn wrapper**. Все пять оставшихся тем относятся к самостоятельному A0/A1/A2, а не к легитимно отложенной Rush integration.
