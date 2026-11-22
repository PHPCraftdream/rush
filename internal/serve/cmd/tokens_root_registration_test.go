package cmd

import (
	"path/filepath"
	"testing"
)

// TestTokensCommandRegisteredOnRoot pins the rush tokens subtree staying
// registered on the real root after lane M6 moved the command to
// internal/serve/cmd/admin/settings. Identity with the settings template
// (the former assertion) is obsolete: NewRootCmd builds fresh children per
// call, so the registered subtree is asserted structurally.
func TestTokensCommandRegisteredOnRoot(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	command, _, err := rootCmd.Find([]string{"tokens"})
	if err != nil || command == nil || command.Name() != "tokens" || command.CommandPath() != "rush tokens" {
		t.Fatalf("root lookup for tokens = %v, %v; want registered tokens command", command, err)
	}
}

// TestTokensInvocationIsCommandScoped is a full-tree dispatch test and stays
// in package cmd: the local settings harness lacks the real rush run command
// and can weaken the false cases.
func TestTokensInvocationIsCommandScoped(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"tokens", "list"}, true},
		{[]string{"run", "tokens"}, false},
		{[]string{"--name", "tokens", "run"}, false},
		{[]string{"--", "tokens"}, false},
	} {
		if got := isTokensInvocation(tc.args); got != tc.want {
			t.Errorf("isTokensInvocation(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// tokenTestEnv keeps the moved tokens CLI e2e file's global-env isolation
// for the root-registration check (same body as the settings copy).
func tokenTestEnv(t *testing.T, base string) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global-dir"))
	for key, sub := range map[string]string{
		"RUSH_GLOBAL_DATA": "data", "RUSH_GLOBAL_CONFIG": "config",
		"XDG_CONFIG_HOME": "xdg-config", "XDG_DATA_HOME": "xdg-data",
	} {
		t.Setenv(key, filepath.Join(base, sub))
	}
}
