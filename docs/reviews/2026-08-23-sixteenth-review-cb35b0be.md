# Sixteenth review (read-only): the never-nil seed, one comment-only doc fix, and no eighth residual in the code

- Reviewed HEAD: `cb35b0be838d0df22054ae614ca37d14e371f817` (branch `main`)
- **Code range actually reviewed: `889a2092..cb35b0be`** — 2 commits, 3 files,
  +338 / −53 in `internal/server`, plus a comment-only delta in
  `internal/agent/mailbox_queue.go`.
- Predecessor: `docs/reviews/2026-08-23-fifteenth-review-889a2092.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every probe,
  mutation and revert-check below ran on **out-of-tree copies**
  (`git archive <rev> | tar -x` into `D:\system_artefact\Temp\crush-r16`,
  `…\crush-r16-old` for `889a2092`, `…\crush-r16-revert`,
  `…\crush-r16-mut`, `…\crush-r16-mut2`), never on the repo.
  `git status --porcelain` at the end shows only entries this review did not
  create (plus this report).

---

## Verdict

**GO**, with three Minors and **zero P-level findings**. Nothing blocking.

**I could not find an eighth residual in the code.** I attacked `cb35b0be`
along every angle the brief named and two it did not, by execution rather than
by reading, and the mechanism held in all of them:

- The union direction is **monotone toward recreating, never toward
  suppressing**. Adding an ID to `baselineIDs` can only flip a row from
  qualifying to non-qualifying (`(!inBaseline || m.ID == targetID)`), i.e.
  toward `Create`. A wider baseline can therefore never cause a *silent loss*
  — only, at worst, a duplicate.
- The union can never mark a genuinely-new row as old. That would require an
  ID present in the pre-delete listing, absent from the post-delete listing,
  and present again at scan time — i.e. a deleted row resurrected under the
  same ID. Message IDs are `uuid.New().String()`
  (`internal/message/message.go:298`) and the only two production
  `createUserMessage` call sites (`agent_control.go:90`, `agent_turn.go:369`)
  both mint fresh ones. There is no ID-preserving insert path into a live
  session's message table.
- `allMsgs` really is a pre-delete snapshot. It is assigned exactly once
  (`handlers_agent.go:704`) and read at `:715`, `:765` and `:868-871`; the
  tail-delete loop iterates it but does not mutate it, and no reassignment
  exists in the function (`grep -n "allMsgs" internal/server/handlers_agent.go`
  — the only other binding is the helper's own local at `:1047`).
- The commit-point invariant still holds after the edit: between step 3's
  delete (`:823`) and `runReturned = true` (`:976`) there is **no**
  `return`/`panic`/`goto` outside comments, so nothing can bypass the recreate
  bookkeeping.
- The union is also strictly **safer** than the post-delete-only set under a
  lagging read. `List` reads a *separate* read-only pool
  (`internal/message/message.go:143`, `internal/db/connect_modernc.go:33-42`,
  WAL, `_txlock=deferred`). If that pool ever omitted a row that genuinely
  exists, the old design would classify it "new" (able to suppress → loss);
  the seeded design classifies it "old" (recreate → at worst a duplicate).
- The revert-check reproduces independently: the new handler-level test fails
  against `889a2092` with **count=2**, passes on HEAD.
- `-count=10 -race` over the whole 26-test rerun suite: **green, 551 s** —
  this discharges the stress sweep the series has owed since the eleventh
  review, at least for this subsystem.

The three Minors are all documentation/test hygiene. Two of them are about the
same paragraph of the new comment; one is pre-existing staleness that
`c674fcdd`'s new text now routes readers through. **None of them indicates a
code defect, and none of them changes the GO.** I file them because the brief
explicitly asked for a verdict on the test-quality question and because M-1's
"microseconds" figure is the stated justification for accepting a deliberate
residual — and it is measurably wrong by three to six orders of magnitude.

`c674fcdd` is exactly what it claims and is correct statement-for-statement
(see "What I checked and found sound").

---

## Findings, by severity

### P1 / P2 / P3 — none.

---

## Minor items

### M-1 — the residual gap's own justification calls a window "microseconds-wide" that I measured at 1.1 ms to 427 ms, and the same comment block never names the writer that gap is about

- `internal/server/handlers_agent.go:863-867` — the residual-gap paragraph
- `internal/server/handlers_agent.go:830-833` — the writer enumeration

The new capture-site comment closes with:

```go
// The residual gap is a row created by a concurrent
// writer BETWEEN the two listings: on this failed-List path it is in
// neither set, so a same-text foreign row could suppress the
// recreate — the same concurrent-writer tolerance the helper's doc
// documents, in a microseconds-wide window.
```

The two listings are `:704` (the seed source, captured for step 2) and `:872`
(the post-delete capture). Between them the handler runs **the entire
tail-delete loop** — one `Messages.Get` + `DeleteMessageIfTerminal` +
`PublishMustDeliver` per tail row (`internal/message/message.go:187-249`) —
plus step 3's target delete. That is O(N) database round trips, not two
adjacent statements.

**Measured, on HEAD, out-of-tree** (`TestR16_WindowBetweenTheTwoBaselineListings`:
a decorator timestamps every `List` call; the delta between calls #1 and #2 is
the window):

| tail rows | window |
|---|---|
| 1 | **1.10 ms** |
| 50 | **22.2 ms** |
| 200 | **99.7 ms** |
| 800 | **426.8 ms** |

≈ 0.53 ms per tail row, and ≈ 1.1 ms even in the *minimum* case of a single
tail row. Rerunning an early message in a long session — the single most
common reason to rerun at all — puts this in the hundreds of milliseconds.
"Microseconds-wide" is off by 3 orders of magnitude at the floor and 5–6 at
the realistic case. (The phrase is inherited verbatim from the fifteenth
review's own fix-shape prescription, so this is a shared inaccuracy, not a
worker invention.)

**Second half of the same item.** The comment block enumerates the unguarded
concurrent writers at `:830-833` as `handleDeleteMessage`,
`handleDeleteMessages` and `handleUpdateMessageContent` — all three of which
*delete or edit* rows; none of them can **add** one. The residual-gap
paragraph 30 lines below is entirely about a writer that adds a row. The only
unguarded adder is `handleInjectMessage` → `coordinator.InjectMessage` →
`sessionAgent.InjectMessage` → `createUserMessage`
(`internal/agent/agent_control.go:89-98`), which takes no ownership check and
is dispatched concurrently. `cb35b0be` itself added that name to the *helper's*
doc (its M-2 fix, `:1023-1024`); leaving it out of the capture site's own
enumeration makes the reader guess which writer the residual is about. I
verified the adder set is exactly `{handleInjectMessage}`: `Run` reserves
before any row is written (`agent_run.go:29-71`, `tryReserveSession` precedes
`runOwned`), so a pumped/queued run cannot land a row in this window.

**What is NOT wrong here.** The residual itself is real, and benign in exactly
the sense the fifteenth review's M-2 accepted. **CONFIRMED by execution**
(`TestR16_ForeignSameTextRowInsideTheTwoListingWindowSuppressesRecreate`:
post-delete `List` fails, a same-text User row is created inside the
tail-delete loop, the replacement turn fires `onHandoff` and errors without
writing a prompt):

```
HEAD (cb35b0be):  final[0] role=user text="rerun me"   <- the FOREIGN row
                  matching user rows = 1
889a2092:         final[0] role=user text="rerun me"   <- the FOREIGN row
                  final[1] role=user text="rerun me"   <- the recreate
                  matching user rows = 2
```

So on this narrow path HEAD suppresses where the predecessor duplicated. The
operator's exact words are still on screen either way, which is precisely why
the fifteenth review graded this outcome class benign for the success path —
I grade it the same here for consistency, and file only the mischaracterised
window size and the missing writer name.

**Fix shape:** replace "in a microseconds-wide window" with something true
("a window that spans the whole tail-delete loop — milliseconds for a short
tail, hundreds of milliseconds for a long one"), and name
`handleInjectMessage` at `:830-833` alongside the three
`handlers_messages.go` handlers, flagged as the only one that can *add* a row.

---

### M-2 — nothing in the 26-test suite pins the SEED's CONTENT; deleting the seed loop keeps every test green while silently reintroducing #644's prompt loss

- `internal/server/handlers_agent.go:868-871` — the seed loop
- `internal/server/p655_rerun_idset_regression_test.go:480` —
  `TestRecreateRerunPromptIfLost_PreDeleteOnlyBaselineStillScans`
- `internal/server/p655_rerun_idset_regression_test.go:634` —
  `TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt`

This is the brief's vacuousness question, and my answer is **worse than the
one asked about**: the problem is not just that the rewritten unit test fails
to distinguish this round's change — it is that *no* test in the suite
distinguishes a correct seed from an empty one.

**Mutation, executed.** Delete only the three-line seed loop, leaving
`baselineIDs := make(map[string]struct{}, len(allMsgs))` (still non-nil, so
the helper still scans) and the post-delete union intact:

```
go test ./internal/server/ -run 'TestHandleRerunMessage_|TestRecreateRerunPromptIfLost_'
ok  github.com/charmbracelet/crush/internal/server  6.639s      <- ALL 26 GREEN
```

The suite is fully green on a handler that has lost the entire point of
`cb35b0be`. In particular
`TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt`
still passes, because the row that suppresses in *its* scenario is the
replacement turn's own prompt row, which is outside the seed anyway — so that
test pins "the set is non-nil / the helper scans", not "the set contains the
pre-delete IDs".

**The empty-seed variant is a genuine defect, not a hypothetical.** With an
empty baseline on the failed-post-delete-List path, every *pre-existing* row
reads as "new", so #644's exact shape — an earlier identical prompt sitting
above the rerun target — suppresses the recreate and the operator's rerun
prompt is silently lost. **CONFIRMED by execution**
(`TestR16_SeedContentIsLoadBearing_EarlierIdenticalPromptOnFailedListPath`:
`[earlier User "rerun me", target User "rerun me", tail]`, post-delete `List`
fails, the turn fires `onHandoff` and errors before `createUserMessage`):

```
HEAD (cb35b0be):      final[0] "rerun me"  final[1] "rerun me"   -> count=2  PASS
seed loop deleted:    final[0] "rerun me"                        -> count=1  FAIL
                      "the operator's rerun prompt was silently lost"
```

(That probe passes against `889a2092` too — its nil baseline created
unconditionally — so it pins the seed against a *future* regression, not
against the predecessor. That is exactly the coverage the suite is missing.)

**On the narrower question the brief asked.** I agree with the orchestrator's
characterisation and go one step further:

- `TestRecreateRerunPromptIfLost_PreDeleteOnlyBaselineStillScans` passes
  unmodified against `889a2092`'s handler — **verified by executing it there**
  — so it does not distinguish this round's change. Its own doc says so, in
  full and accurately, which keeps it from being *misleading*.
- Its stated revert-check *does* hold: mutating the helper to skip the scan
  and create unconditionally reddens it with count=2 — **verified**.
- But behaviourally it is a near-duplicate of its sibling
  `TestRecreateRerunPromptIfLost_NewPromptRowSuppressesDespiteEarlierIdentical`
  (`:428`). The only difference is two extra map keys (`target.ID`, `tail.ID`)
  for rows that have already been deleted, and a `targetID` argument that names
  one of them. Both are provably inert: the helper only consults membership
  for rows its own `List` returns, so an ID with no row can neither suppress
  nor admit anything. Net, the rewrite lost the coverage the old
  `_NilBaselineCreatesUnconditionally` had (it pinned a branch that no longer
  exists — fair) and added none.

**Fix shape:** add the handler-level regression above (it is 90 lines and
needs no new seam — a `List` decorator that fails call #2 plus a coordinator
that fires `onHandoff` and errors). Optionally fold
`_PreDeleteOnlyBaselineStillScans` into its sibling, or leave it; it costs
nothing and its doc is honest.

Severity: **Minor**, per the brief's own ceiling for test-quality items. It is
not a code defect — HEAD is correct. It is the item on this page most worth
acting on, because it is the one that would let round seventeen's edit
reintroduce a known-fixed loss with a green suite.

---

### M-3 — two stale location pointers in the mailbox docs, one of them on the reading path `c674fcdd` newly creates

- `internal/agent/mailbox_queue.go:114-115` — "(agent.go)"
- `internal/agent/mailbox.go:20-24` — "this file implements the mailbox methods (…)"

`c674fcdd` is correct (see below), and its new text redirects the reader:
"moved to the atomic `abandonOwnershipAndPopFirstSubmitted` below **(see its
own doc)**". That doc, 75 lines down, says:

```go
// abandonOwnershipAndPopSubmitted above: runSummarize's manual-compaction
// success path (agent.go) needs exactly popFirstSubmitted's existing
```

`runSummarize` is `internal/agent/agent_compaction.go:107`, and its
`abandonOwnershipAndPopFirstSubmitted` call is `agent_compaction.go:274`.
Nothing about it is in `agent.go`. This is not drift after the fact: I checked
the pointer against the tree **at the commit that wrote it** (`73387062`,
2026-08-18) — `runSummarize` and the call site were already in
`agent_compaction.go` there, because `ee922fda` ("split agent.go into files by
responsibility") is an ancestor of `73387062`. It was born stale. Pre-existing
and outside this batch's range; filed only because `c674fcdd` — a commit whose
entire purpose was making this one doc name the right caller — now points
readers at it.

Second, in the same package: `mailbox.go:20-24` still says "this file
implements the mailbox methods (submit, drainOrRelease, drainOrReleaseFinal,
interruptAndReplace, drainAfterCancel, inject, drainInjects, beginGeneration,
beginCompact, queue, popFirstSubmitted) with no behavior deviation."
`grep -c "^func " internal/agent/mailbox.go` returns **0** — the file declares
types and constants only, and all eleven named methods live in five other
files (`mailbox_ownership.go`, `mailbox_interrupt.go`, `mailbox_inject.go`,
`mailbox_generation.go`, `mailbox_queue.go`). Also pre-existing (same split);
noted because `7d8cdd6c` edited a comment 60 lines below it last round and
`c674fcdd` edited its sibling file this round.

---

## Observations (no finding)

- **The residual named in the fix's own comment is reachable, and I reached
  it** — see M-1's execution transcript. What the comment gets wrong is only
  its size, not its existence or its (benign) consequence.
- **The union changes nothing on the success path, for the reason the comment
  gives.** The seed's "extra" IDs relative to the post-delete listing are
  exactly the rows the tail/target deletes removed; a row that predates the
  run and still exists at scan time was necessarily in both listings. The
  comment's alternative enumeration ("or, when a tail delete failed,
  pre-existing rows") describes rows that are in the post-delete set too, so
  they are not actually "extra" — harmless redundancy, not an error.
- **The `-count=10 -race` stress sweep is finally discharged for this
  subsystem**: 26 rerun tests × 10 iterations under `-race`, `ok … 551.118 s`,
  no flake, no race. The full `internal/server` package under `-race` is also
  clean at 163.5 s (the commit message claims 153 s; same order, different
  machine load).
- **`recreateRerunPromptIfLost` still drops attachments** (`:966`/`:968` pass
  only `text`) — pre-existing, untouched here, carried forward from the
  fourteenth and fifteenth reviews.
- **The M-1 doc rewrite in `cb35b0be` is itself accurate.** Its claim that the
  `#655` reordering protects only the pre-handoff error returns checks out:
  at the explicit call site `runReturned` is already true, so the defer's gate
  `(releaseOnBailout || !runReturned)` collapses to `releaseOnBailout`, which
  `onHandoff` has cleared on the dominant path and left true on
  `RunWithReservedOwnership`'s genuine pre-handoff returns — of which there
  are four (`ErrEmptyPrompt`, `ErrSessionMissing`, `ErrAgentShuttingDown`, the
  `rebindDispatcher` refusal), all above the handoff line at
  `agent_run.go:270-272`.

---

## What I checked and found sound

### `cb35b0be` — the fifteenth review's P3-1, M-1, M-2

- **P3-1 closed, revert-checked independently.** `handlers_agent.go` swapped
  back to its `889a2092` content in a clean out-of-tree copy with the new test
  file kept: `TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt`
  fails there with `expected: 1 / actual: 2` and the exact P3-1 warning line in
  the log; it passes on HEAD. The test exercises the real defect.
- **The nil branch and the unconditional `Create` are genuinely gone.**
  `recreateRerunPromptIfLost` now scans unconditionally
  (`handlers_agent.go:1057-1062`); there is no `baselineIDs == nil` read
  anywhere in the file.
- **The seed can never be unavailable.** If the pre-delete `List` at `:704`
  fails, the handler returns at `:706` — before the commit point and before
  the capture — so there is no path on which `baselineIDs` is built from
  nothing.
- **`targetIdx != -1` is enforced before the seed is built** (`:721-728`),
  so `targetMsg.ID ∈ allMsgs` unconditionally, which is what makes the
  comment's "the target's own ID is in the seed unconditionally" true and
  keeps the `|| m.ID == targetID` disjunct meaningful.
- **M-1 and M-2 doc corrections are accurate** — verified against the code,
  see the Observations above and the `handleInjectMessage` trace in M-1.
- **All 26 `TestHandleRerunMessage_*` / `TestRecreateRerunPromptIfLost_*`
  tests green on HEAD**, including every prior round's (`#614`, `#630`, `#638`,
  `#644`, `#645`, `#651`, `#655`).

### `c674fcdd` — the fifteenth review's M-3

Checked against the code, not against the commit message:

- **"No production caller uses this directly today"** — correct.
  `grep -rn "popFirstSubmitted" internal/ --include=*.go` returns exactly one
  call: `ownership_handoff_test.go:325`. Every other hit is a comment or a
  test-name/doc reference.
- **"runSummarize's manual-compaction success path moved to the atomic
  `abandonOwnershipAndPopFirstSubmitted`"** — correct.
  `agent_compaction.go:274` inside `runSummarize` (`:107`) is the only
  production call, and `agent_compaction.go:267-273` records the same
  reordering rationale independently.
- **"which closes a reordering gap a separate `abandonOwnership()` +
  `popFirstSubmitted()` pair would reopen (see its own doc)"** — correct;
  `mailbox_queue.go:110-126` states exactly that (modulo M-3's stale file
  pointer inside it).
- **"Exercised directly by unit tests of the queue mechanics in isolation from
  ownership state"** — true of the *method* (it pops regardless of mailbox
  state) and true in spirit of the test, though the test itself builds real
  ownership state via a `Run` + `Cancel` first and then simulates the
  success path with a plain `abandonOwnership`. Singular vs. plural ("unit
  tests" for one caller) is the only slack, and the commit message states it
  precisely. Not filed.
- **Comment-only**: `git show c674fcdd` contains no non-comment line.
  `go test ./internal/agent/... -count=1` green across all subpackages
  (59.3 s main).

---

## Executed verification summary

| check | result |
|---|---|
| `go build ./...` (out-of-tree HEAD copy) | clean |
| `gofmt -l internal/ main.go` (real repo, read-only) | empty |
| `go vet ./internal/server/... ./internal/agent/...` | clean |
| all 26 rerun tests on HEAD | **all PASS** |
| `go test ./internal/server/... -count=1 -race` (HEAD copy) | **ok, 163.5 s** |
| `go test ./internal/server/ -count=10 -race -run 'TestHandleRerunMessage_\|TestRecreateRerunPromptIfLost_'` | **ok, 551.1 s** — stress sweep discharged |
| `go test ./internal/agent/... -count=1` | ok, all subpackages |
| revert-check: new handler test vs `889a2092` | **FAIL, count=2** — exercises the real defect |
| revert-check: `_PreDeleteOnlyBaselineStillScans` vs `889a2092` | **PASS** — does not distinguish this round (M-2) |
| mutation: helper skips the scan entirely | `_PreDeleteOnlyBaselineStillScans` FAILS count=2 — its own doc's claim holds |
| **mutation: seed loop deleted (empty non-nil map)** | **all 26 tests still PASS** (M-2) |
| probe: seed content load-bearing (#644 shape on failed-List path) | HEAD count=2 PASS / empty-seed count=1 FAIL — the mutation is a real loss |
| probe: window between the two listings, tail = 1/50/200/800 | **1.10 / 22.2 / 99.7 / 426.8 ms** (M-1) |
| probe: foreign same-text row inside that window, failed post-delete List | HEAD suppresses (1 row) / `889a2092` duplicates (2 rows) — benign either way |
| grep: `allMsgs` reassignment in `handleRerunMessage` | none — assigned once at `:704` |
| grep: `return`/`panic`/`goto` between `:823` and `:976` | none outside comments |
| grep: production `createUserMessage` callers | `agent_control.go:90` (InjectMessage), `agent_turn.go:369` (post-ownership) |
| grep: `popFirstSubmitted` callers | one, `ownership_handoff_test.go:325` |
| `grep -c "^func " internal/agent/mailbox.go` | **0** (M-3) |
| `git status --porcelain` after all work | only pre-existing entries + this report |

---

## Things I could not verify, labelled as such

1. **M-1's window measurements are Windows + SQLite-on-local-NTFS, single
   process, no WebSocket subscribers attached.** A real deployment with a
   backed-up subscriber could be slower still (`PublishMustDeliver` blocks up
   to 50 ms per subscriber, `internal/pubsub/broker.go:45`), but that needs
   >4096 undrained events (`bufferSize`, `:39`) to trigger, so I did not model
   it and do not claim it.
2. **Every failed-`List` scenario is driven by an injected `message.Service`
   decorator**, not by a real `SQLITE_BUSY`. That such a failure is transient
   and that the helper's own `List` runs much later are derived from the code,
   not measured against a contended database.
3. **The concurrent-inject window is executed at the handler level**, with the
   foreign row created from inside `rerunTailDeleteSeam` — faithful in
   ordering, but not a real second WebSocket client.
4. **The `-count=10 -race` sweep covers the rerun subset only** (26 tests). The
   rest of `internal/server` got one `-race` pass at `-count=1`;
   `internal/agent` got `-count=1` without `-race`.
5. **Windows-only.** No POSIX execution.
6. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was read or written, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed.
7. **No `web/` delta in this batch**, so no Playwright run.
