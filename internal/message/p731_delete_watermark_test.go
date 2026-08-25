package message

// Task #731: delete-resurrection risk in the WebSocket message-sync logic.
//
// web/src/store.ts's mergeMessageLists protects live-applied deletes via
// per-session tombstones, cleared whenever an "epoch-clean" snapshot is
// applied (see ws.ts's liveEventEpoch/requestEpoch). The epoch counter only
// proves "no live push landed since this request was sent" -- a statement
// about wall-clock event count -- not that THIS reply's underlying DB read
// actually postdates a delete the client already knows about. The server
// dispatches load_messages across a worker pool with no FIFO guarantee and
// serves reads off a separate read-only connection pool (see
// internal/db.ConnectRead), so a reply's DB read can legitimately be older
// than a delete's commit even when nothing about message arrival order was
// violated at the WS-frame level.
//
// The fix: message.Message gains a RowID field -- SQLite's implicit
// monotonic insertion counter, the same tiebreaker already used throughout
// internal/db/sql/messages.sql. Delete/ForceDelete populate it (fetched via
// GetMessageRowID just before the row is removed) so the DeletedEvent
// payload carries a per-message watermark; ListWithWatermark exposes the
// session's current max rowid (GetMaxMessageRowIDBySession) as the
// snapshot-level watermark. A client comparing the two can recognize a
// snapshot reply as PROVABLY older than an already-applied delete,
// independent of the epoch heuristic.
//
// This file tests the server-side half of that mechanism: that the
// watermarks are populated correctly and have the ordering properties the
// client-side comparison (web/src/ws.ts's isSnapshotStaleForDeletes) relies
// on. The client-side comparison logic itself, and the OLD epoch-only
// heuristic's failure to catch a stale-but-epoch-clean reply, are covered by
// web/tests/messages-list-delete-watermark.spec.ts.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDelete_PopulatesRowIDOnDeletedEventPayload proves Delete fetches and
// attaches the row's watermark to the message it publishes, BEFORE the row
// is gone (rowid does not survive a DELETE). This is the value that ends up
// on the wire as message_deleted's RowID field
// (internal/server/wire.go/toMessageWire).
//
// Revert-check: remove the GetMessageRowID call in Service.Delete and this
// fails -- the published event's RowID stays 0.
func TestDelete_PopulatesRowIDOnDeletedEventPayload(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	sub := svc.Subscribe(ctx)
	created := mustCreateAssistant(t, svc, "test-session-watermark-delete", "start")
	created.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, created))

	// Drain CreatedEvent + terminal UpdatedEvent so neither is mistaken for
	// the DeletedEvent under test below.
	for i := 0; i < 2; i++ {
		select {
		case <-sub:
		case <-time.After(time.Second):
			t.Fatalf("did not receive setup event %d/2", i+1)
		}
	}

	require.NoError(t, svc.Delete(ctx, created.ID))

	select {
	case ev := <-sub:
		require.Equal(t, created.ID, ev.Payload.ID)
		require.Greater(t, ev.Payload.RowID, int64(0),
			"Delete must populate RowID on the DeletedEvent payload with the row's watermark before removing it")
	case <-time.After(time.Second):
		t.Fatal("did not receive DeletedEvent")
	}
}

// TestForceDelete_PopulatesRowIDOnDeletedEventPayload is the ForceDelete
// counterpart -- the orphan-rescue path (deleteMessageRescuingOrphan in
// internal/server/handlers_messages.go) goes through this method instead of
// Delete, and must carry the same watermark guarantee.
func TestForceDelete_PopulatesRowIDOnDeletedEventPayload(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	sub := svc.Subscribe(ctx)
	created := mustCreateAssistant(t, svc, "test-session-watermark-force-delete", "orphaned")

	select {
	case <-sub: // drain CreatedEvent
	case <-time.After(time.Second):
		t.Fatal("did not receive CreatedEvent")
	}

	require.NoError(t, svc.ForceDelete(ctx, created.ID))

	select {
	case ev := <-sub:
		require.Equal(t, created.ID, ev.Payload.ID)
		require.Greater(t, ev.Payload.RowID, int64(0),
			"ForceDelete must populate RowID on the DeletedEvent payload")
	case <-time.After(time.Second):
		t.Fatal("did not receive DeletedEvent")
	}
}

// TestListWithWatermark_ReflectsInsertOrder proves the session-level
// watermark (what a messages_list reply carries) increases monotonically as
// messages are created, matching each message's own per-row watermark
// (what a message_deleted push carries) obtained via GetMessageRowID. This
// is the ordering property the client-side comparison depends on: a
// snapshot's watermark must be directly comparable against a delete's
// watermark as "the same kind of number, from the same monotonic sequence".
func TestListWithWatermark_ReflectsInsertOrder(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-watermark-order"

	m1 := mustCreateAssistant(t, svc, sessionID, "first")
	_, wm1, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)

	row1, err := q.GetMessageRowID(ctx, m1.ID)
	require.NoError(t, err)
	require.Equal(t, row1, wm1, "watermark right after creating the only message must equal that message's own rowid")

	m2 := mustCreateAssistant(t, svc, sessionID, "second")
	_, wm2, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)

	row2, err := q.GetMessageRowID(ctx, m2.ID)
	require.NoError(t, err)
	require.Equal(t, row2, wm2, "watermark after a second create must equal the newest message's rowid")
	require.Greater(t, wm2, wm1, "watermark must strictly increase as messages are created")
}

// TestListWithWatermark_DoesNotRegressAfterDeletingOlderMessage is the crux
// regression test for the resurrection risk itself: deleting an OLDER
// message must not lower the session's watermark below a NEWER message's
// own rowid. If it did, a client that recorded the newer message's delete
// watermark could wrongly treat a stale snapshot (reflecting neither
// delete) as "fresh enough" purely because the session-level watermark
// dropped -- reintroducing exactly the resurrection window this mechanism
// exists to close.
//
// Concretely, this reproduces the scenario the task describes: message O
// (older) is deleted first; a later messages_list read must still report a
// watermark >= the newest surviving message's rowid, proving
// GetMaxMessageRowIDBySession is a true "highest ever assigned, among what
// remains" watermark, not something that could be misread as having
// regressed to before O's delete.
func TestListWithWatermark_DoesNotRegressAfterDeletingOlderMessage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-watermark-no-regress"

	mOld := mustCreateAssistant(t, svc, sessionID, "old, will be deleted")
	mOld.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, mOld))

	mNew := mustCreateAssistant(t, svc, sessionID, "new, survives")
	rowNew, err := q.GetMessageRowID(ctx, mNew.ID)
	require.NoError(t, err)

	// Delete the OLDER message.
	require.NoError(t, svc.Delete(ctx, mOld.ID))

	_, watermarkAfterDelete, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, watermarkAfterDelete, rowNew,
		"session watermark after deleting an older message must not regress below a surviving newer message's own rowid")
}

// TestListWithWatermark_EmptySessionReportsZero pins the documented
// COALESCE(MAX(rowid), 0) behavior for a session with no messages (or every
// message deleted): the watermark must be a comparable integer, never NULL,
// and 0 must compare as "older than every real delete" (SQLite rowids start
// at 1) so a client's isSnapshotStaleForDeletes-style comparison degrades
// safely instead of needing a NULL/undefined special case.
func TestListWithWatermark_EmptySessionReportsZero(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	msgs, watermark, err := svc.ListWithWatermark(ctx, "test-session-watermark-empty")
	require.NoError(t, err)
	require.Empty(t, msgs)
	require.Equal(t, int64(0), watermark)
}
