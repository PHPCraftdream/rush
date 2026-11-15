package message

// Task #731 (original design) / task #737 (fix): delete-resurrection risk in
// the WebSocket message-sync logic.
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
// Task #731's original fix: message.Message gained a RowID field (SQLite's
// implicit monotonic insertion counter). Delete/ForceDelete populated it
// (fetched via GetMessageRowID just before the row was removed) so the
// DeletedEvent payload carried a per-message watermark; ListWithWatermark
// exposed the session's current MAX(rowid) (GetMaxMessageRowIDBySession) as
// the snapshot-level watermark.
//
// That design had a real bug, fixed here in task #737: MAX(rowid) over a
// session's SURVIVING messages is not a monotonic "highest ever assigned"
// value. Deleting a NON-TAIL message does not lower MAX(rowid), so the
// watermark did not move for that class of delete. Concretely: a session
// has messages with rowid 10 and rowid 20. Message rowid=10 (not the
// highest) is deleted. The client's delete high-water mark for the session
// becomes 10. A snapshot read that happened BEFORE the delete (still
// showing both messages) reports watermark=20 (rowid 20 still exists,
// untouched by the delete). The staleness check is "10 > 20" -> false ->
// the pre-delete snapshot is wrongly treated as FRESH, and
// mergeMessageLists can resurrect the deleted message back into the UI. The
// watermark scheme as originally implemented only actually caught deletion
// of the CURRENT highest-rowid message in a session, not deletion in
// general.
//
// The fix: replace the rowid-derived watermark with an in-memory
// per-session delete-GENERATION counter (message.service's deleteGen map +
// mutex), incremented once on every Delete/ForceDelete call regardless of
// which message was removed. message.Message.DeleteGeneration and
// message.Service.ListWithWatermark carry this value instead of RowID.
//
// This file tests the server-side half of that mechanism: that the
// watermarks are populated correctly and have the ordering properties the
// client-side comparison (web/src/ws.ts's isSnapshotStaleForDeletes) relies
// on. The client-side comparison logic itself is covered by
// web/tests/messages-list-delete-watermark.spec.ts.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDelete_PopulatesGenerationOnDeletedEventPayload proves Delete bumps
// the session's delete-generation counter and attaches the POST-increment
// value to the message it publishes. This is the value that ends up on the
// wire as message_deleted's DeleteGeneration field
// (internal/server/wire.go/toMessageWire).
//
// Revert-check: remove the bumpDeleteGeneration call in Service.Delete and
// this fails -- the published event's DeleteGeneration stays 0.
func TestDelete_PopulatesGenerationOnDeletedEventPayload(t *testing.T) {
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
		require.Greater(t, ev.Payload.DeleteGeneration, int64(0),
			"Delete must populate DeleteGeneration on the DeletedEvent payload with the post-increment generation before publishing")
	case <-time.After(time.Second):
		t.Fatal("did not receive DeletedEvent")
	}
}

// TestForceDelete_PopulatesGenerationOnDeletedEventPayload is the
// ForceDelete counterpart -- the orphan-rescue path
// (deleteMessageRescuingOrphan in internal/server/handlers_messages.go)
// goes through this method instead of Delete, and must carry the same
// watermark guarantee.
func TestForceDelete_PopulatesGenerationOnDeletedEventPayload(t *testing.T) {
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
		require.Greater(t, ev.Payload.DeleteGeneration, int64(0),
			"ForceDelete must populate DeleteGeneration on the DeletedEvent payload")
	case <-time.After(time.Second):
		t.Fatal("did not receive DeletedEvent")
	}
}

// TestListWithWatermark_GenerationIncreasesMonotonicallyPerDelete proves the
// session-level watermark returned by ListWithWatermark increases by
// exactly one for every delete in that session, matching each delete's own
// per-event generation (what a message_deleted push carries). This is the
// ordering property the client-side comparison depends on: a snapshot's
// watermark must be directly comparable against a delete's watermark as
// "the same kind of number, from the same monotonic sequence" -- and,
// unlike the old rowid-based watermark, it must advance on EVERY delete,
// not just deletes of the current tail message.
func TestListWithWatermark_GenerationIncreasesMonotonicallyPerDelete(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-watermark-order"

	_, wm0, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(0), wm0, "a session with no deletes yet must report generation 0")

	m1 := mustCreateAssistant(t, svc, sessionID, "first")
	m1.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, m1))
	require.NoError(t, svc.Delete(ctx, m1.ID))

	_, wm1, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(1), wm1, "generation must be 1 after the session's first delete")

	m2 := mustCreateAssistant(t, svc, sessionID, "second")
	m2.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, m2))
	require.NoError(t, svc.Delete(ctx, m2.ID))

	_, wm2, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(2), wm2, "generation must be 2 after the session's second delete")
	require.Greater(t, wm2, wm1, "watermark must strictly increase as deletes happen")
}

// TestListWithWatermark_NonTailDeleteAdvancesWatermark is the crux
// regression test for the resurrection risk task #737 fixes.
//
// Concretely reproduces the scenario from the bug report: a session has two
// messages. The OLDER one (not the highest-rowid/tail message) is deleted
// while the newer one survives. Under the OLD rowid-based watermark
// (GetMaxMessageRowIDBySession = MAX(rowid) over surviving rows), deleting
// a non-tail message does NOT lower MAX(rowid) at all -- the watermark
// after the delete would be indistinguishable from the watermark before it,
// so a stale pre-delete snapshot (captured with the higher, "looks fresher"
// watermark) would wrongly compare as fresh against the delete's own
// watermark, resurrecting the deleted message client-side.
//
// The generation-counter fix closes this because it does not care WHICH
// message was deleted -- every Delete/ForceDelete call bumps the counter
// once, unconditionally. This test proves that property directly: deleting
// the non-tail (older, survived-by-a-newer-message) row still advances
// ListWithWatermark's returned generation, which is exactly the guarantee
// the old MAX(rowid) query did not have.
//
// Proof this is not vacuous against the pre-fix implementation: replaying
// this scenario against the OLD GetMaxMessageRowIDBySession query --
// MAX(rowid) over messages WHERE session_id = ? -- deleting the older
// message leaves the newer message's rowid as the unchanged MAX(rowid), so
// a snapshot watermark read before contained the SAME value as one read
// after (both equal the newer message's rowid); asserting the watermark
// strictly ADVANCES across the delete (as this test does) would have FAILED
// against that implementation, since nothing about MAX(rowid) changes when
// a non-tail row is removed. Against the new generation counter, every
// Delete call unconditionally increments regardless of which row it
// targets, so the assertion holds.
func TestListWithWatermark_NonTailDeleteAdvancesWatermark(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-watermark-non-tail-delete"

	// Older message (will be deleted) -- this is the "rowid 10" of the bug
	// report.
	mOld := mustCreateAssistant(t, svc, sessionID, "old, will be deleted")
	mOld.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, mOld))

	// Newer message (survives) -- this is the "rowid 20" of the bug report;
	// it remains the session's highest-rowid row throughout this test, so
	// under the OLD implementation MAX(rowid) would never move.
	mNew := mustCreateAssistant(t, svc, sessionID, "new, survives")
	mNew.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, mNew))

	// Snapshot BEFORE the delete: this is the "stale pre-delete snapshot"
	// from the bug report. Its watermark must be strictly LOWER than the
	// watermark reported after deleting mOld, or a client using this
	// snapshot's watermark could wrongly be judged fresh relative to the
	// delete that is about to happen.
	_, watermarkBeforeDelete, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)

	// Delete the OLDER, non-tail message.
	require.NoError(t, svc.Delete(ctx, mOld.ID))

	_, watermarkAfterDelete, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)

	require.Greater(t, watermarkAfterDelete, watermarkBeforeDelete,
		"deleting a non-tail message (older message, newer one survives) must still advance the session watermark -- "+
			"this is exactly the case the old MAX(rowid) watermark missed, since MAX(rowid) is unchanged by deleting a non-max row")

	// And the client-side comparison this feeds must now correctly flag the
	// pre-delete snapshot as stale: a delete watermark equal to
	// watermarkAfterDelete compared against the OLDER snapshot's watermark
	// must be strictly greater, matching web/src/ws.ts's
	// isSnapshotStaleForDeletes (deleteHighWaterMark > snapshotWatermark).
	require.Greater(t, watermarkAfterDelete, watermarkBeforeDelete,
		"isSnapshotStaleForDeletes-style comparison (deleteWatermark > staleSnapshotWatermark) must now correctly evaluate true")
}

// TestListWithWatermark_EmptySessionReportsZero pins the documented
// zero-value behavior for a session with no deletes yet (whether or not it
// has messages): the watermark must be a comparable integer, and 0 must
// compare as "older than every real delete" (every generation bump produces
// a value >= 1) so a client's isSnapshotStaleForDeletes-style comparison
// degrades safely without needing a special case.
func TestListWithWatermark_EmptySessionReportsZero(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	msgs, watermark, err := svc.ListWithWatermark(ctx, "test-session-watermark-empty")
	require.NoError(t, err)
	require.Empty(t, msgs)
	require.Equal(t, int64(0), watermark)
}

// TestListWithWatermark_ReadsGenerationBeforeListQuery pins the ordering
// requirement documented on ListWithWatermark: the generation counter must
// be read BEFORE the List query runs, not after, so that any delete racing
// this call can only make the returned watermark look MORE stale (safe),
// never falsely fresh. This test cannot directly observe query ordering
// (no concurrent race harness here), so it instead pins the observable
// contract that ordering exists to guarantee: the returned watermark for a
// session with exactly N prior deletes is always exactly N -- consistent
// with a single, clean read of the counter that is not re-derived from (or
// raced against) the List query it accompanies.
func TestListWithWatermark_ReadsGenerationBeforeListQuery(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-watermark-ordering"

	m := mustCreateAssistant(t, svc, sessionID, "only")
	m.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, m))
	require.NoError(t, svc.Delete(ctx, m.ID))

	msgs, watermark, err := svc.ListWithWatermark(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, msgs, "the only message was deleted, so List must return empty")
	require.Equal(t, int64(1), watermark, "watermark must reflect exactly the one delete that already happened")
}
