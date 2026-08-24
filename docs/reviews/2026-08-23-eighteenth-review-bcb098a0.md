# Eighteenth review — `afb8372c..bcb098a0`

**Date:** 2026-08-23
**Range:** `afb8372c..bcb098a0` (2 commits: `53864eb0`, `bcb098a0`)
**HEAD reviewed:** `bcb098a0`
**Reviewer scope:** fork-owned code only; no upstream-merge component.

---

## Verdict

**GO.**

- **Blocker: 0**
- **Major: 0**
- **Minor: 2** (`M-1`, `M-2` — both comment-vs-code divergence outside the rerun
  mechanism; zero production-logic defects found anywhere)

Both commits in range are correct as landed. `bcb098a0` changes no production
logic (comments + one new permanent regression test). `53864eb0`'s seven
location-pointer edits are **all independently verified accurate** (§3).

### The exhaustion call on `handleRerunMessage`'s recreate mechanism

**Recommendation: consider the narrow rerun mechanism DONE for this review
series. Stop mining it.**

This is not a "found nothing this round" shrug — it is a call based on what
the methodology now *produces*. I ran five structurally-different mutations
against the mechanism (§1), deliberately avoiding the "delete this loop's
body" shape that #659 and #660 already mined. Results:

| Mutation | Shape | Outcome |
|---|---|---|
| MUT-1 | swap seed-loop and union-loop **order** | survives — genuinely equivalent (set union is commutative) |
| MUT-2 | `allMsgs[targetIdx+1:]` → `allMsgs[targetIdx:]` (boundary) | **caught** |
| MUT-3 | drop `releaseOnBailout \|\|` from the recreate gate | survives |
| MUT-4 | post-delete baseline `List` on `holdCtx` instead of `deleteCtx` | survives |
| MUT-5 | deferred `recreateRerunPromptIfLost` on `holdCtx` instead of `deleteCtx` | survives |

Three survivors. **None of them corresponds to a defect in the code as
written** — the code is correct at all five sites. Each survivor is only
fileable as "the suite does not pin this expression", which is *precisely* the
shape of #659 (seed loop content) and #660 (union loop content). I can
generate more of these indefinitely: the region is ~60 lines and every
expression in it is a candidate.

That is the signal. After four consecutive GO verdicts on the central design,
the review's own methodology has stopped being a defect detector on this code
and has become a coverage-pin generator. Filing a tenth item would consume
another fix-cycle to buy risk reduction that is now indistinguishable from
zero. Details on each survivor — including the one I judged strongest and why
I still declined it — are in §1.

The remainder of the budget went outside the mechanism (§2), which is where
both findings came from.

---

## §1 — Rerun mechanism: the bounded attack, and why I stopped

**Method.** Built an isolated copy of the source tree (outside git, cleaned up
afterward — the working tree was never written to; verified via `git status`
and a byte-diff of `internal/server/handlers_agent.go` against the copy after
restore). Baseline: 29 `-run Rerun` cases green. Each mutation applied, full
rerun suite run, restored.

### MUT-1 — order of operations (survives, equivalent mutant)

Swapped the seed loop and the union loop so the post-delete `List` populates
`baselineIDs` *before* `allMsgs` does. All tests green. This is correct
behaviour, not a gap: both loops only ever `baselineIDs[m.ID] = struct{}{}`
into the same map, so the composition is commutative. No test *should* catch
it. Recorded so a future round does not re-derive it.

### MUT-2 — off-by-one boundary (caught)

`for i, m := range allMsgs[targetIdx+1:]` → `allMsgs[targetIdx:]`, i.e. the
tail loop also deletes the target. Suite goes red (the seam index shifts and
step 3's target delete then fails with `sql: no rows in result set`). The
boundary is pinned.

### MUT-3 — `releaseOnBailout ||` disjunct in the recreate gate (survives)

```go
if (releaseOnBailout || !runReturned) && !recreateHandled {
```
→ `if !runReturned && !recreateHandled {`. All tests green.

I traced whether the disjunct is reachable at all. `runReturned` is set true
only at `handlers_agent.go:994`, after `RunWithReservedOwnership` returns.
From there:

- `err != nil` → the explicit call sets `recreateHandled = true`, closing the
  gate regardless.
- `err == nil` → `onHandoff` **must** have fired, so `releaseOnBailout` is
  already false. Confirmed by reading both layers: every early return in
  `coordinator.RunWithReservedOwnership` (`coordinator_run.go:627-648`) and in
  `sessionAgent.RunWithReservedOwnership` (`agent_run.go:193-260`) is an
  *error* return; `onHandoff()` fires at `agent_run.go:270-272`, immediately
  before the handoff line.

So the disjunct is load-bearing on exactly one path: a **panic inside
`recreateRerunPromptIfLost` itself on a pre-handoff error return** — which is
the path `handlers_agent.go:1010-1020` already documents verbatim
(fourteenth-review M-5). Not filed: the code is right, the rationale is
already written down, and pinning it needs a fault-injected panic in
`List`/`Create` for a window that is documented as "loses the prompt either
way" on the dominant path.

### MUT-4 — baseline `List` on the cancellable context (survives)

`a.Messages.List(deleteCtx, …)` → `a.Messages.List(holdCtx, …)`. All green.
Genuinely mild in production: this site is *before* the step-6 handoff, so
`holdCtx` is only cancelled if the operator actually cancelled, and the failure
mode is the documented "fall back to the pre-delete seed alone" path, which is
still a valid superset baseline. Degraded, not incorrect. Not filed.

### MUT-5 — deferred recreate on the cancellable context (survives) — the strongest survivor

```go
recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
```
→ `holdCtx`. All 29 green.

Unlike MUT-4 this one would be **production-fatal**, and the reason is worth
recording even though I am not filing it. `reserveCancel` *is* `holdCancel`
(`ReserveExclusive`, `agent_ownership.go:111-119`, returns the same func), and
`sessionAgent.RunWithReservedOwnership` calls `reserveCancel()`
unconditionally at `agent_run.go:265`, one line before `onHandoff`. So in
production **`holdCtx` is always cancelled at the handoff** — any use of it
past the commit point is dead. Under MUT-5 the deferred recreate's `List`
would fail on every panic-after-handoff unwind and the operator's prompt would
be permanently lost: exactly #645/#655's bug, silently reintroduced.

The suite misses it because the rerun tests drive fake coordinators
(`panicWindowCoordinator`, `mailboxLikeCoordinator`, …) that never call
`reserveCancel`, so `holdCtx` stays live in-test.

**Why not filed anyway:** the code is correct at this site; the finding would
be "add a 30th test that pins `deleteCtx` against a future edit". That is the
identical shape as #659/#660, on the identical 60 lines, for the third round
running — and MUT-3 and MUT-4 are two more of the same shape available right
now. Filing one and not the others would be arbitrary; filing all three
restarts the loop on ground four consecutive reviews have already cleared.
Recorded here so the reasoning is on the record rather than lost: if the
orchestrator disagrees with the exhaustion call, MUT-5 is the one worth
pinning, and this paragraph is the reproduction.

### Writer × path matrix (traced, no gap)

The brief asked whether some (writer class) × (seed / union / neither)
combination is unhandled. Traced all of them against the enumeration
`bcb098a0` landed:

- **Enumeration completeness re-derived from scratch.** Non-test callers that
  can ADD a message row: `agent_compaction.go:369,563`, `agent_prompt.go:81`,
  `agent_turn.go:1109,1283,1678` (all inside a reserved run/compaction, so
  excluded by step 1a), `cmd/sessions_inject.go:169` (cross-process, no lock),
  `coordinator.InjectMessage` — whose only two production callers are
  `handlers_agent.go:401` and `coordinator_background.go:87` — and
  `handlers_agent.go:1082` (the helper itself). The comment's list is exactly
  right, including the "Phase-3 branch" qualifier: `notifyBackgroundJobDone`'s
  Phase-4 branch goes through `c.Run`, which reserves.
- **Delete-only writers** (`handleDeleteMessage`, `handleDeleteMessages`):
  leave a stale ID in the set; a nonexistent ID suppresses nothing. Safe on
  both paths.
- **`handleUpdateMessageContent`**: can only edit a row that already existed,
  so its ID is in the seed → `inBaseline` true → cannot suppress. Safe.
- **Adders, three timings**: before step 2's `List` → in seed, safe. Between
  the two listings → in the union on the success path (pinned by #660), in
  neither on the failed-`List` path (the documented residual gap). After the
  baseline capture → not in baseline, so a same-text row *does* suppress — the
  tolerance `recreateRerunPromptIfLost`'s own doc states. Note the two
  non-operator adders cannot reach that state in practice: both
  `notifyBackgroundJobDone` paths write `backgroundJobSummary(…)` text, which
  never equals an operator prompt.
- **Pending injects do not shift the text comparison.** `crush sessions inject`
  writes a `pending_injects` row alongside the message, but that row is
  consumed at `agent_turn.go:1012` (`DrainPendingInjects`) and spliced into the
  *provider request*, not into `createUserMessage`'s row —
  `agent_prompt.go:73` stores `call.Prompt` verbatim. So the helper's
  `m.Content().Text == text` test cannot be defeated by an inject riding along
  with the replacement turn.

No gap.

---

## §2 — Wider sweep (findings live here)

Read with fresh eyes, not filtered through any prior finding:
`internal/server/handlers_messages.go`, `internal/cmd/sessions_inject.go`,
`internal/agent/coordinator_background.go` (all three named by this session's
own findings but never reviewed on their own merits),
`internal/agent/mailbox_ownership.go`, `mailbox_inject.go`,
`mailbox_generation.go`, `agent_ownership.go`, `agent_run.go`,
`agent_prompt.go`, `agent_usage.go`, `internal/platform/source_tree_guard.go`,
`main.go`.

### M-1 (Minor) — four stale `(agent.go)` pointers survive the "finish the cleanup" commit, and one of them now advertises a formula a documented bug-fix removed

`53864eb0`'s message says it *finishes* the `(agent.go)` cleanup `f4ab5b6b`
started, and it explicitly reasoned about one remaining case
(`internal/agent/tools/tools.go:89`, correctly left alone — it names fantasy
v0.25.2's own `agent.go`). But a full sweep of the fork's non-test sources for
`(agent.go)` / `agent.go:` turns up **four more of the same class**, none of
them touched. `internal/agent/agent.go` is currently **768 lines**, which makes
two of these falsifiable by inspection.

Ordered by value:

**(a) `internal/agent/cliprovider/usage.go:17` — states the pre-fix formula as the current convention.**

```go
//	internal/agent/agent.go:4544  PromptTokens = InputTokens + CacheReadTokens
```

Two problems. The pointer is stale (`agent.go` has 768 lines; the real site is
`internal/agent/agent_usage.go:88`, in `updateSessionTokenCounters`). More
importantly, the *formula* is stale: the code now computes

```go
promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
```

and `agent_usage.go:73-79` documents the two-term version as a fixed bug — "a
real measured turn had input=5842 / cache_creation=16984 / cache_read=0: the
prompt is 22826 tokens but was recorded as 5842, a 74% understatement." So the
comment presents the buggy sum as the downstream convention. This is
circular, too: `agent_usage.go:70-71` cites *`internal/agent/cliprovider/usage.go`*
as the authority for the three-way disjointness — so the two documents now
contradict each other about the same sum, with each pointing at the other.

**(b) `internal/agent/cliprovider/usage.go:18` — stale pointer, correct formula.**

```go
//	internal/agent/agent.go:4517  cost = in*InputTokens + inCached*CacheCreation
//	                                     + outCached*CacheRead + out*OutputTokens
```

Formula matches the code. Real site: `internal/agent/agent_usage.go:45-48`.

**(c) `internal/agent/cliprovider/effort.go:11` — stale field name and stale pointer.**

```go
// A session stores ONE reasoning effort (session.LargeModelReasoningEffort, a
// persisted column) and agent.go:1916 puts it on the context for every model,
```

`session.LargeModelReasoningEffort` does not exist; the field is
`SmartModelReasoningEffort` (`internal/session/session.go:62`, DB column
`smart_model_reasoning_effort`). The context write is
`internal/agent/agent_turn.go:386`.

**(d) `internal/agent/tools/ask_question.go:57` — fork's `agent.go`, not fantasy's.**

```go
// ... That is what lets agent.Run's
// error-classification chain (agent.go) catch it and force-finish the turn
```

The same paragraph correctly says "charm.land/fantasy's agent.go
executeSingleTool" three lines earlier — so the two `agent.go`s sit in one
paragraph and only the first is disambiguated. This one names the fork's own
chain, which lives at `internal/agent/agent_turn.go:233-241`
(`var askErr *tools.AskQuestionError`). This is exactly the fantasy-vs-fork
discrimination `53864eb0` applied to `tools.go:89` but never reached here.

**(e) `internal/agent/agent_turn.go:110-116` — a whole paragraph about a field that no longer exists.**

`handleWatchdogFire`'s doc says:

```go
// largeModel is the SAME kind of runTurn-local snapshot (taken once at turn
// start, agent.go: `largeModel := a.largeModel.Get()`), NOT a fresh re-read
// of a.largeModel here: a.largeModel is mutable mid-turn via SetModels
// ... (task #252 — the #243 extraction regressed exactly this by
// re-reading a.largeModel.Get() here).
```

The method's own parameter, 20 lines below at `agent_turn.go:136`, is
`smartModel Model`. `a.largeModel` does not exist anywhere in the package —
grep for `largeModel` returns this comment and three other comments, no field,
no assignment. The cited line `largeModel := a.largeModel.Get()` does not
exist in any file. The real mechanism is `resolveTurnConfig`
(`internal/agent/agent.go:302-308`) building an immutable `turnConfig`
snapshot, which `runTurn` reads at `agent_turn.go:273` as
`smartModel := cfg.smartModel`. Note the paragraph is not only misnamed but
*understates the guarantee*: `turnConfig` is a per-call value snapshot of every
model-shaped field (task #265 P0-1), which is stronger than the "runTurn-local
`:=` read" it describes.

**Why this is worth a Minor rather than a nit:** (a) is not line drift — it
records a superseded formula as current, in the file the fixed code cites as
its own authority, and the fix's whole point was that the two-term sum
understated the prompt by 74% and delayed compaction. (e) makes a paragraph
about a concurrency invariant read against a field name that no longer maps to
anything.

### M-2 (Minor) — `main.go`'s package doc / swagger block describes a product this fork is not

`main.go:3-10`:

```go
//	@description	Crush is a terminal-based AI coding assistant. This API is served over a Unix socket (or Windows named pipe) and provides programmatic access to workspaces, sessions, agents, LSP, MCP, and more.
//	@BasePath		/v1
```

Four independently-false claims for the fork, each verified against current
code:

- **"served over a Unix socket (or Windows named pipe)"** — `internal/server/server.go:95`
  is `lc.Listen(ctx, "tcp", s.addr)`. TCP only; there is no socket or named-pipe
  listener anywhere in the tree.
- **"LSP"** — no `internal/lsp` package exists. CLAUDE.md records LSP as fully
  removed and instructs that every upstream LSP commit be skipped.
- **"workspaces"** — no `internal/workspace` package exists (`ls internal/`
  confirms); CLAUDE.md records upstream's workspace/multi-client model as
  explicitly skipped.
- **`@BasePath /v1`** — no `/v1` route exists. `server.go:116-127` registers
  `/auth`, `/auth/check`, `/ws`, and `/`.

CLAUDE.md also records the swagger stub itself as removed (`52bb90f8`), so
these annotations feed no generator — they are pure inherited-upstream text in
a fork-owned file, describing three subsystems the fork does not have and a
transport it does not use. `main.go` is one of the files this session touched
and was named for this sweep.

### Swept clean (no finding)

- **`handlers_messages.go`** — `updateMessageAndVerify`'s mid-stream refusal is
  correctly gated on `Role == Assistant` (user/system/tool rows never carry a
  `Finish`, so an ungated `IsFinished()` would refuse every user-message edit).
  Checked the one handler that bypasses it, `handleTogglePinMessage`: it is
  *not* the same hazard, because `UpdateMessage` (`internal/db/sql/messages.sql`)
  writes only `parts`, `finished_at`, `updated_at` — a pin toggled mid-stream
  cannot be clobbered by the turn's terminal write.
- **`sessions_inject.go`** — `doInject` persists the row and *then* queues the
  `pending_injects` signal; a crash between the two leaves a visible user
  message that is simply picked up by the next run's natural preamble read
  (not a lost signal). `isSessionLockAlive`'s mtime-fast-path/PID-fallback
  split matches `InspectSessionLock`'s contract and the Windows unreadable-PID
  case is handled.
- **`coordinator_background.go`** — the Phase-4 branch's detached
  `go runAutoResumeRecovered(...)` correctly uses a *cancel-free*
  `context.Background()` (a turn can be long), while the Phase-3 inject path
  uses a bounded 30s timeout; the recover in `runAutoResumeRecovered` is on the
  right goroutine (a sibling of `OnDone`, whose own recover would not cover it).
- **`mailbox_inject.go` / `mailbox_generation.go`** — generation stamping is
  consistent: `beginCompact` bumps `current.id`, so an inject landing during a
  rerun's reservation is stamped with the reservation's id and is still
  `afterGenID <= genID` for the replacement turn's `beginGeneration`, i.e.
  injects made during a rerun hold are delivered, not stranded.
- **`mailbox_ownership.go`** — `drainOrReleaseFinal`'s `mbReleasing` state
  machine, the `stopped` entry check, and the finalize step's orphan drain all
  match their (extensive) docs. `callReleaseRecoveringPanic` guarantees the
  mailbox always reaches `mbIdle`.
- **`agent_run.go`** — checked the apparent double release at
  `agent_run.go:386-390` (`defer lk.Release()`) against
  `drainOrReleaseMerged` passing `lk.Release` as the drain's release callback.
  Not a bug: `SessionLock.Release` is `sync.Once`-guarded
  (`internal/session/lock.go:522-527`), so the second call cannot unlink a lock
  file a later owner has since created.
- **`platform/source_tree_guard.go`** — marker walk, the `.claude/worktrees`
  parent check, the per-worktree `go.mod` ancestor selection, and both
  root-termination guards (`checkDir == filepath.Dir(checkDir)`) are correct on
  Windows drive roots. `readModuleLine`'s `("", nil)` return is handled
  identically at and above the marker, matching its doc.

---

## §3 — Independent verification of `53864eb0`

Checked every location claim against current code, not just the one spot-check
the orchestrator ran. **All seven edits are accurate:**

| Edited comment | Claim | Verified |
|---|---|---|
| `mailbox.go:185` (`testLoopRearmSeam`) | `agent_run.go`, Run's turn loop, before `beginGeneration(turnCancel)` | ✅ invoked `agent_run.go:411-412`; `beginGeneration(turnCancel)` at `:451` |
| `mailbox.go:211` (`testPreAbandonSeam`) | `agent_compaction.go`, `runSummarize` | ✅ invoked `agent_compaction.go:245-246`, inside `runSummarize` (declared `:107`) |
| `mailbox.go:228` (`testPreSnapshotConsumeSeam`) | `agent_compaction.go`, `runSummarize` | ✅ invoked `agent_compaction.go:212-213`, same function |
| `mailbox_interrupt.go:176` (rearm window) | `agent_run.go` | ✅ same seam site as above |
| `mailbox_interrupt.go:190` (`reclaimReplacementOrKeep`) | called by Run's turn loop, `agent_run.go` | ✅ `agent_run.go:432`, between the seam and `beginGeneration` exactly as described |
| `stream_watchdog.go:180` (`withActivityNotify`) | `agent_ownership.go` | ✅ declared `agent_ownership.go:592` |
| `stream_watchdog.go:376` (`withActivityNotify`) | `agent_ownership.go` | ✅ same |

The commit's decision to leave `internal/agent/tools/tools.go:89` alone is also
correct — that one genuinely names fantasy v0.25.2's `agent.go`, and the
surrounding paragraph (`tools.go:88-92`) makes the attribution explicit.

The commit's *scope* claim ("finish the cleanup") is what M-1 contradicts, not
any of its individual edits.

---

## §4 — Verification performed

- Isolated source copy built and tested outside the git working tree; scratch
  copy removed afterward. Working tree confirmed unmodified (`git status`
  shows only the pre-existing ` D web/dist/.gitkeep` and untracked `docs/`
  files) and `internal/server/handlers_agent.go` byte-identical to HEAD after
  the mutation runs.
- `go build ./...` clean on the copy.
- `go test ./internal/server/ -run Rerun -count=1` — 29 cases green at
  baseline, and green again after every restore.
- Five mutations applied and reverted individually (§1); results tabulated
  above.
- No production code was changed by this review.

---

## §5 — What the orchestrator should do with this

1. **Rerun mechanism: close it out.** Four consecutive GOs on the design, and a
   fifth round in which the only surviving mutants are non-defects of a shape
   already filed twice. Further rounds on `handlers_agent.go:660-1090` should
   be considered out of scope unless a *behavioural* report comes in from
   actual use.
2. **M-1 and M-2 are both comment-only fixes** with no test implications. M-1(a)
   is the one with real content — it should be fixed to name
   `agent_usage.go:88` and the three-term sum, so the two files stop
   contradicting each other. M-1(e) needs the paragraph rewritten around
   `smartModel`/`resolveTurnConfig` rather than a search-and-replace of the
   file name.
3. If the exhaustion call is rejected, **MUT-5 (§1) is the single item worth
   pinning** — the reproduction is written out there in full.
