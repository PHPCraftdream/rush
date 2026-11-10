# Batch commit fault diagnostics

A winning non-success commit claim cancels the hook context **before** the
Runner publishes its fatal signal. The local claim gate still arbitrates
success, error, panic and watchdog callbacks. Cancellation is containment,
not evidence of rollback: a hook may ignore cancellation or finish an
ambiguous write later. The first fault remains immutable; a late result
cannot publish records or dispatch work.

The production default remains 45 seconds. There is no retry, second writer,
reopened faulted Runner, synthetic success or change to the fatal fence,
host retention, containment wait or exit 70.

## Diagnostic contract

`CommitFault` retains the existing concise `jobbatch: commit failed: <kind>`
text and matches `ErrCommitFailed`. An error observed by the winning hook
claim also remains reachable through `errors.Is` and `errors.As`. Panic
values and errors observed only after a timeout are not attached.

The first-claim snapshot contains kind, transition sequence and event,
configured budget, monotonic elapsed time, last phase and phase elapsed
time. Durations measure real elapsed time, independently of an injected
watchdog clock. Phase values are closed: `waiting_hook`, `hook`, `prepare`,
`acquire_transaction`, `statements`, `commit_transaction`. `waiting_hook`
is distinct from an entered hook that has not reached storage. The final
commit phase means the commit boundary was reached, not that commit failed
or that durability is absent.

Reservation and projection mark their existing cache preparation,
transaction acquisition, statements and commit boundaries without changing
SQL or transaction ownership. A warm cache still takes its existing cache
mutex, but does not request a connection. A cold cache may wait for a
connection during preparation. Root-owner lookup before projection remains
in the generic `hook` phase.

The app snapshots the writer's `sql.DB.Stats` before the containment wait.
It issues no SQL and acquires no connection. Pool wait-count and
wait-duration deltas are **since runtime construction**, not per commit.
They establish pool pressure, not the identity of a blocking transaction.
A nil writer omits pool statistics; missing typed fault details report
`unknown` kind and phase.

`commit_error_class` is `sqlite_busy` or `sqlite_locked` only for an observed,
direct `*sqlite.Error` with the corresponding primary code. Other errors,
wrapped errors and watchdog timeouts remain `unknown`. Diagnostic capture
does not walk arbitrary error wrappers or call their `Error`, `As` or
`Unwrap` methods. This deliberately trades classification of custom
wrappers for a bounded fatal path. Callers can still inspect wrapped causes
explicitly outside that path.

Automatic fault logs contain only bounded metadata and safe fault text, not
hook error strings, panic values, payloads, prompts, SQL or credentials.
The established fatal stderr/envelope contract is unchanged; Runner faults
continue to use their concise redacted text. Successful commits stay quiet.

## Regression map and intended revert checks

- `internal/jobbatch/commit_fault_test.go`: manual-clock publication barrier
  proves cancellation does not depend on committer rescheduling; late nil
  and error results cannot replace the first metadata or cause. Removing
  claim-time cancellation must fail the publication-barrier assertion;
  adopting late progress or causes must fail the immutability assertions.
  Returned cause identity and panic redaction are
  checked separately.
- `internal/jobbatch/runner_commit_faultgate_test.go`: an immediate watchdog
  never enters the hook and records `waiting_hook`. Existing arbitration
  tests retain the success-before-timeout and stale-callback oracles.
- `internal/session/batch_commit_timeout_diagnostics_test.go`: a real held
  writer transaction exhausts the only connection. Warm reservation and
  rootless projection identify acquisition; cold reservation identifies
  preparation. Cancellation releases the waiter without rows or dispatch.
  Removing either the corresponding phase marker or claim cancellation must
  fail the phase/canceled-waiter assertions.
- `internal/session/batch_commit_contention_test.go`: a separate fixture-only
  SQLite connection holds an actual writer lock. A test-only zero busy
  budget yields a typed busy error at `BEGIN IMMEDIATE`, not a watchdog
  timeout. Another case observes an actual pool waiter, releases it before
  the budget, and holds a post-commit acknowledgement barrier to prove
  once-only durability precedes dispatch. Trigger-induced SQL failures
  cover statement phases and rollback. Post-commit error injection covers
  final commit phases for reservation and rootless projection; this last
  case is an owning-layer seam, not a real blocked COMMIT reproduction.
- `internal/app/batch_install_factory_test.go`: the dormant production
  factory wires the same writer and its stats baseline; the default remains
  exactly 45 seconds. Removing the wiring or changing the default must fail.
- `internal/app/batch_commit_diagnostics_test.go`: an exhausted pool cannot
  block diagnostic capture or increment its wait count; poison error
  callbacks cannot run; only an observed SQLite busy error is classified.
- `internal/app/app_batch_subprocess_test.go`: real exhausted-pool snapshots
  accompany timeout, returned error and panic faults. Production exit 70,
  one fatal stderr line, one structured diagnostic, fence-before-exit,
  retained host identity and unchanged containment are asserted. Secret
  hook/payload text must be absent. The timeout helper never physically
  returns even after database cancellation. Phase assertions admit a
  watchdog winning before hook scheduling, rather than assuming entry.

Executable checks and revert checks must run in GitHub CI. No blocked
SQLite COMMIT or SQLITE_LOCKED reproduction is claimed. The real-store
watchdog tests still use real deadlines; manual-clock ordering proofs live
in the owning Runner package. The historical incident's underlying stall
remains unidentified, and these changes do not prevent every future timeout.
