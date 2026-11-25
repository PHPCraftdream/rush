// Execute, panic recovery, error styling, color-scheme resolution and the
// unknown-subcommand guard for the Rush CLI. Lane K2 of the sprint0 cmd split:
// merged from the former cmd/root.go (Execute/recover/error-handler part),
// cmd/color_scheme.go and cmd/unknown_subcommand.go (design §2.3/§2.2 rows
// 20, 87, 160).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
	"github.com/PHPCraftdream/rush/internal/kit/sys/machinelock"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	out "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
	"github.com/spf13/cobra"
)

// crashLogMarker is the fixed message text logged by Execute's top-level
// recover before the process exits on an unrecovered panic anywhere in the
// command tree. Kept as a named constant (rather than an inline string) so
// `sessions why` (see its "status: crashed" branch) and this log site can't
// silently drift apart if one side's wording changes.
const crashLogMarker = "rush: fatal panic, exiting"

// recoverAndLogPanic is Execute's top-level panic handler. It logs the
// panic value and a full stack trace via slog.Error under crashLogMarker,
// then re-panics with the SAME value so the process's normal crash
// behavior (exit code, stderr trace) is unchanged for anyone watching the
// terminal directly — this only guarantees the trace is also durably
// logged first. Must be called via `defer recoverAndLogPanic()`, not
// invoked directly (recover() only has an effect when called from a
// deferred function).
//
// Go's default panic handler writes only to os.Stderr, never through slog
// — for a `rush run` invocation whose stderr isn't captured by whatever
// launched it (a backgrounded/redirected orchestrator run, the common case
// this fork is built for), an unrecovered panic anywhere in the command
// tree previously left rush.log with zero trace of what happened; the
// process just silently stopped updating its session lock. This does NOT
// change what happens to the process on a genuine panic — a real
// programmer error should still crash loudly — it only ensures the stack
// trace lands somewhere durable first: via whatever slog.Default()
// currently is, which for most real crashes (occurring well after
// SetupApp's rushlog.Setup call) is already pointed at
// <dataDir>/logs/rush.log.
func recoverAndLogPanic() {
	if r := recover(); r != nil {
		journal.JournalEndForPanic()
		slog.Error(crashLogMarker,
			"panic", r,
			"stack", string(debug.Stack()))
		panic(r)
	}
}

func commandErrorHandler(w io.Writer, styles fang.Styles, err error) {
	var unrecognized *config.ErrSettingsLockUnrecognized
	if errors.As(err, &unrecognized) {
		fmt.Fprintf(w, "rush: settings lock at %s was written by an older Rush or is not a valid record; remove sync_rev by hand and run rush lock again\n", unrecognized.Path)
		return
	}
	var timeoutErr *app.RunTimeoutError
	if !errors.As(err, &timeoutErr) {
		fang.DefaultErrorHandler(w, styles, err)
		return
	}
	fmt.Fprintf(w, "rush: %s; resume: %s; stored work remains in the session (rush sessions show/last)\n", timeoutErr.Error(), timeoutErr.ResumeCommand)
}

// Execute runs the Rush CLI. It builds a fresh root via NewRootCmd() (the
// former package-level rootCmd and its init-time construction are gone) and
// hands it to fang. Behavior — arg handling, exit codes, journaling — is
// unchanged.
func Execute() {
	defer recoverAndLogPanic()
	// Apply --global-dir first, before fang.Execute and hence before any
	// config/path resolution anywhere in the command tree.
	cmdkit.ApplyGlobalDir(globalDirFlagFromArgs(os.Args[1:]))
	journal.JournalExecutionStarted()
	// Every subcommand is registered by now (NewRootCmd has built the tree).
	root := NewRootCmd()
	guardUnknownSubcommands(root)

	options := []fang.Option{
		// Fork patch: show the fork's own release-line version (not
		// upstream's) plus build provenance. See version.VersionLine.
		fang.WithVersion(version.VersionLine()),
		fang.WithNotifySignal(proc.CommandSignals(os.Interrupt)...),
		fang.WithErrorHandler(commandErrorHandler),
	}

	// Resolve --color-scheme / RUSH_COLOR_SCHEME. fang builds its help/error
	// styles from the options we pass here, before cobra has parsed args, so
	// we read the flag straight from os.Args (and fall back to the env var).
	// Only force a palette when the user explicitly asked for light or dark;
	// "auto" leaves fang's own HasDarkBackground detection untouched (exact
	// historical behaviour — we don't even pass WithColorSchemeFunc).
	scheme := resolveColorScheme(
		colorSchemeFlagFromArgs(os.Args[1:]),
		os.Getenv("RUSH_COLOR_SCHEME"),
	)
	if scheme != ColorSchemeAuto {
		isDark := isDarkColorScheme(scheme)
		options = append(options, fang.WithColorSchemeFunc(
			func(_ lipgloss.LightDarkFunc) fang.ColorScheme {
				return fang.DefaultColorScheme(lipgloss.LightDark(isDark))
			},
		))
	}

	err := fang.Execute(
		context.Background(),
		root,
		options...,
	)
	code, tokenCommand := out.TokensExitCode(os.Args[1:], err)
	if err != nil && !tokenCommand {
		code = 1
	}
	journal.FinalizeCommandJournalWithCode(os.Args[1:], err, code)
	if err != nil {
		journal.ExitWithAudit(code)
	}
}

// InstallProcessStop is the package's public entry point (main.go); the
// implementation lives in cli/cmdkit/proc (moved in lane K1).
func InstallProcessStop() func() { return proc.InstallProcessStop() }

// colorSchemeFlagFromArgs scans a raw arg slice for the --color-scheme flag
// (in --color-scheme=value or --color-scheme value form, and the same with a
// single dash) and returns its value, or "" if absent. It deliberately does
// NOT validate the value — resolveColorScheme handles that. This is a minimal
// scanner: it stops at the first "--" (end-of-flags sentinel) and ignores
// values that are clearly another flag, so `--color-scheme --debug` is
// treated as "not set" rather than "--debug".
func colorSchemeFlagFromArgs(args []string) string {
	const name = "--color-scheme"
	const nameShort = "-color-scheme" // tolerate single-dash form

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ""
		}
		if a == name || a == nameShort {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			return "" // flag present but no value follows
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
		if strings.HasPrefix(a, nameShort+"=") {
			return strings.TrimPrefix(a, nameShort+"=")
		}
	}
	return ""
}

// globalDirFlagFromArgs scans a raw arg slice for the --global-dir flag
// (in --global-dir=value or --global-dir value form, and the same with a
// single dash) and returns its value, or "" if absent. Same shape and
// caveats as colorSchemeFlagFromArgs: it stops at the first "--"
// (end-of-flags sentinel) and ignores a value that is clearly another flag,
// so `--global-dir --debug` is treated as "not set" rather than "--debug".
func globalDirFlagFromArgs(args []string) string {
	const name = "--global-dir"
	const nameShort = "-global-dir" // tolerate single-dash form

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ""
		}
		if a == name || a == nameShort {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			return "" // flag present but no value follows
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
		if strings.HasPrefix(a, nameShort+"=") {
			return strings.TrimPrefix(a, nameShort+"=")
		}
	}
	return ""
}

// ColorSchemeAuto leaves fang's built-in light/dark auto-detection in charge
// (the historical behaviour when no override is configured).
const ColorSchemeAuto = "auto"

// resolveColorScheme resolves the effective color-scheme value from an
// explicit flag value and the RUSH_COLOR_SCHEME environment variable.
//
// Precedence: a non-empty flagValue wins over envValue. The result is one
// of "light", "dark", or "auto" (case-insensitive on input). Anything that
// is not a recognised value falls back to "auto" with a warning, so a
// typo can never take the CLI down — it just reverts to today's behaviour.
//
// This function is pure (no I/O) so it can be unit-tested without a real
// terminal. slog.Warn is the only side-effect, and only on invalid input.
func resolveColorScheme(flagValue, envValue string) string {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		raw = strings.TrimSpace(envValue)
	}
	if raw == "" {
		return ColorSchemeAuto
	}

	switch strings.ToLower(raw) {
	case "light", "dark", ColorSchemeAuto:
		return strings.ToLower(raw)
	default:
		slog.Warn(
			"Invalid color scheme value; falling back to auto",
			"value", raw,
			"valid_values", []string{"light", "dark", "auto"},
		)
		return ColorSchemeAuto
	}
}

// isDarkColorScheme reports whether a resolved color-scheme value should
// render for a dark background. "auto" is intentionally false here: callers
// must check for "auto" separately and skip forcing a scheme in that case.
func isDarkColorScheme(resolved string) bool {
	return resolved == "dark"
}

const unknownSubcommandGuard = "rush.unknown-subcommand-guard"

// guardUnknownSubcommands makes every command group reject an unknown
// subcommand with an error. Cobra only checks unknown commands on the root;
// a non-runnable group otherwise prints its help and exits 0, so a typo such
// as `rush sessions lsit` silently succeeds.
func guardUnknownSubcommands(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		guardUnknownSubcommands(c)
	}
	if !cmd.HasParent() || !cmd.HasSubCommands() || cmd.Runnable() {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[unknownSubcommandGuard] = "true"
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		return unknownSubcommandError(c, args[0])
	}
}

func unknownSubcommandError(cmd *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	// Cobra applies its default distance (2) only on the root.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return errors.New(msg)
}

func addMachineLockCommands(cmd *cobra.Command) {
	cmd.AddCommand(newLockRunCmd(), newLockListCmd())
}

func newLockListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List machine-wide locks and stuck-holder diagnostics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := machinelock.List()
			if err != nil {
				return err
			}
			for _, entry := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", entry.Name)
				if h := entry.Holder; h != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  holder pid=%d age=%s started=%s checkout=%s command=%s\n", h.PID, time.Since(h.StartedAt).Round(time.Second), h.StartedAt.Format(time.RFC3339), h.Checkout, h.Command)
					if h.Stuck {
						fmt.Fprintf(cmd.OutOrStdout(), "  STUCK: holder is left running. Inspect/resolve manually; use --on-stuck=release or --on-stuck=stop on the run command.\n%s\n", h.Snapshot)
					}
				}
				for _, h := range entry.Quarantined {
					fmt.Fprintf(cmd.OutOrStdout(), "  QUARANTINED stuck pid=%d checkout=%s command=%s\n%s\n", h.PID, h.Checkout, h.Command, h.Snapshot)
				}
				for _, w := range entry.Waiters {
					fmt.Fprintf(cmd.OutOrStdout(), "  waiter pid=%d age=%s since=%s command=%s\\n", w.PID, time.Since(w.Since).Round(time.Second), w.Since.Format(time.RFC3339), w.Command)
				}
			}
			return nil
		},
	}
}

type minuteDuration struct{ value *time.Duration }

func (d minuteDuration) String() string {
	if d.value == nil {
		return ""
	}
	return d.value.String()
}

func (d minuteDuration) Set(s string) error {
	parsed, err := time.ParseDuration(s)
	if err != nil {
		if n, parseErr := strconv.ParseInt(s, 10, 64); parseErr == nil {
			parsed = time.Duration(n) * time.Minute
		} else {
			return err
		}
	}
	*d.value = parsed
	return nil
}
func (minuteDuration) Type() string { return "duration" }

func newLockRunCmd() *cobra.Command {
	var timeout, wait, idle time.Duration
	var onStuck string
	var forceSafe bool
	cmd := &cobra.Command{
		Use:     "run <name> [--timeout D] [--wait D] [--idle D] [--on-stuck=wait|release|stop] [--force-safe] -- <command...>",
		Short:   "Run a command under a lock; diagnose stalls without killing by default",
		Example: "rush lock run go-build -- go build ./...\nrush lock run go-test -- go test ./...\nrush lock run go-vet -- go vet ./...\nrush lock run golangci-lint -- golangci-lint run\n\nSerialize heavy builds with rush lock run — do not write wrapper scripts.",
		Long:    "`rush lock run` serializes expensive commands across checkouts/worktrees. Examples: `rush lock run go-build -- go build ./...`, `rush lock run go-test -- go test ./...`, `rush lock run go-vet -- go vet ./...`, `rush lock run golangci-lint -- golangci-lint run`. Do not write wrapper scripts. Waiters queue FIFO; holder PID, command line, start time, and checkout are recorded. Stuck is diagnosed from no CPU/I/O progress (default 10m idle), or a child exit while its parent hangs—not wall-clock time alone. A diagnostic snapshot is saved to the lock record and printed to waiter stderr before action. Default `--on-stuck=wait` marks stuck, warns about every minute, never kills, and keeps the queue waiting. `release` quarantines the holder and advances the queue while leaving its tree running. `stop` is graceful-first; force termination requires `--force-safe`. `--timeout` is an upper bound triggering this same stuck handling, not a kill deadline. A gone holder tree is taken over without a kill; exit codes pass through. `rush lock list` shows holders/waiters with ages and snapshots. Bare numeric durations are minutes (90 means 90m); defaults: --timeout disabled, --wait 120m, --idle 10m. See `rush lock run --help` for options.",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, argv := args[0], args[1:]
			if len(argv) > 0 && argv[0] == "--" {
				argv = argv[1:]
			}
			if len(argv) == 0 {
				return fmt.Errorf("command is required")
			}
			code, err := machinelock.RunWithOptions(cmd.Context(), name, machinelock.Record{Command: strings.Join(argv, " ")}, machinelock.Options{Wait: wait, Timeout: timeout, Idle: idle, OnStuck: machinelock.StuckAction(onStuck), ForceSafe: forceSafe}, argv, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return lockExitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().Var(minuteDuration{&timeout}, "timeout", "Upper bound before stuck handling (0 disables; bare numbers are minutes)")
	cmd.Flags().Var(minuteDuration{&wait}, "wait", "Maximum lock wait (default 120m; bare numbers are minutes)")
	cmd.Flags().Var(minuteDuration{&idle}, "idle", "No-progress period before stuck diagnosis (default 10m; bare numbers are minutes)")
	cmd.Flags().StringVar(&onStuck, "on-stuck", "wait", "Stuck action: wait, release, or stop (default wait)")
	cmd.Flags().BoolVar(&forceSafe, "force-safe", false, "Declare the command safe for force termination after graceful stop fails")
	idle = 10 * time.Minute
	wait = 2 * time.Hour
	return cmd
}

type lockExitError struct{ code int }

func (e lockExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.code) }
func (e lockExitError) ExitCode() int { return e.code }
