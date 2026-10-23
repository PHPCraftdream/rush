# R12 Old-Path Inventory Manifest (2026-10-14)

> **Historical snapshot.** Superseded for the current HEAD by `docs/plans/2026-10-21-h4-execution-manifest.md`; do not use this document as a deletion list. Preserve it as historical evidence; do not regenerate it as a current inventory.

## Provenance

- Generator script: `tools/r12a_old_path_manifest.py` (stdlib only, deterministic, no network, not run in CI).
- Generated date (fixed literal): 2026-10-14.
- Exact command equivalent (run once per top-level dir and concatenated, NO cap):
  ```sh
  git grep -n -I -E 'async_jobs|AsyncJob|asyncJob|work_ledger|workLedger|WorkLedger|awaitSync|job_kill|job_output|stallDetach|stallDetacher|stall-|RUSH_BATCH_RUNTIME' -- internal sdk web docs .github .githooks
  ```
- Total hits: 4697 across 443 files (sum of per-directory runs).
- **Do not edit by hand; regenerate with the script.**

## Summary by family

| Family | Files | Hits | Disposition |
|---|---:|---:|---|
| app_lifecycle | 66 | 367 | port |
| cli_sessions | 27 | 198 | port |
| historical_docs | 28 | 746 | historical |
| ledger_consumers | 60 | 371 | port |
| ledger_terminal_authority | 44 | 944 | delete |
| migration_and_testdata | 8 | 69 | retain (historical migrations / DDL fixtures allowlisted) |
| old_model_tools | 12 | 92 | delete |
| prompts_tool_inventories | 8 | 19 | port |
| retained_infrastructure | 5 | 70 | retain |
| server_rerun | 7 | 40 | port |
| session_old_store | 25 | 394 | delete |
| sql_sqlc | 4 | 441 | port (split hosts first) |
| stall_jobs | 8 | 151 | delete (stall clock/watchdog policy retained separately) |
| switch | 3 | 11 | delete |
| unknown | 148 | 857 | review (disposition must be assigned by a human) |
| web_completions | 12 | 41 | port |

Total dispositions across files: 443

| Disposition(s) | Files |
|---|---:|
| delete | 82 |
| delete (stall clock/watchdog policy retained separately) | 8 |
| delete; delete | 1 |
| historical | 28 |
| port | 138 |
| port (split hosts first) | 4 |
| port; port | 21 |
| retain | 5 |
| retain (historical migrations / DDL fixtures allowlisted) | 8 |
| review (disposition must be assigned by a human) | 148 |

## Files

| Path | Lines | Hits | Families | Disposition | Flag |
|---|---:|---:|---|---|---|
| `.github/workflows/publish-fork-npm.yml` | 295 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/async-invariants.md` | 246 | 28 | historical_docs | historical |  |
| `docs/checkpoints/2026-06-25-2218.md` | 102 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-06-26-0831.md` | 103 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-07-19-0729.md` | 217 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-07-30-1400.md` | 84 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-08-03-1031.md` | 60 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-09-28-0029.md` | 72 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-09-28-1711.md` | 77 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-09-29-0925.md` | 63 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-09-30-0810.md` | 56 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-09-30-2008.md` | 208 | 9 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-01-1351.md` | 92 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-01-2114.md` | 65 | 6 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-02-0510.md` | 58 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-05-0755.md` | 42 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-05-1331.md` | 42 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-05-2020.md` | 48 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-06-0530.md` | 41 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-06-1834.md` | 71 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-07-0554.md` | 56 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-07-2137.md` | 55 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-08-1449.md` | 59 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-11-2215.md` | 53 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/checkpoints/2026-10-12-0734.md` | 58 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/ci/2026-10-13-selector-replay.md` | 81 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/design/event-loop-turn.md` | 289 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `docs/plans/2026-09-02-sdk-scoped-file-tools-plan.md` | 740 | 5 | historical_docs | historical |  |
| `docs/plans/2026-09-24-agent-wakes-and-async-job-control.md` | 256 | 4 | historical_docs | historical |  |
| `docs/plans/2026-09-27-async-phase1-spec.md` | 891 | 105 | historical_docs | historical |  |
| `docs/plans/2026-09-27-async-phase2-spec.md` | 1950 | 90 | historical_docs | historical | OVER-LIMIT |
| `docs/plans/2026-09-27-async-structured-concurrency.md` | 332 | 5 | historical_docs | historical |  |
| `docs/plans/2026-09-27-wake-tools-contract.md` | 1091 | 66 | historical_docs | historical | OVER-LIMIT |
| `docs/plans/2026-09-28-async-phase3-spec.md` | 1136 | 70 | historical_docs | historical | OVER-LIMIT |
| `docs/plans/2026-09-28-async-phase4-durable-core.md` | 852 | 28 | historical_docs | historical |  |
| `docs/plans/2026-09-28-async-phase4-spec.md` | 1407 | 162 | historical_docs | historical | OVER-LIMIT |
| `docs/plans/2026-10-01-bg-shell-ledger.md` | 353 | 22 | historical_docs | historical |  |
| `docs/plans/2026-10-01-in-turn-progress-guard.md` | 410 | 13 | historical_docs | historical |  |
| `docs/plans/2026-10-01-inline-window.md` | 119 | 8 | historical_docs | historical |  |
| `docs/plans/2026-10-01-reaction-chain-guard.md` | 127 | 3 | historical_docs | historical |  |
| `docs/plans/2026-10-01-run-cost.md` | 315 | 1 | historical_docs | historical |  |
| `docs/plans/2026-10-01-shared-data-dir.md` | 325 | 2 | historical_docs | historical |  |
| `docs/plans/2026-10-01-structural-refactor.md` | 94 | 2 | historical_docs | historical |  |
| `docs/plans/2026-10-01-turn-arbiter.md` | 600 | 11 | historical_docs | historical |  |
| `docs/plans/2026-10-02-async-job-cap.md` | 253 | 28 | historical_docs | historical |  |
| `docs/plans/2026-10-02-reviewer-pass-verification.md` | 275 | 1 | historical_docs | historical |  |
| `docs/plans/2026-10-02-worker-question-wake.md` | 148 | 9 | historical_docs | historical |  |
| `docs/plans/2026-10-05-async-phase4-rev8-map.md` | 78 | 8 | historical_docs | historical |  |
| `docs/plans/2026-10-05-bg-shell-remainder.md` | 111 | 19 | historical_docs | historical |  |
| `docs/plans/2026-10-07-job-batches-design.md` | 2587 | 8 | historical_docs | historical | OVER-LIMIT |
| `docs/plans/2026-10-12-job-batches-rush-integration.md` | 158 | 12 | historical_docs | historical |  |
| `docs/research/2026-10-12-job-batches-rush-map-a.md` | 146 | 11 | historical_docs | historical |  |
| `docs/research/2026-10-12-job-batches-rush-map-b.md` | 156 | 17 | historical_docs | historical |  |
| `docs/research/2026-10-12-job-batches-rush-map-c.md` | 163 | 8 | historical_docs | historical |  |
| `docs/reviews/2026-07-30-full-project-review.md` | 681 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-04-sdk-library-review-round-16-1209.md` | 293 | 6 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-05-2047-commit-review-36h.md` | 387 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-29-async-phase4-round1-rerun-design.md` | 108 | 19 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-29-async-phase4-round1.md` | 213 | 27 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md` | 172 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round2.md` | 113 | 12 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round3.md` | 72 | 13 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round4.md` | 55 | 5 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round5.md` | 37 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round6.md` | 45 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round7.md` | 35 | 6 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-09-30-async-phase4-round8.md` | 51 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-10-05-async-round9.md` | 164 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-10-08-job-batches-xxs-rush-integration-round2.md` | 134 | 12 | unknown | review (disposition must be assigned by a human) |  |
| `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md` | 159 | 19 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/agent.go` | 908 | 4 | ledger_consumers | port | NEAR-LIMIT |
| `internal/agent/agent_drain_decision.go` | 72 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/agent_notice_pull.go` | 125 | 9 | ledger_consumers | port |  |
| `internal/agent/agent_options.go` | 128 | 1 | ledger_consumers | port |  |
| `internal/agent/agent_timeouts_test.go` | 403 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/agent_turn.go` | 905 | 3 | ledger_consumers | port | NEAR-LIMIT |
| `internal/agent/agent_turn_failure.go` | 326 | 2 | ledger_consumers | port |  |
| `internal/agent/agent_turn_step.go` | 886 | 10 | ledger_consumers | port |  |
| `internal/agent/agent_turn_step_test.go` | 96 | 2 | ledger_consumers | port |  |
| `internal/agent/agent_turn_stream.go` | 507 | 3 | ledger_consumers | port |  |
| `internal/agent/agent_turn_stream_retry_test.go` | 189 | 1 | ledger_consumers | port |  |
| `internal/agent/agent_turn_stream_tool_suggest_test.go` | 121 | 2 | ledger_consumers | port |  |
| `internal/agent/async_tool.go` | 556 | 30 | ledger_terminal_authority | delete |  |
| `internal/agent/async_tool_sync_test.go` | 218 | 14 | ledger_terminal_authority | delete |  |
| `internal/agent/async_tool_test.go` | 231 | 14 | ledger_terminal_authority | delete |  |
| `internal/agent/batch_allowed_controls.go` | 36 | 2 | prompts_tool_inventories | port |  |
| `internal/agent/batch_exec_agent_doc.go` | 53 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/batch_exec_generic_test.go` | 380 | 9 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/batch_install_inventory_test.go` | 351 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/batch_install_tools.go` | 148 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/batch_install_tools_test.go` | 437 | 5 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/batch_model_flag_test.go` | 122 | 11 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/bgshell_claim.go` | 165 | 7 | ledger_terminal_authority | delete |  |
| `internal/agent/bgshell_claim_test.go` | 461 | 39 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/bgshell_observer_recheck_test.go` | 213 | 10 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/cli_active_work.go` | 109 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/cliscope_completion_hold_test.go` | 116 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/coordinator.go` | 651 | 13 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/coordinator_agent_control.go` | 286 | 13 | ledger_consumers | port |  |
| `internal/agent/coordinator_agent_control_test.go` | 460 | 14 | ledger_consumers | port |  |
| `internal/agent/coordinator_autoresume_test.go` | 319 | 8 | ledger_consumers | port |  |
| `internal/agent/coordinator_await_tasks.go` | 330 | 7 | ledger_consumers | port |  |
| `internal/agent/coordinator_await_tasks_worker_test.go` | 375 | 8 | ledger_consumers | port |  |
| `internal/agent/coordinator_background.go` | 236 | 9 | ledger_consumers | port |  |
| `internal/agent/coordinator_bgshell_cap.go` | 124 | 5 | ledger_consumers | port |  |
| `internal/agent/coordinator_bgshell_window_test.go` | 152 | 1 | ledger_consumers | port |  |
| `internal/agent/coordinator_child_policy_durable_test.go` | 349 | 13 | ledger_consumers | port |  |
| `internal/agent/coordinator_claim_driver_error_test.go` | 64 | 4 | ledger_consumers | port |  |
| `internal/agent/coordinator_cli_scope_test.go` | 226 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_drain_cap_test.go` | 132 | 2 | ledger_consumers | port |  |
| `internal/agent/coordinator_drain_policy.go` | 265 | 8 | ledger_consumers | port |  |
| `internal/agent/coordinator_external_driver_durable_test.go` | 282 | 6 | ledger_consumers | port |  |
| `internal/agent/coordinator_external_driver_release_test.go` | 58 | 4 | ledger_consumers | port |  |
| `internal/agent/coordinator_interrupt.go` | 779 | 10 | ledger_consumers | port |  |
| `internal/agent/coordinator_rate_limit.go` | 166 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_rate_limit_budget_test.go` | 53 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_rate_limit_loop_test.go` | 160 | 2 | ledger_consumers | port |  |
| `internal/agent/coordinator_reaction_chain.go` | 70 | 1 | ledger_consumers | port |  |
| `internal/agent/coordinator_reaction_chain_test.go` | 290 | 4 | ledger_consumers | port |  |
| `internal/agent/coordinator_reaction_repeated_test.go` | 82 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_reaction_source.go` | 313 | 19 | ledger_consumers | port |  |
| `internal/agent/coordinator_recheck.go` | 180 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_recheck_child_test.go` | 239 | 14 | ledger_consumers | port |  |
| `internal/agent/coordinator_recheck_debt_deadline_test.go` | 223 | 8 | ledger_consumers | port |  |
| `internal/agent/coordinator_rerun.go` | 45 | 6 | ledger_consumers | port |  |
| `internal/agent/coordinator_rerun_test.go` | 208 | 16 | ledger_consumers | port |  |
| `internal/agent/coordinator_retry_classify.go` | 365 | 1 | ledger_consumers | port |  |
| `internal/agent/coordinator_run.go` | 913 | 1 | ledger_consumers | port | NEAR-LIMIT |
| `internal/agent/coordinator_run_drain_test.go` | 105 | 2 | ledger_consumers | port |  |
| `internal/agent/coordinator_scope_recovery_test.go` | 105 | 13 | ledger_consumers | port |  |
| `internal/agent/coordinator_session_state_sweep.go` | 77 | 3 | ledger_consumers | port |  |
| `internal/agent/coordinator_session_state_sweep_test.go` | 113 | 6 | ledger_consumers | port |  |
| `internal/agent/coordinator_stop_child_driver_test.go` | 105 | 1 | ledger_consumers | port |  |
| `internal/agent/coordinator_stop_restart_test.go` | 85 | 4 | ledger_consumers | port |  |
| `internal/agent/coordinator_stop_tree_test.go` | 142 | 5 | ledger_consumers | port |  |
| `internal/agent/coordinator_subagent_drivers.go` | 227 | 1 | ledger_consumers | port |  |
| `internal/agent/coordinator_subagent_drivers_test.go` | 164 | 7 | ledger_consumers | port |  |
| `internal/agent/coordinator_tool_pinning_test.go` | 995 | 2 | ledger_consumers | port | NEAR-LIMIT |
| `internal/agent/coordinator_tools.go` | 901 | 6 | ledger_consumers | port | NEAR-LIMIT |
| `internal/agent/coordinator_tools_test.go` | 620 | 2 | ledger_consumers | port |  |
| `internal/agent/coordinator_wake.go` | 79 | 4 | ledger_consumers | port |  |
| `internal/agent/coordinator_wake_allowlist_test.go` | 191 | 10 | ledger_consumers | port |  |
| `internal/agent/coordinator_wake_reaction_debt_test.go` | 307 | 10 | ledger_consumers | port |  |
| `internal/agent/coordinator_work_scope.go` | 188 | 8 | ledger_consumers | port |  |
| `internal/agent/coordinator_work_scope_test.go` | 74 | 7 | ledger_consumers | port |  |
| `internal/agent/default_job_timeout_test.go` | 213 | 5 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/delegation_question.go` | 275 | 9 | ledger_terminal_authority | delete |  |
| `internal/agent/delegation_question_test.go` | 540 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_attempt.go` | 569 | 19 | ledger_consumers | port |  |
| `internal/agent/drain_attempt_fixture_test.go` | 302 | 10 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_compaction_continuation_test.go` | 147 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_compaction_submit_test.go` | 445 | 18 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_debt_edge_cases_test.go` | 227 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_gate_streaks_test.go` | 190 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_launch_gate_test.go` | 333 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/drain_notice_pull_test.go` | 462 | 10 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/loop_detection_test.go` | 364 | 6 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/policy_leaf.go` | 105 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/policy_leaf_baseline_test.go` | 93 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/prompt/prompt_scoped_test.go` | 159 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/question_stop_format_test.go` | 80 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/release_callback_join_test.go` | 177 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/restricted_stall_detach_test.go` | 24 | 1 | old_model_tools | delete |  |
| `internal/agent/restricted_tool.go` | 173 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/session_idle_trigger_test.go` | 188 | 10 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/step_progress.go` | 360 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/step_progress_test.go` | 157 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/stream_watchdog.go` | 569 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/stream_watchdog_toolpause_test.go` | 397 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/supervision.go` | 666 | 21 | ledger_consumers | port |  |
| `internal/agent/supervision_silence.go` | 142 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/supervision_silence_test.go` | 283 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/supervision_test.go` | 631 | 14 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/templates/coder.md.tpl` | 79 | 1 | prompts_tool_inventories | port |  |
| `internal/agent/tool_suggest_test.go` | 72 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/agent_control_test.go` | 172 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/await_tasks.md` | 36 | 1 | prompts_tool_inventories | port |  |
| `internal/agent/tools/bash.go` | 463 | 5 | prompts_tool_inventories | port |  |
| `internal/agent/tools/bash.md.tpl` | 136 | 4 | prompts_tool_inventories | port |  |
| `internal/agent/tools/bash_start_failure_test.go` | 183 | 3 | prompts_tool_inventories | port |  |
| `internal/agent/tools/job_kill.go` | 212 | 9 | old_model_tools | delete |  |
| `internal/agent/tools/job_kill.md` | 21 | 1 | old_model_tools | delete |  |
| `internal/agent/tools/job_kill_bgshell.go` | 34 | 1 | old_model_tools | delete |  |
| `internal/agent/tools/job_output.go` | 234 | 7 | old_model_tools | delete |  |
| `internal/agent/tools/job_output_lastoutput_test.go` | 92 | 2 | old_model_tools | delete |  |
| `internal/agent/tools/job_output_pending.go` | 25 | 2 | old_model_tools | delete |  |
| `internal/agent/tools/job_output_test.go` | 227 | 4 | old_model_tools | delete |  |
| `internal/agent/tools/job_run_command_control_test.go` | 262 | 13 | old_model_tools, old_model_tools | delete; delete |  |
| `internal/agent/tools/job_shell_resolver.go` | 139 | 21 | old_model_tools | delete |  |
| `internal/agent/tools/job_shell_resolver_test.go` | 371 | 18 | old_model_tools | delete |  |
| `internal/agent/tools/run_command.go` | 297 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/run_command.md.tpl` | 25 | 1 | prompts_tool_inventories | port |  |
| `internal/agent/tools/run_command_output_buffer.go` | 135 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/run_command_process_kill_test.go` | 146 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/run_command_process_unix.go` | 36 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/run_command_process_windows.go` | 24 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/tools/stall_detach_controller.go` | 56 | 6 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_arbiter.go` | 533 | 4 | ledger_consumers | port |  |
| `internal/agent/turn_arbiter_shadow_test.go` | 344 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_arbiter_state.go` | 477 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_arbiter_testhelpers_test.go` | 112 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_progress_fixture_test.go` | 365 | 5 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_progress_guard.go` | 367 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_stall_clock.go` | 300 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/agent/turn_stall_clock_test.go` | 113 | 2 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_detach_cleanup_test.go` | 217 | 52 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_policy_spacing_test.go` | 82 | 4 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_policy_test.go` | 215 | 2 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_tool_detach.go` | 324 | 63 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_tool_test.go` | 213 | 21 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/turn_stall_zero_timeout_test.go` | 210 | 1 | stall_jobs | delete (stall clock/watchdog policy retained separately) |  |
| `internal/agent/work_job.go` | 269 | 26 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger.go` | 977 | 77 | ledger_terminal_authority | delete | NEAR-LIMIT |
| `internal/agent/work_ledger_abort_cancelled_test.go` | 83 | 10 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_ack_gate_test.go` | 217 | 29 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_ack_tag_test.go` | 214 | 6 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_announce.go` | 281 | 28 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_await.go` | 30 | 2 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_bgshell_kill.go` | 143 | 15 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_bgshell_kill_race_test.go` | 128 | 10 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_bgshell_kill_test.go` | 250 | 32 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_cap.go` | 202 | 5 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_cap_test.go` | 322 | 37 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_claim_id_test.go` | 101 | 10 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_delegation.go` | 554 | 26 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_delegation_test.go` | 775 | 88 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_driver.go` | 15 | 2 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_durable_test.go` | 477 | 49 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_identity_test.go` | 146 | 13 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_inline.go` | 203 | 11 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_inline_test.go` | 515 | 16 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_job_kill_result_test.go` | 201 | 8 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_job_kill_reuse_test.go` | 122 | 15 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_job_kill_terminal_test.go` | 84 | 14 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_job_kill_test.go` | 472 | 78 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_job_shell_test.go` | 115 | 31 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_launch_failure_test.go` | 81 | 3 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_next_event_test.go` | 58 | 4 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_pending.go` | 34 | 2 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_pending_test.go` | 103 | 17 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_reaction.go` | 159 | 11 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_rerun.go` | 52 | 2 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_rerun_test.go` | 109 | 11 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_review_fixes_test.go` | 316 | 37 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_sync_timeout_test.go` | 82 | 8 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_test.go` | 346 | 51 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_teststore_test.go` | 61 | 7 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_timeout.go` | 338 | 21 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_timeout_test.go` | 452 | 41 | ledger_terminal_authority | delete |  |
| `internal/agent/work_ledger_transition.go` | 388 | 17 | ledger_terminal_authority | delete |  |
| `internal/app/app.go` | 579 | 12 | app_lifecycle | port |  |
| `internal/app/app_accessors.go` | 82 | 9 | app_lifecycle | port |  |
| `internal/app/app_agent_setup.go` | 208 | 4 | app_lifecycle | port |  |
| `internal/app/app_batch_cli.go` | 163 | 2 | app_lifecycle | port |  |
| `internal/app/app_batch_doc.go` | 75 | 4 | app_lifecycle | port |  |
| `internal/app/app_batch_install.go` | 178 | 9 | app_lifecycle | port |  |
| `internal/app/app_batch_install_test.go` | 287 | 10 | app_lifecycle | port |  |
| `internal/app/app_batch_runtime.go` | 235 | 3 | app_lifecycle | port |  |
| `internal/app/app_forced_shutdown_host_test.go` | 58 | 3 | app_lifecycle | port |  |
| `internal/app/app_lifecycle.go` | 429 | 3 | app_lifecycle | port |  |
| `internal/app/app_run_a15_a18_a10_test.go` | 193 | 2 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_async_wait.go` | 80 | 4 | app_lifecycle | port |  |
| `internal/app/app_run_await_tasks_test.go` | 401 | 5 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_billing_test.go` | 52 | 4 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_chain_guard_e2e_test.go` | 249 | 6 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_cli_drain_ctrlc_test.go` | 122 | 4 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_deferred_debt_test.go` | 131 | 1 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_job_kill_by_id_test.go` | 134 | 15 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_loop_more_test.go` | 283 | 3 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_loop_round5_test.go` | 274 | 1 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_loop_test.go` | 376 | 6 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_question_open_work_test.go` | 75 | 3 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_quota.go` | 178 | 2 | app_lifecycle | port |  |
| `internal/app/app_run_quota_child_test.go` | 131 | 1 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_quota_test.go` | 160 | 5 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_reviewer_readonly_test.go` | 337 | 1 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_setup.go` | 481 | 1 | app_lifecycle | port |  |
| `internal/app/app_run_subagent_root_wait_test.go` | 472 | 9 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_supervision_test.go` | 84 | 2 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_timeout_test.go` | 136 | 1 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_run_wait_heartbeat_test.go` | 114 | 2 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/app_session_activity.go` | 278 | 4 | app_lifecycle | port |  |
| `internal/app/app_session_activity_test.go` | 193 | 9 | app_lifecycle | port |  |
| `internal/app/app_session_drivers.go` | 69 | 2 | app_lifecycle | port |  |
| `internal/app/app_session_drivers_test.go` | 56 | 3 | app_lifecycle | port |  |
| `internal/app/async_phase4_driver_scenarios_test.go` | 285 | 6 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/async_phase4_recovery_scenarios_test.go` | 634 | 19 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/async_phase4_recovery_unannounced_test.go` | 104 | 5 | app_lifecycle, app_lifecycle | port; port |  |
| `internal/app/batch_install_coordinator_test.go` | 48 | 3 | app_lifecycle | port |  |
| `internal/app/batch_install_factory.go` | 118 | 2 | app_lifecycle | port |  |
| `internal/app/batch_install_factory_test.go` | 69 | 1 | app_lifecycle | port |  |
| `internal/app/p1_5_shutdown_test.go` | 289 | 1 | app_lifecycle | port |  |
| `internal/app/release_gate_test.go` | 430 | 1 | app_lifecycle | port |  |
| `internal/cmd/batch_cli.go` | 273 | 1 | switch | delete |  |
| `internal/cmd/batch_cli_runtime_test.go` | 252 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/cmd/batch_cli_test.go` | 334 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `internal/cmd/claude_wrush_command.md` | 108 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/cmd/run.go` | 997 | 3 | unknown | review (disposition must be assigned by a human) | NEAR-LIMIT |
| `internal/cmd/sessions_activity_cmd_test.go` | 210 | 5 | cli_sessions | port |  |
| `internal/cmd/sessions_audit.go` | 213 | 4 | cli_sessions | port |  |
| `internal/cmd/sessions_audit_db.go` | 334 | 7 | cli_sessions | port |  |
| `internal/cmd/sessions_audit_findings.go` | 181 | 1 | cli_sessions | port |  |
| `internal/cmd/sessions_audit_rules.go` | 341 | 10 | cli_sessions | port |  |
| `internal/cmd/sessions_audit_rules_test.go` | 772 | 11 | cli_sessions | port |  |
| `internal/cmd/sessions_audit_test.go` | 458 | 2 | cli_sessions | port |  |
| `internal/cmd/sessions_cancel_test.go` | 90 | 2 | cli_sessions | port |  |
| `internal/cmd/sessions_compact.go` | 416 | 1 | cli_sessions | port |  |
| `internal/cmd/sessions_gc.go` | 306 | 10 | cli_sessions | port |  |
| `internal/cmd/sessions_gc_jobs_test.go` | 366 | 37 | cli_sessions | port |  |
| `internal/cmd/sessions_help_text_test.go` | 134 | 1 | cli_sessions | port |  |
| `internal/cmd/sessions_inject_livework_test.go` | 133 | 4 | cli_sessions | port |  |
| `internal/cmd/sessions_jobs.go` | 246 | 5 | cli_sessions | port |  |
| `internal/cmd/sessions_jobs_test.go` | 266 | 7 | cli_sessions | port |  |
| `internal/cmd/sessions_list.go` | 243 | 3 | cli_sessions | port |  |
| `internal/cmd/sessions_list_awaiting_test.go` | 81 | 2 | cli_sessions | port |  |
| `internal/cmd/sessions_list_test.go` | 297 | 10 | cli_sessions | port |  |
| `internal/cmd/sessions_own_job_status_test.go` | 232 | 6 | cli_sessions | port |  |
| `internal/cmd/sessions_reset.go` | 186 | 1 | cli_sessions | port |  |
| `internal/cmd/sessions_reset_async_test.go` | 231 | 14 | cli_sessions | port |  |
| `internal/cmd/sessions_run_driver_status_test.go` | 262 | 7 | cli_sessions | port |  |
| `internal/cmd/sessions_show.go` | 365 | 10 | cli_sessions | port |  |
| `internal/cmd/sessions_show_jobs_test.go` | 92 | 4 | cli_sessions | port |  |
| `internal/cmd/sessions_why.go` | 333 | 9 | cli_sessions | port |  |
| `internal/cmd/sessions_why_awaiting_test.go` | 131 | 5 | cli_sessions | port |  |
| `internal/cmd/sessions_why_descendant_test.go` | 403 | 20 | cli_sessions | port |  |
| `internal/config/batch_runtime_flag.go` | 17 | 2 | switch | delete |  |
| `internal/config/batch_runtime_flag_test.go` | 24 | 8 | switch | delete |  |
| `internal/config/config.go` | 675 | 2 | prompts_tool_inventories | port |  |
| `internal/config/config_mcp.go` | 563 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/config/store_oauth_matrix_test.go` | 518 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/db/async_jobs.sql.go` | 1791 | 229 | sql_sqlc | port (split hosts first) | OVER-LIMIT |
| `internal/db/async_phase4_schema_test.go` | 349 | 31 | app_lifecycle | port |  |
| `internal/db/async_phase4_settle_failure_test.go` | 274 | 42 | app_lifecycle | port |  |
| `internal/db/db.go` | 2358 | 217 | unknown | review (disposition must be assigned by a human) | OVER-LIMIT |
| `internal/db/messages.sql.go` | 1440 | 1 | unknown | review (disposition must be assigned by a human) | OVER-LIMIT |
| `internal/db/migrate_policy.go` | 333 | 1 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrate_policy_test.go` | 337 | 1 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20260928000002_add_async_phase4_core.sql` | 173 | 27 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20260929000002_add_claim_id_to_async_jobs.sql` | 32 | 5 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20260929000003_add_announce_message_id_to_async_jobs.sql` | 17 | 2 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20260929000004_add_session_drivers.sql` | 28 | 1 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20260929000005_add_wake_schedules.sql` | 40 | 1 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/migrations/20261005000001_bg_shell_jobs.sql` | 130 | 31 | migration_and_testdata | retain (historical migrations / DDL fixtures allowlisted) |  |
| `internal/db/models.go` | 324 | 1 | sql_sqlc | port (split hosts first) |  |
| `internal/db/querier.go` | 1277 | 88 | sql_sqlc | port (split hosts first) | OVER-LIMIT |
| `internal/db/session_drivers.sql.go` | 182 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/db/session_notices.sql.go` | 570 | 13 | unknown | review (disposition must be assigned by a human) |  |
| `internal/db/sql/async_jobs.sql` | 563 | 123 | sql_sqlc | port (split hosts first) |  |
| `internal/db/sql/messages.sql` | 545 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/db/sql/session_drivers.sql` | 38 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/db/sql/session_notices.sql` | 142 | 13 | unknown | review (disposition must be assigned by a human) |  |
| `internal/log/goroutine_dump.go` | 208 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/message/message.go` | 971 | 2 | unknown | review (disposition must be assigned by a human) | NEAR-LIMIT |
| `internal/server/descendant_work_batch_test.go` | 188 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/descendant_work_liveness_test.go` | 188 | 8 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/handlers_agent_rerun.go` | 596 | 1 | server_rerun | port |  |
| `internal/server/handlers_batch.go` | 433 | 2 | server_rerun | port |  |
| `internal/server/handlers_batch_doc.go` | 38 | 4 | server_rerun | port |  |
| `internal/server/handlers_batch_test.go` | 296 | 4 | server_rerun | port |  |
| `internal/server/handlers_livework.go` | 417 | 12 | server_rerun | port |  |
| `internal/server/handlers_livework_awaiting_test.go` | 64 | 1 | server_rerun | port |  |
| `internal/server/handlers_livework_test.go` | 637 | 16 | server_rerun | port |  |
| `internal/server/p1_6_regression_test.go` | 248 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/p2_regression_test.go` | 386 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/p595_delete_streaming_test.go` | 420 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/p614_rerun_reservation_test.go` | 492 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/p623_panic_window_test.go` | 360 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/p630_rerun_cancel_window_test.go` | 335 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/protocol.go` | 815 | 7 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/readpump_control_starvation_test.go` | 389 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/rerun_hold_test.go` | 179 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/run_driver_liveness_test.go` | 55 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/server/web_composer_history_test.go` | 397 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/async_job_bgshell.go` | 111 | 5 | session_old_store | delete |  |
| `internal/session/async_job_bgshell_announce_test.go` | 60 | 3 | session_old_store | delete |  |
| `internal/session/async_job_bgshell_collision_test.go` | 118 | 7 | session_old_store | delete |  |
| `internal/session/async_job_bgshell_test.go` | 74 | 5 | session_old_store | delete |  |
| `internal/session/async_job_claim_id_test.go` | 110 | 2 | session_old_store | delete |  |
| `internal/session/async_job_dur2_atomicity_test.go` | 72 | 2 | session_old_store | delete |  |
| `internal/session/async_job_gc.go` | 50 | 11 | session_old_store | delete |  |
| `internal/session/async_job_gc_jobkill_test.go` | 72 | 11 | session_old_store | delete |  |
| `internal/session/async_job_gc_test.go` | 183 | 16 | session_old_store | delete |  |
| `internal/session/async_job_kill_notice_test.go` | 335 | 38 | session_old_store | delete |  |
| `internal/session/async_job_reaction.go` | 534 | 44 | session_old_store | delete |  |
| `internal/session/async_job_reaction_test.go` | 883 | 11 | session_old_store | delete |  |
| `internal/session/async_job_reader.go` | 277 | 21 | session_old_store | delete |  |
| `internal/session/async_job_reader_batch.go` | 191 | 6 | session_old_store | delete |  |
| `internal/session/async_job_reader_test.go` | 547 | 9 | session_old_store | delete |  |
| `internal/session/async_job_recovery.go` | 465 | 22 | session_old_store | delete |  |
| `internal/session/async_job_recovery_concurrent_test.go` | 322 | 7 | session_old_store | delete |  |
| `internal/session/async_job_recovery_test.go` | 861 | 25 | session_old_store | delete |  |
| `internal/session/async_job_rerun.go` | 189 | 13 | session_old_store | delete |  |
| `internal/session/async_job_rerun_test.go` | 495 | 8 | session_old_store | delete |  |
| `internal/session/async_job_reset.go` | 134 | 6 | session_old_store | delete |  |
| `internal/session/async_job_store.go` | 709 | 60 | session_old_store | delete |  |
| `internal/session/async_job_store_lockorder_test.go` | 108 | 1 | session_old_store | delete |  |
| `internal/session/async_job_store_test.go` | 543 | 60 | session_old_store | delete |  |
| `internal/session/async_job_store_testhook.go` | 22 | 1 | session_old_store | delete |  |
| `internal/session/batch_adapters.go` | 157 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_consumers_doc.go` | 236 | 20 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_migration_test.go` | 72 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_outbox_doc.go` | 77 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_reaction_test.go` | 361 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_read_doc.go` | 38 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/batch_recovery_doc.go` | 59 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/host_lock.go` | 504 | 5 | retained_infrastructure | retain |  |
| `internal/session/host_lock_register_race_test.go` | 105 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/host_lock_test.go` | 416 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/notice_pull.go` | 544 | 54 | retained_infrastructure | retain |  |
| `internal/session/notice_pull_test.go` | 482 | 20 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/notice_row_ids.go` | 73 | 4 | retained_infrastructure | retain |  |
| `internal/session/notice_row_ids_test.go` | 145 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session.go` | 614 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session_activity.go` | 350 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session_child_question.go` | 108 | 2 | retained_infrastructure | retain |  |
| `internal/session/session_child_question_test.go` | 112 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session_child_question_void_test.go` | 86 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session_driver.go` | 204 | 5 | retained_infrastructure | retain |  |
| `internal/session/session_driver_test.go` | 336 | 6 | unknown | review (disposition must be assigned by a human) |  |
| `internal/session/session_lifecycle.go` | 204 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/background.go` | 839 | 3 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/background_buffer_test.go` | 183 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/background_cap_test.go` | 43 | 4 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/background_release_test.go` | 475 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/background_shell.go` | 310 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/exec_unix.go` | 205 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `internal/shell/exec_windows_test.go` | 154 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `web/src/asyncJobCompletion.ts` | 110 | 13 | web_completions | port |  |
| `web/src/asyncJobMetadata.test.ts` | 28 | 2 | web_completions | port |  |
| `web/src/batchWork.ts` | 129 | 1 | web_completions | port |  |
| `web/src/components/Chat.tsx` | 638 | 2 | web_completions | port |  |
| `web/src/components/LiveWorkList.tsx` | 151 | 3 | web_completions | port |  |
| `web/src/components/Message/ActionRow.tsx` | 227 | 4 | web_completions | port |  |
| `web/src/components/Message/Message.tsx` | 228 | 1 | web_completions | port |  |
| `web/src/components/Message/ToolActivityGroup.tsx` | 324 | 3 | web_completions | port |  |
| `web/src/components/SubAgentBlock.tsx` | 301 | 2 | web_completions | port |  |
| `web/src/store.ts` | 978 | 4 | web_completions | port | NEAR-LIMIT |
| `web/src/types.ts` | 424 | 5 | web_completions | port |  |
| `web/src/useWS.ts` | 563 | 1 | web_completions | port |  |
| `web/tests/async-outcome-badges.spec.ts` | 145 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `web/tests/composer-history-filter.spec.ts` | 207 | 2 | unknown | review (disposition must be assigned by a human) |  |
| `web/tests/live-work-panel.spec.ts` | 252 | 1 | unknown | review (disposition must be assigned by a human) |  |
| `web/tests/wake-schedules.spec.ts` | 242 | 1 | unknown | review (disposition must be assigned by a human) |  |

## Full uncapped hit listing (grouped by file, with line numbers)

### `.github/workflows/publish-fork-npm.yml` (1 hits)

```
288:          # install-style specifier (it tries `git ls-remote` on it) instead
```

### `docs/async-invariants.md` (28 hits)

```
38:mechanism: `asyncJobRegistry`/`subAgentOutcomeRegistry`'s in-memory-only
40:`async_jobs`/`session_notices` tables, `internal/agent/work_ledger_*.go`'s
54:| ASYNC-01 | У каждой задачи ровно один владелец; у дочерней сессии в каждый момент не больше одной активной делегации. | Owner-per-task: `AsyncJobStore.Claim`/`claimOnce` (`internal/session/async_job_store.go`) — one durable row per `(owner, tool_call_id)`, idempotent on a repeated call with the same input hash. A row that is already history (terminal `state` and `delivery` done/void) does not hold the key: a reused id (a provider that numbers calls per response) archives it to `<id>#reused#<uuid>` (`ArchiveAsyncJobToolCallID`, `isArchivableHistoryRow`) and starts a fresh row; a terminal row that is announced but not yet delivered (`delivery='pending'`, `announced=1`) is archived the same way (R2A-6: its notice stays pullable under the archive key and names the id the model used, `displayToolCallID`; pending `wake_only` check-ins follow the row, `RepointSessionNoticesJobToolCallID`); only a terminal row whose "started" result is not written yet (`announced=0`) still answers idempotently. "Not more than one active delegation per child session" is DB-enforced: partial unique index `idx_async_jobs_child_running` on `child_session_id WHERE state='running'` (migration `20260928000002_add_async_phase4_core.sql`) makes a second concurrent delegation to the same child a `claimOnce` conflict; `Claim` recovers the conflicting row's host if provably dead and retries once, else returns `ErrAsyncChildSessionBusy` **before** "started" (design doc sec.3.8, the `resume_session_id` behavior change — CHANGELOG). #1157 carve-out: an `agent` resume whose child's HELD delegation is paused on the child's own `ask_question` is answered INTO that delegation (`workLedger.answerHeldDelegation`, `internal/agent/delegation_question.go`) before `Claim` — no row is created and the index is untouched; the interception is decided by the held question, not by the call's origin (#1212: an idle web or nested parent woken by a Drain turn carries no origin; an SDK-sync X has no durable row to hold and is never intercepted), and the replayed answer turn runs with X's own origin; the phase-4 refusal stands for every other resume of a busy child. `ClaimShell` (bg_shell rows keyed by the per-process shell counter) answers idempotently only for the SAME host; a running row of ANOTHER host on the same `(owner, shell id)` is refused with `ErrBGShellIDCollision` after one dead-host recovery (#1210), and `job_kill` never cancels another host's row. | `TestAsyncJobStore_ClaimIsIdempotent`, `TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal`, `TestAsyncJobStore_ClaimReusedToolCallIDAfterDoneStartsFreshRow`, `TestAsyncJobStore_ClaimReusedToolCallIDAfterTerminalUndeliveredStartsFreshRow`, `TestAsyncJobStore_ClaimReusedToolCallIDStillRunningIsRefused`, `TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted`, `TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted`, `TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused` (all `internal/session/async_job_store_test.go`); `TestWorkLedger_StartASYNC01ConflictRefusedBeforeStarted` (`internal/agent/work_ledger_durable_test.go`). | Выполняется — both halves of the law are DB-enforced and tested directly. |
55:| ASYNC-02 | Область открыта, пока у неё есть незавершённая или недоставленная задача (кроме `loop` и надзора), идущий ход, ожидающее событие или открытая дочерняя область. CLI-хост завершается только при закрытой области корня, по `--timeout`, по отмене оператора (`sessions cancel` читается при каждом пробуждении ожидания, не реже раза в 5 с, а не только перед платным ходом) или по потолку времени по умолчанию (6 ч, мягко, как `--timeout`). Стадия 5a: открытый РАЗОВЫЙ wake-таймер (`wakein`/`wakeon`) тоже держит CLI-область открытой; активные `loop`-расписания сессии НЕ открытая работа — при закрытии области CLI (`rush run`, обычный выход) они снимаются; Ctrl-C/таймаут/`sessions cancel` их оставляет в БД | The scope answer is DB reads, not memory, plus one in-process fact -- the completion hold (`PendingCompletionsOwned`), read FIRST so a shell whose observer already committed its terminal row but whose notice is not saved yet still counts as open work (#1211): `coordinator.CLIScope` (`internal/agent/coordinator_reaction_source.go`; stage 5a: it answers `OnceWakeOpen` from `openOnceWakeSchedules`, `internal/agent/coordinator_wake_cli.go`, and the scope close cancels the session's loop schedules through `CancelLoopWakeSchedules`). It reads the running rows FIRST (`runningWorkOpen`: recovers this owner's own dead-host rows, best-effort, DUR-6, then any `running` row on a host `AsyncJobStore.HostNotDead` does not prove dead gives `WorkOpen`; a terminal transition and its debt commit atomically, so "not running" guarantees the next read sees the debt), then the pending-inclusive debt (`AsyncJobStore.ReactionDebtExists`, DUR-4), and gives that debt ONE launch verdict, `drainPermitted` (`internal/agent/coordinator_drain_policy.go`: the session policy `drainPolicy`, then the per-session launch gate the attempt accounting writes, ASYNC-09): `DrainOwed` (a Drain may launch now), `DrainPaced` (retrying after an unreacted attempt or a refusal, or held by a rerun in progress -- a hold is TEMPORARY, so it is paced with no clock (`RetryAt` zero: the re-check tick and the release retry it) and reads as "open" to every scope consumer, never as drained; the gate reopens at `RetryAt` -- 60s after a counted attempt, at which a newer fact does NOT reopen it early; after a refusal a newer fact does -- or at once on a newer fact after a refusal or a no-turn Drain but NOT after a paid failure), `DrainDeferred` (debt the policy refuses a turn on: a bg-shell-only notice with `AutoResumeOnJobDone` off, or a re-check launch (or a Drain's turn-start commit) of bg-shell-only debt every row of which is a completion that arrived over the web cap once it is spent (recorded by row id; a row whose slot was spent is retried like any debt), a released delegation child, a live foreign driver, Stop's or a pending question's suspension -- neither waited on nor settled, and the reaction chain guard's N=3 consecutive no-progress links (#1113, decide's rule 5 over the facts `readTurnFacts` computes (`internal/agent/turn_arbiter.go`); the marker executor is `coordinator.chainGuardMarker` (`internal/agent/coordinator_reaction_chain.go`): the entire pending-inclusive debt is completions of the chain's own idle launches; the CLI loop reports it through the typed `CLIScopeState.ChainGuard`, a web-driven session gets one `reaction_chain` marker notice, wake=0), `DrainStuck` (dormant gate: three no-turn Drains over a pull that keeps failing -- a human message; a newer fact reopens it -- or three paid attempts whose close kept failing -- only a human message or a restart reopens it). An unreadable policy input is a read error, not a verdict. The scope does **not** check "mid-turn": every caller evaluates it strictly between turns. Callers: (1) the `rush run` loop (`cliLoop.nextStep`, `internal/app/app_run_async.go`): Owed runs a Drain at once (also beside running work); Paced waits for the gate (`WaitForHint` until `RetryAt`) and never launches; running work is waited on; `sessions cancel` and the run's `--max-cost`/`--max-tokens` are read before every one of those waits (`cliLoop.stopError`, at least every 5s, the `WaitForHint` fallback; the flag is a ONE-SHOT request, R8A-2: a web/SDK `Run` and a delegation resuming a child start with it cleared (`coordinator.runInternal` stamps the clear as admission setup when the call has no per-call options and is not a Drain, `runSubAgent`'s resume path; `clearCancelRequest`, `WithAdmissionSetup`, `internal/agent/turn_admission.go`); #1101: the clear and `ExecuteRun`'s other preparatory writes (budget, `ended_reason`) run AT ADMISSION (`sessionAgent.runOwned` invokes the setup once the mailbox and the inter-process session lock are ours; `App.prepareAdmittedRun`, `internal/app/app_run_setup.go`), so a first turn refused by another owner's lock changes nothing about the session, and an in-turn abort that honours it (`enforceRunawayCaps`) clears it unless this process's `rush run` loop drives the session (`isExternalDriver`: the loop reads the flag after the turn and clears it when it honours it) -- a Drain never clears it at its start, so the loop keeps seeing the operator's request; `TestCancelFlag_StaleFlagDoesNotAbortAWebTurn`, `TestCancelFlag_LoopOwnedTurnsKeepTheFlag`, `TestCancelFlag_InTurnAbortHonoursAndClearsIt`, `TestCancelFlag_LoopDrivenSessionKeepsItAfterTheAbort`, `TestRunSubAgent_ResumeClearsAStaleCancelFlag`, `TestCancelFlag_HumanTurnClearsTheFlagAtAdmission` in `internal/agent/cancel_flag_lifecycle_test.go`; `TestRunNonInteractive_FirstTurnLockBusy_LeavesSessionStateUntouched`, `TestRunNonInteractive_AdmittedRunAppliesSetup` in `internal/app/app_run_lockbusy_admission_test.go`); one session-row read per wake when a cap is set (the row carries the cancel flag too), otherwise the cheap flag read alone, so a cancel ends the run "canceled" and a crossed cap ends it "error", both through `exitPrecheck` (envelope, `ended_reason`, `--on-finish`; `App.Shutdown` cancels the run's jobs as Ctrl-C does) while the awaited work is still running, not hours later at the next Drain (`stepCanceled`, R7C-1, R8B-3: what the check sees is the SUBTREE budget (#1130), so a delegated child's spend is visible to the next wake the moment the child charges its own row -- one in-flight child turn can still overshoot, exactly as with the in-turn caps); Deferred and Stuck end the run only when nothing is running (deferred: the notice stays for the next human turn, one stderr line, exit reason unaffected; stuck: an error naming the debt), so a refused-debt-only scope closes for the loop instead of spinning until `--timeout`. The loop never accounts an attempt (the turn loop that ran the Drain does, ASYNC-09); its Drain iterations are mutation-free (`RunRequest.drainTurn`: they write none of the invocation's session setup -- system prompt, reasoning effort, model slots, cancel flag, budget, `ended_reason`, title -- and run no reviewer and no hook), and the first turn is the only one that does: a setup failure before it is launched (`RunRequest.onTurnSubmitted`) ends the run with that error instead of letting a later Drain clear it; a Drain that committed right before Ctrl-C/`--timeout` keeps its answer as the run's final text and the run exits canceled; one stderr line per failed paced attempt names the error and `RetryAt`; a closed scope ends the refusal/failure streak (`refusalSince`, `failedAttempt`, `pacedNoticeAt` are cleared when `nextStep` returns `stepExit`), so a Drain refused after the reviewer turn starts a fresh `cliLockBusyRetryOverallLimit` budget with its own "retrying" line; `ended_reason` is EMPTY while a run is in progress and the run's exit reason afterwards: every turn the loop issues (`RunRequest.loopTurn`: first turn, Drains, reviewer pass) skips `ExecuteRun`'s per-turn write, `SetEndedReason("")` at run start stays, non-loop callers (the SDK, `ExecuteRun` directly) keep the per-turn write, and a crash (kill -9) leaves it empty (R8A-1; `sessions show` / `sessions list --json` read it through `Sessions.Get`); every loop exit (Ctrl-C/`--timeout`, stuck debt, a cap or operator cancel at the precheck or while waiting, an unreadable DB, the ordinary close) writes the envelope's `exit_reason` once -- the ONLY writer for a loop run (`cliLoop.persistEndedReason`, `internal/app/app_run_async_ended.go`: on a context detached from the run's, skipped when the row already holds the value, derived from the error when the launched first turn returned no envelope, nothing written for a first turn refused by another owner of the session; R6C-6, R8A-1); a cancel request the loop honours -- the run ends "canceled" -- is one-shot and is cleared before the exit flush (`cliLoop.clearHonouredCancel`, detached context, best effort, R8A-2), so a later web turn or takeover Drain on the session is not aborted after its first step; Drain turns never clear it, and a run that ends for another reason leaves it to its owner; the envelope lists each sub-agent once (latest text) and keeps a `final_text` warning only for the turn that is the answer; the envelope's cost is the SUBTREE spend delta between the driver claim and the exit (#1130: `cliLoop.sessionUsage`/`applyTotals` read `Sessions.SubtreeSpent` over the delegation tree -- `cost_self` sums, migration `20261001000001_run_cost_subtree.sql` -- at the claim and at the exit; a child charges its OWN `cost_self` via `IncrementCost` the moment it spends, so the window needs no transfer moments -- the `parent_cost_accounted` ledger and the `chargeRunningChildren`/`chargeChildToParent`/`TransferChildCostToParent` machinery are REMOVED -- and a normal exit and a cancel exit report the same cost) and a human turn's spend on the root is counted the same way, as is the spend so far of a delegation still running at a Ctrl-C/`--timeout`/cap exit; the non-loop envelope and `RUSH_COST_USD` (`ExecuteRun`'s `hookCost`/`deltaCost`, `internal/app/app_run.go`, `app_run_reviewer.go`) window over the same query; tokens stay a per-turn last-snapshot sum; a DB read error is a retry with a pause bounded by `cliDBErrorRetryOverallLimit`, never a silent exit; on Unix `rush run` turns SIGPIPE into an EPIPE error (`keepBrokenPipeAsError`, `internal/cmd/run_sigpipe_unix.go`: `signal.Notify`, not `Ignore`, so spawned jobs keep the default disposition), so a consumer that closed stdout fails the exit flush through `flushErr` -- the `--on-finish` hook (a deferred call, run after the flush) and `App.Shutdown` (the job process groups) still run instead of the process dying by SIGPIPE at the envelope write (R8C-7; Windows returns the write error already). (2) The automatic reviewer pass. In the `rush run` loop it runs at most ONCE per invocation, at the first scope-closed exit after a clean last turn (`cliLoop.scopeClosed`, `RunRequest.reviewerTurn`: an ordinary turn, no Drain marker on its context, mutation-free setup), so it reviews the FINAL text, reaction turns included; the loop's own turns set `deferReviewer`. The reviewer turn may start async work of its own (every CLI `bash` is a job), so a clean review turn sends the loop back to `nextStep` with `cliLoop.reviewerDone` set: running work is waited on and Owed debt is Drained on the reviewer's call options and model (`RunRequest.reviewerConfig`: `buildReviewerPassTurn`, sub-agents off, no model persistence; `reviewerCallOptions` copies every field of the run's `CallOptions` except `ModelRole` and `DisableSubAgents`, so `--no-supervision`/`--supervision-interval` cover the reviewer's jobs and their check-ins), the reviewer never runs again, and -- A10 (#1146) -- `final_text` stays the executor's last COMPLETED answer while the reviewer's verdict lands in the envelope's additive `review` field (a reaction Drain that runs after the review only settles usage and errors; it never replaces the reviewed answer -- C9-16); caps are checked before every paid turn, Ctrl-C/`--timeout` end the run canceled with that answer, Stuck debt the review left is the loop's error exit and Deferred debt an ordinary exit; a failed review ends the run at once (an error exit like a cap exit: work it started is cancelled with the run), while a review that queued behind another owner or was refused by the session lock's holder (`turnRefusedByOwner`: a web tab's or `sessions inject`'s turn) ran nothing -- the run keeps its last answer, exits clean, says so on stderr, and the refused turn writes no `ended_reason` (R6C-4). A Drain never runs it (`!req.drainTurn` in `reviewerCandidate`). For callers without the loop (SDK) `reviewerPassBlocked` (`internal/app/app_run_gates.go`, writing to the run's own `Stderr`) blocks it on running work and on Owed/Paced/Stuck debt; debt the policy defers does not block it. (3) A delegation's release (`workLedger.childScopeDrained` -> `childScopeOpenAcrossProcesses`, `internal/agent/work_ledger_delegation.go`) reads the same answer for the child: open while the child has running work or debt it owes or is retrying (Owed/Paced) -- a background shell counts as work until its `bg_shell_done` notice is DURABLE, not only until its process exits: the shell manager keeps a completion hold per shell (`BackgroundShellManager.PendingCompletionsOwned`, `internal/shell/background_completion.go`), taken when the completion callback is registered (`OnDone`, under `retentionMu` with the callback count), independent of the job table, and released by `BackgroundShell.MarkCompletionRecorded` (`notifyBackgroundJobDone`, right after `persistBGShellCompletion` returned: the row committed, or the insert failed and nothing more will be written), by the last callback returning (a panic included), or 10 minutes after the process finished, or, for a shell taken out of the job table (Kill/Remove/Close) whose process never exits (a descendant holds the output pipe, so `done` stays open; `BackgroundShell.detachedAt`), 10 minutes after it was detached (a wedged callback or a killed shell that never exits never holds a scope forever, R8B-2); the callback's own re-check runs after the release, so a re-check between the process exit and the commit (the 60s pass, a release hook) no longer releases the delegation with the child's stale text (R7B-1, amendment (ak).1); a question the child asked, Stop or a foreign driver (Deferred) and Stuck debt release the delegation, because the child would get no turn for it and waiting would hang the parent (a child's question reaches the parent as the delegation's result, `subAgentQuestionFromFinish` -- also when the turn wrote text first, which is kept as the question's preamble and never turns the delegation into a failure: `refreshSubAgentCompletion` checks the question before the text branch, `runSubAgent`'s first-turn response carries the same preamble; stuck rows stay `pending` and visible in `sessions why`); a DB error reads as "open". A child's delegation row stays `running` until its own scope closes, so parent-open-while-child-open is structural (one DB row per level), not a bounded in-memory walk. `rush run` also runs the 60s re-check pass (`ClaimExternalDriver` starts the ticker; `StopRecheckTicker`/`CancelAll` stop it), so a failed delegated child's reaction turn is retried in the CLI as it is in web. `--timeout` and the default wall-clock cap (6 h; `RUSH_RUN_DEFAULT_HARD_TIMEOUT` overrides it; no `--timeout` set) are the same GRACEFUL context deadline (`installRunDeadline`, `internal/cmd/run_deadline.go`; the cap tags its deadline with the cancellation cause `agent.ErrRunDefaultCap`, so a turn it cuts off says "default wall-clock cap" in its finish text instead of a `--timeout` nobody passed, `runTimeoutFinishText`, R7C-5; the cause wraps `context.DeadlineExceeded`, because net/http HTTP/1.1 returns `context.Cause(ctx)` itself from a cut stream and that must classify as a deadline (`isRunTimeout`, `operatorStop`) exactly like the same cut by `--timeout`, R8B-1): a loop that is waiting ends through `exitWait` with an envelope (exit reason `canceled`, the deadline error) and `App.Shutdown` runs; only a process that has not exited 60 s past that deadline is force-killed (exit 124). The cap is not re-armed on progress: nothing outside the loop tells a healthy wait from a wedged process (a wedged one still heartbeats from other goroutines), so a wait longer than the cap needs an explicit `--timeout`. The status surfaces (`sessions why`, `sessions list`, the web session list) read the same fact for a loop between turns: a live `rush run` loop waiting on a paced retry holds no session lock and has no running row, so they read the durable driver marker (`session_drivers`; `App.LiveSessionDrivers`, `internal/app/app_session_drivers.go`: ONE read on the read pool for a whole list, host liveness through `HostLiveness`, unknown counts as alive, a dead host's marker is ignored) and report the session `running` (`sessions why` names the loop's PID and whether a reaction is owed; the web list sets `HasLiveOwnWork`) instead of at rest/done -- never downgrading a crash. The lock is not the liveness fact for a loop between turns (its file is truncated on release, so it reads as an aged, PID-less leftover): `sessions watch` and `sessions tail --follow` do not end while the driver marker, a running own job or a live delegation exists (`App.SessionActivity` facts, R-ACT; `inspectSessionLiveWork` удалён; the wait is named on stderr; a read failure counts as live, and a marker/job/delegation released ends the watch at once), and `sessions locks` shows such a lock as `between turns (rush run PID n)` (JSON `between_turns`, `driver_pid`; not stale, and `--prune` leaves it alone) instead of `offline`; the lock file is named by the SANITISED id (`/ \ : * ? " < > |` and space become `_`) while the marker is keyed by the real one, so `sessions locks` matches by the file stem (`LockStems`, the reverse stem→ids map of the R-ACT batch result, `internal/app/app_session_activity.go`: colliding ids share one file, an empty file belongs to the lowest-PID candidate, a recorded PID only to the loop running under it, so a dead colliding session's PID never reads as a live loop's; R7C-2, post R-ACT the per-command `driversByLockName`/`lockDriver` maps are removed), and looks the sub-agent pulse up by the REAL id (`lockStemIDs` from the session list and the markers, `lockCallTreeActivity`: the freshest of the ids sharing a stem), so a slug id such as `fix/login-timeout` gets its `sub_agent` like a plain one; `sessions locks <id>` (id or hash prefix, else a literal id/stem) lists only that lock (R8C-8, `TestSessionsLocksCmdRun_SlugIdGetsSubAgentPulseAndIdFilter`). `sessions reap` keeps every lock file that records no PID (a clean release: a loop or a web session between turns), unprobed, and still reclaims a crash's PID-recording orphan (R8C-5, `TestSessionsReap_ReleasedLockIsKeptCrashLockIsReaped`). `sessions cancel --all` flags only sessions with live work (`ClassifySessionActivity` live-work facts: a live lock, a driver marker, an own running job, a live delegation; idle ones are skipped and counted on stderr), because the flag is a one-shot request and one left on an idle session would abort its next turn; a named id still flags (R8A-2, `TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork`). `sessions inject` counts a driver marker as running (`running:true`, `between_turns:true`, `driver_pid`; the stderr line names the loop and says no turn is running to interrupt, R7C-3), also counts a marker-less session's own running job or live delegation (a web session between turns; the stderr line names what keeps it open, the R-ACT live-work fact (`live.active()` of `App.SessionActivity`), R8C-4, `TestSessionsInjectCmdRun_OwnJobOrDelegationBetweenTurnsIsRunning`), and `sessions tail` resolves its argument (id or HASH) once and uses the resolved id for the message list and the finish check (R7C-6). A lock that is such a clean-release leftover (an existing file with no recorded PID, or one naming the live driver itself) is never a crash while live work exists: `sessions list`/`why` report `running` (driver, own job) or `delegating` whatever the last finish is, including an error finish after a failed reaction turn (`promoteCleanReleaseCrashes`); `crashed` stays for a recorded dead PID that is not the driver's, for a missing lock file, and for a session with no live work. `HostNotDead` answers through `HostLiveness` (the shared, non-acquiring `ProbeHostShared`), so a concurrent reader's shared hold on a crashed host's file never reads as "alive"; only the recovery paths that must WIN the lock use the exclusive `ProbeHost`. | `TestCLIScope_RecoversDeadHostRowBeforeAnswering`, `TestCLIScope_LiveHostRunningRowKeepsScopeOpen` (`internal/agent/coordinator_scope_recovery_test.go`); `TestCLIScope_SplitsDebtByPolicy`, `TestCLIScope_ForeignLiveDriver_DebtDeferred`, `TestCLIScope_LaunchGateStates` (`internal/agent/coordinator_cli_scope_test.go`); `TestHostNotDead_ConcurrentSharedProberDoesNotFlipVerdict` (`internal/session/async_job_reaction_test.go`); `TestChildScopeDrained_RunningRowOnLiveOtherHost_NotDrained`, `TestChildScopeDrained_TerminalRowWithUnreactedDebt_NotDrained`, `TestChildScopeDrained_NothingPending_Drained` (`internal/agent/coordinator_child_policy_durable_test.go`); `TestChildScope_DeferredDebtReleasesDelegation`, `TestChildScope_RerunHoldKeepsDelegationOpen`, `TestChildDrainQuestion_ReachesParent` (the child's text comes before its question), `TestRunSubAgent_FirstTurnQuestionKeepsPreamble` (`internal/agent/coordinator_child_question_test.go`); `TestClaimExternalDriver_StartsRecheckTicker` (`internal/agent/coordinator_external_driver_durable_test.go`); `TestNextStep_DecisionTable`, `TestNextStep_PersistentDBError_BoundedWithVisibleError`, `TestRunNonInteractive_ReviewerRunsWithDeferredDebt`, `TestRunNonInteractive_ChildFailedDrainRetriedByTick`, `TestRunNonInteractive_BusyFirstTurnJSONEnvelopeAndHook` (`internal/app/app_run_loop_more_test.go`); `TestRunNonInteractive_PermanentPullFailure_ExitsStuck`, `TestRunNonInteractive_FailedDrainPacedThenSettled`, `TestRunNonInteractive_FormerDelegationChildReacts` (`internal/app/app_run_loop_test.go`); `TestRunNonInteractive_ReviewerRunsOnceAfterDrainOnFinalText`, `TestExecuteRun_DrainTurnNeverRunsTheReviewerPass`, `TestRunNonInteractive_FirstTurnFailsBeforeSubmission_EndsTheRun`, `TestRunNonInteractive_DrainCommittedBeforeCancel_KeepsItsAnswer`, `TestRunNonInteractive_DrainTurnsMutationFree`, `TestRunNonInteractive_DrainKeepsCancelRequest`, `TestRunNonInteractive_CancelRequestBeforeDrainEndsRun`, `TestRunNonInteractive_FailedDrainPrintsOneLinePerAttempt`, `TestCLILoop_QueuedIterationUsageIsFoldedIntoTheRun`, `TestLoopTotals_DedupesSubAgentsAndDropsSupersededFinalTextWarnings` (`internal/app/app_run_loop_round3_test.go`); `TestRunLoop_ReviewerPassKeepsExecutorFinalText`, `TestExecuteRunTerseReviewerPassPrintsOnlyFinalResult`, `TestReviewerPass_FailureKeepsPrimaryAnswer`, `TestRunNonInteractive_ReviewerRunsWithDeferredDebt`, `TestReviewerPassBlocked_ScopeDecides` (`internal/app/app_run_loop_round4_test.go`); `TestRunNonInteractive_CostIncludesAsyncChildSpentBetweenTurns`, `TestRunNonInteractive_MaxCostExitEnvelopeReportsTheChildSpend`, `TestRunNonInteractive_CostIncludesForeignSpendOnTheRoot` (`internal/app/app_run_loop_round4_cost_test.go`); `TestReviewerCallOptionsCarryEveryPrimaryField`, `TestExecuteRunReviewerPassTurnUsesReviewerCallOptions`, `TestReviewerCallOptionsCarryEveryPrimaryField`, `TestRunNonInteractive_FailedDrainPrintsOneLinePerAttempt`, `TestRunNonInteractive_CancelExitCostIncludesRunningChildSpend` (cancel, then the child's own charge or Shutdown: the root gets the child's spend once), `TestChargeRunningChildren_NestedDelegationsBottomUpAndIdempotent` (`internal/app/app_run_loop_round5_test.go`); `TestRunLoop_ReviewerRefusedByLockKeepsAnswer`, `TestRunLoop_TimeoutWhileWaitingWritesEnvelopeReason`, `TestRunLoop_BetweenTurnsExitsWriteEnvelopeReason` (`internal/app/app_run_loop_round6_test.go`); `TestRunLoop_EndedReasonStaysEmptyWhileTheLoopWaits`, `TestRunLoop_OnlyTheExitWritesEndedReason`, `TestRunLoop_ReviewerPassWritesEndedReasonOnlyAtTheExit`, `TestExecuteRun_NonLoopCallerKeepsThePerTurnEndedReason` (non-loop callers keep the per-turn write), `TestRunLoop_MaxCostWhileWaitingEndsTheRun` (Paced and running-work waits), `TestRunLoop_HonouredCancelIsCleared`, `TestRunLoop_CancelDuringFirstTurnEndsCanceledAndClearsTheFlag`, `TestRunLoop_LockBusyRefusalLeavesTheOwnersSessionAlone`, `TestRunLoop_ExitFlushOnClosedPipeStillRunsTheHook` (`internal/app/app_run_loop_round8_test.go`); `TestPersistEndedReason_NoEnvelopeDerivesTheReasonFromTheError`, `TestStopError_OneReadPerWake` (`internal/app/app_run_loop_round8_unit_test.go`); `TestKeepBrokenPipeAsError` (Unix only, a real child process on a closed pipe: `internal/cmd/run_sigpipe_unix_test.go`); `TestLiveSessionDrivers_OwnSiblingAndDeadHosts` (`internal/app/app_session_drivers_test.go`); `TestExplainSessionStatus_LiveRunDriverBetweenTurnsIsRunning`, `TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed`, `TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone` (`internal/cmd/sessions_run_driver_status_test.go`); the between-turns and driver shapes (a driver marker, an own running job, a live delegation, a dead host — one verdict table for every reader, post R-ACT the per-reader `isSessionFinished`/`tailSessionFinished`/`promoteCleanReleaseCrashes`/`markLiveRunDrivers` layers are removed): `TestListStatus_DriverShapes`, `TestListStatus_OwnJobShapes` (`internal/cmd/sessions_run_driver_status_test.go`, `internal/cmd/sessions_own_job_status_test.go`), `TestSessionsActivity_OneTableEveryCommand`, `TestVerdictPulse` (`internal/cmd/sessions_activity_cmd_test.go`), `TestExplainSessionStatus_LiveRunDriverBetweenTurnsIsRunning`, `TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed`... `TestSessionsInjectCmdRun_LiveLoopBetweenTurnsIsRunning`, `TestSessionsTailCmdRun_ByHashPrintsMessagesAndFollowEnds`, `TestSessionsTailCmdRun_FollowInterruptedExitsClean`, `TestRunLoop_CancelWhileWaitingOnRunningWorkEndsCanceled`, `TestRunLoop_CancelWhilePacedEndsCanceled` (`internal/cmd/sessions_wake_livework_test.go`); the wall-clock cap: `TestInstallRunDeadline_DefaultCapIsAGracefulDeadline`, `TestInstallRunDeadline_HardKillOnlyPastTheGrace`, `TestInstallRunDeadline_TimeoutFlagUnchanged` (`internal/cmd/run_deadline_test.go`), `TestRun_RunTimeoutFinishNamesItsSource` (`internal/agent/run_timeout_test.go`: both sources, each against a model that returns `ctx.Err()` and one that returns `context.Cause(ctx)`), `TestOperatorStop_DefaultCapIsADeadline` (same file), `TestRunNonInteractive_FirstTurnDeadline_FlushesCanceledEnvelope` (`internal/app/app_run_first_turn_deadline_test.go`); `TestHandleListSessions_LiveRunDriverAnnotation` (`internal/server/run_driver_liveness_test.go`); `TestResolveSession_UseLastSkipsChildSessions` (`internal/app/app_run_resolve_last_test.go`); `TestExecuteRun_ReviewerPassSkipLineGoesToRequestStderr`, `TestRealApp_CoordinatorExposesEveryOptionalInterface`, `TestRunNonInteractive_CLIRunOnCoordinatorWithoutReactionSourceFails` (`internal/app/app_run_round4_misc_test.go`); `TestFoldRoleModel_ContinueNeverResolvesAChildSession` (`internal/cmd/run_last_session_test.go`); `TestReviewerPassBlocked_ScopeDecides`, `TestReviewerPassBlocked_RecoversWithinRetryBudget`, `TestReviewerPassBlocked_PersistentFailureDefaultsToBlocked` (`internal/app/app_run_reviewer_scope_retry_test.go`); `TestRunNonInteractive_RefusedBGShellDebtEndsTheLoop`, `TestRunNonInteractive_AutoResumeOnBGShellDebtStillReacts` (`internal/app/app_run_deferred_debt_test.go`); `TestRunNonInteractive_CancelAfterSuccessfulTurnReturnsCancellation` (`internal/app/app_run_cancel_after_turn_test.go`: a cancel landing after a clean turn returns the cancellation error); root-waits-for-child: `TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`, `TestRunNonInteractiveWaitsForAsyncSubAgentResult`, `TestRunNonInteractiveContinuesPastFiveAsyncCompletions`. | Выполняется for depth 1 (proven end-to-end) and structurally for depth > 1 (each level's own row, proven individually); a live black-box grandchild scenario through `rush run` cannot be built: the `task` agent's read-only tool set has no `agent` tool (`resolveReadOnlyTools`, `internal/config/config.go`), so a child never delegates further in a real run. The debt split and the launch-gate states are unit-tested directly (`TestCLIScope_SplitsDebtByPolicy`, `TestCLIScope_LaunchGateStates`). A child's spend is visible to the parent's SUBTREE budget the moment it charges its own row (COST-1; no transfer moments): `TestDelegationRelease_ChildSpendVisibleToTheParentBudget`, `TestChildRunEnd_ReleaseChargesNothing` (`internal/agent/coordinator_child_cost_test.go`), `TestRunNonInteractive_CostIncludesAsyncChildSpentBetweenTurns` (`internal/app/app_run_loop_round4_cost_test.go`), `TestSubtreeSpent_CycleTerminatesAndCountsOnce` (`internal/session/session_cost_subtree_cycle_test.go`); a parked delegation whose child's shell finishes is released with auto-resume off, over the cap and in the CLI: `TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays` (`internal/agent/coordinator_bgshell_child_test.go`); a shell that finished but whose notice is not durable yet keeps the delegation parked (callback not entered, callback waiting on `bgArrival`, shell still in the job table) and the callback's own re-check runs after the hold drops: `TestChildScope_FinishedShellStaysOpenUntilItsNoticeCommits`, `TestBGShellDone_OwnRecheckRunsAfterTheHoldDrops` (`internal/agent/coordinator_bgshell_window_test.go`); the hold itself: `TestCompletionHold_HeldFromRegistrationUntilTheCallbackReturns`, `TestCompletionHold_PanickingCallbackReleases`, `TestCompletionHold_RecordedEndsTheHoldBeforeTheCallbackReturns`, `TestCompletionHold_FinishedShellCountsForAtMostTheBound`, `TestCompletionHold_OnlyRegisteredShellsAndLastCallbackCount`, `TestCompletionHold_HandBuiltShellIsHarmless`, `TestCompletionHold_ShutdownReleasesEveryHold`, `TestCompletionHold_KilledShellThatNeverExitsExpires` (a shell whose `done` never closes, every removal path: Kill, KillOwned, Remove, RemoveOwned, Close, KillAll), `TestCompletionHold_RunningAttachedShellNeverExpires` (`internal/shell/background_completion_test.go`). `loop`/надзор scoping unchanged from phase 3. Stage 5a wake-hold: `TestRunNonInteractive_OnceWakeHoldsLoopAndReacts`, `TestRunNonInteractive_TimeoutDuringWakeWaitIsCanceled`, `TestRunNonInteractive_LoopScheduleCancelledAtRunEnd`, `TestRunNonInteractive_LoopBesideRunningJobCancelledAtClose`, `TestRunNonInteractive_SessionsCancelDuringWakeWait` (`internal/app/app_run_wake_hold_e2e_test.go`); readers: `TestExplainSessionStatus_OpenWakeScheduleHoldsSessionOpen`, `TestExplainSessionStatus_OpenWakeScheduleHoldsSessionOpen`, `TestExplainSessionStatus_LoopScheduleIsNotLiveWork`, `TestCancelWakeSchedules_CancelsActive` (`internal/cmd/sessions_wake_livework_test.go`). |
57:| ASYNC-03 | Задача достигает терминального состояния ровно один раз; гонка finish/stop/timeout/cancel/interrupted даёт один исход. | The CAS lives in the DB: `workLedger.transition`/`commitTransition` (`internal/agent/work_ledger_transition.go`) is the only caller of `AsyncJobStore.Transition` (`internal/session/async_job_store.go`), itself the ONE terminal-state SQL CAS (`UPDATE ... WHERE state='running' AND claim_id=?`, DUR-1/DUR-2; `claim_id` closes the deleted-then-reclaimed ABA; the in-process half of that ABA — an ack-abort deletes job1, a repeated same-id call starts job2, executor 1 returns late — is closed by executor identity: `finish`/`armDelegation`/`commitTransition`/`setShellID` take the executor's own `*asyncJob` and ignore one that is no longer the key's entry). Every terminal-transition call site (finish, `handleTimeout`, job_kill, `cancelSession`, `recheckChild`) funnels through `transition`; the loser re-reads and adopts the committed row instead of retrying its own cause (`job_kill` answers from that committed row, `commitAndDeliver`). A bg_shell kill and the shell's completion callbacks are serialised on `coordinator.bgArrival` (B9-7, #1219): `StopBackgroundShellRow` decides under the gate and, when the process has already exited, defers to the natural completion (`AlreadyTerminal` with the real output); a won cancel releases the shell's completion hold at once (B9-6); `persistBGShellCompletion` re-checks the cancelled row under the same gate before inserting. The gate's own exclusion window (between the kill's `IsDone` check and its commit) has no deterministic test; the `IsDone` deferral and the under-gate cancelled check are each pinned by a mutant-red test (`TestStopBackgroundShellRow_NaturalExitFirstDeliversExactlyOneOutcome`, `TestPersistBGShellCompletion_CancelledRowInsertsNothing`). | `TestWorkLedger_WebCallbackExactlyOnce`, `TestWorkLedger_ConcurrentFinishAndAcknowledge`, `TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome`, `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome`, `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome`, `TestWorkLedger_MarkJobStopped_AlreadyTerminalAnswersFromCommittedRow` (in-process races); `TestAsyncJobStore_TransitionWonThenLostThenGone`, `TestAsyncJobStore_TransitionPreservesVoidDelivery`, `TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure` (DB CAS backstop, external writer), `TestAsyncTool_StaleExecutorAfterAckAbortCannotCommitOntoReclaimedKey` and `TestWorkLedger_SupersededExecutorWritesNothingByKey` (`internal/agent/work_ledger_identity_test.go`, the real Start→abort→Start→late-finish sequence); `TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow` (cross-process race via recovery, end-to-end outcome) and `TestRecoverDeadHost_ConcurrentRecoverers_OneEffectPerRow` (six recoverers barriered past the probe with the same listing: since R3A-3 recovery holds no lock, so the CAS alone gives one transition per row); `TestRecoverDeadHost_StaleListing_AnnouncedRow_KeepsLiveReclaim` (the claim guard is load-bearing: a stale recoverer's `Transition` keyed by the claim it listed loses to a live host's re-claim of the same `tool_call_id`). | Выполняется. |
58:| ASYNC-04 | Каждая задача, о старте которой сообщено модели, доставляется владельцу ровно один раз (A14: задача, завершившаяся естественным исходом внутри окна немедленного ответа, доставляется В САМОМ tool result — `async:false, inline:true`; Tx2 `AnnounceInlineResult` закрывает `announced=1` и `delivery='done'` одной транзакцией) — в том числе отменённая, просроченная и прерванная смертью хоста. | Ack gate = the durable DUR-7 fusion: `workLedger.acknowledgeWithMessageTx` (`internal/agent/work_ledger_announce.go`) + `AsyncJobStore.AnnounceStarted` (`internal/session/notice_pull.go`) commit "started" and `announced=1` in ONE transaction; `onToolResult` (`internal/agent/agent_turn_stream.go`) is the call site. Delivery to the owner is the driver's pull (DUR-3, `internal/agent/agent_notice_pull.go`) — every terminal cause, including host death (recovery marks `interrupted`, DUR-6) and Stop (`cancelled`, `wake=0`), produces a `delivery='pending'` row the SAME pull mechanism picks up on the owner's next turn; a stopped plain job's "cancelled" notice is therefore delivered exactly once, at the next turn start or step boundary, without waking anyone. Sync (SDK) jobs are unchanged: `deliverLocked`'s sync branch, never DB-backed. | `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction`, `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`, `TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck`, `TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate`, `TestWorkLedger_StopBeforeAckDoesNotOrphanRow` (`internal/agent`); host-death delivery: `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted` (`internal/app`). | Выполняется — delivery depth remains 1 hop per delegation link, transitively structural as ASYNC-02 describes. |
59:| ASYNC-05 | Уведомление никогда не опережает сохранённый tool result, объявивший задачу. | DB-enforced directly: every pull query requires `announced=1` (`ListPendingAsyncJobNoticesForOwner`, `internal/db/sql/async_jobs.sql`, DUR-7); an unannounced row from a dead host -- running or already terminal (R2A-7) -- is deleted without a trace by recovery rather than ever surfacing (DUR-6). | `TestPullJobNotices_InsertsMessageWithNoticeInvariant` (announced row pulled correctly); `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`; `TestRecoverDeadHost_UnannouncedDeleted`, `TestSweepDeadHosts_DeletesTerminalUnannouncedRowsOfDeadHost` (`internal/session/async_job_recovery_test.go`); `TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck` (`internal/app`: two-App, host dies before ack, row deleted, no notice ever appears). | Выполняется. |
61:| ASYNC-07 | Маршрут доставки и пробуждения не зависит от origin и глубины сессии. | `deliverLocked`'s CLI/web branch and its sync-job short-circuit are unchanged from phase 2; phase 4 adds that the terminal cause itself (natural finish, timeout, job_kill, cancel, dead-host recovery) is computed once by `causeStateNoticeKindWake` (`internal/agent/work_ledger_transition.go`) and committed by the SAME `Transition` CAS regardless of origin — a delegation, a CLI bash job, and a dead-host recovery all produce the same `delivery='pending'` row shape (job_kill: `done`) the ONE pull mechanism (DUR-3) picks up. | `TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone`, `TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout` (mode independence, phase 2, unedited); `TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage` (route independent of which process's driver pulls it). | Частично — origin/mode independence and host-death independence are proven; transitive depth > 1 delivery remains untested end-to-end (same caveat as ASYNC-02/04). |
62:| ASYNC-08 | Отмена области отменяет её задачи и дочерние области; прерывание хода (interrupt) задачи не отменяет. | Cancellation is transitive across the live delegation tree via the DB: `coordinator.Cancel` = `stopTree` (`internal/agent/coordinator_interrupt.go`) calls `workLedger.cancelTree` (`internal/agent/work_ledger_delegation.go`) to walk every still-`running` delegation row from `sessionID` down, cancels each session's own driver via `agentFor`, suspends automatic turns for each id (`suspendAutoResume`) and durably zeroes `wake` on every pending/done row of the whole tree (`AsyncJobStore.SetWakeZeroForOwners`, `internal/session/async_job_reaction.go`, DUR-9). Rerun does NOT use it: `Coordinator.CancelTurn` cancels only the live generation, and only the deleted tail's jobs are stopped (`StopRerunJobs`, after the Rerun transaction commits). `context.WithoutCancel`/`context.WithCancel`'s async/sync split (interrupt does not cancel a job) is unchanged from phase 2. | `TestStop_OneSecondAfterNaturalFinish_NoNewTurn`, `TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere` (`internal/agent/coordinator_stop_tree_test.go`); `TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows` (`internal/session/async_job_reaction_test.go`: asserts both tables' `wake`, a running row untouched, and an out-of-scope owner untouched); `TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent` (phase 2, unedited); `TestAsyncTool_SyncCallerCtxCancelUnblocksAwait` (interrupt-does-not-cancel, sync side). | Выполняется — the whole-tree half is proven at N>1 children; the interrupt-does-not-cancel half's async/CLI-detach direction remains proven by code reading, not a black-box test (both available triggers are too timing-fragile to be worth a flaky test over a property already guaranteed by `context.WithoutCancel` at construction). |
63:| ASYNC-09 | Неудачная доставка или пробуждение становятся видимым состоянием области, а не записью в Debug-логе. | A failed wake is visible state at every stage, decided at ONE accounting point: the turn loop that ran a Drain leg (`sessionAgent.closeDrainAttempt` -> `coordinator.accountDrainAttempt`, `internal/agent/drain_attempt.go`), exactly once per leg, after the leg ends and before the mailbox is released, from the DB's own verdict on the snapshot the attempt could see (taken after the Drain's own pull; the hint counter is read before it, so a fact that lands mid-attempt is never absorbed by it). Launchers never account: a queued or merged Drain has no launcher, and the release hook fires inside `Run`. A leg that reached the provider (`agent.Stream` was called) is a COUNTED attempt: `IncrementWakeAttempts` on exactly the snapshot rows, then each snapshot row's OWN counter (`MaxWakeAttempts` on that one row): no row still debt (every visible row reacted) resets the gate; rows whose own counter reached K=3 (`drainFailureSettleThreshold`) are closed by failure -- ONLY those: a notice that arrived later keeps its own clock, so a 1-minute outage never closes it -- and the gate stays paced while other rows remain; a terminal provider classification at the FIRST attempt (quota/402/other 4xx, or a 401 whose credential refresh is not possible; peak hours is a window, not terminal) closes every row of the snapshot; a context-window overflow (the provider's flagged context-size error, a 413, or a 400 whose message names the overflow: `isContextOverflow`, `internal/agent/drain_settle_text.go`) writes a marker that names the cause and the remedy (summarize the session or start a new one) instead of "continuation at the next turn", because the events stay in history and every later request fails the same way (R8B-5; a per-pull token budget was rejected: the estimate needs the live history size the failed request never updates, and a partial pull changes the exactly-once and debt semantics). A 401 is refreshed at this very point (`coordinator.refreshAfterUnauthorized`: OAuth token or API-key template) on its OWN bounded context (`drainRefreshBudget`, 10s, detached from the accounting's 30s), so a black-holed auth endpoint cannot starve the settle that follows: a refresh that succeeds makes the 401 a counted, paced transient (an expired token after a long job), one that times out or fails leaves the 401 terminal and the row is closed with its marker at once. The next attempt runs on the new credentials because `drainCallFor` also refreshes an expired token BEFORE a web or child Drain and rebuilds the call's client on it (a delegated child's frozen client is rebuilt for an OAuth provider AND for an API-key `$` template provider, `freshenDrainClient`; per-call `Credentials` are never replaced). `coordinator.settleDrainDebt` calls `AsyncJobStore.SettleReactedFailedWithMarker`, which settles the closing rows and inserts the `wake_failed` session notice (`session.NoticeKindWakeFailed`, `wake=0`, naming the snapshot's own tool_call_ids) in ONE transaction, the marker only if a row was actually settled; otherwise the gate is paced. What counts: an error, an empty stream, a reaction write that never landed (fantasy swallows `OnStepFinish` errors; the DB verdict catches it), a watchdog stall and a cap abort (both surface as `context.Canceled` but are real, paid attempts: `drainAttempt.stalled`/`capAbort`). What does not: an operator stop -- a cancellation, or a deadline of the TURN's own context (Stop, Ctrl-C, `--timeout`, the default wall-clock cap, interrupt, `sessions cancel`, shutdown; `drainAttemptExempt`/`operatorStop`; the cap's cause `agent.ErrRunDefaultCap` wraps `context.DeadlineExceeded`, so a stream the HTTP/1.1 transport cuts with `context.Cause(ctx)` is exempt exactly like the same cut by `--timeout`, R8B-1), including a Drain an interrupt-and-send or a Stop with a queued human message cuts off (the leg is accounted from the error `agent.Stream` returned, not from the loop's nil hand-over). A net/http timeout also satisfies `errors.Is(err, context.DeadlineExceeded)` but with a live turn context it is a COUNTED provider failure (a black-holed provider is paced and closed at K like any other failure); a refusal before the provider (lock busy or unwritable, provider not configured, peak hours, shutdown: `noteDrainRefused` -- never counted or settled, retried at the gate's pace, 0.5s for a session this process's own `rush run` loop drives and the next re-check tick otherwise); a question the agent asked (a reaction, DUR-4: it suspends automatic turns until a human message); a user turn queued behind the Drain that fails on its own (not charged to the Drain). A Drain call is ONE provider attempt in `runInternal` (no transient-retry or 401-rebuild loop; a pre-agent refusal is a typed `DrainNotAttemptedError`), so paid requests are bounded: one per counted attempt (plus fantasy's own in-step retries). **The launch gate** (`drainGate` on `sessionJobs`, `internal/agent/work_ledger_reaction.go`; read by `drainPermitted`, `internal/agent/coordinator_drain_policy.go`, by the release hook, `wakeSession`, the 60s re-check pass and the CLI loop alike, so no path launches faster than the pace): open when the session was never paced, or a newer fact arrived and the outcome allows it (`hintOpens`: after a refusal or a no-turn Drain yes, after a PAID failure no -- a fact must not restart the retry clock, so K=3 needs at least two pauses), or the retry time (`drainRetryAfterFailure`, 60s) has passed and the gate is not dormant. Dormancy has two independent streaks (`drainGate.freeStreak`/`paidStreak`, `drainDormantStreak`=3; a paid attempt proves the pull works and clears the free streak): three consecutive no-turn Drains that left pending debt (the pull keeps failing; free) make it dormant and a NEWER FACT reopens it; three consecutive paid attempts that left the debt unreacted make it dormant only when the per-row close (or its counting) keeps failing, and then ONLY a human message (`resetConsecutiveResume`) or a restart reopens it. A pull failure therefore never shortens the paid budget (one pull failure plus two provider failures is not dormancy). `DrainStuck` reports either state; one Warn line says which, once, and the debt stays pending and visible (`sessions why`). The web bg-shell auto-turn cap (`maxConsecutiveAutoResumes`=5 per human message) counts SLOTS, spent at admission before the launch decision (`claimAutoResume`, amendment (c)/(x)/(aa)/(ad)): a launch that was paced, refused (lock busy), held by a rerun or deferred by a foreign driver still spent its slot without submitting anything, so the cap bounds the chain of completions that may START a turn per human message, not the attempts on debt that already owns a slot. A completion that finds every slot spent is recorded by its notice ROW ID (`coordinator.bgShellOverCap`; `persistBGShellCompletion` takes the id from the insert, so a completion whose insert failed has no row and is not recorded) and never launches. The claim and the cap bound a session's OWN chain of automatic turns; they do not apply to a delegated CHILD whose delegation row runs: `notifyBackgroundJobDone` wakes it (`wakeSession(fact=true)`, no slot spent) whether or not the claim was granted (web with auto-resume off, web with every slot spent, every CLI process), `delegatedChildDriven` decides "driven" from the durable running `async_jobs` row (another process may own the delegation; an unreadable row falls back to the driver registration, else the 60s pass), and `drainPolicy` allows its turn ahead of the bg-shell policy and the cap, so a parked delegation never waits on a debt nothing launches (R6B-1, amendment (ag)). A root, including one that merely OWNS a running delegation, keeps both. A child's chain of bg-shell wakes (a paid Drain per completion: a child that polls CI with `sleep 90; gh run view` in every Drain) has NO slot cap and no per-delegation bound by decision (a bound that releases the delegation early is the stale-text outcome (ag) rejected, and a count would cap legitimate long polling); its real bounds are Stop (transitive), the delegation's own `timeout` (`terminate_and_wake`, if the model or operator set one: the row goes terminal and a released child gets no further Drain), and in the CLI the run's `--timeout`/6 h default cap and `--max-cost` (the child's spend is charged to the parent, R6C-3). Root supervision does NOT bound it: `job_kill` refuses a delegation (`DelegationJobError`), the check-in says sub-agent control tools are not available, the child's notices never count as progress for the root, and supervision pauses after 6 check-ins (~3 h 15 min of backoff); loop detection is per turn (R7B-3, amendment (ak).3). A release or tick re-check (`wakeSession` with `fact=false`) of bg-shell-only debt, and the turn-start check of a QUEUED Drain (`decideDrainTurn`: it decides on debt its launch decision never saw, and a completion over the cap that landed after the last permitted Drain pulled must not get a sixth automatic turn), compare in `drainPolicy` WITHOUT spending (`bgShellCapDeferred`, `internal/agent/coordinator_bgshell_cap.go`): they defer ("auto-resume cap reached") only when EVERY row of the debt is a recorded over-cap id -- the debt's row ids and the set are read as one step under `bgArrival` (`readTurnFacts` takes the gate before the rows read, all from the ONE `PendingInclusiveDebtRows` read; the read writes nothing: the set is pruned only at a completion's arrival, and a stale id is inert because only ids still owed are counted) -- and otherwise the rows whose slot was spent are retried at the gate's pace and closed at K=3 like any debt. A slot Drain's own pulled-but-unreacted row is not an over-cap id, so the last permitted Drain is never refused for its own slot, while a shell finishing during it is an over-cap row and cannot chain further Drains; an older slot row whose pull failed while newer rows were reacted stays retried, because a newer over-cap row can no longer hide it. Both the slot count and the id set are in memory: cleared by a human message (`ResetAutoResumeCounter`), freed with a deleted session, pruned against the debt at every check and every arrival, lost on restart (which re-arms the cap; the durable rows are then retried like any other debt). Cancellation, deadline, shutdown (`CancelAll`), Stop and non-provider errors settle nothing. `refreshSubAgentCompletion`'s own DB-read Debug (content freshness of an already-delivered result) is not a wake failure and is untouched. The pacing and the suspension of automatic turns are in memory, like Stop's: a web restart lifts them, while K=3 is durable (`wake_attempts`) and shared by two web processes on one data directory. | `TestDrainAttempt_FreshPendingNoticeFirstAttemptCounted`, `TestDrainAttempt_StallIsCountedNotExempt`, `TestDrainAttempt_SwallowedReactionWrite_SettlesAtK`, `TestDrainAttempt_401SettlesFirstAttempt`, `TestDrainAttempt_ShutdownIsExempt`, `TestDrainAttempt_CancelTurnIsExempt`, `TestDrainAttempt_QueuedUserTurnFailureNotCharged`, `TestSettleDrainDebt_MarkerNamesSnapshotToolCallIDs` (`internal/agent/drain_attempt_test.go`), `TestDrainAttempt_ContextTooLargeMarkerNamesCauseAndRemedy`, `TestDrainAttempt_ContextTooLargeThroughTheProvider` (`internal/agent/drain_context_too_large_test.go`), `TestOperatorStop_DefaultCapIsADeadline` (`internal/agent/run_timeout_test.go`); `TestDrainAttempt_EmptyStreamPacedThenSettledAtK`, `TestDrainAttempt_AskQuestionReactsAndSuspends`, `TestDrainAttempt_ReactionAndSettleFail_DormantAfterThree`, `TestDrainAttempt_PermanentPullFailure_BoundedNoTurns`, `TestAdmissionRefusal_NonBusyLockError_NoHotLoop`, `TestReleaseHook_NonChildSession_DoesNotBlockOnDB`, `TestDrainPolicy_ReadErrorFailsClosed` (one read failed per case) (`internal/agent/drain_launch_gate_test.go`); an operator stop is decided from the turn's own context and a cut-off Drain is exempt: `TestDrainAttempt_TransportTimeoutIsCountedPacedAndSettledAtK`, `TestDrainAttempt_RunDeadlineIsExemptAndNamedAsTimeout`, `TestOrdinaryTurn_TransportTimeoutPacesTheGate` (`internal/agent/drain_transport_timeout_test.go`), `TestDrainAttempt_InterruptAndSendIsNotChargedToTheDrain`, `TestDrainAttempt_CancelWithQueuedMessageIsNotChargedToTheDrain` (`internal/agent/drain_handover_test.go`); two dormancy streaks and the per-row close: `TestDrainGate_PullFailureDoesNotShortenThePaidBudget`, `TestDrainGate_FreeDormancyReopensOnNewerFact`, `TestDrainGate_PaidDormancy_NewFactDoesNotReopen`, `TestDrainAttempt_OnlyRowsAtKAreClosed` (`internal/agent/drain_gate_streaks_test.go`); credentials: `TestDrainAttempt_401AfterSuccessfulRefreshIsCountedAndPaced`, `TestDrainAttempt_401WithFailedRefreshSettlesFirstAttempt`, `TestDrainCallFor_RootCallCarriesFreshCredentials`, `TestDrainCallFor_ChildTemplateClientCarriesFreshCredentials`, `TestDrainCallFor_ChildTemplateClientCarriesFreshAPIKey`, `TestFreshenDrainClient_PerCallCredentialsKeepTheirClient`, `TestHasRefreshableCredential`, `TestRunInternal_DrainCarriesFreshCredentialsAfterTheProactiveRefresh`, `TestDrainAttempt_401BlackHoledRefreshStillSettlesTheRow` (`internal/agent/drain_unauthorized_test.go`); the bg-shell cap through real completions and the re-check: `TestBGShellCap_ExactlyFiveAutoResumes`, `TestBGShellCap_SpentSlotsBehindAPauseAreRetriedAndClosed`, `TestBGShellCapDeferred_OnlyWhenEveryDebtRowIsOverCap`, `TestBGShellCapDeferred_BlockedByAnArrivalInFlightFailsClosed`, `TestBGShellCap_ArrivalHoldsTheGateBetweenInsertAndSlotDecision`, `TestClaimAutoResume_CountsOnlyRefusalsForLackOfASlot`, `TestDrainPolicy_CapStateUnreadableFailsClosed` (`internal/agent/drain_bgcap_test.go`); over-cap rows by id: `TestBGShellCap_QueuedDrainNeverCommitsOnAnOverCapRow`, `TestBGShellCap_LastSlotDrainRunsAtTurnStart`, `TestBGShellCap_QueuedDrainForTheLastSlotStillRuns`, `TestBGShellCap_FailedInsertIsNotOverCap`, `TestBGShellCap_SlotRowWhosePullFailedIsRetriedBehindAReactedOverCapRow`, `TestBGShellCapDeferred_FollowsTheDurableDebt`, `TestBGShellCap_OverCapCompletionInTheSeamIsKeptAndDefers`, `TestPersistBGShellCompletion_PrunesOverCapIDsThatLeftTheDebt` (`internal/agent/drain_bgcap_ids_test.go`), `TestInsertSessionNoticeReturningID_ReturnsTheNewRowID`, `TestPendingInclusiveDebtRows_NamesTheDebtNoticeRows` (`internal/session/notice_row_ids_test.go`); per-session state freed: `TestSweepIdleSessions_DropsOnlyEntriesThatHoldNothing`, `TestSweepIdleSessions_ExpiredPauseWithZeroStreaksIsIdle`, `TestRecheckPass_FreesIdleSessionState` (`internal/agent/coordinator_session_state_sweep_test.go`); interface wiring: `TestCoordinatorOptionalInterfaces_ReachableThroughCoordinatorValue`, `TestSetDrainPacingForTest_ShrinksAndRestores` (`internal/agent/coordinator_iface_assert_test.go`); `TestRunInternal_DrainIsOneAttempt`, `TestDrainRefused_TypedAndPacesOnlyDrainCalls` (`internal/agent/coordinator_run_drain_test.go`); `TestAdmissionRefusedRelease_NoImmediateRelaunch_ThenTickRecovers` (`internal/agent/coordinator_wake_lock_refusal_test.go`); the CLI loop: `TestRunNonInteractive_FailedDrainPacedThenSettled`, `TestRunNonInteractive_PeakRefusalNeverSettles`, `TestRunNonInteractive_PermanentPullFailure_ExitsStuck`, `TestRunNonInteractive_TokenCapDrainEndsLoop`, `TestRunNonInteractive_DrainAsksQuestion_AwaitingAnswer`, `TestRunNonInteractive_CtrlCDuringDrain_KeepsAnswerSumsUsage` (`internal/app/app_run_loop_test.go`), `TestRunNonInteractive_ChildFailedDrainRetriedByTick` (`internal/app/app_run_loop_more_test.go`); store level: `TestSettleByFailure_ScopedIncrementAndSettle_GoLayer`, `TestSettleReactedFailedWithMarker_SettlesAndInsertsMarkerAtomically` (`internal/session/async_job_reaction_test.go`). A delegated child is woken whatever the claim says: `TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays` (web with auto-resume off, every slot spent, CLI, with and without the release hook: one Drain, the delegation releases with the child's reaction), `TestBGShellDone_ParentOfARunningDelegationKeepsTheCap` (a root that owns a running delegation keeps the cap and the policy), `TestDelegatedChildDriven_ReadsTheDurableRow` (`internal/agent/coordinator_bgshell_child_test.go`). The reaction chain guard (#1113) lives in the arbiter state (`internal/agent/turn_arbiter_state.go`), NOT in the ledger -- a successful reaction closes the debt (`rows.open == 0`, the gate reset) and would reset the counter with exactly the event that masks the chain; it is reset by real progress, a human message (`resetConsecutiveResume`), or the session's deletion (`sweepDeletedSessionState`), never by a successful reaction. Idle-launch classification is the step classifier `classifyStepCall` (`internal/agent/step_progress.go`): claim ids from the started responses' ClientMetadata, wait-only commands parsed by `shell.IsNoOpCommand`; `pullPendingNotices` does not count a pulled chain link as supervision progress. | Выполняется. |
64:| ASYNC-10 | Любой ответ «почему сессия ждёт» выводится из данных, доступных любому процессу, и называет конкретную задачу. | R-ACT (2026-10-01, шаг 2): единый классификатор `ClassifySessionActivity` (`internal/session/session_activity.go`) над фактами одного читателя `App.SessionActivity`/`SessionActivityBatch` (`internal/app/app_session_activity.go`) — один запрос/проход на вид факта (маркеры драйверов включая мёртвые хосты, реакционный долг, LiveWorkForRoots, разовые расписания, ended_reason по id, lock-enum). Вердикты: `in_turn > delegating > between_turns > crashed > ended > idle`; живая работа выше мёртвого PID (аннотация `CrashedPID`); `delegating` ⇔ живые делегации; ended — только по `ended_reason`; долг держит сессию только при живом драйвере; lock — один enum `absent/held/released/dead/unknown` (`InspectSessionLockFact`, единственный читатель лока; release стирает PID и sidecar, «пусто и читаемо» = released). Все команды `sessions_*` берут живость ТОЛЬКО из вердикта (`kindIsLive`/`listStatus`); слои `computeSessionStatuses`/`reclassifyCrashedAsDone`/`mark*`/`promoteCleanReleaseCrashes`/`inspectSessionLiveWork`/агрегации lock-имён удалены; сопоставление lock-файл↔id — обратная карта `LockStems` из batch-результата (R7C-2/R8C-8). `sessions why` печатает `Verdict.Description` (называет драйвер/задачи/расписания/долг), раздел «Async jobs» и debt — без изменений; `sessions list` маппит Kind→STATUS (running/delegating/crashed/done/пусто). Web-флаги (шаг 3) будут браться из Facts: `HasLiveOwnWork` = `Driver||OwnRunningJobs>0`, `HasLiveDescendantWork` = `LiveDelegations>0`. Нечитаемый факт живой работы = жив (fail-open), нечитаемый факт КОНЦА живости не даёт; отсутствие БД = нет фактов. Limits: остался второй читатель лока `InspectSessionLock` для recovery (fail-closed); обрезанный по cap глубины обход дерева по-прежнему виден через `Unreadable` (`walkIncomplete`). Шаг 3 (R-ACT, 2026-10-01): web-хендлеры переведены — `annotateSessionActivity` (`internal/server/handlers_sessions.go`) берёт ВСЕ аннотации списка (включая `OwnedExternal`/`OwnedByPID`) из ОДНОГО `App.SessionActivityBatch` на опрос; флаги из фактов (`HasLiveOwnWork` = `Driver||OwnRunningJobs>0`, `HasLiveDescendantWork` = `LiveDelegations>0`, `LiveDescendantIDs` = новые `Facts.LiveDescendantSessionIDs`); `OwnedExternal` — из lock-факта того же батча (held + живой чужой PID, без mtime-эвристики). | `TestClassifySessionActivity` + `TestInspectSessionLockFact` (`internal/session/session_activity_test.go`); `TestSessionActivityBatch_MatchesSingle`, `TestSessionActivityBatch_BindingTests` (`internal/app/app_session_activity_test.go`); `TestSessionsActivity_OneTableEveryCommand` (одна таблица форм × все читатели), `TestListStatus_OwnJobShapes`, `TestListStatus_DriverShapes`, `TestSessionLockFact_*` (`internal/cmd/sessions_*_test.go`); `TestExplainSessionStatus_AsyncJobsAndDebtSection` (`internal/cmd/sessions_why_descendant_test.go`); серверные аннотации — `TestHandleListSessions_LiveWorkAnnotationIsBatched` (аннотации == отображению фактов `App.SessionActivity` на таблице форм: живой драйвер между ходами, своя задача, живая делегация, мёртвый хост, at rest; число запросов не растёт с числом сессий), `TestHandleListSessions_LiveRunDriverAnnotation`. | Выполняется — один классификатор, второго места, решающего «жива ли сессия», в cmd и web больше нет; расхождения читателей ловятся одной строкой таблицы. |
66:| ASYNC-12 | Сессия в процессе одновременно держит не больше 50 НЕТЕРМИНАЛЬНЫХ async-задач (бегущие, в т.ч. неподтверждённые и просроченные wake_only, припаркованные делегации). Терминальная задача — недоставленная, ждущая ack или inline-Tx2 — слот не занимает, поэтому потерянное подтверждение не запирает bash-сессию навсегда. Отказ происходит до Claim, строку в БД не создаёт и даёт модели инструкцию и метаданные `async_cap`; `StopTurn` не ставится. Допуск ≠ область: `running()` по-прежнему означает «карта непуста» (ASYNC-02 без изменений). | Снимок и предикат — `runningJobsLocked`/`capSnapshotLocked` под `l.mu`, разбор и форматирование вне блокировки: `work_ledger_cap.go`; проверка в `workLedger.Start` (`work_ledger.go`); типизированный отказ `asyncCapError`, текст и метаданные в ответе — `asyncTool.Run` (`async_tool.go`, `errors.As`), та же подсказка в shell-лимите `internal/shell/background.go`. | `TestWorkLedgerCap_*`, `TestAsyncTool_CapRefusalMetadata` (`internal/agent/work_ledger_cap_test.go`), `TestBackgroundShellManager_CapMessageNamesNextStep` (`internal/shell`) | Выполняется. |
67:| ASYNC-13 | Каждый фоновый shell, переживший ответ инструмента и рождённый ВНЕ async-обёртки (sync-ветка `asyncTool`: SDK-origin и Drain/wake-ходы на `context.Background()`), имеет ровно одну строку `async_jobs` (kind=`bg_shell`, ключ `owner_session_id` + `tool_call_id` = id shell'а, `announced=1` — сам ответ инструмента с id и есть «started»); её терминальный переход — единственный durable факт итога shell'а (state + `result_summary` одним CAS, DUR-1), а запуск фона при недоступной БД — ошибка инструмента (fail-closed), никогда shell без строки. Wake-долг остаётся на `session_notices` (`bg_shell_done`): карта over-cap арбитра ключуется notice-id, перенос долга в строку обошёл бы капу (ASYNC-09) — поэтому строка пишется терминальной с `delivery='done'`, `reacted=1` (никогда не pull'ится и не долг); оболочка без строки (легаси/фикстуры без store) идёт старым notice-путём. `job_kill` по shell_id такого shell'а (или по его id, переданному как job_id) переводит строку в `cancelled` (`notice_kind=job_kill`, `delivery=done`, `reacted=1`, `wake=0`) ДО убийства процесса (`workLedger.StopBackgroundShellRow`, `tools.BGShellStopper`): процесс, не желающий умирать, не оставляет строку `running` (R8B-2), а его выход не пишет ни повторный переход (наблюдатель видит терминальную строку), ни notice `bg_shell_done` с пробуждением (`coordinator.bgShellRowCancelled` в `notifyBackgroundJobDone`); результат tool'а склеивается со строкой по claim (R2A-8). Path 1 (CLI/web async bash) остаётся на строке самой задачи: `setShellID` + `awaitShell`, новый kind для него не создаётся. | Claim: `asyncTool.claimBackgroundShellRow` после `inner.Run` и ДО доставки ответа (`internal/agent/bgshell_claim.go`), `AsyncJobStore.ClaimShell` (`internal/session/async_job_bgshell.go`: Claim + MarkAnnounced, идемпотентен по shell id); терминал: `asyncTool.finishBGShellRow` в наблюдателе `OnDone`, который `claimBackgroundShellRow` регистрирует сразу после claim — независимо от `notify_on_background_job_done` (без колбэка уведомления строка иначе осталась бы `running` навсегда и держала область открытой, #1188); с ограниченным ledger'ом повтором (`retryAsyncStoreOp`); shell, которого уже нет в менеджере, закрывает строку `failed`/«output unavailable»; `persistBGShellCompletion` (`internal/agent/coordinator_bgshell_cap.go`) строку НЕ пишет — только notice `bg_shell_done` и решение о слоте (`internal/agent/bgshell_claim.go`); job_kill: `workLedger.StopBackgroundShellRow` (`internal/agent/work_ledger_bgshell_kill.go`) + `tryStopBGShellRow` в `internal/agent/tools/job_kill.go`/`job_kill_bgshell.go`; миграция `internal/db/migrations/20261005000001_bg_shell_jobs.sql` (rebuild CHECK kind + 'bg_shell'). | `TestAsyncTool_SyncBashBackgroundEscapeClaimsBGShellRow`, `TestAsyncTool_ClaimFailureRefusesBackgroundStart`, `TestAsyncTool_SyncBashBackgroundEscape_ObserverFinishesRowWithoutNotifier`, `TestAsyncTool_SyncBashBackgroundEscape_ObserverMarksFailedExit`, `TestAsyncTool_SyncBashBackgroundEscape_ObserverAndNotifierCoexist`, `TestAsyncTool_SyncBashBackgroundEscape_MissingShellFinishesRowFailed`, `TestFinishBGShellRow_TransitionsClaimedRow`, `TestFinishBGShellRow_FailedShellMarksRowFailed`, `TestFinishBGShellRow_NeverTouchesNonShellRow`, `TestFinishBGShellRow_RetriesUntilTheRowIsReadable`, `TestPersistBGShellCompletion_LeavesTheRowToTheObserver`, `TestPersistBGShellCompletion_RowlessShellWritesNoticeOnly` (`internal/agent/bgshell_claim_test.go`); `TestStopBackgroundShellRow_CancelsTheRowWhileTheProcessStillRuns`, `TestStopBackgroundShellRow_TheExitAfterTheCancelWritesNothingMore`, `TestStopBackgroundShellRow_Verdicts`, `TestJobKill_RawShellIDOverTheRealLedger` (`internal/agent/work_ledger_bgshell_kill_test.go`); `TestJobKillTool_RawShellIDStopsTheDurableRowFirst`, `TestJobKillTool_ShellIDPassedAsJobIDFallsBackToTheRow` и соседние (`internal/agent/tools/job_kill_bgshell_test.go`); `TestClaimShell_RunningAnnouncedAndIdempotent`, `TestBGShellRow_DeadHostSweepInterrupts` (`internal/session/async_job_bgshell_test.go`). | Выполняется. |
90:отдельные запросы между БД и памятью, возвращение гейта в `workLedger` или
97:| DUR-1 | Состояние async-задачи меняет только `transition`: одна транзакция с SQL-CAS; память сходится к БД; держатель исхода повторяет до коммита. | `workLedger.transition`/`commitTransition` (`internal/agent/work_ledger_transition.go`) is the only caller of `AsyncJobStore.Transition` (`internal/session/async_job_store.go`, the one `UPDATE ... WHERE state='running' AND claim_id=?` CAS). `commitTransition` retries with doubling backoff (`asyncStoreRetryBackoff`, 10ms→1s cap) until the store commits or reports the row terminal/gone; the winner writes `job.transitionToTerminal` from the COMMITTED row, the loser adopts the same committed row — memory never decides independently of the DB read. Shutdown latch, exactly as implemented: `workLedger.close()` sets `job.shutdownCancelled` only on a job that is non-terminal AND has not `executorReturned` (`finish()` sets that flag the instant it takes `l.mu`; a delegation's `armDelegation` path does not set it), then closes `closedCh` and cancels those executors. A transition is suppressed (row stays `running`, nothing written) when `l.closed && job.shutdownCancelled` is seen at the top of an attempt, so a natural completion whose executor already returned is still written; the first attempt of any other transition is written even if it races `close()`, but every retry wait (`commitTransition`, `retryAsyncStoreOp`) also ends when `closedCh` closes, leaving the row `running` for the next host. | `TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits`, `TestWorkLedger_ShutdownCausedCancellationLeavesRowRunningWritesNoNotice`, `TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed`, `TestWorkLedger_InFlightLatchSkipsSecondTriggerWithoutBlocking` (`internal/agent/work_ledger_durable_test.go`); `TestWorkLedger_CloseDoesNotLatchOntoExecutorReturnedJob`, `TestWorkLedger_RetryLoopStopsAfterClose` (`internal/agent/work_ledger_review_fixes_test.go`); `TestAsyncJobStore_TransitionWonThenLostThenGone` (loser adopts winner's state, `internal/session/async_job_store_test.go`); `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome`, `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome` (own-cause vs natural-finish races, each keeping its own distinguishable cause on a win). | Выполняется. |
98:| DUR-2 | Терминальный переход, `delivery='pending'` и `wake` по причине — одна транзакция. | `TransitionAsyncJobTerminalPreserveVoid` (`internal/db/sql/async_jobs.sql`, generated into `internal/db/async_jobs.sql.go`) sets `state`/`notice_kind`/`result_summary`/`delivery` (an existing `void` is preserved)/`wake`/`reacted` in ONE `UPDATE` statement, scoped to `state='running' AND claim_id=?`; `causeStateNoticeKindWake` (`internal/agent/work_ledger_transition.go`) computes the wake/delivery/reacted triple per cause BEFORE calling `Transition`, so the commit is a single atomic write of all of them, never a sequence of separate writes. | `TestAsyncJobStore_TransitionWonThenLostThenGone` (won direction: state+delivery+wake set together), `TestAsyncJobStore_TransitionPreservesVoidDelivery`, `TestWorkLedger_TerminalCausesRecordOwnStateNoticeKindAndWake`; `TestTransition_InducedWakeWriteFailureChangesNothing` (`internal/session/async_job_dur2_atomicity_test.go`: through the REAL `store.Transition`, a database trigger aborts the write that sets `wake`; state/delivery/wake/reacted/result must all stay as they were and the same transition then wins once the fault is gone). | Выполняется. |
99:| DUR-3 | Уведомления пишет в историю только ведущий сессии, только в начале хода или на границе шага; уведомление и `delivery='done'` — одна транзакция. | `sessionAgent.pullPendingNotices`/`pullPendingNoticesForStep` (`internal/agent/agent_notice_pull.go`), called from `runTurn` before history loads and from `PrepareStep` at each step boundary (never from a compaction step). The actual pull is `AsyncJobStore.PullJobNotices`/`PullSessionNotices`/`pullOneJobNotice` (`internal/session/notice_pull.go`; #1157 adds kind `child_question` — a delegated child paused on its own question, bound to the held delegation row and voided like `wake_only` once that row is terminal): one `UPDATE ... WHERE delivery='pending' ... RETURNING` per row, in the SAME transaction as the history `INSERT` and the `notice_message_id` write-back (DUR-11). A `wake_only` or supervision notice whose task-scoped condition fails at pull time becomes `void` in that transaction (supervision ignores running rows on provably dead hosts). Nothing else writes a notice into a session between turns. | `TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage` (two independent `*sql.DB` connections racing the SAME row — exactly one message); `TestDrain_NoticeBetweenTwoToolCalls_OnlyAtStepBoundaryAndCarriesForward` (a notice never lands between a tool call and its own result); `TestCompaction_DoesNotPullNotices`; `TestPullJobNotices_CreateTxErrorLeavesRowPending` (a pull error never fails the turn, row stays pending); `TestPullSessionNotices_SupervisionVoidsWhenOnlyRunningRowIsOnADeadHost`. | Выполняется. |
100:| DUR-4 | Долг реакции — строка с `wake=1`, `delivery` ∈ {pending, done} и `reacted=0`; `reacted=1` ставится только транзакцией шага модели с реальным содержимым (или закрытием долга неудачей); четвёртый писатель `reacted=1` (вместе с `delivery='done'`, `wake=0`) — CAS `DeliverAsyncJobInline` в Tx2 A14, для строки, уже отреагированной инлайн-ответом; терминал строки `bg_shell` (наблюдатель при claim — `finishBGShellRow`; job_kill — `StopBackgroundShellRow`) тоже пишет `reacted=1`/`delivery='done'`: строка не долг, долг этого shell'а — notice `bg_shell_done` (ASYNC-13). «Нужен ход» и «область открыта» решаются по долгу и по БД. Каждое освобождение мейлбокса проверяет долг и при необходимости подаёт вызов-забор обычным путём `Run`. Подсказка лишь будит. | **Predicate:** `AsyncJobStore.ReactionDebtExists` (pending-inclusive: `wake=1 AND reacted=0 AND delivery != 'void'`, jobs also `announced=1`) and `VisibleReactionDebtExists` (`delivery='done'` only; read by `sessions why`) in `internal/session/async_job_reaction.go`, wrapped by `workLedger.reactionDebtExists` (`internal/agent/work_ledger_reaction.go`). **Who decides "a turn is needed":** ONE launch predicate, the arbiter's `decide` over `readTurnFacts` (`internal/agent/turn_arbiter.go`), mapped onto the legacy kinds by `drainVerdictOf`/`drainPermitted` (`internal/agent/coordinator_drain_policy.go`; fail-closed on an unreadable input: a live foreign driver, a rerun in progress -- `HoldAutomaticTurns`, `internal/agent/coordinator_turn_hold.go` --, Stop's or a pending question's suspension, a released delegation child (a child whose delegation row RUNS is allowed ahead of the bg-shell policy and the cap, and its bg-shell completion wakes it even when the auto-resume claim is refused, R6B-1), bg-shell-only debt with `AutoResumeOnJobDone` off, and a re-check (or a Drain's turn-start commit) of bg-shell-only debt every row of which is an over-cap completion once the web cap is spent, ASYNC-09), then the per-session launch gate (ASYNC-09). Every launcher reads it and none accounts: the release hook (`coordinator.onSessionIdleHook` runs `go afterRelease`, `internal/agent/supervision.go`, so a stuck DB never blocks the releasing goroutine), `wakeSession` (`internal/agent/coordinator_wake.go`), the 60s re-check pass, the CLI loop (through `CLIScope`, ASYNC-02) and the Drain's own turn-start decision (`decideDrainTurn`, `internal/agent/agent_drain_decision.go`), which reads VISIBLE debt after the Drain's own pull, so debt stuck in `pending` never forces a chain of empty-prompt turns. **Writers of `reacted=1`, exactly three:** (1) `AsyncJobStore.MarkReactedWithMessageUpdate`, in the SAME transaction as the step's own final message, from two call sites: `turnStream.persistStepFinish` (`internal/agent/agent_turn_step.go`; it runs BEFORE the cap/peak checks, so a step that trips `--max-tokens` is still recorded as a reaction) and `turnStream.persistFailureFinish` (`internal/agent/agent_turn_failure.go`; a step that ends on a question the agent asked, `AwaitingAnswerError`, is a reaction too -- fantasy never calls `OnStepFinish` for it -- and suspends automatic turns): every `wake=1, reacted=0, delivery='done'` row of the owner on both tables, and it clears the owner's `reacted_failed` flags (a real reaction supersedes an earlier closure); (2) settle-by-failure `AsyncJobStore.SettleReactedFailedWithMarker` (`coordinator.settleDrainDebt`, `internal/agent/drain_attempt.go`): `reacted=1, reacted_failed=1` on exactly the rows of the attempt's snapshot of `delivery='done'` announced rows taken AFTER the Drain's own pull (`DebtSnapshot`; it names each row by claim and notice message, and every write -- `IncrementWakeAttempts`, `MaxWakeAttempts`, `SettleReactedFailed*`, the `*ForSnapshotRow` queries -- applies only while the row is still that one: `delivery='done'`, same `notice_message_id`, `wake=1`, `reacted=0`, R2A-4/R2A-5, so a stale settle cannot touch a later reaction, a row a Rerun re-pended or voided, or the new row a reused tool_call_id created), for a row after K=3 counted attempts of THAT row (other rows of the snapshot keep their own count) or, for every row, a terminal provider classification at the first attempt; never on cancellation/shutdown (ASYNC-09); (3) `job_kill`: `causeJobKill` writes `delivery='done', reacted=1, wake=0` in its terminal `Transition` (its result is the tool's own answer, so it is not debt). `reacted_failed` is scoped and superseded: cleared by the real-reaction transaction, by `Claim` of a fresh delegation on that child, and reset (with `wake_attempts`) when Rerun re-pends a row. **Retention never purges debt:** `PurgeAsyncJobsOlderThan`/`PurgeSessionNoticesOlderThan` require `delivery IN ('done','void')` and exclude `delivery='done' AND wake=1 AND reacted=0`; a delegation row (`child_session_id` set) is also kept while its child session still has a running row or unreacted debt, because `isDurableDelegationChild` reads it (R2A-10); `PurgeAsyncJobsOlderThan` also keeps a `job_kill` row that is `done` with `notice_message_id IS NULL` (wake=0, so not debt, but exactly the state dead-host recovery re-pends: purged first, the killed job's output would never reach the session, R5A-1; the same predicate as the third arm of `ListDistinctRecoverableHostIDs`, applied to `CountAsyncJobsOlderThan` too); `sessions gc --jobs-older-than` rejects an age <= 0. **Release check:** the single choke point every `Run`/`RunWithReservedOwnership`/`ReleaseExclusive` passes through, `abandonOwnershipWithHandoff` (`internal/agent/agent_ownership.go`), fires `onSessionIdle`; a release after a Drain attempt neither relaunches nor accounts anything (the turn loop that ran the leg already did, before the release): the gate it left behind decides. The Drain call itself is an ordinary `SessionAgentCall` through `Run`/`submit`, ONE provider attempt (`runInternal` runs no transient-retry or 401-rebuild leg for it; a pre-agent refusal is a typed `DrainNotAttemptedError` and paces the gate). **Where the fallback runs:** the 60s re-check set and pass run in the web process (`SetPersistentMode(true)`) and in `rush run` (`ClaimExternalDriver` starts the ticker, `CancelAll` stops it), so a failed delegated child's reaction turn is retried in the CLI too; the CLI loop also re-reads the DB itself, at least every 5s, and has no auto-turn cap (nothing increments the counter in a non-persistent coordinator). **Debt dies with a wipe (R8A-3):** `AsyncJobStore.ResetOwnerHistory` (`internal/session/async_job_reset.go`, behind `rush sessions reset`) deletes the messages and voids every pending/done row of the owner on both tables in ONE transaction (debt excludes `void`; `reacted_failed` is cleared) and refuses while a row of the owner still runs; `resetSessionHistory` (`internal/cmd/sessions_reset.go`) first recovers dead-host rows and refuses while a live driver marker, own job or delegation exists, `--force` included. The reaction chain guard (#1113) adds NO writer of `reacted=1` and no new debt state: it only defers launches; its marker notice (`reaction_chain`) is `wake=0` (never debt) and Rerun voids it like `wake_failed` (`RependSessionNoticesByMessageIDs`/`VoidWakeFailedNoticesByMessageIDs`). | `TestResetOwnerHistory_WipesAndVoidsOnlyThisOwner`, `TestResetOwnerHistory_RunningJobRefusesAndChangesNothing`, `TestResetOwnerHistory_InjectedFailureRollsEverythingBack` (`internal/session/async_job_reset_test.go`), `TestSessionsReset_VoidsAsyncNoticesAndDebt`, `TestSessionsReset_LiveLoopRefusesEvenWithForce`, `TestResetSessionHistory_RefusesWhileLiveWork`, `TestResetSessionHistory_DeadHostRowsAreRecoveredAndVoided` (`internal/cmd/sessions_reset_async_test.go`); `TestMarkReactedWithMessageUpdate_OneTransactionClearsDebt`, `TestMarkReactedWithMessageUpdate_TransactionIsAtomic`, `TestMarkReactedWithMessageUpdate_SessionNoticesHalf`, `TestReactedFailed_RealReactionClearsStaleFailureWithinOneDelegation`, `TestReactedFailed_NewDelegationClaimClearsStaleFailureFromPriorDelegation`, `TestCaptureDebtSnapshot_ExcludesPendingIncludesDone`, `TestSettleReactedFailedWithMarker_SettlesAndInsertsMarkerAtomically`, `TestSettleReactedFailedWithMarker_MarkerInsertFailureRollsBackTheSettle` (a trigger fails the marker insert: the settle rolls back too), `TestDebtSnapshot_LateSettleNeverTouchesRowsTheTurnDidNotSee` (`internal/session/async_job_reaction_test.go`); `TestIncrementAsyncJobWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow`, `TestSettleAsyncJobReactedFailedForSnapshotRow_OnlyTheSnapshotRow`, `TestSettleSessionNoticeReactedFailedForSnapshotRow_OnlyTheSnapshotRow` (`internal/db/async_phase4_settle_failure_test.go`); `TestPurgeJobsOlderThan_NeverPurgesUnreactedDebt`, `TestPurgeSessionNoticesOlderThan_NeverPurgesUnreactedDebt`, `TestPurgeJobsOlderThan_KeepsDelegationRowWhileChildStillHasWork` (`internal/session/async_job_gc_test.go`), `TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery` (`internal/session/async_job_gc_jobkill_test.go`); `TestSessionsGcCmdRun_JobsOlderThan_NeverPurgesUnreactedDebt` (`internal/cmd/sessions_gc_jobs_test.go`); `TestWorkLedger_MarkJobStopped_RowGoesStraightToDoneNeitherDebtNorNotice` (`internal/agent/work_ledger_job_kill_test.go`); a step's reaction is recorded even when the turn ends right after it, and a question is a reaction: `TestOnStepFinish_CapAbortStepIsARecordedReaction`, `TestAskQuestion_InDrainTurnIsAReaction` (`internal/agent/drain_reaction_persist_test.go`); `TestStepFinishWriteFailure_DebtStaysThenOneMoreDrainClearsIt`, `TestCompaction_DoesNotClearUnreactedDebt`, `TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts`, `TestInterruptDuringDrain_OperatorMessageReachesProvider` (`internal/agent/drain_debt_edge_cases_test.go`); the launch predicate and the attempt each leg counts as: `TestDrainAttempt_FreshPendingNoticeFirstAttemptCounted`, `TestDrainAttempt_SwallowedReactionWrite_SettlesAtK` (`internal/agent/drain_attempt_test.go`), `TestDrainAttempt_EmptyStreamPacedThenSettledAtK`, `TestDrainAttempt_AskQuestionReactsAndSuspends`, `TestAdmissionRefusal_NonBusyLockError_NoHotLoop`, `TestReleaseHook_NonChildSession_DoesNotBlockOnDB`, `TestDrainPolicy_ReadErrorFailsClosed` (`internal/agent/drain_launch_gate_test.go`); `TestRunInternal_DrainIsOneAttempt`, `TestDrainRefused_TypedAndPacesOnlyDrainCalls` (`internal/agent/coordinator_run_drain_test.go`); `TestAdmissionRefusedRelease_NoImmediateRelaunch_ThenTickRecovers` (`internal/agent/coordinator_wake_lock_refusal_test.go`); `TestRecheckDebtOnRelease_DrainOutlivesCheckBudget_StillCompletes` (`internal/agent/coordinator_recheck_debt_deadline_test.go`); `TestOrphanedDrain_ReleaseTimeRecheckConverges` (`internal/agent/drain_orphan_release_convergence_test.go`); `TestDrain_CompactionContinuation_NoEmptyPrompt` (`internal/agent/drain_compaction_continuation_test.go`); `TestDrain_WakeNoticeCallsProviderWithNoticeAndNoTextPrompt`, `TestDrain_NothingPendingSkipsProviderAndFinishesTurn` (`internal/agent/drain_notice_pull_test.go`); a rerun holds automatic turns: `TestRerunHold_DefersAutomaticTurnsUntilRelease` (`internal/agent/coordinator_turn_hold_test.go`), `TestRerun_HoldsAutomaticTurnsUntilHandoff`, `TestRerun_BailoutReleasesTheHold`, `TestRerun_StopRerunJobsDoesNotBlockTheHandler` (`internal/server/rerun_hold_test.go`); `TestRunNonInteractive_PermanentPullFailure_ExitsStuck` (`internal/app/app_run_loop_test.go`). A running delegation allows its child's Drain ahead of the bg-shell policy and the cap (R6B-1): `TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays` (`internal/agent/coordinator_bgshell_child_test.go`). | Выполняется with these limits: a Drain reacts only to debt already visible after its own pull; the supervision "moved notice becomes `reacted=1` when the scope closes" half of design sec.3.4 is NOT implemented (a supervision notice pulled while the scope was open keeps `wake=1, reacted=0` and gets one real reaction turn even if the scope closed in between; only the pull-time `void` of session notices exists; job rows have none). |
101:| DUR-5 | Процесс жив ⇔ держит ОС-блокировку своего файла хоста; «мёртв» — только при захвате или отсутствии файла. Часы не используются. | `ProbeHostLock`/`ProbeHost` (exclusive, dead-host recovery) and `ProbeHostLockShared`/`ProbeHostShared` (non-acquiring, reader-safe), both opening the lock file read-only (`internal/session/host_lock.go`); `RegisterHost` runs lazily from `AsyncJobStore.ensureHost` on the first `Claim` of an async job OR the first `ClaimSessionDriver` — so a `rush run` registers a host as soon as it claims its session, even if it never starts a job (removed by `Close`; a crashed one is reaped as an empty dead host). `RegisterHost` never publishes an id whose lock file a reaper can unlink under it (R2A-1): `acquireFreshHostLock` creates the file, locks it and re-checks that the path still names the locked file, retrying under a new id on contention or a vanished file; removers unlink only while holding the lock on POSIX (`RemoveDeadHostFile`, `unlinkBeforeUnlock`), and on Windows an open file cannot be deleted at all. `RecoverDeadHost` holds no lock while it works (R3A-3): it decides with the shared probe and takes the exclusive lock only inside `removeDeadHostFile`, for the host file's removal, so a host being recovered still probes dead to every reader (a crashed driver's session is claimable, `ForeignLiveDriver` and the scope readers do not report a live host, until the recovery ends -- before, the recoverer's exclusive lock made every shared probe read "alive" for the whole row-by-row recovery, up to 60s in the web process). Concurrent recoverers are therefore admitted, and every recovery write is keyed by row identity (`Transition` by claim, the unannounced-row delete by claim, host-scoped statements for rows only the dead host's id carries), so N recoverers produce one effect per row and exactly one of them deletes the `async_hosts` row (`RecoveryOutcome.HostRemoved`). At most one removes the lock file: whoever wins its exclusive lock in `removeDeadHostFile`, the only place recovery and the empty-host reap take it. If another recoverer or reaper holds it at that moment, that one is inside its own `removeDeadHostFile` and unlinks the file itself in the same pass; if a reader's shared probe holds it, nobody does and `purgeOrphanHostLockFiles` (`PurgeExpired`) reaps the file once its row is gone (`TestPurgeOrphanHostLockFiles_ReapsFileWithNoRow`, `internal/session/async_job_recovery_test.go`); a listed file another reaper (or `RecoverDeadHost`) already deleted before this probe is a no-op, not a failure -- the probe reports dead with no lock and no error, and `reapOrphanHostLock` skips it instead of logging "remove lock file failed" (R6A-1; `TestReapOrphanHostLock_FileDeletedBeforeProbe_NoWarn`, `TestReapOrphanHostLock_DeadRowlessFile_RemovedWithoutWarn`, `internal/session/async_job_orphan_lock_race_test.go`). Every deleter removes the row before the file. `purgeEmptyDeadHostFiles` (the empty-host reap in `PurgeExpired`) has the same shape as `RecoverDeadHost` (R5A-2): it decides with the shared probe, deletes the `async_hosts` row holding no lock (that write can wait behind any in-process transaction, up to busy_timeout, 30s) and takes the exclusive lock only inside `removeDeadHostFile`, so a host being reaped probes dead to every reader for that whole time. Residual window: a reader that probes during the single stat-and-unlink of a host whose last row just went sees "alive" for that instant. The `async_hosts.label` column (its comment in migration `20260928000002` says `'cli'|'web'`; a shipped migration is not edited) is the registrant's App label, `"app"` at both `NewAsyncJobStore` call sites, display-only (`sessions jobs` shows it as the host label; nothing decides on it). Unknown = alive: any outcome other than a clean "dead" is folded into unknown by `HostLiveness` and treated as not-dead by `HostNotDead` and the driver checks. No timestamp comparison anywhere in the liveness decision. Forced shutdown: `AsyncJobStore.CloseKeepLock` (called by `App.releaseResources` when turns are still running past the grace period) forgets the in-memory host WITHOUT releasing the OS lock, deleting rows or removing the file (`retainHostLockUntilExit` pins the handle), so no other process can recover rows the live goroutines are still writing; the lock dies with the process, exactly like a crash. It is not released in-process later either (R2C-10): the goroutines that outlive the grace (agent runs, pump workers, ledger executors) have only per-agent/per-job completion signals, no single one to wait on for all of them, so no release point is provably safe (an early release would let another process recover rows a still-running goroutine writes). `rush run` exits right after `Shutdown`; a long-lived embedding process that sees `ShutdownResult.Forced` must exit too, and until then the rows stay `running` on a locked host that a new App in the same process (its own-host set still holds the id) cannot recover. | `TestProbeHostLock_DeadViaENOENT`, `TestProbeHostLock_DeadViaWonLock`, `TestProbeHostLock_Alive`, `TestProbeHostLock_Unknown`, `TestProbeHostLockShared_AliveDoesNotDisturbHolder`, `TestProbeHostLockShared_CoexistsWithAnotherSharedHolder`, `TestProbeHostLockShared_DeadReleasedLockIsReusable`, `TestProbeHostLockShared_DeadViaENOENT`, `TestProbeHostLockShared_Unknown`, `TestProbeHost_RefusesOwnHostID`, `TestProbeHostShared_RefusesOwnHostID` (`internal/session/host_lock_test.go`); `TestRegisterHost_ReaperHoldsLockInCreateWindow_RetriesUnderNewID`, `TestRegisterHost_ReaperUnlinksFreshFile_RegistrantDoesNotKeepUnlinkedInode` (POSIX only) (`internal/session/host_lock_register_race_test.go`); `TestRemoveDeadHostFile_POSIX_UnlinksWhileStillHoldingTheLock` (POSIX only: skipped on Windows, so it runs on Linux/macOS CI; fails when `unlinkBeforeUnlock` is false) (`internal/session/host_lock_unlink_order_test.go`); `TestRecoverDeadHost_MidRecovery_HostStillProbesDead`, `TestRecoverDeadHost_ConcurrentRecoverers_OneEffectPerRow`, `TestRecoverDeadHost_ConcurrentRecoverers_HostRemovedOnce`, `TestRecoverDeadHost_StaleListing_KeepsFreshClaimOfSameToolCallID`, `TestRecoverDeadHost_StaleListing_AnnouncedRow_KeepsLiveReclaim` (`internal/session/async_job_recovery_concurrent_test.go`); `TestPurgeEmptyDeadHostFiles_MidPurge_HostStillProbesDead` (`internal/session/async_job_purge_empty_host_test.go`: the reaper parked between its verdict and the row delete, the host still probes dead, its crashed driver's session is claimable; revert-check: the exclusive probe held across the delete); `TestPurgeExpired_RemovesEmptyDeadHostFiles_KeepsLiveAndNonEmpty` (`internal/session/async_job_recovery_test.go`); `TestHostNotDead_SelfAliveDeadUnknown` (`internal/session/async_job_reaction_test.go`); `TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile` (`internal/session/async_job_recovery_test.go`); `TestClaimSessionDriver_MarkerUnavailableWithoutHostLock` (`internal/session/session_driver_test.go`); `TestAsyncJobStore_CloseKeepLock_LeavesLockHeld` (`internal/session/async_job_recovery_test.go`: the pin survives GC); `TestForcedShutdown_PinsHostLockAndRowsStayRunning` (`internal/app/app_forced_shutdown_host_test.go`: through a real forced `App` shutdown the lock stays held, the own-host id stays marked and a second store in the process leaves the row `running`). | Выполняется. |
102:| DUR-6 | Восстановление только переводит `running` мёртвого хоста в `interrupted`/удаляет неанонсированное; историю не пишет и сессии не будит. | `AsyncJobStore.RecoverDeadHost` (`internal/session/async_job_recovery.go`) and `SweepDeadHosts`/`RecoverOwnerScope` (same file), called from `ensureHost`'s first-registration sweep, `coordinator.RunMaintenanceSweep` (the 60s `RecheckPass` of the web process and of `rush run`; once at `rush run` loop start too), and every scope check (`CLIScope`, `internal/agent/coordinator_reaction_source.go`). `announced=1` rows transition to `interrupted`/`wake=0` through the SAME `Transition` CAS as any other terminal cause (DUR-1/DUR-2); `announced=0` rows are deleted outright (`DeleteUnannouncedAsyncJobForClaim`, keyed by the listed row's claim so a stale recoverer cannot remove a live host's fresh claim of the same `tool_call_id`, R3A-3; terminal `announced=0` rows of the dead host too, `DeleteTerminalUnannouncedAsyncJobsForHost`, R2A-7); a `job_kill` row left `done` with no result message is made a plain pending, `wake=0` notice again (`RependJobKillRowsWithoutNoticeForHost`, R2A-8); a delegation row whose child messages cannot be read stays `running` for a later sweep (never a false "no textual response"). Neither path calls `messages.Create`/`wakeSession` itself. | `TestRecoverDeadHost_CreatesNoMessages` (direct proof: zero messages created by recovery); `TestRecoverDeadHost_AnnouncedDelegationInterruptedWithChildText`, `TestRecoverDeadHost_AnnouncedBashInterruptedFixedText`, `TestRecoverDeadHost_UnannouncedDeleted`, `TestRecoverDeadHost_TwoRecoverersRace_ExactlyOneTransitionPerRow`, `TestSweepDeadHosts_UnstoppedRunningRowsYieldExactlyNFactsThenNone`, `TestSweepDeadHosts_DeletesTerminalUnannouncedRowsOfDeadHost`, `TestSweepDeadHosts_RependsJobKillRowWithoutResultMessage` (`internal/session/async_job_recovery_test.go`; performs no Stop); `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (`internal/agent/coordinator_stop_restart_test.go`; performs a real `coordinator.Cancel` first); end-to-end: `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted`, `TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck` (`internal/app`). | Выполняется. |
103:| DUR-7 | «started» и `announced=1` — одна транзакция; неанонсированная задача никогда не даёт уведомления. | The ack gate's fused write: `internal/agent/work_ledger_announce.go` (workLedger side) + `AsyncJobStore.AnnounceStarted` (`internal/session/notice_pull.go`) commit A14 adds the inline twin of the same shape: a job whose natural terminal transition commits inside the inline window is answered by its own tool result (`ackTag.Inline`), fused by `AsyncJobStore.AnnounceInlineResult` (`announced=1` + `announce_message_id` + `DeliverAsyncJobInline` in ONE transaction; `ErrAsyncJobGone` falls back to a plain Create, 0 delivery rows = the row was voided by a Rerun and the ordinary tail runs); the started flow itself is unchanged. the "started" tool-result message, `announced=1` and `announce_message_id` (DUR-11) in ONE transaction, no state condition; if the row is gone (`ErrAsyncJobGone`, e.g. the owner session was deleted) the transaction rolls back and the caller persists the tool result the ordinary way, so a `tool_use` never lacks its result; `onToolResult` (`internal/agent/agent_turn_stream.go`) calls `workLedger.persistToolResult`, which fuses ONLY the job's own result: `asyncTool` tags its "started" response (and a failed-launch error) with the job's `claim_id`, and `claimAck` hands the right to acknowledge/abort to one tagged result while the job is unannounced in memory AND in its row; an ordinary result that merely shares the `tool_call_id` (providers number calls per response) is a plain `Create` that touches no job, and `abort` leaves an already-announced job alone (R2B-3). A panic between `Start` and the executor launch fails the row with the launch error directly (`launchExecutor`'s recover -> `failLaunch`, never the delegation re-check, whose refresh would report a resumed child's OLD answer) and answers with a claim-tagged error result that the ack gate treats as the job's own, so the row is announced and the failure delivered (R2B-14). Every pull query filters `announced=1` (`internal/db/sql/async_jobs.sql`). | `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction`, `TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled`, `TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice`, `TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck`, `TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate` (`internal/agent/work_ledger_ack_gate_test.go`); `TestAsyncTool_PanicBetweenStartAndExecutorLaunchFinalizesJob` (`internal/agent/async_tool_test.go`), `TestAsyncTool_LaunchPanicOnResumedDelegationFailsRowNotChildsOldAnswer` (`internal/agent/work_ledger_launch_failure_test.go`); `TestAckGate_CollidingOrdinaryResultDoesNotTouchRunningJob`, `TestAckGate_OrdinaryResultBeforeOwnStartedDoesNotAnnounce`, `TestAckGate_SecondStartedResultForSameJobDoesNotReannounce`, `TestAckGate_AbortLeavesAnnouncedJobAlone`, `TestAckGate_RowAnnouncedInDBIsNotFusedAgain` (`internal/agent/work_ledger_ack_tag_test.go`, through `turnStream.onToolResult`). | Выполняется. |
104:| DUR-8 | БД недоступна при старте async-задачи — задача не запускается. | `workLedger.Start` (`internal/agent/work_ledger.go`): a non-sync job's `Start` calls `store.Claim` FIRST — a Claim error (including "no store wired") returns the error and the executor callback is never invoked (fail-closed); `asyncTool.Run` never reaches `go t.run` then. A sync job never touches the store at all (design doc sec.3.1), so it still works with none wired. | `TestWorkLedger_StartFailsClosedWhenStoreUnavailable` (nil store — the literal "no store wired" case; its `cancelCalled` assertion is weak by itself, the executor-never-runs property is proven one layer up); `TestAsyncTool_InnerNeverRunsWhenStartFailsClosed` (`internal/agent/async_tool_test.go`: the inner tool never runs); `TestAsyncJobStore_ClaimFailsClosedAgainstAClosedDB` (`internal/session/async_job_store_test.go`: a closed database makes `Claim` error); `TestWorkLedger_StartSyncJobWorksWithoutStore` (the sync counter-case). | Выполняется — a configured-but-failing DB reaches `Start` only as the store's `Claim` error (proven at the store layer); there is no `Start`-level test against a closed database, since `Start` does not distinguish "no store" from "store errors" (both hit the same `err != nil` return). |
105:| DUR-9 | Stop отменяет дерево делегаций; отменённая и прерванная работа не будит никого. | `coordinator.Cancel` = `stopTree` (`internal/agent/coordinator_interrupt.go`) walks the live tree via `workLedger.cancelTree` (`internal/agent/work_ledger_delegation.go`), cancels each session's own driver (`agentFor`), suspends automatic turns for each id until the next human message (`suspendAutoResume`: a per-session suspension state of its own, separate from the bg-shell cap counter; `drainPolicy` reads it for every wake kind and `autoResumeEligible` for bg-shell auto-resume; `ResetAutoResumeCounter` clears it together with the counter), and durably zeroes `wake` on every pending/done row across the WHOLE stopped tree via `AsyncJobStore.SetWakeZeroForOwners` (`internal/session/async_job_reaction.go`) — closing the race where a job finishes `wake=1` a moment before Stop. After the zeroing `stopTree` re-checks EVERY id of the tree (`recheckChild`), the Rerun tree stop included: a child parked on its own job is not re-checked by any other trigger once Stop drops that job silently (the idle mailbox fires no release hook), so without it the child's driver record and allowlist entry lived as long as the process and `agentFor(child)` kept routing to the idle task agent (R7B-2). The re-check also runs from `workLedger.finish` for a STALE executor (its job already dropped by the Stop): a Stop that lands while the child's async `bash` executor is still inside its one-second fast-failure sleep finds the shell in the table, so the stop's own re-check cannot release the child, and the executor's later `KillOwned` fires no callback (R8B-4). A stopped plain job is recorded `cancelled`/`session_cancel`/`wake=0` (a notice for the next turn, no wake). Recovery (DUR-6) never wakes; a Stop-then-restart adds no extra facts. Rerun uses `CancelTurn` + `StopRerunJobs`, not this path. | `TestStop_OneSecondAfterNaturalFinish_NoNewTurn` (race-won natural completion, wake zeroed, a late hint produces no turn); `TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere` (N=2 sub-agents, each with its own running bash job — Cancel(root) zeroes every descendant's wake, no session in the tree gets a turn), `TestDrainPolicy_StopSuspension_RefusesEveryAutomaticTurn` (Stop's suspension refuses a Drain of any origin and leaves the bg-shell counter alone), `TestDrainPolicy_BgShellCapExhausted_DoesNotBlockAsyncWake`, `TestWakeSession_BgShellCapExhausted_AsyncJobWakeStillRunsTurn`, `TestResetAutoResumeCounter_ClearsStopAndCap` (`internal/agent/coordinator_drain_cap_test.go`); `TestStop_ReleasesTheChildDriverOfAParkedDelegation`, `TestStop_StaleExecutorReleasesTheChildDriverItsShellHeld` (`internal/agent/coordinator_stop_child_driver_test.go`: Stop on the parent, Stop on the child and a Rerun that voided the delegation each release the parked child's driver; the second test stops while the executor's shell is still in the table and releases after the executor returns); `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (`internal/agent/coordinator_stop_restart_test.go`: N=3 sub-agents, a real `Cancel`, then the dead-host sweep adds zero facts); `TestSweepDeadHosts_UnstoppedRunningRowsYieldExactlyNFactsThenNone` (`internal/session/async_job_recovery_test.go`: the no-Stop twin — N unstopped rows on a dead host yield exactly N facts on the first sweep and none on a second); `TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows` (`internal/session/async_job_reaction_test.go`). | Выполняется. |
106:| DUR-10 | Драйвер сессии из другого процесса записан долговременно: `session_drivers(session_id → host_id)`. Драйвер жив ⇔ жив его хост (DUR-5, без часов; unknown = жив). Пока он жив, никакой другой процесс не запускает для этой сессии ход реакции; уже допущенный вызов-забор лишь переносит уведомления. Второй драйвер не допускается: `rush run` занимает маркер до первой записи в сессию и снимает его при выходе. | Store: `AsyncJobStore.ClaimSessionDriver` (idempotent for the same host; refuses with `*ErrSessionDrivenElsewhere` naming host and pid while another host is alive or unknown; takes a dead host's marker over with a CAS on that exact host, so of N takers one wins), `ReleaseSessionDriver` (own claim only), `ForeignLiveDriver` (foreign iff the marker names another host that is not provably dead), `purgeDeadSessionDrivers` (from `PurgeExpired`) (`internal/session/session_driver.go`, migration `20260929000004_add_session_drivers.sql`); `AsyncJobStore.Close` deletes the host's markers, `CloseKeepLock` keeps them (DUR-5); the marker's session FK cascades. Coordinator: `ClaimExternalDriver`/`ReleaseExternalDriver` (`internal/agent/coordinator_reaction_source.go`; the durable release always runs, the in-memory marker of a non-persistent coordinator is kept until exit) and the first branch of `drainPolicy` (`internal/agent/coordinator_drain_policy.go`), which covers both `wakeSession` before submit and `decideDrainTurn` at an admitted Drain's turn start (then only the notice pull runs, `reacted` untouched, no provider call): a foreign live driver with debt puts the session in the 60s re-check set; an unreadable marker fails closed for every coordinator, web and CLI alike (the policy refuses the launch, names the read error and puts the session in the re-check set: `TestSessionDrainPolicy_DriverMarkerUnreadable`, cases `web_fails_closed` and `cli_fails_closed`). App: `runNonInteractiveWithAsyncResults` claims in `onSessionResolved` (`internal/app/app_run_async.go`, `internal/app/app_run.go`) — after the session resolves and before `drainPendingBeforeRun` or any session write, so a refused claim writes nothing to the session and the first turn never runs (the loop's once-per-run `RunMaintenanceSweep` and the claim's own host registration have already happened: the refused run leaves a registered host behind until it exits) — and releases on every exit on a context detached from cancellation. Only a data dir that cannot host an OS lock file degrades the claim (R3A-1): `RegisterHost` returns the typed `ErrHostLockUnavailable` for exactly that class (the lock file cannot be created or opened, or the lock call fails for a reason other than contention), `ClaimSessionDriver` maps exactly that to `ErrDriverMarkerUnavailable`, and `ClaimExternalDriver` logs a Warn and keeps the in-memory marker only, so other processes cannot see the loop. Every other failure registering the host -- a database error such as `SQLITE_BUSY`/`SQLITE_FULL`, a cancelled ctx, an exhausted registration retry -- and every failed marker write is returned by `ClaimSessionDriver` unwrapped from that sentinel: the claim fails, and because it runs in `onSessionResolved` the run fails before it touches the session (the web process is never left free to run a paid Drain on a session a CLI loop drives). | `TestClaimSessionDriver_IdempotentReleaseAndClose`, `TestClaimSessionDriver_LiveRefused_DeadTakenOver`, `TestClaimSessionDriver_ConcurrentTakeoverOneWinner`, `TestClaimSessionDriver_MarkerUnavailableWithoutHostLock`, `TestForeignLiveDriver_Matrix`, `TestPurgeExpired_DeletesOnlyDeadDriverRows`, `TestSessionDrivers_CascadeOnSessionDelete` (`internal/session/session_driver_test.go`); `TestClaimSessionDriver_HostInsertDBError_FailsClaim` (a SQLite trigger aborts `INSERT INTO async_hosts`), `TestClaimSessionDriver_CancelledCtx_FailsClaim`, `TestRegisterHost_ErrorClasses` (`internal/session/session_driver_register_error_test.go`); `TestClaimExternalDriver_HostInsertDBError_FailsClaim`, `TestClaimExternalDriver_NoLockCapableDataDir_DegradesToInMemory` (`internal/agent/coordinator_claim_driver_error_test.go`); `TestSessionDrainPolicy_ForeignDriver_NoDrainParkedWhileDebt`, `TestDecideDrainTurn_ForeignDriver_TransferOnly`, `TestSessionDrainPolicy_OwnLoopNotBlocked`, `TestSessionDrainPolicy_DriverMarkerUnreadable`, `TestClaimExternalDriver_DurableClaimAndRefusal`, `TestClaimExternalDriver_MarkerUnavailableFallsBackToMemory`, `TestReleaseExternalDriver_DurableReleasedEvenWhenNonPersistent` (`internal/agent/coordinator_external_driver_durable_test.go`); `TestReleaseExternalDriver_NonPersistentCoordinator_NeverReleases`, `TestReleaseExternalDriver_PersistentCoordinator_StillReleases` (`internal/agent/coordinator_external_driver_release_test.go`); `TestRunLoop_DrivenSession_FailsBeforeTurnOrMutation`, `TestRunLoop_DurableDriverReleasedInNonPersistentMode`, `TestTwoAppScenarioE_WebJobWhileCLIDrives_RootAnswersInCLI`, `TestTwoAppScenarioF_DriverCrash_WebTakesOver`, `TestTwoAppScenarioF2_DriverCrashBeforeCompletion_WebReactsOnHint` (`internal/app`). | Выполняется. Limits: a marker still claimed when a shutdown is forced counts as alive until the process really exits (`CloseKeepLock`; a `rush run` loop releases its own in its deferred exit, before `Shutdown` runs, so this is a library embedder's case, and the embedder must exit — R2C-10, see the forced-shutdown note in DUR-5); a crashed driver is taken over by the web process only on its next hint or 60s pass; a web human turn on a driven session may still react to the notice (the CLI's summary then does not contain that text). |
107:| DUR-11 | `delivery='done'` ⇒ строка называет сообщение истории, несущее её результат (`notice_message_id`); сообщение «started» названо в `announce_message_id`. Rerun вычисляет свои множества только по этим id. | `notice_message_id`: written in the pull transaction for a pulled job/notice row, by `AnnounceInlineResult` for an A14 inline-delivered row (the inline message IS the result message: `announce_message_id == notice_message_id`), (`pullOneJobNotice`/`pullOneSessionNotice`, `internal/session/notice_pull.go`); for `job_kill`, whose terminal `Transition` goes straight to `delivery='done'`, `AsyncJobStore.AnnounceJobKillResult` writes the tool-result message id in the same transaction as the message insert (`SetAsyncJobNoticeMessageIDForClaimIfDone`, called from `workLedger.persistJobKillResult`). The row is named by the CLAIM job_kill's own transition won (`JobKillResponseMetadata.KilledClaimID`), never by `tool_call_id`: a provider that numbers calls per response can issue a new `call_0` while job_kill is still running, which archives the just-killed row under another key (R3A-2); the guard `delivery='done' AND notice_message_id IS NULL` is defensive: `KilledClaimID` is emitted only after job_kill's own transition won, a Rerun re-pends by `notice_message_id` (an unnamed row never matches) and no other writer names a `done` job_kill row, so the one reachable cause of a 0-row fused write is deletion (the owner session's cascade); it fuses nothing and the tool-result message is persisted regardless (`TestAnnounceJobKillResult_LostRaceStillPersistsMessageWithoutFusing` forces the impossible state to prove the guard). When the fused write does not happen at all (crash between the two commits, an error result, a cancelled context, a failed transaction) the row would be `done` with no message: dead-host recovery re-pends it (`RependJobKillRowsWithoutNoticeForHost`) and the live tool path calls `AsyncJobStore.RependJobKillRowWithoutNotice(owner, claim_id)` (R2A-8, keyed by claim only since R3A-2; the tool-side call belongs to the work ledger). Recovery's re-pend is by host and row state, so it has no key to get wrong: it skips exactly the rows the fused write named. Retention keeps such an unnamed row until recovery has repaired it (`PurgeAsyncJobsOlderThan`/`CountAsyncJobsOlderThan` exclude `delivery='done' AND notice_kind='job_kill' AND notice_message_id IS NULL`, R5A-1): a live host whose fused write kept failing would otherwise lose the row to its own 7-day sweep, or `sessions gc --jobs-older-than` would delete it after a crash, and recovery would find nothing to re-pend. `origin_cli` (comment in migration `20260928000002`: "recovery routing") is not read by recovery: it only stamps the origin of the notice message a pull writes (`JobNoticeRow.OriginCLI`, `agent_notice_pull.go`); routing is by `host_id` and the host lock. `announce_message_id` (migration `20260929000003`): written by `AnnounceStarted` in the ack gate's transaction. Consumer: `session.TruncateForRerun` (`internal/session/async_job_rerun.go`) re-pends delivered rows by `notice_message_id`, voids by `announce_message_id` (legacy rows with NULL fall back to the deleted tail's `tool_call_id`), all from the message rows that transaction actually deleted. | `TestPullJobNotices_InsertsMessageWithNoticeInvariant` (`internal/session/notice_pull_test.go`); `TestAnnounceJobKillResult_NamesTheRowAndSurvivesRerunRepend`, `TestAnnounceJobKillResult_LostRaceStillPersistsMessageWithoutFusing`, `TestRependJobKillRowWithoutNotice`, `TestAnnounceJobKillResult_ReusedToolCallID_NamesTheKilledRowNotTheNewOne` (a reused `call_0` while job_kill runs: the archived row is named, no duplicate delivery after host death, Rerun re-pends it), `TestRependJobKillRowWithoutNotice_ReusedToolCallID_RependsTheKilledRow` (`internal/session/async_job_kill_notice_test.go`); `TestPersistToolResult_JobKillWithReusedToolCallID_NamesTheKilledRow`, `TestPersistToolResult_JobKillErrorResultWithReusedToolCallID_RependsTheKilledRow` (through `persistToolResult`, `internal/agent/work_ledger_job_kill_reuse_test.go`); `TestSweepDeadHosts_RependsJobKillRowWithoutResultMessage` (`internal/session/async_job_recovery_test.go`); `TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery` (`internal/session/async_job_gc_jobkill_test.go`: the unnamed row survives `PurgeJobsOlderThan` and `PurgeExpired`, `RecoverDeadHost` re-pends it); `TestAnnounceStarted_RecordsAnnounceMessageID`, `TestTruncateForRerun_VoidByAnnounceMessage_ToolCallIDReuse`, `TestTruncateForRerun_LegacyRowWithoutAnnounceIDVoidedByToolCallID`, `TestTruncateForRerun_SetsFromActuallyDeletedRows`, `TestTruncateForRerun_InjectedFailureBeforeCommit_ChangesNothing`, `TestTruncateForRerun_RependsNoticesVoidsWakeFailed`, `TestTruncateForRerun_VoidBeatsRepend` (`internal/session/async_job_rerun_test.go`). | Выполняется — including a `job_kill` whose `tool_call_id` was reused before its result message was written (R3A-2: named and re-pended by claim). A row delivered before migration `20260929000003`, or acknowledged through the plain (non-fused) path, has a NULL `announce_message_id` and only the legacy tool_call_id arm can void it. Tool side (round 2): when the fused write does not happen at all (the result is an error, the transaction failed, the context was cancelled) `workLedger.persistJobKillResult` re-pends the row through `AsyncJobStore.RependJobKillRowWithoutNotice` (delivery=pending, reacted=0, wake=0).
163:`childSessionID`'s turns; `workLedger.childScopeDrained`
164:(`internal/agent/work_ledger_delegation.go`) consults that driver
185:| SCHED-05 | Cancel идемпотентен и не воскрешает: повторная отмена — no-op, чужая сессия получает отказ, отменённое никогда не захватывается и не срабатывает; `sessions reset` отменяет все активные расписания владельца в транзакции сброса. | `WakeScheduleStore.CancelSchedule` (owner-checked CAS on `state='active'`), `CancelAllWakeSchedulesForOwner` wired into `AsyncJobStore.ResetOwnerHistory` (`internal/session/async_job_reset.go`); a session DELETE cascades via the FK (`wake_schedules.owner_session_id ... ON DELETE CASCADE`). | `TestWakeScheduleStore_CancelIdempotentAndOwned`, `TestWakeScheduleStore_ResetCancelsActive`, `TestWakeScheduleStore_SessionDeleteCascades`. | Выполняется. |
```

### `docs/checkpoints/2026-06-25-2218.md` (4 hits)

```
15:- **6390224a** pause stall-watchdog during tool exec + non-zero `crush run` exit
22:  (`Options.StreamToolTimeoutSeconds`, 15m default), bounded `job_output --wait`
24:  job_output. Plus `docs/design/event-loop-turn.md`.
56:- #72 watchdog tool-cap, #73 job_output bounded, #74 responsive polling (→ bb9a3f8e)
```

### `docs/checkpoints/2026-06-26-0831.md` (2 hits)

```
17:job_output tests under `-race`).
80:  pre-push hook omits `-race`; CI caught a job_output race the hook couldn't.
```

### `docs/checkpoints/2026-07-19-0729.md` (3 hits)

```
86:   `.crush` dir): the session's last real tool call was `job_kill
90:   fork code: `job_kill` → `shell.BackgroundShellManager.Kill()` →
129:   IRabi project's pre-fix `job_kill` calls (the exact bug class just
```

### `docs/checkpoints/2026-07-30-1400.md` (1 hits)

```
9:**Параллельно (побочный тред работы)**: пользователь попросил расследовать, почему сессия `crush run` дважды упала в другом репозитории (`D:\dev\rust\tor-socks5\.crush`, сессия `guard-desc-retry`). Расследование (через `crush sessions why/last/show` + код-ревью суб-агентом типа Explore) выявило: весь процесс `crush.exe` умирал БЕЗ единой строки в `crush.log` (паника Go идёт в os.Stderr, не через slog) дважды — оба раза сразу после вызова инструмента, управляющего асинхронной работой (`agent`-делегирование саб-агенту, и `job_output` чтение фонового job). Корень: горутина в `internal/app/app.go:1150` (`go func(){ app.AgentCoordinator.Run(...) }`) НЕ обёрнута в recover() — задача #154 (эта же сессия) добавила recover() ТОЛЬКО для WS-хендлеров веб-сервера, CLI-путь `crush run` остался незащищён. По итогам заведены задачи **#177** (добавить recover() в горутину агента + путь саб-агента + background.go callbacks) и **#178** (гарантировать что паника попадает в crush.log, а не только в stderr) — обе добавлены В КОНЕЦ существующей цепочки (`blockedBy: #176` и `#177` соответственно), чтобы не пересекаться файлами с уже запланированной работой.
```

### `docs/checkpoints/2026-08-03-1031.md` (1 hits)

```
16:The remaining 5 (NEW-A: a blocking `onFire`/`DumpGoroutines` disk write could theoretically prevent the stream watchdog from ever cancelling; NEW-C: the same cwd-hardcoding bug just fixed for `sessions locks` survives in 5 sibling commands — `list`/`reap`/`watch`/`why`/`queue`; NEW-D: a silent `os.Remove` failure after a successful death-probe leaves a fresh-looking-but-actually-dead lock file with no error surfaced; NEW-E: the PID-liveness fallback from #228 has no time bound, so a reused PID could in principle pin a session read-only in the web UI forever; NEW-F: a duplicated `"Stream stalled"` string literal between the watchdog's finish-message logic and the coordinator's retry-matching logic could silently break transparent stall-retry on an innocent future rewording) were presented to the user, who — given this was now a fourth review pass and all remaining items were confirmed low-severity — chose to file them as tasks and stop, rather than continue a fourth autonomous fix round. Filed as tasks #232-236 with full architectural context (exact file:line references, concrete failure scenarios, and — for the two that are genuine design tradeoffs, #235 and #232's second half — explicit instructions to think through the tradeoff rather than mechanically implement, matching the precedent task #230 set).
```

### `docs/checkpoints/2026-09-28-0029.md` (2 hits)

```
9:In flight: #1053 (P1: the model could not stop a running async command because job_kill/job_output needed an internal shell_id) — implemented by an sx agent in worktree `worktrees/fix-job-kill-by-id` (branch fix-job-kill-by-id, based on 8efbac67); a full agent/app test run in that worktree was started in the background (task bml8hdzhl) and its result is not yet known. Pending review change before merging: `ResolveJobShellID` returns "still starting; try again" forever for run_command jobs (they never get a shell) — must return a clear "run_command jobs cannot be stopped via job_kill yet" error; stopping run_command belongs to wakes stage 2 (#1023). After #1053 merges: phase 2 (#1046), then phase 3, 4, supervision, wakes stages, and last the UI tasks (#1055–#1057, user asked UI work to go at the end).
18:- #1053 P1: корень не может остановить запущенную async-команду (job_kill ждёт shell_id)
```

### `docs/checkpoints/2026-09-28-1711.md` (3 hits)

```
5:The session continues the async structured-concurrency refactor of Rush (design `docs/plans/2026-09-27-async-structured-concurrency.md`, laws `docs/async-invariants.md`). Merged to main and verified since the previous checkpoint: #1053 job_kill/job_output by job id (da33bcb4), phase 2 unified delivery (62647444), wakes stage 2 command control incl. run_command tree-kill (593bcf2d), phase 3 event-driven scope without polling/ticker (998fd465), supervision tick (33f15959), a Windows config rename retry fix found through a flaky test (50ea2b6f). Main was pushed once (without hooks, at the user's request) up to da33bcb4; everything after is local only.
24:- #1072 Ф4-6: Stop по дереву, Rerun, claim делегации с ASYNC-01, ack gate, job_kill (blockedBy: #1071)
31:- #1063 Бэклог P3: текст «stopped (job_kill)» и гонка MarkJobStopped (blockedBy: #1048)
```

### `docs/checkpoints/2026-09-29-0925.md` (4 hits)

```
7:Ф4-9 is in progress: a full verification run of the merged branch is running in the background, and round 1 of ox code review has been launched as three read-only parallel reviewers (A durable core: SQL/schema/store/host lock/recovery/reader; B agent layer: transition ledger/pull/Drain/debt/policies/settle/ack gate/Stop/Rerun/shutdown; C: CLI loop, sessions commands, server, message service tx variants, registry/CHANGELOG accuracy). Per the operator's explicit exception to the Review Stop Rule, rounds continue until zero P0–P3 findings, fixing P2/P3 too; fixes go to sh agents in the phase4-core worktree. Known items to hand to the review/fix round: DUR-8 is proven directly only for the no-store case (a closed-DB test would be stronger); when the ack-gate row is gone (`ErrAsyncJobGone`) no tool-result message is persisted (P3, `OrphanedToolUse` repair exists); scenario 13 test (Stop suspends auto-turns) lacks an independent revert-check.
22:- #1063 Бэклог P3: текст «stopped (job_kill)» и гонка MarkJobStopped — по сути сделано в Ф4-6, закрыть после влития
33:- #1072 Ф4-6: Stop по дереву, Rerun, claim с ASYNC-01, ack gate, job_kill
46:- `job_kill` result is the tool's own answer; row goes straight to `delivery='done'`, `reacted=1`, `wake=0` (done in Ф4-6, replaces the temporary step-3 behaviour).
```

### `docs/checkpoints/2026-09-30-0810.md` (3 hits)

```
5:The session drives the async structured-concurrency refactor of Rush (design `docs/plans/2026-09-27-async-structured-concurrency.md`, laws `docs/async-invariants.md`). Phase 4 (durable SQLite work registry, design `docs/plans/2026-09-28-async-phase4-durable-core.md` rev 7 plus its "Amendments after review round 1/2") is implemented on branch `phase4-core` (worktree `worktrees/phase4-core`, HEAD `67f4b57f`, pushed once as a backup branch at `d2f22830`; NOT merged into main). Since the last checkpoint three full ox review rounds ran (round 1 ~70 findings, round 2 ~44 incl. 4 P1, round 3 ~22 code findings: 1 P1, 2 P2, rest P3; no P0 — the trend converges) and about fifteen fix branches (store, drain, drain2, drain3, ledger, rerun, docs, fix2, lease, store2, ledger2, attempts2, cmd2) were merged one by one, each verified zero-trust by me (diff, revert-checks, full suites through the serialising wrapper `.rush/audit/heavy.sh`). Notable things found by me during merges: a finalizer-released host lock in `CloseKeepLock` (fixed with a pin registry), a signature drift that silently disabled the job_kill re-pend (fixed + compile-time assertion), two semantic-merge compile errors in a test, and three load-dependent flakes fixed at their mechanism (lease renewal with an injectable pump clock, `sessions locks` probe-cleanup wait, cancel-after-clean-turn returning nil). The Drain launch accounting was redesigned with `ox` into one accounting point (`accountDrainAttempt`) and one launch predicate (`drainPermitted`) with design file `docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md`.
7:In flight now (#1090, round 3 fixes, three sh agents in parallel worktrees, all tests serialised through `.rush/audit/heavy.sh`): `store3` (`p4r3-store3`: R2C-11 driver marker DB errors, job_kill keyed by claim_id, recovery lock hold/false "alive" driver, vacuous tests), `agent3` (`p4r3-agent3`: network timeouts wrongly exempt (P2), child question with text reaches parent as failure (P2), interrupt-cancel counted, dormancy streak, K-close per row, bg-shell cap bypass, OAuth refresh for web/child Drains, bySession cleanup, interface assertions), `app3` (`p4r3-app3`: reviewer pass after last Drain drops the answer (P1), first-turn failure after claim, Drain answer lost on Ctrl-C, model persistence on Drain, sub_agent_outputs dedupe, silent paced retries, stderr routing, `--continue --role` double resolve, plus a known gofumpt issue `internal/server/descendant_work_batch_test.go:83`). Full verification of the merged tree `21c3a166` was green (all packages ok, lint 0 except that gofumpt, file-size ok) and flake loops passed (app x3, cmd x3, session x2). Open flake task #1083 (`TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob`, failed once at 61s in a full-package run, never reproduced in 50+ isolated and several full-package runs; a goroutine/request-order dump was added to the test for the next occurrence). After the three agents: merge them (resolve docs conflicts row by row against the merge base), full verification + flake hunts, then review round 4 with three ox reviewers, repeat until zero P0–P3, then fast-forward main to `phase4-core`. The babysit cron 582cac33 (every 15 min, session-only) is armed.
28:- #1063 Бэклог P3: текст «stopped (job_kill)» и гонка MarkJobStopped  (blockedBy: #1048)
```

### `docs/checkpoints/2026-09-30-2008.md` (9 hits)

```
9:- **Фаза 4** — «долговечное ядро»: одна правда в строке SQLite-таблицы `async_jobs`, долг реакции, замок ОС вместо часов, восстановление после падения, drain-цикл CLI по БД. Дизайн: `docs/plans/2026-09-28-async-phase4-durable-core.md` (ред. 7 плюс поправки (a)…(ap)). Реализована в ветке `phase4-core` (worktree `worktrees/phase4-core`) шагами Ф4-1…Ф4-8.
30:**Предупреждение `ox` перед `deploy.go`.** Перед установкой слитого бинаря проверить реальные data dir: `PRAGMA table_info(async_jobs)` и `goose_db_version`. Data dir, жившая на сборке `phase4-core` до коммита `528a8d24`, не имеет колонок `tool_name`, `timeout_seconds`, `wake_attempts`, `reacted_failed` (все async-задачи упадут «no such column»). Data dir от отвергнутой ветки `async-phase4` падает на шаге goose при старте. Такие каталоги пересоздать.
66:| 1 | Единый реестр работы в памяти (workLedger) | готово | `main` |
75:| Ф4-1 | Схема SQLite (`async_jobs`, `async_hosts`, `session_notices`), файл-замок хоста | готово |
80:| Ф4-6 | Stop по дереву, Rerun, claim делегации (ASYNC-01), ack gate, `job_kill` | готово |
123:- **Одна правда.** Строка `async_jobs` — единственный источник состояния задачи. `Transition` — SQL CAS по `(owner, tool_call_id, claim_id)`. Поля outbox: `delivery` (none/pending/done/void), `wake`, `reacted`, `reacted_failed`, `wake_attempts`, `announced`, `notice_message_id`, `announce_message_id`. Таблица `session_notices` хранит уведомления без задачи (bg-shell, wake_failed и т.п.).
128:- **Replay tool_call_id:** старая строка архивируется как `call_0#reused#uuid`. `job_kill` пишет id сообщения по claim (R3A-2), неименованную done-строку `job_kill` ретеншн не удаляет (R5A-1).
176:- #1063 Бэклог P3: текст «stopped (job_kill)» и гонка MarkJobStopped  (blockedBy: #1048)
194:- Перед `go run deploy.go` (#1092) проверить реальные data dir по предупреждению `ox` (`PRAGMA table_info(async_jobs)`, `goose_db_version`).
```

### `docs/checkpoints/2026-10-01-1351.md` (4 hits)

```
16:- `a4be6886` — `job_output` больше не приглашает к опросу (A13); задеплоен, rush = `a4be6886`;
40:- #1126 R-BG-1: фоновые shell как строки async_jobs (blockedBy: #1122)
48:- #1132 Деплой rush с фиксом job_output (a4be6886)
87:a4be6886 fix(tools): job_output no longer invites polling a ledger job
```

### `docs/checkpoints/2026-10-01-2114.md` (6 hits)

```
9:`03060c5c` #1133 время сброса лимита в поясе машины · `73c3ce7f` R-ACT-2 (все `sessions *` берут живость из классификатора; починен флейк reset --force: reacquireAfterKill) · `c93b98f6` #1134 флейк подсчёта горутин · `9ae59561` R-ACT-3 (web-список сессий из классификатора, один батч на опрос; AnnotateSessionExternalOwnership читает только lock-факт) · `90d40ea9` #1139 cache_support по ID провайдера (zai) · `ea3a979a` R-ARB-2 (все пути запуска автохода через арбитр; три P1 от @oxx исправлены: CommitRefused не сбрасывает гейт, holds переживают сброс по сообщению человека, over-cap из одного чтения под bgArrival) · `5dc7273b` дизайн shared-data-dir · `c49f79fc` #1138 заголовок сессии на z.ai (thinking-опции + reasoning-fallback) · `873a9710` A19 (`sessions locks`: между ходами = between-turns при любом возрасте lock-файла) + флейк stdin-теста (паузы 80→350мс) · `3ef0109e` #1140 SD-A (rush.log: O_APPEND вместо lumberjack, атрибут ws, process start) · `e21ed908` #1137 B1 + A13 (job_output: pending-доставка отвечает текстом; wait на своей running ledger-задаче завершает ход StopTurn; флейк latch-теста сделан детерминированным; чтение БД вынесено из-под l.mu с таймаутом 2с).
25:- #1130 R-LOOP-2 реализация (rloop) · #1131 A14 окно (wa14) · #1135 ask_question vs running jobs (A15; ч.2 job_output уже в main; ч.1 в wloop) · #1145 A18 todos (wloop) · #1146 A10 final_text/review (wloop) · #1147 git-записи/INDEX_LOCK/имена инструментов (wguard)
31:- #1126 R-BG-1 фоновые shell как строки async_jobs · #1129 флейк FK в durable enqueue (1/3 полного internal/agent) · #1127 R-DOC · #1148 зависание полного internal/cmd 25 мин (1 раз из 4, стек потерян — все полные прогоны cmd теперь с `-timeout 8m` и логом в .rush/audit/inv/) · #1065 раунды ox-ревью (только по просьбе) · #1094 dogfooding-напоминание (постоянное)
44:Починено в main: A13/B1 (job_output), A19 (locks stopping), B2, B3, флейки (горутины, latch-тест, stdin-тест, reset --force), A21 (heavy.sh повторяет golangci-lint при «parallel golangci-lint is running», заменён атомарно). В работе у агентов: A15, A18, A10 (wloop), A17, A8, имена инструментов (wguard), A14 (wa14). Открыто/наблюдение: A16 (TaskStop не убивает дерево процессов Windows — убивать orphan `sh`→`capm`→`go test` по PID вручную), A20 (агенты нарушают правила: полный go test без heavy.sh; echo-idle после финала), #1148, `index.lock` в worktree агента после убитого `git status` (A8; снимал вручную — пустой замок без git-процессов), 5-часовой лимит z.ai (13:04→14:34 и ~18:30→19:34; квоту делят мои 6 агентов + чужие rf3-mig-*/w30-*; будильник/возобновление работают; при новом лимите — остановить и поставить будильник).
45:Метрики аудита: 45 из 55 прогонов показывали cache_support none при реальных 96% hit; до 59 job_output на 19 bash за прогон (~35% токенов); автопродолжения ≥ половины ходов (→ #1131).
59:e21ed908 fix(agent): job_output no longer lies about undelivered jobs or invites polling
```

### `docs/checkpoints/2026-10-02-0510.md` (1 hits)

```
27:- #1126 R-BG-1 фоновые shell как строки async_jobs (`docs/plans/2026-10-01-bg-shell-ledger.md`) · #1127 R-DOC (после #1126)
```

### `docs/checkpoints/2026-10-05-0755.md` (3 hits)

```
4:Продолжение долгой сессии «чиним rush через rush» (цель оператора, `/goal`: решить все таски, детектировать аномалии/баги/шероховатости в сессиях, заводить таски и решать их; решаем с помощью /wrush). За этот сегмент (с чекпоинта 2026-10-04-2117) влито в `main` ~30 коммитов, каждый отдельным коммитом на задачу: #1174 (WARN OAuth Codex), #1142 SD-C (привязка сессий к workspace), #1168 (флейк heartbeat), #1171/#1173 (единый предикат видимости моделей: openai-codex gpt ≥ 6, GLM ≥ 5.3), #1143 SD-D (linked worktree → общий каталог данных `<main>/.rush`, живая проверка бинарём на временном репозитории), #1144 SD-E (тексты /rush и /wrush про проектные id сессий и `sessions fork --cwd`), #1157 (вопрос воркера доходит до оркестратора, ответ в удерживаемую делегацию), #1161 (`rush sessions compact`, VACUUM по явной команде; дизайн @om), #1167/#1175/#1168 (флейки, найдены по механизму), #1177 (сквозной тест вопроса воркера), #1158 («ждёт ответа» в why/list/web), #1126 (bg-shell как строки async_jobs; миграция перестройки таблицы проверена на копии настоящей БД), #1127 (дизайн фазы 4 в редакции 8 + реестр законов), #1179 (sessions audit читал секунды как миллисекунды — найдено живым прогоном), #1180 (кэш каталога Codex отдавал контекст 272k: нормализация при выдаче из кэша; печать контекста с двумя знаками 1.05M), #1176 (полный `go test ./...` по всем 49 пакетам: нашёл регрессии #1142 в internal/agent/tools и #1165 в internal/platform — исправлены). Установленный бинарь `af3afd44` (последний деплой; в main дальше только docs-коммиты `06ba112d`). Новые worktree агентов теперь пишут в общую БД проекта (сессии видны из корня `rush sessions locks`), удаление worktree больше не теряет историю. Модели: GLM упёрся в недельный лимит (сброс 2026-10-08 09:33), по указанию оператора переключены слоты: smart = openai-codex/gpt-6-luna@xhigh, worker = openai-codex/gpt-6-luna@low (fast stepfun неактивен, reviewer glm не трогали) — теперь `--role smart` работает как оркестратор (нет edit/write, делегирует worker'у по одному). Идёт агент `wmeta` (worktree `worktrees/wmeta`, сессия `wmeta` в общей БД) по #1181: рефакторинг «один источник метаданных моделей» (дизайн @om `docs/plans/2026-10-05-model-metadata-single-source.md`: таблица `discover/model_facts.go` + одна функция наложения; GLM-5.3 семейство → 1M; golden-тест снят ДО рефакторинга; метки атомов следуют рантайму — решение принято по рекомендации). Журнал аномалий: `.rush/audit/rush_quality.md` (A36–A38), постоянные указания оператора: `.rush/audit/standing-orders.md`. Локально впереди `da16503f` 46 коммитов, push не делался.
13:- #1182 остаток R-BG-1 (backlog P2, нужен дизайн-ход у @om): удержания завершения bg-shell (`PendingCompletionsOwned`) и `job_kill` по голому shell_id не пишет cancelled
40:faa2101f feat(agent): a background shell started on the sync branch of asyncTool claims an async_jobs row (#1126, R-BG-1)
```

### `docs/checkpoints/2026-10-05-1331.md` (2 hits)

```
4:Продолжение долгой сессии «чиним rush через rush» (цель оператора, `/goal`: решить все таски, детектировать аномалии/баги/шероховатости в сессиях, заводить таски и решать их; решаем с помощью /wrush). С чекпоинта 2026-10-05-0755 влито в `main` 12 коммитов, по одному на задачу, каждый проверен зеро-траст (diff, мутанты, полные пакеты, линт, кросс-сборка): #1181 единый источник метаданных моделей + GLM-5.3 = 1M (`ebac0e8f`; golden до рефакторинга снят на чистом main и отличается только тремя строками glm-5.3*), #1183 нулевая запись модели в rush.json больше не затирает значение каталога, #1184 24 замечания линта в internal/oauth/codex, #1185 и #1186 подсказки оркестратору (занятый воркер, пропущенный child_session_id), #1190 effort `max` у z.ai/DeepSeek валил каждый проход ревью («reasoning model `max` not supported»: top-level reasoning_effort проверяется fantasy по словарю OpenAI), #1191 сбой потока Codex server_is_overloaded больше не убивает ход воркера (статус 503/500/429 → повтор), #1188 (P1, регрессия моего #1126) строку bg_shell закрывает наблюдатель, зарегистрированный при claim, а не колбэк уведомления, #1192 `write` отказывает при усыхании большого файла более чем на 75% (параметр allow_shrink), #1194 вопрос воркера в логе INFO вместо ERROR, #1195 флейк TestWS1_Pump_ForeignWorkspace (тест читал last_error после Stop; механизм найден повтором -count=25 -parallel 4). Установленный бинарь `0ea2ee51`; в main после него ещё #1191/#1188/#1192/#1194/#1195 (не задеплоены). Работа вне сессии ждёт: #1189 (P1, job_kill по shell_id) реализован в worktree `worktrees/jobkillbg` (не закоммичен в main): StopBackgroundShellRow переводит строку bg_shell в cancelled ДО убийства процесса, BGShellStopper в tools/job_kill, notifyBackgroundJobDone уступает для cancelled-строки; тесты (4 на реальном ledger/store/shell + 5 на уровне инструмента), все 6 мутантов K1–K6 красные, полный internal/agent зелёный (437 с), идут agent/tools, app, session, vet, линт, кросс-сборка (скрипт `.rush/audit/inv/verify-jobkillbg.sh`, лог `verify-jobkillbg.log`). Далее по плану: влить #1189 (CHANGELOG вставкой в main), задеплоить пачку, полный прогон всех пакетов (#1187-стиль) на новом main, закрыть остаток #1182 ограниченным повтором вставки notice bg_shell_done (вместо рефакторинга «одна транзакция» — после #1188 строка и notice пишутся независимо). Аудит логов и сессий за день дал A39–A46 (журнал `.rush/audit/rush_quality.md`), из них исправлены A39, A40, A42, A43, A44; A45 (ERROR при дедлайне очистки на занятой БД) и A46 (повтор 429 «Weekly/Monthly Limit Exhausted» внутри fantasy) решено не чинить. Оператор по ходу: ответил на вопрос про `max` (это баг rush, слот reviewer @max допустим) и записал в свой rush.json исправленные контексты моделей z.ai (правка согласована его словами «нужно прописать правильный контекст»; бэкап вне репозитория). Агенты rush в режиме оркестратора (smart luna) показали слабое качество (пустые тесты, затёртый файл) — довожу работу сам. Локально впереди `da16503f` ~60 коммитов, push не делался.
11:- #1189 P1: job_kill по shell_id оставляет строку bg_shell running (процесс не умирает, R8B-2) либо пишет failed + лишнее пробуждение — реализовано в worktrees/jobkillbg, идёт проверка (agent/tools, app, session, vet, линт, кросс-сборка)
```

### `docs/checkpoints/2026-10-05-2020.md` (1 hits)

```
4:Продолжение долгой сессии «чиним rush через rush» (цель оператора, `/goal`: решить все таски, детектировать аномалии/баги/шероховатости в сессиях, заводить таски и решать их; решаем с помощью /wrush; Stop-хук не принимает остаток бэклога как «готово», поэтому работа продолжается по бэклогу раунда 9). С чекпоинта 2026-10-05-1331 влито в `main` ещё 19 коммитов, каждый проверен зеро-траст (diff, мутанты, полные пакеты, линт, кросс-сборка): #1189 job_kill по shell_id (строка → cancelled до убийства), #1196 гард усыхания fs_write, ox-ревью раунда 9 по дельте ea3a979a..4874fb73 (три ox; отчёт `docs/reviews/2026-10-05-async-round9.md`) → 4 P1 исправлены (#1197 ClaimShell оставлял running-строку при сбое announce, #1198 паника ExecuteRun при провале ревью в terse/stream без captureResult, #1199 напоминание о todos поверх ask_question, #1200 loop-расписание после первого закрытия области), флейки #1201 (MCP: 1 с → mcpAwaitTimeout; app: скрипт корня в ChildQuestionRoundTrip отвечал по номеру хода, а вопрос мог прийти уже в шаг yield; дедлайны двухприложенных сценариев 30→90 с), бамп версии до 0.2.0-alpha.8 (по просьбе оператора; тег не создан), push main без хуков до 4874fb73 (по просьбе оператора) — 20 коммитов после него локальные, и три P2 из бэклога через /wrush с агентами rush (glm-5.3-flash во всех слотах): #1202 (обрыв SSE Codex → retryable), #1203 (код выхода async-команды в evidence ревьюера), #1204 (дамп каталога в rush.json не считается «заданным пользователем»; хелпер forgetDumpedUserSet для известных провайдеров и живого каталога Codex). После них заведены задачи под остаток бэклога раунда 9: #1208 влит (c6ab8a27, fs_write не накапливает усыхание по элементам); сейчас идут 6 агентов rush в отдельных worktree (worktrees/w1205, w1207, w1210–w1214, сессии с теми же именами; промпты `.rush/stdin/<id>.prompt`, результаты `.rush/stdin/<id>.out`, лог `.err`): #1205 (предупреждения ревьюера теряются в applyTo), #1207 (finishBGShellRow бесконечно повторяет ErrNoRows — агент закончил, моя проверка зелёная: мутант красный, agent целевые тесты 88 с, линт 0, но НЕ влит), #1210 (ClaimShell: чужой host_id → RecoverDeadHost/ErrBGShellIDCollision + guard в StopBackgroundShellRow), #1211 (CLIScope учитывает PendingCompletionsOwned первым чтением), #1212 (P1, подтверждено консультацией @om: простаивающий web/вложенный родитель отвечает на child_question — перехват answerHeldDelegation закрыт условием !sync, ход виснет в awaitSync, итог дважды; фикс: убрать !sync, guard job.sync, origin ответного хода по X), #1213 (home-флаг определять по каталогу, не по источнику), #1214 (WorkspaceRoot: не кешировать ошибку запуска git + обход .git; ставка «не-git → cwd» отложена). Ещё не начаты: #1206 (сбой хода-напоминания не должен менять итог прогона; после #1205 — те же файлы app), #1209 (SubtreeSpent при сбое чтения; текст промпта напоминания «mark cancelled»). Установленный бинарь: `0.2.0-alpha.8 (a0d8b19e)` — в нём #1202–#1204; не задеплоены #1208 и всё, что ещё не влито. Последний полный прогон 56 пакетов на a0d8b19e: 45 ok, 11 без тестов, 0 падений. Dogfooding: три агентских задачи (#1202–#1204) дали корректные диффы при точных ТЗ, но встроенный ревьюер агента дважды ставил FAIL по реальным причинам (неполная матрица тестов, непокрытый production-путь Codex), мутанты выявили недооценённые пути (max-tokens не наблюдаем через факты) — оркестратор доделал; новых rush-дефектов нет (A51–A53 в `.rush/audit/rush_quality.md`). Правила: heavy-команды только через `heavy.sh capm 2g`, мутанты на Python с явным Git bash, один worktree на задачу, cherry-pick -n в main + строка CHANGELOG под `## [0.2.0-alpha.8] - 2026-10-05`, push только по просьбе.
```

### `docs/checkpoints/2026-10-06-0530.md` (1 hits)

```
4:Продолжение цели оператора («решить все таски, детектировать аномалии/баги/шероховатости в сессиях, заводить таски и решать их через /wrush»). С чекпоинта 2026-10-05-2020 разобран весь бэклог раунда 9: влиты #1205–#1214 (потеря предупреждений ревьюера, сбой хода-напоминания, окно наблюдатель/notice, ответ на `child_question` по origin — P1, home-флаг по каталогу, `WorkspaceRoot` без git, коллизия id фонового shell между процессами) и все P3 (A9-12 аренда wake, мёртвый запрос, `job_kill` hold и гонка, recheck наблюдателя, наследование effort ревьюера, evidence ревьюера, sessions cost/list/compact, Ctrl-C в login, терс-режим SDK). Триаж остатка P3 сделали три read-only консультанта `@ox` (17 настоящих, 16 закрыты с причинами), итог записан в `docs/reviews/2026-10-05-async-round9.md`. По просьбе оператора для ревью подключена роль `rush --role reviewer` (sol 6.1) вместе с `@ox`: дельта-ревью серии (6 прогонов, три области) нашло две P1-регрессии в самих фиксах — отмена оператора, проглоченная веткой сброса хода-напоминания (#1227), и потерянный `--effort` при `--model B` (#1230, регрессия #1221), обе исправлены в цикле, плюс P2 (#1228, #1229, #1232) и закрепляющий тест #1226 (расследование: потерянный вопрос не дефект, держится на `suspendAutoResume`). Инцидент: влитый #1233 (усечение текста в `onRetry`) сломал continuation (F3) в `internal/app`; поймал финальный прогон, откат `ef528899`, переделан лениво (#1235, `d261f561`), урок A57 — перед слиянием менять-потребителей запускать полный `app`. Дисциплина: каждый результат агентов rush проверял сам (диф, мутанты, полные пакеты, линт); тесты нескольких агентов были бесполезными (проверяли строку сессии, а не то, что уходит в запрос), две задачи доделал сам. Итог: `main` на `d261f561`, 42 локальных коммита после `4874fb73`; финальный прогон 56 пакетов — 45 ok, 11 без тестов, 0 падений; бинарь задеплоен: `0.2.0-alpha.8 (d261f561)`. Открыто: аномалия A58 (один необъяснённый красный полный `agent` в worktree #1235 при трёх зелёных повторах; имя теста не сохранено) — финальный прогон на main её не воспроизвёл. Ограничения агентов: потолок 60 мин и 5-часовой лимит провайдера (A54/A56) — тяжёлым агентам давать `--timeout 90m`, держать ≤3 параллельно.
```

### `docs/checkpoints/2026-10-06-1834.md` (2 hits)

```
8:Главный инцидент дня: мой `golangci-lint run ./internal/config/...` под `capm 2g` упал OOM (VirtualAlloc errno=1455) и висел зомби 2,5 часа, держа замок `.rush/audit/heavy.sh` — из-за этого агенты w1251/w1252/w1253 исчерпали таймауты; я убил свои процессы по PID. Принципиальный ответ (оператор: «никогда больше»): правило «ни одно ожидание не бесконечно и не немо; кто-то должен проснуться и решить; если будить некого — убить, пару раз перезапустить, потом умереть». Реализовано в `heavy.sh` (#1253, локальный git-ignored скрипт, бэкапы heavy.sh.bak-20261006/.bak2): лимиты по классам (lint 20м, vet/build 15м, test/прочее 45м → код 124), детектор замороженного дерева процессов (`.rush/audit/tree-stats.ps1`, 5 мин → 125, для lint/vet/build), уход из очереди, если родитель rush.exe умер (код 3; только для rush-родителей — шелл Claude выходит сразу), раз в минуту строка «кого жду с какого времени», lint с `GOMEMLIMIT=1500MiB GOMAXPROCS=2` (замер: lint config 117 с, пик 1504 МБ — в 2g влезает), статистика в `.rush/audit/heavy-stats.csv`. Агент #1254 (w1254, дизайн @om GO-WITH-CHANGES): check-in supervision показывает текст команды, возраст, «silent N», флаг «SILENT, possibly stuck» (возраст ≥30м и молчание ≥ max(20м, возраст/2)), решение «проверь job_output, живой процесс ≠ прогресс, иначе job_kill», эскалация на 2 тике, `job_output` «last output N ago», опциональный `--job-timeout`. Агент #1255 (w1255, дизайн @om + две поправки оператора через `sessions inject`): фаза 0 — `sessions why/locks` показывают возраст пульса, открытые tool calls, STALLED; фаза 1 — два таймера (живость/долговечный прогресс), фаза хода, сэмплер 15 с, дамп горутин 0600; зависший синхронный инструмент НЕ отменять, а через 10 мин увести в фоновую job и разбудить модель «выполняется уже 10 мин, job <id>; жди job_output или job_kill»; провайдер — 2 повтора затем stalled; ревьюер — 2 перезапуска затем «review failed», процесс выходит; internal — дамп и abort.
10:Расследование clawforge (D:/dev/Bell/open_claw/clawforge, сессия rf7-s02, промпт rf7-s02c): rush run PID 45980 завис в проходе ревьюера после 17:27:26 (lock mtime 17:27 — пульса не было; в stderr начаты `glob rf7-s02-proof3/**` и `ls tools/checks/kit` без результатов → гипотеза «завис синхронный инструмент, сторож простоя на паузе до 45 мин»), `sessions why` врал «running, in turn»; оркестратор той папки убил процесс ~17:57; сейчас rf7-s02 перезапущена и жива. Причина зависания не доказана (дампа нет). Таймеры: babysit `7aa0f086` (7,37 мин), stall-check `dc40c8c9` (17,37,57), оба session-only. Модели: агентам достаётся слот smart (glm-5.3-flash) — оператор не подтвердил.
```

### `docs/checkpoints/2026-10-07-0554.md` (4 hits)

```
6:Acceptance pattern every time: agent (rush run --role smart, model zai/glm-5.3-flash) in its worktree → I rebase onto main, run full packages + lint through `.rush/audit/heavy.sh capm 2g`, read the diff, run mutants (`.rush/audit/inv/*-mutants.py`), add tests for survivors, then cherry-pick -n into main + CHANGELOG (English) + commit, remove worktree. Agents routinely skipped full-package runs; real defects found in acceptance: w1255 had 5 (double --restrict-run gate through the new detach wrapper; tool detach only at 30 min / never with --stall-timeout 0 — fixed to the 10-min warn threshold per operator; provider stall retries 15 s apart; `sessions why` lost its status line; nil stall clock panic), H2 had a keepalive root-attribution bug and a test-only real-global-dir leak (heartbeatDir testing guard), w1252 README leaked the hidden lock key name (rewritten).
10:In flight: agent `w1259` (worktree worktrees/w1259, session w1259, launched ~04:58, timeout 200m) on #1259 accumulated P2 backlog: (1) heartbeat source label via resolvedOverrides + withModelSource in runInternal, (2) one shared heartbeat dir helper, (3) rate-limit wait visible in `rush run` stderr + heartbeat rate_limited_until in `sessions why`, (4) stallDetachJobs cleanup + panic in a detached tool must complete the job. When it finishes: rebase, full agent/app/cmd/heartbeat + lint, mutants, merge, CHANGELOG. Timers: babysit `7aa0f086` (7,37) and stall-check `dc40c8c9` (17,37,57), session-only. TEMP also holds 1717 `goroutines-*.txt` dumps written by rush when no log dir is set (likely tests) — not yet a task.
33:- Tool detach for a stalled synchronous tool fires at the 10-min warn threshold regardless of --stall-timeout (operator: wake the model, never cancel); abort only past --stall-timeout for provider/internal phases.
52:1b87b9b9 feat(agent): supervision check-ins show each background job's command, age and silence, flag a long silent job as possibly stuck and escalate to kill-or-justify; job_output reports the last output time; opt-in --job-timeout default deadline for background commands
```

### `docs/checkpoints/2026-10-07-2137.md` (2 hits)

```
4:Since checkpoint 2026-10-07-0554 a long wave landed on `main` and was pushed (origin/main = 9ddfcbde; operator asked for push + alpha bumps): #1259 (heartbeat source labels, rate-limit visibility, stall-detach cleanup), #1260/#1261 opencode-init/omp-init installers (wording taken from the harnesses themselves, both reviewed their installed commands live), flake fixes #1262 (P0_338/P2_1, mechanism by @ox read-only), #1263 (ps helpers flush heartbeat), #1266 (abort keeps a Stop-cancelled row and drops the in-memory job), #1267 (queued compaction re-queued on foreign lock), #1268 (10 fragile agent tests), goroutine dumps no longer litter TEMP from tests, inline-window/stall test fixes, version bumps 0.2.0-alpha.10 (76be7bba) and 0.2.0-alpha.11 (80a449cd). #1264 closed as unreachable (fantasy always calls OnToolResult). Batches design written by @ox: docs/plans/2026-10-07-job-batches-design.md (81420be5; operator added an abstract internal/jobbatch engine + task_outcome) — awaiting operator answers to its 6 questions before v1.
10:Timers: babysit `7aa0f086` (7,37) and stall-check `dc40c8c9` (17,37,57), session-only. Clawforge probes use `timeout 45`. Disk D: was full once (29 MB) and recovered to ~618 GB free; the operator dropped the Rust artefact cleanup. Leftover empty worktree dirs (wps, wa59, wiw, w1264, w1262, wci, wci4, wp14, wp03…) are held by a shell handle and could not be deleted — harmless.
```

### `docs/checkpoints/2026-10-08-1449.md` (1 hits)

```
4:Since checkpoint 2026-10-07-2137 the following landed on `main` and was pushed (origin/main = d76970b0 minus the checkpoint-free local tail; everything up to f48f8830 + operator commits is pushed): green CI on alpha.11/alpha.12 (d92b3ffa settings-lock alias fix, c36b2640 procinfo access-denied test, 4d43156b two racy test harness fixes), #1270 await_tasks for the top-level agent (e656b924), #1271 await_tasks for delegated workers (79ce463a, in-tool blocking wait capped at 40m, excluded from stall-detach), alpha.12 bump (237d09f0), #1274 Anthropic HTTP provider drops blank Authorization/X-Api-Key headers (9800b19d, wire probe proved the blank header), and revision 2 of the job-batches plan (f48f8830) resolving 29 first-round XXS review findings with Appendix A.
```

### `docs/checkpoints/2026-10-11-2215.md` (1 hits)

```
32:- Прежние контракты не сохраняются: durable-модель переписана — цикл событий хоста-владельца единственный арбитр, SQLite = проекция «сначала в журнал» (`batch_nodes`, `batch_control_requests`), `async_jobs` удаляется без миграции; наружные эффекты после коммита, сдерживающие (Stop/Dispose) не ждут; отказ коммита фатален (процесс завершается, затем dead-host sweep, `host_lost`).
```

### `docs/checkpoints/2026-10-12-0734.md` (4 hits)

```
4:Цель сессии по указанию оператора (`/babygoal`): реализовать переход на батчи через /wrush, коммиты между задачами, сложные вопросы — в `rush run --role reviewer` (вместо @ox); где можно, работать параллельно через /wrush. Автономная часть (разделы 0-8 плана) готова и влита в main: контракт `internal/jobbatch` (A0a), схема лаборатории и 35 сценариев (A0b), чистый движок Core, Runner (Runtime) с OneItem, процессный исполнитель, лаборатория и команда `cmd/batchlab` (Lab), кросс-срезовый acceptance-тест, карта приёмки `docs/jobbatch-acceptance.md` (51 ID: 32 покрыты, 17 частично, 2 не покрыты — AW-a/AW-b отложены в Rush) и `docs/jobbatch.md`. Проверено: `-race` по `./internal/jobbatch/... ./cmd/batchlab/`, golangci-lint 0, `go build -p 1 ./...`, spawn-guard, file-size guard, реальный бинарник batchlab (L1/L2, replay, испорченный журнал → exit 2). Затем три read-only разведчика составили карты кода (`docs/research/2026-10-12-job-batches-rush-map-{a,b,c}.md`), из них собран `docs/plans/2026-10-12-job-batches-rush-integration.md` (15 пакетов R1-R15b, решения D1-D11, стратегия «инертно до переключения R12»); строгий ревьюер нашёл 4 P1 в плане, все исправлены. Фаза Rush-интеграции: влиты R1 (реестр листьев политик), R2 (retained-handle фоновой оболочки + починка гонки в jq_test), R3 (retained-сессия CLI-провайдера), R5 (инертное хранилище проекции batch_nodes/batch_control_requests, атомарная проекция по нескольким корням), каркас исполнителя Rush `internal/agent/batch_exec.go`, починка теста сессии P0-1. В работе четыре агента: R4 (граница коммита Runner — переписывается на конвейер «применять вперёд, коммитить позади» после трёх подряд P1 строгого ревьюера), R7x (R6 stall-исключение + R7a generic/MCP семейство), R7y (shell-семейство, режим листа bash), R7z (argv-семейство, чинит P1 с Dispose и P2 классификацию Stop). Оговорки: 17 коммитов не запушены (origin/main = c8969428), версию не менял; macOS/Linux ветка процессного исполнителя проверена только кросс-vet; нагрузочные падения `TestRecheckDebtOnRelease_*` под `-race` (бюджеты по времени) не воспроизведены на тихой машине — перепроверить в тихий период; heavy.sh сериализует всю машину, полный `-race` по `internal/agent` занимает >10 мин и падает по таймауту при очереди — гонять его без `-race` и только оркестратору.
12:- #1344 R6 Managed-call stall-detach exclusion — агент jb-r7x (вместе с R7a)
26:- #1357 R12 Cutover: единственный путь, удаление ledger/async_jobs/stall/job_kill/job_output, деструктивная миграция с live-host gate  (blockedBy: #1339-#1356)
35:- Новый путь инертен за `RUSH_BATCH_RUNTIME` до пакета R12, где старый путь удаляется целиком; никаких двух живых источников терминальной истины. Деструктивная миграция `async_jobs` без конвертации строк за проверкой живых хостов (OS host-lock probe).
```

### `docs/ci/2026-10-13-selector-replay.md` (2 hits)

```
26:| 3 | 1b94fad9 (awaitSync select race) | darwin, internal/agent | tier1 | 22/22/22 | included | none active | MISS | HIT | same as 2 | race_roots rule |
37:| 12 | 4988d832 (awaitSync move, 1004-line violation) | linux, internal/agent | tier1 | 22/22/22 | included | linux | HIT for the package, but the file-size ratchet guard was NOT in the always-run set (`.githooks/` missing from always_run, and always_run was never consulted by the selector) | HIT (guard carried) | — | always_run wiring + `.githooks/` added |
```

### `docs/design/event-loop-turn.md` (2 hits)

```
4:responsive `job_output` polling) are shipped and verified. This doc designs the
14:- **Phase 1b** — `job_output --wait` is bounded (`jobOutputMaxWait`, 90s):
```

### `docs/plans/2026-09-02-sdk-scoped-file-tools-plan.md` (5 hits)

```
343:   (`bash run_command job_output job_kill download git_read agentic_fetch
344:   list_mcp_resources read_mcp_resource`; `bash run_command job_output
345:   job_kill` are kept when `KeepsCommandTools()`), and every `fs_*` whose
411:**A folder-scoped call has no `bash`, `run_command`, `job_output` or
412:`job_kill` tool — unless the SAME call is `RestrictedRun` with at least one
```

### `docs/plans/2026-09-24-agent-wakes-and-async-job-control.md` (4 hits)

```
24:- `job_output` и `job_kill` работают с принадлежащими сессии фоновыми shell-job.
45:| Просмотр команды | Расширить `job_output` или добавить `async_job_read(job_id, cursor?)` | Возвращать статус и только новые байты вывода с ограничением размера ответа. |
46:| Остановка команды | Расширить `job_kill` или добавить `stop_command(job_id)` | Останавливать только задачу текущей сессии; повторный stop безопасен. |
180:   `job_output`/`job_kill` при сохранении совместимости.
```

### `docs/plans/2026-09-27-async-phase1-spec.md` (105 hits)

```
8:Область: **только фаза 1** — «Одна машина состояний; `asyncJobRegistry` и
99:`workLedger` не хранит карту драйверов сам — он принимает единственную
134:`work_ledger_driver.go` (только он) должен быть первым, самостоятельным
204:// asyncJobState.completion *AsyncCompletion, где поля SessionID/ToolCallID/
236:// asyncJob — одна запись реестра. Замещает asyncJobState
241:type asyncJob struct {
247:    cli        bool   // origin, дословно сегодняшний asyncJobState.cli — маршрут доставки от него не меняется в фазе 1 (см. §1.6)
259:Все поля `asyncJob`, кроме идентифицирующих (`owner`, `toolCallID`, `kind`,
261:`workLedger.mu` — как и сегодняшний `asyncJobState`, отдельного мьютекса на
267:// work_ledger.go
269:// sessionJobs — замещает asyncJobSession (async_job_registry.go:40-48)
270:// дословно, только jobs теперь map[string]*asyncJob.
272:    jobs    map[string]*asyncJob
278:// workLedger — единственный владелец состояния работы в памяти. Замещает
279:// asyncJobRegistry (async_job_registry.go:50-65) и хранилище
281:type workLedger struct {
283:    bySession map[string]*sessionJobs        // владелец -> его задачи (было asyncJobRegistry.sessions)
284:    byChild   map[string][]*asyncJob          // childSession -> задачи делегаций, ЗАПАРКОВАННЫЕ на нём (было subAgentOutcomeRegistry.byChild); все записи здесь по конструкции ещё не доставлены — доставленная запись удаляется отсюда в том же деле, что и из bySession (см. §1.4 gc)
323:// maxAsyncJobsPerSession; НЕ для дублирующегося ключа — это больше не
325:func (l *workLedger) Start(owner, toolCallID, toolName, childSession string, cli bool, timeout *TimeoutSpec, cancel context.CancelFunc) (job *asyncJob, existing bool, err error)
331:func (l *workLedger) acknowledged(sessionID, toolCallID string)
338:func (l *workLedger) abort(sessionID, toolCallID string)
342:func (l *workLedger) finish(owner, toolCallID string, result jobResult)
352:func (l *workLedger) armDelegation(owner, toolCallID string, captured jobResult)
360:func (l *workLedger) recheckChild(childSessionID string)
365:// asyncJobRegistry.running), НИ ОДНОГО активного фонового шелла
369:func (l *workLedger) childScopeDrained(childSessionID string) bool
374:// одним мьютексом. Замещает asyncJobRegistry.cancelSession
383:func (l *workLedger) cancelSession(sessionID string)
386:// и запрещает дальнейший Start. Замещает asyncJobRegistry.close
390:func (l *workLedger) close()
396:func (l *workLedger) fireTimeout(owner, toolCallID string, partial jobResult)
404:сигнатуры** с `*asyncJobRegistry`/`*subAgentOutcomeRegistry` на `*workLedger`
420:// одного и того же *asyncJob под одним и тем же мьютексом, так что "карта
422:func (j *asyncJob) transitionToTerminal(state jobPhase, result jobResult) bool {
447:func (l *workLedger) deliverLocked(owner string, j *asyncJob) (AsyncCompletion, bool /* callback */)
469:- возвращает `existing=true` и указатель на СУЩЕСТВУЮЩУЮ `*asyncJob` —
476:  `owner`/`toolCallID`, `maxAsyncJobsPerSession` (не проверено под другим
490:| `asyncJobRegistry` (тип, `async_job_registry.go:50`) | `workLedger` (`work_ledger.go`) | `coordinator.asyncJobs` — тип поля |
491:| `newAsyncJobRegistry` (`:67`) | `newWorkLedger` | `coordinator.go` (место постройки координатора — не проверено точное имя функции/строка постройки `c.asyncJobs`, нужно найти при реализации) |
492:| `asyncJobState` (тип, `:33-38`) | `asyncJob` (`work_job.go`) | внутренне |
493:| `asyncJobSession` (тип, `:40-48`) | `sessionJobs` (`work_ledger.go`) | внутренне |
494:| `.start(sessionID, toolCallID, cli, cancel) error` (`:90-109`) | `.Start(owner, toolCallID, toolName, childSession, cli, timeout, cancel) (*asyncJob, bool, error)` | `internal/agent/async_tool.go:56` (`asyncTool.Run`) — новые параметры, новая обработка `existing` |
503:| `.next`/`.pending`/`.running`/`.anyRunning`/`.cancelSession`/`.close` (`:257-353`) | без изменений имени/сигнатуры, тело читает новые поля | `coordinator_background.go` (`NextAsyncCompletion`, `HasPendingAsyncJobs`), `subagent_outcome.go`-преемник (`coordinator_work_scope.go`, см. ниже) |
504:| `subAgentOutcomeRegistry` (тип, `subagent_outcome.go:82-89`) | **удалено** — состояние (`byChild`) переехало в `workLedger` | — |
505:| `.park` (`:101-116`) | `workLedger.armDelegation` (§1.4) | `internal/agent/async_tool.go:150-162` (`asyncTool.finalize`) |
506:| `.tryRelease`/`.nextReleasable` (`:121-148`) | `workLedger.recheckChild` (§1.4) | все триггеры ниже |
509:| `coordinator.refreshSubAgentCompletion` (`:238-262`) | без изменений, остаётся МЕТОДОМ КООРДИНАТОРА (нужен `c.messages`) | вызывается из `workLedger.recheckChild` через `l.coord.refreshSubAgentCompletion(...)` |
512:| `coordinator.sessionOwnsPendingWork` (`:356-370`) | тело меняется минимально: `c.subAgentOutcomes.hasParkedFor(id)` → `c.asyncJobs.hasParkedFor(id)` | то же |
513:| `.noteChildRunEnded` (`:375-380`) | `workLedger.recheckChild` (то же имя роли, новый метод) | `coordinator_run.go:170`, `coordinator_background.go:61,134,153` |
516:| `.releaseCanceledForParent`/`.releaseCanceledForChild`/`.releaseAllCanceled`/`.releaseCanceled` (`:437-512`) | слиты в `workLedger.cancelSession`/`.close` (§1.4) — больше не нужны как отдельные функции | `coordinator_interrupt.go:47-48,59` |
518:| `.close` (registry, `:562-578`) | слито в `workLedger.close` | `coordinator_interrupt.go:59-62` (`CancelAll`) |
519:| `.startTickerLocked`/`.tick` (`:582-638`) | переносятся почти без изменений в `work_ledger_delegation.go`, зовут `recheckChild` вместо `tryRelease` — **сохраняются в фазе 1** (см. §5 почему не удаляются) | внутренний |
520:| `.gcLocked` (`:642-655`) | без изменений роли, оперирует `[]*asyncJob` вместо `[]*subAgentOutcomeEntry` | внутренний |
521:| `coordinator.subAgentWorkTerminal` (`:674-688`) | `workLedger.childScopeDrained` (§1.4) — с ИСПРАВЛЕННЫМ через `sessionBusy`/`driverFor` busy-гейтом ТОЛЬКО если #1049 уже дал драйвер, иначе побайтово тот же (всегда-`false`) дефект | вызывается из `recheckChild` |
522:| `.parkSubAgentOutcome` (`:693-705`) | `workLedger.armDelegation` (см. выше) | `async_tool.go` |
523:| `.noteSubAgentChildRunEnded` (`:709-714`) | тонкая обёртка `c.asyncJobs.recheckChild(sessionID)` — имя МОЖЕТ остаться для минимального диффа в трёх вызывающих | `coordinator_run.go:170`, `coordinator_background.go:61,134,153` |
524:| `.releaseSubAgentOutcomesForParentCancel`/`.releaseSubAgentOutcomesForChildCancel`/`.releaseAllSubAgentOutcomesCanceled` (`:716-740`) | **удалено** — заменено прямыми вызовами `c.asyncJobs.cancelSession`/`.close` | `coordinator_interrupt.go:41-64` (`Cancel`, `CancelAll`) — тела УПРОЩАЮТСЯ (см. §3, шаг 3) |
539:**Шаг 0 — `work_ledger_driver.go` (§0.2) отдельным коммитом.**
540:`subAgentDriver`, `driverFor` (поле у `workLedger`, изначально нигде не
542:методы `*coordinator`, НЕ подключённые ни к чему (пока `c.asyncJobs` —
549:`work_ledger.go` (§1.2, §1.4 частично: `Start`/`acknowledged`/`abort`/
552:компилируемый тип, БЕЗ переключения `coordinator.asyncJobs` на него. Тесты:
554:`work_ledger_test.go` (§4) пишется и проходит ПРОТИВ нового типа в изоляции.
556:**Шаг 2 — делегационная логика, без подключения.** `work_ledger_delegation.go`
560:`l.coord.sessionBusy` — значит `workLedger.coord *coordinator` заполняется
561:здесь. Тесты: `work_ledger_delegation_test.go` (§4, портированные сценарии
572:  `c.Run`; `ClaimAsyncCompletions`/`NextAsyncCompletion`/`HasPendingAsyncJobs`
573:  — тип `c.asyncJobs` меняется, тела не меняются (методы переносятся с теми
577:  `asyncJobs.cancelSession`) схлопываются в один
578:  `c.asyncJobs.cancelSession(sessionID)`; `CancelAll` — два вызова
579:  (`releaseAllSubAgentOutcomesCanceled`, `asyncJobs.close`) схлопываются в
580:  один `c.asyncJobs.close()`.
586:- Постройка координатора: `c.asyncJobs = newWorkLedger(...)`,
587:  `c.asyncJobs.coord = c` (или через параметр конструктора — не проверено,
589:  найти симметричное место для `asyncJobs.coord`), `c.asyncJobs.driverFor =
612:    чёрный ящик и не знают о `asyncJobRegistry`/`subAgentOutcomeRegistry`.
616:использования продакшн-типа) `work_ledger_test.go`,
617:`work_ledger_delegation_test.go`; `async_tool_test.go` правится МИНИМАЛЬНО
618:(`newAsyncJobRegistry` → `newWorkLedger`, тип поля `coordinator.asyncJobs`).
643:| `TestAsyncJobRegistryWaitsForPersistedToolResult` | ack-гейт: доставка ждёт `acknowledged`, FIFO готовой очереди | `work_ledger_test.go`, тот же сценарий против `workLedger` |
644:| `TestAsyncJobRegistryWebCallbackExactlyOnce` | двойной `finish` → колбэк вызван ровно один раз | то же |
645:| `TestAsyncJobRegistryConcurrentFinishAndAcknowledge` | гонка `finish`×`acknowledged` не теряет и не дублирует | то же |
646:| `TestAsyncJobRegistryCancelAndClose` | отмена контекста executor'а, `close` отклоняет новый `Start` | то же |
647:| `TestAsyncAgentTool_NoFinishedNoticeWhileChildOwnedJobsPending` | нет уведомления родителю, пока ребёнок владеет незавершённой работой (свойство 1) | `work_ledger_delegation_test.go`, через `armDelegation`+`recheckChild` вместо `park`+`tryRelease` |
658:переписываются по сценарию, только по имени конструктора (`newAsyncJobRegistry`
659:→ `newWorkLedger`), см. §3 шаг 4.
667:`TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome`
668:(`work_ledger_test.go`): один `Start`, затем одновременно (горутины)
681:`TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce` — одновременно
691:**(c) Ack-гейт.** `TestAsyncJobRegistryWaitsForPersistedToolResult`
693:`TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce` — `Start` делегации,
704:`TestWorkLedger_StartIsIdempotentPerToolCallID` — два `Start` с одним
731:  — **сохраняется**, переносится в `work_ledger_delegation.go` без удаления.
771:  `asyncJobRegistry`+`subAgentOutcomeRegistry` в `workLedger` устраняет
773:  читает `c.asyncJobs.running(id)` и `c.subAgentOutcomes.hasParkedFor(id)`
789:- **Общий мьютекс для команд и делегаций.** Сегодня `asyncJobRegistry.mu` и
791:  При `maxAsyncJobsPerSession = 50` и типичном числе сессий вклад в
793:  мержем стоит короткий бенчмарк (`go test -bench` на `workLedger` с 50
826:| `work_job.go` | `jobKind`, `jobPhase`, `jobResult`, `timeoutKind`, `TimeoutSpec`, `asyncJob`, `transitionToTerminal` | ~150 | часть `asyncJobState`, часть `subAgentOutcomeEntry` |
827:| `work_ledger.go` | `sessionJobs`, `workLedger`, `Start`/`acknowledged`/`abort`/`finish`/`deliverLocked`/`next`/`pending`/`running`/`anyRunning`/`markDrained`/`close` | ~380 | `async_job_registry.go` (353, удаляется) |
828:| `work_ledger_delegation.go` | `byChild`, `armDelegation`, `recheckChild`, `childScopeDrained`, `cancelSession`, `fireTimeout`, тикер, `gcLocked`, `hasParked`/`hasParkedFor`/`parkedParentSessions` | ~340 | делегационная половина `subagent_outcome.go` |
829:| `work_ledger_driver.go` | `subAgentDriver`, `driverFor`, `wakeOwner`, `sessionBusy` | ~80 | новое — точка стыковки с #1049 |
852:   точное имя/сигнатуру интерфейса ДО написания `work_ledger_driver.go`,
858:   `timeoutKind` в `asyncJob` УЖЕ в фазе 1 (без вызывающего в проде, кроме
```

### `docs/plans/2026-09-27-async-phase2-spec.md` (90 hits)

```
6:`internal/agent/work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
19:оркестратором отдельно: задача #1053 (`job_output`/`job_kill` разрешают
20:`job_id` в `shell_id` через новое поле `asyncJob.shellID`, устанавливаемое в
22:спецификация её не переопределяет и не трогает `job_kill.go`/`job_output.go`;
26:`job_id`/`shell_id`, различимый `cancelled` для `job_kill`) в остальном не
85:  async-задаче; она не строит `AsyncCompletion` и не читает `workLedger`. Не
181:`work_ledger.go:111-134`; точная форма аргумента и причина, по которой это
182:НЕ `*asyncJob`, — §2.4, читается вместе с этим разделом) и хранит в
183:`workLedger.noticedBefore` (§2.4) запись `"owner\x00toolCallID" ->
191:`workLedger.Start` (фаза 1, «повторный старт того же вызова возвращает
235:Не заводится отдельное поле `wakeFailed` на `asyncJob`: к моменту, когда
238:в `workLedger` было бы задачей без потребителя (задание не просит `sessions
260:Никакого нового поля на `asyncJob` не заводится: `wakeSession` вызывается
263:`workLedger` (`noticedBefore`, введена в §2.4 вместе с точным обоснованием
284:// job is a flat identity, not *asyncJob: by the time any caller can reach
286:// bySession[owner].jobs (work_ledger.go:179, unchanged) -- see §2.4 for why
376:| `coordinator_background.go:47-95` `notifyAsyncCompletion`, строит `notify`, выбирает драйвер, зовёт `go runAutoResumeRecovered(ctx, ..., notify)` | тело сокращается до: собрать `AsyncCompletion` текст (`FormatAsyncCompletion`) и `noticeKind` (§5.5), построить `jobIdentity{completion.SessionID, completion.ToolCallID}` (см. §2.4 — почему не `*asyncJob`), `go func() { c.wakeSession(ctx, id, text, noticeKind, true) }()` |
379:### 2.4 Открытый технический вопрос, закрытый здесь: `AsyncCompletion` vs `*asyncJob`
382:(из `deliverLocked`'s возврата), а НЕ указатель на `*asyncJob` (запись уже
384:`delete(s.jobs, job.toolCallID)` ДО возврата, `work_ledger.go:179`). Поэтому
385:`wakeSession` (§1.6) принимает не `*asyncJob`, а плоскую `jobIdentity{owner,
388:на мьютекс-защищённой карте НА САМОМ `workLedger`
393:// work_ledger.go (правка): добавить рядом с bySession
398:`bySession`, из `wakeSession` (которая держит ссылку на `l *workLedger` через
399:`c.asyncJobs`). Обоснование неограниченного роста — то же, что фаза 1 уже
415:    if c.asyncJobs != nil {
416:        c.asyncJobs.cancelSession(sessionID)
448:    if c.asyncJobs != nil {
449:        c.asyncJobs.cancelSession(sessionID)
775:    job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
875:    if t.coordinator == nil || t.coordinator.asyncJobs == nil {
880:        t.coordinator.asyncJobs.armDelegation(completion.SessionID, completion.ToolCallID, result)
883:    t.coordinator.asyncJobs.finish(completion.SessionID, completion.ToolCallID, result)
892:// work_job.go, правка asyncJob — добавлено поле
893:type asyncJob struct {
896:                        // consumer is the goroutine blocked in awaitSync
901:                        // for awaitSync to read.
906:// work_ledger.go, правка Start — добавлены sync, timeout параметры (см. §5
911:func (l *workLedger) Start(owner, toolCallID, input, toolName, childSession string, cli, sync bool, timeout *TimeoutSpec, cancel context.CancelFunc) (*asyncJob, bool, error) {
913:    job := &asyncJob{
934:// work_ledger.go, правка deliverLocked — sync short-circuit ПЕРЕД
936:func (l *workLedger) deliverLocked(owner string, job *asyncJob) (AsyncCompletion, bool) {
974:(`work_ledger.go:172-189`), с ОДНОЙ добавленной строкой — `TimedOut:
978:// work_ledger.go, новый метод
979:// awaitSync blocks until job's outcome is ready (deliverLocked closed
983:func (l *workLedger) awaitSync(ctx context.Context, job *asyncJob) (jobResult, error) {
1003:func (t *asyncTool) awaitAndFinish(ctx context.Context, job *asyncJob) (fantasy.ToolResponse, error) {
1004:    result, err := t.coordinator.asyncJobs.awaitSync(ctx, job)
1021:| `OriginSDK` | синхронно, `t.inner.Run(ctx, call)` напрямую | через реестр: `Start(sync=true)` → горутина `t.run` (bash: без принудительного фона, §4.3) → `awaitAndFinish` блокирует до `job.done` | контент/ошибка ToolResponse — байт-в-байт та же (§4.3); タайминг — раньше блокировка была внутри `t.inner.Run`, теперь блокировка в `awaitSync`, разница непринципиальна (то же дерево вызовов, тот же контекст) |
1040:  проверить что во время выполнения `t.coordinator.asyncJobs.running(sessionID)`
1107:type asyncJob struct {
1291:// work_ledger_timeout.go (новый файл)
1293:// timeoutService is the single per-process timer for every asyncJob with a
1302:    ledger *workLedger
1304:    heap   jobDeadlineHeap // container/heap over []*asyncJob, Less by deadline
1309:func newTimeoutService(l *workLedger) *timeoutService {
1322:func (s *timeoutService) arm(job *asyncJob) {
1371:        job := heap.Pop(&s.heap).(*asyncJob)
1380:`workLedger.close()` (`work_ledger.go:329-346`) добавляет `l.timeouts.close()`
1387:// work_ledger_delegation.go либо новый work_ledger_timeout.go — не
1396:func (l *workLedger) handleTimeout(job *asyncJob) {
1432:            text := fmt.Sprintf("Timeout reached for async job %s (%s) — it is still running (elapsed %s). Latest output:\n\n%s\n\nThis was a one-time check-in; it will not repeat automatically. Decide whether to keep waiting, check again later, or stop it with job_kill (bash/run_command) or stop_agent (agent).",
1452:`jobPhase`, только `IsError bool`, `work_ledger.go:23-32`), а НОВОЕ булево
1458:// work_ledger.go, правка AsyncCompletion (§4.4's структура, уже
1513:func (l *workLedger) capturePartialLocked(job *asyncJob) jobResult { ... }
1527:running past its deadline; use job_output to inspect progress» — переиспользуя
1528:уже существующий инструмент просмотра, `job_output`, упомянутый в плане
1554:- `TestWorkLedger_TerminateAndWakeTransitionsToTimedOut` — job с
1568:- `TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome` — одновременно
1571:  продолжение ASYNC-03's теста фазы 1, `TestWorkLedger_
1579:- `TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning` — `timeoutWakeOnly`,
1721:  через `asyncTool`/`workLedger` МЕНЯЕТСЯ п. 4 (единый путь), это отдельно
1748:| ASYNC-04 | ack-гейт (`announced`) теперь ТАКЖЕ верен для `sync`-задач (устанавливается в `Start`, не только в `onToolResult`) — переписать формулировку «ack-gate» с явной оговоркой про `sync` (§4.4). Ссылка на `deliverLocked` остаётся, но добавляется ссылка на `awaitSync`/§4.4. |
1766:`timeoutSeconds`/`timeoutNotified`/`sync`/`done` на `asyncJob`; правка
1767:`work_ledger.go` — добавить `noticedBefore map[string]string` на
1768:`workLedger` (§2.4) — БЕЗ изменения сигнатуры `Start`/`deliverLocked` (новые
1772:**Шаг 1 — `workLedger.Start`/`deliverLocked` новая сигнатура, без
1774:(§4.4), добавить `awaitSync` (§4.4), `noticedBefore` (§2.4). Обновить
1779:`work_ledger_test.go`'s текущий набор + новый `TestWorkLedger_
1822:`work_ledger_timeout.go`, правка `work_job.go` (подключить `deadline`/
1824:заполняются), `work_ledger.go`'s `Start`/`deliverLocked` (арминг таймера,
1855:| `work_job.go` | + `phaseTimedOut`, `timeoutKind`, `TimeoutSpec`, новые поля `asyncJob` | 86 → ~130 | правка |
1856:| `work_ledger.go` | + `sync`/`timeout` параметры `Start`, `awaitSync`, `noticedBefore`, sync-ветка `deliverLocked` | 346 → ~410 | правка |
1857:| `work_ledger_delegation.go` | без структурных изменений (таймаут не меняет делегационную логику напрямую) | 362 | без изменений |
1858:| `work_ledger_timeout.go` | `timeoutService`, `handleTimeout`, `capturePartialLocked`, min-heap | ~180 | новый |
1883:  `work_ledger_timeout_test.go` до мержа.
1895:- **Координация с параллельной задачей #1053.** `asyncJob.shellID` (её
1898:  тип `asyncJob` в ОДНОМ И ТОМ ЖЕ файле (`work_job.go`) в двух параллельных
1949:3. **Порядок слияния**: сначала #1053 (`job_id` в job_kill/job_output,
1950:   `asyncJob.shellID`), затем фаза 2 на его основе.
```

### `docs/plans/2026-09-27-async-structured-concurrency.md` (5 hits)

```
46:| 2 | **Маршрут выбирается по origin, а не по сессии.** Флаг `cli` в `asyncJobState` решает: ready-очередь (pull; разбирает только цикл корня) или callback (push). | `async_job_registry.go` `releaseLocked` | #1029 |
48:| 4 | **Жизненный цикл одной задачи разорван между реестрами.** Строка `asyncJobRegistry` (start/acknowledged/completion), запись паркинга (park/claim/emit), shell в `BackgroundShellManager`, занятость mailbox ребёнка. Строку можно удалить в одном месте и воскресить в другом (`finishParked`). | `async_job_registry.go`, `subagent_outcome.go` | BL-2 (#1032), двойная или потерянная доставка при гонке finish/cancel/timeout |
89:Замещает `asyncJobRegistry`, хранилище `subAgentOutcomeRegistry` и учёт
234:триггера, тикер, BL-1, BL-2); двойной путь `asyncJobRegistry` (ready-очередь
265:**Фаза 1 — реестр работы в памяти.** Одна машина состояний; `asyncJobRegistry`
```

### `docs/plans/2026-09-27-wake-tools-contract.md` (66 hits)

```
26:  строка ~43); каждая обёрнутая задача получает запись в `workLedger`
27:  (`internal/agent/work_ledger.go`) с ключом `(owner=sessionID, toolCallID)`.
28:- `workLedger` (`work_ledger.go` + `work_ledger_delegation.go` +
29:  `work_job.go`) — единственный реестр в памяти. `asyncJob` сегодня НЕ имеет
33:  Значит контракт этого RFC описывает поля, которых в `asyncJob` ещё нет —
55:- `job_output`/`job_kill` (`internal/agent/tools/job_output.go`,
56:  `job_kill.go`) сегодня адресуют `shell_id` — идентификатор
63:  (`call.ID`, из `asyncTool.startedResponse`). Значит **`job_output`/`job_kill`
100:это стиль, которым уже написаны `job_output`/`job_kill`/`bash`/`run_command`
116:(`tools.GetSessionFromContext`, тот же способ, каким `job_output`/`job_kill`
165:`workLedger.Start`'а идентично (см. §7 про то, что `wakein`/`wakeon`/`loop`
325:(`wake_cancel`, `job_kill`, `stop_agent`) правило идемпотентности:** повторный
335:## 2. Контроль команд: `job_output`/`job_kill` — расширение, не новые инструменты
337:**[Решение RFC] Расширить существующие `job_output`/`job_kill`, не вводить
339:`bash.md.tpl`/`run_command.md.tpl` («Use job_output tool to view output or
340:job_kill to terminate») — второй набор имён удвоил бы то, что модель уже
342:это как основной вариант («Расширить `job_output` или добавить
353:задача уже мертва и `job_output`/`job_kill` по нему бессмысленны. Итог:
354:`job_output`/`job_kill`, как написаны сегодня, не могут быть вызваны моделью
366:// job_output input
393:(новое поле `asyncJob.shellID`, заводится той фазой, что реализует этот
394:раздел — см. §7). `job_output`/`job_kill`, получив `job_id`, разрешают его
404:`context.WithTimeout`, НЕ background-shell) — `job_output` для такого
412:явно документировать, что `job_output` для `run_command`-задач возвращает
421:`t.run` подключает его к `workLedger.setRunCommandBuffer` для CLI/web-задач).
422:`job_output` читает его через `workLedger.RunCommandOutput`, `job_kill` —
428:### 2.2 `job_kill` — различить «остановлено» от «упало само»
430:Подтверждено чтением (§0): сегодня `job_kill` работает напрямую с
431:`BackgroundShellManager`, реестр (`workLedger`) о вызове ничего не знает.
432:Когда `job_kill` останавливает CLI/Web-задачу, обёрнутую `asyncTool`, её
437:**Контракт:** `job_kill(job_id)` перед вызовом `BackgroundShellManager.KillOwned`
448:`job_kill` для `run_command`-задачи вызывает `workLedger.StopRunCommandJob`
470:- Отсутствует/не найден/чужой → та же форма ошибки, что у `job_output`.
473:- Успех (немедленный ответ на САМ вызов `job_kill`, отдельно от последующего
478:`job_kill` отказывает: `"job call_abc123 is a sub-agent delegation, not a command — use stop_agent instead"`
487:job_kill/job_output; its result will arrive as a session message when the
561:  которое §2.2 вводит для `job_kill` против «упало само».
570:`stop_agent` (§4.4), НЕ через `workLedger.close()`/`CancelAll` (которые
623:  задачи-делегации в реестре (`asyncJob.state`, см. §5) для этого
687:1. **Сторона реестра** — `workLedger.cancelSession(child_session_id)`,
695:   (`work_ledger_delegation_test.go`), просто раньше не применявшееся с этим
716:    if c.asyncJobs != nil {
717:        c.asyncJobs.cancelSession(sessionID)
749:    if c.asyncJobs != nil {
750:        c.asyncJobs.cancelSession(sessionID)
779:| `cancelled` (`phaseCancelled`) | да | `cancelSession` (сессия целиком), **новое:** `job_kill`/`stop_agent` (точечно, §2.2/§4.3) | `cancelSession` — да; точечная отмена ОДНОЙ задачи без отмены всей сессии — нет, заводится §2.2/§4.3-реализующей фазой |
802:| Ручная остановка (`job_kill`) | `Async job {job_id} ({tool}) was stopped (job_kill). Partial output before the stop:\n\n{partial_output}` |
805:| Таймаут, `wake_only` (НЕтерминально) | `Timeout reached for async job {job_id} ({tool}) — it is still running (elapsed {elapsed}). Latest output:\n\n{partial_output}\n\nThis was a one-time check-in; it will not repeat automatically. Decide whether to keep waiting, check again later, or stop it with job_kill (bash/run_command) or stop_agent (agent).` |
809:| Надзор (супервижн-тик, design doc §7) | **Реализовано (2026-09-28, задача #1043) с ИЗМЕНЁННЫМ хвостом** относительно черновика этой строки: `Supervision check-in (tick {n}, interval {interval}m): {k} background job(s) running, {j} sub-agent delegation(s) in progress.\n\n{по одной строке на каждого потомка: id, elapsed, последняя строка вывода/активности}\n\nKeep waiting, inspect with job_output, or stop with job_kill (commands only; sub-agent control tools are not available yet).` — явное указание `job_output`/`job_kill` вместо «no action needed» (задание #1043 прямо этого требует) и явный отказ называть ещё не существующие `inspect_agent`/`stop_agent`, тем же приёмом, что уже применяет `stopGuidanceFor` (§3, `work_ledger_timeout.go`) для `timeout_wake_only`. После N-го (по умолчанию 6) тика без прогресса тот же шаблон получает хвост «Supervision is now paused after N consecutive check-ins with no progress; it resumes automatically once something changes…» вместо обычной подсказки. |
851:| `max_active_schedules_per_session` | 20 | Новое, без прецедента в коде. На порядок меньше `maxAsyncJobsPerSession` (50, `work_ledger.go:20`) и `MaxBackgroundJobs` (50, `internal/shell/background.go:43`) — расписания видны оператору дольше и должны оставаться обозримым списком в `wake_list`, а не большим количеством фоновых команд. |
913:Стиль — как в `bash.md.tpl`/`job_output.md`: коротко, императивно,
927:**`job_output` (дополнение к текущему тексту)**: `Prefer job_id (the id returned when the job started) over shell_id. If you have a cursor from a previous call, pass it to get only new output since then.`
929:**`job_kill` (дополнение)**: `Accepts job_id (preferred) or shell_id. Stopping a job produces a distinct "stopped" notice, not a generic failure — the owner can tell you cancelled it on purpose.`
943:| §2 (job_id/`job_output`/`job_kill` унификация, различимый `cancelled`) | Фаза 2 (единая доставка) — новые поля `asyncJob.shellID`/причина остановки естественно ложатся туда же, куда `deadline`/`timeoutKind` | Этап 2 «Единый контроль async-командами» | **Да** (2026-09-28, стадия 2 плана, задача #1023) — `job_kill` помечает запись `stopRequested` до убийства, `finish` даёт различимый `phaseCancelled`/`AsyncCompletion.Stopped`, текст «was stopped (job_kill)» без двойной обёртки; `job_output` несёт `cursor`/`next_cursor`; `run_command`-задачи управляемы (tree-kill через `cmd.Cancel`, `job_output` читает `LiveOutputBuffer` на бегу); отказ для делегации — теперь `tools.DelegationJobError`, текст адаптирован под то, что `stop_agent`/`inspect_agent` ещё не существуют (см. §4's примечание к этой фазе) |
945:| §4.1–4.3 (`inspect_agent`/`inject_agent`/`stop_agent`) | Использует существующий `subAgentDriverRegistry` (уже фаза, слитая до фазы 1) | Этап 3 «Контроль под-агентов» | **Да** (2026-09-30, этап 3, задача #1024) — `tools/inspect_agent.go`/`inject_agent.go`/`stop_agent.go` + `coordinator_agent_control.go` (интерфейс `tools.AgentControl`, реализует coordinator); маршрутизация через `agentFor`; статус — driver-aware busy, литерал `awaitingAnswerStoppedTitle`, терминальное состояние реестра (после доставки/gc — по истории ребёнка); ответ/идемпотентность `stop_agent` — §4.3. Расхождение: событие родителю при `stop_agent` идёт существующим путём с текстом `subAgentOutcomeCancelledText`, а не строкой §5.1 «was stopped (stop_agent)» — специализированный текст потребовал бы правки `work_ledger_transition.go`, находящейся вне зоны этапа 3 |
948:| §7 (надзор) | Фаза 5, «первый потребитель сервиса сроков», можно сразу после фазы 3 | Не отдельный этап плана — часть design doc, этап 5 плана («CLI/web интеграция») зависит от его текстов/маркировки | **Да** (2026-09-28, задача #1043) — `internal/agent/supervision.go`: тик через `coordinator.wakeSession` (`NoticeKind="supervision"`), собственный сервис сроков переиспользован (`timeoutService.armFunc`, обобщённый heap вместо только-`*asyncJob`), не `workLedger`-задача (никогда не в `bySession[x].jobs`, не удерживает область — п.3 решений оператора); рост интервала 5→10→20→…→60 мин и пауза после 6 тиков без прогресса реализованы; конфиг `supervision_enabled`/`supervision_interval_minutes` + `rush run --no-supervision`/`--supervision-interval`; устаревшие сводки надзора исключены из истории провайдера (`agent_prompt.go`'s `dropSupersededSupervisionNotices`). `wakein`/`wakeon`/`loop`/`wake_list`/`wake_cancel` (§1) и `inspect_agent`/`inject_agent`/`stop_agent` (§4) остаются вне объёма — текст надзора явно избегает называть их (см. §5.1's пересмотренный текст ниже). |
950:| §5.1/§5.2 веб-рендер событий + §1.4–1.5 просмотр/отмена расписаний (web, #1107) | — | Этап 5 «CLI/web интеграция» | **Да (2026-10-01, этап 5b)** — веб-рендер по значению `NoticeKind`: `BackgroundJobNotice.tsx` показывает иконку/подпись по kind (`wake_fired` → «⏰ wake», `supervision`, `timeout_wake_only`, `timeout_terminated`, `wake_failed`), всё — системное событие слева, не реплика человека; итог async-команды у блока вызова (`ActionRow`) различает `timed out` / `stopped by user` (job_kill) / `cancelled` по текстам §5.1 (`asyncJobCompletion.ts`), timeout/error никогда не рендерятся как реплика пользователя; вкладка «Schedules» панели живой работы: снимок расписаний сессии (`get_session_wake_schedules` / `session_wake_schedules`, тот же коалесцирующий воркер, что у live-work, без опроса БД, только строки запрошенной сессии), кнопка Cancel с подтверждением через `confirm-dialog` → `cancel_wake_schedule` → `CancelSchedule` (чужой/неизвестный id — явная «not found»-ошибка, повторная отмена идемпотентна) + `WakeSchedulerNotify` через optional-interface assertion. `handlers_wake_schedules.go`, `store_livework.ts`, `WakeScheduleList.tsx` |
994:**`job_output`/`job_kill` (§2):**
996:  сразу после «started»-ответа `job_output(job_id=<из started>)` УСПЕШНО
1002:- `job_kill(job_id)` на бегущей задаче → владелец получает уведомление вида
1003:  «was stopped (job_kill)», НЕ «failed»/generic error — прямая проверка
1005:- Повторный `job_kill` на ту же (уже остановленную/завершённую) задачу —
1007:- `job_output`/`job_kill` с `job_id` делегации (`agent`/`agentic_fetch`) —
1011:  первый тест (job_output сразу после started) обязан упасть с «still
1042:  `child_session_id` остаётся валидным для `job_output`/`inspect_agent`
1089:5. Найденные дефекты вынесены в задачи: `job_kill`/`job_output` не видят
```

### `docs/plans/2026-09-28-async-phase3-spec.md` (70 hits)

```
6:work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
7:`work_ledger_timeout.go`, `coordinator_work_scope.go`, `coordinator_wake.go`,
60:реализующему не пришлось заново читать весь `work_ledger.go`, чтобы найти,
80:системы, и одна из двух (счётчик задач в `workLedger.bySession[x].jobs`) УЖЕ
88:`workLedger.Start` (`work_ledger.go:166-202`) кладёт `*asyncJob` в
92:`armDelegation` (`work_ledger_delegation.go:45-64`) только присваивает
94:остаётся в `s.jobs[toolCallID]` до `deliverLocked` (`work_ledger.go:275-329`),
97:из `recheckChild` (`work_ledger_delegation.go:82-120`), и только когда
115:`workLedger.next()` (`work_ledger.go:481-508`) — единственный
122:`work_ledger.go:326`) до следующего изменения.
162:ТОЛЬКО `BackgroundShellManager`, полностью вне `workLedger`. Обнаружение
198:`work_ledger_delegation.go`'s собственный комментарий у
221:  `work_ledger.go:322`) для ЛЮБОЙ сессии, кроме корня, которую
247:- `abort` (`work_ledger.go:361-375`) роняет ещё НЕ армированную задачу
253:- `cancelSession` (`work_ledger_delegation.go:174-227`) обрабатывает ОБЕ
262:- `handleTimeout`'s `timeoutTerminateAndWake`-ветка (`work_ledger_timeout.go:
279:(`app_run_async.go`) и предохранительный тикер (`work_ledger_delegation.go`)
314:    len(workLedger.bySession[S].jobs) > 0        // §1.1: включает и
330:`work_ledger_delegation.go:151-172`) для ОДНОГО хопа «родитель ждёт
340:Точка правки: `workLedger.running`/`l.anyRunning`-подобные подсчёты
341:(`work_ledger.go:517-542`) и любое новое место, суммирующее
344:`jobKind`, а на уровне поля самой `asyncJob`, которое фаза 5 заведёт.
352:1. **Для CLI-хоста (корень).** `workLedger.next()`
353:   (`work_ledger.go:481-508`) возвращает `(_, false, nil)`, когда
361:   (`work_ledger_delegation.go:82-120`)'s цикл выходит через
371:`handleTimeout`'s `timeoutTerminateAndWake`-ветка (`work_ledger_timeout.go:
476:- **`TestWorkLedger_ChildScopeClosesWithoutTicker`**
477:  (`work_ledger_delegation_test.go`) — с УЖЕ удалённым тикером (эта фаза
479:  `TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain` (арминг → ребёнок
503:работающего тикера. Верифицировано на HEAD: `work_ledger_delegation.go`'s
529:| `subAgentOutcomeTickInterval` (`work_ledger_delegation.go:28`) | **Удалено** |
533:| `workLedger.tickStop` (поле, `work_ledger.go:100`) | **Удалено** |
534:| `l.startTickerLocked()`-вызов в `armDelegation` (`work_ledger_delegation.go:59`) | **Удалено** (арминг продолжает синхронно звать `recheckChild`, триггер (i), без изменений) |
535:| `stop := l.tickStop; ...; close(stop)` в `workLedger.close` (`work_ledger.go:554-559`) | **Удалено** (нечего останавливать) |
537:Файл `work_ledger_delegation.go` уменьшается с 362 до ≈300 строк;
538:`work_ledger.go` — с 564 до ≈555 (минус поле и 6 строк в `close`).
544:`TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending`,
545:`TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain`,
546:`TestWorkLedger_FailedChildJobDeliveredOnceAsFailure`,
547:`TestWorkLedger_ConcurrentRecheckDeliversOnce`,
548:`TestWorkLedger_CancelReleasesArmedDelegation`,
549:`TestWorkLedger_CancelSurvivesFinishedChildTurn`,
550:`TestWorkLedger_ResumeAfterNoticeDoesNotReemit`,
551:`TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds`,
552:`TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce`
553:(все — `work_ledger_delegation_test.go`; ни один не манипулирует
560:- **`TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation`** —
680:// work_ledger_delegation.go, правка recheckChild — хвост цикла
681:func (l *workLedger) recheckChild(childSessionID string) {
809:`workLedger`/`subAgentDriverRegistry` — состояние ОДНОГО процесса; вторая
826:трогается, продолжает читать `workLedger.byChild` напрямую, не зависит
850:  фиксирует, что `workLedger.running`/счётчик §2.2 — единственное место,
867:  сервис сроков при завершении процесса (уже делает — `workLedger.close`
868:  зовёт `l.timeouts.close()`, `work_ledger.go:560`, актуально и для
882:`TestWorkLedger_ChildScopeClosesWithoutTicker` (§3.3, второй) К
885:что-либо удалять. Если `TestWorkLedger_ChildScopeClosesWithoutTicker`
895:**Шаг 2 — удаление тикера (§4).** `work_ledger_delegation.go`/
896:`work_ledger.go` (§4.3). Тесты: список §4.4 (существующие + новая
897:`TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation`) зелёные;
903:`releaseDriverIfScopeClosed`), `work_ledger_delegation.go`'s
930:| `internal/agent/work_ledger_delegation.go` | Удаление тикера (§4.3); хвост `recheckChild` (§6.2) | 362 → ≈300 |
931:| `internal/agent/work_ledger.go` | Удаление поля `tickStop` и его остановки (§4.3) | 564 → ≈555 |
934:| `internal/agent/work_ledger_delegation_test.go` | Новые тесты §3.3, §4.4 | — |
1012:| ASYNC-02 | Формулировка закона не меняется; «Где сегодня» переписывается ЦЕЛИКОМ: `internal/app/app_run_async.go`'s цикл дренирования БЕЗ `descendantWorkPending`/BFS (§3.2); `workLedger.next()` (`work_ledger.go:481-508`) — единственный держатель корня, эквивалентность обоснована §1.1–1.3/§2.2-2.3; `coordinator_work_scope.go`'s `DescendantWorkPending`/`anyPendingWorkInMemory`/`sessionOwnsPendingWork` — **удалены**, ссылка снимается. «Транзитивность глубже одного уровня» — по-прежнему не проверена чёрным тестом (то же ограничение, что фаза 0 нашла и не сняла, #1050 делает её недостижимой, а не непроверенной по недосмотру), но теперь ОБОСНОВАНА по коду (§1.1: рекурсивное выражение написано так, что не зависит от глубины) с явной оговоркой «не проверено тестом на глубину > 1». Статус: «Выполняется на проверенных путях» → уточнить: «выполняется; ранее отдельный BFS-механизм удалён как доказанно избыточный (эта фаза), не заменён на другой обходной механизм». |
1013:| ASYNC-03 | Добавить пятую строку к таблице конкурентов CAS: `cancelSession`'s собственная безусловная доставка (`work_ledger_delegation.go:174-227`) — не новый конкурент (не менялась этой фазой), но её роль в закрытии §1.4's гипотетических гэпов стоит явно отметить как часть доказательства «тикер не нужен» (перекрёстная ссылка на этот документ §1.4). |
1061:1. **Шаг 0** (тесты до удаления) — выполнен. `TestWorkLedger_
1062:   ChildScopeClosesWithoutTicker` (`work_ledger_delegation_test.go`) —
1065:   разделяемой машине) — детерминированный `TestWorkLedger_
1077:   `HasPendingAsyncJobs(sess.ID)` — по тому же рассуждению §1.1: армированная
1101:   Побочно найдено и закрыто: `workLedger.anyRunning()` осиротела уже
```

### `docs/plans/2026-09-28-async-phase4-durable-core.md` (28 hits)

```
41:- **Строка задачи** — `async_jobs`: async-задача (`bash`, `run_command`,
64:`workLedger.transition`/`commitTransition`
65:(`internal/agent/work_ledger_transition.go`).
75:  принимают `*asyncJob` исполнителя и игнорируют задачу, которая больше не
93:  в памяти (`transitionSync`, общий с `job_kill`) текстом «timed out after
105:Память (`workLedger`) хранит исполнителей, функции отмены, защёлки и
155:(`session_cancel`), `job_kill`, надзор (`check_in`), `wake_only`-таймаут,
277:| Stop, `job_kill`, прерывание | 0 |
285:Для `job_kill` итог — ответ самого инструмента, строка сразу `done`.
412:  внешнего ключа из `async_jobs` и без отметок времени для решений о жизни.
444:- безымянная строка `job_kill` (`done`, `notice_message_id IS NULL`) →
455:ребёнка есть running-строка или долг, и безымянной строки `job_kill`, чей
471:### 3.8 Stop, Rerun, делегации, ключи, `job_kill`, вопрос ребёнка
511:  ей Claim отвечает идемпотентно; `workLedger.Start` для такой строки без
515:- **`job_kill`:** после выигранной остановки инструмент отвечает
520:  `call_0` архивирует только что убитую строку, пока `job_kill` ещё
544:`workLedger` удалены; запись одна на сессию; уборка — `sweepArbiterEntries`
606:### 3.10 Фоновые shell как строки `async_jobs` (R-BG, ASYNC-13)
610:`context.Background()`), получает **ровно одну строку `async_jobs`** kind=
637:- Wake-уведомление строится при pull из строки; `job_output` во время
689:| DUR-6 | Восстановление только переводит `running` мёртвого хоста в `interrupted`/удаляет неанонсированное/возвращает безымянную строку `job_kill`; историю не пишет и сессии не будит; не держит блокировку мёртвого хоста. |
694:| DUR-11 | `delivery='done'` ⇒ строка называет сообщение истории (`notice_message_id`); «started» назван в `announce_message_id`; Rerun вычисляет множества только по этим id; `job_kill` адресует строку по `claim_id`. |
707:  тройки (триггер-аборт); гонки finish/stop/timeout/job_kill дают один
753:  убит до ack — строки нет и уведомления нет; `job_kill` — итог в ответе
777:`internal/agent/work_ledger*.go`, `async_tool.go`, `bgshell_claim.go`,
780:`notice_pull.go`, `session_activity.go`, `internal/db/sql/async_jobs.sql`.
808:- **Счёт попыток/гейт/капа в разрозненных map координатора и `workLedger`
830:  (g)/(ac): долг, строка делегации и безымянная строка `job_kill`
```

### `docs/plans/2026-09-28-async-phase4-spec.md` (162 hits)

```
7:`internal/agent/work_job.go`, `work_ledger.go`, `work_ledger_delegation.go`,
8:`work_ledger_timeout.go`, `coordinator_work_scope.go`, `coordinator_wake.go`,
31:гонка `job_kill` с естественным завершением обязана сохранить настоящий
108:`workLedger` (в памяти, один процесс) **остаётся единственным источником
117:`l.mu` (`work_ledger_timeout.go:59-62`), и по которому `recheckChild`'s DB-
119:`l.mu` (`work_ledger_delegation.go:70-78`).
127:| `Start`, новый ключ | `INSERT … ON CONFLICT (owner_session_id, tool_call_id) DO NOTHING RETURNING *` (durable claim, §3) | `asyncTool.Run`, ДО вызова `workLedger.Start` (не внутри него) | нет — до входа в `l.mu` вообще |
128:| `onToolResult`'s `acknowledged` (ack-гейт) | `UPDATE async_jobs SET announced=1 …` В ТОЙ ЖЕ транзакции, что `INSERT INTO messages` для tool-result-сообщения | `agent_turn_stream.go:383-394`, новая функция `RecordAsyncJobAnnounced` (§1.4) | нет — транзакция коммитится ДО вызова `workLedger.acknowledged`, которая держит `l.mu` как и сегодня |
129:| `finish`/`cancelSession`/`handleTimeout`'s терминальный переход, ПОСЛЕ того как `transitionToTerminal` вернул `true` (выиграл CAS) | `UPDATE async_jobs SET state=…, notice_kind=…, result_summary=…, result_is_error=…, updated_at=… WHERE owner_session_id=? AND tool_call_id=? AND state='running'` | новая `work_ledger_durable.go`, вызывается СРАЗУ ПОСЛЕ разблокировки `l.mu`, до или после `onWebDone` — порядок не важен, обе стороны независимы | нет |
146:булево поле `asyncJob.announced`, устанавливаемое `acknowledged`
147:(`work_ledger.go:339-357`) ПОСЛЕ того как `onToolResult`
161:async_jobs SET announced=1` ОДНОЙ транзакцией — «ack-гейт становится
164:### 1.2 Таблица `async_jobs`
183:CREATE TABLE IF NOT EXISTS async_jobs (
189:    origin_cli       INTEGER NOT NULL DEFAULT 0, -- byte-for-byte то же, что asyncJob.cli, для маршрутизации при восстановлении
196:    -- alone is not enough to tell job_kill and CancelAll/session-cancel apart
212:CREATE INDEX IF NOT EXISTS idx_async_jobs_lease ON async_jobs (lease_expires_at) WHERE state = 'running';
213:CREATE INDEX IF NOT EXISTS idx_async_jobs_child ON async_jobs (child_session_id) WHERE child_session_id IS NOT NULL;
214:CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_state ON async_jobs (owner_session_id, state);
215:CREATE INDEX IF NOT EXISTS idx_async_jobs_retention ON async_jobs (state, updated_at) WHERE state != 'running';
220:DROP INDEX IF EXISTS idx_async_jobs_retention;
221:DROP INDEX IF EXISTS idx_async_jobs_owner_state;
222:DROP INDEX IF EXISTS idx_async_jobs_child;
223:DROP INDEX IF EXISTS idx_async_jobs_lease;
224:DROP TABLE IF EXISTS async_jobs;
237:  `work_ledger.go:77,178`); заводить отдельный суррогатный `id` поверх нёс
257:  `existing.input != input` → отказ (`work_ledger.go:179-181`) на durable
264:`internal/db/sql/async_jobs.sql` (новый файл, следующий по алфавиту после
282:-- name: RenewAsyncJobLeasesForHost :execrows
283:UPDATE async_jobs SET lease_expires_at = ?, updated_at = ?
286:-- name: ClaimAsyncJob :one
293:INSERT INTO async_jobs (
303:-- name: GetAsyncJob :one
304:SELECT * FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?;
306:-- name: MarkAsyncJobAnnounced :execrows
314:UPDATE async_jobs SET announced = 1, updated_at = ?
317:-- name: RecordAsyncJobTerminal :execrows
322:-- cheap guard, same reasoning as MarkAsyncJobAnnounced above) is a no-op,
324:UPDATE async_jobs
329:-- name: MarkAsyncJobDelivered :execrows
330:UPDATE async_jobs SET delivered_at = ? WHERE owner_session_id = ? AND tool_call_id = ? AND delivered_at IS NULL;
332:-- name: DeleteUnannouncedAsyncJob :execrows
333:-- Mirrors workLedger.abort (work_ledger.go:364-378): the "started" tool
335:DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0;
337:-- name: ListStaleAsyncJobs :many
343:SELECT * FROM async_jobs WHERE state = 'running' AND lease_expires_at < ?
346:-- name: ListAsyncJobsForOwner :many
347:SELECT * FROM async_jobs WHERE owner_session_id = ? ORDER BY created_at ASC;
349:-- name: ListRunningAsyncJobsForOwners :many
355:SELECT * FROM async_jobs WHERE owner_session_id IN (sqlc.slice('owner_ids')) AND state = 'running';
357:-- name: PurgeTerminalAsyncJobsOlderThan :execrows
365:DELETE FROM async_jobs
369:Генерируется в `internal/db/async_jobs.sql.go` тем же `sqlc generate`,
381:if ts.a.asyncJobs != nil {
383:        ts.a.asyncJobs.abort(sessionID, result.ToolCallID)
385:        ts.a.asyncJobs.acknowledged(sessionID, result.ToolCallID)
391:несёт `messages message.Service` (`agent.go:656`) и `asyncJobs *workLedger`
409:// internal/session's RecordAsyncJobAnnounced). Create becomes a thin
417:// RecordAsyncJobAnnounced persists the tool-result message for
418:// (ownerSessionID, toolCallID) AND marks the durable async_jobs row
426:// ever fires for a job THIS process's own workLedger.Start just created in
430:func (s *service) RecordAsyncJobAnnounced(
447:    if _, err := qtx.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
464:if ts.a.asyncJobs != nil && ts.a.sessions != nil {
465:    createdMsg, createMsgErr = ts.a.sessions.RecordAsyncJobAnnounced(ts.ctx, ts.a.messages, sessionID, result.ToolCallID, message.CreateMessageParams{Role: message.Tool, Parts: []message.ContentPart{toolResult}})
469:if ts.a.asyncJobs != nil {
471:        ts.a.asyncJobs.abort(sessionID, result.ToolCallID)
473:        ts.a.asyncJobs.acknowledged(sessionID, result.ToolCallID)
480:`RecordAsyncJobAnnounced` вызывается только тогда, когда `result.ToolCallID`
483:async-обёрнутых четырёх. `MarkAsyncJobAnnounced`'s `execrows`-возврат `0`
488:(симметрично тому, как `ts.a.asyncJobs.acknowledged` уже сегодня
489:безусловно вызывается для каждого tool-call, `work_ledger.go:339-357`
493:получает `messages`/`asyncJobs` сегодня (`agent.go`'s строительная функция
508:координатора (симметрично `c.asyncJobs = newWorkLedger(...)`,
557:| `asyncJobLeaseStaleAfter` | 90s (= 4.5×`asyncHostHeartbeatInterval`) | Больше вдвое задокументированного 45-секундного простоя — переживает ОДИН полный такой простой, оставляя запас на джиттер планировщика и на то, что сам heartbeat-запрос тоже может встать в очередь SQLite позади других писателей. |
560:(`work_ledger_timeout.go:76-105`): один тикер на процесс (не на
583:(`TouchAsyncHostHeartbeat`, `RenewAsyncJobLeasesForHost`) — не
608:// SweepInterruptedAsyncJobs finds every 'running' async_jobs row whose
611://     ASYNC-05): delete the row silently, mirroring workLedger.abort --
632:func SweepInterruptedAsyncJobs(ctx context.Context, s *service, messages message.Service) (reconciled int, err error)
679:| До `BEGIN` | `state='running'`, старый `lease_expires_at` | Находит строку снова (тот же `ListStaleAsyncJobs` запрос), начинает заново — идемпотентно |
680:| Внутри транзакции, до `COMMIT` (включая между `INSERT INTO messages` и `UPDATE async_jobs`) | Транзакция откатывается СУБД при следующем открытии файла (SQLite WAL/rollback journal) — строка НЕ МЕНЯЛАСЬ, `state` всё ещё `'running'` | То же — начинает заново, message-инсерт (если был) тоже откачен, значит повторный `INSERT` не дублирует сообщение |
681:| После `COMMIT`, до `MarkAsyncJobDelivered` (если бы это была отдельная стадия) | Не применимо — `delivered_at` пишется ВНУТРИ той же транзакции, что и `state`/message-insert (см. псевдокод ниже), не отдельным шагом | — |
694:`ListStaleAsyncJobs`, читающий эту же строку ДО того, как первая
699:гонка: два процесса читают `ListStaleAsyncJobs`, оба видят одну и ту же
708:0 строк (`execrows=0`, тот же WHERE-guard, что `MarkAsyncJobAnnounced`
717:Сегодня (`internal/agent/async_tool.go:77-94`): `workLedger.Start` —
732:процессе, чей `workLedger` пуст и не может знать, что этот вызов уже
735:Решение — durable claim СНАРУЖИ `l.mu`, ДО вызова `workLedger.Start`:
739:// до текущего workLedger.Start)
756:    // row's own recorded child session, exactly like workLedger.Start's
764:            // timeout for a job it did not just create (work_ledger.go:195-200
771:    // in-memory Start's own "different input" branch (work_ledger.go:179-181).
774:job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
778:Порядок проверок: durable claim СНАЧАЛА, `workLedger.Start` — ВТОРЫМ.
793:`workLedger.abort` (`work_ledger.go:364-378`) — если запись «started»
794:tool-result не удалась. Durable-версия: `DeleteUnannouncedAsyncJob`
806:если РЕАЛЬНЫЙ краш произошёл МЕЖДУ `ClaimAsyncJob`'s успешным `INSERT` (с
820:        // narrow window between ClaimAsyncJob and CreateTaskSession). Safe
836:вместо повторного `ClaimAsyncJob`.)
893:| `internal/session/descendant_liveness.go` (`LiveDescendants`, `LiveDescendant`, `SubSessionLister`, `maxDescendantWalkDepth`) | `internal/session/async_job_reader.go`'s `LiveJobs` (ниже) — тот же BFS-по-`parent_session_id` каркас (глубина 16, cycle-guard, `walkIncomplete`), но узел проверяется запросом к `async_jobs`, не `InspectSessionLock` |
926:// note on depth 0 above) that durably owns a 'running' async_jobs row,
936:func LiveJobs(ctx context.Context, lister SubSessionLister, store AsyncJobReader, rootSessionID string) (live []LiveJob, walkIncomplete bool)
954:`store AsyncJobReader` — узкий интерфейс (`ListRunningAsyncJobsForOwners`
958:узел; `ListRunningAsyncJobsForOwners(ctx, []string{n1,n2,...})` — один
980:  (`ListAsyncJobsForOwner`, либо `ListRunningAsyncJobsForOwners` по
987:  эквивалент `job_kill`-инструмента: для строки с `state='running'` и
989:  через `job_kill` (нужен способ достучаться до ЖИВОГО host'а — если
1019:func (s *service) ListAsyncJobsForOwner(ctx, sessionID) ([]AsyncJobRow, error)
1023:держит in-memory `workLedger` для этой самой сессии (durable write-through,
1027:`ListAsyncJobsForOwner` плюс `result_summary`/`updated_at`, уже
1038:`PurgeTerminalAsyncJobsOlderThan` (§1.3) — не автоматический фоновый
1050:  вызывает `PurgeTerminalAsyncJobsOlderThan` с 7-дневным окном на каждом
1061:- `async_jobs.owner_session_id REFERENCES sessions(id) ON DELETE CASCADE` —
1071:  `DELETE FROM async_jobs WHERE owner_session_id = ?` тем же образом, что
1086:`async_jobs`/`async_hosts`, теряет ВСЕ durable-записи (обычный DROP TABLE,
1098:таблицы уже созданы, но код фазы 4 отключается): `workLedger`'s durable
1099:поле (§6, `l.durable AsyncJobDurableStore`) — `nil`-safe на каждом вызове
1101:`work_ledger.go`/`work_ledger_timeout.go`). Установка `l.durable = nil`
1103:не вызывать `newAsyncJobDurableStore` при постройке координатора)
1114:20260929000001_add_async_job_ledger.sql`, `internal/db/sql/async_jobs.sql`,
1115:сгенерированный `internal/db/async_jobs.sql.go`, плюс
1133:`DeleteUnannouncedAsyncJob`-обёртка), `internal/session/async_job_ack.go`
1134:(`RecordAsyncJobAnnounced`), `internal/message/message.go`'s
1140:message-insert, ДО async_jobs-update, через тестовый `db.Querier`-мок,
1146:`TestSweepInterruptedAsyncJobs_AnnouncedJobBecomesInterrupted`,
1147:`TestSweepInterruptedAsyncJobs_UnannouncedJobIsDeletedSilently`,
1148:`TestSweepInterruptedAsyncJobs_ConcurrentSweepsDeliverOnce` (два
1156:  `RecordAsyncJobAnnounced` (§1.4); `sessionAgent` получает поле
1160:  постройкой `c.asyncJobs`; `c.asyncJobs.durable = adapterOver(c.asyncHosts)`.
1161:- Новый `internal/agent/work_ledger_durable.go`: интерфейс
1162:  `asyncJobDurableStore` (нужные `workLedger` методы — `RecordTerminal`,
1164:  (`work_ledger.go:562-599`), `cancelSession`
1165:  (`work_ledger_delegation.go:198-233` — ОБЕ ветки, включая ПЛОСКУЮ
1168:  (`work_ledger_timeout.go:135-191`, обе ветки).
1174:- `internal/agent/work_ledger.go`/`internal/agent/tools/job_kill.go`:
1195:`PurgeTerminalAsyncJobsOlderThan`'s вызывающий код. Тест:
1196:`TestSessionsGc_PurgesTerminalAsyncJobsOlderThanCutoff`.
1203:### 7.1 Гонка `job_kill` против естественного завершения (#1063) — находка и фикс
1206:`StopRunCommandJob` (`work_ledger.go:521-548`) и `MarkJobStopped`
1213:заменяет РЕАЛЬНЫЙ переданный `result` на «stopped (job_kill)» текст и
1216:и `job_kill` физически опоздал: процесс уже закончился, а
1226:которого `job_kill`, обратившись именно к ЭТОЙ, всё ещё присутствующей в
1228:успех** («kill requested», `job_kill.go:105-109`) — хотя момент, когда
1229:это тело `job_kill`-инструмента выполнилось, находится СТРОГО ПОСЛЕ
1234:прямое нарушение «job_kill, racing with natural completion, must keep the
1248:// work_ledger.go, правка StopRunCommandJob (аналогично MarkJobStopped)
1249:func (l *workLedger) StopRunCommandJob(owner, jobID string) error {
1251:    var job *asyncJob
1286:`bgManager.KillOwned` отдельно в `job_kill.go`) — она должна попытаться
1290:таймаут уже использует, `work_ledger_timeout.go:216-234`), а не
1293:Прямое следствие для `job_kill.go` (`:88-94`): вызывающий должен
1306:| `TestAsyncJobStore_ClaimIsIdempotentAcrossProcesses` | Два `ClaimJob` с тем же `(owner, toolCallID, inputHash)` из двух отдельных `*workLedger`/DB-хендлов (симулирует два процесса) — второй видит `claimed=false`, читает ПЕРВЫЙ клейм | Убрать `ON CONFLICT DO NOTHING` — тест ловит ошибку вместо мирного возврата существующей строки |
1307:| `TestAsyncJobStore_ClaimRefusesDifferentInputSameKey` | Тот же ключ, другой `inputHash` — отказ, не тихий успех | Убрать `input_hash`-сравнение — тест ловит ложный `claimed=true` |
1308:| `TestRecordAsyncJobAnnounced_MessageAndFlagCommitTogether` | Форс-ошибка на втором `UPDATE` — проверить, что message-insert ТОЖЕ откатился (запрос той же сессии после — ноль строк) | Разбить на два отдельных, не-транзакционных вызова — тест ловит осиротевшее сообщение без `announced=1` |
1309:| `TestSweepInterruptedAsyncJobs_DeliversExactlyOnceUnderConcurrentSweep` | §2.4 — две горутины, один `ListStaleAsyncJobs`-результат, обе пытаются реконсилировать одну строку | Убрать `WHERE state='running'` guard из `UPDATE` — тест ловит два сообщения |
1310:| `TestAsyncTool_ReplayedCallAfterCrashDoesNotRerunExecutor` | Симулировать «новый процесс» (свежий `*workLedger`, старая DB-строка) — `t.run`'s исполнитель НЕ вызывается второй раз (шпион-счётчик) | Убрать durable claim из `Run` — тест ловит второй вызов исполнителя (двойной побочный эффект) |
1313:| `TestStopRunCommandJob_WinningRaceProducesStoppedOutcome` | `StopRunCommandJob` выигрывает ПЕРВЫМ — `finish`, вызванный позже executor'ом, — no-op (проигранный CAS), доставленный текст — «stopped (job_kill)» | Убрать CAS-проверку у `StopRunCommandJob` (безусловный `transitionToTerminal`) — тест ловит двойную доставку |
1315:| `TestLiveJobs_DeadHostRunningRowNotReportedLive` | Строка `state='running'`, `lease_expires_at` в прошлом, sweep ещё не прошёл — `LiveJobs` не считает её живой (read-time reconciliation, §4.2) | Убрать `lease_expires_at`-фильтр из `ListRunningAsyncJobsForOwners` — тест ловит ложно-живую строку |
1319:явный список выше): весь `work_ledger_test.go`/
1320:`work_ledger_delegation_test.go`/`work_ledger_timeout_test.go`'s набор
1329:| ASYNC-01 | «Не больше одной активной делегации на дочернюю сессию» — статус остаётся «не проверено async-специфично» (не входит в объём этой фазы), но добавляется примечание: durable `ClaimAsyncJob`'s PK-конфликт даёт ВТОРУЮ, кросс-процессную линию защиты от двойного клейма того же `(owner, toolCallID)`, независимую от mailbox-эксклюзивности. |
```

### `docs/plans/2026-10-01-bg-shell-ledger.md` (22 hits)

```
1:# R-BG-D: фоновые shell как строки `async_jobs` — дизайн
4:shell, у которого сегодня нет строки в `async_jobs`; удержания завершения;
34:   60 с/`auto_background_after`). Строка `async_jobs` (kind=`command`,
36:   (`asyncJobs.setShellID`, `internal/agent/async_tool.go:299`) и блокируется
64:  читается `GetOutput` по требованию (`job_output`).
76:| «процесс завершился» → «строка `bg_shell_done` записана» | R7B-1: re-check делегации отпускал ребёнка со старым текстом; закрыто удержанием (`PendingCompletionsOwned`), т.е. ещё одним куском памяти-правды (`internal/agent/work_ledger_delegation.go:213-251`) |
87:`childScopeDrained` (`work_ledger_delegation.go:213-251`): `ActiveOwned`
99:`run_in_background` и авто-фон), получает строку `async_jobs`:
123:`AsyncJobStore.Transition`:
131:- `job_kill` / Stop / таймаут-terminate: `state='cancelled'`,
132:  `notice_kind='job_kill'`/`session_cancel`, `delivery='done'`, `wake=0`,
158:### 2.4. `shell_id` в `async_jobs`
163:- `job_output`/`job_kill` разрешают job_id → строка → shell_id →
165:  (`asyncJobs.setShellID`, `job_shell_resolver.go`); колонка делает её
198:- `job_output` во время работы: как сегодня, из памяти менеджера (живой
239:1. `shell_id TEXT` в `async_jobs` + частичный индекс. SQLite: колонка —
242:   расширить ALTER'ом — требуется rebuild таблицы (создать `async_jobs_new`
276:  как обычный участник (пятый писатель reacted не появляется: job_kill/Stop
279:  инструмента, имеет ровно одну строку `async_jobs`, и её терминальный
309:1. **Rebuild `async_jobs` ради CHECK.** Риск: блокировка таблицы на время
321:   переносимой, но не вывод: `job_output` с другого хоста увидит итог из
338:   рождается из обёрнутого вызова (у которого уже есть строка `async_jobs`),
```

### `docs/plans/2026-10-01-in-turn-progress-guard.md` (13 hits)

```
21:| wa14 20:24–20:43 | 102 | 80 | `view` work_ledger_inline_test.go, строки 176–210, окна 1–18; `edit` в наборе БЫЛ | 299K |
65:| `act` | может менять мир: `edit`/`multiedit`/`write`/`fs_write*`/`fs_replace`/`fs_delete`/`download`, `bash`/`run_command` с реальной командой, `agent`/`agentic_fetch`/`fetch`, `job_kill`, `inject_agent`/`stop_agent`, `wakein`/`wakeon`/`loop`/`wake_cancel`, неизвестные (MCP). Ошибка в результате не важна: неудачная правка — тоже повод перечитать | да | очищает |
70:| `neutral` | `job_output`, `todos` (ровно `chainNeutralTools`); `Invalid` (инструмента нет в наборе) | не меняет | — |
76:#1113 сохраняет своё отображение: нейтральны `job_output`/`todos`, холостой
111:- `ownWork` = `workLedger.running(sessionID)` (у сессии есть недоставленная
120:W — wait, D — reread, R — новое чтение, A — act, N — `todos`/`job_output`,
173:  инструмент не вызывается: нет `workLedger.Start` → нет claim, строки
174:  `async_jobs`, уведомления; hooks и разрешения не запускаются; результат пишется
264:  sleep/echo commands: each completion wakes you again», `job_output` — «do not
305:     покрыты) → reread; grep → read; `job_output`/`todos` → neutral; `edit` с
316:   `recordStepFinish`). `workLedger.running` уже есть.
322:     инструмента не вызван, строк `async_jobs` 0; без `ownWork` — проход.
389:  (одинаковые вызовы — его), `job_output` (A13 закрыт его `StopTurn`; опрос без
```

### `docs/plans/2026-10-01-inline-window.md` (8 hits)

```
20:tool-result, несущим результат — та же форма, что pull (DUR-3) и job_kill
38:  `wake=0`, `reacted=1` (как у job_kill), `notice_kind` не трогать.
39:  `ErrAsyncJobGone` → откат + обычный Create (как B15); 0 строк в CAS (только
46:Stop (`ctx`/`stoppedBySession`, нотис cancelled, wake=0); job_kill (модель ещё
82:результат сразу». Новый файл `internal/agent/work_ledger_inline.go`
83:(`awaitInline`, `finishInlineLocally`; `work_ledger.go` уже ~974 строки).
85:sqlc-запрос `DeliverAsyncJobInline` в `internal/db/sql/async_jobs.sql`.
118:(две схемы доставки, нет job_kill по id, timeout и recovery — откат фазы 2);
```

### `docs/plans/2026-10-01-reaction-chain-guard.md` (3 hits)

```
11:Drain-gate (`work_ledger_reaction.go`, `freeStreak`/`paidStreak`) считает только
43:  текст, `job_output`, `todos`. Прогресс: edit/write/view/grep, `bash` с реальной
44:  командой, `agent`/`agentic_fetch`, `job_kill`.
```

### `docs/plans/2026-10-01-run-cost.md` (1 hits)

```
71:| W9 | `internal/agent/work_ledger_delegation.go:144` | УДАЛИТЬ |
```

### `docs/plans/2026-10-01-shared-data-dir.md` (2 hits)

```
11:- `rush run` в новом linked worktree пишет сессии, сообщения, `async_jobs` и логи
297:retention `async_jobs`/`session_notices`; межпроцессная ротация логов; колонки cwd
```

### `docs/plans/2026-10-01-structural-refactor.md` (2 hits)

```
20:| Фоновые shell | ~9 (P1 R6B-1, P2 R7B-1, R8B-2) | новая дыра каждый раунд | вторая правда вне `async_jobs` |
73:### R-BG — фоновые shell как строки `async_jobs`
```

### `docs/plans/2026-10-01-turn-arbiter.md` (11 hits)

```
9:`internal/agent/drain_attempt.go` и гейт в `work_ledger_reaction.go`), плюс
29:- `workLedger` под `l.mu` (`work_ledger.go`, рядом с `hintSeq`): `drainGate`
57:| `drainGateOpen` / `paceDrainGate` (`work_ledger_reaction.go:169`, `:190`) | пейсинг и сон: открыт ли гейт, спит ли (free/paid), `retryAt` | R2B-1(C)/(D) (hot loop), R3B-4 (две серии вместо одной) |
68:| `childScopeDrained` / `childScopeOpenAcrossProcesses` (`work_ledger_delegation.go:213`, `:263`) | когда делегация выпускает ребёнка (область ребёнка закрыта?) | R3C-5 (удержание держит делегацию), R7B-1 |
69:| `recheckChild` (`work_ledger_delegation.go:98`) | запускатель припаркованной делегации | R2B-17 (early return) |
75:| `drainGate{retryAt, hintAt, hintOpens, freeStreak, paidStreak}` (`work_ledger.go`, под `l.mu`) | пейсинг/сон, открывание по новому факту | R3B-4, R3B-8 |
133:`AsyncJobStore` (то, что сегодня делают `ReactionDebtExists`,
210:   (`coordinator.autoResumeMu` и `workLedger.l.mu`). Сведение в одну
217:workLedger'а; реализация R-ARB-1 может сначала просто агрегировать четыре
434:- Не переносить фоновые shell в строки `async_jobs` (R-BG идёт после
493:`workLedger` удалены; всё живёт в `arbiterState` под одним мьютексом
```

### `docs/plans/2026-10-02-async-job-cap.md` (28 hits)

```
21:`maxAsyncJobsPerSession = 50` (`internal/agent/work_ledger.go:24`). Проверяется
22:одна вещь: `len(s.jobs) >= 50` в `workLedger.Start` (`work_ledger.go:271-273`).
32:| Терминальный переход задачи, уже получившей ack | `commitAndDeliver` → `deliverLocked` (`work_ledger_transition.go:336-354`, `work_ledger.go:375,425`) | сразу после коммита CAS (Tx1), то есть **до** забора уведомления |
33:| Терминал раньше ack (быстрая задача) | `finishAcknowledgeLocally` → `deliverLocked` (`work_ledger.go:488-500`) | когда приходит ack; fantasy вызывает `OnToolResult` сразу после `Run` этого же инструмента (fantasy v0.25.2 `agent.go:778`) → `persistToolResult` (`work_ledger_announce.go:103`) — это миллисекунды |
34:| Inline-ответ A14 | `finishInlineLocally` (`work_ledger_inline.go:152-169`) | при Tx2 |
35:| job_kill бегущей | `MarkJobStopped` → `commitAndDeliver` (`work_ledger.go:740`) | при коммите, ещё до kill |
36:| Stop | ветка `stoppedBySession` (`work_ledger.go:393-413`), страж окна (`work_ledger_inline.go:120`) | при коммите/ack |
38:| Сбой записи ack | `abort` → `dropLocked` (`work_ledger.go:510-543`) | сразу |
42:прямо сказано в B1 (`work_ledger.go:604-611`). Строки `delivery='pending'` есть
67:- «Снялось job_kill»: в wa14 job_kill остановил **бегущий** `agentfull49`. Если
68:  бы job_kill попал в терминальную запись, она бы осталась: `MarkJobStopped`
69:  отвечает `AlreadyTerminal` и запись не удаляет (`work_ledger.go:711-718`).
80:и область, и не даёт освободить делегацию (`work_ledger_delegation.go:213`).
81:Страж Stop-в-окне (`work_ledger_inline.go:110-122`) закрывает единственный
113:| ASYNC-04/05, DUR-7 | отказ без строки и без claim-тега → `claimAck` возвращает nil (`work_ledger_announce.go:58-60`) → обычный Create |
140:- free slots: job_kill the jobs you no longer need, timers first
160:  your turn… or job_kill…; do not poll with sleep/echo» вместо «wait for some
166:1. **Допуск по нетерминальным.** Новый файл `internal/agent/work_ledger_cap.go`
167:   (в `work_ledger.go` 994 строки, расти ему нельзя): `runningJobsLocked(s)` —
170:   переход. `work_ledger.go:271-274` заменяется вызовом, итог по строкам
181:## 5. Тесты (реальный ledger: `newWorkLedger` + `newTestAsyncJobStore`; файл `work_ledger_cap_test.go`)
183:- **T1** `TestWorkLedgerCap_AckedInstantJobsNeverAccumulate`: 60 × (Start → ack
188:- **T2** `TestWorkLedgerCap_UnacknowledgedTerminalDoesNotHoldSlot`: 60 × (Start
192:- **T3** `TestWorkLedgerCap_RunningJobsStillCapped`: 50 бегущих (25 с ack,
197:- **T4** `TestWorkLedgerCap_RefusalNamesTimers` + `TestAsyncTool_CapRefusalMetadata`:
199:  `job_kill`, «NO tool call», `ask_question`; первые id в списке — таймеры,
203:  при `maxJobs=1`: префикс сохранён, есть «end your turn» и «job_kill», нет
206:Прогон: только `go test ./internal/agent -run 'WorkLedgerCap|AsyncTool_Cap'
```

### `docs/plans/2026-10-02-reviewer-pass-verification.md` (1 hits)

```
251:| T1 | `TestBuildTools_ReviewerRoleIsReadOnly` (agent/coordinator_tools_reviewer_test.go, настоящий `c.buildTools`) | В наборе view, grep, glob, ls, git_read; нет bash, run_command, edit, multiedit, write, download, fetch, todos, ask_question, agent, job_kill, MCP; с FolderScope, разрешающим create: fs_read есть, fs_write нет | убрать вызов `applyCallReviewerReadOnly` |
```

### `docs/plans/2026-10-02-worker-question-wake.md` (9 hits)

```
10:| 23:56:32 | Корень `wskills` (CLI): `agent` → «Async agent job call_32fb… started». Строка `async_jobs(wskills, call_32fb…)` = running. |
27:4. `async_tool.go:336-338` finalize → `armDelegation` (`work_ledger_delegation.go:57-74`): вопрос ложится в `job.result` в памяти, строка X остаётся running.
28:5. Единственный путь к родителю — release в `recheckChild` (`work_ledger_delegation.go:98-178`), гейт `:115` `childScopeDrained`: `:213` `l.running(child)`, `:224` bg-шеллы, `:243`/`:270` `CLIScope.WorkOpen` (running-строки, `wakein`-расписания, `coordinator_reaction_source.go:233-254`). Отложенный ДОЛГ (вопрос) делегацию отпускает (`:253-255`, `TestChildScope_DeferredDebtReleasesDelegation`), живая РАБОТА — нет. Ни notice, ни wake родителю больше нигде не порождается.
29:6. Supervision (`supervision.go:400-431`) видит X как «running»; `capturePartialDelegation` (`work_ledger_timeout.go:317-338`) берёт последнее ассистентское сообщение С ТЕКСТОМ — у вопроса текста нет (он в tool_call и `finish.details`) → «Now mutant m2:» из давнего шага; совет `:423` «Keep waiting…».
33:8. `agent resume_session_id=C` → `async_tool.go:96` Start → `AsyncJobStore.Claim` (`session/async_job_store.go:307-337`): индекс `idx_async_jobs_child_running` (ASYNC-01) → `ErrAsyncChildSessionBusy` «the sub-agent still has work running from a previous delegation; wait for its result» (`:55-56`). Это осознанное решение фазы 4 (durable-core.md §3.8: «resume при делегации, ход ребёнка которой закончен, а задачи идут → ошибка»), но вопрос в нём не учтён: результата не будет, пока не ответят.
98:- wguardrail: перечисленные файлы — вне плана. `work_ledger.go` (994 строки) не трогаем.
103:Новый `internal/agent/delegation_question.go` (`childAwaitingQuestion`, `noteChildQuestion`, текст); `work_job.go` (`questionNoticed`, `answerHold`); `work_ledger_delegation.go` (вызов в ветке «не осушена»); `coordinator_agent_control.go` (`childLastActivity` на предикате); `session/notice_pull.go` (`NoticeKindChildQuestion`, void как у wake_only). Переякорить ASYNC-02, словарь DUR-3.
111:`async_tool.go` (перехват до Start, ~8 строк), `delegation_question.go` (`answerHeldDelegation`), `work_ledger_delegation.go` (проверка `answerHold`). ASYNC-01 — пометка «ответ в удерживаемую делегацию строку не создаёт»; CHANGELOG.
114:- T2c e2e `internal/app` (скриптованный провайдер, харнесс `app_run_wake_hold_e2e_test.go`), supervision 1 ч, дедлайн 60 с: корень → `agent`; ребёнок → `bash sleep 120` + `ask_question`; корень на «SUB-AGENT QUESTION» → `agent(resume_session_id=…)`; ребёнок → `job_kill` + текст; корень → финал. Exit `end_turn` < 60 с, supervision-тиков нет. Без фикса — дедлайн. (Выключить событие → таймаут; выключить перехват → отказ resume ломает скрипт.)
```

### `docs/plans/2026-10-05-async-phase4-rev8-map.md` (8 hits)

```
22:| §3.7 Восстановление | §3.7 | Влито + (g) (ретеншн не удаляет долг и безымянную строку `job_kill`), (z), (ac). |
23:| §3.8 Stop, Rerun, делегации | §3.8 | Влито + (f),(r) (Rerun: одна транзакция, `announce_message_id`, `CancelTurn`), (h),(n),(o) (ключи/ack/job_kill), carve-out ASYNC-01 (#1157 — ответ в удерживаемую делегацию). |
32:| (f) Rerun: одна транзакция, `announce_message_id`, void маркера `wake_failed`, `CancelTurn`, `StopRerunJobs` | §3.8 | Влито; (r) (остановка Rerun-задач как Stop, не `job_kill`) вплавлена в тот же пункт; удержание автоходов Rerun (m.7, x.10 — `drainPaced` без часов, переживает сообщение человека — P1-2) — в §3.9. |
33:| (g) ретеншн: не удаляет долг, `pending`, безымянную строку `job_kill`; глубина Rerun = глубина ретеншна | §3.7 | Влито; безымянная строка `job_kill` добавлена (ac.1). |
34:| (h) переиспользование `tool_call_id`: архив `<id>#reused#<uuid>`, идемпотентный Claim, `job_kill` по `claim_id` | §3.8 | Влито (включая R3A-2 про `claim_id`). |
41:| (o) результат `job_kill` не теряется: метаданные, `RependJobKillRowWithoutNotice`, адресация по `claim_id` | §3.8 | Влито; адресация по `claim_id` (R3A-2) вплавлена. |
52:| (z) восстановление: разделяемая проба без удержания блокировки; параллельные восстановители, CAS по `claim_id`; один удаляющий файл; защита `job_kill` оборонительная | §3.7 | Влито целиком. |
55:| (ac) раунд 5 (store): безымянная строка `job_kill` не удаляется ретеншном; зачистка пустого хоста через `RecoverDeadHost`-схему; мёртвый SQL удалён, комментарии схемы | §3.7, §3.6 | Влито. |
```

### `docs/plans/2026-10-05-bg-shell-remainder.md` (19 hits)

```
1:# R-BG-1, остаток: переход строки bg_shell и `job_kill` по shell_id
13:и `rush run` зависают). Второй P1: `job_kill` по shell_id для процесса, который не умирает.
24:  `work_ledger_delegation.go:236`.
34:  (`work_ledger_delegation.go:255,277`); recovery (`async_job_recovery.go:97-103`) трогает только мёртвые хосты.
35:- `job_kill` по голому shell_id (`tools/job_kill.go:98,153,169`): регистр не трогается, `KillOwned` убивает процесс,
37:  платный автоход. Обычный `job_kill` для сравнения: `cancelled`/`job_kill`/`done`/`wake=0`/`reacted=1`
38:  (`work_ledger_transition.go:107-109`). `job_kill(job_id=<shell id>)` для `running` bg_shell отвечает ложным
39:  «job not found» (`work_ledger.go:618`, `work_ledger_pending.go:24`).
50:| `job_kill` по shell_id, процесс умер | `failed` вместо `cancelled` + лишнее пробуждение (так было и до R-BG-1) | P2, шаг 2 |
51:| `job_kill` по shell_id, процесс не умирает (R8B-2) | `OnDone` не срабатывает, строка `running` навсегда; раньше удержание истекало через 10 мин | **P1, шаг 2** |
68:  строка `running`. (2) `internal/app/app_run_bgshell_notify_off_test.go` по образцу `app_run_job_kill_by_id_test.go`:
73:## Шаг 2. `job_kill` по shell_id для строки bg_shell (P1 R8B-2 + P2 текст/пробуждение)
75:- Новый файл `internal/agent/work_ledger_bgshell_kill.go`: `StopBackgroundShellRow(owner, shellID) (text, claimID,
77:  частичного вывода (`GetOwned` + `GetOutput`), затем `Transition(cancelled, notice_kind=job_kill, delivery=done,
80:- `tools/job_kill.go`: опциональный интерфейс `BGShellStopper` (type assertion), вызывается до `GetOwned`, когда задан
82:  `JobID=shellID` и `KilledClaimID`: склейка и re-pend (`work_ledger_announce.go:258`) работают без изменений.
86:- Законы: ASYNC-13 — «job_kill по shell_id → `cancelled` по запросу, а не по смерти процесса; несклеенный результат
88:- Тесты: (1) kill по shell_id → строка `cancelled`/`job_kill`, `reacted=1`, `bg_shell_done` нет; revert-check: убрать
89:  ветку в `job_kill` → `failed` + уведомление. (2) shell с незакрывающимся `done` + kill → `childScopeDrained=true`
```

### `docs/plans/2026-10-07-job-batches-design.md` (8 hits)

```
2159:- Exclude batch-managed work from the stall-detach wrapper on that same path,
2161:  a batch leaf and a `stall-*` job.
2188:`async_jobs` has primary key (owner_session_id, tool_call_id), claim_id-scoped
2191:`internal/db/sql/async_jobs.sql`). `pullOneJobNotice` performs its delivery CAS
2211:- Keep `async_hosts`, host locks, and `session_notices`; drop `async_jobs`
2316:| TODAY | Any returns StopTurn; rule 1 requires debt. All arms SleepAll and uses rule 4b. | Current worker captures (owner,tool_call_id) pairs at entry, not claim_id (9d390c38; `coordinator_await_tasks.go: awaitTasksWorker`). Any completes when one captured pair leaves a COMPLETE read; all when none remain. Incomplete reads remove nothing. No StopTurn or stall-detach. |
2402:`job_kill`/`job_output` may be aliases resolving item IDs, or may be removed;
2498:| I4 | Policy construction and stall exclusion | 9.1 Pre-refactoring still worth doing | §9.4 policy/MCP/sibling-call integration acceptance; autonomous scenario not applicable | Deferred to the §9.1 policy-wiring owner: ordinary batches use the pinned policy toolset and are excluded from stall-detach. |
```

### `docs/plans/2026-10-12-job-batches-rush-integration.md` (12 hits)

```
19:   (`async_jobs`, work ledger, stall jobs) stays the only authority and the
24:   (`RUSH_BATCH_RUNTIME`, default off, not a user feature and not documented
40:| 3 | Terminal on stop intent is legacy | `job_kill` and run_command cancel commit "stopped" before physical termination (map A.C, C.C) | Removed, not ported; stop intent, control answer and termination are three facts |
45:| 8 | "Drop `async_jobs` without migration" | Existing databases still need a migration to drop it; `check_db_release_pairing*` checks connection release pairing, not schema (map B.1) | A new destructive Goose migration, no row conversion (D2) |
54:| D2 | "Without migration" = a new destructive Goose migration drops `async_jobs`, `Down` recreates it empty; no row conversion; `batch_nodes` and `batch_control_requests` are new tables in an earlier additive migration (R5). | Machine-readable, works on existing databases; matches "no preservation". |
58:| D6 | Remove `job_kill` and `job_output`; `batch_stop` / `batch_output` / `batch_status` accept item ids. No aliases. | "No legacy preservation". |
68:acceptance of that package. "Flag" = behind `RUSH_BATCH_RUNTIME` until R12.
84:| R6 | Managed-call stall-detach exclusion (including dynamic MCP) | R1 | S | managed calls create zero `stall-*` identities; unmanaged behavior unchanged |
98:| R10c | History and activity consumers: `sessions reset`, rerun (`internal/session/async_job_rerun.go`, `internal/agent/coordinator_rerun.go`), owner deletion, `sessions jobs/why/show/list/gc/audit`, server handlers (`handlers_livework.go` backend, sessions, agent rerun) and the remaining app/CLI consumers of map B's dependency table, each ported to node rows: atomic reset refusal for every nonterminal root state, selected-node delivery reconciliation with deleted history, incarnation-scoped post-commit Stop routing, disposal of legacy bound `session_notices` | R5, R8, R9 | L | reset refused for Pending and Cancelling roots; deleted ack/notice history reconciled; late completion after rerun never reaches a new incarnation; grep inventory of every `AsyncJob`/`async_jobs` consumer with its replacement test |
105:| R12 | Flip: new path is the only path; delete work ledger terminal authority, `async_jobs` readers/writers, stall jobs, `job_kill`/`job_output`; destructive migration behind a **pre-migration live-host gate** (probe every `async_hosts` identity with the OS host-lock probe; refuse the destructive DDL while any host is Alive or Unknown, with a clear message to stop other rush processes); remove the switch | R1-R11, R10c, R13, R15a | L | populated-DB upgrade with zero row copy; an already-running OLD host (a process holding a host lock) makes the new binary refuse the destructive DDL; no remaining `async_jobs` callers (grep gate backed by R10c's inventory); commit-fault subprocess exits and recovery sweeps; shutdown drain; full repository checks once |
111:| R13 | **Before R12.** Model tools `batch_run`, `batch_add`, `batch_status`, `batch_output`, `batch_stop` + prompts/descriptions (they replace `job_kill`/`job_output`); `await_tasks` exposes batch ids | R11 | M-L | restricted/scoped availability; no recursive ingress; OneItem all vs explicit each; the model can inspect and stop a detached execution through them (live run in the flagged mode) |
112:| R15a | **Before R12.** Web backend/wire/store cutover: structured node snapshot replaces the flat `async_jobs` livework rows and text-parsed completions (`internal/server/handlers_livework.go`, `web/src/types.ts`, completion indexing) | R10a | M-L | server tests; the existing web unit and Playwright suites pass with the structured wire; active work does not silently disappear |
```

### `docs/research/2026-10-12-job-batches-rush-map-a.md` (11 hits)

```
15:| Ledger | `internal/agent/work_ledger.go` |
32:**stall-detach → hooks → restricted-run → async → underlying tool**
62:**OBSERVED — delegation completion already exceeds one model turn.** Async finalization parks agent results; release checks running child work, shells/completion holds, busy drivers, and cross-process scope (`Async:360–384`; `internal/agent/work_ledger_delegation.go:115–177`, `internal/agent/work_ledger_delegation.go:221–255`).
70:1. **Work-ledger jobs:** non-sync launch calls `AsyncJobStore.Claim`; kinds map agent→agent, agentic_fetch→fetch, commands→command (`Ledger:283–305`; `internal/agent/work_ledger_transition.go:131–141`).
72:3. **Stall jobs:** an outer wrapper moves eligible synchronous calls into goroutines; a stall signal returns a `stall-*` identity while execution continues. Static exclusions omit dynamically named MCP tools (`Stall:31–38`, `Stall:93–148`).
74:The durable store currently owns terminal CAS arbitration: `Transition` returns won/lost/gone, and the ledger retries errors and adopts the committed row (`internal/session/async_job_store.go:481–557`; `internal/agent/work_ledger_transition.go:244–300`). Started acknowledgment fuses message insertion and announced state; its ledger caller aborts on failure (`internal/session/notice_pull.go:340–376`; `internal/agent/work_ledger_announce.go:155–176`). Dead-host recovery probes the host lock, deletes unannounced rows, and interrupts announced running rows through the same Transition API (`internal/session/async_job_recovery.go:92–149`).
76:`job_output` tries stall control first, then resolves ledger/run_command/shell paths. Shell output concatenates stdout and stderr snapshots and uses snapshot-length cursors; wait means output growth or completion (`internal/agent/tools/job_output.go:70–145`). Stall output is final-only and ordinarily deleted after reading (`Stall:245–273`).
78:`job_kill` similarly routes stall first, then durable shell/run_command/ledger control. It can return a committed “stopped” result even when shell Kill fails (`internal/agent/tools/job_kill.go:61–105`, `internal/agent/tools/job_kill.go:130–203`). run_command terminal cancellation is committed **before** executor cancellation, without waiting for `cmd.Wait` (`Ledger:849–875`).
117:| **A4 Managed-call detach exclusion** | `turn_stall_tool_detach.go`, construction wrappers: capability/context-based exclusion, including dynamic MCP. Basis: `Stall:31–38`. | Unmanaged stall-clock behavior. | Managed calls create zero stall identities; unmanaged behavior retained. Depends A1. **S**; marker forwarding risk. |
120:| **A7 Agent scope adapter** | Extract scope observation/control from `work_ledger_delegation.go`, `coordinator_subagents.go`, `delegation_question.go`, driver/control files; wire agent/agentic_fetch adapters. Basis: scope checks at `internal/agent/work_ledger_delegation.go:221–255`. | Outcome-declaration policy, new outbox schema. | Turn-ended/descendants-live; held question with/without work; same-execution Inject; Stop/Inject race; generation cleanup. Depends A1 and new live-root reader seam. **L**; cross-process scope/debt coupling. |
135:This is capability-based, not the current coarse durable kind map (`internal/agent/work_ledger_transition.go:131–141`). Existing worker await captures job pairs and waits for disappearance; batch conversion must exclude its own invocation and enclosing execution to avoid self-dependency (`internal/agent/coordinator_await_tasks.go:111–178`; `Design:2411–2414`).
```

### `docs/research/2026-10-12-job-batches-rush-map-b.md` (17 hits)

```
13:**OBSERVED — `async_jobs`:** Its current definition is `internal/db/migrations/20261005000001_bg_shell_jobs.sql:17–46`.
33:**OBSERVED — generation:** SQLite migrations and query SQL feed sqlc; generated output includes models, prepared-query plumbing and Querier (`sqlc.yaml:1–14`; `internal/db/models.go:11–45`; `internal/db/db.go:13–60`). Removing only `AsyncJob` is insufficient.
37:**OBSERVED:** `AsyncJobStore` owns the writer, queries, lazy host registration and separate atomic read-query pointer. Its mutex is intentionally never held across writer I/O; registration uses a different mutex (`internal/session/async_job_store.go:160–196`).
42:2. **Settlement:** DB-first terminal CAS writes state, cause, result, delivery, wake and reacted together; checks running plus claim ID; preserves void. Losing writers adopt the stored winner (`internal/db/sql/async_jobs.sql:158–193`; `internal/session/async_job_store.go:474–558`).
44:4. **Notice pull:** pending+announced CAS, history insertion and notice-message identity commit in one transaction; publication follows commit. Failed pulls remain retryable (`internal/session/notice_pull.go:105–170`; `internal/db/sql/async_jobs.sql:237–259`).
45:5. **Inline-close:** result history, acknowledgement and terminal delivery consumption commit together; delivery becomes done, wake=0, reacted=1 (`internal/session/notice_pull.go:443–502`; `internal/db/sql/async_jobs.sql:261–275`).
46:6. **Reaction:** pending-inclusive launch debt and history-visible reaction debt are distinct. Both require wake=1/reacted=0; jobs additionally require announced. Visible debt requires done (`internal/db/sql/async_jobs.sql:299–346`).
64:| Launch/terminal writers | `internal/agent/work_ledger.go:226–330,454–549`; `internal/agent/work_ledger_transition.go:202–308`; `internal/agent/work_ledger_bgshell_kill.go:38–127`; `internal/agent/bgshell_claim.go:49–164` — claims, sync bypass, retrying settlement, kill and shell callbacks. Remove live DB terminal authority. |
65:| Ack/output | `internal/agent/work_ledger_announce.go:53–280`; `internal/agent/work_ledger_inline.go:173–193`; `internal/agent/work_ledger_pending.go:17–33`; `internal/agent/tools/job_output.go:70–113,202–221`; `internal/agent/tools/job_kill.go:67–209` — acknowledgement, durable fallback and controller routing. |
66:| Delegation/input/timeouts | `internal/agent/work_ledger_delegation.go:57–190,221–277`; `internal/agent/coordinator_work_scope.go:52–110`; `internal/agent/delegation_question.go:124–210`; `internal/agent/work_ledger_timeout.go:163–242`; `internal/agent/supervision.go:145–223,405` — scope termination, held input, wake-only and supervision notices. |
67:| Pull/reaction callers | `internal/agent/agent_notice_pull.go:22–124`; `internal/agent/agent_turn.go:371–381`; `internal/agent/agent_turn_step.go:113,780–807`; `internal/agent/agent_turn_failure.go:311–325`; `internal/agent/work_ledger_reaction.go:107–128`; `internal/agent/drain_attempt.go:268,382–401,479`. |
69:| Wake/maintenance/cancel | `internal/agent/coordinator_background.go:62–198`; `internal/agent/coordinator_bgshell_cap.go:58–117`; `internal/agent/coordinator_reaction_chain.go:60`; `internal/agent/coordinator_reaction_source.go:121–312`; `internal/agent/coordinator_recheck.go:80–179`; `internal/agent/coordinator_interrupt.go:57–137`; `internal/agent/coordinator_rerun.go:25`; `internal/agent/work_ledger_rerun.go:24`; `internal/session/wake_scheduler.go:172–243` — hints, maintenance, occurrence delivery and tree cancellation. |
70:| Construction/driver | `internal/agent/coordinator.go:423,459–463`; `internal/agent/coordinator_tools.go:706–716,809`; `internal/agent/work_ledger_driver.go:13`; `internal/app/app.go:370–386`; `internal/app/app_agent_setup.go:137–170`; `internal/app/app_accessors.go:50`. |
98:- The DB CAS remains live terminal authority; replacing storage is an authority change, not a schema rename (`internal/db/sql/async_jobs.sql:158–193`).
99:- A stop path writes cancelled/done/reacted before its caller completes physical termination. Do not carry that contract forward (`internal/agent/work_ledger_bgshell_kill.go:90–118`).
100:- Current sync versus claimed async branches and background escape are not universal ordinary-batch ingress (`internal/agent/work_ledger.go:226–330`; `internal/agent/bgshell_claim.go:49–119`).
144:**INFERRED merge constraint:** Additive/inert modules can merge independently. Actually dropping `async_jobs` cannot activate while old readers/writers remain; sqlc and raw audit SQL both depend on it (`internal/db/db.go:24–60`; `internal/cmd/sessions_audit_rules.go:285–340`).
```

### `docs/research/2026-10-12-job-batches-rush-map-c.md` (8 hits)

```
74:**OBSERVED — incompatible legacy stop semantics.** `job_kill` marks work stopped before physical kill and can return stopped despite kill failure. Legacy cause mapping sets delivery done/reacted and suppresses later completion. Existing store operations also have retry/backoff machinery.  
75:Anchors: `internal/agent/tools/job_kill.go:130`, `internal/agent/tools/job_kill.go:188`, `internal/agent/work_ledger_transition.go:52`, `internal/agent/work_ledger_transition.go:83`.
82:**OBSERVED — tool assembly.** Current registration includes job output/kill, inspect/inject/stop agent, awaits, wakes, and ordinary tools. MCP candidates are filtered before wrappers. Assembly applies async wrapping, restricted-run, top-level hooks, then outer stall-detach.  
105:Anchors: `internal/server/handlers_livework.go:57`, `internal/server/handlers_livework.go:82`, `internal/server/handlers_livework.go:118`, `web/src/components/LiveWorkList.tsx:86`, `web/src/components/LiveWorkList.tsx:141`, `web/src/asyncJobCompletion.ts:31`.
108:Anchors: `web/src/asyncJobCompletion.ts:52`, `docs/plans/2026-10-07-job-batches-design.md:2303`.
121:| Persistence | This is a replacement of legacy ledger consumers, not compatibility wrapping: drop async_jobs, retain host locks/session notices, use node outboxes/control requests. `docs/plans/2026-10-07-job-batches-design.md:2195` |
122:| Web | Two current state paths exist: durable flat snapshots and text-parsed transcript completions. Both require coordinated cutover. `internal/server/handlers_livework.go:82`; `web/src/asyncJobCompletion.ts:52` |
154:3. **Legacy model tool disposition:** remove job_kill/job_output or make node aliases; never retain independent control authority.  
```

### `docs/reviews/2026-07-30-full-project-review.md` (1 hits)

```
78:### 1.6 Cross-process lock конфига — защищает, deadlock'а нет, **но есть новый stall-риск**
```

### `docs/reviews/2026-09-04-sdk-library-review-round-16-1209.md` (6 hits)

```
55:явно diskless capabilities. Новый `job_output` gap ниже — ещё один пример того,
67:`job_output` получает job только по ID и возвращает stdout/stderr, command и
68:working directory (`internal/agent/tools/job_output.go:45-120`). `job_kill`
70:(`internal/agent/tools/job_kill.go:33-57`). Ни один путь не проверяет client,
73:Особенно опасна комбинация с ephemeral library mode: `job_output` и
74:`job_kill` присутствуют в общем tool catalog, отсутствуют и в
```

### `docs/reviews/2026-09-05-2047-commit-review-36h.md` (4 hits)

```
175:CR-6 (P3, поведенческое изменение без документации): sub-agent запускается с **дочерним** session ID в ctx (`internal/agent/agent_turn.go:418`). Job, запущенный из sub-agent'а, теперь недоступен родителю через `job_output`/`job_kill` — `GetOwned` вернёт «not found» (`job_output.go:65`, `job_kill.go:54`). До серии любая сессия видела любой job. Это корректная изоляция, но сценарий «worker запустил `npm run dev` в фоне и вернул ID родителю» теперь ломается молча. Ни commit message, ни описание инструментов не упоминают.
181:its own `job_output` or `job_kill` with the child's shell ID. There is no
193:P3 note: `notifyBackgroundJobDone` (`internal/agent/coordinator_background.go:53`) читает `sh.GetOutput()` в `OnDone` (`background.go:809-825`, выполняется после `close(done)`). Для job, detached'нутого **до** завершения (`KillOwned` из `job_kill`, `Close` при shutdown), буферы уже освобождены на момент callback'а → пустой summary в уведомлении. Для kill это ожидаемо; не блокирует.
349:| CR-6 | P3 | `0b25848c` | Sub-agent jobs (child session ID, `agent_turn.go:418`) недоступны родителю через `job_output`/`job_kill` — недокументированное поведенческое изменение | **Закрыт, docs: intentional per-App/session ownership** |
```

### `docs/reviews/2026-09-29-async-phase4-round1-rerun-design.md` (19 hits)

```
12:   async `call_0`, the tail has a non-async `call_0`: `VoidAsyncJobsByToolCallIDs` voids the kept row and `stopToolCallsForRerun`
28:**Migration `20260929000003_add_announce_message_id_to_async_jobs.sql`:** `ALTER TABLE async_jobs ADD COLUMN announce_message_id TEXT;` (Down: drop column; goose StatementBegin/End like the claim_id migration).
30:**SQL** (`async_jobs.sql`):
31:- `SetAsyncJobAnnounceMessageID :execrows` — `UPDATE async_jobs SET announce_message_id=?, updated_at=? WHERE owner_session_id=? AND tool_call_id=?`.
32:- `VoidAsyncJobsByAnnounceMessageIDs :many` — `UPDATE … SET delivery='void', updated_at=@updated_at WHERE owner_session_id=@owner AND announce_message_id IN (sqlc.slice('message_ids')) RETURNING tool_call_id, state, child_session_id, host_id`.
33:- `VoidAsyncJobsByToolCallIDs` becomes `:many` and only the legacy arm: add `AND announce_message_id IS NULL`, same RETURNING.
39:**Session package** (`async_job_rerun.go`, replaces `AsyncJobStore.RerunTruncate`):
42:type VoidedAsyncJob struct{ ToolCallID, State, ChildSessionID, HostID string }
43:type RerunTruncation struct{ Deleted []message.Message; Voided []VoidedAsyncJob }
48:T (one `BeginTx`, deferred Rollback): (1) `DeleteTx(owner, [target]+TailIDs)`; target not among returned rows → `ErrRerunTargetGone`, rollback. (2) From rows actually returned (target excluded) build `deletedIDs` and `tailToolCallIDs`. (3) `RependAsyncJobsByNoticeMessageIDs(deletedIDs)` (keeps `delivery='done'` guard), `RependSessionNoticesByMessageIDs`, `VoidWakeFailedNoticesByMessageIDs`. (4) `VoidAsyncJobsByAnnounceMessageIDs(deletedIDs)` then `VoidAsyncJobsByToolCallIDs(tailToolCallIDs)` (legacy arm) — voids after re-pends so void wins; collect RETURNING into `Voided`. (5) seam, commit, `publish()`. All IN-lists chunked at 500.
50:**Ack gate:** `AnnounceStarted` (`notice_pull.go`) also runs `SetAsyncJobAnnounceMessageID(msg.ID)` inside its existing tx; archived rows keep the column (fixes the id-reuse cases). **Pull-time:** in `sessionNoticeVoidCondition` wake_only returns `job.State != "running" || job.Delivery == "void"`.
52:**Agent layer:** `Coordinator` drops `RerunTruncateAsyncJobs`, adds `CancelTurn(sessionID)` (`agentFor(id).Cancel(id)` only — no job stop, no tree walk, no suspend, no wake zeroing) and `StopRerunJobs(ctx, sessionID, voided []session.VoidedAsyncJob)` (for `State=="running"` rows → `stopToolCallsForRerun` with job_kill semantics, this process's executors only, log foreign-host rows; for every row with `ChildSessionID` → `c.stopTree(child)`). `stopTree(root)` = the current body of `Cancel` extracted + nil-agent guard; `Cancel` = `c.stopTree(sessionID)`. `stopToolCallsForRerun` loses its internal `cancelTree` loop and return value.
62:**Production files:** new `20260929000003_*.sql`, `internal/message/message_delete_tx.go`; SQL `async_jobs.sql`, `session_notices.sql`, `messages.sql` + regenerated `internal/db/*`; `internal/message/message.go` (interface line); `internal/session/async_job_rerun.go`, `notice_pull.go`; `internal/agent/coordinator.go`, `coordinator_rerun.go`, `coordinator_interrupt.go`, `work_ledger_rerun.go`; `internal/server/handlers_agent_rerun.go`, `handlers_agent.go`. Test fakes to update: coordinator fakes in `server/p1_6_regression_test.go` (`cancelCalled` → `CancelTurn`), `p2_regression`, `p595_delete_streaming`, `p614_rerun_reservation`, `p623_panic_window`, `readpump_control_starvation`, `app/p1_5_shutdown_test.go`, `app/release_gate_test.go`; message-service mock in `agent/p339_no_duplicate_execution_test.go` needs `DeleteTx`.
76:**Session package** (`internal/session/session_driver.go`): `ErrDriverMarkerUnavailable`; `ErrSessionDrivenElsewhere{SessionID, HostID, PID, Status, ProbeErr}`; `SessionDriver{SessionID, HostID, PID, Status}`; `(*AsyncJobStore).ClaimSessionDriver(ctx, sessionID) error`, `ReleaseSessionDriver`, `ForeignLiveDriver(ctx, sessionID) (SessionDriver, bool, error)`, `purgeDeadSessionDrivers`.
81:**Agent layer:** `work_ledger_reaction.go`: `(l *workLedger) foreignLiveDriver(ctx, owner) (bool, error)` (nil store → false). `coordinator_drain_policy.go`: first branch of `sessionDrainPolicy` (covers `wakeSession` before submit and `decideDrainTurn` at a Drain's turn start, the latter giving "transfer only"):
83:if c.asyncJobs != nil && !c.asyncJobs.isExternalDriver(sessionID) { // own loop: skip lookup
84:    foreign, err := c.asyncJobs.foreignLiveDriver(ctx, sessionID)
89:        if debt, _ := c.asyncJobs.reactionDebtExists(ctx, sessionID); debt { c.addToRecheckSet(sessionID) }
102:**Production files:** new `20260929000004_add_session_drivers.sql`, `internal/db/sql/session_drivers.sql` + generated `internal/db/session_drivers.sql.go`, `internal/session/session_driver.go`; regenerated `internal/db/{models,querier,db}.go`; `internal/session/async_job_store.go` (`Close`), `async_job_recovery.go` (`PurgeExpired`); `internal/agent/work_ledger_reaction.go`, `coordinator_drain_policy.go`, `coordinator_reaction_source.go`; `internal/app/app_run_async.go`, `app_run_request.go`, `app_run.go`. Test fakes: `app/app_run_reviewer_scope_retry_test.go`, `app/app_run_async_wait_test.go`, `agent/coordinator_external_driver_release_test.go`, `agent/coordinator_wake_reaction_debt_test.go`.
```

### `docs/reviews/2026-09-29-async-phase4-round1.md` (27 hits)

```
20:- **A4 (P3)** `void` is not terminal: re-pend (`async_jobs.sql`, `session_notices.sql`) has no `delivery` guard, so a retried Rerun
22:- **A5 / C10 (P3)** Retention purge (`async_jobs.sql`, `session_notices.sql`, `async_job_recovery.go`, `async_job_gc.go`,
32:  `ReactionDebtExists`, `ListAsyncJobsForOwner`) run on the single writer connection (`SetMaxOpenConns(1)`); web session-list
37:  from `delivery='done'`). The comment at `async_jobs.sql:203-208` is wrong. Fix: add `(owner)` index or repeat the partial-index
43:- **A12 / C8 (P3)** On forced shutdown (`stillBusy`) `asyncJobStore.Close` still releases the host lock (`app_lifecycle.go:213-267`,
52:- **A14b — B14 (P3)** Async tool-call id reuse: unique key (owner, `tool_call_id`) now outlives the job (`work_ledger.go:280-289`,
57:- **Test fixes (A-b):** `TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing` (vacuous — never calls `store.Transition`;
78:  `childScopeDrained` (`work_ledger_delegation.go:193-214`) ignores DB debt and other-process jobs; `sessionDrainPolicy`
150:- **B9 (P3)** `close()` suppresses natural completions racing shutdown: `work_ledger.go:813-815` sets `shutdownCancelled` on every job incl. ones whose
151:  executor already returned; `work_ledger_transition.go:232-238,253-258` stops retry on `closedCh`. Row stays `running`, next host says "interrupted, output
153:- **B10 (P3)** Stop between Claim and the "started" ack orphans the row (`work_ledger_transition.go:324-331` drops the `stoppedBySession` job without
156:- **B11 (P3)** `job_kill` reports "stopped" after losing the race to natural finish (`work_ledger.go:605-627,703-732`): the natural finish committed with
158:  the bash path (`job_kill.go:110-129` still calls `KillOwned`). Fix: `commitTransition` returns the committed outcome; tool answers from the committed row.
159:- **B15 (P3)** Ack gate holes: `ErrAsyncJobGone` rolls back the tool-result insert (`notice_pull.go:324-326`, `work_ledger_announce.go:49-51`) leaving a `tool_use` with
164:- **A3 (P2)** `job_kill` delivers a result without recording which message carried it: row `done, reacted=1` has no `notice_message_id` (`work_ledger_transition.go:107-109`,
165:  `async_jobs.sql:114-119,130-131`; only writer is the pull) — Rerun after a `job_kill` cannot re-pend it. Fix: law "`delivery='done'` ⇒ the row names the message carrying
166:  the result": write the `job_kill` tool-result message id into `notice_message_id` in the same tx that persists the tool result (pattern of `AnnounceStarted`).
167:- **B-dev9** `wake_only` for sync jobs still produces a session notice and a wake (`work_ledger_timeout.go:195-226`, no `sync` check).
168:- **Tests:** `TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed` (finish before close — race untested), `TestWorkLedger_StartFailsClosedWhenStoreUnavailable` (`executorStarted`
169:  vacuous), `TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes` (checks memory callback, not DB `wake`), `TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome`
170:  (tool answer vs committed outcome), `TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction` (passes with two writes),
171:  `TestPullJobNotices_JobKillProducesExactlyOneStoppedNotice` (stale: pins step-3 contract; production is `causeJobKill` → done/reacted=1). Missing: ack-gate `ErrAsyncJobGone`
180:- **C7 (P2)** The "external driver" marker is per process (`work_ledger_reaction.go:105-135`, `coordinator_wake.go:63-83`): a web process W runs a full paid Drain turn on a CLI
193:  `TestExplainSessionStatus_AsyncJobsAndDebtSection` (no pending-debt case), `TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts` (cited as DUR-9 proof but performs no Stop).
201:  (`coordinator_drain_policy.go:131`), `sessionDrainPolicy` "by construction", `killRequested` (`work_job.go:156-160`), `wire.go` `isHumanTyped` (`wakeNoticeCall` removed), `UpdateTx` doc ("nil func"), `async_jobs.sql:203-208`.
204:  writers, `job_kill` is a third. Design-doc deviations to re-state: `ScopeOpen` `HostNotDead` uses exclusive `ProbeHost` for a read-only decision; `async_hosts.label` documented `'cli'|'web'` but code writes `"app"`; supervision "moved notice → `reacted=1`" half not implemented.
212:Drain calls (no empty user row, `ErrEmptyPrompt` lifted only for Drains, 401 rebuild keeps `IsDrain`, hidden from queue, never durable); `job_kill` single CAS; Stop `cancelTree` (BFS + seen set); ack gate fused tx; Start fails closed; shutdown plumbing selects on `closedCh`;
```

### `docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md` (7 hits)

```
61:Gate: fields on `sessionJobs` (`work_ledger.go`, under `l.mu`, next to `hintSeq`), methods in `work_ledger_reaction.go`; they replace `noTurnDrainRelease`, `noTurnDrainHintSeq`, `admissionRefusedRelease`.
64:func (l *workLedger) drainGateOpen(owner string, now time.Time) (open, dormant bool, retryAt time.Time)
65:func (l *workLedger) paceDrainGate(owner string, hintAt uint64, wait time.Duration, hintOpens, unreacted bool)
66:func (l *workLedger) resetDrainGate(owner string)
152:New `internal/agent/drain_attempt.go`. Changed: `coordinator_wake.go` (delete probe, snapshot, evidence, `recordDrainOutcome`/`checkStuckDrainProgress` calls, cap bump, `wakeSessionAttemptSeam`, `jobIdentity`), `coordinator_drain_policy.go` (`drainPolicy`/`drainPermitted`/`drainDecision`; delete `counted`, capped branch, `persistentMode` fail-open, `isPseudoJobID`, `recordDrainOutcome`, `settleOrRetryDrainFailure`, `incrementThenSettleIfThreshold`, `checkStuckDrainProgress`, `errDrainProgressNotRecorded`; `settleAndMark`→`settleDrainDebt`), `work_ledger.go`+`work_ledger_reaction.go` (gate; delete `noTurnDrainRelease`, `noTurnDrainHintSeq`, `admissionRefusedRelease`, their mark/consume methods, `visibleReactionDebtExists`), `agent_run.go` (per-iteration `att`, `afterTurn`, `noteDrainRefused` on every pre-loop error; delete admission marker, B6 block), `agent_turn.go` (`hintAt` before pull, snapshot after, `decideDrainTurn(snap)`, attempted flag before `Stream`, close before both `drainOrReleaseMerged` calls, R2B-15; delete `onDrainTurnStarting` call and no-turn marker), `agent_drain_decision.go`, `agent_drain.go`, `agent.go` (delete `onDrainTurnStarting`), `agent_notice_pull.go` (drop `anyWake`), `agent_turn_step.go` (persist before caps; `capAbort`), `agent_turn_failure.go` (awaiting-answer reaction write; `att.stalled`), `agent_turn_stream.go` (`att` in `turnStreamConfig`), `supervision.go` (hook = `go afterRelease`; debt-check error → recheck set; delete marker consumption and the external-driver hint bump), `coordinator_recheck.go`, `coordinator_reaction_source.go` (new `CLIScopeState`; ticker start in `ClaimExternalDriver`; `WaitForHint(until)`; delete `ScopeOpen`, `CaptureDrainSnapshot`, `RecordDrainTurnOutcome`), `coordinator_background.go`, `work_ledger_timeout.go`, `agent_prompt.go` (new `wakeSession` signature; delete cap ctx keys), `coordinator_turn_admission.go` (delete `markReachedProvider`/`didReachProvider`), `coordinator_run.go` (Drain = one attempt; refusal wrap), `work_ledger_delegation.go` (`recheckChild` early return; child scope open iff `WorkOpen`/`Owed`/`Paced`), `coordinator_work_scope.go` (question text), `coordinator_subagents.go` (lift child suspension), `coordinator.go` (`turnHolds`, `HoldAutomaticTurns`; human reset also resets the gate), `internal/app/app_run_async.go` (state-driven loop, `loopTotals`, refusal budget, pre-checks; delete `waitAfterNoTurnDrain`, snapshot/record calls), `app_run.go`+`app_run_request.go` (`drainTurn`), `app_run_gates.go` (`CLIScope` gate + stderr), `internal/server/handlers_agent_rerun.go` (hold; async `StopRerunJobs`). All files stay <1000 lines; net production code shrinks.
153:Composition with store2 (their new API shape): `CaptureDebtSnapshot`, `IncrementWakeAttempts`, `MaxWakeAttempts`, `SettleReactedFailedWithMarker` keyed by `(tool_call_id, claim_id)` + `delivery='done'` + `notice_message_id`, `DebtSnapshot.Empty()`, and a way to list the snapshot's tool_call_ids for the marker text; snapshot treated as opaque. Unchanged store APIs used: `ReactionDebtExists`, `PendingInclusiveDebtSummary`, `MarkReactedWithMessageUpdate`, `HasRunningDelegationFor`, `ListAsyncJobsForOwner`, `ListReactedFailedText`, `ForeignLiveDriver`, `ClaimSessionDriver`. ledger2: no API; only the head of `recheckChild` overlaps (their R2B-12 makes my guard return earlier — no conflict).
156:Faults via SQLite triggers, not code seams: `BEFORE UPDATE OF reacted ON async_jobs WHEN NEW.reacted=1 AND NEW.reacted_failed=0 BEGIN SELECT RAISE(ABORT,'x'); END` blocks real reactions only; the same without the `reacted_failed` clause also blocks settles; a `delivery` trigger (pending→done) blocks pulls. Shrink `drainRetryAfterFailure` and `recheckPassIntervalNS` per test.
```

### `docs/reviews/2026-09-30-async-phase4-round2.md` (12 hits)

```
33:- **R2A-8 (P3)** DUR-11 does not hold for `job_kill`: `causeJobKill` commits `done, reacted=1` first; `notice_message_id` is written later by
36:  `notice_message_id` whose cause is job_kill (delivery='pending', reacted=0, wake=0), and the ledger/tool path re-pends when the fused write does not
46:  2N queries per re-poll. Fix: route to the read pool and add one batched reader `ListRunningAsyncJobsForOwners(ids)` + per-call host cache; use it in
50:  (no fault injection: inject a marker-insert failure and prove the settle rolled back too; fix garbled revert-check text), `TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing`
59:  R2 with job2's claim_id. Fix: the executor carries its own `*asyncJob`/claim_id into `finish`/`armDelegation`; skip when `s.jobs[toolCallID] != job`. Test through the real
60:  `Start`→abort→`Start`(same id)→late `finish` sequence (the existing `TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure` models an executor that cannot exist).
65:- **R2B-11 (P3)** `job_kill` wins `MarkJobStopped` (`done,reacted=1`) but a concurrent Stop/timeout/`close()` cancels the job first, `KillOwned` removes the shell, `GetOwned` fails →
69:- **R2B-13 (P3)** a synchronous (SDK) `terminate_and_wake` timeout no longer yields a timed-out outcome (`work_ledger_timeout.go` + transition skip for sync jobs): caller gets the executor's "context canceled" instead
73:- **R2B-20 (P3)** Rerun stop records Stop semantics (`session_cancel`, `stoppedBySession`) but docs (`work_ledger_rerun.go`, `StopRerunJobs`) say "job_kill semantics": fix the docs (or the cause) so `sessions jobs` shows what is documented; pick one and align CHANGELOG/§3.8.
74:- **R2A-8 (tool side)** see W2-STORE: the ledger/`agent_turn_stream.go` path must re-pend the job_kill row when the fused `AnnounceJobKillResult` does not happen (error result, ctx cancel, tx failure).
75:- **Vacuous tests:** `TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure` (see R2A-3), `TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout` (no timeout armed), `TestWorkLedger_AckGate_ErrAsyncJobGone…` (comment says Rerun deletes the row; `TruncateForRerun` only voids; real trigger is session delete — fix the comment/trigger).
113:Claim tx (archive+conflict+insert+child reacted_failed clear atomic); Transition CAS with `claim_id`, empty-claim resolution, void preserved; recovery asserts `row.ClaimID` and holds the lock; void final in every writer; retention predicates identical for purge/count and never delete pending or done/unreacted debt; `TruncateForRerun` (one BEGIN IMMEDIATE tx, no nested connection use, sets from RETURNING rows, target excluded, all re-pends before voids across chunks, publish after commit, cancel point before tx); generated SQL matches; migrations 01–04 additive with symmetric Up/Down; `session_drivers` idempotent claim/takeover CAS/owner-only release/Close vs CloseKeepLock/purge after dead probe; `CloseKeepLock` pin; probes read-only; read-pool ordering (WAL); debt snapshot done-only; `MarkReactedWithMessageUpdate` one tx; depth-cap `walkIncomplete`. Agent: no DB I/O under `l.mu`, lock order `l.mu`→`sr.mu`, callbacks after commit, `closedCh` bounds retries, shutdown latch (B9), drop-at-announce (B10), job_kill verdicts (B11), sync wake_only skip, Drain construction/merge rules, B13 marker, B1 detach, CancelAll reaching drivers, `CLIScope` order and policy split, `HostNotDead` shared probe. App/cmd/server: Rerun handler ordering/panic unwind, driver claim before any session write and deferred release, CLI loop no-turn detection/lock-busy pacing/DB-error bound/cancel-after-clean-turn, shutdown order, `sessions gc`/`why`/`list`/`jobs` reads, additive wire fields, 190 cited test names exist.
```

### `docs/reviews/2026-09-30-async-phase4-round3.md` (13 hits)

```
6:## W3-STORE — durable core (agent `store3`, files: `internal/session/**`, `internal/db/**`, `internal/agent/work_ledger_announce.go`, `ClaimExternalDriver` hunk in `coordinator_reaction_source.go`)
13:- **R3A-2 (P3)** `AnnounceJobKillResult`/`SetAsyncJobNoticeMessageIDIfDone`/`RependJobKillRowWithoutNotice` look the row up by `tool_call_id` TEXT
14:  (`notice_pull.go:380-404`, `async_jobs.sql:46-55,246-261`, `work_ledger_announce.go:246-249`). With per-response call numbering: a new `call_0` `Claim` archives the just-killed
15:  row J1 (`call_0#reused#u`) while `job_kill` still runs, so the fused write matches the NEW row J2 (0 rows → treated as success, no re-pend); J1 stays `done` with NULL
18:  reused key through the real `job_kill` path. Fix the `AnnounceJobKillResult` doc (0 rows now means the row changed after the win).
27:  deadline check that fails) or correct the comment; T3 no test that a DB error propagates from the claim (covered by R3A-1); T4 no `job_kill` test with a reused key (covered by R3A-2).
29:  (plan ~line 586) "answers the repeated call idempotently" is false — `workLedger.Start` returns "was already started earlier; not starting it again" / the false "already running with different input" — state the
32:## W3-AGENT — Drain accounting, ledger edges (agent `agent3`, files: `internal/agent/**` except `work_ledger_announce.go`; NOT `internal/session`, `internal/app`)
40:  `IsError = FinishReasonError`; `question_stop.go:89-102`; `work_ledger_delegation.go:140-142` (the refresh always runs and overwrites `runSubAgent`'s question response, `coordinator_subagents.go:350-353`) → parent gets "Async job … failed"
46:- **R3B-4 (P3)** one dormancy `streak` mixes pull failures and paid failures (`work_ledger_reaction.go:158-183`, `drain_attempt.go:182-183,196`): 1 pull failure + 2 provider 503s = streak 3 → dormant with `hintOpens=false`; new facts get `drainStuck` and are DROPPED (not even in the recheck set), the row (2<K) is never reacted/closed, no marker; a child reports Stuck → delegation released with stale text; a CLI root exits "stuck". Fix: separate counters, or let a new fact reopen a dormant gate unless the close write failed; decide + document + test.
52:- **R3C-5 (P3, agent side)** a web rerun on a delegated child releases the parent's delegation with stale text: `coordinator_drain_policy.go:79-81` maps a held session to `drainDeferred`, and `childScopeDrained` (`work_ledger_delegation.go:262`) keeps the scope open only for `Owed`/`Paced` → during the rerun's hold the child looks drained, the delegation completes with stale/cancelled text. Fix: a rerun hold is TEMPORARY — map it to an "open/paced" state (or recheck), not `Deferred`.
54:- **Docs (D-B, agent side; you may edit only these comments/registry lines, the docs agent handles the rest):** `coordinator_recheck.go:1-11,102-108,141-144` and `coordinator.go:336-338,540-546` (no ticker in `rush run`, `turnAttemptRefused` branch that no longer exists); `coordinator.go:241-247` `StopRerunJobs` "job_kill semantics" → Stop semantics; `supervision.go:193-195,311-313` (`recordProgress` now from `pullPendingNotices`; `pushDeadlineOnTurnEnd` from `afterTurn`); `coordinator_tools.go:674-675`; `DrainStuck`/`drainStuck` doc comments + Warn text at `drain_attempt.go:345` (a stuck/dormant gate reopens only on a human message after a paid failure/failed close — after your R3B-4 decision, make text match); `async_job_reaction.go` `CaptureDebtSnapshot` docs are store3's.
72:R2A-1 (every remover through `RemoveDeadHostFile` after the exclusive lock; POSIX/Windows reasoning; retries capped at 16; no id reaches a DB row before a verified lock); R2A-2 (no lock-order inversion; `regMu` only around registration; `s.mu` leaf; host id captured before `BeginTx`); R2A-4/5/6/7/8 SQL side, R2A-9/10/12 (`LiveWorkForRoots` equals per-root readers); all 155 queries consistent between SQL and generated code; pump clock seam production-neutral; ledger executor identity (`currentLocked` checks), tagged ack gate, `job_kill` result/re-pend, `failLaunch`, sync `transitionSync`, gone-delegation retirement; accounting exactly-once (`closed`), snapshot after pull, hint before pull, `COALESCE` guards, step persisted before cap/peak checks, `stalled`/`capAbort` flags; launch decision inputs fail closed; refusals typed and never counted; `CLIScope` order and CLI exits (Paced/Deferred/Stuck/DB-error bound); mutation-free Drain guards except R3C-6; driver claim before any session write and detached release on every exit incl. panics; Rerun handler hold/idempotent release/detached `StopRerunJobs`; batched web list and additive wire fields; forced shutdown docs+test; `sessions jobs` hints; 238 cited test names exist.
```

### `docs/reviews/2026-09-30-async-phase4-round4.md` (5 hits)

```
14:- **R4A-3 (P3, comments)** `async_job_recovery.go:54,197-201` and `internal/db/sql/async_jobs.sql:59-62` (mirrored in generated `async_jobs.sql.go:253`, `querier.go:139`): `removeDeadHostFile` says that when another holder has the lock "it removes it", but a reader's shared lock also reads as alive and a reader removes nothing;
17:- **R4A-4 (P3, comments/docs)** the listed reasons for the fused `job_kill` write affecting 0 rows include impossible causes: "a Rerun re-pended it" (`RependAsyncJobsByNoticeMessageIDs` matches on `notice_message_id IN (...)`, an unnamed row's NULL never matches), "another writer named it" (nothing else names a `done` job_kill row), DUR-11's "a lost race left it pending" (`KilledClaimID` is emitted only on `commitWon`).
18:  The one reachable cause is deletion (session-delete cascade). Fix `notice_pull.go:377-381`, `async_jobs.sql:268-272` (+ generated copies), DUR-11 and the two stale test comments in `async_job_kill_notice_test.go:32,91` that still name the removed `SetAsyncJobNoticeMessageIDIfDone`; describe the guard as defensive.
20:## W4-AGENT (agent `agent4`; files: `internal/agent/**` except `work_ledger_announce.go` and the `ClaimExternalDriver` hunk; docs rows ASYNC-09, ASYNC-02, DUR-4 and the (x) amendment)
41:  the reviewer turn carries OriginCLI and keeps bash (`buildReviewerPassTurn` only disables sub-agents). Scenario: reviewer configured; `rush run --role smart --json "fix X"`; first turn and Drains end clean → `stepExit` → `exitClosed` runs the reviewer; the reviewer calls `bash {"command":"go test ./..."}`, gets "Async bash job … started …" and ends its turn "Tests are running; I will assess once they finish."; `exitClosed:401-402` makes that the run's answer, exit 0, `final_text` not a verdict; the driver marker is released and `a.Shutdown()` → `asyncJobs.close()` cancels the job (row stays `running`, later recovered `interrupted`; a job that finished after the reviewer's last step leaves an Owed notice with no driver).
```

### `docs/reviews/2026-09-30-async-phase4-round5.md` (7 hits)

```
9:- **R5A-1 (P3)** `internal/db/sql/async_jobs.sql:439-452` (and the matching count at `:457-470`): the retention purge can delete a `job_kill` row whose result was never delivered, so dead-host recovery (R2A-8, `RependJobKillRowsWithoutNoticeForHost`) has nothing left to re-pend. The purge treats every `delivery='done'` row as delivered history; a `job_kill` row that is `done` with `notice_message_id IS NULL` is exactly the state recovery repairs, and its `wake=0, reacted=1` slips past the debt guard.
10:  Scenario A (long-lived web process): web host HW runs bash job J; the model calls `job_kill`, the Transition commits J as `cancelled`, `delivery=done`, `reacted=1`, `wake=0`, `notice_message_id` NULL, `updated_at=T`; another process holds the SQLite write lock > 60s (a big `sessions purge`, VACUUM, disk-full), `AnnounceJobKillResult` fails after busy_timeout and `rependJobKill` gives up after 30s; J stays done+unnamed on a live host (recovery skips live hosts); at T+7d HW's own `RunMaintenanceSweep` → `PurgeExpired` → `PurgeAsyncJobsOlderThan` deletes J; after a restart `RecoverDeadHost(HW)` re-pends 0 rows; J's output never reaches the session.
11:  Scenario B (crash then gc): a `rush run` is force-killed between job_kill's Transition commit and the fused write; > 1h later, before any run/web pass, `rush sessions gc --jobs-older-than 1h` (which registers no host and sweeps nothing) deletes J.
12:  Fix: add `AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)` to `PurgeAsyncJobsOlderThan` and `CountAsyncJobsOlderThan` (same predicate as the third arm of `ListDistinctRecoverableHostIDs`); regenerate sqlc v1.30.0; mention it in DUR-11, DUR-4's "Retention never purges" sentence and plan (g). Test `TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery`: seed a running announced row on `fabricateDeadHost`, `Transition` it with `NoticeKind:"job_kill"`, `Delivery:"done"`, `Reacted:true`, backdate `updated_at`; the row survives `PurgeJobsOlderThan(1h)` and `PurgeExpired`, and `RecoverDeadHost` then reports `Repended==1`. Revert-check: drop the clause.
15:- **R5A-3 (P3, SQL doc/dead code)** `internal/db/sql/async_jobs.sql:280-284`, `:12-14`: `VoidPendingAsyncJobNotice` has zero production and zero test callers, and its doc describes a pull-time "task still running" void for job rows that nothing performs (only `session_notices` has that rule; its `delivery='pending'` guard is the one `VoidPendingSessionNotice`'s own doc explains can never match after the pull CAS) — delete it (regenerate sqlc), or correct doc and guard if a caller exists. `ListAsyncHosts` is documented as the "Reader for `sessions jobs`/`sessions hosts`" but has no production caller and no `sessions hosts` command exists — correct the doc (tests may still use it). Do NOT edit any shipped migration file (comments included; migrations are new-files-only): the two wrong column comments in `20260928000002…sql` (`label` documented as `'cli'|'web'` while both `NewAsyncJobStore` call sites pass `"app"`, `origin_cli` documented as "recovery routing" while it only stamps the notice message's origin) get corrected in the DUR row / plan text (say what the columns really are) instead.
17:## W5-AGENT (agent `agent5`; files `internal/agent/**` except `work_ledger_announce.go` and the `ClaimExternalDriver` hunk; docs rows ASYNC-09, ASYNC-02, DUR-4 cap sentences, plan (aa) annotations + one new section "Amendments after review round 5 (agent, 2026-09-30)" (ad); CHANGELOG cap bullets)
36:- **R5C-5 (P3)** `internal/cmd/sessions_why.go:251-285`, `sessions_list.go` status passes, `internal/server/handlers_sessions.go:257`: no status surface reads the durable driver marker (`session_drivers`; `ForeignLiveDriver` is used only in `work_ledger_driver.go`), so a session whose live `rush run` loop is between turns with debt pending is reported "at rest — not running", contradicting ASYNC-02 (that scope is open). Scenario: a `rush run` job finishes and the Drain fails with a provider 5xx after in-step retries: counted attempt, gate paced 60s, no session lock between turns, no running row; during those 60s `rush sessions why <id>` prints "status: at rest / reason: no lock file present — not running, not crashed" plus "reaction debt: pending"; `sessions list` shows a blank STATUS and the web sidebar shows the session idle; the process is alive, retries at `RetryAt`, and a second `rush run --session <id>` is refused with "already driven by another `rush run`".
```

### `docs/reviews/2026-09-30-async-phase4-round6.md` (3 hits)

```
15:- **R6A-3 (P3, comments)** retention comments in the session package contradict the code: `async_job_recovery.go:36-39` (`AsyncDataRetentionAge` doc) and `:326-328` say `rush run` purges "once at loop start" — in fact `ClaimExternalDriver` starts the recheck ticker and every 60s `RecheckPass` → `RunMaintenanceSweep` → `PurgeExpired` runs (the `sessions gc` help was fixed for exactly this in R3C D1); `async_job_gc.go:7-9` says "a terminal row's host no longer matters once it is terminal" — false since R5A-1 for an unnamed job_kill row (kept until its host is dead and recovery re-pends it). Also fix the two leftovers in area C that only need a comment (tell `app6` nothing; you own these two one-line comment edits): `internal/app/app_agent_setup.go:146` still cites a `sessions hosts` command that does not exist; `internal/app/app.go:304` says RegisterHost happens only on the store's first Claim, while `ClaimSessionDriver` registers too (DUR-5).
17:## W6-AGENT (agent `agent6`; files `internal/agent/**` except `work_ledger_announce.go` and the `ClaimExternalDriver` hunk; docs: ASYNC-02/09, DUR-4 rows you touch, plan (aa)/(ad) annotations + one new section "Amendments after review round 6 (agent, 2026-09-30)" (ag); CHANGELOG bullets)
19:- **R6B-1 (P1, pre-existing, found now)** `internal/agent/coordinator_background.go:130-163` with `coordinator_drain_policy.go:98-104` and `work_ledger_delegation.go:262`: a background-shell completion in a delegated CHILD whose delegation row is still `running` never starts a Drain, yet the child's scope reads `DrainOwed`, so the delegation stays parked forever.
```

### `docs/reviews/2026-09-30-async-phase4-round7.md` (6 hits)

```
7:## W7-STORE (agent `store7`; files `internal/session/**`, `internal/db/**`, plus one comment fix in `internal/agent/work_ledger_announce.go` only if needed; the `fromDBItem` change belongs here (R7C-4); docs: DUR rows you touch + one new plan section "Amendments after review round 7 (store, 2026-09-30)" (aj))
12:- **R7A-2 (P3, comments)** `internal/session/async_job_store.go:46-49`, `:72-73`, `:560-563`; `internal/session/notice_pull.go:319-321`; `internal/db/sql/async_jobs.sql:129-130` (and generated copies `async_jobs.sql.go:944`, `querier.go:579`); `internal/session/session.go:191-193` describe behaviour the code no longer has: `ErrAsyncChildSessionBusy` says "every conflict is a refusal in this step — dead-host recovery … is step 5/6, not here" while `Claim`, 250 lines below, recovers a dead host's conflicting row and retries once (its own doc says so); `ErrAsyncJobGone`, `MarkAnnounced`, `AnnounceStarted` and the `MarkAsyncJobAnnounced` SQL give "a Rerun truncation raced it / a Rerun that removed it" as the cause of a missing row, but since the round-1 Rerun redesign `TruncateForRerun` only UPDATEs `async_jobs` (void/re-pend) and never deletes — the one reachable cause is the owner session's cascade delete (`work_ledger_announce.go`'s B15 comment already says so); the `IncrementCost` interface doc still names `coordinator.updateParentSessionCost` as a caller relying on `IncrementCost(id, 0)`, but that function now calls `TransferChildCostToParent`, which runs the parent UPDATE itself. Reword all (the `.sql` comment needs sqlc v1.30.0 regeneration; editing only generated files is wrong).
16:## W7-AGENT (agent `agent7`; files `internal/agent/**` except `work_ledger_announce.go`, `internal/shell/**` only if truly needed (name it in the report); docs: ASYNC-02/09, DUR-4 rows you touch, plan (ag) annotations + one new section "Amendments after review round 7 (agent, 2026-09-30)" (ak); CHANGELOG bullets)
18:- **R7B-1 (P2, pre-existing, found now)** `internal/agent/work_ledger_delegation.go:223`: `childScopeDrained` treats a child's background shell as finished once the shell stops running (`internal/shell/background.go:462`, before `close(done)`); the shell's `bg_shell_done` notice is inserted later, in `notifyBackgroundJobDone` (`coordinator_background.go:131`). A delegation re-check that lands between those two moments releases the delegation with stale text and the shell's result is never reacted to.
21:- **R7B-2 (P3)** `coordinator_interrupt.go:66-89` with `work_ledger.go:384-397` and `work_ledger_delegation.go:128-130`: when a delegation is stopped while its child is parked on the child's own async job, the child is never re-checked, so the child's driver record and its permission allowlist entry leak and `agentFor(child)` keeps routing to the idle task agent. Scenario: in the web R delegates to C; C's first turn starts async bash J and yields (delegation parked on J); the operator presses Stop on R (a Rerun that voids the delegation, or Stop on C directly, is the same); `cancelSession(R)` delivers the cancelled delegation to R, which triggers `notifyAsyncCompletion(R)` (re-checks R, not C); `cancelSession(C)` marks J stopped-by-session, `deliverLocked` drops J silently with no completion callback, J's executor then calls `finish`, which does nothing because J is no longer current; `agentFor(C).Cancel(C)` hits an idle mailbox, so no release hook fires; `releaseDriverIfScopeClosed` is reachable only from `recheckChild` and nothing calls `recheckChild(C)` (C is not parked: `gcChildLocked` removed the terminal job; no completion or release); `subAgentDrivers.byChild[C]` and the allowlist entry live for the whole life of the web process (one leak per stopped delegation); side effects: `noteWorkStarted` still treats C as a delegated child, so jobs started by a later human turn on C get no supervision; if the operator later continues C in its tab, that turn runs on `currentAgent` (`coordinator_run.go:365`), Stop on C then goes through `agentFor(C)` to the idle driver, so `Cancel` does nothing (the turn keeps running while the UI broadcasts busy=false) and `InjectMessage` lands in the driver's idle mailbox instead of the running turn, until that turn's `runInternal` defer re-checks C and releases the driver.
23:- **R7B-3 (P3, doc)** plan (ag) item 1, "Цепочка" (`docs/plans/2026-09-28-async-phase4-durable-core.md` ~line 878): the plan says a child's chain of bg-shell wakes is bounded by "root supervision, Stop, tool timeouts". Root supervision cannot bound it: `job_kill` on a delegation returns `DelegationJobError` (`work_ledger.go:602-605`); the check-in text itself says "sub-agent control tools are not available yet"; the child's notices never count as progress for the root (`recordProgress` is keyed by the pulling session, `noteWorkStarted` skips children), so root supervision pauses after 6 check-ins (~3 h 15 min of backoff: 5+10+20+40+60+60 min); loop detection is per turn, so the same command repeated across separate Drains never trips it. Scenario: web default config; the child polls CI with `sleep 90; gh run view` in every Drain: a paid Drain roughly every 90 s with no automatic end, while the root gets at most 6 paid check-ins it cannot act on. The real bounds are Stop, the delegation's own `timeout` (terminate_and_wake) if the model set one, and in the CLI the 6 h default cap.
```

### `docs/reviews/2026-09-30-async-phase4-round8.md` (4 hits)

```
12:- **R8A-3 (P3)** `internal/cmd/sessions_reset.go:92` (store side: nothing like `TruncateForRerun` exists for a full wipe): `sessions reset` deletes every message but leaves the session's `async_jobs`/`session_notices` rows alone. The next turn's start pull (`internal/agent/agent_turn.go:366-379`) injects old notices into the "clean slate", and `reset --force` does not look at the driver marker.
17:## W8-AGENT (agent `agent8`; files `internal/agent/**` except `work_ledger_announce.go`, `internal/shell/**`, `internal/cmd/run_deadline.go` (the ErrRunDefaultCap one-liner only); docs: ASYNC-02/09, DUR rows you touch, plan (ak) annotations + section "Amendments after review round 8 (agent, 2026-09-30)" (an); CHANGELOG bullets)
22:- **R8B-2 (P2)** `internal/shell/background_completion.go:73`: the completion hold of a shell that was KILLED but whose process never exits lasts forever: `completedAt` stays 0, so `completionHoldMax` never starts, and the shell is already out of the job table. Scenario: child C (delegation parked or running) runs `bash "git fetch"` with ssh `ControlPersist`, or `adb devices` (starts the adb server); the persistent descendant inherits the stdout/stderr pipe and escapes the process group/tree; the direct child exits but `cmd.Wait` blocks on pipe EOF; after 60 s the call moves to the background and `OnDone` takes the hold; C's model calls `job_kill`; `KillOwned` takes the shell out of the table and cancels it, but the tree kill misses the descendant (`taskkill /T` targets the dead direct pid; `kill(-pgid)` misses a `setsid` child); `done` never closes (`background.go:544-551` documents this residual); C's turn ends; `recheckChild(C)` sees `ActiveOwned=0` and `PendingCompletionsOwned=1` forever: the delegation parks forever (in the CLI until the 6 h cap; before R7B-1 this step released it); after a Stop the driver record and allowlist entry cannot be released either (the R7B-2 symptom returns); (ak).1 says "the hold does not leak … `done` closes on Kill/Close, and a callback with nothing to call yet belongs to a running process that `ActiveOwned` counts" — false after Kill.
24:- **R8B-4 (P3)** `internal/agent/work_ledger.go:879` + `tools/bash.go:205` + `async_tool.go:312`: R7B-2 still leaks the child's driver when Stop lands within the async bash executor's fixed 1 s fast-failure sleep. Scenario: C's turn starts async `bash` J; the executor's inner bash calls `StartOwned(context.Background())` then an uninterruptible `time.Sleep(1s)`; during that second the shell sits in the table as running, independent of J's ctx; within that second Stop on R or C (or a Rerun) arrives: `cancelSession(C)` makes J terminal and drops it (already announced) and cancels its ctx; `stopTree`'s re-check finds C busy; C's cancelled turn ends and `afterRelease` calls `recheckChild(C)`: `l.running` is false but `ActiveOwned(C)=1` (the executor is still sleeping) so C is not drained; after the sleep `awaitShell` sees the ctx done and `KillOwned` takes the shell; `finish(job)` returns early because `!currentLocked`, with no re-check; the shell has no `OnDone` (`WithoutBackgroundCallback`) and the 60 s pass only walks armed delegations: the driver record and allowlist entry live as long as the process — contradicting (ak).2 ("a child with a running turn is released by that turn's end").
```

### `docs/reviews/2026-10-05-async-round9.md` (4 hits)

```
41:- **B9-5** `agent/async_tool.go:85` + `delegation_question.go:168` — a web parent's (or nested parent's) reaction to `child_question` runs as a sync Drain turn, so `answerHeldDelegation` (`!sync`) does not trigger; the resume runs as a sync delegation in parallel with the held one: the parent turn blocks in `awaitSync`, the result arrives twice. The #1157 tests cover `OriginCLI` only.
49:- B9-3 `bgshell_claim.go:82-104` `finishBGShellRow` retries `sql.ErrNoRows` forever on `context.Background()` (goroutine leak when the session is deleted; a test pins it); B9-6 `work_ledger_delegation.go:236` detached shell's hold keeps `childScopeDrained=false` for up to 10 min after `job_kill`; B9-7 `coordinator_background.go:131` vs `work_ledger_bgshell_kill.go:59` natural exit racing `job_kill` can deliver both "stopped" and "finished"; B9-8 `bgshell_claim.go:49` shell id counter restarts after a crash → Existing adopted from a dead host; B9-9 `bgshell_claim.go:70-74` observer does not call `noteSubAgentChildRunEnded` (delegation freed by the 60 s pass); B9-10 `fs_write.go:255` N items on one path shrink a file to (1/4)^N, below 2048 B the next item can empty it; B9-11 `agent_run.go:542-551` WS-1 refusal → recheck set (foreign session retried every 60 s).
89:| B9-6 / B9-7 `job_kill` hold and race with the natural exit | #1219 | `9f4a0a0d` |
140:- `job_kill` holds `bgArrival` for up to its 10 s budget while retrying the transition;
```

### `docs/reviews/2026-10-08-job-batches-xxs-rush-integration-round2.md` (12 hits)

```
25:| **I1** | Агент удерживает общий executable slot и ждёт child command, которому этот же budget не даёт стартовать; lineage/accounting независимых roots не выбран. | **partially fixed** | §3.2:511–519, §8:1117,1125–1126, §9.2:1201–1210 выбирают механизм: waiting levels в разных группах, global не меньше суммы их limits; child launches учитываются по группе, не structural ancestors agent leaf; один existing leaf не запускается второй раз. Отсутствие рекурсивного `agent` у worker действительно enforced: `coordinator_tools.go:633–653`, независимо от AllowedTools; `workerToolNames` объясняет guard в строках 176–198. Delegation действительно живёт дольше первого turn: `async_tool.go:375–384`, `work_ledger_delegation.go:115–177,221–282`. Но §2:138 и §2.1:299–305 разрешают default/unbounded competitors; §3.2:472–479 не резервирует capacity для waiting levels. Поэтому опубликованное арифметическое условие само по себе недостаточно — контрпример ниже. Требование прежнего отчёта прогрессировать именно при общем limit=1 больше не обязательно: выбран другой корректный механизм, такой config должен отвергаться. Выбранный group-accounting вместо наследования agent ancestor cap также не переиздаётся как отдельный дефект. |
26:| **I2** | Runner acceptance order и durable CAS независимо выбирают terminal winner; прямое ctx cancellation может опередить запись причины. | **partially fixed** | §9.2:1212–1223 выбирает **durable ledger CAS** и порядок «durable cause → cancel»; planned additive `Settled.Authoritative`, default false, разрешает committed natural outcome победить ранее latched leaf cause. Это соответствует текущим `work_ledger_transition.go:202–254,296–306` и `async_job_store.go:474–557`: loser adopts committed row, claim участвует в CAS. Контракт не требует SQLite в A0, а лаборатория сохраняет §3.3 acceptance order. Однако flag описан для **leaf**, а §3.1:416–419 оставляет независимую latched cause у batch. Target обычного singleton Stop и согласование singleton root с authoritative leaf ещё не определены: root может остаться Cancelled при единственном Completed leaf и committed Completed job. Остаток ниже — не повтор прежней проблемы выбора leaf winner, а незамкнутый consumer bridge. |
27:| **I3** | Post-commit terminal callback, даже вместе с возвратом asyncTool.run, ошибочно принимается за физическое termination. | **fixed** | §9.1:1151–1159 явно разделяет committed outcome/cause и actual termination, сохраняет completion source в handle после удаления job/shell, ждёт **оба** факта и защищает claim identity. §9.2:1196–1199 дополнительно удерживает admission до real exit; §§3.1.1:444–460 и 3.4:574–582 не разрешают synthetic Settled. Текущий stop действительно commit-ит до cancel (`work_ledger.go:849–865`, `work_ledger_delegation.go:371–406`); delivery удаляет map entry (`work_ledger.go:377–439`), а late finish может уже не найти её (`898–912`). `async_tool.go:409–416` игнорирует bounded kill error; `background.go:608–624` удаляет shell до ожидания done. Независимые реальные источники существуют: `background_shell.go:234–245`, `run_command.go:212–221`; delegation имеет scope gate, не первый Run. План теперь прямо запрещает использовать эти ранние proxy как confirmation. Это исправление дизайна, не заявление, что новые seams уже реализованы. |
28:| **I4** | Внешний stall-detach wrapper может повторно владеть MCP operation и создавать `stall-*` job рядом с batch leaf. | **fixed** | §9.1:1160–1169 выбирает policy-only construction из pinned filtered set, исключает повторный batch ingress **и stall-detach**, в том числе dynamic MCP names; job controls адресуют исходное исполнение, unmigrated stall policy сохраняется. Текущая опасная граница подтверждена: `coordinator_tools.go:771–824` применяет async, restricted-run, hooks, затем stall wrapper; `async_tool.go:521–531` сейчас не wrap-ит MCP; `turn_stall_tool_detach.go:31–38,93–140,171–184` имеет name-based exclusions и отдельный registry/ID. `job_kill.go:61–65` сначала проверяет stall controller. Исправление требует исключить lifecycle wrapper, не снять permissions/hooks; это прямо записано. Отсутствие нового exclusion в ещё не мигрированном коде не переиздаётся как открытая находка. |
29:| **I5** | Lifetime root ID, построенный только из owner/tool_call_id, конфликтует с idempotent retries и повторными incarnations. | **fixed** | §9.3:1275–1283 разделяет logical call, durable claim UUID, root/node и Runner token; active matching retry ищет существующий root **до Submit**, mismatched retry rejected, новая incarnation получает новый root, sync без DB claim — local incarnation. Текущий ledger уже присоединяет matching active call (`work_ledger.go:264–269,314–328`, `async_tool.go:142–155`); Store архивирует history и mint-ит новый UUID (`async_job_store.go:352–398,426–431`). Stale transition защищён claim (`work_ledger_transition.go:244–254,274–289`), ack — claim tag (`work_ledger_announce.go:53–87`), отображаемый старый tool ID отделён от archived key (`async_job_store.go:690–709`). Planned mapping сохраняет эти разные identities и не делает launch token cross-process claim. |
31:| **I7** | Ack-before-effects смешивает pre-dispatch reservation с completion delivery; внутренним leaves явного batch неоткуда получить собственные tool_result acks. | **fixed** | §9.2:1233–1244 отдельно задаёт reservation до dispatch и transactional ack до public completion, разрешает finish-before-ack; outer root ack покрывает execution records, leaves explicit batch_run не создают собственных tool results/notices; singleton/aggregate/inline имеют одного delivery owner. §9.3:1294–1305 закрепляет transactional root delivery, не Events feed. Это согласуется с текущими claim-before-launch (`work_ledger.go:283–305`, `async_tool.go:121–155,203–216`), gate (`work_ledger.go:377–379,491–502`), fused ack (`work_ledger_announce.go:92–111,155–186`) и inline election (`work_ledger_inline.go:79–127`). Notice message и delivery CAS уже атомарны (`notice_pull.go:129–164`). Новые root rows, propagation ack на Add и durable representation ещё предстоит реализовать; выбранные reservation/ack/ownership invariants достаточны, их отсутствие в текущей схеме не является повтором I7. |
48:Разделение agents/commands устраняет исходный двухгрупповой тупик, но не этот allowed forest. Current Rush caps также не становятся частью нового global budget автоматически: `maxAsyncJobsPerSession=50` (`work_ledger.go:24`), admission проверяет одного owner (`271–276`), held delegations считаются nonterminal (`work_ledger_cap.go:45–59`). §9.2:1260–1265 правильно откладывает migration accounting; это не доказательство достаточности автономной формулы.
67:4. По неизменённому §3.1 R имеет собственную latched cause и становится Cancelled, с completed count=1. `Batch.Wait`/root `batch_status`/root notice отличаются от committed job outcome. Текущий ledger control, проигравший CAS, отвечает winner-ом (`work_ledger.go:862–875`, `job_kill.go:135–144`).
73:**Acceptance:** barrier после completed CAS и real termination, но до Runner report; затем обычный singleton stop и timeout/caller cancellation. Ledger, leaf, singleton Wait/status/consumer result показывают один execution winner, без второй completion. Отдельный explicit multi-leaf root stop сохраняет Cancelled aggregate и ранее Completed leaves. Во время delayed durable cause write legacy ctx ещё не отменён; после записи отмена и два observation seams приводят к одному terminal result. SDK memory-only path, разрешённый §9.3:1282–1283 и существующий в `work_ledger.go:283–305,335–336` / `work_ledger_transition.go:367–388`, должен иметь явно выбранную локальную authority, а не ожидать несуществующий DB commit.
106:- **Observation seams — не два владельца.** Committed CAS state и actual exit наблюдаются независимо; adapter handle связывает их одной incarnation. Текущий `childScopeDrained` учитывает owner jobs, shells/completion holds, driver busy и CLIScope/debt (`work_ledger_delegation.go:221–282`), поэтому первый child turn не заменяет release. При integration этот scope должен видеть retained physically-live descendants согласно I3, а не просто переименовать старый post-commit callback. Нужны boundary tests, не второй terminal writer/finished-execution registry.
109:- **Reservation, ack, retention, recovery — разные задачи.** §9.2:1233–1244 корректно меняет root delivery ownership; current failed-ack path ещё abort-ит legacy job (`work_ledger_announce.go:171–177`, `work_ledger.go:514–546`), поэтому будущая durable root reservation должна сохранять controllable identity отдельно от успеха consumer ack. Это уже acceptance I7, не новый дефект плана. Как именно Add inherits committed root ack и где хранится envelope, можно решить при migration, сохранив заявленные invariants.
111:- **Shutdown не user stop; wake_only не termination.** §9.3:1289–1292 и §9.2:1246–1249 сохраняют различия. Текущий ledger shutdown suppresses terminal write и leaves running rows для recovery (`work_ledger.go:956–997`, `work_ledger_transition.go:233–241`); future reconciliation live Runner Interrupted с durable dead-host recovery — интеграционная работа. Как и cross-process item control из §9.4:1314–1323, это не повод расширять автономный Runner до daemon или повторно использовать session-wide cancel для leaf.
```

### `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md` (19 hits)

```
23:**Текущий контракт:** `async_tool.go:375–384` передаёт возврат агентского turn в `armDelegation`, а не завершает delegation. `work_ledger_delegation.go:115–120, 160–177, 221–258` удерживает её до закрытия собственных jobs ребёнка, background shells, занятого driver и межпроцессного scope/debt. Существующий admission проверяет jobs отдельного owner (`work_ledger.go:263–276`; `work_ledger_cap.go:45–59`), поэтому job родителя и jobs child session не занимают один общий глобальный пул.
39:**Текущий контракт:** `work_ledger_transition.go:202–215, 244–254, 296–306` принимает результат DB CAS и усваивает именно состояние победившей строки. `AsyncJobStore.Transition` также возвращает committed winner при проигрыше (`async_job_store.go:474–480, 519–557`). `StopRunCommandJob` сначала делает snapshot/terminal commit и лишь затем вызывает cancel (`work_ledger.go:799–804, 849–875`); проигравший stop возвращает результат победителя. Существующий `job_kill` не выдаёт отмену за победившую, если row уже завершилась естественно (`tools/job_kill.go:135–144`).
43:Обратная проблема появляется при прямой передаче Start context в существующий executor: §3.3 разрешает отменять его после принятия Stop, тогда как legacy path требует «snapshot → commit причины → cancel». Если контекст отменить раньше durable stop-записи, `inner.Run` может вернуть cancellation error, а `asyncTool.finalize`/`finish` успеют committed natural `failed` с wake=1 (`async_tool.go:342–346, 375–384`; `work_ledger_transition.go:93–109`) до intended `session_cancel` с wake=0. Post-commit observer сам по себе не исправляет уже выбранного победителя.
45:**Минимальная корректировка:** назвать одну точку линеаризации интегрированного исполнения и описать мост для всех Stop/job_kill/timeout/SDK-cancel путей. При сохранении нынешнего durable CAS адаптер должен согласовывать принятие Runner intent/report с committed winner и не отменять legacy execution context до записи предназначенной причины. Альтернатива — явно утверждённый cutover всех legacy terminal writers на новую единую authority, а не два независимых правила «кто первый». Нужен cause-aware control/admission seam; одного терминального callback недостаточно. Это решение интеграционной фазы, а не изменение автономной детерминированности.
57:- `StopRunCommandJob` committed terminal row до cancel (`work_ledger.go:849–875`). `stopTargets` делает terminal transition, затем cancel (`work_ledger_delegation.go:371–406`).
58:- `deliverLocked` может удалить terminal job из owner map (`work_ledger.go:377–395, 410–439`). После этого поздний `finish` проходит ветку `!currentLocked` и не даёт нового terminal event (`work_ledger.go:902–920`). Таким образом, единственный post-commit seam может сработать слишком рано, а второго ledger terminal transition уже не будет.
68:### 4. P2 — нынешний stall-detach wrapper остаётся отдельным владельцем jobs и не исключает MCP
74:**Текущий код:** MCP tools уже попадают в filtered slice, но `wrapAsyncTools` оборачивает только bash/run_command/agent/agentic_fetch (`coordinator_tools.go:783–809`; `async_tool.go:521–531`). После restricted-run и hooks поверх всех tools устанавливается `wrapToolsWithStallDetach` (`coordinator_tools.go:815–824`). Его исключения перечислены по именам и не включают MCP (`turn_stall_tool_detach.go:26–45`). При срабатывании policy он сохраняет execution в `stallDetachJobs` и возвращает `stall-<callID>` (`turn_stall_tool_detach.go:101–140, 171–184`). `job_kill` сначала проверяет именно этот controller (`tools/job_kill.go:61–65`).
76:**Отказ `[INFERENCE]`:** если новый MCP ingress поставить в нынешнюю точку async wrapping, а остальные wrappers просто сохранить, долгий synchronous MCP singleton может быть повторно detached внешним stall wrapper. Появятся Runner root и `stall-*` job для одной операции, два control namespace и изменившаяся связь SDK caller cancellation с root. Если policy-wrapped outer tool использовать как executor leaf, возврат stall wrapper может быть ошибочно принят за окончание MCP call, пока inner Run продолжает работу.
80:**Acceptance:** вызвать stall policy во время SDK MCP singleton и во время явного MCP leaf: нет второго job/root, ложного Settled или утраты caller Stop. `job_output`/`job_kill` и batch control адресуют одно исполнение; PreToolUse и restricted-run check выполняются по предусмотренному пути без двойного запуска.
88:**Текущий код:** текущий ключ `(owner, toolCallID)` не является вечной execution identity. `Start` присоединяет повторную активную call только при совпадении tool/input (`work_ledger.go:264–269, 314–328`), а `AsyncJobStore.claimOnce` архивирует историю под другим key и допускает новую call с тем же provider ID (`async_job_store.go:352–398`). Fresh claim получает новый UUID (`async_job_store.go:426–432`). Executor-side CAS сравнивает claim (`work_ledger_transition.go:244–254, 274–289`), а claimAck проверяет tagged result (`work_ledger_announce.go:53–87`).
116:**Текущий порядок:** non-sync durable claim предшествует launch (`work_ledger.go:283–305`; `async_tool.go:121–155, 203–216`), но executor уже работает до сохранения started response. Ack gate запрещает delivery terminal job, пока нет announced (`work_ledger.go:372–379`). Tool result и announced committed вместе (`work_ledger_announce.go:92–111, 155–186`; `session/notice_pull.go:332–379`). Inline path намеренно ждёт natural terminal commit до ack, затем объединяет outcome message и delivery=done (`work_ledger_inline.go:79–89, 105–127`; `session/notice_pull.go:450–505`).
118:**Двусмысленность `[INFERENCE]`:** буквальное чтение «root/leaf ack gate and durable claim precede externally visible effects» в строке 666 как требования *получить ack до исполнения* ломает inline и противоречит acceptance «immediate finish before ack». Другое чтение — независимый существующий ack для каждой внутренней leaf — тоже не определено: у явного `batch_run` есть одна outer tool call, а не собственная tool_use/tool_result для каждого объявленного task. Нынешний claimAck ищет job по `result.ToolCallID` (`work_ledger_announce.go:61–70`); такой запуск не даст leaves их обычных tagged acknowledgements. Просто ждать их означает orphan announced=0, а создавать искусственные leaf tool results — дублировать consumer protocol.
131:| User Stop, CancelTurn, host shutdown | `coordinator_interrupt.go:41–51, 57–115, 119–137` разделяет отмену generation, полного дерева и остановку host. `work_ledger.go:956–997` на shutdown не делает user terminal transition; `commitTransition` подавляет shutdown-caused write (`work_ledger_transition.go:233–241`). Forced shutdown удерживает host lock (`async_job_store.go:631–653`), recovery позднее выставляет Interrupted только доказанно мёртвому host (`async_job_recovery.go:92–149`). §9.3, строки 692–700, справедливо требует сохранить эту разницу. Нельзя вести Runner.Close через legacy user-stop путь или считать `context.Canceled` достаточным кодом причины; live Runner Interrupted и durable recovery row требуют явного согласования, а не синтетического post-commit event. |
135:| Root/leaf caps и legacy work во время миграции | §9.2, строки 680–683, верно требует учитывать work вне batch и сигнал освобождения. В текущем ledger cap=50 на owner и в счётчик входят nonterminal held delegations/timers, но не terminal-awaiting-ack (`work_ledger.go:24, 271–276`; `work_ledger_cap.go:45–59`). Запись orchestration root не является executable slot. До миграции выбрать соответствие owner/kind/group и источник CapacityAvailable для legacy work; не ждать только settlement в том же Runner. Это дополняется замечанием 1 о зависимостях и замечанием 3 о physical termination, а не заменяется ими. |
136:| Cross-process control | §9.4, строки 704–712 и 718–721, честно выделяет отдельную Rush работу. Методы Runner по своему контракту локальны; запуск второго Runner в операторском CLI не управляет handle первого. Сейчас `workLedger` хранит handles в памяти, а `rush sessions cancel` пишет session-wide request (`session/session_update.go:309–339`; `cmd/sessions_cancel.go:74–101`). Такой request нельзя безусловно переиспользовать для остановки leaf: он остановит sibling work той же session. Нужны durable owner/root/item/claim routing и request/ack исполнения на host, владеющем handle, включая idle detached roots. Реализация транспорта не является автономным prerequisite. Acceptance: второй процесс останавливает один вложенный leaf и не затрагивает sibling; Add/Inject доходят до настоящего owner, stale control не действует на новую claim. |
137:| Durable notices и Web Drain | В §1.3, строки 89–92, и §9.3, строки 697–700, корректно сказано, что Events/callback не являются outbox. Нынешний `notifyAsyncCompletion` — только nonblocking wake hint после commit (`coordinator_background.go:62–92`), а history message и delivery CAS объединяются в pull transaction (`session/notice_pull.go:105–160`). Root aggregate должен использовать такой же durable delivery ownership, не отправляться непосредственно из bounded Events feed и не запускать отдельный billable turn для каждого suppressed leaf. Внешний CLI driver получает hint, не конкурирующий Drain (`work_ledger.go:95–100`; `coordinator_background.go:95–101`). WebUI hierarchy/control проверяются позднее в браузере; batchlab этого не доказывает. |
138:| Timeout intent | Автономный `TaskSpec.Timeout` означает termination и подтверждённый TimedOut (§5, строки 426–429). Нынешний Rush `wake_only` не завершает работу, а создаёт отдельный check-in (`work_ledger_timeout.go:211–249`); terminate_and_wake сначала записывает причину и затем отменяет executor (строки 170–205). Адаптер не должен отображать wake_only в engine DeadlineExpired. Таймеры legacy и Runner нужно согласовать для одного execution, не включать два независимых termination deadline по неявному правилу. |
157:Статически прочитан весь план и релевантные реализации named async/ledger/delegation/MCP/store путей, а также вызванные ими construction, held-question, stall-detach, shell completion, notice transaction и session cancel границы. Старые offsets не использовались. Файлы существующих отчётов, конфигурация и credentials не изучались; код не менялся.
```

### `internal/agent/agent.go` (4 hits)

```
76:	// job_output, so this only catches truly stuck tools (hung MCP tools,
77:	// blocking job_output --wait on a deadlocked process, or a sub-agent
594:	asyncJobs          *workLedger
853:		asyncJobs:                  opts.AsyncJobs,
```

### `internal/agent/agent_drain_decision.go` (7 hits)

```
25:	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
34:	v, _, err := a.asyncJobs.coord.arbiterVerdict(ctx, sessionID, siteCommit)
45:		a.asyncJobs.coord.clearSleepAll(sessionID)
52:	if a.asyncJobs == nil {
55:	return a.asyncJobs.captureDebtSnapshot(ctx, sessionID)
62:	if a.asyncJobs == nil {
65:	debt, err := a.asyncJobs.reactionDebtExists(ctx, sessionID)
```

### `internal/agent/agent_notice_pull.go` (9 hits)

```
3:// holding the mailbox and the session-lock -- moves pending async_jobs/
23:	if a.asyncJobs == nil || a.asyncJobs.store == nil || sessionID == "" {
26:	store := a.asyncJobs.store
32:	// claims[i] is the async_jobs claim id of pulled[i] ("" for a
54:	coord := a.asyncJobs.coord
62:		a.asyncJobs.recordProgress(sessionID)
74:// buildJobNoticeMessageParams converts a pulled async_jobs row into the
86:		Stopped:        row.NoticeKind == "job_kill",
107:// work_ledger_timeout.go/coordinator_background.go) -- no second formatter.
```

### `internal/agent/agent_options.go` (1 hits)

```
27:	AsyncJobs            *workLedger
```

### `internal/agent/agent_timeouts_test.go` (1 hits)

```
127:	assert.Equal(t, streamStalledFinishTitle, title, "watchdogFinishMessage's idle-stall title must match coordinator.go's streamStalledFinishTitle exactly, or transparent stall-retry silently stops matching")
```

### `internal/agent/agent_turn.go` (3 hits)

```
370:	if a.asyncJobs != nil && a.asyncJobs.store != nil {
371:		a.asyncJobs.store.RecoverOwnerScope(preambleCtx, call.SessionID, a.messages)
375:	// async_jobs/session_notices rows into history BEFORE history loads for
```

### `internal/agent/agent_turn_failure.go` (2 hits)

```
317:	if awaiting && hasReactionContent(snap) && ts.a.asyncJobs != nil && ts.a.asyncJobs.store != nil {
318:		err := ts.a.asyncJobs.store.MarkReactedWithMessageUpdate(ctx, ts.a.messages, ts.call.SessionID, snap)
```

### `internal/agent/agent_turn_step.go` (10 hits)

```
320:	ownWork := async && ts.a.asyncJobs != nil && ts.a.asyncJobs.running(ts.call.SessionID)
396:// friends, a bash/run_command call with substance, a delegation, job_kill)
429:		if meta.Async && meta.ClaimID != "" && isCommand && class != stepCallRefused && ts.a != nil && ts.a.asyncJobs != nil && ts.a.asyncJobs.coord != nil {
430:			repeated = ts.a.asyncJobs.coord.arb.chainLastLaunch(ts.call.SessionID, command)
452:			if ts.a != nil && ts.a.asyncJobs != nil && ts.a.asyncJobs.coord != nil {
453:				ts.a.asyncJobs.coord.arb.chainLastLaunch(ts.call.SessionID, "")
676:		if l := ts.a.asyncJobs; l == nil || !l.isExternalDriver(ts.call.SessionID) {
772:// wake=1/reacted=0/delivery='done' row of this session, across async_jobs
784:	if stepIsReaction(snap) && ts.a.asyncJobs != nil && ts.a.asyncJobs.store != nil {
785:		return ts.a.asyncJobs.store.MarkReactedWithMessageUpdate(ts.genCtx, ts.a.messages, ts.call.SessionID, snap)
```

### `internal/agent/agent_turn_step_test.go` (2 hits)

```
38:		return fantasyStepFinishFixture{name: "job_output", input: `{"id":"j1"}`, output: "running"}
82:	require.Contains(t, fin.Message, "job_output", "the loop-detected finish text must name the repeated tool")
```

### `internal/agent/agent_turn_stream.go` (3 hits)

```
496:	// Ack gate (DUR-7, doc sec.3.8) and job_kill result fusion (A3): the
502:	if ts.a.asyncJobs != nil {
503:		return ts.a.asyncJobs.persistToolResult(ts.ctx, sessionID, toolResult, ts.a.messages, params)
```

### `internal/agent/agent_turn_stream_retry_test.go` (1 hits)

```
32:	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
```

### `internal/agent/agent_turn_stream_tool_suggest_test.go` (2 hits)

```
21:// sessionAgent with real messages and real tools but asyncJobs nil, so every
33:	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
```

### `internal/agent/async_tool.go` (30 hits)

```
54:	return t.coordinator != nil && t.coordinator.asyncJobs != nil &&
55:		t.coordinator.asyncJobs.inlineWindow > 0 && inlineWindowApplies(t.name)
83:	if t.name == AgentToolName && t.coordinator != nil && t.coordinator.asyncJobs != nil {
86:			if resp, answered := t.coordinator.asyncJobs.answerHeldDelegation(ctx, sessionID, childSessionID, params.Prompt); answered {
121:	job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
170:func (t *asyncTool) launchExecutor(ctx, jobCtx context.Context, cancel context.CancelFunc, sessionID, childSessionID string, call fantasy.ToolCall, sync bool, job *asyncJob) (resp fantasy.ToolResponse, err error) {
210:		t.coordinator.asyncJobs.noteWorkStarted(ctx, sessionID)
224:func (t *asyncTool) launchInline(ctx context.Context, job *asyncJob, childSessionID, jobID string) (fantasy.ToolResponse, error) {
225:	result, inline := t.coordinator.asyncJobs.awaitInline(ctx, job)
227:		t.coordinator.asyncJobs.noteWorkStarted(ctx, job.owner)
242:// awaitAndFinish blocks until job's sync outcome is ready (workLedger.
243:// awaitSync) and returns it as this call's own ToolResponse -- byte-for-byte
247:func (t *asyncTool) awaitAndFinish(ctx context.Context, job *asyncJob) (fantasy.ToolResponse, error) {
248:	result, err := t.coordinator.asyncJobs.awaitSync(ctx, job)
296:func (t *asyncTool) run(ctx context.Context, cancel context.CancelFunc, job *asyncJob, sessionID, childSessionID string, call fantasy.ToolCall, sync bool) {
335:		// ledger as soon as run_command.go's process starts, so job_output
336:		// can read progressive output and job_kill's stopped notice can
339:			t.coordinator.asyncJobs.setRunCommandBuffer(job, buf)
364:// (see work_ledger_delegation.go) and released exactly once when the
375:func (t *asyncTool) finalize(job *asyncJob, childSessionID string, completion AsyncCompletion) {
376:	if t.coordinator == nil || t.coordinator.asyncJobs == nil {
381:		t.coordinator.asyncJobs.armDelegation(job, result)
384:	t.coordinator.asyncJobs.finish(job, result)
387:func (t *asyncTool) awaitShell(ctx context.Context, job *asyncJob, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
393:	// so job_kill/job_output can resolve the job id the model saw to it.
396:	t.coordinator.asyncJobs.setShellID(job, metadata.ShellID)
522:	if c.asyncJobs == nil {
539:func (t *asyncTool) failLaunch(job *asyncJob, msg string) {
540:	if t.coordinator == nil || t.coordinator.asyncJobs == nil {
543:	t.coordinator.asyncJobs.finish(job, jobResult{content: msg, isError: true})
```

### `internal/agent/async_tool_sync_test.go` (14 hits)

```
33:	registry := newWorkLedger(nil)
34:	registry.store = newTestAsyncJobStore(t)
35:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
87:	registry := newWorkLedger(nil)
88:	registry.store = newTestAsyncJobStore(t)
89:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
119:	registry := newWorkLedger(func(AsyncCompletion) { webDoneCalls++ })
120:	registry.store = newTestAsyncJobStore(t)
123:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
171:	// happen via awaitSync's own ctx.Done() branch even if the executor
175:	// caller-facing wait unblocking via awaitSync, while the executor itself
187:	registry := newWorkLedger(nil)
188:	registry.store = newTestAsyncJobStore(t)
189:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
```

### `internal/agent/async_tool_test.go` (14 hits)

```
32:	// one (work_ledger.go's deliverLocked), so this test observes it the
35:	registry := newWorkLedger(func(c AsyncCompletion) { completed <- c })
36:	registry.store = newTestAsyncJobStore(t)
37:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
64:// coverage beyond workLedger.Start alone (TestWorkLedger_StartFailsClosedWhenStoreUnavailable,
65:// work_ledger_durable_test.go, whose own "executorStarted" bool is actually
78:	registry := newWorkLedger(nil)
80:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "bash"}
118:	registry := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
119:	registry.store = newTestAsyncJobStore(t)
121:		asyncJobs: registry, permissions: panickyPermissions{}, sessions: stubChildSessions{},
204:	registry := newWorkLedger(func(result AsyncCompletion) { completed <- result })
205:	registry.store = newTestAsyncJobStore(t)
206:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
```

### `internal/agent/batch_allowed_controls.go` (2 hits)

```
29:	if present["job_output"] {
32:	if present["job_kill"] {
```

### `internal/agent/batch_exec_agent_doc.go` (3 hits)

```
18:| Finalization parks results; release checks child work, shells, holds (work_ledger_delegation) | Scope closes only when the turn ended AND no question is held AND descendants are confirmed terminal; report after closure, exactly once. |
19:| Live child work discovered via ledger scans that tolerate lag (work_ledger_delegation) | Injected batchAgentDescendantReader over R10a; incomplete or unknown reads look live (conservative-read rule) and keep the scope open. |
22:| job_kill/run cancel commit "stopped" before physical termination | Removed per plan correction 3: Stop records intent, cancels the run context (the addressed subtree at depth) and returns the turn acknowledgement; only confirmed closure settles (Cancelled). Siblings are separate executions and untouched. |
```

### `internal/agent/batch_exec_generic_test.go` (9 hits)

```
327:			key := stallDetachKey(t.Name(), "call")
328:			defer stallDetachPending.Delete(key)
329:			defer stallDetachJobs.Delete(key)
337:				resp, _ := (&stallDetacherTool{inner: tool}).Run(ctx, fantasy.ToolCall{ID: "call"})
344:				require.Equal(t, "stall-call", id)
369:		key := stallDetachKey(tools.GetSessionFromContext(ctx), c.ID)
370:		if _, ok := stallDetachPending.Load(key); ok {
373:		if _, ok := stallDetachJobs.Load(key); ok {
378:	_, reports := batchGenericTestStart(t, batchGenericTestExecutor(&stallDetacherTool{inner: tool}), batchKindMCP, tool.name)
```

### `internal/agent/batch_install_inventory_test.go` (3 hits)

```
235:		`SELECT COUNT(*) FROM async_jobs WHERE owner_session_id = ?1`, e.sessionID).Scan(&jobs); err != nil {
236:		e.t.Fatalf("%s: count async_jobs: %v", toolName, err)
239:		e.t.Fatalf("%s: %d async_jobs rows for the owner session, want 0 (ONE authority, no legacy ledger)", toolName, jobs)
```

### `internal/agent/batch_install_tools.go` (3 hits)

```
22:// every family (todos, job_output, rush_info, ...) keep their direct pinned
33://   - job_output, job_kill: (i) D1-ADJACENT LEGACY CONTROLS — the legacy
36://     never batch-wrapped. Anchor: the legacy workLedger
```

### `internal/agent/batch_install_tools_test.go` (5 hits)

```
164:		if _, ok := seen[name].(*stallDetacherTool); ok {
203:// hooks/restrictions, stall-detach outermost, registry leaves stripped of
206:	c := &coordinator{asyncJobs: newWorkLedger(nil)}
207:	c.asyncJobs.inlineWindow = inlineWindow
219:		if _, ok := byName[name].(*stallDetacherTool); !ok {
```

### `internal/agent/batch_model_flag_test.go` (11 hits)

```
15:	t.Setenv("RUSH_BATCH_RUNTIME", "false")
17:	c := &coordinator{asyncJobs: newWorkLedger(nil)}
18:	c.asyncJobs.inlineWindow = inlineWindow
19:	filtered := batchTestTools("bash", "view", "job_output")
25:	legacy := batchTestTools("bash", "view", "job_output")
55:	filtered := batchTestTools("view", "job_output", "job_kill")
63:	require.Equal(t, []string{"batch_status", "batch_output"}, batchControlAvailability(batchTestTools("job_output")))
64:	require.Equal(t, []string{"batch_stop"}, batchControlAvailability(batchTestTools("job_kill")))
83:	t.Setenv("RUSH_BATCH_RUNTIME", "true")
84:	c := &coordinator{asyncJobs: newWorkLedger(nil)}
94:	tools := batchTestTools("bash", "job_output", "job_kill")
```

### `internal/agent/bgshell_claim.go` (7 hits)

```
50:// async_jobs row (R-BG-1, docs/plans/2026-10-01-bg-shell-ledger.md). The
70:func (t *asyncTool) claimBackgroundShellRow(job *asyncJob, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
71:	if t.coordinator == nil || t.coordinator.asyncJobs == nil || t.coordinator.asyncJobs.store == nil {
78:	store := t.coordinator.asyncJobs.store
123:	if t.coordinator == nil || t.coordinator.asyncJobs == nil || t.coordinator.asyncJobs.store == nil {
126:	store := t.coordinator.asyncJobs.store
128:	if err := t.coordinator.asyncJobs.retryAsyncStoreOp(ctx, func() error {
```

### `internal/agent/bgshell_claim_test.go` (39 hits)

```
39:	registry := newWorkLedger(nil)
40:	registry.store = newTestAsyncJobStore(t)
41:	wrapped := &asyncTool{inner: bgShellInnerTool("sh-1"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
65:	store := newTestAsyncJobStore(t)
67:	// held by a row with a DIFFERENT input (ErrAsyncJobInputMismatch).
72:	registry := newWorkLedger(nil)
74:	wrapped := &asyncTool{inner: bgShellInnerTool("sh-x"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
92:	store := newTestAsyncJobStore(t)
93:	registry := newWorkLedger(nil)
95:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
119:	store := newTestAsyncJobStore(t)
120:	registry := newWorkLedger(nil)
122:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
139:	store := newTestAsyncJobStore(t)
140:	registry := newWorkLedger(nil)
142:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
164:	store := newTestAsyncJobStore(t)
165:	registry := newWorkLedger(nil)
167:	c := &coordinator{asyncJobs: registry}
188:	store := newTestAsyncJobStore(t)
189:	registry := newWorkLedger(nil)
191:	c := &coordinator{asyncJobs: registry}
215:func newBGShellFixture(t *testing.T) (*coordinator, *session.AsyncJobStore, *shell.BackgroundShellManager, string) {
217:	store := newTestAsyncJobStore(t)
218:	registry := newWorkLedger(nil)
222:	return &coordinator{asyncJobs: registry, background: mgr}, store, mgr, t.TempDir()
332:	store := newTestAsyncJobStore(t)
333:	registry := newWorkLedger(nil)
335:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
370:	store := newTestAsyncJobStore(t)
371:	registry := newWorkLedger(nil)
373:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
398:	store := newTestAsyncJobStore(t)
399:	registry := newWorkLedger(nil)
401:	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
433:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
434:	foreign := session.NewAsyncJobStore(conn, dataDir, 12345, "other-app")
436:	registry := newWorkLedger(nil)
440:	coord := &coordinator{asyncJobs: registry, background: mgr}
```

### `internal/agent/bgshell_observer_recheck_test.go` (10 hits)

```
41:	coord.asyncJobs = newWorkLedger(func(c AsyncCompletion) { delivered <- c })
42:	coord.asyncJobs.store = newTestAsyncJobStore(t)
43:	coord.asyncJobs.coord = coord
58:		AsyncJobs:  coord.asyncJobs,
93:	_, _, err = coord.asyncJobs.Start(parentSession, parentCall, "", AgentToolName, childSession, false, false, nil, func() {})
95:	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, parentSession, parentCall))
96:	coord.asyncJobs.armDelegation(jobOf(coord.asyncJobs, parentSession, parentCall), jobResult{content: "child yielded: shell still running"})
125:	require.False(t, coord.asyncJobs.hasParked())
126:	row, err := coord.asyncJobs.store.Get(context.Background(), parentSession, parentCall)
129:	shellRow, err := coord.asyncJobs.store.Get(context.Background(), childSession, sh.ID)
```

### `internal/agent/cli_active_work.go` (7 hits)

```
25:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
28:	c.asyncJobs.store.RecoverOwnerScope(ctx, root, c.messages)
30:	tree, treeIncomplete := c.asyncJobs.store.JobsInTree(ctx, root)
46:	jobs, incomplete := c.asyncJobs.store.LiveJobs(ctx, root)
81:			c.asyncJobs.recheckChild(job.ChildSessionID)
82:			if c.asyncJobs.heldQuestionJob(job.ChildSessionID) != nil {
83:				notices, err := c.asyncJobs.store.ListSessionNotices(ctx, job.SessionID)
```

### `internal/agent/cliscope_completion_hold_test.go` (1 hits)

```
3:// observer (bgshell_claim.go) commits the terminal async_jobs row, and
```

### `internal/agent/coordinator.go` (13 hits)

```
247:	// not job_kill's stopped notice), and every voided delegation's child tree
252:	StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob)
280:	// asyncJobs is the single in-memory work ledger: plain async jobs
282:	// state machine and one mutex. See work_ledger.go/work_ledger_delegation.go.
283:	asyncJobs *workLedger
427:// session.NewAsyncJobStore over its own writer *sql.DB and data dir.
438:	asyncStore *session.AsyncJobStore,
474:	c.asyncJobs = newWorkLedger(c.notifyAsyncCompletion)
475:	c.asyncJobs.store = asyncStore
476:	c.asyncJobs.coord = c
477:	c.asyncJobs.timeouts = newTimeoutService(c.asyncJobs)
478:	c.asyncJobs.supervision = newSupervisionRegistry()
482:	c.asyncJobs.inlineWindow = inlineWindow
```

### `internal/agent/coordinator_agent_control.go` (13 hits)

```
45:	if c.asyncJobs == nil {
48:	c.asyncJobs.mu.Lock()
49:	defer c.asyncJobs.mu.Unlock()
50:	entries := c.asyncJobs.byChild[childID]
66:	if c.asyncJobs == nil {
69:	c.asyncJobs.mu.Lock()
70:	defer c.asyncJobs.mu.Unlock()
71:	if s := c.asyncJobs.bySession[childID]; s != nil {
78:	for _, job := range c.asyncJobs.byChild[childID] {
158:	if c.asyncJobs == nil {
166:	c.asyncJobs.mu.Lock()
168:	if s := c.asyncJobs.bySession[callerSessionID]; s != nil {
175:	c.asyncJobs.mu.Unlock()
```

### `internal/agent/coordinator_agent_control_test.go` (14 hits)

```
125:	coord.asyncJobs = newWorkLedger(nil)
126:	coord.asyncJobs.store = newTestAsyncJobStore(t)
127:	coord.asyncJobs.coord = coord
146:// Callers set coord.asyncJobs.onWebDone themselves.
147:func armDelegation(t *testing.T, coord *coordinator, parentID, childID string) *asyncJob {
149:	job, _, err := coord.asyncJobs.Start(parentID, "call-"+childID, "{}", "agent", childID, true, false, nil, func() {})
153:	coord.asyncJobs.mu.Lock()
155:	coord.asyncJobs.mu.Unlock()
157:	coord.asyncJobs.armDelegation(job, jobResult{content: "in progress"})
242:	coord.asyncJobs.mu.Lock()
243:	coord.asyncJobs.byChild[child2][0].transitionToTerminal(phaseCancelled, jobResult{content: "sub-agent canceled", isError: true})
244:	coord.asyncJobs.mu.Unlock()
299:	coord.asyncJobs.onWebDone = func(c AsyncCompletion) { deliveries = append(deliveries, c) }
424:	_, _, err = coord.asyncJobs.Start(parent, "bash-call-list", "{}", "bash", "", true, false, nil, func() {})
```

### `internal/agent/coordinator_autoresume_test.go` (8 hits)

```
269:	coord.asyncJobs = newWorkLedger(nil)
270:	coord.asyncJobs.store = newTestAsyncJobStore(t)
271:	coord.asyncJobs.coord = coord
275:		require.NoError(t, coord.asyncJobs.store.InsertSessionNotice(t.Context(), "sess-1", "manual_test_notice", "owed", true, ""))
311:	coord.asyncJobs = newWorkLedger(nil)
312:	coord.asyncJobs.store = newTestAsyncJobStore(t)
313:	coord.asyncJobs.coord = coord
315:	require.NoError(t, coord.asyncJobs.store.InsertSessionNotice(t.Context(), "child-1", "manual_test_notice", "owed", true, ""))
```

### `internal/agent/coordinator_await_tasks.go` (7 hits)

```
9:// (AsyncJobStore.LiveWorkForRoots).
46:	if !batchFlagEnabled() && (c.asyncJobs == nil || c.asyncJobs.store == nil) {
63:			if c.asyncJobs == nil || c.asyncJobs.store == nil {
72:		if c.asyncJobs == nil || c.asyncJobs.store == nil {
75:		work := c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
186:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
190:		return c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
```

### `internal/agent/coordinator_await_tasks_worker_test.go` (8 hits)

```
17:// newAwaitWorkerEnv builds a coordinator whose sessions/asyncJobs come from
19:func newAwaitWorkerEnv(t *testing.T) (fakeEnv, *coordinator, *session.AsyncJobStore) {
22:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-worker-test")
26:		asyncJobs: &workLedger{store: store},
175:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "await-top-test")
179:		asyncJobs: &workLedger{store: store},
223:// TestAwaitTasksNeverStallDetached pins await_tasks in stallDetachExcludedTools:
226:	require.True(t, stallDetachExcludedTools[tools.AwaitTasksToolName])
```

### `internal/agent/coordinator_background.go` (9 hits)

```
43:		return fmt.Sprintf("Async job %s (%s) was stopped (job_kill). Partial output before the stop:\n\n%s",
62:// notifyAsyncCompletion is the workLedger.onWebDone callback: the job's
64:// async_jobs row with delivery='pending' (work_ledger_transition.go) --
105:// HasPendingAsyncJobs and the in-memory ready queue they read are gone.
126:// (doc sec.2: a notice with no async_jobs row). Step 3: the fact is
136:	// A shell job_kill already cancelled (its row is terminal, the tool result
219:// its delegation currently drives: a RUNNING async_jobs row names it as the
227:	if c.asyncJobs == nil {
230:	running, err := c.asyncJobs.hasRunningDelegationFor(ctx, sessionID)
```

### `internal/agent/coordinator_bgshell_cap.go` (5 hits)

```
65:// The shell's own async_jobs row (kind=bg_shell, claimed by
80:	// B9-7: job_kill owns a cancelled shell's answer: under the same gate the
86:	if c.asyncJobs != nil && c.asyncJobs.store != nil {
89:		id, err := c.asyncJobs.store.InsertSessionNoticeReturningID(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
113:	_, notices, _, err := c.asyncJobs.store.PendingInclusiveDebtRows(ctx, sessionID)
```

### `internal/agent/coordinator_bgshell_window_test.go` (1 hits)

```
53:		remove bool // the shell left the manager (job_kill) before finishing
```

### `internal/agent/coordinator_child_policy_durable_test.go` (13 hits)

```
13://     this process's own in-memory workLedger/IsSessionBusy checks cannot
28:func newChildPolicyTestCoordinator(t *testing.T) (*coordinator, *workLedger, *session.AsyncJobStore, func(context.Context) fakeEnv) {
31:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
34:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
37:	coord.asyncJobs = ledger
69:	// long-finished delegation) while the durable async_jobs row backing
140:// -- no async_jobs row ever names the fork as anyone's ChildSessionID. Such
147:// child). Restored the ListAsyncJobsForOwner/ChildSessionID confirmation;
264:// childScopeOpenAcrossProcesses: a running async_jobs row for the child,
265:// with nothing at all in THIS process's own in-memory workLedger map (it was
298:// delegation test in work_ledger_delegation_test.go fail -- those tests'
309:// the child's reaction explicitly (work_ledger_delegation_test.go's
313:// (work_ledger_delegation.go) to the running-rows-only check -- this test's
```

### `internal/agent/coordinator_claim_driver_error_test.go` (4 hits)

```
17:func claimDriverFixture(t *testing.T) (*coordinator, *workLedger, string, *sql.DB) {
19:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
21:	ledger := newWorkLedger(nil)
24:	coord.asyncJobs = ledger
```

### `internal/agent/coordinator_cli_scope_test.go` (3 hits)

```
20:func seedCompletedJob(t *testing.T, store *session.AsyncJobStore, owner, callID string, kind session.JobKind, childID string) {
34:func seedBGShellNotice(t *testing.T, store *session.AsyncJobStore, owner string) {
152:		coord.asyncJobs.claimExternalDriver(child.ID)
```

### `internal/agent/coordinator_drain_cap_test.go` (2 hits)

```
116:// TestWakeSession_BgShellCapExhausted_AsyncJobWakeStillRunsTurn is the real
121:func TestWakeSession_BgShellCapExhausted_AsyncJobWakeStillRunsTurn(t *testing.T) {
```

### `internal/agent/coordinator_drain_policy.go` (8 hits)

```
153:	if c.asyncJobs == nil {
156:	debt, err = c.asyncJobs.reactionDebtExists(ctx, sessionID)
177:// async_jobs row with Owner=ParentSessionID and ChildSessionID=the exact
184:// the pre-existing ListAsyncJobsForOwner (the same query `sessions jobs`/
203:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
209:	jobs, listErr := c.asyncJobs.store.ListAsyncJobsForOwner(ctx, sess.ParentSessionID)
246:	if c.asyncJobs.store == nil {
249:	hasJobDebt, noticeKinds, err := c.asyncJobs.store.PendingInclusiveDebtSummary(ctx, sessionID)
```

### `internal/agent/coordinator_external_driver_durable_test.go` (6 hits)

```
30:	cli     *session.AsyncJobStore
51:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
54:	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
57:	f.coord.asyncJobs = f.ledger
63:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: f.ledger,
69:	cli := session.NewAsyncJobStore(conn, dataDir, os.Getpid()+1, "cli")
```

### `internal/agent/coordinator_external_driver_release_test.go` (4 hits)

```
25:	ledger := newWorkLedger(nil)
27:	coord.asyncJobs = ledger
45:	ledger := newWorkLedger(nil)
47:	coord.asyncJobs = ledger
```

### `internal/agent/coordinator_interrupt.go` (10 hits)

```
67:	if c.asyncJobs != nil {
68:		ids = c.asyncJobs.cancelTree(sessionID)
99:	if c.asyncJobs != nil && c.asyncJobs.store != nil {
102:		if err := c.asyncJobs.store.SetWakeZeroForOwners(zeroCtx, ids); err != nil {
112:	if c.asyncJobs != nil {
114:			c.asyncJobs.recheckChild(id)
124:	if c.asyncJobs != nil {
125:		c.asyncJobs.timeouts.close()
138:	if c.asyncJobs != nil {
139:		c.asyncJobs.close()
```

### `internal/agent/coordinator_rate_limit.go` (3 hits)

```
103:	if c.asyncJobs == nil {
106:	for _, id := range c.asyncJobs.treeSessionIDs(sessionID) {
107:		if c.asyncJobs.running(id) {
```

### `internal/agent/coordinator_rate_limit_budget_test.go` (3 hits)

```
39:	ledger := newWorkLedger(nil)
40:	ledger.bySession["root"] = &sessionJobs{jobs: map[string]*asyncJob{"job-1": {}}}
41:	c := &coordinator{asyncJobs: ledger}
```

### `internal/agent/coordinator_rate_limit_loop_test.go` (2 hits)

```
2:// admission, the armed-stall-clock invariant, and the wait WARN log. The
115:	_, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-no-stall-clock", nil,
```

### `internal/agent/coordinator_reaction_chain.go` (1 hits)

```
60:	if err := c.asyncJobs.store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindReactionChain, reactionChainMarkerText, false, ""); err != nil {
```

### `internal/agent/coordinator_reaction_chain_test.go` (4 hits)

```
120:	f.exec(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+f.sessID+`' AND tool_call_id IN ('chain1','chain2','chain3')`)
215:// command is progress, run_command sleep is idle, job_output/todos are
256:	// sleep 5 && echo done IS idle; run_command sleep is; job_output and
260:		[]fantasy.ToolCallContent{call("c1", "bash", `{"command":"sleep 5 && echo done"}`), call("c2", "run_command", `{"program":"sleep","args":["90"]}`), call("c3", "job_output", `{}`), call("c4", "todos", `{}`)},
```

### `internal/agent/coordinator_reaction_repeated_test.go` (3 hits)

```
16:	ts := &turnStream{a: &sessionAgent{asyncJobs: f.coord.asyncJobs}, call: SessionAgentCall{SessionID: f.sessID}}
39:		f.exec(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+f.sessID+`' AND tool_call_id='`+previous+`'`)
48:	ts := &turnStream{a: &sessionAgent{asyncJobs: f.coord.asyncJobs}, call: SessionAgentCall{SessionID: f.sessID}}
```

### `internal/agent/coordinator_reaction_source.go` (19 hits)

```
122:	if c.asyncJobs == nil {
125:	if store := c.asyncJobs.store; store != nil {
134:	c.asyncJobs.claimExternalDriver(sessionID)
159:	if c.asyncJobs == nil {
165:	if store := c.asyncJobs.store; store != nil {
174:	c.asyncJobs.releaseExternalDriver(sessionID)
180:	if c.asyncJobs == nil {
183:	return c.asyncJobs.reactionDebtExists(ctx, sessionID)
194:	if c.asyncJobs == nil {
197:	since := c.asyncJobs.hintSeqOf(sessionID)
204:	c.asyncJobs.waitForHint(waitCtx, sessionID, since)
211:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
217:	if _, err := c.asyncJobs.store.SweepDeadHosts(ctx, c.messages); err != nil {
222:	if err := c.asyncJobs.store.PurgeExpired(ctx, session.AsyncDataRetentionAge); err != nil {
230:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
264:	debt, err := c.asyncJobs.store.ReactionDebtExists(ctx, sessionID)
302:	c.asyncJobs.store.RecoverOwnerScope(ctx, sessionID, c.messages)
303:	rows, err := c.asyncJobs.store.ListRunningForOwners(ctx, []string{sessionID})
308:		if c.asyncJobs.store.HostNotDead(row.HostID) {
```

### `internal/agent/coordinator_recheck.go` (3 hits)

```
85:	if c.asyncJobs != nil {
92:		for _, child := range c.asyncJobs.parkedChildSessions() {
93:			c.asyncJobs.recheckChild(child)
```

### `internal/agent/coordinator_recheck_child_test.go` (14 hits)

```
26:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)
28:	require.True(t, coord.asyncJobs.hasParked())
30:	require.Equal(t, []string{parkedParentSession}, coord.asyncJobs.parkedParentSessions())
31:	require.Equal(t, []string{parkedChildSession}, coord.asyncJobs.parkedChildSessions())
42:// := range c.parkedParentSessions() { c.asyncJobs.recheckChild(parent) }` --
52:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)
54:	require.True(t, coord.asyncJobs.hasParked())
62:	coord.asyncJobs.finish(jobOf(coord.asyncJobs, parkedChildSession, parkedChildJob), jobResult{content: "gate ok"})
68:	require.True(t, coord.asyncJobs.hasParked(), "precondition: the delegation itself must still be parked (no run-ended trigger fired)")
82:	require.False(t, coord.asyncJobs.hasParked())
92:	store := newTestAsyncJobStore(t)
94:	coord.asyncJobs = newWorkLedger(coord.notifyAsyncCompletion)
95:	coord.asyncJobs.store = store
96:	coord.asyncJobs.coord = coord
```

### `internal/agent/coordinator_recheck_debt_deadline_test.go` (8 hits)

```
55:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
59:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
62:	coord.asyncJobs = ledger
67:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
146:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
150:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
153:	coord.asyncJobs = ledger
158:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
```

### `internal/agent/coordinator_rerun.go` (6 hits)

```
2:// Rerun-scoped stop (work_ledger_rerun.go). The durable reconciliation is
19:func (c *coordinator) StopRerunJobs(_ context.Context, sessionID string, voided []session.VoidedAsyncJob) {
20:	if c.asyncJobs == nil || len(voided) == 0 {
24:	if c.asyncJobs.store != nil {
25:		ownHost = c.asyncJobs.store.HostID()
41:	c.asyncJobs.stopToolCallsForRerun(sessionID, running)
```

### `internal/agent/coordinator_rerun_test.go` (16 hits)

```
23:	store    *session.AsyncJobStore
30:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
33:	coord.asyncJobs = newWorkLedger(nil)
34:	coord.asyncJobs.store = store
35:	coord.asyncJobs.coord = coord
42:	_, _, err := f.coord.asyncJobs.Start(owner, callID, "", tool, child, false, false, nil, func() {})
44:	f.coord.asyncJobs.acknowledged(jobOf(f.coord.asyncJobs, owner, callID))
47:func (f *rerunCoordFx) row(t *testing.T, owner, callID string) db.AsyncJob {
65:	f.coord.asyncJobs.finish(jobOf(f.coord.asyncJobs, "root", "call-debt"), jobResult{content: "out"})
84:// voided row is stopped (job_kill semantics), and a voided delegation's
96:	f.coord.StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{
119:	f.coord.StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{
130:		(&coordinator{}).StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{{ToolCallID: "x", State: "running"}})
148:	f.coord.asyncJobs.finish(jobOf(f.coord.asyncJobs, "root", "call-kept"), jobResult{content: "kept result"})
174:		_, _, err := f.coord.asyncJobs.Start("root", callID, "", "bash", "", false, false, nil, func() {})
176:		msg, handled, err := f.coord.asyncJobs.acknowledgeWithMessageTx(ctx, jobOf(f.coord.asyncJobs, "root", callID), f.messages, message.CreateMessageParams{
```

### `internal/agent/coordinator_retry_classify.go` (1 hits)

```
248:// condition. This generalizes the original stall-only retry: a watchdog
```

### `internal/agent/coordinator_run.go` (1 hits)

```
173:	defer stallDetachCleanupSession(sessionID)
```

### `internal/agent/coordinator_run_drain_test.go` (2 hits)

```
88:	coord.asyncJobs = newWorkLedger(nil)
89:	coord.asyncJobs.coord = coord // the gate lives in the coordinator's arbiter
```

### `internal/agent/coordinator_scope_recovery_test.go` (13 hits)

```
18:// newTestAsyncJobStoreWithDataDir is newTestAsyncJobStore plus the raw
20:// seed a running row directly (session.AsyncJobStore.Claim always registers
22:func newTestAsyncJobStoreWithDataDir(t *testing.T) (*session.AsyncJobStore, string, *sql.DB) {
31:	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
44:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
51:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
56:	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
62:	c.asyncJobs = newWorkLedger(nil)
63:	c.asyncJobs.store = store
81:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
88:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
95:	c.asyncJobs = newWorkLedger(nil)
96:	c.asyncJobs.store = store
```

### `internal/agent/coordinator_session_state_sweep.go` (3 hits)

```
22:func (l *workLedger) sweepIdleSessions() int {
72:	if c.asyncJobs != nil {
73:		c.asyncJobs.sweepIdleSessions()
```

### `internal/agent/coordinator_session_state_sweep_test.go` (6 hits)

```
11:func ledgerEntryIDs(l *workLedger) map[string]bool {
27:	l := newWorkLedger(nil)
28:	c := &coordinator{asyncJobs: l}
35:	l.sessionLocked("busy").jobs["j"] = &asyncJob{}
96:	l := newWorkLedger(nil)
97:	c := &coordinator{asyncJobs: l}
```

### `internal/agent/coordinator_stop_child_driver_test.go` (1 hits)

```
32:			c.coord.StopRerunJobs(context.Background(), c.parentID, []session.VoidedAsyncJob{{
```

### `internal/agent/coordinator_stop_restart_test.go` (4 hits)

```
32:	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
34:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
37:	coord.asyncJobs = ledger
62:	restart := session.NewAsyncJobStore(conn, dataDir, os.Getpid()+1, "restart")
```

### `internal/agent/coordinator_stop_tree_test.go` (5 hits)

```
68:// must walk the whole live delegation tree (workLedger.cancelTree) and zero
73:// REVERT CHECK: changed coordinator.Cancel to call c.asyncJobs.cancelSession
86:	store := newTestAsyncJobStore(t)
88:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
91:	coord.asyncJobs = ledger
```

### `internal/agent/coordinator_subagent_drivers.go` (1 hits)

```
202:// deferred this cleanup here). Called from recheckChild's tail (work_ledger_
```

### `internal/agent/coordinator_subagent_drivers_test.go` (7 hits)

```
24:	coord.asyncJobs = newWorkLedger(nil)
25:	coord.asyncJobs.store = newTestAsyncJobStore(t)
26:	coord.asyncJobs.coord = coord
38:	require.False(t, coord.asyncJobs.childScopeDrained("child-1"),
42:	require.True(t, coord.asyncJobs.childScopeDrained("child-1"),
47:	require.True(t, coord.asyncJobs.childScopeDrained("no-driver-session"),
84:// tail (work_ledger_delegation.go) and, for this direct-unit-call variant,
```

### `internal/agent/coordinator_tool_pinning_test.go` (2 hits)

```
758:		"bash", "run_command", "job_output", "job_kill",
926:	commandTools := []string{"bash", "run_command", "job_output", "job_kill"}
```

### `internal/agent/coordinator_tools.go` (6 hits)

```
115:		AsyncJobs:         c.asyncJobs,
193:// Includes "await_tasks" and "job_output": the worker's await_tasks BLOCKS
197:// on job_output.
711:		tools.NewJobOutputTool(c.asyncJobs, c.asyncJobs, backgroundManager),
712:		tools.NewJobKillTool(c.asyncJobs, c.asyncJobs, backgroundManager),
831:// hooks/restrictions, then the stall-detach wrapper. Flag ON: hooks and
```

### `internal/agent/coordinator_tools_test.go` (2 hits)

```
583:		"job_output",
584:		"job_kill",
```

### `internal/agent/coordinator_wake.go` (4 hits)

```
4:// async_jobs row with delivery='pending', or a session_notices row). Its job
28:	if c.asyncJobs == nil {
38:		c.asyncJobs.bumpHint(sessionID)
40:	if c.asyncJobs.isExternalDriver(sessionID) {
```

### `internal/agent/coordinator_wake_allowlist_test.go` (10 hits)

```
108:	coord.asyncJobs = newWorkLedger(nil)
109:	coord.asyncJobs.store = newTestAsyncJobStore(t)
110:	coord.asyncJobs.coord = coord
117:	require.NoError(t, coord.asyncJobs.store.InsertSessionNotice(t.Context(), "child-1", "manual_test_notice", "owed", true, ""))
169:	coord.asyncJobs = newWorkLedger(nil)
170:	coord.asyncJobs.store = newTestAsyncJobStore(t)
171:	coord.asyncJobs.coord = coord
174:	_, _, err := coord.asyncJobs.Start(owner, callID, "", AgentToolName, childSession, false, false, nil, func() {})
176:	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, owner, callID))
184:	wrapped.run(ctx, func() {}, jobOf(coord.asyncJobs, owner, callID), owner, childSession, fantasy.ToolCall{
```

### `internal/agent/coordinator_wake_reaction_debt_test.go` (10 hits)

```
5:// throughout (newTestAsyncJobStore).
43:// coordinator + workLedger (real SQLite store) + a real *sessionAgent
50:	ledger   *workLedger
51:	store    *session.AsyncJobStore
124:	f.store = newTestAsyncJobStore(t)
126:	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
129:	f.coord.asyncJobs = f.ledger
135:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: f.ledger,
233:// REVERT CHECK: changed wakeSession's `if c.asyncJobs.isExternalDriver(job.owner)
274:// TestWebAsyncJob_EndToEnd_OneNoticeOneTurn (internal/agent).
```

### `internal/agent/coordinator_work_scope.go` (8 hits)

```
6:// rather than moving onto workLedger itself.
10:// replaced their successor workLedger.next() with the DB scope predicate
53:	if c.asyncJobs != nil && c.asyncJobs.store != nil {
55:		failedTexts, err := c.asyncJobs.store.ListReactedFailedText(failCtx, childSessionID)
154:	if c.asyncJobs == nil {
157:	c.asyncJobs.recheckChild(sessionID)
163:	if c.asyncJobs == nil {
166:	return c.asyncJobs.parkedParentSessions()
```

### `internal/agent/coordinator_work_scope_test.go` (7 hits)

```
14:// writeParkedChildTurnNoText mirrors work_ledger_delegation_test.go's
43:	coord.asyncJobs = newWorkLedger(nil)
44:	coord.asyncJobs.store = newTestAsyncJobStore(t)
45:	coord.asyncJobs.coord = coord
67:	coord.asyncJobs = newWorkLedger(nil)
68:	coord.asyncJobs.store = newTestAsyncJobStore(t)
69:	coord.asyncJobs.coord = coord
```

### `internal/agent/default_job_timeout_test.go` (5 hits)

```
37:// starts from (mirrors work_ledger_timeout_test.go).
38:func defaultTimeoutLedger(t *testing.T, onDone func(AsyncCompletion)) (*workLedger, *coordinator) {
40:	l := newWorkLedger(onDone)
41:	l.store = newTestAsyncJobStore(t)
44:	return l, &coordinator{asyncJobs: l, subAgentDrivers: newSubAgentDriverRegistry(), sessions: stubChildSessions{}}
```

### `internal/agent/delegation_question.go` (9 hits)

```
39:func (l *workLedger) heldQuestionJob(childID string) *asyncJob {
58:func (l *workLedger) childAwaitingQuestion(childID string) (*asyncJob, string, bool) {
91:func (l *workLedger) childOwnRunningJobs(childID string) int {
132:func (l *workLedger) noteChildQuestion(childID string) {
166:		coord.asyncJobs.bumpHint(owner)
183:func (l *workLedger) answerHeldDelegation(ctx context.Context, owner, childID, prompt string) (fantasy.ToolResponse, bool) {
189:	// in awaitSync: it is never answered here.
242:func (l *workLedger) clearAnswerHold(job *asyncJob) {
263:func (l *workLedger) childAwaitingQuestionSummary(childID string, elapsed time.Duration) (line string, ok bool) {
```

### `internal/agent/delegation_question_test.go` (4 hits)

```
262:// delegation's awaitSync -- Run misses the 2s deadline and the inner tool
320:			// delegation whose awaitSync only unblocks when the child's scope
335:				t.Fatal("agent tool did not return within 2s: the answer missed the held delegation and blocked in awaitSync")
414:	// Make X look SDK-sync-claimed (work_ledger.Start sets done for sync jobs).
```

### `internal/agent/drain_attempt.go` (19 hits)

```
8:// writes (work_ledger_reaction.go, coordinator_drain_policy.go).
152:	if a.asyncJobs != nil {
153:		att.hintAt = a.asyncJobs.hintSeqOf(call.SessionID)
166:	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
171:	a.asyncJobs.coord.accountDrainAttempt(ctx, att, turnErr)
230:	l := c.asyncJobs
294:	l := c.asyncJobs
384:	store := c.asyncJobs.store
425:	l := c.asyncJobs
445:	if c.asyncJobs == nil || sessionID == "" {
448:	c.asyncJobs.arbPace(sessionID, 0, drainRetryAfterFailure(), false, paceUncounted)
461:	if c.asyncJobs == nil || c.asyncJobs.store == nil || snap.Empty() {
479:	settled, err := c.asyncJobs.store.SettleReactedFailedWithMarker(ctx, sessionID, snap, text)
493:	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
496:	a.asyncJobs.coord.noteDrainRefused(sessionID, cause)
511:	if a.asyncJobs != nil {
513:		coord := a.asyncJobs.coord
521:			a.asyncJobs.pushDeadlineOnTurnEnd(call.SessionID)
547:	if c.asyncJobs.arbPace(sessionID, hintAt, drainRetryAfterFailure(), hintOpens, kind) {
```

### `internal/agent/drain_attempt_fixture_test.go` (10 hits)

```
2:// phase4-round2-attempts-design.md sec.5): a real coordinator + workLedger on
30:	store         *session.AsyncJobStore
32:	ledger        *workLedger
98:	f.store = session.NewAsyncJobStore(f.env.conn, f.env.workingDir, os.Getpid(), "test")
109:	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
112:	f.coord.asyncJobs = f.ledger
124:		Tools: tools, DisableAutoSummarize: true, AsyncJobs: f.ledger,
205:func (f *attemptFixture) row(ctx context.Context, toolCallID string) db.AsyncJob {
235:	f.exec(ctx, `CREATE TRIGGER fx_block_reaction BEFORE UPDATE OF reacted ON async_jobs
242:	f.exec(ctx, `CREATE TRIGGER fx_block_reaction BEFORE UPDATE OF reacted ON async_jobs
```

### `internal/agent/drain_compaction_continuation_test.go` (3 hits)

```
91:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
93:	ledger := newWorkLedger(nil)
100:		Tools: []fantasy.AgentTool{}, AsyncJobs: ledger,
```

### `internal/agent/drain_compaction_submit_test.go` (18 hits)

```
39:	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
41:	ledger := newWorkLedger(nil)
47:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
184:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
186:	ledger := newWorkLedger(nil)
190:		Sessions: env.sessions, Messages: env.messages, DataDirectory: env.workingDir, AsyncJobs: ledger,
241:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
243:	ledger := newWorkLedger(nil)
248:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
271:// TestWebAsyncJob_EndToEnd_OneNoticeOneTurn drives the whole step-3 path for a
281:func TestWebAsyncJob_EndToEnd_OneNoticeOneTurn(t *testing.T) {
294:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
298:	ledger := newWorkLedger(coord.notifyAsyncCompletion)
301:	coord.asyncJobs = ledger
307:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
403:	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
405:	ledger := newWorkLedger(nil)
411:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
```

### `internal/agent/drain_debt_edge_cases_test.go` (1 hits)

```
74:// PullPendingAsyncJobNotice's own SQL (sql/async_jobs.sql), which only ever
```

### `internal/agent/drain_gate_streaks_test.go` (1 hits)

```
20:const blockPullTrigger = `CREATE TRIGGER fx_block_pull BEFORE UPDATE OF delivery ON async_jobs
```

### `internal/agent/drain_launch_gate_test.go` (2 hits)

```
190:	f.exec(ctx, `CREATE TRIGGER fx_block_pull BEFORE UPDATE OF delivery ON async_jobs
310:			f.exec(ctx, `ALTER TABLE async_jobs RENAME TO fx_async_jobs`)
```

### `internal/agent/drain_notice_pull_test.go` (10 hits)

```
196:	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
197:	ledger := newWorkLedger(nil)
203:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
272:	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
273:	ledger := newWorkLedger(nil)
279:		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
326:	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
327:	ledger := newWorkLedger(nil)
378:		Tools: []fantasy.AgentTool{toolA, toolB}, DisableAutoSummarize: true, AsyncJobs: ledger,
443:	store      *session.AsyncJobStore
```

### `internal/agent/loop_detection_test.go` (6 hits)

```
233:		detail := loopDetail{ToolName: "job_output", Count: 6, Threshold: 5}
244:		if !strings.Contains(msg, "job_output") {
245:			t.Errorf("message %q should name the tool %q", msg, "job_output")
306:		return makeToolStep("job_output", `{"id":"j1"}`, "running")
351:	if firedDetail.ToolName != "job_output" {
352:		t.Errorf("detail.ToolName = %q, want %q", firedDetail.ToolName, "job_output")
```

### `internal/agent/policy_leaf.go` (1 hits)

```
69:	case *stallDetacherTool:
```

### `internal/agent/policy_leaf_baseline_test.go` (2 hits)

```
21:	coord.asyncJobs = &workLedger{}
82:	case *stallDetacherTool:
```

### `internal/agent/prompt/prompt_scoped_test.go` (1 hits)

```
55:	t.Setenv("RUSH_BATCH_RUNTIME", "false")
```

### `internal/agent/question_stop_format_test.go` (3 hits)

```
26:			store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
54:			_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
61:			_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
```

### `internal/agent/release_callback_join_test.go` (1 hits)

```
16:	c.asyncJobs = newWorkLedger(c.notifyAsyncCompletion)
```

### `internal/agent/restricted_stall_detach_test.go` (1 hits)

```
1:// REVERT-CHECK: removing the *stallDetacherTool unwrap from
```

### `internal/agent/restricted_tool.go` (3 hits)

```
33:	"job_kill":                   "delete",
34:	"job_output":                 "read",
93:	if detacher, ok := tool.(*stallDetacherTool); ok {
```

### `internal/agent/session_idle_trigger_test.go` (10 hits)

```
9:// Every OTHER delegation test in this package (work_ledger_delegation_test.go)
111:	coord.asyncJobs = newWorkLedger(func(c AsyncCompletion) { delivered <- c })
112:	coord.asyncJobs.store = newTestAsyncJobStore(t)
113:	coord.asyncJobs.coord = coord
123:		AsyncJobs:  coord.asyncJobs,
158:	_, _, err = coord.asyncJobs.Start(parentSession, parentCall, "", AgentToolName, childSession, false, false, nil, func() {})
160:	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, parentSession, parentCall))
161:	coord.asyncJobs.armDelegation(jobOf(coord.asyncJobs, parentSession, parentCall), jobResult{content: "child yielded: still working"})
162:	require.True(t, coord.asyncJobs.hasParked())
186:	require.False(t, coord.asyncJobs.hasParked())
```

### `internal/agent/step_progress.go` (3 hits)

```
25:	// a real command, agent/fetch, job_kill, wake*, unknown MCP tools. An
39:	// stepCallNeutral neither advances nor launches: job_output/todos, or an
278:// async_tool.go's cap branch: workLedger.Start refused the job, so the tool
```

### `internal/agent/step_progress_test.go` (4 hits)

```
16:// asyncCapTag is the ClientMetadata async_tool.go stamps on a workLedger.Start
18:// claim was taken (work_ledger_cap.go).
108:		{"job_output -> neutral", call("job_output", `{"job_id":"j"}`), ok(""), nil, stepCallNeutral},
114:		{"refusal outranks neutral tool", call("job_output", `{}`), guardRefusedResult(), nil, stepCallRefused},
```

### `internal/agent/stream_watchdog.go` (1 hits)

```
533:			"The turn made no durable progress for over %s and was auto-cancelled by the turn-stall watchdog. Re-run the prompt; if it keeps happening, raise --stall-timeout.",
```

### `internal/agent/stream_watchdog_toolpause_test.go` (1 hits)

```
86:// job_output --wait, or a sub-agent delegation via the "agent" tool that
```

### `internal/agent/supervision.go` (21 hits)

```
6:// Deliberately NOT a workLedger job: a supervision timer never appears in
15:// The single per-process timer goroutine is shared with asyncJob deadlines
16:// (work_ledger_timeout.go's timeoutService/timeoutEntry) -- this file never
134:// the cap on non-terminal async jobs elsewhere (work_ledger_cap.go, ASYNC-12).
152:// Called from asyncTool.Run AFTER workLedger.Start has released l.mu, so
156:func (l *workLedger) noteWorkStarted(ctx context.Context, sessionID string) {
203:func (l *workLedger) recordProgress(sessionID string) {
238:func (l *workLedger) pushDeadlineOnTurnEnd(sessionID string) {
264:func (l *workLedger) clearSupervisionIfPresent(sessionID string) {
285:// supervisionRegistry.mu / workLedger.mu / the agent mailbox before acting
287:// work_ledger_timeout.go's handleTimeout already documents for asyncJob
290:func (l *workLedger) handleSupervisionDeadline(rootSessionID string, generation uint64) {
384:				text += fmt.Sprintf("\njob %s has been silent for %s: job_kill it unless you can name evidence of progress other than processes existing.",
426:// (work_ledger_timeout.go) for each one's last-output-line -- the same
430:func (l *workLedger) buildSupervisionSummary(rootSessionID string, tickNum int, interval time.Duration, paused bool) string {
440:func (l *workLedger) buildSupervisionSummaryAt(now time.Time, rootSessionID string, tickNum int, interval time.Duration, paused bool) (string, []flaggedJob) {
492:	guidance := "Keep waiting, inspect with job_output, or stop with job_kill (commands); for a sub-agent use inspect_agent, inject_agent or stop_agent."
497:		guidance = "Verify liveness with job_output (last output time, recent lines). Process existence is not progress. If there is no sign of progress, stop it with job_kill and report.\n" + guidance
515:			text += fmt.Sprintf("\nSupervision is pausing: job %s has been running %s and silent %s — job_kill it now or state why it must keep running.",
539:func (l *workLedger) idleChildSummary(childID string, elapsed time.Duration) (string, bool) {
644:	if c.asyncJobs == nil || sessionID == "" {
```

### `internal/agent/supervision_silence.go` (4 hits)

```
41:func (l *workLedger) jobLastOutputAt(rootSessionID string, j jobSnapshot) (last time.Time, wrote bool, known bool) {
58:func (l *workLedger) jobSilence(now time.Time, rootSessionID string, j jobSnapshot) (time.Duration, bool) {
114:func (l *workLedger) silenceLine(now time.Time, rootSessionID string, j jobSnapshot) (string, bool) {
131:func (l *workLedger) silenceFlaggedJobs(now time.Time, rootSessionID string, snaps []jobSnapshot) []flaggedJob {
```

### `internal/agent/supervision_silence_test.go` (3 hits)

```
93:func startSilentJob(t *testing.T, l *workLedger, owner, id, input string, buf tools.LiveOutputBuffer, shellID string, startedAt time.Time) {
130:	require.True(t, strings.HasPrefix(guidanceOf(text), "Verify liveness with job_output"),
253:	require.Contains(t, text, "job_kill it now or state why it must keep running")
```

### `internal/agent/supervision_test.go` (14 hits)

```
6:// §5.2/§6/§7). Style follows work_ledger_timeout_test.go: bare *coordinator
29:func latestSessionNoticeText(t *testing.T, l *workLedger, owner string) string {
39:func sessionNoticeTextAt(t *testing.T, l *workLedger, owner string, i int) string {
47:// newSupervisionTestLedger builds a bare workLedger+coordinator pair wired
52:func newSupervisionTestLedger(t *testing.T) (*workLedger, *coordinator) {
54:	l := newWorkLedger(nil)
55:	l.store = newTestAsyncJobStore(t)
58:	coord.asyncJobs = l
63:// like startChildOwnedJob (work_ledger_delegation_test.go) -- l.running(id)
65:func startOpenJob(t *testing.T, l *workLedger, sessionID, toolCallID string) {
245:	require.Contains(t, text, "job_output")
246:	require.Contains(t, text, "job_kill")
413:	sa := &sessionAgent{asyncJobs: l}
569:// timeoutService services both asyncJob deadlines and supervision deadlines.
```

### `internal/agent/templates/coder.md.tpl` (1 hits)

```
20:While workers or your own jobs are running and you have nothing else to do, call `await_tasks` (`until: "all"` when you need every result) — never `ask_question`, never repeated `inspect_agent`/`job_output` polling. Use `ask_question` only for a decision you genuinely cannot make.
```

### `internal/agent/tool_suggest_test.go` (1 hits)

```
68:	available := []string{"bash", "grep", "view", "edit", "job_output"}
```

### `internal/agent/tools/agent_control_test.go` (1 hits)

```
165:	// Stage 3 (task #1024): job_kill/job_output's refusal for a delegation
```

### `internal/agent/tools/await_tasks.md` (1 hits)

```
27:ask_question, and never poll inspect_agent/job_output in a loop — those
```

### `internal/agent/tools/bash.go` (5 hits)

```
28:	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Set to true (boolean) to run this command in the background. Use job_output to read the output later."`
214:					// jobs finish, and job_kill frees them on demand — so the
273:				response := fmt.Sprintf("Background shell started with ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
286:				// model can free slots with job_kill or wait for jobs to
374:			response := fmt.Sprintf("Command is taking longer than expected and has been moved to background.\n\nBackground shell ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
```

### `internal/agent/tools/bash.md.tpl` (4 hits)

```
32:- The completion notice includes output and exit status. Do not poll job_output or call it with wait=true merely to learn whether the job finished.
34:- Use job_kill only when the operator asks to stop a running command or the current task requires cancellation. Stopping it this way produces a distinct "stopped" notice, not a generic failure.
35:- Pass that job ID as job_id to job_kill/job_output to control or inspect the command before it finishes -- shell_id is only for callers that never see a job ID.
36:- job_output accepts a cursor from a previous call's next_cursor to fetch only output written since then.
```

### `internal/agent/tools/bash_start_failure_test.go` (3 hits)

```
23:// goroutine) and that the model can change on purpose via job_kill. The cap
35:// the model is told which of its options (wait vs job_kill) applies.
92:				"(job_kill, waiting, or a foreground command)")
```

### `internal/agent/tools/job_kill.go` (9 hits)

```
15:	JobKillToolName = "job_kill"
18://go:embed job_kill.md
37:// NewJobKillTool builds the job_kill tool. resolver resolves a job_id (the
63:			if resp, handled := stallDetachKillResponse(sessionID, params.JobID); handled {
85:					// placeholder promising a later message (job_kill now
115:			// job_kill) from Stop-cancel or a timeout. The pre-existing
133:				// §2.2) produces a distinct "stopped (job_kill)" outcome
149:					// concurrent job_kill already claimed it): idempotent
195:					slog.Debug("job_kill: shell kill reported an error after the job was already stopped",
```

### `internal/agent/tools/job_kill.md` (1 hits)

```
6:- Stopping a job this way returns a distinct "stopped (job_kill)" result carrying the real output captured right before the stop -- not a generic failure, and not a second, later notice.
```

### `internal/agent/tools/job_kill_bgshell.go` (1 hits)

```
8:	// handled: job_kill is finished (the row was already terminal); resp is
```

### `internal/agent/tools/job_output.go` (7 hits)

```
16:	JobOutputToolName = "job_output"
19://go:embed job_output.md
22:// jobOutputMaxWait bounds a `wait:true` job_output call so it never blocks
46:// NewJobOutputTool builds the job_output tool. resolver resolves a job_id
72:			if resp, handled := stallDetachOutputResponse(sessionID, params); handled {
198:// runCommandOutputResponse serves job_output for a run_command job_id (task
233:	return "\n\n(still running after the wait window — call job_output again to keep waiting)"
```

### `internal/agent/tools/job_output_lastoutput_test.go` (2 hits)

```
16://   TestJobOutputLastOutput_HeaderRunningWithOutput    -> the `", last output %s ago"` header append in job_output.go
17://   TestJobOutputLastOutput_HeaderRunningNoOutput      -> the `", no output yet"` header append in job_output.go
```

### `internal/agent/tools/job_output_pending.go` (2 hits)

```
7:// async_jobs row answers what happened to the result: it is still waiting to
12:// (Delivered=true). A typed error (not a string match) so job_output's
```

### `internal/agent/tools/job_output_test.go` (4 hits)

```
22:	// (read by the tool at job_output.go), which the sibling
188:	require.NotContains(t, resp.Content, "call job_output again to keep waiting")
196:	require.Contains(t, shellResp.Content, "call job_output again to keep waiting")
202:// delivered -- job_output answers with plain text, not an error.
```

### `internal/agent/tools/job_run_command_control_test.go` (13 hits)

```
14:// job_id as a run_command job (RunCommandJobError), for testing job_kill/
15:// job_output's routing to RunCommandController (task #1023 §3) without a
55:	// job_kill's pre-existing generic-wording fallback.
77:// TestJobOutputTool_RunCommandJobRoutesToController proves job_output routes
104:// proves that with no RunCommandController wired, job_output still returns
120:// TestJobKillTool_RunCommandJobRoutesToController proves job_kill stops a
140:// #1063's wiring for run_command: job_kill returns StopRunCommandJob's own
142:// a message" placeholder -- job_kill produces no second, later notice for a
147:	runCtl := &fakeRunCommandController{stopText: "Async job call-1 (run_command) was stopped (job_kill). Partial output before the stop:\n\nline one"}
192:	require.Contains(t, resp.Content, "stop_agent", "stage 3: job_kill now points at the dedicated control tool")
198:// job_kill case for job_output.
215:// TestJobKillTool_MarksJobStoppedBeforeKilling proves job_kill records the
228:	// TestJobKillTool_ResolvesJobIDToShellID for why (B11: job_kill now
```

### `internal/agent/tools/job_shell_resolver.go` (21 hits)

```
18:	// directly (job_output/job_kill return it via NewTextErrorResponse). A
23:	// stopped by an explicit job_kill call, BEFORE job_kill actually kills
24:	// the underlying shell (task #1023 §2.2), and returns job_kill's own
27:	//     output snapshot worded "stopped (job_kill)" and job_kill must now
31:	//     worded from that COMMITTED row, and job_kill must return it as is
34:	//     already delivered, or a concurrent job_kill already claimed it) --
35:	//     text is empty and job_kill refuses with the idempotent "not found
39:	// otherwise, and for a job that has no durable row): job_kill puts it in
58:// RunCommandController lets job_kill/job_output act on a run_command job,
61:// job control -- job_kill/job_output then just surface RunCommandJobError's
70:	// success, text is job_kill's own final answer (task #1063): the real
81:// job_kill/job_output route it through RunCommandController instead of
91:// agentic_fetch) job. job_kill/job_output refuse these directly and point at
103:// errJobShellIDRequired is the shared validation error for job_kill and
104:// job_output: exactly one of job_id/shell_id must be given.
107:// resolveShellID validates the job_id/shell_id pair shared by job_kill and
108:// job_output and returns the concrete shell id to operate on. shellID given
126:// BGShellStopper is the optional second capability of the resolver job_kill
127:// receives: it stops a background shell that has a durable async_jobs row of
130:// not implement it leaves job_kill on its older path.
133:	// BEFORE job_kill kills the process, with the same verdicts as
```

### `internal/agent/tools/job_shell_resolver_test.go` (18 hits)

```
15:// fakeJobShellResolver is a minimal JobShellResolver for testing job_kill/
16:// job_output's job_id wiring without a real work ledger (internal/agent,
24:	// #1063, B11). The zero verdict (JobStopNotFound) makes job_kill refuse
83:// TestJobKillTool_ResolvesJobIDToShellID proves job_kill's job_id path end
97:	// job_kill refuse outright (never touching bgManager) when the
135:// in job_kill.go itself: when MarkJobStopped reports a fresh stop (JobStopStopped)
136:// with its own text, job_kill returns that text VERBATIM as the tool's
150:		markText:    "Async job call-1 (bash) was stopped (job_kill). Partial output before the stop:\n\nline one\nline two",
160:	require.Equal(t, resolver.markText, resp.Content, "job_kill's own final answer must be the resolver's real-output text, not the generic wording")
165:// when MarkJobStopped reports JobStopNotFound (a concurrent job_kill already
166:// claimed the job, or it is gone/delivered), job_kill must refuse outright --
169:// still running (StartOwned), so a pre-fix job_kill would happily kill it and
172:// Revert-check performed: made job_kill.go fall through to bgManager for any
200:// with text worded from the COMMITTED row, job_kill returns that text
232:		require.False(t, bgShell.IsDone(), "job_kill must not kill a shell for a job that already reached a terminal state")
273:// TestJobOutputTool_ResolvesJobIDToShellID mirrors the job_kill case for
274:// job_output: job_id resolves to the shell whose output is read.
319:	const stopped = "Async job call-1 (bash) was stopped (job_kill). Partial output before the stop:\n\nsome output"
```

### `internal/agent/tools/run_command.go` (2 hits)

```
219:			// Tree-kill on ctx cancellation (job_kill, terminate_and_wake
227:					// job_output can read progressive output and job_kill's
```

### `internal/agent/tools/run_command.md.tpl` (1 hits)

```
19:- Use that job ID with job_output(job_id) to read accumulated output while the program is still running, or job_kill(job_id) to stop it early -- this kills the whole process tree it spawned, not just the direct process. Stopping it this way produces a distinct "stopped" notice, not a generic failure.
```

### `internal/agent/tools/run_command_output_buffer.go` (4 hits)

```
12:// only run_command, task #1023 §3) so job_output/job_kill and the ledger's
24:	// best-effort read (job_kill's stopped notice, timeout's capturePartial).
29:// job's output stays resident for progressive job_output reads. Distinct
122:// job_kill/job_output can read/stop it while still running (task #1023 §3).
```

### `internal/agent/tools/run_command_process_kill_test.go` (2 hits)

```
76:// TestRunCommand_CtxCancelTreeKillsGrandchild proves job_kill/timeout
121:	// §3's sink), the same mechanism job_output uses against a real ledger.
```

### `internal/agent/tools/run_command_process_unix.go` (1 hits)

```
13:// signals the direct child) with a whole-group SIGKILL, so job_kill and the
```

### `internal/agent/tools/run_command_process_windows.go` (1 hits)

```
11:// configureRunCommandProcess makes ctx cancellation (job_kill and the
```

### `internal/agent/tools/stall_detach_controller.go` (6 hits)

```
9:// StallDetachedJobController lets the agent package serve its stall-detached
11:// job_output/job_kill. Installed once by the agent package's init; nil
25:// stallDetachOutputResponse serves job_output for a stall-detached job;
27:func stallDetachOutputResponse(sessionID string, params JobOutputParams) (fantasy.ToolResponse, bool) {
44:// stallDetachKillResponse serves job_kill for a stall-detached job;
46:func stallDetachKillResponse(sessionID, jobID string) (fantasy.ToolResponse, bool) {
```

### `internal/agent/turn_arbiter.go` (4 hits)

```
28:	// (workLedger.recheckChild): a release-shaped retry that spends no slot.
59:// SessionFacts is the in-process (coordinator + workLedger) half of the
414:	l := c.asyncJobs
503:	// session-activity facts (AsyncJobStore.LiveWorkForRoots). An unreadable
```

### `internal/agent/turn_arbiter_shadow_test.go` (1 hits)

```
310:	other := session.NewAsyncJobStore(f.env.conn, f.env.workingDir, os.Getpid()+7777, "shadow-other-host")
```

### `internal/agent/turn_arbiter_state.go` (1 hits)

```
4:// suspension set, rerun holds, the reaction-chain trio) plus the workLedger
```

### `internal/agent/turn_arbiter_testhelpers_test.go` (4 hits)

```
42:func (l *workLedger) paceDrainGate(owner string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainPace) bool {
50:func (l *workLedger) resetDrainGate(owner string) {
89:	if c.asyncJobs != nil {
90:		hintNow = c.asyncJobs.hintSeqOf(owner)
```

### `internal/agent/turn_progress_fixture_test.go` (5 hits)

```
3:// coordinator, a real workLedger on a real SQLite store, a real *sessionAgent
59:// announced. workLedger.running() reports exactly that map, so the shared
103:// killHeldJob ends W for real, through the very path the model's own job_kill
207:// workLedger.Start, no async_jobs row, no claim -- and the fifth no-progress
214:// async_jobs row, so this test goes red on the row it expects to be absent (and
```

### `internal/agent/turn_progress_guard.go` (2 hits)

```
56:// a wait/re-read/refusal extends it, a neutral step (`todos`, `job_output`,
218:// runs on a refusal — no workLedger.Start, no hook, no permission prompt — the
```

### `internal/agent/turn_stall_clock.go` (4 hits)

```
32:// stallClock is one turn's stall-detection state, shared by turnStream,
96:// SetTurnStallPolicy installs the stall-policy seam invoked by the
293:// "stall-<first8-of-session-id>-".
299:	return "stall-" + id + "-"
```

### `internal/agent/turn_stall_clock_test.go` (2 hits)

```
98:	if len(base) < 15 || base[:14] != "stall-abcdef12" {
99:		t.Fatalf("dump name = %q, want stall-abcdef12-... prefix", base)
```

### `internal/agent/turn_stall_detach_cleanup_test.go` (52 hits)

```
17://     recover branch of the detach goroutine in stallDetacherTool.Run: without
21://     explicit job_kill (kill-requested jobs keep their final output readable,
26://   - TestStallDetachCleanupSession guards stallDetachCleanupSession's pending
56:		stallDetachPending.Delete(stallDetachKey(sessionID, callID))
57:		stallDetachJobs.Delete(stallDetachKey(sessionID, callID))
60:	tool := &stallDetacherTool{inner: panicTool{release: make(chan struct{})}}
72:		if _, ok := stallDetachPending.Load(stallDetachKey(sessionID, callID)); ok {
90:		if _, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
100:	_, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
102:		t.Fatalf("job_output ok=%v done=%v, want true/false while running", ok, done)
107:	// left intact for the job_output assertions below.
110:		if v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
111:			v.(*stallDetachEntry).mu.Lock()
112:			completed := v.(*stallDetachEntry).done
113:			v.(*stallDetachEntry).mu.Unlock()
124:	text, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
126:		t.Fatalf("job_output after panic ok=%v done=%v, want true/true", ok, done)
135:	t.Cleanup(func() { stallDetachJobs.Delete(stallDetachKey(sessionID, callID)) })
137:	e := &stallDetachEntry{sessionID: sessionID, callID: callID, done: true, content: "final output here"}
138:	stallDetachJobs.Store(stallDetachKey(sessionID, callID), e)
140:	out, done, total, ok := stallDetachJobController{}.StallDetachOutput(sessionID, "stall-"+callID, 0)
147:	if _, _, _, ok2 := (stallDetachJobController{}).StallDetachOutput(sessionID, "stall-"+callID, 0); ok2 {
154:	t.Cleanup(func() { stallDetachJobs.Delete(stallDetachKey(sessionID, callID)) })
156:	e := &stallDetachEntry{sessionID: sessionID, callID: callID, done: true, content: "finished"}
157:	stallDetachJobs.Store(stallDetachKey(sessionID, callID), e)
159:	text, ok := stallDetachJobController{}.StallDetachKill(sessionID, "stall-"+callID)
163:	if _, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
170:	finishedKey := stallDetachKey(sessionID, "call-s4a")
171:	runningKey := stallDetachKey(sessionID, "call-s4b")
172:	pendingKey := stallDetachKey(sessionID, "call-s4c")
173:	otherKey := stallDetachKey("det-cleanup-session-4-other", "call-x")
175:		stallDetachJobs.Delete(finishedKey)
176:		stallDetachJobs.Delete(runningKey)
177:		stallDetachJobs.Delete(otherKey)
178:		stallDetachPending.Delete(pendingKey)
181:	stallDetachJobs.Store(finishedKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4a", done: true, content: "done"})
182:	stallDetachJobs.Store(runningKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4b"})
183:	stallDetachJobs.Store(otherKey, &stallDetachEntry{sessionID: "det-cleanup-session-4-other", callID: "call-x", done: true})
184:	stallDetachPending.Store(pendingKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4c"})
186:	stallDetachCleanupSession(sessionID)
188:	if _, ok := stallDetachPending.Load(pendingKey); ok {
189:		t.Fatal("pending entry survived stallDetachCleanupSession")
191:	if _, ok := stallDetachJobs.Load(finishedKey); ok {
192:		t.Fatal("finished detached job survived stallDetachCleanupSession")
194:	if _, ok := stallDetachJobs.Load(runningKey); !ok {
195:		t.Fatal("still-running detached job was dropped by stallDetachCleanupSession")
197:	if _, ok := stallDetachJobs.Load(otherKey); !ok {
198:		t.Fatal("another session's finished job was dropped by stallDetachCleanupSession")
204:// Revert check: deleting `defer stallDetachCleanupSession(sessionID)` in
210:			key = stallDetachKey(sess.ID, "call-done")
211:			stallDetachJobs.Store(key, &stallDetachEntry{sessionID: sess.ID, callID: "call-done", done: true})
215:	_, ok := stallDetachJobs.Load(key)
```

### `internal/agent/turn_stall_policy_spacing_test.go` (4 hits)

```
33:	tool := &stallDetacherTool{inner: inner}
44:	// Abort disabled (--stall-timeout 0): the detach must still wake the model.
51:		if v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
52:			v.(*stallDetachEntry).cancel()
```

### `internal/agent/turn_stall_policy_test.go` (2 hits)

```
63:	sessionID := "stall-policy-test"
201:	f := newRetryFixture(t, "stall-join")
```

### `internal/agent/turn_stall_tool_detach.go` (63 hits)

```
21:// outcome stays reachable through job_output/job_kill. Delegations
24:// never kill a tool on our own — only an explicit model job_kill does.
26:// stallDetachExcludedTools are never detached: the delegation tools are
31:var stallDetachExcludedTools = map[string]bool{
40:// stallDetachPending holds the in-flight (not yet detached) synchronous
41:// tool calls, keyed by sessionID NUL callID; stallDetachJobs holds the
44:	stallDetachPending sync.Map // string -> *stallDetachEntry
45:	stallDetachJobs    sync.Map // string -> *stallDetachEntry
48:// stallDetachKey builds both registries' key.
49:func stallDetachKey(sessionID, callID string) string {
53:// stallDetacherTool wraps a fantasy.AgentTool so a stalled synchronous Run
55:// only via job_kill after a detach) and this Run returns the wake-up
57:type stallDetacherTool struct {
61:// wrapToolsWithStallDetach wraps every tool with a stallDetacherTool.
66:		out[i] = &stallDetacherTool{inner: t}
71:func (t *stallDetacherTool) Info() fantasy.ToolInfo {
75:func (t *stallDetacherTool) ProviderOptions() fantasy.ProviderOptions {
79:func (t *stallDetacherTool) SetProviderOptions(opts fantasy.ProviderOptions) {
83:func (t *stallDetacherTool) RestrictedRunAction() string {
93:func (t *stallDetacherTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
95:	if isBatchManagedCall(ctx) || stallDetachExcludedTools[name] {
100:	// job_kill only.
103:	entry := &stallDetachEntry{
112:	stallDetachPending.Store(stallDetachKey(entry.sessionID, entry.callID), entry)
134:		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
138:		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
139:		stallDetachJobs.Store(stallDetachKey(entry.sessionID, entry.callID), entry)
146:		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
152:// stallDetachEntry is one detachable synchronous tool run, first pending
154:type stallDetachEntry struct {
164:	// killRequested latches an explicit job_kill so complete() can record
165:	// the stop cause; only job_kill ever cancels the run.
172:// job_output/job_kill.
173:func (e *stallDetachEntry) jobID() string {
174:	return "stall-" + e.callID
178:func (e *stallDetachEntry) notice() string {
182:		"tool %s (%s) is still running after %s with no progress; it continues in the background as job %s. Wait with job_output or stop it with job_kill.",
187:// complete records the inner run's outcome so job_output can serve it.
188:func (e *stallDetachEntry) complete(resp fantasy.ToolResponse, err error) {
196:		e.content = "stopped by job_kill\n" + resp.Content
209:	key := stallDetachKey(sessionID, callID)
210:	v, ok := stallDetachPending.Load(key)
214:	e := v.(*stallDetachEntry)
241:// stallDetachJobController serves the detached jobs to job_output/job_kill
243:type stallDetachJobController struct{}
245:// init installs the controller; job_output/job_kill consult it before the
247:func init() { tools.StallDetachedJobs = stallDetachJobController{} }
252:func (stallDetachJobController) StallDetachOutput(sessionID, jobID string, cursor int64) (string, bool, int64, bool) {
253:	v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
257:	e := v.(*stallDetachEntry)
268:	// polling after job_kill); others are served once and dropped.
278:func (stallDetachJobController) StallDetachKill(sessionID, jobID string) (string, bool) {
279:	v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
283:	e := v.(*stallDetachEntry)
293:	return fmt.Sprintf("detached job %s (%s) stop requested; its final output is kept under job_output", jobID, e.toolName), true
298:	stallDetachJobs.Delete(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
301:// stallDetachCleanupSession drops a session's pending entries and finished
303:func stallDetachCleanupSession(sessionID string) {
305:	stallDetachPending.Range(func(k, v any) bool {
307:			stallDetachPending.Delete(k)
311:	stallDetachJobs.Range(func(k, v any) bool {
315:		e := v.(*stallDetachEntry)
320:			stallDetachJobs.Delete(k)
```

### `internal/agent/turn_stall_tool_test.go` (21 hits)

```
22://     the controller serves job_output output and job_kill on that id.
25://     job-id plumbing through the same controller job_output/job_kill call.
50:		<-ctx.Done() // block until job_kill cancels the run
56:	tool := &stallDetacherTool{inner: inner}
78:	if jobID != "stall-"+callID {
79:		t.Fatalf("job id = %q, want stall-%s", jobID, callID)
86:		for _, want := range []string{"slow_tool", "still running", jobID, "job_output", "job_kill"} {
104:	// job_output sees a running job (controller is the one the tool calls).
105:	text, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
107:		t.Fatalf("job_output ok=%v done=%v, want true/false on a live job", ok, done)
110:		t.Fatal("job_output returned empty text for a live detached job")
113:	// job_kill stops it; the final output stays readable via job_output.
114:	killText, kok := stallDetachJobController{}.StallDetachKill(sessionID, jobID)
116:		t.Fatal("job_kill did not claim the detached job id")
119:		t.Fatal("job_kill returned empty text")
123:		_, d, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
128:			t.Fatal("job_kill did not stop the detached job")
132:	final, d, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
134:		t.Fatalf("job_output after kill = %q done=%v ok=%v", final, d, ok)
150:		tool := &stallDetacherTool{inner: inner}
159:		if _, pending := stallDetachPending.Load(stallDetachKey(sessionID, callID)); pending {
```

### `internal/agent/turn_stall_zero_timeout_test.go` (1 hits)

```
185:	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
```

### `internal/agent/work_job.go` (26 hits)

```
3:// asyncJobState (former async_job_registry.go) and subAgentOutcomeEntry
67:// on the asyncJob itself and are not duplicated here. Assembled into an
80:// asyncJob is one work-ledger entry, keyed by (owner, toolCallID).
81:type asyncJob struct {
87:	cli          bool   // origin, verbatim today's asyncJobState.cli -- delivery routing unchanged (see deliverLocked)
109:	// delegation (state still phaseRunning, indexed in workLedger.byChild),
111:	// workLedger.armDelegation's doc for why that is the end of a model
115:	// workLedger.setShellID once asyncTool.awaitShell learns it from the
117:	// run_command/agent/agentic_fetch jobs). Lets job_kill/job_output resolve
119:	// workLedger.ResolveJobShellID (task #1053).
123:	// unspecified): its only consumer is the goroutine blocked in awaitSync
127:	// when this job's outcome is ready for awaitSync to read.
140:	// (doc sec.3.1/DUR-1): set by workLedger.transition while its retry loop
143:	// workLedger.transition's doc.
145:	// shutdownCancelled is set by workLedger.close() right before it cancels
147:	// by process shutdown" is workLedger.closed AND this flag, checked inside
176:	// killRequested is set (under workLedger.mu) the instant MarkJobStopped/
180:	// closes the true-concurrency window where a second job_kill call could
186:	// bash job_id, job_kill.go acts on the verdict (B11): NotFound and
191:	// outputBuf is set by workLedger.setRunCommandBuffer once a run_command
213:	// inlinePending is set, under workLedger.mu, by awaitInline when THIS
236:// called under workLedger.mu ONLY from workLedger.transition (work_ledger_
240:// whichever caller holds workLedger.mu first for this job wins; every later
245:// change the SAME field of the SAME *asyncJob under the SAME mutex, so "the
253:func (j *asyncJob) transitionToTerminal(state jobPhase, result jobResult) bool {
262:	// before the row itself is terminal in the DB. Called under workLedger.mu,
```

### `internal/agent/work_ledger.go` (77 hits)

```
1:// workLedger is the single in-memory work registry (phase 1 of the async
2:// structured-concurrency rewrite). It replaces asyncJobRegistry (former
5:// lives in work_ledger_delegation.go, the plain-job half here.
24:const maxAsyncJobsPerSession = 50
44:	// PLAIN job (bash/run_command) job_kill call (task #1023 §2.2) -- never
47:	// contract's "stopped (job_kill)" wording and NoticeKind.
50:	// Stop (causeSessionCancel), NOT job_kill (Stopped above). Step 3: a
62:	// recovery writes the DB directly and never touches workLedger's memory.
71:	// finish/timeout-terminated/delegation-release, false for Stop/job_kill/
87:	jobs    map[string]*asyncJob
103:// workLedger is the single owner of in-memory work state: plain async jobs
105:// mutex and one per-job state machine (asyncJob.transitionToTerminal).
106:type workLedger struct {
110:	// session they ran in, FIFO per child. See work_ledger_delegation.go.
111:	byChild   map[string][]*asyncJob
117:	// isolated ledger test that constructs a workLedger by hand without
118:	// also calling newTestAsyncJobStore, which must not start a non-sync
120:	store *session.AsyncJobStore
131:	// jobKillRepend overrides the store as the target of a job_kill row
137:	// work_ledger_delegation.go) and recheckChild's DB refresh. Nil-safe:
149:	// closedCh) call itself: workLedger.close() is not guaranteed single-
154:	// timeouts is the single per-process timer service for every asyncJob
163:	// NOT a workLedger job/asyncJob -- it never appears in bySession[x].jobs,
172:func newWorkLedger(onWebDone func(AsyncCompletion)) *workLedger {
173:	return &workLedger{
175:		byChild:   make(map[string][]*asyncJob),
181:func (l *workLedger) sessionLocked(owner string) *sessionJobs {
184:		s = &sessionJobs{jobs: make(map[string]*asyncJob), changed: make(chan struct{}, 1)}
191:// (owner, toolCallID) key. An executor holds its own *asyncJob for life; the
195:func (l *workLedger) currentLocked(job *asyncJob) bool {
205:func (l *workLedger) dropLocked(job *asyncJob) {
236:// goroutine blocked in awaitSync, so it is never routed to the ready queue
254:func (l *workLedger) Start(owner, toolCallID, input, toolName, childSession string, cli, sync bool, timeout *TimeoutSpec, cancel context.CancelFunc) (*asyncJob, bool, error) {
271:	if running := runningJobsLocked(s); running >= maxAsyncJobsPerSession {
273:		// snapshot is built under the same lock hold (work_ledger_cap.go).
276:		return nil, false, newAsyncCapError(running, maxAsyncJobsPerSession, snap)
292:			Owner: owner, ToolCallID: toolCallID, Kind: asyncJobKindFor(toolName),
303:		// call (work_ledger_transition.go). Meaningless when claimExisting
330:	job := &asyncJob{
372:func (l *workLedger) deliverLocked(owner string, job *asyncJob) (AsyncCompletion, bool) {
382:		// A sync job's only consumer is the goroutine blocked in awaitSync
445:// work_ledger_announce.go): only the job's own tagged started result ever
454:func (l *workLedger) acknowledged(job *asyncJob) {
469:				if errors.Is(err, session.ErrAsyncJobGone) {
484:// transaction, work_ledger_announce.go) -- the durable half differs between
491:func (l *workLedger) finishAcknowledgeLocally(job *asyncJob) {
514:func (l *workLedger) abort(job *asyncJob) {
555:func (l *workLedger) setShellID(job *asyncJob, shellID string) {
572:// Task #1053: before this, job_kill/job_output could never be called on a
576:func (l *workLedger) ResolveJobShellID(owner, jobID string) (string, error) {
581:	var job *asyncJob
648:// transition BEFORE the caller (job_kill's tool wrapper) actually kills the
653:// Task #1063 (step 6): the return value IS the job_kill tool's own final
656:// (job_kill)" notice text, so job_kill never again answers with a content-
665:// job_kill exactly what happened, so it never has to guess:
666://   - JobStopStopped: this call's own transition won; job_kill kills the
669://     another cause; text is worded from that COMMITTED row and job_kill must
672://     concurrent job_kill already claimed it via killRequested, or another
673://     transition is mid-flight); text is empty and job_kill refuses with the
680:func (l *workLedger) MarkJobStopped(owner, toolCallID string) (text, claimID string, verdict tools.JobStopVerdict) {
686:	var job *asyncJob
712:		// (job_kill)" outcome via the OLD memory-only path -- otherwise a
713:		// blocked awaitSync caller silently loses that outcome. A sync job's
740:func (l *workLedger) setRunCommandBuffer(job *asyncJob, buf tools.LiveOutputBuffer) {
753:func (l *workLedger) RunCommandOutput(owner, jobID string, cursor int64) (string, bool, int64, error) {
759:	var job *asyncJob
785:// Task #1063 (step 6): on success, text is job_kill's own final answer --
787:// FormatAsyncCompletion(Stopped: true) -- so job_kill returns it directly
797:func (l *workLedger) StopRunCommandJob(owner, jobID string) (text, claimID string, err error) {
802:	var job *asyncJob
865:// "stopped (job_kill)" result itself. That is gone -- MarkJobStopped/
868:// call arrives for a job_kill'd job, the job is already terminal and
877:func (l *workLedger) finish(job *asyncJob, result jobResult) {
928:func (l *workLedger) running(sessionID string) bool {
939:// latch inside transition (see work_ledger_transition.go) suppresses its
956:func (l *workLedger) close() {
959:	var toCancel []*asyncJob
```

### `internal/agent/work_ledger_abort_cancelled_test.go` (10 hits)

```
1:// Abort vs a Stop-cancelled durable row (#1266): DeleteUnannouncedAsyncJob is
7:// Revert-checks: dropping the state condition from DeleteUnannouncedAsyncJob
19:// TestWorkLedger_AbortKeepsStopCancelledRow covers (a): cancelSession commits
22:func TestWorkLedger_AbortKeepsStopCancelledRow(t *testing.T) {
24:	store := newTestAsyncJobStore(t)
25:	l := newWorkLedger(nil)
55:// TestWorkLedger_AbortStillDeletesUnannouncedRunningRow covers (b) ASYNC-05:
58:func TestWorkLedger_AbortStillDeletesUnannouncedRunningRow(t *testing.T) {
60:	store := newTestAsyncJobStore(t)
61:	l := newWorkLedger(nil)
```

### `internal/agent/work_ledger_ack_gate_test.go` (29 hits)

```
6:// TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce already pins for
30:// TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction pins the
32:func TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction(t *testing.T) {
34:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
36:	l := newWorkLedger(nil)
56:// TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate
60:// writes; this test fails the SECOND half (MarkAsyncJobAnnounced, via a row
69:// Revert-check performed: reverted acknowledgeWithMessageTx's ErrAsyncJobGone
73:// work_ledger_announce.go against git HEAD after restoring: matches the
75:func TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate(t *testing.T) {
77:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
79:	l := newWorkLedger(nil)
93:	require.False(t, handled, "ErrAsyncJobGone must fall back to the caller's own plain Create, not swallow the result")
97:	// only a single shared transaction with MarkAsyncJobAnnounced guarantees
107:	l.acknowledged(jobOf(l, "owner-1", "call-1")) // tolerates the still-gone row (MarkAnnounced's own ErrAsyncJobGone handling)
115:// TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled pins the
120:func TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled(t *testing.T) {
122:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
124:	l := newWorkLedger(nil)
138:// TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice
147:// the abort call; re-ran, passed. Diffed work_ledger_announce.go against
149:func TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice(t *testing.T) {
151:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
155:	l := newWorkLedger(nil)
176:// TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck
180:// deliverLocked guard TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce
183:func TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck(t *testing.T) {
185:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
188:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
```

### `internal/agent/work_ledger_ack_tag_test.go` (6 hits)

```
39:	l        *workLedger
40:	store    *session.AsyncJobStore
52:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
55:	f.l = newWorkLedger(nil)
66:	f.wrapped = &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: f.l}, name: "run_command"}
74:	sa := &sessionAgent{asyncJobs: f.l, messages: f.messages}
```

### `internal/agent/work_ledger_announce.go` (28 hits)

```
53:func (l *workLedger) claimAck(ctx context.Context, sessionID string, result message.ToolResult) (*asyncJob, ackTag) {
62:	var job *asyncJob
81:		// path's ErrAsyncJobGone handling.
99://   - a job_kill result that stopped a tracked job: fused into that row's
103:func (l *workLedger) persistToolResult(ctx context.Context, sessionID string, result message.ToolResult, messages message.Service, params message.CreateMessageParams) error {
141:// B15: ErrAsyncJobGone (the row was deleted out from under the job, e.g. by the owner session's cascade delete; Rerun only voids rows, it never deletes them) means
143:// message insert it had staged, since MarkAsyncJobAnnounced's 0-rows return
151:// (MarkAnnounced's own ErrAsyncJobGone handling) -- the in-memory job, if
153:// resolves TransitionGone (work_ledger_transition.go), same as any other
155:func (l *workLedger) acknowledgeWithMessageTx(ctx context.Context, job *asyncJob, messages message.Service, params message.CreateMessageParams) (msg message.Message, handled bool, err error) {
168:		if errors.Is(err, session.ErrAsyncJobGone) {
183:	// deliverLocked/onWebDone wake-hint machinery (work_ledger.go), the same
189:// jobKillRepender is the store method that puts a job_kill row whose fused
192:// (owner, claim_id -- the claim job_kill's own transition won, never the
193:// tool_call_id a later claim may have taken over, R3A-2), is 'done' by job_kill
194:// and has no notice_message_id. Satisfied by *session.AsyncJobStore (compile-time
195:// assertion below); tests inject a fake through workLedger.jobKillRepend.
202:var _ jobKillRepender = (*session.AsyncJobStore)(nil)
207:// rependJobKill re-pends the job_kill row named by meta (see
211:func (l *workLedger) rependJobKill(ctx context.Context, owner string, meta tools.JobKillResponseMetadata) {
220:			slog.Error("job_kill result was not recorded and the store cannot re-pend its row; the output stays undelivered",
233:		if errors.Is(err, session.ErrAsyncJobGone) {
238:		slog.Error("job_kill: failed to re-pend the row whose result was not recorded",
243:// persistJobKillResult is persistToolResult's job_kill branch (A3 + R2A-8):
245:// carries its result". job_kill's Transition already set delivery='done' /
247:// job_kill's own tool-result message id onto the TARGET row (named by the
248:// killed claim_id in the result's metadata, NOT job_kill's own tool_call_id nor
258:func (l *workLedger) persistJobKillResult(ctx context.Context, sessionID string, result message.ToolResult, messages message.Service, params message.CreateMessageParams) (handled bool, err error) {
```

### `internal/agent/work_ledger_await.go` (2 hits)

```
5:// awaitSync blocks until job's outcome is ready (deliverLocked closed
9:func (l *workLedger) awaitSync(ctx context.Context, job *asyncJob) (jobResult, error) {
```

### `internal/agent/work_ledger_bgshell_kill.go` (15 hits)

```
13:// bgShellStopBudget bounds job_kill's store work for a bg_shell row.
16:// StopBackgroundShellRow implements tools.BGShellStopper (#1189): job_kill on a
20:// The row's terminal "cancelled" transition (notice_kind job_kill,
22:// committed BEFORE job_kill kills the process. That order closes two defects:
38:func (l *workLedger) StopBackgroundShellRow(owner, shellID string) (text, claimID string, verdict tools.JobStopVerdict) {
73:		slog.Warn("job_kill: timed out waiting for the background-arrival gate",
95:			Owner: owner, ToolCallID: shellID, State: "cancelled", NoticeKind: "job_kill",
101:		// The row could not be written: let job_kill take its older path (kill
103:		slog.Error("job_kill: failed to cancel the background-shell row", "session_id", owner, "shell_id", shellID, "err", err)
109:			// B9-6: job_kill's result is this shell's answer, so its
131:// cancelled by job_kill: its completion must then produce neither a wake
132:// notice nor an auto-resume slot, because the job_kill result IS its answer.
134:// persistBGShellCompletion, after the gate a winning job_kill decision holds.
136:	if c.asyncJobs == nil || c.asyncJobs.store == nil {
141:	row, err := c.asyncJobs.store.Get(ctx, sessionID, shellID)
```

### `internal/agent/work_ledger_bgshell_kill_race_test.go` (10 hits)

```
3:// B9-7: a natural exit racing job_kill must deliver EXACTLY ONE outcome:
17:// (before bgArrival) while the row is still running, then job_kill arrives.
30:	coord.asyncJobs.coord = coord
64:	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
65:	// The process had already exited when job_kill arrived: the kill defers to
81:// Order 2, job_kill first: the row is cancelled while the process still runs;
91:	text, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
101:	require.Empty(t, notices, "a job_kill-stopped shell ships only the stopped answer")
104:// A completion callback that lost the race to job_kill (the row is already
113:	_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
```

### `internal/agent/work_ledger_bgshell_kill_test.go` (32 hits)

```
4:// job_kill on a shell that has a durable bg_shell row but no ledger job must
20:func newBGShellKillFixture(t *testing.T) (*coordinator, *session.AsyncJobStore, string) {
23:	coord.asyncJobs.coord = coord
43:	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
51:	require.Equal(t, "job_kill", row.NoticeKind)
52:	require.Equal(t, "done", row.Delivery, "job_kill's own tool result is the answer; the row is never pulled")
58:// After job_kill's cancel, the process's own exit must not write the row again
70:	_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
86:	require.Empty(t, notices, "a job_kill-stopped shell must not wake the session with a completion notice")
93:		coord.asyncJobs.coord = coord
96:		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
101:		coord.asyncJobs.coord = coord
104:		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", "sh-elsewhere")
112:		coord.asyncJobs.coord = coord
117:		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
126:		text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
136:// The real tool over the real ledger, store and shell: a raw shell_id job_kill
140:// REVERT CHECK: dropping the tryStopBGShellRow call from job_kill's raw
149:	tool := tools.NewJobKillTool(coord.asyncJobs, coord.asyncJobs, coord.background)
175:// id collision): job_kill must not cancel that row or kill anything.
179:// want JobStopNotFound: job_kill cancelled another process's running row,
183:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
184:	registry := newWorkLedger(nil)
188:	coord := &coordinator{asyncJobs: registry, background: mgr}
189:	coord.asyncJobs.coord = coord
193:	res, err := conn.ExecContext(context.Background(), `UPDATE async_jobs SET host_id = 'other-rush-host' WHERE owner_session_id = 'session' AND tool_call_id = ?`, sh.ID)
198:	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
219:// B9-6: job_kill releases the shell's completion hold at once: the cancelled
231:	coord.asyncJobs.coord = coord
239:	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow(child, sh.ID)
246:		"job_kill's cancelled row is the answer: no completion may stay pending")
248:	require.True(t, coord.asyncJobs.childScopeDrained(child),
```

### `internal/agent/work_ledger_cap.go` (5 hits)

```
1:// Cap admission for workLedger.Start: the per-session async job limit counts
36:// the model can free slots with job_kill instead of waiting. asyncTool.Run
66:func (l *workLedger) capSnapshotLocked(s *sessionJobs) []asyncCapJob {
161:// forward is freeing slots (job_kill, timers first) or ending the turn --
174:	b.WriteString("- free slots: job_kill the jobs you no longer need, timers first\n")
```

### `internal/agent/work_ledger_cap_test.go` (37 hits)

```
1:// Cap admission for workLedger.Start (ASYNC-12, work_ledger_cap.go): the
2:// per-session limit of maxAsyncJobsPerSession counts only NON-terminal jobs.
28:func capStart(t *testing.T, l *workLedger, owner, toolCallID, input string) {
37:func capOwnerJobCount(l *workLedger, owner string) int {
46:// TestWorkLedgerCap_AckedInstantJobsNeverAccumulate pins the ack-gate's
54:func TestWorkLedgerCap_AckedInstantJobsNeverAccumulate(t *testing.T) {
57:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
58:	l.store = newTestAsyncJobStore(t)
63:	// "started" tool result (work_ledger.go's finishAcknowledgeLocally).
83:	rows, err := l.store.ListAsyncJobsForOwner(context.Background(), owner)
94:// TestWorkLedgerCap_UnacknowledgedTerminalDoesNotHoldSlot pins ASYNC-12's
100:func TestWorkLedgerCap_UnacknowledgedTerminalDoesNotHoldSlot(t *testing.T) {
103:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
104:	l.store = newTestAsyncJobStore(t)
139:	rows, err := l.store.ListAsyncJobsForOwner(context.Background(), owner)
149:// TestWorkLedgerCap_RunningJobsStillCapped pins that the ASYNC-12 fix did not
155:func TestWorkLedgerCap_RunningJobsStillCapped(t *testing.T) {
157:	l := newWorkLedger(func(AsyncCompletion) {})
158:	l.store = newTestAsyncJobStore(t)
181:	require.Equal(t, maxAsyncJobsPerSession, capErr.Limit)
186:	rows, err := l.store.ListAsyncJobsForOwner(ctx, owner)
207:// TestWorkLedgerCap_RefusalNamesTimers pins the refusal's guidance: with 30
211:// tells the model to free slots with job_kill and end its turn instead of
213:func TestWorkLedgerCap_RefusalNamesTimers(t *testing.T) {
215:	l := newWorkLedger(func(AsyncCompletion) {})
216:	l.store = newTestAsyncJobStore(t)
236:	require.Equal(t, maxAsyncJobsPerSession, capErr.Running)
237:	require.Equal(t, maxAsyncJobsPerSession, capErr.Limit)
248:	for _, want := range []string{"50 running", "30 of them", "job_kill", "NO tool call", "ask_question"} {
272:	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Running)
273:	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Limit)
287:	l := newWorkLedger(func(AsyncCompletion) {})
288:	l.store = newTestAsyncJobStore(t)
304:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: l}, name: "run_command"}
313:	require.Contains(t, resp.Content, "job_kill")
319:	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Running)
320:	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Limit)
```

### `internal/agent/work_ledger_claim_id_test.go` (10 hits)

```
19:// TestWorkLedger_StartCapturesClaimIDMatchingTheStore pins the wiring: Start
23:func TestWorkLedger_StartCapturesClaimIDMatchingTheStore(t *testing.T) {
25:	l := newWorkLedger(nil)
26:	store := newTestAsyncJobStore(t)
38:// TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure pins
53:// re-ran, passed. Diffed work_ledger_transition.go against git HEAD after
55:func TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure(t *testing.T) {
58:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
59:	store := newTestAsyncJobStore(t)
71:	// external/detached stale executor whose own in-memory *asyncJob still
```

### `internal/agent/work_ledger_delegation.go` (26 hits)

```
1:// Delegation half of workLedger: parking a delegated sub-agent's (`agent`/
5:// plain half (work_ledger.go) already tracks, so the "two records for one
57:func (l *workLedger) armDelegation(job *asyncJob, captured jobResult) {
91:// latch (see asyncJob.transitionToTerminal's doc).
95:// text. The release itself now goes through workLedger.transition
98:func (l *workLedger) recheckChild(childSessionID string) {
162:			// sec.3.1) -- old memory-only path, so awaitSync still unblocks
194:func oldestArmedLocked(entries []*asyncJob) *asyncJob {
221:func (l *workLedger) childScopeDrained(childID string) bool {
247:	// own workLedger job map plus IsSessionBusy). A child whose own async
271:func (l *workLedger) childScopeOpenAcrossProcesses(childID string) bool {
290:	job          *asyncJob
303:// Replaces asyncJobRegistry.cancelSession + coordinator.
320:// awaitSync unblocks; a bare map delete for a plain job).
321:func (l *workLedger) cancelSession(sessionID string) {
363:// original inline loop, factored out so work_ledger_rerun.go's Rerun-scoped
364:// stop can share it exactly): sync delegations unblock awaitSync in memory,
371:func (l *workLedger) stopTargets(targets []cancelSessionTarget) {
375:			// Old memory-only path: unblocks awaitSync via deliverLocked's
417:func (l *workLedger) treeSessionIDs(root string) []string {
452:func (l *workLedger) cancelTree(root string) []string {
463:func (l *workLedger) gcChildLocked(childID string) {
481:func (l *workLedger) hasParked() bool {
497:func (l *workLedger) parkedParentSessions() []string {
524:func (l *workLedger) parkedChildSessions() []string {
542:func (l *workLedger) hasArmedOrDriver(childID string) bool {
```

### `internal/agent/work_ledger_delegation_test.go` (88 hits)

```
3:// Delegation half of workLedger: parking a delegated sub-agent's (`agent`/
6:// merged workLedger API (park+claim+emit -> armDelegation+recheckChild;
52:// and release a delegated sub-agent outcome: a bare workLedger wired to
59:	coord.asyncJobs = newWorkLedger(onWebDone)
60:	coord.asyncJobs.store = newTestAsyncJobStore(t)
61:	coord.asyncJobs.coord = coord
76:	coord.asyncJobs = newWorkLedger(onWebDone)
77:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
78:	coord.asyncJobs.store = store
79:	coord.asyncJobs.coord = coord
100:	_, err := coord.asyncJobs.store.PullJobNotices(ctx, messages, childSessionID, buildJobNoticeMessageParams)
107:	require.NoError(t, coord.asyncJobs.store.MarkReactedWithMessageUpdate(ctx, messages, childSessionID, reactionMsg))
133:	_, _, err := coord.asyncJobs.Start(parkedParentSession, parkedParentCall, "", AgentToolName, childSession, false, false, nil, func() {})
135:	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, parkedParentSession, parkedParentCall))
143:	wrapped.run(ctx, func() {}, jobOf(coord.asyncJobs, parkedParentSession, parkedParentCall), parkedParentSession, childSession, fantasy.ToolCall{
155:func startChildOwnedJob(t *testing.T, l *workLedger, sessionID, toolCallID string, cli bool) {
168:	coord.asyncJobs.finish(jobOf(coord.asyncJobs, completion.SessionID, completion.ToolCallID), jobResult{content: completion.Content, isError: completion.IsError})
179:// TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending pins property 1:
182:func TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending(t *testing.T) {
188:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)
192:	require.True(t, coord.asyncJobs.hasParked(),
194:	require.True(t, coord.asyncJobs.running(parkedChildSession),
200:// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain pins property 2:
203:func TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain(t *testing.T) {
208:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
210:	require.True(t, coord.asyncJobs.hasParked())
234:	require.False(t, coord.asyncJobs.hasParked(),
238:// TestWorkLedger_FailedChildJobDeliveredOnceAsFailure pins property 3: when
241:func TestWorkLedger_FailedChildJobDeliveredOnceAsFailure(t *testing.T) {
247:	startChildOwnedJob(t, coord.asyncJobs, failedChildSession, parkedChildJob, true)
249:	require.True(t, coord.asyncJobs.hasParked())
259:	// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain -- the child's own
275:// TestWorkLedger_ConcurrentRecheckDeliversOnce -- the shape of the real
286:// TestWorkLedger_ConcurrentRecheckDeliversOnce pins the exactly-once
294:func TestWorkLedger_ConcurrentRecheckDeliversOnce(t *testing.T) {
314:	require.True(t, coord.asyncJobs.hasParked())
325:			coord.asyncJobs.recheckChild(child.ID)
338:	require.False(t, coord.asyncJobs.hasParked(),
342:// TestWorkLedger_CancelReleasesArmedDelegation pins property 4a: a cancel
346:func TestWorkLedger_CancelReleasesArmedDelegation(t *testing.T) {
352:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
354:	require.True(t, coord.asyncJobs.hasParked())
366:	require.False(t, coord.asyncJobs.hasParked(),
370:// TestWorkLedger_CancelSurvivesFinishedChildTurn pins the cancel path against
381:// asyncJobRegistry.cancelSession for a plain job -- it must NOT also surface
383:func TestWorkLedger_CancelSurvivesFinishedChildTurn(t *testing.T) {
398:	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
400:	require.True(t, coord.asyncJobs.hasParked())
406:	coord.asyncJobs.cancelSession(child.ID)
417:	require.False(t, coord.asyncJobs.hasParked(),
419:	require.False(t, coord.asyncJobs.running(child.ID),
423:// TestWorkLedger_ResumeAfterNoticeDoesNotReemit pins property 4b: once a
427:func TestWorkLedger_ResumeAfterNoticeDoesNotReemit(t *testing.T) {
438:	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
440:	require.True(t, coord.asyncJobs.hasParked())
465:	_, _, err = coord.asyncJobs.Start(parkedParentSession, secondParentCall, "", AgentToolName, child.ID, false, false, nil, func() {})
467:	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, parkedParentSession, secondParentCall))
474:	wrapped.run(ctx, func() {}, jobOf(coord.asyncJobs, parkedParentSession, secondParentCall), parkedParentSession, child.ID, fantasy.ToolCall{
483:	require.False(t, coord.asyncJobs.hasParked())
486:// TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds pins the negative busy
489:func TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds(t *testing.T) {
500:	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
502:	require.True(t, coord.asyncJobs.hasParked())
514:	require.True(t, coord.asyncJobs.hasParked())
528:	require.False(t, coord.asyncJobs.hasParked())
531:// TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce is new coverage
548:func TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce(t *testing.T) {
556:		startChildOwnedJob(t, coord.asyncJobs, "child-race", "child-job", false)
558:		_, _, err := coord.asyncJobs.Start("parent-race", "call-race", "", AgentToolName, "child-race", false, false, nil, nil)
560:		coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, "parent-race", "call-race"))
561:		coord.asyncJobs.armDelegation(jobOf(coord.asyncJobs, "parent-race", "call-race"), jobResult{content: "child final answer"})
562:		require.True(t, coord.asyncJobs.hasParked())
567:		coord.asyncJobs.finish(jobOf(coord.asyncJobs, "child-race", "child-job"), jobResult{content: "child job ok"})
576:			coord.asyncJobs.recheckChild("child-race")
581:			coord.asyncJobs.cancelSession("parent-race")
596:		require.False(t, coord.asyncJobs.hasParked(), "iteration %d", i)
600:// TestWorkLedger_ChildScopeClosesWithoutTicker is phase 3 step 0 (spec §3.3,
603:// attributed to it): the exact TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain
608:func TestWorkLedger_ChildScopeClosesWithoutTicker(t *testing.T) {
612:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
614:	require.True(t, coord.asyncJobs.hasParked())
624:	// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain.
632:	require.False(t, coord.asyncJobs.hasParked())
635:// TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation pins phase 3
649:func TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation(t *testing.T) {
663:	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
665:	require.True(t, coord.asyncJobs.hasParked())
673:	coord.asyncJobs.finish(jobOf(coord.asyncJobs, parkedChildSession, parkedChildJob), jobResult{content: "done"})
```

### `internal/agent/work_ledger_driver.go` (2 hits)

```
8:// tests) reports false. See session.AsyncJobStore.ForeignLiveDriver.
9:func (l *workLedger) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
```

### `internal/agent/work_ledger_durable_test.go` (49 hits)

```
3:// job's terminal state, backed by a real temp-SQLite AsyncJobStore. Covers
8:// TestAsyncJobStore_TransitionWonThenLostThenGone) -- the CAS race it
9:// describes is a cross-host/cross-process race; within one workLedger the
30:// lowBusyTimeoutStore builds an AsyncJobStore over a SEPARATE raw connection
33:// the driver -- the only way to actually exercise workLedger.transition's
36:func lowBusyTimeoutStore(t *testing.T, dataDir string) *session.AsyncJobStore {
45:	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
91:// TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits is DUR-1a: a
102:// work_ledger_transition.go against git HEAD after restoring: no diff.
103:func TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits(t *testing.T) {
117:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
150:// TestWorkLedger_InFlightLatchSkipsSecondTriggerWithoutBlocking pins the
160:func TestWorkLedger_InFlightLatchSkipsSecondTriggerWithoutBlocking(t *testing.T) {
172:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
233:// TestWorkLedger_TerminalCausesRecordOwnStateNoticeKindAndWake is DUR-1c:
234:// cancel / timeout / job_kill each record their own state + notice_kind +
238:func TestWorkLedger_TerminalCausesRecordOwnStateNoticeKindAndWake(t *testing.T) {
241:		act            func(l *workLedger)
247:			name: "job_kill via StopRunCommandJob",
248:			act: func(l *workLedger) {
252:			wantState: "cancelled", wantNoticeKind: "job_kill", wantWake: 0,
256:			act: func(l *workLedger) {
268:			act: func(l *workLedger) {
277:			store := newTestAsyncJobStore(t)
278:			l := newWorkLedger(nil)
306:// TestWorkLedger_ShutdownCausedCancellationLeavesRowRunningWritesNoNotice is
315:// work_ledger_transition.go against git HEAD: no diff.
316:func TestWorkLedger_ShutdownCausedCancellationLeavesRowRunningWritesNoNotice(t *testing.T) {
318:	store := newTestAsyncJobStore(t)
320:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
346:// TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed is the other
350:func TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed(t *testing.T) {
352:	store := newTestAsyncJobStore(t)
354:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
378:// TestWorkLedger_StartFailsClosedWhenStoreUnavailable is DUR-8: an
392:func TestWorkLedger_StartFailsClosedWhenStoreUnavailable(t *testing.T) {
398:	l := newWorkLedger(nil)
414:// TestWorkLedger_StartSyncJobWorksWithoutStore pins the other half of DUR-8:
417:func TestWorkLedger_StartSyncJobWorksWithoutStore(t *testing.T) {
419:	l := newWorkLedger(nil)
428:	res, err := l.awaitSync(context.Background(), job)
433:// TestWorkLedger_StartClaimIdempotencyThroughLedger complements the store-
438:func TestWorkLedger_StartClaimIdempotencyThroughLedger(t *testing.T) {
440:	store := newTestAsyncJobStore(t)
441:	l := newWorkLedger(nil)
454:// TestWorkLedger_StartASYNC01ConflictRefusedBeforeStarted pins ASYNC-01
458:func TestWorkLedger_StartASYNC01ConflictRefusedBeforeStarted(t *testing.T) {
460:	store := newTestAsyncJobStore(t)
461:	l := newWorkLedger(nil)
```

### `internal/agent/work_ledger_identity_test.go` (13 hits)

```
2:// retirement (R2B-12): an executor reports its result for the *asyncJob it
31:	store := newTestAsyncJobStore(t)
55:	l := newWorkLedger(func(c AsyncCompletion) { completed <- c })
57:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: l}, name: "run_command"}
97:// TestWorkLedger_SupersededExecutorWritesNothingByKey pins the remaining
100:func TestWorkLedger_SupersededExecutorWritesNothingByKey(t *testing.T) {
102:	l := newWorkLedger(nil)
103:	l.store = newTestAsyncJobStore(t)
118:	require.Empty(t, job2.shellID, "a superseded executor must not name its shell on the new job (job_kill would kill the wrong shell)")
123:// TestWorkLedger_GoneDelegationLeavesNoParkedEntry pins R2B-12: a delegation
130:func TestWorkLedger_GoneDelegationLeavesNoParkedEntry(t *testing.T) {
132:	l := newWorkLedger(nil)
133:	store := newTestAsyncJobStore(t)
```

### `internal/agent/work_ledger_inline.go` (11 hits)

```
5:// (session.AsyncJobStore.AnnounceInlineResult) -- instead of today's
8:// completed/failed outcome happens inside the window (Stop, job_kill, a
70:	if c != nil && c.asyncJobs != nil {
72:			c.asyncJobs.inlineWindow = inlineWindow
74:			c.asyncJobs.inlineWindow = 0
90:func (l *workLedger) awaitInline(ctx context.Context, job *asyncJob) (jobResult, bool) {
133:func (l *workLedger) dropStoppedInlineLocked(job *asyncJob) {
152:func (l *workLedger) finishInlineLocally(job *asyncJob) {
175://   - ErrAsyncJobGone (the row was deleted out from under the job): the
186:func (l *workLedger) acknowledgeInlineResult(ctx context.Context, job *asyncJob, messages message.Service, params message.CreateMessageParams) (message.Message, bool, error) {
195:		if errors.Is(err, session.ErrAsyncJobGone) {
```

### `internal/agent/work_ledger_inline_test.go` (16 hits)

```
4:// (session.AsyncJobStore.AnnounceInlineResult) marks the row announced AND
6:// tests drive the real asyncTool -> workLedger -> turnStream.onToolResult
7:// path against a real SQLite store, like work_ledger_ack_tag_test.go.
29:	l        *workLedger
30:	store    *session.AsyncJobStore
55:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
62:	f.l = newWorkLedger(func(AsyncCompletion) { f.webDone.Add(1) })
79:	f.wrapped = &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: f.l}, name: "run_command"}
87:	sa := &sessionAgent{asyncJobs: f.l, messages: f.messages}
114:func (f *inlineFixture) row(t *testing.T) db.AsyncJob {
239:	_, err := f.conn.ExecContext(context.Background(), `CREATE TRIGGER inline_fail_delivery BEFORE UPDATE ON async_jobs
267:	_, err := f.conn.ExecContext(context.Background(), `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
449:// final, and neither the job_kill re-pend nor any other writer resurrects
459:	_, err := f.conn.ExecContext(context.Background(), `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
462:	// The only live-process writer that re-pends a done row is the job_kill
489:	f2.l.coord = &coordinator{asyncJobs: f2.l, subAgentDrivers: newSubAgentDriverRegistry()}
```

### `internal/agent/work_ledger_job_kill_result_test.go` (8 hits)

```
1:// job_kill result recording (A3 fusion + R2A-8 tool side): the tool result of
2:// a job_kill that stopped a tracked job is fused into that row's
46:	l        *workLedger
58:// MarkJobStopped and builds the job_kill tool result the tool would return.
61:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
63:	f.l = newWorkLedger(nil)
188:	l := newWorkLedger(nil)
189:	store := newTestAsyncJobStore(t)
```

### `internal/agent/work_ledger_job_kill_reuse_test.go` (15 hits)

```
2:// job_kill result path: a provider that numbers calls per response issues a new
3:// "call_0" while the previous "call_0" job's job_kill is still running. The new
19:// reusedKillFixture stops J1 ("call_0") with job_kill, then starts J2 under the
20:// same id before job_kill's own result is persisted.
24:	archived db.AsyncJob // J1 after the new claim archived it
25:	second   *asyncJob   // J2
30:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
33:	base.l = newWorkLedger(nil)
47:	// The model's next response reuses "call_0" while job_kill is still running.
62:func (f *reusedKillFixture) rows(t *testing.T) []db.AsyncJob {
64:	rows, err := f.q.ListAsyncJobsForOwner(context.Background(), "owner-1")
69:func (f *reusedKillFixture) row(t *testing.T, claimID string) db.AsyncJob {
77:	return db.AsyncJob{}
96:	require.True(t, killed.NoticeMessageID.Valid, "DUR-11: the killed row must name the job_kill result message")
118:	require.Equal(t, "pending", killed.Delivery, "the unnamed job_kill row must be deliverable by the ordinary pull")
```

### `internal/agent/work_ledger_job_kill_terminal_test.go` (14 hits)

```
3:// job_kill must answer from the COMMITTED row and must not touch the shell
19:// TestWorkLedger_MarkJobStopped_AlreadyTerminalAnswersFromCommittedRow pins
23:// the committed row, never "stopped (job_kill)", and must not mark it
25:func TestWorkLedger_MarkJobStopped_AlreadyTerminalAnswersFromCommittedRow(t *testing.T) {
27:	l := newWorkLedger(nil)
28:	l.store = newTestAsyncJobStore(t)
39:	require.NotContains(t, text, "was stopped (job_kill)")
48:// job_kill tool against the REAL ledger and a REAL background shell: the job
50:// lingers in the manager -- exactly the state B11 describes. job_kill must
53:// Revert-check performed: made job_kill.go treat JobStopAlreadyTerminal like
67:	l := newWorkLedger(nil)
68:	l.store = newTestAsyncJobStore(t)
81:	require.NotContains(t, resp.Content, "was stopped (job_kill)")
83:	require.False(t, bgShell.IsDone(), "job_kill must not kill a shell whose job already reached a terminal state")
```

### `internal/agent/work_ledger_job_kill_test.go` (78 hits)

```
1:// Unit tests for task #1023 (wakes stage 2): job_kill marking a job
46:// TestWorkLedger_MarkJobStopped_BashProducesDistinctCancelledOutcome pins
50:// contract's exact "stopped (job_kill)" wording, not a double-wrapped
61:// (see work_ledger_timeout.go's capturePartial for the read path). The
69:// Restored the step-2 versions; re-ran, passed. Diffed work_ledger.go
71:func TestWorkLedger_MarkJobStopped_BashProducesDistinctCancelledOutcome(t *testing.T) {
74:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
75:	l.store = newTestAsyncJobStore(t)
90:	require.False(t, got[0].IsError, "a job_kill stop is not a failure")
96:	require.Contains(t, text, "was stopped (job_kill)")
107:// TestWorkLedger_MarkJobStopped_RunCommandUsesLiveBufferForPartialOutput
112:func TestWorkLedger_MarkJobStopped_RunCommandUsesLiveBufferForPartialOutput(t *testing.T) {
115:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
116:	l.store = newTestAsyncJobStore(t)
138:// TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome extends ASYNC-03's
139:// race coverage with MarkJobStopped racing finish -- a job_kill call landing
143:// B11: additionally asserts job_kill's OWN answer (text/ok, not just what
145:// code always answered "stopped (job_kill)" regardless, because MarkJobStopped
155:// contained "was stopped (job_kill)"). Restored the outcome-based answer;
156:// re-ran, passed. Diffed work_ledger.go against git HEAD after restoring:
158:func TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome(t *testing.T) {
163:		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
164:		l.store = newTestAsyncJobStore(t)
191:			require.Equal(t, tools.JobStopStopped, markVerdict, "iteration %d: job_kill won the race but did not report JobStopStopped", i)
192:			require.Contains(t, markText, "was stopped (job_kill)", "iteration %d", i)
195:			require.NotEqual(t, tools.JobStopStopped, markVerdict, "iteration %d: job_kill lost the race but reported JobStopStopped", i)
196:			require.NotContains(t, markText, "was stopped (job_kill)",
197:				"iteration %d: job_kill lost the race to natural finish but its own answer still claimed it stopped the job", i)
203:// TestWorkLedger_StopRunCommandJob_CancelsAndIsIdempotent pins §1.5's
207:func TestWorkLedger_StopRunCommandJob_CancelsAndIsIdempotent(t *testing.T) {
209:	l := newWorkLedger(nil)
210:	l.store = newTestAsyncJobStore(t)
217:	require.Contains(t, stopText, "job_kill", "task #1063: the fresh stop's own answer carries the real output/wording, not a placeholder")
226:// TestWorkLedger_StopRunCommandJob_RejectsNonRunCommandJob pins the kind
229:func TestWorkLedger_StopRunCommandJob_RejectsNonRunCommandJob(t *testing.T) {
231:	l := newWorkLedger(nil)
232:	l.store = newTestAsyncJobStore(t)
241:// TestWorkLedger_RunCommandOutput_CursorSemantics pins §2.1's cursor rules
245:func TestWorkLedger_RunCommandOutput_CursorSemantics(t *testing.T) {
247:	l := newWorkLedger(nil)
248:	l.store = newTestAsyncJobStore(t)
287:// TestWorkLedger_RunCommandOutput_StillStartingRaceIsNotAnError covers the
289:// sink (setRunCommandBuffer race, §3): job_output must not error, just
291:func TestWorkLedger_RunCommandOutput_StillStartingRaceIsNotAnError(t *testing.T) {
293:	l := newWorkLedger(nil)
294:	l.store = newTestAsyncJobStore(t)
304:// TestWorkLedger_ResolveJobShellID_TypedErrors pins the typed-error routing
305:// job_kill/job_output depend on (task #1023): errors.As must find
308:func TestWorkLedger_ResolveJobShellID_TypedErrors(t *testing.T) {
310:	l := newWorkLedger(nil)
311:	l.store = newTestAsyncJobStore(t)
330:// TestWorkLedger_MarkJobStopped_RowGoesStraightToDoneNeitherDebtNorNotice
331:// pins task #1063/doc sec.3.4's wake paragraph: a job_kill'd row commits
333:// pull candidate (ListPendingAsyncJobNoticesForOwner requires
335:// requires wake=1), so job_kill produces no second notice and no turn.
340:// the step-6 version; re-ran, passed. Diffed work_ledger_transition.go
342:func TestWorkLedger_MarkJobStopped_RowGoesStraightToDoneNeitherDebtNorNotice(t *testing.T) {
344:	l := newWorkLedger(nil)
345:	store := newTestAsyncJobStore(t)
353:	require.Contains(t, text, "job_kill")
358:	require.Equal(t, "done", row.Delivery, "job_kill's row is delivered immediately, never a pull candidate")
364:	require.Empty(t, jobPulled, "a job_kill'd row must never surface as a pulled notice")
367:// TestWorkLedger_StopRunCommandJob_RowGoesStraightToDone is the run_command
369:func TestWorkLedger_StopRunCommandJob_RowGoesStraightToDone(t *testing.T) {
371:	l := newWorkLedger(nil)
372:	store := newTestAsyncJobStore(t)
380:	require.Contains(t, text, "job_kill")
389:// TestWorkLedger_MarkJobStopped_ConcurrentCallsYieldExactlyOneFreshStop pins
391:// two callers racing job_kill for the SAME job must see exactly one "fresh"
394:func TestWorkLedger_MarkJobStopped_ConcurrentCallsYieldExactlyOneFreshStop(t *testing.T) {
397:		l := newWorkLedger(nil)
398:		l.store = newTestAsyncJobStore(t)
425:		require.Equal(t, 1, freshCount, "iteration %d: exactly one concurrent job_kill must see a fresh stop", i)
429:// TestWorkLedger_StopRunCommandJob_ConcurrentCallsYieldExactlyOneFreshStop
432:func TestWorkLedger_StopRunCommandJob_ConcurrentCallsYieldExactlyOneFreshStop(t *testing.T) {
435:		l := newWorkLedger(nil)
436:		l.store = newTestAsyncJobStore(t)
469:		require.Equal(t, 1, successCount, "iteration %d: exactly one concurrent job_kill must succeed", i)
```

### `internal/agent/work_ledger_job_shell_test.go` (31 hits)

```
3:// Unit tests for task #1053: job_kill/job_output only ever learn a job's
5:// tool reports it (see work_ledger.go's setShellID/ResolveJobShellID).
14:// TestWorkLedger_ResolveJobShellID_OwnedJobResolves pins the success path:
18:func TestWorkLedger_ResolveJobShellID_OwnedJobResolves(t *testing.T) {
20:	l := newWorkLedger(nil)
21:	l.store = newTestAsyncJobStore(t)
35:// TestWorkLedger_ResolveJobShellID_ForeignSessionNotFound pins ownership: a
38:func TestWorkLedger_ResolveJobShellID_ForeignSessionNotFound(t *testing.T) {
40:	l := newWorkLedger(nil)
41:	l.store = newTestAsyncJobStore(t)
51:// TestWorkLedger_ResolveJobShellID_UnknownJobNotFound covers the plain
53:func TestWorkLedger_ResolveJobShellID_UnknownJobNotFound(t *testing.T) {
55:	l := newWorkLedger(nil)
56:	l.store = newTestAsyncJobStore(t)
62:// TestWorkLedger_ResolveJobShellID_NotACommandJob pins the agent/
64:// job_kill/job_output refuse it with a distinct message instead of silently
66:func TestWorkLedger_ResolveJobShellID_NotACommandJob(t *testing.T) {
68:	l := newWorkLedger(nil)
69:	l.store = newTestAsyncJobStore(t)
78:// TestWorkLedger_ResolveJobShellID_RunCommandRefusedClearly: run_command
81:func TestWorkLedger_ResolveJobShellID_RunCommandRefusedClearly(t *testing.T) {
83:	l := newWorkLedger(nil)
84:	l.store = newTestAsyncJobStore(t)
94:// TestWorkLedger_ResolveJobShellID_NilLedgerIsSafe: a nil *workLedger (a
95:// coordinator built without NewCoordinator, or asyncJobs unset) must not
97:func TestWorkLedger_ResolveJobShellID_NilLedgerIsSafe(t *testing.T) {
99:	var l *workLedger
105:// TestWorkLedger_SetShellID_UnknownJobIsNoOp: recording a shell id for a job
107:func TestWorkLedger_SetShellID_UnknownJobIsNoOp(t *testing.T) {
109:	l := newWorkLedger(nil)
110:	l.store = newTestAsyncJobStore(t)
```

### `internal/agent/work_ledger_launch_failure_test.go` (3 hits)

```
30:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
45:	registry := newWorkLedger(func(AsyncCompletion) {})
48:		asyncJobs: registry, permissions: panickyPermissions{}, sessions: sessions,
```

### `internal/agent/work_ledger_next_event_test.go` (4 hits)

```
5:// Originally proved against workLedger.next() (the in-memory ready-queue
20:// TestWorkLedger_WaitForHintUnblocksOnSignalNotPoll holds a hint back for
33:func TestWorkLedger_WaitForHintUnblocksOnSignalNotPoll(t *testing.T) {
35:	l := newWorkLedger(nil)
```

### `internal/agent/work_ledger_pending.go` (2 hits)

```
11:// map by its durable async_jobs row (B1). Called WITHOUT l.mu (no DB I/O
17:func (l *workLedger) durableDeliveryStatus(owner, jobID string) error {
```

### `internal/agent/work_ledger_pending_test.go` (17 hits)

```
3:// B1: job_output must never answer a false "not found ... or already
5:// (deliverLocked) while its durable async_jobs row is still
21:func dropJob(l *workLedger, owner, toolCallID string) {
29:// TestWorkLedger_ResolveJobShellID_PendingDeliveryFromDurableRow pins the
32:// which job_output turns into plain text.
36:func TestWorkLedger_ResolveJobShellID_PendingDeliveryFromDurableRow(t *testing.T) {
39:	l := newWorkLedger(nil)
40:	l.store = newTestAsyncJobStore(t)
61:// TestWorkLedger_ResolveJobShellID_DoneRowReportsDelivered pins the honest
63:// (job_kill's step 6, or the notice was pulled).
64:func TestWorkLedger_ResolveJobShellID_DoneRowReportsDelivered(t *testing.T) {
67:	l := newWorkLedger(nil)
68:	l.store = newTestAsyncJobStore(t)
87:// TestWorkLedger_ResolveJobShellID_RunningOrVoidRowKeepsNotFound pins that
91:func TestWorkLedger_ResolveJobShellID_RunningOrVoidRowKeepsNotFound(t *testing.T) {
93:	l := newWorkLedger(nil)
94:	l.store = newTestAsyncJobStore(t)
```

### `internal/agent/work_ledger_reaction.go` (11 hits)

```
21:func (l *workLedger) bumpHint(owner string) {
33:func (l *workLedger) hintSeqOf(owner string) uint64 {
47:func (l *workLedger) waitForHint(ctx context.Context, owner string, since uint64) bool {
73:func (l *workLedger) claimExternalDriver(owner string) {
85:func (l *workLedger) releaseExternalDriver(owner string) {
98:func (l *workLedger) isExternalDriver(owner string) bool {
107:func (l *workLedger) reactionDebtExists(ctx context.Context, owner string) (bool, error) {
116:func (l *workLedger) hasRunningDelegationFor(ctx context.Context, childSessionID string) (bool, error) {
124:func (l *workLedger) captureDebtSnapshot(ctx context.Context, owner string) (session.DebtSnapshot, error) {
134:func (l *workLedger) arbPace(owner string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainPace) bool {
142:func (l *workLedger) resetGate(owner string) {
```

### `internal/agent/work_ledger_rerun.go` (2 hits)

```
10:// path as cancelSession, NOT job_kill's: the job is marked stoppedBySession
24:func (l *workLedger) stopToolCallsForRerun(owner string, toolCallIDs []string) {
```

### `internal/agent/work_ledger_rerun_test.go` (11 hits)

```
21:	l := newWorkLedger(nil)
22:	store := newTestAsyncJobStore(t)
42:	l := newWorkLedger(nil)
43:	store := newTestAsyncJobStore(t)
70:	l := newWorkLedger(nil)
71:	l.store = newTestAsyncJobStore(t)
82:// job_kill's: that would be notice_kind job_kill, delivery done.)
85:// changed notice_kind to job_kill and failed the assertion.
88:	l := newWorkLedger(nil)
89:	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
95:	rows, err := db.New(conn).VoidAsyncJobsByToolCallIDs(context.Background(), db.VoidAsyncJobsByToolCallIDsParams{
```

### `internal/agent/work_ledger_review_fixes_test.go` (37 hits)

```
3:// must never wake the session (P2), and a sync job stopped via job_kill
19:// TestWorkLedger_RetryLoopStopsAfterClose pins P1: a store-write retry loop
31:// re-ran, passed. Diffed work_ledger_transition.go against git HEAD after
33:func TestWorkLedger_RetryLoopStopsAfterClose(t *testing.T) {
42:	store := session.NewAsyncJobStore(conn, dataDir, 1, "test")
45:	l := newWorkLedger(nil)
81:// TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes pins P2: a
106:// the branch; re-ran 30 iterations, passed. Diffed work_ledger_transition.go
108:func TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes(t *testing.T) {
110:		store := newTestAsyncJobStore(t)
112:		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
153:// TestWorkLedger_MarkJobStopped_SyncJobPreservesStoppedOutcome pins the P2
154:// sync job_kill regression: a sync (SDK-origin) job stopped via job_kill
155:// must still reach a well-formed "stopped (job_kill)" outcome (Stopped
159:// a blocked awaitSync caller hanging forever with no outcome at all.
163:// FAILED with a context.DeadlineExceeded error (awaitSync never unblocked,
166:func TestWorkLedger_MarkJobStopped_SyncJobPreservesStoppedOutcome(t *testing.T) {
168:	l := newWorkLedger(nil) // sync path never touches the store
177:	res, err := l.awaitSync(ctx, job)
178:	require.NoError(t, err, "a sync job_kill must still unblock awaitSync with a real outcome, not hang")
179:	require.False(t, res.isError, "a job_kill stop is not a failure")
183:// TestWorkLedger_StopRunCommandJob_SyncJobPreservesStoppedOutcome is the
185:func TestWorkLedger_StopRunCommandJob_SyncJobPreservesStoppedOutcome(t *testing.T) {
187:	l := newWorkLedger(nil)
200:	res, err := l.awaitSync(ctx, job)
201:	require.NoError(t, err, "a sync StopRunCommandJob must still unblock awaitSync with a real outcome, not hang")
206:// TestWorkLedger_StopBeforeAckDoesNotOrphanRow pins B10: Stop landing between
217:// checked by PurgeAsyncJobsOlderThan either), an orphan.
225:// once announced is actually true); re-ran, passed. Diffed work_ledger.go
227:func TestWorkLedger_StopBeforeAckDoesNotOrphanRow(t *testing.T) {
229:	store := newTestAsyncJobStore(t)
231:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
266:// TestWorkLedger_CloseDoesNotLatchOntoExecutorReturnedJob pins B9: close()
281:// Restored the fix; re-ran, passed. Diffed work_ledger.go against git HEAD
283:func TestWorkLedger_CloseDoesNotLatchOntoExecutorReturnedJob(t *testing.T) {
285:	store := newTestAsyncJobStore(t)
287:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
```

### `internal/agent/work_ledger_sync_timeout_test.go` (8 hits)

```
19:// TestWorkLedger_SyncTerminateAndWakeTimeoutYieldsTimedOutOutcomeWithPartialOutput
23:// again (which skips sync jobs) the job never became terminal: awaitSync
25:func TestWorkLedger_SyncTerminateAndWakeTimeoutYieldsTimedOutOutcomeWithPartialOutput(t *testing.T) {
27:	l := newWorkLedger(nil)
41:	res, err := l.awaitSync(ctx, job)
68:	registry := newWorkLedger(nil)
69:	registry.store = newTestAsyncJobStore(t)
72:	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
```

### `internal/agent/work_ledger_test.go` (51 hits)

```
3:// Core workLedger coverage: plain-job lifecycle (Start/finish/acknowledged/
31:// TestWorkLedger_WaitsForPersistedToolResult ports
32:// TestAsyncJobRegistryWaitsForPersistedToolResult: delivery waits for
35:// (work_ledger.go's deliverLocked), CLI-origin or not, so this observes
36:// delivery via a callback channel instead of workLedger.next().
37:func TestWorkLedger_WaitsForPersistedToolResult(t *testing.T) {
40:	l := newWorkLedger(func(c AsyncCompletion) { completed <- c })
41:	l.store = newTestAsyncJobStore(t)
73:// TestWorkLedger_WebCallbackExactlyOnce ports
74:// TestAsyncJobRegistryWebCallbackExactlyOnce: a repeated finish for an
76:func TestWorkLedger_WebCallbackExactlyOnce(t *testing.T) {
79:	l := newWorkLedger(func(got AsyncCompletion) {
83:	l.store = newTestAsyncJobStore(t)
93:// TestWorkLedger_ConcurrentFinishAndAcknowledge ports
94:// TestAsyncJobRegistryConcurrentFinishAndAcknowledge: whichever of
96:func TestWorkLedger_ConcurrentFinishAndAcknowledge(t *testing.T) {
99:	l := newWorkLedger(func(c AsyncCompletion) { completed <- c })
100:	l.store = newTestAsyncJobStore(t)
115:// TestWorkLedger_CancelAndClose ports TestAsyncJobRegistryCancelAndClose:
127:// l.next call returning not-ok) passed again, but TestWorkLedger_
128:// ShutdownCausedCancellationLeavesRowRunningWritesNoNotice (work_ledger_
133:func TestWorkLedger_CancelAndClose(t *testing.T) {
135:	l := newWorkLedger(nil)
136:	l.store = newTestAsyncJobStore(t)
153:// TestWorkLedger_StartIsIdempotentPerToolCallID pins #1038 (spec §4.2d): a
161:// `go test -run TestWorkLedger_StartIsIdempotentPerToolCallID`: FAILED at
167:func TestWorkLedger_StartIsIdempotentPerToolCallID(t *testing.T) {
169:	l := newWorkLedger(nil)
170:	l.store = newTestAsyncJobStore(t)
185:func TestWorkLedger_ReusedCallIDWithDifferentInputIsRefused(t *testing.T) {
187:	l := newWorkLedger(nil)
188:	l.store = newTestAsyncJobStore(t)
196:// TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce pins the ack-gate for
208:func TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce(t *testing.T) {
211:	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
212:	l.store = newTestAsyncJobStore(t)
237:// TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome pins ASYNC-03
243:// byte-for-byte identical to today's asyncJobRegistry.cancelSession, see
245:// in work_ledger_delegation_test.go), so racing cancel against finish for a
267:func TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome(t *testing.T) {
271:		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
272:		l.store = newTestAsyncJobStore(t)
304:// TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone pins phase 2 §4.4: a
305:// sync job's outcome is delivered ONLY through awaitSync/job.done, never
314:func TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone(t *testing.T) {
317:	l := newWorkLedger(func(AsyncCompletion) { webDoneCalls++ })
318:	l.store = newTestAsyncJobStore(t)
333:	result, err := l.awaitSync(t.Context(), job)
338:// TestWorkLedger_ConsumeNoticeKeepsMapAndKeysInLockstep and
339:// TestWorkLedger_NoticeReRecordedAfterConsumeDoesNotDuplicateKey (both pinning
343:// phase4-durable-core.md sec.3.4) -- the durable async_jobs/session_notices
```

### `internal/agent/work_ledger_teststore_test.go` (7 hits)

```
5:// per test, matches the existing newWorkLedger(nil) isolation model: each
19:// newTestAsyncJobStore builds an AsyncJobStore backed by a fresh, migrated
27:// step -- async_jobs.owner_session_id's FK exists for production integrity
29:// The session package's own AsyncJobStore tests (internal/session) DO seed
32:func newTestAsyncJobStore(t *testing.T) *session.AsyncJobStore {
41:	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
51:func jobOf(l *workLedger, owner, toolCallID string) *asyncJob {
```

### `internal/agent/work_ledger_timeout.go` (21 hits)

```
21:// elapses. Generalizes the heap beyond asyncJob (phase 2) so a later
25:// wrapping this for the asyncJob case so every existing call site is
49:// timeoutService is the single per-process timer for every asyncJob with a
57:	ledger    *workLedger
62:	closeOnce sync.Once // workLedger.close() is not itself guaranteed single-call (e.g. CancelAll during shutdown); close() below must survive a repeat
65:func newTimeoutService(l *workLedger) *timeoutService {
76:// workLedger.handleTimeout), so this cannot deadlock.
77:func (s *timeoutService) arm(job *asyncJob) {
85:// heap -- arm above is the asyncJob-specific wrapper every existing call
150:// never wires a timeoutService can still call workLedger.close() unconditionally.
163:func (l *workLedger) handleTimeout(job *asyncJob) {
189:			// timed-out outcome in memory, like job_kill's transitionSync, so
190:			// the blocked awaitSync caller gets it with the partial output
213:			// B-dev9: a sync job (SDK/library caller blocked in awaitSync,
214:			// doc sec.3.1) has no async_jobs row and no session_notices
257:// original text unconditionally offered both job_kill (bash/run_command) and
258:// stop_agent (agent) regardless of toolName, but job_kill only controlled a
263:// too (job_kill now tree-kills it via its live output buffer/ctx
267:		return "Decide whether to keep waiting, check again later, or stop it with job_kill."
278:func (l *workLedger) capturePartial(owner, toolCallID, toolName, childSession, shellID string, outputBuf tools.LiveOutputBuffer) jobResult {
317:func (l *workLedger) capturePartialDelegation(childSessionID string) jobResult {
```

### `internal/agent/work_ledger_timeout_test.go` (41 hits)

```
28:	l := newWorkLedger(nil)
29:	l.store = newTestAsyncJobStore(t)
82:	l := newWorkLedger(func(c AsyncCompletion) {
87:	l.store = newTestAsyncJobStore(t)
115:// TestWorkLedger_TerminateAndWakeTransitionsToTimedOut: a terminate_and_wake
118:func TestWorkLedger_TerminateAndWakeTransitionsToTimedOut(t *testing.T) {
121:	l := newWorkLedger(func(c AsyncCompletion) { delivered = append(delivered, c) })
122:	l.store = newTestAsyncJobStore(t)
173:	coord.asyncJobs = newWorkLedger(nil)
174:	coord.asyncJobs.store = newTestAsyncJobStore(t)
175:	coord.asyncJobs.coord = coord
178:	require.NoError(t, coord.asyncJobs.store.InsertSessionNotice(t.Context(), "child-1", "manual_test_notice", "owed", true, ""))
181:	// through as AsyncCompletion.Wake (work_ledger.go).
199:// TestWorkLedger_WakeOnlyUsesTimeoutWakeOnlyNoticeKind: handleTimeout's
205:func TestWorkLedger_WakeOnlyUsesTimeoutWakeOnlyNoticeKind(t *testing.T) {
214:	l := newWorkLedger(nil)
215:	l.store = newTestAsyncJobStore(t)
217:	coord.asyncJobs = l
247:// TestWorkLedger_WakeOnly_SyncJobProducesNoNoticeOrWake pins B-dev9:
249:// sync (SDK/library, no async_jobs row, doc sec.3.1) job it would still
253:// awaitSync). Only terminate_and_wake (via ordinary ctx cancellation) makes
259:// Diffed work_ledger_timeout.go against git HEAD after restoring: matches
261:func TestWorkLedger_WakeOnly_SyncJobProducesNoNoticeOrWake(t *testing.T) {
270:	l := newWorkLedger(nil)
271:	l.store = newTestAsyncJobStore(t)
273:	coord.asyncJobs = l
299:// TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning: calling
305:func TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning(t *testing.T) {
314:	l := newWorkLedger(nil)
315:	l.store = newTestAsyncJobStore(t)
317:	coord.asyncJobs = l
338:// TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome extends ASYNC-03's
339:// phase-1 race test (TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome)
341:func TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome(t *testing.T) {
345:		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
346:		l.store = newTestAsyncJobStore(t)
430:// toolName. As of task #1023 §3, job_kill controls BOTH bash and
436:// "job_kill"). Restored the fix; re-ran, passed.
441:	require.Contains(t, bashText, "job_kill", "bash is one of the two tools job_kill controls")
445:	require.Contains(t, runCommandText, "job_kill", "task #1023 §3 made run_command controllable via job_kill")
450:	require.NotContains(t, agentText, "job_kill")
```

### `internal/agent/work_ledger_transition.go` (17 hits)

```
4:// existing terminal-transition call site (finish, handleTimeout, job_kill/
6:// instead of calling asyncJob.transitionToTerminal directly. The DB row
64:func (l *workLedger) retryAsyncStoreOp(ctx context.Context, op func() error) error {
86:// candidate -- for every cause except job_kill: its result is the job_kill
89:// second, pulled history notice. wake is already false for job_kill, which
108:		state, noticeKind, wake = "cancelled", "job_kill", false
131:// asyncJobKindFor maps a tool name to the async_jobs.kind vocabulary (doc
133:func asyncJobKindFor(toolName string) session.JobKind {
198:// job is the caller's own *asyncJob, not a key (A11's in-process half): a
202:func (l *workLedger) commitTransition(job *asyncJob, cause transitionCause, result jobResult) commitOutcome {
317:// job_kill), not assume its own cause won the race. found is false when the
336:func (l *workLedger) commitAndDeliver(job *asyncJob, cause transitionCause, result jobResult) (commitOutcome, jobOutcomeSnapshot) {
359:// commitAndDeliver's return value. job_kill/StopRunCommandJob (work_ledger.go)
363:func (l *workLedger) transition(job *asyncJob, cause transitionCause, result jobResult) {
367:// transitionSync is the sync-job counterpart of every external cause (job_kill,
373:// blocked in awaitSync gets that outcome (stopped/timed-out, with partial
376:func (l *workLedger) transitionSync(job *asyncJob, phase jobPhase, result jobResult) {
```

### `internal/app/app.go` (12 hits)

```
101:	// asyncJobStore is the phase-4 durable job store (docs/plans/2026-09-28-
105:	// externally via AsyncJobStore(); nil under SkipAgentSetup or when
107:	asyncJobStore *session.AsyncJobStore
111:	// to asyncJobStore below and handed to the coordinator in
116:	// Nil unless RUSH_BATCH_RUNTIME parsed true at New AND the App built its
375:	// (`sessions jobs/why/list/gc`) must be able to read async_jobs/
380:	// ClaimSessionDriver (see AsyncJobStore's own doc), so building it
384:		app.asyncJobStore = session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "app")
385:		app.asyncJobStore.SetMessages(messages)
394:		// ListAsyncJobsForOwner (the `sessions jobs`/`sessions why`/web
400:		app.asyncJobStore.SetReadConn(readConn)
410:	if app.asyncJobStore != nil && config.BatchRuntimeEnabled() {
```

### `internal/app/app_accessors.go` (9 hits)

```
42:// AsyncJobStore returns the phase-4 durable job store App.New builds
50:func (app *App) AsyncJobStore() *session.AsyncJobStore {
51:	return app.asyncJobStore
54:// SetAsyncJobStoreForTest wires an AsyncJobStore into an App built WITHOUT
58:// AsyncJobStore's own SimulateCrashForTest seam.
59:func (app *App) SetAsyncJobStoreForTest(s *session.AsyncJobStore) {
60:	app.asyncJobStore = s
64:// (same construction rules as AsyncJobStore: nil under SkipAgentSetup or
72:// SetAsyncJobStoreForTest.
```

### `internal/app/app_agent_setup.go` (4 hits)

```
147:	if app.asyncJobStore == nil && app.dataDir != "" {
152:		app.asyncJobStore = session.NewAsyncJobStore(app.DB(), app.dataDir, os.Getpid(), "app")
156:		app.asyncJobStore.SetMessages(app.Messages)
169:		app.asyncJobStore,
```

### `internal/app/app_batch_cli.go` (2 hits)

```
39:	return a != nil && a.batchRuntime != nil && a.batchInstall != nil && a.batchInstall.Runner != nil && a.asyncJobStore != nil
147:	if a.asyncJobStore.HostID() != rootIdentity.Host {
```

### `internal/app/app_batch_doc.go` (4 hits)

```
10://   identity via asyncJobStore.CloseKeepLock/Close and the writer via
31://      app.asyncJobStore / app.dbConns; store it in a new App field
41://   3. app_lifecycle.go releaseResources: replace the asyncJobStore
68://     RUSH_BATCH_RUNTIME flag (R11F; flag listed as non-goal here).
```

### `internal/app/app_batch_install.go` (9 hits)

```
2:// runtime via the M1 factory only when RUSH_BATCH_RUNTIME parses true, and
38:// runtime. Caller must have checked the flag and non-nil asyncJobStore.
48:	host, err := app.asyncJobStore.EnsureHost(ctx)
96:	rt.cfg.RetainHostIdentity = app.asyncJobStore.CloseKeepLock
97:	rt.cfg.ReleaseHostIdentity = app.asyncJobStore.Close
151:		if app.asyncJobStore != nil {
152:			app.asyncJobStore.CloseKeepLock()
158:		if app.asyncJobStore != nil {
159:			app.asyncJobStore.CloseKeepLock()
```

### `internal/app/app_batch_install_test.go` (10 hits)

```
67:// RUSH_BATCH_RUNTIME unset, "false" or unparseable, an App built through the
84:				t.Setenv("RUSH_BATCH_RUNTIME", tc.value)
103:// App's own asyncJobStore/DB release path, fault watcher armed — and the
107:	t.Setenv("RUSH_BATCH_RUNTIME", "true")
112:	hostID := a.asyncJobStore.HostID()
124:		if a.asyncJobStore != nil {
127:			_ = a.asyncJobStore.Close(context.Background())
144:	require.NotEmpty(t, a.asyncJobStore.HostID())
152:	t.Setenv("RUSH_BATCH_RUNTIME", "true")
155:	t.Cleanup(func() { _ = a.asyncJobStore.Close(context.Background()) })
```

### `internal/app/app_batch_runtime.go` (3 hits)

```
61:	// ReleaseHostIdentity releases the host identity (AsyncJobStore.Close
90:// (asyncJobStore.CloseKeepLock/Close) and the DB writer release
92:// gate DrainAndRelease. The App fields it will own: asyncJobStore,
```

### `internal/app/app_forced_shutdown_host_test.go` (3 hits)

```
3:// AsyncJobStore.CloseKeepLock): a library caller must exit its process. This
26:	store := h.app.asyncJobStore
52:	second := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "second-app")
```

### `internal/app/app_lifecycle.go` (3 hits)

```
296:	} else if app.asyncJobStore != nil {
298:			app.asyncJobStore.CloseKeepLock()
299:		} else if err := app.asyncJobStore.Close(shutdownCtx); err != nil {
```

### `internal/app/app_run_a15_a18_a10_test.go` (2 hits)

```
42:			_, err := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{
54:	_, err := h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
```

### `internal/app/app_run_async_wait.go` (4 hits)

```
28:	if l.app == nil || l.app.asyncJobStore == nil {
36:	store := l.app.asyncJobStore
37:	rows, err := store.ListRunningAsyncJobsForOwners(ctx, []string{l.sessionID})
66:func (l *cliLoop) describeHost(ctx context.Context, store *session.AsyncJobStore, hostID string) string {
```

### `internal/app/app_run_await_tasks_test.go` (5 hits)

```
24:	_, err := application.asyncJobStore.Claim(ctx, session.ClaimParams{
29:	require.NoError(t, application.asyncJobStore.MarkAnnounced(ctx, owner, toolCallID))
35:	_, err := application.asyncJobStore.Transition(ctx, session.TransitionParams{
78:	rows, err := application.asyncJobStore.ListRunningForOwners(context.Background(), []string{sessionID})
330:				require.NoError(h.t, h.app.asyncJobStore.InsertSessionNotice(ctx, h.sessionID,
```

### `internal/app/app_run_billing_test.go` (4 hits)

```
24:	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: h.sessionID, ToolCallID: "billing-command", Kind: session.JobKindCommand, ToolName: "bash", Input: "durable test command"})
26:	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "billing-command"))
31:			_, completionErr := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{Owner: h.sessionID, ToolCallID: "billing-command", State: "completed", ResultSummary: "billing-command-output", Wake: true})
47:	jobs, err := h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
```

### `internal/app/app_run_chain_guard_e2e_test.go` (6 hits)

```
7:// launches at once -- no workLedger.Start, no async_jobs row, no #1113 claim
192:	// The in-turn guard refuses each poll: no workLedger.Start, no async job.
194:		_, err := app.asyncJobStore.Get(context.Background(), sessionID, callID)
195:		require.Error(t, err, "a refused wait starts no job: no async_jobs row for %s", callID)
200:		open, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
213:	open, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
```

### `internal/app/app_run_cli_drain_ctrlc_test.go` (4 hits)

```
89:		row, err := app.asyncJobStore.Get(context.Background(), sessionID, "call-1")
110:	debtStillOpen, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
113:	row, err := app.asyncJobStore.Get(context.Background(), sessionID, "call-1")
117:	notices, err := app.asyncJobStore.ListSessionNotices(context.Background(), sessionID)
```

### `internal/app/app_run_deferred_debt_test.go` (1 hits)

```
35:				_ = a.asyncJobStore.InsertSessionNotice(context.Background(), sessionID.Load().(string),
```

### `internal/app/app_run_job_kill_by_id_test.go` (15 hits)

```
7:// the fix, job_kill only accepted shell_id, so the model had no way to
10:// Updated for task #1063 (phase-4 durable-core step 6): job_kill's own tool
15:// accordingly: the "final" turn is job_kill's own turn now, not a second
36:// pass back to job_kill/job_output as job_id.
39:func TestRunNonInteractiveRootKillsOwnAsyncJobByJobID(t *testing.T) {
52:		// then call job_kill with job_id (not shell_id).
63:			// ledger. Wait comfortably past that so job_kill's job_id
72:				admissionSSEToolCall("kill", "call-kill", "job_kill", string(input)),
76:		// Turn 3: job_kill's own immediate response now carries the real
79:		case strings.Contains(lastTool, "was stopped (job_kill)"):
113:	// The command itself runs for ~60s; job_kill by job_id must stop it
116:		"job_kill(job_id) must stop the command well before its natural end")
118:	// Task #1063: killing via job_kill must produce ZERO background-job
119:	// notices for the job -- its result is job_kill's OWN tool answer, and
133:	require.Equal(t, 0, notices, "job_kill's result is its own tool answer, never a separate background-job notice")
```

### `internal/app/app_run_loop_more_test.go` (3 hits)

```
182:			require.NoError(h.t, h.app.asyncJobStore.InsertSessionNotice(context.Background(), h.sessionID,
211:	// scheduling race (the completion Drain can merge into the post-job_kill
243:				admissionSSEToolCall("child-kill", "call-kill", "job_kill", `{"shell_id":"001"}`),
```

### `internal/app/app_run_loop_round5_test.go` (1 hits)

```
137:	rows, err := h.app.asyncJobStore.ListAsyncJobsForOwner(context.Background(), h.sessionID)
```

### `internal/app/app_run_loop_test.go` (6 hits)

```
65:	require.NoError(h.t, h.app.asyncJobStore.InsertSessionNotice(context.Background(), h.sessionID,
71:	open, err := h.app.asyncJobStore.ReactionDebtExists(context.Background(), h.sessionID)
78:	notices, err := h.app.asyncJobStore.ListSessionNotices(context.Background(), h.sessionID)
356:		_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
361:		require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, parent.ID, "deleg-1"))
362:		_, err = h.app.asyncJobStore.Transition(ctx, session.TransitionParams{
```

### `internal/app/app_run_question_open_work_test.go` (3 hits)

```
38:				_, transitionErr := h.app.asyncJobStore.Transition(context.Background(),
56:	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
61:	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "deleg-1"))
```

### `internal/app/app_run_quota.go` (2 hits)

```
105:	rows, incomplete := l.app.asyncJobStore.JobsInTree(l.ctx, l.sessionID)
164:	rows, incomplete := l.app.asyncJobStore.JobsInTree(l.ctx, l.sessionID)
```

### `internal/app/app_run_quota_child_test.go` (1 hits)

```
111:	rows, listErr := application.asyncJobStore.ListAsyncJobsForOwner(ctx, sid)
```

### `internal/app/app_run_quota_test.go` (5 hits)

```
67:	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: h.sessionID, ToolCallID: "own-bash", Kind: session.JobKindCommand, ToolName: "bash", Input: "x"})
69:	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "own-bash"))
74:			_, e := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{Owner: h.sessionID, ToolCallID: "own-bash", State: "completed", ResultSummary: "own-output", Wake: true})
95:	jobs, err := h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
106:	jobs, err = h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
```

### `internal/app/app_run_reviewer_readonly_test.go` (1 hits)

```
93:	for _, name := range []string{"bash", "write", "edit", "multiedit", "run_command", "todos", "ask_question", "download", "fetch", "agent", "agentic_fetch", "job_kill"} {
```

### `internal/app/app_run_setup.go` (1 hits)

```
416:	// Turn-stall policy: resolve --stall-timeout (default 30m) onto this call.
```

### `internal/app/app_run_subagent_root_wait_test.go` (9 hits)

```
66:	// childJobKilled is closed by the child's job_kill tool call, i.e. the
114:			// ("Async job call-bash (bash) was stopped (job_kill)"). The
120:			// MERGED into the child's plain post-job_kill continuation: the
134:			// Child turn 3 (unmerged path): the plain post-job_kill
154:				admissionSSEToolCall("child-kill", "call-kill", "job_kill", `{"shell_id":"001"}`),
221:	//     child's job_kill has landed),
311:// grantChildBackgroundShellTools layers bash/job_kill/job_output onto the
327:	for _, name := range []string{"bash", "job_kill", "job_output"} {
397:	// post-job_kill continuation, making that turn carry BOTH the pulled
```

### `internal/app/app_run_supervision_test.go` (2 hits)

```
6:// keeps a supervision timer entirely outside workLedger's job map, so
45:// (workLedger.next()) this test's CLI-level pass depends on; this test adds
```

### `internal/app/app_run_timeout_test.go` (1 hits)

```
43:				_, err := h.app.asyncJobStore.Claim(t.Context(), session.ClaimParams{Owner: h.sessionID, ToolCallID: "held", Kind: session.JobKindCommand, ToolName: "bash", Input: "held"})
```

### `internal/app/app_run_wait_heartbeat_test.go` (2 hits)

```
76:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
81:	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
```

### `internal/app/app_session_activity.go` (4 hits)

```
79:	store := app.AsyncJobStore()
188:func (app *App) sessionDriverFacts(ctx context.Context, ids []string, store *session.AsyncJobStore) (live map[string]session.SessionDriver, dead map[string]bool, err error) {
226:func (app *App) deadHostRunningJobs(ctx context.Context, store *session.AsyncJobStore, ids []string) (map[string]int, error) {
227:	rows, err := store.ListRunningAsyncJobsForOwners(ctx, ids)
```

### `internal/app/app_session_activity_test.go` (9 hits)

```
61:	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, between))
62:	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, slug))
63:	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, between, "wake", "x", true, ""))
64:	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, slug, "wake", "x", true, ""))
73:	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead")
153:	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, withDebt, "wake", "x", true, ""))
156:	debts, err := h.app.asyncJobStore.ReactionDebtOwners(ctx, []string{withDebt, without})
158:	singleDebt, err := h.app.asyncJobStore.ReactionDebtExists(ctx, withDebt)
160:	singleClean, err := h.app.asyncJobStore.ReactionDebtExists(ctx, without)
```

### `internal/app/app_session_drivers.go` (2 hits)

```
24:// hosts' rows. An App without an AsyncJobStore or a database handle answers
31:	store := app.asyncJobStore
```

### `internal/app/app_session_drivers_test.go` (3 hits)

```
30:	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, own))
31:	siblingStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 5151, "sibling")
34:	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead")
```

### `internal/app/async_phase4_driver_scenarios_test.go` (6 hits)

```
81:func jobRow(t *testing.T, app *App, sessionID string) db.AsyncJob {
83:	row, err := app.asyncJobStore.Get(context.Background(), sessionID, "call-1")
174:	require.NoError(t, appC.asyncJobStore.SimulateCrashForTest())
209:	require.NoError(t, appC.asyncJobStore.SimulateCrashForTest())
247:	require.Equal(t, appDriver.asyncJobStore.HostID(), elsewhere.HostID)
259:	require.Equal(t, appDriver.asyncJobStore.HostID(), row.HostID)
```

### `internal/app/async_phase4_recovery_scenarios_test.go` (19 hits)

```
112:	// AsyncJobStore.Close's nil-host early return).
252:	row, err := appA.asyncJobStore.Get(ctx, sessionID, "call-1")
255:	require.Equal(t, appA.asyncJobStore.HostID(), row.HostID)
259:	require.NoError(t, appA.asyncJobStore.SimulateCrashForTest())
273:	rowAfter, err := appB.asyncJobStore.Get(ctx, sessionID, "call-1")
325:	require.NoError(t, appA.asyncJobStore.Close(ctx))
419:		r1, err1 := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
420:		r2, err2 := appA.asyncJobStore.Get(context.Background(), sessionID, "call-2")
437:	// old version called appA.asyncJobStore.Close directly here, bypassing
439:	// coordinator.CancelAll's `c.asyncJobs.close()` -- is what actually
450:	row1, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
453:	row2, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-2")
477:		row, err := appB.asyncJobStore.Get(context.Background(), sessionID, id)
488:// only ever touches `delivery`, never `reacted` -- see PullPendingAsyncJob
562:		row, getErr := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
580:		row, getErr := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
587:	pulled, err := appB.asyncJobStore.PullJobNotices(context.Background(), appB.Messages, sessionID, recoveryScenarioEBuildJobNoticeParams)
592:	debtStillOpen, err := appB.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
614:	debtAfter, err := appA.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
```

### `internal/app/async_phase4_recovery_unannounced_test.go` (5 hits)

```
15:// simultaneously; this test still PASSED. AsyncJobStore.ensureHost's
78:	_, err := appA.asyncJobStore.Claim(ctx, session.ClaimParams{
83:	row, err := appA.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
87:	require.NoError(t, appA.asyncJobStore.SimulateCrashForTest())
100:	_, err = appB.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
```

### `internal/app/batch_install_coordinator_test.go` (3 hits)

```
16:	t.Setenv("RUSH_BATCH_RUNTIME", "false")
27:	t.Setenv("RUSH_BATCH_RUNTIME", "true")
41:		_ = a.asyncJobStore.Close(context.Background())
```

### `internal/app/batch_install_factory.go` (2 hits)

```
37:	if a.asyncJobStore == nil {
48:		deps.Host = a.asyncJobStore.HostID()
```

### `internal/app/batch_install_factory_test.go` (1 hits)

```
23:	a.asyncJobStore = session.NewAsyncJobStore(conn, dataDir, 1, "test")
```

### `internal/app/p1_5_shutdown_test.go` (1 hits)

```
157:func (m *mockCoordinatorForShutdown) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/app/release_gate_test.go` (1 hits)

```
429:func (a *sessionAgentCoordinatorAdapter) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/cmd/batch_cli.go` (1 hits)

```
41:		return errors.New("batch runtime is disabled; set RUSH_BATCH_RUNTIME=1")
```

### `internal/cmd/batch_cli_runtime_test.go` (1 hits)

```
237:	store := session.NewAsyncJobStore(conn, dir, os.Getpid(), "batch-cli-runtime-acceptance")
```

### `internal/cmd/batch_cli_test.go` (7 hits)

```
201:	store           *session.AsyncJobStore
210:	store := session.NewAsyncJobStore(conn, dir, os.Getpid(), "batch-cli-acceptance")
249:	cmd.Env = append(os.Environ(), batchCLIHelperEnv+"=1", "R14_BATCH_CLI_ARGS="+string(encoded), "RUSH_BATCH_RUNTIME=off", "RUSH_GLOBAL_DATA="+t.TempDir(), "RUSH_GLOBAL_CONFIG="+t.TempDir())
322:	old := os.Getenv("RUSH_BATCH_RUNTIME")
323:	require.NoError(t, os.Setenv("RUSH_BATCH_RUNTIME", "off"))
324:	t.Cleanup(func() { _ = os.Setenv("RUSH_BATCH_RUNTIME", old) })
329:	require.EqualError(t, err, "batch runtime is disabled; set RUSH_BATCH_RUNTIME=1")
```

### `internal/cmd/claude_wrush_command.md` (1 hits)

```
47:   run: a linked worktree's sessions, messages, `async_jobs` and logs are
```

### `internal/cmd/run.go` (3 hits)

```
472:			stallTimeout, _     = cmd.Flags().GetDuration("stall-timeout")
754:		// Turn-stall abort policy: --stall-timeout (0 = off) feeds the
849:	runCmd.Flags().Duration("stall-timeout", 30*60*time.Second, "Abort a turn that has made no durable progress (tool call/result, step finish, checkpoint) for this long (turn-stall abort policy). Default 30m; 0 disables the abort.")
```

### `internal/cmd/sessions_activity_cmd_test.go` (5 hits)

```
46:func newDeadHostStore(t *testing.T, dataDir string, pid int, label string) *session.AsyncJobStore {
51:	st := session.NewAsyncJobStore(conn, dataDir, pid, label)
79:func newActivityCmdApp(t *testing.T) (*app.App, string, *session.AsyncJobStore) {
91:	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "activity-cmd-test")
93:	a.SetAsyncJobStoreForTest(store)
```

### `internal/cmd/sessions_audit.go` (4 hits)

```
59:	// A background-job reader invoked to block: `job_output 12 --wait`,
60:	// `job_output 12 --wait=true`.
61:	regexp.MustCompile(`^\s*job_output(\s+(\S+=\S*|--?wait\S*|[0-9A-Za-z_.-]+))*\s*$`),
109:match one of 'sleep 5', 'timeout 30 <cmd>', 'wait', 'job_output 12 --wait'
```

### `internal/cmd/sessions_audit_db.go` (7 hits)

```
33:	HasAsyncJobs           bool
34:	AsyncJobColumns        []string
156:	jobColumns, err := auditTableColumns(ctx, db, "async_jobs")
158:		schema.HasAsyncJobs = true
159:		schema.AsyncJobColumns = make([]string, 0, len(jobColumns))
161:			schema.AsyncJobColumns = append(schema.AsyncJobColumns, column)
163:		sort.Strings(schema.AsyncJobColumns)
```

### `internal/cmd/sessions_audit_findings.go` (1 hits)

```
52:	if err := auditScanAsyncJobs(ctx, db, schema, ids, tally); err != nil {
```

### `internal/cmd/sessions_audit_rules.go` (10 hits)

```
47:// `job_output 12` collapse onto one key.
285:// auditScanAsyncJobs counts the jobs of the selected sessions that never came
290:func auditScanAsyncJobs(ctx context.Context, db *sql.DB, schema auditSchema, ids []string, tally *auditTally) error {
291:	if !schema.HasAsyncJobs {
295:		if !slices.Contains(schema.AsyncJobColumns, required) {
299:	withKind := slices.Contains(schema.AsyncJobColumns, "kind")
305:	query := "SELECT " + strings.Join(columns, ", ") + " FROM async_jobs" +
317:		return fmt.Errorf("query async_jobs: %w", err)
329:			return fmt.Errorf("scan async_jobs: %w", err)
338:		return fmt.Errorf("iterate async_jobs: %w", err)
```

### `internal/cmd/sessions_audit_rules_test.go` (11 hits)

```
44:// auditSchemaFixtureNoJobs is a database with no async_jobs table at all: one
48:// auditSchemaFixtureJobsNoKind drops async_jobs.kind, the one column the
51:CREATE TABLE async_jobs (
73:// auditJobFixture is one async_jobs row a rules test seeds.
122:	jobColumns, err := auditTableColumns(ctx, db, "async_jobs")
158:			_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs (owner_session_id, tool_call_id, kind, state, child_session_id, created_at, updated_at)
162:			_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs (owner_session_id, tool_call_id, state, child_session_id, created_at, updated_at)
413:		wait("timeout 60"), wait("job_output 12 --wait"), wait("poll.sh --interval=5"),
579:// the reduced set, never failed on. Without async_jobs the rule is simply
584:	// No async_jobs table at all: the messages are still audited, the
600:	// async_jobs without the kind column: the rule still fires, naming the
```

### `internal/cmd/sessions_audit_test.go` (2 hits)

```
49:CREATE TABLE async_jobs (
94:	_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs
```

### `internal/cmd/sessions_cancel_test.go` (2 hits)

```
47:	store := a.AsyncJobStore()
52:	a.SetAsyncJobStoreForTest(nil)
```

### `internal/cmd/sessions_compact.go` (1 hits)

```
58:AsyncJobStore) — so an IDLE web server with no claimed jobs is exactly
```

### `internal/cmd/sessions_gc.go` (10 hits)

```
26:voided rows from the phase-4 durable async job ledger (async_jobs and
46:with --jobs-older-than a first line {"kind":"async_jobs","dry_run":...,
47:"async_jobs":N,"session_notices":M} reports the purged (or would-be-purged)
76:// async_jobs and session_notices rows were purged (or, with --dry-run, would
82:	AsyncJobs      int64  `json:"async_jobs"`
168:		store := a.AsyncJobStore()
182:			// line (no "id"; "kind":"async_jobs") precedes the per-session lines.
184:				Kind: "async_jobs", DryRun: dryRun, OlderThan: jobsOlderThanStr,
185:				AsyncJobs: jobsAffected, SessionNotices: noticesAffected,
303:	sessionsGcCmd.Flags().String("jobs-older-than", "", "Also purge terminal async_jobs/session_notices rows (phase-4 ledger) older than this (e.g. 3d, 12h); empty = only the fixed 7-day retention (web server every 60s, each rush run at start and every 60s while it runs)")
```

### `internal/cmd/sessions_gc_jobs_test.go` (37 hits)

```
4:// purges terminal, delivered-or-voided async_jobs/session_notices rows
62:// backdateAsyncJob rewrites updated_at for one async_jobs row, simulating a
65:func backdateAsyncJob(t *testing.T, conn *sql.DB, owner, toolCallID string, at time.Time) {
68:		`UPDATE async_jobs SET updated_at = ? WHERE owner_session_id = ? AND tool_call_id = ?`,
90:func countAsyncJobRows(t *testing.T, conn *sql.DB, owner, toolCallID string) int {
94:		`SELECT COUNT(*) FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?`, owner, toolCallID)
112:// deleted). Covers async_jobs and its session_notices twin.
121:	store := a.AsyncJobStore()
143:	_, err = q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{UpdatedAt: time.Now().Unix(), OwnerSessionID: sess.ID})
146:	backdateAsyncJob(t, conn, sess.ID, "old-terminal", time.Now().Add(-10*24*time.Hour))
157:	backdateAsyncJob(t, conn, sess.ID, "old-debt", time.Now().Add(-30*24*time.Hour))
162:	backdateAsyncJob(t, conn, sess.ID, "old-running", time.Now().Add(-30*24*time.Hour))
172:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
187:	// must survive like the async_jobs twin above.
200:	require.Equal(t, 0, countAsyncJobRows(t, verifyConn, sess.ID, "old-terminal"),
202:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "recent-terminal"),
204:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-debt"),
206:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-running"),
208:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-running-unknown-host"),
211:		"an old, delivered notice must be purged (the async_jobs twin)")
228:	store := a.AsyncJobStore()
238:	_, err = q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{UpdatedAt: time.Now().Unix(), OwnerSessionID: sess.ID})
240:	backdateAsyncJob(t, conn, sess.ID, "dry-run-terminal", time.Now().Add(-10*24*time.Hour))
259:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "dry-run-terminal"),
276:	store := a.AsyncJobStore()
285:	backdateAsyncJob(t, conn, sess.ID, "old-debt", time.Now().Add(-30*24*time.Hour))
297:	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-debt"),
303:// prose). It now emits one summary line with the async_jobs and
308:// "async_jobs" line is printed and the assertions below fail.
317:	store := a.AsyncJobStore()
327:	_, err = q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{UpdatedAt: time.Now().Unix(), OwnerSessionID: sess.ID})
329:	backdateAsyncJob(t, conn, sess.ID, "json-terminal", time.Now().Add(-10*24*time.Hour))
342:			if json.Unmarshal([]byte(line), &s) == nil && s.Kind == "async_jobs" {
346:		t.Fatalf("no async_jobs summary line in --json output %q", stdout)
352:	require.EqualValues(t, 1, dry.AsyncJobs, "dry-run must report the job it would purge")
358:	require.EqualValues(t, 1, purged.AsyncJobs, "the real run must report the job it purged")
364:	require.Equal(t, 0, countAsyncJobRows(t, verifyConn, sess.ID, "json-terminal"))
```

### `internal/cmd/sessions_help_text_test.go` (1 hits)

```
5:// CLI-only install, and a live delegation is a running async_jobs row on a
```

### `internal/cmd/sessions_inject_livework_test.go` (4 hits)

```
47:	store := a.AsyncJobStore()
50:	a.SetAsyncJobStoreForTest(nil)
103:	store := a.AsyncJobStore()
107:	a.SetAsyncJobStoreForTest(nil)
```

### `internal/cmd/sessions_jobs.go` (5 hits)

```
5:// phase4-durable-core.md sec.5 step 7). Lists a session's OWN async_jobs
6:// rows plus its whole delegation tree (session.AsyncJobStore.JobsInTree,
36:	Long: `List the durable async_jobs ledger rows owned by a session AND every
74:// plain db.AsyncJob can't carry (liveness verdict, host pid/label, the
110:	store := a.AsyncJobStore()
```

### `internal/cmd/sessions_jobs_test.go` (7 hits)

```
65:// claimForeignRunning inserts a running async_jobs row directly via
67:// registers via session.RegisterHost/AsyncJobStore.Claim (which would mark
68:// it "own" in the process-wide registry every AsyncJobStore in THIS test
73:	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
99:	// Own-process row: claimed via this seed App's own AsyncJobStore, so its
101:	store := a.AsyncJobStore()
128:	a.SetAsyncJobStoreForTest(nil)
```

### `internal/cmd/sessions_list.go` (3 hits)

```
71:		// its own async_jobs row live, so another process sees the same
77:		statusByID, awaitingByID := listStatusesWithAwaiting(cmd.Context(), a.AsyncJobStore(), activities, idsOf(sessions))
124:func listStatusesWithAwaiting(ctx context.Context, store *session.AsyncJobStore, activities app.SessionActivityBatchResult, ids []string) (map[string]string, map[string]session.ChildQuestion) {
```

### `internal/cmd/sessions_list_awaiting_test.go` (2 hits)

```
36:	store := session.NewAsyncJobStore(conn, dataDir, 1, "list-test")
46:	a.SetAsyncJobStoreForTest(store)
```

### `internal/cmd/sessions_list_test.go` (10 hits)

```
171://   - a REAL running async_jobs delegation row (owner=parent,
173://     AsyncJobStore -- step 7 replaced the session-lock-based descendant
179:// session.AsyncJobStore.LiveDescendantJobs) it must read "delegating"
208:	// AsyncJobStore (built unconditionally by app.New -- see AsyncJobStore's
210:	store := a.AsyncJobStore()
211:	require.NotNil(t, store, "app.New must build an AsyncJobStore for a data-dir'd App")
219:	// releaseResources Close()s app.asyncJobStore, which releases the OS
225:	a.SetAsyncJobStoreForTest(nil)
257:		"a parent with a live async_jobs delegation row must show delegating, never done — the observed bug")
279:	writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
```

### `internal/cmd/sessions_own_job_status_test.go` (6 hits)

```
35:func claimOwnJob(t *testing.T, store *session.AsyncJobStore, owner, toolCallID string) {
43:func finishOwnJob(t *testing.T, store *session.AsyncJobStore, owner, toolCallID string) {
144:	deadStore := session.NewAsyncJobStore(mustDBConn(t, dataDir), dataDir, 4242, "dead")
192:	store := a.AsyncJobStore()
197:	a.SetAsyncJobStoreForTest(nil)
223:	writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
```

### `internal/cmd/sessions_reset.go` (1 hits)

```
143:	store := a.AsyncJobStore()
```

### `internal/cmd/sessions_reset_async_test.go` (14 hits)

```
3:// R8A-3: `sessions reset` wiped the messages but left the session's async_jobs
17:func startedJob(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner, id string) {
31:func seedResetDebris(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner string) {
65:func assertResetClean(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner string) {
95:	seedResetDebris(t, a.AsyncJobStore(), a.Messages, sess.ID)
109:	assertResetClean(t, verify.AsyncJobStore(), verify.Messages, sess.ID)
113:func resetSeeded(t *testing.T, s session.Service, m message.Service, store *session.AsyncJobStore, title string) session.Session {
131:		hold func(t *testing.T, s session.Service, store *session.AsyncJobStore, id string)
134:		{"loop between turns", func(t *testing.T, _ session.Service, store *session.AsyncJobStore, id string) {
137:		{"running own job", func(t *testing.T, _ session.Service, store *session.AsyncJobStore, id string) {
140:		{"running delegation", func(t *testing.T, s session.Service, store *session.AsyncJobStore, id string) {
208:			seedResetDebris(t, a.AsyncJobStore(), a.Messages, sess.ID)
209:			store := a.AsyncJobStore()
211:			a.SetAsyncJobStoreForTest(nil)
```

### `internal/cmd/sessions_run_driver_status_test.go` (7 hits)

```
27:func newRunDriverTestApp(t *testing.T) (a *app.App, s session.Service, m message.Service, store *session.AsyncJobStore, dataDir string) {
37:	store = session.NewAsyncJobStore(conn, dataDir, 4321, "rush-run")
40:	a.SetAsyncJobStoreForTest(store)
139:	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead-loop")
178:	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead")
223:	store := a.AsyncJobStore()
226:	a.SetAsyncJobStoreForTest(nil)
```

### `internal/cmd/sessions_show.go` (10 hits)

```
131:		// AsyncJobsTotal/AsyncJobsRunning are this session's OWN async_jobs
133:		// this App has no AsyncJobStore or the session owns no rows.
134:		AsyncJobsTotal   int       `json:"async_jobs_total,omitempty"`
135:		AsyncJobsRunning int       `json:"async_jobs_running,omitempty"`
205:	if store := a.AsyncJobStore(); store != nil {
206:		if jobs, err := store.ListAsyncJobsForOwner(cmd.Context(), sess.ID); err == nil {
207:			output.AsyncJobsTotal = len(jobs)
210:					output.AsyncJobsRunning++
319:	if output.AsyncJobsTotal > 0 {
320:		fmt.Printf("Async jobs:   %d total, %d running (see `sessions jobs %s`)\n", output.AsyncJobsTotal, output.AsyncJobsRunning, sess.ID)
```

### `internal/cmd/sessions_show_jobs_test.go` (4 hits)

```
18:func TestSessionsShowCmdRun_AsyncJobCounts(t *testing.T) {
41:	store := seed.AsyncJobStore()
53:	seed.SetAsyncJobStoreForTest(nil)
65:func TestSessionsShowCmdRun_NoAsyncJobsPrintsNothingExtra(t *testing.T) {
```

### `internal/cmd/sessions_why.go` (9 hits)

```
50:             delegation is still live: a running async_jobs row names a
118:// reported by describeAsyncJobsAndDebt.
142:	if store := a.AsyncJobStore(); store != nil {
229:	describeAsyncJobsAndDebt(ctx, a, sessionID, out)
270:// describeAsyncJobsAndDebt renders the plain-language jobs/debt section of
274:// AsyncJobStore (SkipAgentSetup, or an App built without one in a test).
275:func describeAsyncJobsAndDebt(ctx context.Context, a *app.App, sessionID string, out io.Writer) {
276:	store := a.AsyncJobStore()
282:	jobs, err := store.ListAsyncJobsForOwner(ctx, sessionID)
```

### `internal/cmd/sessions_why_awaiting_test.go` (5 hits)

```
18:// AsyncJobStore the awaiting-answer reader (#1158) goes through.
19:func whyAwaitingTestEnv(t *testing.T) (*app.App, *session.AsyncJobStore, string) {
31:	store := session.NewAsyncJobStore(conn, dataDir, 1, "why-test")
33:	a.SetAsyncJobStoreForTest(store)
39:func insertChildQuestionNotice(t *testing.T, store *session.AsyncJobStore, owner, childID, jobID, question string) {
```

### `internal/cmd/sessions_why_descendant_test.go` (20 hits)

```
9:// The fix: a session with a live async_jobs DELEGATION row (child_session_id)
11:// explainSessionStatus consults session.AsyncJobStore.LiveDescendantJobs
19:// own locks) with a durable-state walk over live async_jobs delegation
21:// were rewritten from real session locks to real AsyncJobStore rows
46:// async_jobs rows.
80:// hand-rolled schema, since AsyncJobStore needs async_jobs/async_hosts/
82:// AsyncJobStore wired via SetAsyncJobStoreForTest, the seam this class of
84:func newWhyDescendantTestApp(t *testing.T) (a *app.App, s session.Service, m message.Service, store *session.AsyncJobStore, dataDir string) {
94:	store = session.NewAsyncJobStore(conn, dataDir, 999, "test")
97:	a.SetAsyncJobStoreForTest(store)
102:// claimDelegation claims a running 'agent' async_jobs row on parentID
107:func claimDelegation(t *testing.T, store *session.AsyncJobStore, parentID, toolCallID, childID string) {
120:// async_jobs delegation row still names the child. Pre-fix this printed
228:// the root's own async_jobs row delegates to the child, and the CHILD's own
229:// async_jobs row in turn delegates to the grandchild — both rows 'running'
313:// TestExplainSessionStatus_AsyncJobsAndDebtSection covers the plain-language
317:func TestExplainSessionStatus_AsyncJobsAndDebtSection(t *testing.T) {
376:// TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted proves the
377:// debt line flips to "none" once MarkAsyncJobsReactedForOwner-equivalent
379:func TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted(t *testing.T) {
```

### `internal/config/batch_runtime_flag.go` (2 hits)

```
1:// R11F shared flag parser for the inert batch runtime (RUSH_BATCH_RUNTIME).
15:	on, err := strconv.ParseBool(os.Getenv("RUSH_BATCH_RUNTIME"))
```

### `internal/config/batch_runtime_flag_test.go` (8 hits)

```
1:// R11F round-2 (P2 m4-flag-parse): the shared RUSH_BATCH_RUNTIME parser must
14:	t.Setenv("RUSH_BATCH_RUNTIME", "sometimes")
15:	require.False(t, BatchRuntimeEnabled(), "unparseable RUSH_BATCH_RUNTIME means OFF")
16:	t.Setenv("RUSH_BATCH_RUNTIME", "")
17:	require.False(t, BatchRuntimeEnabled(), "empty RUSH_BATCH_RUNTIME means OFF")
18:	t.Setenv("RUSH_BATCH_RUNTIME", "0")
20:	t.Setenv("RUSH_BATCH_RUNTIME", "true")
22:	t.Setenv("RUSH_BATCH_RUNTIME", "1")
```

### `internal/config/config.go` (2 hits)

```
426:		"job_output",
427:		"job_kill",
```

### `internal/config/config_mcp.go` (1 hits)

```
296:	// tool, blocking job_output --wait). Past this cap the watchdog fires
```

### `internal/config/store_oauth_matrix_test.go` (1 hits)

```
236:		// Install-before-publication is reachable with live A.
```

### `internal/db/async_jobs.sql.go` (229 hits)

```
4:// source: async_jobs.sql
14:const archiveAsyncJobToolCallID = `-- name: ArchiveAsyncJobToolCallID :execrows
15:UPDATE async_jobs SET tool_call_id = ?1, updated_at = ?2
21:type ArchiveAsyncJobToolCallIDParams struct {
45:// delivery check: Rerun's VoidAsyncJobsByToolCallIDs can set delivery='void'
50:func (q *Queries) ArchiveAsyncJobToolCallID(ctx context.Context, arg ArchiveAsyncJobToolCallIDParams) (int64, error) {
51:	result, err := q.exec(ctx, q.archiveAsyncJobToolCallIDStmt, archiveAsyncJobToolCallID,
63:const asyncJobDebtOwners = `-- name: AsyncJobDebtOwners :many
64:SELECT DISTINCT owner_session_id AS owner FROM async_jobs
69:// The async_jobs half of the batched reaction-debt read (architect decision
75:func (q *Queries) AsyncJobDebtOwners(ctx context.Context, ownerIds []string) ([]string, error) {
76:	query := asyncJobDebtOwners
111:        SELECT 1 FROM async_jobs
121:// idx_async_jobs_debt and idx_session_notices_debt "(owner) WHERE wake=1
122:// AND reacted=0 AND delivery<>'void'" on both tables. The async_jobs branch
127:// (ListPendingAsyncJobNoticesForOwner/PullPendingAsyncJobNotice) already
130:// then aborts, the row is deleted (DeleteUnannouncedAsyncJob) and the turn
140:const claimAsyncJob = `-- name: ClaimAsyncJob :one
141:INSERT INTO async_jobs (
152:type ClaimAsyncJobParams struct {
173:// (GetAsyncJob), same as before.
183:// into TransitionAsyncJobTerminalPreserveVoid's CAS -- closes the ABA where
187:func (q *Queries) ClaimAsyncJob(ctx context.Context, arg ClaimAsyncJobParams) (AsyncJob, error) {
188:	row := q.queryRow(ctx, q.claimAsyncJobStmt, claimAsyncJob,
204:	var i AsyncJob
237:UPDATE async_jobs SET reacted_failed = 0, updated_at = ?
266:const countAsyncJobsOlderThan = `-- name: CountAsyncJobsOlderThan :one
267:SELECT COUNT(*) FROM async_jobs
270:  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
273:            SELECT 1 FROM async_jobs c
274:            WHERE c.owner_session_id = async_jobs.child_session_id
279:            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
284:// Same predicate as PurgeAsyncJobsOlderThan, read-only, for
286:func (q *Queries) CountAsyncJobsOlderThan(ctx context.Context, updatedAt int64) (int64, error) {
287:	row := q.queryRow(ctx, q.countAsyncJobsOlderThanStmt, countAsyncJobsOlderThan, updatedAt)
295:WHERE id = ? AND NOT EXISTS (SELECT 1 FROM async_jobs WHERE host_id = async_hosts.id)
298:// Deletes a host row that no async_jobs row references (doc sec.3.6): the owner
320:const deleteTerminalUnannouncedAsyncJobsForHost = `-- name: DeleteTerminalUnannouncedAsyncJobsForHost :execrows
321:DELETE FROM async_jobs WHERE host_id = ? AND announced = 0 AND state != 'running'
329:func (q *Queries) DeleteTerminalUnannouncedAsyncJobsForHost(ctx context.Context, hostID string) (int64, error) {
330:	result, err := q.exec(ctx, q.deleteTerminalUnannouncedAsyncJobsForHostStmt, deleteTerminalUnannouncedAsyncJobsForHost, hostID)
337:const deleteUnannouncedAsyncJob = `-- name: DeleteUnannouncedAsyncJob :execrows
338:DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0 AND state <> 'cancelled'
341:type DeleteUnannouncedAsyncJobParams struct {
350:func (q *Queries) DeleteUnannouncedAsyncJob(ctx context.Context, arg DeleteUnannouncedAsyncJobParams) (int64, error) {
351:	result, err := q.exec(ctx, q.deleteUnannouncedAsyncJobStmt, deleteUnannouncedAsyncJob, arg.OwnerSessionID, arg.ToolCallID)
358:const deleteUnannouncedAsyncJobForClaim = `-- name: DeleteUnannouncedAsyncJobForClaim :execrows
359:DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND claim_id = ? AND announced = 0
362:type DeleteUnannouncedAsyncJobForClaimParams struct {
368:// Recovery's twin of DeleteUnannouncedAsyncJob (R3A-3): recoverers no longer
373:func (q *Queries) DeleteUnannouncedAsyncJobForClaim(ctx context.Context, arg DeleteUnannouncedAsyncJobForClaimParams) (int64, error) {
374:	result, err := q.exec(ctx, q.deleteUnannouncedAsyncJobForClaimStmt, deleteUnannouncedAsyncJobForClaim, arg.OwnerSessionID, arg.ToolCallID, arg.ClaimID)
381:const deliverAsyncJobInline = `-- name: DeliverAsyncJobInline :execrows
382:UPDATE async_jobs
388:type DeliverAsyncJobInlineParams struct {
398:// (AnnounceInlineResult) -- exactly job_kill's DUR-11 shape. CAS on
405:func (q *Queries) DeliverAsyncJobInline(ctx context.Context, arg DeliverAsyncJobInlineParams) (int64, error) {
406:	result, err := q.exec(ctx, q.deliverAsyncJobInlineStmt, deliverAsyncJobInline,
434:const getAsyncJob = `-- name: GetAsyncJob :one
435:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?
438:type GetAsyncJobParams struct {
443:func (q *Queries) GetAsyncJob(ctx context.Context, arg GetAsyncJobParams) (AsyncJob, error) {
444:	row := q.queryRow(ctx, q.getAsyncJobStmt, getAsyncJob, arg.OwnerSessionID, arg.ToolCallID)
445:	var i AsyncJob
477:const getAsyncJobByClaimID = `-- name: GetAsyncJobByClaimID :one
478:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs
482:type GetAsyncJobByClaimIDParams struct {
493:func (q *Queries) GetAsyncJobByClaimID(ctx context.Context, arg GetAsyncJobByClaimIDParams) (AsyncJob, error) {
494:	row := q.queryRow(ctx, q.getAsyncJobByClaimIDStmt, getAsyncJobByClaimID, arg.Owner, arg.ClaimID, arg.ToolCallID)
495:	var i AsyncJob
527:const getRunningAsyncJobByChildSession = `-- name: GetRunningAsyncJobByChildSession :one
528:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs WHERE child_session_id = ? AND state = 'running'
535:func (q *Queries) GetRunningAsyncJobByChildSession(ctx context.Context, childSessionID sql.NullString) (AsyncJob, error) {
536:	row := q.queryRow(ctx, q.getRunningAsyncJobByChildSessionStmt, getRunningAsyncJobByChildSession, childSessionID)
537:	var i AsyncJob
569:const incrementAsyncJobWakeAttemptsForSnapshotRow = `-- name: IncrementAsyncJobWakeAttemptsForSnapshotRow :execrows
570:UPDATE async_jobs SET wake_attempts = wake_attempts + 1, updated_at = ?1
576:type IncrementAsyncJobWakeAttemptsForSnapshotRowParams struct {
592:func (q *Queries) IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx context.Context, arg IncrementAsyncJobWakeAttemptsForSnapshotRowParams) (int64, error) {
593:	result, err := q.exec(ctx, q.incrementAsyncJobWakeAttemptsForSnapshotRowStmt, incrementAsyncJobWakeAttemptsForSnapshotRow,
611:// `label` is the registrant's App label ("app" at both NewAsyncJobStore call
643:WHERE id NOT IN (SELECT DISTINCT host_id FROM async_jobs)
647:// async_jobs rows of ANY state. Caller still must verify the lock file
677:const listAsyncJobsForOwner = `-- name: ListAsyncJobsForOwner :many
678:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs WHERE owner_session_id = ? ORDER BY created_at ASC
682:func (q *Queries) ListAsyncJobsForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error) {
683:	rows, err := q.query(ctx, q.listAsyncJobsForOwnerStmt, listAsyncJobsForOwner, ownerSessionID)
688:	items := []AsyncJob{}
690:		var i AsyncJob
733:SELECT DISTINCT host_id FROM async_jobs
736:   OR (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
742:// died before its "started" result committed, R2A-7), or a job_kill row that
768:const listPendingAsyncJobNoticesForOwner = `-- name: ListPendingAsyncJobNoticesForOwner :many
769:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs
776:func (q *Queries) ListPendingAsyncJobNoticesForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error) {
777:	rows, err := q.query(ctx, q.listPendingAsyncJobNoticesForOwnerStmt, listPendingAsyncJobNoticesForOwner, ownerSessionID)
782:	items := []AsyncJob{}
784:		var i AsyncJob
826:const listReactedFailedAsyncJobsForOwner = `-- name: ListReactedFailedAsyncJobsForOwner :many
827:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs WHERE owner_session_id = ? AND reacted_failed = 1 ORDER BY created_at ASC
833:func (q *Queries) ListReactedFailedAsyncJobsForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error) {
834:	rows, err := q.query(ctx, q.listReactedFailedAsyncJobsForOwnerStmt, listReactedFailedAsyncJobsForOwner, ownerSessionID)
839:	items := []AsyncJob{}
841:		var i AsyncJob
883:const listRunningAsyncJobsForHost = `-- name: ListRunningAsyncJobsForHost :many
884:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs WHERE host_id = ? AND state = 'running' ORDER BY created_at ASC
890:func (q *Queries) ListRunningAsyncJobsForHost(ctx context.Context, hostID string) ([]AsyncJob, error) {
891:	rows, err := q.query(ctx, q.listRunningAsyncJobsForHostStmt, listRunningAsyncJobsForHost, hostID)
896:	items := []AsyncJob{}
898:		var i AsyncJob
940:const listRunningAsyncJobsForOwners = `-- name: ListRunningAsyncJobsForOwners :many
941:SELECT owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id, origin_cli, state, notice_kind, host_id, announced, delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed, deadline_at, timeout_kind, result_summary, result_is_error, created_at, updated_at, claim_id, announce_message_id FROM async_jobs
952:func (q *Queries) ListRunningAsyncJobsForOwners(ctx context.Context, ownerIds []string) ([]AsyncJob, error) {
953:	query := listRunningAsyncJobsForOwners
968:	items := []AsyncJob{}
970:		var i AsyncJob
1012:const markAsyncJobAnnounced = `-- name: MarkAsyncJobAnnounced :execrows
1013:UPDATE async_jobs SET announced = 1, updated_at = ?
1017:type MarkAsyncJobAnnouncedParams struct {
1028:func (q *Queries) MarkAsyncJobAnnounced(ctx context.Context, arg MarkAsyncJobAnnouncedParams) (int64, error) {
1029:	result, err := q.exec(ctx, q.markAsyncJobAnnouncedStmt, markAsyncJobAnnounced, arg.UpdatedAt, arg.OwnerSessionID, arg.ToolCallID)
1036:const markAsyncJobsReactedForOwner = `-- name: MarkAsyncJobsReactedForOwner :execrows
1037:UPDATE async_jobs SET reacted = 1, updated_at = ?
1041:type MarkAsyncJobsReactedForOwnerParams struct {
1051:func (q *Queries) MarkAsyncJobsReactedForOwner(ctx context.Context, arg MarkAsyncJobsReactedForOwnerParams) (int64, error) {
1052:	result, err := q.exec(ctx, q.markAsyncJobsReactedForOwnerStmt, markAsyncJobsReactedForOwner, arg.UpdatedAt, arg.OwnerSessionID)
1059:const pullPendingAsyncJobNotice = `-- name: PullPendingAsyncJobNotice :one
1060:UPDATE async_jobs
1066:type PullPendingAsyncJobNoticeParams struct {
1074:// notice_message_id via SetAsyncJobNoticeMessageID, all in the SAME tx. 0
1078:func (q *Queries) PullPendingAsyncJobNotice(ctx context.Context, arg PullPendingAsyncJobNoticeParams) (AsyncJob, error) {
1079:	row := q.queryRow(ctx, q.pullPendingAsyncJobNoticeStmt, pullPendingAsyncJobNotice, arg.UpdatedAt, arg.OwnerSessionID, arg.ToolCallID)
1080:	var i AsyncJob
1112:const purgeAsyncJobsOlderThan = `-- name: PurgeAsyncJobsOlderThan :execrows
1113:DELETE FROM async_jobs
1116:  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
1119:            SELECT 1 FROM async_jobs c
1120:            WHERE c.owner_session_id = async_jobs.child_session_id
1125:            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
1138:// matching the system's own debt definition everywhere else (idx_async_jobs_
1140:// its wake/reacted bits (Rerun's VoidAsyncJobsByToolCallIDs voids a row
1146:// child session still has a running row or unreacted debt (async_jobs or
1150:// R5A-1: a job_kill row that is 'done' with notice_message_id NULL (wake=0,
1155:func (q *Queries) PurgeAsyncJobsOlderThan(ctx context.Context, updatedAt int64) (int64, error) {
1156:	result, err := q.exec(ctx, q.purgeAsyncJobsOlderThanStmt, purgeAsyncJobsOlderThan, updatedAt)
1196:const rependAsyncJobsByNoticeMessageIDs = `-- name: RependAsyncJobsByNoticeMessageIDs :execrows
1197:UPDATE async_jobs SET delivery = 'pending', reacted = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
1201:type RependAsyncJobsByNoticeMessageIDsParams struct {
1220:func (q *Queries) RependAsyncJobsByNoticeMessageIDs(ctx context.Context, arg RependAsyncJobsByNoticeMessageIDsParams) (int64, error) {
1221:	query := rependAsyncJobsByNoticeMessageIDs
1241:UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?1
1244:  AND notice_kind = 'job_kill' AND notice_message_id IS NULL
1254:// job_kill tool call finished without its fused result write (an error
1258:// a reused id archives the killed row to another tool_call_id while job_kill
1269:UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
1271:  AND notice_kind = 'job_kill' AND notice_message_id IS NULL
1279:// DUR-11 for job_kill on a dead host (R2A-8): job_kill's transition commits
1333:const setAsyncJobAnnounceMessageID = `-- name: SetAsyncJobAnnounceMessageID :execrows
1334:UPDATE async_jobs SET announce_message_id = ?, updated_at = ?
1338:type SetAsyncJobAnnounceMessageIDParams struct {
1350:func (q *Queries) SetAsyncJobAnnounceMessageID(ctx context.Context, arg SetAsyncJobAnnounceMessageIDParams) (int64, error) {
1351:	result, err := q.exec(ctx, q.setAsyncJobAnnounceMessageIDStmt, setAsyncJobAnnounceMessageID,
1363:const setAsyncJobNoticeMessageID = `-- name: SetAsyncJobNoticeMessageID :execrows
1364:UPDATE async_jobs SET notice_message_id = ?, updated_at = ?
1368:type SetAsyncJobNoticeMessageIDParams struct {
1376:func (q *Queries) SetAsyncJobNoticeMessageID(ctx context.Context, arg SetAsyncJobNoticeMessageIDParams) (int64, error) {
1377:	result, err := q.exec(ctx, q.setAsyncJobNoticeMessageIDStmt, setAsyncJobNoticeMessageID,
1389:const setAsyncJobNoticeMessageIDForClaimIfDone = `-- name: SetAsyncJobNoticeMessageIDForClaimIfDone :execrows
1390:UPDATE async_jobs SET notice_message_id = ?1, updated_at = ?2
1395:type SetAsyncJobNoticeMessageIDForClaimIfDoneParams struct {
1402:// A3 (docs/reviews/2026-09-29-async-phase4-round1.md): job_kill's own
1404:// pull (doc sec.3.2) -- so nothing else ever calls SetAsyncJobNoticeMessageID
1405:// for that row. This fuses job_kill's own tool-result message id onto it in
1408:// carries its result" (Rerun's RependAsyncJobsByNoticeMessageIDs, matched by
1409:// notice_message_id, could otherwise never find a job_kill'd tool call in a
1412:// job_kill is still running, and a tool_call_id lookup would then name the
1415:// won, and no other writer re-pends or names a done job_kill row (Rerun
1419:func (q *Queries) SetAsyncJobNoticeMessageIDForClaimIfDone(ctx context.Context, arg SetAsyncJobNoticeMessageIDForClaimIfDoneParams) (int64, error) {
1420:	result, err := q.exec(ctx, q.setAsyncJobNoticeMessageIDForClaimIfDoneStmt, setAsyncJobNoticeMessageIDForClaimIfDone,
1432:const setAsyncJobsWakeZeroPendingForOwners = `-- name: SetAsyncJobsWakeZeroPendingForOwners :execrows
1433:UPDATE async_jobs SET wake = 0, updated_at = ?
1437:type SetAsyncJobsWakeZeroPendingForOwnersParams struct {
1445:func (q *Queries) SetAsyncJobsWakeZeroPendingForOwners(ctx context.Context, arg SetAsyncJobsWakeZeroPendingForOwnersParams) (int64, error) {
1446:	query := setAsyncJobsWakeZeroPendingForOwners
1464:const settleAsyncJobReactedFailedForSnapshotRow = `-- name: SettleAsyncJobReactedFailedForSnapshotRow :execrows
1465:UPDATE async_jobs SET reacted = 1, reacted_failed = 1, updated_at = ?1
1471:type SettleAsyncJobReactedFailedForSnapshotRowParams struct {
1483:// IncrementAsyncJobWakeAttemptsForSnapshotRow (R2A-4/R2A-5), so a late settle
1487:// (MarkAsyncJobsReactedForOwner): it is set ONLY here, never by a real
1490:func (q *Queries) SettleAsyncJobReactedFailedForSnapshotRow(ctx context.Context, arg SettleAsyncJobReactedFailedForSnapshotRowParams) (int64, error) {
1491:	result, err := q.exec(ctx, q.settleAsyncJobReactedFailedForSnapshotRowStmt, settleAsyncJobReactedFailedForSnapshotRow,
1504:const transitionAsyncJobTerminalPreserveVoid = `-- name: TransitionAsyncJobTerminalPreserveVoid :one
1505:UPDATE async_jobs
1513:type TransitionAsyncJobTerminalPreserveVoidParams struct {
1531:// rows affected and must re-read the row (GetAsyncJob) and accept whatever
1535:// this replaced -- TransitionAsyncJobTerminal -- is gone, not a second,
1538:// job_kill racing the history truncation).
1541:// cause passes 'pending' except step 6's job_kill, which passes 'done' once
1545:// anything while still running) except job_kill, which passes 1 in the SAME
1546:// statement -- so a job_kill row is neither debt nor a future notice the
1552:// the store's Go layer (AsyncJobStore.Transition) first reads the row's
1556:func (q *Queries) TransitionAsyncJobTerminalPreserveVoid(ctx context.Context, arg TransitionAsyncJobTerminalPreserveVoidParams) (AsyncJob, error) {
1557:	row := q.queryRow(ctx, q.transitionAsyncJobTerminalPreserveVoidStmt, transitionAsyncJobTerminalPreserveVoid,
1570:	var i AsyncJob
1605:        SELECT 1 FROM async_jobs
1622:// partial debt indexes (idx_async_jobs_debt/idx_session_notices_debt,
1625:// async_jobs is narrowed by the (owner_session_id, tool_call_id) primary key
1635:const voidAsyncJobsByAnnounceMessageIDs = `-- name: VoidAsyncJobsByAnnounceMessageIDs :many
1636:UPDATE async_jobs SET delivery = 'void', updated_at = ?1
1641:type VoidAsyncJobsByAnnounceMessageIDsParams struct {
1647:type VoidAsyncJobsByAnnounceMessageIDsRow struct {
1659:// existing 'void' (TransitionAsyncJobTerminalPreserveVoid), so writing void
1665:func (q *Queries) VoidAsyncJobsByAnnounceMessageIDs(ctx context.Context, arg VoidAsyncJobsByAnnounceMessageIDsParams) ([]VoidAsyncJobsByAnnounceMessageIDsRow, error) {
1666:	query := voidAsyncJobsByAnnounceMessageIDs
1683:	items := []VoidAsyncJobsByAnnounceMessageIDsRow{}
1685:		var i VoidAsyncJobsByAnnounceMessageIDsRow
1705:const voidAsyncJobsByToolCallIDs = `-- name: VoidAsyncJobsByToolCallIDs :many
1706:UPDATE async_jobs SET delivery = 'void', updated_at = ?
1711:type VoidAsyncJobsByToolCallIDsParams struct {
1717:type VoidAsyncJobsByToolCallIDsRow struct {
1728:func (q *Queries) VoidAsyncJobsByToolCallIDs(ctx context.Context, arg VoidAsyncJobsByToolCallIDsParams) ([]VoidAsyncJobsByToolCallIDsRow, error) {
1729:	query := voidAsyncJobsByToolCallIDs
1746:	items := []VoidAsyncJobsByToolCallIDsRow{}
1748:		var i VoidAsyncJobsByToolCallIDsRow
1768:const voidUndeliveredAsyncJobsForOwner = `-- name: VoidUndeliveredAsyncJobsForOwner :execrows
1769:UPDATE async_jobs SET delivery = 'void', reacted_failed = 0, updated_at = ?
1773:type VoidUndeliveredAsyncJobsForOwnerParams struct {
1785:func (q *Queries) VoidUndeliveredAsyncJobsForOwner(ctx context.Context, arg VoidUndeliveredAsyncJobsForOwnerParams) (int64, error) {
1786:	result, err := q.exec(ctx, q.voidUndeliveredAsyncJobsForOwnerStmt, voidUndeliveredAsyncJobsForOwner, arg.UpdatedAt, arg.OwnerSessionID)
```

### `internal/db/async_phase4_schema_test.go` (31 hits)

```
15:// (FK target for async_jobs.owner_session_id / session_notices.owner).
31:func insertRunningAsyncJob(t *testing.T, ctx context.Context, conn *sql.DB, q *Queries, toolCallID, childSessionID, hostID string) error {
37:	_, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
64:	job, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
109:	requireTableExists(t, ctx, conn, "async_jobs", true)
121:	requireTableExists(t, ctx, conn, "async_jobs", false)
129:	requireTableExists(t, ctx, conn, "async_jobs", true)
146:// TestAsyncJobs_ChildSessionUniquePartialIndex is a revert-check-backed
147:// regression test for ASYNC-01 (doc sec.3.8): at most one RUNNING async_jobs
154:func TestAsyncJobs_ChildSessionUniquePartialIndex(t *testing.T) {
160:	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-a", "child-1", "host-1"))
162:	err = insertRunningAsyncJob(t, ctx, conn, q, "call-b", "child-1", "host-1")
168:		`UPDATE async_jobs SET state = 'completed', delivery = 'pending', updated_at = ? WHERE tool_call_id = 'call-a'`,
175:	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-c", "child-1", "host-1"),
202:// across both tables: no debt initially, debt appears from an async_jobs
215:	_, err = q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
220:	// Realistic ack-gate ordering: "started" commits (MarkAsyncJobAnnounced)
222:	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
226:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
238:	rows, err := q.MarkAsyncJobsReactedForOwner(ctx, MarkAsyncJobsReactedForOwnerParams{
244:	_, err = conn.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done' WHERE tool_call_id = 'call-1'`)
246:	rows, err = q.MarkAsyncJobsReactedForOwner(ctx, MarkAsyncJobsReactedForOwnerParams{
256:	// A session_notices row alone (no async_jobs row) must also report debt
273:// (DUR-7) and the drain pull already skips it (ListPendingAsyncJobNoticesForOwner/
274:// PullPendingAsyncJobNotice both require announced=1), so without this
283:	_, err = q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
291:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
303:	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
318:// comment on DeleteAsyncHostIfNoJobs in sql/async_jobs.sql). The fix
331:	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-1", "", "host-busy"))
335:	assert.EqualValues(t, 0, rows, "a host with a referencing async_jobs row must NOT be deleted")
```

### `internal/db/async_phase4_settle_failure_test.go` (42 hits)

```
12:// makeDoneAsyncJob claims and terminal-transitions a job to a
17:func makeDoneAsyncJob(t *testing.T, ctx context.Context, q *Queries, owner, toolCallID, hostID string) string {
20:	_, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
25:	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
29:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
41:		`UPDATE async_jobs SET delivery = 'done', notice_message_id = ? WHERE owner_session_id = ? AND tool_call_id = ?`,
48:// the helpers need (there is no PullPendingAsyncJobNotice-free way to move a
57:func snapshotIncrement(claimID, toolCallID, msgID string) IncrementAsyncJobWakeAttemptsForSnapshotRowParams {
58:	return IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
63:func snapshotSettle(claimID, toolCallID, msgID string) SettleAsyncJobReactedFailedForSnapshotRowParams {
64:	return SettleAsyncJobReactedFailedForSnapshotRowParams{
69:// TestIncrementAsyncJobWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow pins the
75:// comparison or `delivery = 'done'` from the query (sql/async_jobs.sql,
78:func TestIncrementAsyncJobWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
84:	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
85:	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")
88:	for name, p := range map[string]IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
92:		rows, err := q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, p)
97:	rows, err := q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, snapshotIncrement(claim1, "call-1", "msg-call-1"))
101:	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
104:	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
108:	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
110:	rows, err = q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, snapshotIncrement(claim1, "call-1", "msg-call-1"))
115:// TestSettleAsyncJobReactedFailedForSnapshotRow_OnlyTheSnapshotRow is the
121:// SettleAsyncJobReactedFailedForSnapshotRow.
122:func TestSettleAsyncJobReactedFailedForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
128:	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
129:	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")
132:	for name, p := range map[string]SettleAsyncJobReactedFailedForSnapshotRowParams{
136:		rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, p)
140:	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'pending' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
142:	rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
145:	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
148:	rows, err = q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
152:	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
157:	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
167:func TestListReactedFailedAsyncJobsForOwner(t *testing.T) {
173:	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
174:	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")
176:	_, err = q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
179:	failed, err := q.ListReactedFailedAsyncJobsForOwner(ctx, "sess-1")
227:// session_notices half of the async_jobs settle test.
```

### `internal/db/db.go` (217 hits)

```
39:	if q.archiveAsyncJobToolCallIDStmt, err = db.PrepareContext(ctx, archiveAsyncJobToolCallID); err != nil {
40:		return nil, fmt.Errorf("error preparing query ArchiveAsyncJobToolCallID: %w", err)
42:	if q.asyncJobDebtOwnersStmt, err = db.PrepareContext(ctx, asyncJobDebtOwners); err != nil {
43:		return nil, fmt.Errorf("error preparing query AsyncJobDebtOwners: %w", err)
162:	if q.claimAsyncJobStmt, err = db.PrepareContext(ctx, claimAsyncJob); err != nil {
163:		return nil, fmt.Errorf("error preparing query ClaimAsyncJob: %w", err)
183:	if q.countAsyncJobsOlderThanStmt, err = db.PrepareContext(ctx, countAsyncJobsOlderThan); err != nil {
184:		return nil, fmt.Errorf("error preparing query CountAsyncJobsOlderThan: %w", err)
258:	if q.deleteTerminalUnannouncedAsyncJobsForHostStmt, err = db.PrepareContext(ctx, deleteTerminalUnannouncedAsyncJobsForHost); err != nil {
259:		return nil, fmt.Errorf("error preparing query DeleteTerminalUnannouncedAsyncJobsForHost: %w", err)
261:	if q.deleteUnannouncedAsyncJobStmt, err = db.PrepareContext(ctx, deleteUnannouncedAsyncJob); err != nil {
262:		return nil, fmt.Errorf("error preparing query DeleteUnannouncedAsyncJob: %w", err)
264:	if q.deleteUnannouncedAsyncJobForClaimStmt, err = db.PrepareContext(ctx, deleteUnannouncedAsyncJobForClaim); err != nil {
265:		return nil, fmt.Errorf("error preparing query DeleteUnannouncedAsyncJobForClaim: %w", err)
267:	if q.deliverAsyncJobInlineStmt, err = db.PrepareContext(ctx, deliverAsyncJobInline); err != nil {
268:		return nil, fmt.Errorf("error preparing query DeliverAsyncJobInline: %w", err)
279:	if q.getAsyncJobStmt, err = db.PrepareContext(ctx, getAsyncJob); err != nil {
280:		return nil, fmt.Errorf("error preparing query GetAsyncJob: %w", err)
282:	if q.getAsyncJobByClaimIDStmt, err = db.PrepareContext(ctx, getAsyncJobByClaimID); err != nil {
283:		return nil, fmt.Errorf("error preparing query GetAsyncJobByClaimID: %w", err)
333:	if q.getRunningAsyncJobByChildSessionStmt, err = db.PrepareContext(ctx, getRunningAsyncJobByChildSession); err != nil {
334:		return nil, fmt.Errorf("error preparing query GetRunningAsyncJobByChildSession: %w", err)
378:	if q.incrementAsyncJobWakeAttemptsForSnapshotRowStmt, err = db.PrepareContext(ctx, incrementAsyncJobWakeAttemptsForSnapshotRow); err != nil {
379:		return nil, fmt.Errorf("error preparing query IncrementAsyncJobWakeAttemptsForSnapshotRow: %w", err)
414:	if q.listAsyncJobsForOwnerStmt, err = db.PrepareContext(ctx, listAsyncJobsForOwner); err != nil {
415:		return nil, fmt.Errorf("error preparing query ListAsyncJobsForOwner: %w", err)
465:	if q.listPendingAsyncJobNoticesForOwnerStmt, err = db.PrepareContext(ctx, listPendingAsyncJobNoticesForOwner); err != nil {
466:		return nil, fmt.Errorf("error preparing query ListPendingAsyncJobNoticesForOwner: %w", err)
483:	if q.listReactedFailedAsyncJobsForOwnerStmt, err = db.PrepareContext(ctx, listReactedFailedAsyncJobsForOwner); err != nil {
484:		return nil, fmt.Errorf("error preparing query ListReactedFailedAsyncJobsForOwner: %w", err)
489:	if q.listRunningAsyncJobsForHostStmt, err = db.PrepareContext(ctx, listRunningAsyncJobsForHost); err != nil {
490:		return nil, fmt.Errorf("error preparing query ListRunningAsyncJobsForHost: %w", err)
492:	if q.listRunningAsyncJobsForOwnersStmt, err = db.PrepareContext(ctx, listRunningAsyncJobsForOwners); err != nil {
493:		return nil, fmt.Errorf("error preparing query ListRunningAsyncJobsForOwners: %w", err)
528:	if q.markAsyncJobAnnouncedStmt, err = db.PrepareContext(ctx, markAsyncJobAnnounced); err != nil {
529:		return nil, fmt.Errorf("error preparing query MarkAsyncJobAnnounced: %w", err)
531:	if q.markAsyncJobsReactedForOwnerStmt, err = db.PrepareContext(ctx, markAsyncJobsReactedForOwner); err != nil {
532:		return nil, fmt.Errorf("error preparing query MarkAsyncJobsReactedForOwner: %w", err)
552:	if q.pullPendingAsyncJobNoticeStmt, err = db.PrepareContext(ctx, pullPendingAsyncJobNotice); err != nil {
553:		return nil, fmt.Errorf("error preparing query PullPendingAsyncJobNotice: %w", err)
558:	if q.purgeAsyncJobsOlderThanStmt, err = db.PrepareContext(ctx, purgeAsyncJobsOlderThan); err != nil {
559:		return nil, fmt.Errorf("error preparing query PurgeAsyncJobsOlderThan: %w", err)
582:	if q.rependAsyncJobsByNoticeMessageIDsStmt, err = db.PrepareContext(ctx, rependAsyncJobsByNoticeMessageIDs); err != nil {
583:		return nil, fmt.Errorf("error preparing query RependAsyncJobsByNoticeMessageIDs: %w", err)
606:	if q.setAsyncJobAnnounceMessageIDStmt, err = db.PrepareContext(ctx, setAsyncJobAnnounceMessageID); err != nil {
607:		return nil, fmt.Errorf("error preparing query SetAsyncJobAnnounceMessageID: %w", err)
609:	if q.setAsyncJobNoticeMessageIDStmt, err = db.PrepareContext(ctx, setAsyncJobNoticeMessageID); err != nil {
610:		return nil, fmt.Errorf("error preparing query SetAsyncJobNoticeMessageID: %w", err)
612:	if q.setAsyncJobNoticeMessageIDForClaimIfDoneStmt, err = db.PrepareContext(ctx, setAsyncJobNoticeMessageIDForClaimIfDone); err != nil {
613:		return nil, fmt.Errorf("error preparing query SetAsyncJobNoticeMessageIDForClaimIfDone: %w", err)
615:	if q.setAsyncJobsWakeZeroPendingForOwnersStmt, err = db.PrepareContext(ctx, setAsyncJobsWakeZeroPendingForOwners); err != nil {
616:		return nil, fmt.Errorf("error preparing query SetAsyncJobsWakeZeroPendingForOwners: %w", err)
627:	if q.settleAsyncJobReactedFailedForSnapshotRowStmt, err = db.PrepareContext(ctx, settleAsyncJobReactedFailedForSnapshotRow); err != nil {
628:		return nil, fmt.Errorf("error preparing query SettleAsyncJobReactedFailedForSnapshotRow: %w", err)
651:	if q.transitionAsyncJobTerminalPreserveVoidStmt, err = db.PrepareContext(ctx, transitionAsyncJobTerminalPreserveVoid); err != nil {
652:		return nil, fmt.Errorf("error preparing query TransitionAsyncJobTerminalPreserveVoid: %w", err)
690:	if q.voidAsyncJobsByAnnounceMessageIDsStmt, err = db.PrepareContext(ctx, voidAsyncJobsByAnnounceMessageIDs); err != nil {
691:		return nil, fmt.Errorf("error preparing query VoidAsyncJobsByAnnounceMessageIDs: %w", err)
693:	if q.voidAsyncJobsByToolCallIDsStmt, err = db.PrepareContext(ctx, voidAsyncJobsByToolCallIDs); err != nil {
694:		return nil, fmt.Errorf("error preparing query VoidAsyncJobsByToolCallIDs: %w", err)
699:	if q.voidUndeliveredAsyncJobsForOwnerStmt, err = db.PrepareContext(ctx, voidUndeliveredAsyncJobsForOwner); err != nil {
700:		return nil, fmt.Errorf("error preparing query VoidUndeliveredAsyncJobsForOwner: %w", err)
736:	if q.archiveAsyncJobToolCallIDStmt != nil {
737:		if cerr := q.archiveAsyncJobToolCallIDStmt.Close(); cerr != nil {
738:			err = fmt.Errorf("error closing archiveAsyncJobToolCallIDStmt: %w", cerr)
741:	if q.asyncJobDebtOwnersStmt != nil {
742:		if cerr := q.asyncJobDebtOwnersStmt.Close(); cerr != nil {
743:			err = fmt.Errorf("error closing asyncJobDebtOwnersStmt: %w", cerr)
941:	if q.claimAsyncJobStmt != nil {
942:		if cerr := q.claimAsyncJobStmt.Close(); cerr != nil {
943:			err = fmt.Errorf("error closing claimAsyncJobStmt: %w", cerr)
976:	if q.countAsyncJobsOlderThanStmt != nil {
977:		if cerr := q.countAsyncJobsOlderThanStmt.Close(); cerr != nil {
978:			err = fmt.Errorf("error closing countAsyncJobsOlderThanStmt: %w", cerr)
1101:	if q.deleteTerminalUnannouncedAsyncJobsForHostStmt != nil {
1102:		if cerr := q.deleteTerminalUnannouncedAsyncJobsForHostStmt.Close(); cerr != nil {
1103:			err = fmt.Errorf("error closing deleteTerminalUnannouncedAsyncJobsForHostStmt: %w", cerr)
1106:	if q.deleteUnannouncedAsyncJobStmt != nil {
1107:		if cerr := q.deleteUnannouncedAsyncJobStmt.Close(); cerr != nil {
1108:			err = fmt.Errorf("error closing deleteUnannouncedAsyncJobStmt: %w", cerr)
1111:	if q.deleteUnannouncedAsyncJobForClaimStmt != nil {
1112:		if cerr := q.deleteUnannouncedAsyncJobForClaimStmt.Close(); cerr != nil {
1113:			err = fmt.Errorf("error closing deleteUnannouncedAsyncJobForClaimStmt: %w", cerr)
1116:	if q.deliverAsyncJobInlineStmt != nil {
1117:		if cerr := q.deliverAsyncJobInlineStmt.Close(); cerr != nil {
1118:			err = fmt.Errorf("error closing deliverAsyncJobInlineStmt: %w", cerr)
1136:	if q.getAsyncJobStmt != nil {
1137:		if cerr := q.getAsyncJobStmt.Close(); cerr != nil {
1138:			err = fmt.Errorf("error closing getAsyncJobStmt: %w", cerr)
1141:	if q.getAsyncJobByClaimIDStmt != nil {
1142:		if cerr := q.getAsyncJobByClaimIDStmt.Close(); cerr != nil {
1143:			err = fmt.Errorf("error closing getAsyncJobByClaimIDStmt: %w", cerr)
1226:	if q.getRunningAsyncJobByChildSessionStmt != nil {
1227:		if cerr := q.getRunningAsyncJobByChildSessionStmt.Close(); cerr != nil {
1228:			err = fmt.Errorf("error closing getRunningAsyncJobByChildSessionStmt: %w", cerr)
1301:	if q.incrementAsyncJobWakeAttemptsForSnapshotRowStmt != nil {
1302:		if cerr := q.incrementAsyncJobWakeAttemptsForSnapshotRowStmt.Close(); cerr != nil {
1303:			err = fmt.Errorf("error closing incrementAsyncJobWakeAttemptsForSnapshotRowStmt: %w", cerr)
1361:	if q.listAsyncJobsForOwnerStmt != nil {
1362:		if cerr := q.listAsyncJobsForOwnerStmt.Close(); cerr != nil {
1363:			err = fmt.Errorf("error closing listAsyncJobsForOwnerStmt: %w", cerr)
1446:	if q.listPendingAsyncJobNoticesForOwnerStmt != nil {
1447:		if cerr := q.listPendingAsyncJobNoticesForOwnerStmt.Close(); cerr != nil {
1448:			err = fmt.Errorf("error closing listPendingAsyncJobNoticesForOwnerStmt: %w", cerr)
1476:	if q.listReactedFailedAsyncJobsForOwnerStmt != nil {
1477:		if cerr := q.listReactedFailedAsyncJobsForOwnerStmt.Close(); cerr != nil {
1478:			err = fmt.Errorf("error closing listReactedFailedAsyncJobsForOwnerStmt: %w", cerr)
1486:	if q.listRunningAsyncJobsForHostStmt != nil {
1487:		if cerr := q.listRunningAsyncJobsForHostStmt.Close(); cerr != nil {
1488:			err = fmt.Errorf("error closing listRunningAsyncJobsForHostStmt: %w", cerr)
1491:	if q.listRunningAsyncJobsForOwnersStmt != nil {
1492:		if cerr := q.listRunningAsyncJobsForOwnersStmt.Close(); cerr != nil {
1493:			err = fmt.Errorf("error closing listRunningAsyncJobsForOwnersStmt: %w", cerr)
1551:	if q.markAsyncJobAnnouncedStmt != nil {
1552:		if cerr := q.markAsyncJobAnnouncedStmt.Close(); cerr != nil {
1553:			err = fmt.Errorf("error closing markAsyncJobAnnouncedStmt: %w", cerr)
1556:	if q.markAsyncJobsReactedForOwnerStmt != nil {
1557:		if cerr := q.markAsyncJobsReactedForOwnerStmt.Close(); cerr != nil {
1558:			err = fmt.Errorf("error closing markAsyncJobsReactedForOwnerStmt: %w", cerr)
1591:	if q.pullPendingAsyncJobNoticeStmt != nil {
1592:		if cerr := q.pullPendingAsyncJobNoticeStmt.Close(); cerr != nil {
1593:			err = fmt.Errorf("error closing pullPendingAsyncJobNoticeStmt: %w", cerr)
1601:	if q.purgeAsyncJobsOlderThanStmt != nil {
1602:		if cerr := q.purgeAsyncJobsOlderThanStmt.Close(); cerr != nil {
1603:			err = fmt.Errorf("error closing purgeAsyncJobsOlderThanStmt: %w", cerr)
1641:	if q.rependAsyncJobsByNoticeMessageIDsStmt != nil {
1642:		if cerr := q.rependAsyncJobsByNoticeMessageIDsStmt.Close(); cerr != nil {
1643:			err = fmt.Errorf("error closing rependAsyncJobsByNoticeMessageIDsStmt: %w", cerr)
1681:	if q.setAsyncJobAnnounceMessageIDStmt != nil {
1682:		if cerr := q.setAsyncJobAnnounceMessageIDStmt.Close(); cerr != nil {
1683:			err = fmt.Errorf("error closing setAsyncJobAnnounceMessageIDStmt: %w", cerr)
1686:	if q.setAsyncJobNoticeMessageIDStmt != nil {
1687:		if cerr := q.setAsyncJobNoticeMessageIDStmt.Close(); cerr != nil {
1688:			err = fmt.Errorf("error closing setAsyncJobNoticeMessageIDStmt: %w", cerr)
1691:	if q.setAsyncJobNoticeMessageIDForClaimIfDoneStmt != nil {
1692:		if cerr := q.setAsyncJobNoticeMessageIDForClaimIfDoneStmt.Close(); cerr != nil {
1693:			err = fmt.Errorf("error closing setAsyncJobNoticeMessageIDForClaimIfDoneStmt: %w", cerr)
1696:	if q.setAsyncJobsWakeZeroPendingForOwnersStmt != nil {
1697:		if cerr := q.setAsyncJobsWakeZeroPendingForOwnersStmt.Close(); cerr != nil {
1698:			err = fmt.Errorf("error closing setAsyncJobsWakeZeroPendingForOwnersStmt: %w", cerr)
1716:	if q.settleAsyncJobReactedFailedForSnapshotRowStmt != nil {
1717:		if cerr := q.settleAsyncJobReactedFailedForSnapshotRowStmt.Close(); cerr != nil {
1718:			err = fmt.Errorf("error closing settleAsyncJobReactedFailedForSnapshotRowStmt: %w", cerr)
1756:	if q.transitionAsyncJobTerminalPreserveVoidStmt != nil {
1757:		if cerr := q.transitionAsyncJobTerminalPreserveVoidStmt.Close(); cerr != nil {
1758:			err = fmt.Errorf("error closing transitionAsyncJobTerminalPreserveVoidStmt: %w", cerr)
1821:	if q.voidAsyncJobsByAnnounceMessageIDsStmt != nil {
1822:		if cerr := q.voidAsyncJobsByAnnounceMessageIDsStmt.Close(); cerr != nil {
1823:			err = fmt.Errorf("error closing voidAsyncJobsByAnnounceMessageIDsStmt: %w", cerr)
1826:	if q.voidAsyncJobsByToolCallIDsStmt != nil {
1827:		if cerr := q.voidAsyncJobsByToolCallIDsStmt.Close(); cerr != nil {
1828:			err = fmt.Errorf("error closing voidAsyncJobsByToolCallIDsStmt: %w", cerr)
1836:	if q.voidUndeliveredAsyncJobsForOwnerStmt != nil {
1837:		if cerr := q.voidUndeliveredAsyncJobsForOwnerStmt.Close(); cerr != nil {
1838:			err = fmt.Errorf("error closing voidUndeliveredAsyncJobsForOwnerStmt: %w", cerr)
1899:	archiveAsyncJobToolCallIDStmt                        *sql.Stmt
1900:	asyncJobDebtOwnersStmt                               *sql.Stmt
1940:	claimAsyncJobStmt                                    *sql.Stmt
1947:	countAsyncJobsOlderThanStmt                          *sql.Stmt
1972:	deleteTerminalUnannouncedAsyncJobsForHostStmt        *sql.Stmt
1973:	deleteUnannouncedAsyncJobStmt                        *sql.Stmt
1974:	deleteUnannouncedAsyncJobForClaimStmt                *sql.Stmt
1975:	deliverAsyncJobInlineStmt                            *sql.Stmt
1979:	getAsyncJobStmt                                      *sql.Stmt
1980:	getAsyncJobByClaimIDStmt                             *sql.Stmt
1997:	getRunningAsyncJobByChildSessionStmt                 *sql.Stmt
2012:	incrementAsyncJobWakeAttemptsForSnapshotRowStmt      *sql.Stmt
2024:	listAsyncJobsForOwnerStmt                            *sql.Stmt
2041:	listPendingAsyncJobNoticesForOwnerStmt               *sql.Stmt
2047:	listReactedFailedAsyncJobsForOwnerStmt               *sql.Stmt
2049:	listRunningAsyncJobsForHostStmt                      *sql.Stmt
2050:	listRunningAsyncJobsForOwnersStmt                    *sql.Stmt
2062:	markAsyncJobAnnouncedStmt                            *sql.Stmt
2063:	markAsyncJobsReactedForOwnerStmt                     *sql.Stmt
2070:	pullPendingAsyncJobNoticeStmt                        *sql.Stmt
2072:	purgeAsyncJobsOlderThanStmt                          *sql.Stmt
2080:	rependAsyncJobsByNoticeMessageIDsStmt                *sql.Stmt
2088:	setAsyncJobAnnounceMessageIDStmt                     *sql.Stmt
2089:	setAsyncJobNoticeMessageIDStmt                       *sql.Stmt
2090:	setAsyncJobNoticeMessageIDForClaimIfDoneStmt         *sql.Stmt
2091:	setAsyncJobsWakeZeroPendingForOwnersStmt             *sql.Stmt
2095:	settleAsyncJobReactedFailedForSnapshotRowStmt        *sql.Stmt
2103:	transitionAsyncJobTerminalPreserveVoidStmt           *sql.Stmt
2116:	voidAsyncJobsByAnnounceMessageIDsStmt                *sql.Stmt
2117:	voidAsyncJobsByToolCallIDsStmt                       *sql.Stmt
2119:	voidUndeliveredAsyncJobsForOwnerStmt                 *sql.Stmt
2133:		archiveAsyncJobToolCallIDStmt:            q.archiveAsyncJobToolCallIDStmt,
2134:		asyncJobDebtOwnersStmt:                   q.asyncJobDebtOwnersStmt,
2174:		claimAsyncJobStmt:                                    q.claimAsyncJobStmt,
2181:		countAsyncJobsOlderThanStmt:                          q.countAsyncJobsOlderThanStmt,
2206:		deleteTerminalUnannouncedAsyncJobsForHostStmt:        q.deleteTerminalUnannouncedAsyncJobsForHostStmt,
2207:		deleteUnannouncedAsyncJobStmt:                        q.deleteUnannouncedAsyncJobStmt,
2208:		deleteUnannouncedAsyncJobForClaimStmt:                q.deleteUnannouncedAsyncJobForClaimStmt,
2209:		deliverAsyncJobInlineStmt:                            q.deliverAsyncJobInlineStmt,
2213:		getAsyncJobStmt:                                      q.getAsyncJobStmt,
2214:		getAsyncJobByClaimIDStmt:                             q.getAsyncJobByClaimIDStmt,
2231:		getRunningAsyncJobByChildSessionStmt:                 q.getRunningAsyncJobByChildSessionStmt,
2246:		incrementAsyncJobWakeAttemptsForSnapshotRowStmt:      q.incrementAsyncJobWakeAttemptsForSnapshotRowStmt,
2258:		listAsyncJobsForOwnerStmt:                            q.listAsyncJobsForOwnerStmt,
2275:		listPendingAsyncJobNoticesForOwnerStmt:               q.listPendingAsyncJobNoticesForOwnerStmt,
2281:		listReactedFailedAsyncJobsForOwnerStmt:               q.listReactedFailedAsyncJobsForOwnerStmt,
2283:		listRunningAsyncJobsForHostStmt:                      q.listRunningAsyncJobsForHostStmt,
2284:		listRunningAsyncJobsForOwnersStmt:                    q.listRunningAsyncJobsForOwnersStmt,
2296:		markAsyncJobAnnouncedStmt:                            q.markAsyncJobAnnouncedStmt,
2297:		markAsyncJobsReactedForOwnerStmt:                     q.markAsyncJobsReactedForOwnerStmt,
2304:		pullPendingAsyncJobNoticeStmt:                        q.pullPendingAsyncJobNoticeStmt,
2306:		purgeAsyncJobsOlderThanStmt:                          q.purgeAsyncJobsOlderThanStmt,
2314:		rependAsyncJobsByNoticeMessageIDsStmt:                q.rependAsyncJobsByNoticeMessageIDsStmt,
2322:		setAsyncJobAnnounceMessageIDStmt:                     q.setAsyncJobAnnounceMessageIDStmt,
2323:		setAsyncJobNoticeMessageIDStmt:                       q.setAsyncJobNoticeMessageIDStmt,
2324:		setAsyncJobNoticeMessageIDForClaimIfDoneStmt:         q.setAsyncJobNoticeMessageIDForClaimIfDoneStmt,
2325:		setAsyncJobsWakeZeroPendingForOwnersStmt:             q.setAsyncJobsWakeZeroPendingForOwnersStmt,
2329:		settleAsyncJobReactedFailedForSnapshotRowStmt:        q.settleAsyncJobReactedFailedForSnapshotRowStmt,
2337:		transitionAsyncJobTerminalPreserveVoidStmt:           q.transitionAsyncJobTerminalPreserveVoidStmt,
2350:		voidAsyncJobsByAnnounceMessageIDsStmt:                q.voidAsyncJobsByAnnounceMessageIDsStmt,
2351:		voidAsyncJobsByToolCallIDsStmt:                       q.voidAsyncJobsByToolCallIDsStmt,
2353:		voidUndeliveredAsyncJobsForOwnerStmt:                 q.voidUndeliveredAsyncJobsForOwnerStmt,
```

### `internal/db/messages.sql.go` (1 hits)

```
245:// simply absent), which the caller reconciles async_jobs against.
```

### `internal/db/migrate_policy.go` (1 hits)

```
81:	// live-host gate (CheckLiveHosts) here for the destructive async_jobs
```

### `internal/db/migrate_policy_test.go` (1 hits)

```
23:// copies columns from: the bg_shell rebuild of async_jobs (20261005) selects
```

### `internal/db/migrations/20260928000002_add_async_phase4_core.sql` (27 hits)

```
11:-- async_jobs, no timestamp is ever consulted to decide whether a host is
21:-- async_jobs is the durable ledger for async tasks (bash/run_command/agent/
35:CREATE TABLE IF NOT EXISTS async_jobs (
93:CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
94:CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
95:CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
97:CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
98:    ON async_jobs (child_session_id)
102:-- async_jobs specifically (doc sec.3.4: "task rows with announced=1" --
107:CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
108:    ON async_jobs (owner_session_id)
112:-- same query shape as the removed idx_async_jobs_retention; harmless to add.
113:CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
114:    ON async_jobs (state, updated_at)
117:-- session_notices carries notices that have no async_jobs row (supervision,
119:-- doc sec.2). Same outbox shape as async_jobs' delivery/wake/reacted fields.
131:    -- schema. Job-row notices keep origin_cli on async_jobs (async_jobs.sql),
137:    -- Same settle-by-failure pair as async_jobs (doc sec.3.4) -- see the
141:    -- job_tool_call_id pairs with `owner` (== async_jobs.owner_session_id)
142:    -- to name the async_jobs row this notice's "task still running?" void
164:DROP INDEX IF EXISTS idx_async_jobs_retention;
165:DROP INDEX IF EXISTS idx_async_jobs_debt;
166:DROP INDEX IF EXISTS idx_async_jobs_child_running;
167:DROP INDEX IF EXISTS idx_async_jobs_running;
168:DROP INDEX IF EXISTS idx_async_jobs_owner_pending;
169:DROP INDEX IF EXISTS idx_async_jobs_host_id;
170:DROP TABLE IF EXISTS async_jobs;
```

### `internal/db/migrations/20260929000002_add_claim_id_to_async_jobs.sql` (5 hits)

```
5:-- deleted (e.g. DeleteUnannouncedAsyncJob after a failed "started" write, or
15:-- (AsyncJobStore.Transition) whenever the caller supplies one: a stale
20:-- Default '' for existing rows (pre-migration): AsyncJobStore.Transition
26:ALTER TABLE async_jobs ADD COLUMN claim_id TEXT NOT NULL DEFAULT '';
31:ALTER TABLE async_jobs DROP COLUMN claim_id;
```

### `internal/db/migrations/20260929000003_add_announce_message_id_to_async_jobs.sql` (2 hits)

```
11:ALTER TABLE async_jobs ADD COLUMN announce_message_id TEXT;
16:ALTER TABLE async_jobs DROP COLUMN announce_message_id;
```

### `internal/db/migrations/20260929000004_add_session_drivers.sql` (1 hits)

```
10:-- async_jobs.host_id): the host row may be reaped independently and a dead
```

### `internal/db/migrations/20260929000005_add_wake_schedules.sql` (1 hits)

```
6:-- owned by its creating session; the FK cascades like async_jobs'
```

### `internal/db/migrations/20261005000001_bg_shell_jobs.sql` (31 hits)

```
5:-- response with NO async_jobs row -- its completion was a session_notices
10:-- (AsyncJobStore.Transition, DUR-1).
17:CREATE TABLE async_jobs_new (
48:INSERT INTO async_jobs_new (
60:FROM async_jobs;
61:DROP TABLE async_jobs;
62:ALTER TABLE async_jobs_new RENAME TO async_jobs;
63:CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
64:CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
65:CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
66:CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
67:    ON async_jobs (child_session_id)
69:CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
70:    ON async_jobs (owner_session_id)
72:CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
73:    ON async_jobs (state, updated_at)
82:UPDATE async_jobs SET state = 'cancelled', delivery = 'done', wake = 0, reacted = 1, updated_at = strftime('%s', 'now') WHERE kind = 'bg_shell' AND state = 'running';
83:DELETE FROM async_jobs WHERE kind = 'bg_shell';
84:CREATE TABLE async_jobs_old_schema (
115:INSERT INTO async_jobs_old_schema SELECT * FROM async_jobs;
116:DROP TABLE async_jobs;
117:ALTER TABLE async_jobs_old_schema RENAME TO async_jobs;
118:CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
119:CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
120:CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
121:CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
122:    ON async_jobs (child_session_id)
124:CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
125:    ON async_jobs (owner_session_id)
127:CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
128:    ON async_jobs (state, updated_at)
```

### `internal/db/models.go` (1 hits)

```
18:type AsyncJob struct {
```

### `internal/db/querier.go` (88 hits)

```
47:	// delivery check: Rerun's VoidAsyncJobsByToolCallIDs can set delivery='void'
52:	ArchiveAsyncJobToolCallID(ctx context.Context, arg ArchiveAsyncJobToolCallIDParams) (int64, error)
53:	// The async_jobs half of the batched reaction-debt read (architect decision
59:	AsyncJobDebtOwners(ctx context.Context, ownerIds []string) ([]string, error)
61:	// idx_async_jobs_debt and idx_session_notices_debt "(owner) WHERE wake=1
62:	// AND reacted=0 AND delivery<>'void'" on both tables. The async_jobs branch
67:	// (ListPendingAsyncJobNoticesForOwner/PullPendingAsyncJobNotice) already
70:	// then aborts, the row is deleted (DeleteUnannouncedAsyncJob) and the turn
181:	// (GetAsyncJob), same as before.
191:	// into TransitionAsyncJobTerminalPreserveVoid's CAS -- closes the ABA where
195:	ClaimAsyncJob(ctx context.Context, arg ClaimAsyncJobParams) (AsyncJob, error)
230:	// Same predicate as PurgeAsyncJobsOlderThan, read-only, for
232:	CountAsyncJobsOlderThan(ctx context.Context, updatedAt int64) (int64, error)
271:	// Deletes a host row that no async_jobs row references (doc sec.3.6): the owner
342:	// Host exit (AsyncJobStore.Close) or dead-host purge: every marker naming
350:	// simply absent), which the caller reconciles async_jobs against.
357:	DeleteTerminalUnannouncedAsyncJobsForHost(ctx context.Context, hostID string) (int64, error)
362:	DeleteUnannouncedAsyncJob(ctx context.Context, arg DeleteUnannouncedAsyncJobParams) (int64, error)
363:	// Recovery's twin of DeleteUnannouncedAsyncJob (R3A-3): recoverers no longer
368:	DeleteUnannouncedAsyncJobForClaim(ctx context.Context, arg DeleteUnannouncedAsyncJobForClaimParams) (int64, error)
372:	// (AnnounceInlineResult) -- exactly job_kill's DUR-11 shape. CAS on
379:	DeliverAsyncJobInline(ctx context.Context, arg DeliverAsyncJobInlineParams) (int64, error)
391:	GetAsyncJob(ctx context.Context, arg GetAsyncJobParams) (AsyncJob, error)
397:	GetAsyncJobByClaimID(ctx context.Context, arg GetAsyncJobByClaimIDParams) (AsyncJob, error)
449:	GetRunningAsyncJobByChildSession(ctx context.Context, childSessionID sql.NullString) (AsyncJob, error)
527:	IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx context.Context, arg IncrementAsyncJobWakeAttemptsForSnapshotRowParams) (int64, error)
534:	// Notices half of IncrementAsyncJobWakeAttemptsForSnapshotRow (doc sec.3.4,
543:	// session_notices carries notices with no async_jobs row (supervision,
569:	// `label` is the registrant's App label ("app" at both NewAsyncJobStore call
573:	// async_jobs rows of ANY state. Caller still must verify the lock file
577:	ListAsyncJobsForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error)
619:	// died before its "started" result committed, R2A-7), or a job_kill row that
705:	ListPendingAsyncJobNoticesForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error)
730:	ListReactedFailedAsyncJobsForOwner(ctx context.Context, ownerSessionID string) ([]AsyncJob, error)
731:	// Notices half of ListReactedFailedAsyncJobsForOwner: the parent-
737:	ListRunningAsyncJobsForHost(ctx context.Context, hostID string) ([]AsyncJob, error)
744:	ListRunningAsyncJobsForOwners(ctx context.Context, ownerIds []string) ([]AsyncJob, error)
781:	MarkAsyncJobAnnounced(ctx context.Context, arg MarkAsyncJobAnnouncedParams) (int64, error)
787:	MarkAsyncJobsReactedForOwner(ctx context.Context, arg MarkAsyncJobsReactedForOwnerParams) (int64, error)
788:	// Same reaction rule as MarkAsyncJobsReactedForOwner, for the notices table.
827:	// notice_message_id via SetAsyncJobNoticeMessageID, all in the SAME tx. 0
831:	PullPendingAsyncJobNotice(ctx context.Context, arg PullPendingAsyncJobNoticeParams) (AsyncJob, error)
833:	// PullPendingAsyncJobNotice: one transaction per notice, caller INSERTs the
845:	// matching the system's own debt definition everywhere else (idx_async_jobs_
847:	// its wake/reacted bits (Rerun's VoidAsyncJobsByToolCallIDs voids a row
853:	// child session still has a running row or unreacted debt (async_jobs or
857:	// R5A-1: a job_kill row that is 'done' with notice_message_id NULL (wake=0,
862:	PurgeAsyncJobsOlderThan(ctx context.Context, updatedAt int64) (int64, error)
863:	// Same retention pass as PurgeAsyncJobsOlderThan (doc sec.3.7), including
865:	// exactly like the async_jobs twin -- see that query's doc.
902:	RependAsyncJobsByNoticeMessageIDs(ctx context.Context, arg RependAsyncJobsByNoticeMessageIDsParams) (int64, error)
904:	// job_kill tool call finished without its fused result write (an error
908:	// a reused id archives the killed row to another tool_call_id while job_kill
911:	// DUR-11 for job_kill on a dead host (R2A-8): job_kill's transition commits
918:	// RependAsyncJobsByNoticeMessageIDs (doc sec.3.8's "same for session_notices
922:	// mirror RependAsyncJobsByNoticeMessageIDs's own A4/A1 fixes -- see that
928:	// R2A-4: session_notices.job_tool_call_id names its async_jobs row by
948:	SetAsyncJobAnnounceMessageID(ctx context.Context, arg SetAsyncJobAnnounceMessageIDParams) (int64, error)
950:	SetAsyncJobNoticeMessageID(ctx context.Context, arg SetAsyncJobNoticeMessageIDParams) (int64, error)
951:	// A3 (docs/reviews/2026-09-29-async-phase4-round1.md): job_kill's own
953:	// pull (doc sec.3.2) -- so nothing else ever calls SetAsyncJobNoticeMessageID
954:	// for that row. This fuses job_kill's own tool-result message id onto it in
957:	// carries its result" (Rerun's RependAsyncJobsByNoticeMessageIDs, matched by
958:	// notice_message_id, could otherwise never find a job_kill'd tool call in a
961:	// job_kill is still running, and a tool_call_id lookup would then name the
964:	// won, and no other writer re-pends or names a done job_kill row (Rerun
968:	SetAsyncJobNoticeMessageIDForClaimIfDone(ctx context.Context, arg SetAsyncJobNoticeMessageIDForClaimIfDoneParams) (int64, error)
972:	SetAsyncJobsWakeZeroPendingForOwners(ctx context.Context, arg SetAsyncJobsWakeZeroPendingForOwnersParams) (int64, error)
976:	// SetAsyncJobsWakeZeroPendingForOwners.
982:	// IncrementAsyncJobWakeAttemptsForSnapshotRow (R2A-4/R2A-5), so a late settle
986:	// (MarkAsyncJobsReactedForOwner): it is set ONLY here, never by a real
989:	SettleAsyncJobReactedFailedForSnapshotRow(ctx context.Context, arg SettleAsyncJobReactedFailedForSnapshotRowParams) (int64, error)
990:	// Notices half of SettleAsyncJobReactedFailedForSnapshotRow (doc sec.3.4,
1076:	// rows affected and must re-read the row (GetAsyncJob) and accept whatever
1080:	// this replaced -- TransitionAsyncJobTerminal -- is gone, not a second,
1083:	// job_kill racing the history truncation).
1086:	// cause passes 'pending' except step 6's job_kill, which passes 'done' once
1090:	// anything while still running) except job_kill, which passes 1 in the SAME
1091:	// statement -- so a job_kill row is neither debt nor a future notice the
1097:	// the store's Go layer (AsyncJobStore.Transition) first reads the row's
1101:	TransitionAsyncJobTerminalPreserveVoid(ctx context.Context, arg TransitionAsyncJobTerminalPreserveVoidParams) (AsyncJob, error)
1174:	// partial debt indexes (idx_async_jobs_debt/idx_session_notices_debt,
1177:	// async_jobs is narrowed by the (owner_session_id, tool_call_id) primary key
1186:	// existing 'void' (TransitionAsyncJobTerminalPreserveVoid), so writing void
1192:	VoidAsyncJobsByAnnounceMessageIDs(ctx context.Context, arg VoidAsyncJobsByAnnounceMessageIDsParams) ([]VoidAsyncJobsByAnnounceMessageIDsRow, error)
1197:	VoidAsyncJobsByToolCallIDs(ctx context.Context, arg VoidAsyncJobsByToolCallIDsParams) ([]VoidAsyncJobsByToolCallIDsRow, error)
1216:	VoidUndeliveredAsyncJobsForOwner(ctx context.Context, arg VoidUndeliveredAsyncJobsForOwnerParams) (int64, error)
1217:	// session_notices half of VoidUndeliveredAsyncJobsForOwner (`sessions reset`,
```

### `internal/db/session_drivers.sql.go` (1 hits)

```
34:// Host exit (AsyncJobStore.Close) or dead-host purge: every marker naming
```

### `internal/db/session_notices.sql.go` (13 hits)

```
83:// Notices half of IncrementAsyncJobWakeAttemptsForSnapshotRow (doc sec.3.4,
115:// session_notices carries notices with no async_jobs row (supervision,
195:// Notices half of ListReactedFailedAsyncJobsForOwner: the parent-
286:// Same reaction rule as MarkAsyncJobsReactedForOwner, for the notices table.
308:// PullPendingAsyncJobNotice: one transaction per notice, caller INSERTs the
337:// Same retention pass as PurgeAsyncJobsOlderThan (doc sec.3.7), including
339:// exactly like the async_jobs twin -- see that query's doc.
360:// RependAsyncJobsByNoticeMessageIDs (doc sec.3.8's "same for session_notices
364:// mirror RependAsyncJobsByNoticeMessageIDs's own A4/A1 fixes -- see that
400:// R2A-4: session_notices.job_tool_call_id names its async_jobs row by
445:// SetAsyncJobsWakeZeroPendingForOwners.
477:// Notices half of SettleAsyncJobReactedFailedForSnapshotRow (doc sec.3.4,
527:// session_notices half of VoidUndeliveredAsyncJobsForOwner (`sessions reset`,
```

### `internal/db/sql/async_jobs.sql` (123 hits)

```
14:-- `label` is the registrant's App label ("app" at both NewAsyncJobStore call
22:-- died before its "started" result committed, R2A-7), or a job_kill row that
25:SELECT DISTINCT host_id FROM async_jobs
28:   OR (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL);
30:-- name: DeleteTerminalUnannouncedAsyncJobsForHost :execrows
36:DELETE FROM async_jobs WHERE host_id = ? AND announced = 0 AND state != 'running';
39:-- DUR-11 for job_kill on a dead host (R2A-8): job_kill's transition commits
44:UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
46:  AND notice_kind = 'job_kill' AND notice_message_id IS NULL;
50:-- job_kill tool call finished without its fused result write (an error
54:-- a reused id archives the killed row to another tool_call_id while job_kill
56:UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = @updated_at
59:  AND notice_kind = 'job_kill' AND notice_message_id IS NULL;
62:-- Deletes a host row that no async_jobs row references (doc sec.3.6): the owner
77:WHERE id = ? AND NOT EXISTS (SELECT 1 FROM async_jobs WHERE host_id = async_hosts.id);
81:-- async_jobs rows of ANY state. Caller still must verify the lock file
84:WHERE id NOT IN (SELECT DISTINCT host_id FROM async_jobs);
86:-- name: ClaimAsyncJob :one
91:-- (GetAsyncJob), same as before.
101:-- into TransitionAsyncJobTerminalPreserveVoid's CAS -- closes the ABA where
105:INSERT INTO async_jobs (
115:-- name: GetAsyncJob :one
116:SELECT * FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?;
118:-- name: GetRunningAsyncJobByChildSession :one
123:SELECT * FROM async_jobs WHERE child_session_id = ? AND state = 'running';
125:-- name: MarkAsyncJobAnnounced :execrows
131:UPDATE async_jobs SET announced = 1, updated_at = ?
134:-- name: SetAsyncJobAnnounceMessageID :execrows
140:UPDATE async_jobs SET announce_message_id = ?, updated_at = ?
143:-- name: DeleteUnannouncedAsyncJob :execrows
148:DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0 AND state <> 'cancelled';
150:-- name: DeleteUnannouncedAsyncJobForClaim :execrows
151:-- Recovery's twin of DeleteUnannouncedAsyncJob (R3A-3): recoverers no longer
156:DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND claim_id = ? AND announced = 0;
158:-- name: TransitionAsyncJobTerminalPreserveVoid :one
163:-- rows affected and must re-read the row (GetAsyncJob) and accept whatever
167:-- this replaced -- TransitionAsyncJobTerminal -- is gone, not a second,
170:-- job_kill racing the history truncation).
173:-- cause passes 'pending' except step 6's job_kill, which passes 'done' once
177:-- anything while still running) except job_kill, which passes 1 in the SAME
178:-- statement -- so a job_kill row is neither debt nor a future notice the
184:-- the store's Go layer (AsyncJobStore.Transition) first reads the row's
188:UPDATE async_jobs
195:-- name: RependAsyncJobsByNoticeMessageIDs :execrows
209:UPDATE async_jobs SET delivery = 'pending', reacted = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
212:-- name: VoidAsyncJobsByAnnounceMessageIDs :many
218:-- existing 'void' (TransitionAsyncJobTerminalPreserveVoid), so writing void
224:UPDATE async_jobs SET delivery = 'void', updated_at = @updated_at
228:-- name: VoidAsyncJobsByToolCallIDs :many
233:UPDATE async_jobs SET delivery = 'void', updated_at = ?
237:-- name: ListPendingAsyncJobNoticesForOwner :many
240:SELECT * FROM async_jobs
244:-- name: PullPendingAsyncJobNotice :one
247:-- notice_message_id via SetAsyncJobNoticeMessageID, all in the SAME tx. 0
251:UPDATE async_jobs
256:-- name: SetAsyncJobNoticeMessageID :execrows
258:UPDATE async_jobs SET notice_message_id = ?, updated_at = ?
261:-- name: DeliverAsyncJobInline :execrows
265:-- (AnnounceInlineResult) -- exactly job_kill's DUR-11 shape. CAS on
272:UPDATE async_jobs
277:-- name: SetAsyncJobNoticeMessageIDForClaimIfDone :execrows
278:-- A3 (docs/reviews/2026-09-29-async-phase4-round1.md): job_kill's own
280:-- pull (doc sec.3.2) -- so nothing else ever calls SetAsyncJobNoticeMessageID
281:-- for that row. This fuses job_kill's own tool-result message id onto it in
284:-- carries its result" (Rerun's RependAsyncJobsByNoticeMessageIDs, matched by
285:-- notice_message_id, could otherwise never find a job_kill'd tool call in a
288:-- job_kill is still running, and a tool_call_id lookup would then name the
291:-- won, and no other writer re-pends or names a done job_kill row (Rerun
295:UPDATE async_jobs SET notice_message_id = @notice_message_id, updated_at = @updated_at
301:-- idx_async_jobs_debt and idx_session_notices_debt "(owner) WHERE wake=1
302:-- AND reacted=0 AND delivery<>'void'" on both tables. The async_jobs branch
307:-- (ListPendingAsyncJobNoticesForOwner/PullPendingAsyncJobNotice) already
310:-- then aborts, the row is deleted (DeleteUnannouncedAsyncJob) and the turn
315:        SELECT 1 FROM async_jobs
332:-- partial debt indexes (idx_async_jobs_debt/idx_session_notices_debt,
335:-- async_jobs is narrowed by the (owner_session_id, tool_call_id) primary key
340:        SELECT 1 FROM async_jobs
348:-- name: MarkAsyncJobsReactedForOwner :execrows
354:UPDATE async_jobs SET reacted = 1, updated_at = ?
357:-- name: GetAsyncJobByClaimID :one
363:SELECT * FROM async_jobs
366:-- name: IncrementAsyncJobWakeAttemptsForSnapshotRow :execrows
375:UPDATE async_jobs SET wake_attempts = wake_attempts + 1, updated_at = @updated_at
380:-- name: SettleAsyncJobReactedFailedForSnapshotRow :execrows
385:-- IncrementAsyncJobWakeAttemptsForSnapshotRow (R2A-4/R2A-5), so a late settle
389:-- (MarkAsyncJobsReactedForOwner): it is set ONLY here, never by a real
392:UPDATE async_jobs SET reacted = 1, reacted_failed = 1, updated_at = @updated_at
397:-- name: ListReactedFailedAsyncJobsForOwner :many
401:SELECT * FROM async_jobs WHERE owner_session_id = ? AND reacted_failed = 1 ORDER BY created_at ASC;
403:-- name: SetAsyncJobsWakeZeroPendingForOwners :execrows
407:UPDATE async_jobs SET wake = 0, updated_at = ?
410:-- name: ListRunningAsyncJobsForHost :many
414:SELECT * FROM async_jobs WHERE host_id = ? AND state = 'running' ORDER BY created_at ASC;
416:-- name: ListRunningAsyncJobsForOwners :many
423:SELECT * FROM async_jobs
427:-- name: ListAsyncJobsForOwner :many
429:SELECT * FROM async_jobs WHERE owner_session_id = ? ORDER BY created_at ASC;
431:-- name: PurgeAsyncJobsOlderThan :execrows
440:-- matching the system's own debt definition everywhere else (idx_async_jobs_
442:-- its wake/reacted bits (Rerun's VoidAsyncJobsByToolCallIDs voids a row
448:-- child session still has a running row or unreacted debt (async_jobs or
452:-- R5A-1: a job_kill row that is 'done' with notice_message_id NULL (wake=0,
457:DELETE FROM async_jobs
460:  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
463:            SELECT 1 FROM async_jobs c
464:            WHERE c.owner_session_id = async_jobs.child_session_id
469:            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
473:-- name: CountAsyncJobsOlderThan :one
474:-- Same predicate as PurgeAsyncJobsOlderThan, read-only, for
476:SELECT COUNT(*) FROM async_jobs
479:  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
482:            SELECT 1 FROM async_jobs c
483:            WHERE c.owner_session_id = async_jobs.child_session_id
488:            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
505:UPDATE async_jobs SET reacted_failed = 0, updated_at = ?
508:-- name: ArchiveAsyncJobToolCallID :execrows
526:-- delivery check: Rerun's VoidAsyncJobsByToolCallIDs can set delivery='void'
531:UPDATE async_jobs SET tool_call_id = @new_tool_call_id, updated_at = @updated_at
536:-- name: VoidUndeliveredAsyncJobsForOwner :execrows
544:UPDATE async_jobs SET delivery = 'void', reacted_failed = 0, updated_at = ?
547:-- name: AsyncJobDebtOwners :many
548:-- The async_jobs half of the batched reaction-debt read (architect decision
554:SELECT DISTINCT owner_session_id AS owner FROM async_jobs
```

### `internal/db/sql/messages.sql` (1 hits)

```
152:-- simply absent), which the caller reconciles async_jobs against.
```

### `internal/db/sql/session_drivers.sql` (1 hits)

```
25:-- Host exit (AsyncJobStore.Close) or dead-host purge: every marker naming
```

### `internal/db/sql/session_notices.sql` (13 hits)

```
2:-- session_notices carries notices with no async_jobs row (supervision,
23:-- PullPendingAsyncJobNotice: one transaction per notice, caller INSERTs the
50:-- Same reaction rule as MarkAsyncJobsReactedForOwner, for the notices table.
55:-- Notices half of IncrementAsyncJobWakeAttemptsForSnapshotRow (doc sec.3.4,
65:-- Notices half of SettleAsyncJobReactedFailedForSnapshotRow (doc sec.3.4,
74:-- Notices half of ListReactedFailedAsyncJobsForOwner: the parent-
80:-- SetAsyncJobsWakeZeroPendingForOwners.
90:-- RependAsyncJobsByNoticeMessageIDs (doc sec.3.8's "same for session_notices
94:-- mirror RependAsyncJobsByNoticeMessageIDs's own A4/A1 fixes -- see that
111:-- Same retention pass as PurgeAsyncJobsOlderThan (doc sec.3.7), including
113:-- exactly like the async_jobs twin -- see that query's doc.
128:-- R2A-4: session_notices.job_tool_call_id names its async_jobs row by
139:-- session_notices half of VoidUndeliveredAsyncJobsForOwner (`sessions reset`,
```

### `internal/log/goroutine_dump.go` (1 hits)

```
160:// filename prefix (e.g. the turn-stall detector's "stall-<id8>-", so
```

### `internal/message/message.go` (2 hits)

```
63:	// also needs to UPDATE another table (async_jobs/session_notices'
80:	// and marks async_jobs/session_notices rows reacted=1 in ONE
```

### `internal/server/descendant_work_batch_test.go` (3 hits)

```
53:	store := a.AsyncJobStore()
87:	for _, row := range []db.ClaimAsyncJobParams{
97:		_, err = q.ClaimAsyncJob(ctx, row)
```

### `internal/server/descendant_work_liveness_test.go` (8 hits)

```
5:// sub-agent session below it still has a LIVE async_jobs delegation row is
11:// the durable state (a live async_jobs delegation row, child_session_id)
13:// itself is session.AsyncJobStore.LiveDescendantJobs — the same helper
17:// The delegation row is claimed via the test App's own REAL AsyncJobStore
100:	// A REAL running async_jobs delegation row (owner=parent,
103:	store := a.AsyncJobStore()
104:	require.NotNil(t, store, "a full App built via appPkg.New must have an AsyncJobStore")
171:	store := a.AsyncJobStore()
```

### `internal/server/handlers_agent_rerun.go` (1 hits)

```
429:	go func(coord agent.Coordinator, voided []session.VoidedAsyncJob) {
```

### `internal/server/handlers_batch.go` (2 hits)

```
5:// completions, replacing the flat async_jobs rows and TEXT-parsed
10:// RUSH_BATCH_RUNTIME parses true (batchRuntimeEnabled). Flag off, the
```

### `internal/server/handlers_batch_doc.go` (4 hits)

```
7:// Old path (unchanged when RUSH_BATCH_RUNTIME is off):
9://     rows built from async_jobs (handlers_livework.go, liveWorkPusher).
14://     (web/src/asyncJobCompletion.ts, consumed via store.ts's
15://     $asyncJobCompletions) — fragile by construction.
```

### `internal/server/handlers_batch_test.go` (4 hits)

```
4:// delivered as session_batch_work, INERT unless RUSH_BATCH_RUNTIME is on.
186:	t.Setenv("RUSH_BATCH_RUNTIME", "")
232:	t.Setenv("RUSH_BATCH_RUNTIME", "1")
270:	t.Setenv("RUSH_BATCH_RUNTIME", "1")
```

### `internal/server/handlers_livework.go` (12 hits)

```
4:// web panel's Commands/Agents tabs. Data comes from the durable async_jobs
5:// table through session.AsyncJobStore's readers (JobsInTree, the read-pool
75:// buildSessionLiveWork reads every async_jobs row in the session's
84:	store := a.AsyncJobStore()
116:	titles := cache.titles(ctx, a, append(append([]db.AsyncJob{}, commands...), agents...))
118:	fill := func(rows []db.AsyncJob) []LiveWorkItemWire {
155:func capLiveWorkRows(rows []db.AsyncJob, kind string) []db.AsyncJob {
156:	sameKind := make([]db.AsyncJob, 0, len(rows))
168:	kept := make([]db.AsyncJob, 0, len(sameKind))
182:// liveWorkTitleKey identifies ONE async_jobs row's title. tool_call_id is
235:// message's tool_call part — async_jobs stores just its hash). Missing
238:func (c *titleCache) titles(ctx context.Context, a *appPkg.App, jobs []db.AsyncJob) map[liveWorkTitleKey]string {
```

### `internal/server/handlers_livework_awaiting_test.go` (1 hits)

```
31:	store := a.AsyncJobStore()
```

### `internal/server/handlers_livework_test.go` (16 hits)

```
5:// async_jobs table through session.AsyncJobStore's readers, so work hosted
139:	store := a.AsyncJobStore()
192:	store := a.AsyncJobStore()
204:	seed("tc-kill", session.TransitionParams{State: "cancelled", NoticeKind: "job_kill", Delivery: "done", Reacted: true})
215:	require.Equal(t, "cancelled|job_kill", statuses["tc-kill"])
232:	store := a.AsyncJobStore()
257:// claimed by a SEPARATE AsyncJobStore with its own host identity (its lock
275:	foreignStore := session.NewAsyncJobStore(other, dataDir, 0, "lw-test-other-process")
304:	store := a.AsyncJobStore()
392:	store := a.AsyncJobStore()
432:	store := a.AsyncJobStore()
452:	_, err = a.DB().ExecContext(ctx, "UPDATE async_jobs SET input_hash = 'reused-hash' WHERE tool_call_id = 'tc-reuse'")
500:	_, err = a.AsyncJobStore().Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "tc-cache-1", Kind: session.JobKindCommand, Input: "echo once", ToolName: "bash"})
531:	store := a.AsyncJobStore()
544:		_, err = a.DB().ExecContext(ctx, "UPDATE async_jobs SET updated_at = ? WHERE tool_call_id = ?", 1_700_000_000+int64(i), id)
609:	store := a.AsyncJobStore()
```

### `internal/server/p1_6_regression_test.go` (1 hits)

```
101:func (f *fakeAlwaysBusyCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/p2_regression_test.go` (1 hits)

```
87:func (f *fakeIsSessionBusyCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/p595_delete_streaming_test.go` (1 hits)

```
380:func (m *mockAgentCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/p614_rerun_reservation_test.go` (2 hits)

```
55:	stopRerunVoided []session.VoidedAsyncJob
142:func (m *mailboxLikeCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/p623_panic_window_test.go` (1 hits)

```
343:func (p *panicWindowCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/p630_rerun_cancel_window_test.go` (4 hits)

```
99:	store     *session.AsyncJobStore
137:	f.store = session.NewAsyncJobStore(a.DB(), t.TempDir(), os.Getpid(), "rerun-handler-test")
182:func (m *mailboxLikeCoordinator) counters() (cancelTurn, stop, stopRerun int, voided []session.VoidedAsyncJob, owned bool) {
185:	return m.cancelTurnCalls, m.stopCalls, m.stopRerunCalls, append([]session.VoidedAsyncJob(nil), m.stopRerunVoided...), m.owned
```

### `internal/server/protocol.go` (7 hits)

```
48:	// AsyncJobStore.LiveJobs/LiveWorkForRoots, exist); #1059 only wires this
59:	// RUSH_BATCH_RUNTIME is on; the old session_live_work wire is
109:	// when RUSH_BATCH_RUNTIME is on; flag off the route answers the normal
717:// handlers_livework.go builds the snapshot from the durable async_jobs table
718:// (session.AsyncJobStore.JobsInTree, the read-pool reader), pushes
731:// is 0 while the row is still running. Status is the durable async_jobs.state
733:// Reason the row's notice_kind ("job_kill" for a user-killed command,
```

### `internal/server/readpump_control_starvation_test.go` (1 hits)

```
112:func (f *blockingCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/rerun_hold_test.go` (1 hits)

```
151:func (m *blockingStopCoordinator) StopRerunJobs(ctx context.Context, sessionID string, voided []session.VoidedAsyncJob) {
```

### `internal/server/run_driver_liveness_test.go` (1 hits)

```
44:	store := a.AsyncJobStore()
```

### `internal/server/web_composer_history_test.go` (4 hits)

```
27:// web/src/asyncJobCompletion.ts). The frontend has no runnable unit-test
40:// asyncNoticePattern mirrors asyncJobCompletion.ts's asyncNoticePattern —
51:// fallback (asyncJobCompletion.ts's isAsyncCompletionNotice).
307:// work_ledger_timeout.go for where each is stamped) -- plus an unenumerated
```

### `internal/session/async_job_bgshell.go` (5 hits)

```
11:// JobKindBGShell is async_jobs.kind for a background shell that survives its
14:// id) and its ONE terminal transition (AsyncJobStore.Transition) commits the
50:func (s *AsyncJobStore) ClaimShell(ctx context.Context, owner, shellID string, originCLI bool) (ClaimResult, error) {
76:		if !errors.Is(err, ErrAsyncJobGone) {
97:func (s *AsyncJobStore) abandonShellClaim(owner, shellID, claimID string) {
```

### `internal/session/async_job_bgshell_announce_test.go` (3 hits)

```
16:const blockBGShellAnnounce = "UPDATE OF announced ON async_jobs WHEN NEW.kind = 'bg_shell'"
33:	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
41:// DeleteUnannouncedAsyncJobForClaim query's announced=0 predicate; this test
```

### `internal/session/async_job_bgshell_collision_test.go` (7 hits)

```
5:// while the async_jobs row is keyed (owner, tool_call_id) in the SHARED
20:func newTwoHostTestStore(t *testing.T) (a, b *AsyncJobStore, q *db.Queries, ctx context.Context) {
27:	a = NewAsyncJobStore(conn, dataDir, 999, "app-a")
28:	b = NewAsyncJobStore(conn, dataDir, 999, "app-b")
83:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
89:	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1700000001, OwnerSessionID: "owner-1", ToolCallID: "sh-dead-1"})
103:	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
```

### `internal/session/async_job_bgshell_test.go` (5 hits)

```
4:// born outside the async-wrapped branch claims a kind=bg_shell async_jobs row
38:	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
47:// REVERT CHECK: no mutant applies -- the sweep reads async_jobs without a
57:	seeded, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
64:	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1700000001, OwnerSessionID: "owner-1", ToolCallID: "sh-dead"})
```

### `internal/session/async_job_claim_id_test.go` (2 hits)

```
25:// Revert-check performed: temporarily reverted TransitionAsyncJobTerminalPreserveVoid's
29:// re-ran, passed. Diffed internal/db/sql/async_jobs.sql and
```

### `internal/session/async_job_dur2_atomicity_test.go` (2 hits)

```
4:// WON direction (TestAsyncJobStore_TransitionWonThenLostThenGone: a
43:	abortingTrigger(t, store, "fail_terminal_wake", "UPDATE OF wake ON async_jobs WHEN NEW.wake = 1", "induced wake write failure")
```

### `internal/session/async_job_gc.go` (11 hits)

```
6:// the SQL itself (PurgeAsyncJobsOlderThan/CountAsyncJobsOlderThan), not
9:// -- except an unnamed job_kill row (delivery='done', no result message),
19:// CountJobsOlderThan reports how many async_jobs/session_notices rows
22:func (s *AsyncJobStore) CountJobsOlderThan(ctx context.Context, age time.Duration) (jobs, notices int64, err error) {
24:	jobs, err = s.q.CountAsyncJobsOlderThan(ctx, cutoff)
26:		return 0, 0, fmt.Errorf("async job store: count jobs older than: async_jobs: %w", err)
35:// PurgeJobsOlderThan deletes terminal, delivered-or-voided async_jobs/
37:// --jobs-older-than`. Never touches unreacted debt or an unnamed job_kill row
39:func (s *AsyncJobStore) PurgeJobsOlderThan(ctx context.Context, age time.Duration) (jobs, notices int64, err error) {
41:	jobs, err = s.q.PurgeAsyncJobsOlderThan(ctx, cutoff)
43:		return 0, 0, fmt.Errorf("async job store: purge jobs older than: async_jobs: %w", err)
```

### `internal/session/async_job_gc_jobkill_test.go` (11 hits)

```
1:// R5A-1: retention must not delete a job_kill row whose result message was
13:// TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery: an old job_kill
16:// named job_kill row and an ordinary done row are still purged (control).
18:// Revert-check: drop `AND NOT (delivery = 'done' AND notice_kind = 'job_kill'
19:// AND notice_message_id IS NULL)` from PurgeAsyncJobsOlderThan -> the row is
31:			Owner: "owner-1", ToolCallID: id, State: "cancelled", NoticeKind: "job_kill",
44:		`UPDATE async_jobs SET notice_message_id = 'm-named' WHERE tool_call_id IN ('call-named', 'call-plain')`)
47:	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET updated_at = ?`, old)
52:	require.EqualValues(t, 2, counted, "the dry-run count keeps the unnamed job_kill row too")
59:	require.ErrorIs(t, err, sql.ErrNoRows, "a job_kill row that names its result message is ordinary history")
63:	require.NoError(t, err, "an unnamed job_kill row is what recovery repairs; retention must leave it")
```

### `internal/session/async_job_gc_test.go` (16 hits)

```
28:	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
33:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
41:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
46:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
103:func seedFinishedDelegation(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries, parent, child, toolCallID string) {
109:		`UPDATE async_jobs SET delivery = 'done', reacted = 1, notice_message_id = 'm', updated_at = 100 WHERE owner_session_id = ? AND tool_call_id = ?`,
117:// child still has a running row or unreacted debt (async_jobs or
122:// REVERT CHECK: the child-work predicate removed from PurgeAsyncJobsOlderThan
123:// and CountAsyncJobsOlderThan (sql/async_jobs.sql, regenerated) -- every
130:		child func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries)
133:		{"child has a running row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
136:		{"child has an unreacted done row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
140:			_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done', notice_message_id = 'm' WHERE owner_session_id = 'child-1'`)
143:		{"child has a pending unreacted row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
148:		{"child has a pending notice", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
151:		{"child is idle", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {}, false},
```

### `internal/session/async_job_kill_notice_test.go` (38 hits)

```
2:// the row names the message that carries its result." job_kill's own
3:// Transition call (causeJobKill, internal/agent/work_ledger_transition.go)
7:// notice_message_id at all, and a Rerun past job_kill's own tool-result
8:// message could never find it to re-pend (RependAsyncJobsByNoticeMessageIDs
22:// end to end: job_kill's Transition call commits delivery='done', its own
27:// Revert-check performed: called SetAsyncJobNoticeMessageID (unguarded, no
32:// production code's fix by removing the SetAsyncJobNoticeMessageIDForClaimIfDone
44:	// job_kill's own Transition call (causeJobKill, doc sec.3.2): delivery=
47:		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
55:	// A3: job_kill's own tool-result message fuses notice_message_id onto
59:		Parts: []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) was stopped (job_kill)."}},
69:	// Rerun truncates PAST job_kill's own tool-result message.
81:	require.Equal(t, "pending", repent.Delivery, "Rerun must be able to re-pend a job_kill'd row once its own notice message is deleted")
89:// it is forced to prove the guard: the tool-result message for job_kill's OWN
91:// notice_message_id is left alone (SetAsyncJobNoticeMessageIDForClaimIfDone's
113:	require.NoError(t, err, "the message must still be persisted even though this job_kill call did not win")
118:	require.Equal(t, "pending", row.Delivery, "the row belongs to the cause that actually won -- untouched by the losing job_kill call")
122:// TestRependJobKillRowWithoutNotice is R2A-8's live-process half: job_kill's
140:	kill := func(id string) db.AsyncJob {
146:			Owner: "owner-1", ToolCallID: id, ClaimID: claimed.Row.ClaimID, State: "cancelled", NoticeKind: "job_kill",
176:	require.False(t, pulled[0].Wake, "the repaired job_kill notice never wakes")
186:	require.False(t, repended, "a job_kill row that names its message must not be re-pended")
192:// killReusedKeyScenario is R3A-2's exact interleaving: job_kill's transition
198:	store    *AsyncJobStore
200:	killed   db.AsyncJob // J1 as job_kill's transition left it
202:	fresh    db.AsyncJob // J2
215:		Owner: "owner-1", ToolCallID: "call_0", ClaimID: first.Row.ClaimID, State: "cancelled", NoticeKind: "job_kill",
228:	rows, err := store.q.ListAsyncJobsForOwner(ctx, "owner-1")
239:func (s *killReusedKeyScenario) row(t *testing.T, toolCallID string) db.AsyncJob {
248:		Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "Async job call_0 (bash) was stopped (job_kill)."}},
254:// key -- names job_kill's result message and the new J2 is untouched. Then
256:// the job_kill result: re-pending would deliver it twice) and a Rerun past the
257:// job_kill message must be able to re-pend it.
259:// Revert-check: key SetAsyncJobNoticeMessageIDForClaimIfDone by tool_call_id
283:	_, err = s.store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET host_id = 'dead-host-r3a2' WHERE owner_session_id = 'owner-1'`)
288:	require.Zero(t, out["dead-host-r3a2"].Repended, "a job_kill row that names its message is not re-pended by recovery")
296:	// Rerun deletes job_kill's result message: J1 (named) is re-pended, J2 not.
305:	require.Equal(t, "pending", s.row(t, s.archived).Delivery, "Rerun must be able to re-pend the archived job_kill row")
```

### `internal/session/async_job_reaction.go` (44 hits)

```
6:// (async_jobs.sql/session_notices.sql) -- this file adds no new SQL, only
21:// tables (async_jobs requires announced=1 too -- see the query's own doc).
28:func (s *AsyncJobStore) ReactionDebtExists(ctx context.Context, owner string) (bool, error) {
40:func (s *AsyncJobStore) VisibleReactionDebtExists(ctx context.Context, owner string) (bool, error) {
48:// HasRunningDelegationFor reports whether a RUNNING async_jobs row currently
52:func (s *AsyncJobStore) HasRunningDelegationFor(ctx context.Context, childSessionID string) (bool, error) {
53:	_, err := s.q.GetRunningAsyncJobByChildSession(ctx, sql.NullString{String: childSessionID, Valid: true})
76:func (s *AsyncJobStore) MarkReactedWithMessageUpdate(ctx context.Context, messages message.Service, owner string, msg message.Message) error {
89:	if _, err := q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{
92:		return fmt.Errorf("async job store: mark reacted: async_jobs: %w", err)
108:		return fmt.Errorf("async job store: mark reacted: clear reacted_failed (async_jobs): %w", err)
124:// DebtJobRef names one async_jobs debt row exactly as the turn saw it
184:// or closed. async_jobs also requires announced=1, matching
187:func (s *AsyncJobStore) CaptureDebtSnapshot(ctx context.Context, owner string) (DebtSnapshot, error) {
188:	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
190:		return DebtSnapshot{}, fmt.Errorf("async job store: capture debt: async_jobs: %w", err)
225:func (s *AsyncJobStore) PendingInclusiveDebtSummary(ctx context.Context, owner string) (hasJobDebt bool, noticeKinds []string, err error) {
226:	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
228:		return false, nil, fmt.Errorf("async job store: pending-inclusive debt summary: async_jobs: %w", err)
254:func (s *AsyncJobStore) IncrementWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) error {
260:		if _, err := s.q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, db.IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
263:			return fmt.Errorf("async job store: increment wake attempts: async_jobs: %w", err)
283:func (s *AsyncJobStore) MaxWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) (int, error) {
286:		row, err := s.q.GetAsyncJobByClaimID(ctx, db.GetAsyncJobByClaimIDParams{
293:			return 0, fmt.Errorf("async job store: max wake attempts: async_jobs: %w", err)
331:func (s *AsyncJobStore) SettleReactedFailed(ctx context.Context, owner string, snap DebtSnapshot) error {
345:		rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, db.SettleAsyncJobReactedFailedForSnapshotRowParams{
349:			return 0, fmt.Errorf("async job store: settle reacted failed: async_jobs: %w", err)
376:func (s *AsyncJobStore) SettleReactedFailedWithMarker(ctx context.Context, owner string, snap DebtSnapshot, markerText string) (int64, error) {
415:// closed for owner (async_jobs.result_summary / session_notices.text),
417:func (s *AsyncJobStore) ListReactedFailedText(ctx context.Context, owner string) ([]ReactedFailedText, error) {
418:	jobs, err := s.q.ListReactedFailedAsyncJobsForOwner(ctx, owner)
420:		return nil, fmt.Errorf("async job store: list reacted-failed: async_jobs: %w", err)
443:func (s *AsyncJobStore) SetWakeZeroForOwners(ctx context.Context, owners []string) error {
448:	if _, err := s.q.SetAsyncJobsWakeZeroPendingForOwners(ctx, db.SetAsyncJobsWakeZeroPendingForOwnersParams{
451:		return fmt.Errorf("async job store: set wake zero: async_jobs: %w", err)
461:// RunningJobRow is a minimal snapshot of a running async_jobs row for scope
469:// ListRunningForOwners lists every currently-running async_jobs row for
472:func (s *AsyncJobStore) ListRunningForOwners(ctx context.Context, owners []string) ([]RunningJobRow, error) {
476:	rows, err := s.q.ListRunningAsyncJobsForOwners(ctx, owners)
495:func (s *AsyncJobStore) HostNotDead(hostID string) bool {
501:func (s *AsyncJobStore) hostNotDeadFor(ownHostID, hostID string) bool {
516:func (s *AsyncJobStore) ReactionDebtOwners(ctx context.Context, owners []string) (map[string]bool, error) {
518:	jobOwners, err := q.AsyncJobDebtOwners(ctx, owners)
```

### `internal/session/async_job_reaction_test.go` (11 hits)

```
40:// REVERT CHECK: commented out the `MarkAsyncJobsReactedForOwner` call inside
103:func abortingTrigger(t *testing.T, store *AsyncJobStore, name, on, msg string) {
112:func pulledDebt(t *testing.T, ctx context.Context, store *AsyncJobStore, messages message.Service, owner string) {
128:// async_jobs, then of session_notices -- is made to fail (a database trigger
144:		{"async_jobs marker fails", "fail_jobs_marker", "UPDATE OF reacted ON async_jobs"},
179:			require.EqualValues(t, 0, job.Reacted, "the async_jobs marker written before the failure must roll back too")
185:// twin: a supervision-style notice (no async_jobs row) also clears via the
223:// listed owners lose their wake bit -- on BOTH async_jobs and session_notices
629:	require.EqualValues(t, 0, job.Reacted, "the async_jobs settle must roll back with the failed marker insert")
715:		`UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake_attempts = 0, notice_message_id = NULL WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
737:	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET wake_attempts = 2 WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
```

### `internal/session/async_job_reader.go` (21 hits)

```
5:// 'running' async_jobs row whose host is not provably dead (sec.3.6's
26:// LiveJob names one concrete 'running' async_jobs row LiveJobs/
36:	// ToolName is the async_jobs.tool_name column (the tool that started
52:func (s *AsyncJobStore) HostLiveness(hostID string) HostLockStatus {
70:// ListRunningAsyncJobsForOwners exposes the query LiveJobs walks (doc sec.5
73:func (s *AsyncJobStore) ListRunningAsyncJobsForOwners(ctx context.Context, ownerIDs []string) ([]db.AsyncJob, error) {
74:	return s.readQuerier().ListRunningAsyncJobsForOwners(ctx, ownerIDs)
77:// ListAsyncJobsForOwner exposes the query `sessions jobs`/`sessions why`
78:// need: every async_jobs row (any state) owned by one session, oldest
80:func (s *AsyncJobStore) ListAsyncJobsForOwner(ctx context.Context, owner string) ([]db.AsyncJob, error) {
81:	return s.readQuerier().ListAsyncJobsForOwner(ctx, owner)
87:func (s *AsyncJobStore) GetAsyncHost(ctx context.Context, id string) (db.AsyncHost, error) {
91:// LiveJobs returns every 'running' async_jobs row live at or below
101:func (s *AsyncJobStore) LiveJobs(ctx context.Context, rootSessionID string) (live []LiveJob, walkIncomplete bool) {
116:		rows, err := s.readQuerier().ListRunningAsyncJobsForOwners(ctx, frontier)
182:func (s *AsyncJobStore) LiveDescendantJobs(ctx context.Context, rootSessionID string) (live []LiveJob, walkIncomplete bool) {
202:func (s *AsyncJobStore) LiveOwnJobs(ctx context.Context, sessionID string) (live []LiveJob, walkIncomplete bool) {
206:	rows, err := s.readQuerier().ListRunningAsyncJobsForOwners(ctx, []string{sessionID})
236:// JobsInTree lists every async_jobs row (ANY state, not just 'running') at
242:func (s *AsyncJobStore) JobsInTree(ctx context.Context, rootSessionID string) (jobs []db.AsyncJob, walkIncomplete bool) {
254:			rows, err := s.readQuerier().ListAsyncJobsForOwner(ctx, owner)
```

### `internal/session/async_job_reader_batch.go` (6 hits)

```
40:func (s *AsyncJobStore) LiveWorkForRoots(ctx context.Context, roots []string) map[string]RootLiveWork {
74:	running := make(map[string][]db.AsyncJob)
96:			rows, err := s.readQuerier().ListRunningAsyncJobsForOwners(ctx, chunk)
124:func liveWorkForRoot(root string, running map[string][]db.AsyncJob, failed map[string]struct{}, statusOf func(string) HostLockStatus) RootLiveWork {
144:		var level []db.AsyncJob
180:func liveJobOf(row db.AsyncJob, depth int, status HostLockStatus) LiveJob {
```

### `internal/session/async_job_reader_test.go` (9 hits)

```
24:// claimRunning is a small helper: claims a running async_jobs row for
26:// AsyncJobStore.Claim's own host registration -- so a test can attribute a
30:	params := db.ClaimAsyncJobParams{
43:	_, err := q.ClaimAsyncJob(ctx, params)
47:func TestAsyncJobStore_HostLiveness_OwnHostAliveWithoutProbing(t *testing.T) {
138:	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1, OwnerSessionID: "owner-1", ToolCallID: "call-1"})
295:	var nilStore *AsyncJobStore
387:		var nilStore *AsyncJobStore
403:// REVERT CHECK: JobsInTree changed back to s.q.ListAsyncJobsForOwner -- the
```

### `internal/session/async_job_recovery.go` (22 hits)

```
37:// async_jobs/session_notices row (and an empty dead host's lock file)
48:var recoverDeadHostRowSeam func(hostID string, row db.AsyncJob)
59:	Repended    int  // job_kill rows left done without a result message, made deliverable again
92:func (s *AsyncJobStore) RecoverDeadHost(ctx context.Context, hostID string, messages message.Service) (RecoveryOutcome, error) {
106:	rows, listErr := s.q.ListRunningAsyncJobsForHost(ctx, hostID)
168:	if n, err := s.q.DeleteTerminalUnannouncedAsyncJobsForHost(ctx, hostID); err != nil {
174:	// R2A-8 (DUR-11 for job_kill): job_kill commits done/reacted=1 first and
182:		slog.Warn("recover dead host: re-pend job_kill rows without a result message failed; will retry on a later sweep",
211:func (s *AsyncJobStore) removeDeadHostFile(hostID string) {
230:func recoveredResultText(ctx context.Context, messages message.Service, row db.AsyncJob) (text string, isError bool, readErr error) {
275:func (s *AsyncJobStore) SweepDeadHosts(ctx context.Context, messages message.Service) (map[string]RecoveryOutcome, error) {
304:func (s *AsyncJobStore) RecoverOwnerScope(ctx context.Context, owner string, messages message.Service) {
308:	rows, err := s.q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
331:// delivered-or-voided async_jobs/session_notices rows past age (never
332:// unreacted debt, nor a job_kill row still unnamed, which dead-host recovery re-pends, R5A-1),
338:func (s *AsyncJobStore) PurgeExpired(ctx context.Context, age time.Duration) error {
344:	if _, err := s.q.PurgeAsyncJobsOlderThan(ctx, cutoff); err != nil {
345:		errs = append(errs, fmt.Errorf("purge expired: async_jobs: %w", err))
363:// once it references zero async_jobs rows of ANY state (doc sec.3.7): a
376:func (s *AsyncJobStore) purgeEmptyDeadHostFiles(ctx context.Context) error {
415:func (s *AsyncJobStore) purgeOrphanHostLockFiles(ctx context.Context) error {
445:func (s *AsyncJobStore) reapOrphanHostLock(ctx context.Context, hostID string) {
```

### `internal/session/async_job_recovery_concurrent_test.go` (7 hits)

```
39:	recoverDeadHostRowSeam = func(host string, _ db.AsyncJob) {
107:func concurrentRecoverers(t *testing.T, ctx context.Context, stores []*AsyncJobStore, hostID string) []RecoveryOutcome {
115:	recoverDeadHostRowSeam = func(host string, row db.AsyncJob) {
190:		Owner: "owner-1", ToolCallID: "k1", State: "cancelled", NoticeKind: "job_kill",
199:	require.Equal(t, 1, total.Repended, "the unnamed job_kill row is re-pended once")
204:	require.Len(t, pulled, 6, "one notice per interrupted row plus the re-pended job_kill row, none twice")
242:// claim (DeleteUnannouncedAsyncJob) -> the fresh row is gone.
```

### `internal/session/async_job_recovery_test.go` (25 hits)

```
39:// seedRunningJob inserts a 'running' async_jobs row owned by hostID.
49:	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
56:		_, err := q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
131:// TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile pins
135:func TestAsyncJobStore_NeverClaims_RegistersNoHostAndCreatesNoLockFile(t *testing.T) {
310:// TestAsyncJobStore_CloseKeepLock_LeavesLockHeld is A12's fix: on a forced
314:func TestAsyncJobStore_CloseKeepLock_LeavesLockHeld(t *testing.T) {
367:// matrix's tail: once a dead host references zero async_jobs rows of any
427:	// A second, independent AsyncJobStore over its OWN *sql.DB connection to
433:	storeB := NewAsyncJobStore(connB, dataDir, 2, "recoverer-b")
486:// DB writes recovery makes are to async_jobs/async_hosts.
619:	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
624:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
631:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
636:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
644:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
655:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
660:	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
774:// REVERT CHECK: DeleteTerminalUnannouncedAsyncJobsForHost removed from
808:// half (DUR-11 for job_kill): the host died between causeJobKill's
812:// job_kill row that DOES name its message is untouched.
826:			Owner: "owner-1", ToolCallID: id, State: "cancelled", NoticeKind: "job_kill",
846:	require.EqualValues(t, 0, lost.Wake, "a re-pended job_kill row must never wake anyone")
849:	require.Equal(t, "done", named.Delivery, "a job_kill row that names its result message is untouched")
858:	require.Len(t, pulled, 1, "the lost job_kill result must be deliverable on the next pull")
```

### `internal/session/async_job_rerun.go` (13 hits)

```
4:// async_jobs/session_notices row against exactly the rows that transaction
33:// VoidedAsyncJob is one async_jobs row the truncation voided, as it stood
36:type VoidedAsyncJob struct {
47:	Voided  []VoidedAsyncJob
122:	var voided []VoidedAsyncJob
125:		if _, err := q.RependAsyncJobsByNoticeMessageIDs(ctx, db.RependAsyncJobsByNoticeMessageIDsParams{
128:			return RerunTruncation{}, fmt.Errorf("rerun truncate: repend async_jobs: %w", err)
143:		rows, err := q.VoidAsyncJobsByAnnounceMessageIDs(ctx, db.VoidAsyncJobsByAnnounceMessageIDsParams{
147:			return RerunTruncation{}, fmt.Errorf("rerun truncate: void async_jobs by announce message: %w", err)
150:			voided = append(voided, VoidedAsyncJob{ToolCallID: r.ToolCallID, State: r.State, ChildSessionID: r.ChildSessionID.String, HostID: r.HostID})
154:		rows, err := q.VoidAsyncJobsByToolCallIDs(ctx, db.VoidAsyncJobsByToolCallIDsParams{
158:			return RerunTruncation{}, fmt.Errorf("rerun truncate: void async_jobs by tool call: %w", err)
161:			voided = append(voided, VoidedAsyncJob{ToolCallID: r.ToolCallID, State: r.State, ChildSessionID: r.ChildSessionID.String, HostID: r.HostID})
```

### `internal/session/async_job_rerun_test.go` (8 hits)

```
26:	store    *AsyncJobStore
85:func (f *rerunFx) job(callID string) db.AsyncJob {
97:func voidedIDs(v []VoidedAsyncJob) []string {
261:		rows, err := f.store.ListAsyncJobsForOwner(f.ctx, rerunOwner)
352:// RependAsyncJobsByNoticeMessageIDs.
359:	_, err := f.q.VoidAsyncJobsByAnnounceMessageIDs(f.ctx, db.VoidAsyncJobsByAnnounceMessageIDsParams{
408:	won, err := f.store.Transition(f.ctx, TransitionParams{Owner: rerunOwner, ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill", Wake: false})
459:// Revert-check: drop the SetAsyncJobAnnounceMessageID call from AnnounceStarted.
```

### `internal/session/async_job_reset.go` (6 hits)

```
2:// deletes every message of a session and voids every async_jobs /
25:// ErrResetJobsRunning means the owner still has running async_jobs rows:
54:func (s *AsyncJobStore) ResetOwnerHistory(ctx context.Context, messages message.Service, owner string) (ResetOutcome, error) {
65:	running, err := q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
89:	if out.JobsVoided, err = q.VoidUndeliveredAsyncJobsForOwner(ctx, db.VoidUndeliveredAsyncJobsForOwnerParams{
92:		return ResetOutcome{}, fmt.Errorf("session reset: void async_jobs: %w", err)
```

### `internal/session/async_job_store.go` (60 hits)

```
1:// AsyncJobStore is the durable job store for the phase-4 durable async core
27:// JobKind is the async_jobs.kind vocabulary this step writes: 'bash'/
38:// HashJobInput is the sha256-hex of a tool call's raw input: async_jobs.
59:// ErrAsyncJobInputMismatch is returned when a (owner, tool_call_id) key is
65:type ErrAsyncJobInputMismatch struct {
69:func (e *ErrAsyncJobInputMismatch) Error() string {
73:// ErrAsyncJobGone is returned by MarkAnnounced when the target row no
77:var ErrAsyncJobGone = errors.New("async job store: row no longer exists")
104:	Row      db.AsyncJob
133:	// entirely (step 6's job_kill, doc sec.3.2).
136:	// cause except job_kill, which passes true so its row is neither debt
138:	// wake paragraph) -- wake=false for job_kill already excludes it from
157:	Row     db.AsyncJob // valid for Won/Lost; zero value for Gone
160:// AsyncJobStore is the durable job store: *sql.DB + *db.Queries + this
163:type AsyncJobStore struct {
192:	// ListAsyncJobsForOwner), wired once via SetReadConn -- see that
203:func (s *AsyncJobStore) SetMessages(messages message.Service) {
209:func (s *AsyncJobStore) messageService() message.Service {
218:// ListAsyncJobsForOwner run on it instead of the single writer connection
226:func (s *AsyncJobStore) SetReadConn(readDB *sql.DB) {
236:func (s *AsyncJobStore) readQuerier() *db.Queries {
243:// NewAsyncJobStore builds a store bound to sqlDB (the App's writer
247:func NewAsyncJobStore(sqlDB *sql.DB, dataDir string, pid int, label string) *AsyncJobStore {
248:	return &AsyncJobStore{sqlDB: sqlDB, q: db.New(sqlDB), dataDir: dataDir, pid: pid, label: label}
265:func (s *AsyncJobStore) ensureHost(ctx context.Context) (string, error) {
290:func (s *AsyncJobStore) HostID() string {
300:// a second executor). Different hash -> ErrAsyncJobInputMismatch. A
307:func (s *AsyncJobStore) Claim(ctx context.Context, p ClaimParams) (ClaimResult, error) {
345:func (s *AsyncJobStore) claimOnce(ctx context.Context, p ClaimParams, hostID, inputHash string) (ClaimResult, *db.AsyncJob, error) {
353:	existing, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID})
360:		// started earlier"/"different input" (see ArchiveAsyncJobToolCallID's
371:			rows, archErr := q.ArchiveAsyncJobToolCallID(ctx, db.ArchiveAsyncJobToolCallIDParams{
393:			return ClaimResult{}, nil, &ErrAsyncJobInputMismatch{ToolCallID: p.ToolCallID}
406:		conflict, err := q.GetRunningAsyncJobByChildSession(ctx, sql.NullString{String: p.ChildSessionID, Valid: true})
418:	params := db.ClaimAsyncJobParams{
445:	row, err := q.ClaimAsyncJob(ctx, params)
454:		// by owner=child (async_jobs.owner_session_id/session_notices.owner
460:			return ClaimResult{}, nil, fmt.Errorf("async job store: claim: clear stale reacted_failed (async_jobs): %w", err)
481:func (s *AsyncJobStore) Transition(ctx context.Context, p TransitionParams) (TransitionResult, error) {
515:		if current, getErr := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID}); getErr == nil {
519:	row, err := q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
544:	current, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: p.Owner, ToolCallID: p.ToolCallID})
563:// must still be marked announced). Returns ErrAsyncJobGone if the row no
566:func (s *AsyncJobStore) MarkAnnounced(ctx context.Context, owner, toolCallID string) error {
567:	rows, err := s.q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
574:		return ErrAsyncJobGone
583:func (s *AsyncJobStore) DeleteUnannounced(ctx context.Context, owner, toolCallID string) error {
584:	if _, err := s.q.DeleteUnannouncedAsyncJob(ctx, db.DeleteUnannouncedAsyncJobParams{
597:func (s *AsyncJobStore) deleteUnannouncedForClaim(ctx context.Context, owner, toolCallID, claimID string) (bool, error) {
598:	rows, err := s.q.DeleteUnannouncedAsyncJobForClaim(ctx, db.DeleteUnannouncedAsyncJobForClaimParams{
611:func (s *AsyncJobStore) Get(ctx context.Context, owner, toolCallID string) (db.AsyncJob, error) {
612:	return s.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: toolCallID})
618:func (s *AsyncJobStore) Close(ctx context.Context) error {
648:func (s *AsyncJobStore) CloseKeepLock() {
657:// its "own id" marking, WITHOUT deleting the async_hosts/async_jobs rows or
667:func (s *AsyncJobStore) SimulateCrashForTest() error {
680:func (s *AsyncJobStore) takeHost() *HostIdentity {
688:// notice merely still pending (R2A-6). Mirrors ArchiveAsyncJobToolCallID's
690:func isArchivableHistoryRow(row db.AsyncJob) bool {
698:// in an archived row's key (ArchiveAsyncJobToolCallID).
```

### `internal/session/async_job_store_lockorder_test.go` (1 hits)

```
23:func holdWriterAndStartRegistration(t *testing.T, store *AsyncJobStore) (commit func(), registered <-chan error) {
```

### `internal/session/async_job_store_test.go` (60 hits)

```
3:// AsyncJobStore coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
6:// boundary that the retry-with-backoff loop lives in the CALLER (workLedger,
22:// returns an AsyncJobStore plus a *db.Queries for seeding sessions rows
23:// (async_jobs.owner_session_id has an FK to sessions) via the existing
25:func newTestStore(t *testing.T) (*AsyncJobStore, *db.Queries, context.Context) {
32:	store := NewAsyncJobStore(conn, dataDir, 999, "test")
40:func TestAsyncJobStore_ClaimIsIdempotent(t *testing.T) {
58:func TestAsyncJobStore_ClaimDifferentInputIsRefused(t *testing.T) {
68:	var mismatch *ErrAsyncJobInputMismatch
73:// TestAsyncJobStore_ClaimReusedToolCallIDAfterDoneStartsFreshRow is B14/A14b's
83:// returned `*ErrAsyncJobInputMismatch` instead of a fresh running row, and
86:func TestAsyncJobStore_ClaimReusedToolCallIDAfterDoneStartsFreshRow(t *testing.T) {
102:	// old row still active this would be ErrAsyncJobInputMismatch; now it
113:	all, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
127:// TestAsyncJobStore_ClaimReusedToolCallIDStillRunningIsRefused proves the
133:func TestAsyncJobStore_ClaimReusedToolCallIDStillRunningIsRefused(t *testing.T) {
141:	// exact shape Rerun's VoidAsyncJobsByToolCallIDs produces for a stop that
143:	_, err = q.VoidAsyncJobsByToolCallIDs(ctx, db.VoidAsyncJobsByToolCallIDsParams{
154:	var mismatch *ErrAsyncJobInputMismatch
158:// TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted pins
163:func TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted(t *testing.T) {
176:	// ClaimAsyncJob's insert, so no row for it exists and no "started" is
192:	_, err = store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "parent-2", ToolCallID: "call-2"})
196:// TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted
206:func TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted(t *testing.T) {
240:// TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused pins
244:func TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused(t *testing.T) {
267:// TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal pins
271:// error -- the running-state case is TestAsyncJobStore_ClaimIsIdempotent.
272:func TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal(t *testing.T) {
295:func TestAsyncJobStore_TransitionWonThenLostThenGone(t *testing.T) {
315:		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
327:// TestAsyncJobStore_TransitionPreservesVoidDelivery pins doc sec.3.8: the
331:func TestAsyncJobStore_TransitionPreservesVoidDelivery(t *testing.T) {
340:	// delivery before the terminal transition lands (job_kill racing
342:	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = ? AND tool_call_id = ?`, "owner-1", "call-1")
345:	res, err := store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill"})
351:func TestAsyncJobStore_MarkAnnouncedAndDeleteUnannounced(t *testing.T) {
359:	row, err := store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner-1", ToolCallID: "call-1"})
365:	require.ErrorIs(t, store.MarkAnnounced(ctx, "owner-1", "no-such-call"), ErrAsyncJobGone)
370:	_, err = store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner-1", ToolCallID: "call-2"})
374:// TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry pins
376:// the CALLER (workLedger.transition, internal/agent), not inside the store.
380:func TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry(t *testing.T) {
398:	store := NewAsyncJobStore(storeConn, dataDir, 1, "test")
420:// TestAsyncJobStore_ClaimFailsClosedAgainstAClosedDB is DUR-8's genuinely-
428:// succeed, never hang -- so workLedger.Start's fail-closed propagation has
430:func TestAsyncJobStore_ClaimFailsClosedAgainstAClosedDB(t *testing.T) {
435:	// TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry
445:	store := NewAsyncJobStore(storeConn, dataDir, 1, "test")
459:// TestAsyncJobStore_SetReadConn_RoutesReadsToTheGivenConnection is A8/C9's
461:// ReactionDebtExists/ListAsyncJobsForOwner) must actually run on the wired
470:func TestAsyncJobStore_SetReadConn_RoutesReadsToTheGivenConnection(t *testing.T) {
477:	rows, err := store.ListAsyncJobsForOwner(ctx, "owner-1")
481:	// A DIFFERENT, empty on-disk DB with none of async_jobs' schema -- if
490:	_, err = store.ListAsyncJobsForOwner(ctx, "owner-1")
494:// TestAsyncJobStore_ClaimReusedToolCallIDAfterTerminalUndeliveredStartsFreshRow
503:// *ErrAsyncJobInputMismatch and the require.NoError below failed. Restored;
505:func TestAsyncJobStore_ClaimReusedToolCallIDAfterTerminalUndeliveredStartsFreshRow(t *testing.T) {
534:	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
```

### `internal/session/async_job_store_testhook.go` (1 hits)

```
10:func (s *AsyncJobStore) WrapReadQueriesForTest(wrap func(db.DBTX) db.DBTX) {
```

### `internal/session/batch_adapters.go` (1 hits)

```
155:func (s *AsyncJobStore) EnsureHost(ctx context.Context) (string, error) {
```

### `internal/session/batch_consumers_doc.go` (20 hits)

```
5:// production code; legacy async_jobs remains the sole live authority until
16://   grep -rn "AsyncJob\|async_jobs\|session_notices" --include="*.go" internal cmd \
19://   grep -rn "AsyncJob\|async_jobs\|session_notices\|AsyncDataRetention\|ResetSession\|sessions reset" ... (same filters)
26://   8 work_ledger_announce.go  7 sessions_audit_db.go  7 app_accessors.go
28://   6 work_ledger.go  5 session_driver.go  5 session.go  5 notice_row_ids.go
32://   3 work_ledger_inline.go  3 coordinator_drain_policy.go  3 coordinator.go
34://   2 message.go  2 app_run_async_wait.go  2 work_ledger_timeout.go
38://   work_ledger_transition.go work_ledger_pending.go work_ledger_driver.go
40://   job_output_pending.go supervision.go coordinator_wake.go
55:// | internal/session/async_job_rerun.go TruncateForRerun | One tx deletes tail messages, repends/voids async_jobs + session_notices, returns Voided set | BatchRerunReconcile (below): same message tx shape, node-row repend/void via BatchRerun{Repend,Void}NodesByNoticeMessageIDs / VoidRootsBy{AnnounceMessage,ToolCall}IDs | TestBatchRerunVoidDeletedHistory; TestBatchRerunRependDoneNotice |
56:// | internal/agent/coordinator_rerun.go StopRerunJobs + work_ledger_rerun.go stopToolCallsForRerun | Post-commit stop of voided running jobs, scoped to owner + tool_call ids | BatchRerunStopVoided (agent package, internal/agent/batch_* — other chunk): stop scoped to EXACT (owner, root_id) incarnation; late old-incarnation completion can never reach a new incarnation (I5) | TestBatchRerunIncarnationScopedStop (agent chunk); session-side TestBatchRerunVoidPreservesIncarnation |
57:// | internal/session/session_lifecycle.go service.Delete (55-155) | Deletes messages/files/session row; async_jobs/session_notices/batch rows go by FK ON DELETE CASCADE; NO refusal of live work (observed) | BatchOwnerDeleteHistory (below) mirrors old behavior: cascade, no refusal, recorded decision D-R10c-1 | TestBatchOwnerDeleteCascade; TestBatchOwnerDeleteRemovesControls |
61:// | internal/cmd/sessions_audit_db.go / sessions_audit_rules.go / sessions_audit_findings.go | raw SQL over async_jobs; missing old table disables the rule | batch audit queries BatchHistoryStateCounts (batch_history.sql) at R12 — needs production rewiring at R12 | TestBatchHistoryStateCounts |
62:// | internal/server/handlers_livework.go, handlers_sessions.go, protocol.go | flat livework snapshot from async_jobs | structured node snapshot [R15a, other package] reading BatchHistorySnapshot — needs production rewiring at R15a/R12 | R15a server tests |
64:// | internal/agent/work_ledger*.go, bgshell_claim.go, notice_pull.go, async_job_reaction.go, notice_row_ids.go | claim/terminal CAS, ack, inline, reaction debt | [R8 batch_outbox_doc.go mapping: BatchPullNotices, BatchAnnounceStarted, BatchAnnounceInlineResult, BatchReaction*] — done, inert | existing batch_pull/reaction tests |
68:// | async_jobs writer core (async_job_store.go, async_job_bgshell.go) | claim/archive/terminal CAS | dropped at R12: replaced by Reserve/Project [R5, batch_store.go]; no compatibility wrapper | existing batch_store tests |
69:// | work_ledger_announce.go / work_ledger_inline.go / work_ledger_pending.go | ack, durable fallback, controller routing | dropped at R12: replaced by BatchAnnounce* [R8] and durable control requests [R9] | existing R8/R9 tests |
70:// | tools/job_output.go, tools/job_kill.go | controller tools | dropped at R12 (D6): replaced by batch_status/output/stop [R13] | R13 tests |
71:// | supervision.go / delegation_question.go / work_ledger_timeout.go question+wake-only notices | supervision notices, held input | retained session_notices infrastructure until cutover; node question delivery is R8 reaction + R7c adapter scope — needs production rewiring at R12/R7c | R7c/R8 tests |
83:// deletes messages/files/session row and lets FKs cascade async_jobs and
```

### `internal/session/batch_migration_test.go` (4 hits)

```
41:	before, err := db.New(conn).ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy", Kind: "command", InputHash: "hash", HostID: "legacy-host", ClaimID: "claim", CreatedAt: 1, UpdatedAt: 1})
49:	after, err := db.New(conn).GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy"})
64:	for _, table := range []string{"async_jobs", "async_hosts", "session_notices", "sessions"} {
69:	after, err = db.New(conn).GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy"})
```

### `internal/session/batch_outbox_doc.go` (1 hits)

```
75:production activation are later packages. async_jobs and all old callers remain
```

### `internal/session/batch_reaction_test.go` (1 hits)

```
135:	// resolve it via async_jobs, and must not rewrite binding/delivery semantics.
```

### `internal/session/batch_read_doc.go` (2 hits)

```
19:| ListRunningAsyncJobsForOwners | BatchLiveNodesForOwners: all nodes of live roots, BFS chunks, Pending/Starting/Cancelling included. |
24:| ListAsyncJobsForOwner / JobsInTree | Historical session consumers deferred to R10c; BatchLevel reads one incarnation, not session history. |
```

### `internal/session/batch_recovery_doc.go` (1 hits)

```
2:// legacy async_jobs remains the sole live authority until the later cutover.
```

### `internal/session/host_lock.go` (5 hits)

```
10:// deletion paths); AsyncJobStore.ensureHost wires it lazily into the first
12:// bookkeeping throughout -- no FK from async_jobs, no timestamp used for
94:// forced-shutdown path (AsyncJobStore.CloseKeepLock). An *os.File is closed by
337:// AsyncJobStore.ensureHost (memoized per store); this function is the
388:// async_jobs rows, its async_hosts row and lock file are both removed;
```

### `internal/session/host_lock_register_race_test.go` (1 hits)

```
90:	reaper := NewAsyncJobStore(store.sqlDB, dataDir, 998, "reaper")
```

### `internal/session/host_lock_test.go` (3 hits)

```
17:// touch async_hosts/async_jobs (doc sec.3.6 requires real-DB coverage, not
241:	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
411:// seedSession inserts a minimal sessions row so async_jobs.owner_session_id
```

### `internal/session/notice_pull.go` (54 hits)

```
14:// never the raw generated db.AsyncJob/db.SessionNotice: package agent does
31:// inserter (supervision.go, work_ledger_timeout.go, coordinator_background.go
39:	// "timeout_wake_only" value directly (work_ledger_timeout.go already
69:// JobNoticeRow is what a pulled async_jobs row hands its caller's build
100:	// ClaimID is the async_jobs row's claim_id (empty for session_notices
105:// PullJobNotices pulls every pending, announced async_jobs notice row for
109:func (s *AsyncJobStore) PullJobNotices(ctx context.Context, messages message.Service, owner string, build func(JobNoticeRow) message.CreateMessageParams) ([]PulledNotice, error) {
110:	rows, err := s.q.ListPendingAsyncJobNoticesForOwner(ctx, owner)
129:func (s *AsyncJobStore) pullOneJobNotice(ctx context.Context, messages message.Service, owner string, row db.AsyncJob, build func(JobNoticeRow) message.CreateMessageParams) (PulledNotice, bool, error) {
137:	pulledRow, err := q.PullPendingAsyncJobNotice(ctx, db.PullPendingAsyncJobNoticeParams{
157:	if _, err := q.SetAsyncJobNoticeMessageID(ctx, db.SetAsyncJobNoticeMessageIDParams{
177:func (s *AsyncJobStore) PullSessionNotices(ctx context.Context, messages message.Service, owner string, build func(SessionNoticeRow) message.CreateMessageParams) ([]PulledNotice, error) {
200:func (s *AsyncJobStore) pullOneSessionNotice(ctx context.Context, messages message.Service, owner, ownHostID string, row db.SessionNotice, build func(SessionNoticeRow) message.CreateMessageParams) (PulledNotice, bool, error) {
263:func (s *AsyncJobStore) sessionNoticeVoidCondition(ctx context.Context, q *db.Queries, owner, ownHostID string, row db.SessionNotice) (bool, error) {
269:		job, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: row.JobToolCallID.String})
284:		running, err := q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
309:// when non-empty, names the async_jobs row sessionNoticeVoidCondition checks
312:func (s *AsyncJobStore) InsertSessionNotice(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) error {
333:// already-pending notice becomes pullable. rows==0 (ErrAsyncJobGone) means
337:// (workLedger.acknowledgeWithMessageTx) is responsible for the in-memory
340:func (s *AsyncJobStore) AnnounceStarted(ctx context.Context, messages message.Service, owner, toolCallID string, params message.CreateMessageParams) (message.Message, error) {
352:	rows, err := q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
359:		return message.Message{}, ErrAsyncJobGone
363:	if _, err := q.SetAsyncJobAnnounceMessageID(ctx, db.SetAsyncJobAnnounceMessageIDParams{
379:// AnnounceJobKillResult is job_kill's own fused transaction (A3, doc
381:// its result"): job_kill's Transition call (causeJobKill) already set
388:// killedClaimID is the claim_id of the TARGET async_jobs row (the job that was
389:// killed; the claim job_kill's own transition won) -- never its tool_call_id:
390:// a new claim under a reused id archives the killed row while job_kill is
392:// params' own tool_call_id is job_kill's own call. The guarded write
393:// (SetAsyncJobNoticeMessageIDForClaimIfDone) is defensive: killedClaimID is
394:// passed only after job_kill's own Transition won, and no other writer
395:// re-pends or names a done job_kill row (Rerun re-pends by notice_message_id,
397:// deletion (the owner session's cascade); it is not an error -- job_kill's
400:func (s *AsyncJobStore) AnnounceJobKillResult(ctx context.Context, messages message.Service, owner, killedClaimID string, params message.CreateMessageParams) (message.Message, error) {
402:		return message.Message{}, errors.New("async job store: announce job_kill result: empty claim id")
406:		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: begin: %w", err)
413:		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: create message: %w", err)
415:	if _, err := q.SetAsyncJobNoticeMessageIDForClaimIfDone(ctx, db.SetAsyncJobNoticeMessageIDForClaimIfDoneParams{
419:		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: set notice message id: %w", err)
422:		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: commit: %w", err)
433:	// DeliveryRows is DeliverAsyncJobInline's rows-affected. 1 = the row's
448:// Shape is job_kill's DUR-11 fusion: insert message -> DeliverAsyncJobInline
451:// ErrAsyncJobGone (MarkAsyncJobAnnounced's 0 rows, same key as
459:func (s *AsyncJobStore) AnnounceInlineResult(ctx context.Context, messages message.Service, owner, toolCallID, claimID string, params message.CreateMessageParams) (InlineAnnouncement, error) {
471:	rows, err := q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
478:		return InlineAnnouncement{}, ErrAsyncJobGone
482:	if _, err := q.SetAsyncJobAnnounceMessageID(ctx, db.SetAsyncJobAnnounceMessageIDParams{
488:	deliveryRows, err := q.DeliverAsyncJobInline(ctx, db.DeliverAsyncJobInlineParams{
507:func (s *AsyncJobStore) ListSessionNotices(ctx context.Context, owner string) ([]SessionNoticeRow, error) {
519:// RependJobKillRowWithoutNotice is R2A-8's live-process repair for job_kill
532:// job_kill row awaiting its message).
533:func (s *AsyncJobStore) RependJobKillRowWithoutNotice(ctx context.Context, owner, claimID string) (bool, error) {
541:		return false, fmt.Errorf("async job store: re-pend job_kill row without notice: %w", err)
```

### `internal/session/notice_pull_test.go` (20 hits)

```
43:// on-disk database file, each wrapped in its own AsyncJobStore -- the
48:func twoConnStores(t *testing.T, sessionIDs ...string) (a, b *AsyncJobStore, ctx context.Context) {
69:	a = NewAsyncJobStore(connA, dataDir, 1, "process-a")
70:	b = NewAsyncJobStore(connB, dataDir, 2, "process-b")
86:// processes/hosts) racing to pull the SAME pending async_jobs row must
102:// PullPendingAsyncJobNotice's SQL (internal/db/sql/async_jobs.sql) and ran
199:// async_jobs row (job_tool_call_id) and is void at pull time iff that job is
253:// scope still has a running async_jobs row; otherwise it voids at pull time.
262:	// No async_jobs row at all for owner-1: the scope is closed.
348:// Delivery parameter's 'done' path (doc sec.3.2, used by step 6's job_kill):
350:// message. In step 3 every cause, job_kill included, still passes 'pending'.
360:		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
375:// ACTUAL, CURRENT job_kill contract (doc sec.3.2): job_kill's own tool
379:// test pinned a DIFFERENT, since-superseded step-3 contract (job_kill keeping
381:// pull) that production's causeStateNoticeKindWake (work_ledger_transition.go)
400:		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
405:	require.Equal(t, "done", res.Row.Delivery, "job_kill's Transition call commits delivery='done' directly, not 'pending'")
412:	require.Empty(t, pulled, "a job_kill'd row (delivery='done' already) must never also surface as a pulled notice")
413:	require.EqualValues(t, 0, countMessages(t, store.q, ctx, "owner-1"), "job_kill's own tool response is the answer -- the pull inserts no second message for it")
433:	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call-1'`)
```

### `internal/session/notice_row_ids.go` (4 hits)

```
26:func (s *AsyncJobStore) InsertSessionNoticeReturningID(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) (int64, error) {
50:func (s *AsyncJobStore) PendingInclusiveDebtRows(ctx context.Context, owner string) (hasJobDebt bool, notices []PendingNoticeDebt, jobClaims []string, err error) {
51:	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
53:		return false, nil, nil, fmt.Errorf("async job store: pending-inclusive debt rows: async_jobs: %w", err)
```

### `internal/session/notice_row_ids_test.go` (2 hits)

```
96:		{name: "reacted_job_is_settled", announce: true, wake: true, setSQL: `UPDATE async_jobs SET reacted=1, delivery='done'`},
97:		{name: "void_job_is_not_debt", announce: true, wake: true, setSQL: `UPDATE async_jobs SET delivery='void'`},
```

### `internal/session/session.go` (3 hits)

```
125:	// (a sub-agent this session delegated to) is named by a live async_jobs
129:	// the session-activity classifier (AsyncJobStore.LiveDescendantJobs).
145:	// "running" promotion (AsyncJobStore.LiveOwnJobs, App.LiveSessionDrivers).
```

### `internal/session/session_activity.go` (1 hits)

```
135:	// DeadHostRunningJobs: the session owns running async_jobs rows on a
```

### `internal/session/session_child_question.go` (2 hits)

```
25:// that asked, the held delegation X the notice is bound to (its async_jobs
84:func (s *AsyncJobStore) PendingChildQuestions(ctx context.Context, owner string) ([]ChildQuestion, error) {
```

### `internal/session/session_child_question_test.go` (1 hits)

```
60:	store := NewAsyncJobStore(conn, dataDir, 1, "test")
```

### `internal/session/session_child_question_void_test.go` (1 hits)

```
31:	store := NewAsyncJobStore(conn, dataDir, 1, "test")
```

### `internal/session/session_driver.go` (5 hits)

```
74:func (s *AsyncJobStore) hostLivenessDetail(hostID string) (HostLockStatus, error) {
95:func (s *AsyncJobStore) ClaimSessionDriver(ctx context.Context, sessionID string) error {
149:func (s *AsyncJobStore) ReleaseSessionDriver(ctx context.Context, sessionID string) error {
164:func (s *AsyncJobStore) ForeignLiveDriver(ctx context.Context, sessionID string) (SessionDriver, bool, error) {
186:func (s *AsyncJobStore) purgeDeadSessionDrivers(ctx context.Context) error {
```

### `internal/session/session_driver_test.go` (6 hits)

```
23:// wrapped in its own AsyncJobStore (its own host identity once it claims),
25:func nStores(t *testing.T, n int, sessionIDs ...string) (stores []*AsyncJobStore, q *db.Queries, ctx context.Context) {
42:		st := NewAsyncJobStore(conn, dataDir, 1000+i, "store")
49:func driverRow(t *testing.T, ctx context.Context, st *AsyncJobStore, sessionID string) (db.SessionDriver, bool) {
166:		go func(st *AsyncJobStore) {
243:	check := func(st *AsyncJobStore, sessionID string, wantForeign bool, why string) {
```

### `internal/session/session_lifecycle.go` (1 hits)

```
56:// toolCallID (deterministic: async_jobs.child_session_id is claimed with the
```

### `internal/shell/background.go` (3 hits)

```
61:	// queries (job_output on a long-finished job) still resolve, just
78:	// (job_output/bash still only ever surface the last MaxOutputLength=30000
454:		return nil, fmt.Errorf("maximum number of background jobs (%d) reached. Nothing is queued: all %d are running now. Each running job reports its result as a session message and wakes you when it finishes. Free slots: job_kill the jobs you no longer need, timers first; otherwise end your turn now with a short status and NO tool call. Do not poll with sleep/echo and do not call ask_question about this limit", limit, active)
```

### `internal/shell/background_buffer_test.go` (2 hits)

```
141:// continuously while job_output/bash poll concurrently). Intended to run
165:	// Readers: simulate job_output polling via GetOutput and WaitForChange.
```

### `internal/shell/background_cap_test.go` (4 hits)

```
2:// work_ledger_cap.go's asyncCapError): hitting MaxBackgroundJobs must name
3:// the next step -- free slots with job_kill, or end the turn now -- and must
16:// job_kill escape hatch, the "end your turn" instruction, and no "wait for"
36:	require.Contains(t, err.Error(), "job_kill")
```

### `internal/shell/background_release_test.go` (1 hits)

```
177:	// WaitForChange baseline path used by job_output.
```

### `internal/shell/background_shell.go` (1 hits)

```
273:// arrived" (job_output's wait:true path) MUST use this — not
```

### `internal/shell/exec_unix.go` (1 hits)

```
115:// (turn cancellation, the stream watchdog, `job_kill` for a backgrounded
```

### `internal/shell/exec_windows_test.go` (1 hits)

```
85:// job_kill call against a proxy process that had forked a worker blocked
```

### `web/src/asyncJobCompletion.ts` (13 hits)

```
6:export type AsyncJobStatus = "finished" | "failed" | "timed_out" | "stopped" | "cancelled";
8:export interface AsyncJobCompletion {
9:  status: AsyncJobStatus;
13:export interface AsyncJobCompletionIndex {
14:  byToolCallID: Map<string, AsyncJobCompletion>;
37:  /^Async job (\S+) \([^)]*\) (timed out after \d+s and was stopped|was stopped \(job_kill\)|was cancelled \(session stopped\))\. Partial output( before the stop)?:\n\n([\s\S]*)$/;
39:function stoppedStatus(cause: string): AsyncJobStatus {
41:  if (cause.includes("job_kill")) return "stopped";
52:export function indexAsyncJobCompletions(messages: Message[]): AsyncJobCompletionIndex {
53:  const byToolCallID = new Map<string, AsyncJobCompletion>();
74:        // subAgentOutcomeCancelledText (internal/agent/work_ledger_delegation.go)
79:        const status: AsyncJobStatus =
82:            : (asyncMatch[2] as AsyncJobStatus);
```

### `web/src/asyncJobMetadata.test.ts` (2 hits)

```
1:// Unit tests for isPendingJob (web/src/asyncJobMetadata.ts), run with
6:import { isPendingJob } from "./asyncJobMetadata.ts";
```

### `web/src/batchWork.ts` (1 hits)

```
12:// with RUSH_BATCH_RUNTIME on -- flag off, the event never arrives and this
```

### `web/src/components/Chat.tsx` (2 hits)

```
10:  $asyncJobCompletions,
349:  const jobCompletions = useStore($asyncJobCompletions);
```

### `web/src/components/LiveWorkList.tsx` (3 hits)

```
2:// of async-work items sourced from the server's durable async_jobs snapshot,
38:// statusLabel maps a durable async_jobs.state (+ notice_kind reason) to the
39:// short label the row shows. Cancelled splits by reason: "job_kill" is a
```

### `web/src/components/Message/ActionRow.tsx` (4 hits)

```
7:import type { AsyncJobCompletion } from "../../asyncJobCompletion";
18:import { isPendingJob } from "../../asyncJobMetadata";
50:  jobCompletions: Map<string, AsyncJobCompletion>;
184:  // (job_kill) / cancelled (session stop) must be distinguishable from a
```

### `web/src/components/Message/Message.tsx` (1 hits)

```
19:import { isAsyncCompletionNotice } from "../../asyncJobCompletion";
```

### `web/src/components/Message/ToolActivityGroup.tsx` (3 hits)

```
14:import { $asyncJobCompletions } from "../../store";
49:  const jobCompletions = useStore($asyncJobCompletions);
187:    // job_output polling). Keep the LAST occurrence (freshest result) and
```

### `web/src/components/SubAgentBlock.tsx` (2 hits)

```
8:import { $subAgentMessages, $messages, $asyncJobStatuses, $activeSessionID, registerSubAgentSession } from "../store";
89:  const asyncStatuses = useStore($asyncJobStatuses);
```

### `web/src/store.ts` (4 hits)

```
3:import { indexAsyncJobCompletions, isAsyncCompletionNotice, type AsyncJobStatus } from "./asyncJobCompletion";
20:export const $asyncJobCompletions = computed($messages, indexAsyncJobCompletions);
21:export const $asyncJobStatuses = computed($asyncJobCompletions, (index): Map<string, AsyncJobStatus> => {
22:  const statuses = new Map<string, AsyncJobStatus>();
```

### `web/src/types.ts` (5 hits)

```
50:  // True when the session has a running delegation row (async_jobs, a
276:  // Durable async_jobs.state: "running" | "completed" | "failed" |
280:  // The row's notice_kind for a terminal state ("job_kill" = user killed
285:  // Last async_jobs row update, unix ms.
302:// ─── Batch work (R15a, inert unless RUSH_BATCH_RUNTIME is on) ───────────────
```

### `web/src/useWS.ts` (1 hits)

```
469:      // when the server runs with RUSH_BATCH_RUNTIME off -- the event
```

### `web/tests/async-outcome-badges.spec.ts` (2 hits)

```
11: * is subAgentOutcomeCancelledText (work_ledger_delegation.go) — mapped to
48:    { id: "c-stop", notice: "Async job c-stop (bash) was stopped (job_kill). Partial output before the stop:\n\npartial", badge: "stopped by user" },
```

### `web/tests/composer-history-filter.spec.ts` (2 hits)

```
94:  // One-time timeout check-in (work_ledger_timeout.go's timeoutWakeOnly):
107:    Parts: [{ type: "text", Text: "Async job call-4 (bash) was stopped (job_kill). Partial output before the stop:\n\npartial" }],
```

### `web/tests/live-work-panel.spec.ts` (1 hits)

```
204:    { toolCallID: "tc-s6", toolName: "bash", title: "killed", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "cancelled", reason: "job_kill" },
```

### `web/tests/wake-schedules.spec.ts` (1 hits)

```
205:      { type: "text", Text: "Async job call-stop (run_command) was stopped (job_kill). Partial output before the stop:\n\nline one" },
```

## Files requiring human disposition (`unknown`)

- `.github/workflows/publish-fork-npm.yml`
- `docs/checkpoints/2026-06-25-2218.md`
- `docs/checkpoints/2026-06-26-0831.md`
- `docs/checkpoints/2026-07-19-0729.md`
- `docs/checkpoints/2026-07-30-1400.md`
- `docs/checkpoints/2026-08-03-1031.md`
- `docs/checkpoints/2026-09-28-0029.md`
- `docs/checkpoints/2026-09-28-1711.md`
- `docs/checkpoints/2026-09-29-0925.md`
- `docs/checkpoints/2026-09-30-0810.md`
- `docs/checkpoints/2026-09-30-2008.md`
- `docs/checkpoints/2026-10-01-1351.md`
- `docs/checkpoints/2026-10-01-2114.md`
- `docs/checkpoints/2026-10-02-0510.md`
- `docs/checkpoints/2026-10-05-0755.md`
- `docs/checkpoints/2026-10-05-1331.md`
- `docs/checkpoints/2026-10-05-2020.md`
- `docs/checkpoints/2026-10-06-0530.md`
- `docs/checkpoints/2026-10-06-1834.md`
- `docs/checkpoints/2026-10-07-0554.md`
- `docs/checkpoints/2026-10-07-2137.md`
- `docs/checkpoints/2026-10-08-1449.md`
- `docs/checkpoints/2026-10-11-2215.md`
- `docs/checkpoints/2026-10-12-0734.md`
- `docs/ci/2026-10-13-selector-replay.md`
- `docs/design/event-loop-turn.md`
- `docs/reviews/2026-07-30-full-project-review.md`
- `docs/reviews/2026-09-04-sdk-library-review-round-16-1209.md`
- `docs/reviews/2026-09-05-2047-commit-review-36h.md`
- `docs/reviews/2026-09-29-async-phase4-round1-rerun-design.md`
- `docs/reviews/2026-09-29-async-phase4-round1.md`
- `docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md`
- `docs/reviews/2026-09-30-async-phase4-round2.md`
- `docs/reviews/2026-09-30-async-phase4-round3.md`
- `docs/reviews/2026-09-30-async-phase4-round4.md`
- `docs/reviews/2026-09-30-async-phase4-round5.md`
- `docs/reviews/2026-09-30-async-phase4-round6.md`
- `docs/reviews/2026-09-30-async-phase4-round7.md`
- `docs/reviews/2026-09-30-async-phase4-round8.md`
- `docs/reviews/2026-10-05-async-round9.md`
- `docs/reviews/2026-10-08-job-batches-xxs-rush-integration-round2.md`
- `docs/reviews/2026-10-08-job-batches-xxs-rush-integration.md`
- `internal/agent/agent_drain_decision.go`
- `internal/agent/agent_timeouts_test.go`
- `internal/agent/batch_exec_agent_doc.go`
- `internal/agent/batch_exec_generic_test.go`
- `internal/agent/batch_install_inventory_test.go`
- `internal/agent/batch_install_tools.go`
- `internal/agent/batch_install_tools_test.go`
- `internal/agent/batch_model_flag_test.go`
- `internal/agent/bgshell_claim_test.go`
- `internal/agent/bgshell_observer_recheck_test.go`
- `internal/agent/cli_active_work.go`
- `internal/agent/cliscope_completion_hold_test.go`
- `internal/agent/coordinator.go`
- `internal/agent/default_job_timeout_test.go`
- `internal/agent/delegation_question_test.go`
- `internal/agent/drain_attempt_fixture_test.go`
- `internal/agent/drain_compaction_continuation_test.go`
- `internal/agent/drain_compaction_submit_test.go`
- `internal/agent/drain_debt_edge_cases_test.go`
- `internal/agent/drain_gate_streaks_test.go`
- `internal/agent/drain_launch_gate_test.go`
- `internal/agent/drain_notice_pull_test.go`
- `internal/agent/loop_detection_test.go`
- `internal/agent/policy_leaf.go`
- `internal/agent/policy_leaf_baseline_test.go`
- `internal/agent/prompt/prompt_scoped_test.go`
- `internal/agent/question_stop_format_test.go`
- `internal/agent/release_callback_join_test.go`
- `internal/agent/restricted_tool.go`
- `internal/agent/session_idle_trigger_test.go`
- `internal/agent/step_progress.go`
- `internal/agent/step_progress_test.go`
- `internal/agent/stream_watchdog.go`
- `internal/agent/stream_watchdog_toolpause_test.go`
- `internal/agent/supervision_silence.go`
- `internal/agent/supervision_silence_test.go`
- `internal/agent/supervision_test.go`
- `internal/agent/tool_suggest_test.go`
- `internal/agent/tools/agent_control_test.go`
- `internal/agent/tools/run_command.go`
- `internal/agent/tools/run_command_output_buffer.go`
- `internal/agent/tools/run_command_process_kill_test.go`
- `internal/agent/tools/run_command_process_unix.go`
- `internal/agent/tools/run_command_process_windows.go`
- `internal/agent/turn_arbiter_shadow_test.go`
- `internal/agent/turn_arbiter_state.go`
- `internal/agent/turn_arbiter_testhelpers_test.go`
- `internal/agent/turn_progress_fixture_test.go`
- `internal/agent/turn_progress_guard.go`
- `internal/agent/turn_stall_clock.go`
- `internal/cmd/batch_cli_runtime_test.go`
- `internal/cmd/batch_cli_test.go`
- `internal/cmd/claude_wrush_command.md`
- `internal/cmd/run.go`
- `internal/config/config_mcp.go`
- `internal/config/store_oauth_matrix_test.go`
- `internal/db/db.go`
- `internal/db/messages.sql.go`
- `internal/db/session_drivers.sql.go`
- `internal/db/session_notices.sql.go`
- `internal/db/sql/messages.sql`
- `internal/db/sql/session_drivers.sql`
- `internal/db/sql/session_notices.sql`
- `internal/log/goroutine_dump.go`
- `internal/message/message.go`
- `internal/server/descendant_work_batch_test.go`
- `internal/server/descendant_work_liveness_test.go`
- `internal/server/p1_6_regression_test.go`
- `internal/server/p2_regression_test.go`
- `internal/server/p595_delete_streaming_test.go`
- `internal/server/p614_rerun_reservation_test.go`
- `internal/server/p623_panic_window_test.go`
- `internal/server/p630_rerun_cancel_window_test.go`
- `internal/server/protocol.go`
- `internal/server/readpump_control_starvation_test.go`
- `internal/server/rerun_hold_test.go`
- `internal/server/run_driver_liveness_test.go`
- `internal/server/web_composer_history_test.go`
- `internal/session/batch_adapters.go`
- `internal/session/batch_consumers_doc.go`
- `internal/session/batch_migration_test.go`
- `internal/session/batch_outbox_doc.go`
- `internal/session/batch_reaction_test.go`
- `internal/session/batch_read_doc.go`
- `internal/session/batch_recovery_doc.go`
- `internal/session/host_lock_register_race_test.go`
- `internal/session/host_lock_test.go`
- `internal/session/notice_pull_test.go`
- `internal/session/notice_row_ids_test.go`
- `internal/session/session.go`
- `internal/session/session_activity.go`
- `internal/session/session_child_question_test.go`
- `internal/session/session_child_question_void_test.go`
- `internal/session/session_driver_test.go`
- `internal/session/session_lifecycle.go`
- `internal/shell/background.go`
- `internal/shell/background_buffer_test.go`
- `internal/shell/background_cap_test.go`
- `internal/shell/background_release_test.go`
- `internal/shell/background_shell.go`
- `internal/shell/exec_unix.go`
- `internal/shell/exec_windows_test.go`
- `web/tests/async-outcome-badges.spec.ts`
- `web/tests/composer-history-filter.spec.ts`
- `web/tests/live-work-panel.spec.ts`
- `web/tests/wake-schedules.spec.ts`

