"""Проверяет оба порядка candidate events и чувствительность regression-тестов."""

import json
import os
from pathlib import Path
import subprocess

TARGET = Path("internal/agent/tools/mcp/candidate_refresh_observation_test.go")
PATTERN = "^(TestCandidateListChangedPublishesAfterCommitAndRefreshesNewSession|TestCandidateRefreshObservation.*)$"
BASE = ["go", "test"] + (["-race"] if os.environ.get("PROBE_RACE") == "1" else [])
BASE += ["-json", "-p", "1", "-parallel", "1", "-timeout=10m"]


def run(name, count, expected_failure=None):
    result = subprocess.run(
        BASE + [f"-count={count}", f"-run={PATTERN}", "./internal/agent/tools/mcp"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=660,
        check=False,
    )
    Path(f"mcp-candidate-{name}.jsonl").write_text(result.stdout, encoding="utf-8")
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    failures = [e.get("Test", "") for e in events if e.get("Action") == "fail"]
    if expected_failure:
        if result.returncode == 0 or expected_failure not in failures:
            raise RuntimeError(f"{name}: expected assertion failure missing: {failures}")
        if "build failed" in result.stdout or "undefined:" in result.stdout:
            raise RuntimeError(f"{name}: compile error is not a killed mutant")
    else:
        if result.returncode != 0:
            raise RuntimeError(f"{name}: baseline failed; inspect JSON artifact")
        for test in (
            "TestCandidateListChangedPublishesAfterCommitAndRefreshesNewSession",
            "TestCandidateRefreshObservationAcceptsBothOrders",
            "TestCandidateRefreshObservationRejectsMissingOrDuplicateFacts",
            "TestCandidateRefreshObservationCancellation",
        ):
            passed = sum(e.get("Action") == "pass" and e.get("Test") == test for e in events)
            if passed != count:
                raise RuntimeError(f"{name}: {test}: pass count {passed}, expected {count}")
        if "candidate refresh failure report:" in result.stdout:
            raise RuntimeError("baseline emitted failure-only diagnostics")
    print(f"{name}: expected outcome verified", flush=True)


run("baseline", 100)
original = TARGET.read_text(encoding="utf-8")
old_condition = "for !observation.refreshPublished || observation.rawEvents == 0 {"
if original.count(old_condition) != 1:
    raise RuntimeError("mutation anchor missing")
start = original.index("\t// Commit уже вернулся:")
end = original.index("\n}\n\nfunc candidateObservationEvents", start)
early_finish = original[:start] + "\treturn observation, nil" + original[end:]
early_finish = early_finish.replace(old_condition, "for !observation.refreshPublished {")
mutants = [
    ("stop-on-refresh", early_finish, "TestCandidateRefreshObservationAcceptsBothOrders/refresh-first"),
    (
        "allow-duplicate",
        original.replace("if o.rawEvents > 1 {", "if o.rawEvents > 999 {"),
        "TestCandidateRefreshObservationRejectsMissingOrDuplicateFacts/duplicate-after-both",
    ),
]
for name, changed, expected in mutants:
    if changed == original:
        raise RuntimeError(f"{name}: mutation did not change source")
    try:
        TARGET.write_text(changed, encoding="utf-8")
        run(name, 1, expected)
    finally:
        TARGET.write_text(original, encoding="utf-8")
