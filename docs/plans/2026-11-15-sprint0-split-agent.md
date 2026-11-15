# Разрезка `internal/agent` — дизайн D1 (2026-11-15, задача #74, спринт 0)

- Источник истины спринта: `docs/plans/2026-11-15-sprint0-plan.md` (§3, §6), данные: `docs/plans/2026-11-15-sprint0-modules-research.md` (И§11.2), швы: `docs/plans/2026-10-31-wave3-structure.md` (ниже «wave3 #N»).
- Предмет: 253 рукописных нетестовых файла самого пакета `internal/agent` (54 776 строк; замер python-подсчётом строк) и 571 его тестовый файл. Существующие подпакеты (`agentguard`, `cliprovider`, `codexprovider`, `hyper`, `limitwords`, `notify`, `prompt`, `tools`, `agentresult`) переезжает этап C — здесь не проектируются, только учитываются как соседи.
- Путь всех новых пакетов начинается с `internal/agent/runtime/`; имена пакетов — новые, уникальные в репозитории. Дизайн выполняется ПОСЛЕ этапа C (пути существующих подпакетов в дереве ниже — пост-C).
- Скрипты-приближения: `prereq.py`, `entangle.py`, `analyze.py` (данные приведены дословно); все ключевые факты проверены чтением кода, ссылки вида `файл:символ`.

## 1. Текущее состояние

### 1.1. Объём и кластеры

`internal/agent` (без подпакетов): 253 нетестовых файла, 54 776 строк; тестовых файлов 571 (569 из них — `package agent`, белые). Замер: python-подсчёт строк `wc -l`; число файлов — `ls`.

Кластеры ≥ 300 строк кода (`analyze.py clusters internal/agent 60`; «файлы» — всего, «код»/«тест» — строки, «c90» — коммиты за 90 дней):

| кластер | файлов | код | тест | c90 | | кластер | файлов | код | тест | c90 |
|---|---|---|---|---|---|---|---|---|---|---|
| batch | 174 | 12491 | 28461 | 125 | | stream | 6 | 569 | 1443 | 21 |
| coordinator | 143 | 12822 | 19496 | 298 | | supervision | 6 | 648 | 1198 | 30 |
| agent | 64 | 11234 | 6735 | 194 | | oauth | 9 | 247 | 1386 | 4 |
| finalization | 48 | 2344 | 4501 | 23 | | queued | 5 | 269 | 1361 | 2 |
| turn | 22 | 1842 | 3420 | 35 | | custom | 9 | 222 | 1385 | 14 |
| mailbox | 12 | 1759 | 2562 | 23 | | codex | 5 | 79 | 1517 | 8 |
| disk | 10 | 1057 | 452 | 10 | | delegation | 19 | 660 | 920 | 14 |
| call | 5 | 955 | 446 | 35 | | cli | 8 | 409 | 801 | 15 |
| credentials | 1 | 710 | 0 | 21 | | usage | 6 | 453 | 638 | 8 |
| drain | 29 | 636 | 6657 | 61 | | cache | 8 | 306 | 732 | 10 |
| public | 6 | 536 | 1491 | 6 | | step | 5 | 393 | 609 | 5 |
| agentic | 3 | 407 | 61 | 17 | | quota | 6 | 369 | 580 | 6 |
| heartbeat | 8 | 374 | 1773 | 10 | | | | | | |

Всего кластеров-префиксов 124; тесты почти всех сценариев (`run_*`, `p0_*`, `interrupt_*`, `release_*`, …) лежат в корне пакета без своего кодового кластера (см. §2.5).

Файлы длиннее 900 строк (7 штук, резать в D2, В7): `coordinator_run.go` 998, `agent.go` 966, `coordinator_interrupt.go` 958, `batch_install_adapters.go` 958, `coordinator_providers.go` 957, `agent_turn_step.go` 932, `agent_run.go` 918.

### 1.2. Цикл предложенных подпакетов (prereq.py, дословно)

`cycles: batch <-> core <-> finalization <-> heartbeat <-> loop <-> mailbox <-> stream <-> usage` — все девять в одном цикле. Вес рёбер (идентификаторы):

| ребро | ids | примеры |
|---|---|---|
| batch → core | 125 | AgentToolName, AttemptFacts, AwaitingAnswerError, CLIScopeState |
| core → batch | 94 | AsyncCompletion, BatchAwaitParams, BatchAwaitWorker, BatchCutoverFacts |
| finalization → core | 19 | AwaitingAnswerError, CallOptions, Coordinator, DiskProviderRegistry |
| core → heartbeat | 15 | NewHeartbeatProvider, addHeartbeatUsage, hbCallSource, hbFastSource |
| core → usage | 13 | LocalizeResetHint, QuotaLimitGuidance, QuotaLimitResetTime, approxTokenCount |
| mailbox → core | 12 | ErrAgentShuttingDown, ErrSessionBusy, SessionAgentCall, SummarizeSnapshot |
| core → stream | 10 | causeHardCap, causeIdleStall, causeStall, causeToolTimeout |
| core → finalization | 10 | enqueueFinalizationCall, finalizationCandidate, finalizationExecution |
| usage → core | 10 | Model, PeakHoursGuidance, Role, cacheKeepAliveExplicitCacheProvider |
| core → mailbox / loop; stream → core; heartbeat → core; финализация → batch/mailbox; листья → batch | 7/5/7/4/2/6/1 | листья берут из batch только `_` (шум счётчика) |

По префиксным кластерам (`entangle.py internal/agent 12 300`) главное ребро уже не batch↔core, а **coordinator→batch: 63** и **batch→coordinator: 14** (CLIScopeState, Coordinator, DrainOwed, DrainPaced, FormatAsyncCompletion, ReactionDebtSource, awaitTasks*-константы, `coordinator` — сам конкретный тип, readTurnFactsGateSeam, workerAwaitBlockCap, workerToolNames); finalization→coordinator: 4 (AwaitingAnswerError, Coordinator, `coordinator`, subAgentDriverRegistry), coordinator→finalization: 3 (enqueueFinalizationCall, finalizationLogicalIdentity, finalizationWiring), finalization→batch: 6 (BatchInstall, BatchRootLeaseStore, batchBindingExecution, batchRootLeaseStore, batchRunnerAdapter).

Причина цикла — не порядок слоёв, а два конкретных типа: **278 методов на `*coordinator`** (замер `rg -c 'func (c *coordinator)'`; размазаны по 30+ файлам всех кластеров, включая batch_*, finalization_*, supervision.go, drain_attempt.go, credentials.go, wake_scheduler.go, cli_active_work.go) и **100 методов на `*sessionAgent`**. Плюс перекрёстные поля: `sessionAgent.coord *coordinator` (drain_attempt.go:172), `SessionAgentCall.batchQuestion *batchChildQuestionState` (agent.go:351), `SessionAgentCall.recovery *codexRecoveryState` (agent.go:280).

### 1.3. Импортёры пакета (analyze.py imports)

Импортируют `internal/agent` ровно 4 пакета: `internal/app`, `internal/cmd`, `internal/server`, `sdk`. Число различных `agent.X`-идентификаторов (регэксп по файлам, возможен маленький перебор из-за локальных переменных с именем `agent` — проверяется на месте в полосе DM8): **app 137, server 41, sdk 25, cmd 23**. Верх app: CLIScopeState 51, ReactionDebtSource 27, ErrSessionBusy 27, SetDrainPacingForTest 23, Coordinator 23, NewCallResultRecorder 22, BatchInstallDeps 21, ErrDiskProviderUnavailable 20, SessionAgentCall 19, ProviderMaxRetries 16. Сервер тянет контракт batch (BatchExternalSpec, BatchCLITask, AdmitBatchRoleContracts, ParseBatchExternalSpecJSONWithAgentDepth), sdk — типы учётных данных (Credential, CredentialModel, CredentialSet, ProviderType*, ModelChoice), cmd — служебные (ProviderMaxRetries — берётся адресом: `cmd/ping.go:307` `MaxRetries: &rushagent.ProviderMaxRetries`, QuotaLimitResetTime, NewHeartbeatProvider, FormatResetDuration).

Проводка из app: `app_agent_setup.go:150-225` — восемь точек: `SetCoordinatorSessionNoticeStore`, `SetCoordinatorExecutionRequirementStore`, `SetCoordinatorSessionCredentialRequirementStore`, `SetCoordinatorDiskCapabilityManager`, `BindCoordinatorBatchControls`, `SetCoordinatorBatchHeldAnswerSink`, `SetBatchDriverClaimer`, `BindCoordinatorBatchCutoverFacts` + `NewBatchCutoverNodeFacts`.

### 1.4. Глобалы, init(), embeds, ОС-файлы (проверено)

- Пакетные переменные через будущие границы: 29 (prereq.py), почти все — сентинел-ошибки; настоящие изменяемые: `cacheProfiles` (cache→usage), `ProviderMaxRetries` (provider_retry.go:13), атомики пейсинга `drainRetryAfterNS/...` (drain_attempt.go:55), `recheckPassIntervalNS` (coordinator_recheck.go), привязки batch: `coordinatorBatchBindings sync.Map` и `processBatchBinding atomic.Pointer` (batch_install_ingress.go:145-149, wave3 #1).
- `init()` в самом пакете — 2: `coordinator_recheck.go:27` (атомик 60s) и `drain_attempt.go:57` (4 атомика пейсинга). ОС-специфичных prod-файлов — 0 (prereq.py: `os-specific prod files: 0`).
- `go:embed` — 8, все `templates/*`: `agent_compaction.go:38` (summary.md), `agent_title.go:81` (title.md), `agent_tool.go:15` (agent_tool.md), `agentic_fetch_tool.go:20,51` (agentic_fetch.md, agentic_fetch_prompt.md.tpl), `prompts.go:11,14,17` (coder.md.tpl, task.md.tpl, initialize.md.tpl).
- Тестовые помощники через границы: 216 идентификаторов в 18 направлениях (prereq.py). Тесты `supervision_notice_binding_app_test.go` (package agent_test, 282 строки, импортирует `internal/app`) и `supervision_notice_binding_export_test.go` (package agent, помощники `SupervisionNoticeStoreForTest` и др. через `coord.sessionNoticeStore()`) — задача #95.
- Существующий подпакет `internal/agentresult` (профиль JSON Schema, 14 файлов) сегодня не импортируется ни одним `.go`-файлом вне себя (проверено `rg -l 'agentresult'`); этап C ставит его в `internal/agent/agentresult`. В разрезку не входит.

## 2. Целевое дерево

### 2.1. Дерево и счёт элементов

После этапа C в `internal/agent` 5 подкаталогов с кодом: `agentresult`, `prompt`, `provider`, `safety`, `tools` (mapping.c.json; `templates/` и `testdata/` — ресурсы, не считаются). Добавляется один каталог-группа `runtime/` (без .go): **`internal/agent` = 6 элементов ≤ 7**, один слот в запасе. Файлы из корня `internal/agent` уезжают все (в корне остаётся 0 .go).

```
internal/agent/                    6 элементов
├── agentresult/ prompt/ provider/ safety/ tools/     (этап C, без изменений)
└── runtime/                      7 элементов (каталог-группа, без .go)
    ├── coord/                    координатор: ядро после консолидации — 7 файлов
    ├── contract/                 2 элемента
    │   ├── agentcall/            лист контракта вызова (7 файлов)
    │   └── agenttest/            тест-помощники (2-4 файла)
    ├── exec/                     7 элементов
    │   ├── agentrun/             sessionAgent — ядро после консолидации: 7 файлов
    │   ├── turnstep/  drainctl/  mailbox/  watchdog/  sessionprompt/  responsesws/
    ├── batch/                    6 элементов
    │   ├── batchinst/  batchrun/  batchflow/  finalize/  delegation/  subagents/
    ├── policy/                   7 элементов
    │   ├── usage/  promptcache/  callbeat/  provauth/  clibridge/  toolwrap/  agenticfetch/
    ├── wakectl/                  durable wake: планировщик + dispatch (5 файлов)
    └── modelkit/                 сборка вызова: модели/провайдеры/инструменты (6 файлов)
```

Каждый лист ≤ 7 рукописных нетестовых файлов; слияние мелких файлов внутри пакета разрешено — оракул чистого переноса по объявлениям это выдерживает. Имена пакетов проверены на уникальность против `git grep -h '^package ' | sort -u` (агент, cmd, config, server, app, session, tools, mcp, cliprovider, …, heartbeat, oauth, prompt, notify, permission, codex, hyper, lab, jobbatch, batch): совпадений нет; новые имена (agentcall, coord, agentrun, turnstep, drainctl, mailbox, watchdog, sessionprompt, responsesws, wakectl, modelkit, batchinst, batchrun, batchflow, finalize, delegation, subagents, usage, promptcache, callbeat, provauth, clibridge, toolwrap, agenticfetch, agenttest) среди существующих не встречаются. «meaningful without path»: coord — дирижёр раннов; agentrun — рантайм сессионного агента; agentcall — контракт одного вызова; modelkit — сборка модели/инструментов вызова; wakectl — durable-будильник; sessionprompt — состояние и промпт сессии; batchinst/batchrun/batchflow — установка/исполнение/доставки batch; finalize — движок финализации вызова (имя `finalization` занято `session/batch/finalization`); callbeat — пульс вызова (имя `heartbeat` занято `session/heartbeat`).

### 2.2. Пакеты: роль, публичный API

| пакет | файлов кода | строки | роль и главный экспорт |
|---|---|---|---|
| contract/agentcall | 7 | ≈1.3k | контракт вызова: `Coordinator`, `SessionAgent`, `SessionAgentCall`, `CallOptions`, `Model`, `Role`, `ProviderType`, `CredentialSet`, `ModelChoice`, `ModelOverride`, `SummarizeSnapshot`, `RequestedModelRole`, `AwaitingAnswerError`, ошибки, `ErrTurnStalled`, `ToSessionAgentCallData`, `EnsureCallBuildScope`, `WithRateLimitNotifySink`, `DefaultCancelAllGrace`, порт `CallBindingHandle` + `BuildResourceScope`, `readyGate` (группа сборки), тексты вопроса (`AwaitingAnswerGuidance`) |
| runtime/wakectl | 5 | ≈0.8k | durable wake: планировщик, `WakeScheduleController`, `DispatchSessionWake`; порт WakeSink (§3.5) |
| runtime/modelkit | 6 | ≈5.3k | сборка вызова: модели/провайдеры/инструменты/snapshot summarize/классификация ретраев; `AutonomySource` (§3.5); `agentTool` через `agentcall.Coordinator` |
| runtime/coord | 7 | ≈5.8k | координатор: структура, `NewCoordinator`, run-цикл, interrupt-семейство, recheck, glue швов |
| exec/agentrun | 7 | ≈5.4k | sessionAgent: реализация `SessionAgent`, ядро turn-цикла (вкл. loop_detection), admission |
| exec/turnstep | 4 | ≈3.0k | машинерия шага: admission/effort/progress, step_progress, checkpoint/helpers/peakhours/ui-notify, заголовок, бюджет контекста/компакция |
| exec/drainctl | 4 | ≈3.4k | гарант drain/арбитр/реакция: `drainAttempt`, `turnArbiter`, reaction chain, cancel-флаг, await-era, `ErrDrainNotAttempted`, `ForeignWorkspaceError`; `CLIScopeState`/`ReactionDebtSource` — объявления уезжают в agentcall |
| exec/mailbox | 4 | ≈2.9k | mailbox + эры владения: тип `mailbox`, generation/inject/interrupt/queue, `agent_ownership` |
| exec/watchdog | 5 | ≈1.4k | сторож/таймеры/надзор: stream watchdog, `timeoutService`, `supervisionRegistry` (ретаргет EX-0F) |
| exec/sessionprompt | 5 | ≈0.9k | состояние сессии и сборка промпта (coordinator+sessionAgent); порт SessionStateHost |
| exec/responsesws | 2 | ≈0.7k | Responses-WebSocket: менеджер и его публичная обёртка |
| batch/batchinst | 7 | ≈6.1k | установка/допуск: `BatchInstall`, `BatchInstallDeps`, ingress+bindings, admission/scope, rehome, disk-provider-подсистема, `finalizationGate`, inline-window (читает привязку) |
| batch/batchrun | 5 | ≈4.0k | исполнение: `batch_exec*`, invocation, spec/timeout/output, model control, child tool scope |
| batch/batchflow | 5 | ≈3.7k | доставки/ожидание: `batch_await*`, `batch_delivery*`, `batch_cli*`, held-question, cutover, lease |
| batch/finalize | 3 | ≈2.1k | финализация вызова: очередь/исполнение/сверка/публикация |
| batch/delegation | 7 | ≈0.7k | роли делегирования (по файлу на роль) |
| batch/subagents | 6 | ≈1.5k | суб-агенты: `subAgentDriverRegistry`, bgshell, таймауты; порт SubAgentHost (§3.5) |
| policy/usage | 7 | ≈1.1k | учёт/квоты/часы пик + `agent_usage` (`unpricedModels` едет с пакетом) |
| policy/promptcache | 3 | ≈0.3k | профиль кэша промпта: `cache_profile`, `cache_diagnostics`, `cache_prefix_mcp` |
| policy/callbeat | 3 | ≈0.8k | пульс вызова + cache-keepalive (`fireCacheKeepAliveHB`) |
| policy/provauth | 7 | ≈1.0k | транспорта/токены + codex-recovery; порт RecoveryHost (§3.5) |
| policy/clibridge | 2 | ≈0.4k | мост CLI-провайдеров |
| policy/toolwrap | 7 | ≈1.2k | обёртки инструментов: hooked/logged/restricted/policy_leaf/role_tool_policy/tool_* |
| policy/agenticfetch | 2 | ≈0.4k | инструмент agentic fetch + 2 шаблона |
| contract/agenttest | 2-4 | — | тест-помощники: фейки `Coordinator`, строители `SessionAgentCall`, изоляция тестового окружения |

Правило направления: `agentcall` ← все; `coord`/`agentrun` — вершины; batch не импортирует coord/agentrun (только контракт и листья); циклов нет. Единственные рёбра batch→exec: `batchrun`/`batchflow` → `mailbox` (generation, mailbox — 2 id, prereq) и → `callbeat`; `finalize` → `batchinst`/`batchrun`.

### 2.3. Полная раскладка 253 файлов (файл → пакет)

Формат: пакет — список файлов целиком; «(+ X из f.go)» — объявление X переезжает из файла f (сам файл остаётся в другом пакете); «− X → pkg» — метод X уходит в pkg; «(причина…)» — основание выбора пакета по проверенным потребителям. Пересчитано по листингу каталога: ровно 253 нетестовых .go-файла (`ls *.go | grep -v _test | wc -l` = 253); каждый файл ниже входит ровно в один список, контрольная сумма в конце равна 253.

- **contract/agentcall** (8 исходных → 7 файлов после слияния errors+attempted): errors.go, call_attempted_error.go (оба — сентинел-ошибки вызова; сливаются в один ≤900), call_options.go, call_allowlist.go, call_data_conversion.go, rate_limit_notify_ctx.go; плюс объявления: `Coordinator` (iface, 34 метода), `ModelOverride`, `AwaitingAnswerError`, `errAwaitingAnswer` (+ coordinator.go); `SessionAgentCall`, `SessionAgent`, `Model`, `DefaultSessionName`, `userAgent` (+ agent.go); `SummarizeSnapshot` (+ coordinator_summarize.go); `ProviderType`+консты, `Role`+консты, `Credential`, `CredentialModel`, `CredentialSet`, `ModelChoice` (+ credentials.go); `RequestedModelRole` (+ delegation_role.go); `codexRecoveryState` (+ agent_codex_recovery.go); `batchChildQuestionState` (+ batch_question_route.go); `batchAgentQuestionBinding` (+ batch_install_policy.go); `ErrTurnStalled` (+ turn_stall_policy.go); `DefaultCancelAllGrace` (+ agent_control.go); `var ProviderMaxRetries` (+ provider_retry.go — временный шов, §3.3); `CLIScopeState`, `ReactionDebtSource` (+ coordinator_reaction_source.go — контрактные типы app, уезжают из reaction-компонента во избежание цикла modelkit↔drainctl); новые: `CallBindingHandle` (порт, §3.1); плюс целые файлы: ready_gate.go (причина: контекстная группа сборки вызова; потребители coordinator_models_build/call_tools, coordinator_tools, batch_cli — лежит ниже всех них), question_stop.go (причина: текстовые помощники `AwaitingAnswerError`; потребители agent_turn_failure, cli_active_work, coordinator_subagents/tools/work_scope, delegation_question, timeout_stop, app_run_result — нужен ниже всех перечисленных пакетов).
- **runtime/wakectl** (5): wake_scheduler.go (воркер durable wake + `WakeScheduleController`; три его метода на coordinator ретаргетятся в EX-0A), session_wake_dispatch.go (`DispatchSessionWake` принимает интерфейс `Coordinator` — единственная точка входа будки рядом с планировщиком), coordinator_wake.go, coordinator_wake_cli.go, coordinator_wake_tools.go (16 методов координатора → тип `wakeComponent`, порт WakeSink, §3.5).
- **runtime/modelkit** (22): coordinator_models.go, coordinator_models_build.go, coordinator_models_call_tools.go, coordinator_models_custom_role.go, coordinator_models_role.go, coordinator_model_cache.go, coordinator_providers.go, coordinator_providers_auth.go, coordinator_providers_network.go, custom_role_runtime.go, provider_limit.go, provider_retry.go, coordinator_rate_limit.go, coordinator_summarize.go, coordinator_summarize_role.go (снапшот summarize потребляет только resolver моделей), coordinator_retry_classify.go, coordinator_retry_testsupport.go (классификация провайдерских ретраев — домен сборки вызова), coordinator_tools.go, coordinator_tools_reviewer.go, coordinator_tool_assembly.go, agent_tool.go (инструмент делегирования строится вместе с инструментами вызова и работает через `agentcall.Coordinator`), mcp_output_access.go (аксессор `mcpOutputAccess` — единственный потребитель coordinator_tools.go); методы на coordinator ретаргетятся в EX-0B (порт AutonomySource, §3.5).
- **runtime/coord** (19): coordinator.go (−вытянутые объявления), coordinator_run.go (делится ≤900×2), coordinator_run_await.go, coordinator_run_effort.go, coordinator_run_queue_call.go, coordinator_recheck.go, coordinator_iface_assert.go, coordinator_interrupt.go (делится), coordinator_external_interrupt.go, coordinator_rerun.go, coordinator_agent_control.go, coordinator_await_tasks.go (+`armAwait` из await_tasks_era.go), coordinator_attempt_evidence.go, coordinator_call_result.go, coordinator_work_scope.go, coordinator_batch_stop.go, coordinator_agent_inject_batch.go, credentials.go (остаток файла: типы → agentcall, 6 методов sessionAgent → agentrun, 6 методов coordinator → cred_glue здесь), prompts.go (+templates/{coder,task,initialize}.md.tpl); + glue-приёмники: batch_glue.go, finalize_glue.go, drain_glue.go, cred_glue.go, cli_glue.go, misc_glue.go (создаются полосами DM/EX, имена уникальны, §5). DM7 консолидирует 19+glue в 7 файлов ≤900 (конкатенация — чистый перенос).
- **exec/agentrun** (10): agent.go, agent_run.go, agent_control.go, agent_options.go, agent_timeouts.go, agent_turn.go, agent_turn_step.go, agent_turn_stream.go, agent_turn_failure.go; loop_detection.go (причина: единственный потребитель — agent_turn_step.go, проверено grep); + приёмы методов: `newDrainAttempt`/`closeDrainAttempt`/`noteRefusal`/`afterTurn` (− drain_attempt.go), 6 методов (− credentials.go), по 1 методу (− queued_call_scope.go, usage_record.go, peak_hours_request_gate.go, turn_arbiter.go).
- **exec/turnstep** (15): turn_admission.go, turn_effort.go, turn_progress_guard.go, step_progress.go, step_progress_edit.go; agent_title.go, agent_turn_title.go (генерация заголовка — машинерия шага turn); agent_turn_checkpoint.go, agent_turn_helpers.go, agent_turn_peakhours.go, agent_turn_ui_notify.go (внутренности turn-цикла — едут с машинерией шага); coordinator_turn_admission.go, coordinator_turn_hold.go (допуск/удержание turn на стороне координатора); agent_compaction.go, agent_context_budget.go (бюджет контекста — работа PrepareStep/скользящего окна, порт ContextBudgetHost ≤15 имён, §3.5).
- **exec/drainctl** (17): drain_attempt.go, drain_settle_text.go, drain_pacing_testsupport.go, turn_arbiter.go, turn_arbiter_state.go, turn_stall_clock.go, turn_stall_policy.go; coordinator_reaction_chain.go, coordinator_reaction_source.go (−`CLIScopeState`, `ReactionDebtSource` → agentcall), reaction_write_cutover.go (реакционная цепь пишет через арбитр `c.arb` — тот же пакет); agent_drain.go, agent_drain_decision.go, agent_notice_pull.go (учёт drain на стороне sessionAgent); cancel_flag.go (причина: одноразовый флаг `sessions cancel` — учёт operatorStop рядом с `operatorStop`; потребители coordinator_run, agent_turn_step, coordinator_subagents — всем можно импортировать drainctl); coordinator_drain_policy.go; await_tasks_era.go (причина: 3 из 4 объявлений — методы `*arbiter`; `armAwait` → coord); agent_workspace_refusal.go (`ForeignWorkspaceError` — факт WS-1 учёта drain).
- **exec/mailbox** (10): mailbox.go, mailbox_generation.go, mailbox_inject.go, mailbox_interrupt.go, mailbox_ownership.go, mailbox_queue.go; queued_call_resolution.go + queued_call_scope.go (−1 метод → agentrun); agent_ownership.go, agent_own_work_batch.go (причина: эры владения держат mailbox напрямую — общий пакет снимает порт; `claimed atomic.Bool` едет с типом, §3.5).
- **exec/watchdog** (5): stream_watchdog.go, timeout_service.go, timeout_stop.go; supervision.go (причина: надзор делит единственный per-process таймер `timeoutService` и домен дедлайнов; 14 методов coordinator → `*supervisionRegistry`, ретаргет EX-0F, §3.5), supervision_testsupport.go (`SetSupervisionDefaultIntervalForTest`).
- **exec/sessionprompt** (5): coordinator_session_prompt.go, coordinator_session_state_sweep.go, coordinator_session_credential_requirement.go (состояние сессии/промпт/credential-требования на стороне координатора; порт SessionStateHost ≤5 имён, §3.5), agent_prompt.go, agent_prompt_order.go (сборка промпта на стороне sessionAgent).
- **exec/responsesws** (2): public_responses_websocket.go, coordinator_responses_ws.go (−методы coordinator → ретаргет в EX-0K на тип менеджера).
- **batch/batchinst** (30): admission.go ← batch_admission.go+batch_role_admission.go+batch_allowed_controls.go; scope.go ← batch_scope.go+batch_scope_recovery.go+batch_profile.go; ingress.go ← batch_ingress.go+batch_ingress_doc.go+batch_install_ingress.go+batch_rehome_coord_runtime.go(−SetCoordinatorSessionNoticeStore→coord)+batch_rehome_runtime.go; install.go ← batch_install_adapters.go(делится ≤900×2)+batch_install_bridge.go+batch_install_agent_driver.go+batch_install_tools.go+batch_install_question.go; rehome.go ← batch_rehome_async_completion.go+batch_rehome_drain_pace.go+batch_rehome_tool_metadata.go; policy.go ← batch_install_policy.go(−batchAgentQuestionBinding)+inline_window.go (причина: `inlineWindowDisabledFor` читает `*batchCoordinatorBinding`; `SetInlineWindowForTest` остаётся экспортной тонкой делегацией coord, реализуя `inlineWindowSetter`); runtime.go ← batch_runtime.go+тип `finalizationGate` (из finalization_gate.go, §3.1); disk.go ← disk_provider_binding.go+disk_provider_call.go+disk_provider_capability.go+disk_provider_continuation.go+disk_provider_continuation_testsupport.go+disk_provider_durable.go+disk_provider_intent.go.
- **batch/batchrun** (18): exec.go ← batch_exec.go+batch_exec_generic.go+batch_exec_shell.go+batch_exec_argv.go; exec_agent.go ← batch_exec_agent.go+batch_exec_agent_doc.go+batch_managed_call.go; invocation.go ← batch_invocation.go+batch_exec_binding.go; spec.go ← batch_external_spec.go+batch_depth_payload.go+batch_timeout_parse.go+batch_terminal_output.go; controls.go ← batch_model_control.go+batch_model_tools.go+batch_worker_hints.go+batch_supervision_arming.go; childscope.go ← batch_child_tool_scope.go (перечный скоуп инструментов ребёнка — домен исполнения).
- **batch/batchflow** (20): await.go ← batch_await.go+batch_await_doc.go+batch_done_cancel.go+batch_cancel.go; delivery.go ← batch_delivery.go+batch_delivery_doc.go+batch_delivery_drain.go+batch_held_question_route.go(−3 метода coordinator → coord); cli.go ← batch_cli.go+batch_cli_runtime.go; cutover.go ← batch_cutover_facts.go(−методы coordinator → coord)+batch_cutover_facts_node.go+batch_cutover_rerun.go(−метод → coord)+batch_cutover_driver.go(−SetBatchDriverClaimer → coord); lease.go ← batch_root_lease.go+batch_credentials_escrow.go+batch_credentials_marker.go; question.go ← batch_question_route.go(−тип)+batch_question_rebuild.go+batch_question_global_context.go+константа batchQuestionMarkerDepth.
- **batch/finalize** (21): queue.go ← finalization_queue_call.go(−3 метода coordinator → coord)+finalization_queue_reservation.go(−3 → coord)+finalization_call_context.go+finalization_candidate.go; execute.go ← finalization_execution.go+finalization_driver_binding.go+finalization_recompose.go; settle.go ← finalization_settle.go+finalization_seal.go+finalization_recover.go+finalization_wait.go+finalization_cancel.go; holds.go ← finalization_holds_assembly.go+finalization_holds_providers.go+finalization_authority_registry.go; wire.go ← finalization_bind.go+finalization_root_bind.go+finalization_wiring.go(−SetCoordinatorFinalizationWiring → coord); publish.go ← finalization_publish.go+finalization_hooks.go+finalization_batch_root_owner_reader.go; finalization_gate.go — тип `finalizationGate` переезжает в batchinst/runtime.go.
- **batch/delegation** (7): delegation_role.go (−RequestedModelRole), delegation_role_calls.go (−3 метода coordinator → coord), delegation_role_models.go, delegation_role_state.go, delegation_depth.go, delegation_question.go, root_role_contract.go.
- **batch/subagents** (6): coordinator_subagents.go, coordinator_subagent_drivers.go (−3 метода coordinator → coord; ретаргет в EX-0D), coordinator_background.go, coordinator_bgshell_cap.go, bgshell_claim.go, default_job_timeout.go.
- **policy/usage** (8): usage_fallback.go, usage_normalize.go, usage_record.go (−1 метод → agentrun), quota_reset.go, quota_reset_text.go, peak_hours_request_gate.go (−1 метод → agentrun), peak_hours_stop.go, agent_usage.go (причина: учёт usage на стороне sessionAgent; `unpricedModels sync.Map` едет с пакетом — глобал перестаёт быть межкластерным); + объявление `PeakHoursError` из coordinator.go:77.
- **policy/promptcache** (3): cache_diagnostics.go, cache_profile.go, cache_prefix_mcp.go.
- **policy/callbeat** (3): heartbeat_ctx.go, heartbeat_provider.go, agent_cache_keepalive.go (причина: cache-keepalive стреляет heartbeat'ом — `fireCacheKeepAliveHB` — тот же домен пульса).
- **policy/provauth** (7): oauth_bearer_transport.go, oauth_token_source.go (−методы → agentrun), codex_token_source.go, anthropic_empty_auth_transport.go; agent_codex_recovery.go, agent_codex_recovery_cleanup.go, agent_compaction_silent.go (причина: тихое восстановление после context-rejection — домен провайдерских auth/ошибок; `codexRecoveryState` уже в agentcall; порт RecoveryHost ≤7 имён, §3.5).
- **policy/clibridge** (2): cli_active_work.go (−4 метода coordinator → coord/cli_glue.go), cli_tool_bridge.go.
- **policy/toolwrap** (8): hooked_tool.go, logged_tool.go, restricted_tool.go, policy_leaf.go, role_tool_policy.go, tool_fatal_error.go, tool_suggest.go, tool_provider_options.go.
- **policy/agenticfetch** (2): agentic_fetch_tool.go, agentic_fetch_policy.go (+ каталог templates/{agentic_fetch.md, agentic_fetch_prompt.md.tpl}).

Контроль раскладки (пересчитан по листингу каталога; каждая цифра — число исходных файлов пакета; сумма равна числу нетестовых файлов каталога): **253 = 8(agentcall) + 5(wakectl) + 22(modelkit) + 19(coord) + 10(agentrun) + 15(turnstep) + 17(drainctl) + 10(mailbox) + 5(watchdog) + 5(sessionprompt) + 2(responsesws) + 8(usage) + 3(promptcache) + 3(callbeat) + 7(provauth) + 2(clibridge) + 8(toolwrap) + 2(agenticfetch) + 30(batchinst) + 18(batchrun) + 20(batchflow) + 21(finalize) + 7(delegation) + 6(subagents)**. Итоговые числа файлов после слияний: agentcall 7, wakectl 5, modelkit 6, coord 7, agentrun 7, turnstep 4, drainctl 4, mailbox 4, watchdog 5, sessionprompt 5, responsesws 2, usage 7, promptcache 3, callbeat 3, provauth 7, clibridge 2, toolwrap 7, agenticfetch 2, batchinst 7, batchrun 5, batchflow 5, finalize 3, delegation 7, subagents 6 — все ≤ 7, каждый файл ≤ 900 строк (проверено по суммам строк: batchinst ≈6.1k/7≈877, coord ≈5.8k/7≈825, drainctl ≈3.4k/4≈860, agentrun ≈5.4k/7≈773, turnstep ≈3.0k/4≈762 — максимумы ниже 900).

### 2.4. Конечные счёт элементов и решение по ядрам (Г1)

Решение оператора (Г1 делегирован, курс на расщепление): постоянных исключений для ядер НЕТ; извлечение компонентов из coord/agentrun — полосы спринта 0 (§3.5, EX-полосы §5). Конечное состояние — во всех каталогах кода ≤ 7 элементов:

| каталог | элементов | | каталог | элементов |
|---|---|---|---|---|
| internal/agent | 6 | | batch/batchinst | 7 файлов |
| runtime | 7 | | batch/batchrun | 5 файлов |
| contract | 2 | | batch/batchflow | 5 файлов |
| exec | 7 | | batch/finalize | 3 файла |
| batch | 6 | | batch/delegation | 7 файлов |
| policy | 7 | | batch/subagents | 6 файлов |
| contract/agentcall | 7 файлов | | policy/usage | 7 файлов |
| runtime/wakectl | 5 файлов | | policy/promptcache | 3 файла |
| runtime/modelkit | 6 файлов | | policy/callbeat | 3 файла |
| runtime/coord | 7 файлов | | policy/provauth | 7 файлов |
| exec/agentrun | 7 файлов | | policy/clibridge | 2 файла |
| exec/turnstep | 4 файла | | policy/toolwrap | 7 файлов |
| exec/drainctl | 4 файла | | policy/agenticfetch | 2 файла |
| exec/mailbox | 4 файла | | contract/agenttest | 2-4 файла |
| exec/watchdog | 5 файлов | | exec/sessionprompt | 5 файлов |
| exec/responsesws | 2 файла | | | |

Запасной механизм (если полоса встанет 1:1 до консолидации — планом не предусмотрен): временная запись в `.githooks/dir_entries_allowlist.txt` с падающим графиком, например `internal/agent/runtime/coord 26  # временно на время EX/DM: 26→…→7 при консолидации DM7; запись удалить` — число только уменьшается, запись удаляет полоса-консолидация. Постоянные записи не создаются; второй храповик A1b (все файлы с тестами) получает сиды на момент влития и монотонно падает по мере консолидаций.

### 2.5. Тесты: раскладка по префиксам

Белые тесты едут с кодом (`package <имя>` в каталоге пакета); тесты не считаются в лимите 7. Тестовый файл → пакет по префиксу кодового кластера (`agent_*_test.go` → agentrun; `batch_*_test.go` — по подстроке: install/ingress/rehome/admission/scope/profile/runtime → batchinst, exec/invocation/model_control/worker/supervision_arming/external_spec/managed → batchrun, await/delivery/cli/cutover/lease/credentials/question → batchflow; `coordinator_*_test.go` → coord; `supervision_*_test.go` → watchdog; `public_responses_websocket*_test.go` → responsesws; `wake_*_test.go`, `session_wake_dispatch_test.go` → coord; `drain_*`, `turn_arbiter*`, `turn_stall*` → drainctl; `mailbox_*`, `queued_call*`, `inject_mailbox_test.go` → mailbox; `stream_*`, `timeout_*` → watchdog; `usage_*`, `quota_*`, `peak_*` → usage; `cache_*` → promptcache; `heartbeat_*` → callbeat; `oauth_*`, `codex_token*` → provauth; `cli_*` → clibridge; `hooked/logged/restricted/policy_leaf/role_tool_policy/tool_*` → toolwrap; `agentic_fetch*` → agenticfetch; `delegation_*`, `root_role*` → delegation; `x2_delegation_fixture_test.go` → agenttest; `finalization_*` → finalize; `disk_provider_*` → batchinst; `call_*`, `call_options*`, `credentials*`, `errors*` → agentcall).

Тестовые сценарии без кодового кластера — по поведению (правило §3.6 плана): `run_*`, `interrupt_*`, `release_*`, `ownership_*`, `checkpoint_*`, `context_*`, `compact_*`, `rebuild_*`, `p0_*`, `p1_*`, `p2_*`, `is_busy`, `pending_inject`, `holder_abandonment`, `goroutine_identity`, `shutdown_gate`, `p343_*`, `r8_3_*`, `q2b_*`, `p1267_*`, `p464/p466/p469/p481/p482/p576/p577/p641/p646` → **agentrun** (жизненный цикл вызова/turn); `summarize_*`, `model_reload_pins`, `web_model_policy_*`, `session_policy_registry`, `prompt_folder_scope`, `nested_agent_tool_recursion`, `task74_*`, `deny_exec_policy_batch`, `fs_tools_unscoped_regression` → **coord** (сборка/политики на уровне координатора); `testutil_isolate_test.go`, `testguard_isolation_test.go`, `redacted_thinking_stream`, `f7_idle_timeout_durable_roundtrip`, `warnings`, `winner_result_no_turn_drain`, `standalone_summary_refusal`, `subagent_permission_inherit`, `session_idle_trigger`, `p350_coder_agent_selfheal`, `final_composition`, `update_models_gate_drain`, `generate_title_late_fallback` → по идентификаторам, фиксирует полоса DM7; `default_job_timeout_test.go` → subagents; `agent_title_*` → agentrun. Сценарии, которым нужны `coord`+`agentrun`+`app` одновременно, — `internal/app/itest` (туда же уходит пара supervision, §3.4). Правило: тест остаётся белым только там, где его неэкспортированные идентификаторы; если footprint шире одного пакета — расщепление по файлам или переезд в itest (решает DM7 по инвентарю).
Исключения из префиксного правила — по коду: `agent_title_*` → turnstep; `agent_compaction*`, `compact_*`, `context_*` → turnstep; `agent_cache_keepalive*` → callbeat; `agent_codex_recovery*` → provauth; `agent_ownership*`, `agent_own_work_*` → mailbox; `agent_prompt*` → sessionprompt; `agent_usage*` → usage; `agent_drain*`, `agent_notice_pull*`, `agent_workspace_refusal*` → drainctl; тесты `supervision_*` → watchdog; тесты `wake_*`, `session_wake_dispatch_test` → wakectl.

### 2.6. Граф зависимостей (стрелка = импортирует)

```
agentcall → {config, session, message, permission, prompt, tools, fantasy}   (лист группы agent)
agenttest → agentcall (+ session/db-фейки)
mailbox, watchdog, responsesws, turnstep, usage, promptcache, callbeat,
provauth, clibridge, toolwrap, agenticfetch, wakectl, modelkit,
sessionprompt, drainctl → agentcall (+ листья ниже)
batchrun → batchinst, callbeat; batchflow → batchinst, batchrun, mailbox;
finalize → batchinst, batchrun; delegation, subagents → agentcall, modelkit
agentrun → agentcall, mailbox, drainctl, watchdog, turnstep, sessionprompt, usage, promptcache, callbeat, provauth, clibridge, toolwrap
coord → всё перечисленное выше + batchinst/batchrun/batchflow + finalize + delegation + subagents + responsesws + wakectl + modelkit
```

Запрет: ничто в `runtime/batch/` не импортирует `coord`/`agentrun` (приёмка: `rg -l 'runtime/coord|runtime/exec/agentrun' runtime/batch` — пусто).

### 2.7. Карта для импортёров (замена квалификатора `agent.`)

Полоса DM8 делает механическую замену по таблице «идентификатор → квалификатор» (полные ~200 имён генерирует скрипт-инвентарь из объявлений; здесь — опорные, покрывающие верх частот):

| идентификаторы | новый квалификатор |
|---|---|
| Coordinator, SessionAgentCall, CallOptions, Model, Role/Role*, ProviderType*, Credential, CredentialSet, ModelChoice, ModelOverride, SummarizeSnapshot, AwaitingAnswerError, ErrSessionBusy, ErrRequestCancelled, ErrAgentShuttingDown, ErrDiskProviderUnavailable, ErrDiskProviderNotDurable, ErrTurnStalled, DefaultCancelAllGrace, ToSessionAgentCallData, EnsureCallBuildScope, WithCallOptions/WithCallOrigin/WithCallResultRecorder/WithAgentDepth/WithRateLimitNotifySink/WithAdmittedBatchRoleContracts/WithBackgroundJobNotice/WithRunAllowlist/WithModelOverrides/WithDrainCall, DefaultSessionName, DefaultAgentDepth, RequestedModelRole, InitializePrompt, ProviderMaxRetries, CLIScopeState, ReactionDebtSource | `agentcall` (путь `internal/agent/runtime/contract/agentcall`) |
| NewCoordinator, CallResultRecorder/NewCallResultRecorder, AutoTurnHolder, PersistSessionAgentDepth, ChildSessionID, ToolName/ToolCallID/ReviewerReadOnlyToolNames, IsContinuationPrompt, InitializePrompt, SetInlineWindowForTest, SetTurnRetryBackoffForTest | `coord` — по месту объявления из §2.3 (фиксирует скрипт-инвентарь) |
| DrainPaced/DrainOwed/DrainNone/DrainDeferred/DrainStuck, SetDrainPacingForTest, IsDrainNotAttempted, ForeignWorkspaceError, RunTimeoutCause | `drainctl` (RunTimeoutCause/WatchdogResumeGuidance — watchdog) |
| BatchInstall, BatchInstallDeps, BatchExternalSpec, BatchCLITask, AdmitBatchRoleContracts, ParseBatchExternalSpecJSONWithAgentDepth, BatchRoleSnapshotHash, BatchRerunStopper, ErrBatch* | `batchinst` / `batchrun` / `batchflow` по §2.3 |
| NewHeartbeatProvider → `callbeat`; WakeScheduleController, SessionWakeDispatcher → `wakectl`; SetCoordinatorSessionNoticeStore, SetCoordinatorExecutionRequirementStore, SetCoordinatorSessionCredentialRequirementStore, SetCoordinatorDiskCapabilityManager, BindCoordinatorBatchControls, SetCoordinatorBatchHeldAnswerSink, SetBatchDriverClaimer, BindCoordinatorBatchCutoverFacts, NewBatchCutoverNodeFacts, SetCoordinatorFinalizationWiring | остаются в `coord` (имена не меняются, §3.2) |
| SetSupervisionDefaultIntervalForTest → `watchdog`; SetTurnStallPolicy → `drainctl` | имена сохранены, меняется только квалификатор |

Имена экспорта при переносе не меняются (меняется только квалификатор) — это делает переезд импортёров механическим sed+build.

## 3. Швы

### 3.1. S-A1 «лист контракта» (рвёт цикл листьев)

Проблема: листья (stream, loop, usage, quota, cache, mailbox, heartbeat, oauth) зависят от ядра только общими типами (И§11.2), но сами типы лежат в ядре; `CallOptions` вдобавок держит приватные поля дисковых типов (`diskBinding *callDiskBinding`, `buildScope *buildResourceScope`, call_options.go:25-45), а `callDiskBinding` (disk_provider_call.go:18) ссылается на `*diskCapabilityManager` и `tools.DiskProvider`.

Шов:
1. Пакет `contract/agentcall` получает перечисленные в §2.3 типы. `Coordinator` (coordinator.go:146-268) и `SessionAgent` (agent.go:528-614) переезжают целиком: их сигнатуры ссылаются только на переезжающие типы и пакеты ниже (fantasy, message, session.SessionAgentCallData, config.SelectedModelType) — проверено построчно.
2. Порт вместо дискового типа: в agentcall объявляется `type CallBindingHandle interface { BindingID() string }`; поле `CallOptions.diskBinding` меняет тип на этот интерфейс, `buildResourceScope` (disk_provider_capability.go:59 — только sync) переезжает в agentcall как `BuildResourceScope`. `callDiskBinding` остаётся в batchinst (владелец реестра) и получает метод `BindingID() string`; функции-потребители поля (`disk_provider_call.go`, `EnsureCallBuildScope` → agentcall) читают его через интерфейс. Вычисления не меняются: поле по-прежнему инициализируется один раз в `BindCallDiskProvider` до публикации опций, чтения — те же точки, добавлена только косвенность вызова метода.
3. `finalizationGate` (finalization_gate.go) переезжает в `batchinst` — снимается ребро batch→finalization (1 id, prereq); финализация получает врата через `batchinst` (finalize → batchinst, §2.6).
4. `ErrTurnStalled` и `DefaultCancelAllGrace` — в лист (их читают app/server).

Аргумент сохранения поведения: перенесены только объявления; все точки вызова меняют квалификатор; порядок инициализации не появляется нового (в листе нет init()); сентинел-ошибки создаются один раз при загрузке agentcall, раньше любого использования (все потребители импортируют agentcall).

### 3.2. S-A2 «наборы возможностей + ключ привязки» (wave3 #1, #2)

Проблема: batch-проводка держится на `coordinatorBatchBindings sync.Map` с ключом `*coordinator` и 8 сеттерах после конструирования (§1.3); после разрезки batch не может ни ссылаться на `*coordinator`, ни делать `c.(*coordinator)` (batch_cli.go:62, batch_cutover_driver.go:31, batch_install_policy.go:254).

Шов:
1. Типизированные наборы при конструировании: `BatchRuntimeCapabilities{Ingress BatchWorkIngress; ModelControl BatchModelControl; DiskBindings *DiskProviderRegistry; LiveReader *session.BatchLiveReader; DriverClaimer BatchDriverClaimer; CutoverFacts BatchCutoverFacts; HeldAnswerSink BatchHeldAnswerSink; NoticeStore *session.SessionNoticeStore}` и `SessionExecutionCapabilities{ExecReqs session.SessionExecutionRequirementStore; CredReqs session.SessionCredentialRequirementStore}` — параметры `NewCoordinator` (coordinator.go:488). Восемь точек из app_agent_setup.go:150-225 переходят с «создал → сеттер» на «собрал bundle → передал»; необязательные возможности остаются явным nil (сегодняшняя семантика сеттеров «не установлен» сохраняется). Порядок: bundle валидируется один раз при конструировании, дальше — только чтение полей; сеттеры остаются тонкими обёртками над записью тех же полей для обратной совместимости вызовов вне app (имена не меняются, §2.7).
2. Ключ привязки: `coordinatorBatchBindings sync.Map` меняет тип ключа с `*coordinator` на `agentcall.Coordinator` (значение-интерфейс сравнимо, `*coordinator` остаётся фактическим ключом — те же записи, тот же first-wins CAS `processBatchBinding.CompareAndSwap`, batch_install_ingress.go:195).
3. Вся glue-логика с type-assertion (`SetBatchDriverClaimer`, `BatchRerunStopVoided`, `readTurnFactsBatch`, `heldQuestionAwaitRefusal` и др. — методы на `*coordinator` из batch_*-файлов) физически переезжает в `coord/batch_glue.go` (приёмники обязаны жить в пакете типа); batch-пакеты видят только контракт.
4. Временный процесс-fallback `processCoordinatorBatchBinding` (wave3 #1, вариант (a)) сохраняется без изменений до отдельной задачи — его судьба не входит в этот дизайн.

Аргумент сохранения поведения: та же карта, те же ключи-значения, тот же порядок «install до Invoke»; смена типа ключа не меняет сравнимости; bundle-поля заполняются теми же значениями из app до первого Run; единственная новая косвенность — чтение возможностей из полей вместо assert-цепочек.

### 3.3. S-A3 «тест-швы и глобалы через границу»

- `var ProviderMaxRetries` (provider_retry.go:13) — единственная изменяемая экспортная переменная, которую читают по адресу (`cmd/ping.go:307` передаёт `&` в fantasy; app мутит и восстанавливает в тестах, wave3 #4). Решение: переменная переезжает в `agentcall` БЕЗ изменения семантики (адрес и ленивость чтений fantasy сохраняются) и записывается в §11 плана как временный шов (прецедент `procinfo.RegisterTrackedJobTerminator`). Настоящий порт — per-call `RetryPolicy` в модели — отдельная задача вне спринта (меняет точки вычисления, урок §3.12 волны 1).
- `cacheProfiles` (promptcache) — единственный не-ошибочный глобал, читаемый чужим кластером (usage): доступ через функцию `promptcache.Profiles()` вместо переменной; сама переменная остаётся непубличной в promptcache.
- `SetDrainPacingForTest`/`SetTurnRetryBackoffForTest`/`SetSupervisionDefaultIntervalForTest`/`SetInlineWindowForTest`/`SetTurnStallPolicy` — функции, не переменные: переезжают со своими файлами (drainctl/coord/watchdog/coord/drainctl), имена сохранены; атомики под ними переезжают вместе.
- Остальные 25 из 29 глобалов через границы — сентинел-ошибки: едут в agentcall (ошибки ядра) или остаются в пакете-владельце; список выдаёт prereq.py, приёмка — «ни одной новой экспортной var» (`rg -n '^var [A-Z]'` по runtime — только ошибки и ProviderMaxRetries).

### 3.4. S-A4 «тест-помощники и задача #95»

- Пакет `contract/agenttest` (по образцу `db/dbtest` — не импортирует пакеты, чьи белые тесты его используют): фейки `Coordinator`/`SessionAgent`, строители `SessionAgentCall`/`CallOptions`, изоляция окружения (`testutil_isolate`), общие фикстуры делегирования (`x2_delegation_fixture_test.go`). Инвентарь 216 идентификаторов (prereq.py) разбирает полоса SA4: идентификатор либо едет в agenttest (если не требует приватных полей), либо остаётся `_test.go`-файлом пакета-владельца (белое правило).
- Задача #95: сценарий `supervision_notice_binding_app_test.go` (нужен app+db+session+coord) → `internal/app/itest`; его экспорт-помощники (`SupervisionNoticeStoreForTest` и др. — читают `coord.sessionNoticeStore()`) превращаются в prod-файл тест-поддержки `watchdog/supervision_notice_support.go` по существующему образцу `supervision_testsupport.go` (белый `_export_test.go` в coord не нужен: itest импортирует экспортируемые `*ForTest`-функции). Зависимость: после SA2 (проводка notice-store в bundle).

### 3.5. S-A5 «компоненты»: вынос методов из ядер (решение по Г1)

Решение оператора (Г1 делегирован, курс на расщепление): постоянных исключений для `coord`/`agentrun` нет; извлечение компонентов — полосы спринта 0 (EX-0 в §5). Конечное состояние — ≤7 файлов в каждом каталоге (§2.4). Механика EX-полосы: (1) в старом пакете вводится тип компонента (`wakeComponent`, `governor`, `sessionOwnership`, …) и методы ретаргетятся: `func (c *coordinator) X(...)` → `func (w *wakeComponent) X(...)` — тела байт-в-байт; (2) вызовы внутри компонента меняют приёмник, внешние остаются на тонких делегатах coordinator (`func (c *coordinator) wakeSession(...) { return c.wake.WakeSession(...) }`) — интерфейс `Coordinator` (agentcall) не меняется, импортёры не трогаются; (3) DM-полоса переносит файлы компонента в его пакет, порт объявляется в пакете компонента и реализуется координатором. Оракул чистоты: шаг (1) — `git diff` показывает только токен приёмника и имя вызова (проверка: sed-нормализация приёмника возвращает пустой diff); шаг (3) — оракул переноса (сортированные объявления, число Test-функций неизменно); для критических путей (batch commit, finalization, drain, watchdog, ownership) — серийные тесты из §5 плюс race-прогон оркестратором.

Порты (имена точные, ≤15; реализуются координатором тонкими методами; списки выверены по инвентарю вызовов `c.X(`/`a.X()` в файлах компонента):

| порт (пакет компонента) | имена (число) | доказательство порядка замков |
|---|---|---|
| WakeSink (wakectl) | WakeSession, DrainDecision, DrainCallFor, HandleDrainBuildError, NoteDrainRefused, RecheckAdd, RecheckClear, AgentFor (8) | планировщик живёт на `startOnce`/`stopOnce` (wake_scheduler.go:62-63) и вызывает порт ВНЕ своих критических секций; реализации берут ровно те замки, что сегодня берут одноимённые методы coord (`recheckMu` coordinator.go:430, arbiter `mu` через drain-методы); `mailbox.mu` не удерживается ни одним вызовом — порядок «без замка → замки ядра» односторонний, инверсия невозможна |
| AutonomySource (modelkit) | WorkerSubAgentActive, WorkerSubAgentActiveForCall (2) | реализация читает `activeModelRoleMu` (coordinator.go:398) и атомик `persistentMode` (:413) без вложенных вызовов modelkit; `modelCacheMu` (:456) переезжает в modelkit как лист — под ним AutonomySource не вызывается (попадания кэша автономию не читают), цикла порядка нет |
| DrainHost (drainctl) | PrepareContinuation, PublishReauthenticate, DriverIsExternal, AddRecheck, CredentialBoundary (5) | учёт drain исполняется после ноги и ДО освобождения mailbox (drain_attempt.go:4-7: «exactly once, after the leg ends and before the mailbox is released») — `mailbox.mu` в момент порта не удерживается; арбитров `mu` (turn_arbiter_state.go:49) переезжает в drainctl и не держится при вызове порта (вердикт decideAccount вычисляется до исполнения — drain_attempt.go:249 и далее); `mu` disk-менеджера (disk_provider_capability.go:60) входит той же единственной точкой, что сегодня |
| OwnershipHost (mailbox) | встраивает `agentcall.SessionAgent`; + RestartOrphanedWithRetry, DiskCapsRef, OnSessionIdle (3) | эры владения работают с mailbox в одном пакете — доступ через `m.mu` (mailbox.go:133, «single critical section») сохраняет сегодняшнюю последовательность байт-в-байт; `claimed atomic.Bool` (agent_ownership.go:809) едет с типом; порт вызывается вне `m.mu` в тех же точках, что сегодня |
| SupervisionHost (watchdog) | HandleSupervisionDeadline, WakeSession, AgentFor, AfterReleaseContext, AdmitCallback, SupervisionWorkOpen, SessionNoticeStore, ResolveSupervisionConfig, ReleaseDriverGeneration, ReleaseChildDriverIfScopeClosed, ReleaseCallbackHook, NoteSubAgentChildRunEnded (12) | тик надзора сегодня идёт вне `mailbox.mu` и вне арбитра `mu`; `supervisionRegistry.mu` (supervision.go:134) и `timeoutService.mu` (timeout_service.go:48) переезжают в watchdog вместе и не вложены друг в друга извне — порядок «тик: registry.mu → порт» сохраняется как сегодня |
| SessionStateHost (sessionprompt) | SweepIdleSessions, SweepDeletedSessionState, SweepArbiterEntries, SweepArbiterEntriesAt, ResolveSessionSystemPrompt (5) | sweep-проходы — фоновый код вне критических секций, как сегодня; арбитров `mu` берётся на ту же операцию тем же кодом |
| SubAgentHost (batch/subagents) | WakeSession, BuildModelsFromCfg, BGShellRowCancelled, HeldQuestionResumeHint, HeldQuestionResumeRefusal, RecheckAdd, SessionNoticeStore, AutonomyEnabled, ClaimAutoResumeSlot, ResetConsecutiveResume, ConsecutiveResume, DelegatedChildDriven (12) | спавн суб-агента идёт из горутины инструмента: `admitMu` (agent.go:710) не пересекается — порт вызывается вне admission-секции, как сегодня |
| RecoveryHost (provauth) | RunSilentRecoverySummary, AccountRecoverySummary, CheckRecoverySummaryInput, PreparePrompt, SessionMessages, WorkaroundMediaLimitations, RecordMessageUsage (7) | recovery-воркер — собственная горутина; собственное состояние — листовые атомики `spent`/`attempted` (agent_codex_recovery.go:36-37) |
| ContextBudgetHost (turnstep) | PreparePrompt, SessionMessages, RecordMessageUsage, UpdateSessionUsage, OpenrouterCost, TryAdmitRunWg, GetMailbox, AbandonOwnershipWithHandoff, RunSummarize, RunSummarizeBody, RequeueSummarizeSnapshot, AccountRecoverySummary, CheckRecoverySummaryInput, EffectiveStreamWatchdogTick (14) | компакция/бюджет выполняется в фазе шага: `mu`/`toolResultMu` стрим-петли (agent_turn_stream.go:181,202) не держатся при вызовах порта — последовательность вызовов та же, что сегодня; `watchdogCauseVal` — локальный атомик шага (agent_turn.go:424) |

Правило доказательства для каждого порта: (i) перечислить замки, которые реализации порта берут сегодня — их же и берёт новый код; (ii) показать по точкам вызова, что компонент не вызывает порт, удерживая собственный замок (замок компонента — лист); (iii) тогда порядок «листовой замок компонента → замки ядра» не порождает цикла, а существующий порядок ядра не меняется, потому что тела реализаций байт-в-байт.

## 4. Глобалы, init(), embeds, ОС-файлы, белые тесты, помощники

1. **init()** — ровно 2, оба безопасны к переносу: `coordinator_recheck.go:27` (`recheckPassIntervalNS.Store(60s)` — атомик, читается только в рантайме) едет в coord; `drain_attempt.go:57` (4 атомика пейсинга) — в drainctl. Ни один не устанавливает порядок для другого пакета; значения те же, первое использование всегда позже загрузки. init() подпакетов (`agentguard/command_policy.go:191` `shell.RegisterCommandPolicy`, `tools/safe.go:245`, `tools/ssrf_guard.go:39`) — вне скоупа (этап C).
2. **go:embed** — 8 файлов: `templates/` делится по потребителям и едет с кодом (пути embed относительные): `coord/templates/{coder.md.tpl, task.md.tpl, initialize.md.tpl}` (prompts.go → coord), `turnstep/templates/{summary.md, title.md}` (agent_compaction.go, agent_title.go → turnstep), `modelkit/templates/agent_tool.md` (agent_tool.go → modelkit), `agenticfetch/templates/{agentic_fetch.md, agentic_fetch_prompt.md.tpl}`. `testdata/` (`TestCoderAgent`, `anthropic_credit_balance_1281.json`) едет с `agent_run_test.go` → `agentrun/testdata/`.
3. **ОС-файлы** — в пакете 0; правило на будущее: пары `_windows/_unix` не разлучать (§3.5 плана спринта).
4. **Белые тесты** — 569/571; остаются белыми в пакетах своих идентификаторов (§2.5); тесты с footprint на два ядра — расщепление или itest (DM7). Число тестовых функций каждого переносимого набора не меняется (оракул).
5. **Тест-помощники** — §3.4; ничего из agenttest не импортирует coord/agentrun.
6. **Порядок init/потоков** — новые пакеты не добавляют init() и горутин на загрузке; `wake_scheduler`-горутина и `timeoutService` стартуют как сегодня — из конструктора coord (coordinator.go:488 и далее; wake-компонент стартует вместе с ним в EX-0A/DM — момент не меняется).

## 5. План полос

Все полосы: `gofumpt`, `go build`, `go vet` затронутых пакетов через `.rush/tmp/golock.sh`; тесты серийно запускает оркестратор (§3.7 плана). Не более 5 агентов одновременно. Вся серия EX-0 выполняется в старом пакете до любых DM-переносов (§3.5): каждая DM-полоса везёт уже ретаргетнутые файлы в их итоговый пакет из §2.3. Оракул каждой полосы: отсортированный список `^(func|type|var|const) ` перенесённого набора файлов до/после — пустой diff; число Test-функций набора не меняется; для glue/швовых полос — дополнительно список вызовов из таблиц швов.

| № | полоса (этап) | скоуп | ждёт | параллельно с | приёмка |
|---|---|---|---|---|---|
| SA1 | 0б | создать `contract/agentcall` (+извлечения объявлений из agent.go, coordinator.go, credentials.go, delegation_role.go, batch_question_route.go, batch_install_policy.go, agent_codex_recovery.go, coordinator_summarize.go, turn_stall_policy.go, agent_control.go); целые файлы ready_gate.go, question_stop.go; порт CallBindingHandle | — | SA3, SA4 | build/vet; в agentcall нет импорта `internal/agent"`; оракул набора; `rg -n 'diskCapabilityManager' contract/` пусто |
| SA2 | 0б | bundle-конструктор NewCoordinator + 8 точек app_agent_setup.go; ключ sync.Map → интерфейс; glue-функции с assert → coord/batch_glue.go | SA1 | — (конфликт с #132 и DM-полосами batch) | build/vet app+agent; тесты привязки: `TestBindCoordinatorBatchRuntimeBindings_*` (root_agent_diskcaps_test.go:22), `TestAgentDriverRunsPinnedLeaf`, `TestAgentDriverFailsClosedWithoutBinding` (batch_install_tools_test.go:301,352), `TestBatchCLIBindOwnsConstructionScopeUntilRelease` (batch_construction_scope_boundary_test.go:127) — зелёные серийно |
| SA3 | 0б | переезд `ProviderMaxRetries` в agentcall + правка квалификаторов в cmd/ping.go и тестах app; `promptcache.Profiles()` | SA1 | SA4 | build/vet; grep: `ProviderMaxRetries` остаётся ровно одна var |
| SA4 | 0б | `contract/agenttest` (инвентарь 216 id, перенос общих помощников); задача #95: сценарий → internal/app/itest, помощники → watchdog/supervision_notice_support.go | SA1, SA2 (для #95) | SA3 | build/vet; itest-тест #95 зелёный серийно |
| EX-0A | 0б | ретаргет 16 методов wake → `wakeComponent`; порт WakeSink (§3.5) | SA2 | — (серия EX-0 последовательна: общие файлы вызовов) | sed-нормализация приёмника даёт пустой diff; build/vet |
| EX-0B | 0б | ретаргет моделей/провайдеров/инструментов/summarize/ретраев → компоненты modelkit; порт AutonomySource | EX-0A | — | то же; серийно: model_reload_pins, web_model_policy_*, p576 |
| EX-0C | 0б | ретаргет reaction+drain_policy → governor; порт DrainHost. **Конфликт с #132** | EX-0B | — | серийно: drain_*, winner_result_no_turn_drain, p0_2_orphaned_retry; race |
| EX-0D | 0б | ретаргет subagents/background/bgshell → компонент; порт SubAgentHost | EX-0C | — | subagent_permission_inherit, nested_agent_tool_recursion |
| EX-0E | 0б | ретаргет session-трио + prompt-пары; порт SessionStateHost | EX-0D | — | session_idle_trigger, prompt_folder_scope, f7_idle_timeout_durable_roundtrip |
| EX-0F | 0б | ретаргет 14 методов supervision → `*supervisionRegistry`; порт SupervisionHost | EX-0E | — | supervision_* серийно; race (таймеры) |
| EX-0G | 0б | ретаргет ownership → `sessionOwnership`; порт OwnershipHost | EX-0F | — | ownership_handoff*, holder_abandonment, r8_3_*; race |
| EX-0H | 0б | компакт-семейство: keepalive→callbeat, silent→provauth, compaction/context→turnBudget; порты | EX-0G | — | agent_compaction_*, compact_lock, context_capacity*, cache_keepalive тесты |
| EX-0K | 0б | ретаргет одиночных методов-фрагментов (cli 4, queued_call_scope 1, usage_record 1, peak gate 1, oauth_token_source, turn_arbiter 1, responses_ws, batch-фрагменты, finalize 6) | EX-0H | — | оракул по каждому фрагменту; build/vet |
| DM0 | D2 | скрипт-инвентарь (только чтение): карта «файл → пакет» §2.3 в машиночитаемом виде + генерация полной таблицы импортёров | SA1-SA4 | — | список покрывает 253/253 и все `agent.X` импортёров |
| DM1 | D2 | exec/mailbox (10 → 4 файлов) + exec/watchdog (5) + exec/sessionprompt (5) + exec/responsesws (2) | SA1, EX-0E, EX-0F, EX-0G, EX-0K | DM2, DM3, DM4, DM-W | оракул; ретаргет-методы уходят в компоненты, остатки — в glue |
| DM2 | D2 | exec/drainctl (17 → 4 файлов) + exec/turnstep (15 → 4 файлов) | SA1, EX-0C, EX-0K | DM1, DM3, DM4, DM-W | оракул; init() drain_attempt переехал (значения те же) |
| DM3 | D2 | policy/usage (8 → 7 файлов) + promptcache (3) + callbeat (3) + provauth (7) + clibridge (2) | SA1, SA3, EX-0H, EX-0K | DM1, DM2, DM4, DM-W | оракул; `cacheProfiles` не экспортирован |
| DM4 | D2 | policy/toolwrap (8 → 7 файлов) + policy/agenticfetch (2, с шаблонами) | SA1, EX-0K | DM1-DM3, DM-W | оракул |
| DM5a | D2 | batch/batchinst (30 → 7 файлов; В7: batch_install_adapters.go 958 делится ≤900) | SA1, SA2, EX-0K | DM5b, DM5c | оракул; batch не импортирует coord/agentrun |
| DM5b | D2 | batch/batchrun (18 → 5 файлов) | SA1, SA2, DM5a | DM5c | оракул; направление batchrun→batchinst |
| DM5c | D2 | batch/batchflow (20 → 5 файлов) | SA1, SA2, DM5a, DM5b | — | оракул; направление batchflow→batchrun→batchinst |
| DM6 | D2 | batch/finalize (21 → 3 файла) + delegation (7) + subagents (6) | DM5a, EX-0D, EX-0K | DM7 | оракул; finalize→batchinst |
| DM-W | D2 | runtime/modelkit (22 → 6 файлов; В7: coordinator_providers.go 957 делится) + runtime/wakectl (5) | EX-0A, EX-0B, EX-0K | DM1-DM6 | оракул переноса; build/vet modelkit/wakectl |
| DM7 | D2 | runtime/coord (19 + glue → 7 файлов; В7: coordinator_run.go 998, coordinator_interrupt.go 958, agent.go 966 делятся) + exec/agentrun (10 + glue → 7 файлов) — консолидация-конкатенация | DM1-DM6, DM-W | — | оракул консолидации (сортированные объявления) и по каждому glue; `rg -n 'func (c *coordinator)'` — только runtime/coord; `rg -n 'func (a *sessionAgent)'` — только agentrun; шаблоны coord на месте |
| DM8 | D2 | импортёры: app, cmd, server, sdk — замена квалификаторов по карте DM0 (+правка якорей живых доков) | DM7 | — | `rg -l 'rush/internal/agent"'` вне runtime — пусто; build/vet 4 пакетов; серийные тесты |
| DM9 | D2 | конфигурация: сиды второго храповика (без записей coord/agentrun — §2.4/В-1), `.github/ci-policy.toml`/`shards.toml` (изоляция `internal/agent` пересобирается по новым путям), `.golangci.yml` | DM8 | — | линтеры pre-push зелёные |

Конфликт с задачей #132 (финализация, `agent`+`session`): полосы **SA2, DM5a-c, DM6, DM7** трогают те же файлы (batch/finalization/coordinator) — #132 не ведётся одновременно ни с одной из них; рекомендуемое окно: 0б-1 (SA1-SA4) до старта #132, либо после его завершения.

Параллелизм: максимум 5 — например, {DM1, DM2, DM3, DM4} или {DM5a, DM5b, DM5c, DM6-подготовка}. Серия EX-0 выполняется последовательно (общие файлы вызовов в старом пакете); одна EX-0 может идти параллельно с одной DM-полосой по непересекающимся файлам. Полосы одного пакета не параллеляются (общие glue-файлы); glue-файлы именуются `<кластер>_glue.go` уникально. DM-W и DM1-DM6 имеют непересекающиеся файлы и могут идти параллельно (в сумме ≤5 агентов); DM7 выполняет консолидацию: coord 19+glue → 7 файлов, agentrun 10+glue → 7 файлов (конкатенация — чистый перенос), поэтому превышения 7 в моменте не возникает (§2.4).

## 6. Риски и открытые вопросы оператору

- **В-1: решено оператором (Г1 делегирован)**: постоянных исключений для ядер нет; извлечение компонентов — полосы спринта 0 (§3.5, EX-0 в §5), конечное состояние ≤7 везде (§2.4). Запасной временный механизм храповика описан в §2.4 и планом не задействован.
- **В-2: `ProviderMaxRetries`** — временный экспортируемый var в agentcall (§3.3). Принять запись в §11 плана или немедленно сделать RetryPolicy-порт (второе меняет точки вычисления — против урока §3.12).
- **В-3: сид второго храповика** для coord/agentrun «с тестами» фиксируется на момент влития DM7 и монотонно падает по мере консолидаций (§2.4); отдельное подтверждение оператора не требуется — механизм утверждён решением В-1.
- **Приближения**: списки идентификаторов — оценка скриптов (entangle/prereq); DM0 обязан сгенерировать точную карту до DM5. Подсчёт `agent.X` импортёров может включать редкие локальные переменные `agent` (проверка на месте в DM8). Поведение fantasy с `*int` (ленивое чтение `ProviderMaxRetries`) — прочитано по комментарию provider_retry.go:3-13, не исполнением; SA3 не меняет механику, риск снят.
- **Не проверено исполнением** (запрещено правилами задачи): отсутствие циклов после DM-полос (`go list`), серийные тесты, время набора. Оркестратор прогоняет их на каждой приёмке полосы.
- **agentresult**: пакет сегодня без импортёров (§1.4); если это ожидающий подписи контракт (#371), его судьба (оставить/заморозить) — вне этого дизайна, вопрос отмечен для docs/modules.md (этап G).
