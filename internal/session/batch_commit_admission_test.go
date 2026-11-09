package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func admissionSubmit(root, ref string, sequence uint64) jobbatch.Transition {
	return jobbatch.Transition{
		Sequence: sequence,
		Event: jobbatch.Event{
			Type: jobbatch.EventSubmit, Root: jobbatch.ID(root), Ref: ref,
			Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}),
		},
	}
}

func TestBatchCommitAdmissionRefusesBeforeReservation(t *testing.T) {
	env := newCommitHookEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// One connection makes callback reads expose admission inside a writer tx.
	env.conn.SetMaxOpenConns(1)
	denied := errors.New("scope sealed")
	admissions, releases := 0, 0
	env.id.OwnerFor = func(jobbatch.Spec) string { return "owner" }
	env.id.Owner = "wrong-default"
	env.id.AdmitOwners = func(ctx context.Context, tr jobbatch.Transition, owners []string) (func(), error) {
		admissions++
		require.Equal(t, jobbatch.EventSubmit, tr.Event.Type)
		require.Equal(t, []string{"owner"}, owners, "use the resolved invoking owner")
		var count int
		require.NoError(t, env.conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes").Scan(&count))
		require.Zero(t, count)
		return func() { releases++ }, denied
	}
	err := env.adapter()(ctx, admissionSubmit("r", "call-r", 1))
	require.Same(t, denied, err, "admission errors retain their identity")
	require.Equal(t, 1, admissions)
	require.Equal(t, 1, releases, "partially acquired tickets release on refusal")
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
}

func TestBatchCommitAdmissionReleaseObservesCommittedReservation(t *testing.T) {
	env := newCommitHookEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	env.conn.SetMaxOpenConns(1)
	released := false
	env.id.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, owners []string) (func(), error) {
		require.Equal(t, []string{"owner"}, owners)
		var count int
		require.NoError(t, env.conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes").Scan(&count))
		require.Zero(t, count)
		return func() {
			owner, err := env.store.RootOwner(ctx, "r")
			require.NoError(t, err, "release must run after writer commit")
			require.Equal(t, "owner", owner)
			released = true
		}, nil
	}
	require.NoError(t, env.adapter()(ctx, admissionSubmit("r", "call-r", 1)))
	require.True(t, released)
}

func TestBatchCommitAdmissionReleasesAfterWriterRollback(t *testing.T) {
	env := newCommitHookEnv(t)
	_, err := env.conn.ExecContext(env.ctx, `CREATE TRIGGER finalization_admission_abort
		BEFORE INSERT ON batch_nodes BEGIN SELECT RAISE(ABORT, 'admission writer failure'); END`)
	require.NoError(t, err)
	releases := 0
	env.id.AdmitOwners = func(context.Context, jobbatch.Transition, []string) (func(), error) {
		return func() {
			releases++
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
		}, nil
	}
	err = env.adapter()(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "admission writer failure")
	require.Equal(t, 1, releases)
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
}

func TestBatchCommitAdmissionCollectsEveryProjectionOwnerBeforeWriter(t *testing.T) {
	env := newCommitHookEnv(t)
	require.NoError(t, env.adapter()(env.ctx, admissionSubmit("r", "call-r", 1)))
	require.NoError(t, env.adapter()(env.ctx, admissionSubmit("s", "call-s", 2)))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "another-owner"))
	other := env.id
	other.Owner = "another-owner"
	require.NoError(t, NewBatchCommitHook(env.store, other)(env.ctx, admissionSubmit("q", "call-q", 3)))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	env.conn.SetMaxOpenConns(1)
	denied := errors.New("one affected scope is sealed")
	admissions := 0
	env.id.AdmitOwners = func(ctx context.Context, tr jobbatch.Transition, owners []string) (func(), error) {
		admissions++
		require.Equal(t, jobbatch.EventStarted, tr.Event.Type)
		require.Equal(t, []string{"another-owner", "owner"}, owners, "all owners arrive once, ordered and distinct")
		var running int
		require.NoError(t, env.conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes WHERE state = 'running'").Scan(&running))
		require.Zero(t, running, "no root may be projected before every ticket is acquired")
		return nil, denied
	}
	changed := make([]jobbatch.NodeView, 0, 3)
	for _, root := range []jobbatch.ID{"s", "q", "r"} {
		changed = append(changed, jobbatch.NodeView{ID: root, Root: root, State: jobbatch.StateRunning, Control: jobbatch.ControlNone})
	}
	err := env.adapter()(ctx, jobbatch.Transition{
		Sequence: 4, Event: jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1}, Changed: changed,
	})
	require.Same(t, denied, err)
	require.Equal(t, 1, admissions)
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE state = 'running'"))
}

func TestBatchCommitAdmissionRequiresReleaseAndSkipsOwnerlessMaintenance(t *testing.T) {
	env := newCommitHookEnv(t)
	admissions := 0
	env.id.AdmitOwners = func(context.Context, jobbatch.Transition, []string) (func(), error) {
		admissions++
		return nil, nil
	}
	err := env.adapter()(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "missing release")
	require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
	require.NoError(t, env.adapter()(env.ctx, jobbatch.Transition{
		Sequence: 2, Event: jobbatch.Event{Type: jobbatch.EventCapacityAvailable},
	}))
	require.Equal(t, 1, admissions, "ownerless checkpoint maintenance is not owned work")
}
