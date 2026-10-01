package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPurgeQueuedWorkForSession verifies that PurgeQueuedWorkForSession
// removes every durable queued-work row of the target session (run queue,
// merge+interrupt injects, orphan outbox) while leaving a neighbouring
// session's rows untouched.
func TestPurgeQueuedWorkForSession(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	purgeSess, err := svc.Create(ctx, "purge-me")
	require.NoError(t, err)
	keepSess, err := svc.Create(ctx, "keep-me")
	require.NoError(t, err)
	purgeID, keepID := purgeSess.ID, keepSess.ID

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "rq-1", purgeID, []byte(`{}`)))
	require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{SessionID: purgeID, MessageID: "m1"}))
	require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{SessionID: purgeID, MessageID: "m2", Interrupt: true}))
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "ob-1", purgeID, []byte(`{}`)))

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "rq-2", keepID, []byte(`{}`)))
	require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{SessionID: keepID, MessageID: "m3"}))
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "ob-2", keepID, []byte(`{}`)))

	purged, err := svc.PurgeQueuedWorkForSession(ctx, purgeID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged.RunQueue)
	assert.Equal(t, int64(2), purged.PendingInject)
	assert.Equal(t, int64(1), purged.OrphanOutbox)

	// The target session must have no durable queued-work row left of any kind.
	entry, err := svc.LeaseRunQueueEntry(ctx, purgeID, "tester", time.Second)
	require.NoError(t, err)
	assert.Nil(t, entry)

	merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, purgeID)
	require.NoError(t, err)
	assert.Empty(t, merge)
	assert.False(t, hasInterrupt)

	peeked, err := svc.PeekInterruptInject(ctx, purgeID)
	require.NoError(t, err)
	assert.Nil(t, peeked)

	outbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	for _, e := range outbox {
		assert.NotEqual(t, purgeID, e.SessionID, "purged session must not stay in the orphan outbox")
	}

	// The neighbouring session must be untouched.
	keepEntry, err := svc.LeaseRunQueueEntry(ctx, keepID, "tester", time.Second)
	require.NoError(t, err)
	require.NotNil(t, keepEntry)
	assert.Equal(t, "{}", keepEntry.CallData)

	outbox, err = svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	var foundKeep bool
	for _, e := range outbox {
		if e.ID == "ob-2" {
			foundKeep = true
			assert.Equal(t, keepID, e.SessionID)
		}
	}
	assert.True(t, foundKeep, "keep-me's orphan outbox row must survive the purge")

	// The drain consumes (deletes) the merge row, so it is not repeated here.
	keepMerge, keepHasInterrupt, err := svc.DrainPendingInjects(ctx, keepID)
	require.NoError(t, err)
	assert.False(t, keepHasInterrupt)
	require.Len(t, keepMerge, 1)
	assert.Equal(t, "m3", keepMerge[0].MessageID)
}

func TestPurgeQueuedWorkForSession_EmptySession(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "empty purge sess")
	require.NoError(t, err)

	purged, err := svc.PurgeQueuedWorkForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, PurgedQueuedWork{}, purged)
}
