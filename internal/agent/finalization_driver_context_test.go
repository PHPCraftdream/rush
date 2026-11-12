package agent

import (
	"context"
	"database/sql"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type finalizationDriverFixture struct {
	conn       *sql.DB
	ctx        context.Context
	api        session.AgentFinalizationService
	parent     session.AgentFinalizationIdentity
	child      session.AgentFinalizationIdentity
	capability session.AgentFinalizationContext
}

func newFinalizationDriverFixture(t *testing.T) finalizationDriverFixture {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	store, conn, ctx := newTestBatchStore(t)
	queries := db.New(conn)
	_, err := queries.CreateSession(ctx, db.CreateSessionParams{ID: "owner", Title: "owner"})
	require.NoError(t, err)
	_, err = queries.CreateSession(ctx, db.CreateSessionParams{ID: "child", Title: "child", ParentSessionID: sql.NullString{String: "owner", Valid: true}})
	require.NoError(t, err)
	err = session.NewBatchCommitHook(store, session.BatchCommitIdentity{Host: "host", Runner: "runner", Owner: "owner"})(ctx, jobbatch.Transition{
		Sequence: 1, Event: jobbatch.Event{Type: jobbatch.EventSubmit, Root: "delegation", Ref: "ref", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"})},
	})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET child_session_id='child', token=1 WHERE node_id='delegation.1'`)
	require.NoError(t, err)
	api := session.NewService(queries, conn).(session.AgentFinalizationService)
	parent := session.AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "parent-call", Generation: 1}
	child := session.AgentFinalizationIdentity{SessionID: "child", LogicalCallID: "child-call", Generation: 1, NodeID: "delegation.1", NodeToken: 1}
	parentAuthority, err := api.BindAgentFinalization(ctx, parent)
	require.NoError(t, err)
	childAuthority, err := api.BindAgentFinalization(ctx, child)
	require.NoError(t, err)
	ctx, err = session.WithAgentFinalizationContext(ctx, parentAuthority, childAuthority)
	require.NoError(t, err)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, identity := range []session.AgentFinalizationIdentity{parent, child} {
			state, err := session.NewAgentFinalizationStore(conn).Snapshot(context.Background(), identity)
			t.Logf("driver authority identity=%+v state=%+v error=%v", identity, state, err)
		}
	})
	return finalizationDriverFixture{conn: conn, ctx: ctx, api: api, parent: parent, child: child, capability: session.CaptureAgentFinalizationContext(ctx)}
}

func TestDriverWakePreservesAuthorityAndImmutableCallPolicy(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	credentials := &CredentialSet{}
	options := &CallOptions{MaxAgentDepth: 1, AgentDepthSet: true}
	original := SessionAgentCall{
		SessionID: "child", LogicalCallID: "child-call", Tools: []fantasy.AgentTool{},
		MaxCost: 2, MaxTokens: 100, RequestedModelRole: RequestedModelRole("reviewer"),
		Credentials: credentials, CallOptions: options,
	}
	agent := &sessionAgent{}
	registry := newSubAgentDriverRegistry()
	generation := registry.register("child", subAgentDriver{agent: agent, call: original, finalizationAuthorities: fixture.capability})
	driver, ok := registry.get("child")
	require.True(t, ok)
	require.Same(t, agent, driver.agent, "wake retains the same driver")
	require.Equal(t, generation, driver.generation)
	wake := newDrainCall(driver.callFor("completion"))
	require.Equal(t, original.LogicalCallID, wake.LogicalCallID)
	require.Same(t, credentials, wake.Credentials)
	require.NotNil(t, wake.Tools, "explicit empty tool baseline remains empty")
	require.Empty(t, wake.Tools)
	require.Equal(t, original.MaxCost, wake.MaxCost)
	require.Equal(t, original.MaxTokens, wake.MaxTokens)
	require.Equal(t, original.RequestedModelRole, wake.RequestedModelRole)
	wake.CallOptions.MaxAgentDepth = 0
	require.Equal(t, 1, options.MaxAgentDepth, "wake options cannot mutate the frozen template")
	ctx, err := agent.restoreFinalizationCallContext(context.Background(), &wake)
	require.NoError(t, err)
	err = session.NewSessionNoticeStore(fixture.conn).InsertSessionNotice(ctx, "child", session.NoticeKindWakeOnly, "completion", true, "")
	require.NoError(t, err, "background wake must carry both child and managed-parent authority")
	for _, identity := range []session.AgentFinalizationIdentity{fixture.parent, fixture.child} {
		state, err := session.NewAgentFinalizationStore(fixture.conn).Snapshot(ctx, identity)
		require.NoError(t, err)
		require.EqualValues(t, 2, state.Revision)
		require.Zero(t, state.OpenTickets)
	}
}

func TestDriverWakeCannotRebindReplacementOrWidenLogicalCall(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "logical_call_mismatch", true: "replaced_incarnation"}[replacement], func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			driver := subAgentDriver{call: SessionAgentCall{SessionID: "child", LogicalCallID: "child-call"}, finalizationAuthorities: fixture.capability}
			wake := driver.callFor("wake")
			ctx := context.Background()
			if replacement {
				_, err := fixture.conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='child'`)
				require.NoError(t, err)
				next := fixture.child
				next.LogicalCallID, next.Generation = "replacement", 2
				authority, err := fixture.api.BindAgentFinalization(ctx, next)
				require.NoError(t, err)
				ctx, err = session.WithAgentFinalizationContext(ctx, authority)
				require.NoError(t, err)
			} else {
				wake.LogicalCallID = "different-call"
			}
			_, err := (&sessionAgent{}).restoreFinalizationCallContext(ctx, &wake)
			require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)
			var notices int64
			require.NoError(t, fixture.conn.QueryRowContext(ctx, `SELECT count(*) FROM session_notices`).Scan(&notices))
			require.Zero(t, notices)
		})
	}
}

func TestRunRefusesStaleCarriedAuthorityBeforePreamble(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "accepted"}[accepted], func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			if accepted {
				tx, err := fixture.conn.BeginTx(fixture.ctx, nil)
				require.NoError(t, err)
				defer tx.Rollback()
				_, err = tx.ExecContext(fixture.ctx, `UPDATE session_agent_finalizations SET candidate_message_id='candidate', candidate_hash='hash', candidate_serial=1 WHERE session_id='child'`)
				require.NoError(t, err)
				require.NoError(t, session.NewAgentFinalizationStore(fixture.conn).SealTx(fixture.ctx, tx, fixture.child, session.AgentFinalizationCandidate{MessageID: "candidate", Hash: "hash", Serial: 1}))
				require.NoError(t, tx.Commit())
			} else {
				_, err := fixture.conn.ExecContext(fixture.ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='child'`)
				require.NoError(t, err)
			}
			agent := &sessionAgent{}
			call := (subAgentDriver{call: SessionAgentCall{SessionID: "child", LogicalCallID: "child-call"}, finalizationAuthorities: fixture.capability}).callFor("")
			_, err := agent.Run(context.Background(), call)
			require.ErrorIs(t, err, session.ErrAgentFinalizationChanged, "stale authority is refused before even empty-prompt validation")
			_, err = agent.Run(context.Background(), SessionAgentCall{SessionID: "child"})
			require.ErrorIs(t, err, ErrEmptyPrompt, "unbound legacy entry preserves its existing refusal")
		})
	}
}
