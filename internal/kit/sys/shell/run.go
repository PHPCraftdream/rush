package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shexec"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shpolicy"
)

// RunOptions configures a single stateless shell execution via [Run].
//
// The zero value is not useful; at minimum Command must be set. Stdin,
// Stdout, and Stderr may be nil (nil readers/writers are treated as
// empty/discard). BlockFuncs may be nil to disable block-list enforcement —
// hooks use this to run user-authored commands with the same trust level as
// a shell alias.
type RunOptions struct {
	// Command is the shell source to parse and execute.
	Command string
	// Cwd is the working directory for the execution. Required: callers
	// must supply a non-empty value. Run does not silently fall back to
	// the Rush process cwd — hooks and the bash tool have different
	// notions of "default" and each owns that decision.
	Cwd string
	// Env is the full environment visible to the command. The caller is
	// responsible for inheriting from os.Environ() if that's desired.
	Env []string
	// Stdin is the command's standard input. nil is equivalent to an empty
	// input stream.
	Stdin io.Reader
	// Stdout receives the command's standard output. nil discards output.
	Stdout io.Writer
	// Stderr receives the command's standard error. nil discards output.
	Stderr io.Writer
	// BlockFuncs is an optional list of deny-list matchers applied before
	// each command reaches the exec layer. nil disables blocking entirely.
	BlockFuncs []BlockFunc
	// RegisterProcess, when non-nil, is called with the pid of every
	// child process the interpreter's exec layer starts, immediately
	// after a successful Start and before Wait. It lets a caller that
	// has given up waiting on a wedged execution (see internal/hooks'
	// abandon path) hard-kill exactly the processes involved.
	// Implementations must be safe for concurrent use: a script may
	// background several commands at once. Processes started via the
	// shebang-dispatch path (./script.sh with a #! line) are NOT
	// registered; their teardown relies on the ctx-driven kill of
	// platform.Command.
	RegisterProcess func(pid int)
}

// Run parses and executes a shell command using the same mvdan.cc/sh
// interpreter stack that the stateful [Shell] type uses (builtins,
// optional block list, optional Go coreutils). It is safe to call
// concurrently from multiple goroutines: each call builds its own
// [interp.Runner] and shares no state with other callers or with any
// [Shell] instance.
//
// Errors returned from the command itself (non-zero exit, context
// cancellation, parse failures) follow the same conventions as
// [Shell.Exec]: inspect with [IsInterrupt] and [ExitCode].
func Run(ctx context.Context, opts RunOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("command execution panic: %v", r)
		}
	}()

	if opts.Cwd == "" {
		return fmt.Errorf("shell.Run: Cwd is required")
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	line, err := syntax.NewParser().Parse(strings.NewReader(opts.Command), "")
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}

	runner, err := newRunner(opts.Cwd, opts.Env, opts.Stdin, stdout, stderr, opts.BlockFuncs, opts.RegisterProcess)
	if err != nil {
		return fmt.Errorf("could not run command: %w", err)
	}

	return runner.Run(ctx, line)
}

// newRunner constructs an [interp.Runner] configured with the standard
// Rush handler stack. Shared by the stateless [Run] entrypoint and the
// stateful [Shell] so the two surfaces cannot drift.
func newRunner(cwd string, env []string, stdin io.Reader, stdout, stderr io.Writer, blockFuncs []BlockFunc, registerProcess func(int)) (*interp.Runner, error) {
	env = withNonInteractiveEnv(env)
	return interp.New(
		interp.StdIO(stdin, stdout, stderr),
		interp.Interactive(false),
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir(cwd),
		execHandlerOption(blockFuncs, registerProcess),
	)
}

// execHandlerOption returns an interp.RunnerOption that installs the
// standard Rush middleware chain (builtins, script dispatch, block list)
// on top of a process-group-isolated base exec handler.
//
// We use interp.ExecHandler (singular) with a manually-built chain rather
// than interp.ExecHandlers because the latter always appends
// interp.DefaultExecHandler as the final handler, which lacks process group
// isolation. On Unix that base handler is processGroupExecHandler, which
// detaches each child into its own session and kills the whole child
// process group on cancellation; on Windows it is the unmodified default.
func execHandlerOption(blockFuncs []BlockFunc, registerProcess func(int)) interp.RunnerOption {
	base := shexec.ProcessGroupExecHandler(shexec.DefaultKillTimeout, registerProcess)
	handler := base
	for _, mw := range slices.Backward(standardHandlers(blockFuncs)) {
		handler = mw(handler)
	}
	return interp.ExecHandler(handler)
}

// nonInteractiveEnvVars are forced on every shell execution to prevent
// commands from hanging on a nonexistent TTY. These are always applied
// regardless of the caller's environment because Rush shells are never
// interactive — preserving user preferences like EDITOR=nvim only causes
// hangs, not useful behavior.
// Ported from upstream c2be8cbf (#3025).
var nonInteractiveEnvVars = []string{
	"TERM=xterm-256color",
	"GIT_EDITOR=false",
	"EDITOR=false",
	"VISUAL=false",
	"JJ_EDITOR=false",
	"JJ_PAGER=cat",
	"GIT_PAGER=cat",
	"PAGER=cat",
}

// withNonInteractiveEnv returns env with nonInteractiveEnvVars forced in,
// replacing any existing values for those keys. The returned slice is a
// new allocation safe to use concurrently with the input.
func withNonInteractiveEnv(env []string) []string {
	overrideKeys := make(map[string]bool, len(nonInteractiveEnvVars))
	for _, kv := range nonInteractiveEnvVars {
		if key, _, ok := strings.Cut(kv, "="); ok {
			overrideKeys[key] = true
		}
	}
	result := make([]string, 0, len(env)+len(nonInteractiveEnvVars))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && overrideKeys[key] {
			continue
		}
		result = append(result, e)
	}
	return append(result, nonInteractiveEnvVars...)
}

// execMiddleware wraps a base [interp.ExecHandlerFunc], composing like HTTP
// middleware: each layer either handles a command itself or delegates to the
// next handler in the chain.
type execMiddleware = func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc

// testInnermostHandler is a test-only seam: when non-nil it is installed as
// the innermost exec handler of the standard chain so package tests can run
// the real middleware stack (panic guard, builtins, script dispatch, block
// list, xargs normaliser, Go coreutils) with a fake command behind it. It is
// always nil outside tests, and tests only set it from sequential (never
// parallel) test code, restoring it via t.Cleanup.
var testInnermostHandler interp.ExecHandlerFunc

// standardHandlers returns the exec-handler middleware chain used by both
// [Run] and [Shell]. Order matters:
//  1. panic guard (outermost — a panic in any layer below becomes one failed
//     command with exit status 2);
//  2. CI-only command policy (before builtins and every execution handler);
//  3. builtins first (so Rush's in-process jq wins over any PATH binary);
//  4. script dispatch (shebang / binary / shell-source for path-prefixed
//     argv[0], no-op for bare commands) — runs before the block list so
//     that deny rules see the already-resolved argv of anything the
//     script exec's rather than the outer path-prefixed wrapper;
//  5. block list;
//  6. when the Go coreutils are on: the xargs argument normaliser
//     immediately before the coreutils handler, then the optional Go
//     coreutils themselves.
func standardHandlers(blockFuncs []BlockFunc) []execMiddleware {
	handlers := []execMiddleware{
		shpolicy.PanicGuardHandler(),
		ciOnlyPolicyHandler(),
		builtinHandler(),
		scriptDispatchHandler(blockFuncs),
		blockHandler(blockFuncs),
	}
	if shexec.UseGoCoreUtils && shexec.CoreUtilsExecHandler != nil {
		handlers = append(handlers, xargsArgsHandler(), shexec.CoreUtilsExecHandler)
	}
	if testInnermostHandler != nil {
		handlers = append(handlers, func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
			return testInnermostHandler
		})
	}
	return handlers
}

func ciOnlyPolicyHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if err := checkRegisteredCommandPolicy(args); err != nil {
				return err
			}
			return next(ctx, args)
		}
	}
}

// builtinHandler returns middleware that dispatches recognized Rush
// builtins to their in-process Go implementations. Currently: jq.
func builtinHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			switch args[0] {
			case "jq":
				hc := interp.HandlerCtx(ctx)
				return handleJQ(ctx, args, hc.Stdin, hc.Stdout, hc.Stderr)
			default:
				return next(ctx, args)
			}
		}
	}
}

// blockHandler returns middleware that rejects commands matched by any of
// the provided [BlockFunc]s before they reach the underlying exec path.
// A nil or empty blockFuncs slice is a no-op.
func blockHandler(blockFuncs []BlockFunc) func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			for _, blockFunc := range blockFuncs {
				if blockFunc(args) {
					return fmt.Errorf("command is not allowed for security reasons: %q", args[0])
				}
			}
			return next(ctx, args)
		}
	}
}

const xargsInvalidNMessage = "xargs: invalid number for -n option"

var errInvalidXargsN = errors.New(xargsInvalidNMessage)

// parseXargsN parses the value of the xargs -n / --max-args option. Any
// non-numeric value, a missing value, zero, and negative numbers are all
// rejected with [errInvalidXargsN]; zero would also make the u-root xargs
// loop spin forever, since it never advances past the current argument.
func parseXargsN(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, errInvalidXargsN
	}
	return n, nil
}

// normalizeXargsArgs rewrites the option tokens of an xargs invocation (the
// tokens after the leading "xargs" word) into the canonical short form the
// u-root Go coreutils handler accepts: u-root splits short-flag clusters into
// single-character flags, so a value attached to a flag letter ("-n1") would
// be misread and can panic or hang the handler. Option scanning stops at the
// first non-option token or at "--"; everything after it is the COMMAND plus
// its arguments and is appended verbatim.
func normalizeXargsArgs(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Everything after "--" is verbatim, including the "--" itself.
			out = append(out, args[i:]...)
			return out, nil
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			// First command word: the rest is the COMMAND and its arguments.
			out = append(out, args[i:]...)
			return out, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			switch name {
			case "max-args":
				if !hasValue {
					i++
					if i >= len(args) {
						return nil, errInvalidXargsN
					}
					value = args[i]
				}
				n, err := parseXargsN(value)
				if err != nil {
					return nil, err
				}
				out = append(out, "-n", strconv.Itoa(n))
			case "null":
				out = append(out, "-0")
			case "verbose":
				out = append(out, "-t")
			default:
				// Leave unknown long options for u-root to report.
				out = append(out, arg)
			}
		default:
			// Short cluster: walk the characters after the leading "-".
			cluster := []string{}
			ok := true
		cluster:
			for j := 1; j < len(arg); j++ {
				switch c := arg[j]; c {
				case 't', 'p', '0':
					cluster = append(cluster, "-"+string(c))
				case 'n':
					value := arg[j+1:]
					if value == "" {
						i++
						if i >= len(args) {
							return nil, errInvalidXargsN
						}
						value = args[i]
					}
					n, err := parseXargsN(value)
					if err != nil {
						return nil, err
					}
					cluster = append(cluster, "-n", strconv.Itoa(n))
					break cluster
				default:
					// Unknown flag letter: emit the raw token untouched.
					cluster = nil
					ok = false
					break cluster
				}
			}
			if ok {
				out = append(out, cluster...)
			} else {
				out = append(out, arg)
			}
		}
	}
	return out, nil
}

// xargsDefaultMaxArgs matches the u-root xargs default batch size.
const xargsDefaultMaxArgs = 5000

// xargsPlan is a normalized xargs invocation this package runs itself.
type xargsPlan struct {
	maxArgs int
	trace   bool
	null    bool
	command []string
}

// planXargs reads the canonical tokens produced by normalizeXargsArgs. It
// reports false for anything it does not run itself (-p, an unknown option):
// those stay with the u-root handler, which owns the prompt and the error text.
func planXargs(args []string) (xargsPlan, bool) {
	plan := xargsPlan{maxArgs: xargsDefaultMaxArgs}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			plan.command = args[i+1:]
			return plan, true
		case arg == "-n" && i+1 < len(args):
			n, err := parseXargsN(args[i+1])
			if err != nil {
				return plan, false
			}
			plan.maxArgs = n
			i++
		case arg == "-t":
			plan.trace = true
		case arg == "-0":
			plan.null = true
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			plan.command = args[i:]
			return plan, true
		default:
			return plan, false
		}
	}
	return plan, true
}

// readXargsItems splits standard input the way u-root xargs does: NUL
// separated with -0, otherwise whitespace separated fields of every line.
func readXargsItems(r io.Reader, null bool) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	var items []string
	if !null {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			items = append(items, strings.Fields(scanner.Text())...)
		}
		return items, nil
	}
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadBytes(0)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n := len(b); n != 0 {
			if b[n-1] == 0 {
				b = b[:n-1]
			}
			items = append(items, string(b))
		}
		if err == io.EOF {
			return items, nil
		}
	}
}

// xargsArgsHandler returns middleware that owns xargs when the Go coreutils
// are on. It rewrites the option tokens into the canonical short form (see
// normalizeXargsArgs) and then runs every COMMAND batch through the exec
// chain below it instead of letting u-root xargs start the process: u-root
// calls os/exec directly, which skips the sanctioned process handler, so on
// Windows each batch opened a console window (no CREATE_NO_WINDOW) and ran
// outside the working directory, environment and kill handling of the
// shell. The first failing batch ends the run with its status, as before.
// It is installed immediately before the coreutils handler and passes
// non-xargs commands through unchanged.
func xargsArgsHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		var handler interp.ExecHandlerFunc
		handler = func(ctx context.Context, args []string) error {
			if len(args) == 0 || args[0] != "xargs" {
				return next(ctx, args)
			}
			hc := interp.HandlerCtx(ctx)
			rewritten, err := normalizeXargsArgs(args[1:])
			if err != nil {
				fmt.Fprintln(hc.Stderr, err.Error())
				return interp.ExitStatus(1)
			}
			plan, ok := planXargs(rewritten)
			if !ok {
				out := make([]string, 0, len(rewritten)+1)
				out = append(out, args[0])
				return next(ctx, append(out, rewritten...))
			}
			if len(plan.command) == 0 {
				plan.command = []string{"echo"}
			}
			items, err := readXargsItems(hc.Stdin, plan.null)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "xargs: "+err.Error())
				return interp.ExitStatus(1)
			}
			for i := 0; i < len(items); i += plan.maxArgs {
				batch := make([]string, 0, len(plan.command)+plan.maxArgs)
				batch = append(batch, plan.command...)
				batch = append(batch, items[i:min(i+plan.maxArgs, len(items))]...)
				if plan.trace {
					fmt.Fprintln(hc.Stderr, strings.Join(batch, " "))
				}
				// A nested xargs takes this handler again, never u-root's.
				if err := handler(ctx, batch); err != nil {
					return err
				}
			}
			return nil
		}
		return handler
	}
}

const jqUsage = `jq - Go implementation of jq (gojq 0.12.19 builtin)

Synopsis:
  %% echo '{"foo": 128}' | jq '.foo'

Usage:
  jq [OPTIONS] [FILTER] [FILE...]

Options:
  -r, --raw-output              output raw strings
  -j, --join-output             implies -r with no newline delimiter
  -c, --compact-output          output without pretty-printing
  -s, --slurp                   read all inputs into an array
  -n, --null-input              use null as input value
  -e, --exit-status             exit 1 when the last value is false or null
  -R, --raw-input               read input as raw strings
      --arg name value          set a string value to a variable
      --argjson name value      set a JSON value to a variable
  -h, --help                    display this help
`

// handleJQ implements the jq builtin using gojq. It supports a subset of jq
// flags: -r (raw output), -c (compact output), -s (slurp), -n (null input),
// -e (exit status), -R (raw input), and --arg name value.
//
// ctx is polled at each iteration of the output loop and at each reader in
// [readInputs] so that hook timeouts or other cancellations can interrupt
// long-running queries. A cancelled context surfaces as ctx.Err(), not an
// [interp.ExitStatus], so callers (e.g. the hook runner) can distinguish
// "filter exited non-zero" from "we ran out of time".
//
// Note that this is somewhat of a reimplmentation of the CLI of the glorious
// github.com/itchyny/gojq, and we'd ideally get the CLI exposed upstream to
// avoid this falling out of sync.
func handleJQ(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var (
		rawOutput  bool
		compact    bool
		slurp      bool
		nullInput  bool
		exitStatus bool
		rawInput   bool
		joinOutput bool
		argNames   []string
		argValues  []any
	)

	// Parse flags and extract the query.
	var queryStr string
	var fileArgs []string
	i := 1 // skip "jq"
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprint(stdout, jqUsage)
			return nil
		case arg == "-r" || arg == "--raw-output":
			rawOutput = true
		case arg == "-j" || arg == "--join-output":
			joinOutput = true
			rawOutput = true
		case arg == "-c" || arg == "--compact-output":
			compact = true
		case arg == "-s" || arg == "--slurp":
			slurp = true
		case arg == "-n" || arg == "--null-input":
			nullInput = true
		case arg == "-e" || arg == "--exit-status":
			exitStatus = true
		case arg == "-R" || arg == "--raw-input":
			rawInput = true
		case arg == "--arg":
			if i+2 >= len(args) {
				fmt.Fprintf(stderr, "jq: --arg requires name and value\n")
				return interp.ExitStatus(2)
			}
			argNames = append(argNames, "$"+args[i+1])
			argValues = append(argValues, args[i+2])
			i += 2
		case arg == "--argjson":
			if i+2 >= len(args) {
				fmt.Fprintf(stderr, "jq: --argjson requires name and value\n")
				return interp.ExitStatus(2)
			}
			var val any
			if err := json.Unmarshal([]byte(args[i+2]), &val); err != nil {
				fmt.Fprintf(stderr, "jq: invalid JSON for --argjson %s: %s\n", args[i+1], err)
				return interp.ExitStatus(2)
			}
			argNames = append(argNames, "$"+args[i+1])
			argValues = append(argValues, val)
			i += 2
		case arg == "--":
			i++
			// Remaining args are file arguments.
			for i < len(args) {
				fileArgs = append(fileArgs, args[i])
				i++
			}
			continue
		case strings.HasPrefix(arg, "-") && queryStr != "":
			fmt.Fprintf(stderr, "jq: unknown option: %s\n", arg)
			return interp.ExitStatus(2)
		default:
			if queryStr == "" {
				queryStr = arg
			} else {
				fileArgs = append(fileArgs, arg)
			}
		}
		i++
	}

	if queryStr == "" {
		queryStr = "."
	}

	query, err := gojq.Parse(queryStr)
	if err != nil {
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(3)
	}

	opts := []gojq.CompilerOption{
		gojq.WithEnvironLoader(os.Environ),
	}
	if len(argNames) > 0 {
		opts = append(opts, gojq.WithVariables(argNames))
	}

	code, err := gojq.Compile(query, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(3)
	}

	// Build input values.
	inputs, err := readInputs(ctx, stdin, fileArgs, nullInput, rawInput, slurp)
	if err != nil {
		// Prefer surfacing ctx cancellation verbatim so timeouts are
		// distinguishable from user input errors.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		fmt.Fprintf(stderr, "jq: %s\n", err)
		return interp.ExitStatus(2)
	}

	var lastFalsy bool
	for _, input := range inputs {
		iter := code.Run(input, argValues...)
		for {
			// Poll ctx on every value so a long-running filter (e.g. a
			// generator over a slurped array) can be interrupted by hook
			// timeouts without waiting for iter.Next to yield.
			if err := ctx.Err(); err != nil {
				return err
			}
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, ok := v.(error); ok {
				fmt.Fprintf(stderr, "jq: %s\n", err)
				return interp.ExitStatus(5)
			}
			if exitStatus {
				lastFalsy = v == nil || v == false
			}
			if err := writeValue(stdout, v, rawOutput, compact, joinOutput); err != nil {
				return err
			}
		}
	}

	if exitStatus && lastFalsy {
		return interp.ExitStatus(1)
	}
	return nil
}

// readInputs reads JSON (or raw) input values from stdin or files.
//
// ctx is polled in three places so that a cancellation observed mid-read
// short-circuits promptly:
//   - between readers (before opening the next file / consuming stdin);
//   - on every io.Read call via ctxReader, so io.ReadAll on a large but
//     non-blocking source (e.g. the bytes.NewReader payload the hook
//     runner supplies) returns ctx.Err() on the next chunk boundary;
//   - inside the post-read value accumulation loops (raw-input line
//     split and JSON stream decode), which are otherwise unbounded in
//     the size of the input.
//
// A reader that blocks forever in Read (e.g. an unterminated pipe) can
// still outlast ctx; the outer abandon-goroutine path in the hook
// runner (internal/hooks/runner.go) is the authoritative enforcer for
// that case.
func readInputs(ctx context.Context, stdin io.Reader, files []string, nullInput, rawInput, slurp bool) ([]any, error) {
	if nullInput {
		return []any{nil}, nil
	}

	var readers []io.Reader
	if len(files) > 0 {
		for _, f := range files {
			file, err := os.Open(f)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			readers = append(readers, file)
		}
	} else {
		readers = []io.Reader{stdin}
	}

	var vals []any
	for _, r := range readers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(ctxReader{ctx: ctx, r: r})
		if err != nil {
			// ctxReader surfaces ctx.Err() verbatim; preserve it so the
			// caller can distinguish cancellation from a parse error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}

		if rawInput {
			lines := strings.Split(string(data), "\n")
			if slurp {
				vals = append(vals, strings.Join(lines, "\n"))
			} else {
				for _, line := range lines {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					if line != "" || !slurp {
						vals = append(vals, line)
					}
				}
			}
			continue
		}

		// Decode potentially multiple JSON values from the stream.
		dec := json.NewDecoder(strings.NewReader(string(data)))
		var streamVals []any
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var v any
			if err := dec.Decode(&v); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("parse error: %w", err)
			}
			streamVals = append(streamVals, v)
		}

		if slurp {
			vals = append(vals, streamVals)
		} else {
			vals = append(vals, streamVals...)
		}
	}

	if len(vals) == 0 {
		return []any{nil}, nil
	}
	return vals, nil
}

// ctxReader wraps an io.Reader so that each Read call checks ctx first.
// This makes io.ReadAll over a large but non-blocking source (e.g. a
// bytes.Reader of the hook stdin payload) cancellable on the next chunk
// boundary. A reader that itself blocks in Read will still outlast ctx —
// the hook runner's abandon-goroutine path is the enforcer of last resort
// for that case.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// writeValue writes a single jq output value.
func writeValue(w io.Writer, v any, raw, compact, join bool) error {
	if raw {
		if s, ok := v.(string); ok {
			if _, err := fmt.Fprint(w, s); err != nil {
				return err
			}
			if !join {
				_, err := fmt.Fprint(w, "\n")
				return err
			}
			return nil
		}
	}

	var bs []byte
	var err error
	if compact {
		bs, err = gojq.Marshal(v)
	} else {
		bs, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		return err
	}
	if _, writeErr := w.Write(bs); writeErr != nil {
		return writeErr
	}
	_, err = fmt.Fprint(w, "\n")
	return err
}
