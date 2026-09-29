package message

// DeleteTx coverage: Rerun truncation's transaction-aware batch delete.
// The rows go away on the caller's transaction only; nothing is published
// until the caller commits and calls the returned func.

import (
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func drainDeleted(t *testing.T, sub <-chan pubsub.Event[Message], n int) []Message {
	t.Helper()
	var out []Message
	for len(out) < n {
		select {
		case ev := <-sub:
			require.Equal(t, pubsub.DeletedEvent, ev.Type)
			out = append(out, ev.Payload)
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d DeletedEvents, got %d", n, len(out))
		}
	}
	return out
}

// TestDeleteTx_RollbackPublishesNothing: a rolled-back transaction leaves
// the rows and publishes nothing -- not even from DeleteTx itself, which
// hands the events to the caller to publish after commit.
//
// Revert-check: publish inside DeleteTx (before the caller decides) -- the
// subscriber sees DeletedEvents for rows that were rolled back.
func TestDeleteTx_RollbackPublishesNothing(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	a := mustCreateAssistant(t, svc, "s1", "a")
	b := mustCreateAssistant(t, svc, "s1", "b")
	sub := svc.Subscribe(ctx)

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	deleted, publish, err := svc.DeleteTx(ctx, tx, "s1", []string{a.ID, b.ID})
	require.NoError(t, err)
	require.Len(t, deleted, 2)
	require.NotNil(t, publish)
	require.NoError(t, tx.Rollback())

	for _, id := range []string{a.ID, b.ID} {
		_, err := svc.Get(ctx, id)
		require.NoError(t, err, "a rolled-back DeleteTx must leave the row")
	}
	select {
	case ev := <-sub:
		t.Fatalf("a rolled-back DeleteTx published %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	_, gen, err := svc.ListWithWatermark(ctx, "s1")
	require.NoError(t, err)
	require.EqualValues(t, 0, gen, "the delete generation must not move without a publish")
}

// TestDeleteTx_CommitThenPublish: after commit the returned func publishes
// one DeletedEvent per deleted row and bumps the generation once per row.
func TestDeleteTx_CommitThenPublish(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	a := mustCreateAssistant(t, svc, "s1", "a")
	b := mustCreateAssistant(t, svc, "s1", "b")
	sub := svc.Subscribe(ctx)

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, publish, err := svc.DeleteTx(ctx, tx, "s1", []string{a.ID, b.ID})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	publish()

	evs := drainDeleted(t, sub, 2)
	gens := []int64{evs[0].DeleteGeneration, evs[1].DeleteGeneration}
	require.ElementsMatch(t, []int64{1, 2}, gens)
	_, err = svc.Get(ctx, a.ID)
	require.Error(t, err)
}

// TestDeleteTx_UnconditionalAndSessionScoped: a still-streaming assistant row
// is deleted (the caller's proofs replace the streaming guard); an id owned
// by another session, and an unknown id, are not deleted and not returned.
func TestDeleteTx_UnconditionalAndSessionScoped(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	streaming := mustCreateAssistant(t, svc, "s1", "still streaming") // no Finish part
	foreign := mustCreateAssistant(t, svc, "s2", "other session")

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	deleted, _, err := svc.DeleteTx(ctx, tx, "s1", []string{streaming.ID, foreign.ID, "no-such-id"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.Len(t, deleted, 1)
	require.Equal(t, streaming.ID, deleted[0].ID)
	_, err = svc.Get(ctx, foreign.ID)
	require.NoError(t, err, "another session's row must not be touched")
}

// TestDeleteTx_ChunksLargeIDList: the IN-list is chunked, all rows deleted.
func TestDeleteTx_ChunksLargeIDList(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	var ids []string
	for i := 0; i < deleteTxChunk*2+7; i++ {
		ids = append(ids, mustCreateAssistant(t, svc, "s1", fmt.Sprintf("m%d", i)).ID)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	deleted, _, err := svc.DeleteTx(ctx, tx, "s1", ids)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, deleted, len(ids))
	left, err := svc.List(ctx, "s1")
	require.NoError(t, err)
	require.Empty(t, left)
}

// TestDeleteTx_EmptyIDs is a no-op with a callable publish func.
func TestDeleteTx_EmptyIDs(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	tx, err := sqlDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	deleted, publish, err := svc.DeleteTx(t.Context(), tx, "s1", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Empty(t, deleted)
	publish()
}
