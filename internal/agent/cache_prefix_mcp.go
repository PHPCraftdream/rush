package agent

import (
	"iter"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
)

func withMCPInstructions(systemPrompt string, cfg *config.ConfigStore, states iter.Seq2[string, mcp.ClientInfo]) string {
	type namedState struct {
		name   string
		server mcp.ClientInfo
	}
	var ordered []namedState
	for name, server := range states {
		ordered = append(ordered, namedState{name: name, server: server})
	}
	slices.SortFunc(ordered, func(a, b namedState) int {
		return strings.Compare(a.name, b.name)
	})

	var instructions strings.Builder
	for _, entry := range ordered {
		if !mcp.IsConfigured(cfg, entry.name) {
			continue
		}
		if entry.server.State != mcp.StateConnected {
			continue
		}
		if instruction := entry.server.Client.InitializeResult().Instructions; instruction != "" {
			instructions.WriteString(instruction)
			instructions.WriteString("\n\n")
		}
	}

	if collected := instructions.String(); collected != "" {
		return systemPrompt + "\n\n<mcp-instructions>\n" + collected + "\n</mcp-instructions>"
	}
	return systemPrompt
}

// withTurnMCPInstructions leaves a pinned recovery prompt untouched: it already
// carries the first attempt's instructions, and re-appending would duplicate
// them and change the prefix.
func withTurnMCPInstructions(systemPrompt string, recoveryContinuation bool, cfg *config.ConfigStore, states iter.Seq2[string, mcp.ClientInfo]) string {
	if recoveryContinuation {
		return systemPrompt
	}
	return withMCPInstructions(systemPrompt, cfg, states)
}
