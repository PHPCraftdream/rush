package shell

import (
	"context"
	"errors"
	"fmt"
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

// xargsArgsHandler returns middleware that rewrites the option tokens of an
// `xargs` invocation before they reach the u-root Go coreutils handler, so
// that attached values ("-n1") and long options ("--max-args=3") are passed
// in the canonical form u-root parses safely. It is only installed when the
// Go coreutils are enabled. An invalid -n value fails the command with exit
// status 1 without invoking the downstream handler.
func xargsArgsHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 || args[0] != "xargs" {
				return next(ctx, args)
			}
			rewritten, err := normalizeXargsArgs(args[1:])
			if err != nil {
				fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, err.Error())
				return interp.ExitStatus(1)
			}
			out := make([]string, 0, len(rewritten)+1)
			out = append(out, args[0])
			return next(ctx, append(out, rewritten...))
		}
	}
}
