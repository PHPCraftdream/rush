package agent

import (
	"context"
	"iter"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type namedMCPInstructionState struct {
	name   string
	server mcp.ClientInfo
}

func mcpInstructionStates(entries []namedMCPInstructionState) iter.Seq2[string, mcp.ClientInfo] {
	return func(yield func(string, mcp.ClientInfo) bool) {
		for _, entry := range entries {
			if !yield(entry.name, entry.server) {
				return
			}
		}
	}
}

func newMCPInstructionClient(t *testing.T, instructions string) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "instruction-fixture"}, &sdkmcp.ServerOptions{Instructions: instructions})
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect in-memory MCP server: %v", err)
	}
	clientSession, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "instruction-fixture-client"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect in-memory MCP client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return &mcp.ClientSession{ClientSession: clientSession}
}

func TestWithMCPInstructionsSortsByMapKeyAcrossAllOrders(t *testing.T) {
	isolateAllGlobalConfigPaths(t)

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{
		"A-key":        {},
		"a-key":        {},
		"whitespace":   {},
		"z-key":        {},
		"disconnected": {},
		"empty":        {},
	}})
	entries := []namedMCPInstructionState{
		{name: "z-key", server: mcp.ClientInfo{Name: "A-key", State: mcp.StateConnected, Client: newMCPInstructionClient(t, "\tZ  ")}},
		{name: "A-key", server: mcp.ClientInfo{Name: "z-key", State: mcp.StateConnected, Client: newMCPInstructionClient(t, "A\n \t")}},
		{name: "a-key", server: mcp.ClientInfo{Name: "not-the-map-key", State: mcp.StateConnected, Client: newMCPInstructionClient(t, "  M\r\n")}},
		{name: "whitespace", server: mcp.ClientInfo{Name: "whitespace", State: mcp.StateConnected, Client: newMCPInstructionClient(t, " \t\n")}},
		{name: "empty", server: mcp.ClientInfo{Name: "empty", State: mcp.StateConnected, Client: newMCPInstructionClient(t, "")}},
		{name: "disconnected", server: mcp.ClientInfo{Name: "disconnected", State: mcp.StateError}},
		{name: "unconfigured", server: mcp.ClientInfo{Name: "unconfigured", State: mcp.StateConnected}},
	}
	const prompt = "base prompt"
	const want = "base prompt\n\n<mcp-instructions>\nA\n \t\n\n  M\r\n\n\n \t\n\n\n\tZ  \n\n\n</mcp-instructions>"

	order := make([]string, len(entries))
	var checkPermutations func(int)
	checkPermutations = func(at int) {
		if at == len(entries) {
			got := withMCPInstructions(prompt, cfg, mcpInstructionStates(entries))
			require.Equal(t, want, got, "entry order %q", order)
			return
		}
		for i := at; i < len(entries); i++ {
			entries[at], entries[i] = entries[i], entries[at]
			order[at] = entries[at].name
			checkPermutations(at + 1)
			entries[at], entries[i] = entries[i], entries[at]
		}
	}
	checkPermutations(0)
}

func TestWithMCPInstructionsReflectsCurrentTextConfigurationAndState(t *testing.T) {
	isolateAllGlobalConfigPaths(t)

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{"mutable": {}}})
	oldClient := newMCPInstructionClient(t, "old text")
	const prompt = "unchanged prompt"
	assemble := func(state mcp.State, client *mcp.ClientSession) string {
		return withMCPInstructions(prompt, cfg, mcpInstructionStates([]namedMCPInstructionState{{
			name:   "mutable",
			server: mcp.ClientInfo{Name: "mutable", State: state, Client: client},
		}}))
	}

	require.Equal(t, "unchanged prompt\n\n<mcp-instructions>\nold text\n\n\n</mcp-instructions>", assemble(mcp.StateConnected, oldClient), "initial assembly")
	oldClient.InitializeResult().Instructions = "updated text"
	require.Equal(t, "unchanged prompt\n\n<mcp-instructions>\nupdated text\n\n\n</mcp-instructions>", assemble(mcp.StateConnected, oldClient), "updated text assembly")
	require.Equal(t, prompt, assemble(mcp.StateError, nil), "disconnected assembly")
	_, removed := cfg.RemoveMCP("mutable")
	require.True(t, removed, "remove configured MCP: entry was absent")
	require.Equal(t, prompt, assemble(mcp.StateConnected, nil), "unconfigured assembly")
	require.True(t, cfg.AddMCP("mutable", config.MCPConfig{}), "restore configured MCP: entry already existed")
	require.Equal(t, "unchanged prompt\n\n<mcp-instructions>\nupdated text\n\n\n</mcp-instructions>", assemble(mcp.StateConnected, oldClient), "reconfigured assembly")
	require.True(t, cfg.AddMCP("empty", config.MCPConfig{}), "configure empty-instruction MCP")
	emptyClient := newMCPInstructionClient(t, "")
	got := withMCPInstructions(prompt, cfg, mcpInstructionStates([]namedMCPInstructionState{{
		name:   "empty",
		server: mcp.ClientInfo{Name: "empty", State: mcp.StateConnected, Client: emptyClient},
	}}))
	require.Equal(t, prompt, got, "empty instruction must preserve original prompt")
	require.Equal(t, prompt, withMCPInstructions(prompt, cfg, mcpInstructionStates(nil)), "no-output assembly")
}
