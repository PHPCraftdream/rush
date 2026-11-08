"""Проверяет контракты результата и чувствительность точечных regression-тестов."""

import json
import os
from pathlib import Path
import subprocess

BASE = ["go", "test"] + (["-race"] if os.environ.get("PROBE_RACE") == "1" else [])
BASE += ["-json", "-p", "1", "-parallel", "1", "-count=1", "-timeout=5m"]
ROOT = Path("internal/agentresult")


def run(name, pattern=".", expected=None):
    result = subprocess.run(
        BASE + [f"-run={pattern}", "./internal/agentresult"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=360,
        check=False,
    )
    Path(f"agent-result-{name}.jsonl").write_text(result.stdout, encoding="utf-8")
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    failed = [e.get("Test", "") for e in events if e.get("Action") == "fail"]
    if expected:
        if result.returncode == 0 or expected not in failed:
            raise RuntimeError(f"{name}: missing expected assertion failure: {failed}")
        if "build failed" in result.stdout or "undefined:" in result.stdout:
            raise RuntimeError(f"{name}: compile failure is not a killed mutant")
    else:
        if result.returncode != 0:
            raise RuntimeError("baseline failed; inspect JSON artifact")
        passed = {e.get("Test") for e in events if e.get("Action") == "pass"}
        for test in (
            "TestSchemaSizeLimitsLargeBounds",
            "TestDiagnosticsForgedLibraryTextNotTrusted",
            "TestDiagnosticsUnionTypeReportsActualFailure",
            "TestDiagnosticsLongPointerDropped",
            "TestDiagnosticsTrustedFeedbackActuallyOverflows",
            "TestDiagnosticConversionsPreserveTrustedPaths",
        ):
            if test not in passed:
                raise RuntimeError(f"baseline did not pass {test}")
    print(f"{name}: expected outcome verified", flush=True)


run("baseline")
mutants = [
    (
        "skip-size-adapter", "schema.go",
        "clampSchemaSizeLimits(registry)", "error(nil)",
        "^TestSchemaSizeLimitsLargeBounds$", "TestSchemaSizeLimitsLargeBounds/maxLength/2147483648",
    ),
    (
        "union-as-intersection", "schema_diagnostics.go",
        "if ok && matchesSchemaType(s, kind) {", "if ok && matchesSchemaType(s, kind) && false {",
        "^TestDiagnosticsUnionTypeReportsActualFailure$", "TestDiagnosticsUnionTypeReportsActualFailure/object-or-null",
    ),
    (
        "restart-overflowed-path", "schema_diagnostics.go",
        "return unavailableDiagnosticPath", 'return ""',
        "^TestDiagnosticsLongPointerDropped$", "TestDiagnosticsLongPointerDropped",
    ),
    (
        "trust-caller-pointers", "errors.go",
        "if i < len(trusted) && d == trusted[i] {", "if true {",
        "^TestFeedbackIsBoundedAndSanitized$", "TestFeedbackIsBoundedAndSanitized",
    ),
]
for name, filename, old, new, pattern, expected in mutants:
    path = ROOT / filename
    original = path.read_text(encoding="utf-8")
    if old not in original:
        raise RuntimeError(f"{name}: mutation anchor missing")
    try:
        path.write_text(original.replace(old, new), encoding="utf-8")
        run(name, pattern, expected)
    finally:
        path.write_text(original, encoding="utf-8")
