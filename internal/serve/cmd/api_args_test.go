package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
)

func TestRejectAPITokenArguments(t *testing.T) {
	const token = "rush_01234567.0123456789012345678901234567890123456789012"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "positional", args: []string{token}},
		{name: "flag value", args: []string{"--remote", token}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAPICmd()
			var positional []string
			if tc.name == "flag value" {
				if err := cmd.ParseFlags(tc.args); err != nil {
					t.Fatal(err)
				}
			} else {
				positional = tc.args
			}
			err := rejectAPITokenArguments(cmd, positional)
			if err == nil {
				t.Fatal("token-looking value accepted")
			}
			var coded *cliout.ExitCodeError
			if !errors.As(err, &coded) || coded.Code() != 2 {
				t.Fatalf("error = %v, want exit code 2", err)
			}
			if got := err.Error(); strings.Contains(got, token) {
				t.Fatalf("error leaked token: %q", got)
			}
		})
	}
}

func TestAPIRemoteArgumentValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{name: "missing token source", args: []string{"remote", "add", "prod", "--url", "https://example.test"}, want: 2},
		{name: "multiple token sources", args: []string{"remote", "add", "prod", "--url", "https://example.test", "--token-env", "TOKEN", "--token-file", "secret"}, want: 2},
		{name: "missing URL", args: []string{"remote", "add", "prod", "--token-env", "TOKEN"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAPICmd()
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("invalid invocation accepted")
			} else {
				var coded *cliout.ExitCodeError
				if !errors.As(err, &coded) || coded.Code() != tc.want {
					t.Fatalf("error = %v, want code %d", err, tc.want)
				}
			}
		})
	}
}

func TestAPIExitCodeMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want int
	}{
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6}, {130, 130},
	} {
		err := cliout.NewExitCodeError(tc.code, errors.New("fixture"))
		got, ok := tokensExitCode([]string{"api", "models"}, err)
		if !ok || got != tc.want {
			t.Errorf("code %d mapped to %d, recognized=%t", tc.code, got, ok)
		}
	}
	if _, ok := apiExitCode(errors.New("untyped")); ok {
		t.Fatal("untyped error recognized as API code")
	}
	if _, ok := apiExitCode(cliout.NewExitCodeError(99, errors.New("invalid"))); ok {
		t.Fatal("unsupported API code recognized")
	}
}

// Commands that live outside this package (run, sessions fork/cost/diff/grep/tree)
// open the app through cmdkit directly, so the option builder has to be in place
// before any of them runs, not only after one of this package's own setup
// wrappers. Without it every `rush run` panics in cmdkit.InitCommandConfig.
func TestCommandLoadOptionsSeamIsInstalledAtInit(t *testing.T) {
	if !cmdkit.LoadOptionsInstalled() {
		t.Fatal("cmdkit command load options are not installed at package init: moved commands call cmdkit.InitCommandConfig directly and would dereference nil")
	}
}
