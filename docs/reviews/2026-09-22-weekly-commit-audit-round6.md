# Rush: независимый release-readiness аудит, round 6

Дата: **2026-09-22**. Проверенный HEAD:
`fbc8a17738a990dffdb6e421da28ccf055f29428`.

## Summary

**Release verdict: NO-GO для общей alpha с заявленными возможностями.**

На проверенном HEAD сохраняются **14 P2 round 5**. Его P1 **R3-1 закрыт
частично новым `b0a55aab`**: прежний сценарий с более новой чужой строкой исправлен.
Однако callback с ID не ограничен жизненным циклом отдельной попытки: ID прошлой
попытки повторно используется после queued admission, что позволяет поставить
continuation в очередь дважды. При исполнении queued call сам callback также
пишет переменную одновременно с чтением coordinator без синхронизации.
R3-1 остаётся P1 **по этому новому доказательству**, а не по закрытому сценарию.

Проверка началась на `6e18502f`; во время аудита другие изменения были
закоммичены. Дополнительные пять коммитов до `fbc8a177` включены в итоговый
срез и перепроверку. Версия `0.2.0-alpha.2`/CHANGELOG изменены этими внешними
коммитами, не действиями аудита.

Шестой проход дополнительно установил два P2:

- **R6-1:** в HTTP proxy-only и внутренних HTTP-транспортах DoH отсутствует
  собственный срок начального TCP dial. Отмена HTTP request не обязана завершать
  этот dial; добавленные ранее handshake/idle timeouts его не покрывают.
- **R6-2:** сочетание нового `--idle-timeout` с
  `--timeout-extends-on-progress` и нулевым hard cap завершает даже непрерывно
  отвечающую модель. Старый алгоритм watchdog трактует отсутствие cap как
  абсолютный предел в один idle interval; новая terminal policy исключает retry.

Итого **17 актуальных findings: один P1 и шестнадцать P2**. F3 учтён только
в R2-2. R6-2 — ранее не описанное взаимодействие новой возможности со старым
дефектом, а не утверждение, что алгоритм watchdog впервые написан этой неделей.
Закрытые F1/F2/F4/F5/R2-1/R2-3 в прежнем объёме не переоткрываются.

Главный блокер — **R3-1, lifecycle retry evidence и queued admission**.
Для нового callback установлена гонка памяти Go по двум несинхронизированным
доступам; `-race` в этом аудите не запускался. Нового доказанного циклического
deadlock или безусловной production panic не установлено. Статическая проверка
не доказывает их отсутствия во всём Rush. Код и тесты отчётом не исправляются.

## Срез, метод и сохранность

- Период: **2026-09-15 00:00:00 +02:00 — 2026-09-22 13:28:45 +02:00**,
  `Europe/Berlin`, по committer date среди предков проверенного HEAD.
  Собственный documentation commit round 6 в срез не входит.
- База: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  2026-09-11 14:08:09 +02:00. Первый коммит периода датирован 19 сентября;
  отличие нижней границы первых двух отчётов на один день не меняет набор
  ранних коммитов. `--since` и `--since-as-filter` дают **35 коммитов**.
- Совокупный diff базы с HEAD: **86 файлов, 11204 добавленных и 720 удалённых
  строк**, включая документацию и тесты. Дополнительный diff
  `6e18502f..fbc8a177`: 24 файла, 804 добавленных / 187 удалённых строк.
  Проверены callback/retry fix, optional peak-hours message, его CLI/WS/web
  преобразования и tests, согласованность версии/schema и привязка textarea labels.
  Старые app/nettransport реализации этой последней серией не изменены.
- Изучены недельные log/diff, изменённые production-пути и вызывающий код,
  все пять предыдущих отчётов, выбранные regression tests. Дополнительно
  проверены нижний TCP dial и сочетания watchdog options. Ссылки
  `файл:строка` относятся к **`fbc8a177`**, включая файлы, которые одновременно
  редактируются вне этого аудита.
- Основной метод — статическая трассировка конкретных ветвей, значений
  и разрешённых последовательностей событий. Сценарии ниже **не являются
  выполненными runtime-тестами**. Для сетевой семантики прочитаны локальные
  исходники Go 1.26.3 и x/net v0.55.0; для Vertex auth — fantasy v0.25.2
  и genai v1.57.0.
- Ограничение окружения: Git показал **единственный worktree на `main`**, а не
  отдельную изолированную копию. Уже до работы существовали изменения исходников,
  CHANGELOG, версии, web-файлов и ` D web/dist/.gitkeep`; в ходе аудита появились
  дополнительные параллельные изменения. После их внешнего commit до `a275e323`
  они стали частью итогового проверенного HEAD; до этого за исправления
  не принимались. Новые worktrees/ветки не создавались.
  Изоляция отдельным worktree здесь не заявляется.
- Чтобы не смешивать срезы, исходники читались через `git show <срез>:...`,
  поиск — через `git grep ... <срез>`; изменение HEAD отдельно сверено diff.
  Единственный файл, записанный аудитом,
  — этот Markdown. Пользовательские изменения не восстанавливались, не
  форматировались и не включаются в commit; удаление `.gitkeep` сохраняется.
  Sub-agents, push, изменение конфигурации или зависимостей не выполнялись.

Предыдущие отчёты:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md).
Их выводы проверены как утверждения; названия fix-коммитов не заменяют
доказательства устранения механизма.

### Карта недельной серии

| Коммиты | Проверенная область |
| --- | --- |
| `57fffb05` | Partial-output continuation, retry, ownership результата |
| `14dd4a54`, `924add8a`, `10e32c56` | Formatter, sub-agent metadata, timestamps |
| `87ee064f`, `64409ab3` | MCP Windows: env isolation, directory identity, ожидания тестов |
| `ccb510ff` | Автоматический reviewer, две фазы ExecuteRun, context, вывод и accounting |
| `0f445020`, `eb59df12` | NetworkConfig, proxy/DNS/DoH, provider clients, auth и snapshots |
| `180f1544`, `5c20c771` | Context-aware вызовы test doubles |
| `f7362f7f` | CLI timeout defaults, idle override, watchdog и durable contract |
| `97c22050` | Playwright-покрытие timestamps и sub-agent metadata |
| `babd295c`, `fd18cf0c` | Reviewer isolation, continuation result, network guards/cache/cleanup |
| `d0966df9`, `54acbf71` | SOCKS5 budget, continuation через tools, reviewer fail-fast |
| `c9eb9a98`, `8aabba4d`, `2169dbe3`, `c32c72d6` | Tagged switches, byte join, retry baseline/refusal guards и тесты |
| `df0051b1`, `85ee8836`, `bf9f6d3d`, `dd03ecf0` | Checkpoints; production-код не меняют |
| `e547e12e`, `31e90d2e`, `295cc386`, `a4f99d8b`, `6e18502f` | Предыдущие отчёты |
| `6f134f0a` | Новый checkpoint, production-код не меняет |
| `b0a55aab` | Callback assistant ID, выбор own evidence, новый lifecycle/queue контрпример |
| `4357e157` | Optional peak-hours message: config, agent, CLI, WS, web, tests |
| `a275e323` | Версия alpha.2, CHANGELOG и schema; read-only проверка согласованности |
| `fbc8a177` | Совпадающие htmlFor/id для двух новых textarea, без новой логики состояния |

## Перепроверка findings предыдущих раундов

P1 означает существенное нарушение исполнения, блокирующее общую alpha.
P2 — ограниченный сценарий, требующий исправления либо явно объявленного
ограничения соответствующей возможности. «Закрыт» означает устранение
описанного механизма по исходникам; acceptance в этом проходе не выполнялся.

| ID | Статус на HEAD | Основание перепроверки |
| --- | --- | --- |
| F1 | Закрыт | `babd295c`: `resetForReviewerPass:599` присваивает reviewer context; reserved token очищается в `:683`. Старый smart-toolset/stale-token сценарий устранён. |
| F2 | Закрыт | `app_run.go:513` исключает `req.Credentials != nil` из configured auto-review. |
| F3 | Объединён с R2-2 | Исходная потеря всего префикса закрыта. Whitespace-only остаток посчитан один раз. |
| F4 | Закрыт в прежнем объёме | `fd18cf0c` + `d0966df9`: raw CONNECT/DNS handshake guards, DoH exchange timeout, TLS/SOCKS budgets присутствуют. Новая неполнота начального TCP dial выделена в R6-1. |
| F5 | Закрыт в прежнем объёме | Cache на 8 entries, 90s idle timeout основного/DoH pools, eviction вне mutex, forwarding CloseIdleConnections. Прежнего бесконечного idle lifetime нет. |
| F6 | P2, открыт | Pinned provider соединяется со свежим чтением global network defaults. |
| F7 | P2, открыт | `IdleTimeout` отсутствует в durable spec и обоих converters. |
| F8 | P2, открыт | Reviewer baseline/reset исключает primary tool inventory. |
| F9 | P2, открыт | JSON `null` разыменовывается formatter вне catch. В этом проходе подтверждено по исходнику. |
| R2-1 | Закрыт | `socksDialer:86` ограничивает весь SOCKS dial; DoH-over-SOCKS использует тот же dialer. |
| R2-2 | P2, частично закрыт | Tool-step и mid-word контрпримеры исправлены `54acbf71` + `8aabba4d`; whitespace-only fragment всё ещё исключается. |
| R2-3 | Закрыт | `app_run_reviewer.go:677` сохраняет `FailIfSessionBusy`; `mailbox.submit:69` отказывает без enqueue. |
| R2-4 | P2, открыт | Parent cancel не закрывает gate следующей фазы; terminal cache/handles переживают reset. |
| R2-5 | P2, открыт | Network URL с credentials включается в error strings без redaction. |
| R2-6 | P2, открыт | Parallel cache-test предполагает отсутствие разрешённого конкурентного eviction. |
| R3-1 | P1, частично закрыт | `b0a55aab` исправляет выбор более новой чужой строки. Остались lifecycle captured ID, повтор queued admission и callback race; новое доказательство ниже. |
| R3-2 | P2, открыт | Custom HTTP CONNECT передаёт `pu.Host` без default port. |
| R4-1 | P2, открыт | Custom CONNECT response head не имеет byte budget. |
| R4-2 | P2, открыт | Resolver возвращает один IP; address fallback отсутствует. |
| R5-1 | P2, открыт | 401 rebuild повторно выбирает session smart, теряя временную reviewer-модель. |
| R5-2 | P2, открыт | Copilot/Hyper OAuth refresh использует default transport вместо provider network policy. |
| R5-3 | P2, открыт | Terse stdout уже содержит primary до запуска reviewer. |

Полностью ложного исторического finding среди перечисленных не установлено.
**Опровергнута актуальность старых конкретных F1/F2/F4/F5/R2-1/R2-3**, а также
исправленных контрпримеров R2-2/R3-1. Одновременно опровергается более сильное
обобщение «после сетевых fixes каждый dial имеет собственный срок»: R6-1
проверяет стадию **до TCP connect**, до ранее проверенных protocol handshakes.
Наличие `IdleTimeout` в live path тоже не доказывает правильность всех
комбинаций watchdog options — новое ограничение этого вывода описано в R6-2.

## Подтверждённые открытые findings прежних раундов

### R3-1. P1 — captured ID переживает попытку и не синхронизирован с queued call

**Коммиты:** continuation `57fffb05`; исправления `2169dbe3`, затем
`b0a55aab`. Ниже проверен именно последний fix на новом HEAD.
**Места:** `internal/agent/coordinator_run.go:266,301,314,428,488,498,529,532,835`;
`internal/agent/agent_turn_step.go:175,176`;
`internal/agent/agent_run.go:83,91,547`;
`internal/agent/mailbox_ownership.go:73,265`.

**Что закрыто.** `ownAttemptAssistantMessage:716` выбирает строку по ID
из `OnAssistantMessageCreated`, проверяя session и assistant role. Более новая
чужая mB больше не заменяет evidence mA. Новые
`TestRunInternal_SuccessfulResultNotClobberedByConcurrentStalledMessage` и
`TestRunInternal_TransientContinuationUsesOwnPartialNotNewerForeignRow`
содержательно проверяют именно это. Старый контрпример не переносится на HEAD.

**Новый остаточный механизм A — stale ID после queued admission.**
`attemptAssistantMsgID` создаётся один раз на весь runInternal. Перед следующим
`run()` он не очищается; код полагается на будущий callback. Однако Run вправе
вернуть `(nil,nil)` после enqueue без создания assistant row. Этот исход
не исключается до classifier, и stalled branch проверяется раньше nil error.

Разрешённый сценарий при default двух retries, `IdleTimeout=0`,
`FailIfSessionBusy=false`:

1. A действительно исполнил первую попытку: partial mA, `Stream stalled`,
   callback установил mA.ID; A освободил ownership и вошёл в первый backoff.
2. B занимает сессию и остаётся её владельцем. Retry A добавляет continuation
   A2 в `mb.submitted`, возвращает `(nil,nil)`, callback ещё не вызывался.
3. Следующий classifier всё ещё получает ID **первой** попытки mA. Он видит
   error/stall/progress и разрешает новый continuation несмотря на queued outcome.
4. После второго backoff, пока B всё ещё владеет сессией, A добавляет A3
   в ту же очередь. В ней две копии continuation вместо одной.

`mailbox.submit` не дедуплицирует такие entries. После B dispatcher последовательно
передаёт обе в runTurn; `persistedModelCalls` в `agent_run.go:440` дедуплицирует
только запись model settings, **не исполнение turn**. Очередь может исполняться
после возврата A с queued result. Этот контрпример не требует повреждения памяти
или чужой assistant строки: достаточно одного собственного старого mA.

**Новый остаточный механизм B — Go data race на callback state.**
У первого queued вызова ID изначально пуст. После enqueue goroutine A продолжает
runInternal и читает обычную string в аргументах classifiers (`:488`, `:498`).
Goroutine владельца B может уже забрать эту queued call и в её PrepareStep
выполнить callback (`:301`), записав **ту же captured string**. Callback проходит
через очередь как часть SessionAgentCall; mutex/atomic/result-channel, который
упорядочил бы эту запись с чтением A, нет. Mailbox mutex защищает очередь,
а не эти обращения после unlock. В multi-step turn записи повторяются.

Это конкретная пара несинхронизированных read/write из разных goroutines,
а не только stale snapshot. Она установлена по source/handoff; диагностический
вывод `-race` и фактическая panic не заявляются. Даже замена string на atomic
не решает механизм A и не делает незавершённую queued call terminal outcome.

**Рекомендация:** явный admission outcome до retry classification, отдельное
состояние каждой попытки и безопасная передача её завершённого evidence.
Queued/refused outcome не может порождать ещё один enqueue. Требуются проверки
«свой stall → конкурент B → retry queued», callback queued call одновременно
с возвратом caller и 401/no-row attempt; сохранить исправленные successful/
foreign-row cases и допустимый recovery собственного завершённого stall.
Оба остатка считаются одним R3-1 — дефектом lifecycle attempt evidence.

### R2-2 / F3. P2 — whitespace-only фрагмент исключается из ответа

**Коммиты:** `babd295c`, `54acbf71`; join исправлен `8aabba4d`.
**Места:** `internal/app/app_run_terminal.go:115,142,180`;
`internal/app/app_run_reviewer.go:389`; `coordinator_run.go:682`.

Join равен `acc + next`, но error fragment добавляется только при непустом
`TrimSpace(text)`. Цепочка attempts `{"text":"a` → один пробел → `b"}`
с настоящими continuation user rows собирается в `{"text":"ab"}` вместо
`{"text":"a b"}`. Непустой reasoning у второго attempt удовлетворяет
`turnMadeProgress`, поэтому следующий continuation действительно разрешён.
Двух default retries достаточно. Оба результата — валидный JSON.

**Рекомендация:** сохранять каждый непустой fragment, включая whitespace-only;
проверять точные байты/значение результата. Старые разрывы слова, числа и
tool-step остаются исправленными. Доказательство — подстановка в production walk.

### R2-4. P2 — отменённый phase context и terminal cache переходят в reviewer

**Коммит:** `ccb510ff`; присваивание ctx исправлено `babd295c`.
**Места:** `internal/app/app_run_reviewer.go:209,356,366,394,598`;
`internal/app/app_run.go:513`.

Порядок commit успешной primary → parent cancel → cancellation probe сохраняет
primary в `cachedTerminal`. `finish` подавляет cancellation на committed success,
а его defer отменяет cached cleanup context. Reviewer gate не проверяет
`ctx.Err()`, reset не очищает cache/context/cancel. Следующее `finish` может
использовать primary как terminal reviewer и читать usage с отменённым context;
иной порядок select даёт cancellation. Это статически разрешённый interleaving.

**Рекомендация:** не начинать новую фазу после parent cancel, определить outcome
уже committed primary; очищать все phase cache/handles и проверять phase identity.
Существующие ownership guards сохранять.

### R2-5. P2 — credentials сетевых URL сохраняются в диагностике

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:79,91,95,103`;
`resolver.go:228,233,237,241,249`; `coordinator_providers_network.go:22`;
`internal/agent/agent_turn_failure.go:231`.

Parse/unsupported-scheme errors интерполируют исходный proxy URL; DoH errors
включают endpoint даже при обычном non-200. Userinfo/query values сохраняются
при `%w`, а runtime `err.Error()` может записываться в finish details/transcript.
Redaction HTTP headers этот data flow не закрывает. Реальные секреты не читались.

**Рекомендация:** безопасное представление endpoint без секретных компонентов,
включая вложенные parse/`url.Error`; проверять диагностические выходы на фиктивных
маркерах. Доказательство — прямой путь данных от config к тексту ошибки.

### R2-6. P2 — cache-test имеет ошибочный concurrency oracle

**Коммит:** `fd18cf0c`.
**Места:** `internal/nettransport/lifecycle_test.go:662,700,703`;
`transport_cache.go:13,70`; `.github/workflows/build.yml:190`.

`TestTransportCacheEvictionReleasesIdleConns` вызывает `t.Parallel`, пользуется
общим cache на 8 entries и требует один accept после двух warm-up requests.
Восемь distinct insertions других parallel tests между requests законно
вытесняют transport/закрывают idle connection. Второй fetch создаёт второй accept,
нарушая assertion. CI `-p 2` не сериализует tests внутри пакета; оговорённый
комментарием `-parallel 1` не установлен.

**Рекомендация:** собственный cache или сериализация данного oracle; concurrent
eviction проверять отдельными барьерами. Это доказанный конфликт assertion
с допустимым расписанием, **не наблюдавшийся в этом аудите flake**.

### F6. P2 — provider и network defaults принадлежат разным snapshots

**Коммит:** `eb59df12`.
**Места:** `internal/agent/coordinator_models.go:780`;
`coordinator_providers.go:729`; `coordinator_providers_network.go:17,18,19`.

Provider берётся из pinned cfg A, helper заново читает `c.cfg.Config()`.
Reload A→B между ними даёт provider A с сетью B; smart/fast тоже могут смешать
поколения. Atomic publication не делает эти два чтения одним snapshot.
**Рекомендация:** передавать pinned global network options до provider builder;
до исправления менять network policy через restart. Доказательство — разные
источники данных и разрешённый reload между чтениями, не memory race.

### F7. P2 — durable replay теряет IdleTimeout

**Коммит:** `f7362f7f`.
**Места:** `internal/agent/call_options.go:69,263`;
`call_data_conversion.go:90,126`; `internal/session/session_runqueue.go:138`.

В durable spec и явных initializer fields обоих converters отсутствует
`IdleTimeout`. Call со значением 5s после mirror/JSON/rebuild получает 0;
аналогично теряется disabled sentinel. Watchdog использует shared/default policy.
**Рекомендация:** совместимое durable поле и оба преобразования; round-trip
unset/positive/disabled. Доказательство — field lists, без wall-clock эксперимента.

### F8. P2 — итог reviewer не содержит primary tool inventory

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:118,135,382,467,598`;
`internal/app/app_run_terminal.go:54`.

Reset обнуляет counts, новый baseline исключает primary rows. Primary с одним
`view` и reviewer без tools дают пустой итоговый `tool_calls`, хотя стоимость
и длительность охватывают обе фазы. При delegation только в primary теряется
и соответствующий reduction warning. Это следует из последовательного reset
и reconciliation, конкуренция не нужна.
**Рекомендация:** отделить выбор terminal фазы от учёта invocation, объединять
tool IDs без повторного счёта; временно получать inventory из полного transcript.

### F9. P2 — JSON null вызывает исключение formatter при web render

**Коммит:** `14dd4a54` расширил старый дефект ActionRow на sub-agent transcript.
**Места:** `web/src/toolFormat.ts:12,13,27`;
`web/src/components/SubAgentBlock.tsx:43`;
`internal/agent/agent_prompt.go:555`, `sanitizeToolInput`.

`JSON.parse("null")` успешен, TypeScript cast не создаёт runtime guard.
`parsed[k]` для bash либо `Object.values(parsed)` для неизвестного инструмента
выбрасывает TypeError вне catch. `json.Valid` на backend допускает null.
В этом проходе функция прочитана, историческое Node-воспроизведение не
выдаётся за новый запуск или browser E2E.
**Рекомендация:** non-null object guard и безопасный preview иных JSON-типов;
render acceptance должен сохранять остальной transcript. Обязательно для web alpha.

### R3-2. P2 — custom HTTP proxy требует порт, который validation не требует

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:76`;
`proxy.go:96,101`; `client.go:87,95`.

Proxy-only HTTP использует stdlib canonical address/default 80. При включении
custom resolver `connectDialer` передаёт `pu.Host` прямо в net.Dialer.
Принятый URL без порта получает `missing port in address` до CONNECT.
SOCKS отдельно нормализован `d0966df9`; это не исправляет HTTP.
**Рекомендация:** Hostname/Port, default 80 и JoinHostPort с учётом IPv6.
Обход — явно указывать порт. Доказательство — различие ветвей адресации.

### R4-1. P2 — custom CONNECT response head не ограничен по размеру

**Коммиты:** `0f445020`, `eb59df12`; `fd18cf0c` добавил срок, не byte limit.
**Места:** `internal/nettransport/proxy.go:120,121`; `client.go:87`.

`http.ReadResponse(bufio.NewReader(conn), ...)` не получает byte budget.
В Go 1.26.3 `net/http/response.go:161,188` и `net/textproto/reader.go:508`
эта цепочка не ограничивает response head практически полезным пределом;
stdlib CONNECT, напротив, применяет LimitedReader (`transport.go:1907`).
Память зависит от H байтов заголовков, O(H); 30s deadline не является memory cap.
Воздействие ограничено некорректным/недоверенным настроенным proxy. Фактическое
исчерпание памяти и эксплуатация не проверялись.
**Рекомендация:** ограниченный status/header parser с закрытием connection
на превышении; после успеха сохранить tunnel payload и уже buffered bytes.

### R4-2. P2 — custom resolver отбрасывает запасные адреса

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/resolver.go:20,72,129,191,266`;
`client.go:116,124,134`.

ResolveFunc отдаёт один IP; первый A исключает остальные A и AAAA fallback.
Далее выполняется единственный dial. При DNS-ответе с недоступным первым
и доступным вторым адресом соединение падает; стабильный порядок ответов
сохраняет отказ на retry. Доказательство — return type, ранние return и отсутствие
цикла fallback. Happy-path tests используют один доступный IP.
**Рекомендация:** набор адресов и bounded fallback обеих families с общим budget,
тем же proxy и исходным TLS hostname. До исправления не обещать DNS failover.

### R5-1. P2 — 401 rebuild теряет временную reviewer-модель

**Коммит новой поверхности:** `ccb510ff`; rebuild/resolve путь старше недели.
**Места:** `internal/app/app_run_reviewer.go:655,682,685`;
`internal/agent/coordinator_run.go:321,327,368`;
`credentials.go:339`; `coordinator_models.go:94,106`;
`coordinator_providers.go:912`.

Reviewer R передан RunWithOverrides без persistence. После 401 и успешного
refresh rebuild вызывает `resolveCallModels(ctx, sessionID, nil)`, выбирающий
обычный session/global smart S. ModelRole=reviewer сохраняет tool policy,
но не заменяет smart selection. Последовательность S → R/401 → refresh → S
даёт успешный «reviewer» на другой модели/в общем случае другом provider.
Для статического ключа без refresh этот путь не срабатывает.

**Рекомендация:** сохранять identity/effort временного override при обновлении
credentials, не персистируя R вместо S. Acceptance: S → R/401 → R и неизменный
durable smart slot. Это отдельный от закрытых F1/F2 механизм.

### R5-2. P2 — OAuth refresh не наследует provider network transport

**Коммит интеграции:** `eb59df12`; helpers существовали до недели.
**Места:** `internal/agent/coordinator_providers_network.go:17`;
`coordinator_providers.go:832,951`; `internal/config/store_oauth.go:207`;
`internal/oauth/copilot/oauth.go:157,168,206`;
`internal/oauth/hyper/device.go:151,170`.

Inference получает configured client, а refresh через ConfigStore вызывает
Copilot/Hyper helpers с `http.Client{Timeout:30s}` без Transport. При доступности
auth endpoint только через Rush proxy inference с действующим token работает,
а refresh не сможет пройти по тому же маршруту. При доступном direct route
auth использует его независимо от per-provider policy. Это не бесконечный wait:
у HTTP request есть 30s timeout; передача token постороннему адресату не доказана.

**Рекомендация:** передавать согласованный network client в auth lifecycle,
сохраняя timeout/cancel и credential isolation. До исправления нужен независимый
рабочий default auth route либо провайдер без такого OAuth refresh.

### R5-3. P2 — terse stdout смешивает primary и reviewer

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:285,293,598`;
`internal/app/app_run.go:495,513`; контракт `app_run_request.go:18`.

Terse печатает каждый finished message сразу. Primary уже записан в stdout
до проверки reviewer gate; reset не может убрать записанные байты. Затем в тот
же writer печатается reviewer. Например, два самостоятельных JSON-ответа
оказываются рядом, хотя обещан итог reviewer как результат запуска.
RunModeJSON подавляет промежуточный stdout и этого механизма не имеет.

**Рекомендация:** выбирать единственный результат до выдачи terse stdout;
до исправления для машинного потребителя auto-review требовать `--json`.
Доказательство — последовательность write → gate → write без буферизации фаз.

## Новые findings round 6

### R6-1. P2 — начальный TCP dial HTTP proxy/DoH лишён собственного срока

**Коммиты:** транспорт введён `0f445020`, подключён `eb59df12`;
`fd18cf0c` восстанавливает TLS/idle defaults, но не DialContext этих ветвей.
**Места:** `internal/nettransport/client.go:80,95,98`;
`internal/nettransport/resolver.go:163,164,187,190`.
Зависимость: Go 1.26.3 `src/net/http/transport.go:48,1303,1320,1390,1529`
и `src/net/dial.go:Dialer.Timeout`.

**Механизм.** Proxy-only HTTP transport заполняет Proxy, но оставляет
DialContext/Dial nil. У DoH transport та же ситуация при direct или HTTP proxy
маршруте; SOCKS-ветвь имеет свой bounded dialer и сюда не относится.
Стандартный Transport в этой конфигурации вызывает **zeroDialer.DialContext**,
то есть `net.Dialer{Timeout:0}`. Это отличается от DefaultTransport с timeout 30s.

`getConn` запускает dial под `context.WithCancel(context.WithoutCancel(ctx))`.
После начала dial отказ request от результата (`wantConn.cancel`) не вызывает
его cancelCtx. Поэтому ни отмена внешнего запроса, ни 10s DoH Client.Timeout
не задают нижнему connect deadline. TLSHandshakeTimeout начинает действовать
после TCP, IdleConnTimeout — после создания idle connection, CONNECT guard —
после принятия TCP. Ни один из них эту стадию не покрывает.

**Доказательство и граница вывода.** Все перечисленные поля/ветви прочитаны
в Rush и фактическом toolchain. При недоступности proxy/DoH endpoint, когда
connect не завершается немедленной ошибкой, HTTP waiter может уже вернуть
cancel/timeout, а начальный TCP connect продолжится до срока ОС либо явного
закрытия transport. В долгоживущем App такой dial не обязан быть вытеснен из cache.
Это отсутствие собственного bound, **не заявление о вечном socket leak на любой
ОС**: системный TCP timeout может завершить его позже. Время и объём удержанных
ресурсов на реальной сети здесь не измерялись.

Прочитанные lifecycle tests начинают cancellation после того, как peer уже
получил CONNECT/DNS/DoH request, то есть после успешного начального TCP dial.
`TestBuildTransportBoundedTimeouts` проверяет только TLS/idle fields;
`TestResolveProviderHTTPClient` в HTTP proxy-only случае даже ожидает nil
DialContext. Эти проверки не устанавливают bound рассматриваемой стадии.

**Рекомендация:** установить bounded обычный TCP DialContext в обоих transport
конструкторах и перекрывать его только специальными resolver/SOCKS dialers;
сохранить текущую Proxy/SNI policy. Acceptance должен проверять завершение
самого initial dial при исчерпании его собственного бюджета, а не только
возврат HTTP waiter. Это новая конкретная неполнота, не повтор исправленных
молчащих CONNECT/DNS/SOCKS protocol handshakes из F4/R2-1.

### R6-2. P2 — progress-extension превращает idle timeout в абсолютный лимит

**Коммит новой поверхности:** `f7362f7f` — per-call `IdleTimeout` и terminal
stall policy. Первопричина старше недели: `git blame` относит вычисление
hardDeadline/effectiveDeadline в watchdog к `9d5b4e0a7` от 2026-05-17.
**Места:** `internal/agent/stream_watchdog.go:213,215,431,443,446`;
`internal/agent/agent_turn.go:461,475,477`;
`internal/agent/coordinator_run.go:773,835`;
`internal/cmd/run.go:578,835,848,849`.
Контракт: `README.md:221,225`, помощь `--timeout-hard-cap`.

**Механизм.** При `hardCap=0` watchdog инициализирует
`hardDeadline = startTime + idleTimeout`. В extendsOnProgress-ветви deadline
сначала продлевается по last activity, но затем **без проверки hardCap > 0**
снова ограничивается hardDeadline. Поэтому фактическая формула при cap=0:

```text
min(max(start + idle, lastActivity + idle), start + idle) = start + idle
```

Флаг, обещающий продление на progress, фактически задаёт абсолютный deadline.
Новая положительная `CallOptions.IdleTimeout` после такого срабатывания также
делает ошибку terminal: оба retry classifiers отказываются восстанавливать её.

**Конкретный сценарий, статический расчёт:** `--idle-timeout 20s`,
`--timeout-extends-on-progress`, `--timeout-hard-cap 0`, `--timeout 0`;
модель выдаёт text/reasoning каждую секунду, инструментов в полёте нет.
На первом watchdog tick после 20s с начала turn effectiveDeadline уже пройден,
хотя idle составляет около секунды. Записывается `Stream stalled`, run
завершается ошибкой. При `extendsOnProgress=false` та же activity не исчерпывает
idle budget. Действительный default idle interval — 15m, так что проблема
затрагивает и долгий healthy stream с одним включённым extension flag.

Алгоритм был ошибочен до недели; в недельный finding входит его несовместимость
с новым per-call порогом и запретом recovery. Это не регрессия default false
самого extension flag. Прочитанные `TestStreamWatchdog_ExtendsOnProgress`
и `..._FiresWhenIdle` всегда задают **положительный** hardCap; live precedence
тест IdleTimeout не проверяет такую комбинацию.

**Рекомендация:** при cap=0 не ограничивать продлённый idle deadline абсолютным
hardDeadline; сохранить отдельное действие положительного hard cap. Acceptance:
continuous progress дольше idle при cap=0, остановившийся progress и настоящий
positive cap. До исправления допустимо явно исключить extension flag из
alpha-профиля; обычный idle watchdog при false остаётся рабочим.

## Проверенные области без дополнительных release findings

- **Lock ordering/re-entrancy:** transport cache держит mutex только на map/clock;
  victim.CloseIdleConnections вызывается после unlock. В обычном переходе
  primary→review предыдущий Run уже вернулся. One-shot reservation claim и
  mailbox epoch/state guards сохраняются; нового цикла mutex/channel не найдено.
- **Lifecycle/cancellation:** handshakeGuard.stop закрывает stop-channel,
  joins watcher и снимает временный deadline до handoff tunnel. Error paths
  CONNECT закрывают connection; bufferedConn сохраняет прочитанные payload bytes.
  SOCKS budget 30s охватывает greeting/auth/CONNECT, DoH body закрывается.
  Это ограниченный вывод о перечисленных стадиях, с исключением R6-1.
- **Channels/panic:** done/drainDone имеют capacity 1, recovered runner panic
  преобразуется в terminal response; закрытый message channel заменяется nil.
  Backoff слушает ctx.Done и имеет конечный default retry count. Произвольный
  blocking writer/provider и все фоновые goroutines этим не сертифицированы.
- **Новый peak-hours message:** поле проходит config → error → guidance и
  обе стороны WS mapping, UI отправляет его для builtin/custom providers,
  CLI time-only update сохраняет существующую заметку. Это operator-authored
  текст, не исполнение команды. Изученные новые tests проверяют suffix,
  пустое поле, wire round-trip и сохранение заметки. Дополнительного доказанного
  release-relevant дефекта в этом diff не установлено.
- **Новая версия/schema:** `forkBaseVersion`, npm meta package и пять optional
  platform dependencies согласованы на alpha.2; `PeakHoursWindow.message`
  добавлен в schema. Это проверка committed metadata, не разрешение релиза.
- **Изоляция:** reviewer переносит FolderScope, DiskProvider и fail-fast,
  наследует context-carried allowlist; configured auto-review отключён для
  credentialed requests. Request auth сам по себе не смешивается от sharing
  одинакового transport. Reviewer write/bash — явная текущая политика,
  не самостоятельный security finding.
- **Бюджеты:** MaxCost/MaxTokens проверяются по total session usage;
  копирование этих лимитов в reviewer не обнуляет уже понесённый расход.
  Общий CLI timeout наследуется context. Не подтверждён отдельный «двойной
  бюджет reviewer» только из-за копирования полей.
- **Provider auth:** передача custom HTTP client не отключает Vertex ADC:
  fantasy Google `LanguageModel` вызывает `UseDefaultCredentials`, genai
  добавляет authorization middleware. Это не исправляет отдельную маршрутизацию
  auth helpers из R5-2. Полный lifecycle всех vendor SDK не проверен.
- **Config/transport construction:** empty config возвращает nil;
  resolver-only проходит раннюю ветвь без nil dereference proxyURL. Merge per
  field и DoH precedence соответствуют контракту. Два concurrent cache misses
  могут создать два transport, но 90s idle lifetime ограничен и у loser;
  старый бесконечный F5 из этого не следует.
- **MCP/processes:** изменения недели тестовые. Env override локализован
  через t.Setenv в serial test, directory identity сравнивается os.SameFile,
  wait helpers сохраняют конечные timers и Stop. Production descendant cleanup
  неделей не менялся; увеличение test timeout до 15s не доказывает отсутствия flakes.
- **Web/performance:** metadata вычисляется в существующем O(messages + parts)
  проходе, TimeBadge не создаёт timers/subscriptions. Нового доказанного
  квадратичного алгоритма или listener leak этой серии не найдено.
- **I/O/аллокации:** app baselines и classifiers повторно читают всю историю,
  reset/reviewer baseline строится дважды. Для N rows и S байтов это
  дополнительные O(N + S) чтения/декодирование/память. `b0a55aab` убрал
  pre-attempt baseline read, но own-ID lookup всё ещё использует Messages.List.
  Число concatenations связано с retry count, по умолчанию двумя retries.
  Без измерений эти расходы
  не объявляются release-critical regression; R4-1 и R6-1 имеют отдельные
  ресурсные механизмы, не основанные на предположении о скорости машины.

## Ограничения аудита и контроль результата

Не выполнялись Go build/test/vet/lint, `-race`, широкие suites, benchmarks,
искусственная нагрузка, fault injection, реальные LLM/OAuth/MCP/proxy requests,
browser/Playwright E2E, remote CI inspection или crash-consistency проверки.
Ни одного runtime failure/flake этого прохода не заявлено. Прочитанные tests
и исторические успешные прогоны — описание покрытия, не новый зелёный gate.

Проверка выборочная: не исследованы все interleavings, все платформенные
реализации process cleanup, весь DNS/TLS protocol, все SDK credential chains.
Незакоммиченные изменения намеренно исключены из release verdict. Будущие
исправления требуют отдельных целевых acceptance, особенно с управляемыми
барьерами для ownership/cancel и проверки ресурсов после возврата waiter.

Выполнены read-only log/diff/blame/status и чтение исходников/локальных
зависимостей. `git diff --check ccc2f9b5 fbc8a177` прошёл. Перед commit
проверены полный status, точный staged file list и `git diff --cached --check`.
Index до staging отчёта был пуст; staged diff содержит только этот Markdown.
После внешних commits один контрольный status содержал лишь отчёт и исходный
` D web/dist/.gitkeep`, затем появилась параллельная unstaged правка
`internal/agent/coordinator_retry_test.go`. Она сохранена и исключена из commit;
гарантировать отсутствие чужих текущих правок в единственном общем worktree
нельзя. Для commit используется явный pathspec только отчёта. Чужие файлы
не откатывались и не записывались аудитом.
Граница commit — **только** `docs/reviews/2026-09-22-weekly-commit-audit-round6.md`.
Частные абсолютные пути компьютера в отчёт не включены.

## Минимальные обязательные исправления и финальный release verdict

Для общей alpha с заявленными режимами минимальный gate:

1. **R3-1:** отдельный lifecycle attempt evidence, синхронизация callback
   и отсутствие повторного enqueue после queued admission. Сохранить уже
   исправленный запрет использования чужих rows. Это главный P1.
2. **R2-2:** точность всех fragments, включая whitespace-only; проверять байты
   и смысл JSON, а не только его синтаксическую валидность.
3. **R2-4 + R5-1:** phase lifecycle после cancel и сохранение временной reviewer
   identity при refresh; **R5-3** — единственный выбранный terse result.
4. **R2-5 + R4-1 + R6-1:** redaction сетевой диагностики, byte bound CONNECT head
   и собственный TCP dial timeout на всех новых transport ветвях.
5. **F9:** null-safe render перед web alpha.
6. **R5-2:** согласованная network policy для OAuth lifecycle до обещания работы
   OAuth-провайдеров исключительно через настройки сети Rush.
7. **R6-2:** исправить zero-cap extension либо явно исключить это сочетание flags.

**R2-6** отдельно закрыть до использования cache-test как надёжного release gate.
После исправлений нужны целевые regression/acceptance tests; concurrency —
с барьерами и целевым `-race`. Один случайный serial pass не снимает finding.

Минимальный ограниченный профиль может отложить F6/F7/F8/R3-2/R4-2 только
с явными условиями: network changes через restart; без обещания durable
idle-policy round-trip; inventory из полного transcript; явный HTTP proxy port;
без обещания fallback custom resolver на другие IP. R5-2 допускает независимый
default auth route, R5-3 — обязательный `--json`, R6-2 — extension flag выключен.
Отключение reviewer/custom network/web может исключить соответствующие ветви,
но **не устраняет R3-1 в оставшихся queue-capable coordinator вызовах**.
Fail-fast профиль исключает приведённые queued сценарии, однако сам по себе
не сертифицирует прочие error/no-row/401 переходы. Такие ограничения
этим аудитом не вводились и согласованными не считаются.

**Финальный verdict для `fbc8a177` / alpha.2: NO-GO общей alpha.** Предыдущие закрытые
механизмы остаются закрытыми; подтверждённые открытые дефекты и два новых P2
не позволяют дать GO. Отчёт фиксирует проверенный срез и условия последующего
release gate; его documentation commit не является разрешением на выпуск.
