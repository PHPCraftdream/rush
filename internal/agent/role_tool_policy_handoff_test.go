package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type rolePolicyCaptureModel struct {
	mu       sync.Mutex
	ctx      context.Context
	call     fantasy.Call
	provider string
	model    string
}

func (m *rolePolicyCaptureModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("unexpected Generate")
}

func (m *rolePolicyCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.ctx = ctx
	m.call = call
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "policy-text"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "policy-text", Delta: "ok"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "policy-text"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *rolePolicyCaptureModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unexpected GenerateObject")
}

func (m *rolePolicyCaptureModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unexpected StreamObject")
}

func (m *rolePolicyCaptureModel) Provider() string { return m.provider }
func (m *rolePolicyCaptureModel) Model() string    { return m.model }

func (m *rolePolicyCaptureModel) snapshot() (context.Context, fantasy.Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctx, m.call
}

func TestRunTurnAttachesRoleToolPolicyToEveryConstrainedTurn(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	hbIsolateEnv(t)
	env := testEnv(t)

	allowedTools := []string{"view"}
	contract := &session.RoleContract{
		Version: session.RoleContractVersion,
		Name:    "analyst",
		Model:   session.ModelCfg{Provider: "tenant", Model: "tenant-model"},
		Tools:   &allowedTools,
	}
	capturedTools := []string{"view", "captured-only"}
	capturedContract := &session.RoleContract{Version: session.RoleContractVersion, Tools: &capturedTools}
	require.NoError(t, capturedContract.Validate())
	currentTools := []string{"view", "current-only"}
	currentContract := &session.RoleContract{Version: session.RoleContractVersion, Name: "analyst", Model: session.ModelCfg{Provider: "tenant", Model: "tenant-model"}, Tools: &currentTools}
	tests := []struct {
		name          string
		providerID    string
		contract      *session.RoleContract
		wantPolicy    bool
		wantAllowed   []string
		wantTools     []string
		captured      bool
		requestedRole RequestedModelRole
	}{
		{name: "custom configured CLI ID", providerID: "local-cli", contract: contract, wantPolicy: true, wantAllowed: []string{"view"}, wantTools: []string{"view"}},
		{name: "cli ID with ordinary API mock", providerID: "cli", contract: contract, wantPolicy: true, wantAllowed: []string{"view"}, wantTools: []string{"view"}},
		{name: "captured and current caps intersect", providerID: "local-cli", contract: currentContract, wantPolicy: true, wantAllowed: []string{"view"}, wantTools: []string{"view"}, captured: true},
		{name: "unrestricted", providerID: "local-cli", wantTools: []string{"view", "write", "captured-only", "current-only"}},
		{name: "builtin without tool cap", providerID: "local-cli", requestedRole: RequestedModelRole(config.SelectedModelTypeSmart), wantTools: []string{"view", "write", "captured-only", "current-only"}},
		{name: "role without tool cap", providerID: "local-cli", contract: &session.RoleContract{Version: session.RoleContractVersion, Name: "analyst", Model: session.ModelCfg{Provider: "tenant", Model: "tenant-model"}}, wantTools: []string{"view", "write", "captured-only", "current-only"}},
		{name: "inherited-only", providerID: "local-cli", contract: &session.RoleContract{Version: session.RoleContractVersion, Inherited: [][]string{{"absent"}}}, wantPolicy: true, wantAllowed: []string{}, wantTools: []string{}},
		{name: "empty capabilities", providerID: "local-cli", contract: &session.RoleContract{Version: session.RoleContractVersion, Tools: &[]string{}}, wantPolicy: true, wantAllowed: []string{}, wantTools: []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.contract != nil {
				require.NoError(t, tc.contract.Validate())
			}
			model := &rolePolicyCaptureModel{provider: "capture", model: "capture-model"}
			wrappedModel := &peakHoursRequestModel{
				LanguageModel: &hbLanguageModel{inner: model},
				check:         func(context.Context) error { return nil },
			}
			modelCfg := config.SelectedModel{Provider: tc.providerID, Model: "chosen-model", ReasoningEffort: "max"}
			modelRef := Model{Model: wrappedModel, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}, ModelCfg: modelCfg}
			tool := func(name string) fantasy.AgentTool {
				return fantasy.NewAgentTool(name, name, func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse(name), nil
				})
			}
			allTools := []fantasy.AgentTool{tool("view"), tool("write"), tool("captured-only"), tool("current-only")}
			if tc.captured {
				allTools = []fantasy.AgentTool{
					roleGuardTool{AgentTool: tool("view"), contracts: []*session.RoleContract{capturedContract}},
					roleGuardTool{AgentTool: tool("write"), contracts: []*session.RoleContract{capturedContract}},
					roleGuardTool{AgentTool: tool("captured-only"), contracts: []*session.RoleContract{capturedContract}},
					roleGuardTool{AgentTool: tool("current-only"), contracts: []*session.RoleContract{capturedContract}},
				}
			}
			agent := testSessionAgent(env, modelRef.Model, modelRef.Model, "policy-test", allTools...).(*sessionAgent)
			agent.SetModels(modelRef, modelRef)
			sess, err := env.sessions.Create(t.Context(), "policy "+tc.name)
			require.NoError(t, err)
			_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "prior context"}},
			})
			require.NoError(t, err)

			ctx := t.Context()
			var callOptions *CallOptions
			if tc.contract != nil {
				callOptions = &CallOptions{RoleContract: tc.contract.Clone()}
				ctx = WithCallOptions(ctx, callOptions)
				t.Cleanup(callOptions.buildScope.close)
			}
			_, err = agent.Run(ctx, SessionAgentCall{
				SessionID:          sess.ID,
				Prompt:             "inspect current state",
				LogicalCallID:      "logical-" + tc.name,
				RequestedModelRole: tc.requestedRole,
				CallOptions:        callOptions,
			})
			require.NoError(t, err)

			capturedCtx, capturedCall := model.snapshot()
			require.NotNil(t, capturedCtx)
			require.NotEqual(t, true, capturedCtx.Value(cliprovider.TitleGenerationContextKey))
			policy, hasPolicy := cliprovider.ToolPolicyFrom(capturedCtx)
			require.Equal(t, tc.wantPolicy, hasPolicy)
			if hasPolicy {
				require.True(t, policy.Constrained)
				require.Equal(t, tc.wantAllowed, policy.Allowed)
			}
			if tc.captured {
				policy, ok := cliprovider.ToolPolicyFrom(capturedCtx)
				require.True(t, ok)
				require.Equal(t, []string{"view"}, policy.Allowed)
				require.True(t, policy.Constrained)
				policy.Allowed[0] = "mutation"
				policyAfterReadMutation, ok := cliprovider.ToolPolicyFrom(capturedCtx)
				require.True(t, ok)
				require.Equal(t, []string{"view"}, policyAfterReadMutation.Allowed)
			}
			require.Len(t, allTools, 4, "per-turn role filtering must not mutate the source tools")
			require.Equal(t, tc.wantTools, rolePolicyCallToolNames(capturedCall))
			require.Equal(t, modelCfg, agent.Model().ModelCfg)
			require.Equal(t, "chosen-model", modelRef.ModelCfg.Model)
			require.Equal(t, tc.providerID, modelRef.ModelCfg.Provider)
			require.Equal(t, "max", modelRef.ModelCfg.ReasoningEffort)
			require.Equal(t, "max", capturedCtx.Value(cliprovider.ReasoningEffortContextKey))

			if tc.contract != nil {
				identityCall := SessionAgentCall{
					SessionID:          sess.ID,
					Prompt:             "resume without carried role context",
					LogicalCallID:      "logical-" + tc.name,
					RequestedModelRole: tc.requestedRole,
				}
				restoredCall, restoreErr := restoreRootRoleContract(t.Context(), env.sessions, identityCall)
				require.NoError(t, restoreErr)
				require.NotNil(t, restoredCall.CallOptions)
				require.NotNil(t, restoredCall.CallOptions.RoleContract)
				require.True(t, tc.contract.Equal(restoredCall.CallOptions.RoleContract))
				t.Cleanup(restoredCall.CallOptions.buildScope.close)
				_, err = agent.Run(t.Context(), restoredCall)
				require.NoError(t, err)
				resumedCtx, resumedCall := model.snapshot()
				require.NotNil(t, resumedCtx)
				require.NotEqual(t, true, resumedCtx.Value(cliprovider.TitleGenerationContextKey))
				resumedPolicy, hasResumedPolicy := cliprovider.ToolPolicyFrom(resumedCtx)
				require.Equal(t, tc.wantPolicy, hasResumedPolicy)
				if hasResumedPolicy {
					require.True(t, resumedPolicy.Constrained)
					require.Equal(t, tc.wantAllowed, resumedPolicy.Allowed)
				}
				require.Equal(t, tc.wantTools, rolePolicyCallToolNames(resumedCall))
				require.Equal(t, "max", resumedCtx.Value(cliprovider.ReasoningEffortContextKey))
			}
		})
	}
}

func rolePolicyCallToolNames(call fantasy.Call) []string {
	names := make([]string, 0, len(call.Tools))
	for _, tool := range call.Tools {
		if function, ok := tool.(fantasy.FunctionTool); ok {
			names = append(names, function.Name)
		}
	}
	return names
}
