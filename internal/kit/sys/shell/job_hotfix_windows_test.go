//go:build windows

package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shexec"
	"golang.org/x/sys/windows"
)

// SAFETY: every test here can spawn a runaway process chain (the incident
// this hotfix fixes is a PATH shim that re-executes itself forever). Two
// guards are therefore mandatory and installed by installSafetyJob and
// startMarkerWatchdog below:
//
//  1. The test process itself is put into a Job Object with
//     JOB_OBJECT_LIMIT_ACTIVE_PROCESS 200, so even a worst-case escape is
//     capped while tests keep full process-creation freedom below it.
//  2. A t.Cleanup pass plus a watchdog goroutine (30s after test start)
//     enumerate processes and terminate BY PID every process whose command
//     line contains the test's unique temp directory. Kill is by PID only,
//     never by image name.
//
// REVERT CHECKS (per test below): remove the TerminateJobObject call in
// shexec/exec_windows.go → T2/T4 fail (the orphaned descendant survives);
// remove AssignProcessToJobObject → T1 runs unbounded until its deadline
// and the peak sample loses its bound; raise maxProcessesPerCommand back
// to a huge value → T1's peak assertion has no bite.

const (
	safetyJobProcessLimit = 200
	watchdogAfter         = 30 * time.Second
	markerPollInterval    = 50 * time.Millisecond
	// Peak slack: the job limit is enforced at process-creation time and
	// the 50ms poller observes a snapshot afterwards, so a handful of
	// processes created in flight around a rejection plus the poller's own
	// bookkeeping overlap are tolerated on top of the lowered limit.
	peakSlack = 8
)

// installSafetyJob puts the test process into a Job Object capped at
// safetyJobProcessLimit active processes. Failure is tolerated (the
// watchdog below still bounds damage) because the host job-nesting policy
// may refuse the assignment.
func installSafetyJob(t *testing.T) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Logf("safety job CreateJobObject failed: %v", err)
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			ActiveProcessLimit: safetyJobProcessLimit,
			LimitFlags:         windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Logf("safety job SetInformationJobObject failed: %v", err)
		windows.CloseHandle(job)
		return
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		t.Logf("safety job assignment refused (nested job?): %v", err)
		windows.CloseHandle(job)
		return
	}
	t.Cleanup(func() {
		// No kill-on-close flag was set, so this terminates nothing.
		windows.CloseHandle(job)
	})
}

// unicodeString mirrors Windows' UNICODE_STRING (not exported by
// golang.org/x/sys/windows); 64-bit layout, 16 bytes.
type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Pad           [4]byte
	Buffer        uintptr
}

// readProcessCmdline reads a process's command line via its PEB. The
// offsets are the classic 64-bit layout: PEB+0x20 is ProcessParameters and
// RTL_USER_PROCESS_PARAMETERS+0x70 is the CommandLine UNICODE_STRING.
func readProcessCmdline(pid uint32) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var pbi windows.PROCESS_BASIC_INFORMATION
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	r1, _, _ := ntdll.NewProc("NtQueryInformationProcess").Call(
		uintptr(h), 0, uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi), 0)
	if r1 != 0 || pbi.PebBaseAddress == nil {
		return "", false
	}
	pebBase := uintptr(unsafe.Pointer(pbi.PebBaseAddress))
	read := func(addr uintptr, size int) ([]byte, bool) {
		buf := make([]byte, size)
		var n uintptr
		if err := windows.ReadProcessMemory(h, addr, &buf[0], uintptr(size), &n); err != nil || n != uintptr(size) {
			return nil, false
		}
		return buf, true
	}
	ppRaw, ok := read(pebBase+0x20, int(unsafe.Sizeof(uintptr(0))))
	if !ok {
		return "", false
	}
	pp := *(*uintptr)(unsafe.Pointer(&ppRaw[0]))
	usRaw, ok := read(pp+0x70, int(unsafe.Sizeof(unicodeString{})))
	if !ok {
		return "", false
	}
	us := (*unicodeString)(unsafe.Pointer(&usRaw[0]))
	if us.Length == 0 || us.Buffer == 0 {
		return "", false
	}
	cmdRaw, ok := read(us.Buffer, int(us.Length))
	if !ok {
		return "", false
	}
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(&cmdRaw[0]))), true
}

// markerProcessInfo describes one live process relevant to a test marker.
type markerProcessInfo struct {
	pid     uint32
	cmdline string
}

// markerProcesses snapshots the process table and returns every process
// whose command line contains marker. When exeFilter is non-empty only
// processes whose image name matches it exactly are inspected; this keeps
// the 50ms polling cheap (the runaways here are always MSYS bash).
func markerProcesses(marker, exeFilter string) []markerProcessInfo {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []markerProcessInfo
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for ok := windows.Process32First(snap, &entry) == nil; ok; ok = windows.Process32Next(snap, &entry) == nil {
		if exeFilter != "" && !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), exeFilter) {
			continue
		}
		cmdline, ok := readProcessCmdline(entry.ProcessID)
		if !ok || !strings.Contains(cmdline, marker) {
			continue
		}
		out = append(out, markerProcessInfo{pid: entry.ProcessID, cmdline: cmdline})
	}
	return out
}

// killByPID terminates one process by its Windows PID.
func killByPID(pid uint32) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
		_ = windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

// killMarkerProcesses terminates every process whose command line contains
// marker, by PID, and returns how many were killed.
func killMarkerProcesses(marker string) int {
	killed := 0
	for _, p := range markerProcesses(marker, "") {
		killByPID(p.pid)
		killed++
	}
	return killed
}

// startMarkerWatchdog installs the mandatory t.Cleanup sweep and the 30s
// watchdog goroutine described at the top of this file.
func startMarkerWatchdog(t *testing.T, marker string) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		if n := killMarkerProcesses(marker); n > 0 {
			t.Logf("cleanup terminated %d leftover marker processes", n)
		}
	})
	go func() {
		timer := time.NewTimer(watchdogAfter)
		defer timer.Stop()
		select {
		case <-timer.C:
			killMarkerProcesses(marker)
		case <-done:
		}
	}()
}

// waitForMarkerOrphan polls until at least one bash process whose command
// line contains the marker substring is alive, or fails the test.
func waitForMarkerOrphan(t *testing.T, marker, substring string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range markerProcesses(marker, "bash.exe") {
			if strings.Contains(p.cmdline, substring) {
				return
			}
		}
		time.Sleep(markerPollInterval)
	}
	t.Fatalf("expected orphaned bash process with %q in its command line within 5s", substring)
}

// waitMarkerGone asserts that no bash process with the marker survives
// within 10 seconds.
func waitMarkerGone(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(markerProcesses(marker, "bash.exe")) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, p := range markerProcesses(marker, "bash.exe") {
				t.Logf("survivor pid=%d cmdline=%q", p.pid, p.cmdline)
			}
			t.Fatalf("marker processes still alive 10s after teardown")
		}
		time.Sleep(markerPollInterval)
	}
}

// hotfixEnv builds a full environment whose PATH resolves the unique temp
// directory first, using forward-slash-safe paths.
func hotfixEnv(t *testing.T, dir string) []string {
	t.Helper()
	marker := filepath.ToSlash(dir)
	return append(os.Environ(), "PATH="+marker+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeHotfixScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(dir, name))
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestRunBoundsSelfRecursivePathShim reproduces the incident: a PATH shim
// named git whose last line re-executes itself forever through PATH.
// REVERT CHECK: remove AssignProcessToJobObject in shexec → the chain is
// unbounded (the process cap never applies) and the peak assertion loses
// its enforcement; set maxProcessesPerCommand = math.MaxInt32 → same.
func TestRunBoundsSelfRecursivePathShim(t *testing.T) {
	installSafetyJob(t)
	dir := t.TempDir()
	marker := filepath.ToSlash(dir)
	startMarkerWatchdog(t, marker)
	writeHotfixScript(t, dir, "git", "exec git \"$@\"\n")

	restore := shexec.SetMaxProcessesPerCommand(24)
	defer restore()

	env := hotfixEnv(t, dir)
	// The marker is appended as an argument so every link of the re-exec
	// chain carries it in its command line and the poller can count it.
	cmd := "bash git rev-parse --is-inside-work-tree " + marker

	var peakMu sync.Mutex
	peak := 0
	pollStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(markerPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pollStop:
				return
			case <-ticker.C:
				peakMu.Lock()
				if n := len(markerProcesses(marker, "bash.exe")); n > peak {
					peak = n
				}
				peakMu.Unlock()
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	start := time.Now()
	err := Run(ctx, RunOptions{Command: cmd, Cwd: dir, Env: env, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	elapsed := time.Since(start)
	cancel()
	close(pollStop)

	if err == nil {
		t.Fatalf("Run returned nil error for a context-canceled runaway chain")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Run returned after %v, past the 15s deadline plus 5s slack", elapsed)
	}
	peakMu.Lock()
	got := peak
	peakMu.Unlock()
	if got > 24+peakSlack {
		t.Fatalf("peak live marker processes %d exceeded lowered limit 24 + slack %d", got, peakSlack)
	}
	waitMarkerGone(t, marker)
}

// TestRunCancelKillsOrphanedDescendants covers the incident's hang mode: a
// backgrounded sleeper outlives its parent and holds the output pipe.
// REVERT CHECK: remove the TerminateJobObject call in exec_windows.go →
// T2 fails (the orphan survives and the run never sees EOF).
func TestRunCancelKillsOrphanedDescendants(t *testing.T) {
	installSafetyJob(t)
	dir := t.TempDir()
	marker := filepath.ToSlash(dir)
	startMarkerWatchdog(t, marker)
	writeHotfixScript(t, dir, "sleeper.sh", "sleep 600\n")
	writeHotfixScript(t, dir, "orphan.sh", "bash \""+marker+"/sleeper.sh\" < /dev/null > /dev/null 2>&1 &\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Command: "bash \"" + marker + "/orphan.sh\"",
			Cwd:     dir,
			Env:     os.Environ(),
			Stdout:  &bytes.Buffer{},
			Stderr:  &bytes.Buffer{},
		})
	}()
	waitForMarkerOrphan(t, marker, "/sleeper.sh")
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return within 20s of cancellation")
	}
	waitMarkerGone(t, marker)
}

// TestRunLeavesDeliberateDaemonAfterNormalExit pins the no-KILL_ON_JOB_CLOSE
// invariant: a daemon a command deliberately leaves running must survive
// the command's completion. REVERT CHECK: add
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE to the job in shexec/job_windows.go →
// T3 fails (the daemon is killed when the job handle is closed).
func TestRunLeavesDeliberateDaemonAfterNormalExit(t *testing.T) {
	installSafetyJob(t)
	dir := t.TempDir()
	marker := filepath.ToSlash(dir)
	startMarkerWatchdog(t, marker)
	writeHotfixScript(t, dir, "daemon.sh", "sleep 600\n")
	writeHotfixScript(t, dir, "start.sh", "bash \""+marker+"/daemon.sh\" < /dev/null > /dev/null 2>&1 &\n")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := Run(ctx, RunOptions{
		Command: "bash \"" + marker + "/start.sh\"",
		Cwd:     dir,
		Env:     os.Environ(),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
	})
	if elapsed := time.Since(start); err != nil || elapsed > 10*time.Second {
		t.Fatalf("Run err=%v elapsed=%v, wanted prompt nil-error completion", err, elapsed)
	}

	deadline := time.Now().Add(10 * time.Second)
	var daemon markerProcessInfo
	for {
		found := markerProcesses(marker+"/daemon.sh", "bash.exe")
		if len(found) > 0 {
			daemon = found[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon with %q in its command line not found after Run", marker+"/daemon.sh")
		}
		time.Sleep(markerPollInterval)
	}
	if h, herr := windows.OpenProcess(windows.SYNCHRONIZE, false, daemon.pid); herr == nil {
		windows.CloseHandle(h)
	} else {
		t.Fatalf("daemon pid %d not alive after Run: %v", daemon.pid, herr)
	}
	// Cleanup kills the daemon by PID only, never by image name.
	t.Cleanup(func() { killByPID(daemon.pid) })
}

// TestRetainedStopKillsOrphanedDescendants covers the same T2 scenario
// through the retained background path, entered publicly via
// BackgroundShellManager.StartRetainedOwned (the lowest public retained
// entry; it requires a session ID string but is otherwise self-contained).
// REVERT CHECK: remove the terminate-before-kill ordering in the retained
// kill closure in exec_windows.go, or the afterStart hook that assigns the
// job on the retained path → T4 fails (the orphan survives Stop).
func TestRetainedStopKillsOrphanedDescendants(t *testing.T) {
	installSafetyJob(t)
	dir := t.TempDir()
	marker := filepath.ToSlash(dir)
	startMarkerWatchdog(t, marker)
	writeHotfixScript(t, dir, "sleeper.sh", "sleep 600\n")
	writeHotfixScript(t, dir, "orphan.sh", "bash \""+marker+"/sleeper.sh\" < /dev/null > /dev/null 2>&1 &\n")

	m := NewBackgroundShellManager()
	t.Cleanup(func() { m.KillAll(context.Background()) })

	execution, err := m.StartRetainedOwned(context.Background(), "hotfix-job-test", dir, nil, "bash \""+marker+"/orphan.sh\"", "hotfix job test")
	if err != nil {
		t.Fatalf("StartRetainedOwned: %v", err)
	}
	waitForMarkerOrphan(t, marker, "/sleeper.sh")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := execution.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitMarkerGone(t, marker)
}
