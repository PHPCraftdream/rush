package session

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func finalizationWriterEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	return newCommitHookEnv(t)
}

func finalizationWriterTransition(project bool) jobbatch.Transition {
	if !project {
		return admissionSubmit("new-root", "new-ref", 2)
	}
	return jobbatch.Transition{
		Sequence: 2, Event: jobbatch.Event{Type: jobbatch.EventStarted, Node: "existing.1", Token: 1},
		Changed: []jobbatch.NodeView{{ID: "existing", Root: "existing", State: jobbatch.StateRunning, Control: jobbatch.ControlNone}},
	}
}

func assertFinalizationWriterUnchanged(t *testing.T, env *commitHookEnv) {
	t.Helper()
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root' OR state='running'`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints WHERE sequence>=2`))
}

func TestBatchWriterRejectsScopeBoundAfterLegacyAdmission(t *testing.T) {
	for _, project := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			name := map[bool]string{false: "reserve", true: "project"}[project] + "/" + map[bool]string{false: "bound", true: "accepted"}[accepted]
			t.Run(name, func(t *testing.T) {
				env := finalizationWriterEnv(t)
				if project {
					require.NoError(t, env.adapter()(env.ctx, admissionSubmit("existing", "existing-ref", 1)))
				}
				api := finalizationCommitService(env)
				identity, err := api.WithAgentFinalizationAuthorities(env.id)
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				env.conn.SetMaxOpenConns(1)
				observed := make(chan struct{})
				resume := make(chan struct{})
				completed := make(chan error, 1)
				finished := make(chan struct{})
				released := make(chan struct{}, 1)
				identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, owners []string) (func() error, error) {
					close(observed)
					select {
					case <-resume:
					case <-ctx.Done():
						return func() error { released <- struct{}{}; return nil }, ctx.Err()
					}
					return func() error { released <- struct{}{}; return nil }, nil
				}
				go func() {
					defer close(finished)
					completed <- NewBatchCommitHook(env.store, identity)(ctx, finalizationWriterTransition(project))
				}()
				defer func() {
					cancel()
					<-finished
				}()
				select {
				case <-observed:
				case <-ctx.Done():
					t.Fatal("admission barrier not reached")
				}
				bound := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "late-bind", Generation: 1}
				_, err = finalizationCommitService(env).BindAgentFinalization(ctx, bound)
				require.NoError(t, err, "independent service binds between legacy observation and writer")
				if accepted {
					require.NoError(t, sealFinalizationForTest(ctx, env.conn, NewAgentFinalizationStore(env.conn), bound, 0))
				}
				finalizationCommitFailureDiagnostics(t, env, bound)
				close(resume)
				select {
				case err = <-completed:
				case <-ctx.Done():
					t.Fatal("writer did not resolve")
				}
				require.ErrorIs(t, err, ErrAgentFinalizationChanged)
				select {
				case <-released:
				default:
					t.Fatal("admission release missing after transaction refusal")
				}
				assertFinalizationWriterUnchanged(t, env)
				state, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, bound)
				require.NoError(t, err)
				require.Zero(t, state.Revision)
				require.Zero(t, state.OpenTickets)
				if accepted {
					require.Equal(t, "accepted", state.Phase, "accepted receipt cannot be bypassed by a stale legacy admission")
				} else {
					require.Equal(t, "executing", state.Phase)
				}
			})
		}
	}
}

func TestBatchWriterRejectsChangedManagedAncestryAfterAdmission(t *testing.T) {
	for _, change := range []string{"new_parent", "removed_edge", "same_owner_new_token"} {
		t.Run(change, func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			if change == "new_parent" {
				require.NoError(t, seedSession(env.ctx, db.New(env.conn), "new-parent"))
				other := env.id
				other.Owner = "new-parent"
				require.NoError(t, NewBatchCommitHook(env.store, other)(env.ctx, admissionSubmit("external-parent", "external-ref", 3)))
			}
			api := finalizationCommitService(env)
			parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
			child := finalizationCommitAuthority(t, api, "child", "child-call", 1)
			grandchild := finalizationCommitAuthority(t, api, "grandchild", "grandchild-call", 1)
			identity := env.id
			identity.Owner = "grandchild"
			identity, err := api.WithAgentFinalizationAuthorities(identity, parent, child, grandchild)
			require.NoError(t, err)
			finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity, grandchild.identity)
			identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, _ []string) (func() error, error) {
				var err error
				switch change {
				case "new_parent":
					_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET child_session_id='grandchild' WHERE node_id='external-parent.1'`)
				case "removed_edge":
					_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET child_session_id=NULL WHERE node_id='parent-root.1'`)
				case "same_owner_new_token":
					_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token=token+1 WHERE node_id='parent-root.1'`)
				}
				return func() error { return nil }, err
			}
			err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 4))
			require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints WHERE sequence>=4`))
			for _, authority := range []*AgentFinalizationAuthority{parent, child, grandchild} {
				record, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
				require.NoError(t, err)
				require.Zero(t, record.OpenTickets)
				require.EqualValues(t, 2, record.Revision)
			}
		})
	}
}

func TestBatchWriterRejectsIncarnationReplacementAfterAdmission(t *testing.T) {
	env := finalizationWriterEnv(t)
	api := finalizationCommitService(env)
	old := finalizationCommitAuthority(t, api, "owner", "old-call", 1)
	identity, err := api.WithAgentFinalizationAuthorities(env.id, old)
	require.NoError(t, err)
	next := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "new-call", Generation: 2}
	finalizationCommitFailureDiagnostics(t, env, old.identity, next)
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, _ []string) (func() error, error) {
		_, err := env.conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='owner' AND logical_call_id='old-call'`)
		if err == nil {
			_, err = api.BindAgentFinalization(ctx, next)
		}
		return func() error { return nil }, err
	}
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 2))
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	assertFinalizationWriterUnchanged(t, env)
	oldState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, old.identity)
	require.NoError(t, err)
	require.Zero(t, oldState.OpenTickets)
	require.EqualValues(t, 2, oldState.Revision)
	nextState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, next)
	require.NoError(t, err)
	require.Zero(t, nextState.OpenTickets)
	require.Zero(t, nextState.Revision)
}

func TestBatchWriterRequiresItsExactUnreleasedTicket(t *testing.T) {
	env := finalizationWriterEnv(t)
	api := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, api, "owner", "call", 1)
	identity, err := api.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	store := NewAgentFinalizationStore(env.conn)
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, _ []string) (func() error, error) {
		var ticket string
		err := env.conn.QueryRowContext(ctx, `SELECT ticket_id FROM session_agent_finalization_tickets WHERE released=0`).Scan(&ticket)
		if err == nil {
			err = store.Release(ctx, authority.identity, ticket)
		}
		if err == nil {
			_, err = store.Admit(ctx, authority.identity, "unrelated-ticket")
		}
		return func() error { return nil }, err
	}
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 2))
	require.ErrorIs(t, err, ErrAgentFinalizationChanged, "another open ticket is not this transition's admission")
	assertFinalizationWriterUnchanged(t, env)
	state, err := store.Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 1, state.OpenTickets)
	require.NoError(t, store.Release(env.ctx, authority.identity, "unrelated-ticket"))
}

func TestBatchWriterChecksPersistedCancellationAfterAdmission(t *testing.T) {
	env := finalizationWriterEnv(t)
	api := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, api, "owner", "call", 1)
	identity, err := api.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, _ []string) (func() error, error) {
		_, err := env.conn.ExecContext(ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='owner'`)
		return func() error { return nil }, err
	}
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 2))
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, env.ctx.Err(), "the transaction must check durable cancellation, not just its context")
	assertFinalizationWriterUnchanged(t, env)
	state, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.Zero(t, state.OpenTickets)
	require.EqualValues(t, 2, state.Revision)
}

func TestDirectBatchWriterCannotBypassBoundScope(t *testing.T) {
	env := finalizationWriterEnv(t)
	require.NoError(t, env.adapter()(env.ctx, admissionSubmit("existing", "existing-ref", 1)))
	api := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, api, "owner", "call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	root, err := batchSpecNode(jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}))
	require.NoError(t, err)
	_, err = env.store.ReserveIdentity(env.ctx, BatchReservation{
		Owner: "owner", Host: env.id.Host, RunnerID: env.id.Runner, RootID: "new-root", ToolCallID: "new-ref", InputHash: "hash", Sequence: 2, Root: root,
	})
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	err = env.store.Project(env.ctx, BatchProjection{
		Host: env.id.Host, RunnerID: env.id.Runner, Sequence: 2,
		Roots: []BatchRootProjection{{Owner: "owner", Root: "existing"}},
	})
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	assertFinalizationWriterUnchanged(t, env)
	state, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.Zero(t, state.OpenTickets)
	require.Zero(t, state.Revision)
}
