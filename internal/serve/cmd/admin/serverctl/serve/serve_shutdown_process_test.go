package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/serve"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
)

func TestExecuteServeShutdownFailureExitCode(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"serve", "--cwd", cwd})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process := platform.Command(ctx, executable, "-test.run=^TestServeShutdownFailureHelperProcess$")
	process.Env = cleanExecuteHelperEnv(os.Environ())
	process.Env = append(process.Env,
		"RUSH_TEST_SERVE_SHUTDOWN_HELPER=1",
		"RUSH_TEST_SERVE_SHUTDOWN_ARGS="+string(args),
		"RUSH_GLOBAL_DIR="+filepath.Join(base, "global"),
		"RUSH_GLOBAL_DATA="+filepath.Join(base, "data"),
		"RUSH_GLOBAL_CONFIG="+filepath.Join(base, "config"),
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
		"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
		"HOME="+filepath.Join(base, "home"),
		"USERPROFILE="+filepath.Join(base, "userprofile"),
	)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	processErr := process.Run()
	if exit, ok := processErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("process error=%v, stdout=%q stderr=%q; want exit 1", processErr, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(normalizedServeError(strings.ToLower(stderr.String())), normalizedServeError("injected shutdown failure")) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestServeShutdownFailureHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_TEST_SERVE_SHUTDOWN_HELPER") != "1" {
		return
	}
	serve.InjectShutdownFailure(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("process", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("RUSH_TEST_SERVE_SHUTDOWN_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	cli.Execute()
	os.Exit(0)
}

func normalizedServeError(value string) string {
	return strings.Join(strings.Fields(value), "")
}

const executeHelperEnv = "RUSH_TEST_EXECUTE_HELPER"

func cleanExecuteHelperEnv(env []string) []string {
	keys := []string{"RUSH_GLOBAL_DIR", "RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME", "USERPROFILE", executeHelperEnv, executeHelperEnv + "_ARGS"}
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, blocked := range keys {
			if strings.EqualFold(key, blocked) {
				remove = true
				break
			}
		}
		if !remove {
			clean = append(clean, entry)
		}
	}
	return clean
}
