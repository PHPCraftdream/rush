# D11 per-call cost — diagnosis tooling + static audit (task #1424)

Round: 2026-10-18 diagnosis (no local execution permitted; benchmarks and
profiles run in GitHub CI only). Evidence input: dispatch run 39143396851
(d11-evidence artifact), mined statically. Tooling added this round: CPU /
mutex / block profiles + pprof tops in the `d11-benchmark` job, and per-round
cumulative-state log lines from the benchmark itself. Production code is NOT
changed in this round; see section 6 for why.

## 1. What the evidence run shows (all numbers from run 39143396851)

Direct-view workload (200 sequential + 160 contention calls per round, 3
rounds, fresh sessions every round):

| Phase | OFF p50/p95 | ON p50/p95 | ON commits | OFF commits |
|---|---|---|---|---|
| sequential-1 | 24.5 / 33.3 ms | 32.6 / 42.4 ms | 3600 | 3000 |
| sequential-2 | 24.5 / 29.5 ms | 33.1 / 44.6 ms | 3600 | 3000 |
| sequential-3 | 24.1 / 29.8 ms | 37.8 / 54.7 ms | 3600 | 3000 |
| contention-1 (cps) | 94.9 ms (83.2) | 145.0 ms (54.2) | 2880 | 2400 |
| contention-2 (cps) | 92.1 ms (84.9) | 169.1 ms (47.2) | 2880 | 2400 |
| contention-3 (cps) | 91.0 ms (86.2) | 201.6 ms (39.4) | 2880 | 2400 |

Key shapes:

- The candidate decays round over round; the baseline is flat (its cps even
  improves 83.2 -> 86.2). The sequential p50 quartile medians shift as whole
  blocks: ON round 1 quartile 1 = 31.6 ms vs round 3 quartile 1 = 36.4 ms,
  and the per-round MINIMUM rises with it (23.3 -> 28.7 ms). This is a
  uniform per-call slowdown growing with cumulative calls, not tail noise.
- Sessions are fresh every round (4 sequential + 8 contention per round), so
  per-session history stays bounded: whatever grows is cumulative process or
  store state, not session history.
- The commit delta is EXACTLY +3 per call in every phase, constant:
  warmup-seq 364 vs 304 (+60/20 calls), warmup-conc 296 vs 248 (+48/16),
  sequential 3600 vs 3000 (+600/200), contention 2880 vs 2400 (+480/160).
  The +3 does NOT grow; the per-call latency does. The decay therefore lives
  in per-transaction/per-operation cost growth, not in more transactions.

## 2. The +3 commits per call are proven REQUIRED (not mergeable)

Each flag-ON call commits exactly three batch checkpoint transactions:

1. Submit reservation: `batchCommitSubmit` (internal/session/batch_commit.go:89)
   -> `store.ReserveIdentity` (batch_commit.go:119) — one transaction: the
   `ReserveBatchNode` insert (internal/db/sql/batch_nodes.sql:4) plus the
   `batch_runner_checkpoints` upsert, written through `BatchProjection`'s
   checkpoint parameter (batch_commit.go:123-127).
2. Started projection: `batchCommitProject` (batch_commit.go:244) ->
   `store.Project` (batch_commit.go:345) — one transaction:
   `ProjectBatchNode` + checkpoint upsert (batch_commit.go:330-335).
3. Settled projection: same shape, plus `SelectBatchNodeNotice`
   (batch_nodes.sql:82) and the response-envelope columns
   (batch_commit.go:304-322) in the same transaction.

The D11 evidence contract REQUIRES these to be three distinct committed
transactions: `validateCalls` fails on "merged checkpoint events in one
transaction" (internal/app/app_batch_bench_helpers_test.go:187-189) and
`validate_call_transactions` fails on "merged checkpoint transactions" /
"one transaction contains multiple checkpoint event kinds for a call"
(tools/d11_report.py:100-103).

Merging would also change durability semantics: the submit reservation must
be durable before executor dispatch (a crash after dispatch without a durable
reservation leaves an undiscoverable root), started marks the dispatched
attempt, settled persists terminal state + envelope + notice selection
atomically. Conclusion: the +3 is the DESIGNED, budgeted cost. The defect to
chase is the growth of per-operation cost around it (sections 4-5).

## 3. Static audit of one flag-ON call — statements, tables, indexes, growth

Call origin: `AgentCoordinator.Run` (internal/agent/coordinator.go:535),
`WithCallOrigin(OriginCLI)` — the rush-run origin, both modes.

Flag-ON per-call sequence (each step: statements -> table -> index -> grows
with cumulative rows?):

| Step | Statements (file:line) | Table.Index | Growth |
|---|---|---|---|
| attach probe | `BatchProbeLive` -> `GetLiveBatchRoot` (batch_nodes.sql:38) | batch_nodes.idx_batch_nodes_live_key (owner, tool_call_id) partial live | No — partial index drops settled rows; owner scope fresh per round |
| engine Submit -> commit hook tx 1 | `ReserveBatchNode` (batch_nodes.sql:4) + checkpoint upsert | batch_nodes PK node_id; batch_runner_checkpoints PK (host_id, runner_id) | No — keyed writes |
| Started -> commit hook tx 2 | `ProjectBatchNode` (batch_nodes.sql:66) + checkpoint | PK + (root_id, node_id, host_id, runner_id) fence | No |
| Settled -> commit hook tx 3 | `ProjectBatchNode` + `SelectBatchNodeNotice` (batch_nodes.sql:82) + envelope + checkpoint | same | No |
| delivery wait | in-process `Batch.Wait` (runner.go:151) or durable `LoadDone` -> `BatchDeliveryOneItemCompletion` (keyed read) | batch_nodes by root/node | No |
| per-turn facts read | `readTurnFactsBatch` (batch_cutover_facts.go:483) -> `PendingInclusiveDebt` (batch_cutover_facts_node.go:63) -> `BatchOutboxDebtNodes` + `BatchOutboxDebtNotices` (batch_outbox.sql) | idx_batch_nodes_debt (owner, root) partial; idx_session_notices_debt partial | No — owner = fresh session |
| notice pull / ack | `BatchOutboxPending`, `BatchOutboxPull`, `BatchOutboxNameMessage` (batch_outbox.sql) | idx_batch_nodes_pending_notice partial + PK rechecks | No — owner scope |
| reaction/drain accounting | `BatchOutboxReactNodes`, `BatchOutboxAccountNode/Notice`, `BatchOutboxClearFailure`, `BatchOutboxFailedNodes` (batch_outbox.sql) | owner-scoped; owner partial indexes | No — owner scope |
| retention | `BatchCountExpired`/`BatchPurgeExpired` (internal/session/batch_retention.go:12-46) | full root scan by cutoff | Not on the per-call path: 7-day GC only, invoked by GC commands |
| async_jobs | never read flag-ON (batch_cutover_facts.go:1-8) | — | — |

Index sources: internal/db/migrations/20261012000001_batch_projection.sql
(all six batch_nodes indexes), 20261012000002_batch_runner_incarnation.sql
(checkpoints PK), 20260928000002_add_async_phase4_core.sql
(idx_session_notices_owner_pending/_debt/_retention),
20260929000001_add_session_notices_owner_index.sql (plain owner index),
20260812000001_add_orphan_call_outbox.sql (status, session_id).

Verdict: NO missing index on any per-call predicate. Every per-call
statement is PK- or owner-scoped, and fresh sessions keep owner scopes
bounded (~55 roots per sequential session, ~22 per contention session).
Baseline issues the same message-machinery commits both modes (15/call);
flag-ON is 15 + 3 checkpoints = 18/call. The store layer does NOT explain
the decay by itself.

## 4. Ranked hypotheses

### H1 (PROVEN defect; magnitude unproven): settled roots are never released for ordinary calls — engine/runner per-event cost grows linearly with cumulative calls

- The ONLY production trigger of `Runner.Release` is the root-lease Done
  watcher: `RootLeases.adopt(root, batch.Done(), ..., release.Release)`
  (internal/agent/batch_root_lease.go:79-95). It is armed ONLY when
  `rootOwned` — i.e. only when `inv.RegistryID != ""`
  (internal/agent/batch_ingress.go:160-213, gate at 197; the model-control
  twin has the same gate, batch_model_control.go:121-125). Ordinary view/
  bash calls (no disk-provider registry identity) never Release. The code
  documents this as intentional: "Settled roots stay RETAINED by the engine
  by design (design §1.2)" (batch_ingress.go:142-144).
- Retained roots are walked on EVERY engine event. `Engine.Apply` runs, per
  event (internal/jobbatch/engine.go:56-173): the settle loop over
  `e.roots` (engine.go:136-140), `admit`'s candidate scan over `e.roots`
  (internal/jobbatch/admission.go:71-90, recursion in admission.go:48-63),
  `selectNotices`' full-tree walk of every non-released root
  (internal/jobbatch/notice.go:3-31), and the `settledViews` full-forest
  recursion (engine.go:148-161). `e.roots` is append-only (engine.go:68);
  `EventRelease` removes nodes from `e.nodes` (internal/jobbatch/tree.go:86)
  but NEVER shrinks `e.roots`.
- Three Applies per call (Submit, Started, Settled) x ~4 walks x cumulative
  roots: ~400 retained roots by the end of round 1, ~1200 by round 3 — the
  exact linear-in-cumulative-calls shape the decay shows, present in BOTH
  the sequential and contention phases.
- Same-chain unbounded memory: `batchRunnerAdapter.started` (internal/agent/
  batch_install_adapters.go:82) and `.owners` (internal/agent/
  batch_cli_runtime.go:42-43) are never deleted; `Runner.released`
  (internal/jobbatch/runner.go:323) grows forever; retained roots'
  `attempts` entries are deleted only by the EventRelease prune
  (runner.go:325-332); `feed.known` is cleared only at runner close
  (runner.go:274).
- Estimated walk cost by round 3 is real but likely ~0.2-1 ms/call —
  probably NOT the whole +5 ms sequential p50 growth and not obviously the
  whole contention halving. PROFILES MUST CONFIRM the CPU share
  (Engine.Apply/admit/selectNotices/settledViews cum %, per round) before
  the retention-policy change is made.

### H2 (needs profile + per-round WAL data): write-path saturation and WAL/checkpoint dynamics under +20% serialized write volume

- All batch commits serialize through the Runner's single committer
  goroutine and loop (runner.go:138-143 committer; runner.go:218-246
  publication/commit gating): 8-way contention makes that loop the
  throughput ceiling, and the candidate pushes 18 vs 15 commits/call
  (+20%) through it ON TOP of the ordinary writer.
- A flat +20% would give a flat ~0.83 ratio; the DECAY needs per-commit
  cost to grow. Candidate mechanism: WAL frames accumulate while the
  auto-checkpoint (default 1000 pages) cannot complete against 8 active
  readers; checkpoint attempts then copy an ever-larger backlog, stalling
  every writer. The new per-round `wal_bytes`/`shm_bytes` logs and the
  block/mutex profiles (modernc sqlite commit path) decide this.

### H3 (cheap to falsify with the new logs): goroutine or timer accumulation

- Known per-call goroutine: the root-lease Done watcher
  (batch_root_lease.go:83-94) — exits at Done, bounded. Per-call tickers
  are defers-stopped (batch_install_adapters.go:182-183). The per-round
  `goroutines=` log line falsifies or confirms this in one dispatch.

### H4 (separate track, NOT a perf fix): fresh-store schema consolidation

Not needed for any finding above (no additive index is required). See
QUESTIONS.

## 5. What the next dispatch must confirm (instrumentation -> hypothesis)

| Signal | Decides |
|---|---|
| per-round `D11 ROUND n STATE:` log lines (both modes): batch_nodes/messages growth vs goroutines vs heap_inuse vs wal_bytes | H1 growth present; H3 leak; H2 WAL backlog |
| candidate CPU pprof `-top -cum` per workload/round: share of Engine.Apply / admit / selectNotices / settledViews vs modernc sqlite commit/wal/checkpoint | H1 vs H2 weight |
| candidate mutex/block pprof: contention on the writer conn / committer channel | H2 serialization |
| baseline CPU pprof (same symbols) | the OFF twin should show none of the batch symbols |

## 6. Changed this round; NOT changed and why

Changed (tooling only):

- .github/workflows/job-batches-shadow.yml (d11-benchmark job only,
  dispatch-only triggers untouched, LF preserved): baseline workloads gain
  `-cpuprofile`, candidate workloads gain `-cpuprofile -mutexprofile
  -blockprofile -mutexprofilefraction 1`, all written to $RUNNER_TEMP; new
  "Print pprof tops" step (always(), non-failing) prints
  `go tool pprof -top -cum -nodecount=50` and `-top -flat -nodecount=40`
  per profile into delimited job-log sections; the d11-evidence artifact
  additionally uploads `d11-*pprof`.
- internal/app/app_batch_bench_helpers_test.go: `d11Env.dir` field +
  `d11LogRoundState` (row counts for batch_nodes, batch_runner_checkpoints,
  session_notices, orphan_call_outbox, messages, sessions; db/wal/shm file
  sizes; runtime.NumGoroutine; HeapInuse) — logged once per round via
  t.Logf; d11_report.py ignores these lines and the summary JSON schema is
  UNCHANGED (tools/d11_report.py and tools/test_d11_report.py untouched).
- internal/app/app_batch_bench_test.go: one `d11LogRoundState(t, e, round)`
  call at the end of each round.

NOT changed (production), deliberately:

- The +3 checkpoint transactions: required by the txid evidence contract and
  by durability semantics (section 2) — merging would relax correctness, and
  the budget is never relaxed.
- Engine/Runner retention policy (H1): the walk cost is proven to grow, but
  its MAGNITUDE versus the budget miss is unproven, and retention is a
  documented design decision (batch_ingress.go:142-144). Planned fix, to be
  executed ONLY after the profiles confirm the CPU share: (a) arm the
  Done->Release adoption for EVERY accepted root (use `SubmitBound`
  unconditionally in `batchRuntime.submit`, always arm `adopt`; for
  non-disk roots the lease release must be skipped and only the runner
  Release fired — batchRootLeaseStore.adopt needs an empty-registry branch),
  guaranteeing exactly-once so the "batch root disposal after settlement
  failed" slog.Error (batch_root_lease.go:88-91) can never fire (it would
  trip the D11 health gate); (b) OR, smaller and purely internal: compact
  `e.roots` on EventRelease (swap-remove released tails) and delete
  `Runner.released` entries — bounded walks even with retention unchanged;
  (c) prune `batchRunnerAdapter.started` post-Done — safe for `AwaitDone`
  because Done closes only AFTER the settled commit is durable
  (runner.go:234-246 publish order) — but `owners`/`CLIOwner` must stay
  answerable for late control requests (commitCLIControl routes on it,
  batch_cli_runtime.go:84), so owners need a policy tied to the control
  answer window, not Done.
- No index added: none is missing (section 3).

## QUESTIONS

1. H4 (single fresh-store schema replacing the migrations; the
   internal/db/testdata/store_v1 fixture and the legacymigrations copy must
   stay consistent) — out of scope for this round and NOT required by any
   finding here. Confirm whether it should be scheduled as its own round.
2. H1 fix shape: root Release for every accepted root (changes the
   documented §1.2 retention behavior) versus the purely internal
   e.roots compaction (keeps retention, bounds the walks). The profiles
   from the next dispatch should pick; operator sign-off required for (a)
   because it reverses a documented design note.
