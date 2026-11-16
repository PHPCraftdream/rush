package shell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

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
