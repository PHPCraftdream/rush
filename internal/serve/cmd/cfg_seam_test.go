package cmd

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
)

// Commands that live outside this package (run, sessions fork/cost/diff/grep/tree)
// open the app through cmdkit directly, so the option builder has to be in place
// before any of them runs, not only after one of this package's own setup
// wrappers. Without it every `rush run` panics in cmdkit.InitCommandConfig.
func TestCommandLoadOptionsSeamIsInstalledAtInit(t *testing.T) {
	if !cmdkit.LoadOptionsInstalled() {
		t.Fatal("cmdkit command load options are not installed at package init: moved commands call cmdkit.InitCommandConfig directly and would dereference nil")
	}
}
