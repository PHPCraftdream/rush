package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRequestedRoleSlotSelectsOnlyTheRequestedProfile(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart:    {Provider: "smart-provider", Model: "smart-model", MaxTokens: 101},
		config.SelectedModelTypeFast:     {Provider: "fast-provider", Model: "fast-model", MaxTokens: 102},
		config.SelectedModelTypeWorker:   {Provider: "worker-provider", Model: "worker-model", MaxTokens: 103},
		config.SelectedModelTypeReviewer: {Provider: "review-provider", Model: "review-model", MaxTokens: 104},
	}}
	for _, role := range []RequestedModelRole{"smart", "fast", "worker", "reviewer"} {
		selected, source, err := requestedRoleSlot(cfg, session.Session{}, role)
		require.NoError(t, err)
		require.Equal(t, cfg.Models[config.SelectedModelType(role)], selected)
		require.Equal(t, hbSourceSlotLive, source)
	}
	smart, _, err := requestedRoleSlot(cfg, session.Session{WorkerModelProvider: "parent-worker", WorkerModelID: "parent-worker-model"}, "smart")
	require.NoError(t, err)
	require.Equal(t, "smart-model", smart.Model, "explicit smart must not enter the automatic smart-to-worker cascade")
}

func TestRequestedRoleSessionOverrideKeepsTheSameProfilesInferenceSettings(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeWorker: {Provider: "configured", Model: "configured-model", ReasoningEffort: "low", MaxTokens: 321},
	}}
	parent := session.Session{WorkerModelProvider: "parent-provider", WorkerModelID: "parent-model", WorkerModelReasoningEffort: "high"}
	selected, source, err := requestedRoleSlot(cfg, parent, "worker")
	require.NoError(t, err)
	require.Equal(t, "parent-provider", selected.Provider)
	require.Equal(t, "parent-model", selected.Model)
	require.Equal(t, "high", selected.ReasoningEffort)
	require.EqualValues(t, 321, selected.MaxTokens)
	require.Equal(t, hbSourceSessionOverride, source)
	require.Equal(t, "configured-model", cfg.Models[config.SelectedModelTypeWorker].Model, "selection cannot mutate the published snapshot")
}

func TestRequestedRoleRejectsAbsentAndIncompleteSlots(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "smart", Model: "smart"},
	}}
	_, _, err := requestedRoleSlot(cfg, session.Session{}, "reviewer")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	_, _, err = requestedRoleSlot(cfg, session.Session{SmartModelID: "incomplete"}, "smart")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	_, _, err = requestedRoleSlot(cfg, session.Session{}, "unknown")
	require.ErrorIs(t, err, ErrAgentRoleInvalid)
}

func TestRequestedTenantRoleDoesNotFallThroughToOperatorModels(t *testing.T) {
	t.Parallel()
	coord := &coordinator{} // Any configured/session resolution would fail here.
	creds := &CredentialSet{Models: map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-smart"}}}
	ctx := withCallCredentials(t.Context(), creds)
	for _, role := range []RequestedModelRole{"fast", "worker", "reviewer"} {
		_, err := coord.resolveRequestedDelegationModel(ctx, &config.Config{}, "parent", role)
		require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	}
	require.False(t, creds.AllowConfiguredRoleFallback, "JSON role selection cannot grant configured-provider fallback")
}
