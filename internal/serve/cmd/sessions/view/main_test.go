package sessview

import (
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
)

// TestMain registers the moved commands' flags exactly once (the white-box
// tests drive the package-level template commands, whose flags the
// constructors register), installs the cmdkit config option builder and the
// once-only global logger (both were installed by package cmd's init phase
// before the move; rushlog.Setup pre-init also avoids Windows log-handle
// leaks under parallel tests), then applies the shared cmdtest isolation.
func TestMain(m *testing.M) {
	_ = NewSessionsShowCmd(cmdkit.SetupApp)
	// Config option builder the moved setupApp path needs (package cmd's
	// init no longer runs in this binary).
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	rushlog.Setup("", false)
	cmdtest.Main(m)
}
