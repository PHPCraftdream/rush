// Command-journal fallback and oracle tests. REVERT-CHECKS (single production
// change each test must catch):
//
// TestJournalFallbackWritesWithoutPriorResolver, TestJournalErrTextFirst-
// LineAndTruncation and the allowlist-oracle test moved into cli/cmdkit/journal
// (package clijournal) with the implementation in lane K1.
//
//	TestJournalExecuteEndToEnd: removing finalizeCommandJournal or the
//	    exitWithAudit(1) call from Execute() in root.go.
//	TestJournalAllowlistsMatchOracle / TestJournalCanary: adding any entry to
//	    journalFlagValueAllowlist or journalPositionalAllowlist (or to the
//	    format special case) without editing the literals below.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/audit"
	"github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

func TestJournalGroupUsageErrorNotHelp(t *testing.T) {
	root := NewRootCmd()
	var group *cobra.Command
	for _, c := range root.Commands() {
		if !c.Runnable() && c.HasSubCommands() && !journal.JournalCommandSuppressed(c, c.CommandPath()) {
			group = c
			break
		}
	}
	require.NotNil(t, group, "no non-runnable command group in the tree")
	path, kind, fixedErr, suppressed := journal.ClassifyJournalFallback(
		[]string{group.Name(), "--definitely-no-such-flag"}, errors.New("unknown flag: --definitely-no-such-flag"))
	require.False(t, suppressed)
	require.Equal(t, group.CommandPath(), path)
	require.Equal(t, "usage_error", kind)
	require.Equal(t, "flag parse error", fixedErr)

	_, kind, fixedErr, _ = journal.ClassifyJournalFallback([]string{group.Name()}, nil)
	require.Equal(t, "help", kind)
	require.Empty(t, fixedErr)
}

func journalCLIEnv(t *testing.T) string {
	t.Helper()
	log.Setup("", false)
	dataDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	journal.EnsureAuditDirFunc()
	journal.ResetCommandJournalForTests()
	t.Cleanup(func() { audit.SetDirFunc(journal.JournalAuditDir) })
	return dataDir
}

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
		journalCLIEnv(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		child := platform.Command(ctx, os.Args[0], "-test.run=^TestJournalExecuteEndToEnd$")
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

// Lock/unlock journal and password-consistency tests exercise the true CLI
// root tree after the command moved to admin/settings.

func journalTestEnv(t *testing.T) string {
	t.Helper()
	log.Setup("", false)
	dataDir := cmdtest.SeedAuditDir(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	journal.EnsureAuditDirFunc()
	journal.ResetCommandJournalForTests()
	t.Cleanup(func() { audit.SetDirFunc(journal.JournalAuditDir) })
	return dataDir
}

func lockJournalRecords(t *testing.T) []map[string]any {
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

func lockJournalReadRaw(t *testing.T) string {
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

func isolateLockEnv(t *testing.T) (string, string, string) {
	t.Helper()
	global := t.TempDir()
	globalConfig := t.TempDir()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", global)
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	require.NoError(t, os.WriteFile(filepath.Join(global, "rush.json"), []byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o600))
	config.ResetProviderCacheForTests()
	log.Setup("", false)
	return global, workspace, dataDir
}

func runLockTree(t *testing.T, workspace, dataDir string, args ...string) (stdout, stderr string, runErr error) {
	t.Helper()
	cmdtest.RestoreWorkingDir(t)
	root := NewRootCmd()
	args = append([]string(nil), args...)
	for i, arg := range args {
		if arg == "lock" {
			args = append(args[:i+1], append([]string{"--yes"}, args[i+1:]...)...)
			break
		}
	}
	root.SetArgs(append([]string{"--cwd", workspace, "--data-dir", dataDir}, args...))
	stdout, stderr = cmdtest.CaptureStdoutAndStderr(t, func() {
		runErr = root.ExecuteContext(t.Context())
	})
	return
}

func TestLockJournal_LocalWrongPasswordJournaled(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "local-right")
	require.NoError(t, err)
	journal.ResetCommandJournalForTests()
	_, _, runErr := runLockTree(t, workspace, dataDir, "--password", "local-wrong", "lock", "--local", "replacement")
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	journal.FinalizeCommandJournal(os.Args[1:], runErr)
	var denied, end map[string]any
	for _, rec := range lockJournalRecords(t) {
		if rec["cmd"] != "rush lock" {
			continue
		}
		switch rec["kind"] {
		case "command_denied":
			denied = rec
		case "command_end":
			if code, ok := rec["exit"].(float64); ok && code == 1 {
				end = rec
			}
		}
	}
	require.NotNil(t, denied, "the local-lock refusal must carry the command_denied event too")
	require.NotNil(t, end, "the refused invocation must be journalled with a non-zero exit")
	require.Contains(t, end["err"], "wrong password")
	require.NotContains(t, end["err"], "local-wrong")
	raw := lockJournalReadRaw(t)
	require.NotContains(t, raw, "local-wrong")
	require.NotContains(t, raw, "local-right")
}

func TestUnlock_HintsAtLocalLockWhenGlobalUnlocked(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "localpw")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "localpw")
	require.Error(t, err)
	require.Contains(t, err.Error(), "only the workspace is locked")
	require.Contains(t, err.Error(), "--local")
	data, readErr := os.ReadFile(filepath.Join(dataDir, "rush.json"))
	require.NoError(t, readErr)
	require.True(t, config.VerifyPassword("localpw", gjson.GetBytes(data, "sync_rev").String()), "the refused unlock must not change the lock")
}

func TestUnlock_EmptyArgumentRefused(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "unlock", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "an argument must not be empty")
}

func TestUnlock_GlobalFlagAndOutputText(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	out, _, err := runLockTree(t, workspace, dataDir, "lock", "--global", "gpw")
	require.NoError(t, err)
	require.Contains(t, out, "settings locked (global)")
	out, _, err = runLockTree(t, workspace, dataDir, "unlock", "--global", "gpw")
	require.NoError(t, err)
	require.Contains(t, out, "settings unlocked (global)")
	_, _, err = runLockTree(t, workspace, dataDir, "lock", "--local", "lpw")
	require.NoError(t, err)
	out, _, err = runLockTree(t, workspace, dataDir, "unlock", "--local", "lpw")
	require.NoError(t, err)
	require.Contains(t, out, "settings unlocked (local)")
}

func TestUnlock_WrongPasswordJournaled(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "right-pw-value")
	require.NoError(t, err)
	journal.ResetCommandJournalForTests()
	_, _, runErr := runLockTree(t, workspace, dataDir, "unlock", "wrong-pw-value")
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	journal.FinalizeCommandJournal(os.Args[1:], runErr)
	var end map[string]any
	for _, rec := range lockJournalRecords(t) {
		if rec["cmd"] == "rush unlock" && rec["kind"] == "command_end" {
			if code, ok := rec["exit"].(float64); ok && code == 1 {
				end = rec
			}
		}
	}
	require.NotNil(t, end)
	require.Contains(t, end["err"], "wrong password")
	raw := lockJournalReadRaw(t)
	require.NotContains(t, raw, "wrong-pw-value")
	require.NotContains(t, raw, "right-pw-value")
}

func TestPersistentPreRunOnlyOnRoot(t *testing.T) {
	root := NewRootCmd()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root {
			require.Nil(t, c.PersistentPreRun, "%s sets PersistentPreRun", c.CommandPath())
			require.Nil(t, c.PersistentPreRunE, "%s sets PersistentPreRunE", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	require.NotNil(t, root.PersistentPreRunE, "root must keep the journal/password pre-run")
	require.Nil(t, root.PersistentPreRun, "root must use the E variant only")
}

func TestPassword_SetupAppCheckAppPasswordPath(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	journal.EnsureAuditDirFunc()
	store, err := config.Init(workspace, dataDir, false)
	require.NoError(t, err)
	require.NoError(t, store.LockSettings(config.ScopeWorkspace, "right"))
	probe := &cobra.Command{Use: "probe"}
	probe.Flags().String("password", "", "")
	require.NoError(t, probe.Flags().Set("password", "wrong"))
	config.SetProcessPassword("wrong")
	t.Cleanup(func() { config.SetProcessPassword("") })
	err = cmdkit.CheckAppPassword(probe, store)
	require.ErrorIs(t, err, config.ErrWrongPassword)
	require.True(t, hasAuditKind(t, "command_denied"), "the refusal must be audited")
	require.NoError(t, probe.Flags().Set("password", ""))
	require.NoError(t, cmdkit.CheckAppPassword(probe, store), "no --password: the setup check is skipped")
}

func hasAuditKind(t *testing.T, kind string) bool {
	t.Helper()
	for _, rec := range lockJournalRecords(t) {
		if rec["kind"] == kind {
			return true
		}
	}
	return false
}

func TestPassword_SetupAppRefusesWrongLocalPassword(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "local-right")
	require.NoError(t, err)
	_, _, runErr := runLockTree(t, workspace, dataDir, "--password", "local-wrong", "sessions", "list")
	require.ErrorIs(t, runErr, config.ErrWrongPassword, "a full-app command must refuse a wrong --password against a workspace lock")
}

const lockKtavFixtureJSON = `{
  "providers": {
    "ktav-p": {
      "name": "Ktav P",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}]
    }
  }
}`

// TestLock_KtavSlotFileNameInOutput pins the user-visible settings-file
// spelling in `rush lock` output to the ACTIVE file of the slot, derived at
// runtime via config.ActiveConfigFileName: "rush.json" when no Ktav sibling
// exists (byte-identical with today's gate-off default) and "rush.ktav"
// when the Ktav sibling exists on disk.
func TestLock_KtavSlotFileNameInOutput(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	defer restore()
	t.Run("gate_off_no_ktav_names_rush_json", func(t *testing.T) {
		ktavIsolateGlobalEnv(t)
		_, workspace, dataDir := isolateLockEnv(t)
		out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
		require.NoError(t, err)
		require.Contains(t, out, filepath.Join(dataDir, "rush.json"))
		require.NotContains(t, out, "rush.ktav")
	})
	t.Run("ktav_sibling_selected_names_rush_ktav", func(t *testing.T) {
		if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Run("gate_off_fallback", func(t *testing.T) {
				_, workspace, dataDir := isolateLockEnv(t)
				out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
				require.NoError(t, err)
				require.Contains(t, out, "rush.json")
			})
			return
		}
		ktavIsolateGlobalEnv(t)
		_, workspace, dataDir := isolateLockEnv(t)
		ktavDoc, err := ktavcodec.ConvertJSONToKtav([]byte(lockKtavFixtureJSON))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"), ktavDoc, 0o600))
		evalDataDir, err := filepath.EvalSymlinks(dataDir)
		require.NoError(t, err)
		out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
		require.NoError(t, err)
		require.Contains(t, out, "rush.ktav")
		require.NotContains(t, out, "rush.json")
		require.True(t, strings.Contains(out, filepath.Join(dataDir, "rush.ktav")) || strings.Contains(out, filepath.Join(evalDataDir, "rush.ktav")), "expected the printed active slot file %s (or %s) in output:\n%s", filepath.Join(dataDir, "rush.ktav"), filepath.Join(evalDataDir, "rush.ktav"), out)
	})
}

func ktavIsolateGlobalEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for _, name := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME", "USERPROFILE", "LOCALAPPDATA"} {
		t.Setenv(name, filepath.Join(resolved, name))
	}
}

func TestLockUnlock_GlobalLocalAndRelock(t *testing.T) {
	global, workspace, dataDir := isolateLockEnv(t)
	path := filepath.Join(global, "rush.json")
	out, stderr, err := runLockTree(t, workspace, dataDir, "lock", "correct")
	require.NoError(t, err)
	require.Empty(t, stderr)
	require.Equal(t, "settings locked (global) — "+path+"\nunlock with: rush unlock [--local] <password>   (or pass --password <password> to a command to change settings)\n", out)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "wrong")
	require.ErrorIs(t, err, config.ErrWrongPassword)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, _, err = runLockTree(t, workspace, dataDir, "lock", "replacement")
	require.ErrorIs(t, err, config.ErrSettingsLocked)
	_, _, err = runLockTree(t, workspace, dataDir, "--password", "correct", "lock", "replacement")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "correct")
	require.ErrorIs(t, err, config.ErrWrongPassword)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "replacement")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "replacement")
	require.ErrorIs(t, err, config.ErrNotLocked)
	localOut, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "localpw")
	require.NoError(t, err)
	localPath := filepath.Join(dataDir, "rush.json")
	_, statErr := os.Stat(localPath)
	require.NoError(t, statErr)
	require.Equal(t, "settings locked (local) — "+localPath+"\nunlock with: rush unlock [--local] <password>   (or pass --password <password> to a command to change settings)\n", localOut)
	_, _, err = runLockTree(t, workspace, dataDir, "lock", "--local", "another")
	require.ErrorIs(t, err, config.ErrSettingsLocked)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "--local", "localpw")
	require.NoError(t, err)
}

func TestPassword_PreRunDeniesWrongGlobalSettingsPassword(t *testing.T) {
	global, workspace, dataDir := isolateLockEnv(t)
	path := filepath.Join(global, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sync_rev":"`+config.HashPassword("right")+`"}`), 0o600))
	bodyRan := false
	root := NewRootCmd()
	probe := &cobra.Command{Use: "probe", Run: func(*cobra.Command, []string) { bodyRan = true }}
	root.AddCommand(probe)
	defer root.RemoveCommand(probe)
	root.SetArgs([]string{"--cwd", workspace, "--data-dir", dataDir, "--password", "wrong", "probe"})
	var runErr error
	_, stderr := cmdtest.CaptureStdoutAndStderr(t, func() {
		runErr = root.ExecuteContext(t.Context())
	})
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	require.False(t, bodyRan)
	require.Contains(t, stderr, config.ErrWrongPassword.Error())
}

func TestPassword_FlagSetsProcessPasswordAndAuthorizesModelsUse(t *testing.T) {
	global, workspace, dataDir := isolateLockEnv(t)
	globalPath := filepath.Join(global, "rush.json")
	config.SetProcessPassword("")
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"sync_rev":"`+config.HashPassword("models-pass")+`","providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o600))
	_, _, err := runLockTree(t, workspace, dataDir, "--password", "models-pass", "models", "use", "glm4_6", "glm5_turbo")
	require.NoError(t, err)
}

func TestPassword_SetupAppLiteRejectsWrongLocalPasswordBeforeWrite(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "local-right")
	require.NoError(t, err)
	_, stderr, err := runLockTree(t, workspace, dataDir, "--password", "local-wrong", "lock", "--local", "replacement")
	require.ErrorIs(t, err, config.ErrWrongPassword)
	require.Contains(t, stderr, config.ErrWrongPassword.Error())
	data, err := os.ReadFile(filepath.Join(dataDir, "rush.json"))
	require.NoError(t, err)
	stored := gjson.GetBytes(data, "sync_rev").String()
	require.True(t, config.VerifyPassword("local-right", stored), "the lock must still verify with the original password")
	require.False(t, config.VerifyPassword("replacement", stored), "the lock must not accept the refused replacement password")
}

func TestSettingsLock_ScopePriorityAndModelsUseRefusal(t *testing.T) {
	global, workspaceA, dataA := isolateLockEnv(t)
	globalPath := filepath.Join(global, "rush.json")
	workspaceB, dataB := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceA, ".rush"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceB, ".rush"), 0o700))
	_, _, err := runLockTree(t, workspaceA, dataA, "lock", "--local", "local-pass")
	require.NoError(t, err)
	globalBefore, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	_, stderr, err := runLockTree(t, workspaceA, dataA, "models", "use", "glm4_6", "glm5_turbo")
	assertRefusal(t, stderr, err, config.ErrSettingsLocked)
	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Equal(t, globalBefore, after)
	_, stderr, err = runLockTree(t, workspaceA, dataA, "models", "use", "--global", "glm4_6", "glm5_turbo")
	assertRefusal(t, stderr, err, config.ErrSettingsLocked)
	_, _, err = runLockTree(t, workspaceB, dataB, "models", "use", "glm4_6", "glm5_turbo")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspaceB, dataB, "lock", "global-pass")
	require.NoError(t, err)
	_, stderr, err = runLockTree(t, workspaceA, dataA, "--password", "local-pass", "models", "use", "glm4_6", "glm5_turbo")
	assertRefusal(t, stderr, err, config.ErrWrongPassword)
	_, _, err = runLockTree(t, workspaceA, dataA, "--password", "global-pass", "models", "use", "glm4_6", "glm5_turbo")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspaceA, dataA, "unlock", "global-pass")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspaceA, dataA, "unlock", "--local", "local-pass")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspaceA, dataA, "models", "use", "glm4_6", "glm5_turbo")
	require.NoError(t, err)
}

func assertRefusal(t *testing.T, stderr string, err error, want error) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, want)
	switch want {
	case config.ErrSettingsLocked:
		require.EqualError(t, err, "settings change is forbidden by the user (settings are locked). Do not try to change models or any other settings and do not look for a way around this; ask the user instead")
	case config.ErrWrongPassword:
		require.EqualError(t, err, "wrong password: settings change is forbidden by the user. Do not guess or retry; ask the user instead")
	default:
		t.Fatalf("unexpected refusal sentinel: %v", want)
	}
	for _, secret := range []string{"secret", "local-right", config.HashPassword("secret"), config.HashPassword("local-right")} {
		require.NotContains(t, err.Error()+stderr, secret)
	}
	message := strings.ToLower(strings.ReplaceAll(err.Error()+stderr, config.ErrWrongPassword.Error(), ""))
	for _, forbidden := range []string{"unlock", "--password", "sha256", "hash", "password"} {
		require.NotContains(t, message, forbidden)
	}
}

func TestDirectConfigCommandsHonorLocalLock(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	localPath := filepath.Join(dataDir, "rush.json")
	const localPassword = "local-right"
	require.NoError(t, os.WriteFile(localPath, []byte(`{"sync_rev":"`+config.HashPassword(localPassword)+`","providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o600))
	before, err := os.ReadFile(localPath)
	require.NoError(t, err)
	const wrongPassword = "bad-explicit-secret"
	for _, command := range []string{"models", "stats"} {
		t.Run(command+"_wrong_password", func(t *testing.T) {
			args := []string{"--password", wrongPassword, command}
			if command == "models" {
				args = append(args, "zai")
			}
			stdout, stderr, runErr := runLockTree(t, workspace, dataDir, args...)
			assertRefusal(t, stderr+stdout, runErr, config.ErrWrongPassword)
			require.Empty(t, stdout)
			for _, secret := range []string{wrongPassword, localPassword, config.HashPassword(localPassword)} {
				require.NotContains(t, runErr.Error()+stderr, secret)
			}
			after, readErr := os.ReadFile(localPath)
			require.NoError(t, readErr)
			require.Equal(t, before, after)
			if command == "stats" {
				for _, name := range []string{storepath.ActiveFilename, storepath.LegacyFilename} {
					_, statErr := os.Stat(filepath.Join(dataDir, name))
					require.Truef(t, os.IsNotExist(statErr), "refused stats run must not create %s", name)
				}
			}
		})
	}
	for _, tc := range []struct {
		name         string
		passwordArgs []string
	}{{"models_without_password", nil}, {"models_with_password", []string{"--password", localPassword}}} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string(nil), tc.passwordArgs...)
			args = append(args, "models", "zai")
			stdout, _, runErr := runLockTree(t, workspace, dataDir, args...)
			require.NoError(t, runErr)
			require.NotEmpty(t, stdout)
		})
	}
}

func TestLockUnlock_ArgumentsAndNoLockPassword(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	for _, args := range [][]string{{"lock"}, {"lock", ""}, {"unlock"}, {"unlock", "x", "y"}} {
		_, _, err := runLockTree(t, workspace, dataDir, args...)
		require.Error(t, err)
	}
	_, _, err := runLockTree(t, workspace, dataDir, "--password", "unused", "lock", "fresh")
	require.NoError(t, err)
}

func TestSettingsAuditRecordsAndWebSocketMapping(t *testing.T) {
	global, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "secret")
	require.NoError(t, err)
	path := filepath.Join(global, "audit-"+time.Now().Format("2006-01-02")+".jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	_, _, err = runLockTree(t, workspace, dataDir, "models", "use", "glm4_6", "glm5_turbo")
	require.ErrorIs(t, err, config.ErrSettingsLocked)
	_, _, err = runLockTree(t, workspace, dataDir, "--password", "secret", "models", "use", "glm4_6", "glm5_turbo")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "--password", "wrong", "models", "use", "glm4_6", "glm5_turbo")
	require.ErrorIs(t, err, config.ErrWrongPassword)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	kinds, outcomes := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		kinds[record["kind"].(string)] = true
		if outcome, ok := record["outcome"].(string); ok {
			outcomes[outcome] = true
		}
		require.NotEmpty(t, record["launch_cwd"])
		lineWithoutOutcome := strings.ReplaceAll(strings.ToLower(stripAuditPaths(line)), `"outcome":"blocked"`, "")
		for _, secret := range []string{"secret", config.HashPassword("secret"), "sync_rev", "sha256", "hash", "lock"} {
			require.NotContains(t, lineWithoutOutcome, strings.ToLower(secret))
		}
	}
	require.True(t, kinds["settings_change"])
	require.True(t, kinds["command_denied"])
	require.True(t, outcomes["blocked"])
	require.True(t, outcomes["applied"])
	for _, forbidden := range []string{"password", "unlock", "--password"} {
		require.NotContains(t, strings.ToLower(config.ErrSettingsLocked.Error()), forbidden)
	}
}

func TestSettingsLock_RefusalMessageDoesNotLeakHints(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "secret")
	require.NoError(t, err)
	_, stderr, err := runLockTree(t, workspace, dataDir, "models", "use", "glm4_6", "glm5_turbo")
	assertRefusal(t, stderr, err, config.ErrSettingsLocked)
	_, stderr, err = runLockTree(t, workspace, dataDir, "--password", "wrong", "models", "use", "glm4_6", "glm5_turbo")
	assertRefusal(t, stderr, err, config.ErrWrongPassword)
}

func stripAuditPaths(line string) string {
	for _, field := range []string{"launch_cwd", "path"} {
		re := regexp.MustCompile(`"` + field + `":"[^"]*"`)
		line = re.ReplaceAllString(line, "")
	}
	return line
}
