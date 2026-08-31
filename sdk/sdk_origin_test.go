package sdk_test

// Origin-stamping tests for sdk.Client.Run / RunWithCredentials: every run
// entered through the SDK must stamp message.OriginSDK on the session it
// resolves (get-or-create via CreateWithIDAndOrigin) and on the user message
// it persists for the prompt.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// originServer answers every chat-completions request with a single-round,
// tool-free assistant message carrying finalText (minimal copy of the
// credentials test's SSE provider).
type originServer struct {
	srv *httptest.Server
}

func newOriginServer(t *testing.T, finalText string) *originServer {
	t.Helper()
	s := &originServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

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
	t.Cleanup(s.srv.Close)
	return s
}

// openSDKClient isolates the global config (same env block as the
// credentials test) and opens a client whose workspace config points the
// smart/fast selection at the given provider server. Returns the client and
// the workspace dir (which hosts the .rush store to read back from).
func openSDKClient(t *testing.T, baseURL string) (*sdk.Client, string) {
	t.Helper()

	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

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
}`, baseURL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })
	return client, workDir
}

// runExpectEndTurn shapes a failed run into a descriptive error (same
// pattern as the credentials test's runTenant).
func runExpectEndTurn(res *sdk.RunResult, err error, buf *bytes.Buffer) (*sdk.RunResult, error) {
	if err != nil {
		return nil, fmt.Errorf("run failed: %w (output %q)", err, buf.String())
	}
	if res == nil {
		return nil, fmt.Errorf("nil envelope")
	}
	if res.ExitReason != "end_turn" {
		return res, fmt.Errorf("exit_reason=%q error=%q warnings=%v output=%q", res.ExitReason, res.Error, res.Warnings, buf.String())
	}
	return res, nil
}

// assertSDKOrigin read-only connects to the workspace's .rush store and
// asserts the session AND its user-role message carry OriginSDK.
func assertSDKOrigin(t *testing.T, workDir, sessionID string) {
	t.Helper()
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, message.OriginSDK, sess.Origin, "session resolved via the SDK must be stamped OriginSDK")

	msgs, err := message.NewService(q).List(context.Background(), sessionID)
	require.NoError(t, err)
	var userMsg *message.Message
	for i := range msgs {
		if msgs[i].Role == message.User {
			userMsg = &msgs[i]
		}
	}
	require.NotNil(t, userMsg, "the run must persist a user-role message for the prompt")
	require.Equal(t, message.OriginSDK, userMsg.Origin, "the user message must be stamped OriginSDK")
}

func TestClientRunMarksSDKOrigin(t *testing.T) {
	server := newOriginServer(t, "SDK_ORIGIN_RUN_OK")
	client, workDir := openSDKClient(t, server.srv.URL)

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "reply with exactly the marker text and nothing else",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: "sdk-origin-run",
		Stdout:            &buf,
		HideSpinner:       true,
	})
	res, err = runExpectEndTurn(res, err, &buf)
	require.NoError(t, err)
	require.Equal(t, "SDK_ORIGIN_RUN_OK", res.FinalText)

	// The fresh ContinueSessionID exercises resolveSession's get-or-create
	// path (CreateWithIDAndOrigin) — the stamp must survive to the store.
	assertSDKOrigin(t, workDir, "sdk-origin-run")
}

func TestRunWithCredentialsMarksSDKOrigin(t *testing.T) {
	server := newOriginServer(t, "SDK_ORIGIN_CREDS_OK")
	client, workDir := openSDKClient(t, server.srv.URL)

	creds := sdk.CredentialSet{
		Credentials: []sdk.Credential{
			{
				Provider: "tenant-provider",
				Type:     sdk.ProviderTypeOpenAICompat,
				APIKey:   "sk-origin-creds-secret",
				BaseURL:  server.srv.URL,
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

	var buf bytes.Buffer
	res, err := client.RunWithCredentials(context.Background(), sdk.RunRequest{
		Prompt:            "reply with exactly the marker text and nothing else",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: "sdk-origin-creds",
		Stdout:            &buf,
		HideSpinner:       true,
	}, creds)
	res, err = runExpectEndTurn(res, err, &buf)
	require.NoError(t, err)
	require.Equal(t, "SDK_ORIGIN_CREDS_OK", res.FinalText)

	assertSDKOrigin(t, workDir, "sdk-origin-creds")
}
