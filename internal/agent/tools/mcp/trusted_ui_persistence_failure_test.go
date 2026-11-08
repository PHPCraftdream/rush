package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTrustedUIDisableFailedCommitPreservesLiveRuntime(t *testing.T) {
	restoreFormat := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restoreFormat)
	const name = "trusted-persist-failure"
	_, server := transactionalNotifyingServer(t, "live-tool", "still connected")
	store, owner := connectedTransactionalServer(t, name, server)
	defer closeCommitOutcomeTestOwner(t, owner)
	original := mustSession(t, name)
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)
	var document map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(before, &document))
	document["concurrent_operator_note"] = json.RawMessage(`true`)
	concurrent, err := json.Marshal(document)
	require.NoError(t, err)

	var reachedCommit atomic.Bool
	var concurrentWriteErr error
	restoreHook := config.SetBeforeConfigCommitForTest(func() {
		if reachedCommit.CompareAndSwap(false, true) {
			// Имитируется чужая запись после guard, но до CAS-проверки commit.
			concurrentWriteErr = os.WriteFile(trustedUIDiskPath(), concurrent, 0o600)
		}
	})
	defer restoreHook()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mutationErr := owner.DisableServerTrustedUI(ctx, store, name)
	restoreHook()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("trusted MCP commit failure: reached=%v mutation_err=%v concurrent_write_err=%v", reachedCommit.Load(), mutationErr, concurrentWriteErr)
		}
	})
	require.True(t, reachedCommit.Load(), "the real trusted persister must reach commit under the settings lock")
	require.NoError(t, concurrentWriteErr)
	require.ErrorIs(t, mutationErr, config.ErrMCPStale)
	require.ErrorContains(t, mutationErr, "failed to persist MCP disabled state")
	require.Equal(t, concurrent, trustedUIReadDisk(t), "the failed commit must preserve the concurrent writer")
	require.False(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool())
	trustedUIAssertSyncRevIntact(t, before)
	require.Same(t, original, mustSession(t, name), "failed persistence must not replace or detach the existing session")
	require.Equal(t, StateConnected, mustState(t, name).State)
	require.Equal(t, []string{"live-tool"}, GetServerToolNames(name))
	require.NoError(t, original.Ping(ctx, nil))
	result, err := RunTool(ctx, store, name, "live-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "still connected", result.Content)
	require.Same(t, original, mustSession(t, name))
}

func TestTrustedUIEnableRollsBackThroughOwnerAfterCallerCancellation(t *testing.T) {
	restoreFormat := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restoreFormat)
	const name = "trusted-owner-rollback"
	_, server := transactionalNotifyingServer(t, "unused-tool", "unused")
	store := persistedMCPStore(t, name, server.URL, true)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	owner.Initialize(t.Context(), nil, store, false)
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var commits atomic.Int32
	restoreHook := config.SetBeforeConfigCommitForTest(func() {
		if commits.Add(1) == 1 {
			// Persist завершается, но новый admission уже не может стартовать.
			cancel()
		}
	})
	defer restoreHook()
	enableErr := owner.EnableServerTrustedUI(ctx, store, name)
	restoreHook()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("trusted MCP owner rollback: commits=%d enable_err=%v", commits.Load(), enableErr)
		}
	})
	require.Error(t, enableErr)
	require.NotErrorIs(t, enableErr, config.ErrSettingsLocked, "Owner rollback must retain its trusted authority")
	require.EqualValues(t, 2, commits.Load(), "both enable and token-conditional rollback must commit under the lock")
	require.True(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool())
	current, exists := store.MCPConfig(name)
	require.True(t, exists)
	require.True(t, current.Disabled)
	require.False(t, hasSession(name), "a cancelled enable must not publish a new session")
	trustedUIAssertSyncRevIntact(t, before)
}
