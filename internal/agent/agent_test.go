package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/joho/godotenv/autoload"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)

	// Isolate every test in this binary from the real host global config
	// (task #456, following up on task #450's test-speed investigation).
	// Several tests in this package reach config.Init with an empty
	// dataDir (coderAgent in common_test.go, used by e.g. TestCoderAgent
	// and friends, plus config.Init call sites in coordinator_test.go,
	// interrupt_test.go, p0_2_fault_injection_test.go, and others) --
	// without this, that falls through to the real
	// GlobalConfigData()/GlobalConfig() resolution paths: the operator's
	// actual ~/.config/crush/crush.json, complete with real provider API
	// keys and any MCP servers it configures. A test that reaches
	// app-level config resolution can then try to open real network
	// connections to those servers -- internal/cmd/models_use_test.go's
	// isolatedModelsEnv documents observing exactly this hang a test run
	// for 9+ minutes until the panic-timeout. This fork's own CLAUDE.md
	// documents the same hazard and the same fix: CRUSH_GLOBAL_DATA and
	// CRUSH_GLOBAL_CONFIG are two SEPARATE resolution paths (not aliases
	// of each other), and BOTH must be pointed at throwaway directories
	// for genuine isolation. Set process-wide here, once, before any test
	// in this binary can run -- t.Setenv isn't usable outside a *testing.T,
	// and this needs to apply uniformly regardless of which test runs
	// first or whether tests run in parallel.
	globalTmp, err := os.MkdirTemp("", "crush-agent-test-global-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to create isolated global config dir: %v\n", err)
		os.Exit(1)
	}
	dataDir := filepath.Join(globalTmp, "data")
	configDir := filepath.Join(globalTmp, "config")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		os.RemoveAll(globalTmp)
		fmt.Fprintf(os.Stderr, "TestMain: failed to create %s: %v\n", dataDir, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		os.RemoveAll(globalTmp)
		fmt.Fprintf(os.Stderr, "TestMain: failed to create %s: %v\n", configDir, err)
		os.Exit(1)
	}
	os.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	os.Setenv("XDG_DATA_HOME", dataDir)
	os.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	os.Setenv("XDG_CONFIG_HOME", configDir)

	m.Run()
	os.RemoveAll(globalTmp)
}
