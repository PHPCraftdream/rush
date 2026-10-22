// H4 batch-only: the CLI loop's durable driver claim must go through the
// job-independent session.HostStore (released by the shutdown drain gate)
// and no legacy AsyncJobStore exists to ensure a host -- before this fix the
// coordinator's driver claim registered a legacy hosts/<id>.lock that the
// shutdown never released (the CI Windows unlinkat failure).
package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestFlagOnDriverClaimUsesHostStore_NoLegacyLock drives the EXACT
// production call cliLoop.claim makes (app_run_async.go ClaimExternalDriver)
// on a real batch-only app, then releases through the same production path
// and shuts down through the real graceful gate.
//
// Revert-check: pointing the coordinator's driver-marker dispatch back at a
// legacy host identity would register a legacy hosts/<id>.lock -- the
// post-shutdown lock re-acquire below fails with a sharing violation on
// Windows.
func TestFlagOnDriverClaimUsesHostStore_NoLegacyLock(t *testing.T) {
	// Same global-config isolation as newLockBusyCLITestApp so nothing
	// touches the real machine config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID: "openaicompat", Type: openaicompat.Name, BaseURL: srv.URL, APIKey: "probe",
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := application.Sessions.Create(ctx, "flag-on-driver-claim-test")
	require.NoError(t, err)

	// The exact call cliLoop.claim makes (app_run_async.go:361), driven
	// directly instead of through SSE turns -- faithful to production.
	source, ok := application.AgentCoordinator.(agent.ReactionDebtSource)
	require.True(t, ok, "coordinator must implement ReactionDebtSource")
	require.NoError(t, source.ClaimExternalDriver(ctx, sess.ID), "flag ON: the claim must succeed through the HostStore")

	// Core oracle: the job-independent HostStore identity WAS registered by
	// the claim -- the driver claim ensures exactly one host identity, and
	// it is the HostStore's.
	hostID := application.BatchHostStore().HostID()
	require.NotEmpty(t, hostID, "batch-only: the claim registered the HostStore identity")

	// Release symmetric: the production release path removes the marker.
	source.ReleaseExternalDriver(ctx, sess.ID)
	var drivers int
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_drivers WHERE session_id = ?`, sess.ID).Scan(&drivers))
	require.Zero(t, drivers, "ReleaseExternalDriver removed the durable driver marker")

	res := application.ShutdownWithResult()
	require.False(t, res.Forced, "graceful shutdown")
	require.Empty(t, res.CleanupErrors)

	// All-OS proof the OS host lock was released by the shutdown drain gate:
	// an exclusively re-acquirable lock (on Windows an open handle fails
	// with a sharing violation) means no host lock leaked.
	lock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, hostID))
	require.NoError(t, err, "the HostStore OS lock was released by shutdown; a failure here means a host lock leaked")
	if lock != nil {
		t.Cleanup(func() { _ = lock.Release() })
	}
}
