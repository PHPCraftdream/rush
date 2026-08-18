package server

// MCP server configuration handlers: enable/disable, add, remove, and
// update of configured MCP servers.

import (
	"context"
	"encoding/json"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
)

func handleSetMCPDisabled(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetMCPDisabledPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	var err error
	if p.Disabled {
		err = mcp.DisableServer(ctx, store, p.Name)
	} else {
		err = mcp.EnableServer(ctx, store, p.Name)
	}
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleAddMCPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p AddMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Name == "" {
		c.reply(msg.ID, EventError, nil, "name is required")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	if err := mcp.AddServer(ctx, store, p.Name, mcpCfg); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveMCPServer(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := mcp.RemoveServer(store, p.Name); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMCPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.OldName == "" || p.Name == "" {
		c.reply(msg.ID, EventError, nil, "oldName and name are required")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove old entry
	if err := mcp.RemoveServer(store, p.OldName); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Add with new config
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	if err := mcp.AddServer(ctx, store, p.Name, mcpCfg); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
