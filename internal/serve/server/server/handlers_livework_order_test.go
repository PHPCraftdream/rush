package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildSessionLiveWork_RootOrdering(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "live-work ordering")
	require.NoError(t, err)

	for _, id := range []string{"tc-order-a", "tc-order-b"} {
		seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: id, taskKind: "bash", state: "running"})
	}
	setUpdatedAt := func(id string, updatedAt int64) {
		t.Helper()
		_, err := a.DB().ExecContext(ctx, "UPDATE batch_nodes SET updated_at = ? WHERE tool_call_id = ?", updatedAt, id)
		require.NoError(t, err)
	}

	setUpdatedAt("tc-order-a", 1_700_000_000)
	setUpdatedAt("tc-order-b", 1_700_000_001)
	snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.Len(t, snap.Commands, 2)
	require.Equal(t, "tc-order-b", snap.Commands[0].ToolCallID, "newer roots sort first")
	require.Equal(t, "tc-order-a", snap.Commands[1].ToolCallID)

	setUpdatedAt("tc-order-a", 1_700_000_001)
	snap, err = buildSessionLiveWork(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.Len(t, snap.Commands, 2)
	require.Equal(t, "tc-order-a", snap.Commands[0].ToolCallID, "equal timestamps sort by tool_call_id ascending")
	require.Equal(t, "tc-order-b", snap.Commands[1].ToolCallID)
}
