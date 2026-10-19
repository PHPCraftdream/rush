package app

// P1 (2026-10-19): with RUSH_BATCH_RUNTIME ON, an ephemeral SDK library-mode
// App (dataDir == "") rejected every supported custom-DiskProvider run with
// agent.ErrDiskProviderUnavailable, because app_run_setup.go gated the per-call
// BindCallDiskProvider on app.batchInstall.DiskBindings and an ephemeral App
// never installs the batch runtime (it has no asyncJobStore to install it
// from). The fix gives such an App its own purely in-memory
// agent.DiskProviderRegistry (app_ephemeral_disk_binding.go), consulted via
// App.resolveDiskBindings; persisted Apps keep binding through the batch
// runtime install, and a persisted App with NO install still fails closed.
//
// Compile-and-CI test: run by CI, never in this worktree.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// openEphemeralTestDB mirrors sdk's openMemoryDB (sdk/library_mode.go): a
// named shared-cache in-memory SQLite database, one pooled connection, so
// the ephemeral App under test has nothing on the host disk — the exact
// shape app.New receives from the SDK ephemeral path. db.Connect is
// deliberately NOT used: it is hard-wired to a file path under a data
// directory, which is what an ephemeral session must not have.
func openEphemeralTestDB(t *testing.T, ctx context.Context) *sql.DB {
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
	conn, err := sql.Open(driver, fmt.Sprintf("file:rush_app_ephemeral_test_%s?mode=memory&cache=shared", uuid.NewString()))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	require.NoError(t, conn.PingContext(ctx))
	require.NoError(t, db.Migrate(ctx, conn))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newEphemeralDiskBindingApp builds the App the way sdk.Open's ephemeral
// library-mode path does: an in-memory database, a library ConfigStore whose
// Options.DataDirectory is "" (no data dir at all) and NoRealWorkspace is
// true, and SkipMCP (library mode deliberately takes no process-wide MCP
// owner). RUSH_BATCH_RUNTIME is set to "1" BEFORE New, exactly like the SDK
// process env would carry it — the ephemeral branch under test fires inside
// New itself.
func newEphemeralDiskBindingApp(t *testing.T) *App {
	t.Helper()
	isolateAppNewTestEnv(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	ctx := context.Background()
	conn := openEphemeralTestDB(t, ctx)
	// Providers/Models/RecentModels are initialized exactly like the SDK's
	// buildLibraryConfig: New reads cfg.IsConfigured() and a nil provider
	// map would panic there.
	store := config.NewLibraryStore(&config.Config{
		Options: &config.Options{
			DataDirectory:   "",
			NoRealWorkspace: true,
		},
		Providers:    csync.NewMap[string, config.ProviderConfig](),
		Models:       make(map[config.SelectedModelType]config.SelectedModel),
		MCP:          config.MCPs{},
		RecentModels: make(map[config.SelectedModelType][]config.SelectedModel),
	}, t.TempDir())

	application, err := New(ctx, conn, store, SkipMCP())
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}

// TestEphemeralAppFlagOnCustomDiskProviderBindsInMemory (P1): an ephemeral
// App (dataDir == "") built with the flag ON must admit a supported
// custom-DiskProvider call setup through prepareExecuteRun — the exact seam
// that returned agent.ErrDiskProviderUnavailable before the fix — binding
// the Call owner against the App's OWN in-memory registry, and the owner
// must be released exactly once via the setup's callBindingRelease (the
// release ExecuteRun defers at internal/app/app_run.go:81).
func TestEphemeralAppFlagOnCustomDiskProviderBindsInMemory(t *testing.T) {
	application := newEphemeralDiskBindingApp(t)

	// Before the fix: application.batchInstall is nil (no data dir, no
	// asyncJobStore, no batch runtime install) and there was NO fallback, so
	// prepareExecuteRun failed closed with agent.ErrDiskProviderUnavailable
	// before any binding could exist. The mutation this test catches is
	// resolveDiskBindings dropping its ephemeralDiskBindings fallback (or
	// app.New's else-branch not constructing it): the call setup below then
	// returns the sentinel and every require after it fails.
	require.Nil(t, application.batchInstall, "ephemeral App never installs the batch runtime")
	require.NotNil(t, application.ephemeralDiskBindings, "flag-ON ephemeral App must own an in-memory binding registry")

	// A supported custom-DiskProvider call needs at least one folder scope
	// that keeps no command tools (app_run_setup.go's hard errors (1)/(2)).
	// The scope is canonicalized THROUGH the fake provider, so seed the dir
	// there; nothing touches the real host disk. Reuses the package's
	// leakFakeDisk harness (app_run_construction_leak_test.go).
	fake := newLeakFakeDisk()
	workDir := application.config.WorkingDir()
	fake.seedDir(workDir)
	fake.seedDir(filepath.ToSlash(workDir))

	_, cancel, setup, err := application.prepareExecuteRun(context.Background(), RunRequest{
		HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{
				Dir: workDir,
				Ops: []permission.FileOp{permission.FileOpRead},
			}},
			DiskProvider: fake,
		},
	})
	require.NoError(t, err, "flag-ON ephemeral App must admit a supported custom-DiskProvider call setup")
	require.NotNil(t, cancel)
	defer cancel()
	require.NotNil(t, setup)
	require.NotNil(t, setup.callOpts.DiskBindingID, "the call must carry an immutable disk binding handle")

	// The Call owner is retained exactly once against the App-owned
	// in-memory registry (R12-H2 §1.2) — never against a batch runtime
	// registry, which does not exist here.
	owners, _, _ := application.ephemeralDiskBindings.OwnerDiagnostics()
	require.Equal(t, 1, owners, "exactly one Call owner must be retained while the binding is live")

	// callBindingRelease is what ExecuteRun defers (app_run.go:81); calling
	// it here stands in for that single outer release. Exactly once: the
	// registry must return to completely empty — no leaked owner, no pinned
	// construction, no registry entry.
	setup.callBindingRelease()
	owners, constructions, registries := application.ephemeralDiskBindings.OwnerDiagnostics()
	require.Equal(t, 0, owners, "the Call owner must be released exactly once")
	require.Equal(t, 0, constructions)
	require.Equal(t, 0, registries)
}

// TestPersistedAppWithoutBatchInstallStillFailsClosed (P1, sentinel
// contract): a persisted App (data dir set) whose batch runtime install did
// NOT happen — here: built with the flag OFF so New never installed it —
// must still return agent.ErrDiskProviderUnavailable from the call setup
// once the flag is observed ON at run time. The mutation this test catches
// is resolveDiskBindings (or its call site) failing OPEN for a persisted App
// with no install — e.g. synthesizing an ad-hoc registry at run time instead
// of failing closed: then prepareExecuteRun below would succeed and both
// requires after it fail.
func TestPersistedAppWithoutBatchInstallStillFailsClosed(t *testing.T) {
	// Built through the real New with the flag genuinely off: no
	// asyncJobStore-driven batch runtime install, so batchInstall is nil
	// exactly like a persisted App whose install did not happen.
	unsetBatchRuntimeEnv(t)
	application := newFullNewApp(t)
	require.Nil(t, application.batchInstall, "precondition: flag-off persisted App has no batch runtime install")
	require.Nil(t, application.ephemeralDiskBindings, "persisted Apps never get an ephemeral registry")

	// The flag is read at RUN time (batchRuntimeFlagEnabled inside
	// prepareExecuteRun), so a flag-ON run against this install-less App
	// takes the exact same gate the ephemeral fix touched.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	fake := newLeakFakeDisk()
	workDir := application.config.WorkingDir()
	fake.seedDir(workDir)
	fake.seedDir(filepath.ToSlash(workDir))

	_, _, setup, err := application.prepareExecuteRun(context.Background(), RunRequest{
		HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{
				Dir: workDir,
				Ops: []permission.FileOp{permission.FileOpRead},
			}},
			DiskProvider: fake,
		},
	})
	require.ErrorIs(t, err, agent.ErrDiskProviderUnavailable, "persisted App with no batch install must still fail closed")
	require.Nil(t, setup, "no setup may be returned on the fail-closed path")
}
