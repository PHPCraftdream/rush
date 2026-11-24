# Coordinator port C1 / `internal/agent` physical split — scouting (2026-11-24)

## Status of this plan (read first)

Two review rounds ended without a clean pass, so this is a scouting baseline
with measured data, not an executable schedule. A later lane may rely on the
measurements and must redo the items below before any move.

- Usable as written: the root count (198 = 192 production files + 6 code
  directories), the file listing, the directory rule (only the immediate parent
  gains an element, so a new directory under the already counted `runtime` and
  `runtime/exec` costs the root nothing), the three file-size ratchet lines, the
  conditional arithmetic 198 - 19 = 179 and 179 - 10 = 169 (valid only if each
  group leaves the root as whole files with zero shims), and the located edges
  (`agent.go:232`, `agent_options.go:30`, `coordinator.go:115`, the four drain
  methods in `drain_component.go:189,203,357,371`).
- Wrong or unproven, to be redone by a discovery lane: (1) the "4 open, 10
  resolved" classification of the 14-member port inventory of
  `docs/plans/2026-11-22-dm0b-scc-cut.md`: members marked resolved are still
  reached through the concrete `*coordinator` receiver (`driverHints` at
  `agent_turn_step.go:704`, `finalization` at `finalization_wiring.go:306`,
  `buildModelsFromCfg` at `agent_codex_recovery.go:127,148`, the batch facts and
  background fields in `agent_own_work_batch.go:27-55`, and the registry through
  `a.coord.subAgentDrivers` in `finalization_driver_binding.go:27`), so lowering
  a type does not by itself remove the access path; the owners of the still open
  signatures (for example `batch_scope_recovery.go:100`) are not specified; (2)
  the lane order: the two DM7 moves are placed first, while their file sets
  depend on DM1-DM6 and DM-W (`docs/plans/2026-11-15-sprint0-split-agent.md`,
  rows around lines 311-320), so the 19 and 10 are historical package inputs,
  not established whole-file removal counts, and "192 moved gives root 6" is a
  target identity, not a route; (3) the receiver census (71 file rows in the
  coordinator table, not 63; `coordinator_iface_assert.go` holds only interface
  assertions) and the line budget (the 11,774-line coordinator total cannot be
  reproduced from the document; the earlier scout's 12,122 lines precede the
  estimated L5 reduction to about 8,000, they do not follow it); (4) the C3
  member was resolved in L6a (`0f615b5f5`), not in dm7d, and the three call-type
  aliases in `contract_bridge.go:26-30` already point to `agentcall`, so moving
  them is not remaining work.
- No compiler probe of a complete move was run for either destination.

## Measurement and scope

Rule copied from `.githooks/check_dir_entries.sh`: direct, tracked, non-generated production `.go` sources plus direct child code directories; exclude `_test.go` and any subtree segment `testdata`, `node_modules`, or `dist`. The root is **198**, not 199: 192 files + 6 code dirs. (A naive filesystem listing sees `templates/`, but it has no tracked Go source descendant and is therefore not a code directory.) The given plan's 198 is confirmed.

Root production file listing (alphabetical):

`agent.go`, `agent_cache_keepalive.go`, `agent_codex_recovery.go`, `agent_codex_recovery_cleanup.go`, `agent_compaction.go`, `agent_compaction_silent.go`, `agent_control.go`, `agent_drain.go`, `agent_options.go`, `agent_own_work_batch.go`, `agent_ownership.go`, `agent_prompt.go`, `agent_run.go`, `agent_timeouts.go`, `agent_title.go`, `agent_tool.go`, `agent_turn.go`, `agent_turn_checkpoint.go`, `agent_turn_failure.go`, `agent_turn_helpers.go`, `agent_turn_peakhours.go`, `agent_turn_step.go`, `agent_turn_stream.go`, `agent_turn_title.go`, `agent_usage.go`, `agentic_fetch_policy.go`, `agentic_fetch_tool.go`, `arbiter_component.go`, `arbiter_state_component.go`, `await_tasks_era.go`, `background_component.go`, `batch_admission.go`, `batch_allowed_controls.go`, `batch_cancel.go`, `batch_cli.go`, `batch_cli_runtime.go`, `batch_credentials_escrow.go`, `batch_credentials_marker.go`, `batch_cutover_driver.go`, `batch_cutover_facts.go`, `batch_cutover_facts_node.go`, `batch_cutover_rerun.go`, `batch_delivery.go`, `batch_delivery_doc.go`, `batch_depth_payload.go`, `batch_exec.go`, `batch_exec_agent.go`, `batch_exec_agent_doc.go`, `batch_exec_argv.go`, `batch_exec_generic.go`, `batch_exec_shell.go`, `batch_external_spec.go`, `batch_glue.go`, `batch_held_question_route.go`, `batch_ingress.go`, `batch_install_adapters.go`, `batch_install_agent_driver.go`, `batch_install_bridge.go`, `batch_install_ingress.go`, `batch_install_policy.go`, `batch_install_question.go`, `batch_install_tools.go`, `batch_invocation.go`, `batch_model_control.go`, `batch_model_tools.go`, `batch_profile.go`, `batch_question_rebuild.go`, `batch_question_route.go`, `batch_rehome_async_completion.go`, `batch_rehome_coord_runtime.go`, `batch_rehome_runtime.go`, `batch_role_admission.go`, `batch_root_lease.go`, `batch_runtime.go`, `batch_scope.go`, `batch_scope_recovery.go`, `batch_terminal_output.go`, `batch_worker_hints.go`, `cache_diagnostics.go`, `cache_profile.go`, `cli_active_work.go`, `cli_tool_bridge.go`, `contract_bridge.go`, `coordinator.go`, `coordinator_agent_control.go`, `coordinator_agent_inject_batch.go`, `coordinator_attempt_evidence.go`, `coordinator_await_tasks.go`, `coordinator_background.go`, `coordinator_batch_stop.go`, `coordinator_call_result.go`, `coordinator_drain_policy.go`, `coordinator_external_interrupt.go`, `coordinator_iface_assert.go`, `coordinator_interrupt.go`, `coordinator_model_cache.go`, `coordinator_models.go`, `coordinator_models_build.go`, `coordinator_models_call_tools.go`, `coordinator_models_custom_role.go`, `coordinator_models_role.go`, `coordinator_providers.go`, `coordinator_providers_auth.go`, `coordinator_rate_limit.go`, `coordinator_reaction_chain.go`, `coordinator_reaction_source.go`, `coordinator_recheck.go`, `coordinator_rerun.go`, `coordinator_responses_ws.go`, `coordinator_retry_classify.go`, `coordinator_retry_testsupport.go`, `coordinator_run.go`, `coordinator_run_await.go`, `coordinator_run_effort.go`, `coordinator_run_queue_call.go`, `coordinator_session_prompt.go`, `coordinator_session_state_sweep.go`, `coordinator_subagents.go`, `coordinator_summarize.go`, `coordinator_summarize_role.go`, `coordinator_tool_assembly.go`, `coordinator_tools.go`, `coordinator_tools_reviewer.go`, `coordinator_turn_hold.go`, `coordinator_wake.go`, `coordinator_wake_cli.go`, `coordinator_wake_tools.go`, `coordinator_work_scope.go`, `credentials.go`, `custom_role_runtime.go`, `default_job_timeout.go`, `delegation_depth.go`, `delegation_question.go`, `delegation_role.go`, `delegation_role_calls.go`, `delegation_role_models.go`, `delegation_role_state.go`, `disk_provider_binding.go`, `disk_provider_call.go`, `disk_provider_capability.go`, `disk_provider_continuation.go`, `disk_provider_durable.go`, `disk_provider_intent.go`, `drain_attempt.go`, `drain_call_component.go`, `drain_component.go`, `drain_policy_component.go`, `finalization_bind.go`, `finalization_call_context.go`, `finalization_driver_binding.go`, `finalization_gate.go`, `finalization_holds_assembly.go`, `finalization_holds_providers.go`, `finalization_hooks.go`, `finalization_queue_call.go`, `finalization_queue_reservation.go`, `finalization_recompose.go`, `finalization_root_bind.go`, `finalization_wait.go`, `finalization_wiring.go`, `inline_window.go`, `loop_detection.go`, `mailbox.go`, `mailbox_generation.go`, `mailbox_inject.go`, `mailbox_interrupt.go`, `mailbox_ownership.go`, `mailbox_queue.go`, `mcp_output_access.go`, `models_component.go`, `oauth_token_source.go`, `peak_hours_request_gate.go`, `prompts.go`, `queued_call_scope.go`, `reaction_component.go`, `role_policy.go`, `root_role_contract.go`, `session_wake_dispatch.go`, `stall_component.go`, `step_progress.go`, `stream_watchdog.go`, `subagent_drivers_component.go`, `subagents_component.go`, `supervision.go`, `supervision_testsupport.go`, `tool_suggest.go`, `tools_component.go`, `toolwrap_bridge.go`, `turn_progress_guard.go`, `usage_glue.go`, `usage_record.go`, `wake_component.go`

Root code directories: `agentresult/`, `prompt/`, `provider/`, `runtime/`, `safety/`, `tools/` (6); total 192 + 6 = **198**.

`runtime/` direct production files: **0**; direct code directories: `batch/`, `contract/`, `exec/`, `modelkit/`, `policy/`, `wakectl/` (6); total **6**.

`runtime/coord/`: **does not exist** today; current measured count 0. `runtime/exec/agentrun/`: **does not exist** today; current measured count 0. Both destinations sit below the already-counted `runtime/` directory: creating `runtime/coord/` raises `runtime/` 6→7 (exactly at its own limit) and costs the root nothing; creating `runtime/exec/agentrun/` raises `runtime/` (unchanged, still 6 direct children) and `runtime/exec/` 6→7, also costing the root nothing. `runtime/exec/` today has 0 direct production `.go` files and exactly 6 direct code directories (`drainctl/`, `mailbox/`, `responsesws/`, `sessionprompt/`, `turnstep/`, `watchdog/`) → measured count **6**, under the limit of 7. Creating `runtime/exec/agentrun/` makes it 7, exactly at the limit.

Line-count check: `coordinator_run.go` 877; `coordinator_interrupt.go` 958; `agent.go` 558. The prior plan text's DM7 line figures (998, 966) are stale relative to this worktree.

## Candidate clusters and boundary evidence

These are the two C1 DM7 clusters named in `docs/plans/2026-11-15-sprint0-split-agent.md` §2.2/§2.3 and §5 DM7. The historical receiver counts are not a live inventory: current grep finds 63 coordinator-bearing files and 25 sessionAgent-bearing files (many split by other lanes). Destination output counts remain unproven until declaration-level moves are measured.

### Live coordinator receiver inventory (candidate superset)

Line counts are today's production lines before DM-W/DM1-DM6 strip declaration ranges; these sums are not post-lane forecasts. A split row must be handled at declaration level.

| file | coordinator method lines / disposition |
|---|---|
| `agent_drain.go` | 38, 231 — split; agentrun methods coexist |
| `agentic_fetch_tool.go` | 23 — DM4 split |
| `agent_tool.go` | 37 — DM-W split |
| `arbiter_component.go` | 105 — DM2 split |
| `await_tasks_era.go` | 8 — coord; arbiter methods DM2 |
| `batch_cutover_driver.go` | 15 — batchflow/coord split |
| `batch_cutover_facts.go` | 149 — batchflow/coord split |
| `batch_cutover_rerun.go` | 56 — batchflow/coord split |
| `batch_held_question_route.go` | 37 — batchflow/coord split |
| `batch_question_rebuild.go` | 28 — batch/coord split |
| `batch_rehome_coord_runtime.go` | 72 — split |
| `batch_rehome_runtime.go` | 91 — split |
| `batch_scope_recovery.go` | 100 — split; helper remains lower batch |
| `cli_active_work.go` | 115, 289, 298 — DM3 clibridge and coord split |
| `coordinator_agent_control.go` | coordinator methods — coord/component split |
| `coordinator_agent_inject_batch.go` | coordinator methods — coord |
| `coordinator_attempt_evidence.go` | coordinator methods — coord |
| `coordinator_await_tasks.go` | coordinator methods — coord |
| `coordinator_background.go` | coordinator methods — coord/component split |
| `coordinator_batch_stop.go` | coordinator methods — coord |
| `coordinator_call_result.go` | coordinator methods — coord |
| `coordinator_drain_policy.go` | 10 — coord/drainctl split |
| `coordinator_external_interrupt.go` | coordinator methods — coord |
| `coordinator.go` | type at 69; split field/method declarations, type to coord |
| `coordinator_iface_assert.go` | coordinator methods — coord |
| `coordinator_interrupt.go` | coordinator methods — coord (958 lines today) |
| `coordinator_model_cache.go` | coordinator methods — DM-W modelkit |
| `coordinator_models_build.go` | 33 — DM-W modelkit |
| `coordinator_models_call_tools.go` | coordinator methods — DM-W modelkit |
| `coordinator_models_custom_role.go` | 9 (`finishCustomRoleResolveSnapshot`) — DM-W modelkit |
| `coordinator_models.go` | coordinator methods — DM-W modelkit |
| `coordinator_models_role.go` | coordinator methods — DM-W modelkit |
| `coordinator_providers_auth.go` | coordinator methods — DM-W modelkit |
| `coordinator_providers.go` | coordinator methods — DM-W modelkit |
| `coordinator_rate_limit.go` | coordinator methods — DM-W modelkit |
| `coordinator_reaction_chain.go` | coordinator methods — DM2 drainctl |
| `coordinator_reaction_source.go` | coordinator methods — DM2/agentcall split |
| `coordinator_recheck.go` | coordinator methods — coord |
| `coordinator_responses_ws.go` | coordinator methods — DM1 split |
| `coordinator_retry_classify.go` | coordinator methods — DM-W modelkit |
| `coordinator_run_await.go` | coordinator methods — coord |
| `coordinator_run.go` | coordinator methods — coord (877 lines today) |
| `coordinator_run_queue_call.go` | coordinator methods — coord |
| `coordinator_session_prompt.go` | coordinator methods — DM1 split |
| `coordinator_session_state_sweep.go` | coordinator methods — DM1 split |
| `coordinator_subagents.go` | coordinator methods — DM6 subagents split |
| `coordinator_summarize.go` | coordinator methods — DM-W modelkit split |
| `coordinator_tool_assembly.go` | coordinator methods — DM-W modelkit |
| `coordinator_tools.go` | coordinator methods — DM-W modelkit |
| `coordinator_turn_hold.go` | coordinator methods — DM2 turnstep |
| `coordinator_wake_cli.go` | coordinator methods — DM-W wakectl |
| `coordinator_wake.go` | coordinator methods — DM-W wakectl |
| `coordinator_wake_tools.go` | coordinator methods — DM-W wakectl |
| `coordinator_work_scope.go` | coordinator methods — coord |
| `credentials.go` | coordinator methods; contract types and agentrun methods split |
| `default_job_timeout.go` | coordinator methods — DM6 subagents |
| `delegation_question.go` | coordinator methods — DM6 delegation |
| `delegation_role_calls.go` | coordinator methods — DM6 delegation |
| `delegation_role_models.go` | coordinator methods — DM6 delegation |
| `disk_provider_continuation.go` | coordinator methods — DM1/DM5 split |
| `drain_attempt.go` | coordinator methods; DrainAttempt already moved |
| `finalization_queue_call.go` | coordinator methods — DM6 finalize |
| `finalization_queue_reservation.go` | coordinator methods — DM6 finalize |
| `finalization_wiring.go` | coordinator and agent methods; split |
| `inline_window.go` | coordinator methods — DM5 batchinst split |
| `mcp_output_access.go` | coordinator methods — DM-W modelkit |
| `oauth_token_source.go` | coordinator methods — DM3 provauth split |
| `peak_hours_request_gate.go` | coordinator methods — DM3 split |
| `session_wake_dispatch.go` | coordinator methods — DM-W wakectl |
| `supervision.go` | coordinator methods — DM1 watchdog split |
| `wake_component.go` | coordinator adapter methods — DM-W wakectl |

The 63-file grep result is a candidate superset, not 63 files all moving to coord. Exact post-lane receiver files/count: **UNPROVEN: needs a discovery lane** — open question: after DM-W and DM1-DM6 move declarations, which files still contain coord-owned declarations and what is the exact consolidated output count?

### Live sessionAgent receiver inventory

The live grep finds 25 files; the 7,942-line sum is today's production count before other lanes remove declarations. These are candidate source files, not a final 25-file package move.

| file | live evidence / split note | lines |
|---|---|---:|
| `agent_cache_keepalive.go` | methods; DM3 callbeat split | 613 |
| `agent_codex_recovery.go` | methods; provauth split | 265 |
| `agent_codex_recovery_cleanup.go` | methods; provauth split | 69 |
| `agent_compaction.go` | methods; turnstep/provauth split | 296 |
| `agent_compaction_silent.go` | methods; provauth split | 510 |
| `agent_control.go` | methods; ShuttingDown at 155 | 325 |
| `agent.go` | sessionAgent type at 222; fields/declarations split | 558 |
| `agent_ownership.go` | methods; ownership component split | 855 |
| `agent_own_work_batch.go` | method at 23; batch split | 59 |
| `agent_prompt.go` | methods; sessionprompt split | 112 |
| `agent_run.go` | methods | 919 |
| `agent_timeouts.go` | methods; watchdog split | 136 |
| `agent_title.go` | method at 207; turnstep split | 461 |
| `agent_turn.go` | method at 58 | 812 |
| `agent_turn_helpers.go` | methods; turnstep/drain split | 185 |
| `agent_usage.go` | methods; usage split | 109 |
| `drain_call_component.go` | methods; drainctl split | 208 |
| `drain_component.go` | newDrainAttempt:189, closeDrainAttempt:203, noteRefusal:357, afterTurn:371; split | 442 |
| `finalization_call_context.go` | methods; finalize split | 119 |
| `finalization_driver_binding.go` | method; finalize/toolwrap split | 30 |
| `finalization_root_bind.go` | methods; finalize split | 116 |
| `finalization_wiring.go` | sessionAgent method at 302; coord methods coexist | 322 |
| `queued_call_scope.go` | method at 101; mailbox/agentrun split | 156 |
| `stall_component.go` | method; drainctl split | 34 |
| `tool_suggest.go` | method; toolwrap split | 99 |
| `usage_record.go` | method at 48; usage split | 132 |

The four drain-attempt sessionAgent methods are in `drain_component.go`, not `drain_attempt.go`. Exact post-lane agentrun source/output count: **UNPROVEN: needs a discovery lane** — open question: after DM-W and DM1-DM6 extraction, which sessionAgent methods remain and how many consolidated files do they require?

### A. `runtime/coord` — 19 planned production files + glue; target final 7 files

Planned files: `coordinator.go`, `coordinator_run.go`, `coordinator_run_await.go`, `coordinator_run_effort.go`, `coordinator_run_queue_call.go`, `coordinator_recheck.go`, `coordinator_iface_assert.go`, `coordinator_interrupt.go`, `coordinator_external_interrupt.go`, `coordinator_rerun.go`, `coordinator_agent_control.go`, `coordinator_await_tasks.go`, `coordinator_attempt_evidence.go`, `coordinator_call_result.go`, `coordinator_work_scope.go`, `coordinator_batch_stop.go`, `coordinator_agent_inject_batch.go`, `credentials.go` (split), `prompts.go`; plus coord-owned glue receivers assembled from the remaining clusters. DM7 consolidates 19 + glue to 7.

Root-owned receiver: `type coordinator` is defined in `internal/agent/coordinator.go:69`. `coordinator_run.go:59` declares `(*coordinator).runInternal`; `coordinator_run.go:685,745,751,796` contain `RunWithOverrides`, `ReserveExclusive`, `ReleaseExclusive`, `RunWithReservedOwnership`. The run and interrupt clusters use the receiver's unexported state/method surface throughout; moving functions alone cannot access fields such as coordinator state. `coordinator_interrupt.go:46,133,197,239,314,597,636,725,881,917,929,939` are examples of methods requiring the receiver. `coordinator_interrupt.go:359` declares a local `SessionAgentCall`; `:467` calls `ToSessionAgentCallData`. The call type is defined in `internal/agent/agent.go:528` and conversion in `internal/agent/call_data_conversion.go` (the latter has already moved to `runtime/contract/agentcall/call_data_conversion.go` in this worktree). `coordinator_run.go:267,284` constructs `SessionAgentCall` and sets `CallOptions`; `:285` calls the modelkit role-contract helper. The options/call boundary types are now in agentcall; this observed current location means this is not a remaining root-owned type dependency. `coordinator.go:69` proves receiver ownership; compiler diagnostics for a full move have **not** been captured.

Smallest legal direction: move `coordinator` with **all** its methods as one legal receiver type; it cannot be divided between packages. To extract subordinate components, methods must be retargeted to component receiver in the source package first, then component package owns its component type; the coordinator imports that lower component and supplies a port implemented by coordinator. No `coord` function may accept a `*coordinator` owned by a higher package: after move, `coord` owns the concrete type; lower packages accept contracts/interfaces and `coord` imports them. Root's `SessionAgentCall`, `ModelOverride`, etc. must move to the lower `runtime/contract/agentcall` package before boundary signatures use them (as the prior plan prescribes), not be aliased across the boundary. Exact complete-move viability and import graph not proved by a compiler probe: **UNPROVEN: needs a discovery lane** — open question: can every coordinator field access/call in these 19 files be kept inside the single `coord` package after the other DM clusters move, with all remaining cross-cluster receiver work represented by lower-owned ports and no cycle?

### B. `exec/agentrun` — 10 planned production files + glue; target final 7 files

Planned files: `agent.go`, `agent_run.go`, `agent_control.go`, `agent_options.go`, `agent_timeouts.go`, `agent_turn.go`, `agent_turn_step.go`, `agent_turn_stream.go`, `agent_turn_failure.go`, `loop_detection.go`; plus moved receiver methods from `drain_attempt.go`, `credentials.go`, `queued_call_scope.go`, `usage_record.go`, `peak_hours_request_gate.go`, `turn_arbiter.go`. Prior plan says 10 + glue consolidate to 7.

Root-owned receiver: `type sessionAgent` is in `internal/agent/agent.go:222`. `agent_run.go:32,263,364,398,436,904` are sessionAgent methods; `agent_turn.go:58` is the turn-cycle method. The methods rely on private `sessionAgent` fields and methods (e.g. `agent.go:455-478`, plus model/tools/prompt accessors at `:532-556`); current call boundary types already reside in agentcall (`agent.go:528` in the historic root layout; now `runtime/contract/agentcall/session_call.go` and `call_options.go`). `agent_turn.go:58` uses `SessionAgentCall`; `agent_turn.go:68` passes `call.CallOptions` to drainctl; `agent_turn.go:84-85` reads role contract and sets context. Those are downward package dependencies if agentrun imports agentcall and drainctl, but private sessionAgent state is not accessible outside its owner. Complete-move compiler diagnostics not captured.

Smallest legal direction: `sessionAgent` must move with **all** its methods, or each extracted method must first become a function/component method that does not split methods from the type. Go forbids declaring methods on a type defined in another package, including an alias. `agentrun` owns `sessionAgent` after transfer; its dependencies on call-boundary data are downward to `runtime/contract/agentcall`, never root-to-agentrun imports. A forwarding method or type alias alone leaves the private-field/type dependency intact. **UNPROVEN: needs a discovery lane** — open question: can all moved sessionAgent methods and their remaining dependencies be made package-local or lower-interface-based, retaining private-state access without creating agentrun→coord or agentcall→agentrun cycles?

### Compiler proof status

No probe edits/builds were made. Therefore there is no real compiler error text to quote; do not treat the prior design's statements as build proof. A valid discovery lane must physically move the *complete* candidate cluster and methods, run exactly one destination-package build per command using the lock wrapper (e.g. `capm 2g go build ./internal/agent/runtime/coord; echo exit=$?`), capture the unedited compiler output verbatim, and immediately revert probe files. Never use a root-package build as a substitute, and never combine package paths in one build command. Any route above marked UNPROVEN remains so until that is done.

## Cross-cluster constraints and smallest removal

* The written inventory in dm0b-scc-cut §3.1 is reconciled against current source here. `sessionAgent.coord` is `*coordinator` (`agent.go:232`; struct starts :222), and `SessionAgentOptions.Coord` is `*coordinator` (`agent_options.go:30`). Live usage examples: `agent_turn_step.go:448,472,493` uses `ts.a.coord.arb`, :704 uses `driverHints`; `agent_turn.go:58` is runTurn, :186 uses `a.coord.cfg`, :205 calls `recoverBatchScopeDeadCandidates(preambleCtx,a.coord,...)`, :226 calls `coordinatorBatchCutoverFacts(a.coord)`; `drain_component.go:195,424` touches coordinator-owned driverHints/arbiterVerdict.
* Already merged call contract types are not prerequisites: root `contract_bridge.go:26,28,30` aliases `SessionAgentCall`, `ModelOverride`, `CallOptions` to agentcall; definitions are `runtime/contract/agentcall/session_call.go:25`, `call_options.go:41`, `coordinator_contract.go:20`. `CallFromActive` moved to `agentcall/call_from_active.go:4`; `DrainAttempt` to `runtime/exec/drainctl/drain_attempt_record.go:10`. Open question is only whether root aliases must disappear for DM7/DM8.
* Boundary placement still supports dm0b §3.1: port belongs to lower `runtime/contract/agentcall`, since agentrun implements nothing; the coordinator implements. Direction is `agentrun → agentcall` and `coord → agentcall`, never `agentcall → agentrun/coord`. The abstract example is `sessionAgent.coord agentcall.CoordinatorPort`, implemented in `runtime/coord`, stored/used in agentrun. Component lower contracts likewise must be owned below their implementations. The current `*coordinator` concrete field violates that direction until converted.
* C3 is resolved: `ShuttingDown()` exists on agentcall.SessionAgent (`runtime/contract/agentcall/session_call.go:349`) and `sessionAgent` implements it (`agent_control.go:155`).

### C1 inventory reconciliation (14 names plus C3)

Status labels indicate whether current concrete dependency remains; resolved here means a named prerequisite was implemented by a merged pass, not that C1 is complete.

| name | live evidence | status |
|---|---|---|
| cfg | `agent_turn.go:186`; `agent_codex_recovery.go:83,98`; `agent.go:232` concrete coord | OPEN: field/accessor edge |
| arb | `agent_turn_step.go:448,472,493` | OPEN: field/accessor edge |
| currentAgent | coordinator field and consumers in live root; receiver scan | RESOLVED by L6a/L6b component ports; recheck post DM lanes |
| subAgentDrivers | `coordinator.go:115`; toolwrap registry | RESOLVED by dm7e registry→toolwrap |
| finalization | coordinator field/remaining references; finalization wiring | RESOLVED by L6a/L6b component extraction |
| background | `agent_own_work_batch.go:55`; coordinator background component | RESOLVED by L6a/L6b component extraction |
| driverHints | `agent_turn_step.go:704`; coordinator/drain references | RESOLVED by L6a/L6b port extraction; verify final agentrun consumer |
| buildModelsFromCfg | live declaration `coordinator_models_build.go:33` | RESOLVED by L6a/L6b modelkit extraction (site moved) |
| batchActiveAfterQuestionPause | `cli_active_work.go:115` | RESOLVED by L6a/L6b (site moved to coordinator cli-active component) |
| buildCredentialModel | `credentials.go:387`; agent recovery call | OPEN: coordinator call crosses boundary |
| drainCredentialBoundary | `coordinator_agent_control.go:245`; agent recovery call at `agent_codex_recovery.go:133` | OPEN: coordinator call crosses boundary |
| batchCutoverFacts | `coordinatorBatchCutoverFacts` at `batch_cutover_facts.go:135`; agent turn :226 | RESOLVED by 27a080063 L6b batch facts (function renamed/moved; agent caller site remains) |
| recoverDeadScopeCandidates | `recoverBatchScopeDeadCandidates` at `batch_scope_recovery.go:100`; agent_turn.go:205 | RESOLVED by L6a/L6b batch scope recovery move; caller still sends coord |
| batchFactsInstalled | `cli_active_work.go:289`; agent_turn_step.go:340 | RESOLVED by L6a/L6b |
| C3 ShuttingDown | contract `session_call.go:349`; implementation `agent_control.go:155` | RESOLVED by dm7d leaf aliases / SessionAgent contract change |

Summary: among 14, OPEN 4 (cfg, arb, buildCredentialModel, drainCredentialBoundary); RESOLVED 10. C3 is RESOLVED separately. The OPEN interface port must be agentcall-owned and implemented by coord, consumed by agentrun; no prerequisite work repeats merged leaf API moves.

## Corrected root-entry arithmetic and lane sequence

The guard counts each immediate parent independently. `internal/agent` starts at 192 production files + 6 child code directories = 198. The destinations are below existing `runtime/`: creating `runtime/coord/` changes `runtime` 6→7; it costs the root **zero**. `runtime/exec/` currently has 6 child dirs; creating `agentrun/` changes it 6→7 and also costs the root zero. No root forwarders are planned.

| stage | root files leaving | root shims | destination parent | net root delta | root count |
|---|---:|---|---|---:|---:|
| start | — | — | — | — | 198 |
| C1a coord | −19 planned, historical count only; live receiver source set is the 63-file candidate superset above and exact post-lane file count is UNPROVEN | +0 (no forwarders planned) | runtime 6→7, at its limit; root unaffected | −19 | 179 |
| C1b agentrun | −10 planned, historical count only; live receiver source set 25 and exact post-lane output count is UNPROVEN | +0 (no forwarders planned) | runtime/exec 6→7, at its limit; root unaffected | −10 | 169 |
| DM1: mailbox/watchdog/sessionprompt/responsesws | **UNPROVEN: needs a discovery lane** — exact live root removals require declaration-level partition; 15-plan package-source counts are 10+5+5+2, not root-origin counts | +0 planned | exec destination dirs | **UNPROVEN** | **UNPROVEN** |
| DM2: drainctl/turnstep | **UNPROVEN: needs a discovery lane** — package-source counts 17+15; root removals not established | +0 planned | exec subdirs | **UNPROVEN** | **UNPROVEN** |
| DM3: usage/promptcache/callbeat/provauth/clibridge | **UNPROVEN: needs a discovery lane** — package-source counts 8+3+3+7+2; root removals not established | +0 planned | policy subdirs | **UNPROVEN** | **UNPROVEN** |
| DM4: toolwrap/agenticfetch | **UNPROVEN: needs a discovery lane** — package-source counts 8+2; root removals not established | +0 planned | policy subdirs | **UNPROVEN** | **UNPROVEN** |
| DM5a: batchinst | **UNPROVEN: needs a discovery lane** — 30 plan-source files; root-origin removals not established | +0 planned | batch/batchinst | **UNPROVEN** | **UNPROVEN** |
| DM5b: batchrun | **UNPROVEN: needs a discovery lane** — 18 plan-source files; root-origin removals not established | +0 planned | batch/batchrun | **UNPROVEN** | **UNPROVEN** |
| DM5c: batchflow | **UNPROVEN: needs a discovery lane** — 20 plan-source files; root-origin removals not established | +0 planned | batch/batchflow | **UNPROVEN** | **UNPROVEN** |
| DM6: finalize/delegation/subagents | **UNPROVEN: needs a discovery lane** — 21+7+6 plan-source files; root-origin removals not established | +0 planned | batch subdirs | **UNPROVEN** | **UNPROVEN** |
| DM-W: modelkit/wakectl | **UNPROVEN: needs a discovery lane** — 22+5 plan-source files; root-origin removals not established | +0 planned | runtime subdirs | **UNPROVEN** | **UNPROVEN** |
| DM8/DM9 | imports/config only; zero root source removals | +0 | — | 0 | unchanged |

**Remaining families from live root listing and 15-plan §2.3 map:** contract_bridge/agentcall remnants; wakectl; modelkit; exec/mailbox+watchdog+sessionprompt+responsesws; drainctl+turnstep; policy usage+promptcache+callbeat+provauth+clibridge; batchinst; batchrun; batchflow; finalize; delegation; subagents; toolwrap+agenticfetch. These are package families, not a proven partition of current root files. For each family the live source count and root net change are **UNPROVEN: needs a discovery lane** — open question: which exact direct-root files and declaration ranges leave in each family after earlier lanes, and which direct root file (if any) remains as a shim? Until those counts exist, running totals after C1b cannot responsibly be filled in. Do not mistake the 15-plan input counts for current stage removals: their naive sum plus C1 stages exceeds 192 and yields negative counts. Required acceptance is a non-overlapping row-level map summing to 192. Its final check is fixed: 192 moved → 0 root production files + 6 code dirs = 6, under 7. A shim adds exactly +1 each and invalidates that check unless separately accounted. Destinations under runtime alter only their immediate parent: `runtime` 6→7 for coord, `runtime/exec` 6→7 for agentrun; root unaffected.

## Line budgets

The seven-file budget is 7×900=6,300 lines. Today the coordinator receiver-bearing source files total 11,774 lines (63 files); sessionAgent receiver-bearing files total 7,942 lines (25 files). These totals include code that other lanes move before DM7 and are NOT output-size forecasts. The dm0b-scc-cut §7 Q1 figures are explicitly AFTER-EX/L5: coord 63 files/12,122 lines and agentrun 30/9,948 lines. Current candidates differ (coord 63/11,774; agentrun 25/7,942), so the 15-plan claim that DM7 can blindly concatenate to seven files cannot be accepted from these counts.

| cluster | current candidate total | 7×900 | verdict |
|---|---:|---:|---|
| coord candidate | 11,774 | 6,300 | exceeds by 5,474 lines; at least 7 files cannot fit; need declaration removal/splits or a relaxed line/file rule. Result after DM-W/DM1-DM6 is UNPROVEN: needs a discovery lane — open question: what exact lines remain after those lanes? |
| agentrun candidate | 7,942 | 6,300 | exceeds by 1,642 lines; result depends on DM-W/DM1-DM6 landing first; current method-bearing source files include lanes reassigned elsewhere. |

Today's per-file maxes include `agent_run.go` 919 and `coordinator_interrupt.go` 958; coordinator custom-role file has 11 lines. Distribution into seven files ≤900 is therefore not established. dm0b Q1 recommendations remain applicable: try declaration-level reassignment into batch/finalize/modelkit first; if insufficient, ask for explicit ratchet exception (7-file or 900-line limit). No guessed after-lane budget is reported.

## Serial verification selectors (not final)

Reconcile after test relocation; these exact selectors match current names in root tests. Do not run tests during scouting.

* coord: `go test ./internal/agent -run '^(TestCoordinator|TestRun|TestInterrupt|TestCancel|TestRecheck|TestRunInternal)'`; includes `TestCoordinator_InterruptAndSend_UsesInterruptAndReplace` at `interrupt_test.go:44` (the previous `TestRun|TestInterrupt...` regex missed it).
* agentrun: `go test ./internal/agent -run '^(TestRun|TestTurn|TestOwnership|TestDrain|TestInterruptAndSend)'`.
* DM families: use per-package selectors only after tests are relocated; known names include batch `TestBatch`, finalize `TestFinalization`, toolwrap `TestTool`, usage `TestModelUnpriced|TestUpdateSessionUsage`, and wake `TestWake`. **UNPROVEN: needs a discovery lane** — open question: which exact tests relocate with each package, and which package-specific regex covers all its behavior tests?

1. **C1a — establish lower contract and coordinator destination.** Root arithmetic: 198−19=179, zero shims. Call contract types already exist in agentcall; only the root alias removal question remains for DM7/DM8. Build probe, if authorized in later discovery, one package per command via `capm 2g go build ./<one-package>; echo exit=$?`; do not run tests in scouting.
2. **C1b — move sessionAgent execution cluster.** Root arithmetic: 179−10=169, zero shims. Depends on C1a; boundary interface owned by agentcall and implemented in coord. Any output count or line budget remains unproven pending receiver extraction and per-package probe.


The listed test selections are intended families, **not asserted to match current exact relocated test names**. Do not run whole-module `./...`, no whole-module builds, no `.githooks` guard in scouting. Current task ran no build/vet/test and made no probe edits. **UNPROVEN: needs a discovery lane** — open question: after full physical moves, does each destination-package build compile without new export leaks or import cycles?
