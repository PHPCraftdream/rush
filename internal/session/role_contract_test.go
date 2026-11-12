package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoleContractCloneAndEqualityPreserveToolPresenceAndAliases(t *testing.T) {
	tools := []string{"view"}
	inherited := [][]string{{"view", "grep"}}
	temperature := 0.5
	nested := map[string]any{"nested": []any{"original"}}
	original := &RoleContract{
		Version: RoleContractVersion,
		Name:    "Rese archΩ",
		Model: ModelCfg{
			Provider:        "provider",
			Model:           "model",
			Temperature:     &temperature,
			ProviderOptions: map[string]any{"options": nested},
		},
		Tools:     &tools,
		Inherited: inherited,
	}
	copy := original.Clone()
	require.True(t, original.Equal(copy))
	(*copy.Tools)[0] = "grep"
	copy.Inherited[0][0] = "bash"
	*copy.Model.Temperature = 0.9
	copy.Model.ProviderOptions["options"].(map[string]any)["nested"].([]any)[0] = "changed"
	require.Equal(t, "view", tools[0])
	require.Equal(t, "view", inherited[0][0])
	require.Equal(t, 0.5, temperature)
	require.Equal(t, "original", nested["nested"].([]any)[0])
	require.False(t, original.Equal(copy))

	absent := &RoleContract{Version: RoleContractVersion}
	empty := &RoleContract{Version: RoleContractVersion, Tools: func() *[]string { v := []string{}; return &v }()}
	require.False(t, absent.Equal(empty))
}

func TestRoleContractToolsPresenceSurvivesPersistence(t *testing.T) {
	for name, tools := range map[string]*[]string{
		"omitted": nil,
		"empty":   func() *[]string { value := []string{}; return &value }(),
		"set":     func() *[]string { value := []string{"view"}; return &value }(),
	} {
		t.Run(name, func(t *testing.T) {
			conn, queries := newTestDB(t)
			svc := NewService(queries, conn)
			root, err := svc.Create(t.Context(), "tools presence")
			require.NoError(t, err)
			contract := &RoleContract{Version: RoleContractVersion, Name: "researcher", Model: ModelCfg{Provider: "p", Model: "m"}, Tools: tools}
			store := svc.(SessionRoleContractStore)
			require.NoError(t, store.BindSessionRoleContract(t.Context(), root.ID, contract))
			got, found, err := store.GetSessionRoleContract(t.Context(), root.ID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, tools == nil, got.Tools == nil)
			if tools != nil {
				require.NotNil(t, got.Tools)
				require.Equal(t, *tools, *got.Tools)
			}
		})
	}
}

func TestRoleContractValidationRejectsMalformedSnapshots(t *testing.T) {
	valid := `{"version":1,"name":"researcher","model":{"provider":"p","model":"m"}}`
	for _, raw := range []string{
		`{"version":2,"name":"researcher","model":{"provider":"p","model":"m"}}`,
		`{"version":1,"name":"researcher","model":{"provider":"p","model":"m"},"unknown":true}`,
		`{"version":1,"name":"researcher","model":{"provider":"p","model":"m"},"tools":null}`,
		`{"version":1,"name":"researcher","model":{"provider":"p","model":"m"},"inherited":[null]}`,
		`{"version":1,"name":"researcher","model":{}}`,
		`{"version":1,"name":"researcher","model":{"provider":"p","model":"m","unknown":true}}`,
		`{"version":1,"name":"Worker","model":{"provider":"p","model":"m"}}`,
	} {
		var contract RoleContract
		require.Error(t, contract.UnmarshalJSON([]byte(raw)), raw)
	}
	var contract RoleContract
	require.NoError(t, contract.UnmarshalJSON([]byte(valid)))
	require.NoError(t, contract.Validate())
}

func TestSessionRoleContractBindingIsImmutableAndDurable(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "role contract")
	require.NoError(t, err)
	store := svc.(SessionRoleContractStore)
	contract := &RoleContract{
		Version:   RoleContractVersion,
		Name:      "ResearchΩ team",
		Model:     ModelCfg{Provider: "provider", Model: "model", ReasoningEffort: "high"},
		Inherited: [][]string{{"view", "grep"}},
	}
	require.NoError(t, store.BindSessionRoleContract(t.Context(), root.ID, contract))
	require.NoError(t, store.BindSessionRoleContract(t.Context(), root.ID, contract.Clone()))
	changed := contract.Clone()
	changed.Inherited[0][0] = "bash"
	require.ErrorIs(t, store.BindSessionRoleContract(t.Context(), root.ID, changed), ErrSessionRoleContractMismatch)
	legacyStore := svc.(AgentModelRoleStore)
	role, found, err := legacyStore.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, contract.Name, role)
	require.NoError(t, legacyStore.BindAgentModelRole(t.Context(), root.ID, contract.Name))
	require.ErrorIs(t, legacyStore.BindAgentModelRole(t.Context(), root.ID, "other-role"), ErrAgentModelRoleMismatch)

	restarted := NewService(queries, conn).(SessionRoleContractStore)
	stored, found, err := restarted.GetSessionRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, contract.Equal(stored))
	stored.Inherited[0][0] = "external mutation"
	storedAgain, found, err := restarted.GetSessionRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "view", storedAgain.Inherited[0][0])
}

func TestSessionRoleContractBindingAllowsConcurrentSameSnapshot(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "concurrent role contract")
	require.NoError(t, err)
	contract := &RoleContract{Version: RoleContractVersion, Name: "researcher", Model: ModelCfg{Provider: "p", Model: "m"}}
	store := svc.(SessionRoleContractStore)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- store.BindSessionRoleContract(t.Context(), root.ID, contract)
		}()
	}
	close(start)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
}

func TestRoleContractCanRepresentEmptyInheritedOnlySelection(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "inherited role contract")
	require.NoError(t, err)
	contract := &RoleContract{Version: RoleContractVersion, Inherited: [][]string{{"view"}}}
	encoded, err := json.Marshal(contract)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"model"`)
	store := svc.(SessionRoleContractStore)
	require.NoError(t, store.BindSessionRoleContract(t.Context(), root.ID, contract))
	role, found, err := svc.(AgentModelRoleStore).GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, role)
	got, found, err := store.GetSessionRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, contract.Equal(got))
}

func TestRoleContractAllowsExactBuiltinButRejectsCaseAlias(t *testing.T) {
	builtin := &RoleContract{Version: RoleContractVersion, Name: "worker", Model: ModelCfg{Provider: "p", Model: "m"}, Inherited: [][]string{{"view"}}}
	require.NoError(t, builtin.Validate())
	alias := builtin.Clone()
	alias.Name = "Worker"
	require.Error(t, alias.Validate())
}

func TestRoleContractBindingAllowsMatchingLegacyRoleAndPreservesRow(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "compatible legacy role")
	require.NoError(t, err)
	legacy := svc.(AgentModelRoleStore)
	require.NoError(t, legacy.BindAgentModelRole(t.Context(), root.ID, "worker"))
	contract := &RoleContract{Version: RoleContractVersion, Name: "worker", Model: ModelCfg{Provider: "p", Model: "m"}, Inherited: [][]string{{"view"}}}
	require.NoError(t, svc.(SessionRoleContractStore).BindSessionRoleContract(t.Context(), root.ID, contract))
	role, found, err := legacy.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "worker", role)
	var legacyRole string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT requested_role FROM session_agent_model_role WHERE session_id=?`, root.ID).Scan(&legacyRole))
	require.Equal(t, "worker", legacyRole)
}

func TestSessionRoleContractBindingRejectsMismatchedLegacyNameWithoutChangingLegacyRow(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "mismatched legacy binding")
	require.NoError(t, err)
	legacy := svc.(AgentModelRoleStore)
	require.NoError(t, legacy.BindAgentModelRole(t.Context(), root.ID, "worker"))
	contract := &RoleContract{Version: RoleContractVersion, Name: "researcher", Model: ModelCfg{Provider: "p", Model: "m"}}
	require.ErrorIs(t, svc.(SessionRoleContractStore).BindSessionRoleContract(t.Context(), root.ID, contract), ErrSessionRoleContractMismatch)
	role, found, err := legacy.GetAgentModelRole(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "worker", role)
	var contracts int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM session_role_contracts WHERE session_id=?`, root.ID).Scan(&contracts))
	require.Zero(t, contracts)
}

func TestSessionRoleContractReadRejectsCorruptSnapshot(t *testing.T) {
	conn, queries := newTestDB(t)
	svc := NewService(queries, conn)
	root, err := svc.Create(t.Context(), "invalid stored contract")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO session_role_contracts(session_id, contract_json) VALUES (?, ?)`, root.ID, `{"version":1,"name":"researcher","model":{"provider":"p","model":"m"},"tools":null}`)
	require.NoError(t, err)
	_, found, err := svc.(SessionRoleContractStore).GetSessionRoleContract(t.Context(), root.ID)
	require.Error(t, err)
	require.True(t, found)
}
