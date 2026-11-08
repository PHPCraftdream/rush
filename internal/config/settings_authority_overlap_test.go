package config

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTrustedUICommitDoesNotUnlockIndependentGuardedStore(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	ui, global := isolatedLegacyStore(t, "global.json")
	SetProcessPassword("")
	require.NoError(t, ui.SetConfigField(ScopeGlobal, "seed", 1))
	require.NoError(t, ui.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	locked := fileBytes(t, global)
	workspace := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(workspace, []byte(`{"seed":1}`), 0o600))
	guarded := newTestConfigStore(testStoreOpts{
		config: &Config{}, globalDataPath: global, workspacePath: workspace,
	})

	entered := make(chan struct{})
	released := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	var claimed atomic.Bool
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = func() {
		if claimed.CompareAndSwap(false, true) {
			close(entered)
			<-released
		}
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	})

	uiDone := make(chan struct{})
	guardedDone := make(chan struct{})
	var uiErr, guardedErr error
	guardedStarted := false
	phase := "trusted commit entry"
	defer func() {
		release()
		wait := func(done <-chan struct{}, name string) {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Errorf("%s writer did not join after release", name)
			}
		}
		wait(uiDone, "trusted")
		if guardedStarted {
			wait(guardedDone, "guarded")
		}
		if t.Failed() {
			t.Logf("authority overlap: phase=%s commit_gate=%v guarded_started=%v", phase, claimed.Load(), guardedStarted)
		}
	}()
	go func() {
		defer close(uiDone)
		uiErr = ui.SetConfigFieldWithAuthority(ScopeGlobal, "ui_key", true, SettingsAuthorityTrustedUI)
	}()
	select {
	case <-entered:
	case <-uiDone:
		require.NoError(t, uiErr)
		t.Fatal("trusted write returned before entering the commit barrier")
	case <-time.After(5 * time.Second):
		t.Fatal("trusted write did not reach the commit barrier")
	}

	// Другой target/store не сериализуется за UI, но подчиняется тому же global lock.
	phase = "guarded write while trusted commit is blocked"
	guardedStarted = true
	go func() {
		defer close(guardedDone)
		guardedErr = guarded.SetConfigField(ScopeWorkspace, "forbidden_key", true)
	}()
	select {
	case <-guardedDone:
		require.ErrorIs(t, guardedErr, ErrSettingsLocked)
	case <-time.After(5 * time.Second):
		t.Fatal("independent guarded write did not finish inside the UI commit window")
	}
	select {
	case <-uiDone:
		t.Fatal("trusted writer escaped the commit barrier")
	default:
	}
	require.False(t, gjson.GetBytes(fileBytes(t, workspace), "forbidden_key").Exists())
	release()
	phase = "trusted commit released"
	select {
	case <-uiDone:
		require.NoError(t, uiErr)
	case <-time.After(5 * time.Second):
		t.Fatal("trusted write did not complete after release")
	}
	require.True(t, gjson.GetBytes(fileBytes(t, global), "ui_key").Bool())
	requireSyncRevUnchanged(t, locked, fileBytes(t, global))
}
