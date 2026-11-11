package cmd

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
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// TestRunCustomRoleProviderDispatch exercises the real top-level Cobra run
// command. Its local OpenAI-compatible endpoint observes provider requests;
// no external or paid model call is possible.
func TestRunCustomRoleProviderDispatch(t *testing.T) {
	ktavGateOn(t)
	originalAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = originalAvailable })

	for _, tc := range []struct {
		name       string
		role       string
		roleConfig string
		wantModel  string
		deleteRole bool
	}{
		{
			name: "unconfigured Research Analyst is refused before effects",
			role: "Research Analyst",
		},
		{
			name:       "exact configured name dispatches custom model",
			role:       "Research Analyst",
			roleConfig: `"Research Analyst":{"provider":"local","model":"custom-only-model"}`,
			wantModel:  "custom-only-model",
		},
		{
			name:       "unknown name is refused before effects",
			role:       "Unknown Analyst",
			roleConfig: `"Research Analyst":{"provider":"local","model":"custom-only-model"}`,
		},
		{
			name:       "wrong case is refused before effects",
			role:       "research analyst",
			roleConfig: `"Research Analyst":{"provider":"local","model":"custom-only-model"}`,
		},
		{
			name:       "deleted name is refused before effects",
			role:       "Research Analyst",
			roleConfig: `"Research Analyst":{"provider":"local","model":"custom-only-model"}`,
			deleteRole: true,
		},
		{
			name:       "incomplete profile is refused before effects",
			role:       "Research Analyst",
			roleConfig: `"Research Analyst":{"provider":"local"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu               sync.Mutex
				requests         []runCLIFakeProviderRequest
				overflowRequests int
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				recorded := runCLIFakeProviderRequest{
					method:    r.Method,
					path:      r.URL.Path,
					hasPrompt: bytes.Contains(body, []byte("test custom role dispatch")),
				}
				if r.Method == http.MethodPost && (r.URL.Path == "/chat/completions" || r.URL.Path == "/v1/chat/completions") {
					var request struct {
						Model string `json:"model"`
					}
					if err := json.Unmarshal(body, &request); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					recorded.model = request.Model
				}
				mu.Lock()
				if len(requests) < 8 {
					requests = append(requests, recorded)
				} else {
					overflowRequests++
				}
				requestCount := len(requests) + overflowRequests
				mu.Unlock()
				if requestCount > 8 {
					http.Error(w, "fake provider request limit exceeded", http.StatusTooManyRequests)
					return
				}
				if recorded.method != http.MethodPost || recorded.model == "" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintln(w, `data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"custom role dispatched"},"finish_reason":null}]}`)
				_, _ = fmt.Fprintln(w, `data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				_, _ = fmt.Fprintln(w, "data: [DONE]")
			}))
			t.Cleanup(server.Close)

			cwd := t.TempDir()
			dataDir := filepath.Join(cwd, ".rush")
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			isolateRunCLIConfig(t)
			providersConfig := `"roles":{` + tc.roleConfig + `}`
			if tc.roleConfig == "" {
				providersConfig = `"roles":{}`
			}
			fixture := fmt.Sprintf(`{
  "options":{"disable_default_providers":true},
  "providers":{"local":{"id":"local","name":"Local","type":"openai-compat","base_url":%q,"api_key":"test-only","discover_models":false,"models":[
    {"id":"smart-only-model","name":"Smart","context_window":200000,"default_max_tokens":128},
    {"id":"custom-only-model","name":"Custom","context_window":200000,"default_max_tokens":128},
    {"id":"title-only-model","name":"Title","context_window":200000,"default_max_tokens":128}
  ]}},
  "models":{"smart":{"provider":"local","model":"smart-only-model"},"fast":{"provider":"local","model":"title-only-model"}},
  %s
}`, server.URL, providersConfig)
			// Keep the role models in this effective workspace config's
			// provider catalog; dispatch must not depend on unknown-model
			// passthrough behavior.
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(fixture), 0o600))
			if tc.deleteRole {
				store, err := config.Init(cwd, dataDir, false)
				require.NoError(t, err)
				role := store.Config().Roles[tc.role]
				require.NoError(t, store.SetCustomRole(config.ScopeWorkspace, tc.role, config.CustomRoleUpdate{
					Model: role.SelectedModel,
					Tools: role.Tools,
				}))
				require.NoError(t, store.RemoveCustomRole(config.ScopeWorkspace, tc.role))
				roles, err := store.CustomRolesForScope(config.ScopeWorkspace)
				require.NoError(t, err)
				require.NotContains(t, roles, tc.role)
				activeConfig := config.ActiveConfigFileName(filepath.Join(dataDir, "rush.json"))
				require.Equal(t, filepath.Join(dataDir, "rush.ktav"), activeConfig)
				require.FileExists(t, activeConfig)
			}

			stdout, runErr := executeTopLevelRun(t, cwd, dataDir, tc.role)
			mu.Lock()
			observedRequests := append([]runCLIFakeProviderRequest(nil), requests...)
			providerRequestOverflow := overflowRequests
			mu.Unlock()
			require.Zero(t, providerRequestOverflow, "fake provider recorder limit exceeded")
			if tc.wantModel != "" {
				require.NoError(t, runErr)
				var result struct {
					SessionID string `json:"session_id"`
				}
				require.NoError(t, json.Unmarshal(stdout, &result), "run stdout: %s", stdout)
				require.Equal(t, "custom-role-cli-session", result.SessionID)
				rootRequestFound := false
				for _, request := range observedRequests {
					require.Equal(t, http.MethodPost, request.method, "unexpected non-completion provider request: %+v", request)
					require.True(t, request.path == "/chat/completions" || request.path == "/v1/chat/completions",
						"unexpected provider endpoint: %+v", request)
					if request.model == tc.wantModel && request.hasPrompt {
						rootRequestFound = true
					}
					require.NotEqual(t, "smart-only-model", request.model,
						"no request should be dispatched to the distinct smart model; observed=%+v", observedRequests)
				}
				require.True(t, rootRequestFound, "top-level prompt did not reach custom model; observed=%+v", observedRequests)

				conn, err := db.Connect(context.Background(), dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
				store := session.NewService(db.New(conn), conn).(session.SessionRootRoleContractStore)
				_, contract, found, err := store.GetRootRoleContract(context.Background(), result.SessionID)
				require.NoError(t, err)
				require.True(t, found, "the admitted custom role must be durably bound to the root session")
				require.Equal(t, tc.role, contract.Name)
				require.Equal(t, tc.wantModel, contract.Model.Model)
				return
			}

			require.Error(t, runErr, "parser acceptance is not configured-role admission")
			require.Empty(t, observedRequests, "invalid custom roles must fail before provider effects: %+v", observedRequests)
			require.Zero(t, countRunCLISessions(t, dataDir), "invalid custom roles must fail before session creation")
		})
	}
}

type runCLIFakeProviderRequest struct {
	method    string
	path      string
	model     string
	hasPrompt bool
}

func isolateRunCLIConfig(t *testing.T) {
	t.Helper()
	globalData := t.TempDir()
	globalConfig := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", globalData)
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("XDG_DATA_HOME", globalData)
	t.Setenv("XDG_CONFIG_HOME", globalConfig)
	t.Setenv("RUSH_GLOBAL_DIR", "")
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	rushlog.Setup("", false)
}

func executeTopLevelRun(t *testing.T, cwd, dataDir, role string) ([]byte, error) {
	t.Helper()
	originalCwd, err := os.Getwd()
	require.NoError(t, err)
	previousStdout := os.Stdout
	previousOut, previousErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	previousContext := rootCmd.Context()
	defer func() {
		os.Stdout = previousStdout
		_ = os.Chdir(originalCwd)
		rootCmd.SetOut(previousOut)
		rootCmd.SetErr(previousErr)
		rootCmd.SetContext(previousContext)
		rootCmd.SetArgs(nil)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rootCmd.SetContext(ctx)

	restore := journalSnapshotRestore(t, runCmd.Flags(), rootCmd.PersistentFlags())
	t.Cleanup(restore)
	for _, flags := range []*pflag.FlagSet{runCmd.Flags(), rootCmd.PersistentFlags()} {
		flags.VisitAll(func(flag *pflag.Flag) {
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		})
	}
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	read, write, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = write
	defer func() {
		_ = write.Close()
		_ = read.Close()
	}()
	captured := make(chan []byte, 1)
	go func() {
		output, _ := io.ReadAll(io.LimitReader(read, 1<<20))
		_, _ = io.Copy(io.Discard, read)
		captured <- output
	}()
	rootCmd.SetArgs([]string{
		"--cwd", cwd, "--data-dir", dataDir,
		"run", "--role", role, "--json", "--session", "custom-role-cli-session",
		"--timeout", "10s", "test custom role dispatch",
	})
	runErr := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	_ = write.Close()
	output := <-captured
	os.Stdout = previousStdout
	_ = read.Close()
	return output, runErr
}

func countRunCLISessions(t *testing.T, dataDir string) int {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&count))
	return count
}
