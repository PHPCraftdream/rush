# Спринт 0: разрезка средних Go-каталогов (D2m, #128) и файлы 951–1000 строк вне D1 (E, #94)

- Дизайн-документ, не код.
- Источник истины: `docs/plans/2026-11-15-sprint0-plan.md` (§3, §5 «D2m», §5 «E», §6 В7).
- Данные: `docs/plans/2026-11-15-sprint0-modules-research.md` (И§9.7, И§11).
- Объём: 11 каталогов из `.githooks/dir_entries_allowlist.txt` + 4 файла 951–1000 строк вне D1-пакетов.
- Принцип: публичный путь пакета сохраняется, внутренности переезжают (у `shell` и `cliprovider` появляется фасад).
- Оракул чистого переноса: отсортированный список `^(func|type|var|const) ` и число `func Test` не меняются.
- Дата: 2026-11-15.
- Все числа пересчитаны из `git ls-files`/`wc -l` на дату дизайна и сверены с `.githooks/dir_entries_allowlist.txt`.

## 1. Текущее состояние

Числа — из листинга каталога (`git ls-files`) и `wc -l`; элемент = рукописный файл кода или подкаталог с кодом (тесты, сгенерированные по маркерам «Code generated»/«DO NOT EDIT», и `testdata` не считаются) — правила `.githooks/check_dir_entries.sh`.

| Каталог | Элементов | Файлов кода | Строк кода | Тестовых файлов (строк) | `func Test` | Файлов-импортёров | Публичный путь |
|---|---|---|---|---|---|---|---|
| `internal/kit/sys/shell` | 24 | 24 | 4975 | 32 (5994) | — | 39 (10 пакетов) | сохраняется |
| `internal/agent/provider/cliprovider` | 23 | 24 (models_cah_gen.go — сгенерирован) | 4834 | 36 (6499) | — | 54 (13 каталогов) | сохраняется |
| `internal/kit/fs/fsext` | 13 | 13 | 1885 | 8 (1765) | 32 | 30 (5 пакетов) | сохраняется |
| `internal/serve/client/apiclient` | 13 | 13 | 2052 | 10 (2541) | 72 | 13 (2 пакета) | сохраняется |
| `internal/store/message` | 12 | 12 | 2688 | 19 (4281) | 109 | 467 (8 пакетов) | сохраняется |
| `internal/agent/provider/nettransport` | 12 | 12 | 3041 | 15 (4385) | 71 | 14 (2 пакета) | сохраняется |
| `internal/serve/client/proto` | 11 | 11 | 1671 | 3 (359) | 10 | 0 (мёртвый: импортёров вне пакета нет) | сохраняется |
| `internal/kit/tasktree` | 10 | 7 (+3 подкаталога lab, memory, protocol) | 1616 (корень) | 10 (2274, корень) | 42 (корень) | 5 (cmd/tasklab) + 28 файлов подкаталогов | сохраняется |
| `internal/kit/dev/cahgen` | 9 | 9 | 1434 | 12 (1768) | 44 | 2 (корневой build.go, kit/dev/cahsync) | сохраняется |
| `internal/agent/provider/codexprovider` | 9 | 9 | 1989 | 14 (3716) | 63 | 23 (3 пакета) | сохраняется |
| `internal/agent/safety/agentguard` | 8 | 8 | 2209 | 6 (1786) | 59 | 5 (3 пакета) | сохраняется |

Строка «—» для shell/cliprovider: `func Test`: shell 168, cliprovider 158 (пересчитаны grep'ом по пакету); суммы «строки кода + тесты» сходятся с `wc -l` каталога: shell 10969, cliprovider 11333, fsext 3650, apiclient 4593, message 6969, nettransport 7426, proto 2030, tasktree-корень 3890, cahgen 3202, codexprovider 5705, agentguard 3995.

Сцепления (кластеры = префикс файла до `_`, инструмент `entangle.py`):

- shell (порог 60 строк): background(1600) ↔ exec(353) по 4 имени в обе стороны; dispatch(639) → background:3/exec:3; run(262) → shell:4/exec:2/dispatch:2/coreutils:2; command(702) и jq(342) почти листья (command → shell:1, powershell:1, coreutils:1; jq → shell:1, run:1). Точное перечисление имён по каждому ребру — в §3.
- cliprovider: provider(2669) ↔ mcpserver(1257) по 11 имён; mcpserver ↔ tool(312) 11/7; provider → procgroup:4, effort:4, usage:2, models:1, merge:1; procgroup/usage/merge — чистые листья (исходящих рёбер нет). Числа из entangle.py завышены комментариями на 4 рёбрах — руки пересчитали кодовые рёбра (§3).
- остальные девять каталогов: подпакеты не создаются (§2), поэтому циклы между подпакетами невозможны; `prereq.py` средние каталоги не покрывает (он о шести больших пакетах) — факт сцеплений внутри них не нужен, кроме проверки «родственных» слияний (§2, колонка «Основание»).

Факты импортёров (`git grep` по строкам импорта, дата дизайна):

- shell — internal/agent 12, agent/tools 9, agent/tools/hooks 4, app 3, safety/agentguard 3, provider/cliprovider 3, config 2, kit/sys/platform 1 (тест-страж spawn_guard_test.go с путями-литералами!), safety/permission 1, agent/prompt 1.
- cliprovider — agent 29 (+2 строковых упоминания), serve/cmd 12 (+1), codexprovider 9, serve/server 3 (+r15bfixture 1), sdk 1, app 1, app/itest 1, cahgen 1 (строковый путь).
- fsext — agent/tools 15 (18 совпадений), config 9, serve/cmd 4, app/projects 1, cliprovider 1.
- apiclient — serve/cmd 12, serve/server 1 (тест).
- message — agent 196, app 102, serve/server 56, serve/cmd 53, session 48, sdk 9, agent/tools 2, serve/client/proto 1.
- nettransport — agent 10, codexprovider 4.
- proto — 0.
- tasktree — cmd/tasklab 5.
- cahgen — build.go (RefreshBuild), cahsync (Options/Run/Dependencies).
- codexprovider — agent 21, serve/cmd 1, config 1 (тест, задача #95).
- agentguard — agent/tools 3, cliprovider 1, agent 1 (тест).

init()/встраивания/ОС-пары (детали в §4):

- в одиннадцати каталогах `go:embed` нет; init() — три: shell/coreutils.go:11 (RUSH_CORE_UTILS), nettransport/provider_client.go:12 (регистрация фабрики в config), agentguard/command_policy.go:191 (shell.RegisterCommandPolicy; сам вызов остаётся); у cliprovider init() нет.
- ОС-пары: shell exec_unix/exec_windows (!windows/windows), coreutils_exec/coreutils_exec_stub (!solaris&&!illumos / solaris||illumos), cliprovider procgroup_unix/procgroup_windows, fsext drive_windows/drive_other и owner_windows/owner_others, cahgen runner_windows/runner_other.

## 2. Целевое дерево

Решения двух типов: (а) слияния внутри пакета — девять каталогов, публичный путь и пакет не меняются, правка импортёров не нужна; (б) подпакеты по швам — `shell` и `cliprovider`.

Проверка имён: `git grep -h '^package ' -- '*.go' | sort -u` и деревья восьми принятых дизайнов (`docs/plans/2026-11-15-sprint0-split-*.md`) — все новые имена свободны: `shellbg`, `shexec`, `shpolicy`, `clipmcp`, `cliptool`, `clipstream`, `clipspec`, `clipproc` (совпадений нет; near-miss `clipproc`/`cliproc` из дизайна cmd — разные имена, отмечено).

### 2.1 internal/kit/sys/shell → фасад + 3 подпакета (публичный пакет = фасад `package shell`, путь прежний)

Вердикт: 24 → 6 элементов (3 файла + 3 подкаталога). Единственная неизбежная правка импортёров: символы фоновой подсистемы (NewBackgroundShellManager, BackgroundShellManager, BackgroundShell, BackgroundExecution, ExecutionOutput, ErrExecutionNotDone) переезжают в `shellbg` — цикл facade→shellbg→facade невозможен (shellbg строит Shell через NewShell, background.go:290,468), поэтому ~30 мест в 12–15 файлах (agent, agent/tools, app) меняют импорт; всё остальное наружу торчит фасадом (алиасы типов и функции-делегаты, §3).

Граф зависимостей:

```
shell (фасад: shell.go, run.go, dispatch.go) → shpolicy {BlockFunc (алиас), CheckRegisteredCommandPolicy, PanicGuardHandler, CanonicalBlockFuncs, BannedCommands, CommandBlocked, CommandsBlocker, CommandCandidatesForPlatform, SkipLauncherPrefixes, DecodePowerShellEncodedCommand, RegisterCommandPolicy, IsNoOpCommand}
shell → shexec {ProcessGroupExecHandler, DefaultKillTimeout, IsolateProcess, KillRetainedProcess, RetainedCommandResult, ExecEnvList, UseGoCoreUtils, CoreUtilsExecHandler, RunRetainedProcess, RetainedStops, RetainedStopKey}
shellbg → shell {NewShell, Options, Shell, BlockFunc}
shellbg → shexec {RetainedStops, RetainedStopKey, API вывода (2-3 имени, §3-S1)}
shexec, shpolicy → (вне семейства) ничего
```

Файлы (24 строки):

| Файл (строк) | Цель |
|---|---|
| background.go (839) | `shellbg/background.go` |
| background_completion.go (94) | `shellbg/` |
| background_execution.go (166) | `shellbg/` |
| background_execution_output.go (94) | `shexec/exec_retained_output.go` (переименование) |
| background_execution_stop.go (97) | `shexec/exec_retained.go` (переименование) |
| background_shell.go (310) | `shellbg/` |
| command_policy.go (702) | `shpolicy/command_policy.go` |
| compound.go (87) | `shell/shell.go` (слияние) |
| coreutils.go (19) | `shexec/` |
| coreutils_exec.go (12) | `shexec/` |
| coreutils_exec_stub.go (8) | `shexec/` |
| deny_exec.go (27) | `shpolicy/` |
| dispatch.go (639) | `shell/dispatch.go` (без изменений) |
| doc.go (25) | `shell/shell.go` (слияние) |
| exec_unix.go (205) | `shexec/` |
| exec_windows.go (148) | `shexec/` |
| expand.go (294) | `shell/shell.go` (слияние) |
| handler_guard.go (39) | `shpolicy/` |
| jq.go (342) | `shell/run.go` (слияние) |
| noop_command.go (104) | `shpolicy/` |
| powershell.go (51) | `shpolicy/` |
| run.go (262) | `shell/run.go` (слияние) |
| shell.go (275) | `shell/shell.go` (слияние) |
| xargs_args.go (136) | `shell/run.go` (слияние) |

Итог по строкам: фасад 681 (275+25+87+294) + 740 (262+136+342) + 639 = 2060; shellbg 1409 (839+310+166+94); shpolicy 923 (702+27+39+104+51); shexec 583 (205+148+19+12+8+97+94); всего 4975 — сходится.

Элементы: `shell/` (фасад) — 3 файла + 3 подкаталога = 6 (было 24); `shellbg` — 4; `shpolicy` — 5; `shexec` — 7 (на пределе, ОС-пары вместе).

Примечание: background.go 839 > целевых 800 — файл не слиялся (чистый перенос), жёсткий предел 1000 соблюдён; отмечено в §6.

Тесты по кластерам (11+6+4+11 = 32 файла, сумма 5994):

| Кластер | Тестовые файлы (строк) | Файлов | Сумма |
|---|---|---|---|
| `shellbg` | background_buffer_test.go (183), background_cap_test.go (43), background_completion_test.go (259), background_execution_behavior_test.go (170), background_execution_test.go (76), background_lastwrite_test.go (126), background_limits_test.go (169), background_ownership_test.go (103), background_release_test.go (475), background_test.go (359), background_wait_test.go (261) | 11 | 2224 |
| `shexec` | background_execution_output_test.go (101), background_execution_stop_test.go (99), background_execution_shebang_test.go (143), exec_windows_test.go (162), isolation_unix_test.go (205), comparison_test.go (41) | 6 | 751 |
| `shpolicy` | command_block_test.go (557), deny_exec_test.go (67), handler_guard_test.go (124), noop_command_test.go (57) | 4 | 805 |
| фасад | compound_test.go (65), dispatch_test.go (594), dispatch_windows_test.go (236), expand_test.go (427), jq_test.go (227), ordinary_lookup_windows_test.go (51), run_test.go (321), shell_git_optional_locks_test.go (38), shell_test.go (120), xargs_args_test.go (54), xargs_integration_test.go (81) | 11 | 2214 |

Все тесты белые (`package shell`) — при переезде меняют предложение пакета на пакет цели; comparison_test.go и background_execution_shebang_test.go едут по неэкспортированным идентификаторам («проверка полосой»). Сумма: 2224+751+805+2214 = 5994.

Публичная поверхность (короткий список групп):

- фон (6 имён → `shellbg`);
- ядро (Run, RunOptions, ExitCode, IsInterrupt, NewShell, Options, Shell, RushEnvMarkers — фасад);
- политика (BlockFunc, CanonicalBlockFuncs, CommandBlocked, BannedCommands, CommandsBlocker, CommandCandidatesForPlatform, SkipLauncherPrefixes, DecodePowerShellEncodedCommand, RegisterCommandPolicy, IsNoOpCommand — алиасы фасада);
- разбор (ParseSimpleCommand, IsCompoundCommand — фасад);
- раскрытие (ExpandValue, NoUnset, HasCommandSubstitution — фасад);
- WSL (LookPathSkippingWSL, ErrWSLLauncherOnly — фасад, dispatch.go).

### 2.2 internal/agent/provider/cliprovider → фасад + 5 подпакетов (публичный пакет = фасад `package cliprovider`)

Вердикт: 23 → 6 элементов (1 файл + 5 подкаталогов). Импортёров НЕ правим: вся внешняя поверхность (22 идентификатора, включая переменную-шов AvailableFunc, которую пишут 8 внешних тестов) остаётся в фасаде — алиасы типов/значений и функции-делегаты (§3); var-алиас допустим только для никогда не присваиваемых имён (4 контекстных ключа, All) — проверено grep'ом присваиваний.

Граф зависимостей:

```
cliprovider (фасад: provider.go) → clipspec {CLISpec, All, + алиасы ключей контекста}
clipstream → clipspec {CLISpec, ResolveLaunchBinary, EffectiveMaxPromptArgLen, ключи контекста}
clipstream → cliptool {ToolPolicy, WithToolPolicy, ToolPolicyFrom, ToolPolicyAllows, PolicyBridgeTools, ApplyRestrictedToolArgs, InvocationToolBridge, ToolBridge, BridgeTool, BuiltinBridgeTools, WithToolBridge, BridgeRequestContext}
clipstream → clipmcp {NewRushMCPServer, RushMCPServer(+методы), MCPToolEvent, QwenMCPID, GeminiMCPID, Register/DeregisterQwenMCP, Register/DeregisterGeminiMCP, SessionMCPName, RushMCPToolArgs, RegisterMCPTools} — на пределе 15, §3-S3
clipstream → clipproc {ConfigureChildProcessGroup, KillChildProcess, TrackChildTree}
clipmcp → cliptool {ToolPolicyFrom, ToolPolicyAllows, RegisterBridgeTools, BridgeMCPResult, FilterBridgeRead, InvocationToolBridge} — 6 имён, полоса сверяет (предел 15)
cliptool → clipspec {CLISpec}
clipspec, clipproc → ничего из семейства; циклов нет
```

Файлы (24 строки):

| Файл (строк) | Цель |
|---|---|
| command_policy_spawn.go (10) | `clipmcp/` |
| effort.go (121) | `clipspec/` |
| mcpserver.go (293) | `clipmcp/` |
| mcpserver_settings.go (344) | `clipmcp/` |
| mcpserver_tools.go (620) | `clipmcp/` |
| merge_lines.go (62) | `clipstream/` |
| models_cah_gen.go (41, сгенерирован) | `clipspec/` (не считается в элементах) |
| procgroup_unix.go (150) | `clipproc/` |
| procgroup_windows.go (52) | `clipproc/` |
| provider.go (268) | `provider.go` (фасад: cliProvider, New, LanguageModel, ProviderType/ProviderID, блок алиасов; отдают: 4 контекстных ключа и ExternalMCPTool/ExternalMCPProxy в подпакеты, см. §3) |
| provider_detection.go (164) | `provider.go` (слияние в фасад: AvailableFunc и кэш детекции остаются в фасаде) |
| provider_failure.go (96) | `clipstream/` |
| provider_logging.go (87) | `clipstream/` |
| provider_parsing.go (294) | `clipspec/` |
| provider_prompt.go (206) | `clipstream/` |
| provider_quota.go (107) | `clipstream/` |
| provider_specs.go (357) | `clipspec/` |
| provider_stream.go (894) | `clipstream/` |
| provider_stream_session.go (196) | `clipstream/` |
| tool_bridge.go (137) | `cliptool/` |
| tool_bridge_schema.go (43) | `cliptool/` |
| tool_policy.go (41) | `cliptool/` |
| tool_policy_args.go (91) | `cliptool/` |
| usage.go (160) | `clipspec/` |

Элементы: `cliprovider/` (фасад) — 1 файл + 5 подкаталогов = 6 (было 23); `clipstream` — 7 (на пределе); `clipmcp` — 4; `cliptool` — 4; `clipspec` — 4 рукописных (+1 сгенерированный, не считается); `clipproc` — 2 (ОС-пара).

Примечание: provider_stream.go 894 — не слиялся (чистый перенос), ≤1000.

Тесты по кластерам (36 файлов, сумма 6499):

| Кластер | Тестовые файлы (строк) | Файлов | Сумма |
|---|---|---|---|
| `clipmcp` | mcp_dereg_race_test.go (214), mcp_startup_failure_test.go (109), mcp_tool_args_test.go (52), mcpbash_allowlist_test.go (22), mcpserver_claudemd_test.go (108), mcpserver_session_test.go (370), mcpserver_tools_argv_test.go (18), mcpserver_tools_git_writes_test.go (96), mcpserver_tools_slice_test.go (23), deny_exec_policy_test.go (125) | 10 | 1137 |
| `cliptool` | tool_bridge_test.go (177), tool_bridge_transport_test.go (66), tool_policy_args_test.go (90) | 3 | 333 |
| `clipproc` | procgroup_spawnedgroup_test.go (140), procgroup_track_test.go (143) | 2 | 283 |
| `clipspec` | p1273_cah_catalog_test.go (55), p469_usage_convention_test.go (162), p470_claude5_specs_test.go (248), p471_effort_dispatch_test.go (159), provider_parsing_test.go (446), provider_usage_test.go (151) | 6 | 1221 |
| `clipstream` | kill_grandchild_test.go (324), provider_failure_test.go (237), provider_kill_test.go (444), provider_prompt_test.go (308), provider_quota_test.go (202), provider_security_test.go (156), provider_stream_logging_test.go (78), provider_stream_mcp_failclosed_test.go (87), provider_stream_mcp_registration_test.go (136), provider_stream_session_test.go (247), provider_stream_test.go (614), provider_test.go (247), provider_windows_test.go (296), merge_lines_test.go (127) | 14 | 3503 |
| фасад | provider_cli_models_test.go (22) | 1 | 22 |

Пометка: три файла едут по неэкспортированным идентификаторам («проверка полосой») — provider_stream_mcp_failclosed_test.go, provider_stream_mcp_registration_test.go и provider_cli_models_test.go (тест белого шва детекции, оставлен в фасаде — уточнение полосой). Числа файлов: 10+3+2+6+14 = 35, плюс фасадный provider_cli_models_test.go — 36; сумма строк тестов 6499 неизменна (проверить арифметику полосе): 1137+333+283+1221+3503+22 = 6499.

### 2.3 internal/kit/fs/fsext — слияния (13 → 7)
Вердикт: 13 → 7 элементов; пакет и публичный путь сохраняются, один коммит.

| Файл (строк) | Цель |
|---|---|
| atomic.go (56) | `fileutil.go` |
| drive_other.go (16) | `os_other.go` (!windows) |
| drive_windows.go (26) | `os_windows.go` (windows) |
| expand.go (29) | `fileutil.go` |
| fileutil.go (335) | `fileutil.go` |
| glob_bounded.go (131) | `walk_bounded.go` |
| list_bounded.go (66) | `walk_bounded.go` |
| lookup.go (281) | `lookup.go` |
| ls.go (403) | `ls.go` |
| owner_others.go (24) | `os_other.go` (!windows) |
| owner_windows.go (15) | `os_windows.go` (windows) |
| paste.go (129) | `paste.go` |
| walk_bounded.go (374) | `walk_bounded.go` |

Итог: fileutil 420 (335+56+29), os_other 40 (16+24), os_windows 41 (26+15), walk_bounded 571 (374+131+66), lookup 281, ls 403, paste 129 = 1885 ✓; элементы: 7 (было 13).

Основание: glob_bounded/list_bounded оба зовут WalkDirBounded (walk_bounded.go:29,68); atomic/expand — утилиты рядом с fileutil; слияние ОС-пар в один файл с тем же тегом — чистая конкатенация, теги `//go:build windows` / `//go:build !windows` сохраняются, оракул добавляет сборку под GOOS=linux. Импортёров правка не нужна (tools использует ограниченный обход из того же пакета).

### 2.4 internal/serve/client/apiclient — слияния (13 → 6)
Вердикт: 13 → 6 элементов; пакет и публичный путь сохраняются, один коммит.

| Файл (строк) | Цель |
|---|---|
| api_batches.go (202) | `api.go` |
| api_misc.go (166) | `api.go` |
| api_runs.go (116) | `api.go` |
| api_sessions.go (78) | `api.go` |
| errors.go (140) | `errors.go` |
| events.go (384) | `events.go` |
| exitcode.go (63) | `errors.go` |
| options.go (160) | `options.go` |
| redact.go (25) | `errors.go` |
| request.go (293) | `request.go` |
| sse.go (126) | `events.go` |
| transport.go (142) | `transport.go` |
| types.go (157) | `options.go` |

Итог: api 562 (202+166+116+78), errors 228 (140+63+25), events 510 (384+126), options 317 (160+157), request 293, transport 142 = 2052 ✓; элементы: 6 (было 13).

Основание: sse.readSSEFrame зовётся только из events.go:257; scrubSecrets/cleanText зовутся из errors.go:33-80 и request.go:35; api_* — методы Client с общими getJSON/mutateJSON/collectPages. Импортёров правка не нужна (serve/cmd 12 файлов + 1 тест server).

### 2.5 internal/store/message — слияния + разрез message.go (12 → 7, один коммит)
Вердикт: 12 → 7 элементов, включая обязательный разрез message.go 976; слияния и разрез — ОДИН коммит.

| Файл (строк) | Цель |
|---|---|
| attachment.go (24) | `parts.go` |
| content.go (748) | `content.go` |
| finalization.go (126) | `finalization.go` |
| finalization_tx.go (70) | `finalization.go` |
| message.go (976) | `message.go` (строки 1–276: CreateMessageParams, интерфейс Service, service, NewService, NewServiceWithReader) + `message_crud.go` (строки 277–976, 700) |
| message_delete_tx.go (51) | `tx.go` |
| message_recovery.go (84) | `tx.go` |
| message_tx_querier.go (38) | `tx.go` |
| parts_codec.go (140) | `parts.go` |
| responses_metadata_decode.go (31) | `parts.go` |
| usage.go (152) | `usage.go` |
| usage_service.go (248) | `usage.go` |

Итог: content 748, parts 195 (24+140+31), finalization 196 (126+70), message 276, message_crud 700, tx 173 (51+84+38), usage 400 (152+248) = 2688 ✓; элементы: 7 (было 12).

Основание: тройка parts делит marshalParts/decodeResponsesReasoningMetadata; tx-тройка — методы *service с *sql.Tx/db.Querier; CreateTxQ (message_tx_querier.go) — импортерская поверхность session, остаётся экспортированной. Разрез message.go по границе «конструкторы → ошибка ErrMessageStillStreaming» (строка 276/277).
ВАЖНО: слияния и разрез — ОДИН коммит (12→7; промежуточное состояние ниже 7 с живой строкой храповика запрещено, §3 п.13 плана). Импортёров правка не нужна; от дизайна db зависит только набор символов db (Querier/Queries/New/Message/*Params) — фасад db их сохраняет (дизайн db), message не правится.

### 2.6 internal/agent/provider/nettransport — слияния (12 → 7)
Вердикт: 12 → 7 элементов; пакет и публичный путь сохраняются, один коммит.

| Файл (строк) | Цель |
|---|---|
| client.go (232) | `client.go` |
| config.go (164) | `config.go` |
| handshake.go (108) | `config.go` |
| provider_client.go (16) | `client.go` (init() внутри — множество init не меняется) |
| proxy.go (235) | `proxy.go` |
| resolver.go (385) | `resolver.go` |
| responses_ws.go (641) | `responses_ws.go` |
| responses_ws_delta.go (672) | `responses_ws_delta.go` |
| responses_ws_diag.go (123) | `responses_ws.go` (методы *ResponsesWSConn) |
| responses_ws_frame.go (51) | `responses_ws_manager.go` |
| responses_ws_manager.go (299) | `responses_ws_manager.go` |
| transport_cache.go (115) | `client.go` (BuildTransport передаёт sharedTransports, client.go:75) |

Итог: client 363 (232+16+115), config 272 (164+108), proxy 235, resolver 385, responses_ws 764 (641+123), responses_ws_delta 672, responses_ws_manager 350 (299+51) = 3041 ✓; элементы: 7 (было 12).

Основание: диагностические члены — методы conn'а (responses_ws.go:163, delta.go:59); кэш транспитов — единственный потребитель BuildTransport; handshake guard делится с proxy.go:145/resolver.go:345, любое размещение — компромисс, выбран «план дозвона». Импортёров правка не нужна.

### 2.7 internal/serve/client/proto — слияния (11 → 6)
Вердикт: 11 → 6 элементов; пакет и публичный путь сохраняются, один коммит.

| Файл (строк) | Цель |
|---|---|
| agent.go (75) | `proto.go` |
| history.go (12) | `proto.go` |
| mcp.go (172) | `mcp.go` |
| message.go (735) | `message.go` |
| permission.go (147) | `permission.go` |
| proto.go (120) | `proto.go` |
| requests.go (137) | `requests.go` |
| server.go (6) | `proto.go` |
| session.go (23) | `proto.go` |
| tools.go (234) | `tools.go` |
| version.go (10) | `proto.go` |

Итог: proto 246 (120+75+12+6+23+10), mcp 172, message 735, permission 147, requests 137, tools 234 = 1671 ✓; элементы: 6 (было 11).

Основание: конверт и мелкие типы протокола вместе. Импортёров нет вообще (пакет мёртв: единственные ссылки — его собственные тесты; проверено grep'ом по всему дереву, включая sdk/, cmd/, tools/, deploy.go) — удаление вне спринта (§6, вопрос оператору), слияние выполняется как есть.

### 2.8 internal/kit/tasktree — слияния корня (10 → 7)
Вердикт: 10 → 7 элементов; корень: 7 файлов → 4; подкаталоги lab, memory, protocol не трогаются; один коммит.

| Файл (строк) | Цель |
|---|---|
| contract.go (60) | `types.go` |
| service.go (274) | `service.go` |
| snapshot.go (233) | `snapshot.go` |
| transition.go (368) | `tree.go` |
| tree.go (346) | `tree.go` |
| types.go (224) | `types.go` |
| view.go (111) | `snapshot.go` |

Итог: types 284 (224+60), service 274, snapshot 344 (233+111), tree 714 (368+346) = 1616 ✓; элементы: 4 файла + 3 подкаталога = 7 (было 10).

Основание: contract-типы нужны только service.go; view-проекции читают обход дерева рядом со snapshot; plan*-планировщики вызываются только из kernelPlan/applyPlan — один движок исполнения команд. Тесты корня (10 файлов, 2274 строк, 42 func Test) остаются в пакете; во всём дереве tasktree 105 func Test (lab 36, memory 8, protocol 19, корень 42); помощники kernelTest* (kernel_transition_test.go:14-99) и serviceTest*/фейковые store не переезжают. Импортёров правка не нужна (cmd/tasklab 5 файлов; 28 файлов подкаталогов ссылаются на корневой пакет — слияние для них невидимо).

### 2.9 internal/kit/dev/cahgen — слияния (9 → 6)
Вердикт: 9 → 6 элементов; пакет и публичный путь сохраняются, один коммит.

| Файл (строк) | Цель |
|---|---|
| build.go (34) | `build.go` |
| downgrade.go (86) | `invariants.go` |
| efforts.go (39) | `build.go` |
| invariants.go (158) | `invariants.go` |
| io.go (596) | `io.go` |
| runner_other.go (7) | `runner_other.go` (!windows, пара) |
| runner_windows.go (25) | `runner_windows.go` (windows, пара) |
| snapshot.go (57) | `build.go` |
| transform.go (432) | `transform.go` |

Итог: build 130 (34+39+57), invariants 244 (158+86), io 596, transform 432, runner-пара 32 (25+7) = 1434 ✓; элементы: 6 (было 9).

Основание: downgrade+invariants — правила версий (io.go:472 зовёт committedVersion/semverLess); build+efforts+snapshot — публикация/снимок (transform.go:174,217 зовёт validateAcceptedEfforts; io.go:490,494 — RenderSnapshot/SnapshotPath). Импортёров правка не нужна (build.go: RefreshBuild; cahsync: Options/Run/Dependencies — все в io.go/build.go). Константы путей вывода: io.go:382-386 (CmdPath, SpecsPath) — их правит полоса CL-A (§5), не эта.

### 2.10 internal/agent/provider/codexprovider — слияния + разрез provider.go (9 → 7, один коммит)
Вердикт: 9 → 7 элементов; слияния + разрез provider.go 979; ОДИН коммит.

| Файл (строк) | Цель |
|---|---|
| cache_affinity.go (20) | `acquire.go` |
| provider.go (979) | `provider.go` (~430–450) + `provider_stream.go` (~560–580) (разрез, группы ниже) |
| provider_acquire.go (127) | `acquire.go` |
| provider_failure.go (233) | `provider_failure.go` |
| provider_websocket.go (363) | `provider_websocket.go` |
| provider_websocket_diag.go (130) | `provider_websocket.go` |
| provider_websocket_failure.go (62) | `provider_failure.go` |
| token_source.go (42) | `token_source.go` |
| usage_metadata.go (33) | `usage_metadata.go` |

Разрез provider.go:

- остаются в `provider.go`: const-блок (строка 25), provider (34), New (41), Name/LanguageModel (48-55), languageModel (57-64), GenerateObject/StreamObject (65-100), Generate (101-144), Stream (145-158), makeRequestBody (159-322), normalizeEffort (323), encodeToolOutput (336), sanitizeCallID (360), providerError (486-514), errStreamStopped (773), String (966), блок interface-asserts (976);
- переезжают в `provider_stream.go`: stream (393), streamTransportError (446), readSSE (450), toolCall (515), streamState (524), newStreamState (539), consume (543-760), streamFailureStatus (761), getTool (775), setFinalArguments (792), finishTool (812), finish (842), responseOutputContent/Item/decodedResponse/decodeResponse/rawString/finishReasonHasTools/parseOutputIndex (854-975).

Итог: acquire 147 (20+127), provider_failure 295 (233+62), provider_websocket 493 (363+130), token_source 42, usage_metadata 33 + 2 части разреза = 1989 + заголовок нового файла (≈20 строк); оракул — список объявлений, не сумма строк; оба файла ≤800; элементы: 7 (было 9).

Основание: transportObservation — параметр streamWebSocket/streamWebSocketOnce (provider_websocket.go:123,133); webSocket*Failure — слой над fantasy.ProviderError рядом с codexFailure; promptCacheKey зовётся из makeRequestBody (provider.go:274), но живёт с acquire-путём (помечено как компромисс); mutex ровно один: websocketObjectAcceptance.mu (:74) — не переезжает между файлами. Импортёров правка не нужна (Name/New/NewWithSource/TokenSource/UsageMetadata/WebSocket* — всё остаётся).

### 2.11 internal/agent/safety/agentguard — слияния + разрез agentguard.go (8 → 7, один коммит)
Вердикт: 8 → 7 элементов; два слияния + один разрез; ОДИН коммит.

| Файл (строк) | Цель |
|---|---|
| agentguard.go (898) | `agentguard.go` (~500) + `agentguard_tables.go` (~410) (разрез, состав ниже) |
| command_policy.go (191) | `command_policy.go` |
| command_policy_candidates.go (201) | `command_policy_candidates.go` |
| command_policy_inline.go (140) | `command_policy_tools.go` |
| command_policy_opaque.go (261) | `command_policy_wrappers.go` |
| command_policy_tools.go (86) | `command_policy_tools.go` |
| command_policy_wrappers.go (229) | `command_policy_wrappers.go` |
| git_writes.go (203) | `git_writes.go` |

Разрез agentguard.go: таблицы запрещённого — deniedAgents/deniedNpmPackages/deniedPypiPackages/packageRunners/shellRunners/commandWrappers/windowOpenerVerbs и константы → `agentguard_tables.go`; Check/CheckArgs/CheckAll/checkAll, разбор строк, извлечение shell-inner, CheckWindowSafety, токенизатор → основной файл; точные границы по списку объявлений, полоса фиксирует в коммите.

Итог: command_policy_tools 226 (86+140), command_policy_wrappers 490 (229+261), остальные файлы без изменений; разрез agentguard.go: 898 + заголовок нового файла; элементы: 7 (было 8).

Основание: inline (parseInlineOption/literalInlineArgvs) и tools (scanToolArgs/таблицы операндов) имеют одинаковый набор потребителей (command_policy.go:112-181, command_policy_opaque.go:156, command_policy_wrappers.go:84-98) — тема «знание опций инструментов»; opaque — разбор непрозрачных обёртывок рядом с walker'ом обёртывок; agentguard.go 898 → оба файла ≤800, иначе оставался бы выше целевых 800. Импортёров правка не нужна (CheckAll/CheckArgs/CheckCommandPolicy(Args)/CheckGitWrites(Args)/CheckWindowSafety/CommandBlockFunc/DeniedError/IsLinkedWorktree — все остаются; init() в command_policy.go:191 не меняется).

### 2.12 Сводка расписания храповика (элементы, подкаталоги из `.githooks/dir_entries_allowlist.txt`)

| Каталог | Было | Промежуточные состояния по коммитам | Итог | Когда удаляется строка |
|---|---|---|---|---|
| kit/sys/shell | 24 | 18 → 14 → 11 | 6 | SH4 |
| agent/provider/cliprovider | 23 | 22 → 19 → 16 → 13 | 6 | CL-C (тем же коммитом) |
| kit/fs/fsext | 13 | — | 7 | одним коммитом |
| serve/client/apiclient | 13 | — | 6 | одним коммитом |
| store/message | 12 | — | 7 | одним коммитом (слияния+разрез вместе) |
| agent/provider/nettransport | 12 | — | 7 | одним коммитом |
| serve/client/proto | 11 | — | 6 | одним коммитом |
| kit/tasktree | 10 | — | 7 | одним коммитом |
| kit/dev/cahgen | 9 | — | 6 | одним коммитом |
| agent/provider/codexprovider | 9 | — | 7 | одним коммитом (слияния+разрез вместе) |
| agent/safety/agentguard | 8 | — | 7 | одним коммитом |

Расписание нигде не опускается до ≤7 при живой строке храповика и не поднимается выше записанного числа (правило §3 п.13 плана); у `shell` и `cliprovider` подкаталоги создаются только вместе с переносом файлов в тот же коммит.

## 3. Швы

Подпакеты создаются только в `shell` и `cliprovider`; девять пакетов из §2.3–2.11 подпакетов не создают — там швов нет, все слияния являются конкатенациями одного пакета (аргумент чистоты — §3.6). Для каждого шва ниже: имена через границу (ориентир ≤15), места вызовов, которые меняются, и аргумент сохранения поведения (что вычисляется когда, порядок блокировок).

### 3.1 S1 — удержанные остановки и вывод: shellbg → shexec

- Проблема: `exec_unix.go`/`exec_windows.go` (будущий `shexec`) вызывают `runRetainedProcess`, `retainedStops`, `retainedStopKey` из `background_execution_stop.go`, и то же трио нужно `dispatch.go` (фасад, строки 203, 210, 217–218). Если остановки оставить в `shellbg`, возникает цикл shexec → shellbg → shell(фасад) → shexec. Поэтому `background_execution_stop.go` → `shexec/exec_retained.go`, `background_execution_output.go` → `shexec/exec_retained_output.go` (переименования).
- Экспортируемая поверхность shexec (ожидается 11 имён):
  - ProcessGroupExecHandler;
  - DefaultKillTimeout;
  - IsolateProcess;
  - KillRetainedProcess;
  - RetainedCommandResult;
  - ExecEnvList;
  - UseGoCoreUtils;
  - CoreUtilsExecHandler;
  - RunRetainedProcess;
  - RetainedStops;
  - RetainedStopKey.
- Плюс строго 2–3 имени API вывода для shellbg (конструктор/подключение записывающего тройника и чтение/сброс вывода — по образцу NewRetainedWriter/SnapshotOutput/DiscardOutput). Полоса SH1 фиксирует финальный список в коммите; если он превысит 15 — останов и эскалация оркестратору, не самовольное расширение.
- Переезжает дословно: runRetainedProcess (ограждение cmd.Start регистрацией под s.mu и соединение с ctx-callback до возврата, background_execution_stop.go:60–97), retainedWriter (тройник), retainedOutput (mu, data, next, changed). Потребители Dispose (background_execution.go:146–150) и ReadOutput (background_execution_output.go:48–81) трогают внутренние поля retainedOutput, поэтому логика этих двух операций переезжает в методы shexec, а методы BackgroundExecution в shellbg становятся однострочными делегатами.
- Сохранение поведения: меняется только пакет типов, ни одна горутина/канал/таймер/блокировка не исчезает. Блокировки, порядок которых запрещено менять (все остаются в shellbg и не пересекают границу S1):
  - startMu охватывает проверку лимита → shells.Set → activeJobs.Add(1) (background.go:434–547, комментарий :446–447 фиксирует защищаемую гонку);
  - порядок retentionMu → holdMu в OnDone (background_shell.go:189–198 → background_completion.go:26–37) против PendingCompletionsOwned (только holdMu, :70–71);
  - идемпотентность releaseBuffers = retentionMu + releaseOnce + bufReleased, проверка retained до releaseOnce (background_shell.go:74–77);
  - Kill/KillOwned = shells.Take → cancel() → detachFromManager → ограниченное ожидание done (background.go:610–644);
  - KillAll/Close — снимок под startMu, затем конкурентные cancel с ограничителем (background.go:753–791);
  - хвост горутины задания exitErr → completedAt.Store → activeJobs.Add(-1) → armBufferReleaseTimer → close(done) (background.go:523–541);
  - порядок Dispose: retentionMu → retained=false → остановка обоих таймеров → releaseBuffers (background_execution.go:151–164).
- Меняющиеся места вызовов: dispatch.go (3 имени), run.go:124 (ProcessGroupExecHandler/DefaultKillTimeout), background.go:517–518 (API вывода), exec-файлы (имена становятся локальными).

### 3.2 S2 — BlockFunc живёт в shpolicy

- BlockFunc (shell.go:72) объявляется в shpolicy; в фасаде остаётся `type BlockFunc = shpolicy.BlockFunc` — структурный алиас, идентичность типа сохраняется, поле Options.BlockFunc (shell.go:84) и четыре внешних потребителя (cliprovider/mcpserver_tools.go, agentguard/agentguard.go, agent/tools/bash.go, agent/tools/job_test.go) не меняются.
- powershell.go переезжает в shpolicy (единственный внутрипакетный потребитель — command_policy.go:528; внешний потребитель agentguard ходит через фасадный делегат DecodePowerShellEncodedCommand).
- handler_guard.go переобъявляет структурный алиас execMiddleware локально (импорт не нужен).
- Итог: shpolicy не импортирует ничего из семейства shell (0 имён) — цикл невозможен.
- Граница фасад → shpolicy — всего ≤16 имён, из них функциональных переходов 3 (остальное — однострочные алиасы/делегаты без логики). Если при реализации алиасов окажется больше — лишние алиасы не создаются: внешние потребители перечислены в §2.1, список закрыт:
  - BlockFunc (алиас типа);
  - CheckRegisteredCommandPolicy;
  - PanicGuardHandler;
  - плюс алиасы внешней поверхности: CanonicalBlockFuncs, BannedCommands, CommandBlocked, CommandsBlocker, CommandsBlockerForPlatform, ArgumentsBlocker, ArgumentsBlockerForPlatform, CommandCandidates, CommandCandidatesForPlatform, ParseCommandCandidates, SkipLauncherPrefixes, DecodePowerShellEncodedCommand, RegisterCommandPolicy, IsNoOpCommand.
- Регистрация agentguard не меняется: `agentguard/command_policy.go:191` вызывает `shell.RegisterCommandPolicy(...)` — фасад оставляет функцию-делегат; семантика first-write-wins и мьютекс denyExecMu (deny_exec.go:9–24) переезжают дословно вместе с deny_exec.go в shpolicy.

### 3.3 S3 — cliModel переезжает в clipstream

- Stream/StartStream объявлены методами *cliModel в provider_stream.go, остальные методы cliModel — в provider.go; Go запрещает методы одного типа в двух пакетах, поэтому cliModel, cliSessionEntry, поле cliSessions (*csync.Map) и тестовые швы streamProcess/streamVisibleCleanup переезжают в clipstream целиком.
- Фасад сохраняет cliProvider, New, LanguageModel (делегирует в clipstream.NewModel, возвращает fantasy.LanguageModel), ProviderType/ProviderID.
- Граница фасад → clipstream: NewModel, LogRawPromptEnabled (функция-делегат; потребители agent_ownership.go:581,655 не меняются), StreamResult (алиас типа) — ≤4 имён.
- Сохранение поведения: порядок конструкции модели (spec → ExternalMCPProxy → карта cliSessions) не меняется; Generate сворачивает stream как прежде; fail-closed pre-flight обязательных MCP до первого вызова streamProcess (mcp_startup_failure_test.go) остаётся в StartStream дословно; полный порядок выхода из потока не меняется:
  - классификация scanErr (io.EOF и os.ErrClosed benign; в PTY-режиме любая ошибка глотается);
  - legacyWait (once);
  - лог выхода;
  - cliSessions.Del при неудачном resume;
  - сборка ошибки (waitErr+stderr / merged output / failure text);
  - mapCLIExitError (обёртка 429 при жёстком лимите);
  - TextEnd + Finish.

### 3.4 S4 — границы clipstream: clipmcp / cliptool / clipproc / clipspec

- clipstream → clipmcp, ожидаемый список ровно 15:
  - NewRushMCPServer;
  - RushMCPServer (тип и его методы stop/mcpConfigJSON/mcpURL);
  - MCPToolEvent;
  - RegisterMCPTools;
  - QwenMCPID;
  - GeminiMCPID;
  - RegisterQwenMCP;
  - DeregisterQwenMCP;
  - RegisterGeminiMCP;
  - DeregisterGeminiMCP;
  - SessionMCPName;
  - RushMCPToolArgs;
  - RushMCPToolArgsWithPolicy;
  - EmitToolStart/EmitToolEnd (пара считается за одно имя-семейство);
  - MCPToolsCanBeScoped.
- Полоса CL-B сверяет фактический список; при >15 предписанный откат (отдельный подшаг того же lane'а, не чистый перенос, тела сравниваются побайтово): блок MCP-оркестрации StartStream — четыре ветки интеграции claude/qwen/gemini/codex, provider_stream.go:262–320 и 560–620 — сворачивается в один вход clipmcp, граница падает до ~4 имён.
- Сохранение поведения S4: общего замка между потоком CLI и HTTP-обработчиками MCP нет; синхронизация — только буферизованный канал toolCh (ёмкость 32, при переполнении события теряются) и once-замыкания очистки. Порядок visibleCleanup не меняется:
  - session.UntrackProcessTree;
  - session.UnregisterChildGroup;
  - deregisterGeminiMCP;
  - deregisterQwenMCP (dereg только если адрес в settings.json всё ещё наш);
  - удаление attachTmpDir/mcpTmpCfg;
  - mcpSrv.stop().
- Токен-аутентификация (заголовок или ?token=) и stateless-режим обработчика сохраняются; MCP-setup выполняется даже в yolo при perms!=nil || bridge!=nil || policy.Constrained.
- clipstream → cliptool, список 12:
  - ToolPolicy;
  - WithToolPolicy;
  - ToolPolicyFrom;
  - ToolPolicyAllows;
  - PolicyBridgeTools;
  - ApplyRestrictedToolArgs;
  - InvocationToolBridge;
  - ToolBridge;
  - BridgeTool;
  - BuiltinBridgeTools;
  - WithToolBridge;
  - BridgeRequestContext.
- Переменная bridgeFallback (atomic.Pointer, tool_bridge.go:47) живёт в cliptool; единственный внешний писатель — internal/agent/batch_install_ingress.go:204 — ходит через фасадную функцию-делегат SetBridgeFallback; атомарность и семантика не меняются.
- clipstream → clipproc, 3 имени: ConfigureChildProcessGroup, KillChildProcess, TrackChildTree. Доказательство порядка:
  - Unix — Setpgid + cmd.Cancel → killChildProcessPid (kill(-pid, SIGKILL), ESRCH → os.ErrProcessDone);
  - TrackChildTree = session.RegisterSpawnedGroup + durable session.RegisterChildGroup, запись под processGroupExists (kill(-pgid, 0); EPERM считается «существует») и session.ReadLockGeneration(session.SessionLockPath(...));
  - PTY-ветка: go-pty форсирует Setsid, ptyProc (atomic.Pointer[os.Process]) публикуется после Start, ptycmd.Cancel его читает, устанавливается только на не-Windows;
  - Windows — configureChildProcessGroup no-op, TrackProcessTree (Job Object с KILL_ON_JOB_CLOSE, при неудаче лог + taskkill-fallback), killChildProcess → procinfo.KillProcess.
- Обе обёртки спавна — session.StartProcessUnlessStopping; каждый путь выхода проходит visibleCleanup.
- Функции переезжают дословно, ОС-пара неразлучна, killChildProcessPid остаётся неэкспортированным внутри clipproc.
- clipstream → clipspec: CLISpec, ResolveLaunchBinary, EffectiveMaxPromptArgLen плюс четыре значения контекстных ключей (см. §4.1).
- cliptool → clipspec: CLISpec (applyRestrictedToolArgs переписывает argv по флагам спеки).
- clipmcp → cliptool, точный список шести (полоса сверяет; предел 15):
  - ToolPolicyFrom;
  - ToolPolicyAllows;
  - RegisterBridgeTools;
  - BridgeMCPResult;
  - FilterBridgeRead;
  - InvocationToolBridge.

### 3.5 S5 — каталог моделей и cahgen-контракт

- models_cah_gen.go (маркер «Code generated by cahsync … DO NOT EDIT», строка 1) переезжает в clipspec. В том же lane'е CL-A правятся:
  - `internal/kit/dev/cahgen/io.go:384` (SpecsPath → `internal/agent/provider/cliprovider/clipspec/models_cah_gen.go`);
  - `internal/kit/dev/cahgen/transform.go:417` (заголовок `package clipspec`);
  - строковые пути в тестах cahgen (refresh_test.go и другие, где путь литералом).
- Без этого падает CI-ворота `cahsync -check -strict` (`.github/workflows/ci.yml:240`).
- cahsync (`internal/kit/dev/cahsync`) импортирует cahgen (Options/Run/Dependencies) — не меняется.
- All []CLISpec (provider_specs.go:260) живёт в clipspec; фасад — `var All = clipspec.All` (делит срез).
- Безопасно, потому что в рантайме срез никто не мутирует (проверено grep'ом присваиваний); если появится рантайм-регистрация, алиас заменяется функцией доступа (§6).
- ExternalMCPTool/ExternalMCPProxy (provider.go) переезжают в clipmcp (потребитель — registerExternalMCPTools); фасадные type-алиасы сохраняют координатору (internal/agent/coordinator_providers.go:902,920–929) прежнюю запись cliprovider.ExternalMCPTool.

### 3.6 Аргумент чистоты слияний

Все слияния §2.3–2.11 — конкатенация файлов одного пакета: множество пакетов и множество объявлений не меняются; единственный init среди девяти пакетов (nettransport/provider_client.go:12) сохраняется внутри client.go; порядок init между пакетами определяется графом импортов и не меняется, порядок внутри пакета при одном init не наблюдаем. Оракул каждой полосы слияния: `grep -hE '^(func|type|var|const) '` по исходным файлам и по целевым файлам, отсортированные списки идентичны (разбивка по целевым файлам задана таблицей §2); число `func Test` по пакету: fsext 32, apiclient 72, message 109, nettransport 71, proto 10, tasktree (корень) 42, cahgen 44, codexprovider 63, agentguard 59. Дополнительно для fsext — сборка под GOOS=linux (пары тегов), для codexprovider/agentguard — побайтовое сравнение переносимых тел при разрезе.

## 4. Глобальные переменные, init(), ОС-пары, белые тесты, помощники

### 4.1 Глобальные переменные (каждая — ровно один пакет)

Таблица shell (Переменная | Где сейчас | Целевой пакет | Обоснование):

| Переменная | Где сейчас | Целевой пакет | Обоснование |
|---|---|---|---|
| backgroundManager + backgroundManagerOnce | background.go:384–387 | shellbg | legacy-синглтон sync.Once; внешний GetBackgroundShellManager не вызывается (deprecated), уезжает целиком |
| denyExecMu, denyExecPolicy | deny_exec.go:9–11 | shpolicy | пишется один раз из init() agentguard; deny_exec_test.go едет вместе |
| useGoCoreUtils, coreUtilsExecHandler | coreutils.go:8, coreutils_exec.go:11/_stub:7 | shexec | init() едет с файлом; чтение в момент вызова обработчиков |
| testInnermostHandler | run.go:180 | фасад | тест-шов run_test.go |
| NoUnset (atomic.Bool) | expand.go:59 | фасад | потребитель config/resolve.go |
| bannedCommands, posixLongValueOption, powerShellValueOption | command_policy.go:45 и рядом | shpolicy | неизменяемые таблицы |
| waitCommands | noop_command.go:11 | shpolicy | неизменяемое множество |
| nonInteractiveEnvVars | run.go:138 | фасад | фактически неизменяемый список |

Таблица cliprovider:

| Переменная | Где сейчас | Целевой пакет | Обоснование |
|---|---|---|---|
| 4 контекстных ключа (SessionIDContextKey, TitleGenerationContextKey, ReasoningEffortContextKey, NonInteractiveContextKey) | provider.go:29–58 | clipspec | значения-синглтоны, повторных присваиваний нет (проверено); фасадные var-алиасы для agent, codexprovider, app |
| All, cahSpecs, cahVersion | provider_specs.go:260, models_cah_gen.go | clipspec | алиас All в фасаде |
| AvailableFunc, detectAvailableCache, testDisablePTY, detectAvailable, Available, AvailableCLIModels | provider_detection.go:23,28,121 | фасад | AvailableFunc пишут 8 внешних тестов (internal/agent/coordinator_folder_scope_cli_test.go, internal/app/itest/main_test.go:28, internal/serve/cmd ×4, internal/serve/server ×2) — детекция не уезжает |
| bridgeFallback | tool_bridge.go:47 | cliptool | писатель один, через делегата SetBridgeFallback |
| testMCPBashSpawn, testRunShellSpawn | command_policy_spawn.go:7–9 | clipmcp | пишутся deny_exec_policy_test.go (едет в clipmcp), читаются mcpserver_tools.go — один пакет |
| ansiEscape | provider.go:144 | clipstream | потребители — цикл потока и quota |
| rushClaudeInitBlockPattern | mcpserver_tools.go:264 | clipmcp | локальная таблица |
| claudeEffortLevels, codexEffortLevels* | effort.go:41–60 | clipspec | методы CLISpec |

Правило: сплит не создаёт ни одной новой экспортируемой изменяемой переменной; каждая существующая — ровно в одном пакете.

Другие пакеты (короткая таблица):

| Пакет | Переменные | Где сейчас | Целевой файл | Примечание |
|---|---|---|---|---|
| nettransport | transportDialer | client.go:43 | `client.go` | остаются в пакете |
| nettransport | socksHandshakeTimeout | handshake.go:39 | `config.go` | мутируется lifecycle_test.go:370–371 |
| nettransport | sharedTransports | transport_cache.go:41 | `client.go` | остаются в пакете |
| fsext | BoundedWalkMaxEntries, BoundedWalkTimeout | walk_bounded.go:15 | `walk_bounded.go` | мутируют walk_bounded_test.go:64–91 |
| apiclient | BatchPollInterval | api_batches.go:174 | `api.go` | — |
| agentguard | processCommandPolicy | command_policy.go:15 | остаётся | — |
| tasktree, cahgen, proto, message | — | — | — | изменяемых глобалов нет (только init-once таблицы с их файлами) |

### 4.2 init()

Список всех init затронутых семейств:

- shell/coreutils.go:11 (RUSH_CORE_UTILS/GOOS → useGoCoreUtils) — уезжает в shexec вместе с файлом; порядок безопасен: чтение в момент вызова standardHandlers, а импорт графа ставит shexec раньше фасада.
- nettransport/provider_client.go:12 (config.SetProviderHTTPClientFactory) — остаётся в пакете внутри client.go.
- agentguard/command_policy.go:191 (shell.RegisterCommandPolicy) — вне D2m, вызов сохраняется через фасадный делегат.
- Других init в одиннадцати каталогах нет (проверено grep'ом), go:embed нет ни в одном.

### 4.3 ОС-пары (таблица Пара | Цель | Примечание)

| Пара | Цель | Примечание |
|---|---|---|
| shell exec_unix.go (!windows) / exec_windows.go (windows) | `shexec`, вместе | kit/sys/platform/spawn_guard_test.go:24–28 исключает спавн-сайты по литеральным путям internal/kit/sys/shell/exec_unix.go и exec_windows.go — полоса SH1 обновляет два ключа карты (правка теста, поведение стража не меняется) |
| shell coreutils_exec.go (!solaris && !illumos) / coreutils_exec_stub.go (solaris \|\| illumos) | `shexec`, вместе | пара покрывает все GOOS |
| cliprovider procgroup_unix.go (!windows) / procgroup_windows.go (windows) | `clipproc`, вместе | сигнатурная парность (dataDir/sessionID принимаются и игнорируются на Windows) сохраняется |
| fsext drive_windows+owner_windows → os_windows.go (тег windows); drive_other+owner_others → os_other.go (тег !windows) | слияние одно-теговых файлов | теги переносятся |
| cahgen runner_windows.go (windows) / runner_other.go (!windows) | остаются двумя файлами в `cahgen` | — |
| Остальные семь каталогов ОС-файлов не имеют; agentguard различает платформы рантайм-проверками GOOS | — | Windows-тесты сохраняют теги: exec_windows_test.go → shexec, dispatch_windows_test.go и ordinary_lookup_windows_test.go → фасад, provider_windows_test.go → clipstream |

### 4.4 Белые тесты и помощники

- Правило размещения: тест едет в пакет тех неэкспортированных идентификаторов, которых касается (таблицы §2.1/§2.2); тест, трогающий два будущих пакета, делится или едет по основному предмету с пометкой «проверка полосой» (в §2 помечены: comparison_test.go, background_execution_shebang_test.go у shell; provider_stream_mcp_failclosed_test.go, provider_stream_mcp_registration_test.go у cliprovider).
- Помощники не экспортируются — при расщеплении в каждый пакет-потребитель кладётся локальная копия (урок дизайна db: чужое имя закрывается копией, не экспортом).
- shell: перекрёстные помощники heldExecution, writeScript, randSuffix дублируются в shexec и shellbg по потребителям; deny_exec_test.go едет в shpolicy вместе с denyExecMu/denyExecPolicy; TestMain в пакете shell нет.
- cliprovider: помощники fastExitHelperEnv, waitForRemovable, processAlive, isolateHomeDir — с потребителями в clipstream; connectMCPTestClient, callMCPTool, recordingMCPPermissionService — в clipmcp; TestMain, включающий testDisablePTY на Windows, переносится вместе с швом testDisablePTY: если он определён в тесте кластера stream — уезжает в clipstream, если в фасадном — остаётся и дублирует установку в clipstream (полоса CL-C определяет место grep'ом `func TestMain` и фиксирует в коммите).
- Пакеты тестовых помощников (по образцу db/dbtest) D2m не нужны: ни один помощник не нужен двум продовым пакетам одновременно; сценарные тесты internal/app/itest не трогаются (itest пишет cliprovider.AvailableFunc через фасад — путь не меняется).
- Тестовые импорты вверх: если полоса обнаружит у теста своего пакета импорт верхнего слоя (например, cliprovider/agentguard-тесты с session-фейками), тест уезжает в internal/app/itest или переписывается на фейк по правилу §3 п.6 плана; на дату дизайна таких у одиннадцати каталогов не найдено.

## 5. Полосы исполнения

Одна полоса — один каталог; одновременно не более 5 агентов (§3 п.7 плана); агенту разрешены только `gofumpt`, `go build`, `go vet` затронутых пакетов через `.rush/tmp/golock.sh`, тесты пакетов серийно запускает оркестратор (`golock.sh go test -p 1 -parallel 1 -count=1 -timeout 300s`); каждая полоса оставляет дерево собирающимся и заканчивается коммитом с падающим счётом элементов; расписание нигде не опускается до ≤7 при живой строке храповика и не поднимается выше записанного числа (§3 п.13 плана).

### 5.1 Полосы shell — строго последовательно SM1 → SH1 → SH2 → SH3 → SH4

| Полоса | Содержание и файлы | Коммиты (элементы каталога) | Предусловия | Приёмка |
|---|---|---|---|---|
| SM1 | шов S1 внутри пакета: логика Dispose/ReadOutput переезжает в методы retainedStops/retainedOutput (background_execution_output.go, background_execution_stop.go), BackgroundExecution.Dispose/ReadOutput становятся однострочными делегатами (background_execution.go, background.go) | 1 коммит, 24 | нет | оракул объявлений; побайтовое сравнение перенесённых тел; build/vet shell; grep: Dispose/ReadOutput — делегаты |
| SH1 | создание `shexec/`: exec_unix.go, exec_windows.go, coreutils.go, coreutils_exec.go, coreutils_exec_stub.go + переименования exec_retained.go (← background_execution_stop.go), exec_retained_output.go (← background_execution_output.go); правка вызовов фасада (dispatch.go:203,210,217–218; run.go:124); обновление двух литеральных ключей в kit/sys/platform/spawn_guard_test.go:24–28; переезд тестов §2.1 (6 файлов shexec) | 1 коммит: 24 → 18 | SM1 | оракул shexec; build/vet shell; сборка shexec под GOOS=linux; head -1 тегов пар; grep новых ключей spawn_guard |
| SH2 | создание `shpolicy/`: command_policy.go, deny_exec.go, handler_guard.go, noop_command.go, powershell.go; BlockFunc переезжает (алиас в фасаде); делегат RegisterCommandPolicy; переезд тестов (4 файла) | 1 коммит: 18 → 14 | SH1 | оракул; build/vet shell + safety/agentguard; grep `shell.RegisterCommandPolicy` — один вызов из agentguard без правки |
| SH3 | создание `shellbg/`: background.go, background_shell.go, background_execution.go, background_completion.go; переписывание импортёров фоновой подсистемы: internal/agent (coordinator.go, coordinator_tools.go, coordinator_background.go, batch_exec_shell.go + тесты), internal/agent/tools (bash.go, bash_leaf.go + тесты), internal/app (app.go, app_agent_setup.go + тест) — шесть имён (NewBackgroundShellManager, BackgroundShellManager, BackgroundShell, BackgroundExecution, ExecutionOutput, ErrExecutionNotDone) и их методы меняют квалификатор на `shellbg.`; переезд тестов (11 файлов) | 1 коммит: 14 → 11 | SH2; полоса идёт В ОДИНОЧКУ — в момент её работы нет открытых полос, правящих internal/agent, internal/agent/tools, internal/app (A1–A5, SA*, DM* по другим дизайнам) | оракул shellbg; build/vet shell + agent + tools + app; grep: шесть имён с квалификатором `shell.` в трёх пакетах не встречаются; пометить в коммите: массовая правка импортёров (§3 п.15 плана) |
| SH4 | слияния фасада: shell.go + doc.go + compound.go + expand.go → shell.go (681); run.go + xargs_args.go + jq.go → run.go (740); удаление строки `internal/kit/sys/shell 24` из .githooks/dir_entries_allowlist.txt | 1 коммит: 11 → 6, строка удалена | SH3 | оракул двух слияний (до: пять файлов, после: один — списки идентичны); build/vet shell; тестовых файлов: 32 в пакете shell, 21 переезжает в подпакеты, 11 остаётся в фасаде |

ОС-пары едут с SH1; init coreutils едет с SH1; порядок init не меняется (§4.2).

### 5.2 Полосы cliprovider — строго последовательно CL-A → CL-B → CL-C

| Полоса | Содержание и файлы | Коммиты (элементы каталога) | Предусловия | Приёмка |
|---|---|---|---|---|
| CL-A | создание `clipproc/` (procgroup_unix.go, procgroup_windows.go) и `clipspec/` (provider_specs.go, effort.go, provider_parsing.go, usage.go + сгенерированный models_cah_gen.go); правки cahgen: io.go:384 (SpecsPath), transform.go:417 (package clipspec), литеральные пути в тестах cahgen (refresh_test.go и др.); фасадные алиасы: All, четыре контекстных ключа, CLISpec, EffectiveMaxPromptArgLen; переезд тестов (6 clipspec + 2 clipproc) | 2 коммита: 23 → 22 → 19 | MG-cahgen завершён (один писатель на каталог cahgen) | оракул clipspec/clipproc; build/vet cliprovider + kit/dev/cahgen + codexprovider; проверку `cahsync -check -strict` выполняет оркестратор (агентам go run запрещён) — строка в отчёте полосы |
| CL-B | создание `cliptool/` (tool_bridge.go, tool_bridge_schema.go, tool_policy.go, tool_policy_args.go) и `clipmcp/` (mcpserver.go, mcpserver_settings.go, mcpserver_tools.go, command_policy_spawn.go); переезд объявлений ExternalMCPTool/ExternalMCPProxy в clipmcp; фасадные алиасы и делегат SetBridgeFallback; переезд тестов (3 cliptool + 10 clipmcp) | 2 коммита: 19 → 16 → 13 | CL-A | оракул; build/vet cliprovider + agent (cli_tool_bridge.go, batch_install_bridge.go, batch_install_ingress.go); фактическая граница clipstream→clipmcp записана в коммите (предел 15; откат — §3.4) |
| CL-C | создание `clipstream/` (provider_stream.go, provider_stream_session.go, merge_lines.go, provider_failure.go, provider_quota.go, provider_logging.go, provider_prompt.go); переезд cliModel/cliSessionEntry/швов; provider_detection.go сливается в provider.go (AvailableFunc и кэш остаются в фасаде); удаление строки `internal/agent/provider/cliprovider 23` тем же коммитом; переезд тестов (14 clipstream + решение по TestMain §4.4) | 1 коммит: 13 → 6, строка удалена (промежуточного состояния «7 с живой строкой» нет) | CL-B | оракул; build/vet cliprovider + agent + codexprovider + serve/cmd + serve/server + sdk; `grep -rhoP "cliprovider\.\w+" internal/ sdk \| sort -u` до/после — идентичны (внешняя поверхность не изменилась) |

22 внешних идентификатора перечислены в §2.2 и §3; AvailableFunc остаётся ровно в фасаде.

### 5.3 Полосы слияний и E-файлы (взаимно параллельны, по одной на каталог)

| Полоса | Каталог | Содержание | Элементы | Приёмка |
|---|---|---|---|---|
| MG-fsext | kit/fs/fsext | слияния §2.3 (три целевых файла + два os-файла) | 13 → 7, строка удалена, один коммит | оракул; GOOS=linux build; 32 func Test |
| MG-apiclient | serve/client/apiclient | слияния §2.4 | 13 → 6, один коммит | оракул; 72 func Test |
| MG-message | store/message | слияния §2.5 + разрез message.go по строке 276/277 | 12 → 7, ОДИН коммит | оракул всех семи целевых файлов; 109 func Test; build/vet message + session + agent (потребитель CreateTxQ) |
| MG-nettransport | agent/provider/nettransport | слияния §2.6 | 12 → 7, один коммит | оракул; 71 func Test |
| MG-proto | serve/client/proto | слияние §2.7 (шесть файлов → proto.go) | 11 → 6, один коммит | оракул; 10 func Test |
| MG-tasktree | kit/tasktree (только корень) | слияния §2.8 | 10 → 7, один коммит | оракул; 42 func Test корня; build/vet cmd/tasklab |
| MG-cahgen | kit/dev/cahgen | слияния §2.9 | 9 → 6, один коммит | оракул; 44 func Test; build/vet cahgen + cahsync + корневой build.go |
| MG-codexprovider | agent/provider/codexprovider | слияния §2.10 + разрез provider.go (группы объявлений заданы) | 9 → 7, ОДИН коммит | оракул; 63 func Test; побайтовое сравнение переносимых тел разреза |
| MG-agentguard | agent/safety/agentguard | слияния §2.11 + разрез agentguard.go (таблицы → agentguard_tables.go) | 8 → 7, ОДИН коммит | оракул; 59 func Test; init() строки 191 без изменений |
| E-sdk | sdk | разрез sdk.go: строки 321–823 → client.go | 4 → 5 (каталог вне храповика) | оракул; 108 func Test; переносимый блок без var/init |
| E-permission | agent/safety/permission | разрез permission.go: строки 60–282 → contract.go | 6 → 7 (предел) | оракул; 87 func Test; импортёры не меняются (тот же пакет) |

### 5.4 Порядок и параллельность (волны по ≤5 агентов)

Перестановки допустимы при соблюдении трёх правил: последовательность внутри shell и cliprovider; SH3 в одиночку; CL-A после MG-cahgen:

1. SM1 (одна).
2. SH1 + MG-fsext + MG-apiclient + MG-proto + MG-cahgen (пять).
3. SH2 + MG-message + MG-nettransport + MG-tasktree + E-sdk (пять).
4. SH3 (одна, массовая правка импортёров).
5. SH4 + MG-codexprovider + MG-agentguard + E-permission (четыре).
6. CL-A (+ свободный слот любой незапущенной MG-полосы при наличии).
7. CL-B.
8. CL-C.

| Пара полос | Правило |
|---|---|
| SM1–SH4 | общий пул файлов shell, строго подряд |
| CL-A–CL-C | общий пул provider.go/provider_stream.go, строго подряд |
| SH3 против A1–A5/SA*/DM (файлы internal/agent, internal/agent/tools, internal/app) | не совмещать |
| MG-cahgen против CL-A | упорядочены |
| остальные пары | не пересекаются файлами |

## 6. Риски и открытые вопросы

1. Граница clipstream→clipmcp — ровно на пределе 15: откат §3.4 не является чистым переносом, тела сравниваются побайтово, подшаг отдельным коммитом.
2. Состав API вывода S1 (2–3 имени) определяет полоса по факту; при >15 — останов и эскалация, самовольное расширение границы запрещено.
3. cahgen-контракт: агент не может исполнить `cahsync -check -strict` — проверку ворот делает оркестратор после CL-A (иначе ложнозелёная полоса).
4. Правка spawn_guard_test.go — вмешательство в чужой тест-страж (kit/sys/platform): только два литеральных ключа, поведение стража не меняется, отметить в коммите SH1.
5. Место TestMain/testDisablePTY (фасад или кластер stream) не установлено — решает CL-C по grep (§4.4), фиксирует в коммите.
6. Четыре каталога остаются ровно на 7 элементов (shexec, clipstream, agentguard, permission): запаса для будущих разрезов нет — новое дробление требует нового дизайна; нарушения правила нет.
7. background.go 839 и provider_stream.go 894 выше целевых 800, но это чистые переносы в пределах 1000; их дальнейшее дробление — предмет храповика предупреждений >900 (#93), не D2m.
8. `serve/client/proto` — мёртвый пакет (импортёров нет): слияние выполняется, удаление — отдельное слово оператора вне спринта (поведение не меняется).
9. Windows-ветки (PTY, Job Object, ConPTY, hide-console) локально не воспроизводимы: ставка на перенос provider_windows_test.go/exec_windows_test.go без изменения тегов и на fork CI ветки.
10. Вопрос оператору: алиас `var All = clipspec.All` безопасен, пока срез не мутируется в рантайме (проверено); если появится рантайм-регистрация моделей — алиас заменяется функцией (одна строка в фасаде).

## 7. Файлы 951–1000 строк: задача E (#94), В7

Все 26 рукописных файлов 951–1000 строк (скан `git ls-files` + wc, дата дизайна); сгенерированные не считаются (internal/store/db/db.go 2038, db/messages.sql.go 1460, db/batch_nodes.sql.go 965, db/querier.go 951 — по маркеру). 22 из 26 лежат в каталогах восьми D1-дизайнов и режутся их полосами D2; четыре — вне их, режутся этим документом (уже входят в полосы §5.3).

| Файл | Строк | Кто режет |
|---|---|---|
| sdk/sdk.go | 1000 | этот дизайн (E-sdk) |
| internal/agent/coordinator_run.go | 998 | дизайн agent (D2) |
| internal/agent/coordinator_tool_pinning_test.go | 995 | дизайн agent |
| internal/app/app_run_async.go | 993 | дизайн app |
| internal/serve/server/hub.go | 992 | дизайн server |
| internal/config/store_config_disk_sync_test.go | 991 | дизайн config |
| internal/agent/batch_cutover_await_test.go | 989 | дизайн agent |
| internal/session/lock_test.go | 985 | дизайн session |
| internal/serve/cmd/providers_peak_hours.go | 985 | дизайн cmd |
| internal/app/app_run_reviewer.go | 982 | дизайн app |
| internal/agent/safety/permission/permission.go | 981 | этот дизайн (E-permission) |
| internal/agent/provider/codexprovider/provider.go | 979 | этот дизайн (MG-codexprovider) |
| internal/store/message/message.go | 976 | этот дизайн (MG-message) |
| internal/session/lock.go | 976 | дизайн session |
| internal/session/childgroup_registry_unix.go | 976 | дизайн session |
| internal/serve/cmd/providers.go | 973 | дизайн cmd |
| internal/serve/cmd/root.go | 972 | дизайн cmd |
| internal/config/apitoken_store_test.go | 969 | дизайн config |
| internal/agent/tools/mcp/mutation.go | 967 | дизайн mcp |
| internal/agent/agent.go | 966 | дизайн agent |
| internal/config/store_reload.go | 965 | дизайн config |
| internal/agent/coordinator_interrupt.go | 958 | дизайн agent |
| internal/agent/batch_install_adapters.go | 958 | дизайн agent |
| internal/agent/tools/view.go | 957 | дизайн tools |
| internal/agent/coordinator_providers.go | 957 | дизайн agent |
| internal/serve/cmd/migrate_test.go | 954 | дизайн cmd |

### 7.1 Разрез sdk/sdk.go (E-sdk)

- Каталог sdk: 4 файла кода (sdk.go 1000, library_mode.go 578, finalization.go 78, sentinels.go 12), подкаталогов с кодом нет → после разреза 5 ≤ 7, строки храповика нет.
- Перенос: строки 321–823 — Client (321), Open (396), resolveWorkingDir (409), openApplication (433), Run (562), RunWithCredentials (628), SubscribeMessages (671), SubscribeSessions (692), Messages (725), Session (747) → новый sdk/client.go (~528 строк с заголовком); остаток sdk.go ≈ 497 (type-алиасы app/agent/message/session/pubsub, константы, OSDiskProvider, ErrClientClosed и ошибки-алиасы, MCPMode, Options, Close, admit, defaultWriter, synchronizedWriter, beginShutdown, CloseEphemeralConnsForced, closeEphemeralConns). Оба ≤800. Переносимый блок не содержит var/init.
- Импортёры: единственный реальный — testdata/embedder/embed_test.go (sdk.Open, sdk.Options, sdk.RunRequest, sdk.RunModeJSON, sdk.ErrSessionBusy, sdk.ErrDiskProviderNotDurable, sdk.ErrDiskProviderUnavailable); остальные `sdk.` вне каталога — комментарии или чужой пакет openaisdk. Пакет один, никто не меняется.
- Оракул: отсортированные списки объявлений до/после (до: sdk.go; после: sdk.go + client.go — объединённый список идентичен); 108 func Test; build/vet sdk + testdata/embedder.

### 7.2 Разрез permission.go (E-permission)

- Каталог internal/agent/safety/permission: 6 файлов кода (permission.go 981, folderscope.go 337, runallowlist.go 383, session_deny.go 93, session_policy.go 116, permission_rules.go 26) → после разреза 7 = предел; дальнейшее дробление каталога без нового дизайна запрещено (зафиксировано в §6 п.6).
- Перенос: строки 60–282 — CreatePermissionRequest (60), PermissionNotification (70), PermissionRequest (76), Service (87), RestrictedRunAuthorizer (121), RestrictedRunContextAuthorizer (125), RestrictedRunToolAuthorizer (131), RestrictedRunDispatchAuthorizer (138), SessionRunAllowlistManager (161), SessionRunAllowlistBaselineManager (257), sessionRunAllowlistEntry (276) → новый contract.go (~240 строк); остаток permission.go ≈ 758 (hookApprovalKey и хуки утверждения на строках 35–56 остаются; permissionService с методами GrantPersistent…Shutdown и NewPermissionService:950 остаются). Оба ≤800. Переносимый блок без var/init.
- Импортёры: 58 файлов без тестов (internal/agent, agent/tools, provider/cliprovider, app, serve/cmd, serve/server) — пакет и квалификатор прежние, правок нет. Windows-тест r5_6_leading_slash_regression_windows_test.go не трогается.
- Оракул: как в §7.1; 87 func Test; build/vet permission + agent + tools.

### 7.3 Правило и связь с В7

- Разрез файла = чистый перенос объявлений внутри одного пакета, без подпакета и без изменения квалификаторов; приёмка — оракул §3.6; добавление одного элемента каталога проверено в каждой из двух полос (4→5 и 6→7).
- После E-sdk, MG-message и MG-codexprovider вне D1-каталогов не остаётся рукописных файлов >900 строк; блокирующий храповик >900 к приёмке Z получает за пределами D2-каталогов пустое множество.

---
