package clie2etest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/stretchr/testify/require"
)

const (
	cliE2EHelperEnv = "RUSH_CLI_E2E_HELPER"
	cliE2EArgsEnv   = "RUSH_CLI_E2E_ARGS"
	cliE2EModeEnv   = "RUSH_CLI_E2E_MODE"
	cliE2EChild     = "role-cli"
	cliE2EDispatch  = "dispatch"
)

// runCLIExecuteHelper starts a fresh CLI process with controlled providers.
// cli.Execute constructs a fresh root and retains production process behavior.
func runCLIExecuteHelper(t *testing.T) {
	if os.Getenv(cliE2EHelperEnv) != "1" {
		return
	}
	cmdtest.KtavGateOn(t)
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(cliE2EArgsEnv)), &args); err != nil {
		t.Fatal(err)
	}
	switch os.Getenv(cliE2EModeEnv) {
	case cliE2EChild:
		configureRoleCLIFixture(t)
	case cliE2EDispatch:
		cliprovider.All = nil
		cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	default:
		t.Fatalf("unknown CLI helper mode")
	}
	// App setup can happen during Execute; establish the isolated logger first.
	log.Setup("", false)
	os.Args = append([]string{"rush"}, args...)
	cli.Execute()
	os.Exit(0)
}

func configureRoleCLIFixture(t *testing.T) {
	var claude cliprovider.CLISpec
	for _, spec := range cliprovider.All {
		if spec.Binary == "claude" && spec.NewPartParser != nil {
			claude = spec
			break
		}
	}
	if claude.NewPartParser == nil {
		t.Fatal("expected a real Claude stream parser in cliprovider.All")
	}
	makeSpec := func(id string) cliprovider.CLISpec {
		spec := claude
		spec.ModelID, spec.ModelName = id, id
		spec.Binary, spec.AlwaysStdin, spec.NoPTY = "claude", true, true
		spec.PromptFlag, spec.StdinPrintFlag = "", ""
		spec.BuildArgs = func(yolo bool) []string {
			args := []string{"-test.run=^TestRunCustomRoleCLIChildHelper$", "--model", id}
			if yolo {
				args = append(args, "--dangerously-skip-permissions")
			}
			return args
		}
		return spec
	}
	cliprovider.All = []cliprovider.CLISpec{makeSpec("cli-role-smart-default"), makeSpec("cli-role-custom-child")}
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return cliprovider.All }

	registryModelIDs := runRoleChildSpecModelIDs(cliprovider.All)
	catalogModelIDs := runRoleChildSpecModelIDs(cliprovider.Available())
	for _, modelID := range []string{"cli-role-smart-default", "cli-role-custom-child"} {
		require.Contains(t, registryModelIDs, modelID, "provider.New registry (cliprovider.All) must include fixture model")
		require.Contains(t, catalogModelIDs, modelID, "config catalog (cliprovider.AvailableFunc) must include fixture model")
	}
	for _, assertion := range runRoleChildPreflightConstructors([]string{"cli-role-smart-default", "cli-role-custom-child"}) {
		require.Empty(t, assertion.Error, "cliprovider.New/LanguageModel preflight must construct registered model %q: %s", assertion.Expected, assertion.Error)
		require.Equal(t, assertion.Expected, assertion.Actual, "cliprovider.New/LanguageModel preflight must preserve registered model ID")
	}
}

func runCLIProcess(t *testing.T, cwd string, mode string, args []string, extra map[string]string) ([]byte, string, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestExecuteHelperProcess$")
	cmd.Dir = cwd
	cmd.Env = overrideEnv(os.Environ(), map[string]string{
		cliE2EHelperEnv: "1", cliE2EArgsEnv: string(encoded), cliE2EModeEnv: mode,
	}, extra)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, runErr := cmd.Output()
	return stdout, stderr.String(), runErr
}

func overrideEnv(env []string, values ...map[string]string) []string {
	overrides := make(map[string]string)
	for _, group := range values {
		for k, v := range group {
			overrides[strings.ToUpper(k)] = v
		}
	}
	out := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[strings.ToUpper(key)]; !replace {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}
