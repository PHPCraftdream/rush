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

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func oauthMatrixSeamRestorer() func() {
	read, write := oauthjournal.OAuthJournalReadFile, oauthjournal.OAuthJournalAtomicWrite
	readWait, retryWait, budget := oauthjournal.OAuthJournalReadWait, oauthjournal.OAuthJournalRetryWait, oauthjournal.OAuthJournalWriteBudget
	rename, copy := oauthjournal.OAuthJournalQuarantineRename, oauthjournal.OAuthJournalQuarantineCopy
	before, wait, idle := oauthjournal.OAuthJournalBeforeWorker, oauthjournal.OAuthJournalWorkerWait, oauthJournalWorkerIdle
	exchange := codexRefreshTokenFn
	configTestHooks.Lock()
	syncParent := configTestHooks.syncParent
	configTestHooks.Unlock()
	return func() {
		oauthjournal.OAuthJournalReadFile, oauthjournal.OAuthJournalAtomicWrite = read, write
		oauthjournal.OAuthJournalReadWait, oauthjournal.OAuthJournalRetryWait, oauthjournal.OAuthJournalWriteBudget = readWait, retryWait, budget
		oauthjournal.OAuthJournalQuarantineRename, oauthjournal.OAuthJournalQuarantineCopy = rename, copy
		oauthjournal.OAuthJournalBeforeWorker, oauthjournal.OAuthJournalWorkerWait, oauthJournalWorkerIdle = before, wait, idle
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
		oauthjournal.OAuthJournalReadFile, oauthjournal.OAuthJournalAtomicWrite, oauthjournal.OAuthJournalReadWait, oauthjournal.OAuthJournalRetryWait,
		oauthjournal.OAuthJournalQuarantineRename, oauthjournal.OAuthJournalQuarantineCopy, oauthjournal.OAuthJournalBeforeWorker,
		oauthjournal.OAuthJournalWorkerWait, oauthJournalWorkerIdle, codexRefreshTokenFn, syncParent,
	}
	var pointers []uintptr
	for _, seam := range seams {
		pointers = append(pointers, reflect.ValueOf(seam).Pointer())
	}
	return pointers
}

func TestOAuthRound9_MatrixCleanupTimeoutPath(t *testing.T) {
	before, budget := oauthMatrixSeamPointers(), oauthjournal.OAuthJournalWriteBudget
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
		oauthjournal.OAuthJournalReadFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
		oauthjournal.OAuthJournalAtomicWrite = func(string, []byte, os.FileMode) error { return os.ErrPermission }
		oauthjournal.OAuthJournalReadWait = func(context.Context, time.Duration) error { return os.ErrPermission }
		oauthjournal.OAuthJournalRetryWait = oauthjournal.OAuthJournalReadWait
		oauthjournal.OAuthJournalWriteBudget = time.Millisecond
		oauthjournal.OAuthJournalQuarantineRename = func(string, string) error { return os.ErrPermission }
		oauthjournal.OAuthJournalQuarantineCopy = oauthjournal.OAuthJournalQuarantineRename
		oauthjournal.OAuthJournalBeforeWorker = func(context.Context) {}
		oauthjournal.OAuthJournalWorkerWait = oauthjournal.OAuthJournalReadWait
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
			assert.Equal(child, budget, oauthjournal.OAuthJournalWriteBudget)
		})
	})
	assert.Equal(t, int32(1), joins.Load(), "fixture cleanup must not join a second time")
	assert.Equal(t, before, oauthMatrixSeamPointers(), "parent continues after fatal-style cleanup")
	assert.Equal(t, budget, oauthjournal.OAuthJournalWriteBudget)
}
