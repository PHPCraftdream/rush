# Rush: release-readiness follow-up, round 10

Date: **2026-09-23**. Reviewed worktree based on `main` at `02cb64e6`, including the accumulated uncommitted fixes from the review/fix cycles. This is a focused follow-up to round 9 findings and their regressions; it is not a fresh enumeration of every commit in the preceding week.

## Summary

**Scoped verdict: audit clean.** The final `hs` review found no remaining P0, P1, P2, or P3 in durable continuation identity, queued-call ownership, or the associated mailbox paths. The fixes and targeted tests cover the defects found during this cycle:

| Finding | Severity | Resolution |
| --- | --- | --- |
| Durable background execution completed, but `ExecuteRun` retained the canceled attempt's identity. | P1 | The executed assistant IDs are carried through queue admission and drain outcomes. `ExecuteRun` accepts identities only after `DrainComplete` and checks the confirmed set and terminal row. |
| Intermediate assistant rows were missing from live output and tool-call accounting. | P2 | Assistant IDs are published to a waiting drain as rows are created, then snapshotted for final reconciliation. Baseline and session ownership filters remain in place. |
| Multiple rows or queue entries could lose identity, and map iteration could leave a tool-step ID in place of the terminal ID. | P1/P2 | The drain accumulates IDs across executions; terminal identity is confirmed after the full set and must belong to that set. |
| An ordinary queued turn could inherit the previous turn's identity callback. | P2 | Callback inheritance is limited to calls explicitly marked as accepted mailbox replacements. Ordinary submitted calls keep their own callback or none. |

## Evidence and implementation

- Queue execution and admission publish assistant identities under synchronization, expose live IDs to the waiting drain, and hand off immutable snapshots: `internal/session/run_queue_admission.go`, `run_queue_entry_exec.go`, `run_queue_entry_dispatch.go`, and `run_queue_drain_session.go`.
- `ExecuteRun` accumulates IDs while draining, rejects empty, baseline, foreign, or mismatched terminal identities, and records the terminal ID last: `internal/app/app_run_reviewer.go`.
- Replacement provenance is marked only where `mailbox.replacement` is extracted. `runOwned` carries the parent's callback only for that handoff: `internal/agent/agent.go`, `agent_run.go`, `mailbox_interrupt.go`, and `mailbox_ownership.go`.
- Regression coverage includes multi-row and sequential drains, identity publication before completion, terminal-ID ordering, intermediate tool events, foreign and late callbacks, replacement versus ordinary queued calls, and the live durable-continuation path.

## Verification

Targeted tests passed in `internal/agent`, `internal/session`, and `internal/app`, including:

- `TestDrainSessionNow_WaitedExecution_CarriesAllAssistantIdentities`
- `TestDrainSessionNow_SequentialRowsPreserveAllAssistantIdentities`
- `TestDrainSessionNow_WaitedExecution_PublishesAssistantIDBeforeCompletion`
- waited-execution success, busy, terminal-failure, ack-failure, and lease-loss cases
- `TestDrainIdentityConfirmsTerminalIDAfterFullSet`
- `TestDrainIdentityProcessesToolRowBeforeCompletion`
- `TestRunOwned_ReplacementInheritsOriginalCallsIdentityCallback_R9_1`
- `TestRunNonInteractive_P0_1_LiveContinuation`
- `TestExecuteRunContinuationChainWithToolStepReturnsFullText`
- `TestExecuteRunCycle7DoneFirstDrainDoesNotDuplicateTerseOrStream`
- queued-admission and stream-watchdog hard-cap regressions

Addressed `-race` runs passed for the session drain identity tests and the agent/app replacement, terminal-identity, live-event, and live-continuation tests. OAuth refresh and network dial-budget tests in `internal/config` and `internal/nettransport` also passed in the targeted verification batch. `git diff --check` passed.

The full test suite, full race suite, build, lint, and CI were not run. The clean verdict applies to the reviewed paths and scenarios; it is not a claim that those broader gates passed. The fixes remain uncommitted in the working tree.
