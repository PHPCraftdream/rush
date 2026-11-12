package cliprovider

// rushMCPServer starts an in-process MCP HTTP server that exposes rush's
// core tools (bash, view, write, edit, glob, grep) to an external CLI process
// (e.g. the claude CLI). Each server instance generates a random Bearer token
// so only the CLI process spawned by rush can connect.
//
// Usage:
//  1. Create the server with newRushMCPServer.
//  2. Write mcpConfigFile() to a temp file and pass it to the claude CLI via
//     the --mcp-config flag.
//  3. Call stop() when the CLI process exits to free the port.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolEvent is emitted by the MCP server when a tool call starts or ends.
// id is a UUID; name is non-empty for start events, empty for end events.
type mcpToolEvent struct {
	id    string
	name  string // non-empty = start event; empty = end event
	input string // JSON-encoded input (start events only)
}

// rushMCPServer is an in-process MCP HTTP server with token auth.
// The token is accepted via Authorization: Bearer header (Claude CLI)
// or as a ?token= query parameter (Qwen CLI, which cannot set headers).
type rushMCPServer struct {
	addr    string // "127.0.0.1:PORT"
	token   string
	httpSrv *http.Server
	// toolCh receives tool-call notifications from MCP handlers so the
	// Stream scan loop can emit ToolInputStart/Delta/End stream parts.
	toolCh chan mcpToolEvent
}

// stop shuts down the HTTP server.
func (s *rushMCPServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		slog.Debug("cliprovider: MCP server shutdown error", "err", err)
	}
}

// mcpConfigJSON returns the JSON bytes of the MCP server config suitable for
// writing to a temp file and passing to the claude CLI via --mcp-config.
func (s *rushMCPServer) mcpConfigJSON() ([]byte, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"rush": map[string]any{
				"type": "http",
				"url":  "http://" + s.addr + "/mcp",
				"headers": map[string]string{
					"Authorization": "Bearer " + s.token,
				},
			},
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// mcpURL returns the URL with the token embedded as a query parameter,
// for clients (e.g. qwen CLI) that cannot set custom HTTP headers.
func (s *rushMCPServer) mcpURL() string {
	return "http://" + s.addr + "/mcp?token=" + s.token
}

// newRushMCPServer starts a local MCP HTTP server and returns it.
// The server exposes rush's core tools; each tool call goes through
// perms.Request before execution so rush's permission dialog appears.
// The token is accepted via Authorization: Bearer header OR ?token= query param.
// If token is empty a cryptographically random one is generated.
// sessions and sessionID are used by the todos tool to persist task updates.
func newRushMCPServer(ctx context.Context, perms permission.Service, sessions session.Service, sessionID string, workingDir string, token string, mcpProxy ExternalMCPProxy) (*rushMCPServer, error) {
	policy, constrained := ToolPolicyFrom(ctx)
	bridge := invocationToolBridge(ctx)
	if bridge == nil && perms != nil && sessionID == "" {
		return nil, fmt.Errorf("cliprovider: MCP server requires a session ID when permissions are enabled")
	}
	if constrained && bridge == nil && mcpProxy != nil && len(mcpProxy.ListTools()) > 0 {
		return nil, errors.New("cliprovider: constrained external MCP tools are unavailable without a pinned bridge")
	}
	if constrained && bridge == nil && !mcpToolsCanBeScoped(mcpProxy) {
		return nil, errors.New("cliprovider: unscoped MCP tools cannot be enabled for a constrained call")
	}

	if token == "" {
		// 32-byte random token → 64-char hex string.
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, fmt.Errorf("cliprovider: generate MCP token: %w", err)
		}
		token = hex.EncodeToString(tokenBytes)
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "rush",
		Title:   "Rush",
		Version: "1.0",
	}, nil)

	toolCh := make(chan mcpToolEvent, 32)
	if bridge != nil {
		if err := registerBridgeTools(ctx, srv, bridge, toolCh); err != nil {
			return nil, err
		}
	} else {
		registerMCPTools(srv, perms, sessions, sessionID, workingDir, toolCh, policy, constrained)
		if mcpProxy != nil && !constrained {
			registerExternalMCPTools(ctx, srv, perms, sessionID, workingDir, mcpProxy, toolCh)
		}
	}

	rawHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			// Stateless mode: each POST creates a temporary session.
			// Simple and sufficient for single-agent CLI use.
			Stateless: true,
		},
	)

	// Auth middleware: accept token via Authorization header or ?token= query param.
	bearer := "Bearer " + token
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != bearer && r.URL.Query().Get("token") != token {
			slog.Debug("cliprovider: MCP request rejected — bad token",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		slog.Debug("cliprovider: MCP request", "method", r.Method, "path", r.URL.Path)
		rawHandler.ServeHTTP(w, r)
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cliprovider: start MCP listener: %w", err)
	}

	httpSrv := &http.Server{Handler: http.StripPrefix("/mcp", handler)}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Debug("cliprovider: MCP server stopped", "err", err)
		}
	}()

	addr := ln.Addr().String()
	slog.Info("cliprovider: MCP server started", "addr", addr)
	return &rushMCPServer{
		addr:    addr,
		token:   token,
		httpSrv: httpSrv,
		toolCh:  toolCh,
	}, nil
}

func mcpToolsCanBeScoped(proxy ExternalMCPProxy) bool {
	return proxy == nil || len(proxy.ListTools()) == 0
}

// registerMCPTools adds Rush native MCP tools. The constrained path includes
// only the canonical names present in the invocation allowlist.
func registerMCPTools(srv *mcp.Server, perms permission.Service, sessions session.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent, policy ToolPolicy, constrained bool) {
	if !constrained || policyHasMCPTool(policy, "bash") {
		registerBashTool(srv, perms, sessionID, workingDir, toolCh)
	}
	if !constrained || policyHasMCPTool(policy, "view") {
		registerViewTool(srv, perms, sessionID, workingDir, toolCh)
	}
	if !constrained || policyHasMCPTool(policy, "write") {
		registerWriteTool(srv, perms, sessionID, workingDir, toolCh)
	}
	if !constrained || policyHasMCPTool(policy, "glob") {
		registerGlobTool(srv, perms, sessionID, workingDir, toolCh)
	}
	if !constrained || policyHasMCPTool(policy, "grep") {
		registerGrepTool(srv, perms, sessionID, workingDir, toolCh)
	}
	if sessions != nil && sessionID != "" && (!constrained || policyHasMCPTool(policy, "todos")) {
		registerTodosTool(srv, sessions, sessionID)
	}
}

// emitToolStart sends a tool-call start notification to toolCh if non-nil.
func emitToolStart(toolCh chan mcpToolEvent, id, name, inputJSON string) {
	if toolCh == nil {
		return
	}
	select {
	case toolCh <- mcpToolEvent{id: id, name: name, input: inputJSON}:
	default:
		slog.Debug("cliprovider: toolCh full, dropping start event", "tool", name)
	}
}

// emitToolEnd sends a tool-call end notification to toolCh if non-nil.
func emitToolEnd(toolCh chan mcpToolEvent, id string) {
	if toolCh == nil {
		return
	}
	select {
	case toolCh <- mcpToolEvent{id: id}:
	default:
		slog.Debug("cliprovider: toolCh full, dropping end event", "id", id)
	}
}

// rushMCPToolArgs returns the claude CLI flags that confine it to rush's MCP
// tools. --allowedTools pre-approves them inside the CLI's own permission
// layer (so calls reach rush's handlers, which run rush's permission checks);
// it blocks nothing under --dangerously-skip-permissions, so --tools limits
// the CLI's built-in set to the non-MCP names of the same list — without it
// the native Bash/Write/Edit bypass agentguard and the run policy.
// TodoWrite is denied so the model uses mcp__rush__todos, which persists to
// the rush session.
func rushMCPToolArgs(bridge ToolBridge, proxy ExternalMCPProxy) []string {
	return rushMCPToolArgsWithPolicy(bridge, proxy, ToolPolicy{})
}

func rushMCPToolArgsWithPolicy(bridge ToolBridge, proxy ExternalMCPProxy, policy ToolPolicy) []string {
	if policy.Constrained {
		tools, err := policyBridgeTools(policy, bridge, proxy)
		if err != nil {
			return []string{"--tools", "", "--allowedTools", ""}
		}
		allowed := make([]string, 0, len(tools))
		for _, tool := range tools {
			allowed = append(allowed, "mcp__rush__"+tool.WireName)
		}
		return []string{"--tools", "", "--allowedTools", strings.Join(allowed, ","), "--disallowedTools", "TodoWrite"}
	}
	allowed := []string{
		// Rush MCP bridge tools (go through rush's permission system).
		"mcp__rush__Bash",
		"mcp__rush__Read",
		"mcp__rush__Write",
		"mcp__rush__Glob",
		"mcp__rush__Grep",
		"mcp__rush__todos",
		// CLI built-ins rush doesn't replicate.
		"WebSearch",
		"WebFetch",
		"Task",
		"Agent",
	}
	// Include external MCP tools registered on the rush MCP bridge.
	if bridge != nil && policy.Constrained {
		allowed = nil
		for _, tool := range bridge.Tools() {
			if policyHasMCPTool(policy, tool.CanonicalName) {
				allowed = append(allowed, "mcp__rush__"+tool.WireName)
			}
		}
	} else if bridge != nil {
		allowed = nil
		for _, tool := range bridge.Tools() {
			allowed = append(allowed, "mcp__rush__"+tool.WireName)
		}
	} else if proxy != nil {
		for _, ext := range proxy.ListTools() {
			allowed = append(allowed, "mcp__rush__"+ext.ServerName+"__"+ext.Name)
		}
	}
	var builtins []string
	for _, name := range allowed {
		if !strings.HasPrefix(name, "mcp__") {
			builtins = append(builtins, name)
		}
	}
	return []string{
		"--tools", strings.Join(builtins, ","),
		"--allowedTools", strings.Join(allowed, ","),
		"--disallowedTools", "TodoWrite",
	}
}
