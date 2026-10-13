package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func batchFactoryApp(t *testing.T) *App {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	a := &App{dataDir: dataDir}
	a.asyncJobStore = session.NewAsyncJobStore(conn, dataDir, 1, "test")
	a.Messages = message.NewService(db.New(conn))
	a.DB = func() *sql.DB { return conn }
	return a
}

func batchFactoryDeps() agent.BatchInstallDeps {
	return agent.BatchInstallDeps{Host: "host", RunnerID: "runner-1", Owner: "owner"}
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
	t.Cleanup(rt.Stop)
	require.NotNil(t, rt.Runner(), "Start constructs the Runner (P1-6 RunnerFactory)")
}
