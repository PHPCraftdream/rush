# Eleventh review (read-only): fifteen commits closing the ninth and tenth reviews

- Reviewed HEAD: `28d55c3393f601e2f8422471a0398d2d6de0c446` (branch `main`)
- Range: `3b2664da..HEAD` — 15 commits, 49 files, +3905 / −318
- Predecessors: `docs/reviews/2026-08-20-ninth-review-3b2664da.md`,
  `docs/reviews/2026-08-20-tenth-review-c65889a1.md` (both untracked, both read)
- Working tree: untouched. No git command that mutates anything was run. All
  revert-checks below were performed on an **out-of-tree copy of HEAD**
  (`git archive HEAD | tar -x` into a temp directory), never on the repo.

## Verdict

**NO-GO.** Two P2 blockers, both of them *open holes that a commit in this
batch asserts are closed*.

Stated plainly first, because it matters and it is the good news:

- **The tenth review's release blocker (F-1) is genuinely closed.** The
  kernel-lock-probe redesign in `bbb258dd` is structurally different, not a
  fourth patch on the same heuristic, and I could not construct a fifth
  fail-open variant in it. This is the strongest single answer this series
  has produced. Details in "What I checked and found sound".
- **No commit in this batch introduced a regression.** Build, `go vet ./...`,
  `gofmt -l internal/`, and the six touched packages are clean, including
  `-race`, including `./internal/server/ -count=6 -race` (the exact command
  that used to eat the package timeout).
- All three revert-checks I ran reproduced, and two of the three reproduced
  *exactly* the test set their commit message claimed.

The blockers are not regressions. They are two places where this batch
**reasoned its way to a conclusion that the code does not support**, and in
both cases the reasoning is what closed the issue:

1. **B-1** — `ca40acbf` built a commit point so that "the user's own prompt is
   never left deleted with nothing recreating it". Nothing between the commit
   point and the handoff returns early — that part is correct and I confirmed
   it. But *the handoff itself can fail before `Run()` recreates the message*,
   on an ordinary error (unresolvable model, coordinator not ready, session
   lock lost to another process, shutdown). I reproduced the end state:
   **session with zero messages, prompt text unrecoverable.**
2. **B-2** — `cdb0b7cc` downgraded F-7 on the explicit ground that
   "InspectSessionLock no longer feeds any destructive decision … This is
   display/diagnostics-only". It still feeds
   `internal/app/app_recovery.go:107`, the startup sweep's **primary guard**
   against clobbering a live sibling process's in-flight assistant message —
   a DB write, on every process start. The fail-open is unchanged there, and
   the `StatErr` field added to fix it is read by nothing.

Both are two-to-ten-line fixes. Neither requires re-opening `bbb258dd`'s
design.

---

## Findings, by severity

### B-1 (P2, BLOCKER) — past the commit point the handler is committed to a handoff that can fail before recreating the prompt

- `internal/server/handlers_agent.go:798-801` (last honoured cancellation),
  `:809-820` (step 3, the commit point), `:872-875` (the handoff)
- Failure sites, all strictly before `runOwned` creates anything:
  `internal/agent/coordinator_run.go:627-651` (`readyWg.Wait`,
  `applyModelOverrides`, `resolveSessionModels`, `buildCall` — four early
  returns), `internal/agent/agent_run.go:184-196` (`tryAdmitRunWg`),
  `:206-215` (`rebindDispatcher`), `:314-338` (session lock busy / unreadable)
- The user message is created at `internal/agent/agent_turn.go:369`
  (`createUserMessage`), i.e. inside the turn loop, **after** every one of
  those failure sites.

**What is wrong.** `ca40acbf`'s invariant is stated as: *"nothing below may
return without first handing off into RunWithReservedOwnership"*. That is
literally true and I verified it mechanically — between line 801 and line 875
there is no `return` statement at all. But the property the commit point
exists to guarantee is not "the handoff was *called*", it is "the prompt gets
recreated", and only `runOwned` → `runTurn` → `createUserMessage` does that.
Every early return inside `RunWithReservedOwnership` (both layers) sits above
its own handoff line, so `Run()` never runs, and the handler replies with an
error over a session whose target message is already gone.

The prompt text survives only in the handler's local `text` variable, which is
discarded when the handler returns. There is no outbox, no retry, no draft.

**Failure scenario (concrete).**

1. Operator's session has `smart_model_id` referring to a model that is no
   longer resolvable (provider removed from config, catalog entry gone, key
   revoked — `coordinator_run.go`'s own comment at `:610-613` says
   `resolveSessionModels` "fails on an ordinary misconfigured/unresolvable
   model, not just shutdown").
2. Operator hits Rerun on their own message.
3. Steps 1/1a/1b all succeed (session idle, reservation claimed, kernel probe
   granted). Tail deleted. **Target user message deleted** (step 3).
4. `RunWithReservedOwnership` → `coordinator_run.go:640` returns
   `"failed to resolve session models: …"`.
5. Handler replies `EventError` with that text. The session now contains
   **zero messages**. The operator's prompt is gone with nothing to retry
   from — worse than the pre-`#630` "cancelled" case, because there is not
   even a target row left to re-run.

**Verification: CONFIRMED by execution.** In an out-of-tree copy of HEAD I
added a coordinator fake whose `RunWithReservedOwnership` reproduces
`coordinator.RunWithReservedOwnership`'s own early-return shape exactly
(release the reservation, return an error, never invoke `onHandoff`) and drove
the real `handleRerunMessage`:

```
ws: handleRerunMessage sessionID=86ab6624… contentPreview="the operator's own words"
ERROR ws: rerun agent error err="failed to resolve session models: no such model"
    reply type=error error="failed to resolve session models: no such model"
    target message after failed handoff: err=sql: no rows in result set
    messages remaining in session: 0
```

What is *inferred* rather than executed: that a real misconfigured model
actually drives `resolveSessionModels` to an error in production. The handler
half — "handoff returns an error ⇒ target row already gone, session empty" —
is executed.

**Pre-existing, not a regression.** `git show 3b2664da:internal/server/handlers_agent.go`
has the same ordering (step 3 delete at `:614`, handoff at `:643`). It is
listed as a blocker here because `ca40acbf` is the commit that introduced the
named commit point and its justification, the task asked specifically whether
"a partial-commit state" is still reachable past it, and it is — on an
ordinary error path, not only on a race.

**Note on fix shape** (so the next round does not re-derive it): the cheap fix
is to not delete the target until the handoff has actually reached
`runOwned` — e.g. hand `text` to the replacement turn first and delete the row
from inside the same era, or re-create the row from `text` on the handler's
error path before replying. The `deleteCtx` already survives cancellation, so
the recreate is safe on the error path.

---

### B-2 (P2, BLOCKER) — `InspectSessionLock`'s fail-open still feeds a destructive DB write; `cdb0b7cc`'s rescoping claim is false

- `internal/session/lock.go:982-993` (the `os.Stat` branch: any error other
  than `IsNotExist` → `LockState{StatErr: err}`, i.e. `Exists:false`,
  `Live:false`)
- Destructive consumer: `internal/app/app_recovery.go:107`
  (`if st := session.InspectSessionLock(dataDir, sess.ID, session.LockStaleDuration); st.Live { skip }`)
- Called from `internal/app/app.go:211` — **every** crush process start, web
  and CLI.

**What is wrong.** `cdb0b7cc`'s commit message and the new `StatErr` doc both
assert that after `bbb258dd`, `InspectSessionLock` "no longer feeds any
destructive decision" and is "display/diagnostics-only". I enumerated its
production consumers:

| consumer | what it does with `.Live` |
|---|---|
| `internal/server/handlers_sessions.go:169,192` | UI badge — display, fail-open correct |
| `internal/cmd/sessions_watch.go:453` | tail-loop termination — display |
| `internal/cmd/sessions_inject.go:212` | JSON `running` field, computed *after* the inject already happened — display |
| **`internal/app/app_recovery.go:107`** | **primary guard before `app.Messages.Update` on another process's session** |

The last one is not display. `app_recovery.go:86-104` documents it as the
task #287 release blocker in its own words: *"never touch a session that
another LIVE crush process still owns … because `message.Update` rewrites the
whole Parts blob from the snapshot read here, that stamp also CLOBBERS
whatever the live owner streamed in between our read and our write. This
fork's entire model is N concurrent `crush run` sessions sharing one data
directory, so that was routine, not rare."*

**Failure scenario (concrete).** Two crush processes share `--data-dir` on a
mapped network drive / SMB share (or the locks directory is momentarily
unreadable for any reason that is not "not found": permission, I/O error,
path-length, transient sharing violation).

1. Process A is 8 minutes into a sub-agent delegation on session S. It holds
   the OS lock; `<dataDir>/locks/session-S.lock` has a fresh heartbeat.
2. Process B starts. `recoverInterruptedTurns` iterates every session.
3. `os.Stat` on S's lock file fails with a non-ENOENT error →
   `InspectSessionLock` returns `Live: false` → **the primary guard does not
   fire**.
4. The only remaining guard is the 30 s age filter
   (`lastAssistant.CreatedAt > staleBefore`). A's assistant row is 8 minutes
   old, so it does not fire either.
5. B writes `FinishReasonError "Process restarted"` over A's live streaming
   assistant message, clobbering A's Parts blob.

This is exactly the shape `externalSessionOwnerRefusal`'s own retired doc
named — *"'Could not look' must not read as 'looked and found nothing'"* —
surviving at the one consumer where it still costs data.

**Verification: CONFIRMED by reading + call-graph enumeration** (grep over
every `InspectSessionLock(` call site outside `_test.go`, then the caller
chain `app.New → recoverInterruptedTurns`). Not reproduced: forging a
non-ENOENT stat error inside a running `app.New` needs a filesystem fault
injector; the batch's own `sessions_why` test shows the NUL-byte technique
that would work for a unit-level probe.

**Fix shape:** one line — treat `st.StatErr != nil` as "live" (fail closed) at
`app_recovery.go:107`. The field already exists; nothing reads it.

---

### F-3 (P3) — `LockState.StatErr` is dead code, and its doc names a reader that does not exist

- `internal/session/lock.go:940-947`
- `internal/cmd/sessions_why.go:109-110, 117, 166-167, 187-189`

The doc reads: *"Display consumers intentionally ignore this and stay
fail-open (Exists/Live both false); **only diagnostics (sessions why) read
it**."* `sessions why` does not read it. `explainSessionStatus` never calls
`InspectSessionLock` at all — it does its own `os.Stat(lockPath)` at
`sessions_why.go:117` and keeps its own `statFailed`/`statFailure` locals. The
commit message repeats the same claim verbatim ("`sessions why` reads the new
field").

`grep -rn StatErr internal/ --include=*.go` returns exactly: the declaration,
the one write inside `InspectSessionLock`, and `internal/session/lock_test.go`.
**No production code reads it.**

Consequence beyond tidiness: the field is the reason B-2 was graded as closed.
A maintainer reading `InspectSessionLock` today is told the "could not check"
case is handled by a diagnostics consumer, and stops looking.

Verification: CONFIRMED by grep + reading both files.

The `sessions why` change itself is real and correct, and its two tests
(`TestExplainSessionStatus_StatFailureSaysCouldNotVerify` and its sibling)
are honest — the NUL-byte forgery genuinely produces a non-`IsNotExist` error
on this platform, and I confirmed the negative control (`t.TempDir()` →
"status: at rest") passes. It just has nothing to do with `LockState`.

---

### F-4 (P3, first-class per this series' rules) — a fourteenth comment-vs-code item, minted by the third comment-cleanup round

- `internal/agent/agent_run.go:135-143` (the "Mailbox hard-stopped" bullet,
  written by `93167971`)
- Contradicted by `internal/agent/mailbox_queue.go:68-95`
  (`abandonOwnershipAndPopSubmitted`), `internal/server/handlers_agent.go:645-654`
  (the caller's still-armed bail-out defer), and
  `internal/agent/agent_run.go:184-196` (the sibling branch, 20 lines up)

The new doc justifies *not* releasing on the `mb.stopped` rebind-failure
branch:

> Releasing here would also start detached runs for any queued work, exactly
> what the stopped latch exists to prevent during teardown, so not releasing
> is deliberate … and every drain refuses on the latch anyway.

Three problems:

1. **The function actually on that path does not consult the latch.**
   `abandonOwnershipWithHandoff` calls `abandonOwnershipAndPopSubmitted`
   (`mailbox_queue.go:68`), which checks **only** `mb.epoch != epoch`. It has
   no `mb.stopped` test. (`drainOrReleaseFinal` *does* honour the latch —
   `mailbox_ownership.go:218-225` — but it is not on this path.) So "every
   drain refuses on the latch anyway" is false for the relevant drain.
2. **The sole production caller releases one stack frame later anyway.** On
   the stopped branch `onHandoff` never fires, so `handleRerunMessage`'s
   `releaseOnBailout` defer (`handlers_agent.go:649-654`) calls
   `ReleaseExclusive` → `abandonOwnershipWithHandoff` → pops the submitted
   queue → `restartOrphanedWithRetry` → detached runs during teardown. The
   epoch still matches on this branch (that is what distinguishes it from the
   mismatch branch), so nothing short-circuits it. The "deliberate"
   protection has no effect in production.
3. **The sibling branch says the opposite.** `agent_run.go:190-192`, twenty
   lines earlier, calls `abandonOwnershipWithHandoff` on the *shutdown* path
   and documents it as *"safe to call post-shutdown-latch: it only
   pops/restarts queued work if any is present"*. The same call is "safe" in
   one branch and "exactly what the latch exists to prevent" in the other.

`93167971`'s message explicitly says item 4 "was checked as a possible CODE
defect rather than assumed to be prose, and it is not one". The check did not
follow `abandonOwnershipWithHandoff` into `mailbox_queue.go` or out into the
caller's defer. The *branch* may still be the right shape; the *justification
recorded for it* is not a property this code has — which is precisely the
defect class the commit exists to eliminate, for the third round running.

Verification: CONFIRMED by reading the four sites and tracing the call chain.
Not executed — reaching it needs `mb.stopped` latched inside the few
instructions between `tryAdmitRunWg` returning true and `rebindDispatcher`
running.

---

### F-5 (P3) — a comment describing a state the `#631` redesign removed

- `internal/server/handlers_agent.go:677-679`

```go
// probeHeld guards the bailout paths between here and the step-6
// handoff; probe.Release is nil-safe, so the no-lock-file case (probe
// == nil) defers cleanly too.
```

There is no "no-lock-file case (probe == nil)". `TryHoldSessionLockShared`
opens with `os.O_CREATE` (`lock.go:924`) specifically so the absent-file case
*creates* the file, pins the inode and returns a real probe — that inode
pinning is one of the redesign's stated improvements. The batch's own test
asserts it two files away
(`p622_external_lock_test.go:221-227`: *"No lock file at all: grant (and the
caller MUST Release the probe)"*, followed by `require.NoError(t, probe.Release())`).

The nil-safety of `Release` is real and worth keeping; the case it is
attributed to is not reachable. Harmless in behaviour, but it is the same
"comment quoting a condition that has since moved" shape `93167971`'s own
message identifies as the recurring generator, minted by `bbb258dd`.

Verification: CONFIRMED by reading `lock.go:915-935`, `handlers_agent.go:670-685`,
and the test.

---

## What I checked and found sound

Recording these so the twelfth round does not re-derive them.

### `bbb258dd`'s shared-lock probe — the central question, answered

**I could not construct a fifth fail-open variant, and I believe the design is
structurally done.** The reason is that the decision no longer has a
"classify" step at all:

- `TryHoldSessionLockShared` (`lock.go:913-947`) has exactly one *allow*
  branch, and it is reached only when `tryLockFileShared` returned nil. Every
  other exit — empty `dataDir`, empty `sessionID`, `MkdirAll` failure,
  `OpenFile` failure, contention, any non-contention lock error — returns an
  error, and both callers treat any error as refuse
  (`handlers_agent.go:127-143`). There is no byte, mtime, PID or age input to
  the decision, so there is no input left to be wrong about.
- The "held vs released leftover" ambiguity that generated variants 1-4 is
  gone by construction: a released lock has no kernel holder, so the probe
  grants; a held lock has one, so it denies, regardless of what the file
  contains. `TestTryHoldSessionLockShared_InertBytesNeverMatter` pins the
  inversion explicitly.
- The acquire-window state (variant 4) is pinned *deterministically* by
  `acquireMidStampSeam` racing a real second lock handle against a goroutine
  paused mid-acquire — not forged bytes.
  **Revert-checked, executed:** forcing `tryLockFileShared` to succeed
  unconditionally turns exactly the probe-dependent tests red and nothing
  else: `TestHandleRerunMessage_ExternalLockHolderFailsClosed`,
  `TestHandleRerunMessage_OwnHeldLockNowRefuses`,
  `TestHoldExternalSilenceProof_FailClosedMatrix`,
  `TestDeleteMessageRescuingOrphan_ExternalLockHolderRefusesRescue`,
  `TestDeleteMessageRescuingOrphan_HeldWithoutSidecarStillRefuses`, plus
  `TestTryHoldSessionLockShared_{MidAcquireWindowDenies,ExclusiveHeldReportsBusy,SharedProbesCoexist}`.
  The `ReleasedLeftover*` and `#630` cancel-window tests stayed green, which
  is the right discrimination.
  *(The commit message says "**exactly** [two tests] go red". Measured: five
  in `internal/server`, three in `internal/session`. The claim is
  understated, not wrong in direction.)*

**Probe leak paths: none.** Both consumers use `defer`, so panics are covered:
`handlers_agent.go:680-685` (`probeHeld` flag + defer) and
`handlers_messages.go:179` (plain `defer probe.Release()`). The single
non-defer release (`handlers_agent.go:858-859`) sets `probeHeld = false`
immediately after, so the defer cannot double-release; and
`handleDeleteMessages`'s loop takes/releases one probe per ID.
`deleteMessageRescuingOrphan` got the identical treatment, not a simplified
one — it is simpler only because it performs a single mutation, which
`ca40acbf` independently re-verified.

**The `#631` F-1 symptom is genuinely gone.** Not just at the primitive level
(`TestTryHoldSessionLockShared_ReleasedLeftoverGrants` does a real acquire +
`Release()` then requires an immediate grant, no `Eventually`) but at the App
level (`TestHoldExternalSilenceProof_FailClosedMatrix`'s fourth case). And the
ordering that makes it correct in production holds: `runOwned`'s `lk.Release`
defer is registered *after* the `abandonOwnershipWithHandoff` defer
(`agent_run.go:261` vs `:340`), so LIFO runs the lock release first; and the
normal path releases `lk` inside the same mailbox critical section that flips
to `mbIdle` (`drainOrReleaseFinal`). `IsSessionBusy` reports `mbReleasing` as
busy, so "idle-poll observed idle" implies "OS lock already free". There is no
window where the handler's own process still holds the lock at step 1b.

**The reverted-2026-08-09 background-probe landmine does not apply.** I read
the note rather than the commit's paraphrase. That failure was a *background*
goroutine taking an *exclusive* probe *after* `Release`, racing the same
caller's near-instant re-acquire. This probe is synchronous, shared (many
coexist — pinned by `TestTryHoldSessionLockShared_SharedProbesCoexist`),
user-initiated, and is released before the only exclusive acquire that
follows it (pinned by `TestHandleRerunMessage_ProbeReleasedBeforeHandoff`,
which attempts a *real* exclusive acquire inside `rerunPreHandoffSeam`). All
four properties the commit claims are properties the code has.

**The `O_CREATE` side effect is inert in practice.** The probe can create a
lock file for a session that never had one, leaving an empty file with a fresh
mtime. `annotateExternalOwnership`/`AnnotateSessionExternalOwnership`
(`handlers_sessions.go:170,193`) both require `st.PID != 0 && st.PID != self`,
so no bogus "owned externally" badge appears. And both destructive entry
points require a session that has already run a turn (a rerunnable user
message; a still-streaming assistant row), which means the lock file already
exists from a prior acquire. I found no reachable case where the probe
introduces a lock file that would not have existed anyway.

### `ca40acbf`'s commit point — exactly one, and nothing returns past it

Mechanically verified: within `handleRerunMessage` there are three
`holdCtx.Err()` checks (`:705`, `:743`, `:798`), the last of which is the
commit point; `awk` over lines 795-890 finds exactly two `return` statements,
one at `:800` (the commit point itself) and one at `:885` (after
`RunWithReservedOwnership` has already been called). `Sessions.Get` at `:830`
correctly moved to `deleteCtx`. The tail loop and step 3 both run under
`context.WithoutCancel(holdCtx)`.

**Revert-checked, executed:** reinstating a `holdCtx.Err()` early return
between step 3 and step 4 turns **exactly one** test red —
`TestHandleRerunMessage_CancelDuringTargetDeleteCommitsToRerun` — with the
other two `#630` tests staying green. That matches the commit message
precisely.

(The gap is that "handoff called" ≠ "prompt recreated" — see B-1.)

### `28d55c33` — reachability claim verified, revert-check reproduced

Grepped every `db.Connect` / `db.ConnectRead` / `db.Release` call site in the
main tree (excluding `.claude/worktrees`, which polluted the first grep):
`internal/cmd/root.go:346` (inside `setupApp`), `internal/cmd/stats.go:133`,
`internal/app/app.go:160`, `internal/app/app_lifecycle.go:115`. Each opens one
dataDir, sequentially. The `sessions` family reaches `db.Connect` only
indirectly through `setupApp`, once, before anything else. **The commit's
claim that the concurrent multi-dataDir production scenario is unreachable
today holds.**

**Revert-checked, executed:** restoring the single global `poolMu` across the
whole `connect` body fails
`TestConnect_UnrelatedDataDirsDoNotSerializeBehindMigration` with the exact
predicted text (*"Connect for an unrelated dataDir was blocked for 30s behind
a paused open of a different database"*), while
`TestConnect_ConcurrentSamePathSharesOneEntry` stays green — i.e. the test
discriminates the mechanism, not "some test fails".

**On `pathLocks sync.Map` never being pruned: not a concern.** Keys are
resolved absolute DB paths. Production opens exactly one per process. Tests
add one per `t.TempDir()` and the binary exits. Each entry is one
`*sync.Mutex`. There is no long-running-server pattern in this codebase that
opens unbounded distinct dataDirs — the web server owns one, and the
`sessions` subcommands are one-shot processes.

One low-severity behavioural note, no finding: `ResetPool()` and
`ReleaseAll(dataDir)` take only `poolMu`, so they no longer serialize against
an in-flight first `connect` for the same path — a `ReleaseAll` landing while
that path's entry has not yet been inserted is now a silent no-op that leaves
the entry pooled. Both are test-only helpers with no production caller; the
`connect_pool_test.go` cleanups all run after their opens complete.

### `ecaff5f8` and the flake picture

`waitHubDrained` is now condition-driven with a 60 s ceiling
(`p547_sticky_update_notice_test.go:71-84`), and the bound has its own
regression test that proves a *fast red* rather than a hang, using a
`captureT` that intercepts `Fatalf` and `runtime.Goexit()`s. The
non-`t.Parallel()` reasoning on the two bound tests is sound: Go resumes
paused parallel top-level tests only after every sequential one has finished,
so nothing can observe the shrunk 50 ms value; the `atomic` access makes it
race-free regardless.

**I swept the batch's new test code for the "hang instead of red" sibling**
`ecaff5f8` exists to prevent. Every wait in
`p623_panic_window_test.go`, `p630_rerun_cancel_window_test.go`,
`p623_hold_ctx_test.go`, `p623_defer_run_cancel_test.go`,
`connect_pool_test.go` and `p631_released_lock_test.go` is a `select` with a
`time.After` arm or a `require.Eventually`. The only bare receive is
`p631_released_lock_test.go:157` (`res := <-done`), which runs strictly after
`close(proceed)` releases a goroutine whose remaining work is a handful of
syscalls, and whose seam has its own 10 s panic guard. Not worth a finding.

**The tenth review's F-2 (eight p547 tests failing together) did not
reproduce.** `go test ./internal/server/ -count=6 -race` — clean, 714.7 s.
`go test ./internal/server/ -count=6` — clean, 135.5 s. Two data points, not
ten; see "could not verify".

### `93167971`'s other four items — checked against the code, all accurate

- **`coordinator.go:145-152`** — verified: `coordinator.ReserveExclusive`
  (`coordinator_run.go:582`) is a pure passthrough to
  `sessionAgent.ReserveExclusive` (`agent_ownership.go:111`) →
  `mailbox.beginCompact` (`mailbox_generation.go:48-60`), whose only refusal is
  `mb.state != mbIdle || mb.stopped`. `readyWg` is not consulted. The new text
  is correct.
- **`server.go:212-221`** — verified against `hub.go:141-231`: `dispatch`
  enqueues on `workQueue` with `select/default`; `startWorkers` runs
  `runRecovered` per item; `dispatchControl` is the only `go` spawn and is
  used by exactly `CmdInterruptAndSend` and `CmdCancelAgent`
  (`handlers.go:42,47`). `grep "go handle" internal/server/` is empty. Accurate.
- **`run_queue_pump.go:419-424`** — the `scheduWhite` typo and the duplicated
  sentence are both gone.
- **`app_run.go:343-352`** — the `DrainNoWork` paragraph now states the
  disjunction ("covers BOTH the shapes … distinguishes") instead of the
  conjunction. Internally consistent.
- **The one non-comment change that rides along** — the error text
  `"(epoch mismatch or stopped mailbox)"`. Verified: `grep -rn "epoch mismatch"`
  over `internal/` and `web/src/` finds no test, caller or client matching the
  old string.
- `handlers.go`, `hub.go` and `server.go` are **comment-only** across this
  entire range (`git diff 3b2664da..HEAD` with comment lines filtered →
  empty), confirming the "comment-only otherwise" claim for those files.

### Spot-checks of items the tenth review filed as settled

Three re-checked rather than all of them, per instructions:

- **`Run()` byte-identical to base** — holds. `git diff 3b2664da..HEAD --
  internal/agent/agent_run.go` produces three hunks: two in the doc block
  above `RunWithReservedOwnership` and one in its body. `Run`'s own body is
  untouched.
- **`onHandoff` transfer** — holds. `agent_run.go:224-226` invokes it, and the
  only statement between it and the handoff line (`:233`) is the call itself;
  `runOwned`'s `defer abandonOwnershipWithHandoff` is its first statement
  (`:261-263`).
- **`RunWithReservedOwnership` has exactly one production caller** —
  holds (`handlers_agent.go:872,874`). Everything else is the interface
  declaration, the coordinator passthrough, or tests.

### Executed verification summary

| check | result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` (module-wide, not per-package) | clean apart from the pre-existing `csync.JSONSchemaAlias` lock-by-value warning |
| `gofmt -l internal/` | empty |
| `go test ./internal/db/ ./internal/session/ ./internal/server/ ./internal/agent/... ./internal/app/ -count=1` | all ok (4.5 / 47.3 / 29.2 / 72.8 / 29.2 s) |
| `go test -race ./internal/session/ ./internal/server/ ./internal/db/ -count=1` | all ok (68.5 / 128.5 / 57.2 s) |
| `go test ./internal/server/ -count=6 -race` | ok, 714.7 s |
| `go test ./internal/server/ -count=6` | ok, 135.5 s |
| `go test ./internal/cmd/ -count=1` | ok, 37.3 s |
| revert-check: probe always grants | 5 server + 3 session tests red, correct set |
| revert-check: post-step-3 early return restored | exactly `CancelDuringTargetDeleteCommitsToRerun` red |
| revert-check: global `poolMu` restored | exactly `UnrelatedDataDirsDoNotSerializeBehindMigration` red, with the predicted message |
| probe: handoff-failure prompt loss | reproduced, session left with 0 messages |

The `28d55c33` slowness claim is **confirmed fixed as a hang**: the command
that used to park inside `goose.Up` against the package timeout now completes.
It is *not* fixed as a duration — 714.7 s exceeds `go test`'s **default** 600 s
package timeout, so `go test ./internal/server/ -count=6 -race` still needs an
explicit `-timeout` on this machine. That is genuine work, not serialization
(the same package at `-count=1 -race` is 128 s, i.e. it scales linearly now).

---

## Things I could not verify, labelled as such

1. **B-1's production trigger.** The handler half is executed; that a real
   misconfigured model actually makes `resolveSessionModels` return an error
   is inferred from `coordinator_run.go:610-613`'s own comment, not measured.
   The other six failure sites (readyWg, buildCall, tryAdmitRunWg,
   rebindDispatcher, lock-busy, lock-unreadable) are inferred by reading.
2. **B-2 is traced, not reproduced.** Forging a non-ENOENT `os.Stat` failure
   inside a live `app.New` needs fault injection. The NUL-byte technique
   `cdb0b7cc` discovered would work for a unit-level probe of
   `recoverInterruptedTurns` via its existing `recoveryDataDir` test seam.
3. **F-4 is not executed.** The `mb.stopped` rebind-failure branch needs
   `CancelAll` to latch inside a few-instruction window. There is a test for
   `rebindDispatcher` returning false on `stopped`
   (`p623_rebind_stopped_test.go`) but none that drives
   `RunWithReservedOwnership` through it with queued work present.
4. **Flake depth.** Two `-count=6` runs of `internal/server` (one `-race`), not
   ten. The tenth review's F-2 did not reproduce, which is evidence but not
   proof; `ecaff5f8`'s 60 s bound would in any case have absorbed the 8.3 s
   stalls that produced it. A `-count=20` run is still owed by someone.
5. **Windows-only platform coverage.** Every run above is Windows
   (`LockFileEx`, mandatory range locks). The POSIX half of
   `tryLockFileShared` (`flock(LOCK_SH|LOCK_NB)`) is read, not executed. The
   claim that separate open file descriptions conflict within one process is
   correct per `flock(2)`, and `TestHeldLockPrimaryReadability_ExercisesRealOSLock`
   is written to assert the platform difference either way — but nobody has
   run it on Linux in this round.
6. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was touched, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed.
7. **`73d5caa2`'s behavioural claim remains unproven**, unchanged from the
   tenth review: nothing shows that a model with no edit tools delegates via
   `agent` rather than stalling. Structural enforcement only.

## Carried-over minor items still open (no finding)

Both were listed as Minor by the tenth review and are still present at HEAD —
recorded so the list-based cleanup pattern the batch itself diagnosed does not
lose them again:

- `internal/agent/p623_defer_run_cancel_test.go:101` still ships
  `fmt.Printf("DEBUG model call prompt: %+v\n", call.Prompt)`, dumping the
  full rendered prompt to stdout on every model call in that test.
- `internal/agent/agent_ownership.go:197` still points at
  `mailbox_ownership.go` for `drainAfterCancel`'s priority; it lives at
  `internal/agent/mailbox_interrupt.go:276`. (The named file *does* exist,
  which makes the wrong pointer harder to notice, not easier.)

The tenth review's third Minor — the `p622` revert-checks naming a
non-existent `externalSessionOwnerPID` — **is closed**: `bbb258dd` rewrote the
file and the identifier appears nowhere in `internal/`.
