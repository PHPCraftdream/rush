package config

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
)

func TestMain(m *testing.M) {
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

	exitVal := m.Run()
	os.Exit(exitVal)
}
