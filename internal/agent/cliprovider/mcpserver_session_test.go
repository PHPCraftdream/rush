package cliprovider

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type recordingMCPPermissionService struct {
	permission.Service
	mu       sync.Mutex
	requests []permission.CreatePermissionRequest
	deny     bool
}

func (s *recordingMCPPermissionService) Request(ctx context.Context, opts permission.CreatePermissionRequest) (bool, error) {
	s.mu.Lock()
	s.requests = append(s.requests, opts)
	deny := s.deny
	s.mu.Unlock()
	if deny {
		return false, nil
	}
	return s.Service.Request(ctx, opts)
}

func (s *recordingMCPPermissionService) Requests() []permission.CreatePermissionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permission.CreatePermissionRequest(nil), s.requests...)
}

type testExternalMCPProxy struct{}

func (testExternalMCPProxy) ListTools() []ExternalMCPTool {
	return []ExternalMCPTool{{
		ServerName:  "external",
		Name:        "ping",
		Description: "Test external tool",
	}}
}

func (testExternalMCPProxy) CallTool(context.Context, string, string, string) (string, error) {
	return "external result", nil
}

func newMCPTestPermissionService(t *testing.T, workingDir string) permission.Service {
	t.Helper()
	dbDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dbDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dbDir)) })
	return permission.NewPermissionService(t.Context(), workingDir, false, nil, db.New(conn))
}

func newMCPTestServer(t *testing.T, perms permission.Service, sessionID, workingDir string, proxy ExternalMCPProxy) *rushMCPServer {
	t.Helper()
	srv, err := newRushMCPServer(t.Context(), perms, nil, sessionID, workingDir, "test-token", proxy)
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	return srv
}

func connectMCPTestClient(t *testing.T, srv *rushMCPServer) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "cliprovider-session-test"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.mcpURL()}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close()) })
	return sess
}

func callMCPTool(t *testing.T, client *mcp.ClientSession, name string, input any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
	require.NoError(t, err)
	return result
}

func TestMCPServerPermissionRequestsUseOwningSessionForAllTools(t *testing.T) {
	workingDir := t.TempDir()
	base := newMCPTestPermissionService(t, workingDir)
	recorder := &recordingMCPPermissionService{Service: base, deny: true}
	srv := newMCPTestServer(t, recorder, "owner-session", workingDir, testExternalMCPProxy{})
	client := connectMCPTestClient(t, srv)

	calls := []struct {
		name  string
		input any
		tool  string
	}{
		{"Bash", mcpBashInput{Command: "echo denied", Description: "test"}, "bash"},
		{"Read", mcpViewInput{Path: "missing.txt"}, "view"},
		{"Write", mcpWriteInput{Path: "written.txt", Content: "nope"}, "write"},
		{"Glob", mcpGlobInput{Pattern: "*.go"}, "glob"},
		{"Grep", mcpGrepInput{Pattern: "needle"}, "grep"},
		{"external__ping", map[string]any{}, "mcp_external__ping"},
	}
	for _, call := range calls {
		result := callMCPTool(t, client, call.name, call.input)
		require.True(t, result.IsError, "permission denial must stop %s", call.name)
	}

	requests := recorder.Requests()
	require.Len(t, requests, len(calls))
	for i, request := range requests {
		require.Equal(t, "owner-session", request.SessionID)
		require.Equal(t, calls[i].tool, request.ToolName)
	}
}

func TestMCPServerAutoApproveUsesOwningSession(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(workingDir, "read.txt")
	require.NoError(t, os.WriteFile(path, []byte("read through MCP"), 0o644))

	perms := newMCPTestPermissionService(t, workingDir)
	perms.AutoApproveSession("owner-session")
	client := connectMCPTestClient(t, newMCPTestServer(t, perms, "owner-session", workingDir, nil))

	result := callMCPTool(t, client, "Read", map[string]any{"path": path})
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	require.Equal(t, "read through MCP", result.Content[0].(*mcp.TextContent).Text)
}

func TestMCPServerRestrictedSessionAllowlistUsesBashInput(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(workingDir, "read.txt")
	require.NoError(t, os.WriteFile(path, []byte("allowed read"), 0o644))

	base := newMCPTestPermissionService(t, workingDir)
	perms := &recordingMCPPermissionService{Service: base}
	perms.AutoApproveSession("owner-session")
	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"view:read"},
		AllowBash:  []string{"echo"},
	})
	require.NoError(t, err)
	allowlistManager, ok := base.(permission.SessionRunAllowlistManager)
	require.True(t, ok)
	ownerCallID := "mcp-test-" + uuid.NewString()
	allowlistManager.SetSessionRunAllowlistForCall("owner-session", allowlist, ownerCallID)
	t.Cleanup(func() {
		allowlistManager.ClearSessionRunAllowlistForCall("owner-session", ownerCallID)
	})

	client := connectMCPTestClient(t, newMCPTestServer(t, perms, "owner-session", workingDir, nil))
	readResult := callMCPTool(t, client, "Read", map[string]any{"path": path})
	require.False(t, readResult.IsError)
	bashResult := callMCPTool(t, client, "Bash", mcpBashInput{Command: "echo allowed", Description: "allowed command"})
	require.False(t, bashResult.IsError)
	deniedCommand := callMCPTool(t, client, "Bash", mcpBashInput{Command: "whoami", Description: "denied command"})
	require.True(t, deniedCommand.IsError)
	deniedTool := callMCPTool(t, client, "Write", mcpWriteInput{Path: "denied.txt", Content: "nope"})
	require.True(t, deniedTool.IsError)
	require.NoFileExists(t, filepath.Join(workingDir, "denied.txt"))

	requests := perms.Requests()
	require.Len(t, requests, 4)
	for _, request := range requests {
		require.Equal(t, "owner-session", request.SessionID)
	}
	var sawBash bool
	for _, request := range requests {
		if request.ToolName != "bash" {
			continue
		}
		input, ok := request.Params.(mcpBashInput)
		require.True(t, ok, "bash permission params must retain mcpBashInput")
		sawBash = sawBash || input.Command == "echo allowed"
	}
	require.True(t, sawBash)
}

func TestMCPServerAutoApproveDoesNotCrossSession(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(workingDir, "read.txt")
	require.NoError(t, os.WriteFile(path, []byte("session A"), 0o644))

	base := newMCPTestPermissionService(t, workingDir)
	perms := &recordingMCPPermissionService{Service: base}
	perms.AutoApproveSession("session-A")
	clientA := connectMCPTestClient(t, newMCPTestServer(t, perms, "session-A", workingDir, nil))
	clientB := connectMCPTestClient(t, newMCPTestServer(t, perms, "session-B", workingDir, nil))

	allowed := callMCPTool(t, clientA, "Read", map[string]any{"path": path})
	require.False(t, allowed.IsError)

	permissionCtx, permissionCancel := context.WithCancel(t.Context())
	defer permissionCancel()
	permissionEvents := base.Subscribe(permissionCtx)
	callCtx, callCancel := context.WithCancel(t.Context())
	defer callCancel()
	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	callDone := make(chan callResult, 1)
	go func() {
		result, err := clientB.CallTool(callCtx, &mcp.CallToolParams{
			Name:      "Read",
			Arguments: map[string]any{"path": path},
		})
		callDone <- callResult{result: result, err: err}
	}()

	var requested permission.PermissionRequest
	deadlockFence := time.NewTimer(5 * time.Second)
	defer deadlockFence.Stop()
	for {
		select {
		case event := <-permissionEvents:
			if event.Payload.SessionID != "session-B" {
				continue
			}
			requested = event.Payload
			base.Deny(requested)
			goto denied
		case <-deadlockFence.C:
			t.Fatal("session-B permission request was not published")
		}
	}

denied:
	select {
	case outcome := <-callDone:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.True(t, outcome.result.IsError)
		require.Len(t, outcome.result.Content, 1)
		require.Equal(t, "read denied", outcome.result.Content[0].(*mcp.TextContent).Text)
	case <-deadlockFence.C:
		t.Fatal("session-B MCP call did not finish after its exact permission request was denied")
	}
	require.Equal(t, "session-B", requested.SessionID)
	requests := perms.Requests()
	require.Len(t, requests, 2)
	require.Equal(t, "session-A", requests[0].SessionID)
	require.Equal(t, "session-B", requests[1].SessionID)
	require.Equal(t, requested.ToolCallID, requests[1].ToolCallID)
}

func TestNewRushMCPServerRejectsMissingSessionWithPermissions(t *testing.T) {
	workingDir := t.TempDir()
	perms := newMCPTestPermissionService(t, workingDir)

	srv, err := newRushMCPServer(t.Context(), perms, nil, "", workingDir, "", nil)
	require.Error(t, err)
	require.Nil(t, srv)
}

func TestNewRushMCPServerAllowsMissingSessionWithoutPermissions(t *testing.T) {
	srv, err := newRushMCPServer(t.Context(), nil, nil, "", t.TempDir(), "test-token", nil)
	require.NoError(t, err)
	srv.stop()
}

func TestMCPServerExternalPermissionRequestCarriesSessionID(t *testing.T) {
	workingDir := t.TempDir()
	base := newMCPTestPermissionService(t, workingDir)
	perms := &recordingMCPPermissionService{Service: base, deny: true}
	srv := newMCPTestServer(t, perms, "external-owner", workingDir, testExternalMCPProxy{})
	client := connectMCPTestClient(t, srv)

	result := callMCPTool(t, client, "external__ping", map[string]any{"value": "x"})
	require.True(t, result.IsError)
	requests := perms.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, "external-owner", requests[0].SessionID)
	require.Equal(t, "mcp_external__ping", requests[0].ToolName)
}
