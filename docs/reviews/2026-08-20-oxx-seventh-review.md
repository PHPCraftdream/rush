# Seventh independent review — 2026-08-20 (`@oxx`)

Scope: the seven unpushed commits `f8ffb68c` … `cbd6e8d4`
(`git log --oneline 89642f01..HEAD`) — the backlog closure of the SIXTH
review (`docs/reviews/2026-08-20-oxx-post-gate-review.md`). Context read:
`CHANGELOG.fork.md` (last two sections), `docs/design/session-lifecycle.md`,
`docs/reviews/2026-08-20-dynamic-release-gate.md`,
`docs/checkpoints/2026-08-20-0600.md`.

Every defect asserted below was **reached by running code** on real Linux
(WSL2 Ubuntu-24.04, `go1.26.3` from `$HOME/sdk/go`). Everything I could not
run is in the final section instead of being softened into prose.

---

## Verdict: **NO-GO**

**The sixth review's two blockers are genuinely closed.** I verified both by
running, not by reading:

- `f8ffb68c` closes the sixth form at **both** sites, and closes it with the
  per-site pinning the sixth review demanded as its own standard: reverting
  site 1 alone fails exactly one test and leaves site 2's test green, and
  vice versa (RC1/RC2 below). The `anyExecuted` guard the coordinator caught
  is pinned too (RC3).
- `75c5ed52` closes the pre-push gap. The exact CI/pre-push fence command now
  runs green on real Linux: **28 tests, `ok internal/session 6.045s`,
  `ok internal/cmd 21.343s`**, including the two `#606` tests that failed at
  every prior commit.

The NO-GO rests on two things, one of which is this round's own work:

1. **There is a seventh form.** `DrainSessionNow` still returns
   `(DrainComplete, nil)` — the one pairing `drainOutcomeError` turns into
   exit code 0 with a success envelope — over a durable row that was never
   executed by anyone. It is reached with a **live** context, **no**
   contention and **no** failure, through the eighth return site (the
   "nothing pending" branch), and I reproduced it. The changelog, the
   checkpoint and `f8ffb68c`'s own commit message all state flatly that no
   seventh form exists. That claim is false, and it is exactly the class of
   claim this release is gated on.
2. **`b7d50642`'s invariant is not pinned by anything.** Reverting the single
   production line that constitutes the `#604` fix leaves the *entire*
   `internal/session` package green. Per the standing rule ("an invariant
   whose revert breaks nothing is P1 regardless of how correct the code
   is"), and per this round's own standard applied to `#607`, that is a
   blocker-adjacent gap: the exact bug can be silently reintroduced by a
   future edit.

Both are small, well-scoped fixes. Nothing here suggests the round's
engineering was wrong — `b7d50642`'s mechanism is real and I confirmed it by
measurement (see P1-2). What is missing is a test and a retraction.

Counts: **2 P1, 9 P2, 0 P0.**

---

## P1-1 — a SEVENTH form: `(DrainComplete, nil)` over a row leased by a foreign owner

**Where:** `internal/session/run_queue_drain_session.go:962` — the final
`return ledger.verdict(false)` in the "nothing pending" branch.

**Mechanism.** `GetOldestPendingRunQueueEntryForSession`
(`internal/db/sql/run_queue.sql:21-28`) filters `status = 'pending'`. A row
in `status = 'leased'` — whether by a live foreign process, or by a process
that has since been `sessions kill`-ed and whose lease has already expired —
is invisible to it. `LeaseRunQueueEntry` therefore returns `(nil, nil)`,
`leased == nil`, and the loop concludes the queue is empty. If an earlier
row in the same call already `recordSuccess`'d, `anyExecuted == true`,
`failed` is empty, `contended` is false → `verdict(false)` returns
`(DrainComplete, nil)`.

`app_run.go:302-310` maps that to `nil`; `app_run.go:834-835` →
`finish(nil)` → `app_run.go:764-765` sets `hookExitReason = "stop"` and
returns `nil` → **exit 0 with a success envelope**.

**This is an unacknowledged gap, not a documented boundary.** The function's
own doc at lines 414-425 names this exact race and claims it is closed:

> *"if the background tick wins the race, THIS call's own lease attempt
> simply finds nothing pending … Silently returning "nothing to drain" in
> that case would reproduce the exact bug this function exists to close …
> The fix: check admission for this session before concluding there is
> nothing left to wait for."*

`admitSession` is in-memory and per-process (`p.inFlight`) — the file's own
re-check comment at lines 866-869 says so verbatim. So the mitigation covers
the in-process half only. The cross-process half is handled for exactly one
row — the `lastRowID` this call itself Nacked — where the same DB state
(`current.Status == "leased"`) is reported as `errLeaseLost` → `DrainFailed`
with the comment *"A DIFFERENT owner holds it right now. Unknown, not
resolved -- report that, not a fabricated success"* (lines 952-955). A row
this call never happened to touch gets the opposite answer from the same
state.

### How it is reached (reproduced)

Probe at `internal/session/zz_reviewer7_probe_test.go` (created, run,
deleted — clean `git status` shown at the end). Two durable rows for one
session; while row A is mid-turn, a *different* pump instance leases row B
with a tiny TTL, then the probe sleeps past the persisted expiry so there is
provably **no live owner at all** — row B is orphaned work awaiting some
future `CleanupExpiredLeases` tick.

```
$HOME/sdk/go/bin/go test ./internal/session/ -run TestZZProbe7 -count=1 -v
```

```
INFO run_queue_pump: executed entry successfully id=zz-probe7-row-A ...
zz_reviewer7_probe_test.go:81: PROBE7 RESULT: result=complete err=<nil> coordCalls=1 ctxErr=<nil>
zz_reviewer7_probe_test.go:88: PROBE7 ROW B: id=zz-probe7-row-B status=leased
    leased_by=a-totally-different-process-pump attempts=0 expires=1787200372 now=1787200372
    Error: Should be false
    Messages: PROBE7: (DrainComplete, nil) returned while row B is still durably
              outstanding (status=leased, attempts=0) and was never executed by anyone
--- FAIL: TestZZProbe7_ForeignLeaseAfterOneSuccess (1.93s)
```

Note `ctxErr=<nil>`: the context is alive. This is not a variant of the
sixth form — it is a different axis entirely.

### Production reachability

Two shapes, both ordinary for this fork:

1. **`crush web` + `crush run`.** The long-lived server process runs its own
   `RunQueuePump` ticking every 3s against the same `.crush` DB. Its tick
   leasing row B while a concurrent `crush run`'s interrupt-driven drain is
   executing row A is the *dominant* interleaving, not an exotic one. The
   `crush run` process then exits 0 with a success envelope for a
   continuation another process is still mid-way through, and whose output
   never reached this envelope.
2. **A killed/crashed holder.** `crush sessions kill` uses `taskkill /F /T`
   / `SIGKILL`. A row leased at the moment of the kill stays `leased` for up
   to `RunQueueLeaseTTL` (30s) plus however long it takes some pump to run
   `CleanupExpiredLeases`. Any drain in that window reports `DrainComplete`
   over it. This is the variant my probe reproduces.

### Severity

I rate this **P1, not P0**, and say why explicitly: unlike the sixth form it
is not single-process reachable (it needs a second live or crashed process),
and the work is not permanently lost — `CleanupExpiredLeases` eventually
recovers the row. What is lost is the truthfulness of the exit code, which
per `CHANGELOG.fork.md` §1 (*"silent success is the worst class of
failure"*) is the fork's stated primary contract. A reviewer applying the
sixth review's own rubric verbatim would call this P0; I am not going to
argue with that reading.

### Nothing pins the current behaviour

No test in `internal/session` drives a foreign lease against the
"nothing pending" branch. A fix collides with nothing.

### Minimal fix (not applied — reporting only)

Either (a) extend the terminal re-check to ask "is anything at all still in
`session_run_queue` for this session?" before `verdict(false)`, reporting
`ErrRowOutcomeUnconfirmed`/`errLeaseLost` when the answer is yes — the same
conservative answer the `lastRowID` path already gives for the identical DB
state; or (b) if that is deliberately out of scope, say so **in the code and
in the doc**, and retract "there is no seventh form" from the changelog and
the checkpoint. What is not acceptable is the current combination of an
unacknowledged gap and an explicit exhaustiveness claim.

---

## P1-2 — `b7d50642`'s fix is correct, measurable, and pinned by nothing

**Where:** `internal/session/run_queue_entry_exec.go:422`
(`watchdogDeadlineAtomic.Store(trueNewExpiresAt.UnixNano())`).

Reverting that one line to the pre-`#604` `time.Unix(newExpiresAt, 0)`
leaves the whole package green:

```
go test ./internal/session/ -count=1 -timeout 300s   →  ok  57.3s
```

The only test that could discriminate,
`TestP1_1_WatchdogCancelsAtTTLMinusMargin`, asserts
`6s < elapsed < 9s` (`p1_1_watchdog_window_test.go:224-229`). Measured on
this machine, `-count=3` each:

| build | elapsed | theoretical |
|---|---|---|
| HEAD | 7.5065s / 7.5063s / 7.5055s | 7.5s |
| line reverted | 8.2859s / 8.3966s / 8.3566s | 7.5s |

So the mechanism is **real and reproducible** — 780–897ms of donated margin,
squarely inside the changelog's claimed 97–930ms range — and the assertion
window is ~1.5s wider than the effect. The fix cannot regress detectably.

This is the same gap the sixth review's own "what would flip this to GO"
demanded be closed for `#607` ("a test that fails when either site's record
is removed **individually**"), applied to `#607` and not to `#604`. Tighten
the bound (e.g. `elapsed < 7.9s`, which HEAD clears by 400ms and the revert
misses by 400ms), or add a direct assertion that the stored deadline is not
a whole-second value.

---

## P2 findings

**P2-1 — the design doc does not contain the table both documents say it
does.** `CHANGELOG.fork.md`: *"all eight return sites were enumerated … and
the table is in `docs/design/session-lifecycle.md`"*.
`docs/checkpoints/2026-08-20-0600.md:14-15`: *"the table lives in the design
doc"*. `docs/design/session-lifecycle.md` was last modified by `95d7e0c1`
(the *previous* round), is not in this round's diff at all, contains no such
table, and contains **zero** occurrences of `verdictOnCtxDone`
(`grep -rn verdictOnCtxDone docs/` → empty). `f8ffb68c`'s own commit message
is honest and says the table "is in the task"; the two documents that a
future reader will actually consult point at a repository file that does not
have it. This is the same failure mode as the fabricated `fsync(2)`
citation the operator caught — an artifact asserted into existence — and it
is the citation supporting the exhaustiveness claim that P1-1 falsifies.

**P2-2 — the design doc is now stale, and nothing updated it.**
`session-lifecycle.md` §2 cites `run_queue_drain_session.go:403` for
`DrainSessionNow` (now **483**) and "строки 417–427" for the
DrainNoWork/`ctx.Err()` behaviour (now **497–507**, and the behaviour itself
moved into `verdictOnCtxDone` at 331-349). Both are off by exactly the +80
lines `f8ffb68c` inserted, which is at least a consistent, mechanical drift.
Everything the doc cites *below* line 269 is still correct — I checked
`:60`, `:218`, `219–226`, `227–237`, `238–243`, `246–251`, `:253`,
`262–269`, `34–39`, `run_queue_pump.go:251`, `run_queue_admission.go:32-63`:
all accurate. The sixth review already flagged line-number drift in this
file; the round that changed the code did not touch the doc.

**P2-3 — "Seven commits, `f8ffb68c` … `a1bfccbc`" — that range holds six.**
`docs/checkpoints/2026-08-20-0600.md:5-6`. `git log --oneline f8ffb68c^..a1bfccbc | wc -l`
→ **6**; the seventh is `cbd6e8d4`, the checkpoint commit that contains the
sentence. This is a verbatim repeat of the error the sixth review found in
the *previous* checkpoint ("nine commits … that range contains ten"), and it
is the sentence a future reader uses to reconstruct the round.

**P2-4 — "two need the zombie window and are load-bearing" is half true.**
`75c5ed52`'s message, the changelog and the checkpoint all name
`TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive` as one of
the two sites whose `reapInBackground=false` is load-bearing, and the
helper's doc warns *"If a future reader sees an unreaped holder here and
'fixes' it by flipping this to true unconditionally, they will silently
break the ordering tests above."* Flipping exactly that one site to `true`:

```
go test -count=5 -run TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive ./internal/cmd/
→ ok  github.com/charmbracelet/crush/internal/cmd  17.440s
```

Five consecutive passes. Reading the test explains why: its assertions
compare `kr.VictimGeneration` against `oldGeneration`/`currentGeneration`,
and its `require.Eventually` on the new owner's generation is independent of
when `forceKillHolder` returns. Only the *narrative* in the comment ("while
forceKillHolder has NOT returned yet") depends on the zombie window; no
assertion does. The **other** named site,
`TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire`, genuinely is
load-bearing — flipping it fails on
`sessions_kill_sweep_unix_test.go:436` ("a session whose lock a NEW owner
already holds must not be re-acquired"). Choosing `false` is harmless and
arguably more faithful to intent; the defect is the warning that overstates
what would break, in a comment written specifically to be trusted by the
next reader.

**P2-5 — the "ten-minute hang" could not be reproduced.** `75c5ed52`, the
changelog and the checkpoint all describe the first attempt's failure as
*"the suite went from 20 seconds to a ten-minute hang: in CI a stuck job,
not a red test"*, and the checkpoint elevates it to "the most instructive
failure this round". Flipping both zombie-window sites to `true` produced a
**FAIL in 6.578s**, not a hang. I could not fully reconstruct the original
"unconditional" variant (it may also have touched `spawnSweepTestNewOwner`'s
path), so I record this as *not reproduced* rather than *false* — but the
lesson the checkpoint draws from it is resting on an unverified observation.

**P2-6 — the "requires BOTH" half of `f4d19ca8` is unpinned.**
`app_run_errors.go:48-51` requires a constraint marker **and** `sessions.id`,
and both the commit and the checkpoint make a point of the conjunction.
Deleting the `sessions.id` half fails exactly one test
(`TestResolveSession_CreationRace_OtherConstraintNotSwallowed`) — good.
Deleting the **marker** half (`return strings.Contains(msg, "sessions.id")`)
leaves `internal/app` fully green. Nothing asserts that a non-constraint
error mentioning `sessions.id` is left alone. Low impact — the failure mode
degrades safely into one redundant `Get` and then the original error — but
the round's own stated invariant is only half covered.

Also on `f4d19ca8`, in its favour: I **verified** the claim the commit
itself labelled "reasoned rather than compiled". `ncruces/go-sqlite3@v0.34.2`
renders `CONSTRAINT` as `"sqlite3: constraint failed"`
(`internal/sqlite3_wrap/error.go:55-56`) and appends SQLite's own
`e.msg` (`error.go:34-48`), so the full text is
`sqlite3: constraint failed: UNIQUE constraint failed: sessions.id` — both
substrings present. The match holds on both drivers. Residual fragility is
low: SQLite's `"<KIND> constraint failed: <table>.<column>"` wording has
been stable for a decade, `FOREIGN KEY constraint failed` names no column so
it cannot collide, and the only realistic false positive
(`NOT NULL constraint failed: sessions.id`) degrades into the safe
fall-through. The dead branch is `"constraint violation"` — neither driver
ever emits it; it widens the match for nothing.

**P2-7 — the `#P1-2` lease-loss invariant is unpinned, and `#604` made it
worse.** Deleting `leaseLost.Store(true); execCancel()` from the renewal
loop's `!ok` branch (`run_queue_entry_exec.go:440-441`) leaves
`internal/session` green — `TestP1_2_ExecCtxCanceledOnLeaseLoss` and
`TestP1_2_OutcomeWriteSkippedOnLeaseLoss` both pass, because the watchdog
produces the same observable. This is **pre-existing**, not caused by this
round (it survives with `#604` also reverted). What `#604` changed is which
mechanism wins: with `TTL=100ms`, the watchdog's post-first-renewal deadline
drops from up to `t₀+950ms` (rounded) to `t₀+83ms` (true), so it now fires
inside those two tests where it previously did not. Counted over a full
`-v` run of `internal/session`: **8 watchdog firings at HEAD vs 6 with
`#604` reverted**, the two extra being `p1-2-lease-loss-probe` and
`p1-2-outcome-skip-probe`. Running the pair in isolation, one took each
path — it is now a coin flip. Not a correctness defect (the watchdog is the
stricter guarantee), but two tests named after a mechanism now frequently
never exercise it.

**P2-8 — `verdictOnCtxDone` masks a more specific prior failure, which is the
exact reason given for NOT unifying the other two sites.** `f8ffb68c`'s
commit message: *"two of them already record a MORE specific cause (leaseErr,
ErrCallQueuedNotExecuted), and since the ledger reports the most recent
failure, adding `ctx.Err()` there would mask the better diagnosis behind the
worse one."* But `verdictOnCtxDone` records `ctx.Err()` unconditionally when
`anyExecuted`, so at *its own* two sites an earlier row's real failure (a
terminal `AlreadyAttempted`, a failed Ack, a lost lease) is superseded as
`mostRecentFailure` by a bare `context.DeadlineExceeded`. The result is
`DrainFailed` either way, so no caller misbehaves today — but the argument
for the asymmetry does not survive contact with the code, and the
"priority rule inside the ledger" the message defers is needed at the two
unified sites too, not only at the two it declined to unify.

**P2-9 — the sixth review's five P2s are all still open**, and the round is
documented as having closed the sixth review's backlog. `P2-1`
(`DrainNoWork`'s const doc at `run_queue_drain_session.go:59` still claims
*"err is always nil when result is DrainNoWork"*, still false at lines 346,
641, 709 and 746), `P2-2` (`drainOutcomeError`'s `default` still has no
explicit `DrainNoWork` case and no unknown-value contract log), `P2-3`
(`procgroup_unix.go:50-53`), `P2-4`, `P2-5` — none touched. The sixth review
called them non-blockers and said P2-1/P2-2 "belong with the P0 fix". They
did not travel with it. Also cosmetic: `.githooks/pre-push`'s header still
enumerates three checks (lines 4-13) and does not mention the fourth it now
runs — though the substantive claim the sixth review flagged ("mirror the CI
gates") is now true.

---

## My own analysis of all eight `DrainSessionNow` return sites

Line numbers at HEAD. Columns are the state at the moment the site is
reached. "→0" means the pair reaches `drainOutcomeError` as `nil`, i.e. the
run exits 0 with a success envelope.

| # | site | line(s) | executed? | failure in ledger? | ctx | contended | returns | →0 with work left? |
|---|---|---|---|---|---|---|---|---|
| 1 | top-of-loop `ctx.Err() != nil` | 509 (`verdictOnCtxDone`) | no | — | dead | — | `(NoWork, ctx.Err())` | no — non-nil err, `default` arm returns `originalErr` |
| 1 | " | 509 | yes | none | dead | — | `(Failed, ctx.Err())` — **`#607` fix** | no |
| 1 | " | 509 | yes | some | dead | — | `(Failed, ctx.Err())` — masks prior cause (P2-8) | no |
| 2 | admission-wait `case <-ctx.Done()` | 561 (`verdictOnCtxDone`) | no | — | dead | — | `(NoWork, ctx.Err())` | no |
| 2 | " | 561 | yes | any | dead | — | `(Failed, ctx.Err())` — **`#607` fix** | no |
| 3 | observed-admission `stopNow` | 617 `verdict(true)` | no | — | alive | yes | `(NoWork, nil)` | **only if a future caller passes `originalErr == nil`** — sixth review's P2-2, still open |
| 3 | " | 617 | yes | none | alive | yes | `(Partial, ErrDrainIncomplete)` | no |
| 3 | " | 617 | yes | some | alive | yes | `(Failed, repErr)` | no |
| 4 | `LeaseRunQueueEntry` error | 641 / 649 | no / yes | — / any | alive | no | `(NoWork, leaseErr)` / `(Failed, leaseErr)` | no |
| 5 | `execSem` wait `ctx.Done()` | 709 / 712 | no / yes | — / any | dead | no | `(NoWork, ctx.Err())` / `(Failed, ctx.Err())` | no |
| 6 | pump stopping | 746 / 749 | no / yes | — / any | alive | no | `(NoWork, ErrCallQueuedNotExecuted)` / `(Failed, …)` | no |
| 7 | local-execution `stopNow` | 835 `verdict(true)` | no / yes+clean / yes+failed | — | alive | yes | `(NoWork, nil)` / `(Partial, ErrDrainIncomplete)` / `(Failed, repErr)` | same caveat as #3 |
| 8 | **nothing pending** | 962 `verdict(false)` | no | — | alive | no | `(NoWork, nil)` | no (queue genuinely empty) |
| 8 | " | 962 | yes | some (incl. re-check) | alive | no | `(Failed, repErr)` | no |
| 8 | " | 962 | **yes** | **none** | **alive** | **no** | **`(Complete, nil)`** | **YES — P1-1, when "nothing pending" only means "nothing in `status='pending'`"** |

Two observations the round's own table (wherever it lives) could not have
produced, because it was built against a single scenario — *"one row
committed, context dead, work remaining"*:

- Sites 1, 2, 4, 5, 6 are now all provably safe for **every** combination,
  not just the ctx-dead one. Sites 3 and 7 are safe under today's single
  caller and unsafe under a caller with a nil `originalErr` — a known,
  filed, non-blocking gap.
- **Site 8 was never in scope of that scenario at all**, because reaching it
  requires the context to be *alive*. Exhaustiveness over one axis was
  reported as exhaustiveness, full stop. That framing is what let the
  seventh form through, and it is worth more than the individual bug.

---

## Revert-check results (one production site at a time)

Each row: exactly **one** site changed, package re-run on real Linux, then
restored. `git diff` at the end is clean.

| # | Invariant | Site I reverted (one only) | What failed |
|---|---|---|---|
| RC1 | ctx death at loop top must be recorded before the verdict (`#607`, site 1) | `run_queue_drain_session.go:509` → pre-fix bare `ledger.verdict(false)` | **exactly 1**: `TestDrainSessionNow_CtxDiesAfterOneSuccess_NeverReportsCleanSuccess`. Site 2's test stayed green. |
| RC2 | ctx death while waiting on another admission must be recorded (`#607`, site 2) | `run_queue_drain_session.go:561` → pre-fix bare `ledger.verdict(false)` | **exactly 1**: `TestDrainSessionNow_ObservedAdmission_CtxDiesAfterOneSuccess_NeverReportsCleanSuccess`. Site 1's test stayed green. |
| RC3 | a ctx that dies before anything ran is `DrainNoWork`, not `DrainFailed` | `verdictOnCtxDone` — `if l.anyExecuted` guard removed | **exactly 1**: `TestVerdictOnCtxDone_NothingExecuted_ReportsNoWork`. |
| RC4 | the watchdog anchors on the true, pre-rounding deadline (`#604`) | `run_queue_entry_exec.go:422` → `time.Unix(newExpiresAt, 0)` | **NOTHING** — `ok internal/session 58.3s`. → **P1-2** |
| RC5 | a lost lease must cancel the in-flight run and skip the outcome write (P1-2, pre-existing) | `run_queue_entry_exec.go:440-441` — `leaseLost.Store(true); execCancel()` deleted | **NOTHING** — `ok internal/session 62.6s`. → P2-7 |
| RC6 | same as RC5, but also with `#604` reverted (is the gap this round's fault?) | RC5 + RC4 together | **NOTHING** — both `TestP1_2_*` still PASS. Gap is pre-existing. |
| RC7 | the constraint match must be narrowed to `sessions.id` (`#605`) | `app_run_errors.go:51` → `return true` | **exactly 1**: `TestResolveSession_CreationRace_OtherConstraintNotSwallowed`. |
| RC8 | `LiveHolderStillKilled` needs a concurrent reaper (`#606`) | `sessions_kill_test.go:180` → `reapInBackground=false` | **exactly 1**: `TestProbeThenKillHolder_LiveHolderStillKilled` (in the full CI fence command). |
| RC9 | `CapturesVictimGenerationWhileHolderAlive` needs the zombie window (claimed load-bearing) | `sessions_kill_sweep_unix_test.go:303` → `reapInBackground=true` | **NOTHING** — `-count=5`, `ok internal/cmd 17.4s`. → P2-4 |
| RC10 | `AcquireSessionLockForReset_SweepsOnlyAfterReacquire` needs the zombie window | `sessions_kill_sweep_unix_test.go:388` → `reapInBackground=true` | **exactly 1**, on its own assertion at `:436`. Genuinely load-bearing. |
| RC11 | the match requires a constraint MARKER as well (`#605`) | `app_run_errors.go:48-50` — marker guard removed | **NOTHING** — `ok internal/app 55.3s`. → P2-6 |

Three of eleven reverts broke nothing. Two of those three (RC4, RC11) are
invariants **this round introduced**.

Baseline for comparison, clean HEAD, same machine:

```
ok  github.com/charmbracelet/crush/internal/session  57.509s
ok  github.com/charmbracelet/crush/internal/cmd      71.773s
ok  github.com/charmbracelet/crush/internal/app      57.671s
```

Re-confirmed after all reverts were restored (66.9s / 88.0s / 59.7s), plus
`gofmt -l internal/` empty and `go vet` clean on all three packages. The two
`#606` tests the sixth review saw fail at clean HEAD are now green — that
regression is genuinely gone.

---

## Document-veracity audit

### Verified correct

- **All 13 commit hashes** cited across the changelog entry, the checkpoint
  and the commit messages resolve, and their subjects match how they are
  described: `f8ffb68c`, `929f3bb8`, `b7d50642`, `f4d19ca8`, `75c5ed52`,
  `a1bfccbc`, `cbd6e8d4`, `8393fa95`, `89642f01`, `d49573e8`, `95d7e0c1`,
  `377e4b94`, `638bc777`. **No invented hashes.**
- **"All 14 call sites across 9 files"** — exact.
  `grep -rn 'spawnKillTestLockHolder(t,' internal/` → 14 hits in 9 files.
  The 2 / 2 / 10 split adds to 14 and matches the diff (2 `true`,
  12 `false`, of which 2 are labelled load-bearing).
- **"97–930ms per renewal, up to 37% of the test's 2.5s margin"** —
  arithmetic checks (930/2500 = 37.2%), and the magnitude is **independently
  corroborated by my own instrumentation**: 780ms / 897ms / 851ms of donated
  slack across three runs, inside the claimed range.
- **"7.5059–7.5077s, spread 1.87ms"** — the spread is internally consistent
  (7.5077 − 7.5059 = 1.8ms). My own three uncontended samples land at
  7.5055 / 7.5063 / 7.5065s — marginally *below* the stated 7.5059s floor,
  which is expected for a different load profile, and the load-bearing claim
  ("always slightly above the theoretical 7.5s and never below") holds for
  mine too.
- **`f8ffb68c`'s "three sibling exits already recorded"** — exact: the
  lease-error path, the `execSem`-wait `ctx.Done()` path and the
  pump-stopping path all call `recordUnattributed` before their verdict.
- **"eight return sites"** — defensible. There are 11 physical `return`
  statements in `DrainSessionNow`, collapsing to 8 distinct branches once
  the three `NoWork`/`verdict(false)` pairs are counted once each.
- **`f4d19ca8`'s ncruces claim**, labelled by its own commit as reasoned
  rather than compiled — I verified it by reading
  `ncruces/go-sqlite3@v0.34.2/internal/sqlite3_wrap/error.go:55-56` and
  `error.go:34-48`. It holds.
- **The gate's 11% vs the fix's 11.7%** — `dynamic-release-gate.md:199`
  says 10/90; the commit says 14/120. Both round to ~11%; "matching the
  gate's independent figure" is fair.
- **`session-lifecycle.md` §2's structural line refs below line 269** —
  all nine I checked are still exact (see P2-2).
- **`build.yml` / `pre-push` pattern coverage** — `ForceStillKillsLiveHolder`
  is genuinely needed (`ForceKillHolder` does not match
  `TestSessionsReset_ForceStillKillsLiveHolder`), and the command selects
  28 tests, all green. `bash -n .githooks/pre-push` passes; `step`/`fail`
  are defined at lines 45-46, above the new block.

### Did not check out

1. **The eight-return-site table is not in `docs/design/session-lifecycle.md`.**
   Asserted by both the changelog and the checkpoint. The file is not in
   this round's diff and contains no such table. (P2-1)
2. **"There is now no seventh"** — false; see P1-1. (Changelog, checkpoint
   line 14, and `f8ffb68c`'s commit message.)
3. **"Seven commits, `f8ffb68c` … `a1bfccbc`"** — that range holds six.
   (P2-3)
4. **"two need the zombie window and are load-bearing"** — one of the two
   does not; five consecutive passes with it flipped. (P2-4)
5. **"the suite went from 20 seconds to a ten-minute hang"** — not
   reproduced; flipping both sites produced a 6.6s FAIL. (P2-5)
6. **`session-lifecycle.md`'s two references into `DrainSessionNow` itself**
   (`:403`, `417–427`) are off by exactly +80. (P2-2)

**No fabricated external citation was found in this round's documents.**
The single externally-checkable claim (`ncruces`'s error text) is true, and
I verified it against the module source. The document-veracity problem this
round is not invention of an outside source — it is invention of an
*internal* artifact (P2-1) and an overstated dependency (P2-4).

---

## What I could not check

- **Windows.** All dynamic evidence here is Linux (WSL2 Ubuntu-24.04,
  `go1.26.3`). The checkpoint's *"`go test ./...` clean on Windows"* at
  `a1bfccbc` is taken on trust, as are the linux/darwin cross-builds.
- **`14/120 → 0/120` and `20 rounds × 6 processes`.** Not re-runnable from
  the repository; it needs multi-process orchestration with isolated
  `CRUSH_GLOBAL_DATA` **and** `CRUSH_GLOBAL_CONFIG`, and I deliberately ran
  nothing that writes global config. Taken on trust; the mechanism is sound
  and `TestResolveSession_CreationRace_*` covers the code path.
- **The `ncruces` build.** Verified by reading the module source, not by
  compiling under its build tag — the same caveat `f4d19ca8` states, now
  narrowed from "reasoned" to "read", not eliminated.
- **The ~40% watchdog rate and the 7-in-15 measurement failures.** Per
  instructions, not reopened. I evaluated the mechanism (confirmed by direct
  measurement, P1-2) rather than the missing frequency statistics.
- **Playwright / `web/`.** Untouched by this round's diff; not run.
- **Project-wide `go test ./...`.** Out of scope per the sub-agent rules; I
  ran `internal/session`, `internal/cmd`, `internal/app` and the fence
  subset end-to-end.
- **P1-1 under a genuinely concurrent second process.** My probe simulates
  the foreign pump instance in-process (a different `PumpInstanceID` writing
  through the same `Service`), which is the same DB state a second process
  produces, but is not literally two OS processes.

---

## What would flip this to GO

1. Close P1-1 — either fix site 8, or state the boundary in the code and
   **retract the "no seventh form" claim** from `CHANGELOG.fork.md`,
   `docs/checkpoints/2026-08-20-0600.md` and, going forward, from commit
   messages. The retraction alone is arguably sufficient for the release;
   the claim is what makes it a blocker, not the bug.
2. Close P1-2 — tighten `TestP1_1_WatchdogCancelsAtTTLMinusMargin`'s upper
   bound (7.9s discriminates cleanly: HEAD 7.506s, reverted 8.29–8.40s), or
   assert directly that the watchdog's deadline is not whole-second-aligned.
3. Either put the eight-site table into `docs/design/session-lifecycle.md`
   (P2-1) or stop citing it there, and refresh the two stale line refs
   (P2-2).

P2-3 through P2-9 are not release blockers. P2-4's comment correction and
P2-3's commit count are one-line edits and should travel with the above,
since both are repeat instances of failure modes previous reviews already
named.

---

## Tree state

Five production files were temporarily reverted (one site at a time), two
test files temporarily edited, and one probe test created; all restored or
removed.

```
$ git diff --stat
 web/dist/.gitkeep | 0
 1 file changed, 0 insertions(+), 0 deletions(-)

$ git status --short
 D web/dist/.gitkeep
?? dev/
```

`web/dist/.gitkeep` and `dev/` are the pre-existing working-tree state
described in `CLAUDE.md` and were not touched. The only file this review
adds is this report.
