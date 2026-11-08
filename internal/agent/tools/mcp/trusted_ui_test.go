package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Trusted-UI settings-write tests (INV-02/INV-03/INV-12/INV-13 chunk).
//
// Fixtures are reused verbatim from this package: isolatedMCPStore /
// persistedMCPStore-style seeding, Acquire + closeCommitOutcomeTestOwner
// owners, and the real loopback HTTP MCP server used by
// state_regression_test.go and ktav_lifecycle_roundtrip_test.go. The settings
// lock is applied with config.ConfigStore.LockSettings and released by
// resetting the process password (the internal/config fixture pattern from
// settings_lock_exemptions_test.go). All tests are sequential: owner/lease
// state is package-global, exactly like the owner-based tests mirrored here.

// trustedUIDiskPath resolves the active global rush.json slot on disk. The
// slot selection (rush.json vs rush.ktav) must follow the same discipline the
// production lock reader uses, so ActiveConfigFileName is used instead of a
// literal path join.
func trustedUIDiskPath() string {
	return config.ActiveConfigFileName(config.GlobalConfigData())
}

func trustedUIReadDisk(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(trustedUIDiskPath())
	require.NoError(t, err)
	return data
}

// trustedUILockSettings locks the global scope and registers cleanup that
// clears the process password (the internal/config test pattern). The process
// password is deliberately left empty: guarded writes must be refused because
// the process does not hold the password, while TrustedUI writes lift only
// the lock veto.
func trustedUILockSettings(t *testing.T, store *config.ConfigStore) {
	t.Helper()
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, store.LockSettings(config.ScopeGlobal, "trusted-ui-test-password"))
}

func trustedUIAssertSyncRevIntact(t *testing.T, before []byte) {
	t.Helper()
	data := trustedUIReadDisk(t)
	beforeRev := gjson.GetBytes(before, "sync_rev").String()
	afterRev := gjson.GetBytes(data, "sync_rev").String()
	require.NotEmpty(t, afterRev, "the settings lock hash must stay on disk")
	require.Equal(t, beforeRev, afterRev, "sync_rev must not change across the mutation")
	require.True(t, config.VerifyPassword("trusted-ui-test-password", afterRev),
		"sync_rev must still verify against the lock password")
}

func trustedUILiveServerURL(t *testing.T) string {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "trusted-ui-server"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "trusted-ui probe"}, func(context.Context, *sdkmcp.CallToolRequest, any) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "trusted-ui"}}}, nil, nil
	})
	httpServer := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)
	return httpServer.URL
}

func TestLockedConfigGuardedMCPServerDisableRefused(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "guarded-disable"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type:    config.MCPStdio,
		Command: name,
	}))
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = owner.DisableServer(context.Background(), store, name)
	require.ErrorIs(t, err, config.ErrSettingsLocked,
		"guarded disable must refuse while the settings lock is active")

	after := trustedUIReadDisk(t)
	require.Equal(t, string(before), string(after), "a refused disable must not touch the disk")
	configured, ok := store.MCPConfig(name)
	require.True(t, ok, "the in-memory MCP entry must survive a refused disable")
	require.False(t, configured.Disabled, "a refused disable must not half-apply the disable")
	require.False(t, hasSession(name), "a refused disable must not tear down any runtime")
}

func TestTrustedUIDisableAppliesUnderLock(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "trusted-disable"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type:    config.MCPStdio,
		Command: name,
	}))
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	require.NoError(t, owner.DisableServerTrustedUI(context.Background(), store, name),
		"the trusted-UI authority must lift only the settings-lock veto")

	data := trustedUIReadDisk(t)
	require.True(t, gjson.GetBytes(data, "mcp."+name+".disabled").Bool(),
		"the disable must be durable on disk")
	trustedUIAssertSyncRevIntact(t, before)
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.True(t, configured.Disabled, "the in-memory snapshot must reflect the durable disable")
	require.False(t, hasSession(name))
}

func TestTrustedUIEnableRoundTripUnderLock(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "trusted-roundtrip"
	url := trustedUILiveServerURL(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     url,
		Timeout: 60,
	}))
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	require.NoError(t, owner.DisableServerTrustedUI(context.Background(), store, name))
	data := trustedUIReadDisk(t)
	require.True(t, gjson.GetBytes(data, "mcp."+name+".disabled").Bool())
	trustedUIAssertSyncRevIntact(t, before)
	require.False(t, hasSession(name))

	// The public EnableServerTrustedUI path performs a real initialization
	// (initClientAdmitted) against the loopback HTTP MCP server.
	require.NoError(t, owner.EnableServerTrustedUI(context.Background(), store, name),
		"the trusted-UI enable must apply under the settings lock")
	trustedUIAssertSyncRevIntact(t, before)
	data = trustedUIReadDisk(t)
	require.False(t, gjson.GetBytes(data, "mcp."+name+".disabled").Bool(),
		"the enable must be durable on disk")
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.False(t, configured.Disabled)
	require.Eventually(t, func() bool {
		return hasSession(name) && mustState(t, name).State == StateConnected
	}, 5*time.Second, time.Millisecond, "the trusted-UI enable must start the runtime")
}

func TestTrustedUIAddAppliesAndGuardedAddRefusedUnderLock(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "ui-added"
	url := trustedUILiveServerURL(t)
	mcpCfg := config.MCPConfig{Type: config.MCPHttp, URL: url, Timeout: 60}
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = owner.AddServer(context.Background(), store, name, mcpCfg)
	require.ErrorIs(t, err, config.ErrSettingsLocked,
		"guarded add must refuse while the settings lock is active")
	require.Equal(t, string(before), string(trustedUIReadDisk(t)),
		"a refused add must not touch the disk")
	_, ok := store.MCPConfig(name)
	require.False(t, ok, "a refused add must not half-apply the definition")

	require.NoError(t, owner.AddServerTrustedUI(context.Background(), store, name, mcpCfg),
		"the trusted-UI add must apply under the settings lock")
	data := trustedUIReadDisk(t)
	entry := gjson.GetBytes(data, `mcp.`+name)
	require.True(t, entry.Exists(), "the added server must be durable on disk")
	require.Equal(t, url, entry.Get("url").String())
	require.Equal(t, string(config.MCPHttp), entry.Get("type").String())
	trustedUIAssertSyncRevIntact(t, before)
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, mcpCfg, configured)
	require.Eventually(t, func() bool {
		return hasSession(name) && mustState(t, name).State == StateConnected
	}, 5*time.Second, time.Millisecond, "the trusted-UI add must publish the runtime")
}

func TestTrustedUIReplaceAndRemoveUnderLock(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName, newName = "replace-src", "replace-dst"
	url := trustedUILiveServerURL(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     url,
		Timeout: 60,
	}))
	newCfg := config.MCPConfig{Type: config.MCPHttp, URL: url, Timeout: 30}
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = owner.ReplaceServer(context.Background(), store, oldName, newName, newCfg)
	require.ErrorIs(t, err, config.ErrSettingsLocked,
		"guarded replace must refuse while the settings lock is active")
	require.Equal(t, string(before), string(trustedUIReadDisk(t)),
		"a refused replace must not touch the disk")
	configured, ok := store.MCPConfig(oldName)
	require.True(t, ok)
	require.Equal(t, 60, configured.Timeout, "a refused replace must not half-apply")

	require.NoError(t, owner.ReplaceServerTrustedUI(context.Background(), store, oldName, newName, newCfg),
		"the trusted-UI replace must apply under the settings lock")
	data := trustedUIReadDisk(t)
	require.False(t, gjson.GetBytes(data, "mcp."+oldName).Exists(),
		"the old name must be gone from disk after the replace")
	entry := gjson.GetBytes(data, "mcp."+newName)
	require.True(t, entry.Exists(), "the new name must be durable on disk")
	require.Equal(t, url, entry.Get("url").String())
	require.Equal(t, int64(30), entry.Get("timeout").Int())
	trustedUIAssertSyncRevIntact(t, before)
	_, ok = store.MCPConfig(oldName)
	require.False(t, ok)
	replaced, ok := store.MCPConfig(newName)
	require.True(t, ok)
	require.Equal(t, newCfg, replaced)

	err = owner.RemoveServer(store, newName)
	require.ErrorIs(t, err, config.ErrSettingsLocked,
		"guarded remove must refuse while the settings lock is active")
	afterRemoveRefused := trustedUIReadDisk(t)
	require.True(t, gjson.GetBytes(afterRemoveRefused, "mcp."+newName).Exists(),
		"a refused remove must not touch the disk")

	require.NoError(t, owner.RemoveServerTrustedUI(store, newName),
		"the trusted-UI remove must apply under the settings lock")
	data = trustedUIReadDisk(t)
	require.False(t, gjson.GetBytes(data, "mcp."+newName).Exists(),
		"the removed server must be gone from disk")
	trustedUIAssertSyncRevIntact(t, before)
	_, ok = store.MCPConfig(newName)
	require.False(t, ok, "the in-memory snapshot must agree with the disk")
	require.False(t, hasSession(newName))
}

func TestTrustedUIFailedPersistencePreservesRuntimeSemantics(t *testing.T) {
	// Config-level leg of persist-before-invalidate: with the lock active,
	// the guarded persister refuses, and nothing observable on the store
	// (MCP snapshot, uncertainty fences) may churn.
	store := isolatedMCPStore(t)
	const name = "guarded-persist"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type:    config.MCPStdio,
		Command: name,
	}))
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)
	uncertaintyBefore := store.MCPUncertaintyVersions()

	result, err := store.PersistMCPDisabledOverrideResult(config.ScopeGlobal, name, true)
	require.ErrorIs(t, err, config.ErrSettingsLocked)
	require.Equal(t, config.MCPMutationResult{}, result,
		"a refused persistence must not return a usable mutation token")

	require.Equal(t, string(before), string(trustedUIReadDisk(t)),
		"a refused persistence must not touch the disk")
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.False(t, configured.Disabled, "the MCP snapshot must be unchanged")
	require.Equal(t, uncertaintyBefore, store.MCPUncertaintyVersions(),
		"a refused persistence must not churn uncertainty fences")
	require.False(t, hasSession(name))
}

// TestTrustedUIDisableUnderUncertaintyPreservesLiveSession is the non-vacuous
// runtime-preservation oracle: a live StateConnected session, a trusted
// disable refused by the uncertainty fence, and every observable preserved.
// The fence is raised while the mutation waits for the server lease — the
// same window testLeaseWaitUncertainty uses — because reconcileUncertainty
// runs before the lease and would clear any fence raised earlier. If the
// post-lease uncertainty check were removed, the trusted disable would
// persist (the authority lifts the settings-lock veto), detach the live
// session, and rewrite the disk, failing every assertion below.
func TestTrustedUIDisableUnderUncertaintyPreservesLiveSession(t *testing.T) {
	_, httpServer := transactionalNotifyingServer(t, "trusted-ui-tool", "trusted-ui")
	store := persistedMCPStore(t, "trusted-uncertain", httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	owner.Initialize(context.Background(), nil, store, false)
	require.Equal(t, StateConnected, mustState(t, "trusted-uncertain").State)
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	lease := serverLeaseFor("trusted-uncertain")
	lease.Lock()
	var releaseOnce sync.Once
	releaseLease := func() { releaseOnce.Do(lease.Unlock) }
	defer releaseLease()
	var enteredOnce sync.Once
	entered := make(chan struct{})
	serverLeaseHooks.Lock()
	hook := func(candidate *serverLease) {
		if candidate == lease {
			enteredOnce.Do(func() { close(entered) })
		}
	}
	serverLeaseHooks.beforeLockFn = hook
	serverLeaseHooks.beforeTryLockFn = hook
	serverLeaseHooks.Unlock()
	t.Cleanup(func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.beforeTryLockFn = nil
		serverLeaseHooks.Unlock()
	})

	disableDone := make(chan error, 1)
	go func() {
		disableDone <- owner.DisableServerTrustedUI(context.Background(), store, "trusted-uncertain")
	}()
	awaitMCPSignal(t, entered)
	store.MarkMCPUncertain("trusted-uncertain")
	releaseLease()

	select {
	case disableErr := <-disableDone:
		require.ErrorIs(t, disableErr, ErrMCPConfigUncertain)
	case <-time.After(mcpAwaitTimeout):
		t.Fatal("trusted disable did not return after the fence and lease release")
	}
	require.True(t, hasSession("trusted-uncertain"),
		"the live session must survive the refused disable")
	require.Equal(t, StateConnected, mustState(t, "trusted-uncertain").State)
	require.NotEmpty(t, GetServerToolNames("trusted-uncertain"),
		"advertised tools must survive")
	require.Equal(t, string(before), string(trustedUIReadDisk(t)),
		"the refused disable must not touch disk or the settings lock")
}

// TestTrustedEnableRollbackSeamUnderLock pins the exact seam the enable
// rollback branch calls (rollbackPersistence →
// PersistMCPEnableRollbackResultWithAuthority): the token-conditional restore
// works under an active settings lock only with the trusted authority, a
// guarded rollback of the same token is vetoed without half-applying, and a
// stale token (the store generation moved) is refused even for the trusted
// authority. The Owner-level branch machinery itself is owned by the
// existing INV-13 tests with injected persisters.
func TestTrustedEnableRollbackSeamUnderLock(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "trusted-rollback"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type:     config.MCPStdio,
		Command:  name,
		Disabled: true,
	}))
	trustedUILockSettings(t, store)
	before := trustedUIReadDisk(t)

	// The trusted enable persist leg: the same call
	// enableServerWithPersistenceAndInitializerAndRollbackForOwner makes.
	token, err := enableMCPConfigTrustedUI(store, config.ScopeGlobal, name, nil)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool(),
		"the trusted enable must land under the lock")
	trustedUIAssertSyncRevIntact(t, before)

	// The identical rollback with guarded authority is vetoed by the lock.
	_, err = store.PersistMCPEnableRollbackResult(config.ScopeGlobal, name, token)
	require.ErrorIs(t, err, config.ErrSettingsLocked)
	require.False(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool(),
		"the vetoed rollback must not half-apply")

	// The trusted rollback restores the prior durable state under the lock.
	_, err = store.PersistMCPEnableRollbackResultWithAuthority(
		config.ScopeGlobal, name, token, config.SettingsAuthorityTrustedUI)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool(),
		"the trusted rollback must restore the disabled state")
	trustedUIAssertSyncRevIntact(t, before)

	// Stale token: the successful rollback advanced the store generation, so
	// replaying the original token must refuse without touching the disk.
	_, err = store.PersistMCPEnableRollbackResultWithAuthority(
		config.ScopeGlobal, name, token, config.SettingsAuthorityTrustedUI)
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	require.True(t, gjson.GetBytes(trustedUIReadDisk(t), "mcp."+name+".disabled").Bool())
}
