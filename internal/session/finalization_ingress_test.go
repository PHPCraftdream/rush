package session

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func finalizationIngressAncestors(t *testing.T, env *commitHookEnv) (context.Context, []*AgentFinalizationAuthority) {
	t.Helper()
	api := finalizationCommitService(env)
	parent := finalizationCommitAuthority(t, api, "owner", "ingress-parent", 1)
	child := finalizationCommitAuthority(t, api, "child", "ingress-child", 1)
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET token=1 WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	grandchild, err := api.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{
		SessionID: "grandchild", LogicalCallID: "ingress-grandchild", Generation: 1, NodeID: "child-root.1", NodeToken: 1,
	})
	require.NoError(t, err)
	authorities := []*AgentFinalizationAuthority{parent, child, grandchild}
	finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity, grandchild.identity)
	ctx, err := WithAgentFinalizationContext(env.ctx, authorities...)
	require.NoError(t, err)
	return ctx, authorities
}

func finalizationIngressQuestionOwners(t *testing.T, env *commitHookEnv) (context.Context, []*AgentFinalizationAuthority) {
	t.Helper()
	api := finalizationCommitService(env)
	parent := finalizationCommitAuthority(t, api, "owner", "question-parent", 1)
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET token=1 WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	child, err := api.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{
		SessionID: "child", LogicalCallID: "question-child", Generation: 1, NodeID: "child-root.1", NodeToken: 1,
	})
	require.NoError(t, err)
	authorities := []*AgentFinalizationAuthority{parent, child}
	finalizationCommitFailureDiagnostics(t, env, parent.identity, child.identity)
	ctx, err := WithAgentFinalizationContext(env.ctx, authorities...)
	require.NoError(t, err)
	return ctx, authorities
}

func TestBatchQuestionPublishAdmissionRollbackAndIdempotentNoop(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			env.conn.SetMaxOpenConns(1)
			ctx, authorities := finalizationIngressQuestionOwners(t, env)
			marker := "[batch-question node=child-root.1 token=1 sequence=1 depth=1 child=grandchild]\n"
			_, err := env.conn.ExecContext(ctx, `CREATE TRIGGER question_requires_all_ancestor_tickets BEFORE INSERT ON session_notices
				WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>2
				BEGIN SELECT RAISE(ABORT, 'question missing ancestor tickets'); END`)
			require.NoError(t, err)
			if rollback {
				_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER question_writer_abort BEFORE INSERT ON session_notices
					BEGIN SELECT RAISE(ABORT, 'question writer rollback'); END`)
				require.NoError(t, err)
			}
			err = env.store.BatchPublishQuestion(ctx, "child", "child-root.1", 1, marker, marker+"question")
			if rollback {
				require.ErrorContains(t, err, "question writer rollback")
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices WHERE kind='child_question'`))
			} else {
				require.NoError(t, err)
				require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices WHERE owner='child' AND kind='child_question'`))
				before, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authorities[0].identity)
				require.NoError(t, snapshotErr)
				require.EqualValues(t, 2, before.Revision)
				after, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authorities[1].identity)
				require.NoError(t, snapshotErr)
				require.EqualValues(t, 2, after.Revision)
				require.Zero(t, before.OpenTickets)
				require.Zero(t, after.OpenTickets)
				require.NoError(t, env.store.BatchPublishQuestion(ctx, "child", "child-root.1", 1, marker, marker+"duplicate"))
				after, snapshotErr = NewAgentFinalizationStore(env.conn).Snapshot(ctx, authorities[0].identity)
				require.NoError(t, snapshotErr)
				require.Equal(t, before.Revision, after.Revision, "duplicate publish must not mint a revision")
			}
			for _, authority := range authorities {
				state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
				require.NoError(t, snapshotErr)
				if rollback {
					require.EqualValues(t, 2, state.Revision, "tickets release despite writer rollback")
				}
			}
		})
	}
}

func TestBatchQuestionPublishRejectsUnprovenScopes(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, outcome := range []string{"missing", "stale", "cancelled", "sealed"} {
		t.Run(outcome, func(t *testing.T) {
			env := finalizationAncestorEnv(t)
			ctx, authorities := finalizationIngressQuestionOwners(t, env)
			if outcome == "missing" {
				ctx = env.ctx
			} else if outcome == "stale" {
				_, err := env.conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id='child'`)
				require.NoError(t, err)
			} else if outcome == "cancelled" {
				_, err := env.conn.ExecContext(ctx, `UPDATE sessions SET cancel_requested=1 WHERE id='child'`)
				require.NoError(t, err)
			} else if outcome == "sealed" {
				require.NoError(t, sealFinalizationForTest(ctx, env.conn, NewAgentFinalizationStore(env.conn), authorities[1].identity, 0))
			}
			marker := fmt.Sprintf("[batch-question node=child-root.1 token=1 sequence=1 depth=1 child=grandchild-%s]\n", outcome)
			err := env.store.BatchPublishQuestion(ctx, "child", "child-root.1", 1, marker, marker+"question")
			if outcome == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrAgentFinalizationChanged)
			}
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices WHERE kind='child_question'`))
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0`))
		})
	}
}

func TestBatchResolveQuestionNoopDoesNotAdmitAndMatchedMutationDoes(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationAncestorEnv(t)
	env.conn.SetMaxOpenConns(1)
	ctx, authorities := finalizationIngressQuestionOwners(t, env)
	closed, err := env.store.BatchResolveChildQuestions(ctx, "child", "child-root.1", "grandchild", 1, 1)
	require.NoError(t, err)
	require.Zero(t, closed)
	for _, authority := range authorities {
		state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, snapshotErr)
		require.Zero(t, state.Revision, "missing question marker must not create synthetic work")
	}
	marker := "[batch-question node=child-root.1 token=1 sequence=1 depth=1 child=grandchild]\n"
	require.NoError(t, env.store.BatchPublishQuestion(ctx, "child", "child-root.1", 1, marker, marker+"question"))
	_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER resolve_requires_all_ancestor_tickets BEFORE UPDATE OF delivery ON session_notices
		WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>2
		BEGIN SELECT RAISE(ABORT, 'resolve missing ancestor tickets'); END`)
	require.NoError(t, err)
	closed, err = env.store.BatchResolveChildQuestions(ctx, "child", "child-root.1", "grandchild", 1, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, closed)
	for _, authority := range authorities {
		state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, snapshotErr)
		require.EqualValues(t, 4, state.Revision, "publish and resolve each acquire/release one ticket")
		require.Zero(t, state.OpenTickets)
	}
}

func TestSessionNoticePullAdmitsOnlyProductiveDelivery(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationAncestorEnv(t)
	env.conn.SetMaxOpenConns(1)
	store := NewSessionNoticeStore(env.conn)
	_, err := store.InsertSessionNoticeReturningID(env.ctx, "grandchild", NoticeKindWakeOnly, "completion", true, "")
	require.NoError(t, err)
	ctx, authorities := finalizationIngressAncestors(t, env)
	_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER notice_pull_requires_ancestor_tickets BEFORE INSERT ON messages
		WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>3
		BEGIN SELECT RAISE(ABORT, 'notice pull missing ancestor tickets'); END`)
	require.NoError(t, err)
	messages := message.NewService(db.New(env.conn))
	build := func(SessionNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "notice"}}}
	}
	pulled, err := store.PullSessionNotices(ctx, messages, "grandchild", build)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	for _, authority := range authorities {
		state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, snapshotErr)
		require.EqualValues(t, 2, state.Revision)
		require.Zero(t, state.OpenTickets)
	}
	before := commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`)
	pulled, err = store.PullSessionNotices(ctx, messages, "grandchild", build)
	require.NoError(t, err)
	require.Empty(t, pulled)
	require.Equal(t, before, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`), "empty pull must not issue a ticket")
}

func TestBatchPullAdmitsEveryManagedAncestorBeforeEffects(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationAncestorEnv(t)
	env.conn.SetMaxOpenConns(1)
	p := batchStoreTestReservation()
	p.Owner, p.ToolCallID, p.RunnerID = "child", "child-call", "runner"
	p.Root.Children = p.Root.Children[:1]
	snapshot, err := env.store.Reserve(env.ctx, p)
	require.NoError(t, err)
	projection := BatchRootProjection{Owner: "child", Root: snapshot.Snapshot.Root.RootID}
	rootUpdate := batchStoreTestUpdate(snapshot.Snapshot.Root.RootID, "completed")
	rootUpdate.OutcomeSummary = snapshot.Snapshot.Root.RootID
	projection.Updates = append(projection.Updates, rootUpdate)
	projection.Notices = append(projection.Notices, batchStoreTestNotice(snapshot.Snapshot.Root.RootID))
	require.NoError(t, env.store.Project(env.ctx, BatchProjection{Host: "host", RunnerID: "runner", Sequence: 2, Roots: []BatchRootProjection{projection}}))
	messages := message.NewService(db.New(env.conn))
	_, err = env.store.BatchAnnounceStarted(env.ctx, messages, "child", snapshot.Snapshot.Root.RootID, batchOutboxText("started"))
	require.NoError(t, err)
	ctx, authorities := finalizationIngressAncestors(t, env)
	_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER batch_pull_requires_ancestor_tickets BEFORE UPDATE OF delivery ON batch_nodes
		WHEN NEW.delivery='done' AND (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>2
		BEGIN SELECT RAISE(ABORT, 'batch pull missing ancestor tickets'); END`)
	require.NoError(t, err)
	pulled, err := env.store.BatchPullNotices(ctx, messages, "child", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	for _, authority := range authorities {
		state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, snapshotErr)
		if authority.identity.SessionID == "grandchild" {
			require.Zero(t, state.Revision, "unaffected descendant is not ticketed")
		} else {
			require.EqualValues(t, 2, state.Revision)
		}
		require.Zero(t, state.OpenTickets)
	}
}

func TestBatchToolResultAdmissionAndUnboundLegacy(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationWriterEnv(t)
	env.conn.SetMaxOpenConns(1)
	snapshot := pdcReserveOneTask(t, env.store, env.ctx)
	pdcSettleOneTaskRoot(t, env.store, env.ctx, snapshot)
	authority := finalizationCommitAuthority(t, finalizationCommitService(env), "owner", "tool-result", 1)
	ctx, err := WithAgentFinalizationContext(env.ctx, authority)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER tool_result_requires_ticket BEFORE INSERT ON messages
		WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0)<>1
		BEGIN SELECT RAISE(ABORT, 'tool result missing ticket'); END`)
	require.NoError(t, err)
	messages := message.NewService(db.New(env.conn))
	_, consumed, err := env.store.BatchConsumeToolResult(ctx, messages, "owner", pdcConsumeIdentity(snapshot.Root.RootID), pdcToolResultParams("result"))
	require.NoError(t, err)
	require.True(t, consumed)
	state, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, state.Revision)
	require.Zero(t, state.OpenTickets)

	legacy, legacyConn, legacyCtx := batchStoreTestDB(t)
	legacySnapshot := pdcReserveOneTask(t, legacy, legacyCtx)
	pdcSettleOneTaskRoot(t, legacy, legacyCtx, legacySnapshot)
	legacyMessages := message.NewService(db.New(legacy.writer))
	_, consumed, err = legacy.BatchConsumeToolResult(legacyCtx, legacyMessages, "owner", pdcConsumeIdentity(legacySnapshot.Root.RootID), pdcToolResultParams("legacy"))
	require.NoError(t, err)
	require.True(t, consumed)
	require.Zero(t, commitHookCount(t, legacyConn, `SELECT count(*) FROM session_agent_finalizations`))
	legacyConn.Close()
}

func TestBatchQuestionUnboundCallersKeepLegacyErrorShape(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationAncestorEnv(t)
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET token=1 WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	marker := "[batch-question node=child-root.1 token=1 sequence=1 depth=1 child=grandchild]\n"

	err = env.store.BatchPublishQuestion(env.ctx, "child", "missing-node", 1, marker, marker+"q")
	require.ErrorContains(t, err, "batch store: question node lookup")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NotErrorIs(t, err, ErrAgentFinalizationChanged)
	err = env.store.BatchPublishQuestion(env.ctx, "owner", "child-root.1", 1, marker, marker+"q")
	require.ErrorIs(t, err, sql.ErrNoRows, "a node of another owner is not found")
	require.NotErrorIs(t, err, ErrAgentFinalizationChanged)
	err = env.store.BatchPublishQuestion(env.ctx, "child", "child-root.1", 2, marker, marker+"q")
	require.EqualError(t, err, "batch store: question node incarnation is not live")

	require.NoError(t, env.store.BatchPublishQuestion(env.ctx, "child", "child-root.1", 1, marker, marker+"q"))
	require.NoError(t, env.store.BatchPublishQuestion(env.ctx, "child", "child-root.1", 1, marker, marker+"again"))
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_notices WHERE kind='child_question'`))

	closed, err := env.store.BatchResolveChildQuestions(env.ctx, "child", "child-root.1", "other-child", 1, 1)
	require.NoError(t, err)
	require.Zero(t, closed, "unknown identity is a no-op")
	closed, err = env.store.BatchResolveChildQuestions(env.ctx, "child", "child-root.1", "grandchild", 1, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, closed)
	closed, err = env.store.BatchResolveChildQuestions(env.ctx, "child", "child-root.1", "grandchild", 1, 1)
	require.NoError(t, err)
	require.Zero(t, closed)
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`))
}

func TestBatchQuestionOrdinaryRefusalsAreNotFinalizationFailures(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationAncestorEnv(t)
	ctx, authorities := finalizationIngressQuestionOwners(t, env)
	marker := "[batch-question node=child-root.1 token=7 sequence=1 depth=1 child=grandchild]\n"
	err := env.store.BatchPublishQuestion(ctx, "child", "child-root.1", 7, marker, marker+"q")
	require.EqualError(t, err, "batch store: question node incarnation is not live")
	require.NotErrorIs(t, err, ErrAgentFinalizationChanged)
	err = env.store.BatchPublishQuestion(ctx, "child", "missing-node", 1, marker, marker+"q")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NotErrorIs(t, err, ErrAgentFinalizationChanged)
	for _, authority := range authorities {
		state, snapshotErr := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, snapshotErr)
		require.Zero(t, state.Revision, "an ordinary refusal must not mint a revision")
	}
}

func TestBatchReactionLegacyBehaviorIsUnchanged(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := finalizationWriterEnv(t)
	snapshot := batchOutboxReserve(t, env.store, env.ctx, 1)
	messages := message.NewService(db.New(env.conn))
	batchOutboxSettle(t, env.store, env.ctx, snapshot, 2, snapshot.Root.RootID)
	batchOutboxAck(t, env.store, env.ctx, messages, snapshot.Root.RootID)
	_, err := env.store.BatchPullNotices(env.ctx, messages, "owner", batchOutboxParams)
	require.NoError(t, err)
	empty, err := messages.Create(env.ctx, "owner", message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)
	require.NoError(t, env.store.BatchReactionMarkReactedWithMessageUpdate(env.ctx, messages, "owner", empty))
	require.NotZero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE reacted=1`), "empty content still marks reaction as before")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalizations`))
}
