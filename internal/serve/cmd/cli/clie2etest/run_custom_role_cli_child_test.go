package clie2etest

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

const (
	runRoleChildEnv         = "RUSH_TEST_ROLE_CLI_CHILD"
	runRoleChildFileEnv     = "RUSH_TEST_ROLE_CLI_CHILD_FILE"
	runRoleChildPrompt      = "test custom role dispatch"
	runRoleChildSession     = "custom-role-cli-session"
	runRoleChildRoleName    = "Research Analyst"
	runRoleChildTitlePrompt = "Generate a concise title for the following content:"
)

var (
	runRoleChildModelFlag      = flag.String("model", "", "fake CLI model")
	runRoleChildEffortFlag     = flag.String("effort", "", "fake CLI effort")
	runRoleChildToolsFlag      = flag.String("tools", "", "fake CLI native tools")
	runRoleChildAllowedFlag    = flag.String("allowedTools", "", "fake CLI allowed tools")
	runRoleChildDisallowedFlag = flag.String("disallowedTools", "", "fake CLI denied tools")
	runRoleChildMCPConfigFlag  = flag.String("mcp-config", "", "fake CLI MCP configuration")
	runRoleChildYoloFlag       = flag.Bool("dangerously-skip-permissions", false, "fake CLI bypass flag")
)

type runRoleChildEvidence struct {
	PID          int      `json:"pid"`
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Tools        string   `json:"tools"`
	Allowed      []string `json:"allowed_tools"`
	Disallowed   string   `json:"disallowed_tools"`
	Yolo         bool     `json:"yolo"`
	Args         []string `json:"args"`
	Home         string   `json:"home"`
	GlobalData   string   `json:"global_data"`
	GlobalConfig string   `json:"global_config"`
	Temp         string   `json:"temp"`
	WorkingDir   string   `json:"working_dir"`
}

type runRoleChildDiagnostic struct {
	PID    int
	Kind   string
	Model  string
	Effort string
	Args   []string
}

type runRoleChildConstructorAssertion struct {
	Expected string
	Actual   string
	Error    string
}

func TestRunCustomRoleCLIChildDispatch(t *testing.T) {
	cmdtest.KtavGateOn(t)
	cmdtest.KtavIsolateGlobalEnv(t)
	guardRoot := os.Getenv("RUSH_TESTGUARD_ROOT")
	tempRoot := t.TempDir()
	for _, tc := range []struct {
		name, role     string
		child, deleted bool
	}{
		{"exact custom role starts selected CLI child", runRoleChildRoleName, true, false},
		{"unknown role starts no child", "Unknown Analyst", false, false},
		{"wrong-case role starts no child", "research analyst", false, false},
		{"missing configured role starts no child", "Missing Analyst", false, false},
		{"deleted role starts no child", runRoleChildRoleName, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(tempRoot, strings.ReplaceAll(tc.name, " ", "-"))
			require.NoError(t, os.MkdirAll(base, 0o700))
			fakeCLIDir := filepath.Join(base, "fake-cli")
			require.NoError(t, os.MkdirAll(fakeCLIDir, 0o700))
			fakeCLIPath := filepath.Join(fakeCLIDir, "claude")
			if runtime.GOOS == "windows" {
				fakeCLIPath += ".exe"
			}
			exe, err := os.Executable()
			require.NoError(t, err)
			source, err := os.Open(exe)
			require.NoError(t, err)
			target, err := os.OpenFile(fakeCLIPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
			require.NoError(t, err)
			_, err = io.Copy(target, source)
			require.NoError(t, err)
			require.NoError(t, target.Close())
			require.NoError(t, source.Close())

			cwd := filepath.Join(base, "workspace")
			dataDir := filepath.Join(cwd, ".rush")
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			fixture := `{"options":{"disable_default_providers":true},"models":{"smart":{"provider":"local-cli","model":"cli-role-smart-default"}},"roles":{"` + runRoleChildRoleName + `":{"provider":"local-cli","model":"cli-role-custom-child","reasoning_effort":"high","tools":["view"]}}}`
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(fixture), 0o600))
			if tc.deleted {
				store, initErr := config.Init(cwd, dataDir, false, config.WithCLIModels(cliprovider.AvailableCLIModels))
				require.NoError(t, initErr)
				role := store.Config().Roles[runRoleChildRoleName]
				require.NoError(t, store.SetCustomRole(config.ScopeWorkspace, runRoleChildRoleName, config.CustomRoleUpdate{Model: role.SelectedModel, Tools: role.Tools}))
				require.NoError(t, store.RemoveCustomRole(config.ScopeWorkspace, runRoleChildRoleName))
				active := config.ActiveConfigFileName(filepath.Join(dataDir, "rush.json"))
				require.Equal(t, filepath.Join(dataDir, "rush.ktav"), active)
				require.FileExists(t, active)
				disk, readErr := config.ReadConfigDocumentLight(filepath.Join(dataDir, "rush.json"))
				require.NoError(t, readErr)
				var document struct {
					Roles map[string]json.RawMessage `json:"roles"`
				}
				require.NoError(t, json.Unmarshal(disk, &document))
				require.NotContains(t, document.Roles, runRoleChildRoleName, "active config document %s must persist role deletion", active)
			}
			home := filepath.Join(base, "home")
			for _, dir := range []string{home, filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			childFile := filepath.Join(base, "child-evidence.jsonl")
			t.Cleanup(func() {
				if t.Failed() {
					children, readErr := runRoleChildDiagnosticEvidence(childFile)
					t.Logf("custom-role CLI failure diagnosis: child_evidence=%+v evidence_read_error=%v", children, readErr)
				}
			})
			globalData, globalConfig := filepath.Join(base, "global-data"), filepath.Join(base, "global-config")
			for _, dir := range []string{globalData, globalConfig} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			extra := map[string]string{
				"HOME": home, "USERPROFILE": home, "APPDATA": filepath.Join(home, "AppData", "Roaming"),
				"LOCALAPPDATA": filepath.Join(home, "AppData", "Local"), "PATH": fakeCLIDir,
				"TMPDIR": base, "TMP": base, "TEMP": base, "RUSH_GLOBAL_DIR": "", "RUSH_GLOBAL_DATA": globalData,
				"RUSH_GLOBAL_CONFIG": globalConfig, "XDG_DATA_HOME": globalData, "XDG_CONFIG_HOME": globalConfig,
				runRoleChildEnv: "1", runRoleChildFileEnv: childFile,
			}
			if guardRoot != "" {
				extra["RUSH_TESTGUARD_ROOT"] = guardRoot
			}
			if runtime.GOOS == "windows" {
				extra["PATHEXT"] = ".COM;.EXE;.BAT;.CMD"
			}
			args := []string{"--cwd", cwd, "--data-dir", dataDir, "run", "--role", tc.role, "--json", "--session", runRoleChildSession, "--timeout", "10s", runRoleChildPrompt}
			stdout, stderr, runErr := runCLIProcess(t, cwd, cliE2EChild, args, extra)
			if !tc.child {
				require.Error(t, runErr, "stderr: %s", stderr)
				_, statErr := os.Stat(childFile)
				require.ErrorIs(t, statErr, os.ErrNotExist, "invalid role must not execute the external CLI child")
				require.Zero(t, countRunCLISessions(t, dataDir), "invalid role must fail before session creation")
				return
			}
			require.NoError(t, runErr, "stderr: %s", stderr)
			var result struct {
				SessionID string `json:"session_id"`
				FinalText string `json:"final_text"`
			}
			require.NoError(t, json.Unmarshal(stdout, &result), "stdout: %s; stderr: %s", stdout, stderr)
			require.Equal(t, runRoleChildSession, result.SessionID)
			require.Equal(t, "external fake CLI response", result.FinalText)
			raw, readErr := os.ReadFile(childFile)
			require.NoError(t, readErr)
			var records []runRoleChildEvidence
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var evidence runRoleChildEvidence
				require.NoError(t, json.Unmarshal([]byte(line), &evidence))
				records = append(records, evidence)
			}
			require.NotEmpty(t, records, "the top-level run must launch the fake CLI child")
			var root runRoleChildEvidence
			var found bool
			for _, evidence := range records {
				require.Positive(t, evidence.PID)
				require.NotEqual(t, os.Getpid(), evidence.PID, "the CLI boundary must be a real child process")
				if strings.Contains(evidence.Prompt, runRoleChildTitlePrompt) {
					continue
				}
				if strings.Contains(evidence.Prompt, runRoleChildPrompt) {
					root, found = evidence, true
					require.Equal(t, "cli-role-custom-child", evidence.Model, "every matching non-title child must select the custom-role model")
				}
			}
			require.True(t, found, "no non-title CLI child received top-level run prompt; child evidence=%+v", runRoleChildDiagnostics(records))
			for _, path := range []string{root.Home, root.GlobalData, root.GlobalConfig, root.Temp, root.WorkingDir} {
				require.True(t, runRoleChildPathUnder(path, filepath.Dir(tempRoot)), "child path escaped test root: %s", path)
			}
			require.Equal(t, "cli-role-custom-child", root.Model)
			require.Equal(t, "high", root.Effort)
			require.True(t, root.Yolo)
			require.Empty(t, root.Tools)
			require.GreaterOrEqual(t, len(root.Args), 6)
			require.Equal(t, []string{"--tools", "", "--allowedTools", "mcp__rush__Read", "--disallowedTools", "TodoWrite"}, root.Args[len(root.Args)-6:])
			require.Equal(t, []string{"mcp__rush__Read"}, root.Allowed)
			require.Equal(t, "TodoWrite", root.Disallowed)
		})
	}
}

func TestRunCustomRoleCLIChildHelper(t *testing.T) {
	if os.Getenv(runRoleChildEnv) != "1" {
		return
	}
	prompt, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	require.NoError(t, err)
	allowed := strings.FieldsFunc(*runRoleChildAllowedFlag, func(r rune) bool { return r == ',' })
	workingDir, err := os.Getwd()
	require.NoError(t, err)
	require.Empty(t, flag.Args())
	evidence := runRoleChildEvidence{
		PID: os.Getpid(), Prompt: string(prompt), Model: *runRoleChildModelFlag,
		Effort: *runRoleChildEffortFlag, Tools: *runRoleChildToolsFlag, Allowed: allowed,
		Disallowed: *runRoleChildDisallowedFlag, Yolo: *runRoleChildYoloFlag, Args: append([]string(nil), os.Args...),
		Home: os.Getenv("HOME"), GlobalData: os.Getenv("RUSH_GLOBAL_DATA"), GlobalConfig: os.Getenv("RUSH_GLOBAL_CONFIG"), Temp: os.TempDir(), WorkingDir: workingDir,
	}
	encoded, err := json.Marshal(evidence)
	require.NoError(t, err)
	file, err := os.OpenFile(os.Getenv(runRoleChildFileEnv), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = fmt.Fprintln(file, string(encoded))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = fmt.Fprintln(os.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"external fake CLI response"}}}`)
	require.NoError(t, err)
}

func runRoleChildDiagnosticEvidence(path string) ([]runRoleChildDiagnostic, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records []runRoleChildEvidence
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var evidence runRoleChildEvidence
		if err := json.Unmarshal([]byte(line), &evidence); err != nil {
			return nil, err
		}
		records = append(records, evidence)
	}
	return runRoleChildDiagnostics(records), nil
}

func runRoleChildDiagnostics(records []runRoleChildEvidence) []runRoleChildDiagnostic {
	diagnostics := make([]runRoleChildDiagnostic, 0, len(records))
	for _, evidence := range records {
		kind := "other"
		if strings.Contains(evidence.Prompt, runRoleChildTitlePrompt) {
			kind = "title"
		} else if strings.Contains(evidence.Prompt, runRoleChildPrompt) {
			kind = "root"
		}
		diagnostics = append(diagnostics, runRoleChildDiagnostic{
			PID: evidence.PID, Kind: kind, Model: evidence.Model,
			Effort: evidence.Effort, Args: runRoleChildSafeArgs(evidence.Args),
		})
	}
	return diagnostics
}

func runRoleChildSafeArgs(args []string) []string {
	redactNext := false
	result := make([]string, len(args))
	for i, arg := range args {
		lower := strings.ToLower(arg)
		if redactNext {
			result[i] = "[REDACTED]"
			redactNext = false
			continue
		}
		if strings.HasPrefix(lower, "--mcp-config=") || runRoleChildArgHasSensitiveValue(lower) {
			result[i] = "[REDACTED]"
			continue
		}
		result[i] = arg
		redactNext = runRoleChildArgTakesSensitiveValue(lower)
	}
	return result
}

func runRoleChildArgHasSensitiveValue(arg string) bool {
	for _, key := range []string{"token", "secret", "password", "authorization", "bearer", "api-key", "apikey", "credential"} {
		if strings.Contains(arg, key+"=") || strings.HasPrefix(arg, key+":") || strings.Contains(arg, key+" ") {
			return true
		}
	}
	return false
}

func runRoleChildArgTakesSensitiveValue(arg string) bool {
	if strings.EqualFold(arg, "--mcp-config") {
		return true
	}
	if strings.Contains(arg, "=") {
		return false
	}
	for _, key := range []string{"token", "secret", "password", "authorization", "bearer", "api-key", "apikey", "credential"} {
		if strings.Contains(arg, key) {
			return true
		}
	}
	return false
}

func runRoleChildSpecModelIDs(specs []cliprovider.CLISpec) []string {
	modelIDs := make([]string, 0, len(specs))
	for _, spec := range specs {
		modelIDs = append(modelIDs, spec.ModelID)
	}
	return modelIDs
}

func runRoleChildPreflightConstructors(modelIDs []string) []runRoleChildConstructorAssertion {
	provider := cliprovider.New("", "", func() bool { return false }, nil, nil, nil)
	assertions := make([]runRoleChildConstructorAssertion, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		assertion := runRoleChildConstructorAssertion{Expected: modelID}
		model, err := provider.LanguageModel(context.Background(), modelID)
		if err != nil {
			assertion.Error = err.Error()
		} else {
			assertion.Actual = model.Model()
		}
		assertions = append(assertions, assertion)
	}
	return assertions
}

func countRunCLISessions(t *testing.T, dataDir string) int {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&count))
	return count
}

func runRoleChildPathUnder(path, root string) bool {
	if path == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
