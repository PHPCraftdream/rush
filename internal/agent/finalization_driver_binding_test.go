package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationDriverBindingInstallsIdentityWithoutChangingFrozenPolicy(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	parent, err := fixture.api.BindAgentFinalization(fixture.ctx, fixture.parent)
	require.NoError(t, err)
	parentCtx, err := session.WithAgentFinalizationContext(context.Background(), parent)
	require.NoError(t, err)
	registry := newSubAgentDriverRegistry()
	agent := NewSessionAgent(SessionAgentOptions{})
	credentials := &CredentialSet{}
	options := &CallOptions{MaxAgentDepth: 1, AgentDepthSet: true}
	original := SessionAgentCall{
		SessionID: fixture.child.SessionID, MaxCost: 2, MaxTokens: 123,
		Credentials: credentials, CallOptions: options, Tools: []fantasy.AgentTool{}, RequestedModelRole: RequestedModelRole("reviewer"),
	}
	generation := registry.register(original.SessionID, subAgentDriver{
		agent: agent, call: original,
		finalizationAuthorities: session.CaptureAgentFinalizationContext(parentCtx),
	})
	incoming := SessionAgentCall{
		SessionID: original.SessionID, LogicalCallID: fixture.child.LogicalCallID,
		driverGeneration: generation, finalizationAuthorities: fixture.capability, MaxCost: 200,
		MaxTokens: 9999, RequestedModelRole: RequestedModelRole("smart"), Credentials: &CredentialSet{},
	}
	require.NoError(t, registry.bindFinalizationIdentity(agent, incoming))
	require.NoError(t, registry.bindFinalizationIdentity(agent, incoming), "same identity installation is idempotent")
	stored, ok := registry.get(original.SessionID)
	require.True(t, ok)
	require.Equal(t, generation, stored.generation)
	require.Equal(t, incoming.LogicalCallID, stored.call.LogicalCallID)
	require.Same(t, credentials, stored.call.Credentials)
	require.Same(t, options, stored.call.CallOptions)
	require.NotNil(t, stored.call.Tools)
	require.Empty(t, stored.call.Tools)
	require.Equal(t, original.MaxCost, stored.call.MaxCost)
	require.Equal(t, original.MaxTokens, stored.call.MaxTokens)
	require.Equal(t, original.RequestedModelRole, stored.call.RequestedModelRole)
	wake := stored.callFor("reaction")
	require.Equal(t, generation, wake.driverGeneration)
	require.Equal(t, incoming.LogicalCallID, wake.LogicalCallID)
	require.Equal(t, fixture.capability.Requirement(), wake.finalizationAuthorities.Requirement())
	for _, identity := range []session.AgentFinalizationIdentity{fixture.parent, fixture.child} {
		state, err := session.NewAgentFinalizationStore(fixture.conn).Snapshot(fixture.ctx, identity)
		require.NoError(t, err)
		require.Zero(t, state.Revision, "identity transport does not mint or mutate a durable scope")
	}
}

func TestFinalizationDriverBindingRejectsStaleGenerationAndLogicalReplacement(t *testing.T) {
	for _, changed := range []string{"generation", "agent", "logical"} {
		t.Run(changed, func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			registry := newSubAgentDriverRegistry()
			agent := NewSessionAgent(SessionAgentOptions{})
			call := SessionAgentCall{SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID, finalizationAuthorities: fixture.capability}
			generation := registry.register(call.SessionID, subAgentDriver{agent: agent, call: call, finalizationAuthorities: fixture.capability})
			call.driverGeneration = generation
			switch changed {
			case "generation":
				registry.register(call.SessionID, subAgentDriver{agent: agent, call: call, finalizationAuthorities: fixture.capability})
			case "agent":
				agent = NewSessionAgent(SessionAgentOptions{})
			case "logical":
				call.LogicalCallID = "different"
			}
			require.Error(t, registry.bindFinalizationIdentity(agent, call))
			stored, ok := registry.get(call.SessionID)
			require.True(t, ok)
			require.Equal(t, fixture.child.LogicalCallID, stored.call.LogicalCallID)
			require.Equal(t, fixture.capability.Requirement(), stored.finalizationAuthorities.Requirement())
		})
	}
}

func TestFinalizationDriverBindingCannotWidenAncestorAuthority(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	_, err := db.New(fixture.conn).CreateSession(fixture.ctx, db.CreateSessionParams{ID: "other-owner", Title: "other"})
	require.NoError(t, err)
	other, err := fixture.api.BindAgentFinalization(fixture.ctx, session.AgentFinalizationIdentity{SessionID: "other-owner", LogicalCallID: "other", Generation: 1})
	require.NoError(t, err)
	wider, err := session.WithAgentFinalizationContext(fixture.ctx, other)
	require.NoError(t, err)
	registry := newSubAgentDriverRegistry()
	agent := NewSessionAgent(SessionAgentOptions{})
	call := SessionAgentCall{SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID, finalizationAuthorities: fixture.capability}
	generation := registry.register(call.SessionID, subAgentDriver{agent: agent, call: call, finalizationAuthorities: fixture.capability})
	call.driverGeneration = generation
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(wider)
	require.ErrorIs(t, registry.bindFinalizationIdentity(agent, call), session.ErrAgentFinalizationChanged)
	stored, ok := registry.get(call.SessionID)
	require.True(t, ok)
	require.Equal(t, fixture.capability.Requirement(), stored.finalizationAuthorities.Requirement())
}

func TestFinalizationLogicalIdentityDoesNotRebindAnExistingCall(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	logical, err := finalizationLogicalIdentity(fixture.capability, fixture.child.SessionID, "")
	require.NoError(t, err)
	require.Equal(t, fixture.child.LogicalCallID, logical)
	_, err = finalizationLogicalIdentity(fixture.capability, fixture.child.SessionID, "other")
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)
}
