package itest

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
)

// The scenarios here reach config and the session store; without isolation
// they would read and rewrite the operator's real global state. The setup
// mirrors the TestMain of the packages the scenarios came from.
func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: isolate global state: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Deterministic provider sets: no providers-cache TTL and no local CLIs
	// injected from whatever binaries happen to be on PATH.
	os.Setenv("RUSH_PROVIDER_CACHE_TTL", "0")
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	code := m.Run()
	heartbeat.QuiesceForTest()
	cleanup()
	os.Exit(code)
}
