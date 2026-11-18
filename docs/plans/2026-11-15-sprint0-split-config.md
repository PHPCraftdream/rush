# D1: дизайн разрезки `internal/config` (задача #74, пакет internal/config)

- Источники: план спринта (`2026-11-15-sprint0-plan.md`, §1–3, §5 «0b»/«D1», §6, §11) и исследование (И§2–5, §9, §11.2). Слой `config`: `kit < store < session < config < agent < app < serve`; под-пакеты одного пакета образуют ациклический граф.
- **Редакция 2 (после Г1):** решение оператора по открытому вопросу 1 редакции 1 — постоянного исключения для ядра хранилища нет и «следующий спринт» отменён: направление «склон к разрезке и рефакторингу», извлечение компонентов MCP/OAuth/settings входит в спринт 0 (шовные полосы X1–X4 волны 4 + переупаковка D2-5). Цель — во всех каталогах ≤ 7 элементов; ниже это выполнено без единого остатка выше 7 (§2.1), запись-храповик для корня `internal/config` — только временная с падающим графиком (§5.4).
- Базовое состояние для дизайна — после этапа C: `internal/discover` → `internal/config/discover`, `internal/oauth` → `internal/config/oauth`, `internal/projects` → `internal/app/projects`; лист `internal/config/catalog` уже существует (волна 1, 0а-1c).
- Замеры: `prereq.py`, `entangle.py`, `analyze.py` читают снимок основного каталога, который **старше волны 1** (нет `catalog/`, есть импорты `internal/agent/*`). Числа из скриптов — направленные; все существенные факты ниже проверены чтением кода этой ветки и помечены `файл:символ`. Проверено на этой ветке: в непроверяемых файлах `internal/config` импортов `internal/agent/**` нет; единственное упоминание в тестах — импорт `agent/codexprovider` (`task1314_oauth_disk_sync_test.go:17`, задача #95).

## 1. Текущее состояние

### 1.1. Объём

| Показатель | Значение | Источник |
|---|---|---|
| Непроверяемые .go в корне пакета | 120 файлов, 26 002 строки | `wc -l`, эта ветка |
| Тестовые .go в корне | 183 файла, все `package config` (белые) | grep, контрпримеров нет |
| Кластер хранилища (планируемый `cfgstore`+компоненты) | 30 файлов, 10 596 строк, 229 методов `*ConfigStore` (mcp 76, oauth 49, settings 16, ядро 88) | подсчёт по файлам этой ветки |
| Внешне вызываемые методы хранилища | 47: mcp 33, oauth 9, settings 5 | grep `\.Имя\(` по 13 группам-потребителям |
| Подкаталоги сейчас | `catalog/` (3 файла + `provider.json`), `testdata/` | `ls` |
| После этапа C | + `discover/` (13 непровер. файлов), + `oauth/` (token.go + `codex/`, `copilot/`, `hyper/`) | И§9.2 |
| Файлов 951–1000 строк | `store_config_disk_sync_test.go` 991 (тест); из кода: `store_reload.go` 965, `store_mcp_transaction.go` 927, `load_providers.go` 917 | `wc -l` |

Кластеры по префиксам файлов (`analyze.py clusters internal/config 60`, данные до волны 1): store 83ф/9357кода/15066тестов/118 коммитов-90д; config 50/3719/3169/72; load 22/2660/3125/60; ktav 27/2321/4310/15; settings 17/1063/2487/15; peak 9/1171/1539/4; apitoken 6/819/1517/4; provider 10/700/1041/12; model 7/454/821/8; resolve 3/288/867/4; mcp 8/545/587/21; datadir 2/507/511/5. Самое горячее — `store` (118/90д) и ядро `config`+`load` (72+60).

### 1.2. Циклы между будущими подпакетами

`prereq.py`: один цикл `core <-> ktav < -> oauth <-> peak <-> provider <-> store` с весами: store→core **105**, ktav→core 36, provider→core 28, core→store 25, store→ktav 24, core→ktav 15, oauth→core 13, oauth→ktav 11, ktav→store 10, oauth→store 9, store→provider 9. `peak`→наружу 3, `apitoken` никем не используется (И§11.2).

Уточняющий замер на этой ветке (комментарии и строки вырезаны; кластеры: load отдельно, store_oauth отдельно, атомарная запись отдельно — «IO»): store→IO 34, store→модель 25, load→модель 14, store→load-движок 18, load→store 6 (+обращения к полям), ktav→IO 24, store→ktav 24, oauthstore↔ktav 11/6, IO→store 10, ktav→load 8, datadir→load 4, load↔provider 5/2, store↔oauthstore 5/9, provider↔store 4/7, load→peak 3.

Три реальных узла: **(а) load↔store** (движок стабильного чтения разорван между `load_files.go` и `store_reload.go`; `loadOnce` лезет в непроверяемые поля `ConfigStore`: `load.go:301-308`), **(б) IO↔store/ktav** (ошибки и стабильное чтение живут в store, хотя принадлежат IO), **(в) provider↔store/load** (`configureProviders` — метод `*Config`, лезущий в `store.removeConfigFieldBestEffort` (`load_providers.go:210`) и `store.cliModels` (`load_providers.go:419`); `configureSelectedModels` параметры `store`/`persist` не использует — `load_providers.go:796`).

### 1.3. Прочие факты

- Белые тесты: 183/183 в корне (`package config`); + `catalog/catalog_test.go` (`package catalog`).
- Тестовые помощники через будущие границы: 70 идентификаторов в 28 направлениях (prereq.py). Экспортированные тестовые швы: `NewTestStore`, `NewTestStoreWithResolver`, `ResetProviderCacheForTests`, `SetKtavAutoMigrateForTest`, `SetBeforeConfigCommitForTest`, `SetSettingsPasswordIterationsForTest`, `SetModelVerifyOnlineForTests`, `SetProviderHTTPClientFactory`.
- `init()`: ровно два (`datadir.go:481` `workspaceHomeFlag.Store(true)`; `settings_lock.go:88` `settingsPasswordIterations.Store(600_000)`) — §4.2.
- `go:embed` в корне нет; один — `catalog/hyper.go:32` (`//go:embed provider.json`).
- Файлов под ОС — 30 (§4.3).
- Внешний API: **178 различных `config.X`** в 13 группах-потребителях (agent, app, cmd, server, sdk, commands, deploy, hooks, heartbeat, nettransport, projects, proto, session; И§1: ~500 файлов).
- `internal/config` импортирует: `catalog`, `csync`, `db/storepath`, `env`, `filelock`, `filepathext`, `fsext`, `home`, `ktavcodec`, `platform`, `shell` (`config_mcp.go:127`), `discover`, `oauth`(+codex/copilot/hyper). Импортов `internal/agent/**` в коде нет.

## 2. Целевое дерево

### 2.1. Дерево и число элементов (лимит 7, тесты и ресурсы не считаются)

```
internal/config/               package config — ПУБЛИЧНЫЙ API (фасад): facade.go            = 7 эл.
├── load/                      package cfgload — композиционный корень: load.go, init.go   = 2
├── model/                                                                  = 7
│   ├── (5 файлов: config.go(+scope.go), config_mcp.go(+responses_transport+finalization_flag+load_hooks), defaults.go, roles.go(+custom_roles_integrity+custom_role_tools+custom_roles_models+canonical-функции), network.go(+provider_http.go целиком)) package cfgmodel
│   ├── merge/                 package cfgmerge: merge.go, overlays.go, customroles.go = 3
│   └── envresolve/            package envresolve: resolve.go                = 1
├── io/                        групповой каталог, без .go                   = 7
│   ├── slots/    package slots      6: ktav_discovery.go, ktav_doc.go, ktav_light.go, ktav_cache.go, ktav_oauth.go, oauthjournal.go(новый, формат журнала)
│   ├── slotmigrate/ package slotmigrate 6: ktav_migration{,_artifacts,_cache_rev,_symlink,_unix,_windows}.go
│   ├── cfgcommit/  package cfgcommit 7: atomicwrite.go, config_commit_test_seam.go, config_commit_unix/windows.go, config_durability_unix/windows.go, config_write_identity.go
│   ├── fileid/   package fileid     7: config_file_identity_unix/windows.go, config_parent_identity_unix/windows.go, config_linkat_unix.go, config_owner_unix/windows.go
│   ├── fswap/    package fswap      7: config_open_unix/windows.go, config_rename_darwin/unix/windows.go, config_renameat2_linux/other_unix.go
│   ├── casefold/ package casefold   7: config_case_sensitivity{,_behavioral_unix,_darwin,_freebsd,_linux,_other_unix,_windows}.go
│   └── docread/  package docread    5 (новые): candidates.go, stabledocs.go, fingerprints.go, normalize.go, mcpdocs.go
├── store/                     групповой каталог, собственных .go нет        = 6
│   ├── cfgstore/    package cfgstore   7: store.go(832, +store_options.go), store_write.go(746), store_reload.go(~650, сплит 966), store_reload_flow.go(~316), store_sync.go(719, = store_staleness 178 + reload_mutex 73 + store_config_disk_sync 468), store_models.go(411, = 257 + store_custom_roles 90 + provider_peak_hours_read 64), store_api.go(~280: адаптер портов, проводка компонентов, 47 делегаторов)
│   ├── mcpstore/    package mcpstore   7: store_mcp.go(486, +mcp_uncertainty.go 78), store_mcp_admission.go(741), store_mcp_json.go(671), store_mcp_locks.go(358), store_mcp_transaction.go(~600, сплит 927), store_mcp_transaction_apply.go(~328), docker_mcp.go(153)
│   ├── oauthcred/   package oauthcred  5: store_oauth.go(645), store_oauth_disk_sync.go(196), store_oauth_pending.go(120), store_oauth_provider.go(126), store_oauth_refresh.go(426)
│   ├── oauthjournal/ package oauthjournal 4: store_oauth_journal.go(остаток ~200 после переноса формата в io/slots), store_oauth_journal_outbox.go(258), store_oauth_journal_quarantine.go(115), store_oauth_journal_worker.go(426)
│   ├── settings/    package settings   6: settings_lock.go(669, +settings_lock_hashes.go 33), settings_lock_sources.go(114), settings_lock_sources_other.go(20), settings_lock_sources_windows.go(21), settings_authority.go(44), settings_models_integrity.go(~120, только методы)
│   └── cfgtest/     package cfgtest    ≤4: общие тестовые помощники (0б-6/D2-6)
├── providers/                 групповой каталог                             = 4
│   ├── catalog/               package catalog  (переезд из internal/config/catalog)
│   ├── discover/              package discover (6 файлов ядра) + discover/local/ (7 файлов, discoverlocal)
│   ├── catalogsrc/            package catalogsrc 6: provider.go(без cachePathFor), catwalk.go, hyper.go, catalog_supplement.go, model_catalog_cache.go, model_verify.go
│   └── provresolve/           package provresolve 4: load_providers.go(~600, сплит 917), load_providers_merge.go, env_overlay.go(=load_env.go), load_providers_apply.go(~317)
└── domains/                   групповой каталог                             = 6
    ├── datadir/   package datadir  7: datadir.go, global_dir.go(+global_testguard.go), globalpaths.go(=load_paths.go+path-утилы load_files.go+cachePathFor), worktreehome.go(=load_worktree.go), skills.go(=load_skills.go), systemconfig_unix.go, systemconfig_windows.go
    ├── peak/      package peak     5: peak_hours.go, peak_hours_merge.go, peak_hours_policy.go, peak_hours_tz.go, peakhours.go(=provider_peak_hours.go)
    ├── apitoken/  package apitoken 2: apitoken_binding.go, apitoken_store.go
    ├── apiremote/ package apiremote 1: api_remotes.go
    ├── listeners/ package listeners 1: server_listeners.go
    └── oauth/     package oauth    (переезд из internal/config/oauth: token.go + codex/ + copilot/ + hyper/)
```

Итог: **каждый каталог ≤ 7 элементов, исключений нет.** Файлы > 900 строк к концу спринта отсутствуют: `store_mcp_transaction.go` 927 → сплит в X2; `store_reload.go` 965 → сплит в D2-5; `load_providers.go` 917 → сплит в D2-3 (график падения — §5.4). Промежуточные каталоги `io/`, `providers/`, `domains/`, `store/` собственных .go не имеют.

### 2.2. Имена пакетов (уникальность)

`grep '^package '` по репозиторию: `cfgmodel`, `cfgmerge`, `cfgstore`, `cfgload`, `envresolve`, `docread`, `slots`, `slotmigrate`, `cfgcommit`, `fileid`, `fswap`, `casefold`, `catalogsrc`, `provresolve`, `datadir`, `peak`, `apitoken`, `apiremote`, `listeners`, `discoverlocal`, **`mcpstore`, `oauthcred`, `oauthjournal`, `settings`, `cfgtest`** — не встречаются ни разу. `oauth`, `discover`, `catalog` — существующие имена, сохраняются по принципу «последняя часть пути не меняется». Запрещённых имён (`core`, `util`, `common`, `base`) нет.

### 2.3. Полная таблица «файл → целевой пакет» (все 120 непроверяемых файлов)

«(+ X)» = содержимое X вливается в этот файл (регруппировка внутри пакета, оракул по объявлениям); «→ Y.Z» = объявление переезжает в файл пакета Y (шов); «сплит» = файл делится на два в том же пакете (оракул по сумме).

| Файл (сейчас) | Цель |
|---|---|
| config.go (+scope.go) | cfgmodel |
| config_mcp.go (+responses_transport.go, finalization_flag.go, load_hooks.go) | cfgmodel |
| load_defaults.go | cfgmodel (defaults.go) |
| custom_roles.go, custom_roles_integrity.go, custom_role_tools.go (MatchRoleToolPattern), custom_roles_models.go (ValidateCustomRoleSelection) | cfgmodel (roles.go) |
| settings_models_integrity.go — чистые `canonicalModelsJSON`, `modelsIntegritySignature`, `settingsKeyFirstComponent`, `ErrModelsEditedDirectly` | cfgmodel (roles.go); 3 метода `*ConfigStore` → settings/settings_models_integrity.go |
| load_files.go — `loadFromBytes`, `mustMarshalConfig` | cfgmerge (merge.go) |
| load_files.go — `sanitizeMCPDisabledOverlays`, `validateMCPDisabledOverlays`, `isMCPDisabledOnlyEntry`, `knownModelSlots`, `warnUnknownModelSlots` | cfgmerge (overlays.go) |
| custom_roles_load.go | cfgmerge (customroles.go) |
| resolve.go | model/envresolve |
| load.go, init.go | cfgload |
| load_paths.go | domains/datadir (globalpaths.go) |
| load_worktree.go | domains/datadir (worktreehome.go) |
| load_skills.go | domains/datadir (skills.go) |
| global_dir.go (+global_testguard.go), datadir.go | domains/datadir |
| config_unix.go, config_windows.go | domains/datadir (systemconfig_unix/windows.go) |
| provider.go — `cachePathFor`; load_files.go — `canonicalConfigPath`, `projectBoundary`, `sameDir` | domains/datadir (globalpaths.go) |
| peak_hours.go, peak_hours_merge.go, peak_hours_policy.go, peak_hours_tz.go, provider_peak_hours.go | domains/peak |
| apitoken_binding.go, apitoken_store.go | domains/apitoken |
| api_remotes.go | domains/apiremote |
| server_listeners.go | domains/listeners |
| ktav_discovery.go, ktav_doc.go, ktav_light.go, ktav_cache.go, ktav_oauth.go | io/slots |
| store_oauth_journal.go — формат (`oauthJournalRecord`, `errOAuthJournalInvalid`, `validOAuthJournal`, `readOAuthJournalWithRetry`, `decodeOAuthJournalDiskToken`, дайджесты/пути из ktav_oauth.go) | io/slots (oauthjournal.go) |
| ktav_migration.go, ktav_migration_artifacts.go, ktav_migration_cache_rev.go, ktav_migration_symlink.go, ktav_migration_unix.go, ktav_migration_windows.go | io/slotmigrate |
| atomicwrite.go, config_commit_test_seam.go, config_commit_unix.go, config_commit_windows.go, config_durability_unix.go, config_durability_windows.go, config_write_identity.go | io/cfgcommit |
| config_file_identity_unix.go, config_file_identity_windows.go, config_parent_identity_unix.go, config_parent_identity_windows.go, config_linkat_unix.go, config_owner_unix.go, config_owner_windows.go | io/fileid |
| store_reload.go — `readOpenedConfigBytes` | io/fileid |
| config_open_unix.go, config_open_windows.go, config_rename_darwin.go, config_rename_unix.go, config_rename_windows.go, config_renameat2_linux.go, config_renameat2_other_unix.go | io/fswap |
| config_case_sensitivity.go + 6 суффиксных (behavioral_unix, darwin, freebsd, linux, other_unix, windows) | io/casefold |
| load_files.go — `lookupConfigCandidates`, `eligibleConfigCandidate`, `eligibleWorkspaceConfig`, `pathAlreadyLoaded`, `configOwnerForWorkingDir`, `configOwnerPolicyForPath` | io/docread (candidates.go) |
| load_files.go — `stableConfigDocument`, `stableConfigAlias`, `loadConfigCandidateStableForWorkingDir`, `readStableConfigDocumentsWithOwner`; store_reload.go — `readStableConfigFileOwned`, `readStableConfigFile` | io/docread (stabledocs.go) |
| store_reload.go — `fileSnapshot`, `reloadFileFingerprint`, `configAliasChainFingerprint`, `configDiscoveryFingerprint`, `reloadStalenessState`, `candidateInputsChanged`, `errStableReadUnstable`, `errReloadDiskChanged`; store.go — `initialMCPRevisions`; mcp_revisions.go — `mcpInputFingerprints` | io/docread (fingerprints.go) |
| store_reload.go — `normalizeReloadPath`, `normalizeDiscoveryPath`, `ErrConfigNonRegular`, `ErrConfigReloadUnstable`, `errConfigOwnerMismatch` | io/docread (normalize.go) |
| mcp_json.go, mcp_revisions.go | io/docread (mcpdocs.go) |
| network.go (+provider_http.go целиком: ProviderHTTPClientFactory, providerHTTPClientFactory, SetProviderHTTPClientFactory, (c *Config) ProviderHTTPClient) | cfgmodel (network.go) |
| provider.go (без cachePathFor), catwalk.go, hyper.go, catalog_supplement.go, model_catalog_cache.go, model_verify.go | providers/catalogsrc |
| catalog/ (целиком) | providers/catalog |
| discover/ — discover.go, enricher.go, provider_catalog.go, model_facts.go, model_visibility.go, anthropic_models.go | providers/discover |
| discover/ — ollama.go, lmstudio.go, llamacpp.go, litellm.go, omlx.go, codex.go, zai_efforts.go | providers/discover/local |
| load_providers.go (сплит 917 → ~600+~317), load_providers_merge.go, load_env.go | providers/provresolve |
| store.go (+store_options.go 87 → 832): структура (поля §S6.5), NewStore/InitialLoad, снапшот-машина (loadSnapshot, publishLocked, updateConfigLocked), Snapshot*/Config/Generation, NewTestStore/NewLibraryStore, LogPath, WorkspaceHome-обёртка | store/cfgstore |
| store_write.go (746): configPath, resolveConfigWriteTarget, withConfigWriteLock*, Set/RemoveConfigField*, HasConfigField, HasWorkspaceConfig, noteInitialLoadWriteLocked, guard-точка → settings | store/cfgstore |
| store_reload.go (965 → сплит ~650 + ~316: autoReload/buildAndPublishReload → store_reload_flow.go; движок стабильного чтения уже ушёл в docread) | store/cfgstore |
| store_staleness.go (178) + reload_mutex.go (73) + store_config_disk_sync.go (468) | store/cfgstore (store_sync.go, 719) |
| store_models.go (257) + store_custom_roles.go (90) + provider_peak_hours_read.go (64) | store/cfgstore (store_models.go, 411) |
| новый store_api.go: адаптер портов компонентов, проводка (NewStore), 47 делегаторов (mcp 33, oauth 9, settings 5) | store/cfgstore |
| store_mcp.go (408) + mcp_uncertainty.go (78) | store/mcpstore (store_mcp.go, 486; тип Store, конструктор, порт) |
| store_mcp_admission.go (741), store_mcp_json.go (671), store_mcp_locks.go (358), docker_mcp.go (153) | store/mcpstore |
| store_mcp_transaction.go (927 → сплит ~600 + ~328) | store/mcpstore (store_mcp_transaction.go + store_mcp_transaction_apply.go) |
| store_oauth.go (645), store_oauth_disk_sync.go (196), store_oauth_pending.go (120), store_oauth_provider.go (126), store_oauth_refresh.go (426) | store/oauthcred |
| store_oauth_journal.go (остаток ~200), store_oauth_journal_outbox.go (258), store_oauth_journal_quarantine.go (115), store_oauth_journal_worker.go (426) | store/oauthjournal |
| settings_lock.go (+settings_lock_hashes.go → 669), settings_lock_sources.go (114), settings_lock_sources_other.go (20), settings_lock_sources_windows.go (21), settings_authority.go (44), settings_models_integrity.go (методы, ~120) | store/settings |
| copilot.go | файл-заглушка (1 строка `package config`) — удалить в D2-4, поведения нет |
| Новые: facade.go; docread candidates.go/stabledocs.go/fingerprints.go/normalize.go/mcpdocs.go; slots/oauthjournal.go | internal/config, io/docread, io/slots |

### 2.4. Тесты (183 файла, все `package config`)

Правило: тест едет в пакет кластера, чьё поведение он проверяет; исторические префиксы решены по импортам и вызываемым идентификаторам (решение Г1, отложенного «по поведению на полосе» больше нет).

| Тесты | Целевой пакет |
|---|---|
| store_mcp_test.go, store_mcp_admission_config_slot_test.go, store_mcp_admission_kept_legacy_test.go, store_mcp_admission_sibling_fence_test.go, store_mcp_format_test.go, store_mcp_identity_unix_test.go, store_mcp_ktav_test.go, store_mcp_q5c_test.go, store_mcp_regression_test.go, docker_mcp_test.go | store/mcpstore (10) |
| store_oauth_test.go, store_oauth_credential_race_test.go, store_oauth_disk_sync_test.go, store_oauth_ktav_test.go, store_oauth_matrix_test.go, store_oauth_matrix_harness_test.go, store_oauth_predecessor_authority_test.go, store_oauth_refresh_cas_test.go, store_oauth_refresh_pending_test.go, store_oauth_refresh_race_test.go, store_oauth_refresh_test.go, store_oauth_task1292_authority_test.go, store_oauth_task1292_engine_test.go, store_oauth_task1292_legacy_test.go, store_oauth_task1292_wait_test.go, store_oauth_task1296_digestless_test.go, store_oauth_task1300_adoption_test.go, store_codex_oauth_test.go, store_codex_oauth_race_test.go | store/oauthcred (19) |
| store_oauth_journal_test.go, store_oauth_journal_contract_test.go, store_oauth_journal_mutation_test.go, store_oauth_journal_order_test.go, store_oauth_journal_outbox_test.go, store_oauth_journal_quarantine_test.go, store_oauth_journal_recovery_test.go, store_oauth_journal_refresh_recovery_test.go, store_oauth_journal_restart_test.go, store_oauth_journal_testsupport_test.go, store_oauth_burned_digest_test.go, store_oauth_outbox_regress_test.go | store/oauthjournal (12) |
| settings_authority_overlap_test.go, settings_authority_test.go, settings_lock_audit_test.go, settings_lock_exemptions_test.go, settings_lock_kdf_test.go, settings_lock_kdf_testmain_test.go (TestMain едет с тестом — урок §3.12 плана), settings_lock_migrate_test.go, settings_lock_regression_test.go, settings_lock_test.go, settings_models_integrity_test.go | store/settings (10) |
| store_test.go, store_write_test.go, store_write_durability_test.go, store_write_lock_timeout_test.go, store_write_reload_fixture_test.go, store_reload_test.go, store_reload_consistency_test.go, store_reload_providers_test.go, store_reload_resolver_context_test.go, store_reload_round13_test.go, store_reload_windows_test.go, store_staleness_test.go, store_snapshot_race_test.go, store_models_test.go, store_models_ktav_test.go, store_config_disk_sync_test.go, store_config_disk_sync_boundary_test.go, store_crossprocess_test.go, store_crossprocess_ktav_test.go, store_slot_lock_regression_test.go, p464_cascade_test.go (каскад UpdatePreferredModel через реальный ConfigStore), recent_models_test.go | store/cfgstore (22) |
| config_* (case_sensitivity, commit*, durability, file_identity, open, owner, parent_identity, rename*, write_identity) | io/casefold, io/cfgcommit, io/fileid, io/fswap по суффиксу; config_cycle7/9, config_commit_outcome/recovery → io/cfgcommit |
| ktav_* | io/slots, io/slotmigrate |
| mcp_json*, mcp_revisions* | io/docread |
| peak_* → domains/peak; apitoken_* → domains/apitoken; api_remotes* → domains/apiremote; server_listeners* → domains/listeners; resolve_* → model/envresolve; datadir, global_dir*, global_testguard* → domains/datadir | — |
| model_*, provider*, catwalk*, hyper*, catalog_supplement*, codex_provider* | providers/catalogsrc, providers/provresolve |
| catalog_dump_userset_test.go | providers/catalogsrc |
| crush_env_overlay_test.go, discover_integration_test.go, zero_dump_merge_test.go (вызывают ConfigureProviders/rushEnvOverlay) | providers/provresolve |
| live_provider_catalog_test.go (живой ростер каталога, кэш efforts) | providers/catalogsrc |
| p1_2_test.go (RemoveProviderAPIKey), p1_3_test.go (SetProviderRuntimeConfig, атомарность снапшота через ту же операцию) | store/oauthcred |
| attribution_migration_test.go (cfg.setDefaults + loadFromBytes) | model/merge (cfgmerge) |
| finalization_flag_test.go, network_test.go, network_proxy_none_test.go, wake_tools_names_test.go, agent_id_test.go | cfgmodel; ИЗ network_proxy_none_test.go тест `TestProviderProxyNoneSurvivesConfigureProviders` → provresolve; ИЗ responses_transport_test.go тест `TestResponsesTransportScopedWriteRejectsBeforeDiskCommit` → store/cfgstore |
| responses_transport_test.go (остальные: типизированная политика, приоритеты) | cfgmodel |
| home_flag_explicit_datadir_test.go, startup_notices_test.go | cfgload |
| workspace_root_nogit_test.go | domains/datadir |
| testutil_isolate_test.go (помощник `isolateAllGlobalConfigPaths`, функций Test нет) | store/cfgtest (переезжает как непроверяемый помощник) |
| task1314_oauth_disk_sync_test.go | #95: после S3/S4 → `internal/app/itest` или фейк codexprovider |
| catalog/catalog_test.go | providers/catalog (не белый) |

`store_oauth_matrix_test.go` (`TestOAuthRound7_CartesianMatrix`, 160–200 с) едет в store/oauthcred **без изменения теста** (В2). Второй храповик (каталог ≤ 21 файла с тестами, число только падает): сиды на момент полос — cfgstore 29, oauthcred 24, mcpstore 17, oauthjournal 16, settings 16, остальные по факту; цели ≤ 21 к приёмке Z (oauthcred: 24 → слияние двух task1292-файлов — механическое, в D2-6).

### 2.5. Публичный API и фасад

**Решение: публичный пакет — `internal/config` (package `config`), файл `facade.go`.** `config.X` используют ~500 файлов в 13 группах (178 идентификаторов); фасад из type-алиасов и forwarding-функций не создаёт изменяемых пакетных переменных (В6). Извлечение компонентов не меняет внешний API: методы компонентов остаются методами `*ConfigStore` как тонкие делегаторы в `store_api.go`, поэтому алиас `type ConfigStore = cfgstore.Store` несёт весь метод-сет, внешние вызовы не переписываются.

Группы экспортируемых имён: 1) модель (Config, ProviderConfig, MCP*, Scope*, SelectedModel*, Options, TUIOptions, Permissions, Agent/AgentCoder/AgentTask, HookConfig, ToolLs/Grep, CustomRole*, PeakHours*, ProviderPeakHours, ResponsesTransport*, NetworkConfig, ProxyNone, ModelEffortInfo, FinalizationOptions, Attribution/TrailerStyle*, ProjectInitFlag, ProviderHTTPClientFactory, RuntimeOverrides); 2) композиция (Load, Init, WithCLIModels, ResolveDataDirectory, NewLibraryStore, InitFlagFilename); 3) store (ConfigStore, CommitOutcome, DataDirSource*, MCPAdmission*, MCPMutationResult, ErrMCP* 8, SettingsWriteAuthority — тип переезжает в cfgmodel, SettingsAuthorityGuarded/TrustedUI); 4) глобальные пути (GlobalConfig/Data/Dir/DirEnv/WorkspaceDir, WorkspaceHome/Root, SystemConfig, EnsureGlobalDirTree, MarkSharedWorkspace, CanonicalAPIWorkspace, GlobalSkillsDirs, ProjectSkillsDir, Global/ProjectPromptSkillsDirs); 5) ktav/файлы (ActiveConfigFileName, ReadConfigDocumentLight, KtavAutoMigrateEnabled, CommitOutcomeFromError); 6) peak (ParsePeakClock/Days/PauseDuration, FormatPeakDays, PeakHoursMaxWindows, ResolvePeakTimezone); 7) токены (APIToken*, GlobalAPITokenStore, LooksLikeAPIToken, APITokenScopeAllows, ConsumeServeBootstrapToken*, ErrAPITokenNotFound/Invalid); 8) настройки/пароль (SettingsEvent, SettingsWriteAllowed, HashPassword, VerifyPassword, CheckPasswordAgainstDisk, SetProcessPassword, ErrWrongPassword, ErrSettingsLocked/LockUnrecognized, ErrModelsEditedDirectly, ErrNoWorkspaceConfig, ErrListenerConfigRead); 9) каталог провайдеров (Providers, Fetch/Refresh/UpdateProviderModels, UpdateProviders, UpdateHyper, Cached/PersistConfirmedModels, ClearModelCatalogCache, ValidateCustomRoleName/Selection, ProjectConfigs); 10) резолвер (NewShellVariableResolver, VariableResolver, IdentityResolver, WithExpander, ResolveValueContext, ErrContextResolverUnsupported); 11) диск-синк (Start/StopConfigDiskSync, ConfigDiskSyncOptions, Start/StopOAuthDiskSync); 12) тестовые швы (NewTestStore*, ResetProviderCacheForTests, Set*ForTest, SetSettingsAuditSink); 13) remotes/listeners (APIRemote*, ServerListener*, GlobalServerListenersWithSources, ClientAuth, CipherSuites, ClientCAs, ErrNotLocked).

Прямые импорты под-пакетов допускаются только внутри `internal/config/**` (кроме существующих импортёров `catalog`: `agent/cliprovider`, `agent/hyper` — путь перепишет скрипт B).

### 2.6. Граф зависимостей (стрелки = импорты, снизу вверх)

```
oauth, catalog, discover(+local), casefold, fileid, fswap, envresolve   (листья)
peak → (stdlib)                                    cfgmodel → peak, oauth, csync, env, shell, catwalk(типы)
cfgmerge → cfgmodel, peak
slots → cfgcommit → fileid, fswap, casefold ;  slots → oauth, cfgcommit
docread → slots, cfgcommit, fileid, datadir, cfgmodel, oauth
datadir → cfgmodel ;  slotmigrate → slots, cfgcommit, docread, datadir, cfgmodel
catalogsrc → cfgmodel, catalog, discover, slots, datadir
provresolve → cfgmodel, catalog, catalogsrc, peak, datadir, slots
mcpstore → cfgmodel, cfgcommit, fileid, fswap, casefold, slots, datadir, envresolve(типы из cfgmodel)
oauthcred → cfgmodel, oauthjournal, cfgcommit, fileid, slots, docread, datadir
oauthjournal → cfgmodel, io/slots, cfgcommit, datadir
settings → cfgmodel, cfgcommit, fileid, docread, datadir
cfgstore → ВСЕ компоненты (mcpstore, oauthcred, oauthjournal, settings) + cfgmodel, cfgmerge, docread, slots, cfgcommit, fileid, fswap, casefold, slotmigrate, datadir, peak, provresolve, catalogsrc, catalog, oauth
apitoken → docread, fileid, cfgcommit, datadir, cfgmodel ;  listeners → docread, slots, datadir, fileid
apiremote → cfgload, cfgstore, cfgcommit, docread, datadir, cfgmodel
cfgload → cfgstore, provresolve, catalogsrc, docread, cfgmerge, datadir, peak, envresolve, cfgmodel
config (фасад) → все
```
Компоненты НЕ импортируют cfgstore: зависимость «назад к ядру» — только через порт (интерфейс объявлен в компоненте, реализован адаптером в cfgstore, §3.6). `apiremote→cfgload` — существующий ленивый `globalAPIRemoteStore() { return Load("","",false) }` (`api_remotes.go:108`), ациклично; сужение — P3.

## 3. Швы

Оракул чистого переноса (сортированный список `^(func|type|var|const)` и число Test-функций) обязателен для D2-полос. Шовные полосы (0б-3.1–3.3, X1–X4) законно меняют объявления; для каждой — аргумент сохранения поведения.

### 3.1. S1 — нижний пакет модели

Переезжают: `Config`, `ProviderConfig`, `SelectedModel(Type)`, `ModelPreset`, `Agent`, `Tools/ToolLs/ToolGrep`, `HookConfig`, `MCP*`, `Options`, `TUIOptions`, `Permissions`, `NetworkConfig`, `ResponsesTransport*`, `FinalizationOptions`, `Attribution/TrailerStyle*`, `Scope*`, `ModelEffortInfo`, `ProviderPeakHours` + `peakHoursPolicyKeys` (в peak), `CustomRole` + `ValidateCustomRoleName`, `canonicalModelsJSON`, `modelsIntegritySignature`, `settingsKeyFirstComponent`, `ErrModelsEditedDirectly`, `ErrNoWorkspaceConfig`, `defaultContextPaths`, `appName`, `defaultDataDirectory`, `defaultInitializeAs`, методы `setDefaults`, `ValidateHooks`, `IsDockerMCPEnabled`, `EffectiveResponsesTransport`, `AgentFinalizationEnabled`, `ProviderHTTPClient` + фабрика (provider_http.go целиком), `MatchRoleToolPattern`, `ValidateCustomRoleSelection`, `RoleToolsAllow`, **`SettingsWriteAuthority` + константы `SettingsAuthorityGuarded/TrustedUI` + `settingsAuditSource`/`liftsSettingsLock` (settings_authority.go:44 строки → cfgmodel/settings_authority.go)** — тип-словарь без состояния, нужен и ядру, и компоненту settings, и агенту/серверу.
Аргумент поведения: декларации и методы над структурой; моменты вызова не меняются; поле `ProviderConfig.PeakHours` меняет тип на `*peak.ProviderPeakHours` с теми же JSON-тегами.

### 3.2. S2 — IO-слой: стабильное чтение и ошибки уезжают вниз

Перегруппировка: `candidates.go`, `stabledocs.go`, `fingerprints.go`, `normalize.go`, `mcpdocs.go` (состав §2.3); владельцы ошибок: `errConfigOwnerMismatch`, `errStableReadUnstable`, `ErrConfigNonRegular`, `readOpenedConfigBytes` → fileid; `errReloadDiskChanged`, `ErrConfigReloadUnstable` → docread; формат OAuth-журнала → io/slots/oauthjournal.go; решения над журналом остаются в компонентах oauthcred/oauthjournal.
Поведение: перегруппировка деклараций внутри пакета — чистый перенос; порядок open→identity→read→fingerprint→commit не меняется; `oauthJournalConfigSyncParent = syncConfigParent` (`store_oauth_journal.go:35`) уезжает в slots целиком.

### 3.3. S3 — разворот provider↔store

`configureProviders` (load_providers.go:26) → функция с параметрами `dropField func(Scope,string)` и `cliModels func()[]catalog.LocalCLIModel` (использования store — ровно два: `load_providers.go:210` и `:419-420`); `defaultModelSelection` (load_providers.go:650) → функция; `configureSelectedModels` (load_providers.go:798) → `ConfigureSelectedModels(cfg, knownProviders)` (параметры store/persist не используются, load_providers.go:796); `noteEndpointDroppedProvider` → экспортируемые акцессоры `Config.NoteEndpointDroppedProvider`/`Config.EndpointDroppedProviders` в cfgmodel. Точки вызова: `load.go:338`, `store_reload.go:563` (передают `s.removeConfigFieldBestEffort` и `s.cliModels` замыканиями), `load.go:351`.
Поведение: `dropField`/`cliModels` вычисляются в момент вызова, как и раньше; оба вызова под теми же `reloadMu`+`publishMu` — тайминги записи (`internalConfigWriteLockTimeout`) не меняются.

### 3.4. S4 — протокол начальной загрузки store

`loadOnce` (load.go:156-162, 301-308) строит `&ConfigStore{…}` литералом и держит `reloadMu`→`publishMu`→`initializing`→`initialLoadFingerprints` руками. Шов: `NewStore(...)` + `InitialLoad(fingerprints, build func(publish func(*InitialSnapshot) error) error) error` в cfgstore; порядок захватов и состав снапшота 1:1 как в load.go:301-336 (взаимная блокировка из комментария load.go:279-300 сохраняется, т.к. порядок тот же).

### 3.5. S5 — точечные перемещения «вниз»

`cachePathFor` (provider.go:71) → datadir; `WorkspaceHome`/`worktreeRoot`/`worktreeRootCache` (load_worktree.go) → datadir; `canonicalConfigPath`/`projectBoundary`/`sameDir` (load_files.go) → datadir/globalpaths.go; `GlobalConfig`/`GlobalConfigData`/`SystemConfig`/`GlobalDir*` (load_paths.go) → datadir; границы provider→store и ktav→oauthstore исчезают (CustomRole-функции → cfgmodel/cfgmerge; формат журнала → slots); `mcp_json.go`+`mcp_revisions.go`+`initialMCPRevisions` → docread (потребители load.go:211,323-324).

### 3.6. S6 — извлечение компонентов хранилища (X1–X4)

Общая схема. Компонент — отдельный пакет под `store/` со своим типом `Store` (или `Workers`), своим состоянием и узким портом. **Интерфейс порта объявляется в пакете компонента, реализуется адаптером `corePort` в cfgstore; компоненты не импортируют cfgstore** — направление «компонент → ядро» существует только как вызов метода порта. Внешний API не меняется: каждый внешне вызываемый метод компонента остаётся методом `*ConfigStore` — делегатор в `store/cfgstore/store_api.go` вида `func (s *ConfigStore) AddMCP(...) (...) { return s.mcp.AddMCP(...) }`. Внутренние (неэкспортируемые извне) вызовы переключаются на `s.mcp.`/`s.oauth.`/`s.settings.` напрямую — для них делегаторов нет.

**Порт (состав для всех компонентов, у каждого — своё подмножество ≤ 15 имён):**

| Имя порта | Семантика | Блокировки внутри реализации |
|---|---|---|
| `ViewSnap(fn func(SnapView) error) error` | атомарное чтение снапшота: `SnapView = { Config() *cfgmodel.Config; WorkspacePath() string; Generation() uint64 }` | нет (atomic.Load) — безопасно в колбэках |
| `PublishMutation(reason string, build func(cur SnapView) (next *cfgmodel.Config, write func() error, changed bool), authority cfgmodel.SettingsWriteAuthority) error` | copy-on-write мутация + запись на диск + публикация одного поколения | `publishMu` → `diskWriteMu` → (write: sidecar-локи компонента) → `publishLocked`; затем `autoReloadAfterWrite` при changed |
| `MutateConfig(reason string, mutate func(*cfgmodel.Config) bool) error` | только в памяти (= updateConfig, store.go) | `publishMu` |
| `ScopePath(scope cfgmodel.Scope) (string, error)` | = configPath | нет (чтение снапшота/неизменяемых полей) |
| `GlobalDataPath() string`, `WorkingDir() string` | неизменяемые поля | нет |
| `DiskWrite(fn func() error) error` | лист-блокировка diskWriteMu | только `diskWriteMu` |
| `NoteInitialLoadWrite()` | = noteInitialLoadWriteLocked | требует publishMu — вызывается ТОЛЬКО внутри колбэка PublishMutation |
| `AutoReload() error` | = autoReloadAfterWrite | `reloadMu.TryLock` (дедуп, не блокирует) |
| `Generation() uint64` | = Generation | нет |
| mcp-доп.: `SystemConfigPath() string`, `WorkingDirOwner() (int, error)`, `SettingsGuard(authority) error` (делегирует в settings) | — | нет / внутри SettingsGuard — локи settings-порта |
| oauth-доп.: `HasConfigField(scope,key) bool`, `SetConfigFields(scope, fields, authority) error`, `RemoveConfigField(scope,key,authority) error`, `WithFileWriteLock(path, fn func(configWriteTarget) error) error` | = одноимённые операции ядра | внутри WithFileWriteLock — sidecar-лок (лист) |

**Доказательство порядка блокировок.** Документированный порядок ядра: `reloadMu` → `publishMu` → `diskWriteMu` → отсортированные sidecar-локи (комментарий store_mcp_locks.go: «Lock order is publishMu (caller) -> diskWriteMu -> sorted sidecar locks»; расширение reloadMu→publishMu — load.go:301-304). Инварианты после извлечения: (1) компоненты не владеют ни одним локом ядра и порты не экспортируют методы захвата — инверсия невозможна конструктивно; (2) единственная точка `publishMu` для компонентов — `PublishMutation`/`MutateConfig`; колбэк `build` выполняется под `publishMu` и договорённо не вызывает `PublishMutation`/`MutateConfig`/`DiskWrite`/`NoteInitialLoadWrite` (sync.Mutex нереентерабелен — нарушение deadlock'ится мгновенно и ловится серийными тестами полосы); внутри колбэка допустимы только собственные локи компонента (`credentialWriteMu`, `oauthWorkerMu`, sidecar-локи MCP) и порты без блокировок (`ViewSnap`, `ScopePath`, `Generation`) — порядок `publishMu → (локи компонента/sidecar)` совпадает с документированным; (3) `DiskWrite` — лист; (4) `AutoReload` — только TryLock после снятия publishMu, ожидания нет. Сегодняшние прямые `s.publishMu.Lock()` в oauth (36 вхождений) и mcp (14) сворачиваются в `PublishMutation` — те же критические секции, тот же порядок, тот же CAS по `generation` (publishLocked, store.go).

**Обратные вызовы ядро → компонент (прямые методы на полях-компонентах, cfgstore импортирует компоненты):** reload → `s.mcp.UncertaintyVersions(names)`, `s.mcp.ClearUncertainty(expected)`, `s.mcp.OwnerPolicyFor(path)`, `s.oauth.OverlayPendingRotation(next *cfgmodel.Config)`; запись → `s.settings.GuardWrite(authority)`, `s.settings.ScopeForPath(path)`, `s.settings.AnyLockExists()`; загрузка/закрытие → `s.journal.RecoverAtLoad()`, `s.settings.VerifyModelsIntegrityAtLoad()`, `s.oauth.StopDiskSync()`, `s.journal.Stop()`.

**S6.1 settings → store/settings (полоса X1).** Тип `Store`; переносится 16 методов: `LockState`, `CheckProcessPassword`, `SettingsLocked`(p), `guardSettingsWrite`(p), `guardSettingsWriteForAuthority`(p), `lockHashesForTarget`(p), `LockSettings`, `UnlockSettings`, `writeSettingsLockRaw`(p), `emitLockEvent`(p), `settingsScopeForPath`(p), `checkExternalSettingsSources`(p), `anySettingsLockExists`(p), `verifyModelsIntegrityAtLoad`(p), `ModelsIntegrity`, + пакетная функция `SetSettingsAuditSink` и тип `SettingsEvent`. Переменные-состояния: `settingsPasswordIterations` (с ней переезжает init(), §4.2), `reservedSettingsKeys`, `canonicalGlobalDataPath` (settings_lock_sources.go:10). Порт (8): ViewSnap, ScopePath, GlobalDataPath, WorkingDir, NoteInitialLoadWrite, AutoReload, WithFileWriteLock, SetModelsIntegrity(error). Поле `modelsIntegrity` остаётся в ядре (пишется один раз в Load) — доступ через порт. Делегаторы (5): `LockState`, `CheckProcessPassword`, `LockSettings`, `UnlockSettings`, `ModelsIntegrity`. Файлы — 6 (§2.1); строки 669/114/20/21/44/~120. Тесты — 10 файлов (§2.4), включая TestMain из settings_lock_kdf_testmain_test.go.

**S6.2 mcpstore → store/mcpstore (полоса X2).** Тип `Store` (+конструктор, порт); переносится 76 методов: `ResolveMCPWritableScope`, `PersistMCPConfig`, `PersistMCPConfigResultWithAuthority`, `PersistMCPConfigExact`, `PersistMCPConfigResult`, `PersistRemoveMCPConfig`, `PersistRemoveMCPConfigExact`, `PersistRemoveMCPConfigResult`, `PersistRemoveMCPConfigResultWithAuthority`, `PersistRemovePendingMCPConfigResult`, `PersistRemovePendingMCPConfigResultWithAuthority`, `PersistRemovePendingMCPConfig`, `PersistMCPDisabledOverride`, `PersistMCPDisabledOverrideResult`, `PersistMCPDisabledOverrideResultWithAuthority`, `PersistMCPEnableRollbackResult`, `PersistMCPEnableRollbackResultWithAuthority`, `PersistMCPDisabledOverrideExact`, `PersistMCPDisabledOverrideExactResult`, `PersistMCPDisabledOverrideAtScope`, `PersistMCPDisabledOverrideInScope`, `PersistReplaceMCP`, `PersistReplaceMCPResult`, `PersistReplaceMCPResultWithAuthority`, `PersistReplaceMCPInScope`, `PersistReplaceMCPAtScope`, `PersistMCPFields`, `PersistMCPFieldsExact`, `PersistMCPConfigAtScope`, `PersistRemoveMCPConfigAtScope`, `PersistMCPFieldsAtScope`, `PersistMCPConfigInScope`, `PersistRemoveMCPConfigInScope`, `PersistMCPFieldsInScope`, `publishMCPValueAndStalenessLocked`(p), `WithCurrentMCPMutation`, `WithCurrentMCPAdmission`, `WithCurrentMCPAdmissionContext`, `prepareMCPAdmissionToken`(p), `validateMCPAdmissionTokenContext`(p), `mcpAdmissionSnapshotCurrent`(p), `mcpMutationResultCurrentLocked`(p), `withMCPWriteLocks`(p), `withMCPAdmissionLocks`(p), `withMCPAdmissionLocksContext`(p), `withMCPLocks`(p), `withMCPLocksUsingCaseSensitivity`(p), `mutateMCP`(p), `mutateMCPExact`(p), `mutatePendingRemoveMCP`(p), `mutatePendingRemoveMCPWithAuthority`(p), `mutateMCPEnableRollback`(p), `mutateMCPEnableRollbackWithAuthority`(p), `mcpEnableRollbackTokenMatches`(p), `mutateMCPWithMode`(p), `prepareMCPFileMutation`(p), `writeMCPFileChanges`(p), `reconcileMCPCommit`(p), `applyCommittedMCPFingerprint`(p), `evaluateMCPFiles`(p), `orderedMCPPaths`(p), `systemConfigPathValue`(p), `mcpPathData`(p), `mcpOwnerPolicy`(p), `verifyMCPReadOnlyInputs`(p), `publishMCPMutationLocked`(p), `configPathOrEmpty`(p), `workspacePathValue`(p), `MarkMCPUncertain`, `MCPUncertaintyVersion`, `MCPUncertaintyVersions`, `clearMCPUncertainty`(p), `PrepareDockerMCPConfig`, `PersistDockerMCPConfig`, `EnableDockerMCP`, `DisableDockerMCP` + из store.go: `AddMCP`, `UpdateMCP`, `RemoveMCP`, `RemoveMCPIfCurrent`, `SetMCPDisabled`, `SnapshotMCPAdmission`. Поле `mcpUncertainty` (+тип mcpUncertaintyState) → компонент. Переменные: `dockerMCPVersionRunner`, `dockerMCPAvailabilityCache`, `dockerMCPRefreshGroup`. Порт (11): ViewSnap, PublishMutation, MutateConfig, ScopePath, GlobalDataPath, WorkingDir, SystemConfigPath, WorkingDirOwner, DiskWrite, NoteInitialLoadWrite, SettingsGuard. `MCPConfig` (акцессор cfg.MCP, store.go) остаётся в ядре. Делегаторы (33): перечисленные Persist*/With*/MarkMCPUncertain/MCPUncertainty*/ResolveMCPWritableScope/SnapshotMCPAdmission/AddMCP/UpdateMCP/RemoveMCP/RemoveMCPIfCurrent/SetMCPDisabled. Обратные (3): UncertaintyVersions, ClearUncertainty, OwnerPolicyFor. Файлы — 7 (сплит transaction, §2.1); строки 486/741/671/358/~600/~328/153. Тесты — 10.

**S6.3 oauthjournal → store/oauthjournal (полоса X3).** Тип `Workers`; переносится 13 методов: `startOAuthJournalWorker`(p), `runOAuthJournalWorker`(p), `oauthJournalWorkerAttempt`(p), `installOAuthOutbox`(p), `oauthOutboxCurrent`(p), `clearOAuthOutbox`(p), `settleOAuthOutbox`(p), `flushOAuthOutbox`(p), `stopOAuthJournalWorkers`(p), `drainOAuthOutboxOnStop`(p), `recoverOAuthJournalsAtLoad`(p), `recoverCodexOAuthJournal`(p), `withOAuthJournalRecoveryWriteLock`(p). Поля: `oauthWorkerMu`, `oauthWorkerCtx`, `oauthWorkerCancel`, `oauthWorkers`, `oauthWorkerWG`, `oauthOutbox`; переменная `oauthJournalLogs`. Порт (5): ViewSnap, PublishMutation, ScopePath, GlobalDataPath, NoteInitialLoadWrite. Делегаторов нет (все методы внутренние). Файлы — 4; строки ~200/258/115/426. Тесты — 12 (включая testsupport-помощник).

**S6.4 oauthcred → store/oauthcred (полоса X4).** Тип `Store` (+поле `journal *oauthjournal.Workers`); переносится 36 методов: `SetProviderAPIKey`, `SetProviderAPIKeyWithAuthority`, `SetProviderRuntimeConfig`, `SetProviderRuntimeAPIKeyIfTemplate`, `RefreshOAuthToken`, `RefreshOAuthTokenWithClient`, `refreshLegacyOAuthLeader`(p), `setOAuthTokenIfCurrent`(p), `setOAuthTokenIfCurrentWithLock`(p), `applyToken`(p), `applyOAuthToken`(p), `applyOAuthTokenWithPending`(p), `applyOAuthTokenWithPendingPredecessor`(p), `loadTokenFromDisk`(p), `ImportCopilot`, `RemoveProviderAPIKey`, `RemoveProviderAPIKeyWithAuthority`, `StartOAuthDiskSync`, `runOAuthDiskSync`(p), `syncOAuthFromDisk`(p), `StopOAuthDiskSync`, `OAuthDiskSyncRunning`, `overlayPendingOAuthLocked`(p), `recordPendingOAuthLocked`(p), `clearPendingOAuth`(p), `pendingOAuthCurrent`(p), `refreshLegacyOAuthToken`(p), `codexRefreshAlreadyCurrent`(p), `codexOAuthAuthorityRecord`(p), `preflightCodexDiskAuthority`(p), `adoptCodexDiskAuthority`(p), `refreshCodexOAuthToken`(p), `refreshCodexOAuthLeader`(p), `applyCodexDiskLogout`(p), `pollCodexRefreshDisk`(p), `withCodexConfigWriteLock`(p). Поля: `pendingOAuth`, `credentialWriteMu`, `credentialSetBeforePublish`, `oauthDiskSyncStarted`. Переменные: `codexRefreshActive`, `errOAuthCredentialCAS`, `oauthProviderDescriptorsOverride` (store_oauth_provider.go). Порт (13): ViewSnap, PublishMutation, MutateConfig, ScopePath, GlobalDataPath, WorkingDir, DiskWrite, NoteInitialLoadWrite, AutoReload, HasConfigField, SetConfigFields, RemoveConfigField, WithFileWriteLock. Делегаторы (9): `SetProviderAPIKeyWithAuthority`, `SetProviderRuntimeConfig`, `SetProviderRuntimeAPIKeyIfTemplate`, `RefreshOAuthToken`, `RefreshOAuthTokenWithClient`, `RemoveProviderAPIKeyWithAuthority`, `StartOAuthDiskSync`, `StopOAuthDiskSync`, `OAuthDiskSyncRunning`. Обратные (1): OverlayPendingRotation. Файлы — 5; строки 645/196/120/126/426. Тесты — 19, включая матрицу (В2).

**S6.5 Конечное состояние ядра cfgstore — 7 файлов, ≤ 900 строк каждый.** Структура `ConfigStore`: 43 поля → 36 (−11: mcpUncertainty, pendingOAuth, oauthOutbox, oauthWorkerMu, oauthWorkerCtx, oauthWorkerCancel, oauthWorkers, oauthWorkerWG, oauthDiskSyncStarted, credentialWriteMu, credentialSetBeforePublish; +4: mcp, oauth, journal, settings — указатели на компоненты, проводятся в NewStore/InitialLoad). Файлы: store.go 832, store_write.go 746, store_reload.go ~650, store_reload_flow.go ~316, store_sync.go 719, store_models.go 411, store_api.go ~280 = **7 файлов, 7 элементов**. Оставшиеся в ядре методы — 88 внешних/служебных: снапшот-доступ (Config, Generation, Snapshot*, Resolver, Resolve, KnownProviders, LoadedPaths, WorkingDir, DataDirSource, WorkspaceHome, MayMigrateData, Overrides, SetSkipPermissionRequests, SetupAgents, LogPath, MCPConfig), запись (configPath, HasConfigField, HasWorkspaceConfig, withConfigWriteLock*, resolveConfigWriteTarget, Set/RemoveConfigField*, noteInitialLoadWriteLocked), reload/staleness (ReloadFromDisk, reloadFromDiskUnlocked, runReloadLocked, buildAndPublishReload, autoReload*, ConfigStaleness, Refresh/CaptureStalenessSnapshot), диск-синк (Start/StopConfigDiskSync, PollConfigDiskOnce, ConfigDiskSyncRunning), модели (ReadModelsAtScope, Read/UpdateAllPreferredModels*, UpdatePreferredModel*, SetSelectedModelRuntime, UpdateAgentAllowedTools, Record/RemoveRecentModel, SetCustomRole*, RemoveCustomRole*, CustomRolesForScope, PeakHoursEntryForScope), тест (NewTestStore*, NewLibraryStore).

**Сводка имён через границы (правило ≤15):** порты компонентов 5/11/13/8 — ок; фасад 178 — по дизайну; IO-внутренности ~35 однонаправленно — исключение с решением (§6.3); docread→выше ~20 — P3 на сужение.

## 4. Глобалы, init(), embeds, OS-файлы, белые тесты, помощники

### 4.1. Пакетные переменные (каждой — ровно один владелец)

| Владелец | Переменные |
|---|---|
| cfgstore | `reloadMutex` (reload_mutex.go → store_sync.go), `dockerMCPVersionRunner/AvailabilityCache/RefreshGroup` → mcpstore, `errKtavWriteNotSupported`→slots, `errReloadProviderEndpointDropped` |
| mcpstore | `dockerMCPVersionRunner`, `dockerMCPAvailabilityCache`, `dockerMCPRefreshGroup` |
| oauthcred | `codexRefreshActive`, `errOAuthCredentialCAS`, `oauthProviderDescriptorsOverride` |
| oauthjournal | `oauthJournalLogs`; `oauthJournalConfigSyncParent`→slots |
| settings | `settingsPasswordIterations` (+init), `reservedSettingsKeys`, `canonicalGlobalDataPath` (settings_lock_sources.go:10) |
| catalogsrc | `providerOnce/providerList/providerErr`, `catwalkSyncer/hyperSyncer`, `supplementalCatalogModels`, `modelVerificationTimeout`, `modelVerifyOnlineForTests`, `cachedModelCatalogProviders`; `providerHTTPClientFactory`→cfgmodel |
| cfgload | `loadAfterProviderConfigTestHook`, `discoverZAIEffortDocs` |
| datadir | `workspaceHomeFlag` (+init), `linkedWorktreeCache`, `devBuildCache`, `worktreeRootCache` |
| slots | `ktavBothFormatsWarned`, `oauthJournalConfigSyncParent` |
| slotmigrate | `ktavAutoMigrate`, `ktavMigrationBudget`, `ktavArtifactMigrationStepHook` |
| cfgcommit | `configTestHooks`, ошибки `errConfigCommit*`, `ErrConfigHardLink`, `errAtomicWriteCommitted`; `errConfigCaseDirectoryBudgetExceeded`→casefold |
| cfgmodel | `defaultContextPaths`; поле `Config.endpointDroppedProviders` — поле, не пакетная переменная |
| peak | `peakDayNames` |
| apitoken | `ErrInvalidAPIToken`, `ErrAPITokenNotFound`, `apiTokenClock` |
| apiremote | `apiRemoteNamePattern/TokenPattern/EnvPattern/apiRemoteMutationMu` |
| envresolve | `ErrContextResolverUnsupported` |

Ни одна не экспортируется ради разрезки.

### 4.2. init() — оба сохраняются, порядок не важен

1. `datadir.go:481` → domains/datadir. 2. `settings_lock.go:88` → store/settings (едет с файлом). Пишут разные переменные, не читают ничьих; после разрезки порядок = порядок импортов, оба ставят константы до первого использования (`Load` переопределяет `workspaceHomeFlag` в load.go:119). Проверка в D2: `grep -rn 'func init('` — ровно эти два.

### 4.3. OS-файлы (30) — парами с кодом

io/fileid (7): config_file_identity_{unix,windows}, config_parent_identity_{unix,windows}, config_linkat_unix, config_owner_{unix,windows}. io/fswap (7): config_open_{unix,windows}, config_rename_{darwin,unix,windows}, config_renameat2_{linux,other_unix}. io/casefold (6 + нейтральный config_case_sensitivity.go). io/cfgcommit (4): config_commit_{unix,windows}, config_durability_{unix,windows}. io/slotmigrate (2): ktav_migration_{unix,windows}. domains/datadir (2): systemconfig_{unix,windows}. **store/settings (2): settings_lock_sources_{windows,other}.go** — с settings_lock.go.

### 4.4. Белые тесты и помощники

Каждый из 183 тестов едет по §2.4 (решения по историческим префиксам — в таблице, отложенных нет). Тест, трогающий неэкспортированные имена двух будущих пакетов, расщепляется по поведению или уходит в cfgtest/`internal/app/itest`. Оракул: суммарное число `func Test…` не меняется. Помощники ≥2 пакетов → `store/cfgtest` (по образцу `db/dbtest`): изоляция `RUSH_GLOBAL_DATA`/`RUSH_GLOBAL_CONFIG`, конструкторы временных конфигов, `isolateAllGlobalConfigPaths` (из testutil_isolate_test.go), `resetProviderState`. Экспортированные швы остаются в своих пакетах и форвардятся фасадом.

## 5. План выполнения (полосы)

Все полосы: `gofumpt`, `go build`, `go vet` затронутых пакетов через `.rush/tmp/golock.sh`; тесты серийно оркестратором; ≤5 агентов одновременно. Порядок волны 4: 0б-3.1 → 3.2 → 3.3 → **X1 → X2 → X3 → X4** (последовательно: все X-полосы правят структуру ConfigStore и store_api.go — файловые области пересекаются); волна 5: D2-1 → (D2-2 ∥ D2-4) → D2-3 → D2-5 → D2-6 (максимум 2 агента параллельно). X-полосы создают каталоги сразу по финальным путям `internal/config/store/<компонент>` — пакет `config` остаётся в корне до D2-5.

### 0б-3.1 «Развязка provider/store/load» (S3+S4+S1-методы)
Файлы: load_providers.go, load_providers_merge.go, store.go, store_reload.go, load.go, provider_http.go, store_reload_providers.go, docker_mcp.go, provider_peak_hours.go, settings_authority.go (→cfgmodel), settings_models_integrity.go (расщепление). Приёмка: build/vet; grep «в load_providers.go нет `store.`», «в load.go нет обращений к полям store кроме новых API»; число `func Test` не изменилось; серийные тесты config.

### 0б-3.2 «IO-перегруппировка» (S2)
Создание candidates/stabledocs/fingerprints/normalize/mcpdocs/oauthjournal.go + перенос readOpenedConfigBytes и ошибок в fileid-файлы. Храповик: `dir_entries_allowlist.txt` — запись `internal/config` сознательно поднимается в том же коммите с комментарием (§5.4). Приёмка: оракул; серийные тесты store+load.

### 0б-3.3 «Точечные перемещения» (S5)
provider.go, load_paths.go, load_worktree.go, load_files.go, store_oauth_journal.go (расщепление формата), load_env.go → provresolve-файл. Приёмка: оракул; один владелец у cachePathFor/WorkspaceHome; серийные тесты.

### X1 «settings» (S6.1)
Скоуп: создание `store/settings/` (6 файлов), тип Store, порт (8), делегаторы (5) в новый store_api.go, обратные вызовы GuardWrite/ScopeForPath/AnyLockExists в store_write.go/store_reload.go, `SettingsWriteAuthority` → cfgmodel (0б-3.1 уже перенёс). Пререквизиты: 0б-3.1–3.3. Приёмка: build+vet+gofumpt; оракул — изменения объявлений только из списка полосы (16 методов: приёмник меняется, тело не меняется; +5 делегаторов; +1 поле-компонент; −3 переменные из ядра); серийно: 10 settings-тестов + store_write_test.go/store_test.go (interplay guard); grep «settings не импортирует cfgstore», «store_write.go не содержит guardSettingsWrite».

#### X1 amendment
Десять binding corrections для выполнения X1:
1. `NoteInitialLoadWrite(path, data)` требует `diskWriteMu` только внутри callback `WithFileWriteLock`; никогда не вызывается через `PublishMutation` и не допускает re-entry.
2. Порт `settings` получает `ModelsIntegrity() error` для чтения рядом с `SetModelsIntegrity`.
3. `SettingsLocked` становится шестым root delegator.
4. `settings` владеет полями `WriteTarget`, которые читает lock writer; `configWriteTarget` корня остаётся private и конвертируется при переходе.
5. Audit/event helpers образуют узкий exported API `settings`; источник и тайминг trusted-UI сохраняются идентично.
6. Чистые `modelsIntegrityOf`, `cacheRevKey`-related revision logic и `applyModelsIntegrity` переходят в `cfgmodel`.
7. `canonicalGlobalDataPath` — unexported settings var. `TestGlobalDirIsolationNoFallbackImports` remains a joined root integration test and calls `settings.SetCanonicalGlobalDataPathForTest(t testing.TB, path string)`, defined in settings `*_testsupport.go`; the helper saves/restores the unexported var via `t.Cleanup`. Other white tests that mutate `canonicalGlobalDataPath` migrate to settings.
8. Password-iteration state принадлежит `settings`; KDF init переезжает с ним, как и тесты.
9. Для путей и `AppName` используются прямые импорты `datadir`/`model`.
10. Телá — оракул, кроме вызовов по receiver/port; в отчёте перечисляются внешние места использования экспортированных имён.

### X2 «mcpstore» (S6.2)
Скоуп: `store/mcpstore/` (7 файлов, сплит transaction), 76 методов, поле mcpUncertainty, порт (11), делегаторы (33), обратные (3), docker-переменные. Пререквизиты: X1 (SettingsGuard). Приёмка: build+vet+gofumpt; оракул по списку; серийно: 10 mcp-тестов + store_reload_round13_test.go, store_snapshot_race_test.go (uncertainty interplay); grep «mcpstore не импортирует cfgstore», «store.go не содержит AddMCP/UpdateMCP».

### X3 «oauthjournal» (S6.3)
Скоуп: `store/oauthjournal/` (4 файла), 13 методов, поля oauthWorker*/oauthOutbox, oauthJournalLogs, порт (5). Пререквизиты: 0б-3.2 (формат в io/slots), X1. Приёмка: build+vet+gofumpt; оракул; серийно: 12 journal-тестов (testsupport с окружением TestMain едет целиком); grep «oauthjournal не импортирует cfgstore/oauthcred».

### X4 «oauthcred» (S6.4)
Скоуп: `store/oauthcred/` (5 файлов), 36 методов, поля pendingOAuth/credential*, порт (13), делегаторы (9), проводка journal из X3. Пререквизиты: X3. Приёмка: build+vet+gofumpt; оракул; серийно: 19 cred-тестов (матрица `TestOAuthRound7_CartesianMatrix` — по решению оркестратора, вне -short); grep «oauthcred не импортирует cfgstore», «в store.go нет pendingOAuth/credentialWriteMu».

### D2-1 «Модель + merge + envresolve»
Создание model/, model/merge/, model/envresolve/; перенос по §2.3. Пререквизиты: X-полосы. Приёмка: оракул; build ./...; grep «cfgmodel не импортирует cfgstore/provresolve/cfgload».

### D2-2 «IO-слой» (∥ D2-4)
io/{slots,slotmigrate,cfgcommit,fileid,fswap,casefold,docread}; перенос; импорты через скрипт B. Приёмка: оракул; vet; grep «io/* не импортирует store|cfgload|provresolve|cfgstore».

### D2-3 «providers» (после D2-1)
catalog/ → providers/catalog/; discover → discover+local; catalogsrc/, provresolve/ (+сплит load_providers.go 917 → ~600+~317, выход из списка >900). Приёмка: оракул; импорты agent/cliprovider, agent/hyper переписаны; build ./...; серийные тесты provresolve+catalogsrc (включая crush/discover/zero/live-тесты).

### D2-4 «domains» (∥ D2-2)
domains/{datadir,peak,apitoken,apiremote,listeners,oauth}; oauth из internal/config/oauth внутрь domains (отклонение от И§9.2 — §6.4); удаление copilot.go. Приёмка: оракул; grep «domains/* не импортирует cfgstore|cfgload» кроме apiremote→cfgload:108.

### D2-5 «store: переупаковка + фасад» (последняя)
git mv ядра в store/cfgstore/ (+сплит store_reload.go 965 → ~650/~316, выход из >900), `package cfgstore`; load/ (cfgload); facade.go (группы §2.5); удаление записи корня из allowlist. Приёмка: build ./... + vet ./internal/...; сборка sdk, cmd, server; grep «вне internal/config нет импортов internal/config/<подпакет>, кроме catalog»; серийные тесты cfgstore+cfgload+model+io+компонентов.

### D2-6 «Тесты и помощники»
Раскладка 183 тестов по §2.4; создание store/cfgtest (incl. isolateAllGlobalConfigPaths); #95; обновление dir_entries_allowlist (удаление корня), ci-policy.toml/shards.toml (скрипт B), docs/package-moves.md; слияние двух task1292-тестов oauthcred (24→23 файла, цель ≤21 к Z). Приёмка: серийный прогон всех новых пакетов; сверка суммы Test-функций.

### 5.4. Храповик-записи (временные, не постоянные исключения)

1. `dir_entries_allowlist.txt`: запись `internal/config <N>` поднимается в 0б-3.2 (создание 6 IO-файлов в корне) и X1 (создание store/): N = фактическое число элементов каталога на момент коммита (старт после C: 123; после X1: 124). Комментарий: «швы 0б/X и переупаковка config; только вниз; удалить при ≤7». График падения: D2-1 −14 (модель), D2-2 −46 (IO), D2-3 −22 (providers), D2-4 −12 (domains), D2-5 → 7 — запись удаляется. Точное значение в каждой строке — по факту каталога в коммите.
2. Список файлов >900 (В7, блокирующий к Z): `store_mcp_transaction.go` 927 → выходит в X2 (сплит); `load_providers.go` 917 → выходит в D2-3; `store_reload.go` 965 → выходит в D2-5. После D2-5 в `internal/config/**` нет файлов >900 (максимум store.go 832).
3. Исключения каталогов выше 7 — **нет** (редакция 2, решение Г1): cfgstore 7, mcpstore 7, каждый каталог §2.1 ≤ 7.

## 6. Риски и решения/вопросы оператору

1. **РЕШЕНО (Г1): исключения для ядра хранилища нет.** Извлечение компонентов — полосы X1–X4 волны 4 + D2-5; конечное состояние: store/ = 6 подкаталогов, cfgstore = 7 файлов ≤900 строк, все каталоги ≤7 (§2.1, §3.6). Остатка выше 7 нет — ни постоянного исключения, ни храповик-исключения; допускается только временная запись корня internal/config (§5.4). Цена: 4 последовательные полосы (структура ядра — общий файл), ~47 делегаторов (~280 строк) как постоянный клей.
2. **Фасад на 178 имён** — оценка ~400–600 строк; если >800 — разрезать нельзя (1 файл в internal/config). Fallback: потребителям apitoken/listeners/apiremote (cmd+server) — прямые импорты leaf-пакетов (~20 файлов).
3. **IO-внутренности экспортируют ~35 имён** между собой — выше ориентира ≤15. Оправдание: одна ОС-абстракция, разрезанная лимитом 7; экспорт однонаправленный; наружу ~12. Альтернатива — исключение на io/ с ~20 файлами в cfgcommit. Решение за оператором.
4. **oauth под domains/** вместо internal/config/oauth (И§9.2) — следствие лимита 7 верхнего уровня. Альтернатива хуже по смыслу (§6 редакции 1).
5. **#95**: task1314_oauth_disk_sync_test.go — после S3/S4 фейк codexprovider или internal/app/itest; эксперимент — прогон после 0б-3.1.
6. **Данные скриптов старше волны 1** — замеры перед D2 повторить; факты дизайна проверены чтением кода (цитаты §1, §3).
7. **apiremote→Load** (api_remotes.go:108) — P3: сузить до конструктора store.
8. **copilot.go** — 1 строка; удалить в D2-4.
9. **Матричный тест** TestOAuthRound7_CartesianMatrix едет в oauthcred без изменений (В2); 160–200 с остаются в матрице oauthcred.
10. **Риск X-полос**: перенос 229 методов с per-component адаптерами — самый большой шов спринта; защита — по-полосные оракулы + серийные тесты компонента и ядра на каждой полосе; прямые `s.publishMu`-секции (oauth 36, mcp 14 вхождений) сворачиваются в PublishMutation с сохранением CAS по generation.

## Поправка 2026-11-15 (оркестратор, по остановке агента D2-1): методы на переезжающем `Config`

Go требует объявлять методы в пакете, где определён тип; алиас `type Config = cfgmodel.Config` не помогает. Полный перечень методов на `Config`, оставшихся в корне после переезда: `setDefaults` (`load_defaults.go`) и `IsDockerMCPEnabled` (`docker_mcp.go`). Решение: (1) `setDefaults` становится свободной функцией `setDefaults(c *Config, workingDir, dataDir string)` в корневом `load_defaults.go` (меняется только строка сигнатуры, тело побайтно прежнее, приёмник переименовывается в параметр `c`; вызовы `cfg.setDefaults(` → `setDefaults(cfg, `, около 25 мест в `internal/config`); в D2-5 файл переезжает в `cfgload` уже как функция; (2) `IsDockerMCPEnabled` и константа `DockerMCPName` переезжают в `model/config_mcp.go`, в `facade.go` добавляется `const DockerMCPName = cfgmodel.DockerMCPName`. Других изменений объявлений в полосе нет. Приоритет «Config переезжает в D2-1» сохранён: откладывание на D2-5 отклонено, потому что та же проблема встала бы там с теми же двумя методами.

