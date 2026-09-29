// C1 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): `rush run
// --session <busy>` (or --continue against a session another process
// currently holds) must fail fast on the FIRST turn instead of silently
// retrying forever. Before this fix, runNonInteractiveWithAsyncResults'
// loop retried ExecuteRun on SessionLockBusyError with no firstTurn guard
// and no limit -- each retry re-ran ExecuteRun's own per-invocation session
// mutations (UpdateSystemPrompt, UpdateReasoningEffort, ClearCancelRequest,
// SetBudget, SetEndedReason) against a session another process owns.
package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newLockBusyCLITestApp is a trimmed-down copy of newAdmissionRaceApp
// (app_run_admission_race_test.go) that additionally returns dataDir, needed
// here to take the SAME real OS session lock a foreign process would hold.
func newLockBusyCLITestApp(t *testing.T, handler http.HandlerFunc) (application *App, sessionID, dataDir string) {
	t.Helper()

	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir = t.TempDir()
	srv := httptest.NewServer(handler)
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

	application, err = New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	sess, err := application.Sessions.Create(context.Background(), "lock-busy-cli-test")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	return application, sess.ID, dataDir
}

// TestRunNonInteractive_FirstTurnSessionLockBusy_FailsFast is the core
// regression test.
//
// Revert-check performed: removed the `if firstTurn { return final, err }`
// branch from app_run_async.go's lock-busy handling (leaving the general
// bounded-retry path to run for the first turn too) -- this test's
// `require.Less(t, elapsed, ...)` FAILED: the call took the full shrunk
// cliLockBusyRetryOverallLimit (~150ms in the shrunk-const test run) instead
// of returning immediately, and a real (non-shrunk) build would have taken
// the full 30s instead of the pre-phase-4 fast error. Restored the
// firstTurn guard; re-ran, passed (elapsed back under 50ms).
func TestRunNonInteractive_FirstTurnSessionLockBusy_FailsFast(t *testing.T) {
	origPause, origLimit := cliLockBusyRetryPause, cliLockBusyRetryOverallLimit
	cliLockBusyRetryPause = 20 * time.Millisecond
	cliLockBusyRetryOverallLimit = 150 * time.Millisecond
	t.Cleanup(func() { cliLockBusyRetryPause, cliLockBusyRetryOverallLimit = origPause, origLimit })

	application, sessionID, dataDir := newLockBusyCLITestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("provider must never be called: the session lock refusal must be caught before any turn runs")
	})

	foreignLock, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })

	start := time.Now()
	_, err = application.RunNonInteractiveWithResult(context.Background(), io.Discard, "hello",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	elapsed := time.Since(start)

	require.Error(t, err, "a busy session must surface as an error, not succeed silently")
	var lockBusy *session.SessionLockBusyError
	require.ErrorAs(t, err, &lockBusy, "the error must be (wrap) the real SessionLockBusyError")
	require.Less(t, elapsed, cliLockBusyRetryOverallLimit,
		"the FIRST turn must fail fast, never retry for the Drain-context overall budget")
}
