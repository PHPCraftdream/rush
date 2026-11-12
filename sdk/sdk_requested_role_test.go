package sdk_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

const requestedSDKRole = "Rôle Research α"

type requestedSDKRequest struct {
	Method string `json:"-"`
	Path   string `json:"-"`
	Model  string `json:"model"`
	Tools  []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

const (
	requestedSDKMaxRequests = 8
	requestedSDKMaxBodySize = 1 << 20
)

type requestedSDKServer struct {
	srv             *httptest.Server
	mu              sync.Mutex
	requests        []requestedSDKRequest
	failures        []string
	failureOverflow int
	overflow        int
}

func newRequestedSDKServer(t *testing.T) *requestedSDKServer {
	t.Helper()
	server := &requestedSDKServer{}
	server.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, requestedSDKMaxBodySize+1))
		if err != nil {
			server.recordFailure(fmt.Sprintf("read request body: %v", err))
			http.Error(w, "request body unavailable", http.StatusBadRequest)
			return
		}
		if len(body) > requestedSDKMaxBodySize {
			server.recordFailure("request body exceeded fake provider limit")
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var request requestedSDKRequest
		if err := json.Unmarshal(body, &request); err != nil {
			server.recordFailure(fmt.Sprintf("decode request JSON: %v", err))
			http.Error(w, "invalid request JSON", http.StatusBadRequest)
			return
		}
		request.Method = r.Method
		request.Path = r.URL.Path

		server.mu.Lock()
		if len(server.requests) < requestedSDKMaxRequests {
			server.requests = append(server.requests, request)
		} else {
			server.overflow++
		}
		requestCount := len(server.requests) + server.overflow
		server.mu.Unlock()
		if requestCount > requestedSDKMaxRequests {
			server.recordFailure("fake provider request limit exceeded")
			http.Error(w, "fake provider request limit exceeded", http.StatusTooManyRequests)
			return
		}
		if request.Method != http.MethodPost || (request.Path != "/chat/completions" && request.Path != "/v1/chat/completions") {
			server.recordFailure(fmt.Sprintf("unexpected fake provider endpoint %s %s", request.Method, request.Path))
			http.NotFound(w, r)
			return
		}
		sseChunks(t, w, []map[string]any{textChunk("probe", "REQUESTED_ROLE_OK"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(server.srv.Close)
	return server
}

func (s *requestedSDKServer) recordFailure(failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failures) < requestedSDKMaxRequests {
		s.failures = append(s.failures, failure)
	} else {
		s.failureOverflow++
	}
}

func (s *requestedSDKServer) failuresSnapshot() ([]string, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.failures...), s.failureOverflow, s.overflow
}

func assertRequestedSDKServerClean(t *testing.T, server *requestedSDKServer) {
	t.Helper()
	failures, failureOverflow, requestOverflow := server.failuresSnapshot()
	require.Zero(t, requestOverflow, "fake provider request limit exceeded")
	require.Zero(t, failureOverflow, "fake provider error capture limit exceeded")
	require.Empty(t, failures, "fake provider errors")
	for _, request := range server.snapshot() {
		require.Equal(t, http.MethodPost, request.Method, "unexpected fake provider method")
		require.True(t, request.Path == "/chat/completions" || request.Path == "/v1/chat/completions",
			"unexpected fake provider path: %s", request.Path)
	}
}

func (s *requestedSDKServer) snapshot() []requestedSDKRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]requestedSDKRequest{}, s.requests...)
}

func isolateRequestedSDKConfig(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	t.Setenv("RUSH_GLOBAL_DATA", root)
	configDir := filepath.Join(root, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
}

func writeRequestedSDKConfig(t *testing.T, workDir, baseURL string) {
	t.Helper()
	configDoc := map[string]any{
		"options": map[string]any{"data_directory": ".rush", "disable_default_providers": true},
		"providers": map[string]any{
			"probe": map[string]any{
				"id": "probe", "name": "probe", "type": "openai-compat",
				"base_url": baseURL, "api_key": "probe-key", "discover_models": false,
				"models": []map[string]any{
					{"id": "smart-model", "name": "smart-model", "context_window": 200000, "default_max_tokens": 1000},
					{"id": "fast-model", "name": "fast-model", "context_window": 200000, "default_max_tokens": 1000},
					{"id": "custom-role-model", "name": "custom-role-model", "context_window": 200000, "default_max_tokens": 1000},
				},
			},
		},
		"models": map[string]any{
			"smart": map[string]string{"provider": "probe", "model": "smart-model"},
			"fast":  map[string]string{"provider": "probe", "model": "fast-model"},
		},
		"roles": map[string]any{
			requestedSDKRole: map[string]any{
				"provider": "probe", "model": "custom-role-model", "tools": []string{"view"},
			},
		},
	}
	encoded, err := json.Marshal(configDoc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), encoded, 0o644))
}

func openRequestedSDKClient(t *testing.T, workDir string) *sdk.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	client, err := sdk.Open(ctx, sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestClientRunRequestedRoleUsesExactUnicodeProfile(t *testing.T) {
	isolateRequestedSDKConfig(t)
	provider := newRequestedSDKServer(t)
	workDir := t.TempDir()
	writeRequestedSDKConfig(t, workDir, provider.srv.URL)
	client := openRequestedSDKClient(t, workDir)
	const sessionID = "sdk-requested-role-exact"
	seedTenantSession(t, workDir, sessionID)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	result, err := client.Run(ctx, sdk.RunRequest{
		Prompt: "reply with the marker", Mode: sdk.RunModeJSON,
		ContinueSessionID: sessionID, HideSpinner: true,
		Overrides: sdk.RunOverrides{RequestedRole: requestedSDKRole},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "REQUESTED_ROLE_OK", result.FinalText)

	requests := provider.snapshot()
	require.NotEmpty(t, requests, "the local fake provider must observe the SDK's outgoing request")
	assertRequestedSDKServerClean(t, provider)
	for _, request := range requests {
		require.Equal(t, "custom-role-model", request.Model,
			"the exact custom profile must drive the outgoing model, not the distinct configured smart slot")
	}
	var toolNames []string
	for _, tool := range requests[0].Tools {
		toolNames = append(toolNames, tool.Function.Name)
	}
	require.Contains(t, toolNames, "view")
	require.NotContains(t, toolNames, "bash", "the custom role tool ceiling must shape the actual provider schema")

	dataDir := filepath.Join(workDir, ".rush")
	dbCtx, dbCancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(dbCancel)
	conn, err := db.Connect(dbCtx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	store, ok := session.NewService(db.New(conn), conn).(session.SessionRootRoleContractStore)
	require.True(t, ok)
	logicalID, contract, found, err := store.GetRootRoleContract(dbCtx, result.SessionID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, logicalID)
	require.NotNil(t, contract)
	require.Equal(t, requestedSDKRole, contract.Name)
	require.Equal(t, "custom-role-model", contract.Model.Model)
	require.NotNil(t, contract.Tools)
	require.Equal(t, []string{"view"}, *contract.Tools)
}

func TestClientRunWithCredentialsMissingRequestedRoleFailsBeforeEffects(t *testing.T) {
	isolateRequestedSDKConfig(t)
	operator := newRequestedSDKServer(t)
	tenant := newRequestedSDKServer(t)
	workDir := t.TempDir()
	writeRequestedSDKConfig(t, workDir, operator.srv.URL)
	client := openRequestedSDKClient(t, workDir)

	// Use the strict-credential helper used by the SDK's isolation tests,
	// but deliberately omit this configured custom slot from tenant models.
	h := &strictHarness{tenant: &credentialServer{srv: tenant.srv}}
	creds := strictCreds(h, validStrictModels(), false)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	const sessionID = "sdk-requested-role-tenant-missing"
	_, err := client.RunWithCredentials(ctx, sdk.RunRequest{
		Prompt: "reply with the marker", Mode: sdk.RunModeJSON,
		ContinueSessionID: sessionID, HideSpinner: true,
		Overrides: sdk.RunOverrides{RequestedRole: requestedSDKRole},
	}, creds)
	require.ErrorContains(t, err, `credential set does not cover requested custom role "`+requestedSDKRole+`"`)

	_, sessionErr := client.Session(ctx, sessionID)
	require.ErrorIs(t, sessionErr, sql.ErrNoRows, "missing tenant role coverage must fail before creating a session")

	assertRequestedSDKServerClean(t, operator)
	assertRequestedSDKServerClean(t, tenant)
	require.Empty(t, operator.snapshot(), "missing tenant role coverage must not cross to the configured operator provider")
	require.Empty(t, tenant.snapshot(), "missing tenant role coverage must fail before tenant provider traffic")
}

func TestClientRunRequestedRoleUnknownAndWrongCaseFailBeforeEffects(t *testing.T) {
	isolateRequestedSDKConfig(t)
	provider := newRequestedSDKServer(t)
	workDir := t.TempDir()
	writeRequestedSDKConfig(t, workDir, provider.srv.URL)
	client := openRequestedSDKClient(t, workDir)

	for _, tc := range []struct {
		role      string
		sessionID string
	}{
		{role: "missing profile", sessionID: "sdk-requested-role-missing"},
		{role: "rôle research α", sessionID: "sdk-requested-role-wrong-case"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			t.Cleanup(cancel)
			_, err := client.Run(ctx, sdk.RunRequest{
				Prompt: "reply with the marker", Mode: sdk.RunModeJSON,
				ContinueSessionID: tc.sessionID, HideSpinner: true,
				Overrides: sdk.RunOverrides{RequestedRole: tc.role},
			})
			require.Error(t, err)
			_, sessionErr := client.Session(ctx, tc.sessionID)
			require.Error(t, sessionErr, "an unavailable exact role must fail before creating a session")
		})
	}
	assertRequestedSDKServerClean(t, provider)
	require.Empty(t, provider.snapshot(), "unknown and wrong-case role selections must fail before provider traffic")
}
