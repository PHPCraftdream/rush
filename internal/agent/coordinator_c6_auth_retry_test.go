package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type c6IdentityModel struct {
	fantasy.LanguageModel
	tag string
}

func (m *c6IdentityModel) Provider() string { return "c6-test" }
func (m *c6IdentityModel) Model() string    { return m.tag }

type c6NotificationRecorder struct {
	notifications []notify.Notification
}

func (r *c6NotificationRecorder) Publish(_ pubsub.EventType, notification notify.Notification) {
	r.notifications = append(r.notifications, notification)
}

func c6ConfigureProvider(t *testing.T, coord *coordinator, providerID, modelID, apiKey string, token *oauth.Token) {
	t.Helper()
	coord.cfg.SetProviderRuntimeConfig(providerID, config.ProviderConfig{
		ID:         providerID,
		Type:       openai.Name,
		APIKey:     apiKey,
		OAuthToken: token,
		Models:     []catwalk.Model{{ID: modelID, DefaultMaxTokens: 321}},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{
		Provider: providerID,
		Model:    modelID,
	}
	coord.cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{
		Provider: providerID,
		Model:    modelID,
	}
}

func c6BuildModel(t *testing.T, coord *coordinator, isSubAgent bool) Model {
	t.Helper()
	cfg, _ := coord.cfg.Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]
	smart, _, err := coord.buildModelsFromCfg(t.Context(), cfg, smartCfg, fastCfg, isSubAgent)
	require.NoError(t, err)
	return smart
}

func c6SuccessResult() *fantasy.AgentResult {
	return agentResultWithText("fresh client succeeded")
}

func c6Unauthorized() error {
	return &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "stale client"}
}

func TestRunSubAgent_401RebuildsPinnedSharedClient(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	t.Setenv("C6_SUBAGENT_KEY", "new-key")
	providerID := "smart-provider"
	c6ConfigureProvider(t, coord, providerID, "pinned-model", "old-key", nil)
	providerCfg, ok := coord.cfg.Config().Providers.Get("fast-provider")
	require.True(t, ok)
	providerCfg.APIKey = "old-key"
	providerCfg.APIKeyTemplate = "$C6_SUBAGENT_KEY"
	coord.cfg.SetProviderRuntimeConfig("fast-provider", providerCfg)
	providerCfg, ok = coord.cfg.Config().Providers.Get(providerID)
	require.True(t, ok)
	providerCfg.APIKeyTemplate = "$C6_SUBAGENT_KEY"
	coord.cfg.SetProviderRuntimeConfig(providerID, providerCfg)

	oldModel := c6BuildModel(t, coord, true)
	oldClient := &c6IdentityModel{tag: "old-client"}
	oldModel.Model = oldClient
	parent, err := env.sessions.Create(t.Context(), "sub-agent auth retry")
	require.NoError(t, err)

	var calls []SessionAgentCall
	agent := &mockSessionAgent{model: oldModel}
	agent.runFunc = func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		if call.SmartModel == nil {
			return nil, c6Unauthorized()
		}
		if _, old := call.SmartModel.Model.(*c6IdentityModel); old {
			c6ConfigureProvider(t, coord, "replacement-provider", "replacement-model", "replacement-key", nil)
			return nil, c6Unauthorized()
		}
		return c6SuccessResult(), nil
	}
	coord.currentAgent = agent

	response, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parent.ID,
		AgentMessageID: "message",
		ToolCallID:     "tool",
		Prompt:         "run with the refreshed client",
		SessionTitle:   "auth retry child",
	})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Len(t, calls, 2)
	firstClient, firstOld := calls[0].SmartModel.Model.(*c6IdentityModel)
	_, secondOld := calls[1].SmartModel.Model.(*c6IdentityModel)
	require.True(t, firstOld, "first client type: %T", calls[0].SmartModel.Model)
	require.Same(t, oldClient, firstClient)
	require.False(t, secondOld)
	require.Equal(t, "pinned-model", calls[1].SmartModel.ModelCfg.Model)
	require.Equal(t, "smart-provider", calls[1].SmartModel.ModelCfg.Provider)
	require.Equal(t, "replacement-provider", coord.cfg.Config().Models[config.SelectedModelTypeSmart].Provider)
	require.Equal(t, "replacement-model", coord.cfg.Config().Models[config.SelectedModelTypeSmart].Model)
}

func TestRunWithUnauthorizedRetry_RebuildFailuresAreTerminal(t *testing.T) {
	rebuildErr := errors.New("local rebuild failed")
	tests := []struct {
		name    string
		rebuild func() error
		want    error
	}{
		{name: "missing rebuild", want: errUnauthorizedRebuildUnavailable},
		{name: "failed rebuild", rebuild: func() error { return rebuildErr }, want: rebuildErr},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, false)
			t.Setenv("C6_REBUILD_KEY", "new-key")
			c6ConfigureProvider(t, coord, "smart-provider", "retry-model", "old-key", nil)
			providerCfg, ok := coord.cfg.Config().Providers.Get("smart-provider")
			require.True(t, ok)
			providerCfg.APIKeyTemplate = "$C6_REBUILD_KEY"
			coord.cfg.SetProviderRuntimeConfig("smart-provider", providerCfg)
			coord.currentAgent = &mockSessionAgent{}

			calls := 0
			err := coord.runWithUnauthorizedRetry(t.Context(), providerCfg, func() error {
				calls++
				return c6Unauthorized()
			}, tc.rebuild)

			require.ErrorIs(t, err, tc.want)
			require.False(t, coord.isUnauthorized(err))
			require.Equal(t, 1, calls)
		})
	}
}

func TestRunSubAgent_401CredentialedCallFailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	c6ConfigureProvider(t, coord, "operator-provider", "operator-model", "operator-key", nil)
	t.Setenv("C6_OPERATOR_KEY", "operator-new-key")
	operatorCfg, ok := coord.cfg.Config().Providers.Get("operator-provider")
	require.True(t, ok)
	operatorCfg.APIKeyTemplate = "$C6_OPERATOR_KEY"
	coord.cfg.SetProviderRuntimeConfig("operator-provider", operatorCfg)

	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant-provider", Type: ProviderTypeOpenAI, APIKey: "tenant-old"}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant-provider", Model: "tenant-model"},
		},
	}
	tenantModel, tenantProvider, err := coord.buildCredentialModel(t.Context(), creds, creds.Models[RoleSmart])
	require.NoError(t, err)
	require.Equal(t, "tenant-provider", tenantProvider.ID)
	parent, err := env.sessions.Create(t.Context(), "credentialed auth retry")
	require.NoError(t, err)

	var calls int
	agent := &mockSessionAgent{model: tenantModel}
	agent.runFunc = func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls++
		require.Same(t, creds, call.Credentials)
		return nil, c6Unauthorized()
	}
	coord.currentAgent = agent

	response, err := coord.runSubAgent(withCallCredentials(t.Context(), creds), subAgentParams{
		Agent:          agent,
		SessionID:      parent.ID,
		AgentMessageID: "message",
		ToolCallID:     "tool",
		Prompt:         "do not cross the tenant boundary",
		SessionTitle:   "credentialed auth retry child",
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, 1, calls)
}

func TestSummarize_ProactiveRefreshRebuildsImmutableSnapshot(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	oldToken := &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	c6ConfigureProvider(t, coord, "smart-provider", "summary-model", "old-key", oldToken)
	session, err := env.sessions.Create(t.Context(), "proactive summarize")
	require.NoError(t, err)
	snapshot, err := coord.buildSummarizeSnapshot(t.Context(), session.ID)
	require.NoError(t, err)
	oldClient := snapshot.model.Model
	snapshot.model.Model = &c6IdentityModel{tag: "old-client"}
	oldClient = snapshot.model.Model

	coord.refreshOAuth2TokenFn = func(_ context.Context, _ config.ProviderConfig) error {
		fresh := config.ProviderConfig{
			ID:         "smart-provider",
			Type:       openai.Name,
			APIKey:     "new-key",
			OAuthToken: &oauth.Token{AccessToken: "new-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:     []catwalk.Model{{ID: "summary-model", DefaultMaxTokens: 321}},
		}
		coord.cfg.SetProviderRuntimeConfig("smart-provider", fresh)
		return nil
	}
	var calls int
	agent := &mockSessionAgent{model: snapshot.model}
	agent.summarizeFunc = func(_ context.Context, _ string, got *SummarizeSnapshot) error {
		calls++
		_, old := got.model.Model.(*c6IdentityModel)
		require.False(t, old)
		return nil
	}
	coord.currentAgent = agent

	require.NoError(t, coord.Summarize(t.Context(), session.ID, snapshot))
	require.Equal(t, 1, calls)
	originalClient, originalOld := snapshot.model.Model.(*c6IdentityModel)
	require.True(t, originalOld)
	require.Same(t, oldClient, originalClient)
}

func TestSummarize_401RebuildsPinnedClient(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	oldToken := &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	c6ConfigureProvider(t, coord, "smart-provider", "summary-model", "old-key", oldToken)
	session, err := env.sessions.Create(t.Context(), "401 summarize")
	require.NoError(t, err)
	snapshot, err := coord.buildSummarizeSnapshot(t.Context(), session.ID)
	require.NoError(t, err)
	oldClient := &c6IdentityModel{tag: "old-client"}
	snapshot.model.Model = oldClient
	coord.refreshOAuth2TokenFn = func(_ context.Context, _ config.ProviderConfig) error {
		c6ConfigureProvider(t, coord, "replacement-provider", "replacement-model", "replacement-key", nil)
		fresh := config.ProviderConfig{
			ID:         "smart-provider",
			Type:       openai.Name,
			APIKey:     "new-key",
			OAuthToken: &oauth.Token{AccessToken: "new-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:     []catwalk.Model{{ID: "summary-model", DefaultMaxTokens: 321}},
		}
		coord.cfg.SetProviderRuntimeConfig("smart-provider", fresh)
		return nil
	}
	var calls []fantasy.LanguageModel
	var selections []config.SelectedModel
	agent := &mockSessionAgent{model: snapshot.model}
	agent.summarizeFunc = func(_ context.Context, _ string, got *SummarizeSnapshot) error {
		calls = append(calls, got.model.Model)
		selections = append(selections, got.model.ModelCfg)
		if _, old := got.model.Model.(*c6IdentityModel); old {
			return c6Unauthorized()
		}
		return nil
	}
	coord.currentAgent = agent

	require.NoError(t, coord.Summarize(t.Context(), session.ID, snapshot))
	require.Len(t, calls, 2)
	firstClient, firstOld := calls[0].(*c6IdentityModel)
	_, secondOld := calls[1].(*c6IdentityModel)
	require.True(t, firstOld)
	require.Same(t, oldClient, firstClient)
	require.False(t, secondOld)
	require.Len(t, selections, 2)
	for _, selection := range selections {
		require.Equal(t, "smart-provider", selection.Provider)
		require.Equal(t, "summary-model", selection.Model)
	}
	require.Equal(t, "replacement-provider", coord.cfg.Config().Models[config.SelectedModelTypeSmart].Provider)
	require.Equal(t, "replacement-model", coord.cfg.Config().Models[config.SelectedModelTypeSmart].Model)
	originalClient, originalOld := snapshot.model.Model.(*c6IdentityModel)
	require.True(t, originalOld)
	require.Same(t, oldClient, originalClient)
}

func TestSummarize_RefreshRebuildFailureDoesNotRetry(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	oldToken := &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	c6ConfigureProvider(t, coord, "smart-provider", "summary-model", "old-key", oldToken)
	session, err := env.sessions.Create(t.Context(), "failed summarize rebuild")
	require.NoError(t, err)
	snapshot, err := coord.buildSummarizeSnapshot(t.Context(), session.ID)
	require.NoError(t, err)
	coord.refreshOAuth2TokenFn = func(_ context.Context, _ config.ProviderConfig) error {
		c6ConfigureProvider(t, coord, "replacement-provider", "replacement-model", "replacement-key", nil)
		coord.cfg.SetProviderRuntimeConfig("smart-provider", config.ProviderConfig{
			ID:         "smart-provider",
			Type:       catwalk.Type("unsupported-test-provider"),
			APIKey:     "new-key",
			OAuthToken: &oauth.Token{AccessToken: "new-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:     []catwalk.Model{{ID: "summary-model"}},
		})
		return nil
	}
	calls := 0
	agent := &mockSessionAgent{model: snapshot.model}
	agent.summarizeFunc = func(context.Context, string, *SummarizeSnapshot) error {
		calls++
		return c6Unauthorized()
	}
	coord.currentAgent = agent

	err = coord.Summarize(t.Context(), session.ID, snapshot)
	require.Error(t, err)
	require.False(t, coord.isUnauthorized(err))
	require.Contains(t, err.Error(), "failed to rebuild summarize model")
	require.Equal(t, 1, calls)
}

func TestRunSubAgent_HyperRebuildFailureDoesNotNotifyReauthentication(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	oldToken := &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	c6ConfigureProvider(t, coord, hyper.Name, "hyper-model", "old-key", oldToken)
	oldModel := c6BuildModel(t, coord, true)
	oldModel.Model = &c6IdentityModel{tag: "old-client"}
	parent, err := env.sessions.Create(t.Context(), "hyper rebuild failure")
	require.NoError(t, err)

	recorder := new(c6NotificationRecorder)
	coord.notify = recorder
	coord.refreshOAuth2TokenFn = func(_ context.Context, _ config.ProviderConfig) error {
		c6ConfigureProvider(t, coord, "replacement-provider", "replacement-model", "replacement-key", nil)
		coord.cfg.SetProviderRuntimeConfig(hyper.Name, config.ProviderConfig{
			ID:         hyper.Name,
			Type:       catwalk.Type("unsupported-test-provider"),
			APIKey:     "new-key",
			OAuthToken: &oauth.Token{AccessToken: "new-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:     []catwalk.Model{{ID: "hyper-model"}},
		})
		return nil
	}
	calls := 0
	agent := &mockSessionAgent{model: oldModel}
	agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		calls++
		return nil, c6Unauthorized()
	}
	coord.currentAgent = agent

	response, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parent.ID,
		AgentMessageID: "message",
		ToolCallID:     "tool",
		Prompt:         "fail the refreshed client rebuild",
		SessionTitle:   "hyper auth retry child",
	})

	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, 1, calls)
	require.Empty(t, recorder.notifications)
}
