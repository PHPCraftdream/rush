package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type mcpOutputPermissions struct {
	permission.Service
	deny string
	asks []string
}

func (p *mcpOutputPermissions) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	p.asks = append(p.asks, request.ToolName)
	return request.ToolName != p.deny, nil
}

type mcpOutputRestrictedPermissions struct {
	*mcpOutputPermissions
	hookLeaked bool
}

func (p *mcpOutputRestrictedPermissions) AuthorizeRestrictedDispatch(ctx context.Context, request permission.CreatePermissionRequest) (bool, bool) {
	p.hookLeaked = permission.HookApprovalGranted(ctx, request.ToolCallID)
	return true, false
}

func mcpOutputToolStore(t *testing.T) (*session.MCPOutputStore, *sql.DB) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	_, err = db.New(conn).CreateSession(t.Context(), db.CreateSessionParams{ID: "output-session", Title: "output"})
	require.NoError(t, err)
	transaction := func(ctx context.Context, _ string, effect func(*sql.Tx) error) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if err := effect(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	return session.NewMCPOutputStore(transaction, transaction), conn
}

func TestMCPOutputPreviewMatchesBashPolicyAndBoundsLargeFragments(t *testing.T) {
	for _, source := range []string{"", "brief", strings.Repeat("x", MaxOutputLength), strings.Repeat("😀α\n", 12000)} {
		for _, chunk := range []int{1, 701, len(source) + 1} {
			preview := &mcpOutputPreview{}
			for offset := 0; offset < len(source); offset += chunk {
				_, err := io.WriteString(preview, source[offset:min(offset+chunk, len(source))])
				require.NoError(t, err)
				require.LessOrEqual(t, len(preview.head), MaxOutputLength)
				require.LessOrEqual(t, len(preview.tail), MaxOutputLength)
				require.LessOrEqual(t, cap(preview.head), MaxOutputLength)
				require.LessOrEqual(t, cap(preview.tail), MaxOutputLength)
			}
			require.Equal(t, TruncateOutput(source), preview.text())
			require.True(t, utf8.ValidString(preview.text()))
		}
	}
}

func TestMCPOutputActualToolAdmissionDurableReplayAndPermissionFloors(t *testing.T) {
	store, conn := mcpOutputToolStore(t)
	permissions := &mcpOutputPermissions{}
	access := &MCPOutputAccess{Store: store, Allowed: map[string]bool{"mcp_output_source": true}, Permissions: permissions}
	fragment := strings.Repeat("αβ😀\n", 7000)
	full := fragment + "\n" + fragment + "\nlast fragment"
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "output"}, nil)
	var executions atomic.Int32
	var traceMu sync.Mutex
	var handlerTrace []struct {
		Error bool
		Media bool
	}
	var runResponses []struct {
		CallID  string
		Content string
	}
	traceResponse := func(callID, content string) {
		traceMu.Lock()
		defer traceMu.Unlock()
		runResponses = append(runResponses, struct {
			CallID  string
			Content string
		}{CallID: callID, Content: content})
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		traceMu.Lock()
		defer traceMu.Unlock()
		t.Logf("MCP output fixture handler inputs: %+v", handlerTrace)
		for _, response := range runResponses {
			t.Logf("MCP output fixture Run %s response.Content: %q", response.CallID, response.Content)
		}
	})
	modelmcp.AddTool(server, &modelmcp.Tool{Name: "source"}, func(_ context.Context, _ *modelmcp.CallToolRequest, input struct {
		Error bool `json:"error,omitempty"`
		Media bool `json:"media,omitempty"`
	},
	) (*modelmcp.CallToolResult, any, error) {
		executions.Add(1)
		traceMu.Lock()
		handlerTrace = append(handlerTrace, struct {
			Error bool
			Media bool
		}{Error: input.Error, Media: input.Media})
		traceMu.Unlock()
		result := &modelmcp.CallToolResult{IsError: input.Error, Content: []modelmcp.Content{
			&modelmcp.TextContent{Text: fragment}, &modelmcp.TextContent{Text: fragment}, &modelmcp.TextContent{Text: "last fragment"},
		}}
		if input.Media {
			result.Content = append(result.Content, &modelmcp.ImageContent{Data: []byte{255, 1}, MIMEType: "image/png"})
		}
		return result, nil, nil
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })
	t.Setenv("RUSH_DISABLE_DEFAULT_PROVIDERS", "1")
	globalConfigDir := os.Getenv("RUSH_GLOBAL_CONFIG")
	require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "rush.json"), []byte(`{
  "providers": {
    "test": {
      "id": "test",
      "base_url": "https://example.invalid/v1",
      "api_key": "test-key",
      "discover_models": false,
      "models": [{"id": "model", "name": "Test model"}]
    }
  }
}`), 0o600))
	workingDir := t.TempDir()
	cfg, err := config.Init(workingDir, workingDir, false)
	require.NoError(t, err)
	require.NoError(t, cfg.PersistMCPConfig(config.ScopeGlobal, "output", config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL}))
	admission := cfg.SnapshotMCPAdmission("output")
	require.True(t, admission.Exists, "fixture MCP definition must be present in the loaded config")
	require.True(t, admission.HasMCPInput, "fixture MCP source must be captured from supported persisted config")
	owner.Initialize(t.Context(), nil, cfg, false)
	require.NoError(t, owner.WaitForInit(t.Context()))
	state, exists := owner.GetState("output")
	require.True(t, exists, "initialized MCP owner must publish its state")
	require.NoError(t, state.Error, "MCP initialization state error; published tool names: %v", owner.GetServerToolNames("output"))
	require.Equal(t, mcp.StateConnected, state.State, "MCP fixture initialization failed; state error: %v; published tool names: %v", state.Error, owner.GetServerToolNames("output"))
	require.Equal(t, []string{"source"}, owner.GetServerToolNames("output"), "published MCP source inventory")
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "output-session")
	published := GetMCPTools(permissions, cfg, cfg.WorkingDir(), owner, access)
	require.Len(t, published, 1, "published source tools: %v", owner.GetServerToolNames("output"))
	tool := published[0]
	require.Equal(t, "mcp_output_source", tool.Name())
	var ref string
	for _, sourceError := range []bool{false, true} {
		callID, input := "success-call", `{"error":false}`
		if sourceError {
			callID, input = "error-call", `{"error":true}`
		}
		response, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: tool.Name(), Input: input})
		traceResponse(callID, response.Content)
		require.NoError(t, err, "MCP source Run response content: %q", response.Content)
		require.Equal(t, sourceError, response.IsError, "unexpected MCP source Run status; response content: %q", response.Content)
		require.True(t, strings.HasPrefix(response.Content, TruncateOutput(full)), "unexpected MCP source Run content: %q", response.Content)
		require.Less(t, len(response.Content), MaxOutputLength+1000, "unexpected MCP source Run content length; response content: %q", response.Content)
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT id FROM mcp_outputs WHERE tool_call_id = ?`, callID).Scan(&ref))
		require.Contains(t, response.Content, "rush-mcp-output://"+ref, "unexpected MCP source Run artifact response content: %q", response.Content)
		// Reserved artifact dispatch must work with NO MCP resource transport.
		reader := NewReadMCPResourceTool(cfg, permissions, nil, access)
		uri := "rush-mcp-output://" + ref
		var replay strings.Builder
		for uri != "" {
			input, err := json.Marshal(ReadMCPResourceParams{MCPName: "output", URI: uri})
			require.NoError(t, err)
			page, err := reader.Run(ctx, fantasy.ToolCall{ID: "read-output", Name: ReadMCPResourceToolName, Input: string(input)})
			traceResponse("read-output", page.Content)
			require.NoError(t, err, "MCP output page Run response content: %q", page.Content)
			require.False(t, page.IsError, "unexpected MCP output page Run status; response content: %q", page.Content)
			var metadata struct {
				Offset      int64  `json:"offset"`
				Next        int64  `json:"next_offset"`
				URI         string `json:"next_uri"`
				SourceError bool   `json:"source_is_error"`
			}
			require.NoError(t, json.Unmarshal([]byte(page.Metadata), &metadata))
			require.Equal(t, sourceError, metadata.SourceError)
			require.EqualValues(t, replay.Len(), metadata.Offset, "pages must advance without gaps or repeats")
			require.Greater(t, metadata.Next, metadata.Offset)
			require.LessOrEqual(t, metadata.Next-metadata.Offset, int64(MaxOutputLength))
			require.True(t, utf8.ValidString(page.Content))
			replay.WriteString(page.Content[:metadata.Next-metadata.Offset])
			uri = metadata.URI
		}
		require.Equal(t, full, replay.String())
	}
	mediaCtx := context.WithValue(ctx, SupportsImagesContextKey, true)
	media, err := tool.Run(mediaCtx, fantasy.ToolCall{ID: "media-call", Name: tool.Name(), Input: `{"media":true}`})
	traceResponse("media-call", media.Content)
	require.NoError(t, err, "MCP media Run response content: %q", media.Content)
	require.False(t, media.IsError, "unexpected MCP media Run status; response content: %q", media.Content)
	require.Equal(t, full, media.Content, "media Run text mismatch; response content: %q", media.Content)
	require.Equal(t, []byte{255, 1}, media.Data, "media Run payload mismatch; response content: %q", media.Content)
	var artifacts int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_outputs`).Scan(&artifacts))
	require.Equal(t, 2, artifacts)
	refuseTransaction := func(context.Context, string, func(*sql.Tx) error) error { return errors.New("finalized") }
	access.Store = session.NewMCPOutputStore(refuseTransaction, refuseTransaction)
	failure, err := tool.Run(ctx, fantasy.ToolCall{ID: "retention-failed", Name: tool.Name(), Input: `{}`})
	traceResponse("retention-failed", failure.Content)
	require.NoError(t, err, "retention-failure Run response content: %q", failure.Content)
	require.True(t, failure.IsError, "retention-failure Run unexpectedly succeeded; response content: %q", failure.Content)
	require.Contains(t, failure.Content, "RETENTION FAILED after MCP tool ran", "unexpected retention-failure Run content: %q", failure.Content)
	require.NotContains(t, failure.Content, "rush-mcp-output://", "unexpected retention-failure Run content: %q", failure.Content)
	require.EqualValues(t, 4, executions.Load(), "retention failure must not rerun the original side effect")
	access.Store = store
	reader := NewReadMCPResourceTool(cfg, permissions, nil, access)
	read := func(ctx context.Context, uri string) fantasy.ToolResponse {
		input, err := json.Marshal(ReadMCPResourceParams{MCPName: "output", URI: uri})
		require.NoError(t, err)
		page, err := reader.Run(ctx, fantasy.ToolCall{ID: "read", Name: ReadMCPResourceToolName, Input: string(input)})
		traceResponse("read", page.Content)
		require.NoError(t, err, "MCP output page Run response content: %q", page.Content)
		return page
	}
	uri := "rush-mcp-output://" + ref
	permissions.deny = ReadMCPResourceToolName
	permissions.asks = nil
	deniedResource := read(ctx, uri)
	require.True(t, deniedResource.IsError, "unexpected resource permission response content: %q", deniedResource.Content)
	require.Equal(t, []string{ReadMCPResourceToolName}, permissions.asks)
	permissions.deny = tool.Name()
	deniedSource := read(ctx, uri)
	require.True(t, deniedSource.IsError, "source permission is independent of resource permission; response content: %q", deniedSource.Content)
	permissions.deny = ""
	restricted := &mcpOutputRestrictedPermissions{mcpOutputPermissions: permissions}
	access.Permissions = restricted
	hookDenied := read(permission.WithHookApproval(ctx, "read"), uri)
	require.True(t, hookDenied.IsError, "resource hook approval must not authorize the original source tool; response content: %q", hookDenied.Content)
	require.False(t, restricted.hookLeaked)
	access.Permissions = permissions
	delete(access.Allowed, tool.Name())
	revokedSource := read(ctx, uri)
	require.True(t, revokedSource.IsError, "revoked source tools must stay inaccessible; response content: %q", revokedSource.Content)
	access.Allowed[tool.Name()] = true
	other := context.WithValue(ctx, SessionIDContextKey, "other-session")
	wrongSession := read(other, uri)
	require.True(t, wrongSession.IsError, "unexpected cross-session resource response content: %q", wrongSession.Content)
	for _, invalid := range []string{uri + "/path", uri + "?offset=-1", uri + "?secret=x", "rush-mcp-output://missing"} {
		invalidResponse := read(ctx, invalid)
		require.True(t, invalidResponse.IsError, "reserved local URIs must not reach an MCP transport; response content: %q", invalidResponse.Content)
	}
	require.EqualValues(t, 4, executions.Load(), "replay and authorization denials must not rerun the source MCP tool")
}

func TestMCPOutputRetentionFailureIsExplicitAfterExecution(t *testing.T) {
	store, _ := mcpOutputToolStore(t)
	refuseTransaction := func(context.Context, string, func(*sql.Tx) error) error { return errors.New("finalized") }
	failed := session.NewMCPOutputStore(refuseTransaction, refuseTransaction)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "output-session")
	for _, candidate := range []MCPOutputStore{store, failed} {
		access := &MCPOutputAccess{Store: candidate, Allowed: map[string]bool{"mcp_output_source": true}}
		source := strings.Repeat("chunk", MaxOutputLength)
		if candidate == store {
			source = strings.Repeat("q", session.MCPOutputLimit+1)
		}
		text, err := access.admit(ctx, "output", "mcp_output_source", "failure", func(w io.Writer) error { _, err := io.WriteString(w, source); return err }, false)
		require.Error(t, err)
		require.Empty(t, text, "no preview or reference may precede durable retention")
		require.Contains(t, err.Error(), "RETENTION FAILED after MCP tool ran")
		require.Contains(t, err.Error(), "do not automatically retry")
		require.NotContains(t, err.Error(), "rush-mcp-output://")
	}
}
