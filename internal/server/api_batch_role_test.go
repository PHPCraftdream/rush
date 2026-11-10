package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAPITaskRequestedRoleReachesDurableChildContract(t *testing.T) {
	for _, role := range []string{"smart", "fast", ""} {
		name := role
		if name == "" {
			name = "omitted legacy default"
		}
		t.Run(name, func(t *testing.T) {
			f := newBatchFixture(t, batchTextProvider("role result"))
			token := f.token(t, "role", []string{"read", "run"}, "ask", config.APITokenBinding{})
			body := `{"prompt":"p"}`
			if role != "" {
				body = fmt.Sprintf(`{"prompt":"p","role":%q}`, role)
			}
			accepted := f.do(t, token, http.MethodPost, "/tasks", body).accepted(t)
			result := f.waitBatchDone(t, token, accepted.RootID)
			require.Equal(t, "completed", result.State)
			store, supported := f.app.Sessions.(session.AgentModelRoleStore)
			require.True(t, supported)
			foundChild := false
			for _, node := range result.Nodes {
				if node.Kind != "task" || node.ChildSessionID == "" {
					continue
				}
				saved, found, err := store.GetAgentModelRole(t.Context(), node.ChildSessionID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, role, saved)
				foundChild = true
			}
			require.True(t, foundChild, "explicit and omitted API role choices must reach the actual child, not only the request schema")
		})
	}
}

func TestAPITaskCustomRolePersistsExactProfileAndToolCapOnChild(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("custom role result"))
	tools := []string{"view", "grep"}
	require.NoError(t, f.cfg.SetCustomRole(config.ScopeWorkspace, "Research Analyst", config.CustomRoleUpdate{
		Model: config.SelectedModel{Provider: "openaicompat", Model: "probe"},
		Tools: &tools,
	}))
	token := f.token(t, "custom-role", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", `{"prompt":"p","role":"Research Analyst"}`).accepted(t)
	result := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "completed", result.State)

	store, supported := f.app.Sessions.(session.SessionRoleContractStore)
	require.True(t, supported)
	foundChild := false
	for _, node := range result.Nodes {
		if node.Kind != "task" || node.ChildSessionID == "" {
			continue
		}
		contract, found, err := store.GetSessionRoleContract(t.Context(), node.ChildSessionID)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "Research Analyst", contract.Name)
		require.Equal(t, "openaicompat", contract.Model.Provider)
		require.Equal(t, "probe", contract.Model.Model)
		require.NotNil(t, contract.Tools)
		require.Equal(t, tools, *contract.Tools)
		foundChild = true
	}
	require.True(t, foundChild, "the exact custom profile and tool cap must be durable on the task child")
}

func TestAPITaskInvalidRolesRefuseBeforeCreatingSessions(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("must not execute"))
	token := f.token(t, "role", []string{"read", "run"}, "ask", config.APITokenBinding{})
	before, err := f.app.Sessions.ListAll(t.Context())
	require.NoError(t, err)
	for _, value := range []string{`null`, `""`, `"auto"`, `"unknown"`, `"Research Analyst"`, `7`} {
		response := f.do(t, token, http.MethodPost, "/tasks", `{"prompt":"p","role":`+value+`}`)
		require.Equal(t, http.StatusBadRequest, response.Status, string(response.Body))
	}
	after, err := f.app.Sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before))
}
