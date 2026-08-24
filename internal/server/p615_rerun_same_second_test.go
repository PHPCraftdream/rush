package server

// Task #615: rerun deletes messages created BEFORE the target when they
// share the same created_at second.
//
// internal/db/sql/messages.sql stores created_at in whole SECONDS
// (strftime('%s', 'now')). handleRerunMessage's old tail-cleanup loop
// (internal/server/handlers_agent.go) walked a.Messages.List's result and
// deleted every row where:
//
//	m.CreatedAt > targetMsg.CreatedAt ||
//	    (m.CreatedAt == targetMsg.CreatedAt && m.ID != targetMsg.ID)
//
// The second disjunct fires for ANY other row sharing the target's second,
// including one inserted BEFORE the target — messages.id is a
// non-monotonic UUID (see messages.sql's own comment on
// ListMessagesBySession), so "m.ID != targetMsg.ID" cannot distinguish
// earlier from later. A message that predates the rerun target within the
// same wall-clock second was silently deleted.
//
// Messages.List already returns messages in a deterministic
// (created_at ASC, rowid ASC) total order (ListMessagesBySession in
// messages.sql). The fix finds the target's index in that ordered list and
// deletes only the slice strictly after it, instead of re-deriving order
// from timestamps that cannot break same-second ties.
import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// forceSameCreatedAt overwrites created_at for every given message ID to an
// identical value, deterministically reproducing the same-second collision
// that strftime('%s','now') would only produce by timing luck between fast
// sequential inserts in a real run.
func forceSameCreatedAt(t *testing.T, db *sql.DB, ids []string, ts int64) {
	t.Helper()
	for _, id := range ids {
		_, err := db.ExecContext(t.Context(), `UPDATE messages SET created_at = ? WHERE id = ?`, ts, id)
		require.NoError(t, err, "forcing created_at for %s", id)
	}
}

// TestHandleRerunMessage_SameSecondBeforeMessageIsPreserved is the
// regression test for #615. It creates three messages — before, target,
// after — in that insertion order, then forces all three to share one
// created_at second (the exact collision the bug depended on). It reruns
// the target and asserts:
//   - before survives untouched,
//   - target is deleted (Run() would recreate it; the coordinator here is
//     a stub so we only assert the delete half),
//   - after is deleted.
//
// Revert-check: restore the old timestamp-only condition
// (m.CreatedAt > targetMsg.CreatedAt || (m.CreatedAt == targetMsg.CreatedAt
// && m.ID != targetMsg.ID)) in handlers_agent.go's tail-cleanup loop and
// this test fails, because "before" — sharing the target's forced second
// and having a different (UUID, so unordered) ID — matches the second
// disjunct and gets deleted too.
func TestHandleRerunMessage_SameSecondBeforeMessageIsPreserved(t *testing.T) {
	// Cannot use t.Parallel(): newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p615-same-second-test")
	require.NoError(t, err)

	before, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "before target"}},
	})
	require.NoError(t, err)

	target, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "the rerun target"}},
	})
	require.NoError(t, err)

	after, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "after target"}},
	})
	require.NoError(t, err)
	after.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, after))

	// Force all three rows onto the exact same created_at second. Without
	// this, ordering still depends on the DB's row order
	// (ListMessagesBySession's rowid tiebreaker), which the fix relies on
	// too -- but forcing the timestamp collision is what makes this test
	// fail deterministically against the OLD (timestamp-only) condition
	// rather than only sometimes.
	const collidedSecond int64 = 1_700_000_000
	forceSameCreatedAt(t, a.DB(), []string{before.ID, target.ID, after.ID}, collidedSecond)

	mockCoord := &mockAgentCoordinator{busy: false}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)

	payload, err := json.Marshal(RerunMessagePayload{MessageID: target.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	// "before" must survive: this is the #615 regression.
	gotBefore, getErr := a.Messages.Get(ctx, before.ID)
	require.NoError(t, getErr, "message created BEFORE the rerun target must NOT be deleted, even when it shares the target's created_at second")
	require.Equal(t, "before target", gotBefore.FullText())

	// "target" is deleted (step 3 of handleRerunMessage; Run() would
	// recreate it, but the mock coordinator's Run just errors out).
	_, getErr = a.Messages.Get(ctx, target.ID)
	require.Error(t, getErr, "target message should have been deleted for rerun")

	// "after" is deleted: it is genuinely tail content following the
	// rerun point.
	_, getErr = a.Messages.Get(ctx, after.ID)
	require.Error(t, getErr, "message created AFTER the rerun target should have been deleted")
}

// TestHandleRerunMessage_TargetMissingFromListFailsClosed covers the other
// half of the #615 fix: handleRerunMessage first fetches the target row
// with a.Messages.Get, then looks for that same row's index inside
// a.Messages.List's result for targetMsg.SessionID. If the row cannot be
// located there -- e.g. Get and List observe it inconsistently, or (as
// exercised here) Get itself fails because the ID never existed -- the
// handler must fail closed: reply EventError and delete nothing, rather
// than guessing a fallback order.
//
// Scope of this test, stated precisely: it drives the OUTER edge of that
// contract -- a.Messages.Get fails immediately for an unknown ID, which is
// the pre-existing guard sitting just above the new code, not the new
// targetIdx == -1 line itself. That line is unreachable from outside the
// handler: List is called with targetMsg.SessionID, so a row Get just
// returned is in that list unless another writer deletes it in between.
// It is a defensive branch against exactly that concurrent deletion, and
// nothing here executes it. What IS covered is the found branch (by
// TestHandleRerunMessage_SameSecondBeforeMessageIsPreserved, which proves
// targetIdx resolves to the right position) and the fail-closed contract
// at the handler's boundary (here).
func TestHandleRerunMessage_TargetMissingFromListFailsClosed(t *testing.T) {
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p615-missing-target-test")
	require.NoError(t, err)

	decoy, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "unrelated decoy"}},
	})
	require.NoError(t, err)

	mockCoord := &mockAgentCoordinator{busy: false}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)

	payload, err := json.Marshal(RerunMessagePayload{MessageID: "does-not-exist"})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type, "rerun of a nonexistent target must fail, not proceed")

	// Nothing in the session was touched.
	stillThere, getErr := a.Messages.Get(ctx, decoy.ID)
	require.NoError(t, getErr, "unrelated message must survive a failed rerun lookup")
	require.Equal(t, "unrelated decoy", stillThere.FullText())
}
