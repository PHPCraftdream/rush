# Coordinator port C1 — access-path audit, lane sequence, budgets, serial selectors, compiler probe (2026-11-24)

Status: discovery document only. One compile probe was performed and fully
reverted (see §5 and §6); no Go file, test or mapping change survives this pass.
All file:line citations were re-measured in the live tree on 2026-11-24; the
scout's `RESOLVED` labels were not trusted — every site was re-read.

## Reliability of this document (read first)

Three review rounds on the scouting series ended without a clean pass; this is
the third document. What a later lane may rely on, and what it must redo:

- Reliable: `2026-11-24-agent-root-map.tsv` lists exactly the 192 production
  files of the live root (set equality with the tracked root listing was
  re-checked by the orchestrator); stage 1 (delete the dead alias file
  `batch_depth_payload.go`, requalify its single test-only use) is
  compile-proven; the probe finding that `cache_diagnostics.go` cannot be moved
  as a plain file (its methods belong to the root-owned `turnStream`, error
  `undefined: turnStream`).
- Wrong or unproven: (1) the "12 open, 2 conditionally resolved" classification
  does not establish a legal cut: `(*sessionAgent).finalizationWiring` still
  reads `a.coord.finalization` (`finalization_wiring.go:302-306`, called from
  `runTurn` at `agent_turn.go:58,763`); `currentAgent` has more than two
  production reads (`models_component.go:54`,
  `finalization_queue_reservation.go:181`); the option of returning a
  toolwrap-owned interface from the agentcall port would create a reverse
  import, because toolwrap already imports agentcall
  (`runtime/policy/toolwrap/driver_registry.go:6`); (2) the only compiler
  evidence is the alias deletion and a one-file copy, not a complete first-stage
  cluster move; (3) the budgets and counts: stage 4 starts from a stale 195
  after stage 1 gives 197, stage 5 counts all 13 batchrun rows without the
  alias already removed, `cache_diagnostics.go` is marked a whole-file move in
  the TSV while the audit documents a required split, the drainctl inputs (4,291
  lines) do not fit the stated four-file budget (3,600), serial tests are given
  for only some stages, and the final 6 is a target, not an established route.
  Every stage from 2 on is a discovery question, not a schedule.

## 0. Re-measured baseline

- Guard-accurate root count today (`git ls-files`-tracked, non-test `.go`
  directly in `internal/agent`): **192 files + 6 code dirs = 198**, confirming
  the scout's 198 (192+6); verified by
  `git ls-files 'internal/agent/*.go' | grep -v '_test\.go$' | grep -c '^internal/agent/[^/]*$'` → 192
  (checked at the base commit, mid-session, and after the probe revert). The
  root-map TSV (`docs/plans/2026-11-24-agent-root-map.tsv`) has 192 rows
  (192 files / 36,548 lines, `already landed=1`) and matches the live 192
  root production files exactly (diff clean, no overlap, checked). An
  earlier 190 reading was a mid-probe artifact (the probe had deleted
  `internal/agent/batch_depth_payload.go` and one further file was at that
  moment also unaccounted) and must not be reported as the live count.
- Live receiver census (production files containing `func (c *coordinator)` /
  `func (a *sessionAgent)`): coord **70 files / 12,209 lines**; agentrun
  **26 files / 7,942 lines**. The scout's 63/11,774 and 25/7,942 are already
  stale on the coord side.
- `runtime/coord` and `runtime/exec/agentrun` do **not** exist today
  (measured 0). `runtime/` has 6 direct code dirs; `runtime/exec/` has 6.
  Creating the two destinations therefore costs the root nothing and lands
  each parent exactly at its own limit of 7.

## 1. 14-member access-path audit

Owner rule applied throughout (dm0b §3.1): the C1 port lives in
`runtime/contract/agentcall` (file `agentcall/agent_coord_port.go`, not yet
created — verified absent), is **implemented by coord**, **consumed by
agentrun**; direction is `agentrun → agentcall` and `coord → agentcall`,
never upward. Seven fields become accessor methods with one-`return` bodies.
A type moves with ALL its methods or its methods become functions; a
forwarding method or alias does not remove a type dependency; no methods on a
type from another package, also not through an alias.

Declarations (all re-read live): `coordinator` struct `coordinator.go:69`
(fields `cfg` :70, `background` :78, `driverHints` :110, `subAgentDrivers`
:130, `finalization` :166, `currentAgent` :168); `sessionAgent.coord`
`agent.go:232` (struct starts :222); `SessionAgentOptions.Coord`
`agent_options.go:30`.

| # | member | live access paths through `*coordinator` (file:line, re-read) | port owner after move | smallest downward-only change | status |
|---|---|---|---|---|---|
| 1 | cfg (field) | `agent_turn.go:186` (`a.coord.cfg`); `agent_codex_recovery.go:83,86,99` (`a.coord.cfg.Snapshot()`); batch-side `batch_cli.go:66,257` (`b.coord.cfg`) stays intra-coord | accessor `Cfg() *config.ConfigStore` on the agentcall port, implemented in coord | retype `sessionAgent.coord` and `SessionAgentOptions.Coord` to the port; `a.coord.cfg` → `a.coord.Cfg()` | OPEN — concrete field edge live at both agentrun consumers |
| 2 | arb (field) | `agent_turn_step.go:448,472,493` (`ts.a.coord.arb.chainLastLaunch`); drain-side `drain_component.go:424` reaches the arbiter via `a.coord.arbiterVerdict` (`coordinator_drain_policy.go:10`) — method, not field | accessor `Arb()` on the port (arbiter type moves with all its methods first; accessor may expose only the port-facing subset, e.g. `ChainLastLaunch(...)`) | move arbiter type+methods to its lane (DM2), then expose the used calls through the port; nil-guard at `agent_turn_step.go:464` must be kept | OPEN — concrete field edge live at 3 sites |
| 3 | currentAgent (field) | `finalization_wiring.go:238,241` (`w.coord.currentAgent`, incl. `.(*sessionAgent)` assertion) | the field stays on coord; the port must not expose it. The consuming declaration (`finalization_wiring.go`) is R5/coord-owned per dm0b §3.3, so after R5 the edge is intra-coord | confirm no agentrun-owned declaration names `currentAgent` after R5; no port method | RESOLVED (conditional) — every live access is coord-owned once R5 re-homes `finalization_wiring.go`; no agentrun→coord path remains. Evidence: rg over production files finds the only two reads at `finalization_wiring.go:238,241` |
| 4 | subAgentDrivers (field) | `finalization_driver_binding.go:27` (`a.coord.subAgentDrivers.BindFinalizationIdentity(a, call)`) — agentrun-owned method on the concrete field | accessor `SubAgentDrivers()` on the port returning the toolwrap registry interface, or fold `BindFinalizationIdentity` into a port method | rettype field to port accessor; the registry itself already lives in toolwrap (dm7e) | OPEN — scout's RESOLVED covers only the registry's home, not this field read; the access path still goes through concrete `*coordinator` |
| 5 | finalization (field) | `finalization_wiring.go:306` (`return a.coord.finalization`, method `finalizationWiring()` :317); all other readers are coord methods | no port method — the consumer (`finalization_wiring.go`) is R5/coord-owned | same as #3: intra-coord after R5 | RESOLVED (conditional) — no agentrun-owned reader found live |
| 6 | background (field) | `agent_own_work_batch.go:55` (`coord.background.ActiveOwned/PendingCompletionsOwned`) — the function's `coord` parameter is `*coordinator` (`agent_own_work_batch.go`, `ownWorkBatch` reads it at :55) | accessor `BackgroundOwnsWork(sessionID) bool` on the port (or `Background()` returning a narrow interface) | make `ownWorkBatch` use the port accessor; the shellbg manager type stays with coord | OPEN — concrete field edge live; the scout's RESOLVED (component extraction) does not touch the agentrun-side read |
| 7 | driverHints (field) | `agent_turn_step.go:704` (`ts.a.coord.driverHints.isExternalDriver`); `drain_component.go:195` (`a.coord.driverHints.hintSeqOf`) — the second site is in the drain component adapter, resolved when DM2 moves it | accessor `IsExternalDriver(sessionID) bool` (and `HintSeqOf` if the drain adapter keeps it) on the port | expose the two used calls; `batchDriverHints` type stays with coord and moves with all its methods | OPEN — concrete field edge live at :704 |
| 8 | buildModelsFromCfg (method) | decl `coordinator_models_build.go:33` (1-line forward to `modelsComponent`); agentrun calls `agent_codex_recovery.go:127,148` (`a.coord.buildModelsFromCfg`) | port method `BuildModelsFromCfg(...) (agentcall.Model, agentcall.Model, error)` (modelkit-owned signature) | port method; the coord body forwards to the modelkit component, which is legal downward (coord→modelkit) | OPEN — method is reachable only through concrete `*coordinator`; the modelkit extraction moved the *body*, not the access path |
| 9 | batchActiveAfterQuestionPause (method) | decl `cli_active_work.go:115` (forward to `cliActiveWorkComponent` :119); agentrun call `agent_own_work_batch.go:45` (`coord.batchActiveAfterQuestionPause`) | port method `BatchActiveAfterQuestionPause(ctx, facts, root) (bool, error)` | port method implemented by coord, forwarding to the component | OPEN — concrete-receiver edge live |
| 10 | buildCredentialModel (method) | decl `credentials.go:387`; agentrun call `agent_codex_recovery.go:108` (`a.coord.buildCredentialModel`) | port method `BuildCredentialModel(...)` | port method | OPEN — concrete-receiver edge live (scout agrees) |
| 11 | drainCredentialBoundary (method) | decl `coordinator_agent_control.go:245`; agentrun call `agent_codex_recovery.go:133` (`a.coord.drainCredentialBoundary`); other callers (`agent_drain.go:66`, `delegation_role_calls.go:86,122`, `delegation_role_models.go:83,129`) are coord-owned | port method `DrainCredentialBoundary(ctx, sessionID) bool` | port method | OPEN — concrete-receiver edge live (scout agrees) |
| 12 | batchCutoverFacts | free function `coordinatorBatchCutoverFacts(c *coordinator)` at `batch_cutover_facts.go:135` (R4 target); agentrun callers `agent_own_work_batch.go:37` (`facts := coordinatorBatchCutoverFacts(coord)`) and `agent_turn.go:226` (`coordinatorBatchCutoverFacts(a.coord)`) — both still pass the concrete pointer | after R4 the function becomes a coord method and the port exposes `BatchCutoverFacts() agentcall.BatchCutoverFacts` | R4 re-home first, then port method; the two free-function call sites rewrite to the port | OPEN — scout's RESOLVED names the L6b move of the *function*, but both agentrun call sites still route the concrete `*coordinator` through it; the access path survives until the port replaces it |
| 13 | recoverDeadScopeCandidates | free function `recoverBatchScopeDeadCandidates(ctx, c *coordinator, owner)` at `batch_scope_recovery.go:100` (R2 target); agentrun caller `agent_turn.go:205` (`recoverBatchScopeDeadCandidates(preambleCtx, a.coord, call.SessionID)`) — still sends `a.coord` | after R2 the coord method exists; port exposes `RecoverDeadScopeCandidates(ctx, owner)` | R2 re-home, then port method; `agent_turn.go:205` rewrites to `a.coord.RecoverDeadScopeCandidates(...)` (port-typed receiver) | OPEN — scout's RESOLVED covers the move of the function, not the caller's concrete-pointer argument |
| 14 | batchFactsInstalled (method) | decl `cli_active_work.go:289`; agentrun callers `agent_own_work_batch.go:28` (`coord.batchFactsInstalled()`) and `agent_turn_step.go:340` (`ts.a.coord.batchFactsInstalled()`) | port method `BatchFactsInstalled() bool` | port method | OPEN — concrete-receiver edges live at both sites (scout's RESOLVED refers to the L6a cli-active extraction only) |

### Score (independent re-audit)

- **RESOLVED: 2** — `currentAgent`, `finalization` — and only *conditionally*:
  both resolutions rest on R5 (`finalization_wiring.go`, whose coord field is
  `finalization_wiring.go:39`) moving to coord; before R5 they are OPEN.
- **OPEN: 12** — cfg, arb, subAgentDrivers, background, driverHints,
  buildModelsFromCfg, batchActiveAfterQuestionPause, buildCredentialModel,
  drainCredentialBoundary, batchCutoverFacts, recoverDeadScopeCandidates,
  batchFactsInstalled. The scout's "OPEN 4 / RESOLVED 10" is not reproducible:
  six members it marked RESOLVED (subAgentDrivers, background, driverHints,
  buildModelsFromCfg, batchCutoverFacts, recoverDeadScopeCandidates) still
  have live agentrun→`*coordinator` access paths, and
  batchActiveAfterQuestionPause + batchFactsInstalled (also marked RESOLVED)
  have direct concrete-receiver call sites. The earlier lane passes moved
  bodies/components, which is necessary but does not by itself remove the
  access path.
- **C3 ShuttingDown (noted separately): RESOLVED** — contract method
  `agentcall/session_call.go:349`; implementation `agent_control.go:155`.
  Verified live; the concrete type assertion of dm0b §3.2 is gone from
  `finalization_queue_reservation.go`.

Ordered change list for the 12 OPEN members (one lane, dm0b L6): (1) land
R1–R5 re-homings (§3.3); (2) create `agentcall/agent_coord_port.go` with the
14 names (7 accessors + 7 methods, signatures owned by modelkit/toolwrap/
batchflow types that have already moved down); (3) retype
`sessionAgent.coord` (`agent.go:232`) and `SessionAgentOptions.Coord`
(`agent_options.go:30`) to the port; (4) rewrite the call sites cited above;
(5) delete the root aliases in `contract_bridge.go` only after DM8 importers
are requalified. No method may be declared on `*coordinator` outside coord;
no port method may accept or return a coord-concrete type.

## 2. Ordered lane sequence (root count 198 → 6)

Backbone: dm0b §5 L1–L7 (most evidence-backed), reconciled with sprint0 §5
DM1–DM9 and the scout's corrections: DM7/C1a/C1b come **after** DM1–DM6 and
DM-W (their file sets depend on them), so the scout's 198−19=179 and
179−10=169 are **not** valid as first stages. Root arithmetic rules: only the
immediate parent is counted; every shim file is its own +1; a new directory
under an already-counted parent costs the root nothing. The start count is
today's live 198 (192 files + 6 dirs); TSV row counts are per destination and
include split rows, so each stage's net is only arithmetic-valid if the
whole-file rows leave whole with zero shims — stated per stage.

| # | stage (dm0b lane) | scope | net arithmetic | root count after | prerequisites |
|---|---|---|---|---|---|
| 0 | start | — | — | **198** (192 files + 6 dirs; verified) | — |
| 1 | L1 leaf tails, provable remainder | delete dead alias file `batch_depth_payload.go` (only consumer is `coordinator_test.go:241`, probe-proven §5, command set B) | −1 file, +0 shims | **197** | none. Compile-proven |
| 2 | L1 remainder | `cache_profile.go` facade (dead: `provauth.PromptCacheTTL` already exists at `provauth/z_cache_policy.go:116`), `cache_diagnostics.go` (turnStream methods — probe-proven NOT plain-moveable, §5 command set A), `oauth_token_source.go` (names `*coordinator`, dm0b §4.2), sessionprompt blockers (2 root decls, §4.2), modelkit blocker (1) | **UNPROVEN: needs a discovery lane** — open question: exact live removal/shim set for each of the five files | UNPROVEN | stage 1 |
| 3 | L2 mailbox remainder | sessionAgent-bound half of `queued_call_scope.go` (dm0b §5.2 records the landed half) | **UNPROVEN: needs a discovery lane** — open question: declaration-level partition of `queued_call_scope.go` (156 lines) | UNPROVEN | stage 2 |
| 4 | DM5a batchinst | 26 TSV rows (2968 lines); plan-out 7 files (batch_install_adapters.go must split ≤900) | 195−26=171 **only if** all 26 rows leave whole with zero shims — 38 TSV rows are `split`, so **UNPROVEN: needs a discovery lane** — open question: which files split and what shim/remainder each leaves | UNPROVEN | stages 1–3 (batch lanes take already-retargeted declarations) |
| 5 | DM5b batchrun | 13 rows (1688 lines) | UNPROVEN (same rule) | UNPROVEN | stage 4 |
| 6 | DM5c batchflow | 15 rows (1745 lines) | UNPROVEN (same rule) | UNPROVEN | stage 5 |
| 7 | DM6 finalize/delegation/subagents | 12+7+6 = 25 rows (1591+472+1384) | UNPROVEN (same rule) | UNPROVEN | stage 6 |
| 8 | DM2 turnstep+drainctl | 9+13 = 22 rows (1956+1911); includes the four drain methods at `drain_component.go:189,203,357,371` | UNPROVEN (same rule) | UNPROVEN | stages 1–3 |
| 9 | DM3 usage/promptcache/callbeat/provauth/clibridge | 4+2+1+4+2 = 13 rows (430+220+613+896+323) | UNPROVEN (same rule) | UNPROVEN | stages 1–2 |
| 10 | DM4 toolwrap/agenticfetch | 3+2 = 5 rows (235+375) | UNPROVEN (same rule) | UNPROVEN | stages 1–9 |
| 11 | DM-W modelkit+wakectl | 21+5 = 26 rows (4258+211); wakectl deps already moved (§4.2) | UNPROVEN (same rule) | UNPROVEN | stages 1–10 |
| 12 | DM1 remainder mailbox/watchdog/sessionprompt/responsesws | 9+3+3+1 = 16 rows (2741+854+304+65) | UNPROVEN (same rule) | UNPROVEN | stages 1–11 |
| 13 | **C1a coord** (dm0b L6, sprint0 DM7a) | R1–R5 + 14-name port + 20 planned coord files (TSV 6026 lines) + glue | 198−19=179 is **NOT established** here: the historical 19 is a package input, the TSV says 20 root rows, and the live coord receiver superset is 70 files; exact post-lane removals are **UNPROVEN: needs a discovery lane** — open question: which coord-owned declaration ranges remain after stages 1–12, and do zero shims hold | UNPROVEN; `runtime` 6→7 (root unaffected) | stages 1–12 (all upstream lanes) |
| 14 | **C1b agentrun** (DM7b) | 10 planned files (TSV 4952 lines) + sessionAgent methods from drain_attempt/credentials/queued_call_scope/usage_record/peak_hours gate/turn_arbiter | 179−10=169 is **NOT established** for the same reason; live superset 26 files; **UNPROVEN: needs a discovery lane** — open question: post-lane sessionAgent method remainder | UNPROVEN; `runtime/exec` 6→7 (root unaffected) | stage 13 (C1a) |
| 15 | DM7 consolidation | concat into ≤7 files ≤900 lines per destination | 0 (in-destination) | unchanged | stage 14 |
| 16 | DM8 importers + DM9 config | `app`/`serve`/`sdk`/`cmd` requalification; ratchet seeds; CI config | 0 root source removals | **6** = 0 root files + 6 code dirs (`agentresult`, `prompt`, `provider`, `runtime`, `safety`, `tools`) — target identity holds only if stages 4–14 land with the exact counts | stage 15 |

Validation identity: the TSV's 192 rows equal the live root production
files (192); the map is a complete non-overlapping row-level partition of
the live root. **Required acceptance (scout §arithmetic) stands: 192 moved
→ 0 files + 6 dirs = 6; any shim invalidates it unless separately
accounted.**

## 3. Line budgets of consolidated destinations

Budget: 7 files × 900 lines = **6,300** per destination. Two columns: TSV
destination totals (root-origin lines mapped by the root map) and live
`wc -l` of each destination package today (production files only).

| destination | TSV rows / lines | live today (files / lines) | plan-out files | verdict at ≤900/file |
|---|---|---|---|---|
| runtime/coord | 20 / 6,026 | — (dir absent) | 7 | **OVER once glue lands**: live coord receiver census is 70 files / 12,209 lines; TSV root-origin is 6,026 which alone fits 6,300 with ~861/file average, but coord additionally absorbs glue receivers and the census shows ~12.2k before upstream strips. After stages 1–12 the remainder must be re-measured; if >6,300 → dm0b Q1 option (c) declaration reassignment, then (a) ratchet exception |
| runtime/exec/agentrun | 10 / 4,952 | — | 7 | **CONDITIONAL**: TSV 4,952 fits; live sessionAgent census 26 files / 7,942 lines does not. Fits only after DM-W/DM1–DM6 strip their declarations; re-measure before consolidation |
| runtime/batch/batchinst | 26 / 2,968 | 4 / 879 | 7 | fits: 2,968/7 ≈ 424/file (watch `batch_install_adapters.go` 958-line split) |
| runtime/modelkit | 21 / 4,258 | 7 / 1,353 | 6 | fits: 4,258/6 ≈ 710/file |
| runtime/batch/batchrun | 13 / 1,688 | — | 5 | fits (338/file) |
| runtime/batch/batchflow | 15 / 1,745 | 7 / 884 | 5 | fits (349/file) |
| runtime/batch/finalize | 12 / 1,591 | — | 3 | fits (530/file) |
| runtime/batch/delegation | 7 / 472 | — | 7 | fits (67/file) |
| runtime/batch/subagents | 6 / 1,384 | — | 6 | fits (231/file) |
| runtime/exec/drainctl | 13 / 1,911 | 5 / 2,380 | 4 | live already 2,380: 2,380/4 = 595/file fits; incoming 1,911 must merge with existing — re-measure at lane start |
| runtime/exec/mailbox | 9 / 2,741 | 3 / 252 | 4 | fits (685/file after merge) |
| runtime/exec/turnstep | 9 / 1,956 | 5 / 1,499 | 4 | fits (489/file) |
| runtime/exec/watchdog | 3 / 854 | 5 / 812 | 5 | fits |
| runtime/exec/sessionprompt | 3 / 304 | 4 / 718 | 5 | fits |
| runtime/exec/responsesws | 1 / 65 | 3 / 721 | 2 | fits |
| runtime/policy/usage | 4 / 430 | 5 / 1,054 | 7 | fits (live 151/file avg) |
| runtime/policy/promptcache | 2 / 220 | 3 / 167 | 3 | fits |
| runtime/policy/callbeat | 1 / 613 | 3 / 419 | 3 | fits |
| runtime/policy/provauth | 4 / 896 | 7 / 1,098 | 7 | fits (live 157/file avg) |
| runtime/policy/clibridge | 2 / 323 | 1 / 73 | 2 | fits |
| runtime/policy/toolwrap | 3 / 235 | 7 / 2,054 | 7 | fits (live 293/file avg) |
| runtime/policy/agenticfetch | 2 / 375 | 1 / 19 | 2 | fits |
| runtime/wakectl | 5 / 211 | 7 / 684 | 5 | fits (137/file) |
| runtime/contract/agentcall | 1 / 330 | 7 / 2,692 | 7 | fits (385/file) + the future `agent_coord_port.go` |

Summary: every destination except **coord** (and conditionally **agentrun**)
stays under 900 lines/file at the planned output counts. coord needs the Q1
path ((c) reassignment into batchinst/finalize/modelkit, then (a) a
ratcheted, shrinking-only exception if the remainder still exceeds 6,300).
agentrun must be re-measured after stages 1–12; no budget is forecast.

## 4. Serial tests per stage (commands NOT to be run now)

From the scout's "Serial verification selectors" and dm0b §6.3. Each stage's
owner runs these serially after the stage lands; nothing below is executed in
discovery.

- Stages 13–15 (C1a/C1b/consolidation):
  - `go test ./internal/agent -run '^(TestCoordinator|TestRun|TestInterrupt|TestCancel|TestRecheck|TestRunInternal)'`
    (coord; includes `TestCoordinator_InterruptAndSend_UsesInterruptAndReplace`, `interrupt_test.go:44`)
  - `go test ./internal/agent -run '^(TestRun|TestTurn|TestOwnership|TestDrain|TestInterruptAndSend)'`
    (agentrun)
- Batch stages (4–7): `TestBatch*` in the batch packages; finalize
  `TestFinalization*`; per-package selectors only after tests relocate.
- DM3 usage: `TestModelUnpriced|TestUpdateSessionUsage`; wake:
  `TestWake*`; toolwrap: `TestTool*`.
- The 62 two-core test files (dm0b §6.3/Q3) must be routed (split or itest)
  before stage 13; exact per-package regexes are
  **UNPROVEN: needs a discovery lane** — open question: which tests relocate
  with each package and which regex covers all its behavior tests.

## 5. Compiler probe

Protocol: complete move of stage 1, then exactly one package per build
command, `capm 2g go build ./<one-package>; echo exit=$?`, verbatim output,
immediate full revert. No test runs, no whole-module builds, no `.githooks`.

Stage 1 per §2 is "L1 leaf tails, provable remainder". During destination
selection two live facts changed the scout's/dm0b §4.1 premise and are
probe-verified here:

- **promptcache already owns colliding declarations**: the destination dir
  `internal/agent/runtime/policy/promptcache/` already contains
  `cache_profile.go` declaring `PromptCacheTTL(provider string, ttlFor func(string) time.Duration)`
  (`:36`) while the root facade `cache_profile.go:26` declares
  `PromptCacheTTL(provider string) time.Duration` — different signatures, so
  moving the root file into that package is a redeclaration, and per the task
  rule ("pick a destination that does not already own the declarations")
  promptcache is excluded as a probe target.
- **`cache_diagnostics.go` is no longer a zero-outbound plain move**: it
  declares methods on `turnStream` (`cache_diagnostics.go:34,156`), a type
  owned by the root (agentrun cluster).

### Command set A — probe of the promptcache leaf premise (expected to fail)

Move attempted: `cache_diagnostics.go` (192 lines) copied to
`runtime/policy/promptcache/cache_diagnostics_stream.go` with
`package agent` → `package promptcache` (its declarations —
`observedCacheReadTokens`, `cacheRequestObs`, `commonPrefixLen`,
`classifyCacheMiss`, `swapCacheObs`, `turnStream.logCacheStepUsage`,
`turnStream.noteCacheRequest` — are owned by nobody in that package).

```
capm 2g go build ./internal/agent/runtime/policy/promptcache; echo exit=$?
```

Verbatim compiler output:

```
exit=1


# github.com/PHPCraftdream/rush/internal/agent/runtime/policy/promptcache
internal\agent\runtime\policy\promptcache\cache_diagnostics_stream.go:34:11: undefined: turnStream
internal\agent\runtime\policy\promptcache\cache_diagnostics_stream.go:156:11: undefined: turnStream
```

Evidence: the two `turnStream` methods make the L1 promptcache tail a
declaration-level split, not a whole-file move. Stage 2's promptcache entry
stays **UNPROVEN: needs a discovery lane** with the concrete open question:
which package owns `turnStream.logCacheStepUsage`/`noteCacheRequest` after
C1b (they must move with `turnStream` or become functions over a port), and
does the remaining diagnostics core then move plain?

### Command set B — probe of stage 1 itself (dead alias removal)

`batch_depth_payload.go` (7 lines, `type batchExternalDepthPayload =
batchinst.ExternalDepthPayload`) has no production consumers — the only use
is `coordinator_test.go:241`. The complete move is: delete the root file,
rewrite the test line to `batchinst.ExternalDepthPayload` and add the
`batchinst` import to that test file.

```
capm 2g go build ./internal/agent/runtime/batch/batchinst; echo exit=$?
```
→ `exit=0` (no output)

```
capm 2g go build ./internal/agent; echo exit=$?
```
→ `exit=0` (no output)

Both destination package and importing package compile. The test-file edit is
not covered by `go build` (tests excluded); it was checked by reading, not
compiled — the stage's test-path leg therefore remains verified only by
inspection. Stage 1 is **compiler-proven for production packages**; its
status in §2 row 1 stands.

## 6. Probe hygiene

- Moved/edited during the probe: created
  `internal/agent/runtime/policy/promptcache/cache_diagnostics_stream.go`;
  deleted `internal/agent/batch_depth_payload.go`; edited
  `internal/agent/coordinator_test.go` (alias use + import). No other files
  touched; no empty directories created (promptcache already existed).
- Reverted: the probe file was deleted; `batch_depth_payload.go` was restored
  byte-identical and `coordinator_test.go` restored (both edits undone).
- Final `git status --porcelain`:
  ```
  ?? docs/plans/2026-11-24-agent-c1-audit.md
  ?? docs/plans/2026-11-24-agent-root-map.tsv
  ```
  Zero tracked modifications (`git diff` empty); the only two entries are
  this audit document and the root-map TSV. No probe file, deletion, or
  test edit survives (`batch_depth_payload.go` and `coordinator_test.go`
  restored byte-identical).
