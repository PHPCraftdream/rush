# Ревью коммитов, 7-дневное окно — chunk 4 из 5 (`7b3c935e..3f3850d7`)

Обзор 43 коммитов от 2026-09-04 16:09 до 2026-09-06 07:40 CEST. Это
четвёртый из пяти непересекающихся срезов окна `da53fe42..f2914d53`;
остальные четыре чанка ревьюятся отдельно и здесь не разбираются.

Проверка: корректность, полнота, пропущенные edge cases, невакуумность
новых/изменённых тестов, соответствие commit message и комментариев
фактическому диффу, concurrency, утечки goroutine/ресурсов, error
handling. Режим только чтения: `go build`/`go test`/`go vet`/hooks не
запускались, ни один существующий файл репозитория не изменён.

Все ссылки вида `file:line` относятся к состоянию **на конце диапазона
(`3f3850d7`)**, если явно не указано иное. Там, где утверждение касается
текущего tip рабочего дерева (`f2914d53`), это оговорено отдельно.

### Глубина проверки по коммитам

Диапазон содержит ~12 100 добавленных строк, поэтому глубина не
одинакова, и это стоит зафиксировать явно:

- **Полный дифф прочитан построчно:** `0e4b9ed3` (частично, см. ниже),
  `92f2ee9a`, `49e008fa`, `2c23fcd3`, `71bd43db`, `bc786a4d`,
  `b49fd1d5`, `77e7a1c4`, `9c3ee9ab`, `ee32c4a8`, `2e152758`,
  `33ea1841`, `b9656981`, `229393f2`, `5c8641e7`, `99371d14`,
  `b7d8614f`, `cf12608e`, `71df8f19`, `14b8677d`, `1bf8bcdc`,
  `3199bf34`, `950c1a04`, `07a69d8f`, `08f5071b`, `7f2d048c`,
  `f2399bd8`, `09cc83a5`, `3f3850d7`.
- **Проверено через накопленное конечное состояние файлов** (прочитаны
  целиком `internal/agent/tools/mcp/init.go`,
  `internal/config/store_mcp.go`,
  `internal/config/store_mcp_transaction.go`, ключевые фрагменты
  `store_reload.go`/`load_files.go`/`load.go`) **плюс выборочный
  просмотр диффов на уровне сигнатур и хунков:** `96aee6fd`,
  `7dc7a4e7`, `f11d8fed`, `95faff44`, `9d1a6f2f`, `779e323b`,
  `366be5dc`, `2e552cce`, `9ebf1c7d`, `b6f2ea88`, `598ef490`,
  `e3b79c04`, `998c5213`. Вердикт «корректно» для этой группы означает
  «итоговое состояние кода, которое эти коммиты производят, проверено и
  корректно», а не «каждая строка каждого промежуточного диффа
  прочитана».

Первый коммит диапазона, `0e4b9ed3`, входит в диапазон предыдущего ревью
(`docs/reviews/2026-09-05-2047-commit-review-36h.md`, `239c8943..0e4b9ed3`).
Его разбор здесь **цитируется** оттуда (§8 того документа), а не выводится
заново; выборочная проверка `beginPoolReset`/`lifecycleResetters` в
`internal/db/connect.go` подтвердила описание. Все остальные выводы
получены самостоятельно.

---

## Сводка коммитов

Хронологически (снизу вверх в `git log`):

| Hash | Тема | Вердикт |
|---|---|---|
| `0e4b9ed3` | writer-preference для `ResetPool` | корректно (цитируется из 36h-ревью) |
| `92f2ee9a` | fence MCP lifecycle state and refreshes (+882 в `init.go`) | корректно как фундамент, промежуточные дефекты закрыты далее в диапазоне |
| `49e008fa` | тесты: admission отменяет HTTP-запросы | корректно, nit F6 |
| `2c23fcd3` | harden MCP lease and refresh lifecycle | корректно; `configGeneration` оказался слишком строгим и снят в `bc786a4d` |
| `71bd43db` | preserve latest MCP lifecycle state | корректно; `sameMCPConfig` снят в `bc786a4d` |
| `b49fd1d5` | Codex: установка всех трёх delegation Skills | корректно |
| `77e7a1c4` | Codex `wrush`: путь к sibling-скиллу | корректно, nit F7 |
| `9c3ee9ab` | parser-safe frontmatter в SKILL.md | корректно |
| `bc786a4d` | allow MCP callbacks after config updates | корректно (откат чрезмерного fencing) |
| `4a2360d0` | docs: 36h commit-range review | — |
| `ee32c4a8` | закрытие CR-10/CR-11 (hook cleanup + goose) | корректно, побочный эффект F5 |
| `2e152758` | docs: correct contract review documentation | корректно, nit F13 |
| `33ea1841` | docs: обновление статусов follow-up | корректно (в т.ч. чинит дубль абзаца из `2e152758`) |
| `b9656981` | pump.Stop до broker shutdown + OnDone-учёт буферов | **закрывает CR-8/CR-12; вносит F1 (флейк теста)** |
| `229393f2` | attached/detached retention разведены | корректно |
| `5c8641e7` | harden MCP session lifecycle ownership | **закрывает CR-1**; вносит F4 (жил 12 минут) |
| `99371d14` | disarm MCP initialization timeouts | **закрывает F4** |
| `b7d8614f` | linearize MCP session promotion | корректно, завершает фикс CR-1 |
| `cf12608e` | transactional + literal-key safe MCP updates | корректно |
| `71df8f19` | preserve admissions during transactional replace | корректно |
| `14b8677d` | finalize inactive replacements | корректно |
| `1bf8bcdc` | preserve runtime lifecycle on refresh failures | корректно |
| `3199bf34` | preserve refresh status and reopen init barriers | корректно |
| `96aee6fd` | keep MCP origins and reloads consistent | корректно |
| `7dc7a4e7` | bound reload consistency retries | корректно |
| `f11d8fed` | scope MCP lifecycle mutations by origin | корректно |
| `95faff44` | restrict MCP global fallback to pending adds | корректно |
| `9d1a6f2f` | close MCP add transactions after durable outcome | корректно |
| `779e323b` | persist pending MCP disables atomically | корректно |
| `366be5dc` | make config MCP mutations transactional | корректно, вносит часть F9 |
| `2e552cce` | revalidate config before initial publish | корректно |
| `9ebf1c7d` | wire MCP mutation results into runtime lifecycle | корректно |
| `b6f2ea88` | transactionally consistent MCP/reload reads | корректно |
| `950c1a04` | pending removal + session handoff cleanup | корректно, закрывает goroutine-leak в `sessionContext` |
| `07a69d8f` | isolate library MCP surfaces by config | корректно |
| `08f5071b` | fence revealed MCP fallbacks | корректно |
| `598ef490` | harden MCP scope and config discovery | корректно |
| `f2399bd8` | test: align exact-scope disabled oracle | корректно |
| `7f2d048c` | isolate library runs from application MCP init | корректно |
| `09cc83a5` | enforce MCP config boundaries | корректно |
| `e3b79c04` | cancellable MCP session operation leases | **закрывает CR-4**; вносит F2 |
| `998c5213` | bind config transactions to trusted file identity | корректно |
| `3f3850d7` | enforce workspace owner policy during load | корректно, поведенческое изменение F8 |

42 из 43 коммитов имеют пустое тело commit message; единственное
исключение — docs-коммит `4a2360d0`. Для Codex-коммитов обоснование
компенсируется правками CHANGELOG/README в том же коммите; для MCP- и
config-серий оно живёт исключительно в комментариях кода. В этом
диапазоне комментарии действительно подробные и, за перечисленными
ниже исключениями (F11, F14, F17), соответствуют коду — это заметно
лучше, чем в предыдущем диапазоне, но CR-9 36h-ревью («обоснование
только вне истории») по сути остаётся в силе.

---

## 0. Главный вопрос: статус CR-1

**CR-1 закрыт внутри этого диапазона. Коммиты-фиксы: `5c8641e7`
(механизм) и `b7d8614f` (линеаризация). Дополнительно `99371d14`
устраняет регрессию, внесённую самим `5c8641e7`.**

### Что было

Формулировка CR-1 (36h-ревью, §7): контекст MCP-сессии, созданной в
`Owner.Initialize`/`initClientAdmitted`, отменялся сразу после
инициализации — `defer cancel()` в `Initialize`, `defer admission.done()`
и `defer finish()` в `initClientAdmitted`. Транспорт (stdio-процесс через
`exec.CommandContext`, HTTP через `headerRoundTripper.ctx`) был потомком
именно этого контекста, поэтому первая же сессия каждого сервера умирала
до первого tool call.

### Что стало (трассировка по коду на `3f3850d7`)

`5c8641e7` вводит промежуточный контекст-handoff `sessionContext`
(`internal/agent/tools/mcp/init.go:2875-2957`) и точку promotion.
Цепочка на конце диапазона:

1. `Owner.Initialize` — `initCtx, cancel := context.WithCancel(ctx)`
   (`init.go:1319`), `defer cancel()` (`init.go:1334`) — **сохранены**.
2. `admission, err := o.admitServer(initCtx, cfg, name, true)`
   (`init.go:1360`) → `operationCtx, cancel = context.WithCancel(ctx)`
   (`init.go:622`), `admission.ctx` — потомок `initCtx`.
3. Горутина инициализации вызывает
   `initClientAdmitted(admission.ctx, …)` (`init.go:1386`) →
   `initClientAdmittedWithState`, где `defer admission.done()`
   (`init.go:1456`) и `defer finish()` (`init.go:1474`) — **тоже
   сохранены**.
4. **Ключевое изменение:** `createSessionWithAdmission` игнорирует
   переданный `ctx` при наличии admission и строит собственный
   lifetime-контекст:

   ```go
   if admission != nil && admission.owner != nil {
       handoff = newSessionContext(admission.owner.lifecycleCtx, admission.ctx)
       sessionCtx = handoff
   }
   lifetimeCtx, cancelSession := context.WithCancelCause(sessionCtx)
   ```
   (`init.go:2961-2967`). Транспорт создаётся от `lifetimeCtx`
   (`init.go:2987`), stdio получает `platform.Command(lifetimeCtx, …)`
   (`init.go:3165`), HTTP/SSE — `headerRoundTripper{ctx: lifetimeCtx}`
   (`init.go:3193-3197`, `:3220-3224`).
5. `sessionContext` — не `context.WithCancel`, а собственный тип с
   воркер-горутиной (`init.go:2894-2910`). Отмена **candidate**-контекста
   (`admission.ctx`) закрывает handoff только если сессия ещё не
   promoted:

   ```go
   func (s *sessionContext) finish(source context.Context, candidate bool) bool {
       ...
       if s.closed || (candidate && s.promoted) { return false }
   ```
   (`init.go:2914-2924`). Отмена **owner**-контекста (`o.lifecycleCtx`)
   закрывает handoff всегда.
6. `b7d8614f` переносит promotion **перед** публикацией и делает её
   отказуемой: `publishPreparedClient` вызывает `session.promoteContext()`
   (`init.go:1563-1567`) под `lifecycleMu` и под write-lease сервера,
   до `sessions.Set(name, session)` (`init.go:1571`); при отказе сессия
   закрывается и публикация не происходит. Тот же порядок в
   `commitRenewal` (`init.go:1055-1059`).

Итог: на нормальном пути `Initialize` promotion выполняется до возврата
из `initClientAdmittedWithState`; последующие `finish()` (отменяет
`operationCtx`, который вообще не входит в цепочку транспорта) и
`admission.done()` → `a.cancel()` (отменяет `admission.ctx`) наталкиваются
на `promoted == true` и **не закрывают** `handoff.done`. `defer cancel()`
в `Initialize` отменяет `initCtx` — тот же эффект. Транспорт начальной
сессии живёт до `Owner.lifecycleCancel()` или явного
`session.Close()`/`retire()`.

Отдельно `950c1a04` закрывает воркер-горутину handoff: добавлены
`case <-s.done` в оба select (`init.go:2896-2909`), `abort()`
(`init.go:2936-2945`) и вызов `terminal()` из `ClientSession.Close`
(`init.go:70-72`).

### Регрессия, внесённая и закрытая внутри диапазона (F4)

`5c8641e7` при переписывании `createSessionWithAdmission` **удалил**
`cancelTimer.Stop()` с success-пути и не заменил его ничем: таймер
`time.AfterFunc(timeout, …)`, вооружённый для connect-фазы, оставался
взведённым после успешного `client.Connect`. Через `mcpTimeout(m)`
(по умолчанию 15 с) он вызывал `cancel(timeoutCause)` на `mcpCtx` —
то есть убивал транспорт уже опубликованной сессии. Это фактически
воспроизводило CR-1 с задержкой 15 секунд.

`99371d14` (через 12 минут) закрывает это: `stopInitTimer()` вызывается
сразу после `Connect` (`init.go:3025`), реализован через `sync.Once`
с `<-timerDone` на случай уже сработавшего таймера
(`init.go:2974-2981`), плюс добавлена проверка «Connect вернул успех,
но контекст уже отменён» (`init.go:3027-3030`). На конце диапазона
проблемы нет.

### Тестовое покрытие фикса

`TestInitializePublishesHTTPSessionBeyondAdmission`
(`internal/agent/tools/mcp/state_regression_test.go:1286-1327`) —
невакуумный оракул именно для CR-1:

- поднимает настоящий `mcp.NewStreamableHTTPHandler`, считает вызовы
  метода `initialize` через receiving middleware;
- ставит `Timeout: 1` (секунда) и **спит 1200 мс** после `Initialize` —
  то есть ловит и исходный CR-1, и регрессию F4;
- после возврата `Initialize` (то есть после `admission.done()`) делает
  `Ping` и `CallTool` на опубликованной сессии и требует
  `initializeCalls == 1`.

На pre-fix коде `Ping` получил бы отменённый транспорт, а respawn поднял
бы счётчик до 2. Дополнительно `TestSessionContextPromotionLinearizesCancellation`,
`TestCommitRenewalPublishesOnlyAfterPromotion`,
`TestPublishedSessionIgnoresCandidateCancellationUntilClose`,
`TestClientSessionCloseAbortsPromotedCandidate`,
`TestRepeatedPromotedCandidateCloseStopsHandoff`
(`internal/agent/tools/mcp/init_test.go:800-974`) фиксируют сам handoff.

**Остаточный gap (P3):** живого stdio-сервера в регрессии по-прежнему
нет — замечание 36h-ревью «stdio-конфиги в тестах используют
`Command: "echo"`» остаётся в силе. Механизм общий (`lifetimeCtx` один
на все транспорты), поэтому это вопрос полноты, а не корректности.

---

## 1. MCP session lifecycle (`92f2ee9a`, `49e008fa`, `2c23fcd3`, `71bd43db`, `bc786a4d`, `5c8641e7`, `99371d14`, `b7d8614f`, `950c1a04`)

**Статус: корректно на конце диапазона; несколько промежуточных состояний
были некорректны и закрыты внутри диапазона.**

`92f2ee9a` — фундамент серии: `leaseRegistry` с refcount вместо
`csync.Map`, `serverAdmission` (owner generation + server epoch + config),
очередь refresh с собственной горутиной и `refreshWG`, который
`finishClose` джойнит (`init.go:1123`), `updateAdmissionState` вместо
безусловного `updateState`, `headerRoundTripper` с собственным пулом
и явной отменой запроса. Замечания по промежуточным состояниям:

- `leaseRegistry.retain` в `92f2ee9a` был `void` и инкрементировал
  `refs` даже для уже освобождённой записи — ABA, ради которого
  refcount и вводился, оставался достижим. Закрыто в `2c23fcd3`:
  `retain` возвращает `bool` и проверяет `r.entries[lease.name] == lease
  && lease.refs != 0` (`init.go:216-225`), а все call-site'ы обрабатывают
  отказ.
- `enqueueRefresh` в `92f2ee9a` имел `default:` -ветку, молча теряющую
  уведомление при переполнении буфера на 128. Закрыто в `2c23fcd3`:
  канал стал `chan struct{}` ёмкостью 1 (wake-up edge), а
  авторитетной очередью стала `refreshPending`
  (`init.go:912-940`).
- `2c23fcd3` добавил `serverLease.downgrade()`, ни разу не вызванный;
  удалён в `71bd43db`. Мёртвый код прожил один коммит.
- `serverAdmission.configGeneration` (`2c23fcd3`) инвалидировал admission
  при **любой** публикации конфига, включая мутацию чужого сервера;
  `71bd43db` сузил до `sameMCPConfig` по полям, `bc786a4d` снял совсем.
  На конце диапазона identity-пиннинг остался только там, где он нужен
  по смыслу — `admitServerForConfig`/`hasConfigIdentity`
  (`init.go:600-602`, `:590-592`), используемый в `startFallback`.
  Итоговое решение верное; тест
  `TestRenewedSessionNotificationSurvivesConfigMutations`
  (`state_regression_test.go:869`) фиксирует именно это.
- `setState` в `92f2ee9a` при `StateError` эвристически удалял сессию,
  опираясь на `previous.Client == nil`. `1bf8bcdc` убрал эвристику
  (`init.go:2816-2828`) и перенёс удаление в явную точку — там, где
  ping-провал идентифицирует конкретную сессию
  (`init.go:2645-2653`). Это правильное направление: состояние
  перестало выводить владение сессией из предыдущего состояния.

`49e008fa` — тестовая правка: `Content-Type: application/json` заставляет
SDK действительно блокироваться на чтении ответа, буферизованные каналы
вместо `close()` убирают потенциальный double-close. Оракулы стали
строже. См. F6 про снятый escape hatch.

`3199bf34` — `beginInitialize` переоткрывает init-барьер при
`fullInitCount == 0` вместо `!initStarted` (`init.go:1001-1005`), то есть
второй полный `Initialize` снова блокирует `WaitForInit`. Тест
`TestSequentialInitializeReopensInitBarrier`
(`state_regression_test.go:1425`) невакуумен (использует
`httptest`-сервер, удерживающий первый запрос). Побочный эффект,
не описанный в комментарии: `WaitForInit`, уже вернувший `nil`, может
позже снова заблокироваться. Единственный production-вызов —
`internal/app/app_run.go:627`, один раз за run, поэтому безопасно.

---

## 2. MCP config mutations как транзакции (`cf12608e`, `71df8f19`, `14b8677d`, `f11d8fed`, `95faff44`, `9d1a6f2f`, `779e323b`, `9ebf1c7d`, `08f5071b`, `950c1a04`)

**Статус: корректно.**

Главное содержательное изменение серии — уход от sjson-путей вида
`"mcp."+name+".disabled"` к операциям над литеральными JSON-ключами
(`cf12608e`). Проблема была реальной: имя MCP-сервера — произвольная
строка, а gjson/sjson трактуют `.`, `\`, `*`, `?`, `#` как синтаксис
пути. Тест
`TestConfigStoreMCPLiteralNamesRoundTripAndExactRemoval`
(`internal/config/store_mcp_test.go`) прогоняет `foo.bar`, `foo\bar`,
`foo#*?`, `foo.disabled`, `foo.timeout` и проверяет, что операция над
одним именем не задевает соседей — невакуумно относительно старого
sjson-пути.

Промежуточный компромисс `mcpLiteralField` (эвристика «последний
компонент из белого списка — это поле, остальное — имя»), введённый
в `cf12608e` вместе с перехватами внутри `SetConfigFields`/
`RemoveConfigField`, был **удалён целиком** в `71df8f19` в пользу явных
`PersistMCP*` API. Это правильно: эвристика не могла различить сервер
с именем `foo.disabled` и поле `disabled` сервера `foo`, и её
единственный корректный режим — не существовать. Комментарий
`internal/config/store_write.go:187-192` теперь честно фиксирует
ограничение legacy-API.

`ReplaceServer` (введён в `cf12608e`, доведён в `71df8f19`/`14b8677d`)
реализован как настоящая транзакция: кандидат готовится полностью
(`prepareClient`), promotion проходит до дисковой записи
(`init.go:1931-1937`), durable RMW выполняется **без** `lifecycleMu`
(`init.go:1941-1946` — комментарий явно объясняет почему), и только
после успешной записи выполняется epoch bump обоих имён и подмена
runtime-состояния. Отдельно правильны:

- `admitReplacementCandidate` (`init.go:657-686`) вместо
  `admitServer(bump=true)`: неудачная замена больше не инвалидирует
  admission живого старого сервера. Это прямое исправление дефекта,
  внесённого в `cf12608e`.
- `serverCancels` переключён с ключа-эпохи на ключ-токен
  (`init.go:630`, `:531-540`) — два кандидата на одну эпоху больше не
  затирают друг друга в мапе.
- `14b8677d` закрывает путь «durable replace успешен, но итоговый
  конфиг не runnable»: фенсинг обоих имён + удаление runtime-артефактов
  + события, описывающие фактическое состояние (`init.go:1996-2003`,
  `:2017-2042`). Возврат `nil` при этом обоснован — durable RMW
  линеаризовался.

`f11d8fed`/`95faff44`/`9d1a6f2f`/`779e323b`/`950c1a04` — механика
`addTransaction`/`pendingGlobalAdds`. Инвариант выдержан: глобальный
scope-fallback в `resolveMCPMutationScope` (`init.go:2061-2067`)
разрешён **только** при наличии pending add для того же `cfg`
(`hasPendingGlobalAddFor`, `init.go:2075-2084`), а
`markPendingGlobalAdd` вызывается ровно из одного места
(`addServerWithInitializer`, `init.go:2128`) — комментарий
`init.go:688-692` это утверждает, и grep подтверждает. Порядок
`defer o.completePendingGlobalAdd(transaction)` (`:2135`) + явный вызов
после durable-успеха (`:2186`) корректен: `sync.Once` внутри
`addTransaction` делает повтор no-op, а `rollbackAddedServer` успевает
увидеть транзакцию живой, потому что deferred-вызов выполняется позже.

`950c1a04` заменил трёхшаговую последовательность «материализовать
определение → удалить» на одну условную запись
`mutatePendingRemoveMCP` (`internal/config/store_mcp_transaction.go:101-147`).
Это устраняет окно, в котором pending-определение реально попадало на
диск. Тест `TestPendingAddRemoveUsesOneConditionalWriteWithoutEnabledWindow`
(`state_regression_test.go:504`) невакуумен.

`08f5071b` — `startFallback` теперь берёт lease на старое имя и сравнивает
эффективное определение с тем, что вернула мутация
(`init.go:2392-2405`), а вызывающий `removeServerWithResultPersistence`
явно освобождает свой lease перед вызовом (`init.go:2331-2333`,
флаг `leaseLocked`). Без этого была бы самоблокировка на том же
`serverLease`; порядок правок в одном коммите корректен.

Замечание по `internal/server/handlers_mcp.go:100-112` (`cf12608e`):
переход `RemoveServer` + `AddServer` → `ReplaceServer` — содержательное
улучшение (было окно, в котором сервер отсутствовал вовсе, и
несогласованный откат при неудачном add). Изменение поведения WS API:
теперь возможна ошибка `ErrMCPTargetExists`/`ErrMCPExternal` там, где
раньше была двухфазная последовательность. UI об этом не уведомлён
(`c.reply(msg.ID, EventError, …)` передаёт текст ошибки как есть) —
приемлемо.

---

## 3. Config store: транзакционные границы, origin, доверенная идентичность (`96aee6fd`, `7dc7a4e7`, `366be5dc`, `2e552cce`, `b6f2ea88`, `598ef490`, `f2399bd8`, `09cc83a5`, `998c5213`, `3f3850d7`)

**Статус: корректно; одно поведенческое изменение без документации (F8).**

Ядро — `withMCPWriteLocks`
(`internal/config/store_mcp_transaction.go:35-91`): один порядок
блокировок `publishMu → diskWriteMu → отсортированные sidecar-локи`,
причём **оба** записываемых файла (global data + workspace) блокируются,
даже когда мутируется один. Комментарий `:31-34` это объясняет, и код
соответствует: проверки origin/existence/target выполняются в одной
кросс-процессной точке линеаризации.

Read-only входы (system config, project-цепочка, `.mcp.json`) не
блокируются, но фиксируются fingerprint'ами и перепроверяются
`verifyMCPReadOnlyInputs` (`:589-608`) непосредственно перед записью.
Это корректная схема optimistic-read + validate-before-commit.
`ErrMCPStale` возвращается при любом расхождении.

`998c5213`/`3f3850d7` привязывают доверие к **открытому дескриптору**, а
не к пути: `readStableConfigFileOwned` (`internal/config/store_reload.go:65+`)
делает open → stat дескриптора → owner-проверку → чтение, и фиксирует
`configFileIdentity` + discovery-chain. Комментарий `:61-64`
(«A path is only a name: stat-then-ReadFile would allow a symlink or
parent directory to be retargeted between those operations») точно
описывает закрываемую TOCTOU. Тест
`TestOwnedConfigReadChecksOwnerOnOpenedFile`
(`internal/config/store_mcp_identity_unix_test.go`) невакуумен —
`eligibleConfigCandidate` до `chown` возвращает путь, после `chown`
чтение падает с `errConfigOwnerMismatch`.

Windows-путь безопасен: `homeConfigOwner()`/`systemConfigOwner()`
возвращают `-1` (`internal/config/config_owner_windows.go`),
`fsext.Owner` возвращает `-1` при успешном `Stat`
(`internal/fsext/owner_windows.go`), `configFileOwner` возвращает
`(-1, true)` (`internal/config/config_file_identity_windows.go:392`), а
`configOwnersMatch(-1, x)` истинно всегда. То есть политика
владения — осознанный no-op на Windows, и это задокументировано
в `config_owner_windows.go:5-6`. Проверено специально, потому что
`3f3850d7` превращает несовпадение владельца в жёсткую ошибку `Load`
(см. F8).

`7dc7a4e7` закрывает две вещи сразу. Во-первых, `discoverMCPJSONFiles`
раньше сам делал `os.Stat` и возвращал только существующие файлы, из-за
чего появление `.mcp.json` между построением кандидата и проверкой
согласованности не считалось изменением; теперь
`mcpJSONCandidatePaths` (`internal/config/mcp_json.go`) возвращает **все**
возможные расположения, включая отсутствующие, и они входят в
staleness-набор (`configAndMCPStalenessPaths`). Во-вторых, цикл
пересборки кандидата ограничен `reloadMaxAttempts = 4` с явной ошибкой
`ErrConfigReloadUnstable` (`internal/config/store_reload.go`) — до этого
непрерывно переписываемый конфиг мог крутить reload бесконечно.
Обработка на вызывающей стороне (`autoReload` логирует warning)
адекватна.

`96aee6fd`/`b6f2ea88` доводят до конца согласованность
внешнего `.mcp.json`. `cf12608e` уже добавил merge внешних серверов в
`buildAndPublishReload` (`store_reload.go:113-118` на момент того
коммита) — до этого reload после появления `.mcp.json` молча терял эти
серверы. `b6f2ea88` переводит чтение `.mcp.json` на тот же
stable-document/fingerprint конвейер, что и Rush-конфиги
(`loadExternalMCPDocumentsStable`, `mergeExternalMCPServersFromDocuments`,
`candidateInputsChanged` в `internal/config/mcp_json.go` и
`store_reload.go`), так что внешние файлы участвуют в проверке
согласованности reload наравне с остальными кандидатами, а прежние
path-основанные `externalMCPDisabledOverride`/`hasNonOverlayMCPDefinition`
удалены. `externalOverlayValue`
(`store_mcp_transaction.go:567-582`) корректно применяет
disabled-overlay из Rush-документов к внешнему определению, не позволяя
одно-полевой overlay притвориться полноценным определением
(`sanitizeMCPDisabledOverlays`, `internal/config/load_files.go:387+`).

`09cc83a5` меняет вывод `rush mcp enable/disable`: вместо
`✓ %s enabled` теперь `writeMCPDisabledMutationMessage`
(`internal/cmd/mcp.go:617-635`), который предупреждает, когда
higher-priority scope перекрывает результат. Полезно и покрыто
`internal/cmd/mcp_test.go`. Формат сообщения на stderr изменился —
скрипты, парсящие его, сломаются; для stderr-информационного вывода
приемлемо, но в CHANGELOG не отражено.

`f2399bd8` — правка оракула, а не кода: старый тест утверждал, что
exact-запись `disabled: true` в глобальный scope не меняет эффективное
значение при наличии workspace-определения. Это было неверно: фикстура
workspace не содержит поля `disabled`, поэтому merge наследует явное
глобальное значение. Новый оракул дополнительно проверяет, что
workspace-файл побайтово не изменился. Коммит честно исправляет
неправильный оракул, а не подгоняет тест под баг.

---

## 4. Operation leases: закрытие CR-4 (`e3b79c04`)

**Статус: механизм корректен; вносит F2 (дисбаланс refcount lease).**

CR-4 (36h-ревью): `serverLease.RLock` удерживался на весь tool call,
поэтому один провалившийся ping блокировал все новые вызовы сервера до
конца in-flight операций (writer preference `sync.RWMutex`).

`e3b79c04` разделяет две сущности:

- **server lease** — теперь только короткие участки координации;
  комментарий `init.go:311-312` прямо говорит «Network calls must never
  run while this lock is held», и это выдержано: `pingWithTimeout`
  (`init.go:2530`) выполняется после `lease.Unlock()` на `:2518`.
- **operation lease** — `ClientSession.acquireOperation`/
  `releaseOperation`/`retire` (`init.go:106-158`), пиннинг поколения
  сессии через refcount. `retire()` отменяет только
  `retireCtx`; транспорт закрывается последним освобождением
  (`releaseOperation` → `s.Close()` при `retired && refs == 0`).

Схема согласована: `retireMCPClient` (`init.go:2364-2369`) закрывает
транспорт немедленно только если refs уже 0. `finishClose` безопасен,
потому что `initWG.Wait()` (`init.go:1120`) отрабатывает раньше, а
каждая операция держит init-ссылку (`o.beginInit()` в
`getOrRenewClient`, `o.endInit()` в `newClientLease.release`).

Учёт ссылок в `getOrRenewClient` проверен по всем ветвям и **сбалансирован**
(шаблон `leaseRefTransferred` + `defer lease.registry.release(lease)`,
`init.go:2521-2527`). В `addServerWithInitializer` — нет, см. F2.

---

## 5. Library/application MCP isolation (`07a69d8f`, `7f2d048c`)

**Статус: корректно.**

MCP-реестр процессный, а `ConfigStore` — нет. `07a69d8f` вводит
`mcp.IsConfigured(cfg, name)` (`init.go:1268-1274`) и применяет его во
всех местах, где registry-данные выходят наружу: инструкции турна
(`internal/agent/agent_turn.go:309-311`), список инструментов
(`internal/agent/tools/mcp-tools.go:47-49`), проксирование во внешние
CLI (`internal/agent/coordinator_providers.go:692-694`), `rush_info`
(`internal/agent/tools/rush_info.go:160-162`, `:206-209`).
`SessionAgentOptions.Config` прокинут в оба существующих call-site
`NewSessionAgent` (`coordinator_tools.go:101`,
`agentic_fetch_tool.go:204`) — grep подтверждает, что других нет.
`IsConfigured(nil, …) == true` сохраняет поведение тестовых фикстур,
что задокументировано.

`7f2d048c` — `ExecuteRun` больше не ждёт процессный init-барьер, если у
App нет MCP-владельца (`internal/app/app_run.go:627-633`). Это правильно:
library-mode App проходит `app.SkipMCP()`, не берёт владельца и не имеет
отношения к барьеру, который может держать одновременно живущий
application-mode App.

Побочный, но верный эффект `07a69d8f`: `writeMCP` больше не печатает
пустую секцию `[mcp]`, когда после фильтрации не осталось записей
(`rush_info.go:178`, `:198`).

---

## 6. Shutdown ordering и background-буферы (`b9656981`, `229393f2`)

**Статус: закрывает CR-8 и CR-12; вносит F1.**

`b9656981` переставляет `app.RunQueuePump.Stop()` **перед** shutdown
брокеров (`internal/app/app_lifecycle.go:166` до `:179-194`). Это ровно
то, что просил CR-8: pump-воркер публикует
терминальное событие, разматываясь после отмены, и подписчик
(`rush run` на stdout) успевает его получить до EOF. Оракул
`TestAppShutdown_PumpStopPrecedesBrokerEOF`
(`internal/app/app_lifecycle_test.go`) невакуумен и покрывает обе ветви
(graceful/forced) — координатор эмитит событие только после
`<-ctx.Done()`, поэтому при старом порядке оно бы терялось. Граница
по времени сохранена: forced-путь не ждёт воркеров дольше grace-периода
`Pump.Stop`.

`b9656981` + `229393f2` закрывают CR-12: `OnDone`-callback теперь
учитывается (`onDoneCount`), а для **detached** job появляется
ограниченное окно `detachedCallbackReleaseGrace = 1s`
(`internal/shell/background.go:65-68`, `:786-808`), в течение которого
буферы не освобождаются. `notifyBackgroundJobDone`
(`internal/agent/coordinator_background.go:53`) первым же действием
делает `sh.GetOutput()`, поэтому реальный вывод до него доходит.
`229393f2` дополнительно правит перекос первой версии: attached-job
теперь освобождает буферы строго по своему обычному retention, а
callback этот контракт не удлиняет и не укорачивает
(`background.go:783-787`). Перевод `bufferRetention` из package-var в
поле менеджера снял ограничение «эти тесты нельзя запускать
параллельно» — тесты действительно получили `t.Parallel()`.

Разбор F1 — ниже.

---

## 7. Codex delegation skills (`b49fd1d5`, `77e7a1c4`, `9c3ee9ab`)

**Статус: корректно.**

`b49fd1d5` добавляет третий Skill (`wrush`) в `codex-init`/`codex-del`/
`cli-refresh`. Sentinel-семантика соблюдена: запись через
`writeSentinelledSkillDir(..., claudeSlashCommandSentinel, ...)`, удаление
через `removeSentinelledSkillDir(..., "wrush", claudeSlashCommandSentinel)`.
Перекрёстного срабатывания между `rush` и `wrush` нет: sentinel'ы —
`<!-- rush-slash-command:v1 -->` и `<!-- wrush-slash-command:v1 -->`,
и `strings.Contains` первого во втором ложно (мешает префикс `<!-- `).
Тесты покрывают все четыре ветки (создание, overwrite с sentinel,
отказ без sentinel, удаление/отказ удаления), включая
global-scope-инсталляцию.

`9c3ee9ab` переносит sentinel из первой строки файла за закрывающий
разделитель frontmatter (`internal/cmd/multi_cli_convert.go:88-96`).
Это необходимо для строгих парсеров Skill'ов, и обратная совместимость
не ломается: `writeSentinelledFile`/`removeSentinelledSkillDir`
используют `strings.Contains`, а не префикс. Новый хелпер
`assertCodexSkillFrontmatter` дополнительно парсит результат настоящим
`skills.ParseContent` — оракул стал сильнее.

`77e7a1c4` — nit F7.

---

## 8. Хуки, goose, DB pool (`ee32c4a8`, `0e4b9ed3`)

`ee32c4a8` закрывает CR-10 и CR-11 36h-ревью:

- `.githooks/check_run_test_segment_retry_flags.sh` — cleanup через
  функцию с проверками `[ -n "$var" ]`, `mktemp` проверяются на успех
  до присваивания. Trap больше не может упасть на unbound variable
  при `set -u`.
- Мёртвый `init()` c `goose.SetBaseFS`/`SetLogger` удалён из
  `internal/db/connect.go`; `testing` остаётся в импортах, так как
  `testing.Testing()` используется на `connect.go:558` — компиляция
  не ломается (проверено).
- Комментарии в тестах и явное указание, что
  `TestOpenLibraryMode_ConcurrentEphemeralAndFileBackedOpen` —
  race-only регрессия. Это ровно то замечание, которое было в
  36h-ревью.

Побочный эффект — F5.

`0e4b9ed3` разобран в 36h-ревью §8 (writer-preference `beginPoolReset` +
`lifecycleResetters`, детерминированный тест через seams). Выборочно
перечитан `internal/db/connect.go` — описание соответствует коду;
самостоятельного разбора здесь не приводится.

---

## 9. Документация (`4a2360d0`, `2e152758`, `33ea1841`)

`4a2360d0` фиксирует 36h-ревью. `2e152758` — correction-серия:
переписаны комментарии `internal/agent/tools/fs_provider_os.go:20-57`
(ложная модель угрозы «comparing uncomparable type» заменена честной
формулировкой «marker как явный identity-предикат»), исправлены
`sdk/sdk.go` и `sdk/README.md` (см. ниже), добавлены correction notes
в R15/R16.

Проверено самостоятельно: утверждение «A second simultaneous
application-mode Open fails with an error wrapping mcp.ErrOwnerBusy»
верно — цепочка `%w` целая: `mcp.Acquire()` →
`fmt.Errorf("failed to acquire MCP application owner: %w", err)`
(`internal/app/app.go:274`) → `fmt.Errorf("sdk: failed to create app
instance: %w", err)` (`sdk/sdk.go:483`). Утверждение «Library mode …
does not acquire the owner» тоже верно (`internal/app/app.go:266`).

`33ea1841` обновляет статусы CR-10/CR-11 и попутно удаляет дубль абзаца
про CR-7, внесённый `2e152758`. Nit F13.

Замечание к самой 36h-документации: на конце этого диапазона
(`3f3850d7`) её итоговый раздел всё ещё утверждает «Блокирует: CR-1»,
хотя CR-1 закрыт в `5c8641e7`/`b7d8614f` — эти коммиты произошли уже
после `33ea1841`. Обновление статуса пришло позже, вне диапазона.
Это ожидаемая последовательность, а не дефект.

---

## Утечки памяти и горутин

**Новых unbounded утечек: 0.** Найдены одна ограниченная утечка
refcount (F2, закрыта сразу за диапазоном) и одна goroutine-утечка,
внесённая и закрытая внутри диапазона.

- `sessionContext` (`5c8641e7`): воркер-горутина, вооружённая на
  `owner.Done()`/`candidate.Done()`, после promotion блокировалась на
  `<-owner.Done()` без выхода при закрытии сессии. Для долгоживущего
  owner'а это одна горутина на каждую созданную сессию, включая
  замененные/отставленные. Закрыто в `950c1a04`: добавлены
  `case <-s.done` в оба select и `abort()` из `ClientSession.Close`
  (`init.go:2894-2910`, `:2936-2945`, `:70-72`). На конце диапазона
  горутина завершается при закрытии сессии, при отмене owner'а и при
  отмене неpromoted-кандидата. Тестовый seam `workerDone`
  (`init.go:2879`) используется в `init_test.go:865,892,907`.
- `Owner.refreshLoop` — одна горутина на owner, `refreshWG` джойнится в
  `finishClose` до сброса реестра (`init.go:1123`).
- `Owner.finishClose` — одна горутина на owner (без изменений
  относительно 36h-ревью).
- `context.AfterFunc` в `operationContext` (`init.go:1080`),
  `admitServer` (`:623`), `admitReplacementCandidate` (`:664`),
  `acquireOperation` (`:119`), `RoundTrip` (`:3337`) — все имеют
  соответствующий `stop()`.
- `serverLease` — refcount; регистрация чистится при `refs == 0`,
  `leases.reset()` на закрытии owner'а. Единственный дисбаланс — F2.
- `time.AfterFunc` в `createSessionWithAdmission` — разоружается
  `stopInitTimer` через `sync.Once`; `detachedReleaseTimer` в
  `background.go` хранится и стопится (`229393f2`).
- `pendingGlobalAdds` — ключи-имена серверов, запись удаляется в
  `completePendingGlobalAdd`, который вызывается через `defer` на всех
  путях `addServerWithInitializer`.

---

## Открытые findings

| ID | Severity | Коммит | Описание | Статус |
|---|---|---|---|---|
| CR-1 | **P0/P1** (историч.) | `01e909d0` (вне диапазона) | Контекст транспорта начальной MCP-сессии отменялся на выходе из `Initialize`/`initClientAdmitted` | **Закрыт в диапазоне: `5c8641e7` (`sessionContext` handoff) + `b7d8614f` (promotion до публикации, отказуемая promote).** Регрессия `TestInitializePublishesHTTPSessionBeyondAdmission` невакуумна |
| F1 | **P2** | `b9656981` | `close(bgShell.done)` перенесён **перед** `armBufferReleaseTimer` (`internal/shell/background.go:468` vs `:480`), но `TestBackgroundShellManager_Remove_RacingCompletionReleasesBuffers` (`internal/shell/background_release_test.go:254-264`) читает `bgShell.bufReleased.Load()` сразу после `bgShell.Wait()`. Между `close(done)` и освобождением буферов теперь есть окно, в котором assert видит `false` | **Открыт, воспроизводится и на tip `f2914d53`** |
| F2 | P2 | `e3b79c04` | `addServerWithInitializer` берёт identity-ссылку `lease.registry.retain(lease)` (`init.go:2140`) и затем `lease.reacquireContext(ctx, true)` (`:2161`), который берёт вторую ссылку, а освобождает только один `lease.Unlock()`. На успешном пути (`:2187`), на `!admission.valid()` (`:2169`) и на ошибках persist (`:2176`, `:2181`) остаётся +1 ссылка навсегда → запись `leases.entries[name]` не переиспользуется/не освобождается | **Закрыт сразу за диапазоном в `3c3b7dde` (`initLeaseRetained` + `defer lease.registry.release`)** |
| F4 | P1 (историч.) | `5c8641e7` | На success-пути `createSessionWithAdmission` удалён `cancelTimer.Stop()` и не добавлен эквивалент: таймер connect-фазы оставался взведённым и через `mcpTimeout` (по умолчанию 15 с) отменял `mcpCtx` уже опубликованной сессии | **Закрыт в диапазоне через 12 минут: `99371d14` (`stopInitTimer` после `Connect`, `init.go:3025`)** |
| F5 | P3 | `ee32c4a8` | Удаление `db.init()` убрало единственный `goose.SetBaseFS(FS)`. `internal/db/list_candidate_interrupted_assistant_sessions_test.go:30` и `internal/db/messages_pagination_test.go:35` вызывают `goose.Up(conn, "migrations")` без установки base FS и работают только потому, что goose по умолчанию читает `migrations/` относительно cwd, а `go test` ставит cwd = каталог пакета, где каталог реально лежит | Открыт, латентная хрупкость |
| F6 | P3 | `49e008fa` | В `TestDisableServerCancelsBlockedInitAndLeavesNoLateSession` удалён канал `serverDone`; handler теперь безусловно блокируется на `<-r.Context().Done()`. Если ассерт `<-canceled` падает по таймауту, `defer server.Close()` виснет (httptest ждёт активные запросы) — вместо чистого падения получаем таймаут пакета | Открыт, nit |
| F7 | P3 | `77e7a1c4` | `toCodexWrushSkillMD` делает `strings.ReplaceAll(body, "rush.md", "../rush/SKILL.md")` (`internal/cmd/multi_cli_convert.go:113`) после охраняемой замены same-dir-ссылки. Появление `wrush.md` в шаблоне даст `w../rush/SKILL.md`; оба оракула (`assert.NotContains(got,"rush.md")` и `Equal(Count(body,"rush.md"), Count(got,"../rush/SKILL.md"))`) этого не поймают, т.к. считают вхождения и внутри `wrush.md` | Открыт, nit |
| F8 | P3 | `3f3850d7` | Workspace-конфиг, принадлежащий другому владельцу (или недоступный `Stat` рабочий каталог), теперь **обрывает** `Load`/reload (`internal/config/load.go:87-101`, `internal/config/store_reload.go:348-371`), тогда как чужой global/project-конфиг по-прежнему молча пропускается discovery. Триггер в проде: `data_directory` на volume, принадлежащем другому uid (контейнеры/CI). Fail-closed выбран осознанно, но в CHANGELOG/README не описан | Открыт, поведенческое изменение без документации |
| F9 | P3 | `96aee6fd`, `366be5dc`, `71df8f19`, `b6f2ea88` | Мёртвая поверхность API после серии: 9 неиспользуемых экспортируемых обёрток `Persist*AtScope`/`Persist*InScope` (`internal/config/store_mcp.go:137-163`, `:273-295`), неиспользуемые `publishMCPConfigLocked` (`store_mcp.go:297-313`), `fingerprintForBytes` (`store_mcp_transaction.go:681-683`), `uniqueNormalizedPaths` (`:647-662`). Отдельно `ErrMCPAmbiguous = ErrMCPExternal` (`store_mcp.go:13`) — алиас двух разных понятий на один sentinel, из-за чего `errors.Is(err, ErrMCPAmbiguous)` истинно для любой external-ошибки. Линтер `unused` отключён в `.golangci.yml`, так что CI это не поймает | Открыт, nit |
| F10 | P3 | `e3b79c04` | `serverLease.lockContext` (`init.go:313-336`) — busy-poll с таймером на 1 мс вместо блокирующего захвата. Пока write-lease удерживается через дисковую транзакцию (до `configWriteLockTimeout`), каждый ожидающий даёт ~1000 пробуждений/с. Дополнительно `TryLock` снимает writer-preference `sync.RWMutex`: писатель может голодать под потоком читателей | Открыт, design note |
| F11 | P3 | `e3b79c04` | `renewalActive` в `getOrRenewClient` (`init.go:2635`) выставляется в `true` и нигде не сбрасывается — deferred `endRenewal()` выполняется всегда. Переменная рудиментарна и вводит в заблуждение (читается как «есть путь, где renewal не завершается») | Открыт, nit |
| F12 | P3 | `e3b79c04` | `ClientSession.releaseOperation` (`init.go:132-142`) может выполнить SDK `Close()` синхронно в горутине tool call'а (`defer lease.close()` в `tools.go:60`, `resources.go:33`,
`prompts.go:29`), когда последняя операция отпускает отставленную сессию. `cancelContext()` выполняется первым, поэтому по факту ограничено, но время закрытия транспорта теперь входит в latency вызова инструмента | Открыт, design note |
| F13 | P3 | `2e152758` | В doc-комментарии `sdk.Open` внесена опечатка `os.ChDir` (`sdk/sdk.go:388`); в stdlib функция называется `os.Chdir`. Тот же коммит продублировал абзац про CR-7 в 36h-ревью (исправлено в `33ea1841`) | Открыт, nit |
| F14 | P3 | `2c23fcd3` → `5c8641e7` | В `getOrRenewClient` вычисляется `sessionCtx = o.lifecycleCtx` с комментарием «The renewed MCP session itself must survive that lease closing» (`init.go:2656-2662`) и передаётся в `createSessionWithAdmission`, но параметр безусловно перекрывается handoff-контекстом, как только `admission != nil` (то есть всегда при `o != nil`). Значение используется только на legacy-пути без владельца; комментарий описывает уже неработающий механизм | Открыт, стухший комментарий |
| F15 | P3 | `cf12608e`, `366be5dc` | `persistMCPRawAt`/`prepareMCPFileMutation` переписывают конфиг целиком через `json.MarshalIndent(root, "", "  ")` (`store_mcp_transaction.go:306-310`). В отличие от прежней точечной sjson-правки это переформатирует весь пользовательский файл и сортирует top-level ключи при каждой MCP-мутации. Семантика сохраняется (остальные ключи проходят как `json.RawMessage`), но форматирование пользователя теряется | Открыт, note |
| F16 | P3 | `cf12608e`, `366be5dc` | Ключи `mcpLockedFiles` заполняются как `normalizeDiscoveryPath(normalizeReloadPath(p))` (`store_mcp_transaction.go:41-44`, `:85`), а читаются в `mcpPathData` как `normalizeDiscoveryPath(p_raw)` (`:504`). Совпадение обеспечивается только тем, что `s.globalDataPath` и `snapshot.workspacePath` уже приведены `normalizeReloadPath` при загрузке (`internal/config/load.go:68`, `:70`). Инвариант нигде не зафиксирован тестом; при его нарушении post-mutation `evaluateMCPFiles` вернул бы домутационные байты и `MCPMutationResult.NewExists` стал бы ложным (для `AddServer` — ложный «disappeared while persisting» + откат успешной записи). На tip `f2914d53` эта область перестроена (`withMCPWriteLocks` теперь ключует `normalizeDiscoveryPath`), полная перепроверка tip не проводилась | Открыт, латентная связность |
| F17 | P3 | серия | Регрессия на живой stdio-сервер, переживающий `Initialize`, по-прежнему отсутствует: `TestInitializePublishesHTTPSessionBeyondAdmission` покрывает только HTTP-транспорт, stdio-конфиги в тестах остаются `Command: "echo"`/in-memory transports. Механизм общий (`lifetimeCtx`), поэтому это gap полноты | Открыт (перенесено из 36h-ревью) |

Закрытые этим диапазоном (подтверждено по коду): **CR-1**
(`5c8641e7`, `b7d8614f`), **CR-4** (`e3b79c04`), **CR-8** (`b9656981`),
**CR-10**/**CR-11** (`ee32c4a8`), **CR-12** (`b9656981`, `229393f2`).
CR-3 закрыт `1bf8bcdc`/`3199bf34` (lazy init-барьер + `closeInitBarrierLocked`
из `finishClose`). CR-2 оставался закрытым весь диапазон.

---

## Итог

Блокирующих findings на конце диапазона нет. Ключевой вопрос задания
разрешён положительно: **CR-1 действительно закрыт**, и закрыт по сути,
а не по формулировке commit message — начальная сессия получает
собственный lifetime-контекст (`sessionContext`), привязанный к
`Owner.lifecycleCtx`, а не к контексту конкретного вызова `Initialize`;
promotion выполняется до публикации, под `lifecycleMu` и write-lease,
и отказ promotion корректно приводит к закрытию кандидата вместо
публикации мёртвой сессии. Регрессионный тест невакуумен и
дополнительно перекрывает 15-секундную регрессию `F4`, внесённую и
закрытую внутри того же диапазона.

**Блокирует:** ничего.

**Требует внимания:** F1 — реальный флейк теста, воспроизводимый и на
текущем tip. Порядок в `internal/shell/background.go` (`close(done)`
до `armBufferReleaseTimer`) выбран осознанно и правильно; чинить надо
оракул: `TestBackgroundShellManager_Remove_RacingCompletionReleasesBuffers`
должен использовать `require.Eventually` на `bufReleased` вместо
мгновенной проверки после `Wait()` (или ждать явного сигнала о
завершении `armBufferReleaseTimer`).

**Подождёт:** F5-F17 — nit'ы, стухшие комментарии, мёртвая поверхность
API и два design note (F10 busy-poll lease, F12 синхронное закрытие
транспорта в горутине tool call'а). F2 и F4 закрыты (F2 — коммитом
`3c3b7dde` сразу за диапазоном, F4 — внутри диапазона) и оставлены в
таблице как исторический след.

Остальные 30+ коммитов (транзакционные мутации MCP-конфига, origin/scope
resolution, доверенная идентичность конфиг-файлов, изоляция
library/application, Codex Skills, shutdown ordering) корректны, их
комментарии соответствуют коду, тесты в большинстве своём невакуумны и
новых неограниченных утечек не добавляют.
