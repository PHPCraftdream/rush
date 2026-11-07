from pathlib import Path
import json
import os
import subprocess

race = os.environ.get('RACE', '').split()
base = ['go', 'test'] + race + ['-json', '-p', '1', '-parallel', '1', '-count=1', '-timeout=10m']
commands = {
    'cmd': base + ['-run', '^Test(ProcessStop|InstallConsoleProcessStop)', './internal/cmd'],
    'session': base + ['-run', '^TestProcessStop', './internal/session'],
    'main': base + ['-run', '^TestProcessStop', '.'],
}

def run(command):
    return subprocess.run(command, text=True, encoding='utf-8', errors='replace', stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

for name, command in commands.items():
    result = run(command)
    Path('first-signal-baseline-' + name + '.jsonl').write_text(result.stdout, encoding='utf-8')
    if result.returncode:
        print(result.stdout)
        raise SystemExit(result.returncode)
    passes = 0
    for line in result.stdout.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get('Action') == 'pass' and event.get('Test'):
            passes += 1
        if 'process-stop failure report:' in event.get('Output', ''):
            raise SystemExit('successful test emitted failure-only diagnostics')
    if passes == 0:
        raise SystemExit('no tests ran for ' + name)
    print(name, 'passing test events:', passes)

mutants = [
    ('skip-tree-stop', 'internal/cmd/process_stop.go', '\n\t\t\tstopTrees()\n', '\n\t\t\t_ = stopTrees\n', 'cmd', 'first request did not exit once'),
    ('skip-first-exit', 'internal/cmd/process_stop.go', '\n\t\t\texit(code)\n', '\n\t\t\t_ = code\n', 'cmd', 'first request did not exit once'),
    ('skip-deadline', 'internal/cmd/process_stop.go', 'stopDeadline := armDeadline(func() { exit(code) })', 'stopDeadline := func() {}', 'cmd', 'exit deadline was not armed before teardown'),
    ('audit-exit', 'internal/cmd/process_stop.go', 'newProcessStop(session.StopTrackedProcessTrees, os.Exit,', 'newProcessStop(session.StopTrackedProcessTrees, exitWithAudit,', 'cmd', 'first signal did not close process tree/output'),
    ('skip-spawn-gate', 'internal/session/track_stop.go', 'if processTreesStopping.Load() {', 'if false {', 'session', 'start admitted after process stop'),
    ('skip-startup-handler', 'main.go', 'stopSignals := cmd.InstallProcessStop()', 'stopSignals := func() {}', 'main', 'process-stop handler must be installed before startup disk work'),
]
if os.name == 'nt':
    mutants += [
        ('swallow-console', 'internal/cmd/console_ctrl_windows.go', 'return 130, true', 'return 0, false', 'cmd', 'handled=false'),
        ('skip-late-registration-kill', 'internal/session/track_windows.go', 'if processTreesStopping.Load() {', 'if false {', 'session', 'late registration escaped the process-stop sweep'),
    ]
else:
    # Обе Unix-точки регистрации должны подчиняться stop-флагу.
    mutants += [
        ('skip-late-registration-kill', 'internal/session/track_unix.go', 'if processTreesStopping.Load() {', 'if false {', 'session', 'late registration escaped the process-stop sweep'),
    ]

for name, filename, before, after, group, expected in mutants:
    path = Path(filename)
    original = path.read_text(encoding='utf-8')
    expected_count = 2 if filename.endswith('track_unix.go') else 1
    if original.count(before) != expected_count:
        raise SystemExit('mutant target changed: ' + name)
    try:
        path.write_text(original.replace(before, after), encoding='utf-8')
        result = run(commands[group])
        Path('first-signal-mutant-' + name + '.jsonl').write_text(result.stdout, encoding='utf-8')
        if result.returncode == 0 or expected not in result.stdout:
            print(result.stdout)
            raise SystemExit('mutant not killed by its expected assertion: ' + name)
        print('killed:', name)
    finally:
        path.write_text(original, encoding='utf-8')
