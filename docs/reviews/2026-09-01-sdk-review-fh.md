# SDK review round 4 — independent re-read of the R3 fix arc (c958425d..49612bfb)

Scope: the 24 files listed in the task, read in full (not just hunks), plus the
mechanisms they lean on (`mailbox_*`, `agent_ownership.go`, `ready_gate.go`,
`credentials.go`, `agent_tool.go`, `app.go`, `tools/mcp/init.go`, `prompt.go`).
Verification run: `go build ./...`, `go vet` on the four packages, and the
new/modified tests in `internal/agent`, `internal/app`, `internal/permission`,
`sdk` (all pass, `-count=1`). No source file was modified; findings below are
from code tracing unless marked otherwise.

Ordering: most severe first.

---

## F1 (HIGH) — R3-1 regressed `RunWithCredentials`: it never gets a pinned toolset, so `DisableSubAgents`/`ModelRole` are silently ignored and (application mode) MCP tools can be missing

**What is wrong.** R3-1 replaced the per-run `UpdateModels(ctx)` publisher with
per-call pinning done inside `resolveSessionModels` / `applyModelOverrides`
(`internal/agent/coordinator_models.go:242-244`, `:378-383`). The SDK's
multi-tenant entry point does not go through either of those:

- `coordinator.RunWithCredentials` (`internal/agent/credentials.go:288-307`)
  resolves via `resolveCredentialsModels` (`:338-390`), which builds
  `resolved := &resolvedOverrides{credentials: creds}` (`:358`) and, on the
  `AllowConfiguredRoleFallback` path, copies only `smart/fast/promptPrefix/
  systemPrompt/providerCfg` from `base` (`:359-365`) — never `tools`. On the
  strict default path `resolveSessionModels` is not even called.
- `resolvedOverrides.pin` skips `Tools` when `r.tools == nil`
  (`coordinator_models.go:405-407`), so `SessionAgentCall.Tools` stays nil.
- `runTurn` then takes the legacy branch `agentTools = a.tools.Copy()`
  (`internal/agent/agent_turn.go:284-287`) and PrepareStep re-reads `a.tools`
  every step (`:1078-1082`).
- The 401 rebuild funnels through `resolveCallModels` → `resolveCredentialsModels`
  (`credentials.go:314-319`), so the retry has no tools either.

Meanwhile the one thing that used to make `a.tools` reflect the current call —
`app.AgentCoordinator.UpdateModels(ctx)` in `ExecuteRun` — is gone
(`internal/app/app_run.go:642-652`), and `UpdateModels` now has no per-run
caller at all (only `overrideModelsForNonInteractive` when `--model` is passed,
`app_agent_setup.go:73`, and credential refresh in `coordinator_providers.go`).
So for every `RunWithCredentials` turn the toolset is whatever the coordinator's
constructor built asynchronously at startup (`coordinator_tools.go:143-150`,
`buildAgent` with the constructor's context, no `CallOptions`).

**Concrete failure scenarios.**

1. `sdk.Client.RunWithCredentials(ctx, RunRequest{Overrides: RunOverrides{DisableSubAgents: true}}, creds)`
   → `CallOptions.DisableSubAgents=true` is attached to ctx
   (`app_run.go:617-620`), but `applyCallDisableSubAgents`
   (`coordinator_tools.go:265-267`) is only reached from `buildTools`, which is
   only reached from `pinCallTools`/`UpdateModels`. Neither runs for this call.
   The provider request carries `agent` and `agentic_fetch`; the model can
   delegate. Before R3-1 this worked (racily) because `UpdateModels(ctx)` rebuilt
   and published the filtered toolset. `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerTools`
   and `TestResolveSessionModels_PinsPerCallDisableSubAgents` both exercise
   `Run`, not `RunWithCredentials`, so nothing catches this.
2. Same for `RunOverrides.ModelRole` (`buildToolsAgentConfigForCall`,
   `coordinator_tools.go:210-237`): an explicit `--role smart` credentials run
   with a worker configured keeps `edit/multiedit/write` instead of the
   orchestrator strip; the prompt (built per call) says "Orchestrator mode" while
   the tools disagree — exactly the mismatch R3-1's own test
   `TestResolveSessionModels_RolePolicyAgreesAcrossPromptModelAndTools` was
   written to prevent.
3. Application mode with MCP servers: `app.New` starts `go mcp.Initialize(...)`
   (`internal/app/app.go:258`) and then `InitCoderAgent` (`:296`), whose
   `buildAgent` reads the live MCP registry immediately on the ready gate
   (`coordinator_tools.go:143-150`, `GetMCPTools` at `:449`). Nothing consumes
   `mcp.EventToolsListChanged` in `internal/app`/`internal/agent` (grep: the
   only non-MCP reference is the proto enum), and `UpdateAgentModel` has no
   production caller. So if MCP init finished after that startup build — the
   common case — the shared toolset never contains the MCP tools, and every
   `RunWithCredentials` turn runs without them. `ExecuteRun`'s
   `mcp.WaitForInit` (`app_run.go:580`) no longer helps because the wait is no
   longer followed by a rebuild. (`Run` is unaffected: `pinCallTools` builds
   after `WaitForInit`.)

**Confidence.** Confirmed by code trace; every link in the chain is a direct
read (`resolveCredentialsModels` has no `tools` assignment; `pin` is
nil-guarded; `runTurn` falls back on nil). Not reproduced by a test, because
adding one would mean writing a source file.

**Fix shape (for the orchestrator, not applied).** In
`resolveCredentialsModels`, after the smart/fast resolution, pin the toolset
from the same snapshot the caller is on: `cfg, _ := c.cfg.Snapshot();
resolved.tools = c.pinCallTools(ctx, cfg)` (and when `base != nil`, copying
`base.tools` is wrong because `base` may have been built before the credential
smart model was chosen — the per-call `ctx` is what matters, so rebuild). Add a
`RunWithCredentials`+`DisableSubAgents` assertion to the app-level pinning test.

---

## F2 (MEDIUM) — R3-4 still fails open on the durable-restart path; the per-call policy does not survive `abandonOwnershipWithHandoff`, and the fallback gate is the zero value

**What is wrong.** `SessionAgentCall.RunAllowlist` is `json:"-"`
(`internal/agent/agent.go:308`), as are `Tools` (`:296`) and `CallOptions`
(`:344`). Any queued call that leaves the in-process mailbox and comes back
through the durable run queue arrives with all three nil. The paths that do
this are not exotic:

- `runOwned`'s deferred `abandonOwnershipWithHandoff` (`agent_run.go:330-332`
  → `agent_ownership.go:294-318`) pops whatever is still in `mb.submitted`
  when the loop exits on a non-cancel error and hands it to
  `restartOrphanedWithRetry` (`:394`), i.e. a `session_run_queue` row.
- `drainOrReleaseFinal` Case 4 (`mailbox_ownership.go:44-49`): a legacy call
  that submits while the owner is `mbReleasing` is "orphaned" and durably
  enqueued instead of being run by the loop.

When the pump rebuilds such a call: `call.RunAllowlist == nil` → `runOwned`
arms nothing (`agent_run.go:485-488`) → `permission.Request` finds no
session entry and falls back to `s.runAllowlistGate.load()`
(`permission.go:390-396`). That gate has **no production writer**:
`SetRunAllowlist` (`permission.go:528`) is called from nowhere outside tests
(R2-1 removed the `ExecuteRun` write). Its zero value is not restricted, and the
session is still `AutoApproveSession`'d (`app_run.go:818`, never revoked), so
the restarted turn is blanket-approved.

**Concrete scenario.** Host issues `Run(--restrict-run --allow-bash "ls")` on
session S (owner, policy P1), then a second legacy-queueing call on S with
`--restrict-run --allow-bash "cat"` (P2). Owner's turn dies on a transient
provider error that `shouldRetryTurn` classifies as terminal → loop exits →
`abandonOwnershipWithHandoff` pops the P2 call → durable row → pump runs it →
no policy → `bash rm -rf` is auto-approved. The same happens for `MaxCost`/
`MaxTokens` caps and the `DisableSubAgents` tool filter (all `json:"-"`).

**Pre-existing?** Yes for the fallback direction (pre-R3-4 the front-end defer
cleared the entry before any queued turn ran, so queued turns always ran on the
fallback). R3-4 fixes the in-loop promotion case and its test
(`TestExecuteRunLegacyQueueingTurnsRunUnderTheirOwnPolicies`) proves that
case. But the commit message's "a queued turn runs under ITS OWN policy once
promoted" is only true while the call never leaves the in-process mailbox, and
the design comment at `permission.go:240-253` still describes the fallback as
"the fail-closed direction", which it is not when the process-wide gate is
never armed.

**Confidence.** Code trace, high. Not test-reproduced.

**Fix shape.** Either persist a serialisable form of the policy spec
(`runAllowlistSpec`, not the compiled matcher) and `CallOptions` on the durable
row and recompile on rebuild, or make the fallback fail closed for
auto-approved sessions armed by `ExecuteRun` (e.g. arm a restricted empty gate
per session at `AutoApproveSession` time and let the call-scoped entry override
it). At minimum, fix the comments that call the fallback fail-closed.

---

## F3 (MEDIUM-LOW) — R3-6 is structurally present but behaviourally inert for the SDK; the production construction site cannot express the case the fix exists for

**What is wrong.** `watchdogTimeoutPolicyForCall` (`internal/agent/call_options.go:109-118`)
does what its doc says. But the only production `CallOptions` construction
site (`internal/app/app_run.go:602-620`, verified by grep — every other
`CallOptions{` literal is in tests) sets

```go
TimeoutOptionsSet: overrides.TimeoutExtendsOnProgress || overrides.TimeoutHardCap > 0,
```

which is literally the pre-fix heuristic. An SDK caller passing an all-zero
timeout policy still gets `TimeoutOptionsSet=false` and inherits
`a.timeoutExtendsOnProgress/a.timeoutHardCap`. The commit and comment say so
openly ("ExecuteRun's semantics are unchanged"), so this is not hidden — but it
means R3-6 fixed nothing observable for `sdk.Client.Run`/`RunWithCredentials`,
which is the surface the review finding was about.

Two further facts make the finding moot rather than dangerous today:
`SetAgentTimeoutOptions` (`coordinator.go:483-485`) has no production caller,
and `buildAgent` never sets `SessionAgentOptions.TimeoutExtendsOnProgress/
TimeoutHardCap`, so the shared fields are always zero in a real process —
"inheriting the shared policy" and "deliberate zero" currently produce the same
watchdog. The bug is unreachable; it is also unexpressible. If the intent is to
close it, `RunOverrides` needs optional fields (pointer or a `TimeoutOptionsSet`
bool of its own) so the SDK can say "zero on purpose".

**Test quality note.** `TestWatchdogTimeoutPolicyForCall_DeliberateZeroIsNotInheritance`
and `_ConcurrentCallsAreIsolated` (`call_options_test.go:343-416`) test the
pure resolver only. The "concurrent" variant is vacuous as a race test: both
goroutines read immutable `CallOptions` values and never-rewritten shared
fields, so `-race` has nothing to observe. Neither test touches `ExecuteRun`,
so they would pass with the production site removed entirely.

---

## F4 (MEDIUM-LOW) — R3-5 doc pass missed the four per-method guarantees that R3-2 invalidated

`sdk/sdk.go` still states, on every public call:

- `Run` (`:403-405`): "An admitted Run always finishes against a fully live
  App: Close waits for it before releasing any resource (see Close)."
- `RunWithCredentials` (`:462-464`), `Messages` (`:552-554`), `Session`
  (`:569-571`): same sentence.

Since R3-2 that is false on the forced path: `cancelAgentsBeforeRelease`
returns `true` without waiting for `drained`
(`internal/app/app_lifecycle.go:129-134`), and `releaseResources` then stops the
pump, runs every cleanup func (`mcp.Close`, background-shell `KillAll`) while
the admitted call is still running (`:156-227`). Only the DB and the in-memory
handles are held back. The `Close` doc and README were updated; these four
were not.

Smaller doc/code mismatches in the same area:

- README `:254-256` / `ShutdownResult.Forced` doc: "`Forced` is therefore
  `true` only when agent work was still busy after cancellation". Code also
  ORs `app.RunQueuePump.Stop()` (`app_lifecycle.go:156-161`), so a pump worker
  that ignores its grace forces the shutdown with all agent work idle.
- On the cooperative-drain branch (`app_lifecycle.go:114-120`) `CancelAgents`
  still decides `Forced`: a background summarize/title generation that does
  not join within `DefaultCancelAllGrace` makes an otherwise fully drained
  `Close` forced (handles left open, host must call `CloseEphemeralConnsForced`).
  The README's phrasing "in-flight work that ignored the cancellation" covers
  this only if the reader knows background agent work counts as in-flight.

---

## F5 (LOW) — `CloseEphemeralConnsForced`'s ordering guard is keyed on "Close started", not "Close finished", so it permits the one window it describes as dangerous

`sdk/sdk.go:738-744` reads `c.closing`, which `beginShutdown` sets at the very
start of phase 1 (`:699-710`). During phase 2 — up to `DefaultCancelAllGrace`
plus `CancelAll`'s own grace, i.e. seconds — admitted `Run`/`Messages`/`Session`
calls are still executing against the in-memory handles. A host that calls
`CloseEphemeralConnsForced` from another goroutine while `Close` is draining
(the only way to call it "concurrently", since `Close` blocks) gets `nil` and
closes the handles under those admitted writers; the later graceful branch's
`closeEphemeralConns` is then a no-op (`connsClosed` already true), and the
in-flight calls fail with `sql: database is closed` instead of finishing.

The doc says the guard exists because closing early "would leave a still-open
Client admitting calls against closed database handles". New admissions are
indeed refused after `closing=true`; already-admitted ones are not protected.
A guard on "closeOnce has completed" (a `closed` flag set after
`ShutdownAfterDrain` returns, under `admissionMu`) closes this with no
legitimate caller lost — nobody can know `Forced` before `Close` returns.

Edge: `Close()` on a `Client` with `app == nil` returns early without
setting `closing` (`:641-643`), so `CloseEphemeralConnsForced` reports "before
Close" after a Close was called. Harmless (such a client has no
`closeConns`), but the message is misleading.

---

## F6 (LOW) — R3-3: the last gate's bail-out still mutates the session (ended_reason, on-finish hook), and the test only exercises the first gate

`ExecuteRun` registers the `SetEndedReason(cleanupCtx, sess.ID, reason)` defer
and the on-finish-hook defer at `internal/app/app_run.go:931-955`, *before* the
final `checkHoldCanceled()` at `:973`. A cancel landing between the
`ClearCancelRequest` gate (`:867`) and `:973` therefore returns with
`hookExitReason = "cancelled"` and:

- overwrites the session's `ended_reason` (which still described the previous
  run) with `"cancelled"` for a run that never started;
- fires `OnFinishHook` with `exit=cancelled`, `cost=0`, `tokens=0`.

Both writes use `context.Background()`-derived contexts, so the hold's
cancellation cannot stop them. The "no mutation" contract stated in the
`checkHoldCanceled` comment (`:757-760`) and in the test header
(`app_run_hold_cancel_test.go:3-9`) does not hold for this window.

`TestExecuteRunMailboxCancelDuringAdmissionHoldAborts` parks *inside*
`ReserveExclusive` and cancels there, so the first gate (`:783`) always fires;
the `SetBudget`/`SetEndedReason`/`Rename` block (`:877-902`, guarded only by
`mutCtx` propagation into SQLite, no explicit gate) and the final gate are
never exercised. The test's `EndedReason` equality assertion therefore passes
for a reason unrelated to the gate it claims to cover.

Otherwise the gate coverage is complete for the things that matter: every
`Sessions.*` write between reservation and handoff is either explicitly gated
or on `mutCtx`; `AutoApproveSession` (no ctx) is explicitly gated; no `mutCtx`
use survives past the `go runAgentTurnRecovered` line (verified: the event loop
and `finish` use `ctx`). `reservedHold` really is `context.WithCancel(ctx)`
wired as the era's cancel (`agent_ownership.go:112-121`,
`mailbox_generation.go:48-60`), so caller cancellation, `Cancel(sessionID)`,
`CancelAll`'s `hardStop`, and an `InterruptAndSend` during the hold all trip it
— and nothing else does before the handoff. No false-abort path found.

---

## R3-4 ABA / stale-clear analysis — verified airtight for the reachable ID space

`runOwned` arms at loop top (`agent_run.go:485-488`) and clears with the *last
armed* id at loop end (`:424-429`), registered after the ownership/lock defers
so it runs before them. `ClearSessionRunAllowlistForCall` is compare-and-delete
(`permission.go:598-604`). Cases checked:

- New owner arms `C3` after the old loop's `runTurn` drain flipped the mailbox
  idle but before the old loop's defers run → old clear(`C2`) is a no-op. OK.
- New owner has *not yet* armed when the old clear fires → entry deleted, new
  owner arms shortly after; a new owner with no policy correctly gets fallback
  rather than `C2`'s policy. OK.
- Same `LogicalCallID` in two overlapping loops: `rebuildCall` (401) and the
  transient-retry loop reuse the ID but only after the previous `Run` returned
  (its clear already ran). Durable rebuilds keep the ID but carry
  `RunAllowlist == nil`, so they arm nothing and there is nothing for a late
  clear to delete. No reachable same-ID overlap.
- Sub-agent child entries (`coordinator_subagents.go:116-125`) are copied with
  the parent's `ownerCallID` and removed by an unconditional
  `ClearSessionRunAllowlist(child)` on return — no interaction with the
  parent's compare-and-delete.
- `SetSessionRunAllowlistForCall` lacks the `sessionID == ""` guard the sibling
  setters have (`permission.go:589-593`); unreachable because `Run` rejects an
  empty session id first, but inconsistent.

One semantic to be aware of (documented for replacements, but it also applies to
every `call = next` promotion): a queued call carrying **no** policy inherits
the previous turn's armed entry. That is the restrictive direction and fine for
the SDK, whose sessions are auto-approved, but it means a web-originated
follow-up on an SDK-auto-approved session runs under the last SDK policy.

---

## R3-2 state machine — verified; no leak found

`cancelAgentsBeforeRelease` (`app_lifecycle.go:102-142`): timer stopped by
defer; the abandoned `drained` channel is closed later by `release()` when the
last admitted call returns (`sdk.go:686-693`), and nothing reads it twice; the
forced return never blocks. `CancelAll` (`agent_control.go:126-212`) latches
`shuttingDown` permanently, which is correct for `Close`. An admitted
`ExecuteRun` that reaches `Run` after `CancelAll` gets `ErrAgentShuttingDown`
and, on the reserved path, releases via the un-disarmed bail-out defer
(`app_run.go:733-737`) — no wedged mailbox. The "unbounded residual wait" for
non-agent calls is real and honestly documented; the only in-process thing I
found that could hold it open without a caller ctx is `mcp.WaitForInit` on a
`Background` ctx in application mode, which the doc's "cancel the contexts you
handed to your own calls" advice covers.

`TestCloseCancelsStuckAdmittedRun` / `TestCloseForcedWhenRunIgnoresCancellation`
(`sdk/close_stall_test.go`) are channel-gated end to end; the only real time is
the documented 5 s grace itself, and every wait has a bounded failure branch.
Both would fail on the pre-R3-2 `Close` (`<-drained` before shutdown).

---

## R3-1 — remaining observations beyond F1

- `pinCallTools` (`coordinator_models.go:464-486`) builds the full coder
  toolset **and a fresh sub-agent** (`buildTools` → `agentTool` →
  `buildAgent(isSubAgent=true)`, two `readyWg.Go` tasks) and then waits on the
  coordinator-wide ready gate. `resolveSessionModels` is called with
  `wantTools=true` from paths that never use the tools:
  `BuildSystemPromptForSession` (`coordinator_summarize.go:64`),
  `buildSummarizeSnapshot` (`:135`), `handleInterruptTick`
  (`coordinator_interrupt.go:174`), and the interrupt/inject paths (`:257`,
  `:543`). Each is a wasted toolset+sub-agent build plus a `Wait` that blocks on
  *every* other caller's pending builds (the gate has one counter). Not a
  correctness bug; latency under concurrent tenants.
- The ready gate's error is sticky (`ready_gate.go:83-85`) and every run entry
  point returns it forever. Any per-call sub-agent build failure therefore
  bricks the coordinator for the process lifetime. Pre-existing (the removed
  `UpdateModels` did the same build), but R3-1 adds trigger sites. `prompt.Build`
  swallows git errors, so a cancelled call ctx does not trip it today.
- `withProviderOptionsOnLast` clones (`tool_provider_options.go:64`), so the
  pinned slice is never mutated. `TestRunTurn_PinnedToolsAreStableAcrossStepsAndSharedSetTools`
  (`coordinator_tool_pinning_test.go:371-506`) is real and deterministic and
  would fail on pre-fix `runTurn` (PrepareStep re-read `a.tools`).
  `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerTools` likewise.
- `UpdateModels` stripping `CallOptions` (`coordinator_models.go:493-495`,
  `:727`) is correct; `TestUpdateModels_NeverPublishesPerCallToolFilter` covers
  it.

---

## Test quality summary

| Test | Exercises the real mechanism? | Would fail pre-fix? | Timing |
|---|---|---|---|
| `TestRunTurn_PinnedToolsAreStableAcrossStepsAndSharedSetTools` | yes (real `sessionAgent`, gated mock model) | yes | channel-gated, 60 s guards |
| `TestResolveSessionModels_*`, `TestUpdateModels_NeverPublishes…` | yes | yes | sync |
| `TestExecuteRunSameSessionBusyLoserCannotClobberWinnerTools` | yes, `Run` path only (see F1) | yes | channel-gated |
| `TestExecuteRunMailboxCancelDuringAdmissionHoldAborts` | first gate only (see F6) | yes | channel-gated |
| `TestExecuteRunLegacyQueueingTurnsRunUnderTheirOwnPolicies` | yes, in-loop promotion only (see F2) | yes (matrix is discriminating) | `require.Eventually` on mailbox state, 1 ms poll — a state signal, not a sleep |
| `TestClearSessionRunAllowlistForCall_*` | yes | yes | sync |
| `TestCloseCancelsStuckAdmittedRun`, `TestCloseForcedWhenRunIgnoresCancellation` | yes / partially (stonewall fakes `CancelAll`) | yes | real 5 s grace, otherwise gated |
| `TestCloseEphemeralConnsForcedBeforeCloseIsRejected` | yes | yes | sync |
| `TestWatchdogTimeoutPolicyForCall_*` | pure function only (see F3) | yes, but not the production site | sync; "concurrent" variant is vacuous |
| `TestRunAgentTurnRecovered_NilResultNoError` (modified) | yes | pins the corrected `(nil,nil)` mapping | sync |
| `TestExecuteRunSameSessionLegacyQueueingStillQueuesBehindOwner` (modified) | yes | the added `Eventually` removes the CI race | state poll |

No `time.Sleep` in any of the listed files (grep). No new test has the
"sleep and assume scheduled" shape that bit `p1_1_main_loop_timeout_test.go`.

---

## Things checked and found clean

- R3-3 `mutCtx` never leaks past the handoff; hold ctx derivation verified.
- R3-4 compare-and-delete is airtight for distinct ids; retries are sequential.
- R3-2 no goroutine/channel leak; timer stopped; `Forced` observable as documented except for the pump/background-work nuances in F4.
- R3-5 README phases 1–3, the reclaim guard description, the multi-client claim (`TestOpenLibraryMode_TwoEphemeralClientsAreIsolated` exists; library mode still calls `go mcp.Initialize` with an empty map so `WaitForInit` returns) all match code.
- R3-6 resolver correct; only one production construction site (F3).
- `runAgentTurnRecovered` `(nil,nil)` → queued mapping is consistent with `sessionAgent.Run`'s single `(nil,nil)` return (`agent_run.go:88-91`).

---

## Verdict

Not ready to trust as-is. Five of the six fixes are correct for the paths their
tests cover, and the concurrency reasoning in R3-2/R3-3/R3-4 holds up under an
independent re-derivation. But R3-1 introduced a real regression on the SDK's
flagship path: `RunWithCredentials` now never gets a per-call toolset, so
`DisableSubAgents` and `ModelRole` are silently ignored and application-mode
MCP tools can be absent for the whole process lifetime (F1) — that must be fixed
before the SDK's multi-tenant story is honest. R3-4 should either persist the
policy across durable restarts or stop describing the fallback as fail-closed
(F2). R3-6 changed no production behaviour and should be recorded as such
rather than as a closed finding (F3). F4–F6 are doc and edge-case cleanups that
can follow.
