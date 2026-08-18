// Session service create/update tests: caller-chosen-id creation via
// CreateWithID, UpdateReasoningEffort overwrite semantics, and the default
// reasoning effort a fresh Create inherits from the schema defaults.
package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CreateWithID is the primitive behind `crush run --session <id>` idempotent
// CI invocations and behind `app.resolveSession`'s get-or-create branch.
// It must (a) honour the caller-chosen id verbatim, (b) round-trip the title,
// and (c) refuse a second insert with the same id (so the get-or-create flow
// can distinguish "race lost" from a real failure).
func TestCreateWithID(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	t.Run("creates with caller-supplied id", func(t *testing.T) {
		s, err := svc.CreateWithID(ctx, "pr-42", "Review PR 42")
		require.NoError(t, err)
		assert.Equal(t, "pr-42", s.ID)
		assert.Equal(t, "Review PR 42", s.Title)

		got, err := svc.Get(ctx, "pr-42")
		require.NoError(t, err)
		assert.Equal(t, "pr-42", got.ID)
		assert.Equal(t, "Review PR 42", got.Title)
	})

	t.Run("rejects duplicate id", func(t *testing.T) {
		_, err := svc.CreateWithID(ctx, "dup", "first")
		require.NoError(t, err)
		_, err = svc.CreateWithID(ctx, "dup", "second")
		require.Error(t, err, "second insert with the same id must fail (UNIQUE constraint)")
	})

	t.Run("does not collide with uuid-allocated Create", func(t *testing.T) {
		// Create() picks a random UUID; CreateWithID() picks a literal — they
		// must coexist in the same table without trouble.
		uuidSess, err := svc.Create(ctx, "uuid sess")
		require.NoError(t, err)
		idSess, err := svc.CreateWithID(ctx, "named-sess", "named")
		require.NoError(t, err)
		assert.NotEqual(t, uuidSess.ID, idSess.ID)
	})
}

func TestUpdateReasoningEffort(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)

	ctx := t.Context()

	// Create a test session
	session, err := svc.Create(ctx, "Test Session")
	require.NoError(t, err)
	require.NotNil(t, session)

	t.Run("sets reasoning effort for both models", func(t *testing.T) {
		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "low")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		assert.Equal(t, "high", updated.LargeModelReasoningEffort)
		assert.Equal(t, "low", updated.SmallModelReasoningEffort)
	})

	t.Run("updates only large model effort", func(t *testing.T) {
		err := svc.UpdateReasoningEffort(ctx, session.ID, "max", "")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		assert.Equal(t, "max", updated.LargeModelReasoningEffort)
		// Empty string overwrites, so small model becomes empty (not preserved)
		assert.Equal(t, "", updated.SmallModelReasoningEffort)
	})

	t.Run("updates only small model effort", func(t *testing.T) {
		// First set both to known values
		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
		require.NoError(t, err)

		// Then update only small model
		err = svc.UpdateReasoningEffort(ctx, session.ID, "", "medium")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		// Empty string overwrites large model
		assert.Equal(t, "", updated.LargeModelReasoningEffort)
		assert.Equal(t, "medium", updated.SmallModelReasoningEffort)
	})

	t.Run("supports all valid effort levels", func(t *testing.T) {
		validLevels := []string{"low", "medium", "high", "max"}
		for _, level := range validLevels {
			err := svc.UpdateReasoningEffort(ctx, session.ID, level, level)
			require.NoError(t, err, "level=%s", level)

			updated, err := svc.Get(ctx, session.ID)
			require.NoError(t, err)
			assert.Equal(t, level, updated.LargeModelReasoningEffort)
			assert.Equal(t, level, updated.SmallModelReasoningEffort)
		}
	})

	t.Run("publishes update event", func(t *testing.T) {
		events := svc.Subscribe(ctx)

		// Start goroutine to consume event
		eventCh := make(chan struct{})
		go func() {
			for range events {
				close(eventCh)
				return
			}
		}()

		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
		require.NoError(t, err)

		select {
		case <-eventCh:
		case <-ctx.Done():
			t.Fatal("timed out waiting for event")
		}
	})
}

func TestCreateSession_DefaultReasoningEffort(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)

	ctx := t.Context()

	session, err := svc.Create(ctx, "Test Session")
	require.NoError(t, err)

	// The DB has DEFAULT 'medium', so when we read back, we get "medium"
	assert.Equal(t, "medium", session.LargeModelReasoningEffort)
	assert.Equal(t, "medium", session.SmallModelReasoningEffort)

	// When we explicitly set a different value, it should override the default
	err = svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
	require.NoError(t, err)

	retrieved, err := svc.Get(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, "high", retrieved.LargeModelReasoningEffort)
	assert.Equal(t, "high", retrieved.SmallModelReasoningEffort)
}
