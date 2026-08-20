# Post-gate independent review — 2026-08-20 (`@oxx`)

Scope: the eleven unpushed commits `537d93df` … `8d534658`
(`git log --oneline 8393fa95..HEAD`), i.e. the backlog closure of the
FOURTH and FIFTH reviews plus the first dynamic gate. Prior reports read:
`2026-08-19-release-readiness-static-follow-up-d3ee9841.md`,
`2026-08-19-oxx-session-review.md`, `2026-08-20-dynamic-release-gate.md`.

Everything asserted below as a defect was **reached by running code**, not
by reading it. Every claim that could not be run is listed in the final
section instead of being softened into prose.

---

## Verdict: **NO-GO**

One P0 and one P1. The P0 is the answer to the question this review was
commissioned to ask.

**There is a sixth form.** The `DrainSessionNow` contract still admits
`(DrainComplete, nil)` — the one pairing the code, the design doc and
`drainOutcomeError` all agree means "the continuation fully completed, exit
0 is authorised" — in a state where the call executed one row, the caller's
context expired, and **real pending work was left in `session_run_queue`
untouched**. This is the same false-success class as `#575`, `#588`, `#592`
and `#593`, reached through the fifth of five early-exit paths rather than
through the ledger. Reproduced on Linux with a purpose-built probe test
(deleted afterwards; evidence verbatim below).

The P1 is that the gate's own stated blocker is only half closed: `377e4b94`
wired the fence suite into `.github/workflows/build.yml` but not into
`.githooks/pre-push`, whose header comment still claims it mirrors
`build.yml`.

Everything else in the round holds up. The row-ledger rewrite, the
generation fence, the streaming-delete guard and the coalescing sticky
channel are each correct as far as I could drive them, and — unlike the
three cases the operator caught this round — each is genuinely pinned: all
four revert-checks below failed the *specific* covering test and nothing
else.

---

## P0-1 — `(DrainComplete, nil)` with pending work, on caller-context expiry

**Where:** `internal/session/run_queue_drain_session.go`, two return sites:

- lines **417–427** (top-of-loop `if ctx.Err() != nil`)
- lines **468–476** (`case <-ctx.Done():` inside the observed-admission wait)

Both do:

```go
result, verdictErr := ledger.verdict(false)
if result == DrainNoWork {
        return DrainNoWork, ctx.Err()
}
return result, verdictErr
```

Neither records the cancellation into the ledger. When an earlier iteration
committed a row cleanly, `ledger.anyExecuted == true` and `len(failed) == 0`,
so `verdict(false)` returns **`(DrainComplete, nil)`** — while the loop has
provably *not* established that the queue is empty. It stopped because the
caller's deadline fired, not because `LeaseRunQueueEntry` came back empty.

The asymmetry is the tell. The **other three** early-exit paths in the same
function all do record before returning:

| line | path | records? |
|---|---|---|
| 417–427 | ctx expired at loop top | **no** |
| 468–476 | ctx expired while waiting on another execution | **no** |
| 552–562 | `LeaseRunQueueEntry` returned an error | yes — `recordUnattributed(leaseErr)` |
| 620–625 | ctx expired waiting for an `execSem` slot | yes — `recordUnattributed(ctx.Err())` |
| 657–662 | pump is stopping | yes — `recordUnattributed(ErrCallQueuedNotExecuted)` |

Site 468–476 is the worse of the two: it abandons a wait on **someone else's**
`admissionEntry.done` without ever reading `otherEntry.err`, and still reports
`DrainComplete`. The outcome it exists to observe is never observed.

### How it is reached

Probe placed at `internal/session/zz_reviewer_probe_test.go` (created, run,
deleted — `git status` clean, shown at the end). Two durable rows A and B for
one session; the fake coordinator cancels the drain's own ctx during row A's
turn and then returns success, exactly as a `--timeout` firing mid-continuation
would:

```
$HOME/sdk/go/bin/go test ./internal/session/ -run TestZZProbe -count=1 -v
```

```
zz_reviewer_probe_test.go:45: PROBE RESULT: result=complete err=<nil> coordCalls=1
zz_reviewer_probe_test.go:55: PROBE PENDING ROWS LEFT: 1
    Error: Should not be: 1
    Messages: PROBE: DrainComplete with 1 rows still pending after the caller's ctx expired
--- FAIL: TestZZProbe_SuccessThenCtxCancel (0.37s)
```

`Ack` survives the cancellation because `executeEntrySync`'s outcome writes use
`newDBCtx()` rooted in `context.Background()`
(`run_queue_entry_exec.go:64-66`), so row A genuinely commits; only the *loop*
dies. Row B is untouched, `attempts = 0`, still `pending`.

### Why this reaches the operator

1. `internal/cmd/run.go:626-629` wraps the ctx handed to `RunNonInteractive`
   in `context.WithTimeout(ctx, timeoutDur)` — the same ctx passed to
   `DrainSessionNow` at `app_run.go:777`.
2. The drain is gated on `isCanceled`, whose dominant production trigger is
   `agent.ErrRequestCancelled` from a cross-process
   `crush sessions inject --interrupt` — at that moment the ctx is still
   **alive**, so the drain starts and runs a real provider turn (seconds to
   minutes). `--timeout` expiring inside that window is the ordinary case,
   not an exotic one.
3. `drainOutcomeError(sessID, DrainComplete, nil, originalErr)` returns `nil`
   (`app_run.go:253-261`).
4. The select loop then has **both** `drainDone` and `ctx.Done()` ready.
   Go picks uniformly at random. Roughly half the time it takes
   `case drainErr := <-drainDone:` → `finish(nil)` → `hookExitReason = "stop"`
   → **exit 0 with a success envelope**, over a durable row that never ran.

Even discounting the 50 % race, `DrainSessionNow` is a package-level API whose
`DrainComplete` doc says "every row this call executed or waited on resolved as
a genuine, confirmed commit". Here it waited on nothing and confirmed nothing.

### No test pins the current behaviour

`TestDrainSessionNow_CallerCancellationStopsExecution`
(`p551_p0_2_drain_context_lifecycle_test.go:60`) discards the result:
`_, _ = pump.DrainSessionNow(ctx, sess.ID)`. So a fix collides with nothing.

### Minimal fix (not applied — reporting only)

Make both sites match the three that already behave. At 417–427 and 468–476,
before `return ledger.verdict(...)`:

```go
ledger.recordUnattributed(ctx.Err())
return ledger.verdict(false)
```

which yields `DrainFailed` + a non-nil error, consistent with line 620–625.
`DrainPartial` via `verdict(true)` is the weaker alternative but reuses
`contended` for a non-contention reason; `recordUnattributed` is the honest
one — this call cannot vouch for the queue either way.

---

## P1-1 — the gate's blocker is half closed: `pre-push` never got the fence step

`docs/reviews/2026-08-20-dynamic-release-gate.md:60-62` states the sole reason
for its NO-GO as: *"the fence suite this whole effort was about is not executed
by CI **or pre-push** at all"*, and line 105-108 names both
`.githooks/pre-push` and `.github/workflows/build.yml:79`.

`377e4b94` added the fence step to `build.yml` only. `.githooks/pre-push` still
runs exactly three `go test -short -failfast ./...` invocations (lines 177,
180, 188, 192) and nothing else — verified by grep. Its own header comment,
line 9, reads:

```
#   3. go test -short -failfast ./... — like build.yml, minus -race (that needs
```

That claim is now false: it is also *minus the entire process-group/lock-fence
suite*. CLAUDE.md lists "Pre-push hook mirroring CI" as a fork crown jewel; the
mirror is broken by this round's own commit, in the one place five consecutive
reviews said was the gap.

I confirmed the CI half genuinely works — the exact `build.yml` command run
against real Linux:

```
go test -failfast -run 'ChildGroup|Registry|WriteChildGroupFile|VerifyGroupStillPlausible|Sweep|CapturesVictimGeneration|OrphanedGenerationCaptured|StalePIDNotKilled|ForceKillHolder' ./internal/session/ ./internal/cmd/
→ 25 tests run, ok internal/session 5.555s, ok internal/cmd 15.035s
```

and that it correctly excludes the two `#606` zombie tests. So this is a
one-line omission, not a design problem — but it is exactly the finding the
gate was run to produce, left half-fixed.

---

## P2 findings

**P2-1 — `DrainNoWork`'s own doc contradicts five return sites.**
`run_queue_drain_session.go:59` — *"err is always nil when result is
DrainNoWork."* False at lines 425, 471, 554, 622 and 659, all of which return
`DrainNoWork` with a non-nil error. The type-level doc (lines 29-33) states the
correct rule; the const-level doc states a stronger one that the code does not
honour. In a contract that has been mis-fixed four times, a false invariant in
the doc a future fixer will read first is not cosmetic.

**P2-2 — `drainOutcomeError`'s `default` is fail-safe only by accident.**
`app_run.go:273-280` folds `DrainNoWork` and every future enum value into
"return `originalErr`". Today that is safe *only* because the single call site
is gated on `isCanceled`, so `originalErr` is non-nil by construction. A second
caller passing `originalErr == nil`, or a new `DrainResult` value, turns the
default into a silent exit 0. The `DrainComplete`/`Partial`/`Failed` arms all
carry explicit contract assertions with `slog.Error`; this one carries none.
Recommend an explicit `case session.DrainNoWork:` and a `default:` that logs an
unknown-value contract violation and fails closed.

**P2-3 — the identity-capture rule (#594) has one more re-derivation site.**
`internal/agent/cliprovider/procgroup_unix.go:50-53` calls
`session.ReadLockGeneration(lockPath)` to stamp `RegisterChildGroup`, rather
than carrying the `generation` field of the `SessionLock` this process actually
holds (`internal/session/lock.go:178`). It is currently *safe* — the process
holds the lock, and `clearHolderMetadata` is generation-checked so no other
holder's cleanup can clobber the sidecar — so this is not a live defect. But it
is the same shape the rule forbids (identity re-read from a shared mutable
location instead of captured from the owning object), and its failure mode is
silent: a missing sidecar yields `""`, the registration is skipped, and the
child tree becomes unsweepable with no log line.

I checked the rest of the codebase for the strict pattern ("proved ownership →
destroyed it → re-read identity") and found **no other violation**:
`KillRegisteredChildGroups` is passed the token by argument and deliberately
never reads it (`childgroup_registry_unix.go:493-497`); `SessionLock.Release`
captures path+generation by value for its background goroutine
(`lock.go:420-451`); both `sessions kill` and `reset --force` capture before
the acquire and before the kill. `lockHolderProvablyDead` →
`removeLockWithRetry` (`sessions_kill.go:233-239`) is a prove-then-relinquish
TOCTOU but carries no identity and is documented as accepted.

**P2-4 — an orphaned unfinished assistant row can never be compacted away.**
`agent_compaction.go:508` and `:691` call `messages.Delete`, which now returns
`ErrMessageStillStreaming` for a non-summary assistant row with no
`finished_at`. Both sites `slog.Warn` and continue, so nothing breaks — but
such a row is now permanently un-compactable and will be re-summarised on
every future compaction. `ForceDelete` exists but only the WS handlers use it.
Low impact; worth a note in the code rather than a fix.

**P2-5 — `deleteMessageRescuingOrphan` is a check-then-act.**
`handlers_messages.go:151-162` calls `IsSessionBusy`, then `ForceDelete`. A
turn starting in the gap gets its live message force-deleted. Narrow
(operator-initiated, idle session) and strictly better than the pre-`#595`
unconditional delete, but the same shape as the rule this round formalised.

---

## Revert-check results (one production site at a time)

Invariants taken from `docs/design/session-lifecycle.md` §3. Each row: exactly
**one** site reverted, full package re-run on real Linux
(WSL2 Ubuntu-24.04, `go1.26.3`), then restored.

| # | Invariant | Site I reverted (one only) | What failed |
|---|---|---|---|
| 1 | Row identity when recording a failure | `rowLedger.recordSuccess` — replaced the `delete(l.failed, rowID)` guard with `l.failed = make(map[string]error)` (the pre-`#592` "any success clears err") | **6 tests**: `TestDrainSessionNow_{Local­Execution,ObservedAdmission}_{Terminal,AckFailure,LeaseLoss}ThenSuccess_ReportsFailed` (`p592_cross_row_identity_test.go`). `TestP593RecordSuccessClearsNilFailure` correctly stayed green (same-row clear is legal). |
| 2 | Identity captured BEFORE ownership is relinquished (#594) | `probeThenKillHolder` — moved `victimGeneration := session.ReadLockGeneration(...)` from before `forceKillHolder` to after it | **exactly 1 test**: `TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive`, on its own assertion — *"VictimGeneration must be the token captured while the VICTIM still held the lock"*. The other five sweep tests stayed green, i.e. the coverage is targeted, not an accidental union. |
| 3 | Failed beats Partial | `rowLedger.verdict` — moved the `if contended` branch ahead of `if len(l.failed) > 0` | **3 tests**: `TestDrainSessionNow_PartialDrain_{Retryable,Terminal,Ack}ErrorThenBusy_PreservesError` (`p588_partial_drain_test.go`). `..._SuccessThenBusy_...` correctly stayed green (no failures in that ledger). |
| 4 | No nil error may survive into a `DrainFailed` verdict | `rowLedger.recordFailure` — deleted the `if outcomeErr == nil` substitution | **1 test**: `TestP593NilFailureViaRecordFailure` (`p593_nil_failure_verdict_test.go`). |

**No invariant I tested was unpinned.** The "correct fix, nothing pins it"
category the operator caught three times this round does not appear in the four
I sampled.

Baseline for comparison (clean HEAD, same machine):
`internal/session`, `internal/app`, `internal/message`, `internal/server`,
`internal/db`, `internal/agent` — all `ok`. `internal/cmd` — only the two
pre-declared `#606` failures (`TestProbeThenKillHolder_LiveHolderStillKilled`,
`TestSessionsReset_ForceStillKillsLiveHolder`), with the documented
"still alive after 5s wait" / "could not confirm the lock holder is dead"
texts. Nothing new.

---

## Document-veracity audit

Prompted by the fabricated `fsync(2)` citation the operator caught. I checked
every externally-checkable reference in this round's documents.

### Verified correct

- **All 23 commit hashes** cited across `session-lifecycle.md`,
  `CHANGELOG.fork.md`, the gate report and the checkpoint resolve, and their
  subjects match how they are described: `a1c5f074`, `638bc777`, `b788eb01`,
  `344dd37c`, `3ba00874`, `0d272f70`, `f627cbac`, `05a1708f`, `b6549640`,
  `984b8cd9`, `6fe0108b`, `d49573e8`, `95d7e0c1`, `377e4b94`, `641c0c20`,
  `8d534658`, `537d93df`, `8393fa95`, `d3ee9841`, `0635c631`, `c14c9b8d`,
  `5c323b55`, `b46dae6c`. **No invented hashes.**
- **All test names cited as "pinned by"** exist and are spelled correctly —
  the six `p592` tests, the four `p588` tests,
  `TestDrainSessionNow_SameRowRetrySucceedsClearsErr` at
  `p578_stale_error_test.go:65` (exact line), the four `p593` tests, the three
  `sessions_kill_sweep_unix_test.go` fence tests, and
  `TestP1_4_CleanupUsesCancelImmuneContext` at
  `internal/agent/p1_3_p1_4_regression_test.go:350`.
- **Quoted source text** is verbatim where quoted: "root cause of a real
  defect" (`childgroup_registry_unix.go:495`, inside the cited 474–497 range);
  "Capture the victim's generation token HERE, before forceKillHolder signals
  anything"; the `agent_run_test.go:49-53` VCR skip comment; the
  `ErrRowOutcomeUnconfirmed` doc at `run_queue_pump.go:158-163`.
- **Structural line ranges** in `session-lifecycle.md` §3 are accurate:
  `run_queue_drain_session.go:172-192` (recordSuccess/recordFailure),
  `203-212` (recordUnattributed), `227` / `246` (the verdict branch order this
  invariant is about), `219-226`, `238-243`, `262-269`, `845-873` (re-check),
  `357-381` (contract decision), `14-53` (`DrainResult` doc);
  `run_queue_admission.go` "строки 142–154" (the `sync.Once` release closure);
  `app_run.go:251-281` (`drainOutcomeError`); `run_queue_pump.go:33/158/211/251`.
- The design doc's honest hedges are honest: `TestTick` does exist
  (`run_queue_pump.go:267-269`), and §3 invariant 4 explicitly flags that it
  did not read the `p593` test bodies — which is true and correctly labelled.
- Gate report Part 3/4 arithmetic checks out against the changelog's summary
  (20 + 25 = 45 SIGKILLs; 15 × 6 = 90 invocations).
- I also checked the CLAUDE.md global-config hazard against
  `internal/cmd`'s own tests, since a Linux run printed
  `set smart = ... in global scope`: **unfounded** — `models_use_test.go:56-84`
  isolates both `CRUSH_GLOBAL_DATA`/`XDG_DATA_HOME` **and**
  `CRUSH_GLOBAL_CONFIG`/`XDG_CONFIG_HOME`, exactly as CLAUDE.md requires.

### Did not check out

1. **`session-lifecycle.md` §3 invariant 2 — `sessions_kill.go:377`.**
   The `victimGeneration := session.ReadLockGeneration(...)` line the whole
   invariant is about is at **line 479**. Line 377 is an unrelated fragment of
   `probeThenKillHolder`'s doc comment ("behavior fell back to unconditionally
   killing whatever PID was"). The doc writes "строка ~377" with a tilde, but
   a 102-line miss points a future reader at the wrong construct.
2. **`session-lifecycle.md` §1 — `lock.go:317`** cited for the
   `"PID-nanoseconds"` generation token. That is `_ = f.Truncate(0)`; the
   `fmt.Sprintf("%d-%d", myPID, time.Now().UnixNano())` is line **316**.
   Off-by-one, trivial.
3. **`session-lifecycle.md` §5 describes `#602` as still open** — *"задача
   #602 … открыта на момент написания"* — but `6fe0108b`, the commit that
   closes it, precedes `95d7e0c1` (the doc's own commit) in this same branch.
   The *residual* gap the bullet describes (an entry whose generation never
   becomes any future sweep's victim) is real, but its framing as an open task
   is wrong at the doc's own commit.
4. **Gate report line 106** states CI runs `go test -short -failfast ./...`.
   `build.yml:79` is `go test -short -race -failfast ./...`. The `-race`
   omission does not change the report's conclusion.
5. **Checkpoint `2026-08-20-0130.md` line 6** — *"nine commits `537d93df` …
   `641c0c20`"*. That range contains **ten**; the full round is **eleven**
   (through `8d534658`). The task prompt inherited "nine" from here.
6. **CI comment vs. CI behaviour.** `build.yml`'s new step says it is "NOT a
   blanket run" with a deliberately narrow list. The `Sweep` alternative also
   selects the re-exec helper `TestHelperSweepTestNewOwner`. Harmless (it
   passes), but the comment overstates the precision of the pattern.

**No fabricated external citation was found in this round's documents.** The
one genuinely external claim (`fsync(2)`) is the one already replaced by the
operator; nothing else in these files cites a man page, RFC or upstream
document.

---

## What I could not check

- **Windows.** All dynamic evidence here is Linux (WSL2 Ubuntu-24.04,
  `go1.26.3`). The changelog's *"`go test ./...` clean on Windows"* at
  `641c0c20` is taken on trust.
- **Playwright `323 passed / 0 failed`.** Not re-run; no browser session was
  started. The `web/` diff (`Message.tsx`, `AssistantHoverActions.tsx`,
  `message-delete.spec.ts`) was read but not executed.
- **The gate report's Part 1 numbers** (39 packages, 3622 sub-tests, ~26 min).
  Re-running a full no-`-short` `./...` was outside this review's budget; I
  re-ran the four packages that matter to this diff plus `internal/db` and
  `internal/agent` instead, and the *fence subset* end-to-end.
- **The P0's second site (lines 468–476)** is argued statically, not probed.
  Constructing "row A commits here, then this call loses admission to a
  background worker, then ctx expires mid-wait" deterministically needs a live
  second pump; the code path is structurally identical to the site I did
  reproduce (same `verdict(false)`, same missing record), so I rate it
  CONFIRMED-by-symmetry rather than CONFIRMED-by-run, and say so.
- **`#604`/`#605`/`#606`** were not re-measured — per instructions, treated as
  known and not reopened. I did observe `#606` reproduce identically at clean
  HEAD, which corroborates the filing.
- **Whether a 50 % select race is the real production rate.** I did not
  instrument `RunNonInteractive`'s select; "uniform random among ready cases"
  is the Go spec guarantee, not a measurement.

---

## Tree state

Four production files were temporarily reverted for the checks above and one
probe test was created; all restored/removed.

```
$ git status --short
 D web/dist/.gitkeep
?? dev/

$ git diff --stat
 web/dist/.gitkeep | 0
 1 file changed, 0 insertions(+), 0 deletions(-)
```

`web/dist/.gitkeep` and `dev/` are the pre-existing working-tree state
described in CLAUDE.md and were not touched. The only file this review adds is
this report.

---

## What would flip this to GO

1. Close P0-1 at both return sites, with a test that fails when either site's
   record is removed **individually** (the round's own standard).
2. Add the fence step to `.githooks/pre-push`, or correct its header comment to
   say plainly that it no longer mirrors `build.yml`.

The P2 items are not release blockers. P2-1 and P2-2 are cheap and belong with
the P0 fix, since all three live in the same contract.
