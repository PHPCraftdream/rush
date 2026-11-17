package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentModelRoleBindingIsImmutableAndDurable(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "role binding")
	require.NoError(t, err)
	store := svc.(AgentModelRoleStore)
	_, found, err := store.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, store.BindAgentModelRole(t.Context(), root.ID, "smart"))
	require.NoError(t, store.BindAgentModelRole(t.Context(), root.ID, "smart"), "same-role retries are idempotent")
	require.ErrorIs(t, store.BindAgentModelRole(t.Context(), root.ID, "worker"), ErrAgentModelRoleMismatch)
	restarted := NewService(queries, conn).(AgentModelRoleStore)
	role, found, err := restarted.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "smart", role)
}

func TestLegacyAgentModelRoleCannotBePromotedOnResume(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "legacy role binding")
	require.NoError(t, err)
	store := svc.(AgentModelRoleStore)
	require.NoError(t, store.BindAgentModelRole(t.Context(), root.ID, ""))
	require.ErrorIs(t, store.BindAgentModelRole(t.Context(), root.ID, "reviewer"), ErrAgentModelRoleMismatch)
	role, found, err := store.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, role)
}

func TestAgentModelRoleStoreRejectsUnknownRoles(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "invalid role binding")
	require.NoError(t, err)
	store := svc.(AgentModelRoleStore)
	require.ErrorContains(t, store.BindAgentModelRole(t.Context(), root.ID, "unknown"), "agent_role_invalid")
	_, found, err := store.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.False(t, found, "invalid input cannot reserve the session's role")
}
