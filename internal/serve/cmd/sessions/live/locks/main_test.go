package sesslocks

import (
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
)

func TestMain(m *testing.M) {
	cmdtest.Main(m)
}

// The tests drive the package-private template commands directly; their flags
// are registered by the factories (once), which only the cmd root called
// before the package move.
func init() {
	_ = NewSessionsReapCmd(nil)
	_ = NewSessionsLocksCmd(nil)
	_ = NewSessionsKillCmd(nil, nil, nil)
	// The purge provider opens the app through cmdkit.SetupAppLite, which
	// needs the config option builder package cmd's init used to install,
	// and a pre-initialised logger.
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	rushlog.Setup("", false)
}
