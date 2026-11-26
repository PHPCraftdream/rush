package cfgstore

// TestMain mirrors the root package's testmain (load_test.go): the store
// engine resolves machine-global paths through datadir, whose testguard
// rejects unisolated test processes, and the log/provider seams keep leaf
// tests deterministic. Moved-package support file for the store move.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: isolate global state: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Setenv("RUSH_PROVIDER_CACHE_TTL", "0")
	code := m.Run()
	cleanup()
	os.Exit(code)
}
