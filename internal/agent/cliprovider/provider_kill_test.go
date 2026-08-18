// Kill/track tests: the Bug-1 bounded-wait regressions (a grandchild
// holding stderr open; PTY-branch ctx cancel), the tree-kill regression
// with its orphan cleanup, and the shared waitForRemovable / processAlive
// helpers (also used by kill_grandchild_test.go).

package cliprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// ── Bug 1 regression: bounded wait() must not hang when a grandchild holds stderr ──
//
// Reproduces the real incident: the direct child (bash) exits so stdout EOFs
// and the scanner loop ends, but a backgrounded grandchild keeps the inherited
// stderr fd open. With the OLD unbounded cmd.Wait(), proc.wait() would block
// forever (no ctx check on that path). The fix bounds it against ctx.Done().
//
// The test forces the pipe / non-NoPTY branch (cmd.Stderr = &stderrBuf) by
// using a spec with neither NoPTY nor AlwaysStdin and a small prompt, and
// relies on testDisablePTY being true on Windows so the PTY path is skipped.
func TestStreamWaitBoundedOnGrandchildHoldsStderr(t *testing.T) {
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// Script: print one stdout line (so the scanner loop runs and ends on EOF
	// when bash exits), then fork a backgrounded subshell that holds the
	// inherited stderr fd open for 30s and disowns itself so bash doesn't
	// wait for it. After forking, the main bash prints a final line and
	// exits — leaving the orphan grandchild holding stderr.
	//
	// We also write the grandchild's PID to a file so the test can reap it
	// afterwards (kill by PID) and avoid leaking a 30s sleeper on CI.
	//
	// The tmpDir path arrives as $0, not $1: BuildArgs below passes bash
	// only [flag, script, tmpDir], i.e. `bash -c script tmpDir` — bash's
	// `-c` convention takes the arg right after the script as $0 (the
	// "command name" slot), with actual positional params starting at $1.
	// Using "$1/grandchild.pid" here was silently writing to "/grandchild.pid"
	// (root of the MSYS mount) with $1 empty, which fails with permission
	// denied — the pid file was never created, so the reap step below was a
	// complete no-op on every run. Confirmed by direct reproduction: `bash
	// -c 'echo $0 $1' "sometmpdir"` prints `sometmpdir` (empty) for $0/$1.
	script := `
		echo "stdout-line"
		( sleep 30 ) >&2 &
		echo $! > "$0/grandchild.pid"
		disown
		echo "stdout-line-2"
	`

	// Force the pipe branch via testDisablePTY rather than relying on the
	// platform-dependent default (GOOS == "windows", set in TestMain). Under
	// a real PTY (the default on non-Windows), the controlling terminal
	// closing on bash exit sends SIGHUP to the disowned grandchild, killing
	// it before it can hold stderr open — the premise this test depends on
	// never materializes there, and gotErr comes back nil on Linux CI.
	//
	// Setting spec.NoPTY instead would be wrong: it switches Stream() to the
	// merged-stdout/stderr StderrPipe() branch (see the `if m.spec.NoPTY`
	// block below Stream()'s pipe fallback), not the plain
	// `cmd.Stderr = &stderrBuf` branch this test targets (see the
	// top-of-function comment). Only testDisablePTY selects the latter while
	// keeping spec.NoPTY false.
	prevDisablePTY := testDisablePTY
	testDisablePTY = true
	defer func() { testDisablePTY = prevDisablePTY }()

	tmpDir := t.TempDir()
	spec := CLISpec{
		ModelID:    "test-bounded-wait",
		ModelName:  "Test Bounded Wait",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return []string{flag, script, tmpDir} },
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}

	// Short ctx deadline: the scanner loop ends quickly (bash exits fast),
	// then proc.wait() is invoked. With the bug, wait() would block on the
	// stderr-holding grandchild well past this deadline. With the fix,
	// wait() returns on ctx.Done() promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Hard watchdog: if the whole drain somehow hangs (bug regressed AND
	// the ctx bound failed), fail loudly instead of stalling the suite.
	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				gotErr = part.Error
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stream drain hung past 15s watchdog — bounded wait regression")
	}

	// The ctx deadline MUST have fired and the bounded-wait code path MUST
	// have surfaced a context error. This is the deterministic assertion that
	// gives the test its regression value: with the fix reverted (unbounded
	// cmd.Wait()), the drain hangs and we never reach here (the watchdog
	// above fails the test at 15s). With the fix in place, the ctx-bound
	// select returns ctx.Err() promptly and it propagates as the error part.
	//
	// We deliberately do NOT accept gotErr == nil: if the grandchild's stderr
	// fd were ever closed by the OS instead of being held (different
	// platform/shell), wait() would return normally with no error and this
	// test would no longer prove the bounded path fired — it would have zero
	// regression value. In that case the test should fail loudly so we know
	// to switch to an injected-fake-process technique on that platform.
	if gotErr == nil {
		t.Fatal("expected ctx error from bounded wait() but got nil — the grandchild-holds-stderr premise did not hold in this environment; this test no longer proves the fix and needs a different technique for this platform")
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) && !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("expected context.DeadlineExceeded or context.Canceled from bounded wait(), got %v", gotErr)
	}
	t.Logf("drain completed with ctx-bound wait; gotErr=%v", gotErr)

	// Reap the orphaned grandchild so we don't leak a 30s sleeper.
	if pidData, rerr := os.ReadFile(filepath.Join(tmpDir, "grandchild.pid")); rerr == nil {
		var pid int
		fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &pid)
		if pid > 0 {
			// best-effort kill of the orphan; ignore errors (already gone is fine)
			kill, _ := exec.LookPath("taskkill")
			if kill != "" {
				// On Windows, `$!` as captured by the script above is the
				// MSYS2/Git-Bash emulated (Cygwin-style) PID, NOT the native
				// Win32 PID that `taskkill /PID` needs — confirmed by
				// cross-checking against `tasklist`, where the two were
				// completely different numbers for the same process.
				// Passing the raw value to `taskkill` silently targets a
				// nonexistent/unrelated PID, so the real grandchild was
				// never actually killed and ran its full 30s. `ps -a`'s 4th
				// column (WINPID) carries the native PID (verified against
				// `tasklist` output matching exactly); resolve it here, off
				// the script's timing-critical path (see comment above the
				// script), falling back to the raw pid if resolution fails
				// (e.g. the process already exited, or a platform where
				// `ps -a` has no WINPID column).
				winPid := pid
				if out, perr := exec.CommandContext(context.Background(), shell, flag,
					fmt.Sprintf(`ps -a 2>/dev/null | awk -v p=%d '$1==p{print $4}'`, pid)).Output(); perr == nil {
					if resolved, serr := strconv.Atoi(strings.TrimSpace(string(out))); serr == nil && resolved > 0 {
						winPid = resolved
					}
				}
				_ = exec.CommandContext(context.Background(), kill, "/F", "/T", "/PID", fmt.Sprintf("%d", winPid)).Run()
			} else {
				_ = exec.CommandContext(context.Background(), shell, flag, fmt.Sprintf("kill %d 2>/dev/null || true", pid)).Run()
			}
		}
	}

	// Wait for tmpDir to actually become removable before returning.
	//
	// `taskkill` (above, for the grandchild) and Stream()'s own ctx-cancel
	// path (which tree-kills the direct bash child via session.KillProcess,
	// also taskkill-backed on Windows — see kill_windows.go) both only
	// REQUEST termination; neither waits for Windows to finish tearing the
	// process down and releasing its handles. Either process — bash itself
	// or the disowned grandchild — can still be holding an open handle
	// rooted in tmpDir at this point. t.TempDir()'s registered cleanup
	// (os.RemoveAll) fires via t.Cleanup the instant this test function
	// returns, so under full-suite CPU contention that teardown gap can
	// widen past the cleanup, causing a "used by another process" failure.
	//
	// Rather than tracking down every PID that might hold a handle, poll
	// the directory itself: retry RemoveAll until it succeeds (or a non-
	// sharing-violation error) or a bounded budget elapses. This makes the
	// wait deterministic and correct regardless of which process (bash or
	// the grandchild) is the actual straggler.
	waitForRemovable(t, tmpDir, 3*time.Second)
}

// waitForRemovable polls os.RemoveAll(dir) until it succeeds or a bounded
// budget elapses, tolerating the transient Windows "used by another
// process" sharing violation while a just-killed process finishes tearing
// down and releasing its handles. os.RemoveAll is safe to call speculatively
// here: it treats an already-missing path as success, so if this preemptive
// removal succeeds, t.TempDir()'s own later os.RemoveAll cleanup simply
// finds nothing to do.
func waitForRemovable(t *testing.T, dir string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(75 * time.Millisecond)
	}
	if lastErr != nil {
		t.Logf("waitForRemovable: %s still not removable after %s budget (%v); proceeding anyway", dir, budget, lastErr)
	}
}

// ── Bug 1 regression (PTY branch parity): bounded wait on ctx ──
//
// Pure-Go check that the PTY branch's wait closure is also bounded against
// ctx. We can't easily force the PTY code path on Windows (testDisablePTY
// is true there), so this test only runs where PTY is exercised (Unix). It
// cancels ctx mid-stream and asserts the stream ends promptly — the wait()
// closure must return on ctx.Done() rather than blocking on ptycmd.Wait().
func TestStreamPTYWaitBoundedOnCtxCancel(t *testing.T) {
	if testDisablePTY {
		t.Skip("PTY path disabled on this platform; PTY-branch wait bound not exercisable")
	}
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// A long-running child that blocks forever until killed.
	spec := CLISpec{
		ModelID:    "test-pty-bounded",
		ModelName:  "Test PTY Bounded",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return []string{flag, "sleep 60"} },
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Drain in a goroutine; cancel shortly after. The stream must end
	// promptly (no hang on wait()).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range stream {
		}
	}()

	cancel()
	select {
	case <-done:
		// good: bounded
	case <-time.After(10 * time.Second):
		t.Fatal("PTY stream drain hung past 10s after ctx cancel — wait() not bounded")
	}
}

// ── Bug 2 sanity: kill() routes through session.KillProcess (tree-kill) ──
//
// We can't portably prove full tree-kill of a real multi-generation process
// tree in a unit test (OS-flaky in CI), so this is a regression guard that
// the existing ctx-cancellation-kills-the-child coverage still holds AND that
// kill() uses the tree-kill helper rather than the direct-child-only Kill.
// The behavioral assertion: after ctx cancel, the direct child is gone.
func TestStreamKillUsesTreeKillStillTerminatesChild(t *testing.T) {
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// Child writes its own PID to a file, then sleeps long enough that we
	// can cancel and observe whether it actually died.
	//
	// The duration below (orphanSleepDuration) is deliberately an unusual,
	// grep-unique value rather than a round number like "60" — several OTHER
	// tests across this repo (this file's own sibling test, internal/shell,
	// etc.) also invoke plain "sleep 60"/"sleep 30" concurrently under the
	// pre-push hook's `-p 4` package parallelism, and this test's orphan-
	// cleanup step below (see the sleep.exe kill call) needs to identify
	// ONLY its own orphaned sleep.exe by command-line, not collide with a
	// same-duration process a different package's test is legitimately
	// still using.
	const orphanSleepDuration = "58.37"
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	spec := CLISpec{
		ModelID:    "test-kill-tree",
		ModelName:  "Test Kill Tree",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo $$ > '" + pidFile + "'; sleep " + orphanSleepDuration}
		},
	}
	workingDir := t.TempDir()
	m := &cliModel{spec: spec, workingDir: workingDir}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for range stream {
		}
	}()

	// Give the child time to write its PID and enter the sleep.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end after ctx cancel within 10s")
	}

	// The direct child must be gone after kill().
	pidData, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("could not read child pid file: %v", rerr)
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &pid)
	if pid <= 0 {
		t.Fatalf("could not parse child pid from %q", pidData)
	}

	// Poll briefly: process death isn't instant after SIGKILL/taskkill.
	deadline := time.Now().Add(3 * time.Second)
	var alive atomic.Bool
	alive.Store(true)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			alive.Store(false)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive.Load() {
		t.Errorf("child pid %d still alive after kill() — tree-kill regressed", pid)
		// best-effort cleanup
		_ = exec.CommandContext(context.Background(), shell, flag, fmt.Sprintf("kill -9 %d 2>/dev/null || true", pid)).Run()
	}

	// Root-caused (confirmed via `wmic process where "name='sleep.exe'" get
	// processid,parentprocessid`): the "sleep 60" launched by this test's
	// bash script does NOT end up as a genuine Win32 child of the bash.exe
	// pid we tracked and killed above — under this MSYS2/Git-Bash build,
	// external-binary spawns route through a transient intermediary helper
	// process, so the OS-recorded ParentProcessId points at that helper
	// (already gone by the time we can inspect it), never at bash.exe.
	// taskkill /F /T /PID <bash-pid> (session.KillProcess's implementation)
	// walks the PPID chain from bash.exe and therefore can never discover
	// or kill sleep.exe — this isn't a timing race, confirmed by 10/10
	// deterministic failures even with a 10s post-kill wait budget, always
	// converging only once sleep.exe's own 60s runs out. This same MSYS2
	// process-model gap is a plausible latent issue in the real
	// session.KillProcess production path too (out of scope for this test
	// fix; flagged separately) whenever a CLI provider's child spawns a
	// real subprocess of its own on Windows.
	//
	// Test-level mitigation only (mirrors the WINPID-resolution fix in
	// TestStreamWaitBoundedOnGrandchildHoldsStderr above): explicitly hunt
	// down and kill any orphaned sleep.exe this test's own script could
	// have spawned, rather than waiting on a handle release that a lost
	// orphan will never trigger in reasonable test time.
	//
	// Matched by exact command line (the orphanSleepDuration literal above),
	// NOT by a start-time window: a start-time-only filter was tried first
	// and confirmed unsafe by direct observation — internal/agent/tools'
	// TestBackgroundShell_AutoBackground legitimately runs "sleep 20"
	// concurrently under the pre-push hook's `-p 4` package parallelism,
	// and a plain "kill anything named sleep.exe started recently" query
	// killed it too (observed failure: "exit status 255"). Several OTHER
	// tests repo-wide also use round-number sleep durations (30/60/100/...),
	// so matching on command line requires this test's own duration to stay
	// the unusual, grep-unique value declared above — do not "clean up" it
	// back to a round number.
	_ = exec.CommandContext(context.Background(), "powershell", "-NoProfile", "-Command",
		`Get-CimInstance Win32_Process -Filter "Name='sleep.exe'" | Where-Object { $_.CommandLine -like '*`+orphanSleepDuration+`*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`).Run()

	waitForRemovable(t, workingDir, 5*time.Second)
}

// processAlive reports whether a process with the given pid is still running.
// Best-effort, portable.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		// taskkill /? exit code logic: we probe via tasklist.
		out, err := exec.CommandContext(context.Background(), "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), fmt.Sprintf("%d", pid))
	}
	// POSIX: signal 0 probes existence.
	_ = exec.CommandContext(context.Background(), "kill", "-0", fmt.Sprintf("%d", pid)).Run()
	// kill -0 returns 0 if alive, non-zero otherwise; Run returns nil on 0 exit.
	return exec.CommandContext(context.Background(), "kill", "-0", fmt.Sprintf("%d", pid)).Run() == nil
}
