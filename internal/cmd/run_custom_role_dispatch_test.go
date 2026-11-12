package cmd

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
	"time"

	openai "github.com/charmbracelet/openai-go"
	"github.com/charmbracelet/openai-go/packages/ssestream"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/session"
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
			trace := newRunCustomRoleDispatchTrace()
			t.Cleanup(func() {
				if t.Failed() {
					requestStages, droppedRequests, executeStages, droppedExecute := trace.snapshot()
					t.Logf("custom-role dispatch failure trace: provider_stages=%q provider_stages_dropped=%d execute_stages=%q execute_stages_dropped=%d", requestStages, droppedRequests, executeStages, droppedExecute)
				}
			})
			var (
				mu               sync.Mutex
				requests         []runCLIFakeProviderRequest
				overflowRequests int
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestID := trace.requestStage("handler entered", runCLIEndpointClass(r.URL.Path))
				defer func() {
					if r.Context().Err() != nil {
						trace.requestStageFor(requestID, "context canceled")
					}
				}()
				body, err := io.ReadAll(r.Body)
				if err != nil {
					trace.requestStageFor(requestID, "request read failed")
					http.Error(w, "request read failed", http.StatusBadRequest)
					return
				}
				trace.requestStageFor(requestID, fmt.Sprintf("request read done bytes=%d", len(body)))
				recorded := runCLIFakeProviderRequest{
					method:      r.Method,
					path:        r.URL.Path,
					hasPrompt:   bytes.Contains(body, []byte("test custom role dispatch")),
					titlePrompt: bytes.Contains(body, []byte("Generate a concise title for the following content:")),
				}
				if r.Method == http.MethodPost && runCLIEndpointClass(r.URL.Path) == "completion" {
					var request struct {
						Model string `json:"model"`
					}
					if err := json.Unmarshal(body, &request); err != nil {
						trace.requestStageFor(requestID, "request classification=invalid-json")
						http.Error(w, "invalid completion request", http.StatusBadRequest)
						return
					}
					recorded.model = request.Model
				}
				trace.requestStageFor(requestID, fmt.Sprintf("request classification=%s model_class=%s endpoint=%s", runCLIRequestClass(recorded), runCLIModelClass(recorded.model), runCLIEndpointClass(recorded.path)))
				mu.Lock()
				if len(requests) < 8 {
					requests = append(requests, recorded)
				} else {
					overflowRequests++
				}
				requestCount := len(requests) + overflowRequests
				mu.Unlock()
				if requestCount > 8 {
					trace.requestStageFor(requestID, "response rejected request limit")
					http.Error(w, "fake provider request limit exceeded", http.StatusTooManyRequests)
					return
				}
				if recorded.method != http.MethodPost || recorded.model == "" {
					trace.requestStageFor(requestID, "response rejected non-completion request")
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				trace.requestStageFor(requestID, "response writes starting")
				written, writeErr := writeRunCustomRoleSSE(w)
				if writeErr != nil {
					trace.requestStageFor(requestID, fmt.Sprintf("response writes failed after frames=%d", written))
					return
				}
				trace.requestStageFor(requestID, fmt.Sprintf("response writes returned frames=%d", written))
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

			stdout, runErr := executeTopLevelRun(t, cwd, dataDir, tc.role, trace)
			mu.Lock()
			observedRequests := append([]runCLIFakeProviderRequest(nil), requests...)
			providerRequestOverflow := overflowRequests
			mu.Unlock()
			require.Zero(t, providerRequestOverflow, "fake provider recorder limit exceeded")
			if tc.wantModel != "" {
				require.NoError(t, runErr)
				var result struct {
					SessionID  string `json:"session_id"`
					FinalText  string `json:"final_text"`
					ExitReason string `json:"exit_reason"`
					Error      string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(stdout, &result), "run stdout must be a JSON envelope")
				require.Equal(t, "custom-role-cli-session", result.SessionID)
				require.Equal(t, "custom role dispatched", result.FinalText)
				require.Equal(t, "end_turn", result.ExitReason)
				require.Empty(t, result.Error)
				rootRequestFound := false
				for _, request := range observedRequests {
					require.Equal(t, http.MethodPost, request.method, "unexpected non-completion provider request: %+v", request)
					require.True(t, request.path == "/chat/completions" || request.path == "/v1/chat/completions",
						"unexpected provider endpoint: %+v", request)
					if runCLIRequestClass(request) == "root" && request.model == tc.wantModel {
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
	method      string
	path        string
	model       string
	hasPrompt   bool
	titlePrompt bool
}

const runCustomRoleDispatchTraceLimit = 64

type runCustomRoleDispatchTrace struct {
	mu              sync.Mutex
	started         time.Time
	nextRequest     int
	requestStages   []string
	droppedRequests int
	executeStages   []string
	droppedExecute  int
}

func newRunCustomRoleDispatchTrace() *runCustomRoleDispatchTrace {
	return &runCustomRoleDispatchTrace{started: time.Now()}
}

func (trace *runCustomRoleDispatchTrace) requestStage(stage, endpoint string) int {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.nextRequest++
	requestID := trace.nextRequest
	trace.recordBounded(&trace.requestStages, &trace.droppedRequests, fmt.Sprintf("request#%d %s endpoint=%s", requestID, stage, endpoint))
	return requestID
}

func (trace *runCustomRoleDispatchTrace) requestStageFor(requestID int, stage string) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.recordBounded(&trace.requestStages, &trace.droppedRequests, fmt.Sprintf("request#%d %s", requestID, stage))
}

func (trace *runCustomRoleDispatchTrace) executeStage(stage string) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.recordBounded(&trace.executeStages, &trace.droppedExecute, stage)
}

func (trace *runCustomRoleDispatchTrace) recordBounded(target *[]string, dropped *int, stage string) {
	if len(*target) == runCustomRoleDispatchTraceLimit {
		*dropped = *dropped + 1
		return
	}
	elapsed := time.Since(trace.started).Round(time.Millisecond).Milliseconds()
	*target = append(*target, fmt.Sprintf("+%dms %s", elapsed, stage))
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

func (trace *runCustomRoleDispatchTrace) snapshot() ([]string, int, []string, int) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.requestStages...), trace.droppedRequests,
		append([]string(nil), trace.executeStages...), trace.droppedExecute
}

func TestRunCLICommandErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{want: "nil"},
		{err: errors.New("run timeout 10s exceeded (source: --timeout)"), want: "owned-timeout"},
		{err: context.DeadlineExceeded, want: "deadline"},
		{err: context.Canceled, want: "canceled"},
		{err: errors.New("configured role was refused"), want: "other"},
	} {
		require.Equal(t, tc.want, runCLICommandErrorClass(tc.err))
	}
}

func runCLIEndpointClass(path string) string {
	if path == "/chat/completions" || path == "/v1/chat/completions" {
		return "completion"
	}
	return "other"
}

func runCLIRequestClass(request runCLIFakeProviderRequest) string {
	switch {
	case request.titlePrompt:
		return "title"
	case request.hasPrompt:
		return "root"
	default:
		return "other"
	}
}

func TestRunCLIRequestClassPrioritizesTitlePrompt(t *testing.T) {
	require.Equal(t, "title", runCLIRequestClass(runCLIFakeProviderRequest{
		hasPrompt:   true,
		titlePrompt: true,
	}))
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

func runCustomRoleSSEFrames() []string {
	return []string{
		`data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"custom role dispatched"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"cli-role","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
}

func writeRunCustomRoleSSE(w io.Writer) (int, error) {
	wrote := 0
	for _, frame := range runCustomRoleSSEFrames() {
		if _, err := io.WriteString(w, frame); err != nil {
			return wrote, err
		}
		wrote++
	}
	return wrote, nil
}

func TestRunCustomRoleSSEFramesDecodeAsOpenAIStream(t *testing.T) {
	var body bytes.Buffer
	var written int
	decodedEvents := 0
	decodedKinds := make([]string, 0, 3)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("SSE framing failure trace: response_bytes=%d frames_written=%d events_decoded=%d event_kinds=%q", body.Len(), written, decodedEvents, decodedKinds)
		}
	})
	var err error
	written, err = writeRunCustomRoleSSE(&body)
	require.NoError(t, err)
	require.Equal(t, 3, written)

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(bytes.NewReader(body.Bytes())),
	}
	decoder := ssestream.NewDecoder(response)
	defer func() { require.NoError(t, decoder.Close()) }()

	var text strings.Builder
	var finishReason string
	var usage openai.CompletionUsage
	for decoder.Next() {
		decodedEvents++
		event := decoder.Event()
		data := strings.TrimSpace(string(event.Data))
		switch {
		case data == "[DONE]":
			decodedKinds = append(decodedKinds, "done")
		case data != "":
			var chunk openai.ChatCompletionChunk
			require.NoError(t, json.Unmarshal([]byte(data), &chunk), "decoded SSE data event must be a chat completion chunk")
			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				text.WriteString(choice.Delta.Content)
				if choice.FinishReason != "" {
					finishReason = choice.FinishReason
				}
			}
			if chunk.Usage.TotalTokens != 0 {
				usage = chunk.Usage
				decodedKinds = append(decodedKinds, "usage")
			} else {
				decodedKinds = append(decodedKinds, "completion")
			}
		default:
			decodedKinds = append(decodedKinds, "empty")
		}
	}
	require.NoError(t, decoder.Err())
	require.Equal(t, 3, decodedEvents, "all three separately-delimited events, including [DONE], must decode")
	require.Equal(t, []string{"completion", "usage", "done"}, decodedKinds)
	require.Equal(t, "custom role dispatched", text.String())
	require.Equal(t, "stop", finishReason)
	require.EqualValues(t, 1, usage.PromptTokens)
	require.EqualValues(t, 1, usage.CompletionTokens)
	require.EqualValues(t, 2, usage.TotalTokens)
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

func executeTopLevelRun(t *testing.T, cwd, dataDir, role string, traces ...*runCustomRoleDispatchTrace) ([]byte, error) {
	t.Helper()
	var trace *runCustomRoleDispatchTrace
	if len(traces) > 0 {
		trace = traces[0]
	}
	recordExecute := func(stage string) {
		if trace != nil {
			trace.executeStage(stage)
		}
	}
	recordExecute("top-level command setup started")
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
	recordExecute("Cobra Execute entered")
	runErr := rootCmd.Execute()
	ctxState := "active"
	if ctx.Err() != nil {
		ctxState = "canceled-or-deadline"
	}
	recordExecute(fmt.Sprintf("Cobra Execute returned error_class=%s outer_context=%s", runCLICommandErrorClass(runErr), ctxState))
	rootCmd.SetArgs(nil)
	_ = write.Close()
	output := <-captured
	recordExecute(fmt.Sprintf("stdout capture completed bytes=%d", len(output)))
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
