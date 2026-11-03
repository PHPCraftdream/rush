package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
)

func runServeExecuteProcess(t *testing.T, base string, args ...string) (int, string, string) {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
	cmd.Env = cleanExecuteHelperEnv(os.Environ())
	globalDir := filepath.Join(base, "isolated global")
	cmd.Env = append(cmd.Env, executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+filepath.Join(base, "data"), "RUSH_GLOBAL_CONFIG="+filepath.Join(base, "config"), "XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(base, "xdg-data"), "HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	got := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			got = exit.ExitCode()
		} else {
			t.Fatalf("process: %v", err)
		}
	}
	return got, stdout.String(), stderr.String()
}

func TestExecuteServeWithoutCwd(t *testing.T) {
	code, stdout, stderr := runServeExecuteProcess(t, t.TempDir(), "serve")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "requires an explicit --cwd") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServePrintConfigStdout(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	want := "address: 127.0.0.1:8080 (source: default)\nsurfaces: rest,ws (source: default)\nallow_cleartext: false (source: default)\nallow_admin: false (source: default)\nallow_auto_approve: false (source: default)\nallowed_origins:  (source: default)\ntoken_ids:  (source: default)\nlimits.rps: 0 (source: default)\nlimits.burst: 0 (source: default)\nlimits.max_conns_per_token: 0 (source: default)\nlimits.max_body: 0 (source: default)\ntls.cert:  (source: default)\ntls.key:  (source: default)\ntls.client_ca:  (source: default)\ntls.min_version:  (source: default)\nroute: full (source: default)\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServePrintConfigAddrOnlyProvenance(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	globalDir := filepath.Join(base, "isolated global")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(globalDir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8443"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"address: 127.0.0.1:8443 (source: global config)",
		"surfaces: rest,ws (source: default)",
		"allow_cleartext: false (source: default)",
		"allow_admin: false (source: default)",
		"limits.rps: 0 (source: default)",
		"tls.cert:  (source: default)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestExecuteServePrintConfigAllFieldProvenance(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	configDir := filepath.Join(base, "isolated global", "config")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"server":{"listeners":[{"addr":"127.0.0.1:8443","surfaces":["rest"],"allow_cleartext":true,"allow_admin":true,"allow_auto_approve":true,"allowed_origins":["https://example.test"],"tokens":["token-a"],"limits":{"rps":2.5,"burst":7,"max_conns_per_token":3,"max_body":"1KiB"},"tls":{"cert":"","key":"","client_ca":"","min_version":""}}]}}`
	if err := os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	want := "address: 127.0.0.1:8443 (source: global config)\nsurfaces: rest (source: global config)\nallow_cleartext: true (source: global config)\nallow_admin: true (source: global config)\nallow_auto_approve: true (source: global config)\nallowed_origins: https://example.test (source: global config)\ntoken_ids: token-a (source: global config)\nlimits.rps: 2.5 (source: global config)\nlimits.burst: 7 (source: global config)\nlimits.max_conns_per_token: 3 (source: global config)\nlimits.max_body: 1024 (source: global config)\ntls.cert:  (source: global config)\ntls.key:  (source: global config)\ntls.client_ca:  (source: global config)\ntls.min_version:  (source: global config)\nroute: full (source: default)\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServePrintConfigFlagProvenance(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	configDir := filepath.Join(base, "isolated global", "config")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"server":{"listeners":[{"addr":"127.0.0.1:8443","surfaces":["rest"],"allow_admin":true}]}}`
	if err := os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--listen", "127.0.0.1:9000", "--allow-cleartext")
	want := "address: 127.0.0.1:9000 (source: flag)\nsurfaces: rest (source: global config)\nallow_cleartext: true (source: flag)\nallow_admin: true (source: global config)\nallow_auto_approve: false (source: default)\nallowed_origins:  (source: default)\ntoken_ids:  (source: default)\nlimits.rps: 0 (source: default)\nlimits.burst: 0 (source: default)\nlimits.max_conns_per_token: 0 (source: default)\nlimits.max_body: 0 (source: default)\ntls.cert:  (source: default)\ntls.key:  (source: default)\ntls.client_ca:  (source: default)\ntls.min_version:  (source: default)\nroute: full (source: default)\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServeEmptyTokenStoreGuidance(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd)
	wantGuidance := fmt.Sprintf("rush tokens create --name <n> --scope run --cwd %q --global-dir %q", cwd, filepath.Join(base, "isolated global"))
	if code != 2 || stdout != "" || !strings.Contains(normalizedServeError(stderr), normalizedServeError(wantGuidance)) {
		t.Fatalf("code=%d stdout=%q stderr=%q, want guidance %q", code, stdout, stderr, wantGuidance)
	}
}

func TestExecuteServeListenerConfigIOError(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "isolated global")
	if err := os.MkdirAll(filepath.Join(globalDir, "config", "rush.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", base)
	if code != 1 || stdout != "" || !strings.Contains(normalizedServeError(strings.ToLower(stderr)), normalizedServeError("read global listener config")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServeListenerConfigParseError(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "isolated global")
	if err := os.MkdirAll(filepath.Join(globalDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config", "rush.json"), []byte(`{"server":{"listeners":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", base)
	if code != 2 || stdout != "" || !strings.Contains(normalizedServeError(strings.ToLower(stderr)), normalizedServeError("decode global listener config")) || !strings.Contains(normalizedServeError(stderr), normalizedServeError("rush.json")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServeListenerValidationError(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "isolated global")
	if err := os.MkdirAll(filepath.Join(globalDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config", "rush.json"), []byte(`{"server":{"listeners":[{"addr":"not-an-address"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", base)
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServeBadArguments(t *testing.T) {
	for _, args := range [][]string{{"serve", "unexpected"}, {"serve", "--cwd"}} {
		code, stdout, stderr := runServeExecuteProcess(t, t.TempDir(), args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestExecuteServeTokenStoreReadErrorGuidance(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "isolated global", "data", "api-tokens.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", base)
	if code != 1 || stdout != "" || !strings.Contains(normalizedServeError(stderr), normalizedServeError("rush tokens create --name <n> --scope run")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestIsServeInvocationCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"serve"}, true},
		{[]string{"serve", "--cwd", "/tmp"}, true},
		{[]string{"serve", "--unknown"}, true},
		{[]string{"serve", "unexpected"}, true},
		{[]string{"tokens", "list"}, false},
		{[]string{"unknown"}, false},
		{nil, false},
	} {
		if got := isServeInvocation(tc.args); got != tc.want {
			t.Errorf("isServeInvocation(%q)=%t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestExecuteServeCobraUsageExitCode(t *testing.T) {
	code, stdout, stderr := runServeExecuteProcess(t, t.TempDir(), "serve", "--unknown")
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
