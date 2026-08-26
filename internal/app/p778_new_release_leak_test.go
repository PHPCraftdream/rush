package app

// Task #778 (P1 release finding): App.New calls db.ConnectRead internally
// (see New's own doc comment above that call), taking a SECOND reference on
// the same pool entry the caller's db.Connect already referenced once. If
// New fails AFTER that ConnectRead succeeds (e.g. InitCoderAgent erroring),
// it used to return (nil, err) without releasing the reference it took
// itself, leaking both the pool entry's refCount and its OS file handle
// forever — the caller's own `defer a.Shutdown()` never runs because `a` is
// nil, and nothing else ever calls Release for New's own ConnectRead.
//
// Ownership split enforced by the fix (see app.go's InitCoderAgent error
// branch and internal/cmd/root.go's setupApp/setupAppLite):
//   - New released the ONE reference it took itself (ConnectRead) on this
//     error path.
//   - New never released the caller's own db.Connect reference — it never
//     owned it. That remains the caller's job (root.go's setupApp/
//     setupAppLite already release it on this same error path).
//
// This test forces the InitCoderAgent failure path deterministically by
// giving the fast model slot a provider that isn't in cfg.Providers
// (errFastModelProviderNotConfigured, from buildModelsFromCfg via
// buildAgent) while an unrelated provider keeps cfg.IsConfigured() == true
// so New reaches InitCoderAgent instead of returning early at the
// "!cfg.IsConfigured()" branch.
//
// Non-vacuousness: reverting the `if readConn != nil { db.Release(...) }`
// block added to app.go's InitCoderAgent error branch reproduces the leak
// and this test fails (verified manually — see the task report).

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestAppNew_ReleasesOwnConnectReadRefOnInitCoderAgentFailure(t *testing.T) {
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)

	// A configured, otherwise-valid provider so cfg.IsConfigured() is true
	// (New must reach InitCoderAgent, not return early).
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
	// Fast model points at a provider that is NOT configured at all, so
	// buildModelsFromCfg fails with errFastModelProviderNotConfigured deep
	// inside InitCoderAgent -> buildAgent, well after New's own ConnectRead
	// has already succeeded.
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "does-not-exist",
		Model:    "probe",
	})
	store.SetupAgents()
	require.True(t, store.Config().IsConfigured(), "precondition: at least one enabled provider so New reaches InitCoderAgent")

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	// Precondition: exactly one reference outstanding (this Connect call)
	// before New runs.
	require.NoError(t, conn.PingContext(context.Background()))

	application, err := New(context.Background(), conn, store)
	require.Error(t, err, "New must fail via InitCoderAgent's fast-model-provider-not-configured error")
	require.Nil(t, application, "New must return a nil *App on this error path")

	// The caller (this test, standing in for root.go's setupApp/
	// setupAppLite) still owns exactly ONE reference: its own db.Connect
	// above. New must have already released the ConnectRead reference IT
	// took internally. So a single Release here must fully close the pool
	// entry — proving refCount returned to zero, not one (which would mean
	// New's internal reference leaked).
	require.NoError(t, db.Release(dataDir))

	require.Error(t, conn.PingContext(context.Background()),
		"the writer connection must be fully closed after exactly one caller Release — "+
			"if this Ping still succeeds, New leaked its own ConnectRead reference "+
			"(refCount stayed above zero after the caller's single Release)")
}
