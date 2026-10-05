# #1161 — сжатие rush.db: `rush sessions compact`

Улика: `rush.db` 408 МБ, freelist 384 МБ, `auto_vacuum=0`, живых данных ~17 МБ.
После `sessions gc/purge` страницы уходят во freelist, файл не уменьшается.
Ограничения оператора: никакого молчаливого VACUUM, никакого VACUUM на старте.

## 1. Механизм — решение

**(а) Явная подкоманда `rush sessions compact`** + подсказка в выводе `gc`/`purge`.
- Против (б) `gc/purge --vacuum`: у VACUUM свои предусловия (эксклюзивность, место, `--force`),
  флаги и вывод. На двух командах это дублируется, и «удалить строки» смешивается
  с «перестроить файл». Отдельная команда нужна и после ручного `sessions delete`.
- Против (в) `auto_vacuum=INCREMENTAL`: переключение режима само требует полного VACUUM.
  Как миграция это VACUUM на старте у всех (запрещено). Режим постоянный, усиливает
  фрагментацию и пишет pointer-map на каждом коммите. Даёт только автоматизм, которого
  оператор не хочет.
- Место — группа `sessions`, рядом с `gc`/`purge`; новая группа верхнего уровня не нужна.
- Подсказка: `gc`/`purge` (кроме `--dry-run`/`--json`) после удаления пишут в stderr одну строку,
  если freelist ≥ 64 МБ и ≥ 50% файла: `rush.db: 384 MB of 408 MB is free pages; run
  'rush sessions compact' to reclaim`. Только чтение `page_count/freelist_count`.

## 2. Безопасность

SQLite сама не даст испортить файл: VACUUM атомарен и идёт под write-lock. Риск — живые
процессы: их писатели ждут `busy_timeout=30000`, потом падают с `SQLITE_BUSY`.
Поэтому шлюз «никто не работает», при нарушении — отказ. Все примитивы уже есть:
1. `db.Connect(dataDir)` — обычное открытие и миграции под `migrate.lock`, после чего lock снят.
2. `filelock.TryAcquireFileLock(<dataDir>/migrate.lock)` держится до конца. `connect()` берёт его
   всегда, поэтому новые `rush run`/web на этой БД ждут на старте и не пишут посреди VACUUM.
   Занят → отказ. Брать строго ПОСЛЕ шага 1, иначе свой `connect` заблокируется.
3. Хосты: каждый файл в `session.HostsDir` → `session.ProbeHostLockShared` (чужой lock не
   захватывается). `Alive`/`Unknown` → отказ.
4. Сессии: тот же обход lock'ов, что у `sessions locks` (`InspectSessionLock`). Есть
   live-holder → отказ со списком id+PID.
5. `PRAGMA wal_checkpoint(TRUNCATE)`; `busy=1` (у кого-то открыта транзакция) → отказ.
   Это последняя проверка на уровне SQLite.
6. VACUUM, затем снова `wal_checkpoint(TRUNCATE)`: в WAL-режиме новые страницы ложатся в WAL,
   а файл БД усекается только при чекпойнте.

Отказ: `refusing to compact: N live rush process(es) use this database (<id/pid…>); stop them
or re-run with --force`. `--force` пропускает только шаги 3–4. Шаги 2 и 5 не обходятся никогда:
при них VACUUM гарантированно столкнётся с чужой записью.
Общая БД (#1143): `dataDir` берётся от резолвера (для linked worktree это `<main>/.rush`), и
шлюзы смотрят его hosts/locks — процессы всех worktree видны. Первая строка вывода — путь к БД.
Windows: in-place VACUUM файл не переименовывает и не удаляет, поэтому «файл занят» не мешает.
Усечение при чекпойнте проходит и при чужих открытых handle (mmap выключен).

## 3. Отказоустойчивость

- **In-place `VACUUM`, не `VACUUM INTO` + замена.** Замена файла под чужими handle ломает
  согласованность WAL/shm (класс `SQLITE_NOTADB` из комментария в `Connect`), а на Windows
  rename над открытым файлом не проходит. In-place транзакционен: при падении посередине
  следующий открывший восстановится по WAL, БД будет в старом или новом виде. Своё
  восстановление не нужно.
- Место заранее: `live = (page_count − freelist_count) × page_size`, нужно
  `2×live + WAL + 64 МБ` (temp-копия + WAL). На время VACUUM `temp_store=FILE` (по умолчанию
  `MEMORY`), чтобы не упереться в потолок RAM. Проверяются тома `dataDir` и temp-каталога.
  Не хватает → отказ с цифрами. Свободное место через `x/sys` (уже в go.mod):
  `diskfree_windows.go` (`GetDiskFreeSpaceEx`) / `diskfree_unix.go` (`Statfs`).
- Ошибка VACUUM (`SQLITE_FULL`/`BUSY`) → ненулевой код и текст; файл не тронут. Lock'и снимаются
  и `temp_store` возвращается через `defer`.

## 4. UX — `rush sessions compact [--dry-run] [--force] [--json]`

- Short: `Reclaim free space in rush.db (SQLite VACUUM)`. Long: удаления не уменьшают файл;
  на время работы БД заблокирована, новые процессы ждут на старте; нужен запас места; при
  живых процессах отказ; что пропускает `--force`; автоматически ничего не запускается.
  Example: `--dry-run`, без флагов, `--json`.
- Вывод: `database: <path>` / `before: 408.0 MB (free pages 384.0 MB, 94%)` /
  `after: 17.2 MB (free pages 0 B)` / `reclaimed 390.8 MB in 2.1s`.
- freelist < 1 МБ → `nothing to reclaim`, код 0, VACUUM не запускается.
- `--dry-run`: размер, freelist, оценка «after ≈ live», нужное/свободное место, результаты
  шлюзов 3–5 (только чтение, без `migrate.lock`).
- `--json`: один объект `{path, dry_run, before_bytes, before_free_bytes, after_bytes,
  after_free_bytes, reclaimed_bytes, duration_ms}` — так же, как у `gc --json`.
- Help: добавить `compact` в перечень `rush sessions` в Long корня (`root.go`).

## 5. План

**Шаг 1 — `internal/db/compact.go` (+ `diskfree_*.go`).** `Stats(conn)` →
{PageSize, PageCount, FreelistCount, WALBytes}; `Compact(ctx, conn)`: checkpoint с проверкой
busy → `temp_store=FILE` → VACUUM → checkpoint → вернуть `temp_store` → новые Stats.
Тесты (временная БД): вставить, удалить → freelist > 0 → Compact → freelist = 0, файл
меньше, число строк то же, `integrity_check` = ok; reader с открытой транзакцией → ошибка busy,
файл не изменён.
Revert-check: без финального checkpoint → «файл меньше» красный; без проверки busy →
тест с reader красный.

**Шаг 2 — `internal/cmd/sessions_compact.go` (новый) + регистрация в `sessions.go`.**
Шлюзы 2–5, место, вывод/JSON/dry-run, `--force`.
Тесты: (a) раздутая БД → размер ≈ live, поля JSON согласованы; (b) живой host-lock в hosts/ →
отказ, файл тот же; с `--force` → проходит; (c) живой session lock → отказ с id; (d) тест
держит `migrate.lock` → отказ даже с `--force`; (e) `--dry-run` → размер и mtime не изменились;
(f) мало места (провайдер подменён через package-var) → отказ с цифрами.
Revert-check: убрать проверку hosts → (b) красный; `--force` обходит `migrate.lock` →
(d) красный; VACUUM в dry-run → (e) красный.

**Шаг 3 — подсказка в `gc`/`purge` + help + CHANGELOG.** Хелпер `compactHint` в
`sessions_compact.go`, вызов в конце `sessions_gc.go`/`sessions_purge.go`.
Тесты: раздутая БД → подсказка в stderr; маленький freelist / `--json` / `--dry-run` → её нет.
Revert-check: убрать порог → «маленький freelist» красный.
CHANGELOG `[Unreleased]`: «`rush sessions compact` возвращает место из rush.db после удалений
(SQLite VACUUM, #1161): отказывается при живых процессах на этой БД (`--force` пропускает
проверку host/session lock'ов, но не startup-lock и не открытые транзакции), заранее проверяет
свободное место, печатает размер и freelist до/после; `--dry-run`, `--json`. `sessions gc/purge`
подсказывают команду, когда свободные страницы ≥ 64 МБ и ≥ 50% файла. Автоматического VACUUM нет.»

## 6. Не-цели и риски

Не-цели: `auto_vacuum=INCREMENTAL` и любая миграция режима; VACUUM на старте или по таймеру;
`--vacuum` у gc/purge; `VACUUM INTO`/бэкап; изменения в `internal/agent/`; смена прагм пула.
Риски:
- Процесс с открытым пулом, но без host/session lock (например простаивающий web-сервер без
  async-реестра), шлюзам 3–4 не виден. Если он начнёт писать, получит BUSY через 30 с.
  Смягчение: шлюз 5 и короткий VACUUM (копируются только живые страницы, 17 МБ — секунды).
  При реализации проверить, регистрирует ли web host; если нет — написать в Long.
- `--force` при живом писателе: его транзакция может упасть по BUSY (задокументировано).
- Пока держится `migrate.lock`, новые процессы ждут на старте. Это цель, но в Long предупредить.
