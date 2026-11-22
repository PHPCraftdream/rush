package sesslocks

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
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
}
