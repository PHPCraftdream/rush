package clie2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	service "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	openai "github.com/charmbracelet/openai-go"
	"github.com/charmbracelet/openai-go/packages/ssestream"
	"github.com/stretchr/testify/require"
)

type runCLIFakeProviderRequest struct {
	method, path, model    string
	hasPrompt, titlePrompt bool
}

// This is the original top-level custom-role dispatch matrix, now executed by
// cli.Execute in an isolated test subprocess instead of package cmd's globals.
func TestRunCustomRoleProviderDispatch(t *testing.T) {
	cmdtest.KtavGateOn(t)
	cmdtest.KtavIsolateGlobalEnv(t)
	for _, tc := range []struct {
		name, role, roleConfig, wantModel string
		deleteRole                        bool
	}{
		{"unconfigured Research Analyst is refused before effects", "Research Analyst", "", "", false},
		{"exact configured name dispatches custom model", "Research Analyst", `"Research Analyst":{"provider":"local","model":"custom-only-model"}`, "custom-only-model", false},
		{"unknown name is refused before effects", "Unknown Analyst", `"Research Analyst":{"provider":"local","model":"custom-only-model"}`, "", false},
		{"wrong case is refused before effects", "research analyst", `"Research Analyst":{"provider":"local","model":"custom-only-model"}`, "", false},
		{"deleted name is refused before effects", "Research Analyst", `"Research Analyst":{"provider":"local","model":"custom-only-model"}`, "", true},
		{"incomplete profile is refused before effects", "Research Analyst", `"Research Analyst":{"provider":"local"}`, "", false},
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
					http.Error(w, "read failed", http.StatusBadRequest)
					return
				}
				request := runCLIFakeProviderRequest{
					method: r.Method, path: r.URL.Path,
					hasPrompt:   bytes.Contains(body, []byte(runRoleChildPrompt)),
					titlePrompt: bytes.Contains(body, []byte(runRoleChildTitlePrompt)),
				}
				if r.Method == http.MethodPost && (r.URL.Path == "/chat/completions" || r.URL.Path == "/v1/chat/completions") {
					var payload struct {
						Model string `json:"model"`
					}
					if json.Unmarshal(body, &payload) != nil {
						http.Error(w, "bad JSON", 400)
						return
					}
					request.model = payload.Model
				}
				mu.Lock()
				if len(requests) < 8 {
					requests = append(requests, request)
				} else {
					overflowRequests++
				}
				requestCount := len(requests) + overflowRequests
				mu.Unlock()
				if requestCount > 8 {
					http.Error(w, "request limit", 429)
					return
				}
				if request.method != http.MethodPost || request.model == "" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = writeRunCustomRoleSSE(w)
			}))
			t.Cleanup(server.Close)
			base := t.TempDir()
			cwd, dataDir := base, filepath.Join(base, ".rush")
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			roles := tc.roleConfig
			fixture := fmt.Sprintf(`{"options":{"disable_default_providers":true},"providers":{"local":{"id":"local","name":"Local","type":"openai-compat","base_url":%q,"api_key":"test-only","discover_models":false,"models":[{"id":"smart-only-model","name":"Smart","context_window":200000,"default_max_tokens":128},{"id":"custom-only-model","name":"Custom","context_window":200000,"default_max_tokens":128},{"id":"title-only-model","name":"Title","context_window":200000,"default_max_tokens":128}]}},"models":{"smart":{"provider":"local","model":"smart-only-model"},"fast":{"provider":"local","model":"title-only-model"}},"roles":{%s}}`, server.URL, roles)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(fixture), 0o600))
			if tc.deleteRole {
				oldAll, oldAvailable := cliprovider.All, cliprovider.AvailableFunc
				cliprovider.All = []cliprovider.CLISpec{{ModelID: "custom-only-model", ModelName: "Custom"}}
				cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return cliprovider.All }
				store, err := config.Init(cwd, dataDir, false, config.WithCLIModels(cliprovider.AvailableCLIModels))
				cliprovider.All, cliprovider.AvailableFunc = oldAll, oldAvailable
				require.NoError(t, err)
				role := store.Config().Roles[tc.role]
				require.NoError(t, store.SetCustomRole(config.ScopeWorkspace, tc.role, config.CustomRoleUpdate{Model: role.SelectedModel, Tools: role.Tools}))
				require.NoError(t, store.RemoveCustomRole(config.ScopeWorkspace, tc.role))
				roles, err := store.CustomRolesForScope(config.ScopeWorkspace)
				require.NoError(t, err)
				require.NotContains(t, roles, tc.role)
				activeConfig := config.ActiveConfigFileName(filepath.Join(dataDir, "rush.json"))
				require.Equal(t, filepath.Join(dataDir, "rush.ktav"), activeConfig)
				require.FileExists(t, activeConfig)
				disk, readErr := config.ReadConfigDocumentLight(filepath.Join(dataDir, "rush.json"))
				require.NoError(t, readErr)
				var document struct {
					Roles map[string]json.RawMessage `json:"roles"`
				}
				require.NoError(t, json.Unmarshal(disk, &document))
				require.NotContains(t, document.Roles, tc.role, "active config document %s must persist role deletion", activeConfig)
			}
			globalData, globalConfig := filepath.Join(base, "global-data"), filepath.Join(base, "global-config")
			for _, dir := range []string{globalData, globalConfig} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			args := []string{"--cwd", cwd, "--data-dir", dataDir, "run", "--role", tc.role, "--json", "--session", runRoleChildSession, "--timeout", "10s", runRoleChildPrompt}
			stdout, stderr, runErr := runCLIProcess(t, cwd, cliE2EDispatch, args, map[string]string{
				"RUSH_GLOBAL_DIR": "", "RUSH_GLOBAL_DATA": globalData, "RUSH_GLOBAL_CONFIG": globalConfig,
				"XDG_DATA_HOME": globalData, "XDG_CONFIG_HOME": globalConfig, "RUSH_PROVIDER_CACHE_ONLY": "1",
			})
			mu.Lock()
			observed := append([]runCLIFakeProviderRequest(nil), requests...)
			providerRequestOverflow := overflowRequests
			mu.Unlock()
			require.Zero(t, providerRequestOverflow, "fake provider recorder limit exceeded")
			if tc.wantModel != "" {
				require.NoError(t, runErr, "stderr: %s", stderr)
				var result struct {
					SessionID  string `json:"session_id"`
					FinalText  string `json:"final_text"`
					ExitReason string `json:"exit_reason"`
					Error      string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(stdout, &result), "stdout: %s stderr: %s", stdout, stderr)
				require.Equal(t, runRoleChildSession, result.SessionID)
				require.Equal(t, "custom role dispatched", result.FinalText)
				require.Equal(t, "end_turn", result.ExitReason)
				require.Empty(t, result.Error)
				found := false
				for _, req := range observed {
					require.Equal(t, http.MethodPost, req.method)
					require.True(t, req.path == "/chat/completions" || req.path == "/v1/chat/completions")
					if runCLIRequestClass(req) == "root" {
						require.Equal(t, tc.wantModel, req.model, "every non-title top-level prompt must dispatch to the selected custom model: %+v", observed)
						found = true
					}
					require.NotEqual(t, "smart-only-model", req.model)
				}
				require.True(t, found, "top-level prompt missed custom model: %+v", observed)
				conn, err := db.Connect(context.Background(), dataDir)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
				store := service.NewService(db.New(conn), conn).(service.SessionRootRoleContractStore)
				_, contract, found, err := store.GetRootRoleContract(context.Background(), result.SessionID)
				require.NoError(t, err)
				require.True(t, found, "the admitted custom role must be durably bound to the root session")
				require.Equal(t, tc.role, contract.Name)
				require.Equal(t, tc.wantModel, contract.Model.Model)
			} else {
				require.Error(t, runErr, "stderr: %s", stderr)
				require.Empty(t, observed, "invalid role caused provider effects: %+v", observed)
				require.Zero(t, countRunCLISessions(t, dataDir))
			}
		})
	}
}

func runCLIEndpointClass(path string) string {
	if path == "/chat/completions" || path == "/v1/chat/completions" {
		return "completion"
	}
	return "other"
}

func runCLIRequestClass(r runCLIFakeProviderRequest) string {
	if r.titlePrompt {
		return "title"
	}
	if r.hasPrompt {
		return "root"
	}
	return "other"
}

func runCLIModelClass(model string) string {
	switch model {
	case "custom-only-model":
		return "custom"
	case "smart-only-model":
		return "smart"
	case "title-only-model":
		return "title"
	case "":
		return "missing"
	default:
		return "other"
	}
}

func TestRunCLICommandErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{nil, "nil"}, {errors.New("run timeout 10s exceeded (source: --timeout)"), "owned-timeout"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"}, {errors.New("configured role was refused"), "other"}} {
		require.Equal(t, tc.want, runCLICommandErrorClass(tc.err))
	}
}

func runCLICommandErrorClass(err error) string {
	switch {
	case err == nil:
		return "nil"
	case strings.HasPrefix(strings.ToLower(err.Error()), "run timeout "):
		return "owned-timeout"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}

func TestRunCLIRequestClassPrioritizesTitlePrompt(t *testing.T) {
	require.Equal(t, "title", runCLIRequestClass(runCLIFakeProviderRequest{hasPrompt: true, titlePrompt: true}))
}

func runCustomRoleSSEFrames() []string {
	return []string{
		`data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"custom role dispatched"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
}

func writeRunCustomRoleSSE(w io.Writer) (int, error) {
	n := 0
	for _, frame := range runCustomRoleSSEFrames() {
		if _, err := io.WriteString(w, frame); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func TestRunCustomRoleSSEFramesDecodeAsOpenAIStream(t *testing.T) {
	var body bytes.Buffer
	written, err := writeRunCustomRoleSSE(&body)
	require.NoError(t, err)
	require.Equal(t, 3, written)
	decoder := ssestream.NewDecoder(&http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(body.Bytes()))})
	defer func() { require.NoError(t, decoder.Close()) }()
	var text strings.Builder
	var finish string
	var usage openai.CompletionUsage
	var kinds []string
	count := 0
	for decoder.Next() {
		count++
		data := strings.TrimSpace(string(decoder.Event().Data))
		if data == "[DONE]" {
			kinds = append(kinds, "done")
			continue
		}
		var chunk openai.ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(data), &chunk))
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
			if chunk.Choices[0].FinishReason != "" {
				finish = chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage.TotalTokens != 0 {
			usage = chunk.Usage
			kinds = append(kinds, "usage")
		} else {
			kinds = append(kinds, "completion")
		}
	}
	require.NoError(t, decoder.Err())
	require.Equal(t, 3, count)
	require.Equal(t, []string{"completion", "usage", "done"}, kinds)
	require.Equal(t, "custom role dispatched", text.String())
	require.Equal(t, "stop", finish)
	require.EqualValues(t, 1, usage.PromptTokens)
	require.EqualValues(t, 1, usage.CompletionTokens)
	require.EqualValues(t, 2, usage.TotalTokens)
}
