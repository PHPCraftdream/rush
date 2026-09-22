# Rush: независимый release-readiness аудит, round 4

Дата: **2026-09-22**. Проверенный HEAD:
`c32c72d6d22514c3ceef953699537812b024874c`.

## Summary

**Release verdict: NO-GO для общей alpha с текущим набором возможностей.**

Последние исправления устраняют прежние контрпримеры, но не полностью закрывают
соответствующие механизмы:

- `8aabba4d` закрывает контрпримеры round 3 с разрывом внутри слова, JSON-строки
  и числа: разделитель больше не вставляется. Однако сборщик всё ещё отбрасывает
  целиком whitespace-only промежуточный фрагмент. Остаток R2-2 теперь оценён
  как **P2**, поскольку требует более узкой последовательности попыток.
- `2169dbe3` закрывает конкретный обход `ErrSessionBusy` из R3-1, а также
  повтор по неизменившейся старой строке. Проверка `ID != baseline` всё ещё не
  устанавливает владельца строки: успешный вызов может продолжить более новый
  stalled response другого вызова. Остаток R3-1 сохраняет **P1**.
- Обнаружены **два новых P2**: отсутствие ограничения размера CONNECT-response
  в custom dialer и потеря резервных адресов при custom DNS/DoH.

Всего в этом срезе **один P1 и одиннадцать P2**, без повторного подсчёта F3
как отдельного от R2-2 finding. P0 не установлен. Отсутствие найденного нового
циклического mutex/channel deadlock не доказывает отсутствия deadlocks или
гонок памяти во всём Rush. R3-1, R2-4 и F6 ниже — подтверждённые ошибки
принадлежности/жизненного цикла состояния, а не результаты запуска `-race`.

| ID | Severity на HEAD | Состояние |
| --- | --- | --- |
| R3-1 | P1 | Частично исправлен; более новая чужая строка всё ещё разрешает retry |
| R2-2 / F3 | P2, ранее P1 | Частично исправлен; теряется whitespace-only фрагмент |
| R2-4 | P2 | Открыт: отмена между фазами и stale terminal cache |
| R2-5 | P2 | Открыт: credentials в диагностических ошибках сети |
| R2-6 | P2 | Открыт: недетерминированный oracle cache-eviction test |
| F6 | P2 | Открыт: смешение provider/network config snapshots |
| F7 | P2 | Открыт: потеря `IdleTimeout` при durable round-trip |
| F8 | P2 | Открыт: неполные tool counts итогового reviewer-envelope |
| F9 | P2 | Открыт: исключение web formatter на JSON `null` |
| R3-2 | P2 | Открыт: HTTP proxy без порта ломается в combined mode |
| R4-1 | P2 | Новый: custom CONNECT читает response head без ограничения размера |
| R4-2 | P2 | Новый: custom resolver лишает соединение fallback на другие IP |

P1 здесь означает существенное нарушение контракта исполнения, которое нельзя
оставлять в общей alpha. P2 — ограниченный сценарий или дефект надёжности
проверки; выпуск затронутой возможности всё равно требует исправления либо
явного ограничения профиля. Обязательный минимальный gate приведён в конце.

## Срез, сохранность и метод

- Период: **2026-09-15 00:00:00 +02:00 — HEAD от
  2026-09-22 06:58:44 +02:00**, `Europe/Berlin`, по committer date среди предков
  HEAD. Собственный documentation commit этого отчёта в срез не входит.
- База перед периодом: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  2026-09-11 14:08:09 +02:00. Первый коммит периода — от 19 сентября;
  изменение нижней границы прежних отчётов с 14 на 15 сентября ранний набор
  коммитов не меняет. `--since` и `--since-as-filter` дают **28 коммитов**.
- Diff `ccc2f9b5..c32c72d6`: **64 файла, 9367 добавленных / 651 удалённая
  строка**, включая тесты и документацию. Отдельно просмотрен diff после
  проверенного в round 3 `54acbf71`.
- Фактически исходное окружение указывало на `main`, а зарегистрированный
  worktree round 3 содержал другую ревизию. Поэтому создан отдельный detached
  worktree от точного проверяемого HEAD. Его исходные index и status пусты.
  Основная рабочая копия не использовалась для записи отчёта или его commit;
  исходный пользовательский ` D web/dist/.gitkeep` сохранён.
- Единственное изменение содержимого — этот Markdown. Исходники, тесты,
  CHANGELOG, версии и конфигурация не редактировались. Sub-agents, merge и push
  не выполнялись. В отчёте используются только пути относительно репозитория
  либо пути внутри явно названной зависимости/toolchain.
- Прочитаны история/состав недельной серии, production diff, актуальные
  реализации и вызывающий код, выбранные regression tests, все три предыдущих
  отчёта. Проверены необходимые участки локальных Go **1.26.3** и
  `golang.org/x/net` **v0.55.0**. Номера строк ниже относятся к `c32c72d6`.
- Основное доказательство — статическая трассировка конкретных ветвей и
  допустимых последовательностей событий. Сценарии Go ниже **не исполнялись**.
  Исключение formatter F9 повторно воспроизведено на фактическом TypeScript
  через Node **v24.12.0**, без записи файлов и установки зависимостей.

Предыдущие отчёты:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md).
Их выводы и сообщения коммитов проверялись как утверждения, а не принимались
за независимое runtime-подтверждение.

### Карта проверенной серии

| Коммиты | Проверенная область |
| --- | --- |
| `57fffb05` | Partial-output continuation, retry classifiers и принадлежность history |
| `14dd4a54`, `924add8a`, `10e32c56` | Tool input formatter, sub-agent metadata, timestamps |
| `87ee064f`, `64409ab3` | Изоляция env и ожидания в Windows MCP-тестах |
| `ccb510ff` | Auto-reviewer, повторный phase lifecycle, ownership и итоговый envelope |
| `0f445020`, `eb59df12` | NetworkConfig, proxy/DNS/DoH, provider clients, snapshot и wrappers |
| `180f1544`, `5c20c771` | Context-aware HTTP calls в test doubles |
| `f7362f7f` | CLI timeout defaults, idle override, watchdog и durable options |
| `97c22050` | Playwright-тесты timestamps/sub-agent metadata |
| `babd295c`, `fd18cf0c` | Reviewer isolation, первая сборка continuation; network deadlines/cache |
| `d0966df9`, `54acbf71` | SOCKS5 deadline, continuation через tools, reviewer fail-fast |
| `c9eb9a98`, `8aabba4d`, `2169dbe3`, `c32c72d6` | Tagged switches, byte join, retry baseline/admission guards и тесты |
| `df0051b1`, `85ee8836`, `bf9f6d3d`, `dd03ecf0` | Checkpoints; production-изменений нет |
| `e547e12e`, `31e90d2e`, `295cc386` | Предыдущие audit reports |

## Перепроверка findings предыдущих раундов

«Закрыт» означает устранение описанного механизма по коду, а не независимо
пройденный acceptance run.

| Прежний finding | Статус и основание на HEAD |
| --- | --- |
| F1, P1 | **Закрыт `babd295c`.** `app_run_reviewer.go:599`, `resetForReviewerPass`, присваивает новый `s.ctx`; `buildReviewerPassTurn:681–685` очищает persistence/reservation и вызывает runner с новым context. Старый token/toolset механизм не переносится на HEAD. |
| F2, P1 | **Закрыт `babd295c`.** `app_run.go:513` требует `req.Credentials == nil`. Credentialed вызовы полностью исключены из configured auto-review; это не реализация reviewer через tenant credentials. |
| F3, P1 | Исходная потеря всего префикса исправлена; оставшаяся точность результата учтена **только как R2-2**, без отдельного счёта. |
| F4, P1 | **Закрыт для описанных неограниченных handshakes** `fd18cf0c` + `d0966df9`. CONNECT/DNS guard, DoH client timeout, TLS timeout и собственный SOCKS5 deadline присутствуют. Новый R4-1 касается размера данных, не повторяет старое ожидание без срока. |
| F5, P1 | **Прежний механизм закрыт `fd18cf0c`.** Main/DoH idle pools имеют 90s timeout; cache ёмкости 8 переиспользует transports; eviction закрывает оба pool вне mutex, wrappers передают cleanup. Полное синхронное освобождение всех App-owned ресурсов этим не доказано. |
| F6, P2 | Подтверждён: pinned provider и live `Options.Network` читаются раздельно. |
| F7, P2 | Подтверждён: `IdleTimeout` отсутствует в durable spec и обоих converters. |
| F8, P2 | Подтверждён: reviewer baseline/count reset исключает primary inventory. |
| F9, P2 | Подтверждён чтением кода и повторным исполнением formatter. |
| R2-1, P1 | **Закрыт `d0966df9`.** `proxy.go:86` задаёт `WithTimeout` всему SOCKS dial; `resolver.go:180` использует тот же dialer для DoH. |
| R2-2, P1 | Старые tool-step и `hel` + `lo` контрпримеры закрыты `54acbf71` + `8aabba4d`; whitespace-only остаток ниже, **P2 на HEAD**. |
| R2-3, P1 | **Конкретная потеря флага закрыта `54acbf71`.** `app_run_reviewer.go:677` копирует `FailIfSessionBusy`; `mailbox_ownership.go:69` атомарно отказывает без enqueue. |
| R2-4–R2-6, P2 | Подтверждены: cache/reset, диагностические URLs и parallel-test oracle не исправлены. |
| R3-1, P1 | **Частично закрыт `2169dbe3`.** Busy/shutdown/OS-lock refusals теперь отсекаются до чтения history; равный baseline ID тоже отсекается. Более новая чужая строка остаётся контрпримером. |
| R3-2, P2 | Подтверждён: default port добавлен только SOCKS5, custom HTTP CONNECT по-прежнему использует `pu.Host` напрямую. |

Полностью ложных исторических findings не установлено. Опровергается перенос
закрытых F1/F2/F4/F5/R2-1/R2-3 и прежних конкретных контрпримеров R2-2/R3-1
на HEAD. Одновременно опровергается более широкое утверждение комментариев
`2169dbe3`, что отличающийся ID сам по себе доказывает принадлежность attempt.

### R3-1. P1 — новый ID не доказывает принадлежность retry evidence

**Коммиты:** исходный continuation `57fffb05`; неполное исправление `2169dbe3`.
**Места:** `internal/agent/coordinator_run.go:420`, `:478–523`,
`lastAssistantMessage:703`, `shouldContinueTurn:812–849`,
`shouldRetryTurn:747–785`. Release/переход dispatcher:
`internal/agent/agent_run.go`, `runOwned`, особенно `:547–558`;
`internal/agent/mailbox_ownership.go`, `drainOrReleaseFinal`.

Текущий guard корректно отвергает `ErrSessionBusy`, `ErrAgentShuttingDown`
и `SessionLockBusyError`. Но после принятого вызова retry loop всё равно читает
последнее assistant message **всей сессии**. Единственная проверка его связи
с attempt — неравенство ID значению, прочитанному до `Run`. Ни возвращённый
успешный `result`, ни `LogicalCallID`, ни конкретный terminal ID не участвуют
в classifier. В stalled branch `true` возвращается до проверки `err == nil`.

**Доказательство: допустимая последовательность, не runtime-прогон.**

1. A захватывает baseline `m0`, успешно выполняет свой turn, сохраняет `mA`
   с `end_turn` и освобождает mailbox/OS ownership. Его `run()` возвращает
   ненулевой result и `nil` error. Приостановить A до retry classification.
2. B законно занимает ту же сессию уже после release A, создаёт новую строку
   `mB`, пишет partial text и завершает её `Stream stalled`. У B можно задать
   positive `IdleTimeout`, чтобы его собственный coordinator не повторял turn.
3. A продолжает с `IdleTimeout == 0` и обычным ненулевым retry budget.
   `turnAttemptRefused(nil) == false`; `mB.ID != m0.ID`; строка имеет error
   finish и progress. `shouldContinueTurn` на строке 843 возвращает true,
   хотя A успешно закончил свою работу.
4. Prompt A заменяется на continuation с текстом B; после backoff и release B
   A запускает дополнительный turn. Даже `FailIfSessionBusy=true` не помогает,
   если к моменту нового admission сессия уже свободна.

Все переходы разрешены текущими ownership guards; одновременный provider turn
под одним lock для этого не требуется. Это не утверждение о межпользовательском
обходе авторизации session ID: нарушена принадлежность результата двух законных
вызовов одной сессии. Дополнительный turn может исполнять tools.

Остаётся и неохваченная ветвь queued admission: `agent_run.go:91` возвращает
`(nil, nil)`, а классификация queued в `app_run_turn.go:68–93` выполняется лишь
**после** возврата coordinator. Guard по error не может остановить этот исход;
более новая чужая stalled row также удовлетворяет classifier. Его нельзя
считать закрытым проверкой только `ErrSessionBusy`.

**Покрытие:** новые `TestRetryClassifiers_AttemptScopedEvidence` и
`TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage`
проверяют refusals, неизменившийся baseline и собственный stall. Они не проверяют
чужую строку, созданную после baseline, на фоне собственного успешного/queued
исхода. Прочитаны, не запускались.

**Рекомендация:** возвращать явный admission/attempt outcome и immutable
идентичность собственных terminal messages; не восстанавливать их владельца
из session-wide «последней строки». Refused/queued/успешный собственный исход
не должен запускать recovery по чужой history. Acceptance с барьерами должен
покрыть указанную смену владельца, queued path и настоящий собственный stall,
проверяя число provider calls и сохранность исходного prompt/result.

### R2-2 / F3. P2 — сборщик теряет whitespace-only промежуточный фрагмент

**Коммиты:** `babd295c`, фильтр в актуальной форме — `54acbf71`;
`8aabba4d` исправляет join, но не фильтр.
**Места:** `internal/app/app_run_terminal.go:142`, `continuationChainText`,
`:180`, `joinContinuationText`; потребитель `app_run_reviewer.go:389`.
Достижимость recovery: `internal/agent/coordinator_run.go:673`,
`turnMadeProgress`, и `continuationPrompt:538`.

`joinContinuationText` теперь правильно возвращает `acc + next`. Однако перед
ним сборщик добавляет error-фрагмент только при `strings.TrimSpace(text) != ""`.
Пробел может быть содержимым JSON-string; исключать весь такой message нельзя.

**Статический контрпример с двумя разрешёнными retries:**

| Сообщение | Содержимое / finish |
| --- | --- |
| Первый assistant attempt | Текст `{"text":"a`, затем transient error |
| User | Continuation prompt |
| Второй assistant attempt | Непустой `ReasoningContent` и `TextContent` из одного пробела, затем transient error |
| User | Continuation prompt без видимого текста |
| Последний assistant attempt | Текст `b"}`, затем `end_turn` |

Вторая попытка имеет progress через reasoning, поэтому coordinator действительно
может перейти в continuation, а не только в blind resend. Оба user boundary
распознаются `IsContinuationPrompt`. `Message.FullText` сохраняет одиночный
пробел, но строка 142 исключает его из `parts`. Ожидаемый byte-exact результат
`{"text":"a b"}` превращается в `{"text":"ab"}`. Оба JSON валидны, поэтому
одной проверки `json.Valid` недостаточно. Сценарий статический, не исполненный.

**Рекомендация:** сохранять каждый ненулевой текстовый фрагмент связанной
цепочки, включая whitespace-only, и проверять точные байты/значение JSON.
Не менять пустые reasoning-only/tool-only messages в искусственные пробелы.
Новые тесты `8aabba4d` проверяют разные разрывы и несколько непустых фрагментов,
но отдельный whitespace-only error fragment не покрывают. Severity снижена
с P1 до P2 за существенно более узкий остаточный сценарий; прежние контрпримеры
round 3 повторно открытыми не объявляются.

## Новые findings

### R4-1. P2 — custom CONNECT-response не ограничен по размеру

**Коммиты:** `0f445020`, подключение к provider clients — `eb59df12`;
добавленный `fd18cf0c` timeout устраняет ожидание без срока, но не этот дефект.
**Место:** `internal/nettransport/proxy.go:120–121`, `connectDialer`;
выбор custom пути — `client.go:88–96` при HTTP proxy + custom DNS/DoH.

**Механизм и доказательство по исходникам.** `bufio.NewReader(conn)` передаётся
в `http.ReadResponse` без счётчика/лимита прочитанных байтов. Размер буфера
`bufio.Reader` не ограничивает суммарный размер собираемой строки и headers.
В локальном Go 1.26.3 `src/net/http/response.go:161,188` вызываются
`textproto.Reader.ReadLine` и `ReadMIMEHeader`; в
`src/net/textproto/reader.go:43,58,507` это неограниченное накопление строки
и лимиты `math.MaxInt64` для headers. Обычная CONNECT-ветвь самого Go Transport,
напротив, ставит `io.LimitedReader` с `maxHeaderResponseSize`
(`src/net/http/transport.go:1907`).

Следовательно, размер response head от настроенного proxy напрямую определяет
аллокации Rush без собственного предела размера. 30s handshake deadline задаёт
срок, но не ограничение памяти; O(H) памяти по объёму заголовков H сохраняется.
Это относится к custom CONNECT, а не ко всем HTTP-ответам провайдера. Условия
воздействия — некорректный либо недоверенный proxy в этой конфигурации.
Фактический OOM, его порог и эксплуатация в аудите не проверялись; дефект
установлен статически как отсутствие ресурсного ограничения на сетевом вводе.

**Рекомендация:** ограничить размер всей status-line + header section при
разборе CONNECT, завершать oversized response ошибкой и закрывать connection.
После успешного handshake лимит не должен обрезать tunnel payload; сохранить
уже buffered bytes. Acceptance после исправления должен отдельно проверять
лимит заголовков, cleanup на отказе и передачу обычного туннеля. Само наличие
deadline или `bufio.NewReaderSize` не закрывает finding.

### R4-2. P2 — custom DNS/DoH отбрасывает резервные IP до соединения

**Коммиты:** `0f445020`, production wiring — `eb59df12`.
**Места:** `internal/nettransport/resolver.go:20`, `resolveFunc`;
`plainDNSResolver:72–80`, `answerIP:129–139`, `newDoHResolver:191–200`,
`dnsOverTCPResolver:266–274`; `internal/nettransport/client.go:113–134`,
`resolvedDialer`.

Resolver возвращает ровно один IP. Plain DNS выбирает первый IPv4, даже когда
есть IPv6; DoH/DNS-over-TCP возвращают первый A и запрашивают AAAA только если
пригодного A вообще не оказалось. `resolvedDialer` передаёт полученный IP в
единственный dial и сразу возвращает его ошибку. На этом уровне уже невозможно
попробовать остальные адреса либо другую address family.

**Доказательный сценарий, не запускался:** DNS-ответ содержит два корректных
адреса A1 и A2; A1 недоступен с текущего маршрута, A2 доступен. При стабильном
порядке ответа каждый provider connection выбирает A1 и завершается ошибкой,
не пытаясь соединиться с A2. Аналогично, при опубликованных A + AAAA и рабочем
только IPv6 маршруте наличие A предотвращает fallback на AAAA. Повтор LLM
request не исправляет это, если DNS возвращает тот же порядок.

Отсутствие списка адресов и цикла после неуспешного dial видно непосредственно
в коде; вывод не зависит от замеров скорости и не объявляет любой custom DNS
неработоспособным. Дефект проявляется при частичной недоступности адресов и
делает переключение resolver настройкой, меняющей доступность провайдера.
Проверенные `resolver_client_test.go` и combined-mode tests используют один
доступный loopback IP и не проверяют это условие.

**Рекомендация:** сохранять набор адресов и выполнять bounded fallback с общим
dial budget, учитывая обе address families. Для proxied режима сохранить
использование того же proxy для каждого выбранного IP и hostname для TLS.
До исправления допустим только явно ограниченный custom-resolver профиль,
где первый возвращаемый адрес гарантированно доступен; общий DNS failover
этой реализацией не обеспечивается.

## Остальные подтверждённые открытые findings

### R2-4. P2 — terminal cache переживает отмену и смену reviewer-фазы

**Коммит:** `ccb510ff`; `babd295c` исправляет передачу context, но не cache.
**Места:** `internal/app/app_run_reviewer.go:209–216`, `:356–367`, `:394–396`,
`resetForReviewerPass:598`; gate `internal/app/app_run.go:513`.

При cancel после commit успешной primary event loop может выбрать `ctx.Done`,
сохранить primary в `cachedTerminal` и вернуть из `finish` успех. При этом
defer отменяет `cachedTerminalCtx`. Gate не проверяет `ctx.Err`; reset не
очищает terminal/cache context/cancel handle. Reviewer получает отменённого
parent, а его `finish` может повторно принять cached primary за свой terminal
и подавить cancel; usage reads используют уже отменённый cleanup context.

**Доказательство:** допустимый порядок «commit primary → parent cancel →
cancellation branch → reviewer done branch» прямо проходит указанные условия.
При ином порядке select возможен cancellation error вместо cached success.
Принудительное воспроизведение не выполнялось.
**Рекомендация:** запрет новой фазы после parent cancel, определённый outcome
уже committed primary, очистка cache/handles и проверка принадлежности terminal
текущей фазе. Acceptance — cancel между commit и done с барьерами.

### R2-5. P2 — секреты из network URLs остаются в диагностике

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:79,91–95,103`, `resolveConfig`;
`resolver.go:228–249`, `dohQuery`; wrapper
`internal/agent/coordinator_providers_network.go:22`.

Ошибки парсинга/unsupported scheme интерполируют исходный URL; ошибки DoH
включают полный endpoint даже при обычном non-200 ответе. Поэтому userinfo и
query credentials сохраняются в error string, а `%w` переносит их выше.
Runtime-ошибка может записываться как `err.Error()` в finish details
(`internal/agent/agent_turn_failure.go:231`). Header redaction это не очищает.

**Доказательство:** прямой data flow URL → форматирование ошибки → wrapping /
finish details. Реальные credentials не читались, внешние запросы не делались.
**Рекомендация:** безопасное представление endpoint без userinfo/секретных
query values, включая вложенные parse/`url.Error`; проверять redaction во всех
диагностических выходах. До исправления не выпускать профиль с секретами
в network URLs как безопасный для записи logs/transcript.

### R2-6. P2 — cache-eviction test имеет ошибочный concurrency oracle

**Коммит:** `fd18cf0c`.
**Места:** `internal/nettransport/lifecycle_test.go:662–706`,
`TestTransportCacheEvictionReleasesIdleConns`; `transport_cache.go:12,70`;
`.github/workflows/build.yml:190`.

Тест вызывает `t.Parallel`, использует общий cache ёмкости 8 и требует один
TCP accept после двух warm-up requests. Восемь distinct config insertions
из других parallel tests между ними законно вытесняют transport и закрывают
idle connection; второй запрос делает ещё один accept и нарушает oracle.
Комментарий о `-parallel 1` не соответствует catch-all CI command, где задан
только `-p 2`. Параллелизм между пакетами не сериализует tests внутри пакета.

**Доказательство:** разрешённое расписание вставок и `require.Equal(t, 1, base)`.
Нового наблюдённого падения/flake нет: тесты не запускались.
**Рекомендация:** отдельный cache или сериализация конкретного oracle;
concurrent eviction проверять отдельными барьерами. Исправить до использования
теста как надёжного release gate; случайный зелёный serial run не доказательство.

### F6. P2 — network policy читается не из pinned provider snapshot

**Коммит:** `eb59df12`.
**Места:** `internal/agent/coordinator_models.go:780`, `buildModelsFromCfg`;
`coordinator_providers.go:729`;
`coordinator_providers_network.go:17–19`, `resolveProviderHTTPClient`.

Provider получен из cfg A, но helper повторно читает live `c.cfg.Config()`.
Reload A→B между этими чтениями собирает provider A с network defaults B;
reload между smart/fast builds может смешать policy пары. Атомарная публикация
immutable snapshots не делает два раздельных чтения единым snapshot.

**Сценарий:** остановить build после захвата A, опубликовать B с другой network
policy, продолжить. Выбор transport определяется B. Это статическая логическая
гонка, не установленная гонка памяти.
**Рекомендация:** передавать pinned global network options до provider builder;
до исправления применять смену network policy только с restart.

### F7. P2 — durable round-trip теряет `IdleTimeout`

**Коммит:** `f7362f7f`.
**Места:** `internal/agent/call_options.go:69,263`;
`call_data_conversion.go:90,126`, оба CallOptions converters;
`internal/session/session_runqueue.go:138`, `CallOptionsSpec`.

Live значение участвует в watchdog, но durable mirror и оба converters поле
не содержат. Преобразование call с `IdleTimeout=5s` в durable data и обратно
детерминированно даёт 0; теряется и CLI disabled sentinel. При replay
включается shared/default policy, а не указанная вызывающим.

**Доказательство:** явные field lists spec и converters; wall-clock тест
не требуется и не запускался.
**Рекомендация:** совместимое поле и оба преобразования; round-trip проверки
unset/positive/disabled. До исправления не обещать сохранение idle policy
при durable handoff/restart.

### F8. P2 — reviewer-envelope считает только инструменты второй фазы

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:118,135,382,467,598–616`;
`internal/app/app_run_terminal.go:53`, `reconcileTerminalMessage`.

Counts и identity baseline сбрасываются на reviewer. Последующее reconciliation
исключает primary rows, а возвращаемый primary result заменяется reviewer
result. Если primary вызвал `view` один раз, а reviewer ответил только текстом,
финальный `tool_calls` пуст. При delegation только в primary исчезает также
reduction warning, поскольку его gate читает сброшенные counts. При этом
cost/duration baseline остаётся на весь invocation.

**Доказательство:** прямой проход указанного двухфазного сценария через reset
и reconciliation, без runtime-запуска.
**Рекомендация:** отделить phase-terminal baseline от invocation accounting,
объединять tool IDs без повторного счёта. До исправления считать inventory
по полному transcript, а не по `tool_calls` reviewer-envelope.

### F9. P2 — JSON `null` выбрасывает исключение web formatter

**Коммит:** `14dd4a54` расширил старый дефект `ActionRow` на sub-agent render.
**Места:** `web/src/toolFormat.ts:12–13`, `formatActionArgs`;
`web/src/components/SubAgentBlock.tsx:43`. Backend
`internal/agent/agent_prompt.go:555` проверяет `json.Valid`, что допускает null.

`JSON.parse("null")` успешен. TypeScript cast не проверяет runtime-тип;
`parsed[k]` либо `Object.values(parsed)` выбрасывает исключение уже вне catch.
Вызов происходит при render; ErrorBoundary в просмотренном `web/src` не найден.

**Выполнено в этом аудите:** чтение фактического formatter, удаление типов
в памяти через `stripTypeScriptTypes`, import через data URL и вызовы:

| Вызов | Результат |
| --- | --- |
| `formatActionArgs("bash", "null")` | `TypeError: Cannot read properties of null (reading 'command')` |
| `formatActionArgs("custom", "null")` | `TypeError: Cannot convert undefined or null to object` |
| bash с `{`, `{}`, `[]`, JSON-string | Пустая строка |
| bash с object argument `command: pwd` | `pwd` |

Браузерный crash E2E не выполнялся; подтверждено исключение реальной функции.
**Рекомендация:** non-null object guard и безопасный preview для иных типов;
render acceptance должен сохранять остальной transcript. Обязательно для web
alpha; CLI-only профиль не затронут.

### R3-2. P2 — HTTP proxy без явного порта не работает с custom resolver

**Коммиты:** `0f445020`, `eb59df12`; `d0966df9` нормализует только SOCKS5.
**Места:** `internal/nettransport/proxy.go:96–103`, `connectDialer`;
`client.go:88–103`, выбор режима; `config.go:75–97`, validation.

Proxy URL без порта принимается validation. Proxy-only HTTP transport добавляет
default port через Go `canonicalAddr`; combined resolver path передаёт
`pu.Host` прямо в `net.Dialer.DialContext`. Authority без `:port` получает
`missing port in address` до успешного CONNECT. IPv6 требует той же нормализации.

**Доказательство:** разные ветви нормализации адреса в Rush и локальном
Go 1.26.3 `src/net/http/transport.go:3033`; сетевой сценарий не запускался.
**Рекомендация:** `Hostname`/`Port` + default HTTP port 80 + `net.JoinHostPort`.
Временное ограничение — явный порт у HTTP proxy в combined mode.

## Проверенные области без дополнительных release findings

- **Mutex/re-entrancy/ABA:** cache держит mutex только на map/clock и закрывает
  victim после unlock. Обычный primary→review переход происходит после возврата
  прежнего coordinator; старый reservation затеняется typed nil, а one-shot
  claim и epoch/state checks остаются. Снятие этих guards для исправления
  R3-1 не требуется. Нового доказанного цикла lock ordering не найдено.
- **Channels/panic/cancellation:** done/drainDone имеют capacity 1;
  `runAgentTurnRecovered` переводит panic runner в terminal response;
  закрытый message channel заменяется nil. Retry backoff слушает `ctx.Done`.
  Это не гарантия bounded completion произвольного provider/tool/writer,
  игнорирующего cancel. R2-4 отдельно ограничивает выводы о переходе фаз.
- **Сетевой lifecycle:** CONNECT error branches закрывают connection;
  `handshakeGuard.stop` joins watcher перед передачей готового tunnel и снимает
  deadline. Buffered bytes сохраняются. SOCKS5 использует собственный 30s
  budget; прочитанный x/net снимает handshake deadline на успешном возврате.
  DoH body закрывается, wire DNS allocation ограничена uint16/64 KiB.
  Эти проверки не заменяют header size bound из R4-1.
- **Credential/scope boundary:** credentialed auto-review отключён; reviewer
  переносит FolderScope/DiskProvider и наследует контекстную allowlist policy.
  Общий transport при одинаковом network config сам по себе не смешивает
  request auth headers. Reviewer с write/bash — текущая явная продуктовая
  политика, а не доказанный самостоятельный дефект.
- **Config/provider wiring:** per-field merge, DoH precedence, zero-config
  nil client и подключение custom HTTP client к provider switch проверены.
  Resolver-only не разыменовывает nil `proxyURL`: проходит более раннюю ветвь.
  TLS hostname verification сохраняет исходное имя запроса, не resolved IP.
  Debug/Copilot wrappers forwarding cleanup присутствует. Сквозная проверка
  OAuth/token refresh и всех vendor SDK не проводилась.
- **MCP/process waits:** недельная серия меняет тесты, а не production
  process-tree cleanup. Env override локализован в нужном serial test;
  directory identity сравнивается через `os.SameFile`; waits имеют конечные
  timers. Увеличение timeout до 15s не является доказательством устранения
  любого CI flake. Новых process experiments не было.
- **CLI idle policy:** positive override доходит до `runTurn`; terminal stall
  policy проверяется обоими classifiers. `--timeout=0` оставляет default 6h
  hard backstop, timer останавливается на возврате. Idle timeout не ограничивает
  любой startup/tool path; это не новый доказанный deadlock.
- **Performance:** metadata вычисляется в существующем линейном web проходе,
  TimeBadge не добавляет interval/subscription. `2169dbe3` добавляет полное
  `Messages.List` перед каждой попыткой, включая первый успешный turn; вместе
  с повторным чтением в classifiers и reviewer baseline это дополнительные
  O(N + S) I/O/аллокации по history и её тексту. Сборка строк зависит от числа
  retries, по умолчанию их два. Без замеров это не объявлено отдельной
  release-critical regression. Ресурсный дефект R4-1 имеет собственное
  доказательство отсутствия лимита размера на входе.

Два одновременных cache miss могут построить разные transports, но 90s idle
timeout сохраняется и у некэшированного результата. Это не повтор F5 с
бесконечным idle lifetime. Внешний `CloseIdleConnections` не доказывает
немедленного закрытия hidden DoH transport; eviction/idle timeout и полное
App-owned shutdown — разные гарантии. Отдельного нового P1 отсюда не следует.

## Ограничения и контроль результата

Это выборочный преимущественно статический аудит, а не сертификация всего Rush.
Не выполнялись Go build/test/vet/lint, `-race`, широкие suites, benchmarks,
искусственная нагрузка, fault injection, Playwright/browser E2E, реальные
LLM/MCP/proxy запросы, remote CI inspection и проверки crash consistency.
Изменяется только документация; зелёный runtime gate этим commit не заявляется.
Предложенные concurrency/Go-сценарии обозначают допустимые механизмы, а не
наблюдавшуюся частоту проблем. Сетевой resource finding не проверялся нагрузкой.

Выполнены read-only log/diff/blame/status, чтение исходников/выбранных tests
и локальных зависимостей, динамическая проверка F9. `git diff --check
ccc2f9b5 HEAD` прошёл. Два существующих web-теста не менялись; их SHA-256
совпадают с предыдущими отчётами:

| Файл | SHA-256 |
| --- | --- |
| `web/tests/message-timestamp-always-visible.spec.ts` | `CE416416F90632CD972DE8F262785E576BC784888F881F9C9811C5BA0952F891` |
| `web/tests/subagent-block-metadata.spec.ts` | `CB72A0EB814462FB64EA7655DBE0B826905FEE79060ED2135ED57E9F22611678` |

Единственный разрешённый artifact/содержимое audit commit:
`docs/reviews/2026-09-22-weekly-commit-audit-round4.md`.
Проверка перед staging: status изолированного worktree содержит только новый
Markdown этого отчёта, index пуст. Основная рабочая копия всё ещё указывает
на `c32c72d6`, её единственное изменение — исходный пользовательский
` D web/dist/.gitkeep`; index пуст. Это удаление не включается в audit commit
и не восстанавливается. Перед commit также проверяются точный staged file list
и `git diff --cached --check`.

## Минимальные обязательные исправления и финальный verdict

Для общей alpha с нынешними execution, reviewer, network и web возможностями
минимальный набор обязательных исправлений:

1. **R3-1:** действительная принадлежность retry evidence текущему attempt;
   успешный, queued и refused outcomes не порождают continuation по чужой history.
2. **R2-2:** сохранить whitespace-only фрагменты логического ответа; acceptance
   проверяет точные данные, а не только синтаксическую валидность JSON.
3. **R2-4:** запрет нового reviewer после parent cancel и полное освобождение/
   сброс phase terminal cache.
4. **R2-5 + R4-1:** redaction network errors и bounded CONNECT-response parsing
   до выпуска соответствующих сетевых режимов.
5. **F9:** null-safe web render до выпуска web alpha.

**R2-6** — отдельный обязательный ремонт release gate до опоры на cache-test
как на надёжное доказательство. После исправлений нужны целевые tests этих
механизмов, управляемые барьеры и целевой `-race` для concurrency paths.
Существующие позитивные tests сохраняются; переписывать их oracle под дефект
или считать единичный serial pass достаточным нельзя.

F6/F7/F8/R3-2/R4-2 допускают отсрочку **только с явными ограничениями**:
network policy меняется с restart; durable replay не обещает сохранение idle
override; inventory читается из полного transcript; HTTP proxy имеет явный
порт; custom DNS/DoH используется только при доступности первого выбранного
адреса. Для полного профиля без этих ограничений нужны и соответствующие
исправления. Здесь ограничения не вводились и согласованными не считаются.

**Финальный verdict: `c32c72d6` — NO-GO для общей alpha.** Старые конкретные
блокеры действительно сняты частично, но принадлежность retry результата
остаётся нарушенной; сохраняются подтверждённые phase/data/security/render
дефекты. Отчёт фиксирует необходимый следующий gate, а не разрешение на выпуск.
