// Central command journaling: every rush invocation writes a start and an
// end record into the audit journal, including failed, denied and
// non-dispatched ones. Free text (prompts, passwords, keys, patterns) is
// never recorded; see the two allowlists below.
package clijournal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/audit"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// settingsAuditOnce guards the audit-dir resolver installation (moved from
// root.go with setSettingsAudit, lane K1).
var settingsAuditOnce sync.Once

// rootCmd is the live command tree root, installed by the cmd package via
// UseJournalRoot (lane K1 crossing: the fallback classifier needs Find).
var rootCmd *cobra.Command

// UseJournalRoot installs the command-tree root used by the journal fallback
// classifier and explicit-exit flush.
func UseJournalRoot(root *cobra.Command) { rootCmd = root }

// JournalAuditDir is the standard audit resolver: the directory holding the
// global rush.json.
func JournalAuditDir() string { return journalAuditDir() }

// EnsureAuditDirFunc installs the audit directory resolver exactly once;
// safe to call early (before config init) and repeatedly. Exported because
// cmdkit.SetSettingsAudit shares the resolver (lane K1 crossing).
func EnsureAuditDirFunc() { ensureAuditDirFunc() }

// ResetCommandJournalForTests clears the per-process journal state.
func ResetCommandJournalForTests() { resetCommandJournalForTests() }

// SetExitFunc replaces the process-exit indirection (test seam).
func SetExitFunc(f func(int)) { exitFunc = f }

// ExitFunc returns the current process-exit indirection (test seam).
func ExitFunc() func(int) { return exitFunc }

// ResetSettingsAuditOnceForTests re-arms the audit-resolver install once
// (test seam; the moved fallback-resolver scenario re-points audit's dir
// func at nil and expects the lazy install to fire again).
func ResetSettingsAuditOnceForTests() { settingsAuditOnce = sync.Once{} }

// LockCommandJournalForTests holds the journal mutex so a helper process
// spawn window writes no end record (test seam).
func LockCommandJournalForTests() { commandJournal.mu.Lock() }

// exitFunc is the process-exit indirection so tests can observe exits.
var exitFunc = os.Exit

const (
	// journalErrMaxRunes caps the end record's error text.
	journalErrMaxRunes = 200
	// journalPosMaxRunes caps each journalled positional argument.
	journalPosMaxRunes = 120
)

// journalFlagValueAllowlist lists typed flags whose VALUES may be
// journalled; every other changed flag is recorded by name only. Flags
// that can carry free text on any command (e.g. --format on `rush run`)
// are excluded or scoped in journalFlagValueAllowed.
var journalFlagValueAllowlist = map[string]bool{
	"session": true, "role": true, "model": true, "effort": true,
	"timeout": true, "cwd": true, "data-dir": true, "n": true,
	"audit-days": true, "color-scheme": true, "host": true, "port": true,
	"agents": true, "max-cost": true, "max-tokens": true,
	"wait": true, "by": true, "at": true,
}

// journalPositionalAllowlist lists full command paths whose positional
// arguments are identifiers (session/model/provider/mcp ids) and may be
// journalled; every other command's positionals are dropped entirely.
var journalPositionalAllowlist = map[string]bool{
	"rush sessions show": true, "rush sessions last": true,
	"rush sessions tail": true, "rush sessions watch": true,
	"rush sessions kill": true, "rush sessions why": true,
	"rush sessions locks": true, "rush sessions fork": true,
	"rush sessions cancel": true, "rush sessions tree": true,
	"rush sessions cost": true, "rush sessions diff": true,
	"rush sessions reset": true, "rush sessions inject": true,
	"rush models use": true, "rush models bump": true, "rush models unset": true,
	"rush providers show": true, "rush providers set": true,
	"rush providers add": true, "rush providers update": true,
	"rush providers enable": true, "rush providers disable": true,
	"rush providers unset": true, "rush providers fetch-models": true,
	"rush providers test": true, "rush providers patch": true,
	"rush mcp show": true, "rush mcp enable": true, "rush mcp disable": true,
	"rush mcp restart": true, "rush mcp test": true, "rush mcp add": true,
	"rush mcp remove": true, "rush mcp set": true,
}

// commandJournalState is the once-per-process record state shared by the
// pre-run start, the end writers and exitWithAudit.
type commandJournalState struct {
	mu           sync.Mutex
	startWritten bool
	endWritten   bool
	suppressed   bool
	cmd          string
	startedAt    time.Time
	execStart    time.Time
}

var commandJournal commandJournalState

// journalAuditDir is the standard audit resolver: the directory holding
// the global rush.json.
func journalAuditDir() string { return filepath.Dir(config.GlobalConfigData()) }

// ensureAuditDirFunc installs the audit directory resolver exactly once;
// safe to call early (before config init) and repeatedly.
func ensureAuditDirFunc() {
	settingsAuditOnce.Do(func() { audit.SetDirFunc(journalAuditDir) })
}

// journalWrite installs the audit resolver and appends one record; every
// writer goes through it so paths that skip the pre-run still journal.
func journalWrite(ev audit.Event) {
	ensureAuditDirFunc()
	_ = audit.Write(ev)
}

// journalExecutionStarted marks Execute's entry time for fallback records.
func JournalExecutionStarted() {
	commandJournal.mu.Lock()
	defer commandJournal.mu.Unlock()
	if commandJournal.execStart.IsZero() {
		commandJournal.execStart = time.Now()
	}
}

// journalCommandStart writes the start record first in the pre-run chain,
// before the password check; write failures are swallowed silently.
func JournalCommandStart(cmd *cobra.Command, args []string) {
	path := cmd.CommandPath()
	commandJournal.mu.Lock()
	defer commandJournal.mu.Unlock()
	if commandJournal.startWritten || commandJournal.endWritten || commandJournal.suppressed {
		return
	}
	if journalCommandSuppressed(cmd, path) {
		commandJournal.suppressed = true
		return
	}
	ev := audit.Event{"kind": "command_start", "cmd": path, "flags": journalFlagsRecord(cmd, path)}
	if pos := journalPositionalRecord(path, args); len(pos) > 0 {
		ev["args"] = pos
	}
	commandJournal.startWritten = true
	commandJournal.cmd = path
	commandJournal.startedAt = time.Now()
	journalWrite(ev)
}

// finalizeCommandJournal writes the end record exactly once. When no start
// was written (--help, --version, unknown command, flag-parse errors) it
// first writes the classified start record with a fixed error string, so
// cobra's raw-argument echo never reaches the journal.
func finalizeCommandJournal(rawArgs []string, runErr error) {
	code := 0
	if runErr != nil {
		code = 1
	}
	FinalizeCommandJournalWithCode(rawArgs, runErr, code)
}

func FinalizeCommandJournalWithCode(rawArgs []string, runErr error, code int) {
	errText := journalErrText(runErr)
	commandJournal.mu.Lock()
	defer commandJournal.mu.Unlock()
	if commandJournal.endWritten || commandJournal.suppressed {
		return
	}
	if !commandJournal.startWritten {
		path, kind, fixedErr, suppressed := classifyJournalFallback(rawArgs, runErr)
		if suppressed {
			commandJournal.suppressed = true
			return
		}
		journalWrite(audit.Event{"kind": kind, "cmd": path})
		commandJournal.startWritten = true
		commandJournal.cmd = path
		if commandJournal.startedAt.IsZero() {
			if !commandJournal.execStart.IsZero() {
				commandJournal.startedAt = commandJournal.execStart
			} else {
				commandJournal.startedAt = time.Now()
			}
		}
		if kind != "usage_error" {
			errText = ""
		} else {
			errText = fixedErr
		}
	}
	journalWriteEndLocked(code, errText)
}

// journalEndForPanic records exit 2 with the fixed error "panic" before
// recoverAndLogPanic re-panics; process behaviour is unchanged.
func JournalEndForPanic() {
	commandJournal.mu.Lock()
	defer commandJournal.mu.Unlock()
	if commandJournal.endWritten || commandJournal.suppressed {
		return
	}
	if !commandJournal.startWritten {
		if cmd, _, _ := rootCmd.Find(os.Args[1:]); cmd != nil {
			if journalCommandSuppressed(cmd, cmd.CommandPath()) {
				commandJournal.suppressed = true
				return
			}
			commandJournal.cmd = cmd.CommandPath()
		}
	}
	journalWriteEndLocked(2, "panic")
}

// exitWithAudit is the single wrapper for explicit exits in the command
// tree: it flushes the pending end record once, then exits.
func ExitWithAudit(code int) {
	commandJournal.mu.Lock()
	if !commandJournal.endWritten && !commandJournal.suppressed {
		if !commandJournal.startWritten {
			if cmd, _, _ := rootCmd.Find(os.Args[1:]); cmd != nil {
				commandJournal.cmd = cmd.CommandPath()
			}
		}
		journalWriteEndLocked(code, "")
	}
	commandJournal.mu.Unlock()
	exitFunc(code)
}

// journalWriteEndLocked writes the end record; callers hold the mutex.
func journalWriteEndLocked(code int, errText string) {
	dur := int64(0)
	if !commandJournal.startedAt.IsZero() {
		dur = time.Since(commandJournal.startedAt).Milliseconds()
	}
	ev := audit.Event{"kind": "command_end", "cmd": commandJournal.cmd, "exit": code, "dur_ms": dur}
	if errText != "" {
		ev["err"] = errText
	}
	journalWrite(ev)
	commandJournal.endWritten = true
}

// journalCommandSuppressed reports invocations that must never touch the
// journal: cobra's TAB-completion helpers and the completion subtree.
func journalCommandSuppressed(cmd *cobra.Command, path string) bool {
	if cmd != nil {
		switch cmd.Name() {
		case "__complete", "__completeNoDesc":
			return true
		}
	}
	return path == "rush completion" || strings.HasPrefix(path, "rush completion ")
}

// journalFlagsRecord maps every changed flag to its value (bools and
// allowlisted typed flags) or to nil (name only), never the password.
func journalFlagsRecord(cmd *cobra.Command, path string) map[string]any {
	flags := map[string]any{}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch {
		case f.Name == "password":
			flags[f.Name] = nil
		case journalFlagValueAllowed(f.Name, path, f.Value.Type()):
			flags[f.Name] = f.Value.String()
		case f.Value.Type() == "bool":
			flags[f.Name] = f.Value.String() == "true"
		default:
			flags[f.Name] = nil
		}
	})
	return flags
}

// journalFlagValueAllowed decides whether a typed flag's value may be
// journalled; --format is enum-shaped only inside the sessions subtree
// (`rush run --format` takes free text).
func journalFlagValueAllowed(name, path, ftype string) bool {
	switch ftype {
	case "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "duration":
	default:
		return false
	}
	if name == "format" {
		return strings.HasPrefix(path, "rush sessions ")
	}
	return journalFlagValueAllowlist[name]
}

// journalPositionalRecord returns the positionals only when the full
// command path is allowlisted; each is capped at 120 runes.
func journalPositionalRecord(path string, args []string) []string {
	if len(args) == 0 || !journalPositionalAllowlist[path] {
		return nil
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, journalTruncate(a, journalPosMaxRunes))
	}
	return out
}

// classifyJournalFallback kinds the record for invocations that never
// reached the pre-run chain; usage errors get fixed error strings.
func classifyJournalFallback(rawArgs []string, runErr error) (path, kind, fixedErr string, suppressed bool) {
	cmd, _, findErr := rootCmd.Find(rawArgs)
	if cmd == nil {
		return "rush", "usage_error", "unknown command", false
	}
	path = cmd.CommandPath()
	if journalCommandSuppressed(cmd, path) {
		return path, "", "", true
	}
	switch {
	case runErr == nil && hasBareToken(rawArgs, "--help", "-h"):
		kind = "help"
	case runErr == nil && hasBareToken(rawArgs, "--version", "-v"):
		kind = "version"
	case findErr != nil:
		kind, fixedErr = "usage_error", "unknown command"
	case !cmd.Runnable() && runErr == nil:
		kind = "help"
	default:
		kind, fixedErr = "usage_error", "flag parse error"
	}
	return path, kind, fixedErr, false
}

// hasBareToken reports whether args contain the flag in long form, exact
// short form, or inside a single-dash group (e.g. -dh for -h).
func hasBareToken(args []string, long, short string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == long || a == short {
			return true
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Contains(a[1:], short[1:]) {
			return true
		}
	}
	return false
}

// journalErrText is the error's first line, truncated to 200 runes.
func journalErrText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return journalTruncate(s, journalErrMaxRunes)
}

// journalTruncate cuts s to max runes, reserving the last for the ellipsis.
func journalTruncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// resetCommandJournalForTests clears the per-process journal state.
func resetCommandJournalForTests() {
	commandJournal.mu.Lock()
	defer commandJournal.mu.Unlock()
	commandJournal.startWritten, commandJournal.endWritten, commandJournal.suppressed = false, false, false
	commandJournal.cmd = ""
	commandJournal.startedAt = time.Time{}
	commandJournal.execStart = time.Now()
}

// FinalizeCommandJournal writes the end record with the default exit code
// mapping (exported wrapper; lane K1).
func FinalizeCommandJournal(rawArgs []string, runErr error) {
	finalizeCommandJournal(rawArgs, runErr)
}

// JournalCommandSuppressed reports invocations that must never touch the
// journal (exported wrapper; lane K1).
func JournalCommandSuppressed(cmd *cobra.Command, path string) bool {
	return journalCommandSuppressed(cmd, path)
}

// ClassifyJournalFallback kinds the record for invocations that never reached
// the pre-run chain (exported wrapper; lane K1).
func ClassifyJournalFallback(rawArgs []string, runErr error) (path, kind, fixedErr string, suppressed bool) {
	return classifyJournalFallback(rawArgs, runErr)
}

// JournalErrText is the error's first line, truncated to 200 runes (exported
// wrapper; lane K1).
func JournalErrText(err error) string { return journalErrText(err) }
