# Sprint 0 agent DM0 — symbol-graph design report

> **FINAL AUTHORITATIVE LAST-PASS NOTICE (2026-11-17).** Supersedes every earlier current-state count, SCC, over-limit list, provisional lane order, and cycle status in this document. The preceding baseline/amended sections remain historical receipts only; do not use them to plan current work. Final receipt: `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-last-pass.json`, projected from the final overrides (last-pass block at lines 657+ of `docs/plans/2026-11-16-sprint0-agent-amended-overrides.tsv`). Summary: **109 edges, 1,696 distinct symbol witnesses, 4,638 uses, 75 seam uses**; **103 genuine inter-owner arcs plus 6 synthetic seam self-edges**; The final projection reports **zero receiver-colocation violations**, and the source receiver census has 285 records. Receiver retargets remain proposals; their exact row counts and source/target selections are in the receiver/execution manifest. For a destination whose existing receiver methods have moved away, a compile-only move still needs new receiving methods; zero existing `a` rows does not imply zero future receiver work. One SCC has 15 owners: `agentrun,batchassemble,batchcontract,batchflow,batchinst,callbeat,clibridge,coord,delegation,drainctl,finalize,mailbox,modelkit,toolwrap,turnstep`. Exactly 12 projected overlimits: `agentrun 46; batchassemble 9; batchcontract 16; batchflow 8; batchinst 65; coord 76; drainctl 12; finalize 9; mailbox 11; provauth 15; toolwrap 10; turnstep 9`. Six active projected owners `usage`, `promptcache`, `responsesws`, `sessionprompt`, `watchdog`, and `provauth` have no path into this SCC and are **all eligible to start now at their stable D1 package names/paths**. `provauth` is over-limit but stays at its stable path `internal/agent/runtime/policy/provauth`; use the named same-package consolidation candidate (15 projected files → 7 candidate files) in the split manifest. This is an unmeasured grouping only, not proof of ≤900 lines. `root` is only the `contract_bridge.go` compatibility shim, not a movable production lane. `callbeat` remains cyclic. `agenticfetch`, `subagents`, `batchrun`, and `wakectl` are empty/dissolved at this checkpoint; do not create phantom lanes at their old paths. This is a final report handoff, not further design iteration or a request to rebuild the graph.

**Status: baseline design report; no package movement performed by this report.** This task includes the recovered DM0 TSV deliverable and graph-tool corrections outside the repository; the TSV is an input here, not permission to edit the in-progress `../cmd` tool. No repository tests or commits were run.

## Decision summary

The concrete whole-file projection is **not acyclic**: 23 active mapped owners form one SCC. `agentcall` is the 24th mapped target but has zero active files in the scanned root and is not an SCC member; `root` is an unlisted shim sink outside the SCC. The graph has 214 directed owner edges overall, 2,162 distinct symbol witnesses and 5,016 cross-boundary uses. This is a current-build diagnostic, not a prediction of the post-EX or post-declaration-move graph. EX0 lanes and SA lanes change ownership and declarations, so this projection cannot show that those later seams remove any particular edge. The explicitly named EX lanes plausibly address several cycles, but design coverage is inferred, not proof; no planned lane claims complete coverage of the whole SCC, so the collective SCC remains a **DESIGN GAP** until every internal edge has an explicit disposition and the transformed graph is rebuilt. There are no planned I/J lanes.

Do not respond to `must_export` by exporting every private symbol or any mutable global. It is diagnostic demand only. Apply sprint-plan rule 18: export only where a compiler-proven external call site requires it, record the identifier and external call site, otherwise keep it private; prefer a narrow same-time parameter/provider port. Test fixtures belong only in `_test.go` files. In particular, no mutable global is authorized for export.

## Inputs, scope, and reproducibility

Read before drafting: `docs/plans/2026-11-15-sprint0-split-agent.md` §2.3, §3.5 and §5; `docs/plans/2026-11-15-sprint0-plan.md` rules 18–19; and `docs/plans/2026-11-16-sprint0-agent-layout.tsv`. Graph input: `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-initial.json`, generated for this worktree and TSV. It analyzes active non-test files of the current `internal/agent` root package only; test files and external importers are outside this graph. `build_excluded_non_test_files` is empty. This is not a source-level audit of every compilation target or caller.

Reproduce the final graph from the tool module with these commands (the executable and JSON output are outside the repository):

```sh
cd D:/dev/go/crush/.rush/tmp/sprint0/symgraph/cmd
bash D:/dev/go/crush/.rush/tmp/golock.sh go build -o D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe .
bash D:/dev/go/crush/.rush/tmp/golock.sh D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe -dir D:/dev/go/crush/worktrees/s0-dm0-20261116/internal/agent -layout D:/dev/go/crush/worktrees/s0-dm0-20261116/docs/plans/2026-11-16-sprint0-agent-layout.tsv -json > D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-final.json
```

Without `-json`, the tool emits human-readable text. A relative `-layout` path is resolved against `-dir`; the explicit absolute path above avoids ambiguity. `-root-owner` defaults to `root`; `-max-elements` defaults to 7. A successful exit does not mean the graph is acyclic or within limits: inspect JSON fields `acyclic`, `sccs`, `summary`, `packages`, and `receiver_colocation_violations`. Missing TSV rows produce warnings and are recorded, not fatal. The analysis uses `go/packages` type information (`TypesInfo.Uses`) to resolve object identities for selector references, fields, methods, promoted members, generic members, and interface selections; each selector occurrence is counted once, while local shadowing is not mistaken for a package declaration. Receiver-colocation constraints are reported separately from symbol-reference edges. The scanner covers active, build-selected non-test files of the target package, not test files or external importers.

The final JSON was independently confirmed equal to the baseline in edges and summary. It records 245 active non-test root files; 254 TSV rows (253 plan rows plus `batch_glue.go`); 244 present mapped files; 10 absent TSV rows; and one present unlisted file, `contract_bridge.go`. There is one SCC containing 23 active mapped owners and 193 intra-SCC directed edges; the other 21 edges touch the root shim owner and are outside that SCC. `agentcall` is the empty 24th target, not an SCC node. The SCC node list is exhaustive for this graph. The direct cycles below are representative evidence only, not a claim to enumerate all simple cycles.

## File inventory: physical now, TSV, and design target

“Physical now” in the table is the number of present, non-test files **still in the current root package** that the TSV assigns to each future owner; it is not the count of files already present in the future package directory. “TSV rows” is the source-map row count, including rows whose named file is now absent from root. “Planned final” is the D1 §2.2/§2.3 intended post-consolidation code-file count, not verified achievable. Values are files, not directory elements; tests are excluded. The current destination tree has five non-test Go files under `runtime/contract/agentcall` and none in the other 23 target package directories; those five are represented in the TSV as absent root rows, not duplicated in the root physical column.

| owner | Physical now | TSV rows | Planned final | Notes |
|---|---:|---:|---:|---|
| agentcall | 0 | 8 | 7 | Three listed files already physically relocated here; D1 merges errors + attempted-error inputs. |
| agenticfetch | 2 | 2 | 2 | |
| agentrun | 10 | 10 | 7 | Includes planned component extraction and consolidation. |
| batchflow | 20 | 20 | 5 | |
| batchinst | 30 | 30 | 7 | **Plan inconsistency:** §2.3 names eight output files (`admission`, `scope`, `ingress`, `install`, `rehome`, `policy`, `runtime`, `disk`) while promising 7. The reduction to seven is not evidenced by the listed grouping. |
| batchrun | 18 | 18 | 5 | |
| callbeat | 3 | 3 | 3 | |
| clibridge | 2 | 2 | 2 | |
| coord | 20 | 20 | 7 | Includes `batch_glue.go`, not in the 253 original-file coverage. |
| delegation | 7 | 7 | 7 | |
| drainctl | 17 | 17 | 4 | |
| finalize | 21 | 21 | 3 | |
| mailbox | 10 | 10 | 4 | |
| modelkit | 21 | 22 | 6 | `provider_retry.go` is absent from root after SA3; plan still counts it in source map. |
| promptcache | 3 | 3 | 3 | |
| provauth | 7 | 7 | 7 | |
| responsesws | 2 | 2 | 2 | |
| sessionprompt | 4 | 5 | 5 | `coordinator_session_credential_requirement.go` absent; do not infer complete absorption. |
| subagents | 6 | 6 | 6 | |
| toolwrap | 8 | 8 | 7 | |
| turnstep | 15 | 15 | 4 | |
| usage | 8 | 8 | 7 | D1 says 8 source files become 7, but §2.3 lists eight distinct input names and no explicit merge output for the pair; verify the concrete consolidation. |
| wakectl | 5 | 5 | 5 | |
| watchdog | 5 | 5 | 5 | |
| **root** | **1** | **0** | **0 intended** | `contract_bridge.go` is an observed compatibility/shim sink. It is not the intended future root owner; TSV says it stays for now and is deleted at DM8. |
| **Totals** | **245** | **254** | **120** | The 24 production targets sum to 120 planned files; adding agenttest’s 2–4 planned test-support files gives 122–124. This is an intended target, not a verified count. |

The 24 physical target counts sum to 244, with the one root shim giving 245. The 12 mapped owners over the seven-file limit in the physical-current root projection are `agentrun` (10), `batchflow` (20), `batchinst` (30), `batchrun` (18), `coord` (20), `drainctl` (17), `finalize` (21), `mailbox` (10), `modelkit` (21), `toolwrap` (8), `turnstep` (15), and `usage` (8). `agentcall` has zero active mapped root files; it is not an over-limit owner.

### The 10 absent TSV rows

These are exactly the missing rows reported by the graph: `call_allowlist.go`, `call_attempted_error.go`, `call_data_conversion.go`, `call_options.go`, `coordinator_session_credential_requirement.go`, `errors.go`, `provider_retry.go`, `question_stop.go`, `rate_limit_notify_ctx.go`, `ready_gate.go`.

* **Physically relocated, positively located:** `call_data_conversion.go`, `call_options.go`, and `question_stop.go` exist under `internal/agent/runtime/contract/agentcall/`. This is observed relocation, not merely inferred from SA1.
* **Absorbed/renamed with declaration evidence:** `provider_retry.go`'s `ProviderMaxRetries` is in `runtime/contract/agentcall/coordinator_contract.go:42`; `ErrCallAlreadyAttempted` (the purpose named for `call_attempted_error.go`) is in `coordinator_contract.go:510`; `readyGate` and its implementation are in `runtime/contract/agentcall/call_options.go:572+`. `WithRunAllowlist` and related context helpers from the allowlist seam appear in that `call_options.go` at lines 431–475. These are declaration-level relocation/absorption clues, not proof that every original declaration moved intact.
* **`errors.go`:** D1 §2.3 explicitly plans `errors.go` + `call_attempted_error.go` to merge into one agentcall file; `ErrCallAlreadyAttempted` is observed in `coordinator_contract.go`. Other contract sentinels are present there too. Exact original-file declaration inventory was not reconstructed, so do not claim that every error from the old `errors.go` is accounted for by this check.
* **`rate_limit_notify_ctx.go`:** the public context provider `WithRateLimitNotifySink` and sink-key declarations are in `runtime/contract/agentcall/call_options.go:485+`; this supports absorption into that file, but the old file’s complete declaration set was not independently checked.
* **`call_allowlist.go`:** run-allowlist context APIs are in the moved `call_options.go:431+`; full old-file absorption has not been proven.
* **`coordinator_session_credential_requirement.go`:** do **not** label this a completed SA2 absorption. The optional setter is still concretely in root `batch_glue.go:199–213`; the destination coordinator field is still `coordinator.go:166–168`. This is partial setter/field evidence only; the file row has no proven physical destination, and no complete absorption claim is made. D1 notes SA2 should bundle requirements at construction, but the current source shows the setter path.

`contract_bridge.go` is not one of the ten missing rows: it is the sole present unlisted row, owned as `root` for graph purposes. Its methods/aliases show it is a transitional SA1 bridge; treating those root references as intended final root API would be wrong.

## Export surface per boundary owner

For each owner below, symbols are the union of its incoming/outgoing edge witnesses, deduplicated by `(symbol name, declaration file, declaration line)`. The listed use figure is only the sum of per-witness counts retained after declaration deduplication; because uses can occur on multiple boundaries, this is an illustrative owner-level union, **not an authoritative owner total or required metric**. Appendix A's per-edge use figures are the authoritative boundary counts. “Already exported” is the Go export status observed in the current root package, **not** authorization to export after moving declarations. Root is included because the shim creates graph edges.

| owner | unioned distinct declarations | already exported | currently unexported | deduplicated witness-use sum (illustrative) |
|---|---:|---:|---:|---:|
| agenticfetch | 83 | 37 | 46 | 121 |
| agentrun | 353 | 51 | 302 | 674 |
| batchflow | 316 | 158 | 158 | 567 |
| batchinst | 552 | 202 | 350 | 1,188 |
| batchrun | 312 | 142 | 170 | 575 |
| callbeat | 40 | 3 | 37 | 84 |
| clibridge | 24 | 5 | 19 | 39 |
| coord | 363 | 102 | 261 | 831 |
| delegation | 68 | 27 | 41 | 208 |
| drainctl | 242 | 82 | 160 | 487 |
| finalize | 114 | 19 | 95 | 338 |
| mailbox | 94 | 13 | 81 | 234 |
| modelkit | 321 | 102 | 219 | 883 |
| promptcache | 18 | 0 | 18 | 22 |
| provauth | 56 | 6 | 50 | 165 |
| responsesws | 9 | 0 | 9 | 22 |
| sessionprompt | 41 | 4 | 37 | 72 |
| subagents | 169 | 51 | 118 | 289 |
| toolwrap | 63 | 7 | 56 | 121 |
| turnstep | 165 | 19 | 146 | 303 |
| usage | 33 | 8 | 25 | 50 |
| wakectl | 30 | 6 | 24 | 61 |
| watchdog | 58 | 5 | 53 | 103 |
| **root** | **70** | **37** | **33** | **148** |

These owner-level use figures are not summed as boundary demand: deduplication collapses repeated symbol witnesses across edges and loses cross-boundary occurrences. Appendix A preserves exact per-boundary symbol/use counts. Recompute ownership and export status after EX and declaration moves; current visibility does not predict post-move visibility.

## SCC disposition and direct-cycle evidence

The sole SCC has exactly these members:

`agenticfetch`, `agentrun`, `batchflow`, `batchinst`, `batchrun`, `callbeat`, `clibridge`, `coord`, `delegation`, `drainctl`, `finalize`, `mailbox`, `modelkit`, `promptcache`, `provauth`, `responsesws`, `sessionprompt`, `subagents`, `toolwrap`, `turnstep`, `usage`, `wakectl`, `watchdog`.

| Observed direct cycle | Example current graph witnesses (declaration → use; use count) | Candidate lane relation / disposition |
|---|---|---|
| batchinst ↔ batchrun | `BatchInvocation` `batch_invocation.go:98` → `batch_ingress.go:27` (16 uses; edge has 139 distinct witnesses/384 uses); `BatchWorkIngress` `batch_install_ingress.go:37` → `batch_exec.go:89` (3 uses; reverse edge has 101 witnesses/154 uses) | **DESIGN GAP:** DM5a/b partition files, but no named receiver-extraction port covers this pair; broad EX0K is not a concrete seam. |
| batchinst ↔ batchflow | `BatchRootLeaseStore` `batch_root_lease.go:40` → `batch_install_adapters.go:687` (1); `BatchInstall` `batch_install_adapters.go:668` → `batch_cli_runtime.go:17` (11; reverse edge has 65 witnesses/160 uses) | **DESIGN GAP:** DM5a/c partition files, but no named receiver-extraction port covers this pair; broad EX0K is not a concrete seam. |
| batchrun ↔ batchflow | `BatchInvocation` `batch_invocation.go:98` → `batch_delivery.go:114` (2; edge 35 witnesses/61 uses); reverse: `BatchCLITask` `batch_cli.go:19` → `batch_external_spec.go:72` (1; edge 15 witnesses/19 uses) | **DESIGN GAP:** DM5b/c partition files, but no named receiver-extraction port covers this pair; broad EX0K is not a concrete seam. |
| batchinst ↔ finalize | `ErrFinalizationScopeChanged` `finalization_publish.go:13` → `finalization_gate.go:212` (1); `BatchInstall.RootLeases` `batch_install_adapters.go:687` → finalize; `finalizationGate` `finalization_gate.go:63` → finalize files (4 uses; reverse edge 46 witnesses/133 uses) | **DESIGN GAP:** S-A1 moves `finalizationGate`, but no named receiver-extraction port dispositions the remaining pair; EX0K is not a concrete seam. |
| modelkit ↔ usage | `QuotaLimitResetTime` `quota_reset.go:26` → `provider_limit.go:77` (1; edge 3 witnesses/4 uses); `structuredLimitReset` `provider_limit.go:50` → `quota_reset.go:55,132` (2; reverse edge 4 witnesses/5 uses) | **DESIGN GAP:** no named receiver-extraction port covers this pair; EX0B/EX0H specify no bidirectional seam. |
| toolwrap ↔ agenticfetch | `privateFetchIngressTool` `agentic_fetch_policy.go:113` → `policy_leaf.go:190` (1; edge 2 witnesses/2 uses); `policyToolBuild` `policy_leaf.go:17` → agenticfetch uses (3; reverse edge 12 witnesses/27 uses) | **DESIGN GAP:** no named receiver-extraction port covers this pair; broad EX0K is not a concrete seam. |
| modelkit ↔ subagents | `subAgentParams` `coordinator_subagents.go:26` → `agent_tool.go:86` (1; edge 13 witnesses/13 uses); `ResolveRoleContract` `custom_role_runtime.go:14` → `coordinator_subagents.go:161` (1; reverse edge 21 witnesses/27 uses) | **DESIGN GAP:** EX0B/EX0D specify no seam for this pair; no named receiver-extraction port covers it. |
| agentrun ↔ promptcache | `cachePreparedDiagnostic` `cache_diagnostics.go:19` → `agent_turn_stream.go:196` (1; edge 4 witnesses/4 uses); `turnStream` `agent_turn_stream.go:110` → `cache_diagnostics.go:91` (1; reverse edge 6 witnesses/9 uses) | **DESIGN GAP:** no named receiver-extraction port covers this pair; EX0H/DM3 specify no complete two-way seam. |
| coord ↔ finalize | Present in graph: `coord → finalize` 9 witnesses/9 uses; `finalize → coord` 8/24. | EX0K explicitly includes finalize fragments; do not mislabel this as wholly uncovered, but sufficiency remains unverified. |
| coord ↔ sessionprompt | Present in graph: `coord → sessionprompt` 6/11; `sessionprompt → coord` 4/17. | EX0E retargets session-state/prompt methods and DM1 moves the leaf. Candidate coverage, not proof of every edge. |

“Addressed” in the cycle table requires a named seam or disposition, not merely a partition of files or likely relevance of a broad lane. The batchinst↔batchrun, batchinst↔batchflow, batchrun↔batchflow, batchinst↔finalize, modelkit↔usage, toolwrap↔agenticfetch, agentrun↔promptcache, and modelkit↔subagents pairs above are explicit **DESIGN GAPs**: the accepted lanes provide no named receiver-extraction ports for those pairs. EX0K's residual-fragment scope is not by itself a concrete seam. Other SCC edges are not implied covered by this sample. No exhaustive simple-cycle enumeration is claimed. Appendix B exposes every directed intra-SCC edge and all its witnesses; this is complete baseline evidence, not proof of the future graph.

### Planned cycle-to-lane map (inferred intent, not graph proof)

| D1 EX lane | Explicit intended owner pair(s) | Named port / component |
|---|---|---|
| EX0A | coord ↔ wakectl | `WakeSink` (wakectl) |
| EX0B | coord ↔ modelkit | `AutonomySource` (modelkit) |
| EX0C | coord ↔ drainctl | `DrainHost` (drainctl) |
| EX0D | coord ↔ subagents | `SubAgentHost` (subagents) |
| EX0E | coord + agentrun ↔ sessionprompt | `SessionStateHost` (sessionprompt) |
| EX0F | coord ↔ watchdog | `SupervisionHost` (watchdog) |
| EX0G | agentrun ↔ mailbox | `OwnershipHost` (mailbox) |
| EX0H | agentrun ↔ callbeat, provauth, turnstep | `RecoveryHost` (provauth), `ContextBudgetHost` (turnstep); callbeat moves with no named port here |
| EX0K | agentrun/coord ↔ responsesws, clibridge, usage, finalize, delegation, batch and residual fragments | Per-fragment disposition; no single named port |

These are mappings inferred from D1 §3.5/§5 names. They are not evidence that all SCC edges are cut. There is no I/J lane in the accepted plan. Any member pair with no plausible lane, and any pair merely assumed covered by a broad residual lane, remains open until the responsible lane records an explicit edge-by-edge disposition.

## Export and seam rules for execution

* Treat `must_export` entries as a report of identifiers that current file placement would make inaccessible. Resolve each with the exact rule-18 proof: compiler/external call location, or no export. The full witness inventory supports this audit; it does not bless a package-wide export surface.
* For globals, keep mutable state private. Use a narrow parameter, constructor field, or provider evaluated at the same point as the old global read. Preserve laziness and ordering; do not turn a lazy read into eager evaluation. Test-only dependencies stay in `_test.go`.
* For EX0 component extraction, follow split-agent §3.5 literally: retarget receiver methods first in the old package with body/order preserved, keep thin delegates at the coordinator, then move the already-retargeted files and define the narrow host port. Verify lock/transaction ordering; no graph-only argument establishes that behavior or lock safety.
* The batch-to-coordinator prohibition in D1 is a target constraint, not satisfied by this cyclic whole-file projection. Resolve private cross-package witnesses by port/declaration move; do not export coordinator internals just to quiet a graph edge.

## Execution/verification plan (future; not performed in this report)

Use split-agent §5 lane order and sprint-plan rules 18–19. One boundary or about 30 files per pass; workers receive batches of at most six files; no more than five agents run concurrently. Keep EX0 sequential where shared call paths overlap. After each batch, compile-only then vet, formatting; serialize verification and avoid concurrent resource-heavy Go commands. No repository tests are authorized in this report; the sprint plan reserves tests for the orchestrator, and this user explicitly prohibits tests now.

Suggested commands at future lane acceptance, from the repository root and via the documented lock wrapper:

```sh
.rush/tmp/golock.sh go test -c -o NUL -gcflags=-e <each affected package>
.rush/tmp/golock.sh go vet <affected packages>
# On linux, darwin, and windows as rule 19 requires; serialize them.
gofumpt -l <affected files>
goimports -l <affected files>
```

Those are future acceptance steps, not assertions of completion. After structural moves, rerun the graph on the actual new layout, inspect the newly emitted SCCs and all new edge witnesses, and update this report only with measured results. Run the plan’s declaration/test-function oracles for each pure move, and separate semantic component-extraction checks from pure-file-move checks. External-importer/DM8 compiler evidence is required before finalizing exports; it is outside this graph.

## Verification status — completed for the baseline tool and hygiene gates

The corrected tool's final snapshot `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-final.json` was independently compared with `agent-initial.json` using Python object equality: edge list and summary are identical. Thus the tabulated graph evidence remains unchanged. The final JSON also reports **285 receiver-colocation violations**, **1,478 private boundary symbols**, and **3,216 private boundary uses**; these are demand signals for narrow ports/declaration placement, not export authorization.

The orchestrator independently reran graph-tool `go build` through golock successfully with no output; `gofumpt -l` on all six cmd and four smoke Go files returned empty; independent cycle/selector smoke assertions passed (cyclic SCC p1/p2; receiver violation 1; `reader.Read`, `generic.Value`; shared.hidden count 3 vs other.hidden 1; shared.hit 2), DAG assertion passed (acyclic, no SCC, no receiver violations, topo count 3), and duplicate normalized `a.go` row was rejected (exit 1 with duplicate-path error; wrapper assertion exit 0). A report-census script independently confirmed all 214 Appendix A edge rows match graph counts and all 193 Appendix B SCC-edge ledgers/1,946 witness entries exactly match `(name, kind, declaration file, declaration line, exported, use count)`. These are tool smoke/hygiene and report-consistency checks only, not repository tests or proof of post-move correctness.

No repository tests or commits were run or created. There is no post-EX layout here: this is still the current-build whole-file projection and cannot certify final cycle removal, final exports, relocation completeness, or planned output counts. Later lane acceptance must regenerate graph against actual files and disposition new evidence. Appendix A preserves exact per-boundary counts; its 214 rows and Appendix B's 193 intra-SCC ledgers / 1,946 witnesses were independently checked against final JSON.

## Appendix A — boundary edge census (all 214 edges)

Per-edge witness and export counts deduplicate declarations by `(name, decl_file, decl_line)`. `Uses` is the graph edge use count (occurrences), not a declaration count. This table includes root-shim edges as well as the 193 intra-SCC edges.

| From | To | Distinct declarations | Already exported | Unexported | Uses |
|---|---|---:|---:|---:|---:|
| agenticfetch | agentrun | 14 | 14 | 0 | 14 |
| agenticfetch | batchinst | 19 | 4 | 15 | 22 |
| agenticfetch | batchrun | 8 | 7 | 1 | 8 |
| agenticfetch | callbeat | 2 | 0 | 2 | 2 |
| agenticfetch | clibridge | 1 | 0 | 1 | 3 |
| agenticfetch | coord | 6 | 0 | 6 | 21 |
| agenticfetch | delegation | 4 | 3 | 1 | 6 |
| agenticfetch | modelkit | 2 | 0 | 2 | 2 |
| agenticfetch | root | 3 | 0 | 3 | 4 |
| agenticfetch | subagents | 9 | 7 | 2 | 9 |
| agenticfetch | toolwrap | 12 | 1 | 11 | 27 |
| agentrun | batchflow | 6 | 2 | 4 | 6 |
| agentrun | batchinst | 8 | 4 | 4 | 22 |
| agentrun | callbeat | 10 | 0 | 10 | 12 |
| agentrun | coord | 5 | 1 | 4 | 8 |
| agentrun | delegation | 2 | 0 | 2 | 3 |
| agentrun | drainctl | 45 | 1 | 44 | 83 |
| agentrun | finalize | 12 | 0 | 12 | 13 |
| agentrun | mailbox | 44 | 0 | 44 | 93 |
| agentrun | modelkit | 3 | 2 | 1 | 3 |
| agentrun | promptcache | 4 | 0 | 4 | 4 |
| agentrun | provauth | 6 | 0 | 6 | 6 |
| agentrun | root | 17 | 14 | 3 | 79 |
| agentrun | sessionprompt | 11 | 1 | 10 | 17 |
| agentrun | toolwrap | 9 | 0 | 9 | 16 |
| agentrun | turnstep | 60 | 1 | 59 | 85 |
| agentrun | usage | 10 | 2 | 8 | 12 |
| agentrun | watchdog | 14 | 0 | 14 | 19 |
| batchflow | agentrun | 2 | 0 | 2 | 3 |
| batchflow | batchinst | 65 | 23 | 42 | 160 |
| batchflow | batchrun | 35 | 25 | 10 | 61 |
| batchflow | coord | 12 | 1 | 11 | 28 |
| batchflow | delegation | 5 | 2 | 3 | 7 |
| batchflow | drainctl | 38 | 33 | 5 | 80 |
| batchflow | modelkit | 3 | 1 | 2 | 6 |
| batchflow | root | 17 | 9 | 8 | 29 |
| batchflow | subagents | 3 | 0 | 3 | 3 |
| batchflow | toolwrap | 4 | 0 | 4 | 4 |
| batchinst | agentrun | 5 | 0 | 5 | 5 |
| batchinst | batchflow | 50 | 10 | 40 | 79 |
| batchinst | batchrun | 139 | 74 | 65 | 384 |
| batchinst | clibridge | 2 | 0 | 2 | 2 |
| batchinst | coord | 11 | 1 | 10 | 30 |
| batchinst | delegation | 5 | 3 | 2 | 8 |
| batchinst | drainctl | 6 | 5 | 1 | 8 |
| batchinst | finalize | 1 | 1 | 0 | 1 |
| batchinst | modelkit | 7 | 4 | 3 | 19 |
| batchinst | root | 17 | 9 | 8 | 62 |
| batchinst | toolwrap | 18 | 1 | 17 | 48 |
| batchinst | wakectl | 4 | 1 | 3 | 4 |
| batchrun | batchflow | 15 | 5 | 10 | 19 |
| batchrun | batchinst | 101 | 44 | 57 | 154 |
| batchrun | delegation | 6 | 3 | 3 | 22 |
| batchrun | modelkit | 6 | 4 | 2 | 19 |
| batchrun | root | 11 | 5 | 6 | 22 |
| batchrun | toolwrap | 15 | 2 | 13 | 37 |
| callbeat | agentrun | 9 | 0 | 9 | 40 |
| callbeat | modelkit | 1 | 0 | 1 | 1 |
| callbeat | promptcache | 3 | 0 | 3 | 4 |
| callbeat | root | 3 | 2 | 1 | 7 |
| callbeat | sessionprompt | 1 | 0 | 1 | 1 |
| callbeat | usage | 2 | 0 | 2 | 2 |
| clibridge | batchflow | 8 | 5 | 3 | 11 |
| clibridge | batchinst | 1 | 0 | 1 | 1 |
| clibridge | coord | 3 | 0 | 3 | 10 |
| clibridge | modelkit | 2 | 0 | 2 | 2 |
| clibridge | root | 2 | 0 | 2 | 2 |
| clibridge | subagents | 1 | 0 | 1 | 1 |
| clibridge | toolwrap | 4 | 0 | 4 | 9 |
| coord | agentrun | 2 | 0 | 2 | 4 |
| coord | batchflow | 46 | 31 | 15 | 67 |
| coord | batchinst | 58 | 18 | 40 | 117 |
| coord | batchrun | 2 | 0 | 2 | 2 |
| coord | callbeat | 3 | 0 | 3 | 5 |
| coord | delegation | 6 | 1 | 5 | 10 |
| coord | drainctl | 16 | 4 | 12 | 29 |
| coord | finalize | 9 | 2 | 7 | 9 |
| coord | mailbox | 1 | 0 | 1 | 8 |
| coord | modelkit | 42 | 1 | 41 | 113 |
| coord | root | 44 | 26 | 18 | 160 |
| coord | sessionprompt | 6 | 2 | 4 | 11 |
| coord | subagents | 13 | 1 | 12 | 26 |
| coord | turnstep | 6 | 2 | 4 | 14 |
| coord | usage | 1 | 0 | 1 | 1 |
| coord | wakectl | 4 | 2 | 2 | 4 |
| coord | watchdog | 7 | 0 | 7 | 7 |
| delegation | batchflow | 3 | 2 | 1 | 3 |
| delegation | batchinst | 1 | 1 | 0 | 4 |
| delegation | callbeat | 3 | 0 | 3 | 4 |
| delegation | coord | 8 | 0 | 8 | 28 |
| delegation | drainctl | 1 | 0 | 1 | 1 |
| delegation | modelkit | 8 | 3 | 5 | 14 |
| delegation | root | 12 | 10 | 2 | 107 |
| delegation | subagents | 2 | 0 | 2 | 4 |
| delegation | toolwrap | 1 | 0 | 1 | 1 |
| drainctl | agentrun | 4 | 0 | 4 | 25 |
| drainctl | batchflow | 23 | 17 | 6 | 39 |
| drainctl | batchinst | 32 | 11 | 21 | 48 |
| drainctl | coord | 18 | 1 | 17 | 79 |
| drainctl | delegation | 3 | 0 | 3 | 3 |
| drainctl | modelkit | 18 | 2 | 16 | 22 |
| drainctl | root | 10 | 8 | 2 | 43 |
| drainctl | sessionprompt | 1 | 0 | 1 | 1 |
| drainctl | subagents | 7 | 1 | 6 | 13 |
| drainctl | turnstep | 1 | 0 | 1 | 1 |
| drainctl | wakectl | 1 | 0 | 1 | 1 |
| drainctl | watchdog | 4 | 0 | 4 | 4 |
| finalize | agentrun | 18 | 0 | 18 | 82 |
| finalize | batchflow | 5 | 1 | 4 | 12 |
| finalize | batchinst | 46 | 6 | 40 | 133 |
| finalize | coord | 8 | 2 | 6 | 24 |
| finalize | mailbox | 1 | 1 | 0 | 1 |
| finalize | root | 5 | 5 | 0 | 41 |
| finalize | subagents | 8 | 1 | 7 | 21 |
| mailbox | agentrun | 7 | 0 | 7 | 22 |
| mailbox | batchflow | 3 | 1 | 2 | 4 |
| mailbox | batchinst | 17 | 4 | 13 | 25 |
| mailbox | batchrun | 1 | 1 | 0 | 4 |
| mailbox | clibridge | 1 | 0 | 1 | 1 |
| mailbox | coord | 2 | 0 | 2 | 4 |
| mailbox | drainctl | 1 | 0 | 1 | 1 |
| mailbox | finalize | 1 | 0 | 1 | 1 |
| mailbox | root | 4 | 3 | 1 | 71 |
| mailbox | turnstep | 3 | 3 | 0 | 3 |
| modelkit | agenticfetch | 1 | 0 | 1 | 1 |
| modelkit | agentrun | 25 | 22 | 3 | 35 |
| modelkit | batchflow | 5 | 2 | 3 | 5 |
| modelkit | batchinst | 25 | 5 | 20 | 30 |
| modelkit | batchrun | 38 | 25 | 13 | 63 |
| modelkit | callbeat | 6 | 1 | 5 | 13 |
| modelkit | coord | 50 | 7 | 43 | 333 |
| modelkit | delegation | 6 | 5 | 1 | 10 |
| modelkit | drainctl | 1 | 0 | 1 | 1 |
| modelkit | provauth | 6 | 0 | 6 | 6 |
| modelkit | responsesws | 3 | 0 | 3 | 6 |
| modelkit | root | 32 | 19 | 13 | 157 |
| modelkit | subagents | 13 | 8 | 5 | 13 |
| modelkit | toolwrap | 24 | 1 | 23 | 62 |
| modelkit | turnstep | 2 | 1 | 1 | 2 |
| modelkit | usage | 3 | 1 | 2 | 4 |
| modelkit | watchdog | 1 | 0 | 1 | 1 |
| promptcache | agentrun | 6 | 0 | 6 | 9 |
| provauth | agentrun | 4 | 0 | 4 | 50 |
| provauth | batchinst | 1 | 1 | 0 | 1 |
| provauth | callbeat | 3 | 0 | 3 | 5 |
| provauth | coord | 4 | 0 | 4 | 8 |
| provauth | delegation | 1 | 0 | 1 | 2 |
| provauth | mailbox | 2 | 0 | 2 | 8 |
| provauth | modelkit | 4 | 0 | 4 | 5 |
| provauth | root | 6 | 5 | 1 | 37 |
| provauth | sessionprompt | 4 | 0 | 4 | 6 |
| provauth | turnstep | 8 | 0 | 8 | 17 |
| provauth | usage | 6 | 0 | 6 | 12 |
| responsesws | coord | 3 | 0 | 3 | 11 |
| responsesws | modelkit | 2 | 0 | 2 | 3 |
| responsesws | provauth | 1 | 0 | 1 | 2 |
| sessionprompt | agentrun | 3 | 0 | 3 | 9 |
| sessionprompt | batchinst | 2 | 0 | 2 | 2 |
| sessionprompt | coord | 4 | 0 | 4 | 17 |
| sessionprompt | drainctl | 4 | 0 | 4 | 5 |
| sessionprompt | modelkit | 4 | 0 | 4 | 6 |
| sessionprompt | promptcache | 2 | 0 | 2 | 2 |
| sessionprompt | root | 2 | 2 | 0 | 2 |
| sessionprompt | watchdog | 1 | 0 | 1 | 2 |
| subagents | agentrun | 2 | 0 | 2 | 2 |
| subagents | batchflow | 8 | 2 | 6 | 10 |
| subagents | batchinst | 21 | 14 | 7 | 34 |
| subagents | batchrun | 6 | 1 | 5 | 9 |
| subagents | callbeat | 5 | 0 | 5 | 7 |
| subagents | coord | 25 | 0 | 25 | 79 |
| subagents | delegation | 13 | 6 | 7 | 19 |
| subagents | drainctl | 4 | 0 | 4 | 4 |
| subagents | finalize | 1 | 0 | 1 | 1 |
| subagents | modelkit | 21 | 3 | 18 | 27 |
| subagents | root | 18 | 13 | 5 | 32 |
| subagents | sessionprompt | 2 | 0 | 2 | 2 |
| subagents | wakectl | 1 | 0 | 1 | 2 |
| subagents | watchdog | 2 | 0 | 2 | 2 |
| toolwrap | agenticfetch | 2 | 1 | 1 | 2 |
| toolwrap | agentrun | 2 | 0 | 2 | 2 |
| toolwrap | batchflow | 2 | 1 | 1 | 2 |
| toolwrap | batchinst | 2 | 1 | 1 | 2 |
| toolwrap | batchrun | 2 | 1 | 1 | 2 |
| toolwrap | root | 2 | 0 | 2 | 2 |
| toolwrap | turnstep | 2 | 0 | 2 | 2 |
| turnstep | agentrun | 29 | 1 | 28 | 81 |
| turnstep | batchinst | 7 | 5 | 2 | 8 |
| turnstep | callbeat | 4 | 0 | 4 | 6 |
| turnstep | coord | 2 | 0 | 2 | 4 |
| turnstep | drainctl | 5 | 0 | 5 | 6 |
| turnstep | mailbox | 15 | 0 | 15 | 28 |
| turnstep | modelkit | 1 | 0 | 1 | 1 |
| turnstep | root | 7 | 6 | 1 | 26 |
| turnstep | sessionprompt | 4 | 0 | 4 | 5 |
| turnstep | toolwrap | 1 | 0 | 1 | 1 |
| turnstep | usage | 5 | 0 | 5 | 8 |
| turnstep | watchdog | 8 | 0 | 8 | 12 |
| usage | agentrun | 2 | 0 | 2 | 4 |
| usage | coord | 5 | 4 | 1 | 9 |
| usage | modelkit | 4 | 0 | 4 | 5 |
| usage | promptcache | 4 | 0 | 4 | 5 |
| usage | root | 2 | 1 | 1 | 5 |
| wakectl | batchinst | 5 | 2 | 3 | 5 |
| wakectl | coord | 5 | 0 | 5 | 35 |
| wakectl | drainctl | 8 | 0 | 8 | 8 |
| wakectl | root | 1 | 1 | 0 | 1 |
| wakectl | subagents | 1 | 0 | 1 | 1 |
| wakectl | watchdog | 1 | 0 | 1 | 1 |
| watchdog | batchflow | 6 | 5 | 1 | 8 |
| watchdog | batchinst | 1 | 0 | 1 | 1 |
| watchdog | coord | 15 | 0 | 15 | 50 |
| watchdog | root | 1 | 0 | 1 | 1 |
| watchdog | subagents | 4 | 0 | 4 | 6 |
| watchdog | wakectl | 1 | 0 | 1 | 2 |

## Appendix B — complete intra-SCC witness ledger (193 directed edges)

Every edge below is internal to the single 23-owner SCC. Each witness gives symbol, declaration kind, declaration file and line, current export status, and use count. This is the complete baseline witness inventory, not exhaustive enumeration of simple cycles and not a post-move graph.

<details>
<summary><code>agenticfetch → agentrun</code> — 14 declarations, 14 uses</summary>

- `NewSessionAgent` — func; `agent.go:504`; exported; 1 use(s).
- `SessionAgentOptions` — type; `agent_options.go:16`; exported; 1 use(s).
- `SessionAgentOptions.Config` — field; `agent_options.go:37`; exported; 1 use(s).
- `SessionAgentOptions.DisableAutoSummarize` — field; `agent_options.go:22`; exported; 1 use(s).
- `SessionAgentOptions.FastModel` — field; `agent_options.go:18`; exported; 1 use(s).
- `SessionAgentOptions.IsSubAgent` — field; `agent_options.go:21`; exported; 1 use(s).
- `SessionAgentOptions.IsYolo` — field; `agent_options.go:23`; exported; 1 use(s).
- `SessionAgentOptions.Messages` — field; `agent_options.go:25`; exported; 1 use(s).
- `SessionAgentOptions.RestrictedRuns` — field; `agent_options.go:131`; exported; 1 use(s).
- `SessionAgentOptions.Sessions` — field; `agent_options.go:24`; exported; 1 use(s).
- `SessionAgentOptions.SmartModel` — field; `agent_options.go:17`; exported; 1 use(s).
- `SessionAgentOptions.SystemPrompt` — field; `agent_options.go:20`; exported; 1 use(s).
- `SessionAgentOptions.SystemPromptPrefix` — field; `agent_options.go:19`; exported; 1 use(s).
- `SessionAgentOptions.Tools` — field; `agent_options.go:26`; exported; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → batchinst</code> — 19 declarations, 22 uses</summary>

- `BatchWorkIngress` — type; `batch_install_ingress.go:37`; exported; 3 use(s).
- `BatchWorkIngress.PinLeafRegistry` — method; `batch_install_ingress.go:42`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 1 use(s).
- `batchPinnedInvoke` — func; `batch_install_ingress.go:266`; private; 1 use(s).
- `batchToolsetFingerprint` — func; `batch_install_tools.go:401`; private; 1 use(s).
- `batchWorkIngress` — type; `batch_install_ingress.go:51`; private; 1 use(s).
- `batchWorkIngress.caps` — field; `batch_install_ingress.go:57`; private; 1 use(s).
- `coordinatorPrivateFetchIngress` — func; `batch_install_ingress.go:181`; private; 1 use(s).
- `diskCapabilityManager` — type; `disk_provider_capability.go:55`; private; 1 use(s).
- `diskCapabilityManager.acquireConstruction` — method; `disk_provider_capability.go:137`; private; 1 use(s).
- `diskCapabilityManager.release` — method; `disk_provider_capability.go:159`; private; 1 use(s).
- `diskOwnerConstruction` — const; `disk_provider_capability.go:31`; private; 1 use(s).
- `diskOwnerKey` — type; `disk_provider_capability.go:43`; private; 1 use(s).
- `diskOwnerKey.instance` — field; `disk_provider_capability.go:45`; private; 2 use(s).
- `diskOwnerKey.kind` — field; `disk_provider_capability.go:44`; private; 1 use(s).
- `diskOwnerKey.registryID` — field; `disk_provider_capability.go:49`; private; 1 use(s).
- `errBatchIngressTool` — type; `batch_install_tools.go:266`; private; 1 use(s).
- `errBatchIngressTool.AgentTool` — field; `batch_install_tools.go:267`; exported; 1 use(s).
- `errBatchIngressTool.err` — field; `batch_install_tools.go:268`; private; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → batchrun</code> — 8 declarations, 8 uses</summary>

- `BatchInvocation` — type; `batch_invocation.go:98`; exported; 1 use(s).
- `BatchInvocation.Origin` — field; `batch_invocation.go:109`; exported; 1 use(s).
- `BatchInvocation.Policy` — field; `batch_invocation.go:124`; exported; 1 use(s).
- `BatchInvocation.RegistryID` — field; `batch_invocation.go:106`; exported; 1 use(s).
- `batchLeafPolicyContext` — type; `batch_invocation.go:67`; private; 1 use(s).
- `batchLeafPolicyContext.RegistryID` — field; `batch_invocation.go:68`; exported; 1 use(s).
- `batchLeafPolicyContext.RestrictedToolset` — field; `batch_invocation.go:81`; exported; 1 use(s).
- `batchLeafPolicyContext.RoleContract` — field; `batch_invocation.go:85`; exported; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → callbeat</code> — 2 declarations, 2 uses</summary>

- `hbSourcePerCall` — const; `heartbeat_ctx.go:20`; private; 1 use(s).
- `withHeartbeat` — func; `heartbeat_ctx.go:66`; private; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → clibridge</code> — 1 declarations, 3 uses</summary>

- `pinnedInvoke` — type; `cli_tool_bridge.go:14`; private; 3 use(s).

</details>

<details>
<summary><code>agenticfetch → coord</code> — 6 declarations, 21 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 1 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 8 use(s).
- `coordinator.filetracker` — field; `coordinator.go:110`; private; 1 use(s).
- `coordinator.messages` — field; `coordinator.go:107`; private; 1 use(s).
- `coordinator.permissions` — field; `coordinator.go:108`; private; 8 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 2 use(s).

</details>

<details>
<summary><code>agenticfetch → delegation</code> — 4 declarations, 6 uses</summary>

- `ErrAgentDepthExceeded` — var; `delegation_depth.go:17`; exported; 2 use(s).
- `MaxAgentDepthFrom` — func; `delegation_depth.go:22`; exported; 1 use(s).
- `WithAgentDepth` — func; `delegation_depth.go:37`; exported; 2 use(s).
- `effectiveParentAgentDepth` — func; `delegation_depth.go:50`; private; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → modelkit</code> — 2 declarations, 2 uses</summary>

- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 1 use(s).
- `coordinator.buildAgentModels` — method; `coordinator_models_build.go:16`; private; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → subagents</code> — 9 declarations, 9 uses</summary>

- `coordinator.runSubAgent` — method; `coordinator_subagents.go:69`; private; 1 use(s).
- `subAgentParams` — type; `coordinator_subagents.go:26`; private; 1 use(s).
- `subAgentParams.Agent` — field; `coordinator_subagents.go:27`; exported; 1 use(s).
- `subAgentParams.AgentMessageID` — field; `coordinator_subagents.go:29`; exported; 1 use(s).
- `subAgentParams.Prompt` — field; `coordinator_subagents.go:31`; exported; 1 use(s).
- `subAgentParams.SessionID` — field; `coordinator_subagents.go:28`; exported; 1 use(s).
- `subAgentParams.SessionSetup` — field; `coordinator_subagents.go:36`; exported; 1 use(s).
- `subAgentParams.SessionTitle` — field; `coordinator_subagents.go:32`; exported; 1 use(s).
- `subAgentParams.ToolCallID` — field; `coordinator_subagents.go:30`; exported; 1 use(s).

</details>

<details>
<summary><code>agenticfetch → toolwrap</code> — 12 declarations, 27 uses</summary>

- `ErrRoleToolDenied` — var; `role_tool_policy.go:14`; exported; 1 use(s).
- `decoratePolicyTools` — func; `policy_leaf.go:250`; private; 1 use(s).
- `guardRoleToolCall` — func; `role_tool_policy.go:91`; private; 1 use(s).
- `newPolicyToolBuildForContext` — func; `policy_leaf.go:38`; private; 1 use(s).
- `policyLeafRegistry` — type; `policy_leaf.go:29`; private; 1 use(s).
- `policyLeafRegistry.resolveForContext` — method; `policy_leaf.go:84`; private; 1 use(s).
- `policyToolBuild` — type; `policy_leaf.go:17`; private; 3 use(s).
- `policyToolBuild.leaves` — field; `policy_leaf.go:19`; private; 3 use(s).
- `policyToolBuild.registryID` — field; `policy_leaf.go:20`; private; 1 use(s).
- `policyToolBuild.tools` — field; `policy_leaf.go:18`; private; 11 use(s).
- `restrictedRunAuthorizer` — func; `restricted_tool.go:62`; private; 2 use(s).
- `roleContractForToolBuild` — func; `policy_leaf.go:136`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → batchflow</code> — 6 declarations, 6 uses</summary>

- `BatchCutoverFacts.CaptureVisibleDebt` — method; `batch_cutover_facts.go:49`; exported; 1 use(s).
- `BatchCutoverFacts.PendingInclusiveDebt` — method; `batch_cutover_facts.go:52`; exported; 1 use(s).
- `armBatchQuestionTurnContext` — func; `batch_question_route.go:87`; private; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 1 use(s).
- `isBatchStartedToolResult` — func; `batch_delivery.go:393`; private; 1 use(s).
- `stripAskQuestionTools` — func; `batch_question_route.go:71`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → batchinst</code> — 8 declarations, 22 uses</summary>

- `DiskProviderRegistry` — alias; `disk_provider_binding.go:43`; exported; 4 use(s).
- `asyncToolMetadata` — type; `batch_rehome_tool_metadata.go:11`; private; 2 use(s).
- `asyncToolMetadata.Async` — field; `batch_rehome_tool_metadata.go:12`; exported; 5 use(s).
- `asyncToolMetadata.ClaimID` — field; `batch_rehome_tool_metadata.go:24`; exported; 7 use(s).
- `asyncToolMetadata.Inline` — field; `batch_rehome_tool_metadata.go:17`; exported; 1 use(s).
- `batchDriverHints.isExternalDriver` — method; `batch_rehome_coord_runtime.go:130`; private; 1 use(s).
- `finalizationStamp` — type; `finalization_gate.go:46`; private; 1 use(s).
- `recoverBatchScopeDeadCandidates` — func; `batch_scope_recovery.go:99`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → callbeat</code> — 10 declarations, 12 uses</summary>

- `addHeartbeatUsage` — func; `heartbeat_ctx.go:152`; private; 1 use(s).
- `cacheKeepAliveEntry` — type; `agent_cache_keepalive.go:79`; private; 2 use(s).
- `cacheKeepAliveEntry.timer` — field; `agent_cache_keepalive.go:80`; private; 1 use(s).
- `cacheKeepAliveInFlightEntry` — type; `agent_cache_keepalive.go:93`; private; 2 use(s).
- `cacheKeepAliveInFlightEntry.cancel` — field; `agent_cache_keepalive.go:95`; private; 1 use(s).
- `hbTurnRole` — func; `heartbeat_ctx.go:106`; private; 1 use(s).
- `hbTurnSource` — func; `heartbeat_ctx.go:115`; private; 1 use(s).
- `sessionAgent.cancelCacheKeepAlive` — method; `agent_cache_keepalive.go:493`; private; 1 use(s).
- `sessionAgent.scheduleCacheKeepAliveFromCtx` — method; `agent_cache_keepalive.go:125`; private; 1 use(s).
- `withHeartbeat` — func; `heartbeat_ctx.go:66`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → coord</code> — 5 declarations, 8 uses</summary>

- `PeakHoursError` — type; `coordinator.go:70`; exported; 1 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 2 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 3 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 1 use(s).
- `coordinator.driverHints` — field; `coordinator.go:136`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → delegation</code> — 2 declarations, 3 uses</summary>

- `bindRootRoleContract` — func; `root_role_contract.go:12`; private; 2 use(s).
- `restoreRootRoleContract` — func; `root_role_contract.go:42`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → drainctl</code> — 45 declarations, 83 uses</summary>

- `ForeignWorkspaceError` — func; `agent_workspace_refusal.go:49`; exported; 1 use(s).
- `arbiter.chainLastLaunch` — method; `turn_arbiter_state.go:388`; private; 3 use(s).
- `armTurnStall` — func; `turn_stall_clock.go:127`; private; 1 use(s).
- `batchCutoverReactionStore` — func; `reaction_write_cutover.go:16`; private; 2 use(s).
- `clearCancelRequest` — func; `cancel_flag.go:25`; private; 1 use(s).
- `disarmTurnStall` — func; `turn_stall_clock.go:145`; private; 1 use(s).
- `drainAccountBudget` — func; `drain_attempt.go:66`; private; 2 use(s).
- `drainAttempt` — type; `drain_attempt.go:103`; private; 3 use(s).
- `drainAttempt.batchCaptured` — field; `drain_attempt.go:157`; private; 1 use(s).
- `drainAttempt.batchSnapshot` — field; `drain_attempt.go:156`; private; 1 use(s).
- `drainAttempt.capAbort` — field; `drain_attempt.go:119`; private; 2 use(s).
- `drainAttempt.chainBatchRoots` — field; `drain_attempt.go:149`; private; 4 use(s).
- `drainAttempt.chainIdleClaims` — field; `drain_attempt.go:141`; private; 4 use(s).
- `drainAttempt.chainProgress` — field; `drain_attempt.go:142`; private; 3 use(s).
- `drainAttempt.closed` — field; `drain_attempt.go:162`; private; 1 use(s).
- `drainAttempt.commitNo` — field; `drain_attempt.go:115`; private; 1 use(s).
- `drainAttempt.credentialed` — field; `drain_attempt.go:134`; private; 1 use(s).
- `drainAttempt.outcome` — field; `drain_attempt.go:111`; private; 2 use(s).
- `drainAttempt.pendingLeft` — field; `drain_attempt.go:113`; private; 2 use(s).
- `drainAttempt.provider` — field; `drain_attempt.go:133`; private; 1 use(s).
- `drainAttempt.stalled` — field; `drain_attempt.go:117`; private; 1 use(s).
- `drainAttempt.streamErr` — field; `drain_attempt.go:128`; private; 1 use(s).
- `drainAttempt.turnCtxDone` — field; `drain_attempt.go:124`; private; 1 use(s).
- `drainAttempted` — const; `drain_attempt.go:34`; private; 1 use(s).
- `drainNoTurn` — const; `drain_attempt.go:32`; private; 1 use(s).
- `notAttempted` — func; `drain_attempt.go:95`; private; 9 use(s).
- `reminderBeforeTail` — func; `agent_drain.go:298`; private; 2 use(s).
- `runStallRetried` — func; `turn_stall_policy.go:126`; private; 1 use(s).
- `sessionAgent.afterTurn` — method; `drain_attempt.go:492`; private; 1 use(s).
- `sessionAgent.closeDrainAttempt` — method; `drain_attempt.go:181`; private; 2 use(s).
- `sessionAgent.decideDrainTurn` — method; `agent_drain_decision.go:19`; private; 1 use(s).
- `sessionAgent.newDrainAttempt` — method; `drain_attempt.go:167`; private; 1 use(s).
- `sessionAgent.noteRefusal` — method; `drain_attempt.go:478`; private; 7 use(s).
- `sessionAgent.pullPendingNotices` — method; `agent_notice_pull.go:22`; private; 1 use(s).
- `sessionAgent.pullPendingNoticesForStep` — method; `agent_notice_pull.go:75`; private; 1 use(s).
- `sessionAgent.stallAbortClosure` — method; `turn_stall_policy.go:153`; private; 1 use(s).
- `stallClock` — type; `turn_stall_clock.go:36`; private; 2 use(s).
- `stallClock.armAttemptCancel` — method; `turn_stall_policy.go:111`; private; 1 use(s).
- `stallClock.armStallAbort` — method; `turn_stall_policy.go:116`; private; 1 use(s).
- `stallClock.markByte` — method; `turn_stall_clock.go:176`; private; 1 use(s).
- `stallClock.markProgress` — method; `turn_stall_clock.go:168`; private; 4 use(s).
- `stallClock.reasoningChars` — field; `turn_stall_clock.go:49`; private; 1 use(s).
- `stallClock.setPhase` — method; `turn_stall_clock.go:159`; private; 2 use(s).
- `stallClock.toolEnter` — method; `turn_stall_clock.go:185`; private; 1 use(s).
- `stallClock.toolExit` — method; `turn_stall_clock.go:195`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → finalize</code> — 12 declarations, 13 uses</summary>

- `finalizationExecution` — type; `finalization_execution.go:12`; private; 1 use(s).
- `finalizationExecutionFor` — func; `finalization_execution.go:34`; private; 1 use(s).
- `finalizationWiring.finalizeTurn` — method; `finalization_wiring.go:282`; private; 1 use(s).
- `isFinalizationCandidate` — func; `finalization_execution.go:95`; private; 1 use(s).
- `persistedFinalizationCandidate` — type; `finalization_candidate.go:15`; private; 1 use(s).
- `sessionAgent.bindFinalizationDriverCall` — method; `finalization_driver_binding.go:74`; private; 1 use(s).
- `sessionAgent.finalizationWiring` — method; `finalization_wiring.go:299`; private; 1 use(s).
- `sessionAgent.restoreFinalizationCallContext` — method; `finalization_call_context.go:11`; private; 2 use(s).
- `sessionAgent.startExecution` — method; `finalization_root_bind.go:20`; private; 1 use(s).
- `turnStream.beginFinalizationStep` — method; `finalization_execution.go:45`; private; 1 use(s).
- `turnStream.markFinalizationStep` — method; `finalization_execution.go:59`; private; 1 use(s).
- `turnStream.persistFinalizationCandidate` — method; `finalization_execution.go:77`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → mailbox</code> — 44 declarations, 93 uses</summary>

- `activeCallToken` — type; `mailbox_generation.go:14`; private; 2 use(s).
- `batchOwnWorkFactsInstalled` — func; `agent_own_work_batch.go:66`; private; 1 use(s).
- `deferReleaseRetainedRunScope` — func; `queued_call_scope.go:193`; private; 1 use(s).
- `generation.cancel` — field; `mailbox.go:115`; private; 1 use(s).
- `mailbox` — type; `mailbox.go:132`; private; 2 use(s).
- `mailbox.beginGeneration` — method; `mailbox_generation.go:25`; private; 2 use(s).
- `mailbox.clearAll` — method; `mailbox_queue.go:20`; private; 1 use(s).
- `mailbox.clearCurrentCall` — method; `mailbox_generation.go:40`; private; 2 use(s).
- `mailbox.current` — field; `mailbox.go:145`; private; 1 use(s).
- `mailbox.currentCallSnapshot` — method; `mailbox_generation.go:48`; private; 1 use(s).
- `mailbox.currentCallState` — method; `mailbox_generation.go:61`; private; 1 use(s).
- `mailbox.currentCallStateWithToken` — method; `mailbox_generation.go:76`; private; 1 use(s).
- `mailbox.dispatcherCancel` — field; `mailbox.go:141`; private; 1 use(s).
- `mailbox.drainAfterCancel` — method; `mailbox_interrupt.go:318`; private; 5 use(s).
- `mailbox.hardStop` — method; `mailbox_interrupt.go:58`; private; 1 use(s).
- `mailbox.injectIfBusy` — method; `mailbox_inject.go:53`; private; 1 use(s).
- `mailbox.interruptAndReplace` — method; `mailbox_interrupt.go:80`; private; 1 use(s).
- `mailbox.interruptAndReplaceIfCurrent` — method; `mailbox_interrupt.go:90`; private; 1 use(s).
- `mailbox.mu` — field; `mailbox.go:133`; private; 10 use(s).
- `mailbox.queue` — method; `mailbox_queue.go:46`; private; 1 use(s).
- `mailbox.rebindDispatcher` — method; `mailbox_generation.go:157`; private; 2 use(s).
- `mailbox.reclaimReplacementOrKeep` — method; `mailbox_interrupt.go:294`; private; 1 use(s).
- `mailbox.setCurrentCall` — method; `mailbox_generation.go:33`; private; 1 use(s).
- `mailbox.state` — field; `mailbox.go:135`; private; 2 use(s).
- `mailbox.submitted` — field; `mailbox.go:157`; private; 2 use(s).
- `mailbox.testLoopRearmSeam` — field; `mailbox.go:227`; private; 2 use(s).
- `mbIdle` — const; `mailbox.go:104`; private; 2 use(s).
- `notifyActivity` — func; `agent_ownership.go:753`; private; 1 use(s).
- `reservedOwnership` — type; `agent_ownership.go:794`; private; 1 use(s).
- `reservedOwnership.cancel` — field; `agent_ownership.go:801`; private; 2 use(s).
- `reservedOwnership.claim` — method; `agent_ownership.go:814`; private; 1 use(s).
- `reservedOwnership.epoch` — field; `agent_ownership.go:796`; private; 2 use(s).
- `reservedOwnership.onHandoff` — field; `agent_ownership.go:807`; private; 2 use(s).
- `reservedOwnership.sessionID` — field; `agent_ownership.go:795`; private; 1 use(s).
- `reservedOwnershipFrom` — func; `agent_ownership.go:830`; private; 1 use(s).
- `sessionAgent.abandonOwnershipWithHandoff` — method; `agent_ownership.go:296`; private; 4 use(s).
- `sessionAgent.drainOrReleaseMerged` — method; `agent_ownership.go:245`; private; 2 use(s).
- `sessionAgent.getMailbox` — method; `agent_ownership.go:27`; private; 21 use(s).
- `sessionAgent.ownWorkBatch` — method; `agent_own_work_batch.go:23`; private; 1 use(s).
- `sessionAgent.restartOrphanedWithRetry` — method; `agent_ownership.go:429`; private; 1 use(s).
- `sessionAgent.tryReserveSession` — method; `agent_ownership.go:57`; private; 1 use(s).
- `turnEndRelease` — var; `queued_call_scope.go:205`; private; 1 use(s).
- `withActivityNotify` — func; `agent_ownership.go:738`; private; 1 use(s).
- `withWatchdogBump` — func; `agent_ownership.go:762`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → modelkit</code> — 3 declarations, 3 uses</summary>

- `AgentToolName` — const; `agent_tool.go:35`; exported; 1 use(s).
- `WithCallRoleContract` — func; `custom_role_runtime.go:204`; exported; 1 use(s).
- `redactNetworkURLs` — func; `coordinator_providers_network.go:74`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → promptcache</code> — 4 declarations, 4 uses</summary>

- `cachePreparedDiagnostic` — type; `cache_diagnostics.go:19`; private; 1 use(s).
- `preparedCacheDiagnostic` — func; `cache_diagnostics.go:48`; private; 1 use(s).
- `turnStream.logCacheStepUsage` — method; `cache_diagnostics.go:91`; private; 1 use(s).
- `withTurnMCPInstructions` — func; `cache_prefix_mcp.go:48`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → provauth</code> — 6 declarations, 6 uses</summary>

- `cloneRecoveryAttachments` — func; `agent_codex_recovery.go:237`; private; 1 use(s).
- `codexContextRejectedBeforeOutput` — func; `agent_codex_recovery.go:21`; private; 1 use(s).
- `codexRecoveryNudge` — const; `agent_codex_recovery.go:15`; private; 1 use(s).
- `sessionAgent.recoverCodexContext` — method; `agent_codex_recovery.go:159`; private; 1 use(s).
- `sessionAgent.runSummarizeSilent` — method; `agent_compaction_silent.go:33`; private; 1 use(s).
- `sessionAgent.snapshotCodexRecovery` — method; `agent_codex_recovery.go:79`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → sessionprompt</code> — 11 declarations, 17 uses</summary>

- `CallOriginFrom` — func; `agent_prompt.go:117`; exported; 1 use(s).
- `providerRetryLogFields` — func; `agent_prompt.go:600`; private; 1 use(s).
- `sanitizeToolInput` — func; `agent_prompt.go:622`; private; 1 use(s).
- `sessionAgent.convertToToolResult` — method; `agent_prompt.go:395`; private; 1 use(s).
- `sessionAgent.createUserMessage` — method; `agent_prompt.go:125`; private; 2 use(s).
- `sessionAgent.getCacheControlOptions` — method; `agent_prompt.go:28`; private; 4 use(s).
- `sessionAgent.getSessionMessages` — method; `agent_prompt.go:330`; private; 2 use(s).
- `sessionAgent.preparePrompt` — method; `agent_prompt.go:146`; private; 2 use(s).
- `sessionAgent.workaroundProviderMediaLimitations` — method; `agent_prompt.go:458`; private; 1 use(s).
- `sessionHeaders` — func; `agent_prompt.go:49`; private; 1 use(s).
- `trimMessagesToWindow` — func; `agent_prompt.go:546`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → toolwrap</code> — 9 declarations, 16 uses</summary>

- `decorateSessionTools` — func; `policy_leaf.go:254`; private; 2 use(s).
- `fatalToolCollector` — type; `tool_fatal_error.go:19`; private; 3 use(s).
- `fatalToolCollector.take` — method; `tool_fatal_error.go:34`; private; 1 use(s).
- `filterRoleContractTools` — func; `role_tool_policy.go:115`; private; 2 use(s).
- `roleToolContractsFrom` — func; `role_tool_policy.go:25`; private; 1 use(s).
- `roleToolsConstrained` — func; `role_tool_policy.go:82`; private; 2 use(s).
- `sessionAgent.augmentUnknownToolResult` — method; `tool_suggest.go:45`; private; 1 use(s).
- `withFatalToolErrors` — func; `tool_fatal_error.go:72`; private; 2 use(s).
- `withProviderOptionsOnLast` — func; `tool_provider_options.go:60`; private; 2 use(s).

</details>

<details>
<summary><code>agentrun → turnstep</code> — 60 declarations, 85 uses</summary>

- `InTurnGuardStopTitle` — const; `turn_progress_guard.go:33`; exported; 1 use(s).
- `admissionSetupFrom` — func; `turn_admission.go:20`; private; 1 use(s).
- `classifyStepCall` — func; `step_progress.go:323`; private; 3 use(s).
- `contextAutoSummarizeThreshold` — func; `agent_context_budget.go:55`; private; 2 use(s).
- `guardStopDetails` — func; `turn_progress_guard.go:358`; private; 1 use(s).
- `inTurnRefuseAfter` — const; `turn_progress_guard.go:24`; private; 1 use(s).
- `isWaitOnlyCall` — func; `step_progress.go:156`; private; 1 use(s).
- `logProviderWarnings` — func; `agent_turn_helpers.go:24`; private; 1 use(s).
- `modelInputBudget` — func; `agent_context_budget.go:26`; private; 2 use(s).
- `namedSpan` — type; `step_progress.go:180`; private; 1 use(s).
- `newTitleGenRegistry` — func; `agent_turn_title.go:27`; private; 1 use(s).
- `newTurnCheckpointWriter` — func; `agent_turn_checkpoint.go:83`; private; 1 use(s).
- `newTurnTitleJoiner` — func; `agent_turn_title.go:210`; private; 1 use(s).
- `newTurnUINotifier` — func; `agent_turn_ui_notify.go:31`; private; 1 use(s).
- `normalizeTurnError` — func; `agent_turn_helpers.go:202`; private; 1 use(s).
- `readCoverage.clear` — method; `step_progress.go:109`; private; 2 use(s).
- `readSpans` — func; `step_progress.go:189`; private; 1 use(s).
- `refuseWait` — func; `turn_progress_guard.go:45`; private; 1 use(s).
- `sessionAgent.drainDueInjects` — method; `agent_turn_helpers.go:64`; private; 1 use(s).
- `sessionAgent.handleWatchdogFire` — method; `agent_turn_helpers.go:117`; private; 1 use(s).
- `sessionAgent.runSummarizeBody` — method; `agent_compaction.go:371`; private; 1 use(s).
- `startTitleGeneration` — func; `agent_turn_title.go:123`; private; 1 use(s).
- `stepCallClass` — type; `step_progress.go:21`; private; 1 use(s).
- `stepCallNeutral` — const; `step_progress.go:41`; private; 2 use(s).
- `stepCallNoWrite` — const; `step_progress.go:43`; private; 2 use(s).
- `stepCallRefused` — const; `step_progress.go:38`; private; 5 use(s).
- `stepCallWait` — const; `step_progress.go:36`; private; 3 use(s).
- `stepClassifyCtx` — type; `step_progress.go:312`; private; 3 use(s).
- `stepClassifyCtx.seen` — field; `step_progress.go:313`; private; 1 use(s).
- `stepGuard.async` — field; `turn_progress_guard.go:114`; private; 1 use(s).
- `stepGuard.canDelegate` — field; `turn_progress_guard.go:115`; private; 1 use(s).
- `stepGuard.canEdit` — field; `turn_progress_guard.go:115`; private; 1 use(s).
- `stepGuard.ownWork` — field; `turn_progress_guard.go:114`; private; 1 use(s).
- `stepGuard.seen` — field; `turn_progress_guard.go:116`; private; 1 use(s).
- `stepGuard.streak` — field; `turn_progress_guard.go:113`; private; 2 use(s).
- `stepGuardFor` — func; `turn_progress_guard.go:124`; private; 1 use(s).
- `titleGenRegistry` — type; `agent_turn_title.go:21`; private; 1 use(s).
- `titleGenRegistry.cancelAll` — method; `agent_turn_title.go:54`; private; 1 use(s).
- `titleGenerationMaxDurationDefault` — const; `agent_title.go:39`; private; 1 use(s).
- `turnAdmission.markQueued` — method; `coordinator_turn_admission.go:43`; private; 1 use(s).
- `turnAdmissionFrom` — func; `coordinator_turn_admission.go:72`; private; 1 use(s).
- `turnCheckpointWriter.fenceFinalization` — method; `agent_turn_checkpoint.go:233`; private; 1 use(s).
- `turnCheckpointWriter.start` — method; `agent_turn_checkpoint.go:94`; private; 1 use(s).
- `turnCheckpointWriter.stop` — method; `agent_turn_checkpoint.go:240`; private; 1 use(s).
- `turnProgress` — type; `turn_progress_guard.go:98`; private; 1 use(s).
- `turnProgress.applyStep` — method; `turn_progress_guard.go:164`; private; 1 use(s).
- `turnProgress.guard` — field; `turn_progress_guard.go:105`; private; 2 use(s).
- `turnProgress.rereads` — field; `turn_progress_guard.go:102`; private; 2 use(s).
- `turnProgress.seen` — field; `turn_progress_guard.go:100`; private; 2 use(s).
- `turnProgress.stopped` — field; `turn_progress_guard.go:104`; private; 4 use(s).
- `turnProgress.streak` — field; `turn_progress_guard.go:99`; private; 2 use(s).
- `turnProgress.waits` — field; `turn_progress_guard.go:101`; private; 2 use(s).
- `turnSmartReasoningEffort` — func; `turn_effort.go:6`; private; 2 use(s).
- `turnTitleJoiner.join` — method; `agent_turn_title.go:214`; private; 1 use(s).
- `turnUINotifier.drainPending` — method; `agent_turn_ui_notify.go:107`; private; 1 use(s).
- `turnUINotifier.notify` — method; `agent_turn_ui_notify.go:78`; private; 1 use(s).
- `turnUINotifier.start` — method; `agent_turn_ui_notify.go:45`; private; 1 use(s).
- `unreportedTailTokens` — func; `agent_context_budget.go:70`; private; 1 use(s).
- `withoutTurnAdmission` — func; `coordinator_turn_admission.go:80`; private; 1 use(s).
- `wrapProgressGuard` — func; `turn_progress_guard.go:232`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → usage</code> — 10 declarations, 12 uses</summary>

- `LocalizeResetHint` — func; `quota_reset.go:195`; exported; 2 use(s).
- `QuotaLimitGuidance` — func; `quota_reset.go:263`; exported; 1 use(s).
- `addStepUsageTokens` — func; `agent_usage.go:109`; private; 2 use(s).
- `cloneFantasyMessages` — func; `usage_fallback.go:62`; private; 1 use(s).
- `fallbackStepUsage` — func; `usage_fallback.go:21`; private; 1 use(s).
- `normalizeProviderUsage` — func; `usage_normalize.go:26`; private; 1 use(s).
- `peakHoursStoppedFinishText` — func; `peak_hours_stop.go:75`; private; 1 use(s).
- `sessionAgent.openrouterCost` — method; `agent_usage.go:51`; private; 1 use(s).
- `sessionAgent.recordMessageUsage` — method; `usage_record.go:46`; private; 1 use(s).
- `sessionAgent.updateSessionUsage` — method; `agent_usage.go:82`; private; 1 use(s).

</details>

<details>
<summary><code>agentrun → watchdog</code> — 14 declarations, 19 uses</summary>

- `causeStall` — const; `stream_watchdog.go:49`; private; 1 use(s).
- `composeWatchdogFinishBody` — func; `stream_watchdog.go:544`; private; 1 use(s).
- `runTimeoutFinishText` — func; `timeout_stop.go:62`; private; 1 use(s).
- `startStreamWatchdog` — func; `stream_watchdog.go:195`; private; 1 use(s).
- `streamWatchdog` — type; `stream_watchdog.go:59`; private; 2 use(s).
- `streamWatchdog.bump` — field; `stream_watchdog.go:63`; private; 2 use(s).
- `streamWatchdog.done` — field; `stream_watchdog.go:107`; private; 1 use(s).
- `streamWatchdog.forceStall` — field; `stream_watchdog.go:111`; private; 1 use(s).
- `streamWatchdog.stalled` — field; `stream_watchdog.go:88`; private; 1 use(s).
- `streamWatchdog.toolFinished` — field; `stream_watchdog.go:83`; private; 1 use(s).
- `streamWatchdog.toolStarted` — field; `stream_watchdog.go:82`; private; 1 use(s).
- `watchdogCause` — type; `stream_watchdog.go:27`; private; 4 use(s).
- `watchdogFinishMessage` — func; `stream_watchdog.go:519`; private; 1 use(s).
- `watchdogToolResultMessage` — func; `stream_watchdog.go:566`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → agentrun</code> — 2 declarations, 3 uses</summary>

- `toolReceiptContextKey` — type; `agent_turn_stream.go:283`; private; 1 use(s).
- `toolResultReceiptSink` — type; `agent_turn_stream.go:292`; private; 2 use(s).

</details>

<details>
<summary><code>batchflow → batchinst</code> — 65 declarations, 160 uses</summary>

- `BatchInstall` — type; `batch_install_adapters.go:668`; exported; 11 use(s).
- `BatchInstall.CLIPump` — field; `batch_install_adapters.go:682`; exported; 2 use(s).
- `BatchInstall.OutputCache` — field; `batch_install_adapters.go:688`; exported; 1 use(s).
- `BatchInstall.RootLeases` — field; `batch_install_adapters.go:687`; exported; 3 use(s).
- `BatchInstall.Runner` — field; `batch_install_adapters.go:672`; exported; 11 use(s).
- `BatchInstall.RunnerHandle` — field; `batch_install_adapters.go:675`; exported; 20 use(s).
- `BatchInstall.Runtime` — field; `batch_install_adapters.go:674`; exported; 13 use(s).
- `BatchInstall.cliAccepted` — field; `batch_install_adapters.go:689`; private; 2 use(s).
- `BatchWorkIngress` — type; `batch_install_ingress.go:37`; exported; 2 use(s).
- `DiskProviderRegistry` — alias; `disk_provider_binding.go:43`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 4 use(s).
- `WithAdmittedBatchRoleContracts` — func; `batch_role_admission.go:21`; exported; 2 use(s).
- `admitBatchRoleContracts` — func; `batch_role_admission.go:97`; private; 1 use(s).
- `batchAgentLifecycleContextKey` — type; `batch_install_policy.go:21`; private; 1 use(s).
- `batchAgentQuestionBindingKey` — type; `batch_install_policy.go:28`; private; 1 use(s).
- `batchAgentQuestionContextKey` — type; `batch_install_policy.go:25`; private; 1 use(s).
- `batchAttachAdapter` — type; `batch_install_adapters.go:237`; private; 1 use(s).
- `batchAttachAdapter.store` — field; `batch_install_adapters.go:237`; private; 1 use(s).
- `batchCoordinatorBinding.ingress` — field; `batch_install_ingress.go:128`; private; 1 use(s).
- `batchDriverHints.hintSeqOf` — method; `batch_rehome_coord_runtime.go:65`; private; 1 use(s).
- `batchDriverHints.isExternalDriver` — method; `batch_rehome_coord_runtime.go:130`; private; 1 use(s).
- `batchLeafKind` — func; `batch_ingress.go:401`; private; 2 use(s).
- `batchMCPCapability` — type; `batch_admission.go:69`; private; 1 use(s).
- `batchRankContext` — type; `batch_admission.go:125`; private; 2 use(s).
- `batchRankContext.Root` — field; `batch_admission.go:129`; exported; 2 use(s).
- `batchRoleContractsWereAdmitted` — func; `batch_role_admission.go:25`; private; 1 use(s).
- `batchRunnerAdapter.Add` — method; `batch_install_adapters.go:129`; exported; 1 use(s).
- `batchRunnerAdapter.SubmitBound` — method; `batch_install_adapters.go:58`; exported; 1 use(s).
- `batchRunnerAdapter.batchFor` — method; `batch_install_adapters.go:218`; private; 3 use(s).
- `batchRunnerAdapter.mu` — field; `batch_install_adapters.go:34`; private; 2 use(s).
- `batchRunnerAdapter.ownerFor` — method; `batch_install_adapters.go:230`; private; 3 use(s).
- `batchRunnerAdapter.owners` — field; `batch_install_adapters.go:36`; private; 1 use(s).
- `batchRunnerAdapter.started` — field; `batch_install_adapters.go:35`; private; 1 use(s).
- `batchRunnerHandle` — type; `batch_runtime.go:18`; private; 3 use(s).
- `batchRunnerHandle.Stop` — method; `batch_runtime.go:22`; exported; 1 use(s).
- `batchRuntime.cfg` — field; `batch_runtime.go:130`; private; 13 use(s).
- `batchRuntimeConfig.Attach` — field; `batch_runtime.go:83`; exported; 1 use(s).
- `batchRuntimeConfig.Host` — field; `batch_runtime.go:73`; exported; 2 use(s).
- `batchRuntimeConfig.MaxDepth` — field; `batch_runtime.go:101`; exported; 2 use(s).
- `batchRuntimeConfig.MaxNodes` — field; `batch_runtime.go:101`; exported; 2 use(s).
- `batchRuntimeConfig.Profile` — field; `batch_runtime.go:75`; exported; 2 use(s).
- `batchRuntimeConfig.Resolver` — field; `batch_runtime.go:78`; exported; 2 use(s).
- `batchRuntimeConfig.Runner` — field; `batch_runtime.go:88`; exported; 1 use(s).
- `batchRuntimeConfig.RunnerID` — field; `batch_runtime.go:73`; exported; 1 use(s).
- `batchSpecNodes` — func; `batch_admission.go:153`; private; 1 use(s).
- `batchWorkIngress` — type; `batch_install_ingress.go:51`; private; 1 use(s).
- `batchWorkIngress.rt` — field; `batch_install_ingress.go:52`; private; 4 use(s).
- `batchWorkTarget` — func; `batch_install_tools.go:55`; private; 1 use(s).
- `classifyBatchLeaf` — func; `batch_admission.go:82`; private; 1 use(s).
- `coordinator.foreignLiveDriver` — method; `batch_rehome_coord_runtime.go:181`; private; 1 use(s).
- `coordinatorBatchBindingFor` — func; `batch_install_ingress.go:164`; private; 1 use(s).
- `diskCapabilityManager` — type; `disk_provider_capability.go:55`; private; 2 use(s).
- `diskCapabilityManager.acquire` — method; `disk_provider_capability.go:103`; private; 1 use(s).
- `diskCapabilityManager.release` — method; `disk_provider_capability.go:159`; private; 1 use(s).
- `diskOwnerKey` — type; `disk_provider_capability.go:43`; private; 2 use(s).
- `diskOwnerKey.bindingID` — field; `disk_provider_capability.go:46`; private; 1 use(s).
- `diskOwnerKey.instance` — field; `disk_provider_capability.go:45`; private; 1 use(s).
- `diskOwnerKey.kind` — field; `disk_provider_capability.go:44`; private; 1 use(s).
- `diskOwnerKey.registryID` — field; `disk_provider_capability.go:49`; private; 1 use(s).
- `diskOwnerRoot` — const; `disk_provider_capability.go:33`; private; 1 use(s).
- `validateBatch` — func; `batch_admission.go:213`; private; 2 use(s).
- `validateBatchRolePayloads` — func; `batch_role_admission.go:223`; private; 3 use(s).
- `validateSelectedBatchRole` — func; `batch_role_admission.go:188`; private; 1 use(s).
- `withBatchAgentQuestionBinding` — func; `batch_install_policy.go:46`; private; 1 use(s).
- `withBatchAgentQuestionContext` — func; `batch_install_policy.go:34`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → batchrun</code> — 35 declarations, 61 uses</summary>

- `BatchInvocation` — type; `batch_invocation.go:98`; exported; 2 use(s).
- `BatchInvocation.BackgroundEscape` — field; `batch_invocation.go:115`; exported; 1 use(s).
- `BatchInvocation.CallID` — field; `batch_invocation.go:110`; exported; 2 use(s).
- `BatchInvocation.Delivery` — field; `batch_invocation.go:114`; exported; 2 use(s).
- `BatchInvocation.SessionID` — field; `batch_invocation.go:100`; exported; 2 use(s).
- `batchControlTarget` — func; `batch_exec_generic.go:86`; private; 1 use(s).
- `batchDeliveryChoice.Mode` — field; `batch_invocation.go:90`; exported; 1 use(s).
- `batchDeliveryChoice.Window` — field; `batch_invocation.go:91`; exported; 1 use(s).
- `batchDeliveryDetached` — const; `batch_invocation.go:26`; private; 2 use(s).
- `batchDeliveryImpl.psvArmSupervision` — method; `batch_supervision_arming.go:19`; private; 3 use(s).
- `batchDeliveryInline` — const; `batch_invocation.go:25`; private; 1 use(s).
- `batchDeliverySync` — const; `batch_invocation.go:24`; private; 2 use(s).
- `batchEscapeAuto` — const; `batch_invocation.go:33`; private; 1 use(s).
- `batchKindAgent` — const; `batch_exec.go:25`; private; 1 use(s).
- `batchLeafPayload` — type; `batch_exec.go:33`; private; 4 use(s).
- `batchLeafPayload.AgentDepth` — field; `batch_exec.go:66`; exported; 1 use(s).
- `batchLeafPayload.CallCredentials` — field; `batch_exec.go:43`; exported; 2 use(s).
- `batchLeafPayload.Input` — field; `batch_exec.go:37`; exported; 2 use(s).
- `batchLeafPayload.Policy` — field; `batch_exec.go:39`; exported; 3 use(s).
- `batchLeafPayload.QuestionRoute` — field; `batch_exec.go:51`; exported; 1 use(s).
- `batchLeafPayload.RegistryID` — field; `batch_exec.go:38`; exported; 2 use(s).
- `batchLeafPayload.SelectedRoleContract` — field; `batch_exec.go:42`; exported; 3 use(s).
- `batchLeafPayload.SessionID` — field; `batch_exec.go:36`; exported; 3 use(s).
- `batchLeafPayload.Tool` — field; `batch_exec.go:34`; exported; 5 use(s).
- `batchLeafPayload.ToolCallID` — field; `batch_exec.go:35`; exported; 2 use(s).
- `batchLeafPolicyContext` — type; `batch_invocation.go:67`; private; 1 use(s).
- `batchLeafPolicyContext.FolderScope` — field; `batch_invocation.go:72`; exported; 1 use(s).
- `batchLeafPolicyContext.MessageID` — field; `batch_invocation.go:69`; exported; 2 use(s).
- `batchLeafPolicyContext.ModelName` — field; `batch_invocation.go:71`; exported; 1 use(s).
- `batchLeafPolicyContext.RegistryID` — field; `batch_invocation.go:68`; exported; 1 use(s).
- `batchLeafPolicyContext.RestrictedToolset` — field; `batch_invocation.go:81`; exported; 1 use(s).
- `batchLeafPolicyContext.RoleContract` — field; `batch_invocation.go:85`; exported; 1 use(s).
- `batchLeafPolicyContext.RunAllowlist` — field; `batch_invocation.go:73`; exported; 1 use(s).
- `batchLeafPolicyContext.SupportsImages` — field; `batch_invocation.go:70`; exported; 1 use(s).
- `batchOutputCache.readWithMetadata` — method; `batch_terminal_output.go:125`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → coord</code> — 12 declarations, 28 uses</summary>

- `NewBatchCLIBuilderWithQuestionRoute` — func; `batch_glue.go:21`; exported; 1 use(s).
- `batchQuestionMarkerDepth` — func; `coordinator_agent_inject_batch.go:278`; private; 1 use(s).
- `callCredentialsFrom` — func; `credentials.go:41`; private; 2 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 10 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 1 use(s).
- `coordinator.autonomyEnabled` — method; `coordinator.go:550`; private; 1 use(s).
- `coordinator.batchDriverClaimer` — field; `coordinator.go:155`; private; 2 use(s).
- `coordinator.bgArrival` — field; `coordinator.go:255`; private; 2 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 3 use(s).
- `coordinator.driverHints` — field; `coordinator.go:136`; private; 2 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 2 use(s).
- `coordinator.stopTree` — method; `coordinator_interrupt.go:60`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → delegation</code> — 5 declarations, 7 uses</summary>

- `DefaultAgentDepth` — const; `delegation_depth.go:12`; exported; 2 use(s).
- `WithParentRoleConfigSnapshot` — func; `delegation_role_models.go:155`; exported; 1 use(s).
- `parentRoleContractFrom` — func; `delegation_role_models.go:159`; private; 2 use(s).
- `persistedAgentDepthContextKey` — type; `delegation_depth.go:19`; private; 1 use(s).
- `validateAgentRoleInput` — func; `delegation_role.go:9`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → drainctl</code> — 38 declarations, 80 uses</summary>

- `DebtFacts.BGShellNotices` — field; `turn_arbiter.go:111`; exported; 1 use(s).
- `DebtFacts.BGShellOnly` — field; `turn_arbiter.go:101`; exported; 2 use(s).
- `DebtFacts.CompletionOnly` — field; `turn_arbiter.go:110`; exported; 3 use(s).
- `DebtFacts.OverCapRows` — field; `turn_arbiter.go:112`; exported; 1 use(s).
- `DebtFacts.PendingIncl` — field; `turn_arbiter.go:100`; exported; 1 use(s).
- `GateFacts.HintNow` — field; `turn_arbiter.go:121`; exported; 1 use(s).
- `SessionFacts.AutoResumes` — field; `turn_arbiter.go:90`; exported; 1 use(s).
- `SessionFacts.AutonomyEnabled` — field; `turn_arbiter.go:87`; exported; 1 use(s).
- `SessionFacts.ChainBatchRoots` — field; `turn_arbiter.go:79`; exported; 5 use(s).
- `SessionFacts.ChainLinks` — field; `turn_arbiter.go:72`; exported; 2 use(s).
- `SessionFacts.ChainNoticed` — field; `turn_arbiter.go:74`; exported; 1 use(s).
- `SessionFacts.ChainOwnsAllDebt` — field; `turn_arbiter.go:73`; exported; 1 use(s).
- `SessionFacts.DurableChild` — field; `turn_arbiter.go:86`; exported; 1 use(s).
- `SessionFacts.ExternallyDrivenSelf` — field; `turn_arbiter.go:61`; exported; 2 use(s).
- `SessionFacts.ForeignDriverLive` — field; `turn_arbiter.go:69`; exported; 1 use(s).
- `SessionFacts.Held` — field; `turn_arbiter.go:70`; exported; 1 use(s).
- `SessionFacts.RunningDelegation` — field; `turn_arbiter.go:80`; exported; 1 use(s).
- `SessionFacts.RunningOwnJobs` — field; `turn_arbiter.go:85`; exported; 1 use(s).
- `SessionFacts.SleepAll` — field; `turn_arbiter.go:89`; exported; 1 use(s).
- `SessionFacts.Suspended` — field; `turn_arbiter.go:71`; exported; 1 use(s).
- `TurnFacts` — type; `turn_arbiter.go:163`; exported; 2 use(s).
- `TurnFacts.Debt` — field; `turn_arbiter.go:167`; exported; 8 use(s).
- `TurnFacts.Gate` — field; `turn_arbiter.go:168`; exported; 2 use(s).
- `TurnFacts.Session` — field; `turn_arbiter.go:166`; exported; 20 use(s).
- `arbiter.snapshot` — method; `turn_arbiter_state.go:96`; private; 1 use(s).
- `arbiterSnapshot.AutoResumes` — field; `turn_arbiter_state.go:85`; exported; 2 use(s).
- `arbiterSnapshot.ChainBatchRoots` — field; `turn_arbiter_state.go:89`; exported; 3 use(s).
- `arbiterSnapshot.ChainLinks` — field; `turn_arbiter_state.go:87`; exported; 1 use(s).
- `arbiterSnapshot.ChainNoticed` — field; `turn_arbiter_state.go:90`; exported; 1 use(s).
- `arbiterSnapshot.Gate` — field; `turn_arbiter_state.go:92`; exported; 1 use(s).
- `arbiterSnapshot.Held` — field; `turn_arbiter_state.go:83`; exported; 1 use(s).
- `arbiterSnapshot.OverCap` — field; `turn_arbiter_state.go:86`; exported; 1 use(s).
- `arbiterSnapshot.SleepAll` — field; `turn_arbiter_state.go:91`; exported; 2 use(s).
- `arbiterSnapshot.Suspended` — field; `turn_arbiter_state.go:84`; exported; 1 use(s).
- `buildBatchNoticeMessageParams` — func; `agent_notice_pull.go:108`; private; 1 use(s).
- `coordinator.isDurableDelegationChild` — method; `coordinator_drain_policy.go:190`; private; 2 use(s).
- `turnAutoResumeCap` — const; `turn_arbiter.go:43`; private; 1 use(s).
- `turnReactionChainLimit` — const; `turn_arbiter.go:41`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → modelkit</code> — 3 declarations, 6 uses</summary>

- `AgentToolName` — const; `agent_tool.go:35`; exported; 2 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 3 use(s).
- `coordinator.buildPolicyTools` — method; `coordinator_tools.go:587`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → subagents</code> — 3 declarations, 3 uses</summary>

- `ctxMutex.lock` — method; `coordinator_bgshell_cap.go:33`; private; 1 use(s).
- `ctxMutex.unlock` — method; `coordinator_bgshell_cap.go:43`; private; 1 use(s).
- `readTurnFactsGateSeam` — var; `coordinator_bgshell_cap.go:56`; private; 1 use(s).

</details>

<details>
<summary><code>batchflow → toolwrap</code> — 4 declarations, 4 uses</summary>

- `policyLeafRegistry` — type; `policy_leaf.go:29`; private; 1 use(s).
- `policyLeafRegistry.resolve` — method; `policy_leaf.go:80`; private; 1 use(s).
- `policyToolBuild.leaves` — field; `policy_leaf.go:19`; private; 1 use(s).
- `policyToolBuild.registryID` — field; `policy_leaf.go:20`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → agentrun</code> — 5 declarations, 5 uses</summary>

- `toolResultReceipt` — type; `agent_turn_stream.go:285`; private; 1 use(s).
- `toolResultReceipt.identity` — field; `agent_turn_stream.go:286`; private; 1 use(s).
- `toolResultReceipt.persist` — field; `agent_turn_stream.go:289`; private; 1 use(s).
- `toolResultReceiptSink` — type; `agent_turn_stream.go:292`; private; 1 use(s).
- `toolResultReceiptSink.add` — method; `agent_turn_stream.go:297`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → batchflow</code> — 50 declarations, 79 uses</summary>

- `BatchCutoverFacts` — type; `batch_cutover_facts.go:43`; exported; 1 use(s).
- `BatchCutoverFacts.LiveNodes` — method; `batch_cutover_facts.go:72`; exported; 1 use(s).
- `BatchInstall.commitCLIControl` — method; `batch_cli_runtime.go:126`; private; 1 use(s).
- `BatchRootLeaseStore` — alias; `batch_root_lease.go:40`; exported; 1 use(s).
- `batchCredentialEscrow` — type; `batch_credentials_escrow.go:24`; private; 5 use(s).
- `batchCredentialEscrow.consume` — method; `batch_credentials_escrow.go:89`; private; 1 use(s).
- `batchCredentialEscrow.registerBatch` — method; `batch_credentials_escrow.go:61`; private; 2 use(s).
- `batchCredentialEscrow.releaseRoot` — method; `batch_credentials_escrow.go:125`; private; 1 use(s).
- `batchCredentialKey` — type; `batch_credentials_escrow.go:10`; private; 1 use(s).
- `batchCredentialKey.callID` — field; `batch_credentials_escrow.go:14`; private; 1 use(s).
- `batchCredentialKey.messageID` — field; `batch_credentials_escrow.go:13`; private; 1 use(s).
- `batchCredentialKey.registryID` — field; `batch_credentials_escrow.go:11`; private; 1 use(s).
- `batchCredentialKey.sessionID` — field; `batch_credentials_escrow.go:12`; private; 1 use(s).
- `batchCredentialKey.tool` — field; `batch_credentials_escrow.go:15`; private; 1 use(s).
- `batchCredentialKeyFor` — func; `batch_credentials_escrow.go:139`; private; 1 use(s).
- `batchCredentialLeaf` — type; `batch_credentials_escrow.go:29`; private; 1 use(s).
- `batchCredentialLeaf.AgentTool` — field; `batch_credentials_escrow.go:30`; exported; 1 use(s).
- `batchCredentialLeaf.escrow` — field; `batch_credentials_escrow.go:31`; private; 1 use(s).
- `batchCredentialRegistration` — type; `batch_credentials_escrow.go:55`; private; 4 use(s).
- `batchCredentialRegistration.creds` — field; `batch_credentials_escrow.go:58`; private; 1 use(s).
- `batchCredentialRegistration.key` — field; `batch_credentials_escrow.go:56`; private; 1 use(s).
- `batchCredentialRegistration.root` — field; `batch_credentials_escrow.go:57`; private; 1 use(s).
- `batchDeliveryDeps` — type; `batch_delivery.go:71`; private; 1 use(s).
- `batchDeliveryDeps.ArmSupervision` — field; `batch_delivery.go:91`; exported; 1 use(s).
- `batchDeliveryDeps.Done` — field; `batch_delivery.go:76`; exported; 1 use(s).
- `batchDeliveryDeps.Elect` — field; `batch_delivery.go:77`; exported; 1 use(s).
- `batchDeliveryDeps.Owner` — field; `batch_delivery.go:75`; exported; 1 use(s).
- `batchDeliveryDeps.Runner` — field; `batch_delivery.go:80`; exported; 1 use(s).
- `batchDeliveryDeps.ToolResultReceipt` — field; `batch_delivery.go:98`; exported; 1 use(s).
- `batchDoneSource` — type; `batch_delivery.go:52`; private; 2 use(s).
- `batchElector` — type; `batch_delivery.go:62`; private; 2 use(s).
- `batchNoticePull` — type; `batch_delivery_drain.go:20`; private; 2 use(s).
- `batchQuestionRouteFrom` — func; `batch_question_route.go:34`; private; 3 use(s).
- `batchRootLeaseStore` — type; `batch_root_lease.go:21`; private; 3 use(s).
- `batchRootLeaseStore.acquire` — method; `batch_root_lease.go:62`; private; 1 use(s).
- `batchRootLeaseStore.adopt` — method; `batch_root_lease.go:98`; private; 1 use(s).
- `batchRootLeaseStore.matches` — method; `batch_root_lease.go:135`; private; 1 use(s).
- `batchRootLeaseStore.release` — method; `batch_root_lease.go:117`; private; 1 use(s).
- `callerCancelOr` — func; `batch_done_cancel.go:10`; private; 3 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 1 use(s).
- `errBatchCredentialEscrowMissing` — var; `batch_credentials_escrow.go:40`; private; 4 use(s).
- `newBatchCredentialEscrow` — func; `batch_credentials_escrow.go:36`; private; 3 use(s).
- `newBatchDelivery` — func; `batch_delivery.go:121`; private; 1 use(s).
- `newBatchRootLeaseStore` — func; `batch_root_lease.go:32`; private; 1 use(s).
- `nodeBatchCutoverFacts` — type; `batch_cutover_facts_node.go:23`; private; 1 use(s).
- `nodeBatchCutoverFacts.live` — field; `batch_cutover_facts_node.go:25`; private; 5 use(s).
- `nodeBatchCutoverFacts.store` — field; `batch_cutover_facts_node.go:24`; private; 2 use(s).
- `stampBatchCallCredentials` — func; `batch_credentials_marker.go:13`; private; 3 use(s).
- `stripAskQuestionTools` — func; `batch_question_route.go:71`; private; 1 use(s).
- `withBatchQuestionRoute` — func; `batch_question_route.go:28`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → batchrun</code> — 139 declarations, 384 uses</summary>

- `BatchInvocation` — type; `batch_invocation.go:98`; exported; 16 use(s).
- `BatchInvocation.AgentDepth` — field; `batch_invocation.go:111`; exported; 12 use(s).
- `BatchInvocation.AgentDepthSet` — field; `batch_invocation.go:112`; exported; 5 use(s).
- `BatchInvocation.BackgroundEscape` — field; `batch_invocation.go:115`; exported; 4 use(s).
- `BatchInvocation.CallID` — field; `batch_invocation.go:110`; exported; 1 use(s).
- `BatchInvocation.Delivery` — field; `batch_invocation.go:114`; exported; 6 use(s).
- `BatchInvocation.MessageID` — field; `batch_invocation.go:102`; exported; 3 use(s).
- `BatchInvocation.Origin` — field; `batch_invocation.go:109`; exported; 3 use(s).
- `BatchInvocation.Policy` — field; `batch_invocation.go:124`; exported; 26 use(s).
- `BatchInvocation.QuestionRoute` — field; `batch_invocation.go:134`; exported; 3 use(s).
- `BatchInvocation.RegistryID` — field; `batch_invocation.go:106`; exported; 11 use(s).
- `BatchInvocation.SessionID` — field; `batch_invocation.go:100`; exported; 5 use(s).
- `BatchInvocation.Validate` — method; `batch_invocation.go:173`; exported; 1 use(s).
- `BatchInvocation.WaiterRank` — field; `batch_invocation.go:120`; exported; 3 use(s).
- `BatchInvocation.roleConfig` — field; `batch_invocation.go:136`; private; 2 use(s).
- `NewInstalledBatchModelControl` — func; `batch_model_control.go:43`; exported; 1 use(s).
- `batchAgentDeps` — type; `batch_exec_agent.go:109`; private; 5 use(s).
- `batchAgentDeps.Bind` — field; `batch_exec_agent.go:130`; exported; 1 use(s).
- `batchAgentDeps.ChildFacts` — field; `batch_exec_agent.go:136`; exported; 1 use(s).
- `batchAgentDeps.Descendants` — field; `batch_exec_agent.go:111`; exported; 3 use(s).
- `batchAgentDeps.Host` — field; `batch_exec_agent.go:124`; exported; 1 use(s).
- `batchAgentDeps.Poll` — field; `batch_exec_agent.go:114`; exported; 1 use(s).
- `batchAgentDeps.QuestionPublisher` — field; `batch_exec_agent.go:115`; exported; 1 use(s).
- `batchAgentDeps.QuestionResolver` — field; `batch_exec_agent.go:120`; exported; 1 use(s).
- `batchAgentDeps.RunnerID` — field; `batch_exec_agent.go:124`; exported; 1 use(s).
- `batchAgentDeps.Runs` — field; `batch_exec_agent.go:110`; exported; 4 use(s).
- `batchAgentDescendantReader` — type; `batch_exec_agent.go:104`; private; 1 use(s).
- `batchAgentHost` — type; `batch_exec_agent.go:58`; private; 2 use(s).
- `batchAgentHost.Ask` — field; `batch_exec_agent.go:59`; exported; 1 use(s).
- `batchAgentHost.BindChildSession` — field; `batch_exec_agent.go:60`; exported; 2 use(s).
- `batchAgentHost.NoChildSelected` — field; `batch_exec_agent.go:61`; exported; 2 use(s).
- `batchAgentRunFactory` — type; `batch_exec_agent.go:87`; private; 1 use(s).
- `batchAgentRunRequest` — type; `batch_exec_agent.go:19`; private; 3 use(s).
- `batchAgentRunRequest.AgentDepth` — field; `batch_exec_agent.go:37`; exported; 5 use(s).
- `batchAgentRunRequest.CallCredentials` — field; `batch_exec_agent.go:33`; exported; 2 use(s).
- `batchAgentRunRequest.ChildSessionID` — field; `batch_exec_agent.go:43`; exported; 4 use(s).
- `batchAgentRunRequest.DiskBindingID` — field; `batch_exec_agent.go:29`; exported; 4 use(s).
- `batchAgentRunRequest.Ingress` — field; `batch_exec_agent.go:49`; exported; 1 use(s).
- `batchAgentRunRequest.Input` — field; `batch_exec_agent.go:24`; exported; 1 use(s).
- `batchAgentRunRequest.MessageID` — field; `batch_exec_agent.go:28`; exported; 3 use(s).
- `batchAgentRunRequest.NodeID` — field; `batch_exec_agent.go:20`; exported; 1 use(s).
- `batchAgentRunRequest.Policy` — field; `batch_exec_agent.go:30`; exported; 3 use(s).
- `batchAgentRunRequest.QuestionRoute` — field; `batch_exec_agent.go:36`; exported; 2 use(s).
- `batchAgentRunRequest.RegistryID` — field; `batch_exec_agent.go:27`; exported; 4 use(s).
- `batchAgentRunRequest.SelectedRoleContract` — field; `batch_exec_agent.go:31`; exported; 2 use(s).
- `batchAgentRunRequest.SessionID` — field; `batch_exec_agent.go:22`; exported; 2 use(s).
- `batchAgentRunRequest.Tool` — field; `batch_exec_agent.go:42`; exported; 4 use(s).
- `batchAgentRunRequest.ToolCallID` — field; `batch_exec_agent.go:23`; exported; 2 use(s).
- `batchAgentTurn` — type; `batch_exec_agent.go:74`; private; 2 use(s).
- `batchAgentTurnResult` — type; `batch_exec_agent.go:65`; private; 2 use(s).
- `batchAgentTurnResult.Response` — field; `batch_exec_agent.go:66`; exported; 1 use(s).
- `batchArgvFamily` — type; `batch_exec_argv.go:17`; private; 1 use(s).
- `batchCanonicalInput` — func; `batch_invocation.go:147`; private; 1 use(s).
- `batchControlTarget` — func; `batch_exec_generic.go:86`; private; 3 use(s).
- `batchDeliveryChoice` — type; `batch_invocation.go:89`; private; 5 use(s).
- `batchDeliveryChoice.Mode` — field; `batch_invocation.go:90`; exported; 4 use(s).
- `batchDeliveryChoice.Window` — field; `batch_invocation.go:91`; exported; 2 use(s).
- `batchDeliveryDetached` — const; `batch_invocation.go:26`; private; 3 use(s).
- `batchDeliverySync` — const; `batch_invocation.go:24`; private; 1 use(s).
- `batchDiskProviderFrom` — func; `batch_invocation.go:301`; private; 2 use(s).
- `batchEscapeAuto` — const; `batch_invocation.go:33`; private; 2 use(s).
- `batchEscapeNever` — const; `batch_invocation.go:32`; private; 2 use(s).
- `batchExecutor.decorateExecution` — field; `batch_exec.go:124`; private; 1 use(s).
- `batchInvocationHash` — func; `batch_invocation.go:161`; private; 1 use(s).
- `batchKindAgent` — const; `batch_exec.go:25`; private; 4 use(s).
- `batchKindArgv` — const; `batch_exec.go:24`; private; 3 use(s).
- `batchKindGeneric` — const; `batch_exec.go:21`; private; 3 use(s).
- `batchKindMCP` — const; `batch_exec.go:22`; private; 3 use(s).
- `batchKindShell` — const; `batch_exec.go:23`; private; 3 use(s).
- `batchLeafEnv` — type; `batch_exec.go:78`; private; 3 use(s).
- `batchLeafEnv.DiskBindings` — field; `batch_exec.go:82`; exported; 2 use(s).
- `batchLeafEnv.Ingress` — field; `batch_exec.go:89`; exported; 2 use(s).
- `batchLeafEnv.Leaves` — field; `batch_exec.go:81`; exported; 2 use(s).
- `batchLeafEnv.OutputCache` — field; `batch_exec.go:79`; exported; 1 use(s).
- `batchLeafFamily` — type; `batch_exec.go:95`; private; 2 use(s).
- `batchLeafPayload` — type; `batch_exec.go:33`; private; 5 use(s).
- `batchLeafPayload.AgentDepth` — field; `batch_exec.go:66`; exported; 1 use(s).
- `batchLeafPayload.CallCredentials` — field; `batch_exec.go:43`; exported; 1 use(s).
- `batchLeafPayload.Input` — field; `batch_exec.go:37`; exported; 4 use(s).
- `batchLeafPayload.Policy` — field; `batch_exec.go:39`; exported; 6 use(s).
- `batchLeafPayload.QuestionRoute` — field; `batch_exec.go:51`; exported; 1 use(s).
- `batchLeafPayload.RegistryID` — field; `batch_exec.go:38`; exported; 1 use(s).
- `batchLeafPayload.SelectedRoleContract` — field; `batch_exec.go:42`; exported; 8 use(s).
- `batchLeafPayload.SessionID` — field; `batch_exec.go:36`; exported; 2 use(s).
- `batchLeafPayload.Timeout` — field; `batch_exec.go:64`; exported; 1 use(s).
- `batchLeafPayload.Tool` — field; `batch_exec.go:34`; exported; 5 use(s).
- `batchLeafPayload.ToolCallID` — field; `batch_exec.go:35`; exported; 1 use(s).
- `batchLeafPolicyContext` — type; `batch_invocation.go:67`; private; 3 use(s).
- `batchLeafPolicyContext.CustomDisk` — field; `batch_invocation.go:74`; exported; 2 use(s).
- `batchLeafPolicyContext.DiskBindingID` — field; `batch_invocation.go:80`; exported; 9 use(s).
- `batchLeafPolicyContext.DiskProvider` — field; `batch_invocation.go:75`; exported; 2 use(s).
- `batchLeafPolicyContext.MessageID` — field; `batch_invocation.go:69`; exported; 1 use(s).
- `batchLeafPolicyContext.ModelName` — field; `batch_invocation.go:71`; exported; 1 use(s).
- `batchLeafPolicyContext.RegistryID` — field; `batch_invocation.go:68`; exported; 4 use(s).
- `batchLeafPolicyContext.RestrictedToolset` — field; `batch_invocation.go:81`; exported; 5 use(s).
- `batchLeafPolicyContext.RoleContract` — field; `batch_invocation.go:85`; exported; 10 use(s).
- `batchLeafPolicyContext.SupportsImages` — field; `batch_invocation.go:70`; exported; 1 use(s).
- `batchLeafTimeoutSpec` — type; `batch_exec.go:73`; private; 1 use(s).
- `batchLeafTimeoutSpec.Kind` — field; `batch_exec.go:74`; exported; 1 use(s).
- `batchLeafTimeoutSpec.Seconds` — field; `batch_exec.go:75`; exported; 1 use(s).
- `batchModelControl` — type; `batch_model_tools.go:33`; private; 1 use(s).
- `batchOutputCache` — type; `batch_terminal_output.go:46`; private; 3 use(s).
- `batchParseTimeoutParam` — func; `batch_timeout_parse.go:69`; private; 1 use(s).
- `batchShellFamily` — type; `batch_exec_shell.go:24`; private; 1 use(s).
- `batchTimeoutIntent.Duration` — field; `batch_timeout_parse.go:27`; exported; 1 use(s).
- `batchTimeoutIntent.Kind` — field; `batch_timeout_parse.go:25`; exported; 1 use(s).
- `batchTimeoutIntent.Seconds` — field; `batch_timeout_parse.go:26`; exported; 1 use(s).
- `batchTimeoutTerminateAndWake` — const; `batch_timeout_parse.go:36`; private; 1 use(s).
- `batchTimeoutWakeOnly` — const; `batch_timeout_parse.go:33`; private; 1 use(s).
- `errBatchAttachNotWired` — var; `batch_invocation.go:53`; private; 2 use(s).
- `errBatchDeliveryNotWired` — var; `batch_invocation.go:51`; private; 1 use(s).
- `errBatchInvocationMismatch` — var; `batch_invocation.go:50`; private; 1 use(s).
- `errBatchLeavesNotWired` — var; `batch_invocation.go:54`; private; 1 use(s).
- `errBatchMissingCallID` — var; `batch_invocation.go:40`; private; 1 use(s).
- `errBatchMissingRegistryID` — var; `batch_invocation.go:41`; private; 1 use(s).
- `errBatchNilLeaf` — var; `batch_invocation.go:48`; private; 1 use(s).
- `errBatchProfileNotWired` — var; `batch_invocation.go:55`; private; 1 use(s).
- `errBatchRunnerNotWired` — var; `batch_invocation.go:52`; private; 2 use(s).
- `errBatchSubmitBeforeInvalid` — var; `batch_invocation.go:56`; private; 1 use(s).
- `newBatchAgentFamily` — func; `batch_exec_agent.go:143`; private; 1 use(s).
- `newBatchExecutor` — func; `batch_exec.go:129`; private; 1 use(s).
- `newBatchGenericFamily` — func; `batch_exec_generic.go:17`; private; 1 use(s).
- `newBatchOutputCache` — func; `batch_terminal_output.go:54`; private; 1 use(s).
- `pctCapturedAgentTool.pctRegister` — method; `batch_child_tool_scope.go:84`; private; 1 use(s).
- `pctCapturedScopeFromTool` — func; `batch_child_tool_scope.go:175`; private; 1 use(s).
- `pctCapturedToolFrom` — func; `batch_child_tool_scope.go:148`; private; 1 use(s).
- `pctChildToolScope` — type; `batch_child_tool_scope.go:13`; private; 1 use(s).
- `pctChildToolScope.pctRetain` — method; `batch_child_tool_scope.go:24`; private; 1 use(s).
- `pctDelegationHandoff` — type; `batch_child_tool_scope.go:192`; private; 1 use(s).
- `pctDelegationHandoff.pctAccepted` — method; `batch_child_tool_scope.go:208`; private; 1 use(s).
- `pctDelegationHandoff.pctChild` — field; `batch_child_tool_scope.go:195`; private; 1 use(s).
- `pctDelegationHandoff.pctRelease` — method; `batch_child_tool_scope.go:272`; private; 23 use(s).
- `pctDelegationHandoff.pctReleasePending` — method; `batch_child_tool_scope.go:246`; private; 2 use(s).
- `pctDelegationHandoff.pctReleaseUnstarted` — method; `batch_child_tool_scope.go:250`; private; 1 use(s).
- `pctDelegationHandoff.releases` — field; `batch_child_tool_scope.go:194`; private; 1 use(s).
- `pctDelegationHandoffKey` — type; `batch_child_tool_scope.go:11`; private; 2 use(s).
- `pctHandoffFrom` — func; `batch_child_tool_scope.go:290`; private; 7 use(s).
- `pctInvocationKey` — func; `batch_child_tool_scope.go:182`; private; 1 use(s).
- `pctNewDelegationHandoff` — func; `batch_child_tool_scope.go:204`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → clibridge</code> — 2 declarations, 2 uses</summary>

- `newCLIToolBridgeForContext` — func; `cli_tool_bridge.go:27`; private; 1 use(s).
- `pinnedInvoke` — type; `cli_tool_bridge.go:14`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → coord</code> — 11 declarations, 30 uses</summary>

- `BindCoordinatorBatchRuntimeBindings` — func; `batch_glue.go:84`; exported; 1 use(s).
- `callCredentialsFrom` — func; `credentials.go:41`; private; 2 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 13 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 1 use(s).
- `coordinator.background` — field; `coordinator.go:113`; private; 2 use(s).
- `coordinator.batchDriverClaimer` — field; `coordinator.go:155`; private; 1 use(s).
- `coordinator.diskCaps` — field; `coordinator.go:161`; private; 4 use(s).
- `coordinator.execReqs` — field; `coordinator.go:165`; private; 2 use(s).
- `coordinator.inlineWindowDisabled` — field; `coordinator.go:117`; private; 2 use(s).
- `coordinator.noticeStore` — field; `coordinator.go:145`; private; 1 use(s).
- `withCallCredentials` — func; `credentials.go:37`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → delegation</code> — 5 declarations, 8 uses</summary>

- `DefaultAgentDepth` — const; `delegation_depth.go:12`; exported; 2 use(s).
- `MaxAgentDepthForResume` — const; `delegation_depth.go:13`; exported; 1 use(s).
- `WithAgentDepth` — func; `delegation_depth.go:37`; exported; 1 use(s).
- `requestedRoleSlot` — func; `delegation_role_models.go:19`; private; 1 use(s).
- `validateAgentRoleInput` — func; `delegation_role.go:9`; private; 3 use(s).

</details>

<details>
<summary><code>batchinst → drainctl</code> — 6 declarations, 8 uses</summary>

- `CLIScopeState` — type; `coordinator_reaction_source.go:46`; exported; 2 use(s).
- `CLIScopeState.Drain` — field; `coordinator_reaction_source.go:51`; exported; 2 use(s).
- `CLIScopeState.WorkOpen` — field; `coordinator_reaction_source.go:49`; exported; 1 use(s).
- `DrainOwed` — const; `coordinator_reaction_source.go:26`; exported; 1 use(s).
- `DrainPaced` — const; `coordinator_reaction_source.go:32`; exported; 1 use(s).
- `arbiter.clearSleepAll` — method; `turn_arbiter_state.go:191`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → finalize</code> — 1 declarations, 1 uses</summary>

- `ErrFinalizationScopeChanged` — var; `finalization_publish.go:13`; exported; 1 use(s).

</details>

<details>
<summary><code>batchinst → modelkit</code> — 7 declarations, 19 uses</summary>

- `AgentToolName` — const; `agent_tool.go:35`; exported; 9 use(s).
- `ResolveRoleContract` — func; `custom_role_runtime.go:14`; exported; 3 use(s).
- `WithCallRoleContract` — func; `custom_role_runtime.go:204`; exported; 1 use(s).
- `WithSelectedRoleContract` — func; `custom_role_runtime.go:192`; exported; 1 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 3 use(s).
- `inheritedRoleCeilings` — func; `custom_role_runtime.go:48`; private; 1 use(s).
- `workerToolNames` — var; `coordinator_tools.go:193`; private; 1 use(s).

</details>

<details>
<summary><code>batchinst → toolwrap</code> — 18 declarations, 48 uses</summary>

- `ErrRoleToolDenied` — var; `role_tool_policy.go:14`; exported; 2 use(s).
- `guardRoleToolCall` — func; `role_tool_policy.go:91`; private; 1 use(s).
- `newPolicyToolBuildForContext` — func; `policy_leaf.go:38`; private; 1 use(s).
- `policyLeafEntry` — type; `policy_leaf.go:24`; private; 1 use(s).
- `policyLeafEntry.tool` — field; `policy_leaf.go:26`; private; 2 use(s).
- `policyLeafRegistry` — type; `policy_leaf.go:29`; private; 9 use(s).
- `policyLeafRegistry.entries` — field; `policy_leaf.go:30`; private; 4 use(s).
- `policyLeafRegistry.resolve` — method; `policy_leaf.go:80`; private; 3 use(s).
- `policyToolBuild` — type; `policy_leaf.go:17`; private; 5 use(s).
- `policyToolBuild.leaves` — field; `policy_leaf.go:19`; private; 3 use(s).
- `policyToolBuild.registryID` — field; `policy_leaf.go:20`; private; 1 use(s).
- `policyToolBuild.tools` — field; `policy_leaf.go:18`; private; 8 use(s).
- `roleContractAllowsName` — func; `role_tool_policy.go:49`; private; 2 use(s).
- `roleContractForToolBuild` — func; `policy_leaf.go:136`; private; 1 use(s).
- `roleContractFromContext` — func; `role_tool_policy.go:39`; private; 1 use(s).
- `roleContractsAllowName` — func; `policy_leaf.go:155`; private; 1 use(s).
- `roleContractsForTools` — func; `policy_leaf.go:203`; private; 1 use(s).
- `withRoleToolContract` — func; `role_tool_policy.go:18`; private; 2 use(s).

</details>

<details>
<summary><code>batchinst → wakectl</code> — 4 declarations, 4 uses</summary>

- `coordinator.wakeSchedulerFor` — method; `coordinator_wake_tools.go:37`; private; 1 use(s).
- `wakeScheduler.Notify` — method; `wake_scheduler.go:164`; exported; 1 use(s).
- `wakeScheduler.now` — field; `wake_scheduler.go:50`; private; 1 use(s).
- `wakeScheduler.store` — field; `wake_scheduler.go:43`; private; 1 use(s).

</details>

<details>
<summary><code>batchrun → batchflow</code> — 15 declarations, 19 uses</summary>

- `BatchCLITask` — type; `batch_cli.go:19`; exported; 1 use(s).
- `BatchCLITask.Input` — field; `batch_cli.go:21`; exported; 1 use(s).
- `BatchCLITask.Tool` — field; `batch_cli.go:20`; exported; 1 use(s).
- `batchCredentialLeaf` — type; `batch_credentials_escrow.go:29`; private; 1 use(s).
- `batchCredentialLeaf.AgentTool` — field; `batch_credentials_escrow.go:30`; exported; 1 use(s).
- `batchDeliveryDeps.ArmSupervision` — field; `batch_delivery.go:91`; exported; 2 use(s).
- `batchDeliveryImpl` — type; `batch_delivery.go:103`; private; 1 use(s).
- `batchDeliveryImpl.deps` — field; `batch_delivery.go:104`; private; 2 use(s).
- `batchQuestionRouteFrom` — func; `batch_question_route.go:34`; private; 1 use(s).
- `batchRootLeaseStore` — type; `batch_root_lease.go:21`; private; 2 use(s).
- `batchRootLeaseStore.acquire` — method; `batch_root_lease.go:62`; private; 1 use(s).
- `batchRootLeaseStore.adopt` — method; `batch_root_lease.go:98`; private; 1 use(s).
- `batchRootLeaseStore.matches` — method; `batch_root_lease.go:135`; private; 2 use(s).
- `batchRootLeaseStore.release` — method; `batch_root_lease.go:117`; private; 1 use(s).
- `stampBatchCallCredentials` — func; `batch_credentials_marker.go:13`; private; 1 use(s).

</details>

<details>
<summary><code>batchrun → batchinst</code> — 101 declarations, 154 uses</summary>

- `AsyncCompletion` — type; `batch_rehome_async_completion.go:11`; exported; 1 use(s).
- `AsyncCompletion.Content` — field; `batch_rehome_async_completion.go:15`; exported; 2 use(s).
- `AsyncCompletion.IsError` — field; `batch_rehome_async_completion.go:16`; exported; 2 use(s).
- `AsyncCompletion.SessionID` — field; `batch_rehome_async_completion.go:12`; exported; 1 use(s).
- `AsyncCompletion.ToolCallID` — field; `batch_rehome_async_completion.go:13`; exported; 1 use(s).
- `AsyncCompletion.ToolName` — field; `batch_rehome_async_completion.go:14`; exported; 1 use(s).
- `BatchChildReactionFacts` — type; `batch_install_policy.go:158`; exported; 2 use(s).
- `BatchChildReactionFacts.ChildFinalOutcome` — method; `batch_install_policy.go:173`; exported; 1 use(s).
- `BatchLeafRegistry` — alias; `batch_install_ingress.go:24`; exported; 1 use(s).
- `BatchModelControl` — type; `batch_install_ingress.go:26`; exported; 2 use(s).
- `BatchProfile` — type; `batch_profile.go:53`; exported; 2 use(s).
- `BatchWorkIngress` — type; `batch_install_ingress.go:37`; exported; 3 use(s).
- `BatchWorkIngress.Registry` — method; `batch_install_ingress.go:46`; exported; 1 use(s).
- `DiskProviderRegistry` — alias; `disk_provider_binding.go:43`; exported; 1 use(s).
- `ErrBatchAgentNotHeld` — var; `batch_scope.go:49`; exported; 1 use(s).
- `ErrBatchAgentStaleGeneration` — var; `batch_scope.go:55`; exported; 1 use(s).
- `ErrBatchAgentStopped` — var; `batch_scope.go:52`; exported; 2 use(s).
- `ErrBatchQuestionNoRoute` — var; `batch_scope.go:65`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 12 use(s).
- `ValidateAdd` — func; `batch_admission.go:208`; exported; 1 use(s).
- `ValidateSubmit` — func; `batch_admission.go:198`; exported; 1 use(s).
- `admitBatchRoleContracts` — func; `batch_role_admission.go:97`; private; 2 use(s).
- `batchAgentHeldQuestion.answer` — field; `batch_scope.go:30`; private; 1 use(s).
- `batchAgentHeldQuestion.depth` — field; `batch_scope.go:28`; private; 1 use(s).
- `batchAgentHeldQuestion.sequence` — field; `batch_scope.go:29`; private; 2 use(s).
- `batchAgentInjectMessage` — type; `batch_scope.go:38`; private; 1 use(s).
- `batchAgentInjectMessage.Sequence` — field; `batch_scope.go:40`; exported; 1 use(s).
- `batchAgentScope` — type; `batch_scope.go:14`; private; 1 use(s).
- `batchAgentScope.beginStop` — method; `batch_scope.go:89`; private; 1 use(s).
- `batchAgentScope.closed` — field; `batch_scope.go:19`; private; 1 use(s).
- `batchAgentScope.closedNow` — method; `batch_scope.go:185`; private; 2 use(s).
- `batchAgentScope.deliverWithCommit` — method; `batch_scope.go:159`; private; 1 use(s).
- `batchAgentScope.descendants` — method; `batch_scope.go:113`; private; 1 use(s).
- `batchAgentScope.discardHold` — method; `batch_scope.go:141`; private; 2 use(s).
- `batchAgentScope.held` — field; `batch_scope.go:18`; private; 4 use(s).
- `batchAgentScope.hold` — method; `batch_scope.go:123`; private; 1 use(s).
- `batchAgentScope.markClosed` — method; `batch_scope.go:192`; private; 1 use(s).
- `batchAgentScope.mu` — field; `batch_scope.go:15`; private; 4 use(s).
- `batchAgentScope.questionSequence` — field; `batch_scope.go:22`; private; 2 use(s).
- `batchAgentScope.signal` — method; `batch_scope.go:81`; private; 1 use(s).
- `batchAgentScope.stop` — method; `batch_scope.go:97`; private; 1 use(s).
- `batchAgentScope.stopped` — field; `batch_scope.go:17`; private; 1 use(s).
- `batchAgentScope.turnEnded` — method; `batch_scope.go:104`; private; 1 use(s).
- `batchAgentScope.waitChange` — method; `batch_scope.go:203`; private; 1 use(s).
- `batchBoundRunnerHandle` — type; `batch_runtime.go:25`; private; 1 use(s).
- `batchBoundRunnerHandle.SubmitBound` — method; `batch_runtime.go:26`; exported; 1 use(s).
- `batchBridgeInvocation` — type; `batch_install_policy.go:63`; private; 1 use(s).
- `batchBridgeInvocation.AgentDepth` — field; `batch_install_policy.go:66`; exported; 1 use(s).
- `batchBridgeInvocation.AgentDepthSet` — field; `batch_install_policy.go:67`; exported; 1 use(s).
- `batchBridgeInvocation.MessageID` — field; `batch_install_policy.go:65`; exported; 1 use(s).
- `batchBridgeInvocation.RegistryID` — field; `batch_install_policy.go:64`; exported; 1 use(s).
- `batchChildQuestionNotice` — type; `batch_install_question.go:15`; private; 1 use(s).
- `batchChildQuestionNotice.ChildSessionID` — field; `batch_install_question.go:16`; exported; 1 use(s).
- `batchChildQuestionNotice.Depth` — field; `batch_install_question.go:19`; exported; 1 use(s).
- `batchChildQuestionNotice.NodeID` — field; `batch_install_question.go:17`; exported; 1 use(s).
- `batchChildQuestionNotice.Owner` — field; `batch_install_question.go:16`; exported; 1 use(s).
- `batchChildQuestionNotice.Question` — field; `batch_install_question.go:21`; exported; 1 use(s).
- `batchChildQuestionNotice.Sequence` — field; `batch_install_question.go:20`; exported; 1 use(s).
- `batchChildQuestionNotice.Token` — field; `batch_install_question.go:18`; exported; 1 use(s).
- `batchChildQuestionPublisher` — type; `batch_install_question.go:23`; private; 1 use(s).
- `batchChildQuestionPublisher.PublishChildQuestion` — method; `batch_install_question.go:24`; exported; 1 use(s).
- `batchDefaultMaxDepth` — const; `batch_runtime.go:107`; private; 2 use(s).
- `batchDefaultMaxNodes` — const; `batch_runtime.go:108`; private; 2 use(s).
- `batchDriverHints` — type; `batch_rehome_coord_runtime.go:28`; private; 1 use(s).
- `batchDriverHints.mu` — field; `batch_rehome_coord_runtime.go:29`; private; 4 use(s).
- `batchDriverHints.waiters` — field; `batch_rehome_coord_runtime.go:31`; private; 6 use(s).
- `batchGroupRanks` — var; `batch_profile.go:23`; private; 1 use(s).
- `batchLeafKind` — func; `batch_ingress.go:401`; private; 1 use(s).
- `batchMCPCapability` — type; `batch_admission.go:69`; private; 1 use(s).
- `batchMCPCapabilityResolver` — type; `batch_admission.go:76`; private; 5 use(s).
- `batchModelRunner` — type; `batch_runtime.go:33`; private; 2 use(s).
- `batchModelRunner.Add` — method; `batch_runtime.go:35`; exported; 1 use(s).
- `batchModelRunner.Output` — method; `batch_runtime.go:38`; exported; 1 use(s).
- `batchModelRunner.Snapshot` — method; `batch_runtime.go:36`; exported; 1 use(s).
- `batchModelRunner.Stop` — method; `batch_runtime.go:39`; exported; 1 use(s).
- `batchModelRunner.Submit` — method; `batch_runtime.go:34`; exported; 1 use(s).
- `batchRankContext` — type; `batch_admission.go:125`; private; 2 use(s).
- `batchRankContext.Rank` — field; `batch_admission.go:126`; exported; 2 use(s).
- `batchRankContext.Root` — field; `batch_admission.go:129`; exported; 1 use(s).
- `batchReleaseHandle` — type; `batch_runtime.go:29`; private; 1 use(s).
- `batchReleaseHandle.Release` — method; `batch_runtime.go:30`; exported; 1 use(s).
- `batchRootID` — func; `batch_runtime.go:207`; private; 1 use(s).
- `batchSpecNodes` — func; `batch_admission.go:153`; private; 2 use(s).
- `boundCallOptions` — func; `disk_provider_call.go:62`; private; 2 use(s).
- `classifyBatchLeaf` — func; `batch_admission.go:82`; private; 2 use(s).
- `configForBatchInvocation` — func; `batch_role_admission.go:30`; private; 1 use(s).
- `diskCapabilityManager.Lookup` — method; `disk_provider_binding.go:128`; exported; 2 use(s).
- `diskCapabilityManager.acquire` — method; `disk_provider_capability.go:103`; private; 1 use(s).
- `diskCapabilityManager.release` — method; `disk_provider_capability.go:159`; private; 1 use(s).
- `diskOwnerExecution` — const; `disk_provider_capability.go:36`; private; 1 use(s).
- `diskOwnerKey` — type; `disk_provider_capability.go:43`; private; 1 use(s).
- `diskOwnerKey.bindingID` — field; `disk_provider_capability.go:46`; private; 1 use(s).
- `diskOwnerKey.instance` — field; `disk_provider_capability.go:45`; private; 1 use(s).
- `diskOwnerKey.kind` — field; `disk_provider_capability.go:44`; private; 1 use(s).
- `diskOwnerKey.registryID` — field; `disk_provider_capability.go:49`; private; 1 use(s).
- `errBatchMCPCapabilityUnknown` — var; `batch_admission.go:21`; private; 2 use(s).
- `newBatchAgentScope` — func; `batch_scope.go:76`; private; 1 use(s).
- `roleContractInherits` — func; `batch_role_admission.go:201`; private; 1 use(s).
- `validateSelectedBatchRole` — func; `batch_role_admission.go:188`; private; 1 use(s).
- `withBatchBridgeInvocation` — func; `batch_install_policy.go:73`; private; 1 use(s).
- `withDiskProviderRegistry` — func; `disk_provider_binding.go:145`; private; 2 use(s).

</details>

<details>
<summary><code>batchrun → delegation</code> — 6 declarations, 22 uses</summary>

- `DefaultAgentDepth` — const; `delegation_depth.go:12`; exported; 13 use(s).
- `MaxAgentDepthForResume` — const; `delegation_depth.go:13`; exported; 1 use(s).
- `WithAgentDepth` — func; `delegation_depth.go:37`; exported; 2 use(s).
- `parentRoleConfigFrom` — func; `delegation_role_models.go:146`; private; 2 use(s).
- `parentRoleContractFrom` — func; `delegation_role_models.go:159`; private; 1 use(s).
- `validateAgentRoleInput` — func; `delegation_role.go:9`; private; 3 use(s).

</details>

<details>
<summary><code>batchrun → modelkit</code> — 6 declarations, 19 uses</summary>

- `AgentToolName` — const; `agent_tool.go:35`; exported; 8 use(s).
- `ResolveRoleContract` — func; `custom_role_runtime.go:14`; exported; 1 use(s).
- `WithCallRoleContract` — func; `custom_role_runtime.go:204`; exported; 1 use(s).
- `WithSelectedRoleContract` — func; `custom_role_runtime.go:192`; exported; 4 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 2 use(s).
- `roleContractAllowsTool` — func; `custom_role_runtime.go:163`; private; 3 use(s).

</details>

<details>
<summary><code>batchrun → toolwrap</code> — 15 declarations, 37 uses</summary>

- `guardRoleContractError` — func; `role_tool_policy.go:181`; private; 1 use(s).
- `hookedTool` — type; `hooked_tool.go:18`; private; 1 use(s).
- `hookedTool.inner` — field; `hooked_tool.go:19`; private; 1 use(s).
- `loggedTool` — type; `logged_tool.go:42`; private; 1 use(s).
- `loggedTool.inner` — field; `logged_tool.go:43`; private; 1 use(s).
- `policyLeafRegistry` — type; `policy_leaf.go:29`; private; 10 use(s).
- `policyLeafRegistry.hasToolset` — method; `policy_leaf.go:76`; private; 2 use(s).
- `policyLeafRegistry.resolve` — method; `policy_leaf.go:80`; private; 3 use(s).
- `policyLeafRegistry.resolveForContext` — method; `policy_leaf.go:84`; private; 1 use(s).
- `restrictedTool` — type; `restricted_tool.go:73`; private; 2 use(s).
- `restrictedTool.AgentTool` — field; `restricted_tool.go:74`; exported; 2 use(s).
- `roleGuardTool` — type; `role_tool_policy.go:160`; private; 2 use(s).
- `roleGuardTool.AgentTool` — field; `role_tool_policy.go:161`; exported; 2 use(s).
- `roleToolAllowed` — func; `role_tool_policy.go:73`; private; 1 use(s).
- `withRoleToolContract` — func; `role_tool_policy.go:18`; private; 7 use(s).

</details>

<details>
<summary><code>callbeat → agentrun</code> — 9 declarations, 40 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 7 use(s).
- `sessionAgent.cacheKeepAlive` — field; `agent.go:446`; private; 7 use(s).
- `sessionAgent.cacheKeepAliveGen` — field; `agent.go:453`; private; 2 use(s).
- `sessionAgent.cacheKeepAliveInFlight` — field; `agent.go:463`; private; 6 use(s).
- `sessionAgent.cacheKeepAliveMu` — field; `agent.go:449`; private; 11 use(s).
- `sessionAgent.coord` — field; `agent.go:240`; private; 2 use(s).
- `sessionAgent.runWg` — field; `agent.go:274`; private; 1 use(s).
- `sessionAgent.sessions` — field; `agent.go:320`; private; 3 use(s).
- `sessionAgent.tryAdmitRunWg` — method; `agent_control.go:143`; private; 1 use(s).

</details>

<details>
<summary><code>callbeat → modelkit</code> — 1 declarations, 1 uses</summary>

- `coordinator.checkLivePeakHours` — method; `coordinator_providers_auth.go:96`; private; 1 use(s).

</details>

<details>
<summary><code>callbeat → promptcache</code> — 3 declarations, 4 uses</summary>

- `cacheProfile.keepAliveEligible` — field; `cache_profile.go:35`; private; 1 use(s).
- `cacheProfile.ttl` — field; `cache_profile.go:38`; private; 1 use(s).
- `cacheProfileFor` — func; `cache_profile.go:113`; private; 2 use(s).

</details>

<details>
<summary><code>callbeat → sessionprompt</code> — 1 declarations, 1 uses</summary>

- `sessionHeaders` — func; `agent_prompt.go:49`; private; 1 use(s).

</details>

<details>
<summary><code>callbeat → usage</code> — 2 declarations, 2 uses</summary>

- `normalizeProviderUsage` — func; `usage_normalize.go:26`; private; 1 use(s).
- `sessionAgent.openrouterCost` — method; `agent_usage.go:51`; private; 1 use(s).

</details>

<details>
<summary><code>clibridge → batchflow</code> — 8 declarations, 11 uses</summary>

- `BatchCutoverFacts` — type; `batch_cutover_facts.go:43`; exported; 3 use(s).
- `BatchCutoverFacts.LiveDelegationNodes` — method; `batch_cutover_facts.go:77`; exported; 1 use(s).
- `BatchCutoverFacts.LiveHeldQuestions` — method; `batch_cutover_facts.go:107`; exported; 1 use(s).
- `BatchCutoverFacts.LiveNodes` — method; `batch_cutover_facts.go:72`; exported; 1 use(s).
- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `batchNodeStateLive` — func; `batch_cutover_facts_node.go:156`; private; 2 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 1 use(s).
- `unknownBatchCutoverFacts` — type; `batch_cutover_facts.go:119`; private; 1 use(s).

</details>

<details>
<summary><code>clibridge → batchinst</code> — 1 declarations, 1 uses</summary>

- `recoverBatchScopeDeadCandidates` — func; `batch_scope_recovery.go:99`; private; 1 use(s).

</details>

<details>
<summary><code>clibridge → coord</code> — 3 declarations, 10 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 5 use(s).
- `coordinator.background` — field; `coordinator.go:113`; private; 3 use(s).
- `coordinator.messages` — field; `coordinator.go:107`; private; 2 use(s).

</details>

<details>
<summary><code>clibridge → modelkit</code> — 2 declarations, 2 uses</summary>

- `classifyHardProviderLimit` — func; `provider_limit.go:100`; private; 1 use(s).
- `providerLimitHard` — const; `provider_limit.go:20`; private; 1 use(s).

</details>

<details>
<summary><code>clibridge → subagents</code> — 1 declarations, 1 uses</summary>

- `coordinator.agentFor` — method; `coordinator_subagent_drivers.go:237`; private; 1 use(s).

</details>

<details>
<summary><code>clibridge → toolwrap</code> — 4 declarations, 9 uses</summary>

- `guardRoleToolCall` — func; `role_tool_policy.go:91`; private; 1 use(s).
- `policyLeafRegistry` — type; `policy_leaf.go:29`; private; 3 use(s).
- `policyLeafRegistry.resolveForContext` — method; `policy_leaf.go:84`; private; 3 use(s).
- `roleToolAllowed` — func; `role_tool_policy.go:73`; private; 2 use(s).

</details>

<details>
<summary><code>coord → agentrun</code> — 2 declarations, 4 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 2 use(s).
- `sessionAgent.bindDiskCaps` — method; `agent.go:488`; private; 2 use(s).

</details>

<details>
<summary><code>coord → batchflow</code> — 46 declarations, 67 uses</summary>

- `BatchAwaitParams` — type; `batch_await.go:67`; exported; 1 use(s).
- `BatchAwaitParams.Capture` — field; `batch_await.go:71`; exported; 1 use(s).
- `BatchAwaitParams.HeldQuestions` — field; `batch_await.go:94`; exported; 1 use(s).
- `BatchAwaitParams.Hints` — field; `batch_await.go:75`; exported; 1 use(s).
- `BatchAwaitParams.Levels` — field; `batch_await.go:73`; exported; 1 use(s).
- `BatchAwaitParams.MaxWaitSeconds` — field; `batch_await.go:69`; exported; 1 use(s).
- `BatchAwaitParams.Mode` — field; `batch_await.go:68`; exported; 1 use(s).
- `BatchAwaitParams.NotifyModeOf` — field; `batch_await.go:78`; exported; 1 use(s).
- `BatchAwaitResult.FinishedCount` — field; `batch_await.go:60`; exported; 1 use(s).
- `BatchAwaitResult.Remaining` — field; `batch_await.go:62`; exported; 2 use(s).
- `BatchAwaitResult.TimedOut` — field; `batch_await.go:59`; exported; 1 use(s).
- `BatchAwaitUnit.Ref` — field; `batch_await.go:44`; exported; 2 use(s).
- `BatchAwaitUnit.ToolCallID` — field; `batch_await.go:45`; exported; 1 use(s).
- `BatchAwaitWorker` — func; `batch_await.go:107`; exported; 1 use(s).
- `BatchCLIBuilder` — type; `batch_cli.go:44`; exported; 2 use(s).
- `BatchCLIBuilder.coord` — field; `batch_cli.go:45`; private; 1 use(s).
- `BatchCLIBuilder.ingress` — field; `batch_cli.go:46`; private; 1 use(s).
- `BatchCLIBuilder.questionRoute` — field; `batch_cli.go:47`; private; 1 use(s).
- `BatchCutoverFacts` — type; `batch_cutover_facts.go:43`; exported; 1 use(s).
- `BatchCutoverFacts.LiveDelegationChildOf` — method; `batch_cutover_facts.go:70`; exported; 1 use(s).
- `BatchCutoverFacts.LiveDelegationNodes` — method; `batch_cutover_facts.go:77`; exported; 2 use(s).
- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `BatchCutoverFacts.ReactedFailedTexts` — method; `batch_cutover_facts.go:57`; exported; 1 use(s).
- `BatchDriverClaimer` — type; `batch_cutover_driver.go:16`; exported; 3 use(s).
- `BatchHeldQuestionRoute` — type; `batch_held_question_route.go:30`; exported; 2 use(s).
- `BatchHeldQuestionRoute.ChildSessionID` — field; `batch_held_question_route.go:32`; exported; 1 use(s).
- `BatchHeldQuestionRoute.Depth` — field; `batch_held_question_route.go:35`; exported; 1 use(s).
- `BatchHeldQuestionRoute.NodeID` — field; `batch_held_question_route.go:33`; exported; 1 use(s).
- `BatchHeldQuestionRoute.Sequence` — field; `batch_held_question_route.go:36`; exported; 1 use(s).
- `BatchHeldQuestionRoute.Token` — field; `batch_held_question_route.go:34`; exported; 1 use(s).
- `BatchHeldQuestionSource` — type; `batch_held_question_route.go:213`; exported; 2 use(s).
- `BatchHeldQuestionSource.ActionableHeldQuestions` — method; `batch_held_question_route.go:214`; exported; 2 use(s).
- `BindCoordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:209`; exported; 1 use(s).
- `armBatchQuestionTurnContext` — func; `batch_question_route.go:87`; private; 1 use(s).
- `batchAwaitTerminalState` — func; `batch_await.go:337`; private; 2 use(s).
- `batchCancelOutcome.Err` — field; `batch_cancel.go:22`; exported; 2 use(s).
- `batchNodeStateLive` — func; `batch_cutover_facts_node.go:156`; private; 1 use(s).
- `batchNotifyAll` — const; `batch_await.go:37`; private; 1 use(s).
- `batchQuestionMarkerChild` — func; `batch_cutover_facts_node.go:430`; private; 2 use(s).
- `batchQuestionMarkerSequence` — func; `batch_held_question_route.go:49`; private; 2 use(s).
- `batchRerunRuntime` — func; `batch_cutover_rerun.go:48`; private; 1 use(s).
- `cancelBatchCall` — func; `batch_cancel.go:34`; private; 1 use(s).
- `coordinator.heldQuestionAwaitRefusal` — method; `batch_held_question_route.go:220`; private; 1 use(s).
- `coordinator.rebuiltChildQuestionState` — method; `batch_question_rebuild.go:9`; private; 2 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 7 use(s).
- `parseBatchQuestionMarker` — func; `batch_cutover_facts_node.go:394`; private; 2 use(s).

</details>

<details>
<summary><code>coord → batchinst</code> — 58 declarations, 117 uses</summary>

- `AsyncCompletion` — type; `batch_rehome_async_completion.go:11`; exported; 4 use(s).
- `AsyncCompletion.Content` — field; `batch_rehome_async_completion.go:15`; exported; 4 use(s).
- `AsyncCompletion.IsError` — field; `batch_rehome_async_completion.go:16`; exported; 3 use(s).
- `BatchChildReactionFacts` — type; `batch_install_policy.go:158`; exported; 1 use(s).
- `BatchModelControl` — type; `batch_install_ingress.go:26`; exported; 2 use(s).
- `BatchWorkIngress` — type; `batch_install_ingress.go:37`; exported; 3 use(s).
- `BindCoordinatorBatchControls` — func; `batch_install_ingress.go:159`; exported; 1 use(s).
- `DiskProviderRegistry` — alias; `disk_provider_binding.go:43`; exported; 4 use(s).
- `ErrBatchAgentStaleGeneration` — var; `batch_scope.go:55`; exported; 2 use(s).
- `ErrBatchQuestionCapabilityNotDurable` — var; `batch_scope.go:73`; exported; 2 use(s).
- `ErrCredentialBindingUnavailable` — var; `disk_provider_durable.go:40`; exported; 3 use(s).
- `ErrCredentialSetNotDurable` — var; `disk_provider_durable.go:45`; exported; 1 use(s).
- `ErrDiskProviderNotDurable` — var; `disk_provider_durable.go:29`; exported; 3 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 8 use(s).
- `admitBatchRoleContracts` — func; `batch_role_admission.go:97`; private; 2 use(s).
- `awaitTaskSummary` — func; `batch_rehome_runtime.go:16`; private; 1 use(s).
- `batchAgentInjectMessage` — type; `batch_scope.go:38`; private; 1 use(s).
- `batchAgentInjectMessage.Depth` — field; `batch_scope.go:39`; exported; 1 use(s).
- `batchAgentInjectMessage.Sequence` — field; `batch_scope.go:40`; exported; 1 use(s).
- `batchAgentInjectMessage.Text` — field; `batch_scope.go:41`; exported; 1 use(s).
- `batchCoordinatorBinding` — type; `batch_install_ingress.go:127`; private; 1 use(s).
- `batchCoordinatorBinding.controls` — field; `batch_install_ingress.go:129`; private; 1 use(s).
- `batchCoordinatorBinding.coord` — field; `batch_install_ingress.go:136`; private; 1 use(s).
- `batchCoordinatorBinding.diskBindings` — field; `batch_install_ingress.go:131`; private; 1 use(s).
- `batchCoordinatorBinding.ingress` — field; `batch_install_ingress.go:128`; private; 1 use(s).
- `batchCoordinatorBinding.live` — field; `batch_install_ingress.go:130`; private; 8 use(s).
- `batchDriverHints` — type; `batch_rehome_coord_runtime.go:28`; private; 1 use(s).
- `batchDriverHints.bumpHint` — method; `batch_rehome_coord_runtime.go:46`; private; 1 use(s).
- `batchDriverHints.isExternalDriver` — method; `batch_rehome_coord_runtime.go:130`; private; 1 use(s).
- `batchRoleContractsWereAdmitted` — func; `batch_role_admission.go:25`; private; 1 use(s).
- `batchSpecHasCustomRole` — func; `batch_role_admission.go:37`; private; 1 use(s).
- `bindExistingCallDiskBinding` — func; `disk_provider_call.go:113`; private; 1 use(s).
- `callCarriesDiskProvider` — func; `disk_provider_durable.go:50`; private; 2 use(s).
- `callCarriesNonDurableDependency` — func; `disk_provider_durable.go:58`; private; 1 use(s).
- `coordinator.PrepareContinuation` — method; `disk_provider_continuation.go:43`; exported; 3 use(s).
- `coordinator.cancelAwaitTimer` — method; `batch_rehome_runtime.go:39`; private; 1 use(s).
- `coordinator.clearSleepAll` — method; `batch_rehome_runtime.go:35`; private; 2 use(s).
- `coordinator.guardTurnDiskFence` — method; `disk_provider_continuation.go:200`; private; 1 use(s).
- `coordinatorBatchBindingFor` — func; `batch_install_ingress.go:164`; private; 3 use(s).
- `coordinatorBatchBindings` — var; `batch_install_ingress.go:145`; private; 1 use(s).
- `coordinatorBridgeFallback` — func; `batch_install_bridge.go:15`; private; 1 use(s).
- `errPscChildScopeUnavailable` — var; `batch_install_policy.go:197`; private; 2 use(s).
- `holdDurableContinuationOwner` — func; `disk_provider_continuation.go:170`; private; 2 use(s).
- `newBatchDriverHints` — func; `batch_rehome_coord_runtime.go:35`; private; 1 use(s).
- `preparedContinuation` — type; `disk_provider_continuation.go:27`; private; 2 use(s).
- `preparedContinuation.applyTo` — method; `disk_provider_continuation.go:155`; private; 1 use(s).
- `preparedContinuation.provider` — field; `disk_provider_continuation.go:29`; private; 1 use(s).
- `preparedContinuation.release` — field; `disk_provider_continuation.go:31`; private; 7 use(s).
- `preparedContinuation.requirement` — field; `disk_provider_continuation.go:28`; private; 2 use(s).
- `processBatchBinding` — var; `batch_install_ingress.go:148`; private; 1 use(s).
- `pscChildReactionFacts` — type; `batch_install_policy.go:201`; private; 1 use(s).
- `pscChildReactionFacts.outcome` — field; `batch_install_policy.go:203`; private; 1 use(s).
- `pscChildReactionFacts.scope` — field; `batch_install_policy.go:202`; private; 1 use(s).
- `releaseDurableContinuationOwner` — func; `disk_provider_continuation.go:185`; private; 5 use(s).
- `requirementFromCall` — func; `disk_provider_continuation.go:233`; private; 5 use(s).
- `setBatchLeafKindOverride` — func; `batch_ingress.go:426`; private; 1 use(s).
- `validateBatchRolePayloads` — func; `batch_role_admission.go:223`; private; 1 use(s).
- `validateBatchSpecShapeForRoleAdmission` — func; `batch_role_admission.go:64`; private; 1 use(s).

</details>

<details>
<summary><code>coord → batchrun</code> — 2 declarations, 2 uses</summary>

- `batchDriverHints.subscribe` — method; `batch_worker_hints.go:6`; private; 1 use(s).
- `batchKindMCP` — const; `batch_exec.go:22`; private; 1 use(s).

</details>

<details>
<summary><code>coord → callbeat</code> — 3 declarations, 5 uses</summary>

- `hbSourcePerCall` — const; `heartbeat_ctx.go:20`; private; 2 use(s).
- `hbSourceSessionOverride` — const; `heartbeat_ctx.go:19`; private; 2 use(s).
- `withModelSource` — func; `heartbeat_ctx.go:29`; private; 1 use(s).

</details>

<details>
<summary><code>coord → delegation</code> — 6 declarations, 10 uses</summary>

- `WithPersistedSessionAgentDepth` — func; `delegation_depth.go:64`; exported; 4 use(s).
- `coordinator.childParentSession` — method; `delegation_question.go:77`; private; 1 use(s).
- `coordinator.restoreRequestedDelegationCall` — method; `delegation_role_calls.go:47`; private; 1 use(s).
- `parentRoleConfigFrom` — func; `delegation_role_models.go:146`; private; 1 use(s).
- `parentRoleContractFrom` — func; `delegation_role_models.go:159`; private; 2 use(s).
- `withPersistedCallAgentDepth` — func; `delegation_depth.go:81`; private; 1 use(s).

</details>

<details>
<summary><code>coord → drainctl</code> — 16 declarations, 29 uses</summary>

- `CLIScopeState` — type; `coordinator_reaction_source.go:46`; exported; 3 use(s).
- `ReactionDebtSource` — type; `coordinator_reaction_source.go:73`; exported; 2 use(s).
- `ReactionDebtSource.CLIScope` — method; `coordinator_reaction_source.go:101`; exported; 1 use(s).
- `arbiter` — type; `turn_arbiter_state.go:48`; private; 1 use(s).
- `arbiter.autoResumesOf` — method; `turn_arbiter_state.go:207`; private; 1 use(s).
- `arbiter.claimAutoResumeSlot` — method; `turn_arbiter_state.go:223`; private; 1 use(s).
- `arbiter.resetForHumanMessage` — method; `turn_arbiter_state.go:312`; private; 1 use(s).
- `arbiter.suspend` — method; `turn_arbiter_state.go:162`; private; 1 use(s).
- `arbiter.suspended` — method; `turn_arbiter_state.go:168`; private; 1 use(s).
- `batchCutoverReactionStore` — func; `reaction_write_cutover.go:16`; private; 2 use(s).
- `callFromActive` — func; `agent_drain.go:75`; private; 2 use(s).
- `clearCancelRequest` — func; `cancel_flag.go:25`; private; 1 use(s).
- `coordinator.RunMaintenanceSweep` — method; `coordinator_reaction_source.go:199`; exported; 1 use(s).
- `coordinator.armAwait` — method; `await_tasks_era.go:46`; private; 1 use(s).
- `coordinator.drainRefused` — method; `drain_attempt.go:549`; private; 9 use(s).
- `coordinator.isDurableDelegationChild` — method; `coordinator_drain_policy.go:190`; private; 1 use(s).

</details>

<details>
<summary><code>coord → finalize</code> — 9 declarations, 9 uses</summary>

- `FinalizationWiringDeps` — type; `finalization_wiring.go:17`; exported; 1 use(s).
- `FinalizationWiringDeps.Enabled` — field; `finalization_wiring.go:18`; exported; 1 use(s).
- `coordinator.finalizationOnCancel` — method; `finalization_wiring.go:291`; private; 1 use(s).
- `coordinator.finalizationQueueDriverForRun` — method; `finalization_queue_reservation.go:138`; private; 1 use(s).
- `coordinator.restoreFinalizationQueuedCall` — method; `finalization_queue_call.go:10`; private; 1 use(s).
- `enqueueFinalizationCall` — func; `finalization_queue_call.go:62`; private; 1 use(s).
- `finalizationWiring` — type; `finalization_wiring.go:37`; private; 1 use(s).
- `finalizationWiring.poll` — field; `finalization_wiring.go:54`; private; 1 use(s).
- `newFinalizationWiring` — func; `finalization_wiring.go:61`; private; 1 use(s).

</details>

<details>
<summary><code>coord → mailbox</code> — 1 declarations, 8 uses</summary>

- `activeCallToken` — type; `mailbox_generation.go:14`; private; 8 use(s).

</details>

<details>
<summary><code>coord → modelkit</code> — 42 declarations, 113 uses</summary>

- `WithRoleContractCopy` — func; `custom_role_runtime.go:211`; exported; 1 use(s).
- `callModelRole` — func; `coordinator_models_role.go:83`; private; 1 use(s).
- `checkPeakHours` — func; `coordinator_providers_auth.go:125`; private; 2 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 4 use(s).
- `cloneRoleContractFromCallOptions` — func; `custom_role_runtime.go:152`; private; 2 use(s).
- `continuationPrompt` — func; `coordinator_retry_classify.go:30`; private; 1 use(s).
- `coordinator.applyModelOverrides` — method; `coordinator_models.go:358`; private; 4 use(s).
- `coordinator.buildAgent` — method; `coordinator_tools.go:28`; private; 1 use(s).
- `coordinator.buildModelsFromCfg` — method; `coordinator_models_build.go:92`; private; 2 use(s).
- `coordinator.buildProviderWithLiteralCredentials` — method; `coordinator_providers.go:788`; private; 1 use(s).
- `coordinator.isUnauthorized` — method; `coordinator_providers_auth.go:234`; private; 1 use(s).
- `coordinator.notifyRateLimitWaitContext` — method; `coordinator_rate_limit.go:223`; private; 1 use(s).
- `coordinator.pinCallTools` — method; `coordinator_models_call_tools.go:102`; private; 2 use(s).
- `coordinator.rebuildOnCurrentCredentials` — method; `coordinator_providers_auth.go:44`; private; 1 use(s).
- `coordinator.refreshExpiredToken` — method; `coordinator_providers_auth.go:29`; private; 1 use(s).
- `coordinator.rejectScopedCallOnCLIProvider` — method; `coordinator_tools.go:527`; private; 4 use(s).
- `coordinator.resolveSessionModels` — method; `coordinator_models.go:82`; private; 8 use(s).
- `coordinator.runWithUnauthorizedRetry` — method; `coordinator_providers_auth.go:197`; private; 1 use(s).
- `coordinator.shouldContinueTurn` — method; `coordinator_retry_classify.go:333`; private; 1 use(s).
- `coordinator.shouldRetryTurn` — method; `coordinator_retry_classify.go:263`; private; 1 use(s).
- `coordinator.waitRateLimit` — method; `coordinator_rate_limit.go:167`; private; 1 use(s).
- `currentOrSnapshotRoleModel` — func; `custom_role_runtime.go:131`; private; 1 use(s).
- `errRateLimitBudgetExhausted` — var; `coordinator_rate_limit.go:28`; private; 1 use(s).
- `isExactoSupported` — func; `coordinator_providers.go:930`; private; 1 use(s).
- `isRateLimitError` — func; `coordinator_rate_limit.go:42`; private; 1 use(s).
- `mergeCallOptions` — func; `coordinator_providers.go:451`; private; 4 use(s).
- `newBoundedModelPairCache` — func; `coordinator_model_cache.go:37`; private; 1 use(s).
- `rateLimitWaitFor` — func; `coordinator_rate_limit.go:126`; private; 1 use(s).
- `resolvedOverrides` — type; `coordinator_models.go:28`; private; 8 use(s).
- `resolvedOverrides.credentials` — field; `coordinator_models.go:62`; private; 2 use(s).
- `resolvedOverrides.fast` — field; `coordinator_models.go:30`; private; 3 use(s).
- `resolvedOverrides.fastSource` — field; `coordinator_models.go:34`; private; 4 use(s).
- `resolvedOverrides.persistFastModel` — field; `coordinator_models.go:77`; private; 1 use(s).
- `resolvedOverrides.persistSmartModel` — field; `coordinator_models.go:76`; private; 1 use(s).
- `resolvedOverrides.pin` — method; `coordinator_models.go:499`; private; 3 use(s).
- `resolvedOverrides.promptPrefix` — field; `coordinator_models.go:35`; private; 6 use(s).
- `resolvedOverrides.providerCfg` — field; `coordinator_models.go:56`; private; 9 use(s).
- `resolvedOverrides.smart` — field; `coordinator_models.go:29`; private; 10 use(s).
- `resolvedOverrides.smartSource` — field; `coordinator_models.go:33`; private; 8 use(s).
- `resolvedOverrides.systemPrompt` — field; `coordinator_models.go:36`; private; 5 use(s).
- `resolvedOverrides.tools` — field; `coordinator_models.go:73`; private; 1 use(s).
- `scopedCallOptionsRequireDistinctTools` — func; `coordinator_models_call_tools.go:61`; private; 1 use(s).

</details>

<details>
<summary><code>coord → sessionprompt</code> — 6 declarations, 11 uses</summary>

- `CallOriginFrom` — func; `agent_prompt.go:117`; exported; 2 use(s).
- `WithCallOrigin` — func; `agent_prompt.go:111`; exported; 1 use(s).
- `coordinator.sessionPromptForCall` — method; `coordinator_session_prompt.go:36`; private; 2 use(s).
- `coordinator.sweepSessionState` — method; `coordinator_session_state_sweep.go:80`; private; 1 use(s).
- `isDrainCallFrom` — func; `agent_prompt.go:96`; private; 3 use(s).
- `noticeFlagsFrom` — func; `agent_prompt.go:71`; private; 2 use(s).

</details>

<details>
<summary><code>coord → subagents</code> — 13 declarations, 26 uses</summary>

- `coordinator.IsSessionBusy` — method; `coordinator_background.go:216`; exported; 1 use(s).
- `coordinator.agentFor` — method; `coordinator_subagent_drivers.go:237`; private; 10 use(s).
- `coordinator.inheritBatchQuestionState` — method; `coordinator_subagents.go:659`; private; 1 use(s).
- `ctxMutex` — type; `coordinator_bgshell_cap.go:25`; private; 1 use(s).
- `discoverSkills` — func; `coordinator_subagents.go:700`; private; 1 use(s).
- `logTurnSkillUsage` — func; `coordinator_subagents.go:753`; private; 1 use(s).
- `newSubAgentDriverRegistry` — func; `coordinator_subagent_drivers.go:115`; private; 1 use(s).
- `subAgentDriver.call` — field; `coordinator_subagent_drivers.go:43`; private; 5 use(s).
- `subAgentDriver.callFor` — method; `coordinator_subagent_drivers.go:75`; private; 1 use(s).
- `subAgentDriverRegistry` — type; `coordinator_subagent_drivers.go:105`; private; 1 use(s).
- `subAgentDriverRegistry.allDriverAgents` — method; `coordinator_subagent_drivers.go:198`; private; 1 use(s).
- `subAgentDriverRegistry.childSessions` — method; `coordinator_subagent_drivers.go:156`; private; 1 use(s).
- `subAgentDriverRegistry.get` — method; `coordinator_subagent_drivers.go:145`; private; 1 use(s).

</details>

<details>
<summary><code>coord → turnstep</code> — 6 declarations, 14 uses</summary>

- `AutoTurnHolder` — type; `coordinator_turn_hold.go:10`; exported; 1 use(s).
- `WithAdmissionSetup` — func; `turn_admission.go:16`; exported; 2 use(s).
- `admissionSetupFrom` — func; `turn_admission.go:20`; private; 2 use(s).
- `newTurnAdmission` — func; `coordinator_turn_admission.go:36`; private; 3 use(s).
- `turnAdmission.wasQueued` — method; `coordinator_turn_admission.go:51`; private; 4 use(s).
- `withTurnAdmission` — func; `coordinator_turn_admission.go:66`; private; 2 use(s).

</details>

<details>
<summary><code>coord → usage</code> — 1 declarations, 1 uses</summary>

- `legacyPeakHoursBypassKey` — type; `peak_hours_request_gate.go:10`; private; 1 use(s).

</details>

<details>
<summary><code>coord → wakectl</code> — 4 declarations, 4 uses</summary>

- `coordinator.ScheduleWakeOnceDelay` — method; `coordinator_wake_tools.go:106`; exported; 1 use(s).
- `coordinator.StopWakeScheduler` — method; `wake_scheduler.go:105`; exported; 1 use(s).
- `coordinator.wakeSession` — method; `coordinator_wake.go:27`; private; 1 use(s).
- `wakeScheduler` — type; `wake_scheduler.go:42`; private; 1 use(s).

</details>

<details>
<summary><code>coord → watchdog</code> — 7 declarations, 7 uses</summary>

- `coordinator.callbackContextLocked` — method; `supervision.go:515`; private; 1 use(s).
- `coordinator.noteWorkStarted` — method; `supervision.go:154`; private; 1 use(s).
- `newSupervisionRegistry` — func; `supervision.go:139`; private; 1 use(s).
- `newTimeoutService` — func; `timeout_service.go:55`; private; 1 use(s).
- `supervisionRegistry` — type; `supervision.go:133`; private; 1 use(s).
- `timeoutService` — type; `timeout_service.go:47`; private; 1 use(s).
- `timeoutService.close` — method; `timeout_service.go:125`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → batchflow</code> — 3 declarations, 3 uses</summary>

- `BatchCutoverFacts.LiveDelegationNodes` — method; `batch_cutover_facts.go:77`; exported; 1 use(s).
- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → batchinst</code> — 1 declarations, 4 uses</summary>

- `ErrCredentialBindingUnavailable` — var; `disk_provider_durable.go:40`; exported; 4 use(s).

</details>

<details>
<summary><code>delegation → callbeat</code> — 3 declarations, 4 uses</summary>

- `hbSourcePerCall` — const; `heartbeat_ctx.go:20`; private; 2 use(s).
- `hbSourceSessionOverride` — const; `heartbeat_ctx.go:19`; private; 1 use(s).
- `hbSourceSlotLive` — const; `heartbeat_ctx.go:18`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → coord</code> — 8 declarations, 28 uses</summary>

- `callCredentialsFrom` — func; `credentials.go:41`; private; 2 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 7 use(s).
- `coordinator.buildCredentialModel` — method; `credentials.go:385`; private; 2 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 1 use(s).
- `coordinator.childLastActivity` — method; `coordinator_agent_control.go:82`; private; 1 use(s).
- `coordinator.drainCredentialBoundary` — method; `coordinator_agent_control.go:243`; private; 4 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 10 use(s).
- `withCallCredentials` — func; `credentials.go:37`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → drainctl</code> — 1 declarations, 1 uses</summary>

- `applyDrainModelOptions` — func; `agent_drain.go:85`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → modelkit</code> — 8 declarations, 14 uses</summary>

- `ResolveRoleContract` — func; `custom_role_runtime.go:14`; exported; 1 use(s).
- `WithCallRoleContract` — func; `custom_role_runtime.go:204`; exported; 1 use(s).
- `WithRoleContractCopy` — func; `custom_role_runtime.go:211`; exported; 4 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 3 use(s).
- `coordinator.buildModelsFromCfg` — method; `coordinator_models_build.go:92`; private; 2 use(s).
- `currentOrSnapshotRoleModel` — func; `custom_role_runtime.go:131`; private; 1 use(s).
- `getProviderOptions` — func; `coordinator_providers.go:109`; private; 1 use(s).
- `selectedRoleContractFrom` — func; `custom_role_runtime.go:198`; private; 1 use(s).

</details>

<details>
<summary><code>delegation → subagents</code> — 2 declarations, 4 uses</summary>

- `coordinator.agentFor` — method; `coordinator_subagent_drivers.go:237`; private; 1 use(s).
- `loadChildRoleContract` — func; `coordinator_subagents.go:51`; private; 3 use(s).

</details>

<details>
<summary><code>delegation → toolwrap</code> — 1 declarations, 1 uses</summary>

- `roleContractFromContext` — func; `role_tool_policy.go:39`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → agentrun</code> — 4 declarations, 25 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 8 use(s).
- `sessionAgent.coord` — field; `agent.go:240`; private; 13 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 2 use(s).
- `streamIdleTimeoutDefault` — const; `agent.go:65`; private; 2 use(s).

</details>

<details>
<summary><code>drainctl → batchflow</code> — 23 declarations, 39 uses</summary>

- `BatchAttemptRows.AtK` — field; `batch_cutover_facts.go:36`; exported; 1 use(s).
- `BatchAttemptRows.Max` — field; `batch_cutover_facts.go:35`; exported; 1 use(s).
- `BatchAttemptRows.Open` — field; `batch_cutover_facts.go:34`; exported; 2 use(s).
- `BatchAttemptRows.UnmappedAtK` — field; `batch_cutover_facts.go:37`; exported; 1 use(s).
- `BatchCutoverFacts.AttemptRows` — method; `batch_cutover_facts.go:80`; exported; 1 use(s).
- `BatchCutoverFacts.DebtToolCallIDs` — method; `batch_cutover_facts.go:89`; exported; 2 use(s).
- `BatchCutoverFacts.DelegationChildEver` — method; `batch_cutover_facts.go:67`; exported; 1 use(s).
- `BatchCutoverFacts.IncrementWakeAttempts` — method; `batch_cutover_facts.go:83`; exported; 1 use(s).
- `BatchCutoverFacts.LiveScopeForOwners` — method; `batch_cutover_facts.go:62`; exported; 1 use(s).
- `BatchCutoverFacts.PendingInclusiveDebtSummary` — method; `batch_cutover_facts.go:55`; exported; 1 use(s).
- `BatchCutoverFacts.PullBatchNotices` — method; `batch_cutover_facts.go:93`; exported; 1 use(s).
- `BatchCutoverFacts.PullPendingSessionNotices` — method; `batch_cutover_facts.go:99`; exported; 1 use(s).
- `BatchCutoverFacts.SelectedDebtExists` — method; `batch_cutover_facts.go:46`; exported; 3 use(s).
- `BatchCutoverFacts.SettleReactedFailedWithMarker` — method; `batch_cutover_facts.go:86`; exported; 1 use(s).
- `BatchDriverClaimer.ClaimSessionDriver` — method; `batch_cutover_driver.go:17`; exported; 1 use(s).
- `BatchDriverClaimer.ReleaseSessionDriver` — method; `batch_cutover_driver.go:18`; exported; 1 use(s).
- `BatchLiveScopeEntry.Live` — field; `batch_cutover_facts.go:24`; exported; 1 use(s).
- `coordinator.driverMarkerStore` — method; `batch_cutover_driver.go:25`; private; 2 use(s).
- `coordinator.readTurnFactsBatch` — method; `batch_cutover_facts.go:234`; private; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 11 use(s).
- `coordinatorBatchCutoverFactsBindings` — var; `batch_cutover_facts.go:203`; private; 1 use(s).
- `nodeBatchCutoverFacts` — type; `batch_cutover_facts_node.go:23`; private; 1 use(s).
- `nodeBatchCutoverFacts.store` — field; `batch_cutover_facts_node.go:24`; private; 2 use(s).

</details>

<details>
<summary><code>drainctl → batchinst</code> — 32 declarations, 48 uses</summary>

- `AsyncCompletion` — type; `batch_rehome_async_completion.go:11`; exported; 1 use(s).
- `AsyncCompletion.Cancelled` — field; `batch_rehome_async_completion.go:37`; exported; 1 use(s).
- `AsyncCompletion.Content` — field; `batch_rehome_async_completion.go:15`; exported; 1 use(s).
- `AsyncCompletion.Interrupted` — field; `batch_rehome_async_completion.go:42`; exported; 1 use(s).
- `AsyncCompletion.IsError` — field; `batch_rehome_async_completion.go:16`; exported; 1 use(s).
- `AsyncCompletion.Stopped` — field; `batch_rehome_async_completion.go:32`; exported; 1 use(s).
- `AsyncCompletion.TimedOut` — field; `batch_rehome_async_completion.go:23`; exported; 1 use(s).
- `AsyncCompletion.ToolCallID` — field; `batch_rehome_async_completion.go:13`; exported; 1 use(s).
- `AsyncCompletion.ToolName` — field; `batch_rehome_async_completion.go:14`; exported; 1 use(s).
- `ErrCredentialBindingUnavailable` — var; `disk_provider_durable.go:40`; exported; 1 use(s).
- `batchDriverHints.claimExternalDriver` — method; `batch_rehome_coord_runtime.go:112`; private; 1 use(s).
- `batchDriverHints.hintSeqOf` — method; `batch_rehome_coord_runtime.go:65`; private; 3 use(s).
- `batchDriverHints.isExternalDriver` — method; `batch_rehome_coord_runtime.go:130`; private; 2 use(s).
- `batchDriverHints.releaseExternalDriver` — method; `batch_rehome_coord_runtime.go:121`; private; 1 use(s).
- `batchDriverHints.waitForHint` — method; `batch_rehome_coord_runtime.go:78`; private; 1 use(s).
- `coordinator.PrepareContinuation` — method; `disk_provider_continuation.go:43`; exported; 2 use(s).
- `coordinator.cancelAwaitTimer` — method; `batch_rehome_runtime.go:39`; private; 4 use(s).
- `coordinator.clearSleepAll` — method; `batch_rehome_runtime.go:35`; private; 2 use(s).
- `coordinator.sessionNoticeStore` — method; `batch_rehome_coord_runtime.go:194`; private; 1 use(s).
- `drainPace` — type; `batch_rehome_drain_pace.go:9`; private; 2 use(s).
- `paceFreeNoTurn` — const; `batch_rehome_drain_pace.go:15`; private; 3 use(s).
- `pacePaidUnreacted` — const; `batch_rehome_drain_pace.go:17`; private; 3 use(s).
- `paceUncounted` — const; `batch_rehome_drain_pace.go:13`; private; 2 use(s).
- `preparedContinuation` — type; `disk_provider_continuation.go:27`; private; 1 use(s).
- `preparedContinuation.applyTo` — method; `disk_provider_continuation.go:155`; private; 2 use(s).
- `preparedContinuation.applyToOptions` — method; `disk_provider_continuation.go:142`; private; 1 use(s).
- `preparedContinuation.release` — field; `disk_provider_continuation.go:31`; private; 1 use(s).
- `preparedContinuation.requirement` — field; `disk_provider_continuation.go:28`; private; 2 use(s).
- `preparedContinuation.restorePolicyBaseline` — method; `disk_provider_continuation.go:97`; private; 1 use(s).
- `recoverBatchScopeDeadCandidates` — func; `batch_scope_recovery.go:99`; private; 1 use(s).
- `requirementFromCall` — func; `disk_provider_continuation.go:233`; private; 1 use(s).
- `stallToolStalled` — func; `batch_rehome_runtime.go:94`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → coord</code> — 18 declarations, 79 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 28 use(s).
- `coordinator.StartRecheckTicker` — method; `coordinator_recheck.go:152`; exported; 1 use(s).
- `coordinator.addToRecheckSet` — method; `coordinator_recheck.go:42`; private; 3 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 16 use(s).
- `coordinator.background` — field; `coordinator.go:113`; private; 2 use(s).
- `coordinator.buildCall` — method; `coordinator_run.go:67`; private; 1 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 1 use(s).
- `coordinator.drainCredentialBoundary` — method; `coordinator_agent_control.go:243`; private; 1 use(s).
- `coordinator.driverHints` — field; `coordinator.go:136`; private; 8 use(s).
- `coordinator.messages` — field; `coordinator.go:107`; private; 2 use(s).
- `coordinator.permissions` — field; `coordinator.go:108`; private; 1 use(s).
- `coordinator.persistentMode` — field; `coordinator.go:247`; private; 1 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 4 use(s).
- `coordinator.subAgentDrivers` — field; `coordinator.go:150`; private; 3 use(s).
- `coordinator.suspendAutoResume` — method; `coordinator.go:582`; private; 1 use(s).
- `errProviderPeakHours` — var; `coordinator.go:60`; private; 2 use(s).
- `maxConsecutiveAutoResumes` — const; `coordinator.go:102`; private; 1 use(s).
- `recheckPassIntervalNS` — var; `coordinator_recheck.go:25`; private; 3 use(s).

</details>

<details>
<summary><code>drainctl → delegation</code> — 3 declarations, 3 uses</summary>

- `coordinator.refreshRequestedDelegationCall` — method; `delegation_role_calls.go:10`; private; 1 use(s).
- `coordinator.refuseLostRequestedRoleDriver` — method; `delegation_role_calls.go:125`; private; 1 use(s).
- `restoreRootRoleContract` — func; `root_role_contract.go:42`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → modelkit</code> — 18 declarations, 22 uses</summary>

- `WithCallRoleContract` — func; `custom_role_runtime.go:204`; exported; 1 use(s).
- `WithRoleContractCopy` — func; `custom_role_runtime.go:211`; exported; 1 use(s).
- `classTerminal` — const; `coordinator_retry_classify.go:72`; private; 1 use(s).
- `classifyProviderError` — func; `coordinator_retry_classify.go:84`; private; 1 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 3 use(s).
- `coordinator.isUnauthorized` — method; `coordinator_providers_auth.go:234`; private; 1 use(s).
- `coordinator.publishReauthenticate` — method; `coordinator_providers_auth.go:55`; private; 1 use(s).
- `coordinator.rebuildInputs` — method; `coordinator_providers.go:506`; private; 1 use(s).
- `coordinator.rebuildOnCurrentCredentials` — method; `coordinator_providers_auth.go:44`; private; 1 use(s).
- `coordinator.refreshAfterUnauthorized` — method; `coordinator_providers_auth.go:73`; private; 1 use(s).
- `coordinator.refreshDrainModel` — method; `coordinator_models_role.go:143`; private; 1 use(s).
- `coordinator.refreshExpiredToken` — method; `coordinator_providers_auth.go:29`; private; 2 use(s).
- `coordinator.resolveSessionModels` — method; `coordinator_models.go:82`; private; 2 use(s).
- `getProviderOptions` — func; `coordinator_providers.go:109`; private; 1 use(s).
- `hasRefreshableCredential` — func; `coordinator_providers_auth.go:312`; private; 1 use(s).
- `isProviderClassifiable` — func; `coordinator_retry_classify.go:154`; private; 1 use(s).
- `redactNetworkURLs` — func; `coordinator_providers_network.go:74`; private; 1 use(s).
- `resolvedOverrides.providerCfg` — field; `coordinator_models.go:56`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → sessionprompt</code> — 1 declarations, 1 uses</summary>

- `isDrainCallFrom` — func; `agent_prompt.go:96`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → subagents</code> — 7 declarations, 13 uses</summary>

- `FormatAsyncCompletion` — func; `coordinator_background.go:35`; exported; 1 use(s).
- `subAgentDriver.call` — field; `coordinator_subagent_drivers.go:43`; private; 2 use(s).
- `subAgentDriver.callFor` — method; `coordinator_subagent_drivers.go:75`; private; 1 use(s).
- `subAgentDriver.generation` — field; `coordinator_subagent_drivers.go:70`; private; 1 use(s).
- `subAgentDriver.parentSessionID` — field; `coordinator_subagent_drivers.go:56`; private; 3 use(s).
- `subAgentDriver.slotRole` — field; `coordinator_subagent_drivers.go:59`; private; 2 use(s).
- `subAgentDriverRegistry.get` — method; `coordinator_subagent_drivers.go:145`; private; 3 use(s).

</details>

<details>
<summary><code>drainctl → turnstep</code> — 1 declarations, 1 uses</summary>

- `sessionAgent.handleWatchdogFire` — method; `agent_turn_helpers.go:117`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → wakectl</code> — 1 declarations, 1 uses</summary>

- `coordinator.openOnceWakeSchedules` — method; `coordinator_wake_cli.go:30`; private; 1 use(s).

</details>

<details>
<summary><code>drainctl → watchdog</code> — 4 declarations, 4 uses</summary>

- `causeStall` — const; `stream_watchdog.go:49`; private; 1 use(s).
- `coordinator.pushDeadlineOnTurnEnd` — method; `supervision.go:236`; private; 1 use(s).
- `coordinator.recordProgress` — method; `supervision.go:201`; private; 1 use(s).
- `noticeKindSupervision` — const; `supervision.go:34`; private; 1 use(s).

</details>

<details>
<summary><code>finalize → agentrun</code> — 18 declarations, 82 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 11 use(s).
- `sessionAgent.coord` — field; `agent.go:240`; private; 4 use(s).
- `sessionAgent.effectiveSessionPreambleMaxDuration` — method; `agent_timeouts.go:79`; private; 1 use(s).
- `sessionAgent.isSubAgent` — field; `agent.go:319`; private; 2 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 3 use(s).
- `sessionAgent.sessions` — field; `agent.go:320`; private; 4 use(s).
- `sessionAgent.shuttingDown` — field; `agent.go:293`; private; 2 use(s).
- `sessionPreambleMaxDurationDefault` — const; `agent.go:187`; private; 3 use(s).
- `turnStream` — type; `agent_turn_stream.go:110`; private; 12 use(s).
- `turnStream.a` — field; `agent_turn_stream.go:111`; private; 6 use(s).
- `turnStream.call` — field; `agent_turn_stream.go:115`; private; 2 use(s).
- `turnStream.currentAssistant` — field; `agent_turn_stream.go:183`; private; 5 use(s).
- `turnStream.fenceCheckpoint` — field; `agent_turn_stream.go:126`; private; 2 use(s).
- `turnStream.finalization` — field; `agent_turn_stream.go:122`; private; 9 use(s).
- `turnStream.finalizationCandidate` — field; `agent_turn_stream.go:125`; private; 5 use(s).
- `turnStream.finalizationStamp` — field; `agent_turn_stream.go:123`; private; 5 use(s).
- `turnStream.genCtx` — field; `agent_turn_stream.go:113`; private; 1 use(s).
- `turnStream.mu` — field; `agent_turn_stream.go:182`; private; 5 use(s).

</details>

<details>
<summary><code>finalize → batchflow</code> — 5 declarations, 12 uses</summary>

- `BatchRootLeaseStore` — alias; `batch_root_lease.go:40`; exported; 2 use(s).
- `batchRootIdentity.owner` — field; `batch_root_lease.go:30`; private; 1 use(s).
- `batchRootLeaseStore` — type; `batch_root_lease.go:21`; private; 2 use(s).
- `batchRootLeaseStore.entries` — field; `batch_root_lease.go:24`; private; 3 use(s).
- `batchRootLeaseStore.mu` — field; `batch_root_lease.go:22`; private; 4 use(s).

</details>

<details>
<summary><code>finalize → batchinst</code> — 46 declarations, 133 uses</summary>

- `BatchInstall` — type; `batch_install_adapters.go:668`; exported; 3 use(s).
- `BatchInstall.DiskBindings` — field; `batch_install_adapters.go:683`; exported; 1 use(s).
- `BatchInstall.RootLeases` — field; `batch_install_adapters.go:687`; exported; 1 use(s).
- `DiskProviderRegistry` — alias; `disk_provider_binding.go:43`; exported; 2 use(s).
- `ErrFinalizationFactsUnavailable` — var; `finalization_gate.go:11`; exported; 19 use(s).
- `ErrFinalizationSealed` — var; `finalization_gate.go:12`; exported; 1 use(s).
- `diskCapabilityManager` — type; `disk_provider_capability.go:55`; private; 1 use(s).
- `diskCapabilityManager.mu` — field; `disk_provider_capability.go:56`; private; 2 use(s).
- `diskCapabilityManager.owners` — field; `disk_provider_capability.go:59`; private; 2 use(s).
- `diskOwnerExecution` — const; `disk_provider_capability.go:36`; private; 2 use(s).
- `diskOwnerKey` — type; `disk_provider_capability.go:43`; private; 2 use(s).
- `diskOwnerKey.instance` — field; `disk_provider_capability.go:45`; private; 1 use(s).
- `diskOwnerKey.kind` — field; `disk_provider_capability.go:44`; private; 3 use(s).
- `diskOwnerKind` — type; `disk_provider_capability.go:22`; private; 2 use(s).
- `diskOwnerRoot` — const; `disk_provider_capability.go:33`; private; 2 use(s).
- `finalizationAccepted` — const; `finalization_gate.go:42`; private; 2 use(s).
- `finalizationCandidate` — type; `finalization_gate.go:55`; private; 6 use(s).
- `finalizationCandidate.hash` — field; `finalization_gate.go:57`; private; 7 use(s).
- `finalizationCandidate.messageID` — field; `finalization_gate.go:56`; private; 7 use(s).
- `finalizationCompose` — const; `finalization_gate.go:41`; private; 1 use(s).
- `finalizationFacts` — type; `finalization_gate.go:16`; private; 16 use(s).
- `finalizationFacts.cancelled` — field; `finalization_gate.go:25`; private; 3 use(s).
- `finalizationFacts.complete` — field; `finalization_gate.go:18`; private; 2 use(s).
- `finalizationFacts.debt` — field; `finalization_gate.go:22`; private; 3 use(s).
- `finalizationFacts.descendants` — field; `finalization_gate.go:20`; private; 3 use(s).
- `finalizationFacts.durableRevision` — field; `finalization_gate.go:17`; private; 1 use(s).
- `finalizationFacts.heldQuestion` — field; `finalization_gate.go:24`; private; 2 use(s).
- `finalizationFacts.own` — field; `finalization_gate.go:19`; private; 1 use(s).
- `finalizationFacts.physicalHolds` — field; `finalization_gate.go:21`; private; 3 use(s).
- `finalizationFacts.queuedCalls` — field; `finalization_gate.go:23`; private; 3 use(s).
- `finalizationGate` — type; `finalization_gate.go:63`; private; 4 use(s).
- `finalizationGate.beginCandidate` — method; `finalization_gate.go:157`; private; 2 use(s).
- `finalizationGate.cancel` — method; `finalization_gate.go:117`; private; 1 use(s).
- `finalizationGate.candidateSerial` — field; `finalization_gate.go:66`; private; 2 use(s).
- `finalizationGate.epoch` — field; `finalization_gate.go:65`; private; 1 use(s).
- `finalizationGate.mu` — field; `finalization_gate.go:64`; private; 2 use(s).
- `finalizationGate.trySeal` — method; `finalization_gate.go:177`; private; 1 use(s).
- `finalizationGate.waitState` — method; `finalization_gate.go:229`; private; 1 use(s).
- `finalizationPauseQuestion` — const; `finalization_gate.go:40`; private; 1 use(s).
- `finalizationStamp` — type; `finalization_gate.go:46`; private; 3 use(s).
- `finalizationStamp.durableRevision` — field; `finalization_gate.go:51`; private; 2 use(s).
- `finalizationStamp.epoch` — field; `finalization_gate.go:48`; private; 2 use(s).
- `finalizationStamp.owner` — field; `finalization_gate.go:47`; private; 1 use(s).
- `finalizationStamp.serial` — field; `finalization_gate.go:49`; private; 3 use(s).
- `finalizationWaitWork` — const; `finalization_gate.go:39`; private; 2 use(s).
- `newFinalizationGate` — func; `finalization_gate.go:77`; private; 1 use(s).

</details>

<details>
<summary><code>finalize → coord</code> — 8 declarations, 24 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 11 use(s).
- `coordinator.RebuildSessionAgentCall` — method; `coordinator_run_queue_call.go:42`; exported; 1 use(s).
- `coordinator.RunSessionAgentCall` — method; `coordinator_run_queue_call.go:411`; exported; 1 use(s).
- `coordinator.currentAgent` — field; `coordinator.go:196`; private; 4 use(s).
- `coordinator.finalization` — field; `coordinator.go:194`; private; 2 use(s).
- `coordinator.readyWg` — field; `coordinator.go:211`; private; 1 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 2 use(s).
- `coordinator.subAgentDrivers` — field; `coordinator.go:150`; private; 2 use(s).

</details>

<details>
<summary><code>finalize → mailbox</code> — 1 declarations, 1 uses</summary>

- `WithReservedOwnership` — func; `agent_ownership.go:820`; exported; 1 use(s).

</details>

<details>
<summary><code>finalize → subagents</code> — 8 declarations, 21 uses</summary>

- `subAgentDriver.FinalizationAuthorities` — field; `coordinator_subagent_drivers.go:45`; exported; 3 use(s).
- `subAgentDriver.agent` — field; `coordinator_subagent_drivers.go:42`; private; 3 use(s).
- `subAgentDriver.call` — field; `coordinator_subagent_drivers.go:43`; private; 7 use(s).
- `subAgentDriver.generation` — field; `coordinator_subagent_drivers.go:70`; private; 2 use(s).
- `subAgentDriverRegistry` — type; `coordinator_subagent_drivers.go:105`; private; 1 use(s).
- `subAgentDriverRegistry.byChild` — field; `coordinator_subagent_drivers.go:107`; private; 2 use(s).
- `subAgentDriverRegistry.get` — method; `coordinator_subagent_drivers.go:145`; private; 1 use(s).
- `subAgentDriverRegistry.mu` — field; `coordinator_subagent_drivers.go:106`; private; 2 use(s).

</details>

<details>
<summary><code>mailbox → agentrun</code> — 7 declarations, 22 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 11 use(s).
- `sessionAgent.coord` — field; `agent.go:240`; private; 2 use(s).
- `sessionAgent.diskCapsRef` — method; `agent.go:495`; private; 3 use(s).
- `sessionAgent.mailboxes` — field; `agent.go:442`; private; 1 use(s).
- `sessionAgent.onSessionIdle` — field; `agent.go:264`; private; 2 use(s).
- `sessionAgent.sessions` — field; `agent.go:320`; private; 2 use(s).
- `sessionAgent.summarizeQueue` — field; `agent.go:430`; private; 1 use(s).

</details>

<details>
<summary><code>mailbox → batchflow</code> — 3 declarations, 4 uses</summary>

- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 2 use(s).
- `unknownBatchCutoverFacts` — type; `batch_cutover_facts.go:119`; private; 1 use(s).

</details>

<details>
<summary><code>mailbox → batchinst</code> — 17 declarations, 25 uses</summary>

- `ErrBatchQuestionCapabilityNotDurable` — var; `batch_scope.go:73`; exported; 1 use(s).
- `ErrCredentialSetNotDurable` — var; `disk_provider_durable.go:45`; exported; 1 use(s).
- `ErrDiskProviderNotDurable` — var; `disk_provider_durable.go:29`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 3 use(s).
- `batchIngressTool` — type; `batch_install_tools.go:84`; private; 1 use(s).
- `batchIngressTool.bind` — field; `batch_install_tools.go:96`; private; 1 use(s).
- `batchIngressTool.inv` — field; `batch_install_tools.go:88`; private; 4 use(s).
- `callCarriesDiskProvider` — func; `disk_provider_durable.go:50`; private; 1 use(s).
- `diskCapabilityManager.acquire` — method; `disk_provider_capability.go:103`; private; 1 use(s).
- `diskCapabilityManager.release` — method; `disk_provider_capability.go:159`; private; 1 use(s).
- `diskOwnerConstruction` — const; `disk_provider_capability.go:31`; private; 1 use(s).
- `diskOwnerKey` — type; `disk_provider_capability.go:43`; private; 1 use(s).
- `diskOwnerKey.instance` — field; `disk_provider_capability.go:45`; private; 1 use(s).
- `diskOwnerKey.kind` — field; `disk_provider_capability.go:44`; private; 1 use(s).
- `diskOwnerKey.registryID` — field; `disk_provider_capability.go:49`; private; 1 use(s).
- `holdDurableContinuationOwner` — func; `disk_provider_continuation.go:170`; private; 2 use(s).
- `requirementFromCall` — func; `disk_provider_continuation.go:233`; private; 3 use(s).

</details>

<details>
<summary><code>mailbox → batchrun</code> — 1 declarations, 4 uses</summary>

- `BatchInvocation.RegistryID` — field; `batch_invocation.go:106`; exported; 4 use(s).

</details>

<details>
<summary><code>mailbox → clibridge</code> — 1 declarations, 1 uses</summary>

- `coordinator.batchActiveAfterQuestionPause` — method; `cli_active_work.go:97`; private; 1 use(s).

</details>

<details>
<summary><code>mailbox → coord</code> — 2 declarations, 4 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 1 use(s).
- `coordinator.background` — field; `coordinator.go:113`; private; 3 use(s).

</details>

<details>
<summary><code>mailbox → drainctl</code> — 1 declarations, 1 uses</summary>

- `drainAccountBudget` — func; `drain_attempt.go:66`; private; 1 use(s).

</details>

<details>
<summary><code>mailbox → finalize</code> — 1 declarations, 1 uses</summary>

- `enqueueFinalizationCall` — func; `finalization_queue_call.go:62`; private; 1 use(s).

</details>

<details>
<summary><code>mailbox → turnstep</code> — 3 declarations, 3 uses</summary>

- `ErrSummarizeQueued` — var; `agent_compaction.go:44`; exported; 1 use(s).
- `sessionAgent.Summarize` — method; `agent_compaction.go:46`; exported; 1 use(s).
- `sessionAgent.TakeSummarizeQueue` — method; `agent_compaction.go:349`; exported; 1 use(s).

</details>

<details>
<summary><code>modelkit → agenticfetch</code> — 1 declarations, 1 uses</summary>

- `coordinator.agenticFetchTool` — method; `agentic_fetch_tool.go:54`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → agentrun</code> — 25 declarations, 35 uses</summary>

- `NewSessionAgent` — func; `agent.go:504`; exported; 2 use(s).
- `SessionAgentOptions` — type; `agent_options.go:16`; exported; 2 use(s).
- `SessionAgentOptions.CheckpointInterval` — field; `agent_options.go:63`; exported; 1 use(s).
- `SessionAgentOptions.Config` — field; `agent_options.go:37`; exported; 2 use(s).
- `SessionAgentOptions.Coord` — field; `agent_options.go:29`; exported; 1 use(s).
- `SessionAgentOptions.DataDirectory` — field; `agent_options.go:54`; exported; 2 use(s).
- `SessionAgentOptions.DisableAutoSummarize` — field; `agent_options.go:22`; exported; 2 use(s).
- `SessionAgentOptions.FastModel` — field; `agent_options.go:18`; exported; 2 use(s).
- `SessionAgentOptions.IsSubAgent` — field; `agent_options.go:21`; exported; 1 use(s).
- `SessionAgentOptions.IsYolo` — field; `agent_options.go:23`; exported; 1 use(s).
- `SessionAgentOptions.Messages` — field; `agent_options.go:25`; exported; 2 use(s).
- `SessionAgentOptions.Notify` — field; `agent_options.go:38`; exported; 1 use(s).
- `SessionAgentOptions.OnSessionIdle` — field; `agent_options.go:33`; exported; 1 use(s).
- `SessionAgentOptions.RestrictedRuns` — field; `agent_options.go:131`; exported; 1 use(s).
- `SessionAgentOptions.RunAllowlists` — field; `agent_options.go:129`; exported; 1 use(s).
- `SessionAgentOptions.Sessions` — field; `agent_options.go:24`; exported; 2 use(s).
- `SessionAgentOptions.SmartModel` — field; `agent_options.go:17`; exported; 2 use(s).
- `SessionAgentOptions.StreamIdleTimeout` — field; `agent_options.go:41`; exported; 1 use(s).
- `SessionAgentOptions.SystemPrompt` — field; `agent_options.go:20`; exported; 1 use(s).
- `SessionAgentOptions.SystemPromptPrefix` — field; `agent_options.go:19`; exported; 1 use(s).
- `SessionAgentOptions.ToolMaxDuration` — field; `agent_options.go:81`; exported; 1 use(s).
- `SessionAgentOptions.Tools` — field; `agent_options.go:26`; exported; 2 use(s).
- `SessionAgentOptions.diskCaps` — field; `agent_options.go:106`; private; 1 use(s).
- `defaultCheckpointInterval` — const; `agent.go:169`; private; 1 use(s).
- `sessionAgent` — type; `agent.go:230`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → batchflow</code> — 5 declarations, 5 uses</summary>

- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `applyBatchQuestionRouteFloor` — func; `batch_question_route.go:51`; private; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 1 use(s).
- `globalQuestionContext` — type; `batch_question_global_context.go:5`; private; 1 use(s).
- `globalQuestionContext.Context` — field; `batch_question_global_context.go:5`; exported; 1 use(s).

</details>

<details>
<summary><code>modelkit → batchinst</code> — 25 declarations, 30 uses</summary>

- `BatchModelControl.InstallBatchModelControls` — method; `batch_install_ingress.go:28`; exported; 1 use(s).
- `BatchWorkIngress.Registry` — method; `batch_install_ingress.go:46`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 1 use(s).
- `batchAgentAskQuestionTool` — type; `batch_install_question.go:83`; private; 1 use(s).
- `batchAgentAskQuestionTool.AgentTool` — field; `batch_install_question.go:83`; exported; 1 use(s).
- `batchControlAvailability` — func; `batch_allowed_controls.go:25`; private; 1 use(s).
- `batchCoordinatorBinding.controls` — field; `batch_install_ingress.go:129`; private; 3 use(s).
- `batchCoordinatorBinding.ingress` — field; `batch_install_ingress.go:128`; private; 2 use(s).
- `batchFamilyDelivery` — func; `batch_install_tools.go:74`; private; 1 use(s).
- `batchGroupDefault` — const; `batch_profile.go:16`; private; 1 use(s).
- `batchGroupRanks` — var; `batch_profile.go:23`; private; 1 use(s).
- `batchIngressTool` — type; `batch_install_tools.go:84`; private; 1 use(s).
- `batchIngressTool.AgentTool` — field; `batch_install_tools.go:85`; exported; 1 use(s).
- `batchIngressTool.bind` — field; `batch_install_tools.go:96`; private; 1 use(s).
- `batchIngressTool.ingress` — field; `batch_install_tools.go:87`; private; 1 use(s).
- `batchIngressTool.inv` — field; `batch_install_tools.go:88`; private; 1 use(s).
- `batchIngressTool.registry` — field; `batch_install_tools.go:86`; private; 1 use(s).
- `batchToolsetFingerprint` — func; `batch_install_tools.go:401`; private; 2 use(s).
- `batchWorkTarget` — func; `batch_install_tools.go:55`; private; 1 use(s).
- `coordinatorBatchBindingFor` — func; `batch_install_ingress.go:164`; private; 2 use(s).
- `folderScopeCommandToolSet` — func; `batch_allowed_controls.go:85`; private; 1 use(s).
- `inlineWindow` — const; `inline_window.go:16`; private; 1 use(s).
- `inlineWindowDisabledFor` — func; `inline_window.go:54`; private; 1 use(s).
- `newBatchWorkToolBuildForContext` — func; `batch_install_tools.go:293`; private; 1 use(s).
- `workerLayeredToolNames` — func; `batch_allowed_controls.go:71`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → batchrun</code> — 38 declarations, 63 uses</summary>

- `BatchInvocation` — type; `batch_invocation.go:98`; exported; 2 use(s).
- `BatchInvocation.AgentDepth` — field; `batch_invocation.go:111`; exported; 2 use(s).
- `BatchInvocation.AgentDepthSet` — field; `batch_invocation.go:112`; exported; 1 use(s).
- `BatchInvocation.BackgroundEscape` — field; `batch_invocation.go:115`; exported; 3 use(s).
- `BatchInvocation.Delivery` — field; `batch_invocation.go:114`; exported; 4 use(s).
- `BatchInvocation.MessageID` — field; `batch_invocation.go:102`; exported; 2 use(s).
- `BatchInvocation.Origin` — field; `batch_invocation.go:109`; exported; 2 use(s).
- `BatchInvocation.Policy` — field; `batch_invocation.go:124`; exported; 9 use(s).
- `BatchInvocation.RegistryID` — field; `batch_invocation.go:106`; exported; 1 use(s).
- `BatchInvocation.SessionID` — field; `batch_invocation.go:100`; exported; 1 use(s).
- `BatchInvocation.WaiterRank` — field; `batch_invocation.go:120`; exported; 2 use(s).
- `BatchInvocation.WorkerStopScope` — field; `batch_invocation.go:126`; exported; 1 use(s).
- `BatchInvocation.roleConfig` — field; `batch_invocation.go:136`; private; 1 use(s).
- `batchDeliveryChoice` — type; `batch_invocation.go:89`; private; 2 use(s).
- `batchDeliveryChoice.Mode` — field; `batch_invocation.go:90`; exported; 2 use(s).
- `batchDeliveryChoice.Window` — field; `batch_invocation.go:91`; exported; 1 use(s).
- `batchDeliveryDetached` — const; `batch_invocation.go:26`; private; 2 use(s).
- `batchEscapeAuto` — const; `batch_invocation.go:33`; private; 1 use(s).
- `batchEscapeNever` — const; `batch_invocation.go:32`; private; 2 use(s).
- `batchLeafPolicyContext` — type; `batch_invocation.go:67`; private; 2 use(s).
- `batchLeafPolicyContext.DiskBindingID` — field; `batch_invocation.go:80`; exported; 1 use(s).
- `batchLeafPolicyContext.DiskProvider` — field; `batch_invocation.go:75`; exported; 1 use(s).
- `batchLeafPolicyContext.FolderScope` — field; `batch_invocation.go:72`; exported; 1 use(s).
- `batchLeafPolicyContext.MessageID` — field; `batch_invocation.go:69`; exported; 1 use(s).
- `batchLeafPolicyContext.ModelName` — field; `batch_invocation.go:71`; exported; 1 use(s).
- `batchLeafPolicyContext.RegistryID` — field; `batch_invocation.go:68`; exported; 1 use(s).
- `batchLeafPolicyContext.RestrictedToolset` — field; `batch_invocation.go:81`; exported; 2 use(s).
- `batchLeafPolicyContext.RoleContract` — field; `batch_invocation.go:85`; exported; 2 use(s).
- `batchLeafPolicyContext.RunAllowlist` — field; `batch_invocation.go:73`; exported; 1 use(s).
- `batchLeafPolicyContext.SupportsImages` — field; `batch_invocation.go:70`; exported; 1 use(s).
- `newBatchControlPlaceholder` — func; `batch_model_tools.go:110`; private; 1 use(s).
- `pctCapturedAgentTool` — type; `batch_child_tool_scope.go:77`; private; 1 use(s).
- `pctCapturedAgentTool.AgentTool` — field; `batch_child_tool_scope.go:78`; exported; 1 use(s).
- `pctCapturedAgentTool.scope` — field; `batch_child_tool_scope.go:79`; private; 1 use(s).
- `pctChildToolBuildContext` — func; `batch_child_tool_scope.go:61`; private; 1 use(s).
- `pctChildToolScope.pctRelease` — method; `batch_child_tool_scope.go:39`; private; 1 use(s).
- `pctHandoffFrom` — func; `batch_child_tool_scope.go:290`; private; 1 use(s).
- `withoutBatchControlPlaceholders` — func; `batch_model_tools.go:97`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → callbeat</code> — 6 declarations, 13 uses</summary>

- `NewHeartbeatProvider` — func; `heartbeat_provider.go:19`; exported; 1 use(s).
- `hbCallSource` — func; `heartbeat_ctx.go:47`; private; 2 use(s).
- `hbOverrideSource` — func; `heartbeat_ctx.go:39`; private; 2 use(s).
- `hbSourceSessionOverride` — const; `heartbeat_ctx.go:19`; private; 4 use(s).
- `hbSourceSlotLive` — const; `heartbeat_ctx.go:18`; private; 3 use(s).
- `hbSourceSlotStart` — const; `heartbeat_ctx.go:16`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → coord</code> — 50 declarations, 333 uses</summary>

- `PeakHoursError` — type; `coordinator.go:70`; exported; 1 use(s).
- `PeakHoursError.End` — field; `coordinator.go:72`; exported; 3 use(s).
- `PeakHoursError.Message` — field; `coordinator.go:76`; exported; 1 use(s).
- `PeakHoursError.ProviderID` — field; `coordinator.go:71`; exported; 1 use(s).
- `PeakHoursError.ReopensAt` — field; `coordinator.go:73`; exported; 5 use(s).
- `PeakHoursError.Start` — field; `coordinator.go:72`; exported; 3 use(s).
- `PeakHoursError.Timezone` — field; `coordinator.go:79`; exported; 1 use(s).
- `cachedModelPair` — type; `coordinator.go:301`; private; 13 use(s).
- `cachedModelPair.fast` — field; `coordinator.go:303`; private; 2 use(s).
- `cachedModelPair.smart` — field; `coordinator.go:302`; private; 2 use(s).
- `callCredentialsFrom` — func; `credentials.go:41`; private; 3 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 85 use(s).
- `coordinator.activeModelRole` — field; `coordinator.go:233`; private; 1 use(s).
- `coordinator.activeModelRoleMu` — field; `coordinator.go:232`; private; 2 use(s).
- `coordinator.activeSkills` — field; `coordinator.go:201`; private; 1 use(s).
- `coordinator.allSkills` — field; `coordinator.go:200`; private; 1 use(s).
- `coordinator.background` — field; `coordinator.go:113`; private; 1 use(s).
- `coordinator.buildCredentialModel` — method; `credentials.go:385`; private; 2 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 64 use(s).
- `coordinator.currentAgent` — field; `coordinator.go:196`; private; 10 use(s).
- `coordinator.diskCaps` — field; `coordinator.go:161`; private; 1 use(s).
- `coordinator.filetracker` — field; `coordinator.go:110`; private; 8 use(s).
- `coordinator.history` — field; `coordinator.go:109`; private; 6 use(s).
- `coordinator.mcpOwner` — field; `coordinator.go:184`; private; 5 use(s).
- `coordinator.messages` — field; `coordinator.go:107`; private; 5 use(s).
- `coordinator.modelCache` — field; `coordinator.go:289`; private; 9 use(s).
- `coordinator.modelCacheEpoch` — field; `coordinator.go:291`; private; 3 use(s).
- `coordinator.modelCacheMu` — field; `coordinator.go:290`; private; 6 use(s).
- `coordinator.modelPairBuilder` — field; `coordinator.go:292`; private; 2 use(s).
- `coordinator.notify` — field; `coordinator.go:112`; private; 3 use(s).
- `coordinator.permissions` — field; `coordinator.go:108`; private; 29 use(s).
- `coordinator.prompt` — field; `coordinator.go:111`; private; 10 use(s).
- `coordinator.rateLimitNotify` — field; `coordinator.go:237`; private; 2 use(s).
- `coordinator.readyWg` — field; `coordinator.go:211`; private; 1 use(s).
- `coordinator.refreshOAuth2TokenFn` — field; `coordinator.go:296`; private; 2 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 12 use(s).
- `coordinator.sharedBuildScope` — field; `coordinator.go:180`; private; 3 use(s).
- `coordinator.sharedBuildScopeMu` — field; `coordinator.go:179`; private; 2 use(s).
- `coordinator.skillTracker` — field; `coordinator.go:202`; private; 2 use(s).
- `credentialProviderConfig` — func; `credentials.go:425`; private; 1 use(s).
- `errCoderAgentNotConfigured` — var; `coordinator.go:48`; private; 2 use(s).
- `errFastModelNotSelected` — var; `coordinator.go:51`; private; 1 use(s).
- `errFastModelProviderNotConfigured` — var; `coordinator.go:53`; private; 1 use(s).
- `errModelProviderNotConfigured` — var; `coordinator.go:49`; private; 4 use(s).
- `errProviderPeakHours` — var; `coordinator.go:60`; private; 2 use(s).
- `errSmartModelNotSelected` — var; `coordinator.go:50`; private; 1 use(s).
- `errSmartModelProviderNotConfigured` — var; `coordinator.go:52`; private; 1 use(s).
- `streamStallRetryBaseBackoff` — var; `coordinator_run.go:58`; private; 3 use(s).
- `streamStalledFinishTitle` — const; `coordinator_run.go:52`; private; 3 use(s).
- `taskPrompt` — func; `prompts.go:28`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → delegation</code> — 6 declarations, 10 uses</summary>

- `DefaultAgentDepth` — const; `delegation_depth.go:12`; exported; 1 use(s).
- `ErrAgentDepthExceeded` — var; `delegation_depth.go:17`; exported; 1 use(s).
- `MaxAgentDepthFrom` — func; `delegation_depth.go:22`; exported; 5 use(s).
- `WithAgentDepth` — func; `delegation_depth.go:37`; exported; 1 use(s).
- `WithParentRoleConfigSnapshot` — func; `delegation_role_models.go:155`; exported; 1 use(s).
- `requestedRoleSlot` — func; `delegation_role_models.go:19`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → drainctl</code> — 1 declarations, 1 uses</summary>

- `drainRefreshBudget` — func; `drain_attempt.go:67`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → provauth</code> — 6 declarations, 6 uses</summary>

- `codexTokenSource` — type; `codex_token_source.go:23`; private; 1 use(s).
- `codexTokenSource.resolve` — field; `codex_token_source.go:25`; private; 1 use(s).
- `codexTokenSource.store` — field; `codex_token_source.go:24`; private; 1 use(s).
- `coordinator.withOAuthBearerClient` — method; `oauth_token_source.go:85`; private; 1 use(s).
- `newCodexTokenProvider` — func; `codex_token_source.go:73`; private; 1 use(s).
- `withoutEmptyAuthHeaders` — func; `anthropic_empty_auth_transport.go:42`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → responsesws</code> — 3 declarations, 6 uses</summary>

- `coordinator.buildPublicResponsesProvider` — method; `coordinator_responses_ws.go:105`; private; 1 use(s).
- `coordinator.codexWebSocketOptions` — method; `coordinator_responses_ws.go:25`; private; 1 use(s).
- `responsesWSReplayForbidden` — func; `coordinator_responses_ws.go:18`; private; 4 use(s).

</details>

<details>
<summary><code>modelkit → subagents</code> — 13 declarations, 13 uses</summary>

- `coordinator.notifyBackgroundJobDone` — method; `coordinator_background.go:137`; private; 1 use(s).
- `coordinator.runSubAgent` — method; `coordinator_subagents.go:69`; private; 1 use(s).
- `subAgentParams` — type; `coordinator_subagents.go:26`; private; 1 use(s).
- `subAgentParams.Agent` — field; `coordinator_subagents.go:27`; exported; 1 use(s).
- `subAgentParams.AgentMessageID` — field; `coordinator_subagents.go:29`; exported; 1 use(s).
- `subAgentParams.Prompt` — field; `coordinator_subagents.go:31`; exported; 1 use(s).
- `subAgentParams.RequestedModelRole` — field; `coordinator_subagents.go:33`; exported; 1 use(s).
- `subAgentParams.ResumeSessionID` — field; `coordinator_subagents.go:42`; exported; 1 use(s).
- `subAgentParams.SessionID` — field; `coordinator_subagents.go:28`; exported; 1 use(s).
- `subAgentParams.SessionTitle` — field; `coordinator_subagents.go:32`; exported; 1 use(s).
- `subAgentParams.ToolCallID` — field; `coordinator_subagents.go:30`; exported; 1 use(s).
- `subAgentParams.pctHandoff` — field; `coordinator_subagents.go:48`; private; 1 use(s).
- `subAgentParams.slotModel` — field; `coordinator_subagents.go:47`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → toolwrap</code> — 24 declarations, 62 uses</summary>

- `cloneRoleToolContracts` — func; `role_tool_policy.go:147`; private; 1 use(s).
- `decoratePolicyTools` — func; `policy_leaf.go:250`; private; 2 use(s).
- `filterRoleContractTools` — func; `role_tool_policy.go:115`; private; 4 use(s).
- `filterRoleToolNames` — func; `role_tool_policy.go:102`; private; 2 use(s).
- `hasRoleContract` — func; `role_tool_policy.go:138`; private; 2 use(s).
- `newPolicyToolBuild` — func; `policy_leaf.go:34`; private; 1 use(s).
- `newPolicyToolBuildForContext` — func; `policy_leaf.go:38`; private; 1 use(s).
- `policyToolBuild` — type; `policy_leaf.go:17`; private; 8 use(s).
- `policyToolBuild.leaves` — field; `policy_leaf.go:19`; private; 1 use(s).
- `policyToolBuild.registryID` — field; `policy_leaf.go:20`; private; 3 use(s).
- `policyToolBuild.tools` — field; `policy_leaf.go:18`; private; 16 use(s).
- `restrictedRunAuthorizer` — func; `restricted_tool.go:62`; private; 4 use(s).
- `roleContractAllowsName` — func; `role_tool_policy.go:49`; private; 3 use(s).
- `roleContractForToolBuild` — func; `policy_leaf.go:136`; private; 1 use(s).
- `roleContractFromContext` — func; `role_tool_policy.go:39`; private; 2 use(s).
- `roleContractsAllowName` — func; `policy_leaf.go:155`; private; 1 use(s).
- `roleContractsForTools` — func; `policy_leaf.go:203`; private; 1 use(s).
- `roleGuardTool` — type; `role_tool_policy.go:160`; private; 1 use(s).
- `roleGuardTool.AgentTool` — field; `role_tool_policy.go:161`; exported; 1 use(s).
- `roleGuardTool.contracts` — field; `role_tool_policy.go:162`; private; 1 use(s).
- `roleToolsConstrained` — func; `role_tool_policy.go:82`; private; 2 use(s).
- `withRoleToolContract` — func; `role_tool_policy.go:18`; private; 2 use(s).
- `wrapToolsWithHooks` — func; `hooked_tool.go:31`; private; 1 use(s).
- `wrapToolsWithRestrictedRun` — func; `restricted_tool.go:78`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → turnstep</code> — 2 declarations, 2 uses</summary>

- `sessionAgent.SummarizeNow` — method; `agent_compaction.go:58`; exported; 1 use(s).
- `turnSmartReasoningEffort` — func; `turn_effort.go:6`; private; 1 use(s).

</details>

<details>
<summary><code>modelkit → usage</code> — 3 declarations, 4 uses</summary>

- `QuotaLimitResetTime` — func; `quota_reset.go:26`; exported; 1 use(s).
- `coordinator.withPeakHoursRequestGate` — method; `peak_hours_request_gate.go:25`; private; 1 use(s).
- `parseZAIResetHint` — func; `quota_reset.go:185`; private; 2 use(s).

</details>

<details>
<summary><code>modelkit → watchdog</code> — 1 declarations, 1 uses</summary>

- `coordinator.onSessionIdleHook` — method; `supervision.go:490`; private; 1 use(s).

</details>

<details>
<summary><code>promptcache → agentrun</code> — 6 declarations, 9 uses</summary>

- `turnStream` — type; `agent_turn_stream.go:110`; private; 1 use(s).
- `turnStream.cacheDiagnostic` — field; `agent_turn_stream.go:196`; private; 1 use(s).
- `turnStream.cachePrepareDuration` — field; `agent_turn_stream.go:198`; private; 1 use(s).
- `turnStream.cachePreparedAt` — field; `agent_turn_stream.go:197`; private; 1 use(s).
- `turnStream.call` — field; `agent_turn_stream.go:115`; private; 1 use(s).
- `turnStream.smartModel` — field; `agent_turn_stream.go:128`; private; 4 use(s).

</details>

<details>
<summary><code>provauth → agentrun</code> — 4 declarations, 50 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 8 use(s).
- `sessionAgent.coord` — field; `agent.go:240`; private; 9 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 20 use(s).
- `sessionAgent.sessions` — field; `agent.go:320`; private; 13 use(s).

</details>

<details>
<summary><code>provauth → batchinst</code> — 1 declarations, 1 uses</summary>

- `ErrCredentialBindingUnavailable` — var; `disk_provider_durable.go:40`; exported; 1 use(s).

</details>

<details>
<summary><code>provauth → callbeat</code> — 3 declarations, 5 uses</summary>

- `addHeartbeatUsage` — func; `heartbeat_ctx.go:152`; private; 3 use(s).
- `hbInherited` — func; `heartbeat_ctx.go:128`; private; 1 use(s).
- `withHeartbeat` — func; `heartbeat_ctx.go:66`; private; 1 use(s).

</details>

<details>
<summary><code>provauth → coord</code> — 4 declarations, 8 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 1 use(s).
- `coordinator.buildCredentialModel` — method; `credentials.go:385`; private; 1 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 5 use(s).
- `coordinator.drainCredentialBoundary` — method; `coordinator_agent_control.go:243`; private; 1 use(s).

</details>

<details>
<summary><code>provauth → delegation</code> — 1 declarations, 2 uses</summary>

- `requestedRoleSlot` — func; `delegation_role_models.go:19`; private; 2 use(s).

</details>

<details>
<summary><code>provauth → mailbox</code> — 2 declarations, 8 uses</summary>

- `notifyActivity` — func; `agent_ownership.go:753`; private; 4 use(s).
- `notifyWatchdog` — func; `agent_ownership.go:769`; private; 4 use(s).

</details>

<details>
<summary><code>provauth → modelkit</code> — 4 declarations, 5 uses</summary>

- `checkPeakHours` — func; `coordinator_providers_auth.go:125`; private; 1 use(s).
- `coordinator.buildModelsFromCfg` — method; `coordinator_models_build.go:92`; private; 2 use(s).
- `coordinator.resolveProviderHTTPClient` — method; `coordinator_providers_network.go:28`; private; 1 use(s).
- `getProviderOptions` — func; `coordinator_providers.go:109`; private; 1 use(s).

</details>

<details>
<summary><code>provauth → sessionprompt</code> — 4 declarations, 6 uses</summary>

- `sessionAgent.getSessionMessages` — method; `agent_prompt.go:330`; private; 2 use(s).
- `sessionAgent.preparePrompt` — method; `agent_prompt.go:146`; private; 2 use(s).
- `sessionAgent.workaroundProviderMediaLimitations` — method; `agent_prompt.go:458`; private; 1 use(s).
- `sessionHeaders` — func; `agent_prompt.go:49`; private; 1 use(s).

</details>

<details>
<summary><code>provauth → turnstep</code> — 8 declarations, 17 uses</summary>

- `buildSummaryPrompt` — func; `agent_compaction.go:599`; private; 2 use(s).
- `effectiveMaxOutputTokens` — func; `agent_context_budget.go:13`; private; 2 use(s).
- `modelInputBudget` — func; `agent_context_budget.go:26`; private; 1 use(s).
- `preparedInputTokenCount` — func; `agent_context_budget.go:89`; private; 1 use(s).
- `summaryCommitMaxDuration` — const; `agent_compaction.go:37`; private; 6 use(s).
- `summaryCompletionTokens` — func; `agent_compaction.go:591`; private; 1 use(s).
- `summaryPrompt` — var; `agent_compaction.go:40`; private; 2 use(s).
- `turnSmartReasoningEffort` — func; `turn_effort.go:6`; private; 2 use(s).

</details>

<details>
<summary><code>provauth → usage</code> — 6 declarations, 12 uses</summary>

- `addStepUsageTokens` — func; `agent_usage.go:109`; private; 2 use(s).
- `normalizeProviderUsage` — func; `usage_normalize.go:26`; private; 2 use(s).
- `sessionAgent.openrouterCost` — method; `agent_usage.go:51`; private; 2 use(s).
- `sessionAgent.recordMessageUsage` — method; `usage_record.go:46`; private; 3 use(s).
- `sessionAgent.updateSessionUsage` — method; `agent_usage.go:82`; private; 2 use(s).
- `updateSessionTokenCounters` — func; `agent_usage.go:136`; private; 1 use(s).

</details>

<details>
<summary><code>responsesws → coord</code> — 3 declarations, 11 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 3 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 4 use(s).
- `coordinator.responsesWS` — field; `coordinator.go:114`; private; 4 use(s).

</details>

<details>
<summary><code>responsesws → modelkit</code> — 2 declarations, 3 uses</summary>

- `coordinator.buildOpenaiProvider` — method; `coordinator_providers.go:611`; private; 1 use(s).
- `coordinator.resolveProviderHTTPClient` — method; `coordinator_providers_network.go:28`; private; 2 use(s).

</details>

<details>
<summary><code>responsesws → provauth</code> — 1 declarations, 2 uses</summary>

- `codexTokenProviderID` — const; `codex_token_source.go:16`; private; 2 use(s).

</details>

<details>
<summary><code>sessionprompt → agentrun</code> — 3 declarations, 9 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 6 use(s).
- `sessionAgent.isSubAgent` — field; `agent.go:319`; private; 1 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 2 use(s).

</details>

<details>
<summary><code>sessionprompt → batchinst</code> — 2 declarations, 2 uses</summary>

- `batchDriverHints.dropIfIdle` — method; `batch_rehome_coord_runtime.go:158`; private; 1 use(s).
- `batchDriverHints.idleCandidates` — method; `batch_rehome_coord_runtime.go:141`; private; 1 use(s).

</details>

<details>
<summary><code>sessionprompt → coord</code> — 4 declarations, 17 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 6 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 5 use(s).
- `coordinator.driverHints` — field; `coordinator.go:136`; private; 2 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 4 use(s).

</details>

<details>
<summary><code>sessionprompt → drainctl</code> — 4 declarations, 5 uses</summary>

- `arbiter.dropIfIdle` — method; `turn_arbiter_state.go:465`; private; 1 use(s).
- `arbiter.dropSession` — method; `turn_arbiter_state.go:509`; private; 1 use(s).
- `arbiter.gateHoldsHint` — method; `turn_arbiter_state.go:499`; private; 1 use(s).
- `arbiter.sessionsWithState` — method; `turn_arbiter_state.go:449`; private; 2 use(s).

</details>

<details>
<summary><code>sessionprompt → modelkit</code> — 4 declarations, 6 uses</summary>

- `coordinator.resolveSessionSystemPrompt` — method; `coordinator_models.go:524`; private; 1 use(s).
- `resolvedOverrides` — type; `coordinator_models.go:28`; private; 1 use(s).
- `resolvedOverrides.orchestrator` — field; `coordinator_models.go:44`; private; 1 use(s).
- `resolvedOverrides.systemPrompt` — field; `coordinator_models.go:36`; private; 3 use(s).

</details>

<details>
<summary><code>sessionprompt → promptcache</code> — 2 declarations, 2 uses</summary>

- `cacheProfile.explicitMarkers` — field; `cache_profile.go:29`; private; 1 use(s).
- `cacheProfiles` — var; `cache_profile.go:59`; private; 1 use(s).

</details>

<details>
<summary><code>sessionprompt → watchdog</code> — 1 declarations, 2 uses</summary>

- `noticeKindSupervision` — const; `supervision.go:34`; private; 2 use(s).

</details>

<details>
<summary><code>subagents → agentrun</code> — 2 declarations, 2 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 1 use(s).
- `sessionAgent.tools` — field; `agent.go:235`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → batchflow</code> — 8 declarations, 10 uses</summary>

- `BatchCutoverFacts.LiveDelegationChildOf` — method; `batch_cutover_facts.go:70`; exported; 1 use(s).
- `BatchCutoverFacts.PendingInclusiveDebt` — method; `batch_cutover_facts.go:52`; exported; 1 use(s).
- `batchQuestionRouteFrom` — func; `batch_question_route.go:34`; private; 1 use(s).
- `coordinator.batchHeldQuestionResumeHint` — method; `batch_held_question_route.go:266`; private; 2 use(s).
- `coordinator.batchHeldQuestionResumeRefusal` — method; `batch_held_question_route.go:237`; private; 1 use(s).
- `coordinator.rebuiltChildQuestionState` — method; `batch_question_rebuild.go:9`; private; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 2 use(s).
- `stripAskQuestionTools` — func; `batch_question_route.go:71`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → batchinst</code> — 21 declarations, 34 uses</summary>

- `AsyncCompletion` — type; `batch_rehome_async_completion.go:11`; exported; 2 use(s).
- `AsyncCompletion.Cancelled` — field; `batch_rehome_async_completion.go:37`; exported; 1 use(s).
- `AsyncCompletion.Content` — field; `batch_rehome_async_completion.go:15`; exported; 1 use(s).
- `AsyncCompletion.Interrupted` — field; `batch_rehome_async_completion.go:42`; exported; 1 use(s).
- `AsyncCompletion.IsError` — field; `batch_rehome_async_completion.go:16`; exported; 1 use(s).
- `AsyncCompletion.SessionID` — field; `batch_rehome_async_completion.go:12`; exported; 2 use(s).
- `AsyncCompletion.Stopped` — field; `batch_rehome_async_completion.go:32`; exported; 1 use(s).
- `AsyncCompletion.TimedOut` — field; `batch_rehome_async_completion.go:23`; exported; 1 use(s).
- `AsyncCompletion.TimeoutSeconds` — field; `batch_rehome_async_completion.go:26`; exported; 1 use(s).
- `AsyncCompletion.ToolCallID` — field; `batch_rehome_async_completion.go:13`; exported; 5 use(s).
- `AsyncCompletion.ToolName` — field; `batch_rehome_async_completion.go:14`; exported; 5 use(s).
- `AsyncCompletion.Wake` — field; `batch_rehome_async_completion.go:56`; exported; 1 use(s).
- `batchAgentLifecycle` — type; `batch_install_policy.go:16`; private; 1 use(s).
- `batchAgentLifecycle.BindChildSession` — field; `batch_install_policy.go:17`; exported; 2 use(s).
- `batchAgentLifecycle.NoChildSelected` — field; `batch_install_policy.go:18`; exported; 2 use(s).
- `batchAgentLifecycleContextKey` — type; `batch_install_policy.go:21`; private; 1 use(s).
- `batchAgentQuestionBindingFrom` — func; `batch_install_policy.go:50`; private; 1 use(s).
- `batchAgentQuestionDepth` — func; `batch_install_policy.go:38`; private; 1 use(s).
- `buildBatchQuestionTools` — func; `batch_install_question.go:63`; private; 1 use(s).
- `coordinator.bgShellRowCancelled` — method; `batch_rehome_runtime.go:77`; private; 2 use(s).
- `coordinator.sessionNoticeStore` — method; `batch_rehome_coord_runtime.go:194`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → batchrun</code> — 6 declarations, 9 uses</summary>

- `pctDelegationHandoff` — type; `batch_child_tool_scope.go:192`; private; 1 use(s).
- `pctDelegationHandoff.pctClaimDriver` — method; `batch_child_tool_scope.go:230`; private; 1 use(s).
- `pctDriverDisposer` — type; `batch_child_tool_scope.go:298`; private; 1 use(s).
- `pctDriverDisposer.SessionAgent` — field; `batch_child_tool_scope.go:299`; exported; 1 use(s).
- `pctDriverDisposer.releases` — field; `batch_child_tool_scope.go:300`; private; 1 use(s).
- `pctReleaseScopeRefs` — func; `batch_child_tool_scope.go:184`; private; 4 use(s).

</details>

<details>
<summary><code>subagents → callbeat</code> — 5 declarations, 7 uses</summary>

- `hbSourcePerCall` — const; `heartbeat_ctx.go:20`; private; 1 use(s).
- `hbSourceSessionOverride` — const; `heartbeat_ctx.go:19`; private; 3 use(s).
- `hbSourceSlotLive` — const; `heartbeat_ctx.go:18`; private; 1 use(s).
- `hbSourceSlotStart` — const; `heartbeat_ctx.go:16`; private; 1 use(s).
- `inheritHeartbeat` — func; `heartbeat_ctx.go:89`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → coord</code> — 25 declarations, 79 uses</summary>

- `callCredentialsFrom` — func; `credentials.go:41`; private; 1 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 15 use(s).
- `coordinator.addToRecheckSet` — method; `coordinator_recheck.go:42`; private; 1 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 2 use(s).
- `coordinator.autonomyEnabled` — method; `coordinator.go:550`; private; 1 use(s).
- `coordinator.bgArrival` — field; `coordinator.go:255`; private; 2 use(s).
- `coordinator.buildCredentialModel` — method; `credentials.go:385`; private; 1 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 5 use(s).
- `coordinator.childQuestionPreamble` — method; `coordinator_work_scope.go:122`; private; 1 use(s).
- `coordinator.claimAutoResumeSlot` — method; `coordinator.go:612`; private; 1 use(s).
- `coordinator.consecutiveResume` — method; `coordinator.go:558`; private; 1 use(s).
- `coordinator.currentAgent` — field; `coordinator.go:196`; private; 3 use(s).
- `coordinator.noteSubAgentChildRunEnded` — method; `coordinator_work_scope.go:158`; private; 4 use(s).
- `coordinator.notify` — field; `coordinator.go:112`; private; 2 use(s).
- `coordinator.notifyCallbackHook` — field; `coordinator.go:130`; private; 2 use(s).
- `coordinator.notifyWG` — field; `coordinator.go:120`; private; 2 use(s).
- `coordinator.permissions` — field; `coordinator.go:108`; private; 5 use(s).
- `coordinator.persistentMode` — field; `coordinator.go:247`; private; 1 use(s).
- `coordinator.readyWg` — field; `coordinator.go:211`; private; 1 use(s).
- `coordinator.recheckWakes` — field; `coordinator.go:280`; private; 2 use(s).
- `coordinator.resetConsecutiveResume` — method; `coordinator.go:566`; private; 1 use(s).
- `coordinator.runAwaitingAdmission` — method; `coordinator_run_await.go:31`; private; 1 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 15 use(s).
- `coordinator.subAgentDrivers` — field; `coordinator.go:150`; private; 8 use(s).
- `errModelProviderNotConfigured` — var; `coordinator.go:49`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → delegation</code> — 13 declarations, 19 uses</summary>

- `ErrAgentDepthExceeded` — var; `delegation_depth.go:17`; exported; 1 use(s).
- `MaxAgentDepthFrom` — func; `delegation_depth.go:22`; exported; 1 use(s).
- `bindDelegationRequestedRole` — func; `delegation_role_state.go:70`; private; 1 use(s).
- `bindDelegationRoleContract` — func; `delegation_role_state.go:56`; private; 3 use(s).
- `coordinator.resolveRequestedDelegationModel` — method; `delegation_role_models.go:49`; private; 1 use(s).
- `delegationRoleModel` — type; `delegation_role_models.go:11`; private; 1 use(s).
- `delegationRoleModel.Credentials` — field; `delegation_role_models.go:15`; exported; 1 use(s).
- `delegationRoleModel.Model` — field; `delegation_role_models.go:12`; exported; 2 use(s).
- `delegationRoleModel.Provider` — field; `delegation_role_models.go:13`; exported; 1 use(s).
- `delegationRoleModel.Source` — field; `delegation_role_models.go:14`; exported; 1 use(s).
- `effectiveParentAgentDepth` — func; `delegation_depth.go:50`; private; 2 use(s).
- `parentRoleContractFrom` — func; `delegation_role_models.go:159`; private; 3 use(s).
- `resolveDelegationRequestedRole` — func; `delegation_role_state.go:12`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → drainctl</code> — 4 declarations, 4 uses</summary>

- `arbiter.overCapCount` — method; `turn_arbiter_state.go:484`; private; 1 use(s).
- `arbiter.pruneOverCap` — method; `turn_arbiter_state.go:265`; private; 1 use(s).
- `callFromActive` — func; `agent_drain.go:75`; private; 1 use(s).
- `clearCancelRequest` — func; `cancel_flag.go:25`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → finalize</code> — 1 declarations, 1 uses</summary>

- `finalizationLogicalIdentity` — func; `finalization_driver_binding.go:9`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → modelkit</code> — 21 declarations, 27 uses</summary>

- `ResolveRoleContract` — func; `custom_role_runtime.go:14`; exported; 1 use(s).
- `WithRoleContractCopy` — func; `custom_role_runtime.go:211`; exported; 1 use(s).
- `WithSelectedRoleContract` — func; `custom_role_runtime.go:192`; exported; 1 use(s).
- `callModelRole` — func; `coordinator_models_role.go:83`; private; 1 use(s).
- `checkPeakHours` — func; `coordinator_providers_auth.go:125`; private; 1 use(s).
- `cloneRoleContractForCall` — func; `custom_role_runtime.go:159`; private; 2 use(s).
- `coordinator.buildModelsFromCfg` — method; `coordinator_models_build.go:92`; private; 3 use(s).
- `coordinator.isUnauthorized` — method; `coordinator_providers_auth.go:234`; private; 1 use(s).
- `coordinator.rebuildInputs` — method; `coordinator_providers.go:506`; private; 1 use(s).
- `coordinator.rebuildPinnedModel` — method; `coordinator_providers.go:475`; private; 1 use(s).
- `coordinator.refreshOAuth2Token` — method; `coordinator_providers_auth.go:242`; private; 1 use(s).
- `coordinator.refreshSubAgentSlotModel` — method; `coordinator_models_role.go:164`; private; 1 use(s).
- `coordinator.resolveSubAgentModelOverride` — method; `coordinator_models.go:332`; private; 1 use(s).
- `coordinator.runWithUnauthorizedRetry` — method; `coordinator_providers_auth.go:197`; private; 1 use(s).
- `coordinator.workerSubAgentActiveForCall` — method; `coordinator_models_role.go:12`; private; 1 use(s).
- `copyInheritedRoleContract` — func; `custom_role_runtime.go:167`; private; 1 use(s).
- `currentOrSnapshotRoleModel` — func; `custom_role_runtime.go:131`; private; 3 use(s).
- `errUnauthorizedRefreshUnavailable` — var; `coordinator_providers_auth.go:193`; private; 1 use(s).
- `getProviderOptions` — func; `coordinator_providers.go:109`; private; 2 use(s).
- `roleContractWithName` — func; `custom_role_runtime.go:179`; private; 1 use(s).
- `selectedRoleContractFrom` — func; `custom_role_runtime.go:198`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → sessionprompt</code> — 2 declarations, 2 uses</summary>

- `autoResumedCtxKey` — type; `agent_prompt.go:59`; private; 1 use(s).
- `backgroundJobNoticeCtxKey` — type; `agent_prompt.go:63`; private; 1 use(s).

</details>

<details>
<summary><code>subagents → wakectl</code> — 1 declarations, 2 uses</summary>

- `coordinator.wakeSession` — method; `coordinator_wake.go:27`; private; 2 use(s).

</details>

<details>
<summary><code>subagents → watchdog</code> — 2 declarations, 2 uses</summary>

- `coordinator.admitCallback` — method; `supervision.go:504`; private; 1 use(s).
- `recheckDebtCheckBudget` — var; `supervision.go:53`; private; 1 use(s).

</details>

<details>
<summary><code>toolwrap → agenticfetch</code> — 2 declarations, 2 uses</summary>

- `privateFetchIngressTool` — type; `agentic_fetch_policy.go:113`; private; 1 use(s).
- `privateFetchIngressTool.AgentTool` — field; `agentic_fetch_policy.go:114`; exported; 1 use(s).

</details>

<details>
<summary><code>toolwrap → agentrun</code> — 2 declarations, 2 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 1 use(s).
- `sessionAgent.tools` — field; `agent.go:235`; private; 1 use(s).

</details>

<details>
<summary><code>toolwrap → batchflow</code> — 2 declarations, 2 uses</summary>

- `batchCredentialLeaf` — type; `batch_credentials_escrow.go:29`; private; 1 use(s).
- `batchCredentialLeaf.AgentTool` — field; `batch_credentials_escrow.go:30`; exported; 1 use(s).

</details>

<details>
<summary><code>toolwrap → batchinst</code> — 2 declarations, 2 uses</summary>

- `batchIngressTool` — type; `batch_install_tools.go:84`; private; 1 use(s).
- `batchIngressTool.AgentTool` — field; `batch_install_tools.go:85`; exported; 1 use(s).

</details>

<details>
<summary><code>toolwrap → batchrun</code> — 2 declarations, 2 uses</summary>

- `pctCapturedAgentTool` — type; `batch_child_tool_scope.go:77`; private; 1 use(s).
- `pctCapturedAgentTool.AgentTool` — field; `batch_child_tool_scope.go:78`; exported; 1 use(s).

</details>

<details>
<summary><code>toolwrap → turnstep</code> — 2 declarations, 2 uses</summary>

- `progressGuardTool` — type; `turn_progress_guard.go:225`; private; 1 use(s).
- `progressGuardTool.inner` — field; `turn_progress_guard.go:226`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → agentrun</code> — 29 declarations, 81 uses</summary>

- `chainNeutralTools` — var; `agent_turn_step.go:421`; private; 1 use(s).
- `largeContextHeadroomDivisor` — const; `agent.go:45`; private; 1 use(s).
- `largeContextWindowBuffer` — const; `agent.go:40`; private; 1 use(s).
- `largeContextWindowThreshold` — const; `agent.go:39`; private; 1 use(s).
- `sessionAgent` — type; `agent.go:230`; private; 18 use(s).
- `sessionAgent.Run` — method; `agent_run.go:30`; exported; 1 use(s).
- `sessionAgent.checkpointInterval` — field; `agent.go:376`; private; 2 use(s).
- `sessionAgent.dataDir` — field; `agent.go:360`; private; 2 use(s).
- `sessionAgent.effectiveStreamWatchdogTick` — method; `agent_timeouts.go:103`; private; 1 use(s).
- `sessionAgent.effectiveTitleGenerationMaxDuration` — method; `agent_timeouts.go:91`; private; 1 use(s).
- `sessionAgent.lockOptions` — field; `agent.go:364`; private; 1 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 12 use(s).
- `sessionAgent.runWg` — field; `agent.go:274`; private; 5 use(s).
- `sessionAgent.sessions` — field; `agent.go:320`; private; 8 use(s).
- `sessionAgent.smartModel` — field; `agent.go:231`; private; 1 use(s).
- `sessionAgent.streamIdleTimeout` — field; `agent.go:328`; private; 2 use(s).
- `sessionAgent.summarizeQueue` — field; `agent.go:430`; private; 6 use(s).
- `sessionAgent.systemPromptPrefix` — field; `agent.go:233`; private; 1 use(s).
- `sessionAgent.timeoutExtendsOnProgress` — field; `agent.go:380`; private; 1 use(s).
- `sessionAgent.timeoutHardCap` — field; `agent.go:384`; private; 3 use(s).
- `sessionAgent.titleGens` — field; `agent.go:356`; private; 2 use(s).
- `sessionAgent.tryAdmitRunWg` — method; `agent_control.go:143`; private; 1 use(s).
- `smallContextWindowRatio` — const; `agent.go:41`; private; 1 use(s).
- `streamIdleTimeoutDefault` — const; `agent.go:65`; private; 1 use(s).
- `turnConfig` — type; `agent.go:194`; private; 2 use(s).
- `turnConfig.fastModel` — field; `agent.go:196`; private; 1 use(s).
- `turnConfig.promptPrefix` — field; `agent.go:198`; private; 1 use(s).
- `turnConfig.smartModel` — field; `agent.go:195`; private; 1 use(s).
- `unreportedCharsPerToken` — const; `agent.go:48`; private; 2 use(s).

</details>

<details>
<summary><code>turnstep → batchinst</code> — 7 declarations, 8 uses</summary>

- `asyncCapMetadata` — type; `batch_rehome_tool_metadata.go:30`; private; 1 use(s).
- `asyncCapMetadata.<anonymous@batch_rehome_tool_metadata.go:31>.Limit` — field; `batch_rehome_tool_metadata.go:33`; exported; 1 use(s).
- `asyncCapMetadata.<anonymous@batch_rehome_tool_metadata.go:31>.Running` — field; `batch_rehome_tool_metadata.go:32`; exported; 1 use(s).
- `asyncCapMetadata.AsyncCap` — field; `batch_rehome_tool_metadata.go:31`; exported; 2 use(s).
- `asyncToolMetadata` — type; `batch_rehome_tool_metadata.go:11`; private; 1 use(s).
- `asyncToolMetadata.Async` — field; `batch_rehome_tool_metadata.go:12`; exported; 1 use(s).
- `asyncToolMetadata.Inline` — field; `batch_rehome_tool_metadata.go:17`; exported; 1 use(s).

</details>

<details>
<summary><code>turnstep → callbeat</code> — 4 declarations, 6 uses</summary>

- `addHeartbeatUsage` — func; `heartbeat_ctx.go:152`; private; 2 use(s).
- `hbFastSource` — func; `heartbeat_ctx.go:121`; private; 1 use(s).
- `hbInherited` — func; `heartbeat_ctx.go:128`; private; 1 use(s).
- `withHeartbeat` — func; `heartbeat_ctx.go:66`; private; 2 use(s).

</details>

<details>
<summary><code>turnstep → coord</code> — 2 declarations, 4 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 2 use(s).
- `coordinator.arb` — field; `coordinator.go:252`; private; 2 use(s).

</details>

<details>
<summary><code>turnstep → drainctl</code> — 5 declarations, 6 uses</summary>

- `arbiter.held` — method; `turn_arbiter_state.go:155`; private; 1 use(s).
- `arbiter.hold` — method; `turn_arbiter_state.go:136`; private; 1 use(s).
- `stallClock` — type; `turn_stall_clock.go:36`; private; 2 use(s).
- `stallClock.markProgress` — method; `turn_stall_clock.go:168`; private; 1 use(s).
- `stallSetPhase` — func; `turn_stall_clock.go:152`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → mailbox</code> — 15 declarations, 28 uses</summary>

- `mailbox` — type; `mailbox.go:132`; private; 1 use(s).
- `mailbox.abandonOwnershipAndPopFirstSubmitted` — method; `mailbox_queue.go:220`; private; 1 use(s).
- `mailbox.beginCompact` — method; `mailbox_generation.go:115`; private; 1 use(s).
- `mailbox.beginRelease` — method; `mailbox_ownership.go:496`; private; 1 use(s).
- `mailbox.drainInjects` — method; `mailbox_inject.go:72`; private; 1 use(s).
- `mailbox.finishRelease` — method; `mailbox_ownership.go:522`; private; 1 use(s).
- `mailbox.testPreAbandonSeam` — field; `mailbox.go:243`; private; 2 use(s).
- `mailbox.testPreSnapshotConsumeSeam` — field; `mailbox.go:253`; private; 2 use(s).
- `notifyActivity` — func; `agent_ownership.go:753`; private; 5 use(s).
- `notifyWatchdog` — func; `agent_ownership.go:769`; private; 4 use(s).
- `pendingInject.msg` — field; `mailbox.go:123`; private; 2 use(s).
- `sessionAgent.abandonOwnershipWithHandoff` — method; `agent_ownership.go:296`; private; 3 use(s).
- `sessionAgent.getMailbox` — method; `agent_ownership.go:27`; private; 2 use(s).
- `withActivityNotify` — func; `agent_ownership.go:738`; private; 1 use(s).
- `withWatchdogBump` — func; `agent_ownership.go:762`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → modelkit</code> — 1 declarations, 1 uses</summary>

- `zai53ModelIDs` — var; `coordinator_providers.go:73`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → sessionprompt</code> — 4 declarations, 5 uses</summary>

- `estimateMsgChars` — func; `agent_prompt.go:577`; private; 1 use(s).
- `sessionAgent.getSessionMessages` — method; `agent_prompt.go:330`; private; 1 use(s).
- `sessionAgent.preparePrompt` — method; `agent_prompt.go:146`; private; 1 use(s).
- `sessionHeaders` — func; `agent_prompt.go:49`; private; 2 use(s).

</details>

<details>
<summary><code>turnstep → toolwrap</code> — 1 declarations, 1 uses</summary>

- `restrictedToolActions` — var; `restricted_tool.go:15`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → usage</code> — 5 declarations, 8 uses</summary>

- `approxTokenCount` — func; `usage_fallback.go:302`; private; 2 use(s).
- `normalizeProviderUsage` — func; `usage_normalize.go:26`; private; 2 use(s).
- `sessionAgent.openrouterCost` — method; `agent_usage.go:51`; private; 2 use(s).
- `sessionAgent.recordMessageUsage` — method; `usage_record.go:46`; private; 1 use(s).
- `sessionAgent.updateSessionUsage` — method; `agent_usage.go:82`; private; 1 use(s).

</details>

<details>
<summary><code>turnstep → watchdog</code> — 8 declarations, 12 uses</summary>

- `causeHardCap` — const; `stream_watchdog.go:45`; private; 2 use(s).
- `causeToolTimeout` — const; `stream_watchdog.go:39`; private; 2 use(s).
- `startStreamWatchdog` — func; `stream_watchdog.go:195`; private; 1 use(s).
- `streamWatchdog` — type; `stream_watchdog.go:59`; private; 2 use(s).
- `streamWatchdog.bump` — field; `stream_watchdog.go:63`; private; 1 use(s).
- `streamWatchdog.disarm` — field; `stream_watchdog.go:104`; private; 1 use(s).
- `streamWatchdog.done` — field; `stream_watchdog.go:107`; private; 1 use(s).
- `watchdogCause` — type; `stream_watchdog.go:27`; private; 2 use(s).

</details>

<details>
<summary><code>usage → agentrun</code> — 2 declarations, 4 uses</summary>

- `sessionAgent` — type; `agent.go:230`; private; 3 use(s).
- `sessionAgent.messages` — field; `agent.go:321`; private; 1 use(s).

</details>

<details>
<summary><code>usage → coord</code> — 5 declarations, 9 uses</summary>

- `PeakHoursError` — type; `coordinator.go:70`; exported; 1 use(s).
- `PeakHoursError.Message` — field; `coordinator.go:76`; exported; 2 use(s).
- `PeakHoursError.ReopensAt` — field; `coordinator.go:73`; exported; 3 use(s).
- `PeakHoursError.Timezone` — field; `coordinator.go:79`; exported; 2 use(s).
- `coordinator` — type; `coordinator.go:104`; private; 1 use(s).

</details>

<details>
<summary><code>usage → modelkit</code> — 4 declarations, 5 uses</summary>

- `checkPeakHours` — func; `coordinator_providers_auth.go:125`; private; 1 use(s).
- `coordinator.checkLivePeakHours` — method; `coordinator_providers_auth.go:96`; private; 1 use(s).
- `isQuotaLimit` — func; `coordinator_retry_classify.go:189`; private; 1 use(s).
- `structuredLimitReset` — func; `provider_limit.go:50`; private; 2 use(s).

</details>

<details>
<summary><code>usage → promptcache</code> — 4 declarations, 5 uses</summary>

- `cacheProfile.inputIncludesCache` — field; `cache_profile.go:32`; private; 1 use(s).
- `cacheProfile.knownCacheReporter` — field; `cache_profile.go:44`; private; 1 use(s).
- `cacheProfileFor` — func; `cache_profile.go:113`; private; 2 use(s).
- `cacheReporterProviderIDs` — var; `cache_profile.go:107`; private; 1 use(s).

</details>

<details>
<summary><code>wakectl → batchinst</code> — 5 declarations, 5 uses</summary>

- `ErrCredentialBindingUnavailable` — var; `disk_provider_durable.go:40`; exported; 1 use(s).
- `ErrDiskProviderUnavailable` — var; `disk_provider_durable.go:35`; exported; 1 use(s).
- `batchDriverHints.bumpHint` — method; `batch_rehome_coord_runtime.go:46`; private; 1 use(s).
- `batchDriverHints.isExternalDriver` — method; `batch_rehome_coord_runtime.go:130`; private; 1 use(s).
- `preparedContinuation.release` — field; `disk_provider_continuation.go:31`; private; 1 use(s).

</details>

<details>
<summary><code>wakectl → coord</code> — 5 declarations, 35 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 17 use(s).
- `coordinator.addToRecheckSet` — method; `coordinator_recheck.go:42`; private; 3 use(s).
- `coordinator.clearFromRecheckSet` — method; `coordinator_recheck.go:58`; private; 1 use(s).
- `coordinator.driverHints` — field; `coordinator.go:136`; private; 2 use(s).
- `coordinator.wakeScheduler` — field; `coordinator.go:173`; private; 12 use(s).

</details>

<details>
<summary><code>wakectl → drainctl</code> — 8 declarations, 8 uses</summary>

- `coordinator.drainCallFor` — method; `agent_drain.go:111`; private; 1 use(s).
- `coordinator.drainDecision` — method; `coordinator_drain_policy.go:152`; private; 1 use(s).
- `coordinator.noteDrainRefused` — method; `drain_attempt.go:450`; private; 1 use(s).
- `drainDeferred` — const; `coordinator_drain_policy.go:24`; private; 1 use(s).
- `drainPaced` — const; `coordinator_drain_policy.go:27`; private; 1 use(s).
- `drainStuck` — const; `coordinator_drain_policy.go:30`; private; 1 use(s).
- `drainVerdict.kind` — field; `coordinator_drain_policy.go:36`; private; 1 use(s).
- `drainVerdict.recheck` — field; `coordinator_drain_policy.go:38`; private; 1 use(s).

</details>

<details>
<summary><code>wakectl → subagents</code> — 1 declarations, 1 uses</summary>

- `coordinator.agentFor` — method; `coordinator_subagent_drivers.go:237`; private; 1 use(s).

</details>

<details>
<summary><code>wakectl → watchdog</code> — 1 declarations, 1 uses</summary>

- `recheckDebtCheckBudget` — var; `supervision.go:53`; private; 1 use(s).

</details>

<details>
<summary><code>watchdog → batchflow</code> — 6 declarations, 8 uses</summary>

- `BatchCutoverFacts.LiveDelegationChildOf` — method; `batch_cutover_facts.go:70`; exported; 1 use(s).
- `BatchCutoverFacts.LiveDelegationNodes` — method; `batch_cutover_facts.go:77`; exported; 1 use(s).
- `BatchCutoverFacts.LiveHeldQuestions` — method; `batch_cutover_facts.go:107`; exported; 1 use(s).
- `BatchCutoverFacts.LiveNodes` — method; `batch_cutover_facts.go:72`; exported; 1 use(s).
- `BatchCutoverFacts.LiveWorkSummary` — method; `batch_cutover_facts.go:60`; exported; 1 use(s).
- `coordinatorBatchCutoverFacts` — func; `batch_cutover_facts.go:220`; private; 3 use(s).

</details>

<details>
<summary><code>watchdog → batchinst</code> — 1 declarations, 1 uses</summary>

- `coordinator.sessionNoticeStore` — method; `batch_rehome_coord_runtime.go:194`; private; 1 use(s).

</details>

<details>
<summary><code>watchdog → coord</code> — 15 declarations, 50 uses</summary>

- `coordinator` — type; `coordinator.go:104`; private; 14 use(s).
- `coordinator.autoResumeSuspended` — method; `coordinator.go:588`; private; 1 use(s).
- `coordinator.callbackCancel` — field; `coordinator.go:126`; private; 1 use(s).
- `coordinator.callbackClosing` — field; `coordinator.go:124`; private; 1 use(s).
- `coordinator.callbackCtx` — field; `coordinator.go:125`; private; 3 use(s).
- `coordinator.callbackMu` — field; `coordinator.go:123`; private; 2 use(s).
- `coordinator.cfg` — field; `coordinator.go:105`; private; 2 use(s).
- `coordinator.noteSubAgentChildRunEnded` — method; `coordinator_work_scope.go:158`; private; 1 use(s).
- `coordinator.releaseCallbackHook` — field; `coordinator.go:129`; private; 2 use(s).
- `coordinator.releaseWG` — field; `coordinator.go:127`; private; 4 use(s).
- `coordinator.sessions` — field; `coordinator.go:106`; private; 2 use(s).
- `coordinator.subAgentDrivers` — field; `coordinator.go:150`; private; 2 use(s).
- `coordinator.supervision` — field; `coordinator.go:140`; private; 10 use(s).
- `coordinator.timeouts` — field; `coordinator.go:141`; private; 4 use(s).
- `streamStalledFinishTitle` — const; `coordinator_run.go:52`; private; 1 use(s).

</details>

<details>
<summary><code>watchdog → subagents</code> — 4 declarations, 6 uses</summary>

- `coordinator.agentFor` — method; `coordinator_subagent_drivers.go:237`; private; 2 use(s).
- `coordinator.releaseDriverGeneration` — method; `coordinator_subagent_drivers.go:274`; private; 1 use(s).
- `subAgentDriver.generation` — field; `coordinator_subagent_drivers.go:70`; private; 1 use(s).
- `subAgentDriverRegistry.get` — method; `coordinator_subagent_drivers.go:145`; private; 2 use(s).

</details>

<details>
<summary><code>watchdog → wakectl</code> — 1 declarations, 2 uses</summary>

- `coordinator.wakeSession` — method; `coordinator_wake.go:27`; private; 2 use(s).

</details>

## Amended design

**Projection status: MEASURED (symbol-overrides projection; ownership-only).** The baseline layout and appendices above remain unchanged as historical baseline evidence. Correction of the earlier PENDING claim (P2c): Rush did record a successful build of the pre-fix symgraph binary (`symgraph.exe`, 8582144 bytes, mtime 2026-11-16 22:41:18), and the corrected binary was subsequently rebuilt via `golock` (exit 0) and validated by `smoke/projection-smoke/check.py` with **PASS: 8 checks** (old CLI baseline; old CLI move; old CLI retarget; projection retarget and hidden-field dependency; reject unknown override; reject member override; reject duplicate override; reject multi-name override). The measured amended receipt is `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-amended-fixed.json` (smoke JSON: `smoke-fix.json`). A `ReceiverTypeLine` bug in `cmd/projection.go` (method position reported as the receiver type line) was fixed and covered by the PASS-8 suite; the projected receipt reports **0 receiver-colocation violations**.

The receiver source is [`2026-11-16-sprint0-agent-receivers.tsv`](2026-11-16-sprint0-agent-receivers.tsv) (285 method records; the orchestrator independently compared method, receiver, current file, assigned owner, and line against baseline JSON). The amended [`2026-11-16-sprint0-agent-amended-overrides.tsv`](2026-11-16-sprint0-agent-amended-overrides.tsv) has exactly five columns: `file`, `symbol`, `owner`, `target_file`, `receiver`; its 304 unique override rows include 285 mapped receiver-source rows and 19 explicit additions. Counts are **a=70, b=0, c=215** (retarget proposal / none / retained glue) for the receiver source. The 70 `a` rows are distributed by lane: EX-0A 16, EX-0B 12, EX-0E 6, EX-0F 14, EX-0G 9, EX-0H 13. EX-0C/0D/0K have no `a` rows in this projection. For an `a` row, `receiver` names `owner.Component`; for `c`, it is empty. The tool applies owner/receiver-token assignments only: body field references and state ownership are not rewritten or proved, so these 70 rows do not assert implemented retargets. Methods on moved types require explicit overrides (the `PeakHoursError.Error` and `.Unwrap` rows have empty receiver because the named type moves); fields inherit the moved type's ownership and have no independent rows. Every `target_file` is owner-relative (filename only). Additional assignments include `cachedModelPair` (`modelkit/cache_contract.go`), `coordinator.agentTool` (`coord/tool_glue.go`), `observedCacheReadTokens` (`agentrun/cache_methods.go`), `ErrFinalizationScopeChanged` (`batchinst/gate_contract.go`), and quota/peak-hours declarations. See the exact gap ledger in [`2026-11-16-sprint0-agent-amended-gaps.tsv`](2026-11-16-sprint0-agent-amended-gaps.tsv).

### Measured amended projection (agent-amended-fixed.json)

Measured with the current **381-row** overrides TSV (304 prior rows + 64 `batchcontract` rows and 24 `batchassemble` rows minus one reconciled duplicate: 11 `BatchInstall` CLI receiver rows were updated in place, so 77 planned additions net to +77). Summary: **edges 166, witnesses 2,220, cross-boundary uses 5,236**; `acyclic false`; **exactly ONE SCC of 20 owners** (`agenticfetch`, `agentrun`, `batchassemble`, `batchflow`, `batchinst`, `batchrun`, `clibridge`, `coord`, `delegation`, `drainctl`, `finalize`, `mailbox`, `modelkit`, `provauth`, `sessionprompt`, `subagents`, `toolwrap`, `turnstep`, `wakectl`, `watchdog`) — `callbeat`, `promptcache`, `responsesws`, `usage`, `agentcall`, and `root` are outside the SCC. Physical and projected over-limit packages are the same 12: `agentrun`, `batchflow`, `batchinst`, `batchrun`, `coord`, `drainctl`, `finalize`, `mailbox`, `modelkit`, `toolwrap`, `turnstep`, `usage`. Receiver-colocation violations: 0 (projected). The tool's `must_export` ledger has 152 boundaries; key batch/modelkit boundary symbol counts (unexported symbols an actual package split would have to export or port): `batchinst→batchrun` 36, `batchrun→batchinst` 50, `batchinst→batchflow` 36, `batchflow→batchinst` 29, `batchrun→batchflow` 8, `batchflow→batchrun` 2, `batchinst→batchcontract` 32, `batchrun→batchcontract` 11, `batchassemble→batchinst` 42, `coord→modelkit` 93, `modelkit→coord` 7, `finalize→batchinst` 38, `watchdog→coord` 17, `toolwrap→agenticfetch` 1.

**Ownership-only projection cannot remove body/state dependencies:** the 70 receiver retargets and the leaf/sentinel moves changed declaration placement and receiver tokens only; the measured graph still contains every documented reciprocal pair. Measured directed edges (witness entries/uses) for the exact remaining cycle decisions:

- `batchinst ↔ batchrun`: 66/146 forward, 86/129 reverse — blocked by `BatchInvocation.roleConfig *config.Config` (`batch_invocation.go:135-136`), the `BatchLeafRegistry = policyLeafRegistry` alias (`batch_install_ingress.go:24`), and implementation-owned ingress/runtime state; the 64 `batchcontract` rows are encoded but the transport closure is not finished, so neither edge is counted as removed.
- `batchinst ↔ batchflow`: 39/65 forward, 36/49 reverse — lease state (`batchRootLeaseStore.mu`/`entries`, `batch_root_lease.go:21-30`) stays in batchflow; the port set (`Acquire`/`Adopt`/`Release`/`Matches`/`OwnerForRoot`, `DeliverBatch`) remains proposed API, and the composition boundary (`newBatchInstall` in `batchassemble`) must inject implementations without a global.
- `batchrun ↔ batchflow`: 9/11 forward, 2/2 reverse — payload/delivery contract placement is the same unfinished `batchcontract` closure.
- `batchinst ↔ finalize`: the measured graph shows **no `batchinst → finalize` edge any more** — the `ErrFinalizationScopeChanged` sentinel move (`finalization_publish.go:13` → `batchinst/gate_contract.go`) removed the last forward witness (`finalization_gate.go:212`); the remaining direction is `finalize → batchinst` at 42/124 (intentional gate-ownership direction, retained).
- `coord ↔ modelkit`: 107/296 forward, 7/9 reverse — the EX-0B coordinator state bundle owner (`cfg`, `sessions`, `currentAgent`, `readyWg`, model cache) and the `coordinator.agentTool` split are still undecided; no seam removes these uses.
- `watchdog → coord`: 17/40, including `coordinator.wakeSession` (`coordinator_wake.go:27`, 2 uses) — this pair needs a named narrow port (wake capability), not an assertion of removal.
- `toolwrap ↔ agenticfetch`: 2/2 forward, 11/24 reverse — EX-0N's `PolicyLeafTool() fantasy.AgentTool` capability is still unimplemented in source; the reverse edge is intentional one-way and may remain.

### Exact outstanding seam blockers

- **EX-0B / modelkit:** 72 receiver-source methods are coordinator-owned `c` glue, not component retargets. They remain attached to `coord`; the proposal groups them in `modelkit_glue.go`, except `coordinator.agentTool` in `tool_glue.go`. Their bodies cite `cfg`, `sessions`, `currentAgent`, `readyWg`, model-cache and other coordinator state. The accepted `AutonomySource` pair does not define this state bundle or its owner boundary. Do not claim that owner/receiver overrides changed these bodies or removed their references.
- **EX-0H / turnBudget:** candidate compaction/context methods directly reference `runWg`, `summarizeQueue`, and watchdog/timeout state. `ContextBudgetHost` does not define the same-time host operations for these references. Until state ownership and each operation are explicitly assigned without eager evaluation or changed ordering, these methods remain unproven extractions; a receiver-token override alone does not solve this.
- **toolwrap → agenticfetch:** the proposed wrapper port is not implemented by an owner override. It targets the 2 concrete-type/field witnesses shown below; the reverse agenticfetch → toolwrap dependency is an intentional one-way edge, not a blocker. Keep the status as a proposal until source implementation and an amended graph receipt confirm it.
- **Batch lower contract / composition split — exact unresolved closure:** propose `batchcontract` for shared transport declarations and immutable payloads, and `batchassemble` for `BatchInstall`, factories, and upper composition. `batchcontract` defines ports, not the mutex-backed lease map. Known candidates are `BatchInvocation`, `BatchCLITask`, `BatchWorkIngress`, `batchDelivery`, `batchLeafPolicyContext`, and `batchDeliveryChoice`. The exact closure remains blocked by `BatchInvocation.roleConfig *config.Config` (`batch_invocation.go:135-136`), `BatchWorkIngress`'s `BatchLeafRegistry` alias and owner boundary (`batch_install_ingress.go:22-47`), runtime/delivery implementation state, and the root-lease store's `mu`/`entries` (`batch_root_lease.go:21-30`). Define the transport-only declaration closure and adapters before claiming any projected edge removed.

### Eight cycle-repair proposals (all unmeasured)

1. **modelkit ↔ usage — quota and peak-hours leaf moves.** Move the complete receiver-free `provider_limit.go` closure to `usage/quota_limits.go`, and move `isQuotaLimit(providerErr *fantasy.ProviderError) bool` out of `coordinator_retry_classify.go` there. Also move `checkPeakHours(providerCfg config.ProviderConfig) error` from `coordinator_providers_auth.go` as a cohesive closure with `PeakHoursError`, its `Error() string`/`Unwrap() error` methods, and `errProviderPeakHours` from `coordinator.go`. The inspected helper reads config/time, builds the typed error, and logs; this is a proposed package move, not a claim that all usage/modelkit edges disappear. Baseline witnesses: `QuotaLimitResetTime` (`quota_reset.go:26`) → `provider_limit.go:77`; `structuredLimitReset` (`provider_limit.go:50`) → `quota_reset.go:55,132`; reverse edge totals are in Appendix A.
2. **agentrun ↔ promptcache — colocate cache observation with the retained turn receiver.** Keep `turnStream.logCacheStepUsage(usage fantasy.Usage, estimated bool, metadata fantasy.ProviderMetadata)` with agentrun and move `observedCacheReadTokens(usage fantasy.Usage, estimated bool, provider string, metadata fantasy.ProviderMetadata) *int64` to the same owner/file family. This is a targeted declaration move; cache-diagnostic types/helpers still require measured inventory. Baseline reverse witnesses are `cachePreparedDiagnostic`, `preparedCacheDiagnostic`, `turnStream.logCacheStepUsage`, and `withTurnMCPInstructions` (Appendix B).
3. **modelkit ↔ subagents — split the coordinator method.** Keep `coordinator.agentTool(ctx context.Context) (fantasy.AgentTool, error)` on `*coordinator` in `coord/tool_glue.go`, rather than with modelkit declarations; do not invent a new request port unless the measured projection still requires one. Baseline witnesses: `subAgentParams` (`coordinator_subagents.go:26`) → `agent_tool.go:86`; `ResolveRoleContract` (`custom_role_runtime.go:14`) → `coordinator_subagents.go:161`.
4. **toolwrap ↔ agenticfetch — narrow wrapper introspection port.** Add `PolicyLeafTool() fantasy.AgentTool`; implement it on the agenticfetch private-fetch wrapper and have toolwrap call that method rather than name `*privateFetchIngressTool`. The wrapper is injected at the private ingress list construction points (`agentic_fetch_policy.go:41,107`); no global registry or mutable global is needed. Exact toolwrap → agenticfetch witnesses are `privateFetchIngressTool` (`agentic_fetch_policy.go:113`) and its `AgentTool` field (`:114`), 2 declarations/2 uses; this is the direction the capability removes. The reverse agenticfetch → toolwrap edge (12 declarations/27 uses: `ErrRoleToolDenied`, `decoratePolicyTools`, `guardRoleToolCall`, `newPolicyToolBuildForContext`, `policyLeafRegistry` and `resolveForContext`, `policyToolBuild` fields, `restrictedRunAuthorizer`, and `roleContractForToolBuild`) is an intentional dependency direction and may remain. A one-way dependency is sufficient; do not classify the reverse edge as a blocker to this cycle repair.
5. **batchinst ↔ batchrun — lower shared transport; upper installer stays upper.** Use `batchcontract` for the smallest shared immutable invocation/payload and validation declarations; move `BatchInstall`, factory construction, and composition to `batchassemble`, not down into executors. Exact baseline witnesses: `BatchInvocation` (`batch_invocation.go:98`) → `batch_ingress.go:27` (16 uses), and `BatchWorkIngress` (`batch_install_ingress.go:37`) → `batch_exec.go:89` (3 uses); edge totals are 139/384 and 101/154 witnesses/uses respectively. The closure remains explicitly blocked by `BatchInvocation.roleConfig *config.Config`, the `BatchLeafRegistry` alias, and implementation-owned ingress/runtime state; do not count either edge as deleted yet.
6. **batchinst ↔ batchflow — neutral lease contract, lease state stays in batchflow.** Keep `batchRootLeaseStore.mu`/`entries` and its implementation in batchflow. Put only the narrow contract in `batchcontract`, with these proposed methods: `Acquire(jobbatch.ID, string, string, string) error`; `Adopt(jobbatch.ID, <-chan struct{}, string, string, func(context.Context, jobbatch.ID) error) error`; `Release(jobbatch.ID, string, string)`; `Matches(jobbatch.ID, string, string) bool`; `OwnerForRoot(jobbatch.ID) string`. Delivery remains the existing `DeliverBatch(ctx context.Context, root jobbatch.ID, inv BatchInvocation) (fantasy.ToolResponse, error)` shape from `batch_runtime.go:45-47`. Exact baseline witnesses: `BatchRootLeaseStore` (`batch_root_lease.go:40`) → `batch_install_adapters.go:687` (1), and `BatchInstall` (`batch_install_adapters.go:668`) → `batch_cli_runtime.go:17` (11); reverse edge is 65/160 witnesses/uses. Port signatures are proposed API, not evidence of move completion; the shared transport closure remains the blocker above.
7. **batchrun ↔ batchflow — neutral invocation/delivery payload, implementations remain directional.** `batchcontract` owns only shared invocation/delivery payload declarations and pure validation; batchrun retains execution and batchflow retains delivery. The `batchassemble.newBatchInstall` constructor injects the components without a global. Preserve the existing lazy runtime ingress lookup in `batch_install_adapters.go:841` as a function/provider evaluated at the same point; do not replace it with an eagerly captured value. Exact baseline witnesses: `BatchInvocation` (`batch_invocation.go:98`) → `batch_delivery.go:114` (2), and `BatchCLITask` (`batch_cli.go:19`) → `batch_external_spec.go:72` (1); edge totals are 35/61 and 15/19 witnesses/uses. The payload/validation closure is the same unresolved inventory blocker, so no edge deletion is claimed.
8. **batchinst ↔ finalize — one-sentinel move; preserve receiver ownership.** Move only `ErrFinalizationScopeChanged` from `finalization_publish.go:13` to `batchinst/gate_contract.go`. Keep `finalizationGate` and its methods with batchinst according to receiver ownership; do not move the gate type or its state to finalize. This removes the specific `batchinst → finalize` sentinel witness only if the measured graph confirms the moved declaration is the sole remaining edge in that direction; finalize → batchinst remains intentionally valid. Other baseline blockers to inspect in the measured result are `BatchInstall.RootLeases` (`batch_install_adapters.go:687`) → finalize and `finalizationGate` (`finalization_gate.go:63`) → finalize files (4 uses; reverse edge 46/133 witnesses/uses). D1 already assigns the gate type to batchinst; this minimal sentinel move is not a blanket batchinst/finalize cycle claim.

### Provisional package set and lane order — MEASURED-BLOCKED candidate

Candidate labels remain the original 24 plus candidate leaves `batchcontract` and `batchassemble` (both now encoded in the overrides TSV and visible as projected owners in the receipt: `batchcontract` 4 projected distinct target files, `batchassemble` 5). The measured result keeps the package set **BLOCKED**, not a final layout: the measured single 20-owner SCC, the 12 over-limit packages (physical = projected), and the pair totals above are the remaining blockers — EX-0B's coordinator state bundle owner; EX-0H's `runWg`/`summarizeQueue`/watchdog same-time host operations; the batch transport closure (`roleConfig`, `BatchLeafRegistry`, lease/runtime/delivery state) with the upper `batchassemble` composition boundary; the unimplemented `PolicyLeafTool` capability; and the `watchdog→coord` wake port.

Provisional order: EX-0A, EX-0B, EX-0C, EX-0D, EX-0E, EX-0F, EX-0G, EX-0H, EX-0K; then **EX-0L** (batchcontract transport declarations and lease/delivery ports), **EX-0M** (batchassemble ownership/factory wiring and coordinator `agentTool` split), **EX-0N** (`PolicyLeafTool` capability), **EX-0P** (quota/peak-hours closure and cache observation); then DM1–DM4, DM5a–DM5c, DM6, DM-W, DM7, DM8, DM9. These are proposed lane scopes, not measured proof of a DAG.

### Measurement availability and reproduction

The amended projection **was measured** in this pass. Receipts (all outside the repository): corrected tool binary built via `golock` exit 0; smoke receipt `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/smoke-fix.json`; `python D:/dev/go/crush/.rush/tmp/sprint0/symgraph/smoke/projection-smoke/check.py` → **PASS: 8 checks**; amended receipt `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-amended-fixed.json` (generated with the current 381-row TSV). Reproduction:

```sh
# Working directory: D:/dev/go/crush/.rush/tmp/sprint0/symgraph/cmd
bash D:/dev/go/crush/.rush/tmp/golock.sh go build -o D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe .
python D:/dev/go/crush/.rush/tmp/sprint0/symgraph/smoke/projection-smoke/check.py   # expect PASS: 8 checks
bash D:/dev/go/crush/.rush/tmp/golock.sh D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe -dir D:/dev/go/crush/worktrees/s0-dm0-20261116/internal/agent -layout D:/dev/go/crush/worktrees/s0-dm0-20261116/docs/plans/2026-11-16-sprint0-agent-layout.tsv -overrides D:/dev/go/crush/worktrees/s0-dm0-20261116/docs/plans/2026-11-16-sprint0-agent-amended-overrides.tsv -inventory -json > D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-amended-fixed.json
# Executable self-check: rerun the smoke projection with -json (smoke-fix.json) and
# diff its summary/edges against the committed smoke receipt before trusting a rebuild.
```


## Execution order

**Start the acyclic leaf lanes now; hold every lane touching the 15-owner SCC until the operator selects a disposition for its edges.** This is a dependency handoff, not a newly measured DAG. The final projection provides eligibility only; compiler evidence establishes whether declarations can cross a package boundary. No `must_export` item authorizes an export without a compiler-proven external call site. The original stable package names/paths in D1 remain stable for all six acyclic leaves: `usage`, `promptcache`, `responsesws`, `sessionprompt`, `watchdog`, `provauth`. `provauth` may use same-package file consolidation (15 projected files → 7 candidate outputs); this is not a count/line proof. `root` is not a lane.

### Exact machine selections and acceptance oracle

Three focused manifests accompany this report:

* [`2026-11-16-sprint0-agent-execution.tsv`](2026-11-16-sprint0-agent-execution.tsv): 4,006 exact declaration inventory rows (`source_file::symbol`, source line/kind, source/target owner, target file) plus the 285 receiver inventory rows (EX disposition, component, target file). Select a whole lane by `target_owner`/`lane`; where only a declaration moves within a mixed file, select the declaration key, **not the whole source file**. This makes the manifest the exact file/declaration selection list; do not infer that every declaration in a mixed source file moves.
* [`2026-11-16-sprint0-agent-splits.tsv`](2026-11-16-sprint0-agent-splits.tsv): all 3,543 declaration rows for the 12 overlimits, with the exact current source location, projected destination, semantic split group, and named proposed destination path. Each proposed path is a design choice, unmeasured and subject to compile review; grouping is not a file-count/line-count proof and does not claim ≤7 files or ≤900 lines.
* [`2026-11-16-sprint0-agent-cycle-edge-cover.tsv`](2026-11-16-sprint0-agent-cycle-edge-cover.tsv): all 70 directed non-self arcs inside the SCC, each root arc with a BFS return path. Its 198 path rows each contain that arc's exact distinct-witness count/use count and a concrete declaration plus use file/line/column. Every internal arc is thus covered by a witnessed cycle and gets arc-specific options. This is an exhaustive **cycle-edge cover**, not enumeration of all simple cycles.

Manifest census expected from the final receipt: the 12 projected owners and declaration-row counts are `agentrun 542`, `batchassemble 77`, `batchcontract 267`, `batchflow 94`, `batchinst 999`, `coord 803`, `drainctl 275`, `finalize 44`, `mailbox 86`, `provauth 72`, `toolwrap 170`, `turnstep 114`. The projected file counts remain the authoritative overlimit values in the notice, not these declaration counts. Leaf declaration-row counts: `usage 64`, `promptcache 17`, `responsesws 70`, `sessionprompt 21`, `watchdog 40`, `provauth 72`; `root 95` refers to the shim and must not be moved as production code. Package files listed in D1 §2.3 stay in their stable directories for all six acyclic leaves; use the exact manifest source-file set for those lane selections.

| Start/order | Lane, stable package/path | Exact selection and projected declaration count | Compile-only / declaration / receiver / ordering-specific acceptance |
|---|---|---|---|
| Now 1 | DM3-usage — `usage`, `internal/agent/runtime/policy/usage` | execution manifest `target_owner=usage`; 64 declaration rows, D1 target ≤7 files | Compile-only affected package on Linux/Darwin/Windows. Declaration key-set stays 64 and owner/file census matches manifest; 0 existing receiver methods assigned to move. Keep mutable globals private; quota/peak-hours closure keeps error identity and passes config/time at use point. |
| Now 2 | DM3-promptcache — `promptcache`, `internal/agent/runtime/policy/promptcache` | `target_owner=promptcache`; 17 rows; stable 3-file target | Same compile-only matrix; 17 declaration rows, 0 existing receiver methods assigned to move. `cacheProfiles` stays private; `Profiles()`/provider use does not eagerly evaluate or add a mutable export. |
| Now 3 | DM1-responsesws — `responsesws`, `internal/agent/runtime/exec/responsesws` | `target_owner=responsesws`; 70 rows; stable 2-file target | Same compile-only matrix; 70 rows, 0 existing receiver methods assigned to move. Verify manager/wrapper declarations and the `coordinator_responses_ws.go` fragment have exactly the manifest ownership, not the old whole-file guess. |
| Now 4 | DM1-sessionprompt — `sessionprompt`, `internal/agent/runtime/exec/sessionprompt` | `target_owner=sessionprompt`; 21 rows; stable D1 5-file target | Same compile-only matrix; 21 rows; 6 existing EX-0E receiver retargets are in the inventory, but those retargets are separately ordered and are not new DM1 movements. Sweep/prompt state and call ordering stay unchanged. |
| Now 5 | DM1-watchdog — `watchdog`, `internal/agent/runtime/exec/watchdog` | `target_owner=watchdog`; 40 rows; stable D1 5-file target | Same compile-only matrix; 40 rows; 14 existing EX-0F receiver retargets are in the inventory, separately ordered from DM1 movement. Timer startup stays at its former constructor point; preserve wake-before-release below. |
| Now 6 | DM3-provauth consolidation — `provauth`, `internal/agent/runtime/policy/provauth` | `target_owner=provauth`; 72 rows across 15 projected target files; split manifest gives candidate same-package output names | Start at this package path without operator approval for new packages. Candidate consolidation is 15→7 files inside `provauth`; compile-only the package on three OSes and reconcile 72 declaration rows. Two existing EX-0H receiver retargets are in the 285-row inventory and remain separately ordered; do not count them as newly moved DM declarations. No mutable exports. |
| After edge decisions | EX-0A…EX-0P and SCC-owner DM lane portions below | Exact declaration/member and method selectors are the execution manifest; exact split allocation is split manifest. | Every affected package compile-only on Linux/Darwin/Windows; reconcile declaration keys against manifests and receiver source census after each lane. Do not run tests in this report. |

For every lane acceptance, use the rule-19 compile-only gate (not tests): `GOOS=<linux|darwin|windows> CGO_ENABLED=0 bash D:/dev/go/crush/.rush/tmp/golock.sh go test -c -o NUL -gcflags=-e -ldflags="-s -w" <each affected package>`. Capture per-package declaration counts and receiver counts before/after from the execution manifest; equality applies to a pure relocation, while an explicitly moved method must show exactly its listed receiver disposition. Then `go vet` on the three OS targets and rule-19 formatter/selfcheck gates at orchestrator acceptance. Test fixtures belong only in `_test.go`; no mutable global export; no production exports absent a compiler-proven external caller. This report ran no tests.

### Proposed semantic splits for all 12 current overlimits

These are **named execution choices only**. In every bullet the linked split manifest is the exact source declaration → target-file allocation, including declaration-level splitting of mixed source files. The `batchcli` allocations under `batchassemble` and `batchinst` go to ONE shared proposed package at `runtime/batch/batchcli`, not two packages.

* `agentrun` (46): `agentrun` core + `turn` + `prompt` + `ownership` + `drain` + `batch` + `finalization` + `lifecycle` under `runtime/exec/agentrun/`. The manifest assigns every declaration to one named group. Do not move a method merely because its old source file is grouped there; obey receiver disposition.
* `batchassemble` (9): `batchassemble` core, `batchfactory`, `batchcli`, `batchfetch`, `finalizeassembly` under `runtime/batch/`; factories/composition remain upper-layer, never in a lower executor.
* `batchcontract` (16): `batchcontract` transport/value declarations, `batchports` structural capabilities, `batchsignals` cutover/reaction/summarize/heartbeat contracts under `runtime/batch/`. Keep it declaration-only and portable.
* `batchflow` (8): `batchflow` lifecycle/delivery and `batchquestion` held-question routing under `runtime/batch/`; lease state remains batchflow.
* `batchinst` (65): `batchinst` admission/install, `batchexec` execution-adjacent payload/exec declarations, `batchdisk` disk capability/durability, `batchcli` CLI bridge under `runtime/batch/`. The split manifest resolves all 999 declaration rows; run receiver/constructor audit before committing a lane.
* `coord` (76): `coord` run/core, `wakectl`, `supervision`, `subagents`, `batchglue`, `finalizeglue`, `drain`, `modelglue`, `session` under `runtime/coord/` or their established D1 leaf directories. These candidates do not authorize a broad state bundle or receiver move.
* `drainctl` (12): `drainctl` drain/arbiter and `stallctl` retry clock/policy under `runtime/exec/`; `CLIScopeState`/`ReactionDebtSource` stay at the approved contract owner.
* `finalize` (9): `finalize` publish/settle, `finalizequeue`, `finalizeexec` under `runtime/batch/`; gate implementation remains batchinst.
* `mailbox` (11): `mailbox` generation/interrupt/queue, `sessionownership`, `queuedcall` under `runtime/exec/`; preserve atomic ownership/epoch ordering and separate finalization enqueue as allocated.
* `provauth` (15 projected files): consolidate within the existing `runtime/policy/provauth` package to these seven candidate files: `auth_transport.go`, `token_sources.go`, `context_policy.go`, `cache_policy.go`, `recovery.go`, `summary_budget.go`, `notify.go`. The split manifest allocates every exact declaration to those same-package files; this is a bounded candidate, not a line-count or compile proof. No package split is a prerequisite. An optional later package extraction requires separate operator approval and is outside this lane's acceptance.
* `toolwrap` (10): `toolwrap` decoration, `rolepolicy` leaf-contract interpretation, `childscope` separately under policy/batch paths; `PolicyLeafTool` is used only for role-contract introspection, not wrapper-copying behavior.
* `turnstep` (9): `turnstep` progress/admission/UI and `turnbudget` summarize/compaction under `runtime/exec/`; move summarizeQueue with turnBudget, but do not claim raw struct-member projection proves that field move.
* Reconciliation of old labels: D1 `usage` now projects to 5 target files, `modelkit` to 7, both within 7; `batchrun` is dissolved into batchinst; `agenticfetch`, `subagents`, `wakectl` have no active target declarations and are dissolved. `callbeat` has 2 target files and remains in the SCC. Do not use historical original-12 lists/counts as current limits.

### Concrete order after operator disposition

Do not claim a topological order for the residual SCC. The following is a **dependency-preserving work sequence**, not a DAG proof. Retarget overlapping coordinator/sessionAgent work serially in the existing listed order; gate every step on its unresolved edge choices and the per-edge ledger.

1. EX-0A → EX-0B → EX-0C → EX-0D → EX-0E → EX-0F → EX-0G → EX-0H → EX-0K. Existing receiver inventory oracle: EX-0A 16 `a` rows (`wake_scheduler.go`, `session_wake_dispatch.go`, `coordinator_wake.go`, `coordinator_wake_cli.go`, `coordinator_wake_tools.go`); EX-0B 12 (`coordinator_providers.go`, `coordinator_providers_auth.go`, `coordinator_providers_network.go`); EX-0C 0 (no receiver `a` rows; exact source selection `coordinator_reaction_chain.go`, `coordinator_reaction_source.go`, `coordinator_drain_policy.go`); EX-0D 0 (no receiver `a` rows; exact source selection `coordinator_subagents.go`, `coordinator_subagent_drivers.go`, `coordinator_background.go`, `coordinator_bgshell_cap.go`, `bgshell_claim.go`); EX-0E 6 (`agent_prompt.go`); EX-0F 14 (`supervision.go`); EX-0G 9 (`agent_ownership.go`); EX-0H 13 (`agent_compaction.go`, `agent_compaction_silent.go`, `agent_turn_helpers.go`); EX-0K 0 receiver `a` rows. EX-0K source selection: `cli_active_work.go` selected coordinator methods; `queued_call_scope.go` only the retained-scope declaration/methods recorded in execution manifest; `usage_record.go` only the `sessionAgent.recordMessageUsage` fragment; `peak_hours_request_gate.go` only `sessionAgent` receiver fragment and its receiver-free request-gate declarations as assigned in execution manifest; `oauth_token_source.go` only the EX-0K receiver methods in execution manifest; `turn_arbiter.go` selected receiver method; `coordinator_responses_ws.go` selected coordinator receiver fragment; batch source files only the explicitly overridden symbols in execution manifest; finalization source files only the six EX-0K receiver entries recorded in execution/receiver manifest. For every retarget, keep bodies/order fixed, thin coordinator delegate, check exact receiver/token diff, and reconcile declaration keys. EX-0C/D/K do not gain invented receiver rows.
2. EX-0L (source selectors: `batch_invocation.go` declarations `BatchInvocation`, its fields and `Validate` [with `roleConfig` specifically excluded into execution-side options]; `batch_exec.go` payload/validation declarations; `batch_cli.go`/`batch_external_spec.go` transport declaration selectors; `batch_install_ingress.go` `BatchWorkIngress`/`BatchLeafRegistry` declarations; `batch_root_lease.go` `BatchRootLeaseStore` contract declaration only; `batch_runtime.go` delivery signature declarations). Candidate contract methods are the exact lease methods in the Five decisions section and `DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse,error)`. EX-0M (source selectors: `batch_install_adapters.go` declarations `BatchInstall`/`BatchInstallDeps`/constructors and their receiver methods, `batch_install_ingress.go` `NewBatchWorkIngress*` and coordinator-binding declarations, `agent_tool.go` declarations `coordinator.agentTool` and template/tool selectors; target `batchassemble` owns construction and `coord/tool_glue.go` keeps `coordinator.agentTool`). EX-0N (source selectors: `agentic_fetch_policy.go` private wrapper declaration and methods `privateFetchIngressTool.*`; `policy_leaf.go` only `roleContractsFromTool` concrete cases 188–195; leave `policyLeafExecution` cases 212–245 intact). EX-0P (source selectors: receiver-free `provider_limit.go` quota/reset declarations; `coordinator_retry_classify.go` `isQuotaLimit`; `coordinator_providers_auth.go` `checkPeakHours` plus its `PeakHoursError`/sentinel/method closure from `coordinator.go`; cache `observedCacheReadTokens` and `turnStream.logCacheStepUsage` declarations in `cache_diagnostics.go`). Use execution manifest source keys for declaration-level selectors; whole mixed source files are search scope only. No new receiver rows are assumed. `coordinator_reaction_chain.go`, `coordinator_reaction_source.go`, `coordinator_drain_policy.go`; EX-0D is `coordinator_subagents.go`, `coordinator_subagent_drivers.go`, `coordinator_background.go`, `coordinator_bgshell_cap.go`, `bgshell_claim.go`; EX-0K comprises only the listed method/declaration fragments. These mixed files are search scope only and must be narrowed to the manifest's declaration keys before any move.
3. DM1 → DM2 → DM3 → DM4 → DM5a → DM5b → DM5c → DM6; DM-W after EX-0A/B/K and before DM7; DM7 only after DM1–DM6 and DM-W; DM8 after DM7 for importer qualification; DM9 follows DM8 but is out of this documentation-only scope. DM5a precedes DM5b, then DM5c; DM6 is only `finalize` + `delegation` (the old `subagents` leaf is empty/dissolved, no DM6 subagents lane). DM5b's old `batchrun` lane is dissolved; run only the DM5a allocation. DM-W's `wakectl` portion is dissolved into coord; run no empty-package lane. DM7 consolidates coord/agentrun only after their decisions/receiver work.
4. DM lanes with SCC-owner portions remain operator-gated; this does not block their explicitly identified acyclic leaf portions from starting now at stable package paths. Start the leaf selections listed above immediately. In combined lanes, hold SCC-owner declarations/methods until edge dispositions; no empty `subagents`, `batchrun`, `agenticfetch`, or `wakectl` lane is created.

### Residual SCC handoff: exhaustive edge cover, unresolved once

The edge-cover TSV covers **all 70** directed inter-owner arcs internal to it, not every simple cycle. Each root arc has a BFS return path; each path row carries its own `options_for_edge` for that exact arc/witness, so options are not copied from the root edge. Every row has an actual declaration/use witness. Choose dispositions for every root arc before proposing any cycle-removal claim. Options are concrete: replace the named body use with a narrow same-time operation owned by the declaration owner; split the exact consumer operation and keep coordinator/sessionAgent state with its current owner; or move only an exact portable immutable declaration not already in its lower contract. Never move coordinator/sessionAgent state lower, and moving an already-batchcontract declaration again is not an option.

| Reciprocal cycle (observed arc witnesses, declaration → use) | Concrete disposition options to choose; not yet implementation |
|---|---|
| `batchcontract ↔ batchinst` | `batchCodeNoLeaf` (`batch_exec.go:103 → :278`) / `BatchCLITask` (`batch_cli.go:19 → batch_external_spec.go:72`). Move only portable transport/validation (`BatchInvocation.Validate() error`, immutable payload with tags) to batchcontract; keep execution options separate; or replace concrete calls by the contract port. |
| `batchinst ↔ coord` | `coordinator` (`coordinator.go:104 → batch_install_ingress.go:181`) / `AsyncCompletion` (`batch_rehome_async_completion.go:11 → batch_glue.go:71`). Keep setters/coord state in coord; batchassemble injects a structural capability; alternatively move only a portable declaration, never coordinator state. |
| `agentrun ↔ coord` | `batchCutoverReactionStore` (`reaction_write_cutover.go:16 → agent_turn_failure.go:372`) / `ErrRunDefaultCap` (`timeout_stop.go:37 → :57`). Keep lifecycle state in its owner and use a cutover/reaction callback at the existing same-time point; otherwise move only the portable value or leave a thin coordinator delegate. |
| `agentrun ↔ mailbox` | `activeCallToken` (`mailbox_generation.go:14 → agent_control.go:98`) / `sessionAgent.mailboxes` (`agent.go:442 → agent_ownership.go:28`). Move ownership methods/state together, or replace direct access with a narrow mailbox operation preserving epoch/lock order; split queued-call scope if it remains a distinct lifecycle. |
| `agentrun ↔ turnstep` | `InTurnGuardStopTitle` (`turn_progress_guard.go:33 → agent_turn_step.go:657`) / `sessionAgent` (`agent.go:230 → agent_turn_ui_notify.go:19`). Move summarizeQueue with turnBudget and inject admission acquire/release; alternatively leave agentrun host operations and use same-time callbacks. |
| `batchflow ↔ batchinst` | `BatchHeldQuestionRoute` (`batch_held_question_route.go:30 → batch_await.go:94`) / `batchRootLeaseStore` (`batch_root_lease.go:21 → batch_install_adapters.go:33`). Keep lease map/state in batchflow, expose only a narrow contract in batchcontract, and wire in batchassemble; move the held-question portable declaration or inject its operation. |
| `batchassemble ↔ batchinst` | `BatchAgentDeps` (`batch_install_ingress.go:275 → batch_install_adapters.go:596`) / `BindCoordinatorBatchControls` (`batch_install_ingress.go:159 → :156`). Keep install implementation below composition; move constructors/factories to batchassemble and inject structural dependencies rather than introducing a reverse package call. |
| `batchassemble ↔ coord` | `BindCoordinatorBatchRuntimeBindings` (`batch_glue.go:84 → batch_install_ingress.go:160`) / `BatchInstall` (`batch_install_adapters.go:668 → finalization_wiring.go:23`). Keep `coordinator.agentTool` on *coordinator in `coord/tool_glue.go`; move composition/factory to batchassemble with constructor injection, never a coordinator-state bundle. |
| `batchcontract ↔ batchflow` | `batchRootLeaseStore` (`batch_root_lease.go:21 → :40`) / `BatchRootLeaseStore` (`:40 → :45`). Replace the pointer alias with the candidate narrow structural contract listed below, inventorying the consumers first; state/map stay batchflow. |
| `batchcontract ↔ toolwrap` | `policyLeafRegistry` (`policy_leaf.go:29 → batch_exec.go:81`) / `BatchInvocation` (`batch_invocation.go:98 → batch_install_tools.go:88`). Keep mutable policyLeafRegistry state in toolwrap; specify only the batchcontract structural consumer operations found in the declaration/use inventory. Do not propose moving this already-lower transport type again. |
| `batchinst ↔ clibridge` | `translateCLIInput` (`cli_tool_bridge.go:105 → :93`) / `cliToolBridge` (`cli_tool_bridge.go:16 → :23`). Choose whether the bridge implementation belongs with batch CLI construction or clibridge; maintain one implementation owner and inject the adapter rather than reciprocal imports. |
| `coord ↔ modelkit` | `AgentToolName` (`agent_tool.go:35 → :57`) / anonymous cache-key `Generation` (`coordinator_model_cache.go:104 → :112`). Keep cfg/sessions/currentAgent/readyWg in coord; move cache key/LRU/private pair and cache state encapsulation to modelkit; retain coordinator assembly as thin glue and use cache operations below. |
| `coord ↔ toolwrap` | `agenticFetchPromptTmpl` (`agentic_fetch_tool.go:52 → :180`) / `batchCoordinatorBinding.pin` (`batch_install_ingress.go:139 → batch_install_tools.go:114`). Keep coordinator agent/tool assembly in coord; move only wrapper/role declarations to toolwrap or pass a typed policy/ingress capability from batchassemble. |

The edge ledger additionally provides three concrete options per directed arc, including non-reciprocal arcs and longer return paths (maximum BFS path length five). The table is representative reciprocal evidence; the TSV is the exhaustive finite scope. The SCC's 70 arc dispositions and the six synthetic self-edges are distinct: the latter are `agentrun` (18 witnesses/24 uses), `batchassemble` (5/5), `batchinst` (4/9), `mailbox` (4/5), `toolwrap` (13/22), and `turnstep` (8/10). Self-edges represent synthetic same-owner seam assumptions and are not inter-owner cycles; do not fold them into the 70 cycle arcs.

### Five decisions selected for execution — not implemented

1. **Policy leaf capability / EX-0N.** Candidate structural method: `PolicyLeafTool() fantasy.AgentTool`, implemented by the private `privateFetchIngressTool` wrapper (`agentic_fetch_policy.go:113–134`) as `return wrapped.AgentTool`. Consume it in `toolwrap` only from the concrete-type switch in `roleContractsFromTool` (`policy_leaf.go:164–201`, cases 188–195). This replaces that introspection only. It does **not** replace `policyLeafExecution`'s recognized-wrapper copying (`policy_leaf.go:212–245`), does not unwrap/copy policy execution, and is not an implemented source port. Keep the private-fetch wrapper's current wrapping owner.
2. **Model cache / EX-0B.** Candidate modelkit-owned operations: `Lookup(key) (cachedModelPair, bool, epoch)`, `StoreIfEpoch(key, pair, epoch)`, `Clear()`, and a compatibility/test `Len() int` operation only where needed. Exact state anchors: `modelPairCacheKey` and bounded LRU in `coordinator_model_cache.go:11–96`; legacy JSON key/nil/test seams at `:99–163`; coordinator epoch/mutex `coordinator.go:290–291`; build outside cache lock and stale-epoch rejection at `:126–163` and `coordinator_models_build.go:263–270`. Keep `cfg`, sessions, currentAgent, readyWg, lifecycle/autonomy in coord. Preserve build-outside-lock, epoch/key/generation, stale-write rejection, `Len`, nil cache, and legacy key behavior. Struct-member ownership is not projected; encapsulate `modelCacheMu/modelCacheEpoch` with modelkit cache state.
3. **Admission/turnBudget / EX-0H.** Candidate operation: `TryAdmitRun() (done func(), ok bool)`; acquire under the existing admission lock and obtain the matching `runWg.Done` release from the same operation, immediately `defer done()` on success. `turnBudget` owns summarizeQueue/compaction-local state; agentrun retains runWg/admission and timeout configuration. Current order anchors: `agent_compaction.go:46–89` (admit then `defer a.runWg.Done`, then queue path) and `:121–135` (lazy stream timeout reads). Do not project a raw struct member as though it were a declaration move. Preserve original lazy timeout evaluation at each old read (candidate immutable per-operation options/provider evaluated there); no copied mutable timeout fields, no eager snapshot, no runWg export.
4. **Batch contract/composition / EX-0L/0M.** `batchcontract` owns portable transport, validation, and narrow structural capabilities only; `batchassemble.newBatchInstall` injects ingress/delivery/lease implementations. `BatchInvocation.roleConfig *config.Config` at `batch_invocation.go:135–136` is execution-side admission snapshot/options, not portable transport: carry the exact admitted snapshot, never fetch a fresh config. Before defining `BatchLeafRegistry`, inventory the exact consumers and signatures they need (`PinLeafRegistry`, `ResolveLeaf`, `Registry`, plus only the operations found by declaration/use census); expose a lower-owned structural interface containing only those required methods, never an alias/pointer-to-interface or alias of mutable `policyLeafRegistry`. Lease state/map (`batch_root_lease.go:21–30`) remains batchflow; candidate contract methods: `Acquire(jobbatch.ID,string,string,string) error`, `Adopt(jobbatch.ID,<-chan struct{},string,string,func(context.Context,jobbatch.ID) error) error`, `Release(jobbatch.ID,string,string)`, `Matches(jobbatch.ID,string,string) bool`, `OwnerForRoot(jobbatch.ID) string`, plus `DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse,error)`. Preserve watcher settlement ordering `release -> forget -> optional callback` at `batch_root_lease.go:98–112`; evaluate providers at their original call point. These are candidate method shapes, not compiled API.
5. **Watchdog wake / EX-0F.** Candidate callback: `func(context.Context, string, bool) error`, injected for watchdog's wake operation only; it does not authorize a broad coordinator/sessionAgent/supervisionRegistry port. Anchor: `supervision.go:550` wake must remain before `:566` `releaseChildDriverIfScopeClosed`. Keep supervision/driver state with its current owner; retain the call ordering exactly. Existing `port_owner` coverage is limited to this wake use and does not prove other field/body dependencies away.

All five decisions are design only. No source implementation or compiler proof is claimed. Apply rules 18–19: tests requiring private names stay with their package, shared fixtures are byte-identical `_test.go` copies, exports require actual compiler-proven external call sites, mutable globals remain private, and behavior-preserving same-time ports are preferred.

## Final verification evidence and status

The orchestrator independently built the graph tool and successfully ran the FINAL projection; this report does not rerun it. Observed receipt is `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/agent-last-pass.json`. Its final-pass override source is the last-pass block beginning at line 657 of `2026-11-16-sprint0-agent-amended-overrides.tsv`. The override file has **648 data rows** (not 649; the earlier worker count included the header); preserve its 5/6-column records and required empty trailing TSV cells. The independent census initially confirmed source declaration/receiver identities, 70 edge roots and 198 arc rows against the final receipt; subsequent reviewer corrections retain the same source identities/cycle paths and change per-arc option prose and proposed split targets only. Treat those proposed targets as unmeasured, not census-certified valid layouts.

Already completed by the orchestrator: graph tool `go test -c` on linux/darwin/windows with `GOOS=<os> CGO_ENABLED=0 bash D:/dev/go/crush/.rush/tmp/golock.sh go test -c -o NUL -gcflags=-e -ldflags="-s -w" .` — each prints `[no test files]`; `go build -o NUL` and `go vet .` each exit 0 with empty output; native graph-tool build exits 0; final graph exits 0 with 10 historical missing-file warnings. These are compile/build/vet/graph checks, **not repository tests**; no tests were executed. No Go source was touched, so no gofmt path is required for this documentation-only edit.

The required rule-19 selfcheck was run on the worktree and returned: `check_dir_entries rc=0`, `check_dir_files rc=0`, `check_file_size rc=0`, `check_file_size_900 rc=0`, `check_layers rc=0`, `check_pkg_names rc=0`; all six selftest checks rc=0 advisory; final **`selfcheck rc=0`**. `git diff --check` can report the required empty trailing TSV cells in the pre-existing task-owned overrides file; do not damage its fixed-width 5/6-column records to silence that diagnostic. Scope of this chunk is documentation only: no internal source, tool, overrides/layout TSV, tests, configs, CLAUDE.md, or commit changed by this report.

