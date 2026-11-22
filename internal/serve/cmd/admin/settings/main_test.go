// TestMain for the settings package (white-box). Registers the moved
// commands' flags exactly once (the white-box tests inspect package-level
// command vars' flags, which the constructors register), installs the
// cliout invocation-classification pointers against a harness root that
// carries the real tokens subtree (formerly package cmd's root_external_test
// bootstrap), pre-initialises the once-only global logger and the cmdkit
// config option builder (both were installed by package cmd's init phase),
// then applies the shared cmdtest isolation.
package settingsctl

import (
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) {
	NewMcpCmd()
	NewTokensCmd()
	NewLockCmd()
	NewDirsCmd()
	NewSchemaCmd()
	NewUpdateProvidersCmd()
	// Config option builder the moved setupAppLite path needs (package
	// cmd's init no longer runs in this binary).
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	// cliout invocation classification: the harness root carries the real
	// tokens command; api/serve are stubs (this package's tests never
	// classify those families, but the pointers must be non-nil).
	apiStub := &cobra.Command{Use: "api"}
	serveStub := &cobra.Command{Use: "serve"}
	cliout.SetInvocationCommands(tokenTestRootCmd, apiStub, tokensCmd, serveStub)
	rushlog.Setup("", false)
	cmdtest.Main(m)
}
