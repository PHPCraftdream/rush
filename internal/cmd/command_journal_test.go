// Command-journal tests. REVERT-CHECKS (the orchestrator runs the mutants;
// do not run them here) — the single-line production change each test is
// meant to catch:
//
//	TestJournalSuccessCommand: removing journalCommandStart from the
//	    rootCmd.PersistentPreRunE chain in root.go init().
//	TestJournalFailedCommand: dropping the end-record write from
//	    finalizeCommandJournal or ignoring runErr's exit code.
//	TestJournalWrongPasswordJournalled: reordering the pre-run chain so
//	    settingsPasswordPreRun runs before journalCommandStart, or putting
//	    the password flag's value into journalFlagsRecord.
//	TestJournalNonDispatchPaths: removing the fallback classification in
//	    finalizeCommandJournal, or substituting cobra's raw error text for
//	    the fixed err strings.
//	TestExitWithAudit: removing the end-record write or the endWritten
//	    once-guard from exitWithAudit.
//	TestJournalPanicEnd: removing journalEndForPanic from
//	    recoverAndLogPanic.
//	TestJournalCompletionSilent: removing the completion/__complete skip
//	    in journalCommandSuppressed.
//	TestJournalCanary: widening journalFlagValueAllowlist or
//	    journalPositionalAllowlist with a free-text flag or command.
//	TestJournalAllowlistDenyListIntersection: adding a deny-listed flag
//	    name to journalFlagValueAllowlist.
//	TestJournalRecordCapsAndWriteErrors: removing the swallow around
//	    audit.Write or the truncation caps.
package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// journalTestEnv isolates the global config/data dirs, points the audit
// resolver at them and resets the per-process journal state.
func journalTestEnv(t *testing.T) string {
	t.Helper()
	// Pre-pin rushlog to no file (io.Discard writer) so no test's temp
	// data dir holds the process-lifetime rush.log handle at cleanup.
	rushlog.Setup("", false)
	dataDir := seedAuditDir(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	ensureAuditDirFunc()
	resetCommandJournalForTests()
	t.Cleanup(func() { audit.SetDirFunc(journalAuditDir) })
	return dataDir
}

// journalRecords returns every journalled record as decoded JSON maps.
func journalRecords(t *testing.T) []map[string]any {
	t.Helper()
	paths, err := audit.Files()
	require.NoError(t, err)
	var recs []map[string]any
	for _, p := range paths {
		bts, err := os.ReadFile(p)
		require.NoError(t, err)
		for _, line := range strings.Split(string(bts), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			recs = append(recs, rec)
		}
	}
	return recs
}

// journalReadRaw returns the raw journal bytes for leak assertions.
func journalReadRaw(t *testing.T) string {
	t.Helper()
	paths, err := audit.Files()
	require.NoError(t, err)
	var sb strings.Builder
	for _, p := range paths {
		bts, err := os.ReadFile(p)
		require.NoError(t, err)
		sb.Write(bts)
	}
	return sb.String()
}

func TestJournalSuccessCommand(t *testing.T) {
	journalTestEnv(t)
	_, err := runRushTree(t, "logs", "path")
	require.NoError(t, err)
	finalizeCommandJournal([]string{"logs", "path"}, nil)

	recs := journalRecords(t)
	require.Len(t, recs, 2)
	start, end := recs[0], recs[1]
	require.Equal(t, "command_start", start["kind"])
	require.Equal(t, "rush logs path", start["cmd"])
	for _, auto := range []string{"ts", "pid", "ppid", "parent", "launch_cwd"} {
		require.Contains(t, start, auto, "auto field %s must be present", auto)
	}
	_, hasArgs := start["args"]
	require.False(t, hasArgs, "logs path takes no journalled positionals")
	require.Equal(t, "command_end", end["kind"])
	require.Equal(t, "rush logs path", end["cmd"])
	require.Equal(t, float64(0), end["exit"])
	require.GreaterOrEqual(t, end["dur_ms"], float64(0))
	require.NotContains(t, end, "err")
}

func TestJournalFailedCommand(t *testing.T) {
	journalTestEnv(t)
	_, runErr := runRushTree(t, "lock", "")
	require.Error(t, runErr)
	finalizeCommandJournal([]string{"lock", ""}, runErr)

	recs := journalRecords(t)
	require.Len(t, recs, 2)
	require.Equal(t, "command_start", recs[0]["kind"])
	end := recs[1]
	require.Equal(t, "command_end", end["kind"])
	require.Equal(t, float64(1), end["exit"])
	require.Equal(t, "an argument must not be empty", end["err"])
	require.LessOrEqual(t, utf8.RuneCountInString(end["err"].(string)), journalErrMaxRunes)
}

func TestJournalWrongPasswordJournalled(t *testing.T) {
	journalTestEnv(t)
	t.Cleanup(func() {
		if f := rootCmd.PersistentFlags().Lookup("password"); f != nil {
			_ = f.Value.Set("")
			f.Changed = false
		}
	})
	workspace, dataDir := t.TempDir(), t.TempDir()
	stdout, stderr, lockErr := runLockTree(t, workspace, dataDir, "lock", "right-pw-value")
	require.NoError(t, lockErr, "stdout=%s stderr=%s", stdout, stderr)

	resetCommandJournalForTests()
	_, runErr := runRushTree(t, "models", "use", "glm4_6", "glm5_turbo", "--password", "WRONG-pw-value")
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	finalizeCommandJournal(os.Args[1:], runErr)

	var start, denied, end map[string]any
	for _, rec := range journalRecords(t) {
		if rec["cmd"] == "rush lock" {
			_, hasArgs := rec["args"]
			require.False(t, hasArgs, "the lock password positional must never be journalled")
			continue
		}
		if rec["cmd"] != "rush models use" {
			continue
		}
		switch rec["kind"] {
		case "command_start":
			start = rec
		case "command_denied":
			denied = rec
		case "command_end":
			end = rec
		}
	}
	require.NotNil(t, start, "start must be written before the password check")
	require.NotNil(t, denied, "the existing command_denied event must stay")
	require.NotNil(t, end)
	require.Equal(t, float64(1), end["exit"])
	require.Contains(t, end["err"], "wrong password")
	flagsMap, ok := start["flags"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, flagsMap, "password")
	require.Nil(t, flagsMap["password"], "the password value must never be journalled")
	require.Equal(t, []any{"glm4_6", "glm5_turbo"}, start["args"])
	raw := journalReadRaw(t)
	require.NotContains(t, raw, "WRONG-pw-value")
	require.NotContains(t, raw, "right-pw-value")
}

func TestJournalNonDispatchPaths(t *testing.T) {
	journalTestEnv(t)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	oldVersion := rootCmd.Version
	rootCmd.Version = "test-version"
	t.Cleanup(func() { rootCmd.Version = oldVersion })

	steps := []struct {
		args     []string
		wantKind string
		wantExit float64
		wantErr  string
	}{
		{args: []string{"--help"}, wantKind: "help", wantExit: 0},
		{args: []string{"--version"}, wantKind: "version", wantExit: 0},
		{args: []string{"definitely-unknown-cmd"}, wantKind: "usage_error", wantExit: 1, wantErr: "unknown command"},
		{args: []string{"--definitely-no-such-flag"}, wantKind: "usage_error", wantExit: 1, wantErr: "flag parse error"},
	}
	for _, s := range steps {
		resetCommandJournalForTests()
		rootCmd.SetArgs(s.args)
		runErr := rootCmd.Execute()
		rootCmd.SetArgs(nil)
		require.Equal(t, s.wantErr == "", runErr == nil, "args %v: unexpected err %v", s.args, runErr)
		finalizeCommandJournal(s.args, runErr)

		recs := journalRecords(t)
		require.GreaterOrEqual(t, len(recs), 2, "args %v", s.args)
		start, end := recs[len(recs)-2], recs[len(recs)-1]
		require.Equal(t, s.wantKind, start["kind"], "args %v", s.args)
		require.Equal(t, "command_end", end["kind"], "args %v", s.args)
		require.Equal(t, s.wantExit, end["exit"], "args %v", s.args)
		_, hasFlags := start["flags"]
		_, hasArgs := start["args"]
		require.False(t, hasFlags, "fallback records must not carry flags")
		require.False(t, hasArgs, "fallback records must not carry args")
		if s.wantErr == "" {
			require.NotContains(t, end, "err")
		} else {
			require.Equal(t, s.wantErr, end["err"], "args %v", s.args)
		}
	}
	raw := journalReadRaw(t)
	require.NotContains(t, raw, "definitely-unknown-cmd")
	require.NotContains(t, raw, "definitely-no-such-flag")
}

func TestExitWithAudit(t *testing.T) {
	journalTestEnv(t)
	recorder := newExitRecorder()
	oldExit := exitFunc
	exitFunc = recorder.exit
	t.Cleanup(func() { exitFunc = oldExit })

	journalCommandStart(logsPathCmd, nil)
	exitWithAudit(7)
	awaitClosed(t, recorder.first, "first exitWithAudit")
	exitWithAudit(9)
	require.Equal(t, []int{7, 9}, recorder.calls())

	recs := journalRecords(t)
	require.Len(t, recs, 2, "two exits must write exactly one end record")
	require.Equal(t, "command_start", recs[0]["kind"])
	require.Equal(t, "command_end", recs[1]["kind"])
	require.Equal(t, float64(7), recs[1]["exit"])
}

func TestJournalPanicEnd(t *testing.T) {
	journalTestEnv(t)
	journalCommandStart(logsPathCmd, nil)
	func() {
		defer func() { require.Equal(t, "boom-value", recover()) }()
		defer recoverAndLogPanic()
		panic("boom-value")
	}()

	recs := journalRecords(t)
	require.Len(t, recs, 2)
	end := recs[1]
	require.Equal(t, "command_end", end["kind"])
	require.Equal(t, float64(2), end["exit"])
	require.Equal(t, "panic", end["err"])
}

func TestJournalCompletionSilent(t *testing.T) {
	journalTestEnv(t)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	for _, args := range [][]string{
		{"completion", "bash"},
		{"completion"},
		{"__complete", "run", ""},
		{"__completeNoDesc", "run", ""},
	} {
		resetCommandJournalForTests()
		rootCmd.SetArgs(args)
		_ = rootCmd.Execute()
		rootCmd.SetArgs(nil)
		finalizeCommandJournal(args, nil)
	}
	require.Empty(t, journalRecords(t), "completion invocations must write nothing")
}

// journalSnapshotRestore snapshots value+Changed of every flag in the sets
// and returns a restore func.
func journalSnapshotRestore(t *testing.T, sets ...*pflag.FlagSet) func() {
	t.Helper()
	type savedFlag struct {
		f       *pflag.Flag
		value   string
		changed bool
	}
	var saved []savedFlag
	for _, fs := range sets {
		fs.VisitAll(func(f *pflag.Flag) {
			saved = append(saved, savedFlag{f: f, value: f.Value.String(), changed: f.Changed})
		})
	}
	return func() {
		for _, s := range saved {
			_ = s.f.Value.Set(s.value)
			s.f.Changed = s.changed
		}
	}
}

// journalCanaryValue returns a parseable per-flag probe value; string-typed
// flags carry the CANARY marker.
func journalCanaryValue(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "bool":
		return "true"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		return "7"
	case "float32", "float64":
		return "1.5"
	case "duration":
		return "1s"
	default:
		return "CANARY-" + f.Name
	}
}

// journalCollectCanaries collects every CANARY- string found at any depth.
func journalCollectCanaries(v any, out map[string]bool) {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "CANARY-") {
			out[t] = true
		}
	case map[string]any:
		for _, vv := range t {
			journalCollectCanaries(vv, out)
		}
	case []any:
		for _, vv := range t {
			journalCollectCanaries(vv, out)
		}
	}
}

// journalCanaryOneCommand sets every flag of cmd to its canary value (via
// real ParseFlags, since pflag's Visit only sees flags populated by parsing)
// and runs ONLY the journal start path; the start record's canary strings must
// be exactly the allowlist's for this command.
func journalCanaryOneCommand(t *testing.T, c *cobra.Command) (expected, got map[string]bool) {
	t.Helper()
	path := c.CommandPath()
	resetCommandJournalForTests()
	restore := journalSnapshotRestore(t, c.Flags(), rootCmd.PersistentFlags())
	defer restore()

	expected = map[string]bool{}
	if c.DisableFlagParsing {
		journalCommandStart(c, nil)
	} else {
		require.NoError(t, c.ParseFlags([]string{}))
		// Stale Changed from earlier tests sharing these flag objects
		// would keep pflag's Set from re-adding flags to f.actual.
		c.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		invocation := []string{}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			probe := journalCanaryValue(f)
			if err := f.Value.Set(probe); err != nil {
				return // exotic unparseable type: exclude from the invocation
			}
			invocation = append(invocation, "--"+f.Name+"="+probe)
			if f.Value.Type() == "string" && journalOracleFlagAllowed(f.Name, path) {
				expected["CANARY-"+f.Name] = true
			}
		})
		invocation = append(invocation, "CANARY-POS")
		require.NoError(t, c.ParseFlags(invocation))
		journalCommandStart(c, c.Flags().Args())
	}
	if journalOraclePositional[path] {
		expected["CANARY-POS"] = true
	}

	recs := journalRecords(t)
	require.NotEmpty(t, recs, "no start record written for %s", path)
	got = map[string]bool{}
	journalCollectCanaries(recs[len(recs)-1], got)
	require.Equal(t, expected, got, "canary mismatch for %s", path)

	if !c.DisableFlagParsing {
		flagsMap, ok := recs[len(recs)-1]["flags"].(map[string]any)
		require.True(t, ok, "start record for %s must carry a flags object", path)
		set := 0
		c.Flags().Visit(func(*pflag.Flag) { set++ })
		require.Equal(t, set, len(flagsMap), "every set flag must be listed for %s", path)
	}
	return expected, got
}

func TestJournalCanary(t *testing.T) {
	journalTestEnv(t)
	mergedExpected := map[string]bool{}
	mergedGot := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !journalCommandSuppressed(c, c.CommandPath()) && c.Runnable() {
			expected, got := journalCanaryOneCommand(t, c)
			for k := range expected {
				mergedExpected[k] = true
			}
			for k := range got {
				mergedGot[k] = true
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	require.Equal(t, mergedExpected, mergedGot,
		"the canary set that reached the journal must be exactly the allowlist set")
}

func TestJournalAllowlistDenyListIntersection(t *testing.T) {
	deny := map[string]bool{
		"password": true, "api-key": true, "system-prompt": true,
		"system-prompt-file": true, "on-finish": true, "message": true,
		"prompt": true, "env": true, "header": true, "url": true,
		"arg": true, "command": true, "base-url": true, "json-body": true,
	}
	for name := range journalFlagValueAllowlist {
		require.False(t, deny[name], "allowlisted flag %q is in the hard deny list", name)
	}
	require.False(t, journalFlagValueAllowlist["password"])
	require.False(t, journalFlagValueAllowlist["json"],
		"json must stay out of the typed allowlist: providers patch --json carries a literal body")
}

func TestJournalRecordCapsAndWriteErrors(t *testing.T) {
	dir := journalTestEnv(t)

	// Oversized allowlisted values are capped by the audit layer.
	big := strings.Repeat("S", 5000)
	require.NoError(t, systemPromptCmd.Flags().Set("session", big))
	t.Cleanup(func() {
		_ = systemPromptCmd.Flags().Set("session", "")
		if f := systemPromptCmd.Flags().Lookup("session"); f != nil {
			f.Changed = false
		}
	})
	journalCommandStart(systemPromptCmd, nil)
	recs := journalRecords(t)
	require.NotEmpty(t, recs)
	sessionValue, ok := recs[len(recs)-1]["flags"].(map[string]any)["session"].(string)
	require.True(t, ok)
	require.LessOrEqual(t, utf8.RuneCountInString(sessionValue), 300)

	// Every record fits the audit byte cap.
	paths, err := audit.Files()
	require.NoError(t, err)
	for _, p := range paths {
		bts, err := os.ReadFile(p)
		require.NoError(t, err)
		for _, line := range strings.Split(string(bts), "\n") {
			require.LessOrEqual(t, len(line), 3500, "record exceeds the audit byte cap")
		}
	}

	// Positionals are capped at 120 runes.
	resetCommandJournalForTests()
	journalCommandStart(sessionsShowCmd, []string{strings.Repeat("P", 400)})
	recs = journalRecords(t)
	args, ok := recs[len(recs)-1]["args"].([]any)
	require.True(t, ok)
	require.Len(t, args, 1)
	require.LessOrEqual(t, utf8.RuneCountInString(args[0].(string)), journalPosMaxRunes)

	// Write failures are swallowed: unwritable dir, command still exits.
	before := len(journalRecords(t))
	blockedParent := filepath.Join(dir, "blocked-parent")
	require.NoError(t, os.WriteFile(blockedParent, []byte("file"), 0o600))
	recorder := newExitRecorder()
	oldExit := exitFunc
	exitFunc = recorder.exit
	t.Cleanup(func() {
		exitFunc = oldExit
		audit.SetDirFunc(journalAuditDir)
	})
	resetCommandJournalForTests()
	require.NotPanics(t, func() {
		audit.SetDirFunc(func() string { return filepath.Join(blockedParent, "sub") })
		journalCommandStart(logsPathCmd, nil)
		finalizeCommandJournal(nil, nil)
		exitWithAudit(3)
	})
	awaitClosed(t, recorder.first, "exitWithAudit under an unwritable audit dir")
	require.Equal(t, []int{3}, recorder.calls())
	audit.SetDirFunc(journalAuditDir)
	require.Len(t, journalRecords(t), before, "a failed audit write must leave no record")
}
