package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentDepthDurableAndLegacyLineage(t *testing.T) {
	conn, q := newTestDB(t)
	svc := NewService(q, conn)
	store := svc.(AgentDepthStore)
	root, err := svc.Create(context.Background(), "root")
	require.NoError(t, err)
	child, err := store.CreateTaskSessionWithAgentDepth(context.Background(), "child", root.ID, "child", 1)
	require.NoError(t, err)
	grandchild, err := store.CreateTaskSessionWithAgentDepth(context.Background(), "grandchild", child.ID, "grandchild", 0)
	require.NoError(t, err)
	remaining, err := store.ResolveAgentDepth(context.Background(), root.ID, 2)
	require.NoError(t, err)
	require.Equal(t, 2, remaining)
	remaining, err = store.ResolveAgentDepth(context.Background(), child.ID, 2)
	require.NoError(t, err)
	require.Equal(t, 1, remaining)
	remaining, err = store.ResolveAgentDepth(context.Background(), grandchild.ID, 2)
	require.NoError(t, err)
	require.Zero(t, remaining)
	require.NoError(t, store.LowerAgentDepth(context.Background(), root.ID, 0))
	remaining, err = store.ResolveAgentDepth(context.Background(), root.ID, 2)
	require.NoError(t, err)
	require.Zero(t, remaining, "persisted roots cannot be raised by later runs")
	remaining, err = store.ResolveAgentDepth(context.Background(), child.ID, 2)
	require.NoError(t, err)
	require.Zero(t, remaining, "a lowered root cap propagates through descendants")
	require.NoError(t, store.LowerAgentDepth(context.Background(), child.ID, 1))
	remaining, err = store.ResolveAgentDepth(context.Background(), child.ID, 32)
	require.NoError(t, err)
	require.Zero(t, remaining, "a saved child depth cannot be raised")
	_, err = store.ResolveAgentDepth(context.Background(), "missing-lineage", 2)
	require.ErrorContains(t, err, "unavailable")
}

func TestAgentDepthCurrentCapOnlyNarrowsLegacyLineage(t *testing.T) {
	conn, q := newTestDB(t)
	svc := NewService(q, conn)
	store := svc.(AgentDepthStore)
	root, err := svc.Create(context.Background(), "root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(context.Background(), "legacy-current-cap", root.ID, "child")
	require.NoError(t, err)
	remaining, err := store.ResolveAgentDepth(context.Background(), child.ID, 0)
	require.NoError(t, err)
	require.Zero(t, remaining)
}

func TestAgentDepthLegacyMultiEdgeUsesConservativeDefault(t *testing.T) {
	conn, q := newTestDB(t)
	svc := NewService(q, conn)
	root, err := svc.Create(context.Background(), "root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(context.Background(), "legacy-child-1", root.ID, "child")
	require.NoError(t, err)
	grandchild, err := svc.CreateTaskSession(context.Background(), "legacy-child-2", child.ID, "grandchild")
	require.NoError(t, err)
	store := svc.(AgentDepthStore)
	remaining, err := store.ResolveAgentDepth(context.Background(), grandchild.ID, 32)
	require.NoError(t, err)
	require.Zero(t, remaining, "two legacy edges exhaust the conservative default of two")
}

func TestAgentDepthLegacyLineageAndCycleFailClosed(t *testing.T) {
	conn, q := newTestDB(t)
	svc := NewService(q, conn)
	store := svc.(AgentDepthStore)
	root, err := svc.Create(context.Background(), "root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(context.Background(), "legacy-child", root.ID, "child")
	require.NoError(t, err)
	remaining, err := store.ResolveAgentDepth(context.Background(), child.ID, 2)
	require.NoError(t, err)
	require.Equal(t, 1, remaining)
	_, err = conn.ExecContext(context.Background(), "UPDATE sessions SET parent_session_id = ? WHERE id = ?", child.ID, root.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), "UPDATE sessions SET parent_session_id = ? WHERE id = ?", root.ID, child.ID)
	require.NoError(t, err)
	_, err = store.ResolveAgentDepth(context.Background(), child.ID, 2)
	require.ErrorContains(t, err, "cycle")
}
