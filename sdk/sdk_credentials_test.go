package sdk_test

// Concurrency-isolation test for sdk.Client.RunWithCredentials
// (per-call multi-tenant credentials): 2 simultaneous RunWithCredentials
// calls on ONE sdk.Client, each with its own ContinueSessionID and its
// own CredentialSet pointing at its own httptest provider. Each provider
// answers with a tenant-specific marker and records every Authorization
// header it sees, so the assertions prove each turn was served by — and
// only by — its own tenant's provider:
//
//   - tenant A's result carries A's marker, tenant B's carries B's;
//   - A's server only ever saw A's API key (never B's), B's only B's;
//   - the operator's own config provider (a third server, wired through
//     rush.json as the workspace's smart/fast selection) saw NO request
//     at all — per-call credentials must not fall back to config.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// credentialServer records every Authorization header value it received
// and answers every chat-completions request with a single-round,
// tool-free assistant message carrying finalText.
type credentialServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth map[string]int // Authorization header value -> hit count
}

func newCredentialServer(t *testing.T, finalText string) *credentialServer {
	t.Helper()
	cs := &credentialServer{auth: map[string]int{}}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cs.mu.Lock()
		cs.auth[r.Header.Get("Authorization")]++
		cs.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(chunks []string) {
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				if fl != nil {
					fl.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		write([]string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, finalText),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":5,"total_tokens":22}}`,
		})
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *credentialServer) hits() map[string]int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make(map[string]int, len(cs.auth))
	for k, v := range cs.auth {
		out[k] = v
	}
	return out
}

func (cs *credentialServer) totalRequests() int {
	n := 0
	for _, v := range cs.hits() {
		n += v
	}
	return n
}

// seedTenantSession pre-creates the tenant's session (explicit
// ContinueSessionID, get-or-create semantics) with one pre-existing user
// message so background title generation never fires and each mock
// provider serves exactly the main flow.
func seedTenantSession(t *testing.T, workDir, sessionID string) {
	t.Helper()
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	_, err = sessions.CreateWithID(context.Background(), sessionID, "tenant "+sessionID)
	require.NoError(t, err)
	messages := message.NewService(q)
	_, err = messages.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
}

func TestRunWithCredentialsConcurrentTenantsIsolated(t *testing.T) {
	// Same global-config isolation as the other sdk tests: config.Init
	// inside sdk.Open must read only the rush.json written below.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// The operator's own provider: what rush.json selects as the
	// workspace's smart/fast model. Tenant runs must NEVER reach it.
	operator := newCredentialServer(t, "OPERATOR_PROVIDER_MUST_NOT_BE_HIT")

	workDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "operator": {
      "id": "operator",
      "name": "operator",
      "type": "openai-compat",
      "base_url": %q,
      "api_key": "operator-key",
      "discover_models": false,
      "models": [
        {"id": "operator-model", "name": "operator-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "operator", "model": "operator-model"},
    "fast": {"provider": "operator", "model": "operator-model"}
  }
}`, operator.srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	const (
		tenantASession = "sdk-creds-tenant-a"
		tenantBSession = "sdk-creds-tenant-b"
		tenantAKey     = "sk-tenant-a-secret"
		tenantBKey     = "sk-tenant-b-secret"
		tenantAMarker  = "TENANT_A_PROVIDER_OK"
		tenantBMarker  = "TENANT_B_PROVIDER_OK"
	)
	serverA := newCredentialServer(t, tenantAMarker)
	serverB := newCredentialServer(t, tenantBMarker)

	seedTenantSession(t, workDir, tenantASession)
	seedTenantSession(t, workDir, tenantBSession)

	credentialSet := func(baseURL, apiKey string) sdk.CredentialSet {
		return sdk.CredentialSet{
			Credentials: []sdk.Credential{
				{
					Provider: "tenant-provider",
					Type:     sdk.ProviderTypeOpenAICompat,
					APIKey:   apiKey,
					BaseURL:  baseURL,
					Models: []sdk.CredentialModel{
						{ID: "tenant-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
					},
				},
			},
			Models: map[sdk.Role]sdk.ModelChoice{
				sdk.RoleSmart:    {Provider: "tenant-provider", Model: "tenant-model"},
				sdk.RoleFast:     {Provider: "tenant-provider", Model: "tenant-model"},
				sdk.RoleWorker:   {Provider: "tenant-provider", Model: "tenant-model"},
				sdk.RoleReviewer: {Provider: "tenant-provider", Model: "tenant-model"},
			},
		}
	}
	credsA := credentialSet(serverA.srv.URL, tenantAKey)
	credsB := credentialSet(serverB.srv.URL, tenantBKey)

	runTenant := func(sessionID string, creds sdk.CredentialSet) (*sdk.RunResult, error) {
		var buf bytes.Buffer
		res, err := client.RunWithCredentials(context.Background(), sdk.RunRequest{
			Prompt:            "reply with exactly the marker text and nothing else",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionID,
			Stdout:            &buf,
			HideSpinner:       true,
		}, creds)
		if err != nil {
			return nil, fmt.Errorf("session %s run failed: %w (output %q)", sessionID, err, buf.String())
		}
		if res == nil {
			return nil, fmt.Errorf("session %s: nil envelope", sessionID)
		}
		if res.ExitReason != "end_turn" {
			return res, fmt.Errorf("session %s: exit_reason=%q error=%q warnings=%v output=%q", sessionID, res.ExitReason, res.Error, res.Warnings, buf.String())
		}
		return res, nil
	}

	// Fire both tenants SIMULTANEOUSLY on the ONE client. require must
	// stay on the test goroutine, so the goroutines only collect errors.
	var (
		wg      sync.WaitGroup
		startCh = make(chan struct{})
		resA    *sdk.RunResult
		resB    *sdk.RunResult
		errA    error
		errB    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		resA, errA = runTenant(tenantASession, credsA)
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resB, errB = runTenant(tenantBSession, credsB)
	}()
	close(startCh)
	wg.Wait()
	require.NoError(t, errA, "tenant A run")
	require.NoError(t, errB, "tenant B run")

	// 1. Each turn was answered by ITS OWN provider (marker check).
	require.Equal(t, tenantAMarker, resA.FinalText, "tenant A's turn must be served by tenant A's provider")
	require.Equal(t, tenantBMarker, resB.FinalText, "tenant B's turn must be served by tenant B's provider")

	// 2. A's server only ever saw A's key; B's only B's. That asserts both
	// "never picked up the other tenant's credentials" and "never fell
	// through to a shared/config provider client".
	aHits := serverA.hits()
	bHits := serverB.hits()
	require.NotEmpty(t, aHits, "tenant A's provider must have received at least one request")
	require.NotEmpty(t, bHits, "tenant B's provider must have received at least one request")
	for auth := range aHits {
		require.Equal(t, "Bearer "+tenantAKey, auth, "tenant A's provider must only ever see tenant A's API key")
	}
	for auth := range bHits {
		require.Equal(t, "Bearer "+tenantBKey, auth, "tenant B's provider must only ever see tenant B's API key")
	}

	// 3. The operator's config provider saw no traffic at all.
	require.Equal(t, 0, operator.totalRequests(),
		"per-call credentials must fully replace config provider resolution; the operator's provider was contacted")
}
