# Lesson: chronic ubuntu-latest CI kill was a real goroutine leak, not runner flakiness

## Symptom

`.github/workflows/build.yml`'s `go test -short -failfast ./...` step
died on `ubuntu-latest` with `exit code 143` (SIGTERM) and the log line
`The runner has received a shutdown signal. This can happen when the
runner service is stopped, or a manually started runner is canceled.`
Chronic: failed on 14 of the last 18 `build` runs on `main` since
2026-08-25, across many unrelated commits — not a new regression from
any single change.

## What did NOT work (and why it looked plausible at the time)

1. **`-race` as the cause.** Removing it entirely reproduced the
   identical exit 143 on the plain command. Not it.
2. **Cold build cache.** `setup-go`'s own log showed a cache hit
   (~1.1GB restored) immediately before every kill. Not it.
3. **Link-workload size (`./...` linking ~50 test binaries at once).**
   Splitting into per-package batches (isolating `internal/agent`,
   `internal/app`, `internal/server`, `internal/session` into solo
   `go test` invocations) did **not** help: the kill reproduced on a
   **single, solo package** (`internal/agent` alone), at wildly
   different times across identical commits (55s, 2m22s, then 46
   minutes once debug logging was enabled) — inconsistent with a
   workload-size-driven cause.
4. **Disk/memory exhaustion.** `df -h`/`free -h`/`ulimit -a` printed
   immediately before a kill showed ample headroom every time (81GB
   free disk, 13GB available RAM). Not it.
5. **The test code itself killing the runner process** (by analogy to
   a past incident in this repo where `taskkill //IM`/`pkill` by image
   name hit unrelated processes — see this repo's own CLAUDE.md). The
   package that died first has zero `exec.Command`/`syscall.Kill` in
   its own files. Not it.
6. **CPU starvation of the runner's own heartbeat.** A real GitHub
   annotation — *"lost communication with the server... starves it for
   CPU/Memory, or blocks its network access"* — surfaced once
   `ACTIONS_STEP_DEBUG`/`ACTIONS_RUNNER_DEBUG` were enabled, which
   pointed here. Plausible-sounding (real-timer watchdog tests with
   5-10ms tickers exist in this package), but two rounds of deep
   contemplation (`/oxx`) and further diagnostics never found an actual
   busy-loop, and the retry-wrapper built around this theory turned out
   to be structurally unable to help anyway (see below).
7. **Retrying the batch on external kill.** Implemented, then proven
   useless: when the *runner itself* is killed, the whole step's shell
   process — including any retry loop inside it — dies with it. The
   loop never got a second iteration; its own `::warning::` line for a
   retry attempt never printed even once across a real run.

Every one of the above was individually falsified with direct
evidence (log excerpts, `gh api` calls, local reproductions) before
being abandoned — none were guessed away.

## What actually worked: binary search + direct instrumentation

1. **Bisected by test file/name** via a disposable
   `workflow_dispatch`-triggered workflow (`bisect-agent.yml`) using
   `go test -run`/`-skip` regexes, so each round needed no commit —
   just a `gh workflow run` dispatch. One round split
   `internal/agent`'s ~23 `TestStreamWatchdog_*` tests (leading
   hypothesis: real timers) from the other ~373 tests. The watchdog
   group was clean; the other 373 reproduced the kill in 1m18s.
2. **Read the actual `-v` log**, not just the exit code. The last
   `=== RUN` line before the kill named a specific test — but running
   that ONE test alone passed cleanly, ruling out a single-test cause
   and pointing at **cumulative pressure across many tests**.
3. **Logged `runtime.NumGoroutine()` every 2s** from `TestMain`
   (temporary, since reverted). On the killed run, goroutine count
   climbed from 21 to 141+ in under 30 seconds — genuine unbounded
   growth, not healthy fluctuation.
4. **Dumped full goroutine stacks** (`runtime.Stack(buf, true)`) once
   the count passed a threshold, in the *same* temporary `TestMain`
   hook. This was the actual answer: dozens of live
   `database/sql.(*DB).connectionOpener` goroutines, "created by
   `database/sql.OpenDB`" — Go's standard-library background goroutine
   that every `*sql.DB` pool spawns, and which only exits when that
   `*sql.DB` is closed via the *correct* API.

## Root cause

`internal/db/connect.go`'s `Connect(ctx, dataDir)` is a **reference-
counted connection pool** keyed by absolute database path (`pool
map[string]*connEntry`, package-level, process-wide). Its own doc
comment says plainly: *"Callers must pair each Connect with a
[Release] when they no longer need the connection."* `Release`
decrements the refcount and only actually closes the underlying
`*sql.DB` (and removes the pool entry) once it reaches zero.

Several test helpers — most importantly `internal/agent/common_test.go`'s
`testEnv`, the shared setup used by the large majority of that
package's ~396 tests — called `conn.Close()` directly on the raw
`*sql.DB` instead of `db.Release(dbDir)`. Each test's `t.TempDir()` is
unique, so every one of those ~396 tests created a *new* pool entry
that was **never removed and never released**, each holding its own
live `connectionOpener` goroutine. Bypassing `Release` doesn't just
leak an entry in a map — it leaves that connection's background
goroutine unreachable from anything that could ever clean it up, for
the remaining lifetime of the whole test binary.

The same exact anti-pattern (`db.Connect` + `conn.Close()` instead of
`db.Release`) was found and fixed in five more spots across the repo:
`internal/agent/agent_checkpoint_test.go`, `internal/app/recovery_test.go`,
`internal/app/recovery_live_session_test.go`,
`internal/app/run_allowlist_test.go`,
`internal/server/p569_message_edit_verify_test.go`,
`internal/filetracker/service_test.go`, and
`internal/permission/permission_test.go` (two spots). Several *other*
files in `internal/app` and `internal/session` already used
`db.Release` correctly — this was an inconsistently-applied pattern,
not a universal one, which is exactly why it went unnoticed: most
call sites were fine.

## Why this manifested as a runner-level kill, specifically on ubuntu-latest

Not fully pinned down, and not necessary to pin down further — the
leak itself is the confirmed, fixed cause, and eliminating it also
eliminated the symptom (see Verification). The working explanation:
enough concurrently-alive `connectionOpener` goroutines (each backed
by a real OS thread parked in a blocking `select` awaiting DB pool
events) eventually pushed some resource axis this repo's diagnostics
didn't directly measure (likely OS thread count / scheduler pressure)
past a point where GitHub's hosted-runner agent process itself lost
its ability to service its own heartbeat to the Actions control
plane — hence "lost communication with the server" rather than a
`go test` timeout or panic. `internal/app` measured at ~117s and
`internal/agent` at ~193s in earlier profiling this session; after the
fix, full local runs of both packages dropped to roughly a third of
that (~30s and ~240s respectively, noting `internal/agent` also runs
far more tests overall) — consistent with real, measurable scheduler
overhead from the leak, not just a coincidence.

## Fix

Changed the affected test cleanups from:
```go
conn, err := db.Connect(ctx, dbDir)
...
t.Cleanup(func() { conn.Close() })
```
to:
```go
conn, err := db.Connect(ctx, dbDir)
...
t.Cleanup(func() { _ = db.Release(dbDir) })
```

No production code changed. No CI workaround (batching, retries,
resource throttling, debug logging) was kept — the root cause is
fixed at the source, so `build.yml`'s test step is back to a single
plain `go test -short -failfast ./...`, exactly as it was before any
of this session's CI investigation began.

## Verification

- `internal/agent`, `internal/app`, `internal/server`,
  `internal/filetracker`, `internal/permission` all pass locally after
  the fix.
- A goroutine-count diagnostic re-run of the previously-failing test
  set showed the count returning to baseline (down to 9, from a peak
  of 141+) by the end of the run, instead of climbing unboundedly.
- `go build ./...` and `go vet ./...` clean across the whole module.

## The actual lesson

1. **A pooled/ref-counted resource needs its own release API enforced
   everywhere it's used — a plain `.Close()` on the underlying handle
   compiles fine, looks correct, and is silently wrong.** `go vet`,
   `staticcheck`, and the compiler all have no way to catch "this
   `*sql.DB` came from a ref-counted pool and needs `Release`, not
   `Close`" — that invariant only lives in a doc comment. Consider a
   lint rule (or at minimum a repo-grep check in CI) for `db.Connect(`
   call sites that aren't paired with `db.Release(` in the same file,
   the way this repo already has bespoke greps for a few other
   fork-specific invariants (ASCII-only SQL, stale model-slot names).
2. **When a CI failure resists every plausible external-config
   hypothesis (resource limits, flags, cache, matrix behavior), and the
   evidence is *inconsistent* across identical runs, stop tuning the
   workflow and start bisecting the code under test.** Every fix
   attempted at the `.github/workflows/build.yml` level (`-p N`,
   dropping `-race`, batching, retries, `fail-fast: false`, debug
   logging) was reasonable given the evidence available at the time,
   and none of them addressed the actual cause — because the cause was
   never in the workflow.
3. **A `workflow_dispatch`-triggered, disposable bisection harness is
   cheap and fast to iterate on** (no commit needed per round, two
   independent VMs can run two candidate splits in parallel) and found
   the specific failing test group in two rounds. Worth keeping the
   *pattern* in mind for the next hard-to-reproduce CI failure, even
   though this specific throwaway workflow file was deleted once its
   job was done.
4. **`runtime.NumGoroutine()` + `runtime.Stack(buf, true)` from a
   temporary `TestMain` hook is a fast, precise way to answer "is
   something leaking, and what exactly" — far faster than auditing
   code by hand.** The stack dump named the exact stdlib function
   (`database/sql.(*DB).connectionOpener`) and its caller
   (`database/sql.OpenDB`) directly; no further guessing was needed
   once that was visible.
5. **Inconsistent adoption of an internal API convention is worse than
   consistently wrong code** — the correct `db.Release` pattern already
   existed and was used correctly in most of `internal/app` and
   `internal/session`, which is likely exactly why this went unnoticed
   for so long: most usages were fine, so nothing about the codebase's
   overall shape looked suspicious.
