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
)

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
