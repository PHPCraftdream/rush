package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchQuestionPublisherWakesOwnerAfterDurablePublish(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_GLOBAL_DIR", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	env := newBatchInventoryEnv(t, "agent")
	nodeID, token := questionTestNode(t, env, "q-wake")
	var wakes []string
	publisher := batchCoordinatorQuestionPublisher{
		store: env.store,
		wake: func(_ context.Context, owner string) {
			var count int
			require.NoError(t, env.conn.QueryRowContext(env.ctx,
				`SELECT COUNT(*) FROM session_notices WHERE owner = ? AND kind = ?`,
				owner, session.NoticeKindChildQuestion).Scan(&count))
			require.Equal(t, 1, count)
			wakes = append(wakes, owner)
		},
	}
	err := publisher.PublishChildQuestion(env.ctx, batchChildQuestionNotice{
		Owner: env.sessionID, ChildSessionID: "child-wake", NodeID: nodeID,
		Token: uint64(token), Depth: 1, Sequence: 1, Question: "which port?",
	})
	require.NoError(t, err)
	require.Equal(t, []string{env.sessionID}, wakes)
}
