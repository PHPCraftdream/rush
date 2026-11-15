package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

type createdToken struct {
	Token  string          `json:"token"`
	Record config.APIToken `json:"record"`
}

func createTokenProcess(t *testing.T, base string, args ...string) createdToken {
	t.Helper()
	code, stdout, stderr := runServeExecuteProcess(t, base, append([]string{"tokens", "create", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("tokens create %v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	var created createdToken
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("create JSON %q: %v", stdout, err)
	}
	return created
}

func TestTokensCreateBatchBindingProcess(t *testing.T) {
	base := t.TempDir()
	created := createTokenProcess(t, base, "--name", "worker", "--batch", "--tools", "view,grep", "--tools", "grep", "--max-roots", "4", "--max-parallel", "3")
	binding := created.Record.Binding
	if binding.Kind != "batch" || binding.Batch == nil || binding.Pin != nil {
		t.Fatalf("binding = %#v", binding)
	}
	if got := strings.Join(binding.Batch.Tools, ","); got != "view,grep" {
		t.Fatalf("tools = %q, want exact names deduplicated in order", got)
	}
	if binding.Batch.MaxRoots != 4 || binding.Batch.MaxParallel != 3 {
		t.Fatalf("limits = %#v", binding.Batch)
	}

	code, stdout, stderr := runServeExecuteProcess(t, base, "tokens", "show", created.Record.ID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "batch (tools=view,grep, max_roots=4, max_parallel=3)") {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runServeExecuteProcess(t, base, "tokens", "list")
	if code != 0 || !strings.Contains(stdout, "batch (tools=view,grep") {
		t.Fatalf("list code=%d stdout=%q", code, stdout)
	}

	plain := createTokenProcess(t, base, "--name", "plain", "--batch")
	if b := plain.Record.Binding; b.Kind != "batch" || b.Batch == nil || len(b.Batch.Tools) != 0 || b.Batch.MaxRoots != 0 {
		t.Fatalf("a bare --batch must default every limit: %#v", b)
	}
}

func TestTokensCreateBatchUsageErrorsProcess(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"with a session pin", []string{"--batch", "--pin-session", "new"}, "cannot be combined with --pin-session"},
		{"with admin scope", []string{"--batch", "--scope", "admin"}, "cannot be combined with the admin scope"},
		{"wildcard tool", []string{"--batch", "--tools", "mcp_*"}, "no wildcards"},
		{"empty tool entry", []string{"--batch", "--tools", "view,,grep"}, "no wildcards"},
		{"negative roots", []string{"--batch", "--max-roots", "-1"}, "must not be negative"},
		{"negative parallel", []string{"--batch", "--max-parallel", "-2"}, "must not be negative"},
		{"tools without batch", []string{"--tools", "view"}, "require --batch"},
		{"roots without batch", []string{"--max-roots", "2"}, "require --batch"},
		{"parallel without batch", []string{"--max-parallel", "2"}, "require --batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			args := append([]string{"tokens", "create", "--name", "x"}, tc.args...)
			code, stdout, stderr := runServeExecuteProcess(t, base, args...)
			if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), strings.ToLower(tc.want)) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", code, stdout, stderr, tc.want)
			}
			// Nothing was stored.
			code, stdout, _ = runServeExecuteProcess(t, base, "tokens", "list")
			if code != 0 || strings.TrimSpace(stdout) != "" {
				t.Fatalf("a refused create must not store a token: code=%d stdout=%q", code, stdout)
			}
		})
	}
}

func TestServeRouteFlagProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "batch")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "route: batch (source: flag)") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	if code != 0 || !strings.Contains(stdout, "route: full (source: default)") {
		t.Fatalf("default route: code=%d stdout=%q", code, stdout)
	}
	code, stdout, stderr = runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "everything")
	if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), `invalid --route "everything"`) {
		t.Fatalf("invalid route: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// A route set in the global config file reaches the policy (and the printed
// configuration) like any other listener field.
func TestServeRouteFromConfigFileProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	configDir := filepath.Join(base, "isolated global", "config")
	for _, dir := range []string{cwd, configDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8443","route":"batch"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "route: batch (source: global config)") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// A flag overrides the file.
	code, stdout, _ = runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "full")
	if code != 0 || !strings.Contains(stdout, "route: full (source: flag)") {
		t.Fatalf("flag override: code=%d stdout=%q", code, stdout)
	}
}

// route batch and a listener pin exclude each other; the refusal happens
// before anything listens.
func TestServeRouteBatchWithPinsIsRefusedProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	created := createTokenProcess(t, base, "--name", "pinme")
	code, stdout, stderr := runServeExecuteProcess(t, base, "serve", "--cwd", cwd, "--route", "batch", "--pin-session", created.Record.ID+"=some-session")
	if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "route batch cannot be combined with session pins") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
