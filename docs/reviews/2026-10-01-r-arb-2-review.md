# Ревью R-ARB-2 (перевод путей запуска на арбитр) — 2026-10-01

Ревьюер: @oxx (архитектор), только чтение. База: e171ecf2 + незакоммиченный дифф
worktree `rarb2` (29 файлов, +617/−894, новые `turn_arbiter_state.go`,
`turn_arbiter_testhelpers_test.go`).

**Вердикт: не вливать — три P1 с воспроизводимым сценарием.** Все правятся
локально. Если после правки новых P0/P1 нет — серия ревью закрыта (Review Stop Rule).

## P1

### P1-1. No-turn с видимым долгом сбрасывает гейт
Где: `drain_attempt.go:247–249` → `turn_arbiter.go:280–285` → `drain_attempt.go:317–319`.

`att.pendingLeft` вычисляется только при ПУСТОМ снимке (`agent_turn.go:407–409`),
поэтому «PendingLeft=false + непустой снимок» — не окно гонки, а КАЖДЫЙ отказ
коммита при видимом долге (paced/stuck-гейт, hold, суспензия, цепочка, капа, чужой
ведущий, нечитаемый вход). Старый код (`default`) гейт не трогал и делал
`addToRecheckSet` при `commitNo.recheck`; новый — `VNone "no debt: gate reset"` →
`resetGate` (пауза и обе серии обнуляются).

Сценарий: Drain D1 идёт, приходит факт → D2 в очереди (`mailbox_queue.go:62–66`).
D1 — пустой ответ/стрим (`agent_turn.go:868–871`) или watchdog
(`agent_turn_failure.go:282–286`) → учёт A7: пауза 60 с, PaidStreak=1. D2 в том же
цикле: снимок непуст, правило 9 → no-turn → новый учёт сбрасывает гейт. Релиз D2 →
`afterRelease` → `wakeSession` → VRun → D3 сразу к провайдеру (старый ждал RetryAt).
Следствия: R3B-4 снят; при потоке фактов K=3 набирается за секунды и строка
закрывается неудачей с маркером; оплаченный сон (D=3) не копится; отказ коммита по
hold стирает паузу.

Юнит-репро: `arb.pace(sid,h,time.Hour,false,pacePaidUnreacted)`, затем
`accountDrainAttempt` с `{outcome:drainNoTurn, snapshot:<1 строка>,
commitNo:{drainPaced, recheck:true}}`. Старое: гейт закрыт, sid в recheckSet.
Новое: гейт открыт, серии 0, recheck нет.

Правка: третий исход no-turn в `AttemptFacts` («коммит отказан») — гейт не трогать,
recheck по `commitNo.recheck`.

### P1-2. Сообщение человека стирает удержание rerun
Где: `turn_arbiter_state.go:266–273` (`*s = arbiterState{}` + delete). Старый
`resetConsecutiveResume` `turnHolds` не трогал.

Репро: `HoldAutomaticTurns(s)`; `ResetAutoResumeCounter(s)`; `drainPermitted` при
долге — было paced «rerun in progress», стало allow. Вложенные удержания: hold A →
reset → hold B → release A снимает B.
В проде: rerun держит hold на cancel и idle-poll до 10 с
(`handlers_agent_rerun.go:64–68`); Send/Inject/InterruptAndSend по той же сессии
идут конкурентно (`handlers.go:39/44/46`) и вызывают reset
(`handlers_agent.go:207/300/379`), как и resume (`coordinator_subagents.go:88`).
Hold пропадает → релиз отменённого хода запускает платный Drain, гоняющийся с rerun.

Правка: `resetForHumanMessage` сохраняет `holds`; запись удаляется только при holds==0.

### P1-3. Over-cap множество подрезается по устаревшему чтению
Где: строки долга читаются в `turn_arbiter.go:352`, bgArrival берётся позже (`:375`),
`retainOverCap` (`:394–396`) работает по устаревшему `owed`. Комментарий `:315–318` и
строка ASYNC-09 утверждают, что bgArrival покрывает чтение — неверно (старый
`bgShellCapDeferred` брал bgArrival ДО чтения).

Сценарий: капа исчерпана, гейт на паузе. Решение прочитало строки долга; до `:375`
приходит завершение R сверх капы, `persistBGShellCompletion` пишет R в overCap.
Решение берёт bgArrival; R нет в `owed` → `retainOverCap` его удаляет. Следующая
проверка: весь долг — bg-shell, R не помечен → OverCapRows < BGShellNotices →
правило 11 молчит → VRun → шестой авто-ход (ASYNC-09/R2B-16). Второе проявление:
BGShellNotices (`:341`) и OverCapRows (`:352`) из двух разных чтений вне замка.
Детерминированно воспроизводится швом между DB-половиной и lock (по образцу
`bgArrivalEnteredSeam`).

Правка: всё из одного `PendingInclusiveDebtRows` (Kind там есть); убрать подрезку из
`readTurnFacts` (достаточно подрезки при приходе под bgArrival). Вернуть оракул тесту
`drain_bgcap_test.go:234–246`: после удаления `f.ledger.store = nil` он проходит из-за
отменённого ctx на первом DB-чтении, а не из-за bgArrival — revert-check мёртв.

## Решения по вопросам исполнителя

- **Трата слота bg-shell в точке прихода** (`persistBGShellCompletion` →
  `arb.claimAutoResumeSlot`) — **утверждено.** По §9.2 оракул — код: слот тратится
  при допуске, до решения о запуске (R4B-1, (aa)); отложенный запуск слот сохраняет.
  Трата на VRun считала бы запуски, а не завершения (пришедшие в паузу не тратили бы,
  повторы тиком `Counted=false` — никогда; капа перестала бы ограничивать цепочку).
  Атомарность «вставка строки + решение по слоту» под bgArrival ((ad), R5B-1/2)
  держится только там. Условия: «писатель один» неверно, пока `retainOverCap` пишет
  overCap (P1-3); `Verdict.Counted` объявить описательным или удалить; поправить §3,
  §5 п.3, §6 дизайна арбитра (там ещё «побочный эффект VRun»).
- **Микро-расхождение (а)** `drainDecision` без долга не смотрит на гейт — допустимо:
  `drainPolicy` повторно вызывает `decide` с `PendingIncl=true`; остаток в CLIScope и
  decideDrainTurn (долг исчез между чтениями → VNone → allow) — узкое безвредное окно.
- **Микро-расхождение (б)** — дефект, это P1-1.
- **Фикс `OverCapRows` только при `AutoResumes >= cap`** — верно (эквивалент старого
  `consecutiveResume < CAP`); оговорка про два чтения — P1-3.
- **Зависание полного прогона** — механизма в диффе нет: порядок замков только
  bgArrival→arb.mu и bgArrival→l.mu; под ними нет DB-ввода и ожиданий; вызывающие
  передают ограниченный ctx. Изменилось: глобальный bgArrival берётся на каждом
  решении, ~11 DB-запросов вместо 4–5 — замедление, не вечное ожидание. Следующий
  прогон — с `-timeout 20m`, чтобы при таймауте получить стеки.

## P2/P3 — бэклог (не чинить в этом цикле)

- P2: уборка ledger больше не держит запись при живом гейте
  (`coordinator_session_state_sweep.go:27`), HintSeen живёт в арбитре — после уборки
  hintSeq обнуляется, free-dormant гейт может проигнорировать первый факт или
  открыться без факта; комментарий «harmless» неверен.
- P2: гейт (8–10) теперь раньше 11/12 (`turn_arbiter.go:228–244`): прежний deferred
  становится paced/stuck; в CLI вместо Deferred-выхода — ожидание до RetryAt или
  Stuck-выход с ошибкой. Буква §9.1, но противоречит §9.2/§7; исполнитель не назвал.
- P2: тень тавтологична — учёт и переходы состояния (reset, sweep, hold) со старым
  кодом не сравнивались; «расхождений нет» по §9.3 не обосновано.
- P3: CLIScope и decideDrainTurn не ставят маркер цепочки.
- P3: `readTurnFacts` читает всё безусловно — новые fail-closed поверхности для CLI-корня.
- P3: глобальный bgArrival на каждом решении — конкуренция между сессиями.
- P3: ветвление по строкам Reason хрупкое; причины нечитаемого входа слиты в одну строку.
- P3: тестовый `bumpConsecutiveResume` отказывает при суспензии и на капе.
- P3: `chainLink` плодит пустые записи арбитра.
