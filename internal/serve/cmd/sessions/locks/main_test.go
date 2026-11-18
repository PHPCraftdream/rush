package sesslocks

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	cmdtest.Main(m)
}

// The reap tests drive the package-private template command directly; its
// flags are registered by the factory (once), which only the cmd root called
// before the package move.
func init() { _ = NewSessionsReapCmd(nil) }
