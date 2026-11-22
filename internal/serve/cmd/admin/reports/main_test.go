// TestMain for the reports package: installs the shared testguard
// isolation (including the Ktav auto-migration gate) before the moved
// white-box tests run, mirroring internal/serve/cmd/cli/climain_test.go
// and the other M3–M5 moved-command mains. Per-test env isolation and
// logger pre-initialisation remain in the individual test files.
package reports

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	cmdtest.Main(m)
}
