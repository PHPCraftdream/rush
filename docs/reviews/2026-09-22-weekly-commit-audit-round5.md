# Rush: независимый release-readiness аудит, round 5

Дата: **2026-09-22**. Проверенный HEAD:
`a4f99d8b449d08a4eebe5d6e328b2a7aa7a3b180`.

## Summary

**Release verdict: NO-GO для общей alpha с текущим набором возможностей.**

После проверенного в round 4 `c32c72d6` добавлен только предыдущий отчёт:
production-код и тесты не изменились. Повторная проверка подтверждает оставшиеся
**один P1 и одиннадцать P2** round 4. Закрытые механизмы не открываются заново.

Найдены **три дополнительных P2** на стыках уже проверявшихся возможностей:

- R5-1: после успешного refresh на HTTP 401 автоматический reviewer теряет
  временный выбор модели и может продолжиться на обычной smart-модели сессии.
- R5-2: новый provider network transport не применяется к OAuth refresh
  Copilot/Hyper; в окружении, требующем настроенного в Rush proxy, обновление
  credentials перестаёт работать независимо от работоспособности inference.
- R5-3: default terse stdout содержит склеенные ответы primary и reviewer,
  хотя контракт обещает итог reviewer как результат запуска.

Итого: **15 актуальных findings — один P1 и четырнадцать P2**, без двойного
подсчёта F3/R2-2. P0 не установлен. Нового доказанного циклического deadlock,
Go data race или безусловной panic в production Go-путях недели не установлено.
Это локальный вывод аудита, не доказательство их отсутствия во всём Rush.

Главный блокер остаётся R3-1: session-wide последнее сообщение нельзя считать
результатом своего attempt только потому, что изменился его ID. Остальные
обязательные исправления для заявленных режимов перечислены в release gate.
Ни код, ни тесты этим отчётом не исправляются.

## Срез, метод и сохранность

- Период: **2026-09-15 00:00:00 +02:00 — HEAD от 2026-09-22
  07:40:12 +02:00**, `Europe/Berlin`, по committer date среди предков HEAD.
  Собственный documentation commit round 5 в проверенный срез не входит.
- База перед периодом: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  2026-09-11 14:08:09 +02:00. Первый коммит периода — 19 сентября; сдвиг нижней
  границы round 1/2 с 14 на 15 сентября не исключает ранних изменений серии.
- `--since` и `--since-as-filter` дают одинаковые **29 коммитов**. Совокупный
  diff базы с HEAD: **65 файлов, 9936 добавленных / 651 удалённая строка**,
  включая документацию и тесты. Diff `c32c72d6..HEAD` — только 569 строк round 4.
- Изучены история, diff недельной серии, актуальные затронутые реализации,
  вызывающий код, выбранные regression tests и все четыре предыдущих отчёта.
  Дополнительно прослежены 401 rebuild, OAuth refresh и оба output-пути reviewer.
  Все ссылки вида `файл:строка` ниже относятся к проверенному HEAD.
- Основные доказательства — статические ветви и допустимые последовательности
  событий. Приведённые Go-сценарии **не запускались**. Для F9 исполнена фактическая
  TypeScript-функция через Node 24.12.0 с удалением типов в памяти, без файловых
  изменений и установки пакетов. Go 1.26.3 и `golang.org/x/net v0.55.0`
  проверены по локальным исходникам, а не по предположениям о cancellation.
- Ограничение предоставленного окружения: `git branch --show-current` показал
  `main`, `git worktree list` — единственную рабочую копию. Отдельного
  изолированного worktree фактически не предоставлено. Аудит выполнен в этой
  копии; новые worktrees/ветки не создавались. Изоляция отдельным worktree
  здесь **не заявляется**.
- Начальное состояние: только пользовательский ` D web/dist/.gitkeep`, index
  пуст. Удаление не восстановлено и не включается в commit. Единственное
  изменение содержимого аудита — этот Markdown; код, CHANGELOG, версии,
  конфигурация и тесты сохранены. Sub-agents, push и внешние записи отсутствуют.

Предыдущие отчёты:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md).
Их выводы, комментарии в коде и названия fix-коммитов проверялись как утверждения;
они не заменяют доказательство на текущем HEAD.

### Карта недельной серии

| Коммиты | Предмет проверки |
| --- | --- |
| `57fffb05` | Partial-output continuation, retry и принадлежность результата |
| `14dd4a54`, `924add8a`, `10e32c56` | Общий formatter, sub-agent metadata, timestamps |
| `87ee064f`, `64409ab3` | Изоляция env, directory identity и ожидания MCP-тестов Windows |
| `ccb510ff` | Две фазы ExecuteRun, reviewer context, временная модель, вывод и accounting |
| `0f445020`, `eb59df12` | NetworkConfig, proxy/DNS/DoH, provider clients, auth и snapshots |
| `180f1544`, `5c20c771` | Context-aware вызовы test doubles |
| `f7362f7f` | CLI timeout defaults, idle override, watchdog и durable round-trip |
| `97c22050` | Playwright-покрытие timestamp и sub-agent metadata |
| `babd295c`, `fd18cf0c` | Reviewer isolation, continuation result, network guards/cache/cleanup |
| `d0966df9`, `54acbf71` | SOCKS5 budget, continuation через tools, reviewer fail-fast |
| `c9eb9a98`, `8aabba4d`, `2169dbe3`, `c32c72d6` | Tagged switches, byte join, retry baseline/refusal guards и тесты |
| `df0051b1`, `85ee8836`, `bf9f6d3d`, `dd03ecf0` | Checkpoints; production-изменений нет |
| `e547e12e`, `31e90d2e`, `295cc386`, `a4f99d8b` | Четыре предыдущих отчёта |

## Статус прежних findings

«Закрыт» означает устранение описанного механизма по коду, а не выполненный
в этом проходе acceptance run. P1 — существенное нарушение исполнения,
блокирующее общую alpha. P2 — ограниченный сценарий, требующий исправления
либо явно принятого ограничения затронутой возможности.

| ID | Severity на HEAD | Независимая перепроверка |
| --- | --- | --- |
| F1 | Закрыт | `babd295c`: `resetForReviewerPass` присваивает `s.ctx`; reviewer получает свой toolset/context и очищенный reservation. Старый stale-token сценарий не актуален. |
| F2 | Закрыт | `babd295c`: gate `app_run.go:513` исключает `req.Credentials != nil`. Configured auto-review не запускается для credentialed calls. |
| F3 | Учтён в R2-2 | Исходная потеря всего префикса устранена. Отдельным открытым finding не считается. |
| F4 | Закрыт в описанном объёме | `fd18cf0c` + `d0966df9`: CONNECT/DNS guards, DoH client timeout, TLS timeout и SOCKS budget устраняют прежние ожидания handshakes без собственного срока. |
| F5 | Закрыт в описанном объёме | `fd18cf0c`: 90s idle timeout основного/DoH pools, cache на 8 entries, eviction вне mutex, cleanup forwarding. Бесконечный idle lifetime прежнего кода не переносится на HEAD. |
| F6 | P2, открыт | Pinned provider config соединяется с заново прочитанным global network config. |
| F7 | P2, открыт | Durable spec/converters не сохраняют `IdleTimeout`. |
| F8 | P2, открыт | Reviewer reset исключает primary tool inventory из итогового envelope. |
| F9 | P2, открыт | Formatter падает на JSON `null`; повторно подтверждено исполнением функции. |
| R2-1 | Закрыт | `d0966df9`: собственный deadline SOCKS применяется и для вложенного DoH dial. |
| R2-2 | P2, частично закрыт | Tool-step и mid-word контрпримеры закрыты `54acbf71` + `8aabba4d`; whitespace-only error fragment всё ещё исключается. |
| R2-3 | Закрыт | `54acbf71`: `FailIfSessionBusy` передан reviewer; `mailbox.submit` отказывает атомарно без enqueue. |
| R2-4 | P2, открыт | Parent cancel не закрывает gate следующей фазы; terminal cache/cleanup handles не сбрасываются. |
| R2-5 | P2, открыт | Network URL попадает в error strings без удаления credentials. |
| R2-6 | P2, открыт | Parallel cache-test ожидает отсутствие разрешённого конкурентного eviction. |
| R3-1 | P1, частично закрыт | Refusal и равный baseline отсекаются; чужая более новая строка по-прежнему управляет retry успешного/queued вызова. |
| R3-2 | P2, открыт | Custom HTTP CONNECT использует `pu.Host` без default port. |
| R4-1 | P2, открыт | Custom CONNECT-response head не ограничен по размеру. |
| R4-2 | P2, открыт | Custom resolver возвращает один IP и теряет возможность address fallback. |

Полностью ложного исторического finding среди этих четырёх отчётов не
установлено. **Опровергнут безусловный перенос закрытых F1/F2/F4/F5/R2-1/R2-3
на HEAD**, а также прежних конкретных контрпримеров R2-2/R3-1. Новый R5-1
не открывает F1/F2: корректный reviewer context и credentialed gate остаются;
ошибка находится в выборе модели при последующем 401 rebuild.

## Подтверждённые оставшиеся механизмы

### R3-1. P1 — более новый ID не устанавливает владельца retry evidence

**Коммиты:** `57fffb05`; неполное исправление `2169dbe3`.
**Места:** `internal/agent/coordinator_run.go:420`, `:478`,
`lastAssistantMessage:703`, `shouldContinueTurn:812`, особенно `:843`;
`shouldRetryTurn:747`. Освобождение ownership: `agent_run.go:547–558`,
`mailbox_ownership.go`, `drainOrReleaseFinal`.

Classifier проверяет refusal, отличие последнего assistant ID от baseline,
error finish и progress. Он не связывает сообщение с returned result или
LogicalCallID данного вызова. Stalled branch возвращает true до проверки
`err == nil`; даже успешный outcome A может быть заменён recovery по B.

**Статическое доказательство — разрешённое расписание двух вызовов одной сессии:**

1. A запоминает baseline `m0`, выполняет turn с `end_turn`, освобождает
   mailbox/OS lock и получает ненулевой result с nil error. A задержан перед
   классификацией retry.
2. B законно занимает уже свободную сессию, сохраняет новое partial message
   `mB` с `Stream stalled` и завершает свой вызов. Positive IdleTimeout у B
   может исключить его собственный retry.
3. A с нулевым IdleTimeout читает `mB`: refusal отсутствует, ID отличается от
   `m0`, progress есть. `shouldContinueTurn` разрешает recovery успешного A.
4. A формирует continuation с текстом B и после backoff исполняет лишний turn,
   который также может вызывать tools. Fail-fast не мешает, когда B уже ушёл.

Это нарушение принадлежности двух разрешённых вызовов сессии, а не доказанный
межпользовательский обход авторизации или race памяти. Аналогичный пробел есть
для `(nil, nil)` queued admission в `agent_run.go:91`: его классификация
в `app_run_turn.go:68` происходит только после возврата coordinator.

**Рекомендация:** передавать явный admission/attempt outcome и идентичность
собственных terminal rows. Refused/queued/успешный исход не должен запускать
recovery по session-wide history. Acceptance с барьерами должен проверять
successful A/new foreign B, queued A, refusal и собственный допустимый stall.
Тесты `TestRetryClassifiers_AttemptScopedEvidence` и
`TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage`
прочитаны: они закрывают refusal/равный baseline, но не приведённое расписание.

### R2-2 / F3. P2 — исключается whitespace-only фрагмент continuation

**Коммиты:** `babd295c`, `54acbf71`; byte join исправлен `8aabba4d`.
**Места:** `internal/app/app_run_terminal.go:117`, `continuationChainText`,
фильтр `:142`, `joinContinuationText:180`; потребитель `app_run_reviewer.go:389`.

Join теперь равен `acc + next`, однако error-message добавляется в parts только
при `strings.TrimSpace(text) != ""`. Цепочка из трёх attempts с текстами
`{"text":"a`, одиночный пробел и `b"}` даёт `{"text":"ab"}` вместо
`{"text":"a b"}`. Между attempts — настоящие continuation user messages;
у второго attempt есть непустой reasoning, поэтому `turnMadeProgress:673`
разрешает continuation несмотря на whitespace-only visible text. Двух retries
по умолчанию достаточно. Это статическая подстановка, не runtime-прогон.

**Рекомендация:** сохранять каждый непустой text fragment связанной цепочки,
включая whitespace-only; отдельно проверять точные байты/значение JSON.
Одна проверка валидности JSON пропускает этот дефект. Прежние случаи `hel` +
`lo`, числа и tool-step остаются исправленными; severity P2 из round 4 сохранена.

### R2-4. P2 — terminal cache и отменённый context переходят в reviewer

**Коммит:** `ccb510ff`; `babd295c` исправляет передачу ctx, но не lifecycle.
**Места:** `internal/app/app_run_reviewer.go:209–216`, `:356–367`, `:394–396`,
`resetForReviewerPass:598`; gate `internal/app/app_run.go:513`.

После commit успешной primary и parent cancel event loop может выбрать
cancellation-ветку, сохранить terminal/cache context и вернуть из `finish`
успех: committed success снимает cancellation error. Defer уже отменил
`cachedTerminalCtx`. Gate не проверяет parent `ctx.Err`, reset оставляет
`cachedTerminal`, context и cancel handle. Следующий `finish` способен принять
cached primary за terminal reviewer и читать usage с отменённым context.

**Доказательство:** порядок commit primary → cancel → cancellation probe →
reviewer done проходит указанные ветви; другой выбор select может дать cancel.
Принудительный interleaving не исполнялся.
**Рекомендация:** не начинать следующую фазу после parent cancel; определить
outcome уже committed primary и сбрасывать все phase cache/handles с проверкой
идентичности фазы. Сохранить epoch guards; они не являются причиной дефекта.

### R2-5. P2 — network credentials сохраняются в диагностических ошибках

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/config.go:79`, `:91–95`, `:103`;
`resolver.go:228–249`, `dohQuery`;
`internal/agent/coordinator_providers_network.go:22`;
`agent_turn_failure.go:231`.

Ошибки parse/unsupported scheme включают исходный proxy URL; ошибки DoH
включают endpoint, в том числе при обычном non-200 ответе. Userinfo и секреты
query string остаются в строке. `%w` переносит её выше; runtime error может
сохраняться в finish details через `err.Error()`. Redaction HTTP headers этот
путь не затрагивает. Доказательство — прямой data flow по исходникам;
реальные credentials не читались, сетевого воспроизведения не было.

**Рекомендация:** безопасное представление endpoint без userinfo/секретных
query values, включая вложенные parse errors и `url.Error`. Проверять
диагностику, transcript и envelope на вымышленных секретных маркерах.

### R2-6. P2 — cache-test имеет недетерминированный oracle

**Коммит:** `fd18cf0c`.
**Места:** `internal/nettransport/lifecycle_test.go:662–706`,
`TestTransportCacheEvictionReleasesIdleConns`; `transport_cache.go:12,70`;
`.github/workflows/build.yml:190`.

Тест вызывает `t.Parallel` и требует ровно один TCP accept после двух warm-up
requests через общий cache на 8 entries. Восемь distinct insertions других
parallel tests между requests законно вытесняют transport и закрывают idle
connection. Второй fetch открывает новую, а `require.Equal(t, 1, base)` падает.
CI `-p 2` не ограничивает in-package parallelism; указанный в комментарии
`-parallel 1` не задан. Это доказанный конфликт oracle с разрешённым расписанием,
**не наблюдавшееся в этом аудите падение теста**.

**Рекомендация:** собственный cache либо сериализация именно этого oracle;
конкурентный eviction проверять отдельными барьерами. Случайный serial pass
не снимает дефект release gate.

### F6. P2 — network defaults читаются из другого snapshot

**Коммит:** `eb59df12`.
**Места:** `internal/agent/coordinator_models.go:780`, `buildModelsFromCfg`;
`coordinator_providers.go:729`; `coordinator_providers_network.go:17–19`.

Provider берётся из pinned cfg A, но network helper заново читает
`c.cfg.Config()`. Reload A→B между чтениями даёт provider A с defaults B;
между smart/fast builds также возможны разные поколения network policy.
Atomic публикация snapshots не объединяет два чтения в одно.
**Доказательство:** раздельные источники значений и допустимый reload между ними.
**Рекомендация:** передавать pinned global network config до provider builder;
временно применять смену сетевой политики только через restart.

### F7. P2 — durable replay теряет IdleTimeout

**Коммит:** `f7362f7f`.
**Места:** `internal/agent/call_options.go:69,263`;
`call_data_conversion.go:90,126`, оба converters;
`internal/session/session_runqueue.go:138`, `CallOptionsSpec`.

Поле участвует в live watchdog, но отсутствует в spec и явных field lists
обоих преобразований. Round-trip `IdleTimeout=5s` через durable mirror/JSON
детерминированно восстанавливает 0. Теряется также disabled sentinel CLI;
вместо per-call threshold включается shared/default policy.
**Доказательство:** перечисленные initializer fields; wall-clock тест не нужен
и не запускался. **Рекомендация:** совместимое durable поле и оба преобразования,
round-trip для unset/positive/disabled. До исправления не обещать сохранение
idle policy при durable handoff/restart.

### F8. P2 — reviewer-envelope теряет primary tool inventory

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:118,135,382,467,598–616`;
`app_run_terminal.go:53`.

Reset обнуляет counts, новый baseline исключает primary rows; reviewer result
заменяет primary result. Primary с одним `view` и reviewer без tools дают
пустой итоговый `tool_calls`, хотя cost/time относятся к обеим фазам. Если
delegation была только в primary, исчезает и gate reduction warning.
**Доказательство:** последовательный проход reset → baseline → reconciliation.
**Рекомендация:** разделить phase-terminal selection и invocation accounting,
объединять tool IDs без повторного счёта. Временный обход — полный transcript.

### F9. P2 — JSON null выбрасывает исключение web formatter

**Коммит:** `14dd4a54` расширил старый дефект ActionRow на sub-agent render.
**Места:** `web/src/toolFormat.ts:12–13`, `formatActionArgs`;
`web/src/components/SubAgentBlock.tsx:43`;
`internal/agent/agent_prompt.go:555`, `sanitizeToolInput`.

`JSON.parse("null")` успешен, cast TypeScript runtime guard не создаёт;
`parsed[k]`/`Object.values(parsed)` падают уже вне catch. Backend `json.Valid`
сам по себе null не исключает. В просмотренном `web/src` ErrorBoundary не найден.

**Выполненная проверка фактического formatter, Node 24.12.0:**

| Аргументы | Результат |
| --- | --- |
| `bash`, `null` | `TypeError: Cannot read properties of null (reading 'command')` |
| `custom`, `null` | `TypeError: Cannot convert undefined or null to object` |
| `bash`, `{` или `{}` | Пустая строка |
| `bash`, object с `command: pwd` | `pwd` |

Функция загружалась из текущего файла через `stripTypeScriptTypes` и data URL,
без переписывания исходника. Browser crash E2E не выполнялся.
**Рекомендация:** non-null object guard, безопасное поведение иных JSON-типов
и render acceptance, сохраняющий остальной transcript. Обязательно для web alpha.

### R3-2. P2 — HTTP proxy без порта ломается с custom resolver

**Коммиты:** `0f445020`, `eb59df12`; `d0966df9` нормализует только SOCKS5.
**Места:** `internal/nettransport/proxy.go:96–103`, `connectDialer`;
`client.go:88–103`; `config.go:75–97`.

Validation принимает HTTP proxy URL без порта. Proxy-only путь использует
stdlib canonicalAddr, добавляющий 80; combined resolver path передаёт
`pu.Host` прямо в `net.Dialer`, которому требуется host:port. Получается
`missing port in address` до CONNECT. Разница сверена с локальным Go 1.26.3
`src/net/http/transport.go:3033`; сетевой сценарий не запускался.
**Рекомендация:** нормализовать Hostname/Port с default 80 через JoinHostPort,
учитывая IPv6. Временный обход — явный порт в proxy URL.

### R4-1. P2 — custom CONNECT-response head не ограничен по размеру

**Коммиты:** `0f445020`, `eb59df12`; `fd18cf0c` добавляет срок, но не size limit.
**Места:** `internal/nettransport/proxy.go:120–121`; custom path `client.go:88`.

`http.ReadResponse(bufio.NewReader(conn), ...)` читает status line/headers
без внешнего byte budget. В Go 1.26.3 `response.go:161,188` вызывает
`textproto.ReadLine`/`ReadMIMEHeader`, а `textproto/reader.go:508` использует
практически неограниченный header budget. Stdlib CONNECT, напротив, добавляет
`io.LimitedReader` в `net/http/transport.go:1907`.

**Доказательство:** отсутствие bound в цепочке чтения. Аллокации зависят от H,
объёма response head: O(H). 30s deadline не задаёт предел памяти. Воздействие
ограничено некорректным/недоверенным настроенным proxy в combined mode;
OOM и эксплуатация не проверялись.
**Рекомендация:** ограничить суммарный response head и закрывать connection
при превышении; после успешного handshake сохранить tunnel payload/buffered
bytes. Изменение только размера bufio buffer не решает проблему.

### R4-2. P2 — custom resolver лишает dial резервных адресов

**Коммиты:** `0f445020`, `eb59df12`.
**Места:** `internal/nettransport/resolver.go:20`, `:72–80`, `answerIP:129`,
`:191–200`, `:266–274`; `client.go:113–134`, `resolvedDialer`.

Resolver отдаёт один IP; наличие первого A исключает остальные A и fallback
на AAAA. `resolvedDialer` делает единственный dial. При двух DNS-адресах,
из которых первый недоступен, а второй доступен, соединение всё равно падает.
Стабильный порядок DNS-ответа воспроизводит отказ при каждом retry.
**Доказательство:** тип resolveFunc, ранние return и отсутствие dial fallback;
проверенный happy-path тест использует один доступный адрес.
**Рекомендация:** сохранить набор адресов и bounded fallback обеих families
с общим budget, неизменным proxy и исходным TLS hostname. Сценарий не запускался.

## Новые findings round 5

### R5-1. P2 — 401 rebuild теряет временную reviewer-модель

**Коммит новой поверхности:** `ccb510ff`, автоматический неперсистентный reviewer.
Путь rebuild старше недели: `97ffd22f` в `credentials.go` и
`coordinator_run.go:316`; это не впервые написанная в сентябре ошибка resolver.
**Места на HEAD:** `internal/app/app_run_reviewer.go:655–685`;
`internal/agent/coordinator_run.go:310–322`, `:356–388`, `RunWithOverrides:855`;
`credentials.go:339–343`; `coordinator_models.go:94–126`;
`coordinator_providers.go:911–922`.

Reviewer получает модель R через аргумент RunWithOverrides и намеренно очищает
model persistence. После 401 и успешного credential refresh `rebuildCall`
вызывает `resolveCallModels(ctx, sessionID, creds)`. Для auto-review `creds == nil`,
поэтому выбирается resolveSessionModels: durable smart slot сессии либо global
smart S. Исходный временный override R туда не передаётся. `ModelRole=reviewer`
в CallOptions меняет tool policy, но не выбор smartCfg этим resolver.

**Статический сценарий с настоящим поддерживаемым refresh-путём:**

1. Smart S успешно завершает primary. Reviewer R отличается от S; его provider
   использует OAuth либо APIKeyTemplate, который допускает повторное разрешение.
2. Первая reviewer-попытка получает 401. Refresh завершается успешно;
   `runWithUnauthorizedRetry` вызывает rebuild и затем повторяет fn.
3. Rebuild читает обычную модель сессии S, сохраняет reviewer prompt/CallOptions,
   но заменяет pinned SmartModel на S. Следующий успешный ответ S принимается
   как итог reviewer-фазы. В общем случае меняется также provider.

Здесь не требуется конкурентный reload или нарушение mailbox ownership.
Достаточна последовательность S → R/401 → успешный refresh → S. При статическом
API key без refresh повтор не происходит: это явное ограничение finding.
Запрет configured reviewer для tenant calls F2 не затронут. Доказана потеря
выбора reviewer, а не отправка данных неизвестному третьему лицу.

**Рекомендация:** сохранять неизменную идентичность per-call model overrides
при обновлении credentials; rebuild должен получать свежий ключ для R и
сохранять её model/effort, не персистируя R в сессию. Acceptance должен проверять
последовательность S → R/401 → R и неизменность durable smart slot.
Прочитанный `Test401Retry_RebuildsCallWithFreshCredentials` проверяет число
вызовов обычного Run; reviewer E2E проверяет только clean response. Эти проверки
не устанавливают сохранение временного override после 401. Новый сценарий
в этом аудите не исполнялся.

### R5-2. P2 — OAuth refresh не использует настроенный provider transport

**Коммит интеграции:** `eb59df12`; NetworkConfig введён `0f445020`.
OAuth helpers существовали до недели; finding относится к неполному подключению
новой сетевой возможности, а не к регрессии прежнего default-route refresh.
**Места:** `internal/agent/coordinator_providers_network.go:17–30`;
`coordinator_providers.go:729`, `refreshTokenIfExpired:832`,
`refreshOAuth2Token:950–961`; `internal/config/store_oauth.go:207–211`;
`internal/oauth/copilot/oauth.go:157–169,206`;
`internal/oauth/hyper/device.go:151–171`.

Новый client передаётся в inference provider builders, включая Copilot.
Однако proactive/401 refresh идёт через ConfigStore в copilot.RefreshToken /
hyper.ExchangeToken. Оба создают `http.Client{Timeout: 30 * time.Second}`
без Transport: применяется process default, а не разрешённый NetworkConfig.
У этих функций вообще нет аргумента, несущего выбранный network client.

**Доказательство:** прослежена полная цепочка от runInternal до HTTP Do.
В окружении без process-wide proxy, где нужные endpoints доступны только через
Rush network proxy, inference с ещё действующим token может работать, а refresh
не сможет обратиться к token endpoint. Если прямой маршрут доступен, запрос
refresh использует его и default resolver, независимо от provider policy.
Поддержка proxy через environment может скрыть проблему, но не реализует
per-provider настройки. Это не неограниченное ожидание: refresh имеет 30s timeout.

Последующий inference с прежним просроченным token и повтор refresh после 401
не восстанавливают работоспособность при том же маршруте. Заявление README
`1458–1466` о настройке provider connections только через rush.json поэтому
недостаточно для полноценного lifecycle OAuth-провайдера в таком окружении.
Не утверждается раскрытие token получателю вне OAuth endpoint; реальных
credentials/запросов в этом проходе не было.

**Рекомендация:** передавать согласованный network transport в refresh path,
сохранив его собственные timeout/cancel и изоляцию credentials; учитывать
направление зависимостей config/nettransport. Целевой acceptance должен
проверить initial inference и последующий refresh при одном provider policy.
До исправления возможен только явно ограниченный профиль: независимый рабочий
default route для auth либо провайдер без такого OAuth refresh.

### R5-3. P2 — terse stdout склеивает primary и reviewer

**Коммит:** `ccb510ff`.
**Места:** `internal/app/app_run_reviewer.go:285–294`, `handleMessageEvent`;
`resetForReviewerPass:598`; `internal/app/app_run.go:487–517`;
контракт `app_run_request.go:18–20`, `README.md:323–334`.

В default RunModeTerse каждый finished message сразу печатается в stdout.
Primary уже напечатан до проверки reviewer gate. Reset очищает tracking maps,
но уже записанный stdout не отменяет. Reviewer печатается в тот же writer;
единственный завершающий newline находится в defer всего ExecuteRun.

**Детерминированная трасса:** primary без tools/переводов строки отвечает
`PRIMARY`, reviewer — `REVIEW`. На stdout получается `PRIMARYREVIEW\n`,
хотя заявленный итог запуска — reviewer. Это не зависит от скорости live
events: `finish:379` воспроизводит committed terminal через тот же handler.
Wrapper, сохраняющий stdout как конечный документ/вердикт, получает смешанный
результат; даже разделения фаз в таком случае нет.

**Рекомендация:** в terse выбирать итоговую фазу до публикации её результата
в stdout и сохранять primary output при неприменимом/отказавшем reviewer
по явно определённому контракту. Acceptance — точные байты stdout для
primary+review и для отключённого reviewer. `--json` с чтением FinalText
обходит именно этот дефект. `--stream` по своему отдельному контракту выводит
все токены: его наличие не используется как самостоятельное доказательство
ошибки. Прочитанные reviewer E2E работают в RunModeJSON; terse сценарий здесь
установлен статически, не запускался.

## Проверенные области без дополнительных release findings

- **Lock ordering и re-entrancy:** transport cache держит mutex только на
  map/clock, закрывает victim после unlock. В нормальном последовательном
  primary→review переходе прежний coordinator уже вернулся, reservation
  очищен. Atomic token claim и mailbox epoch/state guards сохранены. Нового
  цикла взаимного ожидания в этих путях не установлено. R3-1 нарушает
  принадлежность evidence над корректным mailbox, а не доказывает его deadlock.
- **Channels, cancel, panic:** turn done/drainDone имеют capacity 1;
  runAgentTurnRecovered возвращает terminal error при panic своего runner.
  Закрытая message subscription заменяется nil. Backoff слушает ctx.Done;
  interrupt ticker отменяется до join. R2-4 ограничивает утверждения о phase
  cleanup. Произвольный provider/tool/writer, игнорирующий cancel, этим не
  сертифицирован; recovery своей goroutine не является recovery всех goroutines.
- **Handshake lifecycle:** CONNECT error paths закрывают conn; guard.stop
  joins watcher до передачи tunnel и снимает deadline. Buffered bytes остаются.
  SOCKS получает собственный 30s context deadline; прочитанный x/net снимает
  deadline на успехе и закрывает connection при ошибке. DoH body закрывается;
  DoH/wire DNS чтения имеют предел порядка 64 KiB. Эти свойства не закрывают R4-1.
- **Resource ownership:** cache reuse и idle timeout устраняют прежний F5.
  Concurrent cache misses могут создать разные transports; это не бесконечная
  idle-утечка, поскольку timeout есть у обоих. Внешний CloseIdleConnections
  не доказывает немедленного закрытия скрытого DoH pool вне eviction.
  Полный синхронный App-owned shutdown здесь не установлен.
- **Изоляция:** credentialed auto-review отключён; FolderScope/DiskProvider,
  fail-fast и контекстная allowlist переходят в reviewer. Reviewer write/bash
  соответствует текущей явной policy, а не отдельному найденному обходу.
  Cache одинакового network config сам по себе не смешивает request API keys.
  R5-1/R5-2 относятся к другим границам: identity rebuild и transport refresh.
- **Config/provider wiring:** per-field merge, DoH precedence, zero-config
  nil client, HTTP provider switch и forwarding cleanup просмотрены.
  Resolver-only ветвь не разыменовывает nil proxyURL. TLS использует исходный
  hostname запроса. Generation/epoch guards model cache сохранены.
- **MCP/process waits:** изменения недели здесь тестовые. Env override
  локализован в нужном serial test; directory identity сравнивается через
  os.SameFile; изученные wait helpers имеют конечный timer и Stop. Увеличение
  срока до 15s не принимается за доказательство отсутствия flakes. Production
  process-tree cleanup не менялся серией и динамически не проверялся.
- **CLI idle policy:** override доходит до runTurn и обоих retry classifiers;
  disabled sentinel конечен. Default `--timeout=0` сохраняет 6h hard backstop,
  timer останавливается на возврате. Idle timeout не ограничивает любой startup
  или tool path; потеря durable значения выделена в F7.
- **Web/performance:** model/effort вычисляются в существующем линейном проходе;
  TimeBadge не создаёт timers/subscriptions. Нового подтверждённого listener
  leak или квадратичного прохода этих metadata-изменений нет.
- **Лишние I/O и аллокации:** новые attempt baselines, повторные classifiers
  и reviewer baselines читают всю историю. Для N rows и S байтов это
  дополнительные O(N + S) чтения/декодирование/аллокации. Конкатенация зависит
  от retry count, по умолчанию двух retries. Без измерений эти расходы не
  объявлены новым release-critical performance finding. R4-1 имеет отдельное
  доказательство отсутствия ограничения входного размера.

## Ограничения и выполненная проверка результата

Аудит выборочный, преимущественно статический. Не выполнялись Go build/test,
vet/lint, `-race`, широкие suites, benchmarks, искусственная нагрузка,
fault injection, реальные LLM/OAuth/MCP/proxy requests, browser/Playwright E2E,
remote CI inspection и crash-consistency проверки. Не заявляются наблюдённые
Go deadlocks/races/flakes, частота interleavings или зелёный runtime gate.
OAuth refresh проверен до конкретных Rush HTTP clients; аналогичные внутренние
credential chains всех сторонних SDK не сертифицированы.

Выполнены read-only log/diff/blame/status, чтение перечисленных исходников,
выбранных tests и локальных Go/x/net implementations; отдельная проверка F9
описана выше. `git diff --check ccc2f9b5 HEAD` прошёл. Никакие дефекты не
исправлялись в обход запрета изменять код. Рекомендации и acceptance-сценарии
являются условиями последующего исправления, а не выполненными в этом аудите
тестами.

Контроль перед audit commit: status показал только добавленный отчёт и исходный
` D web/dist/.gitkeep`. В staged file list — только
`docs/reviews/2026-09-22-weekly-commit-audit-round5.md`;
`git diff --cached --check` прошёл. Пользовательское удаление остаётся unstaged.
В отчёте нет частных абсолютных путей компьютера или испорченных UTF-8 символов.
Commit ограничен этим Markdown: `docs: add weekly release readiness audit round 5`.

## Минимальные обязательные исправления и финальный release verdict

Для общей alpha с заявленными reviewer, network и web режимами необходимы:

1. **R3-1:** принадлежность attempt/result и отсутствие retry по чужим rows
   для successful/queued/refused outcomes; сохранить recovery собственного stall.
2. **R2-2:** точность всех text fragments, включая whitespace-only, с проверкой
   байтов/значения результата, а не только синтаксической валидности JSON.
3. **R2-4 + R5-1:** корректный phase lifecycle после cancel и сохранение
   временной reviewer identity при credential refresh. Нельзя снимать старые
   ownership guards или персистировать reviewer вместо normal smart slot.
4. **R2-5 + R4-1:** безопасная network diagnostics и ограничение CONNECT head
   до выпуска соответствующих сетевых режимов.
5. **F9:** null-safe render до web alpha.
6. **R5-2:** согласованная маршрутизация OAuth refresh до заявления поддержки
   OAuth-провайдеров исключительно через Rush network policy.
7. **R5-3:** единственный выбранный итог на terse stdout; до исправления
   reviewer-профиль для машинного потребителя должен явно требовать `--json`.

**R2-6** отдельно исправить до использования cache-test как надёжного release
gate. После исправлений требуются целевые regression/acceptance checks, для
concurrency — управляемые барьеры и целевой `-race`, а не случайные reruns.

F6/F7/F8/R3-2/R4-2 допускают отсрочку только с явными ограничениями:
network changes через restart; без обещания durable idle-policy round-trip;
tool inventory из полного transcript; явный HTTP proxy port; ограниченный
custom resolver без обещания fallback на другие адреса. R5-2/R5-3 также можно
исключить из первого профиля через явно объявленные auth-route/JSON условия
выше. При обещании полного набора возможностей нужны соответствующие исправления.
Ограничения здесь не вводились и согласованными с пользователем не считаются.

**Финальный verdict для `a4f99d8b`: NO-GO общей alpha.** Исправленные findings
остаются закрытыми; главным незакрытым P1 является принадлежность retry evidence.
Пятый проход дополнительно установил потерю reviewer-модели на 401, разрыв
network policy на OAuth refresh и смешение фаз в terse stdout. Documentation
commit фиксирует этот срез и требования следующего gate, не разрешает релиз.
