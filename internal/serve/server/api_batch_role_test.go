package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

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
	const customModel = "research-only-model"
	requestedModels := make(chan []byte, 32)
	f := newBatchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			select {
			case requestedModels <- body:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, batchSSEText("custom role result"))
	})
	tools := []string{"view", "grep"}
	require.NoError(t, f.cfg.SetCustomRole(config.ScopeWorkspace, "Research Analyst", config.CustomRoleUpdate{
		Model: config.SelectedModel{Provider: "openaicompat", Model: customModel},
		Tools: &tools,
	}))
	token := f.token(t, "custom-role", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", `{"prompt":"p","role":"Research Analyst"}`).accepted(t)
	result := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "completed", result.State)

	modelDeadline := time.NewTimer(5 * time.Second)
	defer modelDeadline.Stop()
	gotCustomModel := false
	for !gotCustomModel {
		select {
		case body := <-requestedModels:
			var providerRequest struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.Unmarshal(body, &providerRequest))
			gotCustomModel = providerRequest.Model == customModel
		case <-modelDeadline.C:
			require.FailNow(t, "local fake provider never received the selected custom model")
		}
	}

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
		require.Equal(t, customModel, contract.Model.Model)
		require.NotNil(t, contract.Tools)
		require.Equal(t, tools, *contract.Tools)
		foundChild = true
	}
	require.True(t, foundChild, "the exact custom profile and tool cap must be durable on the task child")
}

func TestAPITaskInvalidRolesRefuseBeforeCreatingSessions(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("must not execute"))
	customRole := config.CustomRoleUpdate{Model: config.SelectedModel{Provider: "openaicompat", Model: "probe"}}
	require.NoError(t, f.cfg.SetCustomRole(config.ScopeWorkspace, "research analyst", customRole))
	require.NoError(t, f.cfg.SetCustomRole(config.ScopeWorkspace, "Deleted Analyst", customRole))
	require.NoError(t, f.cfg.RemoveCustomRole(config.ScopeWorkspace, "Deleted Analyst"))
	require.NoError(t, f.cfg.SetCustomRole(config.ScopeWorkspace, "Missing Provider Analyst", config.CustomRoleUpdate{
		Model: config.SelectedModel{Provider: "missing-provider", Model: "probe"},
	}))
	require.Contains(t, f.cfg.Config().Roles, "research analyst")
	require.NotContains(t, f.cfg.Config().Roles, "Research Analyst")
	require.NotContains(t, f.cfg.Config().Roles, "Deleted Analyst")
	require.Contains(t, f.cfg.Config().Roles, "Missing Provider Analyst")
	_, providerConfigured := f.cfg.Config().Providers.Get("missing-provider")
	require.False(t, providerConfigured)

	token := f.token(t, "role", []string{"read", "run"}, "ask", config.APITokenBinding{})
	before, err := f.app.Sessions.ListAll(t.Context())
	require.NoError(t, err)
	for _, value := range []string{`null`, `""`, `"auto"`, `"unknown"`, `"Research Analyst"`, `"Deleted Analyst"`, `"Missing Provider Analyst"`, `7`} {
		response := f.do(t, token, http.MethodPost, "/tasks", `{"prompt":"p","role":`+value+`}`)
		require.Equal(t, http.StatusBadRequest, response.Status, string(response.Body))
	}
	after, err := f.app.Sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before))

	serviceID := serviceSessionID(token2ID(t, f, token))
	_, err = f.app.Sessions.Get(t.Context(), serviceID)
	require.Error(t, err, "refused roles must not create the token's service session")
	fac, err := f.app.BatchCLIStore()
	require.NoError(t, err)
	roots, err := fac.Store.BatchAPIRoots(t.Context(), serviceID)
	require.NoError(t, err)
	require.Empty(t, roots, "refused roles must not write batch roots")
	require.Zero(t, f.trace.providerStarted.Load(), "refused roles must not invoke the local model provider")
	f.service.mu.Lock()
	watching := len(f.service.watching)
	locks := len(f.service.locks)
	f.service.mu.Unlock()
	require.Zero(t, watching, "refused roles must not reserve or watch batch capacity")
	require.Zero(t, locks, "refused roles must not enter batch backend admission")
}
