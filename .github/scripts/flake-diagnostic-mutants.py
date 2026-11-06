from pathlib import Path
import os
import subprocess

package = os.environ['TEST_PACKAGE']
if package == './internal/server':
    tests = '^Test(PanicTimelineReportsOnlyOnFailure|ListenerReportOnlyOnFailure|ListenerSnapshotCountsConnectionStates)$'
    mutants = [
        ('panic-unconditional', 'internal/server/panic_timeline_test.go', 'if !t.Failed() {', 'if false {', 'successful test emitted diagnostics'),
        ('panic-missing', 'internal/server/panic_timeline_test.go', 'if !t.Failed() {', 'if true {', 'report lacks'),
        ('panic-no-state', 'internal/server/panic_timeline_test.go', 'state = snapshot()', 'state = ""', 'report lacks'),
        ('listener-unconditional', 'internal/server/listener_failure_report_test.go', 'if !t.Failed() {', 'if false {', 'successful test emitted or collected listener diagnostics'),
        ('listener-missing', 'internal/server/listener_failure_report_test.go', 'if !t.Failed() {', 'if true {', 'snapshot calls=0'),
        ('listener-no-timeline', 'internal/server/listener_failure_report_test.go', '+ tl.String() +', '+ "" +', 'report lacks'),
    ]
elif package == './internal/session':
    tests = '^Test(PumpReportOnlyOnFailure|PumpQueueReportRetainsReadErrors|PumpQueueSnapshotBeforeFirstLeaseAndOnReadError|PumpFailureReportCarriesTimelineAndPumpState)$'
    mutants = [
        ('pump-unconditional', 'internal/session/pump_failure_report_test.go', 'if !t.Failed() {', 'if false {', 'successful test emitted or collected diagnostics'),
        ('pump-missing', 'internal/session/pump_failure_report_test.go', 'if !t.Failed() {', 'if true {', 'failure report calls=0'),
        ('pump-no-timeline', 'internal/session/pump_failure_report_test.go', 'strings.Join(tl.events, "\\n")', '""', 'report lacks'),
        ('queue-no-state', 'internal/session/pump_queue_report_test.go', 'e.Status, e.Attempts, e.LeasedBy', '"", e.Attempts, e.LeasedBy', 'pending row missing'),
        ('queue-no-read-error', 'internal/session/pump_queue_report_test.go', 'err, time.Since(began)', 'nil, time.Since(began)', 'read error missing'),
    ]
else:
    raise SystemExit('unsupported package: ' + package)

command = ['go', 'test', '-p', '1', '-parallel', '1', '-count=1', '-timeout=10m', '-run', tests, package]
result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
Path('diagnostic-baseline.log').write_text(result.stdout)
if result.returncode:
    print(result.stdout)
    raise SystemExit(result.returncode)

for name, filename, before, after, expected in mutants:
    path = Path(filename)
    original = path.read_text()
    if original.count(before) != 1:
        raise SystemExit('mutant target is not unique: ' + name)
    try:
        path.write_text(original.replace(before, after))
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        Path('diagnostic-' + name + '.log').write_text(result.stdout)
        if result.returncode == 0 or expected not in result.stdout:
            print(result.stdout)
            raise SystemExit('mutant not killed by its assertion: ' + name)
    finally:
        path.write_text(original)
