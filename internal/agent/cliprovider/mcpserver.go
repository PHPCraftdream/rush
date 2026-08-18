package cliprovider

// crushMCPServer starts an in-process MCP HTTP server that exposes crush's
// core tools (bash, view, write, edit, glob, grep) to an external CLI process
// (e.g. the claude CLI). Each server instance generates a random Bearer token
// so only the CLI process spawned by crush can connect.
//
// Usage:
//  1. Create the server with newCrushMCPServer.
//  2. Write mcpConfigFile() to a temp file and pass it to the claude CLI via
//     the --mcp-config flag.
//  3. Call stop() when the CLI process exits to free the port.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolEvent is emitted by the MCP server when a tool call starts or ends.
// id is a UUID; name is non-empty for start events, empty for end events.
type mcpToolEvent struct {
	id    string
	name  string // non-empty = start event; empty = end event
	input string // JSON-encoded input (start events only)
}

// crushMCPServer is an in-process MCP HTTP server with token auth.
// The token is accepted via Authorization: Bearer header (Claude CLI)
// or as a ?token= query parameter (Qwen CLI, which cannot set headers).
type crushMCPServer struct {
	addr    string // "127.0.0.1:PORT"
	token   string
	httpSrv *http.Server
	// toolCh receives tool-call notifications from MCP handlers so the
	// Stream scan loop can emit ToolInputStart/Delta/End stream parts.
	toolCh chan mcpToolEvent
}

// stop shuts down the HTTP server.
func (s *crushMCPServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		slog.Debug("cliprovider: MCP server shutdown error", "err", err)
	}
}

// mcpConfigJSON returns the JSON bytes of the MCP server config suitable for
// writing to a temp file and passing to the claude CLI via --mcp-config.
func (s *crushMCPServer) mcpConfigJSON() ([]byte, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"crush": map[string]any{
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
func (s *crushMCPServer) mcpURL() string {
	return "http://" + s.addr + "/mcp?token=" + s.token
}

// newCrushMCPServer starts a local MCP HTTP server and returns it.
// The server exposes crush's core tools; each tool call goes through
// perms.Request before execution so crush's permission dialog appears.
// The token is accepted via Authorization: Bearer header OR ?token= query param.
// If token is empty a cryptographically random one is generated.
// sessions and sessionID are used by the todos tool to persist task updates.
func newCrushMCPServer(ctx context.Context, perms permission.Service, sessions session.Service, sessionID string, workingDir string, token string, mcpProxy ExternalMCPProxy) (*crushMCPServer, error) {
	if token == "" {
		// 32-byte random token → 64-char hex string.
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, fmt.Errorf("cliprovider: generate MCP token: %w", err)
		}
		token = hex.EncodeToString(tokenBytes)
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "crush",
		Title:   "Crush",
		Version: "1.0",
	}, nil)

	toolCh := make(chan mcpToolEvent, 32)
	registerMCPTools(srv, perms, sessions, sessionID, workingDir, toolCh)
	if mcpProxy != nil {
		registerExternalMCPTools(ctx, srv, perms, workingDir, mcpProxy, toolCh)
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
	return &crushMCPServer{
		addr:    addr,
		token:   token,
		httpSrv: httpSrv,
		toolCh:  toolCh,
	}, nil
}

// registerMCPTools adds crush tool implementations to the MCP server.
// Each tool requests permission via perms.Request before executing.
// toolCh, if non-nil, receives start/end notifications for each tool call.
func registerMCPTools(srv *mcp.Server, perms permission.Service, sessions session.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	registerBashTool(srv, perms, workingDir, toolCh)
	registerViewTool(srv, perms, workingDir, toolCh)
	registerWriteTool(srv, perms, workingDir, toolCh)
	registerGlobTool(srv, perms, workingDir, toolCh)
	registerGrepTool(srv, perms, workingDir, toolCh)
	if sessions != nil && sessionID != "" {
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
