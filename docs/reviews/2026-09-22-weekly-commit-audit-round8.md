# Rush: независимый release-readiness аудит, round 8

Дата: **2026-09-22**. Проверенный HEAD:
`d67a9c7e490cec7805dd46f2d1a6f738b8a6d323`.

## Summary

**Release verdict: NO-GO общей alpha.** Найдены три ранее не описанные
проблемы: потеря provider-specific network policy при загрузке встроенного
провайдера (**R8-1 / P1**), успешный результат отменённого незавершённого turn
по промежуточному `tool_use` (**R8-2 / P1**) и присвоение вызову результата
следующего владельца сессии (**R8-3 / P2**).

R8-1 — дефект интеграции недельной функции. Корни R8-2/R8-3 старше недели;
они обнаружены при проверке текущего lifecycle и перенесённого в этой серии
reviewer event loop. Их первичное появление не приписывается недельным fixes.

На **committed HEAD остаются все 17 P2 round 7**: после проверенного там
`4b2b2a0c` добавлены только отчёт и checkpoint. Вместе с новыми findings это
**2 P1 и 18 P2**. Закрытые F1/F2/F4/F5/R2-1/R2-3 и прежние P1-механизмы
R3-1 не открываются повторно; F3 учитывается только внутри R2-2.

Рабочая копия существенно отличается от HEAD: в ней уже лежит чужой
незакоммиченный пакет исправлений. Он прочитан отдельно. По коду устранены
конкретные механизмы **14 из 17** прежних P2, но для **R2-5, R4-2 и R5-2**
сохранились подтверждённые остатки. Все три новых finding действуют и в этой
копии. Поэтому и этот кандидат пока **NO-GO: 2 P1 и 4 P2** в проверенных
механизмах, без утверждения о полном runtime acceptance.

Нового доказанного циклического deadlock или гонки памяти Go не установлено.
Найдены ошибки границ результата, отмены и сетевой политики. Статическая
проверка не исключает других interleavings, leaks или flakes.

## Срез, метод и фактическая изоляция

- Период: **2026-09-15 00:00:00 +02:00 — HEAD от 2026-09-22
  18:32:35 +02:00**, `Europe/Berlin`, committer date среди предков HEAD.
- База: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`, 2026-09-11.
  Первый коммит выбранного периода — 19 сентября. `--since` и
  `--since-as-filter` дают **41 коммит**. Совокупный diff: **91 файл,
  12752 добавления, 746 удалений**, включая отчёты, tests и checkpoints.
- Между `4b2b2a0c` из round 7 и текущим HEAD изменились только
  `docs/reviews/2026-09-22-weekly-commit-audit-round7.md` и
  `docs/checkpoints/2026-09-22-1829.md`. Checkpoint описывает исправления,
  которые ещё не входят в committed source tree.
- Изучены log, состав и production-diff серии, текущие реализации,
  вызывающие пути, прежние findings и относящиеся к ним тесты. Для изменённых
  пользователем файлов committed-код читался через `git show d67a9c7e:<path>`.
  Ссылки на строки без пометки **WT** относятся к этому HEAD; **WT** означает
  отдельно прочитанную незакоммиченную рабочую копию.
- Доказательства — значения полей, трассировка control flow и допустимые
  последовательности событий. Ни один описанный ниже runtime-сценарий
  не выдаётся за исполненный тест. Для нижнего dial, CONNECT и вложенного
  `url.Error` дополнительно прочитан локальный Go **1.26.3**.
- Вопреки условию о предоставленном изолированном worktree, фактический
  `git worktree list --porcelain` показал **единственный checkout на main**.
  Новый worktree не создавался, ветки не переключались, sub-agents не запускались.
  Это ограничение окружения, а не основание очищать чужие изменения.
- На входе: **34 modified tracked-файла, 1 deleted tracked-файл и
  13 untracked-файлов**, index пуст. Все 48 исходных записей сохранены;
  область записи этого аудита ограничена данным Markdown. Наличие других
  modified-файлов в итоговом status не является изменением аудита.

Прочитаны и сопоставлены:
[round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md),
[round 6](2026-09-22-weekly-commit-audit-round6.md),
[round 7](2026-09-22-weekly-commit-audit-round7.md).
Исторические сообщения об успешных тестах не использованы как текущий green gate.

### Карта недельной серии

| Коммиты | Предмет проверки |
| --- | --- |
| `57fffb05`, `2169dbe3`, `b0a55aab`, `4b2b2a0c` | Retry/continuation, admission, принадлежность и синхронизация assistant evidence. |
| `ccb510ff`, `babd295c`, `54acbf71`, `8aabba4d` | Reviewer, credentials, ownership/context, terminal reconciliation, точность склейки. |
| `0f445020`, `eb59df12`, `fd18cf0c`, `d0966df9` | Config → provider transport, proxy/DNS/DoH, deadlines, cache и cleanup wrappers. |
| `f7362f7f` | CLI timeout defaults, per-call idle policy, durable conversion и watchdog extension. |
| `14dd4a54`, `924add8a`, `10e32c56`, `97c22050` | Web formatter, sub-agent metadata, timestamps и тестовое покрытие. |
| `4357e157`, `fbc8a177`, `a275e323` | Peak-hours message, config/CLI/WS/web mapping, labels и release metadata. |
| `87ee064f`, `64409ab3`, `180f1544`, `5c20c771`, `c9eb9a98`, `c32c72d6`, `1420acb1`, `52a456b8` | MCP tests, noctx/lint, parallel marker и перенос evidence tests. |
| `df0051b1`, `85ee8836`, `bf9f6d3d`, `dd03ecf0`, `6f134f0a`, `d67a9c7e` | Checkpoints, без runtime-изменений. |
| `e547e12e`, `31e90d2e`, `295cc386`, `a4f99d8b`, `6e18502f`, `07cabfae`, `0c4bae10` | Предыдущие аудиты. |

P1 ниже означает существенное нарушение результата или изоляции поддерживаемого
сценария. P2 — ограниченный доказанный дефект, требующий исправления либо явно
принятого ограничения затронутого режима. P0 не установлен.

## Новые findings

### R8-1. P1 — загрузчик встроенных провайдеров теряет их network policy

**Коммиты:** поле добавлено `0f445020`, production-потребитель — `eb59df12`.
Сам literal загрузчика существовал раньше; интеграция нового поля его не обновила.
**Места:** `internal/config/config.go:186`,
`internal/config/load_providers.go:54,117,312` (`configureProviders`),
`internal/config/load.go:256`, `internal/config/store_reload.go:524`,
`internal/agent/coordinator_providers_network.go:17`.
**Статус:** подтверждён на HEAD и WT; `load_providers.go` в WT не изменён.

Загрузчик получает пользовательский `ProviderConfig` для известного catalog ID,
но создаёт новый `prepared := ProviderConfig{...}`. В literal перенесены
`OAuthToken`, headers, body, `PeakHours` и другие поля; **`Network` отсутствует**.
Затем `c.Providers.Set(string(p.ID), prepared)` заменяет исходную конфигурацию.
Позднейшей передачи `config.Network` в этой ветви нет. Это происходит при
первом Load и повторяется при ReloadFromDisk.

Достаточный сценарий проверки, без внешних запросов: включены default providers,
для известного `openai` задан рабочий тестовый ключ и непустой
`providers.openai.network`, global network отсутствует. После
`configureProviders` эффективный `ProviderConfig.Network == nil`.
`ResolveNetworkConfig(nil, nil)` даёт нулевую конфигурацию, `BuildHTTPClient`
возвращает nil, provider SDK использует свой default transport. Если global
network существует, применяется он, хотя явно заданный provider override должен
был его перекрыть. Та же потеря касается `dns_server` и `doh_url`.

Следствие — обычная настройка встроенного провайдера молча перестаёт определять
маршрут. В среде с разрешённым прямым выходом запрос может обойти обязательный
proxy/custom DNS; при доступности endpoint только через proxy запрос не работает.
Не утверждается, что TLS отключается или credentials отправляются неизвестному
получателю: доказано игнорирование заданной сетевой границы.

`TestNetworkConfigUnmarshal` проверяет только decoding. Provider-network tests
строят `ProviderConfig` непосредственно и минуют `configureProviders`; поэтому
их успешность не доказывает сохранность настройки из файла. Ветка полностью
custom provider и `DisableDefaultProviders=true` этим конкретным literal не
затронута. Исправление F6 в WT передаёт согласованный snapshot, но поле к этому
моменту уже потеряно.

**Рекомендация:** сохранять `Network` при catalog merge и проверить полный
Load/Reload → effective provider → выбранный transport. Минимальные случаи:
provider-only proxy, override global proxy, DNS/DoH, отсутствие override;
endpoint и proxy могут быть локальными doubles. Это обязательное исправление
перед alpha с per-provider networking.

### R8-2. P1 — отмена незавершённого turn становится успехом по tool-step

**Коммиты:** первичный механизм `ed744c17` от 2026-09-09, вне недельного окна;
`ccb510ff` переносит его в повторно используемый reviewer loop.
**Места:** `internal/app/app_run_terminal.go:71` (`reconcileTerminalMessage`),
`internal/app/app_run_reviewer.go:209,356,366,394`,
`internal/app/app_run_errors.go:154`, `internal/message/content.go:314`,
`internal/agent/agent_turn_step.go:245` (`classifyStepFinishReason`).
**WT:** тот же выбор строки в `app_run_terminal.go:84`, cancel/suppression
в `app_run_reviewer.go:226,473`; локальный `canceledAfterCommit` это не исправляет.

`Message.IsFinished()` признаёт любой непустой non-partial Finish, в том числе
`tool_use`. Reconciliation выбирает последнюю **завершённую строку**, пропуская
более новую незавершённую assistant row. На `ctx.Done()` этот результат кэшируется
и используется без ожидания завершения runner. `finish` снимает cancellation,
если `!runFailed(finalReason, nil, false)`; для `tool_use` этот predicate
возвращает false, то есть допускает успешный исход.

Статически допустимая последовательность с управляемыми барьерами:

1. В одном вызове шаг A исполнил инструмент и записал Finish `tool_use`.
   Это штатный промежуточный шаг; `StopTurn` отсутствует.
2. Следующий шаг B уже создал новую assistant row и ещё генерирует ответ.
   У B пока нет terminal Finish. Отменяется родительский context или истекает
   его deadline; финальная cancel/error запись B ещё не дошла до БД.
3. Event loop выбирает `ctx.Done()`. Reconciliation пропускает B и выбирает A.
   `finish` считает A достаточным доказательством завершения и очищает ошибку.
4. `ExecuteRun` возвращает nil error и результат с `exit_reason="tool_use"`
   вместо ошибки отмены незавершённого вызова. Последующая запись B уже не
   изменит возвращённый результат. В terse возможен нулевой exit при неполной работе.

Это не R2-4: там primary действительно закончил `end_turn`, а проблема возникала
при переходе к reviewer. Здесь ложный успех появляется **без reviewer** и без
чужого вызова. WT закрывает переход после cancel, но сохраняет ошибочное
предположение, что любой выбранный finished message доказывает завершение turn.

Прочитанные `TestExecuteRunCycle7CancelAfterCommitUsesAuthoritativeTerminal`
и новый WT `TestExecuteRunReviewerPassGateRefusesCanceledParent` ставят барьер
после полного возврата coordinator. `...CancelBeforeTerminalKeepsPromptCancellation`
удаляет единственную assistant row. Ни один из этих случаев не моделирует
`tool_use → unfinished assistant → cancel`.

**Рекомендация:** подавлять cancel только по доказательству завершения именно
логического вызова, а не промежуточного сообщения. Более новая незавершённая
строка и `tool_use` не должны свидетельствовать об успешном окончании.
Сохранить корректный приоритет настоящего committed `end_turn` над поздним cancel.
Acceptance должен проверять обе последовательности и eventual cleanup runner.
Это обязательный gate для CLI/SDK, включая профиль без reviewer/network/web.

### R8-3. P2 — итог вызова может принадлежать следующему владельцу сессии

**Коммиты:** источник `ed744c17` от 2026-09-09; недельные `ccb510ff` и
`babd295c` используют и расширяют этот reconciliation path, не вводя run identity.
**Места:** `internal/app/app_run_reviewer.go:118,148,370`,
`internal/app/app_run_terminal.go:33,56,71,184`,
`internal/app/app_run_turn.go:18,95`, `internal/agent/agent_run.go:553`.
**Статус:** HEAD и WT. WT дополняет inventory, но сохраняет session-wide выбор
terminal по принципу «не существовал до начала фазы».

Baseline отсекает старые сообщения, но не устанавливает принадлежность новых
строк вызову. Ownership заканчивается до обработки `done` и последующего
`Messages.List`. Поэтому A может закончить свой turn и освободить сессию,
затем B законно займёт её и завершит собственный turn, пока event loop A
ещё не сделал terminal reconciliation.

При baseline A = старые строки и порядке новых строк `user A, answer A,
user B, answer B` reconciliation A выбирает последний finished assistant — B.
Хотя `agentTurnResponse.result` несёт результат runner, `finish` не использует
его для ограничения terminal ID. A получает `FinalText` B; при ошибке B
успешный A дополнительно становится неуспешным. Counts и session usage могут
также включить B. Пример не требует одновременно исполнять два turn под одним
lock, silent queue или обходить fail-fast: B входит уже после release A.

Возможный acceptance использует существующий `executeRunDoneCaseSeam`:
задержать A перед drain/reconcile, завершить B в той же сессии и возобновить A;
обоим результатам должны соответствовать собственные ответы. Этот сценарий
в аудите не запускался. Frequency и межпользовательская утечка не заявляются:
речь о двух разрешённых вызовах одной сессии.

**Рекомендация:** переносить identity логического вызова/его terminal message
до ExecuteRun, ограничивая reconciliation и inventory собственными rows;
baseline оставлять дополнительным фильтром. Это отдельная ошибка потребителя
результата: own-ID fixes R3-1/R7-1 защищают retry classifiers и её не закрывают.
До исправления host может сериализовать всю длительность ExecuteRun одной
сессии, включая финальную сборку ответа, а не только provider turn.

## Перепроверка прежних findings на committed HEAD

«Закрыт» означает устранение указанного механизма по коду. Это не результат
нового запуска тестов. В таблице не смешиваются committed fixes и чужой WT.

| Finding | Статус HEAD | Доказательство, локализация и рекомендация |
| --- | --- | --- |
| F1 | Закрыт `babd295c` | `app_run_reviewer.go:599` присваивает reviewer context, `:683` очищает reservation. Старые smart toolset/stale-token примеры не действуют. |
| F2 | Закрыт `babd295c` | `app_run.go:513` исключает auto-review при `req.Credentials != nil`. Tenant transcript этим gate не отправляется configured reviewer. |
| F3 | Только R2-2 | Потеря всего префикса, tool-step разрыв и искусственные разделители устранены; остаток ниже посчитан один раз. |
| F4 | Закрыт в прежнем объёме | `fd18cf0c`/`d0966df9`: CONNECT/DNS guards, DoH exchange timeout и bounded SOCKS. Initial TCP gap отдельно R6-1. |
| F5 | Закрыт в прежнем объёме | `fd18cf0c`: 90s idle lifetime main/DoH pools, cache capacity 8, eviction после unlock, cleanup forwarding в log/Copilot wrappers. Бесконечное удержание idle pools не подтверждается на HEAD. |
| F6 / P2 | Подтверждён | `eb59df12`, `coordinator_providers_network.go:18`: provider из pinned snapshot соединяется с заново прочитанным global network. Reload между чтениями даёт A-provider/B-network. Передавать один snapshot; временный обход — restart при смене сети. Это отдельно от утраты поля в R8-1. |
| F7 / P2 | Подтверждён | `f7362f7f`, `call_data_conversion.go:90,126`, `session/session_runqueue.go:138`: в durable mirror/converters нет IdleTimeout. Round-trip положительного порога или disabled sentinel возвращает ноль. Сохранять поле и проверять old/zero/positive/disabled decoding. |
| F8 / P2 | Подтверждён | `ccb510ff`, `app_run_reviewer.go:604`, `app_run_terminal.go:56`: reset counts и новый baseline исключают primary tools. Primary `view`, reviewer без tools → пустой итоговый inventory при общей cost. Нужен учёт invocation с дедупликацией ID. |
| F9 / P2 | Подтверждён | Новая поверхность `14dd4a54`, `web/src/toolFormat.ts:12,13`, `SubAgentBlock.tsx:43`: parse JSON null успешен, последующий доступ к полю вне catch бросает TypeError. Проверять non-null object до render; старые воспроизведения не считаются новым запуском. |
| R2-1 | Закрыт `d0966df9` | `nettransport/proxy.go`, `socksDialer` ставит собственный deadline на TCP/greeting/auth/CONNECT; DoH-over-SOCKS использует этот dialer. |
| R2-2 / P2 | Остаток подтверждён | `57fffb05`, fixes `babd295c`/`54acbf71`/`8aabba4d`; `app_run_terminal.go:142`: whitespace-only error fragment пропускается. Цепочка `"a"`, `" "`, `"b"` с continuation boundaries превращается в `"ab"`; промежуточный шаг может иметь reasoning, обеспечивающий progress. Сохранять байты каждого fragment. |
| R2-3 | Закрыт `54acbf71` | `app_run_reviewer.go:677` переносит FailIfSessionBusy; mailbox атомарно отказывает без enqueue. Старый reviewer fail-fast bypass не повторяется. |
| R2-4 / P2 | Подтверждён | `ccb510ff`, `app_run.go:513`, `app_run_reviewer.go:209,394,598`: cancel после настоящего primary end_turn может быть подавлен, gate запускает review, cached terminal/context/cancel не сбрасываются. Запретить переход после cancel и очистить тройку cache при смене фазы. |
| R2-5 / P2 | Подтверждён | `0f445020`/`eb59df12`, `nettransport/config.go:79,91,103`, `resolver.go:228,233`: diagnostics включают raw proxy/DoH URLs с userinfo/query. Обычная ошибка схемы или DoH non-200 раскрывает секретный компонент в строке ошибки. Redaction нужна и для вложенных errors, и для конечных logs/envelope. |
| R2-6 / P2 | Подтверждён | `fd18cf0c`, `nettransport/lifecycle_test.go:662`: parallel eviction-test использует global cache и ждёт reuse между двумя requests. Другие tests вправе вытеснить entry в этом промежутке. Нужен собственный cache в тесте, не увеличение timeout. Фактического падения здесь не было. |
| R3-1 | Прежние P1 закрыты | `b0a55aab` выбирает собственный ID; `4b2b2a0c` создаёт отдельный mutex-protected capture на попытку и запечатывает его. Повтор прежнего ID и несинхронизированная assistant string устранены. Не эквивалентно исправлению R7-1. |
| R3-2 / P2 | Подтверждён | `0f445020`/`eb59df12`, `nettransport/config.go:87`, `proxy.go:96,101`: HTTP proxy URL без порта принят, custom CONNECT передаёт `pu.Host` в TCP dial и получает missing port. Нормализовать default 80, включая IPv6; обход — явный порт. |
| R4-1 / P2 | Подтверждён | `0f445020`/`eb59df12`, `proxy.go:120,121`: ReadResponse читает CONNECT head из reader без byte budget. `fd18cf0c` ограничивает время, но память остаётся O(H) от объёма head. Нужен лимит заголовка и корректный handoff buffered payload; воздействие некорректного proxy не проверялось нагрузкой. |
| R4-2 / P2 | Подтверждён | `0f445020`/`eb59df12`, `resolver.go:20,129,191`, `client.go:116`: resolveFunc отдаёт один IP, dial выполняется один раз. Первый недоступный адрес исключает доступный следующий. Нужен список адресов и bounded fallback между families. |
| R5-1 / P2 | Подтверждён | Новая поверхность `ccb510ff`, `app_run_reviewer.go:655`, `coordinator_run.go:340`, `credentials.go:339`: после reviewer R/401 rebuild выбирает durable smart S. Сохранить временные model/effort overrides; проверять S → R/401 → R без записи R в session smart. |
| R5-2 / P2 | Подтверждён | Интеграция `eb59df12`, `coordinator_providers.go:951`, `config/store_oauth.go:184`, `oauth/copilot/oauth.go:168`, `oauth/hyper/device.go:170`: inference настроен, auth refresh создаёт default-route client. Endpoint, доступный только через Rush proxy, не обновляет token. Передавать network policy в auth lifecycle с timeout/cancel. |
| R5-3 / P2 | Подтверждён | `ccb510ff`, `app_run_reviewer.go:285,293`, `app_run.go:495,513`: terse печатает finished primary до gate, затем reviewer. Ответы PRIMARY/REVIEW дают склейку двух результатов. Публиковать только выбранный final после фаз; JSON обходит этот конкретный дефект. |
| R6-1 / P2 | Подтверждён | `0f445020`/`eb59df12`, `client.go:80,98`, `resolver.go:163,190`: HTTP proxy-only/direct DoH не задают DialContext. Go использует zeroDialer и отделяет dial context от request cancel. Connect может пережить HTTP timeout до срока ОС; это не заявление о вечной утечке. Задать собственный TCP dial timeout. |
| R6-2 / P2 | Подтверждён | Новая поверхность `f7362f7f`, алгоритм старше недели (`9d5b4e0a7`); `stream_watchdog.go:215,443`: extension при cap=0 ограничивается start+idle. Continuous progress дольше idle получает ложный stall, который per-call IdleTimeout делает terminal. Clamp применять только при cap>0. |
| R7-1 / P2 | Подтверждён | `57fffb05`/`b0a55aab`/`4b2b2a0c`; `coordinator_attempt_evidence.go:47,61`, `coordinator_run.go:449,504,800`, `agent_run.go:81,91`: queued copy может записать собственный ID до seal отправителя. После её terminal 400 отправитель классифицирует `(nil,nil)` как nil execution error и делает лишний retry. Нужен admission outcome до classifiers; mutex capture этого не гарантирует. |

Опровергнута актуальность закрытых механизмов, а не их историческое существование.
Полностью ложных исторических findings в перечисленных семи отчётах не установлено.
Более сильные заявления «все completion rows принадлежат этому run» и
«finished row означает finished run» опровергаются R8-3 и R8-2 соответственно.

## Отдельная проверка незакоммиченных исправлений

Этот раздел описывает **WT, не HEAD и не содержимое commit отчёта**.
У пакета исправлений нет собственного commit ID; его база — `d67a9c7e`.
Прочитанные новые tests не запускались и сами по себе не закрывают дефект.

| Findings | Результат по WT-коду |
| --- | --- |
| F6 | Исходное отдельное чтение global defaults устранено: `resolveProviderHTTPClient(cfg, providerCfg)` получает snapshot от model builder. Утрата Network до этой стадии остаётся R8-1. |
| F7 | `CallOptionsSpec.IdleTimeout` и оба converters присутствуют; additive decoding старой записи даёт ноль. Новый round-trip test охватывает states. |
| F8 | `invocationToolCalls`/`invocationToolCallIDs`, `mergeReconciledToolCalls` объединяют две фазы. Конкретная потеря primary inventory устранена; run identity отдельно R8-3. |
| F9 | Non-null/object/array guard в formatter закрывает null exception. Backend также нормализует bare null; frontend защита нужна для исторических/live rows и присутствует. |
| R2-2 | `continuationChainText` добавляет FullText каждого error fragment без TrimSpace-filter. Исходный whitespace-only контрпример закрыт. |
| R2-4 | Gate проверяет `canceledAfterCommit` и `loop.ctx.Err()`, reset очищает cached terminal/context/cancel. Межфазный сценарий закрыт. Ошибочное признание tool-step завершением — R8-2. |
| R2-6 | Eviction-test использует `newTransportCache()` и тот же production builder с явным cache. Другие tests больше не вытесняют его warm-up entry. |
| R3-2 | `proxyDialAddr` добавляет port 80 через Hostname/JoinHostPort. |
| R4-1 | LimitedReader ограничивает head; после успешного parse его лимит снимается, bufferedConn сохраняет прочитанный payload. |
| R5-1 | Reviewer context несёт `WithModelOverrides`; `resolveCallModels` повторно применяет override при credential rebuild, persistence очищена. Исходный R → S контрпример закрыт. |
| R5-3 | Terse перестал печатать на каждом event, `flushTerseOutput` вызывается после reviewer gate. Конкретная склейка primary/reviewer устранена. |
| R6-1 | HTTP proxy-only и DoH paths получают transport dialer с Timeout 30s. Отсутствие собственного initial-dial bound устранено по коду. |
| R6-2 | Оба clamp в watchdog защищены `hardCap > 0`. Новый zero-cap progress test направлен на нужное условие. |
| R7-1 | Отдельный `turnAdmission` отмечается до queued return; retry loop проверяет wasQueued до evidence. Recorder отсоединяется от downstream context, поэтому child Run не меняет admission родителя. |

### R2-5 / P2: redaction WT не охватывает вложенный runtime url.Error

**Места WT:** `internal/nettransport/resolver.go:247` (`dohQuery`),
`internal/agent/coordinator_providers_network.go:100`,
`internal/agent/agent_turn_failure.go:249`,
`internal/app/app_run_result.go:290` (`buildRunResult`, ветка err != nil).

В build errors и DoH non-200 наружный endpoint теперь очищается. Но при ошибке
`client.Do` строка строится как безопасный endpoint плюс **`%w` исходного err**.
Go `http.Client` возвращает `url.Error`: его URL очищен от password,
но query сохраняется; `url.Error.Error()` включает этот URL в строку.
Следовательно, timeout/connection failure DoH endpoint с секретом в query
по-прежнему возвращает секрет из `dohQuery`.

Новый `TestDoHErrorsRedactEndpoint` получает HTTP 503 и не проходит эту ветку.
Agent redaction в generic finish очищает одну persisted строку, а
`redactNetworkError` применяется при build client, не ко всем runtime returns.
Возвращаемый error и `buildRunResult` с `err.Error()` остаются отдельными sinks.
Для доказательства остатка достаточно самой строки error resolver;
выполненная отправка данных или чтение реальных секретов не заявляются.

**Рекомендация:** возвращать безопасное текстовое представление всей цепочки,
сохраняя типы для errors.Is/As отдельно. Проверить fake-secret отсутствие в
runtime error, envelope и logs при connection failure/timeout, а не только 503.
Это продолжение R2-5, не новый security finding в подсчёте.

### R4-2 / P2: WT возвращает все A, но не делает IPv6 fallback при наличии A

**Места WT:** `internal/nettransport/resolver.go:205` (`newDoHResolver`),
`:286` (`dnsOverTCPResolver`), `internal/nettransport/client.go:154`.

Вместо одного IP теперь сохраняется список и пробуются последующие адреса.
Однако оба resolver возвращают A-list сразу при `len(a)>0` и не запрашивают
AAAA. Если A-адреса недоступны, а у того же имени есть доступный AAAA, dialer
его никогда не получит. Это сохраняется и при быстрых отказах всех A, то есть
не зависит от исчерпания общего dial budget. Direct plainDNSResolver собирает
обе families; остаток относится к DNS-over-proxy и DoH.

Новый fallback test проверяет два IPv4 loopback addresses и не покрывает
family fallback. **Рекомендация:** получать/пробовать AAAA после неудачи A либо
сохранять обе families при разрешении, с общим конечным бюджетом. Сокращение
второго DNS query не должно устранять работу на доступном IPv6 маршруте.

### R5-2 / P2: WT переходит на default route при ошибке сборки auth client

**Места WT:** `internal/agent/coordinator_providers_auth.go:136,148,151,153`,
`internal/config/store_oauth.go:197`, OAuth `RefreshTokenWithClient` /
`ExchangeTokenWithClient` и `refreshHTTPClient`.

Обычный refresh теперь получает configured client, поэтому прежний безусловный
bypass устранён. Но `clientErr != nil` только логируется, затем `httpClient=nil`
передаётся в auth helper: это явный выбор default route, не остановка.
Достижимая граница — inference уже построен из прежнего рабочего snapshot,
перед 401-refresh опубликованы некорректные global network settings; поля
Network не валидируются `configureProviders`. Refresh читает новый snapshot,
build отказывает, helper выполняет обмен по default transport.

Доказано нарушение policy на error path, а не отправка token постороннему host:
auth endpoint остаётся прежним. **Рекомендация:** при ошибке обязательной network
policy возвращать ошибку, не выполнять обмен по другому маршруту; связывать
auth client с тем же согласованным provider snapshot. Acceptance должен
подтверждать отсутствие default-route request при build failure.

## Проверенные области без дополнительных findings

- **Lock ordering / re-entrancy.** `attemptEvidence` держит mutex только для
  ID/seal, callback вызывается после unlock `turnStream.mu`. Cache eviction
  закрывает idle pools вне mutex. Queue-drain остаётся циклом существующего
  dispatcher и не захватывает тот же OS session lock рекурсивно. Конкретного
  нового цикла mutex/channel waits в рассмотренных изменениях не найдено.
- **Ownership / admission.** One-shot token использует atomic claim, дальнейшие
  попытки переходят к обычному reserve; само наличие старого consumed token не
  означает ошибку retry. Epoch guards сохранены. R7-1 и R8-3 — разные границы:
  admission sender и attribution возвращаемого результата.
- **Channels / cleanup.** `done` и `drainDone` буферизованы одним элементом;
  закрытая subscription становится nil. ExecuteRun имеет deferred cancel,
  поэтому подписка не живёт до отмены произвольного внешнего Background context.
  Interrupt ticker отменяется до join. Это не доказывает завершение каждого
  tool/process при cancel; R8-2 показывает преждевременное завершение caller.
- **Handshakes.** CONNECT error paths закрывают conn; handshakeGuard joins
  watcher до снятия deadline и передачи tunnel. DNS/TCP закрывает connection,
  DoH закрывает response body. Bounded SOCKS применяется и во вложенном DoH.
  Прежние unbounded handshake findings не возвращаются из-за иных dial gaps.
- **Credentials и ограничения.** Configured auto-review отключён для credentialed
  request. Reviewer переносит fail-fast, folder/disk scope и context-carried
  allowlist. Общий transport сам по себе не смешивает request Authorization.
  MaxCost/MaxTokens читают session totals: копирование значений во вторую фазу
  не создаёт независимый второй денежный/token budget.
- **Logging.** HTTP body logging требует отдельного opt-in; known token/secret
  JSON fields редактируются. Нельзя объявлять любой debug OAuth client новой
  безусловной утечкой тела. Подтверждённый URL/error остаток учтён в R2-5.
- **Peak-hours.** Поле Message прослежено через config, error/guidance, оба WS
  converters и frontend payloads; CLI time-only update сохраняет заметку.
  Operator-authored текст не исполняется как команда. Новый release defect
  в этих преобразованиях не установлен.
- **MCP / процессы.** Изменения недели локализуют тестовый env и используют
  directory identity/ограниченные waits. Production process-tree cleanup в
  этом diff не менялся; увеличение timeout tests не считается доказательством
  отсутствия flakes. Никакие процессы в рамках аудита не завершались.
- **Performance.** Own-ID classifiers всё ещё могут дважды читать/декодировать
  O(N+S) истории, reviewer делает повторные baseline reads. Это лишние I/O,
  но без измерений release-critical slowdown не установлен. `acc + next`
  потенциально копирует prefixes многократно; при default bounded retry count
  нет доказанного самостоятельного release blocker. CONNECT O(H) без bound
  отдельно учтён в R4-1. Metadata/timestamps web не добавляют per-message timer
  и остаются в существующем линейном проходе.

## Ограничения и контроль сохранности

Build, Go tests, `-race`, lint/vet, benchmarks, browser E2E, реальные
LLM/OAuth/MCP/proxy запросы и искусственная нагрузка **не запускались**.
Изменение аудита — только документация. Ни одного test failure/flake этого
прохода не наблюдалось; checkpoint не подменяет результаты текущей проверки.
Новые случаи подтверждены исходниками, но interleavings и частота их проявления
не измерялись. Все платформы, SDK entry points, DNS protocol conformance и
crash recovery исчерпывающе не проверялись.

Недельный diff и существующий WT прошли `git diff --check`. Для сохранности
до записи отчёта зафиксированы status и SHA-256 всех существовавших modified/
untracked файлов; удалённый `web/dist/.gitkeep` учтён как отсутствие файла.
Перед staging проверены совпадение всех исходных 48 записей и fingerprints,
пустой index и единственное добавление этого отчёта. Контроль commit включает
явный pathspec этого файла, staged diff/check и проверку фактического file list
после создания; чужие пути не включаются в staging.

В частности, защищены перечисленные пользователем пути:
`internal/agent/agent_turn_failure.go`, `internal/app/app_run_reviewer.go`,
`internal/cmd/ping.go`, `internal/cmd/ping_test.go`,
`internal/agent/quota_reset.go`, `internal/agent/quota_reset_test.go`,
`internal/agent/quota_reset_turn_test.go` и **` D web/dist/.gitkeep`**.
То же правило применено ко всем дополнительным исходным правкам. Код,
CHANGELOG, версии, конфигурация, чужие staged entries и remote не изменяются.
Частные абсолютные пути компьютера в отчёт не включены.

## Минимальные обязательные исправления и финальный release verdict

Для текущего **committed HEAD** нельзя засчитать 14 исправлений, существующих
только в WT. Пакет должен пройти отдельное принятие и войти в release candidate;
этот documentation commit его не включает и не разрешает commit/push кода.

После принятия уже подготовленных исправлений минимальный оставшийся gate:

1. **R8-1:** сохранить provider Network через Load/Reload catalog merge.
2. **R8-2:** не превращать отмену незавершённого turn в успех по tool-step;
   сохранить правильный приоритет настоящего завершённого результата.
3. **R2-5 + R5-2:** очистить runtime error chain и прекращать auth exchange при
   ошибке обязательной network policy. Это необходимо для обещанной сетевой
   изоляции и безопасной диагностики.
4. **R8-3:** привязать итог к вызову либо явно ограничить host полной
   сериализацией ExecuteRun одной сессии.
5. **R4-2:** восстановить address-family fallback либо явно исключить его из
   поддерживаемого профиля DNS-over-proxy/DoH. Без такого ограничения
   networking candidate остаётся неполным.

Acceptance должен быть целевым: Load/Reload с mock catalog и локальным
transport, cancellation после tool-step, конкурентный run перед reconcile,
runtime URL error и auth-client build failure, A-failure/AAAA-success.
Для concurrency случаев нужны управляемые барьеры; успешный случайный serial
run и одни проверки наличия полей недостаточны. Runtime acceptance в этом
аудите не выполнен и не обещан.

**Финальный verdict для `d67a9c7e` / `0.2.0-alpha.2`: NO-GO общей alpha.**
**Для прочитанного WT-кандидата: также NO-GO.** Даже исключение reviewer,
web и custom networking не устраняет R8-2. Более узкие ограничения здесь
не вводились и принятыми пользователем не считаются. Отчёт фиксирует
обнаруженные механизмы и необходимый gate, а не разрешение на выпуск.
