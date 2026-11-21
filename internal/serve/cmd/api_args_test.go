package cmd

import (
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
)

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
