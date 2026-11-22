package clie2etest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
	"github.com/stretchr/testify/require"
)

const modelsCLIHelperEnv = "RUSH_TEST_MODELS_CLI_HELPER"

type modelsCLIRequest struct {
	Args              []string `json:"args"`
	KtavGateOn        bool     `json:"ktav_gate_on,omitempty"`
	LocalCLIAvailable bool     `json:"local_cli_available,omitempty"`
}

type modelsCLIFixture struct {
	root, globalData, globalConfig, cwd, dataDir, localConfig string
}

type modelsCLIResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// runModelsCLIHelperProcess executes the real CLI in a fresh process. Keeping
// the command in a subprocess exercises NewRootCmd/Execute, process-global
// configuration, and the same exit/error path used by the installed binary.
func runModelsCLIHelperProcess(t *testing.T) {
	if os.Getenv(modelsCLIHelperEnv) != "1" {
		return
	}
	var request modelsCLIRequest
	if err := json.Unmarshal([]byte(os.Getenv(modelsCLIHelperEnv+"_REQUEST")), &request); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG",
		"XDG_DATA_HOME", "XDG_CONFIG_HOME", "HOME", "USERPROFILE", "LOCALAPPDATA",
	} {
		if err := os.MkdirAll(os.Getenv(name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config.SetKtavAutoMigrateForTest(false)
	if request.KtavGateOn {
		config.SetKtavAutoMigrateForTest(true)
	}
	if request.LocalCLIAvailable {
		cliprovider.AvailableFunc = func() []cliprovider.CLISpec {
			return []cliprovider.CLISpec{{ModelID: "cli-claude-fable-5", ModelName: "Claude Fable 5"}}
		}
	}
	os.Args = append([]string{"rush"}, request.Args...)
	cli.Execute()
	os.Exit(0)
}

func newModelsCLIFixture(t *testing.T) modelsCLIFixture {
	t.Helper()
	root := t.TempDir()
	fixture := modelsCLIFixture{
		root:         root,
		globalData:   filepath.Join(root, "global-data"),
		globalConfig: filepath.Join(root, "global-config"),
		cwd:          filepath.Join(root, "workspace"),
		dataDir:      filepath.Join(root, "workspace", ".rush"),
		localConfig:  filepath.Join(root, "workspace", ".rush", "rush.json"),
	}
	for _, dir := range []string{
		fixture.globalData, fixture.globalConfig, fixture.cwd, fixture.dataDir,
		filepath.Dir(fixture.localConfig), filepath.Join(root, "xdg-data"), filepath.Join(root, "xdg-config"),
		filepath.Join(root, "home"), filepath.Join(root, "userprofile"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	seed := `{"providers":{"zai":{"api_key":"test-zai-key"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(fixture.globalData, "rush.json"), []byte(seed), 0o600))
	return fixture
}

func runModelsCLI(t *testing.T, fixture modelsCLIFixture, ktavGateOn bool, args ...string) modelsCLIResult {
	t.Helper()
	return runModelsCLIRequest(t, fixture, modelsCLIRequest{
		Args:       append([]string{"--cwd", fixture.cwd, "--data-dir", fixture.dataDir}, args...),
		KtavGateOn: ktavGateOn,
	})
}

func runModelsCLIRequest(t *testing.T, fixture modelsCLIFixture, request modelsCLIRequest) modelsCLIResult {
	t.Helper()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
	cmd.Dir = fixture.cwd
	cmd.Env = overrideEnv(os.Environ(), map[string]string{
		modelsCLIHelperEnv:              "1",
		modelsCLIHelperEnv + "_REQUEST": string(encoded),
		"RUSH_GLOBAL_DATA":              fixture.globalData,
		"RUSH_GLOBAL_CONFIG":            fixture.globalConfig,
		"RUSH_GLOBAL_DIR":               "",
		"RUSH_PROVIDER_CACHE_ONLY":      "1",
		"XDG_DATA_HOME":                 filepath.Join(fixture.root, "xdg-data"),
		"XDG_CONFIG_HOME":               filepath.Join(fixture.root, "xdg-config"),
		"HOME":                          filepath.Join(fixture.root, "home"),
		"USERPROFILE":                   filepath.Join(fixture.root, "userprofile"),
		"LOCALAPPDATA":                  filepath.Join(fixture.root, "localappdata"),
	})
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	result := modelsCLIResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr == nil {
		return result
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		require.NoError(t, runErr, "CLI helper process (stderr: %s)", result.Stderr)
	}
	if exitErr != nil {
		result.ExitCode = exitErr.ExitCode()
	} else if ctx.Err() != nil {
		t.Fatalf("CLI helper timed out: %v\nstderr: %s", ctx.Err(), result.Stderr)
	}
	return result
}

func seedTamperedModelsCLI(t *testing.T, fixture modelsCLIFixture) string {
	t.Helper()
	const badRev = "0000000000000000000000000000000000000000000000000000000000000000"
	seed := `{"providers":{"zai":{"api_key":"test-zai-key"}},"cache_rev":"` + badRev + `","models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	path := filepath.Join(fixture.globalData, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	return path
}

func seedLockedModelsMissingRevCLI(t *testing.T, fixture modelsCLIFixture) string {
	t.Helper()
	seed := `{"providers":{"zai":{"api_key":"test-zai-key"}},"sync_rev":"` + config.HashPassword("pw") + `","models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	path := filepath.Join(fixture.globalData, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	return path
}
