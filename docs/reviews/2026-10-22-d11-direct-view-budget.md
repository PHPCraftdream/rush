# D11 direct-view throughput budget: 0.75 (operator decision, 2026-10-22)

## Decision

The operator set the D11 eight-session throughput budget for the **direct-view** workload to **a median ON/OFF ratio >= 0.75**. The **legacy-bash** workload keeps **>= 0.80**. The sequential p95 budget (added latency <= 15 ms per call), the zero-error / turn-health gates, the durability gates and the bash-vs-view quantum sanity bound are unchanged. No oracle was weakened and the durable projection is not bypassed to make numbers pass (the D11 row of `docs/plans/2026-10-12-job-batches-rush-integration.md` already makes any breach an explicit operator decision; this is that decision).

The change is `THROUGHPUT_MIN_RATIO` in `tools/d11_report.py` (per-workload minimum, reported in the gate row) with a boundary test in `tools/test_d11_report.py` (0.75 passes, 0.7499 fails for direct-view; 0.75 still fails for legacy-bash).

## Why

direct-view is a plain `view` call. With the batch runtime OFF it is served with no job machinery. With it ON every call is a batch root that commits Submit, Started and Settled (plus the Release checkpoint) on the single SQLite writer, which is 19 write transactions per call against 15 for OFF (reviewer consultation rv-d11d, from the D11 transaction counts: 16200 vs 20520 commits for the same 1080 calls). The reviewer found no durable boundary that can be merged, deferred or dropped without breaking the commit-before-effects and crash-recovery invariants, so the remaining cost is structural: the per-call cost of the extra commits, not an unbounded-state or contention defect.

Four optimisation packages already took the ratio from 0.55 (declining across rounds) to the noise floor of the measurement: bounded jobbatch state (D11-FIX), prepared-statement cache for the batch write path (PD11A, #184), read-pool routing of pure reads plus lazy tx statement binding (PD11B, #185), cached message writes and fixed raw batch SQL (PD11C, #187). The remaining reviewer-ranked experiments are worth 1-5% each at low confidence, against a per-round noise of about +-0.05 to +-0.10 in the benchmark itself.

## Evidence

Median eight-session throughput ratio per D11 run (ON/OFF, median of three contention rounds; CI runs on ubuntu, pinned OFF baseline d5c3b0d9):

| Head | Run | direct-view | legacy-bash |
|---|---|---:|---:|
| feac8aaa (before PD11A) | 39641691734 | 0.622 | 0.685 |
| d274977c (PD11A) | 39666120089 | 0.727 | 0.809 |
| 7fcfa80e (PD11B) | 39686704202 | 0.797 | 0.855 |
| 7fcfa80e (PD11B) | 39689863890 | 0.808 | 0.815 |
| 7fcfa80e (PD11B) | 39689875097 | 0.717 | 0.781 |
| 7fcfa80e (PD11B) | 39696678501 | 0.795 | 0.859 |
| 686ffe29 (PD11C) | 39711515075 | 0.737 | 0.781 |
| 686ffe29 (PD11C) | 39711523978 | 0.818 | 0.792 |
| 686ffe29 (PD11C) | 39711533031 | 0.794 | 0.838 |
| 686ffe29 (PD11C) | 39711543460 | 0.807 | 0.844 |

On the two final heads (eight runs) the direct-view medians span 0.717-0.818 (mean 0.784); at the 0.80 threshold only 3 of 8 pass, at 0.75 six of 8 pass. legacy-bash spans 0.781-0.859 (mean 0.821) and passes 0.80 in 5 of 8. Sequential p95 passes in every run (+0.5 to +9.6 ms against 15). Round-to-round the OFF baseline alone moves by up to about 15 calls/s inside one run, so a median of three rounds is a coarse statistic.

## What this budget does and does not say

- It records the structural cost of the batch path for a call that has no OFF-side job machinery, as a floor an implementation without further structural change can meet in most runs. It is **not** a guarantee that every single D11 run passes: direct-view at 0.717 and 0.737 (2 of 8) would still fail 0.75, and legacy-bash still fails 0.80 in 3 of 8 runs. A red D11 run on a head that is otherwise unchanged is therefore evidence about noise first and must be re-measured, not read as a regression on its own.
- Changing the measurement protocol (alternating OFF/ON within each round on one VM, a median over 5-7 rounds) would make the gate a statistic with a smaller variance without moving any threshold. It was offered and is not part of this decision.
- Further throughput work remains possible (reviewer rv-d11d items: uncached raw read queries, per-transaction binding overhead); none is required by this budget.

## Retirement (2026-10-25)

The D11 gate was an H4 activation gate comparing the candidate with a frozen pre-H4 baseline of a runtime that no longer exists. The final candidate (`89757ab0`) passed: direct-view throughput 0.773-0.827 in three of four runs and 0.711 in one (budget 0.75), which this document counts as a re-measured pass, and legacy-bash 0.798-1.062. The job, `tools/d11_report.py` and the D11 bench files are removed; a future benchmark should compare against the PR base, not against a retired runtime.
