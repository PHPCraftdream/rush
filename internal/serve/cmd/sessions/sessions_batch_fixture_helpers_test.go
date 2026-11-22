package sessions

// Batch delegation fixtures use the shared command test seed APIs; these
// wrappers preserve their established local names without exporting test-only
// production APIs.

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func seedCmdBatchDelegation(t *testing.T, conn *sql.DB, owner, host, callID, childID string) (string, string) {
	t.Helper()
	return cmdtest.SeedCmdBatchDelegation(t, conn, owner, host, callID, childID)
}

func seedCmdBatchRootFor(t *testing.T, a *app.App, owner, host, callID string) string {
	t.Helper()
	return cmdtest.SeedCmdBatchRootFor(t, a, owner, host, callID)
}
