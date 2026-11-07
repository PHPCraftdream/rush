from pathlib import Path
import json
import os
import subprocess

package = './internal/session'
tests = '^Test(DrainSessionNow_(WaitedExecution_(CarriesAllAssistantIdentities|PublishesAssistantIDBeforeCompletion)|LateStartDoesNotInheritCompletedIdentities)|AdmissionRefusalNotificationAllowsRepeatedObservations|LiveIdentityNotificationAllowsReplay)$'
command = ['go', 'test'] + os.environ.get('RACE', '').split() + ['-json', '-p', '1', '-parallel', '1', '-timeout=15m', '-run', tests, package]
result = subprocess.run(command + ['-count=100'], text=True, encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
Path('waited-identities-baseline.jsonl').write_text(result.stdout, encoding="utf-8")
if result.returncode:
    print(result.stdout)
    raise SystemExit(result.returncode)
counts = {}
for line in result.stdout.splitlines():
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        continue
    if event.get('Test') and event.get('Action') == 'pass':
        counts[event['Test']] = counts.get(event['Test'], 0) + 1
    if '--- pump failure report' in event.get('Output', '') or '--- pump setup/cleanup failure report' in event.get('Output', ''):
        raise SystemExit('successful baseline emitted failure-only diagnostics')
if len(counts) != 5 or any(count != 100 for count in counts.values()):
    raise SystemExit('incomplete baseline: ' + repr(counts))
print('baseline passes:', counts)

path = Path('internal/session/p575_background_outcome_handoff_test.go')
original = path.read_text(encoding="utf-8")
a = original.index('func TestDrainSessionNow_WaitedExecution_CarriesAllAssistantIdentities(')
b = original.index('type sequentialIdentityCoordinator', a)
scenario = original[a:b]
start = scenario.index('\n\tselect {\n\tcase <-refused:')
end = scenario.index('\n\tgate.release <- nil', start) + len('\n\tgate.release <- nil')
late = scenario[:start] + scenario[end:]
launch = '\tgo func() {'
assert late.count(launch) == 1
late = late.replace(launch, '\tgate.release <- nil\n\twaitForBackgroundAdmissionRelease(t, pump, sessID)\n' + launch)
try:
    path.write_text(original[:a] + late + original[b:], encoding="utf-8")
    result = subprocess.run(command + ['-count=1'], text=True, encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    Path('waited-identities-late-start-mutant.jsonl').write_text(result.stdout, encoding="utf-8")
    if result.returncode == 0 or 'Not equal:' not in result.stdout or 'expected: 1' not in result.stdout or 'actual  : 0' not in result.stdout:
        print(result.stdout)
        raise SystemExit('forced old scheduling did not reproduce the original DrainComplete/DrainNoWork assertion')
finally:
    path.write_text(original, encoding="utf-8")

path = Path('internal/session/run_queue_drain_session.go')
original = path.read_text(encoding="utf-8")
call = 'recordDrainAssistantIdentity(ctx, otherEntry.outcome.assistantIDs, otherEntry.outcome.terminalAssistantID)'
assert original.count(call) == 1
try:
    path.write_text(original.replace(call, '// mutant: omit observed identities'), encoding="utf-8")
    result = subprocess.run(command + ['-count=1'], text=True, encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    Path('waited-identities-missing-ids-mutant.jsonl').write_text(result.stdout, encoding="utf-8")
    if result.returncode == 0 or 'Not equal:' not in result.stdout or 'assistant-tool-step' not in result.stdout:
        print(result.stdout)
        raise SystemExit('identity-loss mutant was not killed by the identity assertion')
finally:
    path.write_text(original, encoding="utf-8")

path = Path('internal/session/p575_late_drain_test.go')
original = path.read_text(encoding="utf-8")
notification = '\tselect {\n\tcase ch <- struct{}{}:\n\tdefault:\n\t}'
assert original.count(notification) == 1
try:
    path.write_text(original.replace(notification, '\tclose(ch)'), encoding="utf-8")
    result = subprocess.run(command + ['-count=1'], text=True, encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    Path('waited-identities-double-close-mutant.jsonl').write_text(result.stdout, encoding="utf-8")
    if result.returncode == 0 or 'panic: close of closed channel' not in result.stdout:
        print(result.stdout)
        raise SystemExit('double-close mutant was not killed by repeated admission notification')
finally:
    path.write_text(original, encoding="utf-8")

path = Path('internal/session/p575_late_drain_test.go')
original = path.read_text(encoding="utf-8")
notification = '\tselect {\n\tcase ch <- id:\n\tdefault:\n\t}'
assert original.count(notification) == 1
try:
    path.write_text(original.replace(notification, '\tch <- id'), encoding="utf-8")
    result = subprocess.run(command + ['-count=1'], text=True, encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    Path('waited-identities-blocking-replay-mutant.jsonl').write_text(result.stdout, encoding="utf-8")
    if result.returncode == 0 or 'identity replay notifier blocked' not in result.stdout:
        print(result.stdout)
        raise SystemExit('blocking replay mutant was not killed by the notification guard')
finally:
    path.write_text(original, encoding="utf-8")
