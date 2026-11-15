# Апгрейд @react-carburetor/core до 0.2.0 (web/)

## Что изменилось в 0.2.0

- Новый входной бандл `@react-carburetor/core/engine` — движок без React: сторы,
  computed, кэши ресурсов, scopes, persist, history. Классы — те же объекты, что и
  в основном входе (dist/esm/Carburetor/index.mjs и dist/esm/Engine/index.mjs
  переэкспортируют одни и те же базовые .mjs файлы). React стал опциональным peer.
- `diagnostics.setFailureHandler(handler)` — обработчик изолированных
  движком ошибок (sources: `delivery`, `transaction`, `settlement`, `update`,
  `computed`, `teardown`). Вызывается в любой сборке, в дополнение к текстовому
  отчёту в development. Хранится один обработчик; `undefined` снимает его;
  бросающий исключение обработчик глотается движком.
- Срез каскада на 1000 раундов теперь репортится в failure handler.
- Дочерние scopes — один класс (вместо нескольких).
- Меньше аллокаций для читателей без resource-cache.
- `createTestClock` управляет свежестью ресурсов.
- lint-мост: статус выхода, отличный от 0/1, теперь ошибка (раньше упавший
  бинарник репортил «проблем нет»).

## Что применяется к web/

- **setFailureHandler** — применяется. Регистрируется один раз в
  `web/src/telemetry/setupTelemetry.ts` через новый
  `web/src/telemetry/carburetorFailures.ts`; источник сохраняется как
  `carburetor:<source>`; текстовый dev-отчёт библиотеки сохраняется (она
  репортит дополнительно — см. Diagnostics.mjs / Diagnostics.d.mts). Юнит-тесты
  `web/src/telemetry/carburetorFailures.test.ts` (3 случая, путь `delivery`): приёмник
  получает полезную нагрузку с `source: carburetor:delivery` и стеком исходной ошибки;
  движок передаёт обработчику ИСХОДНЫЙ объект ошибки и имя источника; не-Error
  приводится к строке. Проверка откатом: убрать вызов
  `diagnostics.setFailureHandler` в `registerCarburetorFailureHandler` — первый случай падает.
- **Срез каскада на 1000 раундов** — риск отсутствует. Проверено: единственный
  watch-колбэк вне тестов — `onActiveSessionChanged` (web/src/useWS.ts:651,
  регистрация на web/src/useWS.ts:770) — только шлёт WS-фреймы и не пишет в
  сторы; computed `asyncJobCompletions.ts`, `myPrompts.ts`,
  `asyncJobStatuses.ts` чистые; ни один subscriber/computed в web/src не пишет
  в стор.
- **ResourceCache / ResourceCarburetor / persist / createTestClock** — 0
  использований в web/src (rg проверен; совпадения — только слово «persist» в
  комментариях). Эти выгоды не применяются.
- **lint:carburetor crash fix** — применяется автоматически с бампом, правок
  кода не требует.
- **/engine**: основной вход и engine реэкспортируют одни и те же файлы модулей,
  поэтому объекты классов идентичны; новый тест импортирует из `./engine`.

## Риски

Минимальные. Диф lockfile затрагивает только пакеты @react-carburetor; react
стал опциональным peer у core (установленный react 19.2.4 не менялся).

## Приёмка

- web/package.json: core/lint ровно "0.2.0"; `pnpm.minimumReleaseAgeExclude`
  = `["@react-carburetor/*"]` (паттерн поддерживается pnpm 10.28.2).
- `pnpm typecheck` — exit 0.
- `pnpm lint` — exit 0: 60 предупреждений в lint:base (бюджет 101, baseline 60 —
  не выросло), 0 в lint:carburetor.
- Оркестратор дополнительно запускает из web/:
  `node --test` с CI-глобом
  `git ls-files "src/*.test.ts" "src/*/*.test.ts" "src/*/*/*.test.ts" "src/*/*/*/*.test.ts"`
  (новый тест покрыт глобом `src/*/*.test.ts`), `pnpm build`, Playwright smoke.
- Что ловит новый тест: регрессию проводки failure-handler — если обработчик
  перестанет регистрироваться или перешлёт обёрнутую/потерянную ошибку, тест
  падает. Revert-проверка: убрать строку `registerCarburetorFailureHandler(...)`
  из setupTelemetry.ts ИЛИ откатить `diagnostics.setFailureHandler` в библиотеке
  — падает ассерт `received.length`.

## Что сознательно не принято и почему

1. **Массовый перевод импортов ~20 файлов в web/src/state на `/engine`** —
   чисто импортная замена с нулевой рантайм-выгодой (классы — те же объекты) и
   выше порога «маленькое изменение». Единственный новый React-free
   потребитель (новый тест) уже импортирует из `/engine`.
2. **createTestClock** — ни один стор в web/src не планирует after()/таймеры
   (`rg '\.after\('` по src: 0); стор-таймеры живут в UI-коде как
   `window.setTimeout` вне контракта часов библиотеки.
3. **ResourceCache / persist** — сегодня не используются; внедрение было бы
   новой функциональностью, вне рамок спринта.
