#!/usr/bin/env python3
"""Build a markdown report from a `go test -v` D11 benchmark log.

The benchmark (internal/app/app_batch_bench_test.go, added by R12-G) emits a
machine-readable summary between the marker lines
`===D11-SUMMARY-BEGIN===` and `===D11-SUMMARY-END===`. The content between
them is a single JSON object with this schema:

    {
      "p95_off_ms": float, "p95_on_ms": float, "p95_delta_ms": float,
      "p95_budget_ms": 15.0,
      "throughput_off_cps": float, "throughput_on_cps": float,
      "throughput_ratio": float,
      "errors_off": int, "errors_on": int,
      "commits_off": int, "commits_on": int,
      "durability": {"journal_mode": "wal", "synchronous": "FULL|NORMAL"},
      "runs": [{"phase": "warmup|off|on", "samples": int, "p50_ms": float,
                "p95_ms": float, "calls_per_sec": float,
                "errors": int, "commits": int}]
    }

Usage:
    python tools/d11_report.py <go-test-log> [-o out.md]

Writes the report to stdout (or -o file), prints a one-line verdict to
stderr, exits 0 iff all pass criteria hold, 1 otherwise. Stdlib only.
"""

import argparse
import json
import sys

BEGIN = "===D11-SUMMARY-BEGIN==="
END = "===D11-SUMMARY-END==="


def fail(msg):
    print("d11_report: error: %s" % msg, file=sys.stderr)
    sys.exit(1)


def extract_summary(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            lines = f.readlines()
    except OSError as e:
        fail("cannot read %s: %s" % (path, e))
    blocks = []
    i = 0
    while i < len(lines):
        if lines[i].strip() == BEGIN:
            j = i + 1
            buf = []
            while j < len(lines) and lines[j].strip() != END:
                buf.append(lines[j])
                j += 1
            if j >= len(lines):
                fail("unterminated summary block (missing %s)" % END)
            blocks.append("".join(buf))
            i = j + 1
        else:
            i += 1
    if not blocks:
        fail("no %r marker found in %s" % (BEGIN, path))
    if len(blocks) > 1:
        fail("multiple summary blocks found (%d); expected exactly one" % len(blocks))
    try:
        return json.loads(blocks[0])
    except json.JSONDecodeError as e:
        fail("summary block is not valid JSON: %s" % e)


def evaluate(s):
    dur = s.get("durability", {})
    criteria = [
        ("p95 delta within budget",
         "p95_delta_ms=%.3fms budget=%.3fms" % (s["p95_delta_ms"], s["p95_budget_ms"]),
         s["p95_delta_ms"] <= s["p95_budget_ms"]),
        ("throughput ratio >= 0.80",
         "ratio=%.3f (off=%.1f cps, on=%.1f cps)" % (
             s["throughput_ratio"], s["throughput_off_cps"], s["throughput_on_cps"]),
         s["throughput_ratio"] >= 0.80),
        ("zero errors in both phases",
         "errors_off=%d errors_on=%d" % (s["errors_off"], s["errors_on"]),
         s["errors_off"] == 0 and s["errors_on"] == 0),
        ("commits not regressed",
         "commits_off=%d commits_on=%d" % (s["commits_off"], s["commits_on"]),
         s["commits_on"] >= s["commits_off"]),
        ("durability not disabled",
         "journal_mode=%s synchronous=%s" % (
             dur.get("journal_mode"), dur.get("synchronous")),
         dur.get("journal_mode") == "wal" and dur.get("synchronous") != "OFF"),
    ]
    return criteria


def render(s, criteria):
    out = ["# D11 per-call-cost benchmark report", ""]
    out.append("| Criterion | Measured | Result |")
    out.append("| --- | --- | --- |")
    for name, detail, ok in criteria:
        out.append("| %s | %s | %s |" % (name, detail, "PASS" if ok else "FAIL"))
    out += ["", "## Per-run breakdown", "",
            "| Phase | Samples | p50 (ms) | p95 (ms) | calls/s | errors | commits |",
            "| --- | --- | --- | --- | --- | --- | --- |"]
    for r in s.get("runs", []):
        out.append("| %s | %s | %s | %s | %s | %s | %s |" % (
            r.get("phase"), r.get("samples"), r.get("p50_ms"), r.get("p95_ms"),
            r.get("calls_per_sec"), r.get("errors"), r.get("commits")))
    out += ["", "## Raw summary", "", "```json",
            json.dumps(s, indent=2, sort_keys=True), "```", ""]
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description="Render a D11 benchmark report from a go test -v log.")
    ap.add_argument("log", help="path to the go test -v log file")
    ap.add_argument("-o", "--out", help="write report to this file instead of stdout")
    args = ap.parse_args()
    summary = extract_summary(args.log)
    criteria = evaluate(summary)
    report = render(summary, criteria)
    ok = all(c[2] for c in criteria)
    if args.out:
        try:
            with open(args.out, "w", encoding="utf-8", newline="\n") as f:
                f.write(report)
        except OSError as e:
            fail("cannot write %s: %s" % (args.out, e))
    else:
        sys.stdout.write(report)
    verdict = "PASS" if ok else "FAIL"
    print("d11_report: %s (%d/%d criteria passed)" % (
        verdict, sum(1 for c in criteria if c[2]), len(criteria)), file=sys.stderr)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
