package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func seedInterruptReaderMessage(t *testing.T, ctx context.Context, writer *sql.DB, id string) {
	t.Helper()
	_, err := db.New(writer).CreateMessage(ctx, db.CreateMessageParams{ID: id, SessionID: "owner", Role: "user", Parts: "[]"})
	require.NoError(t, err)
}

func TestPeekInterruptInjectDoesNotQueueBehindWriter(t *testing.T) {
	for _, withPending := range []bool{false, true} {
		name := "empty"
		if withPending {
			name = "oldest-committed-interrupt"
		}
		t.Run(name, func(t *testing.T) {
			ctx, writer, reader := batchReaderTestDBs(t)
			require.NotSame(t, writer, reader, "the regression fixture requires a separate WAL reader")
			svc := NewServiceWithReader(db.New(writer), writer, db.New(reader), reader)
			if withPending {
				seedInterruptReaderMessage(t, ctx, writer, "message")
				for _, row := range []PendingInject{
					{ID: "old", SessionID: "owner", MessageID: "message", Interrupt: true, CreatedAt: 2},
					{ID: "merge", SessionID: "owner", MessageID: "message", Interrupt: false, CreatedAt: 1},
					{ID: "new", SessionID: "owner", MessageID: "message", Interrupt: true, CreatedAt: 3},
				} {
					require.NoError(t, svc.CreatePendingInject(ctx, row))
				}
			}
			hold, err := writer.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer hold.Rollback()
			var one int
			require.NoError(t, hold.QueryRowContext(ctx, "SELECT 1").Scan(&one))
			if withPending {
				_, err = hold.ExecContext(ctx, `INSERT INTO pending_injects
					(id, session_id, message_id, content, interrupt, created_at)
					VALUES ('uncommitted', 'owner', 'message', '', 1, 0)`)
				require.NoError(t, err)
			}
			before := writer.Stats()
			require.Equal(t, 1, before.MaxOpenConnections)
			require.Equal(t, 1, before.InUse, "writer must remain checked out throughout peek")
			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var result *PendingInject
			var peekErr error
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("phase=peek-with-held-writer pending=%t writer_before=%+v writer_after=%+v reader_after=%+v context_error=%v peek_error=%v result=%+v",
						withPending, before, writer.Stats(), reader.Stats(), readCtx.Err(), peekErr, result)
				}
			})
			result, peekErr = svc.PeekInterruptInject(readCtx, "owner")
			require.NoError(t, peekErr, "a pure peek must not inherit the busy writer's deadline")
			if withPending {
				require.NotNil(t, result)
				require.Equal(t, "old", result.ID, "ignore merge rows and uncommitted interrupts")
				require.True(t, result.Interrupt)
			} else {
				require.Nil(t, result)
			}
			require.NoError(t, hold.Rollback())
			if withPending {
				again, err := svc.PeekInterruptInject(ctx, "owner")
				require.NoError(t, err)
				require.NotNil(t, again)
				require.Equal(t, result.ID, again.ID, "peek must not consume the interrupt")
				var count int
				require.NoError(t, writer.QueryRowContext(ctx, "SELECT count(*) FROM pending_injects WHERE session_id='owner'").Scan(&count))
				require.Equal(t, 3, count, "all committed rows survive, the uncommitted row rolls back")
			}
		})
	}
}

func TestPeekInterruptInjectRetainsSinglePoolFallback(t *testing.T) {
	ctx, writer, _ := batchReaderTestDBs(t)
	seedInterruptReaderMessage(t, ctx, writer, "message")
	for _, svc := range []Service{
		NewService(db.New(writer), writer),
		NewServiceWithReader(db.New(writer), writer, nil, nil),
	} {
		row := PendingInject{ID: "fallback", SessionID: "owner", MessageID: "message", Interrupt: true, CreatedAt: 1}
		if exists, err := svc.PeekInterruptInject(ctx, "owner"); err != nil || exists == nil {
			require.NoError(t, err)
			require.NoError(t, svc.CreatePendingInject(ctx, row))
		}
		peek, err := svc.PeekInterruptInject(ctx, "owner")
		require.NoError(t, err)
		require.NotNil(t, peek)
		require.Equal(t, row.ID, peek.ID)
	}
}

func TestPeekInterruptInjectPropagatesReaderFailure(t *testing.T) {
	ctx, writer, reader := batchReaderTestDBs(t)
	require.NotSame(t, writer, reader)
	svc := NewServiceWithReader(db.New(writer), writer, db.New(reader), reader)
	require.NoError(t, reader.Close())
	_, err := svc.PeekInterruptInject(ctx, "owner")
	require.ErrorContains(t, err, "database is closed", "do not silently reroute failed reads back through the writer")
}
