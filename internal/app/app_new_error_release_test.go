package app

// Package-PE regression tests: App.New installs the batch runtime (flag ON,
// RUSH_BATCH_RUNTIME) -- and with it the host identity, held as the OS lock
// file hosts/<uuid>.lock -- BEFORE InitCoderAgent runs. When New then fails,
// the error path must tear the runtime and host identity back down (via
// peReleaseBatchRuntimeOnNewError) or a long-lived caller (tests, the web
// server) is left holding the lock file forever: a Windows TempDir cleanup
// fails with "being used by another process" exactly like the flag-ON
// shadow run that found this (internal/app/p778_new_release_leak_test.go's
// setup, flag ON).
//
// The host-lock assertion is file removability (os.Remove of the .lock
// file, then os.RemoveAll of the data dir): on Windows a held lock file
// cannot be unlinked, which is the exact production signal from the shadow
// run. On Unix the unlink always succeeds, so those platforms assert the
// weaker property only -- the mutation is caught on the Windows CI job,
// which is where the defect lives.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// peNewFastModelFailureFixture is the p778 setup, copied (not edited): a
// configured provider keeps cfg.IsConfigured() true so New reaches
// InitCoderAgent, and the fast model points at an unconfigured provider so
// InitCoderAgent fails deep inside buildAgent -- well after the flagged
// batch install has already ensured the host identity. Returns the
// caller-owned writer connection (the caller must db.Release it, exactly
// like root.go's setupApp does on this error path) and the data dir.
func peNewFastModelFailureFixture(t *testing.T) (*config.ConfigStore, string) {
	t.Helper()
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)

	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0", // never dialed — build fails before any request
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "does-not-exist",
		Model:    "probe",
	})
	store.SetupAgents()
	require.True(t, store.Config().IsConfigured(), "precondition: at least one enabled provider so New reaches InitCoderAgent")
	return store, dataDir
}

// peNewBatchHostLockPath returns the host lock file the flagged install
// created under <dataDir>/hosts/, or "" when the graceful teardown already
// removed it (HostIdentity.Close deletes the file after a clean release --
// which is itself proof the lock was released).
func peNewBatchHostLockPath(t *testing.T, dataDir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "hosts"))
	if err != nil || len(entries) == 0 {
		return ""
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			return filepath.Join(dataDir, "hosts", e.Name())
		}
	}
	return ""
}

// (1) Flag ON, InitCoderAgent fails: after New returns, the batch host lock
// file must be RELEASED (removable on Windows) and the whole data dir
// removable. Catches the mutation "New's InitCoderAgent error path does not
// call peReleaseBatchRuntimeOnNewError": the HostStore OS lock stays held
// and the os.Remove/os.RemoveAll assertions fail on Windows with
// "being used by another process" -- the shadow-run failure.
func TestAppNewErrorRelease_BatchHostLockNotHeldOnInitCoderAgentFailure(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	store, dataDir := peNewFastModelFailureFixture(t)

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.Error(t, err, "New must fail via InitCoderAgent's fast-model error")
	require.Nil(t, application, "New must return a nil *App on this error path")

	require.NoError(t, db.Release(dataDir), "caller's single Release must fully close the pool")

	// The mutation catcher, in two shapes. If the file survived the
	// teardown it must at least be REMOVABLE (on Windows a still-held
	// hosts/<uuid>.lock fails os.Remove exactly like the shadow run's
	// "being used by another process"); if the graceful release already
	// deleted it, the whole data dir must still be removable.
	lockPath := peNewBatchHostLockPath(t, dataDir)
	if lockPath != "" {
		require.NoError(t, os.Remove(lockPath),
			"the batch host lock file must be released after New fails on the flag-ON path")
	}
	require.NoError(t, os.RemoveAll(dataDir),
		"the data dir must be removable once New's error path released the host identity")
}

// (2) The task #778 assertion still holds on the flag-ON path: New released
// exactly its own ConnectRead reference, so the caller's single Release
// drops the pool entry to zero and the writer connection closes. Catches
// the mutation "the new teardown helper released the caller's conn
// reference too (double-release)" -- the caller's Release would then error
// (or the Ping would still succeed if the helper UNDER-released).
func TestAppNewErrorRelease_ConnectReadStillReleasedFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	store, dataDir := peNewFastModelFailureFixture(t)

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.Error(t, err)
	require.Nil(t, application)

	require.NoError(t, db.Release(dataDir),
		"exactly the caller's one reference must remain after New failed")
	require.Error(t, conn.PingContext(context.Background()),
		"the writer connection must be fully closed after the caller's single Release")
}

// (3) Flag OFF: New's error path is unchanged -- the lazy legacy store never
// registered a host during New (no hosts lock file exists), and the task
// #778 reference accounting holds exactly as before the fix. Catches the
// mutation "the helper runs (or releases anything) on the flag-OFF path":
// the flag-OFF branch of peReleaseNewErrorHostIdentity is a documented
// no-op here because Close on a never-ensured store does nothing, and the
// hosts dir must still be absent.
func TestAppNewErrorRelease_FlagOffPathUnchanged(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	store, dataDir := peNewFastModelFailureFixture(t)

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.Error(t, err)
	require.Nil(t, application)

	_, statErr := os.Stat(filepath.Join(dataDir, "hosts"))
	require.True(t, os.IsNotExist(statErr),
		"flag OFF: New must not create the hosts dir (no host registration before the first Claim)")

	require.NoError(t, db.Release(dataDir))
	require.Error(t, conn.PingContext(context.Background()),
		"flag OFF: the reference accounting is unchanged")
	require.NoError(t, os.RemoveAll(dataDir))
}
