# Fifteenth review (read-only): the id-set watermark, two doc-hygiene commits, and one residual in the new fallback

- Reviewed HEAD: `889a20923a767e3ed91da093f8a32a429ed1bb11` (branch `main`)
- **Code range actually reviewed: `28f37afc..889a2092`** — 3 commits, 5 files,
  +589 / −52 in `internal/server`, plus comment-only deltas in
  `internal/agent` and `internal/platform`.
- Predecessor: `docs/reviews/2026-08-23-fourteenth-review-28f37afc.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every probe,
  mutation and revert-check below ran on **out-of-tree copies**
  (`git archive <rev> | tar -x` into `D:\system_artefact\Temp\crush-r15` for
  HEAD and `…\crush-r15-old` for `28f37afc`), never on the repo.
  `git status --porcelain` at the end shows only entries this review did not
  create (plus this report).

---

## Verdict

**GO**, with one P3 and two Minors. Nothing blocking.

**The six-round pattern is broken.** For the first time in this series the
substantial commit's central mechanism survived a deliberate attempt to break
it. I built five adversarial probes specifically hunting for the "the fix for
round N minted round N+1's defect" shape, and four of them came out clean
against HEAD:

- two sequential reruns of the same session, with an earlier identical prompt
  present, do not leak any watermark state between them;
- a replacement turn that creates its own prompt row and then **deletes that
  exact row** (the `runSummarizeBody` shape — its `toDelete` snapshot really
  does include the turn's own prompt) before erroring restores the operator's
  words exactly once, not twice;
- `targetID` collision is a non-question: message IDs are `uuid.New().String()`
  (`internal/message/message.go:298`);
- there is **no** early return between the recreate defer's registration
  (`handlers_agent.go:892`) and `runReturned = true` (`:954`) — the only
  `return`/`panic` tokens in that span are inside comments — so no
  cancellation/timeout path can bypass the `runReturned` bookkeeping.

Both halves of `889a2092` are load-bearing and independent, which I confirmed
by mutation rather than by reading: dropping `|| m.ID == targetID` fails
`TestRecreateRerunPromptIfLost_SurvivingTargetDoesNotRecreate` with count=2,
and reverting the defer gate to `releaseOnBailout && !recreateHandled` while
**keeping** the id-set fails the M-2 test with count=0 while the P2-1 test
still passes — exactly the "the id-set change alone did NOT fix M-2" claim the
commit message makes.

The one thing I did find is the loose thread the orchestrator flagged and did
not close: the new `baselineIDs == nil` fallback **does** mint a spurious
duplicate, and `28f37afc` did not, in a scenario I reproduced by execution
(P3-1 below). I grade it P3 rather than P2 — unlike every prior round's
blocker, this one is *deliberate*, *documented in both the commit message and
the code*, in the direction the fourteenth review's own P3-2 demanded, and it
needs a **second, independent** failure to reach. But it is a real behavioural
regression against the predecessor on that exact path, and it has a fix that
costs nothing, so it is filed rather than shrugged off.

The two doc commits (`782a0077`, `7d8cdd6c`) are exactly what they claim.
I checked both against the code rather than rubber-stamping: every rewritten
sentence is now true statement-for-statement, the platform test's assertion is
byte-identical, and the removed `os.Mkdir` really was dead.

---

## Findings, by severity

### P3-1 — the nil-baseline fallback creates an unconditional duplicate where `28f37afc` was correct, and the comment that justifies it under-states its own exposure

- `internal/server/handlers_agent.go:840-847` — the fallback comment
- `internal/server/handlers_agent.go:848-857` — `var baselineIDs map[string]struct{}`, left nil on List error
- `internal/server/handlers_agent.go:974` — the ungated explicit error-path call
- `internal/server/handlers_agent.go:1020` — `if baselineIDs != nil { … }`, i.e. skip the scan entirely
- `internal/server/handlers_agent.go:1029` — the unconditional `Messages.Create`

**The mechanism.** When the baseline `Messages.List` at `:849` fails,
`baselineIDs` stays nil. `recreateRerunPromptIfLost` then skips the scan
outright and calls `Messages.Create` no matter what the session already
contains. If the DB error was transient — the replacement turn's own
`createUserMessage` therefore succeeded — and the turn *later* returns an
error, the explicit call at `:974` appends a second copy of the operator's
prompt next to the one the turn already wrote.

**The comment's own justification does not hold on the path where this
fires.** `:845-847` argues:

```go
// In practice the helper's own List
// will most likely fail too (same store, same context) and nothing is
// created either way; the fallback only matters for a transient failure.
```

"Same store, same context" is true, but the two calls are not adjacent in
*time* on the error path: `:849` runs before the handoff and `:1010` runs
after the entire replacement run has returned — seconds to minutes of provider
work later. That is precisely the window in which a transient `SQLITE_BUSY`
(or any momentary DB error) clears. My probe shows exactly that: List call #2
fails, List call #3 — the helper's own, inside the explicit error-path call —
succeeds.

**Verification: CONFIRMED by execution.** Out-of-tree copy of HEAD, driving the
real `handleRerunMessage` with `a.Messages` wrapped in a decorator that fails
List call #2 (the baseline capture; #1 is step 2's tail list) and a fake
coordinator that fires the real `onHandoff`, creates the prompt + a finished
reply exactly as `createUserMessage` does, and then returns a second-turn
error:

```
WARN ws: rerun: failed to list messages for baseline ID set after target delete err="transient DB error (probe)"
ERROR ws: rerun agent error err="second-turn provider error"
    total List calls through the handler+fake: 3
    final[0] role=user      text="rerun me"
    final[1] role=assistant text="first turn reply"
    final[2] role=user      text="rerun me"        <- SPURIOUS duplicate
        Error: Not equal: expected: 1  actual: 2
--- FAIL: TestR15_NilBaselineAfterTransientListFailureDuplicatesPrompt (0.54s)
```

**Revert-check, executed by me:** the identical probe file, dropped unmodified
into a clean out-of-tree copy of `28f37afc` (the commit immediately before this
fix) — it compiles there because it is handler-level and does not touch
`recreateRerunPromptIfLost`'s signature:

```
=== RUN   TestR15_NilBaselineAfterTransientListFailureDuplicatesPrompt
    total List calls through the handler+fake: 3
    final[0] role=user      text="rerun me"
    final[1] role=assistant text="first turn reply"
--- PASS
```

`28f37afc`'s fallback (`baselineCount = targetIdx`) opened the scan window over
the whole list, found the turn's own prompt row, and suppressed correctly. So
this is a widening on this path — the same shape, if not the same weight, as
every prior round.

**Why P3 and not P2, stated rather than hidden.** Three things separate it from
the previous rounds' blockers, all of which cut in the fix's favour:

1. It is **deliberate and documented**, in the commit message and at
   `:840-847` / `:1003-1008` — not an unnoticed side effect.
2. It is the direction the **fourteenth review's own P3-2 asked for**: the old
   fallback biased toward *suppressing*, i.e. toward silently losing the
   operator's words, which this series has consistently graded strictly worse
   than a visible duplicate.
3. It needs **two independent failures** (a transient DB error at `:849` *and*
   a run that both writes the prompt and then fails). Round 13's P2-1 needed
   only a successful run; round 14's P2-1 needed only ordinary compaction.

A reviewer weighing only "the predecessor was correct here and HEAD is not"
would call it P2 and the round NO-GO. I don't, for the three reasons above.

**Fix shape** (so round sixteen doesn't re-derive it). The trade is avoidable
entirely, at zero cost and with no extra query. The handler **already holds a
full pre-delete listing**: `allMsgs` from `:704`, still in scope at `:849`.
Every row in it demonstrably predates the run, so its ID set is a valid
(superset) baseline. Seed `baselineIDs` from `allMsgs` unconditionally, then
union in the post-delete List's IDs when that call succeeds:

- On every path where `:849` succeeds, behaviour is **identical** to today —
  the extra IDs are tail/target rows that no longer exist, and a tail row that
  survived a failed delete is a pre-existing row that must not suppress
  anyway, which is what being in the set already gives you. The surviving
  target is still handled by the `|| m.ID == targetID` disjunct.
- When `:849` fails, the set is no longer nil, so the scan runs against the
  pre-delete baseline instead of creating blind. The only residual gap is a
  row created by a concurrent writer *between* `:704` and `:849` — a
  microseconds-wide window whose exposure is identical to the
  concurrent-writer exposure the design already accepts (M-2 below).

That removes the nil branch, the unconditional `Create`, and this finding,
without touching any behaviour the four `TestRecreateRerunPromptIfLost_*` unit
tests pin — except `_NilBaselineCreatesUnconditionally`, which would be
rewritten to assert the degraded-but-still-scanning behaviour.

---

## Minor items

- **M-1 — `889a2092`'s M-5 reordering is inert on the dominant error path, and
  the comment implies otherwise.** `handlers_agent.go:970-975`:

  ```go
  // The flag is set AFTER the call (fourteenth-review M-5): if the
  // call itself panics (List/Create on a closed DB), the defer stays
  // armed wherever its gate allows and retries, instead of having
  // been silently disarmed before the call.
  recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
  recreateHandled = true
  ```

  On the *normal* error path — `onHandoff` fired, so `releaseOnBailout ==
  false`, and `:954` already set `runReturned = true` — the defer's gate
  `(releaseOnBailout || !runReturned)` evaluates to `(false || false)`, so the
  defer cannot retry no matter what `recreateHandled` holds. The hedge
  "wherever its gate allows" makes the sentence technically true, but a reader
  takes it as coverage it does not have. **CONFIRMED by execution**: with
  `a.Messages.List` made to panic on call #3 (the helper's own) and a
  coordinator that fires `onHandoff` then returns an error, the run ends with
  **3 List calls and 0 prompt rows** — the defer never re-entered. The
  reordering only has an effect on the narrow error return where `onHandoff`
  never fired (`RunWithReservedOwnership`'s pre-handoff error returns), where
  the gate is still true. No behaviour regression — `28f37afc` lost the prompt
  identically on this path, since its gate was `releaseOnBailout` alone — so
  this is a comment-accuracy item, not a defect.

- **M-2 — the helper doc's "so it can only be…" is an over-claim of the family
  this series has now filed twice.** `handlers_agent.go:989-992`:

  ```go
  // text AND either its ID was NOT in the baseline set captured right after
  // the target delete (so it can only be the replacement turn's
  // createUserMessage row or an earlier explicit call's) or its ID IS the
  // original target's …
  ```

  There is a third source: `handleInjectMessage`
  (`handlers_agent.go:366-407`) persists a User row with the caller's text and
  takes **no** ownership check, and it is dispatched through `c.dispatch`
  (`handlers.go:43-44`) concurrently with `handleRerunMessage`, bounded only
  by `maxConcurrentHandlersPerConn = 12` per connection. If its text matches
  the rerun prompt exactly, its row is not in the baseline set and it
  satisfies the qualifying condition. **CONFIRMED by execution**
  (`TestR15_ForeignSameTextRowSuppressesRecreate`): a foreign row with
  identical text created during the run suppresses the recreate, leaving one
  row. The outcome is benign — the operator's exact words *are* on screen —
  and the exposure is unchanged from `28f37afc` (that row sat inside the
  position window too), so this is a doc precision item. The same doc's later
  sentence ("Unrelated concurrent writers are still ignored — their text does
  not match") is the correct caveat; the parenthetical should not out-claim it.

- **M-3 — `popFirstSubmitted`'s doc names a caller that no longer exists.**
  `internal/agent/mailbox_queue.go:32-35` says "Used by runSummarize to
  extract the first queued entry…", but
  `grep -rn "popFirstSubmitted" internal/ --include=*.go` finds **no
  production caller at all** — `runSummarize`'s manual-compaction success path
  moved to `abandonOwnershipAndPopFirstSubmitted` (`:124`), as
  `agent_compaction.go:268` itself records. Pre-existing and outside this
  batch's range; noted only because `7d8cdd6c` edited a comment 22 lines below
  it in the same file.

---

## Observations (no finding)

- **A turn that deletes its own prompt row and then errors is handled
  correctly, and identically by all three watermark designs.**
  `runSummarizeBody`'s `toDelete` snapshot (`agent_compaction.go:355-360`) is
  *every* non-pinned message, which includes the replacement turn's own prompt
  row — so scenario (b) from the review brief is genuinely reachable, not
  hypothetical. Probed against the real handler: the turn creates the prompt,
  creates a summary, deletes the whole snapshot including its own prompt, then
  errors. Result: `[SUMMARY, "rerun me"]` — exactly one restoration, no
  duplicate. Correct: the row really is gone, so recreating it is a restore.
  (`runSummarizeSilent` summarises only the older half, `:541-542`, so it
  cannot reach the turn's own prompt at all.)
- **The `releaseOnBailout` / `runReturned` pair is race-free and the gate is a
  strict superset of `#651`'s.** `(releaseOnBailout || !runReturned)` differs
  from `releaseOnBailout` only when `releaseOnBailout == false && runReturned
  == false`, i.e. after `onHandoff` fired and before the run returned — a
  window reachable **only** by a panic. Every success and error return is
  unaffected, which is why the round's own success-path test
  (`TestHandleRerunMessage_SuccessWithCompactionDoesNotDuplicatePrompt`) still
  passes. Both variables are written and read on the same goroutine
  (`onHandoff` fires synchronously at `agent_run.go:270-272`, before
  `runOwned`), and `go test ./internal/server/... -race` is clean.
- **Reruns still drop attachments** (`:944`/`:946` pass only `text`),
  pre-existing and untouched here. Carried forward from the fourteenth review.
- **`7d8cdd6c` completed the sweep, not just three-of-five.** After it, all
  five places that mention the finalize step and the latch
  (`agent_run.go:153`, `mailbox.go:81-82`, `mailbox_interrupt.go:49`,
  `mailbox_ownership.go:322`, `mailbox_queue.go:60`) agree that the finalize
  step is latch-blind. No sixth stale copy exists.

---

## What I checked and found sound

### `889a2092` — the fourteenth review's P2-1, P3-1, P3-2, M-1, M-2, M-5

- **P2-1 (blocker) closed.** Independently revert-checked, not taken on trust:
  I copied the first 327 lines of `p655_rerun_idset_regression_test.go` (the
  two handler-level regressions; the four direct unit tests need the new
  signature) into a clean `28f37afc` copy and ran them there —
  `ErrorPathAfterCompactionDoesNotDuplicatePrompt` fails with **count=2**,
  `PanicBetweenHandoffAndCreateUserMessagePreservesPrompt` fails with
  **count=0**. Both pass against HEAD. The tests exercise the real defects.
- **The id-set is genuinely invariant under deletion, by construction.** The
  suppression predicate is `User ∧ text == prompt ∧ (ID ∉ baseline ∨ ID ==
  targetID)`; none of the three terms reads an index or a length, so no
  deletion anywhere in the list can move a row into or out of the decision.
  I enumerated the difference against `#651`'s window in both directions and
  every difference is HEAD being right where `#651` was wrong (rows shifted
  below the window by compaction; baseline rows pushed above a
  `targetIdx`-fallback window; the surviving target).
- **`|| m.ID == targetID` is load-bearing.** Mutation: removing the disjunct
  (out-of-tree, restored and diff-verified byte-identical afterwards) fails
  `TestRecreateRerunPromptIfLost_SurvivingTargetDoesNotRecreate` with count=2.
- **The `runReturned` term is load-bearing and orthogonal to the id-set.**
  Mutation: gate reverted to `releaseOnBailout && !recreateHandled` with the
  id-set left intact → M-2's test fails with count=0 while P2-1's still
  passes. That is precisely the commit message's claim.
- **P3-1 closed.** The rewritten capture comment (`:827-838`) now *disclaims*
  exclusivity and names the three unguarded writers by file
  (`handleDeleteMessage`, `handleDeleteMessages`,
  `handleUpdateMessageContent`), which matches `handlers_messages.go`. It no
  longer uses exclusivity as the justification for the watermark.
- **P3-2 closed** at the level filed — the fallback's stated direction now
  matches the code. Its consequence is P3-1 above.
- **M-1 closed.** No "positional check" claim survives at `:859-874`.
- **M-2 closed**, verified twice (regression test + mutation).
- **M-5 mechanically applied** (`recreateHandled` now set at `:975`, after the
  call at `:974`), with the caveat in M-1 above.
- **All 26 rerun-related tests green on HEAD**, including every prior round's
  (`#614`, `#630`, `#638`, `#644`, `#645`, `#651`, `#655`).

### `7d8cdd6c` — the fourteenth review's P3-3, all three copies

Checked against the code, not against the commit message:

- **(a) `mailbox.go:79-92`.** Every replacement clause verifies:
  `grep -n "mb.stopped" internal/agent/*.go` shows the only read in
  `mailbox_ownership.go` is the ENTRY check at `:226` (plus the historical
  note at `:338`) — nothing after `mb.mu` is reacquired at `:310`. The
  finalize step drains `mb.replacement`/`mb.submitted` into `orphaned`
  (`:355-362`) and returns `hasNext = false` (`:379`). `restartOrphaned`
  (`agent_ownership.go:358-360`) delegates to `restartOrphanedWithRetry`,
  documented and implemented as a durable `session_run_queue` enqueue
  (`:362-371`) — so "durably enqueued … a DB row, not a fresh provider turn"
  is accurate, and the old "DISCARDS" claim was indeed stale.
- **(b) `mailbox_interrupt.go:47-57`.** The outcome half was already true and
  is preserved; the mechanism half now points at the ENTRY check, which is
  where the stopped-specific behaviour actually lives.
- **(c) `mailbox_queue.go:57-65`.** This doc belongs to
  `abandonOwnershipAndPopSubmitted` (`:78`), and its rewritten list is
  accurate item by item: `drainAfterCancel` does consult the latch
  (`mailbox_interrupt.go:289`), `interruptAndReplace` does (`:84`),
  `drainOrReleaseFinal`'s ENTRY check does (`mailbox_ownership.go:226`), and
  the finalize step does not. The surrounding claim it did not touch —
  "popping the queue here only feeds `restartOrphanedWithRetry`" — also holds:
  the sole caller is `agent_ownership.go:295`, inside
  `abandonOwnershipWithHandoff`.
- Comment-only: `git show 7d8cdd6c` contains no non-comment line.
  `go test ./internal/agent/... -count=1` green (57.3 s main package, all
  subpackages ok).

### `782a0077` — the fourteenth review's M-3 and M-4

- **M-3 doc correction is exactly right.** `readModuleLine` returns
  `("", nil)` for a go.mod with no module line (`source_tree_guard.go:181`);
  `parseModuleFromLine("")` yields `""`, which is not the crush module; at the
  marker the `checkDir == ancestor` branch `continue`s upward (`:91-97`) and
  above the marker the walk `break`s (`:99-104`). The new doc (`:157-161`) and
  the new inline note (`:178-180`) state both halves and nothing more.
- **M-4 is pure hygiene.** The 8-line scratch block and the
  `os.Mkdir(tmpDir/dev)` are gone; the assertion
  (`require.False(t, IsInSourceTree(exePath))`) and the whole path
  construction are byte-identical. The removed directory was provably dead:
  the walk from `interposed/dev/crush.exe` stats
  `interposed/dev/go.mod` → `interposed/go.mod` → `tmpDir/go.mod` and never
  touches `tmpDir/dev`. `go test ./internal/platform/... -v` shows the same 11
  top-level tests and 14 `TestIsInSourceTree` subtests, all passing — matching
  the commit's own verification claim.

---

## Executed verification summary

| check | result |
|---|---|
| `go build ./...` (real repo, read-only) | clean |
| `gofmt -l internal/ main.go` (real repo, read-only) | empty |
| `go vet ./internal/server/... ./internal/agent/... ./internal/platform/...` | clean |
| `go test ./internal/server/... -count=1 -race` (pristine HEAD copy) | **ok, 142.8 s** |
| `go test ./internal/agent/... -count=1` | ok (57.3 s main + all subpackages) |
| `go test ./internal/platform/... -count=1 -v` | ok, 11 tests / 14 subtests |
| all 26 `TestHandleRerunMessage_*` + `TestRecreateRerunPromptIfLost_*` on HEAD | all PASS |
| revert-check: `#655`'s two handler tests against `28f37afc` | **both FAIL** (count=2 / count=0) — they exercise the real defects |
| mutation: drop `\|\| m.ID == targetID` | `_SurvivingTargetDoesNotRecreate` FAILS (count=2) — disjunct load-bearing |
| mutation: gate → `releaseOnBailout` only, id-set kept | M-2 test FAILS (count=0), P2-1 test PASSES — halves are independent |
| probe: nil baseline + transient List failure + prompt-then-error | **reproduced** — duplicate prompt (P3-1) |
| revert-check: same probe against `28f37afc` | **PASS** — so P3-1 is a widening on that path |
| probe: two sequential reruns, earlier identical prompt present | PASS — no watermark leak |
| probe: turn deletes its own prompt row, then errors | PASS — exactly one restoration |
| probe: `List` panics inside the explicit error-path call | 3 List calls, 0 prompt rows — defer does not retry (M-1) |
| probe: concurrent same-text row during the run | suppresses the recreate (M-2), pre-existing exposure |
| grep: `return`/`panic`/`goto` between `:892` and `:954` | none outside comments — `runReturned` bookkeeping is total |
| grep: stale "finalize step re-reads stopped" copies | none remain — all 5 sites agree |
| `git status --porcelain` after all work | only pre-existing entries + this report |

---

## Things I could not verify, labelled as such

1. **P3-1's production trigger** is a transient DB error at
   `handlers_agent.go:849` that clears before `:1010`. Reproduced with an
   injected `message.Service` decorator, not with a real `SQLITE_BUSY`. That
   such a failure is transient and that the error path re-lists much later are
   both derived from the code, not measured against a contended database.
2. **M-2's concurrent-inject window** is executed at the handler level with the
   foreign row created from inside the fake coordinator (i.e. during the run),
   which is faithful in ordering but is not a real second WebSocket client.
3. **Panics are induced from a fake coordinator**, so they unwind through the
   real handler's defers but not through `runOwned`'s. `runOwned`'s own
   `abandonOwnershipWithHandoff` defer (`agent_run.go:307-309`) was read, not
   exercised, on the panic path.
4. **The `-count=20` / `-race` stress sweep this series has owed since the
   eleventh review is still owed.** I ran `-race` over the whole
   `internal/server` package once (clean, 142.8 s) and the full
   `internal/agent` package without `-race`. I did not observe either
   documented flake and did not re-litigate them.
5. **Windows-only.** Every measurement here is Windows. `internal/platform`'s
   POSIX behaviour is read, not executed.
6. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was read or written, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed.
7. **No `web/` delta in this batch**, so no Playwright run.
