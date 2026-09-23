package agent

// Regression (2026-09-23): without a FolderScope, fs_* tools were still in
// the default toolset but built with the zero scope, so every path --
// even the worktree root -- came back "outside every folder scope". The
// model burned ~20 minutes of a 60-minute run on those refusals (five
// sessions timed out). Unscoped toolsets must not offer fs_* at all.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

var allFsToolNames = []string{"fs_list", "fs_find", "fs_grep", "fs_read", "fs_write", "fs_replace", "fs_write_lines", "fs_delete"}

func TestUnscopedToolsets_OmitFsTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true /* worker configured */)
	cfgSnap, _ := coord.cfg.Snapshot()
	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok)
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok)

	cases := []struct {
		name     string
		ctx      context.Context
		agent    config.Agent
		subAgent bool
	}{
		{"coder, no CallOptions", t.Context(), coderCfg, false},
		{"coder, CallOptions without scope", WithCallOptions(t.Context(), &CallOptions{}), coderCfg, false},
		{"plain sub-agent", WithCallOptions(t.Context(), &CallOptions{}), taskCfg, true},
		{"worker sub-agent", WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart}), taskCfg, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := pinnedToolNames(mustBuildTools(t, coord, tc.ctx, cfgSnap, tc.agent, tc.subAgent))
			assertToolsOmit(t, names, allFsToolNames...)
			assertToolsContain(t, names, "view", "grep", "glob")
		})
	}

	t.Run("scoped call still gets granted fs tools", func(t *testing.T) {
		scope := newFolderScope(t, env.workingDir, permission.FileOpRead, permission.FileOpList)
		ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})
		names := pinnedToolNames(mustBuildTools(t, coord, ctx, cfgSnap, coderCfg, false))
		assertToolsContain(t, names, "fs_read", "fs_list")
	})
}

// The shared toolset published by UpdateModels serves every unscoped run.
func TestUpdateModels_GlobalToolsetOmitsFsTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	require.NoError(t, coord.UpdateModels(t.Context()))
	names, _ := rec.snapshot()
	require.Len(t, names, 1)
	assertToolsOmit(t, names[0], allFsToolNames...)
	assertToolsContain(t, names[0], "view")
}
