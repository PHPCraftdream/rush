# Аудит готовности Rush к alpha: коммиты за 14–21 сентября 2026

## Summary и release verdict

**Вердикт: NO-GO для alpha с полным заявленным набором возможностей текущего HEAD.**
Найдены пять P1 и четыре P2. До alpha необходимо исправить F1–F5:
контекст автоматического reviewer, изоляцию переданных credentials, потерю начала
ответа при continuation, незавершающийся сетевой I/O и владение HTTP-транспортами.
Исправления в рамках этого аудита не выполнялись.

Классического нового deadlock с циклом mutex или взаимным ожиданием каналов в
проверенных изменениях не установлено. Подтверждены другие механизмы, внешне
похожие на зависание: неограниченное ожидание ответа CONNECT/DNS и сохранение
сетевых ресурсов после отмены. Это **не** доказательство отсутствия deadlock или
data race во всём Rush. Новых доказанных гонок памяти Go в выбранных изменениях
не найдено; есть доказанные ошибки жизненного цикла, stale context, смешение
поколений конфигурации и потеря данных на границах фаз/сериализации.

| ID | Severity | Проблема | Решение для релиза |
| --- | --- | --- | --- |
| F1 | P1 / высокая | Reviewer запускается со старым контекстом; возможен stale ownership и неверный toolset | Исправить до alpha |
| F2 | P1 / высокая | Автоматический reviewer выходит за границу `RunWithCredentials` | Исправить до alpha |
| F3 | P1 / высокая | Успешный continuation возвращает только окончание ответа | Исправить до alpha |
| F4 | P1 / высокая | CONNECT/DNS/DoH могут удерживать I/O и goroutine после отмены | Исправить до alpha |
| F5 | P1 / высокая | Созданные HTTP/DoH-транспорты не имеют полного lifecycle; соединения накапливаются | Исправить до alpha для web/SDK |
| F6 | P2 / средняя | Настройки сети берутся из другого поколения config, чем provider | Можно отложить при отсутствии hot reload |
| F7 | P2 / средняя | `IdleTimeout` исчезает при durable round-trip | Можно отложить только с явным ограничением durable replay |
| F8 | P2 / средняя | Итоговый reviewer-envelope теряет tool counts основной фазы | Можно отложить при чтении полного transcript |
| F9 | P2 / средняя | JSON `null` в аргументах инструмента ломает render sub-agent блока | Можно отложить для CLI-only alpha; исправить перед широкой web alpha |

P1 означает существенный сбой поддерживаемого сценария либо нарушение изоляции,
которое нельзя оправдать статусом alpha. P2 означает ограниченный сценарий с
обходным путём; отложить его можно только осознанно, с описанным ограничением.
P0 в рамках этой проверки не установлен.

## Диапазон, состояние дерева и метод

- Дата аудита: **2026-09-21**. Нижняя граница: **2026-09-14 00:00:00 +02:00**
  (`Europe/Berlin`). Срез по committer date, только предки исходного HEAD.
- Проверяемый HEAD: `f7362f7fe0f03f685556d20be8b312a2076df947`,
  **2026-09-20 14:57:04 +02:00**. Коммитов 21 сентября в этом срезе нет.
- База перед периодом: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  **2026-09-11 14:08:09 +02:00**.
- Диапазон: `ccc2f9b5..f7362f7f`, **15 коммитов, 50 файлов,
  4701 добавленная / 641 удалённая строка**. Проверка `--since-as-filter`
  дала тот же набор; author/committer dates выбранных коммитов совпадают.
- Изучены история и состав каждого коммита, совокупные production-diff,
  соответствующие текущие исходники, вызывающий код и выборочные тесты.
  Строки ниже относятся к **исходному проверяемому HEAD**, не к будущим исправлениям.
- Подтверждение Go-находок — статическая трассировка вызовов и состояний;
  предлагаемые ниже Go-сценарии воспроизведения **не запускались**. Существующие
  Go-тесты прочитаны, но не исполнялись. Build, lint, `-race`, полные suites,
  реальные LLM/MCP и браузерные E2E не запускались.
- Для F9 выполнена отдельная проверка фактической функции через Node 24.12.0:
  TypeScript-типы удалены в памяти встроенным `stripTypeScriptTypes`, исходник
  не переписывался. Никакие зависимости не устанавливались.
- Для F4/F5 дополнительно прочитан локальный исходник `net/http` из Go 1.26.3:
  `Transport.getConn`, `CloseIdleConnections`, установка idle timer. Это
  проверка реализации используемого toolchain, не предположение о cancel.
- `git diff --check ccc2f9b5..f7362f7f` завершился успешно. Это проверяет
  whitespace, а не корректность конкурентного исполнения.

Исходно tracked-файлы и index были чистыми. Два существовавших пользовательских
untracked-теста сохранены; они не являются тестовым покрытием проверяемого HEAD:

| Файл | Исходный SHA-256 |
| --- | --- |
| `web/tests/message-timestamp-always-visible.spec.ts` | `CE416416F90632CD972DE8F262785E576BC784888F881F9C9811C5BA0952F891` |
| `web/tests/subagent-block-metadata.spec.ts` | `CB72A0EB814462FB64EA7655DBE0B826905FEE79060ED2135ED57E9F22611678` |

Единственное изменение аудита — этот отчёт. Код, конфигурация, версии, CHANGELOG
и пользовательские тесты не изменялись. Проверка ограничена предоставленным
локальным checkout; состояние удалённой ветки и актуальный CI не запрашивались.

## Карта коммитов

| Коммит | Изменение и предмет проверки |
| --- | --- |
| `57fffb05` | Continuation после частичного ответа: retry, сохранение history, итоговый результат |
| `14dd4a54` | Общий formatter аргументов и timestamps sub-agent инструментов |
| `924add8a` | Модель/effort sub-agent: derivation из сообщений |
| `10e32c56` | Всегда видимый timestamp сообщения |
| `87ee064f` | Windows MCP: локализация переменной окружения теста |
| `64409ab3` | MCP: сравнение каталогов по identity, расширение ожидания барьеров |
| `df0051b1` | Checkpoint; runtime-изменений нет |
| `ccb510ff` | Reviewer-pass, выделение event loop, ownership/context, gate и тесты |
| `0f445020` | NetworkConfig, proxy, DNS, DoH и сетевые doubles |
| `85ee8836` | Checkpoint; runtime-изменений нет |
| `eb59df12` | Встраивание network client в providers, debug/Copilot, schema |
| `180f1544` | Context-aware API в сетевых тестовых doubles |
| `5c20c771` | Context-aware HTTP request в provider-network тесте |
| `bf9f6d3d` | Checkpoint; runtime-изменений нет |
| `f7362f7f` | `--timeout=0`, `--idle-timeout`, per-call override и запрет stall retry |

Checkpoint-документы использованы только как описание намерений и прежних
проверок. Их заявления о зелёном CI и исправленных проблемах не считаются
независимым подтверждением этого аудита.

## Findings: исправить до alpha

### F1. P1 — reviewer context построен, но не передан в выполнение

**Коммит:** `ccb510ff`; сохранено в `f7362f7f`.
**Локализация:** `internal/app/app_run_reviewer.go:571`
(`executeRunLoop.resetForReviewerPass`), `:109` (`runTurnPhase`),
`:619` (`buildReviewerPassTurn`); `internal/app/app_run.go:435` и финальный
reviewer gate.

`buildReviewerPassTurn` создаёт `reviewCtx` с ролью reviewer, запретом sub-agents,
очищенными model-persistence и reserved-ownership. `resetForReviewerPass(ctx)`
использует переданный ctx только для `Messages.List`, **не присваивает `s.ctx`**.
Следующая фаза запускает `runAgentTurnRecovered(s.ctx, ...)` со старым ctx.
Closure `reviewRunFn` использует аргумент ctx, а не захватывает `reviewCtx`.

Отсюда два конкретных результата:

1. При настроенном worker вторая фаза продолжает видеть `ModelRole=smart`.
   `buildToolsAgentConfigForCall` и `applyCallDisableSubAgents`
   (`internal/agent/coordinator_tools.go:224`, `:276`) сохраняют поведение
   orchestrator: разрешают worker-delegation и убирают direct edit tools,
   вместо предусмотренного reviewer toolset.
2. При `FailIfSessionBusy=true` и явном model override основная фаза проходит
   через `RunWithReservedOwnership`. Этот путь не вызывает `reservedOwnership.claim()`
   у токена в ctx. После завершения эпоха освобождена, но токен остаётся
   unclaimed. Reviewer делает обычный `Run`, claim успешно забирает **старый**
   токен, а `mailbox.rebindDispatcher` отвергает уже idle/изменённую эпоху.
   Возвращается `pre-reserved ownership ... is no longer valid`.
   Проверка: `internal/agent/agent_run.go:73`, `:580`;
   `internal/agent/mailbox_generation.go:157`.

**Воспроизведение:** настроить reviewer; выполнить `ExecuteRun`/SDK Run с
`ModelRole="smart"`, явным `SmartModel`, `FailIfSessionBusy=true` и успешным
локальным provider stub. Основная фаза завершается, reviewer падает до своего
LLM-запроса на stale reservation. Отдельный сценарий с worker должен проверять
отсутствие `agent` в запросе reviewer, а не только имя модели.

**Покрытие:** `TestExecuteRunReviewerPassContinuesCleanSmartRunWithReviewerModel`
проверяет смену модели и текст, но helper не включает fail-fast reservation,
не задаёт worker и не сравнивает toolset. Поэтому этот happy path не обнаруживает
потерянный контекст. Статическая трасса достаточна для установления дефекта;
запуска указанного сценария в этом аудите не было.

**Рекомендация:** присваивать контекст новой фазы до её запуска; явно сбрасывать
всё terminal/cache-состояние между фазами и не начинать reviewer после отмены
родительского ctx. Добавить проверки role/toolset и fail-fast + override.
Не ослаблять epoch/state guard mailbox: он корректно обнаруживает stale token.

### F2. P1 — автоматический reviewer обходит per-call credentials

**Коммит:** `ccb510ff`.
**Локализация:** `internal/app/app_run.go:444` (credentials runner и последующий
reviewer gate), `internal/app/app_run_reviewer.go:620`, `:642`
(`buildReviewerPassTurn`); контракт `sdk/sdk.go:563`
(`Client.RunWithCredentials`).

Основная фаза с `req.Credentials` вызывает `RunWithCredentials` через отдельный
`runFn`. После её успеха gate смотрит только роль и **глобальный** reviewer slot.
Вторая фаза безусловно вызывает `RunWithOverrides` с provider/model из
`app.config.Config()`. Переданный `CredentialSet`, его reviewer slot и
`AllowConfiguredRoleFallback=false` здесь не проверяются.

Credentials находятся во внутреннем контексте `coordinator.RunWithCredentials`
(`internal/agent/credentials.go`, одноимённый метод); внешний ctx `ExecuteRun`
не становится credentialed задним числом. Исправление только F1 этот дефект
**не закрывает**. Reviewer получает историю той же сессии, и tenant transcript
может уйти configured provider владельца процесса вопреки strict isolation.

**Воспроизведение:** два локальных HTTP stubs A (tenant) и B (configured
reviewer); корректный tenant credential set со smart/fast,
`AllowConfiguredRoleFallback=false`; `Overrides.ModelRole="smart"`;
configured reviewer B. Основной запрос идёт в A, follow-up с историей — в B.
Отсутствие reviewer у tenant сейчас не вызывает отказ. Это условный сценарий:
без configured reviewer или с пустой `ModelRole` gate не сработает.

**Покрытие:** reviewer E2E использует только `rush.json` и один server; strict
credential tests не проверяют новый автоматический второй turn. Доказательство
статическое, реальной отправки данных каким-либо провайдерам не выполнялось.

**Рекомендация:** reviewer должен участвовать в той же модели изоляции, что и
основная фаза: брать явный tenant role и разрешённый fallback, либо отключать
auto-pass для credentialed runs до определения такого контракта. Тест обязан
проверять **нулевое** число запросов к B при запрещённом fallback.

### F3. P1 — continuation сохраняет history, но теряет начало итогового ответа

**Коммит:** `57fffb05`; путь продолжает действовать на HEAD для transient errors
с partial output. `f7362f7f` отключает только retry по idle-stall у CLI-вызова.
**Локализация:** `internal/agent/coordinator_run.go:471`, `:505`, `:520`
(`runInternal`, `continuationPrompt`);
`internal/app/app_run_terminal.go:19` (`reconcileTerminalMessage`);
`internal/app/app_run_reviewer.go:270`, `:425`.

Continuation просит **не повторять** уже написанный текст и сохраняет его в
старом assistant message. После успешной попытки `runInternal` возвращает
только новый result. Terminal reconciliation выбирает последнее завершённое
assistant message и использует его `FullText()` как `final_text`. Сборки полного
ответа из связанных попыток нет. Следовательно, успешный JSON/SDK-результат
содержит только хвост, хотя исходный запрос мог требовать законченный отчёт,
документ или JSON.

`RecoveredPartial` не спасает: `findOrphanPartial`
(`internal/app/app_recovery.go:328`) смотрит на последнее assistant message и
ищет unfinished checkpoint. Здесь предыдущий фрагмент уже имеет error finish,
а последнее сообщение успешно завершено.

**Воспроизведение:** provider stub выдаёт `Section 1: Introduction`, затем
transient EOF; на continuation возвращает `Section 2: Conclusion` + clean finish.
При отсутствии auto-reviewer envelope возвращает успех и только `Section 2`.
Для CLI нужен transient EOF/5xx, а не terminal `--idle-timeout` stall.
История содержит обе части; потеря относится к публичному результату вызова.

**Покрытие:** существующий
`TestRunInternal_ContinuationRetry_PreservesPartialContent`
в `internal/agent/coordinator_retry_test.go:464` моделирует именно два таких
фрагмента. Он проверяет наличие двух messages в DB и continuation prompt,
но не полноту возвращённого результата/ExecuteRun envelope. Тест прочитан,
не запускался; он не является доказательством сохранности публичного ответа.

**Рекомендация:** связать сообщения попыток с одним результатом и возвращать
полный ответ либо требовать финальную самостоятельную версию после восстановления.
Нельзя слепо склеивать все assistant messages сессии: среди них есть промежуточный
текст и tool turns. Нужен integration-тест результата, включая структурированный
JSON и несколько попыток, а не только сохранности history.

### F4. P1 — сетевые handshakes/lookup не имеют достаточной отмены и deadlines

**Коммиты:** введено `0f445020`, подключено к production providers `eb59df12`.
**Локализация:** `internal/nettransport/proxy.go:72` (`connectDialer`, особенно
`:81–86`), `internal/nettransport/resolver.go:239` (`dnsTCPQuery`, `:257–273`),
`:142` (`newDoHResolver`), `internal/nettransport/client.go:55` (`BuildTransport`).

После успешного TCP dial custom HTTP CONNECT делает blocking `Write` и
`http.ReadResponse` без deadline и без закрытия соединения по ctx. Timeout
`net.Dialer{Timeout:30s}` ограничивает установление TCP, не дальнейший handshake.
`dnsTCPQuery` аналогично блокируется на `Write`/`io.ReadFull`; deferred Close
исполнится лишь после возврата, которого при молчащем peer может не быть.

DoH передаёт ctx в `NewRequestWithContext`, но весь resolver вызывается из
внешнего `Transport.DialContext`. В проверенном Go 1.26.3
`net/http/transport.go:1529`, `getConn` отделяет dial context от deadline/cancel
исходного HTTP-запроса через `context.WithoutCancel`. Собственного timeout у
DoH client нет. Кроме того, новый внешний Transport имеет нулевой
`TLSHandshakeTimeout`, в отличие от default transport.

**Следствие:** отменённый provider HTTP request может вернуться вызывающему
коду, оставив goroutine и socket в незавершённом dial/lookup. Это не означает,
что каждый `rush run` обязательно зависнет в `Client.Do`: верхний watchdog может
вернуть управление. Но освобождения нижних ресурсов он не гарантирует.
Прямой вызов CONNECT/DNS helper не обязан вернуться даже после cancel.

**Воспроизведение:** локальный peer принимает TCP и после прочтения CONNECT
ничего не отвечает; либо после успешного туннеля принимает DNS query, но не
отправляет двухбайтовую длину. Отменить ctx после барьера «запрос принят».
Проверять завершение helper и закрытие socket, а не только возврат внешнего
`Client.Do`. Для DoH — endpoint принимает запрос и не отвечает, отменяется
внешний provider request. Это предлагаемые, не исполненные в аудите сценарии.

**Покрытие:** `TestCombinedPlainDNSThroughProxyMode` и
`TestCombinedDoHThroughProxyMode` проверяют успешную доставку. Их общий
`buildClient` (`internal/nettransport/doubles_test.go:37`) добавляет тестовый
`Client.Timeout=10s` и cleanup; проверки завершения production goroutines после
cancel отсутствуют. Green happy path не доказывает bounded cleanup.

**Рекомендация:** отдельный ограниченный бюджет на каждый lookup/handshake,
deadline на raw connection плюс реакция на cancel; при успешной передаче
туннеля удалить временный deadline и прекратить cancellation callback.
Для transport сохранить разумные dial/TLS defaults. Cancellation callback
должен быть синхронизирован с передачей владения, чтобы сам не закрывал уже
переданный рабочий tunnel. Общий timeout всего LLM-stream не заменяет эти меры.

### F5. P1 — новые HTTP-транспорты удерживают соединения без владельца cleanup

**Коммиты:** `0f445020`, `eb59df12`; auto-reviewer `ccb510ff` добавляет регулярный
uncached model-override путь.
**Локализация:** `internal/nettransport/client.go:55`,
`internal/nettransport/resolver.go:143`,
`internal/agent/coordinator_providers_network.go:17`;
`internal/agent/coordinator_models.go:375` (`applyModelOverrides`),
`internal/agent/coordinator_model_cache.go:65` (`boundedModelPairCache.set`).

При configured network создаётся собственный `http.Transport` с
`IdleConnTimeout=0`; для DoH — ещё один такой transport, скрытый в closure.
Model overrides и per-call credentials создают свежие clients; cache eviction
удаляет модели без освобождения принадлежащих им transports. Production cleanup
этих новых clients не подключён. Goroutines HTTP/1 keep-alive удерживают socket
и transport: отсутствие ссылок из модели само по себе их не закрывает.

`CloseIdleConnections` на внешнем client не освобождает скрытый DoH pool.
При debug/Copilot также есть wrappers без forwarding метода
`CloseIdleConnections` (`internal/log/http.go:65`,
`internal/oauth/copilot/client.go:26`). Раньше эти обёртки работали поверх общего
default transport с idle timeout; теперь за ними может находиться собственный
transport с нулевым timeout.

**Воспроизведение:** долгоживущий App/SDK, network override, локальный HTTP/1
stub с keep-alive без server idle deadline. Последовательно выполнить N
вызовов со свежими credentials/model overrides, полностью прочитать/закрыть
response bodies. Наблюдать server-side connection states и lifecycle клиента:
старые idle connections остаются после завершения вызовов/вытеснения моделей.
С DoH отдельно учитывать внутренние соединения. Нагрузочный эксперимент в
аудите не проводился; механизм следует из отсутствия timeout/close ownership.

**Сложность и риск:** удержание sockets/goroutines/буферов порядка **O(B)**,
где B — число использованных свежих transports, а не число активных запросов.
При постоянном темпе вызовов рост зависит от uptime и поведения peer. Сервер,
сам закрывающий idle connections, смягчает проблему, но не задаёт гарантию Rush.

**Рекомендация:** явный lifecycle внешнего и resolver transports; bounded idle
timeouts, повторное использование там, где допускает изоляция, forwarding
cleanup через wrappers. При eviction учитывать активные вызовы; не закрывать
чужой общий default transport. Проверка должна измерять освобождение
соединений после завершения/Close, а не только размер model cache.

## Findings: допускается отложить с ограничениями

### F6. P2 — network config нарушает единый snapshot построения модели

**Коммит:** `eb59df12`.
**Локализация:** `internal/agent/coordinator_providers_network.go:18–19`
(`resolveProviderHTTPClient`), вызывается из
`internal/agent/coordinator_providers.go:729`;
`internal/agent/coordinator_models.go:780` (`buildModelsFromCfg`).

ProviderConfig получен из pinned `cfg`, но global `Options.Network` заново
читается через `c.cfg.Config()`. Reload между чтениями создаёт provider из
поколения A с global proxy/DNS из B; reload между smart/fast builds даёт разные
network policies внутри одной пары. Это **логическая гонка snapshot**, а не
доказанный concurrent read/write одной области памяти: ConfigStore публикует
immutable snapshots атомарно.

**Воспроизведение:** удержать сборку после захвата snapshot A, опубликовать B
с изменёнными network defaults, продолжить сборку. Проверить фактический proxy
для клиента, переданного из A. Текущие network tests делают только стабильное
per-field merge; concurrent reload между этими чтениями не проверяют.

**Рекомендация:** передавать pinned global options/resolved NetworkConfig по
всей цепочке provider build. Можно отложить для alpha, если network config
меняется только при перезапуске; документировать несовместимость с hot reload.

### F7. P2 — новый IdleTimeout не сохраняется при durable replay

**Коммит:** `f7362f7f`.
**Локализация:** `internal/agent/call_options.go:69` (`CallOptions.IdleTimeout`),
`:263` (`effectiveIdleTimeoutForCall`); пропуск поля в
`internal/agent/call_data_conversion.go:90`, `:126`
(`toSessionCallOptionsSpec` / `fromSessionCallOptionsSpec`),
`internal/session/session_runqueue.go:138` (`CallOptionsSpec`).

Новый per-call timeout влияет на watchdog и terminal stall policy, но mirror
durable options и оба преобразования не содержат этого поля. После сериализации
и восстановления получается `IdleTimeout=0`. Например, явные 5s или отключение
через CLI sentinel превращаются в shared/default timeout; package default на
этом HEAD — **10 минут** (`internal/agent/agent.go:63`), не CLI default 15 минут.

**Воспроизведение:** `SessionAgentCall{CallOptions:&CallOptions{IdleTimeout:5s}}`
пропустить через `ToSessionAgentCallData`, JSON, `FromSessionAgentCallData`.
Проверить `IdleTimeout` и effective threshold: 5s не сохраняются. Аналогично
проверить CLI disabled sentinel. Запускать долгий wall-clock тест не требуется.
Наличие JSON-поля в `CallOptions` не помогло бы само по себе: сериализуется
отдельный mirror.

**Покрытие:** `TestEffectiveIdleTimeoutForCall_PrecedenceOrder` проверяет только
живую структуру; существующие version/round-trip tests не знают о новом поле.
Здесь не утверждается, что любой pump автоматически включает coordinator retry:
доказана именно потеря threshold и per-call значения terminal policy.

**Рекомендация:** добавить поле в durable contract, оба преобразования и тест
round-trip со значениями 0/positive/disabled. Совместимость прежних записей и
schema version определить явно. Отложить можно только если alpha не обещает
сохранения `--idle-timeout` после durable handoff/restart.

### F8. P2 — финальный envelope забывает инструменты основной фазы

**Коммит:** `ccb510ff`.
**Локализация:** `internal/app/app_run_reviewer.go:576`
(`resetForReviewerPass`), `:118`, `:135`, `:382`, `:458`;
`internal/app/app_run_terminal.go:37`.

На переходе к review обнуляются tool counts и переустанавливается baseline
со всеми сообщениями основной фазы. Финальное reconciliation поэтому считает
только новые reviewer tool calls. Результат основной фазы заменяется. При этом
`tokensBefore`, `costBefore`, `runStart` намеренно сохраняются на весь вызов.
Получается envelope с общими cost/duration, но неполным `tool_calls`; исчезает
и reduction-loss warning, если только primary вызывал `agent`.

**Воспроизведение:** primary делает один `view` и завершается; reviewer отвечает
только текстом. Финальный `tool_calls` пуст вместо `view:1`. Для warning — primary
делегирует нескольким sub-agents, reviewer не делегирует. `--aggregation=attach`
собирается отдельным путём и не обязательно теряет `sub_agent_outputs`.

**Покрытие:** reviewer stub в `app_run_reviewer_pass_test.go` не выдаёт tool
calls, поэтому не проверяет этот эффект. Контракт inventory описан в
`README.md:266`, а тест `TestExecuteRunCycle7ReconcileToolCallsUsesOnlyNewRows`
покрывает одиночную фазу, не сумму двух фаз.

**Рекомендация:** разделить baseline terminal message текущей фазы и учёт всего
вызова; объединять tool IDs/counts без повторного счёта reconciliation.
Отложить для alpha можно, если orchestration читает transcript и не использует
`tool_calls`/warnings как полный журнал выполнения.

### F9. P2 — valid JSON null вызывает исключение в новом sub-agent render path

**Коммит:** `14dd4a54`.
**Локализация:** `web/src/toolFormat.ts:12–13` (`formatActionArgs`),
`web/src/components/SubAgentBlock.tsx:43` (`SubAgentMessage`).

`JSON.parse("null")` успешно возвращает null; приведение TypeScript
`as Record<string, unknown>` не проверяет runtime-тип. Следующее `parsed[k]`
либо `Object.values(parsed)` выбрасывает TypeError за пределами try/catch.
Вызов происходит непосредственно во время render. Backend
`sanitizeToolInput` (`internal/agent/agent_prompt.go:555`) проверяет только
`json.Valid`, поэтому null проходит, а `onToolInputEnd` вообще публикует вход
до этой sanitization. ErrorBoundary в `web/src` не найден.

**Выполненное воспроизведение:** загрузка именно `web/src/toolFormat.ts` в
Node, удаление типов в памяти, `formatActionArgs("bash", "null")`:

```text
TypeError: Cannot read properties of null (reading 'command')
```

Контрпробы: `"{"` и `"{}"` возвращают пустую строку,
`'{"command":"pwd"}'` возвращает `pwd`. Браузерный crash E2E не запускался;
доказано исключение фактической функции на данных, достижимых в transcript.

Это **не полностью новый дефект formatter**: до недели он существовал в
`ActionRow.tsx`. Коммит переносит его в общий helper и расширяет воздействие
на sub-agent transcript, где раньше аргументы вообще не парсились. Такая
атрибуция отделяет регрессию поверхности от старого первопричинного дефекта.

**Покрытие:** пользовательский untracked metadata-тест использует нормальный
JSON-object; null не проверяет и в проверяемый HEAD не входит.

**Рекомендация:** после parse проверять non-null object, определить поведение
для array/scalar, возвращать безопасный preview. Дополнить тест некорректными
runtime-типами и проверкой, что остальной transcript продолжает отображаться.
Для CLI-only alpha можно отложить, для широкой web alpha следует закрыть.

## Проверенные области без дополнительных findings

- **Lock ordering и re-entrancy.** В новых nettransport helpers нет собственных
  mutex. Новый phase state меняется event-loop goroutine между фазами;
  `done` и `drainDone` имеют буфер 1. `runAgentTurnRecovered` отправляет один
  terminal outcome, включая recovered panic. Цикла блокировок здесь не найдено.
  Проверенные mailbox guards берут один mutex; отказ старой эпохе из F1 корректен.
- **Retry/backoff.** Select на `ctx.Done()` прерывает ожидание backoff;
  число попыток ограничено. IdleTimeout проверяется в обоих predicates,
  blind retry не превращается в continuation при отсутствии progress.
  Classifier исключает quota/auth/cancel для обычного error пути. F3 остаётся
  дефектом сборки результата, а не бесконечным retry-loop.
- **CLI timeout.** `--timeout=0` не удаляет последний hard backstop:
  `internal/cmd/run.go:712` ставит 6h timer по умолчанию, `:720` останавливает
  его на возврате. Явный timeout имеет graceful context и +60s force-exit.
  `--idle-timeout` не является ограничением startup/MCP init или любого tool:
  tool watchdog использует отдельный budget. Удаление default 60m само по себе
  не объявляется deadlock; оно увеличивает допустимое время ожидания.
- **Network happy paths.** Проверены per-field inheritance, zero-config nil
  client, DoH precedence, SOCKS remote hostname, CONNECT credentials,
  сохранение buffered bytes и границы DNS-response allocations. Ошибочные
  CONNECT-response ветки закрывают socket; DoH закрывает body при возврате.
  Это не отменяет незавершающиеся ветки и hidden pool из F4/F5.
- **Provider wiring.** Просмотрены switch-ветки HTTP providers, сохранение
  headers/auth и композиция debug/Copilot. Environment не меняется для
  переключения proxy. Обнаруженный F6 — смесь snapshots, не новая запись
  глобальных переменных окружения.
- **MCP test changes.** `t.Setenv` перенесён в непараллельный нуждающийся тест;
  package-wide изменение TestMain удалено. `os.SameFile` проверяет identity,
  `QuotedPrefix/Unquote` корректно извлекают Windows path. Helper waits сохраняют
  timers и `defer Stop`; увеличение 5s до 15s не вводит бесконечного ожидания.
  Выборочно прочитан `TestStdioDiagnosticCancellationSettlesProcess`: отмена,
  `cmd.Wait` и stdout-reader join имеют проверяемые барьеры/внешний timeout.
  Production process-tree код в этой неделе не менялся и заново не сертифицирован.
- **Web metadata/timestamps.** Дополнительные model/effort проверки выполняются
  в уже существующем проходе; сложность остаётся O(число parts/messages).
  TimeBadge не создаёт interval/subscription. Доказанного нового listener leak,
  stale-store mutation или квадратичного алгоритма в этих diff нет. F9 —
  отдельный error-path render defect.
- **Схема и тестовые noctx правки.** `NetworkConfig` присутствует в schema;
  изменения `180f1544`/`5c20c771` касаются test doubles. Дополнительный
  `enabled_in_cli` в schema соответствует уже существующему config-полю;
  это не новая concurrency-ветка недели.

## False positives, допустимые риски и стоимость

| Подозрение | Оценка и основание |
| --- | --- |
| nil dereference `rs.proxyURL.Scheme` в `BuildTransport` | Не воспроизводится на validated modes: empty возвращает раньше; resolver-only попадает в первую ветку; оставшийся proxy-only имеет non-nil URL. |
| Новый `ClearReservedOwnership` сам создаёт ABA | Нет: typed nil только затеняет context value; token claim атомарный, rebind проверяет epoch/state. Реальная проблема — неиспользованный reviewCtx, F1. |
| Модель/effort добавляют ещё один квадратичный проход web history | Нет: поля вычисляются в существующем линейном проходе. Парсинг аргументов O(размер JSON) на render есть, но новой доказанной release-critical O(N²) регрессии нет. |
| Ответ DNS может заставить выделить произвольно большой buffer | Нет: TCP length — uint16, DoH чтение ограничено 64 KiB. Проверка `length > 64<<10` недостижима, но сама аллокация уже ограничена. Проверка семантики DNS-ответов не была полным protocol audit. |
| 15s в MCP tests доказывают устранение CI flakes | Нет. Это более мягкий timeout, не устранение доказанного runtime deadlock. Исторический зелёный rerun не заменяет повторяемый stress/race gate. Новых flakes этим аудитом не наблюдалось: тесты не запускались. |
| Reviewer обязан быть read-only | Не текущий контракт: `buildReviewerPassTurn` прямо сохраняет read/write/bash tools как у явного reviewer. Это осознанная продуктовая политика, не добавленный finding; нарушение именно заявленного role/toolset описано в F1. |
| Continuation гарантирует exactly-once инструменты | Нет: инструкция модели «продолжай» не является fencing/idempotency. История и tool results помогают восстановлению, но повтор внешнего side effect после неопределённого исхода остаётся общим риском; конкретный новый двойной side effect не доказан. |

Очевидная лишняя работа: `resetForReviewerPass` делает `Messages.List` и строит
baseline, который немедленно повторно строит `runTurnPhase`; retry сначала читает
всю историю в `shouldContinueTurn`, затем при отказе повторяет чтение в
`shouldRetryTurn`. Для N сообщений/суммарного размера S это дополнительные
O(N + S) чтения и аллокации; сама форма линейная. Это можно отложить после alpha:
без измерений не объявляется критической производительностью. F5 принципиально
отличается накоплением живых ресурсов по числу построенных клиентов.

## Ограничения и необходимые проверки перед снятием NO-GO

Аудит выборочный, преимущественно статический. Он не доказывает отсутствие
редких interleavings, platform-specific процессов, SDK-internal races, сетевых
проблем на реальных proxy или поведения после crash. Не выполнялись benchmarks,
fault injection, race detector, полный DNS/TLS protocol audit и внешний security
audit. Старые finding-списки из `docs/reviews` не переносились в этот отчёт как
актуальные без проверки; закрытие всех старых findings не входит в этот срез.

Проверенные тесты показывают существенный happy-path охват, но пробелы приходятся
на **стыки новых возможностей**: reviewer + fail-fast override; reviewer +
tenant credentials; continuation + итоговый envelope; network + cancel/Close;
idle timeout + durable replay. Поэтому даже зелёный CI проверяемой серии сам по
себе не снимает findings.

Минимальный gate после исправлений:

1. Reviewer с настоящим coordinator: worker toolset, explicit override +
   fail-fast reservation; ошибка/cancel между фазами; отсутствие обращения к
   configured provider для strict tenant credentials.
2. Continuation с transient error после partial text: полный итоговый текст и
   JSON, сохранённый transcript, отсутствие повторного исполнения известных
   успешных tools. Проверять возвращённый envelope, не только DB.
3. Молчащие CONNECT, DNS/TCP и DoH peers с барьерами: ограниченный возврат и
   освобождение goroutine/socket после cancel; отдельно нормальная передача
   туннеля, чтобы cleanup не закрывал живое соединение.
4. Несколько последовательных credential/model builds в одном App: закрытие
   idle pools, включая DoH и debug/Copilot wrappers, без повреждения активных
   запросов. Это небольшой lifecycle-тест, не искусственная нагрузка машины.
5. Целевые `-race` проверки затронутых lifecycle/ownership сценариев; durable
   round-trip, counts двух фаз и null-render — при включении соответствующих
   возможностей в alpha.

**Итоговый release verdict: текущий `f7362f7f` не готов к общей alpha.**
После исправления и целевого подтверждения F1–F5 возможен повторный GO-review;
F6–F9 допускают только явно ограниченный alpha-профиль, описанный в таблице.
Отчёт не является разрешением на выпуск до прохождения этого gate.
