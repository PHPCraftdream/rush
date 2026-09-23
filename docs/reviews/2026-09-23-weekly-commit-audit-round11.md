# Rush: независимый release-readiness аудит, round 11

## Вердикт и граница результата

**NO-GO для выпуска committed HEAD `02cb64e658f96041d35d2c0d45c207b1318af2a8`
с полным заявленным набором возможностей.** Подтверждены **6 актуальных findings:
P0 — 0, P1 — 1, P2 — 5, P3 — 0**. Это повторно проверенные открытые механизмы
round 9, а не шесть новых дефектов. Дополнительных самостоятельных findings
в просмотренных недельных изменениях не установлено.

| ID | Приоритет | Состояние именно committed HEAD |
| --- | --- | --- |
| R9-1 | P1 | Успешная in-process replacement возвращается как отменённая первоначальная попытка. |
| R9-2 | P2 | 401 rebuild теряет model slot, первоначально унаследованный из сессии. |
| R8-3, остаток | P2 | Reserved override не публикует result identity; terminal и tool inventory могут принадлежать другому вызову. |
| F6, остаток | P2 | Pinned rebuild соединяет provider и global network из разных snapshots. |
| R4-2, остаток | P2 | Медленные первые адреса исчерпывают dial budget до доступного следующего адреса. |
| F8, остаток | P2 | Reviewer pre-turn refusal стирает primary tool inventory из итогового envelope. |

У исходного checkout есть незакоммиченные кандидаты исправлений этих механизмов.
Они не переносились в audit worktree и **не засчитаны как shipped fixes**.
Untracked round 10 сообщает **scoped audit clean после незакоммиченных правок**,
а не clean для приведённого выше SHA. Его тестовые результаты не являются
результатами этого прохода.

P1 блокирует выпуск затронутого поддерживаемого сценария; P2 требует исправления
либо явно принятого ограничения сценария. Никакие ограничения release scope
настоящим отчётом не считаются принятыми.

## Точный срез и метод

- Интервал: **2026-09-16 11:27:12 CEST — 2026-09-23 11:27:12 CEST**,
  то есть **2026-09-16T09:27:12Z — 2026-09-23T09:27:12Z**. Отбор по committer
  date среди предков фиксированного HEAD; не плавающее «семь дней от сейчас».
- HEAD: `02cb64e658f96041d35d2c0d45c207b1318af2a8`,
  committer date `2026-09-23T08:27:10+02:00`.
- Base: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`,
  `2026-09-11T14:08:09+02:00`; это родитель первого включённого коммита
  `57fffb0504ce0906e44ad8d76a3a612d6dfc1960` от 19 сентября.
- `git log --since-as-filter='2026-09-16T09:27:12Z'
  --until='2026-09-23T09:27:12Z' <HEAD>` и `git rev-list <base>..<HEAD>`
  дают одинаковое множество: **46 коммитов, 0 merge commits**.
  Полный diff: **131 файл, 17095 добавлений, 1341 удаление**.
- Проверены полный состав base/head diff, production patch, связанные
  callers и целевые тестовые oracles. Предыдущие отчёты использованы как
  проверяемые утверждения; выводы ниже повторно выведены из committed-кода.
  `git blame` отделяет недельную регрессию от более старого первичного механизма.
- Все ссылки вида `path:line` ниже относятся к **`02cb64e6`**, независимо от
  сдвига строк в незакоммиченных исправлениях. Доказательства — статические
  трассы и допустимые последовательности; описанные reproductions не запускались.
- Работа выполнена в отдельном worktree от указанного HEAD, на ветке
  `audit/weekly-round11-20260923`. Исходный checkout использован только для
  чтения status, untracked round 10 и relevant diff кандидатов исправлений.
  При входе там было 30 modified tracked files, 7 untracked files и пустой index.

После проверенного round 9 SHA `53e18976093eaf4b1cdbe78dfd756227f608090f`
в committed history добавлены только сам отчёт round 9 (`b8aeaea4`), checkpoint
(`bf82642f`) и увеличение textarea cap (`02cb64e6`). Между `53e18976` и HEAD
**нет изменений `internal/`**. Поэтому текст checkpoint об исправленных задачах
не доказывает наличие этих исправлений в релизном дереве.

### Карта недельной серии

| Коммиты | Проверенная область |
| --- | --- |
| `57fffb05`, `2169dbe3`, `b0a55aab`, `4b2b2a0c` | Continuation, отказ admission, lifetime и синхронизация attempt evidence. |
| `ccb510ff`, `babd295c`, `54acbf71`, `8aabba4d` | Reviewer, credentials, reservation, terminal reconciliation, сохранность байтов ответа. |
| `0f445020`, `eb59df12`, `fd18cf0c`, `d0966df9` | Provider network, proxy/DNS/DoH, handshakes, deadlines, transport ownership. |
| `f7362f7f` | Timeout defaults, per-call idle policy, watchdog и durable mirror. |
| `14dd4a54`, `924add8a`, `10e32c56`, `97c22050`, `02cb64e6` | Tool formatter, timestamps, sub-agent metadata, textarea resize и web coverage. |
| `4357e157`, `fbc8a177`, `a275e323` | Peak-hours message, UI labels, schema, согласованность release metadata. |
| `87ee064f`, `64409ab3`, `180f1544`, `5c20c771`, `c9eb9a98`, `c32c72d6`, `1420acb1`, `52a456b8` | MCP test isolation, directory identity, context/lint corrections, разбиение tests. |
| `53e18976` | Follow-up batch: result/admission recorders, network/auth fixes, config merge, quota guidance. |
| Остальные 16 | Девять audit reports и семь checkpoints; production-код не меняют. |

Сопоставлены [round 1](2026-09-21-weekly-commit-audit.md),
[round 2](2026-09-21-weekly-commit-audit-round2.md),
[round 3](2026-09-22-weekly-commit-audit-round3.md),
[round 4](2026-09-22-weekly-commit-audit-round4.md),
[round 5](2026-09-22-weekly-commit-audit-round5.md),
[round 6](2026-09-22-weekly-commit-audit-round6.md),
[round 7](2026-09-22-weekly-commit-audit-round7.md),
[round 8](2026-09-22-weekly-commit-audit-round8.md) и
[round 9](2026-09-22-weekly-commit-audit-round9.md).

## Findings на committed HEAD

### R9-1 — P1: результат принятой replacement заменяется отменённым ответом

**Атрибуция:** регрессия `53e18976`, не исправлена последующими коммитами.
**Production path:** `ExecuteRun → runTurnPhase → Run/RunWithOverrides →
runInternal → sessionAgent.runOwned`, параллельно разрешённый
`InterruptAndSend` для той же сессии.
**Строки:** `internal/agent/coordinator_run.go:313,342,467,475`;
`internal/agent/coordinator_interrupt.go:380,401,412`;
`internal/agent/agent_run.go:570,573,576`;
`internal/agent/agent_turn_failure.go:274`;
`internal/app/app_run_terminal.go:95`;
`internal/app/app_run_reviewer.go:470,478,636`.

1. A создаёт assistant `mA`; его callback записывает ID в `curAttempt`.
2. `InterruptAndSend` строит B через `buildCall`, без callback A, и принимает
   его как mailbox replacement. Отменяется generation A, не parent context.
3. A сохраняет `mA/canceled`; `drainAfterCancel` отдаёт B. Dispatcher исполняет
   B под продолжающимся владением, сохраняет `mB/end_turn`, возвращает result B
   и nil error. Присваивание `call = next` не переносит callback.
4. Coordinator запечатывает всё ещё `mA.ID` и передаёт его app recorder.
   `finish` выбирает mA даже если live events уже показали mB. JSON/terse
   возвращают отменённый A; успешный результат B остаётся в transcript.

**Impact:** вызывающий код получает неверные текст и статус уже исполненной
работы. Не требуется race за освобождённую сессию. Durable drain здесь не
помогает: dispatcher вернулся с nil error, его canceled-drain gate не выполняется.
Обязательное повторение side effects и межпользовательская утечка не утверждаются.

**Минимальная рекомендация:** передавать identity фактически завершённой
принятой replacement по всей dispatcher-цепочке, отдельно от retry evidence;
отличать replacement от обычного следующего queued call.
**Oracle:** барьер после создания mA → accepted live interrupt B → mB/end_turn;
`ExecuteRun` обязан вернуть B и успешный outcome в JSON и terse. Контрпроверка:
независимый C после release не должен стать результатом A/B. HEAD-тест
`TestExecuteRunReconciliationIgnoresLaterSessionOwnersRow` проверяет только
позднего независимого владельца, не replacement.

### R9-2 — P2: 401 rebuild теряет унаследованный model slot

**Атрибуция:** ветвь `resolveCallModels` добавлена `53e18976`.
**Production path:** ExecuteRun с одним override, без reserved-ветки →
`RunWithOverrides` → 401 → поддерживаемый OAuth/APIKeyTemplate refresh → rebuild.
**Строки:** `internal/app/app_run_setup.go:360,362`;
`internal/agent/coordinator_run.go:354,609,613,616,627,632`;
`internal/agent/credentials.go:352,354,355`;
`internal/agent/coordinator_models.go:348,349,350,352,363`;
`internal/app/app_run_reviewer.go:825`.

Session smart S отличается от global smart G. Вызов задаёт только fast F;
context содержит `(nil,F)`. `RunWithOverrides` дополняет локальную пару до
`(S,F)`, но не записывает её в context. После 401 и успешного refresh
`resolveCallModels` читает прежние `(nil,F)` и вызывает `applyModelOverrides`:
nil slot берётся из global config. Следующая попытка работает как `(G,F)`.
Concurrent reload и сбой БД не нужны. Без успешного refresh второй попытки нет.

**Impact:** модель/провайдер меняются внутри одного вызова вопреки session choice.
Симметричный reviewer `(R,nil)` сохраняет R, но теряет session fast slot;
это не повтор уже закрытого R5-1 «reviewer R заменяется smart S».
**Минимальная рекомендация:** закреплять полностью разрешённую пару
provider/model/effort до первой попытки и сохранять её при credential rebuild,
не персистируя временный reviewer.
**Oracle:** разные global/session smart и fast; отдельно fast-only и smart-only
override; 401 → refresh → retry. Обе попытки должны использовать одну выбранную
пару и effort; reviewer не должен изменить durable slots. Проверять фактическую
модель повторного запроса, а не только наличие второго вызова.

### R8-3, остаток — P2: не все entry points и поля результата имеют владельца

**Атрибуция:** первичный consumer `ed744c17` от 9 сентября вне интервала;
недельная интеграция `ccb510ff` и неполное исправление `53e18976`.
**Production path:** `ExecuteRun` с `FailIfSessionBusy=true` и model override →
`RunWithReservedOwnership`, затем app reconciliation после освобождения ownership.
**Строки:** `internal/app/app_run.go:435,436`;
`internal/agent/coordinator_run.go:683,719,729`;
`internal/agent/coordinator_call_result.go:39`;
`internal/app/app_run_terminal.go:75,83,95`;
`internal/app/app_run_reviewer.go:234,250,288,470`.

Reserved entry point вызывает `buildCall` и session agent напрямую, минуя
`runInternal`; recorder не заполняется. После успешного A законный B может
завершиться в той же сессии до финального чтения истории A. Оба сообщения новые
относительно baseline; `ownID == ""` на строке 95 разрешает выбрать B. Fail-fast
исключает пересечение владения, но не этот порядок после release.

Даже при непустом ownID обычного Run tool inventory собирается на строках 83–93
до проверки identity: tool call B попадёт в результат A при правильном тексте A.
Live handler также фильтрует session/role, но не call identity. После durable
drain recorder сбрасывается в пустой на строке 234, снова открывая выбор по
baseline; это тот же дефект принадлежности результата, не новый finding.

**Impact:** чужие FinalText/finish/tool counts в результате законного вызова
одной сессии; ошибочный результат особенно существенен для SDK/orchestration.
**Минимальная рекомендация:** единый contract terminal ID и множества assistant
IDs для обычного, reserved и durable исполнения; пустой ID не означает владение
любой новой строкой. Применять фильтр также к inventory и live output.
**Oracle:** барьер A-release → B-complete → A-reconcile, reserved+override и
обычный Run отдельно, B выполняет уникальный инструмент. Результат A должен
содержать только A. Дополнительно: canceled original → подтверждённый durable
drain → независимый C; итог должен принадлежать drain, включая промежуточные
tool rows, а не C или original. HEAD-тест `app_run_r8_test.go:107` не задаёт
reserved override и не даёт B tool call, поэтому этих остатков не исключает.

### F6, остаток — P2: pinned rebuild использует два поколения config

**Атрибуция:** network wiring `eb59df12`, неполный snapshot fix `53e18976`.
**Production path:** proactive/401 refresh configured sub-agent или summarize →
`currentProviderConfig` → `rebuildPinnedModel` → `buildProvider`.
**Строки:** `internal/agent/coordinator_subagents.go:208,212,226`;
`internal/agent/coordinator_summarize.go:118,122,136`;
`internal/agent/coordinator_providers.go:417,423,424,443,444`;
`internal/agent/coordinator_providers_network.go:27,28`.

Первый helper захватывает cfg A и возвращает только provider A. Между helpers
Reload публикует B. `rebuildPinnedModel` берёт новый snapshot B и вызывает
`buildProvider(cfgB, providerA, ...)`. Если A задаёт global proxy PA и provider
DNS DA, а B — PB/DB, результат получает PB/DA: такой конфигурации не было.
Комментарий «One atomic snapshot» не объединяет два чтения.

**Impact:** неверный маршрут либо отказ provider/auth-зависимого исполнения
при поддерживаемом hot reload. Это логическая несогласованность snapshots,
не установленная Go memory race. Обычный `buildModelsFromCfg` уже передаёт
согласованный cfg и не объявляется сломанным по старому контрпримеру.
**Минимальная рекомендация:** передавать cfg и provider из одного snapshot
через обе rebuild-цепочки. Смежная auth-граница
`coordinator_providers_auth.go:157` также сочетает переданный provider с live cfg;
её следует включить в тот же gate, без отдельного счёта finding.
**Oracle:** управляемый reload после захвата provider A, до rebuild; клиент
должен использовать целиком A либо целиком заново разрешённый B. Проверять
реальные sub-agent/summarize callers и transport policy, а не только field merge.

### R4-2, остаток — P2: доступный адрес остаётся без dial budget

**Атрибуция:** `0f445020`/`eb59df12`, текущий fallback добавлен `53e18976`.
**Production path:** configured DNS/DoH provider HTTP client →
`Transport.DialContext` → `resolvedDialer` → `dialTarget`.
**Строки:** `internal/nettransport/client.go:30,35,154,174,180,183,201`;
`internal/nettransport/resolver.go:52,220,310`.

Resolver сохраняет все адреса, IPv4 перед IPv6. Общий dial budget — 60s,
каждый direct TCP dial может занять до 30s; время не делится между кандидатами.
Для `[A1,A2,AAAA1]` два молчащих IPv4 соединения могут занять весь бюджет.
На следующей итерации `dialCtx.Err()` завершает цикл до попытки доступного AAAA1.
При стабильном DNS-порядке повтор запроса получает тот же отказ.

**Impact:** провайдер недоступен через custom resolver при наличии рабочего
адреса. Это ошибка failover, а не предложение ускорить нормальный happy path.
Старые «список содержит один IP» и «AAAA вообще не запрашивается» закрыты.
**Минимальная рекомендация:** выделять кандидатам часть оставшегося времени
либо применять bounded family racing, сохраняя общий deadline, proxy и TLS policy.
**Oracle:** короткий управляемый общий budget, первые два dial ждут своих
deadline, третий доступен; проверить успешную попытку третьего внутри общего
срока и cleanup неудачных попыток. Реальная blackhole-нагрузка не нужна.
HEAD-тест `audit_round7_test.go:269` проверяет быстрый отказ первого из двух
IPv4 адресов; tests `:317,339` проверяют наличие обеих families, не slow fallback.

### F8, остаток — P2: early refusal reviewer теряет primary tool inventory

**Атрибуция:** reviewer `ccb510ff`, частичный inventory fix `53e18976`.
**Production path:** успешный smart primary с инструментом → automatic reviewer
на другом provider → peak-hours refusal до создания assistant row → `finish`.
**Строки:** `internal/app/app_run_reviewer.go:379,400,401,408,475,608,739`;
`internal/agent/coordinator_run.go:218`.

Primary выполняет `view`, reconciliation сохраняет
`invocationToolCalls["view"] == 1`. Reset создаёт пустую phase `toolCallCounts`.
Reviewer отказывает до первой assistant row; его reconciliation не находит
terminal и вызывает `foldLiveToolCallCounts`. Пустая phase вызывает ранний return
до `toolCallCounts = invocationToolCalls`. Итоговый JSON сохраняет ошибку reviewer
и общую стоимость, но `ToolCalls` пуст.

**Impact:** неполный журнал действий и потеря условий для sub-agent accounting
warnings в ошибочном запуске. Обычный successful reviewer уже исправлен.
**Минимальная рекомендация:** финальный envelope должен читать run-wide inventory
и при пустой/отказавшей второй фазе.
**Oracle:** primary tool → reviewer pre-turn refusal/build failure; primary count
остаётся ровно один, error остаётся non-nil, повторного учёта нет. HEAD-тесты
`app_run_reviewer_tool_inventory_test.go:34,91` проверяют successful reviewer и
dedup, но не пустую отказавшую фазу.

## Сопоставление закрытых механизмов rounds 1–9

«Закрыт» означает, что конкретная прежняя трасса устранена committed-кодом;
это не заявление о выполненном в round 11 runtime acceptance.

| Прежний finding | Подтверждённый follow-up на HEAD |
| --- | --- |
| F1, F2 | `babd295c`: reviewer получает свой ctx, reservation очищен; credentialed calls исключены из configured auto-review (`app_run.go:519`, `app_run_reviewer.go:721,827`). |
| F3 / R2-2 | `babd295c`, `54acbf71`, `8aabba4d`, `53e18976`: обход через tool steps и continuation boundaries, whitespace-only fragments сохраняются, join byte-exact (`app_run_terminal.go:170,207`). Посчитаны как один исторический механизм. |
| F4, R2-1, R6-1 | `fd18cf0c`, `d0966df9`, `53e18976`: bounded CONNECT/DNS guards, DoH exchange/TLS/SOCKS и initial TCP deadlines. Прежнее неограниченное ожидание не переносится на HEAD. |
| F5 | `fd18cf0c`: main/DoH idle timeout 90s, cache capacity 8, eviction cleanup вне lock, forwarding через log/Copilot wrappers. |
| F7 | `53e18976`: `session.CallOptionsSpec.IdleTimeout` и оба converters (`call_data_conversion.go:101,141`) сохраняют unset/positive/disabled значения. |
| F9 | `53e18976`: `web/src/toolFormat.ts:18` проверяет null/scalar/array; обе render-поверхности используют helper. |
| R2-3 | `54acbf71`: reviewer сохраняет `FailIfSessionBusy`; mailbox отказывает атомарно без enqueue. |
| R2-4 | `53e18976`: gate проверяет parent cancel/canceledAfterCommit, reset сбрасывает cached terminal/context/cancel. |
| R2-5 | `53e18976`: безопасные endpoint errors, unwrap runtime DoH `url.Error`, redaction generic finish. Это закрывает конкретные прежние примеры, не любые возможные provider error bodies. |
| R2-6 | `53e18976`: eviction test использует собственный cache (`lifecycle_test.go:675`), поэтому чужие parallel inserts не нарушают его warm-up oracle. |
| R3-1, R7-1 | `b0a55aab`, `4b2b2a0c`, `53e18976`: собственная assistant identity, отдельный mutex/seal на attempt, явный queued admission до classifiers (`coordinator_run.go:546`). Старую callback race не переоткрываем. |
| R3-2, R4-1 | `53e18976`: default HTTP proxy port через Hostname/JoinHostPort; CONNECT head ограничен, после успеха byte limit снят и buffered payload сохранён. |
| R5-1 | `53e18976`: reviewer R сохраняется в ctx для rebuild. Пропущенный соседний slot отдельно учтён в R9-2. |
| R5-2 | `53e18976`: OAuth получает configured client, build failure не понижает policy до default route, helpers сохраняют timeout. Согласованность snapshots остаётся в F6. |
| R5-3 | `53e18976`: terse публикуется после выбора итоговой фазы (`app_run.go:528`), primary/reviewer больше не склеиваются прежним способом. |
| R6-2 | `53e18976`: оба watchdog clamp требуют `hardCap > 0`; нулевой cap не становится absolute start+idle deadline. |
| R8-1 | `53e18976`: catalog merge сохраняет `Network` (`load_providers.go:132`); provider-only policy больше не теряется прежним literal. |
| R8-2 | `53e18976`: только `end_turn` может подавить cancellation; промежуточный `tool_use` очищается (`app_run_reviewer.go:495`). Правильность владельца выбранного end_turn остаётся R8-3. |

F6/F8/R4-2/R8-3 закрыты лишь частично, R9-1/R9-2 остаются открытыми; их остатки
посчитаны ровно один раз выше. Заголовок fix-коммита и успешность старого
happy-path test не заменяют проверку конкретного перехода.

## Незакоммиченный кандидат и round 10

В исходном checkout прочитаны следующие relevant изменения; это **другой срез**,
без собственного commit ID. Их наличие установлено чтением diff, прохождение
тестов здесь не подтверждалось.

| Finding / область | Что имеется вне committed HEAD |
| --- | --- |
| R9-1 | `replacementHandoff` выставляется при извлечении mailbox replacement; `inheritReplacementIdentityCallback` переносит callback только для принятой замены. Есть `r9_1_live_replacement_result_identity_test.go`. |
| R9-2 | `RunWithOverrides` кладёт дополненную пару в `WithModelOverrides` перед `applyModelOverrides`; есть `r9_2_overrides_rebuild_inheritance_test.go`. |
| R8-3 | Reserved entry point подключён к recorder; recorder хранит множество IDs и generation/seal; app фильтрует owned rows/events. Есть reserved-result и дополнительные app tests. |
| F6 | `rebuildInputs` возвращает cfg/provider вместе; sub-agent и summarize передают cfg в rebuild. Есть `coordinator_pinned_rebuild_snapshot_test.go`. Это само по себе не сертификат всего auth lifetime. |
| R4-2 | `resolvedDialerWithBudget` и `perAddressTimeout` выделяют время кандидатам; есть `audit_round9_test.go` с slow-candidate oracle. |
| F8 | Убран ранний return из `foldLiveToolCallCounts`; modified inventory test добавляет проверку сохранения primary counts. |
| Round 10 | Queue admission/drain передают множество assistant IDs и terminal ID; app принимает подтверждённую identity после DrainComplete. Дополнены agent/app/session tests. |

`docs/reviews/2026-09-23-weekly-commit-audit-round10.md` прочитан только в исходном
checkout: в HEAD файла нет. В нём заявлены целевые обычные и race-прогоны и
scoped clean для durable identity/queued ownership/mailbox после локальных
исправлений. Этот аудит их не повторяет и не расширяет verdict до всего Rush.

Нельзя механически перенести описанный round 10 промежуточный дефект «после drain
сохраняется canceled identity» на committed HEAD: здесь
`app_run_reviewer.go:234` намеренно очищает recorder. Именно этот baseline-only
fallback оставляет R8-3, но обычный успешный drain без постороннего владельца
не объявляется сломанным по описанию другой версии. Аналогично перенос callback
в ordinary queued call из промежуточных исправлений не является новым shipped
дефектом HEAD, где такого переноса ещё нет.

Дополнительные незакоммиченные OAuth/CAS/header-ownership и тестовые изменения
не переносятся в release verdict как исправления и не объявляются прошедшими
отдельную полную приёмку этого раунда.

## Проверки без дополнительных findings и ограничения

Проверены lock/callback границы новых recorders, admission до classification,
mailbox replacement/release, buffered done/drain channels, отмена и смена
reviewer-фаз. Mutex recorders защищают небольшое состояние; callback assistant
вызывается после unlock `turnStream.mu`. Нового доказанного циклического
deadlock/livelock по этим изменениям не установлено. Это не доказательство
отсутствия гонок или зависаний в проекте.

В network patch прослежены закрытие conn на CONNECT errors, join watcher перед
handoff, очистка provisional deadlines, закрытие DNS conn/DoH body и bounded
cache. Проверен локальный Go 1.26.3 `net/http/transport.go:1529`: dial отделён
от request cancel, поэтому собственные budgets действительно существенны.
Нового доказанного panic или неограниченного resource leak в этих ветвях нет;
bounded ожидание до сетевого deadline не названо deadlock.

Просмотрены quota parsing/guidance, peak-hours config → CLI/WS/web и новые web
render/resize изменения. Nil-вызов quota helper, не показанный production
caller, не объявляется релизной panic. Metadata использует существующий проход
по messages/parts; нового per-message timer не добавлено. MCP-коммиты недели
меняют tests, а не production process cleanup.

История и reconciliation имеют O(N) проходы; сборка `acc + next` копирует
накопленный текст. Без измеренного либо конкретно доказанного пользовательского
воздействия это не отдельные findings. Cache miss concurrency и дополнительные
DNS queries также не переименованы в утечки или speculative optimizations.

**Выполнено:** read-only log/range/base/head/diff/blame, сопоставление reports
1–9 и relevant uncommitted state/round 10, статическая трассировка production
путей, чтение целевых тестовых oracles, `git diff --check` для base..HEAD;
перед commit — проверка единственного staged path, staged diff и whitespace.
Единственное изменение audit worktree — этот Markdown.

**Не запускалось:** Go tests, включая узкие; полный suite и любой `-race`;
build, lint/vet, benchmarks/нагрузка, browser E2E, реальные LLM/OAuth/MCP/network
fault probes и remote CI. Новые тестовые файлы не создавались. Нет заявления
о race-clean состоянии, прохождении прежних tests или частоте проявления
описанных interleavings. Проверки всех ОС, SDK providers и crash recovery не
проводились. Узкие исполнения не потребовались для приведённых source-level
контрпримеров; их acceptance oracles оставлены для проверки исправляющего commit.

Для снятия NO-GO нужен релизный commit с исправлениями R9-1 и оставшихся P2
либо явное решение об ограничении соответствующих P2-сценариев, затем целевая
проверка на этом commit. Наличие исправлений в чужом рабочем дереве и scoped
clean round 10 этот gate не закрывают. Отчёт не вносит code changes, не меняет
версии и не разрешает выпуск.
