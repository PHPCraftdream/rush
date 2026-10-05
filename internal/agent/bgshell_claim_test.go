package agent

// R-BG-1 (#1126, docs/plans/2026-10-01-bg-shell-ledger.md): a SYNC bash call's
// background escape (SDK origin / Drain turns) must leave a durable bg_shell
// row behind, and persistBGShellCompletion must transition that row at the
// shell's completion.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// bgShellInnerTool is a stand-in for the inner bash tool's background escape:
// it answers with the BashResponseMetadata shape the real tool produces.
func bgShellInnerTool(shellID string) fantasy.AgentTool {
	return fantasy.NewAgentTool("bash", "Run a command", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse("Background shell started with ID: "+shellID),
			tools.BashResponseMetadata{Background: true, ShellID: shellID},
		), nil
	})
}

// REVERT CHECK: removing the `if t.name == tools.BashToolName && sync` claim
// block from t.run (async_tool.go) leaves no row -- this test FAILED (Get:
// sql.ErrNoRows). Restored the block; re-ran, passed.
func TestAsyncTool_SyncBashBackgroundEscapeClaimsBGShellRow(t *testing.T) {
	t.Parallel()
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: bgShellInnerTool("sh-1"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{"command":"sleep 30","run_in_background":true}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	row, err := registry.store.Get(context.Background(), "session", "sh-1")
	require.NoError(t, err, "the background escape must have claimed a bg_shell row before the response was delivered")
	require.Equal(t, string(session.JobKindBGShell), row.Kind)
	require.Equal(t, "running", row.State)
	require.Equal(t, int64(1), row.Announced)
}

// A store that cannot commit the claim fails the background start closed: the
// model gets an error result instead of a shell with no row.
//
// REVERT CHECK: removing the kill+error branch from
// claimBackgroundShellRow's error path (leaving the claim failure logged only)
// leaves this test FAILED (resp.IsError false: the escape answered "started"
// over a failed claim). Restored the branch; re-ran, passed.
func TestAsyncTool_ClaimFailureRefusesBackgroundStart(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	// Force the claim to fail deterministically: the shell id's key is already
	// held by a row with a DIFFERENT input (ErrAsyncJobInputMismatch).
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: "session", ToolCallID: "sh-x", Kind: session.JobKindCommand, Input: "different",
	})
	require.NoError(t, err)
	registry := newWorkLedger(nil)
	registry.store = store
	wrapped := &asyncTool{inner: bgShellInnerTool("sh-x"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{"command":"sleep 30","run_in_background":true}`})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a failed claim must fail the background start (fail-closed)")
	require.Contains(t, resp.Content, "refused")
}

// REVERT CHECK: dropping the transitionBGShellRow call from
// persistBGShellCompletion leaves the claimed row running forever -- this test
// FAILED (row.State "running", want "completed"). Restored the call; re-ran,
// passed.
func TestPersistBGShellCompletion_TransitionsClaimedRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	_, err := store.ClaimShell(ctx, "sess", "sh-9", false)
	require.NoError(t, err)

	// The slot decision runs as before (the fixture has no web config, so it
	// refuses; the row transition is what this test pins).
	_ = c.persistBGShellCompletion("sess", "sh-9", "Background job sh-9 finished", false)

	row, err := store.Get(ctx, "sess", "sh-9")
	require.NoError(t, err)
	require.Equal(t, "completed", row.State)
	require.Equal(t, "Background job sh-9 finished", row.ResultSummary.String)
	require.Equal(t, int64(1), row.Reacted, "the row itself is never debt")
	require.Equal(t, "done", row.Delivery, "the notice insert remains the wake channel; the row is never pulled")

	notices, err := store.ListSessionNotices(ctx, "sess")
	require.NoError(t, err)
	require.Len(t, notices, 1, "the bg_shell_done notice remains the wake channel")
	require.Equal(t, session.NoticeKindBGShellDone, notices[0].Kind)

	// A second completion (lost race, duplicate callback) must not move the
	// row again.
	c.persistBGShellCompletion("sess", "sh-9", "second", false)
	row2, err := store.Get(ctx, "sess", "sh-9")
	require.NoError(t, err)
	require.Equal(t, "Background job sh-9 finished", row2.ResultSummary.String, "the first transition wins; the loser adopts the row")
}

func TestPersistBGShellCompletion_FailedShellMarksRowFailed(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	_, err := store.ClaimShell(ctx, "sess", "sh-f", false)
	require.NoError(t, err)

	c.persistBGShellCompletion("sess", "sh-f", "exit 1", true)
	row, err := store.Get(ctx, "sess", "sh-f")
	require.NoError(t, err)
	require.Equal(t, "failed", row.State)
	require.Equal(t, int64(1), row.ResultIsError.Int64)
}

// A rowless shell (pre-claim shell or store-less fixture) keeps today's
// notice-only path: no row is invented.
func TestPersistBGShellCompletion_RowlessShellWritesNoticeOnly(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	c.persistBGShellCompletion("sess", "sh-legacy", "legacy", false)
	_, err := store.Get(ctx, "sess", "sh-legacy")
	require.Error(t, err, "no row must be invented for a shell that never claimed one")
	notices, err := store.ListSessionNotices(ctx, "sess")
	require.NoError(t, err)
	require.Len(t, notices, 1)
}

// A store whose shell row belongs to a DIFFERENT kind (a real async job's row
// reused id) is never transitioned as a shell.
func TestPersistBGShellCompletion_NeverTouchesNonShellRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	_, err := store.Claim(ctx, session.ClaimParams{Owner: "sess", ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "echo"})
	require.NoError(t, err)

	c.persistBGShellCompletion("sess", "call-1", "summary", false)
	row, err := store.Get(ctx, "sess", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "a command row is not a shell row; only its notice is written")
}
