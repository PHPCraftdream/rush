// Lock/unlock journal and password-consistency tests. REVERT-CHECKS (the
// orchestrator runs the mutants; do not run them here) — the single-line
// production change each test is meant to catch:
//
//	TestLockJournal_LocalWrongPasswordJournaled — dropping checkAppPassword's
//	  command_denied write or the journal end record for a wrong --password
//	  against a LOCAL lock (root.go).
//	TestUnlock_HintsAtLocalLockWhenGlobalUnlocked — removing the LockState
//	  hint branch in unlockCmd (lock.go).
//	TestUnlock_EmptyArgumentRefused — removing the empty-argument guard in
//	  unlockCmd (lock.go).
//	TestUnlock_GlobalFlagAndOutputText — removing the --global/--local flag
//	  registration or the unlock output line (lock.go).
//	TestUnlock_WrongPasswordJournaled — dropping the pre-run denial journal
//	  end record for a wrong --password against a GLOBAL lock (root.go).
//	TestPersistentPreRunOnlyOnRoot — setting PersistentPreRun/PersistentPreRunE
//	  on any command other than rootCmd anywhere in the tree.
//	TestPassword_SetupAppCheckAppPasswordPath — removing the checkAppPassword
//	  call from setupApp/setupAppLite or its empty-password skip (root.go).
package cmd

import (
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestLockJournal_LocalWrongPasswordJournaled: a wrong --password against a
// workspace (local) lock is refused before the command's work and journaled
// (command_end, non-zero exit, refusal's first line, no password text).
func TestLockJournal_LocalWrongPasswordJournaled(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "local-right")
	require.NoError(t, err)

	resetCommandJournalForTests()
	_, _, runErr := runLockTree(t, workspace, dataDir, "--password", "local-wrong", "lock", "--local", "replacement")
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	finalizeCommandJournal(os.Args[1:], runErr)

	var denied, end map[string]any
	for _, rec := range journalRecords(t) {
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
	raw := journalReadRaw(t)
	require.NotContains(t, raw, "local-wrong")
	require.NotContains(t, raw, "local-right")
}

// TestUnlock_HintsAtLocalLockWhenGlobalUnlocked: unlocking global while only
// the workspace is locked points at --local instead of "settings are not locked".
func TestUnlock_HintsAtLocalLockWhenGlobalUnlocked(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "localpw")
	require.NoError(t, err)
	_, _, err = runLockTree(t, workspace, dataDir, "unlock", "localpw")
	require.Error(t, err)
	require.Contains(t, err.Error(), "only the workspace is locked")
	require.Contains(t, err.Error(), "--local")
	data, readErr := os.ReadFile(dataDir + string(os.PathSeparator) + "rush.json")
	require.NoError(t, readErr)
	require.Contains(t, string(data), config.HashPassword("localpw"), "the refused unlock must not change the lock")
}

// TestUnlock_EmptyArgumentRefused: `rush unlock ""` fails like `rush lock ""`.
func TestUnlock_EmptyArgumentRefused(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	_, _, err := runLockTree(t, workspace, dataDir, "unlock", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "an argument must not be empty")
}

// TestUnlock_GlobalFlagAndOutputText: the explicit --global scope and the
// unlock confirmation text for both scopes.
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

// TestUnlock_WrongPasswordJournaled: a wrong --password on unlock (global
// lock) is journaled with the refusal's first line and without the password.
func TestUnlock_WrongPasswordJournaled(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "right-pw-value")
	require.NoError(t, err)

	resetCommandJournalForTests()
	_, _, runErr := runLockTree(t, workspace, dataDir, "unlock", "wrong-pw-value")
	require.ErrorIs(t, runErr, config.ErrWrongPassword)
	finalizeCommandJournal(os.Args[1:], runErr)

	var end map[string]any
	for _, rec := range journalRecords(t) {
		if rec["cmd"] == "rush unlock" && rec["kind"] == "command_end" {
			if code, ok := rec["exit"].(float64); ok && code == 1 {
				end = rec
			}
		}
	}
	require.NotNil(t, end)
	require.Contains(t, end["err"], "wrong password")
	raw := journalReadRaw(t)
	require.NotContains(t, raw, "wrong-pw-value")
	require.NotContains(t, raw, "right-pw-value")
}

// TestPersistentPreRunOnlyOnRoot: only rootCmd installs the journal/password
// pre-run chain; no subcommand may set PersistentPreRun(Persistent)RunE.
func TestPersistentPreRunOnlyOnRoot(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != rootCmd {
			require.Nil(t, c.PersistentPreRun, "%s sets PersistentPreRun", c.CommandPath())
			require.Nil(t, c.PersistentPreRunE, "%s sets PersistentPreRunE", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	require.NotNil(t, rootCmd.PersistentPreRunE, "root must keep the journal/password pre-run")
	require.Nil(t, rootCmd.PersistentPreRun, "root must use the E variant only")
}

// TestPassword_SetupAppCheckAppPasswordPath: the setupApp/setupAppLite shared
// check refuses a wrong --password after Init (workspace lock visible), writes
// the command_denied event, and is skipped without --password.
func TestPassword_SetupAppCheckAppPasswordPath(t *testing.T) {
	_, workspace, dataDir := isolateLockEnv(t)
	ensureAuditDirFunc()
	store, err := config.Init(workspace, dataDir, false)
	require.NoError(t, err)
	require.NoError(t, store.LockSettings(config.ScopeWorkspace, "right"))

	probe := &cobra.Command{Use: "probe"}
	probe.Flags().String("password", "", "")
	require.NoError(t, probe.Flags().Set("password", "wrong"))
	config.SetProcessPassword("wrong")
	t.Cleanup(func() { config.SetProcessPassword("") })

	err = checkAppPassword(probe, store)
	require.ErrorIs(t, err, config.ErrWrongPassword)
	require.True(t, hasAuditKind(t, "command_denied"), "the refusal must be audited")

	require.NoError(t, probe.Flags().Set("password", ""))
	require.NoError(t, checkAppPassword(probe, store), "no --password: the setup check is skipped")
}

// hasAuditKind reports whether the journal holds any record of the kind.
func hasAuditKind(t *testing.T, kind string) bool {
	t.Helper()
	for _, rec := range journalRecords(t) {
		if rec["kind"] == kind {
			return true
		}
	}
	return false
}

// REVERT-CHECK: dropping checkAppPassword from setupApp (the full app path)
// must fail TestPassword_SetupAppRefusesWrongLocalPassword.
func TestPassword_SetupAppRefusesWrongLocalPassword(t *testing.T) {
	journalTestEnv(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
	_, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "local-right")
	require.NoError(t, err)

	_, _, runErr := runLockTree(t, workspace, dataDir, "--password", "local-wrong", "sessions", "list")
	require.ErrorIs(t, runErr, config.ErrWrongPassword, "a full-app command must refuse a wrong --password against a workspace lock")
}
