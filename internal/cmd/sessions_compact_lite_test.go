package cmd

// These tests pin `sessions compact` to the lite setup path (setupAppLite):
// the command must never run startup recovery, whose orphan-stamping would
// mutate live sessions as a side effect of a vacuum.

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestSessionsCompactCmdRun_DoesNotRunStartupRecovery: compact's setup is
// setupAppLite, which skips recoverInterruptedTurns -- an orphaned assistant
// tool call must stay orphaned through a compact run.
// REVERT CHECK: pointing sessionsCompactCmdRun back at setupApp turns this
// red (recovery stamps the orphan during the command's own startup).
func TestSessionsCompactCmdRun_DoesNotRunStartupRecovery(t *testing.T) {
	_, seed, run := isolatedCompactEnv(t)
	ctx := context.Background()

	sess, err := seed.Sessions.Create(ctx, "compact-lite-orphan-holder")
	require.NoError(t, err)

	orphan, err := seed.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: the tool call must be orphaned")

	// Backdate past the 30s orphan-age threshold, exactly like
	// internal/app/app_new_skip_agent_setup_test.go does.
	_, err = seed.DB().ExecContext(ctx,
		"UPDATE messages SET created_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), orphan.ID)
	require.NoError(t, err)

	out, _, err := run(t)
	require.NoError(t, err)
	require.Contains(t, out, "database: ",
		"the command body must have run; a setup failure would have returned an error")

	got, err := seed.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	require.False(t, got.IsFinished(),
		"compact must not run startup recovery: its setup is setupAppLite, which skips recoverInterruptedTurns")
}
