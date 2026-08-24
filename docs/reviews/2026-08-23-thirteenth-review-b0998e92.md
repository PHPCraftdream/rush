# Thirteenth review (read-only): five commits closing the twelfth review's five P3s and three Minors, plus a new startup guard

- Reviewed HEAD: `b0998e92b7b63d1cbebaade4589f6d6d48da8d47` (branch `main`)
- **Code range actually reviewed: `7ba4431a..b0998e92`** — 5 commits, 16 files,
  +892 / −139.
- Predecessor: `docs/reviews/2026-08-23-twelfth-review-7ba4431a.md` (read in
  full; it is the baseline for every "closed / not closed" judgement here).
- Working tree: **untouched**. No mutating git command was run. Every
  revert-check and every probe below ran on an **out-of-tree copy of HEAD**
  (`git archive HEAD | tar -x` into `D:\system_artefact\Temp\crush-r13`),
  never on the repo. `git status --porcelain` before and after this review
  shows the same two pre-existing entries (` D web/dist/.gitkeep`, the
  untracked twelfth-review report).

## Verdict

**NO-GO**, on exactly one finding. Everything else is P3/Minor and would not
have blocked.

Four of the five commits do what they claim. `b1c1f349` closes N-1 at the
level it was filed, `a8b35e96`'s outcome-(a) reasoning is correct where it
matters (`restartOrphaned` really is a durable `session_run_queue` write and
not a provider turn — traced independently, not taken from the commit
message), `65c09958`'s three comment fixes are complete and accurate, and the
source-tree guard `b0998e92` correctly refuses `<crushrepo>/dev/**` and
correctly does **not** refuse a real npm/`go install` layout, including the
adversarial shapes I threw at it.

The blocker is `6c695dcb`. Moving the prompt-recreate into an unconditional
defer registered after step 3's commit point made it fire on the **success**
path as well — a path it never ran on before. The positional check it reuses
(`len(allMsgs) <= targetIdx`) assumes the session's row count can only grow
once the replacement turn starts, and that assumption is false: silent/auto
compaction runs **synchronously inside the same turn**
(`agent_turn.go:1838` → `runSummarizeSilent` → `agent_compaction.go:690-694`)
and deletes rows. The arithmetic is unforgiving — after steps 2/3 the session
holds exactly `targetIdx` rows, the turn adds user + assistant + summary
(+3), so the guard misfires the moment compaction deletes **three or more**
messages, which is essentially any compaction at all. I reproduced it end to
end against the real `handleRerunMessage`: a **successful** rerun leaves the
operator's prompt duplicated after the assistant's reply.

This is not data loss and the fix is small (about five lines: only run the
recreate when the run did not reach a successful completion, or capture the
post-delete row count instead of trusting `targetIdx`). I grade it P2 and the
round NO-GO not because the outcome is severe — it is milder than N-1, which
the twelfth review graded P3 — but because it is a **newly introduced,
silent, user-visible regression on the common path**, minted by a
release-hardening commit whose entire purpose is protecting the transcript. A
reviewer who weighs only outcome-severity would call it P3 and the round GO;
I state that counterargument explicitly rather than hide the judgement call.

Beyond that, the round repeated the series' signature pattern twice more, in
both cases inside the commit that was supposed to eliminate it: `6c695dcb`
added a comment asserting the recreate "runs … while this handler still holds
the reservation, so no concurrent writer can interleave", which is false on
every path except one (P3-2), and `a8b35e96` reduced
`drainOrReleaseFinal`'s `mb.stopped` finalize branch to code that is
byte-identical to the fall-through below it while leaving a twenty-line
comment saying the re-check is "required, not optional" (P3-3, confirmed by
deleting the whole branch and watching the entire `internal/agent` package
stay green).

---

## Findings, by severity

### P2-1 (blocker) — `6c695dcb`'s success-path defer duplicates the operator's prompt whenever the replacement turn compacts

- `internal/server/handlers_agent.go:844-846` — the new unconditional defer
- `internal/server/handlers_agent.go:942` — `if len(allMsgs) > targetIdx { return }`
- `internal/agent/agent_turn.go:1838-1842` — silent compaction, synchronous, inside the turn
- `internal/agent/agent_compaction.go:690-694` — the commit loop that deletes the summarised rows
- (same shape via `runSummarizeBody`, `internal/agent/agent_compaction.go:507-511`)

Before `6c695dcb` the recreate ran only inside `if err != nil`. The defer now
runs on **every** return past step 3, including the ordinary success return at
`handlers_agent.go:920`.

The positional check's premise — "any row now present at or past that index
was necessarily created by the replacement turn" — silently assumes the row
count is monotonic from step 3 onward. Compaction breaks it. Counting:

| stage | raw rows |
|---|---|
| after steps 2/3 | `targetIdx` |
| + `createUserMessage` + assistant | `targetIdx + 2` |
| + hidden summary row | `targetIdx + 3` |
| − `toSummarise` deletions | `targetIdx + 3 − n` |

so the defer misfires whenever `n >= 3`. `runSummarizeSilent` bails only when
the visible window is `< 4` messages (`agent_compaction.go:535`) and splits at
`len/2`, so any window of 6+ messages yields `n >= 3`. Note also that the
rerun's tail delete does **not** reset the session's token counters, so the
sliding-window trim that sets `silentCompactNeeded`
(`agent_turn.go:1045-1067`) still fires against the pre-rerun usage — the
combination "rerun a message deep in a long session" is the natural trigger,
not an exotic one.

**Failure scenario (concrete).** Session of 8 rows `U A U A U A U A`;
operator reruns the 4th user message (`targetIdx == 6`). Steps 2/3 leave 6
rows. The replacement turn succeeds, writes the prompt + a reply, and compacts
away 4 older rows. Final list:

```
final[0] user      "prompt-c"
final[1] assistant "reply-c"
final[2] user      "prompt-d"   <- the legitimately recreated prompt
final[3] assistant "new reply"
final[4] user      "prompt-d"   <- SPURIOUS duplicate appended by the defer
```

The reply the client receives is `EventResponse` — a clean success. Nothing is
logged. The phantom row publishes a `CreatedEvent`, so the browser shows the
operator's prompt echoed a second time after the answer, and the row enters
the next turn's LLM context as a trailing user message with no reply.

**Verification: CONFIRMED by execution.** Out-of-tree copy of HEAD, driving
the real `handleRerunMessage` with a coordinator fake that fires the real
`onHandoff`, creates the user + assistant rows exactly as `createUserMessage`
does, then deletes older rows exactly as `runSummarizeSilent`'s commit loop
does, and returns `nil`:

```
reply type=response error=""
final[0] role=user      text="prompt-c"
final[1] role=assistant text="reply-c"
final[2] role=user      text="prompt-d"
final[3] role=assistant text="new reply"
final[4] role=user      text="prompt-d"
    expected: 1   actual: 2 copies of the operator's prompt
```

**Revert-check, executed by me:** disabling only the new defer (leaving the
explicit call in the `err != nil` branch intact) makes the duplicate
disappear — the probe passes. So the duplicate is attributable to `6c695dcb`'s
defer specifically, not to `b1c1f349`'s positional check on its own.

**Fix shape** (so the next round doesn't re-derive it): the defer needs to
distinguish "we never handed off / we bailed" from "the turn ran to
completion". The handler already has exactly that signal — `releaseOnBailout`
is flipped by `onHandoff`, and `err` is in scope. Either guard the defer on
"the run did not return success", or stop trusting `targetIdx` as a stable
watermark and capture `len(allMsgs) - (len(allMsgs) - targetIdx)` … i.e.
record the actual post-delete row count (or the recreated row's ID) at the
commit point and compare against that instead of against an index into a list
that no longer exists.

---

### P3-1 — `b1c1f349`'s positional check has its own false-suppression class, with the same prompt-loss end state N-1 was filed for

- `internal/server/handlers_agent.go:942` (`if len(allMsgs) > targetIdx`)
- `internal/server/handlers_agent.go:925-932` (the doc: *"any row now present
  at or past that index was necessarily created by the replacement turn"*)
- `b1c1f349`'s commit message: *"Collision-proof by construction"*

The check is a **count** comparison, so it cannot tell "a row the replacement
turn created" from "a row anything else created". Two reachable sources of the
latter:

1. **A concurrent writer in the release→check window.** On every early-error
   path except the `mb.stopped` rebind branch, the agent layer has already
   called `ReleaseExclusive` by the time `RunWithReservedOwnership` returns
   (this is `earlyReturnCoordinator`'s own documented shape, and
   `agent_run.go:182-189`'s freshly-rewritten paragraph says so explicitly).
   The mailbox is `mbIdle`; a queued `handleSendMessage`, a second WS client,
   or the run-queue pump executing a durable row for the same session can
   persist a row before `recreateRerunPromptIfLost` lists.
2. **A tail delete that failed.** `handlers_agent.go:783-789` logs and
   continues on both `ForceDelete` failure and generic `Delete` failure, so a
   surviving old row can occupy an index `>= targetIdx`.

Either way the count exceeds `targetIdx`, the recreate is suppressed, and the
operator's prompt is gone — the exact B-1/N-1 end state. (The pre-`b1c1f349`
text scan handled both of these correctly; it failed on the *duplicate-text*
case instead. This is a trade, not a pure win — a narrower trigger for a
comparable outcome.)

**Verification: CONFIRMED by execution** for source (1). Out-of-tree copy,
history `[U "words I would hate to lose", A]`, rerun the user message
(`targetIdx == 0`), coordinator releases then a different writer persists one
unrelated row, then the run fails:

```
ERROR ws: rerun agent error err="model resolution failed: smart model not found"
final[0] role=user text="unrelated message from another client"
    "the operator's prompt must survive" — got 1 message, prompt absent
```

Source (2) is **CONFIRMED by reading** only (forcing a `Delete`+`ForceDelete`
double failure needs DB-level failure injection this codebase has no seam
for).

Not P2: source (1) needs a genuinely concurrent writer landing in a narrow
window, and source (2) needs a DB error. Both are strictly rarer than N-1's
"operator typed 'continue' twice" trigger, so the fix is still a net
improvement — this is a residual, not a regression.

---

### P3-2 — `6c695dcb` minted a comment asserting a mutual exclusion that does not exist on any path but one

- `internal/server/handlers_agent.go:837-839`:

```go
// It runs before the releaseOnBailout/probeHeld defers (LIFO), i.e.
// while this handler still holds the reservation, so no concurrent
// writer can interleave with the list-then-create.
```

The LIFO half is right: the recreate defer is registered at `:844`, after
`releaseOnBailout`'s and `probeHeld`'s, so it does run first. The conclusion
drawn from it is wrong, because those two defers are not what holds the
reservation on most paths:

| path | who released before the defer runs |
|---|---|
| success | `runOwned`'s defer, inside `RunWithReservedOwnership`, already returned |
| ordinary early error | the agent layer itself (`agent_run.go:196, 201, 228`, `coordinator_run.go`) |
| `mb.stopped` rebind branch | **nobody yet** — the caller's defer is the only release (the one case the comment describes correctly) |
| panic before handoff | **nobody yet** — correct |

`probeHeld` is already `false` by `:885`, so that defer is a no-op on every
path that reaches step 6 at all.

The twelfth review recorded this window as an Observation ("The recreate runs
outside the reservation"). This batch did not close it; it added a comment
stating the opposite. P3-1's probe is the executable demonstration that the
window is real.

**Verification: CONFIRMED by reading + call-graph, and by P3-1's probe.**

---

### P3-3 — `a8b35e96` left `drainOrReleaseFinal`'s `mb.stopped` finalize branch byte-identical to its fall-through, under a comment that says the re-check is "required, not optional"

- `internal/agent/mailbox_ownership.go:322-351` (the comment and the branch)
- `internal/agent/mailbox_ownership.go:365-372` (the fall-through)
- `internal/agent/agent_run.go:150-153` (untouched, now stale)

After the change the two blocks are identical statement for statement —
`replacement` appended, `submitted` appended, `return SessionAgentCall{},
false, orphaned, releaseErr` — with `orphaned` nil on entry to both. The
`if mb.stopped` branch has no behavioural effect whatsoever, yet it is
introduced by:

```go
// Re-checking mb.stopped here (rather than trusting the snapshot that
// sent us down this path) is required, not optional: …
```

That sentence was true before this commit (the branch discarded). It is now
false: the re-check decides nothing.

Same staleness one file over. `agent_run.go:150-153` still names
*"drainOrReleaseFinal's finalize step"* as one of the stopped latch's "real
teardown protections". As of `a8b35e96` the finalize step is latch-blind in
effect; the only thing in that function that still acts on `mb.stopped` is the
**entry** check at `:226` (which is what keeps `hasNext` false). The twelfth
review noted the comment "names only the finalize step, a harmless
understatement" — the understatement has inverted into a misstatement.

**Verification: CONFIRMED by execution.** In the out-of-tree copy I deleted
the entire `if mb.stopped { … }` block from the finalize step and ran the
**whole** `internal/agent` package:

```
ok  github.com/charmbracelet/crush/internal/agent  56.702s
```

Zero tests distinguish the branch from its fall-through — including
`TestMailbox_DrainOrReleaseFinal_HardStopDuringReleaseWindow_HandsWorkToDurableEnqueue`
and `TestAgent_DrainOrReleaseMerged_StoppedFinalizeEnqueuesOrphanedWork`, the
two the commit added for it.

Not a behaviour bug — outcome (a) is what the task chose and the outcome is
correct. The finding is that the branch is now dead code carrying a load-
bearing-sounding justification, i.e. exactly the divergence class this series
exists to remove. Either delete the branch (and rewrite the comment to say the
finalize step deliberately treats stopped and non-stopped identically, with
the entry check carrying the whole shutdown semantics), or keep it and say
plainly that it is retained for readability.

---

### P3-4 — the new guard's user-facing remedy points operators at UPSTREAM crush

- `internal/platform/source_tree_guard.go:98-106`, specifically `:103`:

```
  go install github.com/charmbracelet/crush@latest
```

The fork's module path is `github.com/charmbracelet/crush` but the fork is
`PHPCraftdream/crush`. `go install …@latest` resolves through the module proxy
to **upstream charmbracelet/crush** — the project CLAUDE.md describes as "an
external project", with the Bubble Tea TUI, no `sessions` family, no
`cliprovider`, no web UI, no `crush run` harness. An operator who trips this
guard (which is, by construction, a confused operator) and follows the printed
advice replaces their fork install with upstream and loses every fork feature.

The fork's actual channels are `npm install -g @phpcraftdream/crush`
(`README.md:11,15,640`, `.github/workflows/publish-fork-npm.yml:48`) and
`go run deploy.go` (`deploy.go:39`).

**Verification: CONFIRMED by reading** (README, publish workflow, deploy.go,
`go.mod:1`).

---

### P3-5 — three confirmed false negatives in `IsInSourceTree`, one of them one scratch file away from disabling the guard entirely

All four sub-items **CONFIRMED by execution** against the real `IsInSourceTree`
in the out-of-tree copy.

**(a) A `go.mod` for any other module inside `dev/` silently disables the
guard for `dev/`.** `internal/platform/source_tree_guard.go:37-38` sets
`ancestor = dir` — the `dev` directory *itself* — and `:63-67` stops the walk
at the first `go.mod` it finds ("Different module — stop, don't walk past
it"). So `<crushrepo>/dev/go.mod` declaring anything else ends the walk before
it ever reaches the repo's own `go.mod`:

```
scratch go.mod inside dev/: IsInSourceTree(".../002/dev/crush.exe") = false   (want true)
```

`dev/` is by definition a scratch directory; a stray `go.mod` there (a
throwaway module, an extracted dependency, a vendored tool) is exactly the
kind of thing that lands in it. Nothing warns; the guard just stops guarding.
The `.claude/worktrees` branch does **not** have this exposure — `:41` starts
above `.claude`.

**(b) A `.claude/worktrees/<x>/` checkout whose `.claude` parent is not itself
a crush repo is not detected**, even though the worktree's own `go.mod` *is*
crush's:

```
worktree checkout with non-crush parent: IsInSourceTree(".../003/.claude/worktrees/agent-x/crush.exe") = false   (want true)
```

Harmless **today**: this machine's worktrees are repo-local
(`D:\dev\go\crush\.claude\worktrees`), so `ancestor` lands on the repo root
and the match succeeds. But the guard covers exactly two locations and one of
them is protected only by an incidental property of where the harness happens
to put worktrees. Checking the marker directory's own subtree (or just
`ancestor = dir` for worktrees too, mirroring `dev`) would close it without
weakening anything.

**(c) The marker comparison is case-sensitive** (`:25`, `:29`). On Windows
`filepath.EvalSymlinks` canonicalises each component to its on-disk casing
(verified: `…\DEV\x.exe` → `…\dev\x.exe`), so an invocation that merely
*types* the path in the wrong case is fine — but a directory literally named
`Dev`/`DEV` on disk is never matched:

```
IsInSourceTree(".../dev/crush.exe") = true
IsInSourceTree(".../Dev/crush.exe") = false
IsInSourceTree(".../DEV/crush.exe") = false
```

**(d) `readModuleLine` requires the literal prefix `"module "`** (`:124`).
`module\tgithub.com/charmbracelet/crush` — valid go.mod syntax — is not
recognised, and because `:128` maps "no module line found" to
`os.ErrNotExist`, the walk **continues upward past that module boundary**
rather than stopping at it:

```
tab-separated module line: IsInSourceTree(".../004/dev/crush.exe") = false   (want true)
```

This repo's own `go.mod:1` uses a space, so it is latent, not live.

**What I could not break, and tried to.** No false positive on any real
install shape:

```
npm install under a plain dev/ dir     -> false   (correct)
npm install under <crushrepo>/dev/     -> true    (correct)
exe in bin/ with crush go.mod at root  -> false   (correct, per design scope)
worktrees marker without .claude parent-> false   (correct)
unrelated go.mod interposed above dev/ -> false   (correct)
```

And no infinite loop or panic on any degenerate path shape — `C:\`, `C:crush.exe`,
`\\server\share`, `\\server\share\`, `\\?\C:\dev\crush.exe`,
`\\.\pipe\x\dev\crush.exe`, `dev\crush.exe`, `crush.exe`, `.`, `""` all
terminate (measured; all `0s`–`96µs` except the UNC case below).

---

## Minor items

- **M-1 — `main.go:27-29`'s "before any other initialization" is not
  accurate.** `main.go:23` imports `_ "github.com/joho/godotenv/autoload"`,
  whose `init()` is `godotenv.Load()` — it reads `.env` from the current
  working directory and `os.Setenv`s every key, before `main()` is entered
  (Go initialisation order; `internal/dns`'s `init()` also reads
  `TERMUX_VERSION`). The substance of the claim survives — no DB open, no
  network call, no crush config read happens first, and `cmd.Execute()`,
  `AssignToNewJobObject` and the pprof listener all run strictly after the
  guard — but "any other initialization" is a stronger statement than the code
  supports. CONFIRMED by reading (`autoload.go` in the module cache) +
  language semantics.
- **M-2 — `b0998e92`'s "no `.github/workflows` file invokes [a compiled crush
  binary] either" is false.** `.github/workflows/schema-update.yml:24` runs
  `go run . schema > ./schema.json`, which compiles and executes the crush
  binary. The conclusion still holds by accident: `go run` places the binary in
  the Go build cache under the OS temp directory, which carries no
  `dev`/`.claude/worktrees` component, so the guard does not fire and no
  escape hatch is needed. `.githooks/pre-push` genuinely only runs
  `go build`/`go test`/`go run golangci-lint@…` (verified line by line).
- **M-3 — scope vs. claim on the guard.** The commit subject and the
  user-facing message both say "inside its own source tree", but only `dev/`
  and `.claude/worktrees/` are covered (the function's own doc, `:12-15`, is
  honest about this). The repo's *own* build outputs are in the uncovered set:
  `Makefile:5` (`go build -o crush .`) and `build.go:40`
  (`out = "crush.exe"`) both write to the repo root, and `make web-dev`'s own
  comment tells you to "pair with: `crush web --port 3030 --no-open`". Those
  binaries stale exactly the way `dev/crush-local.exe` did, and the guard lets
  them run. This is *necessary* — `deploy.go:106` runs `go run build.go` and
  then copies `<repo>/crush.exe` out — and it matches the user's explicit
  path-based direction, so it is recorded as a residual, not asked to change.
- **M-4 — double newline on the refusal.** The error string at
  `source_tree_guard.go:105` already ends in `\n`, and `main.go:31` uses
  `fmt.Fprintln`. Cosmetic.
- **M-5 — `drainOrReleaseMerged` discards `restartOrphaned`'s error.**
  `internal/agent/agent_ownership.go:258` calls `a.restartOrphaned(orphaned)`
  and ignores the return, while `restartOrphaned`'s own doc
  (`:352`) says *"P0-3 fix: now propagates the error instead of silently
  discarding it"* and the sibling caller
  (`abandonOwnershipWithHandoff:306-311`) does check and log it. Pre-existing,
  but `a8b35e96` routes a new class of work (everything queued on a
  hard-stopped mailbox) through this exact call, so the gap now covers the
  shutdown path too. `restartOrphanedWithRetry` logs its own `slog.Error`
  internally, so nothing is fully silent.

## Observations (no finding)

- **Shutdown latency, widened.** `restartOrphanedWithRetry` is synchronous
  (`wg.Wait()` at `agent_ownership.go:537`) with a 30 s enqueue budget plus a
  fresh 30 s outbox budget per call, and `a8b35e96` puts it on the
  normal-completion shutdown path for the first time. `CancelAll` waits on
  `runWg` for only 5 s by default (`agent_control.go`, `grace := 5 *
  time.Second`), after which `App.Shutdown` proceeds to close the DB. Outcome
  on a lost race is a logged failure, not corruption — the same posture the
  twelfth review recorded for the abandon path — but the set of shutdown paths
  that can hit it is now larger.
- **A UNC executable path makes the guard do blocking network I/O before
  anything else in `main()`.** `IsInSourceTree("\\\\server\\share\\dev\\crush.exe")`
  took **2.29 s** on this machine against a nonexistent server, entirely
  inside `os.Open` on `\\server\share\dev\go.mod`. It terminates (I measured
  it at 70 s of headroom; the 2.3 s is SMB/name-resolution, not an algorithmic
  loop), and for a *real* UNC install the shares exist so the probes are fast.
  Recording it because the guard is now the first statement of `main()`, ahead
  of any logging setup, so a stall there is a completely silent hang.
- **The guard is an accident-prevention device, not a boundary.** A hardlink
  or copy of the dev binary placed outside the tree runs normally
  (`os.Executable()` reports the launch path), and any `os.Executable` /
  `EvalSymlinks` / `Abs` failure is a documented silent no-op
  (`:80-95`). That is the right trade for what it is for; noted so nobody
  later mistakes it for enforcement.
- **`readModuleLine` mapping "file exists but has no module line" to
  `os.ErrNotExist`** (`:128`) is what makes P3-5(d)'s walk continue past a
  module boundary. Independent of the tab case, a `go.mod` that is
  present-but-unparseable is treated as absent rather than as a boundary.

---

## What I checked and found sound

### `a8b35e96`'s outcome (a) — the highest-risk *behavioural* change in the batch, and the mechanism is right

Traced independently, link by link, not from the commit message:

- **`restartOrphaned` is a durable enqueue, not a turn.**
  `agent_ownership.go:353-355` is a thin alias for
  `restartOrphanedWithRetry` (`:388-546`): synchronous (`wg.Wait()` before
  return), `EnqueueRunQueueEntry` with a `LogicalCallID`-derived idempotency
  key, `WriteToOrphanOutbox` fallback on failure, errors collected. No
  `Run`/`Stream` anywhere on that path. The batch's own new test proves the
  same thing from the other end (`countingModel` at zero,
  `p646_stopped_finalize_enqueue_test.go:117`).
- **`FromDurableQueue` calls are skipped** (`:415-419`), so re-orphaning a
  pump-delivered call cannot create a second row. This is the only
  double-execution hazard the new path could plausibly introduce, and it is
  already closed one layer down.
- **No double-handling with the abandon path.** `drainOrReleaseFinal` clears
  `mb.submitted`/`mb.replacement` while holding `mb.mu`, and does **not** bump
  the epoch, so `Run`'s deferred `abandonOwnershipWithHandoff(sessionID,
  epoch)` runs a moment later with a matching epoch, finds both queues empty,
  and enqueues nothing. If a new owner won the now-`mbIdle` mailbox first, the
  epoch has moved and the abandon is a no-op. Either way, exactly one enqueue.
- **No DB I/O under `mb.mu`.** `drainOrReleaseFinal` returns (running its
  deferred unlock) before `drainOrReleaseMerged:258` calls `restartOrphaned`.
- **Ordering is consistent with the live branches** (replacement, then
  submitted FIFO — `:342-349` mirrors `:365-372`), and per the function's own
  doc at `:359-364` the order has no atomicity consequence since orphaned
  calls are restarted independently.
- **Exactly one production caller** of `drainOrReleaseFinal`
  (`agent_ownership.go:247`), itself with exactly one production caller
  (`agent_turn.go:1874`), so there is no second context where the new
  `orphaned` return could be interpreted differently.
- **The `#296/P1-C` history the comment references is not disturbed:** the
  invariant that matters ("once in `mbReleasing`, never return `hasNext=true`
  again", `:185-207`) is untouched — `hasNext` is still hard-`false` on both
  branches, and the entry check at `:226` still prevents a stopped mailbox
  from taking the live replacement/submitted branches.

The only thing wrong with this commit is that it made the branch it changed
redundant without saying so (P3-3).

### `b1c1f349` (N-1) — closed at the level it was filed

- The text scan is gone; `targetIdx` (computed at `handlers_agent.go:714-720`,
  fail-closed at `:721-728` when the target isn't found) is the sole input.
- `TestHandleRerunMessage_PromptRecreatedDespiteEarlierIdenticalPrompt`
  reproduces the twelfth review's exact `[U1 "continue", A1, U2 "continue",
  A2]` scenario and asserts both the recreate *and* the final
  three-message transcript.
- The other direction still holds:
  `TestHandleRerunMessage_NoDuplicateWhenPromptAlreadyRecreated` (run past
  `createUserMessage`, then fail) stays green, so the check still recognises
  "already recreated".
- Residual: P3-1 above.

### `6c695dcb` (N-2) — the panic hole it targeted is genuinely closed

- The defer is registered strictly after step 3's delete (`:823-846`), so it
  cannot fire for any of the six early returns above the commit point.
- LIFO ordering relative to `releaseOnBailout`/`probeHeld` is as claimed.
- `RunWithReservedOwnership` is called **synchronously** (`:898-901`), so the
  defer cannot race an in-flight turn — the obvious hazard of moving this into
  a defer does not exist.
- `deleteCtx` (`context.WithoutCancel(holdCtx)`) is the right context for both
  the list and the create on the panic path.
- `TestHandleRerunMessage_PanicBeforeHandoffRecreatesPrompt` drives the real
  `rerunPreHandoffSeam` panic and asserts prompt survival; the older
  `…ReleasesReservation` test still asserts the reservation half separately.
- Residuals: P2-1 and P3-2 above.

### `65c09958` (M-1/M-2/M-3) — all three complete, no fourth copy left

- `ChatToolbar.tsx:228-229` now describes conditional rendering; the code it
  describes is at `:333` (`{activeSessionID && (` wrapping
  `data-test-id="header-prompt-button"`) — verified, not assumed.
- `grep "return null" web/tests/ui.spec.ts` → empty;
  `grep "confirmed regression" web/tests/*.ts` → empty. Both surviving copies
  are gone and no assertion depended on them.
- `p595_delete_streaming_test.go:272` now states only the true post-state.

### `b0998e92` — what the guard gets right

- `main.go:30` is the first statement of `main()`, ahead of
  `AssignToNewJobObject`, the pprof listener and `cmd.Execute()`, so it covers
  `--help`, `--version` and malformed args identically. (Caveat M-1.)
- Fail-open on every resolution failure (`:80-95`), so a normal install can
  never be blocked by an `os.Executable`/`EvalSymlinks`/`Abs` error.
- No false positive on any real install shape I could construct (table under
  P3-5), including npm's `node_modules/@phpcraftdream/crush-win32-x64/bin/`
  layout both inside and outside a plain `dev/` directory.
- Anchoring on a real `go.mod` module match rather than a path substring is
  the correct design and is what makes `~/dev/**`, `$GOPATH` under a `dev/`
  directory, and `D:\dev\**` on this machine all safe — the walk goes *up*
  from the marker, and `D:\dev\go\crush` lies *below* `D:\dev`, so a binary
  anywhere else under `D:\dev` never sees crush's `go.mod`.
- Terminates on every degenerate path shape tested; no panic, no unbounded
  loop.
- `deploy.go`'s pipeline is unaffected: `go run build.go` writes
  `<repo>/crush.exe` (no marker) and the `--version` verification at
  `deploy.go:207` runs the *destination* copy, not the repo one.
- Self-exec sites (`internal/cmd/queue.go:403`,
  `internal/cmd/sessions_pick.go:95`) inherit the parent's verdict, so no
  inconsistent parent/child behaviour.

### Regression spot-checks against the ninth/tenth reviews (two, per instructions)

- **`Run()` byte-identical to base — still holds.** The entire non-comment
  delta to `agent_run.go` across `28d55c33..b0998e92` is still the four-line
  `testReserveRebindSeam` guard inside `RunWithReservedOwnership`.
- **`externalSessionOwnerPID` / `externalSessionOwnerRefusal` still absent**
  from `internal/` — the tenth review's third Minor stays closed.

### Executed verification summary

| check | result |
|---|---|
| `go build ./...` (out-of-tree copy) | clean |
| `gofmt -l internal/ main.go` (real repo, read-only) | empty |
| `go vet ./internal/platform ./internal/server ./internal/agent .` | clean |
| `go test ./internal/platform/ ./internal/server/ -count=1` | ok (0.4 s / 22.9 s) |
| `go test ./internal/agent/ -count=1` (with P3-3 probe applied) | ok, 56.7 s |
| probe: successful rerun + in-turn compaction | **reproduced** — duplicate prompt (P2-1) |
| revert-check: disable the `6c695dcb` defer | duplicate gone → attributable to that defer |
| probe: unrelated row lands before the positional check | **reproduced** — prompt lost (P3-1) |
| probe: delete the whole `mb.stopped` finalize branch | full `internal/agent` package still green (P3-3) |
| probe: 12 degenerate/UNC/extended-length path shapes | all terminate; timings recorded |
| probe: 7 guard layouts (false-positive hunt) | all correct |
| probe: 4 guard layouts (false-negative hunt) | 3 false negatives found (P3-5) |
| probe: `EvalSymlinks` case canonicalisation on Windows | confirmed (`\DEV\` → `\dev\`) |

---

## Things I could not verify, labelled as such

1. **P2-1's *production* compaction trigger.** The duplicate is reproduced
   against the real `handleRerunMessage` with a coordinator fake that performs
   the same row operations `runSummarizeSilent` performs. That a real turn
   reaches `silentCompactNeeded == true` for a given real session is derived
   from `agent_turn.go:1045-1067` and the arithmetic above, not measured
   against a live provider.
2. **P3-1's source (2)** (a tail delete that fails) is read, not executed —
   there is no failure-injection seam for `message.Service.Delete`.
3. **P3-5(b)'s relevance depends on where worktrees live.** I verified this
   repo's `.claude/worktrees` is repo-local. Whether the harness could ever
   place a worktree under a non-crush `.claude` parent is outside what I can
   check from here.
4. **The `-race` sweep this series still owes was not run.** Everything above
   is `-count=1` without `-race`; the eleventh review's owed
   `-count=20`/`-race` sweep remains owed. I did not re-litigate the
   documented `TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_UsesPlainAbandon`
   flake and did not observe it in any run here.
5. **Windows-only.** Every measurement is Windows. In particular P3-5(c)'s
   conclusion depends on `filepath.EvalSymlinks`'s Windows case
   canonicalisation, which I measured; the POSIX behaviour of the guard is
   read, not executed. `os.Executable()` semantics under a hardlink (the
   "not a boundary" observation) are reasoned, not measured.
6. **Nothing was exercised through the real CLI.** No `crush` binary was
   invoked; no global config was read or written, so `CRUSH_GLOBAL_DATA` /
   `CRUSH_GLOBAL_CONFIG` were never needed. I did not repeat the
   orchestrator's two-binary manual verification of the guard.
7. **The web change was not run.** No `pnpm typecheck`, `build`, or Playwright
   run — `65c09958`'s web delta is comment-only and I reviewed it statically
   against the JSX it describes.
