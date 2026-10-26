# H4 execution manifest (P0)

**Source-review baseline:** `e1a6a3a3c84e77af144680af76e9a94e886fd8cc`; reviewer base `04af7909f62cbaab7136261d505c3c684a884df6`. No Go/code/workflow changes, tests, builds, formatters, sqlc, generators, or runtime checks are authorized or performed. Source statements are observed; behavior not executed. Fixed-pattern path coverage below is a text search, not semantic proof for aliases or dynamically generated names.

**Reliability.** This manifest is a SEED for the H4 lanes, not a certified inventory. It was produced by static reading only and went through several rounds with an automated reviewer; the orchestrator corrected the rows the reviewer proved wrong (the shell cap-guidance anchor, the Repoint query anchors, the ephemeral-binding row, and the host-registry rebind obligation in §4). Every lane MUST re-open each cited `file:line` and re-run `git grep -n -w <symbol>` for its own symbols before editing; the retained-consumer column in §4 lists the consumers seen at the baseline only. Open classification questions are in §6.

Disposition: DELETE legacy-only authority; REWRITE retain but remove named legacy mechanism; MIXED retain only named declarations; KEEP retained behavior. H4 is atomic activation; P1 importer precedes; H5 and I follow. Lane ownership in §5 is exclusive; a file has one owner even when several lanes consume its API.

## 1. Literal tracked-path inventory

Rows are literal paths; paths listed once. Non-hit lane paths are not deletion candidates. Each row classifies test scenarios and historical prose as retained where applicable. Historical documents below remain historical and are not rewrite targets.

| Lane | Literal path | Disposition / scenario |
|---|---|---|
| P5 | `internal/agent/batch_agent_reaction_scope_test.go` | KEEP batch agent reaction-scope behavior. |
| P5 | `internal/agent/batch_delivery.go` | KEEP batch delivery; supervision alignment reference. |
| P5 | `internal/agent/batch_exec_agent.go` | KEEP batch executor; flag-off comment is test fallback. |
| P5 | `internal/agent/coordinator_batch_stop_retry_test.go` | KEEP batch Stop retry. |
| P2 | `internal/message/message.go` | REWRITE obsolete old-table documentation (`async_jobs` :63). |
| P6 | `internal/shell/background.go` | REWRITE old tool cap guidance: the user-facing limit error names `job_kill` (:454; comment mention `job_output` :61 is prose only). |
| P6 | `internal/shell/background_buffer_test.go` | REWRITE old tool-name comments (`job_output` :141). |
| P6 | `internal/shell/background_cap_test.go` | REWRITE legacy cap prose (`job_kill` :36). |
| P6 | `internal/shell/background_release_test.go` | REWRITE old output-tool comment (`job_output` :177). |
| P6 | `internal/shell/background_shell.go` | REWRITE stale wait comment (`job_output` :273). |
| P6 | `internal/shell/exec_unix.go` | REWRITE old tool comment (`job_kill` :115). |
| P6 | `internal/shell/exec_windows_test.go` | REWRITE old tool comment (`job_kill` :85). |
| P5 | `internal/agent/agent.go` | REWRITE ledger field/construction (`asyncJobs *workLedger` :604). |
| P5 | `internal/agent/agent_drain_decision.go` | REWRITE legacy decision source (`a.asyncJobs.coord` :28). |
| P5 | `internal/agent/agent_notice_pull.go` | REWRITE old-store guard/progress and OFF branch (`a.asyncJobs` :23). |
| P5 | `internal/agent/agent_options.go` | REWRITE legacy store option (`AsyncJobs *workLedger` :27). |
| P5 | `internal/agent/agent_own_work_batch.go` | REWRITE ledger routing to batch facts (`a.asyncJobs.coord` :26). |
| P5 | `internal/agent/agent_own_work_batch_test.go` | REWRITE flag-controlled fact source (`coord.asyncJobs` :43). |
| P5 | `internal/agent/agent_timeouts_test.go` | KEEP timeout contract; port old fixture. |
| P5 | `internal/agent/agent_turn.go` | REWRITE legacy route and ledger dependency (`batchFlagEnabled` :373). |
| P5 | `internal/agent/agent_turn_failure.go` | REWRITE legacy completion carrier (`a.asyncJobs` :317). |
| P5 | `internal/agent/agent_turn_step.go` | REWRITE legacy fallback and started-boundary selector (`batchFlagEnabled` :330). |
| P5 | `internal/agent/agent_turn_step_test.go` | REWRITE legacy carrier fixture; retain assertions (`job_output :38). |
| P5 | `internal/agent/agent_turn_stream.go` | REWRITE completion carrier (`ts.a.asyncJobs` :503). |
| P5 | `internal/agent/agent_turn_stream_retry_test.go` | REWRITE legacy completion fixture only (`newTestAsyncJobStoreWithDataDir :32). |
| P5 | `internal/agent/agent_turn_stream_tool_suggest_test.go` | REWRITE old tool fixture only (`newTestAsyncJobStoreWithDataDir :33). |
| P5 | `internal/agent/async_tool.go` | DELETE wrapper authority; rehome timer/supervision. |
| P5 | `internal/agent/async_tool_sync_test.go` | DELETE wrapper-only cases; retain neutral sync cases. |
| P5 | `internal/agent/async_tool_test.go` | DELETE wrapper/ledger-only tests; port retained behavior. |
| P5 | `internal/agent/batch_allowed_controls.go` | REWRITE temporary selector (`BatchRuntimeEnabled` :11). |
| P5 | `internal/agent/batch_answer_inject_test.go` | REWRITE fixture if flag-dependent; KEEP batch answer/inject (`RUSH_BATCH_RUNTIME :24). |
| P5 | `internal/agent/batch_chain_evidence_test.go` | KEEP batch chain evidence. |
| P5 | `internal/agent/batch_cli_marker_test.go` | KEEP marker; rewrite selector coverage. |
| P5 | `internal/agent/batch_construction_scope_boundary_test.go` | KEEP boundary contract. |
| P5 | `internal/agent/batch_construction_scope_round2_test.go` | KEEP scope contract. |
| P5 | `internal/agent/batch_credentials_escrow_integration_test.go` | KEEP credential escrow. |
| P5 | `internal/agent/batch_credentials_marker_test.go` | KEEP marker behavior. |
| P5 | `internal/agent/batch_cutover_accounting_test.go` | KEEP batch accounting; no OFF authority. |
| P5 | `internal/agent/batch_cutover_await_b_test.go` | KEEP await; translate legacy fixture. |
| P5 | `internal/agent/batch_cutover_await_test.go` | KEEP await; translate legacy fixture. |
| P5 | `internal/agent/batch_cutover_consumers_test.go` | KEEP batch consumers; remove old-store prerequisite. |
| P5 | `internal/agent/batch_cutover_driver.go` | REWRITE old authority guard (`session.AsyncJobStore` :15). |
| P5 | `internal/agent/batch_cutover_facts.go` | REWRITE ledger adapter/selector (`async_jobs` :3,34). |
| P5 | `internal/agent/batch_cutover_facts_node.go` | KEEP node-backed facts. |
| P5 | `internal/agent/batch_cutover_facts_test.go` | DELETE ledger adapter tests; KEEP node/unavailable cases. |
| P5 | `internal/agent/batch_cutover_held_identity_test.go` | KEEP held-identity behavior. |
| P5 | `internal/agent/batch_cutover_rerun.go` | REWRITE old ledger Stop path to batch; see section 6 item 8. |
| P5 | `internal/agent/batch_cutover_rerun_test.go` | REWRITE legacy fixtures; KEEP rerun semantics; see section 6 item 9. |
| P5 | `internal/agent/batch_drain_turn_entry_test.go` | KEEP batch drain; remove selector assumption. |
| P5 | `internal/agent/batch_drain_turn_test.go` | KEEP batch drain behavior. |
| P5 | `internal/agent/batch_exec_agent_doc.go` | REWRITE stale legacy-execution prose (`asyncTool.Run :17). |
| P5 | `internal/agent/batch_exec_generic_test.go` | KEEP generic batch execution. |
| P5 | `internal/agent/batch_fixture_port_a1_test.go` | REWRITE OFF/ON fixture branches to batch (`batchFlagEnabled :25). |
| P5 | `internal/agent/batch_fixture_port_a2_test.go` | REWRITE legacy fixture arguments and OFF branches (`workLedger :31). |
| P5 | `internal/agent/batch_fixture_port_a3_test.go` | REWRITE direct old-table writes to batch equivalent (`UPDATE async_jobs :36). |
| P5 | `internal/agent/batch_fixture_port_b1_test.go` | REWRITE old seeding/ledger args; retain scenario (`session.AsyncJobStore :30). |
| P5 | `internal/agent/batch_fixture_port_selftest_test.go` | REWRITE helper self-test branches (`UPDATE async_jobs :167). |
| P5 | `internal/agent/batch_fixture_port_test.go` | REWRITE fixture helper signatures/OFF semantics (`session.AsyncJobStore :14). |
| P5 | `internal/agent/batch_gate_refusal_test.go` | KEEP genuine unbound fail-closed case. |
| P5 | `internal/agent/batch_install_inventory_test.go` | REWRITE inventory to batch-only tools (`async_jobs :240). |
| P5 | `internal/agent/batch_install_policy.go` | REWRITE legacy policy hooks (`AsyncCompletion` :175, `workLedger` :191). |
| P5 | `internal/agent/batch_install_tools.go` | REWRITE old wrapper assembly; preserve batch tool safety (`workLedger :39). |
| P5 | `internal/agent/batch_install_tools_test.go` | REWRITE inventory expectations; retain policy tests (`asyncTool :176). |
| P5 | `internal/agent/batch_model_flag_test.go` | REWRITE all-values batch selection (`RUSH_BATCH_RUNTIME :15). |
| P5 | `internal/agent/batch_notice_presentation_test.go` | KEEP presentation; use neutral completion type. |
| P5 | `internal/agent/batch_private_fetch_round2_test.go` | KEEP private-fetch behavior; remove obsolete flag references. |
| P5 | `internal/agent/batch_question_integration_test.go` | REWRITE legacy question fixtures; retain held-question behavior (`async_jobs :112). |
| P5 | `internal/agent/batch_reaction_persist_test.go` | KEEP batch assertions (`async_jobs` :181). |
| P5 | `internal/agent/batch_scope_recovery_test.go` | REWRITE flag expectations (`AsyncJobStore` :24). |
| P5 | `internal/agent/batch_supervision_arming.go` | REWRITE ledger started notification (`noteWorkStarted` :48). |
| P5 | `internal/agent/batch_supervision_arming_test.go` | REWRITE ledger assertion helper; retain arming/exemption (`workLedger :64). |
| P5 | `internal/agent/batch_supervision_debt_test.go` | REWRITE ledger carrier; keep batch debt (`RUSH_BATCH_RUNTIME :94). |
| P5 | `internal/agent/batch_timeout_wake_parity_test.go` | KEEP batch parity; legacy comparison is baseline only. |
| P5 | `internal/agent/batch_toolset_inventory_test.go` | REWRITE OFF expectations; retain policy cases (`RUSH_BATCH_RUNTIME :127). |
| P5 | `internal/agent/bgshell_claim.go` | REWRITE old claim carrier (`async_jobs` :50, `asyncTool` :70). |
| P5 | `internal/agent/bgshell_claim_test.go` | DELETE ledger claims; translate process-lifetime contract. |
| P5 | `internal/agent/bgshell_observer_recheck_test.go` | KEEP physical observer/recheck. |
| P5 | `internal/agent/cli_active_work.go` | REWRITE ON legacy question fallback (`c.asyncJobs.store` :38). |
| P5 | `internal/agent/cli_active_work_batch_flagon_test.go` | REWRITE unconditional batch selector tests (`RUSH_BATCH_RUNTIME :19). |
| P5 | `internal/agent/cli_active_work_batch_question_test.go` | KEEP batch question behavior. |
| P5 | `internal/agent/cli_tool_bridge_test.go` | KEEP bridge behavior; no authority removal from spelling alone. |
| P5 | `internal/agent/cliscope_completion_hold_test.go` | REWRITE carrier to neutral type; keep hold semantics; see section 6 item 10. |
| P5 | `internal/agent/coordinator.go` | REWRITE ledger ownership/construction (`asyncJobs *workLedger` :286). |
| P5 | `internal/agent/coordinator_agent_control.go` | REWRITE ledger map reads (`c.asyncJobs` :47). |
| P5 | `internal/agent/coordinator_agent_control_test.go` | REWRITE ledger fixture; preserve list/stop behavior (`coord.asyncJobs.store :126). |
| P5 | `internal/agent/coordinator_autoresume_test.go` | KEEP lifecycle; port old fixture. |
| P5 | `internal/agent/coordinator_await_tasks.go` | REWRITE old-store guard/OFF route (`batchFlagEnabled` :51). |
| P5 | `internal/agent/coordinator_await_tasks_identity_test.go` | KEEP identity; use batch fixture. |
| P5 | `internal/agent/coordinator_await_tasks_worker_test.go` | REWRITE selectors/carriers; retain await behavior (`session.NewAsyncJobStore :24). |
| P5 | `internal/agent/coordinator_background.go` | REWRITE ledger callback; preserve generic completion (`c.asyncJobs :230). |
| P5 | `internal/agent/coordinator_batch_stop_running_test.go` | KEEP batch Stop; ensure no legacy mirror. |
| P5 | `internal/agent/coordinator_batch_stop_test.go` | KEEP batch Stop. |
| P5 | `internal/agent/coordinator_bgshell_cap.go` | REWRITE cap ownership after transfer (`pruneBGShellOverCap :87). |
| P5 | `internal/agent/coordinator_bgshell_child_test.go` | REWRITE ledger fixture; retain child lifecycle (`AsyncCompletion :25). |
| P5 | `internal/agent/coordinator_bgshell_policy_test.go` | KEEP policy; remove wrapper setup. |
| P5 | `internal/agent/coordinator_bgshell_window_test.go` | REWRITE legacy window setup; see section 6 item 11. |
| P5 | `internal/agent/coordinator_child_policy_durable_test.go` | REWRITE legacy premise; retain conservative policy (`workLedger` :28). |
| P5 | `internal/agent/coordinator_child_question_test.go` | REWRITE legacy question jobs; retain response scenario (`AsyncCompletion :36). |
| P5 | `internal/agent/coordinator_claim_driver_error_test.go` | REWRITE ledger fixture; retain claim failures (`workLedger :17). |
| P5 | `internal/agent/coordinator_cli_scope_test.go` | REWRITE old job seeds; preserve scope (`session.AsyncJobStore :24). |
| P5 | `internal/agent/coordinator_drain_policy.go` | REWRITE old-store owner query/guard to batch facts (`c.asyncJobs.store.ListAsyncJobsForOwner :222). |
| P5 | `internal/agent/coordinator_external_driver_durable_test.go` | REWRITE ledger marker fixture; preserve driver fact (`session.AsyncJobStore :30). |
| P5 | `internal/agent/coordinator_external_driver_release_test.go` | REWRITE ledger fixture; keep release behavior (`coord.asyncJobs :27). |
| P5 | `internal/agent/coordinator_interrupt.go` | REWRITE Stop ledger paths and old-table write (`c.asyncJobs` :93, `treeSessionIDs` :100). |
| P5 | `internal/agent/coordinator_rate_limit.go` | REWRITE retained rate-limit ownership (`c.asyncJobs` :163). |
| P5 | `internal/agent/coordinator_rate_limit_budget_test.go` | REWRITE direct sessionJobs fixture; preserve budget behavior (`sessionJobs :40). |
| P5 | `internal/agent/coordinator_rate_limit_loop_test.go` | KEEP rate-limit policy. |
| P5 | `internal/agent/coordinator_rate_limit_test.go` | KEEP rate-limit behavior. |
| P5 | `internal/agent/coordinator_reaction_chain.go` | REWRITE ledger debt/driver ownership (`InsertSessionNotice` :60). |
| P5 | `internal/agent/coordinator_reaction_chain_test.go` | REWRITE ledger fixture; retain chain contract (`batchFlagEnabled :35). |
| P5 | `internal/agent/coordinator_reaction_repeated_test.go` | KEEP repeated reaction semantics. |
| P5 | `internal/agent/coordinator_reaction_source.go` | REWRITE source selection; remove old-store dependency (`batchFlagEnabled :216). |
| P5 | `internal/agent/coordinator_recheck.go` | REWRITE ledger recheck dependency (`c.asyncJobs` :98). |
| P5 | `internal/agent/coordinator_recheck_child_test.go` | REWRITE ledger seed; retain child recheck (`AsyncCompletion :25). |
| P5 | `internal/agent/coordinator_recheck_debt_deadline_test.go` | REWRITE old debt carrier; retain deadline behavior (`session.NewAsyncJobStore :55). |
| P5 | `internal/agent/coordinator_rerun.go` | REWRITE old stop/rerun authority (`BatchRuntimeEnabled` :10). |
| P5 | `internal/agent/coordinator_rerun_test.go` | REWRITE old-store fixture; retain rerun (`session.AsyncJobStore :23). |
| P5 | `internal/agent/coordinator_run.go` | REWRITE ledger lifecycle/control calls (`stallDetachCleanupSession :183). |
| P5 | `internal/agent/coordinator_run_drain_test.go` | REWRITE ledger fixture; keep drain (`coord.asyncJobs :88). |
| P5 | `internal/agent/coordinator_scope_recovery_test.go` | REWRITE old claim fixture; retain recovery (`newTestAsyncJobStoreWithDataDir :22). |
| P5 | `internal/agent/coordinator_session_state_sweep.go` | REWRITE ledger sweep owner (`sweepIdleSessions` :29). |
| P5 | `internal/agent/coordinator_session_state_sweep_test.go` | DELETE ledger-only helper test; retain batch sweep assertions. |
| P5 | `internal/agent/coordinator_stop_restart_test.go` | REWRITE ledger Stop fixture (`newWorkLedger :34). |
| P5 | `internal/agent/coordinator_stop_tree_test.go` | REWRITE cancelTree premise; retain transitive Stop (`newTestAsyncJobStore :89). |
| P5 | `internal/agent/coordinator_subagent_drivers.go` | REWRITE old driver callbacks to retained owner; see section 6 item 12. |
| P5 | `internal/agent/coordinator_subagent_drivers_test.go` | REWRITE fixture; preserve routing (`newTestAsyncJobStoreWithDataDir :28). |
| P5 | `internal/agent/coordinator_subagents.go` | REWRITE ledger delegation state; see section 6 item 13. |
| P5 | `internal/agent/coordinator_subagents_resume_test.go` | REWRITE ledger fixture; retain resume; see section 6 item 14. |
| P5 | `internal/agent/coordinator_tool_pinning_test.go` | KEEP pinning; update removed tool names only. |
| P5 | `internal/agent/coordinator_tools.go` | REWRITE old factories/wrappers (`AsyncJobs: c.asyncJobs` :119). |
| P5 | `internal/agent/coordinator_tools_test.go` | REWRITE old inventory; retain supported tool assertions (`batchFlagEnabled :396). |
| P5 | `internal/agent/coordinator_wake.go` | REWRITE hint ownership (`async_jobs` :4, `c.asyncJobs` :28). |
| P5 | `internal/agent/coordinator_wake_allowlist_test.go` | KEEP allowlist policy; update names if exact expectation. |
| P5 | `internal/agent/coordinator_wake_reaction_debt_test.go` | REWRITE ledger/store fixture; retain debt (`session.AsyncJobStore :51). |
| P5 | `internal/agent/coordinator_work_scope.go` | REWRITE legacy scope owner (`workLedger` :6). |
| P5 | `internal/agent/coordinator_work_scope_test.go` | REWRITE ledger fixture; keep scope (`coord.asyncJobs.store :44). |
| P5 | `internal/agent/default_job_timeout_test.go` | REWRITE timeout ledger/carriers; retain timeout contract (`workLedger :38). |
| P5 | `internal/agent/delegation_question.go` | REWRITE ledger methods (`workLedger.heldQuestionJob` :39). |
| P5 | `internal/agent/delegation_question_test.go` | DELETE legacy held-ledger cases; translate batch held-question behavior. |
| P5 | `internal/agent/drain_attempt.go` | REWRITE ledger dependencies (`a.asyncJobs.hintSeqOf` :172). |
| P5 | `internal/agent/drain_attempt_fixture_test.go` | REWRITE real ledger fixture to retained owner (`session.NewAsyncJobStore :101). |
| P5 | `internal/agent/drain_bgcap_test.go` | KEEP cap semantics; remove legacy setup. |
| P5 | `internal/agent/drain_compaction_continuation_test.go` | KEEP continuation semantics. |
| P5 | `internal/agent/drain_compaction_submit_test.go` | KEEP submit semantics. |
| P5 | `internal/agent/drain_debt_edge_cases_test.go` | REWRITE old debt carrier; retain edge cases; see section 6 item 15. |
| P5 | `internal/agent/drain_epoch_test.go` | KEEP unbound/epoch negative case. |
| P5 | `internal/agent/drain_hint_sweep_test.go` | REWRITE ledger hint state; keep sweep; see section 6 item 16. |
| P5 | `internal/agent/drain_launch_gate_test.go` | KEEP gate contract. |
| P5 | `internal/agent/drain_notice_pull_test.go` | REWRITE old notice store fixture; keep pull contract (`session.NewAsyncJobStore :197). |
| P5 | `internal/agent/drain_orphan_release_convergence_test.go` | KEEP convergence; rebind fixture. |
| P5 | `internal/agent/drain_policy_restore_test.go` | KEEP policy restoration; rebind fixture. |
| P5 | `internal/agent/drain_x1_batch_fixture_test.go` | REWRITE cross-authority fixture; preserve batch premise; see section 6 item 17. |
| P5 | `internal/agent/inline_window.go` | KEEP inline-window constant; separate from ledger setter. |
| P5 | `internal/agent/loop_detection_test.go` | KEEP loop detection; remove only obsolete runtime expectation. |
| P5 | `internal/agent/policy_leaf.go` | REWRITE removed wrapper dispatch (`stallDetacherTool` :69). |
| P5 | `internal/agent/policy_leaf_baseline_test.go` | REWRITE ledger fixture; retain baseline policy (`workLedger :21). |
| P5 | `internal/agent/policy_leaf_test.go` | REWRITE old control cases; keep policy floors (`asyncTool :47). |
| P6 | `internal/agent/prompt/prompt.go` | REWRITE old inventory references; keep prompt assembly (`BatchRuntimeEnabled :800). |
| P6 | `internal/agent/prompt/prompt_await_tasks_test.go` | REWRITE OFF expectation; keep guidance tests (`RUSH_BATCH_RUNTIME :56). |
| P6 | `internal/agent/prompt/prompt_scoped_test.go` | REWRITE OFF expectations; retain scope tests (`RUSH_BATCH_RUNTIME :55). |
| P5 | `internal/agent/question_stop_format_test.go` | REWRITE old claim fixture (`ClaimAsyncJob` :54). |
| P5 | `internal/agent/reaction_write_cutover.go` | REWRITE selector/fallback (`async_jobs` :12). |
| P5 | `internal/agent/release_callback_join_test.go` | KEEP callback join lifecycle. |
| P5 | `internal/agent/restricted_stall_detach_test.go` | DELETE detach action tests; retain denial scenario. |
| P5 | `internal/agent/restricted_tool.go` | REWRITE removed tool policy case (`job_kill` :41). |
| P5 | `internal/agent/session_idle_trigger_test.go` | REWRITE legacy carrier (`AsyncCompletion` :109). |
| P5 | `internal/agent/session_wake_dispatch_test.go` | REWRITE ledger hint carrier (`claimExternalDriver` :16). |
| P5 | `internal/agent/step_progress.go` | REWRITE old cap path (`job_kill` :25). |
| P5 | `internal/agent/step_progress_test.go` | REWRITE cap metadata source; retain progress cases (`job_output :108). |
| P5 | `internal/agent/stream_watchdog_toolpause_test.go` | KEEP watchdog/pause policy; remove detach authority only. |
| P5 | `internal/agent/supervision.go` | REWRITE ledger receiver (`workLedger` :156). |
| P5 | `internal/agent/supervision_silence.go` | REWRITE legacy snapshot dependency (`workLedger.jobSilence` :58). |
| P5 | `internal/agent/supervision_silence_test.go` | DELETE legacy silent-job cases; retain SV cases subject to signoff. |
| P5 | `internal/agent/supervision_test.go` | REWRITE ledger harness; preserve SV behavior (`workLedger :30). |
| P6 | `internal/agent/templates/coder.md.tpl` | REWRITE old controls/guidance; retain general guidance (`BatchRuntimeAvailable :20). |
| P5 | `internal/agent/timeout_service.go` | REWRITE ledger timer coupling (`ledger *workLedger` :49). |
| P5 | `internal/agent/tool_suggest_test.go` | KEEP suggestions; remove obsolete tool candidates only. |
| P6 | `internal/agent/tools/agent_control_test.go` | REWRITE removed inventory; retain batch controls; see section 6 item 18. |
| P6 | `internal/agent/tools/await_tasks.md` | REWRITE legacy control prose (`job_output :27). |
| P6 | `internal/agent/tools/bash.go` | REWRITE old wrapper integration; preserve physical shell/output (`job_kill :282). |
| P6 | `internal/agent/tools/bash.md.tpl` | REWRITE old async prose (`job_output :32). |
| P6 | `internal/agent/tools/bash_start_failure_test.go` | REWRITE wrapper setup; retain physical start failure case (`job_kill :92). |
| P6 | `internal/agent/tools/job_kill.go` | DELETE old model control. |
| P6 | `internal/agent/tools/job_kill.md` | DELETE old tool description. |
| P6 | `internal/agent/tools/job_kill_bgshell.go` | DELETE old background-shell control. |
| P6 | `internal/agent/tools/job_output.go` | DELETE old model output tool. |
| P6 | `internal/agent/tools/job_output.md` | DELETE old tool description. |
| P6 | `internal/agent/tools/job_output_lastoutput_test.go` | DELETE old output-tool tests; retain buffer tests elsewhere. |
| P6 | `internal/agent/tools/job_output_pending.go` | DELETE old pending-output implementation. |
| P6 | `internal/agent/tools/job_output_test.go` | DELETE old tool tests. |
| P6 | `internal/agent/tools/job_run_command_control_test.go` | REWRITE removed controls; keep process contract (`NewJobOutputTool :84). |
| P6 | `internal/agent/tools/job_shell_resolver.go` | DELETE legacy shell resolver. |
| P6 | `internal/agent/tools/job_shell_resolver_test.go` | DELETE resolver-only tests. |
| P6 | `internal/agent/tools/run_command.go` | REWRITE wrapper edge; preserve direct physical command; see section 6 item 19. |
| P6 | `internal/agent/tools/run_command.md.tpl` | REWRITE stale async prose; preserve safety instructions (`job_kill :19). |
| P6 | `internal/agent/tools/run_command_output_buffer.go` | KEEP physical output buffer/lifetime. |
| P6 | `internal/agent/tools/run_command_process_kill_test.go` | KEEP process termination contract. |
| P6 | `internal/agent/tools/run_command_process_unix.go` | KEEP OS process implementation. |
| P6 | `internal/agent/tools/run_command_process_windows.go` | KEEP OS process implementation. |
| P6 | `internal/agent/tools/stall_detach_controller.go` | DELETE detach controller; preserve watchdog policy elsewhere. |
| P5 | `internal/agent/turn_arbiter.go` | REWRITE old-store/ledger guard (`c.asyncJobs` :431). |
| P5 | `internal/agent/turn_arbiter_foreign_test.go` | REWRITE ledger fixture; retain driver fact (`batchFlagEnabled :175). |
| P5 | `internal/agent/turn_arbiter_shadow_test.go` | REWRITE dual-runtime expectation; retain arbiter contract (`batchFlagEnabled :145). |
| P5 | `internal/agent/turn_arbiter_state.go` | REWRITE ledger-owned fields (`workLedger` :4). |
| P5 | `internal/agent/turn_arbiter_testhelpers_test.go` | REWRITE ledger pacing helper; retain used policy tests (`workLedger :42). |
| P5 | `internal/agent/turn_progress_batch_test.go` | REWRITE ledger carrier; retain batch outcome (`batchFlagEnabled :49). |
| P5 | `internal/agent/turn_progress_fixture_test.go` | REWRITE legacy fixture and flag branches (`AsyncCompletion :48). |
| P5 | `internal/agent/turn_progress_guard.go` | REWRITE legacy start/refusal metadata (`workLedger.Start` :218). |
| P5 | `internal/agent/turn_stall_detach_cleanup_test.go` | DELETE detach cleanup expectations; retain clock policy where independent. |
| P5 | `internal/agent/turn_stall_policy_spacing_test.go` | KEEP stall policy spacing. |
| P5 | `internal/agent/turn_stall_tool_detach.go` | DELETE detach registry/action; keep generic stall clock. |
| P5 | `internal/agent/turn_stall_tool_test.go` | REWRITE to stall policy; delete detach expectations (`stallDetacherTool :56). |
| P5 | `internal/agent/turn_stall_zero_timeout_test.go` | KEEP zero-timeout policy. |
| P5 | `internal/agent/work_job.go` | DELETE old job wrapper/type; batch owns physical process. |
| P5 | `internal/agent/work_ledger.go` | DELETE authority; extract `AsyncCompletion` and retained hints/supervision first. |
| P5 | `internal/agent/work_ledger_abort_cancelled_test.go` | DELETE old transition; port cancellation contract. |
| P5 | `internal/agent/work_ledger_ack_gate_test.go` | DELETE legacy ACK test; batch delivery coverage remains. |
| P5 | `internal/agent/work_ledger_ack_tag_test.go` | DELETE old ACK-tag tests; retain shared formatting assertion. |
| P5 | `internal/agent/work_ledger_announce.go` | DELETE old ledger authority. |
| P5 | `internal/agent/work_ledger_await.go` | DELETE old ledger await queue. |
| P5 | `internal/agent/work_ledger_bgshell_kill.go` | DELETE old kill authority. |
| P5 | `internal/agent/work_ledger_bgshell_kill_race_test.go` | DELETE authority race; process tests remain P6. |
| P5 | `internal/agent/work_ledger_bgshell_kill_test.go` | DELETE old authority tests. |
| P5 | `internal/agent/work_ledger_cap.go` | DELETE ledger cap after policy transfer. |
| P5 | `internal/agent/work_ledger_cap_test.go` | DELETE implementation test; keep batch cap tests. |
| P5 | `internal/agent/work_ledger_claim_id_test.go` | DELETE old claim test; preserve batch identity. |
| P5 | `internal/agent/work_ledger_delegation.go` | REWRITE/DELETE delegation machine (`workLedger.armDelegation` :57). |
| P5 | `internal/agent/work_ledger_delegation_test.go` | DELETE old state tests; preserve batch delegation scenarios. |
| P5 | `internal/agent/work_ledger_driver.go` | DELETE ledger driver receiver (`foreignLiveDriver` :9). |
| P5 | `internal/agent/work_ledger_durable_test.go` | DELETE old storage tests; importer tests P1. |
| P5 | `internal/agent/work_ledger_identity_test.go` | DELETE old identity tests; translate batch identity. |
| P5 | `internal/agent/work_ledger_inline.go` | DELETE old inline path; retain batch policy. |
| P5 | `internal/agent/work_ledger_inline_test.go` | DELETE old inline tests; translate retained sync behavior. |
| P5 | `internal/agent/work_ledger_job_kill_result_test.go` | DELETE removed job_kill result tests. |
| P5 | `internal/agent/work_ledger_job_kill_reuse_test.go` | DELETE removed job_kill identity tests. |
| P5 | `internal/agent/work_ledger_job_kill_terminal_test.go` | DELETE removed job_kill terminal tests. |
| P5 | `internal/agent/work_ledger_job_kill_test.go` | DELETE removed job_kill tests. |
| P5 | `internal/agent/work_ledger_job_shell_test.go` | DELETE resolver contract; retain physical shell tests P6. |
| P5 | `internal/agent/work_ledger_launch_failure_test.go` | DELETE old fixture; translate batch launch failure. |
| P5 | `internal/agent/work_ledger_next_event_test.go` | DELETE old event queue tests. |
| P5 | `internal/agent/work_ledger_pending.go` | DELETE legacy pending queue. |
| P5 | `internal/agent/work_ledger_pending_test.go` | DELETE legacy pending queue tests. |
| P5 | `internal/agent/work_ledger_reaction.go` | DELETE ledger hint owner (`workLedger.bumpHint` :21). |
| P5 | `internal/agent/work_ledger_rerun.go` | DELETE old rerun stop helper. |
| P5 | `internal/agent/work_ledger_rerun_test.go` | DELETE legacy rerun tests; retain batch rerun. |
| P5 | `internal/agent/work_ledger_review_fixes_test.go` | DELETE old races unless translated individually. |
| P5 | `internal/agent/work_ledger_sync_timeout_test.go` | DELETE ledger timeout tests; retain generic timer tests. |
| P5 | `internal/agent/work_ledger_test.go` | DELETE ledger lifecycle tests; port necessary contracts. |
| P5 | `internal/agent/work_ledger_teststore_test.go` | DELETE fake old store/job helpers. |
| P5 | `internal/agent/work_ledger_timeout.go` | DELETE legacy timeout handler; preserve generic timeout service. |
| P5 | `internal/agent/work_ledger_timeout_test.go` | DELETE old timeout scenarios; retain batch/SV cases. |
| P5 | `internal/agent/work_ledger_transition.go` | DELETE old SQL transition authority. |
| P5 | `internal/agent/x2_delegation_fixture_test.go` | REWRITE authority selector fixture to batch delegation; see section 6 item 20. |
| P4 | `internal/session/async_job_bgshell.go` | DELETE old store background-shell methods. |
| P4 | `internal/session/async_job_bgshell_announce_test.go` | DELETE old store announcement test; batch notice coverage remains. |
| P4 | `internal/session/async_job_bgshell_collision_test.go` | DELETE old claim collision; batch equivalent only if independently needed. |
| P4 | `internal/session/async_job_bgshell_test.go` | DELETE legacy background-shell store tests. |
| P4 | `internal/session/async_job_claim_id_test.go` | DELETE old claim-ID test; batch identity is separate. |
| P4 | `internal/session/async_job_dur2_atomicity_test.go` | DELETE old-table transaction test; retain batch atomicity. |
| P4 | `internal/session/async_job_gc_jobkill_test.go` | DELETE old GC/kill semantics. |
| P4 | `internal/session/async_job_gc_test.go` | DELETE old-table GC tests; retain batch retention. |
| P4 | `internal/session/async_job_kill_notice_test.go` | DELETE old kill notice; retain batch cancellation notice as applicable. |
| P4 | `internal/session/async_job_reaction.go` | REWRITE retained notice/debt operations (`async_jobs.sql` :6). |
| P4 | `internal/session/async_job_reaction_test.go` | DELETE old SQL tests; batch reaction tests remain. |
| P4 | `internal/session/async_job_reader.go` | DELETE old reader and live-work queries. |
| P4 | `internal/session/async_job_reader_batch.go` | DELETE old reader (`AsyncJobStore.LiveWorkForRoots` :40). |
| P4 | `internal/session/async_job_reader_test.go` | DELETE old reader cases; translate required projection behavior. |
| P4 | `internal/session/async_job_recovery.go` | DELETE old recovery authority; retain batch recovery. |
| P4 | `internal/session/async_job_recovery_concurrent_test.go` | DELETE old-store concurrency test; use P1/batch coverage. |
| P4 | `internal/session/async_job_recovery_test.go` | DELETE old-table recovery; preserve batch-equivalent contract. |
| P4 | `internal/session/async_job_rerun.go` | DELETE old rerun SQL. |
| P4 | `internal/session/async_job_rerun_test.go` | DELETE old SQL tests; preserve batch rerun acceptance. |
| P4 | `internal/session/async_job_reset.go` | DELETE old-store reset. |
| P4 | `internal/session/async_job_store.go` | DELETE old store; move retained notice/host APIs. |
| P4 | `internal/session/async_job_store_lockorder_test.go` | DELETE old-store lock-order test; retain host/batch lock tests. |
| P4 | `internal/session/async_job_store_test.go` | DELETE old store tests; do not port obsolete row assertions. |
| P4 | `internal/session/async_job_store_testhook.go` | DELETE old-store-only hook. |
| P4 | `internal/session/batch_adapters.go` | REWRITE old-store adapter calls (`AsyncJobStore.EnsureHost` :341). |
| P4 | `internal/session/batch_consumers_doc.go` | REWRITE stale prose (`async_jobs` :5). |
| P4 | `internal/session/batch_migration_test.go` | REWRITE old-table fixture (`ClaimAsyncJob` :42). |
| P4 | `internal/session/batch_outbox_doc.go` | REWRITE old adapter references (`job_tool_call_id` :25). |
| P4 | `internal/session/batch_pull.go` | KEEP batch pull. |
| P4 | `internal/session/batch_pull_result_test.go` | KEEP batch pull result. |
| P4 | `internal/session/batch_reaction_test.go` | KEEP batch reaction/wake-zero. |
| P4 | `internal/session/batch_recovery_doc.go` | KEEP batch recovery guidance; correct stale comparison. |
| P4 | `internal/session/host_driver_test.go` | REWRITE host fixture (`async_jobs` :3). |
| P4 | `internal/session/host_lock.go` | KEEP OS host lock. |
| P4 | `internal/session/host_lock_register_race_test.go` | REWRITE old registration setup (`NewAsyncJobStore` :90). |
| P4 | `internal/session/host_lock_test.go` | REWRITE old claim fixture (`ClaimAsyncJob` :241). |
| P4 | `internal/session/host_store.go` | KEEP batch HostStore. |
| P4 | `internal/session/host_store_test.go` | KEEP HostStore tests. |
| P4 | `internal/session/notice_pull.go` | MIXED keep notice types (`JobNoticeRow` :73); remove old helpers. |
| P4 | `internal/session/notice_pull_test.go` | DELETE old job-row pull cases (`AsyncJobStore` :48). |
| P4 | `internal/session/notice_row_ids.go` | MIXED keep IDs (`AsyncJobStore` :31); delete old job receiver. |
| P4 | `internal/session/notice_row_ids_test.go` | DELETE old debt cases (`async_jobs` :96). |
| P4 | `internal/session/retention_purge.go` | MIXED keep `AsyncDataRetentionAge` :29. |
| P4 | `internal/session/session.go` | REWRITE old store fields (`async_jobs` :125). |
| P4 | `internal/session/session_activity.go` | REWRITE old-store activity query (`async_jobs` :135). |
| P4 | `internal/session/session_child_question.go` | MIXED keep parser (`async_jobs` :26); remove old-store receiver. |
| P4 | `internal/session/session_child_question_test.go` | KEEP parser/presentation with neutral inputs. |
| P4 | `internal/session/session_child_question_void_test.go` | DELETE old-row void fixture. |
| P4 | `internal/session/session_driver.go` | MIXED keep shared core (`AsyncJobStore` :91); remove old receiver. |
| P4 | `internal/session/session_driver_test.go` | REWRITE old-store driver fixture (`AsyncJobStore` :25). |
| P4 | `internal/session/session_lifecycle.go` | KEEP lifecycle. |
| P4 | `internal/session/session_notice_store.go` | KEEP SessionNoticeStore and bound lookup; retain job_tool_call_id batch identity. |
| P4 | `internal/session/session_notice_store_test.go` | KEEP notice-store tests (`async_jobs` :35). |
| P2 | `internal/db/sql/async_jobs.sql` | DELETE complete legacy query file (`async_jobs` :25). |
| P2 | `internal/db/async_jobs.sql.go` | DELETE old generated source (`archiveAsyncJobToolCallID` :14). |
| P2 | `internal/db/models.go` | MIXED remove `AsyncJob` :18; retain `AsyncHost` :11. |
| P2 | `internal/db/querier.go` | MIXED remove legacy methods (`ArchiveAsyncJobToolCallID` :52). |
| P2 | `internal/db/db.go` | MIXED remove legacy wiring (`archiveAsyncJobToolCallIDStmt` :39). |
| P2 | `internal/db/sql/session_notices.sql` | MIXED remove only the `RepointSessionNoticesJobToolCallID` query (`-- name:` :127). |
| P2 | `internal/db/session_notices.sql.go` | MIXED remove the generated Repoint symbols (const :389, `...Params` :394, method :407). |
| P2 | `internal/db/sql/files.sql` | MIXED remove invalid `ListNewFiles` :58. |
| P2 | `internal/db/files.sql.go` | MIXED remove generated `ListNewFiles` :251. |
| P2 | `internal/db/migrations/20260809000001_add_session_run_queue.sql` | REWRITE active migration chain to one final initial schema (`+goose Up :1). |
| P2 | `internal/db/migrations/20260928000002_add_async_phase4_core.sql` | REWRITE/replace active chain (`CREATE TABLE IF NOT EXISTS async_jobs :35). |
| P2 | `internal/db/migrations/20260929000002_add_claim_id_to_async_jobs.sql` | REWRITE/replace active chain (`ALTER TABLE async_jobs :26). |
| P2 | `internal/db/migrations/20260929000003_add_announce_message_id_to_async_jobs.sql` | REWRITE/replace active chain (`ALTER TABLE async_jobs ADD COLUMN announce_message_id :11). |
| P2 | `internal/db/migrations/20260929000004_add_session_drivers.sql` | REWRITE/replace chain while retaining required driver schema (`CREATE TABLE IF NOT EXISTS session_drivers :14). |
| P2 | `internal/db/migrations/20260929000005_add_wake_schedules.sql` | REWRITE/replace chain while retaining wake schedules (`CREATE TABLE IF NOT EXISTS wake_schedules :15). |
| P2 | `internal/db/migrations/20261005000001_bg_shell_jobs.sql` | REWRITE/replace chain; no old background-shell table (`CREATE TABLE async_jobs_new :17). |
| P1 | `internal/db/legacymigrations/20260928000002_add_async_phase4_core.sql` | KEEP frozen historical source migration. |
| P1 | `internal/db/legacymigrations/20260929000002_add_claim_id_to_async_jobs.sql` | KEEP frozen historical source migration. |
| P1 | `internal/db/legacymigrations/20260929000003_add_announce_message_id_to_async_jobs.sql` | KEEP frozen historical source migration. |
| P1 | `internal/db/legacymigrations/20260929000004_add_session_drivers.sql` | KEEP frozen historical source migration. |
| P1 | `internal/db/legacymigrations/20260929000005_add_wake_schedules.sql` | KEEP frozen historical source migration. |
| P1 | `internal/db/legacymigrations/20261005000001_bg_shell_jobs.sql` | KEEP frozen historical source migration. |
| P2 | `internal/db/async_phase4_schema_test.go` | REWRITE old schema tests (`async_jobs` :109). |
| P2 | `internal/db/async_phase4_settle_failure_test.go` | DELETE old settlement test (`makeDoneAsyncJob` :17). |
| P1 | `internal/db/legacy_import.go` | REWRITE importer activation only within P1; preserve frozen source lineage (`async_jobs :353). |
| P1 | `internal/db/legacy_import_policy.go` | KEEP/REWRITE importer policy within P1; discard old execution state (`async_jobs :26). |
| P1 | `internal/db/legacy_import_test.go` | REWRITE fixtures to frozen migration chain; preserve importer scenarios (`async_jobs :81). |
| P1 | `internal/session/legacy_import_drain_external_test.go` | REWRITE source fixture to frozen Goose lineage (jobbatch-pim worktree: `fs.Sub` over `legacymigrations` :47, `goose.NewProvider` :49; this worktree: `db.Migrate` :48); preserve external drain scenario (`TestLegacyImportNormalizedProcessingOutboxIsDrainedEndToEnd` :39, `DrainOrphanOutboxEntry` :85, idempotent second drain :100). |
| P2 | `internal/db/migrate_live_hosts.go` | REWRITE host compatibility to store-v1 (`async_jobs` :19). |
| P2 | `internal/db/migrate_live_hosts_test.go` | REWRITE fixture to frozen chain (`async_jobs` :33). |
| P2 | `internal/db/migrate_policy.go` | KEEP permission logic (`async_jobs` :81). |
| P2 | `internal/db/migrate_policy_test.go` | KEEP migration permission scenarios. |
| P2 | `internal/db/sql/async_hosts.sql` | KEEP node-backed host queries (`async_jobs` :2 comment only). |
| P2 | `internal/db/async_hosts.sql.go` | KEEP host query API (`async_jobs` :23 comment only). |
| P2 | `internal/db/sql/messages.sql` | KEEP message queries (`async_jobs` :152 comment only). |
| P2 | `internal/db/sql/session_drivers.sql` | KEEP driver API (`AsyncJobStore` :25 comment only). |
| P2 | `internal/db/messages.sql.go` | KEEP generated messages API (`async_jobs` :245 comment only). |
| P2 | `internal/db/session_drivers.sql.go` | KEEP generated driver API (`AsyncJobStore` :34 comment only). |
| P2 | `internal/db/store_v1_schema_test.go` | KEEP fresh-store assertion (`async_jobs` :218). |
| P2 | `internal/db/testdata/r12a_drop_async_jobs_testonly/20991231000001_r12a_drop_async_jobs_testonly.sql` | DELETE obsolete test-only migration fixture. |
| P3 | `internal/db/storepath/storepath.go` | REWRITE active filename to store-v1; retain legacy filename and lock/path helpers (`ActiveFilename = LegacyFilename :8). |
| P3 | `internal/config/config.go` | REWRITE selector use in tool inventory (`job_output` :434). |
| P3 | `internal/config/config_mcp.go` | REWRITE stale job_output wait wording (`job_output` :296). |
| P7 | `internal/app/app_batch_held_answer_wiring_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :24). |
| P7 | `internal/app/app_batch_child_reaction_scope_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :31). |
| P7 | `internal/app/batch_timeout_wake_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :63). |
| P7 | `internal/app/app_run_quota_held_question_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :20). |
| P7 | `internal/app/app_run_continuation_fence_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :178). |
| P7 | `internal/app/batch_completion_wake_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :201). |
| P7 | `internal/app/app_batch_provider_crash_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :108). |
| P7 | `internal/app/app_batch_finished_work_export_test.go` | REWRITE false/true selectors (`RUSH_BATCH_RUNTIME` :23). |
| P7 | `internal/app/app_run_batch_heartbeat_presentation_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :34). |
| P7 | `internal/app/batch_shutdown_acceptance_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :311). |
| P7 | `internal/app/batch_facts_binding_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :27). |
| P7 | `internal/app/app_new_error_release_test.go` | KEEP failed-New cleanup (`RUSH_BATCH_RUNTIME` :97). |
| P7 | `internal/app/batch_timeout_checkin_install_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :26). |
| P7 | `internal/app/batch_reviewer_scope_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :67). |
| P7 | `internal/app/app_run_construction_leak_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :119). |
| P7 | `internal/app/app_ephemeral_disk_binding_wire_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :62). |
| P7 | `internal/app/app_ephemeral_disk_binding.go` | KEEP both functions (`resolveDiskBindings` :5, `newEphemeralDiskBindings` :19: they serve library-mode Apps without a persisted batch install); the file has no flag-reading code, only the doc comment (:5-8) that says "with RUSH_BATCH_RUNTIME ON" - reword that comment in H5/R12-I when the switch is removed. |
| P7 | `internal/app/app_run_cli_drain_ctrlc_test.go` | REWRITE old-store notice check (`asyncJobStore` :134). |
| P7 | `internal/app/async_scenarios_cancel_test.go` | REWRITE old asyncTool assumption; retain cancellation scenario; see section 6 item 21. |
| P7 | `internal/app/batch_facts_app_run_test.go` | REWRITE opt-in (`RUSH_BATCH_RUNTIME` :86). |
| P8 | `internal/cmd/sessions_why_test.go` | REWRITE old/false cases (`RUSH_BATCH_RUNTIME` :89). |
| P9 | `internal/server/handlers_batch_test.go` | REWRITE empty/OFF selector case (`RUSH_BATCH_RUNTIME` :210). |
| P9 | `internal/server/batch_rerun_notices_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :61). |
| P9 | `internal/server/descendant_work_annotation_reads_on_test.go` | REWRITE flag setup (`RUSH_BATCH_RUNTIME` :299). |
| P9 | `internal/server/rerun_batch_fx_test.go` | REWRITE flag-dependent fixture (`RUSH_BATCH_RUNTIME` :3). |
| P5 | `internal/agent/coordinator_batch_stop_terminal_test.go` | REWRITE opt-in setup (`RUSH_BATCH_RUNTIME` :15). |
| P7 | `internal/app/app_batch_driver_status_test.go` | REWRITE opt-in setup; retain driver status scenarios (`RUSH_BATCH_RUNTIME :19). |
| P12 | `docs/plans/2026-10-07-job-batches-design.md` | KEEP historical design (`async_jobs` :2188). |
| P12 | `docs/plans/2026-10-12-job-batches-rush-integration.md` | KEEP historical integration plan (`RUSH_BATCH_RUNTIME` :24). |
| P12 | `docs/research/2026-10-14-r12-cutover-decomposition.md` | KEEP historical decomposition research. |
| P12 | `docs/research/2026-10-14-r12-flag-on-failures.md` | KEEP historical flag-on failure research. |
| P12 | `docs/research/2026-10-14-r12-old-path-manifest.md` | KEEP historical inventory (`async_jobs` :11). |
| P12 | `docs/research/2026-10-15-r12h-fresh-store-and-sdk-provider.md` | KEEP historical fresh-store research. |
| H5-W | `web/src/batchWork.ts` | REWRITE stale flag-off comment (`RUSH_BATCH_RUNTIME` :13). |
| P9 | `internal/server/web_composer_history_test.go` | KEEP imported transcript presentation; H5-W owns UI retirement. |
| H5-W | `web/src/useWS.ts` | REWRITE stale flag-off comment (`RUSH_BATCH_RUNTIME` :469). |
| P3 | `internal/config/datadir.go` | KEEP dual-file old-directory hints. |
| P3 | `internal/config/datadir_test.go` | KEEP legacy-local and dual-filename detection. |
| P3 | `internal/config/batch_runtime_flag.go` | H4 unconditional helper (`RUSH_BATCH_RUNTIME` :15); P3 later erases. |
| P3 | `internal/config/batch_runtime_flag_test.go` | H4 rewrite all-values batch acceptance; P3 later removes parser-only tests. |
| P2 | `internal/db/testdata/store_v1/README.md` | REWRITE stale `ListNewFiles` :1 promise. |
| P7 | `internal/app/app.go` | REWRITE old store setup (`AsyncJobStore` :110). |
| P7 | `internal/app/app_accessors.go` | REWRITE old store accessor (`AsyncJobStore` :50). |
| P7 | `internal/app/app_agent_setup.go` | REWRITE old store construction (`NewAsyncJobStore` :151). |
| P7 | `internal/app/app_batch_cli.go` | REWRITE old store precondition/host access; retain batch CLI (`asyncJobStore :147). |
| P7 | `internal/app/app_batch_cutover_driver_test.go` | KEEP driver acceptance (`RUSH_BATCH_RUNTIME` :37). |
| P7 | `internal/app/app_batch_cutover_facts.go` | REWRITE selector/reader comments (`RUSH_BATCH_RUNTIME` :1). |
| P7 | `internal/app/app_batch_cutover_hosts.go` | REWRITE obsolete condition (`RUSH_BATCH_RUNTIME` :1). |
| P7 | `internal/app/app_batch_cutover_hosts_test.go` | REWRITE selectors/store fixtures (`RUSH_BATCH_RUNTIME` :49). |
| P7 | `internal/app/app_batch_cutover_test.go` | REWRITE OFF/old-store cases (`RUSH_BATCH_RUNTIME` :122). |
| P7 | `internal/app/app_batch_doc.go` | REWRITE stale migration instructions (`RUSH_BATCH_RUNTIME` :70). |
| P7 | `internal/app/app_batch_install.go` | REWRITE old host branches/guard (`RUSH_BATCH_RUNTIME` :2). |
| P7 | `internal/app/app_batch_install_test.go` | REWRITE selector/no-install tests (`RUSH_BATCH_RUNTIME` :87). |
| P7 | `internal/app/app_batch_notice_result_e2e_test.go` | KEEP batch notice scenario. |
| P7 | `internal/app/app_batch_runtime.go` | REWRITE selector to unconditional install (`AsyncJobStore.Close` :61). |
| P7 | `internal/app/app_forced_shutdown_host_test.go` | REWRITE old host fixture (`AsyncJobStore.CloseKeepLock` :3). |
| P7 | `internal/app/app_lifecycle.go` | REWRITE old store teardown (`AsyncJobStore` :306). |
| P7 | `internal/app/app_new_error_release.go` | KEEP failed-New cleanup (`AsyncJobStore.Close` :59). |
| P7 | `internal/app/app_run_a15_a18_a10_test.go` | REWRITE dual-mode outcome (`async_jobs` :55). |
| P7 | `internal/app/app_run_async_completion_test.go` | REWRITE source selector (`BatchRuntimeEnabled` :28). |
| P7 | `internal/app/app_run_async_wait.go` | REWRITE old store gate (`AsyncJobStore` :73). |
| P7 | `internal/app/app_run_await_tasks_test.go` | REWRITE dual-mode helper (`RUSH_BATCH_RUNTIME` :23). |
| P7 | `internal/app/app_run_billing_test.go` | REWRITE OFF old-row premise (`async_jobs` :24). |
| P7 | `internal/app/app_run_chain_guard_e2e_test.go` | REWRITE dual-mode premise (`async_jobs` :280). |
| P7 | `internal/app/app_run_cost_batch_routing_test.go` | KEEP batch routing (`job_kill` :64). |
| P7 | `internal/app/app_run_deferred_debt_test.go` | REWRITE old debt carrier; retain deferred debt (`a.asyncJobStore.InsertSessionNotice :35). |
| P7 | `internal/app/app_run_e3_helpers_test.go` | REWRITE old-row seed (`async_jobs` :62). |
| P7 | `internal/app/app_run_ephemeral_disk_binding_test.go` | KEEP SDK binding (`RUSH_BATCH_RUNTIME` :67). |
| P7 | `internal/app/app_run_job_kill_by_id_test.go` | DELETE old model tool scenario (`job_kill` :78). |
| P7 | `internal/app/app_run_legacy_policy_epoch_test.go` | REWRITE flag branches (`BatchRuntimeEnabled` :71). |
| P7 | `internal/app/app_run_loop_more_test.go` | REWRITE old output expectation (`job_kill` :249). |
| P7 | `internal/app/app_run_loop_test.go` | REWRITE legacy completion fixture; retain run loop (`asyncJobStore.Claim :356). |
| P7 | `internal/app/app_run_quota.go` | REWRITE OFF walk (`async_jobs` :237). |
| P7 | `internal/app/app_run_quota_child_test.go` | REWRITE old reader (`async_jobs` :201). |
| P7 | `internal/app/app_run_quota_test.go` | REWRITE old-store fixture/reads; retain quota tests (`asyncJobStore.Claim :78). |
| P7 | `internal/app/app_run_reviewer_evidence.go` | REWRITE completion evidence carrier to neutral type; see section 6 item 22. |
| P7 | `internal/app/app_run_reviewer_evidence_async_real_test.go` | REWRITE legacy authority fixture; retain actual batch/physical evidence (`agent.FormatAsyncCompletion :65). |
| P7 | `internal/app/app_run_reviewer_evidence_async_test.go` | REWRITE dual mode; retain relevant acceptance (`agent.FormatAsyncCompletion :17). |
| P7 | `internal/app/app_run_reviewer_readonly_test.go` | REWRITE removed tool inventory (`job_kill` :93). |
| P7 | `internal/app/app_run_round4_misc_test.go` | REWRITE selector assertion (`BatchRuntimeEnabled` :31). |
| P7 | `internal/app/app_run_router_batch_test.go` | REWRITE old tool label (`job_kill` :305). |
| P7 | `internal/app/app_run_six_completion_routing_test.go` | REWRITE old carrier; retain routing; see section 6 item 23. |
| P7 | `internal/app/app_run_subagent_root_wait_test.go` | DELETE old tool transcript (`job_kill` :160). |
| P7 | `internal/app/app_run_supervision_test.go` | REWRITE ledger harness; preserve SV lifecycle; see section 6 item 24. |
| P7 | `internal/app/app_run_timeout_test.go` | REWRITE old claim fixture; preserve timeout contract (`asyncJobStore.Claim :48). |
| P7 | `internal/app/app_run_wait_batch_fixture_test.go` | REWRITE dual-mode helper; retain batch wait/timeout; see section 6 item 25. |
| P7 | `internal/app/app_run_wait_heartbeat_test.go` | REWRITE old claim/store fixture; retain heartbeat (`q.ClaimAsyncJob :83). |
| P7 | `internal/app/app_run_setup.go` | KEEP generic turn-stall policy; no detach deletion. |
| P7 | `internal/app/app_session_activity.go` | REWRITE OFF reader (`AsyncJobStore` :84). |
| P7 | `internal/app/app_session_activity_test.go` | REWRITE old fixtures (`NewAsyncJobStore` :73). |
| P7 | `internal/app/app_session_drivers.go` | REWRITE old-store fallback (`AsyncJobStore` :24). |
| P7 | `internal/app/app_session_drivers_test.go` | REWRITE fixtures (`NewAsyncJobStore` :31). |
| P7 | `internal/app/async_phase4_batch_helpers_test.go` | REWRITE old helpers (`RUSH_BATCH_RUNTIME` :3). |
| P7 | `internal/app/async_phase4_driver_scenarios_test.go` | REWRITE OFF reader (`async_jobs` :14). |
| P7 | `internal/app/async_phase4_recovery_scenarios_test.go` | REWRITE false/dual fixture (`RUSH_BATCH_RUNTIME` :18). |
| P7 | `internal/app/async_phase4_recovery_unannounced_test.go` | REWRITE OFF claim scenario (`async_jobs` :10). |
| P7 | `internal/app/batch_fixture_port_f6_test.go` | REWRITE legacy fixture branch (`RUSH_BATCH_RUNTIME` :5). |
| P7 | `internal/app/batch_fixture_port_f6a2_test.go` | REWRITE old claim/readback (`RUSH_BATCH_RUNTIME` :5). |
| P7 | `internal/app/batch_fixture_port_f6a3_test.go` | REWRITE OFF debt predicate (`async_jobs` :52). |
| P7 | `internal/app/batch_install_coordinator_test.go` | REWRITE explicit false installer (`RUSH_BATCH_RUNTIME` :16). |
| P7 | `internal/app/batch_install_factory.go` | REWRITE old store host dependency; preserve HostStore (`a.asyncJobStore :57). |
| P7 | `internal/app/batch_install_factory_test.go` | REWRITE old store setup (`NewAsyncJobStore` :24). |
| P8 | `internal/cmd/batch_cli.go` | REWRITE disabled guard (`BatchRuntimeEnabled` :40). |
| P8 | `internal/cmd/batch_cli_runtime_test.go` | REWRITE selectors (`NewAsyncJobStore` :238). |
| P8 | `internal/cmd/batch_cli_test.go` | KEEP CLI behavior. |
| P8 | `internal/cmd/batch_reset_recovery_test.go` | KEEP batch reset recovery. |
| P8 | `internal/cmd/claude_wrush_command.md` | REWRITE old controls in help/prose (`async_jobs` :47). |
| P8 | `internal/cmd/sessions_activity_cmd_test.go` | REWRITE old fixture (`AsyncJobStore` :46). |
| P8 | `internal/cmd/sessions_audit.go` | REWRITE old absence JSON/query rules (`job_output` :61). |
| P8 | `internal/cmd/sessions_audit_db.go` | REWRITE hardcoded old DB path (`BatchRuntimeEnabled` :98). |
| P8 | `internal/cmd/sessions_audit_findings.go` | REWRITE obsolete authority finding (`async_jobs` :43). |
| P8 | `internal/cmd/sessions_audit_rules.go` | REWRITE old-table absence rule (`job_output` :47). |
| P8 | `internal/cmd/sessions_audit_rules_test.go` | REWRITE direct old-table inserts (`async_jobs` :52). |
| P8 | `internal/cmd/sessions_audit_test.go` | REWRITE direct old-table seed (`async_jobs` :50). |
| P8 | `internal/cmd/sessions_batch_act.go` | KEEP batch action. |
| P8 | `internal/cmd/sessions_batch_act_test.go` | KEEP batch action tests. |
| P8 | `internal/cmd/sessions_batch_audit.go` | KEEP batch audit. |
| P8 | `internal/cmd/sessions_batch_audit_test.go` | KEEP batch audit tests. |
| P8 | `internal/cmd/sessions_batch_read.go` | KEEP batch read. |
| P8 | `internal/cmd/sessions_batch_read_test.go` | KEEP batch read tests. |
| P8 | `internal/cmd/sessions_batch_session_facts_test.go` | REWRITE mixed carrier (`RUSH_BATCH_RUNTIME` :18). |
| P8 | `internal/cmd/sessions_cancel_test.go` | REWRITE cancellation fixture (`AsyncJobStore` :47). |
| P8 | `internal/cmd/sessions_compact.go` | REWRITE hardcoded old DB path (`AsyncJobStore` :58). |
| P8 | `internal/cmd/sessions_gc.go` | REWRITE old query path (`async_jobs` :27). |
| P8 | `internal/cmd/sessions_gc_jobs_test.go` | DELETE old-table fixtures (`async_jobs` :69). |
| P8 | `internal/cmd/sessions_help_text_test.go` | REWRITE obsolete query wording (`async_jobs` :5). |
| P8 | `internal/cmd/sessions_inject_livework_test.go` | REWRITE legacy fixture (`AsyncJobStore` :47). |
| P8 | `internal/cmd/sessions_jobs.go` | REWRITE query implementation (`async_jobs` :37). |
| P8 | `internal/cmd/sessions_jobs_test.go` | REWRITE old claims (`async_jobs` :67). |
| P8 | `internal/cmd/sessions_list.go` | REWRITE old-store liveness fallback (`async_jobs` :73). |
| P8 | `internal/cmd/sessions_list_awaiting_test.go` | REWRITE flag branches (`NewAsyncJobStore` :38). |
| P8 | `internal/cmd/sessions_list_test.go` | REWRITE old delegation claims (`async_jobs` :257). |
| P8 | `internal/cmd/sessions_own_job_status_test.go` | H4 translate status (`AsyncJobStore` :47). |
| P8 | `internal/cmd/sessions_reset.go` | REWRITE flag branch (`BatchRuntimeEnabled` :107). |
| P8 | `internal/cmd/sessions_reset_async_test.go` | H4 translate reset semantics (`async_jobs` :3). |
| P8 | `internal/cmd/sessions_run_driver_status_test.go` | REWRITE false old status cases (`AsyncJobStore` :27). |
| P8 | `internal/cmd/sessions_show.go` | REWRITE old SQL (`AsyncJobsTotal` :138). |
| P8 | `internal/cmd/sessions_show_jobs_test.go` | REWRITE batch-only status (`AsyncJobStore` :42). |
| P8 | `internal/cmd/sessions_why.go` | REWRITE old query (`async_jobs` :51). |
| P8 | `internal/cmd/sessions_why_awaiting_test.go` | REWRITE explicit false cases (`AsyncJobStore` :19). |
| P8 | `internal/cmd/sessions_why_descendant_test.go` | REWRITE false/old-row cases (`async_jobs` :9). |
| P9 | `internal/server/descendant_work_batch_on_test.go` | KEEP batch behavior (`RUSH_BATCH_RUNTIME` :93). |
| P9 | `internal/server/descendant_work_batch_test.go` | KEEP batch behavior (`AsyncJobStore` :58). |
| P9 | `internal/server/batch_child_bind_walk_test.go` | REWRITE opt-in setup (`RUSH_BATCH_RUNTIME` :18). |
| P9 | `internal/server/descendant_work_liveness_test.go` | REWRITE old-table premise (`async_jobs` :5). |
| P9 | `internal/server/handlers_agent_rerun_cutover_test.go` | REWRITE false legacy rerun branch (`RUSH_BATCH_RUNTIME` :73). |
| P9 | `internal/server/handlers_agent_rerun.go` | REWRITE old rerun/truncation (`async_jobs` :305). |
| P9 | `internal/server/handlers_batch.go` | REWRITE selector to unconditional handler (`async_jobs` :5). |
| P9 | `internal/server/handlers_batch_doc.go` | REWRITE old path documentation (`RUSH_BATCH_RUNTIME` :7). |
| P9 | `internal/server/handlers_livework.go` | REWRITE source to batch projection (`async_jobs` :4). |
| P9 | `internal/server/handlers_livework_awaiting_test.go` | REWRITE old fixtures (`AsyncJobStore` :31). |
| P9 | `internal/server/handlers_livework_batch.go` | KEEP H4 flat projection (`async_jobs` :6 comment only). |
| P9 | `internal/server/handlers_livework_batch_test.go` | KEEP batch scenarios (`RUSH_BATCH_RUNTIME` :62). |
| P9 | `internal/server/handlers_livework_test.go` | REWRITE old SQL fixtures (`async_jobs` :463). |
| P9 | `internal/server/handlers_sessions_batch_cutover_test.go` | KEEP batch projection (`RUSH_BATCH_RUNTIME` :81). |
| P9 | `internal/server/livework_seed_test.go` | REWRITE old seed helper (`AsyncJobStore` :73). |
| P9 | `internal/server/p630_rerun_cancel_window_test.go` | KEEP batch fence (`AsyncJobStore` :100). |
| P9 | `internal/server/protocol.go` | KEEP H4 projection schema (`AsyncJobStore` :48 comment only). |
| P9 | `internal/server/p1_6_regression_test.go` | KEEP regression; translate fixture only if old-store dependent. |
| P9 | `internal/server/p2_regression_test.go` | KEEP regression; translate fixture only if old-store dependent. |
| P9 | `internal/server/p595_delete_streaming_test.go` | KEEP streaming deletion regression. |
| P9 | `internal/server/p614_rerun_reservation_test.go` | REWRITE reservation fixture to batch (`session.VoidedAsyncJob :55). |
| P9 | `internal/server/p623_panic_window_test.go` | KEEP panic-window regression; migrate old fixture if present. |
| P9 | `internal/server/readpump_control_starvation_test.go` | KEEP read-pump control regression. |
| P9 | `internal/server/rerun_hold_test.go` | REWRITE old rerun hold fixture to batch (`batchRuntimeEnabled :205). |
| P9 | `internal/server/run_driver_liveness_test.go` | REWRITE old driver fixture (`AsyncJobStore` :44). |
| P10 | `sdk/README.md` | KEEP SDK API/readme; update current default semantics. |
| P10 | `sdk/sdk_batch_cutover_test.go` | REWRITE opt-in (`RUSH_BATCH_RUNTIME` :35). |
| P10 | `sdk/sdk_provider_run_shapes_test.go` | REWRITE runtime env setup (`RUSH_BATCH_RUNTIME` :141). |
| H5-W | `web/src/asyncJobCompletion.ts` | KEEP historical completion read-only through H4. |
| H5-W | `web/src/asyncJobMetadata.test.ts` | KEEP historical metadata coverage pending compatibility decision. |
| H5-W | `web/src/completionIndex.ts` | KEEP historical transcript indexing through H4. |
| H5-W | `web/src/completionIndex.test.ts` | KEEP historical rendering through H4. |
| H5-W | `web/src/components/Chat.tsx` | KEEP historical display compatibility through H4. |
| H5-W | `web/src/components/LiveWorkList.tsx` | KEEP batch panel; remove old projection in H5-W. |
| H5-W | `web/src/components/Message/ActionRow.tsx` | KEEP historical badge until H5-W. |
| H5-W | `web/src/components/Message/Message.tsx` | KEEP historical rendering until H5-W. |
| H5-W | `web/src/components/Message/ToolActivityGroup.tsx` | KEEP historical completion rendering until H5-W. |
| H5-W | `web/src/components/SubAgentBlock.tsx` | KEEP imported completion rendering pending approval. |
| H5-W | `web/src/store.ts` | KEEP compatibility state through H4. |
| H5-W | `web/src/types.ts` | KEEP old wire types through H4. |
| H5-W | `web/tests/async-outcome-badges.spec.ts` | KEEP historical rendering through H4; H5-W changes with signoff. |
| H5-W | `web/tests/batch-structured-completions.spec.ts` | KEEP batch UI tests. |
| H5-W | `web/tests/composer-history-filter.spec.ts` | KEEP history filter. |
| H5-W | `web/tests/live-work-panel.spec.ts` | KEEP live batch panel. |
| H5-W | `web/tests/wake-schedules.spec.ts` | KEEP wake schedules. |
| P11 | `.github/workflows/job-batches-shadow.yml` | REWRITE shadow selector/gates; preserve pinned baseline isolation (`RUSH_BATCH_RUNTIME :34). |
| P11 | `.github/workflows/build.yml` | KEEP normal CI/sqlc job; adjust acceptance routing only if needed. |
| P11 | `internal/app/app_batch_bench_helpers_test.go` | KEEP benchmark fixture in pinned baseline only. |
| P11 | `internal/app/app_batch_bench_observer_test.go` | KEEP baseline-only observer. |
| P11 | `internal/app/app_batch_bench_test.go` | KEEP pinned baseline benchmark. |
| P12 | `docs/jobbatch.md` | REWRITE current controls/default semantics (`internal/jobbatch/ :14). |
| P12 | `docs/jobbatch-acceptance.md` | REWRITE current acceptance matrix; retain historical gates (`go test :23). |
| P10 | `testdata/embedder/embed_test.go` | KEEP public embedder contract. |
| P10 | `testdata/embedder/go.mod` | KEEP external module metadata. |
| P10 | `testdata/embedder/go.sum` | KEEP external dependency checksums. |
| P10 | `testdata/embedder/testdata/negativeimport/negative.go` | KEEP negative-import fixture. |
| P12 | `README.md` | REWRITE current default/import/rollback guidance; preserve history (`job_output :271). |
| P12 | `CHANGELOG.md` | REWRITE current release note; preserve historical entries (`job_output :56). |
| P12 | `docs/research/2026-10-12-job-batches-rush-map-a.md` | KEEP historical research map. |
| P12 | `docs/research/2026-10-12-job-batches-rush-map-b.md` | KEEP historical research map. |
| P12 | `docs/research/2026-10-12-job-batches-rush-map-c.md` | KEEP historical research map. |
| P12 | `docs/plans/2026-09-02-sdk-scoped-file-tools-plan.md` | KEEP historical plan. |
| P12 | `docs/plans/2026-09-24-agent-wakes-and-async-job-control.md` | KEEP historical plan. |
| P12 | `docs/plans/2026-09-27-async-phase1-spec.md` | KEEP historical spec. |
| P12 | `docs/plans/2026-09-27-async-phase2-spec.md` | KEEP historical spec. |
| P12 | `docs/plans/2026-09-27-async-structured-concurrency.md` | KEEP historical design. |
| P12 | `docs/plans/2026-09-27-wake-tools-contract.md` | KEEP historical contract. |
| P12 | `docs/plans/2026-09-28-async-phase3-spec.md` | KEEP historical spec. |
| P12 | `docs/plans/2026-09-28-async-phase4-durable-core.md` | KEEP historical design. |
| P12 | `docs/plans/2026-09-28-async-phase4-spec.md` | KEEP historical spec. |
| P12 | `docs/plans/2026-10-01-bg-shell-ledger.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-01-in-turn-progress-guard.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-01-inline-window.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-01-reaction-chain-guard.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-01-shared-data-dir.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-01-structural-refactor.md` | KEEP historical plan. |
| P12 | `docs/plans/2026-10-01-turn-arbiter.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-02-async-job-cap.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-02-reviewer-pass-verification.md` | KEEP historical review plan. |
| P12 | `docs/plans/2026-10-02-worker-question-wake.md` | KEEP historical design. |
| P12 | `docs/plans/2026-10-05-async-phase4-rev8-map.md` | KEEP historical map. |
| P12 | `docs/plans/2026-10-05-bg-shell-remainder.md` | KEEP historical plan. |
| P12 | `docs/async-invariants.md` | KEEP historical invariant record. |
| P12 | `docs/checkpoints/2026-06-25-2218.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-06-26-0831.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-07-19-0729.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-07-30-1400.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-09-28-0029.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-09-28-1711.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-09-29-0925.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-09-30-0810.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-09-30-2008.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-01-1351.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-01-2114.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-02-0510.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-05-0755.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-05-1331.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-05-2020.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-06-0530.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-06-1334.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-06-1834.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-07-0554.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-11-2215.md` | KEEP historical checkpoint. |
| P12 | `docs/checkpoints/2026-10-12-0734.md` | KEEP historical checkpoint. |
| P12 | `docs/design/event-loop-turn.md` | KEEP historical design. |
| P12 | `docs/reviews/2026-09-04-sdk-library-review-round-16-1209.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-05-2047-commit-review-36h.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-29-async-phase4-round1-rerun-design.md` | KEEP historical review/design. |
| P12 | `docs/reviews/2026-09-29-async-phase4-round1.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md` | KEEP historical review/design. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round2.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round3.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round4.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round5.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round6.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round7.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-09-30-async-phase4-round8.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-05-async-round9.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-08-job-batches-xxs-rush-integration-round2.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-18-flag-on-port-ui.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-18-flag-on-shadow-triage.md` | KEEP historical review. |
| P12 | `docs/reviews/2026-10-19-h4-prerequisites-review.md` | KEEP prerequisite evidence; not proof gates passed. |
| H5-C | `tools/r12a_old_path_manifest.py` | H5-C owner for historical generator; do not run. |

## 2. Coupling findings

| Coupling | Status | Observed mechanism / required disposition |
|---|---|---|
| Stop writes `async_jobs` ON | BLOCKS-DELETION | `coordinator_interrupt.go:148-154` invokes legacy wake-zero SQL; batch wake-zero at `:157-164` is separate. |
| Stop reads/cancels ledger ON | BLOCKS-DELETION | `coordinator_interrupt.go:93-120,172-175`; tree IDs/cancel/recheck use ledger. |
| SV supervision depends on ledger | BLOCKS-DELETION | `batch_supervision_arming.go:39-48`; `supervision.go:156,306`. |
| Supervision notices use old store | BLOCKS-DELETION | `supervision.go:418-425`; rebind to SessionNoticeStore. |
| Hints/external-driver state in ledger | BLOCKS-DELETION | `work_ledger_reaction.go:21-102`; wake calls in `coordinator_wake.go:28-41`. |
| Batch arbiter takes ledger | BLOCKS-DELETION | `turn_arbiter.go:424-438,487`; `batch_cutover_facts.go:483,522`. |
| Foreign driver helper uses old store | BLOCKS-DELETION | `work_ledger_driver.go:9-14`; use retained driver facts. |
| Notice pull guard/progress uses old store | BLOCKS-DELETION | `agent_notice_pull.go:22-46,79-98`. |
| Reaction write falls back to legacy SQL | BLOCKS-DELETION | `agent_turn_step.go:845-859`; batch-only fail closed. |
| Own-work guard routes through ledger | BLOCKS-DELETION | `agent_own_work_batch.go:25-39`; caller `agent_turn_step.go:330-333`. |
| Agent controls read ledger maps | BLOCKS-DELETION | `coordinator_agent_control.go:46-85,159-177,309-316`. |
| Legacy question fallback is ON | BLOCKS-DELETION | `cli_active_work.go:336-364`. |
| Ledger-backed batch adapter | BLOCKS-DELETION | `batch_cutover_facts.go:233-303,322-339`. |
| Await-task old guard/OFF route | BLOCKS-DELETION | `coordinator_await_tasks.go:51,80`. |
| Turn selector/facts | BLOCKS-DELETION | `agent_turn.go:373-382,411`; `agent_turn_step.go:449`. |
| Batch fact selector and old live-work projection | BLOCKS-DELETION | `batch_cutover_facts.go:222,233-303,322-339`; `session/async_job_reader_batch.go:23-96`. |
| Physical shell completion/output; AsyncCompletion formatting | KEEP after carrier extraction | Physical lifecycle remains; AsyncCompletion currently `work_ledger.go:27`. |
| `session_notices.job_tool_call_id`, SessionNoticeStore | KEEP | Batch-bound identity; remove obsolete Repoint query only. |
| AsyncDataRetentionAge, foreignLiveDriverFor, host locks, watchdog policy | KEEP | Retain named declarations; remove old receivers/actions only. |

## 3. Test and fixture disposition

Flag-OFF/parser cases become all-values-batch tests at H4; selector parser removal belongs to P3. Prompt expectations in `internal/agent/prompt/prompt_await_tasks_test.go:75`, `prompt_scoped_test.go:55,149`, `batch_model_flag_test.go:15`, and `batch_toolset_inventory_test.go:136,165,196,230,280` are rewritten while retaining policy assertions. Legacy-only timeout, silent-job, and held-ledger cases in `work_ledger_timeout_test.go:162,216,321`, `supervision_silence_test.go:126-313`, `delegation_question_test.go:515,548`, and `supervision_test.go:276` are deleted or translated; presentation reductions need operator signoff. Genuine unbound/fail-closed cases in `batch_cutover_facts_test.go:319`, `batch_gate_refusal_test.go:86`, and `drain_epoch_test.go:116` remain.

App no-install/OFF fixtures in `app_batch_install_test.go:87`, `batch_install_coordinator_test.go:16`, `app_batch_cutover_test.go:122,171,205,284`, `app_new_error_release_test.go:154`, `app_batch_cutover_hosts_test.go:971`, and `app_batch_finished_work_export_test.go:23,55,115,168` are ported to explicit construction while preserving failure, export, and batch scenarios. CLI legacy scenarios in `sessions_why_test.go`, `sessions_why_awaiting_test.go`, `sessions_why_descendant_test.go`, `sessions_run_driver_status_test.go`, `sessions_reset_async_test.go`, and `sessions_own_job_status_test.go` are translated to batch/HostStore facts or old-row-only assertions deleted. `internal/server/descendant_work_liveness_test.go`; 

Shared dual-mode fixture files are permanently owned by their package lane: root agent fixtures P5; App fixtures P7; SQL/import source fixtures P1/P2; benchmark files P11. P0 does not authorize overlapping edits. The pinned baseline keeps its benchmark-only legacy setup; candidate product code has no OFF authority. H4 must remove any test reference blocking removal of AsyncJob, AsyncJobStore, workLedger, or old tools. H5 receives only dependency-free residual JSON/prose/web cleanup.

Importer CI acceptance: Connect imports once before destination open and reopen preserves later writes; missing source/destination initializes one new schema; existing target never overwritten; MayMigrate(false) does not publish; Alive and Unknown hosts refuse; crash before rename leaves no partial published store; retries succeed; source DB/WAL/SHM bytes remain unchanged; copied/normalized values are explicit; old/preparation execution state is absent; frozen source migrations work after active migration replacement; readonly audit imports nothing; integrity/foreign-key/message/timestamp checks pass. The external drain test is not itself startup wiring acceptance.

External drain scenario (P1, `internal/session/legacy_import_drain_external_test.go`, `TestLegacyImportNormalizedProcessingOutboxIsDrainedEndToEnd` :39): the fixture builds the legacy store over the frozen legacy chain — in the jobbatch-pim worktree via `fs.Sub` over the `legacymigrations` embed with `goose.NewProvider` + `Up` (:45-52); this worktree applies the same frozen chain through `db.Migrate` (:48-49) — and seeds an `orphan_call_outbox` row stuck in the retired pre-P0-4 `processing` claim state (:56). The test asserts the import normalizes it to `pending` with the payload `{"x":1}` verbatim (:78-79), then exercises a real `DrainOrphanOutboxEntry` drain into `session_run_queue` with `call_data` unchanged and the outbox row deleted (:85-97), and a second drain of the same id reports false (idempotent, :100-102). H4 does not delete it: it is the only end-to-end, production-API-only proof that legacy `processing` outbox state survives the import and is genuinely drained rather than stranded, which §3 importer acceptance ("old/preparation execution state is absent", "retries succeed") depends on; its rewrite is confined to the fixture's frozen Goose lineage.

## 4. Exact generated API inventory

This is a source-observed deletion checklist, not a generated-code patch. Names and generated const/type/method anchors were checked with targeted per-file grep. Const/type/Q line numbers in this table refer to `internal/db/async_jobs.sql.go`; Querier refers to `internal/db/querier.go`. P/C/F/T are `internal/db/db.go` Prepare, Close, struct field, WithTx-copy anchors. `—` means no named Params or Row type.

| Query | SQL const (generated line) | Params / Row (generated lines) | Q | Querier | stmt field | P / C / F / T | Retained consumers |
|---|---|---|---:|---:|---|---|---|
| RegisterAsyncHost | registerAsyncHost:1163 | RegisterAsyncHostParams:1169 / — | 1179 | 909 | registerAsyncHostStmt | 585 / 1661 / 2121 / 2360 | REBIND BEFORE DELETE (P4). The retained interface `AsyncHostStore` (`session/host_lock.go:214-217`) and its adapter `hostNodeStore` (`session/host_store.go:39-40`, which converts to `RegisterAsyncHostRowParams`) are typed with `db.RegisterAsyncHostParams`, a symbol that exists only because of `async_jobs.sql`; `async_hosts.sql` defines the differently named `RegisterAsyncHostRow` (:12), not a copy. P4 retypes `AsyncHostStore.RegisterAsyncHost` to `db.RegisterAsyncHostRowParams` and drops the conversion; only then is this symbol deleted. Test-only users to move to `RegisterAsyncHostRow` in their lanes: `app/app_run_batch_heartbeat_presentation_test.go:72`, `app/app_run_wait_heartbeat_test.go:74`, `cmd/sessions_gc_jobs_test.go:171`, `cmd/sessions_jobs_test.go:110,154,251`, `db/async_phase4_schema_test.go`. |
| GetAsyncHost | getAsyncHost:418 | — | 422 | 412 | getAsyncHostStmt | 279 / 1151 / 2019 / 2258 | REBIND BEFORE DELETE. Production users: `cmd/sessions_jobs.go:159` (P8), `app/app_run_async_wait.go:79` (P7), `session/async_job_reader.go:84-88` (`AsyncJobStore.GetAsyncHost`, dies with `AsyncJobStore`; P4 decides whether a display-only host read survives); replacement is `GetAsyncHostRow` (`async_hosts.sql:21`). Test users: `session/async_job_recovery_test.go`, `session/async_job_purge_empty_host_test.go:55`. |
| ListAsyncHosts | listAsyncHosts:606 | — | 613 | 596 | listAsyncHostsStmt | 417 / 1381 / 2065 / 2304 | REBIND BEFORE DELETE: test users `session/host_lock_test.go:221`, `session/async_job_recovery_concurrent_test.go:228`, `db/async_phase4_schema_test.go:341` move to `ListAsyncHostRows` (`async_hosts.sql:24`). |
| ListDistinctRecoverableHostIDs | listDistinctRecoverableHostIDs:732 | — | 745 | 652 | listDistinctRecoverableHostIDsStmt | 438 / 1416 / 2072 / 2311 | none. |
| DeleteTerminalUnannouncedAsyncJobsForHost | deleteTerminalUnannouncedAsyncJobsForHost:320 | — | 329 | 379 | deleteTerminalUnannouncedAsyncJobsForHostStmt | 261 / 1121 / 2013 / 2252 | none. |
| RependJobKillRowsWithoutNoticeForHost | rependJobKillRowsWithoutNoticeForHost:1268 | RependJobKillRowsWithoutNoticeForHostParams:1274 / — | 1284 | 961 | rependJobKillRowsWithoutNoticeForHostStmt | 603 / 1691 / 2127 / 2366 | none. |
| RependJobKillRowWithoutNotice | rependJobKillRowWithoutNotice:1240 | RependJobKillRowWithoutNoticeParams:1247 / — | 1260 | 955 | rependJobKillRowWithoutNoticeStmt | 600 / 1686 / 2126 / 2365 | P4 `notice_pull.go` call path; lane removes it; deleted. |
| DeleteAsyncHostIfNoJobs | deleteAsyncHostIfNoJobs:293 | — | 312 | 285 | deleteAsyncHostIfNoJobsStmt | 216 / 1046 / 1998 / 2237 | The generated symbol is used by `session/async_job_recovery.go:179,382` (legacy recovery, deleted by P4) and `db/async_phase4_schema_test.go:333,337`. SEPARATELY, `DeleteAsyncHostIfNoJobs` is also a METHOD NAME on the retained interface `AsyncHostStore` (`session/host_lock.go:216`, caller :397; spies at `session/host_lock_test.go:39,49`); `hostNodeStore` already routes it to `DeleteAsyncHostIfNoNodes` (`async_hosts.sql:28`). P4 keeps or renames the interface method; the generated query symbol is deleted. |
| ListAsyncHostsWithNoJobs | listAsyncHostsWithNoJobs:641 | — | 649 | 600 | listAsyncHostsWithNoJobsStmt | 420 / 1386 / 2066 / 2305 | used only by `session/async_job_recovery.go:368` (legacy recovery, deleted by P4); its batch twin `ListAsyncHostsWithNoNodes` (`async_hosts.sql:57`) stays. |
| ClaimAsyncJob | claimAsyncJob:140 | ClaimAsyncJobParams:152 / — | 187 | 195 | claimAsyncJobStmt | 162 / 956 / 1980 / 2219 | test-fixture call sites only (`batch_migration_test`, `host_lock_test`, `question_stop_format_test`, `coordinator_scope_recovery_test`, `sessions_jobs_test`, `descendant_work_batch_test`, `app_run_wait_heartbeat_test`, `async_phase4_schema_test`); owning lanes rebind fixtures to batch seeds; deleted. |
| GetAsyncJob | getAsyncJob:434 | GetAsyncJobParams:438 / — | 443 | 414 | getAsyncJobStmt | 285 / 1161 / 2021 / 2260 | P4 call sites `notice_pull.go:269` (old pull removed) and `batch_migration_test.go:50,72` (fixture rewritten); deleted. |
| GetRunningAsyncJobByChildSession | getRunningAsyncJobByChildSession:527 | — | 535 | 472 | getRunningAsyncJobByChildSessionStmt | 339 / 1251 / 2039 / 2278 | P4 `async_job_reaction.go:53`; lane rebinds to batch; deleted. |
| MarkAsyncJobAnnounced | markAsyncJobAnnounced:1012 | MarkAsyncJobAnnouncedParams:1017 / — | 1028 | 811 | markAsyncJobAnnouncedStmt | 540 / 1586 / 2106 / 2345 | P4 `notice_pull.go:352,471` announce paths removed by lane; deleted. |
| SetAsyncJobAnnounceMessageID | setAsyncJobAnnounceMessageID:1333 | SetAsyncJobAnnounceMessageIDParams:1338 / — | 1350 | 993 | setAsyncJobAnnounceMessageIDStmt | 621 / 1721 / 2133 / 2372 | P4 `notice_pull.go:363,482`; removed by lane; deleted. |
| DeleteUnannouncedAsyncJob | deleteUnannouncedAsyncJob:337 | DeleteUnannouncedAsyncJobParams:341 / — | 350 | 384 | deleteUnannouncedAsyncJobStmt | 264 / 1126 / 2014 / 2253 | none. |
| DeleteUnannouncedAsyncJobForClaim | deleteUnannouncedAsyncJobForClaim:358 | DeleteUnannouncedAsyncJobForClaimParams:362 / — | 373 | 390 | deleteUnannouncedAsyncJobForClaimStmt | 267 / 1131 / 2015 / 2254 | none. |
| TransitionAsyncJobTerminalPreserveVoid | transitionAsyncJobTerminalPreserveVoid:1504 | TransitionAsyncJobTerminalPreserveVoidParams:1513 / — | 1556 | 1146 | transitionAsyncJobTerminalPreserveVoidStmt | 666 / 1796 / 2148 / 2387 | P2 `async_phase4_schema_test.go` fixture; rewritten by lane; deleted. |
| RependAsyncJobsByNoticeMessageIDs | rependAsyncJobsByNoticeMessageIDs:1196 | RependAsyncJobsByNoticeMessageIDsParams:1201 / — | 1220 | 947 | rependAsyncJobsByNoticeMessageIDsStmt | 597 / 1681 / 2125 / 2364 | `session_notices.sql` references are comments only; no code consumer; deleted. |
| VoidAsyncJobsByAnnounceMessageIDs | voidAsyncJobsByAnnounceMessageIDs:1635 | VoidAsyncJobsByAnnounceMessageIDsParams:1641 / VoidAsyncJobsByAnnounceMessageIDsRow:1647 | 1665 | 1237 | voidAsyncJobsByAnnounceMessageIDsStmt | 705 / 1861 / 2161 / 2400 | none. |
| VoidAsyncJobsByToolCallIDs | voidAsyncJobsByToolCallIDs:1705 | VoidAsyncJobsByToolCallIDsParams:1711 / VoidAsyncJobsByToolCallIDsRow:1717 | 1728 | 1242 | voidAsyncJobsByToolCallIDsStmt | 708 / 1866 / 2162 / 2401 | none. |
| ListPendingAsyncJobNoticesForOwner | listPendingAsyncJobNoticesForOwner:768 | — | 776 | 735 | listPendingAsyncJobNoticesForOwnerStmt | 477 / 1481 / 2085 / 2324 | P4 `notice_pull.go:110`; removed by lane; deleted. |
| PullPendingAsyncJobNotice | pullPendingAsyncJobNotice:1059 | PullPendingAsyncJobNoticeParams:1066 / — | 1078 | 861 | pullPendingAsyncJobNoticeStmt | 564 / 1626 / 2114 / 2353 | P4 `notice_pull.go`/P5 `drain_debt_edge_cases_test.go` fixture; removed/rewritten by lanes; deleted. |
| SetAsyncJobNoticeMessageID | setAsyncJobNoticeMessageID:1363 | SetAsyncJobNoticeMessageIDParams:1368 / — | 1376 | 995 | setAsyncJobNoticeMessageIDStmt | 624 / 1726 / 2134 / 2373 | P4 `notice_pull.go:157`; removed by lane; deleted. |
| DeliverAsyncJobInline | deliverAsyncJobInline:381 | DeliverAsyncJobInlineParams:388 / — | 405 | 401 | deliverAsyncJobInlineStmt | 270 / 1136 / 2016 / 2255 | P4 `notice_pull.go:488`; removed by lane; deleted. |
| SetAsyncJobNoticeMessageIDForClaimIfDone | setAsyncJobNoticeMessageIDForClaimIfDone:1389 | SetAsyncJobNoticeMessageIDForClaimIfDoneParams:1395 / — | 1419 | 1013 | setAsyncJobNoticeMessageIDForClaimIfDoneStmt | 627 / 1731 / 2135 / 2374 | P4 `notice_pull.go:415`; removed by lane; deleted. |
| AsyncReactionDebtExists | asyncReactionDebtExists:108 | — | 133 | 73 | asyncReactionDebtExistsStmt | 45 / 761 / 1941 / 2180 | P4 `async_job_reaction.go:29` and P5 `agent_turn.go`; both lanes rebind to batch/notice debt; deleted. |
| VisibleAsyncReactionDebtExists | visibleAsyncReactionDebtExists:1602 | — | 1628 | 1225 | visibleAsyncReactionDebtExistsStmt | 702 / 1856 / 2160 / 2399 | P4 `async_job_reaction.go:41`; lane rebinds; deleted. |
| MarkAsyncJobsReactedForOwner | markAsyncJobsReactedForOwner:1036 | MarkAsyncJobsReactedForOwnerParams:1041 / — | 1051 | 817 | markAsyncJobsReactedForOwnerStmt | 543 / 1591 / 2107 / 2346 | P4 `async_job_reaction.go`; `session_notices.sql:50` mention is a comment; lane rebinds to notices twin; deleted. |
| GetAsyncJobByClaimID | getAsyncJobByClaimID:477 | GetAsyncJobByClaimIDParams:482 / — | 493 | 420 | getAsyncJobByClaimIDStmt | 288 / 1166 / 2022 / 2261 | P4 `async_job_reaction.go:286`; lane rebinds; deleted. |
| IncrementAsyncJobWakeAttemptsForSnapshotRow | incrementAsyncJobWakeAttemptsForSnapshotRow:569 | IncrementAsyncJobWakeAttemptsForSnapshotRowParams:576 / — | 592 | 550 | incrementAsyncJobWakeAttemptsForSnapshotRowStmt | 384 / 1326 / 2054 / 2293 | P4 `async_job_reaction.go`; `session_notices.sql:55` is a comment; deleted. |
| SettleAsyncJobReactedFailedForSnapshotRow | settleAsyncJobReactedFailedForSnapshotRow:1464 | SettleAsyncJobReactedFailedForSnapshotRowParams:1471 / — | 1490 | 1034 | settleAsyncJobReactedFailedForSnapshotRowStmt | 642 / 1756 / 2140 / 2379 | P4 `async_job_reaction.go`; `session_notices.sql:65` is a comment; deleted. |
| ListReactedFailedAsyncJobsForOwner | listReactedFailedAsyncJobsForOwner:826 | — | 833 | 760 | listReactedFailedAsyncJobsForOwnerStmt | 495 / 1511 / 2091 / 2330 | P4 `async_job_reaction.go`; `session_notices.sql:74` is a comment; deleted. |
| SetAsyncJobsWakeZeroPendingForOwners | setAsyncJobsWakeZeroPendingForOwners:1432 | SetAsyncJobsWakeZeroPendingForOwnersParams:1437 / — | 1445 | 1017 | setAsyncJobsWakeZeroPendingForOwnersStmt | 630 / 1736 / 2136 / 2375 | P4 `async_job_reaction.go`; `session_notices.sql:80` is a comment; deleted. |
| ListRunningAsyncJobsForHost | listRunningAsyncJobsForHost:883 | — | 890 | 767 | listRunningAsyncJobsForHostStmt | 501 / 1521 / 2093 / 2332 | none. |
| ListRunningAsyncJobsForOwners | listRunningAsyncJobsForOwners:940 | — | 952 | 774 | listRunningAsyncJobsForOwnersStmt | 504 / 1526 / 2094 / 2333 | P5 `agent_notice_pull.go` and P7 `app_session_activity.go`, `app_run_async_wait.go`; lanes rebind to batch facts; deleted. |
| ListAsyncJobsForOwner | listAsyncJobsForOwner:677 | — | 682 | 607 | listAsyncJobsForOwnerStmt | 426 / 1396 / 2068 / 2307 | P5 `batch_cutover_facts.go`, `coordinator_drain_policy.go` rebind to node-backed facts; P7 `app.go`, P8 `sessions_show.go`, `sessions_why.go` rebind to batch readers; P4 `notice_row_ids.go:56` old receiver deleted; all lanes rebind; deleted. |
| PurgeAsyncJobsOlderThan | purgeAsyncJobsOlderThan:1112 | — | 1155 | 892 | purgeAsyncJobsOlderThanStmt | 570 / 1636 / 2116 / 2355 | P4 `retention_purge.go` legacy purge call; `session_notices.sql:111` is a comment; lane rebinds to batch retention; deleted. |
| CountAsyncJobsOlderThan | countAsyncJobsOlderThan:266 | — | 286 | 232 | countAsyncJobsOlderThanStmt | 183 / 991 / 1987 / 2226 | P4 `retention_purge.go`; lane rebinds; deleted. |
| ClearReactedFailedForOwner | clearReactedFailedForOwner:236 | ClearReactedFailedForOwnerParams:241 / — | 258 | 221 | clearReactedFailedForOwnerStmt | 171 / 971 / 1983 / 2222 | P4 `async_job_reaction.go`; `session_notices.sql:122` is a comment; deleted. |
| ArchiveAsyncJobToolCallID | archiveAsyncJobToolCallID:14 | ArchiveAsyncJobToolCallIDParams:21 / — | 50 | 52 | archiveAsyncJobToolCallIDStmt | 39 / 751 / 1939 / 2178 | none. |
| VoidUndeliveredAsyncJobsForOwner | voidUndeliveredAsyncJobsForOwner:1768 | VoidUndeliveredAsyncJobsForOwnerParams:1773 / — | 1785 | 1261 | voidUndeliveredAsyncJobsForOwnerStmt | 714 / 1876 / 2164 / 2403 | P4; `session_notices.sql:139` is a comment; deleted. |
| AsyncJobDebtOwners | asyncJobDebtOwners:63 | — | 75 | 59 | asyncJobDebtOwnersStmt | 42 / 756 / 1940 / 2179 | P4 `async_job_reaction.go:518`; lane rebinds; deleted. |
| SessionNoticeDebtOwners | sessionNoticeDebtOwners:1292 | — | 1300 | 987 | sessionNoticeDebtOwnersStmt | 618 / 1716 / 2132 / 2371 | P4 `async_job_reaction.go:522` (retained notices twin, not the async_jobs entry); async_jobs duplicate deleted. |
| ListNewFiles | listNewFiles:244 | — | 251 | 727 | listNewFilesStmt | 471 / 1471-1473 / 2083 / 2322 | none outside its own retained `files.sql` twin and the stale README promise P2 rewrites; invalid query removed. |
| RepointSessionNoticesJobToolCallID | repointSessionNoticesJobToolCallID:389 | RepointSessionNoticesJobToolCallIDParams:394 / — | 407 | 980 | repointSessionNoticesJobToolCallIDStmt | 609 / 1701-1703 / 2129 / 2368 | none; `session_notices.sql` source removed by P2. |

There are 43 `async_jobs.sql` queries plus the two separate queries above: **45 total**. Remove `AsyncJob` (`models.go:18`); retain `AsyncHost` (`models.go:11-16`) used by active `async_hosts.sql` and `HostStore` (`async_hosts.sql.go:54,71,111,167`; `session/host_store.go:39-40`).

Host-registry classification (consistent across §1/§4/§5): `RegisterAsyncHost`, `GetAsyncHost`, `ListAsyncHosts`, and `DeleteAsyncHostIfNoJobs` are NOT deleted as queries — each has a retained consumer (`session/host_lock.go:215-216,373,397`, `session/host_store.go:39-46`, `cmd/sessions_jobs.go:159`) and a live source query in retained `sql/async_hosts.sql` (§1 P2 KEEP rows); §4 deletes only their `async_jobs.sql`/`async_jobs.sql.go` duplicates, and lane P2 rebinds consumer imports to the regenerated `async_hosts.sql.go`. `ListAsyncHostsWithNoJobs` has no retained consumer outside its retained `async_hosts.sql` twin; its `async_jobs.sql` duplicate is deleted. Every other symbol in this table has no retained consumer: all non-deleted call sites live in files whose owning lane rewrites the call (P4 `notice_pull.go`/`async_job_reaction.go`/`retention_purge.go`/`notice_row_ids.go`, P5 `batch_cutover_facts.go`/`coordinator_drain_policy.go`/`agent_notice_pull.go`/`agent_turn.go`, P7/P8 readers), so the query is deleted and the named lane rebinds to the named replacement (batch reader or SessionNoticeStore twin); `session_notices.sql` mentions of deleted queries are comments only.

## 5. Literal lane ownership

The literal lane allocation is in the companion file; every §1 path has exactly one owner. Inventory contains 614 unique literal paths, of which 585 matched the declared fixed regex and 29 are explicitly path-reviewed retained/fixture files outside that expression. Lane totals: P1 10, P2 32, P3 7, P4 53, P5 219, P6 34, P7 82, P8 40, P9 30, P10 7, P11 5, P12 75, H5-W 19, H5-C 1. P3 permanently owns selector activation and later erasure; no transfer.

| Lane | Exact literal path list (companion `docs/plans/2026-10-21-h4-execution-manifest-lanes.md`) |
|---|---|
| P1 | See companion ownership list, P1 heading (10 literal files). |
| P2 | See companion ownership list, P2 heading (32 literal files). |
| P3 | See companion ownership list, P3 heading (7 literal files). |
| P4 | See companion ownership list, P4 heading (53 literal files). |
| P5 | See companion ownership list, P5 heading (219 literal files). |
| P6 | See companion ownership list, P6 heading (34 literal files). |
| P7 | See companion ownership list, P7 heading (82 literal files). |
| P8 | See companion ownership list, P8 heading (40 literal files). |
| P9 | See companion ownership list, P9 heading (30 literal files). |
| P10 | See companion ownership list, P10 heading (7 literal files). |
| P11 | See companion ownership list, P11 heading (5 literal files). |
| P12 | See companion ownership list, P12 heading (75 literal files). |
| H5-W | See companion ownership list, H5-W heading (19 literal files). |
| H5-C | See companion ownership list, H5-C heading (1 literal file). |


## 6. Coverage method and limit

Fixed source search: `git grep -l -I -E 'workLedger|sessionJobs|asyncJob|AsyncJobStore|async_jobs|asyncTool|stallDetach|stallDetacher|job_kill|job_output|job_shell_resolver|RUSH_BATCH_RUNTIME|BatchRuntimeEnabled|batchFlagEnabled|SetBatchRuntime|AsyncCompletion|ListNewFiles|RepointSessionNoticesJobToolCallID'` scoped to `internal/agent`, `internal/session`, `internal/app`, `internal/cmd`, `internal/server`, `internal/db`, `internal/config`, `internal/shell`, `internal/message`, `sdk`, `testdata/embedder`, `web/src`, `web/tests`, `.github`, `tools`, `docs`, `README.md`, and `CHANGELOG.md`. Per-scope tracked match counts and §1 inventory rows (grep column = `git grep -l -I -E <regex> -- <dir> | wc -l`; rows column = unique literal §1 paths under that directory):

| Directory | git grep -l | §1 rows | Diff | Explanation of every non-zero diff (all 29 outside-regex files, individually) |
|---|---:|---:|---:|---|
| internal/agent (root) | 215 | 219 | +4 | `agent_timeouts_test.go` KEEP timeout contract, fixture is flag/ledger-free. `batch_cutover_rerun.go` KEEP batch rerun stop surface (flag-on batch only). `batch_cutover_rerun_test.go` KEEP batch rerun coverage over the jobbatch engine. `coordinator_rate_limit_loop_test.go` KEEP rate-limit policy; no legacy token in file. |
| internal/agent/tools | 22 | 23 | +1 | `job_output.md` DELETE old tool description; literal deletion target, prose carries no searchable token. |
| internal/agent/prompt | 3 | 3 | 0 | — |
| internal/agent/templates | 1 | 1 | 0 | — |
| internal/session | 53 | 54 | +1 | `legacy_import_drain_external_test.go` P1 external drain scenario; fixture built via `fs.Sub` over `legacymigrations` + Goose, no regex token. |
| internal/app | 82 | 85 | +3 | `app_batch_bench_observer_test.go` KEEP baseline-only observer. `app_batch_bench_test.go` KEEP pinned benchmark. `app_run_setup.go` KEEP generic turn-stall policy, no detach deletion. |
| internal/cmd | 40 | 40 | 0 | — |
| internal/server | 23 | 30 | +7 | `p1_6_regression_test.go` KEEP regression (fixture only if old-store dependent). `p2_regression_test.go` KEEP regression (same rule). `p595_delete_streaming_test.go` KEEP streaming deletion regression. `p614_rerun_reservation_test.go` REWRITE reservation fixture to batch (uses `session.VoidedAsyncJob` only). `p623_panic_window_test.go` KEEP panic-window regression. `readpump_control_starvation_test.go` KEEP read-pump control regression. `rerun_hold_test.go` REWRITE old rerun hold fixture to batch (flag helpers only). |
| internal/db | 38 | 41 | +3 | `models.go` MIXED: remove `AsyncJob`, retain `AsyncHost` (token-free retained half). `storepath/storepath.go` P3: rewrite active filename to store-v1, retain legacy filename helpers. `migrations/20260809000001_add_session_run_queue.sql` P2: replace active chain (goose markers only). |
| internal/config | 4 | 6 | +2 | `datadir.go` KEEP dual-file old-directory hints. `datadir_test.go` KEEP legacy-local/dual-filename detection. |
| internal/shell | 7 | 7 | 0 | — |
| internal/message | 1 | 1 | 0 | — |
| sdk | 2 | 3 | +1 | `sdk/README.md` KEEP SDK API/readme; update current default semantics only. |
| testdata/embedder | 0 | 4 | +4 | `embed_test.go`, `go.mod`, `go.sum`, `testdata/negativeimport/negative.go` all KEEP: external module metadata, checksums, public embedder contract, negative-import fixture. |
| web/src | 14 | 14 | 0 | — |
| web/tests | 5 | 5 | 0 | — |
| .github | 1 | 2 | +1 | `workflows/build.yml` KEEP normal CI/sqlc job. |
| tools | 1 | 1 | 0 | — |
| docs | 71 | 73 | +2 | `docs/jobbatch.md` REWRITE current controls/default semantics. `docs/jobbatch-acceptance.md` REWRITE current acceptance matrix. |
| root (README.md, CHANGELOG.md) | 2 | 2 | 0 | — |

The grep column sums to 585 and the rows column to 614; the difference is exactly the 29 individually explained files above, which are retained/fixture or literal-deletion targets outside the fixed textual match set, listed for lane assignment, not claimed as regex hits. This is textual-pattern coverage only, not semantic completeness for aliases, reflection, or unsearched prose.


This section records operator decisions, not unfinished inventory tasks:

1. Confirm rollout quiescence: old processes and automatic restarts stopped through import. Host enumeration alone is not a registration fence.
2. Decide operational handling of a preexisting `rush_store_v1.db`; importer never overwrites it.
3. Confirm operator understanding that re-queued imported work can repeat external side effects; importer is not exactly-once execution.
4. Decide whether historical web completion badges remain read-only or are removed in H5-W; H5-W cannot remove historical display absent approval.
5. Select whether SDK interim execution fence stays through H4 or moves to a sessions column as a dependency-closed package.
6. Confirm no version bump/tag/release; none is authorized here.
7. Obtain signoff for specified #1475 presentation reductions and any D11 budget exception.
8. `internal/agent/batch_cutover_rerun.go`: rewrite the old ledger Stop path to batch; the legacy mechanism exists only as prose around the optional `BatchRerunStopper` interface (comments at :24-27); no executable legacy line is anchorable (verified by grep with comments filtered).
9. `internal/agent/batch_cutover_rerun_test.go`: rewrite legacy fixtures; keep rerun semantics; the file is already flag-ON batch (`jobbatch.Runner` :3) and contains no executable legacy line, so no anchor exists.
10. `internal/agent/cliscope_completion_hold_test.go`: rewrite the completion carrier to a neutral type; the legacy `async_jobs` reference at :3 is prose only.
11. `internal/agent/coordinator_bgshell_window_test.go`: rewrite legacy window setup; the only `job_kill` reference (:53) is a struct-field comment.
12. `internal/agent/coordinator_subagent_drivers.go`: rewrite old driver callbacks to the retained owner; the legacy callback reference (:11) is a comment.
13. `internal/agent/coordinator_subagents.go`: rewrite ledger delegation state; legacy references (:273, :321) are comments about `notifyAsyncCompletion`.
14. `internal/agent/coordinator_subagents_resume_test.go`: rewrite ledger fixture; the legacy reference (:64) is a comment.
15. `internal/agent/drain_debt_edge_cases_test.go`: rewrite the old debt carrier; the `PullPendingAsyncJobNotice` SQL reference (:77) is a comment.
16. `internal/agent/drain_hint_sweep_test.go`: rewrite ledger hint state; the legacy `workLedger` reference (:3) is a comment.
17. `internal/agent/drain_x1_batch_fixture_test.go`: rewrite the cross-authority fixture; the premise is documented in comments (:9-10) only.
18. `internal/agent/tools/agent_control_test.go`: rewrite removed inventory; the `job_kill`/`job_output` reference (:165) is a comment.
19. `internal/agent/tools/run_command.go`: rewrite the wrapper edge; the `job_kill`/`job_output` references (:228, :236) are comments; physical command handling is preserved.
20. `internal/agent/x2_delegation_fixture_test.go`: rewrite the authority selector fixture; `batchFlagEnabled()`/`RUSH_BATCH_RUNTIME` appear only in comments (:4-6).
21. `internal/app/async_scenarios_cancel_test.go`: rewrite the old `asyncTool` assumption; the reference (:6) is a comment.
22. `internal/app/app_run_reviewer_evidence.go`: rewrite the completion evidence carrier to a neutral type; `FormatAsyncCompletion` references (:592, :601, :639) are comments.
23. `internal/app/app_run_six_completion_routing_test.go`: rewrite the old carrier; the `FormatAsyncCompletion` reference (:49) is a comment.
24. `internal/app/app_run_supervision_test.go`: rewrite the ledger harness; `workLedger` references (:6, :45) are comments; preserve SV lifecycle.
25. `internal/app/app_run_wait_batch_fixture_test.go`: rewrite the dual-mode helper; the `RUSH_BATCH_RUNTIME`/`AsyncJobStore` references (:1, :4) are comments.

H4 activation requires exact-SHA normal CI, three-OS shadow build/agent, shadow web, actual nonzero AW coverage, importer startup/reopen acceptance, CT Windows production acceptance, #1427 recovery evidence, #1475 SV/check-in semantics, and D11 on final fresh-store candidate. Prior source presence is not evidence that those gates passed. No activation or merge authorization is asserted by this manifest.
