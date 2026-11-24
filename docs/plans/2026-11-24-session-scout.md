# Sprint 0 — Session Subsystem Directory-Element Scout (2026-11-24)

Task #105. Deliverable is a plan, not a code move. Source: four scouting
passes (one per package) done on this branch; every probe move was reverted
before reporting, and the tree was verified clean afterwards.

## Status of this plan (read first)

Three review rounds ended without a clean pass, so the plan is a scouting
baseline, not an executable schedule. What a later lane may rely on, and what
it must decide first:

- Verified by direct reading (usable as written): the measured baseline below;
  batch doc merges S1 (28 -> 24); the batch recovery, history and stop/control
  move sizes (24 -> 23 -> 21 -> 19, each -files +1 dir); run row context joining
  an existing child directory (8 -> 7); the clock and notice-path citations; the
  finding that no facade-to-sessionbatch cycle exists today
  (`session_notice_store.go:390` is a plain consumer edge).
- Arithmetic only, boundary NOT decided (each needs a discovery lane before a
  move lane): service S5-S7 (fork and agent-depth methods return the
  root-owned `Session` type; a forwarding method does not remove that type
  dependency, so child-owned boundary types or conversions without a reverse
  import must be specified) and run S10-S14 (the drain classifier takes the
  root-owned `admissionOutcome`).
- UNPROVEN, open question stated: batch piecemeal route: a child that needs
  the root `BatchStore` cannot reach its private `writer`/`reader` fields and
  cannot be called from the root without an import cycle; the lane must decide
  receiver-data ownership or a child-owned interface that does not import the
  root, and the combined single-child alternative creates a child with as many
  elements as it removes, so it is not a count reduction by itself. Lab:
  `Scenario` moves with all its methods, but replay (`replay_engine.go:33,48`)
  needs them before their new owner exists; the lane must choose between a
  shared dependency package both replay and the runner import, an interface
  injected into replay, or co-moving the dependent code; `cmd/batchlab`
  calls `Replay`, `LoadScenario` and `RunScenario` separately, so a
  no-facade endpoint retargets each call to its owner.
- The 'endpoint' numbers in the stage table (batch 7, lab 6 or 7, service 5 or
  6, run 6) are goals under those undecided boundaries, not established
  results; no probe in any pass reached a compiling end state.

## Measured baseline (guard counts, not task-quoted counts)

Counts below come from `.githooks/check_dir_entries.sh` with the recorded
ratchet in `.githooks/dir_entries_allowlist.txt`. The task description's
numbers were stale: it quoted 21 for the session batch package (guard/ratchet:
28) and 17 for the lab package (guard/ratchet: 24). The guard is authoritative. Its rule: a count above 7 stays allowed while it does not exceed its recorded ceiling; the allowlist line must be deleted in the same commit once the count reaches 7.

| Package | Guard count | Over | Composition |
|---|---|---|---|
| internal/session/service | 23 | +16 | 22 non-test files + 1 child dir (`components`) |
| internal/session/batch/sessionbatch | 28 | +21 | 28 non-test files, no child dir |
| internal/session/jobbatch/lab | 24 | +17 | 24 non-test files (3 build-tagged: `process_other.go`, `process_unix.go`, `process_windows.go`); testdata excluded |
| internal/session/run/sessionrun | 12 | +5 | 12 non-test files, no child dir |

Element lists are in each package section below.

## Build/vet protocol used for probes

All compiles in the scouting passes (and required of any later lane) ran only
through the shared machine-level go lock wrapper, never bare, no test runs:

```
capm 2g bash <checkout-independent: repo-external golock.sh> \
  env GOMEMLIMIT=700MiB go test -c -o NUL -gcflags=-e -ldflags='-s -w' ./<pkg>
capm 2g bash <golock.sh> env GOMEMLIMIT=700MiB go vet ./<pkg>
```

`golock.sh` is the shared wrapper created by earlier lanes under the main
checkout's `.rush/tmp/` (repo-external, machine-level; see
docs/plans/2026-11-24-session-ex4-ledger.tsv for the exact invocation rows).
Lab additionally needs a `GOOS=linux go build` probe whenever platform-tagged
files move.

## Cross-package fact that kills one "known blocker"

`internal/session/session_notice_store.go` (package `session`) imports
`sessionbatch` (line 34) and calls `sessionbatch.ReadBatchDebtNodeRefs` at
line 390. The service package has no `sessionbatch` import at all; the
reverse edge exists (`sessionbatch` imports `internal/session/service`:
batch_commit.go:13, batch_control.go:10, batch_adapters.go:11,
batch_delivery_store.go:8). OBSERVED: there is **no facade-to-sessionbatch
cycle**. The earlier-pass blocker dissolves once read as a dependency
direction question: `session` is a consumer of both packages, not a cycle
participant. `internal/session/session_bridge.go:62-159` holds
aliases/forwarders sourced from `sessionbatch` — facade debt, not a cycle.
No stage below depends on this blocker being fixed.

A second package-wide constraint for `service` and `sessionrun`: methods are
defined on an unexported receiver (`*service`, session.go:439; `*RunQueuePump`,
run_queue_pump.go). Methods cannot move to another package until the receiver
state they touch is reachable through an exported seam.

---

## Package 1: internal/session/service (23 elements)

Elements (22 files): agent_depth.go, agent_model_role.go,
finalization_compat.go, finalization_context.go,
finalization_ingress_helpers.go, finalization_notify.go,
finalization_notify_gate.go, role_contract_store.go,
root_role_contract_store.go, session.go, session_cost_subtree.go,
session_credential_requirement.go, session_execution_requirement.go,
session_fork.go, session_inject.go, session_lifecycle.go,
session_orphan_outbox.go, session_purge_queue.go, session_read.go,
session_runqueue.go, session_update.go, session_workspace.go; + `components`
dir (finqueue, sessguard, sesstypes already extracted).

### Clusters, coupling, receipts, net counts

Moving a cluster of k files into a NEW subdir of the already-counted
`components/` tree changes the root 23 -> 23-k (the new dir adds nothing at
the root; it lands inside the counted `components` element).

- **Finalization (5 files: finalization_compat.go, finalization_context.go,
  finalization_ingress_helpers.go, finalization_notify.go,
  finalization_notify_gate.go).** Coupling: private
  `finalizationIngressPayload` declared at finalization_ingress_helpers.go:13,
  used by finalization_ingress_helpers.go:59,104,203,230,252,
  finalization_compat.go:224,254, **and by non-finalization files**
  session_inject.go:384 and session_orphan_outbox.go:112. Private
  `mergeFinalizationAuthorities` (finalization_context.go:90, called :83,:138).
  Smallest de-coupling: export the ingress payload (or move it to sesstypes),
  then finalization moves; inject/outbox move later or consume the seam.
  Net: 23 -> 18. (No probe run for the complete move; coupling is read from
  source.)
- **Requirements (2 files: session_credential_requirement.go,
  session_execution_requirement.go).** Coupling (probed — attempted move to
  components/requirements, compile exit 1): `session_runqueue.go:250:19:
  undefined: SessionExecutionRequirement`; `session_fork.go:130:4: undefined:
  SessionCredentialMarkerRowID` (definition session_credential_requirement.go:31);
  `session_purge_queue.go:49:21: undefined: SessionExecutionRequirementRowID`
  (definition session_execution_requirement.go:62); plus `*service does not
  implement Service (missing method HasSessionCredentialRequirement)`. Smallest
  de-coupling: move the requirement types + deterministic row-ID helpers into an
  exported shared types package (sesstypes is the natural home) and give
  `*service` an adapter; runqueue, purge, fork consume the seam. Net: 23 -> 21.
- **Role contract (3 files: role_contract_store.go, root_role_contract_store.go,
  agent_model_role.go).** Coupling: private `decodeRoleContract`
  (role_contract_store.go:31) called from root_role_contract_store.go:34,75,82,107
  and agent_model_role.go:52; all methods on private `*service`. Smallest
  de-coupling: export the decoder (role-contract codec), adapter for the
  receiver. Net: 23 -> 20.
- **Runqueue/purge/outbox (3 files: session_runqueue.go, session_purge_queue.go,
  session_orphan_outbox.go).** Coupling: SessionExecutionRequirement
  (session_runqueue.go:250); requirement row-ID helpers (session_purge_queue.go:49);
  shares `finalizationIngressPayload` (session_orphan_outbox.go:112). Must follow
  the requirements seam and the finalization ingress export. Net: 23 -> 20
  (from the post-finalization count: 18 -> 15... see stage order; arithmetic is
  per-move: minus 3 files).
- **Fork/inject/workspace (3 files: session_fork.go, session_inject.go,
  session_workspace.go).** Coupling: credential row ID (session_fork.go:130,152);
  ingress payload (session_inject.go:384); lifecycle calls private
  `gitBranchFromWorkDir` (session_lifecycle.go:29 -> session_workspace.go:50).
  Net: minus 3 files, needs both seams above.
- **Cost (1 file: session_cost_subtree.go).** Private `resetCostBaseSQL`
  (:46, used :64); no cross-file SQL use found. One file alone: 23 -> 22. Not a
  stage on its own — fold it into whichever adapter-establishing stage runs.
- **CRUD core (session.go, session_lifecycle.go, session_read.go,
  session_update.go).** The `*service` receiver lives at session.go:439;
  `marshalTodos`/`unmarshalTodos` (session.go:549,560; used session_update.go:166,
  session.go:497). This is the root that stays; it shrinks only after every
  other cluster has moved.
- **agent_depth.go** — NOT independent: its methods call root CRUD-core
  helpers `s.bindWorkspace` (agent_depth.go:36; defined in session_lifecycle.go:27) and
  `isSessionsIDUniqueConstraintError` (agent_depth.go:41; defined in session_lifecycle.go:99). It moves only with
  or after the CRUD-core receiver seam (stage 6), not as a free single file.

### Service stage order

Prerequisite for stages 2+ of this package: an exported service adapter seam
(a small exported interface/constructor over the `*service` receiver state),
established while moving the first cluster.

1. Export requirement types + row-ID helpers into sesstypes; adapt `*service`.
   Zero reduction: 23 -> 23. Justified: stages 2-6 consume the seam.
2. Move requirements cluster under components/ (existing direct child: costs
   NO root element). 23 - 2 = **21**. The `*service` methods in the moved
   files cannot move (Go: methods cannot be defined on a type from another
   package, alias or not): each retained public method becomes a one-line
   forwarding method on `*service`, landing in an EXISTING root file
   (session.go or session_lifecycle.go), delegating to the child's exported
   function. Zero element cost; this is the standing shim budget for every
   service stage below.
3. Move role-contract cluster under components/. 21 - 3 = **18**. Forwarding
   methods per the stage-2 budget.
4. Export `finalizationIngressPayload` (sesstypes); move finalization
   cluster under components/. 18 - 5 = **13**.
5. Move runqueue/purge/outbox cluster under components/. 13 - 3 = **10**.
6. Move fork/inject/workspace + cost + agent_depth (5 files) under
   components/. 10 - 5 = **5**. Root keeps session.go, session_lifecycle.go,
   session_read.go, session_update.go + components. Delete the allowlist
   line (5 <= 7).

Prereqs strictly ordered: 1 -> 2 -> {3} -> {4 -> 5 -> 6}. All moves are nested under the existing components/ dir, so no stage creates a root element.

---

## Package 2: internal/session/batch/sessionbatch (28 elements)

Elements (28 files): batch_ack.go, batch_adapters.go, batch_api_store.go,
batch_child_bind.go, batch_cli_store.go, batch_commit.go,
batch_commit_doc.go, batch_consumers.go, batch_consumers_doc.go,
batch_control.go, batch_delivery_store.go, batch_history_rerun.go,
batch_history_reset.go, batch_history_snapshot.go, batch_inline.go,
batch_outbox_doc.go, batch_pull.go, batch_question.go, batch_reaction.go,
batch_recovery.go, batch_recovery_candidates.go, batch_recovery_doc.go,
batch_retention.go, batch_stop.go, batch_stop_fence.go, batch_store.go,
batch_tool_result.go, retention_purge.go.

### Clusters, coupling, receipts, net counts

Moving k files to a new child dir of sessionbatch: 28 -> 28-k+1.

- **Recovery (batch_recovery.go, batch_recovery_candidates.go,
  batch_recovery_doc.go).** Coupling (probed, exit 1): uses `BatchStore`
  (batch_recovery.go:32) and private `batchStateLive` (batch_recovery.go:91;
  definition batch_store.go:348); `batch_control.go:53:88: undefined:
  BatchHostProbe`, `batch_control.go:67:14: s.BatchRecoverDeadHost undefined`;
  undefined `BatchRecoveryOutcome`, `BatchRecoveryCursorStart`,
  `BatchRecoveryCursor`, `BatchRecoveryLimits`; white-box tests call the
  recovery methods. Net: 28 -> 26. De-coupling: recovery API/types used by
  control and the tests need a boundary; fixing `batchStateLive` alone is
  insufficient.
- **History (batch_history_rerun.go, batch_history_reset.go,
  batch_history_snapshot.go).** Coupling (probed, exit 1): `BatchStore`
  receiver methods; white-box tests (`batch_history_rerun_test.go:128:66:
  undefined: BatchRerunTruncation`, `:129:17: f.store.BatchRerunReconcile
  undefined`); shares private `sessionMessageIDsTx`
  (`batch_consumers.go:47:14: undefined: sessionMessageIDsTx`), `chunkStrings`
  /`rerunChunk`/`nullStrings` (batch_consumers.go:90-92). Net: 28 -> 26.
  De-coupling: move shared helpers to the consumer side or a shared package.
- **Stop/control (batch_stop.go, batch_stop_fence.go, batch_control.go).**
  Coupling (probed, exit 1): shared private `psfOwnerStopSQLPredicate`
  (`batch_store.go:30:181: undefined: psfOwnerStopSQLPredicate`; also used in
  batch_stop_fence.go), `psfOwnerStopRequestID` (white-box test
  batch_stop_fence_test.go:74:237), `batchStateLive` (batch_control.go:41,44),
  `BatchStore` methods (batch_control.go:53,67). Net: 28 -> 26. De-coupling:
  separate the stop SQL/request-ID helpers from the store/control impl.
- **Stores (batch_api_store.go, batch_cli_store.go, batch_delivery_store.go,
  batch_store.go).** Coupling (probed, exit 1): `BatchStore` and
  `batchStateLive` defined in batch_store.go are used package-wide
  (batch_control.go:41,53; batch_commit.go:30; tests). Net: 28 -> 25.
  De-coupling: moving `batch_store.go` requires resolving the rest of the
  package's receiver methods + white-box tests first — this is the LAST
  cluster, not the first.
- **Delivery/pull (batch_delivery_store.go, batch_inline.go, batch_pull.go).**
  Partial probe (delivery+inline subset) exit 1:
  `batch_delivery_store_test.go:45:18: s.BatchDeliveryRootEnvelope undefined`,
  `batch_pull_test.go:236:15: a.BatchAnnounceInlineResult undefined`. Shared
  helpers `pdcCreateTx`/`pdcUpdateTx` live in batch_tool_result.go:32,40. Net
  (all 3): 28 -> 26.
- **Commit (batch_commit.go + batch_commit_doc.go).** Uses `BatchStore`
  (batch_commit.go:30) and service APIs (:30,:36). Net: 28 -> 27. Probe of the
  doc file alone produced only unrelated service-package diagnostics (not a
  commit-coupling receipt).
- **Doc-only files** (batch_commit_doc.go, batch_consumers_doc.go,
  batch_outbox_doc.go, batch_recovery_doc.go): documentation bodies with no
  independent boundary. Merging each into its owning code file is a **-1
  element, text-only, low-risk** change — the only verified count reducers in
  this package.
- **Misc cluster map (every remaining file, coupling use -> definition):**
  batch_ack.go — `pdcCreateTx` (batch_ack.go:42 -> batch_tool_result.go:32);
  defines `BatchAnnounceStarted` (:21). batch_child_bind.go —
  `batchCommitInt63` (:63 -> batch_commit.go:48), `ErrBatchValueOverflow`
  (:64 -> batch_commit.go:22); defines `BatchBindChildSession` (:62).
  batch_consumers.go — `sessionMessageIDsTx` (:47 -> batch_history_reset.go:26),
  `chunkStrings`/`rerunChunk`/`nullStrings` (:90-92 -> batch_history_rerun.go
  :68,:65,:77); defines three `BatchOwnerDelete*`/history receiver methods
  (:37,:85,:109). batch_question.go — no cross-file private identifiers;
  defines `BatchPublishQuestion` (:22), `BatchResolveChildQuestions` (:208).
  batch_reaction.go — `pdcUpdateTx` (:332 -> batch_tool_result.go:40); defines
  15 receiver methods (:200-:571) AND the notice-store path:
  `ReadBatchDebtNodeRefs` (:91) with its private statement helper
  `batchOutboxDebtNodesVoidAware` (:69), both in this one file. External
  consumers of that path: internal/session/session_notice_store.go:390 (+
  its test :321,:387), bridge alias `BatchDebtNodeRef` at
  internal/session/session_bridge.go:90, and agent-side fixtures/tests
  (drain_attempt_fixture_test.go:233, batch_gate_refusal_test.go:98,
  batch_fixture_port_a3_test.go:56, batch_cutover_facts_node.go:219,
  batch_await_test.go:516,:597). batch_retention.go — `AsyncDataRetentionAge`
  (:65 -> retention_purge.go:18); five receiver methods (:13-:73).
  retention_purge.go — none. batch_adapters.go — `batchCommitSpecHash`
  (:166 -> batch_commit.go:137); `BatchProbeLive` (:40). batch_tool_result.go
  — none outbound; `pdcCreateTx`/`pdcUpdateTx` (:32,:40) serve ack/inline/pull
  (:42,:44,:259) and reaction (:332); defines `BatchConsumeToolResult` (:54).
  batch_commit.go holds the commit helpers every adapter/bind file needs
  (:22,:48,:137).

### Package-boundary rule for this package (receiver methods)

Go rule this package must obey: a type defined in one package cannot get
methods in another package, also not through an alias. A type moves together
with ALL its methods (the old name then becomes an alias and every caller
uses only the exported API), or its methods are turned into functions.
'Type in the child, methods left in the parent' does not compile.
`BatchStore` is defined at batch_store.go:44 with private `writer`/`reader`
fields (:44-62) and constructors :52,:60. Two legal end states exist:
(a) the type + ALL `*BatchStore` methods (spread over batch_store.go,
batch_ack.go, batch_child_bind.go, batch_consumers.go, batch_question.go,
batch_reaction.go, batch_retention.go, batch_adapters.go, batch_tool_result.go,
batch_commit.go, batch_delivery_store.go, batch_api_store.go, batch_cli_store.go,
recovery, history, stop/control files) move together into one child package,
and external importers (internal/session/session_bridge.go,
session_notice_store.go:390, internal/agent/runtime/batch/batchmeta/*,
batchcore/modelcontrol/*, sessionbatchread tests) retarget to the child's
exported API — retargets live outside sessionbatch, zero element cost; or
(b) every receiver method is first converted IN PLACE to a package function
taking `*BatchStore` (no element change), after which a file may move only if
every cross-cluster caller of its converted functions moves in the SAME stage
or the call is re-pointed to an already-moved child. Route (b) is the only
possible piecemeal route and is UNPROVEN: needs a discovery lane per cluster
to enumerate cross-cluster callers before any stage below can be asserted
legal. The earlier batchcore-leaf idea (type in leaf, methods staying in
sessionbatch) is DROPPED: illegal under this rule.

### Batch stage order (route to 7; arithmetic = files moved +1 new dir each)

1. Merge `batch_commit_doc.go` into `batch_commit.go` -> **27**.
2. Merge `batch_consumers_doc.go` into `batch_consumers.go` -> **26**.
3. Merge `batch_outbox_doc.go` into `batch_pull.go` -> **25**.
4. Merge `batch_recovery_doc.go` into `batch_recovery.go` -> **24**.
5-11. UNPROVEN: needs a discovery lane each. Target arithmetic, not
   asserted stages: recovery 24-2+1=23; history 23-3+1=21; stop/control
   21-3+1=19; delivery+tool_result 19-4+1=16; interaction (ack,
   child_bind, consumers, question, reaction, adapters, commit)
   16-7+1=10; retention 10-2+1=9; stores+BatchStore-type last 9-3+1=7.
   Open question per lane: under receiver rule (b), which cross-cluster
   callers must co-move; under rule (a), the type+all-methods move is one
   combined stage, not seven lanes. Endpoint under (a) or a proven (b):
   root = child dirs only, at most 7, allowlist line deleted at 7. The
   earlier probe receipts (exit 1) show the couplings these lanes must
   break; none of steps 5-11 was probe-verified as compiling.

---

## Package 3: internal/session/jobbatch/lab (24 elements)

Elements (24 files): controlled.go, controlled_cleanup.go,
controlled_gates.go, controlled_output.go, errors.go, journal.go,
journal_state.go, oracle.go, oracle_check.go, oracle_cleanup.go, process.go,
process_other.go, process_unix.go, process_windows.go, replay.go,
replay_engine.go, replay_metadata.go, scenario.go, scenario_actions.go,
scenario_decode.go, scenario_diagnostics.go, scenario_run.go,
scenario_validate.go, schema.go. (testdata/ excluded by the guard.)

### Boundary map (verified this pass; supersedes "no verified map")

- **errors** (errors.go) — leaf. Exports `ErrUnknownAttempt`,
  `ErrAttemptPhase`. Consumed by controlled (controlled.go:82,106,187,212,215,
  242,257,264; controlled_gates.go:16,30,44,73,80,102,106;
  controlled_output.go:14) and schema (schema.go:265-266).
- **schema** (schema.go) — definitions-only hub; exports `Scenario`, `Spec`,
  `Action*`, `Expect`, `GateResult`, `Journal*`, `Error*` etc. Consumed by
  everything; only consumes errors.
- **process** (process.go + 3 platform files) — leaf vs siblings. Consumes
  schema `ProcessOptions` (process.go:31). Platform files implement
  `processTree`, `processPrepare`, `attach`, `term`, `kill`, `awaitGone` etc.
  consumed from process.go:35,96,119,120,123,128,177,227,231,240,244,261.
  Constraint: all four files must stay in one package compiled under both
  windows and linux.
- **controlled** (controlled.go, _cleanup, _gates, _output) — exports
  `ControlledExecutor`, `NewControlledExecutor`, gate/dispose/read APIs.
  Consumes errors + schema (`GateResult` controlled.go:36-39) and scenario's
  private `scenarioGateValid` (controlled_gates.go:72,181).
- **journal** (journal.go, journal_state.go) — hub. Consumes oracle
  (journal.go:77; journal_state.go:11-23), replay helpers
  (journal.go:158,162; journal.go:165,170), scenario/schema types.
- **oracle** (oracle.go, _check, _cleanup) — hub. Type-asserts
  `*ControlledExecutor` (oracle.go:184); consumes journal internals
  (oracle_check.go:16,23,165,170); `Expect` (oracle_check.go:11,41-43).
- **replay** (replay.go, _engine, _metadata) — heavy journal-internal
  consumer (replay.go:23-51,93,103; replay_engine.go:33,48,144,170,206;
  replay_metadata.go:11).
- **scenario** (6 files) — biggest hub: controlled (scenario.go:88-89,147-149,231;
  scenario_actions.go:80-118; scenario_diagnostics.go:5-14; scenario_run.go:89,97-102),
  journal (scenario_run.go:129-130,259-269; scenario_actions.go:134-148),
  oracle (scenario.go:21-23,84-89,116,120-125,128,277-279;
  scenario_actions.go:83,160), process (scenario_run.go:97), schema.

### Probe receipts

- errors.go move probe: exit 1 — undefined `ErrUnknownAttempt`/`ErrAttemptPhase`
  at controlled/schema and test call sites. Net if moved alone: 24 -> 24 —
  **not a stage**.
- process move probe (4 files to child dir): exit 1 — undefined
  `processExecution`/`NewProcessExecutor` at test and scenario call sites.
  De-coupling: exported constructor/interface for scenario; then net 24 -> 21.
- replay move probe (3 files): exit 1 — undefined `Replay`, `journalActionMap`,
  `journalEvent`, `journalRebuild`, `journalTransition` at oracle, test, and
  replay call sites. Net after de-coupling: 24 -> 22 (later: fewer).
- (Process disclosure: the errors and process probes overlapped in execution;
  outputs are separate observed compiler runs but not strictly serialized.
  A journal probe never compiled — bad path in the probe command; reverted.)

### Lab stage order (arithmetic = -files +1 dir per new child; unproven
stages labelled)

Baseline receipts are BASELINE ONLY (windows `go test -c` exit 0, GOOS=linux
`go build` exit 0); no extraction below is probe-verified.

1. Process: 4 files into lab/process. `ProcessOptions` MOVES INTO the child
   (from schema.go:68; used process.go:31, scenario_run.go:20,93-97), one
   alias line stays in schema.go until the Scenario stage. Child imports
   only the parent jobbatch package. 24 - 4 + 1 = **21**. Arithmetic is
   certain; the move itself is unproven (probe of the naive move exited 1).
2-6. UNPROVEN: needs a discovery lane. Open question, exactly: the root-only
   schema types (`GateResult`, `StartDecision`, `Action`, `Expect`, ...) are
   consumed by controlled/oracle/journal/replay while `scenarioGateValid`
   (scenario.go:328) is called from controlled_gates.go:72,:181, and
   replay_engine.go:33,:48 calls `Scenario` methods (`SubmitRequest`
   scenario.go:266, `AddRequest` :293; also :177,:217,:320). Go's receiver
   rule forces `Scenario` + its methods to move together with scenario.go,
   so schema.go (where `Scenario` is defined) cannot move to a leaf before
   scenario.go — the earlier types stage (errors+schema first) is ILLEGAL
   and is dropped. Legal shapes to evaluate in the discovery lane:
   (i) one combined stage moving schema.go + 6 scenario files + errors.go
   + controlled(4) + oracle(3) + journal+replay(5) = 19 files, one or more
   children — not lane-sized; (ii) export the root-only schema types into a
   new leaf under lab first (leaf costs +1 element), then controlled ->
   oracle -> journal+replay -> scenario in that order, each child importing
   the leaf. Target arithmetic under (ii) from 21: leaf +1 = 22;
   controlled 22-4+1=19; oracle 19-3+1=17; journal+replay 17-5+1=13;
   scenario(+schema+errors) 13-8+1=6. ENDPOINT DECISION: no root shim file
   is required — cmd/batchlab/main.go:59,:72,:78 and scenario_test.go
   retarget from lab to the scenario child (outside lab, zero element
   cost), so the root ends at **6** child dirs, below the limit; the
   allowlist line is deleted at the last stage. If a lane instead keeps a
   root facade file for the lab.* API, the endpoint is 7 (6 dirs + 1
   counted facade file).

---

## Package 4: internal/session/run/sessionrun (12 elements)

Elements (12 files): run_queue_admission.go, run_queue_clock.go,
run_queue_drain_classify.go, run_queue_drain_ledger.go,
run_queue_drain_session.go, run_queue_entry_dispatch.go,
run_queue_entry_exec.go, run_queue_lifecycle.go, run_queue_orphan_drain.go,
run_queue_pump.go, run_queue_row_context.go, run_queue_testsupport.go.

Known blocker, now precisely mapped. Drain-cluster receiver methods on
`*RunQueuePump`: `DrainSessionNow` (run_queue_drain_session.go:15) — the only
one; classify and ledger files define no receiver methods. Private pump
members the drain session file touches: `admitSession` (:77),
`cfg.TestAfterAdmissionRefusal` (:107-108), `cfg.Sessions`/`cfg.PumpInstanceID`/`leaseTTL` (:217,:262,:265,:303-305,:330-333,
:339-342,:566-567,:574,:733,:756), `dbWriteTimeout` (:261,:303,:330,:339,:566),
`execSem` (:288,:378,:387), `admitMu`/`stopping`/`workerWg`
(:303-305,:330-333,:339-342,:378-379,:387), `executeEntrySync` (:387).
run_queue_drain_ledger.go:538 mentions `p.inFlight` only in a comment.
Minimal change in the pump cluster: expose one exported drain-facing handle
constructed from `RunQueuePump` carrying admission, lease/terminal/nack/recheck,
execution, worker/semaphore admission and the timeouts/config hook; drain
result and admission outcome types need exported forms. A plain struct of the
private fields does not work across the package boundary.

### Clusters, receipts, net counts (12 -> 12-k+1 per move of k files)

- **Admission (run_queue_admission.go)** — probed, exit 1:
  `undefined: admissionEntry` at run_queue_pump.go:607,664;
  `undefined: admissionOutcome` + helpers in drain/dispatch
  (run_queue_entry_dispatch.go:41,71,168,262-288; run_queue_drain_session.go:77,233,283,302,338,382,399,492);
  missing `p.admitSession`/`p.AdmitSessionForTest`. Not a standalone stage;
  the admission data contract + gate API must become an exported boundary first.
- **Drain cluster (classify, ledger, session)** — probed (classify subset),
  exit 1: `undefined: classifyBackgroundOutcome` at drain-session :135,:399,
  `undefined: isOrdinaryRetryableOutcome` at :423. All three move together
  behind the drain handle: net 12 -> **10**.
- **Clock (run_queue_clock.go)** — probed, exit 1: `undefined: PumpClock`
  (the PumpClock INTERFACE is declared in run_queue_clock.go:11;
  run_queue_pump.go:364 only declares the field `TestClock PumpClock` using
  it), `p.now`/`p.clock` undefined in
  run_queue_entry_exec.go:399,418,449,516 and run_queue_lifecycle.go:121,178,194.
  Alone: 12 -> 12, not a stage; pair it.
- **Entry dispatch/exec (run_queue_entry_dispatch.go, run_queue_entry_exec.go)**
  — not probed as a pair; coupling listed above plus exec's lease helpers and
  session services (run_queue_entry_exec.go:97,115,203-218). Behind a shared
  worker/execution handle: net 12 -> 10 (from the post-drain 10: 10 -> 9... the
  pair is k=2 -> -1).
- **Lifecycle / orphan drain / row context / testsupport** — each single-file
  moves are 12 -> 12 (not stages alone); pair two into one shared child dir
  for -1. Orphan drain needs a small exported service/config handle instead of
  the pump (run_queue_orphan_drain.go:25-35,93-95,175-182). Row context is externally consumed: `WithRunQueueRowID` at
  internal/session/finalization_queue_test.go:198 and
  internal/agent/finalization_queue_call_test.go:49; `RunQueueRowIDFrom` at
  internal/app/app_finalization_wiring.go:118, internal/app/app.go:77,
  internal/agent/finalization_queue_reservation_test.go:94; in-package use at
  run_queue_entry_exec.go:590. Moving it is an import retarget for those
  external files (zero root element cost) PLUS one root import of the child
  from run_queue_entry_exec.go — allowed because the row-context child imports
  nothing from root (its context-key type is private and self-contained).
  Test support reads private `p.execSem` (run_queue_testsupport.go:6-7); no external
  consumer of `LiveRunQueueDispatchesForTest` found.
- **Pump (run_queue_pump.go)** — the dependency root; stays last.

### Sessionrun stage order

1. Drain handle + boundary types; move the drain cluster (3 files; the
   retained `DrainSessionNow` METHOD stays as a one-line forwarding method
   on `*RunQueuePump` in run_queue_pump.go — a root file, zero element
   cost — delegating to the child's exported function (legal: the method stays on the package's own type). 12 - 3 + 1 = **10**.
2. Worker/execution interfaces; move entry dispatch+exec (2 files; same
   forwarding-method rule for any retained public receiver method, shim in
   run_queue_pump.go). Prereq: stage 1's exported contracts.
   10 - 2 + 1 = **9**.
3. Move clock + orphan drain into one child (2 files; orphan drain uses
   only `p.ctx`, `p.cfg`, `dbWriteTimeout` (run_queue_pump.go:483) and its
   own helpers — budget: one exported config/timeout handle; clock moves
   its `PumpClock` interface (run_queue_clock.go:11) and root keeps a
   `p.now`/`p.clock` forwarding shim in run_queue_pump.go). 9 - 2 + 1 = **8**.
3b. OPTIONAL: row context joins the SAME child dir created in 3 (no new
   dir): 8 - 1 = **7**. Goal met; delete the allowlist line. If row context
   is NOT moved, the goal stays 8 and the allowlist line survives — this
   is the fork the earlier draft missed: the third file changes the goal
   only because it lands in an existing dir.
4. OPTIONAL: testsupport + lifecycle into a second child (2 files, +1 dir):
   7 - 2 + 1 = **6**. Receiver placement per the Go receiver rule: `Start`
   and `Stop` (run_queue_lifecycle.go:19,:37) and
   `LiveRunQueueDispatchesForTest` (run_queue_testsupport.go:6) are methods
   on `*RunQueuePump` and CANNOT move; they stay as methods in
   run_queue_pump.go (existing root file, zero element cost), their bodies
   becoming calls to free functions in the child (shim budget: 3 forwarding
   methods + exported handle). Same rule already applies to
   `drainOrphanOutbox` (run_queue_orphan_drain.go:19) in stage 3: converted
   to a child function, its root caller re-pointed. Prereq: exported
   accessor replacing the `execSem` read (run_queue_testsupport.go:6-7) and
   a lifecycle controller interface. Prereq: stages 1-3.

Stages 1 -> 2 -> {3 (+3b)} -> {4}.

---

## Full stage list (ordered across packages)

| # | Stage | Goal count | Prereqs |
|---|---|---|---|
| S1 | Batch doc merges | 28 -> 27 -> 26 -> 25 -> 24 (four -1 merges) | none |
| S2 | Service requirements export | 23 -> 23 (zero; S3-S7 consume the seam) | none |
| S3 | Service requirements move | 23 -> 21 (23-2, under components) | S2 |
| S4 | Role-contract | 21 -> 18 (21-3) | S2 |
| S5 | Finalization | 18 -> 13 (18-5) | S2 |
| S6 | Runqueue/purge/outbox | 13 -> 10 (13-3) | S2, S5 |
| S7 | Fork/inject/workspace+cost+agent_depth | 10 -> 5 (10-5, under components; 6 if agent_depth stays) | S2, S5 |
| S8 | Lab process | 24 -> 21 (24-4+1) | none (move unproven) |
| S9 | Lab schema/scenario/controlled/oracle/journal+replay | UNPROVEN: discovery lane; target endpoint 6 (retarget cmd/batchlab) or 7 with a counted facade file | S8 |
| S10 | Run drain cluster | 12 -> 10 (12-3+1) | none |
| S11 | Run dispatch/exec | 10 -> 9 (10-2+1) | S10 |
| S12 | Run clock+orphan | 9 -> 8 (9-2+1) | S11 |
| S13 | Run row context joins S12 dir | 8 -> 7 (8-1) | S12 |
| S14 | Run testsupport+lifecycle | 7 -> 6 (7-2+1) | S13 |
| S15 | Batch cluster extractions | UNPROVEN: discovery lanes; target endpoint 7 via receiver rule (a) or (b) | S1 |

Each stage is one lane: probe-move first with the lock wrapper, land the move,
update `.githooks/dir_entries_allowlist.txt` as the guard requires: a count above 7 stays allowed while it does not exceed its recorded ceiling; the line must be deleted in the same commit once the count reaches 7.

## Honesty ledger

- Compiler receipts above are OBSERVED from lock-wrapper probe builds that
  were reverted immediately; no probe demonstrated a compiling end state.
- Stage counts are arithmetic under the plan's own rule (-files +1 dir per
  new child dir, shims budgeted per stage); probe receipts (exit codes and
  error text) cover the FIRST attempted moves of requirements, recovery,
  history, stop/control, stores, delivery subset, errors, process, replay,
  admission, clock and drain-classify only — all failed, none demonstrated
  a compiling end state.
- sessionbatch cluster moves and service stages S5-S7 are the least verified;
  budget a discovery lane each before the move lane.
- Routes marked UNPROVEN: batch steps 5-11 (receiver-method rule makes
  piecemeal moves illegal until caller enumeration) and lab stages 2-6
  (Scenario/receiver ownership makes the earlier types stage illegal);
  both need a discovery lane before any move is asserted.
- The lab endpoint is 6 child dirs with external import retargets, or 7
  with a counted root facade file; the service endpoint is 5 (6 if
  agent_depth.go stays). The table states what each route really reaches.
