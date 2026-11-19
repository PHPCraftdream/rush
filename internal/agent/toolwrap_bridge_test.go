package agent

// Shared test fixtures for the root tests that exercise the wrappers now
// living in internal/agent/runtime/policy/toolwrap through the bridge
// (toolwrap_bridge.go). The unit tests for the wrappers themselves moved
// with them into the toolwrap package; these helpers stay only for the
// staying root integration tests.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// stubTool returns whatever it is told to.
type stubTool struct {
	name string
	resp fantasy.ToolResponse
	err  error
}

func (s stubTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: s.name} }
func (s stubTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (s stubTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (s stubTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return s.resp, s.err
}

// newRunner builds a hooks.Runner from a single HookConfig, running the
// config-loader path that compiles the matcher regex.
func newRunner(t *testing.T, cmd string) *hooks.Runner {
	t.Helper()
	cfg := &config.Config{
		Hooks: map[string][]config.HookConfig{
			hooks.EventPreToolUse: {{Command: cmd}},
		},
	}
	require.NoError(t, cfg.ValidateHooks())
	return hooks.NewRunner(cfg.Hooks[hooks.EventPreToolUse], t.TempDir(), t.TempDir())
}

func cacheOpts() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

func newProviderOptionsTestTool(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "test tool",
		func(context.Context, providerOptionsTestParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		})
}

type providerOptionsTestParams struct {
	Value string `json:"value" description:"anything"`
}
