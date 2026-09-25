# Session fixes review backlog (2026-09-25)

This document records findings from the review of the
worktree `wrush-four-session-bugs-20260924` change set (reviewed
2026-09-25) that are deliberately NOT fixed in this cycle. Per the
fork's review-stop rule, P2 and P3 findings are backlog inputs, not
same-cycle fixes: this cycle shipped only the fixes required to make
the change set correct, and everything below is parked here so the
next cycle can pick it up with the full context of why the code
shaped up the way it did.

## 1. Fallback-ticker stop race in subAgentOutcomeRegistry

- ID: BL-2026-09-25-1
- Severity: P2
- Anchor: `internal/agent/subagent_outcome.go` — `tick` at lines
  581-611; `startTickerLocked` at 553-577; registry `close` at
  536-551 (tickStop handling spread across ~540-610).

Mechanism: the fallback tick goroutine evaluates `hasParked()` in one
critical section (line 601) and then, in a SEPARATE critical section
(lines 604-607), reads `r.tickStop`, sets it to nil, and closes that
channel after unlocking. A `park()` landing inside that window runs
`startTickerLocked` while the old stop channel is still installed, so
it returns early (`r.tickStop != nil`) without arming a new ticker;
the tick goroutine then closes the old one, leaving the registry with
parked entries but no running fallback ticker. Event-driven triggers
(`tryRelease` on completion/release events) still fire in that
window, so this is a safety-net gap, not a lost wakeup: the parked
row is only re-checked on the next park, which re-arms the ticker.

Why not fixed now: event triggers cover the wakeup in every reachable
path, so there is no correctness hole to close this cycle. Fixing it
properly means a re-arm invariant redesign (e.g. making park/tick
share a single critical section that decides together whether the
ticker must run, or holding a generation counter), which is a design
change that deserves its own cycle and its own determinism tests.

## 2. finishParked is not idempotent across row deletion

- ID: BL-2026-09-25-2
- Severity: P2
- Anchor: `internal/agent/async_job_registry.go` — `finishParked` at
  line 164; `releaseLocked` at line 189; intent comment at 147-163.

Mechanism: `finishParked` does not check whether the
`(sessionID, toolCallID)` row was already released and delivered.
`releaseLocked` deletes the delivered row, so a hypothetical second
`finishParked` for the same key would re-insert the completion and
re-deliver it to the CLI queue / web callback, double-delivering the
sub-agent's outcome.

Why not fixed now: unreachable once this cycle's `release()`
claim-first latch fix is in place — `finishParked` is called only
from the registry's own emit path, and the latch guarantees that path
runs exactly once per job. Recorded as an invariant to preserve: if a
new `finishParked` call site ever appears (e.g. a retry or replay
path), it must either make `finishParked` idempotent (check-then-act
under the mutex before inserting) or go through the same latch.

## 3. checkEditFileSize TOCTOU

- ID: BL-2026-09-25-3
- Severity: P3
- Anchor: `internal/agent/tools/edit.go` — `checkEditFileSize` at
  line 71 (bound `editMaxFileSizeBytes`, 256 MiB, at line 60); the
  stat-then-read pair at lines 343 and following.

Mechanism: `checkEditFileSize` validates `fileInfo.Size()` from a
`Stat` call, and `os.ReadFile` runs afterwards. A concurrent writer
can grow the file between the stat and the read, so the bytes
actually allocated and read can exceed the 256 MiB bound. The
comparison itself is correct (size <= bound passes), and the impact
is bounded by how fast a writer can grow one file within the tool
call's own execution window.

Why not fixed now: correctness impact is bounded and the off-by-one
boundary is right; a real fix means reading with a hard size cap
(`io.LimitReader` plus a truncation error, or checking the read
length after the fact and failing closed), which changes edit-tool
error taxonomy and needs its own tests. Parked for a dedicated
hardening pass rather than bolted onto this bug-fix cycle.

## 4. False-alive edges in descendant liveness

- ID: BL-2026-09-25-4
- Severity: P3
- Anchor: `internal/session/descendant_liveness.go` — live-decision
  site at line 119 (`InspectSessionLock(dataDir, child.ID,
  LockStaleDuration)`); heuristic documentation at lines 60-78.
  Related bounds: `maxPidFallbackAge` (60m) and `LockStaleDuration`
  (20s) in `internal/session/lock.go`.

Mechanism: two documented false-alive edges. A crashed child whose
lock-file mtime is younger than `maxPidFallbackAge` (60 minutes) and
whose recorded PID was reused by an unrelated live process reads as
`Live=true`, pinning the parent's "delegating" state for up to 60
minutes. A just-released child (OS lock dropped, mtime not yet older
than `LockStaleDuration`) reads as live for up to 20 seconds. Both
are inherent to PID/mtime heuristics, not implementation slips.

Why not fixed now: these are documented trade-offs of the heuristic
design (a mtime fresh-PID fallback and a short stale window exist to
avoid the opposite failure — declaring a live holder dead and
letting another process steal the session). Removing them requires a
stronger liveness oracle (e.g. durable holder tokens or start-time
comparison in `/proc`), which is a design change beyond this cycle's
scope.

## 5. annotateLiveDescendantWork cost per session list reply

- ID: BL-2026-09-25-5
- Severity: P3
- Anchor: `internal/server/handlers_sessions.go` —
  `annotateLiveDescendantWork` at line 250, called at line 279 on
  every session list reply.

Mechanism: for every session on every list reply, the annotation
walks the full descendant tree and collects lock stats (one
inspection per child), giving O(sessions × descendants) work per
poll of the sessions list. At large workspace sizes (hundreds of
sessions, deep delegation chains) this is a per-request latency
amplifier.

Why not fixed now: this is a performance note for large workspaces,
not a correctness bug, and the current cycle is already changing the
sessions code paths it touches. Caching lock stats briefly (per
child, with the session-lock staleness window as a TTL) or batching
the tree walk across sessions should be designed and benchmarked on
a realistic workspace shape rather than squeezed into the bug-fix
cycle.

## 6. titleJoinGrace default raised from 5s to 10s

- ID: BL-2026-09-25-6
- Severity: Note (intentional change, recorded for release notes)
- Anchor: `internal/agent/agent_title.go` — `titleJoinGrace` at line
  74 (doc comment at lines 35-40).

Mechanism: `titleJoinGrace` bounds how long `runTurn`'s join waits
for session-title generation before yielding without it. The default
rose from 5s to 10s, so a turn can now wait up to 10 seconds for a
title to persist before proceeding. The extra wait is intentional:
it lets more titles (especially slower model responses) actually
persist instead of being discarded.

Why recorded: not a fix or a defect — an intentional behavior change
from this cycle that belongs in the release notes so the latency
trade-off is visible to anyone benchmarking turn latency.

## 7. Web UI does not yet render the live-descendant session fields

- ID: BL-2026-09-25-7
- Severity: P3
- Anchor: `internal/server/handlers_sessions.go` —
  `annotateLiveDescendantWork` at line 250; `web/src/types.ts` — the
  `Session` interface's `HasLiveDescendantWork` / `LiveDescendantIDs`.

Mechanism: the Go session-list API payload and the TS types both ship
`HasLiveDescendantWork` / `LiveDescendantIDs`, but no tab in web/src
renders them, so the UI cannot yet name the sub-agent a session is
waiting on.

Why not fixed now: needs UI/UX work in web/src (list badge or detail
line) plus a Playwright assertion; the CLI `sessions why` already names
live descendants.
