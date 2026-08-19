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
