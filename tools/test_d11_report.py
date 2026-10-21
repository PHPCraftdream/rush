import json
import os
import statistics
import sys
import tempfile
import unittest
from unittest import mock

TOOLS_DIR = os.path.dirname(os.path.abspath(__file__))
if TOOLS_DIR not in sys.path:
    sys.path.insert(0, TOOLS_DIR)

import d11_report  # noqa: E402

SHA = d11_report.PINNED
CANDIDATE = "a" * 40
ROUNDS = (1, 2, 3)
MEASURED_CALLS = len(ROUNDS) * 4


def transactions(call_ids):
    records = []
    tx = 1
    for call_id in call_ids:
        for event in d11_report.EVENTS:
            records.append({"call_id": call_id, "root_id": "root-" + call_id, "event": event, "transaction": tx})
            tx += 1
    return records


def run(workload, phase, values, calls_per_sec=100.0, commits=None, transitions=0):
    return {
        "workload": workload, "phase": phase, "samples": len(values), "attempts": len(values), "errors": 0,
        "p50_ms": d11_report.percentile(values, 50) if values else 0.0,
        "p95_ms": d11_report.percentile(values, 95) if values else 0.0,
        "p99_ms": d11_report.percentile(values, 99) if values else 0.0,
        "calls_per_sec": calls_per_sec if phase.startswith("contention") else 0.0,
        "commits": commits if commits is not None else len(values),
        "submit_writes": transitions, "started_writes": transitions, "settled_writes": transitions,
        "raw_ms": values,
    }


def find_run(summary, phase):
    return next(entry for entry in summary["runs"] if entry["phase"] == phase)


def refresh(summary):
    measured = [entry for entry in summary["runs"] if entry["phase"].startswith(("sequential", "contention"))]
    for entry in summary["runs"]:
        values = entry["raw_ms"]
        if values:
            entry["p50_ms"] = d11_report.percentile(values, 50)
            entry["p95_ms"] = d11_report.percentile(values, 95)
            entry["p99_ms"] = d11_report.percentile(values, 99)
    sequential = [value for entry in measured if entry["phase"].startswith("sequential") for value in entry["raw_ms"]]
    contention_runs = [entry for entry in measured if entry["phase"].startswith("contention")]
    contention = [value for entry in contention_runs for value in entry["raw_ms"]]
    summary["p50_ms"] = d11_report.percentile(sequential, 50)
    summary["p95_ms"] = d11_report.percentile(sequential, 95)
    summary["p99_ms"] = d11_report.percentile(sequential, 99)
    summary["p95_contention_ms"] = d11_report.percentile(contention, 95)
    summary["throughput_cps"] = sum(entry["calls_per_sec"] for entry in contention_runs) / len(contention_runs)
    summary["sequential_attempts"] = sum(entry["attempts"] for entry in measured if entry["phase"].startswith("sequential"))
    summary["contention_attempts"] = sum(entry["attempts"] for entry in contention_runs)
    summary["errors"] = sum(entry["errors"] for entry in measured)
    summary["commits"] = sum(entry["commits"] for entry in measured)
    for key in ("submit_writes", "started_writes", "settled_writes"):
        summary[key] = sum(entry[key] for entry in measured)


def mode_summary(mode, workload, rounds=ROUNDS):
    call_ids = ["call-%d" % n for n in range(len(rounds) * 4)] if mode == "candidate" else []
    transitions_count = len(rounds) * 4 if mode == "candidate" else 0
    runs = []
    per = transitions_count // (2 * len(rounds))
    for r in rounds:
        runs.append(run(workload, "warmup-seq-%d" % r, [1.0, 2.0], commits=0))
        runs.append(run(workload, "sequential-%d" % r, [10.0] * 2, transitions=per if mode == "candidate" else 0))
        runs.append(run(workload, "warmup-conc-%d" % r, [1.0, 2.0], commits=0))
        runs.append(run(workload, "contention-%d" % r, [12.0] * 2, calls_per_sec=100.0, transitions=per if mode == "candidate" else 0))
    summary = {
        "mode": mode, "workload": workload, "candidate_sha": CANDIDATE, "baseline_sha": SHA,
        "measured_call_ids": call_ids, "call_transactions": transactions(call_ids),
        "root_call_ids": {call_id: "root-" + call_id for call_id in call_ids},
        "p50_ms": 0, "p95_ms": 0, "p99_ms": 0, "p95_contention_ms": 0,
        "throughput_cps": 0, "errors": 0, "commits": 0,
        "sequential_attempts": 0, "contention_attempts": 0,
        "submit_writes": 0, "started_writes": 0, "settled_writes": 0,
        "health_violations": 0, "off_wait_path": "test: cli origin, production wait",
        "durability": {"journal_mode": "wal", "synchronous": "NORMAL"},
        "runs": runs,
    }
    refresh(summary)
    return summary


def pair(workload="legacy-bash"):
    baselines = {name: mode_summary("baseline", name) for name in d11_report.WORKLOADS}
    candidates = {name: mode_summary("candidate", name) for name in d11_report.WORKLOADS}
    return baselines, candidates


class D11ReportTests(unittest.TestCase):
    # Trace: inclusive +15ms delta and a median-of-rounds ratio gate; mutate only legacy-bash to prove independent gates.
    def test_both_workloads_budget_independence(self):
        baselines, candidates = pair()
        for workload in d11_report.WORKLOADS:
            find_run(candidates[workload], "sequential-2")["raw_ms"] = [25.0] * 2
            find_run(candidates[workload], "contention-1")["calls_per_sec"] = 90.0
            find_run(candidates[workload], "contention-2")["calls_per_sec"] = 79.0
            refresh(candidates[workload])
        result = d11_report.evaluate(baselines, candidates)
        # delta exactly 15.0 passes; ratios 0.9/0.79/0.9 have median 0.9 which passes.
        self.assertTrue(result["gates"]["direct-view.sequential_p95"])
        self.assertTrue(result["gates"]["direct-view.throughput"])
        self.assertTrue(result["gates"]["legacy-bash.sequential_p95"])
        self.assertTrue(result["gates"]["legacy-bash.throughput"])
        find_run(candidates["legacy-bash"], "sequential-2")["raw_ms"] = [25.01] * 2
        find_run(candidates["legacy-bash"], "contention-3")["calls_per_sec"] = 79.0
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        # delta 15.01 fails; ratios 0.9/0.79/0.79 have median 0.79 which fails.
        self.assertTrue(result["gates"]["direct-view.sequential_p95"])
        self.assertTrue(result["gates"]["direct-view.throughput"])
        self.assertFalse(result["gates"]["legacy-bash.sequential_p95"])
        self.assertFalse(result["gates"]["legacy-bash.throughput"])

    # Trace: remove one call, duplicate an event, merge transaction IDs, alter root, and duplicate transitions; all evidence mutations must fail closed.
    def test_transaction_evidence_rejects_missing_duplicate_merged_and_wrong_root(self):
        mutations = []
        baselines, candidates = pair(); candidates["direct-view"]["call_transactions"].pop(); mutations.append((baselines, candidates))
        baselines, candidates = pair(); candidates["direct-view"]["call_transactions"][0]["transaction"] = candidates["direct-view"]["call_transactions"][1]["transaction"]; mutations.append((baselines, candidates))
        baselines, candidates = pair(); candidates["direct-view"]["call_transactions"][0]["root_id"] = "wrong"; mutations.append((baselines, candidates))
        baselines, candidates = pair(); candidates["direct-view"]["call_transactions"].append(dict(candidates["direct-view"]["call_transactions"][0])); mutations.append((baselines, candidates))
        baselines, candidates = pair(); candidates["direct-view"]["measured_call_ids"][0] = "missing-call"; mutations.append((baselines, candidates))
        for baseline, candidate in mutations:
            with self.subTest(candidate=candidate["direct-view"]):
                with self.assertRaises(ValueError): d11_report.evaluate(baseline, candidate)

    # Trace: two actual calls must not claim one root; workloads must share the same candidate source SHA.
    def test_shared_root_and_cross_workload_sha_rejected(self):
        baselines, candidates = pair()
        records = candidates["direct-view"]["call_transactions"]
        for record in records:
            if record["call_id"] == "call-1": record["root_id"] = "root-call-0"
        with self.assertRaisesRegex(ValueError, "share one root"):
            d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        baselines["legacy-bash"]["candidate_sha"] = "b" * 40
        candidates["legacy-bash"]["candidate_sha"] = "b" * 40
        with self.assertRaisesRegex(ValueError, "different candidate SHAs"):
            d11_report.evaluate(baselines, candidates)

    # Trace: a checkpoint transaction carrying two event kinds for one call is rejected even if all three event names exist.
    def test_transaction_rejects_two_event_kinds_in_one_commit(self):
        baselines, candidates = pair()
        records = candidates["direct-view"]["call_transactions"]
        records[1]["transaction"] = records[0]["transaction"]
        with self.assertRaisesRegex(ValueError, "merged checkpoint"):
            d11_report.evaluate(baselines, candidates)

    # Trace: fewer than three matched contention rounds, or mismatched phase names, are rejected.
    def test_throughput_requires_three_matched_rounds(self):
        baselines, candidates = pair()
        for summary in (baselines["legacy-bash"], candidates["legacy-bash"]):
            summary["runs"] = [entry for entry in summary["runs"] if not entry["phase"].endswith("-3")]
            refresh(summary)
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        find_run(candidates["legacy-bash"], "contention-3")["phase"] = "contention-x"
        refresh(candidates["legacy-bash"])
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)

    # Trace: evaluate -> evaluate_pair per workload -> validate(baseline) first
    # (d11_report.py:212-213). Two-round summaries rebuilt with matching
    # measured_call_ids (8 identities = 8 attempts) would pass the candidate
    # identity check at :208, but they never get that far: the round gate at
    # :180-181 raises "expected at least three sequential and three contention
    # rounds" during BASELINE validation, before the candidate is validated at
    # :214. What the test catches: a validator reorder/regression letting a
    # sub-3-round summary through the :180-181 gate when its identities are
    # internally consistent. Under that mutation the test still fails, but with
    # a different message: evaluate_pair's own gate at :223-224 fires
    # "contention rounds must match across modes with at least three rounds"
    # (2 < 3), so the assertRaisesRegex below — which pins the :180-181
    # message — goes red. The assertion is correct as written.
    def test_minimum_rounds_gate_with_consistent_identities(self):
        baselines = {name: mode_summary("baseline", name, rounds=(1, 2)) for name in d11_report.WORKLOADS}
        candidates = {name: mode_summary("candidate", name, rounds=(1, 2)) for name in d11_report.WORKLOADS}
        with self.assertRaisesRegex(ValueError, "at least three sequential and three contention rounds"):
            d11_report.evaluate(baselines, candidates)

    # Trace: a median round ratio of exactly 0.80 passes the inclusive gate; anything below fails.
    def test_throughput_median_ratio_exact_boundary(self):
        baselines, candidates = pair()
        for r in ROUNDS:
            find_run(candidates["legacy-bash"], "contention-%d" % r)["calls_per_sec"] = 80.0
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertTrue(result["gates"]["legacy-bash.throughput"])  # median(0.8, 0.8, 0.8) = 0.8 >= .8
        self.assertEqual(0.8, result["workloads"]["legacy-bash"]["throughput_ratio_median"])
        baselines, candidates = pair()
        for r in ROUNDS:
            find_run(candidates["legacy-bash"], "contention-%d" % r)["calls_per_sec"] = 79.99
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertFalse(result["gates"]["legacy-bash.throughput"])  # median ratio 0.7999 < .8

    # Trace: the throughput gate keys on the median round ratio, not the mean.
    def test_median_ratio_gate_uses_median(self):
        baselines, candidates = pair()
        find_run(candidates["legacy-bash"], "contention-1")["calls_per_sec"] = 50.0
        find_run(candidates["legacy-bash"], "contention-2")["calls_per_sec"] = 100.0
        find_run(candidates["legacy-bash"], "contention-3")["calls_per_sec"] = 100.0
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertTrue(result["gates"]["legacy-bash.throughput"])  # median(0.5, 1.0, 1.0) = 1.0
        self.assertEqual(1.0, result["workloads"]["legacy-bash"]["throughput_ratio_median"])
        baselines, candidates = pair()
        find_run(candidates["legacy-bash"], "contention-1")["calls_per_sec"] = 50.0
        find_run(candidates["legacy-bash"], "contention-2")["calls_per_sec"] = 50.0
        find_run(candidates["legacy-bash"], "contention-3")["calls_per_sec"] = 130.0
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertFalse(result["gates"]["legacy-bash.throughput"])  # median(0.5, 0.5, 1.3) = 0.5
        item = result["workloads"]["legacy-bash"]
        self.assertEqual(statistics.median([r["ratio"] for r in item["throughput_rounds"]]), item["throughput_ratio_median"])
        self.assertEqual(3, len(item["throughput_rounds"]))
        self.assertEqual(min(r["ratio"] for r in item["throughput_rounds"]), item["throughput_ratio_worst"])

    # Trace: any nonzero health violation count fails validation in either mode.
    def test_health_violations_rejected(self):
        baselines, candidates = pair()
        candidates["direct-view"]["health_violations"] = 1
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        baselines["legacy-bash"]["health_violations"] = 2
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)

    # Trace: off_wait_path must be present, nonempty, and identical across all four summaries.
    def test_off_wait_path_required_and_shared(self):
        baselines, candidates = pair()
        baselines["direct-view"].pop("off_wait_path")
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        baselines["direct-view"]["off_wait_path"] = ""
        with self.assertRaises(ValueError): d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        candidates["legacy-bash"]["off_wait_path"] = "different"
        with self.assertRaisesRegex(ValueError, "call origin / wait path"):
            d11_report.evaluate(baselines, candidates)
        baselines, candidates = pair()
        baselines["legacy-bash"]["off_wait_path"] = "baseline-only-disagreement"
        with self.assertRaisesRegex(ValueError, "wait path"):
            d11_report.evaluate(baselines, candidates)

    # Trace: a 100 ms polling quantum in either mode must trip the cross-workload p50 bound of 20 ms.
    def test_bash_quantum_gate(self):
        baselines, candidates = pair()
        result = d11_report.evaluate(baselines, candidates)
        self.assertIn("bash_quantum_off", result["gates"])
        self.assertIn("bash_quantum_on", result["gates"])
        self.assertTrue(result["gates"]["bash_quantum_off"])
        self.assertTrue(result["gates"]["bash_quantum_on"])
        baselines, candidates = pair()
        find_run(baselines["legacy-bash"], "sequential-1")["raw_ms"] = [30.0] * 2
        find_run(baselines["legacy-bash"], "sequential-2")["raw_ms"] = [30.0] * 2
        find_run(baselines["legacy-bash"], "sequential-3")["raw_ms"] = [30.0] * 2
        refresh(baselines["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertEqual(20.0, result["bash_quantum_off_ms"])
        self.assertTrue(result["gates"]["bash_quantum_off"])  # inclusive boundary
        find_run(baselines["legacy-bash"], "sequential-1")["raw_ms"] = [30.01] * 2
        find_run(baselines["legacy-bash"], "sequential-2")["raw_ms"] = [30.01] * 2
        find_run(baselines["legacy-bash"], "sequential-3")["raw_ms"] = [30.01] * 2
        refresh(baselines["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertFalse(result["gates"]["bash_quantum_off"])
        baselines, candidates = pair()
        for r in ROUNDS:
            find_run(candidates["legacy-bash"], "sequential-%d" % r)["raw_ms"] = [30.01] * 2
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
        self.assertFalse(result["gates"]["bash_quantum_on"])

    # Trace: mutate SHA, workload, scalar, durability, sample and aggregate data independently.
    def test_sha_workload_numbers_durability_samples_and_totals(self):
        mutations = []
        baseline, candidate = pair(); candidate["direct-view"]["candidate_sha"] = "b" * 40; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["legacy-bash"]["workload"] = "direct-view"; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["p50_ms"] = float("nan"); mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["durability"] = {}; mutations.append((baseline, candidate))
        baseline, candidate = pair(); find_run(candidate["direct-view"], "sequential-1")["samples"] = 0; mutations.append((baseline, candidate))
        baseline, candidate = pair(); find_run(candidate["direct-view"], "warmup-seq-1")["errors"] = 1; mutations.append((baseline, candidate))
        for baseline, candidate in mutations:
            with self.assertRaises(ValueError): d11_report.evaluate(baseline, candidate)

    # Trace: malformed marker blocks are rejected regardless of summary data.
    def test_malformed_markers(self):
        texts = ("no markers", d11_report.BEGIN + "\n{}\n", (d11_report.BEGIN + "\n{}\n" + d11_report.END + "\n") * 2)
        for text in texts:
            with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False) as stream:
                stream.write(text); path = stream.name
            try:
                with self.assertRaises(ValueError): d11_report.extract_summary(path)
            finally:
                os.unlink(path)

    # Trace: report renders the per-round throughput table, provenance lines, health row, and the FAIL marker.
    def test_report_shows_each_workload_and_failure(self):
        baselines, candidates = pair()
        for r in ROUNDS:
            find_run(candidates["legacy-bash"], "sequential-%d" % r)["raw_ms"] = [26.0] * 2
        refresh(candidates["legacy-bash"])
        report = d11_report.render(d11_report.evaluate(baselines, candidates))
        self.assertIn("direct-view", report); self.assertIn("legacy-bash", report)
        self.assertIn("same trivial bash command", report); self.assertIn("raw_ms", report); self.assertIn("FAIL", report)
        self.assertIn("Median ratio", report); self.assertIn("Pinned baseline", report)
        self.assertIn("Call origin", report); self.assertIn("Turn-health", report)

    # Trace: the step-summary rendering drops the raw samples and the merged JSON (GITHUB_STEP_SUMMARY caps at 1 MiB) but keeps every gate row.
    def test_summary_rendering_omits_raw_evidence_and_keeps_gate_rows(self):
        baselines, candidates = pair()
        result = d11_report.evaluate(baselines, candidates)
        full, compact = d11_report.render(result), d11_report.render(result, evidence=False)
        self.assertIn("raw_ms", full); self.assertIn("Merged JSON", full)
        self.assertNotIn("raw_ms", compact); self.assertNotIn("Merged JSON", compact); self.assertNotIn("Raw run evidence\n", compact)
        self.assertIn("Median ratio", compact); self.assertIn("Turn-health", compact); self.assertIn("Throughput calls/s", compact)
        self.assertLess(len(compact), len(full))

    # Trace: --summary-out writes the compact form next to the full report, and the full report keeps the raw evidence.
    def test_cli_writes_compact_summary_next_to_full_report(self):
        with tempfile.TemporaryDirectory() as tmp:
            baselines, candidates = pair()
            summaries = [baselines["direct-view"], candidates["direct-view"], baselines["legacy-bash"], candidates["legacy-bash"]]
            paths = [os.path.join(tmp, "%d.log" % i) for i in range(4)]
            for path, summary in zip(paths, summaries):
                with open(path, "w", encoding="utf-8") as stream:
                    stream.write(d11_report.BEGIN + "\n" + json.dumps(summary) + "\n" + d11_report.END + "\n")
            report, compact = os.path.join(tmp, "report.md"), os.path.join(tmp, "summary.md")
            with mock.patch.object(sys, "argv", ["d11_report", *paths, "-o", report, "--summary-out", compact]):
                d11_report.main()
            with open(report, encoding="utf-8") as stream: full_text = stream.read()
            with open(compact, encoding="utf-8") as stream: compact_text = stream.read()
            self.assertIn("Merged JSON", full_text); self.assertNotIn("Merged JSON", compact_text)
            self.assertIn("Median ratio", compact_text); self.assertLess(len(compact_text), len(full_text))

    # Trace: four-input CLI emits both reports and structured invalid artifact when a workload log is absent or malformed.
    def test_cli_writes_four_workload_logs_and_failure_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            baselines, candidates = pair()
            for r in ROUNDS:
                find_run(candidates["legacy-bash"], "sequential-%d" % r)["raw_ms"] = [26.0] * 2
            refresh(candidates["legacy-bash"])
            summaries = [baselines["direct-view"], candidates["direct-view"], baselines["legacy-bash"], candidates["legacy-bash"]]
            paths = [os.path.join(tmp, "%d.log" % i) for i in range(4)]
            for path, summary in zip(paths, summaries):
                with open(path, "w", encoding="utf-8") as stream:
                    stream.write(d11_report.BEGIN + "\n" + json.dumps(summary) + "\n" + d11_report.END + "\n")
            report, merged = os.path.join(tmp, "report.md"), os.path.join(tmp, "merged.json")
            with mock.patch.object(sys, "argv", ["d11_report", *paths, "-o", report, "--json-out", merged]):
                self.assertEqual(1, d11_report.main())
            with open(report, encoding="utf-8") as stream: self.assertIn("legacy-bash", stream.read())
            with open(merged, encoding="utf-8") as stream:
                result = json.load(stream)
            self.assertTrue(result["gates"]["direct-view.sequential_p95"])
            self.assertFalse(result["gates"]["legacy-bash.sequential_p95"])
            with open(paths[3], "w", encoding="utf-8") as stream: stream.write("bad log")
            with mock.patch.object(sys, "argv", ["d11_report", *paths, "-o", report, "--json-out", merged]):
                self.assertEqual(1, d11_report.main())
            with open(merged, encoding="utf-8") as stream: failure = json.load(stream)
            self.assertEqual("invalid", failure["status"]); self.assertIn("reason", failure)


if __name__ == "__main__":
    unittest.main()
