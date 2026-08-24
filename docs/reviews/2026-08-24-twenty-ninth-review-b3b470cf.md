# Round 29 review — `crush` fork @ `b3b470cf`

**Verdict: GO.** No product-behavior defect found after a full sweep of round
28's own follow-up leads, the entire `web/tests/` suite (run twice, full
parallel, 360/360 both times), and a fresh pass over Go server WS
dispatch/broadcast code. This round's own candidate lead (a transient
Playwright timeout) turned out to be pre-existing test-infra flakiness, not a
deterministic bug — see "Investigated, not filed" below.

---

## Context re-verified before starting

`6810c63e` (F-1 of round 28, task #685) was re-derived from its actual diff,
not just trusted from the round 28 report:

- `web/src/ws.ts:111-133` adds `latestLoadMessagesID` (a `Map<sessionID,
  msgID>`, intentionally never cleared) plus `sendLoadMessages`/
  `isStaleMessagesReply`. Every `load_messages` send now carries a
  client-generated `msgID` via `crypto.randomUUID()`.
- `web/src/useWS.ts:235` (`messages_list` handler) now calls
  `isStaleMessagesReply(sid, msg.id)` and returns early — drops the reply —
  before ever reaching `setMessages`/`setSubAgentMessages`.
- All 9 real `load_messages` call sites were migrated to `sendLoadMessages`
  (confirmed: `useWS.ts` ×5 — hashchange, session_created ×2,
  sessions_list-piggyback, dedicated follow-poll — plus `Sidebar.tsx:61`'s
  session-switch and `SubAgentBlock.tsx:198`'s lazy load).
- `web/tests/messages-list-reorder.spec.ts` genuinely captures the two real
  send sites' real `msgID`s and replies out of order; the control test
  (in-order replies) and bug test (out-of-order, older-second) both pass
  against the current code, and both were read in full to confirm they
  assert the correct final DOM state, not just that a WS event fired.

This fix does exactly what its commit message and round 28's report claim.
Not re-flagged.

`b3b470cf` (test-hygiene fix, task #686) was checked for completeness per the
brief's instruction: `web/tests/sessions.spec.ts` was read in full (371
lines). The stale "deleted session disappears from sidebar immediately" test
is gone; a comment at `:274-278` correctly points to
`sidebar-delete.spec.ts`'s "CONTROL: successful delete_session removes the
row and stays removed" as the surviving, correctly-reply-gated equivalent. No
other assertion in the file references pre-#684 (optimistic session delete)
or pre-#685 (unconditional `messages_list` apply) behavior — every
`sessions_list`/`session_created`/`session_updated`/`session_deleted`/
`messages_list` test in the file sends the event and asserts against
current, reply-driven semantics. `sidebar-delete.spec.ts` itself (211 lines)
was also read in full: all 5 tests (control delete, rejected delete with
inline error, partial-failure `delete_other_sessions`, full-success
`delete_other_sessions`, no-spurious-`create_session`-on-rejected-only-delete)
match `3c6c56d4`'s actual diff. No gap found — #686 is complete.

---

## Investigated, not filed — transient Playwright flakiness in `reasoning-effort.spec.ts`

The first full-suite run (360 tests, `workers: "50%"` = 8 parallel workers)
produced exactly one failure:

```
1) tests\reasoning-effort.spec.ts:296:3 › Reasoning Effort Controls ›
   effort badge renders from the real server wire payload

Test timeout of 30000ms exceeded.
Error: page.evaluate: Test timeout of 30000ms exceeded.
   at helpers\mock-ws.ts:77
   at tests\reasoning-effort.spec.ts:323:5

1 failed
359 passed (3.0m)
```

This test (`web/tests/reasoning-effort.spec.ts:75-110`,
`realWireEffortMaxMessage()`) is unique in the suite: it shells out
synchronously via `execFileSync("go", ["test", "-overlay=...", ...])` mid-test
to compile-and-run a temporary Go probe (`TestZZEffortWireProbe`) that proves
`toMessageWire` really puts `ReasoningEffort` on the wire, rather than
trusting a hand-written mock field — a deliberate design choice that traces
back to round 23's F-2 finding (`docs/reviews/2026-08-24-twenty-third-review-7c8a170e.md:199-213`),
which caught this same test asserting a field the wire never actually sent.
The intent is sound; the mechanism (a blocking `go test` compile inside one
Playwright worker, sharing the machine with 7 other browser-driving workers)
is measurably more timing-sensitive than the rest of the suite.

Verified this is flakiness, not a regression, three ways:
1. Re-ran only this test in isolation (`--workers=1`): passed in 9.4s
   (`1 passed (25.2s)` wall time including webServer boot).
2. Re-ran the entire 360-test suite a second time, full parallel, identical
   command: **360/360 passed**, this same test completing in 15.4s.
3. No other spec in `web/tests/` shells out to an external process
   (`grep -rn "execFileSync\|spawnSync" web/tests/` matches only this one
   call site), so this fragility is contained to a single test, not a
   systemic pattern.

Not filed as a finding: it is pre-existing test-infra fragility (a slow,
resource-contending step racing a fixed action timeout under heavy parallel
load), not a product defect, and reproduced clean on immediate retry — it
does not meet this loop's bar of "a real behavioral bug a user/operator would
see." Noting it here for the record in case a future round sees it recur and
wants to harden the timeout or move the probe to a `beforeAll`/fixture,
neither of which was in scope for read-only review.

## Fresh Go-side ground covered (angle (c) from the brief)

- **`events.go`'s pubsub-to-hub bridge** (`internal/server/events.go`, full
  144 lines read): checked whether the messages goroutine's batching
  (`CreatedEvent`/`DeletedEvent` broadcast immediately, `UpdatedEvent`
  deduped into `pending` and flushed on a 16ms ticker) can let an update for
  message X reach a client before X's own create. It cannot: both event
  types are handled by the same single goroutine reading one `ch` in
  sequence, `Broadcast` synchronously enqueues into `h.broadcast` before that
  goroutine loops to the next pubsub event, and `Hub.Run` drains
  `h.broadcast` strictly FIFO (already confirmed structurally sound in round
  28's swept §4) — so a create for a given message ID is always enqueued
  into the hub's broadcast channel before any later update for that same ID
  can be. No reordering hazard here.
- **`Hub.Run`'s register/replay/sticky path** (`hub.go:386-520`): re-read
  against the current code (unchanged since round 28's own re-verification)
  — still correct, nothing new.
- **`sessions_list`/`list_sessions`** (round 28's own lower-confidence,
  not-filed lead): re-confirmed the structural symmetry with `messages_list`
  — `CmdListSessions` dispatches through the same non-FIFO worker pool
  (`handlers.go:56-57`, `hub.go:147-157`), and `setSessions` (`store.ts:57-59`)
  is the same kind of unconditional full-array replace `setMessages` was
  before the fix. Concur with round 28's judgment not to file it: the
  send-site count differs materially (`list_sessions` fires from one
  recurring 5s timer plus two one-shot triggers — tab-focus and reconnect —
  versus `messages_list`'s two permanently-coexisting recurring timers for
  the entire time a session is `OwnedExternal`), and a stale sessions-list
  snapshot is a less perceptible regression than a chat transcript losing
  its newest message. This is still a real theoretical sibling gap, not a
  demonstrated one — leaving it as a standing note rather than re-filing it
  with no new evidence beyond round 28's own analysis.
- **`SubAgentBlock.tsx`'s lazy-load path** (`:184-199`): confirmed sub-agent
  session IDs are distinct UUIDs from main-session IDs, so they occupy
  separate keys in `ws.ts`'s `latestLoadMessagesID` map with no risk of
  cross-session guard collision.
- **Reconnect interaction with the new stale-reply guard**: `latestLoadMessagesID`
  is intentionally never cleared, including across a WebSocket disconnect/
  reconnect cycle. Confirmed this is safe, not a hazard: a closed socket
  cannot deliver any reply after close, and a fresh connection starts a new
  send/reply cycle, so no pre-disconnect request's reply can ever arrive
  post-reconnect and be wrongly accepted or wrongly dropped.

## Full suite run twice, clean

`pnpm typecheck` (tsc --noEmit): clean. `rsbuild build`: clean, 11.7s,
812.8 kB / 232.9 kB gzip, no new warnings beyond the pre-existing
browserslist-staleness notice. Full Playwright suite (360 tests):
360/360 first run minus the one flaky timeout discussed above,
**360/360 clean on immediate re-run**. No `.skip(`/`.fixme(` anywhere in
`web/tests/`.

---

## Swept, nothing filed

So a hypothetical round 30 does not redundantly re-check the same ground.

1. **`6810c63e` (round 28 F-1) and `b3b470cf` (test-hygiene fix)** —
   re-derived from their actual diffs and cross-checked against every
   session/message-list test in `web/tests/sessions.spec.ts` and
   `web/tests/sidebar-delete.spec.ts`. Both complete and correct; no gap.
2. **`sessions_list`/`list_sessions`'s theoretical sibling gap to F-1** —
   re-confirmed structurally identical worker-pool exposure to what
   `messages_list` had, but round 28's judgment that it's materially
   lower-risk (fewer coexisting recurring pollers, less perceptually jarring
   if it does regress) still holds; no new evidence changes that call.
3. **`events.go`'s create/update/delete batching for the same message ID** —
   read in full; single-goroutine-per-source plus FIFO broadcast channel
   makes cross-type reordering for the same message structurally
   impossible, independent of round 28's broadcast-vs-broadcast finding
   (which covered same-type reordering).
4. **`Hub.Run`'s sticky/replay ordering invariant** — re-read against
   current code, unchanged and still sound since round 28's own
   re-verification.
5. **`reasoning-effort.spec.ts`'s wire-probe test** — confirmed via isolated
   rerun (9.4s, pass) and a second full-suite run (360/360, pass) that the
   one observed failure was parallel-load timing flakiness in a test that
   shells out to `go test` synchronously, not a deterministic defect. Not a
   reportable finding; noted above for the record only.
6. **`web/tests/` for other `.skip(`/`.fixme(` or stale-assertion tests** —
   grepped the whole directory; zero matches. Read every test in
   `sessions.spec.ts` and `sidebar-delete.spec.ts` line-by-line (the two
   files most likely to carry pre-#684/#685 assumptions) and found none
   asserting outdated behavior.
7. **Go backend** — not reopened beyond what was necessary to trace the
   `events.go`/`hub.go` ordering questions above, consistent with the
   round-19 closure and every prior round's treatment of web/server
   co-located questions. No backend-only defect found.

## Artefacts

All investigation was read-only against the repo; no files under
`D:\dev\go\crush` were modified (confirmed via `git status --porcelain`
before and after — only the pre-existing `D web/dist/.gitkeep` and untracked
`docs/`/`dev/` entries that predate this session appear). Verification
commands run (all outside-the-repo-effect, standard dev tooling):

- `cd web && node_modules/.bin/tsc.CMD --noEmit -p .` — clean
- `cd web && node_modules/.bin/rsbuild.CMD build` — clean, 11.7s
- `cd web && E2E_PORT=<port> node_modules/.bin/playwright.CMD test --reporter=list` — run twice: 359/360 then 360/360 (one flaky timeout, confirmed non-reproducing)
- `cd web && E2E_PORT=<port> node_modules/.bin/playwright.CMD test tests/reasoning-effort.spec.ts -g "effort badge renders from the real server wire payload" --workers=1` — isolated rerun, 9.4s pass
