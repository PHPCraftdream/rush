# Ninth review (read-only): the seven-commit circle closing the eighth review

- Reviewed HEAD: `3b2664da8a789fce6e026da6f3294fc7ed0949ce` (branch `main`)
- Range: `a70d1b73..HEAD` — 7 commits, 43 files, +4648 / -443
- Closes: `docs/reviews/2026-08-20_07-28-07-readonly-release-review-a70d1b73.md`
  (F1–F6 plus the P2 block)
- Method: full read of the changed production paths and their new tests,
  plus **executed** verification: package tests, cross-compiles, `go vet`,
  `gofmt`, and five purpose-written probes run in a throwaway git worktree.
- Working tree: untouched. `git status` before and after this review is
  byte-identical to the expected baseline (`D web/dist/.gitkeep`, `?? dev/`,
  `?? docs/checkpoints/2026-08-20-0810.md`,
  `?? docs/reviews/2026-08-20_07-28-07-…md`). This report file is the only
  addition.

## Verdict

**NO-GO for a stable-release label — but the seven commits are a clear net
improvement and must stay.**

F1–F6 are genuinely closed for the shapes the eighth review described, and I
could not find a ninth way to reach a false `DrainComplete`: after this circle
`DrainComplete` is reachable from exactly one `return` in the whole function,
and that return is now gated. What blocks the label is different in kind from
the previous eight rounds. First, F5 is closed only *in-process*: the new
reservation proves nothing about a different `crush` process holding the same
session's OS lock, yet the code comment and the force-delete branch beneath it
now read as if ownership had been proven, so web rerun can still force-delete
a message another process is actively streaming (F-1 below, the same
transcript-corruption outcome F5 named). Second, #614 introduced a new
permanent-wedge window: a panic between `releaseOnBailout = false` and
`runOwned`'s defer leaves the session at `mbOwned` forever, and unlike the
hypothetical #616 §1 was written against, the recovery boundary that makes
this observable **already exists** in `hub.go` (F-2). Third, this circle's own
recurring second-place defect is not fixed but continued: I found six more
places where a doc comment claims a property the code does not provide —
including two in `internal/server` that still present the F1 defect *as the
design*, in the very files #612 rewrote to remove it, and one test comment that
asserts the opposite of what its own code leaves behind (verified by probe).

## Findings, by severity

### F-1 (P1) — F5 is closed only in-process; web rerun can still force-delete another process's live message

- `internal/server/handlers_agent.go:467-496` (idle poll),
  `:510-545` (`ReserveExclusive` + `releaseOnBailout` defer),
  `:593-611` (tail delete + `ForceDelete` rescue)
- `internal/agent/agent_ownership.go:122-131` (`ReserveExclusive` →
  `mailbox.beginCompact`)
- `internal/server/handlers_messages.go:127-168` (the same rescue shape on the
  ordinary delete path)

Mechanism. Everything `handleRerunMessage` uses to prove the session is not
being written to is **in-process mailbox state**: `AgentCoordinator.Cancel`,
`ClearQueue`, `IsSessionBusy`, and `ReserveExclusive` all resolve to
`a.mailboxes[sessionID]` inside this one process. The inter-process guard for
this session — `session.TryAcquireSessionLockWithOptions` — is acquired only
much later, inside `runOwned` (`internal/agent/agent_run.go:277-311`), i.e.
*after* the tail has already been deleted. `internal/server` never consults
`session.InspectSessionLock` on this path (it uses it only for the session
list, `handlers_sessions.go:169,192`).

Failure sequence.

1. `crush run --session S` (a different process) is mid-turn, holds S's OS
   lock, and has an assistant row in the DB with no terminal `Finish` part.
2. In the web UI the operator hits rerun on an earlier user message of S.
3. `Cancel(S)` / `ClearQueue(S)` hit this process's empty mailbox — no-ops for
   the foreign turn. `IsSessionBusy(S)` returns false. The idle poll succeeds
   on its first iteration.
4. `ReserveExclusive(S)` succeeds — `beginCompact` sees `mbIdle` in *this*
   process.
5. The tail loop reaches the foreign, still-streaming row. `Messages.Delete`
   returns `ErrMessageStillStreaming` (the guard is a pure DB predicate:
   "does this row have a terminal finish part", `internal/message/message.go:187-234`).
6. `handlers_agent.go:595-606` treats that exactly as the crashed-orphan case
   and calls `ForceDelete` — deleting a row the other process is still
   writing.

The comment at `:596-600` states the false inference explicitly: *"the session
has been cancelled and waited for idle, so this is an orphaned row from a
crashed/killed turn that will never receive a terminal Finish."* Neither
clause is true across processes.

This is not a regression from #614 — the same in-process-only proof backs
`deleteMessageRescuingOrphan` and predates this circle. It is a
previously-unnamed half of F5 that #614's own comment (`:510-528`) now
implicitly claims to have closed ("guaranteeing no concurrent
Send/Rerun … can slip through and start writing a new streaming assistant
message while this handler deletes the tail below").

Verification: **traced, not reproduced.** I read every guard on the path and
grepped `internal/server` for any session-lock consultation (none on this
path). I did not stage two live processes against one session — that needs a
harness this review had no mandate to build.

Cheap containment, if the operator wants it before a fix: in the
`ErrMessageStillStreaming` branch, consult `session.InspectSessionLock(dataDir,
sessionID, …)` — the function already exists in this package — and fail closed
when a live external holder is reported, instead of force-deleting.

### F-2 (P2) — a panic after the rerun handoff line wedges the session at `mbOwned` forever, and the recovering boundary already exists

- `internal/server/handlers_agent.go:536-545` (`releaseOnBailout` + defer),
  `:640` (`releaseOnBailout = false`)
- `internal/agent/coordinator_run.go:621-650`
- `internal/agent/agent_run.go:141-200` (the handoff line at `:199`),
  `:227-229` (`runOwned`'s releasing defer)
- `internal/server/hub.go:143-153`, `:228-239` (`runRecovered`)

Mechanism. `handleRerunMessage`'s bail-out defer is the only thing releasing
the reservation until `runOwned` registers its own defer. `releaseOnBailout`
is cleared at `:640`, but `runOwned`'s defer is not registered until several
frames later — after `hub.Broadcast`, `readyWg.Wait`, `applyModelOverrides` /
`resolveSessionModels`, `buildCall` (which does a DB read for the session
system prompt), prompt/attachment validation, `tryAdmitRunWg`,
`context.WithCancel` and `rebindDispatcher`. A panic anywhere in that span
unwinds past a defer that has been told to stand down and past `runOwned`,
which was never entered — so `abandonOwnershipWithHandoff` never runs, the
mailbox stays `mbOwned`, and `IsSessionBusy(S)` returns true for the rest of
the process's life. Every subsequent Send, Rerun, InterruptAndSend and
`ReserveExclusive` for S is refused.

Why it matters more than #616 §1. The panic-safety fix in
`run_queue_drain_session.go:855-888` was justified against a *hypothetical*
future outer recover ("A panic here currently crashes the whole process …
but if recovery is ever added at any outer boundary"). Here that boundary is
already installed and unconditional: `handleRerunMessage` runs as a
`workItem` under `runRecovered`, which recovers and logs. So the process does
not die — it survives with one permanently-busy session.

Fix shape: keep a second, epoch-scoped safety defer, or hand off release only
once `runOwned` has actually registered its own defer (e.g. have
`RunWithReservedOwnership` accept an "armed" flag the caller flips from inside
the callee).

Verification: **traced, not reproduced.** I confirmed by reading that (a)
`runRecovered` recovers `item.fn` panics, (b) `handleRerunMessage` is
dispatched through the work queue (`handlers.go:108`), and (c) no defer between
`:640` and `runOwned:227` releases the epoch. I did not inject a panic.

### F-3 (P2) — `rebindDispatcher` ignores the `stopped` latch that its sibling `beginCompact` honours

- `internal/agent/mailbox_generation.go:85-94` vs `:48-60`
- `internal/agent/agent_control.go:118-160` (`CancelAll`)

`beginCompact` refuses when `mb.state != mbIdle || mb.stopped`.
`rebindDispatcher` checks only `mb.epoch != epoch || mb.state != mbOwned`. So
once `CancelAll`'s per-mailbox `hardStop()` sweep has latched a mailbox closed,
`RunWithReservedOwnership` still rebinds and proceeds into `runOwned`, starting
a fresh turn loop during shutdown. `Run`'s equivalent path fails closed,
because `tryReserveSession`→`submit` does consult `stopped`.

`CancelAll` sets `a.shuttingDown` *before* the sweep, so `tryAdmitRunWg` closes
most of this window; what is left is the interleaving where `tryAdmitRunWg`
succeeds first and the sweep lands between it and `rebindDispatcher`. The
consequence is bounded by `CancelAll`'s 5s grace (it returns `stillBusy=true`
and the process forces shutdown over a live turn).

Verification: **confirmed by executed probe** (throwaway worktree, since
removed):

```
REVIEW9 rebindDispatcher after hardStop() => true (mb.stopped=true, state=1)
REVIEW9 beginCompact  after hardStop() => ok=false
```

The doc at `mailbox_generation.go:80-84` ("Returns false (a safe no-op) if the
era has since moved on") does not cover this case.

### F-4 (P2) — `Cancel(sessionID)` landing during the rerun hold is silently swallowed, and the replacement turn starts anyway

- `internal/agent/agent_ownership.go:122-131` (`_, holdCancel :=
  context.WithCancel(ctx)` — the derived context is discarded)
- `internal/agent/agent_control.go:14-46` (`Cancel` targets
  `mb.current.cancel`)
- `internal/agent/agent_run.go:178-192` (`rebindDispatcher` overwrites it)

From `ReserveExclusive` returning to `rebindDispatcher` running,
`mb.current.cancel` and `mb.dispatcherCancel` both point at `holdCancel`, whose
context nobody holds a reference to. A `Cancel(S)` in that window invokes a
non-nil `CancelFunc` that cancels nothing observable, is then *overwritten* by
`rebindDispatcher`, and the replacement turn proceeds. From the operator's
side: pressing cancel while rerun is deleting history has no effect at all, and
the new turn still starts.

Verification: **confirmed by executed probe** — `a.ReserveExclusive(callerCtx,
"s1")`, then `a.Cancel("s1")`, then `callerCtx.Err() == nil`. `ReserveExclusive`'s
own doc (`agent_ownership.go:88-110`) is honest about this; `RunWithReservedOwnership`'s
is not — see the comment section below.

### F-5 (P3) — `#610`'s outstanding-row check is skipped whenever this call executed nothing, including after its own panic

- `internal/session/run_queue_drain_session.go:1197` (the
  `ledger.anyExecuted && !ledger.hasFailures()` gate)

I enumerated every terminal return (see "what I checked" below); the gate is
sound in the direction that matters — it can never produce a false
`DrainComplete`. But when `!ledger.anyExecuted`, `DrainSessionNow` returns
`(DrainNoWork, nil)` while a durable, unresolved row for that session exists,
and the new query is never even asked. The gate's own comment justifies this
by pointing at `TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter`, and
at the sole production call site (`app_run.go:860-864`, reached only when
`runErr != nil`) `DrainNoWork` re-surfaces the original error, so no exit-0
follows. I am recording it as a residual, not a defect — with one exception,
which is a real problem and is filed as a comment/code mismatch below.

Verification: **confirmed by executed probe**. After a panicking
`Coordinator.Run` on the synchronous drain path:

```
REVIEW9 row after panic: status="leased" leasedBy="review9-pump" attempts=0
REVIEW9 HasOutstandingRunQueueEntriesForSession=true
REVIEW9 follow-up drain: result=no-work err=<nil>
```

### F-6 (P3) — `RunWithReservedOwnership` never cancels its own `runCtx`

- `internal/agent/agent_run.go:178` vs `Run`'s `:59-60`

`Run` pairs `runCtx, runCancel := context.WithCancel(ctx)` with
`defer runCancel()`. `RunWithReservedOwnership` creates the same pair and
invokes `runCancel` only on the `rebindDispatcher` failure branch (`:185`);
on the success path nothing ever cancels it. In production the parent is
`context.WithoutCancel(...)`, whose `Done()` is nil, so `propagateCancel`
never registers the child and the leak is one unreachable `cancelCtx`. With a
cancellable parent (any future caller, or a test) the child stays in the
parent's children map until the parent is cancelled. Its own comment claims
the pair spans "the whole dispatcher, exactly like Run's own" — which is where
the divergence is easiest to miss.

Verification: read-only; traced through `context`'s `propagateCancel`
short-circuit on `parent.Done() == nil`.

### F-7 (P3) — three drain error paths let a generic cause shadow a specific one, which is exactly what `hasFailures()` was added to prevent elsewhere

- `internal/session/run_queue_drain_session.go:745`, `:814`, `:851`
  (unconditional `recordUnattributed(...)` before the final verdict)
- `internal/agent/…` n/a — contrast with `:1180-1196`, `hasFailures()`'s own
  rationale

#610's gate exists specifically so a generic "still outstanding" entry cannot
shadow an already-diagnosed, row-scoped cause via `mostRecentFailure`'s
freshest-surviving-entry rule. The same shadowing is still unguarded on the
lease-error, execSem-ctx-death and pump-stopping paths, and inside
`verdictOnCtxDone` (`:377-380`): a row's `ErrTurnCommitFailed` / `errLeaseLost`
is replaced in the reported error by `context.Canceled` or the lease error.
No work is lost; the operator (and any `errors.Is` consumer) just sees the less
specific cause.

Verification: read-only, by tracing `mostRecentFailure`'s reverse walk over
`l.order`.

## Comment against code

Every item below was checked against the code, not read and believed.

1. **`internal/server/hub.go:22-32`** — `maxConcurrentHandlersPerConn`'s doc
   still says *"acquiring the semaphore applies natural backpressure to
   readPump once the cap is hit, so excess frames simply wait to be dispatched
   instead of spawning more goroutines."* That is a verbatim description of
   the F1 defect, presented as the design rationale, in the file #612 rewrote
   to eliminate it. There is no semaphore on the work path any more
   (`dispatch`, `:182-189`, is a `select`/`default`), and backpressure on
   `readPump` is precisely what must never happen.

2. **`internal/server/handlers.go:11-25`** — `handleIncoming`'s doc says
   operations are *"launched in goroutines via c.dispatch"* and that
   *"handleIncoming itself never blocks except for the brief semaphore acquire
   inside dispatch once that cap is hit."* Both are now false: `dispatch` hands
   work to a fixed pool (`hub.go:143-153`), never spawns per-item goroutines,
   and never blocks. This is the second of the two files the eighth review
   cited as the source of F1's false guarantee (`handlers.go:18-25`), and the
   sentence survived the fix.

3. **`internal/server/server.go:212-217`** — the fork-merge note still
   describes `Client.dispatch` as recovering panics *"around every `go
   handleX(...)` spawned from handleIncoming"*. Same staleness as (2).

4. **`internal/agent/agent_run.go:100-104`** — *"rebindDispatcher keeps
   mb.epoch/state/current.id untouched, so Cancel(sessionID)/CancelAll landing
   either before or after this swap still target a live, correct CancelFunc for
   the whole hold-to-turn transition."* Before the swap the target is
   `holdCancel`, which cancels a discarded context: non-nil, but not "live" and
   not "correct" in any sense a caller can observe. Confirmed by probe (F-4).
   `ReserveExclusive`'s own doc at `agent_ownership.go:88-110` says the
   opposite, correctly — the two comments about the same object disagree.

5. **`internal/agent/mailbox_generation.go:80-84`** — *"Returns false (a safe
   no-op) if the era has since moved on."* It returns **true** on a mailbox
   `hardStop()` has latched closed, where its sibling `beginCompact` returns
   false. Confirmed by probe (F-3).

6. **`internal/agent/coordinator.go:143-149`** — *"ok is false (fail closed)
   when the session is already busy **or the coordinator is shutting down**."*
   The shutdown clause is not implemented: `ReserveExclusive` consults only
   `mb.stopped`, and `CancelAll`'s sweep can only latch mailboxes that already
   exist. Confirmed by probe:

   ```
   REVIEW9 ReserveExclusive with shuttingDown=true, fresh mailbox => ok=true
   ```

   (`RunWithReservedOwnership`'s own `tryAdmitRunWg` then refuses and releases,
   so nothing wedges — but the interface contract as written is wrong, and the
   history deletion in `handleRerunMessage` still happens before that refusal.)

7. **`internal/session/p616_sync_exec_panic_safety_test.go:136`** — *"the entry
   was already consumed by the panicking call; nothing left pending."* The
   entry was not consumed: it is `status="leased"`, `attempts=0`, owned by the
   panicking pump, and `HasOutstandingRunQueueEntriesForSession` reports true.
   Confirmed by probe (F-5). The assertion the comment justifies
   (`DrainNoWork`, `NoError`) is correct as a description of *current
   behaviour*; the reason given for it is not.

8. **`internal/session/run_queue_pump.go:415-426`** — cosmetic but worth one
   line: `TestOnWatchdogDeadlineStored`'s doc contains a corrupted word
   (`"normal scheduWhite jitter"`, line 422) and then repeats the same sentence
   verbatim four lines later. `gofmt` will not catch either.

## Tests that are green for another reason

1. **`TestDrainSessionNow_F4_ObservedRetryable_ThenLocalSuccess_ReportsComplete`**
   (`internal/session/p613_typed_admission_outcome_test.go:421-472`) does not
   pin the mechanism it names. It synchronises on a bare
   `time.Sleep(150 * time.Millisecond)` and never asserts that the drain was
   actually refused admission — so nothing prevents it from taking the
   *local-lease* path instead of the observed-admission path F4 is about, in
   which case no failure is ever recorded in the ledger and the row-identity
   rule under test is never exercised. Its own sibling three functions down
   (`:488-…`, scenario 3) does this correctly, with
   `SetTestAfterAdmissionRefusalForTest` and a hard
   `t.Fatal("DrainSessionNow was never refused admission — test setup is
   broken, proves nothing about the observed-admission path under test")`.

   Verification: **confirmed by executed probe.** I reproduced scenario 2 with
   the drain provably arriving after admission was released and confirmed that
   its three assertions (`NoError`, `DrainComplete`, `Empty(pending)`) all
   still hold with no refusal at all:

   ```
   REVIEW9: NO admission refusal happened -- the observed-admission branch was never taken
   REVIEW9 drain: result=complete err=<nil> (background Run calls=2)
   ```

   Note the mitigation, so this is not over-stated: with the shipped
   `TestTick` of 5ms, a drain that misses the window by a wide margin usually
   *fails loudly* (`DrainNoWork`, because the background tick re-leases and
   finishes the row first) rather than passing for the wrong reason — the
   silent-pass window is only the few milliseconds between the nack and the
   next tick. The fix is one line: the refusal seam scenario 3 already uses.

2. **`TestDispatchControlBypassesWorkSemaphore`**
   (`internal/server/dispatch_test.go:209-244`) is still not a #612 detector,
   and its doc still describes itself as the regression test "for the bug fixed
   here". It issues exactly `workerPoolSize` dispatches, so no dispatch call
   ever reaches the admission boundary; it would pass unchanged against a
   blocking `dispatch`. The eighth review already said this. It is harmless
   now that `TestReadPumpCancelBypassesQueuedWorkFrame` exists and is a real
   end-to-end oracle — but the two docs should not both claim the same job.

3. **`TestHandleRerunMessage_HeldReservationBlocksConcurrentSend`**
   (`internal/server/p614_rerun_reservation_test.go:307-399`) asserts against
   `mailboxLikeCoordinator`'s own mutex, not the real mailbox. It correctly
   proves *handleRerunMessage holds a reservation across the delete window*
   (the seam position is what does the work). It does **not** prove that a real
   `coordinator.Run` is refused while a real `ReserveExclusive` is held —
   nothing in this circle drives the real `mailbox.beginCompact`/`submit` pair
   for that direction. The property does follow from the pre-existing mailbox
   state machine, so this is a coverage gap, not a hole.

4. **Not a finding, recorded so the next round does not re-check it:** the two
   new #611 oracles are the real thing. `TestP1_1_WatchdogDeadlineStoredIs
   TruePreRoundingValue` is a direct value comparison against the same
   `trueNewExpiresAt` production stores, and `TestP1_2_LeaseLossCancelCauseIs
   RenewalNotWatchdog` asserts the branch identity via `cancelCauseAtomic`. Both
   carry executed revert-check transcripts in their headers, and #611's header
   states plainly that the two pre-existing lease-loss tests stay green in
   0.5s with the `!ok` branch fully disabled.

## What I checked and did not find

Recording these so the tenth round does not repeat them.

**Every terminal return of `DrainSessionNow`** (`run_queue_drain_session.go`),
with what each asserts versus what is known there:

| line | return | can it be `DrainComplete`? |
|---|---|---|
| 576 | `verdictOnCtxDone` (top of loop) | no — `anyExecuted`⇒`recordUnattributed(ctx.Err())`⇒Failed; else `(NoWork, ctx.Err())` |
| 628 | `verdictOnCtxDone` (admission wait) | no — same |
| 714 | `verdict(true)` after an observed busy outcome | no — Partial/Failed/NoWork only |
| 738 | `(DrainNoWork, leaseErr)` | no |
| 746 | `verdict(false)` after `recordUnattributed(leaseErr)` | no — map non-empty ⇒ Failed |
| 812 | `(DrainNoWork, ctx.Err())` | no |
| 815 | `verdict(false)` after `recordUnattributed(ctx.Err())` | no |
| 849 | `(DrainNoWork, ErrCallQueuedNotExecuted)` | no |
| 852 | `verdict(false)` after `recordUnattributed(...)` | no |
| 967 | `verdict(true)` after a local busy outcome | no |
| **1228** | `verdict(false)` — the terminal "nothing pending" | **yes, the only one** |

So the ninth form of *"reported complete over unresolved work"* would have to
come through line 1228, and 1228 is gated. The gate itself:

- `!ledger.anyExecuted` ⇒ check skipped ⇒ `(DrainNoWork, nil)`. A foreign or
  self-leased row can be outstanding here (F-5, verified) — but `DrainNoWork`
  is not a success claim, and the sole production caller re-raises the
  original error, so no exit-0 follows.
- `ledger.hasFailures()` ⇒ check skipped ⇒ `DrainFailed` regardless. Correct,
  and the shadowing rationale in `hasFailures`' doc (`:286-300`) matches the
  code.
- otherwise the query runs, and both of its bad outcomes
  (`outstandingErr != nil`, `hasOutstanding`) record a failure. The eighth
  form (a failed confirmation reported as "confirmed empty") is closed and
  pinned by `TestDrainSessionNow_OutstandingCheckFailure_NeverReportsComplete`.

**`internal/server` after #612.**

- Nothing can send on `workQueue` after `close`. `dispatch` is called from
  exactly one place (`handleIncoming`, `handlers.go:34-144`), which is called
  from exactly one place (`server.go:227`), synchronously on `readPump`'s own
  goroutine. Grepped the whole package; the only other callers are tests.
- Items still queued when the connection breaks are **drained and executed**
  (`for item := range c.workQueue` finishes the buffer after close). Their
  `c.reply` calls land on a `c.send` the hub may already have closed;
  `reply`'s `defer recover()` (`hub.go:547`) absorbs that. Their captured
  `ctx` is the WS request context, already cancelled. No panic escapes.
- Defer order in `readPump` is `close(workQueue)` → `unregister` → `conn.Close`
  (LIFO from `:190,191,203`), which is the right order.
- Deliberate close-path check for #621: I ran `go test ./internal/server/
  -count=6` on Windows — **clean, 121.9s**, plus the earlier single run. The
  reported one-off failure did not reproduce in 7 total passes. I did find one
  resource-profile change worth knowing but not worth filing: `newClient` now
  starts `workerPoolSize` (12) goroutines eagerly per connection instead of
  spawning per dispatch, so 12 goroutines per connection are resident even when
  idle, and they leak if `readPump` is never reached (the `s.hub.register <- c`
  send in `handleWS:172` is unbuffered against a hub that has already stopped
  reading). Both are bounded and shutdown-only.

**#614 ownership accounting.** I walked every exit of all three layers:
`coordinator.RunWithReservedOwnership` (`coordinator_run.go:621-650`: three
early returns, each with an explicit `ReleaseExclusive`, one handoff line),
`sessionAgent.RunWithReservedOwnership` (`agent_run.go:141-200`: three early
returns with `abandonOwnershipWithHandoff` + `reserveCancel`, one deliberate
no-release exception on `rebindDispatcher` failure, one handoff line), and
`runOwned` (`:227-229`, one unconditional defer). Plus `handleRerunMessage`'s
`releaseOnBailout` flag, cleared at exactly one place (`:640`) immediately
before the handoff call. **No double release and no leak on any non-panicking
path.** The panicking path is F-2.

**#616 §1 defer ordering.** The new scoped closure
(`run_queue_drain_session.go:876-888`) unwinds as
`releaseSession` → `<-execSem` → `workerWg.Done`, which is byte-for-byte the
same order as `executeEntry` (`run_queue_entry_dispatch.go:261-285`). The
re-panic inside the recover defer still lets the two outer defers run. The
window it opens — admission free while our `execSem` slot is still held — is
closed a few instructions later and cannot deadlock: a waiter that re-admits
and blocks on `execSem <- struct{}{}` is released by our own next defer. This
matches the background path exactly; nothing new here.

**#616 §5 typed SQLite errors.** No new platform is broken. `internal/app`'s
unconditional `modernc.org/sqlite` import adds nothing, because
`internal/session/run_queue_orphan_drain.go` has had the identical
unconditional import since before `a70d1b73` (verified with `git show
a70d1b73:…`) and `internal/app` already imports `internal/session`. Executed
cross-builds of `./internal/app ./internal/session`:

| target | result |
|---|---|
| openbsd/amd64 | builds |
| netbsd/amd64 | fails **inside modernc's own generated `sqlite_netbsd_amd64.go`** |
| solaris/amd64, linux/mips64 | fail with `imports … internal/session imports modernc.org/sqlite` |

— i.e. every failure traces through the pre-existing `internal/session` import,
not the new one. The `& 0xff` masking is correct: `SQLITE_CONSTRAINT` is 19 and
the extended codes are `19 | (sub<<8)` (1555 = PRIMARYKEY, 2067 = UNIQUE), both
`& 0xff == 19`; `modernc.org/sqlite`'s `*Error.Code()` returns whatever `rc`
the driver got (`error.go:12-21`, `conn.go:730-746`), and the mask is right for
either the primary or the extended value. I also confirmed the removed
`"constraint violation"` phrase: `errstrForDB` renders text from
`sqlite3_errstr`, which masks to the primary code and yields "constraint
failed" — the eighth review's suspicion that the phrase was never observed is
right.

**#615 tail selection.** `Messages.List` is unfiltered
(`internal/message/message.go:497-510`) and `ListMessagesBySession` orders by
`(created_at ASC, rowid ASC)` with an explicit comment that `messages.id` is a
non-monotonic UUID unsuitable as a tiebreaker (`internal/db/sql/messages.sql:6-18`).
Deleting `allMsgs[targetIdx+1:]` after a fail-closed `targetIdx == -1` check is
the correct total-order tail. F6 is properly closed.

**Package tests, Windows, `-count=1`:** `internal/session` ok 56.2s,
`internal/app` ok 31.2s, `internal/server` ok 26.3s, `internal/agent` ok 67.7s.
`go vet` on all four: clean. `gofmt -l internal/`: empty. Race check on the
`p613`/`p592` gated-coordinator family (`-race -count=3`): clean — the
unsynchronised `sequentialGatedCoordinator.calls` read at
`p613_typed_admission_outcome_test.go:558` was not reported.

**Not re-raised:** #617 (single typed outcome end-to-end) — filed, not
authorised, and three reviews have now reached it; #618, #620, #621 (the last
not reproduced in 7 runs); `web/dist/.gitkeep`.

## Housekeeping

I created and then removed a throwaway worktree at
`.claude/worktrees/agent-review9` for the five probes above. `git worktree
list` confirms it is gone and the main checkout is untouched. The branch
`wt-review9` it was created on may still exist — deleting a branch is on this
role's forbidden list, so I left it; it is safe to remove with
`git branch -D wt-review9`.
