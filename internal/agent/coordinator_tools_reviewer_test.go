package agent

// Reviewer-role tool gating: the per-call read-only filter
// (applyCallReviewerReadOnly) that keeps a reviewer-role call to the
// read-only tools. The buildTools tests mirror coordinator_tool_pinning_test.go's
// FolderScope tests (same fixtures, same "leak" style assertions).

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildReviewerToolNames runs the real buildTools for the given agent config
// and returns the resulting tool names.
func buildReviewerToolNames(t *testing.T, coord *coordinator, agentCfg config.Agent, ctx context.Context) []string {
	t.Helper()

	built, err := coord.buildTools(ctx, coord.cfg.Config(), agentCfg, false)
	require.NoError(t, err)

	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	return names
}

// reviewerLeakedToolNames is everything a reviewer-role call must NOT hold:
// the write family, the command/exec tools, the delegation and network tools,
// MCP, and the fs_* write side.
var reviewerLeakedToolNames = []string{
	tools.BashToolName, tools.RunCommandToolName,
	tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName,
	tools.DownloadToolName, tools.FetchToolName,
	tools.TodosToolName, tools.AskQuestionToolName,
	AgentToolName, tools.JobKillToolName,
	tools.FSWriteToolName, tools.FSReplaceToolName, tools.FSWriteLinesToolName, tools.FSDeleteToolName,
	tools.InspectAgentToolName, tools.InjectAgentToolName, tools.StopAgentToolName,
	tools.SourcegraphToolName, tools.RushLogsToolName, tools.RushInfoToolName,
	tools.ListMCPResourcesToolName, tools.ReadMCPResourceToolName, tools.AgenticFetchToolName,
}

// assertNoReviewerLeak fails naming the specific tool that leaked, so a
// regression points at one tool instead of a diff of two slices.
func assertNoReviewerLeak(t *testing.T, names []string) {
	t.Helper()
	for _, name := range reviewerLeakedToolNames {
		assert.NotContains(t, names, name, "tool %q leaked into the reviewer-role toolset", name)
	}
}

// TestBuildTools_ReviewerRoleIsReadOnly pins the reviewer role's top-level
// toolset to the read-only names: everything that writes, executes,
// delegates, talks to the network, or reaches outside the folder scope is
// gone. Two cases, because the two per-call filters that shape a reviewer
// call are ordered:
//
//   - unscoped: the whole read-only list survives, including the legacy
//     single-target file tools.
//   - scoped (read + create grants): applyCallFolderScope has already run and
//     swapped the legacy file tools for the fs_* family, so
//     applyCallReviewerReadOnly -- which runs AFTER it -- sees that final
//     list. fs_read survives; fs_write, which the create grant re-added, is
//     dropped again. That is the whole reason the ordering matters.
func TestBuildTools_ReviewerRoleIsReadOnly(t *testing.T) {
	env := testEnv(t)
	// No worker: the reviewer filter is not a sub-agent filter, and
	// newWorkerToolTestCoordinator isolates the host's global config.
	coord := newWorkerToolTestCoordinator(t, env, false)

	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")

	t.Run("unscoped keeps the legacy read-only tools", func(t *testing.T) {
		ctx := WithCallOptions(t.Context(), &CallOptions{
			ModelRole: config.SelectedModelTypeReviewer,
		})

		names := buildReviewerToolNames(t, coord, coderCfg, ctx)

		for _, want := range []string{
			tools.ViewToolName, tools.GrepToolName, tools.GlobToolName, tools.LSToolName,
			tools.GitReadToolName, tools.ReadDelegationTranscriptToolName,
		} {
			assert.Contains(t, names, want, "a reviewer must keep the read-only tool %q", want)
		}
		assertNoReviewerLeak(t, names)
	})

	t.Run("scoped keeps only the granted fs read tools and drops fs_write", func(t *testing.T) {
		scope := newFolderScope(t, env.workingDir, permission.FileOpRead, permission.FileOpCreate)
		ctx := WithCallOptions(t.Context(), &CallOptions{
			ModelRole:   config.SelectedModelTypeReviewer,
			FolderScope: &scope,
		})

		names := buildReviewerToolNames(t, coord, coderCfg, ctx)

		// The scoped call's allowed set IS the granted fs_* family: no
		// unsourced tool survives two filters that both only remove.
		assert.Contains(t, names, tools.FSReadToolName,
			"fs_read is granted by the read op and must survive the read-only filter")
		assert.Contains(t, names, tools.ReadDelegationTranscriptToolName,
			"read_delegation_transcript is role plumbing, not a filesystem tool")

		// The legacy single-target file tools and git_read are stripped by
		// applyCallFolderScope for a scoped call -- that is the scope's
		// doing, not the reviewer filter's, and either way a reviewer must
		// not hold them here.
		for _, gone := range []string{
			tools.ViewToolName, tools.GlobToolName, tools.GrepToolName, tools.LSToolName,
			tools.GitReadToolName,
		} {
			assert.NotContains(t, names, gone,
				"tool %q must not reach a scoped reviewer call", gone)
		}
		assertNoReviewerLeak(t, names)
	})
}

// TestBuildTools_SmartRoleKeepsBash is the control: the read-only filter is
// gated on the per-call ModelRole, so an ordinary smart-role call keeps the
// full toolset (bash included). Without this the filter above could be an
// unconditional strip and the rest of the product would never notice.
func TestBuildTools_SmartRoleKeepsBash(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")

	ctx := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})

	names := buildReviewerToolNames(t, coord, coderCfg, ctx)

	assert.Contains(t, names, tools.BashToolName,
		"the read-only filter must only apply to the reviewer role; a smart-role call keeps bash")
	assert.Contains(t, names, tools.EditToolName, "a smart-role call keeps the write tools")
	assert.Contains(t, names, tools.WriteToolName, "a smart-role call keeps the write tools")
	assert.Contains(t, names, tools.ViewToolName, "a smart-role call keeps the read tools")
}
