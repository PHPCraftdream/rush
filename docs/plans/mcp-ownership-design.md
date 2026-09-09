# MCP Ownership Design — Target Model and Line Budget (stage 2)

Task #906, stage 2 of `docs/plans/2026-09-09-mcp-consolidation-plan.md`.
Inputs: `docs/mcp-invariants.md` (stage 1.3: S = 5, M = 25, X = 0), the plan
including its 2026-09-09 correction box, and
`docs/plans/mcp-consolidation-baseline.md`. All numbers below were counted on
worktree HEAD `4c2f11bd3` (branch `p906-ownership-design`, 2026-09-09) by
reading code, not by estimating. Line references are to
`internal/agent/tools/mcp/init.go` unless another file is named; they drift.

## Summary for the operator

| Option | What moves | `init.go` after | S-laws unbreakable | Effort | Test sites touched |
|---|---|---|---|---|---|
| (a) full migration | all 12 globals | **≈ 6445** (−0.5 %) | 5 of 5 (15a already structural) | 3–5 days, 8–10 suite cycles | ≈ 240 rewritten refs |
| (b) narrow | `initDone`, `stateOwners`, `sessions`; delete generation | **≈ 6450** (−0.4 %) | 2 of 5 (+15a) | 1–1.5 days, 3 suite cycles | ≈ 110 rewritten refs |
| (c) stop at stage 1 | nothing | 6478 (unchanged) | 1 of 5 (15a, pre-existing) | 0 | 0 |

The plan's `init.go` < 2000 target is **dead**: a full migration deletes
~34 lines. The migration's value was never lines; measured against the defect
record, the class it structurally eliminates produced one realized **P1**
(R16-5) and one **P2** (CR-3) — both closed with standing oracles. Both
realized MCP **P0s** (CH5-1, CR-1) are M-class and are untouched by any
ownership move. Recommendation: **stop (c)**; if any movement is wanted,
**(b)** is the only bounded version. Details in the Recommendation section.

---

## Part 1 — The line budget

### 1.1 Method

Every guard line whose only purpose is cross-owner protection was enumerated
from the registry's S set (INV-02a, 03a, 04b, 15a, 22b) by grepping all
`owner ==`/`owner !=`, `isCurrentLocked`, generation-comparison and
`initDone` sites in the six prod files, then reading each site. A line counts
as **deleted** only if it disappears entirely; lines that keep an M-leg
(`closing`, epoch, store checks) count as **shortened**, not deleted. Package
globals were counted with a bare-identifier regex (field accesses like
`a.generation` excluded); the method is fuzzy by ±10 % and matches the plan's
1354 internal references (I count 1338).

Baseline: `init.go` = **6478 lines / 188,707 bytes**, 269 functions; package
prod = 7164 lines in 6 files (matches the stage-0.4 baseline).

### 1.2 What a migration deletes — the complete S-guard inventory

**INV-03a: the owner-generation mechanism (~24 lines, all deletable).**
Once `sessions` + `stateOwners` are owner-relative, every owner-generation
comparison is tautological (an admission can only reach its own owner's maps)
and the whole mechanism is vestige:

| Lines | What |
|---|---|
| 376 | package `generation uint64` declaration |
| 794 / 1114 / 698 | `Owner.generation` / `serverAdmission.generation` / `clientLease.generation` fields |
| 1700, 1704 | `generation++` and its capture in `acquire` |
| 1419, 1479, 1594, 5381, 5491 | five capture sites (`generation: o.generation` / `ownerGeneration(o)` / `admission.generation`) |
| 5387–5392 | `ownerGeneration()` helper |
| 2369–2373 | `acceptsGeneration()` — already dead code, zero callers |

Shortened (M-legs remain): 730–731, 1235, 1299–1301, 1337–1339, 3916, 4424.

**INV-22b: the `initDone` package mirror (4 lines, all deletable).**
Decl 375; mirror writes 1725 (`acquire`), 2244 (`beginInitialize`), 2486
(`finishClose`). `WaitForInit` (2780–2790) is rewritten, not deleted: it reads
`currentOwner().initDone`. This closes CR-3's mechanism by construction: a
waiter can no longer capture a dying owner's barrier from package state.

**INV-02a/04b: the current-owner fences (5 lines, deletable only at the very
end).** `coordinateClose`'s early return 2398–2401 (4 lines) and the
`if owner == o` guard 2481 around the registry reset. These are deletable
**only after the entire reset footprint is owner-relative** — 2482–2488 clears
`committedAdmissions`, `sessions`, `states`, `stateOwners`, `allTools`,
`allPrompts`, `allResources`, `leases`, and shuts down `broker`
(`resetRegistryLocked` 2494–2516). If any of those stay package-level, the
guard must stay with them.

**INV-15a: 0 lines.** Pre-existing structural law; the fence already lives on
the ConfigStore (see §2.5). Nothing deletable, nothing to do.

**What is NOT deletable — important honesty note.** The `isCurrentLocked()`
rejection branches (1383–1385, 1460, 1512, 1575, 1968, 2215, 2224, 2238,
2289, 5589, 5658) and the `owner != o || o.closing` gates (980, 996, 1017,
2164, 2184, 2657, 1799, 2321, 4382, 6032) do **not** disappear. Their
cross-owner operand becomes tautological, but the branches degenerate to the
closing fence and stay load-bearing for within-owner reasons: e.g.
`admitServerWithConfig` does `o.initWG.Add(1)` (1436) under that guard, and
`Add` after `Wait` has completed is a `panic` — the guard is what makes the
check atomic with the owner swap under `lifecycleMu` (INV-06 discipline).
About 17 lines shrink; none of these are deletions.

**Total: ~34 lines deleted (~1.2 KB of 188.7 KB), ~17 lines shortened,
~10 stale comments updated.** That is the entire line payoff of stage 3.

### 1.3 What stays — the M machinery

Rough block accounting of the ~6410 lines no ownership move touches:

| Block (init.go) | ~Lines | Laws |
|---|---|---|
| `ClientSession` + operation leases (35–250) | 216 | INV-19, INV-01 |
| `sessionCloser` + close queue (251–364) | 114 | INV-04a |
| lease registry + `serverLease` + `clientLease` (402–788) | 387 | INV-06, 07, 19 |
| `Owner` struct + session tracking (789–897) | 109 | INV-04a |
| `fallbackWorker` + enqueue/adopt (898–1021) | 123 | INV-04a, 09 |
| `addTransaction` (1023–1107) | 85 | INV-11, 23 |
| `serverAdmission` + validity (1109–1366) | 258 | INV-03, 10, 24, 25 |
| admit/invalidate/cancel (1368–1641) | 274 | INV-03b |
| `Acquire`/reclaim/`currentOwner` (1642–1736) | 95 | INV-02b |
| refresh queue (1738–1945) | 208 | INV-16, 17 |
| uncertainty capture/reload (1947–2004) | 58 | INV-15b |
| final-turn + reconcile (2056–2205) | 150 | INV-25, 14 |
| init barrier (2221–2284) | 64 | INV-22a |
| `commitRenewal` (2296–2386) | 91 | INV-19, 08 |
| `Close`/`finishClose`/reset (2387–2516) | 130 | INV-04a |
| exported wrappers + states (2580–2646) | 67 | API surface |
| `Initialize`/`InitializeSingle` + retry (2649–2970) | 322 | INV-25, 02b, 15b |
| prepare/publish client (2971–3260) | 290 | INV-11, 08, 06 |
| Disable/Enable (3260–3658) | 400 | INV-12, 13, 03 |
| Add/Replace/rollback (3658–4448) | 791 | INV-08, 09, 10, 11, 12, 23 |
| Remove (4449–4639) | 191 | INV-12, 03, 08 |
| fence/detach (4646–4738) | 93 | INV-14, 05 |
| fallback start (4739–4943) | 205 | INV-08, 09 |
| getOrRenew (4944–5363) | 420 | INV-19, 24, 03, 25 |
| state/admission events (5365–5611) | 247 | INV-18, 19 |
| skipped transitions (5622–5680) | 59 | INV-12, 18 |
| `sessionContext`/createSession (5681–5966) | 286 | INV-01 |
| transport cleanup/notify (5967–6112) | 146 | INV-20, 16, 24 |
| createTransport/HTTP/stdio (6113–6478) | 366 | INV-20, 21 |

This confirms stage 1.3: 20 of 25 laws are wholly mechanical; the migration
simplifies their cross-owner branches (the ~17 shortened lines) and deletes
none of them.

### 1.4 The resulting sizes

- **Option (a), full migration: 6478 → ≈ 6445** (−34 lines, −0.5 %; −1.2 KB).
- **Option (b), narrow: 6478 → ≈ 6450** (−28 lines; keeps the 5
  current-owner fence lines because `broker`/`leases`/`states`/`all*` remain
  package-level and `resetRegistryLocked` must not run on them cross-owner).
- The plan's stage-3 exit gate "`init.go` < 2000" is **unreachable by ~3.2×**
  through this migration. Upstream's 1391 lines had no owner concept, no
  admissions, no leases, no transactions, no refresh queue, no uncertainty
  fencing — that machinery is the M majority and it is the fix, not the bug.

### 1.5 The defect class a migration eliminates

Checked against the review record, not asserted:

| Defect | Severity | Class | Structurally eliminated by migration? |
|---|---|---|---|
| R16-5: cross-client MCP ownership — late session written into the global registry after cleanup snapshot; library client's `Close` kills the application client's MCP sessions/broker (round-16 review; fixed by `5f2dbebc` + `28ad297f` + `07a69d8f`, ~660 lines of fixes and oracles) | **P1** | S | **Yes** — per-owner `sessions`/broker make "old owner writes into new owner's registry" unaddressable; option (b) covers the session leg, the broker/close legs need option (a) |
| CR-3: `WaitForInit` hangs on a pre-Close `initDone` captured from the package mirror (36h review; fixed by `3199bf34` + `922316af`) | P2 | S (INV-22b) | **Yes** — the mirror is exactly what dies |
| CH5-1: `committedAdmissions` unsynchronized map — `fatal: concurrent map writes` (`f2914d53c`, fixed by `00c9911f5`) | **P0** | M (INV-05) | **No** — same-owner cross-server race |
| CR-1: `defer cancel()` killed published transports (`01e909d0`, fixed by `5c8641e7`/`b7d8614f`) | **P0** (on `0e4b9ed3`) | M (INV-01) | **No** — within-one-owner context handoff |
| CH5-3: no-op reload over-fenced candidates (`779802b2`, fixed by `00c9911f5`) | P1 | M (INV-25) | No |

The plan's correction box says the five S-laws cover the defect class that
"порождал P0". **The review record does not support that**: both realized
MCP P0s are M-class. The S-class realized severities are P1 (R16-5) and P2
(CR-3), both closed, both with standing oracles
(`sdk_mcp_isolation_test.go`, `implicit_owner_reclaim_test.go`,
`TestInitializeBarrierIsGenerationScopedAcrossRollover`). The migration
eliminates a class whose historical cost was real but whose current open
incidence is zero — and the M machinery, where the P0s lived, stays at any
option.

### 1.6 Two plan numbers corrected while counting

- **"1602 external call sites" is not reproducible.** Measured: rush's `mcp`
  package is imported by **15 external files** (agent ×3, server ×2, app ×2,
  tools ×5, commands ×1, cmd ×1, sdk test ×1) containing **55 `mcp.X`
  references**, of which **~31 are function calls** (the rest are types and
  state constants). The only way to approach 1602 is to count all
  `mcp.`-prefixed identifiers repo-wide (678), most of which are the
  **go-sdk** namespace (`modelcontextprotocol/go-sdk/mcp`), which is unrelated.
  This matters for §2.4: changing the exported API costs ~31 call sites, not
  1602.
- **"27 exported functions" is now 29**: 27 package-level functions + 2
  `Owner` methods (`Initialize`, `Close`). Package-level mutable state is
  **17 vars**, not 9: the 9 in `init.go:366-377`, the 3 `all*` maps, 4 test
  seams (`serverLeaseHooks` 442, `addAdmissionHooks` 449,
  `mcpReloadAfterSuccessHook` 2008, `mcpInitTestHooks` 2010) +
  `resourcesBeforePublishHook` (resources.go:23), plus 4 immutable error
  sentinels. The stage-3 gate "package-level var = 2" should read:
  **2 state vars** (`owner`, `lifecycleMu`) + seams/sentinels unchanged.

---

## Part 2 — The ownership model

### 2.1 The plan's proposal, verified

"All state from `init.go:366-377` becomes `Owner` fields; only
`currentOwner *Owner` plus a mutex stay package-level; the 27 exported
functions become thin wrappers" — **verified feasible with amendments**:

- `Owner` (init.go:790-821) already carries 21 fields including the hard ones
  (`serverEpochs`, `serverCancels`, `committedAdmissions`, `pendingGlobalAdds`,
  refresh maps, `trackedSessions`, `closer`, `fallbackWorker`). Adding the
  migrated state (`sessions`, `states`, `stateOwners`, `broker`, `leases`,
  `initDone` + the `all*` trio) is mechanical.
- The wrapper pattern **already exists** for exactly the hard cases:
  `Close` (2618), `Initialize` (2635), `InitializeSingle` (2793),
  `ensureOwner` (4639), `currentBroker` (2584), `currentClientLease` (5413)
  all resolve `currentOwner()` and delegate. The remaining getters
  (`GetStates` 2595, `GetState` 2613, `Tools`/`Prompts`/`Resources`) read
  globals directly today and convert to delegation trivially.
- **Amendment 1 — `lifecycleMu` stays package-level.** It cannot become a
  field: it guards the owner swap itself (`acquire` 1680-1730,
  `coordinateClose` 2396) and the within-owner invariants INV-05/06 sit under
  it. The plan's "one mutex for the swap" **is** `lifecycleMu`; do not invent
  a second swap mutex — that would reintroduce lock-ordering surface (INV-06).
- **Amendment 2 — the acceptance checks stay.** As shown in §1.2, the
  `isCurrentLocked`-style rejections remain as closing-fence/WaitGroup-safety
  checks. Wrappers are thin; `Owner` methods are not guard-free.

### 2.2 Package level after migration

`owner *Owner` + `lifecycleMu` (already the shape of `init.go:373-374`), plus
the 4 test-seam vars and 4 error sentinels, which are out of the invariant
system and stay. The 3 `all*` globals move (§2.3). Nothing else.

### 2.3 Complication 1 — twelve globals, not nine: confirmed

`allTools` (tools.go:29), `allPrompts` (prompts.go:16), `allResources`
(resources.go:21) are read by `canReclaimLocked` (1666-1668), cleared by
`resetRegistryLocked` (2504-2511), cleared per-name by `clearAdvertised`
(4938-4941) and the replace publish path (3949), and written by
`updateTools`/`updatePrompts`/`updateResources` in tools/prompts/resources.
**No S-law among INV-02a/04b is fully unbreakable until they move**, because
`finishClose`'s reset (and therefore the `if owner == o` fence around it)
covers them. They move as one step ("advertised data trio") after `leases`,
before `broker`. Their writers already run under leases with an owner in
scope (`getOrRenewClient` 4944 resolves `currentOwner()`), so the move is
mechanical.

### 2.4 Complication 2 — thin wrappers re-merge the SDK multi-App case

Confirmed: under `currentOwner()`-routed wrappers, a second ConfigStore in one
process still reaches the **current** owner's registry through name-only
getters, so the `IsConfigured` per-store filtering convention
(`IsConfigured` 2604-2610; filter sites agent_turn.go:310,
coordinator_providers.go:798, mcp-tools.go:47, rush_info.go:160/207) remains
necessary. Wrappers are the right *compatibility* layer and the wrong *target*
API. The target API has two tiers:

1. **Primary: `*Owner` methods.** `Acquire()` already returns `*Owner`, and
   `(o *Owner).Initialize/.Close` already exist. The mutation and query
   surface (`RunTool`, `GetStates`, `GetState`, `Tools`, `Prompts`,
   `Resources`, `GetServerToolNames`, `GetPromptMessages`, `ListResources`,
   `ReadResource`, `Refresh*`) gains `Owner`-receiver forms; an App (or SDK
   client) that acquired an owner uses only that owner — one store sees only
   its own registry, and `IsConfigured` filtering at those five sites becomes
   redundant for multi-store consumers.
2. **Compat: the 27 package-level functions keep their exact signatures** and
   delegate to `currentOwner()` (acquiring an implicit owner where they do
   today). Deprecated in docs, not removed.

**Cost, measured:** ~31 external call sites in 15 files (§1.6) — the
Owner-routed tier is a ~1-day mechanical change plus the five filter-site
simplifications. The real cost is in-package: the 230-test oracle set calls
the package-level functions ~900 times; those stay compiling unchanged
(wrappers remain), while new Owner-method tests are added incrementally. Note
this API tier is **independent of the global migration**: it can land without
moving a single global, and it — not the migration — is what actually fixes
the SDK multi-App isolation story.

### 2.5 Complication 3 — INV-15's fence must not move: verified

The uncertainty fence is a `ConfigStore` field: `mcpUncertainty
mcpUncertaintyState` (internal/config/store.go:88), with raise/report/clear in
internal/config/mcp_uncertainty.go (raised at `MarkMCPUncertain`, cleared only
by the version-checked reload at store_reload.go:356/587). The owner side only
carries tokens (`captureUncertainty` 1965-1976,
`reloadWithUncertaintyToken` 1984-2004) and re-checks through
`cfg.MCPUncertaintyVersion*`. Design rule for stage 3: **`Owner` gains no
uncertainty field of any name**; every fence read/write keeps going through
the `cfg` pointer carried by admissions. INV-15a's "survives rollover" then
holds before, during, and after the migration, and
`TestMaybeCommittedFenceSurvivesOwnerRollover` /
`TestWrongStoreReloadDoesNotClearMCPFence` stay green by construction.

---

## Part 3 — Per-invariant disposition

Stage 3's checklist. "Structure" = the guard becomes unreachable and the
named lines are deletable. "Mechanism" = the mechanism stays, scoped to one
Owner. Line numbers are pre-migration.

| INV | Disposition | Guard deletable / mechanism that stays |
|---|---|---|
| INV-01 | Mechanism | `sessionContext` promote handoff (5703-5840), renewal path (2296-2386); owner-neutral, unchanged |
| INV-02a | **Structure** (last step) | Fences at 2398-2401 and 2481 deletable once the full reset footprint (2482-2488: `sessions`, `states`, `stateOwners`, `broker`, `leases`, `all*`, `initDone`) is owner-relative; `owner == o` legs elsewhere (1948 + call sites) degenerate to the closing fence and stay |
| INV-02b | Mechanism | `canReclaimLocked` (1662-1678), `rememberConfig` store binding (2206-2219), reclaim protocol (1680-1730); reads become `o.`-fields, ordering unchanged |
| INV-03a | **Structure** (after `stateOwners`+`sessions`) | Owner+generation legs of validity (730-731, 1235, 1299-1301, 1337-1339, 3916, 4424) tautological; the whole generation mechanism (~24 lines, §1.2) deletable; epoch legs stay |
| INV-03b | Mechanism | Bump-only-after-durable-write (1392-1398), `invalidateServerLocked`/`cancelServerCandidates` (1617-1640) on `o.serverEpochs` |
| INV-04a | Mechanism | `finishClose` joins (2430-2492), `trackSessionLocked` (830-853), closer/fallback workers (251-364, 898-1021) |
| INV-04b | **Structure** (same step as 02a) | The `if owner == o` fence (2481) and its rationale comment (2409-2415) deletable under the same condition as 02a |
| INV-05 | Mechanism | `lifecycleMu` inside detach (4689-4707); map becomes `o.committedAdmissions`, discipline identical |
| INV-06 | Mechanism | Lease-before-`lifecycleMu` order, no-I/O-under-lock, cancel-outside (4687-4696, 6077-6079; `lockContext` 612-648); `lifecycleMu` stays package-level |
| INV-07 | Mechanism | Refcounted lease registry (402-546, 665-707) becomes `o.leases`; ABA/retain logic unchanged |
| INV-08 | Mechanism | `publishPreparedClientLocked` contract (3172-3259) and publication paths; broker refs become `o.broker` |
| INV-09 | Mechanism | Transactional replace flow (3715-4072) |
| INV-10 | Mechanism | `bindGuardLocked` / `replacementNamesValidLocked` (1216-1276) |
| INV-11 | Mechanism | `addTransaction` (1023-1107), rollback guards (4381-4448) |
| INV-12 | Mechanism | Persist-first branches (3307-3438, 4490-4638); store out of scope |
| INV-13 | Mechanism | Conditional enable rollback (3461-3648) |
| INV-14 | Mechanism | `commitOutcomeNeedsRuntimeFence` (4840-4846), `fenceMCPRuntimeLocked` (4671-4685) acting through the store |
| INV-15a | **Structure — pre-existing** | Nothing deletable; hard rule §2.5: fence stays on the ConfigStore |
| INV-15b | Mechanism | Version-check clear (store_reload.go:356/587) + owner-side token capture (1965-2004) |
| INV-16 | Mechanism | `deferUntilCommit`/candidate token (1093-1107, 6013-6043), `activateRefreshesLocked` (1831-1871) |
| INV-17 | Mechanism | Refresh queue (1738-1945) becomes `o.refresh*`; coalescing/dirty-rerun unchanged |
| INV-18 | Mechanism | State-token ownership (5497-5610); `stateOwners` becomes `o.stateOwners` |
| INV-19 | Mechanism | Operation leases (163-250), renewal single-flight (569-611), follower wait (5019-5356) |
| INV-20 | Mechanism | `headerRoundTripper`/`ownerResponseBody`/per-transport pool (6234-6350); owner-neutral |
| INV-21 | Mechanism | Stdio process-group kill + bounded diagnostics (6356-6478, process_*.go); owner-neutral |
| INV-22a | Mechanism | Lazy barrier (2221-2284), close-before-drain (2488) on `o.initDone` |
| INV-22b | **Structure** (cheapest — one small step) | Package mirror dies: decl 375, writes 1725/2244/2486; `WaitForInit` (2780-2790) reads `currentOwner().initDone` |
| INV-23 | Mechanism | `resolveMCPMutationScope` (4073-4117); unchanged |
| INV-24 | Mechanism | `mcpConnectionConfigEqual` (1362-1366) + definition equality in `committedValidLocked` |
| INV-25 | Mechanism | `withMCPAdmissionFinalTurn` (2058-2095), bounded retry (2887-2969) |

Tally: 5 structural (of which 15a pre-existing; 22b cheap; 03a medium;
02a/04b only at the very end), 25 mechanical, 0 speculative.

---

## Part 4 — Migration order, revised

### 4.1 S-payoff per global

| Plan step | Global (refs prod+test) | S-law unlocked | Verdict |
|---|---|---|---|
| 3.3 | `initDone` (19) | **INV-22b immediately** | Move first — highest S-per-line |
| 3.1 | `stateOwners` (7) | prerequisite of 03a | Keep early |
| 3.7 | `sessions` (126) | **completes INV-03a**; unlocks the generation deletion | Keep, second half of the payoff |
| 3.5 | `generation` (33) | — | **Not a move — a deletion**, executed inside 3.7 |
| 3.4 | `states` (52) | none (only 02a/04b's reset) | Defer |
| 3.6 | `leases` (66) | none (only 02a/04b's reset) | Defer |
| — | `allTools`/`allPrompts`/`allResources` (60) | none alone; required for 02a/04b | Defer; move as one trio step |
| 3.2 | `broker` (15) | **none** (registry: "no S-conversion" — broker is already shutdown/recreated at reset, 2514-2515) | Defer — plan's early position has no payoff |
| 3.8 | `lifecycleMu`+`owner` | enables deleting 2398-2401/2481 (02a+04b) once everything else is a field | Last, as in the plan |

### 4.2 Revised order (option a)

1. **`initDone`** → `Owner` (22b). Revert-check oracles:
   `TestInitializeBarrierIsGenerationScopedAcrossRollover`,
   `TestWaitForInitIsImmediateWithoutFullInitialize`.
2. **`stateOwners`** → `Owner` (warm-up, 7 refs).
3. **`sessions`** → `Owner` + **delete the generation mechanism** (03a).
   Revert-check: `TestEnableServerFromOldOwnerCannotPublishIntoNewOwner`,
   `TestRetiredClientCannotPublishAfterReplacement`. Watch the one design
   wrinkle: `adoptSessionForRetirement` (1009-1021) adopts ownerless sessions
   into the *current* owner; retarget adoption at `session.owner` (the field
   exists; `queueCloseWithMode` 353-364 already routes by it).
4. **`states`** → `Owner`.
5. **`leases`** → `Owner`.
6. **`allTools`/`allPrompts`/`allResources`** → `Owner` (one step).
7. **`broker`** → `Owner`.
8. **`lifecycleMu` + `owner` last**: delete 2398-2401 and 2481 (02a+04b);
   `lifecycleMu` itself stays package-level (§2.1).

Each step keeps the plan's §3 procedure unchanged (build, full `-race` suite,
delete the S-guard the step made dead, revert-check, commit with INV + lines).

### 4.3 Minimal subset for option (b) — named

**Move exactly three globals: `initDone`, `stateOwners`, `sessions` — and
delete the generation mechanism inside the third step.** That is the whole of
option (b):

- Makes **INV-22b** and **INV-03a** unbreakable by construction (2 of the 5
  S-laws; INV-15a is already structural for free).
- Leaves INV-02a/04b guards in place — correct, not lazy: while `broker`,
  `leases`, `states`, `all*` remain package-level, `resetRegistryLocked`
  still mutates shared state and the `if owner == o` fence must stay with it.
- **Cost**: ~45 prod references (sessions 31, stateOwners 7, initDone 7) and
  ~110 test references (sessions 95 — mostly direct `sessions.Set` test
  setup, a mechanical rewrite to the acquired owner's field; initDone 12).
  3 commits, each with a full `-race` suite run (~83 s baseline) plus
  revert-checks; **1–1.5 days** including test rewrites.
- **Result**: 6478 → ≈ **6450** (−28 lines, −0.4 %).
- Risks: (i) the adoption wrinkle above; (ii) `canReclaimLocked` keeps
  reading global `states`/`all*` while `sessions` is owner-relative — mixed
  reads are correct (the map sets are disjoint by construction) but must be
  called out in the commit; (iii) seam-coupled oracles that poke `sessions`
  directly get rewritten per the plan's rule — rewritten, never dropped, and
  each rewrite logged with its INV.

---

## Part 5 — What NOT to change

1. **Exported signatures** of all 27 package-level functions and the 2
   `Owner` methods. New Owner-receiver forms (§2.4) are additions; nothing
   existing changes shape or semantics.
2. **Single-owner behavior.** Every `ErrOwnerBusy` /
   `ErrMCPConfigStoreBusy` / skip-branch that exists today still fires in the
   same situations. After the migration these branches guard the closing
   fence and `initWG` reuse safety (§1.2), not owner identity — but from the
   outside nothing observable changes. Deleting them would be a functional
   change and is out of stage 3's scope.
3. **Broker event semantics**: event types, publish-before-lease-unlock
   ordering (INV-08), subscribe/close behavior, `SubscribeEvents` shape.
4. **INV-15's fence**: stays on the ConfigStore, version-check clear
   discipline untouched (§2.5).
5. **The oracle set**: every one of the 25 invariants keeps at least one
   live, non-vacuous test (the plan's post-1.2 rule). Seam-coupled tests are
   rewritten mechanically when a seam moves; a dropped or renamed-without-
   replacement test is a stop signal.
6. **Transport behavior** (INV-20/21): HTTP cancellation fencing, stdio
   process-group kill, bounded diagnostics — owner-neutral, untouched.
7. **Error sentinels** (`ErrOwnerBusy`, `ErrMCPConfigStoreBusy`,
   `ErrMCPConfigUncertain`, `ErrStdioDiagnosticTooLarge`): public contract.

---

## Recommendation

**Stop at stage 1 (option c).** The numbers:

- The line-budget justification is dead. A full migration deletes ~34 lines
  (0.5 %) for 3–5 days of work and ~240 test-site rewrites; the <2000 target
  is off by 3.2×. An honest "6478 → ≈6445" kills the migration as a cleanup.
- The defect-class justification is narrow. The S-class produced one realized
  P1 (R16-5) and one P2 (CR-3), both closed with standing oracles; both
  realized P0s are M-class and stay M-class under any ownership model. There
  is no open P0/P1 of the S-class today. The registry plus 230 oracles now
  hold this line at review cost — and review cost is what plan rule 0.3 was
  written to cap.
- The one genuine structural gain left on the table — the SDK multi-App
  isolation story — is an **API** problem (§2.4), not a state-placement
  problem: the Owner-routed API fixes it in ~1 day at ~31 call sites and can
  land without migrating a single global.

**If** the operator wants structural hardening anyway, option (b) —
`initDone` + `stateOwners` + `sessions`, delete generation — is the only
bounded version (1–1.5 days, 2 of 5 S-laws made unbreakable, −28 lines, zero
behavior change). It is defensible, but so is not spending the day: what it
removes is guard code that is currently correct, tested, and documented.

**Do not do option (a).** Five more days buys +6 deleted lines over (b), the
deletion of two 5-line fences, and a rewrite touching ~240 test sites — for
laws whose realized severity never exceeded P1 and are currently closed.

Recommendation order: **(c) stop, with (b) held as the fallback if a driver
appears; (a) rejected on the numbers.**
