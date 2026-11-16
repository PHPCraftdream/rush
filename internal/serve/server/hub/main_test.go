package hub

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

// Tests here reach config.Init; without this they read, migrate and rewrite
// the operator's real global config. Verbatim copy of the package-server
// TestMain (design §4.4: testguard.Main has no local state).
func TestMain(m *testing.M) { testguard.Main(m) }
