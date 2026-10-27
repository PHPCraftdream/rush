package cmd

// Tests for foldRoleModel (task #1060, plan D2): `rush run --role worker` /
// `--role reviewer` validates against the configured role slot only — a
// model stored on the session never satisfies it, because rush run follows
// settings. The fold no longer copies a model into the smart slot — the
// coordinator resolves the role's model live per turn — so foldRoleModel
// only resolves --continue's session and validates that the slot exists.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// roleTestModels configures both role slots, as `rush models use --worker
// --reviewer` would.
var roleTestModels = map[config.SelectedModelType]config.SelectedModel{
	config.SelectedModelTypeWorker:   {Provider: "cfg-prov", Model: "cfg-worker"},
	config.SelectedModelTypeReviewer: {Provider: "cfg-prov", Model: "cfg-reviewer"},
}

// TestFoldRoleModel_SessionPinDoesNotReplaceConfigSlot: a worker/reviewer
// model stored on the session no longer satisfies --role; with the slot
// unconfigured the run fails fast instead of running a stale session model.
//
// Revert-check: restoring the session-pin early return in foldRoleModel
// makes the worker case return no error.
func TestFoldRoleModel_SessionPinDoesNotReplaceConfigSlot(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()
	sess, err := s.Create(ctx, "pinned")
	require.NoError(t, err)
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, sess.ID,
		&session.ModelSlotUpdate{Provider: "pin-prov", Model: "pin-worker"},
		&session.ModelSlotUpdate{Provider: "pin-prov", Model: "pin-reviewer"}))
	a := &app.App{Sessions: s}

	_, _, _, err = foldRoleModel(ctx, a, nil, "worker", config.SelectedModelTypeWorker, "", sess.ID, false)
	require.ErrorContains(t, err, "no worker model configured")
	_, _, _, err = foldRoleModel(ctx, a, nil, "reviewer", config.SelectedModelTypeReviewer, "", sess.ID, false)
	require.ErrorContains(t, err, "no reviewer model configured")
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
// `rush run --continue --role worker` names its session only implicitly; the
// fold resolves the session --continue continues and hands it on, while an
// explicit --session still wins (the run's own precedence).
//
// Revert-check: the GetLast resolution removed from foldRoleModel -- the fold
// returns the caller's ("", true) pair, failing the resolved-id assertion.
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

	got, resolved, useLast, err := foldRoleModel(ctx, a, roleTestModels, "worker", config.SelectedModelTypeWorker, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "", got, "the fold no longer copies the model (D2); resolution is per turn in the coordinator")
	require.Equal(t, last.ID, resolved, "the resolved session is handed on so the run does not resolve it again")
	require.False(t, useLast)

	got, _, _, err = foldRoleModel(ctx, a, roleTestModels, "reviewer", config.SelectedModelTypeReviewer, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "", got, "the fold no longer copies the model (D2); resolution is per turn in the coordinator")

	got, resolved, useLast, err = foldRoleModel(ctx, a, roleTestModels, "worker", config.SelectedModelTypeWorker, "", older.ID, true)
	require.NoError(t, err)
	require.Equal(t, "", got, "the fold no longer copies the model (D2); resolution is per turn in the coordinator")
	require.Equal(t, older.ID, resolved)
	require.True(t, useLast, "an explicit id resolves nothing: the flags come back as given")

	got, resolved, useLast, err = foldRoleModel(ctx, a, roleTestModels, "worker", config.SelectedModelTypeWorker, "explicit/model", "", true)
	require.NoError(t, err)
	require.Equal(t, "explicit/model", got, "an explicit --model is never overridden")
	require.Empty(t, resolved)
	require.True(t, useLast)

	got, _, _, err = foldRoleModel(ctx, a, roleTestModels, "smart", config.SelectedModelTypeSmart, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "", got, "--role smart never folds")
}

// bumpingLastService answers GetLast with a different session on every call, as
// happens when another process creates or touches a session between two reads.
type bumpingLastService struct {
	session.Service
	ids   []string
	calls int
}

func (b *bumpingLastService) GetLast(ctx context.Context) (session.Session, error) {
	id := b.ids[min(b.calls, len(b.ids)-1)]
	b.calls++
	return b.Get(ctx, id)
}

// TestFoldRoleModel_ResolvedLastSessionIsHandedOn is R3C-10: the session the
// fold reads the pin from is the one the run must continue. The fold returns
// that id (useLast cleared), so the run resolves nothing a second time; a
// "last session" that moved between the two reads can no longer split the pin
// (session A) from the continued session (B).
//
// Revert-check: returning the caller's ("", true) pair unchanged again (the
// run then resolves GetLast itself) turns the id and flag assertions red.
func TestFoldRoleModel_ResolvedLastSessionIsHandedOn(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	a1, err := s.Create(ctx, "a")
	require.NoError(t, err)
	b1, err := s.Create(ctx, "b")
	require.NoError(t, err)
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, a1.ID,
		&session.ModelSlotUpdate{Provider: "a-prov", Model: "a-worker"}, nil))
	require.NoError(t, s.UpdateWorkerReviewerModels(ctx, b1.ID,
		&session.ModelSlotUpdate{Provider: "b-prov", Model: "b-worker"}, nil))
	svc := &bumpingLastService{Service: s, ids: []string{a1.ID, b1.ID}}
	a := &app.App{Sessions: svc}

	model, sessionID, useLast, err := foldRoleModel(ctx, a, roleTestModels, "worker", config.SelectedModelTypeWorker, "", "", true)

	require.NoError(t, err)
	require.Equal(t, "", model, "the fold no longer copies the model (D2); resolution is per turn in the coordinator")
	require.Equal(t, a1.ID, sessionID, "the run continues the session the pin came from")
	require.False(t, useLast, "the run must not resolve the last session again")
	require.Equal(t, 1, svc.calls, "exactly one GetLast")
}

// TestFoldRoleModel_NoFoldLeavesSmartEmpty pins the D2 contract's no-fold
// paths: an explicit --model comes back unchanged even for a role run, and
// --role smart never folds. The unconfigured-slot path is covered by
// TestFoldRoleModel_SessionPinDoesNotReplaceConfigSlot.
func TestFoldRoleModel_NoFoldLeavesSmartEmpty(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	a := &app.App{Sessions: s}
	ctx := context.Background()

	got, resolved, useLast, err := foldRoleModel(ctx, a, roleTestModels, "worker", config.SelectedModelTypeWorker, "explicit/model", "", true)
	require.NoError(t, err)
	require.Equal(t, "explicit/model", got, "an explicit --model is returned unchanged")
	require.Empty(t, resolved)
	require.True(t, useLast)

	got, resolved, useLast, err = foldRoleModel(ctx, a, roleTestModels, "smart", config.SelectedModelTypeSmart, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "", got, "--role smart never folds")
	require.Empty(t, resolved)
	require.True(t, useLast)
}
