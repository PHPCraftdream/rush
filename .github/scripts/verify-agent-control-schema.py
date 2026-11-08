"""CI-only proof that generated control schemas include their runtime target."""
import json
import os
from pathlib import Path
import re
import subprocess

BASE = Path("internal/agent/tools")
FILES = [BASE / name for name in ("inject_agent.go", "inspect_agent.go", "stop_agent.go", "agent_control_test.go")]
SCHEMA_TEST = "TestAgentControlSchemasExposeFlatChildSessionID"
PATTERN = "^(TestAgentControlSchemasExposeFlatChildSessionID|TestAgentControlChildSessionIDWireRemainsFlat|TestInspectAgentTool_NoChildListsDelegations|TestControlTools_MissingChildIDPointsAtInspectAgent)$"


def run(label, pattern, count):
    args = ["go", "test", "-json", "-p", "1", "-parallel", "1", f"-count={count}", "-run", pattern]
    if os.environ.get("PROBE_RACE") == "1":
        args.append("-race")
    args.append("./internal/agent/tools")
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            encoding="utf-8", errors="replace", timeout=1800)
    Path(f"control-schema-{label}.jsonl").write_text(result.stdout, encoding="utf-8")
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return result, events


baseline, events = run("baseline", PATTERN, 20)
passes = [e for e in events if e.get("Action") == "pass" and e.get("Test") and "/" not in e["Test"]]
if baseline.returncode or len(passes) != 80 or "generated control schema=" in baseline.stdout:
    print(baseline.stdout[-20000:])
    raise SystemExit("baseline failed, omitted tests, or emitted failure diagnostics")

originals = {path: path.read_bytes() for path in FILES}
try:
    for path in FILES[:3]:
        source = originals[path].decode("utf-8")
        source, count = re.subn(r'(?m)^\tChildSessionID\s+string `[^`]+`$', "\tchildSessionIDParam", source)
        if count != 1:
            raise SystemExit(f"old-layout mutation anchor changed: {path}")
        path.write_text(source, encoding="utf-8")
    path = FILES[3]
    source = originals[path].decode("utf-8")
    for before, after, expected in (
        ('InspectAgentParams{ChildSessionID: "child-1"}', 'InspectAgentParams{childSessionIDParam{"child-1"}}', 2),
        ('StopAgentParams{ChildSessionID: "child-1"}', 'StopAgentParams{childSessionIDParam{"child-1"}}', 1),
        ('InjectAgentParams{ChildSessionID: "child-1"', 'InjectAgentParams{childSessionIDParam: childSessionIDParam{"child-1"}', 2),
    ):
        if source.count(before) != expected:
            raise SystemExit("typed fixture mutation anchor changed")
        source = source.replace(before, after)
    path.write_text(source, encoding="utf-8")
    mutant, events = run("old-private-embedding", "^" + SCHEMA_TEST + "$", 1)
    failures = [e["Test"] for e in events if e.get("Action") == "fail" and e.get("Test") and "/" not in e["Test"]]
    if (mutant.returncode == 0 or failures != [SCHEMA_TEST] or "[build failed]" in mutant.stdout
            or "the model must see the target required by runtime" not in mutant.stdout):
        print(mutant.stdout[-20000:])
        raise SystemExit("old field layout did not fail the expected schema assertion")
finally:
    for path, content in originals.items():
        path.write_bytes(content)
print("baseline: 80 PASS; old private embedding: expected schema assertion failure")
