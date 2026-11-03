package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchFixture is the production serve listener (NewServeAPI + Configure +
// NewWithPolicy + a real token store) over a real App whose model is an
// in-process SSE provider driven by handler.
type batchFixture struct {
	base    string
	app     *appPkg.App
	store   *config.APITokenStore
	service *apiBatchService
	// workspace is the workspace the listener serves.
	workspace string
	cfg       *config.ConfigStore
}

func batchSSEText(text string) string {
	return fmt.Sprintf("data: %s\n\ndata: %s\n\ndata: [DONE]\n\n",
		fmt.Sprintf(`{"id":"b","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, text),
		`{"id":"b","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
}

func batchTextProvider(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, batchSSEText(text))
	}
}

func newBatchFixture(t *testing.T, provider http.HandlerFunc) *batchFixture {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	provSrv := httptest.NewServer(provider)
	t.Cleanup(provSrv.Close)
	cfg, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID: "openaicompat", Type: openaicompat.Name, BaseURL: provSrv.URL, APIKey: "probe",
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	cfg.SetupAgents()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseAll(dataDir) })
	t.Cleanup(func() { session.ReleaseRetainedHostLocksForTest() })
	application, err := appPkg.New(context.Background(), conn, cfg)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	store := config.GlobalAPITokenStore()
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Workspace: dataDir,
		Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		Limits:   ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000},
	}
	api, err := NewServeAPI(policy, application, store, nil)
	require.NoError(t, err)
	api.Configure(&policy)
	base := startPolicyServer(t, NewWithPolicy(application, policy, nil))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, api.Stop(ctx))
	})
	apiBatchServices.Lock()
	service := apiBatchServices.byApp[application]
	apiBatchServices.Unlock()
	require.NotNil(t, service)
	return &batchFixture{base: base, app: application, store: store, service: service, workspace: dataDir, cfg: cfg}
}

func (f *batchFixture) token(t *testing.T, name string, scopes []string, permission string, binding config.APITokenBinding) string {
	t.Helper()
	token, _, err := f.store.CreateAPITokenWithBinding(name, scopes, permission, nil, binding)
	require.NoError(t, err)
	return token
}

type batchResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (f *batchFixture) do(t *testing.T, token, method, path string, body any, headers ...string) batchResponse {
	t.Helper()
	var reader io.Reader
	switch value := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(value)
	default:
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, f.base+"/api/v1"+path, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return batchResponse{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

func (r batchResponse) accepted(t *testing.T) apiBatchAccepted {
	t.Helper()
	require.Equal(t, http.StatusAccepted, r.Status, string(r.Body))
	var out apiBatchAccepted
	require.NoError(t, json.Unmarshal(r.Body, &out))
	require.NotEmpty(t, out.RootID)
	return out
}

func (r batchResponse) batch(t *testing.T) apiBatch {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, string(r.Body))
	var out apiBatch
	require.NoError(t, json.Unmarshal(r.Body, &out))
	return out
}

// waitBatchDone polls the batch read route until the root is terminal.
func (f *batchFixture) waitBatchDone(t *testing.T, token, root string) apiBatch {
	t.Helper()
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := f.do(t, token, http.MethodGet, "/batches/"+root, nil).batch(t)
		if current.Done {
			return current
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			require.FailNow(t, "batch did not finish", "state=%s", current.State)
		}
	}
}
