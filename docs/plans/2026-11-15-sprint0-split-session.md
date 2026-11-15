# Дизайн разрезки `internal/session` (D1, задача #74, спринт 0; рев. 2 — решение Г1 по компонентной экстракции)

Документ этапа D1: целевое дерево, швы, компонентная экстракция, порядок полос для 0б-2 (#83) и D2 по пакету `internal/session`. Источники: план `docs/plans/2026-11-15-sprint0-plan.md` (§3, §5, §6), исследование И§2–5, И§9, И§11.2, скрипты `prereq.py`/`entangle.py`/`analyze.py`. Ревизия 2: оператор делегировал Г1 и решил вопрос о превышениях «7 элементов» — ПОСТОЯННЫХ ИСКЛЮЧЕНИЙ НЕТ; компонентная экстракция (разрезка методо-связанного ядра на типы-компоненты в подпакетах с узкими портами) входит в объём спринта как полосы D2; строка-храповик допустима только как временная, с падающим числом и расписанием. То же решение выдано для хранилища конфигурации (документ D1-config координируется отдельно). Дефекты из ревью исправлены: S6 переписан (psf не используется batch_commit.go), тег `!windows` уточнён, `MaxPidFallbackAge` — `lock.go:110`, jobbatch — 20 нетестовых файлов (пересчитано), `run_queue_orphan_drain.go` назначен (в рев. 1 пропущен).

## 0. Резюме

1. Ядро `service` (100 методов `*service` в 23 файлах) делится на 6 компонентов + лист общих типов (`sesstypes`); ядро `BatchStore` (73 метода, 22 файла) — на 3 компонента + корневой store; ядро `RunQueuePump` (22 метода, 9 файлов) — на 3 компонента. Компонент = новый тип в подпакете; ядро хранит ссылку и однострочные делегаты; публичный API `session.X` сохраняется точно (type-alias + делегат), внешние слои меняют только строки импорта.
2. КОНЕЧНОЕ состояние: ВНУТРИ `internal/session` нет ни одного каталога выше 7 нетестовых элементов; постоянных строк в `dir_entries_allowlist` не остаётся. Временные строки с падающим расписанием (единица — ЭЛЕМЕНТЫ каталога: нетестовые файлы + подкаталоги с кодом, правило 11): `internal/session/service` 34→34→21→15→8→6→4 (удалить), `internal/session/batch/sessionbatch` 26→24→23→18→21→8→7 (удалить). Вне пакета: `internal/procinfo 12` (kit, вопрос В2).
3. Единственная общая память — реестры `trackedGroups`/`trackedJobs` — переезжает в новый лист `kit/sys/proctrack` (S1, решение В12 плана), временный шов `RegisterTrackedJobTerminator` исчезает (`proctrack` импортирует `procinfo`).
4. Ребро base→run из `prereq.py` — комментарии (проверено: `session.go:388,396`, `session_runqueue.go:491`); кодовый цикл держится на guard допуска мутаций (уезжает в `sessguard`, EX4) и fusion-файле `batch_commit.go` (делится: тип `BatchCommitIdentity` → `sessguard`, протокол функций → `sessionbatch`, EX4/EX6).
5. Метод-компонент сохраняет поведение: тело функции переносится без изменений (замена только приёмника `s`→`c` и полей на ссылки компонента); каждый порт ≤15 имён; для критичных путей (batch commit, допуск финализации, stop fence, drain) полоса доказывает чистоту побайтовым diff перенесённых тел и серийными тестами оркестратора (§4.4, §5).

## 1. Текущее состояние

### 1.1. Объём

- 297 файлов: 130 рукописных нетестовых `.go`, 167 тестовых (128 белых `package session`, 39 внешних `package session_test`); ~24.7k строк кода и ~39k тестов (И§1). Подкаталогов нет. Элементов: 133 (храповик).
- Лимит 1000 строк не нарушен; у предела (В7): `lock.go` 976, `childgroup_registry_unix.go` 976. Далее: `run_queue_entry_exec.go` 786, `run_queue_drain_session.go` 763, `run_queue_pump.go` 670, `session.go` 618, `session_runqueue.go` 618, `batch_reaction.go` 590.

### 1.2. Кластеры (entangle.py, порог 300)

| Кластер | файлов | код | тесты | 90д |
|---|---|---|---|---|
| batch | 76 | 6566 | 9035 | 67 |
| session | 40 | 4814 | 4453 | 72 |
| run | 14 | 4374 | 627 | 44 |
| finalization | 55 | 2757 | 4735 | 25 |
| lock | 10 | 1609 | 1436 | 15 |
| childgroup | 3 | 1008 | 787 | 10 |
| host | 10 | 724 | 1026 | 14 |
| wake | 6 | 591 | 633 | 9 |
| agent_* | 6 | 493 | 302 | 5 |
| role | 3 | 448 | 221 | 1 |
| track | 10 | 422 | 307 | 10 |

Мелкие: mcp_output 220, notice 111, starttime 109, proc 64, file 26, retention 18; kill уже в `procinfo` (волна 1). Горячие — `session` (72) и `batch` (67).

### 1.3. Циклы `prereq.py` и проверка кодом

Черновик base/batch/finalization/proc/run: один цикл `base<->batch<->finalization<->proc<->run`; рёбра: batch→base 34, finalization→base 19, base→finalization 14, base→proc 11, base→batch 6, batch→finalization 7, finalization→batch 10, run→base 8, base→run 4, run→proc 2, run→finalization 2, finalization→proc 5, finalization→run 1.

Кодовые факты (скрипт считает и комментарии; здесь только код):
- **base→run — только комментарии** (`session.go:388,396`; `session_runqueue.go:491`; `session_purge_queue.go` — комментарии). Кодовых ссылок нет; рано исчезает.
- **base→batch/finalization в коде** — guard: `session_inject.go` (5 мест), `session_notice_store.go` (4), `session_orphan_outbox.go` (2) вызывают `admitFinalizationMutation`/`batchCommitReleaseError` и держат `*batchFinalizationAdmission`; `session_runqueue.go:35` — поле `finalizationLease *agentFinalizationQueueLease`; `batch_commit.go:138,375` вызывают `admitBatchCommitOwners(store, ...)`.
- **Fusion:** `BatchCommitIdentity` (`batch_commit.go:25-51`) держит `finalizationService *service` и `finalizationAuthorities []*AgentFinalizationAuthority` (неэкспортируемые поля заполняет только `service.WithAgentFinalizationAuthorities`, `finalization_authority.go:70`); `admitBatchCommitOwners` сверяет `store.writer != service.db` (`batch_commit_admission.go:52`) и зовёт `service.hasFinalizationScope` (`:90`).
- **Методо-связанность:** `*service` 100 методов/23 файла; `*BatchStore` 73/22; `*RunQueuePump` 22/9; `*SessionNoticeStore` 14; `*WakeScheduleStore` 12; `*HostStore` 11; `*AgentFinalizationStore` 10; `*BatchLiveReader` 8; `*BatchTimeoutCheckin` 7. Метод переезжает только вместе с типом-владельцем — отсюда компонентная экстракция (§4).
- **Свободные от ядер файлы:** весь proc-кластер (lock/childgroup/track/starttime), host 5, wake 3, notice 3, mcp_output, session_activity, session_lock_fact, session_child_question+child_question_format, session_driver, retention_purge, file_lock_bridge, errno_unix, role_contract.go, agent_finalization.go, факты/debt/seal/notify/retire/candidate финализации, `run_queue_drain_ledger/classify/row_context`, `batch_read*`, `batch_timeout_checkin.go`, `*_doc.go`.

### 1.4. Глобальные переменные

| Переменная | Где | Кто читает | Решение |
|---|---|---|---|
| `trackedGroups/Mu/Snapshot` | `track_unix.go:36-38` | `finalization_facts_holds_tracked_unix.go` (len), `track_stop_unix.go` | → `proctrack`, API `TrackedProcessCount()` (S1) |
| `trackedJobs/Mu` | `track_windows.go:32-33` | `finalization_facts_holds_tracked_windows.go` (len), `track_stop_windows.go` | то же (S1) |
| `processTreesStopping` | `track_stop.go:8` (atomic; подтверждено) | `track_unix.go:59,91`, `track_windows.go:114` | с реестром в `proctrack` (S1) |
| `agentFinalizationAuthoritiesKey`, `agentFinalizationQueueLeaseKey` | `finalization_context.go`, `finalization_queue_lease.go` | guard/finqueue | едут с guard/finqueue, не экспортируются |
| `Err*`, `pdcBatchRootFenceSQL`, `psfOwnerStopSQLPredicate` | `batch_store.go:16-29`, `batch_stop_fence.go:19` | `batch_stop.go:39,49` | иммутабельные; предикат остаётся с `sessionbatch` (S6, испр.) |

### 1.5. init(), ОС-файлы, embeds

- `init()` два: `track_windows.go:14` (`RegisterTrackedJobTerminator(terminateTrackedJob)` — исчезает в S1); `batch_store.go:34` (4× `db.PdbBatchRegisterSQL`). Реестр SQL потребляется `PdaBatchEnsure` лениво (`internal/db/pda_batch_prepare_test.go:TestPdaBatchEnsurePreparesOnce`), порядок init не меняется; `sessionbatch` импортируется агентом — регистрация гарантирована.
- ОС-файлов 15: `lock_unix/lock_windows`, `childgroup_registry_unix/_windows`, `track_unix/track_windows`, `track_stop_unix/_windows`, `starttime_linux/starttime_other_unix`, `host_lock_unlink_unix/_windows`, `finalization_facts_holds_tracked_unix/_windows`, `errno_unix`. Теги: `lock_unix.go:1` и `errno_unix.go:1` — оба `//go:build !windows` (не `unix`; исправлено по ревью) — теги идентичны, склейка безопасна на всех GOOS. Пары едут вместе; track — в `procinfo` целиком; `parent_other/parent_windows` heartbeat — MV8.
- `go:embed` — нет.

### 1.6. Внешние потребители

Фактические импортёры: agent, agent/cliprovider, agent/codexprovider, agent/tools, agent/tools/mcp, app, app/itest, cmd, server, server/r15bfixture, nettransport, sdk. `shell` и `hooks` НЕ импортируют (волна 1; проверено grep — данные `analyze.py` устарели). Поверхность: 232 пакетных идентификатора + 145 методов/констант; топ: `Session` 438, `Service` 193, `NewBatchStore` 146, `RoleContract` 128, `BatchReservationNode` 118, `SessionAgentCallData` 116, `NewService` 116, `TryAcquireSessionLock` 98, `Todo` 91, `ModelSlotUpdate` 80.

| Слой | Кластеры |
|---|---|
| agent | ядро, batch (17 файлов API), finalization (22), run, host, wake, notice, mcp_output, track(→proctrack), childgroup, lock, role, drain |
| app | ядро (13), batch (9), run (8), host(+testhook), wake, notice, finalization, stores |
| cmd | ядро (8), batch (8), host(+testhook), wake, notice, track(→proctrack), childgroup, lock, lock-facts |
| server | ядро (5), batch (8), run (2), host(+testhook), role, finalization |
| sdk | ядро, финализация (notify_gate) |
| nettransport | 1 ссылка из `session.go` |
| cliprovider/codexprovider/tools/mcp | ядро, lock, track(→proctrack), childgroup, host, mcp_output |

### 1.7. Зависимости вниз

`internal/session` импортирует только `db`, `message`, `pubsub`, `platform`, `filelock`, `jobbatch`, `procinfo`, `proctrack` — слои store/kit. После разрезки все новые пакеты внутри группы `session` — порядок `kit < store < session < config < agent < app < serve` сохраняется. `heartbeat` импортирует только `procinfo`; `jobbatch` — только stdlib.

### 1.8. Тесты и помощники

- 128 белых / 39 внешних (39 подтверждено переписью); 82 идентификатора помощников пересекают границы (prereq).
- Дубль (ПОПРАВЛЕНО 2026-11-15 по доказательству агента SM4): пара `internal/app/itest/session_support_test.go` (409) ↔ `internal/session/run_queue_test_support_test.go` (454) общих объявлений НЕ имеет (множества дополняют друг друга). Настоящие семейства дублей: (1) `itest/session_support_test.go` ≈ `pump_fake_clock_test.go`, `pump_failure_report_test.go`, `pump_queue_report_test.go` (`awaitPump`/`awaitPumpFor`: `pump_fake_clock_test.go:254,263` против `session_support_test.go:248,257`, тела и комментарии совпадают); (2) `run_queue_test_support_test.go` ≈ четыре сценария `itest` (`run_queue_round2`, `p575`, `p588`, `p592`); (3) третья копия `limitParallel`: `run_queue_test_support_test.go:74` и `parallel_gate_test.go:65`. Общая часть всех трёх семейств поднимается в `run/sessiontest` (S8), объём правки около 30 файлов тестов, а не 2.
- Пять сценариев в `internal/app/itest`: `p575_background_outcome_handoff`, `p588_partial_drain`, `p592_cross_row_identity`, `p624_drain_visibility`, `run_queue_round2`.

## 2. Целевое дерево (конечное состояние)

### 2.1. Принципы

1. **Публичный API:** путь `internal/session/service`, имя пакета `session`. Сохранение достигается механикой «alias + делегат» (S9): каждый тип, на который ссылаются снаружи как `session.X`, объявляется в `sesstypes` (или в пакете-владельце, не импортирующем ядро) и реэкспортируется ядром как `type X = ...`; каждый метод интерфейса `Service` и экспортируемых ядер (`BatchStore`, `RunQueuePump`) получает однострочный делегат. Все ~377 обращений `session.X` остаются валидными; внешние слои меняют только строки импорта.
2. **Решение Г1 (записано):** постоянных исключений «>7 элементов» нет. Компонентная экстракция — полосы D2 (§5 EX). Храповик — только временный, число только падает, после каждой EX-полосы обновляется (§2.2, FIN).
3. **Компонент:** новый тип в подпакете-листе; хранит свои поля и свои мьютексы; зависимости получает при конструировании (db, порты-интерфейсы ≤15 методов); назад в ядро не звонит. Ядро держит ссылку и делегаты.
4. **Контейнеры без кода:** `proc`, `run`, `batch`, `state`, `service/components`, `run/components`, `batch/components` — только подкаталоги.
5. **Тесты едут с компонентом** (§2.5); сценарии на несколько модулей — `internal/app/itest`.
6. Отклонение от черновика И§9.2: глубина 3-4 (правило 7 элементов), track→`proctrack` (S1), guard/очередь финализации — компоненты ядра, а не соседний пакет.

### 2.2. Дерево и КОНЕЧНОЕ число элементов (все ≤7 внутри internal/session)

```
internal/session/                    7: service, proc, run, batch, state, jobbatch, heartbeat
├── service/        package session  4: session.go, session_fork.go, session_delegates.go, components/
│   └── components/                  7: sesstypes, sessrows, sessqueue, sessagent, sessguard, finqueue, sessmutate
│       ├── sesstypes/               5 файлов: types_session, types_runcall, types_role, types_notice, types_batch
│       ├── sessrows/                5: update.go, read.go, lifecycle.go, cost.go, purge.go
│       ├── sessqueue/               3: queue_crud.go, credential_store.go, execution_store.go
│       ├── sessagent/               4: depth.go, model_role.go, role_contract_store.go, root_role_contract_store.go
│       ├── sessguard/               7: admission.go(+BatchCommitIdentity), mutation.go, ancestors.go, finalization_tx.go, batch_finalization_tx.go, authority.go, context.go(+finalizationScoped)
│       ├── finqueue/                7: queue.go, enqueue.go, restore.go, prepare.go, lease.go, failure.go, ingress_helpers.go
│       └── sessmutate/              2: inject.go, orphan_outbox.go
├── proc/                            2: sessionlock 7 файлов, sessioncgroup 5
├── run/                             3: sessionrun 3, sessiontest 3, components 3
│   └── components/                  3: runadmission 1 (admission.go), runexec 1 (entry_exec.go), rundrain 5 (drain_session.go, drain_ledger.go, row_context.go, orphan_drain.go, drain_classify.go)
├── batch/                           7: sessionbatch 7, components 3, finstore 1, finfacts 5, finholds 5, findebt 3, finterm 5
│   └── components/                  3: batchreaction 1 (reaction.go), batchdeliver 5 (delivery.go, pull.go, history.go, consumers.go, adapters.go), batchrecover 5 (recovery.go, recovery_candidates.go, recovery_doc.go, retention.go, retention_purge.go)
├── state/                           5: sessionnotice 5, sessionhosts 6, sessionwake 3, mcpoutput 1, sessionfacts 2
├── jobbatch/                        приезд C; 20 нетестовых файлов + lab/24 — сплит MV7 (обязателен по решению Г1: 3 подпакета ≤7)
└── heartbeat/                       приезд C; 10 нетестовых файлов — сплит MV8 (обязателен: 2 подпакета по 5)
```

Сводка элементов: корень 7; service 4 (3 файла + каталог); service/components 7; листья сервиса 5/3/4/7/7/2/5; proc 2 (7, 5); run 3 (3, 3, 3); run/components 3 (1, 1, 5); batch 7 (7, 3, 1, 5, 5, 3, 5); batch/components 3 (1, 5, 5); state 5 (5, 6, 3, 1, 2). Всё ≤7. Каталогов с кодом >7 внутри `internal/session` нет.

Временные строки храповика (решение Г1: только падающие, с расписанием; после каждой EX-полосы FIN правит файл):
```
# ВРЕМЕННО: компонентная экстракция ядра session (D1-session §5, EX-полосы). Единица — ЭЛЕМЕНТЫ каталога
# (нетестовые файлы + подкаталоги с кодом); создание components/ добавляет +1 (правило 11). Число только падает.
internal/session/service 34              # 34 (MV1) → 34 (EX1: −role_contract.go, +components/) → 21 (EX4: −14 guard/finqueue, +session_delegates.go) → 15 (EX2: −6) → 8 (EX3: −7) → 6 (EX5: −2) → 4 (EX6: −batch_commit/-doc; строку удалить)
# ВРЕМЕННО: компонентная экстракция BatchStore (D1-session §5, EX6). Элементы = файлы (подкаталогов внутри sessionbatch нет).
internal/session/batch/sessionbatch 26   # 26 (MV3) → 24 (cli/api −2) → 23 (reaction −1) → 18 (recover −5) → 21 (+batch_commit/+doc/+delegates приходят ДО deliver) → 8 (deliver −13) → 7 (вклейка stop+control; строку удалить; пик 21 < 26, ниже 7 не опускается)
```
Пик `service/components` — ровно 7 детей (новый компонент требует переработки дерева). `procinfo` остаётся с 7 файлами, реестры tracked уходят в `proctrack` (5 файлов): решение В12 плана, строки храповика нет.

Проверка имён (правило 2; `git grep -h "^package "`): совпадений с новым списком нет — `sessionlock`, `sessioncgroup`, `sessionrun`, `sessiontest`, `runadmission`, `runexec`, `rundrain`, `sessionbatch`, `sessionbatchread`, `batchreaction`, `batchdeliver`, `batchrecover`, `finstore`, `finfacts`, `finholds`, `findebt`, `finterm`, `sesstypes`, `sessrows`, `sessqueue`, `sessagent`, `sessguard`, `finqueue`, `sessmutate`, `sessionnotice`, `sessionhosts`, `sessionwake`, `mcpoutput`, `sessionfacts` — уникальны и осмысленны без пути; `session` остаётся у ядра. `lab`→`joblab` — MV7 (план §2).

### 2.3. Публичный API (механика S9)

| Пакет | Экспорт (сегодня `session.X`), сохраняемый alias/делегатом ядра |
|---|---|
| sesstypes | `Session`, `Todo(+Status*)`, `ModelSlotUpdate`, `CallTreeActivity`, `Owns`, `ErrForeignWorkspace`, `RunQueueEntry`, `QueueLease` (бывш. `agentFinalizationQueueLease`, данные), `ModelCfg`, `CallOptionsSpec(+Version)`, `RunAllowlistSpec`, `FolderScope(Spec/Entry)`, `FileOp`, `SessionAgentCallData`, `Session(Credential/Execution)Requirement*`, `IsSession{Credential,Execution}RequirementCallData`, `RoleContract(+Version)`, `NoticeKind*`, `ChildQuestion`, `ParseChildQuestionNotice`, `PulledNotice`, `SessionNoticeRow`, `PendingInject`, `PendingNoticeDebt`, `BatchReservation(Node)`, `BatchProjection`, `BatchSnapshot`, `BatchControl*`, `BatchLive*`, `BatchPendingDebt`, `BatchDebtNodeRef`, `BatchDebtSnapshot`, `BatchRecovery*`, `BatchHostProbe`, `DrainResult` и прочие внешние типы batch |
| service (ядро) | `Service`, `NewService(+WithWorkspace/WithReader)`, `AgentFinalizationService` (интерфейс, `finalization_authority.go:119`), все alias из sesstypes, guard-alias (`AdmitFinalizationMutation`, `FinalizationAdmission`, `JoinReleaseError`, `ValidateFinalizationMutationTx`, `FinalizationScoped`, `BatchCommitIdentity`, `AgentFinalizationAuthority`, `AgentFinalizationContext`, `Capture/WithAgentFinalizationContext`, `ErrAgentFinalization*`), однострочные делегаты всех ~60 методов интерфейса `Service` |
| sessrows | компонент `RowStore` (внутренний; наружу — делегаты ядра) |
| sessqueue | компонент `QueueStore`; наружу `session.EnqueueRunQueueEntry` и др. — делегаты |
| sessagent | компонент `AgentStateStore`; наружу `AgentDepthStore`, `AgentModelRoleStore`, `SessionRoleContractStore`, `RootRoleContractStore` (интерфейсы остаются в ядре, `var _` проверки) |
| sessguard | `Guard` (конструктор New(writer, reader *sql.DB)); наружу через alias ядра |
| finqueue | компонент `Queue`; наружу `Prepare/RestoreAgentFinalizationQueue` — делегаты |
| sessmutate | компонент `MutationStore` |
| sessionlock | `TryAcquireSessionLock(+WithOptions)`, `TryHoldSessionLockShared`, `InspectSessionLock`, `SessionLock(Path/Stem)`, `SessionLockBusyError`, `LockHeld/LockStaleDuration/LockOption`, `SharedLockProbe`, `MaxPidFallbackAge` (определение `lock.go:110` — испр. по ревью: уезжает в sessionlock, НЕ в procinfo) |
| sessioncgroup | `RegisterChildGroup`, `UnregisterChildGroup`, `KillRegisteredChildGroups`, `ChildGroupSweepResult` |
| sessionrun | `RunQueuePump` (Start/Stop/DrainSessionNow остаются методами pump), `RunQueuePumpConfig`, `NewRunQueuePump`, `Coordinator`, `RunQueuePumpInterval`, `RunQueueMaxAttempts`, `PumpTicker` |
| runadmission | `Admission` (+AdmissionForTest-методы); `AdmissionEntryHandleForTest` |
| rundrain | `RowLedger` (экспорт `rowLedger`), `WithRunQueueRowID`, `RunQueueRowIDFrom`, `ErrOutstandingRunQueueEntry` |
| sessionbatch | `BatchStore` (Reserve/Project/Read/RootOwner/Control/Stop остаются), `NewBatchStore(+WithReader)`, `NewBatchCommitHook`, `ErrBatch*`, делегаты 50 перенесённых методов |
| sessionbatchread | `BatchLiveReader`, `NewBatchLiveReader`, `BatchTimeoutCheckin`, `NewBatchTimeoutCheckin`, `SetBatchLiveServedSeamForTest`, новые `CLIViews`/`APIViews` (методы BatchCLI*/BatchAPI*), делегаты на BatchStore |
| finstore | `AgentFinalizationStore`, `NewAgentFinalizationStore`, `AgentFinalizationIdentity`, `AgentFinalizationRecord`, `ErrAgentFinalizationChanged` |
| finfacts/finholds | `Finalization*Facts`, `NewFinalizationPhysicalHoldsFacts*`, `FinalizationHoldsProvider`, `CountFinalizationTrackedProcessHolds` |
| findebt | `FinalizationDebtStore`, `NewFinalizationDebtStore`, `AgentFinalizationDebtSettler` |
| finterm | `AgentFinalizationCandidate*`, `FinalizationNotifyGate`, `DefaultFinalizationTerminalGate`, `NewPollingFinalizationTerminalGate`, `FinalizationSeal*` |
| sessionnotice | `SessionNoticeStore`, `NewSessionNoticeStore`, notice-хелперы (типы — в sesstypes) |
| sessionhosts | `HostStore`, `NewHostStore`, `HostLockPath`, `HostLockStatus`, `HostStatus*`, `ProbeHost*`, `IsOwnHostID`, `HostsDir`, `RegisterHost`, `SessionDriver`, `ErrDriverMarkerUnavailable`, `ErrSessionDrivenElsewhere`, `ReleaseRetainedHostLocksForTest` |
| sessionwake | `WakeScheduleStore`, `NewWakeScheduleStore(+WithWorkspace)`, `CreateWakeScheduleParams`, `WakeKind*`, `WaitSchedule`, `ErrWakeSchedule*` |
| mcpoutput | `MCPOutputStore`, `NewMCPOutputStore`, `MCPOutputLimit` |
| sessionfacts | `ActivityVerdict`, `ClassifySessionActivity`, `Activity*`, `InspectSessionLockFact`, `LockFact` (`CallTreeActivity` — sesstypes; sessionfacts его не использует — проверено grep) |
| proctrack (kit, новый лист `kit/sys/proctrack`) | `TrackProcessTree`, `UntrackProcessTree`, `KillAllTrackedTrees`, `StopTrackedProcessTrees`, `StartProcessUnlessStopping` (определение `track_stop.go:11` — подтверждено), `TrackedProcessCount` (новое) |

### 2.4. Полная таблица файлов (нетестовые, 1:1; испр. по ревью)

Формат: файл → конечный владелец (через EX-полосу; до неё — место MV-полосы). «В7» — разрез файла; «вклейка» — присоединение к файлу того же пакета; «блок→sesstypes» — типы из файла уезжают в sesstypes, файл растворяется в компонент.

**service/ (конечные 3 + delegates):** `session.go` (структура service, интерфейс `Service`, конструкторы; типы Session/Todo/CallTreeActivity → sesstypes, EX1); `session_fork.go` (остаётся: tx+guard- coupled); `session_delegates.go` (НОВЫЙ, EX4: однострочные делегаты ~60 методов `Service` + alias-блок S9).
**→ sesstypes (EX1, 5 файлов):** блоки из `session.go` (Session/Todo/ModelSlotUpdate/CallTreeActivity/PurgedQueuedWork), `session_workspace.go` (`Owns`, `ErrForeignWorkspace`, folder-scope-валидация), `session_runqueue.go` (типы `RunQueueEntry`, `QueueLease`, `ModelCfg`, `CallOptionsSpec(+Version)`, `RunAllowlistSpec`, `FolderScopeSpec/Entry`, `FileOp`, `SessionAgentCallData`), `session_credential_requirement.go`+`session_execution_requirement.go` (типы + классификаторы `IsSession*CallData`), `role_contract.go` (целиком) → types_runcall/types_role; + новые types_notice (из `session_notice_store.go`/`session_child_question.go`/`notice_pull.go`: `NoticeKind*`, `ChildQuestion`, `ParseChildQuestionNotice`, `PulledNotice`, `SessionNoticeRow`, `PendingNoticeDebt`) и types_batch (из `batch_reaction.go`: `BatchPendingDebt`, `BatchDebtNodeRef`, `BatchDebtSnapshot`, `BatchReactedFailedText`; из `batch_store.go`/`batch_control.go`/`batch_read*`/`batch_recovery*`/`batch_stop.go`: `BatchReservation*`, `BatchProjection`, `BatchSnapshot`, `BatchControl*`, `BatchLive*`, `BatchRecovery*`, `BatchHostProbe`; из `run_queue_drain_ledger.go`: `DrainResult`).
**→ sessrows (EX2, 5 файлов):** `session_update.go` (21 метод), `session_read.go` (7), `session_lifecycle.go` (12: Create*/Delete/bindWorkspace/AgentTool-хелперы), `session_cost_subtree.go` (4), `session_purge_queue.go` (1); `session_workspace.go` растворяется (`ownsArgs` → метод `Scope` в read.go).
**→ sessqueue (EX3, 3 файла):** `session_runqueue.go` (12 методов CRUD; типы уже в sesstypes), `session_credential_requirement.go` (2 store-метода), `session_execution_requirement.go` (3 store-метода).
**→ sessagent (EX3, 4 файла):** `agent_depth.go` (4), `agent_model_role.go` (2), `role_contract_store.go` (2), `root_role_contract_store.go` (2).
**→ sessguard (EX4, 7 файлов):** `finalization_authority.go` (3 метода + тип `Authority`; поле `service *service` → `db *sql.DB` — доказано: используются только `a.service.db`, `finalization_authority.go:39,42,54`; `hasFinalizationScope` — тело читает только `s.readDB/s.db`, `:96-117`), `finalization_context.go` (+ сюда one-liner `finalizationScoped` из ingress_helpers), `finalization_ingress_helpers.go` → остаётся в finqueue (ниже), `finalization_mutation.go`, `finalization_ancestors.go`, `finalization_tx.go`, `batch_finalization_tx.go`, `batch_commit_admission.go` + сюда переносится объявление `BatchCommitIdentity` из `batch_commit.go:25-51`.
**→ finqueue (EX4, 7 файлов):** `finalization_queue.go`, `finalization_queue_enqueue.go` (2 метода), `finalization_queue_restore.go` (2), `finalization_queue_prepare.go` (1), `finalization_queue_lease.go` (1 + `ErrAgentFinalizationLeaseChanged`), `finalization_queue_failure.go` (+`recordFinalizationQueueFailure` — используется rundrain), `finalization_ingress_helpers.go` (2 метода + payload-типы; `finalizationScoped` уезжает в sessguard).
**→ sessmutate (EX5, 2 файла):** `session_inject.go` (6), `session_orphan_outbox.go` (7).
**proc/sessionlock (8→7):** `lock.go` (В7-разрез `lock.go`+`lock_acquire.go`; вклейка `file_lock_bridge.go`), `lock_inspect.go`, `lock_heartbeat.go`, `lock_handoff.go`, `lock_unix.go` (вклейка `errno_unix.go`; оба `//go:build !windows` — испр.), `lock_windows.go`.
**proc/sessioncgroup (4→5):** `childgroup_registry_unix.go` (В7-разрез на два), `childgroup_registry_windows.go`, `starttime_linux.go`, `starttime_other_unix.go`.
**proctrack (5, новый лист `kit/sys/proctrack`):** `track_unix.go`, `track_windows.go`, `track_stop.go`, `track_stop_unix.go`, `track_stop_windows.go`.
**batch/sessionbatch (конечные 7):** `batch_store.go` (структура, конструкторы, init()-регистрация SQL, Reserve/ReserveIdentity/Project/Read/beginReadTx/LiveRootsForHost/CreateControl/PendingControls/AnswerControl/DeleteRoot + `RootOwner` из batch_commit.go, SM2), `batch_commit.go` (прибытие из ядра, EX6: `batchCommitTransition/Submit/Project`, `NewBatchCommitHook`, хелперы; тип `BatchCommitIdentity` уже в sessguard), `batch_commit_doc.go` (прибытие), `batch_stop_fence.go` (`psfOwnerStopSQLPredicate` — остаётся здесь, испр. S6: пользователи `batch_store.go:29`, `batch_stop.go:39,49`; batch_commit.go предикат НЕ использует), `batch_stop.go` (+ вклейка `batch_control.go`), `batch_child_bind.go`, `session_delegates.go` (НОВЫЙ).
**→ batchreaction (EX6, 1):** `batch_reaction.go` (15 методов; типы долга — в sesstypes).
**→ batchdeliver (EX6, 13 исходных → 5 файлов):** delivery.go (=`batch_delivery_store.go`+`batch_ack.go`+`batch_inline.go`+`batch_tool_result.go`), pull.go (=`batch_pull.go`+`batch_question.go`+`batch_outbox_doc.go`), history.go (=`batch_history_snapshot.go`+`batch_history_reset.go`+`batch_history_rerun.go`), consumers.go (=`batch_consumers.go`+`batch_consumers_doc.go`), adapters.go (=`batch_adapters.go`). Методы: 19.
**→ batchrecover (EX6, 5):** `batch_recovery.go` (4), `batch_recovery_candidates.go` (6), `batch_recovery_doc.go`, `batch_retention.go` (5), `retention_purge.go`.
**→ sessionbatchread (+2 файла = 7):** `batch_read.go`, `batch_read_doc.go`, `batch_read_level.go`, `batch_read_live.go`, `batch_timeout_checkin.go` + НОВЫЕ `cli_views.go`/`api_views.go` (компоненты `CLIViews`/`APIViews`: методы `BatchCLI*` из `batch_cli_store.go` (4), `BatchAPI*` из `batch_api_store.go` (3); на `BatchStore` остаются однострочные делегаты).
**→ runadmission (EX7, 1):** `run_queue_admission.go` (4 метода pump: admitSession/AdmitSessionForTest/AdmitSessionObservedEntryForTest/SetTestAfterAdmissionRefusalForTest; типы `admissionEntry` :26, `admissionOutcome` :161 — ИСПРАВЛЕНО S4: исход остаётся в admission, не в drain_ledger).
**→ runexec (EX7, 1):** `run_queue_entry_exec.go` (executeEntrySync + хелперы).
**→ rundrain (EX7, 5):** `run_queue_drain_session.go` (остаток ~733 после релокации `DrainSessionNow` в pump, SM3), `run_queue_drain_ledger.go` (+экспорт `RowLedger`), `run_queue_row_context.go`, `run_queue_orphan_drain.go` (3 метода pump: drainOrphanOutbox/processOrphanOutboxEntry/recordOrphanOutboxDrainFailure — ИСПРАВЛЕНО: в рев. 1 файл был не назначен), `run_queue_drain_classify.go`.
**sessionrun (конечные 3):** `run_queue_pump.go` (вклейки `run_queue_clock.go`, `run_queue_testsupport.go`, релокация `DrainSessionNow` — SM3; структура/cfg/конструкторы/Start/Stop/run/tick/DrainSessionNow), `run_queue_lifecycle.go`, `run_queue_entry_dispatch.go`.
**run/sessiontest (3 новых файла):** фикстуры из `run_queue_test_support_test.go` и `session_support_test.go` (S8).
**state/sessionnotice (5):** `session_notice_store.go` (445), `notice_pull.go`, `notice_row_ids.go`, `session_child_question.go`, `child_question_format.go` (типы — sesstypes).
**state/sessionhosts (6):** `host_store.go`, `host_lock.go`, `host_lock_unlink_unix.go`, `host_lock_unlink_windows.go`, `host_lock_testhook.go`, `session_driver.go`.
**state/sessionwake (3):** `wake_schedule_store.go`, `wake_schedule_fire.go`, `wake_schedule_await_tasks.go`.
**state/mcpoutput (1):** `mcp_output.go`.
**state/sessionfacts (2):** `session_activity.go`, `session_lock_fact.go`.

Счёт исходных 130: service-исход 34 (2 остаются + 32 расходятся) + lock 8 + cgroup 4 + proctrack 5 + sessionbatch-исход 26 + run 12 + state 17 + finstore 1 + finfacts 5 + finholds 5 + findebt 3 + finterm 5 = 130. Все назначены, включая `run_queue_orphan_drain.go` (пропуск рев. 1 исправлен).

### 2.5. Тесты — назначение (едут с компонентом)

| Префикс/принадлежность | Каталог |
|---|---|
| `session_update/read/fork/lifecycle/workspace/cost/purge/inject/orphan` (белые), `p782`, `pdc_batch_tx_message` | EX2/EX5: с `sessrows`/`sessmutate` (guard-тесты inject/orphan — с sessguard-фикстурой через ядро) |
| `session_runqueue*`, `session_credential*`, `session_execution*`, `agent_depth`, `agent_model_role`, `role_contract*`, `root_role_contract*` | EX3: с `sessqueue`/`sessagent` |
| `finalization_authority/context/ingress/queue*/lease/mutation/ancestors/tx`, `batch_commit_admission*`, `batch_commit_authority*` | EX4: с `sessguard`/`finqueue` |
| `batch_reaction*`, `batch_pull*`, `batch_question*`, `batch_tool_result*`, `batch_history*`, `batch_consumers*`, `batch_ack/inline/delivery/adapters` | EX6: с `batchreaction`/`batchdeliver` |
| `batch_recovery*`, `batch_retention*`, `pda_batch_prepare_equiv`, `pdc_batch` SQL-эквивалентность | EX6: с `batchrecover`/корнем sessionbatch (fence/commit) |
| `p587_aba`, `p607_*`, `p613_typed_admission` | EX7: с `runadmission` |
| тесты `entry_exec`/worker-пула (`p1_4_bounded_worker_pool`, `p616_sync_exec_panic`) | EX7: с `runexec` |
| `p0_*`, `p1_1/p1_2/p1_3`, `p348*`, `p350*`, `p421*`, `p550-p553`, `p575(late)`, `p578`, `p589`, `p591`, `p593`, `p610`, `p631`, `pump_*`, `run_queue_gone`, `parallel` | EX7: с `rundrain`/`sessionrun` по владельцу идентификаторов |
| `batch_cli*`, `batch_api_*` | EX6: с `sessionbatchread` (cli/api views) |
| `session_row_fields`, `origin`, `legacy_import_drain_external` (В8: по поведению — itest либо ядро) | service |
| `lock_*`, `childgroup_*`, `starttime*`, `track_*`, `host_*`, `wake_*`, `mcp_*`, `notice*`, `session_child_question*`, `session_activity`, `session_lock_fact`, `finalization_facts*`, `finalization_candidate/seal/notify/retire/debt*`, `p1142`, `p556` | proc/sessioncgroup/procinfo/state/fin-пакеты — с владельцем (как в §2.4) |
| сценарии agent+app | `internal/app/itest` (после S8 импортируют `run/sessiontest`) |

### 2.6. Граф зависимостей (листья; все рёбра вниз, циклов нет)

```
sesstypes ← все компоненты сервиса; sessionnotice, sessionwake, sessionbatch(sessionbatchread), rundrain, runadmission, runexec, sessiontest. sessionhosts sesstypes НЕ импортирует (host-файлы и session_driver.go не ссылаются на типы ядра — проверено grep)
proctrack ← sessioncgroup, finholds, [внешние]; proctrack → procinfo
sessionlock ← sessioncgroup, sessionfacts, [внешние]
sessionhosts ← finfacts, [внешние]
finstore ← sessguard, finqueue, sessionbatch, finfacts, finholds, findebt, finterm, [внешние]
sessguard ← finqueue, sessmutate, ядро(делегаты), sessionbatch(через ядро S2 — прямого импорта нет), [внешние через ядро]
finqueue ← sessqueue, sessmutate, rundrain(recordFinalizationQueueFailure), ядро
sessrows ← sessagent (CreateTaskSession), ядро
ядро service ← ВСЕ сателлиты (state/*, sessionbatch*, fin*) и внешние слои; proc ядро НЕ импортирует (проверено grep по кодовым ссылкам)
sessionrun → runadmission, runexec, rundrain; sessionrun ← sessiontest
rundrain → runadmission, finqueue; runexec → runadmission; rundrain ⊂ components
sessionbatch → ядро (guard-API 5 имён, S2), → finstore, → jobbatch, → sesstypes; sessionbatchread → sessionbatch(порты), sesstypes
batchreaction/batchdeliver/batchrecover → sessionbatch-порты (Reader/Guard), sesstypes, message
jobbatch ← sessionbatch, ядро(batch_commit), [внешние]
```

Ключевое правило ацикличности: пакет, типы которого ядро реэкспортирует alias-ом, НЕ импортирует ядро (проверяется grep-приёмкой каждой EX-полосы). Единственное перекрёстное ребро контейнеров — «run/components → service/components» (`rundrain → finqueue` ради `RecordFinalizationQueueFailure`): направление вниз по группе, легально. Ребра state→batch НЕ существует: `BatchPendingDebt` лежит в sesstypes (types_batch, EX1), а `BatchPendingInclusiveDebtRows` — собственный метод `SessionNoticeStore` (`session_notice_store.go:381`, использует только тип из sesstypes).

## 3. Швы

### S1. Реестр отслеживаемых деревьев → `proctrack` (решение В12 плана)
Без изменений против рев. 1: `track_unix/track_windows/track_stop/track_stop_unix/track_stop_windows` + тесты → новый лист `internal/kit/sys/proctrack` (после C; имя `proctrack`); новый `TrackedProcessCount()`; правка `finalization_facts_holds_tracked_unix/_windows` (len→API); перепись внешних потребителей Track*/Kill*/Stop*/Start* (`StartProcessUnlessStopping` — `track_stop.go:11`); init из `track_windows.go:14` удаляется. Поведение: `proctrack` вызывает `procinfo.KillProcess` напрямую (порт `RegisterTrackedJobTerminator` не нужен, `procinfo` остаётся с 7 файлами); счётчик возвращает тот же len под тем же мьютексом.

### S2. `admitBatchCommitOwners` теряет `*BatchStore` (предпосылка EX4)
Сигнатура `(store *BatchStore, ctx, id, transition, owners)` → `(writer *sql.DB, ctx, id, transition, owners)`; вызовы: `finalization_mutation.go:25` (перестаёт собирать throwaway `NewBatchStore`), `batch_commit.go:138,375` (передают `store.writer`). Единственная store-зависимость guard — сравнение `store.writer != service.db` (`batch_commit_admission.go:52`). Конечная граница **sessionbatch → ядро: 5 имён** (`AdmitFinalizationMutation`, `FinalizationAdmission`, `JoinReleaseError`, `ValidateFinalizationMutationTx`, `FinalizationScoped`) — guard доставляется ядром (alias на sessguard), прямого импорта sessionbatch→sessguard нет.

### S3. Fusion-файл `batch_commit.go` делится (EX4+EX6)
Тип `BatchCommitIdentity` (`batch_commit.go:25-51`) → `sessguard/admission.go` (EX4): неэкспортируемые поля `finalizationService`/`finalizationAuthorities` становятся внутренними для sessguard — их заполняет только `Guard.WithAgentFinalizationAuthorities` (метод переехал с `*service`, `finalization_authority.go:70`); снаружи идентичность конструируется только экспортируемыми полями `Host/Runner/Owner/OwnerFor/AuthorityFor/AdmitOwners` (проверено чтением: внешние сборки используют только их). Функции протокола (`batchCommitTransition/Submit/Project`, `NewBatchCommitHook`, хелперы) → `sessionbatch` (EX6); метод `RootOwner` (`batch_commit.go:475`) → `batch_store.go` (SM2, релокация внутри пакета).

### S4. (`admissionOutcome`) — ЗАМЕНЕНА EX7
В рев. 1 предлагался перенос типа в drain_ledger. По решению о компонентах тип остаётся в `run_queue_admission.go:161` и уезжает с файлом в `runadmission`; направление зависимостей: `rundrain → runadmission`, `runexec → runadmission`, `sessionrun → runadmission`. Обратных рёбер нет (classify читает тип, ничего не пишет в admission).

### S5. `DrainSessionNow` возвращается в pump-файл (SM3)
`run_queue_drain_session.go:13` — единственный метод pump в файле (подтверждено переписью). Тело релоцируется в `run_queue_pump.go`, остаток (~733 строк, свободные функции) уезжает в `rundrain`. Критичный путь drain: тело `DrainSessionNow` не меняется.

### S6. `psfOwnerStopSQLPredicate` остаётся с `sessionbatch` (ИСПРАВЛЕНО по ревью)
Определение `batch_stop_fence.go:19`; пользователи: `batch_store.go:29` (init-регистрация `pdcBatchRootFenceSQL`) и `batch_stop.go:39,49`. `batch_commit.go` предикат НЕ использует (проверено grep) — релокация в ядро и экспорт ОТМЕНЕНЫ; ребро sessionbatch→core сужено до 5 имён (S2). Текст SQL pinned тестом `pda_batch_prepare_equiv_test.go`.

### S7. Потребители guard
`session_notice_store.go` использует те же 5 имён guard (`:168,169,189,191,243,256,318,320,330`) — доставляются ядром (S2); inject/orphan после EX5 импортируют sessguard напрямую (соседи внутри components). Отдельного шва нет.

### S8. Тестовые помощники → `run/sessiontest`
Перечень фикстур прежний (рев. 1): `mockCoordinator`, `gatedCoordinator`, `sequentialGatedCoordinator`, `twoRowFailThenSucceedCoordinator`, `fakePumpClock/Ticker`, `fakeClockService`, `testTimeline`, `setupTestSession`, `enqueueTwoRows`, `awaitPump/For`, `limitParallel/parallelSlots`, `stopPumpLoggingForcedShutdown`, `oneEntrySetupReported`. ПОПРАВКА (2026-11-15): источники и потребители — три семейства из §1.8 (около 30 файлов тестов), а не два файла.

Порядок полосы SM4:
1. Агент строит перепись компилятором, не grep: для каждого кандидата в `sessiontest` — объявления в обоих деревьях (`internal/session`, `internal/app/itest`), список файлов-потребителей, совпадение тел (оракул по мультимножеству строк). Фикстура с несовпадающими телами не поднимается молча: остаётся локальной, строка идёт в отчёт.
2. Белый тест `parallel_gate_test.go` остаётся в пакете `session` (ему нужны неэкспортированные имена); он импортирует `sessiontest` только если все его фикстуры экспортируемы без расширения API ядра. Иначе локальная копия `limitParallel` остаётся и записывается в отчёт как остаток.
3. `sessiontest` не импортирует ядро `session`: только `db`, `dbtest`, `message`, `pubsub`, интерфейсы-порты (если фикстура требует тип ядра — остаётся в потребителе).
4. Инвариант: число функций `Test*` в `internal/session` и `internal/app/itest` до и после равно (до: 684 в `session`); каждая фикстура из перечня объявлена один раз во всём дереве (`grep -rn 'func <имя>'`).
5. Храповик: +2 элемента (`run/`, `run/sessiontest`) — осознанной записью с комментарием оркестратора; падение удалённых дублей учитывается `ratchet_sync.py`.

### S9. Механика «alias + делегат» (сохранение публичного API при экстракции)
- Тип `T`, используемый снаружи как `session.T`, объявляется в `sesstypes` (или в пакете-владельце, не импортирующем ядро); в ядре: `type T = pkg.T`. Вызовы `session.T` валидны без правок.
- Метод интерфейса `Service`, реализация которого уехала в компонент `c`: в ядре `func (s *service) M(args) (rets) { return s.c.M(args) }` — одна строка, тело не дублируется.
- Экспортируемый метод `BatchStore`/`RunQueuePump`, ушедший в компонент: делегат на ядре-владельце типа (`sessionbatch/session_delegates.go`).
- Приёмка каждой EX-полосы: grep внешних слоёв `session\.(T|M)` до/после — множество совпадает; `go vet` валидирует alias-эквивалентность.

### Сводка границ (ориентир ≤ ~15)

| Граница | Имён | Состав |
|---|---|---|
| sessionbatch → ядро | 5 | S2 (испр.: без psf) |
| sessionnotice → sessguard/sesstypes | 5+типы | S7; debt-типы — sesstypes (types_batch), batchreaction не нужен |
| sessqueue → finqueue | 2 | `EnqueueFinalizationAware`, `BindQueueLeaseTx` |
| sessmutate → sessguard/finqueue | 5+2 | S7 + `validateFreshFinalizationIngressTx`, ingress-payload |
| finqueue → sessguard | ≤5 | допуск/валидация enqueue-пути (сверить при EX4) |
| sessguard → finstore | 5 | `NewAgentFinalizationStore`, `ReadAgentFinalization`, `Identity`, `Record`, `ErrAgentFinalizationChanged` |
| sessagent → sessrows | 1 | `CreateTaskSession` |
| sessionrun → runadmission/runexec/rundrain | ≤6 | Admit(+release-типы), ExecuteEntry, Drain-функции |
| runexec → sessionrun-порт `ExecHost` | 4 | `BusyBackoffSet`, `BusyBackoffTake`, `Now`, `LeaseTTL` |
| runexec → порт `Lease` | 1 | `Renew` (на sessqueue) |
| rundrain → finqueue | 1 | `RecordFinalizationQueueFailure` |
| batchreaction/batchdeliver/batchrecover → корень sessionbatch | 1-4 | `BeginReadTx`; `Scoped/AdmitMutation/ValidateTx` (deliver); `RequestStop` (recover) |
| fin{facts,holds,debt,term} → finstore | ≤5 | как в рев. 1 |
| finholds → proctrack | 1 | `TrackedProcessCount` |
| sessionfacts → sessionlock | ≤3 | `InspectSessionLock` + типы |
| sessioncgroup → sessionlock | ≤6 | entangle: childgroup→lock 6 |

## 4. Компонентная экстракция (решение Г1)

### 4.1. Ядро `service` → 6 компонентов + sesstypes

| Компонент (пакет) | Тип-владелец | Поля (новые) | Методы, переехавшие с `*service` | Порт назад (имена) |
|---|---|---|---|---|
| sesstypes | — (данные) | — | — (чистые типы + чистые функции `Owns`, `IsSession*CallData`, `ParseChildQuestionNotice`) | — |
| sessrows | `RowStore` | db, q, qRead, `Scope{WorkspaceRoot, WorkDir, Home}` | 46 — все методы файлов `session_update.go` (21), `session_read.go` (7), `session_lifecycle.go` (12), `session_cost_subtree.go` (4), `session_purge_queue.go` (1) + `ownsArgs` из `session_workspace.go`; точный перечень — блок под таблицей | нет (Scope при конструировании) |
| sessqueue | `QueueStore` | db, q, finq (guard не нужен: lease/enqueue-допуск идут через finqueue — проверено grep по guard-идентификаторам) | 17 (перечень ниже) | ← finqueue: `EnqueueFinalizationAware`, `BindQueueLeaseTx` |
| sessagent | `AgentStateStore` | db, rows | 10: depth 4, model_role 2, role-хранилища 4 | ← sessrows: `CreateTaskSession` |
| sessguard | `Guard` | writer, reader *sql.DB | Bind/With/hasFinalizationScope (3) + admit/mutation/ancestry/tx/context | ← finstore 5 (см. §3) |
| finqueue | `Queue` | db, guard, finstore | 8 (перечень ниже) | ← sessguard ровно 4: `AdmitFinalizationMutation` (`finalization_queue_enqueue.go:20`), `ValidateFinalizationMutationTx` + `ReadFinalizationAncestry` (`finalization_ingress_helpers.go:58,69,111,140`; `finalization_queue_restore.go:51,104`), `FinalizationScoped` (`finalization_ingress_helpers.go:277`) |
| sessmutate | `MutationStore` | db, guard, finq | 13: inject 6, orphan 7 | ← sessguard 5, ← finqueue 2 |

Перечни методов (точные, приёмники `*service`; каждый файл переезжает целиком):
- sessrows 46: IncrementCost, IncrementCostIfUnderMax, UpdateSystemPrompt, SetUsage, SetSummaryAndUsage, SetTodos, UpdateModels, UpdateWorkerReviewerModels, UpdateWorkerReviewerReasoningEffort, UpdateReasoningEffort, UpdateSmartReasoningEffort, UpdateFastReasoningEffort, UpdateWorkerReasoningEffort, UpdateReviewerReasoningEffort, afterEffortWrite, Rename, RequestCancel, IsCancelRequested, ClearCancelRequest, SetEndedReason, SetBudget; Get, GetLast, ListSubSessions, GetCallTreeActivity, GetCallTreeActivityBatch, List, ListAll; bindWorkspace, createWithOrigin, Create, CreateWithID, CreateTaskSession, CreateTitleSession, Delete, CreateAgentToolSessionID, ParseAgentToolSessionID, IsAgentToolSession, CreateWithOrigin, CreateWithIDAndOrigin; SubtreeBudget, SubtreeSpent, SubtreeUpdatedAt, ResetCostBase; PurgeQueuedWorkForSession; ownsArgs.
- sessqueue 17: EnqueueRunQueueEntry, LeaseRunQueueEntry, RenewRunQueueLease, AckRunQueueEntry, NackRunQueueEntry, NackRunQueueEntryNoAttemptPenalty, TerminalFailRunQueueEntry, ListPendingRunQueueEntries, GetRunQueueEntry, HasOutstandingRunQueueEntriesForSession, ListStaleLeasedRunQueueEntries, CleanupExpiredLeases; StoreSessionCredentialRequirement, HasSessionCredentialRequirement; StoreSessionExecutionRequirement, DeleteSessionExecutionRequirement, LoadSessionExecutionRequirement.
- sessagent 10: CreateTaskSessionWithAgentDepth, ResolveAgentDepth, ResolveSavedAgentDepth, LowerAgentDepth; GetAgentModelRole, BindAgentModelRole; GetSessionRoleContract, BindSessionRoleContract; GetRootRoleContract, BindRootRoleContract.
- sessguard: методы BindAgentFinalization, WithAgentFinalizationAuthorities, hasFinalizationScope + свободные функции guard-файлов (полный перечень идентификаторов — §3 S2: admitBatchCommitOwners, admitFinalizationMutation, batchFinalizationAdmission, batchFinalizationTicket, finalizationTicketRelease, combineTicketReleases, JoinReleaseError (бывш. batchCommitReleaseError), validateBatchFinalizationTx, readFinalizationAncestry, finalizationAncestry, FinalizationScoped (бывш. finalizationScoped)).
- finqueue 8: enqueueFinalizationAware, existingFinalizationQueueEnqueue, RestoreAgentFinalizationQueue, restoreFinalizationQueue, PrepareAgentFinalizationQueue, bindFinalizationQueueLeaseTx, prepareStoredFinalizationIngress, prepareFinalizationIngress (+ свободные функции `finalization_queue_failure.go`).
- sessmutate 13: CreatePendingInject, DrainPendingInjects, PeekInterruptInject, DeleteInterruptInject, ConsumeInterruptInjectAndEnqueue, ReconcileInterruptInjectEnqueue; WriteToOrphanOutbox, writeUnboundOrphanOutbox, existingBoundOutboxWrite, ListPendingOrphanOutboxEntries, GetOrphanOutboxEntry, DrainOrphanOutboxEntry, RecordOrphanOutboxFailure.

Ядро после экстракции: `session.go` (структура `service` получает поля компонентов; конструкторы собирают граф), `session_fork.go` (12 tx-методов копирования + guard — связность с `WithFinalizationTx` и `admit` в одном файле), `session_delegates.go` (~60 делегатов + alias-блок). Элементы service: 3 файла + каталог = 4.

**Доказательство отсутствия инверсии порядка (service-компоненты):** в `service` нет собственных мьютексов — координация через single-writer SQLite + guard-билеты (документировано `batch_store.go:41-43` «no store locks span I/O»). Компоненты получают `*sql.DB` при конструировании и: (а) не открывают транзакцию внутри чужой (все методы, работавшие внутри tx вызывающего, принимают tx параметром — `validateFinalization*`, `bindFinalizationQueueLeaseTx`); (б) сохраняют дисциплину admit(билеты до SQL) → tx → отложенный release — тела не меняются; (в) `busyBackoff`-аналогов не создают. Проверка: побайтовый diff тел (замена только `s.x`→`c.x`), серийные тесты `batch_commit_*`, `finalization_queue_*`, `p613_*`, `p782_*`.

### 4.2. Ядро `BatchStore` → 3 компонента + корень

| Компонент | Тип | Методы (все `*BatchStore` сегодня) | Порт от корня |
|---|---|---|---|
| batchreaction | `ReactionLedger` | 15: BatchReactionCaptureDebtSnapshot, batchReactionDebtExists, BatchReactionDebtExists, BatchReactionVisibleDebtExists, BatchReactionPendingInclusiveDebtRows, BatchReactionPendingInclusiveDebtSummary, BatchReactionDebtOwners, BatchReactionMarkReactedWithMessageUpdate, batchReactionAccount, BatchReactionIncrementWakeAttempts, BatchReactionSettleReactedFailed, BatchReactionSettleReactedFailedWithMarker, BatchReactionMaxWakeAttempts, BatchReactionListReactedFailedText, BatchReactionSetWakeZeroForOwners | `Reader{BeginReadTx}` (1) |
| batchdeliver | `DeliveryHub` | 19: BatchDeliveryRootEnvelope, BatchDeliveryElectInline, BatchDeliveryElectStarted, BatchAnnounceStarted, BatchAnnounceInlineResult, BatchConsumeToolResult, BatchPullNotices, batchPullOneNotice, BatchPublishQuestion, BatchResolveChildQuestions, BatchHistorySnapshot, BatchHistoryRootViews, BatchResetOwnerHistory, BatchRerunReconcile, BatchOwnerDeleteHistory, BatchHistoryReconcileDelivery, BatchDisposeLegacyBoundNotices, BatchProbeLive, BatchDeliveryOneItemCompletion | `Reader{BeginReadTx}` + `GuardPort{Scoped; AdmitMutation; ValidateTx}` (3; функции guard ядро отдаёт при конструировании — S2) |
| batchrecover | `RecoverySweeper` | 15: BatchRecoverDeadHost, batchRecoverRoot, BatchSweepDeadHosts, BatchRecoverOwnerScope, batchRecoveryCandidates, batchRecoveryCandidateByID, batchRecoveryRootsForHost, BatchRecoverRoot, BatchRecoverPass, RecoverHostsOnce, BatchSettleCLIWakeDebt, BatchCountExpired, BatchPurgeExpired, BatchCountJobsOlderThan, BatchPurgeJobsOlderThan | `Reader{BeginReadTx}` + `RequestStop` (2) |
| sessionbatchread | `CLIViews`/`APIViews` | 7: BatchCLIIdentities, BatchCLIFind, BatchCLIGetNode, BatchCLIPollControl (4); BatchAPIRoots, BatchAPIHeldQuestions, BatchOwnsRows (3) | writer/reader при конструировании |

Корень `sessionbatch` сохраняет: структуру `BatchStore{writer, reader}`, `init()`-регистрацию, Reserve/ReserveIdentity/Project/Read/beginReadTx/LiveRootsForHost/Control-четырку/DeleteRoot/RootOwner, commit-протокол (S3), stop-fence (S6), child_bind. Наружу — 50 однострочных делегатов на `BatchStore` (`session_delegates.go`): API `session.BatchReaction*/BatchHistory*/BatchRecover*/BatchPull*/BatchQuestion*/BatchDelivery*/BatchCLI*/BatchAPI*` неизменно.

**Доказательство порядка (batch):** `BatchStore` не имеет мьютексов; критичные последовательности: (1) **commit**: admit-билеты (sessguard, до SQL) → ОДНА транзакция store (Submit: reservation+view атомарно, `batch_commit.go:53-58`) → deferred release — функции переносятся целыми телами, границы tx (Begin/Commit/Rollback в тех же операторах) не трогаются; (2) **stop fence**: `psfOwnerStopSQLPredicate` и `pdcBatchRootFenceSQL` остаются в корне (S6), SQL-текст pinned `pda_batch_prepare_equiv_test.go`, `pdc_batch_tx_message_test.go`; (3) **admission-вызовы** child_bind/pull/question/tool_result — уносят с собой порт GuardPort с теми же функциями: порядок admit→tx→release воспроизведён вызовом тех же функций. Проверка: серийные `batch_commit_contention/close/authority/admission/timeout/order/priority/barriers`, `batch_control_races`, `batch_terminal_delivery`, `batch_sqllog`.

### 4.3. Ядро `RunQueuePump` → 3 компонента

| Компонент | Тип | Методы pump | Состояние | Порт |
|---|---|---|---|---|
| runadmission | `Admission` | admitSession, AdmitSessionForTest, AdmitSessionObservedEntryForTest, SetTestAfterAdmissionRefusalForTest | свой `admitMu`, map сессий, `admissionEntry`, `admissionOutcome` | `Clock{Now; LeaseTTL}` (2) |
| runexec | `Executor` | executeEntrySync (+хелперы файла) | без своего состояния | `ExecHost{BusyBackoffSet; BusyBackoffTake; Now; LeaseTTL}` (4) + `Lease{Renew}` (1) + Coordinator + типы runadmission |
| rundrain | свободные функции + `RowLedger` | drainOrphanOutbox, processOrphanOutboxEntry, recordOrphanOutboxDrainFailure | rowLedger | ← runadmission (тип outcome), ← finqueue `RecordFinalizationQueueFailure` (1) |

Состояние `busyBackoffUntil`+`busyBackoffMu` ОСТАЁТСЯ в pump (`run_queue_pump.go:637-638`): три точки касания — `entry_dispatch.go:82-88` (чтение/сброс, остаётся в sessionrun), `entry_exec.go:721-723` (запись → порт `BusyBackoffSet`), `run_queue_lifecycle.go:189-195` (sweep, остаётся).
**Доказательство отсутствия инверсии замков (pump):** сегодня `admitMu` никогда не вложен в `execSem` и наоборот: admitSession возвращает (entry, release) при ОТПУЩЕННОМ admitMu; release повторно лочит внутри себя; exec держит семафор и логическую lease, но не admitMu, во время `Coordinator.Run`. Экстракция сохраняет тела (release-замыкание рождается внутри admission и захватывает свой admitMu) — порядок «admitMu (кратко) → execSem (надолго) → admitMu (кратко)» воспроизведён. `DrainSessionNow` остаётся методом pump (тело не меняется). Проверка: серийные `p587_admission_aba`, `p607_*`, `p613_*`, `p1_4_bounded_worker_pool`, `p616_*`, `pump_*`, `p421*`, `p350*`, `p0_1_durable_double_execution`.

### 4.4. Правила доказательства чистоты (все EX-полосы)

1. **Оракул переноса-экстракции:** отсортированный список `^(func|type|var|const)` множества «до» равен «после» ПОСЛЕ механической нормализации: `(s \*service)`→`(c \*RowStore)` и `s.поле`→`c.поле` (sed-таблица фиксируется в описании полосы); число `func Test` в перенесённых тестах неизменно.
2. **Критичные пути** (batch commit, допуск финализации, stop fence, drain): дополнительно побайтовый diff тел после sed — отличия только в приёмнике/получателе; серийные тесты из §4.1-4.3 гоняет оркестратор.
3. **Порты ≤15 имён**, перечислены в §3; новый порт в полосе — стоп-условие.
4. **Проверка ацикличности:** после каждой EX — `go vet` + grep «компонент не импортирует ядро» для всех alias-доноров.
5. Сборка/vet затронутых пакетов через `golock.sh` — первый шаг полосы (В7).

## 5. План исполнения

Правила: ≤5 агентов одновременно; полосы одного каталога — последовательно или с непересекающимися файлами; агенты — `gofumpt`/`go build`/`go vet` через `golock.sh`, тесты серийно гоняет оркестратор (`golock.sh go test -p 1 -parallel 1 -count=1 -timeout 300s`). Оракулы: §4.4.

### Полосы швов (0б-2)

| Полоса | Скоуп | Предпосылки | Приёмка |
|---|---|---|---|
| SM1 (S1) | track×5 + тесты → proctrack (новый лист `kit/sys/proctrack`); `TrackedProcessCount`; правка holds; перепись импортов | нет | grep: в internal/session нет `trackedGroups|trackedJobs|RegisterTrackedJobTerminator`; оракул; build/vet proctrack+procinfo+session+cliprovider+cmd |
| SM2 (S2+S3-часть) | сигнатура writer; вызовы `finalization_mutation.go:25`, `batch_commit.go:138,375`; релокация `RootOwner`→batch_store.go | нет (∥SM1/SM3/SM4 — файлы не пересекаются) | build/vet; grep `admitBatchCommitOwners(` без `*BatchStore`; серийно: batch_commit_admission/authority |
| SM3 (S5) | релокация `DrainSessionNow` (`run_queue_drain_session.go:13`) → `run_queue_pump.go` | нет | build/vet; число Test-функций пакета не изменилось; серийно: p421, pump_* |
| SM4 (S8) | `run/sessiontest` (фикстуры), правка трёх семейств дублей (около 30 файлов тестов, см. §1.8 и S8) | нет | build/vet (+ `go vet` тестов обоих деревьев); grep: объявления фикстур один раз; число `Test*` неизменно; серийно: run-queue и pump в `session` + сценарии `itest` |

### Полосы размещения (D2, до экстракции)

| Полоса | Скоуп | Предпосылки | Параллельность | Приёмка |
|---|---|---|---|---|
| MV1 | 34 файла + тесты ядра → `internal/session/service/`; перепись импортов 12 внешних пакетов (`shell`/`hooks` исключены) + конфиги CI | SM1-4 | одна, первой | оракул 34; контрольный grep: в service/*.go нет импортов сателлитов; `internal/session` = 7 элементов |
| MV2 | lock×8 (+В7, вклейки) → proc/sessionlock; childgroup/starttime×4 (+В7) → proc/sessioncgroup | MV1 | ∥MV3-MV5 | оракул; пары `_unix/_windows` вместе; ≤7 |
| MV3 | batch/sessionbatch 26 + sessionbatchread 5 + finstore/finfacts/finholds/findebt/finterm 19 + тесты | MV1 | ∥ | оракул; границы S2; #132-окно |
| MV4 | run: 12 файлов → sessionrun/sessiondrain(врем.)/sessiontest | MV1, SM3 | ∥ | оракул; серийно run-queue |
| MV5 | state: 17 файлов → 5 пакетов | MV1 | ∥ | оракул; grep-границы |

### Полосы экстракции (D2, решение Г1; после размещения)

| Полоса | Скоуп (файлы) | Предпосылки | Приёмка (оракул §4.4 + тесты оркестратора) |
|---|---|---|---|
| EX1 sesstypes | блоки типов из session.go/workspace/runqueue/credential/execution/role_contract + types_notice + types_batch; alias-блок в ядре | MV1 | сборка/vet; grep внешних `session\.X` — множество имён не изменилось; серийно: session_row_fields, role_contract, типовые |
| EX4 sessguard+finqueue | 14 файлов guard/finqueue (7+7); `finalizationScoped`→context.go; `BatchCommitIdentity`→admission.go; `Authority.service`→`db`; `hasFinalizationScope`→Guard; делегаты ядра | EX1 | побайтовый diff тел; серийно: finalization_* (queue/authority/context/ingress/lease/tx/mutation/ancestors), batch_commit_admission/authority, p613; **#132-окно** |
| EX2 sessrows | update/read/lifecycle/cost/purge/workspace → 5 файлов + делегаты | EX1, EX4 | оракул; серийно: session_update/read/lifecycle/cost/purge, p782 |
| EX3 sessqueue+sessagent | runqueue-методы + credential/execution store + 4 agent-файла + делегаты | EX1, EX4 | оракул; серийно: session_runqueue*, credential/execution, agent_depth, role stores |
| EX5 sessmutate | inject + orphan_outbox + делегаты | EX1, EX4 | оракул; серийно: session_inject*, orphan/outbox-тесты (p556), p0_3_orphan_outbox_drain |
| EX6 batch-компоненты | reaction 1; deliver 13→5; recover 5; cli/api views +2; commit/doc прибытие из ядра; вклейки stop+control, outbox_doc→pull.go; делегаты BatchStore | EX1, EX4 (identity уже в sessguard) | побайтовый diff тел commit/fence; серийно: batch_commit_* (контention/close/authority/timeout/order), batch_control_races, batch_terminal_delivery, batch_sqllog, pda/pdc SQL-эквивалентность; **#132-окно** |
| EX7 run-компоненты | admission→runadmission; entry_exec→runexec; classify/drain_ledger/row_context/orphan→rundrain; sessiondrain растворяется; делегаты не нужны (DrainSessionNow остаётся) | MV4, SM3, EX1 | побайтовый diff тел; серийно: p587/p607/p613 (admission), p1_4/p616 (exec), p0_*/p1_*/p348/p350/p421/p5xx/p6xx/pump_* (drain), itest run_queue_round2 |

Граф: `SM1∥SM2∥SM3∥SM4` → `MV1` → `(MV2∥MV3∥MV4∥MV5)` → `EX1` → `EX4` → `(EX2∥EX3∥EX5∥EX6∥EX7)` — ровно 5 агентов → `(MV7∥MV8)` → `FIN`. EX-полосы создают детей `components/` по одному — каталог никогда не превышает 7.

### Окна конфликта с #132 (пересборка финализации)

Не вести одновременно с #132: **SM1** (holds-файлы), **SM2** (guard-вызовы), **MV3** (fin-файлы), **EX4** (guard/finqueue/identity), **EX6** (commit-протокол). Рекомендация: SM1-SM4 и EX1 до старта #132; EX4/EX6/MV3 — до старта или после завершения #132; MV1/MV2/MV4/MV5/EX2/EX3/EX5/EX7 с #132 по файлам не пересекаются, но правят импорты агента — в одну волну не ставить.

### Обязательные сплиты (решение Г1 «без постоянных исключений»)

| Полоса | Скоуп | Приёмка |
|---|---|---|
| MV7 jobbatch | 20 нетестовых файлов (пересчитано: admission, batch_testhook, commit_fault, doc, engine, errors, events, executor, notice, runner, runner_commit, runner_control, runner_dispatch, runner_observe, runner_requests, runtime_types, tree, tree_apply, types, view) → `jobbatch` (engine, tree, tree_apply, types, runtime_types = 5), `jobevents` (executor, notice, errors, events, admission, view, doc = 7), `jobrunner` (runner, runner_commit(+вклейка commit_fault), runner_control, runner_dispatch, runner_observe, runner_requests, batch_testhook = 7); `lab`→`joblab` (24 файла — отдельная строка храповика с расписанием MVP/позже) | оракул; уникальность `joblab` |
| MV8 heartbeat | 10 файлов → `heartbeat` (heartbeat, budget, failure, lifecycle, rate_limit = 5) + `heartbeatio` (reader, writer, dir, parent_other, parent_windows = 5) | оракул |

### FIN — храповик, расписание падения

| Строка (элементы) | 34 (MV1) | 34 (EX1) | 21 (EX4) | 15 (EX2) | 8 (EX3) | 6 (EX5) | 4 (EX6) |
|---|---|---|---|---|---|---|---|
| `internal/session/service` | да | да | да | да | да | да | удалить (3 файла + components/) |

| Строка (элементы) | 26 (MV3) | 24 (cli/api) | 23 (reaction) | 18 (recover) | 21 (+commit/doc/delegates) | 8 (deliver) | 7 (вклейки) |
|---|---|---|---|---|---|---|---|
| `internal/session/batch/sessionbatch` | да | да | да | да | да (21<26; храповик сравнивает с записанным числом, не с минимумом) | да | удалить |

Правило храповика: число сравнивается с ЗАПИСАННЫМ в строке (34 / 26), а не с историческим минимумом — временное поднятие 18→21 в середине EX6 легально (порядок внутри EX6: сначала приходят три файла, затем deliver уходит; число никогда не опускается ниже 7, поэтому правило «каталог на пределе или ниже удаляется из списка» не срабатывает посреди полосы); конечные 4 и 7 → строки удаляются. Шаги расписания — дельты полос; внутри параллельной группы (EX2∥EX3∥EX5∥EX6∥EX7) порядок любой, сумма дельт даёт конечное число. Арифметика service: 34−1+1=34 (EX1), −14+1=21 (EX4), −6=15 (EX2), −7=8 (EX3), −2=6 (EX5), −2=4 (EX6). Арифметика sessionbatch: 26−2=24, −1=23, −5=18, +3=21, −13=8, −1=7. `internal/heartbeat 10` / `internal/jobbatch 21` / `lab 24` — удаляются MV7/MV8. `internal/procinfo 12` — В2.

## 6. Открытые вопросы (после решения Г1)

Решение записано: постоянных исключений нет; компонентная экстракция — в объёме спринта (EX-полосы §5); храповик только временный с падающим расписанием; то же — для хранилища конфигурации (D1-config). Остались:
- **В2 (решено, В12 плана).** `internal/procinfo` после S1 получил бы 12 нетестовых файлов; решение: реестры tracked уходят в новый лист `kit/sys/proctrack` (5 файлов), `procinfo` остаётся с 7 (проба, живость, убийство процесса). Строки храповика нет. Затрагивает SM1.
- **В3.** Резерв имён `fin*`, `sesstypes`, `sess*` за session — сверка четырёх D1-документов оркестратором (параллельный D1-agent проектирует свою финализацию).
- **В5.** MV7/MV8 теперь обязательны по решению Г1 (без них строки `jobbatch 21`, `lab 24`, `heartbeat 10` не падают); подтвердить их волну (5) и расписание `lab`.
- **В6.** Второй храповик A1b: сиды service/sessionbatch на временный период велики; цель ≤21 — вопрос приёмки Z (числа падают по расписанию FIN).
- **В7.** Смоук: связки «метод↔receiver» и граф импортов проверены переписью исходников, не компилятором; первый шаг каждой полосы — `go build`/`go vet` через `golock.sh` (§4.4.5).
- **В8.** `legacy_import_drain_external_test.go` — назначить по поведению при MV1 (itest либо ядро).
- **В9.** Остаток непроверенного (сужён по ревью): состав `RunQueuePumpConfig` (скрытые типы ядра → sesstypes при EX7); `AgentDepthStore`/`AgentModelRoleStore` — интерфейс или структура (влияет на EX3-делегаты). Разрешено по ревью: `DrainSessionNow` = метод в `run_queue_drain_session.go:13`; `processTreesStopping` = `track_stop.go:8`; `StartProcessUnlessStopping` = `track_stop.go:11`; `MaxPidFallbackAge` = `lock.go:110` → sessionlock; `BatchCommitIdentity` извне собирается только экспортируемыми полями.
