# Проверяющий ревью-проход (#1165, #1166)

Статус: дизайн @om утверждён оператором по рекомендациям §10 (2026-10-02). Спека задачи: `.rush/tasks/1165-1166-reviewer-pass.md`, журнал: `.rush/audit/rush_quality.md` (A34/A35). Пометки: **OBSERVED** — видно в данных/коде, **INFERRED** — вывод.

Требование оператора: ревьюер ПРОВЕРЯЕТ (1) качество реализации, (2) соответствие запросу, (3) разумность всего (решения, отчёт, процесс). «Зачёт» без собственной проверки по файлам недопустим.

## 1. Диагноз

### 1.1 Ревьюер ничего не проверяет (#1165)

**OBSERVED, БД.** Все 4 ревью-хода (wtools r1/r2, wguardrail r2/r3) — один шаг без `tool_call`, модель glm-5.3-flash, части reasoning+text+finish, вход 89k–197k токенов, 60–90 с.

**OBSERVED, код.** Набор инструментов ревьюера НЕ пуст, а полный, включая запись: `reviewerCallOptions` ставит только `ModelRole=reviewer` и `DisableSubAgents=true` (`internal/app/app_run_reviewer.go:917-938`); `applyCallDisableSubAgents` убирает только `agent`/`agentic_fetch` (`internal/agent/coordinator_tools.go:286-310`); для роли reviewer `buildToolsAgentConfigForCall` ничего не меняет (`coordinator_tools.go:234-237`); комментарий `app_run_reviewer.go:865-869` прямо говорит «read+write+bash toolset». Инструменты были — модель ими не воспользовалась.

**OBSERVED, reasoning r3.** Модель пишет «Let me reconstruct what was actually verified by me vs claimed by workers: Phase 1 — verified by me directly: Read full files…»; в тексте «я прочитал их целиком и прогнал сам». Ревьюер принял ход оркестратора за свой.

**INFERRED (по силе улик):** (1) ревьюер перепутал себя с оркестратором — в истории все assistant-сообщения «его собственные», промпт (`app_run_reviewer.go:36-43`) не говорит, что писал другой агент; (2) промпт ничего не требует проверять («review everything that happened above»), нет обязательных шагов, формата вердикта, запрета «Зачёт» без проверки; (3) огромный контекст (до 197k) — пересказ дешевле всего.

**OBSERVED, вырожденный финал r3.** Настоящий отчёт оркестратора — сообщение 59b152be; после него пришло уведомление 6bcf3ca2 (`background_job_notice=1`, «Async job … (agent) finished»), ответный ход a7f0dbc6 написал короткий текст с «finalmente» — он и стал `final_text`. Ревьюер оценивал настоящий отчёт из контекста, а `final_text` не проверял. Значит «вырожденный финал» — прежде всего «финал из ответа на уведомление, а не отчёт». Эвристики по тексту (одно латинское слово среди идентификаторов) не ловят; ловит признак происхождения (§4) + оценка моделью по промпту.

### 1.2 Метка effort (#1166)

**OBSERVED.** У всех assistant-сообщений `reasoning_effort='medium'`, включая ревью-ходы; reviewer настроен `max` (`rush models state`).

**Что реально уходит провайдеру — INFERRED по коду, высокая уверенность, на проводе не наблюдалось:** (1) `buildReviewerPassTurn` ставит `ReasoningEffort: reviewerCfg.ReasoningEffort` = max (`app_run_reviewer.go:890-895`); (2) `RunWithOverrides` подставляет effort сессии только если у override он пуст (`coordinator_run.go:718-720`); (3) `applyModelOverrides` → `smartCfg.ReasoningEffort="max"` (`coordinator_models.go:339-361`); (4) `getProviderOptions`, ветка openaicompat→ZAI: `extra_body.thinking.type=enabled`, `extra_body.reasoning_effort="max"` (`coordinator_providers.go:256-307`). Запрос уходит с **max**.

**Откуда метка (OBSERVED по коду).** `agent_turn_step.go:215` пишет `ts.currentSession.SmartModelReasoningEffort` (из строки сессии, `agent_turn.go:343`); override ревьюера в сессию не пишется (`app_run_reviewer.go:905`). Гипотеза верна по сути; ссылка на `coordinator_models.go:134` неточна (это слияние override из сессии).

**Дополнительно (INFERRED, P2).** `agent_turn.go:504` кладёт в контекст CLI-провайдеров тот же effort сессии (`cliprovider.ReasoningEffortContextKey`, читает `provider_stream.go:65`) — если reviewer на CLI-провайдере, на провод уйдёт effort смарта (не только неверная метка). Тот же шаблон: `agent_compaction.go:373`, `:569`.

## 2. Инструменты ревьюера: read-only

Признак — `ModelRole == reviewer` (уже сохраняется в `CallOptionsSpec`, `call_data_conversion.go:96-97,136-137`, значит durable-очередь сохранит read-only, `CallOptionsSpecVersion` НЕ поднимаем).

Новый файл `internal/agent/coordinator_tools_reviewer.go`:

```go
// reviewerReadOnlyToolNames: the only tools a reviewer-role top-level call may hold.
var reviewerReadOnlyToolNames = []string{
    tools.ViewToolName, tools.GrepToolName, tools.GlobToolName, tools.LSToolName,
    tools.GitReadToolName, tools.ReadDelegationTranscriptToolName,
    tools.FSReadToolName, tools.FSListToolName, tools.FSFindToolName, tools.FSGrepToolName,
}
const reviewerToolCallBudget = 12

func applyCallReviewerReadOnly(ctx context.Context, agent config.Agent, isSubAgent bool) config.Agent
// Intersects agent.AllowedTools with reviewerReadOnlyToolNames, AllowedMCP = map[string][]string{} (no MCP).
// No-op unless callOptionsFrom(ctx).ModelRole == config.SelectedModelTypeReviewer && !isSubAgent.

func withReviewerToolBudget(ctx context.Context, list []fantasy.AgentTool) []fantasy.AgentTool
// Same gate; wraps each tool in budgetedTool sharing one *atomic.Int32; call N > reviewerToolCallBudget returns
// fantasy.NewTextErrorResponse("review tool budget exhausted (12 calls): write your verdict now") without running.
```

`coordinator_tools.go`, `buildTools`: (1) после `applyCallFolderScope` (:571), до floor (:597): `agent = applyCallReviewerReadOnly(ctx, agent, isSubAgent)` — именно после FolderScope, иначе fs_write вернётся через grants; (2) сразу после `wrapAsyncTools` (:801): `filteredTools = withReviewerToolBudget(ctx, filteredTools)`.

Без bash и run_command: надёжной read-only обёртки нет (`coordinator_tools.go:202-207`), `go test` пишет кэши и два параллельных запуска дают errno 1455. Коды выхода тестов ревьюер получает из блока улик (§4). `ask_question` исчезает из набора — ревьюер не может уйти в `awaiting_answer` механически. Переписать doc-комментарий `app_run_reviewer.go:865-869`.

Тестирование — на реальных инструментах (T1–T5 в §8).

## 3. Новый `reviewerPassPrompt`

Константы переносятся в новый файл `internal/app/app_run_reviewer_prompt.go` (освобождает ~13 строк в `app_run_reviewer.go`). Первая строка остаётся ДОСЛОВНО прежней (по подстроке «independent reviewer» её ищут тестовые harness `app_run_loop_round4_test.go:104,110`, аудит и счётчик tool_call ревьюера):

```go
const reviewerPassMarker = "You are now acting as the independent reviewer"
```

Полный текст (английский — модели надёжнее следуют; язык ответа задаётся внутри):

```text
You are now acting as the independent reviewer for this session.

ROLE SWITCH. The orchestrator agent has FINISHED its work: its final report
is the last assistant message above, and its run is over. You are NOT that
agent. From this message on you are a different agent with a different job:
the independent reviewer who audits what the orchestrator did and claimed.
You received its conversation only as evidence to examine; do not continue
its task, do not fix anything, do not speak as "I" about its actions.

WHO WROTE WHAT. Every assistant message above was written by the orchestrator
and by the workers it delegated to - not by you. When those
messages say "I read", "I ran", "verified", "green", "closed", they are CLAIMS
to check, not facts you observed. Until you call a tool in this turn, you have
verified nothing. Never write "I ran" or "I verified" about anything you did
not do in this turn.

YOUR TOOLS are read-only: view, grep, glob, ls, git_read (status, diff, log,
show, blame) and read_delegation_transcript. You cannot edit files, run shell
commands or tests, or ask questions - do not try. You have a budget of about
12 tool calls: issue independent reads in parallel in one step, and use
offset/limit and path filters instead of reading whole large files.

EVIDENCE. The <review_evidence> block at the end of this message was computed
by rush itself from the git working tree and the session database, not by the
orchestrator. Where it disagrees with the transcript, trust the evidence and
say so.

MANDATORY CHECKS - do all of them, in this order:
1. Request. State in 1-3 lines what was asked (original_request in the
   evidence; if it points to a spec or task file, view that file). Judge
   everything below against the request, not against the orchestrator's own
   summary of it.
2. Changes. Use the evidence's changed-file list and git_read diff on the
   files that matter. Flag every change the request did not ask for: unrelated
   files, deleted or weakened tests, config or version edits, generated files.
3. Claims. Take at least 3 concrete claims from the orchestrator's final
   report (all of them if there are fewer) - "function X does Y", "test Z
   fails without the fix", "file is N lines", "item M is closed" - and check
   each against the files with view/grep. Mark each one: confirmed (file:line),
   refuted (file:line), or unverifiable with these tools.
4. Tests. Read each new or changed test and judge whether it is vacuous: would
   it still pass if the change were reverted? Does it assert the behaviour, or
   only that code runs? Does it mock the very thing under test, skip, or assert
   constants? Take what actually ran, and its exit code, from the evidence's
   command results: code that changed with no passing test/build run after it
   is a finding.
5. Stubs. grep the changed files for TODO, FIXME, XXX, "not implemented",
   placeholder panics, empty bodies and commented-out code added by this run.
6. Final answer. Judge the orchestrator's final answer itself (final_text in
   the evidence): is it coherent, in the request's language, and an actual
   report of the work - or is it empty, truncated, repetitive, garbled or
   mixed-language, or a short reply to a background notice that buried the
   real report? Address every flag in final_text_checks explicitly. A
   degenerate, empty or incoherent final answer is a finding on its own, even
   when the work itself is good.
7. Process. Note waiting loops (sleep/echo/polling), repeated failing
   attempts, open todos from the evidence, and work the orchestrator said it
   would do but did not do.

VERDICT RULES.
- FAIL if any of: the request is not fulfilled; a claim is refuted; a P0/P1
  defect; the last relevant test/build run failed, or code changed with no
  run after it; out-of-scope changes that matter; stubs left in; the final
  answer is degenerate or is not a report; or you could not verify anything
  yourself.
- PASS_WITH_NOTES if the work meets the request but you found P2/P3 issues or
  some claims stayed unverifiable.
- PASS only if you confirmed at least 3 claims yourself with tools in this
  turn and found nothing listed above.
A PASS or PASS_WITH_NOTES without your own tool checks in this turn is
forbidden.

OUTPUT. Write in the language of the original request. The first line must be
exactly one of:
VERDICT: PASS
VERDICT: PASS_WITH_NOTES
VERDICT: FAIL
For a Russian request append the label after the token: "Зачёт",
"Зачёт с замечаниями" or "Отказ". Then these sections:
## Request vs result - what was asked, what was done, what is missing.
## Checked myself - each check: tool, file:line, result.
## Only from the orchestrator's words - claims you did not or could not check.
## Findings - each: severity P0-P3, file:line, what is wrong, why it matters.
## Final answer and process - checks 6 and 7.
This is the session's final message: do not ask questions, do not offer
further work, do not address the orchestrator. Conclude.
```

## 4. Блок улик (детерминированный, без доверия к оркестратору)

Вставляется в user-сообщение ревью-хода сразу после промпта, обёрнутым в `<review_evidence trust="computed by rush, not by the orchestrator"> … </review_evidence>` (не в system: system общий для сессии; user-сообщение сохраняется в БД — улики остаются в журнале для аудита).

Новый файл `internal/app/app_run_reviewer_evidence.go`:

```go
type gitSnapshot struct{ OK bool; Head, Branch string; Status map[string]string /*path->XY*/; Numstat map[string]string /*path->"a\td"*/ }
type reviewBasis struct{ Start time.Time; Prompt, WorkingDir string; Before gitSnapshot }

// nil unless shouldRunReviewerPass(role, cfg): runs without a reviewer stay byte-identical.
func (app *App) captureReviewBasis(ctx context.Context, role config.SelectedModelType, prompt string, start time.Time) *reviewBasis
func readGitSnapshot(ctx context.Context, dir string) gitSnapshot
func (app *App) reviewerTurnPrompt(ctx context.Context, sessionID string, basis *reviewBasis) string // reviewerPassPrompt + "\n\n" + block; basis==nil -> prompt alone
func buildReviewEvidence(basis *reviewBasis, after gitSnapshot, msgs []message.Message, todos []session.Todo) string
func commandResults(msgs []message.Message, since int64) []commandResult // {Cmd string; Exit int; Async bool; Known bool}
func finalTextFlags(text string) []string
func finalTextSource(msgs []message.Message) string // "answer_turn" | "reaction_to_background_notice"
const reviewEvidenceMaxChars = 6000
```

**git** — `platform.Command` (как `git_read.go:218`), `cmd.Dir = WorkingDir`, timeout 10 с, env `GIT_OPTIONAL_LOCKS=0`: `git -c core.quotepath=off status --porcelain=v1 --branch`; `git -c core.quotepath=off diff --numstat HEAD` (fallback без `HEAD` для пустого репо); `git rev-parse --short HEAD`. Снимок берётся дважды: до первого хода (`basis.Before`) и перед ревью. `changed_during_run` = пути, у которых изменилась строка status/numstat, плюс новые `??`. Отдельно `dirty_before_run` — «не приписывать этому прогону» (защита общего дерева, пример `web/dist/.gitkeep`). Не git-каталог → `git: not a repository`.

**original_request:** `basis.Prompt`, первые 1500 символов + «(truncated)».

**Команды тестов/сборки** (только корневая сессия, сообщения с `CreatedAt >= basis.Start.Unix()`): tool_call `bash` (`input.command`) или `run_command` (`program + args`), отфильтрованные по подстрокам `go test`, `go build`, `go vet`, `gofmt`, `golangci-lint`, `npm`, `pnpm`, `pytest`, `cargo`, `make`. Код выхода: синхронный результат — `is_error` + `Exit code (\d+)` (`bash.go:424`), иначе 0; async (metadata `"async":true`) — user-сообщение с `BackgroundJobNotice` и тем же job id, из него `finished: exit (-?\d+)` (`coordinator_background.go:112`), `timed out`, `was stopped`, `failed`; иначе `exit=unknown (still running?)`. Последние 8, команда до 160 символов. Ни одной → `no_test_or_build_command_seen: true`.

**open_todos:** `sess.Todos` со `Status != completed`, до 10 по 120 символов. **tool_calls_this_run:** счёт tool_call по именам. **final_text_checks:** `chars=N`, `flags=[…]|none`, `source=answer_turn|reaction_to_background_notice` (источник: последняя не-ревьюерская user-строка перед последним завершённым assistant имеет `BackgroundJobNotice=true`, поле `message/content.go:174`; в r3 у 6bcf3ca2 = 1). Плюс строка `note: worker sub-sessions are not scanned; check worker claims in files or with read_delegation_transcript`.

**`finalTextFlags` — чистая функция без ML**, порядок флагов фиксирован: `empty` (пусто после TrimSpace); `very_short` (< 80 рун); `garbage_chars` (U+FFFD или управляющие, кроме `\n\r\t`); `unexpected_script` (букв ≥ 40 и доля букв вне `unicode.Latin`/`unicode.Cyrillic` > 5 %); `repeated_lines` (непустая строка ≥ 20 символов встречается ≥ 3 раз); `repeated_phrases` (шингл из 6 слов ≥ 3 раз); `possibly_truncated` (последний символ не из `.!?…)»"'`*|:>]` и текст не кончается на тройной бэктик).

**Размер:** каждая секция режется отдельно (request 1500, changed 40 строк, dirty_before 20, commands 8, todos 10); итог жёстко ≤ `reviewEvidenceMaxChars` с суффиксом `\n(evidence truncated)\n</review_evidence>`.

## 5. Вердикт

Поле `review` остаётся. Читатели (`.Review`, `"review"`): запись `app_run.go:622`, `app_run_async.go:464`; определение `app_run_result.go:37`; help `cmd/run.go:65-76,132`; sdk и web поле не читают — добавление поля конверт не ломает.

Новый файл `internal/app/app_run_reviewer_outcome.go`:

```go
// app_run_result.go, рядом с Review:
ReviewVerdict string `json:"review_verdict,omitempty"` // pass | pass_with_notes | fail | unverified | unparsed | error

func parseReviewVerdict(text string) string // regexp `(?m)^\W*VERDICT:\s*(PASS_WITH_NOTES|PASS|FAIL)\b`, first match; "" if none
func reviewerToolCalls(msgs []message.Message) int // tool_call parts in assistant rows after the LAST user row containing reviewerPassMarker
func (app *App) attachReview(ctx context.Context, final *RunResult, sessionID, reviewText string, stderr io.Writer)
func reviewFailureKeepsPrimary(runCtx context.Context, err error) bool
```

`attachReview`: `final.Review = reviewText`; `verdict = parseReviewVerdict(text)`: `""` → `unparsed` + warning `reviewer verdict line missing`; pass/pass_with_notes при `reviewerToolCalls == 0` → `unverified` + warning `reviewer passed the run without any read-tool check; treat the review as unverified` (кодовая гарантия требования «Зачёт без проверки недопустим»); `fail` → warning `reviewer verdict: FAIL - see review`. Для fail/unverified/unparsed — одна строка stderr: `rush run: reviewer verdict: <V> (see the review field)`. **Код выхода не меняется** (вердикт — мнение второй модели; оркестраторы ветвятся по exit code). Новых флагов и режимов нет.

## 6. Исправление метки effort (#1166)

Новый файл `internal/agent/turn_effort.go`:

```go
// turnSmartReasoningEffort is the effort of THIS call's request: a per-call model
// override (reviewer pass, non-persisted RunWithOverrides) wins over the session row.
func turnSmartReasoningEffort(ctx context.Context, sess session.Session) string {
    if smart, _ := modelOverridesFrom(ctx); smart != nil && smart.ReasoningEffort != "" {
        return smart.ReasoningEffort
    }
    return sess.SmartModelReasoningEffort
}
```

Применить: `agent_turn_step.go:215` → `turnSmartReasoningEffort(callContext, ts.currentSession)`; `agent_turn.go:504` → `turnSmartReasoningEffort(ctx, currentSession)` (чинит и реальный effort на проводе для CLI-провайдера ревьюера); опционально `agent_compaction.go:373`, `:569`. Не `smartModel.ModelCfg.ReasoningEffort`: для запусков без override (web) это сменило бы effort на проводе у CLI-провайдеров (переключатель effort в web пишет effort в сессию, `session_update.go:263-270`). Остаток: ход, переигранный durable pump, теряет override из ctx и получит метку сессии — P3 в бэклог.

## 7. Стоимость и время

- Вход 89k–197k на шаг; бюджет 12 вызовов + параллельные чтения → ~4–7 шагов (×4–7 от сегодняшнего входа без учёта кэша). При ~197k возможна авто-суммаризация (`agent_turn_step.go:760-783`) — смягчает бюджет и заранее собранные улики.
- Таймаут: ревью под ctx прогона (`--timeout`, 6 ч); наследуются `IdleTimeout`, `MaxCost`, `MaxTokens`, `TimeoutHardCap` (`app_run_reviewer.go:921-929`). Свой потолок `reviewerPassTimeout = 15 * time.Minute` ВНУТРИ замыкания `reviewRunFn` (`app_run_reviewer.go:907-909`): `ctx, cancel := context.WithTimeout(ctx, reviewerPassTimeout); defer cancel()`.
- `reviewerPassBlocked` не меняется.
- Ошибка ревьюера не роняет прогон: сейчас упавший ревью заменяет результат (`app_run.go:625-627`, `app_run_async.go:469-471`). Новое: если `reviewFailureKeepsPrimary(runCtx, err)`, остаётся основной результат, `ReviewVerdict="error"`, warning `reviewer pass failed: <err>`, строка stderr, exit по основному ходу. `reviewFailureKeepsPrimary` = `err != nil && runCtx.Err() == nil && !errors.Is(err, ErrRunQueued) && !errors.Is(err, agent.ErrSessionBusy) && !turnRefusedByOwner(err)`. Отмена прогона (Ctrl-C/`--timeout`) и R2-3 fail-fast (`TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim`) — как есть. В `app_run.go` до `resetForReviewerPass` сохранить `primaryFinalText := loop.finalText` и при сохранении основного результата вернуть, иначе terse-режим напечатает текст ревьюера.

## 8. План реализации

Размеры (wc -l): `app_run_reviewer.go` 939 (запас 61), `app_run_async.go` 966 (запас 34), `coordinator_models.go` 957, `agent_turn.go` 877, `agent_turn_step.go` 836, `coordinator_tools.go` 823, `app_run.go` 648. Нельзя раздувать: `app_run_async.go` — не более +8 строк, всё новое в отдельные файлы; `app_run_reviewer.go` — только перенос промпта (−13) и правка doc; `coordinator_models.go` не трогать.

Шаги:
1. **agent: read-only + бюджет.** `coordinator_tools_reviewer.go` (~90 строк) + две строки в `buildTools` (§2).
2. **agent: effort.** `turn_effort.go` (~20 строк); замена `agent_turn_step.go:215`, `agent_turn.go:504` (опц. compaction).
3. **app: промпт.** `app_run_reviewer_prompt.go`: `reviewerPassMarker` + `reviewerPassPrompt` (§3); удалить константу из `app_run_reviewer.go:31-43`; переписать doc :865-869.
4. **app: улики.** `app_run_reviewer_evidence.go` (~300 строк, §4). В `RunRequest` (`app_run_request.go`) поле `reviewBasis *reviewBasis`. В `app_run.go` перед `loop := &executeRunLoop{` (≈:513): `basis := req.reviewBasis; if basis == nil && !req.reviewerTurn && !req.drainTurn && !req.deferReviewer { basis = app.captureReviewBasis(ctx, overrides.ModelRole, prompt, runStart) }`; :551 и :617: `loop.runTurnPhase(app.reviewerTurnPrompt(reviewCtx, sess.ID, basis), reviewRunFn)`. `app_run_async.go`: поле `reviewBasis` в `cliLoop` (:196), инициализация в конструкторе (:303-307) `app.captureReviewBasis(ctx, overrides.ModelRole, prompt, started)`, передача `reviewBasis: l.reviewBasis` в `runReviewerTurn` (:399-408).
5. **app: вердикт и ошибки.** `app_run_reviewer_outcome.go` (~120 строк) + поле `ReviewVerdict`. `app_run.go:621-627`: успех → `app.attachReview(...)`; ошибка с `reviewFailureKeepsPrimary` → основной результат + `ReviewVerdict="error"` + warning. `app_run_async.go:463-465` и ветка ошибки в `default:` (:469) — через тот же helper (≤ +6 строк).
6. **app: таймаут ревью** в `reviewRunFn` (§7).
7. **Документация:** `cmd/run.go:65-76`, `:128-134` — read-only инструменты, `review_verdict` и значения, «ошибка ревьюера не меняет итог прогона»; CHANGELOG.md (строки по-русски, вставкой); правило README задач про help и тесты help.
8. **Существующие тесты, сценарий которых станет недостижим** (ревьюер больше не запускает async bash): `app_run_loop_round4_test.go`: `TestRunNonInteractive_ReviewerAsyncBashIsWaitedOnAndAnswered` (:153), `…TerseAnswerIsTheReaction` (:176), `…CtrlCWhileWaitingOnReviewerJob_KeepsReviewerAnswer` (:213), `TestCLILoop_ReviewerLeftStuckOrDeferredDebt` (:273); `app_run_loop_round5_test.go`: `TestRunLoop_ReviewerJobHonorsNoSupervision` (:158), `…SupervisionInterval` (:186), `TestCLILoop_RefusalStreakDoesNotOutliveClosedScope` (:225); `app_run_deadline_wait_test.go:31`. Решение оператора: удалить и заменить T4/T5 (код цикла `evCloseAgain`, Drain на настройках ревьюера оставить как защитный). Тесты на «упавший ревью заменяет результат» — найти grep'ом `Review`/`review` в `app_run_reviewer_pass_test.go` и обновить под новое правило. `TestReviewerCallOptionsCarryEveryPrimaryField` не меняется.

Тесты (у каждого revert-check):

| # | Тест | Что ловит | Revert-check |
|---|---|---|---|
| T1 | `TestBuildTools_ReviewerRoleIsReadOnly` (agent/coordinator_tools_reviewer_test.go, настоящий `c.buildTools`) | В наборе view, grep, glob, ls, git_read; нет bash, run_command, edit, multiedit, write, download, fetch, todos, ask_question, agent, job_kill, MCP; с FolderScope, разрешающим create: fs_read есть, fs_write нет | убрать вызов `applyCallReviewerReadOnly` |
| T2 | `TestBuildTools_SmartRoleKeepsBash` | контроль: при роли smart bash в наборе | сделать фильтр безусловным |
| T3 | `TestReviewerToolBudget_RefusesAfterLimit` (настоящий `tools.NewViewTool` на temp-каталоге) | вызовы 1..N отдают файл, N+1 — «budget exhausted» | убрать проверку счётчика |
| T4 | `TestReviewerPass_WireOffersOnlyReadTools` (app/app_run_reviewer_readonly_test.go, harness `wireModelAndTools`) | tools в запросе ревьюера без bash/write/edit, git_read и view есть; в запросе смарта bash есть | убрать фильтр |
| T5 | `TestReviewerPass_ReadToolRunsWriteDoesNot` | ревьюер вызывает `view` на файл с маркером, затем `write`, затем «VERDICT: PASS»: маркер в tool_result в БД, файл write не создан, `review_verdict=="pass"` | убрать фильтр → файл создан |
| T6 | `TestReviewerPass_PromptCarriesEvidence` (skip без git) | temp git-репо; первый ход меняет файл; в последнем user ревьюера: маркер, `<review_evidence`, исходный prompt, путь в changed_during_run; грязный до прогона файл — в dirty_before_run | `captureReviewBasis` вернёт nil |
| T7 | `TestFinalTextFlags` (таблица) | empty; very_short; garbage_chars (U+FFFD); unexpected_script (CJK в русском); repeated_lines; repeated_phrases; possibly_truncated; контроль: нормальный русский отчёт с `agent_turn_step.go`, `M6/M7` → без флагов | любую ветку заменить на `return nil` |
| T8 | `TestFinalTextSource` | перед финалом user с `BackgroundJobNotice=true` → `reaction_to_background_notice`; обычный user → `answer_turn` (фикстура повторяет r3) | игнорировать `BackgroundJobNotice` |
| T9 | `TestCommandResultsFromTranscript` | sync bash `go test ./x` is_error + «Exit code 1» → exit=1; async bash + уведомление «…finished: exit 0…» → exit=0, async; `ls` исключён; `run_command go build ./...` включён; async без уведомления → unknown | сломать regex |
| T10 | `TestParseReviewVerdict`, `TestAttachReview_PassWithoutToolsIsUnverified` | PASS_WITH_NOTES не читается как PASS; `**VERDICT: FAIL**` распознаётся; нет строки → unparsed; PASS при 0 tool_call → unverified + warning; при 1 → pass | убрать понижение до unverified |
| T11 | `TestReviewEvidence_SizeCapped` | огромные входы → `len ≤ reviewEvidenceMaxChars`, хвост «(evidence truncated)» | убрать обрезку |
| T12 | `TestReviewerPass_RecordsReviewerEffort` (app) | reviewer effort «max», смарт «medium» (catwalk `CanReason`, уровни low/medium/high/max): тело запроса ревьюера содержит `"reasoning_effort":"max"`; assistant после маркера `ReasoningEffort=="max"`, строки основного хода — `"medium"` | вернуть `ts.currentSession.SmartModelReasoningEffort` в :215 |
| T12b | `TestTurnSmartReasoningEffort` (agent/turn_effort_test.go) | override max → max; без override → effort сессии; override с пустым effort → effort сессии | вернуть всегда effort сессии |
| T13 | `TestReviewerPass_FailureKeepsPrimaryAnswer` | ревьюер получает HTTP 400 (не 5xx — без ретраев) → прогон без ошибки, `final_text` основной, `review_verdict=="error"`, warning есть; существующий FailFast-тест зелёный | вернуть замену результата |

Оценка: код ~600 строк (4 новых файла + ~40 строк правок), тесты ~700 строк, 8 старых тестов удалить/переписать. Запуск только `internal/agent` и `internal/app` по `-run` и `capm 2g -p 1`.

## 9. Риски и «не делаем»

Риски: явный `rush run --role reviewer` тоже станет read-only (следствие признака по роли); allowlist `permissions.run.restrict` может запретить git_read/view (`wrapToolsWithRestrictedRun`, `coordinator_tools.go:807`) — задокументировать; страж прогресса #1149 может счесть повторные чтения ревьюера «re-read» (T5 покажет); блок улик сканирует только корневую сессию; фильтр «с начала прогона» по `CreatedAt` с точностью до секунды зацепит максимум одно сообщение прошлого прогона; `git_read status` без `GIT_OPTIONAL_LOCKS=0` может писать `.git/index` в общем дереве (P3); ревьюер может игнорировать требование проверок — тогда `unverified`.

Не делаем: ML/словари для вырожденного текста; запуск тестов ревьюером; суб-агентов ревьюера; новые CLI-флаги/режимы; смену exit code по вердикту; новые поля `CallOptions` и подъём `CallOptionsSpecVersion`; правку `reviewerPassBlocked`; починку выбора `final_text` (ответ на уведомление вытеснил отчёт в r3 — отдельный P2 в бэклог); слот reviewer сессии (`ReviewerModelID`/`ReviewerModelReasoningEffort`, `session_update.go:227-260`) — P3 в бэклог.

## 10. Решения (приняты по рекомендациям @om)

1. Read-only и для явного `--role reviewer` — да. 2. Восемь тестов «ревьюер запускает async bash» удалить, заменить T4/T5. 3. Exit code при FAIL не менять (только `review_verdict`, warning, stderr). 4. Не повторять ревью при отсутствии проверок — `unverified`. 5. Ошибка ревьюера сохраняет основной результат (кроме отмены/busy/queued). 6. Бюджет 12 вызовов, таймаут ревью 15 мин, блок улик 6000 символов — пересмотреть после 2–3 реальных прогонов. 7. `run_command` с allowlist ревьюеру не даём в этой итерации.
