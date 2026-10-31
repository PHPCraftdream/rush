package cmd

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionsReset_LockRefusalPreservesMessages(t *testing.T) {
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-locked-session", "locked")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "keep me"}}})
	require.NoError(t, err)
	lock, err := session.TryAcquireSessionLock(a.Config().Options.DataDirectory, sess.ID)
	require.NoError(t, err)
	defer lock.Release()
	a.Shutdown()

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "false"))
	err = sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "a turn holds the session lock")
	require.Contains(t, err.Error(), "sessions cancel")
	require.Contains(t, err.Error(), "--force")
	verification, err := setupAppLite(sessionsResetCmd)
	require.NoError(t, err)
	t.Cleanup(verification.Shutdown)
	messages, err := verification.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
}

func TestSessionsReset_UnownedSessionSucceeds(t *testing.T) {
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-unowned-session", "unowned")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "clear me"}}})
	require.NoError(t, err)
	a.Shutdown()

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "false"))
	err = sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
	require.NoError(t, err)
	verification, err := setupAppLite(sessionsResetCmd)
	require.NoError(t, err)
	t.Cleanup(verification.Shutdown)
	messages, err := verification.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, messages)
}
