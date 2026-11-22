package sessclean

import (
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
)

func TestMain(m *testing.M) {
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	rushlog.Setup("", false)
	cmdtest.Main(m)
}
