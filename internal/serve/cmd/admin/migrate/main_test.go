package migrate

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

// TestMain initializes the migrate command template's flags (formerly done
// during CLI-root setup) before running the package's white-box tests, many
// of which invoke migrateCmd.RunE directly and expect recursive/dry-run
// flags to exist.
func TestMain(m *testing.M) {
	NewMigrateCmd()
	cmdtest.Main(m)
}
