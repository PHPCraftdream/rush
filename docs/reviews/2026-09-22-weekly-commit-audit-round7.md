# Rush: независимый release-readiness аудит, round 7

Дата: **2026-09-22**. Проверенный production HEAD:
`4b2b2a0ccca614f6840f1bbaae421c2f5be2c8b0`.

## Summary

**Release verdict: NO-GO для общей alpha с заявленными возможностями.**

Новый `4b2b2a0c` устраняет оба конкретных механизма R3-1 из round 6:
повторное использование ID предыдущей попытки и несинхронизированные обращения
к assistant ID. Прежний P1 по этим доказательствам **снят**. Mutex, отдельный
объект на попытку и запрет записи после `resolve` действительно присутствуют;
называть прежнюю гонку памяти всё ещё открытой было бы неверно.

Однако найден другой порядок событий: очередь может начать исполнять копию
вызова **до** возврата отправителя из `Run` и запечатывания evidence. Тогда
собственный ID queued-вызова успевает попасть в capture, а его `(nil, nil)`
admission outcome допускается к retry classification как отсутствие ошибки
исполнения. В частности, возможен повтор после постоянной ошибки провайдера.
Этот более узкий дефект учитывается один раз как **R7-1 / P2**; это не возврат
к старому утверждению о чужой строке или data race.

Сохраняются **16 P2 round 6**. Итого: **17 открытых P2, подтверждённых открытых
P0/P1 в исследованном срезе не установлено**. F3 включён в R2-2, отдельного
подсчёта нет. Отсутствие P1 не означает готовность всех заявленных режимов:
остаются ошибки точности результата, фазового lifecycle, конфиденциальности
диагностики, сетевых resource bounds и web render.

Нового доказанного циклического deadlock, Go data race или безусловной
production panic в изменениях после round 6 не найдено. Статический аудит
не доказывает их отсутствия во всём проекте. Код и тесты не исправлялись.

## Срез, метод и состояние рабочей копии

- Период: **2026-09-15 00:00:00 +02:00 — 2026-09-22 15:44:53 +02:00**,
  `Europe/Berlin`, по committer date среди предков указанного HEAD.
- База перед периодом: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  2026-09-11 14:08:09 +02:00. Первый коммит периода — 19 сентября.
  `--since` и `--since-as-filter` дают **39 коммитов**. Более ранняя нижняя
  граница первых отчётов не меняет состав общей ранней части серии.
- Совокупный diff: **89 файлов, 12091 добавленная / 746 удалённых строк**,
  включая tests, checkpoints и предыдущие отчёты. Собственный documentation
  commit round 7 не является частью проверенного production-среза.
- От HEAD round 6, `fbc8a177`, добавлены четыре коммита, перечисленные ниже.
  Их совокупный diff — 5 файлов, 1408 добавленных / 547 удалённых строк;
  большая часть — отчёт round 6 и перенос тестов.
- Изучены log, состав недельного diff, изменения production-путей, текущие
  реализации и вызывающий код, предыдущие findings и выбранные regression
  tests. Доказательства — статическая трассировка ветвей, значений и допустимых
  последовательностей событий. Сценарии ниже **не выдаются за выполненные тесты**.
- Для семантики нижнего TCP dial и CONNECT parser прочитаны локальные исходники
  Go **1.26.3**. Внешние сервисы, реальные ключи и provider endpoints не проверялись.
- **Изоляция окружения фактически отсутствовала:** `git worktree list` показал
  единственный worktree на `main`. В начале status содержал только
  ` D web/dist/.gitkeep`. Позднее появились чужие правки
  `internal/agent/agent_turn_failure.go`, `internal/app/app_run_reviewer.go`,
  `internal/cmd/ping.go`, `internal/cmd/ping_test.go` и новые
  `internal/agent/quota_reset.go`, `internal/agent/quota_reset_test.go`,
  `internal/agent/quota_reset_turn_test.go`.
  Они не изменялись аудитом и не считаются исправлениями проверенного HEAD.
  Затронутые параллельной работой исходники дополнительно читались через
  `git show 4b2b2a0c:<файл>`. Все ссылки на строки относятся к **этому commit**.
- Единственный записываемый аудитом файл — этот Markdown. Удаление `.gitkeep`
  сохранено. Новые worktrees, sub-agents, изменения версий/конфигурации,
  установка зависимостей и push не выполнялись. Чужое незакоммиченное состояние
  нельзя очищать ради формально чистого status; commit ограничивается явным
  pathspec отчёта.

Предыдущие отчёты:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md),
[round 6](2026-09-22-weekly-commit-audit-round6.md).
Их утверждения сверены с исходниками; названия fix-коммитов и сообщения
об исторических успешных тестах не заменяют проверку механизма.

### Изменения после round 6

| Commit | Изменение и результат проверки |
| --- | --- |
| `07cabfae` | Отчёт round 6; production-поведение не меняет. |
| `1420acb1` | `var ownResult =` заменён на `ownResult :=` в тесте; семантика прежняя. |
| `52a456b8` | Evidence-тесты перенесены из `coordinator_retry_test.go` в `coordinator_retry_evidence_test.go`; это перенос, не исправление runtime. |
| `4b2b2a0c` | Добавлен `attemptEvidence`; capture перевооружается перед initial/401/transient попытками, чтение и запись синхронизированы. Новый regression test моделирует queued outcome без callback; ранний dispatch до seal не покрывает. |

### Карта остальной недельной серии

| Коммиты | Проверенные области |
| --- | --- |
| `57fffb05`, `2169dbe3`, `b0a55aab` | Continuation/retry, refused admission, выбор собственного assistant evidence. |
| `ccb510ff`, `babd295c`, `54acbf71`, `8aabba4d` | Reviewer, credentials/context, fail-fast, сборка continuation через tool steps и точная конкатенация. |
| `0f445020`, `eb59df12`, `fd18cf0c`, `d0966df9` | Сетевая конфигурация, provider clients, proxy/DNS/DoH, handshake guards, transport cache и cleanup wrappers. |
| `f7362f7f` | CLI timeout defaults, idle policy и её durable/extension взаимодействия. |
| `14dd4a54`, `924add8a`, `10e32c56`, `97c22050` | Formatter, sub-agent metadata, timestamps и Playwright-покрытие. |
| `87ee064f`, `64409ab3`, `180f1544`, `5c20c771`, `c9eb9a98`, `c32c72d6` | MCP env/identity/timers, context-aware test doubles, lint-правки и parallel marker. |
| `4357e157`, `fbc8a177`, `a275e323` | Optional peak-hours message, config/CLI/WS/web mapping, textarea labels, committed version/schema metadata. |
| `df0051b1`, `85ee8836`, `bf9f6d3d`, `dd03ecf0`, `6f134f0a` | Checkpoints; исполняемый код не меняют. |
| `e547e12e`, `31e90d2e`, `295cc386`, `a4f99d8b`, `6e18502f` | Отчёты предыдущих проходов. |

## Подтверждённые и опровергнутые findings предыдущих раундов

P1 — существенное нарушение исполнения/изоляции, блокирующее общую alpha.
P2 — ограниченный, но доказанный дефект; для соответствующего режима требуется
исправление либо явно объявленное ограничение. «Закрыт» ниже означает устранение
конкретного описанного механизма по исходникам, без нового runtime acceptance.

| ID | Статус на `4b2b2a0c` | Основание |
| --- | --- | --- |
| F1 | Закрыт | `app_run_reviewer.go:599` присваивает reviewer context, `:683` очищает reservation. Старые smart-toolset/stale-token сценарии не актуальны. |
| F2 | Закрыт | `app_run.go:513`: configured auto-review не запускается при `req.Credentials != nil`. |
| F3 | Учтён в R2-2 | Потеря всего префикса исправлена; остаток whitespace-only считается один раз. |
| F4 | Закрыт в прежнем объёме | CONNECT/DNS guards, DoH exchange timeout и SOCKS budget присутствуют. Неполнота initial TCP dial отдельно учтена в R6-1. |
| F5 | Закрыт в прежнем объёме | Cache на 8 entries, idle timeout 90s, eviction после unlock и forwarding cleanup. Старого бесконечного idle lifetime нет. |
| F6 | P2, подтверждён | Provider и global network defaults читаются из разных snapshots. |
| F7 | P2, подтверждён | Durable spec и converters не сохраняют IdleTimeout. |
| F8 | P2, подтверждён | Финальный reviewer inventory исключает primary tool calls. |
| F9 | P2, подтверждён | Formatter разыменовывает JSON null вне catch. |
| R2-1 | Закрыт | `socksDialer:86` задаёт срок на весь dial; DoH-over-SOCKS использует его же. |
| R2-2 | P2, частично закрыт | Tool-step и mid-word примеры исправлены; whitespace-only error fragment всё ещё отбрасывается. |
| R2-3 | Закрыт | `buildReviewerPassTurn:677` переносит FailIfSessionBusy, `mailbox.submit:69` атомарно отказывает без enqueue. |
| R2-4 | P2, подтверждён | Gate не проверяет parent cancel; cached terminal/cleanup handles переживают смену фазы. |
| R2-5 | P2, подтверждён | Network URLs с секретными компонентами попадают в error strings. |
| R2-6 | P2, подтверждён | Oracle parallel cache-test несовместим с разрешённым eviction другими tests. |
| R3-1 | Прежние P1-механизмы закрыты | `b0a55aab` исключил чужую строку; `4b2b2a0c` исключил ID прошлой попытки и Go race assistant callback. Остаток контракта queued outcome учитывается только в новом R7-1 / P2. |
| R3-2 | P2, подтверждён | Custom HTTP CONNECT не добавляет default port. |
| R4-1 | P2, подтверждён | Custom CONNECT response head не имеет byte budget. |
| R4-2 | P2, подтверждён | Custom resolver возвращает один IP без address fallback. |
| R5-1 | P2, подтверждён | 401 rebuild теряет временную reviewer model identity. |
| R5-2 | P2, подтверждён | OAuth refresh не получает provider network client. |
| R5-3 | P2, подтверждён | Terse stdout содержит primary и reviewer без выбора единственного результата. |
| R6-1 | P2, подтверждён | Initial TCP dial HTTP proxy-only/DoH не имеет собственного timeout. |
| R6-2 | P2, подтверждён | Extension при hard cap=0 ограничивается исходным idle deadline. |

Полностью ложных исторических findings не установлено. Опровергнута их
**актуальность после соответствующих исправлений**, а не историческое
существование F1/F2/F4/F5/R2-1/R2-3 и прежних контрпримеров R3-1.
Отдельно опровергается обобщение нового комментария «queued attempt's instance
stays empty»: оно верно для позднего callback после seal, но не для R7-1.

## Новый finding round 7

### R7-1. P2 — ранний dispatch queued-вызова допускает retry без его error outcome

**Коммиты:** исходный retry/continuation путь — `57fffb05`; own-ID callback —
`b0a55aab`; проверяемая граница seal — `4b2b2a0c`.
**Места:** `internal/agent/coordinator_attempt_evidence.go:47,61`
(`record`, `resolve`); `coordinator_run.go:326,441,449,504,525,549,800`;
`agent_run.go:81,91,553`; `mailbox_ownership.go:73,266`;
`agent_turn_step.go:175`; `internal/app/app_run_turn.go:68`.

**Механизм.** `mailbox.submit` публикует полную копию SessionAgentCall, включая
callback, и отпускает mailbox mutex. Чужой dispatcher уже вправе забрать её.
Ни возврат `(nil, nil)` в отправителе, ни последующий `curAttempt.resolve()`
не происходят атомарно с публикацией. Mutex evidence предотвращает data race,
но допускает обычный порядок `record → resolve` из разных goroutines.

Следовательно, queued outcome может иметь непустой **собственный** assistant ID.
Coordinator не проверяет admission outcome до classifiers. Переданный им
`originalErr == nil` означает «поставлено в очередь», но `shouldRetryTurn:800`
трактует его как «исполненная попытка вернулась без ошибки». Настоящая ошибка
queued turn возвращается другому dispatcher, а не отправителю.

**Статически допустимый сценарий с барьерами; не исполнялся:**

1. B владеет сессией. A вызывает coordinator с `FailIfSessionBusy=false` и
   ненулевым retry budget. `submit` ставит A в очередь. Приостановить goroutine A
   после unlock `submit`, до возврата `Run`/seal.
2. B завершает собственный turn и забирает A через `drainOrReleaseFinal`.
   При PrepareStep queued A callback записывает ID `mA` в ещё незапечатанный
   capture. Провайдер для A возвращает обычную **400** ошибку без text/reasoning/tools;
   `handleStreamFailure` сохраняет `FinishReasonError` на `mA`. Dispatcher B
   возвращает эту ошибку и освобождает сессию. Никакой transient error не нужен.
3. A продолжает: его исходный `Run` возвращает `(nil, nil)`, `resolve` выдаёт
   `mA.ID`. `shouldContinueTurn` отказывает из-за отсутствия progress;
   `shouldRetryTurn` видит error finish, отсутствие progress и **nil err**,
   поэтому возвращает true до классификации постоянной ошибки.
4. После backoff A снова вызывает `Run` с тем же запросом. Имеется второй
   provider attempt после terminal 400; обычный явно возвращённый 400 такого
   retry не разрешил бы. Внешний слой сможет распознать queued outcome только
   после возврата coordinator, то есть слишком поздно.

Все события используют законное владение одной сессией; одновременные provider
turns под одним lock не нужны. Нужна задержка отправителя после публикации
очереди, а не перестановка операций внутри mutex. При callback после seal
новый код корректно игнорирует запись — этот путь не объявляется сломанным.

**Риск и severity.** Лишний запрос, неверный результат queued admission и
повтор операторской ошибки. Это P2: узкое расписание и queue-capable caller;
не доказаны memory race, обход авторизации или обязательное повторение tool
side effects. Positive IdleTimeout не исключает пример с 400/не-stall finish.
Fail-fast admission исключает именно этот queued сценарий.

**Покрытие:** `TestRunInternal_QueuedAttemptNotRetriedOnStaleEvidence:573`
возвращает `(nil, nil)` из mock вообще без callback. Он содержательно закрывает
старый stale-ID пример, но не проверяет публикацию в mailbox и ранний drain.
В commit message он назван end-to-end; по исходнику это coordinator test с
mock SessionAgent, а не проверка конкурентного dispatcher.

**Рекомендация:** явно отделить queued/refused/completed outcome до retry
classification. Возвращать из execution boundary admission status и связанные
с ним result/error/evidence; синхронизированный ID не заменяет этот статус.
Для queued outcome игнорировать evidence независимо от момента callback.
Сохранить нынешние per-attempt captures и mutex. Acceptance: барьеры
enqueue → callback/terminal write → return/seal, terminal 400 и настоящий
transient failure; проверить отсутствие второго запроса для queued outcome
и сохранение разрешённого recovery реально исполненной попытки.

## Подтверждённые открытые findings прежних раундов

### R2-2 / F3. P2 — whitespace-only фрагмент теряется при continuation

**Коммиты:** `57fffb05`, `babd295c`, `54acbf71`; join исправлен `8aabba4d`.
**Места:** `internal/app/app_run_terminal.go:142,180`,
`app_run_reviewer.go:389`; `internal/agent/coordinator_run.go:700,829`.

Сборщик отбрасывает error fragment при `TrimSpace(text)==""`, хотя join уже
byte-exact. Цепочка из `{"text":"a`, затем одного пробела вместе с непустым
reasoning, затем `b"}` возвращает `{"text":"ab"}` вместо `{"text":"a b"}`.
Reasoning разрешает второй continuation, поэтому это достижимая цепочка при
двух retries. Оба JSON валидны. **Рекомендация:** сохранять whitespace-only
текст связанной цепочки; acceptance должен сравнивать байты и значение JSON.
Исправленные разрывы внутри слова и переходы через tool steps не переоткрываются.

### R2-4. P2 — отмена и terminal cache пересекают границу reviewer-фазы

**Коммиты:** `ccb510ff`, частичное исправление context в `babd295c`.
**Места:** `internal/app/app_run_reviewer.go:209,356,366,394,598`;
`app_run.go:513`.

После committed primary success parent cancel может выбрать cancellation probe,
сохранить primary в `cachedTerminal`, а `finish` подавит cancellation как уже
завершённый успех. Его defer отменит cached cleanup context. Gate всё же запускает
reviewer, reset не очищает cache/context/cancel. Следующий finish способен принять
primary за terminal reviewer и читать usage отменённым context. Иной select order
даёт cancellation: это ошибка lifecycle, не гарантированный исход каждого cancel.
**Рекомендация:** gate по cancellation и полный сброс/идентичность phase state;
проверить committed-success → cancel → reviewer boundary с барьерами.

### R2-5. P2 — сетевые credentials раскрываются через диагностику

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:79,91,95,103`;
`resolver.go:228,233,237,241,249`;
`internal/agent/coordinator_providers_network.go:22`, `agent_turn_failure.go:231`.

Parse/unsupported-scheme errors включают исходный proxy URL; DoH errors включают
полный endpoint и вложенную ошибку. Userinfo/query values могут сохраниться при
оборачивании и попасть в persisted finish details/transcript. Header redaction
этот путь данных не меняет. Доказательство — прямые string interpolation и
передача error text; реальные секреты не использовались.
**Рекомендация:** безопасное представление URL и очистка вложенных ошибок;
проверять все диагностические выходы с фиктивными маркерами секретов.

### R2-6. P2 — concurrency oracle cache-test допускает ложное падение

**Коммит:** `fd18cf0c`.
**Места:** `internal/nettransport/lifecycle_test.go:662,663,698,701`;
`transport_cache.go:13,70`; `.github/workflows/build.yml:190`.

Parallel test требует один server accept после двух warm-up requests к общему
cache. Восемь distinct insertions других parallel tests могут законно вытеснить
transport между ними, закрыть idle connection и вызвать второй accept. CI `-p 2`
не устанавливает упомянутый в комментарии `-parallel 1`. Это конфликт assertion
с допустимым расписанием, **не наблюдавшийся здесь runtime flake**.
**Рекомендация:** локальный cache для oracle либо корректная сериализация;
concurrent eviction проверять отдельными управляемыми барьерами.

### F6. P2 — provider и global network defaults из разных поколений config

**Коммит:** `eb59df12`.
**Места:** `internal/agent/coordinator_models.go:780`,
`coordinator_providers.go:729`, `coordinator_providers_network.go:17`.

Provider строится из pinned cfg A, helper заново читает `c.cfg.Config()`.
Reload A→B между чтениями даёт provider A с network B; atomic publication
не объединяет эти операции в один snapshot. Это stale-policy дефект, не
доказанная memory race. **Рекомендация:** передавать pinned network defaults
в builder вместе с provider. Обход — изменять сеть через restart, без hot reload.

### F7. P2 — durable replay теряет IdleTimeout

**Коммит:** `f7362f7f`.
**Места:** `internal/agent/call_options.go:69,263`,
`call_data_conversion.go:90,126`; `internal/session/session_runqueue.go:138`.

У live options поле есть, у durable spec и обоих converters — нет. После
mirror/JSON/rebuild положительное значение, например 5s, становится 0:
используется shared/default watchdog и исчезает terminal idle policy.
Теряется и положительный disabled sentinel CLI. **Рекомендация:** совместимое
durable поле и оба преобразования; round-trip unset/positive/disabled.
Доказательство — исчерпывающие списки полей и initializer, без ожидания watchdog.

### F8. P2 — итог reviewer теряет primary tool inventory

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:118,135,382,467,598`;
`app_run_terminal.go:54`.

Reset обнуляет counts, новая baseline исключает primary rows. Primary с одним
`view` и reviewer без tools дают пустой итоговый `tool_calls`, хотя стоимость
и длительность охватывают обе фазы. Primary-only delegation также исчезает
из условия reduction warning. **Рекомендация:** invocation accounting отдельно
от выбора terminal фазы, объединение tool IDs без дублей. До исправления
inventory можно получать из полного transcript.

### F9. P2 — JSON null вызывает исключение web formatter

**Коммит:** `14dd4a54` расширяет прежний ActionRow-дефект на sub-agent render.
**Места:** `web/src/toolFormat.ts:12,13,27`,
`web/src/components/SubAgentBlock.tsx:43`;
`internal/agent/agent_prompt.go:555`, `sanitizeToolInput`.

`JSON.parse("null")` успешно возвращает null. Последующие `parsed[k]` и
`Object.values(parsed)` выбрасывают TypeError вне catch; TypeScript cast
runtime guard не создаёт. Backend `json.Valid` допускает null. Доказательство
по коду; исторические Node-прогоны не считаются новым исполнением этого аудита.
**Рекомендация:** non-null object guard и безопасное представление иных JSON-типов;
проверить сохранение остального transcript при таком input до web alpha.

### R3-2. P2 — custom HTTP proxy без порта не соединяется

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:76`, `proxy.go:96,101`,
`client.go:87,95`.

Validation принимает HTTP proxy URL без порта. Proxy-only путь stdlib добавляет
80; с custom resolver `connectDialer` передаёт `pu.Host` прямо в TCP dial и
получает `missing port in address`. Нормализация SOCKS не меняет эту ветвь.
**Рекомендация:** Hostname/Port + default 80 + JoinHostPort, включая IPv6.
Обход — явный порт. Это адресация, не доказанная credential leak.

### R4-1. P2 — custom CONNECT response head не ограничен по размеру

**Коммиты:** `0f445020`, `eb59df12`; `fd18cf0c` добавляет срок, не byte bound.
**Места:** `internal/nettransport/proxy.go:120,121`; `client.go:87`.

`http.ReadResponse(bufio.NewReader(conn), ...)` получает неограниченный reader.
В Go 1.26.3 `net/http/response.go:161,188` и `net/textproto/reader.go:508`
нет практически полезного ограничения status/header section; stdlib CONNECT
использует LimitedReader (`net/http/transport.go:1907`). Аллокации O(H) зависят
от объёма заголовков H; 30s deadline не является memory cap. Условие воздействия —
некорректный либо недоверенный настроенный proxy. OOM и эксплуатация не проверялись.
**Рекомендация:** byte bound всего head, закрытие connection на превышении,
сохранение уже buffered tunnel payload при успехе.

### R4-2. P2 — custom resolver не сохраняет резервные IP

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/resolver.go:20,72,129,191,266`;
`client.go:116,124,134`.

ResolveFunc возвращает ровно один IP; первый пригодный A исключает другие A
и AAAA fallback. Затем делается единственный dial. При недоступном первом
и доступном втором адресе каждый retry со стабильным DNS-порядком снова
проваливается. Доказательство — тип результата, ранние return и отсутствие
цикла fallback. **Рекомендация:** список адресов и ограниченный общий dial budget
для fallback обеих families, сохраняя proxy и исходный TLS hostname.

### R5-1. P2 — 401 rebuild меняет временную reviewer-модель

**Коммит новой поверхности:** `ccb510ff`; rebuild path существовал раньше.
**Места:** `internal/app/app_run_reviewer.go:655,682,685`;
`internal/agent/coordinator_run.go:334,340,381`;
`credentials.go:339`, `coordinator_models.go:94,106`, `coordinator_providers.go:912`.

Reviewer R задан неперсистентным override. После 401 и успешного refresh rebuild
вызывает обычный session resolver с nil credentials, выбирающий smart S.
Получается S → R/401 → refresh → S; ModelRole=reviewer сохраняет tool policy,
но не identity модели. При статическом ключе без refresh этот путь не срабатывает.
**Рекомендация:** при refresh сохранять per-call model/effort и обновлять только
credentials соответствующего provider; не записывать R вместо durable smart S.
Acceptance: S → R/401 → R при неизменном session smart slot.

### R5-2. P2 — OAuth refresh не наследует provider network policy

**Коммит интеграции:** `eb59df12`; auth helpers старше недели.
**Места:** `internal/agent/coordinator_providers_network.go:17`,
`coordinator_providers.go:832,951`; `internal/config/store_oauth.go:207`;
`internal/oauth/copilot/oauth.go:157,168,206`, `internal/oauth/hyper/device.go:151,170`.

Inference получает настроенный transport; ConfigStore refresh вызывает helpers
с `http.Client{Timeout:30s}` без Transport. Если auth endpoint доступен только
через Rush proxy, inference с действующим token работает, а refresh по тому же
маршруту пройти не может. Доступный direct/default route используется независимо
от per-provider policy. Отправка token неизвестному адресату не доказана.
**Рекомендация:** согласованный network client для auth lifecycle с собственным
timeout/cancel. Обход — отдельный рабочий default auth route либо иной auth mode.

### R5-3. P2 — terse stdout содержит primary и reviewer вместе

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:285,293,598`,
`app_run.go:487,495,513`; контракт `app_run_request.go:18` и `internal/cmd/run.go:70`.

Каждый finished message сразу печатается. Primary уже записан до reviewer gate;
reset не отменяет stdout. Для текстов `PRIMARY` и `REVIEW` получается
`PRIMARYREVIEW\n`, хотя выбранным результатом обещан reviewer. Final reconciliation
повторяет тот же handler, поэтому пример не зависит от доставки live event.
**Рекомендация:** выбирать единственный final result до terse publication.
Обход для машинного потребителя — `--json`; stream имеет отдельный контракт.

### R6-1. P2 — initial TCP dial HTTP proxy/DoH не имеет собственного срока

**Коммиты:** `0f445020`, `eb59df12`; defaults частично восстановлены `fd18cf0c`.
**Места:** `internal/nettransport/client.go:80,95,98`, `resolver.go:163,187,190`;
Go 1.26.3 `net/http/transport.go:1303,1320,1390,1529`.

HTTP proxy-only и direct/HTTP-proxy DoH transports оставляют DialContext/Dial nil.
Stdlib использует zeroDialer без Timeout и отделяет dial context от request cancel.
После начала dial возврат HTTP waiter по cancel/DoH Client.Timeout не обязан
завершить TCP connect. TLS/CONNECT/idle timeouts действуют на следующих стадиях.
При неотвечающем маршруте ресурсы остаются до срока ОС либо закрытия transport;
вечная утечка на любой ОС не заявляется. **Рекомендация:** bounded TCP DialContext
в обоих конструкторах; acceptance должен проверять завершение самого dial.

### R6-2. P2 — extension при нулевом hard cap становится абсолютным deadline

**Коммит новой поверхности:** `f7362f7f`; ошибочный алгоритм старше недели
(`9d5b4e0a7`, 2026-05-17), здесь проверяется его взаимодействие с новым IdleTimeout.
**Места:** `internal/agent/stream_watchdog.go:213,215,431,443,446`,
`agent_turn.go:461`, `coordinator_run.go:795,855`; `internal/cmd/run.go:578,835`.

При cap=0 `hardDeadline=start+idle`. Extension вычисляет новый срок по activity,
но безусловно ограничивает его тем же hardDeadline. Поэтому при idle=20s,
extension=true, cap=0 и progress каждую секунду первое срабатывание после 20s
считает здоровый stream stalled. Положительный per-call IdleTimeout делает его
terminal. Default extension=false этим примером не затронут.
**Рекомендация:** применять абсолютный cap только при cap>0; проверить continuous
progress дольше idle, остановившийся progress и положительный cap. Обход —
не включать extension flag. Existing extension tests используют положительный cap.

## Проверенные области без дополнительных release findings

- **Locks/re-entrancy:** новый evidence mutex защищает только sealed/msgID;
  под ним нет DB, mailbox, provider calls или ожиданий каналов. Callback вызывается
  после отпускания `turnStream.mu`. Цикл lock ordering этим diff не создаётся.
  R7-1 — ошибка границы admission, не mutex deadlock.
- **Ownership:** one-shot reservation, epoch guards, fail-fast mailbox branch
  сохранены. Queue-drain исполняет runTurn в существующем dispatcher, без
  рекурсивного захвата того же OS session lock. Работа с lock release остаётся
  вне mailbox critical section в соответствующей release-фазе.
- **Сетевой cleanup:** `handshakeGuard.stop` joins watcher и снимает deadline
  до handoff; CONNECT error paths закрывают connection; bufferedConn сохраняет
  прочитанные сверх head байты. SOCKS бюджет охватывает TCP/greeting/auth/CONNECT,
  DoH body закрывается. Эти выводы не закрывают отдельный initial-dial R6-1.
- **Cache:** eviction вызывает CloseIdleConnections после unlock. Concurrent
  misses могут создать лишний transport, но его idle lifetime ограничен 90s;
  из этого не следует прежний бесконечный F5. Полный shutdown всех SDK pools
  данным аудитом не сертифицируется.
- **Channels/cancel:** done/drainDone имеют capacity 1, закрытая subscription
  переводится в nil, retry backoff выбирает также ctx.Done. Runner panic
  преобразуется в terminal response. Безусловного нового ожидания канала без
  отправителя в просмотренных изменениях не найдено; phase lifecycle имеет R2-4.
- **Credentials/reviewer:** configured pass исключён для credentialed requests;
  FolderScope, DiskProvider и fail-fast переносятся, context-carried allowlist
  наследуется. Sharing одинакового HTTP transport само по себе не смешивает
  request authorization. Write/bash reviewer — явная текущая tool policy,
  не самостоятельный security finding.
- **Бюджеты:** MaxCost/MaxTokens проверяются по total session usage, поэтому их
  копирование в reviewer не создаёт второй независимый денежный/token budget.
  Общий timeout остаётся в context; отдельный «двойной бюджет reviewer» не доказан.
- **Peak-hours message:** прослежены config → error/guidance, оба WS converters,
  frontend payloads и сохранение заметки при CLI time-only update. Поле содержит
  operator-authored текст; исполнения команды из него в этом diff нет.
  Label/id связаны; новых независимых release defects здесь не установлено.
- **MCP/processes:** недельные изменения относятся к tests: env из TestMain
  перенесён в serial t.Setenv, directory identity проверяется os.SameFile,
  wait helpers имеют конечный timer и Stop. Production process cleanup этой
  серией не меняется; увеличение timeout теста не доказывает отсутствие flakes.
- **Сложность/I/O:** own-ID lookup всё ещё вызывает Messages.List; два classifiers
  могут повторять O(N+S) чтение/декодирование всей истории, где S — её объём.
  Reviewer baseline строится повторно. Это лишняя работа, но без измерений
  не объявляется release-critical slowdown. Новый capture добавляет O(1) память
  на попытку; удержание объектом queued call соответствует времени жизни очереди.
- **Web/performance:** metadata собирается в имеющемся O(messages+parts) проходе,
  timestamp не добавляет таймер на каждое сообщение. Нового подтверждённого
  квадратичного алгоритма, subscription leak или значимого allocation regression
  в этом diff нет. Ошибка null отдельно учтена как F9.

## Ограничения и контроль результата

Не запускались build, Go tests, `-race`, lint/vet, benchmarks, широкие suites,
нагрузка, fault injection, реальные LLM/OAuth/MCP/proxy запросы и browser E2E.
Изменяется документация; описанные tests прочитаны для оценки покрытия.
Исторические успешные прогоны из commit messages не являются текущим green gate.
Ни одного runtime test failure/flake в этом проходе не наблюдалось и не заявляется.

Проверка выборочная: все возможные interleavings, SDK credential chains,
платформенные process cleanup реализации, DNS protocol conformance и
crash-consistency не покрыты. Для R7-1 дан разрешённый source-level interleaving,
но его частота и wall-clock проявление не измерялись. Предположения о повторном
исполнении произвольных tools или утечке данных постороннему получателю
не выдаются за доказанные последствия.

Выполнен `git diff --check ccc2f9b5 4b2b2a0c`. Перед staging проверен полный
status: index пуст, кроме отчёта видны перечисленные чужие правки. Проверка diff
и точного file list ограничивает commit отчётом. Из-за обнаруженной параллельной
работы нельзя заявлять, что полный worktree содержит только отчёт и `.gitkeep`;
никаких чужих файлов ради такого состояния не удалялось и не восстанавливалось.
Граница разрешённого commit — **только**
`docs/reviews/2026-09-22-weekly-commit-audit-round7.md`, с отдельной проверкой
фактического file list commit. Частные абсолютные пути компьютера в отчёт
не включены.

## Минимальные обязательные исправления и финальный release verdict

Для **общей alpha с сохранением заявленных возможностей** минимальный gate:

1. **R7-1:** завершать queued/refused admission до classifiers и проверить
   ранний callback до seal. Не откатывать исправленные own-ID и synchronization
   гарантии `4b2b2a0c`.
2. **R2-2:** сохранять все текстовые fragments результата, включая whitespace-only.
3. **R2-4 + R5-1 + R5-3:** отмена/terminal state на границе reviewer, сохранение
   reviewer identity после refresh и единственный итог в terse stdout.
4. **R2-5 + R4-1 + R6-1:** redaction диагностики, byte bound CONNECT head,
   bounded initial TCP dial. **R5-2** обязателен до обещания работы OAuth lifecycle
   исключительно через настроенную provider network policy.
5. **F9:** безопасный formatter перед web alpha.
6. **R6-2:** правильная extension semantics при cap=0 либо явное исключение
   этого сочетания options из поддерживаемого alpha-профиля.

**R2-6** нужно исправить до использования parallel cache-test как надёжного
release gate. Прогоны должны быть целевыми; для конкурентных переходов —
управляемые барьеры и отдельный `-race`, без надежды на случайный serial pass.

Ограниченный профиль может отложить **F6/F7/F8/R3-2/R4-2** только с явными
условиями: смена сети через restart, отсутствие гарантии durable idle round-trip,
tool inventory из transcript, обязательный proxy port, отсутствие обещания
address failover custom resolver. Отключение reviewer исключает его фазовые
дефекты; `--json` обходит R5-3, fail-fast admission обходит R7-1, extension=false
обходит R6-2. Отключение web/custom network исключает соответствующие ветви.
Такие ограничения в рамках аудита **не вводились и согласованными не считаются**;
они не закрывают R2-2 в оставшемся continuation path и не заменяют acceptance.

**Финальный verdict для `4b2b2a0c` / `0.2.0-alpha.2`: NO-GO общей alpha.**
Основание — перечисленные подтверждённые P2 и отсутствие принятых ограничений
поддерживаемых режимов, а не повтор уже снятого P1. Documentation commit
фиксирует аудит; он не является разрешением на выпуск.
