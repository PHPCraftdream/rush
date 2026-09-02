package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestFolderScope_PromptHintFollowsCallOptions verifies the context wiring
// end to end, from THIS package's side: WithCallOptions must derive the
// coder prompt's scoped-filesystem block from the very CallOptions that
// drives applyCallFolderScope's toolset — true for FolderScope != nil,
// false for an unscoped CallOptions — and withoutCallOptions (the global
// refresh path) must clear a stale hint. Rendered with the real embedded
// coder.md.tpl via coderPrompt.
func TestFolderScope_PromptHintFollowsCallOptions(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	workingDir := t.TempDir()
	store, err := config.Init(workingDir, "", false)
	require.NoError(t, err)

	fixedTime := func() time.Time {
		parsed, err := time.Parse("1/2/2006", "1/2/2026")
		require.NoError(t, err)
		return parsed
	}
	p, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir("/fixed/workdir"),
	)
	require.NoError(t, err)

	scope := newFolderScope(t, workingDir, permission.FileOpRead)

	render := func(ctx context.Context) string {
		out, err := p.Build(ctx, "smart-provider", "smart-model", store, store.Config(), false)
		require.NoError(t, err)
		return out
	}

	plain := render(context.Background())
	scopedCall := render(WithCallOptions(context.Background(), &CallOptions{FolderScope: &scope}))
	unscopedCall := render(WithCallOptions(context.Background(), &CallOptions{}))
	refreshed := render(withoutCallOptions(WithCallOptions(context.Background(), &CallOptions{FolderScope: &scope})))

	require.Contains(t, scopedCall, "<scoped_filesystem>",
		"a folder-scoped call's coder prompt must carry the scoped-filesystem block")
	require.Equal(t, plain, unscopedCall,
		"an unscoped CallOptions must render byte-identical to a no-CallOptions context")
	require.Equal(t, plain, refreshed,
		"withoutCallOptions must clear a stale scoped hint (global refresh must not render from a caller's scope)")
}
