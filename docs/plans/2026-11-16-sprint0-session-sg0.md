# SG0 Phase 2 — `internal/session` pre-EX symbol-graph report

**Status: STOP before MV1–MV5.** The corrected literal placement snapshot is not an acyclic package layout. The measured graph has one cyclic SCC with eight owners and 19 internal directed edges on Linux and Darwin (also Windows), plus 9 receiver-colocation violations. The design’s named seams cover some edges, but do not resolve or even specify a number of observed private-symbol crossings. The recommendations below are proposed design amendments, not claims that a post-EX layout has been measured or proved.

This report is the phase-2 deliverable only. It does not amend `2026-11-15-sprint0-split-session.md`, the master plan, or the phase-1 TSV. No code, tests, builds, vets, or commits were performed for this report. The only repository file written in this phase is this report.

## 1. Scope, evidence, and limitations

### 1.1 Inputs and exact invocation

The layout input is the corrected, provisional MV-only file map at `docs/plans/2026-11-16-sprint0-session-layout.tsv` (284 rows; SHA-256 `e57703110e88d60d53d33d63f767b0d8fe1ae19765024cfa5260b6a6d1f90dea`). The raw graph outputs and status files are in `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/session-sg0-20261116/`.

The existing Windows checker binary was reused; it was not rebuilt. Its phase-1 SHA-256 was `2858167434c7c51d4986fc54d084ca7605564b4f587a5525bc3e29f786454615` (Go 1.27.0, build revision `66efec3c5d1f89aaf5acdfe488cb9e7768b8beb1`). For each `GOOS` below, with `GOARCH=amd64`, the exact command was:

```text
D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe -dir D:/dev/go/crush/worktrees/s0-w5-sm1-20261115/internal/session -layout D:/dev/go/crush/worktrees/s0-w5-sm1-20261115/docs/plans/2026-11-16-sprint0-session-layout.tsv -root-owner internal/session -max-elements 7 -json
```

Each run exited 0 and wrote empty stderr. The per-target `.status.txt` records contain the effective command, stdout/stderr byte counts, elapsed time, and exit status; the `.json` files are the raw outputs. Original run records:

| GOOS | Exit | Elapsed | JSON stdout bytes | stderr bytes | Edges / witnesses / cross-boundary uses | Over-limit owners |
|---|---:|---:|---:|---:|---:|---:|
| linux | 0 | 56.997 s | 1,195,892 | 0 | 35 / 267 / 826 | 3 |
| darwin | 0 | 48.095 s | 1,195,892 | 0 | 35 / 267 / 826 | 3 |
| windows | 0 | 45.700 s | 1,190,030 | 0 | 34 / 265 / 822 | 3 |

The checker binary at the shared path was replaced concurrently after those runs (current SHA-256 reported/observed: `34c62a89c8d4f205b640f3cf60b08d83543476b1d7582203d60ec35aa34ff321`; its embedded build revision is `691049508bd3d36ec098076d4af9f99c1b762a56`). An independent orchestrator rerun with that newer binary reported the same headline Linux graph counts. Its receiver diagnostic has a known field regression: `receiver_type_line` is emitted as the method line, rather than the receiver type declaration line. Therefore this report uses the preserved original JSON (which reports the receiver type locations correctly) and independently checked those declaration locations in source. The newer run is corroboration for headline counts only, not the source of the receiver table or witness appendix. This report did not rebuild or execute either binary.

### 1.2 Checker scope limits

The checker’s `parseLayout` explicitly skips `_test.go` rows (`.rush/tmp/sprint0/symgraph/cmd/load.go`, lines 67–69); `loadPackage` also sets `Tests: false` (lines 112–115). Therefore:

- The TSV covers 284 current immediate `.go` files, **123 non-test and 161 test files**, but the graph has 123 rows/scanned files. The test rows are not missing/relocated diagnostics; they are deliberately filtered out.
- SCCs, package counts, boundary witnesses, and receiver checks say nothing about test-only references. Rule 18 test/unexported-symbol ownership was not typechecked here. Test mappings are provisional behavior-based assignments; no claim is made that test closure or future test compilation is proved.
- `-dir` is the immediate `internal/session` package directory. Nested `jobbatch`, `jobbatch/lab`, and `heartbeat` files are not scanned by this run. The proposed MV7/MV8 mappings are not verified by this graph or by this TSV.
- This is the graph of the **literal pre-EX MV placement**, not a simulated/proved final layout. No extraction is assumed to remove an edge. There is no topological order because `acyclic=false`.

## 2. Inventory and owner counts

The corrected TSV maps every current immediate Go file exactly once: 284 rows, 123 non-test and 161 test, no missing/extra/duplicate root files. Counts below are TSV rows (`non-test / test`). The checker sees only the non-test count; its 7-element threshold is applied to that count.

| Target owner | Rows | Non-test | Test | Checker non-test count | `>7`? |
|---|---:|---:|---:|---:|---|
| `internal/session/service` | 77 | 34 | 43 | 34 | yes |
| `internal/session/batch/sessionbatch` | 57 | 26 | 31 | 26 | yes |
| `internal/session/batch/sessionbatchread` | 10 | 5 | 5 | 5 | no |
| `internal/session/batch/finstore` | 2 | 1 | 1 | 1 | no |
| `internal/session/batch/finfacts` | 5 | 3 | 2 | 3 | no |
| `internal/session/batch/finholds` | 12 | 5 | 7 | 5 | no |
| `internal/session/batch/findebt` | 6 | 3 | 3 | 3 | no |
| `internal/session/batch/finterm` | 9 | 5 | 4 | 5 | no |
| `internal/session/proc/sessionlock` | 14 | 8 | 6 | 8 | yes |
| `internal/session/proc/sessioncgroup` | 6 | 4 | 2 | 4 | no |
| `internal/session/run/sessionrun` | 36 | 7 | 29 | 7 | no |
| `internal/session/run/sessiondrain` (temporary) | 18 | 5 | 13 | 5 | no |
| `internal/session/state/sessionhosts` | 13 | 6 | 7 | 6 | no |
| `internal/session/state/sessionnotice` | 8 | 5 | 3 | 5 | no |
| `internal/session/state/sessionwake` | 6 | 3 | 3 | 3 | no |
| `internal/session/state/mcpoutput` | 2 | 1 | 1 | 1 | no |
| `internal/session/state/sessionfacts` | 3 | 2 | 1 | 2 | no |
| **Total** | **284** | **123** | **161** | **123** | **3 owners over** |

The checker also reports the root owner `internal/session` with 0 files/rows: all 123 present non-test files are assigned, and no non-test file is unlisted. Windows excludes 6 build-tagged non-test files in the active package configuration; Linux and Darwin exclude 4 each. This does not change TSV inventory counts.

### 2.1 Actual drift from the design baseline

D1 §1.1 records 130 non-test and 167 test files (297 total). At the baseline tree used by that design, the immediate package had 297. At the measured current tree it has 284 (123+161), a net reduction of 13:

- Five non-test files and five tests (`track*.go`/`track*_test.go`) have moved to `internal/kit/sys/proctrack` (outside this immediate directory and outside this graph).
- Two OS-specific non-test `finalization_facts_holds_tracked_{unix,windows}.go` files were removed when the hold-count read became common code in `finalization_facts_holds_tracked.go`; the OS-specific test files remain in `internal/session`.
- `parallel_gate_test.go` was deleted as part of SM4 helper consolidation.
- No immediate root `.go` files were added relative to that baseline.

Thus 130/167 is a design snapshot, not today’s immediate-directory inventory. The design §2.4 aggregate includes five relocated proctrack source files; the current root graph correctly does not. The current TSV also excludes nested jobbatch/heartbeat files by user scope.

### 2.2 Literal MV placement clarifications

- `batch_commit.go` and `batch_commit_doc.go` map to `internal/session/service` until EX6. `BatchStore` methods remain in `internal/session/batch/sessionbatch` until EX6.
- `batch_timeout_checkin.go`, `batch_timeout_checkin_lifecycle_test.go`, and `batch_timeout_checkin_test.go` map to `internal/session/batch/sessionbatchread`, following the explicit §2.4 line 186 and MV3 line 375 placement. `retention_purge.go` remains in `sessionbatch` before EX6.
- Admission and entry execution remain in `internal/session/run/sessionrun` before EX7.
- MV4 says only “run: 12 files → sessionrun/sessiondrain(temporary)/sessiontest” and does not give the temporary `sessiondrain` file list. This TSV provisionally maps the five files that EX7 later names for `rundrain`—`run_queue_drain_session.go`, `run_queue_drain_ledger.go`, `run_queue_drain_classify.go`, `run_queue_row_context.go`, and `run_queue_orphan_drain.go`—to temporary `internal/session/run/sessiondrain`; `run_queue_admission.go`, `run_queue_entry_exec.go`, pump, lifecycle, dispatch, clock and testsupport stay in `sessionrun`. This is an explicit inference from EX7’s later list, not a definitive MV4 assignment. It creates measured receiver splits before EX7, which must be resolved in design/order before any lane crosses that boundary.
- **Design omission:** the §2.2 tree enumerates seven children under `batch/` but omits `sessionbatchread`; §2.3, §2.4, §2.6 and MV3 do use it. The TSV follows the specific §2.4/MV3 placements. The final tree/parent element count should explicitly account for this eighth child or establish another intended parent.
- MV7 and MV8 are subordinate proposals, not measured here. `internal/session/jobbatch`, `internal/session/jobbatch/lab`, and `internal/session/heartbeat` were not part of this immediate-directory TSV or graph. Do not treat their proposed mappings or package cycles as verified.

### 2.3 Explicit test rows in the TSV that contradict §2.5

The phase-1 TSV currently assigns two tests to `internal/session/batch/sessionbatch` even though §2.5 explicitly maps `batch_cli*` and `batch_api_*` tests to `internal/session/batch/sessionbatchread` (line 212):

| TSV file | Current TSV owner | §2.5 owner |
|---|---|---|
| `batch_cli_store_test.go` | `internal/session/batch/sessionbatch` | `internal/session/batch/sessionbatchread` |
| `batch_api_held_questions_test.go` | `internal/session/batch/sessionbatch` | `internal/session/batch/sessionbatchread` |

This report leaves the phase-1 TSV unchanged as instructed. The owner table in §2 counts the current TSV literally; its test-row split for these two files is provisional and not a statement that the table’s mapping has been accepted. Correct the TSV only with operator authorization, then rerun the graph (tests remain invisible to this checker either way).

## 3. Observed graph result

Linux/Darwin report the same single SCC:

```text
internal/session/batch/finstore
internal/session/batch/finterm
internal/session/batch/sessionbatch
internal/session/batch/sessionbatchread
internal/session/run/sessiondrain
internal/session/run/sessionrun
internal/session/service
internal/session/state/sessionnotice
```

Windows reports the same eight owners. It contains **19 internal directed edges** on Linux/Darwin/Windows. There is no topological order. This is an observed property of the exact layout TSV, not a claim about a hypothetical post-EX graph.

### 3.1 Every internal SCC edge and lane assessment

Edge numbers in §3.1 are local to the SCC table; their corresponding positions in the full 35-edge Linux appendix (§8.1) are 6–12, 14, 16, 19–20, 22–26, and 32–34. “Gap” means the current D1 text does not specify enough ownership/port/order to prove resolution.

| # | Directed edge (owner labels abbreviated only in this table) | Uses / distinct witnesses | Assessment against D1 |
|---:|---|---:|---|
| 1 | `batch/finstore → batch/finterm` | 11 / 6 | **Design gap G1.** `AgentFinalizationStore` calls `AgentFinalizationCandidate` and its unexported `valid`; no S/EX lane places `AgentFinalizationCandidate`/`valid` or accounts for a finstore→finterm edge. |
| 2 | `batch/finterm → batch/finstore` | 101 / 19 | **Partial, G1.** §2.6 anticipates finstore dependencies on finterm and §3 lists a small finterm→finstore boundary, but does not account for methods on the finstore receiver living in finterm. Five receiver methods are split; private `valid`, `active`, `.conn`, and `readAgentFinalization` also cross. This is not repaired by exporting them. |
| 3 | `batch/finterm → batch/sessionbatch` | 9 / 1 | **Gap G2.** The witness is `BatchDeliveryEnvelope` declared in `batch_delivery_store.go`, which EX6 moves into batchdeliver. Neither EX1’s types list nor the package graph states where this shared envelope type/API belongs or how finterm consumes it without a reverse dependency. |
| 4 | `batch/finterm → service` | 2 / 2 | **Partial.** `Session` is explicitly moved to sesstypes in EX1, which should move this use to `sesstypes`; the target dependency from finterm is not explicit in §2.6. Name that consumer edge rather than assuming the alias fixes ownership. |
| 5 | `batch/sessionbatch → batch/finstore` | 1 / 1 | **Named direction.** §2.6 lists sessionbatch→finstore. The witness is `ErrAgentFinalizationChanged`; its ownership/alias route should still be explicit when the component is split. |
| 6 | `batch/sessionbatch → batch/sessionbatchread` | 29 / 21 | **Gap G3.** Root history/adapter code reads `BatchLiveReader` and private batch-read helpers. §2.6 documents sessionbatchread→sessionbatch, not this reverse edge. EX6 moves some history consumers but does not specify this read seam. The boundary is a cycle with edge 5+other links, not a proven post-EX cycle. |
| 7 | `batch/sessionbatch → service` | 34 / 9 | **Partial, G4.** S2 names five public guard API identifiers; EX4 moves guard files; EX6 moves commit/doc/helpers. The measured edge also has `batchCommitInt63`, `batchCommitSpecHash`, `finalizationFenceError`, `admitFinalizationMutation`, `batchFinalizationAdmission`, and `batchCommitReleaseError`, plus `ErrBatchValueOverflow`. Exact ownership and call sites are not all reconciled with S2’s five-name surface. `finalizationScoped`/`validateBatchFinalizationTx` are named elsewhere and should be treated as such. |
| 8 | `batch/sessionbatch → state/sessionnotice` | 22 / 8 | **Partial, G5.** EX1 moves `NoticeKind*` and `PendingNoticeDebt` into types_notice/types_batch; it does not explicitly name `DebtNoticeRef`. Identify all data-only symbols as sesstypes (or state another port) to remove this dependency on the state implementation. |
| 9 | `batch/sessionbatchread → state/sessionnotice` | 3 / 1 | **Named via EX1.** `NoticeKindWakeOnly` falls under EX1’s `NoticeKind*` move; explicitly route the read package to sesstypes. |
| 10 | `run/sessiondrain → run/sessionrun` | 133 / 43 | **Partial, G6.** EX7 and S5 cover admission/exec/drain decomposition and `DrainSessionNow`, but the measured edge includes many private pump fields/methods, `admissionEntry`/`admissionOutcome` implementation details, and config fields. EX7 does not inventory all accesses/ports; its desired graph says drain helpers depend downward on admission/finqueue, not back into sessionrun. The 133 uses include the method still in the drain file at measurement time. |
| 11 | `run/sessiondrain → service` | 33 / 16 | **Gap G7.** EX1 lists RunQueueEntry/DrainResult types; however drain code uses the Service interface and queue/outbox methods. The graph does not name a lower consumer port/interface for these methods. Define it or keep calls behind the pump; do not export implementation details merely to satisfy the split. |
| 12 | `run/sessionrun → run/sessiondrain` | 3 / 2 | **Named only as a dependency direction in §2.6/EX7.** `drainOrphanOutbox` and `WithRunQueueRowID` make this sessionrun→drain direction explicit. The receiver form of the three orphan methods still needs conversion/port design (G6); package placement alone does not legalize a receiver split. |
| 13 | `run/sessionrun → service` | 114 / 22 | **Partial, G8.** EX1/EX3 move queue data/store logic; EX4 names `recordFinalizationQueueFailure` and the queue lease file. `ceilUnixSeconds` in `session_runqueue.go` is not assigned as a shared helper/port, and the component consumer’s required Service surface is not enumerated. |
| 14 | `service → batch/finstore` | 140 / 14 | **Partial, G9.** EX4 moves authority/guard callers to sessguard and §2.6 expects sessguard→finstore. However the moved consumer needs `AgentFinalizationIdentity.valid`, `AgentFinalizationRecord.active`, and `readAgentFinalization`; the finstore API list/§3 port table does not name these private helpers. Name an implementation port or move their callers/definitions. |
| 15 | `service → batch/sessionbatch` | 66 / 42 | **Partially scheduled by EX6/S9, G10.** The edge includes the commit protocol/types that EX6 moves, plus `BatchStore.writer`, private `finalizationAdmission` fields, and `batchRequireRow` used by a finalization queue lease caller. S2 addresses writer passing; EX6 should make commit-only fields local to sessionbatch. The `batchRequireRow` cross-call and any remaining private field uses need explicit treatment. |
| 16 | `service → run/sessionrun` | 1 / 1 | **Gap G11.** `finalization_queue_failure.go:21` uses `ErrCallQueuedNotExecuted` from the pump. EX4 moves the caller to finqueue, but EX1 does not move the sentinel, and §2.6 does not list finqueue→sessionrun. Put the immutable outcome sentinel in a shared lower contract (with a core alias if needed) or specify another acyclic port. |
| 17 | `state/sessionnotice → batch/finstore` | 2 / 1 | **Gap G12.** `session_notice_store.go:153` uses `ErrAgentFinalizationChanged`; §2.6 does not list sessionnotice→finstore. Clarify shared error ownership or provide a lower contract; do not add a reverse finstore dependency. |
| 18 | `state/sessionnotice → batch/sessionbatch` | 10 / 5 | **Gap G13; contradicts §2.6 line 236.** `session_notice_store.go:385` calls `batchOutboxDebtNodesVoidAware`, and line 395 reads private `batchDebtNodeRow.ref`. Moving only `BatchPendingDebt` to sesstypes does not remove the query/algorithm edge. This contradicts the design claim that no state→batch edge exists. Move the algorithm to its consuming owner or specify a narrow acyclic port and remeasure. |
| 19 | `state/sessionnotice → service` | 9 / 5 | **Partial, G14.** S7 claims guard delivery through the core/S2. But the actual cross-package names are unexported (`admitFinalizationMutation`, `batchCommitReleaseError`, `batchFinalizationAdmission`, `finalizationScoped`, `validateBatchFinalizationTx`). The public five-name S2 list does not itself make these private names callable from a sibling package. Specify consumer-defined port/constructor wiring and preserve Rule 18; do not export by default. |

### 3.2 What is and is not an inferred fix

The observed graph is the MV snapshot. It is fair to infer that S2/S3, EX1, EX4, EX6, and EX7 may move or redirect some of these symbols. That is not evidence that the resulting graph is acyclic. In particular, no report statement above assumes that moving declarations into sesstypes, sessguard, finstore, batchdeliver, runadmission, or rundrain eliminates cycles unless both the outgoing calls and the reverse imports are specified. The complete SCC witness evidence is in §8.1 so a design amendment can trace each symbol.

## 4. Receiver-colocation violations

The original Linux JSON reports nine violations. The `method_line` and `receiver_type_line` below were verified against source declarations (not the newer binary’s faulty type-line field). All `AgentFinalizationStore` receiver types are declared at `internal/session/agent_finalization.go:44`; `RunQueuePump` is declared at `internal/session/run_queue_pump.go:510`.

| Method | Method declaration (path:line; owner) | Receiver declaration (path:line; owner) | Design coverage / issue |
|---|---|---|---|
| `AgentFinalizationStore.StoreCandidateTx` | `internal/session/finalization_candidate.go:64`; `batch/finterm` | `internal/session/agent_finalization.go:44`; `batch/finstore` | G1: no EX lane assigns this receiver method with its type. |
| `AgentFinalizationStore.RecoverCandidate` | `internal/session/finalization_candidate.go:128`; `batch/finterm` | `internal/session/agent_finalization.go:44`; `batch/finstore` | G1. |
| `AgentFinalizationStore.RetireStaleActive` | `internal/session/finalization_retire.go:16`; `batch/finterm` | `internal/session/agent_finalization.go:44`; `batch/finstore` | G1. |
| `AgentFinalizationStore.RetireCancelledLogical` | `internal/session/finalization_retire.go:41`; `batch/finterm` | `internal/session/agent_finalization.go:44`; `batch/finstore` | G1. |
| `AgentFinalizationStore.TrySeal` | `internal/session/finalization_seal.go:126`; `batch/finterm` | `internal/session/agent_finalization.go:44`; `batch/finstore` | G1. |
| `RunQueuePump.DrainSessionNow` | `internal/session/run_queue_drain_session.go:13`; temporary `run/sessiondrain` | `internal/session/run_queue_pump.go:510`; `run/sessionrun` | S5/SM3 explicitly says move method to pump; measured source/layout has not done so. Ensure the SM3 placement precedes the package split. |
| `RunQueuePump.drainOrphanOutbox` | `internal/session/run_queue_orphan_drain.go:19`; temporary `run/sessiondrain` | `internal/session/run_queue_pump.go:510`; `run/sessionrun` | EX7 names the method, but moving its file does not move its receiver. Needs component receiver/free-function/port design before the boundary. |
| `RunQueuePump.processOrphanOutboxEntry` | `internal/session/run_queue_orphan_drain.go:49`; temporary `run/sessiondrain` | `internal/session/run_queue_pump.go:510`; `run/sessionrun` | Same G6. |
| `RunQueuePump.recordOrphanOutboxDrainFailure` | `internal/session/run_queue_orphan_drain.go:175`; temporary `run/sessiondrain` | `internal/session/run_queue_pump.go:510`; `run/sessionrun` | Same G6. |

The first five are an uncovered receiver/type boundary even though EX4/EX6 touch finalization files generally. The final four are not a clean “move file” operation; specifically, the three orphan methods are named under EX7 but cannot be moved with their current `*RunQueuePump` receiver to `rundrain` while `RunQueuePump` remains in `sessionrun`. Amend lane sequencing/port design before work crosses this boundary. Rule 18 requires exact seam decisions without gratuitous exports; Rule 19 requires graph and receiver gates before heavy lanes.

## 5. Required boundary surface (unexported symbols)

The Linux original-binary output reports **16 owner boundaries and 78 unexported symbol witnesses**. This is a measured list of private references that would cross the proposed package boundaries if the TSV were executed literally. It is not a list of symbols to export: under Rule 18, preferred remedies are declaration/caller relocation, consumer-defined ports, or a lower shared contract. Appendix §8.2 gives every unexported symbol with declaration and one observed use. The boundary summary is:

| Boundary | Required private identifiers (count) | Named design match / gap |
|---|---|---|
| `batch/finfacts → service` | `finalizationAncestorLimit` (1) | G15: EX4 moves ancestors to sessguard, but finfacts’ descendant SQL uses this constant; finfacts→sessguard/import seam is not named. |
| `batch/finstore → batch/finterm` | `AgentFinalizationCandidate.valid` (1) | G1. |
| `batch/finterm → batch/finstore` | `AgentFinalizationIdentity.valid`, `AgentFinalizationRecord.active`, `AgentFinalizationStore.conn`, `readAgentFinalization` (4) | G1/G9. |
| `batch/sessionbatch → batch/sessionbatchread` | `BatchLiveCounts.add`, `BatchLiveReader.batchLevelRows`, `BatchLiveReader.hostStatus`, `batchLiveFact`, `batchReadChunk`, `batchReadTerminal`, `errBatchReadUnavailable` (7) | G3. |
| `batch/sessionbatch → service` | `admitFinalizationMutation`, `batchCommitInt63`, `batchCommitReleaseError`, `batchCommitSpecHash`, `batchFinalizationAdmission`, `finalizationFenceError`, `finalizationScoped`, `validateBatchFinalizationTx` (8) | G4; partial S2/EX4/EX6 coverage. |
| `proc/sessioncgroup → proc/sessionlock` | `isErrno`, `sanitiseSessionID` (2) | §3 boundary summary anticipates childgroup→lock, but exact private helper names and handling are not specified. |
| `run/sessiondrain → run/sessionrun` | `RunQueuePump.admitMu`, `RunQueuePump.admitSession`, `RunQueuePump.cfg`, `RunQueuePump.ctx`, `RunQueuePump.dbWriteTimeout`, `RunQueuePump.execSem`, `RunQueuePump.executeEntrySync`, `RunQueuePump.leaseTTL`, `RunQueuePump.stopping`, `RunQueuePump.workerWg`, `admissionEntry.done`, `admissionEntry.observeAssistantIDs`, `admissionEntry.outcome`, `admissionOutcome`, `admissionOutcome.assistantIDs`, `admissionOutcome.err`, `admissionOutcome.kind`, `admissionOutcome.rowID`, `admissionOutcome.terminalAssistantID`, `busyOutcome`, `errLeaseLost`, `executedOutcome`, `noRowTouched`, `outcomeBusy`, `outcomeNoRowTouched`, `outcomeTerminalDeleted`, `recordDrainAssistantIdentity`, `recordDrainAssistantIdentityLive`, `terminalDeletedOutcome` (29) | G6; S4 covers `admissionOutcome` placement, S5 covers the drain method, EX7 names components, but the complete current private pump/entry surface is not translated into ports. |
| `run/sessionrun → run/sessiondrain` | `RunQueuePump.drainOrphanOutbox` (1) | EX7 lists the method but its receiver must change; G6. |
| `run/sessionrun → service` | `ceilUnixSeconds`, `recordFinalizationQueueFailure`, `withFinalizationQueueLease` (3) | EX4 names failure/lease files; `ceilUnixSeconds` remains unassigned (G8). |
| `service → batch/finstore` | `AgentFinalizationIdentity.valid`, `AgentFinalizationRecord.active`, `readAgentFinalization` (3) | G9; design’s sessguard→finstore five-name port does not enumerate these. |
| `service → batch/sessionbatch` | `BatchProjection.finalizationAdmission`, `BatchReservation.finalizationAdmission`, `BatchStore.writer`, `batchRequireRow` (4) | G10; S2 resolves writer parameter, EX6 relocates commit users, but queue lease’s `batchRequireRow` call needs a decision. |
| `state/sessionfacts → proc/sessionlock` | `pidSidecarPath`, `sanitiseSessionID` (2) | G16: §3 only names `InspectSessionLock` plus types; these two helpers are additional private boundary symbols. |
| `state/sessionhosts → proc/sessionlock` | `classifyAndLock`, `isContentionError`, `isLockContentionError`, `openLockFile`, `tryLockFile` (5) | G17: hosts→lock helper API/port absent from §2.6 and the named boundary list. |
| `state/sessionnotice → batch/sessionbatch` | `batchDebtNodeRow.ref`, `batchOutboxDebtNodesVoidAware` (2) | G13; explicit contradiction to “no state→batch” claim. |
| `state/sessionnotice → service` | `admitFinalizationMutation`, `batchCommitReleaseError`, `batchFinalizationAdmission`, `finalizationScoped`, `validateBatchFinalizationTx` (5) | G14; S7 describes the behavior but not private-name/package-call mechanics. |
| `state/sessionwake → service` | `homeFlag` (1) | G18: EX1 lists workspace/folder types but not this helper; no wake→core seam is named. |

### 5.1 Other measured package references requiring explicit ownership

The complete witness appendix also includes exported references, which matter to the cycle even where they are already public API. Specific design reconciliations:

1. **Session-run sentinel.** `ErrCallQueuedNotExecuted` is declared in `run_queue_pump.go:124` and referenced by `finalization_queue_failure.go:21`. EX4 moves the caller into finqueue; the design needs a shared low-level sentinel contract or an explicit port. Adding an import from finqueue to sessionrun would not match the intended dependency graph.
2. **Session-notice debt algorithm.** `session_notice_store.go:385` calls `batchOutboxDebtNodesVoidAware`; line 395 reads its private `batchDebtNodeRow.ref`. Moving the `BatchPendingDebt` data type alone (EX1) does not move this algorithm. Amend §2.6/§3/EX6 to choose the algorithm owner and remove the observed state→batch direction.
3. **Host/lock helpers.** Host lock files call five private sessionlock helpers; sessionfacts uses `pidSidecarPath`/`sanitiseSessionID`; sessioncgroup uses `isErrno`/`sanitiseSessionID`. The latter direction is broadly anticipated by the §3 childgroup→lock boundary, but private identifiers need explicit non-export remedies. The host and facts directions are not enumerated in the package graph/port list.
4. **Wake helper.** `wake_schedule_store.go:98` calls `homeFlag` in `session_workspace.go:114`; EX1’s folder-scope extraction does not explicitly assign this conversion.
5. **Finalization error identity.** `session_notice_store.go:153` refers to `ErrAgentFinalizationChanged` in finstore. If the error remains owned in finstore, document the acyclic import/port; alternatively name a lower shared error contract and preserve the API alias.

## 6. Proposed design amendments (inference, not measured fixes)

Before accepting MV1 as ready under Rule 19, update the D1 design—not implementation—to account for G1–G18 and rerun the graph against a revised layout. At minimum the amendment should:

1. Resolve `AgentFinalizationCandidate` and all five `AgentFinalizationStore` receiver methods as one owner/component decision; list `valid`, `active`, `conn`, and `readAgentFinalization` ports/relocations. Name the resulting finstore↔finterm direction and show it is one-way.
2. Add the exact `BatchDeliveryEnvelope` owner and finterm dependency; explicitly route `Session` to sesstypes and show the import edge. State whether `DebtNoticeRef` joins types_notice/types_batch.
3. Design the `sessionbatchread`/batch root seam in both directions. Keep the corrected MV placement for timeout-checkin. Include `BatchLiveReader` private helpers and the consumers that must move or use a port.
4. Reconcile all measured batch→service private functions/types with S2/EX4/EX6, including `batchCommitInt63`, `batchCommitSpecHash`, `ErrBatchValueOverflow`, `finalizationFenceError`, and `batchRequireRow`; preserve writer/transaction semantics and do not rely on private exports.
5. Correct the session-notice debt algorithm claim and move it or place it behind an acyclic, typed port. Move/alias outcome errors used by finqueue (including `ErrCallQueuedNotExecuted`) without introducing finqueue→sessionrun.
6. Add explicit sessionlock APIs/ports for hosts, sessionfacts, and childgroup, distinguishing the already-described childgroup direction from the missing hosts/facts calls. Specify how `homeFlag` is owned/consumed.
7. Inventory the actual run component surface: service methods/types, pump fields, admission state, `ceilUnixSeconds`, `withFinalizationQueueLease`, and orphan drain. Make SM3’s `DrainSessionNow` move precede MV4 in the schedule and convert all three orphan pump methods to receiver-safe component functions/objects before assigning them to rundrain. Recheck the intended one-way sessionrun→run components graph.
8. Add `sessionbatchread` to the §2.2 target tree/count, and give MV4’s temporary sessiondrain placement an explicit file-by-file list. Keep MV7/MV8 as unverified separate layouts unless separately inventoried and graphed.
9. Expand the test map for the exact ambiguities below. Rule 18 requires compiler-backed ownership of unexported references; filename/prefix matching and this non-test-only graph cannot establish it.

These are design proposals based on observed edges and source sites. They have **not** been projected, compiled, typechecked, or validated by tests. Do not convert this report into a claim that the future graph is acyclic.

## 7. Test-file placements not uniquely resolved by §2.5

These 37 current test files are not uniquely assigned by a named behavior/prefix in §2.5 (or are listed only under a broad family without a unique component). The TSV keeps each with the most conservative current owner inferred from test package/private references or direct behavior. This is provisional; because the checker filters tests, none of these assignments has been mechanically validated for unexported symbol closure. “Unplaced” here means “not uniquely placed by the design”; all 37 do have a provisional TSV destination.

| File | Provisional owner | Reason for conservative placement |
|---|---|---|
| `agent_finalization_test.go` | `internal/session/batch/finstore` | Tests finalization store identity/record behavior; no exact prefix row in §2.5. |
| `batch_await_facts_test.go` | `internal/session/batch/sessionbatch` | Exercises batch reaction debt and live-reader facts together; the design gives no unique owner. |
| `batch_child_bind_test.go` | `internal/session/batch/sessionbatch` | Root BatchStore child-bind path; no explicit prefix row. |
| `batch_commit_close_test.go` | `internal/session/service` | Commit/finalization guard test remains with commit/guard owners until EX6/EX4. |
| `batch_commit_contention_test.go` | `internal/session/service` | Commit-protocol behavior remains at the pre-EX core placement. |
| `batch_commit_test.go` | `internal/session/service` | Commit protocol remains service until EX6. |
| `batch_commit_timeout_diagnostics_test.go` | `internal/session/service` | Commit timeout diagnostics remain with commit protocol before EX6. |
| `batch_control_races_test.go` | `internal/session/batch/sessionbatch` | Control/fence root behavior; broad batch tests row does not specify a new component. |
| `batch_control_test.go` | `internal/session/batch/sessionbatch` | Control root behavior; broad batch tests row does not specify a new component. |
| `batch_finalization_tx_test.go` | `internal/session/service` | Tests guard/transaction integration retained in core until EX4. |
| `batch_migration_test.go` | `internal/session/batch/sessionbatch` | Root store schema/registration behavior; not assigned by named prefix. |
| `batch_read_behavior_test.go` | `internal/session/batch/sessionbatchread` | Exercises reader behavior and private reader types. |
| `batch_read_live_served_test.go` | `internal/session/batch/sessionbatchread` | Tests served live reader implementation/private SQL seam. |
| `batch_read_test.go` | `internal/session/batch/sessionbatchread` | Uses `BatchLiveReader` internals/helpers; assigned with reader before extraction. |
| `batch_sqllog_test.go` | `internal/session/batch/sessionbatch` | Batch SQL/protocol integration crosses root store; no unique EX component mapping. |
| `batch_stop_fence_test.go` | `internal/session/batch/sessionbatch` | Explicit S6 stop-fence behavior remains at root. |
| `batch_stop_test.go` | `internal/session/batch/sessionbatch` | Stop/control root behavior. |
| `batch_store_reader_test.go` | `internal/session/batch/sessionbatch` | Uses root store/read setup; not uniquely placed by behavior prefix. |
| `batch_store_test.go` | `internal/session/batch/sessionbatch` | Root store fixtures and private store helpers. |
| `batch_terminal_delivery_test.go` | `internal/session/batch/sessionbatch` | Terminal delivery spans commit, notice and delivery behaviors; retained conservatively at root. |
| `batch_timeout_checkin_lifecycle_test.go` | `internal/session/batch/sessionbatchread` | Follows explicit corrected timeout-checkin placement. |
| `batch_timeout_checkin_test.go` | `internal/session/batch/sessionbatchread` | Follows explicit corrected timeout-checkin placement. |
| `child_question_format_test.go` | `internal/session/state/sessionnotice` | Directly tests notice/question formatting; no distinct EX1 type test owner stated. |
| `finalization_testkit_test.go` | `internal/session/service` | Uses core-private finalization fixtures; do not move without Rule 18 symbol audit. |
| `p1_5_clobber_fix_test.go` | `internal/session/proc/sessionlock` | Private lock metadata seam; retained with lock owner. |
| `p2_1_fifo_tiebreak_test.go` | `internal/session/run/sessionrun` | Run queue FIFO behavior but not an explicitly named §2.5 prefix. |
| `p575b_no_execution_sentinel_test.go` | `internal/session/run/sessiondrain` | Drain/no-execution behavior; boundary-private tests require explicit review. |
| `p631_released_lock_test.go` | `internal/session/proc/sessionlock` | Private shared-lock probe/acquire seam. |
| `session_calltree_test.go` | `internal/session/service` | Service call-tree behavior; no named package-level prefix. |
| `session_concurrency_test.go` | `internal/session/service` | Uses service-private fixtures and update behavior. |
| `session_crud_test.go` | `internal/session/service` | Core CRUD behavior, conservatively retained with service. |
| `session_driver_register_error_test.go` | `internal/session/state/sessionhosts` | Driver/host registration error behavior. |
| `session_driver_test.go` | `internal/session/state/sessionhosts` | Driver ownership and host behavior. |
| `session_getlastsession_activity_test.go` | `internal/session/service` | Mixed session/activity behavior; no unique design destination. |
| `session_notice_store_test.go` | `internal/session/state/sessionnotice` | SessionNoticeStore private behavior; includes the measured debt/guard cross-boundaries. |
| `session_test.go` | `internal/session/service` | Core test fixtures/private declarations; conservative package owner. |
| `session_title_event_test.go` | `internal/session/service` | Session title/service event behavior. |

`legacy_import_drain_external_test.go` is the design’s explicit unresolved V8 case (D1 §6, line 424): it remains assigned to `internal/session/service` in the TSV pending a service-versus-`internal/app/itest` decision. This explicit V8 test is **additional** to the 37 tests above, for 38 provisional/ambiguous placements total. Do not silently treat that provisional choice as a settled design result.

## 8. Self-contained symbol evidence appendices

The tables below are rendered from the preserved original Linux JSON. Each witness gives its declaration file/line, exported status, occurrence count, and one first use site. Paths are relative to `internal/session`; the complete raw JSON remains in the scratch evidence directory. The appendix includes every witness on every measured edge; §3.1 labels the 19 edges internal to the SCC. The complete unabridged SCC-only appendix is also retained in scratch as `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/session-sg0-20261116/scc-witnesses.txt`. “Exported” describes the Go identifier as declared today, not a recommendation to export it across a new package.

### 8.1 Every edge and symbol witness

```text
EDGE 1: internal/session/batch/findebt -> internal/session/batch/finfacts; uses=1; witnesses=1
  type FinalizationReactionDebtFacts decl=finalization_facts.go:29; exported=True; occurrences=1; use=finalization_debt.go:20
EDGE 2: internal/session/batch/finfacts -> internal/session/batch/finterm; uses=14; witnesses=7
  type AgentFinalizationScopeFacts decl=finalization_seal.go:97; exported=True; occurrences=7; use=finalization_facts.go:71
  field AgentFinalizationScopeFacts.AgentFinalizationScopeFacts.ActiveDescendants decl=finalization_seal.go:99; exported=True; occurrences=1; use=finalization_facts.go:93
  field AgentFinalizationScopeFacts.AgentFinalizationScopeFacts.Complete decl=finalization_seal.go:98; exported=True; occurrences=1; use=finalization_facts.go:92
  field AgentFinalizationScopeFacts.AgentFinalizationScopeFacts.PhysicalHolds decl=finalization_seal.go:101; exported=True; occurrences=1; use=finalization_facts.go:95
  field AgentFinalizationScopeFacts.AgentFinalizationScopeFacts.QueuedCalls decl=finalization_seal.go:102; exported=True; occurrences=1; use=finalization_facts.go:94
  field AgentFinalizationScopeFacts.AgentFinalizationScopeFacts.ReactionDebt decl=finalization_seal.go:100; exported=True; occurrences=1; use=finalization_facts.go:96
  type AgentFinalizationScopeReader decl=finalization_seal.go:107; exported=True; occurrences=2; use=finalization_facts.go:43
EDGE 3: internal/session/batch/finfacts -> internal/session/service; uses=3; witnesses=1
  const finalizationAncestorLimit decl=finalization_ancestors.go:12; exported=False; occurrences=3; use=finalization_facts_descendants.go:20
EDGE 4: internal/session/batch/finholds -> internal/session/batch/finfacts; uses=12; witnesses=2
  type FinalizationPhysicalHoldsFacts decl=finalization_facts.go:23; exported=True; occurrences=10; use=finalization_facts_holds.go:24
  method FinalizationPhysicalHoldsFacts.FinalizationPhysicalHoldsFacts.FinalizationPhysicalHoldsTx decl=finalization_facts.go:24; exported=True; occurrences=2; use=finalization_facts_holds_sum.go:49
EDGE 5: internal/session/batch/finholds -> internal/session/state/sessionhosts; uses=4; witnesses=4
  const HostStatusAlive decl=host_lock.go:43; exported=True; occurrences=1; use=finalization_facts_holds_host.go:76
  const HostStatusDead decl=host_lock.go:48; exported=True; occurrences=1; use=finalization_facts_holds_host.go:74
  func IsOwnHostID decl=host_lock.go:134; exported=True; occurrences=1; use=finalization_facts_holds_host.go:65
  func ProbeHostShared decl=host_lock.go:253; exported=True; occurrences=1; use=finalization_facts_holds_host.go:68
EDGE 6: internal/session/batch/finstore -> internal/session/batch/finterm; uses=11; witnesses=6
  type AgentFinalizationCandidate decl=finalization_candidate.go:21; exported=True; occurrences=1; use=agent_finalization.go:199
  field AgentFinalizationCandidate.AgentFinalizationCandidate.Hash decl=finalization_candidate.go:22; exported=True; occurrences=2; use=agent_finalization.go:210
  field AgentFinalizationCandidate.AgentFinalizationCandidate.MessageID decl=finalization_candidate.go:22; exported=True; occurrences=2; use=agent_finalization.go:210
  field AgentFinalizationCandidate.AgentFinalizationCandidate.Revision decl=finalization_candidate.go:23; exported=True; occurrences=3; use=agent_finalization.go:211
  field AgentFinalizationCandidate.AgentFinalizationCandidate.Serial decl=finalization_candidate.go:23; exported=True; occurrences=2; use=agent_finalization.go:210
  method AgentFinalizationCandidate.AgentFinalizationCandidate.valid decl=finalization_candidate.go:26; exported=False; occurrences=1; use=agent_finalization.go:200
EDGE 7: internal/session/batch/finterm -> internal/session/batch/finstore; uses=101; witnesses=19
  type AgentFinalizationIdentity decl=agent_finalization.go:15; exported=True; occurrences=5; use=finalization_candidate.go:39
  field AgentFinalizationIdentity.AgentFinalizationIdentity.Generation decl=agent_finalization.go:17; exported=True; occurrences=5; use=finalization_candidate.go:40
  field AgentFinalizationIdentity.AgentFinalizationIdentity.LogicalCallID decl=agent_finalization.go:16; exported=True; occurrences=4; use=finalization_candidate.go:40
  field AgentFinalizationIdentity.AgentFinalizationIdentity.SessionID decl=agent_finalization.go:16; exported=True; occurrences=7; use=finalization_candidate.go:65
  method AgentFinalizationIdentity.AgentFinalizationIdentity.valid decl=agent_finalization.go:22; exported=False; occurrences=1; use=finalization_seal.go:130
  type AgentFinalizationRecord decl=agent_finalization.go:26; exported=True; occurrences=13; use=finalization_candidate.go:46
  field AgentFinalizationRecord.AgentFinalizationRecord.Cancelled decl=agent_finalization.go:35; exported=True; occurrences=3; use=finalization_candidate.go:47
  field AgentFinalizationRecord.AgentFinalizationRecord.CandidateHash decl=agent_finalization.go:30; exported=True; occurrences=9; use=finalization_candidate.go:147
  field AgentFinalizationRecord.AgentFinalizationRecord.CandidateMessageID decl=agent_finalization.go:30; exported=True; occurrences=9; use=finalization_candidate.go:147
  field AgentFinalizationRecord.AgentFinalizationRecord.CandidateSerial decl=agent_finalization.go:33; exported=True; occurrences=6; use=finalization_candidate.go:53
  field AgentFinalizationRecord.AgentFinalizationRecord.OpenTickets decl=agent_finalization.go:34; exported=True; occurrences=1; use=finalization_seal.go:179
  field AgentFinalizationRecord.AgentFinalizationRecord.Phase decl=agent_finalization.go:29; exported=True; occurrences=2; use=finalization_seal.go:145
  field AgentFinalizationRecord.AgentFinalizationRecord.Revision decl=agent_finalization.go:28; exported=True; occurrences=6; use=finalization_candidate.go:53
  method AgentFinalizationRecord.AgentFinalizationRecord.active decl=agent_finalization.go:38; exported=False; occurrences=3; use=finalization_candidate.go:50
  type AgentFinalizationStore decl=agent_finalization.go:44; exported=True; occurrences=7; use=finalization_candidate.go:64
  method AgentFinalizationStore.AgentFinalizationStore.SealTx decl=agent_finalization.go:199; exported=True; occurrences=1; use=finalization_seal.go:219
  field AgentFinalizationStore.AgentFinalizationStore.conn decl=agent_finalization.go:44; exported=False; occurrences=8; use=finalization_candidate.go:132
  var ErrAgentFinalizationChanged decl=agent_finalization.go:10; exported=True; occurrences=8; use=finalization_candidate.go:15
  func readAgentFinalization decl=agent_finalization.go:69; exported=False; occurrences=3; use=finalization_candidate.go:72
EDGE 8: internal/session/batch/finterm -> internal/session/batch/sessionbatch; uses=9; witnesses=1
  type BatchDeliveryEnvelope decl=batch_delivery_store.go:14; exported=True; occurrences=9; use=finalization_notify.go:58
EDGE 9: internal/session/batch/finterm -> internal/session/service; uses=2; witnesses=2
  type Session decl=session.go:48; exported=True; occurrences=1; use=finalization_notify.go:48
  field Session.Session.EndedReason decl=session.go:99; exported=True; occurrences=1; use=finalization_notify.go:49
EDGE 10: internal/session/batch/sessionbatch -> internal/session/batch/finstore; uses=1; witnesses=1
  var ErrAgentFinalizationChanged decl=agent_finalization.go:10; exported=True; occurrences=1; use=batch_pull.go:174
EDGE 11: internal/session/batch/sessionbatch -> internal/session/batch/sessionbatchread; uses=29; witnesses=21
  field BatchLevelResult.BatchLevelResult.Complete decl=batch_read_level.go:17; exported=True; occurrences=1; use=batch_history_snapshot.go:61
  field BatchLevelResult.BatchLevelResult.Disappeared decl=batch_read_level.go:17; exported=True; occurrences=1; use=batch_history_snapshot.go:61
  field BatchLevelResult.BatchLevelResult.Node decl=batch_read_level.go:14; exported=True; occurrences=1; use=batch_history_snapshot.go:86
  field BatchLevelResult.BatchLevelResult.Subtree decl=batch_read_level.go:15; exported=True; occurrences=1; use=batch_history_snapshot.go:71
  type BatchLiveCounts decl=batch_read.go:43; exported=True; occurrences=1; use=batch_history_snapshot.go:28
  method BatchLiveCounts.BatchLiveCounts.add decl=batch_read.go:48; exported=False; occurrences=1; use=batch_history_snapshot.go:78
  type BatchLiveNode decl=batch_read.go:82; exported=True; occurrences=2; use=batch_consumers_doc.go:236
  field BatchLiveNode.BatchLiveNode.HostStatus decl=batch_read.go:90; exported=True; occurrences=1; use=batch_history_snapshot.go:86
  field BatchLiveNode.BatchLiveNode.NodeID decl=batch_read.go:84; exported=True; occurrences=1; use=batch_history_snapshot.go:72
  type BatchLiveReader decl=batch_read.go:21; exported=True; occurrences=2; use=batch_history_snapshot.go:49
  field BatchLiveReader.BatchLiveReader.Source decl=batch_read.go:22; exported=True; occurrences=3; use=batch_history_snapshot.go:53
  method BatchLiveReader.BatchLiveReader.batchLevelRows decl=batch_read_level.go:42; exported=False; occurrences=1; use=batch_history_snapshot.go:60
  method BatchLiveReader.BatchLiveReader.hostStatus decl=batch_read.go:107; exported=False; occurrences=1; use=batch_history_snapshot.go:108
  type BatchLiveRef decl=batch_read.go:40; exported=True; occurrences=1; use=batch_history_snapshot.go:60
  field BatchLiveRef.BatchLiveRef.OwnerSessionID decl=batch_read.go:40; exported=True; occurrences=1; use=batch_history_snapshot.go:60
  field BatchLiveRef.BatchLiveRef.RootID decl=batch_read.go:40; exported=True; occurrences=1; use=batch_history_snapshot.go:60
  method BatchLiveSource.BatchLiveSource.BatchLevelNodes decl=batch_read.go:16; exported=True; occurrences=1; use=batch_history_snapshot.go:56
  func batchLiveFact decl=batch_read.go:95; exported=False; occurrences=1; use=batch_history_snapshot.go:108
  const batchReadChunk decl=batch_read_live.go:15; exported=False; occurrences=4; use=batch_reaction.go:135
  func batchReadTerminal decl=batch_read.go:99; exported=False; occurrences=1; use=batch_history_reset.go:57
  var errBatchReadUnavailable decl=batch_read.go:127; exported=False; occurrences=2; use=batch_history_snapshot.go:54
EDGE 12: internal/session/batch/sessionbatch -> internal/session/service; uses=34; witnesses=9
  var ErrBatchValueOverflow decl=batch_commit.go:20; exported=True; occurrences=1; use=batch_child_bind.go:62
  func admitFinalizationMutation decl=finalization_mutation.go:13; exported=False; occurrences=5; use=batch_child_bind.go:64
  const batchCommitInt63 decl=batch_commit.go:77; exported=False; occurrences=1; use=batch_child_bind.go:61
  func batchCommitReleaseError decl=batch_commit_admission.go:169; exported=False; occurrences=5; use=batch_child_bind.go:66
  func batchCommitSpecHash decl=batch_commit.go:166; exported=False; occurrences=1; use=batch_adapters.go:164
  type batchFinalizationAdmission decl=batch_commit_admission.go:21; exported=False; occurrences=3; use=batch_pull.go:189
  func finalizationFenceError decl=finalization_ingress_helpers.go:268; exported=False; occurrences=6; use=batch_question.go:38
  func finalizationScoped decl=finalization_ingress_helpers.go:277; exported=False; occurrences=1; use=batch_pull.go:188
  func validateBatchFinalizationTx decl=batch_finalization_tx.go:12; exported=False; occurrences=11; use=batch_child_bind.go:135
EDGE 13: internal/session/batch/sessionbatch -> internal/session/state/sessionhosts; uses=15; witnesses=5
  type HostLockStatus decl=host_lock.go:33; exported=True; occurrences=4; use=batch_history_snapshot.go:30
  const HostStatusAlive decl=host_lock.go:43; exported=True; occurrences=1; use=batch_control.go:66
  const HostStatusDead decl=host_lock.go:48; exported=True; occurrences=5; use=batch_control.go:63
  func IsOwnHostID decl=host_lock.go:134; exported=True; occurrences=4; use=batch_recovery.go:29
  func ProbeHostShared decl=host_lock.go:253; exported=True; occurrences=1; use=batch_recovery.go:18
EDGE 14: internal/session/batch/sessionbatch -> internal/session/state/sessionnotice; uses=22; witnesses=8
  type DebtNoticeRef decl=notice_pull.go:90; exported=True; occurrences=3; use=batch_reaction.go:22
  field DebtNoticeRef.DebtNoticeRef.ID decl=notice_pull.go:91; exported=True; occurrences=3; use=batch_reaction.go:169
  field DebtNoticeRef.DebtNoticeRef.NoticeMessageID decl=notice_pull.go:92; exported=True; occurrences=2; use=batch_reaction.go:169
  const NoticeKindChildQuestion decl=notice_pull.go:44; exported=True; occurrences=8; use=batch_api_store.go:71
  const NoticeKindWakeFailed decl=notice_pull.go:30; exported=True; occurrences=1; use=batch_reaction.go:369
  type PendingNoticeDebt decl=notice_row_ids.go:10; exported=True; occurrences=2; use=batch_reaction.go:31
  field PendingNoticeDebt.PendingNoticeDebt.ID decl=notice_row_ids.go:11; exported=True; occurrences=1; use=batch_reaction.go:242
  field PendingNoticeDebt.PendingNoticeDebt.Kind decl=notice_row_ids.go:12; exported=True; occurrences=2; use=batch_reaction.go:242
EDGE 15: internal/session/batch/sessionbatchread -> internal/session/state/sessionhosts; uses=19; witnesses=6
  type HostLockStatus decl=host_lock.go:33; exported=True; occurrences=8; use=batch_read.go:31
  const HostStatusAlive decl=host_lock.go:43; exported=True; occurrences=2; use=batch_read.go:112
  const HostStatusDead decl=host_lock.go:48; exported=True; occurrences=1; use=batch_read.go:121
  const HostStatusUnknown decl=host_lock.go:39; exported=True; occurrences=6; use=batch_read.go:109
  func IsOwnHostID decl=host_lock.go:134; exported=True; occurrences=1; use=batch_read.go:36
  func ProbeHostShared decl=host_lock.go:253; exported=True; occurrences=1; use=batch_read.go:36
EDGE 16: internal/session/batch/sessionbatchread -> internal/session/state/sessionnotice; uses=3; witnesses=1
  const NoticeKindWakeOnly decl=notice_pull.go:24; exported=True; occurrences=3; use=batch_timeout_checkin.go:63
EDGE 17: internal/session/proc/sessioncgroup -> internal/session/proc/sessionlock; uses=4; witnesses=2
  func isErrno decl=errno_unix.go:11; exported=False; occurrences=3; use=childgroup_registry_unix.go:716
  func sanitiseSessionID decl=lock_inspect.go:16; exported=False; occurrences=1; use=childgroup_registry_unix.go:125
EDGE 18: internal/session/run/sessiondrain -> internal/session/proc/sessionlock; uses=1; witnesses=1
  type SessionLockBusyError decl=lock.go:256; exported=True; occurrences=1; use=run_queue_drain_classify.go:197
EDGE 19: internal/session/run/sessiondrain -> internal/session/run/sessionrun; uses=133; witnesses=43
  type AlreadyAttempted decl=run_queue_entry_dispatch.go:21; exported=True; occurrences=1; use=run_queue_drain_classify.go:34
  method AlreadyAttempted.AlreadyAttempted.AlreadyAttempted decl=run_queue_entry_dispatch.go:22; exported=True; occurrences=1; use=run_queue_drain_classify.go:35
  var ErrCallQueuedNotExecuted decl=run_queue_pump.go:124; exported=True; occurrences=5; use=run_queue_drain_classify.go:31
  var ErrDrainFailureUnspecified decl=run_queue_pump.go:253; exported=True; occurrences=3; use=run_queue_drain_ledger.go:232
  var ErrDrainIncomplete decl=run_queue_pump.go:252; exported=True; occurrences=1; use=run_queue_drain_ledger.go:295
  var ErrOutstandingCheckUnconfirmed decl=run_queue_pump.go:321; exported=True; occurrences=1; use=run_queue_drain_session.go:755
  var ErrOutstandingRunQueueEntry decl=run_queue_pump.go:282; exported=True; occurrences=1; use=run_queue_drain_session.go:757
  var ErrRowOutcomeUnconfirmed decl=run_queue_pump.go:168; exported=True; occurrences=1; use=run_queue_drain_session.go:582
  var ErrTurnCommitFailed decl=run_queue_pump.go:140; exported=True; occurrences=1; use=run_queue_drain_classify.go:28
  const RunQueueMaxAttempts decl=run_queue_pump.go:47; exported=True; occurrences=1; use=run_queue_drain_session.go:258
  type RunQueuePump decl=run_queue_pump.go:510; exported=True; occurrences=4; use=run_queue_drain_session.go:13
  field RunQueuePump.RunQueuePump.admitMu decl=run_queue_pump.go:549; exported=False; occurrences=2; use=run_queue_drain_session.go:328
  method RunQueuePump.RunQueuePump.admitSession decl=run_queue_admission.go:347; exported=False; occurrences=1; use=run_queue_drain_session.go:75
  field RunQueuePump.RunQueuePump.cfg decl=run_queue_pump.go:511; exported=False; occurrences=30; use=run_queue_drain_session.go:105
  field RunQueuePump.RunQueuePump.ctx decl=run_queue_pump.go:512; exported=False; occurrences=4; use=run_queue_orphan_drain.go:21
  method RunQueuePump.RunQueuePump.dbWriteTimeout decl=run_queue_pump.go:482; exported=False; occurrences=5; use=run_queue_drain_session.go:259
  field RunQueuePump.RunQueuePump.execSem decl=run_queue_pump.go:647; exported=False; occurrences=3; use=run_queue_drain_session.go:286
  method RunQueuePump.RunQueuePump.executeEntrySync decl=run_queue_entry_exec.go:94; exported=False; occurrences=1; use=run_queue_drain_session.go:385
  method RunQueuePump.RunQueuePump.leaseTTL decl=run_queue_pump.go:463; exported=False; occurrences=1; use=run_queue_drain_session.go:215
  field RunQueuePump.RunQueuePump.stopping decl=run_queue_pump.go:550; exported=False; occurrences=1; use=run_queue_drain_session.go:329
  field RunQueuePump.RunQueuePump.workerWg decl=run_queue_pump.go:523; exported=False; occurrences=2; use=run_queue_drain_session.go:331
  field RunQueuePumpConfig.RunQueuePumpConfig.PumpInstanceID decl=run_queue_pump.go:334; exported=True; occurrences=19; use=run_queue_drain_session.go:215
  field RunQueuePumpConfig.RunQueuePumpConfig.Sessions decl=run_queue_pump.go:326; exported=True; occurrences=9; use=run_queue_drain_session.go:215
  field RunQueuePumpConfig.RunQueuePumpConfig.TestAfterAdmissionRefusal decl=run_queue_pump.go:410; exported=True; occurrences=2; use=run_queue_drain_session.go:105
  field admissionEntry.admissionEntry.done decl=run_queue_admission.go:33; exported=False; occurrences=1; use=run_queue_drain_session.go:122
  method admissionEntry.admissionEntry.observeAssistantIDs decl=run_queue_admission.go:78; exported=False; occurrences=1; use=run_queue_drain_session.go:77
  field admissionEntry.admissionEntry.outcome decl=run_queue_admission.go:46; exported=False; occurrences=3; use=run_queue_drain_session.go:133
  type admissionOutcome decl=run_queue_admission.go:161; exported=False; occurrences=1; use=run_queue_drain_classify.go:97
  field admissionOutcome.admissionOutcome.assistantIDs decl=run_queue_admission.go:180; exported=False; occurrences=1; use=run_queue_drain_session.go:136
  field admissionOutcome.admissionOutcome.err decl=run_queue_admission.go:177; exported=False; occurrences=4; use=run_queue_drain_classify.go:149
  field admissionOutcome.admissionOutcome.kind decl=run_queue_admission.go:168; exported=False; occurrences=1; use=run_queue_drain_classify.go:98
  field admissionOutcome.admissionOutcome.rowID decl=run_queue_admission.go:165; exported=False; occurrences=4; use=run_queue_drain_classify.go:151
  field admissionOutcome.admissionOutcome.terminalAssistantID decl=run_queue_admission.go:183; exported=False; occurrences=1; use=run_queue_drain_session.go:136
  func busyOutcome decl=run_queue_admission.go:197; exported=False; occurrences=1; use=run_queue_drain_session.go:336
  var errLeaseLost decl=run_queue_pump.go:175; exported=False; occurrences=2; use=run_queue_drain_classify.go:28
  func executedOutcome decl=run_queue_admission.go:203; exported=False; occurrences=3; use=run_queue_drain_session.go:380
  func noRowTouched decl=run_queue_admission.go:191; exported=False; occurrences=3; use=run_queue_drain_session.go:231
  const outcomeBusy decl=run_queue_admission.go:149; exported=False; occurrences=1; use=run_queue_drain_classify.go:127
  const outcomeNoRowTouched decl=run_queue_admission.go:122; exported=False; occurrences=1; use=run_queue_drain_classify.go:99
  const outcomeTerminalDeleted decl=run_queue_admission.go:142; exported=False; occurrences=1; use=run_queue_drain_classify.go:136
  func recordDrainAssistantIdentity decl=run_queue_admission.go:230; exported=False; occurrences=2; use=run_queue_drain_session.go:136
  func recordDrainAssistantIdentityLive decl=run_queue_admission.go:270; exported=False; occurrences=1; use=run_queue_drain_session.go:78
  func terminalDeletedOutcome decl=run_queue_admission.go:281; exported=False; occurrences=1; use=run_queue_drain_session.go:281
EDGE 20: internal/session/run/sessiondrain -> internal/session/service; uses=33; witnesses=16
  field OrphanOutboxFailureOutcome.OrphanOutboxFailureOutcome.AlreadyTerminal decl=session_orphan_outbox.go:266; exported=True; occurrences=1; use=run_queue_orphan_drain.go:185
  field OrphanOutboxFailureOutcome.OrphanOutboxFailureOutcome.Attempts decl=session_orphan_outbox.go:256; exported=True; occurrences=2; use=run_queue_orphan_drain.go:194
  field OrphanOutboxFailureOutcome.OrphanOutboxFailureOutcome.MaxAttempts decl=session_orphan_outbox.go:258; exported=True; occurrences=2; use=run_queue_orphan_drain.go:194
  field OrphanOutboxFailureOutcome.OrphanOutboxFailureOutcome.Quarantined decl=session_orphan_outbox.go:262; exported=True; occurrences=1; use=run_queue_orphan_drain.go:188
  field RunQueueEntry.RunQueueEntry.Attempts decl=session_runqueue.go:29; exported=True; occurrences=1; use=run_queue_drain_session.go:258
  field RunQueueEntry.RunQueueEntry.ID decl=session_runqueue.go:22; exported=True; occurrences=15; use=run_queue_drain_session.go:260
  field RunQueueEntry.RunQueueEntry.Status decl=session_runqueue.go:25; exported=True; occurrences=1; use=run_queue_drain_session.go:583
  field RunQueueEntry.RunQueueEntry.TerminalFailure decl=session_runqueue.go:31; exported=True; occurrences=1; use=run_queue_drain_session.go:258
  method Service.Service.DrainOrphanOutboxEntry decl=session.go:409; exported=True; occurrences=1; use=run_queue_orphan_drain.go:93
  method Service.Service.GetRunQueueEntry decl=session.go:377; exported=True; occurrences=1; use=run_queue_drain_session.go:565
  method Service.Service.HasOutstandingRunQueueEntriesForSession decl=session.go:391; exported=True; occurrences=1; use=run_queue_drain_session.go:731
  method Service.Service.LeaseRunQueueEntry decl=session.go:347; exported=True; occurrences=1; use=run_queue_drain_session.go:215
  method Service.Service.ListPendingOrphanOutboxEntries decl=session.go:403; exported=True; occurrences=1; use=run_queue_orphan_drain.go:25
  method Service.Service.NackRunQueueEntryNoAttemptPenalty decl=session.go:365; exported=True; occurrences=2; use=run_queue_drain_session.go:302
  method Service.Service.RecordOrphanOutboxFailure decl=session.go:414; exported=True; occurrences=1; use=run_queue_orphan_drain.go:179
  method Service.Service.TerminalFailRunQueueEntry decl=session.go:366; exported=True; occurrences=1; use=run_queue_drain_session.go:260
EDGE 21: internal/session/run/sessionrun -> internal/session/proc/sessionlock; uses=1; witnesses=1
  type SessionLockBusyError decl=lock.go:256; exported=True; occurrences=1; use=run_queue_entry_exec.go:769
EDGE 22: internal/session/run/sessionrun -> internal/session/run/sessiondrain; uses=3; witnesses=2
  method RunQueuePump.RunQueuePump.drainOrphanOutbox decl=run_queue_orphan_drain.go:19; exported=False; occurrences=2; use=run_queue_lifecycle.go:137
  func WithRunQueueRowID decl=run_queue_row_context.go:16; exported=True; occurrences=1; use=run_queue_entry_exec.go:587
EDGE 23: internal/session/run/sessionrun -> internal/session/service; uses=114; witnesses=22
  func IsSessionCredentialRequirementCallData decl=session_credential_requirement.go:76; exported=True; occurrences=1; use=run_queue_entry_exec.go:199
  func IsSessionExecutionRequirementCallData decl=session_execution_requirement.go:164; exported=True; occurrences=1; use=run_queue_entry_exec.go:199
  type RunQueueEntry decl=session_runqueue.go:21; exported=True; occurrences=3; use=run_queue_entry_dispatch.go:26
  field RunQueueEntry.RunQueueEntry.Attempts decl=session_runqueue.go:29; exported=True; occurrences=3; use=run_queue_entry_dispatch.go:118
  field RunQueueEntry.RunQueueEntry.CallData decl=session_runqueue.go:24; exported=True; occurrences=3; use=run_queue_entry_exec.go:199
  field RunQueueEntry.RunQueueEntry.ID decl=session_runqueue.go:22; exported=True; occurrences=49; use=run_queue_entry_dispatch.go:41
  field RunQueueEntry.RunQueueEntry.LeaseExpiresAt decl=session_runqueue.go:28; exported=True; occurrences=1; use=run_queue_entry_exec.go:391
  field RunQueueEntry.RunQueueEntry.SessionID decl=session_runqueue.go:23; exported=True; occurrences=30; use=run_queue_entry_dispatch.go:39
  field RunQueueEntry.RunQueueEntry.TerminalFailure decl=session_runqueue.go:31; exported=True; occurrences=1; use=run_queue_entry_dispatch.go:118
  type Service decl=session.go:157; exported=True; occurrences=1; use=run_queue_pump.go:326
  method Service.Service.AckRunQueueEntry decl=session.go:363; exported=True; occurrences=1; use=run_queue_entry_exec.go:674
  method Service.Service.CleanupExpiredLeases decl=session.go:369; exported=True; occurrences=1; use=run_queue_lifecycle.go:174
  method Service.Service.LeaseRunQueueEntry decl=session.go:347; exported=True; occurrences=2; use=run_queue_entry_dispatch.go:121
  method Service.Service.ListPendingRunQueueEntries decl=session.go:367; exported=True; occurrences=1; use=run_queue_lifecycle.go:198
  method Service.Service.NackRunQueueEntry decl=session.go:364; exported=True; occurrences=1; use=run_queue_entry_exec.go:780
  method Service.Service.NackRunQueueEntryNoAttemptPenalty decl=session.go:365; exported=True; occurrences=5; use=run_queue_entry_dispatch.go:142
  method Service.Service.RenewRunQueueLease decl=session.go:354; exported=True; occurrences=1; use=run_queue_entry_exec.go:516
  method Service.Service.TerminalFailRunQueueEntry decl=session.go:366; exported=True; occurrences=4; use=run_queue_entry_dispatch.go:162
  type SessionAgentCallData decl=session_runqueue.go:190; exported=True; occurrences=2; use=run_queue_entry_exec.go:198
  func ceilUnixSeconds decl=session_runqueue.go:335; exported=False; occurrences=1; use=run_queue_entry_exec.go:514
  func recordFinalizationQueueFailure decl=finalization_queue_failure.go:12; exported=False; occurrences=1; use=run_queue_entry_exec.go:635
  func withFinalizationQueueLease decl=finalization_queue_lease.go:58; exported=False; occurrences=1; use=run_queue_entry_exec.go:587
EDGE 24: internal/session/service -> internal/session/batch/finstore; uses=140; witnesses=14
  type AgentFinalizationIdentity decl=agent_finalization.go:15; exported=True; occurrences=7; use=batch_commit_admission.go:17
  field AgentFinalizationIdentity.AgentFinalizationIdentity.Generation decl=agent_finalization.go:17; exported=True; occurrences=4; use=batch_finalization_tx.go:70
  field AgentFinalizationIdentity.AgentFinalizationIdentity.LogicalCallID decl=agent_finalization.go:16; exported=True; occurrences=6; use=batch_finalization_tx.go:70
  field AgentFinalizationIdentity.AgentFinalizationIdentity.SessionID decl=agent_finalization.go:16; exported=True; occurrences=23; use=batch_commit_admission.go:66
  method AgentFinalizationIdentity.AgentFinalizationIdentity.valid decl=agent_finalization.go:22; exported=False; occurrences=7; use=batch_commit_admission.go:63
  field AgentFinalizationRecord.AgentFinalizationRecord.Cancelled decl=agent_finalization.go:35; exported=True; occurrences=4; use=batch_finalization_tx.go:59
  method AgentFinalizationRecord.AgentFinalizationRecord.active decl=agent_finalization.go:38; exported=False; occurrences=4; use=batch_finalization_tx.go:62
  method AgentFinalizationStore.AgentFinalizationStore.Admit decl=agent_finalization.go:101; exported=True; occurrences=1; use=batch_commit_admission.go:105
  method AgentFinalizationStore.AgentFinalizationStore.Bind decl=agent_finalization.go:50; exported=True; occurrences=1; use=finalization_authority.go:28
  method AgentFinalizationStore.AgentFinalizationStore.Release decl=agent_finalization.go:165; exported=True; occurrences=1; use=batch_commit_admission.go:141
  method AgentFinalizationStore.AgentFinalizationStore.Snapshot decl=agent_finalization.go:95; exported=True; occurrences=1; use=finalization_authority.go:43
  var ErrAgentFinalizationChanged decl=agent_finalization.go:10; exported=True; occurrences=74; use=batch_finalization_tx.go:15
  func NewAgentFinalizationStore decl=agent_finalization.go:46; exported=True; occurrences=4; use=batch_commit_admission.go:104
  func readAgentFinalization decl=agent_finalization.go:69; exported=False; occurrences=3; use=batch_finalization_tx.go:55
EDGE 25: internal/session/service -> internal/session/batch/sessionbatch; uses=66; witnesses=42
  type BatchAddition decl=batch_store.go:221; exported=True; occurrences=1; use=batch_commit.go:309
  field BatchAddition.BatchAddition.Node decl=batch_store.go:224; exported=True; occurrences=1; use=batch_commit.go:309
  field BatchAddition.BatchAddition.Ordinal decl=batch_store.go:223; exported=True; occurrences=1; use=batch_commit.go:309
  field BatchAddition.BatchAddition.Parent decl=batch_store.go:222; exported=True; occurrences=1; use=batch_commit.go:309
  type BatchProjection decl=batch_store.go:226; exported=True; occurrences=1; use=batch_commit.go:358
  field BatchProjection.BatchProjection.Checkpoint decl=batch_store.go:232; exported=True; occurrences=1; use=batch_commit.go:359
  field BatchProjection.BatchProjection.Host decl=batch_store.go:227; exported=True; occurrences=1; use=batch_commit.go:358
  field BatchProjection.BatchProjection.Roots decl=batch_store.go:229; exported=True; occurrences=2; use=batch_commit.go:373
  field BatchProjection.BatchProjection.RunnerID decl=batch_store.go:227; exported=True; occurrences=1; use=batch_commit.go:358
  field BatchProjection.BatchProjection.Sequence decl=batch_store.go:228; exported=True; occurrences=1; use=batch_commit.go:358
  field BatchProjection.BatchProjection.finalizationAdmission decl=batch_store.go:234; exported=False; occurrences=1; use=batch_commit.go:382
  type BatchReservation decl=batch_store.go:87; exported=True; occurrences=1; use=batch_commit.go:145
  field BatchReservation.BatchReservation.Checkpoint decl=batch_store.go:95; exported=True; occurrences=1; use=batch_commit.go:151
  field BatchReservation.BatchReservation.Host decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:147
  field BatchReservation.BatchReservation.InputHash decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:148
  field BatchReservation.BatchReservation.Notices decl=batch_store.go:93; exported=True; occurrences=1; use=batch_commit.go:150
  field BatchReservation.BatchReservation.OriginCLI decl=batch_store.go:90; exported=True; occurrences=1; use=batch_commit.go:149
  field BatchReservation.BatchReservation.Owner decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:147
  field BatchReservation.BatchReservation.Root decl=batch_store.go:91; exported=True; occurrences=1; use=batch_commit.go:150
  field BatchReservation.BatchReservation.RootID decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:147
  field BatchReservation.BatchReservation.RunnerID decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:147
  field BatchReservation.BatchReservation.Sequence decl=batch_store.go:89; exported=True; occurrences=1; use=batch_commit.go:150
  field BatchReservation.BatchReservation.ToolCallID decl=batch_store.go:88; exported=True; occurrences=1; use=batch_commit.go:148
  field BatchReservation.BatchReservation.finalizationAdmission decl=batch_store.go:97; exported=False; occurrences=1; use=batch_commit.go:146
  type BatchReservationNode decl=batch_store.go:78; exported=True; occurrences=5; use=batch_commit.go:179
  field BatchReservationNode.BatchReservationNode.Children decl=batch_store.go:80; exported=True; occurrences=6; use=batch_commit.go:203
  field BatchReservationNode.BatchReservationNode.Values decl=batch_store.go:79; exported=True; occurrences=3; use=batch_commit.go:214
  field BatchReservationResult.BatchReservationResult.Snapshot decl=batch_store.go:106; exported=True; occurrences=1; use=batch_commit.go:160
  type BatchRootProjection decl=batch_store.go:236; exported=True; occurrences=3; use=batch_commit.go:274
  field BatchRootProjection.BatchRootProjection.Additions decl=batch_store.go:239; exported=True; occurrences=2; use=batch_commit.go:309
  field BatchRootProjection.BatchRootProjection.Notices decl=batch_store.go:240; exported=True; occurrences=2; use=batch_commit.go:356
  field BatchRootProjection.BatchRootProjection.Owner decl=batch_store.go:237; exported=True; occurrences=1; use=batch_commit.go:371
  field BatchRootProjection.BatchRootProjection.Root decl=batch_store.go:237; exported=True; occurrences=1; use=batch_commit.go:370
  field BatchRootProjection.BatchRootProjection.Updates decl=batch_store.go:238; exported=True; occurrences=4; use=batch_commit.go:325
  field BatchSnapshot.BatchSnapshot.Root decl=batch_store.go:102; exported=True; occurrences=1; use=batch_commit.go:160
  type BatchStore decl=batch_store.go:43; exported=True; occurrences=4; use=batch_commit.go:59
  method BatchStore.BatchStore.Project decl=batch_store.go:256; exported=True; occurrences=1; use=batch_commit.go:383
  method BatchStore.BatchStore.ReserveIdentity decl=batch_store.go:128; exported=True; occurrences=1; use=batch_commit.go:145
  method BatchStore.BatchStore.RootOwner decl=batch_store.go:404; exported=True; occurrences=1; use=batch_commit.go:366
  field BatchStore.BatchStore.writer decl=batch_store.go:44; exported=False; occurrences=2; use=batch_commit.go:138
  var ErrBatchProjectionConflict decl=batch_store.go:17; exported=True; occurrences=1; use=batch_commit.go:161
  func batchRequireRow decl=batch_store.go:244; exported=False; occurrences=2; use=finalization_queue_lease.go:48
EDGE 26: internal/session/service -> internal/session/run/sessionrun; uses=1; witnesses=1
  var ErrCallQueuedNotExecuted decl=run_queue_pump.go:124; exported=True; occurrences=1; use=finalization_queue_failure.go:21
EDGE 27: internal/session/state/sessionfacts -> internal/session/proc/sessionlock; uses=3; witnesses=3
  func SessionLockPath decl=lock_inspect.go:45; exported=True; occurrences=1; use=session_lock_fact.go:33
  func pidSidecarPath decl=lock_inspect.go:199; exported=False; occurrences=1; use=session_lock_fact.go:61
  func sanitiseSessionID decl=lock_inspect.go:16; exported=False; occurrences=1; use=session_lock_fact.go:26
EDGE 28: internal/session/state/sessionfacts -> internal/session/state/sessionhosts; uses=5; witnesses=2
  type SessionDriver decl=session_driver.go:54; exported=True; occurrences=1; use=session_activity.go:107
  field SessionDriver.SessionDriver.PID decl=session_driver.go:57; exported=True; occurrences=4; use=session_activity.go:193
EDGE 29: internal/session/state/sessionfacts -> internal/session/state/sessionnotice; uses=3; witnesses=3
  type ChildQuestion decl=session_child_question.go:28; exported=True; occurrences=1; use=session_activity.go:131
  field ChildQuestion.ChildQuestion.ChildSessionID decl=session_child_question.go:30; exported=True; occurrences=1; use=session_activity.go:313
  field ChildQuestion.ChildQuestion.Question decl=session_child_question.go:32; exported=True; occurrences=1; use=session_activity.go:313
EDGE 30: internal/session/state/sessionfacts -> internal/session/state/sessionwake; uses=3; witnesses=3
  type OpenWakeSchedule decl=wake_schedule_store.go:349; exported=True; occurrences=1; use=session_activity.go:124
  field OpenWakeSchedule.OpenWakeSchedule.ID decl=wake_schedule_store.go:350; exported=True; occurrences=1; use=session_activity.go:329
  field OpenWakeSchedule.OpenWakeSchedule.NextRunAt decl=wake_schedule_store.go:352; exported=True; occurrences=1; use=session_activity.go:329
EDGE 31: internal/session/state/sessionhosts -> internal/session/proc/sessionlock; uses=14; witnesses=6
  alias FileLock decl=file_lock_bridge.go:12; exported=True; occurrences=8; use=host_lock.go:98
  func classifyAndLock decl=file_lock_bridge.go:22; exported=False; occurrences=1; use=host_lock.go:313
  func isContentionError decl=file_lock_bridge.go:26; exported=False; occurrences=2; use=host_lock.go:361
  func isLockContentionError decl=lock_unix.go:44; exported=False; occurrences=1; use=host_lock.go:191
  func openLockFile decl=file_lock_bridge.go:20; exported=False; occurrences=1; use=host_lock.go:306
  func tryLockFile decl=lock_unix.go:19; exported=False; occurrences=1; use=host_lock.go:189
EDGE 32: internal/session/state/sessionnotice -> internal/session/batch/finstore; uses=2; witnesses=1
  var ErrAgentFinalizationChanged decl=agent_finalization.go:10; exported=True; occurrences=2; use=session_notice_store.go:153
EDGE 33: internal/session/state/sessionnotice -> internal/session/batch/sessionbatch; uses=10; witnesses=5
  type BatchPendingDebt decl=batch_reaction.go:29; exported=True; occurrences=4; use=session_notice_store.go:381
  field BatchPendingDebt.BatchPendingDebt.Nodes decl=batch_reaction.go:30; exported=True; occurrences=2; use=session_notice_store.go:395
  field BatchPendingDebt.BatchPendingDebt.Notices decl=batch_reaction.go:31; exported=True; occurrences=2; use=session_notice_store.go:398
  field batchDebtNodeRow.batchDebtNodeRow.ref decl=batch_reaction.go:49; exported=False; occurrences=1; use=session_notice_store.go:395
  func batchOutboxDebtNodesVoidAware decl=batch_reaction.go:65; exported=False; occurrences=1; use=session_notice_store.go:385
EDGE 34: internal/session/state/sessionnotice -> internal/session/service; uses=9; witnesses=5
  func admitFinalizationMutation decl=finalization_mutation.go:13; exported=False; occurrences=2; use=session_notice_store.go:189
  func batchCommitReleaseError decl=batch_commit_admission.go:169; exported=False; occurrences=2; use=session_notice_store.go:191
  type batchFinalizationAdmission decl=batch_commit_admission.go:21; exported=False; occurrences=1; use=session_notice_store.go:169
  func finalizationScoped decl=finalization_ingress_helpers.go:277; exported=False; occurrences=1; use=session_notice_store.go:168
  func validateBatchFinalizationTx decl=batch_finalization_tx.go:12; exported=False; occurrences=3; use=session_notice_store.go:243
EDGE 35: internal/session/state/sessionwake -> internal/session/service; uses=1; witnesses=1
  func homeFlag decl=session_workspace.go:114; exported=False; occurrences=1; use=wake_schedule_store.go:98

### 8.2 Every currently unexported cross-owner symbol

This is the complete Linux `must_export` witness set (16 boundaries, 78 symbols). “Required boundary” means the current layout references a private declaration from another proposed owner; relocation or an appropriate port can resolve it without exporting the identifier.

BOUNDARY 1: internal/session/batch/finfacts -> internal/session/service; unexported=1
  const finalizationAncestorLimit decl=finalization_ancestors.go:12; occurrences=3; use=finalization_facts_descendants.go:20
BOUNDARY 2: internal/session/batch/finstore -> internal/session/batch/finterm; unexported=1
  method AgentFinalizationCandidate.AgentFinalizationCandidate.valid decl=finalization_candidate.go:26; occurrences=1; use=agent_finalization.go:200
BOUNDARY 3: internal/session/batch/finterm -> internal/session/batch/finstore; unexported=4
  method AgentFinalizationIdentity.AgentFinalizationIdentity.valid decl=agent_finalization.go:22; occurrences=1; use=finalization_seal.go:130
  method AgentFinalizationRecord.AgentFinalizationRecord.active decl=agent_finalization.go:38; occurrences=3; use=finalization_candidate.go:50
  field AgentFinalizationStore.AgentFinalizationStore.conn decl=agent_finalization.go:44; occurrences=8; use=finalization_candidate.go:132
  func readAgentFinalization decl=agent_finalization.go:69; occurrences=3; use=finalization_candidate.go:72
BOUNDARY 4: internal/session/batch/sessionbatch -> internal/session/batch/sessionbatchread; unexported=7
  method BatchLiveCounts.BatchLiveCounts.add decl=batch_read.go:48; occurrences=1; use=batch_history_snapshot.go:78
  method BatchLiveReader.BatchLiveReader.batchLevelRows decl=batch_read_level.go:42; occurrences=1; use=batch_history_snapshot.go:60
  method BatchLiveReader.BatchLiveReader.hostStatus decl=batch_read.go:107; occurrences=1; use=batch_history_snapshot.go:108
  func batchLiveFact decl=batch_read.go:95; occurrences=1; use=batch_history_snapshot.go:108
  const batchReadChunk decl=batch_read_live.go:15; occurrences=4; use=batch_reaction.go:135
  func batchReadTerminal decl=batch_read.go:99; occurrences=1; use=batch_history_reset.go:57
  var errBatchReadUnavailable decl=batch_read.go:127; occurrences=2; use=batch_history_snapshot.go:54
BOUNDARY 5: internal/session/batch/sessionbatch -> internal/session/service; unexported=8
  func admitFinalizationMutation decl=finalization_mutation.go:13; occurrences=5; use=batch_child_bind.go:64
  const batchCommitInt63 decl=batch_commit.go:77; occurrences=1; use=batch_child_bind.go:61
  func batchCommitReleaseError decl=batch_commit_admission.go:169; occurrences=5; use=batch_child_bind.go:66
  func batchCommitSpecHash decl=batch_commit.go:166; occurrences=1; use=batch_adapters.go:164
  type batchFinalizationAdmission decl=batch_commit_admission.go:21; occurrences=3; use=batch_pull.go:189
  func finalizationFenceError decl=finalization_ingress_helpers.go:268; occurrences=6; use=batch_question.go:38
  func finalizationScoped decl=finalization_ingress_helpers.go:277; occurrences=1; use=batch_pull.go:188
  func validateBatchFinalizationTx decl=batch_finalization_tx.go:12; occurrences=11; use=batch_child_bind.go:135
BOUNDARY 6: internal/session/proc/sessioncgroup -> internal/session/proc/sessionlock; unexported=2
  func isErrno decl=errno_unix.go:11; occurrences=3; use=childgroup_registry_unix.go:716
  func sanitiseSessionID decl=lock_inspect.go:16; occurrences=1; use=childgroup_registry_unix.go:125
BOUNDARY 7: internal/session/run/sessiondrain -> internal/session/run/sessionrun; unexported=29
  field RunQueuePump.RunQueuePump.admitMu decl=run_queue_pump.go:549; occurrences=2; use=run_queue_drain_session.go:328
  method RunQueuePump.RunQueuePump.admitSession decl=run_queue_admission.go:347; occurrences=1; use=run_queue_drain_session.go:75
  field RunQueuePump.RunQueuePump.cfg decl=run_queue_pump.go:511; occurrences=30; use=run_queue_drain_session.go:105
  field RunQueuePump.RunQueuePump.ctx decl=run_queue_pump.go:512; occurrences=4; use=run_queue_orphan_drain.go:21
  method RunQueuePump.RunQueuePump.dbWriteTimeout decl=run_queue_pump.go:482; occurrences=5; use=run_queue_drain_session.go:259
  field RunQueuePump.RunQueuePump.execSem decl=run_queue_pump.go:647; occurrences=3; use=run_queue_drain_session.go:286
  method RunQueuePump.RunQueuePump.executeEntrySync decl=run_queue_entry_exec.go:94; occurrences=1; use=run_queue_drain_session.go:385
  method RunQueuePump.RunQueuePump.leaseTTL decl=run_queue_pump.go:463; occurrences=1; use=run_queue_drain_session.go:215
  field RunQueuePump.RunQueuePump.stopping decl=run_queue_pump.go:550; occurrences=1; use=run_queue_drain_session.go:329
  field RunQueuePump.RunQueuePump.workerWg decl=run_queue_pump.go:523; occurrences=2; use=run_queue_drain_session.go:331
  field admissionEntry.admissionEntry.done decl=run_queue_admission.go:33; occurrences=1; use=run_queue_drain_session.go:122
  method admissionEntry.admissionEntry.observeAssistantIDs decl=run_queue_admission.go:78; occurrences=1; use=run_queue_drain_session.go:77
  field admissionEntry.admissionEntry.outcome decl=run_queue_admission.go:46; occurrences=3; use=run_queue_drain_session.go:133
  type admissionOutcome decl=run_queue_admission.go:161; occurrences=1; use=run_queue_drain_classify.go:97
  field admissionOutcome.admissionOutcome.assistantIDs decl=run_queue_admission.go:180; occurrences=1; use=run_queue_drain_session.go:136
  field admissionOutcome.admissionOutcome.err decl=run_queue_admission.go:177; occurrences=4; use=run_queue_drain_classify.go:149
  field admissionOutcome.admissionOutcome.kind decl=run_queue_admission.go:168; occurrences=1; use=run_queue_drain_classify.go:98
  field admissionOutcome.admissionOutcome.rowID decl=run_queue_admission.go:165; occurrences=4; use=run_queue_drain_classify.go:151
  field admissionOutcome.admissionOutcome.terminalAssistantID decl=run_queue_admission.go:183; occurrences=1; use=run_queue_drain_session.go:136
  func busyOutcome decl=run_queue_admission.go:197; occurrences=1; use=run_queue_drain_session.go:336
  var errLeaseLost decl=run_queue_pump.go:175; occurrences=2; use=run_queue_drain_classify.go:28
  func executedOutcome decl=run_queue_admission.go:203; occurrences=3; use=run_queue_drain_session.go:380
  func noRowTouched decl=run_queue_admission.go:191; occurrences=3; use=run_queue_drain_session.go:231
  const outcomeBusy decl=run_queue_admission.go:149; occurrences=1; use=run_queue_drain_classify.go:127
  const outcomeNoRowTouched decl=run_queue_admission.go:122; occurrences=1; use=run_queue_drain_classify.go:99
  const outcomeTerminalDeleted decl=run_queue_admission.go:142; occurrences=1; use=run_queue_drain_classify.go:136
  func recordDrainAssistantIdentity decl=run_queue_admission.go:230; occurrences=2; use=run_queue_drain_session.go:136
  func recordDrainAssistantIdentityLive decl=run_queue_admission.go:270; occurrences=1; use=run_queue_drain_session.go:78
  func terminalDeletedOutcome decl=run_queue_admission.go:281; occurrences=1; use=run_queue_drain_session.go:281
BOUNDARY 8: internal/session/run/sessionrun -> internal/session/run/sessiondrain; unexported=1
  method RunQueuePump.RunQueuePump.drainOrphanOutbox decl=run_queue_orphan_drain.go:19; occurrences=2; use=run_queue_lifecycle.go:137
BOUNDARY 9: internal/session/run/sessionrun -> internal/session/service; unexported=3
  func ceilUnixSeconds decl=session_runqueue.go:335; occurrences=1; use=run_queue_entry_exec.go:514
  func recordFinalizationQueueFailure decl=finalization_queue_failure.go:12; occurrences=1; use=run_queue_entry_exec.go:635
  func withFinalizationQueueLease decl=finalization_queue_lease.go:58; occurrences=1; use=run_queue_entry_exec.go:587
BOUNDARY 10: internal/session/service -> internal/session/batch/finstore; unexported=3
  method AgentFinalizationIdentity.AgentFinalizationIdentity.valid decl=agent_finalization.go:22; occurrences=7; use=batch_commit_admission.go:63
  method AgentFinalizationRecord.AgentFinalizationRecord.active decl=agent_finalization.go:38; occurrences=4; use=batch_finalization_tx.go:62
  func readAgentFinalization decl=agent_finalization.go:69; occurrences=3; use=batch_finalization_tx.go:55
BOUNDARY 11: internal/session/service -> internal/session/batch/sessionbatch; unexported=4
  field BatchProjection.BatchProjection.finalizationAdmission decl=batch_store.go:234; occurrences=1; use=batch_commit.go:382
  field BatchReservation.BatchReservation.finalizationAdmission decl=batch_store.go:97; occurrences=1; use=batch_commit.go:146
  field BatchStore.BatchStore.writer decl=batch_store.go:44; occurrences=2; use=batch_commit.go:138
  func batchRequireRow decl=batch_store.go:244; occurrences=2; use=finalization_queue_lease.go:48
BOUNDARY 12: internal/session/state/sessionfacts -> internal/session/proc/sessionlock; unexported=2
  func pidSidecarPath decl=lock_inspect.go:199; occurrences=1; use=session_lock_fact.go:61
  func sanitiseSessionID decl=lock_inspect.go:16; occurrences=1; use=session_lock_fact.go:26
BOUNDARY 13: internal/session/state/sessionhosts -> internal/session/proc/sessionlock; unexported=5
  func classifyAndLock decl=file_lock_bridge.go:22; occurrences=1; use=host_lock.go:313
  func isContentionError decl=file_lock_bridge.go:26; occurrences=2; use=host_lock.go:361
  func isLockContentionError decl=lock_unix.go:44; occurrences=1; use=host_lock.go:191
  func openLockFile decl=file_lock_bridge.go:20; occurrences=1; use=host_lock.go:306
  func tryLockFile decl=lock_unix.go:19; occurrences=1; use=host_lock.go:189
BOUNDARY 14: internal/session/state/sessionnotice -> internal/session/batch/sessionbatch; unexported=2
  field batchDebtNodeRow.batchDebtNodeRow.ref decl=batch_reaction.go:49; occurrences=1; use=session_notice_store.go:395
  func batchOutboxDebtNodesVoidAware decl=batch_reaction.go:65; occurrences=1; use=session_notice_store.go:385
BOUNDARY 15: internal/session/state/sessionnotice -> internal/session/service; unexported=5
  func admitFinalizationMutation decl=finalization_mutation.go:13; occurrences=2; use=session_notice_store.go:189
  func batchCommitReleaseError decl=batch_commit_admission.go:169; occurrences=2; use=session_notice_store.go:191
  type batchFinalizationAdmission decl=batch_commit_admission.go:21; occurrences=1; use=session_notice_store.go:169
  func finalizationScoped decl=finalization_ingress_helpers.go:277; occurrences=1; use=session_notice_store.go:168
  func validateBatchFinalizationTx decl=batch_finalization_tx.go:12; occurrences=3; use=session_notice_store.go:243
BOUNDARY 16: internal/session/state/sessionwake -> internal/session/service; unexported=1
  func homeFlag decl=session_workspace.go:114; occurrences=1; use=wake_schedule_store.go:98

## 9. Phase-2 handoff

The evidence supports a **design stop**, not a source-change lane: revise the D1 map/schedule and its port/receiver tables to account for G1–G18; build a revised TSV without changing this phase’s snapshot; then rerun the symbol graph before MV1 under Rule 19. This operator instruction says to stop at the report; no design files, master plan, TSV, or code were changed by this phase.

## Amended design (2026-11-16)

The staged decisions D1–D7 were encoded in a new amended layout and measured. Original `2026-11-16-sprint0-session-layout.tsv` was left untouched.

### A.1 Exact checker invocations and exit codes

```text
D:/dev/go/crush/.rush/tmp/sprint0/symgraph/symgraph.exe -dir D:/dev/go/crush/worktrees/s0-w5-sm1-20261115/internal/session -layout D:/dev/go/crush/worktrees/s0-w5-sm1-20261115/docs/plans/2026-11-16-sprint0-session-layout-amended.tsv -root-owner internal/session -max-elements 7 -json
```
run once with `GOOS=linux GOARCH=amd64` and once with `GOOS=windows GOARCH=amd64`. Final (converged) runs: linux exit 0, windows exit 0, empty stderr (0 bytes). Raw outputs: `D:/dev/go/crush/.rush/tmp/sprint0/symgraph/session-sg0-amended-20261116/amended-linux.json`, `amended-windows.json` (+ `.stderr`, plus `layout-alt-commit-in-service.tsv` and `alt-linux.json` for the D6 alternative, and `original-layout-copy.tsv`).

An intermediate configuration was also measured and rejected: both `finalization_queue_failure.go`/`finalization_queue_lease.go` in `run/runadmission` and `role_contract.go` in `sesstypes` produced one 6-owner SCC ({sessionbatch, sessionbatchread, runadmission, sessionrun, service, sesstypes}), 35 edges, and 1 receiver violation (`service.bindFinalizationQueueLeaseTx`, receiver `session.go:417`). The converged layout reverts those three files to `service`.

The D6 alternative (`batch_commit.go`/`batch_commit_doc.go` kept in `service`) was measured (linux exit 0): identical SCC shape — same 3 owners, 28 edges, 0 receiver violations. D6 as amended (early MV3 arrival) is SCC-neutral and was kept.

### A.2 Amended TSV

`docs/plans/2026-11-16-sprint0-session-layout-amended.tsv`: 284 rows — 123 non-test / 161 test; SHA-256 `933ea73b899b954399cf595d87698de318fde6e40468b7a9f2215d0501b9b34b` (original remains `e57703110e88d60d53d33d63f767b0d8fe1ae19765024cfa5260b6a6d1f90dea`). Deltas vs original: finstore dissolved (agent_finalization pair → finterm); notify/notify_gate (+test) staged in service; 5 sessiondrain non-test files + 13 drain tests → sessionrun; 4 notice/type files + `child_question_format_test.go` → new `internal/session/sesstypes`; batch_commit/batch_commit_doc → sessionbatch.

### A.3 Measured result per GOOS (converged layout)

| GOOS | Exit | Edges | SCCs | SCC members | Internal SCC edges | Receiver violations | Over-7 owners |
|---|---:|---:|---:|---|---:|---:|---:|
| linux | 0 | 28 | 1 | service, run/sessionrun, batch/sessionbatch | 4 | 0 | 4 |
| windows | 0 | 27 | 1 | same | 4 (identical witnesses) | 0 | 4 |

Original run for comparison: 35 edges, 1 SCC with 8 owners and 19 internal edges, 9 receiver violations.

The residual SCC (hard target 0 not reachable at file granularity) has exactly four internal directed edges; each dissolves only after the named declaration decision:

1. `service → run/sessionrun` (1 use): `ErrCallQueuedNotExecuted` decl `run_queue_pump.go:124`, use `finalization_queue_failure.go:21`. **Decision: EX1 moves the sentinel to sesstypes/types_runcall with an immutable alias in sessionrun.**
2. `run/sessionrun → service` (147 uses / 32 witnesses): `Service` interface methods (`session.go:157..417`), `RunQueueEntry`/`SessionAgentCallData`/`OrphanOutboxFailureOutcome` (`session_runqueue.go`, `session_orphan_outbox.go`), `ceilUnixSeconds` (`session_runqueue.go:335`), `recordFinalizationQueueFailure` (`finalization_queue_failure.go:12`), `withFinalizationQueueLease` (`finalization_queue_lease.go:58`). **Decisions: EX1 moves the run wire types (RunQueueEntry, SessionAgentCallData, ModelCfg) to sesstypes; EX3 moves the queue stores; EX7 introduces the run consumer ports; EX4 moves failure/lease to finqueue.** Not dissolvable by file moves alone.
3. `service → batch/sessionbatch` (47 uses / 7 witnesses): `BatchCommitIdentity` + methods (`batch_commit.go:25,43,47,49,50`), `BatchDeliveryEnvelope` (`batch_delivery_store.go:14`), `batchRequireRow` (`batch_store.go:244`). **Decisions: S2 signature changes (writer/identity as parameters) + EX1 batch wire types to sesstypes; batchRequireRow call site moves with the lease file at EX4.**
4. `batch/sessionbatch → service` (39 uses / 10 witnesses): `admitBatchCommitOwners` (`batch_commit_admission.go:27`), `admitFinalizationMutation` (`finalization_mutation.go:13`), `batchCommitReleaseError` (`batch_commit_admission.go:169`), `batchFinalizationAdmission` (`batch_commit_admission.go:21`), `batchFinalizationIdentityFromContext` (`finalization_context.go:104`), `finalizationFenceError`/`finalizationScoped` (`finalization_ingress_helpers.go:268,277`), `validateBatchFinalizationTx` (`batch_finalization_tx.go:12`), `AgentFinalizationAuthority` (`finalization_authority.go:18`), type `service` (`session.go:417`). **Decisions: EX4 converts authority/guard to sessguard (`finalization_authority.go` pinned today by the `hasFinalizationScope *service` receiver); after EX1/S2 the admission/context/tx/ingress files join sessionbatch/sessguard per the MV3/EX4 schedule.** service and sessionbatch are NOT merged.

Everything else is acyclic at file granularity: notably `sessionbatch → sessionbatchread` (29/21, G3 read seam) is now one-way and outside the SCC; `sessionnotice → sessionbatch` is the documented legal interim edge (G13, removed by EX6); `sessionnotice → finterm` and `sessionbatch → finterm` are the documented legal one-way error edges (G12); `finterm → service` (Session) is staged for EX1.

### A.4 Receiver violations

**0 measured violations on the amended TSV (both GOOS)** — the checker's own receiver diagnostics now report an empty list (original: 9). Classification of the original 9: the 5 `AgentFinalizationStore` method splits resolved by D1 (same-package colocation in finterm); the 4 `RunQueuePump` drain/orphan splits resolved by D3 (files reunified with the pump in sessionrun); their final EX-era homes still require the EX4/EX7 receiver conversions named in §7 (authority→Guard: EX4; `bindFinalizationQueueLeaseTx` receiver conversion: EX4; run component conversions: EX7).

Receiver evidence source: the preserved original binary JSON (correct `receiver_type_line`) plus source greps (`^func (` per moved file, receiver type declarations). The installed shared `symgraph.exe` was not rebuilt or modified; an attempt to compile a private copy of the checker from `.rush/tmp/sprint0/symgraph/cmd/*.go` failed (the shared source tree does not compile: `projection.go:591` undefined `receiverOwner`/`receiverType`/`receiverFile`), so the shared binary (non-receiver fields corroborated by two GOOS runs) plus original JSON + source was used, as sanctioned. Notably the shared binary's receiver diagnostics on the amended layout agree with source: 0 violations, and the intermediate run's single violation matched the source-declared `*service` receiver exactly.

### A.5 Per-owner non-test counts vs the 7 rule (amended TSV)

| Owner | Non-test | Test | >7 |
|---|---:|---:|---|
| service | 34 | 44 | yes — ratchet core (FIN §2.2 schedule unchanged: 34→34→21→15→8→6→4) |
| batch/sessionbatch | 28 | 31 | yes — ratchet core (amended FIN: starts 28 with commit/doc early arrival; final 7 unchanged) |
| run/sessionrun | 12 | 42 | yes — ratchet core (D3; EX7 schedule) |
| proc/sessionlock | 8 | 6 | yes — pre-existing, unchanged by amendments (В7 split) |
| batch/finterm | 4 | 4 | no (final 6 after EX1) |
| sesstypes | 4 | 1 | no (staged; final 5 files per EX1) |
| batch/sessionbatchread | 5 | 5 | no |
| proc/sessioncgroup | 4 | 2 | no |
| state/sessionhosts | 6 | 7 | no |
| batch/findebt, finfacts, finholds | 3, 3, 5 | 3, 2, 7 | no |
| state/sessionnotice | 1 | 2 | no (4 files staged in sesstypes) |
| state/sessionwake, sessionfacts, mcpoutput | 3, 2, 1 | 3, 1, 1 | no |

Over-7 owners are exactly the ratchet cores service / sessionbatch / sessionrun (plus pre-existing sessionlock 8, untouched by this amendment) — no new owner exceeds 7.

### A.6 Must-export boundary deltas vs the original run

12 boundaries / 78 → 12 boundaries but fewer witnesses (172 vs 267 total; must_export records 12 vs the original 16-boundary/78-witness summary). Disappeared boundaries vs original: finstore↔finterm both directions (D1), finterm→sessionbatch (notify staged out; envelope decision deferred to EX6), finterm→service (Session; staged out), sessiondrain→sessionrun and sessionrun↔sessiondrain (D3), run/sessionrun→run/runadmission edges (D2 reverted), sesstypes→service (role_contract pinned; EX1 ModelCfg move named). Remaining must-export boundaries are the pre-existing named seams: finfacts→service (G15), sessionbatch→sessionbatchread (G3), sessionbatch→service + service→sessionbatch (G4/G10, SCC above), sessioncgroup→sessionlock, sessionrun→service (G8 + ports), service→finterm (G9: `AgentFinalizationIdentity.valid`, `AgentFinalizationRecord.active`, `readAgentFinalization` still need the EX4 sessguard→finterm port enumeration), sessionfacts/sessionhosts→sessionlock (G16/G17), sessionnotice→sessionbatch (G13 interim), sessionnotice→service (G14), sessionwake→service (G18).

### A.7 Lane-ordering constraints discovered

- SM3 (DrainSessionNow relocation) must precede ANY run package split (D3).
- EX1 precedes: the finterm notify/notify_gate file moves, the role_contract sesstypes move (ModelCfg), the failure/lease finqueue move (sentinel), and the run wire-type extraction.
- S2 precedes final MV3 if the residual service↔sessionbatch SCC must dissolve before EX6; EX4's Guard conversion precedes the authority file move.
- D2's `runadmission` staged holder is NOT viable before EX1+EX4 (measured 6-owner SCC + receiver violation); finqueue is the only final home, reached via EX1 then EX4.

### A.8 Updated verdict

**MV1 can start as amended.** The corrected literal placement now has 0 receiver violations, all cycle members reduced to the three ratchet cores with named declaration decisions (EX1/S2/EX4/EX7), and every original gap G1–G18 mapped to a decision, symbol, and lane (design §7). MV1 itself (34 service files) is untouched by the residual SCC: the service row is unchanged and its ratchet schedule already accounts for the exits.
