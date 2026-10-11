package server

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type attachmentsLifecycleCoordinator struct {
	mockAgentCoordinator
	conn      *sql.DB
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
	cancelled atomic.Bool
	joined    atomic.Bool
	workerErr error
}

func (c *attachmentsLifecycleCoordinator) CancelAll() bool {
	c.cancelled.Store(true)
	return false
}

func (c *attachmentsLifecycleCoordinator) SetWakeScheduleStore(*session.WakeScheduleStore) {}

func (c *attachmentsLifecycleCoordinator) StopWakeScheduler() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
	c.joined.Store(true)
}

func TestAttachmentsTestAppCleanup(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "original"
		if replace {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			forceLocalCLIProviderForTest(t)
			workingDir, dataDir := t.TempDir(), t.TempDir()
			before := runtime.NumGoroutine()
			var a *appPkg.App
			var original agent.Coordinator
			var conn *sql.DB
			var replacement *attachmentsLifecycleCoordinator
			t.Cleanup(func() {
				if original != nil {
					original.CancelAll()
					original.(agent.WakeScheduleController).StopWakeScheduler()
				}
				if replacement != nil {
					replacement.StopWakeScheduler()
				}
				if a != nil {
					a.Shutdown()
				}
			})

			require.True(t, t.Run("app", func(t *testing.T) {
				a = newAttachmentsTestApp(t, workingDir, dataDir)
				original, conn = a.AgentCoordinator, a.DB()
				require.NotNil(t, original)
				require.Implements(t, (*agent.WakeScheduleController)(nil), original)
				_, epoch, cancel, ok := original.ReserveExclusive(context.Background(), "cleanup-probe")
				require.True(t, ok)
				original.ReleaseExclusive("cleanup-probe", epoch, cancel)
				if replace {
					replacement = &attachmentsLifecycleCoordinator{
						conn: conn, stop: make(chan struct{}), done: make(chan struct{}),
					}
					go func() {
						defer close(replacement.done)
						<-replacement.stop
						replacement.workerErr = replacement.conn.PingContext(context.Background())
					}()
					a.AgentCoordinator = replacement
				}
			}))

			ctx, epoch, cancel, admitted := original.ReserveExclusive(context.Background(), "cleanup-probe")
			if admitted {
				original.ReleaseExclusive("cleanup-probe", epoch, cancel)
			}
			_ = ctx
			closedErr := conn.PingContext(context.Background())
			t.Logf("After child cleanup: goroutines before=%d after=%d; original admitted=%t; writer ping=%v",
				before, runtime.NumGoroutine(), admitted, closedErr)
			require.False(t, admitted, "original coordinator admission remains open after child cleanup")
			if replacement != nil {
				require.True(t, replacement.cancelled.Load(), "replacement cancellation was skipped")
				require.True(t, replacement.joined.Load(), "replacement scheduler join was skipped")
				select {
				case <-replacement.done:
				default:
					t.Fatal("replacement scheduler worker survived cleanup")
				}
				require.NoError(t, replacement.workerErr, "scheduler must finish while DB is open")
			}
			require.ErrorContains(t, closedErr, "database is closed")
			require.NoError(t, os.RemoveAll(dataDir))
			require.NoError(t, os.RemoveAll(workingDir))
			_, err := os.Stat(dataDir)
			require.True(t, os.IsNotExist(err))
			_, err = os.Stat(workingDir)
			require.True(t, os.IsNotExist(err))
		})
	}
}
