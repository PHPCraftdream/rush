package sessstate

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	sessbatch "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/batch"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// These deliberately configless fixtures exercise the private state header
// and the production batch why body without setting up global configuration.

func TestBatchRuntimeFacts_ConfiglessAppDegradesToUnknownNotPanics(t *testing.T) {
	conn, q := cmdtest.NewTestDB(t)
	a := &app.App{Messages: message.NewService(q), Sessions: coresession.NewService(q, conn)}
	require.Nil(t, a.DB, "precondition: no DB func wired")

	got := sesskit.BatchRuntimeFacts(context.Background(), a, t.TempDir(), []string{"sess-x"})
	require.NotNil(t, got, "a configless App's batch facts are UNAVAILABLE (unknown), never nil")
	f := got["sess-x"]
	require.True(t, f.Unknown, "an unreadable read is marked unknown")
	require.True(t, f.Live, "an unavailable read is conservatively live, never idle")

	var buf bytes.Buffer
	require.NoError(t, sessbatch.ExplainWhyBatch(context.Background(), a, t.TempDir(), "sess-x", &buf, printStallHeader))
	require.Contains(t, buf.String(), "batch unknown",
		"the configless why render reports the conservative unknown")
	require.NotContains(t, buf.String(), "status: at rest",
		"an unreadable batch must never read as idle")
}

func TestExplainWhyBatch_EmptyReadableDBReadsAtRest(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	a := &app.App{
		Messages: message.NewService(q),
		Sessions: coresession.NewService(q, conn),
		DB:       func() *sql.DB { return conn },
	}
	var buf bytes.Buffer
	require.NoError(t, sessbatch.ExplainWhyBatch(ctx, a, dataDir, "sess-empty", &buf, printStallHeader))
	require.Contains(t, buf.String(), "status: at rest",
		"a readable DB with no batch rows and no lock IS at rest")
}

func TestBatchRuntimeFacts_UnavailableDBVariantsMarkUnknown(t *testing.T) {
	ctx := context.Background()

	t.Run("nil App receiver", func(t *testing.T) {
		got := sesskit.BatchRuntimeFacts(ctx, nil, t.TempDir(), []string{"s1"})
		require.NotNil(t, got, "UNAVAILABLE facts, never nil")
		f := got["s1"]
		require.True(t, f.Unknown, "the nil-App branch marks the read unknown")
		require.True(t, f.Live, "an unavailable read is conservatively live, never idle")
	})

	t.Run("DB func returns nil", func(t *testing.T) {
		conn, q := cmdtest.NewTestDB(t)
		a := &app.App{
			Messages: message.NewService(q),
			Sessions: coresession.NewService(q, conn),
			DB:       func() *sql.DB { return nil },
		}
		got := sesskit.BatchRuntimeFacts(ctx, a, t.TempDir(), []string{"s1"})
		require.NotNil(t, got, "UNAVAILABLE facts, never nil")
		f := got["s1"]
		require.True(t, f.Unknown, "the nil-handle branch marks the read unknown")
		require.True(t, f.Live, "an unavailable read is conservatively live, never idle")

		var buf bytes.Buffer
		require.NoError(t, sessbatch.ExplainWhyBatch(ctx, a, t.TempDir(), "s1", &buf, printStallHeader))
		require.Contains(t, buf.String(), "batch unknown",
			"the nil-handle why render reports the conservative unknown")
		require.NotContains(t, buf.String(), "status: at rest",
			"an unreadable batch must never read as idle")
	})
}
