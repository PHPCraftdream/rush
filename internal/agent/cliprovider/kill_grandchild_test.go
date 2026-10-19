package cliprovider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// grandchildSleepDuration is deliberately non-round and grep-unique:
// this test's Windows orphan-cleanup backstop hunts leftover sleep.exe
// processes BY COMMAND LINE, and other packages' tests legitimately run
// round-number sleeps concurrently under the pre-push hook's -p 4
// package parallelism — a name-plus-recency hunt was observed killing
// TestBackgroundShell_AutoBackground's "sleep 20" ("exit status 255").
// Do not tidy this value back to a round number; see
// TestStreamKillUsesTreeKillStillTerminatesChild for the full war
// story.
const grandchildSleepDuration = "37.91"

// TestStreamKillTerminatesGrandchild is the regression test for the
// production defect where session.KillProcess reached only the DIRECT
// CLI child: on Unix the pipe-branch child was not a process-group
// leader and KillProcess signalled a single pid; on Windows taskkill
// /T cannot see a grandchild whose OS-recorded parent is an
// already-exited MSYS2 intermediary. Either way the grandchild was
// orphaned and kept running. The fix spawns pipe-branch children with
// Setpgid + group-kills confirmed leaders (Unix) and tracks the child
// tree on a KILL_ON_JOB_CLOSE Job Object that KillProcess terminates
// (Windows).
//
// Form: the CLI child (bash) spawns a grandchild (sleep) that
// outlives it; after ctx cancel — which routes through the stream's
// kill()/Stop → Cmd.Cancel / killChildProcess group kill — the
// grandchild must be dead.
//
// REVERT CHECK: reverting the group kill / Setpgid / the Job Object
// tracking makes this fail on the "still alive" assertion below.
func TestStreamKillTerminatesGrandchild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		noPTY bool
	}{
		// default: PTY branch on Unix (go-pty Setsid already makes
		// the child a group leader), plain pipe branch on Windows
		// (TestMain sets testDisablePTY there).
		{"default", false},
		// nopty: the merged stdout/stderr pipe branch everywhere —
		// the branch whose Unix children only become group leaders
		// via configureChildProcessGroup.
		{"nopty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shell, flag := "bash", "-c"
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("shell %q not found", shell)
			}

			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			// The grandchild sleeps; the script records its pid.
			// Under Git-Bash/MSYS2, $! is the emulated Cygwin-style
			// pid, NOT the Win32 pid tasklist/taskkill see. Resolve
			// the native WINPID in-script (so it cannot race the
			// kill): /proc/<pid>/winpid first, then ps -a's 4th
			// column — the same resolution sibling tests do. The
			// awk numeric-column guard keeps the ps fallback inert on
			// real Unix, where $4 of ps -a is not a pid (TIME/CMD),
			// so it falls back to $! itself there. NB the obvious
			// `[ -r /proc/$p/winpid ] && cat …` does NOT work on
			// observed Git-Bash builds: test -r succeeds on the
			// virtual path while cat fails, silently writing an
			// empty pid file — capture-and-check-empty instead.
			script := "sleep " + grandchildSleepDuration + " & p=$!; w=$(cat /proc/$p/winpid 2>/dev/null); if [ -z \"$w\" ]; then w=$(ps -a 2>/dev/null | awk -v p=$p '$1==p && $4 ~ /^[0-9]+$/ {print $4}'); fi; echo \"${w:-$p}\" > '" + pidFile + "'; wait"
			spec := CLISpec{
				ModelID:    "test-kill-grandchild",
				ModelName:  "Test Kill Grandchild",
				Binary:     shell,
				PromptFlag: "-p",
				NoPTY:      tc.noPTY,
				BuildArgs:  func(bool) []string { return []string{flag, script} },
			}
			workingDir := t.TempDir()
			m := &cliModel{spec: spec, workingDir: workingDir}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := m.Stream(ctx, fantasy.Call{
				Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
			})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range stream {
				}
			}()

			grandpid := 0
			// Orphan cleanup on EVERY exit path — pass, fail, or
			// Fatalf — never leave the sleeper behind. Registered
			// before the pid file is read so even that failure path
			// hunts by command line on Windows.
			t.Cleanup(func() {
				if grandpid > 0 {
					if runtime.GOOS == "windows" {
						_ = platform.Command(context.Background(), "taskkill", "/F", "/PID", fmt.Sprintf("%d", grandpid)).Run()
					} else {
						_ = platform.Command(context.Background(), shell, flag, fmt.Sprintf("kill -9 %d 2>/dev/null || true", grandpid)).Run()
					}
				}
				if runtime.GOOS == "windows" {
					// Command-line-matched backstop for the cases the
					// pid-based kill cannot cover (pid file never
					// read, winpid mismatch). Matched by the unique
					// duration literal, NEVER by bare name — see the
					// comment on grandchildSleepDuration.
					_ = platform.Command(context.Background(), "powershell", "-NoProfile", "-Command",
						`Get-CimInstance Win32_Process -Filter "Name='sleep.exe'" | Where-Object { $_.CommandLine -like '*`+grandchildSleepDuration+`*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`).Run()
				}
				waitForRemovable(t, workingDir, 5*time.Second)
			})

			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if data, rerr := os.ReadFile(pidFile); rerr == nil {
					if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
						grandpid = pid
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			if grandpid == 0 {
				t.Fatalf("grandchild pid file %s never became readable", pidFile)
			}

			// Premise guard: the grandchild must be observably ALIVE
			// before the cancel, or this test would prove nothing.
			sawAlive := false
			deadline = time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if processAlive(grandpid) {
					sawAlive = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !sawAlive {
				t.Fatalf("grandchild %d never observed alive before cancel — test premise broken (vacuous)", grandpid)
			}

			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("stream did not end after ctx cancel within 10s")
			}

			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !processAlive(grandpid) {
					return // grandchild died with the tree — fix verified
				}
				time.Sleep(50 * time.Millisecond)
			}
			if runtime.GOOS != "windows" {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				output, err := platform.Command(ctx, "ps", "-o", "pid,ppid,pgid,stat,command", "-p", strconv.Itoa(grandpid)).CombinedOutput()
				t.Logf("grandchild process diagnostics: output=%q error=%v", string(output), err)
			}
			t.Errorf("grandchild pid %d still alive after kill() — tree-kill does not reach grandchildren (regression of the orphaned-grandchild fix)", grandpid)
		})
	}
}

func TestConfigureChildProcessGroupKillsGrandchild(t *testing.T) {
	for _, mode := range []string{"cancel", "leader-exited"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("Unix process groups only")
			}

			if _, err := exec.LookPath("bash"); err != nil {
				t.Skipf("shell %q not found", "bash")
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			script := "sleep " + grandchildSleepDuration + " & p=$!; echo $p; "
			if mode == "leader-exited" {
				script += "exit 0"
			} else {
				script += "wait"
			}
			cmd := platform.Command(context.Background(), "bash", "-c", script)
			cmd.Stdout = writer
			configureChildProcessGroup(cmd)
			if err := cmd.Start(); err != nil {
				_ = reader.Close()
				_ = writer.Close()
				t.Fatal(err)
			}
			_ = writer.Close()
			grandchildPID := 0
			waited := false
			t.Cleanup(func() {
				if grandchildPID > 0 {
					_ = platform.Command(context.Background(), "kill", "-9", fmt.Sprint(grandchildPID)).Run()
				}
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				_ = reader.Close()
				_ = writer.Close()
			})

			if err := reader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("set grandchild PID read deadline: %v", err)
			}
			line, err := bufio.NewReader(reader).ReadString('\n')
			if err != nil {
				t.Fatalf("read grandchild pid: %v", err)
			}
			grandchildPID, err = strconv.Atoi(strings.TrimSpace(line))
			if err != nil || grandchildPID <= 0 {
				t.Fatalf("invalid grandchild pid %q: %v", line, err)
			}

			if mode == "cancel" {
				if cmd.Cancel == nil {
					t.Fatal("configureChildProcessGroup did not install Cancel")
				}
				if err := cmd.Cancel(); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
				_ = cmd.Wait()
				waited = true
			} else {
				if err := cmd.Wait(); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				waited = true
				if err := killChildProcess(cmd.Process); err != nil {
					t.Fatalf("killChildProcess after leader exit: %v", err)
				}
			}

			if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set inherited pipe read deadline: %v", err)
			}
			var one [1]byte
			_, err = reader.Read(one[:])
			if err != io.EOF {
				t.Fatalf("grandchild still holds inherited pipe after %s: read error=%v", mode, err)
			}
		})
	}
}
