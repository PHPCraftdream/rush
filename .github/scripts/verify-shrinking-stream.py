"""CI-only deterministic shrinking-stream regression and assertion mutant."""
import json
import os
from pathlib import Path
import subprocess

TEST = "TestExecuteRunCycle7ShrinkingStreamEventRemainsHardFailure"
SOURCE = Path("internal/app/app_run_reviewer.go")


def run(label, count):
    args = ["go", "test", "-json", "-p", "1", "-parallel", "1", f"-count={count}", "-run", "^" + TEST + "$"]
    if os.environ.get("PROBE_RACE") == "1":
        args.append("-race")
    args.append("./internal/app")
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            encoding="utf-8", errors="replace", timeout=1800)
    Path(f"shrinking-stream-{label}.jsonl").write_text(result.stdout, encoding="utf-8")
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return result, events


baseline, events = run("baseline", 100)
passes = [e for e in events if e.get("Action") == "pass" and e.get("Test") == TEST]
if baseline.returncode or len(passes) != 100 or "shrinking stream: assistant=" in baseline.stdout:
    print(baseline.stdout[-20000:])
    raise SystemExit("baseline failed, omitted tests, or emitted failure-only diagnostics")

original = SOURCE.read_bytes()
try:
    source = original.decode("utf-8")
    anchor = 'return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)'
    if source.count(anchor) != 1:
        raise SystemExit("shrinking-error mutation anchor changed")
    SOURCE.write_text(source.replace(anchor, "return nil"), encoding="utf-8")
    mutant, events = run("ignore-shrink", 1)
    failures = [e.get("Test") for e in events if e.get("Action") == "fail" and e.get("Test")]
    if (mutant.returncode == 0 or failures != [TEST] or "[build failed]" in mutant.stdout
            or "An error is expected but got nil" not in mutant.stdout):
        print(mutant.stdout[-20000:])
        raise SystemExit("mutant did not fail the exact error assertion")
finally:
    SOURCE.write_bytes(original)
print("baseline: 100 PASS; ignored shrinking error: expected assertion failure")
