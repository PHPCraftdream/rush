package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// Scope read внедрён; OS-probe, очередь и pending injects — настоящие.
func TestSessionsKillFreeTurnLockPreservesLiveBatchQueue(t *testing.T) {
	for _, hasLock := range []bool{false, true} {
		for _, scope := range []batchKillScopeState{{Note: "live held batch scope", Unconfirmed: true}, {Note: "batch scope is unreadable", Unconfirmed: true}, {Note: "only completion debt remains"}, {}} {
			note := scope.Note
			t.Run(fmt.Sprintf("lock=%v/note=%q", hasLock, note), func(t *testing.T) {
				a, dataDir := stopPurgeEnv(t)
				if a.RunQueuePump != nil {
					a.RunQueuePump.Stop()
				}
				ctx := context.Background()
				sess, err := a.Sessions.CreateWithID(ctx, "between-turn-parent", "parent waits for batch")
				require.NoError(t, err)
				mergeID, interruptID := seedQueuedWorkForStop(t, a, sess.ID)
				a.Shutdown()
				lockPath := session.SessionLockPath(dataDir, sess.ID)
				require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o700))
				if hasLock {
					require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
				}
				cmd := &cobra.Command{}
				ensureRootFlagStandIns(cmd, dataDir)
				cmd.Flags().Bool("keep-lock", false, "")
				cmd.Flags().Duration("wait", time.Second, "")
				stopPurgeCmdContext(t, cmd)
				bounded := false
				var commandErr error
				stderr := captureStderr(t, func() {
					commandErr = sessionsKillCmdRunWithBatchNote(cmd, []string{sess.ID}, func(ctx context.Context, _ string, id string) batchKillScopeState {
						_, bounded = ctx.Deadline()
						require.Equal(t, sess.ID, id)
						return scope
					})
				})
				t.Cleanup(func() {
					if t.Failed() {
						if len(stderr) > 2048 {
							stderr = stderr[len(stderr)-2048:]
						}
						t.Logf("free-lock kill: lock=%v note=%q error=%v bounded_read=%v\n%s", hasLock, note, commandErr, bounded, stderr)
					}
				})
				require.True(t, bounded, "rescue metadata read needs a deadline")
				if scope.Unconfirmed {
					require.ErrorIs(t, commandErr, errBatchKillScopeUnconfirmed)
				} else {
					require.NoError(t, commandErr)
				}
				conn, err := db.Connect(ctx, dataDir)
				require.NoError(t, err)
				defer func() { require.NoError(t, db.ReleaseConn(conn)) }()
				sessions := session.NewService(db.New(conn), conn)
				pending, err := sessions.PeekInterruptInject(ctx, sess.ID)
				require.NoError(t, err)
				queued, err := sessions.HasOutstandingRunQueueEntriesForSession(ctx, sess.ID)
				require.NoError(t, err)
				preserved := scope.Unconfirmed || !hasLock
				require.Equal(t, preserved, queued, "a free parent lock is not proof that its live batch scope stopped")
				if preserved {
					require.NotNil(t, pending)
					require.Equal(t, interruptID, pending.MessageID)
				} else {
					require.Nil(t, pending)
				}
				messages := message.NewService(db.New(conn))
				for _, id := range []string{mergeID, interruptID} {
					_, err := messages.Get(ctx, id)
					require.NoError(t, err, "kill is not delete-history")
				}
			})
		}
	}
}

func TestKillBatchScopeAbsentStoreDoesNotCreateDatabase(t *testing.T) {
	dataDir := t.TempDir()
	state := batchKillLiveNote(t.Context(), dataDir, "absent-session")
	_, err := os.Stat(storepath.StoreV1Path(dataDir))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("absent-store scope=%+v stat_error=%v", state, err)
		}
	})
	require.False(t, state.Unconfirmed)
	require.Empty(t, state.Note)
	require.ErrorIs(t, err, os.ErrNotExist)
}
