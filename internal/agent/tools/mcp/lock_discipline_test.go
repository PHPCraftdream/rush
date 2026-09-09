package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp/internal/contextlock"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// probeWriteLockFree reports whether m is currently free for writers. A free
// contextlock is granted synchronously, so true is deterministic. When the
// lock is held, the probe waits until the hold ends or the budget expires
// and then returns false without keeping the lock.
func probeWriteLockFree(m *contextlock.RWMutex, budget time.Duration) bool {
	probeCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	acquired := make(chan bool, 1)
	go func() {
		acquired <- m.LockContext(probeCtx, true)
	}()
	granted := <-acquired
	if granted {
		m.Unlock()
	}
	return granted
}

// TestReplaceServerAcquiresLeasesInSortedNameOrder pins the INV-07 clause
// that multi-server operations take their leases in sorted name order. The
// rename call site passes (oldName, newName) in reverse lexical order, so a
// removed sort changes the observed acquisition sequence.
func TestReplaceServerAcquiresLeasesInSortedNameOrder(t *testing.T) {
	const oldName = "zz-order-source"
	const newName = "aa-order-target"
	store := persistedMCPStore(t, oldName, "http://127.0.0.1:1/unreachable", false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	var orderMu sync.Mutex
	var lockOrder []string
	serverLeaseHooks.Lock()
	previous := serverLeaseHooks.beforeTryLockFn
	serverLeaseHooks.beforeTryLockFn = func(lease *serverLease) {
		orderMu.Lock()
		lockOrder = append(lockOrder, lease.name)
		orderMu.Unlock()
	}
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeTryLockFn = previous
		serverLeaseHooks.Unlock()
	}()

	prepare := func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
		return nil, errors.New("injected preparation failure")
	}
	err = replaceServerWithResultPersistenceAndPreparation(context.Background(), store, oldName, newName,
		config.MCPConfig{Type: config.MCPHttp, URL: "http://127.0.0.1:1/unreachable", Timeout: 1},
		func(*config.ConfigStore, config.Scope, string, string, config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, errors.New("persist must not run")
		}, prepare)
	require.ErrorContains(t, err, "injected preparation failure")

	orderMu.Lock()
	defer orderMu.Unlock()
	var renameLeases []string
	for _, name := range lockOrder {
		if name == oldName || name == newName {
			renameLeases = append(renameLeases, name)
		}
	}
	require.Equal(t, []string{newName, oldName}, renameLeases)
}

// TestReplacePersistRunsOutsideLifecycleLock pins the INV-06 clause at the
// durable replacement write: the disk I/O seam runs with lifecycleMu free so
// Owner.Close can proceed while the persist is stalled. The ordered server
// leases are held at this seam by design and are deliberately not probed.
func TestReplacePersistRunsOutsideLifecycleLock(t *testing.T) {
	_, oldHTTP := transactionalNotifyingServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	store, owner := connectedTransactionalServer(t, "same-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	persistErr := errors.New("injected persistence failure")
	var persistCalls int
	lifecycleFree := true
	err := replaceServerWithPersistence(context.Background(), store, "same-name", "same-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}, func(*config.ConfigStore, string, string, config.MCPConfig) error {
		persistCalls++
		if !probeWriteLockFree(&lifecycleMu, 500*time.Millisecond) {
			lifecycleFree = false
		}
		return persistErr
	})
	require.ErrorIs(t, err, persistErr)
	require.Equal(t, 1, persistCalls)
	require.True(t, lifecycleFree, "durable replacement write ran while lifecycleMu was held")
}

// TestUncertaintyReloadRunsOutsideLifecycleLocks pins the INV-06 clause at
// the uncertainty reload: the disk read and its finalization run outside
// lifecycleMu and every server lease. The observation point is the post
// reload finalize seam (mcpReloadAfterSuccessHook), which a regression that
// moves the reload under a lock cannot pass without holding it.
func TestUncertaintyReloadRunsOutsideLifecycleLocks(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	owner.rememberConfig(store)

	const name = "reload-lock-state"
	seeded := serverLeaseFor(name)
	defer leases.release(seeded)

	store.MarkMCPUncertain(name)
	token, ok := owner.captureUncertainty(store, name)
	require.True(t, ok)

	var heldMu sync.Mutex
	var heldUnderLock []string
	probedLeases := 0
	mcpReloadAfterSuccessHook = func() {
		if !probeWriteLockFree(&lifecycleMu, 500*time.Millisecond) {
			heldMu.Lock()
			heldUnderLock = append(heldUnderLock, "lifecycleMu")
			heldMu.Unlock()
		}
		leases.mu.Lock()
		entries := make([]*serverLease, 0, len(leases.entries))
		for _, lease := range leases.entries {
			entries = append(entries, lease)
		}
		leases.mu.Unlock()
		for _, lease := range entries {
			probedLeases++
			if !probeWriteLockFree(&lease.mu, 500*time.Millisecond) {
				heldMu.Lock()
				heldUnderLock = append(heldUnderLock, "lease "+lease.name)
				heldMu.Unlock()
			}
		}
	}
	defer func() { mcpReloadAfterSuccessHook = nil }()

	require.NoError(t, reloadWithUncertaintyToken(context.Background(), token))
	require.GreaterOrEqual(t, probedLeases, 1, "lease probe never ran; oracle would be vacuous")
	require.Empty(t, heldUnderLock)
	_, stillFenced := store.MCPUncertaintyVersion(name)
	require.False(t, stillFenced)
}
