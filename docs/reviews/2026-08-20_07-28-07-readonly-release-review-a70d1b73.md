# Read-only release review: multi-agent/session stability

- Timestamp: **2026-08-20 07:28:07 +02:00** (Europe/Berlin)
- Reviewed HEAD: `a70d1b73f044727dedf4589a61b93accbef69c14`
- Branch: `main`
- Main comparison range: `d3ee9841..a70d1b73` (**23 commits**)
- Diff size: **102 files, +10,380 / -948**
- Method: static inspection of Git history, diffs, production code, SQL,
  related tests and prior review artifacts.
- Tests/build/lint/race detector/application: **not run**, per request.
- Repository was already dirty before this review:
  `D web/dist/.gitkeep`, `?? dev/`. Those paths were not touched.

## Verdict

**NO-GO for a stable release.**

The recent fixes close several real defects, but not all release blockers.
There is still a reproducible-by-inspection control-plane freeze in the WebSocket
dispatcher and at least two ways for `DrainSessionNow` to report a terminal
outcome that does not describe all accepted work. The rerun handler also has two
independent transcript-corruption/data-loss races.

I did not find a new classic circular mutex deadlock in the changed
session/child-process/message code. The dangerous hangs are logical deadlocks:
the goroutine capable of reading a cancel command can block before it reads that
command, and drain can confuse “not pending” with “fully resolved”.

Release-blocking findings:

| ID | Severity | Area | Result |
|---|---:|---|---|
| F1 | P0 | WebSocket dispatch | The read loop can block behind ordinary work, so later Cancel/Interrupt frames are never read. |
| F2 | P0 | Durable drain | A foreign `leased` row is invisible to the pending-only scan; an earlier success can turn this into `(DrainComplete, nil)`. |
| F3 | P0 | Admission outcome | Max-attempt terminal deletion is published as “no execution attempted”; a waiting drain can lose the failure and return Complete. |
| F4 | P1 | Admission row identity | An observed retryable failure cannot be cleared by a later success of the same row, producing a false failure. |
| F5 | P1 | Rerun ownership | Idle polling is a TOCTOU check; rerun can force-delete a newly active streaming message. |
| F6 | P1 | Rerun ordering | Same-second messages before the target are deleted because UUID inequality is used as an ordering relation. |

## Findings

### F1 — P0: Cancel/Interrupt can be trapped behind the work semaphore

Evidence:

- `internal/server/server.go:211-220` reads one WebSocket frame and calls
  `handleIncoming` synchronously before reading the next frame.
- `internal/server/handlers.go:34-45` routes regular commands through
  `Client.dispatch` and Cancel/Interrupt through `dispatchControl`.
- `internal/server/hub.go:112-114` performs the work semaphore acquire
  synchronously: `c.sem <- struct{}{}`.
- The work semaphore allows 12 handlers. With all 12 occupied, processing a
  13th ordinary frame blocks the sole `readPump` goroutine at the acquire.
- A Cancel/Interrupt frame that arrives after that 13th frame remains unread.
  Its separate `controlSem` is therefore never reached. Pong processing also
  stops, and the connection can expire at the read deadline.

This directly contradicts the guarantee documented at `hub.go:123-140` and
`handlers.go:18-25`. The separate control semaphore only helps if the control
frame has already been parsed; it cannot bypass an earlier blocked dispatch.

The existing regression `internal/server/dispatch_test.go:147-181` calls
`dispatchControl` directly after saturating `sem`. It omits the decisive
sequence: saturated `sem` -> 13th ordinary frame blocks `readPump` -> control
frame arrives. It therefore cannot detect this freeze.

Impact: a multi-session/multi-agent client can become unable to cancel the very
turns that saturated it. This is a release blocker even without a mutex cycle.

Recommended fix:

1. Make the socket read loop permanently non-blocking with respect to work
   admission.
2. Put regular work into a bounded queue served by fixed workers, or reject it
   immediately with an overload response when the bound is reached.
3. Do not replace the blocking acquire with one goroutine per waiting frame;
   that merely moves the unbounded queue into goroutine memory.
4. Keep control traffic on a separate bounded path, but ensure its admission
   cannot block the reader either.
5. Add a regression through `readPump`/`handleIncoming`, not direct dispatcher
   calls: saturate 12 handlers, submit ordinary frame 13, then Cancel, and prove
   Cancel runs before any work slot is released.

### F2 — P0: foreign leased work can still become false `DrainComplete`

This confirms the main blocker in
`docs/reviews/2026-08-20-oxx-seventh-review.md` by inspecting the current code.

Evidence:

- `internal/db/sql/run_queue.sql:20-26` selects only
  `status = 'pending'` for `GetOldestPendingRunQueueEntryForSession`.
- `DrainSessionNow` remembers `lastRowID` only for a locally nacked ordinary
  retry (`internal/session/run_queue_drain_session.go:790-803`).
- When the next lease returns nothing and `lastRowID` is empty, the terminal
  path performs no all-status queue check and returns
  `ledger.verdict(false)` at `run_queue_drain_session.go:962`.
- A ledger containing one prior success and no recorded failure resolves to
  `(DrainComplete, nil)` at `run_queue_drain_session.go:246-253`.

Failure sequence:

1. Row A executes and commits; the ledger records success.
2. Another process leases row B for the same session, or dies while holding
   that lease.
3. This drain asks for the oldest *pending* row and gets none.
4. Because this call did not locally nack B, `lastRowID` is empty.
5. The drain returns Complete although B still exists in durable state
   `leased` and its outcome is unknown.

Recommended fix: before the only terminal `DrainComplete` decision, query for
**any** outstanding queue row for the session, regardless of `pending` versus
`leased`. An outstanding foreign lease must yield Partial/Failed/Unconfirmed,
never Complete. Prefer an explicit service operation such as
`HasOutstandingRunQueueEntriesForSession` rather than inferring global queue
state from a pending lease attempt.

Required regression: after one successful row, install a second unexpired row
leased by another owner and assert that `(DrainComplete, nil)` is impossible.
Repeat with an expired-but-not-yet-cleaned lease.

### F3 — P0: terminal deletion is published as “nothing happened”

`admissionEntry` publishes only one `error`; it does not publish row identity or
an outcome kind (`internal/session/run_queue_admission.go:23-63`). This makes a
destructive bookkeeping outcome indistinguishable from a harmless early return.

The background path demonstrates the defect:

- `internal/session/run_queue_entry_dispatch.go:112-146` leases an
  attempts-exhausted row and calls `TerminalFailRunQueueEntry`, which deletes
  it.
- The deferred release at `run_queue_entry_dispatch.go:63-68` nevertheless
  publishes `errNoExecutionAttempted`.
- A waiting drain maps that sentinel to “nothing ran; loop and inspect pending
  work” without recording a failure (`run_queue_drain_session.go:1042-1055`).
- The row is now gone. If the waiting drain had already observed any earlier
  success, its next empty pending scan reaches line 962 and returns Complete.

The synchronous drain repeats the semantic mismatch: it terminal-deletes an
exhausted row at `run_queue_drain_session.go:665-683`, records the failure only
in its private ledger, then publishes `errNoExecutionAttempted` to any other
waiter. That waiter cannot learn that accepted work was terminally discarded.

There is a second bad branch in the background path: if
`TerminalFailRunQueueEntry` itself fails at `run_queue_entry_dispatch.go:142-144`,
the waiter still receives the same harmless sentinel, losing both row identity
and the DB failure.

Recommended fix: replace the `error`-only handoff with a typed outcome carrying
at least `{rowID, kind, err}`. Distinguish:

- no durable row was touched;
- committed success;
- retryable failure;
- terminal failure/dead-letter;
- outcome unconfirmed/lease lost;
- busy/deferred without execution.

A terminal deletion must be published as a row-scoped failure, even though the
model/tool execution was not attempted. A failed terminal write must be
published as unconfirmed/failure, not as a no-op.

Required regressions: make row A succeed, have another admitted drain/background
worker terminal-fail exhausted row B, and prove a waiting drain cannot return
Complete. Cover both successful and failed terminal DB writes.

### F4 — P1: observed same-row retry success cannot clear its failure

The same untyped admission handoff causes a false failure in the opposite
direction.

- An observed execution outcome is handled at
  `run_queue_drain_session.go:572-596`.
- Because `admissionEntry` carries no row ID, an observed failure is stored as
  a synthetic `__unattributed_N` ledger entry (`recordUnattributed`, lines
  203-211).
- A locally executed outcome does know `leased.ID`; a later success calls
  `recordSuccess(leased.ID)` at lines 769-775.
- That success cannot remove the synthetic key created for the same logical
  row.

Failure sequence:

1. A background holder executes row A and gets an ordinary retryable error;
   A is nacked to pending.
2. The waiting drain records an unattributed failure.
3. The drain leases the same A and successfully commits it.
4. The queue is empty, but the synthetic failure survives and the drain returns
   `DrainFailed`.

This does not silently lose work, but it produces a false non-zero exit and can
encourage an unsafe external retry of already committed work. The typed outcome
recommended for F3 fixes this too: observed outcomes must retain row identity.

Required regressions: observed retryable A -> local successful A, and observed
retryable A -> later observed successful A, both ending in Complete when the
queue is genuinely empty.

### F5 — P1: rerun can delete a new live turn (TOCTOU)

Evidence:

- `internal/server/handlers_agent.go:457-480` cancels, clears the queue and
  polls `IsSessionBusy` until it observes false.
- This is only a snapshot; the handler acquires no coordinator reservation and
  no session OS lock for the maintenance operation.
- It then lists/deletes history at lines 482-514.
- If `Delete` returns `ErrMessageStillStreaming`, lines 492-503 assume the row
  is an orphan and call `ForceDelete` without rechecking or owning the session.
- A new Send/Rerun can start after the idle observation and create an active
  streaming assistant row before or during that list/delete loop.

Impact: rerun can force-delete the message currently being written by a new
turn, then start another run itself. The atomic streaming-delete guard added by
`537d93df` is bypassed exactly where ownership is not actually proven.

Recommended fix: add an exclusive coordinator “maintenance/reservation” API
that atomically transitions from idle/stopped into ownership and holds that
ownership across tail deletion, target deletion and the start/handoff of the
replacement run. A second `IsSessionBusy` check narrows but does not close the
race.

Required regression: pause rerun after its idle observation, start a new turn,
resume rerun, and prove it fails closed without force-deleting the active row.

### F6 — P1: rerun's same-second ordering deletes messages before the target

Evidence:

- Messages are timestamped in whole seconds
  (`internal/db/sql/messages.sql:20-38`).
- The canonical total order is `(created_at ASC, rowid ASC)`; the SQL comments
  explicitly state that UUID `messages.id` is not a valid tiebreaker
  (`messages.sql:15-18`).
- Rerun deletes every message with a later timestamp **or every other message
  with the same timestamp**:
  `handlers_agent.go:488-490` checks
  `m.CreatedAt == targetMsg.CreatedAt && m.ID != targetMsg.ID`.

Therefore, if messages before and after the target were inserted in the same
second, rerun deletes both groups. This is deterministic data loss, not merely
unstable presentation order.

Recommended fix: `Messages.List` already returns the total oldest-first order.
Find the exact target index and delete only the slice after it; fail closed if
the target is absent. A stronger long-term API would expose a DB cursor/rowid
boundary and delete the tail transactionally.

Required regression: insert `before`, `target`, `after` with the same
`created_at`; rerun must retain `before`, replace `target`, and delete only
`after`.

## What the recent commits did fix

The range is not a failed effort; several high-risk changes are materially
better now:

1. **Streaming message deletion guard (`537d93df`)**
   - `DeleteMessageIfTerminal` makes the “still streaming?” predicate and
     delete one DB operation.
   - terminal updates use rows affected and no longer publish a ghost
     `UpdatedEvent` after a concurrent delete.
   - ordinary web delete paths check coordinator busy state before the narrow
     orphan-rescue `ForceDelete` path.
   - Caveat: F5 bypasses the proof by treating a stale idle observation as an
     ownership guarantee.

2. **Child-group victim fencing (`05a1708f`, `6fe0108b`, `984b8cd9`)**
   - sweep targets the immutable victim generation rather than a newly current
     owner;
   - production callers hold the session OS lock across read/verify/kill/rewrite;
   - crashed-holder orphan groups are swept;
   - registry rename durability now includes parent-directory fsync.
   - No circular OS-lock/file-mutex order was found in these paths.

3. **Sticky event coalescing (`b6549640`)**
   - the channel is now only a wake-up token and the newest state is retained in
     `stickyPending`/`sticky`, so a full wake-up channel no longer loses the
     newest sticky value.
   - Delivery to an already slow client remains best-effort when its send queue
     is full; comments should avoid promising stronger delivery than that.

4. **Typed drain result and row ledger (`344dd37c`, `f8ffb68c`)**
   - `DrainNoWork`, `DrainComplete`, `DrainPartial`, `DrainFailed` are a large
     improvement over one overloaded boolean;
   - locally observed row failures are keyed by row ID and same-row success can
     supersede them;
   - context-death exits now feed the ledger before returning.
   - Caveat: F2-F4 show that the state machine still loses global outstanding
     state and row identity at the admission boundary.

5. **Lease watchdog deadline (`b7d50642`)**
   - production code now stores the true pre-rounding renewal deadline at
     `internal/session/run_queue_entry_exec.go:402-422`, avoiding the previous
     whole-second donation.
   - The implementation direction is correct. The committed seventh review
     demonstrates that the current test window still passes when that line is
     reverted; this remains a regression-test gap, not a newly found production
     defect. No test was rerun during this review.

6. **First-session creation race (`f4d19ca8`)**
   - a narrowly recognized `sessions.id` constraint race is converted into the
     intended busy path rather than leaking raw SQLite text.
   - The error classification is still string-based and therefore fragile
     across driver wording changes.

## Lower-priority defects and code smells

### P2 — cleanup around synchronous execution is not panic-safe

`run_queue_drain_session.go:752-756` calls `executeEntrySync` and only afterward
calls `workerWg.Done`, releases `execSem`, and releases session admission. The
background wrapper uses deferred cleanup, but this synchronous branch does not.
An unrecovered panic currently terminates the process; if recovery is added at
any outer boundary, the leaked wait-group count/semaphore/admission will turn
into a permanent in-process wedge. Put all three releases in a scoped defer
around the execution call and define the published panic outcome explicitly.

### P2 — drain API documentation contradicts reachable returns

`DrainNoWork` says its error is always nil at
`run_queue_drain_session.go:57-60`, but current branches return
`DrainNoWork` with non-nil errors at lines 641, 709 and 746. Either change the
contract and consumers or use a result that represents “no row executed, but
drain failed/deferred”. Contract comments are being used as correctness proofs,
so this mismatch is risky.

### P2 — unknown `DrainResult` does not fail closed explicitly

`internal/app/app_run.go:300-328` handles `DrainComplete`, Partial/Failed, then
uses `default` for both `DrainNoWork` and any invalid enum. Add an explicit
`DrainNoWork` case and make the default log a contract violation and return a
non-nil sentinel. This protects future enum additions and memory/caller bugs
from silently inheriting `originalErr` (which may be nil in a future call path).

### P2 — stale generation documentation

`internal/session/childgroup_registry_unix.go:152-156` says a registry entry
must match the **current** on-disk generation at kill time. The fixed API now
correctly matches the immutable `victimGeneration` captured for the dead owner.
The stale comment describes the exact unsafe model that `05a1708f` replaced.

### P2 — test oracles do not isolate several mechanisms

The committed seventh review documents two relevant gaps:

- reverting the true watchdog deadline still leaves the current watchdog test
  green because its elapsed-time window is too broad;
- lease-loss tests can pass because the watchdog cancels first, so they do not
  prove the intended `!ok` renewal branch performed cancellation.

Use branch-specific test seams/observations, not only a final cancellation
effect that several mechanisms can produce.

### P2 — string parsing of SQLite errors

`internal/app/app_run_errors.go:43-51` recognizes a specific constraint race by
English error text. This is portable across the two currently documented
drivers but semantically brittle. Prefer typed extended SQLite codes plus a
query/re-read of the target session ID; keep text matching only as a guarded
fallback. The accepted phrase `constraint violation` is not supported by the
captured driver examples in the comment.

### Maintainability — the drain state machine is too implicit

`run_queue_drain_session.go` has grown into a roughly thousand-line state
machine whose correctness depends on comments, sentinel errors, a private
ledger, multiple early returns and out-of-band DB status. The repeated sequence
of false-success fixes is evidence that the representation is underspecified,
not that one more local condition will be enough.

The durable row should have one typed outcome flowing through local execution,
background execution and admission observation. The terminal verdict should be
computed once from:

- row-scoped outcomes seen by this call;
- whether any durable row for the session remains outstanding in any status;
- why observation stopped (empty, contention, context death, shutdown);
- whether every destructive DB transition was confirmed.

This would eliminate the current semantic overload of `error`,
`errNoExecutionAttempted`, `lastRowID` and `recordUnattributed`.

## Deadlock/lock-order overview

Static inspection found no new circular lock ordering in the recent production
changes:

- `admitSession` holds `inFlightMu` only for map mutation and closes the outcome
  before reacquiring it for deletion; waiters do not hold that mutex while
  waiting on `done`.
- pump shutdown releases `admitMu` before waiting on `workerWg`.
- child-group sweep obtains the OS session lock before entering the registry
  file operation; `RegisterChildGroup` does not acquire the OS lock while
  holding `childGroupFileMu`, so the touched paths do not form a reverse edge.
- sticky state releases `stickyMu` before channel notification/fan-out.
- message deletion uses one SQL predicate rather than a check-then-delete lock
  sequence.

This is a static assessment, not a proof against runtime deadlocks. F1 remains
a concrete scheduler-independent control-plane deadlock despite having no
mutex cycle.

## Recommended release order

1. Fix F1 and add the real read-loop/control regression.
2. Replace the admission `error` handoff with a row-scoped typed outcome; close
   F3 and F4 together.
3. Add the all-status outstanding-row check at the single terminal drain
   boundary; close F2.
4. Give rerun exclusive maintenance ownership, then fix its total-order tail
   selection; close F5 and F6.
5. Tighten the watchdog and lease-loss test oracles.
6. Align the drain/result and generation documentation with actual behavior.
7. Only after those changes, run the existing gate plus focused cross-process,
   WebSocket saturation and rerun-concurrency regressions. This review did not
   run them.

## Final answer to the release question

No, not everything is fixed. The generation sweep, sticky coalescing, streaming
delete guard, context-death verdict and watchdog production deadline are useful
and mostly correct fixes. However, the current HEAD can still:

- make Cancel/Interrupt unreachable behind saturated ordinary work;
- report durable accepted work as fully completed while a row is leased or was
  terminally discarded by another admission holder;
- report a committed same-row retry as failed;
- corrupt history during rerun through both a session-ownership race and an
  invalid same-second ordering rule.

Until F1-F6 are fixed and pinned, this branch should not be labeled a stable
release.
