"""CI-only proof of fixture isolation and failure-only diagnostics."""
import json
import os
from pathlib import Path
import subprocess

TARGET = Path("internal/config/store_write_reload_fixture_test.go")
TEST = "TestWriteReloadFixtureDoesNotTrackForeignGlobalSlot"
PATTERN = "^(TestSetConfigField_AutoReloads|TestRemoveConfigField_AutoReloads|TestWriteReloadFixtureDoesNotTrackForeignGlobalSlot|TestWriteReloadFixtureStillDetectsOwnExternalEdit)$"


def run(name, pattern, count):
    args = ["go", "test", "-json", "-p", "1", "-parallel", "1", f"-count={count}", "-run", pattern]
    if os.environ.get("PROBE_RACE") == "1":
        args.append("-race")
    args.append("./internal/config")
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            encoding="utf-8", errors="replace", timeout=1800)
    Path(f"config-staleness-{name}.jsonl").write_text(result.stdout, encoding="utf-8")
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return result, events


baseline, events = run("baseline", PATTERN, 30)
passes = [e for e in events if e.get("Action") == "pass" and e.get("Test") and "/" not in e["Test"]]
if baseline.returncode or len(passes) != 120 or "auto-reload staleness:" in baseline.stdout:
    print(baseline.stdout[-20000:])
    raise SystemExit("baseline failed, omitted tests, or emitted failure diagnostics")

original = TARGET.read_bytes()
needle = b"\tktavLightIsolateEnv(t)\n"
if original.count(needle) != 1:
    raise SystemExit("fixture isolation mutation anchor changed")
try:
    TARGET.write_bytes(original.replace(needle, b"", 1))
    mutant, events = run("without-isolation", "^" + TEST + "$", 1)
    failures = [e.get("Test") for e in events if e.get("Action") == "fail" and e.get("Test")]
    if (mutant.returncode == 0 or failures != [TEST] or "[build failed]" in mutant.stdout
            or "unowned global slots" not in mutant.stdout):
        print(mutant.stdout[-20000:])
        raise SystemExit("isolation mutant did not fail the expected regression assertion")
finally:
    TARGET.write_bytes(original)
print("baseline: 120 PASS, silence on success; isolation mutant: expected assertion failure")
