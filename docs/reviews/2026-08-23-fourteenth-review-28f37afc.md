# Fourteenth review (read-only): four commits closing the thirteenth review's P2, four P3s and five Minors

- Reviewed HEAD: `28f37afc56fb85ab6e367c543b7ca93994c16512` (branch `main`)
- **Code range actually reviewed: `b0998e92..28f37afc`** — 4 commits, 8 files,
  +757 / −85, across `internal/server`, `internal/agent`, `internal/platform`,
  `main.go`.
- Predecessor: `docs/reviews/2026-08-23-thirteenth-review-b0998e92.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every probe and
  revert-check below ran on **out-of-tree copies** (`git archive <rev> | tar -x`
  into `D:\system_artefact\Temp\crush-r14` for HEAD and
  `D:\system_artefact\Temp\crush-r14-old` for `b0998e92`), never on the repo.
  `git status --porcelain` shows only entries this review did not create (plus
  this report).

## Verdict

**NO-GO**, on exactly one finding.

Three of the four commits do what they claim, cleanly. `4fae9a7f` really does
delete dead code and the replacement comment is accurate against the code it
now describes. `f9240924`'s two edits are exact (the `restartOrphaned` log is a
byte-level mirror of its sibling; `main.go`'s reworded init-order claim is now
true statement-for-statement). `3ab48011`'s guard changes close all four
false negatives, the fork-channel remedy matches the fork's actual publish
channels, and I could not construct a false positive it introduces — eleven
adversarial layouts, including the two "real install" shapes and the repo-root
build output the doc says is intentionally uncovered, all came out right.

The blocker is `28f37afc`, and it is this series' signature failure mode for
the sixth consecutive round on the same mechanism: the fix for the thirteenth
review's P2-1 duplicate **moved the duplicate rather than removing it**. Gating
the *defer* on `releaseOnBailout` genuinely closes the success path. But the
*explicit error-path call* at `handlers_agent.go:938-939` was left ungated and
switched from a row-count comparison to an **index-based** scan window, and an
index baseline is invalidated by anything that deletes a row below the
replacement turn's own prompt. In-turn compaction — the exact mechanism that
made P2-1 real — does precisely that. I reproduced a spurious duplicate prompt
against the real `handleRerunMessage`, and the same probe **passes against
`b0998e92`**: for one and two deleted rows the code this commit replaced was
correct and the replacement is not. That is a widening, not a residual.

I grade it P2 and the round NO-GO on the same rationale the thirteenth review
applied to P2-1: a newly introduced, silent, user-visible transcript defect
minted by a commit whose entire stated purpose was closing that defect. The
honest counterargument, stated rather than hidden: this one sits on the error
path and needs a second turn in the loop to reach it, so its trigger is
strictly narrower than P2-1's, and a reviewer weighing only trigger-frequency
would call it P3 and the round GO.

Beyond that, the round repeated the *comment*-half of the pattern too. The very
paragraph `28f37afc` rewrote to fix the thirteenth review's P3-2 ("no
concurrent writer can interleave", false) now carries a fresh exclusivity
claim of the same shape two dozen lines up (P3-1), and `4fae9a7f` corrected two
of the five places that describe `drainOrReleaseFinal`'s deleted `mb.stopped`
branch, leaving three (P3-3).

---

## Findings, by severity

### P2-1 (blocker) — `28f37afc`'s error-path recreate duplicates the operator's prompt whenever anything deletes a row below the replacement turn's prompt

- `internal/server/handlers_agent.go:836-842` — the index baseline (`baselineCount`)
- `internal/server/handlers_agent.go:938-939` — the explicit error-path call, **not** gated on `releaseOnBailout`
- `internal/server/handlers_agent.go:970-974` — the scan window `for i := baselineCount; i < len(allMsgs); i++`
- `internal/agent/agent_run.go:267-272` — `onHandoff` fires **before** `runOwned`
- `internal/agent/agent_turn.go:369` — `createUserMessage`, inside `runTurn`
- `internal/agent/agent_compaction.go:507-511` / `:688-693` — the two commit loops that delete summarised rows
- `internal/agent/agent_turn.go:1797-1811, 1826-1827, 1838-1842` — the two reachable "compact, then run another turn" shapes

**The mechanism.** `baselineCount` is a *position*. The scan trusts the
premise, stated verbatim in the helper's new doc at `:949-951`, that "rows at
index < baselineCount predate the commit point (survivors of a failed tail
delete)". That premise holds only if the row list is append-only from the
commit point onward. It is not: every deletion of a row *below* the
replacement turn's prompt shifts that prompt down one index. After `d`
deletions the prompt sits at index `baselineCount − d`, i.e. outside the scan
window for any `d ≥ 1`, the scan finds nothing, and `Messages.Create` appends a
second copy.

The old check this replaced (`len(allMsgs) > targetIdx`, commit `b1c1f349`)
compared *counts*, so the three rows a compacting turn adds (prompt, assistant,
summary) offset the deletions: it only misfired at `d ≥ 3`. The new check
misfires at `d ≥ 1`.

| deleted rows `d` | `b0998e92` (count check) | `28f37afc` (index scan) |
|---|---|---|
| 0 | correct (skip) | correct (skip) |
| 1 | correct (skip) | **duplicate** |
| 2 | correct (skip) | **duplicate** |
| ≥ 3 | duplicate | duplicate |

**Why the `releaseOnBailout` gate does not cover this.** The gate is on the
defer only (`:864`). The error path calls the helper directly at `:939`,
unconditionally, and it must: `onHandoff` fires at `agent_run.go:270`,
*before* `runOwned` and therefore before `createUserMessage`
(`agent_turn.go:369`), so `releaseOnBailout == false` proves only "the turn
loop started", not "the prompt exists". Gating the explicit call the same way
would trade this duplicate straight back for the B-1 prompt loss.

**Production trigger.** Compaction runs at the *end* of a turn and swallows its
own error (`agent_turn.go:1839-1841`), so a single-turn run returns `nil` after
compacting. Reaching `err != nil` after a compaction needs a second loop
iteration, and there are two ordinary ones:

1. **Auto-compaction mid-tool-use.** `shouldSummarize` → `runSummarizeBody`
   (deletes the summarised rows, `agent_compaction.go:507-511`) → pending tool
   calls → `agent_turn.go:1826-1827` returns a continuation call with
   `hasNext = true`. The continuation turn then errors (provider blip, token
   limit, lock failure). Note the continuation's prompt is *rewritten*
   ("The previous session was interrupted because it got too long…"), so it
   cannot accidentally satisfy the text match either.
2. **Queued follow-up.** Silent compaction (`:1838-1842`) → `drainOrReleaseMerged`
   pops a message the operator queued while the rerun was running → that turn
   errors.

**Verification: CONFIRMED by execution.** Out-of-tree copy of HEAD, driving the
real `handleRerunMessage` with a fake `agent.Coordinator` that fires the real
`onHandoff`, creates the prompt + reply exactly as `createUserMessage` does,
creates a summary row and deletes two older rows exactly as
`runSummarizeSilent`'s commit loop does, then returns an error:

```
=== RUN   TestR14_ErrorPathAfterCompactionDuplicatesPrompt
ERROR ws: rerun agent error err="provider stream failed"
    final[0] role=user      text="q1"
    final[1] role=assistant text="a1"
    final[2] role=user      text="rerun me"        <- the turn's own prompt
    final[3] role=assistant text="partial reply"
    final[4] role=user      text="summary of earlier conversation"
    final[5] role=user      text="rerun me"        <- SPURIOUS duplicate
        Error:      Not equal: expected: 1  actual  : 2
--- FAIL: TestR14_ErrorPathAfterCompactionDuplicatesPrompt (0.51s)
```

**Revert-check, executed by me:** the identical probe file, dropped into a
clean out-of-tree copy of `b0998e92` (the commit immediately before this fix)
and run unmodified:

```
=== RUN   TestR14_ErrorPathAfterCompactionDuplicatesPrompt
    final[0..4] ... (no sixth row)
--- PASS: TestR14_ErrorPathAfterCompactionDuplicatesPrompt (0.56s)
ok  github.com/charmbracelet/crush/internal/server  0.641s
```

So the duplicate is attributable to `28f37afc` specifically, not to the
pre-existing mechanism.

**Outcome.** The client receives `EventError` (the run really did fail), and
the browser additionally shows the operator's prompt echoed a second time below
the failed turn. The phantom row publishes a `CreatedEvent` and enters the next
turn's LLM context as a trailing user message with no reply — identical end
state to the thirteenth review's P2-1.

**Second, read-only source of the same defect.** Step 3's target delete logs
and continues on failure (`handlers_agent.go:823-825`). If it fails, the
original prompt survives at index `targetIdx` and `baselineCount` becomes
`targetIdx + 1 == len(allMsgs)`, so the scan window is empty and an error path
recreates the prompt that never went away. `b0998e92`'s count check handled
this correctly (`len > targetIdx` → skip). CONFIRMED by reading only — there is
no failure-injection seam for `message.Service.Delete`.

**Fix shape** (so round fifteen doesn't re-derive it). Stop using a position as
the watermark; it is the wrong primitive for a list that can shrink. The
handler already lists the session at `:837` — capture the **set of message IDs**
from that same list instead of just its length, and have
`recreateRerunPromptIfLost` recreate iff there is no `message.User` row with
`Content().Text == text` whose ID is *not* in that set. That costs no extra
query, is immune to deletions (compaction, concurrent `handleDeleteMessage`)
because it never depends on ordering, is still immune to unrelated concurrent
writers (text match), and still handles #644's earlier identical prompt
correctly (that row's ID *is* in the set). Keep the `releaseOnBailout` gate on
the defer as-is — it is correct and it is what closes the thirteenth review's
P2-1.

---

### P3-1 — the rewritten paragraph traded one false exclusivity claim for another, twenty-four lines up

- `internal/server/handlers_agent.go:827-832`:

```go
// Capture a precise baseline row count immediately after the target delete.
// While this handler still holds the exclusive reservation and the
// external-silence probe, no other writer can be modifying the session,
// so the list below captures the true post-delete count.
```

This is the same claim, in the same file, that the thirteenth review filed as
P3-2 ("so no concurrent writer can interleave with the list-then-create") and
that this very commit removed from `:837-839`. It is false for the same reason
and for one additional one:

| writer | gate |
|---|---|
| `handleDeleteMessage` → `deleteMessageRescuingOrphan` (`handlers_messages.go:138`) | **none** — `Messages.Delete` runs first, unconditionally; the `IsSessionBusy` + probe gate at `:161-176` is only reached for a *streaming* row |
| `handleDeleteMessages` (`handlers_messages.go:209-237`) | **none** — same helper, in a loop |
| `handleUpdateMessageContent` (`handlers_messages.go:250-281`) | **none** — rewrites a row's `TextContent` with no ownership check at all |

None of the three consults `AgentCoordinator.ReserveExclusive`,
`IsSessionBusy`, or `holdExternalSilenceProof` for a non-streaming row, and all
three are dispatched **concurrently** with `handleRerunMessage`:
`handlers.go:86-89, 108-109` route all of them through `c.dispatch`, which
`hub.go:36` bounds at `maxConcurrentHandlersPerConn = 12` *per connection* —
and the web UI is multi-client by design.

This is not merely a wrong comment; it is the stated justification for
trusting an index. Both directions bite:

- **A delete landing after the baseline capture** shifts the prompt below
  `baselineCount` — P2-1's duplicate, reached without any compaction.
- **A delete landing between step 3 and the baseline `List`** makes
  `baselineCount` one lower than the surviving-row count, so the scan window
  opens on the *last surviving pre-commit row*. If that row is a `User` message
  with the same text — the "continue"/"go on" shape task #644 exists for — the
  scan matches it, suppresses the recreate, and the operator's prompt is gone.
  That is the original B-1/N-1 loss, resurrected through the new mechanism.

**Verification: CONFIRMED by reading + call-graph** (handler bodies and the
dispatch table quoted above). Not executed: landing a foreign delete inside a
sub-millisecond window needs a seam this handler does not have, and P2-1's
probe already demonstrates the identical downstream failure.

---

### P3-2 — the baseline-fallback comment states the wrong failure direction

- `internal/server/handlers_agent.go:833-835`:

```go
// Use a sensible fallback: if listing errors, log and fall back to targetIdx
// (the subsequent positional+textual scan in recreateRerunPromptIfLost still
// decides correctly — fail-open toward recreating).
```

`targetIdx` is the *post-delete* count only when every tail delete succeeded;
whenever one failed it is strictly **lower** than the real count. A lower
baseline means a **wider** scan window, which means **more** chances to match
and therefore a bias toward *suppressing* the recreate — the exact opposite of
"fail-open toward recreating". The rows the widened window newly admits are
precisely the surviving-tail rows, which are the ones most likely to be a
duplicate of the operator's own text.

Independently: if `a.Messages.List` failed at `:837`, the *same* `List` call at
`:959` (same store, same `deleteCtx`, microseconds later) will most likely fail
too, and `recreateRerunPromptIfLost` returns at `:963` without creating
anything. So the realistic behaviour on a list error is "no recreate at all",
not "fail-open".

**Verification: CONFIRMED by reading** (`handlers_agent.go:765-791` for the
continue-on-error tail deletes, `:836-842`, `:958-974`).

---

### P3-3 — `4fae9a7f` fixed two of the five places that describe the deleted `mb.stopped` branch; three still describe it as live

`4fae9a7f` deleted `drainOrReleaseFinal`'s `if mb.stopped { … }` finalize
branch and correctly rewrote the two comments in the files it touched
(`mailbox_ownership.go:322-341`, `agent_run.go:150-154`). Confirmed: after
`mb.mu` is reacquired at `mailbox_ownership.go:310`, there is **no** read of
`mb.stopped` anywhere in the function — the only remaining read is the ENTRY
check at `:226` (`grep -n "mb.stopped" internal/agent/mailbox_ownership.go` →
`226` and one historical-note line).

Three copies elsewhere still assert the removed behaviour:

**(a) `internal/agent/mailbox.go:79-90`** — the `mbReleasing` const's own doc,
i.e. the canonical state-machine reference the other files point at. Three
false statements in one sentence:

```go
//     there is nothing left running to interrupt). `stopped` is read again
//     when drainOrReleaseFinal reacquires mb.mu to finalize: if it is now
//     set, the finalize step ends the era at mbIdle and DISCARDS (not
//     orphans — a detached restart would start a fresh provider turn while
//     the process is exiting, exactly the P0-C bug this mailbox exists to
//     prevent) anything that landed in mb.submitted/mb.replacement during
//     the release window …
//     … so drainOrReleaseFinal checks it
//     explicitly — see its own doc.
```

`stopped` is *not* read again; the finalize step *orphans* rather than
discards (since `a8b35e96`/#646); the parenthetical justification is the one
`mailbox_ownership.go:331-332` now records as having stopped being true in task
#340; and `drainOrReleaseFinal` no longer "checks it explicitly" at all.

**(b) `internal/agent/mailbox_interrupt.go:47-52`** — `hardStop`'s doc:

```go
// itself is still fully effective — drainOrReleaseFinal's finalize step
// re-reads mb.stopped after reacquiring mb.mu specifically to catch a
// hardStop that landed during the release() window (see its own doc) and
// ends the era at mbIdle instead of handing any newly-queued work back to
// the (now shutting-down) turn loop.
```

The outcome half is still true; the mechanism half ("re-reads mb.stopped …
specifically to catch a hardStop that landed during the release() window") is
now the description of deleted code. A hardStop landing in the release window
is now handled by *nothing in particular* — the finalize step behaves the same
way regardless — which is fine, but is the opposite of what this says.

**(c) `internal/agent/mailbox_queue.go:57-60`**:

```go
// This method deliberately does NOT consult mb.stopped: the hard-stop
// latch's job is to stop TURN-LOOP drains from handing a shutting-down
// process another provider turn (drainAfterCancel, drainOrReleaseFinal's
// finalize step, interruptAndReplace).
```

This is the *identical* misstatement `4fae9a7f` corrected one file over at
`agent_run.go:151-154` ("drainOrReleaseFinal's ENTRY check … since #646 its
finalize step is latch-blind in effect"). The list should name the entry check,
not the finalize step.

**Verification: CONFIRMED by reading**, and by the absence of any `mb.stopped`
read after `mailbox_ownership.go:310` (grepped, quoted above).

---

## Minor items

- **M-1 — `handlers_agent.go:851-853` is stale in two ways and repeats the
  premise P2-1 disproved.** The paragraph still reads *"Idempotent and harmless
  on non-error paths: the positional check finds nothing to recreate once the
  replacement turn's own createUserMessage has run."* The check is no longer
  positional (it is positional **and** textual, per the same commit's own
  rewrite at `:948`), the defer no longer runs on non-error paths at all (it is
  gated on `releaseOnBailout` at `:864`), and "finds nothing to recreate once
  createUserMessage has run" is exactly the assumption compaction breaks —
  which is P2-1. The commit rewrote the four lines below this sentence and left
  the sentence itself untouched.
- **M-2 — `28f37afc` narrows `#645`'s panic protection.** `onHandoff` fires at
  `agent_run.go:270`, before `runOwned` and therefore before
  `createUserMessage`. A panic in the window between them —
  `TryAcquireSessionLockWithOptions`, `withActivityNotify`, `sessions.Get`,
  `getSessionMessages` — now unwinds with `releaseOnBailout == false`, so the
  recreate defer no-ops and the prompt is lost. Before `28f37afc` the defer's
  count check would have caught it (`len(allMsgs) <= targetIdx`). The #645
  regression test only drives `rerunPreHandoffSeam`, which is *before*
  `onHandoff`, so nothing detects the narrowing. Recorded rather than filed
  because a panic in that specific window is not a demonstrated shape — but the
  ID-set fix proposed under P2-1 restores the coverage for free.
- **M-3 — `readModuleLine`'s doc overstates the (d2) behaviour.**
  `source_tree_guard.go:157` and `3ab48011`'s commit message both say a go.mod
  that exists but has no module line is "treated as a module boundary (stop the
  walk)". True *above* the marker directory (`:100-104` breaks), false **at**
  the marker: `:91-97` takes the `checkDir == ancestor` branch and `continue`s
  upward. `internal/platform/source_tree_guard_test.go`'s own subtest "empty
  go.mod at marker continues up" asserts the real behaviour, so only the doc is
  wrong.
- **M-4 — a shipped test carries unedited scratch-pad reasoning.**
  `source_tree_guard_test.go`, subtest `"different go.mod above dev/ stops
  walk"`: a nine-line comment block reasoning aloud in the second person
  ("But the actual case: … Actually, let's test a simpler case: … Correct
  test: …"), and a `devDir` created with `os.Mkdir` at `tmpDir/dev` that is
  then abandoned when `devDir` is reassigned to `interposed/dev`. The assertion
  is right; the surrounding material is not review-grade.
- **M-5 — `recreateHandled = true` is set before the call it guards**
  (`handlers_agent.go:938-939`). If `recreateRerunPromptIfLost` panics there
  (it can: `Messages.List`/`Create` on a closed DB), the defer will not retry.
  Trivial — the defer's own ordering is fine — but the flag would be more
  honestly set *after* the call on the explicit path.

## Observations (no finding)

- **`releaseOnBailout`'s lifecycle is exactly as the commit describes, and is
  race-free.** Declared at `handlers_agent.go:649`; the only writer is the
  `func() { releaseOnBailout = false }` closure passed at `:920`/`:922`; the
  only invoker is `agent_run.go:270-272`, once, synchronously, on the caller's
  goroutine, after a successful `rebindDispatcher`. `coordinator_run.go:626-654`
  passes it straight through and its three early-error returns are all above
  the agent layer, so `onHandoff` cannot fire on them. No goroutine boundary is
  crossed, so `go test ./internal/server/ -run TestHandleRerunMessage -race`
  (40.2 s, clean) is a meaningful confirmation rather than a coincidence. The
  one thing the commit's own prose glosses: `releaseOnBailout == false` means
  "the turn loop started", **not** "the prompt exists" — see M-2 and P2-1.
- **The text half of the scan is sound.** `coordinator.buildCall`
  (`coordinator_run.go:105`) sets `Prompt: prompt` verbatim and
  `createUserMessage` (`agent_prompt.go:73`) writes
  `TextContent{Text: call.Prompt}` as the first part, while the handler's
  `text` comes from `targetMsg.Content().Text` and `Content()`
  (`message/content.go:187-194`) returns the first `TextContent` — so the value
  round-trips exactly. Two rows both matching at/after the baseline is handled
  correctly too: the scan `return`s on the first match, so it can only
  under-create, never double-create.
- **Reruns still drop attachments**, pre-existing and untouched here: the
  handler passes only `text` into `RunWithReservedOwnership` (`:920`/`:922`), so
  a rerun of a message that carried `BinaryContent` parts recreates it without
  them. Not introduced by this batch; noted because the text-only scan makes it
  slightly more visible.
- **`4fae9a7f` is a genuinely clean deletion.** The replacement comment at
  `mailbox_ownership.go:322-341` is accurate against the code: the entry check
  at `:226` is the only `mb.stopped` consumer, `hasNext` is hard-`false` on the
  release path, and the finalize step's drain order (`:355-362`) mirrors the
  live branches (`:247-275`) exactly as the doc claims. The full
  `internal/agent` package is green (55.7 s).
- **`3ab48011` survived a false-positive hunt.** Eleven layouts, all correct:
  repo-root build output uncovered (matching the new M-3 doc note), npm layout
  under a plain `dev/` (with and without a foreign go.mod at `dev/`), the real
  `dev/` scratch build, repo-local and foreign `.claude/worktrees` checkouts,
  an unparseable go.mod *above* the marker still stopping the walk, `DEV`
  uppercase, and a home directory literally named `dev` with the crush clone
  *below* it. The one behaviour change worth naming: with P3-5(a)'s fix, a
  foreign `go.mod` at the marker no longer stops the walk, so a binary under
  `<crushrepo>/dev/` is now refused even when `dev/` is its own module —
  intended, and it does not leak into any install shape I could build.

---

## What I checked and found sound

### `f9240924` — both edits exact

- `agent_ownership.go:258-263` is a byte-level mirror of the sibling call site
  at `:311-317`: same message string, same `session_id`/`num_calls`/`err`
  fields, same `slog.Error` level. Closes M-5 at the level it was filed.
- `main.go:27-32`'s reworded claim is now true statement-for-statement: the
  guard is the first statement of `main()` (`:33`), and `AssignToNewJobObject`
  (`:38`), the pprof listener (`:42-49`) and `cmd.Execute()` (`:51`) all follow
  it; the only things ahead of it are the package `init()`s the comment now
  names explicitly (`_ "github.com/joho/godotenv/autoload"` at `:23`,
  `_ ".../internal/dns"` at `:21`). Closes M-1.

### `3ab48011` — P3-4 and all four P3-5 sub-items

- **P3-4:** `npm install -g @phpcraftdream/crush` matches `README.md:11,15,640`
  and `.github/workflows/publish-fork-npm.yml:261`; `go run deploy.go` matches
  `deploy.go:39`. The upstream `go install github.com/charmbracelet/crush@latest`
  line is gone.
- **P3-5(a)** — foreign go.mod at the marker: `:91-97` continues instead of
  breaking. Verified true for `<crushrepo>/dev/go.mod` declaring a stray module.
- **P3-5(b)** — worktrees: `:41-64` anchors on the worktree's own directory
  when it has a go.mod. Verified true for a crush worktree under a non-crush
  `.claude` parent.
- **P3-5(c)** — `strings.EqualFold` at `:28, 30, 32, 41`. Verified for `DEV`.
- **P3-5(d)** — tab after `module` at `:170`, and `readModuleLine` returning
  `("", nil)` at `:175` rather than `os.ErrNotExist` (doc caveat: M-3).
- **M-4 (thirteenth review)** — the refusal string at `:152` no longer ends in
  `\n`, so `main.go:34`'s `Fprintln` supplies the only newline.
- Neither loop can spin: the outer `continue` at `:33` still advances via
  `dir = filepath.Dir(dir)` and the inner `continue` at `:96` via
  `checkDir = filepath.Dir(checkDir)`.

### `28f37afc` — what it does close

- **The thirteenth review's P2-1 is genuinely closed on the success path.** The
  defer's `releaseOnBailout && !recreateHandled` gate at `:864` means a
  successful handoff can never reach the recreate, whatever compaction did to
  the row count. `TestHandleRerunMessage_SuccessWithCompactionDoesNotDuplicatePrompt`
  is load-bearing for the gate specifically, not merely for the new scan: its
  fixture leaves `baselineCount == 2` and exactly 2 final rows, so the scan
  window is empty and the baseline change alone would still duplicate.
- **The thirteenth review's P3-1 source (1) is closed.** An unrelated
  concurrent writer's row cannot satisfy the text match.
  `TestHandleRerunMessage_ConcurrentUnrelatedWriterDoesNotSuppressRecreate` is
  likewise load-bearing (`baselineCount == 0`, the foreign row inside the
  window, text mismatch forces the recreate).
- **#644's earlier-identical-prompt case still works** and
  `TestHandleRerunMessage_PromptRecreatedDespiteEarlierIdenticalPrompt` still
  passes: the earlier "continue" lives below the baseline.
- **#645's pre-handoff panic protection is intact**
  (`TestHandleRerunMessage_PanicBeforeHandoffRecreatesPrompt` green) —
  `releaseOnBailout` is still `true` through an unwind that never reaches
  `onHandoff`. Caveat M-2 for the window *after* it.

### Executed verification summary

| check | result |
|---|---|
| `go build ./...` (out-of-tree copy of HEAD) | clean |
| `gofmt -l internal/ main.go` (real repo, read-only) | empty |
| `go vet ./internal/server ./internal/agent ./internal/platform .` | clean |
| `go test ./internal/server/ -count=1` | ok, 23.2 s |
| `go test ./internal/platform/ -count=1` | ok, 0.33 s |
| `go test ./internal/agent/ -count=1` | ok, 55.7 s |
| `go test ./internal/server/ -run TestHandleRerunMessage -race -count=1` | ok, 40.2 s |
| probe: error path + in-turn compaction (HEAD) | **reproduced** — duplicate prompt (P2-1) |
| revert-check: same probe against `b0998e92` | **PASS** — so P2-1 is a widening, not a residual |
| probe: 11 source-tree-guard layouts (false-positive/negative hunt) | all 11 correct |
| grep: `mb.stopped` reads after `mailbox_ownership.go:310` | none — confirms P3-3 |

---

## Things I could not verify, labelled as such

1. **P2-1's production compaction trigger.** The duplicate is reproduced
   against the real `handleRerunMessage` with a fake coordinator performing the
   same row operations `runSummarizeSilent`/`runSummarizeBody` perform. That a
   real session reaches `shouldSummarize`/`silentCompactNeeded` *and then* fails
   a second loop iteration is derived from `agent_turn.go:1066, 1537, 1797-1842`
   and the arithmetic above, not measured against a live provider.
2. **P2-1's second source** (a failed step-3 target delete) and **P3-1's
   concurrent-writer window** are read, not executed — there is no
   failure-injection seam for `message.Service.Delete` and no seam that lets a
   test land a foreign `handleDeleteMessage` inside the baseline window.
3. **M-2's panic window** is reasoned from `agent_run.go:270-272` +
   `runOwned`'s body, not induced.
4. **The `-race` sweep this series still owes was not run.** I ran `-race` only
   over `TestHandleRerunMessage*`; the eleventh review's owed
   `-count=20`/`-race` sweep remains owed. I did not observe either of the two
   documented flakes
   (`TestP342_SecondManualCompactCoalescedAfterFirstCompletes`,
   `TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_UsesPlainAbandon`)
   in any run here and did not re-litigate them.
5. **Windows-only.** Every measurement is Windows. The guard's POSIX behaviour
   (including whether `filepath.EvalSymlinks` canonicalises case, which is what
   makes P3-5(c) latent rather than live) is read, not executed.
6. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was read or written, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed.
7. **No web/Playwright run.** This batch contains no `web/` delta.
