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

// lastPinnedService makes GetLast deterministic (updated_at has one-second
// granularity, so "the newest session" is otherwise a tie).
type lastPinnedService struct {
	session.Service
	lastID string
}

func (l lastPinnedService) GetLast(ctx context.Context) (session.Session, error) {
	return l.Get(ctx, l.lastID)
}

// TestFoldRoleModel_ContinueResolvesLastSessionBeforeFold is R2C-14:
// `rush run --continue --role worker` names its session only implicitly, and
// the fold used to read the pin with an empty session id, so it always fell
// back to the config default. It must read the pin of the session --continue
// resolves to; an explicit --session still wins (the run's own precedence).
//
// Revert-check: the GetLast resolution removed from foldRoleModel -- the fold
// fell through to a.Config() (this test App has none) and the test panicked at
// the first assertion.
func TestFoldRoleModel_ContinueResolvesLastSessionBeforeFold(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	older, err := s.Create(ctx, "older")
	require.NoError(t, err)
	last, err := s.Create(ctx, "last")
	require.NoError(t, err)
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, older.ID,
		&session.ModelSlotUpdate{Provider: "old-prov", Model: "old-worker"},
		&session.ModelSlotUpdate{Provider: "old-prov", Model: "old-reviewer"}))
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, last.ID,
		&session.ModelSlotUpdate{Provider: "last-prov", Model: "last-worker"},
		&session.ModelSlotUpdate{Provider: "last-prov", Model: "last-reviewer"}))
	a := &app.App{Sessions: lastPinnedService{Service: s, lastID: last.ID}}

	got, err := foldRoleModel(ctx, a, "worker", config.SelectedModelTypeWorker, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "last-prov/last-worker", got, "--continue must use the pin of the session it continues")

	got, err = foldRoleModel(ctx, a, "reviewer", config.SelectedModelTypeReviewer, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "last-prov/last-reviewer", got)

	got, err = foldRoleModel(ctx, a, "worker", config.SelectedModelTypeWorker, "", older.ID, true)
	require.NoError(t, err)
	require.Equal(t, "old-prov/old-worker", got, "an explicit session id takes precedence over --continue, as in resolveSession")

	got, err = foldRoleModel(ctx, a, "worker", config.SelectedModelTypeWorker, "explicit/model", "", true)
	require.NoError(t, err)
	require.Equal(t, "explicit/model", got, "an explicit --model is never overridden")

	got, err = foldRoleModel(ctx, a, "smart", config.SelectedModelTypeSmart, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "", got, "--role smart never folds")
}
