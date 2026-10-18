// D11-profiled pull-path guards: PullPendingSessionNotices must not touch
// batch_nodes (the live-scope read) unless a pending SUPERVISION row needs
// the scope answer, must still deliver non-supervision rows without it, and
// must keep the supervision void/deliver semantics identical.
package agent

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type batchPullCountingSource struct {
	session.BatchLiveSource
	q     *db.Queries
	calls int
}

func (s *batchPullCountingSource) BatchLiveNodesForOwners(ctx context.Context, owners []string) ([]db.BatchNode, error) {
	s.calls++
	return s.q.BatchLiveNodesForOwners(ctx, owners)
}

func newBatchPullEnv(t *testing.T) (*cutoverEnv, *nodeBatchCutoverFacts, *batchPullCountingSource) {
	t.Helper()
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	counting := &batchPullCountingSource{q: db.New(env.conn)}
	env.live.Source = counting
	facts := nodeBatchCutoverFacts{store: env.store, live: env.live, writer: env.conn}
	return env, &facts, counting
}

func TestBatchPullPendingNoticesSkipsLiveReadWhenEmpty(t *testing.T) {
	env, facts, counting := newBatchPullEnv(t)
	pulled, err := facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Empty(t, pulled)
	require.Zero(t, counting.calls, "an empty pull must not read batch_nodes")
}

func TestBatchPullPendingNoticesDeliversWithoutLiveRead(t *testing.T) {
	env, facts, counting := newBatchPullEnv(t)
	now := time.Now().Unix()
	_, err := env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindBGShellDone, Text: "bg done",
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	pulled, err := facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.Zero(t, counting.calls, "non-supervision delivery never needs the live scope")
}

func TestBatchPullSupervisionOpenScopeDelivers(t *testing.T) {
	env, facts, _ := newBatchPullEnv(t)
	batchPullReserveLiveRoot(t, env)
	now := time.Now().Unix()
	_, err := env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindSupervision, Text: "check-in", Wake: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	pulled, err := facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "an open live scope delivers the check-in")
	// The ON live read runs through the store's transactional read (see
	// liveResult), not through the injected Source spy — the DELIVERY itself
	// proves the scope was consulted and open (a closed scope voids the row
	// and pulled stays empty).
}

func TestBatchPullSupervisionClosedScopeVoids(t *testing.T) {
	env, facts, _ := newBatchPullEnv(t)
	now := time.Now().Unix()
	_, err := env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindSupervision, Text: "check-in", Wake: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	pulled, err := facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a closed scope voids the check-in")
	var delivery string
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery FROM session_notices WHERE owner = ?`, env.sessionID).Scan(&delivery))
	require.Equal(t, "void", delivery)
}

func batchPullReserveLiveRoot(t *testing.T, env *cutoverEnv) {
	t.Helper()
	_, err := env.store.Reserve(env.ctx, session.BatchReservation{
		Owner: env.sessionID, Host: "host", RunnerID: "runner", ToolCallID: "call-live",
		InputHash: "hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true},
			State: "pending", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
}
