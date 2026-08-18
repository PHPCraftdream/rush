// Cross-process pending-inject queue tests: enqueue/drain round-trips,
// interrupt-row survival, and the peeked-row-matched atomic contract of
// ConsumeInterruptInjectAndEnqueue.
package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPendingInjects exercises the cross-process inject queue foundation:
// enqueue a row, drain it (which must return it AND delete it), and confirm a
// second drain is empty. It also checks that interrupt rows are surfaced via
// the hasInterrupt flag but neither returned in the merge slice nor deleted.
func TestPendingInjects(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "inject sess")
	require.NoError(t, err)

	t.Run("create, drain returns and deletes, re-drain empty", func(t *testing.T) {
		err := svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID,
			MessageID: "msg-1",
			Content:   "hello from another process",
		})
		require.NoError(t, err)

		merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, hasInterrupt)
		require.Len(t, merge, 1)
		assert.Equal(t, "msg-1", merge[0].MessageID)
		assert.Equal(t, "hello from another process", merge[0].Content)
		assert.False(t, merge[0].Interrupt)
		assert.NotEmpty(t, merge[0].ID)

		// Second drain must be empty (delete-after-read).
		merge2, hasInterrupt2, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, hasInterrupt2)
		assert.Empty(t, merge2)
	})

	t.Run("interrupt rows are reported but not drained", func(t *testing.T) {
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-int", Content: "stop now", Interrupt: true,
		}))
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-merge", Content: "also this",
		}))

		merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, hasInterrupt)
		require.Len(t, merge, 1)
		assert.Equal(t, "msg-merge", merge[0].MessageID)

		// The interrupt row must survive; the non-interrupt one is gone.
		merge2, hasInterrupt2, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, hasInterrupt2, "interrupt row must persist after a non-interrupt drain")
		assert.Empty(t, merge2)
	})
}

// TestConsumeInterruptInjectAndEnqueue_MatchesPeekedRow is a P0-2 regression
// test: ConsumeInterruptInjectAndEnqueue must match the exact injectID the
// caller peeked (and built callData from), not silently re-select "the
// oldest pending row". Before this fix, if the peeked row was deleted by a
// concurrent path between Peek and Consume, the re-select-oldest query would
// grab a DIFFERENT row — deleting and enqueuing it under callData that was
// actually built from the (now-gone) peeked row's content, permanently
// losing the second row's real content.
func TestConsumeInterruptInjectAndEnqueue_MatchesPeekedRow(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "atomic consume sess")
	require.NoError(t, err)

	t.Run("vanished peeked row is a safe no-op, does not touch a different row", func(t *testing.T) {
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "int-old", Interrupt: true, CreatedAt: 1000,
		}))
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "int-new", Interrupt: true, CreatedAt: 2000,
		}))

		peeked, err := svc.PeekInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, peeked)
		require.Equal(t, "int-old", peeked.MessageID, "peek must return the oldest row")

		// Simulate a concurrent path consuming/deleting the peeked row between
		// Peek and Consume (e.g. another tick, a foreign process).
		require.NoError(t, svc.DeleteInterruptInject(ctx, peeked.ID))

		// Consume, matching the now-vanished peeked ID. Before the fix, this
		// would re-select "oldest" and silently consume "int-new" instead.
		result, err := svc.ConsumeInterruptInjectAndEnqueue(ctx, sess.ID, peeked.ID, "idem-vanished", []byte(`{"ok":true}`))
		require.NoError(t, err)
		assert.Nil(t, result, "vanished injectID must be a no-op, not a fallback to a different row")

		// The unrelated "int-new" row must be untouched.
		stillPending, err := svc.PeekInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, stillPending)
		assert.Equal(t, "int-new", stillPending.MessageID, "the other row must survive untouched")

		// And no run queue entry should have been created for the vanished row.
		entries, err := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, err)
		assert.Empty(t, entries, "no enqueue should happen for a vanished injectID")
	})

	t.Run("matching injectID atomically deletes and enqueues", func(t *testing.T) {
		sess2, err := svc.Create(ctx, "atomic consume sess 2")
		require.NoError(t, err)

		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess2.ID, MessageID: "int-real", Interrupt: true, CreatedAt: 3000,
		}))
		peeked, err := svc.PeekInterruptInject(ctx, sess2.ID)
		require.NoError(t, err)
		require.NotNil(t, peeked)

		result, err := svc.ConsumeInterruptInjectAndEnqueue(ctx, sess2.ID, peeked.ID, "idem-real", []byte(`{"ok":true}`))
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, peeked.ID, result.ID)

		gone, err := svc.PeekInterruptInject(ctx, sess2.ID)
		require.NoError(t, err)
		assert.Nil(t, gone, "row must be deleted after successful consume")

		entries, err := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 1, "only sess2's entry, sess's own vanished-injectID case must not have enqueued")
		assert.Equal(t, "idem-real", entries[0].ID)
	})
}
