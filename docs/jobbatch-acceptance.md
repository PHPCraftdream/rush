# Current H4 acceptance requirements (not gates-passed evidence)

This is the required evidence matrix for the composed H4 candidate. No row is
claimed passed here. All execution is GitHub CI; this documentation worker ran
no tests, builds, lint, benchmarks, or formatters. Every artifact must identify
the exact candidate SHA and preserve raw output/artifacts.

| Gate | Required evidence before rollout |
|---|---|
| Normal CI | Required normal workflow green on the exact composed SHA, including its schema/sqlc checks. |
| Shadow build/agent | `shadow-build` and `shadow-agent` green on all three OSes. |
| Shadow web | `shadow-web` green; web unit/type/lint is not browser acceptance. |
| AW | Nonzero actual AW-a and AW-b execution; a green selector with “no tests to run” is insufficient. |
| Import startup/reopen | `Connect` imports before destination open; re-open preserves subsequent destination writes; import once only. Verify existing (even malformed) destination wins, import errors have no fallback, old source and sidecars remain untouched, durable history/settings/queues/schedules are carried, and old/preparation execution/control state is absent. |
| CT / Windows | CT Windows production-path acceptance green, not merely helper tests. |
| #1427 recovery | Exact-SHA evidence for real startup/periodic/scope recovery, confirmed-dead-only and live/unknown negatives, one non-waking notice/no replay, bounded progress, and reset/attach recovery. Record issue scope/status rather than assuming closure. |
| #1475 supervision/check-in/presentation | Verify wake-only ingress and one later delivery without termination/admission release; SV started-boundary arming, child exemption, per-call/config disable and pause/resume. Historical silence flagging, repeated-question summary, idle-child own-jobs text, and heartbeat tool/task detail must not be reduced without explicit signoff. |
| D11 final store candidate | Re-measure on the exact final fresh-store candidate with pinned isolated baseline and raw artifacts, baseline/candidate SHAs and transition evidence. Current minimum eight-session throughput ratio vs the pinned baseline is 0.75 for the direct-view workload and 0.80 for legacy-bash (`tools/d11_report.py:17-21`); the added p95 latency budget is 15 ms (`tools/d11_report.py:239`). No errors or persistence bypass; the old unified 80%-only threshold must not be used. Both workloads independently pass; do not claim evidence without artifacts. |

Operational acceptance also requires quiescence (stop old processes and automatic
restarts throughout import; enumeration is not a registration fence), drain/stop
all preparation hosts, acknowledgment that pre-SF bare-UUID owner-stop provenance
cannot safely be inferred, and acknowledgment that requeued work may repeat
external side effects. Rollback stops new hosts and runs the previous binary
against untouched `rush.db`, restoring the old snapshot only; preserve
`rush_store_v1.db` and sidecars, with no reverse importer or merge-back.

## Historical 2026-10-12 autonomous-engine ledger (superseded for H4)

The dated ledger below remains an immutable account of its historical scope,
experiments and limits. It is not current H4 acceptance, does not establish
current gates, and must not be read as a gates-passed claim.

Evidence date: 2026-10-12. Scope: design
`docs/plans/2026-10-07-job-batches-design.md`, section 6, native Windows.
This ledger distinguishes executed assertions from design promises. It does not
claim autonomous completion or Rush integration readiness.

## Evidence and commands

Test declarations were located with `rg '^func Test' internal/jobbatch -g
'*_test.go'` and the equivalent search in `cmd/batchlab`. Relevant assertions,
fixtures, interpreter, command parser and the entire 731-line acceptance test
were reread. Names below are exact declarations, not proposed tests.

All command examples replace the actual machine wrapper location with the
portable placeholder `<repo-root>`. Run from the tested checkout/worktree;
`<repo-root>` is the checkout supplying the wrapper, possibly its parent.
No machine-specific paths are required in scenarios or documentation.

**T: directly observed targeted verbose run.**

```sh
bash <repo-root>/.rush/audit/heavy.sh capm 2g go test -race -p 1 -parallel 1 -count=1 -timeout 900s -run 'Test(Core(E[1-9]|ReSeeded|WeSeeded|AddLast|Final|Notice)|Runtime|Acceptance|Journal|Scenario(ProcessL[125]|L5Withheld|WCLifecycle|PhysicalStop|ReadEntry|StrictValidation|ReferencesAndAsync|Conversion|InterpreterRetains)|Controlled(AttemptPhases|PhysicalStop|Dispose|ReadGate|OutputWaits)|Replay|OracleClosed|MainEvery)' -v ./internal/jobbatch/... ./cmd/batchlab/
```

First T passed: rc=0, wrapper wall=31s, peak=383MB, lab=12.720s,
cmd=3.235s. Verbose seeded subtests exceeded the tool output limit. A second
successful T retained only top-level RUN/PASS, package tails, capture totals and
process evidence, using `set -o pipefail` and piping `2>&1` to:

```sh
rg '^=== RUN   Test[^/]+$|^--- PASS:|^--- FAIL:|^ok|^FAIL|^heavy.sh:|49 classified|intentional F-c gap|exit3 own trees dead'
```

This second run was output compaction, not a failure/flake retry. Every named
mapped test selected by T appeared as RUN and PASS in the compact result.

```text
ok github.com/PHPCraftdream/rush/internal/jobbatch 3.708s
ok github.com/PHPCraftdream/rush/internal/jobbatch/lab 11.980s
ok github.com/PHPCraftdream/rush/cmd/batchlab 2.788s
heavy.sh: class=test rc=0 wall=29s peak=384MB cpu=4s
49 classified: map[closed:37 closed-gap:1 exit3-closed:5 invalid:6]
intentional F-c gap: retained suffix only; no invariant/history reconstruction
```

**A: directly observed focused acceptance baseline, after five restorations.**

```sh
bash <repo-root>/.rush/audit/heavy.sh capm 2g go test -race -p 1 -parallel 1 -count=1 -timeout 900s -run 'TestAcceptance' ./internal/jobbatch/... ./cmd/batchlab/
```

rc=0, wall=14s; core=1.129s and cmd=1.050s had no matching tests;
lab=4.359s. Wrapper printed peak=0MB/cpu=0s; these zero readings are not a
claim of zero resource use. Before mutants the improved baseline also passed,
lab=2.997s, wrapper wall=13s. Formatting `-w` then `-l` of the acceptance file
with the provided gofumpt executable produced no listing.

**O: orchestrator-observed results supplied to this documentation phase.**
These are reported observations, not commands rerun by this worker:

| Selection under the same wrapper/packages/race/serial/900s settings | Reported actual result |
|---|---|
| `-count=1 -run 'Test'` | rc=0, wall=30s, peak=386MB; core=3.868s, lab=14.599s, cmd=2.571s. |
| `-count=1 -run 'Test' -v` | rc=0, wall=31s; lab=14.331s, cmd=2.523s; all names ran, output truncated. |
| `-count=5 -run 'TestAcceptance'` | rc=0, wall=35s, peak=383MB; core=1.268s and cmd=1.723s no matching tests; lab=20.887s. |
| Scoped golangci-lint, `HEAVY_TIMEOUT_SEC=1800` | `0 issues.`, rc=0, wall=33s, peak=383MB; exact orchestrator command below. |

```sh
HEAVY_TIMEOUT_SEC=1800 bash <repo-root>/.rush/audit/heavy.sh capm 2g golangci-lint run ./internal/jobbatch/... ./cmd/batchlab/...
```

Reported output tail:

```text
0 issues.
```

Reported wrapper result: rc=0, wall=33s, peak=383MB.

No external standalone batchlab executable run/replay, native Unix run, cross-vet
or Go environment override was performed in this phase. No auditable earlier
cross-vet command/result is attached, so cross-vet is unobserved. Windows tests
cannot establish Unix process-group behavior.

## Status meaning and shared evidence

- **Covered:** assertions matching the row's autonomous boundary passed on
  Windows; finite test coverage, not proof for every event interleaving.
- **Partially covered:** executed evidence exists, but a requested subvariant,
  exact public boundary or executable/platform requirement remains unverified.
- **Not covered:** no autonomous evidence for the requested integration seam.

Every non-AW row below references T (direct named RUN/PASS). O is supplementary
whole-suite evidence. Scenario stems mean existing `.json` files in
`internal/jobbatch/lab/testdata/scenarios/` and are executed by
`TestAcceptanceFixtureJournalReplaySweep` and `TestMainEveryScenarioFixture`.
The latter calls command `Main` in process; it does not execute `main` externally.
The expected/actual column states what assertions observed, not inferred logs.

The acceptance sweep reconstructs events using scenario refs/spec actions and
public Engine.Apply/Transition.Changed. It never reads private engine state or
rebuilds missing history from snapshots. This reconstruction depends on the
production Engine and is not an independent scheduler oracle. Separate assertions
check tree/admission/notice invariants. Core seeded tests additionally inspect
internal accounting/generations; they are not public-feed-only proofs.

## Complete section-6 map (51 distinct IDs)

### Engine, runtime and notice cases

| ID | Status | Exact test/scenario evidence (T) | Expected / observed boundary; remaining limit |
|---|---|---|---|
| E1 | Covered | `TestCoreE1TaskRootRejected`; `TestRuntimeNestedAddOwnershipAndDiscovery`; `TestRuntimeNoticesAndAdd`; `TestAcceptanceE1SameIdentityIngress`; `TestScenarioConversion` | Raw-root lab and explicit OneItem-equivalent batch at the same root identity yield equal policy/tree/feed except Ref, one launch/result/notice; core and Runner root-task rejection and nested/Add no-double-wrap assertions pass. |
| E2 | Covered | `TestCoreE2SequentialNestedBudgets`; `TestCoreE2NestedSequentialOrdering`; `TestCoreE2PositiveParallelAncestorCap`; `TestRuntimeSequentialPendingNestedSettlement` | Sequential(0)/(1), nested gate, positive ancestor cap, g:1/h:2 no inherited cap, active cancellation and leaf-count assertions pass. |
| E3 | Covered | `TestCoreE3FailureSkippingPreservesTerminal`; `TestCoreNoticeSkippedCancelledAndAllSuppression`; `l3` | Failed/TimedOut/Interrupted skip pending successors; Cancelled does not; Continue starts next; cancelled/empty terminal successors retain states and skipped notices pass. |
| E4 | Covered | `TestCoreE4PersistentRoundRobin`; `TestCoreE4NoGroupHeadOfLineBlockage`; `TestRuntimeClosedAndOpenProfiles`; `w-a` | B admission follows A settlement without extra B wake, blocked groups do not hide eligible work, closed separate-root controlled dependency case passes. Liveness still assumes finite work and executor premises. |
| E5 | Covered | `TestCoreE5CapacityGenerationBothOrdersStaleToken`; `TestCoreE5SettledGenerationUnblocks`; `TestCoreE5SettledBeforeLateCapacityRefusal`; `TestRuntimeCapacityRetryTimer` | Both refusal/signal orders, settlement generation, stale token/deadline, no refusal spin and fresh retry token pass. |
| E6 | Covered | `TestCoreE6ControlRetryTable`; `TestCoreE6LastPendingStopSettlement`; `TestRuntimeSelectiveSubtreeStopRetry`; `TestRuntimeSerializedStopRetry`; `TestRuntimeStopPanicHoldsSlot` | Pending stop has no execution, cause/slot persist through errors/ack/timeouts, selective retries and serialized physical Stop/timer gates pass. |
| E7 | Covered | `TestCoreE7AddAtomicBoundsIDsEmpty`; `TestCoreAddLastSettleBothOrdersAndNestedDiscovery`; `r-c-empty`, `r-c-pending`; `TestAcceptanceDeterministicConstructionStreams` | Atomic invalid Add, ordinal IDs, bounds/duplicate root, both last-settle orders, zero-leaf nested settlement/no commands and repeated fenced streams pass. |
| E8 | Covered | `TestCoreE8StaleDuplicateDeadlineAndCauseWinner`; `TestCoreReSeededTreeSettlement`; `TestCoreWeSeededAccountingClosedAndOpen`; `TestAcceptanceFixtureJournalReplaySweep` | Stale/duplicate events cannot change winner; after each reconstructed transition live-child/terminal-descendant and budget invariants hold; notice uniqueness/settlement/sole-child and one Final pass for intact captures. |
| E9 | Partially covered | `TestCoreE9LimitsViewsAndFullLaunch`; `TestRuntimeGroupOnlyBindingOwnership`; `TestRuntimeSubmitMutationAfterHandoff`; `TestRuntimeNestedAddOwnershipAndDiscovery`; `TestRuntimeCapabilities`; `TestRuntimeReportedOutcomesPrivacy`; `TestAcceptanceResultAndProcessPrivacyJournal` | Limits, Submit/Add payloads, messages, views and records detach; real Summary inspection, Details report and production writer privacy pass. Exhaustive mutation-after-handoff of every section-2.2 value, particularly original Result Details after callback, is not established by these assertions. |
| R1 | Partially covered | `TestRuntimeStartResourceCleanupAndSlot`; `TestRuntimeStartOutcomes`; `TestRuntimeStopBeforeStartReturns`; `c-e` | Immediate report/Started ordering, fail/nil/panic, executor-owned resource cleanup, rejected report ignore and in-flight Stop pass. A dedicated concurrently accepted Add/Stop/Settled three-way race is not identified here. |
| R2 | Covered | `TestRuntimeVirtualDeadlines`; `TestRuntimeCapacityRetryTimer`; `TestRuntimeStartReturnsAfterCloseDeadline`; `TestRuntimeHostCancellationFailedControlDrains`; `TestRuntimeCancelledCloseStillDrains`; `TestRuntimeDonePublishesWholeFinalTransition` | Natural/stop/timeout winners, stale timers, cancelled Wait, first Close timeout/late Start/report, detached controls and records-before-Done pass. |
| R3 | Partially covered | `TestRuntimeNoticesAndAdd`; `TestRuntimeFinalFeedAndFrozenReads`; `TestJournalFrozenGapWithReaderBarrier`; `TestJournalWriteFailureStopsAtContiguousPrefix`; `f-a`, `f-b` | Public selected-notice suppression, held readers, explicit gaps and failed writer capture pass. Failed-observer scheduling/cancellation isolation as a dedicated consumer-failure case is not fully established. |
| R4 | Partially covered | `TestRuntimeOutputCursorAndInjectPanic`; `TestRuntimeCapabilities`; `TestRuntimePinsBlockDisposal`; `TestRuntimeRetentionHeldCalls`; `TestControlledReadGateAndStream`; `TestControlledOutputWaitsForData` | Cursor A/B/EOF, truncation, capability errors, copied Inject, read/Stop pins and disposal pass. Exact nested paused Inject-vs-settlement race and all error-order variants are not separately established. |
| R5 | Covered | `TestRuntimeRequestCancellationWinner`; `TestRuntimeCommittedReplyPublishesRecords`; `TestRuntimeNeverAdmittedSettlement`; `TestRuntimeNestedAddOwnershipAndDiscovery`; `TestRuntimeAddGroupsAndOutcomes` | Queued abandonment versus accepted/reply cancellation, actual reply-after-record boundary, failed Wait, deep Launch metadata/discovery and never-started terminal/Release cached results pass. |
| R3-one | Covered | `r3-one`; `TestRuntimeNoticesAndAdd/one`; `TestAcceptanceDeterministicConstructionStreams` | NotifyEach one child selects only R once, fences and repeat identical streams pass. |
| R3-chain | Covered | `r3-chain`; `TestRuntimeNoticesAndAdd/chain`; `TestAcceptanceDeterministicConstructionStreams` | One-child chain collapses to outer R notice, not intermediate/leaf notices; passes. |
| R3-add | Partially covered | `r3-add`, `r3-add-helper`; `TestRuntimeNoticesAndAdd/add`, `TestRuntimeNoticesAndAdd/helper-add` | Add reply R.2 before first settlement; Each selects R.1,R.2,R; raw helper/all selects R. Runtime helper changed explicitly to NotifyEach before Submit is not present in helper-add (it remains all). |

### Settlement, feed and controls

| ID | Status | Exact test/scenario evidence (T) | Expected / observed boundary; remaining limit |
|---|---|---|---|
| R-a | Partially covered | `r-a`, `r-a-h-held`, `r-a-h-blocked`; `TestRuntimeNeverAdmittedSettlement`; `TestAcceptanceFixtureJournalReplaySweep` | Never-admitted cancellation, no starts, rejected Add, terminal Stop, release and replay settlement pass, including h held/blocked. Exact single-leaf helper-root pre-Release Done/Wait/head/zero-disposal API bundle is not independently pinned by the two-child Runtime test. |
| R-b | Covered | `r-b`; `TestRuntimeSequentialPendingNestedSettlement` | Pending nested child cancelled before predecessor settles, no nested starts, next sibling admission and notice order/counts pass. |
| R-c | Covered | `r-c-empty`, `r-c-pending`; `TestCoreE7AddAtomicBoundsIDsEmpty` | Empty nesting completes on Submit with zero counts/commands; stopped pending subtree settles before root; terminal Add/Release and replay pass. |
| R-d | Covered | `r-d-i`, `r-d-ii`; `TestCoreE6LastPendingStopSettlement`; `TestRuntimeNeverAdmittedSettlement` | Partial pending subtree stop leaves B live, last stop reduces with empty cause; async HostClosing variant settles pending B before held A confirmation; passes. |
| R-e | Covered | `TestCoreReSeededTreeSettlement` | Seeds 1..32 at global 2/3; valid events, tree/budget/notice invariants and authoritative HostClosing drain/replay pass. Finite seeded coverage only. |
| F-a | Covered | `f-a`; `TestRuntimeFinalFeedAndFrozenReads`; `TestRuntimeDonePublishesWholeFinalTransition` | Close independent of reader, all Final records before Done, selected R.1/R.2/R and exact closed cursor/sequence pass. |
| F-b | Covered | `f-b`; `TestJournalIndependentClosedReaders`; `TestRuntimeFinalFeedAndFrozenReads` | Concurrent nonconsuming identical feeds, F+1/F+3 EOF recovery cursor, late report no changes and frozen final reads/postclosed errors pass. |
| F-c | Covered | `f-c`; `TestJournalFrozenGapWithReaderBarrier`; `TestRuntimeFrozenRetentionAndTail`; `TestAcceptanceFixtureJournalReplaySweep` | Held writer forces gap first_missing=1; four-record frozen suffix ends at F; run/replay exit5 and no history repair. Gap suffix is explicitly excluded from complete-history invariant sweep. |
| F-d | Partially covered | `f-d`; `TestRuntimeFrozenRetentionAndTail`; `TestCoreFinalPendingDrainAndClosed`; `TestReplayL6Integrity`; `TestJournalReplayAndMutations` | Capacity+HostClosing stateless tail S_F=s_A+2, no-root Final, exact F and missing closed/wrong-F rejection pass. Exact removal of HostClosing+closed together with retained footer and explicit release-tail comparison are not identified as dedicated corruptions/assertions here. |
| F-e | Covered | `f-e`; `TestJournalUnfinishedCutAndLaterFinal`; `TestRuntimeStartReturnsAfterCloseDeadline` | Shortened harness captures intact Cancelling prefix, absent final_cursor and replay3; later report/Close cannot alter journal. Full fixture eventually closes but retains run exit3. |
| F-f | Covered | `f-f`; `TestJournalInvocationCancellationDrainsClosed`; `TestJournalCancelledAttemptedCloseUsesFreshCleanup` | Actual invocation cancellation harness exits3, fresh cleanup reaches complete closed F and replayed failed aggregate1. Unmodified f-f fixture alone exits1 and is not evidence of invocation cancellation. |
| C-a | Partially covered | `c-a`, `c-a-ii`; `TestRuntimePinsBlockDisposal`; `TestScenarioReadEntryBarrierAndCleanupJoin` | Held Stop settlement-before-control and timeout-before-settlement, Release/pin-delayed Dispose, cached results and held-read entry are exercised. Exact full C-a iii independent cancelled read variant with return_read/join/await_dispose script is not a fixture; harness coverage is related, not identical. |
| C-b | Covered | `c-b`, `c-b-mirror`; `TestRuntimeSerializedStopRetry`; `TestRuntimeStopPanicHoldsSlot` | Two physical Stops, one queued retry, dispatch-only timer, third no-op request, late return pin-only, held slot until report and mirrored late-error/nil control pass. |
| C-c | Covered | `c-c`; `TestRuntimeSelectiveSubtreeStopRetry`; `TestCoreE6ControlRetryTable` | Retry failed descendant only, acknowledged/terminal siblings excluded; exact request/command/counter and own root cause/counts pass. |
| C-d | Covered | `c-d`, `c-d-i`, `c-d-ii`, `c-d-iii`; `TestRuntimeDisposeResults`; `TestRuntimePinsBlockDisposal`; `TestOracleClosedBeforeDisposeWithReaderHeld` | Closed before disposal; failure/success/pending/Stop-pin variants, fixed safe codes/node/token, stable repeated cleanup, zero unstarted disposal, Wait/feed unchanged pass. |
| C-e | Covered | `c-e`, `c-e-undeclared`; `TestControlledAttemptPhases`; `TestScenarioInterpreterRetainsUndeclaredMismatch` | Early/repeated mutation typed errors cannot queue effects; one Started/Settled; undeclared phase error retains mismatch4 after orderly closed cleanup. Replay remains4 because declared exit oracle mismatches reconstructed success. |

### Accounting, validation and laboratory

| ID | Status | Exact test/scenario evidence (T) | Expected / observed boundary; remaining limit |
|---|---|---|---|
| W-a | Covered | `w-a`; `TestRuntimeClosedAndOpenProfiles` | Closed default/agents/commands caps admit three separate roots, B starts/notice precede A/C settlement, Inject observed and max overlap3/aggregate notices pass. This scripted dependency is not a general rank-validator proof. |
| W-b | Partially covered | `w-b-i`, `w-b-ii`; `TestRuntimeClosedAndOpenProfiles`; `TestRuntimeSerializedStopRetry` | Unsafe open and commands-saturated closed cases keep B pending through Stop acknowledgement and admit only after C report; exit1 passes. Promising consumer rejection of unclosed/unrankable mapping is not implemented/verified as autonomous Runner behavior. |
| W-c | Partially covered | `w-c-i`, `w-c-ii`; `TestScenarioPhysicalStopReturnBarriers`; `TestScenarioWCLifecycle`; `TestRuntimeHungGroupAndPendingSubtree` | Hung commands unit isolates tools, actual return precedes retry, failed gates persist and first timeout/late drain retains exit3; passes. W-c ii design oracle says X Cancelled, but fixture/test observe X Interrupted with own host_closing and one Cancelled leaf: literal design state not matched; reconciliation is deferred, no product/fixture change made. |
| W-d | Covered | `w-d`; `TestRuntimeHungGroupAndPendingSubtree` | Pending dependency subtree stop produces cancelled leaves with zero attempts, notice before waiter report, no accidental slots/physical Stops and independent group admission pass. |
| W-e | Covered | `TestCoreWeSeededAccountingClosedAndOpen` | All reject reasons/control results forced per seed; exact units/generation deltas, closed eligible progress, open global-only block, final drain and deterministic replay pass. Internal counters are intentionally inspected here. |
| V-a | Partially covered | `v-a-bad-ref`, `v-a-duplicate-ref`, `v-a-empty-ref`, `v-a-missing-ref`; `TestScenarioReferencesAndAsync`; `TestScenarioStrictValidation` | Submit/Add shared ref, bad/empty/missing refs and unknown payload/message reject before journal open with exit2. Fixture named duplicate-ref is submit+add, not two adds; separate two-Add duplicate-ref file/action case is not identified. |
| V-b | Covered | `v-b`; `TestRuntimeAddGroupsAndOutcomes`; `TestAcceptanceFixtureJournalReplaySweep` | g/h Add IDs/outcomes/one attempt, terminal late Add rejected with no record, diagnostic inputs/order and run/replay1 pass. |
| V-c | Covered | `v-c`, `v-c-empty-code`, `v-c-result-code`, `v-c-kept-code`; `TestRuntimeReportedOutcomesPrivacy`; `TestAcceptanceFixtureJournalReplaySweep` | Valid kept code, empty fallback, 65-byte/invalid/reserved raw values normalize; rejected code/error text absent; result-code variant accepted once and run/replay1 pass. |
| V-d | Partially covered | `TestReplayIncompleteMetadataAndPrefix`; `TestJournalReplayAndMutations`; `TestAcceptanceReplayRejectsAlteredRootFooter` | Unknown/rejected ref records, invalid diagnostics/request count and forged root count rejected2. Exact V-b dropped vb-h input, vb-late changed accepted and vb-g/vb-h input swap trio is not separately identified in current tests. |
| V-e | Covered | `v-e-task`, `v-e-batch`; `TestAcceptanceE1SameIdentityIngress`; `TestScenarioStrictValidation` | Same-identity equality strengthens relabelled fixtures; one accepted launch/result and replay0 pass. Strict-validation case inserts forbidden notify into raw task and rejects it, so the policy-negative is tested although not a separate frozen file. |
| L1 | Partially covered | `TestScenarioProcessL1`; `TestAcceptanceResultAndProcessPrivacyJournal` | Real helper exit7/missing/wrong-code variants yield run1/1/4 with safe codes/accepted counts; production process journal replays0. Standalone external CLI executable and Unix execution not run. |
| L2 | Partially covered | `TestScenarioProcessL2` | Real ready handshakes, own leader/descendant dead at settlement, sibling/sentinel alive and partial sibling output retained pass on Windows. External CLI and Unix tree strategy unverified. |
| L3 | Partially covered | `l3`; `TestAcceptanceFixtureJournalReplaySweep`; `TestMainEveryScenarioFixture` | Multi-root controlled Add/capacity/failure/nested stop/notices and declared exit1 pass, with production journal/replay. CLI Main API only; external command executable not run. |
| L4 | Covered | `TestAcceptanceFixtureJournalReplaySweep`; `TestReplayNeverExecutesProcessPayload`; `TestAcceptanceReplayRejectsAlteredRootFooter` | Closed root states/counts/s_r, commands/notices/cursors/sequences/Final replay; process side-effect file remains absent; distinct unfinished prefix tested. Physical fixture counters/cleanup intentionally not replayable. |
| L5 | Partially covered | `TestScenarioProcessL5`; `TestScenarioL5WithheldConfirmation`; `TestJournalInvocationCancellationDrainsClosed` | Invocation/waitbound both exit3, fresh cleanup kills own real trees while unrelated sentinel survives; controlled withheld report retains admission. External CLI interrupt behavior and Unix execution unverified. |
| L6 | Partially covered | `TestReplayL6Integrity`; `TestReplayRejectsExtraTransitionTails`; `TestReplayIncompleteMetadataAndPrefix`; `TestJournalFrozenGapWithReaderBarrier` | Removed footer/closed, internal cursor hole, version/digest change, wrong F and announced gap/write-error distinctions reject2 or incomplete5 as specified; no lost-history repair. Exact F-d compound deletion gap remains recorded under F-d. |
| AW-a | Not covered | No autonomous test for Rush worker/top-level await_tasks debt/arbiter seam | Deferred sections 9.2/9.4: selected root debt versus held delivery, worker any/all, SleepAll and child-question wake need Rush integration harness. |
| AW-b | Not covered | No autonomous test for Rush batch_stop/synchronous-result debt seam | Deferred sections 9.2/9.4: captured waiter set, independent debt, rule1/4b fences, synchronous notice consumption and elected launch are not established by Runner admission tests. |

Counts: **51 IDs = 32 covered + 17 partially covered + 2 not covered**.
Variant stems are evidence for the parent ID, not additional IDs. Covered status
is native Windows and bounded assertions only, with the shared limits above.

## Fixture capture classification

Exactly 49 paths are explicitly inventoried in the acceptance file:

- 6 invalid-loader: `v-a-bad-ref`, `v-a-duplicate-ref`, `v-a-empty-ref`,
  `v-a-missing-ref`, `v-a-unknown-message`, `v-a-unknown-payload`; exit2 and
  events file factory never invoked. No journal exists to privacy-check/replay.
- 5 declared exit3, closed: `c-d`, `c-d-ii`, `c-d-iii`, `f-e`, `w-c-ii`.
- 37 other closed fixtures, including `c-e-undeclared` with exit4.
- 1 intentionally gapped closed capture: `f-c` with a held reader/writer.
  This harness exits5/replays5; unheld fixture Main declares/exits1.
- 0 final unfinished captures in the unmodified fixture sweep. The shortened
  F-e harness produces and verifies a genuine unfinished prefix separately.

Complete captures compare run/replay footers, root states/counts/s_r, exact
last_cursor=final_cursor=closed cursor, monotonic sequence and exactly one last
closed. Deterministic construction runs twice compare entire records/footer for
`r-c-empty`, `r3-one`, `r3-chain`, `r3-add`, `r3-add-helper`, `f-d`, `v-e-task`.
No promise of identical OS/control race order is made.

Every emitted fixture journal checks canonical full payload and nonempty message
objects/arrays, recursively meaningful string data, raw/JSON-escaped sentinels,
and private field keys. Short/public-vocabulary strings and empty objects do
not constitute standalone leak evidence because of collisions. Real Summary/
Details/Inject/payload reporting runs through the production journalCapture
writer/drain/footer; Summary also reaches explicit inspection. A real process
prints an argv sentinel and its production journal remains private/replayable.
This is schema-specific absence testing, not a general information-flow proof.

## Five observed production mutation experiments

Authorized temporary mutations were run one at a time. Each Python experiment
read original bytes, asserted `original.count(old)==1`, applied one replacement,
ran the exact named race test through the wrapper, and restored original bytes
in `finally`, then asserted matching SHA-256. No compilation failure was counted
as a kill; all five failed a named acceptance assertion. No flaky retries.

Each command used this portable template, with the table's exact regex:

```sh
bash <repo-root>/.rush/audit/heavy.sh capm 2g go test -race -p 1 -parallel 1 -count=1 -timeout 900s -run '<regex>' ./internal/jobbatch/... ./cmd/batchlab/
```

| Mutant / file | Exact once replacement | Regex / assertion tail | Observed test result |
|---|---|---|---|
| M1 `internal/jobbatch/lab/scenario.go` | `sp = jobbatch.OneItem(*sp.Task)` -> `sp = jobbatch.Spec{Task: sp.Task}` | `TestAcceptanceE1SameIdentityIngress`; `ingress run failed` | rc1, wall12s; compiled. |
| M2 `internal/jobbatch/lab/journal.go` | `c.writer.Encode(r)` -> expression below | `TestAcceptanceResultAndProcessPrivacyJournal`; `journal leaked private field key "summary"` | rc1, wall11s; compiled. |
| M3 `internal/jobbatch/runner_observe.go` | `Record{Sequence: t.Sequence, Type: RecordClosed}` -> `Record{Sequence: 1, Type: RecordClosed}` | `TestAcceptanceResultAndProcessPrivacyJournal`; `nonmonotonic sequence` | rc1, wall30s, peak359MB; compiled. |
| M4 `internal/jobbatch/notice.go` | `len(n.parent.children) == 1 && t.settled[n.parent.view.ID]` -> same expression with `== 0` | `TestAcceptanceDeterministicConstructionStreams/r3-one`; `batch and sole child both selected` | rc1, wall16s; compiled. |
| M5 `internal/jobbatch/lab/replay.go` | `if !journalRootsEqual(journalSortedRoots(roots), footer.Roots) {` -> `if false && !journalRootsEqual(journalSortedRoots(roots), footer.Roots) {` | `TestAcceptanceReplayRejectsAlteredRootFooter`; `forged root counts accepted: replay exit=0 want=2` | rc1, wall14s; compiled. |

M2 replacement reads actual Runner-inspected Summary, rather than fabricating a
secret independently of the exercise:

```go
c.writer.Encode(func() any {
    if r.Type == jobbatch.RecordSettled {
        v, _ := c.runner.View(context.Background(), r.Node)
        return struct {
            jobbatch.Record
            Summary string `json:"summary"`
        }{r, v.Outcome.Summary}
    }
    return r
}())
```

Original = restored = final rechecked SHA-256 for each file:

| Mutant | SHA-256 |
|---|---|
| M1 | `0e12c8aacacc74b2f080dc10ff1ef3c55e588ae3dee5733f554e543f29ee8546` |
| M2 | `6b8cc13fcc1f7c0c821938965a280ed1f32e4b2a746bcbf3a8374921c62551cb` |
| M3 | `32cb8b096a1c03797f00dc8bb8ff4ab91d084b8dc9bb96061be908c9548c6139` |
| M4 | `657219eebacb94e294a7ac6cb260ac6dd521715cfb27dd341c030221ad1c8c44` |
| M5 | `187e4ad4d1d1dbdb42b6f29580c45fb4f65695e37b2555bedd2312e1a132f273` |

An initial M1 subprocess resolved a different bash and returned127 before any
test, with restoration verified. Explicit Git bash fixed wrapper resolution;
that infrastructure attempt is not a sixth mutant kill. Earlier acceptance
failures in chunk1 were test assumptions (F-e eventual close and C-e declared
replay exit4), corrected only in the new acceptance file.

No production/fixture edits remain from these experiments. This documentation
phase creates only the two jobbatch documents; no git writes, new product fixes,
fixtures or claims of independent whole-repository verification.
