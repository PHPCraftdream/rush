// TestMain for the diag package (white-box). rushlog.Setup("", false)
// pre-initialises the once-only global logger so no test-run opens a
// lumberjack handle under a t.TempDir — on Windows the open handle would
// lock the file and break TempDir cleanup.
package diag

import (
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	NewLogsCmd()
	rushlog.Setup("", false)
	cmdtest.Main(m)
}
