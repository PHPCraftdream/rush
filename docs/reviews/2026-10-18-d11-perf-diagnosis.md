# D11 per-call cost — diagnosis (tasks #1424, rounds 1-3)

Round 1 (2026-10-18): evidence mining + static audit + CI profiling tooling
(run 39143396851). Round 2: profiles landed (run 39152597249, job
117749121024), decided the hypotheses, first production fixes. Round 3
(same day): review findings closed — snapshot-consistent live read, the
missed growth-dependent UPDATE audited and routed to DECISION NEEDED, the
duplicate notices read removed, tests re-bound to production mutations.

EXECUTED-EVIDENCE STATEMENT: nothing was executed locally in any round (the
operator regime forbids compilation and tests on this machine). The profile
numbers are CI artifacts; every plan class below is a STATIC reading of the
schema's indexes and predicates. The CI dispatch (build + vet + lint + the
new EXPLAIN-plan and behavior tests) is the only oracle that confirms them.

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

## 2. Statement re-audit — onStepFinish / pull / turn-facts (round 3)

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
| 4 | BatchOutboxClearFailure: owner + reacted_failed=1 (batch_outbox.sql:59-61, called batch_reaction.go:230) | SCAN batch_nodes | YES |
| 5 | ClearSessionNoticesReactedFailedForOwner (owner) | SEARCH idx_session_notices_owner | no |

Pull (3x/call: turn start agent_turn.go:384 + every step boundary
agent_turn_step.go:113):

| # | Statement | Plan | Grows? |
|---|---|---|---|
| 6 | batchOutboxPendingVoidAware (owner + notice_selected + delivery='pending') | SEARCH idx_batch_nodes_pending_notice; r-leg PK seek | no |
| 7 | BatchOutboxPull CAS + NameMessage | SEARCH PK + idx_batch_nodes_parent prefix | no |
| 8 | pending session_notices | SEARCH idx_session_notices_owner_pending | no |
| 9 | live-scope read — ONLY when a supervision notice is pending (round-2 lazy fix) | two-step in ONE read tx: idx_batch_nodes_owner_roots + idx_batch_nodes_parent | no |

Turn facts (1x/call, readTurnFacts -> readTurnFactsBatch):

| # | Statement | Plan | Grows? |
|---|---|---|---|
| 10 | batchDebtNodesVoidAware (owner + notice_selected + wake + reacted + delivery != 'void' verbatim) | SEARCH idx_batch_nodes_debt | no |
| 11 | BatchOutboxDebtNotices (owner) | SEARCH idx_session_notices_owner; read ONCE per call (round-3 dedupe) | no |
| 12 | foreignLiveDriver / DurableChild / RunningDelegation | session-scoped host infra; delegation reads skipped for root sessions | no |
| 13 | LiveWorkSummary — only when SleepAll is armed | same tx two-step as #9 | no |
| 14 | CaptureVisibleDebt (drain legs) -> both debt queries | as #10/#11 | no |
| 15 | AttemptRows/MaxWakeAttempts (failure accounting) | GetBatchNode per ref, PK | no |
| 16 | BatchOutboxFailedNodes: owner + notice_selected + reacted_failed=1 (batch_outbox.sql:78-80, failure paths only) | SCAN batch_nodes | YES (rare) |

Round-2's claim that the existing indexes suffice for the hot paths is
WITHDRAWN: statements #4 and #16 have no applicable index and scan
batch_nodes; #4 runs on every qualifying step finish even when it changes
zero rows. No other growth-dependent statement remains on these paths.

## 3. Fixes in the tree (rounds 2+3), each with its mutation-catching test

1. Lazy live-scope read (batch_cutover_facts_node.go:445-449, 485-495): the
   pull reads pending session_notices first and consults the live scope only
   for a pending SUPERVISION row. Test: TestBatchPullPendingNoticesSkipsLiveReadWhenEmpty + ...DeliversWithoutLiveRead (batch_cutover_pull_lazy_test.go) — restoring the unconditional read makes the counting Source spy see >= 1 call and the `require.Zero` assertions fail. Semantics: openScope is consumed only by the supervision void; both reads were outside any transaction before and after.
2. Index-served live read (batch_read_live.go:215-246): the join (SCAN) is
   replaced by two index-served statements per owner chunk —
   ListLiveBatchRootsForOwner (idx_batch_nodes_owner_roots) + ListBatchNodes
   per root (idx_batch_nodes_parent). Test: TestBatchLiveNodesForOwnersServedMatchesJoin — dropping the settled-root exclusion, breaking per-root ordering, or missing rows fails the set/order assertions against the join oracle on a real schema (several owners, several live roots each).
3. Snapshot consistency (batch_read_live.go:43-59, 248-256): the two-step
   read runs inside ONE read-only transaction (BatchLiveForSessionsTx on the
   store writer; WAL deferred read tx). This restores the old single-statement join's snapshot semantics: a root that settles or is purged mid-read keeps its rows, a root submitted mid-read stays invisible. (A single restored statement was impossible without touching generated sqlc code: *db.Queries exposes no arbitrary-SQL escape hatch.) Busy/locked path: one deferred read tx on the same single-connection writer the live reads already share — no new lock class, same busy_timeout. An empty per-root node result cannot occur inside the tx: a root row is its own first node, and a purge by another connection cannot move the tx's snapshot. Test: TestBatchLiveServedReadIsSnapshotConsistent (batch_read_live_served_test.go:139) — a test-only seam (batchLiveServedSeam, batch_read_live.go:17-19) sits exactly between the root list and the node reads; a second raw connection submits root B and purges root A there. MUTATION: removing the transaction (the pre-fix shape) lets the purge land between the statements -> A's node read is empty, B was never listed -> HasLive/Own assertions fail.
4. Verbatim partial-index terms (batch_reaction.go:51-59 debt const
   batchDebtNodesVoidAwareSQL; :73-84 batchOutboxReactNodesVoidAware; react
   executed at :230 in the same tx position): `delivery != 'void'` spelled
   out so idx_batch_nodes_debt is usable (`IN` does not imply the
   inequality). Result sets unchanged (the IN term already fixed delivery to
   pending/done; 'done' already excludes 'void'). Test:
   TestBatchHotStatementsPlanWithoutScans (batch_read_live_served_test.go:164) explains the PRODUCTION constants — dropping either verbatim term makes the SCAN assertion fail. (The plan text itself is CI-verified; static analysis says the index is used.)
5. Duplicate notices read removed (batch_reaction.go:164-200): the
   pending-inclusive read now executes the nodes query and the notices query
   EXACTLY once each inside its read tx (the previous shape read the
   notices twice — refs via batchDebtSnapshot, kinds via a second query).
   Test: TestBatchPendingInclusiveDebtRowsReadsNoticesOnce
   (batch_reaction_dedupe_test.go) — a counting DBTX sniffs the statement
   text; restoring the second read makes the count two and fails.

Honest coverage limits: reverting the *db.Queries dispatch back to the
Source interface path (batch_read_live.go:28-31) or the react call site back
to the generated query is behavior-identical SQL-wise and is caught by the
next profiled CI dispatch, not by a unit test; everything else above has a
direct failing test.

## 4. Hypothesis verdicts (profiles, run 39152597249)

- Growth-dependent per-call SQL: CONFIRMED primary cost (section 2).
- H1 engine/Runner walks: DEMOTED — absent from the top-50 cum nodes; the
  +4 MB/round heap is consistent with retained roots. internal/jobbatch is
  owned by the jb/d11-fix branch; out of scope here.
- H2 WAL/checkpoint stalls: REFUTED — WAL flat ~4.2 MB both modes.
- H3 goroutine/timer accumulation: REFUTED — goroutines flat (25 vs 21).

## 5. What remains unproven / deferred

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

## DECISION NEEDED (operator): additive partial index

No schema-free fix exists for statements #4/#16: rows with reacted_failed=1
always carry reacted=1 (batchOutboxAccountNode sets both together), so
idx_batch_nodes_debt — whose partial WHERE requires reacted = 0 — can never
serve them, and no other batch_nodes index leads with owner for these
predicates. Narrowing the UPDATE's predicate to indexable terms would change
which rows it clears (a semantics change). The code therefore stays on the
current schema-free shape, and the fix requires a migration:

    -- Purely additive; tiny (only failure-marked rows are indexed).
    -- Serves BatchOutboxClearFailure (#4) and BatchOutboxFailedNodes (#16).
    CREATE INDEX idx_batch_nodes_reacted_failed ON batch_nodes (owner_session_id)
        WHERE reacted_failed = 1;

If approved, this migration MUST be mirrored consistently in
internal/db/testdata/store_v1/*.sql and internal/db/legacymigrations/ (the
drift test pins the latter) — H4 fold-in note: if the fresh-store schema
consolidation lands first, this index belongs inside that single schema
instead of a new migration file. Do NOT add it without operator sign-off;
until then statements #4/#16 stay SCAN-class and are re-measured per
dispatch.

## QUESTIONS

1. Approve idx_batch_nodes_reacted_failed (DECISION NEEDED above), or accept
   the scans on #4/#16 with their measured cost?
2. H4 stays on its own track?
3. Prepared-statement reuse: authorize a follow-up round where CI measures
   a prototype first?
