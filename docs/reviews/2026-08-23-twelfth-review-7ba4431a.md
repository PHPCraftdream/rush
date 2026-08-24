# Twelfth review (read-only): six commits closing the eleventh review's two blockers and three P3s

- Reviewed HEAD: `7ba4431af916f3c1a9f4fd67f255a2f0ce0619df` (branch `main`)
- **Code range actually reviewed: `28d55c33..a3b204df`** — 6 commits, 18 files,
  +692 / −92. `7ba4431a` landed mid-review; verified **docs-only**
  (`git show --name-only 7ba4431a | grep -v '^docs/'` → empty), so nothing in
  it affects any conclusion below.
- Predecessor: `docs/reviews/2026-08-23-eleventh-review-28d55c33.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every
  revert-check and every probe below ran on an **out-of-tree copy of HEAD**
  (`git archive HEAD | tar -x` into a temp directory), never on the repo.
  `git status --porcelain` before and after this review shows the same single
  pre-existing entry (` D web/dist/.gitkeep`).

## Verdict

**GO**, with five P3s and three Minors, none of which I could escalate to P2.

The five eleventh-review findings are genuinely closed at the level they were
filed: `75b972a4` gives `StatErr` a real destructive-path reader and fails
closed (B-2, F-3), `907b6111` closes the ordinary-error-path prompt loss that
made B-1 a blocker, `e6fcb14a` fixes F-5 **and the orchestrator's
second-order self-correction landed cleanly — no third-order version slipped
through**, and `a3b204df`'s "outcome (b)" reasoning chain — the one item I was
asked to scrutinise hardest — **holds under independent tracing**: I verified
every link (`restartOrphanedWithRetry` → durable `session_run_queue` row;
`tryAdmitRunWg` refusing pump-driven `Run`s; `App.Shutdown` stopping the pump
before the DB close) and reproduced the stated revert-check exactly. I could
not construct a case where adding the `mb.stopped` refusal the old comment
implied would be correct.

What keeps this from being a clean pass is that the batch repeated the series'
own signature pattern twice more. `907b6111` fixed the path it targeted but its
commit message's unqualified claim ("their words no longer vanish with it") is
false on two other paths, one of which I reproduced with the exact end state
B-1 was blocked for (**session with zero messages**, N-2) and one of which
fires on an ordinary prompt the operator merely typed twice (N-1). And
`a3b204df` — a commit whose entire purpose was to reconcile a comment with its
code — canonised "a durable enqueue during teardown is fine" in `agent_run.go`
while leaving three other sites in the same package asserting the opposite,
two of which I proved false by execution (N-3), and left a third paragraph
inside the very doc comment it rewrote directly contradicting its own new one
(N-4).

I grade N-1/N-2 P3 rather than P2 because in both the *recovery* posture is
strictly better than the state B-1 described: in N-1 the identical text is
still on screen one message up, and N-2 needs a panic, not an ordinary error.
An operator who reads "their words no longer vanish" as a hard guarantee should
override me and call N-1 a P2 — I state the counterargument explicitly rather
than hide the judgement call.

---

## Findings, by severity

### N-1 (P3, highest) — `907b6111`'s recreate is suppressed by any earlier identical user message; the reran prompt is then silently dropped

- `internal/server/handlers_agent.go:899-905` (the `promptExists` scan)

The scan matches **any** `message.User` row anywhere in the session whose
`Content().Text` equals the captured `text`. Step 2 only deletes messages
*after* the target, so every earlier message survives — including an earlier
copy of the same prompt. For short prompts an agent-tooling operator types
repeatedly ("continue", "go on", "yes", "try again", "fix it"), the collision
is routine, not exotic.

**Failure scenario (concrete).** History `[U1 "continue", A1, U2 "continue", A2]`.

1. Operator hits Rerun on `U2`.
2. Steps 1/1a/1b succeed. Tail delete removes `A2`. Step 3 deletes `U2`.
3. `RunWithReservedOwnership` fails early (unresolvable `smart_model_id` — the
   same trigger B-1 named).
4. The error block lists the session, finds `U1` with text `"continue"`, sets
   `promptExists = true`, and **skips the recreate**.
5. Final state: `[U1 "continue", A1]`. The turn the operator asked to rerun is
   gone from the transcript, and the row they would retry from no longer exists.

**Verification: CONFIRMED by execution.** In an out-of-tree copy of HEAD I
drove the real `handleRerunMessage` against the batch's own
`earlyReturnCoordinator` fake with the history above:

```
ws: handleRerunMessage sessionID=493dd252… contentPreview=continue
ERROR ws: rerun agent error err="model resolution failed: smart model not found"
    remaining: role=user      id=0564d827… text="continue"     <- U1
    remaining: role=assistant id=b1efca1e… text="first reply"  <- A1
    u1=0564d827… u2=92f7fdda…
    expected: 2   actual: 1
```

`U2` is gone and nothing recreated it. (The probe file was written only into
the out-of-tree copy.)

**Fix shape** (so the next round doesn't re-derive it): the handler already
knows `targetIdx`, and every message from `targetIdx` onward was deleted, so
anything present at index ≥ `targetIdx` on the error path is necessarily new.
Replacing the text match with `if len(newList) <= targetIdx { recreate }` is
positional, collision-proof, and one line shorter. (`targetIdx` is computed at
`handlers_agent.go:713-719` and is still in scope at the error block.)

---

### N-2 (P3) — the recreate is not defer-protected, so the pre-handoff panic window the codebase already has a seam for still reproduces B-1's exact end state

- `internal/server/handlers_agent.go:889-916` (recreate lives lexically inside
  `if err != nil { … }`, not in a defer)
- `internal/server/handlers_agent.go:870-871` (`rerunPreHandoffSeam`, which
  fires **after** step 3's delete and after `probe.Release()`)
- `internal/server/p623_panic_window_test.go:57` — the existing test that
  drives exactly this window and asserts only that the *reservation* is
  released, never that the prompt survives

`releaseOnBailout` and `probeHeld` are both defers, so a panic between step 3
and the handoff correctly releases the reservation and the probe. The prompt
recreate is not — a panic unwinds straight past it into `hub.runRecovered`
(`internal/server/hub.go:228-238`), leaving the session empty.

**Verification: CONFIRMED by execution.** Out-of-tree copy of HEAD, arming
`rerunPreHandoffSeam` to panic (mirroring `p623_panic_window_test.go`'s own
setup) and mirroring `runRecovered`'s recover:

```
ws: handleRerunMessage sessionID=505d371b… contentPreview="words I would hate to lose"
    Should NOT be empty, but was []
```

Zero messages — the literal end state the eleventh review filed B-1 for
(*"session with zero messages, prompt text unrecoverable"*), just reached
through the panic door instead of the model-resolution door.

Not P2: reaching it requires a panic in `hub.Broadcast`, the coordinator
wrapper, or the agent's pre-handoff body, not an ordinary error. But the fork
already treats this window as real enough to have built `onHandoff` and a
dedicated seam for it, so "panics don't happen here" is not this codebase's own
position. Fix shape: move the recreate into a defer guarded by the same flag
`onHandoff` flips (`releaseOnBailout`), so it fires on exactly the returns that
did not reach the handoff.

---

### N-3 (P3) — `a3b204df` canonised "a durable enqueue during teardown is fine" in one file while three sites in the same package still say the opposite; two of those are provably false since task #340

- `internal/agent/agent_run.go:139-156` (the new paragraph: the caller's
  release "pops any queued work and durably enqueues it … That is acceptable
  (and arguably better than the alternative)")
- `internal/agent/mailbox_interrupt.go:27-42` (`hardStop`'s doc): *"So on
  shutdown a prompt that never reached a turn IS lost, with only a slog.Error
  to show for it. … Making this genuinely lossless needs the queue to be
  durable, which is a design change well beyond a shutdown latch — tracked
  separately, not papered over here."*
- `internal/agent/mailbox_interrupt.go:283-287` (`drainAfterCancel`'s stopped
  branch): *"a queued call is NOT a DB row, so an unstarted prompt is genuinely
  lost when the process exits. Leaving it is the lesser evil, not a save."*
- `internal/agent/mailbox_ownership.go:321-341` (`drainOrReleaseFinal`'s
  finalize step) — **discards** raced-in work, justified by *"starting a fresh
  provider turn while the process is trying to exit is precisely the P0-C bug
  … restarting it as 'orphaned' would be just as wrong"*

`a3b204df`'s commit message states its whole justification as: *"A durable
enqueue during teardown does not violate the 'don't start a fresh provider
turn while the DB is tearing down' trade **the hardStop doc actually
states**."* The `hardStop` doc states two things — the trade (correct, and the
fix's reading of it is right) **and** a factual claim that queued work is lost
on shutdown. The second half has been false since task #340, and it is exactly
the half that would have told a maintainer the abandon-path enqueue is not an
anomaly.

**What is actually true.** `drainAfterCancel` refuses on `stopped` and leaves
`submitted` in place → `runTurn` returns early (`agent_turn.go:1785-1794`,
`drainOrReleaseMerged` never reached) → `runOwned`'s
`defer abandonOwnershipWithHandoff` (`agent_run.go:290-292`) fires with a
**still-matching epoch** (`hardStop` never bumps it) → pop → durable enqueue.

**Verification: CONFIRMED by execution (probe 1).** Out-of-tree copy: a real
`sessionAgent.Run` blocked in `Stream`, a real `QueueMessage` of a follow-up,
a real `CancelAll`:

```
CancelAll stillBusy=false
Run returned err=call already attempted: context canceled (isCancel=true)
durable enqueues: [851eeb92-…-probe-queued-followup]
```

The prompt `hardStop`'s doc says is lost was durably enqueued.

**And the behaviour is asymmetric.** The *other* shutdown path — the turn
completing normally inside the shutdown window, the documented P0-A race —
reaches `drainOrReleaseFinal` with `stopped` latched and **silently discards**
the same queued prompt.

**Verification: CONFIRMED by execution (probe 2).** Direct mailbox-level
exercise of both discard branches:

```
--- PASS: .../stopped_at_entry              (orphaned == nil, submitted cleared)
--- PASS: .../hardStop_lands_during_release() (orphaned == nil, submitted cleared)
```

So: same shutdown, same queued prompt, **durably preserved if the turn was
cancelled, silently destroyed if it happened to finish**. Nothing about that is
new in this batch — but `a3b204df` is the commit that decided the durable
enqueue is correct, and it did so without reconciling the two comments that say
it is wrong or the one code path that does the opposite. Whoever owns this
should now decide deliberately whether the `drainOrReleaseFinal` discard is
still wanted, given its stated justification ("would start a fresh provider
turn") is factually obsolete.

Verification of the reasoning chain itself, which **does** hold:
`restartOrphanedWithRetry` is a synchronous `EnqueueRunQueueEntry` with an
orphan-outbox fallback (`agent_ownership.go:383-541`); `Run`
(`agent_run.go:47`) and `Summarize` (`agent_compaction.go:54`) both gate on
`tryAdmitRunWg`; `App.Shutdown` calls `CancelAll` → `RunQueuePump.Stop()` →
only then closes the DB (`app_lifecycle.go:24-56`). One gap in the *stated*
argument, not in the code: the pump ordering it cites does not protect the
enqueue **write** itself, which happens after `runWg.Done()` and therefore
races `App.Shutdown`'s DB close with only the pump's ≤5 s `Stop()` as slack.
Outcome on a lost race is a logged failure, not corruption — same posture as
everywhere else in this layer.

---

### N-4 (P3) — `a3b204df` rewrote two paragraphs of one doc comment and left a third, ten lines below, asserting the opposite

- `internal/agent/agent_run.go:177-180` (untouched by `a3b204df`):

```go
// Any early return BEFORE onHandoff fires will trigger both the agent-level
// early release (in this function) AND the caller's still-armed defer — the
// second release is a verified epoch-guarded no-op (idle mailbox, empty queues,
// spent CancelFunc).
```

Every clause of that sentence is false for the `mb.stopped` rebind branch that
`a3b204df`'s new paragraph at `:139-156` is entirely about:

| claim | reality on the stopped branch |
|---|---|
| "the agent-level early release (in this function)" | there is none — that is the whole point of the exception |
| "epoch-guarded no-op" | the epoch **matches** (`hardStop` doesn't bump it), so nothing short-circuits |
| "idle mailbox" | `mbOwned` — pinned by the new test's own `require.Equal(t, mbOwned, state)` |
| "empty queues" | the new test queues one call and asserts it is popped and enqueued |

`a3b204df`'s own `p641_mbstopped_rebind_test.go:123-136` is the proof that the
caller-side release on this branch is the opposite of a no-op. The batch
therefore left, inside the single doc comment it was rewriting, the exact class
of divergence it exists to eliminate — the fourth round running.

Secondary, same paragraph family: `:166-170` says the caller "must not call
ReleaseExclusive itself after calling this method", which reads as forbidding
precisely the caller behaviour `:141-143` now depends on. `:177-180` is what
was supposed to reconcile the two, and it no longer does.

**Verification: CONFIRMED by reading + the batch's own test.**

---

### N-5 (P3, minor end) — the corrected sibling-branch comment enumerates what `abandonOwnershipWithHandoff` does during teardown and omits the one thing on that path that would be a provider turn

- `internal/agent/agent_run.go:206-212`: *"'restarting' queued work means
  durably enqueueing it via restartOrphanedWithRetry, not starting a provider
  turn … **With no queued work it just flips the era to mbIdle.**"*
- Contradicted by `internal/agent/agent_ownership.go:309-330`

`abandonOwnershipWithHandoff` does a **third** thing: it drains
`summarizeQueue` and spawns `go a.Summarize(context.Background(), …)`.
`summarizeQueue` is unconditionally non-nil (`agent.go:723`), so this runs on
every call, including on a hard-stopped mailbox. `Summarize` *is* a provider
turn; it is harmless here only because it gates on `tryAdmitRunWg`
(`agent_compaction.go:54`) and returns `ErrAgentShuttingDown`. A paragraph
whose job is "here is why this call is safe post-shutdown-latch" omitting the
single operation on that path that the admission gate is load-bearing for is a
real gap, not a stylistic one.

**Verification: CONFIRMED by reading + call-graph.**

---

## Minor items

- **M-1 — `b2109db6` minted a comment-vs-code divergence in the file it
  changed.** `web/src/components/ChatToolbar.tsx:228-229` still reads *"only
  genuinely session-bound controls are hidden individually (Compact below;
  **Prompt is disabled via its own `disabled={!activeSessionID}`**; …)"*. The
  same commit replaced that with conditional rendering
  (`ChatToolbar.tsx:333` — `{activeSessionID && (`), so the Prompt button is
  now *absent*, not *disabled*, when no session is active. The commit message
  says "Every data-test-id on the moved buttons is unchanged" — true of the
  ids, silent about the disabled→unmounted semantics change. (The behaviour
  change itself is fine and the spec was updated for it:
  `web/tests/system-prompt.spec.ts:46-49`.)
- **M-2 — half-finished stale-comment cleanup in `web/tests/ui.spec.ts`.**
  `b2109db6` deleted one copy of the "ChatToolbar returns null with no active
  session" claim, and left two others: `ui.spec.ts:35` (*"ChatToolbar.tsx has
  `if (!activeSessionID) return null;` (line ~195) … That gating is a real,
  confirmed regression"*) and `ui.spec.ts:201`. Both were made false by
  `34f80494` (2026-08-19), whose fix is documented in
  `ChatToolbar.tsx:220-230` as *"No `if (!activeSessionID) return null;` here —
  deliberately."* Pre-existing, but this batch touched the file and removed the
  third copy, so the list-based cleanup pattern the series itself diagnosed
  struck again.
- **M-3 — `internal/server/p595_delete_streaming_test.go:272-273`.**
  `907b6111` rewrote the assertion and the comment directly above it, but left
  the comment two lines higher: *"Verify the session is now empty (or has only
  the recreated message if the run proceeded further than we care about)."* The
  session is now never empty on this path, by design.

## Observations (no finding)

- **The recreate runs outside the reservation.** By the time
  `handlers_agent.go:889` executes, both the coordinator layer
  (`coordinator_run.go:628,640,647`) and the agent layer
  (`agent_run.go:187,192,213`) have already called `ReleaseExclusive` on every
  early-return path except the stopped-rebind one, so the mailbox is `mbIdle`
  and a concurrent `handleSendMessage` can become owner while the handler
  lists-then-creates. Worst case is an appended user row landing after a newly
  started turn's assistant row (confusing ordering, picked up as context next
  turn) — not destructive. Inferred from reading; not reproduced.
- **The startup-recovery guard still fails open when `dataDir == ""`**
  (`app_recovery.go:106`). B-2's own principle ("could not look" ≠ "looked and
  found nothing") applies to "could not resolve where to look" too, and
  `holdExternalSilenceProofFromConfig:128-130` refuses outright for exactly
  that condition. The recovery comment (`app_recovery.go:49-51`) is honest that
  this is degenerate/test-only, and `config.setDefaults` always resolves a
  directory in production, so I am not filing it.
- **Flake lead, offered not asserted.** I reproduced
  `TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_UsesPlainAbandon`
  failing under a full `-race` package run (`ownership_handoff_test.go:313`,
  "must have 3 calls in submitted: expected 3, actual 0"), and confirm the
  orchestrator's "pre-existing, documented, unrelated to `a3b204df`"
  classification: the test's own comment (`:270-289`) names the same prior CI
  failure, and `a3b204df` touched neither that file nor any non-comment code
  outside the nil-by-default seam. One datum for whoever eventually
  investigates: my N-3 probe demonstrates that
  `abandonOwnershipWithHandoff`'s pop **clears `mb.submitted` and durably
  enqueues it whenever the epoch still matches** — which is a concrete
  mechanism by which `len(mb.submitted)` can read 0 at `:313` after the first
  `Run` exits. That is a lead, not a conclusion; I did not confirm it is *the*
  mechanism.

---

## What I checked and found sound

### `a3b204df`'s "outcome (b)" chain — the highest-risk item, and it holds

Traced independently, link by link, not from the commit message:

- **`restartOrphanedWithRetry` is a durable enqueue, not a turn.**
  `agent_ownership.go:383-541`: synchronous (`wg.Wait` before return),
  `EnqueueRunQueueEntry` with a `LogicalCallID`-derived idempotency key,
  `WriteToOrphanOutbox` fallback on failure, error surfaced. `restartOrphaned`
  is a thin alias (`:348-350`). Confirmed by execution — the p641 test's own
  recorder captures the real enqueue.
- **The shutdown admission gate really does cover the pump.**
  `tryAdmitRunWg` (`agent_control.go:108-116`) is checked by `Run`
  (`agent_run.go:47`), `RunWithReservedOwnership` (`:200`) and `Summarize`
  (`agent_compaction.go:54`); the pump reaches all of them through
  `coordinatorAdapterImpl.Run` → `RunSessionAgentCall` (`app.go:52-67`), and
  `CancelAll` latches `shuttingDown` under `admitMu` **before** anything else
  (`agent_control.go:125-127`). A refused execution Nacks the durable row
  rather than terminal-failing it (`run_queue_entry_exec.go:699`; only
  `ErrCallAlreadyAttempted` is terminal), so the row survives to a later
  process.
- **`App.Shutdown` stops the pump before closing the DB.**
  `app_lifecycle.go:24-56`: `CancelAll` → `RunQueuePump.Stop()` → DB cleanup.
- **`hardStop` does not bump the epoch** (`mailbox_interrupt.go:58-63`), which
  is precisely why the caller's later `ReleaseExclusive` is not a no-op — the
  property the whole conclusion rests on.
- **The named "real teardown protections" all genuinely consult the latch:**
  `drainAfterCancel` (`mailbox_interrupt.go:288`), `drainOrReleaseFinal`
  (`mailbox_ownership.go:225` at entry **and** `:337` at finalize — the comment
  names only the finalize step, a harmless understatement), and
  `interruptAndReplace` (`mailbox_interrupt.go:84`).
- **`abandonOwnershipAndPopSubmitted` really does not consult `mb.stopped`**
  (`mailbox_queue.go:77-104`) — the eleventh review's F-4 premise, re-verified.

**Revert-check, executed by me:** adding `|| mb.stopped` to
`abandonOwnershipAndPopSubmitted`'s epoch guard in an out-of-tree copy turns
**exactly one** test red across the whole `internal/agent` package —
`TestRunWithReservedOwnership_MbStoppedInRebindWindow_CallerReleaseStillEnqueuesQueuedWork`,
with the predicted message `"[]" should have 1 item(s), but has 0`. The commit
message's revert-check claim is exact, not approximate.

The new seam is genuinely test-only and inert: `testReserveRebindSeam` has no
non-test assignment anywhere in `internal/` (grep), and the entire non-comment
delta to `agent_run.go` in this range is the four-line nil-guarded call —
`Run`'s own body is untouched.

### `907b6111` — the path it targeted is closed, and there is no duplication hazard

- `text` is `targetMsg.Content().Text` (`handlers_agent.go:557`) and reaches
  `createUserMessage` **verbatim**: `coordinator.RunWithReservedOwnership` →
  `buildCall` sets `Prompt: prompt` with no transform
  (`coordinator_run.go:103-105`), and `createUserMessage` puts
  `TextContent{Text: call.Prompt}` **first** in `Parts`
  (`agent_prompt.go:73-78`), which is exactly what `Message.Content()` returns
  (`internal/message/content.go:187-194`). So the false-negative direction
  ("the run created it but the scan can't recognise it") does not exist — the
  match is byte-exact by construction. Attachments are not a factor: the rerun
  path passes none.
- The `ExistingMessageID` skip (`agent_turn.go:365`) cannot apply to a rerun —
  `buildCall` never sets it.
- No double-execution hazard: the recreate writes a DB row and nothing runs it;
  the turn has already failed.
- The recreated row publishes a `CreatedEvent`, so the web client sees the
  prompt reappear alongside the error.
- `deleteCtx` (`context.WithoutCancel(holdCtx)`) is correct for both the list
  and the create — no deadline, survives a cancel past the commit point.
- The `p595` assertion change is honest: that test's coordinator fails before
  `createUserMessage`, so exactly one recreated user message is the correct
  post-state, and the old `require.Empty` genuinely did encode the bug.

### `75b972a4` / `5a939df0` — B-2 and F-3 fully closed

`InspectSessionLock`'s `StatErr` now has a real production reader on the one
consumer where fail-open cost data (`app_recovery.go:119`), and all three
copies of the false "only diagnostics read it" claim are corrected
(`lock.go:942-950`, `lock.go:987-990`, `lock_test.go:771-776`).
`grep -rn StatErr internal/` shows no surviving diagnostics claim anywhere.

**Revert-check, executed by me:** restoring `if st.Live` in the out-of-tree
copy turns **exactly** `TestRecoverInterruptedTurns_StatError_FailsClosed` red
(with the orphan stamped `Process restarted`, i.e. the real clobber), while all
nine sibling `TestRecoverInterruptedTurns_*` tests stay green. Matches the
commit message precisely. The NUL-byte forgery produces a genuine non-ENOENT
error on this platform (`invalid argument`), asserted through the production
entry point before the recovery run.

### `e6fcb14a` — F-5 closed, and the second-order self-correction landed cleanly

The committed comment (`handlers_agent.go:677-683`) reads:

> probe is never nil here: the refuse branch above (the only path
> holdExternalSilenceProof can return a nil probe on) already returned before
> this defer is registered, and TryHoldSessionLockShared opens with os.O_CREATE
> …

I checked this for a third-order version of the same defect and found none.
Enumerated every return in `holdExternalSilenceProof` /
`holdExternalSilenceProofFromConfig` (`handlers_agent.go:115-143`): there are
five, four return `(nil, true, msg)`, and the single `refuse == false` return
is `return probe, false, ""` guarded by `err == nil` from
`TryHoldSessionLockShared` — which has exactly one non-error exit, and it
returns a real probe (`lock.go:913-947`). So "the only path that can return a
nil probe is the refuse branch, and it returns before the defer is registered"
is exactly true, and the comment correctly demotes `Release`'s nil-safety to
"defensive, not load-bearing" rather than attributing it to a reachable path.
The two carried-over Minors are also gone: no `fmt.Printf` survives in
`internal/agent`/`internal/server` tests, and `agent_ownership.go:198` now
points at `mailbox_interrupt.go`, where `drainAfterCancel` actually lives.

### `b2109db6` (web) — sanity pass

- **Outside-click / Escape logic is sound.** The listeners are registered only
  while open and torn down in the effect's cleanup
  (`ChatToolbar.tsx:157-176`). Outside-click is `mousedown`, item activation is
  `onClick`; because the popover renders **inside** the same `moreMenuRef`
  container as the trigger, the item's own mousedown is never "outside", so
  the two can't race — the commit's stated rationale is accurate. Re-clicking
  the trigger while open closes via the toggle, not via a double-close.
- **The `data-test-id`s the specs rely on all exist**: `header-more-button`,
  `header-prompt-button`, `header-mcp-button`, `header-providers-button`,
  `header-default-models-button`, `header-settings-button`, and the newly-added
  `header-logs-button`. All four specs gained an `openMoreMenu` helper that
  waits on `header-logs-button` being visible rather than sleeping.
- **The ModelSelector mirroring is faithful and, if anything, better.**
  `ModelSelector.tsx:120-227` uses `useState` + `useRef` + a document
  `mousedown` listener; the More menu adds an Escape handler `ModelSelector`
  does not have. It does *not* copy `ModelSelector`'s viewport-position
  recalculation (`resize`/`scroll` listeners), which is correct — this popover
  is CSS-positioned (`absolute bottom-full`), not portalled with computed
  coordinates.
- **A11y gap, noted not filed** (matches the pattern being mirrored): no
  `aria-haspopup` / `aria-expanded` / `role="menu"` / arrow-key navigation.
  `ModelSelector` has none either, so this is a pre-existing house style
  decision rather than a regression this commit introduced. Tab still reaches
  every item; Escape closes.

### Regression spot-checks against the ninth/tenth reviews (two, per instructions)

- **`Run()` byte-identical to base — still holds.** The entire non-comment
  delta to `agent_run.go` across `28d55c33..a3b204df` is the four-line
  `testReserveRebindSeam` guard inside `RunWithReservedOwnership`.
- **`externalSessionOwnerPID` / `externalSessionOwnerRefusal` still absent**
  from `internal/` — the tenth review's third Minor stays closed.
- Bonus: the probe defer / `probeHeld` structure at
  `handlers_agent.go:684-689` and the single non-defer release at `:862-863`
  are unchanged (comment-only diff), so the eleventh review's "probe leak
  paths: none" finding is untouched.

### Executed verification summary

| check | result |
|---|---|
| `go build ./...` | clean |
| `gofmt -l internal/` | empty |
| `go vet ./internal/...` | clean apart from the pre-existing `csync.JSONSchemaAlias` lock-by-value warning |
| `go test ./internal/agent/ -count=1` | ok, 53.8 s |
| `go test ./internal/server/ ./internal/app/ ./internal/session/ -count=1` | ok (25.9 / 26.7 / 43.8 s) |
| `go test -race ./internal/server/ ./internal/app/ -count=1` | ok (138.5 / 83.0 s) |
| `go test -race ./internal/agent/ -count=1` | 1 failure: the documented pre-existing `ManualCompactionSuccess_UsesPlainAbandon` flake (434.6 s) |
| `go test ./internal/agent/ -run '…MbStoppedInRebindWindow…' -count=3 -race` | ok, 3/3 |
| revert-check: `\|\| mb.stopped` added to `abandonOwnershipAndPopSubmitted` | exactly 1 test red, package-wide, with the predicted message |
| revert-check: `if st.Live` restored in `app_recovery.go` | exactly `StatError_FailsClosed` red, 9 siblings green |
| probe: rerun failure with a duplicate earlier prompt | reproduced — reran message deleted, not recreated (N-1) |
| probe: panic in the pre-handoff window | reproduced — session left with 0 messages (N-2) |
| probe: `CancelAll` with queued work | reproduced — durably enqueued, contradicting `hardStop`'s doc (N-3) |
| probe: `drainOrReleaseFinal` stopped branches | reproduced — queued work discarded, both branches (N-3) |

---

## Things I could not verify, labelled as such

1. **N-1 and N-2's *production* trigger.** Both handler-side end states are
   executed against the real `handleRerunMessage`. That a real misconfigured
   model drives `resolveSessionModels` to an error, and that a real panic
   occurs in the pre-handoff window, are inferred from
   `coordinator_run.go:610-613` and from `p623_panic_window_test.go`'s
   existence, not measured in production.
2. **N-3's asymmetry is proven at the mailbox level, not end to end.** The
   durable-enqueue half is a full `Run` + `CancelAll`; the discard half is a
   direct `drainOrReleaseFinal` call. Driving a real turn into the
   normal-completion-inside-shutdown window needs the P0-A race the codebase
   documents but has no deterministic seam for on this path.
3. **N-4 is read, not executed.** It is a claim about a comment; the branch it
   mis-describes is executed by `p641_mbstopped_rebind_test.go`, which is what
   makes the mismatch demonstrable.
4. **The enqueue-vs-DB-close race noted under N-3 is not reproduced.** It needs
   a shutdown landing inside the handler's bail-out defer.
5. **Flake depth.** One `-race` full-package run of `internal/agent`,
   `internal/server`, `internal/app` each; no `-count=6`/`-count=20` sweep. The
   eleventh review's owed `-count=20` run is still owed.
6. **Windows-only.** Every run is Windows (`LockFileEx`). The POSIX half of
   `tryLockFileShared` is read, not executed — unchanged from the eleventh
   review.
7. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was read or written, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed.
8. **The web change was not run.** No `pnpm typecheck`, `build`, or Playwright
   run — the orchestrator reports all four affected specs (72 tests) plus
   typecheck and build green, and I reviewed the source and specs statically
   only.
