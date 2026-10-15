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


def run(phase, values, calls_per_sec=100.0, commits=None, transitions=0):
    return {
        "phase": phase, "samples": len(values), "attempts": len(values), "errors": 0,
        "p50_ms": d11_report.percentile(values, 50),
        "p95_ms": d11_report.percentile(values, 95),
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


def mode_summary(mode):
    measured_calls = 8
    transitions = measured_calls if mode == "candidate" else 0
    summary = {
        "mode": mode, "candidate_sha": CANDIDATE, "baseline_sha": SHA,
        "p50_ms": 0, "p95_ms": 0, "p99_ms": 0, "p95_contention_ms": 0,
        "throughput_cps": 0, "errors": 0, "commits": 0,
        "sequential_attempts": 0, "contention_attempts": 0,
        "submit_writes": 0, "started_writes": 0, "settled_writes": 0,
        "durability": {"journal_mode": "wal", "synchronous": "NORMAL"},
        "runs": [run("warmup", [1.0, 2.0], commits=0),
                 run("sequential-1", [10.0, 10.0, 10.0, 10.0], commits=12, transitions=transitions // 2),
                 run("contention-1", [12.0, 12.0, 12.0, 12.0], calls_per_sec=100.0, commits=12, transitions=transitions - transitions // 2)],
    }
    refresh(summary)
    return summary


class D11ReportTests(unittest.TestCase):
    def pair(self):
        return mode_summary("baseline"), mode_summary("candidate")

    # Trace: construct raw p95 values at the inclusive +15 ms and .80 throughput boundaries, then exceed each limit.
    def test_threshold_boundaries_and_slowdown_mutations(self):
        baseline, candidate = self.pair()
        baseline["runs"][1]["raw_ms"] = [10.0] * 4
        candidate["runs"][1]["raw_ms"] = [25.0] * 4
        candidate["runs"][2]["calls_per_sec"] = 80.0
        refresh(baseline); refresh(candidate)
        gates = d11_report.evaluate(baseline, candidate)["gates"]
        self.assertTrue(gates["sequential_p95"]); self.assertTrue(gates["throughput"])
        candidate["runs"][1]["raw_ms"] = [25.01] * 4; refresh(candidate)
        self.assertFalse(d11_report.evaluate(baseline, candidate)["gates"]["sequential_p95"])
        candidate["runs"][1]["raw_ms"] = [25.0] * 4
        candidate["runs"][2]["calls_per_sec"] = 79.99; refresh(candidate)
        self.assertFalse(d11_report.evaluate(baseline, candidate)["gates"]["throughput"])

    # Trace: mutate invalid scalar numbers and a required field; derived metric mismatch must reject forged summary p95.
    def test_bad_numbers_missing_fields_and_forged_aggregate(self):
        for value in (True, float("nan"), float("inf"), -1, "10"):
            baseline, candidate = self.pair(); candidate["p50_ms"] = value
            with self.assertRaises(ValueError): d11_report.evaluate(baseline, candidate)
        baseline, candidate = self.pair(); del candidate["p99_ms"]
        with self.assertRaises(ValueError): d11_report.evaluate(baseline, candidate)
        baseline, candidate = self.pair(); candidate["p95_ms"] = 999
        with self.assertRaisesRegex(ValueError, "raw run evidence"): d11_report.evaluate(baseline, candidate)

    # Trace: mutate SHA, transitions, durability, samples, phase totals and warmup errors independently.
    def test_sha_transition_durability_samples_and_totals(self):
        mutations = []
        baseline, candidate = self.pair(); candidate["candidate_sha"] = "b" * 40; mutations.append((baseline, candidate))
        baseline, candidate = self.pair(); candidate["submit_writes"] = 7; mutations.append((baseline, candidate))
        baseline, candidate = self.pair(); candidate["durability"] = {}; mutations.append((baseline, candidate))
        baseline, candidate = self.pair(); candidate["runs"][1]["samples"] = 0; mutations.append((baseline, candidate))
        baseline, candidate = self.pair(); candidate["runs"][1]["commits"] += 1; mutations.append((baseline, candidate))
        baseline, candidate = self.pair(); candidate["runs"][0]["errors"] = 1; mutations.append((baseline, candidate))
        for baseline, candidate in mutations:
            with self.assertRaises(ValueError): d11_report.evaluate(baseline, candidate)

    # Trace: supply absent, unterminated and duplicate summary markers; extraction must reject every ambiguous artifact.
    def test_malformed_markers(self):
        texts = ("no markers", d11_report.BEGIN + "\n{}\n",
                 (d11_report.BEGIN + "\n{}\n" + d11_report.END + "\n") * 2)
        for text in texts:
            with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False) as stream:
                stream.write(text); path = stream.name
            try:
                with self.assertRaises(ValueError): d11_report.extract_summary(path)
            finally:
                os.unlink(path)

    # Trace: budget regression must render FAIL while retaining raw per-run evidence.
    def test_human_report_failure(self):
        baseline, candidate = self.pair(); candidate["runs"][1]["raw_ms"] = [26.0] * 4; refresh(candidate)
        text = d11_report.render(d11_report.evaluate(baseline, candidate))
        self.assertIn("Per-run raw samples", text); self.assertIn("raw_ms", text); self.assertIn("FAIL", text)

    # Trace: invoke CLI with valid-but-failing data and assert both report files preserve the failure; invalid logs produce structured reason artifacts.
    def test_cli_writes_failure_and_invalid_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            baseline, candidate = self.pair(); candidate["runs"][1]["raw_ms"] = [26.0] * 4; refresh(candidate)
            paths = [os.path.join(tmp, name) for name in ("baseline.log", "candidate.log")]
            for path, summary in zip(paths, (baseline, candidate)):
                with open(path, "w", encoding="utf-8") as stream:
                    stream.write(d11_report.BEGIN + "\n" + json.dumps(summary) + "\n" + d11_report.END + "\n")
            report, merged = os.path.join(tmp, "report.md"), os.path.join(tmp, "merged.json")
            with mock.patch.object(sys, "argv", ["d11_report", *paths, "-o", report, "--json-out", merged]):
                self.assertEqual(1, d11_report.main())
            with open(report, encoding="utf-8") as stream: self.assertIn("FAIL", stream.read())
            with open(merged, encoding="utf-8") as stream: self.assertFalse(json.load(stream)["gates"]["sequential_p95"])
            with open(paths[1], "w", encoding="utf-8") as stream: stream.write("bad log")
            with mock.patch.object(sys, "argv", ["d11_report", *paths, "-o", report, "--json-out", merged]):
                self.assertEqual(1, d11_report.main())
            with open(merged, encoding="utf-8") as stream: failure = json.load(stream)
            self.assertEqual("invalid", failure["status"]); self.assertIn("reason", failure)


if __name__ == "__main__":
    unittest.main()
