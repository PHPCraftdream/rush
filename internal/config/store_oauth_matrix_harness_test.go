package config

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func oauthMatrixSeamRestorer() func() {
	read, write := oauthJournalReadFile, oauthJournalAtomicWrite
	readWait, retryWait, budget := oauthJournalReadWait, oauthJournalRetryWait, oauthJournalWriteBudget
	rename, copy := oauthJournalQuarantineRename, oauthJournalQuarantineCopy
	before, wait, idle := oauthJournalBeforeWorker, oauthJournalWorkerWait, oauthJournalWorkerIdle
	exchange := codexRefreshTokenFn
	configTestHooks.Lock()
	syncParent := configTestHooks.syncParent
	configTestHooks.Unlock()
	return func() {
		oauthJournalReadFile, oauthJournalAtomicWrite = read, write
		oauthJournalReadWait, oauthJournalRetryWait, oauthJournalWriteBudget = readWait, retryWait, budget
		oauthJournalQuarantineRename, oauthJournalQuarantineCopy = rename, copy
		oauthJournalBeforeWorker, oauthJournalWorkerWait, oauthJournalWorkerIdle = before, wait, idle
		codexRefreshTokenFn = exchange
		configTestHooks.Lock()
		configTestHooks.syncParent = syncParent
		configTestHooks.Unlock()
	}
}

// Both the matrix and timeout harness execute this cleanup path.
func oauthMatrixCleanup(t *testing.T, store *ConfigStore, callerDone <-chan struct{}, join func(), timeout time.Duration, cancel func(), restore func(), fail func(string, ...any)) {
	defer restore()
	cancel()
	consumeOAuthJournalTestStore(t, store)
	callerJoined := false
	select {
	case <-callerDone:
		callerJoined = true
	case <-time.After(timeout):
	}
	workerJoined := oauthMatrixBoundedJoin(join, timeout)
	if !callerJoined || !workerJoined {
		restore()
		fail("%s: cleanup timeout caller-joined=%t worker-joined=%t", t.Name(), callerJoined, workerJoined)
	}
}

func oauthMatrixSeamPointers() []uintptr {
	configTestHooks.Lock()
	syncParent := configTestHooks.syncParent
	configTestHooks.Unlock()
	seams := []any{
		oauthJournalReadFile, oauthJournalAtomicWrite, oauthJournalReadWait, oauthJournalRetryWait,
		oauthJournalQuarantineRename, oauthJournalQuarantineCopy, oauthJournalBeforeWorker,
		oauthJournalWorkerWait, oauthJournalWorkerIdle, codexRefreshTokenFn, syncParent,
	}
	var pointers []uintptr
	for _, seam := range seams {
		pointers = append(pointers, reflect.ValueOf(seam).Pointer())
	}
	return pointers
}

func TestOAuthRound9_MatrixCleanupTimeoutPath(t *testing.T) {
	before, budget := oauthMatrixSeamPointers(), oauthJournalWriteBudget
	restore := oauthMatrixSeamRestorer()
	t.Cleanup(restore)
	var joins atomic.Int32
	release, joined := make(chan struct{}), make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("stalled fake worker did not finish after release")
		}
	}()
	t.Run("fatal-style-cleanup", func(child *testing.T) {
		store := &ConfigStore{}
		trackOAuthJournalTestStore(child, store)
		oauthJournalReadFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
		oauthJournalAtomicWrite = func(string, []byte, os.FileMode) error { return os.ErrPermission }
		oauthJournalReadWait = func(context.Context, time.Duration) error { return os.ErrPermission }
		oauthJournalRetryWait = oauthJournalReadWait
		oauthJournalWriteBudget = time.Millisecond
		oauthJournalQuarantineRename = func(string, string) error { return os.ErrPermission }
		oauthJournalQuarantineCopy = oauthJournalQuarantineRename
		oauthJournalBeforeWorker = func(context.Context) {}
		oauthJournalWorkerWait = oauthJournalReadWait
		oauthJournalWorkerIdle = func(*ConfigStore, string) {}
		codexRefreshTokenFn = func(context.Context, *http.Client, string) (*oauth.Token, error) { return nil, os.ErrPermission }
		configTestHooks.Lock()
		configTestHooks.syncParent = func(string) error { return os.ErrPermission }
		configTestHooks.Unlock()
		callerCtx, cancel := context.WithCancel(context.Background())
		callerDone := make(chan struct{})
		close(callerDone)
		child.Cleanup(func() {
			finished := make(chan struct{})
			failure := make(chan string, 1)
			go func() {
				defer close(finished)
				oauthMatrixCleanup(child, store, callerDone, func() {
					joins.Add(1)
					<-release
					close(joined)
				}, time.Millisecond, cancel, restore, func(format string, args ...any) {
					failure <- fmt.Sprintf(format, args...)
					runtime.Goexit()
				})
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				child.Fatal("cleanup did not continue after its timeout")
			}
			assert.ErrorIs(child, callerCtx.Err(), context.Canceled)
			require.Len(child, failure, 1)
			assert.Contains(child, <-failure, "caller-joined=true worker-joined=false")
			value, ok := oauthJournalTestStores.Load(child)
			require.True(child, ok)
			assert.Empty(child, value, "registration must be consumed before fixture cleanup")
			assert.Equal(child, before, oauthMatrixSeamPointers(), "all matrix function seams restored")
			assert.Equal(child, budget, oauthJournalWriteBudget)
		})
	})
	assert.Equal(t, int32(1), joins.Load(), "fixture cleanup must not join a second time")
	assert.Equal(t, before, oauthMatrixSeamPointers(), "parent continues after fatal-style cleanup")
	assert.Equal(t, budget, oauthJournalWriteBudget)
}
