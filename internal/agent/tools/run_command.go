package tools

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/agentguard"
	"github.com/PHPCraftdream/rush/internal/agent/safety/lockwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"mvdan.cc/sh/v3/syntax"
)

// RunCommandToolName is the name of the no-shell program runner tool.
const RunCommandToolName = "run_command"

const (
	// runCommandDefaultTimeoutSeconds is long enough for a normal
	// test/build invocation to finish without the model having to think
	// about timeouts at all.
	runCommandDefaultTimeoutSeconds = 120

	// runCommandMaxTimeoutSeconds is the hard ceiling so a single
	// run_command call can never hang the turn indefinitely.
	runCommandMaxTimeoutSeconds = 600
)

type RunCommandParams struct {
	Program        string   `json:"program" description:"The executable to run, resolved via the normal PATH lookup (e.g. \"go\", \"git\", \"C:/tools/tool.exe\"). Shell builtins (echo, cd, test, set) are NOT available — there is no shell"`
	Args           []string `json:"args,omitempty" description:"Arguments passed to the program EXACTLY as given. There is NO shell: no pipes, no ; or &&, no $( ), no backticks, no $VAR expansion, no glob expansion. If you need an env var's value or a glob expansion, resolve it yourself (read the value, list the files) and pass literal strings"`
	WorkingDir     string   `json:"working_dir,omitempty" description:"Working directory for the program, relative to the current working directory. Defaults to the current working directory. Must stay inside the current working directory"`
	Description    string   `json:"description,omitempty" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Maximum seconds to wait for the program to finish before killing it (default 120, maximum 600)"`
	// Timeout is an optional explicit ledger-level deadline (#1037), a
	// SEPARATE axis from TimeoutSeconds above's own process-kill clamp --
	// see parseTimeoutParam's doc for the exact relationship (TimeoutSeconds
	// alone is treated as an alias for Timeout{Kind: "terminate_and_wake"};
	// setting both is a validation error).
	Timeout *TimeoutParams `json:"timeout,omitempty" description:"Optional explicit deadline: {\"seconds\": N, \"kind\": \"wake_only\"|\"terminate_and_wake\"}. \"wake_only\" lets the work keep running and just notifies you it's taking a while; \"terminate_and_wake\" stops it and reports partial output. No default -- set it only when you actually want a deadline. Mutually exclusive with the legacy timeout_seconds above. A bare number of seconds is also accepted and means {\"seconds\": N, \"kind\": \"wake_only\"}."`
}

type RunCommandPermissionsParams struct {
	Program        string         `json:"program"`
	Args           []string       `json:"args"`
	WorkingDir     string         `json:"working_dir"`
	Description    string         `json:"description"`
	TimeoutSeconds int            `json:"timeout_seconds"`
	Timeout        *TimeoutParams `json:"timeout"`
}

// RunAllowlistCommand synthesizes a single shell-like command string from
// program and args so allow_bash patterns (permissions.run) written for the bash tool
// match run_command calls the same way (e.g. "go test" matches
// program "go", args ["test", "./..."]). Each argument that is not
// already shell-safe is quoted with mvdan.cc/sh/v3/syntax.Quote
// (LangBash) so metacharacters inside an argument can never make the
// synthesized string parse as a compound command.
func (p RunCommandPermissionsParams) RunAllowlistCommand() string {
	if p.Program == "" {
		return ""
	}
	parts := []string{p.Program}
	for _, arg := range p.Args {
		quoted, err := syntax.Quote(arg, syntax.LangBash)
		if err != nil {
			quoted = strconv.Quote(arg)
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " ")
}

type RunCommandResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
}

//go:embed run_command.md.tpl
var runCommandDescriptionTmpl []byte

var runCommandDescriptionTpl = template.Must(
	template.New("runCommandDescription").
		Parse(string(runCommandDescriptionTmpl)),
)

type runCommandDescriptionData struct {
	BannedCommands  string
	MaxOutputLength int
	DefaultTimeout  int
	MaxTimeout      int
}

func runCommandDescription() string {
	return renderTemplate(runCommandDescriptionTpl, runCommandDescriptionData{
		BannedCommands:  strings.Join(shell.BannedCommands(), ", "),
		MaxOutputLength: MaxOutputLength,
		DefaultTimeout:  runCommandDefaultTimeoutSeconds,
		MaxTimeout:      runCommandMaxTimeoutSeconds,
	})
}

// NewRunCommandTool builds the no-shell program runner scoped to workingDir.
func NewRunCommandTool(permissions permission.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RunCommandToolName,
		runCommandDescription(),
		func(ctx context.Context, params RunCommandParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Program == "" {
				return fantasy.NewTextErrorResponse("program is required"), nil
			}

			argv := append([]string{params.Program}, params.Args...)
			if guardErr := agentguard.CheckCommandPolicyArgs(argv); guardErr != nil {
				return fantasy.NewTextErrorResponse(guardErr.Error()), nil
			}

			// Same shared block list as the bash tool (banned programs AND
			// argument blockers like `go install` / `npm install --global` /
			// `go test -exec`) — deliberately NOT a second list, so the two
			// surfaces cannot drift apart.
			for _, blocker := range shell.CanonicalBlockFuncs() {
				if blocker(argv) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q with these arguments is not allowed (run_command reuses the bash tool's block list)",
						params.Program)), nil
				}
			}

			// Structured argv must be checked without synthesizing a shell
			// string, which can lose argument boundaries and wrapper options.
			if guardErr := agentguard.CheckArgs(argv); guardErr != nil {
				return fantasy.NewTextErrorResponse(guardErr.Error()), nil
			}

			execDir := workingDir
			if params.WorkingDir != "" {
				if msg := pathInsideWorkingDir(workingDir, params.WorkingDir); msg != "" {
					return fantasy.NewTextErrorResponse(msg), nil
				}
				// Join (validated above) so execDir is absolute — cmd.Dir
				// is resolved against THIS process's cwd, not workingDir.
				execDir = filepath.Join(workingDir, params.WorkingDir)
			}

			// A17: same orchestrator-commits rule as the bash tool — git
			// writes are refused for runs rooted in a linked worktree, keyed
			// on the resolved exec directory.
			if agentguard.IsLinkedWorktree(execDir) {
				if gitErr := agentguard.CheckGitWritesArgs(argv); gitErr != nil {
					return fantasy.NewTextErrorResponse(gitErr.Error()), nil
				}
			}

			n := params.TimeoutSeconds
			clampNotice := ""
			if n == 0 {
				n = runCommandDefaultTimeoutSeconds
			}
			if n < 0 {
				return fantasy.NewTextErrorResponse("timeout_seconds must be positive"), nil
			}
			if n > runCommandMaxTimeoutSeconds {
				clampNotice = fmt.Sprintf("(timeout_seconds %d clamped to %d)\n", n, runCommandMaxTimeoutSeconds)
				n = runCommandMaxTimeoutSeconds
			}
			runCtx := ctx
			cancel := func() {}
			if argvLeafFromContext(ctx) == nil {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(n)*time.Second)
			}
			defer cancel()

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing commands")
			}

			// Every run_command call executes a program — there is no
			// safe-read-only fast path, so a permission request is ALWAYS
			// raised.
			p, err := permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        execDir,
				ToolCallID:  call.ID,
				ToolName:    RunCommandToolName,
				Action:      "execute",
				Description: cmp.Or(params.Description, fmt.Sprintf("Run %s", params.Program)),
				Params:      RunCommandPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			if spawn := commandSpawnFromContext(ctx); spawn != nil {
				return spawn(ctx, RunCommandToolName, argv)
			}
			if testCommandSpawn != nil {
				return testCommandSpawn(ctx, RunCommandToolName, argv)
			}
			startTime := time.Now()
			// R7b lifetime seam: only after the same shared guards and permission.
			leaf := argvLeafFromContext(ctx)
			commandCtx := runCtx
			if leaf != nil {
				commandCtx = context.WithoutCancel(ctx)
			}
			if leaf == nil {
				commandString := RunCommandPermissionsParams(params).RunAllowlistCommand()
				if wrap, lockName := lockwrap.Decision(lockwrap.CurrentLockWrapPatterns(), commandString, false, false); wrap {
					fmt.Fprintf(os.Stderr, "wrapped by machine lock %s\n", lockName)
					startTime := time.Now()
					stdout, stderr, code, lockErr := lockwrap.RunLockedCommand(runCtx, lockName, commandString, execDir, argv)
					if lockErr != nil {
						if errors.Is(lockErr, context.DeadlineExceeded) || runCtx.Err() == context.DeadlineExceeded {
							text := stdout + stderr
							if text == "" {
								text = lockErr.Error()
							}
							return fantasy.NewTextErrorResponse(fmt.Sprintf("program %q timed out after %ds and was killed\n%s", params.Program, n, text)), nil
						}
						return fantasy.NewTextErrorResponse(fmt.Sprintf("machine lock %s failed: %v", lockName, lockErr)), nil
					}
					text := truncateOutput(stdout + stderr)
					if code != 0 {
						text += fmt.Sprintf("\nExit code %d", code)
					}
					if text == "" {
						text = BashNoOutput
					}
					metadata := RunCommandResponseMetadata{StartTime: startTime.UnixMilli(), EndTime: time.Now().UnixMilli(), Output: text, Description: params.Description, WorkingDirectory: execDir}
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), metadata), nil
				}
			}
			cmd := platform.Command(commandCtx, params.Program, params.Args...)
			cmd.Dir = execDir
			buf := newRunCommandOutputBuffer(0)
			cmd.Stdout = buf
			cmd.Stderr = buf
			// Tree-kill on ctx cancellation (batch_stop, terminate_and_wake
			// timeout -- task #1023 §3), not just the direct process.
			configureRunCommandProcess(cmd)
			execErr := cmd.Start()
			if execErr == nil {
				if leaf == nil {
					// Registers buf via the context sink WHILE the process
					// is still running, so the batch output cache can read
					// progressive output and a stop's result can quote it
					// -- see LiveOutputBuffer's doc.
					if sink := LiveOutputSinkFromContext(ctx); sink != nil {
						sink(buf)
					}
					execErr = cmd.Wait()
				} else {
					execErr = runArgvLeaf(ctx, leaf, cmd, buf)
				}
			}
			endTime := time.Now()

			metadata := RunCommandResponseMetadata{
				StartTime:        startTime.UnixMilli(),
				EndTime:          endTime.UnixMilli(),
				Output:           truncateOutput(buf.String()),
				Description:      params.Description,
				WorkingDirectory: execDir,
			}

			// Error contract: model-correctable failures come back as
			// error RESPONSES (nil Go error); only caller cancellation is
			// fatal.
			if execErr != nil {
				switch {
				case ctx.Err() == context.Canceled:
					return fantasy.ToolResponse{}, ctx.Err()
				case runCtx.Err() == context.DeadlineExceeded || errors.Is(execErr, context.DeadlineExceeded):
					text := truncateOutput(buf.String())
					if text == "" {
						text = execErr.Error()
					}
					metadata.Output = text
					return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q timed out after %ds and was killed\n%s",
						params.Program, n, text)), nil), nil
				case errors.Is(execErr, exec.ErrNotFound) || errors.As(execErr, new(*exec.Error)):
					underlying := execErr.Error()
					var execError *exec.Error
					if errors.As(execErr, &execError) {
						underlying = execError.Err.Error()
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q not found in PATH: %s", params.Program, underlying)), nil
				default:
					// Non-zero exit (or other spawn-time failure) — the
					// model must see it and can correct. NOT a Go error.
					text := truncateOutput(buf.String())
					if code := shell.ExitCode(execErr); code != 0 {
						text += fmt.Sprintf("\nExit code %d", code)
					}
					if clampNotice != "" {
						text = clampNotice + text
					}
					metadata.Output = text
					return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(text), metadata), nil
				}
			}

			text := truncateOutput(buf.String())
			if text == "" {
				text = BashNoOutput
			}
			if clampNotice != "" {
				text = clampNotice + text
			}
			metadata.Output = text
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), metadata), nil
		},
	)
}

// ArgvLeaf opts a guarded run_command call into retained process ownership.
// OnStart runs after Start succeeds and before Wait begins. It must return
// promptly; it may publish the handle, but must not wait for Done itself.
type (
	ArgvLeaf           struct{ OnStart func(*ArgvProcess) }
	argvLeafContextKey struct{}
)

// WithArgvLeaf selects the lifetime seam without bypassing tool policy.
func WithArgvLeaf(ctx context.Context, leaf *ArgvLeaf) context.Context {
	return context.WithValue(ctx, argvLeafContextKey{}, leaf)
}

func argvLeafFromContext(ctx context.Context) *ArgvLeaf {
	leaf, _ := ctx.Value(argvLeafContextKey{}).(*ArgvLeaf)
	return leaf
}

var (
	ErrArgvNotDone  = errors.New("argv process has not completed")
	ErrArgvDisposed = errors.New("argv process disposed")
	ErrArgvBusy     = errors.New("argv process has an active physical stop")
)

// ArgvOutcome is sourced exclusively from the real, drained exec.Cmd.Wait.
// ExitCode is -1 when the platform supplies no numeric exit status.
type ArgvOutcome struct {
	WaitErr  error
	ExitCode int
}

// ArgvProcess retains completion and output independently of tool return.
type ArgvProcess struct {
	mu       sync.Mutex
	buf      *runCommandOutputBuffer
	done     chan struct{}
	stopSlot chan struct{}
	kill     func() error
	outcome  ArgvOutcome
	disposed bool
}

func newArgvProcess(cmd *exec.Cmd, buf *runCommandOutputBuffer) *ArgvProcess {
	p := &ArgvProcess{buf: buf, done: make(chan struct{}), stopSlot: make(chan struct{}, 1), kill: cmd.Cancel}
	p.stopSlot <- struct{}{}
	buf.mu.Lock()
	buf.changed = make(chan struct{})
	buf.mu.Unlock()
	return p
}

func runArgvLeaf(ctx context.Context, leaf *ArgvLeaf, cmd *exec.Cmd, buf *runCommandOutputBuffer) (err error) {
	p := newArgvProcess(cmd, buf)
	// A callback panic cannot strand the successfully started process.
	defer func() {
		if v := recover(); v != nil {
			_ = p.Stop(context.Background())
			p.finish(cmd.Wait(), cmd)
			panic(v)
		}
	}()
	if leaf.OnStart != nil {
		leaf.OnStart(p)
	}
	go func() { p.finish(cmd.Wait(), cmd) }()
	select {
	case <-p.done:
	case <-ctx.Done():
		go func() { _ = p.Stop(context.Background()) }()
		<-p.done
	}
	outcome, _ := p.Outcome()
	return outcome.WaitErr
}

func (p *ArgvProcess) finish(err error, cmd *exec.Cmd) {
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	p.mu.Lock()
	p.outcome = ArgvOutcome{WaitErr: err, ExitCode: code}
	close(p.done)
	p.mu.Unlock()
}

// Done closes only after real Wait, including stdout/stderr drain.
func (p *ArgvProcess) Done() <-chan struct{} { return p.done }

// Outcome refuses to infer an exit result before completion.
func (p *ArgvProcess) Outcome() (ArgvOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return p.outcome, nil
	default:
		return ArgvOutcome{}, ErrArgvNotDone
	}
}

// Stop serializes physical tree kills. A caller deadline bounds its wait,
// never releases the slot of a kill still physically executing.
func (p *ArgvProcess) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopSlot:
	}
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		p.stopSlot <- struct{}{}
		return ErrArgvDisposed
	}
	select {
	case <-p.done:
		p.mu.Unlock()
		p.stopSlot <- struct{}{}
		return nil
	default:
	}
	p.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		defer func() { p.stopSlot <- struct{}{} }()
		result <- p.kill()
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReadOutput blocks at the live head and returns caller-owned retained bytes.
// Invalid signed cursors normalize to zero, as on the existing live buffer.
func (p *ArgvProcess) ReadOutput(ctx context.Context, cursor int64) (jobbatch.OutputChunk, error) {
	for {
		p.mu.Lock()
		if p.disposed {
			p.mu.Unlock()
			return jobbatch.OutputChunk{}, ErrArgvDisposed
		}
		b := p.buf
		b.mu.Lock()
		if cursor < 0 || cursor > b.total {
			cursor = 0
		}
		oldest := b.availableFromLocked()
		start := max(cursor, oldest)
		chunk := jobbatch.OutputChunk{Data: append([]byte(nil), b.data[start-oldest:]...), Next: uint64(b.total), Oldest: uint64(oldest), Truncated: cursor < oldest}
		changed := b.changed
		select {
		case <-p.done:
			chunk.EOF = true
		default:
		}
		b.mu.Unlock()
		p.mu.Unlock()
		if len(chunk.Data) > 0 || chunk.EOF {
			return chunk, nil
		}
		select {
		case <-ctx.Done():
			return jobbatch.OutputChunk{}, ctx.Err()
		case <-changed:
		case <-p.done:
		}
	}
}

// Dispose gives up output once, only after completion and physical Stop return.
func (p *ArgvProcess) Dispose(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return ErrArgvDisposed
	}
	select {
	case <-p.done:
	default:
		p.mu.Unlock()
		return ErrArgvNotDone
	}
	p.mu.Unlock()
	// Stop takes mu after acquiring the slot; never wait with mu held.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopSlot:
	}
	defer func() { p.stopSlot <- struct{}{} }()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.disposed {
		return ErrArgvDisposed
	}
	p.disposed = true
	p.buf.mu.Lock()
	p.buf.data = nil
	p.buf.mu.Unlock()
	return nil
}

// LiveOutputBuffer is a concurrency-safe, size-bounded output sink a
// still-running tool execution registers with the work ledger (currently
// only run_command, task #1023 §3) so job_output/job_kill and the ledger's
// own timeout capturePartial can read accumulated output WHILE the process
// is still running. run_command has no BackgroundShellManager entry to read
// from otherwise (see the run_command branch of ResolveJobShellID's callers
// in job_shell_resolver.go).
type LiveOutputBuffer interface {
	// Read returns the bytes written from cursor onward, plus the new
	// cursor (== total bytes ever written). A cursor outside [0, total] is
	// treated as 0 (wake-tools-contract.md §2.1: a stale/negative cursor is
	// not an error).
	Read(cursor int64) (data string, nextCursor int64)
	// String returns everything currently resident, for a one-shot
	// best-effort read (job_kill's stopped notice, timeout's capturePartial).
	String() string
}

// runCommandLiveBufferMax bounds how much of a still-running run_command
// job's output stays resident for progressive job_output reads. Distinct
// from MaxOutputLength (the FINAL response's display truncation): this
// bounds memory for a long-running, chatty command, not display width.
const runCommandLiveBufferMax = 1 << 20 // 1 MiB

// runCommandOutputBuffer implements LiveOutputBuffer and io.Writer so it can
// be used directly as cmd.Stdout/cmd.Stderr. Oldest bytes are dropped once
// resident content would exceed maxBytes -- simpler than internal/shell's
// boundedBuffer (head+tail retention): a run_command job's FINAL output is
// separately truncated for display already (truncateOutput), so this only
// needs to serve "recent progress" reads while running, not preserve the
// command's early output verbatim.
type runCommandOutputBuffer struct {
	mu       sync.Mutex
	data     []byte
	maxBytes int
	total    int64         // monotonic count of all bytes ever written
	changed  chan struct{} // Optional leaf-only write notification.
	// lastWriteNS is the nanosecond Unix timestamp of the last non-empty
	// Write (0 = never written).
	lastWriteNS atomic.Int64
}

func newRunCommandOutputBuffer(maxBytes int) *runCommandOutputBuffer {
	if maxBytes <= 0 {
		maxBytes = runCommandLiveBufferMax
	}
	return &runCommandOutputBuffer{maxBytes: maxBytes}
}

// Write implements io.Writer. Always reports len(p) bytes written (matching
// io.Writer semantics) even once bytes start being dropped from the front.
func (b *runCommandOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	// Record the last-output time only for real output; empty writes don't count.
	if len(p) > 0 {
		b.lastWriteNS.Store(time.Now().UnixNano())
		if b.changed != nil {
			close(b.changed)
			b.changed = make(chan struct{})
		}
	}
	b.data = append(b.data, p...)
	if excess := len(b.data) - b.maxBytes; excess > 0 {
		b.data = b.data[excess:]
	}
	return len(p), nil
}

// LastWriteAt reports when the buffer last received non-empty output.
func (b *runCommandOutputBuffer) LastWriteAt() (time.Time, bool) {
	ns := b.lastWriteNS.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// availableFromLocked is the earliest logical offset still resident (bytes
// before it were dropped by Write's cap). Caller must hold b.mu.
func (b *runCommandOutputBuffer) availableFromLocked() int64 {
	return b.total - int64(len(b.data))
}

func (b *runCommandOutputBuffer) Read(cursor int64) (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < 0 || cursor > b.total {
		cursor = 0
	}
	start := cursor - b.availableFromLocked()
	if start < 0 {
		start = 0 // cursor points at already-dropped data; return what's left rather than erroring
	}
	if start > int64(len(b.data)) {
		start = int64(len(b.data))
	}
	return string(b.data[start:]), b.total
}

func (b *runCommandOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// liveOutputSinkContextKey is the context key WithLiveOutputSink installs.
type liveOutputSinkContextKey struct{}

// WithLiveOutputSink attaches a callback that a tool execution invokes with
// its LiveOutputBuffer as soon as the underlying process starts, so
// job_kill/job_output can read/stop it while still running (task #1023 §3).
// Wired by internal/agent's asyncTool.run for run_command only;
// LiveOutputSinkFromContext returns nil for every other tool/origin, and
// callers must treat a nil sink as "do not register".
func WithLiveOutputSink(ctx context.Context, sink func(LiveOutputBuffer)) context.Context {
	return context.WithValue(ctx, liveOutputSinkContextKey{}, sink)
}

// LiveOutputSinkFromContext returns the sink WithLiveOutputSink attached, or
// nil.
func LiveOutputSinkFromContext(ctx context.Context) func(LiveOutputBuffer) {
	sink, _ := ctx.Value(liveOutputSinkContextKey{}).(func(LiveOutputBuffer))
	return sink
}
