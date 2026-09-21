package app

// F2 (2026-09-21 weekly audit): the automatic reviewer pass must never
// bypass per-call tenant credentials. A credentialed run
// (RunRequest.Credentials, sdk.Client.RunWithCredentials) whose OWN
// credential set has no reviewer slot, on a process where a reviewer IS
// globally configured, previously continued with RunWithOverrides — the
// GLOBAL config path — silently sending the tenant transcript to the
// operator's reviewer provider. The fix skips the reviewer auto-pass
// entirely for credentialed runs; this test pins that with two provider
// stubs: the tenant's (A) and the globally-configured reviewer's (B),
// asserting ZERO requests ever reach B.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

const (
	credTenantText  = "TENANT ANSWER: only the tenant provider saw this prompt."
	credGlobalText  = "GLOBAL LEAK: the globally-configured reviewer received a tenant transcript."
	credGlobalModel = "global-reviewer"
)

// countingStub is an openai-compat provider stub that records every
// request's model id and answers with a fixed text.
type countingStub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	models []string
	text   string
}

func newCountingStub(t *testing.T, text string) *countingStub {
	t.Helper()
	s := &countingStub{text: text}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		s.mu.Lock()
		s.models = append(s.models, req.Model)
		s.mu.Unlock()
		contentJSON, mErr := json.Marshal(s.text)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"cs","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"cs","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *countingStub) requestedModels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

func TestExecuteRunReviewerPassSkippedForCredentialedRun(t *testing.T) {
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	tenant := newCountingStub(t, credTenantText)
	global := newCountingStub(t, credGlobalText)

	// The GLOBAL config has a reviewer slot pointed at stub B. The run
	// itself is credentialed and its tenant set covers smart+fast only —
	// the exact shape under which the unfixed reviewer pass leaked the
	// transcript to B.
	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "global": {
      "id": "global", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":%q,"context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"global","model":"smart-default"},
    "fast": {"provider":"global","model":"fast-default"},
    "reviewer": {"provider":"global","model":%q}
  }
}`, global.srv.URL, credGlobalModel, credGlobalModel)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "global", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "global", Model: "fast-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "global", Model: credGlobalModel})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	creds := &agent.CredentialSet{
		Credentials: []agent.Credential{{
			Provider: "tenant",
			Type:     agent.ProviderTypeOpenAICompat,
			APIKey:   "tenant-key",
			BaseURL:  tenant.srv.URL,
			Models: []agent.CredentialModel{
				{ID: "tenant-smart", ContextWindow: 200000, DefaultMaxTokens: 1000},
				{ID: "tenant-fast", ContextWindow: 200000, DefaultMaxTokens: 1000},
			},
		}},
		Models: map[agent.Role]agent.ModelChoice{
			agent.RoleSmart: {Provider: "tenant", Model: "tenant-smart"},
			agent.RoleFast:  {Provider: "tenant", Model: "tenant-fast"},
		},
	}
	require.NoError(t, creds.Validate())

	result, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:      "do the tenant thing",
		Overrides:   RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:        RunModeJSON,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
		HideSpinner: true,
		Credentials: creds,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.NotEmpty(t, tenant.requestedModels(), "the tenant's own provider must serve the credentialed run")
	require.Empty(t, global.requestedModels(),
		"a credentialed run must send NOTHING to the globally-configured reviewer: the auto-pass is skipped, not re-routed to global config")
	require.Equal(t, credTenantText, result.FinalText,
		"the envelope's final text must come from the tenant's provider, not from any global reviewer")
}
