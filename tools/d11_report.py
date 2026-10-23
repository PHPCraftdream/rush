#!/usr/bin/env python3
"""Validate independent D11 baseline/candidate workloads and emit evidence."""
import argparse
import json
import math
import re
import statistics
import sys

BEGIN = "===D11-SUMMARY-BEGIN==="
END = "===D11-SUMMARY-END==="
PINNED = "d5c3b0d9ec8607370e2d97c80503c9d5a7866740"
SHA = re.compile(r"^[0-9a-f]{40}$")
WORKLOADS = ("direct-view", "legacy-bash")
EVENTS = ("submit", "started", "settled")
QUANTUM_BUDGET_MS = 20.0
# Minimum median eight-session throughput ratio (ON/OFF) per workload. direct-view is 0.75 by the
# operator decision of 2026-10-22 (docs/reviews/2026-10-22-d11-direct-view-budget.md): the batch
# path adds the Submit/Started/Settled/Release commits to a call that the OFF path serves with no
# job machinery at all. legacy-bash stays at the original 0.80.
THROUGHPUT_MIN_RATIO = {"direct-view": 0.75, "legacy-bash": 0.80}
BASELINE_DESCRIPTION = ("integration branch at %s, batch runtime flag OFF; D11 measures ON vs OFF at the integration branch, not against pre-batch main" % PINNED)


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


def validate_call_transactions(summary):
    calls = summary.get("measured_call_ids")
    evidence = summary.get("call_transactions")
    if not isinstance(calls, list) or not calls or any(not isinstance(call, str) or not call for call in calls):
        raise ValueError("candidate measured_call_ids must be a nonempty list of call IDs")
    if len(set(calls)) != len(calls):
        raise ValueError("duplicate measured call ID")
    if not isinstance(evidence, list) or len(evidence) != 3 * len(calls):
        raise ValueError("candidate must provide exactly three transaction records per measured call")
    by_call = {call: {} for call in calls}
    root_owners = {}
    transaction_events = {}
    for record in evidence:
        if not isinstance(record, dict):
            raise ValueError("invalid call transaction record")
        call_id, root_id, event, txid = (record.get(key) for key in ("call_id", "root_id", "event", "transaction"))
        if call_id not in by_call:
            raise ValueError("transaction evidence references missing or unknown call")
        if not isinstance(root_id, str) or not root_id:
            raise ValueError("transaction evidence missing root identity")
        if event not in EVENTS:
            raise ValueError("unknown checkpoint event")
        if not isinstance(txid, int) or isinstance(txid, bool) or txid <= 0:
            raise ValueError("transaction ID must be a positive integer")
        if event in by_call[call_id]:
            raise ValueError("duplicate checkpoint transition for call")
        roots = by_call[call_id]
        roots[event] = (root_id, txid)
        transaction_events.setdefault((call_id, txid), set()).add(event)
    for call_id, records in by_call.items():
        if set(records) != set(EVENTS):
            raise ValueError("missing checkpoint transition for call %s" % call_id)
        roots = {value[0] for value in records.values()}
        txids = [records[event][1] for event in EVENTS]
        if len(roots) != 1:
            raise ValueError("call %s has inconsistent root identity" % call_id)
        root_id = next(iter(roots))
        root_owner = root_owners.setdefault(root_id, call_id)
        if root_owner != call_id:
            raise ValueError("multiple measured calls share one root identity")
        if len(set(txids)) != 3:
            raise ValueError("call %s has merged checkpoint transactions" % call_id)
    if any(len(events) != 1 for events in transaction_events.values()):
        raise ValueError("one transaction contains multiple checkpoint event kinds for a call")


def validate(summary, mode, workload):
    if not isinstance(summary, dict) or summary.get("mode") != mode:
        raise ValueError("expected %s summary" % mode)
    if summary.get("workload") != workload:
        raise ValueError("expected %s workload summary" % workload)
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
    if not summary["sequential_attempts"] or not summary["contention_attempts"] or summary["errors"]:
        raise ValueError("attempts, errors or transaction counts are invalid")
    durability = summary.get("durability")
    if (not isinstance(durability, dict)
            or str(durability.get("journal_mode", "")).lower() != "wal"
            or str(durability.get("synchronous", "")).upper() not in ("NORMAL", "FULL", "EXTRA")):
        raise ValueError("missing or unsafe durability data")
    runs = summary.get("runs")
    if not isinstance(runs, list) or not runs:
        raise ValueError("runs must be nonempty")
    sequential, contention = [], []
    phase_commits = phase_errors = seq_attempts = conc_attempts = warmups = 0
    phase_transitions = {key: 0 for key in ("submit_writes", "started_writes", "settled_writes")}
    for run in runs:
        if not isinstance(run, dict) or not isinstance(run.get("phase"), str):
            raise ValueError("run phase missing")
        if run.get("workload") != workload:
            raise ValueError("run workload mismatch")
        phase, raw = run["phase"], run.get("raw_ms")
        if not isinstance(raw, list):
            raise ValueError("run raw samples missing")
        warmup = phase == "warmup" or phase.startswith("warmup")
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
    if warmups < 1:
        raise ValueError("expected at least one warmup run")
    sequential_rounds = {run["phase"] for run in runs if run["phase"].startswith("sequential")}
    contention_rounds = {run["phase"] for run in runs if run["phase"].startswith("contention")}
    if len(sequential_rounds) < 3 or len(contention_rounds) < 3:
        raise ValueError("expected at least three sequential and three contention rounds")
    if len(contention_rounds) != len(contention):
        raise ValueError("contention rounds must be distinct")
    if len(sequential_rounds) != len([run for run in runs if run["phase"].startswith("sequential")]):
        raise ValueError("sequential rounds must be distinct")
    health = summary.get("health_violations")
    if not isinstance(health, int) or isinstance(health, bool) or health != 0:
        raise ValueError("unhealthy %s turn machinery: %r health violations" % (mode, health))
    if not isinstance(summary.get("off_wait_path"), str) or not summary["off_wait_path"]:
        raise ValueError("off_wait_path must be a nonempty string")
    if (seq_attempts != summary["sequential_attempts"] or conc_attempts != summary["contention_attempts"]
            or phase_errors != summary["errors"] or phase_commits != summary["commits"]):
        raise ValueError("aggregate attempts/errors/commits differ from run totals")
    close(summary["p50_ms"], percentile(sequential, 50), "sequential p50")
    close(summary["p95_ms"], percentile(sequential, 95), "sequential p95")
    close(summary["p99_ms"], percentile(sequential, 99), "sequential p99")
    close(summary["p95_contention_ms"], percentile([x for run in contention for x in run["raw_ms"]], 95), "contention p95")
    close(summary["throughput_cps"], sum(run["calls_per_sec"] for run in contention) / len(contention), "contention throughput")
    if mode == "candidate":
        calls = summary["sequential_attempts"] + summary["contention_attempts"]
        for key in phase_transitions:
            value = summary.get(key)
            if not isinstance(value, int) or isinstance(value, bool) or value != calls:
                raise ValueError("%s must equal candidate call count" % key)
            if phase_transitions[key] != calls:
                raise ValueError("run transition totals differ from candidate calls")
        validate_call_transactions(summary)
        if len(summary["measured_call_ids"]) != calls:
            raise ValueError("measured call identity count differs from attempts")


def evaluate_pair(baseline, candidate, workload):
    validate(baseline, "baseline", workload)
    validate(candidate, "candidate", workload)
    if baseline["candidate_sha"] != candidate["candidate_sha"]:
        raise ValueError("candidate SHA mismatch")
    if baseline["off_wait_path"] != candidate["off_wait_path"]:
        raise ValueError("baseline and candidate summaries disagree on the call origin / wait path")
    delta = candidate["p95_ms"] - baseline["p95_ms"]
    ratio = candidate["throughput_cps"] / baseline["throughput_cps"]
    base_rates = {run["phase"]: run["calls_per_sec"] for run in baseline["runs"] if run["phase"].startswith("contention")}
    cand_rates = {run["phase"]: run["calls_per_sec"] for run in candidate["runs"] if run["phase"].startswith("contention")}
    if len(base_rates) < 3 or set(base_rates) != set(cand_rates):
        raise ValueError("contention rounds must match across modes with at least three rounds")
    phases = sorted(base_rates)
    ratios = [cand_rates[p] / base_rates[p] for p in phases]
    median_ratio = statistics.median(ratios)
    return {
        "workload": workload,
        "off_choice": "direct view fixture (not legacy-wrapped)" if workload == "direct-view" else "same trivial bash command, OFF measured through the CLI-origin production wait (async wrapper)",
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
        "call_transactions": candidate.get("call_transactions", []),
        "health_violations": {"baseline": baseline["health_violations"], "candidate": candidate["health_violations"]},
        "off_wait_path": candidate["off_wait_path"], "off_wait_path_baseline": baseline["off_wait_path"],
        "throughput_rounds": [{"phase": p, "cps_off": base_rates[p], "cps_on": cand_rates[p], "ratio": cand_rates[p] / base_rates[p]} for p in phases],
        "throughput_ratio_median": median_ratio, "throughput_ratio_worst": min(ratios),
        "durability": {"baseline": baseline["durability"], "candidate": candidate["durability"]},
        "runs": {"baseline": baseline["runs"], "candidate": candidate["runs"]},
        "throughput_min_ratio": THROUGHPUT_MIN_RATIO[workload],
        "gates": {"sequential_p95": delta <= 15.0, "throughput": median_ratio >= THROUGHPUT_MIN_RATIO[workload],
                  "zero_errors": baseline["errors"] == candidate["errors"] == 0,
                  "durability": True, "transition_commits": True},
    }


def evaluate(baselines, candidates):
    workloads = {workload: evaluate_pair(baselines[workload], candidates[workload], workload) for workload in WORKLOADS}
    gates = {"%s.%s" % (workload, key): value for workload, result in workloads.items() for key, value in result["gates"].items()}
    candidate_shas = {result["candidate_sha"] for result in workloads.values()}
    baseline_shas = {result["baseline_sha"] for result in workloads.values()}
    if len(candidate_shas) != 1:
        raise ValueError("candidate workload summaries use different candidate SHAs")
    if len(baseline_shas) != 1:
        raise ValueError("baseline workload summaries use different baseline SHAs")
    off_wait_paths = {path for result in workloads.values() for path in (result["off_wait_path"], result["off_wait_path_baseline"])}
    if len(off_wait_paths) != 1:
        raise ValueError("workload summaries disagree on the call origin / wait path")
    result = {"candidate_sha": workloads[WORKLOADS[0]]["candidate_sha"], "baseline_sha": PINNED, "baseline_description": BASELINE_DESCRIPTION, "off_wait_path": workloads[WORKLOADS[0]]["off_wait_path"], "workloads": workloads, "gates": gates}
    for label, mode in (("off", "baseline"), ("on", "candidate")):
        quantum = abs(workloads["legacy-bash"]["p50_%s_ms" % label] - workloads["direct-view"]["p50_%s_ms" % label])
        result["bash_quantum_%s_ms" % label] = quantum
        gates["bash_quantum_%s" % label] = quantum <= QUANTUM_BUDGET_MS
    return result


def render(result, evidence=True):
    workloads = result["workloads"]
    rows = ["# D11 baseline/candidate comparison", "",
            "Each workload independently gates sequential p95 delta <=15 ms, median eight-session throughput ratio over all rounds (legacy-bash >=%.2f; direct-view >=%.2f, operator budget of 2026-10-22), zero turn-health violations, and a bash-vs-view p50 quantum sanity bound of 20 ms per mode." % (
                THROUGHPUT_MIN_RATIO["legacy-bash"], THROUGHPUT_MIN_RATIO["direct-view"]), "",
            "Pinned baseline %s: %s." % (PINNED, BASELINE_DESCRIPTION),
            "Call origin / wait path (both modes): %s." % workloads[WORKLOADS[0]]["off_wait_path"],
            "Bash-vs-view OFF p50 quantum: %.3f ms (gate <=20 ms, %s); ON: %.3f ms (%s)." % (
                result["bash_quantum_off_ms"], "PASS" if result["gates"]["bash_quantum_off"] else "FAIL",
                result["bash_quantum_on_ms"], "PASS" if result["gates"]["bash_quantum_on"] else "FAIL"), ""]
    for workload in WORKLOADS:
        item = result["workloads"][workload]
        gates = item["gates"]
        rows.extend(["## %s" % workload, "", "OFF workload: %s." % item["off_choice"], "",
                     "| Metric | Baseline | Candidate | Delta / ratio | Gate |", "|---|---:|---:|---:|---|"])
        rows.append("| Sequential p50/p95/p99 ms | %.3f / %.3f / %.3f | %.3f / %.3f / %.3f | %+0.3f ms | %s |" % (
            item["p50_off_ms"], item["p95_off_ms"], item["p99_off_ms"], item["p50_on_ms"], item["p95_on_ms"], item["p99_on_ms"], item["p95_delta_ms"], "PASS" if gates["sequential_p95"] else "FAIL"))
        rows.append("| Contention p95 ms | %.3f | %.3f | %+0.3f ms | informational |" % (item["p95_contention_off_ms"], item["p95_contention_on_ms"], item["p95_contention_on_ms"]-item["p95_contention_off_ms"]))
        rows.append("| Throughput calls/s | %.2f | %.2f | %.3f ratio | %s |" % (item["throughput_off_cps"], item["throughput_on_cps"], item["throughput_ratio"], "PASS" if gates["throughput"] else "FAIL"))
        rows.extend(["", "| Round | OFF calls/s | ON calls/s | ratio |", "|---|---:|---:|---:|"])
        for round_row in item["throughput_rounds"]:
            rows.append("| %s | %.2f | %.2f | %.3f |" % (round_row["phase"], round_row["cps_off"], round_row["cps_on"], round_row["ratio"]))
        rows.append("| Median ratio (worst %.3f; gate >=%.2f) | | | %.3f | %s |" % (item["throughput_ratio_worst"], item["throughput_min_ratio"], item["throughput_ratio_median"], "PASS" if gates["throughput"] else "FAIL"))
        rows.append("")
        rows.append("| Turn-health violations | %d | %d | — | %s |" % (item["health_violations"]["baseline"], item["health_violations"]["candidate"], "PASS" if item["health_violations"]["baseline"] == item["health_violations"]["candidate"] == 0 else "FAIL"))
        rows.append("| Errors | %d | %d | — | %s |" % (item["errors_off"], item["errors_on"], "PASS" if gates["zero_errors"] else "FAIL"))
        rows.append("| Commit count | %d | %d | — | recorded |" % (item["commits_off"], item["commits_on"]))
        rows.extend(["", "Candidate per-call committed checkpoint evidence: %d rows." % len(item["call_transactions"]), ""])
    if evidence:
        rows.extend(["## Raw run evidence", "", "```json", json.dumps({name: result["workloads"][name]["runs"] for name in WORKLOADS}, indent=2, sort_keys=True), "```", "", "## Merged JSON", "", "```json", json.dumps(result, indent=2, sort_keys=True), "```", ""])
    else:
        rows.extend(["Raw run evidence and the merged JSON are in the d11-report.md and d11-summary.json artifacts (GITHUB_STEP_SUMMARY is capped at 1 MiB).", ""])
    return "\n".join(rows)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline_view")
    parser.add_argument("candidate_view")
    parser.add_argument("baseline_bash")
    parser.add_argument("candidate_bash")
    parser.add_argument("-o", "--out")
    parser.add_argument("--json-out")
    parser.add_argument("--summary-out", help="compact markdown without the raw evidence, for GITHUB_STEP_SUMMARY")
    args = parser.parse_args()
    try:
        baselines = {"direct-view": extract_summary(args.baseline_view), "legacy-bash": extract_summary(args.baseline_bash)}
        candidates = {"direct-view": extract_summary(args.candidate_view), "legacy-bash": extract_summary(args.candidate_bash)}
        result = evaluate(baselines, candidates)
        report = render(result)
        summary = render(result, evidence=False)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as exc:
        reason = str(exc)
        result = {"status": "invalid", "reason": reason, "gates": {}}
        report = "# D11 comparison unavailable\n\nValidation failed: %s\n" % reason
        summary = report
        print("d11_report: error: %s" % reason, file=sys.stderr)
    try:
        if args.out:
            with open(args.out, "w", encoding="utf-8", newline="\n") as stream:
                stream.write(report)
        else:
            sys.stdout.write(report)
        if args.summary_out:
            with open(args.summary_out, "w", encoding="utf-8", newline="\n") as stream:
                stream.write(summary)
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
    print("d11_report: %s (%d/%d gates)" % ("PASS" if passed else "FAIL", sum(result["gates"].values()), len(result["gates"])), file=sys.stderr)
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
