package hooks

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// abandonGrace is how long runOne waits after ctx cancellation for the
// shell goroutine to yield before returning control to the caller and
// letting the goroutine finish on its own. Mirrors the historical
// cmd.WaitDelay = time.Second behavior of the previous os/exec path.
const abandonGrace = time.Second

// maxAbandonedWorkers caps how many hook goroutines may be simultaneously
// abandoned (still running past timeout+abandonGrace, e.g. a failed kill,
// a nested interpreter, or a process holding a pipe open). The normal
// Windows/Unix exec path already tree-kills the underlying process on ctx
// cancellation (see shell.processGroupExecHandler), so this cap guards the
// rarer case where that kill doesn't land and the worker goroutine keeps
// running indefinitely. 32 is generous for any realistic hook fan-out
// (Run() dedupes by command and a single event rarely matches more than a
// handful of hooks) while still being small enough that hitting it is a
// clear signal something is systemically wedged rather than normal load.
// Enforcement: Run checks the gauge before spawning workers and rejects
// (non-blocking, loud error log) once it is at/above the cap; the check is
// advisory under concurrency (a burst that passes the check before the
// first abandonment lands can still overshoot by its in-flight size).
const maxAbandonedWorkers = 32

// abandonedWorkers tracks hook goroutines currently abandoned past
// timeout+abandonGrace (increment on abandon, decrement when the worker
// eventually finishes), so their number is observable (AbandonedWorkers)
// instead of silently growing without bound.
var abandonedWorkers atomic.Int64

// AbandonedWorkers returns the number of hook worker goroutines currently
// abandoned past their timeout+abandonGrace deadline. Intended for
// diagnostics/metrics.
func AbandonedWorkers() int64 {
	return abandonedWorkers.Load()
}

// abandonSeam, when non-nil, is called in place of the real hard-kill
// attempt on the abandon path, receiving the pids registered by the
// wedged worker via shell.RunOptions.RegisterProcess. Test-only hook so
// tests can observe/force that path deterministically without depending
// on OS process timing or killing real pids.
var abandonSeam func(pids []int)

// runShell is the shell executor used by runOne. It is a package-level
// variable so tests can substitute a blocking or non-yielding
// implementation to exercise the abandon-on-timeout path without
// depending on the scheduling behavior of the real interpreter.
var runShell = shell.Run

// seamMu guards runShell and abandonSeam. Both are read exactly once per
// hook invocation, from a goroutine whose scheduling is decoupled from
// the wall-clock timeout logic that governs when a test's own r.Run()
// call returns and its cleanup runs -- a bare package-var swap races the
// read with a later test's restore/reassignment under the Go memory
// model even when, in practice, whole seconds of real time separate
// them (confirmed by `go test -race`, which caught exactly this: a read
// at runOne's `runShell(ctx, ...)` call site racing a subsequent test's
// cleanup, despite the reading goroutine's owning r.Run() call having
// already returned by the time that cleanup ran). The mutex does not
// change *which* function value a maximally-delayed reader ends up
// observing -- that stays a harmless, pre-existing timing
// nondeterminism inherent to swapping a test seam -- it only makes the
// access itself properly synchronized, which is what both the race
// detector and the memory model require.
var seamMu sync.RWMutex

// getRunShell returns the current runShell value under seamMu's read
// lock. Use this at every production call site instead of reading the
// bare variable.
func getRunShell() func(context.Context, shell.RunOptions) error {
	seamMu.RLock()
	defer seamMu.RUnlock()
	return runShell
}

// getAbandonSeam returns the current abandonSeam value under seamMu's
// read lock. Use this at every production call site instead of reading
// the bare variable.
func getAbandonSeam() func(pids []int) {
	seamMu.RLock()
	defer seamMu.RUnlock()
	return abandonSeam
}

// compiledHook pairs a HookConfig with its compiled matcher regex. A nil
// matcher means "match every tool".
type compiledHook struct {
	cfg     config.HookConfig
	matcher *regexp.Regexp
}

// Runner executes hook commands and aggregates their results.
type Runner struct {
	hooks      []compiledHook
	cwd        string
	projectDir string
}

// NewRunner creates a Runner from the given hook configs. Each hook's
// Matcher is compiled here so the Runner is self-sufficient; callers do
// not have to pre-compile matchers on the config, and reloads or merges
// that rebuild HookConfig values can't silently strip compiled state.
//
// Hooks whose matcher fails to compile are skipped with a warning rather
// than treated as match-everything. ValidateHooks is expected to have
// caught syntax errors earlier, so this is defense in depth.
func NewRunner(hooks []config.HookConfig, cwd, projectDir string) *Runner {
	compiled := make([]compiledHook, 0, len(hooks))
	for _, h := range hooks {
		ch := compiledHook{cfg: h}
		if h.Matcher != "" {
			re, err := regexp.Compile(h.Matcher)
			if err != nil {
				slog.Warn(
					"Hook matcher failed to compile; skipping hook",
					"matcher", h.Matcher,
					"command", h.Command,
					"error", err,
				)
				continue
			}
			ch.matcher = re
		}
		compiled = append(compiled, ch)
	}
	return &Runner{
		hooks:      compiled,
		cwd:        cwd,
		projectDir: projectDir,
	}
}

// Hooks returns the hook configs the runner was created with, in config
// order. Hooks whose matcher failed to compile at construction are
// omitted. Intended for diagnostics; callers should not rely on ordering
// or identity beyond that.
func (r *Runner) Hooks() []config.HookConfig {
	out := make([]config.HookConfig, len(r.hooks))
	for i, h := range r.hooks {
		out[i] = h.cfg
	}
	return out
}

// Run executes all matching hooks for the given event and tool, returning
// an aggregated result.
func (r *Runner) Run(ctx context.Context, eventName, sessionID, toolName, toolInputJSON string) (AggregateResult, error) {
	matching := r.matchingHooks(toolName)
	if len(matching) == 0 {
		return AggregateResult{Decision: DecisionNone}, nil
	}

	// Deduplicate by command string.
	seen := make(map[string]bool, len(matching))
	var deduped []config.HookConfig
	for _, h := range matching {
		if seen[h.Command] {
			continue
		}
		seen[h.Command] = true
		deduped = append(deduped, h)
	}

	// Saturation guard: once maxAbandonedWorkers hook goroutines are
	// stuck past their timeout+grace, spawning more only grows the
	// pile of leaked goroutines and processes, so reject the run
	// before any worker is spawned. The check is advisory, not a hard
	// invariant — workers turn abandoned only at timeout+grace, so a
	// burst that passes the check before the first abandonment lands
	// can still overshoot by its in-flight size; what this guarantees
	// is that growth stops once saturation is observed. A rejected run
	// is non-blocking (DecisionNone), deliberately matching how a
	// single wedged hook is treated: turning a hooks-subsystem failure
	// into blocked tool calls would trade a bounded leak for an agent
	// outage.
	if abandoned := abandonedWorkers.Load(); abandoned >= int64(maxAbandonedWorkers) {
		slog.Error(
			"Hook run rejected: too many abandoned hook workers",
			"event", eventName,
			"tool", toolName,
			"abandoned_workers", abandoned,
			"cap", maxAbandonedWorkers,
		)
		return AggregateResult{Decision: DecisionNone}, nil
	}

	envVars := BuildEnv(eventName, toolName, sessionID, r.cwd, r.projectDir, toolInputJSON)
	payload := BuildPayload(eventName, sessionID, r.cwd, toolName, toolInputJSON)

	results := make([]HookResult, len(deduped))
	var wg sync.WaitGroup
	wg.Add(len(deduped))

	for i, h := range deduped {
		go func(idx int, hook config.HookConfig) {
			defer wg.Done()
			results[idx] = r.runOne(ctx, hook, envVars, payload)
		}(i, h)
	}
	wg.Wait()

	agg := aggregate(results, toolInputJSON)
	agg.Hooks = make([]HookInfo, len(deduped))
	for i, h := range deduped {
		agg.Hooks[i] = HookInfo{
			Name:         h.DisplayName(),
			Matcher:      h.Matcher,
			Decision:     results[i].Decision.String(),
			Halt:         results[i].Halt,
			Reason:       results[i].Reason,
			InputRewrite: results[i].UpdatedInput != "",
		}
	}
	slog.Info(
		"Hook completed",
		"event", eventName,
		"tool", toolName,
		"hooks", len(deduped),
		"decision", agg.Decision.String(),
	)
	return agg, nil
}

// matchingHooks returns hooks whose matcher matches the tool name (or has
// no matcher, which matches everything).
func (r *Runner) matchingHooks(toolName string) []config.HookConfig {
	var matched []config.HookConfig
	for _, h := range r.hooks {
		if h.matcher == nil || h.matcher.MatchString(toolName) {
			matched = append(matched, h.cfg)
		}
	}
	return matched
}

// workerTracker coordinates the abandon handshake between runOne's
// outer frame and its worker goroutine, and collects the pids of
// processes the worker spawns so the abandon path can hard-kill
// them. All methods are safe for concurrent use: RegisterProcess
// fires from inside the interpreter, potentially on several
// goroutines at once for backgrounded commands.
type workerTracker struct {
	mu        sync.Mutex
	pids      []int
	finished  bool
	abandoned bool
}

// register is passed as shell.RunOptions.RegisterProcess.
func (wt *workerTracker) register(pid int) {
	wt.mu.Lock()
	wt.pids = append(wt.pids, pid)
	wt.mu.Unlock()
}

// workerFinished runs on the worker goroutine after runShell
// returns. If the outer frame already abandoned this worker, it
// performs the matching decrement — the count must come back down
// even though nobody was waiting on the worker anymore.
func (wt *workerTracker) workerFinished() {
	wt.mu.Lock()
	wt.finished = true
	if wt.abandoned {
		abandonedWorkers.Add(-1)
	}
	wt.mu.Unlock()
}

// abandon runs on the outer frame once it gives up waiting. It marks
// the worker abandoned and increments the global count — unless the
// worker snuck in a finish between the grace deadline and now, in
// which case there is nothing left to track. The mutex pairs with
// workerFinished so exactly one side of the increment/decrement
// pairing happens under either interleaving. Returns a snapshot of
// the registered pids for the hard-kill attempt.
func (wt *workerTracker) abandon() []int {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	wt.abandoned = true
	if !wt.finished {
		abandonedWorkers.Add(1)
	}
	return slices.Clone(wt.pids)
}

// hardKillAbandoned is the abandon path's escalation: mark the worker
// abandoned (incrementing the global count) and, off the caller's
// goroutine, re-attempt a hard kill of the processes the worker
// registered. The ctx-driven kill inside the shell exec layer has
// already fired by now (SIGINT then SIGKILL on Unix, taskkill /T on
// Windows); this is defense in depth for the cases where that kill
// did not land — a failed kill, a nested interpreter, or a process
// still holding a pipe. It must never block the caller, so the kill
// runs in its own goroutine; that goroutine is bounded by the same
// cap because abandonments themselves are capped via the Run-level
// saturation check.
func hardKillAbandoned(wt *workerTracker, hook config.HookConfig) {
	pids := wt.abandon()
	// Read the seam here, synchronously on hardKillAbandoned's own
	// caller goroutine (runOne's abandon-path select, itself on the
	// goroutine Run() spawns per hook and properly joins before
	// returning) -- NOT inside the goroutine below. A test that installs
	// a seam always does so before calling r.Run() and restores it only
	// after r.Run() returns, so a read that happens synchronously within
	// that window is provably ordered by Run()'s own join, with no
	// dependency on when the spawned goroutine below happens to be
	// scheduled. Reading it lazily inside the goroutine put the read on
	// an uncoupled timeline and raced a later test's restore under the
	// Go memory model, confirmed by `go test -race`, even with whole
	// seconds of real time separating them in every practical run.
	seam := getAbandonSeam()
	go func() {
		if seam != nil {
			seam(pids)
			return
		}
		for _, pid := range pids {
			if err := session.KillProcess(pid); err != nil {
				slog.Warn(
					"Hard kill of abandoned hook process failed",
					"pid", pid,
					"command", hook.Command,
					"error", err,
				)
			}
		}
	}()
}

// runOne executes a single hook command and returns its result.
//
// Execution goes through Rush's embedded POSIX shell (shell.Run) so the
// same interpreter, builtins, and coreutils are visible to hooks as to
// the bash tool. BlockFuncs are intentionally omitted: hooks are
// user-authored config that carry the same trust as a shell alias.
//
// A hook that fails to yield after its deadline has passed is abandoned
// after abandonGrace so the caller never blocks longer than
// timeout + abandonGrace. Ownership of the stdout and stderr buffers is
// strictly single-goroutine:
//   - before receiving from `done`, only the goroutine writes to them;
//   - after `done` delivers a value, the goroutine is finished and the
//     outer frame reads them;
//   - on the abandon path, the goroutine may still be writing and the
//     outer frame must not touch them again.
//   - the abandonment is counted in the global abandonedWorkers gauge and
//     undone when the goroutine eventually finishes;
//   - a best-effort hard kill of the worker's registered pids is attempted
//     asynchronously and must never touch the buffers or block the caller.
func (r *Runner) runOne(parentCtx context.Context, hook config.HookConfig, envVars []string, payload []byte) HookResult {
	timeout := hook.TimeoutDuration()
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	tracker := &workerTracker{}
	done := make(chan error, 1)
	// Read runShell here, synchronously on runOne's own caller goroutine
	// (the one Run() spawns per hook and properly joins before
	// returning) -- NOT inside the goroutine below. See
	// hardKillAbandoned's identical comment on getAbandonSeam() for the
	// full reasoning: a read on an uncoupled timeline (inside the spawned
	// goroutine, whose scheduling has no relation to when a test's
	// cleanup runs) is a genuine data race under the Go memory model,
	// confirmed by `go test -race`, regardless of how much real
	// wall-clock time separates the two accesses in practice.
	shellFn := getRunShell()
	go func() {
		err := shellFn(ctx, shell.RunOptions{
			Command:         hook.Command,
			Cwd:             r.cwd,
			Env:             envVars,
			Stdin:           bytes.NewReader(payload),
			Stdout:          &stdout,
			Stderr:          &stderr,
			RegisterProcess: tracker.register,
		})
		tracker.workerFinished()
		done <- err
	}()

	var err error
	select {
	case err = <-done:
		// Normal path: goroutine has finished, buffers are safe to read.
	case <-ctx.Done():
		select {
		case err = <-done:
			// Interpreter yielded within the grace period; safe to read.
		case <-time.After(abandonGrace):
			slog.Warn(
				"Hook did not yield after cancel; abandoning goroutine",
				"command", hook.Command,
				"timeout", timeout,
			)
			hardKillAbandoned(tracker, hook)
			// The goroutine may still be writing to stdout/stderr; do
			// not read either buffer below this point.
			return HookResult{Decision: DecisionNone}
		}
	}

	if shell.IsInterrupt(err) {
		// Distinguish timeout from parent cancellation.
		if parentCtx.Err() != nil {
			slog.Debug("Hook cancelled by parent context", "command", hook.Command)
		} else {
			slog.Warn("Hook timed out", "command", hook.Command, "timeout", timeout)
		}
		return HookResult{Decision: DecisionNone}
	}

	if err != nil {
		exitCode := shell.ExitCode(err)
		switch exitCode {
		case 2:
			// Exit code 2 = block this tool call. Stderr is the reason.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "blocked by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Reason:   reason,
			}
		case HaltExitCode:
			// Exit code 49 = halt the whole turn. Stderr is the reason.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "turn halted by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Halt:     true,
				Reason:   reason,
			}
		default:
			// Other non-zero exits are non-blocking errors.
			slog.Warn(
				"Hook failed with non-blocking error",
				"command", hook.Command,
				"exit_code", exitCode,
				"stderr", strings.TrimSpace(stderr.String()),
				"error", err,
			)
			return HookResult{Decision: DecisionNone}
		}
	}

	// Exit code 0 — parse stdout JSON.
	result := parseStdout(stdout.String())
	slog.Debug(
		"Hook executed",
		"command", hook.Command,
		"decision", result.Decision.String(),
	)
	return result
}
