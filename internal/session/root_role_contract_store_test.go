package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootRoleContractReplacesOnlyForNewLogicalCall(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "root role contract")
	require.NoError(t, err)
	store := svc.(SessionRootRoleContractStore)
	first := &RoleContract{Version: RoleContractVersion, Name: "researcher", Model: ModelCfg{Provider: "p", Model: "m"}, Tools: func() *[]string { v := []string{"view"}; return &v }()}
	require.NoError(t, store.BindRootRoleContract(t.Context(), root.ID, "logical-1", first))
	require.NoError(t, store.BindRootRoleContract(t.Context(), root.ID, "logical-1", first.Clone()))
	widened := first.Clone()
	*widened.Tools = []string{"bash"}
	require.ErrorIs(t, store.BindRootRoleContract(t.Context(), root.ID, "logical-1", widened), ErrSessionRoleContractMismatch)

	second := &RoleContract{Version: RoleContractVersion, Name: "researcher", Model: ModelCfg{Provider: "p", Model: "new-model"}, Inherited: [][]string{{"view", "grep"}}}
	require.NoError(t, store.BindRootRoleContract(t.Context(), root.ID, "logical-2", second))
	restarted := NewService(queries, conn).(SessionRootRoleContractStore)
	logicalID, got, found, err := restarted.GetRootRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "logical-2", logicalID)
	require.True(t, second.Equal(got))

	require.NoError(t, restarted.BindRootRoleContract(t.Context(), root.ID, "logical-3", nil))
	logicalID, got, found, err = restarted.GetRootRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "logical-3", logicalID)
	require.Nil(t, got)
	require.NoError(t, restarted.BindRootRoleContract(t.Context(), root.ID, "logical-3", nil))
	require.ErrorIs(t, restarted.BindRootRoleContract(t.Context(), root.ID, "logical-3", first), ErrSessionRoleContractMismatch)
	logicalID, got, found, err = restarted.GetRootRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "logical-3", logicalID)
	require.Nil(t, got)
}

func TestRootRoleContractReadRejectsEmptyPersistedLogicalCallID(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "empty root logical id")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO session_root_role_contracts(session_id, logical_call_id, contract_json) VALUES (?, '', NULL)`, root.ID)
	require.NoError(t, err)
	_, _, found, err := svc.(SessionRootRoleContractStore).GetRootRoleContract(t.Context(), root.ID)
	require.Error(t, err)
	require.True(t, found)
}

func TestRootRoleContractRejectsInvalidAndCorruptBindings(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "invalid root role contract")
	require.NoError(t, err)
	store := svc.(SessionRootRoleContractStore)
	require.Error(t, store.BindRootRoleContract(t.Context(), root.ID, "", nil))
	_, err = conn.ExecContext(t.Context(), `INSERT INTO session_root_role_contracts(session_id, logical_call_id, contract_json) VALUES (?, ?, ?)`, root.ID, "logical-1", `{"version":1,"name":"researcher","model":{"provider":"p","model":"m"},"tools":null}`)
	require.NoError(t, err)
	_, _, found, err := store.GetRootRoleContract(t.Context(), root.ID)
	require.Error(t, err)
	require.True(t, found)
	require.Error(t, store.BindRootRoleContract(t.Context(), root.ID, "logical-1", nil))
}
