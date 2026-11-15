# Спринт 0, D3 (#76): дизайн разрезки web-клиента (2026-11-15)

- **Статус:** дизайн, кода нет. Исполнение — этап D3 (волна 5), независимая от Go полоса; вливать после этапа C (план §5 D3).
- **Источники:** план §1–§3, §5 (D1, 0б, D3), §6, §11; исследование И§2–§5, И§9.4, И§11; уроки рецензий sibling-дизайнов (план §11); `CLAUDE.md` (оракул чистого переноса для TS — отсортированный список экспортируемых имён на множестве файлов).
- **Объём:** рукописные `.ts`/`.tsx` под `web/src` (348 файлов) и `web/tests` (17 файлов-помощников и конфигов: `helpers` 9, `e2e` 7, `real-backend` 1), плюс 2 конфига корня `web/` (`playwright.config.ts`, `rsbuild.config.ts`). Сгенерированное (`web/dist`, `pnpm-lock.yaml`) и `web/embed.go` не входят. Всего в таблицах §2 — 368 файлов (348 нетестовых в §2.4 + 20 юнит-тестов в §2.5); помощники `web/tests` (9), `e2e` (7), `real-backend` (1) и 2 конфига корня `web/` описаны в §4.4–§4.5 (их расположение не меняется, кроме перегруппировки helpers).
- **Правила:** план §3 действует целиком; для TS дополнительно: (а) лимит `oxlint max-lines` 1000 строк — жёсткий (`web/.oxlintrc.json`), цель ≤ 800; (б) строки ≤ 120 символов держит линтер; (в) тесты `*.test.ts`/`*.spec.ts` не считаются в лимите 7 элементов, но считаются во втором храповике «все файлы каталога ≤ 21» (В5, A1b).
- **Оракул чистого переноса (для каждой полосы):** отсортированный список экспортируемых имён (`export`/`export type`) на множестве перенесённых файлов до и после совпадает, и число тестовых функций (`test(`/`it(` с начала строки) на множестве тестов совпадает. Проверка: `rg -o 'export (default )?(async )?(type |const |function |class |interface |enum )?[A-Za-z_$]+' --no-filename <файлы> | sort` до и после; для тестов `rg -c '^\s*test\(|^\s*it\('`.
- **Чего полосы не делают:** не запускают `npm run build`, `pnpm test`, Playwright и vitest локально (агенты — только `pnpm typecheck` и `pnpm lint`, это дёшево и входит в pre-push, `.githooks/pre-push:297-303`); сборка и все тесты — CI (`.github/workflows/ci.yml:340-359`: `pnpm typecheck`, `pnpm lint`, `node --test src/*.test.ts`, `pnpm build`). Vitest в репозитории нет: юнит-тесты — `node --test` (в задании D3 упомянут vitest; фактический раннер — `node --test` + Playwright, это и есть доказательная база ниже).

## 1. Текущее состояние

### 1.1. Дерево и счёт файлов (проверено `git ls-files` + прогон `check_dir_entries.sh`)

Скрипты `.rush/tmp/sprint0/` (`prereq.py`, `entangle.py`, `analyze.py`) принимают путь Go-пакета и на `web/` не работают; замеры ниже сделаны самим стражем `check_dir_entries.sh` (read-only) и подсчётом `git ls-files -- 'web/src/*.ts' 'web/src/*.tsx'` + `rg`-цензусом импортов. Числа элементов в строках-храповиках совпали с моим подсчётом у всех 15 каталогов (у `web/src` расхождение 36 против 35 в списке объясняется тем, что `index.css` не `.ts` и стражем не считается).

| Каталог | Элементы (храповик) | Файлов всего | Нетестовых `.ts/.tsx` | Тестов | Состав (кратко) |
|---|---|---|---|---|---|
| `web/src` | 35 | 39 | 19 | 19 тестов + `index.css` | 16 подкаталогов, фасады `store.ts`, `ws.ts`, `types.ts`, `useWS.ts` (783), `App.tsx`, `main.tsx` и др. |
| `web/src/actions` | 52 | 52 | 52 | 0 | 52 файла «один экспорт — один файл», подкаталогов нет |
| `web/src/components/Message` | 48 | 48 | 48 | 0 | 33 `.tsx` (включая `index.tsx`) + 15 `.ts` |
| `web/src/state` | 37 | 37 | 37 | 0 | 17 классов `*Carburetor.ts` + 17 файлов-одиночек + 3 `computed`-производных |
| `web/src/components` | 33 | 23 | 22 | 1 (`batchControls.test.ts`) | 22 файла в корне + 11 подкаталогов |
| `web/src/components/ChatToolbar` | 21 | 21 | 21 | 0 | тулбар чата: 15 `.tsx` (включая `index.tsx`) + 6 `.ts` |
| `web/src/components/ChatInput` | 18 | 18 | 18 | 0 | композер; `ChatInput.tsx` 873 строки — максимум репозитория |
| `web/src/components/Chat` | 14 | 14 | 14 | 0 | оболочка чата + очередь + выделение |
| `web/src/liveWork` | 13 | 13 | 13 | 0 | live work + wake schedules: префиксы, предикаты, отправители |
| `web/src/components/peakHours` | 12 | 12 | 12 | 0 | чистая логика пик-часов (все `.ts`, ни одного компонента) |
| `web/src/components/scopedModels` | 11 | 11 | 11 | 0 | слоты моделей сессии |
| `web/src/components/providers` | 9 | 9 | 9 | 0 | формы провайдеров |
| `web/tests/helpers` | 9 | 9 | 9 | 0 | помощники Playwright |
| `web/src/components/batchControl` | 8 | 8 | 8 | 0 | чистая логика batch-управления |
| `web/src/net` | 8 | 8 | 8 | 0 | WS-транспорт |
| `web/tests` (справочно) | 3 (не в списке) | 67 | 0 | 64 `.spec.ts` | 64 спеки в корне + `e2e` 7, `helpers` 9, `real-backend` 2; в режиме `--count-tests` страж видит 67 — второй храповик A1b возьмёт это значение как сид, число только падает |
| прочие каталоги `web/src` (включая вложенные в components) | ≤ 7 каждый | 56 | 56 | 0 | `effort` 7, `completion` 7, `asyncJob` 7, `modelList` 7, `mcp` 6, `attachmentLimit` 4, `customRole` 3, `queueOnIdle` 3, `batch` 2, `keepAlive` 2, `sitter` 2, `telemetry` 2, `wakeSnapshot` 2, `sidebar` 2 |

Сумма нетестовых рукописных файлов по каталогам §1.1: корень `src` 19 + `actions` 52 + `Message` 48 + `state` 37 + корень `components` 22 + `ChatToolbar` 21 + `ChatInput` 18 + `Chat` 14 + `liveWork` 13 + `peakHours` 12 + `scopedModels` 11 + `providers` 9 + `batchControl` 8 + `net` 8 + прочие 56 = 19+52+48+37+22+21+18+14+13+12+11+9+8+8+56 = **348**. Слагаемое «прочие 56» раскрыто в последней строке таблицы: `effort` 7 + `completion` 7 + `asyncJob` 7 + `modelList` 7 + `mcp` 6 + `attachmentLimit` 4 + `customRole` 3 + `queueOnIdle` 3 + `batch` 2 + `keepAlive` 2 + `sitter` 2 + `telemetry` 2 + `wakeSnapshot` 2 + `sidebar` 2 = 56. В «нетестовые» включены `index.tsx`-фасады (`Message/index.tsx`, `Chat/index.tsx`, `ChatInput/index.tsx`, `ChatToolbar/index.tsx`) — они не тесты. Проверка против стража: `check_dir_entries.sh` (обычный режим) — зелёный, все 15 записей в пределах записанных чисел; в режиме `--count-tests` два сообщения: `web/src/components grew to 34` (тест `batchControls.test.ts`) и `web/tests 67` — оба объяснимы и на целевое дерево не влияют.

### 1.2. Размеры файлов

Файлов > 1000 строк нет (`oxlint max-lines: 1000` — error, зелёный в CI). Выше цели 800 — один файл: `ChatInput/ChatInput.tsx` 873. Далее: `useWS.ts` 783, `CustomRolesEditor.tsx` 673, `ModelSelector.tsx` 598, `TodoList.tsx` 545, `PeakHoursEditor.tsx` 519, `SettingsModal.tsx` 506, `types.ts` 490, `MessagesCarburetor.ts` 436, `BatchTree.tsx` 421, `ProviderForm.tsx` 414. Полоса дробления внутри каталога — §5, L6 (только `ChatInput.tsx`; остальным дробление не требуется и не планируется).

### 1.3. Слои и направления импортов (цензус `rg 'from "..."'` по 453 файлам)

Фактические слои снизу вверх — уже слоистые, циклов на уровне каталогов нет (проверено полным цензусом; каждое ребро ниже — одностороннее):

```
types.ts (490) — импортируют все: 56 прямых импортов + 20 из Message + ...
domain: asyncJob ← completion ← state; customRole, attachmentLimit,
        effort, mergeMessage (← state/messages), toolFormat, ...  — pure, без state/net
state (37): ← actions (34 ребра), ← net (1: WSClient→connection), ← ui (40 файлов), ← useWS (17)
net (8): ← actions (21), ← liveWork (3), ← sitter (1), ← telemetry (2), ← ui через ws.ts (9)
службы: queueOnIdle (0 импортов), keepAlive (1 внутренняя), sitter (→net 1 + state 5),
        batch (→state 1 type), telemetry (→net 2, →state 1), liveWork (→net 3, →state 4),
        wakeSnapshot (→types)                                    — между net и actions
actions (52): →state 34, →net 21, →queueOnIdle 3, →telemetry 2; ← только store.ts и useWS
store.ts (фасад, 73 строки, ~48 реэкспортов) ← ui (36 файлов), ← useWS
ws.ts (фасад, 8 строк) ← ui (~9 импортов); actions-файлы импортируют net/ напрямую, мимо бареля
useWS.ts (783) ← components/WsConnector.tsx — ЕДИНСТВЕННЫЙ импортёр (rg: один файл)
components ← App.tsx ← main.tsx
```

Единственное «нетипичное» ребро: `net/WSClient.ts:2,148,158,162` пишет в `state/connection.ts` (`connectionCarburetor.setOutboxCount`) — net над state, цикла нет; шов для него не нужен (§3.4).

## 2. Целевое дерево

### 2.1. Дерево с числом элементов

Элементы = подкаталоги с кодом + нетестовые `.ts/.tsx` в каталоге (правила стража `check_dir_entries.sh`). Все каталоги ≤ 7, все файлы ≤ 800 строк после полосы L6 (`ChatInput.tsx` 873 → ≤ 800 дроблением без смены экспортов, §5).

```
web/src/                          6 элементов — 6 подкаталогов, файлов в корне нет
  app/                            5: main.tsx, App.tsx, WsConnector.tsx, useWS.ts, env.d.ts (+ index.css, не элемент)
  state/                          7 подкаталогов (+1 тест в корне, тест — не элемент)
    link/         4   AuthCarburetor.ts, auth.ts, ConnectionCarburetor.ts, connection.ts
    config/       6   ConfigCarburetor.ts, config.ts, SkillsCarburetor.ts, skills.ts, McpCarburetor.ts, mcp.ts
    transcript/   6   MessagesCarburetor.ts, messages.ts, QueueCarburetor.ts, queue.ts, SelectionCarburetor.ts, selection.ts
    status/       6   NoticesCarburetor.ts, notices.ts, BusyCarburetor.ts, busy.ts, SignalsCarburetor.ts, signals.ts
    sessions/     2   SessionsCarburetor.ts, sessions.ts
    live/         6   LiveWorkCarburetor.ts, liveWork.ts, WakeSchedulesCarburetor.ts, wakeSchedules.ts, WakeCancelCarburetor.ts, wakeCancel.ts
    runtime/      7   SitterCarburetor.ts, sitter.ts, BatchWorkCarburetor.ts, batchWork.ts, myPrompts.ts, asyncJobCompletions.ts, asyncJobStatuses.ts
  backend/                        7: index.ts (бывш. ws.ts) + 6 подкаталогов
    ws/           6   WSClient.ts, client.ts, requestTracker.ts, SessionRequestTracker.ts, reconnectTranscript.ts, requests/ (4: wsRequest.ts, WSRequestError.ts, sendLoadMessages.ts, sendListSessions.ts)
    keepAlive/    3   KeepAlive.ts, keepAliveRuntime.ts, setKeepAliveEnabled.ts (последний — из actions/)
    queue/        3   drainSendableQueue.ts, flushQueueOnIdle.ts, sendQueuedItem.ts
    sitter/       2   SitterRunner.ts, sitterLoop.ts
    telemetry/    3   index.ts (бывш. telemetry.ts), logClientEvent.ts, setupTelemetry.ts
    batch/        3   index.ts (бывш. batchWork.ts), pickBatchWork.ts, buildBatchTree.ts
  actions/                        7: index.ts (фасад, бывш. store.ts) + 6 подкаталогов
    theme/        4   THEME_STORAGE_KEY.ts, applyTheme.ts, restoreCachedTheme.ts, setTheme.ts
    config/       3 подкаталога: providers/ (6: ConfigScope.ts, addCustomProvider.ts, removeCustomProvider.ts, updateCustomProvider.ts, setProviderKey.ts, removeProviderKey.ts), peakHours/ (4: setProviderPeakHours.ts, pauseProviderPeakHours.ts, resumeProviderPeakHours.ts, fetchPeakHoursStatus.ts), contexts/ (4: addContextPath.ts, removeContextPath.ts, addSkillsPath.ts, removeSkillsPath.ts)
    sessions/     3 подкаталога: lifecycle/ (7: removeSession.ts, setActiveSession.ts, summarizeSession.ts, cancelQueuedSummarize.ts, removeSessionQueueAndBusy.ts, removeSubAgentState.ts, updateTodos.ts), models/ (4: getDefaultModelKey.ts, removeRecentModel.ts, trackModelUsage.ts, sendWithFastModel.ts), slots/ (4: setSessionModel.ts, clearSessionModelSlot.ts, setSessionRoleEffort.ts, roleFieldPrefix.ts)
    messages/     2 подкаталога: edit/ (6: deleteMessage.ts, deleteMessagePart.ts, deleteMessages.ts, updateMessageContent.ts, updateMessagePart.ts, updateMessageThinking.ts), nav/ (5: togglePinMessage.ts, tombstoneMessage.ts, jumpToMessage.ts, collectTurnContent.ts, rerunFromMessage.ts)
    queue/        7   queuedMessagePayload.ts, sendFailureState.ts, describeSendFailure.ts, sendQueuedMessageDirect.ts, sendQueuedMessageNow.ts, interruptAndSendQueuedMessage.ts, flushQueuedMessagesOnIdle.ts
    liveWork/     3 подкаталога: protocol/ (7: LIVE_WORK_REQUEST_PREFIX.ts, WAKE_SCHEDULES_CANCEL_PREFIX.ts, WAKE_SCHEDULES_GET_PREFIX.ts, isLiveWorkRequestID.ts, isWakeSchedulesCancelRequestID.ts, isWakeSchedulesGetRequestID.ts, isStaleWakeSchedulesSnapshot.ts), transfer/ (6: pickLiveWork.ts, pickWakeSchedules.ts, sendGetSessionLiveWork.ts, sendGetSessionWakeSchedules.ts, sendCancelWakeSchedule.ts, applyWakeSchedulesSnapshot.ts), wakeSnapshot/ (2: pickPendingWake.ts, isStaleWakeSnapshot.ts)
  domain/                         7: types.ts + 6 подкаталогов
    text/         4   mergeMessage.ts, toolFormat.ts, asyncJobMetadata.ts, liveWorkAwaiting.ts
    asyncJob/     7   ASYNC_NOTICE_PATTERNS.ts, asyncJobTypes.ts, indexAsyncJobCompletions.ts, isAsyncCompletionNotice.ts, messageText.ts, resultMetadata.ts, stoppedStatus.ts
    completion/   7   buildMyPrompts.ts, completionStatuses.ts, completionTypes.ts, isComposerTypedMessage.ts, mergeCompletionIndex.ts, partsToText.ts, structuredStatus.ts
    customRole/   4   index.ts (бывш. customRoleTypes.ts), customRoleAt.ts, roleToolsMode.ts, roleToolsPayload.ts
    attachmentLimit/ 5  index.ts (бывш. attachmentLimit.ts), ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES.ts, MAX_ATTACHMENT_PAYLOAD_BYTES.ts, estimateAttachmentPayloadBytes.ts, isAttachmentPayloadWithinLimit.ts
    effort/       7   EFFORT_LEVELS.ts, clampEffort.ts, defaultEffortFor.ts, effortLabel.ts, effortLevelsFor.ts, isCLIClaudeModel.ts, supportsEffort.ts
  ui/                             7: Login.tsx + 6 подкаталогов
    chat/         4 подкаталога: Chat/ (5), ChatInput/ (5), ChatToolbar/ (6), Message/ (7)
    shared/       7   ConfirmDialog.tsx, findSession.ts, watchCollapseAllSignal.ts, watchExpandToolCallSignal.ts, markdown/ (4: TextBlock.tsx, DiffView.tsx, mdRemarkPlugins.ts, mdRehypePlugins.ts), notices/ (3: BackgroundJobNotice.tsx, FinishErrorBlock.tsx, StreamPausedBlock.tsx), edit/ (2: EditForm.tsx, EditPreview.tsx)
    sidebar/      3   Sidebar.tsx, SessionRow.tsx, formatTokens.ts
    livework/     7   LiveWorkPanel.tsx, LiveWorkTabBar.tsx, TodoList.tsx, WakeScheduleList.tsx, StatusBar.tsx, BatchTree.tsx, batchControl/ (batchControls.ts + payload/ 4 + status/ 4)
    models/       5   ModelSelector.tsx, CustomRolesEditor.tsx, ScopedModelsModal.tsx, modelList/ (7 файлов, без внутренних переносов), scopedModels/ (3 подкаталога: pickers/ 4, rows/ 5, types/ 2)
    providers/    7   ProvidersModal.tsx, PeakHoursEditor.tsx, PeakHoursStatus.tsx, forms/ (6), draft/ (3), peakHours/ (rules/ 6 + policy/ 6), mcp/ (7)
web/tests/                        3 элемента (как сегодня) — спеки не двигаются
  helpers/        3   index.ts (новый), factories/ (5), ws/ (5)
  e2e/            7   без изменений
  real-backend/   1   без изменений
```

Развернуто внутренности `ui/chat`, `ui/models`, `ui/providers`, `ui/livework` (каждая строка — каталог → элементы):

```
ui/chat/Chat/        5: index.tsx, Chat.tsx + transcript/ (7: ChatScroller, TranscriptList, buildRenderItems, EmptyState, BusyIndicator, AgentErrorBanner, ToolRun), queue/ (2: QueueSection, QueuedMessageItem), selection/ (2: SelectionToolbar, clearSelection)
ui/chat/ChatInput/   5: index.tsx + composer/ (5: ChatInput.tsx, ComposerActions.tsx, fitTextarea.ts, foldQueuedDraft.ts, buildSendPayload.ts), attachments/ (5: AttachmentBadges.tsx, PendingAttachment.ts, readFileAsBase64.ts, toWireAttachments.ts, formatBytes.ts), menus/ (5: HistoryMenu.tsx, SlashMenu.tsx, slashQueryOf.ts, filterSkills.ts, sourceBadgeClass.ts), banners/ (2: ForeignOwnedBanner.tsx, OfflineBanner.tsx)
ui/chat/ChatToolbar/ 6: index.tsx, ChatToolbar.tsx + pills/ (6: BusyDots, TokenPill, UpdateBadge, SitterPill, ThemeToggle, KeepAliveToggle), controls/ (3: CompactButton, CollapseAllButton, MoreMenu), modals/ (7: ToolbarModal.ts, ToolbarModals.tsx, SystemPromptModal.tsx, SystemPromptFetch.ts, ShutdownConfirm.tsx, LogsModal.tsx, SettingsModal.tsx), model/ (5: ModelCluster.tsx, activeSmartModel.ts, lookupModel.ts, formatTokens.ts, pctColor.ts)
ui/chat/Message/     7 подкаталогов, index.tsx удаляется (§3.2): message/ (5: Message.tsx, Part.tsx, IntermediateAssistantMessage.tsx, agentPrompt.ts, ForkSessionModal.tsx), content/ (7: AssistantBlock, AssistantContent, UserContent, ThinkingPart, StandaloneThinking, blocks.ts, extractText.ts), tools/ (5: ToolCallBlock, ToolResultBlock, ToolActivityGroup, groupToolActions, fileWriteTools), controls/ (6: ActionRow, AssistantHoverActions, UserHoverActions, CopyButton, CopyTurnButton, tallyActions), badges/ (4: DurationBadge, TimeBadge, UsageBadge, EffortBadge), states/ (4: isTerminallyFinished, AskQuestionBlock, parseAwaitingAnswer, CheckboxContextMenu), subagent/ (7: SubAgentBlock, SubAgentFrame, SubAgentMessage, SummaryMessage, summarizeSubTranscript, selectSubAgentView, findErrorFinish)
ui/models/modelList/      7 без изменений (файлы не двигаются)
ui/models/scopedModels/   3: pickers/ (4: EffortPicker.tsx, ModelPicker.tsx, SLOTS.ts, pickedEffort.ts), rows/ (5: ScopedSlotRow.tsx, ScopedSlotsBlock.tsx, SessionSlotRow.tsx, SessionSlotsBlock.tsx, setSessionModelSlot.ts), types/ (2: scopedModelsTypes.ts, inheritedText.ts)
ui/providers/forms/       6: ProviderForm.tsx, ProviderRow.tsx, BuiltinProviderEditor.tsx, ModelEditor.tsx, ModelDraft.ts, ScopeToggle.tsx
ui/providers/draft/       3: emptyModel.ts, providerFormTypes.ts, wireModels.ts
ui/providers/peakHours/   2: rules/ (6: DAYS.ts, END_RE.ts, HH_MM_RE.ts, MAX_WINDOWS.ts, canonicalDays.ts, toggleDay.ts), policy/ (6: defaultPolicy.ts, legacyToPolicy.ts, peakDaysSet.ts, peakHoursProblems.ts, peakSummary.ts, peakSummaryText.ts)
ui/providers/mcp/         7: MCPSettings.tsx, MCPForm.tsx, PLACEHOLDER_JSON.ts, ServerRow.tsx, StatusBadge.tsx, buildInitialJson.ts, mcpServerPayload.ts
ui/livework/batchControl/ 3: batchControls.ts (+ тест, не элемент) + payload/ (4: batchControlTypes.ts, batchControlPayload.ts, batchControlCommands.ts, requestBatchControl.ts), status/ (4: batchNodeNotice.ts, batchNodeStatus.ts, batchResultMessage.ts, isStopConfirmed.ts)
```

### 2.2. Граф зависимостей новых каталогов (ацикличен)

```
domain  → (ничего из src; только npm-пакеты)
state   → domain
backend → state, domain            (ws/WSClient → state/connection; queue, sitter, telemetry, batch → state)
actions → backend, state, domain   (actions выше backend: → ws/client, ws/requests, queue, telemetry)
app     → actions, backend, state  (useWS → actions/index, backend/ws, state/*; App → state; main → backend/telemetry)
ui      → actions, backend, state, domain (ui НЕ импортирует app: единственный импортёр useWS —
           WsConnector.tsx, он переезжает в app рядом с ним — §3.3)
```

Проверка ацикличности по цензусу §1.3: единственные «встречные» рёбра — `backend/ws → state` (только `WSClient.ts:2,148,158,162`, только `setOutboxCount`) и `actions → backend` (actions выше backend по слоям, направление законно). Обратных рёбер нет: ни один файл `state` не импортирует `backend`/`actions`; ни один файл `backend` не импортирует `actions`; `ui` не импортирует `app`. Внутри `ui` допустимы ровно четыре семейства рёбер, каждое одностороннее (проверено цензусом импортёров): фича → `shared`; `chat/Chat` → `chat/Message` (`ToolRun.tsx:5`, `TranscriptList.tsx:3`); `chat` → `livework` (`ChatToolbar.tsx`→`StatusBar`, `Chat.tsx`→`LiveWorkPanel`); `chat` → `providers`/`models` (`ToolbarModals`→`LogsModal`, `SettingsModal`, `MCPSettings`, `ProvidersModal`, `ScopedModelsModal`; `ModelCluster`→`ModelSelector`). Обратных рёбер из `providers`/`models`/`livework` в `chat` нет: `ProvidersModal`, `ScopedModelsModal`, `LogsModal`, `SettingsModal`, `MCPSettings` импортируются только из `ToolbarModals`; `ModelSelector` — из `CustomRolesEditor` и `ModelCluster`; `StatusBar` — из `LiveWorkTabBar` и `ChatToolbar`.

Внутри `chat/Message` граф подкаталогов: `content`, `badges`, `states` не импортируют соседей по Message; `tools` → `controls` (`tallyActions`), `message` (`agentPrompt`); `message` → `subagent` (`Part`→`SubAgentBlock`, `Message.tsx`→`SummaryMessage`), `shared/edit` (`IntermediateAssistantMessage`→`EditForm`); `controls` → `shared/edit` (`ActionRow`→`EditForm`); `subagent` → `badges` (`EffortBadge`, `TimeBadge`), `content` (`SubAgentMessage`→`extractText`), `shared` (`markdown`, `notices/FinishErrorBlock`, `watchExpandToolCallSignal`) — ацикличен. Ключевое решение против цикла: `SummaryMessage.tsx` живёт в `subagent/` (его импортируют и `message/Message.tsx`, и `subagent/SubAgentFrame`); `EditForm`/`EditPreview` — в `shared/edit/` (их импортируют одновременно `controls`, `content`, `message` и `subagent`).

### 2.3. Имена

Новых имён каталогов верхнего уровня шесть: `app`, `state`, `backend`, `actions`, `domain`, `ui` — все осмысленны без пути; запрещённых (`core`, `util`, `common`, `base`) нет. Сверка с Go-пакетами репозитория (`git grep -h '^package ' -- '*.go' | sort -u`) дала ровно одно совпадение: `app` — в репозитории есть `package app` (`internal/app`). Технического конфликта это не создаёт: `web/src/app/` — каталог TypeScript с относительными импортами, а не Go-пакет, деревья и системы имён не пересекаются; остальные пять имён (`state`, `backend`, `actions`, `domain`, `ui`) среди Go-пакетов не встречаются. Внутренние имена (`link`, `config`, `transcript`, `status`, `sessions`, `live`, `runtime`, `ws`, `requests`, `keepAlive`, `queue`, `sitter`, `telemetry`, `batch`, `theme`, `providers`, `peakHours`, `contexts`, `lifecycle`, `models`, `slots`, `edit`, `nav`, `liveWork`, `protocol`, `transfer`, `wakeSnapshot`, `message`, `content`, `tools`, `controls`, `badges`, `states`, `subagent`, `markdown`, `notices`, `pickers`, `rows`, `types`, `forms`, `draft`, `rules`, `policy`, `payload`) уникальны внутри своего родителя. Четыре межветочных повтора — осознанные, путь даёт контекст, коллизий импортов нет (импорты относительные): `state/sessions` ↔ `actions/sessions`; `actions/config/providers` ↔ `ui/providers`; `actions/config/peakHours` ↔ `ui/providers/peakHours` (wire-действия против UI-логики одного домена); `backend/queue` ↔ `actions/queue`. Пятый потенциальный повтор (`backend/ws/requests` ↔ `actions/liveWork/requests`) устранён переименованием второй группы в `transfer`.

### 2.4. Таблица «файл → цель»: все 348 нетестовых файлов `web/src`

Строки сгруппированы по целевому каталогу; в каждой строке перечислены все файлы, попадающие в цель (пути от `web/src/`, имена без расширения — все `.ts`/`.tsx` как сегодня). Каждое имя встречается ровно один раз; суммы и автоматическая сверка — после таблицы.

| Цель | Файлы (сегодняшние пути от `web/src/`) |
|---|---|
| `app/` | `App.tsx`, `main.tsx`, `env.d.ts`, `useWS.ts`; из `components/`: `components/WsConnector.tsx` |
| `domain/types.ts` | `types.ts` |
| `domain/text/` | `mergeMessage.ts`, `toolFormat.ts`, `asyncJobMetadata.ts`, `liveWorkAwaiting.ts` |
| `domain/asyncJob/` | `asyncJob/ASYNC_NOTICE_PATTERNS.ts`, `asyncJob/asyncJobTypes.ts`, `asyncJob/indexAsyncJobCompletions.ts`, `asyncJob/isAsyncCompletionNotice.ts`, `asyncJob/messageText.ts`, `asyncJob/resultMetadata.ts`, `asyncJob/stoppedStatus.ts` |
| `domain/completion/` | `completion/buildMyPrompts.ts`, `completion/completionStatuses.ts`, `completion/completionTypes.ts`, `completion/isComposerTypedMessage.ts`, `completion/mergeCompletionIndex.ts`, `completion/partsToText.ts`, `completion/structuredStatus.ts` |
| `domain/customRole/` | `customRoleTypes.ts` (→ `index.ts`), `customRole/customRoleAt.ts`, `customRole/roleToolsMode.ts`, `customRole/roleToolsPayload.ts` |
| `domain/attachmentLimit/` | `attachmentLimit.ts` (→ `index.ts`), `attachmentLimit/ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES.ts`, `attachmentLimit/MAX_ATTACHMENT_PAYLOAD_BYTES.ts`, `attachmentLimit/estimateAttachmentPayloadBytes.ts`, `attachmentLimit/isAttachmentPayloadWithinLimit.ts` |
| `domain/effort/` | `effort/EFFORT_LEVELS.ts`, `effort/clampEffort.ts`, `effort/defaultEffortFor.ts`, `effort/effortLabel.ts`, `effort/effortLevelsFor.ts`, `effort/isCLIClaudeModel.ts`, `effort/supportsEffort.ts`; барель корня удаляется (§3.2) |
| `state/link/` | `state/AuthCarburetor.ts`, `state/auth.ts`, `state/ConnectionCarburetor.ts`, `state/connection.ts` |
| `state/config/` | `state/ConfigCarburetor.ts`, `state/config.ts`, `state/SkillsCarburetor.ts`, `state/skills.ts`, `state/McpCarburetor.ts`, `state/mcp.ts` |
| `state/transcript/` | `state/MessagesCarburetor.ts`, `state/messages.ts`, `state/QueueCarburetor.ts`, `state/queue.ts`, `state/SelectionCarburetor.ts`, `state/selection.ts` |
| `state/status/` | `state/NoticesCarburetor.ts`, `state/notices.ts`, `state/BusyCarburetor.ts`, `state/busy.ts`, `state/SignalsCarburetor.ts`, `state/signals.ts` |
| `state/sessions/` | `state/SessionsCarburetor.ts`, `state/sessions.ts` |
| `state/live/` | `state/LiveWorkCarburetor.ts`, `state/liveWork.ts`, `state/WakeSchedulesCarburetor.ts`, `state/wakeSchedules.ts`, `state/WakeCancelCarburetor.ts`, `state/wakeCancel.ts` |
| `state/runtime/` | `state/SitterCarburetor.ts`, `state/sitter.ts`, `state/BatchWorkCarburetor.ts`, `state/batchWork.ts`, `state/myPrompts.ts`, `state/asyncJobCompletions.ts`, `state/asyncJobStatuses.ts` |
| `backend/index.ts` | `ws.ts` (фасад, §3.1) |
| `backend/ws/` | `net/WSClient.ts`, `net/client.ts`, `net/requestTracker.ts`, `net/SessionRequestTracker.ts`, `reconnectTranscript.ts` |
| `backend/ws/requests/` | `net/wsRequest.ts`, `net/WSRequestError.ts`, `net/sendLoadMessages.ts`, `net/sendListSessions.ts` |
| `backend/keepAlive/` | `keepAlive/KeepAlive.ts`, `keepAlive/keepAliveRuntime.ts`; из `actions/`: `actions/setKeepAliveEnabled.ts` |
| `backend/queue/` | `queueOnIdle/drainSendableQueue.ts`, `queueOnIdle/flushQueueOnIdle.ts`, `queueOnIdle/sendQueuedItem.ts` |
| `backend/sitter/` | `sitter/SitterRunner.ts`, `sitter/sitterLoop.ts` |
| `backend/telemetry/` | `telemetry.ts` (→ `index.ts`), `telemetry/logClientEvent.ts`, `telemetry/setupTelemetry.ts` |
| `backend/batch/` | `batchWork.ts` (→ `index.ts`), `batch/pickBatchWork.ts`, `batch/buildBatchTree.ts` |
| `actions/index.ts` | `store.ts` (фасад, §3.1) |
| `actions/theme/` | `actions/THEME_STORAGE_KEY.ts`, `actions/applyTheme.ts`, `actions/restoreCachedTheme.ts`, `actions/setTheme.ts` |
| `actions/config/providers/` | `actions/ConfigScope.ts`, `actions/addCustomProvider.ts`, `actions/removeCustomProvider.ts`, `actions/updateCustomProvider.ts`, `actions/setProviderKey.ts`, `actions/removeProviderKey.ts` |
| `actions/config/peakHours/` | `actions/setProviderPeakHours.ts`, `actions/pauseProviderPeakHours.ts`, `actions/resumeProviderPeakHours.ts`, `actions/fetchPeakHoursStatus.ts` |
| `actions/config/contexts/` | `actions/addContextPath.ts`, `actions/removeContextPath.ts`, `actions/addSkillsPath.ts`, `actions/removeSkillsPath.ts` |
| `actions/sessions/lifecycle/` | `actions/removeSession.ts`, `actions/setActiveSession.ts`, `actions/summarizeSession.ts`, `actions/cancelQueuedSummarize.ts`, `actions/removeSessionQueueAndBusy.ts`, `actions/removeSubAgentState.ts`, `actions/updateTodos.ts` |
| `actions/sessions/models/` | `actions/getDefaultModelKey.ts`, `actions/removeRecentModel.ts`, `actions/trackModelUsage.ts`, `actions/sendWithFastModel.ts` |
| `actions/sessions/slots/` | `actions/setSessionModel.ts`, `actions/clearSessionModelSlot.ts`, `actions/setSessionRoleEffort.ts`, `actions/roleFieldPrefix.ts` |
| `actions/messages/edit/` | `actions/deleteMessage.ts`, `actions/deleteMessagePart.ts`, `actions/deleteMessages.ts`, `actions/updateMessageContent.ts`, `actions/updateMessagePart.ts`, `actions/updateMessageThinking.ts` |
| `actions/messages/nav/` | `actions/togglePinMessage.ts`, `actions/tombstoneMessage.ts`, `actions/jumpToMessage.ts`, `actions/collectTurnContent.ts`, `actions/rerunFromMessage.ts` |
| `actions/queue/` | `actions/queuedMessagePayload.ts`, `actions/sendFailureState.ts`, `actions/describeSendFailure.ts`, `actions/sendQueuedMessageDirect.ts`, `actions/sendQueuedMessageNow.ts`, `actions/interruptAndSendQueuedMessage.ts`, `actions/flushQueuedMessagesOnIdle.ts` |
| `actions/liveWork/protocol/` | `liveWork/LIVE_WORK_REQUEST_PREFIX.ts`, `liveWork/WAKE_SCHEDULES_CANCEL_PREFIX.ts`, `liveWork/WAKE_SCHEDULES_GET_PREFIX.ts`, `liveWork/isLiveWorkRequestID.ts`, `liveWork/isWakeSchedulesCancelRequestID.ts`, `liveWork/isWakeSchedulesGetRequestID.ts`, `liveWork/isStaleWakeSchedulesSnapshot.ts` |
| `actions/liveWork/transfer/` | `liveWork/pickLiveWork.ts`, `liveWork/pickWakeSchedules.ts`, `liveWork/sendGetSessionLiveWork.ts`, `liveWork/sendGetSessionWakeSchedules.ts`, `liveWork/sendCancelWakeSchedule.ts`, `liveWork/applyWakeSchedulesSnapshot.ts` |
| `actions/liveWork/wakeSnapshot/` | `wakeSnapshot/pickPendingWake.ts`, `wakeSnapshot/isStaleWakeSnapshot.ts` |
| `ui/Login.tsx` | `components/Login.tsx` |
| `ui/shared/` | `components/ConfirmDialog.tsx`; из `Chat/`: `components/Chat/findSession.ts`; из `Message/`: `components/Message/watchCollapseAllSignal.ts`, `components/Message/watchExpandToolCallSignal.ts` |
| `ui/shared/markdown/` | из `Message/`: `components/Message/TextBlock.tsx`, `components/Message/DiffView.tsx`, `components/Message/mdRemarkPlugins.ts`, `components/Message/mdRehypePlugins.ts` |
| `ui/shared/notices/` | из `Message/`: `components/Message/BackgroundJobNotice.tsx`, `components/Message/FinishErrorBlock.tsx`, `components/Message/StreamPausedBlock.tsx` |
| `ui/shared/edit/` | из `Message/`: `components/Message/EditForm.tsx`, `components/Message/EditPreview.tsx` |
| `ui/sidebar/` | `components/Sidebar.tsx`, `components/sidebar/SessionRow.tsx`, `components/sidebar/formatTokens.ts` |
| `ui/livework/` | `components/LiveWorkPanel.tsx`, `components/LiveWorkTabBar.tsx`, `components/TodoList.tsx`, `components/WakeScheduleList.tsx`, `components/StatusBar.tsx`, `components/BatchTree.tsx` |
| `ui/livework/batchControl/` | `components/batchControls.ts`; `components/batchControl/batchControlCommands.ts`, `components/batchControl/batchControlPayload.ts`, `components/batchControl/batchControlTypes.ts`, `components/batchControl/batchNodeNotice.ts`, `components/batchControl/batchNodeStatus.ts`, `components/batchControl/batchResultMessage.ts`, `components/batchControl/isStopConfirmed.ts`, `components/batchControl/requestBatchControl.ts` |
| `ui/models/` | `components/ModelSelector.tsx`, `components/CustomRolesEditor.tsx`, `components/ScopedModelsModal.tsx` |
| `ui/models/modelList/` | `components/modelList/buildModelList.ts`, `components/modelList/buildProviderGroups.ts`, `components/modelList/defaultModelKey.ts`, `components/modelList/deriveModelSelection.ts`, `components/modelList/modelListTypes.ts`, `components/modelList/resolveRecentModels.ts`, `components/modelList/searchModels.ts` |
| `ui/models/scopedModels/pickers/` | `components/scopedModels/EffortPicker.tsx`, `components/scopedModels/ModelPicker.tsx`, `components/scopedModels/SLOTS.ts`, `components/scopedModels/pickedEffort.ts` |
| `ui/models/scopedModels/rows/` | `components/scopedModels/ScopedSlotRow.tsx`, `components/scopedModels/ScopedSlotsBlock.tsx`, `components/scopedModels/SessionSlotRow.tsx`, `components/scopedModels/SessionSlotsBlock.tsx`, `components/scopedModels/setSessionModelSlot.ts` |
| `ui/models/scopedModels/types/` | `components/scopedModels/scopedModelsTypes.ts`, `components/scopedModels/inheritedText.ts` |
| `ui/providers/` | `components/ProvidersModal.tsx`, `components/PeakHoursEditor.tsx`, `components/PeakHoursStatus.tsx` |
| `ui/providers/forms/` | `components/providers/ProviderForm.tsx`, `components/providers/ProviderRow.tsx`, `components/providers/BuiltinProviderEditor.tsx`, `components/providers/ModelEditor.tsx`, `components/providers/ModelDraft.ts`, `components/providers/ScopeToggle.tsx` |
| `ui/providers/draft/` | `components/providers/emptyModel.ts`, `components/providers/providerFormTypes.ts`, `components/providers/wireModels.ts` |
| `ui/providers/peakHours/rules/` | `components/peakHours/DAYS.ts`, `components/peakHours/END_RE.ts`, `components/peakHours/HH_MM_RE.ts`, `components/peakHours/MAX_WINDOWS.ts`, `components/peakHours/canonicalDays.ts`, `components/peakHours/toggleDay.ts` |
| `ui/providers/peakHours/policy/` | `components/peakHours/defaultPolicy.ts`, `components/peakHours/legacyToPolicy.ts`, `components/peakHours/peakDaysSet.ts`, `components/peakHours/peakHoursProblems.ts`, `components/peakHours/peakSummary.ts`, `components/peakHours/peakSummaryText.ts` |
| `ui/providers/mcp/` | `components/MCPSettings.tsx`; `components/mcp/MCPForm.tsx`, `components/mcp/PLACEHOLDER_JSON.ts`, `components/mcp/ServerRow.tsx`, `components/mcp/StatusBadge.tsx`, `components/mcp/buildInitialJson.ts`, `components/mcp/mcpServerPayload.ts` |
| `ui/chat/Chat/` | `components/Chat/index.tsx`, `components/Chat/Chat.tsx` |
| `ui/chat/Chat/transcript/` | `components/Chat/ChatScroller.tsx`, `components/Chat/TranscriptList.tsx`, `components/Chat/buildRenderItems.ts`, `components/Chat/EmptyState.tsx`, `components/Chat/BusyIndicator.tsx`, `components/Chat/AgentErrorBanner.tsx`, `components/Chat/ToolRun.tsx` |
| `ui/chat/Chat/queue/` | `components/Chat/QueueSection.tsx`, `components/Chat/QueuedMessageItem.tsx` |
| `ui/chat/Chat/selection/` | `components/Chat/SelectionToolbar.tsx`, `components/Chat/clearSelection.ts` |
| `ui/chat/ChatInput/` | `components/ChatInput/index.tsx` |
| `ui/chat/ChatInput/composer/` | `components/ChatInput/ChatInput.tsx`, `components/ChatInput/ComposerActions.tsx`, `components/ChatInput/fitTextarea.ts`, `components/ChatInput/foldQueuedDraft.ts`, `components/ChatInput/buildSendPayload.ts` |
| `ui/chat/ChatInput/attachments/` | `components/ChatInput/AttachmentBadges.tsx`, `components/ChatInput/PendingAttachment.ts`, `components/ChatInput/readFileAsBase64.ts`, `components/ChatInput/toWireAttachments.ts`, `components/ChatInput/formatBytes.ts` |
| `ui/chat/ChatInput/menus/` | `components/ChatInput/HistoryMenu.tsx`, `components/ChatInput/SlashMenu.tsx`, `components/ChatInput/slashQueryOf.ts`, `components/ChatInput/filterSkills.ts`, `components/ChatInput/sourceBadgeClass.ts` |
| `ui/chat/ChatInput/banners/` | `components/ChatInput/ForeignOwnedBanner.tsx`, `components/ChatInput/OfflineBanner.tsx` |
| `ui/chat/ChatToolbar/` | `components/ChatToolbar/index.tsx`, `components/ChatToolbar/ChatToolbar.tsx` |
| `ui/chat/ChatToolbar/pills/` | `components/ChatToolbar/BusyDots.tsx`, `components/ChatToolbar/TokenPill.tsx`, `components/ChatToolbar/UpdateBadge.tsx`, `components/ChatToolbar/SitterPill.tsx`, `components/ChatToolbar/ThemeToggle.tsx`, `components/ChatToolbar/KeepAliveToggle.tsx` |
| `ui/chat/ChatToolbar/controls/` | `components/ChatToolbar/CompactButton.tsx`, `components/ChatToolbar/CollapseAllButton.tsx`, `components/ChatToolbar/MoreMenu.tsx` |
| `ui/chat/ChatToolbar/modals/` | `components/ChatToolbar/ToolbarModal.ts`, `components/ChatToolbar/ToolbarModals.tsx`, `components/ChatToolbar/SystemPromptModal.tsx`, `components/ChatToolbar/SystemPromptFetch.ts`, `components/ChatToolbar/ShutdownConfirm.tsx`; из корня `components/`: `components/LogsModal.tsx`, `components/SettingsModal.tsx` |
| `ui/chat/ChatToolbar/model/` | `components/ChatToolbar/ModelCluster.tsx`, `components/ChatToolbar/activeSmartModel.ts`, `components/ChatToolbar/lookupModel.ts`, `components/ChatToolbar/formatTokens.ts`, `components/ChatToolbar/pctColor.ts` |
| `ui/chat/Message/message/` | `components/Message/Message.tsx`, `components/Message/Part.tsx`, `components/Message/IntermediateAssistantMessage.tsx`, `components/Message/agentPrompt.ts`; из корня `components/`: `components/ForkSessionModal.tsx` |
| `ui/chat/Message/content/` | `components/Message/AssistantBlock.tsx`, `components/Message/AssistantContent.tsx`, `components/Message/UserContent.tsx`, `components/Message/ThinkingPart.tsx`, `components/Message/StandaloneThinking.tsx`, `components/Message/blocks.ts`, `components/Message/extractText.ts` |
| `ui/chat/Message/tools/` | `components/Message/ToolCallBlock.tsx`, `components/Message/ToolResultBlock.tsx`, `components/Message/ToolActivityGroup.tsx`, `components/Message/groupToolActions.ts`, `components/Message/fileWriteTools.ts` |
| `ui/chat/Message/controls/` | `components/Message/ActionRow.tsx`, `components/Message/AssistantHoverActions.tsx`, `components/Message/UserHoverActions.tsx`, `components/Message/CopyButton.tsx`, `components/Message/CopyTurnButton.tsx`, `components/Message/tallyActions.ts` |
| `ui/chat/Message/badges/` | `components/Message/DurationBadge.tsx`, `components/Message/TimeBadge.tsx`, `components/Message/UsageBadge.tsx`, `components/Message/EffortBadge.tsx` |
| `ui/chat/Message/states/` | `components/Message/isTerminallyFinished.ts`, `components/Message/AskQuestionBlock.tsx`, `components/Message/parseAwaitingAnswer.ts`, `components/Message/CheckboxContextMenu.tsx` |
| `ui/chat/Message/subagent/` | из корня `components/`: `components/SubAgentBlock.tsx`; из `Message/`: `components/Message/SubAgentFrame.tsx`, `components/Message/SubAgentMessage.tsx`, `components/Message/SummaryMessage.tsx`, `components/Message/summarizeSubTranscript.ts`, `components/Message/selectSubAgentView.ts`, `components/Message/findErrorFinish.ts` |
| удаляются (§3.2) | `asyncJobCompletion.ts`, `completionIndex.ts`, `effort.ts`, `components/Message/index.tsx` |

Сумма таблицы (три независимых подсчёта, все сходятся; «нетестовый файл» = рукописный `.ts`/`.tsx` без суффиксов `.test`/`.spec`, включая `index.tsx`-фасады):

1. По исходным каталогам — подкаталоги и корень `components` (без корня `src`): `actions` 52 + `liveWork` 13 + `wakeSnapshot` 2 + `state` 37 + `net` 8 + `keepAlive` 2 + `queueOnIdle` 3 + `sitter` 2 + `telemetry` 2 + `batch` 2 + `effort` 7 + `completion` 7 + `asyncJob` 7 + `attachmentLimit` 4 + `customRole` 3 + корень `components` 22 + `sidebar` 2 + `modelList` 7 + `scopedModels` 11 + `providers` 9 + `peakHours` 12 + `batchControl` 8 + `mcp` 6 + `Chat` 14 + `ChatInput` 18 + `ChatToolbar` 21 + `Message` 48 = 329; корень `src` — 19 файлов; 329 + 19 = **348** ✓.
2. По исходу: переезжают 344, удаляются 4 (`asyncJobCompletion.ts`, `completionIndex.ts`, `effort.ts`, `components/Message/index.tsx`); 344 + 4 = **348** ✓.
3. По целям: `app` 5 + `domain` 35 (types 1, text 4, asyncJob 7, completion 7, customRole 4, attachmentLimit 5, effort 7) + `state` 37 + `backend` 24 (index 1, ws 5, ws/requests 4, keepAlive 3, queue 3, sitter 2, telemetry 3, batch 3) + `actions` 67 (index 1, theme 4, providers 6, peakHours 4, contexts 4, lifecycle 7, models 4, slots 4, edit 6, nav 5, queue 7, protocol 7, transfer 6, wakeSnapshot 2) + `ui` 176 (Login 1, shared 13 = 4+4+3+2, sidebar 3, livework 6 + batchControl 9, models 3 + modelList 7 + scopedModels 11, providers 3 + forms 6 + draft 3 + peakHours 12 + mcp 7, Chat 2+7+2+2 = 13, ChatInput 1+5+5+5+2 = 18, ChatToolbar 2+6+3+7+5 = 23, Message 5+7+5+6+4+4+7 = 38; 1+13+3+15+21+31+13+18+23+38 = 176) = **344** = 348 − 4 удалённых ✓. Прибытия/убытия на стыках: `ui` получает корень `components` (22 файла: `WsConnector` → `app`, `SubAgentBlock`/`ForkSessionModal` → `Message`, `LogsModal`/`SettingsModal` → `ChatToolbar/modals`, `MCPSettings` → `providers/mcp`, `ConfirmDialog` → `shared`, `Login` → корень `ui`, остальные 14 по фичам) + `findSession.ts` из `Chat`; `Message` 48 → −1 (`index.tsx`) − 11 в `shared` (TextBlock, DiffView, mdRemarkPlugins, mdRehypePlugins, watchCollapseAllSignal, watchExpandToolCallSignal, BackgroundJobNotice, FinishErrorBlock, StreamPausedBlock, EditForm, EditPreview) + 2 из корня `components` (SubAgentBlock, ForkSessionModal) = 38 ✓; `ChatToolbar` 21 → 23 (+LogsModal, +SettingsModal); `Chat` 14 → 13 (−`findSession.ts`); `actions` = 52 + 13 + 2 − 1 (`setKeepAliveEnabled` → backend) + 1 (`store.ts` → index) = 67 ✓; `backend` = 8 + 2 + 3 + 2 + 2 + 2 + 5 корневых (`ws.ts`, `telemetry.ts`, `batchWork.ts`, `reconnectTranscript.ts`, `setKeepAliveEnabled.ts`) = 24 ✓; `app` = 4 корневых + 1 (`WsConnector`) = 5 ✓. Автосверка полосы L10: каждое имя из этой таблицы встречается в ней ровно один раз, и множество совпадает с `git ls-files` (проверено при написании: 348 = 348).

### 2.5. Тесты: «файл → цель» (20 юнит-тестов)

| Тест (сегодня) | Цель | Поведение, которое он держит |
|---|---|---|
| `asyncJobMetadata.test.ts` | `domain/text/` | классификатор pending-метаданных async-инструментов |
| `attachmentLimit.test.ts` | `domain/attachmentLimit/` | бюджет вложений |
| `batchWork.test.ts` | `backend/batch/` | сборка batch-дерева и выборка работы |
| `completionIndex.test.ts` | `domain/completion/` | слияние индекса завершений |
| `customRoleTypes.test.ts` | `domain/customRole/` | роли: mode/payload/at |
| `keepAlive.test.ts` | `backend/keepAlive/` | такты keep-alive |
| `liveWorkAwaiting.test.ts` | `domain/text/` | awaiting-бейдж живой работы |
| `reconnectTranscript.test.ts` | `backend/ws/` | перезагрузка транскрипта после reconnect |
| `state_auth.test.ts` | `state/link/` | хранилище входа |
| `state_connection.test.ts` | `state/link/` | хранилище связи |
| `state_mcp.test.ts` | `state/config/` | хранилище MCP |
| `state_messages.test.ts` | `state/transcript/` | хранилище сообщений |
| `state_misc.test.ts` | `state/` (корень; тест — не элемент) | Busy/Config/Selection/Signals/Sitter/Skills одним файлом |
| `state_notices.test.ts` | `state/status/` | хранилище уведомлений |
| `state_sessions.test.ts` | `state/sessions/` | хранилище сессий |
| `state_wakeCancel.test.ts` | `state/live/` | отмена wake-расписаний |
| `store.regression.test.ts` | `domain/text/` | регресс `mergePreserveContent` |
| `store_commands.test.ts` | `backend/queue/` | очередь отправки при простое + `QueueCarburetor` |
| `store_livework.test.ts` | `actions/liveWork/` | wake-снапшоты + хранилища live work |
| `components/batchControls.test.ts` | `ui/livework/batchControl/` | batch-команды; сегодня CI-глобом не покрывается, покрывается после L0 |

Оракул тестов: число `test(` на множестве 20 файлов до/после совпадает; имена файлов сохраняются (переименований нет — greppability), меняются только пути. `state_misc.test.ts` остаётся одним файлом: его идентификаторы — экспортируемые классы `Carburetor`, разъехавшиеся по подкаталогам `state/`; тест пользуется только публичными именами, «белым» не является и файла-элемента не добавляет.

## 3. Швы

Ни один цикл не разрывается новым швом — на уровне каталогов циклов нет (§1.3). Швы ниже — это (а) сохранение существующих фасадов как единственных точек входа слоёв, (б) удаление четырёх барелей, которые не помещаются в целевые каталоги как файлы-элементы, (в) размещение `useWS` так, чтобы у `ui` не появилось ребра к `app`. Для каждого шва: идентификаторы границы и аргумент сохранения поведения.

### 3.1. Фасады `store.ts` → `actions/index.ts` и `ws.ts` → `backend/index.ts`

`src/store.ts` (73 строки: бок-эффект `import "./actions/restoreCachedTheme"` + 8 реэкспортов типов + ~40 реэкспортов функций) переезжает в `src/actions/index.ts` без изменения списка экспортов: `./actions/X` → `./theme/X` и т.д. по таблице §2.4; `./liveWork/pickLiveWork` → `./liveWork/transfer/pickLiveWork`; `./wakeSnapshot/pickPendingWake` → `./liveWork/wakeSnapshot/pickPendingWake` (алиас `pickPendingWake as pickPendingWakeSchedule` сохраняется, `store.ts:101`); `./mergeMessage` → `../domain/text/mergeMessage`; `./completionIndex` → `../domain/completion/completionTypes` (тип `MyPromptItem`); типы состояния `./state/...` → `../state/...`. Потребители (`ui` 36 файлов, `useWS`) меняют только спецификатор `"../store"` → `"../actions"` (после переезда файлов — `"../../actions"`).

Поведение: бок-эффект восстановления темы выполняется при первой загрузке модуля фасада — `actions/index.ts` оценивается в тот же момент жизни приложения, что и `store.ts` сегодня (первый импорт фасада из `useWS`/компонента); порядок «тема восстановлена до первого рендера» сохраняется, потому что цепочка импортов `main` → `App` → `WsConnector` → `actions` неизменна по составу.

`src/ws.ts` (8 строк: `ws`, `wsRequest` + тип `WSRequestOptions`, `WSRequestError`, `requestTracker`, `sendLoadMessages`, `sendListSessions`) → `src/backend/index.ts`, пути `./net/X` → `./ws/X`, `./ws/requests/X`. Импорты `"../ws"` в `ui` меняются на `"../backend"`. Экспортируемые имена — те же семь.

Граница «actions → ui» несёт ~48 имён — это СУЩЕСТВУЮЩИЙ фасад (сегодняшний `store.ts`), разрезка его не расширяет; правило «≤ ~15 имён на новую границу» (план §3 п. 3, В6) применяется к новым границам, и все они узкие (§3.5). `backend/index.ts` — те же 7 имён, что у `ws.ts`.

### 3.2. Барели, которые не переезжают, а удаляются (3 + 1) и превращаются в `index.ts` (4)

| Файл сегодня | Судьба | Потребители (цензус, все перечислены) | Правка |
|---|---|---|---|
| `completionIndex.ts` (8 строк) | удалить | `store.ts` (тип `MyPromptItem`) | `actions/index.ts` реэкспортирует тип из `../domain/completion/completionTypes` |
| `effort.ts` (8 строк) | удалить | `components/CustomRolesEditor.tsx`, `components/ModelSelector.tsx`, `components/scopedModels/pickedEffort.ts`, `components/scopedModels/EffortPicker.tsx`, `components/modelList/deriveModelSelection.ts`, `components/Message/EffortBadge.tsx` | 6 файлов импортируют файлы `domain/effort/` напрямую (по списку реэкспортов `effort.ts`) |
| `asyncJobCompletion.ts` (4 строки) | удалить | `components/Message/ActionRow.tsx`, `components/Message/Message.tsx`, `components/Message/SubAgentFrame.tsx` | 3 файла импортируют `asyncJobTypes` (типы), `isAsyncCompletionNotice`, `indexAsyncJobCompletions` из `domain/asyncJob/` напрямую |
| `components/Message/index.tsx` (6 имён) | удалить | `components/Chat/ToolRun.tsx:5` (`ToolActivityGroup`), `components/Chat/TranscriptList.tsx:3` (`IntermediateAssistantMessage`, `Message`) — единственные внешние потребители каталога `../Message` | 2 файла переписывают импорт на файлы-хозяева: `../Message/tools/ToolActivityGroup`, `../Message/message/IntermediateAssistantMessage`, `../Message/message/Message` |
| `customRoleTypes.ts` (33 строки) | → `domain/customRole/index.ts` | `types.ts`, `customRole/*` (3), `components/CustomRolesEditor.tsx`, `tests/custom-roles.spec.ts:8` | пути обновляются; содержимое (3 реэкспорта + интерфейс `CustomRole` и родня) не меняется |
| `attachmentLimit.ts` (5 строк) | → `domain/attachmentLimit/index.ts` | `components/ChatInput/ChatInput.tsx` | путь обновляется |
| `telemetry.ts` | → `backend/telemetry/index.ts` | `main.tsx` (`setupTelemetry`); `logClientEvent` импортируется из actions и useWS напрямую, мимо бареля | путь обновляется |
| `batchWork.ts` (5 строк) | → `backend/batch/index.ts` | `useWS.ts`, `components/BatchTree.tsx`, `components/LiveWorkPanel.tsx` (файл `state/asyncJobCompletions.ts` импортирует `./batchWork` — одиночку состояния, не этот барель) | пути обновляются |

Аргумент сохранения поведения: все четыре удаляемых файла — чистые реэкспорты (проверено содержимым, §1.4), они не добавляют имён; имена продолжают экспортироваться файлами-хозяевами, поэтому оракул «список экспортов множества файлов» не меняется. Правило «не плодить барели, скрывающие циклы» (CLAUDE.md) не нарушается: два оставленных фасада (`actions/index`, `backend/index`) существуют сегодня и их замыкание ациклично (§2.2); `index.ts` в `helpers` (§4.4) — над листовыми утилитами без циклов.

### 3.3. `useWS.ts` и `WsConnector.tsx` — в `app/`

`useWS.ts` (783 строки) — маршрутизатор WS-событий: обработчики `on*`, `connectWS()` (`useWS.ts:755`). Импортёры: ровно один файл — `components/WsConnector.tsx:2` (цензус `rg 'from "\.\.?/useWS"'` — один файл). `WsConnector` (9 строк, монтирует `connectWS` в `useEffect`) рендерится из `App.tsx`. Оба переезжают в `app/`: ребро `app → actions/backend/state` направлено вниз, ребро `ui → app` не возникает ни при каком раскладе — это и есть шов против цикла «оболочка ↔ виджеты». Порядок вызовов не меняется: `App` → `WsConnector` → `connectWS` — та же цепочка в тех же `useEffect`-точках; роутер остаётся одним файлом (783 < 800), дробить его полосы не должны — его поведение держат Playwright-спеки (§5, L5).

### 3.4. Ребро `net → state` (`WSClient` → `connectionCarburetor`) — шов не нужен

`net/WSClient.ts:2` пишет `connectionCarburetor.setOutboxCount(this.outbox.length)` в трёх точках (148, 158, 162) при изменении оффлайн-очереди. В целевом дереве это ребро `backend/ws → state/link` (`connection` живёт в `state/link/`), направление `backend → state` — вниз по слоям (§2.2), цикла нет, порядок вызовов не меняется (тот же метод, те же точки). Введение порта-колбэка здесь возможных циклов не чинит и меняет момент записи — по правилу §3 п. 1 не делается. Альтернатива (порт `onOutboxCount` в конструктор `WSClient`) — кандидат на будущий спринт, не на этот (§6).

### 3.5. Новые границы и число имён через них (правило ≤ ~15)

| Граница | Имена (экспорты каталога-источника, которые читает верхний слой) | Оценка |
|---|---|---|
| `state/link → все` | `authCarburetor`, `AuthCarburetor`, `connectionCarburetor`, `ConnectionCarburetor` + 2 интерфейса состояния | 6 |
| `state/transcript → все` | 3 одиночки + 3 класса + 3 интерфейса (`messages`, `queue`, `selection`) | 9 |
| `state/live → все` | 3 одиночки + 3 класса + 3 интерфейса + типы wake-снапшотов | 10 |
| `backend/ws → все` | `ws`, `WSClient`, `requestTracker`, `SessionRequestTracker`, `wsRequest`, `WSRequestError`, `sendLoadMessages`, `sendListSessions` + типы | 9 |
| `actions/queue → все` | по 1–2 экспорта на файл × 7 файлов | ≤ 12 |
| `actions/liveWork/protocol → transfer, useWS` | 3 префикса + 4 предиката + `isStaleWakeSchedulesSnapshot` | 8 |
| `ui/shared → фичи` | `ConfirmDialog`, `findSession`, `watchCollapseAllSignal`, `watchExpandToolCallSignal`, `TextBlock`, `DiffView`, `MD_REMARK`, `MD_REHYPE`, `BackgroundJobNotice`, `FinishErrorBlock`, `StreamPausedBlock`, `EditForm`, `EditPreview` | 13 |

Каждая новая граница ≤ 15 имён; граница `actions/index → ui` (~48) — существующий фасад `store.ts`, она не расширяется (§3.1).

### 3.6. CI-глоб юнит-тестов (`.github/workflows/ci.yml:356`)

Сегодня: `node --test src/*.test.ts` — одноуровневый глоб, 19 файлов; `components/batchControls.test.ts` не исполняется вовсе. Полоса L0 меняет шаг на `node --test $(git ls-files "src/*.test.ts" "src/*/*.test.ts" "src/*/*/*.test.ts" "src/*/*/*/*.test.ts")` (рабочая директория `web`; `git ls-files` возвращает только существующие файлы, глубина 4 покрывает `state/link/x.test.ts` и `ui/livework/batchControl/x.test.ts`). Это правка CI-конфига, не cron — запрет плана §3 п. 9 её не касается. Сборка (`pnpm build`), `pnpm typecheck`, `pnpm lint` не меняются.

## 4. Глобалы, инициализация, ресурсы, тесты, смежные эффекты

### 4.1. Модульное состояние: реестр (каждый одиночка — ровно в одном модуле)

| Одиночка | Сегодня | Цель | Кто читает |
|---|---|---|---|
| 17 экземпляров `*Carburetor` — `authCarburetor`, `batchWorkCarburetor`, `busyCarburetor`, `configCarburetor`, `connectionCarburetor`, `liveWorkCarburetor`, `mcpCarburetor`, `messagesCarburetor`, `noticesCarburetor`, `queueCarburetor`, `selectionCarburetor`, `sessionsCarburetor`, `signalsCarburetor`, `sitterCarburetor`, `skillsCarburetor`, `wakeCancelCarburetor`, `wakeSchedulesCarburetor` | `state/<имя>.ts` | `state/<домен>/<имя>.ts` | компоненты, actions, useWS — импорт файла-одиночки напрямую (как сегодня) |
| 3 `computed`-производных: `myPrompts`, `asyncJobCompletions`, `asyncJobStatuses` | `state/*.ts` | `state/runtime/` | ChatInput/HistoryMenu, Chat, ActionRow, SubAgentBlock |
| `ws` (единственный `WSClient`) | `net/client.ts` | `backend/ws/client.ts` | все отправители через `backend/index` или напрямую |
| `requestTracker` | `net/requestTracker.ts` | `backend/ws/requestTracker.ts` | `wsRequest`, `sendLoadMessages`, `sendListSessions`, actions |
| бок-эффект `restoreCachedTheme` | `actions/restoreCachedTheme.ts` | `actions/theme/restoreCachedTheme.ts`, запускается импортом из `actions/index.ts` | — (§3.1) |
| `let wsReady` (флаг однократной настройки телеметрии) | `telemetry/setupTelemetry.ts:12` | `backend/telemetry/setupTelemetry.ts` | файл сам |
| `let editorSeq` (счётчик ключей редактора) | `components/CustomRolesEditor.tsx:18` | `ui/models/CustomRolesEditor.tsx` | файл сам |

Полный реестр изменяемых модульных переменных получен `rg '^(let|var) '` по `src` (2 находки, обе в таблице; `const`-одиночки перечислены выше). Новый экспортируемый глобал не создаётся; `carburetor/no-module-level-store` (`web/.oxlintrc.carburetor.json`, `--max-warnings=0`) остаётся зелёным — подавления `oxlint-disable-next-line` переезжают вместе со своими файлами. Экспорт изменяемой переменной ради разрезки не вводится ни в одной полосе.

### 4.2. `init()`, `go:embed`, OS-пары

В `web/src` нет `init()`-аналогов с порядковой зависимостью: единственные импортируемые-ради-эффекта модули — `restoreCachedTheme` (§3.1) и `index.css` (§4.3); порядок оценки модулей ES-графа не меняется, потому что состав и направление рёбер импортов сохраняются (меняются только спецификаторы путей). `go:embed` в веб-клиенте отсутствует; `web/embed.go`/`embed_dev.go` встраивают `dist` и полосами не трогаются (`web/dist/.gitkeep` не удаляется — план §3 п. 9). OS-пар (`_windows`/`_unix`/...) в `web/src` нет (проверено `rg '\.(windows|unix|darwin|linux)\.(ts|tsx)$'` — пусто); платформенная логика живёт в Go.

### 4.3. CSS и ассеты

`src/index.css` переезжает в `app/index.css` вместе с `main.tsx` (импорт `"./index.css"` не меняется, `main.tsx:3`); `env.d.ts` с `declare module "*.css"` переезжает в `app/` и продолжает действовать (tsconfig `include: ["src"]` покрывает всё дерево). Прочих импортов ассетов в `src` нет (`rg 'from "[^"]*\.(png|jpg|svg|gif|woff)"'` — пусто). `tailwind.config.cjs` сканирует `./src/**/*.{ts,tsx}` — переезды внутри `src` на него не влияют.

### 4.4. Тестовые помощники `web/tests/helpers` (9 файлов, 9 элементов — строка храповика)

Цель: `helpers/index.ts` (новый, реэкспорт всех имён) + `helpers/factories/` (`make-config.ts`, `make-message.ts`, `make-permission.ts`, `make-session.ts`) + `helpers/ws/` (`clear-ws-sent.ts`, `fulfill-auth-ok.ts`, `mock-ws.ts`, `send-mock-ws-message.ts`, `wait-for-ws-send.ts`) = 3 элемента. 64 спеки меняют импорты `./helpers/<имя>` → `./helpers` (имена импортов не меняются — спеки импортируют именованные символы, пример `tests/chat.spec.ts:2`); внутри помощников `./mock-ws` → `../ws/mock-ws` (2 файла). `make-permission.ts` не импортируется ни спеками, ни `e2e`, ни `real-backend` (цензус) — переезжает в `factories/` с пометкой «мёртвый кандидат»; удаление — решение оператора, не этой разрезки. Помощники не импортируют `src` (проверено) — барель над ними циклов скрыть не может.

### 4.5. Playwright e2e: селекторы, пути, фикстуры

Селекторы (`data-test-id`, роли, тексты) — атрибуты DOM; компоненты переезжают файлами целиком, атрибуты не меняются, селекторы не затрагиваются. Спеки остаются в `web/tests/*.spec.ts` (64 файла не двигаются), значит `playwright.config.ts` (`testDir: "./tests"`, `testIgnore "**/real-backend/**"`) и `scripts/run-tests.mjs` не меняются. Файл-путей `src` в спеках ровно один: `tests/custom-roles.spec.ts:8` (`from "../src/customRoleTypes"`) → полоса L1 правит на `"../src/domain/customRole"`. `tests/tsconfig.json` (`include: ["./**/*.ts"]`) покрывает перегруппированных помощников без правки. `tests/e2e/` (7 файлов, свой конфиг и утилиты сессий; спеками не импортируется — проверено `rg 'from "./e2e/' tests/*.spec.ts` — пусто) и `tests/real-backend/` не трогаются — у `e2e` ровно 7 элементов, запаса на рост нет.

### 4.6. Go-сторона, CI, документы

Ссылок Go-кода на `web/src` как функциональных нет: все вхождения — комментарии, плюс один импорт в спеке. Полный перечень получен прогоном `rg 'web/src|web/tests'` по всем отслеживаемым файлам (сами этот документ и исторические документы исключены из правок); правит перечисленное та полоса, что названа в колонке «Кто правит» (внутренние комментарии `web/src` — L1, остальное — L10 одним заходом после всех полос; строки «не править» остаются как есть, их спеки не двигаются):

| Место | Упомянутый путь → куда переедет | Кто правит |
|---|---|---|
| `internal/store/db/sql/messages.sql:134` | `web/src/components/Message/Message.tsx` → `ui/chat/Message/message/Message.tsx` | L10: править комментарий в `.sql`-источнике и запустить `sqlc generate`; `internal/store/db/messages.sql.go:198` и `internal/store/db/querier.go:250` СГЕНЕРИРОВАНЫ из `.sql` и наследуют комментарий — руками их не править |
| `internal/store/message/content.go:231` | `web/src/ws.ts` → `backend/ws` | L10 |
| `internal/store/message/p731_delete_watermark_test.go:6,48,240` | `web/src/store.ts` → `actions/index.ts`; `web/src/ws.ts` (×2) → `backend/ws` | L10 |
| `internal/store/message/p595_delete_streaming_test.go:162` | `web/src/components/Message/Message.tsx` → `ui/chat/Message/message/Message.tsx` | L10 |
| `internal/session/session_fork.go:226` | `web/src/useWS.ts` → `app/useWS.ts` | L10 |
| `internal/serve/server/wire.go:106,124` | `web/src/store.ts` → `actions/index.ts` | L10 |
| `internal/serve/server/web_composer_history_test.go:6,26,27,50` | `web/src/store.ts` → `actions/index.ts`; `web/src/asyncJobCompletion.ts` (барель удаляется) | L10 |
| `internal/serve/server/protocol.go:753,821` | `web/src/types.ts` → `domain/types.ts` | L10 |
| `internal/serve/server/p684_delete_other_sessions_test.go:9` | `web/src/components/Sidebar.tsx` → `ui/sidebar/Sidebar.tsx` | L10 |
| `internal/serve/server/p460_model_scope_test.go:26` | `web/src/useWS.ts` → `app/useWS.ts` | L10 |
| `internal/serve/server/handlers_models.go:233` | `web/src/useWS.ts` → `app/useWS.ts` | L10 |
| `internal/serve/server/handlers_messages.go:241` | `web/src/components/Chat.tsx` → `ui/chat/Chat/Chat.tsx` | L10 |
| `internal/agent/coordinator_run.go:46` | `web/src/components/Message.tsx` (путь устарел ещё до разрезки) → `ui/chat/Message/message/Message.tsx` | L10 |
| `web/tests/ws-offline-request.spec.ts:13`, `web/tests/messages-list-delete-watermark.spec.ts:5`, `web/tests/merge-guard.spec.ts:2`, `web/tests/composer-history-filter.spec.ts:5`, `web/tests/message-timestamp-always-visible.spec.ts:8`, `web/tests/batch-structured-completions.spec.ts:15-16` | комментарии: `web/src/ws.ts`, `web/src/store.ts`, `web/src/components/Message/TimeBadge.tsx`, `web/src/completionIndex.ts` (барель удаляется), `web/src/types.ts` | L10 |
| `web/src/types.ts:136`, `web/src/asyncJobMetadata.test.ts:1`, `web/src/completionIndex.test.ts:1`, `web/src/liveWorkAwaiting.test.ts:1` | комментарии о путях `web/src/...` внутри переезжающих файлов | L1 (правит в своих же файлах) |
| `.githooks/file_size_allowlist.txt:10`, `.githooks/check_stale_model_slots.sh:16,38,139`, `.githooks/check_file_size_selftest.sh:24` | комментарии о `web/src/store.ts` → `actions/index.ts`; функциональных ссылок нет: страж моделей ищет по паттернам содержимого, а не по пути файла, в `file_size_allowlist.txt` записи для `store.ts` нет (только комментарий) | L10 |
| `tools/mutation/README.md:233` | `web/src/batchWork.test.ts` → `backend/batch/` | L10 (документация инструментов) |
| `internal/store/message/p731_delete_watermark_test.go:50`, `.githooks/check_stale_model_slots.sh:185`, `docs/peak-hours.md:274`, `CHANGELOG.fork.md:241` | пути `web/tests/*.spec.ts` | правка не нужна: спеки не двигаются |
| `tools/r12a_old_path_manifest.py:136` | regex старых путей `^web/src/` | не правится: инструмент намеренно документирует старые пути |

Упоминания в исторических документах (`docs/reviews/*`, `docs/research/*`, `docs/checkpoints/*`, `docs/rush-rename-scope.md:105`, `docs/plans/*`, `CHANGELOG.md`) не правятся — политика §3 п. 16 (4). `.github/ci-policy.toml`, `.github/workflows` и `.github/scripts` упоминаний `web/src`/`web/tests` не содержат (проверено полным прогоном по `.github`) — правки не требуют; `.github/workflows/ci.yml` (web-job: typecheck, lint, `node --test`, build) — меняется одна строка теста (§3.6, L0); `licenses.yml` и `publish-fork-npm.yml` смотрят только на `web/package.json`/`pnpm-lock.yaml`/`dist` — не трогаются. Комментарии в коде `web/src`, описывающие поведение, а не пути (например `StandaloneThinking.tsx:18` — «cross-message ToolRun»), не правятся.

## 5. Полосы исполнения

Общие правила: агент полосы не запускает `npm run build`/`pnpm test`/Playwright — только `pnpm typecheck` и `pnpm lint` (дёшево, входят в pre-push); сборку, юнит и Playwright гоняет CI на интеграционной ветке волны (план §3 п. 8, §5 D3). Каждая полоса — один коммит целиком (перенос файлов + правки импортов + удаление своей строки храповика в том же коммите): страж каталогов никогда не видит промежуточного состояния, временных записей в `.githooks/dir_entries_allowlist.txt` не нужно. Полосы с общими файлами идут строго последовательно (L1→L2→L3→L4→L5→L6→L7→L8); параллелить можно только пары с непересекающимися файлами (указано явно); одновременно открыто ≤ 5 полос. Приёмка каждой полосы: (1) оракул экспортов/тестов, (2) `pnpm typecheck`, (3) `pnpm lint` (счётчик предупреждений 101 не растёт), (4) `bash .githooks/check_dir_entries.sh` зелёный и без «RATCHET CLEANUP» после синхронного удаления строки, (5) grep-проверки из столбца «Приёмка».

| Полоса | Объём (файлы) | До этого | Оракул и приёмка (сверх общих) | Тесты-доказательства |
|---|---|---|---|---|
| **L0** (оркестратор) | 1 строка `.github/workflows/ci.yml:356`: глоб юнит-тестов → `node --test $(git ls-files "src/*.test.ts" "src/*/*.test.ts" "src/*/*/*.test.ts" "src/*/*/*/*.test.ts")` (§3.6) | — | job web зелёный в CI; `git ls-files` из `web/` возвращает 20 файлов | юнит-набор целиком (те же 19 + впервые `batchControls.test.ts`) |
| **L1** domain | `types.ts`, 4 файла `text/` + 2 теста, `asyncJob/` 7, `completion/` 7 + тест, `customRole/` 4, `attachmentLimit/` 5 + тест, `effort/` 7; удаление барелей `effort.ts`, `completionIndex.ts`, `asyncJobCompletion.ts` (§3.2); правка импортов: `store.ts`, `state/messages.ts`, `state/myPrompts.ts`, `state/asyncJobCompletions.ts`, `state/asyncJobStatuses.ts`, 6 потребителей `effort`, 3 потребителя `asyncJobCompletion`, `tests/custom-roles.spec.ts:8` — все перечислены в §3.2 | L0; правит файлы будущих полос L2-L8, поэтому идёт строго до них | `rg 'from "[^"]*completionIndex"' src` пусто; `rg 'from "[^"]*effort"' src` — только прямые импорты файлов `domain/effort/`; список экспортов перенесённого множества до/после равен | юнит: completionIndex, attachmentLimit, customRoleTypes, asyncJobMetadata, liveWorkAwaiting, store.regression; Playwright (CI): custom-roles, reasoning-effort, provider-catalog-effort, composer-history-filter |
| **L2** state | 37 файлов → 7 подкаталогов + раскладка 8 тестов `state_*.test.ts` по §2.5; правка импортов во ВСЕХ потребителях state (actions 34 ребра, `net/WSClient.ts`, `liveWork/*` 4, `sitter/SitterRunner.ts`, `telemetry/logClientEvent.ts`, `useWS.ts` 17 импортов, `App.tsx` 2, `components/*` 40 файлов) — полоса массовых правок, идёт одна (план §3 п. 15) | L1 | все пути state имеют вид `state/<домен>/...` (`rg 'from "[^"]*state/' src`); `rg 'from "\.\./state/' src/components` пусто; элементы state = 7, строка храповика удалена | юнит: state_auth, state_connection, state_mcp, state_messages, state_misc, state_notices, state_sessions, state_wakeCancel; Playwright (CI): sessions, settings, auth, subagent-state-cleanup |
| **L3** backend | `net/` 8, `ws.ts`→`backend/index.ts`, `keepAlive/` 2 + `setKeepAliveEnabled.ts` из actions + `keepAlive.test.ts`, `queueOnIdle/` 3 + `store_commands.test.ts`, `sitter/` 2, `telemetry/` 3, `batch/` 3 + `batchWork.test.ts`, `reconnectTranscript.ts` + тест; правка импортов: actions (~21 ребро), `liveWork/*` 3, `useWS.ts`, `main.tsx`, `components/ChatToolbar/SitterPill.tsx`, `components/ChatInput/ChatInput.tsx`, `components/BatchTree.tsx`, `components/LiveWorkPanel.tsx` | L2 | `rg 'from "[^"]*net/' src` пусто; пути `queueOnIdle|/sitter/|/keepAlive/|/telemetry/|/batch/` существуют только под `backend/`; элементы net = 0 (каталог исчез), строка удалена | юнит: batchWork, keepAlive, reconnectTranscript, store_commands; Playwright (CI): ws-offline-request, ws-offline-send, ws-queue-race, wake-schedules |
| **L4** actions | 52 файла actions → подкаталоги §2.1 + `liveWork/` 13 + `wakeSnapshot/` 2 + `store.ts`→`actions/index.ts` + `store_livework.test.ts` в `actions/liveWork/`; правка импортов: 36 потребителей `"../store"` в components → `"../actions"`, `useWS.ts`, `components/WsConnector.tsx` (сам переедет в L5) | L3 | `rg 'from "[^"]*/store"' src` пусто; действий напрямую никто не импортирует (цензус §1.3) — `rg 'from "[^"]*actions/' src` показывает только `actions/index`; элементы actions = 6, liveWork = 0, строки удалены | юнит: store_livework; Playwright (CI): queued-message-actions, batch-controls, burst-badge |
| **L5** app | `main.tsx`, `App.tsx`, `WsConnector.tsx`, `useWS.ts`, `env.d.ts`, `index.css` → `app/`; `rsbuild.config.ts` entry → `./src/app/main.tsx` | L4 | `pnpm build` в CI собирает с новым entry; элементы `web/src` = 6 подкаталогов (включая ещё живой `components`), строка `web/src` удалена в этом коммите | весь Playwright-набор (CI) — дымовой критерий загрузки: ui.spec, navigation, auth, chat |
| **L6** chat-тройка + каркас ui | создаёт `ui/`, `ui/shared/` (`ConfirmDialog`, `findSession` из Chat), `ui/Login.tsx`; переносит `Chat/` (13 + выделение `findSession`), `ChatInput/` (17), `ChatToolbar/` (19 + приём `LogsModal`, `SettingsModal`) под `ui/chat/` с нарезкой §2.1; дробление `ChatInput/ChatInput.tsx` 873→≤800 выносом внутренних помощников в соседние файлы композера (экспортируемые имена модуля не меняются) | L5 | каталогов `components/Chat*` нет; элементы Chat = 5, ChatInput = 5, ChatToolbar = 6, строки удалены; `wc -l` нового `ChatInput.tsx` ≤ 800 | Playwright (CI): chat, chat-scroll-magnet, model-chat, burst-badge, burst-partindex, composer-history-filter, cli-models, queued-message-actions |
| **L7** Message | 48 файлов → 7 подкаталогов §2.1, 11 файлов → `ui/shared/`, приём `SubAgentBlock.tsx` + `ForkSessionModal.tsx` из корня, удаление `Message/index.tsx`, правка `Chat/ToolRun.tsx:5` и `Chat/TranscriptList.tsx:3` | L6 | `rg 'from "\.\./Message"'` пусто; экспорт-список множества Message до/после равен минус барель (§3.2); элементы Message = 7, строка удалена | Playwright (CI): message-edit, message-delete, message-retry, message-model, thinking-parts, thinking-card-collapsed-actions, subagent-block-metadata, subagent-state-cleanup, merge-guard |
| **L8a** livework | корень: LiveWorkPanel, LiveWorkTabBar, TodoList, WakeScheduleList, StatusBar, BatchTree + `batchControl/` 8 + `batchControls.ts` + тест; правка `ui/chat/Chat/Chat.tsx` (путь LiveWorkPanel), `ChatToolbar.tsx` (путь StatusBar) | L7 | элементы livework = 7, batchControl = 3, строки удалены | Playwright (CI): live-work-panel, batch-controls, batch-selection, batch-structured-completions, todos, wake-schedules, shutdown-server, statusbar-detail |
| **L8b** models | ModelSelector, CustomRolesEditor, ScopedModelsModal + `scopedModels/` 11 → 3 подкаталога + `modelList/` 7; правка `ModelCluster.tsx` (путь ModelSelector) | L7; параллельна L8c/L8d (файлы не пересекаются) | элементы models = 5, scopedModels = 3, строки удалены | Playwright (CI): model-switch, model.spec, recent-models, role-model-selector, custom-roles, reasoning-effort |
| **L8c** providers | ProvidersModal, PeakHoursEditor, PeakHoursStatus + `providers/` 9 → forms/draft + `peakHours/` 12 → rules/policy + `mcp/` 6 + MCPSettings | L7; параллельна L8b/L8d | элементы providers = 7, peakHours = 0 (каталог исчез), строки удалены; `components` впервые ≤ 7 → его строка удаляется в этом коммите (§5.1) | Playwright (CI): providers, peak-hours-weekly, provider-catalog-effort, settings |
| **L8d** sidebar | Sidebar + `sidebar/` 2; каталог `components` пустеет и удаляется | L7 и после L8c; параллельна L8b | каталога `components` больше нет | Playwright (CI): sidebar-delete, sidebar-delete-race, sidebar-detail, sessions, sessions-list-live-race, session-filtering |
| **L9** tests/helpers | `helpers/` → `index.ts` + `factories/` + `ws/`; правка импортов во всех 64 спеках (`./helpers/<имя>` → `./helpers`); `custom-roles.spec.ts:8` уже поправлен в L1; `e2e/`, `real-backend/` не трогаются | независима: дерево `web/tests` не пересекается с `web/src` — параллельна L1-L4 (в паре, ≤5 агентов) | `rg 'from "\./helpers/' tests` пусто; `pnpm test` (Playwright, CI) зелёный полностью; элементы helpers = 3, строка удалена | весь Playwright-набор |
| **L10** (оркестратор) | синхронное удаление строк храповика (если остались); правка всех упоминаний путей `web/src` в комментариях Go, тестов, спеков и хуков — полный перечень в §4.6 (15 файлов Go, включая `.sql`-источник с последующей перегенерацией sqlc; 6 спеков; 3 файла `.githooks`; README инструментов); финальная сверка таблицы §2.4 (каждое имя ровно один раз); отчёт по расписанию храповика | все | `check_dir_entries.sh` зелёный без web-записей; `rg 'web/src' internal web/tests tools .githooks` не находит устаревших путей; греп-сверка таблицы §2.4 | полный CI |

Параллелизм: L1→L2→L3→L4→L5→L6→L7 строго последовательно (каждая правит импорты чужих будущих файлов — правило §3 п. 15). L9 параллельна любой из L1-L4 (разные деревья). L8a/L8b/L8c/L8d — до 4 агентов параллельно после L7 (файлы не пересекаются; правки чужих импортов касаются только уже завершённых полос). Итого одновременно ≤ 5.

### 5.1. Расписание храповика (элементы; строка удаляется в коммите полосы, где счёт ≤ 7)

| Каталог (строка сегодня) | L1 | L2 | L3 | L4 | L5 | L6 | L7 | L8a | L8b | L8c | L8d | L9 | строка удалена |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `web/src` 35 | 20 | 20 | 11 | 8 | **6** | 7 | 7 | 7 | 7 | 7 | **6** | 6 | L5 |
| `web/src/actions` 52 | 52 | 52 | 52 | **6** | 6 | 6 | 6 | 6 | 6 | 6 | 6 | 6 | L4 |
| `web/src/state` 37 | 37 | **7** | 7 | 7 | 7 | 7 | 7 | 7 | 7 | 7 | 7 | 7 | L2 |
| `web/src/net` 8 | 8 | 8 | **0** | — | — | — | — | — | — | — | — | — | L3 |
| `web/src/liveWork` 13 | 13 | 13 | 13 | **0** | — | — | — | — | — | — | — | — | L4 |
| `web/src/components` 33 | 33 | 33 | 33 | 33 | 32 | 25 | 22 | **14** | **9** | **2** | 0 (каталог удалён) | — | L8c |
| `web/src/components/Chat` 14 | 14 | 14 | 14 | 14 | 14 | **5** | 5 | 5 | 5 | 5 | 5 | 5 | L6 |
| `web/src/components/ChatInput` 18 | 18 | 18 | 18 | 18 | 18 | **5** | 5 | 5 | 5 | 5 | 5 | 5 | L6 |
| `web/src/components/ChatToolbar` 21 | 21 | 21 | 21 | 21 | 21 | **6** | 6 | 6 | 6 | 6 | 6 | 6 | L6 |
| `web/src/components/Message` 48 | 48 | 48 | 48 | 48 | 48 | 48 | **7** | 7 | 7 | 7 | 7 | 7 | L7 |
| `web/src/components/peakHours` 12 | 12 | 12 | 12 | 12 | 12 | 12 | 12 | 12 | 12 | **0** | — | — | L8c |
| `web/src/components/scopedModels` 11 | 11 | 11 | 11 | 11 | 11 | 11 | 11 | 11 | **3** | 3 | 3 | 3 | L8b |
| `web/src/components/providers` 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | **0** | — | — | L8c |
| `web/src/components/batchControl` 8 | 8 | 8 | 8 | 8 | 8 | 8 | 8 | **3** | 3 | 3 | 3 | 3 | L8a |
| `web/tests/helpers` 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | 9 | **3** | L9 |

Вывод чисел для `web/src` (единственный каталог с транзитом; старт 35 = 16 подкаталогов + 19 файлов): L1 — −5 подкаталогов (asyncJob, completion, customRole, attachmentLimit, effort поглощены domain) и −10 файлов (types, mergeMessage, toolFormat, asyncJobMetadata, liveWorkAwaiting + барели completionIndex, effort, asyncJobCompletion, customRoleTypes, attachmentLimit) = 11 + 9 = 20; L2 — перегруппировка внутри state, сверху без изменений = 20; L3 — −6 подкаталогов (net, keepAlive, queueOnIdle, sitter, telemetry, batch) + backend = 6, файлы 9−4 (ws.ts, telemetry.ts, batchWork.ts, reconnectTranscript.ts) = 5, итого 11; L4 — −2 подкаталога (liveWork, wakeSnapshot) = 4, −1 файл (store.ts) = 4, итого 8; L5 — −4 файла (App, main, env.d.ts, useWS) +1 каталог app = 6 каталогов (state, backend, actions, domain, app, components), 0 файлов = 6 ≤ 7 → строка удалена в этом коммите; L6 — +1 каталог ui = 7 (`components` ещё жив: 25); L6–L8c — 7 (`components` тает: 25 → 22 → 14 → 9 → 2 — счёт ≤ 7, строка уже удалена); L8d — `components` удалён = 6; L9 — 6. Число ни разу не поднимается выше записанных 35 и после удаления строки не превышает 7; каталоги, упавшие в 0, перестают быть код-каталогами — строка становится stale и удаляется тем же коммитом (страж предупреждает о stale-записях).

Второй храповик (все файлы каталога вместе с тестами, цель ≤ 21, A1b — В5): после разрезки самый тяжёлый каталог — `ui/chat/ChatToolbar` 23 файла (тестов нет), далее `ui/livework` 16 (15 + тест), `state/runtime` 7, `ui/chat/Message/subagent` 7, `ui/chat/ChatToolbar/modals` 7. 23 > 21: сид A1b возьмёт фактические значения, число только падает; сведение ниже 21 для `ChatToolbar` (например, вынос `modals/` в `ui/modals/`) — решение приёмки (Z), в D3 не входит. `web/tests` остаётся 67 (64 спеки не двигаются) — сид; разбивка спек по фичам — возможное продолжение (§6 п. 3).

### 5.2. Что гоняет агент полосы (без сборки и тестов)

`pnpm typecheck` (tsc --noEmit), `pnpm lint` (oxlint, счётчик предупреждений 101 не растёт), `bash .githooks/check_dir_entries.sh` (read-only), оракульные `rg`/`wc -l`. Полосы НЕ запускают `pnpm build`, `node --test`, `pnpm test` (Playwright) — их гоняет CI на интеграционной ветке волны после каждой полосы (один полный прогон на ветку, план §5a п. 6). Если typecheck ловит пропущенного импортёра — агент находит его `rg 'from "<старый путь>"'` и дополняет правку в том же коммите.

## 6. Риски и открытые вопросы

1. **Правки импортов в чужих файлах — главный операционный риск.** L2 правит ~110 файлов (все потребители state), L3 ~25, L4 ~40. Пропущенный импортёр = красный typecheck уже в CI. Смягчение: перед коммитом агент обязан получить пустой вывод `rg 'from "<каждый старый путь>"'` (старые пути = каталоги-источники §2.4); полосы строго последовательны.
2. **`ui/chat/ChatToolbar` 23 файла против цели A1b 21** (§5.1). Сид возьмёт 23; решение о дополнительном уплотнении — на приёмке. Открытый вопрос оператору: допустим ли сид 23.
3. **`web/tests` 67 файлов** не уменьшается в D3 сознательно: спеки не двигаются, селекторы и конфиг Playwright не затрагиваются. Если приёмка (Z) потребует ≤ 21 — отдельная полоса разбивки спек по фичам с полным прогоном Playwright.
4. **Дробление `ChatInput.tsx` (873 строки)** — единственное изменение структуры файла, не чистый перенос. Оракул: экспорт-список модуля ChatInput не меняется; поведение доказывают Playwright-спеки композера в CI. Если дробление затянет L6 — допустимо отложить: жёсткий лимит 1000 не нарушается и сегодня, цель 800 — рекомендация В5.
5. **Мёртвый код по цензусу:** `tests/helpers/make-permission.ts` никем не импортируется. В D3 не удаляется (не разрезка); вопрос оператору — удалить отдельной микрополосой.
6. **Комментарии Go, тестов, спеков и хуков с путями `web/src`** (полный перечень в §4.6: 15 файлов Go — включая `.sql`-источник и два сгенерированных наследника, 6 спеков, 4 переезжающих файла `src`, 3 файла `.githooks`, README инструментов) правит полоса L10 — web-агенты в `internal/` не заходят.
7. **Не проверено без исполнения, проверяется полосами:** (а) полный список импортёров каждого файла получен адресными grep по ходу — L2-L4 обязаны повторить `rg` по старым путям перед коммитом; (б) запуск `node --test` с подстановкой `git ls-files` на той же node-версии CI (механика запуска `.ts` не меняется, меняется только список файлов); (в) счётчик предупреждений oxlint 101 после переносов — проверяется lint в каждой полосе.
