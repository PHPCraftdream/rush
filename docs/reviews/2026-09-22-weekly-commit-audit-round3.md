# Rush: независимый release-readiness аудит, round 3

Дата: **2026-09-22**. Проверенная ревизия:
`54acbf71d702f3d596c363a889d181a0a6a33377`.

## Summary

**Release verdict: NO-GO для общей alpha текущего HEAD.**

Исправление SOCKS5 действительно закрывает R2-1. Копирование
`FailIfSessionBusy` закрывает конкретный механизм R2-3 — постановку reviewer
в очередь из-за потерянного флага. Продолжение через tool-step теперь собирается,
но R2-2 закрыт не полностью: склейка всё ещё повреждает ответ при разрыве внутри
слова без граничного пробела. Название исправляющего коммита этого не опровергает.

Найдены **два ранее не описанных механизма**: P1 — continuation принимает чужой
stalled message за результат собственного вызова и может повторить уже
отклонённый fail-fast reviewer; P2 — включение custom DNS/DoH ломает HTTP proxy URL
без явно указанного порта. С учётом незакрытых прежних находок получается
**два P1 и восемь P2**, без повторного подсчёта F3 как отдельного от R2-2 дефекта.
P0 не установлен.

| ID | Severity | Состояние на проверенном HEAD |
| --- | --- | --- |
| R2-2 / F3 | P1 | Частично исправлен; новый контрпример склейки `hel` + `lo` |
| R3-1 | P1 | Новый: retry по чужому/stale terminal message обходит отказ admission |
| R2-4 | P2 | Открыт: terminal cache и отменённый cleanup context переживают смену фазы |
| R2-5 | P2 | Открыт: proxy/DoH credentials включаются в диагностические ошибки |
| R2-6 | P2 | Открыт: cache-eviction test зависит от отсутствия разрешённых параллельных вставок |
| F6 | P2 | Открыт: provider и global network policy читаются из разных поколений config |
| F7 | P2 | Открыт: durable round-trip теряет `IdleTimeout` |
| F8 | P2 | Открыт: итоговые tool counts и reduction warning не охватывают primary-фазу |
| F9 | P2 | Открыт: JSON `null` выбрасывает исключение в web formatter |
| R3-2 | P2 | Новый: HTTP proxy без порта перестаёт работать в combined resolver mode |

P1 означает существенное нарушение контракта выполнения или целостности
результата в поддерживаемом сценарии. P2 означает более узкий сценарий с
обходным путём либо дефект надёжности проверки. P2, связанный с секретами или
web render, всё равно блокирует выпуск соответствующей возможности без
исправления или явно принятого ограничения.

Нового доказанного циклического mutex/channel deadlock и гонки памяти Go
не установлено. R3-1, R2-4 и F6 — ошибки принадлежности состояния и допустимых
последовательностей событий; называть их результатом `-race` было бы неверно.

## Срез, рабочая копия и метод

- Период: **2026-09-15 00:00:00 +02:00 — исходный HEAD от
  2026-09-22 00:06:53 +02:00**, `Europe/Berlin`, по committer date среди
  предков HEAD. Собственный documentation commit аудита в срез не входит.
- База: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  2026-09-11 14:08:09 +02:00. Между базой и нижней границей новых коммитов нет.
  `--since` и `--since-as-filter` дают одинаковые **23 коммита**.
- Совокупный diff `ccc2f9b5..54acbf71`: **63 файла, 8421 добавленная и
  643 удалённые строки**; сюда входят документация и тесты, а не только код.
- Исходное окружение указывало на `main`; отдельного предоставленного worktree
  в `git worktree list --porcelain` не было. Для соблюдения изоляции создан
  отдельный detached worktree ровно от указанного HEAD. Обнаруженное в основной
  рабочей копии удаление `web/dist/.gitkeep` оставлено без изменений и в аудит
  коммитов не включено. Index и файлы основной рабочей копии не использовались
  для внесения отчёта или его коммита.
- В изолированном worktree исходный `git status --porcelain=v1
  --untracked-files=all` пуст. Разрешённое изменение — только этот Markdown.
  Исходники, тесты, версии, CHANGELOG и конфигурация не изменялись. Sub-agents,
  реальные LLM-запросы и push не использовались.
- Изучены log, состав коммитов, production diff, текущие реализации, вызывающий
  код, локальные зависимости и выбранные регрессионные тесты. Все номера строк
  ниже относятся к **`54acbf71`**, если явно не указана другая ревизия.
- Основное доказательство Go findings — статическая трассировка конкретного
  пути или допустимого interleaving. Приведённые сценарии Go не запускались.
  Отдельно выполнена проверка фактического TypeScript formatter через Node;
  её результат указан в F9. Заявления прежних отчётов или commit messages о
  тестах не считаются результатом этого прохода.

Предыдущие отчёты:
[round 1](2026-09-21-weekly-commit-audit.md) и
[round 2](2026-09-21-weekly-commit-audit-round2.md).
Их нижняя граница была 14 сентября; в данном отчёте она сдвинута на 15 сентября
по заданию. Набор ранних коммитов не изменился: первый из них датирован 19 сентября.

## Карта недельной серии

| Коммит | Проверенная область |
| --- | --- |
| `57fffb05` | Partial-output continuation, классификация retry, принадлежность history |
| `14dd4a54` | Общий formatter tool input и новая поверхность sub-agent render |
| `924add8a` | Model/effort metadata, проход по sub-agent history |
| `10e32c56` | Постоянное отображение timestamp |
| `87ee064f` | Удаление package-wide env override из Windows MCP TestMain |
| `64409ab3` | Сравнение directory identity и bounded MCP test waits |
| `df0051b1` | Checkpoint, production-код не меняет |
| `ccb510ff` | Автоматический reviewer, повторный phase lifecycle |
| `0f445020` | NetworkConfig, proxy/DNS/DoH, ошибки и маршрутизация |
| `85ee8836` | Checkpoint, production-код не меняет |
| `eb59df12` | Provider HTTP clients, snapshot и debug/Copilot wrappers |
| `180f1544` | Context-aware network test doubles |
| `5c20c771` | Context-aware request в provider-network test |
| `bf9f6d3d` | Checkpoint, production-код не меняет |
| `f7362f7f` | CLI timeout defaults, idle watchdog и durable options |
| `e547e12e` | Первый аудит, проверен как набор утверждений |
| `dd03ecf0` | Checkpoint, production-код не меняет |
| `97c22050` | Два web-теста metadata/timestamps |
| `babd295c` | Reviewer context/credentials и первая сборка continuation text |
| `fd18cf0c` | CONNECT/DNS/DoH bounds, transport cache и forwarding cleanup |
| `31e90d2e` | Второй аудит, проверен как набор утверждений |
| `d0966df9` | Собственный deadline SOCKS5, включая вложенный DoH transport |
| `54acbf71` | Continuation через tools, whitespace heuristic, fail-fast reviewer |

## Перепроверка прежних findings

«Закрыт» здесь означает устранение описанного механизма по исходному коду,
а не утверждение о независимо пройденном acceptance run.

| Прежний finding | Подтверждение / опровержение актуальности |
| --- | --- |
| F1, P1 | Исторически подтверждён, закрыт `babd295c`: `resetForReviewerPass:599` присваивает `s.ctx`; `buildReviewerPassTurn:683` очищает reserved ownership. Старый reviewer со smart toolset/stale token повторно не заявляется. |
| F2, P1 | Исторически подтверждён, закрыт `babd295c`: `app_run.go:513` исключает все `req.Credentials != nil`. Tenant transcript не уходит в configured auto-reviewer по прежнему пути. |
| F3, P1 | Частично закрыт `babd295c` и `54acbf71`. Tool-step больше не обрывает обычную цепочку; порча байтов сохраняется как R2-2. |
| F4, P1 | Описанные CONNECT/DNS/TLS/DoH и последующий SOCKS5 механизмы закрыты `fd18cf0c` + `d0966df9`. Не переносится на HEAD как открытый P1. |
| F5, P1 | Прежнее бесконечное удержание idle pools закрыто `fd18cf0c`: cache, 90s idle timeout, eviction основного и DoH pools, cleanup forwarding. Полное синхронное освобождение всех App-owned ресурсов этим не доказано. |
| F6–F9, P2 | Подтверждены на текущем коде; собственные доказательства и рекомендации ниже. |
| R2-1, P1 | Закрыт `d0966df9`: `socksDialer:86` создаёт `context.WithTimeout`, `newDoHResolver:180` использует тот же bounded dialer вместо stdlib SOCKS branch. |
| R2-2, P1 | Частично закрыт: прежний пример с tail ` world` работает; новый tail без пробела всё ещё повреждается. Детали ниже, без нового ID для того же дефекта. |
| R2-3, P1 | Конкретная потеря флага закрыта `54acbf71`, `app_run_reviewer.go:677`; atomic `mailbox.submit:69` отказывает без enqueue. Более широкая гарантия fail-fast не доказана: новый R3-1 находится выше mailbox, в retry policy. |
| R2-4–R2-6, P2 | Не закрыты; новые fix-коммиты не меняют соответствующие gate/cache-reset, redaction и parallel-test механизмы. |

Полностью ложных исторических findings первых двух раундов не установлено.
Опровергается перенос уже закрытых F1/F2/F4/F5/R2-1 и исходного механизма R2-3
на текущий HEAD. Также опровергается утверждение о полном восстановлении
точности continuation в `54acbf71`.

### Почему R2-1 действительно закрыт

В `internal/nettransport/proxy.go:53–89` budget охватывает TCP dial, greeting,
auth и SOCKS CONNECT. Локально проверен `golang.org/x/net v0.55.0`:
`internal/socks/client.go`, `Dialer.connect`, устанавливает deadline из context;
на успешном возврате синхронизируется с cancellation watcher и снимает deadline.
`internal/socks/socks.go`, `Dialer.DialContext`, закрывает connection при ошибке.

`internal/nettransport/resolver.go:163–190` направляет DoH-over-SOCKS через этот
же механизм, поэтому собственного 10s `http.Client.Timeout` больше не требуется
ошибочно считать сроком нижнего detached SOCKS dial. Production budget SOCKS
составляет 30s: отмена внешнего request не обязана немедленно закрыть его dial,
но прежнее неограниченное ожидание молчащего proxy устранено.

Прочитаны `TestSOCKS5HandshakeBoundedOnCancelledRequest` и
`TestDoHOverSocks5NestedDialBounded`: оба проверяют также исчезновение socket
на стороне stub. Изменение package timeout выполняется в serial tests;
на основании одного наличия package var гонка с `t.Parallel` не заявляется.
Эти тесты в данном проходе не исполнялись.

## Незакрытый P1 с новым доказательством

### R2-2 / F3. P1 — склейка continuation всё ещё меняет байты ответа

**Коммиты:** исходная возможность `57fffb05`; сборка `babd295c`;
неполное исправление `54acbf71`.
**Место:** `internal/app/app_run_terminal.go:117`, `continuationChainText`,
и `:178–188`, `joinContinuationText`; использование результата —
`internal/app/app_run_reviewer.go:389`.

Теперь обход пропускает tool results и assistant tool steps, а на user rows
проверяет continuation prefix. Это закрывает прежний обычный сценарий
`error → continuation → tool_use → tool result → end_turn`.

Но выбор разделителя основан на пробеле на краю фрагмента. При отсутствии
пробела с обеих сторон строка 188 безусловно вставляет `\n\n`. Разрыв сетевого
потока не обязан совпасть с границей слова:

```text
partial: {"text":"hel
tail:    lo"}
ожидается: {"text":"hello"}
получается: {"text":"hel<LF><LF>lo"}
```

Полученный JSON содержит буквальные переводы строки внутри string и невалиден.
Тот же механизм меняет обычное слово `hel` + `lo` и разрывает JSON number
`{"n":1` + `2}`. Это детерминированная подстановка в production branch,
а не предположение о том, как модель обычно пишет пробелы.

**Воспроизводимый сценарий, не запускался:** взять provider stub из
`TestExecuteRunContinuationMidJSONStringJoinsWithoutSeparator:517`, заменить
`partial` на `{"text":"hel`, а `tail` на `lo"}`; остальные transient-failure
steps оставить теми же. Проверить точные байты возвращённого `FinalText` и
`json.Valid`. Текущий тест использует tail с ведущим пробелом и поэтому
доказывает только одну ветвь heuristic. Условие positive CLI `IdleTimeout`
не предотвращает этот случай: transient сетевой сбой отличается от idle stall.

**Рекомендация:** определить точный контракт результата — продолжение байтов
одного документа либо самостоятельная полная финальная версия. Не угадывать
границу абзаца по whitespace. Связать attempts с логическим ответом; проверить
разрывы внутри слова/числа/строки, whitespace, несколько attempts и tool steps.

## Новые findings

### R3-1. P1 — чужой stalled message превращает отказ admission в continuation

**Коммит:** `57fffb05`, `shouldContinueTurn`; `f7362f7f` ограничивает только
вызовы с positive `IdleTimeout`. `54acbf71` исправляет mailbox policy reviewer,
но не этот путь.
**Место:** `internal/agent/coordinator_run.go:464–505`, retry loop;
`:668`, `lastAssistantMessage`; `:741–758`, `shouldContinueTurn`.
Отказ ниже: `internal/agent/agent_run.go:88–89`, `mailbox_ownership.go:69`.

**Механизм.** После любого результата `currentAgent.Run` coordinator ищет
последнее assistant message всей сессии. Проверки принадлежности message
только что выполненному attempt/LogicalCallID нет. Если оно имеет partial
content и finish `error / Stream stalled`, `shouldContinueTurn` возвращает
true **до проверки переданного `err`**. При `IdleTimeout == 0` это верно даже
для `ErrSessionBusy`, для которого собственный provider request не начинался.
Затем coordinator заменяет prompt на continuation с чужим partial, ждёт backoff
и повторяет `Run`. Mailbox корректно отказал; более высокий слой превратил
отказ в новую попытку.

**Допустимый interleaving, статическое доказательство:**

1. Вызов A — SDK smart run с auto-reviewer, `FailIfSessionBusy=true`,
   `IdleTimeout=0`, retry budget не нулевой. Primary A успешно завершён.
2. До admission reviewer другой законный вызов B получает эту же сессию.
   B сохраняет partial assistant с `Stream stalled`; остановить B барьером
   после terminal write, до освобождения ownership. Такое окно существует
   между terminal write в `handleStreamFailure` (`agent_turn_failure.go:240`) и возвратом
   dispatcher `runOwned`.
3. Reviewer A приходит в `mailbox.submit`, получает `ErrSessionBusy`, не
   добавляется в очередь. Его retry classifier читает последнюю строку B,
   возвращает true на строке 758 и формирует continuation уже с текстом B.
4. B освобождает ownership до окончания 10s backoff. Повторный `run()` A
   теперь допускается и выполняет reviewer, хотя A должен был вернуть busy.

В тесте это воспроизводится барьерами, provider stub и коротким test backoff;
ждать production watchdog не нужно. Дополнительная минимальная проверка:
`shouldContinueTurn` с partial stalled row и `ErrSessionBusy` должен вернуть
false; по текущим условиям он возвращает true. Текущий
`TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim:330` оставляет последней
успешную primary-строку A, поэтому этот контрпример не покрывает.

**Риск:** нарушение fail-fast, неожиданный повтор исполнения и использование
чужого контекста попытки; reviewer имеет write/bash tools. Здесь не утверждается
межпользовательский обход авторизации session ID или исполнение после возврата
A: отличие от старого R2-3 именно в retry до возврата, без mailbox enqueue.
Для обычного CLI с positive idle override конкретная stalled ветвь закрыта,
но SDK по умолчанию оставляет override нулевым.

**Рекомендация:** admission/busy/shutdown/queued outcomes завершать до retry
classification. Stall evidence должен относиться к собственному начатому
attempt, а не к последнему message сессии. Проверять как отказ с чужим stalled
partial, так и настоящий собственный stall; не ломать разрешённый recovery.
Нужна интеграционная проверка отсутствия второго provider call после busy.

### R3-2. P2 — HTTP proxy URL без порта ломается при включении resolver

**Коммиты:** `0f445020`, подключение к provider path — `eb59df12`;
`d0966df9` нормализует default port только для SOCKS5.
**Место:** `internal/nettransport/proxy.go:96–103`, `connectDialer`;
`client.go:87–103`, выбор режима; `config.go:75–97`, validation.

**Механизм.** `http://127.0.0.1` — принимаемый `resolveConfig` URL. В proxy-only
ветке `http.Transport.Proxy` добавляет стандартный порт HTTP 80 через
`net/http.canonicalAddr`. При добавлении `DNSServer` или `DoHURL` выбирается
custom `resolvedDialer`/`connectDialer`. Последний передаёт `pu.Host` прямо в
`net.Dialer.DialContext`, то есть `127.0.0.1` без порта. TCP dial требует
`host:port` и возвращает `missing port in address` ещё до CONNECT.

**Сценарий, не запускался:**
`BuildHTTPClient(NetworkConfig{Proxy:"http://127.0.0.1", DNSServer:"127.0.0.1:53"})`,
затем запрос к `http://target.invalid/`. Config construction успешен, а
DNS-over-TCP tunnel не открывается из-за отсутствующего proxy port. Для этого
отказа реальный proxy/DNS server не нужен. Контрпроба — тот же proxy с `:80`;
отдельно проверить proxy-only URL и IPv6 authority. Статически сверена разница
с Go 1.26.3 `src/net/http/transport.go:3033`, `canonicalAddr`.

**Риск:** ранее работающая proxy-only конфигурация перестаёт выполнять provider
запросы после включения DNS/DoH. Это ошибка адресации, а не зависание или
доказанная утечка credentials.

**Рекомендация:** нормализовать HTTP proxy authority с default port 80 до
передачи custom dialer, используя `Hostname`/`Port` и `net.JoinHostPort`.
Временное ограничение alpha — требовать явный порт для HTTP proxy в combined
mode; обход доступен без изменения кода, но сейчас требование не выражено
validation и зависит от режима.

## Остальные открытые findings: повторная проверка и рекомендации

### R2-4. P2 — terminal cache не сбрасывается при переходе после cancel

**Коммит:** `ccb510ff`; `babd295c` и `54acbf71` не закрывают этот механизм.
**Место:** `internal/app/app_run_reviewer.go:209–216`, `:356–367`, `:394` и
`resetForReviewerPass:598`; reviewer gate `app_run.go:513`.

При cancel после committed successful primary cancellation-ветка сохраняет
`cachedTerminal`, context и cancel handle. `finish` принимает committed success,
снимает cancellation error и своим defer отменяет cleanup context. Gate не
проверяет `ctx.Err()`, а reset не очищает эти три поля. Reviewer стартует с
отменённым parent; при его completion через `finish` cached primary снова
становится authoritative terminal, а usage читается с уже отменённым context.

**Доказательство/сценарий:** барьер между commit primary и done, parent cancel,
выбор cancellation-ветки, затем reviewer completion. Возможен успешный primary
на месте reviewer outcome; при другом порядке select — cancellation error.
Это сохраняющийся статический interleaving, не наблюдавшийся в этом аудите flake.
**Рекомендация:** gate на живой parent перед новой фазой, определённый outcome
уже завершённой primary, очистка terminal cache/handles и проверка phase identity.

### R2-5. P2 — credentials остаются в network error strings

**Коммиты:** `0f445020`, `eb59df12`.
**Место:** `internal/nettransport/config.go:79`, `:91–95`, `:103`;
`resolver.go:228–249`, `dohQuery`; wrapper
`internal/agent/coordinator_providers_network.go:22`.

Unsupported proxy `https://demo:SECRET@proxy.invalid:8443` включается в ошибку
целиком через `%q`; DoH endpoint
`https://demo:SECRET@resolver.invalid/dns-query?token=TOKEN` включается через `%s`
даже при HTTP 503. Wrapping `%w` сохраняет секрет; runtime `err.Error()` может
попасть в finish details (`agent_turn_failure.go:231`). Header redaction этот
текст не очищает. Это прямая подстановка вымышленных маркеров в ветви ошибок,
без внешних запросов или чтения реальных credentials.

**Рекомендация:** безопасное endpoint representation, исключение userinfo и
секретных query values, sanitization вложенного `url.Error` и parse errors.
Проверить отсутствие маркеров в errors, logs, transcript и envelope. Для
alpha с credentials в network URLs исправление обязательно.

### R2-6. P2 — cache eviction test остаётся зависимым от расписания

**Коммит:** `fd18cf0c`.
**Место:** `internal/nettransport/lifecycle_test.go:662–706`,
`TestTransportCacheEvictionReleasesIdleConns`; `transport_cache.go:12`, `:70`;
`.github/workflows/build.yml:190`.

Тест всё ещё вызывает `t.Parallel`, работает с process-global cache ёмкости 8
и требует один accept после двух warm-up requests. Восемь distinct config
insertions из других parallel tests между этими requests законно вытесняют
pool. Второй fetch открывает новое соединение, и oracle ошибочно падает.
Комментарий про `-parallel 1` не обеспечен catch-all CI command.

**Доказательство:** допустимое расписание вставок и точное условие
`require.Equal(t, 1, base)`. Фактического тестового падения в этом проходе нет:
тесты не запускались. **Рекомендация:** изолировать cache или сериализовать
именно этот oracle; concurrency eviction проверять отдельно с барьерами.
Исправить до использования данного теста как release evidence.

### F6. P2 — network defaults не принадлежат pinned provider snapshot

**Коммит:** `eb59df12`.
**Место:** `internal/agent/coordinator_models.go:780`, `buildModelsFromCfg`;
`coordinator_providers.go:729`; `coordinator_providers_network.go:17–19`.

Provider берётся из pinned config A, но helper снова читает `c.cfg.Config()`.
Reload A→B между этими точками создаёт provider A с proxy/DNS policy B; smart
и fast builds также могут получить разные поколения. Atomic immutable snapshots
не исправляют смешение двух отдельных чтений.

**Сценарий:** остановить build после получения A, опубликовать B с другим proxy,
продолжить и сравнить выбранный transport. **Рекомендация:** передавать pinned
global network options вместе с provider config. До исправления не обещать
горячую смену network policy; использовать restart.

### F7. P2 — durable replay теряет idle policy

**Коммит:** `f7362f7f`.
**Место:** `internal/agent/call_options.go:69`, `:263`;
`call_data_conversion.go:90`, `:126`; `internal/session/session_runqueue.go:138`.

`CallOptionsSpec` и оба converters всё ещё не содержат `IdleTimeout`.
Round-trip call с `IdleTimeout=5s` через durable mirror/JSON даёт 0; аналогично
теряется CLI disabled sentinel. Вместо объявленного threshold включается
shared/default значение. Live precedence-тест этого не проверяет.

**Рекомендация:** добавить поле и оба преобразования с совместимостью старых
записей; проверить 0, positive и disabled round-trip. Пока это не сделано,
сохранение idle policy после durable handoff/restart не гарантируется.

### F8. P2 — reviewer-envelope не содержит полного tool inventory

**Коммит:** `ccb510ff`.
**Место:** `internal/app/app_run_reviewer.go:118`, `:135`, `:382`, `:458`,
`resetForReviewerPass:598–616`; `app_run_terminal.go:53`.

Reset и новая phase обнуляют counts и исключают primary messages новым
baseline. Reconciliation считает только reviewer calls, хотя cost/tokens/time
остаются общими для invocation. Если primary вызывает один `view`, а reviewer
отвечает текстом, итоговый `tool_calls` не содержит `view:1`. При delegation
только в primary исчезает и reduction warning, условие которого читает counts.

**Рекомендация:** раздельные границы phase-terminal и invocation accounting,
объединение tool IDs без повторного счёта. До исправления оркестратор должен
читать полный transcript; `tool_calls` не является полным inventory.

### F9. P2 — подтверждённое исключение web formatter на JSON null

**Коммит:** `14dd4a54` расширил существовавший дефект на sub-agent render.
**Место:** `web/src/toolFormat.ts:12–13`, `formatActionArgs`;
`web/src/components/SubAgentBlock.tsx:43`.

`JSON.parse("null")` успешен, TypeScript cast runtime guard не создаёт.
Последующий `parsed[k]` бросает исключение за пределами try/catch.
Backend `sanitizeToolInput` (`internal/agent/agent_prompt.go:555`) проверяет
`json.Valid`, поэтому null сам по себе не отсекается.

**Выполненное воспроизведение:** фактический `toolFormat.ts` прочитан Node
24.12.0, типы удалены в памяти через `stripTypeScriptTypes`, module исполнен
через data URL. Файлы не переписывались. Результаты:

| Input для `formatActionArgs("bash", input)` | Результат |
| --- | --- |
| `null` | `TypeError: Cannot read properties of null (reading 'command')` |
| `{` | Пустая строка |
| `{}` | Пустая строка |
| `{"command":"pwd"}` | `pwd` |
| `[]` | Пустая строка |
| `"hello"` | Пустая строка |

Это подтверждает исключение реальной функции, но не выдаётся за выполненный
browser crash E2E. **Рекомендация:** non-null object guard, определённое поведение
scalar/array и render-тест, сохраняющий остальной transcript. Для web alpha
исправление обязательно; CLI-only режим этой поверхностью не затронут.

## Проверенные области без дополнительных release findings

- **Lock ordering / re-entrancy:** cache mutex удерживается только на map/clock;
  `CloseIdleConnections` выполняется после unlock. Последовательный обычный
  primary→review переход не вызывает recursive `Run` под собственным OS lock.
  Mailbox epoch/state и one-shot ownership claim не ослаблены. R3-1 не отменяет
  корректность atomic mailbox refusal — дефект в дальнейшем решении retry.
- **Channels и cancellation:** phase done/drainDone имеют capacity 1;
  recovered turn panic превращается в terminal response. Закрытый message
  channel обнуляется, не создаёт busy loop. В retry backoff есть `ctx.Done`.
  Для arbitrary writer/provider, игнорирующего cancel, общего доказательства
  bounded completion нет. R2-4 остаётся отдельно указанным phase-state дефектом.
- **CONNECT/SOCKS handoff:** custom CONNECT закрывает socket на ошибках;
  guard stop дожидается watcher до передачи tunnel и снимает deadline;
  buffered reader сохраняет уже прочитанные tunnel bytes. Успешный CONNECT
  body нельзя механически закрывать как обычный response body. SOCKS timeout
  после успешного соединения не должен обрезать весь LLM stream, и проверенный
  x/net снимает handshake deadline.
- **Credential/scope boundary:** credentialed auto-review отключён; FolderScope,
  DiskProvider и context-carried allowlist передаются в reviewer. Общий transport
  сам по себе не смешивает API keys: auth принадлежит запросам. Сохранение
  MaxCost/MaxTokens проверено по total session usage, поэтому «новая фаза
  автоматически обнуляет бюджет» не подтверждается кодом.
- **HTTP provider composition:** custom client передаётся в switch-ветки;
  debug/Copilot cleanup forwarding доходит до base transport. Vertex auth
  не объявлен потерянным из-за custom client: fantasy v0.25.2 вызывает
  `UseDefaultCredentials`, а genai v1.57.0 добавляет authorization middleware.
  Реальный OAuth/token refresh через proxy и все vendor SDK end-to-end не
  проверялись. TLS hostname verification не заменена проверкой resolved IP.
- **Config и cache:** NetworkConfig merge выполняется по полям; DoH precedence
  соответствует коду/документации. Resolver-only и empty branches не дают
  предполагаемого nil dereference `rs.proxyURL.Scheme`. Два параллельных cache
  misses могут создать два transports; idle timeout ограничивает idle lifetime,
  поэтому это не повтор прежней бесконечной утечки F5. Полная App/Client
  ownership-модель скрытого DoH transport этим не доказана.
- **MCP/process waits:** production process-tree код недельная серия не меняет.
  Изученные правки ограничивают env mutation нужным serial test, сравнивают
  directory identity через `os.SameFile` и сохраняют timer.Stop/конечный срок
  wait helpers. Увеличение timeout до 15s не считается доказательством
  устранения всех CI flakes; новые process cleanup эксперименты не выполнялись.
- **Performance:** metadata вычисляется в существующем линейном проходе;
  TimeBadge не добавляет interval/subscription. Повторные `Messages.List` и
  baseline construction дают дополнительные O(N + S) чтения/аллокации по
  истории и тексту. Сборка префикса повторными string concatenations зависит
  также от числа retry attempts; при default budget оно ограничено двумя
  retries. Без измерений нет основания объявлять эти конечные расходы новой
  release-critical performance regression. Искусственная нагрузка не создавалась.

## Ограничения и контроль сохранности

Выполнены read-only history/diff/blame/status, чтение исходников и выбранных
локальных Go 1.26.3/x/net/provider SDK implementations, проверка formatter F9.
`git diff --check ccc2f9b5 HEAD` прошёл до внесения отчёта.

**Не выполнялись:** Go build/test/vet/lint, `-race`, project-wide suites,
benchmarks, stress/fault injection, Playwright/browser E2E, реальные
LLM/MCP/proxy requests, remote CI inspection. Изменяется только документация;
никакого заявления о зелёном runtime gate этот commit не делает. Для R3-1 и
R2-4 доказано допустимое расписание по коду, а не частота его появления.

Не проведены полный DNS/TLS protocol audit, проверка всех OS-specific путей,
всех возможных interleavings и crash-consistency всей системы. Отсутствие нового
доказанного deadlock не означает сертификации всего Rush. Подозрения без
конкретного механизма и release impact в findings не включались.

Два web-теста из прежних отчётов не менялись; SHA-256 совпадают:

| Файл | SHA-256 |
| --- | --- |
| `web/tests/message-timestamp-always-visible.spec.ts` | `CE416416F90632CD972DE8F262785E576BC784888F881F9C9811C5BA0952F891` |
| `web/tests/subagent-block-metadata.spec.ts` | `CB72A0EB814462FB64EA7655DBE0B826905FEE79060ED2135ED57E9F22611678` |

## Минимальный обязательный gate и финальный verdict

Для снятия текущих двух P1 необходимы:

1. **R2-2:** точность возвращаемого continuation-result, включая разрыв без
   whitespace. Acceptance проверяет полный envelope и валидность JSON, tools
   между fragments и границу reviewer phase.
2. **R3-1:** retry только собственного реально начатого attempt; busy/queued
   admission не превращаются в continuation по чужой history. Acceptance
   проверяет отсутствие дополнительного provider/tool execution после отказа.

Для общей alpha с нынешними auto-reviewer, network credentials и web также
необходимы **R2-4** (cancel/phase cache), **R2-5** (secret redaction) и **F9**
(null-safe render). **R2-6** нужно устранить до опоры на cache-test как на
release evidence; обычный случайный зелёный serial run его не закрывает.

F6/F7/F8/R3-2 допускают только явные ограничения первого alpha-профиля:
network policy меняется с restart; durable replay не обещает сохранение idle
policy; inventory берётся из полного transcript; HTTP proxy в combined mode
указывает порт явно. Если эти ограничения не приняты, соответствующие
исправления также обязательны до обещания полного набора возможностей.
Здесь перечислены условия будущего решения, а не уже введённые ограничения.

После исправлений нужны только целевые regression/acceptance checks описанных
механизмов; concurrency cases — с управляемыми барьерами и целевым `-race`.
Повторять всю недельную suite для доказательства простой byte-склейки не нужно.

**Итог: `54acbf71` остаётся NO-GO для общей alpha.** Последние исправления
снимают реальные прежние блокеры, но целостность результата и принадлежность
retry attempt всё ещё нарушены. Исходный код в этом аудите намеренно не менялся.

Контроль перед коммитом: `git status --porcelain=v1 --untracked-files=all`
показывает только новый файл этого отчёта; tracked diff и исходный index пусты.
Отчёт подготовлен для отдельного semantic commit
`docs: add weekly release readiness audit round 3`.
