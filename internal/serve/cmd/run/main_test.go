package run

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	NewRunCmd()
	cmdtest.Main(m)
}
