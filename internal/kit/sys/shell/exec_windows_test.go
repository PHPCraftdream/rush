//go:build windows

package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shexec"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// helperEnvVar switches this test binary into one of two "leaky process
// tree" helper modes (see TestMain) instead of running the normal test
// suite. This is the standard Go idiom for reproducing real OS-level
// process/handle-inheritance behavior deterministically (the same pattern
// os/exec's own tests use), rather than depending on external tools like
// cmd.exe's `start` or PowerShell's `Start-Process`, whose handle
// inheritance semantics are not guaranteed to be stable across Windows
// versions or CI images.
const helperEnvVar = "RUSH_SHELL_TEST_HELPER"

func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: isolate global state: %v\n", err)
		os.Exit(1)
	}
	switch os.Getenv(helperEnvVar) {
	case "level1":
		runLevel1Helper()
		return
	case "level2":
		runLevel2Helper()
		return
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// runLevel1Helper models the "direct child" a background shell command
// tracks — e.g. a dev-server or proxy process. It forks a grandchild
// (level 2) that inherits its own stdout/stderr handles without waiting for
// it, then stays alive (like a real long-running server) until killed.
func runLevel1Helper() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	cmd := platform.Command(context.Background(), self)
	cmd.Env = append(os.Environ(), helperEnvVar+"=level2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	// Fire-and-forget: do NOT Wait() on the grandchild — it must outlive
	// this process's own death exactly like an orphaned worker would.
	fmt.Println("level1-alive")
	time.Sleep(30 * time.Second)
}

// runLevel2Helper models the orphaned grandchild: it inherited level 1's
// stdout/stderr handle and keeps it open long after level 1 is gone.
func runLevel2Helper() {
	fmt.Println("grandchild-alive")
	time.Sleep(60 * time.Second)
}

// TestBackgroundShellManager_Kill_TreeKillsOrphanedGrandchild_Windows proves
// killing a background shell tree-kills a grandchild the direct child
// spawned, instead of hanging.
//
// Before the exec_windows.go fix, cancelling ctx only called
// cmd.Process.Signal(os.Kill) on the DIRECT child (level 1 here). Since
// cmd.Stdout/Stderr are plain io.Writers, os/exec backs them with an OS
// pipe whose copy-goroutine cmd.Wait() joins — that goroutine only sees EOF
// once every handle holder closes it. The still-alive grandchild (level 2)
// keeps its inherited copy of the handle open even after level 1 dies, so
// cmd.Wait() — and therefore execCommon/runner.Run, and therefore
// bgShell.done — never unblocks on its own; against the old code this test
// would hang for the grandchild's full sleep (60s) instead of returning
// promptly. This is exactly the failure mode observed in production: a
// batch_stop call against a proxy process that had forked a worker blocked
// for the full 15-minute tool watchdog instead of returning immediately.
func TestBackgroundShellManager_Kill_TreeKillsOrphanedGrandchild_Windows(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	require.NoError(t, err)

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Leading VAR=value is POSIX inline-assignment syntax: mvdan/sh scopes
	// it to this one command's environment without polluting the shell's
	// own. filepath.ToSlash avoids POSIX backslash-escape ambiguity in the
	// Windows path (see the package doc comment in shell.go).
	command := fmt.Sprintf("%s=level1 %s", helperEnvVar, filepath.ToSlash(self))

	bgShell, err := manager.Start(t.Context(), workingDir, nil, command, "")
	require.NoError(t, err)

	// Wait until BOTH helper levels have actually reported alive before
	// killing — otherwise we might kill before the grandchild even exists,
	// which wouldn't reproduce the bug.
	require.Eventually(t, func() bool {
		stdout, _, _, _ := bgShell.GetOutput()
		return strings.Contains(stdout, "level1-alive") && strings.Contains(stdout, "grandchild-alive")
	}, 5*time.Second, 50*time.Millisecond, "helper processes did not report alive")

	start := time.Now()
	err = manager.Kill(context.Background(), bgShell.ID)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// Tree-kill normally returns in well under 2s. The 30s bound (not a
	// tight one) exists only to distinguish "prompt" from the broken
	// behavior this test guards against — hanging for the grandchild's
	// full 60s sleep — while leaving headroom for process-spawn/taskkill
	// latency under a loaded machine (observed: 10.85s against a 10s
	// bound during a full `-race` package-suite run; isolated runs are
	// consistently under 2s). A real regression back to the old
	// direct-child-only kill would still take the full 60s, nowhere
	// close to this bound either way.
	//
	// Note: this 30s bound numerically matches runLevel1Helper's own 30s
	// time.Sleep (see above) — that's coincidental, not load-bearing. The
	// discrimination this assertion relies on is driven by the grandchild's
	// handle (60s sleep, level 2), not level 1's sleep duration: a broken
	// (non-tree-kill) implementation hangs on the grandchild's inherited
	// stdout/stderr handle regardless of how long level 1 itself would have
	// slept, so this bound would still correctly fail even if level 1's
	// sleep were changed to something other than 30s.
	require.Less(t, elapsed, 30*time.Second,
		"Kill must tree-kill the orphaned grandchild and return promptly; "+
			"without tree-kill it hangs until the grandchild's own sleep ends")
}

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

// killByPID terminates one process by its Windows PID and waits (up to 5s)
// for it to actually exit so its cwd/handles are released.
func killByPID(pid uint32) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
	_, _ = windows.WaitForSingleObject(h, 5000)
}

// childPIDs returns the direct children of pid from a process snapshot.
func childPIDs(pid uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []uint32
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for ok := windows.Process32First(snap, &entry) == nil; ok; ok = windows.Process32Next(snap, &entry) == nil {
		if entry.ParentProcessID == pid && entry.ProcessID != pid {
			out = append(out, entry.ProcessID)
		}
	}
	return out
}

// killTreeByPID kills pid's descendants (by PID) and then pid itself, waiting
// for each to exit.
func killTreeByPID(pid uint32) {
	for _, c := range childPIDs(pid) {
		killTreeByPID(c)
	}
	killByPID(pid)
}

// killMarkerProcesses terminates every process whose command line contains
// marker, by PID, and returns how many were killed. It re-sweeps until a
// sweep finds nothing (up to 10s) because a re-exec chain keeps creating a
// successor faster than a single sweep can kill it.
func killMarkerProcesses(marker string) int {
	killed := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		procs := markerProcesses(marker, "")
		if len(procs) == 0 {
			break
		}
		for _, p := range procs {
			killByPID(p.pid)
			killed++
		}
	}
	// Verify the final state is empty before returning.
	verify := time.Now().Add(5 * time.Second)
	for len(markerProcesses(marker, "")) > 0 && time.Now().Before(verify) {
		for _, p := range markerProcesses(marker, "") {
			killByPID(p.pid)
		}
		time.Sleep(markerPollInterval)
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
// named git whose last line re-executes itself forever through PATH, started
// as an orphan chain that holds the output pipe. The live process count
// stays tiny (MSYS exec replaces the old process), so the ACTIVE_PROCESS
// limit alone does not bound it; what bounds it is TerminateJobObject on
// cancellation. REVERT CHECK (run for real): making assignProcessJob return
// ErrJobUnavailable -> Run never returns after the deadline and this test
// fails after 13s ("orphaned chain survived cancellation").
func TestRunBoundsSelfRecursivePathShim(t *testing.T) {
	installSafetyJob(t)
	dir := t.TempDir()
	// MSYS rewrites the temp dir in command lines (D:\...\Temp\X becomes
	// /tmp/X), so the unique per-test directory NAME is the marker.
	marker := filepath.Base(filepath.Dir(dir))
	startMarkerWatchdog(t, marker)
	// The shebang is load-bearing: MSYS only treats an extensionless
	// script as executable (and so finds it through PATH on `exec git`)
	// when it starts with #!. Without it bash silently skips the shim and
	// runs the real git, and no recursion happens.
	writeHotfixScript(t, dir, "git", "#!/bin/bash\nexec git \"$@\"\n")

	restore := shexec.SetMaxProcessesPerCommand(24)
	defer restore()

	env := hotfixEnv(t, dir)
	// A real bash looks git up through PATH and finds the shim; every
	// re-exec link then runs as `bash <tmpdir>/git ...` and carries the
	// unique marker in its command line. MSYS exec creates the new process
	// and the old one dies, so the LIVE count stays tiny while the chain
	// churns through ever new PIDs: the poller counts distinct PIDs seen.
	//
	// The chain is started in the background so its root's parent exits at
	// once: the chain is then an ORPHAN that still holds the output pipe,
	// reachable by TerminateJobObject only (taskkill /T needs live parents).
	cmd := "bash -c 'git rev-parse --is-inside-work-tree &'"

	var mu sync.Mutex
	peak := 0
	seen := map[uint32]bool{}
	pollStop := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		ticker := time.NewTicker(markerPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pollStop:
				return
			case <-ticker.C:
				procs := markerProcesses(marker, "bash.exe")
				mu.Lock()
				if len(procs) > peak {
					peak = len(procs)
				}
				for _, p := range procs {
					seen[p.pid] = true
				}
				mu.Unlock()
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	began := time.Now()
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, RunOptions{Command: cmd, Cwd: dir, Env: env, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	}()
	var err error
	select {
	case err = <-runDone:
	case <-time.After(13 * time.Second):
		// Without the job the orphaned chain holds the output pipe and Run
		// never returns; fail fast (cleanup sweeps the chain by PID).
		cancel()
		close(pollStop)
		<-pollDone
		t.Fatalf("Run did not return within 13s (8s deadline + 5s): orphaned chain survived cancellation")
	}
	elapsed := time.Since(began)
	cancel()
	close(pollStop)
	<-pollDone

	mu.Lock()
	gotPeak, gotSeen := peak, len(seen)
	mu.Unlock()
	t.Logf("recursion: peak live=%d distinct pids seen=%d elapsed=%v run err=%v", gotPeak, gotSeen, elapsed, err)
	if gotSeen < 5 {
		t.Fatalf("recursion never ran: only %d distinct marker processes seen", gotSeen)
	}
	if err == nil {
		t.Fatalf("Run returned nil error for a context-canceled runaway chain")
	}
	if elapsed > 13*time.Second {
		t.Fatalf("Run returned after %v, past the 8s deadline plus 5s slack", elapsed)
	}
	if gotPeak > 24+peakSlack {
		t.Fatalf("peak live marker processes %d exceeded lowered limit 24 + slack %d", gotPeak, peakSlack)
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
	writeHotfixScript(t, dir, "orphan.sh", "bash \""+marker+"/sleeper.sh\" < /dev/null &\n")

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
	t.Cleanup(func() { killTreeByPID(daemon.pid) })
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
	writeHotfixScript(t, dir, "orphan.sh", "bash \""+marker+"/sleeper.sh\" < /dev/null &\n")

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
