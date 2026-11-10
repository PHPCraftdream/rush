package session

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func finalizationAncestorEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := newCommitHookEnv(t)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "child"))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "grandchild"))
	require.NoError(t, env.adapter()(env.ctx, admissionSubmit("parent-root", "parent-ref", 1)))
	result, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET child_session_id='child' WHERE node_id='parent-root.1'`)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	childID := env.id
	childID.Owner = "child"
	require.NoError(t, NewBatchCommitHook(env.store, childID)(env.ctx, admissionSubmit("child-root", "child-ref", 2)))
	result, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET child_session_id='grandchild' WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	changed, err = result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	return env
}

func TestBatchCommitAdmitsManagedAncestorsBeforeWriter(t *testing.T) {
	env := finalizationAncestorEnv(t)
	api := finalizationCommitService(env)
	parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
	child := finalizationCommitAuthority(t, api, "child", "child-call", 1)
	grandchild := finalizationCommitAuthority(t, api, "grandchild", "grandchild-call", 1)
	finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity, grandchild.identity)
	identity := env.id
	identity.Owner = "grandchild"
	identity, err := api.WithAgentFinalizationAuthorities(identity)
	require.NoError(t, err)
	ctx, err := WithAgentFinalizationContext(env.ctx, parent, child, grandchild)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	env.conn.SetMaxOpenConns(1)
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, owners []string) (func() error, error) {
		require.Equal(t, []string{"child", "grandchild", "owner"}, owners)
		for _, authority := range []*AgentFinalizationAuthority{parent, child, grandchild} {
			record, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
			require.NoError(t, err)
			require.EqualValues(t, 1, record.OpenTickets, "every ancestor ticket precedes writer I/O")
		}
		require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
		return func() error {
			require.EqualValues(t, 2, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
			return nil
		}, nil
	}
	require.NoError(t, NewBatchCommitHook(env.store, identity)(ctx, admissionSubmit("new-root", "new-ref", 3)))
	for _, authority := range []*AgentFinalizationAuthority{parent, child, grandchild} {
		record, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, err)
		require.Zero(t, record.OpenTickets)
		require.EqualValues(t, 2, record.Revision)
	}
}

func TestBatchCommitMissingOrAcceptedManagedAncestorCannotBecomeLegacy(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "accepted"}[accepted], func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			api := finalizationCommitService(env)
			parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
			child := finalizationCommitAuthority(t, api, "grandchild", "grandchild-call", 1)
			finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity)
			identity := env.id
			identity.Owner = "grandchild"
			authorities := []*AgentFinalizationAuthority{child}
			if accepted {
				require.NoError(t, sealFinalizationForTest(env.ctx, env.conn, NewAgentFinalizationStore(env.conn), parent.identity, 0))
				authorities = append(authorities, parent)
			}
			identity, err := api.WithAgentFinalizationAuthorities(identity, authorities...)
			require.NoError(t, err)
			err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 3))
			require.Error(t, err)
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
			record, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, child.identity)
			require.NoError(t, err)
			require.Zero(t, record.OpenTickets, "partial multi-owner admission releases on failure")
		})
	}
}

func TestBatchCommitAncestorTraversalRejectsCycleBeforeTickets(t *testing.T) {
	env := finalizationAncestorEnv(t)
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET child_session_id='owner' WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	api := finalizationCommitService(env)
	parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
	child := finalizationCommitAuthority(t, api, "child", "child-call", 1)
	identity, err := api.WithAgentFinalizationAuthorities(env.id, parent, child)
	require.NoError(t, err)
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 3))
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
}

func TestBatchCommitDoesNotTreatSessionForkAsManagedAncestor(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := newCommitHookEnv(t)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "fork"))
	_, err := env.conn.ExecContext(env.ctx, `UPDATE sessions SET parent_session_id='owner' WHERE id='fork'`)
	require.NoError(t, err)
	api := finalizationCommitService(env)
	parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
	require.NoError(t, sealFinalizationForTest(env.ctx, env.conn, NewAgentFinalizationStore(env.conn), parent.identity, 0))
	identity := env.id
	identity.Owner = "fork"
	identity, err = api.WithAgentFinalizationAuthorities(identity)
	require.NoError(t, err)
	require.NoError(t, NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("fork-root", "fork-ref", 1)))
	require.EqualValues(t, 2, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='fork-root'`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`))
}

func TestBatchCommitAncestorTicketsReleaseAfterRollbackOrCancelledOwner(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer_rollback", true: "cancelled_ancestor"}[cancelled], func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			api := finalizationCommitService(env)
			parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
			child := finalizationCommitAuthority(t, api, "child", "child-call", 1)
			grandchild := finalizationCommitAuthority(t, api, "grandchild", "grandchild-call", 1)
			finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity, grandchild.identity)
			identity := env.id
			identity.Owner = "grandchild"
			identity, err := api.WithAgentFinalizationAuthorities(identity, parent, child, grandchild)
			require.NoError(t, err)
			if cancelled {
				_, err = env.conn.ExecContext(env.ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='owner'`)
			} else {
				_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER ancestor_writer_abort BEFORE INSERT ON batch_nodes
					WHEN NEW.root_id='new-root' BEGIN SELECT RAISE(ABORT, 'ancestor writer rollback'); END`)
			}
			require.NoError(t, err)
			err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("new-root", "new-ref", 3))
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorContains(t, err, "ancestor writer rollback")
			}
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE root_id='new-root'`))
			for _, authority := range []*AgentFinalizationAuthority{parent, child, grandchild} {
				record, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
				require.NoError(t, err)
				require.Zero(t, record.OpenTickets)
			}
		})
	}
}

func TestFinalizationContextRejectsReplacementAndForeignService(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := newCommitHookEnv(t)
	api := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, api, "owner", "call", 1)
	ctx, err := WithAgentFinalizationContext(env.ctx, authority)
	require.NoError(t, err)
	ctx, err = WithAgentFinalizationContext(ctx, authority)
	require.NoError(t, err, "same capability inheritance is idempotent")
	_, err = WithAgentFinalizationContext(ctx, &AgentFinalizationAuthority{
		service:  authority.service,
		identity: AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "replacement", Generation: 2},
	})
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	otherService := finalizationCommitService(env)
	identity, err := otherService.WithAgentFinalizationAuthorities(env.id)
	require.NoError(t, err)
	err = NewBatchCommitHook(env.store, identity)(ctx, admissionSubmit("new-root", "new-ref", 1))
	require.ErrorContains(t, err, "another service")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
}
