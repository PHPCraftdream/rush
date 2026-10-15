import json
import os
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


def transactions(call_ids):
    records = []
    tx = 1
    for index, call_id in enumerate(call_ids):
        for event in d11_report.EVENTS:
            records.append({"call_id": call_id, "root_id": "root-" + call_id, "event": event, "transaction": tx})
            tx += 1
    return records


def run(workload, phase, values, calls_per_sec=100.0, commits=None, transitions=0):
    return {
        "workload": workload, "phase": phase, "samples": len(values), "attempts": len(values), "errors": 0,
        "p50_ms": d11_report.percentile(values, 50), "p95_ms": d11_report.percentile(values, 95),
        "p99_ms": d11_report.percentile(values, 99),
        "calls_per_sec": calls_per_sec if phase.startswith("contention") else 0.0,
        "commits": commits if commits is not None else len(values),
        "submit_writes": transitions, "started_writes": transitions, "settled_writes": transitions,
        "raw_ms": values,
    }


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


def mode_summary(mode, workload):
    call_ids = ["call-%d" % n for n in range(8)] if mode == "candidate" else []
    transitions_count = 8 if mode == "candidate" else 0
    summary = {
        "mode": mode, "workload": workload, "candidate_sha": CANDIDATE, "baseline_sha": SHA,
        "measured_call_ids": call_ids, "call_transactions": transactions(call_ids),
        "root_call_ids": {call_id: "root-" + call_id for call_id in call_ids},
        "p50_ms": 0, "p95_ms": 0, "p99_ms": 0, "p95_contention_ms": 0,
        "throughput_cps": 0, "errors": 0, "commits": 0,
        "sequential_attempts": 0, "contention_attempts": 0,
        "submit_writes": 0, "started_writes": 0, "settled_writes": 0,
        "durability": {"journal_mode": "wal", "synchronous": "NORMAL"},
        "runs": [run(workload, "warmup", [1.0, 2.0], commits=0),
                 run(workload, "sequential-1", [10.0] * 4, commits=12, transitions=transitions_count // 2),
                 run(workload, "contention-1", [12.0] * 4, calls_per_sec=100.0, commits=12, transitions=transitions_count - transitions_count // 2)],
    }
    refresh(summary)
    return summary


def pair():
    baselines = {workload: mode_summary("baseline", workload) for workload in d11_report.WORKLOADS}
    candidates = {workload: mode_summary("candidate", workload) for workload in d11_report.WORKLOADS}
    return baselines, candidates


class D11ReportTests(unittest.TestCase):
    # Trace: each workload is accepted at inclusive +15ms/.80 boundaries; mutate only legacy-bash to prove independent gates.
    def test_both_workloads_budget_independence(self):
        baselines, candidates = pair()
        for workload in d11_report.WORKLOADS:
            baselines[workload]["runs"][1]["raw_ms"] = [10.0] * 4
            candidates[workload]["runs"][1]["raw_ms"] = [25.0] * 4
            candidates[workload]["runs"][2]["calls_per_sec"] = 80.0
            refresh(baselines[workload]); refresh(candidates[workload])
        result = d11_report.evaluate(baselines, candidates)
        self.assertTrue(result["gates"]["direct-view.sequential_p95"])
        self.assertTrue(result["gates"]["legacy-bash.throughput"])
        candidates["legacy-bash"]["runs"][1]["raw_ms"] = [25.01] * 4
        candidates["legacy-bash"]["runs"][2]["calls_per_sec"] = 79.99
        refresh(candidates["legacy-bash"])
        result = d11_report.evaluate(baselines, candidates)
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

    # Trace: mutate SHA, workload, scalar, durability, sample and aggregate data independently.
    def test_sha_workload_numbers_durability_samples_and_totals(self):
        mutations = []
        baseline, candidate = pair(); candidate["direct-view"]["candidate_sha"] = "b" * 40; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["legacy-bash"]["workload"] = "direct-view"; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["p50_ms"] = float("nan"); mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["durability"] = {}; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["runs"][1]["samples"] = 0; mutations.append((baseline, candidate))
        baseline, candidate = pair(); candidate["direct-view"]["runs"][0]["errors"] = 1; mutations.append((baseline, candidate))
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

    # Trace: one workload budget failure is visible while the peer workload remains passing and raw evidence remains rendered.
    def test_report_shows_each_workload_and_failure(self):
        baselines, candidates = pair()
        candidates["legacy-bash"]["runs"][1]["raw_ms"] = [26.0] * 4
        refresh(candidates["legacy-bash"])
        report = d11_report.render(d11_report.evaluate(baselines, candidates))
        self.assertIn("direct-view", report); self.assertIn("legacy-bash", report)
        self.assertIn("same trivial bash command", report); self.assertIn("raw_ms", report); self.assertIn("FAIL", report)

    # Trace: four-input CLI emits both reports and structured invalid artifact when a workload log is absent or malformed.
    def test_cli_writes_four_workload_logs_and_failure_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            baselines, candidates = pair()
            candidates["legacy-bash"]["runs"][1]["raw_ms"] = [26.0] * 4
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
