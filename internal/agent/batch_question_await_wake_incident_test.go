package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestChildQuestionDebtBreaksAwaitSleepForAnyAndAll(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_GLOBAL_DIR", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	for _, mode := range []string{"any", "all"} {
		t.Run(mode, func(t *testing.T) {
			leaf := newGatedLeaf("view")
			env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
			releaseAllAt(t, leaf)
			coord := newCutoverCoordinator(t, env)
			cutoverBindFacts(t, coord, env)
			env.invokeDetached("question-sleep-"+mode, "view", leaf)
			callID := "question-sleep-" + mode
			leaf.waitStarted(t, callID)
			root := env.rootID(callID)
			if mode == "all" {
				armSleepAll(t, coord, env)
			} else {
				armAwaitAny(t, coord, env)
			}
			var nodeID string
			var token int64
			require.NoError(t, env.conn.QueryRowContext(env.ctx,
				`SELECT node_id, token FROM batch_nodes WHERE root_id = ?1 AND node_id != root_id AND state IN ('pending','starting','running','cancelling') ORDER BY ordinal LIMIT 1`,
				root).Scan(&nodeID, &token))
			require.Greater(t, token, int64(0))
			require.NoError(t, (batchCoordinatorQuestionPublisher{store: env.store}).PublishChildQuestion(env.ctx, batchChildQuestionNotice{
				Owner: env.sessionID, ChildSessionID: "question-worker-" + mode, NodeID: nodeID,
				Token: uint64(token), Depth: 1, Sequence: 1, Question: "which option?",
			}))

			debt := func(ctx context.Context) (session.BatchPendingDebt, error) {
				return env.store.BatchReactionPendingInclusiveDebtRows(ctx, env.sessionID)
			}
			live := func(ctx context.Context) session.BatchLiveResult {
				return env.live.BatchLiveForOwner(ctx, env.sessionID)
			}
			facts, err := ReadBatchArbiterFacts(env.ctx, BatchArbiterDeps{Live: live, Debt: debt})
			require.NoError(t, err)
			require.True(t, facts.Complete)
			require.True(t, facts.LiveRoots, "the worker is still nonterminal")
			require.True(t, facts.DebtPendingIncl)
			require.False(t, facts.CompletionOnly, "child_question is non-completion debt")
			if mode == "all" {
				require.False(t, facts.SleepHolds(true, BatchSleepBreakers{}), "question debt breaks the all-sleep")
				verdict := coord.drainPolicy(env.ctx, env.sessionID, false)
				require.Equal(t, drainAllow, verdict.kind, "owner wake is re-evaluated rather than deferred by all-sleep")
			} else {
				require.False(t, facts.SleepHolds(false, BatchSleepBreakers{}), "any-mode has no all-sleep hold")
				verdict := coord.drainPolicy(env.ctx, env.sessionID, false)
				require.Equal(t, drainAllow, verdict.kind, "owner wake re-evaluates the any-mode wait")
			}
			pulled, err := NewBatchCutoverNodeFacts(env.store, env.live, env.conn).PullPendingSessionNotices(
				env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
			require.NoError(t, err)
			require.NotEmpty(t, pulled, "the production session-notice pull delivers the child question into history")
			require.Equal(t, session.NoticeKindChildQuestion, pulled[0].Message.NoticeKind)
			var questionCount int
			require.NoError(t, env.conn.QueryRowContext(env.ctx,
				`SELECT count(*) FROM messages WHERE session_id = ?1 AND notice_kind = ?2`, env.sessionID, session.NoticeKindChildQuestion).Scan(&questionCount))
			require.Equal(t, 1, questionCount)
		})
	}
}
