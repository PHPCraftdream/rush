package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/testguard"
)

// Tests here reach config.Init/app.New; without this they read, migrate and
// rewrite the operator's real global config.
func TestMain(m *testing.M) { testguard.Main(m) }

func TestGlobalConfigPathsAreIsolated(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	for _, p := range []string{config.GlobalConfig(), config.GlobalConfigData()} {
		resolved, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			resolved = filepath.Dir(p)
		}
		if !strings.HasPrefix(strings.ToLower(resolved), strings.ToLower(tmp)) {
			t.Fatalf("global config path %s is outside the temp dir %s", p, tmp)
		}
	}
}
