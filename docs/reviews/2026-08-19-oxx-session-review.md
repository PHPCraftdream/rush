# Review of the 23-commit session `da10303f..ea320f11` — 2026-08-19 (oxx)

Method: read the range, then **ran** things. All experiments were done in a
throwaway copy of the tree at `D:\dev\go\_crush_review_oxx` (the working tree
at `D:\dev\go\crush` was never modified — verified with `git status` before and
after). Unix-only code was exercised for real by cross-compiling the package
test binary (`GOOS=linux go test -c`) and running it under WSL2 Ubuntu 24.04.

Every finding below is labelled **[ran]** (I reproduced it), **[read]** (I
traced it in the source and it is deterministic from the code), or
**[hypothesis]** (plausible, not demonstrated).

---

## Verdict

**NO-GO.** Two release blockers, both **demonstrated by execution**, both
*inside the fixes written today*:

1. `DrainSessionNow` still returns `(drained=true, err=nil)` — the pair
   `app_run.go` converts into exit code 0 and a success envelope — after a row
   whose accepted work was permanently terminal-failed, or whose turn ran but
   was never committed, provided a **later, different** row in the same call
   commits cleanly. This is the sixth path.
2. On Unix, `sessions kill`'s child-group sweep fences on the generation it
   reads **after** the victim is dead. I ran it on Linux: it SIGKILLed the new
   owner's process group, dropped the dead holder's own entry as a "generation
   mismatch", and deleted the registry file.

Everything else I revert-checked in this range holds up. Five of the six
regression fixes I tested genuinely fail when their production change is
removed. One half of one fix has no coverage at all.

---

## Blockers

### B1. A later row's success erases an earlier row's failure → exit 0 over lost work  [ran]

`internal/session/run_queue_drain_session.go` uses the named return `err` as a
**session-scoped** accumulator and overwrites it unconditionally on every
classified outcome, at both sites (`:210-221` in the observed-admission branch,
`:375-394` in the executed-here branch):

```go
outcomeDrained, outcomeErr, stopNow := classifyBackgroundOutcome(execErr)
if outcomeDrained {
        drained = true
        err = outcomeErr          // <- no row identity anywhere
```

Nothing ties `err` to the row it describes. `lastErrRowID` — the one piece of
row identity in the function — is *cleared* by any later outcome, including a
**different** row's success, which also disables the cross-process re-check at
`:514-538` that exists to stop exactly this class of false success.

Reproduced with two probes against the real pump and a real SQLite DB
(`enqueue A, enqueue B` for one session):

| Row A outcome | Row B outcome | `DrainSessionNow` returned |
|---|---|---|
| terminal `AlreadyAttempted` (row **deleted**, work permanently lost) | clean Ack | `drained=true, err=<nil>` |
| Ack write fails (`ErrTurnCommitFailed`, row **still leased**, turn re-runs after TTL) | clean Ack | `drained=true, err=<nil>` |
| terminal `AlreadyAttempted` (control: A alone) | — | `drained=true, err=call already attempted` ✓ |

`RunNonInteractive` (`internal/app/app_run.go:740-808`) reads `drainErr == nil
&& drained` as "the durable continuation ran **and** the queue was fully
drained" and replaces the original cancellation with `nil`. So:

- the terminal case tells the operator their run succeeded while accepted work
  was permanently dead-lettered;
- the Ack case tells them it succeeded while the turn is still uncommitted and
  will be executed **a second time** after the lease expires — the operator has
  already been told it is finished.

Both are the exact failure class commits `638bc777`/`b788eb01` exist to close;
they closed the "first row" version of it and left the "second row" version
open. Nothing in `internal/session` catches it (`go test ./internal/session/`
is green at HEAD, with the defect present).

The function's own doc is what makes this look correct on the page — it
describes the overwrite as scoped to a row:

> "once a retry at **the same logical row** later resolves, that later
> resolution supersedes an earlier retryable failure **at the same row**…
> the immediate overwrite `err = outcomeErr` on every classified outcome"

The code has no notion of "the same row" at that point. See M2.

**Fix direction.** Carry row identity in the accumulator: a success may clear a
prior error only when the rowID matches; terminal failure, `ErrTurnCommitFailed`,
`errLeaseLost` and unconfirmed outcomes of one row must survive another row's
success. This is precisely the `NoWork / Complete / Partial / Failed` typed
aggregate the `0635c631` review's gate item 2 asked for and that was
implemented as a single extra sentinel instead (see "Gate items not done").

**Regression contracts that would have caught it:** `terminal A -> success B`,
`Ack-failure A -> success B`, `lease-loss A -> success B`, for both branches;
plus the existing `retryable A -> successful retry of A` which must stay a
success.

### B2. The Unix kill sweep fences on the generation that is on disk *after* the victim dies  [ran, on real Linux]

`forceKillHolder` (`internal/cmd/sessions_kill.go:351-392`) calls
`reportChildGroupSweep` **after** the holder PID is confirmed gone. On Unix the
lock is released by process death, so by then a new owner can already hold the
session. `KillRegisteredChildGroups`
(`internal/session/childgroup_registry_unix.go:411-478`) then reads the
**current** generation off disk and signals every entry that matches it.

Every acquire mints a fresh token (`lock.go:315`, `pid-nanotime`), so "current"
after a replacement means *the new owner's*.

Run on WSL2 Ubuntu 24.04 (holder A registers a real process group under gen A,
releases; owner B acquires — gen B — and registers its own group; then the
sweep runs, with `killpgFunc` stubbed to record targets rather than signal):

```
PROBE: victimPGID=383 innocentPGID=385 killedTargets=map[385:true]
       result={Killed:1 GenerationMismatch:true Implausible:0 Retained:0}
DEFECT CONFIRMED: the sweep SIGKILLed the NEW owner's process group (pgid 385)
DEFECT CONFIRMED: the dead holder's own registered group (pgid 383) was NOT reached
DEFECT CONFIRMED: the registry file was deleted, so a retry has no durable pointer left
```

Three failures at once, with **different reachability**:

- *Wrong target killed* — needs the new owner to have spawned **and
  registered** a CLI-provider group inside the window between "PID observed
  gone" (polled at 100 ms) and the sweep. Narrow. **[ran, but the window is
  artificial in my probe]**
- *Victim's entries dropped as "confirmed stale" and the registry file
  deleted* — needs only that **something re-acquired the lock**, which is one
  `flock` away and requires no provider at all. Wide. The rescue then reports
  "NOT reached — check for it manually" and has destroyed the only durable
  pointer to the orphaned tree. **[ran]**
- `reset --force` is worse ordered: `acquireSessionLockForReset` calls
  `forceKillHolder` (and therefore the sweep) **before** re-acquiring
  (`sessions_kill.go:448-476`), so the sweep is guaranteed to run in the window
  where a new owner may already be in. **[read]**

Related, same root: `childGroupFileMu`'s doc claims "the session lock itself
already guarantees only one process holds a given session at a time, so there
is only ever one legitimate writer". The **sweeping process does not hold the
lock** — it is a rescue tool running in a third process — so it can and does
rewrite the file concurrently with a new owner's `RegisterChildGroup`,
clobbering it with a retained snapshot. **[read]**

**Fix direction.** Capture `(holderPID, victimGeneration)` at the moment the
busy probe proves contention, pass it down as an immutable token, and sweep
only entries with the **victim's** generation. Hold the OS session lock across
read/verify/kill/rewrite; for `reset --force`, sweep after the re-acquire, not
before.

---

## High

### H1. Half of the partial-drain fix has no test coverage at all  [ran]

`b788eb01` added the same `if err == nil && drained { err = ErrDrainIncomplete }`
guard at both `stopNow` return points. Reverting **the observed-admission
(wait) branch** — `run_queue_drain_session.go:242-245` — back to
`return drained, nil` and running the **entire** `internal/session` package:
everything passes (only my own probes failed). Reverting **the executed-here
branch** fails all four `p588` tests, exactly as the file's recorded VERBATIM
output claims.

So the recorded revert-check is honest but covers one site; the other site is
production code with zero regression pressure on it. The uncovered branch is
reachable in production whenever a drain that already ran a row loses
admission to a same-pump background worker that comes back busy.

### H2. Streaming guard covers edit but not delete  [read]

`547b0815` correctly refuses edits to an assistant message that is not
terminally finished (`internal/server/handlers_messages.go:16-83`, plus the UI
hiding the pencil). The delete paths next to it have no such check:

- `handleDeleteMessage` → `a.Messages.Delete` directly (`:107-123`);
- `handleDeleteMessages` swallows per-ID errors and still replies
  `status: ok` (`:126-137`);
- `message.service.Delete` does an unconditional `DELETE`
  (`internal/message/message.go:172-192`).

The live turn keeps its `currentAssistant`. Its terminal write then takes the
"terminal update always wins" branch, which **hardcodes** `rowsAffected = 1`
(`message.go:292-358`) and publishes `UpdatedEvent` regardless of whether the
row still exists — so a deleted streaming message can reappear in the live UI
while being absent from SQLite, and disappear again on reload. Code path is
read-verified; the UI symptom is **[hypothesis]** (needs an e2e run).

---

## Medium

### M1. One of the five new Unix registry tests cannot detect what it names  [ran + read]

`TestWriteChildGroupFileLocked_NeverObservedTruncated` claims to pin "no reader
ever observes a truncated or missing file". Its reader loop `continue`s on
`os.IsNotExist` **and** on `content == ""` — i.e. it explicitly skips the exact
observation the atomic-rename fix exists to prevent. The only thing it can fail
on is a line with fewer than two fields, which needs a reader to land inside a
single small `write(2)`. It passes on Linux (I ran it), and would almost
certainly also pass against the pre-fix `os.WriteFile`. **[ran the test;
vacuity is read-verified, not demonstrated by reverting]**

The other four registry tests are sound, and all of them — plus every other
`!windows` test in the package — **pass on real Linux** (see "Verified by
running"). That closes the checkpoint's "five new registry tests have never
run" item, for Linux.

### M2. Comments that claim more than the code does

- `run_queue_drain_session.go:102-144` — the CONTRACT DECISION block describes
  the `err = outcomeErr` overwrite as bounded to "the same logical row". The
  code has no row identity at that point. This is the prose that made **B1**
  read as correct. **[read]**
- `run_queue_drain_session.go:503-506` — "still present and `'leased'` **by a
  DIFFERENT leasedBy**": the code only checks `current.Status == "leased"`. It
  errs conservative (an own stuck lease also reports `errLeaseLost`), but the
  claim is not implemented. **[read]**
- `childgroup_registry_unix.go:356-362` — `GenerationMismatch` entries are
  "CONFIRMED stale … safe to drop". "Can never match again" is not "no longer
  points at a live process"; under B2 these entries are precisely the live
  orphan tree the feature exists to reach. **[ran — this is what the probe
  showed]**
- `childgroup_registry_unix.go:108-114` — "only ever one legitimate writer" —
  false for the sweeping process (B2). **[read]**
- `run_queue_pump.go:185-211` — `errNoExecutionAttempted`'s doc enumerates "a
  shutdown-in-progress nack" as one of its cases, but `DrainSessionNow`'s own
  stopping path publishes `ErrCallQueuedNotExecuted`
  (`run_queue_drain_session.go:350`). Harmless today (a waiter stops instead of
  retrying, during shutdown), but it is a documented contract the code does not
  keep. **[read]**
- `childgroup_registry_unix.go:64-72,247-266` — "WRITE DURABILITY" /
  fsync-before-rename: correct for process kill and crash, but the parent
  directory is never fsynced, so the rename is not durable across power loss.
  Already the third review's P2-3; the wording still reads broader than the
  guarantee. **[read]**

### M3. Dead seam with a doc describing behaviour that no longer exists  [read]

`DrainSessionNowPollInterval`, `RunQueuePumpConfig.TestDrainSessionPollInterval`
and `(*RunQueuePump).drainSessionPollInterval` have no callers left after the
`admissionEntry.done` handoff replaced polling. Their docs still describe "the
poll granularity DrainSessionNow uses while waiting … the loop's only source of
added latency in that branch".

### M4. Sticky: the already-connected client can still get neither generation  [read]

`ff9efec1`'s sequence check correctly suppresses a queued-stale `v1` for a
client that registers after the map holds `v2`. But `BroadcastSticky` drops
`v2`'s own channel send when `stickyBroadcast` is full, and `v1` is then
suppressed on drain — an **already-connected** client receives neither. The
existing test only asserts the newly-registering client. The comment's "one
path or the other — never neither" is scoped to a *registering* client, so it
is accurate as written; the general "latest per type" promise is not. Same as
the third review's P2-1; unchanged, and acceptable while sticky is only the
update notice.

---

## Gate items from the three reviews that were not done

- **`0635c631` gate item 2** asked to *"replace the ambiguous drain result"*
  with a typed aggregate (`NoWork/Complete/Partial/Failed` or a separate
  `complete bool`), on the explicit argument that `(drained bool, error)` can no
  longer express the states. What shipped is one more sentinel
  (`ErrDrainIncomplete`) on the same two-value shape. **B1 is a direct
  consequence** — with a row-tagged typed outcome the overwrite could not have
  been written.
- **`0635c631` gate item 5 / `da10303f` gate item 6** (the dynamic gate:
  multi-process inject/drain stress, repeated shutdown during an outbox
  transaction, a real Unix process-tree kill) does not exist — but this is
  **stated openly** in both the checkpoint and `CHANGELOG.fork.md`, so it is
  not "quietly not done". I closed a piece of it here (the Unix suite now has
  a real Linux run; the process-tree *replacement* race is B2).
- Everything else in the two named reviews' gates is genuinely done: the
  `models.fast` / slot-clearing web paths, the CLI help/errors/`crush_info` /
  builtin-skill `smart|fast` sweep (no `large`/`small` occurrences remain
  outside one deliberate `models_set.go` redirect comment), the local + CI
  guards, the pinned config snapshot in `buildAgent`/`buildTools` (MCP
  exception documented rather than claimed away), the sqlc-drift/ASCII/slot-name
  CI steps, the watchdog disarm inside `joinTitle`, the outbox fallback's own
  budget, and the admission ABA.

---

## What I verified by running

All in the sandbox copy; `internal/session` and `internal/server` only.

| Experiment | Result |
|---|---|
| Probe: terminal-fail A + success B through the real pump | `(true, nil)` — **B1** |
| Probe: Ack-failure A + success B | `(true, nil)` — **B1** |
| Control: terminal-fail A alone | `(true, err)` ✓ |
| Revert `ErrDrainIncomplete` at the **wait** site, run whole package | green — **H1** |
| Revert `ErrDrainIncomplete` at the **executed-here** site | all 4 `p588` tests fail ✓ (matches the recorded output) |
| Reintroduce the ABA (refusal drops the entry, drain re-looks-up) | `p587` fails at 4.18 s on B's never-closed channel ✓ |
| Disable the `p.ctx` short-circuit in `processOrphanOutboxEntry` | `p589`'s call-site test fails ✓ |
| Disable the sticky sequence check in `hub.go` | `p547`'s new test fails ✓ |
| `processEntry`'s deferred release publishes `nil` again | both `p575b` tests fail ✓ |
| Wait branch ignores the outcome (pre-`#575` blind success) | 4 of 5 `p575` tests fail ✓ |
| `GOOS=linux go vet` + `go build` for session/cmd/cliprovider | clean |
| **Full `internal/session` suite on real Linux** (WSL2 Ubuntu 24.04, cross-compiled test binary) | **PASS**, including all five `p591` registry tests and `startTimeToken` |
| Probe on real Linux: sweep after a lock re-acquire | killed the new owner's pgid, dropped the victim's, deleted the registry — **B2** |
| `internal/session` on Windows at HEAD, unmodified | pass (~35 s, single run) |

Not run: the web e2e suite (13 min, and other agents may be holding ports), so
the "314 passed / 0 failed" claim is **unverified by me**. `go test ./...` was
not run per the task's scope rules.

## What is hypothesis

- The **wrong-target kill** half of B2 in the field: my probe registers the new
  owner's group before the sweep, which in reality requires a provider spawn
  inside a ~100 ms window. The *other* two halves (victim entries silently
  dropped, registry deleted) need only a lock re-acquire and are wide open.
- H2's UI symptom (a deleted streaming message resurrecting in the live UI).
  The server/DB path is read-verified; the front-end behaviour is not.
- M1's claim that `NeverObservedTruncated` would also pass pre-fix: read from
  the test's own control flow, not demonstrated by reverting
  `writeChildGroupFileLocked`.

## What I checked and found clean

- No admission leak: every `DrainSessionNow` return/continue path after a
  granted admission releases it, including the `execSem`/`stopping`/lease-error
  paths; `executeEntry` releases under `recover`. A waiter cannot be stranded.
- `admitSession`'s refusal genuinely returns the entry it lost to, under the
  same lock — the ABA is closed (proved by reverting).
- `classifyBackgroundOutcome`'s three categories are mutually exclusive and
  both branches funnel through it; `errNoExecutionAttempted` vs `nil` is
  correctly separated at every publisher except the one noted in M2.
- Orphan-outbox polarity: only a real `SQLITE_CONSTRAINT` quarantines; `p.ctx`
  cancellation short-circuits before the recorder. Both halves proved by
  reverting.
- The `large/small` → `smart/fast` sweep is complete in web, CLI help, errors,
  the builtin skill and `crush_info`, and is now guarded in the pre-push hook
  and CI, alongside the sqlc-drift and SQL-ASCII guards.
- Config pinning in `buildAgent`/`buildTools`/`RebuildSessionAgentCall`; the
  remaining `c.cfg.Config()` mentions in `coordinator_tools.go` are comments,
  not calls.
- Watchdog disarm is inside `joinTitle`'s `sync.Once` body, ahead of the
  bounded title join, and the `recordActivity` comments now match the code
  (the earlier false heartbeat claim is gone).
- The outbox fallback has its own fresh 30 s budget with the reasoning recorded
  accurately.
- `parallel_gate_test.go` bounds the package's own parallelism without touching
  any duration; the changelog's correction of its own earlier "15 → 0" claim is
  the right kind of honesty and matches what the code does.
