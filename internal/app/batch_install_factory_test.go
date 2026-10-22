package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchFactoryApp is the explicit batch-construction fixture: a
// hand-assembled App over a real data dir and writer connection, with the
// batch side built through the production seams only (batchHostStore() memoizes
// the session.HostStore over the App's writer on first use; batchHostEnsure
// registers the host identity). No legacy store exists in the final world.
func batchFactoryApp(t *testing.T) *App {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	a := &App{dataDir: dataDir}
	a.Messages = message.NewService(db.New(conn))
	a.DB = func() *sql.DB { return conn }
	return a
}

func batchFactoryDeps() agent.BatchInstallDeps {
	return agent.BatchInstallDeps{Host: "host", RunnerID: "runner-1", Owner: "owner", DiskBindings: agent.NewDiskProviderRegistry()}
}

func TestBatchRuntimeFactoryFailsClosedWithoutResources(t *testing.T) {
	t.Parallel()
	_, err := newBatchRuntimeFactory(nil, batchFactoryDeps())
	require.Error(t, err)
	_, err = newBatchRuntimeFactory(&App{}, batchFactoryDeps())
	require.Error(t, err, "no agent store")
	a := batchFactoryApp(t)
	a.Messages = nil
	_, err = newBatchRuntimeFactory(a, batchFactoryDeps())
	require.Error(t, err, "no message service")
}

// build constructs the adapted install and attaches the Runner; the fault
// watcher stays disarmed and no store row is written (no side effects until
// Start).
func TestBatchRuntimeFactoryBuildDormant(t *testing.T) {
	t.Parallel()
	a := batchFactoryApp(t)
	factory, err := newBatchRuntimeFactory(a, batchFactoryDeps())
	require.NoError(t, err)
	require.Equal(t, "host", factory.deps.Host, "explicit host identity wins")
	require.NotNil(t, factory.deps.Store)
	require.NotNil(t, factory.deps.Messages)
	rt, install, err := factory.build(context.Background())
	require.NoError(t, err)
	t.Cleanup(rt.Stop)
	// R11F P1-6: the deferred install keeps NO Runner at build — the Runner
	// is constructed at BatchRuntime.Start through the RunnerFactory.
	require.Nil(t, rt.Runner(), "dormant build constructs no Runner")
	require.False(t, rt.Fenced(), "dormant build is not fenced")
	require.NotNil(t, install.Runtime)
	require.NotNil(t, install.Delivery)
	require.NotNil(t, install.NoticePull)
	rt.Start()
	require.NotNil(t, rt.Runner(), "Start constructs the Runner (P1-6 RunnerFactory)")
	// jb-r11f.ci1#2 (T2): the checked Close drains the Runner — Stop alone does
	// NOT (app_batch_runtime.go Stop never drains). Registered before Stop so
	// LIFO runs Stop first, then bounded Close joins shutdown.
	runner := rt.Runner()
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, runner.Close(closeCtx), "Close drains the Runner")
	})
	t.Cleanup(rt.Stop)
}
