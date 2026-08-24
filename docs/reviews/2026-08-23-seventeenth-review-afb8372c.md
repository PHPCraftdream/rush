# Seventeenth review (read-only): the seed is now pinned, the UNION is not; two adders the enumeration misses; three stale pointers in the file the doc fix edited

- Reviewed HEAD: `afb8372c0d714ebc33a4f58ce7316f20fe8f07d0` (branch `main`)
- **Code range actually reviewed: `cb35b0be..afb8372c`** — 2 commits, 4 files,
  +188 / −8 (one 170-line test addition, the rest comment-only).
- Predecessor: `docs/reviews/2026-08-23-sixteenth-review-cb35b0be.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every build,
  test, mutation and probe below ran on an **out-of-tree copy**
  (`git archive afb8372c | tar -x` into
  `D:\system_artefact\Temp\crush-r17`), never on the repo.
  `git status --porcelain` at the end shows only entries this review did not
  create (plus this report).

---

## Verdict

**GO**, with three Minors and **zero P-level findings**. Nothing blocking.

Both commits do exactly what they claim, and I verified both by execution
rather than by reading:

- **M-2 is closed and the new test is genuinely load-bearing.** I reproduced
  the sixteenth review's mutation independently (delete the three-line seed
  loop, leave `baselineIDs` non-nil but empty) and confirmed that
  `TestHandleRerunMessage_SeedContentPinsEarlierIdenticalPromptOnFailedListPath`
  is the **only** one of the 27 rerun tests that goes red. I also probed four
  other mutation shapes against it (see "What I checked and found sound"): it
  is tight for what it claims, and I found **no** "green for the wrong reason"
  in it. `handoffOnlyErrorCoordinator` is a faithful model of a real
  post-handoff, pre-`createUserMessage` failure.
- **M-1's reworded window claim is now correct and its mechanism explanation
  matches the code** (`Messages.Delete` really is a get +
  `DeleteMessageIfTerminal` + publish per tail row).
- **`f4ab5b6b`'s two pointers are accurate.** `runSummarize`'s
  `abandonOwnershipAndPopFirstSubmitted` call really is
  `agent_compaction.go:274`; `internal/agent/mailbox.go` really declares zero
  functions and all eleven named methods really live in exactly the five files
  the new text names. I checked all eleven individually, not the claim.

**No eighth residual in the mechanism.** As instructed, I did not re-litigate
the ID-set design; I attacked this round's own artefacts instead. The three
Minors that came out of that are:

1. The **mirror image of the finding just closed**: nothing in the 27-test
   suite pins the post-delete **union** loop. Deleting it keeps all 27 green
   while silently losing the operator's prompt on the success path when a
   concurrent writer lands a same-text row inside the very window `afb8372c`'s
   new comment is about. Confirmed by execution, both directions.
2. The adder enumeration the same comment introduces is **incomplete by two
   production paths** — one in-process (`notifyBackgroundJobDone`), one
   cross-process (`crush sessions inject`) — and the cross-process one also
   makes step 1b's blanket "no external writer can start mid-delete" an
   overstatement. This is what the brief's item 2 asked me to look for.
3. `f4ab5b6b` corrected one stale `(agent.go)` pointer in `mailbox_queue.go`
   while leaving **three identical ones in `mailbox.go`, the other file it
   edited in the same commit**, 160–200 lines below its own edit.

None of the three is a code defect. HEAD is correct on every path I executed.

---

## Findings, by severity

### P1 / P2 / P3 — none.

---

## Minor items

### M-1 — nothing pins the post-delete UNION loop: delete it and all 27 tests stay green while the operator's rerun prompt is silently lost

- `internal/server/handlers_agent.go:880-887` — the union loop
- `internal/server/handlers_agent.go:867-875` — the residual-gap paragraph
  `afb8372c` rewrote, which is exactly about the rows the union covers

This is the same shape as the sixteenth review's M-2, one loop further down,
and it survived that review because the mutation tried there was on the seed,
not on the union.

**What the union uniquely contributes.** The seed is every ID in the
pre-delete listing. The union adds only IDs that are *not* in the seed and
*are* in the post-delete listing — i.e. exactly the rows **created between the
two listings**, the window `afb8372c`'s own new comment measures at
milliseconds-to-hundreds-of-milliseconds. On the success path, the union is
what puts such a row into the baseline so it **cannot** suppress the recreate.
Without it, that row reads as "new" — the helper's `!inBaseline` disjunct
qualifies it — and the recreate is suppressed.

**Mutation, executed.** Replace the union loop's body with nothing (keep the
`List` call and the `else` warning branch, so only the union is removed):

```go
// mutated
if _, listErr := a.Messages.List(deleteCtx, sessionID); listErr == nil {
        _ = listErr
} else {
```

```
go test ./internal/server/ -count=1 -run 'TestHandleRerunMessage_|TestRecreateRerunPromptIfLost_'
ok  github.com/charmbracelet/crush/internal/server  5.459s      <- ALL 27 GREEN
```

The suite is fully green on a handler that has lost the union entirely.

**The mutation is a real loss, not a hypothetical.** Probe
(`TestR17_UnionLoopIsLoadBearing_ForeignRowInsideTheWindow`, written
out-of-tree, run on both HEAD and the mutant): history
`[target User "rerun me", tail Assistant "old reply" (finished)]`;
`rerunTailDeleteSeam(0)` creates a foreign `User` row with the same text —
i.e. exactly a concurrent `handleInjectMessage` / `crush sessions inject`
landing inside the tail-delete window; the post-delete `List` **succeeds**;
the replacement turn is `handoffOnlyErrorCoordinator` (the fake `afb8372c`
itself just added), which fires `onHandoff` and errors without writing a row.

```
HEAD (afb8372c):     final[0] user "rerun me"   <- the FOREIGN row
                     final[1] user "rerun me"   <- the recreate
                     matching user rows = 2     PASS

union loop deleted:  final[0] user "rerun me"   <- the FOREIGN row only
                     matching user rows = 1     FAIL
```

So on the mutant the operator's rerun prompt is **gone** and a foreign
message stands in its place — the same loss class as #644, on the *success*
path this time (the failed-`List` path is the documented residual and is
unaffected either way).

**Why the existing 27 miss it.** `TestHandleRerunMessage_ConcurrentUnrelatedWriterDoesNotSuppressRecreate`
is the only test that lands a concurrent row, and its text deliberately does
*not* match, so the row is inert regardless of which set it lands in.
`TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt`
and the new `_SeedContentPins…` both run on the failed-`List` path, where the
union never executes. Every other test has no writer in the window at all, so
seed ⊇ post-delete set and the union is a no-op by construction.

**Fix shape:** the probe above, verbatim, ~70 lines, no new seam
(`rerunTailDeleteSeam` and `handoffOnlyErrorCoordinator` both already exist).
Revert-check for its own doc: remove the union loop's body and it fails with
count=1.

Severity **Minor**, consistent with the sixteenth review's ceiling for
test-quality items. Like its predecessor, it is the item on this page most
worth acting on: it is the one that would let round eighteen's edit
reintroduce a prompt loss with a green suite.

---

### M-2 — the new writer enumeration is short by two production adders, and step 1b's "no external writer can start mid-delete" is an overstatement

- `internal/server/handlers_agent.go:830-837` — the enumeration `afb8372c` extended
- `internal/server/handlers_agent.go:661-664` — the pre-existing external-silence claim
- `internal/agent/coordinator_background.go:87` — missing adder #1
- `internal/cmd/sessions_inject.go:169` — missing adder #2

This is the brief's item 2. I traced it from scratch rather than trusting the
sixteenth review's own trace, and the adder set is **larger** than the brief's
expected `{handleInjectMessage, the replacement turn's createUserMessage,
recreateRerunPromptIfLost}`.

Complete production set of "creates a `User`-role row with caller-supplied
text into an **existing** session"
(`grep -rn "Messages\.Create\|messages\.Create" --include=*.go internal/`,
then role-checked at each site):

| site | role | ownership held? | in the comment? |
|---|---|---|---|
| `agent_prompt.go:81` via `agent_turn.go:369` (`runOwned` preamble) | User | **yes** — reserved before any write | yes (as "agent Run") |
| `agent_prompt.go:81` via `agent_control.go:90` ← `coordinator.InjectMessage` ← **`handleInjectMessage`** (`handlers_agent.go:401`) | User | no | **yes** |
| `agent_prompt.go:81` via `agent_control.go:90` ← `coordinator.InjectMessage` ← **`coordinator.notifyBackgroundJobDone`** (`coordinator_background.go:87`) | User | no | **NO** |
| **`doInject`** (`cmd/sessions_inject.go:169`) — `crush sessions inject` | User | no, and **another process** | **NO** |
| `handlers_agent.go:1072` (`recreateRerunPromptIfLost`) | User | n/a (is the mechanism) | n/a |

Ruled out, checked individually: `agent_compaction.go:369` and `:563` are
`message.Assistant` (summary rows); `agent_turn.go:1109` is the streaming
assistant row; `agent_turn.go:1283` and `:1678` are `message.Tool`;
`session_fork.go:190` writes into the **new** fork id, never the live session;
`coordinator.InterruptAndSend` writes no row itself (it goes through
`interruptAndReplace` / `EnqueueRunQueueEntry`, and the row is minted later by
a run that holds ownership).

**Adder #1 — `notifyBackgroundJobDone`.** It runs on a detached
`BackgroundShell.OnDone` goroutine (`coordinator_tools.go:293-296`) that
"outlives the turn that started it" (its own doc, `coordinator_background.go:49`).
When `autoResumeEligible` is false — autonomy off, non-persistent mode, or the
`maxConsecutiveAutoResumes` cap reached (`coordinator.go:471-475`) — it takes
the Phase-3 branch and calls `c.InjectMessage` directly, with no ownership
check and no relation to any WS handler. It can therefore fire *inside* the
capture window at any instant. Its text is
`backgroundJobSummary(...)`, which embeds a unique shell id, so a text
collision with a rerun prompt is close to unreachable — but it is a second,
non-handler caller of the exact sink the comment attributes to
`handleInjectMessage` alone, and the comment's parenthetical
("via `coordinator.InjectMessage`") points at the shared sink while naming
only one of its two callers.

**Adder #2 — `crush sessions inject`, and the stronger claim it breaks.**
`doInject` calls `messages.Create` with `Role: message.User` and
operator-supplied text, in a **different process**, after a `setupApp` that
does nothing but `config.Init` + `db.Connect` (`cmd/root.go:310-349`) — no
session lock is taken anywhere on the path. Its only interaction with the lock
is `isSessionLockAlive`, a read-only `InspectSessionLock` call made **after**
the write, purely to choose a status string. So step 1b's shared-lock probe
does not exclude it, and `handlers_agent.go:661-664`'s

```go
// while the shared lock is held, no process — including this one — can
// acquire the exclusive lock, so no external writer can start mid-
// delete.
```

is true for the writer class it was written about (`crush run --session S`
takes the exclusive lock) and false as stated: `sessions inject` is an
external writer that starts, and completes, mid-delete. This one's text
collision is entirely realistic for this fork's positioning — an orchestrator
script injecting `"continue"` while the operator reruns an earlier
`"continue"` is #644's literal shape.

**What is NOT wrong here.** No code defect. The ID-set design tolerates
arbitrary concurrent writers by construction, which is the whole point of
`#655`/`#658`, and both new adders land in exactly the tolerated classes
(before the pre-delete listing → in the seed; between the listings → in the
union, unless M-1's loop is removed; after the capture → the documented
"can land a same-text row in that window too" case the helper's own doc at
`:1029-1034` already names). The finding is that the enumeration reads as
exhaustive, is used to justify a deliberate residual, and misses the only
**cross-process** member of the set — the one the reader is least likely to
derive themselves, and the one step 1b's own comment claims cannot exist.

**Fix shape:** at `:830-837`, name `coordinator.InjectMessage`'s two callers
(`handleInjectMessage` and `notifyBackgroundJobDone`) rather than the handler
alone, and add one clause for `crush sessions inject`
(`cmd/sessions_inject.go:169`) as a cross-process adder the shared-lock probe
does **not** exclude. At `:663-664`, narrow "no external writer" to "no
external agent **run**" — the lock excludes lock-taking writers, not
`sessions inject`.

---

### M-3 — `f4ab5b6b` fixed one stale `(agent.go)` pointer and left three identical ones in the other file it edited

- `internal/agent/mailbox.go:185` — `testLoopRearmSeam` "invoked by Run's turn loop **(agent.go)**"
- `internal/agent/mailbox.go:211` — `testPreAbandonSeam` "invoked by runSummarize **(agent.go)**"
- `internal/agent/mailbox.go:228` — `testPreSnapshotConsumeSeam` "invoked by runSummarize **(agent.go)**"

`f4ab5b6b` edited `mailbox.go:20-26` (the file-level doc) and
`mailbox_queue.go:115` (`"(agent.go)"` → `"(agent_compaction.go)"`). Both
edits are correct — see "What I checked and found sound". But the *same*
`(agent.go)` staleness the commit went out of its way to fix in
`mailbox_queue.go` appears three more times inside `mailbox.go` itself,
160–200 lines below the hunk the commit landed there:

| pointer | claims | actually |
|---|---|---|
| `mailbox.go:185` | `agent.go` | `agent_run.go:411-412` |
| `mailbox.go:211` | `agent.go` | `agent_compaction.go:245-246` (inside `runSummarize`, `agent_compaction.go:107`) |
| `mailbox.go:228` | `agent.go` | `agent_compaction.go:212-213` (same function) |

Two of the three name `runSummarize` — the *identical* miss
`mailbox_queue.go:115` had, in the same package, in the same commit's own
diff context.

Two more of the same class outside the edited hunks, for completeness:

- `internal/agent/mailbox_interrupt.go:176` — "The loop's own
  `testLoopRearmSeam` window (agent.go)" → `agent_run.go:411`.
- `internal/agent/mailbox_interrupt.go:190` — "`reclaimReplacementOrKeep` is
  called by Run's turn loop (agent.go)" → `agent_run.go:432`.
- `internal/agent/stream_watchdog.go:180` and `:376` — "`withActivityNotify`
  (agent.go)" → `agent_ownership.go:592`.

(`internal/agent/tools/tools.go:89`'s `(agent.go)` is **correct** — it refers
to *fantasy v0.25.2*'s `agent.go`, not the fork's. Checked, not filed.)

All of these predate the split-out (`ee922fda`) in the same way M-3 of last
round did. I file it because the commit under review is the one whose entire
subject line is "two stale location pointers in the mailbox docs", and it left
three of them in a file it was editing anyway — a reader who follows
`mailbox.go`'s own field docs lands in the wrong file on the very next hop.

**Fix shape:** three one-word substitutions in `mailbox.go`
(`agent.go` → `agent_run.go` at `:185`, → `agent_compaction.go` at `:211` and
`:228`), and optionally the four in `mailbox_interrupt.go` /
`stream_watchdog.go`.

---

## Observations (no finding)

- **`handoffOnlyErrorCoordinator` is faithful.** Real
  `RunWithReservedOwnership` (`agent_run.go:192-280`) fires `onHandoff` at
  `:270-272`, immediately after `rebindDispatcher` and `reserveCancel()`, and
  everything past the HANDOFF LINE at `:274-278` is `runOwned`, whose deferred
  `abandonOwnershipWithHandoff` owns the release. The fake fires `onHandoff`
  first, installs `defer ReleaseExclusive`, and errors — the exact shape of a
  `runOwned` preamble failure (`sessions.Get` / `getSessionMessages` /
  the OS-lock acquire) before `createUserMessage`. The four genuine
  *pre*-handoff error returns (`ErrEmptyPrompt`, `ErrSessionMissing`,
  `ErrAgentShuttingDown`, the `rebindDispatcher` refusal) all sit above the
  handoff and are a different shape, correctly not modelled here.
- **One fidelity gap in the fake, inert:** real code calls `reserveCancel()`
  at `:265` *before* `onHandoff`, while `cancellableHoldCoordinator.ReleaseExclusive`
  (`p630_rerun_cancel_window_test.go:71-79`) ignores the `cancel` it is
  handed, so `holdCtx` stays live in the test where production would have
  cancelled it. No divergence: past the commit point the handler never reads
  `holdCtx` again (its last check is `:803`), and everything downstream runs
  under `deleteCtx = context.WithoutCancel(holdCtx)`. Every other fake in the
  suite shares this, so it is a suite-wide convention, not a new defect.
- **The new test's assertions are tight for what they claim.** Besides the
  seed mutation, I reasoned through four other broken shapes against it:
  seeding from `allMsgs[targetIdx+1:]` instead of `allMsgs` → **red**
  (count=1); a helper that creates unconditionally → green here but red in
  `_TransientBaselineListFailure…`; dropping the `|| m.ID == targetID`
  disjunct → green here (the target row is deleted on this path) but red in
  `_SurvivingTargetDoesNotRecreate`; a double recreate (defer + explicit) →
  **red**, because the assertion is `require.Equal(t, 2, count)`, not a
  lower bound. The three preconditions it asserts before the decisive one
  (`listCalls == 3`, `failedAt == 2`, and the three `Get` outcomes) genuinely
  pin the path rather than decorating it — I confirmed the handler + helper
  make exactly three `List` calls (`:704`, `:880`, `:1055`) and no more.
- **The residual-gap rewrite is mechanically accurate.** `Messages.Delete`
  (`internal/message/message.go:187-249`) really is a `Get` +
  `DeleteMessageIfTerminal` + `PublishMustDeliver` per row, so "one
  `Messages.Delete` … per tail row" describes the code, and the commit
  replaced a wrong magnitude with a structural reason rather than substituting
  a second machine-specific number — the right call.
- **The enumeration at `:830-837` also omits four more `handlers_messages.go`
  mutators** (`handleUpdateMessageThinking:284`, `handleDeleteMessagePart:321`,
  `handleUpdateMessagePart:343`, `handleTogglePinMessage:395`, all via
  `updateMessageAndVerify`). Immaterial and deliberately not filed: all four
  are edit-only, which is the class the sentence already covers, and an
  edit can never suppress — the edited row's ID is in the seed regardless of
  what its text becomes.
- **Cosmetic:** `handlers_agent.go:837` ends up 78 characters past the tab
  where the rest of the paragraph wraps at ~72 — the inserted clause did not
  re-wrap the sentence's tail. `gofmt` is clean; noted only so the next editor
  reflows it while touching M-2's fix.
- **`recreateRerunPromptIfLost` still drops attachments** (`:1072-1075` pass
  only `text`) — pre-existing, untouched, carried forward from the
  fourteenth/fifteenth/sixteenth reviews.
- **`mailbox.go`'s new file-level doc is very slightly narrower than the
  file**: it says the file "declares the mailbox type, its state constants and
  its fields only", while the file also declares the `generation` and
  `pendingInject` helper types — which are the types of two of those fields.
  Within tolerance; not filed.

---

## What I checked and found sound

### `afb8372c` — the sixteenth review's M-1 and M-2

- **The commit is comment-plus-test only.** `git show afb8372c` contains
  exactly two comment hunks in `handlers_agent.go` and one addition block in
  `p655_rerun_idset_regression_test.go` (the new fake, its
  `var _ agent.Coordinator` assertion, the file-header note, and the test).
  Zero production statements changed.
- **M-2's mutation reproduces independently.** Deleting only the three-line
  seed loop (leaving `baselineIDs := make(map[string]struct{}, len(allMsgs))`
  and the union intact) reddens **exactly one** test:

  ```
  --- FAIL: TestHandleRerunMessage_SeedContentPinsEarlierIdenticalPromptOnFailedListPath (0.19s)
  FAIL  github.com/charmbracelet/crush/internal/server  5.570s
  ```

  All 26 others, including `#658`'s own
  `_TransientBaselineListFailureDoesNotDuplicatePrompt`, stay green — matching
  the sixteenth review's finding and the orchestrator's own re-derivation.
- **All 27 rerun tests pass on HEAD** (`6.128 s`), and the whole
  `internal/server` package passes (`24.662 s`), as does
  `./internal/agent/...` across all subpackages (`58.2 s` main).
- **M-1's `handleInjectMessage` addition is correct as far as it goes** —
  `handleInjectMessage` → `coordinator.InjectMessage`
  (`coordinator_interrupt.go:537-552`) → `sessionAgent.InjectMessage`
  (`agent_control.go:89-98`) → `createUserMessage`, with no reservation
  anywhere on the path, and it is dispatched concurrently
  (`handlers.go:43-44`). The "agent Run cannot land one here" clause also
  holds: `RunWithReservedOwnership` reserves and rebinds before any row is
  written, and the Phase-4 auto-resume branch of `notifyBackgroundJobDone`
  goes through `c.Run`, which cannot become owner while this handler holds
  the reservation. See M-2 above for what the clause is missing.
- **The three enumerated `handlers_messages.go` handlers really are
  delete/edit-only** — `handleDeleteMessage:195` (`Messages.Delete`),
  `handleDeleteMessages:209` (`Delete`/`ForceDelete`),
  `handleUpdateMessageContent:250` (`updateMessageAndVerify` →
  `Messages.Update`). No `Messages.Create` exists anywhere in
  `internal/server` outside `recreateRerunPromptIfLost`.

### `f4ab5b6b` — the sixteenth review's M-3

Checked against the code, not against the commit message:

- **`(agent.go)` → `(agent_compaction.go)` is right.** `runSummarize` is
  `agent_compaction.go:107`; its manual-compaction success-path call
  `mb.abandonOwnershipAndPopFirstSubmitted(epoch)` is `agent_compaction.go:274`,
  under the `// Success path: release ownership, drain the mailbox queue` block
  at `:262-273`. It is the only production caller
  (`grep -rn abandonOwnershipAndPopFirstSubmitted` returns that one plus three
  `mailbox_handoff_regression_test.go` hits and the method's own declaration).
- **`mailbox.go` really declares zero functions.**
  `grep -c "^func " internal/agent/mailbox.go` → `0`.
- **All eleven named methods are in exactly the five named files** — checked
  one by one, not as a group:
  `submit` `mailbox_ownership.go:22`, `drainOrRelease` `:80`,
  `drainOrReleaseFinal` `:208`; `interruptAndReplace` `mailbox_interrupt.go:80`,
  `drainAfterCancel` `:276`; `inject` `mailbox_inject.go:16`,
  `drainInjects` `:63`; `beginGeneration` `mailbox_generation.go:16`,
  `beginCompact` `:48`; `queue` `mailbox_queue.go:26`,
  `popFirstSubmitted` `:39`. No named method lives outside those five files.
- **The replacement text's positive claim is accurate**: `mailbox.go` declares
  `mailboxState` + the `mbIdle`/`mbOwned`/`mbReleasing` constants, the
  `generation` and `pendingInject` helper types, and the `mailbox` struct's
  fields — nothing executable.
- **Comment-only**: `git show f4ab5b6b` contains no non-comment line.

---

## Executed verification summary

| check | result |
|---|---|
| `go build ./...` (out-of-tree HEAD copy) | clean |
| `go vet ./internal/server/... ./internal/agent/...` | clean |
| `gofmt -l internal/ main.go` | empty |
| all 27 `TestHandleRerunMessage_*` / `TestRecreateRerunPromptIfLost_*` on HEAD | **all PASS**, 6.128 s |
| `go test ./internal/server/ -count=1` (whole package) | **ok**, 24.662 s |
| `go test ./internal/agent/... -count=1` | **ok**, all subpackages, 58.2 s main |
| **mutation: seed loop deleted** | exactly **1 of 27 red** — the new test, count=1 (M-2 closed, test load-bearing) |
| **mutation: union loop body deleted** | **all 27 still PASS** (M-1) |
| **probe: foreign same-text row inside the two-listing window, post-delete `List` succeeds** | HEAD count=2 PASS / union deleted count=1 FAIL — **a real prompt loss** (M-1) |
| trace: every production `Messages.Create` with `Role: message.User` | 3 sinks, 4 reachable production paths — 2 not in the comment (M-2) |
| trace: `crush sessions inject` lock behaviour | `setupApp` → `config.Init` + `db.Connect` only; `InspectSessionLock` is read-only and runs **after** the write (M-2) |
| trace: `coordinator.InjectMessage` callers | `handleInjectMessage` **and** `notifyBackgroundJobDone` (M-2) |
| count: `List` calls in the handler + helper | exactly 3 (`:704`, `:880`, `:1055`) — the new test's `listCalls == 3` precondition is exact |
| `grep -c "^func " internal/agent/mailbox.go` | **0** — `f4ab5b6b` correct |
| all 11 mailbox methods vs. the five files named in the new doc | **all match** |
| `runSummarize` + its `abandonOwnershipAndPopFirstSubmitted` call | `agent_compaction.go:107` / `:274` — `f4ab5b6b` correct |
| `grep -rn "(agent\.go)"` in `internal/agent` (non-test) | 9 hits: 7 stale, 2 correct (fantasy's own `agent.go`) — M-3 |
| `git status --porcelain` after all work | only pre-existing entries + this report |

---

## Things I could not verify, labelled as such

1. **No cross-process execution.** M-2's `crush sessions inject` path was
   traced through the code and its lock behaviour derived from
   `cmd/root.go:310-349` + `cmd/sessions_inject.go:95,208-213`; I did **not**
   run a second `crush` process against a live server, and per CLAUDE.md I did
   not invoke the CLI at all (no `CRUSH_GLOBAL_DATA` / `CRUSH_GLOBAL_CONFIG`
   were needed because nothing that reads or writes config was executed).
2. **M-1's probe drives the concurrent writer from `rerunTailDeleteSeam`**,
   faithful in ordering but not a real second WebSocket client or a second
   process.
3. **Every failed-`List` scenario is an injected `message.Service` decorator**,
   not a real `SQLITE_BUSY`.
4. **No `-race` and no `-count=N` stress sweep this round.** The sixteenth
   review discharged `-count=10 -race` over this exact subset (551 s) and the
   orchestrator re-ran `./internal/server/... -race` on `afb8372c`; this batch
   changes no production statement, so I spent the budget on mutation coverage
   instead. Both packages were run once at `-count=1` without `-race`.
5. **Windows-only.** No POSIX execution.
6. **`notifyBackgroundJobDone`'s Phase-3 branch was traced, not executed** —
   that `autoResumeEligible` returns false in realistic configurations follows
   from `coordinator.go:471-475`, not from a run.
7. **No `web/` delta in this batch**, so no Playwright run.
