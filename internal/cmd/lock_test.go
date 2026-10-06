package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestLockUnlock_GlobalLocalAndRelock(t *testing.T) {
	path := isolatedModelsEnv(t)
	defer resetModelsUseFlags(t)
	workspace, dataDir := t.TempDir(), t.TempDir()
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
	probe := &cobra.Command{Use: "probe", Run: func(*cobra.Command, []string) { bodyRan = true }}
	rootCmd.AddCommand(probe)
	defer rootCmd.RemoveCommand(probe)
	_, stderr, err := runLockTree(t, workspace, dataDir, "--password", "wrong", "probe")
	require.ErrorIs(t, err, config.ErrWrongPassword)
	require.False(t, bodyRan)
	require.Contains(t, stderr, config.ErrWrongPassword.Error())
}

func TestPassword_FlagSetsProcessPasswordAndAuthorizesModelsUse(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	defer resetModelsUseFlags(t)
	config.SetProcessPassword("")
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"sync_rev":"`+config.HashPassword("models-pass")+`","providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o600))
	rootCmd.AddCommand(modelsCmd)
	defer rootCmd.RemoveCommand(modelsCmd)
	_, _, err := runLockTree(t, filepath.Dir(globalPath), filepath.Join(filepath.Dir(globalPath), "test-data"), "--password", "models-pass", "models", "use", "glm4_6", "glm5_turbo")
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
	require.Contains(t, string(data), config.HashPassword("local-right"))
	require.NotContains(t, string(data), config.HashPassword("replacement"))
}

func TestSettingsLock_ScopePriorityAndModelsUseRefusal(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	defer resetModelsUseFlags(t)
	workspaceA, dataA := t.TempDir(), t.TempDir()
	workspaceB, dataB := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceA, ".rush"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceB, ".rush"), 0o700))
	if modelsCmd.Parent() != rootCmd {
		rootCmd.AddCommand(modelsCmd)
	}

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
	message := strings.ToLower(strings.ReplaceAll(err.Error()+stderr, config.ErrWrongPassword.Error(), ""))
	for _, forbidden := range []string{"unlock", "--password", "sha256", "hash", "password"} {
		require.NotContains(t, message, forbidden)
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
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		require.NoError(t, err)
	}
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
		lineWithoutOutcome := strings.ReplaceAll(strings.ToLower(line), `"outcome":"blocked"`, "")
		for _, secret := range []string{"secret", config.HashPassword("secret"), "sync_rev", "sha256", "hash", "lock"} {
			require.NotContains(t, lineWithoutOutcome, strings.ToLower(secret))
		}
	}
	require.True(t, kinds["settings_change"])
	require.True(t, kinds["command_denied"])
	require.True(t, outcomes["blocked"])
	require.True(t, outcomes["applied"])
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "server", "handlers_config.go"), nil, parser.AllErrors)
	require.NoError(t, err)
	mapped := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "handleSetTheme" && fn.Name.Name != "handleSetKeepAlive") {
			return true
		}
		ast.Inspect(fn.Body, func(bodyNode ast.Node) bool {
			call, ok := bodyNode.(*ast.CallExpr)
			if !ok || len(call.Args) != 4 {
				return true
			}
			reply, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || reply.Sel.Name != "reply" {
				return true
			}
			event, ok := call.Args[1].(*ast.Ident)
			if !ok || event.Name != "EventError" {
				return true
			}
			if nilArg, ok := call.Args[2].(*ast.Ident); !ok || nilArg.Name != "nil" {
				return true
			}
			errCall, ok := call.Args[3].(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := errCall.Fun.(*ast.SelectorExpr)
			if !ok || method.Sel.Name != "Error" {
				return true
			}
			receiver, ok := method.X.(*ast.Ident)
			if ok && receiver.Name == "err" {
				mapped[fn.Name.Name] = true
			}
			return true
		})
		return false
	})
	require.True(t, mapped["handleSetTheme"])
	require.True(t, mapped["handleSetKeepAlive"])
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
	return global, workspace, dataDir
}

func runLockTree(t *testing.T, cwd, dataDir string, args ...string) (stdout, stderr string, runErr error) {
	t.Helper()
	oldRootCtx, oldModelsCtx := rootCmd.Context(), modelsUseCmd.Context()
	rootCmd.SetContext(context.Background())
	modelsUseCmd.SetContext(context.Background())
	defer func() {
		rootCmd.SetContext(oldRootCtx)
		modelsUseCmd.SetContext(oldModelsCtx)
	}()
	if modelsCmd.Parent() != rootCmd {
		rootCmd.AddCommand(modelsCmd)
	}
	// Snapshot root flag state before resetting or setting any models flag:
	// inherited flags may share the root flag's Value pointer.
	rootFlags := rootCmd.PersistentFlags()
	cwdRootFlag, dataRootFlag, passRootFlag := rootFlags.Lookup("cwd"), rootFlags.Lookup("data-dir"), rootFlags.Lookup("password")
	origCwd, origCwdChanged := cwdRootFlag.Value.String(), cwdRootFlag.Changed
	origData, origDataChanged := dataRootFlag.Value.String(), dataRootFlag.Changed
	origPass, origPassChanged := passRootFlag.Value.String(), passRootFlag.Changed
	localFlags := modelsUseCmd.Flags()
	cwdFlag, dataFlag := localFlags.Lookup("cwd"), localFlags.Lookup("data-dir")
	var origModelsCwd, origModelsData string
	var origModelsCwdChanged, origModelsDataChanged bool
	if cwdFlag != nil && cwdFlag != cwdRootFlag {
		origModelsCwd, origModelsCwdChanged = cwdFlag.Value.String(), cwdFlag.Changed
	}
	if dataFlag != nil && dataFlag != dataRootFlag {
		origModelsData, origModelsDataChanged = dataFlag.Value.String(), dataFlag.Changed
	}
	origWD, _ := os.Getwd()
	defer func() {
		if cwdFlag != nil && cwdFlag != cwdRootFlag {
			_ = cwdFlag.Value.Set(origModelsCwd)
			cwdFlag.Changed = origModelsCwdChanged
		}
		if dataFlag != nil && dataFlag != dataRootFlag {
			_ = dataFlag.Value.Set(origModelsData)
			dataFlag.Changed = origModelsDataChanged
		}
		_ = cwdRootFlag.Value.Set(origCwd)
		cwdRootFlag.Changed = origCwdChanged
		_ = dataRootFlag.Value.Set(origData)
		dataRootFlag.Changed = origDataChanged
		_ = passRootFlag.Value.Set(origPass)
		passRootFlag.Changed = origPassChanged
	}()
	resetLockFlags()
	_ = rootFlags.Set("cwd", cwd)
	_ = rootFlags.Set("data-dir", dataDir)
	_ = rootFlags.Set("password", "")
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	require.NoError(t, err)
	rErr, wErr, err := os.Pipe()
	require.NoError(t, err)
	var outBuf, errBuf bytes.Buffer
	doneOut, doneErr := make(chan struct{}), make(chan struct{})
	go func() { _, _ = io.Copy(&outBuf, rOut); close(doneOut) }()
	go func() { _, _ = io.Copy(&errBuf, rErr); close(doneErr) }()
	os.Stdout, os.Stderr = wOut, wErr
	rootCmd.SetArgs(append([]string{"--cwd", cwd, "--data-dir", dataDir}, args...))
	runErr = rootCmd.Execute()
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	_ = os.Chdir(origWD)
	resetLockFlags()

	config.SetProcessPassword("")
	<-doneOut
	<-doneErr
	stdout, stderr = outBuf.String(), errBuf.String()
	if errors.Is(runErr, config.ErrSettingsLocked) || errors.Is(runErr, config.ErrWrongPassword) || errors.Is(runErr, config.ErrNotLocked) {
		assertRefusal(t, stdout+stderr, runErr, runErr)
	}
	_ = rOut.Close()
	_ = rErr.Close()
	return stdout, stderr, runErr
}

func resetLockFlags() {
	for _, c := range []*cobra.Command{lockCmd, unlockCmd} {
		for _, name := range []string{"global", "local"} {
			if f := c.Flags().Lookup(name); f != nil {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		}
		c.SetArgs(nil)
	}
	for _, name := range []string{"global", "local", "smart", "fast", "worker", "reviewer"} {
		if f := modelsUseCmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	modelsUseCmd.SetArgs(nil)
}
