// Command-journal fallback and oracle tests. REVERT-CHECKS (single production
// change each test must catch):
//
//	TestJournalFallbackWritesWithoutPriorResolver: removing ensureAuditDirFunc
//	    from journalWrite (fallback, exit and panic records are then lost).
//	TestJournalGroupUsageErrorNotHelp: dropping `&& runErr == nil` from the
//	    non-runnable case in classifyJournalFallback.
//	TestJournalErrTextFirstLineAndTruncation: removing the first-line cut or
//	    the rune cap in journalErrText.
//	TestJournalExecuteEndToEnd: removing finalizeCommandJournal or the
//	    exitWithAudit(1) call from Execute() in root.go.
//	TestJournalAllowlistsMatchOracle / TestJournalCanary: adding any entry to
//	    journalFlagValueAllowlist or journalPositionalAllowlist (or to the
//	    format special case) without editing the literals below.
package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The oracle is deliberately a literal copy, NOT derived from production
// code: widening an allowlist must take a matching edit here.
var journalOracleFlags = map[string]bool{
	"session": true, "role": true, "model": true, "effort": true,
	"timeout": true, "cwd": true, "data-dir": true, "n": true,
	"audit-days": true, "color-scheme": true, "host": true, "port": true,
	"agents": true, "max-cost": true, "max-tokens": true,
	"wait": true, "by": true, "at": true,
}

var journalOraclePositional = map[string]bool{
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

// journalOracleFlagAllowed is the independent expectation for string flags.
func journalOracleFlagAllowed(name, path string) bool {
	if name == "format" {
		return strings.HasPrefix(path, "rush sessions ")
	}
	return journalOracleFlags[name]
}

func TestJournalAllowlistsMatchOracle(t *testing.T) {
	require.Equal(t, journalOracleFlags, journalFlagValueAllowlist)
	require.Equal(t, journalOraclePositional, journalPositionalAllowlist)
}

func TestJournalFallbackWritesWithoutPriorResolver(t *testing.T) {
	journalTestEnv(t)
	oldExit := exitFunc
	exitFunc = func(int) {}
	t.Cleanup(func() {
		exitFunc = oldExit
		settingsAuditOnce = sync.Once{}
		ensureAuditDirFunc()
	})
	cases := []struct {
		name string
		want int
		run  func()
	}{
		{"finalize-help", 2, func() { finalizeCommandJournal([]string{"--help"}, nil) }},
		{"exit-before-prerun", 1, func() { exitWithAudit(3) }},
		{"panic", 1, journalEndForPanic},
	}
	for _, tc := range cases {
		resetCommandJournalForTests()
		before := len(journalRecords(t))
		audit.SetDirFunc(nil)
		settingsAuditOnce = sync.Once{}
		tc.run()
		require.Len(t, journalRecords(t), before+tc.want, tc.name)
	}
}

func TestJournalGroupUsageErrorNotHelp(t *testing.T) {
	var group *cobra.Command
	for _, c := range rootCmd.Commands() {
		if !c.Runnable() && c.HasSubCommands() && !journalCommandSuppressed(c, c.CommandPath()) {
			group = c
			break
		}
	}
	require.NotNil(t, group, "no non-runnable command group in the tree")
	path, kind, fixedErr, suppressed := classifyJournalFallback(
		[]string{group.Name(), "--definitely-no-such-flag"}, errors.New("unknown flag: --definitely-no-such-flag"))
	require.False(t, suppressed)
	require.Equal(t, group.CommandPath(), path)
	require.Equal(t, "usage_error", kind)
	require.Equal(t, "flag parse error", fixedErr)

	_, kind, fixedErr, _ = classifyJournalFallback([]string{group.Name()}, nil)
	require.Equal(t, "help", kind)
	require.Empty(t, fixedErr)
}

func TestJournalErrTextFirstLineAndTruncation(t *testing.T) {
	require.Equal(t, "first line", journalErrText(errors.New("first line\nsecond line SECRET")))
	got := journalErrText(errors.New(strings.Repeat("é", 300) + "\nmore"))
	require.Equal(t, journalErrMaxRunes, utf8.RuneCountInString(got))
	require.True(t, strings.HasSuffix(got, "…"))
	require.NotContains(t, got, "more")
	require.Empty(t, journalErrText(nil))
}

// TestJournalExecuteEndToEnd runs the real Execute() in a child process (the
// test binary re-invoked) and reads the journal it left behind.
func TestJournalExecuteEndToEnd(t *testing.T) {
	if scenario := os.Getenv("RUSH_JOURNAL_CHILD"); scenario != "" {
		os.Args = []string{"rush", "logs", "path"}
		if scenario == "badflag" {
			os.Args = []string{"rush", "--definitely-no-such-flag"}
		}
		Execute()
		return
	}
	for _, tc := range []struct {
		scenario string
		exit     int
	}{{"ok", 0}, {"badflag", 1}} {
		journalTestEnv(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalExecuteEndToEnd$")
		child.Env = append(os.Environ(), "RUSH_JOURNAL_CHILD="+tc.scenario)
		out, err := child.CombinedOutput()
		cancel()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			require.ErrorAs(t, err, &ee, string(out))
			code = ee.ExitCode()
		}
		require.Equal(t, tc.exit, code, "%s: %s", tc.scenario, out)
		recs := journalRecords(t)
		require.Len(t, recs, 2, "%s: %v", tc.scenario, recs)
		require.Equal(t, "command_end", recs[1]["kind"])
		require.Equal(t, float64(tc.exit), recs[1]["exit"])
		if tc.scenario == "ok" {
			require.Equal(t, "command_start", recs[0]["kind"])
			require.Equal(t, "rush logs path", recs[0]["cmd"])
		} else {
			require.Equal(t, "usage_error", recs[0]["kind"])
			require.Equal(t, "flag parse error", recs[1]["err"])
			require.NotContains(t, journalReadRaw(t), "definitely-no-such-flag")
		}
	}
}
