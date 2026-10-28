package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/testguard"
)

func TestGlobalPathResolutionRequiresTestguard(t *testing.T) {
	for name, resolve := range map[string]func(){
		"GlobalDir":        func() { GlobalDir() },
		"GlobalConfig":     func() { GlobalConfig() },
		"GlobalConfigData": func() { GlobalConfigData() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RUSH_TESTGUARD_ROOT", "")
			defer func() {
				if got := recover(); got == nil {
					t.Fatal("global path resolution did not reject unisolated test state")
				} else if !strings.Contains(fmt.Sprint(got), "config: global state resolved in test without testguard isolation") {
					t.Errorf("panic = %v, want testguard isolation diagnostic", got)
				}
			}()
			resolve()
		})
	}
}

func TestMain(m *testing.M) {
	// The command-substitution helper runs this binary with a minimal
	// environment (no temp dir) and never touches config.
	if os.Getenv(resolveCommandHelperEnv) == "1" {
		os.Exit(m.Run())
	}
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: isolate global state: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Fork patch (orchestrator UX): disable the providers-cache TTL
	// for the test pkg so the catwalk/hyper sync tests that exercise
	// the network-fetch path are not short-circuited by the new
	// time-based skip. TTL behaviour itself is covered by dedicated
	// tests that re-enable a non-zero value via t.Setenv.
	os.Setenv("RUSH_PROVIDER_CACHE_TTL", "0")

	// Stub CLI detection so the always-on local-CLI provider is not injected
	// from whatever binaries (claude/gemini/npx) happen to be on the runner's
	// PATH. These tests assert exact provider sets and must be deterministic
	// across environments (clean CI vs a dev box with the CLIs installed).
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }

	code := m.Run()
	cleanup()
	os.Exit(code)
}
