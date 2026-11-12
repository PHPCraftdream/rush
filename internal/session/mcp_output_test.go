package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func mcpOutputTestTransaction(conn *sql.DB) MCPOutputTransaction {
	return func(ctx context.Context, _ string, effect func(*sql.Tx) error) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if err := effect(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func mcpOutputTestStore(t *testing.T) (*MCPOutputStore, *sql.DB) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	_, err = db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: "output-owner", Title: "output owner"})
	require.NoError(t, err)
	tx := mcpOutputTestTransaction(conn)
	return NewMCPOutputStore(tx, tx), conn
}

func TestMCPOutputStoreAtomicReplayAndSessionCleanup(t *testing.T) {
	store, conn := mcpOutputTestStore(t)
	text := strings.Repeat("αβ\n", 24000) + "final tail"
	ref, err := store.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", "original-call", true, int64(len(text)), func(w io.Writer) error {
		_, err := io.WriteString(w, text)
		return err
	})
	require.NoError(t, err)
	require.NotEmpty(t, ref)
	// A new store has no in-memory artifact state; pages come from committed rows.
	replayed := NewMCPOutputStore(mcpOutputTestTransaction(conn), mcpOutputTestTransaction(conn))
	var got strings.Builder
	for offset := int64(0); offset < int64(len(text)); {
		tool, sourceError, total, page, err := replayed.ReadMCPOutput(t.Context(), "output-owner", ref, offset, 7001)
		require.NoError(t, err)
		require.Equal(t, "mcp_server_tool", tool)
		require.True(t, sourceError)
		require.EqualValues(t, len(text), total)
		require.LessOrEqual(t, len(page), 7001)
		require.NotEmpty(t, page, "every non-EOF byte page must advance")
		got.Write(page)
		offset += int64(len(page))
	}
	require.Equal(t, text, got.String(), "byte pages must replay every source byte exactly")
	_, _, _, page, err := replayed.ReadMCPOutput(t.Context(), "other-session", ref, 0, 100)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Nil(t, page)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = ?`, "output-owner")
	require.NoError(t, err)
	for _, table := range []string{"mcp_outputs", "mcp_output_chunks"} {
		var count int
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
		require.Zero(t, count, "session deletion must cascade through %s", table)
	}
}

func TestMCPOutputStorePartialActivationCannotInvokeOwnersOrExposeArtifacts(t *testing.T) {
	store, conn := mcpOutputTestStore(t)
	writes, reads := 0, 0
	mutate := func(ctx context.Context, owner string, effect func(*sql.Tx) error) error {
		writes++
		return store.mutate(ctx, owner, effect)
	}
	read := func(ctx context.Context, owner string, effect func(*sql.Tx) error) error {
		reads++
		return store.read(ctx, owner, effect)
	}
	complete := NewMCPOutputStore(mutate, read)
	render := func(w io.Writer) error { _, err := io.WriteString(w, "retained"); return err }
	ref, err := complete.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", "complete", true, 8, render)
	require.NoError(t, err)
	require.NotEmpty(t, ref)
	require.Equal(t, 1, writes, "a complete pair must retain normally")
	writes, reads = 0, 0
	for name, candidate := range map[string]*MCPOutputStore{
		"mutation owner only": NewMCPOutputStore(mutate, nil),
		"read owner only":     NewMCPOutputStore(nil, read),
		"no owners":           NewMCPOutputStore(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			newRef, err := candidate.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", "blocked", false, 8, render)
			require.Error(t, err)
			require.Empty(t, newRef)
			tool, sourceError, total, data, err := candidate.ReadMCPOutput(t.Context(), "output-owner", ref, 0, 100)
			require.Error(t, err)
			require.Empty(t, tool)
			require.False(t, sourceError)
			require.Zero(t, total)
			require.Nil(t, data)
			require.Zero(t, writes, "partial activation must not invoke a valid mutation callback")
			require.Zero(t, reads, "partial activation must not invoke a valid read callback")
			var count int
			require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_outputs`).Scan(&count))
			require.Equal(t, 1, count, "partial activation must not insert another manifest")
		})
	}
	tool, sourceError, total, data, err := complete.ReadMCPOutput(t.Context(), "output-owner", ref, 0, 100)
	require.NoError(t, err)
	require.Equal(t, "mcp_server_tool", tool)
	require.True(t, sourceError)
	require.EqualValues(t, 8, total)
	require.Equal(t, "retained", string(data))
	require.Equal(t, 1, reads, "a complete pair must replay normally")
}

func TestMCPOutputStoreFailureCancellationAndAdmissionAreAtomic(t *testing.T) {
	store, conn := mcpOutputTestStore(t)
	renderFailure := errors.New("capture stopped")
	for _, mode := range []string{"write failure", "size mismatch", "cancellation", "admission failure", "missing admission", "commit failure", "output quota"} {
		t.Run(mode, func(t *testing.T) {
			candidate := store
			ctx := t.Context()
			total := int64(mcpOutputChunkBytes + 10)
			render := func(w io.Writer) error {
				_, err := io.WriteString(w, strings.Repeat("x", int(total)))
				return err
			}
			switch mode {
			case "write failure":
				render = func(w io.Writer) error {
					_, err := io.WriteString(w, strings.Repeat("x", mcpOutputChunkBytes))
					if err != nil {
						return err
					}
					return renderFailure
				}
			case "size mismatch":
				render = func(w io.Writer) error { _, err := io.WriteString(w, "short"); return err }
			case "cancellation":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				render = func(w io.Writer) error { cancel(); _, err := io.WriteString(w, "cancelled"); return err }
			case "admission failure":
				candidate = NewMCPOutputStore(func(context.Context, string, func(*sql.Tx) error) error { return errors.New("finalized") }, store.read)
			case "missing admission":
				candidate = NewMCPOutputStore(nil, nil)
			case "commit failure":
				candidate = NewMCPOutputStore(func(ctx context.Context, _ string, effect func(*sql.Tx) error) error {
					tx, err := conn.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback() //nolint:errcheck
					if err := effect(tx); err != nil {
						return err
					}
					return errors.New("commit refused")
				}, store.read)
			case "output quota":
				total = MCPOutputLimit + 1
			}
			ref, err := candidate.RetainMCPOutput(ctx, "output-owner", "mcp_server_tool", "failure-call", false, total, render)
			require.Error(t, err)
			if mode == "commit failure" {
				require.ErrorContains(t, err, "commit refused")
			}
			if mode == "admission failure" {
				require.ErrorContains(t, err, "finalized")
			}
			require.Empty(t, ref, "a failed capture or commit must never publish a reference")
			for _, table := range []string{"mcp_outputs", "mcp_output_chunks"} {
				var count int
				require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
				require.Zero(t, count)
			}
		})
	}
}

func TestMCPOutputStoreAppQuotaAndSessionDeletionFreeCapacity(t *testing.T) {
	store, conn := mcpOutputTestStore(t)
	for i := range 4 {
		owner := fmt.Sprintf("retained-%d", i)
		_, err := db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: owner, Title: owner})
		require.NoError(t, err)
		for j := range 8 {
			_, err := conn.ExecContext(t.Context(), `INSERT INTO mcp_outputs (id, session_id, tool_name, tool_call_id, is_error, total_bytes) VALUES (?, ?, 'mcp_server_tool', 'old', 0, ?)`, fmt.Sprintf("app-quota-%d-%d", i, j), owner, MCPOutputLimit)
			require.NoError(t, err)
		}
	}
	render := func(w io.Writer) error { _, err := io.WriteString(w, "new"); return err }
	ref, err := store.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", "new", false, 3, render)
	require.ErrorIs(t, err, ErrMCPOutputQuota)
	require.Empty(t, ref)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = 'retained-0'`)
	require.NoError(t, err)
	ref, err = store.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", "new", false, 3, render)
	require.NoError(t, err)
	require.NotEmpty(t, ref)
}

func TestMCPOutputStoreConcurrentQuotaReservation(t *testing.T) {
	store, conn := mcpOutputTestStore(t)
	// Exercise SQLite transaction exclusion, not only the writer pool's queue.
	conn.SetMaxOpenConns(2)
	// Quota-only existing manifests avoid generating irrelevant large payloads.
	pageSize := int64(mcpOutputChunkBytes + 1)
	for i := range 8 {
		size := int64(MCPOutputLimit)
		if i == 7 {
			size -= pageSize
		}
		_, err := conn.ExecContext(t.Context(), `INSERT INTO mcp_outputs (id, session_id, tool_name, tool_call_id, is_error, total_bytes) VALUES (?, ?, ?, ?, 0, ?)`, fmt.Sprintf("quota-%d", i), "output-owner", "mcp_server_tool", "old", size)
		require.NoError(t, err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.RetainMCPOutput(t.Context(), "output-owner", "mcp_server_tool", fmt.Sprintf("concurrent-%d", i), false, pageSize, func(w io.Writer) error {
				_, err := io.WriteString(w, strings.Repeat("q", int(pageSize)))
				return err
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	require.Equal(t, 1, succeeded)
	var bytes int64
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT SUM(total_bytes) FROM mcp_outputs WHERE session_id = ?`, "output-owner").Scan(&bytes))
	require.EqualValues(t, MCPOutputSessionLimit, bytes)
}
