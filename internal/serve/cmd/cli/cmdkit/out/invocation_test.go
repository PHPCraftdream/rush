package cliout

import (
	"testing"

	"github.com/spf13/cobra"
)

func installInvocationTestCommands(t *testing.T) {
	t.Helper()
	oldRoot, oldAPI, oldTokens, oldServe := rootCmd, apiCmd, tokensCmd, serveCmd
	root := &cobra.Command{Use: "rush"}
	root.PersistentFlags().String("cwd", "", "working directory")
	root.PersistentFlags().String("name", "", "name")
	api := &cobra.Command{Use: "api"}
	tokens := &cobra.Command{Use: "tokens"}
	tokens.AddCommand(&cobra.Command{Use: "list"})
	serve := &cobra.Command{Use: "serve"}
	root.AddCommand(api, tokens, serve, &cobra.Command{Use: "run"})
	SetInvocationCommands(root, api, tokens, serve)
	t.Cleanup(func() { SetInvocationCommands(oldRoot, oldAPI, oldTokens, oldServe) })
}

func TestIsServeInvocationCommandParsing(t *testing.T) {
	installInvocationTestCommands(t)
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

func TestTokensInvocationIsCommandScoped(t *testing.T) {
	installInvocationTestCommands(t)
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
