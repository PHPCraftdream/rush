// Package shell provides cross-platform shell execution capabilities.
//
// This package provides Shell instances for executing commands with their own
// working directory and environment. Each shell execution is independent.
//
// WINDOWS COMPATIBILITY:
// This implementation provides POSIX shell emulation (mvdan.cc/sh/v3) even on
// Windows. Commands should use forward slashes (/) as path separators to work
// correctly on all platforms.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shpolicy"
)

// ShellType represents the type of shell to use
type ShellType int

const (
	ShellTypePOSIX ShellType = iota
	ShellTypeCmd
	ShellTypePowerShell
)

// RushEnvMarkers returns a fresh slice of the environment variables that
// Rush unconditionally sets on every shell it spawns — both the interactive
// bash tool's [Shell] and the hook runner's [Run] calls. Tools that want to
// detect "am I being invoked by an AI agent?" can check any of these.
// Keeping them in one place guarantees the two shell surfaces cannot drift.
// A fresh slice is returned on every call so callers may append freely.
func RushEnvMarkers() []string {
	return []string{
		"RUSH=1",
		"AGENT=rush",
		"AI_AGENT=rush",
	}
}

// hasEnvVar reports whether env already carries an entry for name (with
// or without a value).
func hasEnvVar(env []string, name string) bool {
	prefix := name + "="
	for _, entry := range env {
		if entry == name || strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// Logger interface for optional logging
type Logger interface {
	InfoPersist(msg string, keysAndValues ...any)
}

// noopLogger is a logger that does nothing
type noopLogger struct{}

func (noopLogger) InfoPersist(msg string, keysAndValues ...any) {}

// BlockFunc is a function that determines if a command should be blocked.
// The type lives in shpolicy; this alias keeps every importer unchanged.
type BlockFunc = shpolicy.BlockFunc

// The command-policy surface moved to shpolicy; these aliases keep the
// external consumers (agentguard, agent/tools, cliprovider) unchanged.
var (
	BannedCommands                 = shpolicy.BannedCommands
	CanonicalBlockFuncs            = shpolicy.CanonicalBlockFuncs
	CommandsBlocker                = shpolicy.CommandsBlocker
	CommandBlocked                 = shpolicy.CommandBlocked
	CommandCandidatesForPlatform   = shpolicy.CommandCandidatesForPlatform
	SkipLauncherPrefixes           = shpolicy.SkipLauncherPrefixes
	DecodePowerShellEncodedCommand = shpolicy.DecodePowerShellEncodedCommand
	IsNoOpCommand                  = shpolicy.IsNoOpCommand
)

// Shell provides cross-platform shell execution with optional state persistence
type Shell struct {
	env        []string
	cwd        string
	mu         sync.Mutex
	logger     Logger
	blockFuncs []BlockFunc
}

// Options for creating a new shell
type Options struct {
	WorkingDir string
	Env        []string
	Logger     Logger
	BlockFuncs []BlockFunc
}

// NewShell creates a new shell instance with the given options
func NewShell(opts *Options) *Shell {
	if opts == nil {
		opts = &Options{}
	}

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	env := opts.Env
	if env == nil {
		env = os.Environ()
	}

	// Allow tools to detect execution by Rush.
	env = append(env, RushEnvMarkers()...)

	// Fork patch (A8): a timed-out/job-killed `git status` leaves
	// .git/worktrees/<n>/index.lock behind, and every later git write in
	// that worktree dies with "Unable to create index.lock". Disabling
	// git's opportunistic index refresh makes read-only commands stop
	// taking index.lock at all. An explicit user value wins.
	if !hasEnvVar(env, "GIT_OPTIONAL_LOCKS") {
		env = append(env, "GIT_OPTIONAL_LOCKS=0")
	}

	logger := opts.Logger
	if logger == nil {
		logger = noopLogger{}
	}

	return &Shell{
		cwd:        cwd,
		env:        env,
		logger:     logger,
		blockFuncs: opts.BlockFuncs,
	}
}

// Exec executes a command in the shell
func (s *Shell) Exec(ctx context.Context, command string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.exec(ctx, command)
}

// ExecStream executes a command in the shell with streaming output to provided writers
func (s *Shell) ExecStream(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.execStream(ctx, command, stdout, stderr)
}

// GetWorkingDir returns the current working directory
func (s *Shell) GetWorkingDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// SetWorkingDir sets the working directory
func (s *Shell) SetWorkingDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify the directory exists
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("directory does not exist: %w", err)
	}

	s.cwd = dir
	return nil
}

// GetEnv returns a copy of the environment variables
func (s *Shell) GetEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	env := make([]string, len(s.env))
	copy(env, s.env)
	return env
}

// SetEnv sets an environment variable
func (s *Shell) SetEnv(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update or add the environment variable
	keyPrefix := key + "="
	for i, env := range s.env {
		if strings.HasPrefix(env, keyPrefix) {
			s.env[i] = keyPrefix + value
			return
		}
	}
	s.env = append(s.env, keyPrefix+value)
}

// SetBlockFuncs sets the command block functions for the shell
func (s *Shell) SetBlockFuncs(blockFuncs []BlockFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockFuncs = blockFuncs
}

// newInterp creates a new interpreter with the current shell state. A nil
// stdin is equivalent to an empty input stream.
func (s *Shell) newInterp(stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, error) {
	// Stateful Shell sessions have no abandon-time hard-kill consumer,
	// so there is no pid registration here.
	return newRunner(s.cwd, s.env, stdin, stdout, stderr, s.blockFuncs, nil)
}

// updateShellFromRunner updates the shell from the interpreter after execution.
func (s *Shell) updateShellFromRunner(runner *interp.Runner) {
	s.cwd = runner.Dir
	s.env = s.env[:0]
	for name, vr := range runner.Vars {
		if vr.Exported {
			s.env = append(s.env, name+"="+vr.Str)
		}
	}
}

// execCommon is the shared implementation for executing commands
func (s *Shell) execCommon(ctx context.Context, command string, stdout, stderr io.Writer) (err error) {
	var runner *interp.Runner
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("command execution panic: %v", r)
		}
		if runner != nil {
			s.updateShellFromRunner(runner)
		}
		s.logger.InfoPersist("command finished", "command", command, "err", err)
	}()

	line, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}

	runner, err = s.newInterp(nil, stdout, stderr)
	if err != nil {
		return fmt.Errorf("could not run command: %w", err)
	}

	err = runner.Run(ctx, line)
	return err
}

// exec executes commands using a cross-platform shell interpreter.
func (s *Shell) exec(ctx context.Context, command string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := s.execCommon(ctx, command, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// execStream executes commands using POSIX shell emulation with streaming output
func (s *Shell) execStream(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return s.execCommon(ctx, command, stdout, stderr)
}

// IsInterrupt checks if an error is due to interruption
func IsInterrupt(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// ExitCode extracts the exit code from an error
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(exitErr)
	}
	return 1
}

// Example usage of the shell package:
//
// 1. For one-off commands:
//
//	shell := shell.NewShell(nil)
//	stdout, stderr, err := shell.Exec(context.Background(), "echo hello")
//
// 2. For maintaining state across commands:
//
//	shell := shell.NewShell(&shell.Options{
//	    WorkingDir: "/tmp",
//	    Logger: myLogger,
//	})
//	shell.Exec(ctx, "export FOO=bar")
//	shell.Exec(ctx, "echo $FOO")  // Will print "bar"
//
// 3. Managing environment and working directory:
//
//	shell := shell.NewShell(nil)
//	shell.SetEnv("MY_VAR", "value")
//	shell.SetWorkingDir("/tmp")
//	cwd := shell.GetWorkingDir()
//	env := shell.GetEnv()

// ParseSimpleCommand returns the literal argv of one simple command. It
// rejects variable assignments, redirections, expansions, and control syntax.
func ParseSimpleCommand(cmd string) ([]string, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	if stmt.Background || stmt.Negated || stmt.Coprocess || stmt.Disown || stmt.Semicolon.IsValid() || len(stmt.Redirs) != 0 {
		return nil, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) == 0 {
		return nil, false
	}

	args := make([]string, len(call.Args))
	for i, word := range call.Args {
		if len(word.Parts) != 1 {
			return nil, false
		}
		lit, ok := word.Parts[0].(*syntax.Lit)
		if !ok {
			return nil, false
		}
		if strings.ContainsAny(lit.Value, "*?") || strings.HasPrefix(lit.Value, "~") {
			return nil, false
		}
		args[i] = lit.Value
	}
	return args, true
}

// IsCompoundCommand reports whether cmd is anything other than a single
// simple command — i.e. it chains (`;`, a raw newline, `&&`, `||`, `|`),
// backgrounds (`&`), substitutes (`$(...)`, backticks), or opens a
// subshell / block / control structure.
//
// It parses cmd with the same shell grammar this package executes
// (mvdan.cc/sh/v3) rather than scanning for a fixed set of metacharacter
// substrings, so it can't be fooled by a metacharacter the substring
// scan happened to miss (a raw newline, a bare backgrounding `&`) and it
// does NOT trip on a metacharacter that only appears inside quotes
// (`echo 'a; b'` is a single command) or in a plain redirection
// (`ls 2>&1` operates on one command). A command that fails to parse is
// reported as compound: callers gating a security-sensitive fast-path
// (skip a permission prompt, or approve a command by a prefix pattern)
// must err toward "not simple" for anything they can't prove is a single
// command.
func IsCompoundCommand(cmd string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return true
	}
	if len(file.Stmts) != 1 {
		return true
	}
	stmt := file.Stmts[0]
	if stmt.Background {
		return true
	}
	// Anything that isn't a single simple command (a pipeline / `&&` /
	// `||` BinaryCmd, a subshell, a block, an if/for/while, …) is compound.
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return true
	}
	// Reject command / process substitution anywhere in the words or
	// redirections of the single command.
	compound := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			compound = true
			return false
		}
		return true
	})
	return compound
}

const (
	// maxInnerStdoutBytes bounds each command substitution's stdout. 64 KiB is
	// enough for a config value while keeping accidental producers harmless.
	maxInnerStdoutBytes = 64 << 10
	// maxInnerStderrBytes bounds retained diagnostics from a command
	// substitution, including diagnostics from commands that fail.
	maxInnerStderrBytes = 512
)

// ErrCommandSubstitutionOutputLimit identifies output that exceeded a command
// substitution stream's byte budget.
var ErrCommandSubstitutionOutputLimit = errors.New("command substitution output limit exceeded")

// CommandSubstitutionOutputLimitError reports which command substitution
// stream exceeded its byte budget.
type CommandSubstitutionOutputLimitError struct {
	Stream string
	Limit  int
}

func (e *CommandSubstitutionOutputLimitError) Error() string {
	return fmt.Sprintf("command substitution %s exceeded %d-byte output limit", e.Stream, e.Limit)
}

func (e *CommandSubstitutionOutputLimitError) Unwrap() error {
	return ErrCommandSubstitutionOutputLimit
}

// NoUnset controls whether ExpandValue treats unset variables as an
// error. Default false matches bash: $UNSET expands to "". Store true
// to re-enable strict mode globally. Not exposed in rush.json; this is
// an internal escape hatch in case the lenient default turns out to be
// the wrong call.
//
// Declared atomic because ExpandValue is invoked concurrently (multiple
// MCP / LSP / provider loads in flight at startup, hook execution, etc.)
// and an unsynchronised read/write pair is a data race under the Go
// memory model regardless of test-level happens-before reasoning. The
// atomic load on the hot path is negligible against the cost of parsing
// and running through mvdan.
var NoUnset atomic.Bool

// ExpandValue expands shell-style substitutions in a single config value.
//
// Supported constructs match the bash tool:
//
//   - $VAR and ${VAR}.
//   - ${VAR:-default} / ${VAR:+alt} / ${VAR:?msg}.
//   - $(command) with full quoting and nesting.
//   - escaped and quoted strings ("...", '...').
//
// Contract:
//
//   - Returns exactly one string. No field splitting, no globbing, no
//     pathname generation. Multi-word command output is preserved
//     verbatim; it is never split into multiple values.
//   - Nounset is off by default, matching bash: unset variables expand
//     to "". Opt in to strict behaviour per-reference with
//     ${VAR:?msg}, which errors loudly when VAR is unset regardless of
//     the global toggle. Flip the global default via
//     shell.NoUnset.Store(true) as an internal escape hatch.
//   - Embedded whitespace and newlines in the input are preserved
//     verbatim. Command substitution strips trailing newlines only
//     (POSIX), never leading or internal whitespace.
//   - Errors wrap the failing inner command's exit code and a bounded
//     prefix of its stderr. Callers that surface the error to users
//     should additionally scrub it for the original template text.
func ExpandValue(ctx context.Context, value string, env []string) (string, error) {
	// Parse the value as a here-doc style word: no word splitting, no
	// globbing, but full support for $VAR, ${VAR...}, $(...), and
	// quoted/escaped strings.
	word, err := syntax.NewParser().Document(strings.NewReader(value))
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}

	// Build a minimal Shell value purely to reuse its handler chain
	// (builtins, block funcs, optional Go coreutils) inside $(...).
	// We deliberately skip NewShell so the passed-in env is used
	// verbatim, with no RUSH/AGENT/AI_AGENT injection: callers of
	// ExpandValue control the env, and nounset must treat any name
	// not in env as unset.
	cwd, _ := os.Getwd()
	s := &Shell{
		cwd:    cwd,
		env:    env,
		logger: noopLogger{},
	}

	strict := NoUnset.Load()

	cfg := &expand.Config{
		Env:     expand.ListEnviron(env...),
		NoUnset: strict,
		CmdSubst: func(w io.Writer, cs *syntax.CmdSubst) error {
			stderrBuf := new(bytes.Buffer)
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			limits := &commandSubstitutionLimitState{cancel: cancel}
			stdout := &commandSubstitutionWriter{
				dst:    w,
				limit:  maxInnerStdoutBytes,
				stream: "stdout",
				state:  limits,
			}
			stderr := &commandSubstitutionWriter{
				dst:    stderrBuf,
				limit:  maxInnerStderrBytes,
				stream: "stderr",
				state:  limits,
			}
			runnerOpts := []interp.RunnerOption{
				interp.StdIO(nil, stdout, stderr),
				interp.Interactive(false),
				interp.Env(expand.ListEnviron(env...)),
				interp.Dir(s.cwd),
				// Nested in-process runner: its children share the
				// parent's ctx-driven kill, and the parent Run's pid
				// registration already covers what the hooks abandon
				// path needs.
				execHandlerOption(s.blockFuncs, nil),
			}
			if strict {
				// Match the outer NoUnset: an unset $VAR inside
				// $(...) is also an error, not a silent empty.
				runnerOpts = append(runnerOpts, interp.Params("-u"))
			}
			runner, rerr := interp.New(runnerOpts...)
			if rerr != nil {
				return rerr
			}
			rerr = runner.Run(runCtx, &syntax.File{Stmts: cs.Stmts})
			if limitErr := limits.err(); limitErr != nil {
				return wrapCmdSubstErr(limitErr, stderr.bytes())
			}
			if rerr != nil {
				return wrapCmdSubstErr(rerr, stderr.bytes())
			}
			return nil
		},
		// ReadDir / ReadDir2 left nil: globbing is disabled.
	}

	return expand.Document(cfg, word)
}

type commandSubstitutionLimitState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	limit  *CommandSubstitutionOutputLimitError
}

func (s *commandSubstitutionLimitState) overflow(stream string, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit == nil {
		s.limit = &CommandSubstitutionOutputLimitError{Stream: stream, Limit: limit}
		s.cancel()
	}
	return s.limit
}

func (s *commandSubstitutionLimitState) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit == nil {
		return nil
	}
	return s.limit
}

type commandSubstitutionWriter struct {
	mu       sync.Mutex
	dst      io.Writer
	limit    int
	stream   string
	state    *commandSubstitutionLimitState
	written  int
	overflow error
}

func (w *commandSubstitutionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if w.overflow != nil {
		return 0, w.overflow
	}
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.overflow = w.state.overflow(w.stream, w.limit)
		return 0, w.overflow
	}

	allowed := len(p)
	if allowed > remaining {
		allowed = remaining
	}
	n, err := w.dst.Write(p[:allowed])
	w.written += n
	if err != nil {
		return n, err
	}
	if n != allowed {
		return n, io.ErrShortWrite
	}
	if allowed != len(p) {
		w.overflow = w.state.overflow(w.stream, w.limit)
		return allowed, w.overflow
	}
	return n, nil
}

func (w *commandSubstitutionWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	buf, ok := w.dst.(*bytes.Buffer)
	if !ok {
		return nil
	}
	return append([]byte(nil), buf.Bytes()...)
}

// HasCommandSubstitution reports whether value contains a command substitution
// according to the same document grammar used by ExpandValue. Parse failures
// are treated as dynamic so a resolver revision cannot remain stale.
func HasCommandSubstitution(value string) bool {
	word, err := syntax.NewParser().Document(strings.NewReader(value))
	if err != nil {
		return true
	}
	if word == nil {
		return false
	}

	found := false
	syntax.Walk(word, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// wrapCmdSubstErr attaches a bounded prefix of the inner command's stderr
// to the original error, if any.
func wrapCmdSubstErr(err error, stderrBytes []byte) error {
	msg := sanitizeStderr(stderrBytes)
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

// sanitizeStderr trims, bounds, and scrubs non-printable bytes from the
// stderr of a failing command so the result is safe to include in an
// error message shown to the user.
func sanitizeStderr(b []byte) string {
	b = bytes.TrimRight(b, "\n")
	if len(b) > maxInnerStderrBytes {
		b = b[:maxInnerStderrBytes]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c == '\t' || c == '\n' || (c >= 0x20 && c < 0x7f) {
			out[i] = c
		} else {
			out[i] = '?'
		}
	}
	return string(out)
}
