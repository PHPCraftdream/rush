#!/usr/bin/env python3
"""Validate and compare the two D11 benchmark summaries."""
import argparse
import json
import math
import re
import sys

BEGIN = "===D11-SUMMARY-BEGIN==="
END = "===D11-SUMMARY-END==="
PINNED = "d5c3b0d9ec8607370e2d97c80503c9d5a7866740"
SHA = re.compile(r"^[0-9a-f]{40}$")


def extract_summary(path):
    with open(path, encoding="utf-8", errors="replace") as stream:
        lines = stream.readlines()
    blocks = []
    index = 0
    while index < len(lines):
        if lines[index].strip() != BEGIN:
            index += 1
            continue
        end = index + 1
        body = []
        while end < len(lines) and lines[end].strip() != END:
            body.append(lines[end])
            end += 1
        if end == len(lines):
            raise ValueError("unterminated summary")
        blocks.append(json.loads("".join(body)))
        index = end + 1
    if len(blocks) != 1:
        raise ValueError("expected exactly one summary block, got %d" % len(blocks))
    return blocks[0]


def finite(value, label, positive=False):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise ValueError("%s must be finite numeric" % label)
    if value < 0 or (positive and value <= 0):
        raise ValueError("%s out of range" % label)


def percentile(values, p):
    ordered = sorted(values)
    return ordered[math.ceil(p * len(ordered) / 100) - 1]


def close(actual, expected, label):
    if not math.isclose(actual, expected, rel_tol=1e-9, abs_tol=1e-9):
        raise ValueError("%s does not match raw run evidence" % label)


def validate(summary, mode):
    if not isinstance(summary, dict) or summary.get("mode") != mode:
        raise ValueError("expected %s summary" % mode)
    for key in ("baseline_sha", "candidate_sha"):
        if not isinstance(summary.get(key), str) or not SHA.fullmatch(summary[key]):
            raise ValueError("%s must be 40 lowercase hex digits" % key)
    if summary["baseline_sha"] != PINNED:
        raise ValueError("baseline SHA differs from pinned revision")
    for key in ("p50_ms", "p95_ms", "p99_ms", "p95_contention_ms", "throughput_cps"):
        finite(summary.get(key), key, positive=key == "throughput_cps")
    for key in ("commits", "sequential_attempts", "contention_attempts", "errors"):
        value = summary.get(key)
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            raise ValueError("%s must be a nonnegative integer" % key)
    if not summary["sequential_attempts"] or not summary["contention_attempts"] or summary["errors"] or not summary["commits"]:
        raise ValueError("attempts, errors or transaction counts are invalid")
    durability = summary.get("durability")
    if (not isinstance(durability, dict)
            or str(durability.get("journal_mode", "")).lower() != "wal"
            or str(durability.get("synchronous", "")).upper() not in ("NORMAL", "FULL", "EXTRA")):
        raise ValueError("missing or unsafe durability data")
    runs = summary.get("runs")
    if not isinstance(runs, list) or not runs:
        raise ValueError("runs must be nonempty")
    sequential = []
    contention = []
    phase_commits = phase_errors = seq_attempts = conc_attempts = 0
    phase_transitions = {key: 0 for key in ("submit_writes", "started_writes", "settled_writes")}
    warmups = 0
    for run in runs:
        if not isinstance(run, dict) or not isinstance(run.get("phase"), str):
            raise ValueError("run phase missing")
        phase = run["phase"]
        raw = run.get("raw_ms")
        if not isinstance(raw, list):
            raise ValueError("run raw samples missing")
        warmup = phase == "warmup" or phase.endswith("-warmup")
        if not raw and not warmup:
            raise ValueError("measured run raw samples missing")
        for sample in raw:
            finite(sample, "raw latency")
        if run.get("samples") != len(raw):
            raise ValueError("run sample count mismatch")
        for key in ("attempts", "errors", "commits", "submit_writes", "started_writes", "settled_writes"):
            value = run.get(key)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise ValueError("invalid run %s" % key)
        if run["attempts"] != run["samples"] + run["errors"]:
            raise ValueError("run attempted/error/sample counts inconsistent")
        for key in ("p50_ms", "p95_ms", "p99_ms"):
            finite(run.get(key), "run " + key)
        if warmup:
            warmups += 1
            if run["errors"]:
                raise ValueError("warmup errors are not allowed")
            continue
        phase_commits += run["commits"]
        phase_errors += run["errors"]
        for key in phase_transitions:
            phase_transitions[key] += run[key]
        if phase.startswith("sequential"):
            sequential.extend(raw)
            seq_attempts += run["attempts"]
        elif phase.startswith("contention"):
            contention.append(run)
            conc_attempts += run["attempts"]
            finite(run.get("calls_per_sec"), "run calls_per_sec", positive=True)
        else:
            raise ValueError("unknown measured run phase: %s" % phase)
    if warmups != 1 or not sequential or not contention:
        raise ValueError("expected one warmup and sequential/contention runs")
    if (seq_attempts != summary["sequential_attempts"]
            or conc_attempts != summary["contention_attempts"]
            or phase_errors != summary["errors"] or phase_commits != summary["commits"]):
        raise ValueError("aggregate attempts/errors/commits differ from run totals")
    close(summary["p50_ms"], percentile(sequential, 50), "sequential p50")
    close(summary["p95_ms"], percentile(sequential, 95), "sequential p95")
    close(summary["p99_ms"], percentile(sequential, 99), "sequential p99")
    contention_samples = [sample for run in contention for sample in run["raw_ms"]]
    close(summary["p95_contention_ms"], percentile(contention_samples, 95), "contention p95")
    close(summary["throughput_cps"], sum(run["calls_per_sec"] for run in contention) / len(contention), "contention throughput")
    if mode == "candidate":
        calls = summary["sequential_attempts"] + summary["contention_attempts"]
        for key in phase_transitions:
            value = summary.get(key)
            if not isinstance(value, int) or isinstance(value, bool) or value != calls:
                raise ValueError("%s must equal candidate call count" % key)
            if phase_transitions[key] != calls:
                raise ValueError("run transition totals differ from candidate calls")
        if summary["commits"] < 3 * calls:
            raise ValueError("candidate transition transactions missing")


def evaluate(baseline, candidate):
    validate(baseline, "baseline")
    validate(candidate, "candidate")
    if baseline["candidate_sha"] != candidate["candidate_sha"]:
        raise ValueError("candidate SHA mismatch")
    delta = candidate["p95_ms"] - baseline["p95_ms"]
    ratio = candidate["throughput_cps"] / baseline["throughput_cps"]
    return {
        "candidate_sha": candidate["candidate_sha"], "baseline_sha": PINNED,
        "p50_off_ms": baseline["p50_ms"], "p50_on_ms": candidate["p50_ms"],
        "p95_off_ms": baseline["p95_ms"], "p95_on_ms": candidate["p95_ms"],
        "p95_delta_ms": delta, "p95_budget_ms": 15.0,
        "p95_contention_off_ms": baseline["p95_contention_ms"],
        "p95_contention_on_ms": candidate["p95_contention_ms"],
        "p99_off_ms": baseline["p99_ms"], "p99_on_ms": candidate["p99_ms"],
        "throughput_off_cps": baseline["throughput_cps"],
        "throughput_on_cps": candidate["throughput_cps"], "throughput_ratio": ratio,
        "errors_off": baseline["errors"], "errors_on": candidate["errors"],
        "commits_off": baseline["commits"], "commits_on": candidate["commits"],
        "submit_writes_on": candidate.get("submit_writes", 0),
        "started_writes_on": candidate.get("started_writes", 0),
        "settled_writes_on": candidate.get("settled_writes", 0),
        "durability": {"baseline": baseline["durability"], "candidate": candidate["durability"]},
        "runs": {"baseline": baseline["runs"], "candidate": candidate["runs"]},
        "gates": {"sequential_p95": delta <= 15, "throughput": ratio >= .8,
                  "zero_errors": baseline["errors"] == candidate["errors"] == 0,
                  "durability": True, "transition_commits": True},
    }


def render(result):
    gates = result["gates"]
    rows = ["# D11 baseline/candidate comparison", "",
            "| Metric | Baseline | Candidate | Delta / ratio | Gate |", "|---|---:|---:|---:|---|"]
    rows.append("| Sequential p50/p95/p99 ms | %.3f / %.3f / %.3f | %.3f / %.3f / %.3f | %+0.3f ms | %s |" % (
        result["p50_off_ms"], result["p95_off_ms"], result["p99_off_ms"], result["p50_on_ms"],
        result["p95_on_ms"], result["p99_on_ms"], result["p95_delta_ms"],
        "PASS" if gates["sequential_p95"] else "FAIL"))
    contention = result["p95_contention_on_ms"] - result["p95_contention_off_ms"]
    rows.append("| Contention p95 ms | %.3f | %.3f | %+0.3f ms | informational |" % (
        result["p95_contention_off_ms"], result["p95_contention_on_ms"], contention))
    rows.append("| Throughput calls/s | %.2f | %.2f | %.3f ratio | %s |" % (
        result["throughput_off_cps"], result["throughput_on_cps"], result["throughput_ratio"],
        "PASS" if gates["throughput"] else "FAIL"))
    rows.append("| Errors | %d | %d | — | %s |" % (result["errors_off"], result["errors_on"],
                                                       "PASS" if gates["zero_errors"] else "FAIL"))
    rows.append("| Committed write transactions | %d | %d | — | recorded |" % (result["commits_off"], result["commits_on"]))
    rows += ["", "## Per-run raw samples", "", "```json", json.dumps(result["runs"], indent=2, sort_keys=True),
             "```", "", "## Merged JSON", "", "```json", json.dumps(result, indent=2, sort_keys=True), "```", ""]
    return "\n".join(rows)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("baseline")
    parser.add_argument("candidate")
    parser.add_argument("-o", "--out")
    parser.add_argument("--json-out")
    args = parser.parse_args()
    try:
        baseline = extract_summary(args.baseline)
        candidate = extract_summary(args.candidate)
        result = evaluate(baseline, candidate)
        report = render(result)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as exc:
        reason = str(exc)
        result = {"status": "invalid", "reason": reason, "gates": {}}
        report = "# D11 comparison unavailable\n\nValidation failed: %s\n" % reason
        print("d11_report: error: %s" % reason, file=sys.stderr)
    try:
        if args.out:
            with open(args.out, "w", encoding="utf-8", newline="\n") as stream:
                stream.write(report)
        else:
            sys.stdout.write(report)
        if args.json_out:
            with open(args.json_out, "w", encoding="utf-8", newline="\n") as stream:
                json.dump(result, stream, sort_keys=True, separators=(",", ":"))
                stream.write("\n")
    except OSError as exc:
        print("d11_report: output error: %s" % exc, file=sys.stderr)
        return 1
    if result.get("status") == "invalid":
        return 1
    passed = all(result["gates"].values())
    print("d11_report: %s (%d/%d gates)" % ("PASS" if passed else "FAIL",
          sum(result["gates"].values()), len(result["gates"])), file=sys.stderr)
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
