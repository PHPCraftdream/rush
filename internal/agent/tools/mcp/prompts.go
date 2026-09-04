package mcp

import (
	"context"
	"iter"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Prompt = mcp.Prompt

var allPrompts = csync.NewMap[string, []*Prompt]()

// Prompts returns all available MCP prompts.
func Prompts() iter.Seq2[string, []*Prompt] {
	return allPrompts.Seq2()
}

// GetPromptMessages retrieves the content of an MCP prompt with the given arguments.
func GetPromptMessages(ctx context.Context, cfg *config.ConfigStore, clientName, promptName string, args map[string]string) ([]string, error) {
	lease, err := getOrRenewClient(ctx, cfg, clientName)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	result, err := lease.session.GetPrompt(lease.ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, msg := range result.Messages {
		if msg.Role != "user" {
			continue
		}
		if textContent, ok := msg.Content.(*mcp.TextContent); ok {
			messages = append(messages, textContent.Text)
		}
	}
	return messages, nil
}

// RefreshPrompts gets the updated list of prompts from the MCP and updates the
// global state.
func RefreshPrompts(ctx context.Context, name string) {
	lease, err := currentClientLease(ctx, name)
	if err != nil {
		slog.Warn("Refresh prompts: no session", "name", name)
		return
	}
	defer lease.close()

	prompts, err := getPrompts(lease.ctx, lease.session)
	if err != nil {
		updateState(name, StateError, err, nil, Counts{})
		return
	}

	updatePrompts(name, prompts)

	prev, _ := states.Get(name)
	prev.Counts.Prompts = len(prompts)
	updateState(name, StateConnected, nil, lease.session, prev.Counts)
}

func getPrompts(ctx context.Context, c *ClientSession) ([]*Prompt, error) {
	if c.InitializeResult().Capabilities.Prompts == nil {
		return nil, nil
	}
	result, err := c.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}
	return result.Prompts, nil
}

// updatePrompts updates the global mcpPrompts and mcpClient2Prompts maps
func updatePrompts(mcpName string, prompts []*Prompt) {
	if len(prompts) == 0 {
		allPrompts.Del(mcpName)
		return
	}
	allPrompts.Set(mcpName, prompts)
}
