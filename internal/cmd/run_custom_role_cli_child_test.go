package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

const (
	runRoleChildEnv      = "RUSH_TEST_ROLE_CLI_CHILD"
	runRoleChildFileEnv  = "RUSH_TEST_ROLE_CLI_CHILD_FILE"
	runRoleChildPrompt   = "test custom role dispatch"
	runRoleChildSession  = "custom-role-cli-session"
	runRoleChildRoleName = "Research Analyst"
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

func TestRunCustomRoleCLIChildDispatch(t *testing.T) {
	ktavGateOn(t)
	guardRoot := os.Getenv("RUSH_TESTGUARD_ROOT")
	tempRoot, err := os.MkdirTemp("", "rush-role-cli-child-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempRoot) })
	isolateRunRoleChildEnvironment(t, tempRoot, guardRoot)
	originalAvailable := cliprovider.AvailableFunc
	t.Cleanup(func() { cliprovider.AvailableFunc = originalAvailable })

	executable, err := os.Executable()
	require.NoError(t, err)
	fakeCLIDir := filepath.Join(tempRoot, "fake-cli")
	require.NoError(t, os.MkdirAll(fakeCLIDir, 0o700))
	fakeCLIPath := filepath.Join(fakeCLIDir, "claude")
	if runtime.GOOS == "windows" {
		fakeCLIPath += ".exe"
	}
	source, err := os.Open(executable)
	require.NoError(t, err)
	target, err := os.OpenFile(fakeCLIPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	require.NoError(t, err)
	_, copyErr := io.Copy(target, source)
	require.NoError(t, copyErr)
	require.NoError(t, target.Close())
	require.NoError(t, source.Close())

	var claude cliprovider.CLISpec
	for _, spec := range cliprovider.All {
		if spec.Binary == "claude" && spec.NewPartParser != nil {
			claude = spec
			break
		}
	}
	require.NotEmpty(t, claude.ModelID, "expected a real Claude stream parser in cliprovider.All")

	makeSpec := func(modelID string) cliprovider.CLISpec {
		spec := claude
		spec.ModelID = modelID
		spec.ModelName = modelID
		spec.Binary = "claude"
		spec.AlwaysStdin = true
		spec.NoPTY = true
		spec.PromptFlag = ""
		spec.StdinPrintFlag = ""
		spec.BuildArgs = func(yolo bool) []string {
			args := []string{"-test.run=^TestRunCustomRoleCLIChildHelper$", "--model", modelID}
			if yolo {
				args = append(args, "--dangerously-skip-permissions")
			}
			return args
		}
		return spec
	}
	smartModelID, customModelID := "cli-role-smart-default", "cli-role-custom-child"
	available := []cliprovider.CLISpec{makeSpec(smartModelID), makeSpec(customModelID)}
	originalAll := cliprovider.All
	t.Cleanup(func() { cliprovider.All = originalAll })
	cliprovider.All = available
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return available }

	for _, tc := range []struct {
		name   string
		role   string
		child  bool
		delete bool
	}{
		{name: "exact custom role starts selected CLI child", role: runRoleChildRoleName, child: true},
		{name: "unknown role starts no child", role: "Unknown Analyst"},
		{name: "wrong-case role starts no child", role: "research analyst"},
		{name: "missing configured role starts no child", role: "Missing Analyst"},
		{name: "deleted role starts no child", role: runRoleChildRoleName, delete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunCLIConfig(t)
			base := filepath.Join(tempRoot, strings.ReplaceAll(tc.name, " ", "-"))
			require.NoError(t, os.MkdirAll(base, 0o700))
			home := filepath.Join(base, "home")
			require.NoError(t, os.MkdirAll(home, 0o700))
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
			t.Setenv("PATH", filepath.Dir(fakeCLIPath))
			if runtime.GOOS == "windows" {
				t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
			}

			cwd := filepath.Join(base, "workspace")
			dataDir := filepath.Join(cwd, ".rush")
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			fixture := `{"options":{"disable_default_providers":true},"models":{"smart":{"provider":"local-cli","model":"` + smartModelID + `"}},"roles":{"` + runRoleChildRoleName + `":{"provider":"local-cli","model":"` + customModelID + `","reasoning_effort":"high","tools":["view"]}}}`
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(fixture), 0o600))
			if tc.delete {
				store, initErr := config.Init(cwd, dataDir, false)
				require.NoError(t, initErr)
				role := store.Config().Roles[runRoleChildRoleName]
				require.NoError(t, store.SetCustomRole(config.ScopeWorkspace, runRoleChildRoleName, config.CustomRoleUpdate{Model: role.SelectedModel, Tools: role.Tools}))
				require.NoError(t, store.RemoveCustomRole(config.ScopeWorkspace, runRoleChildRoleName))
				activePath := config.ActiveConfigFileName(filepath.Join(dataDir, "rush.json"))
				require.Equal(t, filepath.Join(dataDir, "rush.ktav"), activePath)
				require.FileExists(t, activePath)
				disk, readErr := config.ReadConfigDocumentLight(filepath.Join(dataDir, "rush.json"))
				require.NoError(t, readErr)
				var document struct {
					Roles map[string]json.RawMessage `json:"roles"`
				}
				require.NoError(t, json.Unmarshal(disk, &document))
				require.NotContains(t, document.Roles, runRoleChildRoleName, "the active config document %s must persist deletion", activePath)
			}

			childFile := filepath.Join(base, "child-evidence.jsonl")
			t.Setenv(runRoleChildEnv, "1")
			t.Setenv(runRoleChildFileEnv, childFile)
			stdout, runErr := executeTopLevelRun(t, cwd, dataDir, tc.role)
			if !tc.child {
				require.Error(t, runErr)
				_, statErr := os.Stat(childFile)
				require.ErrorIs(t, statErr, os.ErrNotExist, "invalid role must not execute the external CLI child")
				require.Zero(t, countRunCLISessions(t, dataDir), "invalid role must fail before session creation")
				return
			}

			require.NoError(t, runErr, "run stdout: %s", stdout)
			var result struct {
				SessionID string `json:"session_id"`
				FinalText string `json:"final_text"`
			}
			require.NoError(t, json.Unmarshal(stdout, &result), "run stdout: %s", stdout)
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
			rootPromptFound := false
			var rootEvidence runRoleChildEvidence
			for _, evidence := range records {
				require.Positive(t, evidence.PID)
				require.NotEqual(t, os.Getpid(), evidence.PID, "the CLI boundary must be a real child process")
				if strings.Contains(evidence.Prompt, runRoleChildPrompt) {
					rootPromptFound = true
					rootEvidence = evidence
				}
			}
			require.True(t, rootPromptFound, "no child received the top-level run prompt")
			require.True(t, runRoleChildPathUnder(rootEvidence.Home, filepath.Dir(tempRoot)), "child HOME escaped test temp root")
			require.True(t, runRoleChildPathUnder(rootEvidence.GlobalData, filepath.Dir(tempRoot)), "child global data escaped test temp root")
			require.True(t, runRoleChildPathUnder(rootEvidence.GlobalConfig, filepath.Dir(tempRoot)), "child global config escaped test temp root")
			require.True(t, runRoleChildPathUnder(rootEvidence.Temp, filepath.Dir(tempRoot)), "child temp dir escaped test temp root")
			require.True(t, runRoleChildPathUnder(rootEvidence.WorkingDir, filepath.Dir(tempRoot)), "child working dir escaped test temp root")
			require.Equal(t, customModelID, rootEvidence.Model, "custom role must select its distinct CLI model, not the smart default")
			require.Equal(t, "high", rootEvidence.Effort)
			require.True(t, rootEvidence.Yolo, "non-interactive run should pass the CLI bypass flag")
			require.Empty(t, rootEvidence.Tools, "role-limited CLI must disable native tools")
			require.GreaterOrEqual(t, len(rootEvidence.Args), 6)
			require.Equal(t, []string{"--tools", "", "--allowedTools", "mcp__rush__Read", "--disallowedTools", "TodoWrite"}, rootEvidence.Args[len(rootEvidence.Args)-6:])
			require.Equal(t, []string{"mcp__rush__Read"}, rootEvidence.Allowed, "the child argv must contain only the role-approved Rush read tool")
			require.Equal(t, "TodoWrite", rootEvidence.Disallowed)
		})
	}
}

func isolateRunRoleChildEnvironment(t *testing.T, tempRoot, guardRoot string) {
	t.Helper()
	priorEnv := os.Environ()
	var windowsEnv []string
	for _, entry := range priorEnv {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "SYSTEMROOT") || strings.EqualFold(key, "WINDIR") {
			windowsEnv = append(windowsEnv, entry)
		}
	}
	os.Clearenv()
	t.Cleanup(func() {
		os.Clearenv()
		for _, entry := range priorEnv {
			key, value, _ := strings.Cut(entry, "=")
			_ = os.Setenv(key, value)
		}
	})
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		require.NoError(t, os.Setenv(key, tempRoot))
	}
	for _, entry := range windowsEnv {
		key, value, _ := strings.Cut(entry, "=")
		require.NoError(t, os.Setenv(key, value))
	}
	if guardRoot != "" {
		require.NoError(t, os.Setenv("RUSH_TESTGUARD_ROOT", guardRoot))
	}
}

func runRoleChildPathUnder(path, root string) bool {
	if path == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
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
	require.Empty(t, flag.Args(), "all fake CLI arguments should be parsed as flags, with no positional arguments")
	evidence := runRoleChildEvidence{
		PID: os.Getpid(), Prompt: string(prompt), Model: *runRoleChildModelFlag,
		Effort: *runRoleChildEffortFlag, Tools: *runRoleChildToolsFlag,
		Allowed:    allowed,
		Disallowed: *runRoleChildDisallowedFlag,
		Yolo:       *runRoleChildYoloFlag, Args: append([]string(nil), os.Args...),
		Home: os.Getenv("HOME"), GlobalData: os.Getenv("RUSH_GLOBAL_DATA"),
		GlobalConfig: os.Getenv("RUSH_GLOBAL_CONFIG"), Temp: os.TempDir(),
		WorkingDir: workingDir,
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
