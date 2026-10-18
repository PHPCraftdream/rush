# D11 per-call cost — diagnosis (tasks #1424, rounds 1-4)

Round 1 (2026-10-18): evidence mining + static audit + CI profiling tooling
(run 39143396851). Round 2: profiles landed (run 39152597249, job
117749121024), decided the hypotheses, first production fixes. Round 3
(same day): snapshot-consistent live read, growth-dependent UPDATE audited,
duplicate notices read removed, tests re-bound — but the round-3 status was
OVERCLAIMED: the post-hoc review (P2-C) returned FAIL (five compile defects,
an unsafe plain path bypassing the transaction, tests disconnected from the
production call shape). Round 4 (same day, supersedes round 3): compile
blockers fixed; the transactional dispatch moved INSIDE BatchLiveForSessions
so callers cannot bypass it; the two remaining scans (#4/#16) eliminated
schema-free WITHOUT a migration; tests re-bound to production (driver-level
SQL capture, public-entry interleaving, TxStarter-layer counting,
whole-result order). Round 3 status was overclaimed; round 4 supersedes it.

EXECUTED-EVIDENCE STATEMENT: formatting (gofumpt/gofmt) and the static
.githooks guards were run locally every round, including round 4; what never
ran locally in any round is compilation, tests or benchmarks (the operator
regime forbids them on this machine). The profile numbers are CI artifacts; every plan class below is a
STATIC reading of the schema's indexes and predicates — "static trace; CI
pending". The CI dispatch (build + vet + lint + the EXPLAIN-plan and behavior
tests) is the only oracle that confirms them; no claim here is verified before CI
runs.

## 1. Evidence (runs 39143396851 / 39152597249)

Direct-view workload, 3 rounds x (200 sequential + 160 contention calls),
fresh sessions every round:

| Phase | OFF p50/p95 | ON p50/p95 | ON commits | OFF commits |
|---|---|---|---|---|
| sequential-1 | 24.5 / 33.3 ms | 32.6 / 42.4 ms | 3600 | 3000 |
| sequential-2 | 24.5 / 29.5 ms | 33.1 / 44.6 ms | 3600 | 3000 |
| sequential-3 | 24.1 / 29.8 ms | 37.8 / 54.7 ms | 3600 | 3000 |
| contention-1 (cps) | 94.9 ms (83.2) | 145.0 ms (54.2) | 2880 | 2400 |
| contention-2 (cps) | 92.1 ms (84.9) | 169.1 ms (47.2) | 2880 | 2400 |
| contention-3 (cps) | 91.0 ms (86.2) | 201.6 ms (39.4) | 2880 | 2400 |

Per-round state (ON): batch_nodes 792/1584/2376 (OFF: 0); WAL ~4.2 MB flat
both; goroutines 25 vs 21 flat; heap 16.2->24.1 MB ON vs 12.9->14.6 MB OFF.
CPU: 44.78 s ON vs 31.82 s OFF samples (+41%), the extra almost all SQL:
VdbeExec 8.8 s vs 2.5 s; prepare 6.2 s vs 3.7 s. Hot ON chain:
pullPendingNotices 5.21 s -> PullPendingSessionNotices 4.78 s;
onStepFinish 4.89 s. No jobbatch.Engine/Runner node reaches the top-50 cum
cutoff (0.22 s). Mutex 425 ms total; block profile idle.

Round-1 facts that still stand: ON decays per round, OFF flat; fresh
sessions per round; the commit delta is exactly +3 per call, constant — the
submit/started/settled checkpoint transactions, each contract-required as a
DISTINCT committed transaction (app_batch_bench_helpers_test.go:187-189,
tools/d11_report.py:100-103) and not mergeable (the reservation must be
durable before dispatch). Untouched.

## 2. Statement re-audit — onStepFinish / pull / turn-facts (round 4)

Plan classes: SEARCH-idx = served by a listed index (bounded); SCAN = full
batch_nodes read (cost grows with total rows). Partial indexes only serve a
query whose WHERE implies the index's WHERE — in practice the partial
predicate's terms must appear verbatim; `x IN ('pending','done')` does NOT
supply `x != 'void'`.

Every qualifying step finish (persistStepFinish ->
BatchReactionMarkReactedWithMessageUpdate, one tx):

| # | Statement | Plan | Grows? |
|---|---|---|---|
| 1 | batchOutboxReactNodesVoidAware (batch_reaction.go:81) | SEARCH idx_batch_nodes_debt (verbatim terms, round-2 fix) | no |
| 2 | GetMessage + messages.UpdateTx (PK) | SEARCH PK | no |
| 3 | MarkSessionNoticesReactedForOwner (owner) | SEARCH idx_session_notices_owner | no |
| 4 | batchOutboxClearFailureServed (batch_reaction.go:130, call site :310): owner's root IDs via batchClearFailureRootsSQL (idx_batch_nodes_owner_roots, partial node_id=root_id), then chunked UPDATE ... root_id IN (...) served from idx_batch_nodes_parent's root prefix; row set IDENTICAL (composite FK: every node of a root shares the root's owner) | SEARCH both legs | no — bounded by the owner's OWN roots, not by total table rows |
| 5 | ClearSessionNoticesReactedFailedForOwner (owner) | SEARCH idx_session_notices_owner | no |

Honest tradeoff on #4: a few more small statements per qualifying step
finish (one roots SELECT + one UPDATE per <=500 roots, batchReadChunk) in
exchange for removing the table scan. The net per-call cost is a measured
quantity for the next CI dispatch, not a static claim.

Pull (3x/call: turn start agent_turn.go:384 + every step boundary
agent_turn_step.go:113):

| # | Statement | Plan | Grows? |
|---|---|---|---|
| 6 | batchOutboxPendingVoidAware (owner + notice_selected + delivery='pending') | SEARCH idx_batch_nodes_pending_notice; r-leg PK seek | no |
| 7 | BatchOutboxPull CAS + NameMessage | SEARCH PK + idx_batch_nodes_parent prefix | no |
| 8 | pending session_notices | SEARCH idx_session_notices_owner_pending | no |
| 9 | live-scope read — ONLY when a supervision notice is pending (round-2 lazy fix) | two-step in ONE read tx (reader.TxStarter): idx_batch_nodes_owner_roots + idx_batch_nodes_parent | no |

Turn facts (1x/call, readTurnFacts -> readTurnFactsBatch):

| # | Statement | Plan | Grows? |
|---|---|---|---|
| 10 | batchDebtNodesVoidAware (owner + notice_selected + wake + reacted + delivery != 'void' verbatim) | SEARCH idx_batch_nodes_debt | no |
| 11 | BatchOutboxDebtNotices (owner) | SEARCH idx_session_notices_owner; read ONCE per call (round-3 dedupe) | no |
| 12 | foreignLiveDriver / DurableChild / RunningDelegation | session-scoped host infra; delegation reads skipped for root sessions | no |
| 13 | LiveWorkSummary — only when SleepAll is armed | same tx two-step as #9 (reader.TxStarter) | no |
| 14 | CaptureVisibleDebt (drain legs) -> both debt queries | as #10/#11 | no |
| 15 | AttemptRows/MaxWakeAttempts (failure accounting) | GetBatchNode per ref, PK | no |
| 16 | batchOutboxFailedNodesServed (batch_reaction.go:479, call site :538): same owner-roots rewrite as #4; Go-side sort reproduces the original ORDER BY (root_id, notice_sequence, notice_ordinal, node_id) including NULLS-FIRST (nullInt64Less, :446) | SEARCH both legs | no (failure paths only, rare) |

Round-2's claim that the existing indexes suffice for the hot paths is
WITHDRAWN: statements #4 and #16 had no applicable index and scanned
batch_nodes. Round 4 removed both scans schema-free — no migration is
requested anymore. No other growth-dependent statement remains on these
paths. Readers without a TxStarter (CLI inspection: app_batch_cli.go:35 and
the four internal/cmd readers) fall back to the single-statement join —
consistent by construction, scan-class, out of the hot path.

## 3. Fixes in the tree (rounds 2-4), each with its mutation-catching test

Each entry: what changed (file:line), the test, the STATIC TRACE
(entry point -> first failing assertion), and the ONE production mutation it
catches. Only what the trace proves is claimed.

1. Transactional dispatch inside the public entry
   (internal/session/batch_read_live.go:41-73): BatchLiveForSessions itself
   begins ONE read-only tx via reader.TxStarter and runs all fetches inside
   it (rollback after); on errBatchLiveRootVanished it retries ONCE on a
   fresh snapshot; a second occurrence stays incomplete — never silently
   complete. Callers cannot bypass the dispatch. Test:
   TestBatchLiveServedReadIsSnapshotConsistent
   (batch_read_live_served_test.go:157) drives BOTH public entries
   (BatchLiveForOwner and BatchLiveForSessions) through the test-only seam
   (batchLiveServedSeam) with a second raw connection purging root A
   mid-read. MUTATION: routing the dispatch back to a nontransactional
   two-step helper lets the purge land between the statements -> root A
   vanishes -> vanish error or wrong root identity -> the Complete/HasLive/
   identity assertions fail. STATIC TRACE:
   BatchLiveForSessions -> seam -> the complete/identity require.* lines.
2. Global ordering (batch_read_live.go:256-261 area): the served helper
   sorts ALL owner entries together by (created_at, root_id) instead of
   concatenating per owner. Test: TestBatchLiveNodesForOwnersServedMatchesJoin
   (batch_read_live_served_test.go:90) require.Equal's the WHOLE result
   slice against the single-statement join oracle, with REVERSED owner
   input and deterministic interleaved created_at (100/200/300/400).
   MUTATION: per-owner concatenation diverges on the interleaved fixture ->
   the whole-slice equal fails. The direct helper call is deliberate: the
   public map API groups per owner and cannot express global order.
3. Verbatim partial-index terms (batch_reaction.go:51-59 debt const
   batchDebtNodesVoidAwareSQL; :73-84 batchOutboxReactNodesVoidAware; react
   executed in the same tx position): `delivery != 'void'` spelled out so
   idx_batch_nodes_debt is usable. Result sets unchanged. Test:
   TestBatchHotStatementsPlanWithoutScans (served_test.go:180). MUTATION:
   dropping either verbatim term makes the SCAN assertion fail. STATIC
   TRACE: TestBatchHotStatementsPlanWithoutScans -> the per-statement SCAN
   require.
4. Schema-free scan elimination #4 — batchOutboxClearFailureServed
   (batch_reaction.go:130, call site :310): owner roots via
   idx_batch_nodes_owner_roots (partial node_id = root_id), then chunked
   UPDATE ... root_id IN (...) served from idx_batch_nodes_parent's root
   prefix; chunks of batchReadChunk (500). Row set IDENTICAL via the
   composite FK (every node of a root shares the root's owner_session_id).
   MUTATION: reverting the call site to the generated owner-wide UPDATE
   captures that UPDATE in the plan test — its plan scans batch_nodes ->
   fails (see trace 6 below).
5. Schema-free scan elimination #16 — batchOutboxFailedNodesServed
   (batch_reaction.go:479, call site :538): same owner-roots rewrite; rows
   sorted Go-side to reproduce the original ORDER BY exactly, including
   NULLS-FIRST for the two nullable columns (nullInt64Less :446). Failure
   paths only, rare.
6. Tests bound to production (driver-level capture): the plan test
   EXPLAINs statements CAPTURED at the driver level from the real live read
   and the real reaction tx (the driver.Connector shim in
   batch_sqllog_test.go: it wraps the connector to record every statement
   and forward BeginTx) — no copied SQL remains for the hot statements.
   MUTATIONS + STATIC TRACES:
   - Reverting the live dispatch to the Source JOIN captures the join
     statement -> its plan scans batch_nodes ->
     TestBatchHotStatementsPlanWithoutScans fails.
   - Reverting the ClearFailure call site to the generated owner-wide
     UPDATE captures that UPDATE -> SCAN -> the same test fails.
   - TestBatchPendingInclusiveDebtRowsReadsNoticesOnce
     (batch_reaction_dedupe_test.go:19) counts "FROM session_notices" +
     "wake = 1" statements over the WHOLE public
     BatchReactionPendingInclusiveDebtRows call at the driver layer:
     a duplicate read in the wrapper OR the helper -> count 2 -> fails.
   - The lazy-lookup tests (batch_cutover_pull_lazy_test.go) count BeginTx
     on the reader's TxStarter (batchPullCountingTxStarter) — the exact
     handle the production dispatch calls. Begins: 0/0/1/1 across the four
     tests. MUTATION: restoring the unconditional LiveWorkSummary ->
     begins 0->1 -> the two zero-call tests fail. STATIC TRACE:
     TestBatchPullPendingNoticesSkipsLiveReadWhenEmpty -> require.Zero
     (counting.begins).
   - TestBatchDescendantsLiveUsesTxDispatch
     (batch_descendants_tx_test.go:16): the descendant closure crosses the
     same transactional dispatch — begins 1 then 2.
7. Lazy live-scope read (batch_cutover_facts_node.go:93-102 liveResult via
   the reader only; comment :450-456): the pull consults the live scope
   only for a pending SUPERVISION row; liveResult routes through
   BatchLiveForOwner, so the TxStarter wiring applies. Covered by the
   counting tests above.

### Schema-free rewrite (round 4)

Candidates considered for #4/#16 and their verdicts:

- (a) idx_batch_nodes_debt — cannot serve: its partial WHERE requires
  reacted = 0 while failed rows always carry reacted = 1
  (batchOutboxAccountNode sets both together).
- (b) keying off the session_notices failure markers — reacted_failed lives
  only on batch_nodes; no relation exists to join the notices index onto
  the node predicate.
- (c) narrowing the UPDATE predicate to indexable terms — rejected: changes
  which rows are cleared (a semantics change).
- (d) owner-root enumeration + root_id IN (...) — WORKS and is implemented
  (fixes 4-5 above): same row set via the composite FK, per-owner bounded,
  chunked at batchReadChunk.

Per-statement plan classes remain STATIC claims; the CI EXPLAIN test
(TestBatchHotStatementsPlanWithoutScans) asserts them against the real
planner.

### Honest remaining gaps

- (i) app_batch_cli.go:35 and the four internal/cmd readers keep the join
  fallback — correct-but-scan-class, CLI inspection only.
- (ii) actual planner choices, benchmark deltas, and the per-statement CPU
  split remain CI-measured.
- (iii) the equality of the served rewrite's row set to the generated
  statements rests on the composite-FK argument plus the equivalence/plan
  tests, not on a runtime diff.

## 4. Hypothesis verdicts (profiles, run 39152597249)

- Growth-dependent per-call SQL: CONFIRMED primary cost (section 2).
- H1 engine/Runner walks: DEMOTED — absent from the top-50 cum nodes; the
  +4 MB/round heap is consistent with retained roots. internal/jobbatch is
  owned by the jb/d11-fix branch; out of scope here.
- H2 WAL/checkpoint stalls: REFUTED — WAL flat ~4.2 MB both modes.
- H3 goroutine/timer accumulation: REFUTED — goroutines flat (25 vs 21).

## 5. What remains unproven / deferred

- The served ClearFailure/FailedNodes rewrite's measured per-call cost vs
  the removed scans (bounded-vs-growing tradeoff) is unproven until the
  next profiled dispatch.
- The per-statement CPU split inside the pull chain (the -top -cum 50-node
  cap truncated intermediates); the next profiled dispatch measures the fix
  effect directly.
- Whether SQLite would have inferred `delivery = 'done'` implies
  `!= 'void'` for the generated ReactNodes (making the verbatim term a
  no-op): the term makes it moot; the CI EXPLAIN test settles it.
- The remaining ~7 ms sequential ON-vs-OFF delta after the scans are gone:
  attribute from the next dispatch before touching anything else.
- Prepared-statement reuse (prepare 6.2 s vs 3.7 s): deferred. The writer
  pool is a single connection (internal/db/connect.go), so per-pool
  *sql.Stmt caching is plausible, but safety cannot be demonstrated without
  running code. Follow-up measured round.
- H4 (fresh-store schema consolidation): unchanged, not required here.

## QUESTIONS

1. H4 stays on its own track?
2. Prepared-statement reuse: authorize a follow-up round where CI measures
   a prototype first?
3. The CLI inspection readers (app_batch_cli.go:35, internal/cmd/*)
   deliberately keep the join fallback — accept as-is, or wire their
   TxStarter too?
