package agent

// Regression test for task #576/P1-3: buildTools used to read config live
// (many separate c.cfg.Config() calls) instead of the pinned snapshot
// buildAgent captures once for the whole build. This proves buildTools
// itself now answers strictly from the *config.Config parameter it is
// given, never re-reading c.cfg.Config() live -- the same shape of gap
// p554_pinned_config_test.go closed for prompt.Build and
// p436_config_snapshot_threading_test.go closed for
// workerSubAgentActive/buildAgentModels.
//
// REVERT CHECK: in buildTools (coordinator_tools.go), change
// "if len(cfg.MCP) > 0 {" back to "if len(c.cfg.Config().MCP) > 0 {"
// (ignoring the cfg parameter) and
// TestBuildTools_UsesThePinnedConfigNotTheLiveStore fails: the MCP-gated
// tools appear/disappear based on the LIVE store instead of the pinned cfg
// argument.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// snapshotConfigWithMCP makes a copy of cfg that is independent of it for
// MCP -- a plain *cfg is NOT enough, and getting that wrong is exactly how
// the first attempt at the prompt-seam version of this test
// (p554_pinned_config_test.go) came out vacuous: config.ConfigStore.Config()
// returns a SHARED pointer, and MCP is a map, so a shallow copy still
// aliases it. The "pinned" and "live" configs would then be the same
// object and the test would compare a value with itself.
func snapshotConfigWithMCP(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.MCP = make(config.MCPs, len(cfg.MCP))
	for k, v := range cfg.MCP {
		clone.MCP[k] = v
	}
	return &clone
}

// TestBuildTools_UsesThePinnedConfigNotTheLiveStore covers P1-3 of the
// 2026-08-18 release-readiness review (task #576).
//
// The probe is whether list_mcp_resources/read_mcp_resource appear in the
// built tool slice, gated purely by len(cfg.MCP) > 0 -- no filesystem or
// other source feeds that decision, unlike (say) Options.ContextPaths,
// which the prompt-seam test's own doc warns is USELESS as a probe because
// context files are ALSO discovered from the working directory regardless
// of which generation named them.
func TestBuildTools_UsesThePinnedConfigNotTheLiveStore(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")

	// Generation 0, pinned by the caller exactly as buildAgent pins the
	// config it passes into buildTools: no MCP servers configured.
	live := coord.cfg.Config()
	pinned := snapshotConfigWithMCP(live)
	require.Empty(t, pinned.MCP, "pinned generation must start with no MCP servers")

	// Generation N+1 lands while the caller is still mid-construction: an
	// MCP server is added to the LIVE config, but NOT to the pinned copy.
	live.MCP = config.MCPs{
		"probe-server": config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "does-not-exist-and-is-never-invoked",
		},
	}
	require.NotEmpty(t, live.MCP, "live generation must have gained an MCP server")

	built, err := coord.buildTools(t.Context(), pinned, coderCfg, false)
	require.NoError(t, err)

	names := make(map[string]bool, len(built))
	for _, tool := range built {
		names[tool.Info().Name] = true
	}

	require.False(t, names["list_mcp_resources"],
		"buildTools read the live store instead of the pinned config: list_mcp_resources appeared even though the PINNED generation had no MCP servers")
	require.False(t, names["read_mcp_resource"],
		"buildTools read the live store instead of the pinned config: read_mcp_resource appeared even though the PINNED generation had no MCP servers")
}

// TestBuildTools_PinnedConfigIsHonouredWhenTheLiveStoreHasNoMCP is the other
// direction, ruling out a buildTools that simply ignores the cfg parameter
// (e.g. always answering false regardless of what's passed). Here the LIVE
// config has NO MCP servers; only the pinned generation does. The MCP tools
// must still be built from the pinned value.
func TestBuildTools_PinnedConfigIsHonouredWhenTheLiveStoreHasNoMCP(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")

	live := coord.cfg.Config()
	live.MCP = config.MCPs{
		"probe-server": config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "does-not-exist-and-is-never-invoked",
		},
	}
	pinned := snapshotConfigWithMCP(live)
	require.NotEmpty(t, pinned.MCP, "pinned generation must have the MCP server")

	// The reload removes the MCP server entirely from the live config.
	live.MCP = config.MCPs{}

	built, err := coord.buildTools(t.Context(), pinned, coderCfg, false)
	require.NoError(t, err)

	names := make(map[string]bool, len(built))
	for _, tool := range built {
		names[tool.Info().Name] = true
	}

	require.True(t, names["list_mcp_resources"],
		"the pinned generation's MCP server was dropped because buildTools consulted the live store, which no longer has one")
	require.True(t, names["read_mcp_resource"],
		"the pinned generation's MCP server was dropped because buildTools consulted the live store, which no longer has one")
}
