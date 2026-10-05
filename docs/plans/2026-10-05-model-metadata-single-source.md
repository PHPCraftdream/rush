# Один источник метаданных моделей (context window / max output)

Дата: 2026-10-05. Статус: дизайн, не реализовано. Тип: рефакторинг + одно согласованное
изменение поведения (семейство GLM-5.3 → 1M).

## Проблема и механизм

`rush models list` показывает `zai/glm-5.3*` как «?» при атоме «1M». Причина в источнике:
`rush providers update zai` (`internal/cmd/providers.go:577`) пишет сырой `/models` z.ai
(контекста там нет, `ContextWindow=0`) в глобальный `providers.zai.models`. При загрузке
`config.Models` идут первыми (`load_providers.go:69-95`), `glm-5.3` уже есть с нулём → синтез
(`:314-334`) пропускается («не перезаписывать конфиг»), `mergeProviderModels` (`:643`) ноль
не чинит. Flash/flashx приходят только из live/дампа — их контекст есть только в атомах.

Факты «контекст/макс. вывод» сейчас живут в 5 местах:

| # | Где | Что | Когда применяется |
|---|---|---|---|
| 1 | catwalk (`zai.json` и др.) | значения провайдера | сборка `p.Models` |
| 2 | `discover/codex.go` `codexContextWindow` | пол/дефолт gpt-6*, gpt-5.6* | разбор + `NormalizeCodexModel` на выдаче кэша |
| 3 | `load_providers.go:314-334` | синтез `glm-5.3` 1M/131072/levels | только если модели нет в списке |
| 4 | `cmd/models_atoms.go` `CtxLabel`, `zai53ReasoningLevels` | строки показа | только блок атомов |
| 5 | `cliprovider/provider_specs.go` | контекст local-cli | синтез провайдера `local-cli` |

Уже расходятся: атом `glm4_7` «200k» vs catwalk 204800, `glm4_6v` «204.8k» vs catwalk
131072 — атомы показывают не то, по чему реально считается компакция.

## 1. Форма источника

- **Таблица документированных фактов** — новый файл `internal/discover/model_facts.go`.
  `discover` уже лист (импортирует только catwalk; его импортируют `config` и `cmd`, там же
  живёт `ModelVisible`) — отдельный пакет не нужен, циклов нет.
- Строка: `provider` (точный id: `zai`, `openai-codex`) + матчер id (exact / prefix /
  prefix+набор суффиксов; `-wm` канонизируется как сейчас) + `ContextWindow` +
  `DefaultMaxTokens` + вид правила (п.2) + источник (URL docs / «оператор, не проверено»).
  Первая совпавшая строка побеждает; порядок = порядок веток `codexContextWindow`.
- **Поля v1: только ContextWindow и DefaultMaxTokens** + инвариант `DefaultMaxTokens ≤
  ContextWindow` (сейчас только у Codex). Уровни reasoning в overlay НЕ входят: в рантайме их
  источник — live/`zai-docs` через `LiveEfforts` и wire-маппинг `coordinator_providers.go`;
  перенос раздувает работу на эффорт-подсистему. Исключение — шаблон синтеза `glm-5.3` (п.4)
  несёт levels как данные строки, атомы берут тот же срез.
- **Одна функция наложения** `discover.ApplyModelFacts(provider, models, userSet)` и **одна
  точка вызова**: финальный проход в конце `loadProviders` по `c.Providers` (после known-,
  local-cli-, custom-веток, до валидации peak_hours). Остальные пути (`config.Models`,
  catwalk, live, кэш) только поставляют сырьё.
- `cliprovider.All` остаётся единственным источником для `local-cli` (это спецификация CLI,
  не копия); в таблицу не дублируется.

## 2. Семантика наложения

- Два вида правила (оба уже есть в `codexContextWindow`):
  **floor** — поднять, если меньше или 0, никогда не снижать (gpt-6*-astra/sol/luna 1.05M,
  gpt-5.6-luna/sol/terra 1M, glm-5.3* 1M); **default** — заполнить только при 0 (gpt-5.6
  372k, прочие gpt-6* 1.05M, gpt-5.6-* 372k, остальные openai-codex 272k). Переопределения
  вниз нет: заниженное окно ломает компакцию тише, чем «?».
- **0 = неизвестно**, всегда; ноль из `rush.json` (дамп `providers update`) не считается
  пользовательским значением.
- **Пользователь побеждает**: до цикла known-провайдеров собрать `userSet[provider]` = id с
  `config.Models[id].ContextWindow > 0` (так же для DefaultMaxTokens); overlay их поле не
  трогает, даже ниже пола. Совпадает с текущим Codex (`mergeCodexModels`: configured
  побеждает discovered, нормализация к нему не применялась).
- **Кэш** хранит то, что вернул fetch; наложение — при выдаче конфига, устаревший кэш (TTL 7
  дней) больше не возвращает старые числа. `normalizeCatalogModels`, `NormalizeCodexModel`
  удаляются; `parseCodexModel` отдаёт сырое (0 при отсутствии поля). Формат кэша прежний.

## 3. Атомы

- Поле `CtxLabel` удаляется. Метка = `atomCtx(cfg, atom)`: модель в
  `cfg.Providers[a.Provider].Models` (после overlay) → `humanCtx`; иначе
  `discover.LookupModelFacts` → `humanCtx`; иначе «?». Один форматтер `humanCtx` («1M»,
  «1.05M», «200k») для обоих блоков и JSON (`atomJSON.Ctx` остаётся строкой);
  `formatAtomLine` получает готовую метку.
- `zai53ReasoningLevels` ссылается на срез строки `glm-5.3` таблицы. SYNC WARNING
  load_providers↔атомы удаляется; SYNC WARNING атомы↔`coordinator_providers.go`↔
  `models_efforts.go` (wire-состояния) остаётся — вне зоны.
- Нерасхождение: тест в `cmd` — для каждого атома на фикстурном конфиге метка атома == метка
  той же модели в OTHER MODELS. Тест в `discover` — каждая строка таблицы (`zai`,
  `openai-codex`) даёт ожидаемое окно через `ApplyModelFacts`, таблично.

## 4. Синтез glm-5.3, codexContextWindow, граница поведения

- Синтез → флаг строки «ensure present» с шаблоном (Name, 1M, 131072, CanReason, levels
  low/high/max, default high); вставка в той же финальной точке, только `zai`, только если id
  отсутствует. Блок `:278-334` удаляется.
- `codexContextWindow` и константы → строки таблицы, функция удаляется. Предикат
  `codexGPT6Documented` (gpt-6 + astra/sol/luna) сохраняется — gpt-6.N покрыт уже сейчас.
- **Допустимые наблюдаемые изменения** (всё прочее — провайдер × модель ×
  ContextWindow/DefaultMaxTokens — обязано совпасть):
  1. `zai/glm-5.3`, `glm-5.3-flash`, `glm-5.3-flashx`: 1M вместо 0, max 131072 только где 0
     (flash — docs.z.ai/guides/vlm/glm-5.3-flash; glm-5.3 — копия 5.2; flashx — по запросу
     оператора, «не проверено»);
  2. метки атомов следуют рантайму: `glm4_7` 200k→204k, `glm4_6v` 204.8k→131k —
     **требует подтверждения оператора** (альтернатива — пол в таблице поднимет реальное
     окно компакции; не рекомендуется).
- Доказательство — характеризационный golden-тест (шаг 1), снятый ДО рефакторинга.

## План

1. **Характеризация.** `internal/config/model_metadata_golden_test.go`: `loadProviders` на
   фикстурах — embedded catwalk, записанные ответы Codex и z.ai `/models` (httptest), кэш со
   старыми значениями (272000 у gpt-6), `rush.json` с дампом нулей и с пользовательским
   значением ниже пола; golden = отсортированные `(provider, id, ctx, maxTokens)` +
   `LiveEfforts`. Зелёный на текущем коде, коммитится первым. Revert-check: убрать пол gpt-6 →
   golden падает.
2. **Таблица + overlay, удаление копий в config/discover.** `discover/model_facts.go`
   (+тест), `config/load_providers.go` (userSet, финальный проход, удалить синтез),
   `discover/codex.go` (удалить `codexContextWindow`/`NormalizeCodexModel`),
   `config/model_catalog_cache.go` (удалить `normalizeCatalogModels`),
   `discover/codex_normalize_test.go` → на overlay. Golden меняется ТОЛЬКО строками glm-5.3*;
   diff golden — часть ревью. Revert-check: убрать финальный проход → golden и тест stale-кэша
   падают; убрать userSet → кейс «пользователь ниже пола» падает.
3. **Атомы.** `cmd/models_atoms.go` (удалить `CtxLabel`, levels из таблицы),
   `cmd/models_list.go` (`atomCtx`, JSON), тест паритета атом↔OTHER MODELS. Revert-check:
   вернуть литерал метки у `glm5_3` при фикстуре с другим окном → тест падает.
4. **Сверка потребителей.** `server/handlers_config.go` (`modelInfoWire`),
   `config.ToProvider`, `coordinator_models.go` читают только финализированные
   `c.Providers` — правок не ожидается; `cliprovider/provider_specs.go` не меняется. Тест в
   `server`: пикер отдаёт `contextWindow` 1000000 для `zai/glm-5.3-flash` при нуле в конфиге.
   Удалить устаревшие комментарии о синтезе.

Не-цели: сетевые запросы, новые провайдеры, UI, reasoning в overlay, wire-маппинг эффортов,
GLM у `zhipu`/`zhipu-coding`/openrouter (свои каталоги), `agent/credentials.go` (отдельный
путь метаданных — P3 в бэклог), `providers fetch-models` (намеренно сырой ответ сервера).

## Риски

- **Порядок записи**: последним пишет финальный проход; код, меняющий `ContextWindow` после
  него, ломает инвариант — комментарий у прохода + golden.
- **«0 = неизвестно»** (`coordinator_models.go:832`, `agent_turn_step.go` `cw > 0`): overlay
  только заполняет/поднимает; для GLM-5.3 включится проактивная компакция на 1M (при 0 была
  выключена) — ожидаемо; прочие без изменений.
- **Reload** (`reloadFromDiskLocked`) идёт через `loadProviders` → overlay сохраняется;
  веб add/update custom provider мутирует `c.Providers` вне загрузки, но custom-провайдеры
  таблицей не покрыты.
- **Кэш**: записи старых версий безвредны, сброс не нужен.
- **Веб-пикер**: для `zai` levels идут из `LiveEfforts`; синтезированная `glm-5.3` не должна
  попадать в `LiveEfforts` (как сейчас) — покрыто golden шага 1.
- **Размер**: `load_providers.go` 943 строки — уменьшается; лимит 1000 не задет.
- **Codex CLI vs API**: `cli-codex-sol` 272k (spec CLI) vs `openai-codex/gpt-5.6-sol` 1M —
  разные пути обслуживания, не дубль; проверить отдельно (P3, бэклог).
