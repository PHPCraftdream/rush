package modeluse

import (
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/spf13/cobra"
)

var (
	ownerUseCmd   *cobra.Command
	ownerStateCmd *cobra.Command
	ownerBumpCmd  *cobra.Command
)

func setupOwnerCommands(t *testing.T) (globalPath string) {
	t.Helper()
	globalPath, ownerUseCmd = isolatedModelsEnv(t)
	ownerStateCmd = NewStateCmd()
	ownerBumpCmd = NewBumpCmd()
	root := &cobra.Command{Use: "rush"}
	parent := &cobra.Command{Use: "models"}
	parent.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("cwd", "", "")
	root.AddCommand(parent)
	parent.AddCommand(ownerUseCmd, ownerStateCmd, ownerBumpCmd)
	// --data-dir scope restores the pre-move fixture per command rather than
	// one blanket value: use/state keep the harness's tmp root (identical to
	// what isolatedModelsEnv gave every leaf before this package existed), while
	// bump gets the global data dir itself — HEAD's setupBumpEnv passed
	// os.Getenv("RUSH_GLOBAL_DATA") there, and models_bump_test.go's per-step
	// db.Release(filepath.Dir(globalPath)) (internal/store/db keys the pool by
	// <data-dir>/rush_store_v1.db) only releases the connection it actually
	// opened when bump's data dir IS filepath.Dir(globalPath). Each command
	// keeps its own value because ensureRootFlagStandIns registers the leaf's
	// data-dir flag first, and pflag/cobra AddFlagSet skips names that already
	// exist, so the shared parent persistent flag never overrides it.
	tmp := filepath.Dir(filepath.Dir(globalPath))
	dataDir := filepath.Dir(globalPath)
	for _, owned := range []struct {
		cmd *cobra.Command
		dir string
	}{
		{ownerUseCmd, tmp},
		{ownerStateCmd, tmp},
		{ownerBumpCmd, dataDir},
	} {
		ensureRootFlagStandIns(owned.cmd, owned.dir)
		owned.cmd.SetContext(ownerUseCmd.Context())
		owned.cmd.Flags().AddFlagSet(parent.PersistentFlags())
	}
	return globalPath
}

func setupOwnerBumpEnv(t *testing.T) (globalPath string) { return setupOwnerCommands(t) }

func resetOwnerStateFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	if f := cmd.Flags().Lookup("json"); f != nil {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	cmd.SetArgs(nil)
}

func resetOwnerBumpFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	for _, name := range []string{"global", "local"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	cmd.SetArgs(nil)
}

func ktavGateOn(t *testing.T) { cmdtest.KtavGateOn(t) }
