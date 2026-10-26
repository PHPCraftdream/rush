package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type batchGenericTestTool struct {
	name string
	run  func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func (t *batchGenericTestTool) Info() fantasy.ToolInfo                   { return fantasy.ToolInfo{Name: t.name} }
func (*batchGenericTestTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (*batchGenericTestTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *batchGenericTestTool) Run(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.run(ctx, c)
}

func batchGenericTestExecutor(tool fantasy.AgentTool) *batchExecutor {
	family := newBatchGenericFamily()
	return newBatchExecutor(batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) {
		return &policyLeafRegistry{entries: map[string]policyLeafEntry{tool.Info().Name: {tool: tool}}}, nil
	}}, map[string]batchLeafFamily{batchKindGeneric: family, batchKindMCP: family})
}

func batchGenericTestStart(t *testing.T, e *batchExecutor, kind, name string) (jobbatch.Execution, chan jobbatch.Result) {
	t.Helper()
	payload, err := json.Marshal(batchLeafPayload{Tool: name, ToolCallID: "call", SessionID: t.Name(), Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	reports := make(chan jobbatch.Result, 2)
	execution, err := e.Start(context.Background(), jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: kind, Payload: payload}}, func(r jobbatch.Result) { reports <- r })
	require.NoError(t, err)
	return execution, reports
}

func batchGenericTestReport(t *testing.T, reports chan jobbatch.Result) jobbatch.Result {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("termination report missing")
		return jobbatch.Result{}
	}
}

func TestBatchGenericStopWaitsForReturn(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	tool := &batchGenericTestTool{name: "mcp_dynamic_call", run: func(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return fantasy.NewTextResponse("partial"), ctx.Err()
	}}
	e, reports := batchGenericTestStart(t, batchGenericTestExecutor(tool), batchKindMCP, tool.name)
	batchWait(t, entered)
	require.NoError(t, e.Stop(context.Background()))
	require.NoError(t, e.Stop(context.Background()))
	batchWait(t, cancelled)
	select {
	case <-reports:
		t.Fatal("Stop settled before Run returned")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := e.(jobbatch.OutputReader).ReadOutput(ctx, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	r := batchGenericTestReport(t, reports)
	require.Equal(t, jobbatch.StateCancelled, r.State)
	select {
	case <-reports:
		t.Fatal("duplicate report")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchGenericOutcomeAndOutput(t *testing.T) {
	image := fantasy.NewImageResponse([]byte{0, 255, 2}, "image/png")
	image.Content, image.Metadata, image.StopTurn = "image text", `{"policy":"image"}`, true
	audio := fantasy.NewMediaResponse([]byte{255, 0, 3}, "audio/wav")
	audio.Content, audio.Metadata = "audio text", `{"policy":"audio"}`
	for _, tc := range []struct {
		name     string
		err      error
		stop     bool
		response fantasy.ToolResponse
		state    jobbatch.State
	}{
		{"image_metadata", nil, false, image, jobbatch.StateCompleted},
		{"audio_metadata", nil, false, audio, jobbatch.StateCompleted},
		{"success", nil, false, fantasy.NewTextResponse("content"), jobbatch.StateCompleted},
		{"denied", nil, false, fantasy.NewTextErrorResponse("denied"), jobbatch.StateFailed},
		{"error", errors.New("failure"), false, fantasy.NewMediaResponse([]byte{1, 2}, "audio/wav"), jobbatch.StateFailed},
		{"cancel_without_stop", context.Canceled, false, fantasy.NewTextResponse("partial"), jobbatch.StateFailed},
		{"stop_success", nil, true, fantasy.NewTextResponse("content"), jobbatch.StateCompleted},
		{"stop_error", errors.New("failure"), true, fantasy.NewTextResponse("partial"), jobbatch.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			tool := &batchGenericTestTool{name: "view", run: func(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
				<-release
				return tc.response, tc.err
			}}
			e, reports := batchGenericTestStart(t, batchGenericTestExecutor(tool), batchKindGeneric, tool.name)
			if tc.stop {
				require.NoError(t, e.Stop(context.Background()))
			}
			close(release)
			r := batchGenericTestReport(t, reports)
			require.Equal(t, tc.state, r.State)
			var response fantasy.ToolResponse
			require.NoError(t, json.Unmarshal(r.Details, &response))
			require.Equal(t, tc.response, response)
			_, messenger := e.(jobbatch.Messenger)
			require.False(t, messenger)
			reader := e.(jobbatch.OutputReader)
			chunk, err := reader.ReadOutput(context.Background(), 0)
			require.NoError(t, err)
			require.Equal(t, tc.response.Content, string(chunk.Data))
			require.True(t, chunk.EOF)
			if len(chunk.Data) > 0 {
				chunk.Data[0] = 'X'
			}
			again, err := reader.ReadOutput(context.Background(), 0)
			require.NoError(t, err)
			require.Equal(t, tc.response.Content, string(again.Data))
			end, err := reader.ReadOutput(context.Background(), again.Next+10)
			require.NoError(t, err)
			require.Empty(t, end.Data)
			require.Equal(t, again.Next, end.Next)
			require.NoError(t, e.(jobbatch.Disposer).Dispose(context.Background()))
			_, err = reader.ReadOutput(context.Background(), 0)
			require.ErrorIs(t, err, jobbatch.ErrReleased)
		})
	}
}

func TestBatchGenericPanic(t *testing.T) {
	tool := &batchGenericTestTool{name: "view", run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) { panic("private") }}
	_, reports := batchGenericTestStart(t, batchGenericTestExecutor(tool), batchKindGeneric, tool.name)
	r := batchGenericTestReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Equal(t, "tool_panic", r.Code)
	require.Empty(t, r.Summary)
}

func TestBatchGenericRejectedStarts(t *testing.T) {
	tool := &batchGenericTestTool{name: "view", run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		t.Error("rejected Start ran tool")
		return fantasy.ToolResponse{}, nil
	}}
	e := batchGenericTestExecutor(tool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Start(ctx, batchTestLaunch(batchKindGeneric, `{"tool":"view","session_id":"s","input":{}}`), func(jobbatch.Result) { t.Error("rejected Start reported") })
	require.ErrorIs(t, err, context.Canceled)
	for _, name := range []string{"missing", "await_tasks", "batch_run", "ask_question", "stop_agent"} {
		p, err := json.Marshal(batchLeafPayload{Tool: name, SessionID: "s"})
		require.NoError(t, err)
		_, err = e.Start(context.Background(), jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: p}}, func(jobbatch.Result) { t.Error("rejected Start reported") })
		var coded interface{ Code() string }
		require.ErrorAs(t, err, &coded)
		require.Equal(t, "tool_unavailable", coded.Code())
	}
}

func TestBatchGenericSharedMCPStopIsolation(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "batch-local"}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: "call"}, func(ctx context.Context, _ *modelmcp.CallToolRequest, input struct {
		ID string `json:"id"`
	},
	) (*modelmcp.CallToolResult, any, error) {
		entered <- input.ID
		select {
		case <-release:
			return &modelmcp.CallToolResult{Content: []modelmcp.Content{&modelmcp.TextContent{Text: "survivor"}}}, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	defer httpServer.Close()
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	cfg := config.NewLibraryStore(&config.Config{MCP: config.MCPs{"batch-local": {Type: config.MCPHttp, URL: httpServer.URL}}}, "")
	owner.Initialize(context.Background(), nil, cfg, false)
	owner.WaitForInit(context.Background())
	tool := &batchGenericTestTool{name: "mcp_batch-local_call", run: func(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
		result, err := owner.RunTool(ctx, cfg, "batch-local", "call", c.Input)
		response := fantasy.NewTextResponse(result.Content)
		response.IsError = result.IsError
		return response, err
	}}
	executor := batchGenericTestExecutor(tool)
	start := func(id string) (jobbatch.Execution, chan jobbatch.Result) {
		p, err := json.Marshal(batchLeafPayload{Tool: tool.name, SessionID: t.Name(), Input: json.RawMessage(`{"id":"` + id + `"}`)})
		require.NoError(t, err)
		reports := make(chan jobbatch.Result, 2)
		e, err := executor.Start(context.Background(), jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: batchKindMCP, Payload: p}}, func(r jobbatch.Result) { reports <- r })
		require.NoError(t, err)
		return e, reports
	}
	stopped, stoppedReports := start("stopped")
	_, survivorReports := start("survivor")
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("local calls did not enter")
		}
	}
	require.NoError(t, stopped.Stop(context.Background()))
	require.Equal(t, jobbatch.StateCancelled, batchGenericTestReport(t, stoppedReports).State)
	close(release)
	survivor := batchGenericTestReport(t, survivorReports)
	require.Equal(t, jobbatch.StateCompleted, survivor.State)
	require.Equal(t, "survivor", survivor.Summary)
}

func batchWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("barrier timed out")
	}
}

func TestBatchGenericResolverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := &batchGenericTestTool{name: "view", run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		t.Error("cancelled resolver ran leaf")
		return fantasy.ToolResponse{}, nil
	}}
	e := batchGenericTestExecutor(tool)
	resolve := e.env.Leaves
	e.env.Leaves = func(s string) (*policyLeafRegistry, error) {
		cancel()
		return resolve(s)
	}
	execution, err := e.Start(ctx, batchTestLaunch(batchKindGeneric, `{"tool":"view","session_id":"s","input":{}}`), func(jobbatch.Result) { t.Error("cancelled resolver reported") })
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, execution)
}

func TestBatchGenericStartLifetime(t *testing.T) {
	type unrelatedKey struct{}
	ctx := context.WithValue(context.Background(), unrelatedKey{}, "must not retain")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "message")
	ctx = context.WithValue(ctx, tools.SupportsImagesContextKey, true)
	ctx = context.WithValue(ctx, tools.ModelNameContextKey, "model")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	tool := &batchGenericTestTool{name: "view", run: func(runCtx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(entered)
		<-release
		assert.NoError(t, runCtx.Err())
		assert.Nil(t, runCtx.Value(unrelatedKey{}))
		assert.Equal(t, "s", tools.GetSessionFromContext(runCtx))
		assert.Equal(t, "message", tools.GetMessageFromContext(runCtx))
		assert.True(t, tools.GetSupportsImagesFromContext(runCtx))
		assert.Equal(t, "model", tools.GetModelNameFromContext(runCtx))
		return fantasy.NewTextResponse("ok"), nil
	}}
	reports := make(chan jobbatch.Result, 2)
	_, err := batchGenericTestExecutor(tool).Start(ctx, batchTestLaunch(batchKindGeneric, `{"tool":"view","session_id":"s","input":{}}`), func(r jobbatch.Result) { reports <- r })
	require.NoError(t, err)
	batchWait(t, entered)
	cancel()
	// Release via a separate barrier so assertions finish before the test returns.
	close(release)
	require.Equal(t, jobbatch.StateCompleted, batchGenericTestReport(t, reports).State)
}

// TestBatchGenericDirectStall and TestBatchGenericManagedStallBypass were
// DELETEd with the stall-detach authority (turn_stall_tool_detach.go,
// stallDetacherTool, the stallDetach* maps): there is no detach action left
// to pin, managed or otherwise. The generic stall clock's spacing and
// zero-timeout policy remain pinned by turn_stall_policy_spacing_test.go and
// turn_stall_zero_timeout_test.go.
