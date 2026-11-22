package sesslive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func TestSessionsReset_ForceDoesNotKillStalePID(t *testing.T) {
	a, cmd, dataDir, shutdown := resetTestEnv(t, "", "")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-stale-pid", "regression title")
	require.NoError(t, err)
	lockPath := resetLockPath(dataDir, sess.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	require.True(t, procinfo.IsProcessAlive(os.Getpid()))
	shutdown()

	stderr := runResetCommand(t, cmd, sess.ID, true)
	t.Logf("reset --force stderr:\n%s", stderr)
	require.True(t, procinfo.IsProcessAlive(os.Getpid()), "reset --force must never kill the calling test process via a stale lock-file PID")
	require.FileExists(t, lockPath, "reset --force must leave the lock file in place (held+released), not unlink it")
}

func TestSessionsReset_ForceHonorsConfiguredDataDir(t *testing.T) {
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "configured-elsewhere-data")
	a, cmd, resolvedDir, shutdown := resetTestEnv(t, dataDir, "")
	require.Equal(t, dataDir, resolvedDir, "test setup assumption: resolved DataDirectory must equal the --data-dir we configured")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-configured-datadir", "regression title")
	require.NoError(t, err)
	lockPath := resetLockPath(dataDir, sess.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	require.True(t, procinfo.IsProcessAlive(os.Getpid()))
	shutdown()

	stderr := runResetCommand(t, cmd, sess.ID, true)
	t.Logf("reset --force stderr:\n%s", stderr)
	require.NotContains(t, stderr, "could not determine lock state", "reset --force must acquire the lock at the configured data dir rather than fail closed")
	require.Contains(t, stderr, dataDir, "report must reference the configured data dir's lock path, not a cwd-based guess")
	require.True(t, procinfo.IsProcessAlive(os.Getpid()), "reset --force must never kill the calling test process via a stale lock-file PID")
	require.FileExists(t, lockPath, "lock file at the configured data dir is held+released, not removed")
}

func TestSessionsReset_ForceStillKillsLiveHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, cmd, dataDir, shutdown := resetTestEnv(t, "", "")
	sess, err := a.Sessions.CreateWithID(context.Background(), "reset-force-live-holder", "regression title")
	require.NoError(t, err)
	// reapInBackground=true: same rationale as
	// TestProbeThenKillHolder_LiveHolderStillKilled — reset --force also
	// drives forceKillHolder's SIGKILL-then-poll and needs the victim
	// reaped concurrently so the poll can observe death within its wait
	// budget (see spawnKillTestLockHolder's doc comment).
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID, true)
	defer holder.stop(t)
	require.True(t, procinfo.IsProcessAlive(holder.pid))
	shutdown()
	stderr := runResetCommand(t, cmd, sess.ID, true)
	t.Logf("reset --force stderr:\n%s", stderr)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && procinfo.IsProcessAlive(holder.pid) {
		time.Sleep(50 * time.Millisecond)
	}
	require.False(t, procinfo.IsProcessAlive(holder.pid), "a genuinely live holder must still be killed by reset --force")
}

func TestSessionsReset_ForceClearsUnfinishedAssistantMessages(t *testing.T) {
	a, cmd, _, shutdown := resetTestEnv(t, "", "")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-unfinished-assistant", "regression title")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "user input"}}})
	require.NoError(t, err)
	unfinished, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "orphaned response, never finished"}}})
	require.NoError(t, err)
	require.False(t, unfinished.IsFinished(), "precondition: assistant message must be unfinished")
	before, err := a.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, before, 2, "session must have 2 messages before reset")
	shutdown()
	stderr := cmdtest.CaptureStderr(t, func() {
		setResetForce(t, cmd, true)
		require.NoError(t, cmd.RunE(cmd, []string{sess.ID}), "reset --force must succeed even with an unfinished assistant message")
	})
	t.Logf("reset --force stderr:\n%s", stderr)
	verify := resetVerifyApp(t, cmd)
	after, err := verify.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, after, "all messages must be deleted, including the unfinished assistant")
	count, err := verify.Messages.Count(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), count, "message count must be 0 after reset --force")
	_, err = verify.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err, "session row must still exist after reset")
}
