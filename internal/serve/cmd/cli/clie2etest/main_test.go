package clie2etest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestCommandLoadOptionsSeamIsInstalledAtInit(t *testing.T) {
	if !cmdkit.LoadOptionsInstalled() {
		t.Fatal("cmdkit command load options are not installed at package init: moved commands call cmdkit.InitCommandConfig directly and would dereference nil")
	}
}

func TestMain(m *testing.M) {
	// Fake CLI binaries re-enter this package's test binary under the name
	// `claude`; skip package isolation/setup for those helper processes.
	base := strings.ToLower(filepath.Base(os.Args[0]))
	if base == "claude" || base == "claude.exe" {
		os.Exit(m.Run())
	}
	rushlog.Setup("", false)
	cmdtest.Main(m)
}
