# Rush: независимый release-readiness аудит, round 2

Дата: **2026-09-21**. Проверенная ревизия:
`fd18cf0c38394faf4f776f45c4d998efc9365218`.

## Краткий summary

**Вердикт: NO-GO для общей alpha с текущими CLI, SDK, web и network-возможностями.**

Первый отчёт проверял `f7362f7f`, а этот проход включает также исправления
`babd295c` и `fd18cf0c`. Нельзя переносить его пять P1 на новый HEAD без проверки:
F1 и F2 исправлены, прежний механизм неограниченного удержания idle-соединений F5
устранён; F3 и F4 закрыты лишь частично. F6–F9 остаются актуальными.

Второй проход установил **шесть новых находок/уточнённых механизмов**, из них
три P1. Вместе с четырьмя оставшимися F6–F9 это **три P1 и семь P2**, без двойного
счёта F3/F4. P0 не установлен.

| ID | Severity | Актуальная проблема |
| --- | --- | --- |
| R2-1 | P1 | SOCKS5 handshake остаётся без собственного срока; отменённые HTTP/DoH-запросы могут оставлять незавершённые dial и sockets |
| R2-2 | P1 | Исправление continuation всё ещё теряет начало при промежуточном tool-step и меняет байты структурированного ответа |
| R2-3 | P1 | Новая reviewer-фаза теряет SDK `FailIfSessionBusy` и может исполняться в очереди уже после возврата вызывающему |
| R2-4 | P2 | Переход в reviewer после отмены сохраняет terminal cache предыдущей фазы и уже отменённый cleanup context |
| R2-5 | P2 | Network error paths включают proxy/DoH credentials в текст ошибок |
| R2-6 | P2 | Новый тест cache eviction зависит от отсутствия параллельных вставок, хотя сам запускается параллельно |
| F6 | P2 | Provider и global network settings берутся из разных config snapshots |
| F7 | P2 | `IdleTimeout` теряется в durable round-trip |
| F8 | P2 | Reviewer-envelope теряет tool counts и предупреждение основной фазы |
| F9 | P2 | JSON `null` в tool input выбрасывает исключение при web render |

P1 здесь — потеря результата, нарушение обещанного жизненного цикла выполнения
либо неограниченное удержание ресурсов в поддерживаемом сценарии. P2 — более
узкий сценарий, для которого возможен явно ограниченный alpha-профиль.
Наличие P2 не означает согласия молча выпускать соответствующую возможность.

Нового доказанного циклического mutex/channel deadlock в просмотренной серии
не найдено. R2-1 — зависание сетевого I/O, а R2-3/R2-4/F6 — ошибки владения и
состояния; называть их установленными гонками памяти Go было бы неточно.
Race detector в этом проходе не запускался.

## Срез, метод и сохранность рабочей копии

- Период: **2026-09-14 00:00:00 +02:00 — исходный HEAD 2026-09-21
  21:57:31 +02:00**, `Europe/Berlin`, по committer date среди предков HEAD.
- База до периода: `ccc2f9b56ad6f17a9d703b0e0577a1e61f370d23`.
  Диапазон `ccc2f9b5..fd18cf0c`: **20 коммитов, 62 файла,
  7156 добавленных / 642 удалённых строки**. `--since-as-filter` даёт тот же
  набор коммитов. Собственный documentation commit этого аудита в срез не входит.
- Изучены карта и изменения недельной серии, текущие реализации затронутых
  production-путей, вызывающий код и выбранные тесты. Findings первого отчёта
  проверены по механизмам, а не по названиям исправляющих коммитов.
- Номера строк ниже относятся к **`fd18cf0c`**, если явно не названа другая
  ревизия. Для зависимостей указаны версия и путь внутри модуля/toolchain.
- На входе tracked-файлы и index чисты; untracked-файлов Git не показал.
  Указанные пользователем два теста уже tracked в `97c22050`; их содержимое
  совпадает с хешами, записанными первым аудитом. Они не редактировались.
- Ограничение окружения: предоставленный checkout зарегистрирован Git как
  `main`; `git worktree list --porcelain` не показал отдельного изолированного
  worktree. Новые worktrees и ветки не создавались. Проверка выполнена в
  предоставленной рабочей копии с единственным разрешённым изменением — этим
  отчётом; конфигурация, исходники, версии и CHANGELOG не менялись.

| Сохранённый файл | SHA-256 |
| --- | --- |
| `web/tests/message-timestamp-always-visible.spec.ts` | `CE416416F90632CD972DE8F262785E576BC784888F881F9C9811C5BA0952F891` |
| `web/tests/subagent-block-metadata.spec.ts` | `CB72A0EB814462FB64EA7655DBE0B826905FEE79060ED2135ED57E9F22611678` |

Go-выводы ниже основаны на статической трассировке и конкретных допустимых
последовательностях событий. Предлагаемые Go-сценарии не выдаются за выполненные
тесты. Единственное динамическое воспроизведение — фактический TypeScript
formatter F9, загруженный через Node без изменения файлов.

## Карта проверенной недельной серии

| Коммит | Предмет проверки |
| --- | --- |
| `57fffb05` | Partial output, continuation/retry, новый user turn и публичный результат |
| `14dd4a54` | Общий formatter аргументов, расширение его поверхности на sub-agents |
| `924add8a` | Model/effort в заголовке sub-agent, проход по истории |
| `10e32c56` | Постоянный timestamp, перенос `TimeBadge` |
| `87ee064f` | Локализация env override в Windows MCP-тесте |
| `64409ab3` | Identity сравнение рабочего каталога, bounded MCP test waits |
| `df0051b1` | Checkpoint, production-код не меняет |
| `ccb510ff` | Повторный ExecuteRun phase, reviewer gate, контексты и ownership |
| `0f445020` | Proxy, DNS/DoH, разрешение и каскад NetworkConfig |
| `85ee8836` | Checkpoint, production-код не меняет |
| `eb59df12` | Provider clients, debug/Copilot wrappers, snapshots и ошибки |
| `180f1544` | Context-aware сетевые test doubles |
| `5c20c771` | Context-aware request в provider-network тесте |
| `bf9f6d3d` | Checkpoint, production-код не меняет |
| `f7362f7f` | Timeout defaults, per-call idle watchdog, durable contract |
| `e547e12e` | Первый аудит; использован как набор проверяемых утверждений |
| `dd03ecf0` | Checkpoint, production-код не меняет |
| `97c22050` | Добавлены два web-теста; прежние пользовательские файлы сохранены |
| `babd295c` | Исправления F1/F2, новая сборка continuation text и её границы |
| `fd18cf0c` | Handshake guard, transport cache, cleanup forwarding и новые тесты |

## Перепроверка первого отчёта

Первый отчёт: [2026-09-21-weekly-commit-audit.md](2026-09-21-weekly-commit-audit.md).
«Исправлено» ниже означает, что описанный механизм устранён по текущему коду;
это не утверждение о независимо выполненном зелёном acceptance run.

| Finding | Результат независимой проверки |
| --- | --- |
| F1, P1 | **Исторически подтверждён, исправлен `babd295c`.** `resetForReviewerPass:599` теперь присваивает `s.ctx = ctx`; запуск `runTurnPhase:109` получает reviewer options и очищенный reservation token. Старый stale-token сценарий с explicit override больше не следует из этого кода. Новые неполнота policy и cleanup state описаны отдельно в R2-3/R2-4. |
| F2, P1 | **Исторически подтверждён, исправлен `babd295c`.** `app_run.go:513` требует `req.Credentials == nil`: credentialed run вообще не проходит auto-review gate. Это закрывает отправку tenant transcript configured reviewer независимо от наличия reviewer slot у tenant. |
| F3, P1 | **Исторически подтверждён, частично исправлен `babd295c`.** Два соседних assistant-фрагмента `error → end_turn` теперь попадают в `combinedText`. Полный контракт результата не восстановлен: R2-2. |
| F4, P1 | **Исторически подтверждён, частично исправлен `fd18cf0c`.** Custom CONNECT и DNS/TCP I/O ограничены guard, DoH HTTP exchange — client timeout, TLS — 10s. Но SOCKS5 dial и вложенный DoH→SOCKS5 handshake остались без собственного срока: R2-1. |
| F5, P1 | **Исторически подтверждён; прежний P1-механизм idle-pool роста устранён `fd18cf0c`.** Main/DoH transports имеют idle timeout 90s, обычные builds переиспользуют cache, eviction закрывает оба pool, wrappers передают CloseIdleConnections. Это не равнозначно полному cleanup каждого App/Client; остаточные ограничения перечислены ниже. |
| F6, P2 | **Подтверждён на HEAD.** `resolveProviderHTTPClient:18` заново читает live config; pinned snapshot не передаётся. |
| F7, P2 | **Подтверждён на HEAD.** Поля нет в `session.CallOptionsSpec` и обоих converters. |
| F8, P2 | **Подтверждён на HEAD.** Counts обнуляются при новом phase, baseline исключает всю основную фазу. |
| F9, P2 | **Подтверждён на HEAD динамически.** `formatActionArgs("bash", "null")` всё ещё бросает TypeError. Добавленный web-тест проверяет нормальные object arguments, не null. |

**Полностью ложных исторических F1–F9 не установлено.** Опровергается их
безусловный перенос на новый HEAD: объявлять F1/F2/F5 всё ещё открытыми P1 было
бы false positive. Также неверно считать названия двух fix-коммитов
доказательством полного закрытия F3/F4.

### Что действительно закрыто и где остаётся ограничение доказательств

- F1: `ClearReservedOwnership` затеняет значение typed nil; atomic claim и
  epoch/state guard не ослаблены. Смена контекста в обычном последовательном
  переходе фаз не создаёт сама по себе Go data race. Новый
  `TestExecuteRunReviewerPassTurnUsesReviewerCallOptions` проверяет worker и
  наличие/отсутствие edit/delegation tools. Отдельного сценария fail-fast +
  explicit override + межфазный конкурент в этом тесте нет.
- F2: `TestExecuteRunReviewerPassSkippedForCredentialedRun` использует отдельные
  tenant/configured stubs и проверяет нулевой список запросов второго
  (`app_run_reviewer_pass_credentials_test.go:150–166`). Проверка содержательная,
  но в этом аудите не исполнялась. Auto-review для credentialed runs отключён
  целиком — reviewer через tenant credentials этим изменением не реализован.
- F4: `handshakeGuard.stop` закрывает stop-channel, ждёт watcher и снимает
  deadline до передачи tunnel; error paths custom CONNECT закрывают connection.
  Это устраняет первоначальный blocking Read/Write без срока именно на этих
  путях. `TestDNSTCPQueryRespectsContextCancellation` использует direct dial,
  а DoH lifecycle-тесты доходят до HTTP handler; молчащий SOCKS handshake они
  не проверяют.
- F5: `transportCache.put` закрывает victim **после** unlock; нового цикла
  cache mutex → HTTP internals → cache mutex не найдено. Но get/build/put не
  атомарны: два одновременных miss возвращают разные transports, loser не
  становится cache entry. Idle timeout всё же ограничивает их idle lifetime;
  это не доказательство прежней бесконечной утечки. `CloseIdleConnections`
  внешнего клиента по-прежнему не доходит до resolver closure; DoH pool
  закрывается при eviction или по idle timeout. Поэтому «полный lifecycle
  всех клиентов доказан» тоже было бы завышенным утверждением. Отдельным P1
  это не считается; незавершённый dial из R2-1 имеет другой механизм.

## Новые findings и остаточные механизмы

### R2-1. P1 — SOCKS5 handshake обходит новый bounded cleanup

**Коммит:** введено `0f445020`, подключено `eb59df12`; не закрыто `fd18cf0c`.
**Место:** `internal/nettransport/proxy.go:45–64`, `socksDialer`;
`internal/nettransport/client.go:103`, proxy-only SOCKS branch;
`internal/nettransport/resolver.go:163–172`, `newDoHResolver`.

**Механизм.** `proxy.SOCKS5(..., proxy.Direct)` получает context, но Rush не
добавляет к нему timeout. В локально проверенном `golang.org/x/net@v0.55.0`
`proxy/direct.go` создаёт нулевой `net.Dialer`, а
`internal/socks/client.go`, `Dialer.connect`, ставит deadline только если он
есть в context. Затем идут blocking `Write`/`io.ReadFull` при выборе auth,
аутентификации и ответе CONNECT.

Это недостаточно для production HTTP: Go 1.26.3
`src/net/http/transport.go:1529`, `getConn`, создаёт dial context через
`context.WithoutCancel`. `wantConn.cancel:1390` отмечает отказ от результата,
но не отменяет уже выполняющийся dial. `CloseIdleConnections:911` может отменить
такой dial, однако завершение каждого run этот метод не вызывает, а один
постоянный network cache key вообще не обязан вытесняться.

Отдельно DoH использует `Transport.Proxy`. При SOCKS5 стандартный transport
в `transport.go:1835–1849` делает `DialWithConn` под своим detached dial context.
Новые 10s `http.Client.Timeout` ограничивают возврат DoH `client.Do`, но не
ставят срок этому вложенному handshake. `TLSHandshakeTimeout` включается
позже, а `IdleConnTimeout` относится к уже установленным idle connections.

**Сценарий воспроизведения, статически подтверждённый, не запускался.**
Локальный SOCKS5 peer принимает TCP и greeting, сигнализирует барьер и молчит.
Выполнить запрос через `BuildHTTPClient` с SOCKS5 proxy, после барьера отменить
HTTP request. Внешний запрос возвращает cancel, но до закрытия peer/transport
handshake остаётся в `io.ReadFull`. Повторить отдельно с `DoHURL` и SOCKS5 proxy:
resolver вернёт timeout, а сокет вложенного dial останется. Проверять именно
завершение dial и peer EOF, не только возврат HTTP-запроса. Для теста нужен
независимый cleanup peer, чтобы регрессия не оставляла висящий тест.

**Риск:** число удерживаемых sockets/goroutines растёт как O(K) по числу
незавершённых попыток при неизменной конфигурации; idle timeout это не ограничит.
Особенно значимо для долгоживущих web/SDK процессов. Это не mutex deadlock.

**Рекомендация:** собственный ограниченный бюджет на весь SOCKS dial/auth/
CONNECT, включая DoH transport; не полагаться только на переданный HTTP context.
Проверить безопасную передачу готового tunnel, peer EOF после cancel/timeout и
оба production-пути с молчащим SOCKS stub. Текущие direct CONNECT/DoH tests
этот acceptance gate не заменяют.

### R2-2. P1 — continuation восстановлен только для соседних абзацев

**Коммиты:** `57fffb05` — continuation; `babd295c` — неполное исправление.
**Место:** `internal/app/app_run_terminal.go:98`, `continuationChainText`,
особенно `:116`, `:119`, `:127`, `:130`; использование
`internal/app/app_run_reviewer.go:389`. Создание отдельного assistant на каждом
step: `internal/agent/agent_turn_step.go:156`; `tool_use` finish: `:247`.

**Механизм A: потеря начала после инструмента.** Обратный проход принимает
только непосредственно предшествующие assistant rows с `FinishReasonError`.
Продолжение полноценного agent turn вправе сначала вызвать инструмент. Тогда
history имеет вид:

```text
a1: error, частичный ответ "Раздел 1"
u2: continuation prompt
a2: tool_use, например view
t2: tool result
a3: end_turn, продолжение "Раздел 2"
```

После фильтрации user/tool rows остаются `a1,a2,a3`; проход упирается в `a2`,
возвращает пустой `combinedText`, а successful `final_text` снова содержит только
«Раздел 2». Утверждение комментария, что clean tool-loop step обязательно
отделяет независимую цепочку, здесь неверно.

**Механизм B: изменение самих данных.** Даже без tools функция применяет
`TrimSpace` к каждому фрагменту и вставляет `"\n\n"`. При прерывании внутри
JSON-строки partial `{"text":"hello` и tail ` world"}` превращаются в строку с
двумя буквальными переводами строки внутри JSON string вместо
`{"text":"hello world"}`. Аналогично портятся слова, отступы кода и Markdown.
`--format json` может получить `invalid_json` при корректном смысловом
продолжении модели; без валидации возвращаются изменённые данные.

**Доказательство/сценарий.** Обе последовательности прямо подставляются в
условия `continuationChainText`; Go-исполнения в этом проходе не было.
Новые `TestExecuteRunContinuationRetryReturnsFullText` и
`TestContinuationChainText` проверяют самостоятельные абзацы и именно join с
пустой строкой между ними. Tool-step внутри продолжения и обрыв внутри JSON
string/слова не покрыты. Это остаток F3, а не второй независимый подсчёт F3.

**Рекомендация:** явно связать attempts с логическим результатом; отделить
tool-step narrative от частей возвращаемого документа. Задать точный контракт:
либо сохраняемые фрагменты без произвольной нормализации, либо самостоятельная
полная финальная версия после recovery. Не склеивать всю историю по признаку
соседства. Acceptance: возвращённый envelope, несколько attempts, tools между
фрагментами, разрыв внутри JSON string/слова/отступа и reviewer phase boundary.

### R2-3. P1 — reviewer теряет fail-fast policy SDK

**Коммиты:** неполный набор reviewer options добавлен `ccb510ff`;
`babd295c`, начав использовать `reviewCtx`, делает потерю поля действующей.
**Место:** `internal/app/app_run_reviewer.go:654`, `buildReviewerPassTurn`;
`internal/agent/coordinator_run.go:285`; `internal/agent/mailbox_ownership.go:69`;
публичный контракт `sdk/sdk.go:525–528`, установка флага `:562`.

**Механизм.** SDK принудительно ставит `FailIfSessionBusy=true`, но новая
структура `reviewCallOpts` его не копирует. Reserved ownership основной фазы
освобождается до второй фазы и корректно очищается в её context. Если другой
вызов успел занять ту же сессию, reviewer делает обычный `mailbox.submit` с
false: попадает в `mb.submitted`, а `sessionAgent.Run` возвращает `(nil,nil)`.
`runAgentTurnRecovered` превращает это в queued result, и ExecuteRun завершается
с `ErrRunQueued` вместо обещанного отказа без постановки работы в очередь.

**Конкретный interleaving, не запускался.** A — SDK Run с role smart и reviewer.
После завершения primary A, до reviewer admission, B занимает ту же сессию и
остаётся активным. A отправляет reviewer call, получает queued и возвращается.
Затем B завершает свой turn и его dispatcher исполняет reviewer A, используя
жизненный цикл B (`agent_run.go`, `runOwned`/queue-drain). Отмена context A после
возврата не удаляет уже записанный `SessionAgentCall` из очереди B. Reviewer
имеет write/bash tools, поэтому это может быть исполнение с побочными эффектами
после того, как SDK-клиент уже получил неуспех и начал обработку ошибки.

Это не утверждение о нарушении межпользовательской авторизации сессий: SDK
явно возлагает авторизацию session ID на host. Нарушен собственный fail-fast
контракт и lifetime вызова в одной законно используемой сессии.

**Рекомендация:** сохранить `primary.FailIfSessionBusy` во второй фазе и
атомарно применять его при admission. Межфазный конкурент должен приводить к
определённому busy outcome без отложенного reviewer. Если нужен единый owner на
весь primary+review, оформить это отдельным корректным ownership handoff,
не возвращать старый epoch. Acceptance с барьером A-primary-done/B-owned обязан
проверять и ошибку A, и отсутствие reviewer call в последующем drain B.

### R2-4. P2 — terminal cache переживает отмену и смену фазы

**Коммит:** `ccb510ff`; `babd295c` исправил context assignment, но не этот state.
**Место:** `internal/app/app_run_reviewer.go:209–216`, `:356–367`, `:394–396`,
`resetForReviewerPass:598`; gate `internal/app/app_run.go:513`.

**Механизм.** При гонке committed primary finish с отменой `runTurnPhase`
может выбрать ctx.Done, положить primary в `cachedTerminal` и вызвать `finish`.
Если primary успешен, `finish` снимает cancellation error; его defer отменяет
`cachedTerminalCtx`. Gate после этого смотрит только resultErr/credentials/
role/config, не `ctx.Err`, и запускает review. Reset не очищает
`cachedTerminal`, `cachedTerminalCtx`, `cachedTerminalCancel`.

**Сценарий.** Зафиксировать successful primary row, отменить parent и дать
event loop обработать cancellation до done. Первое `finish` возвращает успех;
review начинает работать с отменённым parent. Его baseline read не удаётся.
Если его cancellation result уже лежит в done, второе `finish` берёт cached
primary как якобы authoritative terminal второй фазы и снова подавляет cancel;
usage reads выполняются с уже отменённым cleanup context. При другом порядке
select возвращается ошибка отмены. Получаются разные outcomes на одном и том
же committed primary, включая primary result на месте ожидаемого reviewer.
Это статический сценарий; принудительный interleaving в тесте не запускался.

**Рекомендация:** не входить в новую фазу после parent cancel; сохранить
определённый outcome уже завершённой primary. Всегда сбрасывать terminal cache
и его cleanup handles при разрешённом переходе. Acceptance должен отдельно
покрывать cancel после commit, до done, и явную проверку phase identity cached
message. Только присваивания нового `s.ctx` недостаточно.

### R2-5. P2 — network credentials попадают в ошибки

**Коммиты:** `0f445020`, production exposure `eb59df12`.
**Место:** `internal/nettransport/config.go:79`, `:91–95`, `:103`,
`resolveConfig`; `internal/nettransport/resolver.go:210–231`, `dohQuery`;
wrapper `internal/agent/coordinator_providers_network.go:22`.

**Механизм и доказательство.** Ошибки interpolируют исходный proxy/DoH URL,
включая userinfo и query, без redaction. Например, для фиктивного значения
`https://demo:SECRET@proxy.invalid:8443` unsupported-scheme branch возвращает
ошибку с `demo:SECRET`. Это обычная ошибка выбора неподдерживаемой HTTPS proxy
схемы, а не необходимость включить debug body logging. Для корректного
`https://demo:SECRET@resolver.invalid/dns-query?token=TOKEN` даже HTTP 503 от
DoH endpoint возвращает полный endpoint из `resolver.go:215`.

Provider build error передаётся вверх как `%w`; runtime network errors могут
попасть в finish details через `agent_turn_failure.go:231` (`err.Error()`) и
далее в transcript/envelope. HTTP header redaction не очищает строки ошибок.
Примеры используют только вымышленные маркеры, реальные credentials не читались
и не отправлялись. Проверка статическая; запроса к `.invalid` не выполнялось.

**Рекомендация:** безопасное представление endpoint в диагностике: исключить
пароль/userinfo и секретные query values; не включать сырой input при parse
failure. Проверять также вложенные `url.Error` при wrapping — redaction только
наружного `%s` недостаточна. Сохранить полезную классификацию ошибки без секрета.
Acceptance: fake secret не встречается в build/runtime errors, logs и finish
details на malformed URL, unsupported scheme, timeout и non-200 DoH.

### R2-6. P2 — новый cache-тест имеет недопустимое предположение о параллелизме

**Коммит:** `fd18cf0c`.
**Место:** `internal/nettransport/lifecycle_test.go:451–490`,
`TestTransportCacheEvictionReleasesIdleConns`; cache capacity/eviction в
`transport_cache.go:12`, `:70`. CI: `.github/workflows/build.yml:190`.

**Механизм.** Тест вызывает `t.Parallel`, пользуется process-global cache на
восемь entries и ожидает ровно один TCP accept после двух warm-up requests.
Комментарий требует `-p 1 -parallel 1`, но catch-all CI запускает nettransport
с `-p 2` и без ограничения `-parallel`; ограничение параллелизма отдельных
agent-тестов на этот пакет не распространяется.

**Допустимое расписание.** После первого `fetch(client)` приостановить тест;
другие parallel proxy/resolver tests вставляют восемь distinct network configs.
Первый pool вытесняется и правильно закрывает idle connection. После
`fetch(client2)` сервер насчитает два accepts, и `require.Equal(t,1,base)`
ложно объявит поломку reuse. Ещё одно окно — eviction между двумя
`BuildHTTPClient(cfg)`, когда они законно получают разные transports.

Это доказанный конфликт test oracle с разрешённым расписанием, **не сообщение
о наблюдавшемся падении CI**: тесты/повторные stress runs здесь не запускались.
В выбранном расписании production cache действует согласно своему контракту.

**Рекомендация:** исключить конкурирующие вставки в cache на время проверки
либо предоставить тесту собственный cache; отдельно тестировать concurrency
без предположения «entry не может быть вытеснен». Не закреплять случайный зелёный
serial run как доказательство отсутствия flake и не скрывать проблему глобальным
увеличением таймаутов. Gate — целевой nettransport test при обычном in-package
параллелизме, с контролируемыми барьерами для eviction.

## Сохранившиеся findings первого прохода: самостоятельные доказательства

### F6. P2 — network policy из другого поколения config

**Коммит:** `eb59df12`.
**Место:** `internal/agent/coordinator_models.go:780`, `buildModelsFromCfg`;
`coordinator_providers.go:729`; `coordinator_providers_network.go:17–19`,
`resolveProviderHTTPClient`.

ProviderConfig берётся из аргумента `cfg`, захваченного snapshot, а global
`Options.Network` — из нового `c.cfg.Config()`. При reload между этими чтениями
получается provider A с network B; между smart/fast builds возможно смешение
сетевой политики пары. Атомарная публикация snapshots в `config/store.go`
исключает необходимость гонки памяти, но не этот torn logical read.

**Сценарий:** остановить build после выбора provider из snapshot A, опубликовать
B с другим global proxy, продолжить. Построенный из A provider пойдёт через B.
Прочитанные network tests проверяют merge на стабильном store, этого барьера
нет; сценарий не исполнялся.

**Рекомендация:** передавать pinned global options/resolved network через весь
provider-build путь; проверить reload между build smart и fast. До исправления
допустимо только явное ограничение смены network policy перезапуском.

### F7. P2 — durable replay теряет idle policy

**Коммит:** `f7362f7f`.
**Место:** `internal/agent/call_options.go:69`, `IdleTimeout`, и `:263`,
`effectiveIdleTimeoutForCall`; converters
`internal/agent/call_data_conversion.go:90`, `:126`;
`internal/session/session_runqueue.go:138`, `CallOptionsSpec`.

Оба converter перечисляют timeout fields явно; нового `IdleTimeout` среди них
нет. `CallOptions{IdleTimeout:5*time.Second}` после
`ToSessionAgentCallData → JSON → FromSessionAgentCallData` получает ноль.
Порог станет shared/default; package default — 10 минут, CLI default — 15 минут.
Теряется также positive-value признак terminal idle policy и CLI disabled
sentinel. Потеря значения следует непосредственно из initializer новой
структуры; длинного таймера для доказательства не требуется.

**Рекомендация:** durable field и оба преобразования, совместимость версии
контракта, round-trip для нуля/positive/disabled. В этом аудите round-trip не
запускался и никакая версия не менялась. До исправления нельзя обещать
сохранность per-call idle policy после durable handoff/restart; это не
доказательство, что любой pump обязательно запускает coordinator retry.

### F8. P2 — tool inventory покрывает только reviewer

**Коммит:** `ccb510ff`.
**Место:** `internal/app/app_run_reviewer.go:118–135`, `:382`, `:467`,
`resetForReviewerPass:604`; `internal/app/app_run_terminal.go:51`.

Основная фаза уже посчитана, но её result заменяется результатом reviewer.
Counts сбрасываются, новый baseline содержит всю primary history, и terminal
reconciliation исключает её tool calls. Tokens/cost/runStart при этом сохраняются
на всю invocation. Проверка reduction warning тоже использует только оставшиеся
reviewer counts `agent`/`agentic_fetch`.

**Сценарий:** primary вызывает один `view`, reviewer отвечает только текстом.
Финальный JSON содержит пустой `tool_calls` при стоимости обеих фаз. Если только
primary делегировал, его reduction warning может исчезнуть. Отдельный
`aggregation=attach` не доказывает сохранность counts. Сценарий статический;
reviewer stubs в прочитанных acceptance-тестах не вызывают tools.

**Рекомендация:** отделить terminal baseline фазы от invocation-wide inventory;
агрегировать по tool-call ID без повторного счёта. До исправления не использовать
`tool_calls` и reduction warnings как полный журнал primary+review; читать
transcript.

### F9. P2 — null input ломает formatter при web render

**Коммит:** `14dd4a54` расширяет старый дефект на sub-agent render;
ошибка не была впервые написана в этом коммите.
**Место:** `web/src/toolFormat.ts:12–13`, `formatActionArgs`;
`web/src/components/SubAgentBlock.tsx:43`;
`internal/agent/agent_prompt.go:555`, `sanitizeToolInput`.

`JSON.parse("null")` успешен; TypeScript cast не превращает null в object.
Обращение `parsed[k]` и fallback `Object.values(parsed)` падают вне try/catch.
Backend принимает `json.Valid("null")`; formatter вызывается в render без
type guard. Поиск ErrorBoundary/componentDidCatch/getDerivedStateFromError
в `web/src` совпадений не дал.

**Выполнено:** Node **v24.12.0**, чтение текущего `toolFormat.ts`,
`node:module.stripTypeScriptTypes` и import через data URL, без записи файлов
и установки зависимостей. Результаты фактической функции:

```text
bash, "null"   -> TypeError: Cannot read properties of null (reading 'command')
custom, "null" -> TypeError: Cannot convert undefined or null to object
bash, "{"      -> ""
bash, "{}"     -> ""
bash, {"command":"pwd"} -> "pwd"
```

Браузерное размонтирование UI не воспроизводилось; доказано исключение из
реального formatter на достижимом значении. Два сохранённых Playwright-теста
не содержат этого null-сценария и не запускались.

**Рекомендация:** runtime guard non-null object после parse, определённое
поведение scalar/array, безопасный preview. Для web alpha нужен тест render с
null и сохранением остального transcript. CLI-only профиль этот дефект не
затрагивает.

## Проверенные области без дополнительных release findings

- **Lock ordering и re-entrancy.** Новый cache mutex защищает только map/clock;
  закрытие victim вынесено за mutex. В обычном primary→review переходе запуск
  не рекурсивен: предыдущий coordinator call уже вернулся, старый reservation
  очищен. Epoch guard и one-shot claim остаются; их ослабление не требуется.
  Это локальная проверка путей, не полная сертификация mailbox/ConfigStore.
- **Channels и panic completion.** `runTurnPhase` использует done/drainDone с
  capacity 1; `runAgentTurnRecovered` отправляет единственный result, включая
  recovered panic. Закрытый message channel превращается в nil, не оставляет
  hot loop на постоянно готовом чтении. В обычном завершении нового
  бесконечного channel wait не установлено. Нельзя переносить этот вывод на
  произвольный пользовательский writer/provider, игнорирующий cancellation.
- **Custom CONNECT handoff.** Guard имеет stop/join и cleanup deadline;
  bufferedConn сохраняет прочитанные после response head bytes. Закрывать
  успешный CONNECT body как обычный HTTP response было бы неверной универсальной
  рекомендацией: соединение уже передаётся владельцу tunnel.
- **Network construction.** Нулевой config возвращает nil client/transport.
  Подозрение на nil dereference `rs.proxyURL.Scheme` опровергнуто порядком
  ветвей: resolver-only проходит первую, empty возвращает раньше. Merge по
  полям и приоритет DoH соответствуют описанному контракту. Default TLS hostname
  verification не заменена проверкой IP; request hostname остаётся источником
  SNI. Полный DNS response-validation и TLS protocol audit не выполнялся.
- **Credential boundary.** F2 закрыт gate; reviewer получает FolderScope и
  DiskProvider основной фазы, контекстные allowlist values наследуются.
  Совместное использование transport с одинаковым network config само по себе
  не доказывает смешения API keys: auth headers принадлежат запросам. Выявленная
  утечка R2-5 — другой путь, через диагностический текст.
- **Idle timeout live path.** Per-call threshold передан в runTurn;
  shouldRetryTurn и shouldContinueTurn проверяют positive IdleTimeout при
  `Stream stalled`. CLI default 15m, disabled sentinel и hard-kill backstop
  не являются сами по себе бесконечным timer loop. Отличать inactivity от
  tool execution нужно по существующему tool-aware watchdog. Durable-пропуск — F7.
- **MCP/process changes.** Недельные правки здесь тестовые: `t.Setenv` заменяет
  package-wide TestMain mutation, identity comparison использует os.SameFile,
  wait helpers сохраняют timer.Stop и конечный срок 15s. Production process-tree
  cancellation на этой неделе не менялось; Windows/Linux descendant cleanup
  заново не сертифицирован.
- **Web timestamps/model/effort.** Новые metadata вычисляются в существующем
  линейном проходе messages/parts. `TimeBadge` не добавляет interval/subscription.
  В этих diff нет установленного нового O(N²) прохода или listener leak.
  Null-render — отдельно подтверждённый F9.
- **Стоимость.** Повторный `Messages.List` в reset и runTurnPhase, чтение history
  в shouldContinue/shouldRetry и новый временный slice assistant rows добавляют
  O(N + S) работы/памяти по истории и её тексту, но без измерений это не основание
  объявлять release-critical performance regression. Бесконечное удержание
  ресурсов R2-1 принципиально отличается от этих конечных линейных затрат.

## Ограничения и выполненные проверки

Выполнены read-only Git history/diff/status и чтение исходников; прочитаны
локальные Go 1.26.3 `net/http` и `golang.org/x/net v0.55.0` для проверки реальных
cancel/deadline semantics. `git diff --check ccc2f9b5 HEAD` прошёл. Formatter F9
исполнен в Node с указанными контрпробами. Хеши двух защищённых тестов проверены.

**Не запускались:** Go build/test/vet/lint, `-race`, полные suites, benchmarks,
stress/fault injection, Playwright/browser E2E, реальные LLM/MCP/proxy запросы,
SDK shutdown/process-tree эксперименты. В частности, R2-6 — статически
доказанное опасное расписание теста, а не якобы наблюдавшийся в этом проходе flake.
Исторические заявления коммитов/checkpoints о зелёных тестах не считаются
результатом этой независимой проверки. Remote state и текущий CI не запрашивались.

Проверка выборочная. За пределами выводов остаются все возможные interleavings,
полный DNS/TLS protocol audit, поведение каждого provider SDK, crash consistency
всей системы и динамическое доказательство отсутствия resource leaks. Код
намеренно не исправлялся: разрешён только documentation artifact.

## Минимальные обязательные исправления и финальный verdict

Безусловные блокеры общей alpha:

1. **R2-1:** ограничить SOCKS5 handshake на обоих путях — provider и вложенный
   DoH; доказать завершение нижних dial/socket после timeout/cancel.
2. **R2-2:** закончить контракт continuation result, включая tools и точность
   возвращаемых структурированных данных; проверить именно финальный envelope.
3. **R2-3:** сохранить fail-fast admission reviewer и исключить его скрытое
   выполнение после возврата SDK-вызова; проверить межфазного конкурента.

Для заявленного полного профиля дополнительно обязательны null-safe web render
**F9** и отсутствие credentials в network errors **R2-5**. При исправлении
reviewer следует закрыть **R2-4** тем же phase-lifecycle gate. **R2-6** необходимо
устранить до использования нового cache-теста как release evidence.

F6/F7/F8 допускают только явно ограниченный первый alpha-профиль: без обещания
горячей смены network policy, без обещания сохранности idle policy после durable
replay и без трактовки reviewer `tool_calls` как полного invocation inventory.
Это условия возможного последующего решения о выпуске, не уже внесённые
ограничения и не разрешение на текущий релиз.

После исправлений достаточно целевых regression/acceptance-проверок указанных
механизмов: bounded local peers, controllable phase/admission barriers, exact
continuation envelope, durable round-trip, двухфазный tool inventory,
secret-redaction и null render. Для конкурентных Go-путей нужен целевой `-race`;
один зелёный serial run не доказывает lifecycle/cancellation guarantees.

**Итог: `fd18cf0c` всё ещё NO-GO для общей alpha.** Исправления после первого
прохода реально закрыли часть проблем, но новых доказательств для GO нет;
R2-1/R2-2/R2-3 оставляют существенные runtime и SDK regressions.

Контроль рабочей копии перед коммитом: единственное изменение — новый
`docs/reviews/2026-09-21-weekly-commit-audit-round2.md`; tracked diff и исходный
index чисты, хеши обеих защищённых test files повторно совпали. Отчёт подготовлен
для отдельного semantic commit `docs: add weekly release readiness audit round 2`.
