package cliout_test

import (
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
)

func TestAPIExitCodeMapping(t *testing.T) {
	// Install the production command registry so this verifies API dispatch,
	// not just recognition of the error type in isolation.
	cli.NewRootCmd()
	for _, tc := range []struct {
		code int
		want int
	}{
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6}, {130, 130},
	} {
		err := cliout.NewExitCodeError(tc.code, errors.New("fixture"))
		got, ok := cliout.TokensExitCode([]string{"api", "models"}, err)
		if !ok || got != tc.want {
			t.Errorf("code %d mapped to %d, recognized=%t", tc.code, got, ok)
		}
	}
	if _, ok := cliout.ErrorExitCode(errors.New("untyped"), "api"); ok {
		t.Fatal("untyped error recognized as API code")
	}
	if _, ok := cliout.ErrorExitCode(cliout.NewExitCodeError(99, errors.New("invalid")), "api"); ok {
		t.Fatal("unsupported API code recognized")
	}
}
