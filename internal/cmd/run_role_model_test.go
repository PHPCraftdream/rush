package cmd

// Tests for sessionRoleModelOverride (task #1060): `rush run --role worker` /
// `--role reviewer` must prefer a per-session worker/reviewer override over
// the config-wide default, the same precedence resolveSubAgentModelOverride
// already applies for sub-agent dispatch (internal/agent/coordinator_models.go).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// TestSessionRoleModelOverride_EmptySessionID guards the brand-new-run path
// (no --session / not yet resolved): must return "" so the caller falls
// back to the config default, never panics on an empty lookup.
func TestSessionRoleModelOverride_EmptySessionID(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	a := &app.App{Sessions: s}

	got := sessionRoleModelOverride(context.Background(), a, "", config.SelectedModelTypeWorker)
	require.Equal(t, "", got)
}

// TestSessionRoleModelOverride_NoOverrideReturnsEmpty covers a session that
// never called set_session_models for worker/reviewer: falls back to "",
// so the caller uses the config default instead of an accidental empty pin.
func TestSessionRoleModelOverride_NoOverrideReturnsEmpty(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	a := &app.App{Sessions: s}
	ctx := context.Background()

	sess, err := s.Create(ctx, "no override")
	require.NoError(t, err)

	require.Equal(t, "", sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeWorker))
	require.Equal(t, "", sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeReviewer))
}

// TestSessionRoleModelOverride_UsesWorkerAndReviewerOverride is the direct
// regression: a session with explicit worker/reviewer overrides resolves to
// EXACTLY those, not the coordinator-wide config default for that slot.
func TestSessionRoleModelOverride_UsesWorkerAndReviewerOverride(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	a := &app.App{Sessions: s}
	ctx := context.Background()

	sess, err := s.Create(ctx, "with overrides")
	require.NoError(t, err)
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, sess.ID,
		&session.ModelSlotUpdate{Provider: "worker-prov", Model: "worker-model"},
		&session.ModelSlotUpdate{Provider: "reviewer-prov", Model: "reviewer-model"}))

	require.Equal(t, "worker-prov/worker-model",
		sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeWorker))
	require.Equal(t, "reviewer-prov/reviewer-model",
		sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeReviewer))
}

// TestSessionRoleModelOverride_SmartFastNeverOverridden proves this helper
// is scoped to worker/reviewer only: smart/fast already have their own
// resolution cascade inside the coordinator (resolveSessionModels), so
// routing them through here too would double-apply the session override.
func TestSessionRoleModelOverride_SmartFastNeverOverridden(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	a := &app.App{Sessions: s}
	ctx := context.Background()

	sess, err := s.Create(ctx, "smart fast set")
	require.NoError(t, err)
	require.NoError(t, s.UpdateModels(ctx, sess.ID,
		&session.ModelSlotUpdate{Provider: "smart-prov", Model: "smart-model"},
		&session.ModelSlotUpdate{Provider: "fast-prov", Model: "fast-model"}))

	require.Equal(t, "", sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeSmart))
	require.Equal(t, "", sessionRoleModelOverride(ctx, a, sess.ID, config.SelectedModelTypeFast))
}
