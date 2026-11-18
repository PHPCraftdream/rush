package wtree

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	NewWorktreeCmd()
	cmdtest.Main(m)
}
