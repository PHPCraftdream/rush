# Dynamic release gate — 2026-08-20

Five prior reviews (2026-07-30 through 2026-08-19) recommended a dynamic gate
and none of them ran one: all checks were static reads of the source, and
the Unix-only concurrency code was only cross-compiled, never executed. This
report is that gate: everything below was actually run, on real Linux, with
real separate OS processes for the parts that need them. No conclusion here
is inferred from reading code alone — every claim has an attached command
and output excerpt, and every "no defect found" claim states how many
attempts were made.

## Environment

- Real Linux: WSL2 Ubuntu 24.04.4 LTS, `go1.26.3 linux/amd64` at
  `$HOME/sdk/go/bin/go`.
- Worktree: `D:\dev\go\crush\.claude\worktrees\agent-gate`, branch
  `worktree-agent-gate`, HEAD `95d7e0c12d3a11a9de54c84aef13abc492359fe2`
  (same as `main` at task start).
- The worktree lives on `/mnt/d` (9p), which is slow and — per the task
  brief — a potential source of misleading timing. To rule that out, the
  whole tree was `rsync`'d to native WSL2 ext4 at `~/gate` (source) and
  `~/gate-run` (build output + all test artifacts). **All dynamic runs in
  this report used the native-FS copy, not `/mnt/d`.** `web/dist/.gitkeep`
  was manually restored in the native copy after an `rsync --exclude` typo
  excluded it — the `//go:embed all:dist` build tag needs *some* file there
  and CLAUDE.md forbids deleting the real one.
- **Environment hazard discovered and worked around:** this WSL2 instance's
  `/tmp` is swept by something (systemd-tmpfiles or similar) on a timescale
  of single-digit minutes — a built binary and a completed test log both
  vanished mid-session despite a "task completed" notification having
  already fired. Everything durable was moved to `~/gate-run` (native FS,
  not `/tmp`) after that was discovered; no result in this report depends on
  anything that was ever only in `/tmp`.
- **Tooling hazard discovered and worked around:** the bash tool's relay to
  `wsl.exe -d Ubuntu-24.04 -- bash -c '<multi-statement script>'` reliably
  corrupts `$?` and `for`-loop variable expansion when several statements
  are chained with `;`/newlines inside one `-c` string (confirmed with a
  minimal repro: `bash -c 'false; echo $?'` prints `0`, and standalone
  `bash -c 'exit 1'` correctly returns 1 to the outer caller). This
  produced a false "crush run exits 0 on error" reading early in this
  session — traced down through `runFailed`/`fang.Execute`/`os.Exit(1)`,
  all of which turned out to be correct, before the actual cause (a shell
  transport bug, not a crush bug) was found and confirmed with
  `bash -c 'false'` alone reaching the outer shell as exit 1. After that,
  every multi-statement script in this report was written to a `.sh` file
  first (via the Write tool, which doesn't go through the same relay) and
  invoked as a single command. This is called out explicitly because it is
  exactly the kind of near-miss the task warned about: a defect that looked
  real, backed by an exit code, that dissolved under a second, more careful
  measurement. It is NOT reported as a crush defect below.

## Summary verdict: **NO-GO**

Four real test failures survive on Linux without `-short`, two of them
newly-discovered by this gate (not on the known-noise list) rather than
already filed. One is a legitimate concurrency/robustness defect
(degraded error message under a session-creation race — see Part 2); the
rest are either the pre-declared known noise, a pre-declared known flake now
measured at its stated rate, or an out-of-scope credentials-gated test. No
data corruption, no silent-success, and no non-zero-exit-code violation was
observed anywhere in ~200 real concurrent/adversarial process invocations
across Parts 2–4. The reason for NO-GO is exclusively Part 1: **the fence
suite this whole effort was about is not executed by CI or pre-push at all**
(confirmed, not assumed — see Part 1), which was already the core finding
of all five prior reviews and remains unfixed.

## Part 1 — full `go test ./... -count=1` (no `-short`) on Linux

Command (run once, ~26 minutes wall clock in this WSL2/9p-adjacent
environment even against the native-FS copy):

```
cd ~/gate && $HOME/sdk/go/bin/go test ./... -count=1 -v > ~/gate-run/gate_full.log 2>&1
```

Result: 39 packages exercised, 3622 sub-tests run (`grep -c '^=== RUN'`), 4
`--- FAIL` lines, 3 packages reported `FAIL` at the package level
(`internal/agent`, `internal/cmd`, `internal/session`); the other 36
packages (including `internal/app`, `internal/db`, `internal/server`,
`internal/config`, `internal/message`, `internal/permission`) all passed
clean. No `panic:`, no `DATA RACE`, no `fatal error` anywhere in the log
(`grep` came back empty).

**Confirmed independently: none of these tests run under CI/pre-push.** All
four failing tests are gated `if testing.Short() { t.Skip(...) }` and CI
(`.githooks/pre-push`, `.github/workflows/build.yml:79`) runs
`go test -short -failfast ./...`. This means the fence suite this task
exists to validate has *never once executed* in this project's CI, on any
platform, on any commit — a fact this report can now assert from direct
observation of the skip guards and the CI invocation, not by inference.

| Test | Package | Result | Classification |
|---|---|---|---|
| `TestCoderAgent/glm-5.1/simple_test` | `internal/agent` | FAIL (149.68s) | Environment/out-of-scope — VCR cassette test against `hyper.charm.land`, requires a live Hyper API key to re-record; source comment at `internal/agent/agent_run_test.go:49-53` explicitly documents "skip in -short (CI)... runs in full go test locally for anyone who can re-record." No credentials were available or should be used per task instructions. Not a concurrency defect. |
| `TestProbeThenKillHolder_LiveHolderStillKilled` | `internal/cmd` | FAIL (6.71s) | **Pre-declared known noise, confirmed.** Failure text: `"a genuinely live holder that was killed must be confirmed dead"` / `"a genuinely live holder must still be killed"` — matches the task's own description exactly (holder helper not reaped, forceKillHolder's wait window doesn't observe the zombie's death). The adjacent, same-pattern `TestForceKillHolder_LiveProcess` in the same run PASSED, supporting "flaky reaping timing" over "systemic kill-path defect." |
| `TestSessionsReset_ForceStillKillsLiveHolder` | `internal/cmd` | FAIL (6.87s) | **Pre-declared known noise, confirmed.** Same root cause as above (shared helper). |
| `TestP1_1_WatchdogCancelsAtTTLMinusMargin` | `internal/session` | FAIL (18.99s) | **Pre-declared known flake (#604), frequency measured directly — see below.** |

Everything else in `internal/cmd` (the package this whole effort is about)
**passed**, including the tests that matter most for this gate:

```
--- PASS: TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive (3.17s)
--- PASS: TestProbeThenKillHolder_OrphanedGenerationCapturedBeforeProbeAcquire (3.50s)
--- PASS: TestSessionsKillCmdRun_SweepsOrphanedGroupAfterCrash (1.86s)   # the #602/6fe0108b sweep fix
--- PASS: TestProbeThenKillHolder_StalePIDNotKilled (0.04s)
--- PASS: TestProbeThenKillHolder_UnknownProbeError_FailsClosed (0.00s)
--- PASS: TestSessionsReset_ForceLeavesLockFileInPlace (0.20s)
--- PASS: TestSessionsReset_ForceHoldsLockDuringDBReset (0.03s)
--- PASS: TestSessionsReset_ForceDoesNotKillStalePID (0.20s)
--- PASS: TestSessionsReset_ForceClearsUnfinishedAssistantMessages (0.22s)
```

### #604 frequency, measured directly (not assumed from the task brief)

```
cd ~/gate && for i in $(seq 1 10); do
  $HOME/sdk/go/bin/go test ./internal/session/... \
    -run TestP1_1_WatchdogCancelsAtTTLMinusMargin -count=1 -v \
    2>&1 | grep -E '^--- (PASS|FAIL)'
done
```

Result, 10/10 rounds:

```
--- PASS (11.25s)
--- PASS (12.02s)
--- PASS (11.45s)
--- FAIL (12.94s)
--- PASS (11.32s)
--- FAIL (13.21s)
--- PASS (11.48s)
--- FAIL (13.26s)
--- PASS (11.32s)
--- FAIL (13.00s)
```

**4 of 10 (40%)** — matches the task's stated ~40% baseline for #604
essentially exactly. Confirmed, not fixed, per instructions (#604 is
already tracked separately).

## Part 2 — multi-process inject/drain stress (real separate OS processes)

Isolation: `CRUSH_GLOBAL_DATA` and `CRUSH_GLOBAL_CONFIG` both pointed at
throwaway dirs (`~/gate-run/p2-global-data`, `~/gate-run/p2-global-config`)
for every invocation; `crush.json` in the work dir points `smart`/`fast` at
a fake `openai`-shaped provider with `base_url: http://127.0.0.1:1/v1` (a
port nothing listens on — instant connection-refused, no network, no real
model, `stream_stall_retries: 0` to keep failures fast and deterministic).
No `crush models`/`crush providers` command was ever run.

Driver (`gate-part2-stress.sh`): for each of 15 rounds, launch **6
independent `crush run` processes** (real `fork`+`exec`, not goroutines)
against the *same* `--session stress-session-<round>` id simultaneously,
plus one `crush sessions inject` fired ~0.3s in, then wait for all 7 to
exit and record each one's exit code and stdout/stderr.

```
~/gate-run/gate-part2-multiround.sh 15 6
```

### Result table (15 rounds × 6 `crush run` processes = 90 total invocations)

| Round | Winners (did real work) | Busy-rejected (clean) | Session-creation race (see below) |
|---|---|---|---|
| 1, 2, 4, 5, 10–13, 15 (9 rounds) | 1 | 5 | 0 |
| 3, 6, 9 (3 rounds) | 1 | 4 | 1 |
| 7, 8, 14 (3 rounds) | 1 | 2–3 | 2–3 |

**Exactly one winner in every one of the 15 rounds, zero exceptions.** The
mutual-exclusion property the task calls out as having broken four times
before ("ровно один процесс владеет сессией") held in 15/15 trials here.
Every loser either got the friendly busy rejection:

```
session "stress-session-1" is already running in crush process PID 5565. ...
Agent processing failed: ... session is already locked by crush process PID 5565 ...
```

(exit code confirmed 1 in every case, checked via single-statement
`wsl.exe` invocations to avoid the `$?`-relay bug described above), or hit
the finding below. **No run ever silently succeeded while another process
held the session, and no run with unperformed work ever exited 0.**

### Finding: session-creation race produces an unfriendly error instead of a busy rejection

When N processes race `crush run --session <new-id>` for an id that does
**not yet exist**, the loser of the `CreateWithID` race gets a raw SQLite
error instead of the "session busy, use `sessions inject`" message the
already-exists case gets:

```
Failed to create session for non-interactive mode: session "stress-session-7"
not found and could not be created: constraint failed: UNIQUE constraint
failed: sessions.id (1555).
```

- **Frequency:** 10 of 90 individual `crush run` invocations (11%), in 6 of
  15 rounds (40%) — reproducible, not a rare flake.
- **Source:** `internal/app/app_run.go` — the get-or-create path does
  `app.Sessions.Get(ctx, id)` (miss) then `app.Sessions.CreateWithID(ctx,
  id, id)`; when several processes race the first-ever creation of an id,
  all but the DB-transaction winner get the raw constraint violation
  wrapped into `"session %q not found and could not be created: %w"`.
- **What is NOT broken:** the exit code is still reliably non-zero (checked
  directly, not inferred) — no silent success, no corrupted session, no
  duplicate row survives (confirmed: `sessions list` afterward shows
  exactly one row per id, never two, never a half-written one). This is a
  message-quality/robustness gap, not a correctness or safety defect: an
  orchestrator parsing this message would misclassify a transient race as
  a permanent failure instead of retrying or falling back to
  `sessions inject`.
- **Disposition: NOT fixed.** This is not a one-line change — a proper fix
  means either retrying `CreateWithID` on a unique-constraint violation (by
  re-`Get`ting and falling into the existing continue-path) or reclassifying
  the wrapped error so `sessionBusyGuidance` recognizes it, and either one
  touches session-store semantics shared by other callers. Per the task's
  instruction to fix only small, single-place defects, this is reported for
  separate follow-up, not patched here.

Lock-file hygiene after 15 rounds of unmanaged concurrent access: `sessions
locks` showed exactly one leftover entry, for the *last* round's session
only, correctly marked `offline` (stale PID, not alive) — the 14 sessions
from earlier rounds all cleaned their own lock files on exit. No lock leak.

## Part 3 — repeated SIGKILL mid-run / mid-outbox-write

Two variants were run, both against a fresh isolated `CRUSH_GLOBAL_DATA` /
`CRUSH_GLOBAL_CONFIG` / work dir, same fake-provider config as Part 2.

### 3a — SIGKILL the `crush run` holder process itself, 20 rounds

Driver (`gate-part3-sigkill.sh`): launch `crush run --session sigkill-session
--timeout 30s` in the background, sleep a random 0.1–0.9s, `kill -9` it,
`wait` for it, record the reaped exit status. Repeated 20 times against the
same session id.

```
round 1..20: delay=0.1-0.9s killstatus=137   (all 20: confirmed SIGKILL landed)
```

After 20 kills: `sessions list` shows **zero sessions ever persisted** —
every single kill landed before the session-creation transaction committed
(all 20 attempts used delays under 0.9s; a later timing probe found normal
process startup + config discovery alone routinely takes 1.6–3.2s in this
environment, so this variant mostly exercised "kill during startup," not
"kill mid-write" — noted honestly rather than overclaimed). What it does
confirm: `crush.db` (184KB, real data from other test runs sharing the
directory structure) was left in a queryable, non-corrupted state — a
follow-up clean `crush run` against a new session id afterward worked
normally (exited 1 for the expected fake-provider reason, not a DB-open or
lock error), and `.crush/locks` contained no stale entries from any of the
20 kills.

### 3b — SIGKILL `crush sessions inject` specifically, targeting its two sequential writes

This is the more literal reading of "kill mid outbox-transaction":
`doInject` (`internal/cmd/sessions_inject.go:155`) does two sequential DB
writes — `messages.Create` then `sessions.CreatePendingInject` — an ideal
target for a split-write race.

Calibration first (measuring where the time actually goes, since the first
attempt at this test used a 5–45ms kill window and landed 0/3 kills anywhere
near a write — all killed during pure process startup):

```
5 uninterrupted `sessions inject` runs: 3175ms, 3141ms, 1608ms, 3197ms, 3185ms
```

— almost entirely fixed per-process overhead (config discovery walk,
logged as `"No git repository detected... will limit file walk operations"`),
not the writes themselves. Kill window was retargeted to the 2.7–3.4s tail.

25 rounds, all 25 kills confirmed landed (`killstatus=137`):

| Outcome | Count |
|---|---|
| Inject completed (both writes + success message observed) before the kill landed | 3 of 25 (rounds 5, 7, 18) |
| Killed before any write (no message, no success text) | 22 of 25 |
| **Split-write (message created without success confirmation, or vice versa)** | **0 of 25** |

Cross-checked against the session's actual message list afterward
(`sessions show --with-messages`): exactly 3 injected user messages exist
("inject round 5", "inject round 7", "inject round 18"), and they match
1:1 with the 3 runs whose stdout showed either success-message variant
(`"injected into session..."` for the live-holder path or `"message
persisted... picked up when the session next runs"` for the no-holder
path — the test's first success-detection pass only grepped for the first
variant and wrongly read 2 real successes as failures until this was
caught and corrected against the DB state directly). **No orphaned
message, no message referenced by a pending-inject row that doesn't exist,
no duplicate row, in 25/25 kills.**

**Honest limitation:** even at the tuned tail window, only 3 of 25 attempts
landed anywhere near the actual commit sequence (the other 22 died in
process-startup overhead that dominates this environment's wall-clock
budget). 3 successful landings is a small sample for a race window that is
likely microseconds wide inside a ~3-second process lifetime; this result
should be read as "no defect surfaced in the attempts that got close," not
as exhaustive coverage of the write boundary. A tighter reproduction would
need either a slower/instrumented DB layer to widen the window
artificially, or running on a faster filesystem where fixed overhead
doesn't dominate — both out of scope for this pass.

## Part 4 — real process-tree kill on Unix

Two independent lines of evidence, both actually executed (not read):

### 4a — the existing Go test suite (Part 1's run), specifically the tests never run by CI

All of the following ran as real `os/exec` subprocesses with `Setpgid:
true` (own process group, per the file's own pattern for simulating a
CLI-provider child), executed and PASSED in this session's Part 1 run,
none of them ever having executed under `-short` CI before:

- `TestSessionsKillCmdRun_SweepsOrphanedGroupAfterCrash` — PASS (1.86s).
  This is the exact "the orphaned-generation sweep commit (6fe0108b)"
  scenario: a crashed holder (SIGKILL, no `Release`) with a live orphaned
  child process group, verifying the sweep still reaches it.
- `TestProbeThenKillHolder_CapturesVictimGenerationWhileHolderAlive` — PASS
  (3.17s). Live holder with a registered child group, killed cleanly.
- `TestProbeThenKillHolder_OrphanedGenerationCapturedBeforeProbeAcquire` —
  PASS (3.50s).
- `TestProbeThenKillHolder_StalePIDNotKilled` — PASS. Negative case: a
  recycled/stale PID must NOT be killed.
- `TestProbeThenKillHolder_UnknownProbeError_FailsClosed` — PASS.
- `TestSessionsReset_ForceLeavesLockFileInPlace`,
  `TestSessionsReset_ForceHoldsLockDuringDBReset`,
  `TestSessionsReset_ForceDoesNotKillStalePID`,
  `TestSessionsReset_ForceClearsUnfinishedAssistantMessages` — all PASS,
  covering `sessions reset --force` against both a stale and a genuinely
  dead-but-registered holder.

The only two live-holder-kill tests that failed
(`TestProbeThenKillHolder_LiveHolderStillKilled`,
`TestSessionsReset_ForceStillKillsLiveHolder`) are the pre-declared known
noise (Part 1), not a new-owner-safety or sweep-reach defect.

### 4b — black-box CLI-level confirmation of the P0 #594 scenario

To verify this end-to-end through the actual CLI binary (not just the Go
test harness), `gate-part4-kill-cli.sh` did the following against a fresh
isolated data dir:

1. Started a real `crush run --session p4-kill-session --timeout 60s` in
   the background (holder acquires the lock, fails against the fake
   provider, then would sit in its own retry/backoff — long enough to
   kill).
2. Waited 2s, confirmed via `sessions locks` the lock was live:
   `p4-kill-session  754  alive  1s ago  1s  ∞  -`
3. Ran `crush sessions kill p4-kill-session --wait 10s` from a **separate**
   process. Output: `killed PID 754` / `PID 754 exited` / `removed lock
   .../session-p4-kill-session.lock`. Exit code 0.
4. `wait`ed on the original holder's PID from the driver script (not the
   kill command) — reaped exit status 137 (SIGKILL), confirming the kill
   command, not something else, actually terminated it.
5. `sessions locks` afterward: the `p4-kill-session` row is gone entirely
   (only an unrelated leftover session remained).
6. Immediately started a **new** `crush run --session p4-kill-session
   --timeout 10s` (the P0 #594 scenario: new owner right after old
   holder's death). Result: exit 1, but for the expected fake-provider
   connection-refused reason — **not** a "session busy" rejection and
   **not** a crash — i.e. the new owner was not harmed by the kill/sweep
   that just ran, matching #594's fix intent exactly.

This is a full, real, dynamic reproduction of the four bullet points Part 4
of the task asked for, via the actual `crush` binary and actual separate OS
processes, not simulated.

## What could NOT be run, and why

- **`TestCoderAgent`** (Part 1) needs a live Hyper API key to re-record its
  VCR cassette; per task instructions, no real provider/credentials were
  used. Left as pre-existing, documented, out-of-scope.
- **Part 3's split-write race** was only exercised at 3 near-the-boundary
  samples out of 25 attempts (see Part 3b's "Honest limitation" above) — the
  fixed per-process startup cost in this WSL2 environment (1.6–3.2s) makes
  the actual DB-write window a small fraction of total process lifetime,
  and no mechanism was available in the time budget to narrow the kill
  window further (e.g. an instrumented build with a deliberate sleep
  between the two writes, which would have been a more invasive code
  change than this pass's scope allows).
- **Windows-side dynamic testing was not attempted.** The task specifically
  asked for the Unix fence/process-tree-kill code, which is Linux-only
  (`!windows` build tag) and was the whole point of using real WSL2. No
  claim is made here about Windows behavior.
- A wider stress sweep (more than 15/20/25/30 rounds per scenario, higher
  concurrency than 6 processes) was not run due to time budget; the sample
  sizes above (90 total `crush run` invocations in Part 2, 20+25 kills in
  Part 3, 10 repeats for the #604 frequency count) are what this report's
  numbers are based on — stated explicitly so they are not mistaken for
  exhaustive.

## Files touched

No code changes were made. All defects found were either pre-declared known
noise/flakes (confirmed, not touched) or a real-but-non-trivial defect
(session-creation race message quality, Part 2) that the task's own
instructions direct to report rather than patch. `git diff` against
`worktree-agent-gate`'s starting point (`95d7e0c1`) is empty except for this
report and the scratch driver scripts used to produce it
(`gate-*.sh`, `gate-crush.json` at the worktree root) — the driver scripts
are kept alongside the report for reproducibility of every command quoted
above; the debug print briefly added to `internal/app/app_run.go` while
chasing the exit-code transport-bug false alarm was reverted before this
report was written and is not part of the final diff.
