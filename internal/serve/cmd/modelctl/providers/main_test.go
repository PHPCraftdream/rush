package providers

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
)

func TestMain(m *testing.M) {
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	NewProvidersCmd()
	cmdtest.Main(m)
}
