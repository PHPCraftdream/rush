//go:build windows

package shexec

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// defaultKillTimeout matches mvdan's DefaultExecHandler default.
const DefaultKillTimeout = 2 * time.Second

// isolateProcess on Windows just hides the console window (session
// isolation via Setsid is a Unix-only concept; Windows process-group
// handling is left to our own exec handler below, which already creates
// child processes adequately detached for our purposes). Used by the
// shebang-script fallback path in dispatch.go.
func IsolateProcess(cmd *exec.Cmd) { platform.HideConsoleWindow(cmd) }

// processGroupExecHandler returns a Windows exec handler that behaves like
// interp.DefaultExecHandler but additionally hardens process creation —
// upstream's DefaultExecHandler builds a bare exec.Cmd with no
// SysProcAttr, so every single bash-tool command spawns a NEW, briefly
// visible console window when the rush process itself has no console to
// share (see cmd.maybeDetachConsole's doc comment for why rush ends up
// console-less on a detached/orchestrator launch).
//
// The hardening goes through platform.HideConsoleWindow rather than an
// inline SysProcAttr literal so this path can never drift from the
// sanctioned one: it sets CREATE_NO_WINDOW (the load-bearing half — the
// console is then created without a window at all) in addition to
// HideWindow (which only asks for an already-created window to be
// hidden, and so races with that window painting). See that function's
// doc comment for why HideWindow alone was never sufficient, despite
// this comment previously claiming it "sets the Windows CREATE_NO_WINDOW
// creation flag" — it does not.
func ProcessGroupExecHandler(killTimeout time.Duration, registerProcess func(int), retainedLaunch func(ctx context.Context, cmd *exec.Cmd, kill func() error, register func(int), afterStart func(*exec.Cmd) func()) (bool, error)) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    ExecEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
		}
		platform.HideConsoleWindow(&cmd)

		// The afterStart callback, used only on the retained launch route,
		// is called with the same command immediately after Start succeeds
		// and returns a release function invoked once the retained Wait
		// has returned. The Windows implementation uses it to create and
		// assign the per-command Job Object (see job_windows.go) and to
		// close its handle after the wait.
		if retainedLaunch != nil {
			var job commandJob
			if handled, err := retainedLaunch(ctx, &cmd,
				// Terminate the job first: TerminateJobObject reaches
				// orphaned descendants whose parents already exited, which
				// taskkill /T cannot (its tree walk needs live parents).
				// KillRetainedProcess stays as the fallback for the case
				// where the job could not be created or assigned.
				func() error {
					job.terminate()
					return KillRetainedProcess(cmd.Process.Pid)
				}, registerProcess,
				func(c *exec.Cmd) func() {
					// Ignore the error: on failure the kill closure falls
					// back to KillRetainedProcess, matching today's behavior.
					_ = assignProcessJob(c, &job)
					return job.close
				}); handled {
				return RetainedCommandResult(ctx, hc.Stderr, err)
			}
		}
		// The small window between Start and AssignProcessToJobObject is
		// accepted: children created inside it escape the job and remain
		// reachable only via the taskkill /T fallback. CREATE_SUSPENDED was
		// considered and deliberately NOT used because it would change
		// observable behavior (pipe/exit-status timing and potential
		// deadlocks on resume), which is not proven behavior-preserving
		// here.
		err = cmd.Start()
		if err == nil {
			var job commandJob
			// A failed job never fails the command; teardown then relies
			// on the taskkill /T fallback below, exactly as before.
			_ = assignProcessJob(&cmd, &job)
			if registerProcess != nil && cmd.Process != nil {
				registerProcess(cmd.Process.Pid)
			}
			var waitMu sync.Mutex
			waitFinished := false
			cancellationWon := false
			cancelStarted := make(chan struct{})
			cancelDone := make(chan struct{})
			stopf := context.AfterFunc(ctx, func() {
				close(cancelStarted)
				defer close(cancelDone)
				waitMu.Lock()
				if waitFinished {
					waitMu.Unlock()
					return
				}
				cancellationWon = true
				pid := cmd.Process.Pid
				waitMu.Unlock()
				// cmd.Stdout/Stderr here are plain io.Writers (our
				// boundedBuffer, not *os.File), so os/exec backs them with an
				// OS pipe and a copy-goroutine that cmd.Wait() joins. That
				// goroutine only sees EOF once EVERY process holding the
				// pipe's write end closes it. A plain Process.Signal —
				// Go doesn't support sending Interrupt on Windows anyway,
				// so this was always a hard kill — only terminates this
				// DIRECT child; any grandchild it spawned (a dev-server or
				// proxy forking a worker process is a common real case)
				// keeps its inherited copy of the handle open, so
				// cmd.Wait() then hangs indefinitely instead of returning
				// once the direct child is gone. Tree-kill via taskkill /T
				// instead, matching procinfo.KillProcess's approach to the
				// exact same class of problem in `rush sessions kill` —
				// it kills the direct process AND everything Windows
				// recorded as its descendant.
				// TerminateJobObject first: unlike taskkill /T it reaches
				// orphaned descendants whose parents already exited. The
				// direct-child kill below stays as the fallback for when
				// the job could not be created or assigned.
				job.terminate()
				if pid > 0 {
					_ = procinfo.KillProcess(pid)
				}
			})
			err = cmd.Wait()
			waitMu.Lock()
			waitFinished = true
			waitMu.Unlock()
			if !stopf() {
				<-cancelStarted
				<-cancelDone
			}
			// Close only after the kill callback is fully joined, so the
			// terminate inside it always sees a live handle.
			job.close()
			waitMu.Lock()
			canceled := cancellationWon
			waitMu.Unlock()
			return windowsCommandResult(ctx, hc.Stderr, err, canceled)
		}
		return windowsCommandResult(ctx, hc.Stderr, err, false)
	}
}

func KillRetainedProcess(pid int) error { return procinfo.KillProcess(pid) }

// ExecEnvList converts an expand.Environ to the []string form that
// os/exec.Cmd.Env expects. Only exported string variables are included,
// matching what a real shell would pass to a child process.
func ExecEnvList(env expand.Environ) []string {
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			out = append(out, name+"="+vr.Str)
		}
		return true
	})
	return out
}

func RetainedCommandResult(ctx context.Context, stderr io.Writer, err error) error {
	return windowsCommandResult(ctx, stderr, err, ctx.Err() != nil)
}

func windowsCommandResult(ctx context.Context, stderr io.Writer, err error, cancellationWon bool) error {
	if cancellationWon {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}

	switch err := err.(type) {
	case *exec.ExitError:
		return interp.ExitStatus(err.ExitCode())
	case *exec.Error:
		fmt.Fprintf(stderr, "%v\n", err)
		return interp.ExitStatus(127)
	default:
		return err
	}
}
