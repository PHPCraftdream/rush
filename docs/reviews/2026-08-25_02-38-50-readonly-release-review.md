# Read-only release review — 2026-08-25 02:38:50 (Europe/Berlin)

## Итог

Релиз пока **NO-GO**. Основные гонки из предыдущего ревью действительно
закрыты, но остались два блокера, которые могут проявляться как «зависание»
сессии/интерфейса, и один функциональный блокер релизного npm/deploy-пути.
Классического цикла взаимных блокировок mutex в просмотренном коде не найдено;
проблемы ниже — это потерянные сообщения, ожидание ответа без тайм-аута и
гонки завершения.

Проверялся HEAD `7a34c972` (2026-08-25 02:26:39+02:00), последние изменения
за 23–25 августа и связанный код. Исследование статическое: тесты, сборка,
lint и запуск приложения **не выполнялись**.

## Findings

### P1 — deploy ищет старое имя platform npm-пакета

`internal/deploy/deploy.go:450-452` строит путь через
`@phpcraftdream/crush-%s-%s`, хотя новый npm wrapper и
`npm/rush/package.json` используют `@phpcraftdream/rush-%s-%s`.
`deploy.go` вызывает эту функцию при обновлении установленного бинарника.

После установки нового `@phpcraftdream/rush` updater не находит реальный
platform binary и завершает `resolveDests()` ошибкой «no replaceable binary».
Это регрессия, оставшаяся после `1a89e2a3` и `94cbe6b1`; тесты
`internal/deploy/deploy_test.go:506-532` закрепляют тот же устаревший префикс.

**Исправить:** заменить `crush-` на `rush-` в helper, тестах и комментариях
root `deploy.go`; добавить проверку соответствия npm wrapper/package/deploy
пути. Старый `npm/crush` пакет сам по себе не ошибка: он оставлен для
совместимости и должен быть отдельной задачей deprecation.

### P1/P2 — UI может навсегда остаться busy после разрыва WebSocket

`web/src/ws.ts:91-104` правильно возвращает `false`, если кадр не отправлен,
но многие владельцы пользовательского состояния это значение игнорируют и
ждут только reply.

Самый явный случай — `web/src/components/ProvidersModal.tsx:418-457`:
`sendAndWait()` регистрирует listener, вызывает `ws.send()` и не имеет ни
обработки `false`, ни `_disconnected`, ни тайм-аута. `submit()` ждёт
`Promise.all`, поэтому при отключении `busy` остаётся `true` навсегда.
Аналогичный риск есть у:

- сохранения system prompt (`ChatToolbar.tsx:123-142`);
- initialize project (`SettingsModal.tsx:168-189`);
- загрузки логов (`LogsModal.tsx:21-42`);
- обновления skills (`SettingsModal.tsx:80-108`).

Исправленный fetch system prompt уже показывает правильный шаблон —
корреляция, тайм-аут и обработка disconnect — но save-путь его не использует.

**Исправить:** вынести единый request helper с корреляцией, bounded timeout,
отпиской и немедленным reject при `send() == false`/`_disconnected`; на ошибке
сбрасывать busy/loading и сохранять пользовательский draft.

### P2 — queued-сообщение может потеряться при reconnect/send race

В `web/src/useWS.ts:320-340` обработчик `agent_busy=false` сначала вызывает
`dequeueAllMessages()`, а затем делает обычный `ws.send("send_message", ...)`.
Если сокет закрывается между этими операциями, очередь уже удалена, а
`send()` возвращает `false`; сообщение теряется.

Кроме того, `_connected` (`useWS.ts:66-91`) очищает `$busySessions`. Если
disconnect поглотил последний `agent_busy=false`, локальная очередь может
остаться без события, которое её когда-либо flush-ит. Очередь находится только
в памяти вкладки (`web/src/store.ts:790-835`), поэтому перезагрузка её также
теряет.

**Исправить:** отправлять через `sendQueued` либо возвращать элемент в очередь
при неуспешной отправке; на reconnect явно синхронизировать busy/queue state и
иметь понятную политику для `interrupt_and_send`/`inject_message`, которые
могут стать неуместными после восстановления соединения.

### P2 — алиасы PowerShell обходят window-safety guard

После `90be93f6` built-in Bash и MCP Bash вызывают guard, а обычные Windows
процессы создаются через `platform.Command` с `HideWindow`. Но
`internal/agent/agentguard/agentguard.go:125-137,346-359` распознаёт только
`start`, `start-process` и `start-job`. PowerShell-алиасы `saps`
(`Start-Process`) и `sajb` (`Start-Job`) отсутствуют.

Команда вроде `powershell -Command "saps notepad"` может пройти текстовую
проверку и создать видимое окно/процесс. Это напрямую соответствует
наблюдаемому дефекту «иногда открывается окно»; защита является best-effort
парсером и также не может гарантировать покрытие произвольной косвенной
экзекуции.

**Исправить:** добавить canonical aliases в `windowOpenerVerbs` и
`commandWrappers`, рекурсивно проверять их так же, как полные cmdlet-имена,
и добавить регрессионные cases для вложенных PowerShell-команд.

### P2 — гонка регистрации WebSocket во время shutdown

`internal/server/server.go:216-231` защищает отправку в buffered `hub.register`
через `select { case register <- c; case <-ctx.Done(): ... }`. Если shutdown
уже начался и канал всё ещё имеет место, обе ветки готовы, а Go выбирает
случайную. Регистрация может выиграть после завершения `Hub.Run`; тогда клиент
попадает в канал, который больше никто не читает, а его pumps остаются без
нормального закрытия до конца процесса.

Это не постоянный lock cycle, но реальная teardown-race, способная оставить
подключение и goroutine в подвешенном состоянии под supervisor-ом.

**Исправить:** сделать lifecycle hub явным (stop gate/close signal),
проверять shutdown до и после регистрации и гарантировать закрытие клиента,
если hub уже остановлен. Одной случайной `select`-защиты недостаточно.

## P3 и code smells

- `web/src/store.ts:149-155` при `removeSession` очищает только список сессий и
  request bookkeeping. `$subAgentSessions`, `$subAgentMessages`,
  `$messageBlockBreaks` и `deletedMessageIDs` не очищаются. При массовом
  удалении delegated sessions память и tombstones растут в течение жизни
  вкладки, а поздние события могут маршрутизироваться в удалённое состояние.
- `mergeMessageLists` всё ещё является эвристическим merge snapshot + live tail.
  При потерянном delete push теоретически возможна реанимация удалённого
  сообщения; надёжнее использовать server sequence/watermark.
- В `npm/rush/package.json` homepage/repository/bugs всё ещё указывают
  `PHPCraftdream/crush`; это не runtime-блокер, но опубликованный пакет ведёт
  пользователей на неверные ссылки. `npm/platform/package.json.tmpl` также
  содержит upstream `crush` metadata. Коммит `1a89e2a3` явно оставил это как
  follow-up — перед стабильным релизом нужно либо завершить rename, либо
  документировать намеренно отложенную часть.
- В `_connected` (`useWS.ts:66-91`) несколько `ws.send()` игнорируют boolean
  результата. Для housekeeping это менее критично, но общий scatter-shot API
  облегчает появление новых silent drops.

## Что уже выглядит исправленным

Проверены и не повторяют прежние findings: корреляция и merge snapshots
`messages_list`/`sessions_list` (`4d9775a1`, `5a0468e8`), stale closure в
sidebar (`80b49e21`), attachment-only и offline send paths (`aa2821e3`,
`eff307b8`), correlated system-prompt fetch (`b91e8285`), очистка некоторых
web request maps (`785cc67d`), recovery дочерних sessions (`f3a2cb03`),
per-field model update (`4ba5b74e`) и подключение window guard к built-in Bash
(`90be93f6`).

В agent/server loop не обнаружен очевидный бесконечный mutex cycle. Recovery
имеет deadline и bounded lock-liveness check; hub использует bounded queues и
worker pools. Это хорошие свойства, но они не устраняют перечисленные
offline/shutdown races.

## Рекомендованный порядок перед release

1. Исправить `rush-` deploy path и связанные проверки/документацию.
2. Ввести единый bounded WebSocket request API и перевести все UI actions,
   меняющие busy/loading state, на него.
3. Сделать flush очереди атомарным относительно disconnect/reconnect и
   определить политику устаревших queued actions.
4. Добавить `saps`/`sajb` и покрытие вложенных PowerShell aliases в guard.
5. Устранить shutdown registration race и очистку sub-agent store state.
6. Перед публикацией обновить npm metadata и отдельно проверить legacy
   compatibility paths.

Рабочее дерево до и после ревью содержало существующее пользовательское
удаление `web/dist/.gitkeep`; файл не изменялся.
