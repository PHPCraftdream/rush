package clibatchtest

import (
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	cmdtest.Main(m)
}
