package app

// F7b regression pin: with RUSH_BATCH_RUNTIME=1, an ephemeral library-mode
// App (dataDir == "") built by F7 owned an in-memory DiskProviderRegistry
// (app_ephemeral_disk_binding.go) and app_run_setup.go bound per-call Call
// owners against it — but the coordinator's diskCaps was never bound. Only
// BindCoordinatorBatchRuntimeBindings did that, and an ephemeral App has no
// batch runtime install. coordinator.PrepareContinuation therefore failed
// closed with agent.ErrDiskProviderUnavailable ("no capability manager is
// bound") for every session with a persisted execution requirement — the
// root cause of the three failing SDK tests (sdk/r16_6_disk_provider_test.go,
// sdk/r5_6_library_virtual_root_test.go, sdk/r6_1_library_mode_diskless_test.go).
// This test pins the InitCoderAgent wiring that fixes it.
//
// Compile-and-CI test: run by CI, never in this worktree.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// openEphemeralWireTestDB is this file's own in-memory database helper,
// modeled on openEphemeralTestDB: a named shared-cache in-memory SQLite
// database with a single pooled connection, so the App under test has
// nothing on the host disk.
func openEphemeralWireTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	var driver string
	for _, candidate := range []string{"sqlite", "sqlite3"} {
		for _, d := range sql.Drivers() {
			if d == candidate {
				driver = candidate
			}
		}
	}
	require.NotEmpty(t, driver, "no SQLite driver registered for the in-memory database (have %v)", sql.Drivers())
	conn, err := sql.Open(driver, fmt.Sprintf("file:rush_ephemeral_wire_test_%s?mode=memory&cache=shared", uuid.NewString()))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	require.NoError(t, conn.PingContext(ctx))
	require.NoError(t, db.Migrate(ctx, conn))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestEphemeralAppCoordinatorBindsEphemeralDiskCaps(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	isolateAppNewTestEnv(t)

	ctx := context.Background()
	conn := openEphemeralWireTestDB(t, ctx)

	// The provider probe never needs to be called; its existence satisfies
	// config validation without a network endpoint (same as newFullNewAppAt).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	store := config.NewLibraryStore(&config.Config{
		Options: &config.Options{
			DataDirectory:   "",
			NoRealWorkspace: true,
			// The bash tool dereferences Attribution while the ready gate
			// builds the tool set; a bare Options (no config defaults)
			// would panic that goroutine.
			Attribution: &config.Attribution{TrailerStyle: config.TrailerStyleNone},
		},
		Providers:    csync.NewMap[string, config.ProviderConfig](),
		Models:       make(map[config.SelectedModelType]config.SelectedModel),
		MCP:          config.MCPs{},
		RecentModels: make(map[config.SelectedModelType][]config.SelectedModel),
	}, t.TempDir())
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID: "openaicompat", Type: openaicompat.Name, BaseURL: srv.URL, APIKey: "probe",
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()

	application, err := New(ctx, conn, store, SkipMCP())
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	// Preconditions: the flag-ON ephemeral branch built its own registry and
	// never installed the batch runtime.
	require.NotNil(t, application.AgentCoordinator)
	require.NotNil(t, application.ephemeralDiskBindings, "flag-ON ephemeral App must own an in-memory binding registry")
	require.Nil(t, application.batchInstall, "ephemeral App never installs the batch runtime")

	// Register a live binding in the App's OWN registry, exactly the way
	// app_run_setup.go's per-call BindCallDiskProvider does.
	fake := newLeakFakeDisk()
	opts, release, err := agent.BindCallDiskProvider(application.resolveDiskBindings(), fake, "f7b-wire", &agent.CallOptions{})
	require.NoError(t, err)
	defer release()
	require.NotEmpty(t, opts.DiskBindingID)

	// Persist a fence naming that binding, like a custom-DiskProvider
	// admission does.
	reqStore, ok := application.Sessions.(session.SessionExecutionRequirementStore)
	require.True(t, ok)
	wireSess, err := application.Sessions.Create(ctx, "f7b wire")
	require.NoError(t, err)
	require.NoError(t, reqStore.StoreSessionExecutionRequirement(ctx, wireSess.ID, session.SessionExecutionRequirement{BindingID: opts.DiskBindingID}))

	// THE REGRESSION ASSERTION: with the InitCoderAgent wiring bound, the
	// coordinator must resolve the session's fence through the App-owned
	// ephemeral registry — before the fix this failed with
	// agent.ErrDiskProviderUnavailable: no capability manager is bound.
	// Production mutation caught: deleting the ephemeralDiskBindings wiring
	// in InitCoderAgent (or the SetCoordinatorDiskCapabilityManager bind)
	// makes diskCaps nil again and this exact call returns the typed
	// sentinel. PrepareContinuation is a method on the unexported
	// coordinator type whose return type is unexported too, so package app
	// reaches it only via reflection.
	m := reflect.ValueOf(application.AgentCoordinator).MethodByName("PrepareContinuation")
	require.True(t, m.IsValid(), "coordinator must expose PrepareContinuation")
	var source *session.SessionExecutionRequirement
	out := m.Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(wireSess.ID), reflect.ValueOf(source)})
	errVal, _ := out[1].Interface().(error)
	require.NoError(t, errVal, "with the wiring bound, the coordinator must resolve the session's fence through the App-owned ephemeral registry — before the fix this failed with agent.ErrDiskProviderUnavailable: no capability manager is bound")
	require.NotNil(t, out[0].Interface(), "a resolved continuation must be returned")

	// Fail-closed pin (same registry, adversarial binding): a fence whose
	// binding was never registered must return the typed sentinel, proving
	// the coordinator actually consults the App-owned registry rather than
	// passing vacuously. (A second call on "sdk-f7b-wire" would NOT fail:
	// the successful resolution above acquired a Continuation owner whose
	// release lives inside the unexported *preparedContinuation, so the
	// binding stays retained — the negative case needs its own session.)
	lostSess, err := application.Sessions.Create(ctx, "f7b wire lost")
	require.NoError(t, err)
	require.NoError(t, reqStore.StoreSessionExecutionRequirement(ctx, lostSess.ID, session.SessionExecutionRequirement{BindingID: "f7b-never-registered"}))
	out2 := m.Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(lostSess.ID), reflect.ValueOf(source)})
	errVal2, _ := out2[1].Interface().(error)
	require.ErrorIs(t, errVal2, agent.ErrDiskProviderUnavailable, "a fence naming an unregistered binding must fail closed through the App-owned registry")
	require.Nil(t, out2[0].Interface(), "no continuation may be resolved for a lost binding")
}
