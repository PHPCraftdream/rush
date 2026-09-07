# Ревью коммитов, chunk 5/5 (`3f3850d7b..f2914d53c`)

43 коммита от 2026-09-06 07:07 до 2026-09-07 01:15 CEST — последний срез
7-дневного окна `da53fe42a..f2914d53c`, заканчивающийся на текущем tip'е.
Проверка: корректность, полнота, пропущенные edge cases, невакуумность
тестов, соответствие commit message фактическому diff, concurrency,
goroutine/resource leaks, error handling.

Режим только чтения: `go build`/`go test`/`go vet`/`golangci-lint`/hooks не
запускались (жёсткое ограничение задания). Все выводы получены чтением
диффов и текущего состояния файлов на tip'е воркtree.

Диапазон не пересекается с `docs/reviews/2026-09-05-2047-commit-review-36h.md`
(`239c8943..0e4b9ed3`). Единственное, что цитируется из того документа —
формулировка механизма **CR-1**; сам статус CR-1 на tip'е выведен заново
трассировкой живого кода (раздел 1), а не из его «Current status» пометок.

---

## Сводка коммитов

Хронологически (снизу вверх в `git log`):

| Hash | Тема | Вердикт |
|---|---|---|
| `3c3b7dde0` | completeness MCP lease fencing, `publishIfCurrent` | корректно в основном; **вносит CH5-2** (регрессия ожидания renewal) |
| `5da43c1ae` | fence exported `RefreshResources` | корректно |
| `aa3ac7024` | docs: CR-4 follow-up | — |
| `922316af4` | deferred refresh, lazy init barrier, `InitializeSingle` под lease | корректно; откатывает `sessionCtx = o.lifecycleCtx` в пользу handoff-promote (эквивалент, см. §1) |
| `3875b7be6` | config transaction identity (unix commit, alias records) | корректно |
| `f47c285bc` | рандомизированные descriptor-relative temp-имена | корректно |
| `f7ea4c803` | docs | — |
| `646b0f49e` | линеаризация candidate promotion/notifications | корректно |
| `6c2030dd7` | deferral raw-notification до commit | корректно |
| `acae70a90` | CAS aliases + MCP durability | корректно |
| `29f44e2f2` | сохранение fsync-uncertainty для MCP | корректно, см. §5 note |
| `b079cd57a` | reconcile generic uncertain writes | корректно |
| `54c48a8f3` | типизированные `CommitOutcome` | корректно |
| `98f769e7c` | reconcile uncertain lifecycle commits | корректно; вводит `else`, из-за которого позже возникает **CH5-4** |
| `c4440e60d` | typed commit outcome status | корректно |
| `f551d136d` | durability outcomes, darwin/linux rename | корректно |
| `629657707` | recovery осиротевших temp alias | корректно, guarded fail-closed |
| `18332c65b` | fence uncertain config lifecycle; `ReloadAndReconcileMCPConfig` в coordinator | корректно; расширяет частоту reload'ов (усиливает CH5-3) |
| `f739fabd1` | `renameatx_np` + `reloadMutex.LockContext` | корректно, nit по starvation |
| `46f68de7e` | линеаризация add publication с durable commit | корректно |
| `4040f4d56` | детерминированный oracle через `serverLeaseHooks` | корректно (production lock ownership не меняется) |
| `fc1d4f815` | platform durable publication | корректно |
| `a470adf63` | close detached transports вне server lease | корректно, важное улучшение; nit §6 |
| `49c92cb19` | линеаризация remove-события | корректно |
| `c2206f6e2` | Windows durability outcomes | корректно |
| `a62a37e60` | линеаризация replacement-события | корректно; nit §6 (`startFallback` до retire) |
| `5b6f8976f` | test: изоляция providerless фикстур | корректно |
| `bd6e9dc9d` | test: закрытие HTTP-фикстур | корректно |
| `e69d44f91` | валидация frontmatter установленного codex skill | корректно, тесты невакуумны |
| `32af28b33` | pin Windows publication handles (NtCreateFile-relative) | корректно |
| `b639b3170` | fence uncertain MCP mutations | корректно |
| `27d1639da` | fence Windows parent replacement + `parsed.Validate()` | корректно |
| `1133f89d4` | Windows aliases → physical parents | корректно |
| `da066d7f9` | uncertainty вынесена из snapshot в `ConfigStore` | корректно, закрывает реальную дыру (reload терял fence) |
| `59d1d8d9d` | fence rename/enable rollback races | корректно по существу; **вносит CH5-4** (dead code) |
| `eaae036be` | MCP alias fingerprints | корректно |
| `c7dfca658` | fence stale replacement lifecycle results | корректно |
| `6c15f3306` | условный enable-rollback (CAS-токен) | корректно |
| `79215f9d5` | Windows read/rename retries | корректно; **вносит CH5-5** (leak handle) |
| `62371a334` | pin MCP lifecycle publication against disk | корректно |
| `779802b20` | fence MCP lifecycle admissions | **вносит CH5-3** (resolverRevision фенсит кандидатов) |
| `28c14030c` | preserve Windows rename retry CAS | корректно; переносит CH5-5 |
| `f2914d53c` | preserve MCP session identity across revisions | важное улучшение, но **вносит CH5-1** (data race на `committedAdmissions`) |

Тела commit message пусты у всех 43 коммитов. Для этой серии это особенно
дорого: `internal/agent/tools/mcp/init.go` вырос на +2560/−639 строк за 18
часов, и обоснование каждого шага не зафиксировано нигде, кроме заголовка.

---

## 1. CR-1 на tip'е: **закрыт**

Задание требовало не верить commit message'ам и проследить цепочку контекста
в текущем файле. Сделано; ниже трассировка целиком по
`internal/agent/tools/mcp/init.go` на `f2914d53c`.

Механизм CR-1 (по предыдущему ревью): `Owner.Initialize` создавал `initCtx` и
безусловно отменял его при выходе; транспорт сессии (stdio-процесс через
`exec.CommandContext`, HTTP round-tripper через `headerRoundTripper.ctx`)
деривировался из этого ctx, поэтому каждый сконфигурированный MCP-сервер
умирал сразу после листинга tools/prompts и респаунился на первом реальном
вызове.

Цепочка на tip'е:

1. `Owner.Initialize` — `initCtx, cancel := context.WithCancel(ctx)`
   (`init.go:2001`), `defer cancel()` (`:2026`) — **сохранился**.
2. `admitServerForConfig(initCtx, …, bump=true)` (`:2085`) →
   `admitServerWithConfig` → `operationCtx, cancel = context.WithCancel(ctx)`
   (`:953`), т.е. `admission.ctx` — потомок `initCtx`.
3. Горутина: `initClientAdmitted(admission.ctx, …)` (`:2115`) →
   `initClientAdmittedWithState`, где `defer admission.done()` (`:2226`) в
   итоге вызывает `a.cancel()` (`:752-754`), а
   `operationCtx, finish = admission.owner.operationContext(ctx)` +
   `defer finish()` (`:2242-2244`) отменяет ещё один потомок.
4. `prepareClient(operationCtx, …)` (`:2245`) →
   `createSessionWithAdmission(ctx=operationCtx, …)` (`:2263`).
5. **Ключевое звено** — `createSessionWithAdmission` (`:4761-4781`):

   ```go
   candidateCtx, candidateCancel = context.WithCancel(admission.ctx)
   candidateStop = context.AfterFunc(ctx, candidateCancel)
   handoff = newSessionContextWithCaller(
       admission.owner.lifecycleCtx, // owner
       candidateCtx,                 // candidate
       ctx,                          // caller
       candidateCancel, candidateStop)
   sessionCtx = handoff
   ```

   `lifetimeCtx, cancelSession := context.WithCancelCause(sessionCtx)`
   (`:4782`) и именно `lifetimeCtx` уходит в `createTransport` (`:4802`) и
   `client.Connect` (`:4839`). То есть транспорт больше не привязан к
   `initCtx`/`admission.ctx` напрямую — только к `*sessionContext`.
6. `sessionContext.Done()` (`:4751`) закрывается только через
   `finish()`/`rejectLocked()`, а рабочая горутина (`:4644-4660`) после
   успешного `promote()` возвращает `false` из `finish(candidate, true)`
   (`:4666`) и дальше ждёт **только** `owner.Done()` или `s.done`.
7. `promote()` вызывается из `publishPreparedClientLocked`
   (`session.promoteContext()`, `:2409`), из `commitRenewalForLease`
   (`:1717`) и из `ReplaceServer` (`:3037`). После него `candidateStop`
   снимается, `candidateCancel` обнуляется (`:4707-4711`), и отмена
   `admission.ctx`/`operationCtx`/`initCtx` на сессию не влияет.

Итог: **CR-1 закрыт**. Механически его закрыли `5c8641e7` («harden MCP
session lifecycle ownership») и `b7d8614f` («linearize MCP session
promotion») — оба вне моего диапазона (они предшествуют `3f3850d7b`, т.е.
попадают в chunk 4). В моём диапазоне цепочка не сломана; наоборот,
`922316af4` укрепил её: он снял отдельный костыль
`sessionCtx = o.lifecycleCtx` в renewal-пути `getOrRenewClient` и заменил
его тем же handoff-механизмом (`init.go:4328-4333`, комментарий
«createSessionWithAdmission then hands its lifetime context over to the
owner when the candidate is committed»), а `createSessionWithAdmission`
получил явную фиксацию инварианта в комментарии `:4861-4862` («Keep the
context live for the published SDK connection. The caller's operation
context is intentionally not its lifetime context»).

Проверенные граничные случаи, которые могли бы вернуть CR-1:

- **Renewal без owner** (`o == nil`, только тесты): `admission == nil` →
  `handoff == nil` → `sessionCtx = ctx` вызывающего. Сессия действительно
  умрёт вместе с ctx вызова, но эта ветка недостижима в production
  (`currentOwner()` не nil, пока App жив).
- **`promote()` до `wg.Wait()`**: `Initialize` ждёт горутины (`:2120`) до
  `defer cancel()`, так что даже без handoff'а гонки бы не было — но
  handoff покрывает и `InitializeSingle`/`EnableServer`/`AddServer`, где
  `defer cancel()` срабатывает раньше публикации.
- **Утечка горутины handoff'а**: одна на опубликованную сессию; выходит по
  `owner.lifecycleCtx.Done()` или по `s.done`, который закрывает
  `ClientSession.Close() → terminal() → handoff.abort()` (`:70-72`,
  `:4877-4881`). Все пути отказа (`prepareClient`, `discardPreparedClient`,
  `closeMCPClient`) вызывают `Close()`, а не только `cancelContext()`.

Отдельно: `candidateCancel` на успешном promote **не вызывается**, а
обнуляется (`:4710`). Это не утечка — `candidateCtx` — потомок
`admission.ctx`, который гарантированно отменяется в `admission.done()`
(`:752-754`); контекст-потомок отцепляется от родителя в этот момент.

---

## 2. CH5-1 (**P0**): гонка на `Owner.committedAdmissions` — `fatal error: concurrent map writes`

Введено в **`f2914d53c`** — самом свежем коммите диапазона.

`f2914d53c` добавил process-wide карту `Owner.committedAdmissions`
(`init.go:580`), которая индексируется именем сервера и защищена **только**
`lifecycleMu`. Все чтения/записи в новом коде это соблюдают:

- `publishPreparedClientLocked` — запись под `lifecycleMu` (`init.go:2430`);
- `commitRenewalForLease` — запись под `lifecycleMu` (`:1735`);
- `replaceServer…` — запись/удаление внутри `lifecycleMu.Lock(); defer
  Unlock` (`:3143`, `:3157`, `:3164`);
- `reconcilePublishedSessions` — итерация под `lifecycleMu` (`:1583`);
- `detachInvalidCommittedSessionLocked` — чтение под `lifecycleMu` (`:5003`);
- `finishClose` — `clear()` под `lifecycleMu` (`:1833`);
- даже тесты берут `lifecycleMu` вокруг присваивания
  (`state_regression_test.go:1272-1274`, `:1352-1361`).

Но тот же коммит добавил удаление из этой карты внутрь
`detachSessionLocked`:

```go
// init.go:3877-3886
func detachSessionLocked(name string) *ClientSession {
	if o := owner; o != nil {
		delete(o.committedAdmissions, name)   // ← plain map write
	}
	if session, ok := sessions.Get(name); ok {
		sessions.Del(name)
		return session
	}
	return nil
}
```

Комментарий над функцией (`:3875-3876`) говорит только про server write
lease и ничего про `lifecycleMu`. И действительно, **пять** из её вызовов
`lifecycleMu` не держат:

| Место | Контекст |
|---|---|
| `init.go:2474` (`DisableSingle`) | `lease.Lock()`; `o.invalidateServer(name)` на `:2473` берёт и **отпускает** `lifecycleMu` (`:1137-1144`) |
| `init.go:2599` (`disableServerWithResultPersistence`, reconciled-ветка) | то же, после `o.invalidateServer` на `:2598` |
| `init.go:2627` (`disableServerWithResultPersistence`, успешная ветка) | то же, после `:2626` |
| `init.go:3757` (`removeServerWithResultPersistence`, pending-add) | то же, после `:3755` |
| `init.go:3798` (`removeServerWithResultPersistence`) | то же, после `:3797` |
| `init.go:3869` (`fenceMCPRuntimeLocked`) | `lifecycleMu` явно отпущен на `:3864` перед вызовом |

Кроме того, `detachSessionLocked` читает глобальный `owner` без
`lifecycleMu` — вторая, более мелкая гонка на том же участке.

`sessions`/`states`/`allTools` — это `csync.Map` с внутренним `sync.RWMutex`
(`internal/csync/maps.go:10-14`), поэтому раньше вызов без `lifecycleMu` был
безопасен. `committedAdmissions` — обычная Go-карта.

**Сценарий отказа.** Два сервера, `A` и `B`, разные server lease (взаимного
исключения между ними нет):

- goroutine 1: `Owner.Initialize` поднимает `B`; после листинга
  `publishPreparedClientLocked` берёт `lifecycleMu` и пишет
  `committedAdmissions["B"] = admission` (`:2430`);
- goroutine 2: web UI/`rush mcp remove` дергает `RemoveServer("A")` →
  `removeServerWithResultPersistence` → `o.invalidateServer("A")` (отпускает
  `lifecycleMu`) → `detachSessionLocked("A")` → `delete(owner.committedAdmissions, "A")`
  **без** `lifecycleMu`.

Одновременные write+delete по одной Go-карте → runtime бросает
`fatal error: concurrent map writes`. Это не паника — `throw`, его нельзя
`recover`, процесс `rush` (web-сервер со всеми сессиями) падает целиком.
Если детектор не сработает — тихая порча бакетов карты.

Триггеры на стороне goroutine 2 не экзотические: `DisableServer`,
`DisableSingle`, `RemoveServer` и **любой** `fenceMCPRuntimeLocked` (то есть
каждый неопределённый durable-commit — а весь смысл Windows-ветки этого же
диапазона в том, чтобы такие исходы стали наблюдаемыми). На стороне
goroutine 1 — публикация любой сессии, включая автоматический renewal после
неудавшегося `Ping` (`commitRenewalForLease`, `:1735`), который происходит
сам по себе, без участия пользователя.

**Исправление:** сделать `detachSessionLocked` действительно
lifecycle-locked — либо перевести шесть перечисленных вызовов на
`lifecycleMu.Lock()`/`Unlock()` вокруг блока «invalidate + detach + clear +
setState» (как уже сделано в `getOrRenewClient:4317-4321` и
`fenceInvalidCommittedNotification:4974-4983`), либо вынести
`delete(committedAdmissions)` в отдельный `*Locked`-хелпер и вызывать его
только из мест, где `lifecycleMu` уже удержан. Заодно заменить чтение
глобального `owner` внутри функции на явный параметр.

**Регрессионный тест:** `-race`, две горутины — одна крутит
`publishPreparedClientLocked` для сервера `B`, вторая `DisableSingle("A")`
в цикле; без фикса детектор гонок стреляет на первом же пересечении.

---

## 3. CH5-2 (**P2**): конкурентные MCP-вызовы во время renewal теперь падают вместо ожидания

Введено в **`3c3b7dde0`**, живо на tip'е.

`3c3b7dde0` переставил порядок проверок в цикле `getOrRenewClient`.
Было (`3f3850d7b:internal/agent/tools/mcp/init.go:2475-2503`):

```go
sess, ok = sessions.Get(name)
…
if (!ok || retired) && lease.renewing {   // ← ждём renewDone
    …
    case <-renewDone: continue
}
m, exists = cfg.MCPConfig(name)
if !ok || !exists || m.Disabled { return "mcp '…' not available" }
```

Стало (`init.go:4117-4170`):

```go
sess, ok = sessions.Get(name)
…
m, exists = cfg.MCPConfig(name)
…
if !ok || !exists || m.Disabled {          // ← :4141, теперь ПЕРЕД ожиданием
    lease.Unlock(); … ; return nil, fmt.Errorf("mcp '%s' not available", name)
}
if (!ok || retired) && lease.renewing {    // ← :4149, дизъюнкт !ok мёртв
    …
}
if !ok { … }                               // ← :4163, весь блок мёртв
```

Renewal-путь снимает сессию с публикации под write-lease
(`detachSessionLocked` на `:4319`), отпускает lease на `:4325` и уходит в
`createSessionWithAdmission` — сетевой шаг длиной до `mcpTimeout` (по
умолчанию 15 с, `:5265-5267`). Всё это время `sessions.Get(name)` даёт
`!ok`.

**Сценарий отказа.** Один MCP-сервер, две параллельные сессии агента
(штатный режим форка — «N concurrent web sessions»). Сессия 1 получает
ошибку `Ping`, начинает renewal и уходит на респаун `npx`-сервера (секунды).
Сессия 2 в этот момент вызывает любой инструмент того же сервера:
`getOrRenewClient` берёт lease, видит `!ok`, `exists && !Disabled` — и
возвращает `mcp 'X' not available` вместо того, чтобы дождаться
`lease.renewDone` и переиспользовать новую сессию. Модель получает ошибку
инструмента; самолечение только на следующей попытке. Именно ради этого
случая `renewDone` и вводился.

Побочный признак того, что это непреднамеренно: после перестановки
`!ok`-дизъюнкт в условии `:4149` и весь блок `:4163-4169` стали
недостижимыми — до `:4149` `ok` всегда истинно.

**Исправление:** вернуть ожидание `renewDone` перед проверкой доступности,
т.е. переставить блок `:4149-4162` выше блока `:4141-4148`, оставив
проверку `!exists || m.Disabled` на прежнем месте (она не связана с
renewal). После этого блок `:4163-4169` снова становится живым.

---

## 4. CH5-3 (**P1**): любой успешный `ReloadFromDisk` убивает все MCP-кандидаты в полёте

Введено в **`779802b20`**.

`779802b20` добавил в `serverAdmission` поля `mcpRevision`/`resolverRevision`
(`init.go:693-694`) и стал проверять их в `candidateValidLocked`
(`init.go:850-860`):

```go
if a.hasConfigIdentity {
	snapshot := a.cfg.SnapshotMCPAdmission(a.name)
	if a.mcpRevision != 0 && snapshot.MCPRevision != a.mcpRevision ||
		a.resolverRevision != 0 && snapshot.ResolverRevision != a.resolverRevision {
		return false
	}
	…
}
```

`mcpRevision` после `f2914d53c` инкрементируется точечно
(`mcpRevisionDiff`, `internal/config/mcp_revisions.go:64-104` — только для
имён, у которых поменялось эффективное определение или байты входного
документа). Это правильная гранулярность.

`resolverRevision` — нет. Он инкрементируется **безусловно на каждом
успешном reload**:

```go
// internal/config/store_reload.go:512
candidate.resolverRevision = cur.resolverRevision + 1
```

Других мест, где он меняется, в пакете нет (`load.go:128,243`,
`store.go:553,576,607` только инициализируют единицей).

Тем же коммитом `779802b20` все lifecycle-точки перевели с `admitServer` на
`admitServerForConfig`, т.е. `hasConfigIdentity = true` теперь у **всех**
кандидатов: `Owner.Initialize` (`:2085`), `InitializeSingle` (`:2209`),
`EnableServer` (`:2809`), `AddServer` (`:3363`), `startFallback` (`:4032`).

**Следствие:** пока сервер поднимается (для stdio через `npx`/`uvx` это
единицы–десятки секунд), любой успешный reload конфига — даже такой, где ни
одно MCP-определение и ни одна переменная резолвера не изменились — делает
`candidateValidLocked` ложным. Дальше:

- `prepareClient` на `:2296-2299` закрывает уже установленную сессию и
  возвращает `ErrOwnerBusy`;
- в `Owner.Initialize` эта ошибка только логируется на уровне Debug
  (`:2115-2117`);
- `updateAdmissionStateUnpinned` (`:4574-4577`) молча ничего не публикует;
- ретрая нет — `Initialize` вызывается один раз на App.

Сервер навсегда остаётся в `StateStarting` (если reload успел после
публикации `StateStarting`) или вовсе отсутствует в `states`.

**Насколько это достижимо.** Очень: `18332c65b` (в этом же диапазоне)
поменял `checkLivePeakHours` так, что при «грязном» конфиге он делает
`mcp.ReloadAndReconcileMCPConfig` перед **каждым** provider-запросом
(`internal/agent/coordinator_providers.go:741-748`). В форке с N
параллельными `rush run`/web-сессиями чужая запись конфига делает
`ConfigStaleness().Dirty` истинным регулярно. Плюс собственный
`autoReloadAfterWrite` после любой записи конфига
(`internal/config/store_write.go:336,353,387,397`) — например смены модели в
web UI на старте, ровно когда MCP-серверы ещё коннектятся.

`f2914d53c` признал ровно эту проблему для **опубликованных** сессий: он
вынес revision-проверки из `committedValidLocked` и заменил их на
`mcpConnectionConfigEqual` (`init.go:905-915`), чтобы живая сессия
переживала reload. Для кандидатов та же поправка не сделана.

**Исправление:** убрать `resolverRevision` из `candidateValidLocked`
(оставить `mcpRevision` + сравнение самого определения — они уже дают
нужную защиту), либо считать «резолвер-ревизию» от фактических значений,
влияющих на этот MCP (результат `ResolvedArgs`/`ResolvedEnv`/`ResolvedURL`/
`ResolvedHeaders`), а не от счётчика reload'ов. Как минимум — добавить
retry: сейчас единственный отказ кандидата означает, что сервер не
поднимется до перезапуска процесса.

**Регрессионный тест:** поднять owner с одним HTTP MCP, застабить
`prepareClient` на паузу между `createSession` и публикацией, из второй
горутины сделать `store.ReloadFromDisk` без изменений на диске, снять
паузу — сессия должна опубликоваться.

---

## 5. CH5-4 (**P2**): недостижимый код в `enableServerWithPersistenceAndInitializerAndRollback`

`init.go:2767-2784` на tip'е:

```go
	if !o.acceptsSession() {
		if commitUncertainty == nil {
			detached, rollbackErr := rollbackAndReport(ErrOwnerBusy)
			lease.Unlock()
			retireMCPClient(name, detached)
			return rollbackErr
		} else {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			if commitUncertainty != nil {          // ← всегда true в этой ветке
				return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
			}
			return ErrOwnerBusy                     // ← мёртвая строка
		}
		lease.Unlock()                              // ← :2782, недостижимо
		return ErrOwnerBusy                         // ← :2783, недостижимо
	}
```

Хвост стал недостижимым в `59d1d8d9d`: до него ветка
`commitUncertainty == nil` вызывала `rollbackPersistence()` и **проваливалась
вниз**, поэтому `:2782-2783` были живым выходом; после замены на
`return rollbackErr` обе ветки `if/else` терминальны.

Практического бага (двойной `lease.Unlock()`) здесь нет — код просто
недостижим. Но:

1. Это верный признак незавершённой правки: следующий читатель разумно
   решит, что `commitUncertainty == nil` всё ещё falls through.
2. `govet`'s `unreachable` входит в набор анализаторов, который
   `golangci-lint` включает по умолчанию, а `govet` в `.golangci.yml`
   не отключён (отключены только `errcheck`, `ineffassign`, `unused`), и
   CI гоняет `golangci-lint run --config=.golangci.yml ./...`
   (`.github/workflows/lint.yml:50-53`). Я не мог запустить линтер
   (read-only режим), поэтому утверждаю осторожно: это **вероятная** красная
   CI-проверка начиная с `59d1d8d9d`. Проверить дешевле, чем спорить.

**Исправление:** убрать `else` и хвостовые `:2782-2783`, оставив
`if commitUncertainty == nil { … return rollbackErr }` и линейное
продолжение для uncertain-случая.

---

## 6. CH5-5 (**P3**): утечка Windows-хендла в `verifyWindowsRenameRetryState`

`internal/config/config_rename_windows.go:173-179`:

```go
	target, err := openWindowsConfigEntryAt(parent, filepath.Base(destination))
	if !replace {
		if isWindowsEntryNotFound(err) || os.IsNotExist(err) {
			return nil
		}
		return errConfigCommitVerification   // ← target не закрыт, если err == nil
	}
	if err != nil { … }
	defer target.Close()                     // ← :183, слишком поздно
```

Ветка `!replace && err == nil` — «ожидали, что назначения нет, а оно есть» —
возвращает ошибку, не закрывая уже открытый `*os.File`. Хендл держит
`FILE_SHARE_READ|WRITE|DELETE`, так что чужие операции он не блокирует, а
`os.NewFile` ставит финализатор, поэтому дескриптор рано или поздно
закроется GC — но это недетерминированно, и на Windows задержка закрытия
может продлить окно, в котором POSIX-семантика delete/rename на этом файле
даёт `ERROR_ACCESS_DENIED` другому процессу — ровно тот класс ошибок,
который `28c14030c` и пытается ретраить.

Введено в `79215f9d5` (в исходном `windowsRenameRetryState` было
`return isWindowsEntryNotFound(err) || os.IsNotExist(err)` — та же утечка) и
перенесено в `28c14030c`. Оба коммита в моём диапазоне.

**Исправление:** перенести `defer target.Close()` (с nil-guard) сразу после
вызова `openWindowsConfigEntryAt`, до ветки `!replace`.

---

## 7. Композиция двух тредов: MCP-fencing × Windows config durability

Задание просило проверить, не ломают ли друг друга «MCP lifecycle fencing» и
«Windows config durability». Точка стыка одна и она явная:

```
writeMCPFileChanges (store_mcp_transaction.go:633-727)
  → commitConfigFile (config_commit_windows.go:18 / config_commit_unix.go:21)
  → *CommitOutcome
  → mcpCommitUncertainError / mcpCommitWasReconciled (store_mcp.go:22-37)
  → CommitOutcomeFromError в MCP (init.go)
  → commitOutcomeNeedsRuntimeFence / commitOutcomeIsReconciled (init.go:3988-3994)
  → fenceMCPRuntimeLocked → cfg.MarkMCPUncertain (init.go:3859-3873)
```

Что проверено и **работает**:

- **Порядок блокировок.** `publishMu → diskWriteMu → sidecar file locks` в
  конфиге; `server lease → lifecycleMu` в MCP. `WithCurrentMCPMutation`
  (`store_mcp.go:68-81`) соединяет их в одном направлении: caller держит
  server lease, берёт `publishMu`/`diskWriteMu`, затем `fn()` берёт
  `lifecycleMu`. Обратного ребра (`lifecycleMu → publishMu`) нет:
  `admitServerWithConfig`, `candidateValidLocked`, `committedValidLocked`
  под `lifecycleMu` вызывают только `SnapshotMCPAdmission`/`MCPConfig`
  (lock-free `atomic.Pointer`) и `MCPUncertaintyVersion` (отдельный
  leaf-RWMutex, `mcp_uncertainty.go:31-39`). Дедлока не нашёл.
- **Reload вне блокировок.** Все MCP-входы вызывают `reconcileUncertainty`
  до взятия server lease (`init.go:2464`, `:2533`, `:2672`, `:3338`,
  `:3689`, `:4004`, `:4067`), а `reconcilePublishedSessions` берёт lease
  уже после того, как `ReloadFromDisk` отпустил `reloadMu`/`publishMu`.
  Комментарии (`:1479-1481`, `:1509-1511`) это фиксируют, и код им
  соответствует.
- **Версионирование fence'ов.** `runReloadLocked` снимает
  `MCPUncertaintyVersions()` до цикла попыток
  (`store_reload.go:298`) и чистит только те версии, что не изменились
  (`mcp_uncertainty.go:66-77`). Fence, поднятый во время reload'а,
  переживает его. Это корректно, потому что `MarkMCPUncertain` вызывается
  строго после возврата из `persist`, т.е. состояние диска к моменту снятия
  снимка уже устоялось.
- **`da066d7f9`** закрыл реальную дыру: до него uncertainty жила в
  заменяемом snapshot'е и терялась при первом же reload'е. Вынос в
  `ConfigStore.mcpUncertainty` (отдельный `sync.RWMutex`, вне snapshot'а) —
  правильный.
- **Windows-специфика readback'а.** `commitConfigFile` (windows) требует
  `committedFingerprint.parentDiscovery == expected.parentDiscovery`
  (`config_commit_windows.go:103-104`). `configDiscoveryFingerprint` для
  директорий намеренно не включает mtime (`store_reload.go:206-218`), а
  `configFileIdentityOf(os.FileInfo)` на Windows всегда возвращает пустую
  структуру (`config_file_identity_windows.go:29`), поэтому создание
  temp-файла рядом с целью не делает parentDiscovery «грязным» и не
  превращает штатный commit в ложный fence. Это ровно то, ради чего сделаны
  `27d1639da`/`1133f89d4`; проверено — держится.

Что **не** держится или требует внимания (design notes, не баги диапазона):

- **CH5-6 (P2, design).** `MarkMCPUncertain` снимается **только** полным
  успешным `ReloadFromDisk`. Но `buildAndPublishReload` падает по причинам,
  никак не связанным с MCP: `failed to load providers during reload`
  (`store_reload.go:449`), `failed to configure providers during reload`
  (`:459` — сюда входит зависшая `$(...)` shell-подстановка в `api_key`
  провайдера), `invalid hook configuration on reload` (`:437`),
  `failed to configure selected models during reload` (`:471`). Пока такая
  ошибка не устранена, **каждая** операция над зафенсенным MCP-сервером
  возвращает `ErrMCPConfigUncertain`, а `Owner.Initialize` его пропускает
  («Skipping MCP with unreconciled config mutation», `:2039-2042`). То есть
  сломанный provider-hook надолго отключает MCP-сервер, у которого когда-то
  был неопределённый commit. Fence в памяти, так что рестарт лечит; но
  диагностируется это тяжело. Стоит хотя бы логировать причину отказа
  reload'а на уровне Warn в `reconcileUncertainty` (сейчас `slog.Warn` есть
  только в `Owner.Initialize:2016-2018`, а `reconcileUncertainty` в
  остальных точках возвращает ошибку молча).
- **CH5-7 (P3).** Durability-uncertain (`errConfigCommitDurabilityUncertain`
  — rename прошёл, fsync родительского каталога нет) после
  `writeMCPFileChanges:659-681` возвращается как `Committed && Reconciled`,
  то есть **не** фенсится (`commitOutcomeNeedsRuntimeFence` = false), но
  ошибка всё равно доходит до пользователя. В `EnableServer`
  (`init.go:2839-2841`) это даёт «сервер успешно поднялся, но enable вернул
  ошибку». Поведение защитимое, однако в web UI выглядит как ложный отказ.
- **CH5-8 (P3).** `a62a37e60`/`49c92cb19` переставили `startFallback` так,
  что он выполняется **до** ретайра старой сессии:
  `removeServerWithResultPersistence` (`init.go:3811-3813`: `lease.Unlock()`
  → `startFallback` → `retireMCPClient`) и
  `replaceServerWithResultPersistenceAndPreparation` (`:3236-3244`). Для
  stdio-сервера это значит, что старый процесс ещё жив, пока fallback
  спаунит новый; сервер, держащий эксклюзивный ресурс (сокет, lock-файл,
  порт), в этот момент откажет новому экземпляру. Раньше `unlock()` ретайрил
  до fallback'а.

---

## 8. Утечки памяти и горутин

**Новых unbounded утечек не найдено.** Разбор новых/изменённых источников:

- `sessionContext` worker (`init.go:4644-4660`) — по одной горутине на
  созданную сессию. Выход: `owner.lifecycleCtx.Done()`, либо `s.done`,
  который закрывают `promote`-отказ (`rejectLocked`), `abort()` из
  `ClientSession.Close()` (`:70-72`) и `finish()` из cancel-воркера. Все
  пути отказа в `prepareClient`/`publishPrepared*`/`discardPreparedClient`
  идут через `Close()`/`closeMCPClient`, а не через голый `cancelContext()`,
  поэтому висящих воркеров не остаётся. `finishClose` (`:1819-1829`) сначала
  `cancelContext()` всем, потом последовательный `Close()` — воркеры
  завершаются на первом же шаге через `owner.Done()`.
- `context.AfterFunc` в `operationContext` (`:1761`), `Initialize` (`:2024`),
  `admitServerWithConfig` (`:954`), `admitReplacementCandidate` (`:1004`),
  `acquireOperation` (`:119`), `RoundTrip` (`:5243`) — все снимаются
  соответствующими `stop()`.
- `Owner.committedAdmissions` — ограничена числом имён серверов; чистится в
  `detachSessionLocked`, обеих ветках replace (`:3143`, `:3164`) и
  `clear()` в `finishClose` (`:1833`). Утечки нет (но см. CH5-1 —
  синхронизация).
- `Owner.refreshPending`/`refreshRunning` — ключи включают
  `candidateToken`; deferred-записи снимаются `discardDeferredRefreshesLocked`
  из `admission.done()` (`:738-740`) и `nextRefresh` (`:1269-1272`).
  Неограниченного роста при постоянно отменяемых кандидатах не нашёл.
- `serverLeaseHooks` (`4040f4d56`) — package-global test seam, в production
  все три поля nil; `Lock/Unlock` серверного lease теперь берут ещё один
  `sync.Mutex` на каждый вызов. Это лишний uncontended lock на горячем пути
  (`serverLease.Lock` вызывается на каждый tool call), но не утечка.
- `leaseRegistry` — refcounted, `release` удаляет запись при нуле
  (`:276-287`); `lockContext` на всех отказных путях отдаёт ссылку
  (`:373`, `:395`, `:401`, `:410`).
- Windows: единственный незакрытый хендл — CH5-5.

---

## 9. Качество тестов

Выборочно проверил невакуумность нового покрытия (+~2900 строк тестов):

- `TestGetOrRenewFencesStaleCommittedSessionBeforePing`
  (`state_regression_test.go:1305-1337`) — считает `ping`-вызовы через
  receiving middleware и требует `pingCalls == 0`; до фикса устаревшая
  сессия действительно пинговалась. Невакуумно.
- `TestReloadReconciliationWaitsForNewerLeaseWinner`
  (`state_regression_test.go:1340-1414`) — детерминирован через
  `serverLeaseHooks.beforeLockFn`, а не через sleep; проверяет, что
  reconcile не сносит победившую более новую сессию. Хороший оракул.
- `TestInvalidCommittedNotificationClearsAllAdvertisedData`
  (`:1252-1303`) — проверяет ровно одно событие и отсутствие лишних
  (`select … default: t.Fatalf`). Невакуумно.
- `TestEnableRollbackPreservesConcurrentDefinitionAndFencesStore`
  (`p1_p2_regression_test.go`) — второй `ConfigStore` через `config.Init`
  меняет определение между persist и rollback; ожидает
  `ErrOwnerBusy` + `ErrMCPConfigUncertain` + сохранённое чужое значение на
  диске. До `6c15f3306` безусловный rollback затёр бы чужую запись.
  Невакуумно и точно бьёт в заявленный баг.
- `TestWriteCodexSkillRejectsInvalidFrontmatter` / `…EmptyDescription` /
  `…OversizedDescription` (`codex_init_test.go:184-202`) — проверяют и
  ошибку, и отсутствие файла на диске. Невакуумно.
- `5b6f8976f`/`bd6e9dc9d` — чистые тестовые коммиты: изолируют
  providerless-фикстуры и закрывают `httptest.Server`/`mcp.Server` через
  `cleanupTestMCPServer`. Без них соседние тесты ловили чужие события и
  подвешенные HTTP-серверы. Полезно и по делу.

**Чего тесты не покрывают** (и почему находки выше не были пойманы CI):

- CH5-1: во всех тестах, трогающих `committedAdmissions`, доступ обёрнут в
  `lifecycleMu` вручную; ни один тест не гоняет `DisableSingle`/`RemoveServer`
  для одного имени **параллельно** с публикацией другого имени, а именно это
  и нужно, чтобы `-race` увидел гонку.
- CH5-2: нет теста «второй вызов во время renewal дожидается `renewDone`».
  Существующие renewal-тесты (`operation_lease_test.go`) синхронны.
- CH5-3: нет теста «reload без изменений во время инициализации не убивает
  кандидата». Есть обратный — `TestGetOrRenewFencesStaleCommittedSession…`,
  где конфиг реально меняется.

---

## Открытые findings

| ID | Severity | Коммит | Описание | Статус |
|---|---|---|---|---|
| CR-1 | — | `01e909d0` (истор.) | `Initialize` отменял ctx транспортов сразу после инициализации | **Закрыт на tip'е `f2914d53c`.** Механизм: handoff `sessionContext` + `promote` (`init.go:4761-4781`, `:4687-4716`, `:4861-4883`); транспорт деривируется из `lifetimeCtx ← handoff`, а не из `initCtx`. Закрыли `5c8641e7` + `b7d8614f` (вне моего диапазона, chunk 4); в моём диапазоне `922316af4` укрепил цепочку, сняв временный костыль `sessionCtx = o.lifecycleCtx` в renewal. Трассировка проверена по живому файлу, не по commit message |
| CH5-1 | **P0** | `f2914d53c` | `detachSessionLocked` (`init.go:3877-3886`) делает `delete(owner.committedAdmissions, name)` без `lifecycleMu`; шесть вызовов (`:2474`, `:2599`, `:2627`, `:3757`, `:3798`, `:3869`) держат только server lease → concurrent map write с `publishPreparedClientLocked:2430` / `commitRenewalForLease:1735` для другого имени → `fatal error: concurrent map writes`, нерекаверимое падение процесса | Открыт |
| CH5-3 | **P1** | `779802b20` | `candidateValidLocked` (`init.go:850-856`) фенсит кандидата по `resolverRevision`, а `store_reload.go:512` инкрементирует его на каждом reload'е → любой reload (в т.ч. из `checkLivePeakHours`, `coordinator_providers.go:745`) обрывает поднимающийся MCP-сервер без ретрая | Открыт |
| CH5-2 | P2 | `3c3b7dde0` | В `getOrRenewClient` проверка «сессии нет» (`init.go:4141`) переехала выше ожидания `renewDone` (`:4149`) → конкурентные вызовы во время renewal получают `mcp '…' not available`; `:4163-4169` стал мёртвым кодом | Открыт |
| CH5-4 | P2 | `59d1d8d9d` | Недостижимые `lease.Unlock()`/`return ErrOwnerBusy` (`init.go:2782-2783`) после if/else, обе ветки которого терминальны; вероятная красная проверка `govet`/`unreachable` в `golangci-lint` CI | Открыт |
| CH5-6 | P2 | серия (design) | Uncertainty-fence снимается только полным успешным `ReloadFromDisk`; тот падает по не связанным с MCP причинам (провайдеры, hooks, зависшая shell-подстановка) → сервер остаётся отключённым до рестарта, без внятного лога | Открыт, design note |
| CH5-5 | P3 | `79215f9d5`, `28c14030c` | Утечка `*os.File` в `verifyWindowsRenameRetryState` (`config_rename_windows.go:173-179`) на пути `!replace && err == nil` | Открыт |
| CH5-7 | P3 | `29f44e2f2` | Durability-uncertain commit классифицируется как `Committed && Reconciled` (не фенсится), но ошибка всё равно возвращается наверх → успешный `EnableServer` рапортует ошибку (`init.go:2839-2841`) | Открыт, note |
| CH5-8 | P3 | `a62a37e60`, `49c92cb19` | `startFallback` вызывается до `retireMCPClient` старой сессии (`init.go:3811-3813`, `:3236-3244`) → старый stdio-процесс жив, пока fallback спаунит новый | Открыт, note |
| CH5-9 | P3 | серия | 43 из 43 коммитов с пустым телом при +2560/−639 в `init.go`; мотивация каждого шага fencing'а нигде не зафиксирована | Открыт, process note |

---

## Итог

**Блокирует:** CH5-1 (`f2914d53c` — незасинхронизированная запись в
`Owner.committedAdmissions`, `fatal error: concurrent map writes`, падение
процесса целиком) и CH5-3 (`779802b20` — `resolverRevision` в
`candidateValidLocked` обрывает инициализацию MCP при любом reload'е
конфига, без ретрая). Обе находки — в двух самых поздних содержательных
коммитах диапазона; обе в коде, который правился в этом же окне ради
надёжности lifecycle'а, и обе не покрыты новыми тестами.

**Подождёт:** CH5-2 (спорадические `mcp '…' not available` во время
renewal), CH5-4 (мёртвый код, вероятно красный `golangci-lint`), CH5-6
(fail-closed uncertainty без диагностики), CH5-5/CH5-7/CH5-8/CH5-9.

**CR-1 закрыт** — проверено трассировкой `Owner.Initialize` →
`admitServerForConfig` → `initClientAdmitted` → `prepareClient` →
`createSessionWithAdmission` → `sessionContext`/`promote` →
`createTransport` в текущем файле на `f2914d53c`, а не по формулировкам
commit message'ей. Опубликованная сессия переживает возврат из
`Initialize`, отмену `admission.ctx` и закрытие operation lease; респауна на
первом tool call нет.

Остальные 38 коммитов диапазона (Windows-durability тред целиком, unix
commit outcomes, alias recovery, линеаризация событий, codex-skill
валидация, тестовая изоляция) корректны, тесты по большей части
невакуумны, новых утечек не добавляют. Два тематических треда — MCP
lifecycle fencing и Windows config durability — стыкуются в одной явной
точке (`writeMCPFileChanges` → `commitConfigFile` → `CommitOutcome` →
`fenceMCPRuntimeLocked`), порядок блокировок между ними односторонний
(`server lease → publishMu → diskWriteMu → sidecar → lifecycleMu`),
обратного ребра нет; композиционных дедлоков не нашёл. Единственный
композиционный дефект — CH5-6: fail-closed fence снимается только полностью
успешным reload'ом, который может не проходить по причинам, не имеющим
отношения к MCP.
