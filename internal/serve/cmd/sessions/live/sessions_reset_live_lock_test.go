package sesslive

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func TestSessionsReset_LockRefusalPreservesMessages(t *testing.T) {
	a, cmd, _, shutdown := resetTestEnv(t, "", "")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-locked-session", "locked")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "keep me"}}})
	require.NoError(t, err)
	lock, err := sessionlock.TryAcquireSessionLock(a.Config().Options.DataDirectory, sess.ID)
	require.NoError(t, err)
	defer lock.Release()
	shutdown()
	setResetForce(t, cmd, false)
	err = cmd.RunE(cmd, []string{sess.ID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "a turn holds the session lock")
	require.Contains(t, err.Error(), "sessions cancel")
	require.Contains(t, err.Error(), "--force")
	verification := resetVerifyApp(t, cmd)
	messages, err := verification.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
}

func TestSessionsReset_UnownedSessionSucceeds(t *testing.T) {
	a, cmd, _, shutdown := resetTestEnv(t, "", "")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-unowned-session", "unowned")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "clear me"}}})
	require.NoError(t, err)
	shutdown()
	setResetForce(t, cmd, false)
	require.NoError(t, cmd.RunE(cmd, []string{sess.ID}))
	verification := resetVerifyApp(t, cmd)
	messages, err := verification.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, messages)
}
