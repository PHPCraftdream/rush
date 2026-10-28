package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestGlobalConfigPathsAreIsolatedFromGlobalDir(t *testing.T) {
	if _, ok := os.LookupEnv(config.GlobalDirEnv); ok {
		t.Fatalf("%s must be unset in the agent test process", config.GlobalDirEnv)
	}
	for _, name := range []string{"RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME", "RUSH_GLOBAL_DATA", "XDG_DATA_HOME", "RUSH_CACHE_DIR"} {
		got := os.Getenv(name)
		if got == "" || !strings.HasPrefix(strings.ToLower(got), strings.ToLower(os.TempDir())) {
			t.Errorf("%s = %q, want isolated temporary path", name, got)
		}
	}

	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	for _, path := range []string{config.GlobalConfig(), config.GlobalConfigData()} {
		dir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			dir = filepath.Dir(path)
		}
		if !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(tmp)) {
			t.Errorf("global config path %q is outside temporary root %q", path, tmp)
		}
	}
}
