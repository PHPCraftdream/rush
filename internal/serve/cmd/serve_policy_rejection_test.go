package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

func writeServePolicyConfig(t *testing.T, globalDir, body string) {
	t.Helper()
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteServeDuplicateListenerAddresses(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "isolated global")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	writeServePolicyConfig(t, globalDir, `{"server":{"listeners":[{"addr":"127.0.0.1:8181"},{"addr":"127.0.0.1:8181"}]}}`)
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd)
	if code != 2 || stdout != "" || !strings.Contains(normalizedServeError(strings.ToLower(stderr)), normalizedServeError("duplicate address")) || !strings.Contains(normalizedServeError(strings.ToLower(stderr)), normalizedServeError("127.0.0.1:8181")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecuteServeUISurfaceRejected(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "isolated global")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	writeServePolicyConfig(t, globalDir, `{"server":{"listeners":[{"addr":"127.0.0.1:8182","surfaces":["rest","ui"]}]}}`)
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd)
	if code != 2 || stdout != "" || !strings.Contains(normalizedServeError(strings.ToLower(stderr)), normalizedServeError("ui surface")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
