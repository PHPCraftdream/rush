"""CI-only repetition of the release-callback database cleanup regression."""
import json
import os
from pathlib import Path
import subprocess

TEST = "TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts"
args = ["go", "test", "-json", "-p", "1", "-parallel", "1", "-count=100", "-run", "^" + TEST + "$"]
if os.environ.get("PROBE_RACE") == "1":
    args.append("-race")
args.append("./internal/agent")
result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                        encoding="utf-8", errors="replace", timeout=1800)
Path("debt-cleanup-baseline.jsonl").write_text(result.stdout, encoding="utf-8")
events = []
for line in result.stdout.splitlines():
    try:
        events.append(json.loads(line))
    except json.JSONDecodeError:
        pass
passes = [e for e in events if e.get("Action") == "pass" and e.get("Test") == TEST]
if result.returncode or len(passes) != 100 or "another-holder: stage=" in result.stdout:
    print(result.stdout[-20000:])
    raise SystemExit("cleanup regression failed, omitted tests, or emitted failure-only diagnostics")
print("release callback cleanup: 100 PASS")
