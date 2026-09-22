# Rush: независимый release-readiness аудит, round 9

Дата: **2026-09-22**. Проверенный committed HEAD:
`53e18976093eaf4b1cdbe78dfd756227f608090f`.

## Summary

**Release verdict: NO-GO общей alpha с заявленным набором возможностей.**
Пакет `53e18976` существенно сокращает прежний список: из 20 открытых findings
round 8 конкретные механизмы 16 устранены по исходникам. Для **F6, F8, R4-2,
R8-3** остаются более узкие, подтверждённые ниже случаи. Найдены две новые
регрессии этого пакета: неправильный итог после принятого in-process interrupt
и потеря унаследованного model slot при 401 rebuild.

На проверенном срезе **шесть актуальных findings: один P1 и пять P2**.
Исправленные сценарии не пересчитаны как новые; F3 остаётся историческим
предшественником R2-2, а не отдельным остаточным дефектом.

| ID | Severity | Подтверждённая проблема |
| --- | --- | --- |
| R9-1 | P1 | Успешный live interrupt/replacement возвращается как отменённый исходный turn: recorder сохраняет ID вытесненной попытки. |
| R9-2 | P2 | После 401 непереданный model slot берётся из global config, хотя первоначальная попытка наследовала slot сессии. |
| R8-3, остаток | P2 | Reserved + model-override entry point не записывает result identity; attribution снова зависит от последней строки сессии. Inventory также не ограничен identity. |
| F6, остаток | P2 | Rebuild sub-agent/summarize получает provider и global network из двух snapshots. |
| R4-2, остаток | P2 | Два медленных IPv4 dial могут исчерпать общий бюджет до попытки доступного IPv6. |
| F8, остаток | P2 | Reviewer, отказавший до создания assistant row, снова стирает primary tool inventory из итогового envelope. |

Новых доказанных циклических mutex/channel deadlocks, Go memory races, panic
или неограниченных resource leaks в рассмотренных изменениях не установлено.
Это результат выборочного статического аудита, не доказательство их отсутствия
во всём проекте. Найденные нарушения относятся к принадлежности результата,
жизненному циклу вызова, согласованности конфигурации и failover.

## Срез, метод и сохранность

- Период: **2026-09-15 00:00:00 +02:00 — 2026-09-22 22:29:37 +02:00**,
  предки указанного HEAD, committer date, `Europe/Berlin`.
- База: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23` от 11 сентября.
  Первый коммит периода — 19 сентября. `--since` и `--since-as-filter`
  дают **43 коммита**. Совокупный diff: **128 файлов, 16574 добавления,
  1338 удалений**, включая документацию и тесты.
- После проверенного round 8 `d67a9c7e` появились отчёт `abc98c00` и
  исправления `53e18976`. Последний изменяет 53 файла; помимо перечисленных
  в его заголовке fixes он переносит quota-reset parsing из ping в agent и
  добавляет локальное время reset в диагностические details.
- Изучены log, состав недельного diff, ключевые production-diff, committed
  реализации, вызывающие пути и соответствующие тесты. Код для выводов
  читался через `git show HEAD:<path>` и сверялся с фиксированным HEAD.
  Все номера строк ниже относятся к **`53e18976`**, не к старым отчётам.
- Доказательства — исходники и допустимые последовательности событий.
  Предлагаемые acceptance-сценарии **не исполнялись**. Исторические заявления
  commit/checkpoint о зелёных тестах не считаются проверкой этого прохода.
- Фактический `git worktree list --porcelain` показал один checkout на
  `main`, а не отдельный изолированный worktree. Другой checkout не создавался,
  ветки не переключались, sub-agents не запускались. Это ограничение среды;
  область записи осталась единственным разрешённым отчётом.
- На входе полный status содержал только **` D web/dist/.gitkeep`**;
  index был пуст. Перечисленного в запросе modified/untracked source-пакета
  на входе уже не было: изменения, обсуждавшиеся в round 8 как WT,
  теперь находятся в committed `53e18976`. Они проверяются как часть HEAD.
  Удалённый `.gitkeep` не восстанавливался, не добавлялся в index и не
  включается в documentation commit. Отдельного WT-verdict для исходников
  здесь нет, поскольку такого отличающегося кандидата не обнаружено.

Сопоставлены все восемь предыдущих отчётов:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md),
[round 6](2026-09-22-weekly-commit-audit-round6.md),
[round 7](2026-09-22-weekly-commit-audit-round7.md),
[round 8](2026-09-22-weekly-commit-audit-round8.md).

### Карта недельной серии

| Коммиты | Проверяемая область |
| --- | --- |
| `57fffb05`, `2169dbe3`, `b0a55aab`, `4b2b2a0c` | Retry/continuation, admission, принадлежность и синхронизация evidence. |
| `ccb510ff`, `babd295c`, `54acbf71`, `8aabba4d` | Reviewer, credentials, context/ownership, reconciliation, точность текста. |
| `0f445020`, `eb59df12`, `fd18cf0c`, `d0966df9` | Network config, provider wiring, proxy/DNS/DoH, deadlines, cache/cleanup. |
| `f7362f7f` | Timeout defaults, idle backstop, watchdog и durable conversion. |
| `14dd4a54`, `924add8a`, `10e32c56`, `97c22050` | Tool formatter, sub-agent metadata, timestamps и web tests. |
| `4357e157`, `fbc8a177`, `a275e323` | Peak-hours message, UI labels, schema/release metadata. |
| `87ee064f`, `64409ab3`, `180f1544`, `5c20c771`, `c9eb9a98`, `c32c72d6`, `1420acb1`, `52a456b8` | MCP test isolation, context/lint fixes и разделение tests. |
| `53e18976` | Перепроверка каждого исправляемого finding и соседних error/cancel/rebuild путей; новые recorders, quota guidance. |
| Остальные 14 коммитов | Восемь прежних отчётов и шесть checkpoints; production-код не меняют. |

P1 означает существенное нарушение результата поддерживаемого сценария,
блокирующее его выпуск. P2 — ограниченный дефект, требующий исправления либо
явно принятого ограничения соответствующего режима. P0 не установлен.

## Новые findings

### R9-1. P1 — live replacement завершается успешно, но ExecuteRun возвращает отменённый ответ

**Коммит регрессии:** `53e18976`, result recorder из исправления R8-3.
**Места:** `internal/agent/coordinator_run.go:108,313,467,475`,
`internal/agent/coordinator_interrupt.go:380,401`,
`internal/agent/agent_run.go:570,576`,
`internal/agent/agent_turn_failure.go` (`handleStreamFailure`,
ветка `drainAfterCancel`),
`internal/app/app_run_terminal.go:95`,
`internal/app/app_run_reviewer.go:470,636`.

Recorder использует ID **attempt evidence** исходного `SessionAgentCall`.
Но `sessionAgent.runOwned` — dispatcher: он может принять разрешённый
interrupt/replacement, выполнить следующий call и вернуть результат именно
этого последнего turn. Новый `InterruptAndSend` call строится через `buildCall`
и не содержит `OnAssistantMessageCreated` исходного coordinator attempt.
Его строки поэтому не обновляют `curAttempt` исходного вызова.

Достаточная последовательность без гонки за освобождённую сессию:

1. A выполняется через `ExecuteRun → Run/RunWithOverrides → runInternal`;
   уже создана assistant row `mA`, callback записал её ID.
2. Законный in-process `InterruptAndSend` принимает replacement B и отменяет
   только текущую generation. Родительский context A остаётся действующим.
3. Error path A сохраняет на `mA` finish `canceled`, забирает B через
   `drainAfterCancel` и возвращает `hasNext=true`. `runOwned` исполняет B
   под продолжающимся владением. B записывает `mB/end_turn` и успешно возвращает
   ненулевой result. `runOwned` возвращает этот result и nil error.
4. У A `curAttempt.resolve()` всё ещё равен `mA.ID`. Retry classifiers
   правильно не повторяют canceled row, но новый call-result recorder
   передаёт этот устаревший ID в app.
5. Reconciliation обязано выбрать `mA`, хотя B завершено. В JSON получается
   partial A с `exit_reason="canceled"` и `runIncompleteError`; terse также
   выбирает A. Успешный ответ B остаётся только в transcript.

Это регрессия относительно `abc98c00`: прежний session-wide scan в этом
конкретном dispatcher выбирал последнюю `mB`. Это **не** предложение вернуть
session-wide выбор: он ошибочен при независимом последующем владельце R8-3.
Нужно различать принятую замену внутри dispatcher и чужой новый invocation.
Сценарий не относится к durable `DrainSessionNow`: для него новый код явно
сбрасывает recorder, а live replacement приходит прямо из `runOwned` с nil error.

Риск — неверный статус и потеря результата уже выполненной replacement-работы
для вызывающего кода. Повторное исполнение её side effects не утверждается
как обязательное, но consumer больше не может доверять возвращённому outcome.
Затронут host, совмещающий ExecuteRun с in-process interrupt; обычный CLI
cross-process durable inject не объявляется этим примером сломанным.

**Рекомендация:** передавать terminal identity из фактического результата
dispatcher, включая lineage принятых replacements. Не использовать
retry-evidence ID как универсальный итог всей dispatcher-жизни.
Acceptance: барьер после создания `mA`, live interrupt, успешное завершение B,
проверка B в JSON/terse и отсутствия canceled status; отдельно оставить
проверку «A завершён, независимый B стартовал после release».
`app_run_r8_test.go` покрывает второй случай, но не live replacement.

### R9-2. P2 — 401 rebuild теряет model slot, унаследованный из сессии

**Коммит:** `53e18976`, новая ветка `resolveCallModels`.
**Места:** `internal/agent/credentials.go:352,354,355`,
`internal/agent/coordinator_run.go:354,609,613,616,627`,
`internal/agent/coordinator_models.go:348,350,352,363`,
`internal/app/app_run_setup.go:360,362`,
`internal/app/app_run_reviewer.go:825`.

Первоначальный `RunWithOverrides` дополняет отсутствующий smart/fast override
из durable slots сессии. Это локальные переменные метода: дополненная пара
не записывается в context. После 401 новый `resolveCallModels` берёт из context
исходную неполную пару и сразу вызывает `applyModelOverrides`, минуя это
наследование. Для nil slot последний начинает с **global** `cfg.Models`.

Статический сценарий: session smart = S, global smart = G, S != G;
ExecuteRun без fail-fast reservation задаёт **только FastModel=F**.
Context содержит `(nil,F)`, но первая попытка правильно строится как `(S,F)`.
Провайдер S возвращает 401; поддерживаемое обновление OAuth/APIKeyTemplate
успешно. Rebuild получает `(nil,F)` и строит **`(G,F)`**. Повторный provider
запрос теперь выполняется на G, хотя smart slot сессии не менялся.
Не нужны concurrent reload или ошибки БД. Без доступного credential refresh
этого повторного запроса нет.

Симметричный случай automatic reviewer: `(R,nil)` первоначально наследует
session fast F, а после 401 строится с global fast. Сам reviewer R уже
сохраняется — старый точный R5-1 «R заменился на S» закрыт. Новая регрессия
касается **пропущенного соседнего slot**, включая обычный one-slot override.
Речь об ошибке выбора модели/провайдера; межпользовательская утечка credentials
этим доказательством не установлена.

**Рекомендация:** сохранять полностью разрешённую пару model identities и
effort на всё время вызова, обновляя при 401 только credentials; либо применять
одинаковое session inheritance при первой сборке и rebuild. Не персистировать
временный reviewer. Acceptance: разные global/session smart и fast, отдельно
smart-only и fast-only override, 401 → refresh → повтор, проверка обеих slots
и фактической модели второго provider request.

## Подтверждённые остатки прежних findings

### R8-3. P2 — reserved override не участвует в новом result recorder

**Коммиты:** исходный consumer — `ed744c17` от 9 сентября, вне недели;
недельная интеграция `ccb510ff`, частичное исправление `53e18976`.
**Места:** `internal/app/app_run.go:435,436`,
`internal/agent/coordinator_run.go:683,719,729`,
`internal/agent/coordinator_call_result.go:39`,
`internal/app/app_run_terminal.go:75,83,95`,
`internal/app/app_run_reviewer.go:234,250,470`.

Обычный `runInternal` теперь записывает ID до возврата; прежний сценарий
round 8 для этого пути исправлен. Но `FailIfSessionBusy=true` вместе с model
override вызывает **`RunWithReservedOwnership`**, который строит call через
`buildCall` и сразу вызывает session agent, вообще не проходя `runInternal`.
В нём нет записи `CallResultRecorder`; context с recorder сам по себе ничего
не записывает. Утверждение комментария о едином funnel не охватывает этот
production entry point.

A с указанными options успешно завершает turn и освобождает ownership.
До его финального чтения истории законный B завершает другой turn той же
сессии. Recorder A пуст; условие `ownID == "" || msg.ID == ownID` выбирает B.
A получает текст/finish B. Fail-fast запрещает пересечение **владения**, но
не исключает этот порядок после release. Это тот же R8-3, а не новый finding.
Существующий новый тест использует обычный Run, без reserved model override.

Дополнительно scope результата остаётся неполным даже при непустом ownID:
tool inventory собирается на строках 83–93 **до** identity-фильтра terminal.
Поэтому инструмент B может попасть в `ToolCalls` A при правильном `FinalText` A.
Пустой recorder также допускается в cancel probe до возврата `runInternal`,
а после durable drain сбрасывается явно; эти ветви нельзя считать защищёнными
по одному тесту обычного завершения. Частота проявления не измерялась.

**Рекомендация:** единый outcome/identity contract для всех execution entry
points, cancellation и принятого drain; пустой ID не должен означать
«любая новая строка моя». Привязать и inventory к принятой цепочке call IDs.
Acceptance: повторить барьерный A-release → B-complete → A-reconcile на
reserved+override пути, дать B собственный tool call; дополнительно проверить
cancel до публикации recorder и genuine cancel-after-commit.

### F6. P2 — snapshot исправлен в обычной сборке, но не в pinned rebuild

**Коммиты:** network wiring `eb59df12`; неполное исправление `53e18976`.
**Места:** `internal/agent/coordinator_providers.go:417,423,424,443,444`,
`internal/agent/coordinator_subagents.go:208,212`,
`internal/agent/coordinator_summarize.go:118,122`,
`internal/agent/coordinator_providers_network.go:27,28`.

`buildModelsFromCfg` действительно передаёт один cfg своим provider builders.
Но sub-agent/summarize refresh сначала вызывает `currentProviderConfig`,
который захватывает snapshot A и возвращает только ProviderConfig. Затем
`rebuildPinnedModel` самостоятельно читает snapshot ещё раз. При reload между
этими операциями он передаёт `buildProvider(cfgB, providerA, ...)`.
Комментарий «One atomic snapshot» на строке 422 не делает эти чтения едиными.

Например, A задаёт global proxy PA и provider DNS DA; B одновременно меняет
их на PB/DB. Rebuild получает **PB/DA**, хотя ни одного такого поколения
config не было. Это реальный production путь после refresh sub-agent либо
summarize, а не прямой неправильный вызов internal helper из теста.
Это логическая гонка snapshot, не несинхронизированный read/write памяти Go.

**Рекомендация:** возвращать provider вместе с тем же cfg/resolved network
либо выбирать оба внутри одного rebuild snapshot. Аналогично проверить auth
boundary: `refreshOAuth2Token:157` сейчас тоже получает старый `providerCfg`
и отдельно читает live cfg; этот родственник не считается вторым finding.
Acceptance с двумя контролируемыми snapshots должен покрывать реальные
refresh/rebuild callers, а не только чистую функцию network merge.
Временное ограничение — менять network policy через restart, без hot reload.

### R4-2. P2 — address list восстановлен, но IPv6 может не получить времени на dial

**Коммиты:** `0f445020`, `eb59df12`; текущий fallback — `53e18976`.
**Места:** `internal/nettransport/client.go:30,35,154,174,180,183,201`,
`internal/nettransport/resolver.go` (`plainDNSResolver`, `newDoHResolver`,
`dnsOverTCPResolver`).

Старые случаи закрыты: resolver возвращает список, DoH и DNS/TCP запрашивают
обе families, быстрый отказ первого адреса допускает следующий. Но список
упорядочен «все A, затем AAAA», общий dial budget равен **60s**, а каждый
direct TCP dial получает до **30s** без разделения оставшегося бюджета между
адресами. Family race или ускоренного перехода к IPv6 нет.

Для ответа `[A1,A2,AAAA1]`, где A1 и A2 не отвечают на соединение, а AAAA1
доступен, первые два dial исчерпывают общий срок. Проверка `dialCtx.Err()`
останавливает цикл до AAAA1. При стабильном DNS-порядке следующие retries
повторяют тот же отказ. Даже локально доступный третий адрес не испытывается.
Здесь не заявляется отсутствие любого fallback: дефект касается медленных
отказов и недоступной первой family, а не `connection refused`.

`TestResolvedDialerFallsBackAcrossAddresses` проверяет два IPv4 loopback адреса
с быстрым отказом первого. Тесты на две DNS families проверяют получение
records, а не фактическую возможность дойти до IPv6 при медленных A.
Сетевая blackhole-проба и ожидание production 60s в этом аудите не выполнялись.

**Рекомендация:** распределять оставшееся время между кандидатами либо
использовать bounded family racing, сохраняя общий deadline и proxy policy.
Acceptance должен моделировать задержанные ошибки первой family и доступный
последующий адрес; только наличие AAAA в slice недостаточно. Это продолжение
R4-2, не повтор закрытого «резолвер всегда возвращает один IP».

### F8. P2 — early refusal reviewer теряет сохранённый primary inventory

**Коммиты:** `ccb510ff`; частичное исправление `53e18976`.
**Места:** `internal/app/app_run_reviewer.go:379,400,401,475,608,739`,
`internal/agent/coordinator_run.go:217,219`.

Новый `invocationToolCalls` правильно объединяет две успешно reconciled фазы.
Но reset создаёт пустой `toolCallCounts`; при отказе reviewer до первой
assistant row reconciliation возвращает ошибку. `foldLiveToolCallCounts`
при `len(s.toolCallCounts)==0` сразу возвращает управление, не восстанавливая
ссылку на уже заполненный `invocationToolCalls`.

Сценарий: primary действительно выполняет `view` и завершается; reviewer
на другом provider отказан его активным peak-hours window до запуска turn.
В новой фазе нет tool events/assistant rows, все primary rows уже в baseline.
Итоговый JSON корректно содержит ошибку reviewer, но `ToolCalls` пуст,
несмотря на `invocationToolCalls["view"]==1` и учтённую стоимость primary.
Тест `TestExecuteRunReviewerPassToolCallsSpanBothPhases` рассматривает
успешный reviewer с terminal row и эту ветку не покрывает.

**Рекомендация:** финальный envelope всегда читает run-wide inventory,
включая пустую/отказавшую вторую фазу; phase events дополняют его при необходимости.
Acceptance: primary tool → reviewer pre-turn refusal/build error, затем
проверка неизменного primary count и ненулевого error outcome.

## Подтверждённые и опровергнутые findings прежних раундов

«Закрыт» означает устранение конкретного механизма по committed-коду.
Это не утверждение о новом успешном runtime acceptance. Историческое
существование findings не опровергается; опровергается их актуальность после
исправлений. Полностью ложного исторического finding здесь не установлено.

| Finding | Статус на `53e18976` и основание |
| --- | --- |
| F1 | Закрыт `babd295c`: `resetForReviewerPass:721` присваивает reviewer ctx; `buildReviewerPassTurn:826,827` очищает persistence и reservation. Старый smart toolset/stale token сценарий не действует. |
| F2 | Закрыт `babd295c`: `app_run.go:519` исключает `req.Credentials != nil` из configured auto-review. |
| F3 / R2-2 | Закрыт по всем ранее показанным контрпримерам: `babd295c`, `54acbf71`, `8aabba4d`, затем `53e18976`; `continuationChainText:170,171` сохраняет whitespace-only fragment, `joinContinuationText:207` соединяет без вставки/trim. |
| F4 | Закрыт в прежнем объёме `fd18cf0c`/`d0966df9`: CONNECT/DNS guards, DoH exchange timeout, TLS/SOCKS budgets. Initial dial отдельно закрыт как R6-1. |
| F5 | Закрыт в прежнем объёме `fd18cf0c`: main/DoH idle timeout 90s, cache capacity 8, eviction вне lock, forwarding CloseIdleConnections. Полное синхронное закрытие каждого SDK pool этим не доказано. |
| F6 | Частично закрыт `53e18976`; остаток двух snapshots в pinned rebuild описан выше. |
| F7 | Закрыт `53e18976`: `session.CallOptionsSpec.IdleTimeout`, `call_data_conversion.go:101,141`; zero/positive/disabled проходят обе стороны JSON mirror. Версия старых записей не менялась, отсутствующее поле даёт ноль. |
| F8 | Частично закрыт `53e18976`: primary inventory сохраняется при обычном reviewer terminal; early refusal остаётся выше. |
| F9 | Закрыт `53e18976`: `web/src/toolFormat.ts:19` проверяет null/scalar/array до индексирования; `sanitizeToolInput` дополнительно нормализует bare null. |
| R2-1 | Закрыт `d0966df9`: bounded `socksDialer` применяется также во вложенном DoH client. |
| R2-3 | Закрыт `54acbf71`: reviewer переносит `FailIfSessionBusy`, mailbox отказывает атомарно без enqueue. |
| R2-4 | Закрыт `53e18976`: gate проверяет cancel, `resetForReviewerPass:729` очищает terminal/context/cancel вместе. |
| R2-5 | Прежние доказанные build/non-200/runtime DoH URL leaks закрыты `53e18976`: redacted URL, unwrap `url.Error` в `dohQuery`, redaction generic finish. Это не сертификат отсутствия любых секретов в любом provider error body. |
| R2-6 | Закрыт `53e18976`: `TestTransportCacheEvictionReleasesIdleConns:675` использует отдельный `newTransportCache()`, недопустимая зависимость от parallel global inserts устранена. |
| R3-1 | Прежние P1 закрыты `b0a55aab`/`4b2b2a0c`: own ID, отдельный capture на attempt, mutex/seal. Новые result-consumer дефекты не означают возврата той Go race. |
| R3-2 | Закрыт `53e18976`: `proxyDialAddr` использует Hostname/JoinHostPort и default port 80, включая IPv6. |
| R4-1 | Закрыт `53e18976`: CONNECT head ограничен LimitedReader; после parse лимит снят, buffered payload сохранён. |
| R4-2 | Частично закрыт `53e18976`: список/обе families восстановлены, распределение dial budget остаётся проблемой. |
| R5-1 | Точный reviewer R → 401 → S сценарий закрыт `53e18976`: `WithModelOverrides` сохраняет R при rebuild. Новая потеря соседнего унаследованного slot выделена в R9-2. |
| R5-2 | Прежние default-route refresh и downgrade при build failure закрыты `53e18976`: `RefreshOAuthTokenWithClient`, early return на clientErr, сохранённый refresh timeout. Несогласованность snapshots учитывается в F6, не вторично здесь. |
| R5-3 | Закрыт `53e18976`: terse печатается единожды через `flushTerseOutput` после reviewer gate; общий defer добавляет newline. |
| R6-1 | Закрыт `53e18976`: HTTP proxy-only и direct/HTTP-proxy DoH задают DialContext с собственным 30s TCP timeout. |
| R6-2 | Закрыт `53e18976`: оба watchdog clamp применяются только при `hardCap > 0`; ноль больше не означает start+idle absolute cap. |
| R7-1 | Закрыт `53e18976`: отдельный admission recorder помечает queued до возврата; `coordinator_run.go:546` исключает classification. Recorder отсоединён от downstream ctx, ранний callback не меняет queued outcome. |
| R8-1 | Закрыт `53e18976`: `configureProviders`, `load_providers.go:132`, переносит provider Network в catalog merge; исходный Load/Reload пропуск поля устранён. |
| R8-2 | Точный `tool_use → unfinished step → cancel` случай закрыт `53e18976`: подавление cancel в `finish:495` ограничено `end_turn`; stale tool_use label очищается. Принадлежность выбранного end_turn остаётся отдельным R8-3. |
| R8-3 | Частично закрыт `53e18976`: обычный завершённый runInternal защищён; reserved entry point и scope inventory — остатки выше. |

## Проверенные области без дополнительных findings

- **Locks, re-entrancy, ABA.** Новые admission/evidence/result mutex защищают
  только значения; I/O и channel wait под ними не добавлены. Assistant callback
  вызывается после unlock `turnStream.mu`. Cache eviction закрывает idle pools
  после unlock. One-shot reservation и epoch/state guards сохранены; live drain
  продолжает dispatcher, не захватывая повторно тот же session OS lock.
  Новый цикл взаимного ожидания по этим изменениям не установлен.
- **Channels, panic, cancellation.** `done`/`drainDone` имеют buffer 1;
  закрытая subscription переводится в nil. Runner panic превращается в error
  response; retry backoff выбирает также ctx.Done. Interrupt ticker отменяется
  до join. ExecuteRun имеет deferred cancel. Это не гарантия завершения
  произвольного provider/tool, игнорирующего cancel; terminal attribution
  и replacements отдельно отражены в findings.
- **Сетевые ресурсы.** CONNECT error paths закрывают conn; guard joins watcher
  до handoff и снимает provisional deadline. DNS/TCP закрывает connection,
  DoH закрывает body. Лимит CONNECT head не распространяется на дальнейший
  tunnel payload. Нулевой network config выходит до обращения к proxyURL;
  resolver-only проходит отдельную ветку, поэтому предполагаемая nil panic
  на `rs.proxyURL.Scheme` не подтверждается.
- **Credentials/изоляция.** Configured reviewer исключён для credentialed
  calls; reviewer context переносит folder/disk scope и allowlist. Общий
  transport не переносит Authorization между запросами сам по себе. TLS
  остаётся на исходном hostname поверх resolved connection. Проверенные
  redaction/fail-closed fixes нельзя объявлять неполными только на основании
  прежних контрпримеров: их ветви теперь изменены.
- **Watchdog и durable options.** Idle threshold достигает turn, положительное
  значение исключает transparent idle-stall retry, mirror сохраняет значение.
  Extension без hard cap теперь следует activity. Tool cap и явно заданный
  hard cap остаются отдельными механизмами; timer-driven фиктивная activity
  не добавлена. MaxCost/MaxTokens продолжают читать session totals.
- **Quota/peak-hours.** Прослежены перенос reset-parser из ping и вызов guidance
  из provider-error branch, config → CLI/WS/frontend для peak-hours message.
  Просмотренные production calls reset-parser передают ненулевой error;
  искусственный прямой вызов exported helper с nil не объявлен release panic.
  Provider timezone assumptions и реальные заголовки всех провайдеров этим
  проходом не сертифицированы. Operator message не становится shell command.
- **Web.** Formatter guard закрывает исключение на null для обеих consumers.
  Metadata остаётся в существующем проходе messages/parts, TimeBadge не
  добавляет per-message timer. В изменённых render/mapping ветках нового
  доказанного listener leak или release-critical квадратичного обхода нет.
- **MCP/processes.** Недельные изменения локализуют env теста, используют
  directory identity и конечные waits. Production process-tree cleanup этой
  серией не менялся; увеличение test timeout не засчитано как доказательство
  устранения всех flakes. MCP servers и process-test helpers в этом аудите
  не запускались.
- **Сложность/I/O.** Classifiers и reconciliation читают историю O(N+S), где
  S — объём текста; двойной baseline read reviewer и повторные Lists сохранены.
  Склейка `acc + next` копирует накопленный префикс, но при default ограничении
  retries самостоятельный release-critical slowdown без измерений не доказан.
  Новый сбор обеих DNS families добавляет запросы; найденный release-relevant
  недостаток времени на fallback учтён в R4-2. Concurrent cache misses всё ещё
  могут построить разные transports, однако их idle lifetime ограничен;
  прежний бесконечный F5 на этом основании не переоткрывается.

## Ограничения и контроль результата

Build, Go tests, `-race`, lint/vet, benchmarks, browser E2E, сетевые fault probes,
реальные LLM/OAuth/MCP запросы и искусственная нагрузка **не запускались**.
Изменяется только документация. Содержательные acceptance-тесты прочитаны
для оценки границ покрытия; их прохождение в этом аудите не заявляется.
Новых наблюдавшихся test failures/flakes нет, поскольку исполнения tests не было.

Не проверены все возможные interleavings, каждый provider SDK, все платформы
process cleanup, полное DNS protocol conformance, crash recovery и внешнее CI.
Число/частота runtime проявлений findings не измерялись. Предлагаемые барьеры
и сценарии — критерии последующей целевой проверки, не отчёт об их запуске.

Выполнены `git diff --check ccc2f9b5 53e18976` и проверка исходного рабочего diff.
Перед staging проверены неизменность HEAD, пустой index, сохранение
` D web/dist/.gitkeep` и единственное добавление — этот Markdown. Commit
ограничивается явным pathspec отчёта с последующей проверкой staged diff/check
и фактического file list commit. Код, версии, CHANGELOG, конфигурация и
пользовательские source-файлы не редактировались. Частные пути компьютера
в отчёт не включены. Push не выполняется.

## Минимальный gate и финальный release verdict

1. **R9-1 + R8-3:** передавать принадлежащий выполнению итог через все entry
   points, отличая принятую замену от независимого следующего владельца.
   Проверить live interrupt, reserved+override, cancel-before-recorder и
   обычный A/B post-release сценарий; вернуть правильные text/status/inventory.
2. **R9-2:** сохранить полностью разрешённую пару моделей при 401 rebuild.
   Проверять оба one-slot overrides и временный reviewer, включая effort.
3. **F6:** устранить повторный snapshot в refresh/rebuild цепочках перед
   обещанием согласованной hot-reload network policy. Ограничение «network
   changes требуют restart» возможно только как явно принятый release scope.
4. **R4-2:** дать доступной следующей family шанс внутри общего dial budget.
   **F8:** сохранять primary tool inventory при early reviewer refusal.
   До исправления можно лишь явно ограничить обещания failover/accounting;
   такие ограничения этим аудитом не вводятся и принятыми не считаются.

После правок нужны целевые acceptance-проверки соответствующих переходов;
для concurrency — контролируемые барьеры и уместный race-check. Обычный serial
happy path, наличие нового поля/recorder или заголовок fix-коммита не заменяют
проверку конкретного перехода.

**Итог для `53e18976` / `0.2.0-alpha.2`: NO-GO общей alpha.** Большая часть
прежнего блокирующего списка устранена, но завершённый вызов ещё может
вернуть не свой либо уже вытесненный результат. Этот documentation commit
фиксирует проверку и не включает исправления кода или разрешение на выпуск.
