package install

// TestMain: isolate the operator's real global config for this package the
// same way internal/serve/cmd's main_test.go did before the installer sources
// moved here (claude global-scope and cli-refresh tests rely on it; no test
// in this package may read real config).

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) { cmdtest.Main(m) }
