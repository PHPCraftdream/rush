package server

// R12-C terminal-root merge tests: the flat live-work panel must keep
// finished commands after the BatchHistoryRootsForOwner history read is
// merged with the live feed. Covered, per test:
//
//   - HistoryRootDedupedByRootID: a root that went terminal between the live
//     and history reads is served once, from the LIVE row (mutation caught:
//     dropping the liveRootIDs dedupe duplicates it or lets the stale
//     terminal state win).
//   - HistoryFailureDowngrades: a failed history read only downgrades
//     Complete — the live row is never dropped (mutation caught: failing
//     open on Complete or dropping live rows).
//   - BoundedPerOwner: the per-owner history read is capped to the
//     batchHistoryTerminalBound newest terminal roots (mutation caught:
//     removing the cap returns all 60).
//   - TerminalRootChildReachable: the BFS reads a terminal root's node rows,
//     so a delegation child's own terminal root stays visible (mutation
//     caught: dropping the BatchLevelNodes child frontier).
//   - TerminalCausesMapToStatusReason: batchRootItem maps each terminal
//     state's Status/Reason/FinishedAt from the node columns (mutation
//     caught: any change to that mapping).
//
// Cannot use t.Parallel(): the batch seams are package vars and other
// tests use t.Setenv.

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBuildSessionLiveWorkFromBatch_HistoryRootDedupedByRootID(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	_, err := a.Sessions.Create(ctx, "live work dedupe")
	require.NoError(t, err)

	live := batchNode("r1", "r1", "s1", "running", "tc-a", 0)
	live.UpdatedAt = 100
	terminal := live
	terminal.State = "completed"
	src := &fakeBatchSource{
		rows:           []db.BatchNode{live},
		historyByOwner: map[string][]db.BatchNode{"s1": {terminal}},
	}
	withBatchSeams(t, src, probeAlive, nil)

	snap, err := buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), "s1")
	require.NoError(t, err)
	require.Len(t, snap.Commands, 1)
	require.Equal(t, "tc-a", snap.Commands[0].ToolCallID)
	require.Equal(t, "running", snap.Commands[0].Status)
	require.NotNil(t, snap.Complete)
	require.True(t, *snap.Complete)
}

func TestBuildSessionLiveWorkFromBatch_HistoryFailureDowngrades(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	_, err := a.Sessions.Create(ctx, "live work history failure")
	require.NoError(t, err)

	live := batchNode("r1", "r1", "s1", "running", "tc-a", 0)
	live.UpdatedAt = 100
	src := &fakeBatchSource{rows: []db.BatchNode{live}, failHistory: true}
	withBatchSeams(t, src, probeAlive, nil)

	snap, err := buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), "s1")
	require.NoError(t, err)
	require.Len(t, snap.Commands, 1)
	require.Equal(t, "running", snap.Commands[0].Status)
	require.NotNil(t, snap.Complete)
	require.False(t, *snap.Complete)
}

func TestCollectBatchHistoryTerminalRoots_BoundedPerOwner(t *testing.T) {
	ctx := t.Context()
	history := make([]db.BatchNode, 0, 60)
	for i := 0; i < 60; i++ {
		r := batchNode(fmt.Sprintf("r%02d", i), fmt.Sprintf("r%02d", i), "s0", "completed", fmt.Sprintf("tc-%02d", i), int64(i))
		r.UpdatedAt = int64(i)
		history = append(history, r)
	}
	src := &fakeBatchSource{
		historyByOwner: map[string][]db.BatchNode{"s0": history},
		levels:         map[string][]db.BatchNode{},
	}

	metas, ok := collectBatchHistoryTerminalRoots(ctx, src, []string{"s0"}, nil)
	require.True(t, ok)
	require.Len(t, metas, liveWorkTerminalLimit)
	kept := map[string]bool{}
	for _, m := range metas {
		require.Equal(t, m.root.RootID, m.root.NodeID)
		kept[m.root.RootID] = true
	}
	for i := 59; i >= 10; i-- {
		require.True(t, kept[fmt.Sprintf("r%02d", i)], "r%02d must be kept", i)
	}
	for i := 0; i < 10; i++ {
		require.False(t, kept[fmt.Sprintf("r%02d", i)], "r%02d must be cut", i)
	}
}

func TestBuildSessionLiveWorkFromBatch_TerminalRootChildReachable(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	_, err := a.Sessions.Create(ctx, "terminal root child")
	require.NoError(t, err)

	parent := batchNode("r1", "r1", "s0", "completed", "tc-parent", 0)
	parent.UpdatedAt = 200
	childNode := batchNode("r1-c", "r1", "s0", "completed", "tc-child", 1)
	childNode.ChildSessionID = sql.NullString{String: "s1", Valid: true}
	childRoot := batchNode("r2", "r2", "s1", "failed", "tc-childroot", 0)
	childRoot.UpdatedAt = 201
	childRoot.Cause = "stop_on_fail"
	src := &fakeBatchSource{
		historyByOwner: map[string][]db.BatchNode{
			"s0": {parent},
			"s1": {childRoot},
		},
		levels: map[string][]db.BatchNode{
			"r1": {childNode},
		},
	}
	withBatchSeams(t, src, probeAlive, nil)

	snap, err := buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), "s0")
	require.NoError(t, err)
	toolCalls := map[string]bool{}
	for _, item := range snap.Commands {
		toolCalls[item.ToolCallID] = true
	}
	require.True(t, toolCalls["tc-parent"])
	require.True(t, toolCalls["tc-childroot"])
	require.NotNil(t, snap.Complete)
	require.True(t, *snap.Complete)
}

func TestBuildSessionLiveWorkFromBatch_LiveChildBehindTerminalParent(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	_, err := a.Sessions.Create(ctx, "live child behind terminal parent")
	require.NoError(t, err)

	parent := batchNode("r1", "r1", "s0", "completed", "tc-parent", 0)
	parent.UpdatedAt = 200
	childNode := batchNode("r1-c", "r1", "s0", "completed", "tc-child", 1)
	childNode.ChildSessionID = sql.NullString{String: "s1", Valid: true}
	liveRoot := batchNode("r2", "r2", "s1", "running", "tc-live", 0)
	liveRoot.UpdatedAt = 201
	src := &fakeBatchSource{
		historyByOwner: map[string][]db.BatchNode{
			"s0": {parent},
			"s1": {liveRoot},
		},
		levels: map[string][]db.BatchNode{"r1": {childNode}},
	}
	withBatchSeams(t, src, probeAlive, nil)

	snap, err := buildSessionLiveWorkFromBatch(ctx, a, newTitleCache(), "s0")
	require.NoError(t, err)
	status := map[string]string{}
	for _, item := range snap.Commands {
		status[item.ToolCallID] = item.Status
	}
	require.Equal(t, "completed", status["tc-parent"])
	require.Equal(t, "running", status["tc-live"])
	require.NotNil(t, snap.Complete)
	require.True(t, *snap.Complete)
}

func TestCollectBatchHistoryTerminalRoots_DepthOverflowDowngrades(t *testing.T) {
	ctx := t.Context()
	history := map[string][]db.BatchNode{}
	levels := map[string][]db.BatchNode{}
	// A chain longer than batchHistoryDepth: each session's terminal root
	// names the next session as its delegation child.
	for i := 0; i <= batchHistoryDepth+2; i++ {
		owner := fmt.Sprintf("s%02d", i)
		root := fmt.Sprintf("r%02d", i)
		history[owner] = []db.BatchNode{batchNode(root, root, owner, "completed", "tc-"+root, 0)}
		child := batchNode(root+"-c", root, owner, "completed", "tc-"+root+"-c", 1)
		child.ChildSessionID = sql.NullString{String: fmt.Sprintf("s%02d", i+1), Valid: true}
		levels[root] = []db.BatchNode{child}
	}
	src := &fakeBatchSource{historyByOwner: history, levels: levels}

	metas, ok := collectBatchHistoryTerminalRoots(ctx, src, []string{"s00"}, nil)
	require.False(t, ok, "an unvisited frontier at the depth cap must downgrade")
	require.NotEmpty(t, metas)
}

func TestBatchRootItem_TerminalCausesMapToStatusReason(t *testing.T) {
	cases := []struct {
		state string
		cause string
	}{
		{"completed", ""},
		{"failed", "stop_on_fail"},
		{"timed_out", "timeout"},
		{"cancelled", "stop"},
		{"interrupted", ""},
	}
	for _, tc := range cases {
		node := batchNode("r1", "r1", "s0", tc.state, "tc-a", 0)
		node.Cause = tc.cause
		node.UpdatedAt = 500
		m := batchRootMeta{root: node}
		item := batchRootItem(m, map[liveWorkTitleKey]string{}, map[string]session.ChildQuestion{})
		require.Equal(t, tc.state, item.Status, tc.state)
		require.Equal(t, tc.cause, item.Reason, tc.state)
		require.NotZero(t, item.FinishedAt, tc.state)
		require.Equal(t, "tc-a", item.ToolCallID, tc.state)
	}
}
