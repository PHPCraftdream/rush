package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// partialCheckpoint builds the shape the auto-checkpoint ticker writes: a
// snapshot whose Finish is Partial, so finished_at stays NULL and the row is
// still "in progress".
func partialCheckpoint(base Message, text string, generation int64) Message {
	snap := base.Clone()
	snap.Parts = []ContentPart{TextContent{Text: text}}
	snap.CheckpointGeneration = generation
	snap.AddFinish(FinishReasonUnknown, "", "")
	for i := len(snap.Parts) - 1; i >= 0; i-- {
		if f, ok := snap.Parts[i].(Finish); ok {
			f.Partial = true
			snap.Parts[i] = f
			break
		}
	}
	return snap
}

func textOf(t *testing.T, m Message) string {
	t.Helper()
	for _, p := range m.Parts {
		if tc, ok := p.(TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no text part in message %s", m.ID)
	return ""
}

// TestUpdate_StaleCheckpointGenerationCannotOverwriteNewer covers P1-3 of the
// 2026-08-18 release-readiness review (task #555).
//
// runTurn tears down and replaces its checkpoint writer per step, and
// stopCheckpoint waits only a bounded grace for the old one before letting
// the next proceed. The old writer is cancelled, but that grace exists
// precisely for the case where the DB or filesystem does NOT honour the
// context promptly -- so a stale write can still land after a newer one.
//
// finished_at alone could not catch this: it distinguishes terminal from
// partial, not one partial generation from another. The terminal write at
// the end of the turn repairs the row, but a crash in between leaves
// recovery reading an outdated checkpoint, which can replay tool actions
// that already ran.
//
// Revert-check: drop `AND checkpoint_generation <= ?` from
// UpdateMessageIfNotTerminal and this fails -- the stale write wins.
func TestUpdate_StaleCheckpointGenerationCannotOverwriteNewer(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-checkpoint-fence", CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	// Generation 2 -- the CURRENT writer -- lands first.
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "from generation 2", 2)))

	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "from generation 2", textOf(t, stored))

	// Generation 1 -- the previous writer, unblocked late by a slow DB or
	// filesystem -- tries to write afterwards. It must lose.
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "from generation 1", 1)))

	stored, err = svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "from generation 2", textOf(t, stored),
		"a stale checkpoint writer overwrote a newer generation's parts -- a crash before the terminal write would leave recovery with outdated state and replay tool actions that already ran")
}

// TestUpdate_SameCheckpointGenerationMayRewriteItsOwnRow pins the reason the
// comparison is <= and not <. One generation checkpoints repeatedly for the
// whole step it owns; a strict < would let a writer write exactly once and
// then silently start losing every subsequent tick, which would look like a
// frozen UI rather than an error.
func TestUpdate_SameCheckpointGenerationMayRewriteItsOwnRow(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-checkpoint-same-gen", CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "tick one", 3)))
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "tick two", 3)))
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "tick three", 3)))

	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "tick three", textOf(t, stored),
		"a writer must be able to keep updating its own row for the whole step it owns")
}

// TestUpdate_TerminalWriteStillWinsOverAnyGeneration is the control for the
// guard that already existed. A terminal write takes the unconditional path
// and must not be fenced by a generation, and once it has landed no partial
// of any generation may undo it -- finished_at, not the generation, is what
// protects that.
func TestUpdate_TerminalWriteStillWinsOverAnyGeneration(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-checkpoint-terminal", CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "partial", 5)))

	// A real terminal finish: not Partial, so finished_at is set.
	final := created.Clone()
	final.Parts = []ContentPart{TextContent{Text: "final answer"}}
	final.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, final))

	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "final answer", textOf(t, stored))

	// A later partial, with a generation HIGHER than anything seen, still
	// must not resurrect an in-progress state over a finished message.
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "late partial", 99)))

	stored, err = svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "final answer", textOf(t, stored),
		"a partial checkpoint overwrote a terminal finish -- the generation fence must not have weakened the finished_at guard")
}

// TestUpdate_GetMutateUpdate_EditLandsOnGenuineCheckpointRow is the
// regression test for task #569 / release-blocker F1.
//
// Every test above builds its partial snapshot in memory via
// partialCheckpoint() and never round-trips it through Get -- which is
// exactly why they stayed green while production broke. Message.
// CheckpointGeneration is write-only in fromDBItem before this fix: Get
// never hydrated it, so a message a handler loaded off a genuinely
// mid-stream row always came back with CheckpointGeneration == 0, no matter
// what the row actually carried. handleUpdateMessageContent (and the other
// three WS editing handlers in internal/server/handlers_messages.go) do
// exactly Get -> mutate Parts -> Update, carrying CheckpointGeneration
// through unexamined -- so every operator edit to a message the checkpoint
// ticker still owned went out stamped generation 0, UpdateMessageIfNotTerminal's
// "AND checkpoint_generation <= ?" fenced it out (0 rows affected), and the
// old code path returned nil: the edit silently vanished while the client
// was told "ok".
//
// This test starts from a row written the way the checkpoint ticker
// actually writes it (Update with a Partial Finish and a real generation),
// re-reads it through the service exactly like a handler would, mutates the
// text exactly like handleUpdateMessageContent does, and asserts the
// mutation is actually stored afterward -- not just that Update returned
// nil.
//
// Revert-check: remove "CheckpointGeneration: item.CheckpointGeneration,"
// from fromDBItem in message.go and this fails -- the mutated text never
// reaches the row.
func TestUpdate_GetMutateUpdate_EditLandsOnGenuineCheckpointRow(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-checkpoint-edit", CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	// Step 1: the checkpoint ticker writes a genuine mid-stream row, exactly
	// the way agent_turn.go's checkpoint goroutine does -- Partial Finish,
	// checkpoint_generation stamped to the writer's generation (3 here,
	// chosen to be > 0 so a still-zeroed CheckpointGeneration on read would
	// provably fence out any later write).
	require.NoError(t, svc.Update(ctx, partialCheckpoint(created, "streamed so far", 3)))

	// Step 2: Get, exactly like handleUpdateMessageContent does. If Get does
	// not hydrate CheckpointGeneration, loaded.CheckpointGeneration is 0
	// here even though the row's checkpoint_generation column is 3.
	loaded, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, loaded.IsPartial(), "row must still be mid-stream for this test to exercise the fenced branch")
	require.Equal(t, int64(3), loaded.CheckpointGeneration,
		"Get did not hydrate CheckpointGeneration from the row -- every message-editing handler carries this field through Get -> Update unexamined, so a zeroed value here is what silently fences out real edits")

	// Step 3: mutate the text in place, exactly like
	// handleUpdateMessageContent's replace-the-text-part loop.
	for i, part := range loaded.Parts {
		if _, ok := part.(TextContent); ok {
			loaded.Parts[i] = TextContent{Text: "OPERATOR EDIT"}
			break
		}
	}

	// Step 4: persist the edit through the exact same Service.Update call
	// the handler makes.
	require.NoError(t, svc.Update(ctx, loaded))

	// Step 5: the edit must have actually landed.
	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "OPERATOR EDIT", textOf(t, stored),
		"operator edit to a mid-stream message was discarded -- Update matched zero rows because the edit was stamped with a stale (zeroed) checkpoint_generation instead of the row's real one")
}
