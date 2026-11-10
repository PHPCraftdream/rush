package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestSessionNoticeInsertionAdmitsManagedAncestors(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			env.conn.SetMaxOpenConns(1)
			api := finalizationCommitService(env)
			parent := finalizationCommitAuthority(t, api, "owner", "parent-call", 1)
			child := finalizationCommitAuthority(t, api, "child", "child-call", 1)
			grandchild := finalizationCommitAuthority(t, api, "grandchild", "grandchild-call", 1)
			finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity, grandchild.identity)
			ctx, err := WithAgentFinalizationContext(env.ctx, parent, child, grandchild)
			require.NoError(t, err)
			_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER notice_requires_ancestor_tickets BEFORE INSERT ON session_notices
				WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>3
				BEGIN SELECT RAISE(ABORT, 'notice missing ancestor tickets'); END`)
			require.NoError(t, err)
			if rollback {
				_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER notice_writer_abort BEFORE INSERT ON session_notices
					BEGIN SELECT RAISE(ABORT, 'notice writer rollback'); END`)
				require.NoError(t, err)
			}
			id, err := NewSessionNoticeStore(env.conn).InsertSessionNoticeReturningID(ctx, "grandchild", NoticeKindWakeOnly, "completion", true, "")
			if rollback {
				require.ErrorContains(t, err, "notice writer rollback")
				require.Zero(t, id)
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices`))
			} else {
				require.NoError(t, err)
				require.Positive(t, id)
				require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices WHERE owner='grandchild' AND text='completion'`))
			}
			for _, authority := range []*AgentFinalizationAuthority{parent, child, grandchild} {
				state, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
				require.NoError(t, err)
				require.Zero(t, state.OpenTickets)
				require.EqualValues(t, 2, state.Revision)
			}
		})
	}
}

func TestSessionNoticeInsertionDoesNotDowngradeKnownScope(t *testing.T) {
	for _, state := range []string{"missing", "accepted", "cancelled", "stale"} {
		t.Run(state, func(t *testing.T) {
			env := finalizationWriterEnv(t)
			api := finalizationCommitService(env)
			authority := finalizationCommitAuthority(t, api, "owner", "call", 1)
			finalizationCommitFailureDiagnostics(t, env, authority.identity)
			ctx := env.ctx
			var err error
			if state != "missing" {
				ctx, err = WithAgentFinalizationContext(ctx, authority)
				require.NoError(t, err)
			}
			switch state {
			case "accepted":
				require.NoError(t, sealFinalizationForTest(ctx, env.conn, NewAgentFinalizationStore(env.conn), authority.identity, 0))
			case "cancelled":
				_, err = env.conn.ExecContext(ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='owner'`)
				require.NoError(t, err)
			case "stale":
				_, err = env.conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='owner'`)
				require.NoError(t, err)
				_, err = api.BindAgentFinalization(ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "replacement", Generation: 2})
				require.NoError(t, err)
			}
			err = NewSessionNoticeStore(env.conn).InsertSessionNotice(ctx, "owner", NoticeKindWakeOnly, "must not insert", true, "")
			if state == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			}
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices`))
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0`))
		})
	}
}

func TestChildBindAdmitsOwnerAndChildBeforeWriter(t *testing.T) {
	for _, outcome := range []string{"commit", "rollback", "missing_child", "missing_owner", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			store, conn, dir, node, root := batchBindFixture(t)
			conn.SetMaxOpenConns(1)
			ctx := t.Context()
			api := NewService(db.New(conn), conn).(AgentFinalizationService)
			owner := finalizationCommitAuthority(t, api, "owner", "owner-call", 1)
			child := finalizationCommitAuthority(t, api, "owner-child", "child-call", 1)
			env := &commitHookEnv{t: t, store: store, conn: conn, ctx: ctx}
			finalizationCommitFailureDiagnostics(t, env, owner.identity, child.identity)
			authorities := []*AgentFinalizationAuthority{owner, child}
			if outcome == "missing_child" {
				authorities = []*AgentFinalizationAuthority{owner}
			} else if outcome == "missing_owner" {
				authorities = []*AgentFinalizationAuthority{child}
			}
			ctx, err := WithAgentFinalizationContext(ctx, authorities...)
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, `CREATE TRIGGER child_bind_requires_tickets BEFORE UPDATE OF child_session_id ON batch_nodes
				WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>2
				BEGIN SELECT RAISE(ABORT, 'child bind missing tickets'); END`)
			require.NoError(t, err)
			if outcome == "rollback" {
				_, err = conn.ExecContext(ctx, `CREATE TRIGGER child_bind_writer_abort BEFORE UPDATE OF child_session_id ON batch_nodes
					BEGIN SELECT RAISE(ABORT, 'child bind writer rollback'); END`)
				require.NoError(t, err)
			} else if outcome == "cancelled" {
				_, err = conn.ExecContext(ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='owner-child'`)
				require.NoError(t, err)
			}
			err = store.BatchBindChildSession(ctx, "owner", "host", "runner", root, node, 0, "owner-child")
			if outcome == "commit" {
				require.NoError(t, err)
				require.Equal(t, "owner-child", batchBindReadLink(t, dir, node).String)
			} else {
				switch outcome {
				case "rollback":
					require.ErrorContains(t, err, "child bind writer rollback")
				case "cancelled":
					require.ErrorIs(t, err, context.Canceled)
				default:
					require.ErrorContains(t, err, "explicit authority required")
				}
				require.False(t, batchBindReadLink(t, dir, node).Valid)
			}
			for _, authority := range []*AgentFinalizationAuthority{owner, child} {
				state, err := NewAgentFinalizationStore(conn).Snapshot(ctx, authority.identity)
				require.NoError(t, err)
				require.Zero(t, state.OpenTickets)
				if outcome == "commit" || outcome == "rollback" {
					require.EqualValues(t, 2, state.Revision)
				}
			}
		})
	}
}

func TestFinalizationMutationPreservesUnboundLegacyNoticeInsertion(t *testing.T) {
	env := finalizationWriterEnv(t)
	id, err := NewSessionNoticeStore(env.conn).InsertSessionNoticeReturningID(env.ctx, "owner", NoticeKindWakeOnly, "legacy", false, "")
	require.NoError(t, err)
	require.Positive(t, id)
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalizations`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`))
}
