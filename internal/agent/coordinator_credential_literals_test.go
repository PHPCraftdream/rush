package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

type credentialLiteralProbe struct {
	calls atomic.Int64
}

func (p *credentialLiteralProbe) resolver() config.VariableResolver {
	return config.NewShellVariableResolver(env.NewFromMap(map[string]string{
		"HOST_VALUE": "host-value",
	}), config.WithExpander(func(_ context.Context, value string, _ []string) (string, error) {
		p.calls.Add(1)
		if value == "configured-$HOST_VALUE" {
			return "resolved-configured-key", nil
		}
		if strings.Contains(value, "$") {
			return "expanded-host-value", nil
		}
		return value, nil
	}))
}

func credentialLiteralStore(probe *credentialLiteralProbe) *config.ConfigStore {
	return config.NewTestStoreWithResolver(&config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"operator": {
				ID:     "operator",
				Type:   openai.Name,
				APIKey: "configured-$HOST_VALUE",
			},
		}),
	}, probe.resolver())
}

func TestBuildCredentialModelUsesLiteralCredentialValues(t *testing.T) {
	probe := &credentialLiteralProbe{}
	store := credentialLiteralStore(probe)
	coord := &coordinator{cfg: store}

	const apiKey = "tenant-$HOST_VALUE-$(printf literal)"
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	baseURL := server.URL + "/api/$HOST_VALUE/$(printf literal)"

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "tenant",
			Type:     ProviderTypeOpenAI,
			APIKey:   apiKey,
			BaseURL:  baseURL,
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "tenant-model"},
		},
	}
	model, providerCfg, err := coord.buildCredentialModel(t.Context(), creds, creds.Models[RoleSmart])
	require.NoError(t, err)
	require.Equal(t, apiKey, providerCfg.APIKey)
	require.Equal(t, baseURL, providerCfg.BaseURL)

	languageModel := model.Model
	_, _ = languageModel.Generate(t.Context(), fantasy.Call{})
	require.Equal(t, "Bearer "+apiKey, gotAuth)
	require.Equal(t, "/api/$HOST_VALUE/$(printf literal)/chat/completions", gotPath)
	require.Equal(t, int64(0), probe.calls.Load(), "tenant credential construction must not invoke the config resolver")
}

func TestBuildProviderStillResolvesConfiguredValues(t *testing.T) {
	probe := &credentialLiteralProbe{}
	coord := &coordinator{cfg: credentialLiteralStore(probe)}
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	providerCfg := config.ProviderConfig{
		ID:      "operator",
		Type:    openai.Name,
		APIKey:  "configured-$HOST_VALUE",
		BaseURL: server.URL,
	}

	provider, err := coord.buildProvider(providerCfg, config.SelectedModel{}, false)
	require.NoError(t, err)
	languageModel, err := provider.LanguageModel(t.Context(), "configured-model")
	require.NoError(t, err)
	_, _ = languageModel.Generate(t.Context(), fantasy.Call{})
	require.Equal(t, "Bearer resolved-configured-key", gotAuth)
	require.Equal(t, int64(2), probe.calls.Load(), "configured provider fields must retain shell resolution")
}

func TestRunWithCredentialsUsesLiteralCredentialValues(t *testing.T) {
	probe := &credentialLiteralProbe{}
	store := credentialLiteralStore(probe)
	coord := &coordinator{cfg: store}

	const apiKey = "tenant-$HOST_VALUE-$(printf literal)"
	var requests atomic.Int64
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	baseURL := server.URL + "/api/$HOST_VALUE/$(printf literal)"

	testEnvironment := testEnv(t)
	session, err := testEnvironment.sessions.Create(t.Context(), "credential literals")
	require.NoError(t, err)
	coord.sessions = testEnvironment.sessions
	coord.messages = testEnvironment.messages
	var observedCredentials *CredentialSet
	var observedCall SessionAgentCall
	coord.currentAgent = &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			observedCredentials = call.Credentials
			observedCall = call
			_, _ = call.SmartModel.Model.Generate(ctx, fantasy.Call{})
			call.Credentials.Credentials[0].APIKey = "mutated-in-run"
			return agentResultWithText("done"), nil
		},
	}

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "tenant",
			Type:     ProviderTypeOpenAI,
			APIKey:   apiKey,
			BaseURL:  baseURL,
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "tenant-smart"},
			RoleFast:  {Provider: "tenant", Model: "tenant-fast"},
		},
	}
	_, err = coord.RunWithCredentials(t.Context(), session.ID, "prompt", creds)
	require.NoError(t, err)
	require.Equal(t, int64(1), requests.Load())
	require.Equal(t, "Bearer "+apiKey, gotAuth)
	require.Equal(t, "/api/$HOST_VALUE/$(printf literal)/chat/completions", gotPath)
	require.Equal(t, int64(0), probe.calls.Load(), "RunWithCredentials must not invoke the config resolver for tenant fields")
	require.NotSame(t, creds, observedCredentials)
	require.Equal(t, "tenant-smart", observedCall.SmartModel.ModelCfg.Model)
	require.Equal(t, "tenant-fast", observedCall.FastModel.ModelCfg.Model)
	require.Equal(t, apiKey, creds.Credentials[0].APIKey, "RunWithCredentials must isolate its cloned credentials")
}
