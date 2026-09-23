# Rush: независимый release-readiness аудит, round 12

## Вердикт

**NO-GO для выпуска `78b0143d2b3a66596f44462d5a7a97432f8b61a5` с полным
заявленным набором возможностей.** Подтверждены по committed-коду два открытых
механизма: **P0 — 0, P1 — 1, P2 — 1, P3 — 0**.

| ID | Приоритет | Открытый механизм |
| --- | --- | --- |
| R12-1 | P1 | Отмена во время durable drain может вернуть успех до подтверждения результата очереди: authoritative row и live finish принадлежат разным execution. |
| F6, остаток auth lifetime | P2 | OAuth client построен из snapshot A, но refresh token заново читается из snapshot B; новая пара provider/global network внутри client builder не закрывает эту следующую границу. |

Старый NO-GO round 11 **не перенесён автоматически**. Его P1 R9-1 закрыт;
R9-2, описанные в round 11 остатки R8-3, R4-2 и F8 также закрыты. У F6 закрыты
прежний pinned rebuild и смешение provider/global при построении auth client,
но подтверждён следующий разрыв между client и используемым token. Поэтому
итог closure шести позиций: **пять закрыты полностью в объёме прежних
контрпримеров, F6 закрыт частично**. Новый P1 R12-1 имеет другой механизм.

Это статический аудит с разбором production callers и regression-test oracles.
Ниже «закрыт» означает устранение конкретной трассы committed-кодом, а не
заявление о выполненном здесь тестовом прогоне. Частота описанных interleavings
и вероятность их появления на конкретной машине не измерялись.

## Зафиксированный срез и сохранность checkout

- Интервал, включительно: **2026-09-16 13:43:23 CEST —
  2026-09-23 13:43:23 CEST**, то есть
  **2026-09-16T11:43:23Z — 2026-09-23T11:43:23Z**.
- Отбор по **committer date**, только среди предков заданного HEAD.
  Author date, текущая дата запуска и другие ветки не определяют диапазон.
- HEAD: `78b0143d2b3a66596f44462d5a7a97432f8b61a5`,
  committer date `2026-09-23T13:38:13+02:00`.
- Base: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  committer date `2026-09-11T14:08:09+02:00`. Это родитель первого включённого
  коммита `57fffb0504ce0906e44ad8d76a3a612d6dfc1960` от 19 сентября.
- Date-filtered список и `base..HEAD` дают одно и то же множество:
  **48 коммитов, 0 merge commits**. Между base и первым включённым коммитом
  нет пропущенной части недельной серии.
- Полный committed diff: **152 файла, 19984 добавления, 1401 удаление**.
  Сам `78b0143d`: **38 файлов, 2562 добавления, 108 удалений**.
- До создания audit worktree исходный checkout имел пустой
  `git status --porcelain=v1`, включая index и untracked files, и ровно
  заданный HEAD. Все дальнейшие изменения сделаны в отдельном worktree,
  созданном от этого SHA на ветке `audit/2026-09-23-weekly-round12`.
  Исходный checkout использовался только для read-only проверок.
- Все номера строк в отчёте относятся к **`78b0143d`**. Прочитаны base/head,
  состав полного committed diff, production patches и связанные callers;
  целевые tests проверены по содержанию assertions, а не по названию.
  Diff не подменён содержимым когда-либо существовавших незакоммиченных fixes.

Воспроизводимый расчёт диапазона:

```text
git status --porcelain=v1
git rev-parse HEAD
git log 78b0143d2b3a66596f44462d5a7a97432f8b61a5 \
  --since-as-filter=2026-09-16T11:43:23Z \
  --until=2026-09-23T11:43:23Z --reverse --format='%H %cI %s'
git rev-parse 57fffb0504ce0906e44ad8d76a3a612d6dfc1960^
git rev-list ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23..78b0143d2b3a66596f44462d5a7a97432f8b61a5
git diff --stat ccc2f9b5 78b0143d
git diff --check ccc2f9b5 78b0143d
```

Сопоставлены [round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md),
[round 6](2026-09-22-weekly-commit-audit-round6.md),
[round 7](2026-09-22-weekly-commit-audit-round7.md),
[round 8](2026-09-22-weekly-commit-audit-round8.md),
[round 9](2026-09-22-weekly-commit-audit-round9.md),
[round 10](2026-09-23-weekly-commit-audit-round10.md) и
[round 11](2026-09-23-weekly-commit-audit-round11.md).
Round 10 теперь входит в committed tree через `78b0143d`; его описание
«fixes remain uncommitted» относится к времени того отчёта. Round 11 проверял
`02cb64e6`; его утверждение об отсутствии fixes в том HEAD было правильным,
но не описывает рассматриваемый здесь новый commit.

## Повторная проверка всех шести findings round 11

### R9-1: закрыт прежний P1 live replacement

`internal/agent/mailbox_interrupt.go:282,339` и
`internal/agent/mailbox_ownership.go:262` помечают извлечённую replacement.
`internal/agent/agent_run.go:520,578,583` переносит identity callback только
на такой handoff и снимает маркер. Следовательно, завершившийся mB попадает
в capture исходного dispatcher; обычный queued C этот callback не наследует.
`internal/agent/coordinator_run.go:363,496,507` и
`internal/app/app_run_terminal.go:114,134` доводят собственный terminal ID
до результата. Прежнее mA/canceled вместо mB/end_turn из round 11 устранено.

Прочитан `TestRunOwned_ReplacementInheritsOriginalCallsIdentityCallback_R9_1`
в `internal/agent/r9_1_live_replacement_result_identity_test.go:54`:
реальный mailbox cancel, две provider calls, committed mB/end_turn и наличие
mB.ID в callbacks. Это проверка dispatcher/callback, не полноценный тест
JSON/terse envelope; последняя часть closure подтверждена трассировкой consumer.
Новый R12-1 ниже касается другой, durable, ветви и другого момента отмены.

### R9-2: закрыта потеря session slot при 401 rebuild

После наследования пропущенных slots `RunWithOverrides` теперь повторно
записывает дополненную пару в context:
`internal/agent/coordinator_run.go:637,673`. Rebuild через
`internal/agent/credentials.go:352` читает именно эту пару. Session smart S
больше не превращается в global G при fast-only override и успешном refresh.
Reviewer по-прежнему очищает persistence, поэтому временный выбор не должен
перезаписать durable slots.

`TestRunWithOverrides_401RebuildKeepsInheritedSmartSlot_R9_2`
(`internal/agent/r9_2_overrides_rebuild_inheritance_test.go:36`) проверяет
разные session/global providers, 401, две calls и идентичность smart slot
обеих попыток. Symmetric smart-only/fast inheritance следует из общей записи
пары; отдельный runtime-прогон этой симметрии в round 12 не выполнялся.

### R8-3: закрыты прежние reserved/result/tool-inventory gaps

`internal/agent/coordinator_run.go:739,791` подключает recorder и в
`RunWithReservedOwnership`, который обходит `runInternal`.
`internal/app/app_run_terminal.go:90,93,114,134` отказывается выбирать
произвольный terminal при пустом ownID, отбрасывает чужие assistant IDs и
останавливает учёт после собственного terminal. Live consumer проверяет
owner (`internal/app/app_run_reviewer.go:355`). Поэтому прежние B-answer
и B-tool в результате A после освобождения A устранены.

Прочитаны `TestRunWithReservedOwnership_RecordsCallResultIdentity_R8_3`
(`internal/agent/r8_3_reserved_ownership_result_identity_test.go:34`),
`TestExecuteRunReconciliationExcludesLaterSessionOwnersToolCalls`
(`internal/app/app_run_r8_test.go:156`), проверки empty recorder, foreign
live events и interleaved tools в `internal/app/app_run_cycle7_test.go:384,573,595`.
Reserved test использует mock session agent; независимый B проверяется другими
consumer tests. Это несколько составляющих oracle, а не заявленный здесь
единый исполненный reserved+override+concurrent-B E2E.

### F6: прежние места исправлены; auth lifetime остаётся частичным

`internal/agent/coordinator_providers.go:459` возвращает cfg/provider одной
генерации; `rebuildPinnedModel:428` больше не читает новый snapshot.
Оба production callers передают эту пару:
`coordinator_subagents.go:208,212`, `coordinator_summarize.go:118,122`.
Дополнительно `coordinator_providers_auth.go:160,162,166` заново разрешает
provider из того же snapshot, из которого берёт global network для auth client.
Старые сочетания cfgB/providerA в этих местах **закрыты**.

Прочитаны четыре теста в
`internal/agent/coordinator_pinned_rebuild_snapshot_test.go`:
`TestRebuildPinnedModel_UsesCallerPinnedSnapshotForNetwork`,
`TestSummarize_ProactiveRefreshRebuildCarriesFreshProviderPair`,
`TestRunSubAgent_401RebuildCarriesFreshProviderPair`,
`TestRefreshOAuth2Token_UsesFreshSameSnapshotProviderPair`.
Последний останавливается на malformed network build, до ConfigStore refresh.
Следующее чтение credential внутри store он не проверяет. Единственный
оставшийся F6/P2 описан ниже; закрытый pinned-rebuild механизм не посчитан повторно.

### R4-2: закрыт slow-address starvation

`internal/nettransport/client.go:166,199,227` выделяет каждому раннему
кандидату часть оставшегося бюджета и отменяет attempt context после dial.
Для прежнего примера из трёх адресов и 60s два первых не могут штатно занять
по 30s: при отсутствии иных задержек их лимиты около 10s и 12.5s, последний
получает оставшееся время с cap 30s. Обе families уже сохранялись в resolver.
Это устраняет конкретное недостижение третьего доступного адреса.

Прочитаны `TestPerAddressTimeoutSplitsRemainingBudget` и
`TestResolvedDialerBudgetSharedAcrossSlowCandidates`
(`internal/nettransport/audit_round9_test.go:24,95`): первые два doubles ждут
своих deadlines, третий требует дополнительного времени и соединяется;
проверяются все три адреса по порядку. Тест использует три IPv4 адреса,
а полнота A/AAAA отдельно проверяется в `audit_round7_test.go:317,339`.
Family-independent цикл применяется к тому же списку. Нагрузка/blackhole
на реальной сети и ожидание production 60s не выполнялись.

### F8: закрыта потеря primary inventory при пустом reviewer

`internal/app/app_run_reviewer.go:475,480` больше не возвращается раньше
присваивания `toolCallCounts = invocationToolCalls`. Пустая отказавшая фаза
добавляет ноль и сохраняет ранее учтённые primary tools.
`TestFoldLiveToolCallCountsAliasesInventoryEvenWhenPhaseContributedNothing`
(`internal/app/app_run_reviewer_tool_inventory_test.go:123`) содержит именно
oracle primary `view:1` → пустая новая phase → `view:1`; соседние tests
проверяют две успешные фазы и dedup по tool ID. Это unit oracle fallback,
не выполненный в этом раунде интеграционный peak-hours refusal.

## Открытые findings

### R12-1 — P1: отмена durable drain превращает неподтверждённый live finish в успех

**Атрибуция:** новая комбинация ownership-фильтра и двух recorder в
`78b0143d`; callback/live identity hardening round 10. Это не прежний
R9-1 и не повтор закрытого R8-2 с `tool_use`.

**Строки на HEAD:**
`internal/app/app_run_reviewer.go:217,236,257,268,282,284,289,355,553,566,568,575`;
`internal/app/app_run_reviewer_drain_identity.go:70,71`;
`internal/session/run_queue_entry_exec.go:565,601,648,651`.
Production path: `ExecuteRun → canceled original → DrainSessionNow`
параллельно обслуживаемому app event loop; затем отмена родительского context.

После отмены original A loop намеренно сохраняет `callResultRec=A`, но
переключает `eventOwner=B` для live rows durable execution. B становится
authoritative result только после `drainDone` и `adoptDrainIdentity` с
`DrainComplete`. Однако ветвь `ctx.Done()` не учитывает это промежуточное состояние.

Достаточная допустимая последовательность, установленная по исходникам:

1. A записал свой `canceled` terminal и вернул cancellation из-за durable
   interrupt. Parent context ещё жив; loop запускает drain и меняет event owner.
2. B исполняет replacement, публикует свой `end_turn`. Live event обработан:
   `finalReason=end_turn`, `finalText=B`. Исполнение очереди ещё не подтвердило
   Ack: поставить барьер перед/в `AckRunQueueEntry`. Message commit и queue Ack
   являются разными операциями; второй находится после возврата coordinator.
3. Пользователь отменяет parent context либо истекает его timeout.
   Выбирается `ctx.Done()` до получения `drainDone`.
4. Cancel probe читает `callResultRec=A` и успешно находит **A/canceled**.
   В `finish` эта строка передаётся `handleMessageEvent`, но owner-фильтр
   отвергает A, поскольку `eventOwner` уже B. Handler возвращает nil,
   не заменив live `finalReason=end_turn`.
5. `authoritativeTerminal` тем не менее становится true. Проверка на строке
   568 использует оставшийся **B** `finalReason`, очищает `runErr` и
   `isCanceled`, и возвращает успешный JSON/terse outcome.
6. Ack B может затем вернуть ошибку или execution потерять lease; результат
   drain уже не принимается этим вернувшимся вызовом. Даже без ошибки Ack
   успех был заявлен до проверки всего drain, включая оставшиеся queue rows.

Состояние в момент ошибочной проверки:

| Значение | Источник |
| --- | --- |
| `callResultRec`, найденная committed row | A, отменённый original |
| `eventOwner`, live `finalText/finalReason` | B, ещё не подтверждённый drain |
| `authoritativeTerminal=true` | Успешный поиск A, хотя replay A был отброшен |
| `DrainComplete` | Ещё не получен; проверка `adoptDrainIdentity` не выполнялась |

**Impact:** CLI/SDK получает nil error и `exit_reason=end_turn`, хотя durable
результат не подтверждён. При Ack failure сохраняется leased row, которую
штатная recovery вправе исполнить повторно. Обязательное повторение внешнего
side effect не утверждается: ошибка здесь — ложный успешный outcome и потеря
ошибки/незавершённости очереди. Уязвимый сценарий не требует reviewer, чужого
владельца сессии или memory race.

**Почему это новая регрессия:** в parent `c7ada69a` replay A не имел
`eventOwner.Owns`-фильтра, поэтому восстанавливал `canceled` до проверки
подавления cancellation. В новом коде корректный для live events фильтр
использован также для authoritative replay при несовпадающих owners.

**Минимальный fix:** отделить применение authoritative terminal metadata от
фильтра live events; основанием подавления cancel должны быть конкретная
принятая row и outcome её execution. Пока drain не подтверждён, нельзя
соединять authoritative status A с live finish B или объявлять success B.
На отмене вернуть определённый canceled/incomplete outcome и корректно
завершить lifetime drain; обработанный `DrainComplete` должен сохранять
работающий успешный путь.

**Regression-test oracle:** real app loop с DB doubles и барьерами:
A/canceled → drain B/end_turn event обработан → Ack B заблокирован → parent
cancel → Ack возвращает injected error. `ExecuteRun` не должен вернуть nil
error/успешный `end_turn`; ошибка Ack либо отмена с неподтверждённым drain
должны быть видимы, а сообщения и queue row сохраняться согласно контракту.
Повторить с pending второй queue row и с успешным Ack; отдельно сохранить
`DrainComplete → own B` и обычный `A/end_turn → late cancel` positive controls.
Unit-вариант может напрямую проверить `finish(context.Canceled)` при A recorder,
B event owner и live B/end_turn. Никаких sleep-based гонок или нагрузки не нужно.

**Пробел нынешних tests:** `app_run_cycle7_test.go:398,510,545` проверяет
adoption и live output отдельно, `app_run_r8_test.go:29` — обычную отмену
после tool step. `p575_background_outcome_handoff_test.go:318` проверяет Ack
failure самого pump. Ни одна из этих проверок не проходит указанное
состояние app consumer с двумя owners и parent cancel до `drainDone`.
Reproduction в round 12 не исполнялся.

### F6, остаток — P2: сеть и token расходятся после исправленного auth client builder

**Атрибуция:** неполный network/auth lifetime из `eb59df12`/`53e18976`;
`78b0143d` закрывает прежнюю пару provider/global, но не следующий store read.
Один остаточный F6, без повторного счёта уже исправленных мест.

**Строки на HEAD:**
`internal/agent/coordinator_providers_auth.go:160,162,166,170`;
`internal/agent/coordinator_providers_network.go:27,28`;
`internal/config/store_oauth.go:226,227,228,236,240,243,252,254,276`.
Production path: proactive или 401 refresh Copilot/Hyper → coordinator
разрешает client → ConfigStore самостоятельно разрешает token → OAuth helper.

`RefreshOAuthTokenWithClient` получает только provider ID и готовый client;
связь с переданным coordinator snapshot отсутствует. Его первое действие —
новый `s.loadSnapshot()`. Следовательно, атомарность provider/global внутри
`resolveProviderHTTPClient` не распространяется на реально отправляемый credential.

Статический контрпример с поддерживаемым reload:

1. Coordinator захватывает A и строит client CA из network A. Для особенно
   простого случая A не задаёт network, CA равен nil, то есть default route.
2. До входа в `RefreshOAuthTokenWithClient` reload публикует B для того же
   provider: token TB и обязательный proxy/DNS NB. TB требует refresh.
   На диске также B; preflight disk check не возвращает другой свежий token.
3. Store читает B и вызывает `hyperExchangeTokenFn(ctx, CA, TB.RefreshToken)`
   либо аналогичный Copilot helper. Получается **CA + TB**, хотя B требует NB.
4. CAS после ответа сравнивает expected TB с B и может успешно завершиться:
   он защищает публикацию token, но не выбирает маршрут уже отправленного запроса.

**Impact:** при hot reload auth отправляется по сети другого поколения:
может обойти только что настроенный provider proxy либо не обновить token,
если доступ к auth endpoint возможен лишь по NB. Не утверждаются TLS bypass
или отправка token произвольному неизвестному HTTP endpoint. Это логическая
несогласованность snapshot, не доказанная Go memory race. Без reload в этом
интервале нынешняя configured-client передача работает.

**Минимальный fix:** захватывать credential и network как одну immutable пару
до network I/O. Например, store должен принимать snapshot-derived expected
token вместе с client либо разрешать client через factory от собственного
captured snapshot. При смене поколения до отправки можно пересобрать всю пару;
при смене после отправки сохранять нынешний CAS. Один mutex на весь HTTP
request не требуется. Простое повторное чтение только provider или проверка
generation после отправки не исправляют выбранный маршрут.

**Regression-test oracle:** барьер после создания CA, до store token lookup;
опубликовать B с другим network и token. Перехват exchange фиксирует одновременно
client/route и refresh token. Допустимы согласованные CA/TA, CB/TB либо отказ
до отправки; **CA/TB запрещён**. Проверить proactive и 401 callers, а затем
оставить положительные CAS tests на параллельную замену и durability error.
Всё воспроизводимо локальными doubles без реальных credentials/сети.

`TestRefreshOAuth2Token_UsesFreshSameSnapshotProviderPair`
(`coordinator_pinned_rebuild_snapshot_test.go:348`) проверяет другой,
**уже закрытый** интервал: reload до создания client. Его malformed policy
останавливает код до store. `TestRefreshOAuthToken_SurvivesReloadDuringNetworkCall`
(`store_oauth_test.go:289`) проверяет публикацию после network call;
client/token pairing между этими двумя границами не покрывается.
Описанный counterexample не запускался.

## Round 10 identity и OAuth CAS: что действительно принято по коду

Успешный durable handoff теперь несёт весь набор assistant IDs и terminal ID:
`internal/session/run_queue_entry_exec.go:131`,
`run_queue_admission.go:44,175,203`, `run_queue_drain_session.go:77,136,400`.
ID snapshots и capture защищены mutex; callbacks admission вызываются после
его unlock. App на обычном `drainDone` принимает только подтверждённый набор
и terminal, входящий в него (`app_run_reviewer_drain_identity.go:70`).
Прежний canceled-ID после подтверждённого drain и потеря intermediate IDs
закрыты. R12-1 обходит именно эту исправленную финальную ветвь через cancel.

Прочитаны tests waited/sequential/live identities в
`internal/session/p575_background_outcome_handoff_test.go:73,115,150`,
durable coordinator recorder test, tests terminal-last ordering и foreign IDs
в `internal/app/app_run_cycle7_test.go`. Round 10 сообщает об обычных и race
прогонах другой рабочей версии; их результаты не присвоены round 12.

OAuth CAS в `78b0143d` имеет содержательную защиту:

- `internal/config/store_oauth.go:317,325,331` под config write lock сравнивает
  token и соответствующий API key на диске с expected, затем использует
  fingerprint-aware commit. Поздний refresh не затирает уже изменённые bytes.
- `store_oauth.go:378,389` отдельно сравнивает in-memory credential под
  `publishMu`; обновляет копию текущего provider, сохраняя другие свежие поля.
- Committed durability error отличён от отсутствия commit; по возможности
  перечитывается disk token, память синхронизируется, а ошибка сохраняется
  (`store_oauth.go:282,288,298,305`). Это не безусловный optimistic success.
- Runtime API-key refresh заменяет только key при совпадении template и старого
  resolved key (`store_oauth.go:171`, `coordinator_providers_auth.go:189`).
  `SetupGitHubCopilot` клонирует headers перед записью (`config.go:219`).

Прочитаны `TestRefreshOAuthToken_DiskReplacementWinsConcurrentRefresh`,
`TestRefreshOAuthToken_CommittedDurabilityErrorSynchronizesMemory`,
`TestSetupGitHubCopilotClonesExtraHeaders` (`store_oauth_test.go:414,464,512`)
и два runtime API-key tests (`coordinator_c6_auth_retry_test.go:133,165`).
Их assertions соответствуют перечисленным CAS/ownership гарантиям. Прежний
overwrite не объявлен открытым из-за нового F6: publication CAS и outbound
network pairing — разные границы. Revert-checks из commit message здесь
не повторялись, crash durability и все filesystem outcomes не сертифицированы.

## Сверка остальных прежних findings

| Finding | Повторная оценка на HEAD |
| --- | --- |
| F1, F2 | Закрыты: reviewer получает свой ctx, cleared reservation и role/tool policy; `req.Credentials != nil` исключён из auto-review (`app_run.go:519`, `app_run_reviewer.go:794,875,902`). |
| F3 / R2-2 | Прежние prefix/tool-step/mid-word/whitespace-only потери закрыты: owned messages проходят chain walk, error fragments сохраняются, join byte-exact (`app_run_terminal.go:186,213,249`). Прочитаны соответствующие continuation oracles; один исторический механизм. |
| F4, R2-1, R6-1 | Прежние unbounded handshake/initial-dial ветви закрыты: CONNECT/DNS guards, SOCKS budget, DoH client timeout, 30s TCP dial и 10s TLS bound. Request cancellation не приравнена к немедленному исчезновению каждого внутреннего dial. |
| F5 | Прежний неограниченный idle lifetime закрыт: 90s main/DoH idle timeout, transport cache capacity 8, eviction cleanup после unlock и forwarding через wrappers. Это не гарантия мгновенного освобождения каждого App transport. |
| F7 | Закрыт: `CallOptionsSpec.IdleTimeout` и converters сохраняют unset/positive/disabled. Прочитан `TestF7_IdleTimeoutSurvivesDurableRoundTrip`; additive field не требует изменения версии old rows. |
| F9 | Закрыт: formatter отвергает null/scalar/array до обращения к полям; backend нормализует bare null. Обе web surfaces используют общий helper; web oracle включает non-object input. |
| R2-3 | Закрыт: reviewer сохраняет `FailIfSessionBusy`, mailbox admission отказывает атомарно без enqueue. |
| R2-4 | Закрыт прежний переход после cancel: reviewer gate учитывает cancel и `canceledAfterCommit`; reset очищает terminal/context/cancel вместе. R12-1 возникает внутри одной drain-фазы. |
| R2-5 | Прежние URL-secret примеры закрыты: безопасный endpoint text, unwrap runtime DoH `url.Error`, redaction generic finish. Не утверждается очистка любого произвольного provider error body. |
| R2-6 | Закрыт неправильный eviction-test oracle: тест использует собственный `newTransportCache()` (`nettransport/lifecycle_test.go:675`), а не отсутствие других parallel inserts. |
| R3-1, R7-1 | Прежние чужое/stale evidence, assistant callback race и queued classification закрыты: собственный ID, отдельный mutex/sealed capture на attempt, явный queued admission gate перед classifier. |
| R3-2, R4-1 | Закрыты: HTTP proxy получает default port 80 с корректным IPv6 JoinHostPort; CONNECT head ограничен, после parse limit снят и buffered payload сохранён. |
| R5-1 | Прежнее reviewer R → 401 → S закрыто: override передаётся rebuild; session-inheritance остаток R9-2 разобран отдельно выше и также закрыт. |
| R5-2 | Прежние default-route refresh и downgrade после client-build error закрыты; сохраняется timeout. Остаточная snapshot-согласованность посчитана только в F6. |
| R5-3 | Закрыт: terse выводится после выбора последней фазы через `flushTerseOutput`, поэтому PRIMARY и REVIEW не склеиваются прежним способом. |
| R6-2 | Закрыт: оба clamp требуют `hardCap > 0`; ноль не превращается в абсолютный start+idle deadline. |
| R8-1 | Закрыт: catalog provider merge переносит `Network` (`config/load_providers.go:132`). |
| R8-2 | Прежнее `tool_use → unfinished step → cancel` не подавляет cancel: success suppression ограничен `end_turn`. Новый R12-1 использует end_turn другого execution, а не этот закрытый tool_use пример. |

## Покрытие полного диапазона и performance

Инвентарь всех 152 изменённых файлов, включая tests и документацию:

| Область | Файлов | Добавлено | Удалено |
| --- | ---: | ---: | ---: |
| Root/release: changelogs, README, npm, schema | 5 | 248 | 13 |
| Audit reports и checkpoints | 18 | 5811 | 0 |
| Agent, исключая MCP tests | 49 | 4537 | 493 |
| MCP tests | 3 | 60 | 14 |
| App | 16 | 3507 | 483 |
| CLI и встроенные command descriptions | 10 | 276 | 269 |
| Config | 9 | 511 | 36 |
| Log HTTP wrappers | 2 | 68 | 1 |
| Nettransport | 13 | 3911 | 0 |
| OAuth | 4 | 125 | 10 |
| Server | 4 | 42 | 5 |
| Session/queue | 6 | 348 | 26 |
| Version | 1 | 1 | 1 |
| Web | 12 | 539 | 50 |

Помимо двух findings прослежены ownership transfer, mailbox replacement/ordinary
queue, отказы admission, sealed callbacks, buffered completion channels, cancel
и interrupt-ticker join. Нового доказанного циклического deadlock/livelock или
несинхронизированного доступа памяти в этих новых механизмах не установлено.
Это не утверждение о race-clean состоянии проекта. Nil-helper edge case без
показанного production caller не повышен до релизной panic.

На network paths проверены закрытие conn/body на errors, guard join до передачи
tunnel, снятие provisional deadline, cache lock/eviction ordering и отсутствие
тайного default-route fallback после client build error. Snapshot F6 является
логической ошибкой согласования значений; новый P1 — ошибкой принятия результата,
а не зависанием на mutex. Межпользовательская авторизация сессий не переоценивалась.

Прослежены quota/reset guidance и перенос parser из ping, peak-hours message
от config через CLI/WS до frontend, null-safe formatter, timestamps, model/effort
metadata и textarea cap. Недельные MCP commits меняют test isolation, сравнение
directory identity и timeout тестовых барьеров; production process-tree code
ими не меняется. Рост test timeout не принят как доказательство устранения
всех flakes. Existing version metadata в Go/npm согласовано; версии не менялись.

**Отдельных performance findings нет.** Проверенные реальные пути:

- Retry classification и terminal reconciliation читают историю: O(N+S) по
  числу messages N и объёму данных S; повторные List/baseline reads существуют.
- Capture ID имеет амортизированный O(1) insert; snapshot множества требует
  O(A) памяти/копирования по числу assistant IDs A. Это стоимость результата
  конкретного execution, а не доказанная утечка на каждый token event.
- `joinContinuationText(acc,next)` копирует накопленный prefix. Сумма копий
  равна сумме длин prefixes; при K равных fragments длины L худшая форма
  O(K²L). Default retry budget ограничен двумя retries; доказанного
  пользовательского замедления от этого в данном аудите нет.
- Custom resolver хранит O(M) адресов и пробует до M dials под бюджетами;
  исправленный R4-2 оценивается как availability/failover, не micro-optimization.
- Web metadata добавлено в уже существующий проход messages/parts;
  новый per-message timer не добавлен. JSON preview стоит O(размер input).

Ни один из этих наблюдаемых costs не объявлен P2/P3 только за линейность,
аллокацию map/slice или отсутствие singleflight. Замеры latency, heap и
искусственная нагрузка не выполнялись; доказанного user impact для отдельного
perf finding здесь нет.

## Выполненные проверки и пределы результата

Выполнены read-only `git status`, `rev-parse`, `log`, `rev-list`, `show`,
`diff`/`--stat`/`--numstat`, `blame`, чтение и поиск по исходникам и прошлым
reports. Сверены fixed range/base/head и отсутствие изменений исходного
checkout; выполнен `git diff --check` для base..HEAD без замечаний.
Прочитаны относящиеся к шести closure и двум открытым механизмам тестовые
fixtures/assertions. Это проверка соответствия oracle механизму, не выполнение tests.

**Не запускались:** Go tests, включая узкие; полный suite; любой race suite
или `-race`; build; lint/vet; benchmarks; browser E2E; remote CI;
реальные LLM/OAuth/MCP вызовы; сетевые fault probes и искусственная нагрузка.
Не создавались временные regression-test файлы и не делались revert-checks.
Нет наблюдавшихся в этом раунде test failures/flakes, потому что tests не
исполнялись. Чужие claims о зелёных прогонах в reports/commit message
не являются результатами этой проверки. Все interleavings, все SDK providers,
ОС и crash recovery этим статическим аудитом не сертифицированы.

Изменение аудита — только этот Markdown. Перед semantic commit проверяются
единственный staged path, полный staged diff и `git diff --check` для index
и worktree. Commit сохраняется на `audit/2026-09-23-weekly-round12`, затем
чистый audit worktree удаляется штатным `git worktree remove`; код и исходный
checkout не изменяются. Push, tag и cherry-pick не выполняются.

Для повторного release gate нужны исправление R12-1 и согласованная
credential/network пара F6 с приведёнными regression oracles на новом
committed HEAD. Отсутствие runtime-прогона само по себе не оформлено как
дополнительный finding; NO-GO основан на двух конкретных source-level механизмах.

## Полный список включённых коммитов

Порядок от base к HEAD; отчёт round 12 и его будущий commit сюда не входят.

```text
57fffb05  continuation after partial output
14dd4a54  web tool arguments/timestamps
924add8a  web sub-agent model/effort
10e32c56  always-visible timestamps
87ee064f  MCP Windows env isolation
64409ab3  MCP Windows test fixes
df0051b1  checkpoint 2026-09-20-0101
ccb510ff  automatic reviewer pass
0f445020  network config/resolvers
85ee8836  checkpoint 2026-09-20-1040
eb59df12  provider HTTP network wiring
180f1544  nettransport test noctx
5c20c771  provider-network test noctx
bf9f6d3d  checkpoint 2026-09-20-1414
f7362f7f  timeout defaults/idle backstop
e547e12e  audit round 1
dd03ecf0  checkpoint 2026-09-21-2116
97c22050  web metadata tests
babd295c  reviewer/credentials/continuation fixes
fd18cf0c  network bounds/transport cache
31e90d2e  audit round 2
d0966df9  SOCKS handshake bound
54acbf71  continuation tool steps/reviewer fail-fast
c9eb9a98  tagged switches
295cc386  audit round 3
8aabba4d  byte-exact continuation join
2169dbe3  attempt-scoped classifiers
c32c72d6  parallel test annotation
a4f99d8b  audit round 4
6e18502f  audit round 5
6f134f0a  checkpoint 2026-09-22-1317
b0a55aab  attempt-owned evidence
4357e157  peak-hours custom message
a275e323  alpha.2 metadata/changelog/schema
fbc8a177  peak-hours textarea labels
07cabfae  audit round 6
1420acb1  test formatting
52a456b8  retry test file split
4b2b2a0c  per-attempt evidence lifetime
0c4bae10  audit round 7
d67a9c7e  checkpoint 2026-09-22-1829
abc98c00  audit round 8
53e18976  round 7/8 follow-up batch
b8aeaea4  audit round 9
bf82642f  checkpoint 2026-09-23-0055
02cb64e6  textarea growth cap
c7ada69a  audit round 11
78b0143d  identity/OAuth CAS/F6 follow-up; round 10 report
```
